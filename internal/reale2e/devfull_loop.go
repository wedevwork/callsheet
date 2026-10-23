package reale2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The development supervisor's single loop (design 12a-dev-flow-harness,
// Task identity and ledger, Execution lifecycle). Control requests (the
// socket goroutines forward them through supervisor.handleControl), owner
// terminal lines, coordinator phase exits and monitor ticks are handled
// here, one at a time: no state is shared with another goroutine (the one
// owned publication pull runs apart and hands its result back here). Every
// deadline is the injected clock's (now >= deadline is expired) and is
// checked before any request, decision or result is accepted.

// loop runs until the attempt stopped or fully executed.
func (d *devSupervisor) loop() {
	d.looping = true
	for !d.halted() {
		t, stop := d.s.w.Clock.NewTimer(d.s.w.Poll)
		select {
		case m := <-d.s.reqs:
			stop()
			m.reply <- d.serve(m.req)
		case line, ok := <-d.lines:
			stop()
			d.ownerLine(line, ok)
		case <-d.phaseDone():
			stop()
			d.phaseExited()
		case <-d.inspectDone():
			stop()
			d.inspected()
		case <-t:
			d.tick()
		case <-d.s.ctx.Done():
			stop()
			d.stop("interrupted", d.s.ctx.Err().Error())
		}
	}
}

// current is the running (not yet handled) phase, or nil.
func (d *devSupervisor) current() *devPhase {
	if n := len(d.phases); n > 0 && !d.phases[n-1].exited {
		return d.phases[n-1]
	}
	return nil
}

// phaseDone is the running phase's exit channel (nil: none).
func (d *devSupervisor) phaseDone() <-chan struct{} {
	if p := d.current(); p != nil {
		return p.proc.Done()
	}
	return nil
}

// tick is one monitor pass: deadlines, every admission on the plane, then
// the pending slot's progress.
func (d *devSupervisor) tick() {
	d.ticks++
	if d.deadlines() {
		return
	}
	if p := d.current(); p != nil && (p.stdout.Truncated() || p.stderr.Truncated()) {
		d.stop("coordinator_stream_limit", fmt.Sprintf("coordinator phase %d output passed its bound", p.n))
		return
	}
	d.scan()
	if !d.halted() {
		d.progress()
	}
}

// deadlines stops the attempt at the attempt, task or decision deadline,
// cancelling an overdue task through the plane. It reports a stop.
func (d *devSupervisor) deadlines() bool {
	if d.halted() {
		return true
	}
	switch {
	case d.expired(d.attemptDeadline()):
		d.stop("attempt_timeout", "the attempt reached its 90m bound")
	case d.window != nil && d.expired(d.window.deadline):
		d.stop("owner_expired", fmt.Sprintf("no %s decision for generation %d within its window", d.window.kind, d.window.gen))
	case d.slot != nil && d.slot.matched != "" && d.expired(d.slot.admittedAt.Add(FeatureTaskBound)):
		d.s.cancelTask(d.slot.matched)
		d.stop("task_timeout", fmt.Sprintf("workflow task %s reached its 10m bound after admission before wait consumption and publication admission completed", d.slot.matched))
	}
	return d.halted()
}

// scan accounts for every task on the private plane, all pages. A new
// task matching the pending exposed plan's exact request is its
// admission; anything else is an extra, duplicate or premature admission,
// cancelled and fatal. A list or read error stops for diagnosis: an
// incomplete scan is never "no admissions".
func (d *devSupervisor) scan() {
	after := ""
	sw := d.sweeps.begin(sweepMonitor)
	for !d.halted() {
		ctx, cancel := d.s.opCtx()
		page, next, err := d.s.plane.ListTasks(ctx, after, contract.MaxTaskListLimit)
		cancel()
		if err != nil {
			// A transport error closes the sweep explicitly.
			sw.end(err)
			d.stop("task_scan_failed", err.Error())
			return
		}
		sw.page(after, page, next)
		for _, t := range page {
			if d.s.known[t.TaskID] != "" {
				continue
			}
			v, err := d.s.show(t.TaskID, 0)
			if err != nil {
				sw.end(err)
				d.stop("task_scan_failed", err.Error())
				return
			}
			d.classify(v)
			if d.halted() {
				sw.end(errors.New("the sweep stopped at a refused admission"))
				return
			}
		}
		if next == nil {
			sw.end(nil)
			return
		}
		after = *next
	}
}

// workflowLabel is a slot's evidence label.
func workflowLabel(ordinal int) string { return "workflow-" + hopDir(ordinal-1) }

// classify accounts for one new task.
func (d *devSupervisor) classify(v contract.TaskView) {
	d.s.known[v.TaskID] = "seen"
	sl := d.slot
	match := sl != nil && sl.exposed && v.Role.ID == sl.plan.Role && v.Request.Goal == sl.plan.Goal
	switch {
	case v.Role.ID == "coder" && !d.coderAdmissible():
		d.reject(v.TaskID, "coding_before_approval")
	case !match:
		d.reject(v.TaskID, "extra_admission")
	case sl.matched != "" && sl.matched != v.TaskID:
		d.reject(v.TaskID, "duplicate_admission")
	default:
		d.matchSlot(v)
	}
}

// reject cancels an owned admission that is not the pending plan's and
// stops the attempt; cleanup accounts for it.
func (d *devSupervisor) reject(task, code string) {
	d.s.known[task] = "rejected"
	d.rejected = append(d.rejected, task)
	d.s.ev.event(Event{Type: EvExtraAdmission, Task: task, Code: code})
	d.s.cancelTask(task)
	d.stop(code, "task "+task+" is not the pending plan's single admission")
}

// matchSlot records the pending slot's authoritative admission; its 10m
// bound runs from the plane's creation time.
func (d *devSupervisor) matchSlot(v contract.TaskView) {
	sl := d.slot
	at, ok := contract.ParseTime(v.CreatedAt)
	if !ok {
		at = d.s.w.Clock.Now()
	}
	sl.matched, sl.admittedAt = v.TaskID, at
	d.s.known[v.TaskID] = d.planLabel(sl.plan)
	sl.admittedSeq = d.s.ev.event(Event{Type: EvTaskAdmitted, Hop: d.planLabel(sl.plan), Task: v.TaskID}).Seq
	sl.cmd = devWaitCommand(d.s.w.Executable, d.s.runtime, v.TaskID, sl.plan.Ordinal)
}

// bindingOK compares the authoritative task with the persisted plan: the
// exact goal and acceptance bytes, empty payload, no override, the frozen
// selection, and the workspace name, instance and planned base.
func (d *devSupervisor) bindingOK(v contract.TaskView, plan TaskPlan) error {
	if d.probe != nil {
		return d.probe.binding(v, plan)
	}
	role, ok := d.flow.Role(plan.Role)
	switch {
	case !ok || v.Role.ID != plan.Role || v.Role.Adapter != role.Adapter:
		return errors.New("the task's role is not the plan's")
	case v.Request.Goal != plan.Goal || v.Request.Acceptance != plan.Acceptance || len(v.Request.Payload) != 0 || v.Request.Override != nil:
		return errors.New("the dispatched request is not the prepared plan")
	case v.Effective.Model != role.Model || v.Effective.Effort != role.Effort:
		return fmt.Errorf("the effective pair is %s/%s", v.Effective.Model, v.Effective.Effort)
	}
	b := v.WorkspaceBinding
	if b == nil || b.Name != d.s.workspace.Name || b.Instance != d.s.workspace.Instance || b.BaseCommit == nil || *b.BaseCommit != plan.ExpectedBase {
		return errors.New("the workspace binding is not the plan's")
	}
	return nil
}

// serve answers one control request inside the loop.
func (d *devSupervisor) serve(req ControlRequest) controlResponse {
	if d.halted() || d.deadlines() {
		return devCode(CodeClosing)
	}
	switch req.Op {
	case opDevNext:
		return d.devNext()
	case opDevObserve:
		return devCode(d.devObserve(req))
	case opDevWait:
		return devCode(d.devWait(req))
	}
	return devCode(CodeInvalidRequest)
}

// devNext names the pending plan's prepared document, the gate to end the
// phase at, or completion. It never dispatches and never creates a slot.
// Repeating it before the plan's admission returns the same plan. Once the
// slot holds an admission it is never named again: the slot's evidence is
// advanced first (a consumed wake starts the publication's owned
// inspection, which never runs inside this reply), and a slot still
// occupied, its publication still being inspected included, answers
// pending.
func (d *devSupervisor) devNext() controlResponse {
	if sl := d.slot; sl != nil && (sl.matched != "" || sl.observed) {
		d.progress()
		switch {
		case d.halted():
			return devCode(CodeClosing)
		case d.slot == sl:
			return devCode(CodePending)
		}
		return d.devNext()
	}
	switch {
	case d.complete:
		return devCode(CodeComplete)
	case d.gate != nil:
		return controlResponse{OK: true, Code: CodePaused, Summary: fmt.Sprintf("gate=%s generation=%d", d.gate.kind, d.gate.gen)}
	case d.window != nil:
		return devCode(CodeOwnerGateClosed)
	case d.slot == nil:
		return devCode(CodeClosing)
	}
	if !d.seedVerified {
		ok, err := d.verifySeed()
		if err != nil {
			d.fail(err)
			return devCode(CodeClosing)
		}
		if !ok {
			return devCode(CodeSeedUnpublished)
		}
		d.seedVerified = true
		d.s.ev.event(Event{Type: EvSeedCreated, Code: "seed_pushed"})
		if !d.writeEvidenceJSON("setup/seed-push.json", DevSeedPush{Schema: devKind("seed-push"), RunID: d.s.runID, Workspace: d.s.workspace.Name,
			Instance: d.s.workspace.Instance, Ref: contract.DefaultBranchRef, Commit: d.s.workspace.SeedCommit}) {
			return devCode(CodeClosing)
		}
	}
	sl := d.slot
	if sl.plan.Role == "coder" {
		if err := d.checkReceipt(sl.plan.ExpectedBase); err != nil {
			d.stop("stale_receipt", err.Error())
			return devCode(CodeClosing)
		}
	}
	if d.halted() {
		// An evidence failure while verifying the seed closes dispatch.
		return devCode(CodeClosing)
	}
	if !sl.exposed {
		// The exposure is persisted before the slot is exposed or its plan
		// named: a failure of this event itself closes dispatch.
		d.s.ev.event(Event{Type: EvCoordinatorReady, Hop: d.planLabel(sl.plan), Code: "plan_exposed"})
		if d.halted() {
			return devCode(CodeClosing)
		}
		sl.exposed = true
	}
	return controlResponse{OK: true, Code: CodePrepared, Path: sl.docPath, Digest: sl.docDigest, Ordinal: sl.plan.Ordinal, Role: sl.plan.Role, Mode: sl.plan.Mode}
}

// devObserve accepts the coordinator's report of the pending slot's
// admitted task. A repeated report is idempotent; any other conflict is
// fatal and the conflicting admission is cancelled.
func (d *devSupervisor) devObserve(req ControlRequest) string {
	n, ok := devOrdinalOK(req.Ordinal)
	if !ok || !contract.ValidTaskID(req.Task) {
		return CodeInvalidRequest
	}
	for _, t := range d.state.Tasks {
		if t.TaskID == req.Task || t.Ordinal == n {
			if t.TaskID == req.Task && t.Ordinal == n {
				return CodeAlreadyObserved
			}
			d.stop("observation_conflict", "task "+req.Task+" and ordinal "+req.Ordinal+" belong to another slot")
			return CodeObservationClash
		}
	}
	sl := d.slot
	if sl == nil || n != sl.plan.Ordinal {
		return CodeTaskMismatch
	}
	switch label := d.s.known[req.Task]; {
	case sl.observed && sl.matched == req.Task:
		return CodeAlreadyObserved
	case sl.observed, sl.matched != "" && sl.matched != req.Task, label != "" && label != "seen" && label != d.planLabel(sl.plan):
		d.stop("observation_conflict", "task "+req.Task+" conflicts with the slot's admission")
		return CodeObservationClash
	}
	v, err := d.s.show(req.Task, 0)
	if err != nil {
		if contract.CodeOf(err) == contract.CodeNotFound {
			return CodeTaskMismatch
		}
		return CodeInternalError
	}
	if !sl.exposed {
		d.reject(req.Task, "premature_admission")
		return CodeTaskMismatch
	}
	if err := d.bindingOK(v, *sl.plan); err != nil {
		d.reject(req.Task, "binding_rejected")
		return CodeTaskMismatch
	}
	if sl.plan.Role == "coder" && !d.coderAdmissible() {
		d.reject(req.Task, "coding_before_approval")
		return CodeOwnerGateClosed
	}
	if sl.matched == "" {
		d.matchSlot(v)
	}
	sl.observed, sl.phase = true, len(d.phases)-1
	sl.observedSeq = d.s.ev.event(Event{Type: EvObserved, Hop: d.planLabel(sl.plan), Task: req.Task}).Seq
	if !d.writeEvidenceJSON(d.planDir(sl.plan)+"/observation.json", DevObservation{Schema: devKind("observation"), RunID: d.s.runID, Ordinal: n, Task: req.Task,
		AdmittedSeq: sl.admittedSeq, ObservedSeq: sl.observedSeq, Phase: sl.phase + 1, CreatedAt: v.CreatedAt}) {
		return CodeInternalError
	}
	return CodeAccepted
}

// devWait admits the slot's single background wait, for the observed
// task only; a second wait is refused before it spawns a watcher.
func (d *devSupervisor) devWait(req ControlRequest) string {
	n, ok := devOrdinalOK(req.Ordinal)
	sl := d.slot
	if !ok || !contract.ValidTaskID(req.Task) || sl == nil || n != sl.plan.Ordinal || !sl.observed || sl.matched != req.Task {
		return CodeTaskMismatch
	}
	if sl.waitStarted {
		d.stop(CodeDuplicateWait, "a second wait for task "+req.Task)
		return CodeDuplicateWait
	}
	sl.waitStarted = true
	sl.waitSeq = d.s.ev.event(Event{Type: EvTaskAdmitted, Hop: d.planLabel(sl.plan), Task: req.Task, Code: "wait_started"}).Seq
	return CodeAccepted
}

// progress advances the pending slot: its authoritative terminal state,
// then its qualified wait evidence (waited for within the task deadline),
// then its publication's admission and the semantic transition.
func (d *devSupervisor) progress() {
	sl := d.slot
	if sl == nil || sl.matched == "" {
		// No admission yet.
		return
	}
	// Design 12a-dev-flow-verify r0.8: once the wait started, the phase
	// stream is decoded on every pass, before the inspection, terminal and
	// observation gates below, so an observed unsupported notification
	// stops the attempt at its first complete record whatever the task's
	// state, even while its publication is being inspected (code review
	// C1).
	var w devWait
	if sl.waitStarted {
		var ok bool
		if w, ok = d.waitEvidence(sl); !ok {
			return
		}
	}
	if d.inspection != nil {
		// The consumed wait's publication is being inspected: the loop
		// takes the pull's result when it returns.
		return
	}
	v, err := d.s.show(sl.matched, contract.DefaultTailLines)
	if err != nil {
		d.stop("task_scan_failed", err.Error())
		return
	}
	if !contract.TaskTerminal(v.State) {
		return
	}
	dir := d.planDir(sl.plan)
	if !sl.terminal {
		sl.terminal = true
		d.s.ev.event(Event{Type: EvTaskTerminal, Hop: d.planLabel(sl.plan), Task: v.TaskID})
		if !d.writeEvidenceJSON(dir+"/task.json", contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v}) {
			return
		}
		if v.Result != nil && v.Result.FinalMessage != nil && !d.writeEvidence(dir+"/final.txt", []byte(*v.Result.FinalMessage)) {
			return
		}
		// The task's log is retrieved once, here, with its complete
		// metadata; the reviewer's execution evidence is derived from these
		// bytes and no later retrieval may replace them.
		sl.logs, sl.logMeta = d.storeDevLogs(dir, v.TaskID)
		if d.halted() {
			// A log (or event) write failed: nothing more is inspected.
			return
		}
	}
	if dur, ok := duration(v); !ok || dur >= FeatureTaskBound {
		d.stop("task_timeout", fmt.Sprintf("workflow task %s ran for its 10m bound", v.TaskID))
		return
	}
	if !sl.observed || !sl.waitStarted {
		return
	}
	files := readDevWaitFiles(d.s.runtime, sl.plan.Ordinal)
	rec := DevWaitRecord{Ordinal: sl.plan.Ordinal, Task: sl.matched, Command: d.s.tok(sl.cmd), StartSeq: sl.waitSeq, Wait: w}
	captured := files.captured(sl.matched, &rec)
	if files.OKX && !captured {
		d.writeWaitFiles(sl, files)
		d.stop(devWaitFailed, "the captured wait for task "+sl.matched+" did not end with exit 0 and its terminal result")
		return
	}
	if !w.complete() || !captured {
		// Incomplete evidence: wait for it within the task's deadline.
		return
	}
	rec.ConsumedSeq = d.s.ev.event(Event{Type: EvTaskTerminal, Hop: d.planLabel(sl.plan), Task: sl.matched, Code: "wait_consumed"}).Seq
	if !d.writeWaitFiles(sl, files) || !d.writeEvidenceJSON(dir+"/wait.json", envelope("wait", d.s.runID, rec)) {
		d.stop("evidence_write_failed", "the wait evidence of task "+sl.matched)
		return
	}
	if d.probe != nil && sl.plan.Role == HopDesigner {
		d.probe.admitWake(v, rec.ConsumedSeq)
		return
	}
	d.admitPublication(v, rec.ConsumedSeq)
}

// waitEvidence decodes the slot's phase stream and derives its wait
// evidence, stopping the attempt (ok false) at a stream or wait problem;
// a refused notification is named by its record.
func (d *devSupervisor) waitEvidence(sl *devSlot) (devWait, bool) {
	p := d.phases[sl.phase]
	stdout := p.stdout.Bytes()
	if !p.exited {
		// A running phase's unfinished last record is a partial pipe read:
		// decode the complete records only and keep the tail for a later
		// pass. The whole stream is validated once the phase exits.
		stdout = completeRecords(stdout)
	}
	stream := decodeDevStreamFor(devStreamFile(p.n), stdout, d.sessionID, devWaitPrefixOf(d.s.w.Executable, d.s.runtime))
	if stream.problem != "" {
		d.stop(stream.problem, fmt.Sprintf("coordinator phase %d stream", p.n)+refusedAt(stream.problem, stream.problemSrc))
		return devWait{}, false
	}
	w, src, problem := deriveDevWait(stream.records, sl.cmd, sl.matched)
	if problem != "" {
		d.stop(problem, "the wait for task "+sl.matched+refusedAt(problem, src.refused))
		return w, false
	}
	return w, true
}

// refusedAt names a refused notification's record in a stop reason.
func refusedAt(problem string, src SourceRef) string {
	if problem != devWakeUnsupported || src.File == "" {
		return ""
	}
	return fmt.Sprintf(": the notification at %s line %d", src.File, src.FirstLine)
}

// completeRecords is a running phase's stream up to and including its last
// LF: only complete LF-terminated records.
func completeRecords(stdout []byte) []byte {
	return stdout[:bytes.LastIndexByte(stdout, '\n')+1]
}

// admitPublication checks a terminal, observed, waited-for task's
// authoritative result and starts its publication's owned inspection; the
// loop admits the publication under FP-6 and applies the semantic
// transition once the pull returned (inspected).
func (d *devSupervisor) admitPublication(v contract.TaskView, consumedSeq int) {
	sl := d.slot
	plan := *sl.plan
	if err := d.bindingOK(v, plan); err != nil {
		d.stop("binding_rejected", err.Error())
		return
	}
	r := v.Result
	switch {
	case v.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0:
		d.stop("task_failed", fmt.Sprintf("workflow task %s ended %s", v.TaskID, v.State))
		return
	case r.FinalMessage == nil || r.FinalMessageTruncated:
		d.stop("final_message_missing", "workflow task "+v.TaskID+" has no complete final answer")
		return
	case r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || r.Workspace.Commit == nil:
		d.stop("publication_rejected", "workflow task "+v.TaskID+" published no result")
		return
	}
	marker, err := parseTerminalMarker(*r.FinalMessage)
	if err != nil || !markerAllowed(plan.Role, plan.Mode, marker) {
		d.stop("invalid_marker", "workflow task "+v.TaskID+"'s final answer has no single role-valid STATUS line")
		return
	}
	if err := d.startInspection(v, marker, consumedSeq); err != nil {
		d.stop("publication_rejected", err.Error())
	}
}

// devInspection is the pending slot's one owned publication pull. It runs
// on its own goroutine, so the loop keeps serving control requests (a
// dev-next answers pending meanwhile), terminal lines and deadlines; the
// goroutine touches nothing but the puller, its context and its own
// fields, and done is closed once the pull returned and err is set.
// Cancellation and cleanup join it through cancel and done.
type devInspection struct {
	view        contract.TaskView
	marker      string
	commit      string
	consumedSeq int
	plan        TaskPlan
	dir         string
	cancel      context.CancelFunc
	done        chan struct{}
	err         error
}

// inspectDone is the running inspection's completion (nil: none).
func (d *devSupervisor) inspectDone() <-chan struct{} {
	if d.inspection != nil {
		return d.inspection.done
	}
	return nil
}

// startInspection creates the slot's new supervisor-owned ordinary
// (non-bare) inspection repository and starts the result pull with the
// existing Puller, bounded by the earlier of the task's 10m bound and the
// attempt's; the loop also rechecks both on the injected clock while it
// runs and after it.
func (d *devSupervisor) startInspection(v contract.TaskView, marker string, consumedSeq int) error {
	sl := d.slot
	plan := *sl.plan
	dir := filepath.Join(d.s.runtime, "inspect", hopDir(plan.Ordinal-1))
	if _, err := os.Lstat(dir); err == nil {
		return errors.New("the inspection repository already exists")
	}
	if _, err := git.PlainInit(dir, false); err != nil {
		return err
	}
	deadline := minTime(d.attemptDeadline(), sl.admittedAt.Add(FeatureTaskBound))
	ctx, cancel := context.WithTimeout(d.s.ctx, max(0, deadline.Sub(d.s.w.Clock.Now())))
	in := &devInspection{view: v, marker: marker, commit: *v.Result.Workspace.Commit, consumedSeq: consumedSeq, plan: plan, dir: dir,
		cancel: cancel, done: make(chan struct{})}
	d.inspection = in
	puller, task := d.s.puller, v.TaskID
	go func() {
		defer close(in.done)
		in.err = puller.PullResult(ctx, task, dir)
	}()
	return nil
}

// joinInspection cancels a running inspection and waits for its
// goroutine to return.
func (d *devSupervisor) joinInspection() {
	if in := d.inspection; in != nil {
		in.cancel()
		<-in.done
	}
}

// inspected handles the finished pull on the loop: the published commit
// (exactly one parent, the planned base) and its base are read, every
// deadline is rechecked, and the publication is admitted under FP-6.
func (d *devSupervisor) inspected() {
	in := d.inspection
	d.inspection = nil
	in.cancel()
	if d.halted() {
		return
	}
	if in.err != nil {
		d.rejectInspection(in, "publication_rejected", fmt.Errorf("publication: the result pull failed: %w", in.err).Error())
		return
	}
	base, result, tree, err := d.readInspection(in)
	if err != nil {
		d.rejectInspection(in, "publication_rejected", err.Error())
		return
	}
	if d.deadlines() {
		// The task's (or the attempt's) deadline passed while the
		// publication was inspected: nothing is admitted.
		return
	}
	d.admitInspected(in, base, result, tree)
}

// admitInspected admits an inspected publication and applies the semantic
// transition.
func (d *devSupervisor) admitInspected(insp *devInspection, base, result []TreeFile, tree string) {
	sl, plan, v, marker, commit, consumedSeq := d.slot, insp.plan, insp.view, insp.marker, insp.commit, insp.consumedSeq
	r := v.Result
	in := admitInput{Role: plan.Role, Mode: plan.Mode, Marker: marker, Base: base, Result: result, Final: *r.FinalMessage, TaskID: v.TaskID}
	bmap, _ := indexTree(base)
	if plan.Role == "design-reviewer" {
		in.DesignDigest = fileSHA(bmap, "design.md")
	}
	dir := d.planDir(&plan)
	if plan.Role == "code-reviewer" {
		if plan.ExpectedBase != d.state.CandidateCommit {
			d.rejectInspection(insp, "publication_rejected", "the code review's input is not the candidate")
			return
		}
		in.Candidate = plan.ExpectedBase
		// The reviewer's execution evidence, derived from the log captured
		// at its terminal observation, is persisted before the review can
		// select any transition: an approval needs its corroborated
		// successful receipt.
		rmap, _ := indexTree(result)
		ev := DeriveDevReviewEvidence(d.reviewEvidenceInput(sl, v, marker, bmap, fileText(rmap, "review.md")))
		if !d.writeEvidenceJSON(dir+"/review-evidence.json", DevReviewRecord{Schema: devKind("review-evidence"), RunID: d.s.runID, Evidence: ev}) {
			return
		}
		if code, stop := ev.blocks(); stop {
			d.rejectInspection(insp, code, fmt.Sprintf("the code review %s's execution evidence is %s (%s)", v.TaskID, ev.Status, ev.Code))
			return
		}
		in.ReviewRun = ev.runDigest()
	}
	res, err := AdmitPublication(in)
	if err != nil {
		d.rejectInspection(insp, "publication_rejected", err.Error())
		return
	}
	if plan.Role == "coder" {
		if err := d.checkReceipt(plan.ExpectedBase); err != nil || fileSHA(res.Files, "design.md") != d.state.Receipt.Design ||
			fileSHA(res.Files, "revision-history.md") != d.state.Receipt.History {
			d.rejectInspection(insp, "stale_receipt", "the coder's publication is not bound to the current receipt")
			return
		}
	}
	d.trees[commit] = res.Files
	d.commits = append(d.commits, devCommit{Ordinal: plan.Ordinal, Task: v.TaskID, Commit: commit, Parent: plan.ExpectedBase, Tree: tree})
	d.finals[v.TaskID] = *r.FinalMessage
	selection, _ := d.flow.Role(plan.Role)
	out := Admitted{Ordinal: plan.Ordinal, TaskID: v.TaskID, Role: plan.Role, Mode: plan.Mode, Marker: marker, Commit: commit, Tree: tree,
		Base: plan.ExpectedBase, Workspace: d.s.workspace.Name, Instance: d.s.workspace.Instance, Selection: selection, Final: *r.FinalMessage,
		Report: res.Report, DesignSHA: fileSHA(res.Files, "design.md"), HistorySHA: fileSHA(res.Files, "revision-history.md"),
		ReviewSHA: fileSHA(res.Files, "design-review.md"), Artifacts: treeDigests(res.Files), Blocker: res.Blocker,
		DesignRequired: res.DesignRequired, BriefCurrent: res.BriefCurrent, CreatedAt: v.CreatedAt, AdmittedSeq: sl.admittedSeq,
		ObservedSeq: sl.observedSeq, ConsumedSeq: consumedSeq}
	if v.FinishedAt != nil {
		out.FinishedAt = *v.FinishedAt
	}
	if !d.writeEvidenceJSON(dir+"/admission.json", DevAdmission{Schema: devKind("admission"), RunID: d.s.runID, Task: v.TaskID, Marker: marker,
		Commit: commit, Parent: plan.ExpectedBase, Tree: tree, Artifacts: out.Artifacts, Disposition: dispositionOf(plan.Role, marker),
		Blocker: res.Blocker, DesignRequired: res.DesignRequired, BriefCurrent: res.BriefCurrent}) {
		return
	}
	if d.deadlines() {
		// Rechecked before the result is admitted and its successor
		// prepared; a sticky evidence failure also stops here.
		return
	}
	var st DevFullState
	var step StepResult
	if d.probe != nil {
		st, step = d.probe.next(d.state, out)
	} else {
		st, step = Next(d.state, out)
	}
	d.slot = nil
	d.apply(st, step)
	if len(d.state.Tasks) > 0 && d.state.Tasks[len(d.state.Tasks)-1].Ordinal == plan.Ordinal {
		last := d.state.Tasks[len(d.state.Tasks)-1]
		d.writeEvidenceJSON(dir+"/record.json", envelope("task-record", d.s.runID, last))
	}
}

// reviewEvidenceInput is a code review's evidence derivation input: the
// log captured at its terminal observation, its authoritative task, the
// plan's helper recipe and the candidate and seed trees.
func (d *devSupervisor) reviewEvidenceInput(sl *devSlot, v contract.TaskView, marker string, cand map[string]TreeFile, report string) reviewEvidenceInput {
	in := reviewEvidenceInput{Ordinal: sl.plan.Ordinal, Log: sl.logs, LogFile: d.planDir(sl.plan) + "/logs.txt", Meta: sl.logMeta, Task: v.TaskID,
		Helper: d.helper, Candidate: sl.plan.ExpectedBase, Initial: d.state.InitialCommit, CandTree: d.candidateTree(sl.plan.ExpectedBase),
		CandFiles: cand, SeedMain: fileContent(d.trees[d.state.InitialCommit], "main.go"), Marker: marker, Report: report}
	if at, ok := contract.ParseTime(v.CreatedAt); ok {
		in.AdmittedAt = at
	}
	if v.FinishedAt != nil {
		if at, ok := contract.ParseTime(*v.FinishedAt); ok {
			in.FinishedAt = at
		}
	}
	return in
}

// storeDevLogs retrieves a terminal task's log once and stores it with
// its complete metadata (the legacy log storage, retaining the bytes).
func (d *devSupervisor) storeDevLogs(dir, task string) ([]byte, DevLogMeta) {
	ctx, cancel := d.s.opCtx()
	logs, err := d.s.plane.TaskLogs(ctx, task)
	cancel()
	data, meta := devLogMetaOf(logs, err)
	d.writeEvidence(dir+"/logs.txt", storedLog(data))
	d.writeEvidenceJSON(dir+"/logs.json", DevLogRecord{Schema: devKind("logs"), RunID: d.s.runID, Task: task, Meta: meta})
	return data, meta
}

// readInspection reads a finished pull's inspection repository: the
// published commit (exactly one parent, the planned base) and its base.
func (d *devSupervisor) readInspection(in *devInspection) (base, result []TreeFile, tree string, err error) {
	commit, plan := in.commit, in.plan
	repo, err := git.PlainOpen(in.dir)
	if err != nil {
		return nil, nil, "", err
	}
	result, parent, tree, err := readCommitTree(repo, commit)
	if err != nil {
		return nil, nil, "", err
	}
	if parent != plan.ExpectedBase {
		return nil, nil, "", fmt.Errorf("publication: parent %s is not the planned base %s", parent, plan.ExpectedBase)
	}
	if known, ok := d.trees[plan.ExpectedBase]; ok {
		for _, f := range known {
			base = append(base, f)
		}
	} else if base, err = loadBaseTree(repo, plan.ExpectedBase); err != nil {
		return nil, nil, "", err
	}
	if err := copyObjects(repo.Storer, d.s.collect.Storer, []plumbing.Hash{plumbing.NewHash(commit)}); err != nil {
		return nil, nil, "", fmt.Errorf("publication: %w", err)
	}
	return base, result, tree, nil
}

// apply installs Next's state and carries out its decision; every new
// transition record is persisted first, append-only.
func (d *devSupervisor) apply(st DevFullState, res StepResult) {
	if res.Invalidate {
		st = InvalidateReceipt(st)
	}
	d.state = st
	for ; d.written < len(d.state.Transitions); d.written++ {
		if !d.writeEvidenceJSON(fmt.Sprintf("transitions/%02d.json", d.written+1), envelope("transition", d.s.runID, d.state.Transitions[d.written])) {
			return
		}
	}
	if d.halted() && res.Action != actionStop {
		// A sticky evidence failure closes dispatch, pauses and completion.
		return
	}
	switch res.Action {
	case actionStop:
		d.stop(res.Stop, cmpOr(res.Reason, res.Stop))
	case actionComplete:
		d.complete = true
		d.s.say("the code review approved candidate %s: the workflow is complete; final validation and verification are pending", d.state.CandidateCommit)
	case actionPause:
		d.gate = &devGate{kind: res.Pause, gen: res.Generation}
	case actionDispatch:
		d.prepareSlot()
	}
}

// prepareSlot builds the pending plan's complete bounded envelope,
// persists the plan and its dispatch document before either can be
// exposed, and opens its slot.
func (d *devSupervisor) prepareSlot() {
	plan := d.state.Pending
	goal, acc, inputs, err := plan.Goal, plan.Acceptance, plan.Inputs, error(nil)
	if d.probe == nil {
		goal, acc, inputs, err = d.envelope(plan)
	}
	if err != nil {
		d.stop(codeOf(err), err.Error())
		return
	}
	plan.Goal, plan.Acceptance, plan.Inputs = goal, acc, inputs
	if plan.Role == "coder" {
		if err := d.checkReceipt(plan.ExpectedBase); err != nil {
			d.stop("stale_receipt", err.Error())
			return
		}
	}
	doc := devDispatchDoc{Schema: devDispatchSchema, Ordinal: plan.Ordinal, Role: plan.Role, Mode: plan.Mode,
		Target: contract.TaskTarget{Kind: contract.TargetID, Value: plan.Role}, Goal: goal, Acceptance: acc, Payload: []string{},
		Workspace: d.s.workspace.Name, Instance: d.s.workspace.Instance, Base: plan.ExpectedBase}
	if d.probe != nil && plan.Role == HopDesigner {
		// A wake probe has no workspace, base or publication.
		doc.Workspace, doc.Instance, doc.Base = "", "", ""
	}
	body, err := marshalDispatch(doc)
	if err != nil || len(body) > maxDispatchDocByte {
		d.stop(CodePromptBudget, "the prepared dispatch document exceeds 32 KiB")
		return
	}
	dir := d.planDir(plan)
	if !d.writeEvidenceJSON(dir+"/plan.json", envelope("plan", d.s.runID, *plan)) || !d.writeEvidence(dir+"/dispatch.json", body) || d.halted() {
		return
	}
	path, sum, err := exposeDispatch(filepath.Join(d.s.runtime, "dispatch"), plan.Ordinal, goal, body)
	if err != nil {
		d.stop(CodePromptBudget, err.Error())
		return
	}
	d.slot = &devSlot{plan: plan, docPath: path, docDigest: sum}
}

// phaseExited handles the running phase's exit: its whole group must be
// gone, its exit 0 and its stream clean with a successful result. A gate
// opens its decision window; completion executes the attempt; anything
// else is the coordinator ending early.
func (d *devSupervisor) phaseExited() {
	p := d.current()
	d.observePhase(p, StoppedExited, groupBound)
	stream := decodeDevStreamFor(devStreamFile(p.n), p.stdout.Bytes(), d.sessionID, devWaitPrefixOf(d.s.w.Executable, d.s.runtime))
	if d.sessionID == "" {
		d.sessionID = stream.sessionID
	}
	switch {
	case d.halted():
		// The phase's evidence could not be written: no gate or completion.
		return
	case stream.problem != "":
		d.stop(stream.problem, fmt.Sprintf("coordinator phase %d stream", p.n)+refusedAt(stream.problem, stream.problemSrc))
		return
	case !p.rec.GroupGone:
		d.stop("coordinator_group_alive", fmt.Sprintf("coordinator phase %d's process group did not end", p.n))
		return
	case p.rec.ExitCode == nil || *p.rec.ExitCode != 0:
		d.stop("coordinator_exit_nonzero", fmt.Sprintf("coordinator phase %d exited %s", p.n, exitText(p.proc)))
		return
	case !stream.hasResult || !stream.resultOK:
		d.stop(devResultFailed, fmt.Sprintf("coordinator phase %d has no successful result", p.n))
		return
	}
	switch {
	case d.complete && d.slot == nil && d.probe != nil:
		if _, complete, err := parseProbePhaseGate(stream.resultText); err != nil || !complete {
			d.stop("phase_result", fmt.Sprintf("probe phase %d did not end with exactly probe=complete", p.n))
			return
		}
		d.executed = true
	case d.complete && d.slot == nil:
		// The approving review's wait was consumed and the phase exited
		// cleanly: the final candidate is validated now, before execution
		// is marked finished and before cleanup.
		d.s.say("coordinator phase %d ended after the approving review; validating the final candidate", p.n)
		if d.runDevFinalValidation() {
			d.executed = true
		}
	case d.gate != nil && d.probe != nil:
		gen, complete, err := parseProbePhaseGate(stream.resultText)
		if err != nil || complete || gen != d.gate.gen {
			d.stop("phase_result", fmt.Sprintf("probe phase %d did not end with exactly gate=probe generation=%d", p.n, d.gate.gen))
			return
		}
		d.openWindow(p.rec.ExitedSeq)
	case d.gate != nil:
		gate, gen, err := parsePhaseGate(stream.resultText)
		if err != nil || gate != d.gate.kind || gen != d.gate.gen {
			d.stop("phase_result", fmt.Sprintf("coordinator phase %d did not end with the gate result gate=%s generation=%d", p.n, d.gate.kind, d.gate.gen))
			return
		}
		d.openWindow(p.rec.ExitedSeq)
	case d.slot != nil && d.slot.matched != "" && !d.slot.observed:
		d.stop("observation_missing", "the coordinator ended without observing task "+d.slot.matched)
	case d.inspection != nil:
		d.stop("coordinator_exited", fmt.Sprintf("coordinator phase %d ended while the publication of task %s was being admitted", p.n, d.slot.matched))
	case d.slot != nil && d.slot.observed:
		// A refused notification of the ended phase is its own stop.
		if d.slot.waitStarted {
			if _, ok := d.waitEvidence(d.slot); !ok {
				return
			}
		}
		d.stop(devWaitFailed, "the coordinator ended before the wait for task "+d.slot.matched+" was consumed")
	default:
		d.stop("coordinator_exited", fmt.Sprintf("coordinator phase %d ended with no gate and no completion", p.n))
	}
}

// observePhase records an exited (or stopped) phase's process observation
// and session evidence; its whole group must be gone within bound.
func (d *devSupervisor) observePhase(p *devPhase, stoppedBy string, bound time.Duration) {
	if p.exited {
		return
	}
	p.exited = true
	code, sig := p.proc.Exit()
	p.rec.Signal, p.rec.StoppedBy = sig, stoppedBy
	select {
	case <-p.proc.Done():
		p.rec.Reaped = true
	default:
	}
	if sig == "" && p.rec.Reaped {
		p.rec.ExitCode = &code
	}
	p.rec.GroupGone = p.rec.Reaped && awaitGone(p.proc, d.s.w.Clock, bound)
	p.rec.ExitedSeq = d.s.ev.event(Event{Type: EvPhaseExited, Handle: phaseHandle(p.n), Code: exitText(p.proc)}).Seq
	d.writeEvidence(fmt.Sprintf("session/phase-%d.stdout.jsonl", p.n), p.stdout.Bytes())
	d.writeEvidence(fmt.Sprintf("session/phase-%d.stderr.txt", p.n), []byte(toValidUTF8(p.stderr.Bytes())))
	d.captureTranscript(p.n)
}

// captureTranscript copies the session transcript read-only once phase n
// exited, when it can be located exactly once (report-only otherwise).
func (d *devSupervisor) captureTranscript(n int) {
	if !sessionIDRE.MatchString(d.sessionID) {
		return
	}
	path, _, err := findTranscript(d.s.w.Home, d.sessionID)
	if err != nil {
		return
	}
	if data, err := readBounded(path, maxTextFile); err == nil {
		d.writeEvidence(fmt.Sprintf("session/transcript-phase-%d.jsonl", n), data)
	}
}
