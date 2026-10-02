package sidecar

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// blockingOpen makes the task-start manual reads of path wait for
// release (the probe and validation checks are unaffected).
type blockingOpen struct {
	mu      sync.Mutex
	path    string
	arrived chan struct{}
	release chan struct{}
}

func (b *blockingOpen) open(p string) (*os.File, error) {
	b.mu.Lock()
	hit := p == b.path
	if hit {
		b.path = ""
	}
	b.mu.Unlock()
	if hit {
		close(b.arrived)
		<-b.release
	}
	return openManual(p)
}

// TestTaskStream is UT FP-3/9 for the sidecar's task frames; its duplex,
// bounds, result-receipt and result-ack-loss subtests are delegated from
// tests/function (TestTaskProtocol), together with the plane's. Do not
// rename or skip them.
func TestTaskStream(t *testing.T) {
	t.Parallel()
	t.Run("duplex", func(t *testing.T) {
		t.Parallel()
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "INSTRUCTION")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		// The first ready cycle completes before the clock moves: it read
		// its start (T0) and armed the next cycle for T0+5s, the instant
		// the advance below reaches. Moving the clock before that start
		// would shift the whole cadence past the advance.
		tr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 1 })
		// Its prompt ready report (iteration 10a) was acknowledged and
		// processed before the clock moves.
		s.report(t, 1, func(r contract.RoleStatus) bool { return r.CanAccept })
		s.c.settle()
		// Full duplex: the sidecar's heartbeat is outstanding while the
		// plane's start is answered, on independent slots (the periodic
		// heartbeat, read and acknowledged by the test).
		s.c.setManual()
		tr.clk.Advance(heartbeatInterval)
		hb := s.request(t)
		if hb.Type != contract.FrameHeartbeat {
			t.Fatalf("got %s %s, want the periodic heartbeat", hb.Type, hb.RequestID)
		}
		k, _ := strconv.Atoi(hb.RequestID[1:])
		st := startBody(1, s.cfgs[0], 1, 1, "goal one")
		s.c.sendStart("p2", st)
		if r := s.c.startResult("p2"); r.Err != nil || r.TaskID != st.TaskID {
			t.Fatalf("start = %+v", r)
		}
		ch := tr.child(t)
		// The occupied slot is reported once the heartbeat's exchange ends.
		s.c.setAuto()
		s.c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, hb.RequestID, nil)
		// The heartbeat ack, and the ready cycle due at the same tick (the
		// second cycle, the first published since the wait above), finish
		// in either order; the final heartbeat's readiness depends on that
		// cycle.
		tr.ev.awaitAll(t, func(ev event) bool { return ev.kind == evAck && ev.acks == k }, nthKind(evCycleDone, 1))
		if r := s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 1 }); r.Roles[0].Concurrency != 2 {
			t.Fatalf("occupied report %+v", r.Roles)
		}
		if p := ch.prompt(t); !bytes.Contains(p, []byte("INSTRUCTION")) || !bytes.Contains(p, []byte(`"goal":"goal one"`)) {
			t.Fatalf("prompt %s", p)
		}
		// Output and the result take the next b numbers after heartbeats.
		ch.out([]byte("progress\n"))
		if got := s.logs(t, st, 9); string(got) != "progress\n" {
			t.Fatalf("log %q", got)
		}
		// An identical duplicate start (new request ID) returns the same
		// outcome and starts nothing.
		s.c.sendStart("p3", st)
		if r := s.c.startResult("p3"); r.Err != nil {
			t.Fatalf("duplicate = %+v", r)
		}
		tr.noChild(t)
		ch.exitCode(0)
		res := s.result(t, st)
		if *res.ExitCode != 0 || res.OutputBytes != 9 || res.Signal != nil {
			t.Fatalf("result %+v", res)
		}
		// Heartbeats report the run-lifetime local count: back to zero,
		// reported at once.
		if hb := s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 0 }); !hb.Roles[0].CanAccept {
			t.Fatalf("heartbeat %+v", hb.Roles)
		}
		// Even after completion the attachment remembers the start: a
		// delayed duplicate cannot start another child.
		s.c.sendStart("p4", st)
		if r := s.c.startResult("p4"); r.Err != nil {
			t.Fatalf("late duplicate = %+v", r)
		}
		tr.noChild(t)
		// Different data for a known task is a protocol error (terminal for
		// the sidecar, like every invalid plane message).
		changed := st
		changed.Request.Goal = "another goal"
		s.c.sendStart("p5", changed)
		f := s.c.expect(contract.FrameError, "p5")
		if e, _ := contract.ParseErrorBody(f.Body); e == nil || !strings.Contains(e.Message, "different data") {
			t.Fatalf("collision error %s", f.Body)
		}
		if st := s.c.closed(); st != websocket.StatusPolicyViolation {
			t.Fatalf("close %v", st)
		}
		wantCode(t, tr.run.result(t), contract.CodeInvalidArgument, "different data")
	})
	t.Run("token", func(t *testing.T) {
		t.Parallel()
		// The first start of an attachment sets its execution token;
		// another token on the same attachment is a protocol error.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "i")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		_, ch := s.run(t, 1, 0, "gen one")
		other := startBody(2, s.cfgs[0], 1, 7, "wrong token")
		s.c.sendStart(s.nextP(), other)
		if f := s.c.recv(); f.Type != contract.FrameError {
			t.Fatalf("token mismatch answered %s", f.Type)
		}
		tr.noChild(t)
		wantCode(t, tr.run.result(t), contract.CodeInvalidArgument, "execution token")
		ch.exitCode(0)
	})
	t.Run("arbitration", func(t *testing.T) {
		t.Parallel()
		// Result-ready and log-ready tasks alternate, round-robin by task
		// ID inside each class; a result waits for its own output.
		sup := &taskSupervisor{workers: map[*taskWorker]bool{}}
		tag := &attachTag{n: 1}
		mk := func(n int, logs, exited bool) *taskWorker {
			w := &taskWorker{start: contract.TaskStartBody{TaskID: taskID(n)}, tag: tag, ring: newOutputRing(1 << 20), replied: true}
			if exited {
				zero := 0
				res := contract.TaskResultBody{TaskID: taskID(n), Outcome: contract.OutcomeNatural, ExitCode: &zero}.Sealed()
				w.outcome = &res
			}
			if logs {
				w.ring.write([]byte("x"))
			}
			sup.workers[w] = true
			return w
		}
		mk(1, true, false)
		mk(2, false, true)
		mk(3, true, false)
		mk(4, false, true)
		mk(5, true, true) // exited but not drained: logs only
		rs := &roleSession{tasks: sup, tag: tag}
		now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
		var got []string
		for i := 0; i < 6; i++ {
			w, kind, _ := rs.nextOutput(now)
			got = append(got, map[reqKind]string{reqLog: "L", reqResult: "R"}[kind]+w.id()[len(w.id())-1:])
		}
		if want := "L1,R2,L3,R4,L5,R2"; strings.Join(got, ",") != want {
			t.Fatalf("order %v, want %s", got, want)
		}
		// A result awaiting its uncommitted retry is not due before it; the
		// earliest retry is reported.
		for w := range sup.workers {
			if w.outcome != nil {
				w.nextSend = now.Add(resultRetry)
			}
		}
		for i := 0; i < 3; i++ {
			if w, kind, retry := rs.nextOutput(now); kind != reqLog || !retry.Equal(now.Add(resultRetry)) {
				t.Fatalf("retry pass %d picked %v %v (retry %v)", i, w, kind, retry)
			}
		}
		// Unbound (fenced), unreplied or committed tasks never qualify.
		for w := range sup.workers {
			w.tag = nil
		}
		if w, _, _ := rs.nextOutput(now); w != nil {
			t.Fatal("an unbound task was picked")
		}
	})
	t.Run("bounds", func(t *testing.T) {
		t.Parallel()
		t.Run("output-exchange", func(t *testing.T) {
			t.Parallel()
			// A task_log receipt not acknowledged within four seconds of its
			// slot reservation ends the session. Chunks are at most 16 KiB.
			// The next attachment reconciles the execution (continue) and
			// resends exactly the unacknowledged chunk, then the rest, then
			// the result.
			fp := startFakePlane(t)
			tr := startTaskRun(t, fp, taskOpts{})
			ins, run := manuals(t, tr.dir, "a", "i")
			s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
			st, ch := s.run(t, 1, 0, "big")
			ch.out(bytes.Repeat([]byte("z"), 3*contract.MaxLogChunkBytes/2))
			rid, lb := s.nextLog(t)
			if len(lb.Data) != contract.MaxLogChunkBytes {
				t.Fatalf("chunk of %d bytes", len(lb.Data))
			}
			tr.ev.awaitMatch(t, evOutputWritten, func(ev event) bool { return ev.id == rid })
			tr.clk.Advance(outputExchange)
			if st := s.c.closed(); st == websocket.StatusNormalClosure {
				t.Fatalf("close %v", st)
			}
			ended := tr.ev.await(t, evEnded)
			if !strings.Contains(ended.err.Error(), "no task output acknowledgement within 4s") {
				t.Fatalf("ended with %v", ended.err)
			}
			tr.advanceBackoff(t, backoff(0, 0.5))
			s, _ = tr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionContinue}, roleConfig("a", ins, run))
			rid, again := s.nextLog(t)
			if again.Offset != lb.Offset || !bytes.Equal(again.Data, lb.Data) || again.LateDigest != nil {
				t.Fatalf("resent chunk at %d of %d bytes", again.Offset, len(again.Data))
			}
			s.c.ackLog(rid, st.TaskID, again.Offset+len(again.Data))
			ch.exitCode(0)
			tr.settled(t, st.TaskID)
			r, rest := s.drain(t, st)
			if len(rest) != contract.MaxLogChunkBytes/2 || r.OutputBytes != 3*contract.MaxLogChunkBytes/2 {
				t.Fatalf("after the resend: %d bytes, result %+v", len(rest), r)
			}
			// The freed slot is reported at once (iteration 10a).
			s.report(t, 2, func(r contract.RoleStatus) bool { return r.Inflight == 0 })
		})
		t.Run("preparation", func(t *testing.T) {
			t.Parallel()
			// Preparation is bounded to ten seconds from receipt (06a): a
			// start still preparing then is refused (preparation_timeout)
			// and its worker is never authorized afterwards; the stream
			// stays up and keeps its heartbeats through the budget.
			fp := startFakePlane(t)
			bo := &blockingOpen{arrived: make(chan struct{}), release: make(chan struct{})}
			tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) { d.openManual = bo.open }})
			ins, run := manuals(t, tr.dir, "a", "i")
			s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
			tr.ev.awaitMatch(t, evCycleDone, nil)
			bo.mu.Lock()
			bo.path = ins
			bo.mu.Unlock()
			st := startBody(1, s.cfgs[0], 1, 1, "stuck")
			s.c.sendStart("p2", st)
			<-bo.arrived
			// The reserved slot's report, then the periodic heartbeat.
			s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 1 && r.CanAccept })
			s.periodic(t, 1)
			// At the budget the refusal and the next heartbeat are both due:
			// read them in whichever order they were written.
			tr.clk.Advance(prepBudget - heartbeatInterval)
			var r *contract.TaskStartResult
			for r == nil {
				f := s.c.recv()
				switch f.Type {
				case contract.FrameHeartbeat:
					s.c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, f.RequestID, nil)
					s.b++
				case contract.FrameTaskStartResult:
					got, err := contract.DecodeTaskStartResult(f.Body)
					if err != nil || f.RequestID != "p2" {
						t.Fatalf("start result %s %v", f.Body, err)
					}
					r = &got
				default:
					t.Fatalf("unexpected %s", f.Type)
				}
			}
			if r.Err == nil || r.Err.Details["reason"] != contract.ReasonPreparationTimeout {
				t.Fatalf("stuck start = %+v", r)
			}
			tr.ev.awaitMatch(t, evStartReplied, func(ev event) bool { return ev.id == st.TaskID })
			close(bo.release)
			tr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == st.TaskID })
			tr.noChild(t)
			if w := tr.super(t).find(st.TaskID); w != nil {
				tr.ev.awaitCollected(t, st.TaskID)
			}
			if _, err := os.Stat(filepath.Join(tr.root, "tasks", st.TaskID)); !os.IsNotExist(err) {
				t.Fatalf("the refused start kept its journal: %v", err)
			}
		})
		t.Run("authorization-collection", func(t *testing.T) {
			t.Parallel()
			// A launch authorization (holding its worker's lock) and a
			// worker collection running concurrently: collection has
			// reached the workers it is about to inspect when authorization
			// continues. With one lock order both complete and the start is
			// answered; a crossed order deadlocks here.
			fp := startFakePlane(t)
			var sup *taskSupervisor
			reached := make(chan struct{}, 1)
			var armed atomic.Bool
			collected := make(chan struct{})
			tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) {
				d.taskGCHook = func() {
					if armed.CompareAndSwap(true, false) {
						reached <- struct{}{}
					}
				}
				d.taskAuthHook = func(string) {
					armed.Store(true)
					go func() { sup.gc(); close(collected) }()
					<-reached
				}
			}})
			sup = tr.super(t)
			ins, run := manuals(t, tr.dir, "a", "i")
			s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
			st, ch := s.run(t, 1, 0, "racing collection")
			select {
			case <-collected:
			case <-time.After(testWait):
				t.Fatal("worker collection never completed")
			}
			ch.exitCode(0)
			s.result(t, st)
		})
		t.Run("authorized", func(t *testing.T) {
			t.Parallel()
			// Launch authorized but not confirmed at the bound (DW6):
			// neither ok nor a refusal is truthful, so no reply is sent and
			// the WebSocket closes at once with 1001 and a reason distinct
			// from a graceful shutdown's; the error is retryable. The
			// worker, its slot and journal are kept: the next attachment's
			// inventory reports it running, and after reconciliation its
			// result flows there.
			fp := startFakePlane(t)
			block := make(chan struct{})
			gate := &gatedFactory{block: block, arrived: make(chan struct{})}
			tr := startTaskRun(t, fp, taskOpts{wrap: gate.wrap})
			ins, run := manuals(t, tr.dir, "a", "i")
			s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
			st := startBody(1, s.cfgs[0], 1, 1, "slow start")
			s.c.sendStart("p2", st)
			<-gate.arrived
			// The heartbeat due mid-budget is answered first (after the
			// passed cycle's and the reserved slot's reports).
			s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 1 && r.CanAccept })
			s.periodic(t, 1)
			tr.clk.Advance(prepBudget - heartbeatInterval)
			var code websocket.StatusCode
			var reason string
			for {
				r := s.c.next()
				if r.err != nil {
					var ce websocket.CloseError
					if errors.As(r.err, &ce) {
						code, reason = ce.Code, ce.Reason
					}
					break
				}
				if f, err := contract.DecodeFrame(r.data, contract.FromSidecar); err == nil && f.Type == contract.FrameTaskStartResult {
					t.Fatalf("an unconfirmed launch was answered: %s", f.Body)
				}
			}
			if code != websocket.StatusGoingAway || reason != prepOverrunReason || reason == "sidecar shutting down" {
				t.Fatalf("closed with %v %q", code, reason)
			}
			ended := tr.ev.await(t, evEnded)
			if !strings.Contains(ended.err.Error(), "authorized to start") || contract.CodeOf(ended.err) != contract.CodeUnavailable || terminal(ended.err) {
				t.Fatalf("ended with %v", ended.err)
			}
			tr.advanceBackoff(t, backoff(0, 0.5))
			s, inv := tr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionContinue}, roleConfig("a", ins, run))
			if len(inv) != 1 || inv[0].Phase != contract.PhaseRunning || inv[0].StartedAt != nil {
				t.Fatalf("inventory %+v", inv)
			}
			close(block)
			ch := tr.child(t)
			ch.exitCode(0)
			tr.settled(t, st.TaskID)
			if r, _ := s.drain(t, st); *r.ExitCode != 0 || r.Outcome != contract.OutcomeNatural {
				t.Fatalf("reconciled result %+v", r)
			}
		})
	})
	t.Run("result-receipt", func(t *testing.T) {
		t.Parallel()
		// The plane's receipt acknowledgement ends the exchange: nothing
		// waits for durability; the transmission copy is discarded while
		// the attachment keeps the start's outcome; another task keeps
		// running and the stream stays attached past four seconds.
		t.Run("overlapping-collectors", func(t *testing.T) {
			t.Parallel()
			// Two collectors overlap: the worker's own completion collector
			// is paused right after its snapshot (it still holds the
			// worker) while the session applies the acknowledgement and
			// its collector deletes the worker. Collection is reported only
			// once no snapshot that could hold the worker is outstanding,
			// so acknowledgement plus collection always means unreachable.
			fp := startFakePlane(t)
			var armed atomic.Bool
			paused, release := make(chan struct{}), make(chan struct{})
			released := false
			releaseA := func() {
				if !released {
					released = true
					close(release)
				}
			}
			tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) {
				d.taskGCHook = func() {
					if armed.CompareAndSwap(true, false) {
						close(paused)
						<-release
					}
				}
			}})
			t.Cleanup(releaseA) // runs before the Run's own cleanup
			ins, run := manuals(t, tr.dir, "a", "i")
			s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
			st, ch := s.run(t, 1, 0, "collected")
			w := tr.super(t).find(st.TaskID)
			wp := weak.Make(w)
			w = nil
			// Collector A: the first collection after the child exits is
			// the worker's own completion; it pauses after its snapshot.
			armed.Store(true)
			ch.exitCode(0)
			select {
			case <-paused:
			case <-time.After(testWait):
				t.Fatal("the completion collector never took its snapshot")
			}
			// Collector B: the session applies the acknowledgement and
			// collects (the worker is done and acknowledged).
			rid, res := s.nextResult(t)
			s.c.ackResult(rid, st.TaskID, res.Digest, true)
			collected, acked := false, false
			deadline := time.After(testWait)
			for !acked || !collected {
				select {
				case ev := <-tr.ev.ch:
					switch {
					case ev.kind == evTaskCollected && ev.id == st.TaskID:
						collected = true
						if !released {
							// Reported while A's snapshot is outstanding.
							runtime.GC()
							if wp.Value() != nil {
								t.Fatal("the worker was reported collected while another collector's snapshot still held it")
							}
						}
					case ev.kind == evResultAcked && ev.id == st.TaskID:
						acked = true
						releaseA() // B finished; now A may finish too
					}
				case <-deadline:
					t.Fatalf("acknowledged %v, collected %v", acked, collected)
				}
			}
			runtime.GC()
			if wp.Value() != nil {
				t.Fatal("the acknowledged, collected worker is still retained")
			}
		})
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "i")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st1, c1 := s.run(t, 1, 0, "one")
		st2, c2 := s.run(t, 2, 0, "two")
		// Once its result is acknowledged, nothing of the completed worker
		// stays reachable: not its request, result or output ring (up to
		// 10 MiB) through the attachment's start-deduplication table,
		// which keeps only a compact outcome.
		w1 := tr.super(t).find(st1.TaskID)
		wp, rp := weak.Make(w1), weak.Make(w1.ring)
		w1 = nil
		c1.exitCode(0)
		rid, r1 := s.nextResult(t)
		s.c.ackResult(rid, st1.TaskID, r1.Digest, true)
		// The acknowledgement and the worker's own completion are
		// independent: GC is forced only after both the session applied
		// the ack and the supervisor collected the finished worker.
		tr.ev.awaitAll(t, func(ev event) bool { return ev.kind == evResultAcked && ev.id == st1.TaskID },
			func(ev event) bool { return ev.kind == evTaskCollected && ev.id == st1.TaskID })
		runtime.GC()
		if wp.Value() != nil || rp.Value() != nil {
			t.Fatal("the acknowledged worker (or its output ring) is still retained")
		}
		// Past the four-second exchange bound (and to the next heartbeat)
		// with no write in flight: the session is still attached (the freed
		// slot's report, then the periodic heartbeat).
		s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 1 && r.CanAccept })
		s.periodic(t, 1)
		sup := tr.super(t)
		if sup.find(st1.TaskID) != nil || sup.find(st2.TaskID) == nil {
			t.Fatal("the acknowledged result was retained, or the running task dropped")
		}
		s.c.sendStart(s.nextP(), st1)
		if r := s.c.startResult("p4"); r.Err != nil {
			t.Fatalf("duplicate after receipt = %+v", r)
		}
		tr.noChild(t)
		c2.out([]byte("still running\n"))
		s.logs(t, st2, 14)
		c2.exitCode(0)
		s.result(t, st2)
	})
	t.Run("result-ack-loss", func(t *testing.T) {
		t.Parallel()
		// The acknowledgement never arrives: after the result's write
		// returned the clock moves four seconds, the session ends
		// (fencing), the loss is recorded locally, the child was reaped and
		// its slot released. A receipt never authorized deletion: the
		// outbox is kept, the next attachment's inventory reports the
		// frozen result (the same digest), the plane asks for it
		// (send_result), and the same result is sent again until a
		// committed acknowledgement deletes it; a receipt-only
		// acknowledgement schedules a retry no sooner than one second.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "i")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st, ch := s.run(t, 1, 0, "lost ack")
		ch.exitCode(3)
		rid, first := s.nextResult(t)
		if *first.ExitCode != 3 {
			t.Fatalf("result %+v", first)
		}
		tr.ev.awaitMatch(t, evOutputWritten, func(ev event) bool { return ev.id == rid })
		tr.clk.Advance(outputExchange)
		tr.ev.await(t, evEnded)
		tr.logs.await(t, "result receipt unconfirmed", func(s string) bool { return strings.Contains(s, "result_receipt_unconfirmed") })
		tr.advanceBackoff(t, backoff(0, 0.5))
		s, inv := tr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionSendResult}, roleConfig("a", ins, run))
		if len(inv) != 1 || inv[0].Phase != contract.PhaseResult || *inv[0].ResultDigest != first.Digest {
			t.Fatalf("inventory %+v", inv)
		}
		again := s.resultAck(t, st, false)
		if again.Digest != first.Digest || *again.ExitCode != 3 {
			t.Fatalf("resent %+v", again)
		}
		// The slot was released with the lost acknowledgement (the new
		// attachment reports it free at once).
		s.report(t, 2, func(r contract.RoleStatus) bool { return r.Inflight == 0 && r.CanAccept })
		// Uncommitted: retried no sooner than a second later (every
		// acknowledgement processed before the clock moves).
		s.c.settle()
		tr.clk.Advance(heartbeatInterval)
		if third := s.result(t, st); third.Digest != first.Digest {
			t.Fatalf("retried %+v", third)
		}
		tr.ev.awaitCollected(t, st.TaskID)
		if tr.super(t).find(st.TaskID) != nil {
			t.Fatal("the committed result is still held")
		}
		if _, err := os.Stat(filepath.Join(tr.root, "tasks", st.TaskID)); !os.IsNotExist(err) {
			t.Fatalf("the committed outbox remains: %v", err)
		}
	})
}

// gatedFactory holds the next guardian's release (the adapter's launch
// in progress) until released.
type gatedFactory struct {
	block   chan struct{}
	arrived chan struct{}
	once    sync.Once
}

func (g *gatedFactory) wrap(next guardianFactory) guardianFactory {
	return func(spec guardianSpec) guardianProc { return &gatedGuardian{guardianProc: next(spec), g: g} }
}

type gatedGuardian struct {
	guardianProc
	g *gatedFactory
}

func (p *gatedGuardian) Release() {
	p.g.once.Do(func() { close(p.g.arrived) })
	<-p.g.block
	p.guardianProc.Release()
}
