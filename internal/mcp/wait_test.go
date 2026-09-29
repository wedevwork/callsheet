package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// FP-7: the budget algebra: reserves, deadlines, the admission allowance,
// elapsed setup, millisecond truncation and the range.
func TestBudgetAlgebra(t *testing.T) {
	d := func(s string) time.Duration {
		v, err := time.ParseDuration(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		b                  string
		r                  string
		setup              string
		wait, dispatchWait string
	}{
		{"10s", "1s", "0", "8s", "2s"},
		{"10s", "1s", "1.5s", "6.5s", "500ms"},
		{"10s", "1s", "8s", "0", "0"},
		{"1s", "250ms", "0", "500ms", "0"},
		{"1s", "250ms", "100ms", "400ms", "0"},
		{"2s", "500ms", "0", "1s", "0"},
		{"5m", "1s", "0", "4m58s", "4m52s"},
		{"10s", "1s", "1.2345678s", "6.765s", "765ms"},
	} {
		b := NewBudget(d(c.b))
		if b.R != d(c.r) || b.N != d(c.r) {
			t.Fatalf("%s reserves %+v", c.b, b)
		}
		delivery, client := b.Deadlines(epoch)
		if !delivery.Equal(epoch.Add(d(c.b))) || !client.Equal(delivery.Add(-d(c.r))) {
			t.Fatalf("%s deadlines %v %v", c.b, delivery, client)
		}
		now := epoch.Add(d(c.setup))
		if got := b.Available(delivery, now, false); got != d(c.wait) {
			t.Fatalf("%s after %s: wait %v", c.b, c.setup, got)
		}
		if got := b.Available(delivery, now, true); got != d(c.dispatchWait) {
			t.Fatalf("%s after %s: dispatch wait %v", c.b, c.setup, got)
		}
	}
	three, zero := 3*time.Second, time.Duration(0)
	if EffectiveWait(nil, 8*time.Second) != 8*time.Second || EffectiveWait(&three, 8*time.Second) != three || EffectiveWait(&three, time.Second) != time.Second ||
		EffectiveWait(&zero, 8*time.Second) != 0 {
		t.Fatal("EffectiveWait")
	}
	for _, v := range []time.Duration{time.Second, DefaultBudget, 5 * time.Minute} {
		if !ValidBudget(v) {
			t.Fatalf("%v rejected", v)
		}
	}
	for _, v := range []time.Duration{999 * time.Millisecond, 0, -time.Second, 5*time.Minute + 1} {
		if ValidBudget(v) {
			t.Fatalf("%v accepted", v)
		}
	}
	if DefaultBudget != 10*time.Second || NewBudget(DefaultBudget).String() != "10s" {
		t.Fatal("the shipping interim budget is 10s")
	}
}

// waitHarness is a ready session whose WaitTasks and DispatchWithWait
// record the requested wait and hold until released.
type waitRec struct {
	mu     sync.Mutex
	waits  []time.Duration
	ctxs   []context.Context
	answer func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error)
}

func (w *waitRec) record(ctx context.Context, wait time.Duration) {
	w.mu.Lock()
	w.waits = append(w.waits, wait)
	w.ctxs = append(w.ctxs, ctx)
	w.mu.Unlock()
}

func (w *waitRec) last(t *testing.T) (time.Duration, context.Context) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.waits) == 0 {
		t.Fatal("no wait was requested")
	}
	return w.waits[len(w.waits)-1], w.ctxs[len(w.ctxs)-1]
}

func stillRunning(ids []string, wait time.Duration) contract.WaitResponse {
	rows := make([]contract.WaitRow, len(ids))
	for i, id := range ids {
		rows[i] = contract.WaitRow{TaskID: id, State: contract.TaskRunning, ElapsedMS: 5, LastLogLine: "working", DurabilityConfirmed: true}
	}
	return contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitStillRunning, EffectiveWaitMS: int(wait / time.Millisecond), Tasks: rows}
}

func newWaitHarness(t *testing.T, setup time.Duration, opts ...hopt) (*harness, *waitRec) {
	t.Helper()
	rec := &waitRec{}
	var h *harness
	opts = append(opts, withFactory(func(ctx context.Context) (Client, error) {
		h.factory.Add(1)
		if setup > 0 {
			// Trust setup takes time: the call's timers are already armed.
			h.clock.Advance(setup)
		}
		return h.fake, nil
	}))
	h = start(t, opts...)
	happy(h.fake)
	h.fake.waitTasks = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
		rec.record(ctx, wait)
		if rec.answer != nil {
			return rec.answer(ctx, ids, wait)
		}
		return stillRunning(ids, wait), nil
	}
	dw := h.fake.dispatchWait
	h.fake.dispatchWait = func(ctx context.Context, req contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error) {
		rec.record(ctx, wait)
		return dw(ctx, req, wait)
	}
	h.ready()
	return h, rec
}

// FP-7: the plane wait requested: the available budget when omitted, an
// explicit wait capped by it, zero kept as an immediate snapshot, trust
// setup counted, and no dispatch when no client time remains.
func TestWaitRequested(t *testing.T) {
	ids := `"task_ids":["` + taskA + `"]`
	for _, c := range []struct {
		budget, setup time.Duration
		tool, args    string
		want          time.Duration
	}{
		{DefaultBudget, 0, toolTaskWait, `{` + ids + `}`, 8 * time.Second},
		{DefaultBudget, 0, toolTaskWait, `{` + ids + `,"wait":"3s"}`, 3 * time.Second},
		{DefaultBudget, 0, toolTaskWait, `{` + ids + `,"wait":"0"}`, 0},
		{DefaultBudget, 0, toolTaskWait, `{` + ids + `,"wait":"5m"}`, 8 * time.Second},
		{DefaultBudget, 1500 * time.Millisecond, toolTaskWait, `{` + ids + `}`, 6500 * time.Millisecond},
		{time.Second, 0, toolTaskWait, `{` + ids + `}`, 500 * time.Millisecond},
		{time.Second, 0, toolTaskWait, `{` + ids + `,"wait":"100ms"}`, 100 * time.Millisecond},
		{5 * time.Minute, 0, toolTaskWait, `{` + ids + `,"wait":"5m"}`, 298 * time.Second},
		{DefaultBudget, 0, toolDispatch, `{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":"5m"}`, 2 * time.Second},
		{DefaultBudget, 1500 * time.Millisecond, toolDispatch, `{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":"5m"}`, 500 * time.Millisecond},
		{time.Second, 0, toolDispatch, `{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":"5m"}`, 0},
	} {
		h, rec := newWaitHarness(t, c.setup, withBudget(c.budget))
		a := h.ask(c.tool, c.args)
		got, ctx := rec.last(t)
		if a.isError || got != c.want {
			t.Fatalf("%v %s %s: wait %v %+v", c.budget, c.tool, c.args, got, a)
		}
		// The client's context carries the D-R cancellation, never a
		// longer own allowance.
		if _, has := ctx.Deadline(); has {
			t.Fatal("the client context has a wall-clock deadline instead of the injected D-R")
		}
	}
	// Without a wait a dispatch never waits and is not budgeted.
	h, rec := newWaitHarness(t, 0)
	if a := h.ask(toolDispatch, `{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a"}`); a.isError || len(rec.waits) != 0 || h.fake.count("Dispatch") != 1 {
		t.Fatalf("dispatch without wait %+v %v", a, rec.waits)
	}
	// No client time left after trust setup: unavailable, nothing sent.
	for _, tool := range []string{toolTaskWait, toolDispatch} {
		h, _ := newWaitHarness(t, 9*time.Second)
		args := `{` + ids + `}`
		if tool == toolDispatch {
			args = `{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":"1s"}`
		}
		e := h.ask(tool, args).errorOf(t)
		if e.Code != contract.CodeUnavailable || e.Details["reason"] != ReasonCallBudget || h.fake.count("WaitTasks")+h.fake.count("DispatchWithWait") != 0 {
			t.Fatalf("%s without time: %+v %v", tool, e, h.fake.called())
		}
		if tool == toolDispatch && !strings.Contains(e.Message, "inspect task_ls") {
			t.Fatalf("dispatch budget guidance %q", e.Message)
		}
	}
	// Malformed, negative and oversized waits are invalid, never clamped.
	h, _ = newWaitHarness(t, 0)
	for _, w := range []string{`"-1s"`, `"5m1s"`, `"soon"`, `5`, `""`} {
		if e := h.ask(toolTaskWait, `{`+ids+`,"wait":`+w+`}`).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("wait %s %+v", w, e)
		}
	}
	for _, bad := range []string{`{"task_ids":[]}`, `{"task_ids":["` + taskA + `","` + taskA + `"]}`, `{"task_ids":"` + taskA + `"}`, `{}`} {
		if e := h.ask(toolTaskWait, bad).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("ids %s %+v", bad, e)
		}
	}
}

// FP-7: the plane's own clamp, its terminal answer (a failed task is a
// successful observation) and an outage (unavailable, never an invented
// still_running) pass through unchanged; still_running keeps caller order
// within its wire bound for 1 and 16 IDs.
func TestWaitAnswers(t *testing.T) {
	h, rec := newWaitHarness(t, 0)
	ids := make([]string, 16)
	for i := range ids {
		ids[i] = "t_" + strings.Repeat(string("0123456789abcdef"[15-i]), 32)
	}
	worst := strings.Repeat(`"`, contract.MaxLastLogLineBytes)
	rec.answer = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
		r := stillRunning(ids, 100*time.Millisecond) // the plane clamped the wait
		for i := range r.Tasks {
			r.Tasks[i].LastLogLine = worst
			r.Tasks[i].ElapsedMS = contract.MaxSafeInteger
		}
		return r, nil
	}
	for _, n := range []int{1, 16} {
		b, _ := json.Marshal(ids[:n])
		h.nextID++
		h.call(h.nextID, toolTaskWait, `{"task_ids":`+string(b)+`}`)
		line := h.nextRaw()
		limit := stillRunningWireOne
		if n > 1 {
			limit = stillRunningWireMany
		}
		if len(line) > limit {
			t.Fatalf("%d ids: %d bytes over %d", n, len(line), limit)
		}
		var m struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		json.Unmarshal(line, &m)
		r, err := contract.ParseWaitResponse([]byte(m.Result.Content[0].Text), ids[:n], 8*time.Second)
		if m.Result.IsError || err != nil || r.EffectiveWaitMS != 100 || r.Tasks[n-1].TaskID != ids[n-1] || r.Tasks[0].LastLogLine != worst {
			t.Fatalf("%d ids: %v %s", n, err, m.Result.Content[0].Text)
		}
	}
	v := taskView(taskA)
	v.State = contract.TaskFailed
	rec.answer = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
		return contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 8000, Winner: taskA, Task: &v}, nil
	}
	if a := h.ask(toolTaskWait, `{"task_ids":["`+taskA+`"]}`); a.isError || !strings.Contains(a.text, `"status":"terminal"`) || !strings.Contains(a.text, `"state":"failed"`) {
		t.Fatalf("terminal %+v", a)
	}
	rec.answer = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
		return contract.WaitResponse{}, contract.New(contract.CodeUnavailable, "cannot reach the plane at https://p: the connection was closed")
	}
	if e := h.ask(toolTaskWait, `{"task_ids":["`+taskA+`"]}`).errorOf(t); e.Code != contract.CodeUnavailable || strings.Contains(e.Message, "still_running") {
		t.Fatalf("outage %+v", e)
	}
	// An answer over the contract's still_running bound is internal.
	rec.answer = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
		r := stillRunning(ids, wait)
		r.Tasks[0].LastLogLine = strings.Repeat("x", 4096)
		return r, nil
	}
	if e := h.ask(toolTaskWait, `{"task_ids":["`+taskA+`"]}`).errorOf(t); e.Code != contract.CodeInternal {
		t.Fatalf("oversized still_running %+v", e)
	}
	if h.fake.count("CancelTask") != 0 {
		t.Fatal("a wait cancelled a task")
	}
	// A dispatch's still_running answer keeps the same bounds: the worst
	// legal row fits, an oversized one is internal.
	dispatchArgs := `{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":"1s"}`
	h.fake.dispatchWait = func(_ context.Context, _ contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error) {
		wr := stillRunning([]string{taskA}, wait)
		wr.Tasks[0].LastLogLine, wr.Tasks[0].ElapsedMS = worst, contract.MaxSafeInteger
		return contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: taskA, WaitResult: &wr}, nil
	}
	if a := h.ask(toolDispatch, dispatchArgs); a.isError || !strings.Contains(a.text, `"status":"still_running"`) {
		t.Fatalf("dispatch still_running %+v", a)
	}
	h.fake.dispatchWait = func(_ context.Context, _ contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error) {
		wr := stillRunning([]string{taskA}, wait)
		wr.Tasks[0].LastLogLine = strings.Repeat("x", 4096)
		return contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: taskA, WaitResult: &wr}, nil
	}
	if e := h.ask(toolDispatch, dispatchArgs).errorOf(t); e.Code != contract.CodeInternal {
		t.Fatalf("oversized dispatch still_running %+v", e)
	}
}

// awaitArmed waits for a call's two budget timers (D-R and D) after its
// admission at the current instant.
func awaitArmed(t *testing.T, h *harness, b Budget, admitted time.Time) {
	t.Helper()
	delivery, client := b.Deadlines(admitted)
	if err := h.clock.AwaitWaiter(testWait, func(ws []testkit.Waiter) bool {
		d, c := false, false
		for _, w := range ws {
			d = d || w.At.Equal(delivery)
			c = c || w.At.Equal(client)
		}
		return d && c
	}); err != nil {
		t.Fatal(err)
	}
}

// FP-7: a client reports its own deadline over a late response. The
// answer decoded before D-R but queued after it, a response released only
// after the D-R expiry, and a response queued at exactly D-R are all
// unavailable; one instant earlier the plane's answer is delivered.
func TestWaitOwnDeadline(t *testing.T) {
	b := NewBudget(DefaultBudget)
	ids := `{"task_ids":["` + taskA + `"]}`
	t.Run("decoded-then-deadline", func(t *testing.T) {
		atQueue := make(chan struct{})
		resume := make(chan struct{})
		h, _ := newWaitHarness(t, 0, withHook(func(stage, id string) {
			if stage == StageQueue && id == "n1" {
				close(atQueue)
				<-resume
			}
		}))
		h.call(1, toolTaskWait, ids)
		awaitArmed(t, h, b, epoch)
		<-atQueue // the plane's answer was decoded; now D-R fires
		h.clock.Advance(b.B - b.R)
		a := h.answer() // the expiry's answer is written while the handler waits
		close(resume)
		if e := a.errorOf(t); e.Code != contract.CodeUnavailable || e.Details["reason"] != ReasonCallBudget {
			t.Fatalf("answer %+v", e)
		}
		h.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
		if rawID(t, h.nextRaw()) != `"p"` {
			t.Fatal("the late result was written after the expiry")
		}
	})
	t.Run("deadline-then-response", func(t *testing.T) {
		g := newGate()
		h, rec := newWaitHarness(t, 0, withHook(nil))
		rec.answer = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
			g.entered <- struct{}{}
			<-g.release // the plane answers late, ignoring cancellation
			return stillRunning(ids, wait), nil
		}
		h.call(1, toolTaskWait, ids)
		g.awaitEntered(t)
		awaitArmed(t, h, b, epoch)
		h.clock.Advance(b.B - b.R)
		if e := h.answer().errorOf(t); e.Details["reason"] != ReasonCallBudget {
			t.Fatalf("answer %+v", e)
		}
		_, ctx := rec.last(t)
		if context.Cause(ctx) != errOwnDeadline {
			t.Fatalf("client context cause %v", context.Cause(ctx))
		}
		close(g.release)
		h.await(StageReleased, "n1")
		h.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
		if rawID(t, h.nextRaw()) != `"p"` {
			t.Fatal("the late response was written")
		}
	})
	for _, c := range []struct {
		name   string
		at     time.Duration
		expire bool
	}{{"equal", b.B - b.R, true}, {"before", b.B - b.R - time.Nanosecond, false}} {
		t.Run(c.name, func(t *testing.T) {
			// Set moves the clock without firing the D-R timer: only the
			// recheck immediately before queueing decides.
			atQueue := make(chan struct{})
			resume := make(chan struct{})
			h, _ := newWaitHarness(t, 0, withHook(func(stage, id string) {
				if stage == StageQueue && id == "n1" {
					close(atQueue)
					<-resume
				}
			}))
			h.call(1, toolTaskWait, ids)
			awaitArmed(t, h, b, epoch)
			<-atQueue
			h.clock.Set(epoch.Add(c.at))
			close(resume)
			a := h.answer()
			if a.isError != c.expire {
				t.Fatalf("at %v: %+v", c.at, a)
			}
		})
	}
}

// FP-7: the absolute delivery deadline D closes the session when a
// waiting call's answer has not finished writing, even while the reader
// keeps the writer's progress timer alive.
func TestWaitDeliveryDeadline(t *testing.T) {
	w := newCtlWriter(64 << 10)
	b := NewBudget(DefaultBudget)
	var h *harness
	h = start(t, withOutput(w, w.Close))
	happy(h.fake)
	v := taskView(taskA)
	v.State = contract.TaskSucceeded
	v.LogTail = strings.Repeat("x", 160<<10) // more than the 64 KiB pipe plus D's eleven paced 4 KiB drains; no escaping
	h.fake.waitTasks = func(context.Context, []string, time.Duration) (contract.WaitResponse, error) {
		return contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 1, Winner: taskA, Task: &v}, nil
	}
	readyOn(t, h, w)
	h.call(1, toolTaskWait, `{"task_ids":["`+taskA+`"]}`)
	awaitArmed(t, h, b, epoch)
	for i := 0; ; i++ {
		if w.awaitFullOrLine(t) {
			t.Fatal("the frame completed; it must be larger than the paced transfer")
		}
		if err := h.clock.AwaitWaiter(testWait, armedAt(h.clock.Now().Add(WriteIdle), WriteIdle)); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(900 * time.Millisecond)
		if h.clock.Now().Sub(epoch) >= b.B {
			// D passed while the frame kept progressing.
			if code := h.exit(); code != ExitOutput || !strings.Contains(h.stderr.String(), "not delivered within its 10s call budget") ||
				strings.Contains(h.stderr.String(), "no progress") {
				t.Fatalf("exit %d at %v: %q", code, h.clock.Now().Sub(epoch), h.stderr.String())
			}
			return
		}
		select {
		case code := <-h.code:
			t.Fatalf("the session ended at %v before D: %d %q", h.clock.Now().Sub(epoch), code, h.stderr.String())
		default:
		}
		w.drain(4 << 10)
		if i > 20 {
			t.Fatal("D never closed the session")
		}
	}
}

// FP-7: cancelling an outstanding wait stops only the wait: no answer,
// no task cancellation, the session usable.
func TestWaitCancelOnly(t *testing.T) {
	g := newGate()
	h, rec := newWaitHarness(t, 0, withHook(nil))
	rec.answer = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
		if err := g.wait(ctx); err != nil {
			return contract.WaitResponse{}, err
		}
		return stillRunning(ids, wait), nil
	}
	h.call(1, toolTaskWait, `{"task_ids":["`+taskA+`"]}`)
	g.awaitEntered(t)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	h.await(StageReleased, "n1")
	_, ctx := rec.last(t)
	if context.Cause(ctx) != errCancelled {
		t.Fatalf("cause %v", context.Cause(ctx))
	}
	h.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if rawID(t, h.nextRaw()) != `"p"` {
		t.Fatal("cancelled wait answered")
	}
	if h.fake.count("CancelTask") != 0 {
		t.Fatal("cancelling a wait cancelled the task")
	}
	// The D timer of the cancelled call does not end the session.
	h.await(StageWritten, "sp")
	h.clock.Advance(DefaultBudget)
	h.send(`{"jsonrpc":"2.0","id":"q","method":"ping"}`)
	if rawID(t, h.nextRaw()) != `"q"` {
		t.Fatal("session ended after a cancelled call's D")
	}
}
