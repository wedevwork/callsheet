package plane

import (
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestTaskInterruption is UT FP-9 for the interrupted-execution boundary
// on the plane: no fabricated outcome, no replay, reservations held; its
// disconnect, remaining-capacity and recovery-remove subtests are
// delegated from tests/function (TestTaskRecoveryBoundary), together with
// the sidecar's. Do not rename or skip them.
func TestTaskInterruption(t *testing.T) {
	t.Parallel()
	t.Run("disconnect", func(t *testing.T) {
		t.Parallel()
		// Three points of loss: a start queued behind another and never
		// written is rejected (start_not_sent, slot released); a start
		// written without a reply stays pending and uncertain; a started
		// task stays running and uncertain. An offline lease releases
		// nothing, and the old generation's report is refused on the next
		// attachment.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 3), 1)}, idA)
		w := tp.worker(t, idA)
		running := tp.run(t, w, "a", "after start", "p2")
		written := tp.admit(t, taskReq(contract.TargetID, "a", "ambiguous write"))
		w.start("p3")
		queued := tp.admit(t, taskReq(contract.TargetID, "a", "never sent"))
		tp.log.awaitOnce(t, "start-queued "+idA+" "+queued.TaskID)
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		tp.log.awaitOnce(t, "terminal-committed "+queued.TaskID)
		for id, want := range map[string]struct{ state, reason string }{
			running.TaskID: {contract.TaskRunning, contract.RecoveryAttachmentLost},
			written.TaskID: {contract.TaskPending, contract.RecoveryStartUnconfirmed},
		} {
			v := tp.show(t, id)
			if v.State != want.state || !v.RecoveryRequired || *v.RecoveryReason != want.reason {
				t.Fatalf("%s = %+v", id, v)
			}
		}
		if v := tp.show(t, queued.TaskID); v.State != contract.TaskRejected || v.Reason.Code != contract.ReasonStartNotSent || v.RecoveryRequired {
			t.Fatalf("never sent = %+v", v)
		}
		if tp.roleInflight(t, "a") != 2 {
			t.Fatalf("inflight %d", tp.roleInflight(t, "a"))
		}
		tp.clk.Advance(leaseDuration)
		if n := tp.nodePlane.show(t, idA); n.Liveness != contract.LivenessOffline || n.Roles[0].Inflight != 2 {
			t.Fatalf("offline node %+v", n)
		}
		w = tp.worker(t, idA)
		expectClosed(t, w, w.sendLog(running, 0, []byte("old generation")))
		if v := tp.show(t, running.TaskID); v.Log.ReceivedBytes != 0 || v.State != contract.TaskRunning {
			t.Fatalf("old generation output accepted: %+v", v)
		}
	})
	t.Run("remaining-capacity", func(t *testing.T) {
		t.Parallel()
		remainingCapacity(t, true)
	})
	t.Run("recovery-remove", func(t *testing.T) {
		t.Parallel()
		t.Run("plain", func(t *testing.T) { t.Parallel(); recoveryRemove(t, false) })
		t.Run("force", func(t *testing.T) { t.Parallel(); recoveryRemove(t, true) })
	})
}
