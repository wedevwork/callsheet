package plane

import (
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestTaskInterruption is UT FP-9 of iteration 05, its assertions replaced
// by iteration 06a's lease/reconnect boundary on the plane: an ended
// attachment fabricates no outcome and replays nothing; its tasks await
// reconciliation with their slots held until the node's lease expires,
// which resolves them lost and releases the slots once durable. Its
// disconnect, remaining-capacity and recovery-remove subtests are
// delegated from tests/function (TestTaskRecoveryBoundary), together
// with the sidecar's. Do not rename or skip them.
func TestTaskInterruption(t *testing.T) {
	t.Parallel()
	t.Run("disconnect", func(t *testing.T) {
		t.Parallel()
		// Three points of loss: a start queued behind another and never
		// written is rejected (start_not_sent, slot released); a start
		// written without a reply stays pending and reconciling; a started
		// task stays running and reconciling. Closing the stream declares
		// nothing lost; the lease's expiry resolves both lost (a queued
		// report of the old generation is refused afterwards) and releases
		// their slots once durable; the returning worker is told to stop
		// the running one, and no start is replayed.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 3), 1)}, idA)
		w := tp.worker(t, idA)
		running := tp.run(t, w, "a", "after start", "p2")
		started := tp.clk.Now()
		written := tp.admit(t, taskReq(contract.TargetID, "a", "ambiguous write"))
		w.start("p3")
		queued := tp.admit(t, taskReq(contract.TargetID, "a", "never sent"))
		tp.log.awaitOnce(t, "start-queued "+idA+" "+queued.TaskID)
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		tp.log.awaitOnce(t, "terminal-committed "+queued.TaskID)
		for id, state := range map[string]string{running.TaskID: contract.TaskRunning, written.TaskID: contract.TaskPending} {
			v := tp.show(t, id)
			if v.State != state || !v.Reconciling || !v.Log.LogMayBeIncomplete {
				t.Fatalf("%s = %+v", id, v)
			}
		}
		if v := tp.show(t, queued.TaskID); v.State != contract.TaskRejected || v.Reason.Code != contract.ReasonStartNotSent || v.Reconciling {
			t.Fatalf("never sent = %+v", v)
		}
		if tp.roleInflight(t, "a") != 2 {
			t.Fatalf("inflight %d", tp.roleInflight(t, "a"))
		}
		tp.clk.Advance(leaseDuration)
		for _, id := range []string{running.TaskID, written.TaskID} {
			tp.log.awaitOnce(t, "lost-latched "+id+" "+contract.ReasonLeaseExpired)
			tp.log.awaitOnce(t, "terminal-committed "+id)
		}
		if n := tp.nodePlane.show(t, idA); n.Liveness != contract.LivenessOffline || n.Roles[0].Inflight != 0 {
			t.Fatalf("offline node %+v", n)
		}
		if v := tp.show(t, running.TaskID); v.State != contract.TaskLost || v.StartedAt == nil || v.Result.ExitCode != nil {
			t.Fatalf("lost running task %+v", v)
		}
		if v := tp.show(t, written.TaskID); v.State != contract.TaskLost || v.StartedAt != nil {
			t.Fatalf("lost pending task %+v", v)
		}
		w = tp.workerInv(t, idA, map[string]string{running.TaskID: contract.ActionStopLost}, entry(running, contract.PhaseRunning, &started, nil))
		if v := tp.show(t, running.TaskID); v.State != contract.TaskLost || v.Log.ReceivedBytes != 0 {
			t.Fatalf("after return %+v", v)
		}
		// A primary chunk of the old execution is refused: it is decided.
		expectClosed(t, w, w.sendLog(running, 0, []byte("old generation")))
		n := 0
		for _, e := range tp.log.all() {
			if strings.HasPrefix(e, "start-writing "+idA+" ") && strings.HasSuffix(e, " "+running.TaskID) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("the start was written %d times", n)
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
