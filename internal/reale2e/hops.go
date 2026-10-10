package reale2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The feature chain (design 12a-real-e2e, Feature and hop protocol,
// Owner decision): the coordinator dispatches each hop and reports it with
// observe; the supervisor fetches the authoritative task, checks its role,
// run marker, pinned workspace instance, base commit, effective selection
// and (for the coder) the owner's receipt, persists the observation and
// acknowledges it. A monitor also sees every admission to the run's roles
// on its own, cancels any coding admission without the receipt and any
// out-of-order or extra admission, and enforces each task's 10m bound
// from admission through publication.

// maxStoredLog bounds a hop's stored log bytes.
const maxStoredLog = 1 << 20

// grokEnvelopeAllowance is the composed-prompt allowance for the
// adapter's envelope beyond the goal, acceptance, payload and manuals.
const grokEnvelopeAllowance = 4 << 10

// expectedBase is hop i's required base commit, or false when its
// predecessor has no accepted result yet.
func (s *supervisor) expectedBase(i int) (string, bool) {
	if i == 0 {
		return s.workspace.SeedCommit, s.workspace.SeedCommit != ""
	}
	p := s.hops[i-1]
	if !p.terminal || p.commit.IsZero() {
		return "", false
	}
	return p.commit.String(), true
}

// handleRequest judges one control request inside the loop.
func (s *supervisor) handleRequest(req ControlRequest) string {
	if req.Op == opFinish {
		if !s.finish {
			s.finish = true
			s.ev.event(Event{Type: EvFinishRequested})
			s.say("finish requested: collecting evidence and stopping")
		}
		return CodeAccepted
	}
	// Deadlines first: an observation never admits work past a bound.
	if s.checkDeadlines() {
		s.cancelLate(req.Task)
		return CodeClosing
	}
	i := hopIndex(req.Hop)
	h := &s.hops[i]
	if h.observed {
		if h.task == req.Task {
			return CodeAlreadyObserved
		}
		s.fail("observation_conflict", fmt.Errorf("hop %s was already observed as another task", req.Hop))
		return CodeObservationClash
	}
	if label, ok := s.known[req.Task]; ok && label != req.Hop {
		s.fail("observation_conflict", fmt.Errorf("task %s belongs to %s", req.Task, label))
		return CodeObservationClash
	}
	if !contract.ValidTaskID(req.Task) {
		return CodeInvalidRequest
	}
	v, err := s.show(req.Task, 0)
	if err != nil {
		if contract.CodeOf(err) == contract.CodeNotFound {
			return CodeTaskMismatch
		}
		return CodeInternalError
	}
	if v.Role.ID != req.Hop {
		return CodeTaskMismatch
	}
	// The predecessor's authoritative state, not a cached one: its result
	// may have landed since the last poll.
	if i > 0 {
		s.refreshHop(i - 1)
		if s.failed {
			s.cancelLate(req.Task)
			return CodeClosing
		}
	}
	if req.Hop == HopCoder && s.decision == nil {
		s.premature(req.Task, "coding_before_approval")
		return CodeOwnerGateClosed
	}
	if h.task != "" && h.task != req.Task {
		s.fail("observation_conflict", fmt.Errorf("hop %s was admitted as another task", req.Hop))
		return CodeObservationClash
	}
	rec, code := s.bindingOf(i, v)
	if code != "" {
		s.cancelTask(req.Task)
		s.fail("binding_mismatch", fmt.Errorf("task %s does not satisfy hop %s's bindings (%s)", req.Task, req.Hop, code))
		return CodeTaskMismatch
	}
	s.admit(i, req.Task, v.CreatedAt)
	if s.checkDeadlines() {
		return CodeClosing
	}
	ev := s.ev.event(Event{Type: EvObserved, Hop: req.Hop, Task: req.Task})
	rec.ObservedSeq = ev.Seq
	h.observed, h.dispatch = true, rec
	if err := s.ev.writeJSON("hops/"+hopDir(i)+"/dispatch.json", rec); err != nil {
		s.fail("evidence_write_failed", err)
		return CodeInternalError
	}
	s.say("observed %s task %s", req.Hop, req.Task)
	return CodeAccepted
}

// bindingOf checks a hop task's authoritative bindings and returns its
// dispatch record, or a fixed mismatch code.
func (s *supervisor) bindingOf(i int, v contract.TaskView) (DispatchRecord, string) {
	hop := Hops[i]
	r, _ := s.flow.Role(hop)
	b := v.WorkspaceBinding
	base, ok := s.expectedBase(i)
	rec := DispatchRecord{Schema: DispatchSchema, Hop: hop, Task: v.TaskID, RoleID: v.Role.ID, Model: v.Effective.Model, Effort: v.Effective.Effort,
		Marker: markerFor(s.runID, hop), InlineFiles: []InlineFile{}}
	switch {
	case len(v.Request.Payload) == 0 || v.Request.Payload[0] != rec.Marker:
		return rec, "marker"
	case b == nil || b.Name != s.workspace.Name || b.Instance != s.workspace.Instance:
		return rec, "workspace"
	case !ok || b.BaseCommit == nil || *b.BaseCommit != base:
		return rec, "base"
	case v.Request.Override != nil || v.Effective.Model != r.Model || v.Effective.Effort != r.Effort || v.Role.Adapter != r.Adapter:
		return rec, "selection"
	}
	rec.Workspace, rec.Instance, rec.BaseCommit = b.Name, b.Instance, base
	if hop == HopCodeReviewer {
		inline, err := parseInlineFiles(v.Request.Goal)
		if err != nil || len(inline) != len(inlinePaths) {
			return rec, "inline"
		}
		coder := s.hops[2].files
		for _, p := range inlinePaths {
			if string(inline[p]) != string(coder[p]) {
				return rec, "inline"
			}
			rec.InlineFiles = append(rec.InlineFiles, InlineFile{Path: p, SHA256: sha256Hex(inline[p])})
		}
		size := len(v.Request.Goal) + len(v.Request.Acceptance) + len(v.Request.Payload[0]) + grokEnvelopeAllowance +
			len(s.rendered[manualName(hop, "instruction")]) + len(s.rendered[manualName(hop, "runbook")])
		if size > adapter.MaxGrokPromptBytes {
			return rec, "grok_prompt_overflow"
		}
	}
	return rec, ""
}

// admit records hop i's admission of task (once). Its bound runs from the
// plane's authoritative creation time (the supervisor's clock only when
// that is unreadable).
func (s *supervisor) admit(i int, task, createdAt string) {
	h := &s.hops[i]
	if h.task == task {
		return
	}
	at, ok := contract.ParseTime(createdAt)
	if !ok {
		at = s.w.Clock.Now()
	}
	h.task, h.admittedAt = task, at
	s.known[task] = Hops[i]
	s.ev.event(Event{Type: EvTaskAdmitted, Hop: Hops[i], Task: task})
}

// premature cancels a coding admission without the owner's receipt and
// fails the attempt, even when observe refused it.
func (s *supervisor) premature(task, code string) {
	s.known[task] = HopCoder
	s.ev.event(Event{Type: EvPrematureAdmission, Hop: HopCoder, Task: task})
	s.cancelTask(task)
	s.fail(code, fmt.Errorf("coding task %s was admitted before the owner's recorded yes", task))
}

// checkDeadlines fails the attempt when the attempt, a pending owner
// decision or a running hop (from its authoritative admission) passed its
// bound, cancelling an overdue hop through the plane. It runs before any
// success, decision, observation or admission is accepted.
func (s *supervisor) checkDeadlines() bool {
	if s.failed {
		return true
	}
	now := s.w.Clock.Now()
	switch {
	case now.Sub(s.attempt) > AttemptBound:
		s.fail("attempt_deadline_exceeded", errors.New("the attempt passed its 90m bound"))
		return true
	case s.owner == ownerAwaiting && now.After(s.ownerDeadline):
		s.fail("owner_decision_timeout", errors.New("no owner decision within 20m"))
		return true
	}
	for i := range s.hops {
		h := &s.hops[i]
		if h.task != "" && !h.terminal && now.Sub(h.admittedAt) > FeatureTaskBound {
			s.cancelTask(h.task)
			s.fail("feature_deadline_exceeded", fmt.Errorf("hop %s passed its 10m bound", Hops[i]))
			return true
		}
	}
	return false
}

// cancelLate cancels a task reported or admitted after the attempt
// stopped, when it is a running task of the run's roles: no paid work
// continues past a failure.
func (s *supervisor) cancelLate(task string) {
	if !contract.ValidTaskID(task) || slices.Contains(s.cancelled, task) {
		return
	}
	v, err := s.show(task, 0)
	if err != nil || hopIndex(v.Role.ID) < 0 || contract.TaskTerminal(v.State) {
		return
	}
	if s.known[task] == "" {
		s.known[task] = v.Role.ID
	}
	s.cancelTask(task)
}

// refreshHop processes hop i's task when the plane reports it terminal
// (an observed task only), so a successor is never judged on a stale
// predecessor state.
func (s *supervisor) refreshHop(i int) {
	h := &s.hops[i]
	if s.failed || h.task == "" || h.terminal || !h.observed {
		return
	}
	v, err := s.show(h.task, contract.DefaultTailLines)
	if err == nil && contract.TaskTerminal(v.State) {
		s.terminal(i, v)
	}
}

// tick is one monitor pass: deadlines, then every hop's authoritative
// state, then new admissions.
func (s *supervisor) tick() {
	if s.checkDeadlines() {
		return
	}
	for i := range s.hops {
		s.refreshHop(i)
	}
	if !s.failed {
		s.scanAdmissions()
	}
}

// scanAdmissions lists the plane's tasks and classifies every new task of
// the run's roles.
func (s *supervisor) scanAdmissions() {
	after := ""
	for !s.failed {
		ctx, cancel := s.opCtx()
		page, next, err := s.plane.ListTasks(ctx, after, contract.MaxTaskListLimit)
		cancel()
		if err != nil {
			return
		}
		for _, t := range page {
			i := hopIndex(t.Role.ID)
			if i < 0 || s.known[t.TaskID] != "" {
				continue
			}
			if i > 0 {
				s.refreshHop(i - 1)
				if s.failed {
					s.cancelLate(t.TaskID)
					return
				}
			}
			switch {
			case i == 2 && s.decision == nil:
				s.premature(t.TaskID, "coding_before_approval")
			case s.hops[i].task != "" || !s.predecessorDone(i):
				s.known[t.TaskID] = Hops[i]
				s.ev.event(Event{Type: EvExtraAdmission, Hop: Hops[i], Task: t.TaskID})
				s.cancelTask(t.TaskID)
				s.fail("extra_admission", fmt.Errorf("task %s is an extra or out-of-order %s admission", t.TaskID, Hops[i]))
			default:
				s.admit(i, t.TaskID, t.CreatedAt)
				s.checkDeadlines()
			}
			if s.failed {
				return
			}
		}
		if next == nil {
			return
		}
		after = *next
	}
}

// predecessorDone reports whether hop i may be admitted now.
func (s *supervisor) predecessorDone(i int) bool {
	if i == 0 {
		return true
	}
	p := s.hops[i-1]
	return p.terminal && !p.commit.IsZero() && (i != 2 || s.decision != nil)
}

// terminal records hop i's terminal task and advances the chain.
func (s *supervisor) terminal(i int, v contract.TaskView) {
	h := &s.hops[i]
	h.terminal, h.view = true, &v
	dir := "hops/" + hopDir(i)
	s.storeTask(dir, v)
	s.storeLogs(dir, v.TaskID)
	s.ev.event(Event{Type: EvTaskTerminal, Hop: Hops[i], Task: v.TaskID})
	// A success that lands past the bound is not accepted: neither its
	// elapsed time on the plane nor the supervisor's watchdog may exceed it.
	if d, ok := duration(v); !ok || d > FeatureTaskBound || s.w.Clock.Now().Sub(h.admittedAt) > FeatureTaskBound {
		s.fail("feature_deadline_exceeded", fmt.Errorf("hop %s ended after its 10m bound", Hops[i]))
		return
	}
	if s.checkDeadlines() {
		return
	}
	r := v.Result
	switch {
	case v.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0:
		s.fail("hop_failed", fmt.Errorf("hop %s ended %s", Hops[i], v.State))
		return
	case r.FinalMessage == nil || r.FinalMessageTruncated:
		s.fail("final_message_missing", fmt.Errorf("hop %s has no complete final message", Hops[i]))
		return
	case finalMarker(Hops[i], *r.FinalMessage) != approvedMarkers[Hops[i]]:
		s.fail("hop_not_approved", fmt.Errorf("hop %s did not end with %s", Hops[i], approvedMarkers[Hops[i]]))
		return
	case r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || r.Workspace.Commit == nil:
		s.fail("publication_missing", fmt.Errorf("hop %s published no result", Hops[i]))
		return
	}
	if err := s.acceptResult(i, *r.Workspace.Commit); err != nil {
		s.fail(codeOf(err), err)
		return
	}
	s.say("hop %s succeeded: result %s", Hops[i], h.commit)
	switch Hops[i] {
	case HopDesignReviewer:
		s.awaitOwner()
	case HopCodeReviewer:
		s.runFinalTests()
	}
}

// acceptResult pulls hop i's result and checks its parent, its allowed
// changes and (for the reviewer) its unchanged tree.
func (s *supervisor) acceptResult(i int, commit string) error {
	h := &s.hops[i]
	ctx, cancel := s.attemptCtx()
	defer cancel()
	if err := s.puller.PullResult(ctx, h.task, s.collectPath); err != nil {
		return failure("result_pull_failed", err)
	}
	c, err := s.collect.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return failure("result_pull_failed", err)
	}
	base, _ := s.expectedBase(i)
	if len(c.ParentHashes) != 1 || c.ParentHashes[0].String() != base {
		return failure("parent_mismatch", fmt.Errorf("hop %s's result does not have exactly the parent %s", Hops[i], base))
	}
	files, err := commitFiles(c)
	if err != nil {
		return failure("result_unreadable", err)
	}
	before := s.seed.Files
	if i > 0 {
		before = s.hops[i-1].files
	}
	for _, p := range changedPaths(before, files) {
		if !slices.Contains(allowedChanges[Hops[i]], p) {
			return failure("disallowed_change", fmt.Errorf("hop %s changed %s", Hops[i], p))
		}
	}
	h.commit, h.tree, h.files = c.Hash, c.TreeHash, files
	return nil
}

// commitFiles reads a commit's files.
func commitFiles(c *object.Commit) (map[string][]byte, error) {
	t, err := c.Tree()
	if err != nil {
		return nil, err
	}
	return treeFiles(t, projectBound*4)
}

// storeLogs stores a hop's complete retained log, bounded.
func (s *supervisor) storeLogs(dir, task string) {
	ctx, cancel := s.opCtx()
	defer cancel()
	logs, err := s.plane.TaskLogs(ctx, task)
	meta := LogMeta{Schema: LogMetaSchema}
	var data []byte
	if err == nil {
		data = logs.Data
		meta.SourceBytes, meta.RetainedBytes, meta.PlaneTruncated, meta.PlaneIncomplete = logs.SourceBytes, logs.RetainedBytes, logs.Truncated, logs.Incomplete
	} else {
		meta.PlaneIncomplete = true
	}
	if len(data) > maxStoredLog {
		data, meta.StoredTruncated = data[len(data)-maxStoredLog:], true
	}
	data = []byte(toValidUTF8(data))
	meta.StoredBytes = len(data)
	s.ev.writeFile(dir+"/logs.txt", data)
	s.ev.writeJSON(dir+"/logs.json", meta)
}

// awaitOwner presents the approved design and review to the owner and
// waits for the decision on the terminal.
func (s *supervisor) awaitOwner() {
	h := s.hops[1]
	design, review := h.files["design.md"], h.files["design-review.md"]
	if len(design) == 0 || lastNonEmptyLine(string(review)) != approvedMarkers[HopDesignReviewer] {
		s.fail("design_review_not_approved", errors.New("the reviewed design or its approving report is missing"))
		return
	}
	s.reviewSums = [2]string{sha256Hex(design), sha256Hex(review)}
	dp, rp := filepath.Join(s.runtime, "review", "design.md"), filepath.Join(s.runtime, "review", "design-review.md")
	for _, f := range []struct {
		p string
		b []byte
	}{{dp, design}, {rp, review}} {
		if err := writePrivateReadOnly(f.p, f.b); err != nil {
			s.fail("runtime_unavailable", err)
			return
		}
	}
	// The 20m window runs from the recorded awaiting_owner event itself
	// (the instant the checker measures from), never from a later point.
	ev := s.ev.event(Event{Type: EvAwaitingOwner, Hop: HopDesignReviewer, Task: h.task})
	at, ok := parseTime(ev.Time)
	if !ok {
		at = s.w.Clock.Now()
	}
	s.owner, s.ownerDeadline = ownerAwaiting, at.Add(OwnerDecisionBound)
	s.say("OWNER DECISION NEEDED (within 20m). Read the reviewed design and its approval:")
	s.say("  %s  sha256 %s", dp, s.reviewSums[0])
	s.say("  %s  sha256 %s", rp, s.reviewSums[1])
	s.say("Type exactly 'yes %s' to approve coding, or 'no %s' to stop. Anything else is not approval.", s.runID, s.runID)
}

// handleLine handles one operator terminal line (ok false: EOF).
func (s *supervisor) handleLine(line string, ok bool) {
	// The owner deadline is checked before any input is accepted: a yes
	// arriving together with (or after) the timeout is never approval.
	if s.checkDeadlines() {
		if !ok {
			s.lines = nil
		}
		return
	}
	if !ok {
		s.lines = nil
		if s.owner == ownerAwaiting {
			s.fail("owner_decision_missing", errors.New("the operator terminal closed before a decision"))
		}
		return
	}
	switch s.owner {
	case ownerNone:
		s.say("no decision is pending; input ignored")
		return
	case ownerDecided:
		s.say("the owner decision is already recorded and cannot change")
		return
	}
	var decision string
	switch line {
	case DecisionYes + " " + s.runID:
		decision = DecisionYes
	case DecisionNo + " " + s.runID:
		decision = DecisionNo
	default:
		s.say("not a decision: type exactly 'yes %s' or 'no %s'", s.runID, s.runID)
		return
	}
	s.decide(decision)
}

// decide records the owner's decision bound to the reviewed task, commit
// and digests; yes creates the read-only receipt, no ends the attempt.
func (s *supervisor) decide(decision string) {
	h := s.hops[1]
	s.owner = ownerDecided
	ev := s.ev.event(Event{Type: EvOwnerDecision, Hop: HopDesignReviewer, Task: h.task, Code: decision})
	rec := DecisionRecord{Schema: DecisionSchema, RunID: s.runID, Decision: decision, ReviewedTask: h.task, ResultCommit: h.commit.String(),
		DesignSHA256: s.reviewSums[0], ReviewSHA256: s.reviewSums[1], Seq: ev.Seq, DecidedAt: ev.Time}
	if err := s.ev.writeJSON("owner-decision.json", rec); err != nil {
		s.fail("evidence_write_failed", err)
		return
	}
	if decision == DecisionNo {
		s.say("the owner declined: no coding task will be admitted; stopping")
		s.fail("owner_rejected", errors.New("the owner answered no"))
		return
	}
	b, _ := encodeJSON(rec)
	if err := writePrivateReadOnly(filepath.Join(s.runtime, "owner-receipt.json"), b); err != nil {
		s.fail("runtime_unavailable", err)
		return
	}
	s.decision = &rec
	s.ev.event(Event{Type: EvReceiptCreated, Hop: HopDesignReviewer, Task: h.task})
	s.say("decision recorded. Resume the coordinator session with: continue after recorded decision")
}

// runFinalTests checks the reviewer's unchanged tree and runs the final
// tests on an export of the fourth result.
func (s *supervisor) runFinalTests() {
	coder, rev := s.hops[2], s.hops[3]
	if coder.tree != rev.tree {
		s.fail("reviewer_tree_changed", errors.New("the code reviewer's result tree differs from the coder's"))
		return
	}
	run, out, errOut, err := runGoTests(s.command, s.goBin, s.pathEnv, filepath.Join(s.runtime, "final"), rev.files, rev.commit.String(), rev.tree.String())
	s.finalTests = &run
	s.ev.writeJSON("validation/final-test.json", run)
	s.ev.writeFile("validation/final-stdout.txt", []byte(toValidUTF8(out)))
	s.ev.writeFile("validation/final-stderr.txt", []byte(toValidUTF8(errOut)))
	s.ev.event(Event{Type: EvFinalTests, Hop: HopCodeReviewer, Task: rev.task})
	switch {
	case err != nil:
		s.fail("final_tests_failed", err)
	case run.ExitCode != 0 || run.TimedOut:
		s.fail("final_tests_failed", fmt.Errorf("go test exited %d", run.ExitCode))
	default:
		s.complete = true
		s.say("the chain is complete and the final tests passed. Close the coordinator session, place the owner files in %s, then run: %s finish --run %s",
			filepath.Join(s.runtime, "owner"), s.w.Executable, s.runtime)
	}
}

// writePrivateReadOnly creates p (new) with b, mode 0400.
func writePrivateReadOnly(p string, b []byte) error {
	if err := writeAtomic(filepath.Dir(p), filepath.Base(p), b); err != nil {
		return err
	}
	return os.Chmod(p, 0o400)
}

// toValidUTF8 replaces invalid UTF-8 in stored diagnostics.
func toValidUTF8(b []byte) string { return string([]rune(string(b))) }
