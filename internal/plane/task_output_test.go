package plane

import (
	"bytes"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestTaskOutput is UT FP-5 for the plane's output receipt: bounded
// retention, receipt independent of disk, and the final flush; its
// retention, backpressure and final subtests are delegated from
// tests/function (TestTaskLogs), together with the sidecar's. Do not
// rename or skip them.
func TestTaskOutput(t *testing.T) {
	t.Parallel()
	t.Run("retention", func(t *testing.T) {
		t.Parallel()
		// The ring keeps exactly the newest 10 MiB of received bytes
		// (evicting leading bytes, partial lines included); offsets are
		// absolute; a gap is incomplete, a duplicate range idempotent, a
		// partial overlap refused.
		l := newPlaneLog()
		total := contract.MaxLogRetainedBytes + 3*contract.MaxLogChunkBytes + 7
		period := make([]byte, 251)
		for i := range period {
			period[i] = byte(i)
		}
		// The stream is periodic, so the bytes at any offset are a slice
		// of one chunk-plus-period window (no full-size copy is built).
		window := bytes.Repeat(period, contract.MaxLogChunkBytes/251+2)
		at := func(off, n int) []byte { return window[off%251 : off%251+n] }
		for off := 0; off < total; {
			n := min(contract.MaxLogChunkBytes, total-off)
			next, err := l.append(off, at(off, n))
			if err != nil || next != off+n {
				t.Fatalf("append at %d: %d %v", off, next, err)
			}
			off += n
		}
		lg := l.snapshot()
		if len(lg.Data) != contract.MaxLogRetainedBytes || lg.SourceBytes != total || lg.ReceivedBytes != total || lg.Incomplete {
			t.Fatalf("retained %d source %d received %d", len(lg.Data), lg.SourceBytes, lg.ReceivedBytes)
		}
		for i, base := 0, total-contract.MaxLogRetainedBytes; i < len(lg.Data); i += contract.MaxLogChunkBytes {
			n := min(contract.MaxLogChunkBytes, len(lg.Data)-i)
			if !bytes.Equal(lg.Data[i:i+n], at(base+i, n)) {
				t.Fatalf("the retained bytes are not the exact suffix (at %d)", i)
			}
		}
		if cap(l.buf) > contract.MaxLogRetainedBytes {
			t.Fatalf("ring capacity %d", cap(l.buf))
		}
		if next, err := l.append(total-10, []byte("0123456789")); err != nil || next != total {
			t.Fatalf("duplicate: %d %v", next, err)
		}
		if _, err := l.append(total-5, []byte("0123456789")); err == nil {
			t.Fatal("a partial overlap was accepted")
		}
		if next, _ := l.append(total+100, []byte("tail")); next != total+104 || !l.incomplete || l.received != total+4 || l.source != total+104 {
			t.Fatal("a gap was not accounted")
		}
		if err := l.snapshot().Validate(); err != nil {
			t.Fatalf("snapshot invariants: %v", err)
		}
		// Over the stream: binary output stays byte exact through logs.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
		w := tp.worker(t, idA)
		st := tp.run(t, w, "a", "binary", "p2")
		bin := []byte{0, 1, 0x1b, '[', '2', 'J', 0xff, 0xfe, '\r', '\n', 0x7f}
		w.log(st, 0, bin)
		w.log(st, 20, []byte("after gap\n"))
		logs, err := tp.cl.TaskLogs(bg, st.TaskID)
		if err != nil || !bytes.Equal(logs.Data, append(append([]byte{}, bin...), "after gap\n"...)) || !logs.Incomplete || logs.SourceBytes != 30 {
			t.Fatalf("logs %+v %v", logs, err)
		}
		if v := tp.show(t, st.TaskID); !v.Log.Incomplete || !v.Log.Truncated || v.Log.ReceivedBytes != 21 {
			t.Fatalf("gap meta %+v", v.Log)
		}
	})
	t.Run("backpressure", func(t *testing.T) {
		t.Parallel()
		// A checkpoint held before its first syscall never blocks receipt:
		// output keeps being acknowledged into bounded memory, and the
		// held checkpoint publishes its own immutable snapshot, never the
		// newer live bytes.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 1), 1)}, idA)
		w := tp.worker(t, idA)
		// The output is acknowledged before running is published, so the
		// writer's pass after running takes the first checkpoint (due at
		// once) and the hold catches it; acknowledged after running, that
		// pass could publish it unheld before the hook was armed, leaving
		// nothing dirty for a clock advance to queue.
		st, running := tp.runHeld(t, w, "a", "slow disk", "p2")
		w.log(st, 0, []byte("first\n"))
		hold := tp.th.arm("checkpoint-queued", st.TaskID)
		running()
		call := paused(t, hold, "checkpoint-queued")
		off := 6
		for i := 0; i < 50; i++ {
			off = w.log(st, off, []byte("more\n"))
		}
		w.beat()
		if v := tp.show(t, st.TaskID); v.Log.ReceivedBytes != off || !bytes.HasSuffix([]byte(v.LogTail), []byte("more\n")) {
			t.Fatalf("receipt waited for the disk: %+v", v.Log)
		}
		close(call.release)
		tp.log.await(t, "published checkpoint "+st.TaskID)
		if r := tp.taskFile(t, st.TaskID); string(r.Log.Data) != "first\n" || r.Log.SourceBytes != 6 {
			t.Fatalf("the checkpoint took newer bytes: %q", r.Log.Data)
		}
		if v := tp.show(t, st.TaskID); v.Log.ReceivedBytes != off {
			t.Fatal("the checkpoint replaced newer live bytes")
		}
		// The first checkpoint was selected at the current instant: the
		// next is due one second later, and the newer bytes are dirty.
		tp.clk.Advance(checkpointEvery)
		tp.log.await(t, "published checkpoint "+st.TaskID)
		if r := tp.taskFile(t, st.TaskID); r.Log.SourceBytes != off {
			t.Fatalf("second checkpoint %d", r.Log.SourceBytes)
		}
	})
	t.Run("final", func(t *testing.T) {
		t.Parallel()
		// Output then result: the terminal document's tail is exactly the
		// received retained bytes; a result reporting more bytes than
		// arrived is a trailing gap (incomplete); output after the result
		// is refused; the final message and tail are consistent.
		tp := startTaskPlane(t, nil, []contract.RoleRecord{record(taskCfg("a", "coder", idA, 2), 1)}, idA)
		w := tp.worker(t, idA)
		st := tp.run(t, w, "a", "complete", "p2")
		out := []byte("line one\n{\"type\":\"callsheet_final\",\"message\":\"m\"}\n")
		w.log(st, 0, out)
		msg := "m"
		w.resultAck(w.sendResult(result(st, 0, len(out), &msg)), st.TaskID)
		tp.log.awaitOnce(t, "terminal-committed "+st.TaskID)
		r := tp.taskFile(t, st.TaskID)
		if !bytes.Equal(r.Log.Data, out) || r.Log.Incomplete || r.Log.SourceBytes != len(out) || *r.FinalMessage != "m" {
			t.Fatalf("complete terminal log %+v", r.Log)
		}
		v := tp.show(t, st.TaskID)
		if v.LogTail != string(out) || v.Result.LogTail != v.LogTail || v.Log.Truncated {
			t.Fatalf("view %+v", v)
		}
		st2 := tp.run(t, w, "a", "trailing gap", "p3")
		w.log(st2, 0, []byte("partial"))
		res := result(st2, 1, 100, nil)
		res.LogIncomplete = true
		rid := w.sendResult(res.Sealed())
		w.resultAck(rid, st2.TaskID)
		expectClosed(t, w, w.sendLog(st2, 7, []byte("late")))
		tp.log.awaitOnce(t, "terminal-committed "+st2.TaskID)
		r = tp.taskFile(t, st2.TaskID)
		if !r.Log.Incomplete || r.Log.SourceBytes != 100 || r.Log.ReceivedBytes != 7 || string(r.Log.Data) != "partial" {
			t.Fatalf("trailing gap %+v", r.Log)
		}
		if v := tp.show(t, st2.TaskID); !v.Log.Incomplete || v.Log.DroppedBytes != 93 || v.State != contract.TaskFailed {
			t.Fatalf("trailing gap view %+v", v.Log)
		}
		// A result reporting fewer bytes than arrived is refused.
		w = tp.worker(t, idA)
		st3 := tp.run(t, w, "a", "short", "p2")
		w.log(st3, 0, []byte("12345"))
		expectClosed(t, w, w.sendResult(result(st3, 0, 3, nil)))
		if v := tp.show(t, st3.TaskID); v.State != contract.TaskRunning || v.CompletionPending {
			t.Fatalf("short result accepted: %+v", v)
		}
	})
}
