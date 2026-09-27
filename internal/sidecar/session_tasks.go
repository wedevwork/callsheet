package sidecar

import (
	"strconv"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

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
// changed body for a known task is a protocol error; a new start is
// checked against the installed instance and the current local slot
// bound, reserves its slot and hands preparation to a Run-owned worker.
// The reply follows when the outcome is known (pollStart).
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
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: rid, body: contract.TaskStartResult{TaskID: taskID}}
	default:
		rs.pend = &pendingStart{id: rid, w: e.w, e: e}
		rs.pend.timer.set(rs.d.clock, e.w.deadline)
	}
}

// pollStart turns the open start's outcome into its reply. An outcome
// already published wins over the deadline. At the deadline a start still
// preparing is refused (preparation_timeout; its worker is never
// authorized afterwards); one whose Start was authorized but has no
// result yet cannot be answered truthfully either way, so the exchange is
// fenced by ending the session.
func (rs *roleSession) pollStart() error {
	p := rs.pend
	if p == nil {
		return nil
	}
	w := p.w
	w.mu.Lock()
	switch w.phase {
	case phaseStarted:
		w.mu.Unlock()
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: p.id, body: contract.TaskStartResult{TaskID: w.id()}, started: w}
		p.e.settle(nil)
	case phaseRefused:
		refusal := w.refusal
		w.mu.Unlock()
		rs.reply = &reply{typ: contract.FrameTaskStartResult, id: p.id, body: contract.TaskStartResult{TaskID: w.id(), Err: refusal}}
		p.e.settle(refusal)
	default:
		if rs.d.clock.Now().Before(w.deadline) {
			w.mu.Unlock()
			return nil
		}
		if w.phase == phaseAuthorized {
			w.mu.Unlock()
			p.timer.clear()
			rs.pend = nil
			return contract.New(contract.CodeUnavailable, "task "+w.id()+" was authorized to start but its start did not complete within "+prepBudget.String())
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

// nextOutput picks the next task output exchange for this attachment:
// log-ready tasks (unsent retained output) and result-ready tasks (exited,
// their output fully acknowledged) alternate, round-robin by task ID
// inside each class. Only tasks whose start reply was written here and
// that are not fenced qualify.
func (rs *roleSession) nextOutput() (*taskWorker, reqKind) {
	var logs, results []*taskWorker
	for _, w := range rs.tasks.attached(rs.tag) {
		w.mu.Lock()
		ok, exited := w.replied && !w.fenced && !w.resultSent, w.exited
		w.mu.Unlock()
		switch {
		case !ok:
		case w.ring.pending():
			logs = append(logs, w)
		case exited && w.ring.drained():
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
		return w, reqResult
	case len(logs) > 0:
		w := pick(logs, rs.lastLog)
		rs.lastLog, rs.resultTurn = w.id(), true
		return w, reqLog
	}
	return nil, reqHeartbeat
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
	if kind == reqLog {
		c := w.ring.next()
		if c == nil {
			return nil
		}
		o.end = c.offset + len(c.data)
		typ, body = contract.FrameTaskLog, contract.TaskLogBody{TaskID: w.id(), Execution: w.start.Execution, Offset: c.offset, Data: c.data}
	} else {
		w.mu.Lock()
		w.resultSent = true
		res := w.result
		w.mu.Unlock()
		typ, body = contract.FrameTaskResult, res
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
// outstanding output exchange: it must name that task (and, for a log,
// acknowledge at least the chunk's end) and count only when read strictly
// before the exchange's deadline.
func (rs *roleSession) onOutputAck(f contract.NodeFrame, at time.Time) error {
	o := rs.out
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
		if a.TaskID != o.w.id() {
			return invalid(f.RequestID, "task_result_ack "+f.RequestID+" names another task")
		}
	}
	if !at.Before(o.deadline) {
		o.expired = true
		return nil
	}
	o.timer.clear()
	rs.out = nil
	if o.kind == reqLog {
		o.w.ring.ack(o.end)
		rs.emit(event{kind: evLogAcked, id: o.w.id()})
		return nil
	}
	o.w.mu.Lock()
	o.w.acked = true
	o.w.mu.Unlock()
	rs.tasks.gc()
	rs.emit(event{kind: evResultAcked, id: o.w.id()})
	return nil
}
