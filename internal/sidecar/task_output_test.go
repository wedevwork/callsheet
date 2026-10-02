package sidecar

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestTaskOutput is UT FP-5 for the worker's output buffering; its
// retention, backpressure and final subtests are delegated from
// tests/function (TestTaskLogs), together with the plane's. Do not rename
// or skip them.
func TestTaskOutput(t *testing.T) {
	t.Parallel()
	t.Run("retention", func(t *testing.T) {
		t.Parallel()
		// The ring retains at most its cap of unsent output with absolute
		// offsets, evicting the oldest; the in-flight copy is immutable;
		// eviction past it leaves a gap the next chunk's offset shows; the
		// counter saturates at its bound, incomplete and overflowed.
		r := newOutputRing(64)
		r.write([]byte(strings.Repeat("a", 40)))
		c := r.next()
		if c == nil || c.offset != 0 || len(c.data) != 40 || r.next() != nil {
			t.Fatalf("first chunk %+v", c)
		}
		r.write([]byte(strings.Repeat("b", 60)))
		if string(c.data) != strings.Repeat("a", 40) {
			t.Fatal("eviction mutated the in-flight copy")
		}
		// Bytes already acknowledged leave no gap.
		r.ack(40)
		c = r.next()
		if c == nil || c.offset != 40 || string(c.data) != strings.Repeat("b", 60) {
			t.Fatalf("after eviction %+v", c)
		}
		// Unsent bytes evicted while a chunk is in flight: the next chunk
		// starts at the oldest survivor, beyond a gap.
		r.write([]byte(strings.Repeat("c", 20)))
		r.ack(100)
		if c = r.next(); c == nil || c.offset != 100 || string(c.data) != strings.Repeat("c", 20) {
			t.Fatalf("after the in-flight chunk %+v", c)
		}
		r.write([]byte(strings.Repeat("d", 60)))
		r.write([]byte(strings.Repeat("e", 30)))
		r.ack(120)
		if c = r.next(); c == nil || c.offset != 146 || string(c.data) != strings.Repeat("d", 34)+strings.Repeat("e", 30) {
			t.Fatalf("gap chunk %+v", c)
		}
		r.ack(210)
		if !r.drained() || r.pending() {
			t.Fatal("not drained")
		}
		end, inc, over := r.totals()
		if end != 210 || inc || over {
			t.Fatalf("totals %d %v %v", end, inc, over)
		}
		// A single write larger than the cap keeps its suffix.
		r.write([]byte("0123456789" + strings.Repeat("x", 64)))
		if got := r.retained(); len(got) != 64 || got[0] != 'x' {
			t.Fatalf("oversized write kept %q", got)
		}
		// Counter saturation.
		sat := newOutputRing(64)
		sat.limit = 10
		sat.write([]byte("0123456"))
		sat.write([]byte("789abc"))
		sat.write([]byte("more"))
		if end, inc, over := sat.totals(); end != 10 || !inc || !over || string(sat.retained()) != "0123456789" {
			t.Fatalf("saturated %d %v %v %q", end, inc, over, sat.retained())
		}
		// 10 MiB: memory stays bounded and the suffix is exact.
		big := newOutputRing(contract.MaxLogRetainedBytes)
		total := contract.MaxLogRetainedBytes + 5*contract.MaxLogChunkBytes + 3
		stream := patterned(total)
		for off := 0; off < total; {
			n := min(contract.MaxLogChunkBytes, total-off)
			big.write(stream[off : off+n])
			off += n
		}
		got := big.retained()
		if len(got) != contract.MaxLogRetainedBytes || cap(big.buf) > contract.MaxLogRetainedBytes || !bytes.Equal(got, stream[total-contract.MaxLogRetainedBytes:]) {
			t.Fatalf("retained %d cap %d: not the exact suffix", len(got), cap(big.buf))
		}
		if c := big.next(); c.offset != total-contract.MaxLogRetainedBytes || len(c.data) != contract.MaxLogChunkBytes {
			t.Fatalf("oldest surviving chunk at %d", c.offset)
		}
		// Through a session: both pipes drain into one merged stream, per
		// pipe in order, byte exact.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st, ch := s.run(t, 1, 0, "both pipes")
		ch.prompt(t)
		ch.out([]byte{0, 0xff, '\r', 0x1b})
		ch.errOut([]byte("err\n"))
		ch.exitCode(1)
		out := s.logs(t, st, 8)
		if !bytes.Contains(out, []byte{0, 0xff, '\r', 0x1b}) || !bytes.Contains(out, []byte("err\n")) || len(out) != 8 {
			t.Fatalf("merged %q", out)
		}
		if r := s.result(t, st); r.OutputBytes != 8 || r.LogIncomplete {
			t.Fatalf("result %+v", r)
		}
	})
	t.Run("backpressure", func(t *testing.T) {
		t.Parallel()
		// A receiver that stops acknowledging never blocks the child: its
		// pipes keep draining into the bounded ring while the only chunk
		// in flight waits; the child exits; the result waits for the
		// output, which resumes from the oldest surviving byte (a gap).
		const ringCap = 64 << 10 // the 10 MiB constant itself is retention's
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) { d.taskRingCap = ringCap }})
		ins, run := manuals(t, tr.dir, "a", "m")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st, ch := s.run(t, 1, 0, "flood")
		ch.prompt(t)
		ch.out([]byte("start\n"))
		rid, first := s.nextLog(t)
		if first.Offset != 0 {
			t.Fatalf("first chunk %+v", first.Offset)
		}
		total := len(first.Data)
		flood := bytes.Repeat([]byte("f"), contract.MaxLogChunkBytes)
		for total < 1<<20 {
			ch.out(flood) // never blocks: the pipe drains into the ring
			total += len(flood)
		}
		ch.exitCode(0)
		tr.settled(t, st.TaskID)
		w := tr.super(t).find(st.TaskID)
		if w == nil || len(w.ring.retained()) != ringCap || cap(w.ring.buf) > ringCap {
			t.Fatal("the ring grew beyond its cap")
		}
		s.c.ackLog(rid, st.TaskID, len(first.Data))
		// The next chunk jumps over the evicted bytes.
		rid, next := s.nextLog(t)
		if next.Offset != total-ringCap {
			t.Fatalf("resumed at %d, want %d", next.Offset, total-ringCap)
		}
		sent := next.Offset + len(next.Data)
		s.c.ackLog(rid, st.TaskID, sent)
		for sent < total {
			rid, lb := s.nextLog(t)
			sent = lb.Offset + len(lb.Data)
			s.c.ackLog(rid, st.TaskID, sent)
		}
		if r := s.result(t, st); r.OutputBytes != total || r.LogIncomplete {
			t.Fatalf("result %+v", r)
		}
	})
	t.Run("final", func(t *testing.T) {
		t.Parallel()
		// After the exit the surviving output is flushed before the
		// result; the final marker survives log truncation (extraction
		// sees every stdout byte first) and stderr never supplies it; a
		// descendant holding the pipes past the one-second drain bound
		// cuts the log off, incomplete.
		const ringCap = 64 << 10
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) { d.taskRingCap = ringCap }})
		ins, run := manuals(t, tr.dir, "a", "m")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st, ch := s.run(t, 1, 0, "marker first")
		ch.prompt(t)
		ch.errOut([]byte(`{"type":"callsheet_final","message":"from stderr"}` + "\n"))
		rid, first := s.nextLog(t)
		ch.out([]byte(`{"type":"callsheet_final","message":"kept"}` + "\n"))
		ch.out(bytes.Repeat([]byte("x"), ringCap+1))
		ch.exitCode(0)
		tr.settled(t, st.TaskID)
		s.c.ackLog(rid, st.TaskID, len(first.Data))
		r, out := s.drain(t, st)
		if r.FinalMessage == nil || *r.FinalMessage != "kept" || len(out) != ringCap || bytes.Contains(out, []byte("kept")) {
			t.Fatalf("lost marker or output: %+v (%d bytes)", r, len(out))
		}
		// Descendant holds the pipes: the bound closes them.
		st2, ch2 := s.run(t, 2, 0, "holder")
		ch2.prompt(t)
		ch2.out([]byte("before exit\n"))
		s.logs(t, st2, 12)
		tr.ev.awaitMatch(t, evLogAcked, func(ev event) bool { return ev.id == st2.TaskID })
		ch2.exitHolding(0)
		tr.ev.awaitMatch(t, evChildExited, func(ev event) bool { return ev.id == st2.TaskID })
		// No write is in flight: the bound is the only new timer.
		if err := tr.clk.AwaitWaiter(testWait, testkit.HasTimer(drainBound)); err != nil {
			t.Fatal(err)
		}
		tr.clk.Advance(drainBound)
		tr.settled(t, st2.TaskID)
		ch2.spec.stdout.Close()
		ch2.spec.stderr.Close()
		if r := s.result(t, st2); !r.LogIncomplete || *r.ExitCode != 0 || r.OutputBytes != 12 {
			t.Fatalf("held pipes result %+v", r)
		}
	})
}

// BenchmarkTaskLogTail measures 16 KiB writes crossing the 10 MiB cap:
// steady-state insertion allocates nothing proportional to lifetime
// output, and the ring never exceeds its cap.
func BenchmarkTaskLogTail(b *testing.B) {
	r := newOutputRing(contract.MaxLogRetainedBytes)
	chunk := bytes.Repeat([]byte("0123456789abcdef"), contract.MaxLogChunkBytes/16)
	for range contract.MaxLogRetainedBytes / len(chunk) {
		r.write(chunk)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		r.write(chunk)
	}
	if n := len(r.retained()); n != contract.MaxLogRetainedBytes || cap(r.buf) != contract.MaxLogRetainedBytes {
		b.Fatalf("retained %d, capacity %d", n, cap(r.buf))
	}
	if allocs := testing.AllocsPerRun(100, func() { r.write(chunk) }); allocs != 0 {
		b.Fatalf("steady-state insertion allocates %v times", allocs)
	}
}

// patterned returns n bytes of a period-253 pattern (bulk-built).
func patterned(n int) []byte {
	period := make([]byte, 253)
	for i := range period {
		period[i] = byte(i)
	}
	return bytes.Repeat(period, n/253+1)[:n]
}
