package plane

import (
	"bytes"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// detachWorker ends w's attachment: by closing its stream, or by letting
// its lease expire on the fake clock (no write in flight).
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

// remainingCapacity is the shared remaining-capacity contract: with one
// started task fenced by the end of its attachment, the same instance's
// other slot is admissible once the node reconnects, installs and
// acknowledges its snapshot and reports ready; the old task stays
// nonterminal and recovery-required; nothing is replayed.
func remainingCapacity(t *testing.T, byLease bool) {
	tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
	w := tp.worker(t, idA)
	old := tp.run(t, w, "a", "old", "p2")
	tp.detachWorker(t, w, byLease)
	v := tp.show(t, old.TaskID)
	if v.State != contract.TaskRunning || !v.RecoveryRequired || *v.RecoveryReason != contract.RecoveryAttachmentLost || tp.roleInflight(t, "a") != 1 {
		t.Fatalf("fenced task %+v", v)
	}
	w = tp.worker(t, idA)
	nv := tp.admit(t, taskReq(contract.TargetID, "a", "new"))
	if nv.Role.RegistrationOrder != 1 || nv.Role.ID != "a" || tp.roleInflight(t, "a") != 2 {
		t.Fatalf("second task %+v (inflight %d)", nv.Role, tp.roleInflight(t, "a"))
	}
	if st := w.start("p2"); st.TaskID != nv.TaskID {
		t.Fatalf("replayed %s", st.TaskID)
	}
	if v := tp.show(t, old.TaskID); !v.RecoveryRequired || v.State != contract.TaskRunning {
		t.Fatalf("old task after reuse %+v", v)
	}
	cs := wantAdmissionOK(t, tp, "a")
	if cs.Inflight != 2 || cs.RecoveryInflight != 1 || cs.Reason != contract.ReasonFull {
		t.Fatalf("candidate %+v", cs)
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

// recoveryRemove is the shared recovery-only removal contract: a fenced
// task's reserved slot blocks dispatch; plain or forced rm removes only
// the registry entry (the task's record, state, log and order unchanged,
// no signal); the re-added role is a new instance that starts at zero;
// the old task derives role_removed, also after a reload.
func recoveryRemove(t *testing.T, force bool) {
	tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
	w := tp.worker(t, idA)
	old := tp.run(t, w, "a", "old", "p2")
	w.log(old, 0, []byte("kept\n"))
	tp.clk.Advance(checkpointEvery)
	tp.log.await(t, "published checkpoint "+old.TaskID)
	tp.detachWorker(t, w, false)
	w = tp.worker(t, idA)
	if c := wantAdmissionOK(t, tp, "a"); c.Reason != contract.ReasonFull || c.RecoveryInflight != 1 {
		t.Fatalf("reserved slot %+v", c)
	}
	before := taskBytes(t, tp.root, old.TaskID)
	if err := tp.cl.RemoveRole(bg, "a", force); err != nil {
		t.Fatalf("recovery-only rm (force %v): %v", force, err)
	}
	if b := w.ackReplace("p2"); len(b.Roles) != 0 {
		t.Fatalf("snapshot after rm %+v", b)
	}
	v := tp.show(t, old.TaskID)
	if v.State != contract.TaskRunning || *v.RecoveryReason != contract.RecoveryRoleRemoved || v.Role.RegistrationOrder != 1 {
		t.Fatalf("history %+v", v)
	}
	rv := tp.add(t, w.peer, "p3", "p4", taskCfg("a", "coder", idA, 1))
	if rv.RegistrationOrder != 2 || rv.Inflight != 0 {
		t.Fatalf("re-added %+v", rv)
	}
	w.ready["a"] = true
	w.beat()
	nv := tp.admit(t, taskReq(contract.TargetID, "a", "new"))
	if nv.Role.RegistrationOrder != 2 || tp.roleInflight(t, "a") != 1 {
		t.Fatalf("new instance task %+v", nv.Role)
	}
	w.start("p5")
	if !bytes.Equal(before, taskBytes(t, tp.root, old.TaskID)) {
		t.Fatal("removal rewrote the old task")
	}
	tp.restart(t)
	if v := tp.show(t, old.TaskID); *v.RecoveryReason != contract.RecoveryRoleRemoved || v.LogTail != "kept\n" {
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
		// rm of a busy role is refused whole (tasks_inflight; with force,
		// force_cancel_not_supported), with counts; rm and dispatch are
		// serialized by the shared gate; set never rewrites a running
		// task's snapshot.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
		w := tp.worker(t, idA)
		st := tp.run(t, w, "a", "busy", "p2")
		for _, force := range []bool{false, true} {
			err := tp.cl.RemoveRole(bg, "a", force)
			want := map[bool]string{false: contract.ReasonTasksInflight, true: contract.ReasonForceNotSupported}[force]
			if contract.CodeOf(err) != contract.CodeConflict || reasonOf(err) != want {
				t.Fatalf("rm force=%v: %v", force, err)
			}
			if d := err.(*contract.Error).Details; d["inflight"] == nil || d["recovery_inflight"] == nil {
				t.Fatalf("rm details %v", d)
			}
		}
		if tp.view(t, "a").ID != "a" {
			t.Fatal("a refused rm removed something")
		}
		pub := tp.th.arm("before-task-publication", "racing")
		res := tp.dispatchAsync(taskReq(contract.TargetID, "a", "racing"))
		p := paused(t, pub, "racing")
		wantReason(t, tp.cl.RemoveRole(bg, "a", false), contract.CodeUnavailable, contract.ReasonBusy)
		close(p.release)
		racing, err := res.wait(t)
		if err != nil {
			t.Fatal(err)
		}
		model := "changed model"
		sres := tp.setAsync(bg, "a", contract.RolePatch{Model: &model})
		w.start("p3")
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
			// One uncertain and one current reservation: both rm forms are
			// refused until the current task completes durably; then the
			// all-uncertain role is removable.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			tp.run(t, w, "a", "uncertain", "p2")
			tp.detachWorker(t, w, false)
			w = tp.worker(t, idA)
			cur := tp.run(t, w, "a", "current", "p2")
			for _, force := range []bool{false, true} {
				err := tp.cl.RemoveRole(bg, "a", force)
				d, _ := err.(*contract.Error)
				if contract.CodeOf(err) != contract.CodeConflict {
					t.Fatalf("mixed rm force=%v: %v", force, err)
				}
				held, _ := d.DetailInt("inflight")
				rec, _ := d.DetailInt("recovery_inflight")
				if held != 2 || rec != 1 {
					t.Fatalf("mixed counts %v", d.Details)
				}
			}
			tp.finish(t, w, cur, 0, 0)
			if err := tp.cl.RemoveRole(bg, "a", false); err != nil {
				t.Fatalf("rm after the current task: %v", err)
			}
		})
		t.Run("mailbox", func(t *testing.T) {
			t.Parallel()
			// A captured result awaiting persistence is not
			// recovery-required, even after detach: rm is refused until its
			// commit completes.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "captured", "p2")
			hold := tp.th.arm("terminal-commit-queued", st.TaskID)
			w.resultAck(w.sendResult(result(st, 0, 0, nil)), st.TaskID)
			call := paused(t, hold, "terminal-commit-queued")
			tp.detachWorker(t, w, false)
			if err := tp.cl.RemoveRole(bg, "a", true); contract.CodeOf(err) != contract.CodeConflict {
				t.Fatalf("rm with a pending commit: %v", err)
			}
			close(call.release)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			if err := tp.cl.RemoveRole(bg, "a", false); err != nil {
				t.Fatalf("rm after the commit: %v", err)
			}
		})
	})
}
