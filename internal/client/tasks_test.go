package client

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

const (
	taskA = "t_0000000000000000000000000000000a"
	taskB = "t_0000000000000000000000000000000b"
)

func taskRequest(goal string) contract.DispatchRequest {
	return contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetName, Value: "coder"}, Goal: goal, Payload: []string{"repo://x"},
		Acceptance: "tests pass", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "dev", Hostname: "coord-1"}}
}

// taskView is a valid view of id for req in state.
func taskView(id string, req contract.DispatchRequest, state string) contract.TaskView {
	now := "2026-09-27T10:00:00Z"
	v := contract.TaskView{TaskID: id, TimeoutPolicy: contract.TimeoutPolicyEnforced, Request: req, Role: contract.TaskRole{ID: "worker-a", Name: "coder", Node: nodeA, Adapter: "fake", RegistrationOrder: 1},
		Effective: contract.TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour}, State: state, CreatedAt: now, DurabilityConfirmed: true}
	if state == contract.TaskSucceeded {
		zero := 0
		v.StartedAt, v.FinishedAt = &now, &now
		v.Result = &contract.TaskResult{State: state, ExitCode: &zero}
	}
	return v
}

func envelopeOf(t *testing.T, v any) string {
	t.Helper()
	b, err := contract.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTaskClient is UT FP-7 for the four task operations: exact
// requests, strict responses with operation-specific bounds, trust, no
// retry or cache, and the operation deadline over a late response.
func TestTaskClient(t *testing.T) {
	ca := newTestCA(t)
	type req struct {
		method, path, query, ctype, version, body string
	}
	var mu sync.Mutex
	var last req
	var hits atomic.Int64
	routes := map[string]http.HandlerFunc{}
	s := startServer(t, ca.leaf(t, true), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != contract.PathCA {
			hits.Add(1)
		}
		mu.Lock()
		last = req{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), r.Header.Get(contract.ProtocolHeader), string(b)}
		mu.Unlock()
		planeHandler(ca.pem, routes).ServeHTTP(w, r)
	}))
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sent := func() req {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	r := taskRequest("fix it")
	t.Run("dispatch", func(t *testing.T) {
		routes[contract.PathTasks] = jsonRoute("6", 202, envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskA, Task: ptrView(taskView(taskA, r, contract.TaskPending))}))
		v, err := c.Dispatch(bg, r)
		got := sent()
		if err != nil || v.TaskID != taskA || got.method != "POST" || got.path != contract.PathTasks || got.ctype != "application/json" || got.version != "6" {
			t.Fatalf("dispatch = %+v %v %+v", v, err, got)
		}
		// The attribution is the caller's, sent as given.
		var body map[string]json.RawMessage
		if json.Unmarshal([]byte(got.body), &body) != nil || string(body["requested_by"]) != `{"name":"callsheet","version":"dev","hostname":"coord-1"}` || body["override"] != nil {
			t.Fatalf("body %s", got.body)
		}
		// Invalid requests fail before anything is sent.
		before := hits.Load()
		bad := r
		bad.RequestedBy.Hostname = "not a host"
		if _, err := c.Dispatch(bg, bad); contract.CodeOf(err) != contract.CodeInvalidArgument || hits.Load() != before {
			t.Fatalf("invalid request = %v, %d requests", err, hits.Load()-before)
		}
		// Admission errors keep their code, reason and candidates; nothing
		// is retried.
		routes[contract.PathTasks] = jsonRoute("6", 503, `{"error":{"code":"unavailable","message":"no candidate","details":{"reason":"no_capacity","candidates":[{"role_id":"worker-a","node_id":"`+nodeA+`","registration_order":1,"node_liveness":"online","inflight":1,"reconciling_inflight":0,"concurrency":1,"can_accept":false,"reason":"full"}]}}}`)
		before = hits.Load()
		_, err = c.Dispatch(bg, r)
		ce, ok := err.(*contract.Error)
		if !ok || ce.Code != contract.CodeUnavailable || ce.Details["reason"] != "no_capacity" || hits.Load() != before+1 {
			t.Fatalf("admission error %v (%d requests)", err, hits.Load()-before)
		}
		if cs, err := contract.ParseCandidates(ce.Details["candidates"]); err != nil || len(cs) != 1 || cs[0].Reason != contract.ReasonFull {
			t.Fatalf("candidates %v %v", cs, err)
		}
		for name, rt := range map[string]http.HandlerFunc{
			"status":   jsonRoute("6", 200, envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskA, Task: ptrView(taskView(taskA, r, contract.TaskPending))})),
			"mismatch": jsonRoute("6", 202, envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskB, Task: ptrView(taskView(taskA, r, contract.TaskPending))})),
			"other":    jsonRoute("6", 202, envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskA, Task: ptrView(taskView(taskA, taskRequest("other"), contract.TaskPending))})),
			"version":  jsonRoute("6", 202, `{}`),
			"big":      jsonRoute("6", 202, `{"version":6,"x":"`+strings.Repeat("x", contract.MaxTaskViewBytes)+`"}`),
		} {
			routes[contract.PathTasks] = rt
			if _, err := c.Dispatch(bg, r); err == nil {
				t.Fatalf("dispatch %s accepted", name)
			}
		}
	})
	t.Run("list", func(t *testing.T) {
		sum := func(id string) contract.TaskSummary {
			v := taskView(id, r, contract.TaskPending)
			return contract.TaskSummary{TaskID: id, Target: r.Target, Role: v.Role, State: v.State, CreatedAt: v.CreatedAt, Effective: v.Effective,
				RequestedBy: r.RequestedBy, DurabilityConfirmed: true}
		}
		next := taskB
		routes[contract.PathTasks] = jsonRoute("6", 200, envelopeOf(t, contract.TaskListResponse{Version: 6, Tasks: []contract.TaskSummary{sum(taskB)}, NextAfter: &next}))
		tasks, n, err := c.ListTasks(bg, taskA, 1)
		if err != nil || len(tasks) != 1 || *n != taskB || sent().query != "after="+taskA+"&limit=1" || sent().method != "GET" || sent().body != "" {
			t.Fatalf("list = %+v %v %v %+v", tasks, n, err, sent())
		}
		routes[contract.PathTasks] = jsonRoute("6", 200, `{"version":6,"tasks":[],"next_after":null}`)
		if tasks, n, err := c.ListTasks(bg, "", 0); err != nil || len(tasks) != 0 || n != nil || sent().query != "" {
			t.Fatalf("empty page = %v %v %v", tasks, n, err)
		}
		// The page must follow the cursor and respect the limit.
		routes[contract.PathTasks] = jsonRoute("6", 200, envelopeOf(t, contract.TaskListResponse{Version: 6, Tasks: []contract.TaskSummary{sum(taskA)}}))
		if _, _, err := c.ListTasks(bg, taskB, 0); err == nil {
			t.Fatal("a page before the cursor was accepted")
		}
		routes[contract.PathTasks] = jsonRoute("6", 200, envelopeOf(t, contract.TaskListResponse{Version: 6, Tasks: []contract.TaskSummary{sum(taskA), sum(taskB)}}))
		if _, _, err := c.ListTasks(bg, "", 1); err == nil {
			t.Fatal("a page over the limit was accepted")
		}
		for _, bad := range []func() error{
			func() error { _, _, err := c.ListTasks(bg, "t_x", 0); return err },
			func() error { _, _, err := c.ListTasks(bg, "", 101); return err },
			func() error { _, _, err := c.ListTasks(bg, "", -1); return err },
		} {
			if err := bad(); contract.CodeOf(err) != contract.CodeInvalidArgument {
				t.Fatalf("invalid list input = %v", err)
			}
		}
	})
	t.Run("show", func(t *testing.T) {
		routes[contract.PathTasks+"/"+taskA] = jsonRoute("6", 200, envelopeOf(t, contract.TaskShowResponse{Version: 6, Task: taskView(taskA, r, contract.TaskSucceeded)}))
		v, err := c.ShowTask(bg, taskA, 5)
		if err != nil || v.State != contract.TaskSucceeded || sent().query != "lines=5" {
			t.Fatalf("show = %+v %v", v, err)
		}
		routes[contract.PathTasks+"/"+taskA] = jsonRoute("6", 404, `{"error":{"code":"not_found","message":"task t_... does not exist"}}`)
		if _, err := c.ShowTask(bg, taskA, 5); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("unknown = %v", err)
		}
		routes[contract.PathTasks+"/"+taskA] = jsonRoute("6", 200, envelopeOf(t, contract.TaskShowResponse{Version: 6, Task: taskView(taskB, r, contract.TaskPending)}))
		if _, err := c.ShowTask(bg, taskA, 5); err == nil {
			t.Fatal("another task was accepted")
		}
		if _, err := c.ShowTask(bg, "t_bad", 5); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("bad id = %v", err)
		}
		if _, err := c.ShowTask(bg, taskA, 201); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("bad lines = %v", err)
		}
	})
	t.Run("logs", func(t *testing.T) {
		data := []byte{0, 1, 0xff, '\n'}
		lr := contract.TaskLogsResponse{Version: 6, TaskID: taskA, Data: data, RetainedBytes: 4, SourceBytes: 10, DroppedBytes: 6, Truncated: true, Incomplete: true}
		routes[contract.PathTasks+"/"+taskA+"/logs"] = jsonRoute("6", 200, envelopeOf(t, lr))
		got, err := c.TaskLogs(bg, taskA)
		if err != nil || string(got.Data) != string(data) || !got.Incomplete || !got.Truncated || got.DroppedBytes != 6 {
			t.Fatalf("logs = %+v %v", got, err)
		}
		lr.TaskID = taskB
		routes[contract.PathTasks+"/"+taskA+"/logs"] = jsonRoute("6", 200, envelopeOf(t, lr))
		if _, err := c.TaskLogs(bg, taskA); err == nil {
			t.Fatal("another task's logs were accepted")
		}
		// Over the 16 MiB logs bound: a declared length is refused before
		// any body is read (the streamed, undeclared form is the shared
		// bound checked by the role responses).
		routes[contract.PathTasks+"/"+taskA+"/logs"] = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(contract.ProtocolHeader, "6")
			w.Header().Set("Content-Length", strconv.Itoa(contract.MaxTaskLogsBytes+1))
			w.WriteHeader(200)
			io.WriteString(w, `{"version":6,"x":"`)
		}
		if _, err := c.TaskLogs(bg, taskA); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("oversized logs = %v", err)
		}
	})
	t.Run("late-logs", func(t *testing.T) {
		// Iteration 06a: the late evidence's separate tail, byte exact,
		// through ?late=true; a task without one is not_found.
		data := []byte("late tail\n\xff")
		lr := contract.TaskLogsResponse{Version: 6, TaskID: taskA, Data: data, RetainedBytes: len(data), SourceBytes: len(data) + 2,
			DroppedBytes: 2, Truncated: true, Incomplete: true}
		path := contract.PathTasks + "/" + taskA + "/logs"
		ok := jsonRoute("6", 200, envelopeOf(t, lr))
		routes[path] = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("late") != "true" {
				w.WriteHeader(500)
				return
			}
			ok(w, r)
		}
		got, err := c.TaskLateLogs(bg, taskA)
		if err != nil || string(got.Data) != string(data) || !got.Incomplete {
			t.Fatalf("late logs = %+v %v", got, err)
		}
		routes[path] = jsonRoute("6", 404, `{"error":{"code":"not_found","message":"task has no late result","details":{"reason":"no_late_result"}}}`)
		if _, err := c.TaskLateLogs(bg, taskA); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("no late result = %v", err)
		}
		lr.TaskID = taskB
		routes[path] = jsonRoute("6", 200, envelopeOf(t, lr))
		if _, err := c.TaskLateLogs(bg, taskA); err == nil {
			t.Fatal("another task's late logs were accepted")
		}
		routes[path] = jsonRoute("6", 201, envelopeOf(t, lr))
		if _, err := c.TaskLateLogs(bg, taskA); err == nil {
			t.Fatal("an unexpected status was accepted")
		}
		if _, err := c.TaskLateLogs(bg, "t_bad"); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("bad id = %v", err)
		}
	})
	t.Run("trust", func(t *testing.T) {
		// A pinned client works; a wrong CA is never trusted and nothing is
		// retried without verification.
		pinned, err := New(s.url, Trust{CAPEM: ca.pem, Fingerprint: Fingerprint(ca.cert.Raw)})
		if err != nil {
			t.Fatal(err)
		}
		defer pinned.Close()
		routes[contract.PathTasks+"/"+taskA] = jsonRoute("6", 200, envelopeOf(t, contract.TaskShowResponse{Version: 6, Task: taskView(taskA, r, contract.TaskPending)}))
		if _, err := pinned.ShowTask(bg, taskA, 0); err != nil {
			t.Fatal(err)
		}
		other := newTestCA(t)
		wrong, err := New(s.url, Trust{CAPEM: other.pem})
		if err != nil {
			t.Fatal(err)
		}
		defer wrong.Close()
		before := hits.Load()
		if _, err := wrong.Dispatch(bg, r); contract.CodeOf(err) != contract.CodeTrustFailed || hits.Load() != before {
			t.Fatalf("wrong CA = %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		// A response delivered after the operation deadline is the
		// deadline, not the plane's answer.
		dc, err := New("https://127.0.0.1:1", Trust{CAPEM: ca.pem})
		if err != nil {
			t.Fatal(err)
		}
		defer dc.Close()
		shortenOperationTimeout(t, 50*time.Millisecond)
		body := envelopeOf(t, contract.DispatchResponse{Version: 6, TaskID: taskA, Task: ptrView(taskView(taskA, r, contract.TaskPending))})
		var closed atomic.Bool
		lt := &lateTransport{respond: lateResponse(202, http.Header{contract.ProtocolHeader: {"6"}}, body, &closed)}
		dc.http.Transport = lt
		_, err = dc.Dispatch(bg, r)
		wantLateTimeout(t, "dispatch", lt, &closed, err)
	})
}

// ptrView is v's address (a dispatch response's task).
func ptrView(v contract.TaskView) *contract.TaskView { return &v }
