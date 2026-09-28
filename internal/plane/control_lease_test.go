package plane

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestControlLease is UT FP-3 on the plane, delegated from tests/function
// (TestControlNodeLoss) with the sidecar's: the lease boundary at expiry
// minus one, exactly and plus one tick; pending, ambiguous, running,
// queued and storage-blocked tasks at expiry; reconnection before and
// after expiry; no reroute; removed and re-added roles. Do not rename or
// skip it.
func TestControlLease(t *testing.T) {
	t.Parallel()
	t.Run("boundary", func(t *testing.T) {
		t.Parallel()
		for name, offset := range map[string]time.Duration{"minus-one": -time.Nanosecond, "exact": 0, "plus-one": time.Nanosecond} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				tp, w := ctlPlane(t, 1)
				st := tp.run(t, w, "a", name, "p2")
				// The last heartbeat was at t0: the lease ends at t0+15 s.
				deadline := tp.clk.Now().Add(leaseDuration)
				tp.clk.Advance(leaseDuration + offset)
				if offset < 0 {
					// Strictly before the deadline the heartbeat renews the
					// same generation; the sweep at this instant expires
					// nothing.
					tp.svc(t).reg.Expire()
					w.beat()
					tp.svc(t).reg.Expire()
					if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || v.Reconciling || tp.log.seenPrefix("loss-applied ") {
						t.Fatalf("renewed lease %+v", v)
					}
					if n := tp.nodePlane.show(t, idA); n.Liveness != contract.LivenessOnline {
						t.Fatalf("node %s one tick before expiry", n.Liveness)
					}
					return
				}
				// At or after the deadline a heartbeat cannot revive the
				// expired generation: the attachment ends and every
				// affected task is resolved lost, released once durable.
				w.send(contract.ProtocolVersion, contract.FrameHeartbeat, "b"+strconv.Itoa(w.b), w.body())
				w.closed()
				tp.log.awaitOnce(t, "lost-latched "+st.TaskID+" "+contract.ReasonLeaseExpired)
				tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
				v := tp.show(t, st.TaskID)
				if v.State != contract.TaskLost || v.Reason.Code != contract.ReasonLeaseExpired || v.StartedAt == nil || v.Result.ExitCode != nil ||
					tp.roleInflight(t, "a") != 0 {
					t.Fatalf("expired %+v", v)
				}
				if fin, _ := contract.ParseTime(*v.FinishedAt); fin.Before(deadline) {
					t.Fatalf("finished %v before the lease deadline %v", fin, deadline)
				}
				if n := tp.nodePlane.show(t, idA); n.Liveness != contract.LivenessOffline {
					t.Fatalf("node %s after expiry", n.Liveness)
				}
			})
		}
	})
	t.Run("states", func(t *testing.T) {
		t.Parallel()
		// At expiry: a running task and a pending one whose start was
		// written without a reply are lost; a start still queued is
		// rejected unsent (current-Run proof); a task whose natural result
		// is latched but whose commit is failing keeps that result (it is
		// retried, never replaced). No task moves to another role: role b
		// has free capacity and receives nothing.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 4), 1), record(taskCfg("b", "coder", idB, 4), 2)}, idA, idB)
		w := tp.worker(t, idA)
		wb := tp.worker(t, idB)
		blocked := tp.run(t, w, "a", "storage blocked", "p2")
		running := tp.run(t, w, "a", "running", "p3")
		ambiguous := tp.admit(t, taskReq(contract.TargetID, "a", "ambiguous"))
		w.start("p4")
		queued := tp.admit(t, taskReq(contract.TargetID, "a", "queued"))
		tp.log.awaitOnce(t, "start-queued "+idA+" "+queued.TaskID)
		tp.inj.set("rename", taskRel(blocked.TaskID))
		w.resultAck(w.sendResult(result(blocked, 0, 0, nil)), blocked.TaskID)
		tp.log.await(t, "publish-failed terminal "+blocked.TaskID)
		retry := tp.th.arm("terminal-commit-queued", blocked.TaskID)
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		tp.log.awaitOnce(t, "terminal-committed "+queued.TaskID)
		for i := 0; i < 3; i++ {
			tp.clk.Advance(heartbeatInterval)
			wb.beat()
		}
		for _, id := range []string{running.TaskID, ambiguous.TaskID} {
			tp.log.awaitOnce(t, "lost-latched "+id+" "+contract.ReasonLeaseExpired)
			tp.log.awaitOnce(t, "terminal-committed "+id)
		}
		if v := tp.show(t, ambiguous.TaskID); v.State != contract.TaskLost || v.StartedAt != nil || v.Role.ID != "a" {
			t.Fatalf("ambiguous %+v", v)
		}
		if v := tp.show(t, running.TaskID); v.State != contract.TaskLost || v.StartedAt == nil || v.Role.ID != "a" {
			t.Fatalf("running %+v", v)
		}
		if v := tp.show(t, queued.TaskID); v.State != contract.TaskRejected || v.Reason.Code != contract.ReasonStartNotSent {
			t.Fatalf("queued %+v", v)
		}
		if tp.log.seen("lost-latched " + blocked.TaskID + " " + contract.ReasonLeaseExpired) {
			t.Fatal("the lease replaced a latched natural result")
		}
		call := paused(t, retry, "terminal-commit-queued")
		tp.inj.set("", "")
		close(call.release)
		tp.log.awaitOnce(t, "terminal-committed "+blocked.TaskID)
		if v := tp.show(t, blocked.TaskID); v.State != contract.TaskSucceeded {
			t.Fatalf("latched natural result replaced: %+v", v)
		}
		if tp.roleInflight(t, "a") != 0 || tp.roleInflight(t, "b") != 0 || tp.log.seenPrefix("start-writing "+idB) {
			t.Fatal("a lost execution was rerouted or kept its slot")
		}
		tp.checkCounts(t)
	})
	t.Run("latched-failing", func(t *testing.T) {
		t.Parallel()
		// A natural result latched for commit whose writes keep failing
		// when the lease expires is retried, never replaced by lost.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "failing", "p2")
		tp.inj.set("rename", taskRel(st.TaskID))
		w.resultAck(w.sendResult(result(st, 4, 0, nil)), st.TaskID)
		tp.log.await(t, "publish-failed terminal "+st.TaskID)
		// The next retry is held before its write: no retry races the
		// test's clock afterwards.
		retry := tp.th.arm("terminal-commit-queued", st.TaskID)
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		tp.clk.Advance(leaseDuration)
		call := paused(t, retry, "terminal-commit-queued")
		tp.log.await(t, "loss-applied "+idA+" "+contract.ReasonLeaseExpired)
		tp.inj.set("", "")
		close(call.release)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskFailed || *v.Result.ExitCode != 4 || tp.log.seen("lost-latched "+st.TaskID+" "+contract.ReasonLeaseExpired) {
			t.Fatalf("latched result %+v", v)
		}
	})
	t.Run("reconnect-before", func(t *testing.T) {
		t.Parallel()
		// Reconnecting before expiry: the new attachment reconciles the
		// running execution (continue) and its own heartbeats govern the
		// lease; the old generation's deadline passes without a loss.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "before", "p2")
		started := tp.clk.Now()
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		tp.clk.Advance(leaseDuration - time.Second)
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionContinue}, entry(st, contract.PhaseRunning, &started, nil))
		tp.clk.Advance(2 * time.Second)
		w.beat()
		tp.svc(t).reg.Expire()
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || v.Reconciling || tp.log.seenPrefix("loss-applied ") {
			t.Fatalf("reconnected %+v", v)
		}
		tp.finish(t, w, st, 0, 0)
	})
	t.Run("reconnect-after", func(t *testing.T) {
		t.Parallel()
		// The lease expired (the loss eligible) before a new attachment
		// reports the execution: whichever applies the loss first, the
		// new attachment never erases it and is told to stop the
		// execution; the node stays unready until it reconciles.
		tp, w := ctlPlane(t, 2)
		st := tp.run(t, w, "a", "after", "p2")
		started := tp.clk.Now()
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		tp.clk.Advance(leaseDuration)
		p := tp.dial(t)
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		p.heartbeat(1)
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "before reconcile"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:role_unsynced")
		if got := p.reconcile(entry(st, contract.PhaseRunning, &started, nil)); got[st.TaskID] != contract.ActionStopLost {
			t.Fatalf("dispositions %v", got)
		}
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskLost || v.Reason.Code != contract.ReasonLeaseExpired {
			t.Fatalf("after %+v", v)
		}
	})
	t.Run("readd", func(t *testing.T) {
		t.Parallel()
		// A lost task's role is removed once the loss is durable and
		// re-added as a new instance: the returning worker is told to stop
		// the old execution, which keeps its old role snapshot; the new
		// instance inherits nothing.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "old instance", "p2")
		started := tp.clk.Now()
		tp.loseByLease(t, w, st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		if err := rmRole(tp.cl, bg, "a", false); err != nil {
			t.Fatalf("rm after the durable loss: %v", err)
		}
		p := tp.dial(t)
		p.ready = map[string]bool{}
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		if got := p.reconcile(entry(st, contract.PhaseRunning, &started, nil)); got[st.TaskID] != contract.ActionStopLost {
			t.Fatalf("dispositions %v", got)
		}
		p.ackReplace("p1")
		p.heartbeat(1)
		rv := tp.add(t, p, "p2", "p3", taskCfg("a", "coder", idA, 1))
		if rv.RegistrationOrder != 2 || rv.Inflight != 0 {
			t.Fatalf("re-added %+v", rv)
		}
		if v := tp.show(t, st.TaskID); v.Role.RegistrationOrder != 1 || v.State != contract.TaskLost {
			t.Fatalf("old task %+v", v)
		}
	})
}

// TestControlProtocol is UT FP-2 on the plane, delegated from
// tests/function (TestControlReconnect) with the contract's and the
// sidecar's: the attachment order (hello, inventory, reconcile, roles,
// readiness), heartbeats during paging, a role revision change mid
// inventory, bounded strict paging, identity and fencing, unknown
// executions, duplicate and conflicting results, lost acknowledgements
// and unsent starts. Do not rename or skip it.
func TestControlProtocol(t *testing.T) {
	t.Parallel()
	t.Run("order", func(t *testing.T) {
		t.Parallel()
		// Hello, then inventory pages with heartbeats answered between
		// them and no plane request; the reconcile pages after the final
		// page (heartbeats still answered while one is outstanding); the
		// role snapshot only after the final reconcile acknowledgement,
		// at the latest revision (a removal mid inventory coalesces).
		// Admission is gated until readiness follows.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1), record(taskCfg("b", "coder", idA, 1), 2)}, idA)
		p := tp.dial(t)
		p.ready = map[string]bool{}
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		p.heartbeat(1)
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "gated"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:role_unsynced")
		p.send(contract.ProtocolVersion, contract.FrameTaskInventory, "i1", contract.TaskInventoryBody{RunID: peerRunID})
		p.expect(contract.FrameTaskInventoryAck, "i1")
		if err := rmRole(tp.cl, bg, "b", false); err != nil {
			t.Fatalf("rm mid inventory: %v", err)
		}
		p.heartbeat(2)
		p.send(contract.ProtocolVersion, contract.FrameTaskInventory, "i2", contract.TaskInventoryBody{RunID: peerRunID, Page: 1, Final: true})
		p.expect(contract.FrameTaskInventoryAck, "i2")
		f := p.expect(contract.FrameTaskReconcile, "r1")
		if b, err := contract.DecodeTaskReconcile(f.Body); err != nil || !b.Final || len(b.Entries) != 0 {
			t.Fatalf("reconcile %s %v", f.Body, err)
		}
		p.heartbeat(3)
		p.send(contract.ProtocolVersion, contract.FrameTaskReconcileAck, "r1", contract.TaskReconcileAckBody{Received: true})
		tp.log.await(t, "reconciled "+idA)
		snap := p.readReplace("p1")
		if snap.Revision != 2 || len(snap.Roles) != 1 || snap.Roles[0].ID != "a" {
			t.Fatalf("first snapshot %+v", snap)
		}
		p.rev, p.roles = snap.Revision, snap.Roles
		p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p1", contract.RolesReplaceAckBody{Revision: snap.Revision})
		tp.log.await(t, "replace-acked "+idA+" p1 "+strconv.Itoa(snap.Revision))
		_, err = tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "unready"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:worker_unready")
		p.ready["a"] = true
		p.heartbeat(4)
		tp.admit(t, taskReq(contract.TargetID, "a", "ready"))
	})
	t.Run("flood", func(t *testing.T) {
		t.Parallel()
		// Many full pages of unknown executions: every page is answered,
		// heartbeats between them keep the lease, and more than 64
		// dispositions page into several reconcile pages.
		tp, _ := ctlPlane(t, 1)
		tp.workers[0].c.CloseNow()
		tp.log.await(t, "detached "+idA)
		p := tp.dial(t)
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		const pages = 3
		for pg := 0; pg < pages; pg++ {
			var es []contract.TaskInventoryEntry
			for i := 0; i < contract.MaxInventoryEntries; i++ {
				id := "t_" + strings.Repeat("0", 26) + strconv.Itoa(100000+pg*1000+i)
				es = append(es, contract.TaskInventoryEntry{TaskID: id, Execution: contract.ExecutionToken{Epoch: strings.Repeat("e", 32), Attachment: 1},
					StartDigest: strings.Repeat("d", 64), Phase: contract.PhasePreparing})
			}
			rid := "i" + strconv.Itoa(pg+1)
			p.send(contract.ProtocolVersion, contract.FrameTaskInventory, rid, contract.TaskInventoryBody{RunID: peerRunID, Page: pg, Final: pg == pages-1, Entries: es})
			p.expect(contract.FrameTaskInventoryAck, rid)
			// Strictly inside the first-heartbeat window and the lease;
			// after the final page the reconcile exchange's 4 s budget runs.
			if pg < pages-1 {
				tp.clk.Advance(heartbeatInterval - time.Second)
			}
			p.heartbeat(pg + 1)
		}
		got := 0
		for n := 1; ; n++ {
			rid := "r" + strconv.Itoa(n)
			f := p.expect(contract.FrameTaskReconcile, rid)
			b, err := contract.DecodeTaskReconcile(f.Body)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range b.Entries {
				if e.Action != contract.ActionStopLost {
					t.Fatalf("unknown execution %s: %s", e.TaskID, e.Action)
				}
			}
			got += len(b.Entries)
			p.heartbeat(pages + n)
			p.send(contract.ProtocolVersion, contract.FrameTaskReconcileAck, rid, contract.TaskReconcileAckBody{Received: true})
			if b.Final {
				break
			}
		}
		if got != pages*contract.MaxInventoryEntries {
			t.Fatalf("%d dispositions", got)
		}
		p.ready = map[string]bool{"a": true}
		p.ackReplace("p1")
		// The unknown executions' outcome is received, never committed
		// (the worker keeps its evidence), and the node stays unready on
		// this attachment although it reports its roles ready.
		unknown := contract.ExecutionToken{Epoch: strings.Repeat("e", 32), Attachment: 1}
		id := "t_" + strings.Repeat("0", 26) + "100000"
		res := contract.TaskResultBody{TaskID: id, Execution: unknown, Outcome: contract.OutcomeLost}.Sealed()
		hb := pages + got/contract.MaxReconcileEntries + 1
		p.heartbeat(hb)
		p.send(contract.ProtocolVersion, contract.FrameTaskResult, "b"+strconv.Itoa(hb+1), res)
		f := p.expect(contract.FrameTaskResultAck, "b"+strconv.Itoa(hb+1))
		if a, err := contract.DecodeTaskResultAck(f.Body); err != nil || a.Committed {
			t.Fatalf("unknown result ack %s %v", f.Body, err)
		}
		tp.log.await(t, "unknown-output "+idA+" "+id)
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "unknown held"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:worker_unready")
		// Unknown executions never create tasks.
		if list, _, err := tp.cl.ListTasks(bg, "", 100); err != nil || len(list) != 0 {
			t.Fatalf("tasks %d %v", len(list), err)
		}
	})
	t.Run("controls", func(t *testing.T) {
		t.Parallel()
		controlFairness(t)
	})
	t.Run("strict", func(t *testing.T) {
		t.Parallel()
		// Malformed identity, a changed start digest, out-of-order or
		// duplicate pages and entries, a changed Run ID and a page after
		// the final one each close the attachment.
		tp, w := ctlPlane(t, 2)
		st := tp.run(t, w, "a", "strict", "p2")
		started := tp.clk.Now()
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		good := entry(st, contract.PhaseRunning, &started, nil)
		wrongExec := good
		wrongExec.Execution.Attachment++
		wrongDigest := good
		wrongDigest.StartDigest = strings.Repeat("0", 64)
		for name, pages := range map[string][]struct {
			rid  string
			body contract.TaskInventoryBody
		}{
			"execution":     {{"i1", contract.TaskInventoryBody{RunID: peerRunID, Final: true, Entries: []contract.TaskInventoryEntry{wrongExec}}}},
			"start digest":  {{"i1", contract.TaskInventoryBody{RunID: peerRunID, Final: true, Entries: []contract.TaskInventoryEntry{wrongDigest}}}},
			"page order":    {{"i1", contract.TaskInventoryBody{RunID: peerRunID, Page: 1}}},
			"request id":    {{"i2", contract.TaskInventoryBody{RunID: peerRunID}}},
			"duplicate":     {{"i1", contract.TaskInventoryBody{RunID: peerRunID, Entries: []contract.TaskInventoryEntry{good}}}, {"i2", contract.TaskInventoryBody{RunID: peerRunID, Page: 1, Entries: []contract.TaskInventoryEntry{good}}}},
			"run id":        {{"i1", contract.TaskInventoryBody{RunID: peerRunID}}, {"i2", contract.TaskInventoryBody{RunID: strings.Repeat("9", 32), Page: 1}}},
			"after final":   {{"i1", contract.TaskInventoryBody{RunID: peerRunID, Final: true, Entries: []contract.TaskInventoryEntry{good}}}, {"i2", contract.TaskInventoryBody{RunID: peerRunID, Page: 1}}},
			"repeated page": {{"i1", contract.TaskInventoryBody{RunID: peerRunID}}, {"i1", contract.TaskInventoryBody{RunID: peerRunID}}},
		} {
			p := tp.dial(t)
			p.hello(idA)
			p.expect(contract.FrameHelloOK, "h1")
			for i, pg := range pages {
				p.send(contract.ProtocolVersion, contract.FrameTaskInventory, pg.rid, pg.body)
				if i < len(pages)-1 {
					p.expect(contract.FrameTaskInventoryAck, pg.rid)
					if pg.body.Final {
						p.expect(contract.FrameTaskReconcile, "r1")
					}
				}
			}
			for {
				f := p.recv()
				if f.Type == contract.FrameError {
					break
				}
			}
			if st := p.closed(); st != websocket.StatusPolicyViolation {
				t.Fatalf("%s: close %v", name, st)
			}
			tp.log.await(t, "detached "+idA)
		}
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || !v.Reconciling {
			t.Fatalf("a rejected inventory decided the task: %+v", v)
		}
	})
	t.Run("fencing", func(t *testing.T) {
		t.Parallel()
		// Output and results are accepted only from the node's current
		// attachment, and only after its reconciliation authorized the
		// execution: a valid stable token from an old attachment, or on
		// a new attachment before reconciliation, is refused.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "fenced", "p2")
		started := tp.clk.Now()
		ts := tp.svc(t)
		ts.mu.Lock()
		oldGen := ts.tasks[st.TaskID].gen
		ts.mu.Unlock()
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		// A new attachment, not yet reconciled.
		p := tp.dial(t)
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		if _, err := ts.receiveLog(idA, oldGen, contract.TaskLogBody{TaskID: st.TaskID, Execution: st.Execution, Data: []byte("old")}); err == nil {
			t.Fatal("an old attachment's output was accepted")
		}
		if _, err := ts.receiveResult(idA, oldGen, result(st, 0, 0, nil)); err == nil {
			t.Fatal("an old attachment's result was accepted")
		}
		p.send(contract.ProtocolVersion, contract.FrameTaskLog, "b1", contract.TaskLogBody{TaskID: st.TaskID, Execution: st.Execution, Data: []byte("early")})
		for {
			if f := p.recv(); f.Type == contract.FrameError {
				break
			}
		}
		p.closed()
		tp.log.await(t, "detached "+idA)
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionContinue}, entry(st, contract.PhaseRunning, &started, nil))
		if next := w.log(st, 0, []byte("after reconcile\n")); next != 16 {
			t.Fatalf("next %d", next)
		}
		tp.finish(t, w, st, 0, 16)
	})
	t.Run("results", func(t *testing.T) {
		t.Parallel()
		// An identical result is acknowledged again (committed once
		// durable); a changed digest for the same execution is a
		// conflict that never overwrites the original; a lost committed
		// acknowledgement is recovered by the next reconciliation
		// (forget).
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "results", "p2")
		started := tp.clk.Now()
		res := result(st, 0, 0, nil)
		w.resultAck(w.sendResult(res), st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		if !w.resultAck(w.sendResult(res), st.TaskID) {
			t.Fatal("the duplicate was not acknowledged committed")
		}
		rev := tp.taskFile(t, st.TaskID).Revision
		rid := w.sendResult(result(st, 1, 0, nil))
		expectClosed(t, w, rid)
		if r := tp.taskFile(t, st.TaskID); r.Revision != rev || *r.ResultDigest != res.Digest || r.Late != nil {
			t.Fatalf("a conflicting result changed the record: %+v", r)
		}
		tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionForget}, entry(st, contract.PhaseResult, &started, &res.Digest))
	})
	t.Run("unsent", func(t *testing.T) {
		t.Parallel()
		// A start queued behind an outstanding one whose deadline passes
		// before any write is rejected unsent and never written later.
		tp, w := ctlPlane(t, 2)
		tp.admit(t, taskReq(contract.TargetID, "a", "outstanding"))
		w.start("p2")
		queued := tp.admit(t, taskReq(contract.TargetID, "a", "queued"))
		tp.log.awaitOnce(t, "start-queued "+idA+" "+queued.TaskID)
		for i := 0; i < 2; i++ {
			tp.clk.Advance(heartbeatInterval)
			w.beat()
		}
		tp.clk.Advance(startControl - 2*heartbeatInterval)
		tp.log.awaitOnce(t, "terminal-committed "+queued.TaskID)
		if v := tp.show(t, queued.TaskID); v.State != contract.TaskRejected || v.Reason.Code != contract.ReasonStartNotSent {
			t.Fatalf("queued %+v", v)
		}
		if tp.log.seen("start-writing " + idA + " " + queued.TaskID) {
			t.Fatal("the expired start was written")
		}
	})
}
