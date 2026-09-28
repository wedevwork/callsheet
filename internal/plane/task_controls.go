package plane

import (
	"context"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Task controls on the plane (iteration 06b, controls.md): the durable
// stop intent and its cancel responses, control delivery to the reporting
// attachment, and bounded any-of waits woken at terminal commit. Every
// decision is made under the task observation lock by the single writer's
// candidates; no disk or network I/O happens under it.

// ---- Cancel ----

// newStopID returns 32 random lowercase hex digits.
func (ts *taskService) newStopID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(ts.rand, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// definitelyUnsentLocked reports that e's start can be proved never
// written in this plane Run: not loaded, still pending, and not claimed
// for its write (claimStart rejects a selected intent afterwards).
func (e *taskEntry) definitelyUnsentLocked() bool {
	return !e.loaded && e.rec.State == contract.TaskPending && (e.send == sendNone || e.send == sendQueued || e.send == sendUnsent)
}

// latchCancelledBeforeStartLocked latches e's control terminal cancelled
// without start, exit or signal (reason cancelled_before_start): no
// adapter ever ran, so no cleanup remains.
func (ts *taskService) latchCancelledBeforeStartLocked(e *taskEntry) {
	t := ts.clock.Now().UTC()
	o := terminalOutcome{state: contract.TaskCancelled, finished: t,
		reason: &contract.TaskReason{Code: contract.ReasonCancelledBeforeStart, Message: "the task was cancelled before any adapter started"}}
	if e.ring != nil {
		o.log = e.ring.snapshot()
	} else {
		o.log = e.rec.Log
		o.log.Data = append([]byte(nil), e.rec.Log.Data...)
	}
	e.cand = &terminalCand{kind: candControl, at: t, outcome: o}
	e.needRunning = false
	e.reconciling = false
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.event("cancelled-before-start " + e.rec.TaskID)
}

// selectIntentLocked selects e's one frozen stop intent (a nonterminal
// writer publication). A start this Run can prove unsent is withdrawn
// atomically here and cancelled before start: its transport token is
// released now (exactly once) and it is never sent later.
func (ts *taskService) selectIntentLocked(e *taskEntry, id string) {
	e.pendingIntent = &contract.StopIntent{ID: id, Kind: contract.StopKindCancelled, RequestedAt: ts.clock.Now().UTC()}
	ts.event("stop-intent-queued " + e.rec.TaskID)
	if e.definitelyUnsentLocked() {
		e.send = sendUnsent
		e.res.release()
		ts.latchCancelledBeforeStartLocked(e)
	}
	ts.refreshLocked(e)
	ts.wakeLocked(e)
}

// cancel requests cancellation of task id within the 6 s mutation budget.
// An already durable terminal task answers accepted=false (200). A latched
// natural, lost or refusal candidate is not displaced: the request waits
// for its commit and then answers accepted=false. Otherwise one stop
// intent is selected (an existing one is joined: no second ID, no reset)
// and the answer is accepted=true once it is durable (202). Before
// selection the caller may abort; after it, the caller only detaches, and
// a budget or storage timeout is unavailable (the intent can still
// commit: inspect and retry). The response never waits for termination.
func (ts *taskService) cancel(ctx context.Context, id string) (contract.TaskView, bool, error) {
	deadline := ts.clock.Now().Add(admissionTimeout)
	mctx, stop := clockDeadline(ctx, ts.clock, deadline)
	defer stop()
	ts.at("cancel-received", id, mctx)
	stopID, err := ts.newStopID()
	if err != nil {
		return contract.TaskView{}, false, wrapf(contract.CodeInternal, err, "cannot generate a stop ID: %v", err)
	}
	ts.mu.Lock()
	e := ts.tasks[id]
	if e == nil {
		ts.mu.Unlock()
		return contract.TaskView{}, false, taskNotFound(id)
	}
	// The request's own resolution is decided at entry: an already durably
	// terminal task is 200; one with (or getting) an intent is accepted once
	// the intent is durable; a latched candidate without one is 200 once
	// it commits.
	durableTerminal := e.terminal() && e.released
	withIntent := false
	if !durableTerminal {
		if e.intentLocked() == nil && !e.terminal() && e.cand == nil {
			if err := mctx.Err(); err != nil {
				ts.mu.Unlock()
				return contract.TaskView{}, false, ts.cancelAbort(ctx, id)
			}
			ts.selectIntentLocked(e, stopID)
		}
		withIntent = e.intentLocked() != nil
	}
	for {
		done := durableTerminal || (withIntent && e.intentDurable) || (!withIntent && e.terminal() && e.released)
		if done {
			ts.mu.Unlock()
			v, err := ts.show(id, contract.DefaultTailLines)
			return v, withIntent, err
		}
		w := e.waiterLocked()
		ts.mu.Unlock()
		select {
		case <-w:
			ts.mu.Lock()
			continue
		case <-mctx.Done():
		case <-ts.stop:
		}
		ts.mu.Lock()
		e.dropWaiterLocked(w)
		if (withIntent && e.intentDurable) || (!withIntent && e.terminal() && e.released) {
			continue
		}
		ts.mu.Unlock()
		return contract.TaskView{}, false, ts.cancelAbort(ctx, id)
	}
}

// cancelAbort is a detached cancel response: the caller's own
// cancellation, or unavailable (budget, storage or shutdown), never a
// claim that nothing was requested.
func (ts *taskService) cancelAbort(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return contract.TaskError(contract.CodeUnavailable, "", "", "the cancellation of task %s was not confirmed durable in time; it may still commit: inspect callsheet task show %s and retry (a retry is safe)", id, id)
}

// requestCancel selects task id's stop intent without waiting (a forced
// role removal's fan-out): a latched or terminal task and an existing
// intent are left as they are.
func (ts *taskService) requestCancel(id string) error {
	stopID, err := ts.newStopID()
	if err != nil {
		return err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil || e.terminal() || e.cand != nil || e.intentLocked() != nil {
		return nil
	}
	ts.selectIntentLocked(e, stopID)
	return nil
}

// heldIDs returns the task IDs holding instance k's reservations, sorted.
func (ts *taskService) heldIDs(k instanceKey) []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []string
	for _, id := range ts.ids {
		if e := ts.tasks[id]; e.key == k && e.held() {
			out = append(out, id)
		}
	}
	return out
}

// ---- Control delivery ----

// controlItem is one task_cancel to write on an attachment.
type controlItem struct {
	id   string
	body contract.TaskCancelBody
}

// deliverableLocked reports that e's durable intent should reach attachment
// gen (the attachment authorized to report on it) now or later.
func (e *taskEntry) deliverableLocked(gen uint64) bool {
	return e.intentDurable && !e.terminal() && e.cand == nil && e.rgen != 0 && e.rgen == gen
}

// wantControlLocked records that e's durable intent is to be delivered to
// its reporting attachment, and wakes that attachment's session.
func (ts *taskService) wantControlLocked(e *taskEntry) {
	if !e.deliverableLocked(e.rgen) {
		return
	}
	node := e.rec.Role.Node
	m := ts.ctlNodes[node]
	if m == nil {
		m = map[string]bool{}
		ts.ctlNodes[node] = m
	}
	m[e.rec.TaskID] = true
	ts.reg.kick(node)
	ts.event("control-wanted " + e.rec.TaskID)
}

// dropControlLocked forgets e's pending control delivery.
func (ts *taskService) dropControlLocked(e *taskEntry) {
	node := e.rec.Role.Node
	if m := ts.ctlNodes[node]; m != nil {
		delete(m, e.rec.TaskID)
		if len(m) == 0 {
			delete(ts.ctlNodes, node)
		}
	}
	e.ctlInflight = false
}

// takeControl returns the next due control for attachment gen of node,
// round-robin after the task ID last, and the earliest later due instant
// (zero: none). Undeliverable entries are dropped.
func (ts *taskService) takeControl(node string, gen uint64, now time.Time, last string) (*controlItem, time.Time) {
	if ts == nil {
		return nil, time.Time{}
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	m := ts.ctlNodes[node]
	if len(m) == 0 {
		return nil, time.Time{}
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	i := sort.SearchStrings(ids, last)
	if i < len(ids) && ids[i] == last {
		i++
	}
	var next time.Time
	var pick *taskEntry
	for k := 0; k < len(ids); k++ {
		id := ids[(i+k)%len(ids)]
		e := ts.tasks[id]
		if e == nil || !e.intentDurable || e.terminal() || e.cand != nil {
			delete(m, id)
			continue
		}
		if e.rgen != gen || e.ctlInflight {
			continue
		}
		if now.Before(e.ctlDue) {
			if next.IsZero() || e.ctlDue.Before(next) {
				next = e.ctlDue
			}
			continue
		}
		if pick == nil {
			pick = e
		}
	}
	if len(m) == 0 {
		delete(ts.ctlNodes, node)
	}
	if pick == nil {
		return nil, next
	}
	pick.ctlInflight = true
	in := pick.rec.StopIntent
	return &controlItem{id: pick.rec.TaskID, body: contract.TaskCancelBody{TaskID: pick.rec.TaskID, Execution: pick.rec.Execution, StopID: in.ID,
		Kind: in.Kind}}, next
}

// hasControl reports whether node has a control due or pending for gen.
func (ts *taskService) hasControl(node string) bool {
	if ts == nil {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.ctlNodes[node]) > 0
}

// controlDone ends task id's control exchange on attachment gen: a receipt
// (acked at at) schedules the coalesced redelivery storageRetry later while
// the task stays undecided; a failed exchange leaves it to the next
// attachment's reconciliation.
func (ts *taskService) controlDone(id string, acked bool, at time.Time) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil {
		return
	}
	e.ctlInflight = false
	if acked {
		e.ctlDue = at.Add(storageRetry)
		ts.event("control-acked " + id)
	}
}

// ---- Bounded wait ----

// maxWaitCalls bounds the plane-wide active wait registrations.
const maxWaitCalls = 1024

// waitReg is one registered wait: its ids in caller order, the private
// wake channel (closed once) and the winner (set under the task lock in
// the terminal confirmation's critical section).
type waitReg struct {
	ids    []string
	ch     chan struct{}
	winner string
	done   bool
}

// wakeWaitsLocked selects e as the winner of every registration naming
// it that has none yet, in the confirmation's own critical section, and
// removes those registrations from every task list exactly once.
func (ts *taskService) wakeWaitsLocked(e *taskEntry) {
	regs := append([]*waitReg(nil), e.waitRegs...)
	for _, r := range regs {
		if r.winner == "" && !r.done {
			r.winner = e.rec.TaskID
			ts.dropWaitLocked(r)
			close(r.ch)
		}
	}
}

// registerWaitLocked registers one bounded waiter on every (known) id: it
// references only IDs and its private channel, never task output.
func (ts *taskService) registerWaitLocked(ids []string) *waitReg {
	r := &waitReg{ids: ids, ch: make(chan struct{})}
	ts.waits[r] = true
	for _, id := range ids {
		e := ts.tasks[id]
		e.waitRegs = append(e.waitRegs, r)
	}
	return r
}

// dropWaitLocked removes r from the registry and every task it names,
// exactly once.
func (ts *taskService) dropWaitLocked(r *waitReg) {
	if r.done {
		return
	}
	r.done = true
	delete(ts.waits, r)
	for _, id := range r.ids {
		e := ts.tasks[id]
		if e == nil {
			continue
		}
		for i, x := range e.waitRegs {
			if x == r {
				e.waitRegs = append(e.waitRegs[:i], e.waitRegs[i+1:]...)
				break
			}
		}
	}
}

// waitCapacityErr refuses a registration beyond maxWaitCalls.
func waitCapacityErr() error {
	return contract.TaskError(contract.CodeUnavailable, "", contract.ReasonWaitCapacity, "the plane already serves %d waits; nothing was changed: retry, or wait with --wait 0 (an immediate snapshot)", maxWaitCalls)
}

// waitReservation is one registration reserved by a dispatch with a
// positive wait before its admission: released (or consumed by the
// registration) exactly once.
type waitReservation struct {
	ts   *taskService
	done bool
}

// reserveWait reserves one registration.
func (ts *taskService) reserveWait() (*waitReservation, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.waits)+ts.waitReserve >= maxWaitCalls {
		return nil, waitCapacityErr()
	}
	ts.waitReserve++
	return &waitReservation{ts: ts}, nil
}

// release returns an unconsumed reservation (idempotent).
func (r *waitReservation) release() {
	if r == nil {
		return
	}
	r.ts.mu.Lock()
	r.releaseLocked()
	r.ts.mu.Unlock()
}

// releaseLocked is release with the task lock held.
func (r *waitReservation) releaseLocked() {
	if r != nil && !r.done {
		r.done = true
		r.ts.waitReserve--
	}
}

// effectiveWait is min(requested, the plane's cap).
func (ts *taskService) effectiveWait(requested time.Duration) time.Duration {
	return min(requested, ts.maxWait)
}

// ceilMS rounds d up to whole milliseconds.
func ceilMS(d time.Duration) int { return int((d + time.Millisecond - 1) / time.Millisecond) }

// confirmedTerminalLocked returns the first id (caller order) whose task is
// durably terminal.
func (ts *taskService) confirmedTerminalLocked(ids []string) string {
	for _, id := range ids {
		if e := ts.tasks[id]; e != nil && e.terminal() && e.released {
			return id
		}
	}
	return ""
}

// rowLocked is id's compact still_running row.
func (ts *taskService) rowLocked(e *taskEntry, now time.Time) contract.WaitRow {
	v := ts.viewLocked(e, 0, ts.roles.load().visible, now)
	lg, retained := ts.logOfLocked(e)
	const window = 4 << 10
	var tail []byte
	switch {
	case e.ring != nil && (e.cand == nil || e.cand.kind == candRefusal):
		tail = e.ring.tail(window)
	case lg.Data != nil:
		tail = lg.Data[max(0, len(lg.Data)-window):]
	}
	line, cut := contract.LastLogLine(tail)
	truncated := cut || v.Log.Truncated || lg.Incomplete || lg.CounterOverflow || (tail == nil && retained > 0)
	return contract.WaitRow{TaskID: e.rec.TaskID, State: e.rec.State, ElapsedMS: min(v.ElapsedMS, contract.MaxSafeInteger), LastLogLine: line,
		LogTruncated: truncated, DurabilityConfirmed: e.confirmed}
}

// wait answers a bounded any-of wait on ids (validated, unique): every id
// must exist (an unknown one rejects the whole call before registering).
// A durably terminal id wins at once (first in caller order); an
// effective wait of zero is an immediate snapshot; otherwise one
// registration (reserved when reserved is true) waits until a terminal
// commit wakes it, the deadline, the caller's disconnect or shutdown. At
// the deadline committed state is rechecked before the compact snapshot.
func (ts *taskService) wait(ctx context.Context, ids []string, requested time.Duration, reserved *waitReservation) (contract.WaitResponse, error) {
	eff := ts.effectiveWait(requested)
	// The absolute monotonic deadline is fixed before the registration is
	// published: a clock moving after it cannot shift the deadline.
	deadline := ts.clock.Now().Add(eff)
	resp := contract.WaitResponse{Version: contract.ProtocolVersion, EffectiveWaitMS: ceilMS(eff)}
	defer reserved.release()
	key := strings.Join(ids, ",")
	if eff <= 0 {
		// The zero wait decides in its first critical section.
		ts.at("wait-decide", key, ctx)
	}
	ts.mu.Lock()
	for _, id := range ids {
		if ts.tasks[id] == nil {
			ts.mu.Unlock()
			return contract.WaitResponse{}, taskNotFound(id)
		}
	}
	winner := ts.confirmedTerminalLocked(ids)
	var reg *waitReg
	switch {
	case winner != "":
	case eff > 0:
		if reserved == nil && len(ts.waits)+ts.waitReserve >= maxWaitCalls {
			ts.mu.Unlock()
			return contract.WaitResponse{}, waitCapacityErr()
		}
		reg = ts.registerWaitLocked(ids)
		reserved.releaseLocked() // the registration now holds the reserved capacity
		ts.event("wait-registered " + key)
	default:
		// An immediate snapshot: the rows are built in the same critical
		// section that found no winner.
		ts.stillRunningLocked(&resp, ids)
		ts.mu.Unlock()
		return resp, nil
	}
	ts.mu.Unlock()
	if reg != nil {
		timer, stop := ts.clock.NewTimerAt(deadline)
		select {
		case <-reg.ch:
		case <-timer:
		case <-ctx.Done():
		case <-ts.stop:
		}
		stop()
		ts.at("wait-decide", key, ctx)
		// One critical section decides the answer: the registration's winner,
		// else a recheck of committed state (a published winner is never
		// hidden by which channel was ready), else the compact rows of the
		// same observation (never a durably terminal row that did not win).
		ts.mu.Lock()
		winner = reg.winner
		ts.dropWaitLocked(reg)
		if winner == "" {
			winner = ts.confirmedTerminalLocked(ids)
		}
		if winner == "" {
			if err := ctx.Err(); err != nil {
				ts.mu.Unlock()
				return contract.WaitResponse{}, err
			}
			select {
			case <-ts.stop:
				ts.mu.Unlock()
				return contract.WaitResponse{}, contract.New(contract.CodeUnavailable, "the plane is shutting down; retry the wait")
			default:
			}
			ts.stillRunningLocked(&resp, ids)
			ts.mu.Unlock()
			return resp, nil
		}
		ts.mu.Unlock()
	}
	// A durably terminal task stays terminal: its view is read afterwards.
	v, err := ts.show(winner, contract.DefaultTailLines)
	if err != nil {
		return contract.WaitResponse{}, err
	}
	resp.Status, resp.Winner, resp.Task = contract.WaitTerminal, winner, &v
	return resp, nil
}

// stillRunningLocked fills resp with the compact rows of ids (caller
// order) at one observation.
func (ts *taskService) stillRunningLocked(resp *contract.WaitResponse, ids []string) {
	now := ts.clock.Now()
	resp.Status = contract.WaitStillRunning
	for _, id := range ids {
		resp.Tasks = append(resp.Tasks, ts.rowLocked(ts.tasks[id], now))
	}
}
