package sidecar

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// timerSlot is one armed one-shot timer at an absolute instant; re-arming
// at the same instant keeps it, so repeated loop passes register nothing.
type timerSlot struct {
	c    <-chan time.Time
	stop func() bool
	at   time.Time
	on   bool
}

// set arms the timer for the absolute instant at.
func (t *timerSlot) set(c clock, at time.Time) {
	if t.on && t.at.Equal(at) {
		return
	}
	t.clear()
	t.c, t.stop = c.NewTimerAt(at)
	t.at, t.on = at, true
}

func (t *timerSlot) clear() {
	if t.on {
		t.stop()
	}
	t.on, t.c = false, nil
}

// ch is the armed channel or nil.
func (t *timerSlot) ch() <-chan time.Time {
	if t.on {
		return t.c
	}
	return nil
}

// fired marks the timer consumed.
func (t *timerSlot) fired() { t.on, t.c = false, nil }

// reqKind is the kind of a sidecar request (iteration 05: heartbeats,
// task_log and task_result share one b1, b2, ... sequence and one
// in-flight slot).
type reqKind int

const (
	reqHeartbeat reqKind = iota
	reqLog
	reqResult
)

// outstanding is the one sidecar request awaiting its reply.
type outstanding struct {
	id       string
	k        int
	deadline time.Time
	timer    timerSlot
	expired  bool
	kind     reqKind
	// w and end are a task_log's worker and chunk end, or a task_result's
	// worker.
	w   *taskWorker
	end int
}

// startEntry is one start of this attachment's deduplication table: the
// canonical body digest and the start's outcome. While the outcome is
// unknown it references the worker; once known it is a compact immutable
// value (started, or the refusal) and the worker, its request, result and
// output ring are no longer reachable from it. Entries live for the
// attachment's lifetime.
type startEntry struct {
	digest  [32]byte
	w       *taskWorker
	started bool
	refusal *contract.Error
}

// settle replaces the entry's worker with its outcome.
func (e *startEntry) settle(refusal *contract.Error) {
	e.w, e.started, e.refusal = nil, refusal == nil, refusal
}

// pendingStart is the open task_start awaiting its outcome.
type pendingStart struct {
	id    string
	w     *taskWorker
	e     *startEntry
	timer timerSlot
}

// reply is the one pending reply to a plane request.
type reply struct {
	typ, id string
	body    any
	// rev is set for a roles_replace_ack.
	ack bool
	rev int
	// started is a task start's worker whose ok:true this reply carries:
	// its output may flow once the write completes.
	started *taskWorker
}

// snapshot is the installed role configuration of this session.
type snapshot struct {
	any   bool
	rev   int
	roles []contract.RoleRecord
}

// valState is the one registration validation of this session.
type valState struct {
	job      *job
	id       string
	deadline time.Time
	timer    timerSlot
	expired  bool
	replied  bool
}

// cycleState is the running ready-check cycle.
type cycleState struct {
	job       *job
	rev       int
	start     time.Time
	budget    timerSlot
	expired   bool
	abandoned bool
}

// cycleResult is the last complete cycle for a revision.
type cycleResult struct {
	rev    int
	start  time.Time
	passed []bool
}

// roleSession is one connection's protocol state after hello_ok. Only its
// goroutine reads or changes it, installs snapshots, prepares heartbeats
// and writes.
type roleSession struct {
	d        *deps
	s        *sessionConn
	ctx      context.Context
	n        int
	id       string
	env      roleEnv
	w        *workers
	logger   *slog.Logger
	onStable func()
	jobs     context.Context

	k       int
	acks    int
	out     *outstanding
	nextDue time.Time
	due     timerSlot

	lastP int
	open  bool
	reply *reply

	inst   snapshot
	fenced bool

	val *valState

	cyc       *cycleState
	cycleDue  bool
	nextCycle time.Time
	cadence   timerSlot
	last      *cycleResult
	ready     map[string]bool

	// Iteration 05 tasks: the Run's supervisor, this attachment's tag,
	// its execution token (set by the first start), the deduplication
	// table, the open start and the round-robin cursors.
	tasks      *taskSupervisor
	tag        *attachTag
	token      *contract.ExecutionToken
	starts     map[string]*startEntry
	pend       *pendingStart
	lastLog    string
	lastResult string
	resultTurn bool
}

// cleanup stops every timer and cancels the session's worker jobs; the
// workers keep their slots until their work returns (Run joins them).
func (rs *roleSession) cleanup(cancelJobs context.CancelFunc) {
	if rs.out != nil {
		rs.out.timer.clear()
	}
	if rs.pend != nil {
		rs.pend.timer.clear()
	}
	// Task workers survive the session, but never send on another one.
	rs.tasks.fence(rs.tag)
	rs.due.clear()
	if rs.val != nil {
		rs.val.timer.clear()
	}
	if rs.cyc != nil {
		rs.cyc.budget.clear()
	}
	rs.cadence.clear()
	cancelJobs()
}

func (rs *roleSession) emit(e event) {
	e.session = rs.n
	rs.d.emit(e)
}

func (rs *roleSession) run() error {
	rs.nextDue = rs.d.clock.Now()
	for {
		if err := rs.ctx.Err(); err != nil {
			return err
		}
		if r, ok := rs.s.in.take(); ok {
			if err := rs.handle(r); err != nil {
				return err
			}
			continue
		}
		if rs.out != nil && rs.out.expired {
			if rs.out.kind != reqHeartbeat {
				return contract.New(contract.CodeUnavailable, "no task output acknowledgement within "+outputExchange.String())
			}
			return contract.New(contract.CodeUnavailable, "no heartbeat acknowledgement within "+stepTimeout.String())
		}
		rs.pollValidation()
		rs.pollCycle()
		rs.startCycle()
		if err := rs.pollStart(); err != nil {
			return err
		}
		if rs.reply != nil {
			if err := rs.writeReply(); err != nil {
				return err
			}
			continue
		}
		now := rs.d.clock.Now()
		if !rs.fenced && rs.out == nil && !now.Before(rs.nextDue) {
			if err := rs.writeHeartbeat(now); err != nil {
				return err
			}
			continue
		}
		if !rs.fenced && rs.out == nil {
			if w, kind := rs.nextOutput(); w != nil {
				if err := rs.writeOutput(w, kind); err != nil {
					return err
				}
				continue
			}
		}
		if !rs.fenced && rs.out == nil {
			rs.due.set(rs.d.clock, rs.nextDue)
		} else {
			rs.due.clear()
		}
		var outC, valTimer, budget, startTimer <-chan time.Time
		var valDone, cycDone <-chan struct{}
		if rs.out != nil {
			outC = rs.out.timer.ch()
		}
		if rs.pend != nil {
			startTimer = rs.pend.timer.ch()
		}
		if rs.val != nil {
			valDone = rs.val.job.ready
			valTimer = rs.val.timer.ch()
		}
		if rs.cyc != nil {
			cycDone = rs.cyc.job.ready
			budget = rs.cyc.budget.ch()
		}
		select {
		case <-rs.s.in.ready:
		case <-outC:
			rs.out.timer.fired()
			rs.out.expired = true
		case <-rs.due.ch():
			rs.due.fired()
		case <-valDone:
		case <-valTimer:
			rs.val.timer.fired()
			rs.val.expired = true
		case <-cycDone:
		case <-budget:
			rs.cyc.budget.fired()
			rs.cyc.expired = true
		case <-rs.cadence.ch():
			rs.cadence.fired()
			rs.cycleDue = true
		case <-rs.w.freed:
		case <-startTimer:
			rs.pend.timer.fired()
		case <-rs.tasks.notify:
		case <-rs.ctx.Done():
		}
	}
}

// handle processes one delivered message.
func (rs *roleSession) handle(r readResult) error {
	f, err := decode(r)
	if err != nil {
		return err
	}
	switch f.Type {
	case contract.FrameHeartbeatAck, contract.FrameTaskLogAck, contract.FrameTaskResultAck:
		return rs.onAck(f, r.at)
	case contract.FrameRoleValidate, contract.FrameRolesReplace, contract.FrameTaskStart:
		if rs.lastP >= contract.MaxSafeInteger {
			return invalid(f.RequestID, "the plane request counter is exhausted")
		}
		if want := "p" + strconv.Itoa(rs.lastP+1); f.RequestID != want {
			return invalid(f.RequestID, "plane request "+f.RequestID+" is out of sequence (want "+want+")")
		}
		if rs.open {
			return invalid(f.RequestID, "the plane sent "+f.RequestID+" before the reply to its previous request")
		}
		rs.lastP++
		rs.open = true
		switch f.Type {
		case contract.FrameRoleValidate:
			return rs.onValidate(f, r.at)
		case contract.FrameTaskStart:
			return rs.onTaskStart(f, r.at)
		}
		return rs.onReplace(f)
	}
	return invalid(f.RequestID, "unexpected "+f.Type+" message from the plane")
}

func (rs *roleSession) onAck(f contract.NodeFrame, at time.Time) error {
	if rs.out == nil {
		return invalid(f.RequestID, "unexpected "+f.Type+" message from the plane")
	}
	if f.RequestID != rs.out.id {
		return invalid(f.RequestID, "unknown or stale acknowledgement "+f.RequestID+" (want "+rs.out.id+")")
	}
	want := map[reqKind]string{reqHeartbeat: contract.FrameHeartbeatAck, reqLog: contract.FrameTaskLogAck, reqResult: contract.FrameTaskResultAck}[rs.out.kind]
	if f.Type != want {
		return invalid(f.RequestID, "the plane answered "+f.RequestID+" with "+f.Type+" (want "+want+")")
	}
	if rs.out.kind != reqHeartbeat {
		return rs.onOutputAck(f, at)
	}
	if err := contract.DecodeAck(f.Body); err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	// Only an acknowledgement read strictly before the exchange's
	// deadline counts.
	if !at.Before(rs.out.deadline) {
		rs.out.expired = true
		return nil
	}
	rs.out.timer.clear()
	k := rs.out.k
	rs.out = nil
	rs.acks++
	if k == 1 {
		rs.logger.Info("heartbeat acknowledged", "session", rs.n)
	}
	rs.emit(event{kind: evAck, acks: k})
	if rs.acks == stableAcks {
		rs.onStable()
		rs.emit(event{kind: evStable, acks: k})
	}
	return nil
}

// validationReply queues the reply to the open validation request.
func (rs *roleSession) validationReply(id string, err error) {
	var ce *contract.Error
	if err != nil && !asContract(err, &ce) {
		ce = contract.New(contract.CodeInternal, "the role validation failed on this node")
	}
	rs.reply = &reply{typ: contract.FrameRoleValidateResult, id: id, body: contract.RoleValidateResult{Err: ce}}
}

// onValidate checks a candidate: identity, structure and adapter metadata
// at once; files and the executable in the validation worker, whose slot
// admits no queue (a busy slot is unavailable/busy).
func (rs *roleSession) onValidate(f contract.NodeFrame, at time.Time) error {
	raw, err := contract.DecodeRoleValidate(f.Body)
	if err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	if rs.val != nil && rs.val.replied {
		rs.val.timer.clear()
		rs.val = nil
	}
	c, err := contract.ParseResolvedRoleConfig(raw, rs.env.lookup())
	if err == nil && c.Node != rs.id {
		err = contract.RoleError(contract.CodeInvalidArgument, c.ID, rs.id, "node", "", "the role names node %s, but this node is %s", c.Node, rs.id)
	}
	if err != nil {
		rs.validationReply(f.RequestID, err)
		return nil
	}
	var j *job
	if rs.val == nil {
		j = rs.d.startValidation(rs.jobs, rs.w, rs.env, c)
	}
	if j == nil {
		rs.validationReply(f.RequestID, contract.RoleError(contract.CodeUnavailable, c.ID, rs.id, "", contract.ReasonBusy, "node %s is already validating a role; retry", rs.id))
		return nil
	}
	rs.val = &valState{job: j, id: f.RequestID, deadline: at.Add(validationBudget)}
	if rs.val.deadline.After(rs.d.clock.Now()) {
		rs.val.timer.set(rs.d.clock, rs.val.deadline)
	} else {
		rs.val.expired = true
	}
	rs.emit(event{kind: evValidating, id: f.RequestID})
	return nil
}

// pollValidation turns a finished or expired validation into its reply.
// Completion counts only strictly before the deadline; at expiry the reply
// is unavailable/validation_timeout and a late result is discarded, the
// slot staying occupied until the work returns.
func (rs *roleSession) pollValidation() {
	v := rs.val
	if v == nil {
		return
	}
	done, at, err, _ := v.job.result()
	if !v.replied {
		switch {
		case done && at.Before(v.deadline):
			rs.validationReply(v.id, err)
		case done || v.expired:
			v.job.cancel()
			rs.validationReply(v.id, contract.RoleError(contract.CodeUnavailable, "", rs.id, "", contract.ReasonValidationTimeout, "node %s did not finish the role checks within %v", rs.id, validationBudget))
		default:
			return
		}
		v.replied = true
		v.timer.clear()
	}
	if done {
		rs.val = nil
	}
}

// onReplace installs a full snapshot in one step: replace the installed
// revision and roles, drop older readiness, fence heartbeats and queue the
// acknowledgement, the session's next write.
func (rs *roleSession) onReplace(f contract.NodeFrame) error {
	b, err := contract.DecodeRolesReplace(f.Body, rs.env.lookup())
	if err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	switch {
	case b.Revision == 0 && len(b.Roles) > 0:
		return invalid(f.RequestID, "roles_replace revision 0 must be empty")
	case rs.inst.any && b.Revision <= rs.inst.rev:
		return invalid(f.RequestID, "roles_replace revision "+strconv.Itoa(b.Revision)+" is not newer than the installed "+strconv.Itoa(rs.inst.rev))
	}
	for _, r := range b.Roles {
		if r.Node != rs.id {
			return invalid(f.RequestID, "roles_replace carries role "+r.ID+" of another node")
		}
	}
	rs.inst = snapshot{any: true, rev: b.Revision, roles: b.Roles}
	rs.last = nil
	if rs.cyc != nil && !rs.cyc.abandoned {
		rs.cyc.abandoned = true
		rs.cyc.budget.clear()
		rs.cyc.job.cancel()
	}
	rs.cycleDue = false
	rs.cadence.clear()
	rs.fenced = true
	rs.reply = &reply{typ: contract.FrameRolesReplaceAck, id: f.RequestID, body: contract.RolesReplaceAckBody{Revision: b.Revision}, ack: true, rev: b.Revision}
	rs.emit(event{kind: evInstalled, rev: b.Revision, id: f.RequestID})
	return nil
}

func (rs *roleSession) writeReply() error {
	r := rs.reply
	if err := rs.s.write(rs.ctx, r.typ, r.id, r.body); err != nil {
		return err
	}
	rs.reply, rs.open = nil, false
	if r.typ == contract.FrameTaskStartResult {
		if w := r.started; w != nil {
			// Output and the result flow only after the start reply was
			// written on this attachment.
			w.mu.Lock()
			if !w.fenced {
				w.replied = true
			}
			w.mu.Unlock()
		}
		rs.emit(event{kind: evStartReplied, id: r.body.(contract.TaskStartResult).TaskID})
		return nil
	}
	if r.ack {
		rs.fenced = false
		if len(rs.inst.roles) > 0 {
			rs.cycleDue = true
		}
		rs.emit(event{kind: evAckWritten, rev: r.rev, id: r.id})
		return nil
	}
	rs.emit(event{kind: evReplied, id: r.id})
	return nil
}

// statuses computes every installed role's readiness now: the instance's
// local occupied slots (run-lifetime starts and children, matched by
// registration order) are below the concurrency, no earlier child's group
// cleanup is unconfirmed, and the last complete cycle for this revision
// passed it and started less than freshness ago.
func (rs *roleSession) statuses(now time.Time) []contract.RoleStatus {
	out := make([]contract.RoleStatus, len(rs.inst.roles))
	for i, r := range rs.inst.roles {
		n, blocked := rs.tasks.local(keyOf(r))
		ok := rs.last != nil && rs.last.rev == rs.inst.rev && i < len(rs.last.passed) && rs.last.passed[i] &&
			now.Before(rs.last.start.Add(freshness)) && n < r.Concurrency && !blocked
		out[i] = contract.RoleStatus{RoleID: r.ID, Inflight: min(n, contract.MaxConcurrency), Concurrency: r.Concurrency, CanAccept: ok}
	}
	return out
}

// writeHeartbeat assigns the next ID as the write begins; the exchange's
// deadline starts before the write.
func (rs *roleSession) writeHeartbeat(now time.Time) error {
	rs.k++
	o := &outstanding{id: "b" + strconv.Itoa(rs.k), k: rs.k, deadline: now.Add(stepTimeout)}
	rs.nextDue = now.Add(heartbeatInterval)
	body := contract.HeartbeatBody{RolesRevision: rs.inst.rev, Roles: rs.statuses(now)}
	if err := rs.s.writeBy(rs.ctx, o.deadline, contract.FrameHeartbeat, o.id, body); err != nil {
		return err
	}
	rs.out = o
	if o.deadline.After(rs.d.clock.Now()) {
		o.timer.set(rs.d.clock, o.deadline)
	} else {
		o.expired = true
	}
	rs.emit(event{kind: evAwaitReply, acks: o.k, rev: body.RolesRevision, statuses: body.Roles})
	return nil
}

// startCycle starts a due cycle when the slot is free: never overlapping,
// one pending tick at most, cadence from the previous start.
func (rs *roleSession) startCycle() {
	if !rs.cycleDue || rs.fenced || len(rs.inst.roles) == 0 || rs.cyc != nil {
		return
	}
	start := rs.d.clock.Now()
	j := rs.d.startCycle(rs.jobs, rs.w, rs.env, rs.inst.roles)
	if j == nil {
		return // an earlier cycle still occupies the slot
	}
	rs.cycleDue = false
	rs.cyc = &cycleState{job: j, rev: rs.inst.rev, start: start}
	rs.cyc.budget.set(rs.d.clock, start.Add(cycleBudget))
	rs.nextCycle = start.Add(cycleInterval)
	if !rs.d.noCadence {
		rs.cadence.set(rs.d.clock, rs.nextCycle)
	}
	rs.emit(event{kind: evCycleStarted, rev: rs.inst.rev})
}

// pollCycle publishes a finished cycle atomically if it completed within
// its budget for the still installed revision; at budget expiry the cycle
// publishes all-false and its late result is discarded.
func (rs *roleSession) pollCycle() {
	c := rs.cyc
	if c == nil {
		return
	}
	done, at, _, passed := c.job.result()
	if !c.abandoned {
		switch {
		case done && at.Before(c.start.Add(cycleBudget)):
			if c.rev == rs.inst.rev {
				rs.publish(&cycleResult{rev: c.rev, start: c.start, passed: passed})
			}
		case done || c.expired:
			c.job.cancel()
			if c.rev == rs.inst.rev {
				rs.publish(&cycleResult{rev: c.rev, start: c.start, passed: make([]bool, len(rs.inst.roles))})
			}
		default:
			return
		}
		c.abandoned = true
		c.budget.clear()
	}
	if done {
		rs.cyc = nil
	}
}

// publish adopts a cycle result and logs readiness transitions.
func (rs *roleSession) publish(res *cycleResult) {
	rs.last = res
	if rs.ready == nil {
		rs.ready = map[string]bool{}
	}
	for i, r := range rs.inst.roles {
		ok := i < len(res.passed) && res.passed[i]
		if was, seen := rs.ready[r.ID]; !seen || was != ok {
			if ok {
				rs.logger.Info("role ready", "role_id", r.ID)
			} else {
				rs.logger.Warn("role not ready", "role_id", r.ID)
			}
			rs.ready[r.ID] = ok
		}
	}
	rs.emit(event{kind: evCycleDone, rev: res.rev, passed: append([]bool(nil), res.passed...)})
}
