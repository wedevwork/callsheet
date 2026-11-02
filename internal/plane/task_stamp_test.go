package plane

import (
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// wantStamp requires st's revision, and that of its durable record, to be
// the revision w installed.
func wantStamp(t *testing.T, tp *taskPlane, w *worker, st contract.TaskStartBody) {
	t.Helper()
	if st.RolesRevision != w.rev {
		t.Fatalf("task_start of %s stamped %d, node %s installed %d", st.Role.ID, st.RolesRevision, w.node, w.rev)
	}
	if r := tp.taskFile(t, st.TaskID); r.RolesRevision != w.rev {
		t.Fatalf("record of %s stamped %d, node %s installed %d", st.Role.ID, r.RolesRevision, w.node, w.rev)
	}
}

// TestTaskStartRevision is the plane side of the cross-node role_changed
// defect: a start carries the revision of its node's own snapshot, which
// the node has installed (admission requires it), never a registry revision
// that only another node received.
func TestTaskStartRevision(t *testing.T) {
	t.Parallel()
	twoNodes := func(t *testing.T) (*taskPlane, *worker, *worker) {
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 4), 1), record(taskCfg("b", "coder", idB, 4), 2)}, idA, idB)
		return tp, tp.worker(t, idA), tp.worker(t, idB)
	}
	t.Run("other-node-mutation", func(t *testing.T) {
		t.Parallel()
		tp, wa, wb := twoNodes(t)
		tp.add(t, wb.peer, "p2", "p3", taskCfg("c", "coder", idB, 4))
		wb.ready["c"] = true
		wb.beat()
		visible := tp.svc(t).roles.load().visible.revision
		if wa.rev >= visible || wb.rev != visible {
			t.Fatalf("revisions: node A %d, node B %d, registry %d", wa.rev, wb.rev, visible)
		}
		wantStamp(t, tp, wa, tp.run(t, wa, "a", "stale node", "p2"))
		wantStamp(t, tp, wb, tp.run(t, wb, "c", "mutated node", "p4"))

		// The node's own change raises its stamp once it has installed it,
		// and masks the node until then.
		res := tp.addAsync(bg, taskCfg("d", "coder", idA, 4))
		wa.validateOK("p3")
		if _, err := res.wait(t); err != nil {
			t.Fatal(err)
		}
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "unsynced"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:role_unsynced")
		wa.ackReplace("p4")
		wa.ready["d"] = true
		wa.beat()
		if wa.rev <= visible {
			t.Fatalf("node A at %d after its own change, registry was %d", wa.rev, visible)
		}
		wantStamp(t, tp, wa, tp.run(t, wa, "a", "own change", "p5"))
		wantStamp(t, tp, wb, tp.run(t, wb, "b", "now the stale node", "p5"))
	})
	t.Run("blocked", func(t *testing.T) {
		t.Parallel()
		// A visible but unconfirmed publication is never distributed and
		// admits nothing. A node attaching meanwhile installs the confirmed
		// snapshot, older than the visible document, and confirmation
		// advances only the changed node: the attached node's stamp stays
		// its own, the changed node's follows once installed.
		tp, wa, wb := twoNodes(t)
		tp.inj.set("dirsync", rolesName)
		res := tp.addAsync(bg, taskCfg("c", "coder", idB, 4))
		wb.validateOK("p2")
		if _, err := res.wait(t); contract.CodeOf(err) != contract.CodeInternal {
			t.Fatalf("unconfirmed add: %v", err)
		}
		before := tp.taskCount(t)
		if _, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "blocked")); contract.CodeOf(err) != contract.CodeInternal {
			t.Fatalf("dispatch while blocked: %v", err)
		}
		if n := tp.taskCount(t); n != before {
			t.Fatalf("a blocked registry admitted a task: %d -> %d", before, n)
		}
		wa.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		wa = tp.worker(t, idA)
		rs := tp.svc(t).roles.load()
		if !rs.blocked || wa.rev != rs.confirmed.revision || wa.rev >= rs.visible.revision {
			t.Fatalf("attached while blocked at %d; blocked %v, confirmed %d, visible %d", wa.rev, rs.blocked, rs.confirmed.revision, rs.visible.revision)
		}
		visible := rs.visible.revision

		tp.inj.set("", "")
		wantStamp(t, tp, wa, tp.run(t, wa, "a", "after confirmation", "p2"))
		if rs := tp.svc(t).roles.load(); rs.blocked || rs.confirmed.revision != visible {
			t.Fatalf("after the retried sync: blocked %v, confirmed %d, want %d", rs.blocked, rs.confirmed.revision, visible)
		}
		// Node B's snapshot is on its way: masked until it is installed.
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "c", "unsynced"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "c:role_unsynced")
		wb.ackReplace("p3")
		wb.ready["c"] = true
		wb.beat()
		if wb.rev != visible {
			t.Fatalf("node B installed %d, want %d", wb.rev, visible)
		}
		wantStamp(t, tp, wb, tp.run(t, wb, "c", "confirmed role", "p4"))
	})
}
