package reale2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

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
	// The owner gate opens only once phase 1 itself ended with its pause
	// result and observed exit (phaseExited), never on the review alone.
	if Hops[i] == HopCodeReviewer {
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
	// The window is at most 20m from the prompt, bounded by the attempt.
	s.owner, s.ownerDeadline = ownerAwaiting, minTime(at.Add(OwnerDecisionBound), s.attempt.Add(AttemptBound))
	s.say("OWNER DECISION NEEDED (within 20m). The reviewed design and its approving review:")
	for _, f := range []struct {
		label, path, sum string
		text             []byte
	}{{"design.md", dp, s.reviewSums[0], design}, {"design-review.md", rp, s.reviewSums[1], review}} {
		s.say("----- %s (%s, sha256 %s) -----", f.label, f.path, f.sum)
		fmt.Fprintln(s.stdout, terminalSafe(f.text))
		s.say("----- end of %s -----", f.label)
	}
	s.prompt()
	// The terminal is read only from now on.
	s.lines = s.w.Terminal.Activate()
}

// decisionPrompt is the exact owner prompt.
const decisionPrompt = "Approve this design and start coding? [yes/no]"

// prompt prints the decision prompt.
func (s *supervisor) prompt() { fmt.Fprintln(s.stdout, decisionPrompt) }

// minTime returns the earlier time.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// terminalSafe renders text for the terminal: control characters other
// than newline and tab escaped.
func terminalSafe(b []byte) string {
	var out strings.Builder
	for _, r := range toValidUTF8(b) {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			fmt.Fprintf(&out, "\\x%02x", r)
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// handleLine handles one operator terminal line (ok false: EOF). Only
// "yes" or "no" (surrounding whitespace trimmed) decides; anything else
// is reprompted within the same window.
func (s *supervisor) handleLine(line string, ok bool) {
	// The owner deadline is checked before any input is accepted: a yes
	// arriving together with (or after) the timeout is never approval.
	if s.checkDeadlines() {
		s.lines = nil
		return
	}
	if !ok {
		s.lines = nil
		if s.owner == ownerAwaiting {
			s.fail("owner_decision_missing", errors.New("the operator terminal closed before a decision"))
		}
		return
	}
	if s.owner != ownerAwaiting {
		return
	}
	switch strings.TrimSpace(line) {
	case DecisionYes:
		s.decide(DecisionYes)
	case DecisionNo:
		s.decide(DecisionNo)
	default:
		s.say("not a decision: answer yes or no")
		s.prompt()
	}
}

// decide records the owner's decision bound to the reviewed task, commit
// and digests; yes creates the read-only receipt and, its bindings
// rechecked, automatically resumes the session in phase 2; no ends the
// attempt without coding.
func (s *supervisor) decide(decision string) {
	h := s.hops[1]
	s.owner, s.lines = ownerDecided, nil
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
	// The bindings are rechecked immediately before continuing.
	for i, name := range []string{"design.md", "design-review.md"} {
		b, err := readBounded(filepath.Join(s.runtime, "review", name), projectBound)
		if err != nil || sha256Hex(b) != s.reviewSums[i] || sha256Hex(h.files[name]) != s.reviewSums[i] {
			s.fail("decision_binding_changed", errors.New("the reviewed artifacts changed before coding"))
			return
		}
	}
	b, _ := encodeJSON(rec)
	if err := writePrivateReadOnly(filepath.Join(s.runtime, "owner-receipt.json"), b); err != nil {
		s.fail("runtime_unavailable", err)
		return
	}
	s.decision = &rec
	s.ev.event(Event{Type: EvReceiptCreated, Hop: HopDesignReviewer, Task: h.task})
	vals := map[string]string{"RUN_ID": s.runID, "RECEIPT": filepath.Join(s.runtime, "owner-receipt.json"), "REVIEWED_TASK": h.task,
		"RESULT_COMMIT": h.commit.String(), "DESIGN_SHA256": s.reviewSums[0], "REVIEW_SHA256": s.reviewSums[1], "WORKSPACE": s.workspace.Name,
		"INSTANCE": s.workspace.Instance, "COORD_DIR": filepath.Join(s.runtime, "coord")}
	prompt, err := renderTemplate(PhasePrompt2, s.templates[PhasePrompt2], vals)
	if err != nil {
		s.fail("templates_invalid", err)
		return
	}
	s.rendered[PhasePrompt2] = prompt
	if err := s.ev.writeFile("setup/"+PhasePrompt2, prompt); err != nil {
		s.fail("evidence_write_failed", err)
		return
	}
	s.manifest.Rendered = append(s.manifest.Rendered, FileHash{Name: "setup/" + PhasePrompt2, SHA256: sha256Hex(prompt)})
	sort.Slice(s.manifest.Rendered, func(i, j int) bool { return s.manifest.Rendered[i].Name < s.manifest.Rendered[j].Name })
	s.say("decision recorded: yes. Resuming the coordinator session for coding")
	if err := s.launchPhase(2, prompt); err != nil {
		s.fail(codeOf(err), err)
	}
}

// runFinalTests checks the reviewer's unchanged tree and runs the final
// validation (tests, build, the ten CLI cases and go test -json) on an
// export of the fourth result, within the one shared budget.
func (s *supervisor) runFinalTests() {
	coder, rev := s.hops[2], s.hops[3]
	if coder.tree != rev.tree {
		s.fail("reviewer_tree_changed", errors.New("the code reviewer's result tree differs from the coder's"))
		return
	}
	mark := func(typ, name string) int { return s.ev.event(Event{Type: typ, Code: name}).Seq }
	v := runFinalValidation(s.command, s.w.Clock, s.goBin, s.pathEnv, filepath.Join(s.runtime, "final"), rev.files, rev.commit.String(), rev.tree.String(), mark)
	s.validation = &v
	s.finalTests = &v.Test
	s.ev.writeJSON("validation/final-test.json", v.Test)
	s.ev.writeFile("validation/final-stdout.txt", []byte(toValidUTF8(v.TestOut)))
	s.ev.writeFile("validation/final-stderr.txt", []byte(toValidUTF8(v.TestErr)))
	s.ev.writeJSON("validation/cases.json", v.Cases)
	s.ev.writeFile(buildStdoutFile, []byte(toValidUTF8(v.BuildOut)))
	s.ev.writeFile(buildStderrFile, []byte(toValidUTF8(v.BuildErr)))
	s.ev.writeFile("validation/tests.jsonl", []byte(toValidUTF8(v.TestJSON)))
	s.ev.writeFile("validation/test-json-stderr.txt", []byte(toValidUTF8(v.TestJSONEr)))
	s.ev.writeJSON("validation/requirements.json", v.Reqs)
	s.ev.event(Event{Type: EvFinalTests, Hop: HopCodeReviewer, Task: rev.task})
	casesOK := v.Cases.Build.Started && v.Cases.Build.ExitCode == 0 && len(v.Cases.Cases) == len(FeatureCases)
	for _, c := range v.Cases.Cases {
		casesOK = casesOK && c.Pass
	}
	switch {
	case v.Err != nil || v.Test.ExitCode != 0 || v.Test.TimedOut || v.Test.Toolchain == "":
		s.fail("final_tests_failed", fmt.Errorf("go test exited %d: %v", v.Test.ExitCode, v.Err))
	case !casesOK && !v.Cases.Build.Started:
		s.fail(CodeCaseFailed, errors.New("the CLI build did not start"))
	case !casesOK && (v.Cases.Build.ExitCode != 0 || v.Cases.Build.TimedOut):
		s.fail(CodeCaseFailed, fmt.Errorf("the CLI build exited %d (timed out %t); %s: %s", v.Cases.Build.ExitCode, v.Cases.Build.TimedOut,
			buildStderrFile, contract.SafeText(string(v.BuildErr), maxBuildDiagnostic)))
	case !casesOK:
		s.fail(CodeCaseFailed, errors.New("a CLI feature case did not produce its required result"))
	case len(v.Reqs.MissingTests) > 0 || len(v.Reqs.FailedTests) > 0 || !v.Reqs.RunSignature || !v.Reqs.TableTests || !v.Reqs.TestCallsRun:
		s.fail(CodeTestMapping, errors.New("a required TestRun/<case-id> subtest or the run/table structure is missing or failing"))
	case !v.Reqs.TestRunPassed:
		s.fail(CodeValidationFail, fmt.Errorf("go test -json did not pass (exit %d, failures %v)", v.Reqs.TestRun.ExitCode, v.Reqs.TestFailures))
	default:
		s.complete = true
		s.say("the chain is complete and the final validation passed")
	}
}

// buildStdoutFile and buildStderrFile keep the final validation's build
// output; maxBuildDiagnostic bounds the excerpt a failure prints.
const (
	buildStdoutFile    = "validation/build-stdout.txt"
	buildStderrFile    = "validation/build-stderr.txt"
	maxBuildDiagnostic = 2000
)

// writePrivateReadOnly creates p (new) with b, mode 0400.
func writePrivateReadOnly(p string, b []byte) error {
	if err := writeAtomic(filepath.Dir(p), filepath.Base(p), b); err != nil {
		return err
	}
	return os.Chmod(p, 0o400)
}

// toValidUTF8 replaces invalid UTF-8 in stored diagnostics.
func toValidUTF8(b []byte) string { return string([]rune(string(b))) }
