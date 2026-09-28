package plane

import (
	"bytes"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06a plane families (TestControlCommit here; the others in
// control_*_test.go). Every product timer runs on the fake node clock;
// the tests synchronize on named plane events and paused stages, never on
// sleeps, and call the sweep's duties directly where a boundary must not
// fire (a tick's absence is not observable).

// ctlPlane serves a plane with role a (concurrency conc) on idA and a
// connected worker.
func ctlPlane(t *testing.T, conc int) (*taskPlane, *worker) {
	t.Helper()
	tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, conc), 1)}, idA)
	return tp, tp.worker(t, idA)
}

// checkCounts requires the derived reservation counts to equal those of
// the entries, and nothing negative.
func (tp *taskPlane) checkCounts(t *testing.T) {
	t.Helper()
	ts := tp.svc(t)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	want := map[instanceKey]heldCount{}
	for _, e := range ts.tasks {
		if e.held() {
			c := want[e.key]
			c.held++
			if e.isReconciling() {
				c.reconciling++
			}
			want[e.key] = c
		}
	}
	for k, c := range ts.counts {
		if c.held < 0 || c.reconciling < 0 || *c != want[k] {
			t.Fatalf("counts of %v = %+v, want %+v", k, *c, want[k])
		}
	}
	for k, c := range want {
		if ts.heldLocked(k) != c {
			t.Fatalf("counts of %v missing (want %+v)", k, c)
		}
	}
}

// entryOf returns task id's entry (callers lock ts.mu for its fields).
func (tp *taskPlane) entryOf(t *testing.T, id string) (*taskService, *taskEntry) {
	t.Helper()
	ts := tp.svc(t)
	ts.mu.Lock()
	e := ts.tasks[id]
	ts.mu.Unlock()
	if e == nil {
		t.Fatalf("no task %s", id)
	}
	return ts, e
}

// loseByLease ends w's attachment and lets the node's lease expire; the
// affected task id is latched lost.
func (tp *taskPlane) loseByLease(t *testing.T, w *worker, id string) {
	t.Helper()
	w.c.CloseNow()
	tp.log.await(t, "detached "+w.node)
	tp.clk.Advance(leaseDuration)
	tp.log.awaitOnce(t, "lost-latched "+id+" "+contract.ReasonLeaseExpired)
}

// restartAfter is restart with f run on the stopped plane's root.
func (tp *taskPlane) restartAfter(t *testing.T, f func()) {
	t.Helper()
	for _, w := range tp.workers {
		w.c.CloseNow()
	}
	tp.workers = nil
	tp.served.stop(t)
	f()
	d := fast(testDeps(t))
	d.streamEvents = tp.log.add
	d.roleHook = tp.hooks.fn
	d.taskHook = tp.th.fn
	d.fail = tp.inj.fail
	d.onTasks = func(ts *taskService) { tp.ts <- ts }
	tp.nodePlane = serveNodePlaneAt(t, d, tp.root)
}

// writeTaskFile replaces task rec's document on a stopped plane.
func writeTaskFile(t *testing.T, root string, rec contract.TaskRecord) {
	t.Helper()
	b, err := contract.EncodeTaskRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout{root: root}.path(taskRel(rec.TaskID)), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestControlCommit is UT FP-1 on the plane, delegated from
// tests/function (TestControlDurability): every write, sync, close and
// rename fault of a natural or lost terminal commit, the visible but
// unconfirmed latch and its resync, both durable orders of a natural
// result and a lost decision, restarts during a failing or unconfirmed
// commit, exactly-once release, reader atomicity at confirmation, the
// 30-second watchdog boundary, revision exhaustion and bounded buffers.
// Do not rename or skip it.
func TestControlCommit(t *testing.T) {
	t.Parallel()
	t.Run("faults", func(t *testing.T) {
		t.Parallel()
		for _, kind := range []string{"terminal", "lost"} {
			for _, op := range []string{"create", "write", "sync", "close", "rename", "dirsync"} {
				t.Run(kind+"-"+op, func(t *testing.T) { t.Parallel(); commitFault(t, kind, op) })
			}
		}
	})
	t.Run("order", func(t *testing.T) {
		t.Parallel()
		t.Run("natural-first", func(t *testing.T) {
			t.Parallel()
			// A captured natural result owns the commit: the lease expiring
			// while its commit is held never replaces it with lost.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "natural first", "p2")
			hold := tp.th.arm("terminal-commit-queued", st.TaskID)
			w.resultAck(w.sendResult(result(st, 0, 0, nil)), st.TaskID)
			call := paused(t, hold, "terminal-commit-queued")
			w.c.CloseNow()
			tp.log.await(t, "detached "+idA)
			tp.clk.Advance(leaseDuration)
			tp.log.await(t, "loss-applied "+idA+" "+contract.ReasonLeaseExpired)
			if tp.log.seen("lost-latched " + st.TaskID + " " + contract.ReasonLeaseExpired) {
				t.Fatal("the lease replaced a latched natural result")
			}
			close(call.release)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskSucceeded || v.Reason != nil || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("natural first %+v", v)
			}
			tp.checkCounts(t)
		})
		t.Run("lost-first", func(t *testing.T) {
			t.Parallel()
			// A durable lost decision governs: the worker's later natural
			// result is recorded only as late evidence, and the released
			// reservation is never reacquired.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "lost first", "p2")
			started := tp.clk.Now()
			tp.loseByLease(t, w, st.TaskID)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			before := tp.show(t, st.TaskID)
			res := result(st, 0, 0, nil)
			w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &res.Digest))
			w.resultAck(w.sendResult(res), st.TaskID)
			tp.log.awaitOnce(t, "late-committed "+st.TaskID)
			v := tp.show(t, st.TaskID)
			if v.State != contract.TaskLost || *v.FinishedAt != *before.FinishedAt || v.LateResult == nil || v.LateResult.Outcome != contract.OutcomeNatural ||
				tp.roleInflight(t, "a") != 0 {
				t.Fatalf("lost first %+v", v)
			}
			tp.checkCounts(t)
			// Exactly-once release: the instance's one slot is free once.
			tp.admit(t, taskReq(contract.TargetID, "a", "next"))
			_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "full"))
			wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:full")
			tp.checkCounts(t)
		})
	})
	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		t.Run("before-rename", func(t *testing.T) {
			t.Parallel()
			// The terminal commit fails before its rename and the plane
			// restarts: the running record is the loaded authority,
			// reconciling; the worker's outbox supplies the result again.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "restart", "p2")
			started := tp.clk.Now()
			tp.inj.set("rename", taskRel(st.TaskID))
			res := result(st, 0, 0, nil)
			w.resultAck(w.sendResult(res), st.TaskID)
			tp.log.await(t, "publish-failed terminal "+st.TaskID)
			tp.inj.set("", "")
			tp.restart(t)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || !v.Reconciling {
				t.Fatalf("after restart %+v", v)
			}
			w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &res.Digest))
			w.resultAck(w.sendResult(res), st.TaskID)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			if !w.resultAck(w.sendResult(res), st.TaskID) {
				t.Fatal("the durable result was not acknowledged committed")
			}
			if r := tp.taskFile(t, st.TaskID); r.State != contract.TaskSucceeded || *r.ResultDigest != res.Digest {
				t.Fatalf("durable %+v", r)
			}
		})
		t.Run("visible-unconfirmed", func(t *testing.T) {
			t.Parallel()
			// The terminal record is visible but its directory sync failed
			// when the plane restarts: the complete surviving file is the
			// recovered authority (confirmed, nothing held) and the
			// worker's report of the same digest is forgotten.
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "visible", "p2")
			started := tp.clk.Now()
			tp.inj.set("dirsync", tasksName)
			res := result(st, 3, 0, nil)
			w.resultAck(w.sendResult(res), st.TaskID)
			tp.log.awaitOnce(t, "published terminal "+st.TaskID)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskFailed || v.DurabilityConfirmed || tp.roleInflight(t, "a") != 1 {
				t.Fatalf("visible unconfirmed %+v", v)
			}
			tp.inj.set("", "")
			tp.restart(t)
			if v := tp.show(t, st.TaskID); v.State != contract.TaskFailed || !v.DurabilityConfirmed || v.Reconciling || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("loaded terminal %+v", v)
			}
			tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionForget}, entry(st, contract.PhaseResult, &started, &res.Digest))
			tp.checkCounts(t)
		})
		t.Run("revision-exhausted", func(t *testing.T) {
			t.Parallel()
			// A task whose revision counter is exhausted keeps its safe
			// state; its pending result surfaces the storage condition and
			// admissions stay blocked.
			tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
			w := tp.worker(t, idA)
			st := tp.run(t, w, "a", "exhausted", "p2")
			started := tp.clk.Now()
			rec := tp.taskFile(t, st.TaskID)
			tp.restartAfter(t, func() {
				rec.Revision = contract.MaxSafeInteger
				writeTaskFile(t, tp.root, rec)
			})
			res := result(st, 0, 0, nil)
			w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &res.Digest))
			if w.resultAck(w.sendResult(res), st.TaskID) {
				t.Fatal("an exhausted task's result was acknowledged committed")
			}
			ts, e := tp.entryOf(t, st.TaskID)
			deadline := time.After(testWait)
			for {
				ts.mu.Lock()
				fault := e.fault
				ts.mu.Unlock()
				if fault {
					break
				}
				select {
				case <-deadline:
					t.Fatal("the exhausted revision never surfaced")
				case <-time.After(time.Millisecond):
				}
			}
			v := tp.show(t, st.TaskID)
			if v.State != contract.TaskRunning || v.PersistenceReason == nil || *v.PersistenceReason != contract.ReasonResultStorageUnconfirmed {
				t.Fatalf("exhausted %+v", v)
			}
			_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "blocked"))
			wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:storage_unconfirmed")
		})
	})
	t.Run("reader-atomic", func(t *testing.T) {
		t.Parallel()
		// Readers observe terminal confirmation and the reservation's
		// release in one critical section: at every observation either the
		// task holds its slot or it is terminal and confirmed.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "atomic", "p2")
		ts, e := tp.entryOf(t, st.TaskID)
		stop := make(chan struct{})
		var bad []string
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ts.mu.Lock()
				held := ts.heldLocked(e.key).held
				term, conf := e.terminal(), e.confirmed
				ts.mu.Unlock()
				if (held == 0) != (term && conf) {
					bad = append(bad, "held="+itoa(held))
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
		w.resultAck(w.sendResult(result(st, 0, 0, nil)), st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		close(stop)
		wg.Wait()
		if len(bad) > 0 {
			t.Fatalf("readers observed %v", bad)
		}
	})
	t.Run("watchdog", func(t *testing.T) {
		t.Parallel()
		// The 30 s commit watchdog: at 30 s minus one tick nothing is
		// visible; at exactly 30 s the storage condition is, admissions
		// are blocked, and the late commit clears it.
		tp, w := ctlPlane(t, 2)
		st := tp.run(t, w, "a", "slow", "p2")
		hold := tp.th.arm("terminal-commit-queued", st.TaskID)
		w.resultAck(w.sendResult(result(st, 0, 0, nil)), st.TaskID)
		call := paused(t, hold, "terminal-commit-queued")
		_, e := tp.entryOf(t, st.TaskID)
		ts := tp.svc(t)
		ts.mu.Lock()
		since := e.cand.at
		ts.mu.Unlock()
		for tp.clk.Now().Add(5 * time.Second).Before(since.Add(commitWatchdog)) {
			tp.clk.Advance(5 * time.Second)
			w.beat()
		}
		tp.clk.Advance(since.Add(commitWatchdog).Sub(tp.clk.Now()) - time.Nanosecond)
		ts.tick()
		if v := tp.show(t, st.TaskID); v.PersistenceReason != nil || tp.log.seen("commit-watchdog "+st.TaskID) {
			t.Fatalf("watchdog one tick early: %+v", v)
		}
		tp.clk.Advance(time.Nanosecond)
		ts.tick()
		tp.log.awaitOnce(t, "commit-watchdog "+st.TaskID)
		w.beat()
		if v := tp.show(t, st.TaskID); v.PersistenceReason == nil || *v.PersistenceReason != contract.ReasonResultStorageUnconfirmed || !v.CompletionPending {
			t.Fatalf("watchdog view %+v", v)
		}
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "blocked"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:storage_unconfirmed")
		close(call.release)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.PersistenceReason != nil || v.State != contract.TaskSucceeded {
			t.Fatalf("after the late commit %+v", v)
		}
		tp.admit(t, taskReq(contract.TargetID, "a", "unblocked"))
	})
	t.Run("bounded", func(t *testing.T) {
		t.Parallel()
		// Terminal confirmation drops the live ring, the latched candidate
		// and the frozen tail from memory; reads use the document.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "bounded", "p2")
		w.log(st, 0, bytes.Repeat([]byte("x"), 100))
		w.resultAck(w.sendResult(result(st, 0, 100, nil)), st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		ts, e := tp.entryOf(t, st.TaskID)
		ts.mu.Lock()
		ring, cand, data, late := e.ring, e.cand, e.rec.Log.Data, e.lateRing
		ts.mu.Unlock()
		if ring != nil || cand != nil || data != nil || late != nil {
			t.Fatal("a confirmed terminal task kept buffers")
		}
		if v := tp.show(t, st.TaskID); v.LogTail != string(bytes.Repeat([]byte("x"), 100)) {
			t.Fatalf("tail %q", v.LogTail)
		}
		tp.checkCounts(t)
	})
}

// commitFault fails op at every attempt of a natural (terminal) or lost
// commit until cleared: before visibility the old record stays
// authoritative with the slot held and admissions blocked, and the same
// candidate is retried every second; a failed directory sync latches the
// visible record unconfirmed and resyncs it. Clearing the fault commits
// and releases exactly once.
func commitFault(t *testing.T, kind, op string) {
	tp, w := ctlPlane(t, 2)
	st := tp.run(t, w, "a", kind+" "+op, "p2")
	prefix := tasksName + "/"
	if op == "dirsync" {
		prefix = tasksName
	}
	tp.inj.set(op, prefix)
	res := result(st, 0, 0, nil)
	if kind == "terminal" {
		w.resultAck(w.sendResult(res), st.TaskID)
	} else {
		tp.loseByLease(t, w, st.TaskID)
	}
	final := contract.TaskSucceeded
	if kind == "lost" {
		final = contract.TaskLost
	}
	blocked := func() {
		t.Helper()
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "blocked"))
		if contract.CodeOf(err) != contract.CodeUnavailable || tp.roleInflight(t, "a") != 1 {
			t.Fatalf("%s %s: admission %v, inflight %d", kind, op, err, tp.roleInflight(t, "a"))
		}
		if kind == "terminal" {
			wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:storage_unconfirmed")
		}
	}
	if op == "dirsync" {
		tp.log.awaitOnce(t, "published "+kind+" "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.State != final || v.DurabilityConfirmed {
			t.Fatalf("visible unconfirmed %+v", v)
		}
		blocked()
		hits := tp.inj.hits()
		from := tp.log.mark()
		tp.clk.Advance(storageRetry)
		tp.log.awaitFrom(t, from, "resync-failed")
		if tp.inj.hits() <= hits {
			t.Fatal("the directory sync was not retried")
		}
	} else {
		tp.log.await(t, "publish-failed "+kind+" "+st.TaskID)
		v := tp.show(t, st.TaskID)
		if v.State != contract.TaskRunning || !v.CompletionPending || v.PersistenceReason == nil || *v.PersistenceReason != contract.ReasonResultStorageUnconfirmed {
			t.Fatalf("failed commit %+v", v)
		}
		if r := tp.taskFile(t, st.TaskID); r.State != contract.TaskRunning {
			t.Fatal("the old record was replaced")
		}
		if left := tempLeftovers(t, tp.root); len(left) != 0 {
			t.Fatalf("a failed commit left %v", left)
		}
		blocked()
		from := tp.log.mark()
		tp.clk.Advance(storageRetry)
		tp.log.awaitFrom(t, from, "publish-failed "+kind+" "+st.TaskID)
	}
	tp.inj.set("", "")
	tp.clk.Advance(storageRetry)
	tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
	v := tp.show(t, st.TaskID)
	if v.State != final || !v.DurabilityConfirmed || v.PersistenceReason != nil || v.CompletionPending || tp.roleInflight(t, "a") != 0 {
		t.Fatalf("committed %+v (inflight %d)", v, tp.roleInflight(t, "a"))
	}
	r := tp.taskFile(t, st.TaskID)
	if r.State != final || (kind == "terminal") != (r.ResultDigest != nil) {
		t.Fatalf("durable %+v", r)
	}
	tp.checkCounts(t)
}
