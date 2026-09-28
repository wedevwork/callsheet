package plane

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b plane control fixtures and TestControlCancel (FP-1).
// Every product timer runs on the fake node clock; tests synchronize on
// the named events stop-intent-queued, stop-intent-committed,
// terminal-committed and the stream's cancel-sent/cancel-acked, never on
// sleeps, and advance the clock only after the timer they test is armed.

// cancelAsync cancels task id through the verified client.
func (tp *taskPlane) cancelAsync(ctx context.Context, id string) *call[contract.CancelResponse] {
	return async(func() (contract.CancelResponse, error) { return tp.cl.CancelTask(ctx, id) })
}

// cancel cancels task id and requires a response.
func (tp *taskPlane) cancel(t *testing.T, id string) contract.CancelResponse {
	t.Helper()
	r, err := tp.cl.CancelTask(bg, id)
	if err != nil {
		t.Fatalf("cancel %s: %v", id, err)
	}
	return r
}

// stopID is task id's durable stop intent ID (its document).
func (tp *taskPlane) stopID(t *testing.T, id string) string {
	t.Helper()
	rec := tp.taskFile(t, id)
	if rec.StopIntent == nil {
		t.Fatalf("task %s has no durable stop intent", id)
	}
	return rec.StopIntent.ID
}

// expectCancel reads task_cancel pid for st and waits until the plane's
// write returned.
func (w *worker) expectCancel(pid string, st contract.TaskStartBody) contract.TaskCancelBody {
	w.t.Helper()
	f := w.expect(contract.FrameTaskCancel, pid)
	b, err := contract.DecodeTaskCancel(f.Body)
	if err != nil || b.TaskID != st.TaskID || b.Execution != st.Execution || b.Kind != contract.StopKindCancelled {
		w.t.Fatalf("task_cancel %s: %+v %v", f.Body, b, err)
	}
	w.ev.awaitFrom(w.t, w.dialed, "cancel-sent "+w.node+" "+pid+" "+st.TaskID)
	return b
}

// ackCancel acknowledges task_cancel pid and waits for the plane's
// receipt.
func (w *worker) ackCancel(pid string, b contract.TaskCancelBody) {
	w.t.Helper()
	w.send(contract.ProtocolVersion, contract.FrameTaskCancelAck, pid, contract.TaskCancelAckBody{TaskID: b.TaskID, Execution: b.Execution, StopID: b.StopID, Received: true})
	w.ev.awaitFrom(w.t, w.dialed, "cancel-acked "+w.node+" "+pid+" "+b.TaskID)
}

// ctlResult is a sealed control or natural result of st.
func ctlResult(st contract.TaskStartBody, outcome string, stopID *string, exit *int, signal *string, out int) contract.TaskResultBody {
	return contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: outcome, StopID: stopID, ExitCode: exit, Signal: signal,
		OutputBytes: out}.Sealed()
}

func ip(n int) *int               { return &n }
func sp(s string) *string         { return &s }
func tptr(t time.Time) *time.Time { return &t }

// reconcileEntries reports entries as one final inventory page i1 and
// returns the plane's reconcile entries by task (every page acknowledged).
func (p *peer) reconcileEntries(entries ...contract.TaskInventoryEntry) map[string]contract.TaskReconcileEntry {
	p.t.Helper()
	p.send(contract.ProtocolVersion, contract.FrameTaskInventory, "i1", contract.TaskInventoryBody{RunID: peerRunID, Final: true, Entries: entries})
	p.expect(contract.FrameTaskInventoryAck, "i1")
	out := map[string]contract.TaskReconcileEntry{}
	for n := 1; ; n++ {
		rid := "r" + itoa(n)
		f := p.expect(contract.FrameTaskReconcile, rid)
		b, err := contract.DecodeTaskReconcile(f.Body)
		if err != nil {
			p.t.Fatalf("reconcile %s: %v", f.Body, err)
		}
		for _, e := range b.Entries {
			out[e.TaskID] = e
		}
		p.send(contract.ProtocolVersion, contract.FrameTaskReconcileAck, rid, contract.TaskReconcileAckBody{Received: true})
		if b.Final {
			return out
		}
	}
}

// workerEntries connects id's worker reporting entries and returns it
// with the plane's reconcile entries.
func (tp *taskPlane) workerEntries(t *testing.T, id string, entries ...contract.TaskInventoryEntry) (*worker, map[string]contract.TaskReconcileEntry) {
	t.Helper()
	dialed := tp.log.mark()
	p := tp.dial(t)
	p.ready = map[string]bool{}
	p.hello(id)
	p.expect(contract.FrameHelloOK, "h1")
	got := p.reconcileEntries(entries...)
	tp.log.awaitFrom(t, dialed, "reconciled "+id)
	b := p.ackReplace("p1")
	for _, r := range b.Roles {
		p.ready[r.ID] = true
	}
	w := &worker{peer: p, b: 1, ev: tp.log, node: id, marks: map[string]int{}, dialed: dialed}
	tp.workers = append(tp.workers, w)
	w.beat()
	return w, got
}

// commitResult sends b and waits for its durable terminal commit.
func (tp *taskPlane) commitResult(t *testing.T, w *worker, b contract.TaskResultBody) {
	t.Helper()
	w.resultAck(w.sendResult(b), b.TaskID)
	tp.log.awaitOnce(t, "terminal-committed "+b.TaskID)
}

// TestControlCancel is UT FP-1 on the plane, delegated from tests/function
// (TestControlCancellation): an unsent start (this Run's proof, never a
// loaded one) is withdrawn and cancelled before start with its transport
// token released once; a loaded pending task is potentially live (stop
// control on its reconciliation, or lost by complete-inventory absence
// with the intent retained); the single writer's durable order decides
// between natural results, lost and refusal decisions and the stop intent;
// intent writes retry before and after rename; offline cancels resolve on
// reconnect or lost at lease expiry; duplicates join one intent and one
// coalesced control. Do not rename or skip it.
func TestControlCancel(t *testing.T) {
	t.Parallel()
	t.Run("unsent", func(t *testing.T) {
		t.Parallel()
		// A start queued behind another in-flight request is withdrawn
		// atomically: never written, its token released once, the task
		// cancelled before start (two publications: intent, terminal).
		tp, w := ctlPlane(t, 2)
		first := tp.admit(t, taskReq(contract.TargetID, "a", "first"))
		st1 := w.start("p2")
		if st1.TaskID != first.TaskID {
			t.Fatal("wrong start")
		}
		v := tp.admit(t, taskReq(contract.TargetID, "a", "queued"))
		tp.log.awaitOnce(t, "start-queued "+idA+" "+v.TaskID)
		if n := tp.startTokens(t, idA); n != 2 {
			t.Fatalf("tokens before the cancel %d", n)
		}
		r := tp.cancel(t, v.TaskID)
		if !r.Accepted || !r.Task.StopRequested {
			t.Fatalf("cancel %+v", r)
		}
		tp.log.awaitOnce(t, "stop-intent-committed "+v.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+v.TaskID)
		got := tp.show(t, v.TaskID)
		if got.State != contract.TaskCancelled || got.StartedAt != nil || got.Reason == nil || got.Reason.Code != contract.ReasonCancelledBeforeStart ||
			!got.StopRequested || got.Result == nil || got.Result.ExitCode != nil || got.Result.Signal != nil {
			t.Fatalf("cancelled before start %+v", got)
		}
		if n := tp.startTokens(t, idA); n != 1 {
			t.Fatalf("tokens after the cancel %d (the withdrawn start's is released once)", n)
		}
		if rec := tp.taskFile(t, v.TaskID); rec.StopIntent == nil || rec.State != contract.TaskCancelled || rec.TimeoutPolicy != contract.TimeoutPolicyEnforced {
			t.Fatalf("durable %+v", rec)
		}
		// The in-flight start is answered; the withdrawn one is never sent:
		// the next plane request is a control, not a start.
		w.answer("p2", st1.TaskID, nil)
		tp.log.awaitOnce(t, "published running "+st1.TaskID)
		tp.cancel(t, st1.TaskID)
		b := w.expectCancel("p3", st1)
		if tp.log.seenPrefix("start-writing "+idA+" p3") || tp.log.seen("start-sent "+idA+" p3 "+v.TaskID) {
			t.Fatal("the withdrawn start was written")
		}
		w.ackCancel("p3", b)
		tp.commitResult(t, w, ctlResult(st1, contract.OutcomeCancelled, &b.StopID, nil, sp("SIGTERM"), 0))
		if tp.roleInflight(t, "a") != 0 || tp.startTokens(t, idA) != 0 {
			t.Fatalf("inflight %d tokens %d", tp.roleInflight(t, "a"), tp.startTokens(t, idA))
		}
		tp.checkCounts(t)
		// A publication still unconfirmed (never submitted) is cancelled
		// before start once its sync is retried; nothing is ever sent.
		tp.inj.set("dirsync", tasksName)
		u := tp.admit(t, taskReq(contract.TargetID, "a", "unconfirmed"))
		res := tp.cancelAsync(bg, u.TaskID)
		tp.log.awaitOnce(t, "stop-intent-queued "+u.TaskID)
		tp.inj.set("", "")
		tp.clk.Advance(storageRetry)
		cr, err := res.wait(t)
		if err != nil || !cr.Accepted {
			t.Fatalf("cancel of an unconfirmed pending %+v %v", cr, err)
		}
		tp.log.awaitOnce(t, "terminal-committed "+u.TaskID)
		if got := tp.show(t, u.TaskID); got.State != contract.TaskCancelled || got.Reason.Code != contract.ReasonCancelledBeforeStart {
			t.Fatalf("unconfirmed cancelled %+v", got)
		}
		if tp.log.seen("start-queued " + idA + " " + u.TaskID) {
			t.Fatal("a cancelled unconfirmed start was queued")
		}
		tp.checkCounts(t)
	})
	t.Run("loaded-pending", func(t *testing.T) {
		t.Parallel()
		for _, absent := range []bool{true, false} {
			name := map[bool]string{true: "absent", false: "stop-control"}[absent]
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				// A start written before a restart is loaded pending: never
				// proven unsent, so the cancel persists its intent and keeps the
				// reservation until reconciliation resolves it.
				tp, w := ctlPlane(t, 1)
				v := tp.admit(t, taskReq(contract.TargetID, "a", "loaded"))
				st := w.start("p2")
				tp.restart(t)
				r := tp.cancel(t, v.TaskID)
				if !r.Accepted || r.Task.State != contract.TaskPending || !r.Task.StopRequested || r.Task.Result != nil {
					t.Fatalf("loaded cancel %+v", r)
				}
				if tp.roleInflight(t, "a") != 1 || tp.log.seen("cancelled-before-start "+v.TaskID) {
					t.Fatal("a loaded pending task was treated as unsent")
				}
				id := tp.stopID(t, v.TaskID)
				if absent {
					// Complete-inventory absence: lost (execution_missing) with the
					// intent retained, never cancelled.
					tp.workerEntries(t, idA)
					tp.log.awaitOnce(t, "terminal-committed "+v.TaskID)
					got := tp.show(t, v.TaskID)
					if got.State != contract.TaskLost || got.Reason.Code != contract.ReasonExecutionMissing || !got.StopRequested {
						t.Fatalf("absent %+v", got)
					}
					return
				}
				w, ents := tp.workerEntries(t, idA, entry(st, contract.PhasePreparing, nil, nil))
				e := ents[st.TaskID]
				if e.Action != contract.ActionStopControl || e.Stop == nil || e.Stop.ID != id || e.Stop.Kind != contract.StopKindCancelled {
					t.Fatalf("disposition %+v", e)
				}
				tp.commitResult(t, w, ctlResult(st, contract.OutcomeCancelled, &id, nil, nil, 0))
				got := tp.show(t, v.TaskID)
				if got.State != contract.TaskCancelled || got.StartedAt != nil || got.Reason.Code != contract.ReasonCancelledBeforeStart {
					t.Fatalf("stop control %+v", got)
				}
				tp.checkCounts(t)
			})
		}
	})
	t.Run("durable-order", func(t *testing.T) {
		t.Parallel()
		t.Run("natural-first", func(t *testing.T) {
			t.Parallel()
			// A durable natural result stands: 200, not accepted, no control.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "done", "p2")
			tp.finish(t, w, st, 0, 0)
			r := tp.cancel(t, st.TaskID)
			if r.Accepted || r.Task.State != contract.TaskSucceeded || r.Task.StopRequested {
				t.Fatalf("after success %+v", r)
			}
			if rec := tp.taskFile(t, st.TaskID); rec.StopIntent != nil {
				t.Fatal("a terminal task gained an intent")
			}
		})
		t.Run("latched-natural", func(t *testing.T) {
			t.Parallel()
			// A latched natural candidate owns its commit: the cancel waits
			// for it and then answers not accepted; no intent is selected.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "latched", "p2")
			hold := tp.th.arm("terminal-commit-queued", st.TaskID)
			w.resultAck(w.sendResult(result(st, 3, 0, nil)), st.TaskID)
			call := paused(t, hold, "terminal-commit-queued")
			entered := tp.th.arm("cancel-received", st.TaskID)
			res := tp.cancelAsync(bg, st.TaskID)
			close(paused(t, entered, "cancel-received").release)
			close(call.release)
			r, err := res.wait(t)
			if err != nil || r.Accepted || r.Task.State != contract.TaskFailed {
				t.Fatalf("latched natural %+v %v", r, err)
			}
			if tp.log.seen("stop-intent-queued " + st.TaskID) {
				t.Fatal("an intent displaced a latched natural result")
			}
		})
		t.Run("intent-first", func(t *testing.T) {
			t.Parallel()
			// A durable intent reserves cancelled: the worker's later natural
			// result (its adapter's exit during cleanup, null stop_id) supplies
			// the terminal fields and partial output; the state is cancelled.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "intent", "p2")
			w.log(st, 0, []byte("partial\n"))
			r := tp.cancel(t, st.TaskID)
			if !r.Accepted || r.Task.State != contract.TaskRunning || !r.Task.StopRequested {
				t.Fatalf("intent %+v", r)
			}
			b := w.expectCancel("p3", st)
			w.ackCancel("p3", b)
			res := ctlResult(st, contract.OutcomeNatural, nil, ip(0), nil, 8)
			tp.commitResult(t, w, res)
			got := tp.show(t, st.TaskID)
			if got.State != contract.TaskCancelled || *got.Result.ExitCode != 0 || got.Reason != nil || got.LogTail != "partial\n" || !got.StopRequested {
				t.Fatalf("intent first %+v", got)
			}
			if rec := tp.taskFile(t, st.TaskID); rec.ResultDigest == nil || *rec.ResultDigest != res.Digest {
				t.Fatal("the result digest was not kept for committed acknowledgements")
			}
			if !w.resultAck(w.sendResult(res), st.TaskID) {
				t.Fatal("the durable result was not acknowledged committed")
			}
			tp.checkCounts(t)
		})
		t.Run("intent-before-result", func(t *testing.T) {
			t.Parallel()
			// The intent is selected but not yet written when a result arrives:
			// the result waits behind it (no natural terminal is selected), and
			// the two publications land in order.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "ordered", "p2")
			hold := tp.th.arm("stop-intent-queued", st.TaskID)
			res := tp.cancelAsync(bg, st.TaskID)
			call := paused(t, hold, "stop-intent-queued")
			w.resultAck(w.sendResult(result(st, 0, 0, nil)), st.TaskID)
			tp.log.awaitOnce(t, "result-captured "+st.TaskID)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || !v.CompletionPending || v.StopRequested {
				t.Fatalf("held %+v", v)
			}
			close(call.release)
			if r, err := res.wait(t); err != nil || !r.Accepted {
				t.Fatalf("cancel %+v %v", r, err)
			}
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskCancelled || *v.Result.ExitCode != 0 {
				t.Fatalf("ordered %+v", v)
			}
			all := strings.Join(tp.log.all(), "\n")
			if strings.Index(all, "published stop-intent "+st.TaskID) > strings.Index(all, "published terminal "+st.TaskID) {
				t.Fatal("the terminal record preceded the intent")
			}
		})
		t.Run("refusal-after-intent", func(t *testing.T) {
			t.Parallel()
			// A written start is potentially live; a definite refusal after
			// the winning intent proves no cleanup remains: cancelled before
			// start, not rejected.
			tp, w := ctlPlane(t, 1)
			v := tp.admit(t, taskReq(contract.TargetID, "a", "refused"))
			st := w.start("p2")
			if r := tp.cancel(t, v.TaskID); !r.Accepted || r.Task.State != contract.TaskPending {
				t.Fatalf("cancel %+v", r)
			}
			w.answer("p2", st.TaskID, contract.TaskError(contract.CodeUnavailable, "", contract.ReasonLocalFull, "full"))
			tp.log.awaitOnce(t, "terminal-committed "+v.TaskID)
			if got := tp.show(t, v.TaskID); got.State != contract.TaskCancelled || got.Reason.Code != contract.ReasonCancelledBeforeStart || len(got.Candidates) != 0 {
				t.Fatalf("refusal after the intent %+v", got)
			}
		})
		t.Run("lost-latched", func(t *testing.T) {
			t.Parallel()
			// A lost decision latched first is never displaced.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "lost", "p2")
			hold := tp.th.arm("terminal-commit-queued", st.TaskID)
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			tp.clk.Advance(leaseDuration)
			call := paused(t, hold, "terminal-commit-queued")
			res := tp.cancelAsync(bg, st.TaskID)
			close(call.release)
			r, err := res.wait(t)
			if err != nil || r.Accepted || r.Task.State != contract.TaskLost || r.Task.StopRequested {
				t.Fatalf("lost first %+v %v", r, err)
			}
		})
		t.Run("stop-id-conflict", func(t *testing.T) {
			t.Parallel()
			// A nonnull stop_id naming no stored intent is a protocol error:
			// the attachment ends and nothing is captured.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "foreign", "p2")
			rid := w.sendResult(ctlResult(st, contract.OutcomeCancelled, sp(strings.Repeat("e", 32)), nil, nil, 0))
			w.expectError(rid, contract.CodeInvalidArgument)
			w.detached()
			if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || v.CompletionPending {
				t.Fatalf("after a foreign stop_id %+v", v)
			}
		})
	})
	t.Run("storage-retry", func(t *testing.T) {
		t.Parallel()
		// The intent's write fails before its rename: the old record stays
		// authoritative, the failure is retried every second, the response
		// ends unavailable at its 6 s budget (the intent can still commit),
		// and the fault's clearing commits the same frozen intent; a retry
		// joins it.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "retry", "p2")
		tp.inj.set("rename", taskRel(st.TaskID))
		res := tp.cancelAsync(bg, st.TaskID)
		tp.log.awaitOnce(t, "publish-failed stop-intent "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.StopRequested || v.PersistenceReason == nil {
			t.Fatalf("failed intent %+v", v)
		}
		from := tp.log.mark()
		tp.clk.Advance(storageRetry)
		tp.log.awaitFrom(t, from, "publish-failed stop-intent "+st.TaskID)
		for tp.clk.Now().Add(5 * time.Second).Before(t0.Add(admissionTimeout + storageRetry)) {
			tp.clk.Advance(4 * time.Second)
			w.beat()
		}
		tp.clk.Advance(t0.Add(admissionTimeout + storageRetry).Sub(tp.clk.Now()))
		if _, err := res.wait(t); contract.CodeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("budget %v", err)
		}
		w.beat()
		// The budget's advances may have left an attempt in flight: the
		// clock moves only once it failed and armed its retry.
		tp.awaitWriterParked(t, st.TaskID)
		// Regression (CI run 36454235765): the fault was cleared and the
		// clock moved while a failing attempt had not stored its retry yet;
		// it armed the retry an interval after that advance and the intent
		// never committed. Here the next attempt is held after its failure
		// and before its outcome is applied, while the clock moves (that
		// order).
		held, stall := make(chan struct{}), make(chan struct{})
		var unstall sync.Once
		t.Cleanup(func() { unstall.Do(func() { close(stall) }) })
		tp.inj.onNextFailure(func() {
			close(held)
			<-stall
		})
		tp.clk.Advance(storageRetry)
		select {
		case <-held:
		case <-time.After(testWait):
			t.Fatal("the due retry never reached its rename")
		}
		if s := tp.writerState(t, st.TaskID); s.parked() {
			t.Fatalf("an attempt in flight counts as parked: %+v", s)
		}
		tp.inj.set("", "")
		tp.clk.Advance(storageRetry)
		w.beat()
		unstall.Do(func() { close(stall) })
		// That advance fired nothing: the retry is a full interval ahead.
		if s := tp.awaitWriterParked(t, st.TaskID); s.retryAt.Sub(s.now) != storageRetry || tp.log.seen("stop-intent-committed "+st.TaskID) {
			t.Fatalf("after an advance during the failing attempt: %+v", s)
		}
		// Parked on the armed retry, with the fault cleared: one interval
		// commits the same frozen intent.
		tp.clk.Advance(storageRetry)
		tp.log.awaitOnce(t, "stop-intent-committed "+st.TaskID)
		id := tp.stopID(t, st.TaskID)
		if r := tp.cancel(t, st.TaskID); !r.Accepted || tp.stopID(t, st.TaskID) != id {
			t.Fatalf("retry %+v", r)
		}
		b := w.expectCancel("p3", st)
		if b.StopID != id {
			t.Fatal("the control carries another stop ID")
		}
		// After the rename, a failed directory sync leaves the intent
		// visible but unconfirmed: not accepted until a resync confirms it.
		tp2, w2 := ctlPlane(t, 1)
		st2 := tp2.run(t, w2, "a", "visible", "p2")
		tp2.inj.set("dirsync", tasksName)
		res2 := tp2.cancelAsync(bg, st2.TaskID)
		tp2.log.awaitOnce(t, "published stop-intent "+st2.TaskID)
		if v := tp2.show(t, st2.TaskID); !v.StopRequested || v.DurabilityConfirmed {
			t.Fatalf("visible intent %+v", v)
		}
		if !res2.pending() {
			t.Fatal("an unconfirmed intent was accepted")
		}
		tp2.inj.set("", "")
		tp2.clk.Advance(storageRetry)
		if r, err := res2.wait(t); err != nil || !r.Accepted {
			t.Fatalf("after the resync %+v %v", r, err)
		}
		w2.expectCancel("p3", st2)
	})
	t.Run("offline", func(t *testing.T) {
		t.Parallel()
		t.Run("reconnect", func(t *testing.T) {
			t.Parallel()
			// Offline before the lease expires: accepted, then delivered as
			// the reconnecting attachment's stop_control.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "offline", "p2")
			started := tp.clk.Now()
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			r := tp.cancel(t, st.TaskID)
			if !r.Accepted || r.Task.State != contract.TaskRunning || !r.Task.Reconciling {
				t.Fatalf("offline cancel %+v", r)
			}
			w, ents := tp.workerEntries(t, idA, entry(st, contract.PhaseRunning, &started, nil))
			id := tp.stopID(t, st.TaskID)
			if e := ents[st.TaskID]; e.Action != contract.ActionStopControl || e.Stop == nil || e.Stop.ID != id {
				t.Fatalf("disposition %+v", e)
			}
			tp.commitResult(t, w, ctlResult(st, contract.OutcomeCancelled, &id, nil, sp("SIGKILL"), 0))
			if v := tp.show(t, st.TaskID); v.State != contract.TaskCancelled || *v.Result.Signal != "SIGKILL" || v.StartedAt == nil {
				t.Fatalf("reconnected %+v", v)
			}
		})
		t.Run("lease", func(t *testing.T) {
			t.Parallel()
			// Offline until the lease expires: lost with the intent retained
			// (stop_requested), never a claim of a cancelled remote group.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "partition", "p2")
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			if r := tp.cancel(t, st.TaskID); !r.Accepted {
				t.Fatalf("cancel %+v", r)
			}
			tp.clk.Advance(leaseDuration)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			v := tp.show(t, st.TaskID)
			if v.State != contract.TaskLost || v.Reason.Code != contract.ReasonLeaseExpired || !v.StopRequested || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("lease %+v", v)
			}
			// An already terminal lost task is unchanged by a cancel.
			if r := tp.cancel(t, st.TaskID); r.Accepted || r.Task.State != contract.TaskLost {
				t.Fatalf("after loss %+v", r)
			}
		})
	})
	t.Run("duplicate", func(t *testing.T) {
		t.Parallel()
		// Repeated cancels join one frozen intent and one coalesced control:
		// no second stop ID, no reset; redelivery is at most once per second
		// after a receipt and stops with the task's decision.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "dup", "p2")
		r1 := tp.cancel(t, st.TaskID)
		id := tp.stopID(t, st.TaskID)
		b := w.expectCancel("p3", st)
		r2 := tp.cancel(t, st.TaskID)
		if !r1.Accepted || !r2.Accepted || tp.stopID(t, st.TaskID) != id || b.StopID != id {
			t.Fatalf("duplicates %+v %+v", r1, r2)
		}
		if n := strings.Count(strings.Join(tp.log.all(), "\n"), "stop-intent-queued "+st.TaskID); n != 1 {
			t.Fatalf("%d intents selected", n)
		}
		w.ackCancel("p3", b)
		tp.clk.Advance(storageRetry)
		w.beat()
		b2 := w.expectCancel("p4", st)
		if b2 != b {
			t.Fatalf("redelivery %+v", b2)
		}
		w.ackCancel("p4", b2)
		tp.commitResult(t, w, ctlResult(st, contract.OutcomeCancelled, &id, nil, sp("SIGTERM"), 0))
		tp.clk.Advance(storageRetry)
		w.beat()
		if tp.log.seenPrefix("cancel-writing " + idA + " p5") {
			t.Fatal("a control was redelivered after the decision")
		}
		// Errors: unknown task, bad ID, a body other than {} and a query.
		wantCode(t, func() error { _, err := tp.cl.CancelTask(bg, "t_"+strings.Repeat("9", 32)); return err }(), contract.CodeNotFound)
		wantCode(t, func() error { _, err := tp.cl.CancelTask(bg, "bad"); return err }(), contract.CodeInvalidArgument)
		for _, c := range []struct{ path, body string }{{"/cancel", `{"x":1}`}, {"/cancel", ``}, {"/cancel?x=1", `{}`}} {
			req, _ := http.NewRequest(http.MethodPost, tp.url+contract.PathTasks+"/"+st.TaskID+c.path, bytes.NewReader([]byte(c.body)))
			req.Header.Set(contract.ProtocolHeader, "5")
			req.Header.Set("Content-Type", "application/json")
			resp, err := client(t, readCA(t, tp.root), "127.0.0.1", 0).Do(req)
			if err != nil || resp.StatusCode != 400 {
				t.Fatalf("cancel %s %q = %v %v", c.path, c.body, resp, err)
			}
			resp.Body.Close()
		}
		// A caller canceled before selection selects nothing; one that
		// leaves after selection only detaches: the intent commits anyway.
		tp2, w2 := ctlPlane(t, 1)
		st2 := tp2.run(t, w2, "a", "detached caller", "p2")
		entered := tp2.th.arm("cancel-received", st2.TaskID)
		ctx0, cancel0 := context.WithCancel(bg)
		res0 := tp2.cancelAsync(ctx0, st2.TaskID)
		c0 := paused(t, entered, "cancel-received")
		cancel0()
		// The plane observes the caller's departure before it selects.
		select {
		case <-c0.ctx.Done():
		case <-time.After(testWait):
			t.Fatal("the plane never saw the caller leave")
		}
		close(c0.release)
		if _, err := res0.wait(t); err == nil {
			t.Fatal("a canceled caller got an answer")
		}
		if tp2.log.seen("stop-intent-queued " + st2.TaskID) {
			t.Fatal("a caller canceled before selection selected an intent")
		}
		hold := tp2.th.arm("stop-intent-queued", st2.TaskID)
		ctx, cancel := context.WithCancel(bg)
		res := tp2.cancelAsync(ctx, st2.TaskID)
		call := paused(t, hold, "stop-intent-queued")
		cancel()
		if _, err := res.wait(t); err == nil {
			t.Fatal("a canceled caller got an answer")
		}
		close(call.release)
		tp2.log.awaitOnce(t, "stop-intent-committed "+st2.TaskID)
		if v := tp2.show(t, st2.TaskID); !v.StopRequested {
			t.Fatalf("detached caller %+v", v)
		}
	})
}
