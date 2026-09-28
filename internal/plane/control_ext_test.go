package plane

import (
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b extensions of the 06a families: TestControlCommit's
// stop-versus-result durability and TestControlProtocol's control frames.

// stopIntentDurability: the serialized candidate that obtains durability
// decides. An intent whose write fails before its rename holds back the
// natural result that arrives meanwhile (cancelled once it lands); an
// intent never made durable before a restart decides nothing (the
// worker's natural result governs the loaded record); a durable intent
// survives a restart and governs the result reported afterwards; the
// terminal write's own faults retry the same control candidate and
// release the reservation once.
func stopIntentDurability(t *testing.T) {
	t.Run("before-rename", func(t *testing.T) {
		t.Parallel()
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "intent fault", "p2")
		started := tp.clk.Now()
		tp.inj.set("rename", taskRel(st.TaskID))
		pending := tp.cancelAsync(bg, st.TaskID)
		tp.log.awaitOnce(t, "publish-failed stop-intent "+st.TaskID)
		res := result(st, 0, 0, nil)
		w.resultAck(w.sendResult(res), st.TaskID)
		tp.log.awaitOnce(t, "result-captured "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || v.StopRequested || !v.CompletionPending {
			t.Fatalf("held behind the intent %+v", v)
		}
		// The plane restarts before the intent is durable: nothing was
		// decided; the natural result governs.
		tp.inj.set("", "")
		tp.restart(t)
		// The caller of the undurable intent was never told it was accepted.
		if r, err := pending.wait(t); err == nil || r.Accepted {
			t.Fatalf("an undurable intent was answered %+v %v", r, err)
		}
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || v.StopRequested {
			t.Fatalf("after restart %+v", v)
		}
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &res.Digest))
		tp.commitResult(t, w, res)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskSucceeded || v.StopRequested {
			t.Fatalf("natural after an undurable intent %+v", v)
		}
		tp.checkCounts(t)
	})
	t.Run("durable-restart", func(t *testing.T) {
		t.Parallel()
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "durable", "p2")
		started := tp.clk.Now()
		if r := tp.cancel(t, st.TaskID); !r.Accepted {
			t.Fatalf("cancel %+v", r)
		}
		id := tp.stopID(t, st.TaskID)
		tp.restart(t)
		v := tp.show(t, st.TaskID)
		if v.State != contract.TaskRunning || !v.StopRequested || !v.Reconciling {
			t.Fatalf("loaded intent %+v", v)
		}
		// A natural result reported after the durable intent is cleanup
		// evidence: cancelled with its exit code.
		res := result(st, 0, 0, nil)
		w, ents := tp.workerEntries(t, idA, entry(st, contract.PhaseResult, &started, &res.Digest))
		if e := ents[st.TaskID]; e.Action != contract.ActionSendResult {
			t.Fatalf("disposition %+v", e)
		}
		// The terminal write fails once before its rename: retried, the
		// same candidate, one release.
		tp.inj.set("rename", taskRel(st.TaskID))
		w.resultAck(w.sendResult(res), st.TaskID)
		tp.log.awaitOnce(t, "publish-failed terminal "+st.TaskID)
		tp.inj.set("", "")
		tp.clk.Advance(storageRetry)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		v = tp.show(t, st.TaskID)
		if v.State != contract.TaskCancelled || *v.Result.ExitCode != 0 || tp.stopID(t, st.TaskID) != id || tp.roleInflight(t, "a") != 0 {
			t.Fatalf("durable intent %+v", v)
		}
		tp.checkCounts(t)
	})
}

// controlFairness: controls share the attachment's single p<n> counter
// and outstanding slot with starts and snapshots; heartbeats keep their
// priority between controls (a flood of cancels never starves them); an
// unsolicited or mismatched task_cancel_ack ends the attachment.
func controlFairness(t *testing.T) {
	tp, w := ctlPlane(t, 3)
	var sts []contract.TaskStartBody
	for i, pid := range []string{"p2", "p3", "p4"} {
		sts = append(sts, tp.run(t, w, "a", "flood "+itoa(i), pid))
	}
	for _, st := range sts {
		if r := tp.cancel(t, st.TaskID); !r.Accepted {
			t.Fatalf("cancel %+v", r)
		}
	}
	seen := map[string]bool{}
	for i, pid := range []string{"p5", "p6", "p7"} {
		f := w.expect(contract.FrameTaskCancel, pid)
		b, err := contract.DecodeTaskCancel(f.Body)
		if err != nil || seen[b.TaskID] {
			t.Fatalf("control %s: %+v %v", pid, b, err)
		}
		seen[b.TaskID] = true
		// A heartbeat while the control is outstanding is answered first.
		w.beat()
		w.ackCancel(pid, b)
		_ = i
	}
	// Unsolicited acknowledgement: the attachment ends.
	st := sts[0]
	w.send(contract.ProtocolVersion, contract.FrameTaskCancelAck, "p9", contract.TaskCancelAckBody{TaskID: st.TaskID, Execution: st.Execution,
		StopID: strings.Repeat("a", 32), Received: true})
	w.expectError("p9", contract.CodeInvalidArgument)
	w.detached()
}
