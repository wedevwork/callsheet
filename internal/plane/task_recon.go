package plane

import (
	"context"
	"sort"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Reconciliation, receipt and loss decisions (iteration 06a, FP-2, FP-3,
// FP-6 and FP-7). A stable execution is its original {epoch, attachment}
// token and start digest, checked against the stored task's node,
// independently of the attachment currently reporting on it; only a
// callback of the node's current attached stream can propose work.

// ---- Output and results ----

// receiveLog accepts one output chunk into bounded memory and returns the
// acknowledged next offset. It never waits for disk. Primary output (null
// late_digest) is accepted only for a nonterminal task without a latched
// candidate. A tagged chunk is a reconciled worker's replayed tail: it is
// primary output while the task is still undecided, and late evidence
// (the separate staging ring, bound to its digest) once the task is
// decided otherwise; either needs its reconciliation's authorization.
func (ts *taskService) receiveLog(node string, gen uint64, b contract.TaskLogBody) (int, error) {
	if ts == nil {
		return 0, errNoTasks
	}
	ts.at("log-received", b.TaskID, context.Background())
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.authorizedLocked(node, gen, b.TaskID, b.Execution)
	if err != nil {
		return 0, err
	}
	undecided := !e.terminal() && e.cand == nil
	if b.LateDigest == nil {
		if !undecided || e.ring == nil || e.lateOK {
			return 0, errf(contract.CodeInvalidArgument, "task %s already reported its result or is resolved; no further primary output is accepted", b.TaskID)
		}
	} else {
		d := *b.LateDigest
		switch {
		case !e.lateOK:
			return 0, errf(contract.CodeInvalidArgument, "task %s's reconciliation did not authorize a replayed tail", b.TaskID)
		case e.lateBound != "" && e.lateBound != d:
			return 0, errf(contract.CodeInvalidArgument, "task %s's replayed tail changed its digest (%s, then %s)", b.TaskID, e.lateBound, d)
		}
		e.lateBound = d
		if !undecided || e.ring == nil {
			if e.lateRing == nil {
				e.lateRing = newPlaneLogAt(b.Offset)
			}
			next, err := e.lateRing.appendReplay(b.Offset, b.Data)
			if err == nil {
				ts.event("late-log " + b.TaskID)
			}
			return next, err
		}
	}
	var next int
	if b.LateDigest != nil {
		next, err = e.ring.appendReplay(b.Offset, b.Data)
	} else {
		next, err = e.ring.append(b.Offset, b.Data)
	}
	if err != nil {
		return 0, err
	}
	e.logDirty = true
	return next, nil
}

// resultOutcome derives the terminal outcome of worker result b.
func (ts *taskService) resultOutcome(e *taskEntry, b contract.TaskResultBody, now time.Time) terminalOutcome {
	o := terminalOutcome{finished: now.UTC(), exit: b.ExitCode, signal: b.Signal, final: b.FinalMessage, truncated: b.FinalMessageTruncated}
	switch {
	case e.started:
		s := e.startedAt
		o.started = &s
	case e.rec.StartedAt != nil:
		s := *e.rec.StartedAt
		o.started = &s
	}
	if e.ring == nil {
		e.ring = newPlaneLogFrom(e.rec.Log)
	}
	// A lost outcome may know less output than was received (a worker
	// that crashed after sending it reports what its journal held): the
	// received output and counters stay, and the log is incomplete.
	short := b.OutputBytes < e.ring.next
	o.log = e.ring.finalize(b.OutputBytes, b.LogIncomplete || short, b.CounterOverflow)
	if b.Outcome == contract.OutcomeLost {
		o.state = contract.TaskLost
		o.reason = &contract.TaskReason{Code: contract.ReasonWorkerLost, Message: "the worker could not confirm the execution's outcome (it restarted or stopped while the execution was active)"}
		return o
	}
	o.state = contract.TaskFailed
	if b.ExitCode != nil && *b.ExitCode == 0 {
		o.state = contract.TaskSucceeded
	}
	return o
}

// committedLocked reports whether the outcome with digest d is durable for
// e: its confirmed terminal result, or its confirmed late evidence.
func (ts *taskService) committedLocked(e *taskEntry, d string) bool {
	if !e.terminal() || !e.released {
		return false
	}
	if e.rec.ResultDigest != nil && *e.rec.ResultDigest == d {
		return true
	}
	return e.rec.Late != nil && e.rec.Late.Digest == d && e.confirmed && e.late == nil
}

// receiveResult captures a worker's frozen outcome and reports whether it
// is already committed. Receipt is timestamped at the plane and never
// publishes a terminal state or releases a slot: an undecided task
// latches it as its terminal candidate (the first candidate governs), a
// task decided otherwise records it as late evidence, and an identical
// resubmission is acknowledged again (committed once durable). A changed
// digest for the same execution is a protocol conflict and never
// overwrites the original.
func (ts *taskService) receiveResult(node string, gen uint64, b contract.TaskResultBody) (bool, error) {
	if ts == nil {
		return false, errNoTasks
	}
	ts.at("result-received", b.TaskID, context.Background())
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.authorizedLocked(node, gen, b.TaskID, b.Execution)
	if err != nil {
		return false, err
	}
	d := b.Digest
	now := ts.clock.Now()
	conflict := func(have string) error {
		return &contract.Error{Code: contract.CodeConflict, Details: map[string]any{"reason": contract.ReasonResultConflict, "task_id": b.TaskID, "digest": d, "recorded_digest": have},
			Message: "task " + b.TaskID + " already has a different worker outcome (" + have + "); a changed result never overwrites the original"}
	}
	switch {
	case ts.committedLocked(e, d):
		return true, nil
	case e.cand != nil && e.cand.digest == d, e.late != nil && e.late.result.Digest == d:
		return false, nil
	case e.terminal() && e.rec.ResultDigest != nil && *e.rec.ResultDigest == d:
		return false, nil // visible, not yet confirmed
	case e.terminal() && e.rec.Late != nil && e.rec.Late.Digest == d:
		return false, nil
	case e.cand != nil && e.cand.digest != "":
		return false, conflict(e.cand.digest)
	case e.terminal() && e.rec.ResultDigest != nil:
		return false, conflict(*e.rec.ResultDigest)
	case e.late != nil:
		return false, conflict(e.late.result.Digest)
	case e.terminal() && e.rec.Late != nil:
		return false, conflict(e.rec.Late.Digest)
	case !e.terminal() && e.cand == nil:
		if e.ring != nil && b.OutputBytes < e.ring.next && b.Outcome == contract.OutcomeNatural {
			return false, errf(contract.CodeInvalidArgument, "task %s reports %d output bytes, fewer than the %d received", b.TaskID, b.OutputBytes, e.ring.next)
		}
		if !e.started && e.rec.StartedAt == nil && b.Outcome == contract.OutcomeNatural {
			// The worker's launch fact precedes its natural result: running
			// is published before the terminal record.
			e.started, e.needRunning, e.startedAt = true, true, now.UTC()
		}
		o := ts.resultOutcome(e, b, now)
		kind := candNatural
		if b.Outcome == contract.OutcomeLost {
			kind = candLost
		}
		e.cand = &terminalCand{kind: kind, outcome: o, at: now, digest: d}
		e.reconciling = false
		ts.refreshLocked(e)
		ts.wakeLocked(e)
		ts.event("result-captured " + b.TaskID)
		return false, nil
	}
	// Decided otherwise (lost, refused, or migrated history): late
	// evidence in the single slot, never a change of the terminal state.
	var lg contract.TaskLog
	if e.lateRing != nil && b.OutputBytes < e.lateRing.next && b.Outcome == contract.OutcomeNatural {
		return false, errf(contract.CodeInvalidArgument, "task %s reports %d output bytes, fewer than the %d received", b.TaskID, b.OutputBytes, e.lateRing.next)
	}
	if e.lateRing != nil {
		lg = e.lateRing.finalize(b.OutputBytes, b.LogIncomplete || b.OutputBytes < e.lateRing.next, b.CounterOverflow)
	} else {
		lg = contract.TaskLog{SourceBytes: b.OutputBytes, ReceivedBytes: 0, Incomplete: b.OutputBytes > 0}
	}
	e.late = &lateCand{result: b, at: now.UTC(), log: lg}
	e.lateRing = nil
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.event("late-captured " + b.TaskID)
	return false, nil
}

// ---- Losses ----

// lossFact is a lease or startup-grace expiry captured under the node
// lock: the node, the lost reason, whether only loaded tasks are affected
// (startup grace), and the admission sequence at that instant (tasks
// admitted afterwards are never affected).
type lossFact struct {
	node       string
	reason     string
	loadedOnly bool
	maxSeq     uint64
}

// enqueueLoss records a loss fact; it is called under the node lock and
// takes only the innermost loss queue lock.
func (ts *taskService) enqueueLoss(f lossFact) {
	if ts == nil {
		return
	}
	ts.lossMu.Lock()
	ts.losses = append(ts.losses, f)
	ts.lossMu.Unlock()
	notifyChan(ts.lossKick)
}

// applyLossesLocked drains the loss queue and latches a lost decision on
// every affected undecided task. A captured terminal candidate already
// latched for commit is retried rather than replaced. The caller holds the
// task lock.
func (ts *taskService) applyLossesLocked() {
	ts.lossMu.Lock()
	facts := ts.losses
	ts.losses = nil
	ts.lossMu.Unlock()
	for _, f := range facts {
		msg := "the node's lease expired before the execution's outcome was confirmed"
		if f.reason == contract.ReasonStartupGraceExpired {
			msg = "the node did not reconnect within the plane's startup reconciliation grace"
		}
		ts.event("loss-applied " + f.node + " " + f.reason)
		for _, id := range ts.ids {
			e := ts.tasks[id]
			if e.rec.Role.Node != f.node || e.seq > f.maxSeq || (f.loadedOnly && !e.loaded) {
				continue
			}
			ts.latchLostLocked(e, f.reason, msg)
		}
	}
}

// latchLostLocked latches a plane lost decision on an undecided task: its
// frozen tail is the live ring's (or the loaded record's) at this instant.
func (ts *taskService) latchLostLocked(e *taskEntry, reason, msg string) {
	if e.terminal() || e.cand != nil {
		return
	}
	now := ts.clock.Now().UTC()
	o := terminalOutcome{state: contract.TaskLost, finished: now, reason: &contract.TaskReason{Code: reason, Message: msg}}
	switch {
	case e.started:
		s := e.startedAt
		o.started = &s
	case e.rec.StartedAt != nil:
		s := *e.rec.StartedAt
		o.started = &s
	}
	if e.ring != nil {
		o.log = e.ring.snapshot()
	} else {
		o.log = e.rec.Log
		o.log.Data = append([]byte(nil), e.rec.Log.Data...)
	}
	e.cand = &terminalCand{kind: candLost, outcome: o, at: now}
	e.reconciling = false
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.event("lost-latched " + e.rec.TaskID + " " + reason)
}

// lossWorker applies enqueued loss facts under the task lock until the
// plane shuts down.
func (ts *taskService) lossWorker() {
	defer ts.wg.Done()
	for {
		select {
		case <-ts.lossKick:
			ts.mu.Lock()
			ts.applyLossesLocked()
			ts.mu.Unlock()
		case <-ts.stop:
			return
		}
	}
}

// start runs the loss worker (after construction, before serving).
func (ts *taskService) start() {
	ts.wg.Add(1)
	go ts.lossWorker()
}

// graceNodes returns the nodes that hold loaded nonterminal tasks: each
// gets one startup reconciliation grace.
func (ts *taskService) graceNodes() map[string]bool {
	if ts == nil {
		return nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := map[string]bool{}
	for _, e := range ts.tasks {
		if e.loaded && !e.terminal() {
			out[e.rec.Role.Node] = true
		}
	}
	return out
}

// ---- Inventory and reconciliation ----

// reconciliation is one attachment's reconciliation state, owned by its
// session goroutine: the expected inventory page and Run ID, the
// dispositions (coalesced by task ID) and, after the final page, the
// reconcile pages and how many were acknowledged.
type reconciliation struct {
	next    int
	runID   string
	final   bool
	actions map[string]contract.TaskReconcileEntry
	pages   []contract.TaskReconcileBody
	acked   int
	done    bool
	// unknown are the executions this attachment reported that the plane
	// does not hold: their output and outcome are acknowledged without
	// being recorded (never committed), so the worker keeps its evidence.
	unknown map[string]bool
}

func newReconciliation() *reconciliation {
	return &reconciliation{actions: map[string]contract.TaskReconcileEntry{}, unknown: map[string]bool{}}
}

// inventory validates and applies one inventory page on attachment gen of
// node. It holds the task lock (no disk I/O), applies pending loss facts
// first (an eligible loss is never erased by a newer attachment), and
// returns an error (the stream closes) on an out-of-order or duplicate
// page or task entry, a malformed identity or a changed start digest. An
// execution absent from plane storage is a conflict answered stop_lost;
// after the final page every held task of the node absent from the
// complete inventory is lost (execution_missing) and the reconcile pages
// are built.
func (ts *taskService) inventory(node string, gen uint64, b contract.TaskInventoryBody, rc *reconciliation) error {
	if rc.final {
		return errf(contract.CodeInvalidArgument, "task_inventory page %d after the final page", b.Page)
	}
	if b.Page != rc.next {
		return errf(contract.CodeInvalidArgument, "task_inventory page %d is out of order (want %d)", b.Page, rc.next)
	}
	if rc.next > 0 && b.RunID != rc.runID {
		return errf(contract.CodeInvalidArgument, "task_inventory run_id changed within one attachment")
	}
	rc.runID = b.RunID
	rc.next++
	if ts == nil {
		if len(b.Entries) > 0 {
			return errNoTasks
		}
		if b.Final {
			rc.final = true
			rc.pages = []contract.TaskReconcileBody{{Final: true}}
		}
		return nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.applyLossesLocked()
	if !ts.reg.currentAttachment(node, gen) {
		return errf(contract.CodeInvalidArgument, "attachment %d is no longer node %s's live attachment", gen, node)
	}
	for _, in := range b.Entries {
		if _, dup := rc.actions[in.TaskID]; dup {
			return errf(contract.CodeInvalidArgument, "task_inventory reports task %s twice", in.TaskID)
		}
		e := ts.tasks[in.TaskID]
		if e == nil {
			// Never authority to recreate a task: the worker stops it and
			// keeps its evidence; nothing of its request content is kept.
			rc.actions[in.TaskID] = contract.TaskReconcileEntry{TaskID: in.TaskID, Execution: in.Execution, Action: contract.ActionStopLost}
			rc.unknown[in.TaskID] = true
			ts.reg.markUnknown(node, gen)
			ts.logger.Warn("node reported an execution unknown to this plane", "node_id", node, "task_id", in.TaskID, "reason", contract.ReasonUnknownExecution)
			ts.event("unknown-execution " + node + " " + in.TaskID)
			continue
		}
		switch {
		case e.rec.Role.Node != node || e.rec.Execution != in.Execution:
			return errf(contract.CodeInvalidArgument, "task_inventory entry %s does not match the task's stable execution identity", in.TaskID)
		case e.rec.StartDigest == nil || *e.rec.StartDigest != in.StartDigest:
			return errf(contract.CodeInvalidArgument, "task_inventory entry %s changed its start digest", in.TaskID)
		}
		e.seen = gen
		rc.actions[in.TaskID] = contract.TaskReconcileEntry{TaskID: in.TaskID, Execution: in.Execution, Action: ts.disposeLocked(e, gen, in)}
	}
	ts.event("inventory " + node + " " + itoa(b.Page))
	if !b.Final {
		return nil
	}
	rc.final = true
	// A complete inventory: every held, undecided task of this node it
	// omits is lost, unless this Run can prove its start was never sent
	// (those are rejected unsent by their own path).
	for _, id := range ts.ids {
		e := ts.tasks[id]
		if e.rec.Role.Node != node || e.terminal() || e.cand != nil || e.seen == gen {
			continue
		}
		if e.loaded || e.send == sendSending || e.send == sendReplied {
			ts.latchLostLocked(e, contract.ReasonExecutionMissing, "the node's complete inventory does not hold this execution")
		}
	}
	ids := make([]string, 0, len(rc.actions))
	for id := range rc.actions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for len(ids) > 0 || len(rc.pages) == 0 {
		n := min(len(ids), contract.MaxReconcileEntries)
		p := contract.TaskReconcileBody{Entries: []contract.TaskReconcileEntry{}}
		for _, id := range ids[:n] {
			p.Entries = append(p.Entries, rc.actions[id])
		}
		ids = ids[n:]
		p.Final = len(ids) == 0
		rc.pages = append(rc.pages, p)
	}
	ts.event("inventory-complete " + node)
	return nil
}

// disposeLocked decides one known execution's disposition and applies it:
// an undecided task continues (active) or sends its result; a decided one
// is forgotten when the reported digest is committed, else stopped
// (active) or asked for its result as late evidence.
func (ts *taskService) disposeLocked(e *taskEntry, gen uint64, in contract.TaskInventoryEntry) string {
	active := in.Phase == contract.PhasePreparing || in.Phase == contract.PhaseRunning
	if !e.terminal() && e.cand == nil {
		e.rgen, e.lateOK, e.lateBound, e.reconciling = gen, !active, "", false
		if e.send == sendSending {
			e.send = sendReplied
		}
		if e.ring == nil {
			e.ring = newPlaneLogFrom(e.rec.Log)
			e.rec.Log.Data = nil
		}
		if in.Phase == contract.PhaseRunning && in.StartedAt != nil && !e.started && e.rec.StartedAt == nil {
			// The worker's launch fact (its own clock, informational only).
			t, _ := contract.ParseTime(*in.StartedAt)
			e.started, e.startedAt, e.needRunning = true, t, true
			ts.wakeLocked(e)
		}
		ts.refreshLocked(e)
		if active {
			return contract.ActionContinue
		}
		return contract.ActionSendResult
	}
	if in.ResultDigest != nil && ts.committedLocked(e, *in.ResultDigest) {
		return contract.ActionForget
	}
	e.rgen, e.lateOK, e.lateBound = gen, true, ""
	if in.ResultDigest != nil {
		e.lateBound = *in.ResultDigest
	}
	if active {
		return contract.ActionStopLost
	}
	return contract.ActionSendResult
}

// ---- Reads ----

// logOfLocked returns e's current primary log (data may be nil for a
// terminal record whose tail is on disk) and its retained byte count.
func (ts *taskService) logOfLocked(e *taskEntry) (contract.TaskLog, int) {
	switch {
	case e.cand != nil && e.cand.kind != candRefusal:
		return e.cand.outcome.log, len(e.cand.outcome.log.Data)
	case e.ring != nil:
		return e.ring.meta()
	}
	return e.rec.Log, e.retained
}

// viewLocked builds e's TaskView at now; lines selects the tail. A
// terminal task's tail is read from its document by the caller when its
// data is not in memory (show).
func (ts *taskService) viewLocked(e *taskEntry, lines int, doc *roleDoc, now time.Time) contract.TaskView {
	// A latched candidate is published only once durable: the view keeps
	// the visible nonterminal state, with completion_pending.
	rec := e.rec
	v := contract.TaskView{TaskID: rec.TaskID, Request: rec.Request, Role: contract.PublicRole(rec.Role), Effective: rec.Effective,
		State: rec.State, CreatedAt: contract.FormatTime(rec.CreatedAt), DurabilityConfirmed: e.confirmed, Reason: rec.Reason,
		Candidates: rec.Candidates, CompletionPending: e.cand != nil && !e.terminal(), Reconciling: e.isReconciling()}
	if rec.StartedAt != nil {
		s := contract.FormatTime(*rec.StartedAt)
		v.StartedAt = &s
	}
	end := now
	if rec.FinishedAt != nil {
		s := contract.FormatTime(*rec.FinishedAt)
		v.FinishedAt = &s
		end = *rec.FinishedAt
	}
	v.ElapsedMS = int(max(0, end.Sub(rec.CreatedAt).Milliseconds()))
	if (e.cand != nil || e.late != nil) && (e.watchdog || e.commitFailed) {
		r := contract.ReasonResultStorageUnconfirmed
		v.PersistenceReason = &r
	}
	lg, retained := ts.logOfLocked(e)
	meta := contract.MetaOf(contract.TaskLog{SourceBytes: lg.SourceBytes, ReceivedBytes: lg.ReceivedBytes, Incomplete: lg.Incomplete, CounterOverflow: lg.CounterOverflow}, v.Reconciling)
	meta.RetainedBytes = retained
	meta.DroppedBytes = max(0, lg.SourceBytes-retained)
	meta.Truncated = lg.ReceivedBytes > retained || lg.Incomplete
	v.Log = meta
	switch {
	case e.ring != nil && (e.cand == nil || e.cand.kind == candRefusal):
		v.LogTail, v.TailTruncated = contract.LogTail(e.ring.tail(contract.MaxTailBytes), lines)
		v.TailTruncated = v.TailTruncated || (lines > 0 && retained > contract.MaxTailBytes)
	case lg.Data != nil || retained == 0:
		v.LogTail, v.TailTruncated = contract.LogTail(lg.Data, lines)
	}
	if contract.TaskTerminal(rec.State) {
		v.Result = &contract.TaskResult{State: rec.State, ExitCode: rec.ExitCode, Signal: rec.Signal, FinalMessage: rec.FinalMessage,
			FinalMessageTruncated: rec.FinalMessageTruncated, LogTail: v.LogTail}
		if rec.Late != nil {
			v.LateResult = lateSummary(*rec.Late, e.lateRetained)
		}
	}
	return v
}

// lateSummary is the public summary of late evidence whose tail (retained
// bytes) lives in the task document.
func lateSummary(l contract.LateResult, retained int) *contract.TaskLateSummary {
	lg := l.Log
	l.Log.Data = nil
	s := contract.SummaryOf(l)
	s.Log.RetainedBytes = retained
	s.Log.DroppedBytes = max(0, lg.SourceBytes-retained)
	s.Log.Truncated = lg.ReceivedBytes > retained || lg.Incomplete
	return s
}

// needsFileTailLocked reports that e's tail must come from its document.
func (ts *taskService) needsFileTailLocked(e *taskEntry) bool {
	lg, retained := ts.logOfLocked(e)
	return lg.Data == nil && retained > 0 && e.ring == nil
}

// show returns id's view (tail lines), reading a terminal tail from its
// validated document outside the locks.
func (ts *taskService) show(id string, lines int) (contract.TaskView, error) {
	ts.mu.Lock()
	e := ts.tasks[id]
	if e == nil {
		ts.mu.Unlock()
		return contract.TaskView{}, taskNotFound(id)
	}
	v := ts.viewLocked(e, lines, ts.roles.load().visible, ts.clock.Now())
	file := ts.needsFileTailLocked(e)
	ts.mu.Unlock()
	if file {
		rec, err := ts.st.l.readTaskFile(id, ts.lookup)
		if err != nil {
			return contract.TaskView{}, err
		}
		v.LogTail, v.TailTruncated = contract.LogTail(rec.Log.Data, lines)
		if v.Result != nil {
			v.Result.LogTail = v.LogTail
		}
	}
	return v, nil
}

// logs returns id's complete retained primary output snapshot, or with
// late its late-evidence tail (not_found/no_late_result without one).
// Ordinary logs are never changed by late evidence.
func (ts *taskService) logs(id string, late bool) (contract.TaskLogsResponse, error) {
	ts.mu.Lock()
	e := ts.tasks[id]
	if e == nil {
		ts.mu.Unlock()
		return contract.TaskLogsResponse{}, taskNotFound(id)
	}
	if late {
		has := e.rec.Late != nil
		ts.mu.Unlock()
		if !has {
			return contract.TaskLogsResponse{}, contract.TaskError(contract.CodeNotFound, "", contract.ReasonNoLateResult, "task %s has no late result", id)
		}
		rec, err := ts.st.l.readTaskFile(id, ts.lookup)
		if err != nil {
			return contract.TaskLogsResponse{}, err
		}
		if rec.Late == nil {
			return contract.TaskLogsResponse{}, contract.TaskError(contract.CodeNotFound, "", contract.ReasonNoLateResult, "task %s has no late result", id)
		}
		return logsResponse(id, rec.Late.Log, false), nil
	}
	lg, retained := ts.logOfLocked(e)
	var data []byte
	switch {
	case e.ring != nil && (e.cand == nil || e.cand.kind == candRefusal):
		data = e.ring.tail(contract.MaxLogRetainedBytes)
	case lg.Data != nil:
		data = append([]byte(nil), lg.Data...)
	}
	file := data == nil && retained > 0
	rr := e.isReconciling()
	ts.mu.Unlock()
	if file {
		rec, err := ts.st.l.readTaskFile(id, ts.lookup)
		if err != nil {
			return contract.TaskLogsResponse{}, err
		}
		data, lg = rec.Log.Data, rec.Log
	}
	lg.Data = data
	return logsResponse(id, lg, rr), nil
}

func logsResponse(id string, lg contract.TaskLog, mayBeIncomplete bool) contract.TaskLogsResponse {
	data := lg.Data
	if data == nil {
		data = []byte{}
	}
	m := contract.MetaOf(contract.TaskLog{Data: data, SourceBytes: lg.SourceBytes, ReceivedBytes: lg.ReceivedBytes, Incomplete: lg.Incomplete, CounterOverflow: lg.CounterOverflow}, mayBeIncomplete)
	return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id, Data: data, RetainedBytes: m.RetainedBytes, SourceBytes: m.SourceBytes,
		DroppedBytes: m.DroppedBytes, Truncated: m.Truncated, Incomplete: m.Incomplete, CounterOverflow: m.CounterOverflow, LogMayBeIncomplete: mayBeIncomplete}
}

// list returns up to limit summaries after the exclusive cursor, ordered
// by task ID, with next_after when more rows exist in this snapshot.
func (ts *taskService) list(after string, limit int) ([]contract.TaskSummary, *string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	doc, now := ts.roles.load().visible, ts.clock.Now()
	i := sort.SearchStrings(ts.ids, after)
	if i < len(ts.ids) && ts.ids[i] == after {
		i++
	}
	var out []contract.TaskSummary
	for ; i < len(ts.ids) && len(out) < limit; i++ {
		e := ts.tasks[ts.ids[i]]
		v := ts.viewLocked(e, 0, doc, now)
		out = append(out, contract.TaskSummary{TaskID: v.TaskID, Target: e.rec.Request.Target, Role: v.Role, State: v.State, CreatedAt: v.CreatedAt,
			StartedAt: v.StartedAt, FinishedAt: v.FinishedAt, ElapsedMS: v.ElapsedMS, Effective: v.Effective, RequestedBy: e.rec.Request.RequestedBy,
			Reconciling: v.Reconciling, CompletionPending: v.CompletionPending, PersistenceReason: v.PersistenceReason,
			DurabilityConfirmed: v.DurabilityConfirmed, Reason: v.Reason})
	}
	if i < len(ts.ids) && len(out) > 0 {
		next := out[len(out)-1].TaskID
		return out, &next
	}
	return out, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
