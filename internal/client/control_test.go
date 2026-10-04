package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b client families: TestControlCancel (FP-1) and
// TestControlWait (FP-3) against a verified in-process server. The wait
// retry schedule's seams (its clock, its sleep and its transport margin)
// are replaced per case and restored; nothing sleeps in real time except
// a transport bound shortened to milliseconds.

// ctlServer is a verified server whose handler the case sets, recording
// every request body.
type ctlServer struct {
	c      *Client
	mu     sync.Mutex
	h      http.HandlerFunc
	bodies []string
	paths  []string
}

func startCtlServer(t *testing.T) *ctlServer {
	t.Helper()
	ca := newTestCA(t)
	cs := &ctlServer{}
	s := startServer(t, ca.leaf(t, true), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == contract.PathCA {
			w.Write(ca.pem)
			return
		}
		b, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.bodies = append(cs.bodies, string(b))
		cs.paths = append(cs.paths, r.Method+" "+r.URL.Path)
		h := cs.h
		cs.mu.Unlock()
		h(w, r)
	}))
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	cs.c = c
	return cs
}

func (cs *ctlServer) set(h http.HandlerFunc) {
	cs.mu.Lock()
	cs.h, cs.bodies, cs.paths = h, nil, nil
	cs.mu.Unlock()
}

func (cs *ctlServer) seen() ([]string, []string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]string(nil), cs.paths...), append([]string(nil), cs.bodies...)
}

// hangUp closes the connection without an answer (a plane restart).
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		conn.Close()
	}
}

// stopView is a running view with a stop request.
func stopView(id string) contract.TaskView {
	v := taskView(id, taskRequest("cancel me"), contract.TaskRunning)
	now := "2026-09-27T10:00:01Z"
	v.StartedAt, v.StopRequested = &now, true
	return v
}

// TestControlCancel is UT FP-1 on the client, delegated from
// tests/function (TestControlCancellation): the exact request, the
// accepted (202) and terminal (200) answers, and strict rejection of
// every inconsistent answer. Do not rename or skip it.
func TestControlCancel(t *testing.T) {
	cs := startCtlServer(t)
	path := contract.PathTasks + "/" + taskA + "/cancel"
	t.Run("accepted", func(t *testing.T) {
		cs.set(jsonRoute("6", 202, envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskA, Accepted: true, Task: stopView(taskA)})))
		r, err := cs.c.CancelTask(bg, taskA)
		if err != nil || !r.Accepted || !r.Task.StopRequested {
			t.Fatalf("accepted %+v %v", r, err)
		}
		if paths, bodies := cs.seen(); len(paths) != 1 || paths[0] != "POST "+path || bodies[0] != "{}" {
			t.Fatalf("request %v %v", paths, bodies)
		}
	})
	t.Run("terminal", func(t *testing.T) {
		cs.set(jsonRoute("6", 200, envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskA, Task: taskView(taskA, taskRequest("done"), contract.TaskSucceeded)})))
		r, err := cs.c.CancelTask(bg, taskA)
		if err != nil || r.Accepted || r.Task.State != contract.TaskSucceeded {
			t.Fatalf("terminal %+v %v", r, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		if _, err := cs.c.CancelTask(bg, "t_X"); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("bad id %v", err)
		}
		running := taskView(taskA, taskRequest("x"), contract.TaskRunning)
		for name, c := range map[string]struct {
			h    http.HandlerFunc
			code contract.Code
		}{
			"not found":         {jsonRoute("6", 404, `{"error":{"code":"not_found","message":"task does not exist"}}`), contract.CodeNotFound},
			"unavailable":       {jsonRoute("6", 503, `{"error":{"code":"unavailable","message":"retry"}}`), contract.CodeUnavailable},
			"accepted 200":      {jsonRoute("6", 200, envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskA, Accepted: true, Task: stopView(taskA)})), contract.CodeInvalidArgument},
			"unaccepted 202":    {jsonRoute("6", 202, envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskA, Task: taskView(taskA, taskRequest("d"), contract.TaskSucceeded)})), contract.CodeInvalidArgument},
			"not terminal":      {jsonRoute("6", 200, envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskA, Task: running})), contract.CodeInvalidArgument},
			"no stop":           {jsonRoute("6", 202, envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskA, Accepted: true, Task: running})), contract.CodeInvalidArgument},
			"other task":        {jsonRoute("6", 202, envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskB, Accepted: true, Task: stopView(taskB)})), contract.CodeInvalidArgument},
			"extra field":       {jsonRoute("6", 202, strings.Replace(envelopeOf(t, contract.CancelResponse{Version: 6, TaskID: taskA, Accepted: true, Task: stopView(taskA)}), `{"version"`, `{"x":1,"version"`, 1)), contract.CodeInvalidArgument},
			"version":           {jsonRoute("7", 202, `{}`), contract.CodeProtocolMismatch},
			"unexpected status": {jsonRoute("6", 201, `{}`), contract.CodeInvalidArgument},
		} {
			cs.set(c.h)
			if _, err := cs.c.CancelTask(bg, taskA); contract.CodeOf(err) != c.code {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// The caller's own cancellation wins over a late answer.
		block, arrived := make(chan struct{}), make(chan struct{})
		defer close(block)
		cs.set(func(w http.ResponseWriter, r *http.Request) {
			close(arrived)
			select {
			case <-block:
			case <-r.Context().Done():
			}
		})
		ctx, cancel := context.WithCancel(bg)
		go func() {
			<-arrived
			cancel()
		}()
		if _, err := cs.c.CancelTask(ctx, taskA); err != context.Canceled {
			t.Fatalf("caller cancellation %v", err)
		}
	})
}

// waitSeams replaces the wait's clock, sleep and margin for one case.
func waitSeams(t *testing.T, margin time.Duration) *fakeWaitClock {
	t.Helper()
	fc := &fakeWaitClock{now: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)}
	oldNow, oldSleep, oldMargin := waitNow, waitSleep, waitMarginFor
	waitNow = fc.Now
	waitSleep = func(ctx context.Context, d time.Duration) error {
		fc.add(d)
		return ctx.Err()
	}
	waitMarginFor = func(bool) time.Duration { return margin }
	t.Cleanup(func() { waitNow, waitSleep, waitMarginFor = oldNow, oldSleep, oldMargin })
	return fc
}

// fakeWaitClock is the wait's retry clock: it moves by each backoff.
type fakeWaitClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeWaitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeWaitClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// running is a still_running answer for ids.
func running(t *testing.T, eff int, ids ...string) string {
	t.Helper()
	var rows []contract.WaitRow
	for _, id := range ids {
		rows = append(rows, contract.WaitRow{TaskID: id, State: contract.TaskRunning, ElapsedMS: 5, LastLogLine: "line", DurabilityConfirmed: true})
	}
	b, err := contract.EncodeWaitResponse(contract.WaitResponse{Version: 6, Status: contract.WaitStillRunning, EffectiveWaitMS: eff, Tasks: rows})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestControlWait is UT FP-3 on the client, delegated from tests/function
// (TestControlBoundedWait): an interrupted wait retries within one
// original deadline asking only for what remains, then one snapshot or
// unavailable; a dispatch with a wait is never retried; the caller's and
// the client's own deadlines win over a late answer; answers are
// correlated with the request exactly. Do not rename or skip it.
func TestControlWait(t *testing.T) {
	cs := startCtlServer(t)
	t.Run("restart-budget", func(t *testing.T) {
		// Two interrupted attempts (100 ms then 200 ms backoff), then an
		// answer: the retries ask for the remaining wait.
		waitSeams(t, OperationTimeout)
		var n atomic.Int64
		cs.set(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) <= 2 {
				hangUp(w, r)
				return
			}
			jsonRoute("6", 200, running(t, 4700, taskA))(w, r)
		})
		r, err := cs.c.WaitTasks(bg, []string{taskA}, 5*time.Second)
		if err != nil || r.Status != contract.WaitStillRunning {
			t.Fatalf("after restarts %+v %v", r, err)
		}
		_, bodies := cs.seen()
		if len(bodies) != 3 || !strings.Contains(bodies[0], `"wait":"5s"`) || !strings.Contains(bodies[1], `"wait":"4.9s"`) || !strings.Contains(bodies[2], `"wait":"4.7s"`) {
			t.Fatalf("attempts %v", bodies)
		}
		// Past the wait's end a reachable plane gets one immediate
		// snapshot; an unreachable one is unavailable at the deadline, never
		// a fabricated answer.
		waitSeams(t, OperationTimeout)
		n.Store(0)
		cs.set(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) <= 2 {
				hangUp(w, r)
				return
			}
			jsonRoute("6", 200, running(t, 0, taskA))(w, r)
		})
		if r, err := cs.c.WaitTasks(bg, []string{taskA}, 300*time.Millisecond); err != nil || r.EffectiveWaitMS != 0 {
			t.Fatalf("snapshot %+v %v", r, err)
		}
		if _, bodies := cs.seen(); !strings.Contains(bodies[len(bodies)-1], `"wait":"0s"`) {
			t.Fatalf("snapshot request %v", bodies)
		}
		waitSeams(t, OperationTimeout)
		cs.set(hangUp)
		if _, err := cs.c.WaitTasks(bg, []string{taskA}, time.Second); contract.CodeOf(err) != contract.CodeUnavailable {
			t.Fatalf("unreachable %v", err)
		}
		// Not retried: validation, not_found and trust answers.
		for _, h := range []http.HandlerFunc{jsonRoute("6", 404, `{"error":{"code":"not_found","message":"no"}}`),
			jsonRoute("6", 400, `{"error":{"code":"invalid_argument","message":"no"}}`)} {
			cs.set(h)
			if _, err := cs.c.WaitTasks(bg, []string{taskA}, time.Second); err == nil {
				t.Fatal("an error answer passed")
			}
			if p, _ := cs.seen(); len(p) != 1 {
				t.Fatalf("an error answer was retried %v", p)
			}
		}
	})
	t.Run("no-dispatch-retry", func(t *testing.T) {
		// A dispatch with a wait is admitted at most once: a lost answer is
		// unavailable with the inspection instruction, never a retry.
		waitSeams(t, OperationTimeout)
		cs.set(hangUp)
		_, err := cs.c.DispatchWithWait(bg, taskRequest("once"), time.Second)
		if contract.CodeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "task ls") {
			t.Fatalf("lost dispatch %v", err)
		}
		paths, bodies := cs.seen()
		if len(paths) != 1 || paths[0] != "POST "+contract.PathTasks || !strings.Contains(bodies[0], `"wait":"1s"`) {
			t.Fatalf("attempts %v %v", paths, bodies)
		}
		if _, err := cs.c.DispatchWithWait(bg, taskRequest("once"), 6*time.Minute); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("over the cap %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		// The client's own transport deadline (the request plus its margin,
		// shortened here) is unavailable; the caller's cancellation is its
		// own error; neither publishes the late answer.
		waitSeams(t, 50*time.Millisecond)
		block := make(chan struct{})
		defer close(block)
		cs.set(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-block:
			case <-r.Context().Done():
			}
		})
		if _, err := cs.c.WaitTasks(bg, []string{taskA}, 0); contract.CodeOf(err) != contract.CodeUnavailable {
			t.Fatalf("own deadline %v", err)
		}
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if _, err := cs.c.WaitTasks(ctx, []string{taskA}, time.Second); err != context.Canceled {
			t.Fatalf("caller %v", err)
		}
		// A decoded answer that arrives after the outer deadline is not
		// published.
		fc := waitSeams(t, OperationTimeout)
		cs.set(func(w http.ResponseWriter, r *http.Request) {
			fc.add(time.Minute)
			jsonRoute("6", 200, running(t, 1000, taskA))(w, r)
		})
		if _, err := cs.c.WaitTasks(bg, []string{taskA}, time.Second); contract.CodeOf(err) != contract.CodeUnavailable {
			t.Fatalf("late answer %v", err)
		}
	})
	t.Run("correlation", func(t *testing.T) {
		waitSeams(t, OperationTimeout)
		term := taskView(taskB, taskRequest("b"), contract.TaskSucceeded)
		wr := contract.WaitResponse{Version: 6, Status: contract.WaitTerminal, EffectiveWaitMS: 1000, Winner: taskB, Task: &term}
		good, _ := contract.EncodeWaitResponse(wr)
		cs.set(jsonRoute("6", 200, string(good)))
		if r, err := cs.c.WaitTasks(bg, []string{taskA, taskB}, time.Second); err != nil || r.Winner != taskB {
			t.Fatalf("winner %+v %v", r, err)
		}
		for name, body := range map[string]string{
			"foreign winner": string(good),
			"row order":      running(t, 1000, taskB, taskA),
			"missing row":    running(t, 1000, taskA),
			"over effective": running(t, 1001, taskA, taskB),
		} {
			cs.set(jsonRoute("6", 200, body))
			ids := []string{taskA, taskB}
			if name == "foreign winner" {
				ids = []string{taskA}
			}
			if _, err := cs.c.WaitTasks(bg, ids, time.Second); contract.CodeOf(err) != contract.CodeInvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := cs.c.WaitTasks(bg, []string{taskA, taskA}, time.Second); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("duplicate %v", err)
		}
		// A dispatch's answer names its own task, and never carries the
		// admitted view beside the wait.
		one := contract.WaitResponse{Version: 6, Status: contract.WaitStillRunning, EffectiveWaitMS: 1000,
			Tasks: []contract.WaitRow{{TaskID: taskA, State: contract.TaskPending}}}
		v := taskView(taskA, taskRequest("d"), contract.TaskPending)
		for name, c := range map[string]struct {
			body string
			ok   bool
		}{
			"ok":        {envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskA, WaitResult: &one}), true},
			"other":     {envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskB, WaitResult: &one}), false},
			"with task": {envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskA, Task: &v, WaitResult: &one}), false},
			"null wait": {envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskA, Task: &v}), false},
		} {
			cs.set(jsonRoute("6", 202, c.body))
			r, err := cs.c.DispatchWithWait(bg, taskRequest("d"), time.Second)
			if (err == nil) != c.ok || (c.ok && (r.TaskID != taskA || r.WaitResult == nil)) {
				t.Fatalf("%s: %+v %v", name, r, err)
			}
		}
	})
}
