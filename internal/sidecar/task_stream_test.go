package sidecar

import (
	"bytes"
	"os"
	"runtime"
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
		// Full duplex: the sidecar's heartbeat is outstanding while the
		// plane's start is answered, on independent slots.
		tr.clk.Advance(heartbeatInterval)
		hbRid := s.nextB()
		s.c.readHeartbeat(2)
		st := startBody(1, s.cfgs[0], 1, 1, "goal one")
		s.c.sendStart("p2", st)
		if r := s.c.startResult("p2"); r.Err != nil || r.TaskID != st.TaskID {
			t.Fatalf("start = %+v", r)
		}
		ch := tr.child(t)
		s.c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, hbRid, nil)
		// The heartbeat ack, and the ready cycle due at the same tick (the
		// second cycle), finish in either order; the final heartbeat's
		// readiness depends on that cycle.
		tr.ev.awaitAll(t, func(ev event) bool { return ev.kind == evAck && ev.acks == 2 }, nthKind(evCycleDone, 2))
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
		// Heartbeats report the run-lifetime local count: back to zero.
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 1); hb.Roles[0].Inflight != 0 || !hb.Roles[0].CanAccept {
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
			w := &taskWorker{start: contract.TaskStartBody{TaskID: taskID(n)}, tag: tag, ring: newOutputRing(1 << 20), replied: true, exited: exited}
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
		var got []string
		for i := 0; i < 6; i++ {
			w, kind := rs.nextOutput()
			got = append(got, map[reqKind]string{reqLog: "L", reqResult: "R"}[kind]+w.id()[len(w.id())-1:])
		}
		if want := "L1,R2,L3,R4,L5,R2"; strings.Join(got, ",") != want {
			t.Fatalf("order %v, want %s", got, want)
		}
		// Fenced, unreplied or already-sent tasks never qualify.
		for w := range sup.workers {
			w.fenced = true
		}
		if w, _ := rs.nextOutput(); w != nil {
			t.Fatal("a fenced task was picked")
		}
	})
	t.Run("bounds", func(t *testing.T) {
		t.Parallel()
		t.Run("output-exchange", func(t *testing.T) {
			t.Parallel()
			// A task_log receipt not acknowledged within four seconds of its
			// slot reservation ends the session; the output is never resent
			// on the next attachment. Chunks are at most 16 KiB.
			fp := startFakePlane(t)
			tr := startTaskRun(t, fp, taskOpts{})
			ins, run := manuals(t, tr.dir, "a", "i")
			s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
			st, ch := s.run(t, 1, 0, "big")
			ch.out(bytes.Repeat([]byte("z"), 3*contract.MaxLogChunkBytes/2))
			rid := s.nextB()
			lb := s.c.expectLog(rid)
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
			s = tr.connect(t, 2, 2, roleConfig("a", ins, run))
			ch.exitCode(0)
			tr.settled(t, st.TaskID)
			tr.clk.Advance(heartbeatInterval)
			if hb := s.beat(t, 2); hb.Roles[0].Inflight != 0 {
				t.Fatalf("heartbeat %+v", hb.Roles)
			}
			// Nothing of the fenced task follows: the next frame is the
			// next heartbeat.
			tr.clk.Advance(heartbeatInterval)
			s.beat(t, 2)
		})
		t.Run("preparation", func(t *testing.T) {
			t.Parallel()
			// Preparation is bounded to two seconds from receipt: a start
			// still preparing then is refused (preparation_timeout) and its
			// worker is never authorized afterwards; the stream stays up.
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
			tr.clk.Advance(prepBudget)
			r := s.c.startResult("p2")
			if r.Err == nil || r.Err.Details["reason"] != contract.ReasonPreparationTimeout {
				t.Fatalf("stuck start = %+v", r)
			}
			tr.ev.awaitMatch(t, evStartReplied, func(ev event) bool { return ev.id == st.TaskID })
			close(bo.release)
			tr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == st.TaskID })
			tr.noChild(t)
			tr.clk.Advance(heartbeatInterval - prepBudget)
			if hb := s.beat(t, 1); hb.Roles[0].Inflight != 0 {
				t.Fatalf("the timed-out start kept its slot: %+v", hb.Roles)
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
			// Start authorized but not returned at the bound: neither ok
			// nor a refusal is truthful, so the exchange is fenced; the
			// child that then starts is supervised and never reported.
			fp := startFakePlane(t)
			block := make(chan struct{})
			gate := &gatedFactory{block: block, arrived: make(chan struct{})}
			tr := startTaskRun(t, fp, taskOpts{wrap: gate.wrap})
			ins, run := manuals(t, tr.dir, "a", "i")
			s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
			st := startBody(1, s.cfgs[0], 1, 1, "slow start")
			s.c.sendStart("p2", st)
			<-gate.arrived
			tr.clk.Advance(prepBudget)
			ended := tr.ev.await(t, evEnded)
			if !strings.Contains(ended.err.Error(), "authorized to start") {
				t.Fatalf("ended with %v", ended.err)
			}
			close(block)
			ch := tr.child(t)
			ch.exitCode(0)
			tr.settled(t, st.TaskID)
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
			rid := s.nextB()
			s.c.expectResult(rid)
			s.c.ackResult(rid, st.TaskID)
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
		rid := s.nextB()
		s.c.expectResult(rid)
		s.c.ackResult(rid, st1.TaskID)
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
		// with no write in flight: the session is still attached.
		tr.clk.Advance(heartbeatInterval)
		s.beat(t, 1)
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
		// The receipt ack never arrives: after the result's write returned
		// the clock moves four seconds, the session ends (fencing), the
		// loss is recorded locally, the child was reaped and its slot
		// released, and nothing is resent on the next attachment.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "i")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st, ch := s.run(t, 1, 0, "lost ack")
		ch.exitCode(3)
		rid := s.nextB()
		if r := s.c.expectResult(rid); *r.ExitCode != 3 {
			t.Fatalf("result %+v", r)
		}
		tr.ev.awaitMatch(t, evOutputWritten, func(ev event) bool { return ev.id == rid })
		tr.clk.Advance(outputExchange)
		tr.ev.await(t, evEnded)
		tr.logs.await(t, "result receipt unconfirmed", func(s string) bool { return strings.Contains(s, "result_receipt_unconfirmed") })
		tr.advanceBackoff(t, backoff(0, 0.5))
		s = tr.connect(t, 2, 2, roleConfig("a", ins, run))
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 2); hb.Roles[0].Inflight != 0 {
			t.Fatalf("slot after the lost ack: %+v", hb.Roles)
		}
		tr.clk.Advance(heartbeatInterval)
		s.beat(t, 2)
		tr.ev.awaitCollected(t, st.TaskID)
		if tr.super(t).find(st.TaskID) != nil {
			t.Fatal("the unconfirmed result is still held for sending")
		}
	})
}

// gatedFactory holds the next child's Start until released.
type gatedFactory struct {
	block   chan struct{}
	arrived chan struct{}
	once    sync.Once
}

func (g *gatedFactory) wrap(next procFactory) procFactory {
	return func(spec procSpec) taskProc { return &gatedProc{taskProc: next(spec), g: g} }
}

type gatedProc struct {
	taskProc
	g *gatedFactory
}

func (p *gatedProc) Start() error {
	p.g.once.Do(func() { close(p.g.arrived) })
	<-p.g.block
	return p.taskProc.Start()
}
