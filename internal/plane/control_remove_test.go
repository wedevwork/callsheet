package plane

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b TestControlRemove (FP-4): role rm --force through the
// verified client against the served plane on the fake node clock.

// forceAsync runs a forced removal of id (with an operation token).
func (tp *taskPlane) forceAsync(ctx context.Context, id, op string) *call[contract.RoleRemoveResult] {
	return async(func() (contract.RoleRemoveResult, error) { return tp.cl.RemoveRole(ctx, id, true, op) })
}

// fenceOf reads role id's removal fence through role show.
func (tp *taskPlane) fenceOf(t *testing.T, id string) *contract.RoleRemoval {
	t.Helper()
	v := tp.view(t, id)
	if v.Removing != (v.Removal != nil) || (v.Removing && v.CanAccept) {
		t.Fatalf("inconsistent removal view %+v", v)
	}
	return v.Removal
}

// awaitRetryArmed waits until the coordinator armed its retry timer
// after the (first) event failure: a pass arms it only after returning,
// so the clock must not move before.
func (tp *taskPlane) awaitRetryArmed(t *testing.T, failure string) {
	t.Helper()
	tp.log.awaitOnce(t, failure)
	at := func() bool {
		all := tp.log.all()
		i := slices.Index(all, failure)
		return i >= 0 && slices.Contains(all[i+1:], "removal-retry-armed")
	}
	deadline := time.After(testWait)
	for !at() {
		select {
		case <-tp.log.sig:
		case <-deadline:
			if at() {
				return
			}
			t.Fatalf("no retry armed after %q in %v", failure, tp.log.all())
		}
	}
}

// awaitPending advances the clock through exactly the 6 s mutation budget
// (armed at the request's entry, before its fence event; the worker keeps
// its lease), after which res must answer 202, and returns it. The clock
// never moves by whether the answer already arrived.
func (tp *taskPlane) awaitPending(t *testing.T, w *worker, res *call[contract.RoleRemoveResult]) *contract.RoleRemovePendingResponse {
	t.Helper()
	for i := 0; i < 2; i++ {
		tp.clk.Advance(mutationTimeout / 2)
		if w != nil {
			w.beat()
		}
	}
	r, err := res.wait(t)
	if err != nil || r.Pending == nil || r.Completed != nil {
		t.Fatalf("pending removal %+v %v", r, err)
	}
	return r.Pending
}

// until reads plane requests until one of type typ, acknowledging role
// snapshots and passing validations on the way, and returns it.
func (w *worker) until(typ string) contract.NodeFrame {
	w.t.Helper()
	for {
		f := w.recv()
		switch f.Type {
		case typ:
			return f
		case contract.FrameRolesReplace:
			b, err := contract.DecodeRolesReplace(f.Body, roleLookup)
			if err != nil {
				w.t.Fatal(err)
			}
			w.rev, w.roles = b.Revision, b.Roles
			w.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, f.RequestID, contract.RolesReplaceAckBody{Revision: b.Revision})
		case contract.FrameRoleValidate:
			w.peer.reply(f.RequestID, nil)
		default:
			w.t.Fatalf("got %s %s while waiting for %s", f.Type, f.RequestID, typ)
		}
	}
}

// ackSnapshot reads the next roles_replace (validations answered on the
// way), acknowledges it and waits for the plane's receipt.
func (w *worker) ackSnapshot() {
	w.t.Helper()
	f := w.until(contract.FrameRolesReplace)
	b, err := contract.DecodeRolesReplace(f.Body, roleLookup)
	if err != nil {
		w.t.Fatal(err)
	}
	w.rev, w.roles = b.Revision, b.Roles
	w.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, f.RequestID, contract.RolesReplaceAckBody{Revision: b.Revision})
	w.ev.awaitFrom(w.t, w.dialed, "replace-acked "+w.node+" "+f.RequestID+" "+itoa(b.Revision))
}

// nextCancel reads the next task_cancel (snapshots and validations
// answered on the way) and acknowledges it.
func (w *worker) nextCancel() contract.TaskCancelBody {
	w.t.Helper()
	f := w.until(contract.FrameTaskCancel)
	b, err := contract.DecodeTaskCancel(f.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	w.ev.awaitFrom(w.t, w.dialed, "cancel-sent "+w.node+" "+f.RequestID+" "+b.TaskID)
	w.ackCancel(f.RequestID, b)
	return b
}

// TestControlRemove is UT FP-4 on the plane, delegated from tests/function
// (TestControlForceRemove): the durable instance fence and its admission
// effects, draining held tasks by cancellation (pending, unsent, offline
// and lost), restart resumption and caller departure, storage faults of
// the fence, the cancellations and the deletion, and instance reuse. Do
// not rename or skip it.
func TestControlRemove(t *testing.T) {
	t.Parallel()
	t.Run("fence", func(t *testing.T) {
		t.Parallel()
		// The fence is durable before any cancellation, closes admission
		// (role_removing), refuses set and ordinary rm, survives unrelated
		// mutations, and a mismatched token changes nothing.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1), record(taskCfg("b", "coder", idA, 1), 2)}, idA)
		w := tp.worker(t, idA)
		st := tp.run(t, w, "a", "held", "p2")
		res := tp.forceAsync(bg, "a", "")
		tp.log.awaitOnce(t, "fenced a")
		f := tp.fenceOf(t, "a")
		if f == nil || f.RegistrationOrder != 1 || !contract.ValidOperationID(f.OperationID) {
			t.Fatalf("fence %+v", f)
		}
		if !strings.Contains(string(registryBytes(t, tp.root)), `"operation_id": "`+f.OperationID+`"`) {
			t.Fatal("the fence is not in the registry document")
		}
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "fenced"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:role_removing")
		model := "m2"
		if _, err := tp.cl.SetRole(bg, "a", contract.RolePatch{Model: &model}); contract.CodeOf(err) != contract.CodeConflict || reason(err) != contract.ReasonRoleRemoving {
			t.Fatalf("set on a fenced role %v", err)
		}
		wantReason(t, rmRole(tp.cl, bg, "a", false), contract.CodeConflict, contract.ReasonRoleRemoving)
		other := strings.Repeat("0", 32)
		if _, err := tp.cl.RemoveRole(bg, "a", true, other); reason(err) != contract.ReasonRemovalOperationMismatch {
			t.Fatalf("mismatched token %v", err)
		}
		if _, err := tp.cl.RemoveRole(bg, "b", true, other); reason(err) != contract.ReasonRemovalOperationMismatch {
			t.Fatalf("token on an unfenced instance %v", err)
		}
		// The held task's cancellation is delivered; an unrelated mutation
		// keeps the fence.
		b := w.nextCancel()
		sres := tp.setAsync(bg, "b", contract.RolePatch{Model: &model})
		w.peer.reply(w.until(contract.FrameRoleValidate).RequestID, nil)
		if _, err := sres.wait(t); err != nil {
			t.Fatal(err)
		}
		if g := tp.fenceOf(t, "a"); g == nil || g.OperationID != f.OperationID {
			t.Fatal("an unrelated mutation erased the fence")
		}
		// After the held task's durable resolution the role and its fence
		// are deleted together, within the budget: 200.
		tp.commitResult(t, w, ctlResult(st, contract.OutcomeCancelled, &b.StopID, nil, sp("SIGTERM"), 0))
		r, err := res.wait(t)
		if err != nil || r.Completed == nil || r.Completed.Removed != "a" || r.Pending != nil {
			t.Fatalf("completed %+v %v", r, err)
		}
		if doc := tp.svc(t).roles.load().visible; len(doc.removals) != 0 || doc.fenceOf("a", 1) != nil {
			t.Fatalf("the fence survived %+v", doc.removals)
		}
		if _, err := tp.cl.ShowRole(bg, "a"); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatal("the role survived")
		}
		if v := tp.show(t, st.TaskID); v.State != contract.TaskCancelled {
			t.Fatalf("task %+v", v)
		}
	})
	t.Run("drain", func(t *testing.T) {
		t.Parallel()
		// Every held task is cancelled: running ones by control, a written
		// start refused after its intent (cancelled before start), an
		// offline one lost at lease expiry; beyond the budget the answer is
		// 202 with the operation, a retry with it joins it, and after the
		// deletion that retry is not_found.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 3), 1)}, idA)
		w := tp.worker(t, idA)
		run := tp.run(t, w, "a", "running", "p2")
		off := tp.run(t, w, "a", "running too", "p3")
		first := tp.admit(t, taskReq(contract.TargetID, "a", "sent"))
		sent := w.start("p4")
		if sent.TaskID != first.TaskID {
			t.Fatal("wrong start")
		}
		res := tp.forceAsync(bg, "a", "")
		tp.log.awaitOnce(t, "fenced a")
		for _, id := range []string{run.TaskID, off.TaskID, sent.TaskID} {
			tp.log.awaitOnce(t, "stop-intent-committed "+id)
		}
		p := tp.awaitPending(t, w, res)
		if p.RoleID != "a" || p.RegistrationOrder != 1 || !p.Removing {
			t.Fatalf("pending %+v", p)
		}
		w.answer("p4", sent.TaskID, contract.TaskError(contract.CodeUnavailable, "", contract.ReasonLocalFull, "full"))
		tp.log.awaitOnce(t, "terminal-committed "+sent.TaskID)
		stops := map[string]string{}
		for i := 0; i < 2; i++ {
			b := w.nextCancel()
			stops[b.TaskID] = b.StopID
		}
		id := stops[run.TaskID]
		tp.commitResult(t, w, ctlResult(run, contract.OutcomeCancelled, &id, ip(143), nil, 0))
		// A joined retry answers pending while one task remains; the
		// offline one is lost at its lease expiry, which permits removal.
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		tp.clk.Advance(leaseDuration - time.Second)
		tp.hooks.pause("force-joined")
		joined := tp.forceAsync(bg, "a", p.OperationID)
		close(tp.hooks.wait(t, "force-joined").release)
		tp.clk.Advance(time.Second)
		tp.log.awaitOnce(t, "terminal-committed "+off.TaskID)
		tp.log.awaitOnce(t, "removed a")
		if r, err := joined.wait(t); err != nil || r.Completed == nil {
			t.Fatalf("joined completion %+v %v", r, err)
		}
		if v := tp.show(t, off.TaskID); v.State != contract.TaskLost || !v.StopRequested {
			t.Fatalf("offline task %+v", v)
		}
		if v := tp.show(t, sent.TaskID); v.State != contract.TaskCancelled || v.Reason.Code != contract.ReasonCancelledBeforeStart {
			t.Fatalf("refused task %+v", v)
		}
		if _, err := tp.cl.RemoveRole(bg, "a", true, p.OperationID); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("retry after deletion %v", err)
		}
		tp.checkCounts(t)
	})
	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		// The caller leaves after the fence commit (the plane owns
		// completion); the plane restarts before the deletion; the durable
		// fence resumes: its task is stopped by the worker's reconciliation
		// and the role is removed; admission stays closed meanwhile.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "restart", "p2")
		started := tp.clk.Now()
		tp.hooks.pause("force-joined")
		ctx, cancel := context.WithCancel(bg)
		res := tp.forceAsync(ctx, "a", "")
		call := tp.hooks.wait(t, "force-joined")
		cancel()
		close(call.release)
		if _, err := res.wait(t); err == nil {
			t.Fatal("a departed caller got an answer")
		}
		tp.log.awaitOnce(t, "stop-intent-committed "+st.TaskID)
		op := tp.fenceOf(t, "a").OperationID
		tp.restart(t)
		if f := tp.fenceOf(t, "a"); f == nil || f.OperationID != op {
			t.Fatalf("fence after restart %+v", f)
		}
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "closed"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:role_removing")
		w, ents := tp.workerEntries(t, idA, entry(st, contract.PhaseRunning, &started, nil))
		id := tp.stopID(t, st.TaskID)
		if e := ents[st.TaskID]; e.Action != contract.ActionStopControl || e.Stop.ID != id {
			t.Fatalf("disposition %+v", e)
		}
		tp.commitResult(t, w, ctlResult(st, contract.OutcomeCancelled, &id, nil, nil, 0))
		tp.log.awaitOnce(t, "removed a")
		if _, err := tp.cl.ShowRole(bg, "a"); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatal("the resumed removal did not complete")
		}
	})
	t.Run("storage-retry", func(t *testing.T) {
		t.Parallel()
		// A fence write failing before its rename creates no operation; one
		// visible but unconfirmed cancels nothing until its resync; a
		// failing cancellation write keeps the operation alive; a failing
		// deletion is retried and never reported complete before it is
		// confirmed.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "faults", "p2")
		before := registryBytes(t, tp.root)
		tp.inj.set("rename", roleRegistryName)
		if _, err := tp.cl.RemoveRole(bg, "a", true, ""); contract.CodeOf(err) != contract.CodeInternal {
			t.Fatalf("pre-rename fault %v", err)
		}
		if string(registryBytes(t, tp.root)) != string(before) || tp.fenceOf(t, "a") != nil || tp.log.seen("stop-intent-queued "+st.TaskID) {
			t.Fatal("a failed fence created an operation")
		}
		tp.inj.set("dirsync", rolesName)
		if _, err := tp.cl.RemoveRole(bg, "a", true, ""); contract.CodeOf(err) != contract.CodeInternal {
			t.Fatalf("post-rename fault %v", err)
		}
		if tp.fenceOf(t, "a") == nil {
			t.Fatal("the visible fence is not shown")
		}
		tp.awaitRetryArmed(t, "removal-resync-failed")
		if tp.log.seen("stop-intent-queued " + st.TaskID) {
			t.Fatal("an unconfirmed fence drove a cancellation")
		}
		// Review C4: a retry (joining by token, or without one) never
		// accepts the visible but unconfirmed fence: while its directory
		// sync keeps failing it answers the storage error, never 202.
		vis := tp.fenceOf(t, "a").OperationID
		for _, tok := range []string{vis, ""} {
			r, err := tp.forceAsync(bg, "a", tok).wait(t)
			if contract.CodeOf(err) != contract.CodeInternal || r.Pending != nil || r.Completed != nil {
				t.Fatalf("retry %q of an unconfirmed fence: %+v %v", tok, r, err)
			}
		}
		if tp.log.seen("stop-intent-queued " + st.TaskID) {
			t.Fatal("a retry drove a cancellation from an unconfirmed fence")
		}
		// Confirmed on the next pass, whose cancellation write fails.
		tp.inj.set("rename", taskRel(st.TaskID))
		tp.clk.Advance(removalRetry)
		w.beat()
		tp.log.awaitOnce(t, "publish-failed stop-intent "+st.TaskID)
		// The failure's retry is due one interval later: the next attempt
		// (the first the clock enables) is held before any syscall, so no
		// attempt is in flight when the fault is cleared.
		hold := tp.th.arm("stop-intent-queued", st.TaskID)
		// The now confirmed fence's snapshot is distributed: answer it
		// before the clock moves.
		w.ackSnapshot()
		op := tp.fenceOf(t, "a").OperationID
		tp.hooks.pause("force-joined")
		jr := tp.forceAsync(bg, "a", op)
		close(tp.hooks.wait(t, "force-joined").release)
		tp.clk.Advance(3 * time.Second)
		w.beat()
		tp.clk.Advance(3 * time.Second)
		w.beat()
		if r, err := jr.wait(t); err != nil || r.Pending == nil || r.Pending.OperationID != op {
			t.Fatalf("join during failures %+v %v", r, err)
		}
		retry := paused(t, hold, "stop-intent-queued")
		tp.inj.set("", "")
		close(retry.release)
		b := w.nextCancel()
		// The deletion's rename fails, then succeeds.
		tp.inj.set("rename", roleRegistryName)
		tp.commitResult(t, w, ctlResult(st, contract.OutcomeCancelled, &b.StopID, nil, nil, 0))
		tp.awaitRetryArmed(t, "removal-delete-failed a")
		if _, err := tp.cl.ShowRole(bg, "a"); err != nil {
			t.Fatal("a failed deletion removed the role")
		}
		tp.inj.set("", "")
		tp.clk.Advance(removalRetry)
		w.beat()
		tp.log.awaitOnce(t, "removed a")
		if len(tp.svc(t).roles.load().visible.removals) != 0 {
			t.Fatal("the fence survived its deletion")
		}
	})
	t.Run("instance-reuse", func(t *testing.T) {
		t.Parallel()
		// A removed ID re-added is a new instance: the old token never
		// matches it (conflict, nothing changes), and its own forced removal
		// is a separate operation.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "old", "p2")
		// The coordinator is held right after it woke the removal's waiter:
		// a caller answered 200 then mutates at once, and must never find
		// the mutation gate still held by the completed deletion.
		tp.hooks.pause("removal-completed")
		res := tp.forceAsync(bg, "a", "")
		tp.log.awaitOnce(t, "fenced a")
		old := tp.fenceOf(t, "a").OperationID
		b := w.nextCancel()
		tp.commitResult(t, w, ctlResult(st, contract.OutcomeCancelled, &b.StopID, nil, nil, 0))
		if r, err := res.wait(t); err != nil || r.Completed == nil {
			t.Fatalf("removal %+v %v", r, err)
		}
		held := tp.hooks.wait(t, "removal-completed")
		t.Cleanup(func() {
			select {
			case <-held.release:
			default:
				close(held.release)
			}
		})
		if tp.svc(t).gate.Load() {
			t.Fatal("the 200 was answered while the deletion still held the mutation gate")
		}
		add := tp.addAsync(bg, taskCfg("a", "coder", idA, 1))
		w.peer.reply(w.until(contract.FrameRoleValidate).RequestID, nil)
		v, err := add.wait(t)
		close(held.release)
		if err != nil || v.RegistrationOrder != 2 {
			t.Fatalf("re-added right after the 200 %+v %v", v, err)
		}
		if _, err := tp.cl.RemoveRole(bg, "a", true, old); reason(err) != contract.ReasonRemovalOperationMismatch {
			t.Fatalf("old token on the new instance %v", err)
		}
		if tp.fenceOf(t, "a") != nil {
			t.Fatal("the old token fenced the new instance")
		}
		r, err := tp.cl.RemoveRole(bg, "a", true, "")
		if err != nil || r.Completed == nil {
			t.Fatalf("the new instance's removal %+v %v", r, err)
		}
		if v := tp.show(t, st.TaskID); v.State != contract.TaskCancelled || v.Role.RegistrationOrder != 1 {
			t.Fatalf("history %+v", v)
		}
	})
}
