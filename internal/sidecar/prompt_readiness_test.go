package sidecar

import (
	"bytes"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 10a, UT-A1 (FP-1): prompt readiness reports on the session's
// ordinary heartbeat exchange. Every subtest acknowledges heartbeats
// itself (the fake plane's manual mode), so the exact frame order, the one
// outstanding request and each report's simulated send instant (the
// heartbeat's evAwaitReply event) are observed directly. The fake clock
// moves only after the event it tests was observed.

// manualRoleRun is a role run whose first connection is in the manual
// acknowledgement mode, connected (b1 acknowledged and processed) and
// reconciled.
func manualRoleRun(t *testing.T, adjust func(d *deps)) (*roleRun, *fakeConn) {
	t.Helper()
	fp := startFakePlane(t)
	rr := startRoleRunWith(t, fp, true, adjust)
	c := fp.accept(t)
	c.setManual()
	c.helloOK(testID)
	rr.beat(c, 1, 0)
	c.reconcileEmpty()
	return rr, c
}

// scan consumes the main event stream until match accepts an event (every
// kind is shown to it) and returns that event.
func (e *events) scan(t *testing.T, match func(event) bool) event {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case ev := <-e.ch:
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("an awaited event never arrived")
			return event{}
		}
	}
}

// sent returns heartbeat k's write event (its send instant and whether it
// was an immediate report), failing if a later request's write comes
// first.
func (e *events) sent(t *testing.T, k int) event {
	t.Helper()
	return e.scan(t, func(ev event) bool {
		if ev.kind == evAwaitReply && ev.acks > k {
			t.Fatalf("heartbeat b%d was written before b%d", ev.acks, k)
		}
		return ev.kind == evAwaitReply && ev.acks == k
	})
}

// ackedBefore requires heartbeat k's acknowledgement to be processed
// before any later heartbeat is written (one outstanding request).
func (e *events) ackedBefore(t *testing.T, k int) {
	t.Helper()
	e.scan(t, func(ev event) bool {
		if ev.kind == evAwaitReply && ev.acks > k {
			t.Fatalf("heartbeat b%d was written while b%d was outstanding", ev.acks, k)
		}
		return ev.kind == evAck && ev.acks == k
	})
}

// startManual (manual acknowledgement mode) sends task n's start on role
// i and reads its reply and, in either order, the occupied slot's report
// of inflight (acknowledged); it returns the started child.
func (s *taskSession) startManual(t *testing.T, n, i int, goal string, inflight int) (contract.TaskStartBody, *fakeChild) {
	t.Helper()
	b := startBody(n, s.cfgs[i], i+1, s.gen, goal)
	rid := s.nextP()
	s.c.sendStart(rid, b)
	var replied, reported bool
	for !replied || !reported {
		f := s.c.recv()
		switch f.Type {
		case contract.FrameTaskStartResult:
			r, err := contract.DecodeTaskStartResult(f.Body)
			if err != nil || f.RequestID != rid || r.Err != nil {
				t.Fatalf("start %s: %+v %v", f.RequestID, r, err)
			}
			replied = true
		case contract.FrameHeartbeat:
			s.c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, f.RequestID, nil)
			k, _ := strconv.Atoi(f.RequestID[1:])
			s.b = max(s.b, k+1)
			hb, err := contract.DecodeHeartbeat(f.Body)
			if err != nil {
				t.Fatal(err)
			}
			reported = reported || hb.Roles[0].Inflight == inflight
		default:
			t.Fatalf("unexpected %s %s", f.Type, f.RequestID)
		}
	}
	ch := s.tr.child(t)
	s.tr.ev.awaitMatch(t, evStartReplied, func(ev event) bool { return ev.id == b.TaskID })
	return b, ch
}

// wantReport checks heartbeat k's write: an immediate report (or not) sent
// at exactly the simulated instant at.
func wantReport(t *testing.T, ev event, prompt bool, at time.Time) {
	t.Helper()
	if ev.prompt != prompt || !ev.at.Equal(at) {
		t.Fatalf("heartbeat b%d: report %v at %v, want report %v at %v (latency %v simulated)", ev.acks, ev.prompt, ev.at, prompt, at, ev.at.Sub(at))
	}
}

// TestPromptReadinessContract is UT-A1 of iteration 10a (FP-1).
func TestPromptReadinessContract(t *testing.T) {
	t.Parallel()
	t.Run("cycle-latency", func(t *testing.T) {
		t.Parallel()
		// A new revision is reported at once, all false, before its first
		// cycle; the passed cycle is reported at the same simulated instant
		// its completion was observed (0 ms), without moving toward the
		// 5 s heartbeat deadline; the periodic heartbeat follows exactly
		// heartbeatInterval after that report, and the cycle repeating an
		// identical result in between sends nothing.
		rr, c := manualRoleRun(t, nil)
		block := make(chan struct{})
		rr.script.set(nil, block)
		ins, run := manuals(t, rr.dir, "a", "x")
		c.replace("p1", 1, roleConfig("a", ins, run))
		c.expectReplaceAck("p1", 1)
		rr.script.awaitProbe(t)
		installedAt := rr.clk.Now()
		if s := statuses(c.heartbeatAt(2, 1)); s["a"] || len(s) != 1 {
			t.Fatalf("new revision report %v", s)
		}
		wantReport(t, rr.ev.sent(t, 2), true, installedAt)
		rr.ev.ackedBefore(t, 2)
		rr.clk.Advance(time.Second) // the cycle is still held (T+1s)
		close(block)
		if done := rr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 1 }); !done.passed[0] {
			t.Fatalf("cycle %v", done.passed)
		}
		doneAt := rr.clk.Now() // armed at the observed completion
		if s := statuses(c.heartbeatAt(3, 1)); !s["a"] {
			t.Fatalf("ready report %v", s)
		}
		wantReport(t, rr.ev.sent(t, 3), true, doneAt)
		rr.ev.ackedBefore(t, 3)
		rr.script.set(nil, nil)
		// The next cycle (5 s after its predecessor's start, T+5s) passes
		// again: identical, no report.
		if err := rr.clk.AwaitWaiter(testWait, func(ws []testkit.Waiter) bool {
			for _, w := range ws {
				if w.At.Equal(doneAt.Add(heartbeatInterval)) {
					return true
				}
			}
			return false
		}); err != nil {
			t.Fatalf("no periodic heartbeat armed at %v: %v", doneAt.Add(heartbeatInterval), err)
		}
		rr.clk.Advance(heartbeatInterval - time.Second) // T+5s: the cycle
		if done := rr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 1 }); !done.passed[0] {
			t.Fatalf("second cycle %v", done.passed)
		}
		rr.clk.Advance(time.Second) // T+6s: the periodic heartbeat
		if s := statuses(c.heartbeatAt(4, 1)); !s["a"] {
			t.Fatalf("periodic %v", s)
		}
		wantReport(t, rr.ev.sent(t, 4), false, doneAt.Add(heartbeatInterval))
	})
	t.Run("freshness", func(t *testing.T) {
		t.Parallel()
		// A passed cycle going stale is reported at the instant it does,
		// between two periodic heartbeats (no further cycle runs here).
		rr, c := manualRoleRun(t, func(d *deps) { d.noCadence = true })
		block := make(chan struct{})
		rr.script.set(nil, block)
		ins, run := manuals(t, rr.dir, "a", "x")
		c.replace("p1", 1, roleConfig("a", ins, run))
		c.expectReplaceAck("p1", 1)
		rr.script.awaitProbe(t)
		start := rr.clk.Now() // the cycle's start: fresh until start+10s
		rr.beat(c, 2, 1)
		rr.clk.Advance(time.Second)
		close(block)
		rr.awaitCycle(t, 1)
		if s := statuses(rr.beat(c, 3, 1)); !s["a"] {
			t.Fatalf("ready %v", s)
		}
		rr.clk.Advance(heartbeatInterval) // T+6s: periodic, still fresh
		if s := statuses(rr.beat(c, 4, 1)); !s["a"] {
			t.Fatalf("periodic %v", s)
		}
		rr.clk.Advance(freshness - heartbeatInterval - time.Second) // T+10s
		if s := statuses(c.heartbeatAt(5, 1)); s["a"] {
			t.Fatalf("stale report %v", s)
		}
		wantReport(t, rr.ev.sent(t, 5), true, start.Add(freshness))
	})
	t.Run("coalesce", func(t *testing.T) {
		t.Parallel()
		// Reconciliation (an empty snapshot, unchanged) and an
		// acknowledgement are no trigger: the next heartbeat is the
		// periodic one. Changes while a heartbeat is outstanding (a passed
		// cycle, then a new revision) write nothing; its acknowledgement
		// alone is no trigger, but the changed snapshot then goes at once,
		// coalesced to the latest values computed at the send; a lost
		// acknowledgement ends the session without a second outstanding
		// request. The next session starts with its periodic first
		// heartbeat at revision 0, then reports its snapshot at once.
		rr, c := manualRoleRun(t, nil)
		at := rr.clk.Now()
		rr.clk.Advance(heartbeatInterval)
		c.heartbeatAt(2, 0)
		wantReport(t, rr.ev.sent(t, 2), false, at.Add(heartbeatInterval))
		rr.ev.ackedBefore(t, 2)
		block := make(chan struct{})
		rr.script.set(nil, block)
		ins, run := manuals(t, rr.dir, "a", "x")
		a := roleConfig("a", ins, run)
		c.replace("p1", 1, a)
		c.expectReplaceAck("p1", 1)
		rr.script.awaitProbe(t)
		if s := statuses(c.readHeartbeat(3)); s["a"] {
			t.Fatalf("b3 %v", s)
		}
		rr.ev.sent(t, 3)
		close(block)
		if p := rr.awaitCycle(t, 1); !p[0] {
			t.Fatalf("cycle %v", p)
		}
		hold := make(chan struct{})
		rr.script.set(nil, hold)
		c.replace("p2", 2, a)
		c.expectReplaceAck("p2", 2)
		rr.script.awaitProbe(t)
		c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b3", nil)
		rr.ev.ackedBefore(t, 3)
		// One report: the latest revision, all false (its cycle is held).
		b := c.readHeartbeat(4)
		if b.RolesRevision != 2 || statuses(b)["a"] {
			t.Fatalf("coalesced report %+v", b)
		}
		if ev := rr.ev.sent(t, 4); !ev.prompt {
			t.Fatal("the coalesced snapshot was not an immediate report")
		}
		// Lost: nothing else is written; the exchange's bound ends the
		// session.
		rr.clk.Advance(stepTimeout)
		ended := rr.ev.await(t, evEnded)
		wantCode(t, ended.err, contract.CodeUnavailable, "no heartbeat acknowledgement within 5s")
		for {
			r := c.next()
			if r.err != nil {
				break
			}
			if f, err := contract.DecodeFrame(r.data, contract.FromSidecar); err == nil && f.Type == contract.FrameHeartbeat {
				t.Fatalf("a second heartbeat %s while b4 was outstanding", f.RequestID)
			}
		}
		close(hold)
		rr.advanceBackoff(t, jitterDelay(0))
		c2 := rr.fp.accept(t)
		c2.setManual()
		c2.helloOK(testID)
		if b := rr.beat(c2, 1, 0); len(b.Roles) != 0 {
			t.Fatalf("first heartbeat %+v", b)
		}
		c2.reconcileEmpty()
		rr.script.set(nil, make(chan struct{})) // held: canceled when Run ends
		c2.replace("p1", 7, a)
		c2.expectReplaceAck("p1", 7)
		c2.heartbeatAt(2, 7)
		wantReport(t, rr.ev.sent(t, 2), true, rr.clk.Now())
	})
	t.Run("fairness", func(t *testing.T) {
		t.Parallel()
		// While task output is ready, at most one immediate report goes
		// between two output exchanges, however often the status changes;
		// exactly one request is outstanding at a time.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		fp.mu.Lock()
		fp.autoAck = false
		fp.mu.Unlock()
		block := make(chan struct{})
		tr.script.set(nil, block)
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		cfg.Concurrency = 4
		s := tr.connect(t, 1, 1, cfg)
		s.c.heartbeatAt(2, 1)
		close(block)
		tr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 1 })
		s.c.heartbeatAt(3, 1)
		s.b = 4
		st1, ch1 := s.startManual(t, 1, 0, "output", 1)
		ch1.prompt(t)
		ch1.out(bytes.Repeat([]byte("o"), 3*contract.MaxLogChunkBytes))
		// All three chunks are in the ring: output stays ready throughout.
		w := tr.super(t).find(st1.TaskID)
		deadline := time.Now().Add(testWait)
		for len(w.ring.retained()) < 3*contract.MaxLogChunkBytes {
			if time.Now().After(deadline) {
				t.Fatal("the output never reached the ring")
			}
			time.Sleep(time.Millisecond)
		}
		rid, lb := s.nextLog(t) // b5, held
		_, ch2 := s.run(t, 2, 0, "second")
		s.c.ackLog(rid, st1.TaskID, lb.Offset+len(lb.Data))
		// The changed snapshot goes first (b6, held), output still ready.
		f := s.request(t)
		if b, _ := contract.DecodeHeartbeat(f.Body); f.Type != contract.FrameHeartbeat || b.Roles[0].Inflight != 2 {
			t.Fatalf("after the log: %s %s", f.Type, f.Body)
		}
		_, ch3 := s.run(t, 3, 0, "third")
		s.c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, f.RequestID, nil)
		// Changed again, but a report was the last exchange: output first.
		rid, lb = s.nextLog(t)
		s.c.ackLog(rid, st1.TaskID, lb.Offset+len(lb.Data))
		f = s.request(t)
		if b, _ := contract.DecodeHeartbeat(f.Body); f.Type != contract.FrameHeartbeat || b.Roles[0].Inflight != 3 {
			t.Fatalf("after the second log: %s %s", f.Type, f.Body)
		}
		s.c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, f.RequestID, nil)
		for _, ch := range []*fakeChild{ch2, ch3} {
			ch.prompt(t)
		}
	})
}

// benchEventBound bounds the recorder's main queue between two benchmark
// iterations (each iteration's events are discarded at its end).
const benchEventBound = 256

// BenchmarkPromptReadiness measures iteration 10a's immediate reports end
// to end through the fake plane's TLS stream: event-to-send, the session's
// own wall time from a snapshot's acknowledgement write (the status
// change) to the report's write, per change (aggregated as it runs); and,
// with one heartbeat's acknowledgement withheld, the frames per update
// while K changes coalesce (exactly one report of the latest snapshot: the
// pending report never queues). The recorder's queues are drained every
// iteration and must stay bounded, so any -benchtime runs.
// awaitRecorded waits until the benchmark's tap has counted revision
// rev's report; a revision it never counts fails here, as at the total.
func awaitRecorded(b *testing.B, recorded <-chan int, rev int) {
	b.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case r := <-recorded:
			if r == rev {
				return
			}
		case <-deadline:
			b.Fatalf("revision %d's report was never recorded", rev)
		}
	}
}

func BenchmarkPromptReadiness(b *testing.B) {
	b.Run("event-to-send", func(b *testing.B) {
		var mu sync.Mutex
		var ackRev, reported int
		var ackAt time.Time
		var total time.Duration
		// recorded carries each revision whose report the tap counted. The
		// session emits evAwaitReply only after the report's write
		// returned, so the plane can see the frame before the tap ran:
		// every iteration waits here for its own revision before the next
		// one (and before the total is checked). One revision is counted at
		// most once and only after the previous one was received, so the
		// send never finds the buffer full.
		recorded := make(chan int, 1)
		tap := func(ev event) {
			now := time.Now()
			mu.Lock()
			defer mu.Unlock()
			switch {
			case ev.kind == evAckWritten:
				ackRev, ackAt = ev.rev, now
			case ev.kind == evAwaitReply && ev.prompt && ev.rev == ackRev && !ackAt.IsZero():
				total += now.Sub(ackAt)
				reported++
				ackAt = time.Time{} // the first report of this revision only
				select {
				case recorded <- ev.rev:
				default:
				}
			}
		}
		fp := startFakePlane(b)
		rr := startRoleRunWith(b, fp, true, func(d *deps) { d.observe = tap })
		c := fp.accept(b)
		c.connect()
		ins, run := manuals(b, rr.dir, "a", "x")
		a := roleConfig("a", ins, run)
		rev, hold := 0, make(chan struct{})
		for b.Loop() {
			rr.script.set(nil, hold) // held probes, no cycle report; drains their starts
			rev++
			c.replace("p"+strconv.Itoa(rev), rev, a)
			c.expectReplaceAck("p"+strconv.Itoa(rev), rev)
			c.awaitLatest(func(o beatObs) bool { return o.body.RolesRevision == rev })
			awaitRecorded(b, recorded, rev)
			if n := rr.ev.discard(); n > benchEventBound {
				b.Fatalf("%d events queued in one iteration", n)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if reported != rev {
			b.Fatalf("%d of %d revisions were reported at once", reported, rev)
		}
		b.ReportMetric(float64(total.Nanoseconds())/float64(rev), "event-to-send-ns")
	})
	b.Run("coalesced", func(b *testing.B) {
		const k = 8
		fp := startFakePlane(b)
		rr := startRoleRunWith(b, fp, true, nil)
		c := fp.accept(b)
		c.setManual()
		c.helloOK(testID)
		rr.beat(c, 1, 0)
		c.reconcileEmpty()
		ins, run := manuals(b, rr.dir, "a", "x")
		a := roleConfig("a", ins, run)
		rev, p, beat, frames, updates := 0, 0, 1, 0, 0
		hold := make(chan struct{})
		for b.Loop() {
			rr.script.set(nil, hold) // held probes: no cycle report; drained
			// One report outstanding, its acknowledgement withheld.
			rev++
			p++
			c.replace("p"+strconv.Itoa(p), rev, a)
			c.expectReplaceAck("p"+strconv.Itoa(p), rev)
			beat++
			held := "b" + strconv.Itoa(beat)
			if hb := c.readHeartbeat(beat); hb.RolesRevision != rev {
				b.Fatalf("%s at %d, want %d", held, hb.RolesRevision, rev)
			}
			for range k {
				rev++
				p++
				c.replace("p"+strconv.Itoa(p), rev, a)
				c.expectReplaceAck("p"+strconv.Itoa(p), rev)
				updates++
			}
			c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, held, nil)
			beat++
			if hb := c.readHeartbeat(beat); hb.RolesRevision != rev {
				b.Fatalf("coalesced b%d at %d, want the latest %d", beat, hb.RolesRevision, rev)
			}
			frames++
			c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b"+strconv.Itoa(beat), nil)
			rr.ev.awaitMatch(b, evAck, func(ev event) bool { return ev.acks == beat })
			if n := rr.ev.discard(); n > benchEventBound {
				b.Fatalf("%d events queued in one iteration", n)
			}
		}
		if frames != b.N || updates != k*b.N {
			b.Fatalf("%d reports for %d updates", frames, updates)
		}
		b.ReportMetric(float64(frames)/float64(updates), "frames/update")
	})
}
