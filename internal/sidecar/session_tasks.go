package sidecar

import (
	"strconv"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// errPrepOverrun ends an attachment whose start was authorized but did not
// complete within the preparation budget (iteration 06a DW6): no start
// reply is sent (an uncertain start is neither a success nor a refusal),
// the WebSocket closes at once with 1001 and prepOverrunReason (distinct
// from a graceful shutdown's), the error is retryable, and the worker,
// its slot and journal are kept for the next attachment's inventory.
type prepOverrunError struct{ err *contract.Error }

func (e *prepOverrunError) Error() string { return e.err.Error() }
func (e *prepOverrunError) Unwrap() error { return e.err }

// prepOverrunReason is the close reason of a preparation overrun.
const prepOverrunReason = "task preparation overrun"

// installedInstance returns the installed record of the admitted role's
// instance: same ID, node and registration order.
func (rs *roleSession) installedInstance(role contract.RoleRecord) (contract.RoleRecord, bool) {
	for _, r := range rs.inst.roles {
		if r.ID == role.ID {
			return r, r.Node == role.Node && r.RegistrationOrder == role.RegistrationOrder
		}
	}
	return contract.RoleRecord{}, false
}

// onTaskStart handles a task_start: the body and the attachment's
// execution token are checked strictly (protocol errors close the
// stream); an identical duplicate returns its recorded outcome and a
// changed body for a known task is a protocol error; a start for an
// execution this Run already holds from an earlier attachment is a replay,
// never accepted; a new start is checked against the installed instance
// and the current local slot bound, reserves its slot and hands
// preparation to a Run-owned worker. The reply follows when the outcome
// is known (pollStart).
func (rs *roleSession) onTaskStart(f contract.NodeFrame, at time.Time) error {
	b, err := contract.DecodeTaskStart(f.Body, rs.env.lookup())
	if err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	if rs.token == nil {
		tok := b.Execution
		rs.token = &tok
	} else if *rs.token != b.Execution {
		return invalid(f.RequestID, "task_start "+b.TaskID+" carries an execution token other than this attachment's")
	}
	digest := b.Digest()
	if e, ok := rs.starts[b.TaskID]; ok {
		if e.digest != digest {
			return invalid(f.RequestID, "task_start "+b.TaskID+" repeats a known task with different data")
		}
		rs.answerStart(f.RequestID, b.TaskID, e)
		return nil
	}
	if rs.tasks.known(b.TaskID) {
		return invalid(f.RequestID, "task_start "+b.TaskID+" replays an execution this worker already holds")
	}
	e := &startEntry{digest: digest}
	rs.starts[b.TaskID] = e
	inst, current := rs.installedInstance(b.Role)
	switch {
	case b.Role.Node != rs.id || !current:
		e.refusal = refuse(contract.ReasonRoleMissing)
	case b.RolesRevision > rs.inst.rev:
		e.refusal = refuse(contract.ReasonRoleChanged)
	case !rs.tasks.reserve(keyOf(inst), inst.Concurrency):
		e.refusal = refuse(contract.ReasonLocalFull)
	default:
		e.w = rs.tasks.begin(b, rs.tag, at.Add(prepBudget))
	}
	rs.answerStart(f.RequestID, b.TaskID, e)
	return nil
}

// answerStart queues the reply for a start whose outcome is known, or
// waits for its worker (bounded by the preparation deadline).
func (rs *roleSession) answerStart(rid, taskID string, e *startEntry) {
	switch {
	case e.refusal != nil:
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: rid, body: contract.TaskStartResult{TaskID: taskID, Err: e.refusal}}
	case e.started:
		// A duplicate of an answered start: its recorded outcome.
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: rid, body: contract.TaskStartResult{TaskID: taskID, Preparing: e.preparing}}
	default:
		// A workspace start's worker replaces the deadline under w.mu
		// (prepareWorkspace) concurrently with this read.
		e.w.mu.Lock()
		deadline := e.w.deadline
		e.w.mu.Unlock()
		rs.pend = &pendingStart{id: rid, w: e.w, e: e}
		rs.pend.timer.set(rs.d.clock, deadline)
	}
}

// pollStart turns the open start's outcome into its reply. An outcome
// already published wins over the deadline. At the deadline a start still
// preparing is refused (preparation_timeout; its worker is never
// authorized afterwards and revokes its guardian's release); one whose
// running authorization began but whose launch is not confirmed cannot be
// answered truthfully either way (DW6): no reply, and the attachment
// closes at once.
func (rs *roleSession) pollStart() error {
	p := rs.pend
	if p == nil {
		return nil
	}
	w := p.w
	w.mu.Lock()
	switch {
	case w.phase == phaseStarted:
		w.mu.Unlock()
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: p.id, body: contract.TaskStartResult{TaskID: w.id()}, started: w}
		p.e.settle(nil)
	case w.phase == phaseRefused:
		refusal := w.refusal
		w.mu.Unlock()
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: p.id, body: contract.TaskStartResult{TaskID: w.id(), Err: refusal}}
		p.e.settle(refusal)
	case w.prepReply:
		// Iteration 10b: a workspace start's durable reservation and
		// journal, definitely no adapter yet: answered preparing (the
		// outcome follows as task_prepared).
		w.mu.Unlock()
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: p.id, body: contract.TaskStartResult{TaskID: w.id(), Preparing: true}, started: w}
		p.e.settle(nil)
		p.e.preparing = true
	default:
		if rs.d.clock.Now().Before(w.deadline) {
			w.mu.Unlock()
			return nil
		}
		if w.phase == phaseAuthorized {
			w.mu.Unlock()
			p.timer.clear()
			rs.pend = nil
			rs.emit(event{kind: evPreparationFenced, id: w.id()})
			return &prepOverrunError{err: contract.New(contract.CodeUnavailable, "task "+w.id()+" was authorized to start but its launch did not complete within "+prepBudget.String()+"; the worker keeps it for the next connection's inventory")}
		}
		w.expired, w.phase, w.refusal = true, phaseRefused, refuse(contract.ReasonPreparationTimeout)
		refusal := w.refusal
		w.mu.Unlock()
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: p.id, body: contract.TaskStartResult{TaskID: w.id(), Err: refusal}}
		p.e.settle(refusal)
	}
	p.timer.clear()
	rs.pend = nil
	return nil
}

// outputState is one attached worker's transmission view, sampled under
// its lock.
type outputState struct {
	replied, late, waitAck, committed bool
	// cleaning: a previous Run's recorded group is still being cleaned;
	// its outcome is reported once the cleanup attempt ended.
	cleaning bool
	outcome  *contract.TaskResultBody
	nextSend time.Time
	// prepared: a task_prepared awaits its exchange on this attachment.
	prepared bool
}

func (w *taskWorker) outputState() outputState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return outputState{replied: w.replied, late: w.late, waitAck: w.waitAck, committed: w.committed, cleaning: w.recovered && w.cleaning,
		outcome: w.outcome, nextSend: w.nextSend, prepared: w.prepOut != nil && !w.prepSent}
}

// nextOutput picks the next task output exchange for this attachment (a
// recovered execution only after its group's cleanup attempt ended):
// log-ready tasks (unsent retained output; a replayed tail only once its
// outcome's digest is frozen) and result-ready tasks (a journaled outcome,
// its output fully acknowledged, not committed, its retry due) alternate,
// round-robin by task ID inside each class. Only workers bound to this
// attachment (a start reply written here, or reconciled here) qualify.
// It also returns the earliest future result retry (zero: none).
func (rs *roleSession) nextOutput(now time.Time) (*taskWorker, reqKind, time.Time) {
	var logs, results []*taskWorker
	var retry time.Time
	for _, w := range rs.tasks.attached(rs.tag) {
		st := w.outputState()
		if !st.replied || st.cleaning {
			continue
		}
		if st.prepared {
			// Iteration 10b: a preparation outcome goes first (its child
			// waits behind the release barrier).
			return w, reqPrepared, retry
		}
		switch {
		case w.ring.pending() && (!st.late || st.outcome != nil):
			logs = append(logs, w)
		case st.outcome != nil && !st.waitAck && !st.committed && w.ring.drained():
			if now.Before(st.nextSend) {
				if retry.IsZero() || st.nextSend.Before(retry) {
					retry = st.nextSend
				}
				continue
			}
			results = append(results, w)
		}
	}
	pick := func(ws []*taskWorker, last string) *taskWorker {
		for _, w := range ws {
			if w.id() > last {
				return w
			}
		}
		return ws[0]
	}
	switch {
	case len(results) > 0 && (rs.resultTurn || len(logs) == 0):
		w := pick(results, rs.lastResult)
		rs.lastResult, rs.resultTurn = w.id(), false
		return w, reqResult, retry
	case len(logs) > 0:
		w := pick(logs, rs.lastLog)
		rs.lastLog, rs.resultTurn = w.id(), true
		return w, reqLog, retry
	}
	return nil, reqHeartbeat, retry
}

// writeOutput sends one task_log or task_result as the next b request.
// Its receipt exchange is bounded to outputExchange from this wire-slot
// reservation, write included; no disk work is awaited by the plane's
// acknowledgement.
func (rs *roleSession) writeOutput(w *taskWorker, kind reqKind) error {
	if rs.k >= contract.MaxSafeInteger {
		return contract.New(contract.CodeUnavailable, "the sidecar request counter is exhausted")
	}
	now := rs.d.clock.Now()
	o := &outstanding{id: "b" + strconv.Itoa(rs.k+1), k: rs.k + 1, deadline: now.Add(outputExchange), kind: kind, w: w}
	var typ string
	var body any
	st := w.outputState()
	if kind == reqPrepared {
		w.mu.Lock()
		b := w.prepOut
		w.prepSent = b != nil
		w.mu.Unlock()
		if b == nil {
			return nil
		}
		rs.k++
		if err := rs.s.writeBy(rs.ctx, o.deadline, contract.FrameTaskPrepared, o.id, *b); err != nil {
			return err
		}
		rs.out = o
		if o.deadline.After(rs.d.clock.Now()) {
			o.timer.set(rs.d.clock, o.deadline)
		} else {
			o.expired = true
		}
		rs.emit(event{kind: evOutputWritten, id: o.id})
		return nil
	}
	if kind == reqLog {
		c := w.ring.next()
		if c == nil {
			return nil
		}
		o.end = c.offset + len(c.data)
		lb := contract.TaskLogBody{TaskID: w.id(), Execution: w.start.Execution, Offset: c.offset, Data: c.data}
		if st.late {
			d := st.outcome.Digest
			lb.LateDigest = &d
		}
		typ, body = contract.FrameTaskLog, lb
	} else {
		w.mu.Lock()
		w.waitAck = true
		w.mu.Unlock()
		o.digest = st.outcome.Digest
		typ, body = contract.FrameTaskResult, *st.outcome
	}
	rs.k++
	if err := rs.s.writeBy(rs.ctx, o.deadline, typ, o.id, body); err != nil {
		return err
	}
	rs.out = o
	if o.deadline.After(rs.d.clock.Now()) {
		o.timer.set(rs.d.clock, o.deadline)
	} else {
		o.expired = true
	}
	rs.emit(event{kind: evOutputWritten, id: o.id})
	return nil
}

// onOutputAck handles the plane's task_log_ack or task_result_ack for the
// outstanding output exchange: it must name that task (a log's must
// acknowledge at least the chunk's end, a result's its digest) and counts
// only when read strictly before the exchange's deadline. A committed
// result acknowledgement authorizes the outbox's deletion; an uncommitted
// one schedules the same result's retry.
func (rs *roleSession) onOutputAck(f contract.NodeFrame, at time.Time) error {
	o := rs.out
	var committed bool
	if o.kind == reqPrepared {
		return rs.onPreparedAck(f, at)
	}
	if o.kind == reqLog {
		a, err := contract.DecodeTaskLogAck(f.Body)
		if err != nil {
			return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
		}
		if a.TaskID != o.w.id() || a.NextOffset < o.end {
			return invalid(f.RequestID, "task_log_ack "+f.RequestID+" does not acknowledge the chunk sent")
		}
	} else {
		a, err := contract.DecodeTaskResultAck(f.Body)
		if err != nil {
			return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
		}
		if a.TaskID != o.w.id() || a.Digest != o.digest {
			return invalid(f.RequestID, "task_result_ack "+f.RequestID+" names another task or result")
		}
		committed = a.Committed
	}
	if !at.Before(o.deadline) {
		o.expired = true
		return nil
	}
	o.timer.clear()
	rs.out = nil
	if o.kind == reqLog {
		o.w.ring.ack(o.end)
		if o.w.recovered {
			rs.tasks.kickReplay()
		}
		rs.emit(event{kind: evLogAcked, id: o.w.id()})
		return nil
	}
	o.w.mu.Lock()
	o.w.waitAck = false
	if !committed {
		o.w.nextSend = at.Add(resultRetry)
	}
	o.w.mu.Unlock()
	rs.emit(event{kind: evResultAcked, id: o.w.id()})
	if committed {
		o.w.markCommitted()
		rs.tasks.kickJanitor()
		rs.emit(event{kind: evResultCommitted, id: o.w.id()})
	}
	return nil
}

// onPreparedAck handles the plane's task_prepared_ack (iteration 10b): it
// must name the outstanding execution and counts only strictly before the
// exchange's deadline; its release decision is handed to the worker (a
// late acknowledgement is not counted: the request is resent on the next
// attachment, where the plane replays the same decision).
func (rs *roleSession) onPreparedAck(f contract.NodeFrame, at time.Time) error {
	o := rs.out
	a, err := contract.DecodeTaskPreparedAck(f.Body)
	if err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	if a.TaskID != o.w.id() || a.Execution != o.w.start.Execution {
		return invalid(f.RequestID, "task_prepared_ack "+f.RequestID+" names another execution")
	}
	if !at.Before(o.deadline) {
		o.expired = true
		return nil
	}
	o.timer.clear()
	rs.out = nil
	o.w.mu.Lock()
	pending := o.w.prepOut != nil
	o.w.prepOut, o.w.prepSent = nil, false
	o.w.mu.Unlock()
	if pending {
		o.w.release <- a.Release
	}
	rs.emit(event{kind: evPreparedAcked, id: o.w.id()})
	return nil
}

// ---- Inventory and reconciliation (iteration 06a) ----

// inventoryPage builds page n of this attachment's frozen inventory:
// metadata only (never prompts or output), at most MaxInventoryEntries
// entries from existing per-task state; executions forgotten since the
// freeze are omitted. final reports the last page (an empty inventory is
// page 0, final, without entries).
func (rs *roleSession) inventoryPage(n int) (contract.TaskInventoryBody, bool) {
	pages := max(1, (len(rs.invIDs)+contract.MaxInventoryEntries-1)/contract.MaxInventoryEntries)
	final := n == pages-1
	b := contract.TaskInventoryBody{RunID: rs.tasks.runID, Page: n, Final: final, Entries: []contract.TaskInventoryEntry{}}
	lo := n * contract.MaxInventoryEntries
	hi := min(len(rs.invIDs), lo+contract.MaxInventoryEntries)
	for _, id := range rs.invIDs[lo:hi] {
		w := rs.tasks.find(id)
		if w == nil {
			continue
		}
		if e, ok := w.inventoryEntry(); ok {
			b.Entries = append(b.Entries, e)
		}
	}
	return b, final
}

// inventoryEntry is w's inventory entry, if it is still an execution:
// preparing before its running authorization, running until its outcome
// is frozen (started_at once the adapter launched), then result or lost
// with the outcome's digest. A definitely refused start is none.
func (w *taskWorker) inventoryEntry() (contract.TaskInventoryEntry, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := contract.TaskInventoryEntry{TaskID: w.id(), Execution: w.start.Execution, StartDigest: w.digest}
	switch {
	case w.outcome != nil:
		d := w.outcome.Digest
		e.ResultDigest, e.Phase = &d, contract.PhaseResult
		if w.outcome.Outcome == contract.OutcomeLost {
			e.Phase = contract.PhaseLost
		}
		if w.started != nil {
			s := contract.FormatTime(*w.started)
			e.StartedAt = &s
		}
	case w.phase == phaseRefused:
		return e, false
	case w.phase == phasePreparing:
		e.Phase = contract.PhasePreparing
	default:
		e.Phase = contract.PhaseRunning
		if w.started != nil {
			s := contract.FormatTime(*w.started)
			e.StartedAt = &s
		}
	}
	return e, true
}

// writeInventory sends the next inventory page as request i<page+1> in
// the sidecar's one request slot, bounded by inventoryExchange.
func (rs *roleSession) writeInventory() error {
	now := rs.d.clock.Now()
	body, final := rs.inventoryPage(rs.invPage)
	o := &outstanding{id: "i" + strconv.Itoa(rs.invPage+1), deadline: now.Add(inventoryExchange), kind: reqInventory, page: rs.invPage, final: final}
	if err := rs.s.writeBy(rs.ctx, o.deadline, contract.FrameTaskInventory, o.id, body); err != nil {
		return err
	}
	rs.out = o
	if o.deadline.After(rs.d.clock.Now()) {
		o.timer.set(rs.d.clock, o.deadline)
	} else {
		o.expired = true
	}
	rs.emit(event{kind: evOutputWritten, id: o.id})
	return nil
}

// onInventoryAck handles the acknowledgement of the outstanding page: a
// receipt, not reconciliation.
func (rs *roleSession) onInventoryAck(f contract.NodeFrame, at time.Time) error {
	o := rs.out
	a, err := contract.DecodeTaskInventoryAck(f.Body)
	if err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	if a.Page != o.page {
		return invalid(f.RequestID, "task_inventory_ack "+f.RequestID+" names another page")
	}
	if !at.Before(o.deadline) {
		o.expired = true
		return nil
	}
	o.timer.clear()
	rs.out = nil
	rs.invPage++
	rs.invDone = o.final
	rs.emit(event{kind: evInventoryAcked, rev: a.Page})
	return nil
}

// onReconcile applies one reconcile page after the final inventory page's
// acknowledgement and queues its acknowledgement (the session's next
// write; nothing here waits for disk or a process): continue binds a
// held execution to this attachment; send_result binds it with its
// replayed tail tagged by its outcome digest; stop_lost binds it so and
// asks its guardian to clean the group (the plane state never changes);
// forget authorizes its outbox's deletion once its cleanup is confirmed.
// The final page's acknowledgement ends reconciliation.
func (rs *roleSession) onReconcile(f contract.NodeFrame) error {
	if !rs.invDone {
		return invalid(f.RequestID, "task_reconcile before the final inventory page was acknowledged")
	}
	if rs.lastR >= contract.MaxSafeInteger {
		return invalid(f.RequestID, "the plane reconcile counter is exhausted")
	}
	if want := "r" + strconv.Itoa(rs.lastR+1); f.RequestID != want {
		return invalid(f.RequestID, "task_reconcile "+f.RequestID+" is out of sequence (want "+want+")")
	}
	if rs.open {
		return invalid(f.RequestID, "the plane sent "+f.RequestID+" before the reply to its previous request")
	}
	b, err := contract.DecodeTaskReconcile(f.Body)
	if err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	rs.lastR++
	rs.open = true
	for _, e := range b.Entries {
		w := rs.tasks.find(e.TaskID)
		if w == nil || w.start.Execution != e.Execution {
			rs.logger.Warn("reconciliation named an execution this worker does not hold", "task_id", e.TaskID, "action", e.Action)
			continue
		}
		switch e.Action {
		case contract.ActionForget:
			w.markCommitted()
			rs.tasks.kickJanitor()
		default:
			w.mu.Lock()
			w.tag, w.replied, w.late = rs.tag, true, e.Action != contract.ActionContinue && e.Action != contract.ActionStopControl
			w.mu.Unlock()
			switch e.Action {
			case contract.ActionStopLost:
				w.requestStop()
			case contract.ActionStopControl:
				// The durable plane intent is latched before this page's
				// acknowledgement; the worker drives the cleanup.
				rs.tasks.control(w, *e.Stop)
			}
		}
		rs.emit(event{kind: evDisposed, id: e.TaskID, action: e.Action})
	}
	rs.tasks.kickReplay()
	rs.reply = &reply{typ: contract.FrameTaskReconcileAck, id: f.RequestID, body: contract.TaskReconcileAckBody{Received: true}, reconFinal: b.Final}
	rs.tasks.signal()
	return nil
}

// errExecutionMissing answers a control naming an execution this worker
// does not hold (iteration 06b): the bounded error message with
// unavailable/execution_missing; the plane closes the attachment and
// reconciles on the next one. It is retryable, never a terminal
// configuration error.
func errExecutionMissing(rid, id string) error {
	return &protocolError{requestID: rid, err: contract.TaskError(contract.CodeUnavailable, "", contract.ReasonExecutionMissing,
		"this worker holds no execution of task %s", id)}
}

// onTaskCancel handles a task_cancel (a plane request p<n>, iteration
// 06b): the body is strictly decoded (a timed_out kind or a malformed body
// is a protocol error); the worker holding exactly this stable execution
// latches the control (receipt, not cleanup: it is acknowledged before any
// journal or FIFO I/O, which its Run-owned worker performs); the same stop
// again, or another stop for an already latched or decided execution, is
// acknowledged without applying anything twice. A worker without it
// answers execution_missing.
func (rs *roleSession) onTaskCancel(f contract.NodeFrame) error {
	b, err := contract.DecodeTaskCancel(f.Body)
	if err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	w := rs.tasks.find(b.TaskID)
	if w == nil || w.start.Execution != b.Execution {
		return errExecutionMissing(f.RequestID, b.TaskID)
	}
	w.mu.Lock()
	gone := w.forgotten || (w.phase == phaseRefused && w.outcome == nil)
	w.mu.Unlock()
	if gone {
		return errExecutionMissing(f.RequestID, b.TaskID)
	}
	rs.tasks.control(w, contract.StopIntent{ID: b.StopID, Kind: b.Kind, RequestedAt: rs.d.clock.Now().UTC()})
	rs.reply = &reply{typ: contract.FrameTaskCancelAck, id: f.RequestID,
		body: contract.TaskCancelAckBody{TaskID: b.TaskID, Execution: b.Execution, StopID: b.StopID, Received: true}}
	return nil
}
