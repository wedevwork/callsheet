package plane

import (
	"bytes"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// detachWorker ends w's attachment: by closing its stream, or by letting
// its lease expire on the fake clock (no write in flight); an expired
// lease also resolves the node's undecided tasks lost (iteration 06a).
func (tp *taskPlane) detachWorker(t *testing.T, w *worker, byLease bool) {
	t.Helper()
	if byLease {
		tp.clk.Advance(leaseDuration)
		// Answer the plane's close so it never waits out its grace.
		w.closed()
	} else {
		w.c.CloseNow()
	}
	tp.log.await(t, "detached "+idA)
}

// remainingCapacity is the shared remaining-capacity contract (06a
// boundary): with one started task whose attachment ended, the node's
// return decides. Before its lease expires the new attachment reconciles
// the running execution (continue, no replay) and the instance's other
// slot is admissible with both reservations held. When the lease expired
// first, the task is resolved lost and its slot released once the loss
// is durable; the returning worker is told to stop it (stop_lost) and a
// new task is admissible; the old start is never replayed.
func remainingCapacity(t *testing.T, byLease bool) {
	tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
	w := tp.worker(t, idA)
	old := tp.run(t, w, "a", "old", "p2")
	started := tp.clk.Now()
	tp.detachWorker(t, w, byLease)
	want := contract.ActionContinue
	if byLease {
		tp.log.awaitOnce(t, "lost-latched "+old.TaskID+" "+contract.ReasonLeaseExpired)
		tp.log.awaitOnce(t, "terminal-committed "+old.TaskID)
		if v := tp.show(t, old.TaskID); v.State != contract.TaskLost || v.Reason.Code != contract.ReasonLeaseExpired || tp.roleInflight(t, "a") != 0 {
			t.Fatalf("expired task %+v (inflight %d)", v, tp.roleInflight(t, "a"))
		}
		want = contract.ActionStopLost
	} else if v := tp.show(t, old.TaskID); v.State != contract.TaskRunning || !v.Reconciling || tp.roleInflight(t, "a") != 1 {
		t.Fatalf("fenced task %+v", v)
	}
	w = tp.workerInv(t, idA, map[string]string{old.TaskID: want}, entry(old, contract.PhaseRunning, &started, nil))
	nv := tp.admit(t, taskReq(contract.TargetID, "a", "new"))
	held := 2
	if byLease {
		held = 1
	}
	if nv.Role.RegistrationOrder != 1 || nv.Role.ID != "a" || tp.roleInflight(t, "a") != held {
		t.Fatalf("second task %+v (inflight %d)", nv.Role, tp.roleInflight(t, "a"))
	}
	if st := w.start("p2"); st.TaskID != nv.TaskID {
		t.Fatalf("replayed %s", st.TaskID)
	}
	if v := tp.show(t, old.TaskID); v.Reconciling || (byLease != (v.State == contract.TaskLost)) {
		t.Fatalf("old task after reuse %+v", v)
	}
	if !byLease {
		cs := wantAdmissionOK(t, tp, "a")
		if cs.Inflight != 2 || cs.ReconcilingInflight != 0 || cs.Reason != contract.ReasonFull {
			t.Fatalf("candidate %+v", cs)
		}
	}
}

// wantAdmissionOK returns role's candidate from a refused dispatch.
func wantAdmissionOK(t *testing.T, tp *taskPlane, role string) contract.TaskCandidate {
	t.Helper()
	_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, role, "probe"))
	if err == nil {
		t.Fatal("the probe dispatch was admitted")
	}
	return candidatesOf(t, err)[0]
}

// wantRemovalBusy requires ordinary rm to be refused for role a with the
// given held and reconciling counts (iteration 06b: a forced removal
// fences and cancels instead, TestControlRemove).
func wantRemovalBusy(t *testing.T, tp *taskPlane, held, reconciling int) {
	t.Helper()
	for _, force := range []bool{false} {
		err := rmRole(tp.cl, bg, "a", force)
		d, _ := err.(*contract.Error)
		why := contract.ReasonTasksInflight
		if contract.CodeOf(err) != contract.CodeConflict || d == nil || d.Details["reason"] != why {
			t.Fatalf("rm force=%v: %v", force, err)
		}
		h, _ := d.DetailInt("inflight")
		r, _ := d.DetailInt("reconciling_inflight")
		if h != held || r != reconciling {
			t.Fatalf("rm force=%v counts %v", force, d.Details)
		}
	}
}

// recoveryRemove is the shared removal contract (06a replaced 05's
// recovery-only exception): a task whose attachment ended holds its
// reservation, so ordinary and forced rm stay busy; at lease expiry the
// task is resolved lost, and rm stays busy until that loss is durably
// published. Then rm (plain or forced) removes only the registry entry:
// the lost record, its log and historical instance are unchanged and
// reload unchanged; the re-added role is a new instance that starts at
// zero.
func recoveryRemove(t *testing.T, force bool) {
	tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
	w := tp.worker(t, idA)
	old := tp.run(t, w, "a", "old", "p2")
	w.log(old, 0, []byte("kept\n"))
	tp.clk.Advance(checkpointEvery)
	tp.log.await(t, "published checkpoint "+old.TaskID)
	tp.detachWorker(t, w, false)
	wantRemovalBusy(t, tp, 1, 1)
	hold := tp.th.arm("terminal-commit-queued", old.TaskID)
	tp.clk.Advance(leaseDuration)
	call := paused(t, hold, "terminal-commit-queued")
	tp.log.awaitOnce(t, "lost-latched "+old.TaskID+" "+contract.ReasonLeaseExpired)
	// Latched but not yet durable: still busy.
	wantRemovalBusy(t, tp, 1, 0)
	close(call.release)
	tp.log.awaitOnce(t, "terminal-committed "+old.TaskID)
	before := taskBytes(t, tp.root, old.TaskID)
	if err := rmRole(tp.cl, bg, "a", force); err != nil {
		t.Fatalf("rm after the durable loss (force %v): %v", force, err)
	}
	v := tp.show(t, old.TaskID)
	if v.State != contract.TaskLost || v.Reason.Code != contract.ReasonLeaseExpired || v.Role.RegistrationOrder != 1 || v.LogTail != "kept\n" {
		t.Fatalf("history %+v", v)
	}
	w = tp.worker(t, idA)
	rv := tp.add(t, w.peer, "p2", "p3", taskCfg("a", "coder", idA, 1))
	if rv.RegistrationOrder != 2 || rv.Inflight != 0 {
		t.Fatalf("re-added %+v", rv)
	}
	w.ready["a"] = true
	w.beat()
	nv := tp.admit(t, taskReq(contract.TargetID, "a", "new"))
	if nv.Role.RegistrationOrder != 2 || tp.roleInflight(t, "a") != 1 {
		t.Fatalf("new instance task %+v", nv.Role)
	}
	w.start("p4")
	if !bytes.Equal(before, taskBytes(t, tp.root, old.TaskID)) {
		t.Fatal("removal rewrote the old task")
	}
	tp.restart(t)
	if v := tp.show(t, old.TaskID); v.State != contract.TaskLost || v.LogTail != "kept\n" || v.Role.RegistrationOrder != 1 {
		t.Fatalf("reloaded history %+v", v)
	}
	if !bytes.Equal(before, taskBytes(t, tp.root, old.TaskID)) {
		t.Fatal("reload rewrote the old task")
	}
	if n := tp.roleInflight(t, "a"); n != 1 {
		t.Fatalf("new instance reservations after reload %d", n)
	}
}

// TestTaskRoleIntegration is UT FP-8 for real counts, set and rm; its
// counts, mutation, remaining-capacity and recovery-remove subtests are
// delegated from tests/function (TestTaskRoles), together with the
// sidecar's. Do not rename or skip them.
func TestTaskRoleIntegration(t *testing.T) {
	t.Parallel()
	t.Run("counts", func(t *testing.T) {
		t.Parallel()
		// Role ls/show and node views report the plane's held
		// reservations; heartbeat counts are observations only; a lowered
		// concurrency may sit below inflight; confirmed terminal
		// publication releases the slot.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
		w := tp.worker(t, idA)
		t1 := tp.run(t, w, "a", "one", "p2")
		t2 := tp.run(t, w, "a", "two", "p3")
		list, err := tp.cl.ListRoles(bg)
		if err != nil || list[0].Inflight != 2 || list[0].CanAccept {
			t.Fatalf("role ls %+v %v", list, err)
		}
		n := tp.nodePlane.show(t, idA)
		if n.Roles[0].Inflight != 2 || n.Roles[0].CanAccept {
			t.Fatalf("node view %+v", n.Roles)
		}
		// A worker's own count (even beyond the concurrency) changes no
		// plane reservation.
		rid := w.nextID()
		w.send(contract.ProtocolVersion, contract.FrameHeartbeat, rid, contract.HeartbeatBody{RolesRevision: w.rev,
			Roles: []contract.RoleStatus{{RoleID: "a", Inflight: 7, Concurrency: 2}}})
		w.reply(contract.FrameHeartbeatAck, rid)
		if tp.roleInflight(t, "a") != 2 {
			t.Fatal("a heartbeat changed the plane's count")
		}
		one := 1
		res := tp.setAsync(bg, "a", contract.RolePatch{Concurrency: &one})
		w.validateOK("p4")
		if v, err := res.wait(t); err != nil || v.Concurrency != 1 || v.Inflight != 2 || v.CanAccept {
			t.Fatalf("lowered %+v %v", v, err)
		}
		w.ackReplace("p5")
		w.beat()
		tp.finish(t, w, t1, 0, 0)
		if v := tp.view(t, "a"); v.Inflight != 1 || v.CanAccept {
			t.Fatalf("after one release %+v", v)
		}
		tp.finish(t, w, t2, 0, 0)
		if v := tp.view(t, "a"); v.Inflight != 0 || !v.CanAccept {
			t.Fatalf("after both releases %+v", v)
		}
	})
	t.Run("mutation", func(t *testing.T) {
		t.Parallel()
		// Ordinary rm of a busy role is refused whole (tasks_inflight), with
		// counts (a forced one cancels: TestControlRemove); rm and dispatch are
		// serialized by the shared gate; set never rewrites a running
		// task's snapshot.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
		w := tp.worker(t, idA)
		st := tp.run(t, w, "a", "busy", "p2")
		for _, force := range []bool{false} {
			err := rmRole(tp.cl, bg, "a", force)
			want := contract.ReasonTasksInflight
			if contract.CodeOf(err) != contract.CodeConflict || reasonOf(err) != want {
				t.Fatalf("rm force=%v: %v", force, err)
			}
			if d := err.(*contract.Error).Details; d["inflight"] == nil || d["reconciling_inflight"] == nil {
				t.Fatalf("rm details %v", d)
			}
		}
		if tp.view(t, "a").ID != "a" {
			t.Fatal("a refused rm removed something")
		}
		pub := tp.th.arm("before-task-publication", "racing")
		res := tp.dispatchAsync(taskReq(contract.TargetID, "a", "racing"))
		p := paused(t, pub, "racing")
		wantReason(t, rmRole(tp.cl, bg, "a", false), contract.CodeUnavailable, contract.ReasonBusy)
		close(p.release)
		racing, err := res.wait(t)
		if err != nil {
			t.Fatal(err)
		}
		// The dispatch returned with its start queued, not written, and a
		// waiting validation takes the free request slot before queued
		// starts: read the start first, so the set's validation is p4.
		w.start("p3")
		model := "changed model"
		sres := tp.setAsync(bg, "a", contract.RolePatch{Model: &model})
		w.answer("p3", racing.TaskID, nil)
		w.validateOK("p4")
		if _, err := sres.wait(t); err != nil {
			t.Fatal(err)
		}
		w.ackReplace("p5")
		if v := tp.show(t, st.TaskID); v.Effective.Model != "example model" {
			t.Fatalf("set rewrote the task's settings: %+v", v.Effective)
		}
		if r := tp.taskFile(t, st.TaskID); r.Role.Model != "example model" {
			t.Fatalf("set rewrote the task's role snapshot: %+v", r.Role)
		}
	})
	t.Run("remaining-capacity", func(t *testing.T) {
		t.Parallel()
		remainingCapacity(t, false)
	})
	t.Run("recovery-remove", func(t *testing.T) {
		t.Parallel()
		// The plain and forced removal/re-add of an all-uncertain role,
		// with its reload, is the shared recoveryRemove contract run by
		// TestTaskInterruption/recovery-remove; here the refusal side.
		t.Run("mixed", func(t *testing.T) {
			t.Parallel()
			// One reconciling and one current reservation: both rm forms are
			// refused; the current task's durable completion leaves the
			// reconciling one, still busy; its lease's expiry resolves it
			// lost, and only then is the role removable.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			unc := tp.run(t, w, "a", "uncertain", "p2")
			started := tp.clk.Now()
			tp.detachWorker(t, w, false)
			w = tp.workerInv(t, idA, map[string]string{unc.TaskID: contract.ActionContinue}, entry(unc, contract.PhaseRunning, &started, nil))
			cur := tp.run(t, w, "a", "current", "p2")
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			wantRemovalBusy(t, tp, 2, 2)
			w = tp.workerInv(t, idA, map[string]string{unc.TaskID: contract.ActionContinue, cur.TaskID: contract.ActionContinue},
				entry(unc, contract.PhaseRunning, &started, nil), entry(cur, contract.PhaseRunning, &started, nil))
			tp.finish(t, w, cur, 0, 0)
			wantRemovalBusy(t, tp, 1, 0)
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			tp.clk.Advance(leaseDuration)
			tp.log.awaitOnce(t, "terminal-committed "+unc.TaskID)
			if err := rmRole(tp.cl, bg, "a", false); err != nil {
				t.Fatalf("rm after the durable loss: %v", err)
			}
		})
		t.Run("mailbox", func(t *testing.T) {
			t.Parallel()
			// A captured result awaiting persistence is not reconciling,
			// even after detach: rm is refused until its commit completes.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "captured", "p2")
			hold := tp.th.arm("terminal-commit-queued", st.TaskID)
			w.resultAck(w.sendResult(result(st, 0, 0, nil)), st.TaskID)
			call := paused(t, hold, "terminal-commit-queued")
			tp.detachWorker(t, w, false)
			if err := rmRole(tp.cl, bg, "a", false); contract.CodeOf(err) != contract.CodeConflict {
				t.Fatalf("rm with a pending commit: %v", err)
			}
			close(call.release)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			if err := rmRole(tp.cl, bg, "a", false); err != nil {
				t.Fatalf("rm after the commit: %v", err)
			}
		})
	})
}
