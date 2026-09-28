package plane

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestControlPlaneRestart is UT FP-6 on the plane, delegated from
// tests/function (TestControlPlaneRecovery) with the sidecar's: running
// work survives a plane restart unchanged, a completion during downtime
// is committed, a missing or partial inventory, the D2 capped-reconnect
// timeline inside the 60 s startup grace, the grace at 60 s minus one
// tick, exactly and plus one tick for loaded pending and running tasks,
// and no start replay. Do not rename or skip it.
func TestControlPlaneRestart(t *testing.T) {
	t.Parallel()
	t.Run("running", func(t *testing.T) {
		t.Parallel()
		// The same execution continues: its reservation is rebuilt, its
		// output resumes at the acknowledged offset and its result is the
		// task's; nothing is started again.
		tp, w := ctlPlane(t, 2)
		st := tp.run(t, w, "a", "survivor", "p2")
		started := tp.clk.Now()
		w.log(st, 0, []byte("before\n"))
		tp.clk.Advance(checkpointEvery)
		tp.log.await(t, "published checkpoint "+st.TaskID)
		w.beat()
		from := tp.log.mark()
		tp.restart(t)
		tp.log.awaitFrom(t, from, "startup-grace-armed")
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || !v.Reconciling || tp.roleInflight(t, "a") != 1 {
			t.Fatalf("restored %+v", v)
		}
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionContinue}, entry(st, contract.PhaseRunning, &started, nil))
		if next := w.log(st, 7, []byte("after\n")); next != 13 {
			t.Fatalf("resumed at %d", next)
		}
		tp.finish(t, w, st, 0, 13)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskSucceeded || v.LogTail != "before\nafter\n" || v.Log.Incomplete {
			t.Fatalf("result %+v", v)
		}
		for _, e := range tp.log.all()[from:] {
			if strings.HasPrefix(e, "start-writing ") {
				t.Fatalf("a start was replayed: %s", e)
			}
		}
	})
	t.Run("completed-during-downtime", func(t *testing.T) {
		t.Parallel()
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "downtime", "p2")
		started := tp.clk.Now()
		tp.restart(t)
		res := result(st, 5, 4, nil)
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &res.Digest))
		w.logAck(w.sendLateLog(st, 0, []byte("out\n"), res.Digest))
		w.resultAck(w.sendResult(res), st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskFailed || *v.Result.ExitCode != 5 || v.LogTail != "out\n" || v.LateResult != nil {
			t.Fatalf("downtime completion %+v", v)
		}
	})
	t.Run("recovered-lost-after-output", func(t *testing.T) {
		t.Parallel()
		// The worker crashed after the plane received output: its
		// recovered lost outcome reports what its journal knew (nothing),
		// less than was received. It still resolves the execution lost,
		// keeping the received output (incomplete), and replayed chunks
		// overlapping what was received are accepted, never a protocol
		// error.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "crash after output", "p2")
		started := tp.clk.Now()
		out := []byte("seventeen bytes!\n")
		w.log(st, 0, out)
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		res := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeLost, OutputBytes: 0, LogIncomplete: true}.Sealed()
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseLost, &started, &res.Digest))
		w.resultAck(w.sendResult(res), st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		v := tp.show(t, st.TaskID)
		if v.State != contract.TaskLost || v.Reason.Code != contract.ReasonWorkerLost || v.LogTail != string(out) || !v.Log.Incomplete ||
			v.Log.SourceBytes != len(out) || v.LateResult != nil {
			t.Fatalf("recovered lost %+v", v)
		}
		// A replayed tail straddling what was received: accepted.
		st2 := tp.run(t, w, "a", "straddle", "p2")
		started2 := tp.clk.Now()
		w.log(st2, 0, []byte("abcdef"))
		w.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		res2 := contract.TaskResultBody{TaskID: st2.TaskID, Execution: st2.Execution, Outcome: contract.OutcomeLost, OutputBytes: 10, LogIncomplete: true}.Sealed()
		w = tp.workerInv(t, idA, map[string]string{st2.TaskID: contract.ActionSendResult}, entry(st2, contract.PhaseLost, &started2, &res2.Digest))
		if next := w.logAck(w.sendLateLog(st2, 2, []byte("cdefghij"), res2.Digest)); next != 10 {
			t.Fatalf("straddling chunk acknowledged through %d", next)
		}
		w.resultAck(w.sendResult(res2), st2.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st2.TaskID)
		if v := tp.show(t, st2.TaskID); v.LogTail != "abcdefghij" || v.State != contract.TaskLost {
			t.Fatalf("straddled %+v", v)
		}
	})
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		// A complete inventory without the loaded task resolves it lost
		// (execution_missing), never restarted.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "missing", "p2")
		tp.restart(t)
		tp.workerInv(t, idA, map[string]string{})
		tp.log.awaitOnce(t, "lost-latched "+st.TaskID+" "+contract.ReasonExecutionMissing)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskLost || v.StartedAt == nil || tp.roleInflight(t, "a") != 0 {
			t.Fatalf("missing %+v", v)
		}
	})
	t.Run("partial", func(t *testing.T) {
		t.Parallel()
		// Interrupted paging proves nothing: the task stays reconciling
		// until a complete inventory reports it.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "partial", "p2")
		started := tp.clk.Now()
		tp.restart(t)
		p := tp.dial(t)
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		p.send(contract.ProtocolVersion, contract.FrameTaskInventory, "i1", contract.TaskInventoryBody{RunID: peerRunID})
		p.expect(contract.FrameTaskInventoryAck, "i1")
		p.c.CloseNow()
		tp.log.await(t, "detached "+idA)
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || !v.Reconciling || tp.log.seenPrefix("lost-latched "+st.TaskID) {
			t.Fatalf("after a partial inventory %+v", v)
		}
		tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionContinue}, entry(st, contract.PhaseRunning, &started, nil))
	})
	t.Run("timeline", func(t *testing.T) {
		t.Parallel()
		// D2: the returning worker's capped reconnect (30 s backoff, the
		// 10 s dial and both 5 s hello phases each one tick short)
		// attaches at 50 s minus three ticks and heartbeats before its
		// first-heartbeat window ends: the attachment wins the grace and
		// its lease governs past 60 s; the same execution continues with
		// no lost decision.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "timeline", "p2")
		started := tp.clk.Now()
		from := tp.log.mark()
		tp.restart(t)
		tp.log.awaitFrom(t, from, "startup-grace-armed")
		armed := tp.clk.Now()
		tp.clk.Advance(30*time.Second + (10*time.Second - time.Nanosecond) + (5*time.Second - time.Nanosecond) + (5*time.Second - time.Nanosecond))
		if got := tp.clk.Now().Sub(armed); got != 50*time.Second-3*time.Nanosecond {
			t.Fatalf("attach at %v", got)
		}
		p := tp.dial(t)
		p.ready = map[string]bool{}
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		tp.log.awaitFrom(t, from, "grace-attached "+idA)
		if got := p.reconcile(entry(st, contract.PhaseRunning, &started, nil)); got[st.TaskID] != contract.ActionContinue {
			t.Fatalf("dispositions %v", got)
		}
		b := p.ackReplace("p1")
		for _, r := range b.Roles {
			p.ready[r.ID] = true
		}
		// The plane processed the acknowledgement before time moves.
		tp.log.awaitFrom(t, from, "replace-acked "+idA+" p1 "+strconv.Itoa(b.Revision))
		tp.clk.Advance(5*time.Second - time.Nanosecond)
		p.heartbeat(1)
		tp.clk.Advance(armed.Add(startupGrace + time.Nanosecond).Sub(tp.clk.Now()))
		tp.svc(t).reg.Expire()
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || v.Reconciling || tp.log.seenPrefix("loss-applied "+idA) {
			t.Fatalf("past the grace %+v", v)
		}
		w = &worker{peer: p, b: 2, ev: tp.log, node: idA, marks: map[string]int{}}
		tp.workers = append(tp.workers, w)
		tp.finish(t, w, st, 0, 0)
	})
	t.Run("grace", func(t *testing.T) {
		t.Parallel()
		for _, state := range []string{contract.TaskPending, contract.TaskRunning} {
			for name, offset := range map[string]time.Duration{"minus-one": -time.Nanosecond, "exact": 0, "plus-one": time.Nanosecond} {
				t.Run(state+"-"+name, func(t *testing.T) { t.Parallel(); graceBoundary(t, state, offset) })
			}
		}
	})
}

// graceBoundary attaches a returning worker holding one loaded task
// (pending with its start written, or running) at the startup grace's
// deadline plus offset: strictly before it continues; at or after the
// deadline the task is lost (startup_grace_expired) and the worker is
// told to stop it.
func graceBoundary(t *testing.T, state string, offset time.Duration) {
	tp, w := ctlPlane(t, 2)
	var st contract.TaskStartBody
	if state == contract.TaskRunning {
		st = tp.run(t, w, "a", "loaded running", "p2")
	} else {
		tp.admit(t, taskReq(contract.TargetID, "a", "loaded pending"))
		st = w.start("p2")
	}
	started := tp.clk.Now()
	from := tp.log.mark()
	tp.restart(t)
	tp.log.awaitFrom(t, from, "startup-grace-armed")
	if v := tp.show(t, st.TaskID); v.State != state || !v.Reconciling {
		t.Fatalf("loaded %+v", v)
	}
	tp.clk.Advance(startupGrace + offset)
	p := tp.dial(t)
	p.ready = map[string]bool{}
	p.hello(idA)
	p.expect(contract.FrameHelloOK, "h1")
	got := p.reconcile(entry(st, contract.PhaseRunning, &started, nil))
	if offset < 0 {
		tp.log.awaitFrom(t, from, "grace-attached "+idA)
		if got[st.TaskID] != contract.ActionContinue {
			t.Fatalf("dispositions %v", got)
		}
		if state == contract.TaskPending {
			// The worker's launch fact publishes running.
			tp.log.awaitOnce(t, "published running "+st.TaskID)
		}
		b := p.ackReplace("p1")
		for _, r := range b.Roles {
			p.ready[r.ID] = true
		}
		p.heartbeat(1)
		tp.svc(t).reg.Expire()
		if v := tp.show(t, st.TaskID); v.State != contract.TaskRunning || tp.log.seenPrefix("loss-applied "+idA) {
			t.Fatalf("attached before the grace %+v", v)
		}
		return
	}
	tp.log.awaitFrom(t, from, "loss-applied "+idA+" "+contract.ReasonStartupGraceExpired)
	tp.log.awaitOnce(t, "lost-latched "+st.TaskID+" "+contract.ReasonStartupGraceExpired)
	if got[st.TaskID] != contract.ActionStopLost || tp.log.seen("grace-attached "+idA) {
		t.Fatalf("dispositions %v", got)
	}
	tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
	v := tp.show(t, st.TaskID)
	if v.State != contract.TaskLost || v.Reason.Code != contract.ReasonStartupGraceExpired || (state == contract.TaskPending) != (v.StartedAt == nil) {
		t.Fatalf("expired %+v", v)
	}
}

// TestControlLate is UT FP-7 on the plane, delegated from tests/function
// (TestControlLateResult) with the sidecar's: lost then a success, a
// failure or a lost outcome; an exact duplicate after acknowledgement
// loss; a changed result's conflict; the combined tail budget (frozen
// primary, newest late suffix, zero capacity); a crash during the late
// append; unknown task, node or token rejection. Do not rename or skip
// it.
func TestControlLate(t *testing.T) {
	t.Parallel()
	for name, res := range map[string]func(contract.TaskStartBody) contract.TaskResultBody{
		"success": func(st contract.TaskStartBody) contract.TaskResultBody { return result(st, 0, 13, nil) },
		"failure": func(st contract.TaskStartBody) contract.TaskResultBody { return result(st, 3, 13, nil) },
		"lost":    func(st contract.TaskStartBody) contract.TaskResultBody { return lostResult(st, 13) },
	} {
		t.Run("after-lost-"+name, func(t *testing.T) {
			t.Parallel()
			tp, w := ctlPlane(t, 1)
			st := tp.run(t, w, "a", "late "+name, "p2")
			started := tp.clk.Now()
			w.log(st, 0, []byte("primary\n"))
			tp.loseByLease(t, w, st.TaskID)
			tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
			before := tp.show(t, st.TaskID)
			rev := tp.taskFile(t, st.TaskID).Revision
			primary, _ := tp.cl.TaskLogs(bg, st.TaskID)
			if _, err := tp.cl.TaskLateLogs(bg, st.TaskID); contract.CodeOf(err) != contract.CodeNotFound || reasonOf(err) != contract.ReasonNoLateResult {
				t.Fatalf("late logs before any: %v", err)
			}
			r := res(st)
			w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &r.Digest))
			w.logAck(w.sendLateLog(st, 8, []byte("late\n"), r.Digest))
			if w.resultAck(w.sendResult(r), st.TaskID) {
				t.Fatal("late evidence acknowledged committed before its write")
			}
			tp.log.awaitOnce(t, "late-committed "+st.TaskID)
			if !w.resultAck(w.sendResult(r), st.TaskID) {
				t.Fatal("durable late evidence not acknowledged committed")
			}
			v := tp.show(t, st.TaskID)
			if v.State != contract.TaskLost || *v.FinishedAt != *before.FinishedAt || v.Reason.Code != before.Reason.Code || v.Result.ExitCode != nil ||
				v.LogTail != before.LogTail || tp.roleInflight(t, "a") != 0 {
				t.Fatalf("the late result changed the task: %+v", v)
			}
			l := v.LateResult
			if l == nil || l.Digest != r.Digest || l.Outcome != r.Outcome || l.OutputBytes != 13 || l.Log.RetainedBytes != 5 || !l.Log.Incomplete {
				t.Fatalf("late summary %+v", l)
			}
			if (name == "lost") != (l.ExitCode == nil) {
				t.Fatalf("late exit %+v", l)
			}
			if lg, err := tp.cl.TaskLateLogs(bg, st.TaskID); err != nil || string(lg.Data) != "late\n" {
				t.Fatalf("late logs %q %v", lg.Data, err)
			}
			if after, _ := tp.cl.TaskLogs(bg, st.TaskID); !bytes.Equal(after.Data, primary.Data) || after.SourceBytes != primary.SourceBytes {
				t.Fatal("ordinary logs changed")
			}
			rec := tp.taskFile(t, st.TaskID)
			if rec.Revision != rev+1 || rec.Late == nil || rec.State != contract.TaskLost {
				t.Fatalf("late record revision %d (was %d)", rec.Revision, rev)
			}
			tp.checkCounts(t)
		})
	}
	t.Run("conflict", func(t *testing.T) {
		t.Parallel()
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "conflict", "p2")
		started := tp.clk.Now()
		tp.loseByLease(t, w, st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		r := result(st, 0, 0, nil)
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &r.Digest))
		w.resultAck(w.sendResult(r), st.TaskID)
		tp.log.awaitOnce(t, "late-committed "+st.TaskID)
		rev := tp.taskFile(t, st.TaskID).Revision
		expectClosed(t, w, w.sendResult(result(st, 1, 0, nil)))
		if rec := tp.taskFile(t, st.TaskID); rec.Revision != rev || rec.Late.Digest != r.Digest {
			t.Fatal("a conflicting result replaced the late evidence")
		}
		// The reconnecting worker's committed digest is forgotten.
		tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionForget}, entry(st, contract.PhaseResult, &started, &r.Digest))
	})
	t.Run("budget", func(t *testing.T) {
		t.Parallel()
		// The combined decoded tails stay within the budget (10 MiB; a
		// 16-byte budget here, the arithmetic being the same): the primary
		// tail (read from the document) is frozen, the newest late suffix
		// fits the rest and is truncated, and a full primary tail leaves
		// zero late capacity. The writer's composition is driven directly
		// on a store; the wire path of late evidence is after-lost-*, the
		// real 10 MiB bound BenchmarkControlCommit's late-10485760 and the
		// contract's combined-tail validation (10 MiB documents here would
		// dominate the race-enabled stress shard).
		const budget = 16
		for name, c := range map[string]struct {
			primary int
			want    string
		}{"near": {budget - 5, "56789"}, "zero": {budget, ""}, "roomy": {0, "123456789"}} {
			root := filepath.Join(t.TempDir(), "state")
			os.MkdirAll(root, 0o700)
			st := &taskStore{l: layout{root: root}, d: fast(testDeps(t))}
			ts := &taskService{st: st, clock: realClock{}, logger: slog.New(slog.DiscardHandler), lookup: roleLookup, tasks: map[string]*taskEntry{},
				counts: map[instanceKey]*heldCount{}, blocked: map[*taskEntry]bool{}, stop: make(chan struct{}), logCap: budget}
			start, fin := t0.Add(time.Second), t0.Add(time.Minute)
			rec := contract.TaskRecord{TaskID: "t_00000000000000000000000000000001", Request: taskReq(contract.TargetID, "a", "budget"),
				Role: record(taskCfg("a", "coder", idA, 1), 1), Effective: contract.TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour},
				Execution: contract.ExecutionToken{Epoch: strings.Repeat("0", 32), Attachment: 1}, State: contract.TaskLost, CreatedAt: t0, StartedAt: &start,
				FinishedAt: &fin, Reason: &contract.TaskReason{Code: contract.ReasonLeaseExpired, Message: "expired"}, Revision: 2,
				TimeoutPolicy: contract.TimeoutPolicyLegacy, Log: contract.TaskLog{Data: bytes.Repeat([]byte("p"), c.primary), SourceBytes: c.primary, ReceivedBytes: c.primary}}
			tmp, err := st.prepare(rec)
			if err == nil {
				_, err = st.publishFirst(tmp, rec.TaskID)
			}
			if err != nil {
				t.Fatal(err)
			}
			res := contract.TaskResultBody{TaskID: rec.TaskID, Execution: rec.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int), OutputBytes: 9}.Sealed()
			mem := rec
			mem.Log.Data = nil
			e := &taskEntry{rec: mem, retained: c.primary, key: keyOf(rec.Role), confirmed: true, released: true,
				late: &lateCand{result: res, at: fin, log: contract.TaskLog{Data: []byte("123456789"), SourceBytes: 9, ReceivedBytes: 9}}}
			ts.tasks[rec.TaskID] = e
			ts.mu.Lock()
			job, _ := ts.nextJobLocked(e, fin)
			ts.mu.Unlock()
			if job == nil || job.kind != "late" {
				t.Fatalf("%s: job %+v", name, job)
			}
			job, err = ts.composeLate(e, job)
			if err != nil {
				t.Fatal(err)
			}
			l := job.rec.Late
			if string(l.Log.Data) != c.want || len(job.rec.Log.Data) != c.primary || l.Log.ReceivedBytes != 9 {
				t.Fatalf("%s: late tail %q, primary %d", name, l.Log.Data, len(job.rec.Log.Data))
			}
			if s := lateSummary(*l, len(l.Log.Data)); s.Log.RetainedBytes != len(c.want) || s.Log.Truncated != (len(c.want) < 9) {
				t.Fatalf("%s: summary %+v", name, s.Log)
			}
			if err := st.update(job.rec); err != nil {
				t.Fatalf("%s: the combined document: %v", name, err)
			}
			ts.mu.Lock()
			ts.applyLocked(e, job, nil)
			ts.mu.Unlock()
			if e.lateRetained != len(c.want) || e.rec.Late.Log.Data != nil || e.late != nil {
				t.Fatalf("%s: installed late evidence kept %d / %v", name, e.lateRetained, e.rec.Late.Log.Data)
			}
		}
	})
	t.Run("crash-during-append", func(t *testing.T) {
		t.Parallel()
		// The late write fails: admissions are blocked globally, the
		// released reservation is not reacquired; after a restart the
		// worker's uncommitted outcome is requested again and recorded.
		tp, w := ctlPlane(t, 2)
		st := tp.run(t, w, "a", "crash", "p2")
		started := tp.clk.Now()
		tp.loseByLease(t, w, st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		r := result(st, 0, 4, nil)
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &r.Digest))
		tp.inj.set("rename", taskRel(st.TaskID))
		w.logAck(w.sendLateLog(st, 0, []byte("tail"), r.Digest))
		w.resultAck(w.sendResult(r), st.TaskID)
		tp.log.await(t, "publish-failed late "+st.TaskID)
		if tp.roleInflight(t, "a") != 0 {
			t.Fatal("a failing late write reacquired the slot")
		}
		_, err := tp.cl.Dispatch(bg, taskReq(contract.TargetID, "a", "blocked"))
		wantAdmission(t, err, contract.CodeUnavailable, contract.ReasonNoCapacity, "a:storage_unconfirmed")
		if v := tp.show(t, st.TaskID); v.PersistenceReason == nil || v.LateResult != nil || v.State != contract.TaskLost {
			t.Fatalf("failing late write %+v", v)
		}
		tp.inj.set("", "")
		tp.restart(t)
		if rec := tp.taskFile(t, st.TaskID); rec.Late != nil {
			t.Fatal("the failed late write reached the document")
		}
		w = tp.workerInv(t, idA, map[string]string{st.TaskID: contract.ActionSendResult}, entry(st, contract.PhaseResult, &started, &r.Digest))
		w.logAck(w.sendLateLog(st, 0, []byte("tail"), r.Digest))
		w.resultAck(w.sendResult(r), st.TaskID)
		tp.log.awaitOnce(t, "late-committed "+st.TaskID)
		if lg, err := tp.cl.TaskLateLogs(bg, st.TaskID); err != nil || string(lg.Data) != "tail" {
			t.Fatalf("late tail %q %v", lg.Data, err)
		}
		tp.admit(t, taskReq(contract.TargetID, "a", "unblocked"))
	})
	t.Run("rejection", func(t *testing.T) {
		t.Parallel()
		// Invalid identities never create late history: an unknown task,
		// a wrong execution token, another node, or a tagged tail without
		// its reconciliation's authorization.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1), record(taskCfg("b", "coder", idB, 1), 2)}, idA, idB)
		w := tp.worker(t, idA)
		st := tp.run(t, w, "a", "rejected", "p2")
		tp.loseByLease(t, w, st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		r := result(st, 0, 0, nil)
		unknown := st
		unknown.TaskID = "t_" + strings.Repeat("f", 32)
		wrongExec := st
		wrongExec.Execution.Attachment += 7
		for name, send := range map[string]func(w *worker) string{
			"unknown task": func(w *worker) string { return w.sendResult(result(unknown, 0, 0, nil)) },
			"token":        func(w *worker) string { return w.sendResult(result(wrongExec, 0, 0, nil)) },
			"no authority": func(w *worker) string { return w.sendLateLog(st, 0, []byte("x"), r.Digest) },
			"no reconcile": func(w *worker) string { return w.sendResult(r) },
		} {
			w := tp.worker(t, idA)
			expectClosed(t, w, send(w))
			tp.log.await(t, "detached "+idA)
			_ = name
		}
		wb := tp.worker(t, idB)
		expectClosed(t, wb, wb.sendResult(r))
		if v := tp.show(t, st.TaskID); v.LateResult != nil || v.State != contract.TaskLost {
			t.Fatalf("an unauthorized report created late history: %+v", v)
		}
		if _, err := tp.cl.ShowTask(bg, unknown.TaskID, 0); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("unknown task created: %v", err)
		}
	})
}

// golden05 copies the golden iteration 05 documents into root's tasks/
// and returns their task IDs by name.
func golden05(t *testing.T, root string) map[string]string {
	t.Helper()
	src := filepath.Join("..", "contract", "testdata", "schema1")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, tasksName), 0o700)
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		rec, err := contract.ParseTaskRecord(b, roleLookup)
		if err != nil {
			t.Fatalf("golden %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(root, tasksName, rec.TaskID+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(e.Name(), ".json")] = rec.TaskID
	}
	return out
}

// goldenNode and goldenRole are the golden documents' node and role.
const goldenNode = "n_0123456789abcdef0123456789abcdef"

func goldenRole() contract.RoleRecord {
	return contract.RoleRecord{RoleConfig: contract.RoleConfig{ID: "worker-a", Name: "implementer", Node: goldenNode, Adapter: "fake", Instruction: "/srv/i.md",
		Runbook: "/srv/r.md", Model: "example", Effort: "medium", Concurrency: 2, Timeout: 2 * time.Hour, HasTimeout: true}, RegistrationOrder: 1}
}

// goldenRoot is a plane root holding the golden 05 task history, with
// worker-a registered (order 1) and worker-gone (order 2) removed.
func goldenRoot(t *testing.T) (string, map[string]string) {
	t.Helper()
	root := freshRoot(t)
	writeNodeRecord(t, root, goldenNode+".json", encodeNodeRecord(goldenNode, t0), 0o600)
	writeRegistry(t, root, docOf(1, 3, goldenRole()))
	return root, golden05(t, root)
}

// taskFiles reads every task document's bytes by task ID.
func taskFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(root, tasksName))
	out := map[string][]byte{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(root, tasksName, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

// TestControlMigration is UT FP-8 on the plane, delegated from
// tests/function (TestControlLegacy) with the contract's: golden 05
// pending, running, succeeded, failed and rejected records (one of a
// removed role) are loaded; the read-only loaders expose the prospective
// migration and write nothing; Run migrates only nonterminal records to
// lost (legacy_execution_unrecoverable) one file at a time, releasing
// their reservations, and never rewrites terminal history; a failure
// halfway prevents admission and a rerun converts the rest; ordinary
// removal has no recovery exception. Do not rename or skip it.
func TestControlMigration(t *testing.T) {
	t.Parallel()
	t.Run("read-only", func(t *testing.T) {
		t.Parallel()
		root, _ := goldenRoot(t)
		before := taskFiles(t, root)
		st, err := (&deps{now: fixed(t0)}).inspect(bg, root)
		if err != nil || st.PendingMigrations != 3 {
			t.Fatalf("status %+v %v", st.PendingMigrations, err)
		}
		d := testDeps(t)
		if _, err := d.init(bg, InitOptions{StateDir: root}); err != nil {
			t.Fatalf("init validation: %v", err)
		}
		if _, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"127.0.0.1", "localhost"}}); err != nil {
			t.Fatalf("reissue: %v", err)
		}
		after := taskFiles(t, root)
		if len(after) != len(before) {
			t.Fatalf("%d task files, want %d", len(after), len(before))
		}
		for name, b := range before {
			if !bytes.Equal(b, after[name]) {
				t.Fatalf("a read-only loader rewrote %s", name)
			}
		}
	})
	t.Run("run", func(t *testing.T) {
		t.Parallel()
		root, ids := goldenRoot(t)
		before := taskFiles(t, root)
		d := fast(testDeps(t))
		np := serveNodePlaneAt(t, d, root)
		migrated := map[string]string{"pending": "", "running": "partial\n", "removed-running": ""}
		for name, id := range ids {
			v, err := np.cl.ShowTask(bg, id, contract.DefaultTailLines)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			rec, err := layout{root: root}.readTaskFile(id, roleLookup)
			if err != nil {
				t.Fatal(err)
			}
			if tail, ok := migrated[name]; ok {
				if v.State != contract.TaskLost || v.Reason.Code != contract.ReasonLegacyUnrecoverable || v.FinishedAt == nil || v.Reconciling ||
					v.LogTail != tail || v.TimeoutEnforced || rec.Schema != contract.TaskRecordSchemaVersion || rec.TimeoutPolicy != contract.TimeoutPolicyLegacy {
					t.Fatalf("%s migrated %+v", name, v)
				}
				if (name == "pending") != (v.StartedAt == nil) {
					t.Fatalf("%s started_at %v", name, v.StartedAt)
				}
				continue
			}
			if !bytes.Equal(before[id+".json"], taskFiles(t, root)[id+".json"]) || rec.Schema != contract.LegacyTaskRecordSchemaVersion {
				t.Fatalf("terminal history %s was rewritten", name)
			}
		}
		if v, err := np.cl.ShowTask(bg, ids["succeeded"], 0); err != nil || v.State != contract.TaskSucceeded {
			t.Fatalf("a 05 success is still a success: %+v %v", v, err)
		}
		if r, err := np.cl.ShowRole(bg, "worker-a"); err != nil || r.Inflight != 0 {
			t.Fatalf("worker-a %+v %v", r, err)
		}
		// Ordinary removal needs no recovery exception now.
		if err := np.cl.RemoveRole(bg, "worker-a", false); err != nil {
			t.Fatalf("rm: %v", err)
		}
	})
	t.Run("halfway", func(t *testing.T) {
		t.Parallel()
		// The second migrated file fails: Run fails before listening
		// (nothing admitted), leaving a valid mix; a rerun converts only
		// the remaining schema-1 nonterminal record. A visible replacement
		// whose sync fails is resynced before the next.
		root, ids := goldenRoot(t)
		order := []string{ids["pending"], ids["running"], ids["removed-running"]}
		failID := order[1]
		d := fast(testDeps(t))
		d.fail = failAt("rename", taskRel(failID))
		if err := d.run(bg, RunOptions{StateDir: root, Logger: slog.New(slog.DiscardHandler)}); contract.CodeOf(err) != contract.CodeInternal ||
			!strings.Contains(err.Error(), "cannot migrate legacy task "+failID) {
			t.Fatalf("run = %v", err)
		}
		schemas := func() map[string]int {
			out := map[string]int{}
			for _, id := range order {
				rec, err := layout{root: root}.readTaskFile(id, roleLookup)
				if err != nil {
					t.Fatal(err)
				}
				out[id] = rec.Schema
			}
			return out
		}
		if s := schemas(); s[order[0]] != 2 || s[order[1]] != 1 || s[order[2]] != 1 {
			t.Fatalf("after the failure %v", s)
		}
		migratedBytes := taskFiles(t, root)[order[0]+".json"]
		d = fast(testDeps(t))
		failed := false
		d.fail = func(op, name string) error {
			if op == "dirsync" && name == tasksName && !failed {
				failed = true
				return os.ErrDeadlineExceeded
			}
			return nil
		}
		np := serveNodePlaneAt(t, d, root)
		if s := schemas(); s[order[1]] != 2 || s[order[2]] != 2 || !failed {
			t.Fatalf("after the rerun %v (sync failed %v)", s, failed)
		}
		if !bytes.Equal(migratedBytes, taskFiles(t, root)[order[0]+".json"]) {
			t.Fatal("the rerun rewrote an already migrated record")
		}
		if st, err := np.cl.ShowTask(bg, order[2], 0); err != nil || st.State != contract.TaskLost {
			t.Fatalf("%+v %v", st, err)
		}
	})
	t.Run("late-history", func(t *testing.T) {
		t.Parallel()
		// A later authorized late result on a schema-1 terminal record
		// writes it as schema 2 then (never at startup).
		root, ids := goldenRoot(t)
		d := fast(testDeps(t))
		served := make(chan *taskService, 1)
		d.onTasks = func(ts *taskService) { served <- ts }
		serveNodePlaneAt(t, d, root)
		ts := <-served
		id := ids["failed"]
		rec, _ := layout{root: root}.readTaskFile(id, roleLookup)
		res := contract.TaskResultBody{TaskID: id, Execution: rec.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int), OutputBytes: 0}.Sealed()
		// The late evidence as an authorized receipt leaves it (the wire
		// path is TestControlLate's); the writer publishes it.
		ts.mu.Lock()
		e := ts.tasks[id]
		e.late = &lateCand{result: res, at: t0, log: contract.TaskLog{}}
		ts.wakeLocked(e)
		ts.mu.Unlock()
		deadline := time.After(testWait)
		for {
			got, err := layout{root: root}.readTaskFile(id, roleLookup)
			if err == nil && got.Late != nil {
				if got.Schema != contract.TaskRecordSchemaVersion || got.State != contract.TaskFailed || got.Revision != rec.Revision+1 {
					t.Fatalf("late on legacy history %+v", got)
				}
				break
			}
			select {
			case <-deadline:
				t.Fatal("the late write never happened")
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
}

// BenchmarkControlCommit measures the per-task writer's terminal
// publications (natural and lost) and a late append, with 0, 64 KiB and
// 10 MiB tails: encode, temporary write and fsync, rename and directory
// sync, installation under the task lock. It reports the bytes written
// per commit and asserts bounded allocation (about one document per
// write; a late append also decodes the document for the frozen primary
// tail), the
// release of every buffer and exactly-once release of the reservation.
func BenchmarkControlCommit(b *testing.B) {
	for _, kind := range []string{"natural", "lost", "late"} {
		for _, size := range []int{0, 64 << 10, contract.MaxLogRetainedBytes} {
			b.Run(kind+"-"+strconv.Itoa(size), func(b *testing.B) { benchCommit(b, kind, size) })
		}
	}
}

func benchCommit(b *testing.B, kind string, size int) {
	root := filepath.Join(b.TempDir(), "state")
	os.MkdirAll(root, 0o700)
	d := defaultDeps()
	st := &taskStore{l: layout{root: root}, d: d}
	ts := &taskService{st: st, clock: realClock{}, logger: slog.New(slog.DiscardHandler), lookup: roleLookup, tasks: map[string]*taskEntry{},
		counts: map[instanceKey]*heldCount{}, blocked: map[*taskEntry]bool{}, stop: make(chan struct{})}
	start := t0.Add(time.Second)
	digest := strings.Repeat("ab", 32)
	rec := contract.TaskRecord{TaskID: "t_00000000000000000000000000000001", Request: taskReq(contract.TargetID, "a", "bench"),
		Role: record(taskCfg("a", "coder", idA, 1), 1), Effective: contract.TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour},
		Execution: contract.ExecutionToken{Epoch: "00000000000000000000000000000000", Attachment: 1}, State: contract.TaskRunning,
		CreatedAt: t0, StartedAt: &start, Revision: 1, StartDigest: &digest, TimeoutPolicy: contract.TimeoutPolicyLegacy}
	tail := contract.TaskLog{Data: bytes.Repeat([]byte{0x1b, 'x', '\n'}, size/3+1)[:size], SourceBytes: size, ReceivedBytes: size}
	if kind == "late" {
		fin := start.Add(time.Second)
		rec.State, rec.FinishedAt = contract.TaskLost, &fin
		rec.Reason = &contract.TaskReason{Code: contract.ReasonLeaseExpired, Message: "expired"}
	}
	tmp, err := st.prepare(rec)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := st.publishFirst(tmp, rec.TaskID); err != nil {
		b.Fatal(err)
	}
	res := contract.TaskResultBody{TaskID: rec.TaskID, Execution: rec.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int), OutputBytes: size}.Sealed()
	var written int64
	var before, after runtime.MemStats
	b.ReportAllocs()
	runtime.ReadMemStats(&before)
	n := 0
	for b.Loop() {
		n++
		e := &taskEntry{rec: rec, key: keyOf(rec.Role), confirmed: true, wake: make(chan struct{}, 1)}
		now := time.Now()
		switch kind {
		case "natural":
			e.cand = &terminalCand{kind: candNatural, at: now, digest: res.Digest,
				outcome: terminalOutcome{state: contract.TaskSucceeded, started: &start, finished: now, exit: res.ExitCode, log: tail}}
		case "lost":
			e.cand = &terminalCand{kind: candLost, at: now,
				outcome: terminalOutcome{state: contract.TaskLost, started: &start, finished: now, log: tail, reason: &contract.TaskReason{Code: contract.ReasonLeaseExpired, Message: "expired"}}}
		case "late":
			e.released = true
			e.late = &lateCand{result: res, at: now, log: tail}
		}
		ts.mu.Lock()
		ts.tasks[rec.TaskID] = e
		ts.refreshLocked(e)
		job, _ := ts.nextJobLocked(e, now)
		ts.mu.Unlock()
		if job == nil {
			b.Fatal("no publication")
		}
		if kind == "late" {
			if job, err = ts.composeLate(e, job); err != nil {
				b.Fatal(err)
			}
		}
		enc, err := encodeTask(job.rec)
		if err != nil || len(enc) > maxTaskFile {
			b.Fatalf("document %d bytes: %v", len(enc), err)
		}
		written += int64(len(enc))
		err = st.update(job.rec)
		ts.mu.Lock()
		ts.applyLocked(e, job, err)
		ok := e.confirmed && e.released && e.cand == nil && e.late == nil && e.ring == nil && ts.heldLocked(e.key).held == 0
		ts.mu.Unlock()
		if err != nil || !ok {
			b.Fatalf("commit: %v (confirmed %v released %v)", err, e.confirmed, e.released)
		}
		rec = e.rec
		if kind != "late" {
			// Every iteration commits the same running task afresh.
			rec.State, rec.FinishedAt, rec.ExitCode, rec.Reason, rec.ResultDigest = contract.TaskRunning, nil, nil, nil, nil
		}
		rec.Log.Data = nil
	}
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(written)/float64(max(n, 1)), "bytes-written/op")
	per := (after.TotalAlloc - before.TotalAlloc) / uint64(max(n, 1))
	// A terminal commit encodes about one document (two with the frozen
	// tail's copy); a late append also strictly decodes the current
	// document to read the frozen primary tail, which the strict reader
	// (05's readTaskFile, also behind show and logs) does at about ten
	// times the document's size: bounded, per task, transient.
	docs := uint64(3)
	if kind == "late" {
		docs = 14
	}
	if bound := docs*uint64(written/int64(max(n, 1))) + 4<<20; per > bound {
		b.Fatalf("%d bytes allocated per commit (bound %d)", per, bound)
	}
	got, err := st.l.readTaskFile(rec.TaskID, roleLookup)
	if err != nil {
		b.Fatal(err)
	}
	if kind == "late" && (got.Late == nil || len(got.Log.Data)+len(got.Late.Log.Data) > contract.MaxLogRetainedBytes) {
		b.Fatal("late tail budget")
	}
}
