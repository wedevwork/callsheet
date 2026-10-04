package plane

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b TestControlWait (FP-3) and BenchmarkControlWait. Waits run
// through the verified client against the served plane; their server-side
// deadline runs on the fake node clock and is advanced only after the
// wait-registered event.

// waitAsync waits on ids through the client.
func (tp *taskPlane) waitAsync(ctx context.Context, ids []string, d time.Duration) *call[contract.WaitResponse] {
	return async(func() (contract.WaitResponse, error) { return tp.cl.WaitTasks(ctx, ids, d) })
}

// registered waits until a registration naming ids exists.
func (tp *taskPlane) registered(t *testing.T, ids ...string) {
	t.Helper()
	tp.log.awaitOnce(t, "wait-registered "+strings.Join(ids, ","))
}

// waitCount is the active registrations and reserved capacity.
func (tp *taskPlane) waitCount(t *testing.T) int {
	t.Helper()
	ts := tp.svc(t)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := len(ts.waits) + ts.waitReserve
	for _, e := range ts.tasks {
		n += len(e.waitRegs)
	}
	return n
}

// TestControlWait is UT FP-3 on the plane, delegated from tests/function
// (TestControlBoundedWait): registration against terminal publication
// races, any-of in caller order, the deadline's recheck and compact rows,
// the 1,024-registration capacity, shutdown and dispatch with a wait. Do
// not rename or skip it.
func TestControlWait(t *testing.T) {
	t.Parallel()
	t.Run("register-race", func(t *testing.T) {
		t.Parallel()
		// A registered wait wakes at the terminal commit, without the clock;
		// a commit held while the wait registers still wakes it; an
		// unconfirmed terminal never wins, and after its resync a new wait
		// returns it at once.
		tp, w := ctlPlane(t, 2)
		st := tp.run(t, w, "a", "race", "p2")
		res := tp.waitAsync(bg, []string{st.TaskID}, time.Minute)
		tp.registered(t, st.TaskID)
		tp.finish(t, w, st, 0, 0)
		r, err := res.wait(t)
		if err != nil || r.Status != contract.WaitTerminal || r.Winner != st.TaskID || r.Task.State != contract.TaskSucceeded || r.EffectiveWaitMS != 30000 {
			t.Fatalf("woken %+v %v", r, err)
		}
		if tp.waitCount(t) != 0 {
			t.Fatal("a registration survived its winner")
		}
		st2 := tp.run(t, w, "a", "held", "p3")
		hold := tp.th.arm("terminal-commit-queued", st2.TaskID)
		w.resultAck(w.sendResult(result(st2, 1, 0, nil)), st2.TaskID)
		call := paused(t, hold, "terminal-commit-queued")
		res = tp.waitAsync(bg, []string{st2.TaskID}, time.Minute)
		tp.registered(t, st2.TaskID)
		close(call.release)
		if r, err := res.wait(t); err != nil || r.Winner != st2.TaskID || r.Task.State != contract.TaskFailed {
			t.Fatalf("held commit %+v %v", r, err)
		}
		// Visible but unconfirmed: a row (durability_confirmed false), not a
		// winner; the deadline rechecks and answers still_running.
		tp2, w2 := ctlPlane(t, 1)
		st3 := tp2.run(t, w2, "a", "unconfirmed", "p2")
		tp2.inj.set("dirsync", tasksName)
		w2.resultAck(w2.sendResult(result(st3, 0, 0, nil)), st3.TaskID)
		tp2.log.awaitOnce(t, "published terminal "+st3.TaskID)
		res = tp2.waitAsync(bg, []string{st3.TaskID}, 2*time.Second)
		tp2.registered(t, st3.TaskID)
		tp2.clk.Advance(2 * time.Second)
		r, err = res.wait(t)
		if err != nil || r.Status != contract.WaitStillRunning || r.Tasks[0].State != contract.TaskSucceeded || r.Tasks[0].DurabilityConfirmed {
			t.Fatalf("unconfirmed %+v %v", r, err)
		}
		tp2.inj.set("", "")
		tp2.clk.Advance(storageRetry)
		tp2.log.awaitOnce(t, "terminal-committed "+st3.TaskID)
		if r, err := tp2.cl.WaitTasks(bg, []string{st3.TaskID}, 0); err != nil || r.Winner != st3.TaskID {
			t.Fatalf("after the resync %+v %v", r, err)
		}
		// A published winner is never hidden by the timer: the commit lands,
		// then the deadline fires; the answer is the winner.
		st4 := tp.run(t, w, "a", "timer", "p4")
		res = tp.waitAsync(bg, []string{st4.TaskID}, time.Second)
		tp.registered(t, st4.TaskID)
		tp.finish(t, w, st4, 0, 0)
		tp.clk.Advance(time.Second)
		if r, err := res.wait(t); err != nil || r.Winner != st4.TaskID {
			t.Fatalf("timer versus winner %+v %v", r, err)
		}
		tp.checkCounts(t)
	})
	t.Run("any-of", func(t *testing.T) {
		t.Parallel()
		// The first durable terminal of several wins; at the initial
		// observation ties go to caller order; ids are validated before
		// anything registers.
		tp, w := ctlPlane(t, 3)
		a := tp.run(t, w, "a", "one", "p2")
		b := tp.run(t, w, "a", "two", "p3")
		c := tp.run(t, w, "a", "three", "p4")
		res := tp.waitAsync(bg, []string{a.TaskID, b.TaskID, c.TaskID}, time.Minute)
		tp.registered(t, a.TaskID, b.TaskID, c.TaskID)
		tp.finish(t, w, b, 0, 0)
		if r, err := res.wait(t); err != nil || r.Winner != b.TaskID {
			t.Fatalf("any-of %+v %v", r, err)
		}
		tp.finish(t, w, c, 0, 0)
		if r, err := tp.cl.WaitTasks(bg, []string{a.TaskID, c.TaskID, b.TaskID}, time.Minute); err != nil || r.Winner != c.TaskID {
			t.Fatalf("caller order %+v %v", r, err)
		}
		unknown := "t_" + strings.Repeat("7", 32)
		if _, err := tp.cl.WaitTasks(bg, []string{a.TaskID, unknown}, time.Second); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("unknown %v", err)
		}
		if n := tp.waitCount(t); n != 0 {
			t.Fatalf("a rejected wait registered (%d)", n)
		}
		for name, raw := range map[string]string{
			"duplicate": `{"task_ids":["` + a.TaskID + `","` + a.TaskID + `"],"wait":"1s"}`,
			"empty":     `{"task_ids":[],"wait":"1s"}`,
			"17":        `{"task_ids":[` + strings.Repeat(`"`+a.TaskID+`",`, 16) + `"` + b.TaskID + `"],"wait":"1s"}`,
			"too long":  `{"task_ids":["` + a.TaskID + `"],"wait":"5m1s"}`,
			"negative":  `{"task_ids":["` + a.TaskID + `"],"wait":"-1s"}`,
			"no wait":   `{"task_ids":["` + a.TaskID + `"]}`,
			"extra":     `{"task_ids":["` + a.TaskID + `"],"wait":"1s","x":1}`,
		} {
			if status, body := tp.post(t, contract.PathTaskWait, raw); status != 400 || !strings.Contains(body, "invalid_argument") {
				t.Fatalf("%s = %d %s", name, status, body)
			}
		}
	})
	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		t.Run("decide-atomic", func(t *testing.T) {
			// Review C1: the answer (a winner, or rows) is decided in one
			// critical section. A terminal commit landing just before that
			// decision (held there) is the winner, never a durably terminal
			// row of a still_running answer, on both the zero-wait and the
			// deadline paths.
			for _, zero := range []bool{true, false} {
				tp, w := ctlPlane(t, 1)
				st := tp.run(t, w, "a", "atomic", "p2")
				hold := tp.th.arm("wait-decide", st.TaskID)
				var res *call[contract.WaitResponse]
				if zero {
					res = tp.waitAsync(bg, []string{st.TaskID}, 0)
				} else {
					res = tp.waitAsync(bg, []string{st.TaskID}, time.Second)
					tp.registered(t, st.TaskID)
					tp.clk.Advance(time.Second)
				}
				call := paused(t, hold, "wait-decide")
				tp.finish(t, w, st, 0, 0)
				close(call.release)
				if r, err := res.wait(t); err != nil || r.Status != contract.WaitTerminal || r.Winner != st.TaskID {
					t.Fatalf("zero=%v: %+v %v", zero, r, err)
				}
				if tp.waitCount(t) != 0 {
					t.Fatalf("zero=%v: a registration survived", zero)
				}
			}
		})
		// At the deadline, one compact row per id in caller order with the
		// bounded last log line; the effective wait is min(request, cap).
		tp, w := ctlPlane(t, 2)
		a := tp.run(t, w, "a", "rows", "p2")
		b := tp.run(t, w, "a", "quiet", "p3")
		w.log(a, 0, []byte("first\n"+strings.Repeat("é", 60)+"\x1b\n"))
		res := tp.waitAsync(bg, []string{b.TaskID, a.TaskID}, 3*time.Second)
		tp.registered(t, b.TaskID, a.TaskID)
		tp.clk.Advance(3*time.Second - time.Nanosecond)
		if !res.pending() {
			t.Fatal("the wait ended before its deadline")
		}
		tp.clk.Advance(time.Nanosecond)
		r, err := res.wait(t)
		if err != nil || r.Status != contract.WaitStillRunning || r.EffectiveWaitMS != 3000 || len(r.Tasks) != 2 || r.Tasks[0].TaskID != b.TaskID {
			t.Fatalf("rows %+v %v", r, err)
		}
		row := r.Tasks[1]
		if row.State != contract.TaskRunning || !row.LogTruncated || len(row.LastLogLine) > contract.MaxLastLogLineBytes ||
			!strings.HasSuffix(row.LastLogLine, "é\x1b") || row.ElapsedMS != 3000 || !row.DurabilityConfirmed {
			t.Fatalf("row %+v", row)
		}
		if r.Tasks[0].LastLogLine != "" || r.Tasks[0].LogTruncated {
			t.Fatalf("quiet row %+v", r.Tasks[0])
		}
		// The cap clamps (a 5 minute request answers within 30 s) and a zero
		// wait is an immediate snapshot that never registers.
		res = tp.waitAsync(bg, []string{a.TaskID}, 5*time.Minute)
		tp.registered(t, a.TaskID)
		for i := 0; i < 6; i++ {
			tp.clk.Advance(5 * time.Second)
			w.beat()
		}
		if r, err := res.wait(t); err != nil || r.EffectiveWaitMS != 30000 || r.Status != contract.WaitStillRunning {
			t.Fatalf("clamped %+v %v", r, err)
		}
		if r, err := tp.cl.WaitTasks(bg, []string{a.TaskID}, 0); err != nil || r.EffectiveWaitMS != 0 || r.Status != contract.WaitStillRunning || tp.waitCount(t) != 0 {
			t.Fatalf("snapshot %+v %v", r, err)
		}
		// A caller that leaves ends its registration.
		ctx, cancel := context.WithCancel(bg)
		res = tp.waitAsync(ctx, []string{a.TaskID}, time.Minute)
		tp.registered(t, a.TaskID)
		cancel()
		if _, err := res.wait(t); err == nil {
			t.Fatal("a canceled wait answered")
		}
		deadline := time.After(testWait)
		for tp.waitCount(t) != 0 {
			select {
			case <-deadline:
				t.Fatal("a departed caller's registration was kept")
			case <-tp.log.sig:
			case <-time.After(time.Millisecond):
			}
		}
	})
	t.Run("capacity", func(t *testing.T) {
		t.Parallel()
		// At 1,024 registrations a positive wait (and a dispatch with one) is
		// unavailable/wait_capacity with nothing changed; an immediate
		// snapshot needs no registration.
		tp, w := ctlPlane(t, 2)
		st := tp.run(t, w, "a", "capacity", "p2")
		ts := tp.svc(t)
		var rel []*waitReservation
		for i := 0; i < maxWaitCalls; i++ {
			f, err := ts.reserveWait()
			if err != nil {
				t.Fatalf("reservation %d: %v", i, err)
			}
			rel = append(rel, f)
		}
		_, err := tp.cl.WaitTasks(bg, []string{st.TaskID}, time.Second)
		wantReason(t, err, contract.CodeUnavailable, contract.ReasonWaitCapacity)
		n := tp.taskCount(t)
		_, err = tp.cl.DispatchWithWait(bg, taskReq(contract.TargetID, "a", "refused"), time.Second)
		wantReason(t, err, contract.CodeUnavailable, contract.ReasonWaitCapacity)
		if tp.taskCount(t) != n {
			t.Fatal("a capacity refusal admitted a task")
		}
		if r, err := tp.cl.WaitTasks(bg, []string{st.TaskID}, 0); err != nil || r.Status != contract.WaitStillRunning {
			t.Fatalf("snapshot at capacity %+v %v", r, err)
		}
		for _, f := range rel {
			f.release()
			f.release() // exactly once
		}
		if tp.waitCount(t) != 0 {
			t.Fatal("reservations leaked")
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		t.Parallel()
		// Plane shutdown ends a registered wait (unavailable, never a
		// fabricated snapshot) and its handler exits.
		tp, w := ctlPlane(t, 1)
		st := tp.run(t, w, "a", "shutdown", "p2")
		res := tp.waitAsync(bg, []string{st.TaskID}, time.Minute)
		tp.registered(t, st.TaskID)
		for _, w := range tp.workers {
			w.c.CloseNow()
		}
		tp.served.stop(t)
		if _, err := res.wait(t); err == nil {
			t.Fatal("a wait answered across shutdown")
		}
	})
	t.Run("dispatch", func(t *testing.T) {
		t.Parallel()
		// dispatch --wait admits once and waits from confirmed admission: a
		// task ending in time answers terminal (202) with its result; a
		// running one answers its compact row; the wait never replays the
		// admission.
		tp, w := ctlPlane(t, 2)
		res := async(func() (contract.DispatchResponse, error) {
			return tp.cl.DispatchWithWait(bg, taskReq(contract.TargetID, "a", "waited"), time.Minute)
		})
		st := w.start("p2")
		tp.registered(t, st.TaskID)
		w.answer("p2", st.TaskID, nil)
		tp.log.awaitOnce(t, "published running "+st.TaskID)
		tp.finish(t, w, st, 0, 0)
		r, err := res.wait(t)
		if err != nil || r.TaskID != st.TaskID || r.Task != nil || r.WaitResult == nil || r.WaitResult.Status != contract.WaitTerminal ||
			r.WaitResult.Task.State != contract.TaskSucceeded {
			t.Fatalf("dispatch terminal %+v %v", r, err)
		}
		res = async(func() (contract.DispatchResponse, error) {
			return tp.cl.DispatchWithWait(bg, taskReq(contract.TargetID, "a", "running"), time.Second)
		})
		st2 := w.start("p3")
		tp.registered(t, st2.TaskID)
		w.answer("p3", st2.TaskID, nil)
		tp.clk.Advance(time.Second)
		r, err = res.wait(t)
		if err != nil || r.WaitResult.Status != contract.WaitStillRunning || len(r.WaitResult.Tasks) != 1 || r.WaitResult.Tasks[0].TaskID != st2.TaskID {
			t.Fatalf("dispatch running %+v %v", r, err)
		}
		if n := strings.Count(strings.Join(tp.log.all(), "\n"), "task-published running"); n != 1 {
			t.Fatalf("the admission ran %d times", n)
		}
		// A terminal read failure is an error, never a fabricated answer.
		w.log(st2, 0, []byte("tail\n"))
		tp.finish(t, w, st2, 0, 5)
		os.WriteFile(layout{root: tp.root}.path(taskRel(st2.TaskID)), []byte("{}"), 0o600)
		if _, err := tp.cl.WaitTasks(bg, []string{st2.TaskID}, 0); err == nil {
			t.Fatal("an unreadable terminal answered")
		}
		// An invalid wait refuses before admission.
		n := tp.taskCount(t)
		if status, _ := tp.post(t, contract.PathTasks, strings.Replace(mustJSONOf(t, taskReq(contract.TargetID, "a", "bad")), `{`, `{"wait":"6m",`, 1)); status != 400 ||
			tp.taskCount(t) != n {
			t.Fatalf("invalid wait %d", status)
		}
	})
}

// newJSONPost is a protocol-5 JSON POST request.
func newJSONPost(url, body string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(contract.ProtocolHeader, "6")
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// post sends one protocol-5 JSON POST and returns its status and body.
func (tp *taskPlane) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req, err := newJSONPost(tp.url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client(t, readCA(t, tp.root), "127.0.0.1", 0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

// BenchmarkControlWait measures wait registration and wake-up at terminal
// commit: registering and unregistering one and sixteen ids, and waking
// one and 1,000 waiters of one task. Every waiter wakes and no
// registration is retained (invariants, not timings, fail it).
func BenchmarkControlWait(b *testing.B) {
	newTS := func(n int) (*taskService, []string) {
		ts := &taskService{tasks: map[string]*taskEntry{}, waits: map[*waitReg]bool{}}
		var ids []string
		for i := 0; i < n; i++ {
			id := "t_" + strings.Repeat("0", 31) + string(rune('a'+i%26))
			if i >= 26 {
				id = "t_" + strings.Repeat("1", 30) + string(rune('a'+i/26)) + string(rune('a'+i%26))
			}
			ts.tasks[id] = &taskEntry{rec: contract.TaskRecord{TaskID: id}}
			ids = append(ids, id)
		}
		return ts, ids
	}
	for _, n := range []int{1, 16} {
		b.Run("register-"+itoa(n), func(b *testing.B) {
			ts, ids := newTS(n)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ts.mu.Lock()
				r := ts.registerWaitLocked(ids)
				ts.dropWaitLocked(r)
				ts.mu.Unlock()
			}
			if len(ts.waits) != 0 || len(ts.tasks[ids[0]].waitRegs) != 0 {
				b.Fatal("a registration was retained")
			}
		})
	}
	for _, n := range []int{1, 1000} {
		b.Run("wake-"+itoa(n), func(b *testing.B) {
			ts, ids := newTS(1)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				regs := make([]*waitReg, n)
				ts.mu.Lock()
				for j := range regs {
					regs[j] = ts.registerWaitLocked(ids)
				}
				ts.mu.Unlock()
				b.StartTimer()
				ts.mu.Lock()
				ts.wakeWaitsLocked(ts.tasks[ids[0]])
				ts.mu.Unlock()
				b.StopTimer()
				for _, r := range regs {
					select {
					case <-r.ch:
					default:
						b.Fatal("a waiter was not woken")
					}
					if r.winner != ids[0] {
						b.Fatal("a waiter has no winner")
					}
				}
				if len(ts.waits) != 0 || len(ts.tasks[ids[0]].waitRegs) != 0 {
					b.Fatal("a registration was retained")
				}
				b.StartTimer()
			}
		})
	}
}
