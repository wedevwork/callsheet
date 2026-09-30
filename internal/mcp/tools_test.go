package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

var (
	nodeA = "n_" + strings.Repeat("a", 32)
	taskA = "t_" + strings.Repeat("a", 32)
	taskB = "t_" + strings.Repeat("b", 32)
	opA   = strings.Repeat("e", 32)
)

func mustEncode(t *testing.T, v any) string {
	t.Helper()
	b, err := contract.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func roleView(id string, order int) contract.RoleView {
	return contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: contract.RoleConfig{ID: id, Name: "impl", Node: nodeA, Adapter: "fake",
		Instruction: "/srv/i.md", Runbook: "/srv/r.md", Model: "m", Effort: "low", Concurrency: 1, Timeout: 2 * time.Hour, HasTimeout: true}, RegistrationOrder: order},
		NodeLiveness: "online", CanAccept: true}
}

func taskView(id string) contract.TaskView {
	return contract.TaskView{TaskID: id, State: contract.TaskRunning, CreatedAt: "2026-09-28T12:00:00Z", Request: contract.DispatchRequest{
		Target: contract.TaskTarget{Kind: contract.TargetID, Value: "r"}, Goal: "g", Acceptance: "a", Payload: []string{},
		RequestedBy: contract.RequestedBy{Name: "n", Version: "v", Hostname: "h"}}}
}

// happy scripts every operation with a small successful answer.
func happy(f *fakeClient) {
	f.listNodes = func(context.Context) ([]contract.Node, error) {
		return []contract.Node{{ID: nodeA, Liveness: "online"}}, nil
	}
	f.showNode = func(_ context.Context, id string) (contract.Node, error) {
		return contract.Node{ID: id, Liveness: "offline"}, nil
	}
	f.addRole = func(_ context.Context, rc contract.RoleConfig) (contract.RoleView, error) {
		return contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: rc.Resolved(), RegistrationOrder: 1}}, nil
	}
	f.setRole = func(_ context.Context, id string, p contract.RolePatch) (contract.RoleView, error) {
		return roleView(id, 1), nil
	}
	f.listRoles = func(context.Context) ([]contract.RoleView, error) {
		return []contract.RoleView{roleView("b", 2), roleView("a", 1)}, nil
	}
	f.showRole = func(_ context.Context, id string) (contract.RoleView, error) { return roleView(id, 1), nil }
	f.removeRole = func(_ context.Context, id string, force bool, op string) (contract.RoleRemoveResult, error) {
		return contract.RoleRemoveResult{Completed: &contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: id}}, nil
	}
	f.dispatch = func(_ context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
		return taskView(taskA), nil
	}
	f.dispatchWait = func(_ context.Context, req contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error) {
		wr := contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitStillRunning, EffectiveWaitMS: int(wait / time.Millisecond),
			Tasks: []contract.WaitRow{{TaskID: taskA, State: contract.TaskRunning}}}
		return contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: taskA, WaitResult: &wr}, nil
	}
	f.listTasks = func(context.Context, string, int) ([]contract.TaskSummary, *string, error) { return nil, nil, nil }
	f.showTask = func(_ context.Context, id string, _ int) (contract.TaskView, error) { return taskView(id), nil }
	f.taskLogs = func(_ context.Context, id string) (contract.TaskLogsResponse, error) {
		return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id}, nil
	}
	f.taskLateLogs = f.taskLogs
	f.cancelTask = func(_ context.Context, id string) (contract.CancelResponse, error) {
		return contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: id, Accepted: true, Task: taskView(id)}, nil
	}
	f.waitTasks = func(_ context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
		rows := make([]contract.WaitRow, len(ids))
		for i, id := range ids {
			rows[i] = contract.WaitRow{TaskID: id, State: contract.TaskRunning}
		}
		return contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitStillRunning, EffectiveWaitMS: int(wait / time.Millisecond), Tasks: rows}, nil
	}
}

// ask runs one tools/call on a ready harness and returns its answer.
func (h *harness) ask(name, args string) toolAnswer {
	h.t.Helper()
	h.nextID++
	h.call(h.nextID, name, args)
	a := h.answer()
	// The writer finished (its progress timer disarmed) before the test
	// may move the clock again.
	h.await(StageWritten, "n"+itoa(h.nextID))
	return a
}

// FP-2: every contract code maps one to one; Cause and Go error strings
// never reach stdout; errors keep the session usable.
func TestErrorMapping(t *testing.T) {
	h := start(t)
	h.ready()
	var next error
	h.fake.showNode = func(context.Context, string) (contract.Node, error) { return contract.Node{}, next }
	secret := errors.New("SECRET-CAUSE raw HTTP body")
	for code := range knownCodes {
		next = &contract.Error{Code: code, Message: "msg " + string(code), Details: map[string]any{"field": "id", "candidates": []any{map[string]any{"role_id": "r"}}}, Cause: secret}
		a := h.ask(toolNodeShow, `{"id":"`+nodeA+`"}`)
		want := `{"error":{"code":"` + string(code) + `","message":"msg ` + string(code) + `","details":{"candidates":[{"role_id":"r"}],"field":"id"}}}`
		if !a.isError || a.text != want {
			t.Fatalf("%s: %+v", code, a)
		}
	}
	for _, c := range []struct {
		err  error
		code contract.Code
		msg  string
	}{
		{errors.New("SECRET go error"), contract.CodeInternal, fixedInternal},
		{fmt.Errorf("wrapped: %w", &contract.Error{Code: "weird", Message: "SECRET"}), contract.CodeInternal, fixedInternal},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), contract.CodeUnavailable, "deadline"},
		{fmt.Errorf("wrapped: %w", contract.Wrap(contract.CodeConflict, "busy", secret)), contract.CodeConflict, "busy"},
	} {
		next = c.err
		e := h.ask(toolNodeShow, `{"id":"`+nodeA+`"}`).errorOf(t)
		if e.Code != c.code || !strings.Contains(e.Message, c.msg) || strings.Contains(e.Message, "SECRET") {
			t.Fatalf("%v: %+v", c.err, e)
		}
	}
	h.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if rawID(t, h.nextRaw()) != `"p"` {
		t.Fatal("session unusable after errors")
	}
	// Frames over their bounds: an error over 512 KiB becomes the fixed
	// internal error, an answer over its bound a small internal error.
	id := requestID{raw: []byte("1"), key: "n1"}
	big := contract.New(contract.CodeConflict, strings.Repeat("x", MaxErrorFrameBytes))
	if f := errorResultFrame(id, big); len(f) > 1024 || !bytes.Contains(f, []byte("internal")) {
		t.Fatalf("oversized error frame %d bytes", len(f))
	}
	if f := successFrame(id, toolResult{text: bytes.Repeat([]byte("x"), 200), wireLimit: 100}); !bytes.Contains(f, []byte(`\"code\":\"internal\"`)) ||
		!bytes.Contains(f, []byte("exceeds its 100-byte bound")) {
		t.Fatalf("over wire limit %s", f)
	}
	// The global bound holds the largest contract payload at the worst
	// sixfold escaping plus its envelope.
	if contract.MaxTaskLogsBytes*6+1<<10 > MaxFrameBytes || textFrameSize(id, bytes.Repeat([]byte{0x01}, 1000), false) != len(framePrefix)+1+len(resultOpen)+6000+len(resultCloseOK) {
		t.Fatal("frame bound arithmetic")
	}
}

// FP-2: trust is resolved afresh for every admitted call (one factory
// call and one Close each); a trust failure is a tool error and the next
// call resolves again.
func TestFactoryPerCall(t *testing.T) {
	fake := &fakeClient{}
	happy(fake)
	fail := true
	calls := 0
	h2 := start(t, withFactory(func(ctx context.Context) (Client, error) {
		calls++
		if fail {
			fail = false
			return nil, contract.New(contract.CodeTrustFailed, "connection not trusted: the --ca file x does not exist")
		}
		return fake, nil
	}))
	h2.ready()
	if e := h2.ask(toolNodeLs, "").errorOf(t); e.Code != contract.CodeTrustFailed {
		t.Fatalf("trust %+v", e)
	}
	for range 3 {
		if a := h2.ask(toolNodeLs, ""); a.isError {
			t.Fatalf("%+v", a)
		}
	}
	if calls != 4 || fake.closes != 3 {
		t.Fatalf("factory %d closes %d", calls, fake.closes)
	}
}

// FP-2/FP-5: mutations are never retried here; a lost answer carries the
// tool's ambiguity guidance.
func TestNoRetry(t *testing.T) {
	h := start(t)
	h.ready()
	lost := contract.Wrap(contract.CodeUnavailable, "cannot reach the plane at https://p: timed out", errors.New("i/o timeout"))
	h.fake.dispatch = func(context.Context, contract.DispatchRequest) (contract.TaskView, error) {
		return contract.TaskView{}, lost
	}
	h.fake.dispatchWait = func(context.Context, contract.DispatchRequest, time.Duration) (contract.DispatchResponse, error) {
		return contract.DispatchResponse{}, lost
	}
	h.fake.removeRole = func(context.Context, string, bool, string) (contract.RoleRemoveResult, error) {
		return contract.RoleRemoveResult{}, lost
	}
	d := `{"target":{"kind":"name","value":"impl"},"goal":"g","acceptance":"a"`
	for _, args := range []string{d + `}`, d + `,"wait":"1s"}`} {
		e := h.ask(toolDispatch, args).errorOf(t)
		if e.Code != contract.CodeUnavailable || !strings.Contains(e.Message, "inspect task_ls before dispatching again") {
			t.Fatalf("dispatch %+v", e)
		}
	}
	if e := h.ask(toolRoleRm, `{"id":"r","force":true}`).errorOf(t); !strings.Contains(e.Message, "read role_show for its removal operation token") {
		t.Fatalf("force rm %+v", e)
	}
	if e := h.ask(toolRoleRm, `{"id":"r"}`).errorOf(t); strings.Contains(e.Message, "role_show") {
		t.Fatalf("plain rm %+v", e)
	}
	// A plane's own unavailable answer is definitive: no guidance.
	full := contract.TaskError(contract.CodeUnavailable, "", contract.ReasonNoCapacity, "no candidate role can accept the task now; nothing was dispatched")
	h.fake.dispatch = func(context.Context, contract.DispatchRequest) (contract.TaskView, error) {
		return contract.TaskView{}, full
	}
	if e := h.ask(toolDispatch, d+`}`).errorOf(t); e.Message != full.Message || e.Details["reason"] != contract.ReasonNoCapacity {
		t.Fatalf("no capacity %+v", e)
	}
	h.fake.dispatch = func(context.Context, contract.DispatchRequest) (contract.TaskView, error) {
		return contract.TaskView{}, context.DeadlineExceeded
	}
	if e := h.ask(toolDispatch, d+`}`).errorOf(t); !strings.Contains(e.Message, "inspect task_ls") {
		t.Fatalf("deadline %+v", e)
	}
	if h.fake.count("Dispatch") != 3 || h.fake.count("DispatchWithWait") != 1 || h.fake.count("RemoveRole") != 2 || h.fake.count("ListTasks") != 0 {
		t.Fatalf("calls %v", h.fake.called())
	}
}

// FP-3/FP-4: node and role tools: typed conversions, omission versus
// zero, immutable fields, force/operation rules and exact serialization.
func TestRegistryTools(t *testing.T) {
	h := start(t)
	happy(h.fake)
	var gotRC contract.RoleConfig
	h.fake.addRole = func(_ context.Context, rc contract.RoleConfig) (contract.RoleView, error) {
		gotRC = rc
		return contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: rc.Resolved(), RegistrationOrder: 3}}, nil
	}
	var gotPatch contract.RolePatch
	h.fake.setRole = func(_ context.Context, id string, p contract.RolePatch) (contract.RoleView, error) {
		gotPatch = p
		return roleView(id, 1), nil
	}
	var gotRm []any
	var rmResult contract.RoleRemoveResult
	h.fake.removeRole = func(_ context.Context, id string, force bool, op string) (contract.RoleRemoveResult, error) {
		gotRm = []any{id, force, op}
		return rmResult, nil
	}
	h.ready()
	if a := h.ask(toolNodeLs, "{}"); a.text != mustEncode(t, contract.NodeListResponse{Version: contract.ProtocolVersion, Nodes: []contract.Node{{ID: nodeA, Liveness: "online"}}}) {
		t.Fatalf("node_ls %s", a.text)
	}
	if a := h.ask(toolNodeShow, `{"id":"`+nodeA+`"}`); a.text != mustEncode(t, contract.NodeResponse{Version: contract.ProtocolVersion, Node: contract.Node{ID: nodeA, Liveness: "offline"}}) {
		t.Fatalf("node_show %s", a.text)
	}
	add := `{"id":"worker-a","name":"impl","node":"` + nodeA + `","adapter":"fake","instruction":"/srv/i.md","runbook":"/srv/r.md","model":"a model","effort":"medium","concurrency":2`
	a := h.ask(toolRoleAdd, add+`}`)
	want := contract.RoleConfig{ID: "worker-a", Name: "impl", Node: nodeA, Adapter: "fake", Instruction: "/srv/i.md", Runbook: "/srv/r.md", Model: "a model", Effort: "medium", Concurrency: 2}
	if a.isError || gotRC != want || a.text != mustEncode(t, contract.RoleResponse{Version: contract.ProtocolVersion, Role: contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: want.Resolved(), RegistrationOrder: 3}}}) {
		t.Fatalf("role_add %+v %+v", a, gotRC)
	}
	for _, c := range []struct {
		timeout string
		want    time.Duration
	}{{"90m", 90 * time.Minute}, {"0", 0}} {
		if a := h.ask(toolRoleAdd, add+`,"timeout":"`+c.timeout+`"}`); a.isError || !gotRC.HasTimeout || gotRC.Timeout != c.want {
			t.Fatalf("timeout %s: %+v %+v", c.timeout, a, gotRC)
		}
	}
	before := h.factory.Load()
	for _, bad := range []string{
		`{}`, add + `,"extra":1}`, add + `,"timeout":null}`, add + `,"timeout":5}`, add + `,"timeout":"-1s"}`, add + `,"timeout":" "}`,
		strings.Replace(add, `"concurrency":2`, `"concurrency":0`, 1) + `}`, strings.Replace(add, `"concurrency":2`, `"concurrency":2147483648`, 1) + `}`,
		strings.Replace(add, `"concurrency":2`, `"concurrency":1.5`, 1) + `}`, strings.Replace(add, `"concurrency":2`, `"concurrency":"2"`, 1) + `}`,
		strings.Replace(add, `"fake"`, `"claude"`, 1) + `}`, strings.Replace(add, `/srv/i.md`, `relative.md`, 1) + `}`,
		strings.Replace(add, `"medium"`, `"max"`, 1) + `}`, strings.Replace(add, `"worker-a"`, `"Worker"`, 1) + `}`,
	} {
		if e := h.ask(toolRoleAdd, bad).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("role_add %s: %+v", bad, e)
		}
	}
	for _, bad := range []string{`{"id":"r"}`, `{"id":"r","node":"` + nodeA + `"}`, `{"id":"r","registration_order":2}`, `{"id":"R","name":"x"}`,
		`{"id":"r","concurrency":0}`, `{"id":"r","adapter":"nope"}`, `{"id":"r","timeout":"x"}`, `{"id":"r","name":null}`} {
		if e := h.ask(toolRoleSet, bad).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("role_set %s: %+v", bad, e)
		}
	}
	if h.factory.Load() != before {
		t.Fatal("invalid role arguments contacted the plane")
	}
	zero := time.Duration(0)
	if a := h.ask(toolRoleSet, `{"id":"r","timeout":"0"}`); a.isError || gotPatch.Timeout == nil || *gotPatch.Timeout != zero || gotPatch.Name != nil || gotPatch.Concurrency != nil {
		t.Fatalf("role_set zero timeout %+v %+v", a, gotPatch)
	}
	if a := h.ask(toolRoleSet, `{"id":"r","name":"x","concurrency":3,"adapter":"fake"}`); a.isError || *gotPatch.Name != "x" || *gotPatch.Concurrency != 3 || gotPatch.Timeout != nil {
		t.Fatalf("role_set %+v %+v", a, gotPatch)
	}
	if a := h.ask(toolRoleLs, ""); a.text != mustEncode(t, contract.RoleListResponse{Version: contract.ProtocolVersion, Roles: []contract.RoleView{roleView("b", 2), roleView("a", 1)}}) {
		t.Fatalf("role_ls order %s", a.text)
	}
	if a := h.ask(toolRoleShow, `{"id":"r"}`); a.text != mustEncode(t, contract.RoleResponse{Version: contract.ProtocolVersion, Role: roleView("r", 1)}) {
		t.Fatalf("role_show %s", a.text)
	}
	if e := h.ask(toolRoleShow, `{"id":"-r"}`).errorOf(t); e.Code != contract.CodeInvalidArgument {
		t.Fatalf("role_show bad id %+v", e)
	}
	rmResult = contract.RoleRemoveResult{Completed: &contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: "r"}}
	if a := h.ask(toolRoleRm, `{"id":"r"}`); a.text != `{"version":5,"removed":"r"}` || fmt.Sprint(gotRm) != "[r false ]" {
		t.Fatalf("role_rm %+v %v", a, gotRm)
	}
	pending := contract.RoleRemovePendingResponse{Version: contract.ProtocolVersion, OperationID: opA, RoleID: "r", RegistrationOrder: 4, Removing: true}
	rmResult = contract.RoleRemoveResult{Pending: &pending}
	a = h.ask(toolRoleRm, `{"id":"r","force":true,"operation":"`+opA+`"}`)
	if a.isError || a.text != `{"version":5,"operation_id":"`+opA+`","role_id":"r","registration_order":4,"removing":true}` || fmt.Sprint(gotRm) != "[r true "+opA+"]" {
		t.Fatalf("role_rm pending %+v %v", a, gotRm)
	}
	rmResult = contract.RoleRemoveResult{}
	if e := h.ask(toolRoleRm, `{"id":"r","force":true}`).errorOf(t); e.Code != contract.CodeInternal {
		t.Fatalf("no branch %+v", e)
	}
	for _, bad := range []string{`{"id":"r","operation":"` + opA + `"}`, `{"id":"r","force":false,"operation":"` + opA + `"}`,
		`{"id":"r","force":true,"operation":"xyz"}`, `{"id":"r","force":"yes"}`, `{"id":"r","force":null}`, `{"id":"--"}`} {
		if e := h.ask(toolRoleRm, bad).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("role_rm %s: %+v", bad, e)
		}
	}
}

// FP-5: dispatch builds exactly one request from the exclusive target,
// the bounded texts and payload, the override and the session's
// attribution; it is sent once.
func TestDispatchTool(t *testing.T) {
	h := start(t)
	happy(h.fake)
	var got contract.DispatchRequest
	var gotWait time.Duration
	h.fake.dispatch = func(_ context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
		got = req
		return taskView(taskA), nil
	}
	h.fake.dispatchWait = func(_ context.Context, req contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error) {
		got, gotWait = req, wait
		v := taskView(taskA)
		v.State = contract.TaskSucceeded
		wr := contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 7, Winner: taskA, Task: &v}
		return contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: taskA, WaitResult: &wr}, nil
	}
	h.ready()
	base := `{"target":{"kind":"id","value":"worker-a"},"goal":"fix it","acceptance":"tests pass"`
	a := h.ask(toolDispatch, base+`,"payload":["repo://x"]}`)
	v := taskView(taskA)
	if a.isError || a.text != mustEncode(t, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: taskA, Task: &v}) || !strings.HasSuffix(a.text, `"wait_result":null}`) {
		t.Fatalf("dispatch %+v", a)
	}
	want := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "worker-a"}, Goal: "fix it", Acceptance: "tests pass",
		Payload: []string{"repo://x"}, RequestedBy: contract.RequestedBy{Name: "test-client", Version: "1.2.3", Hostname: "coord-host"}}
	if mustEncode(t, got) != mustEncode(t, want) {
		t.Fatalf("request %s", mustEncode(t, got))
	}
	a = h.ask(toolDispatch, `{"target":{"kind":"name","value":"impl"},"goal":"g","acceptance":"a","override":{"model":"m2","effort":"high","timeout":"0"},"wait":"0"}`)
	if a.isError || got.Override == nil || *got.Override.Model != "m2" || *got.Override.Timeout != 0 || gotWait != 0 || got.Payload == nil || len(got.Payload) != 0 {
		t.Fatalf("override %+v %+v", a, got)
	}
	var top map[string]json.RawMessage
	json.Unmarshal([]byte(a.text), &top)
	if _, hasTask := top["task"]; hasTask || len(top) != 3 || !strings.HasPrefix(string(top["wait_result"]), `{"version":5,"status":"terminal"`) {
		t.Fatalf("dispatch-with-wait %s", a.text)
	}
	before := h.factory.Load()
	long := strings.Repeat("x", contract.MaxPointerBytes+1)
	many := `["` + strings.Repeat(`p","`, contract.MaxPayloadPointers) + `p"]`
	for _, bad := range []string{
		`{}`, base + `,"requested_by":{"name":"a","version":"b","hostname":"c"}}`, base + `,"workspace":"w"}`, base + `,"base":"b"}`,
		`{"target":{"kind":"both","value":"x"},"goal":"g","acceptance":"a"}`, `{"target":{"kind":"id"},"goal":"g","acceptance":"a"}`,
		`{"target":{"kind":"id","value":"x","name":"y"},"goal":"g","acceptance":"a"}`, `{"target":"x","goal":"g","acceptance":"a"}`,
		base + `,"payload":` + many + `}`, base + `,"payload":["` + long + `"]}`, base + `,"payload":[""]}`, base + `,"payload":null}`,
		base + `,"override":{}}`, base + `,"override":{"model":" "}}`, base + `,"override":{"timeout":"-1s"}}`, base + `,"override":{"workspace":"x"}}`,
		base + `,"wait":"6m"}`, base + `,"wait":"-1s"}`, base + `,"wait":5}`, base + `,"wait":" 1s"}`,
		`{"target":{"kind":"id","value":"x"},"goal":"   ","acceptance":"a"}`, `{"target":{"kind":"id","value":"x"},"goal":"` + strings.Repeat("g", contract.MaxGoalBytes+1) + `","acceptance":"a"}`,
	} {
		if e := h.ask(toolDispatch, bad).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("dispatch %.80s: %+v", bad, e)
		}
	}
	if h.factory.Load() != before || h.fake.count("Dispatch")+h.fake.count("DispatchWithWait") != 2 {
		t.Fatalf("invalid dispatches reached the client: %v", h.fake.called())
	}
	// Attribution outside Callsheet's grammar is refused before any
	// mutation: a client name with a space, an unavailable or invalid
	// hostname.
	for _, c := range []struct {
		name, version string
		host          func() (string, error)
	}{
		{"Claude Code", "1", func() (string, error) { return "h", nil }},
		{"c", strings.Repeat("v", 129), func() (string, error) { return "h", nil }},
		{"c", "1", func() (string, error) { return "", errors.New("no hostname") }},
		{"c", "1", func() (string, error) { return "bad host", nil }},
	} {
		h2 := start(t, withHostname(c.host))
		happy(h2.fake)
		h2.send(`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"` + c.name + `","version":"` + c.version + `"}}}`)
		h2.next()
		h2.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		e := h2.ask(toolDispatch, base+`}`).errorOf(t)
		if e.Code != contract.CodeInvalidArgument || e.Details["field"] != "requested_by" || h2.factory.Load() != 0 {
			t.Fatalf("attribution %q: %+v", c.name, e)
		}
		if a := h2.ask(toolTaskShow, `{"id":"`+taskA+`"}`); a.isError {
			t.Fatalf("read tools refused: %+v", a)
		}
	}
}

// FP-6: task reads pass pagination and tail bounds unchanged, return the
// exact serialized responses and byte-exact logs, and keep missing late
// output not_found.
func TestTaskReadTools(t *testing.T) {
	h := start(t, withRealClock())
	happy(h.fake)
	var gotAfter string
	var gotLimit, gotLines int
	next := taskB
	h.fake.listTasks = func(_ context.Context, after string, limit int) ([]contract.TaskSummary, *string, error) {
		gotAfter, gotLimit = after, limit
		return []contract.TaskSummary{{TaskID: taskA, State: contract.TaskRunning}}, &next, nil
	}
	h.fake.showTask = func(_ context.Context, id string, lines int) (contract.TaskView, error) {
		gotLines = lines
		return taskView(id), nil
	}
	data := make([]byte, 128<<10)
	rand.Read(data)
	data = append(data, 0xff, 0xfe, '\n', 0x1b, '[', 0)
	h.fake.taskLogs = func(_ context.Context, id string) (contract.TaskLogsResponse, error) {
		return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id, Data: data, RetainedBytes: len(data), SourceBytes: len(data) + 9, DroppedBytes: 9, Truncated: true}, nil
	}
	h.fake.taskLateLogs = func(_ context.Context, id string) (contract.TaskLogsResponse, error) {
		return contract.TaskLogsResponse{}, contract.TaskError(contract.CodeNotFound, "", contract.ReasonNoLateResult, "task %s has no late result", id)
	}
	h.ready()
	a := h.ask(toolTaskLs, "")
	if a.isError || gotAfter != "" || gotLimit != contract.DefaultTaskListLimit ||
		a.text != mustEncode(t, contract.TaskListResponse{Version: contract.ProtocolVersion, Tasks: []contract.TaskSummary{{TaskID: taskA, State: contract.TaskRunning}}, NextAfter: &next}) {
		t.Fatalf("task_ls %+v", a)
	}
	if a := h.ask(toolTaskLs, `{"after":"`+taskA+`","limit":1}`); a.isError || gotAfter != taskA || gotLimit != 1 || !strings.Contains(a.text, `"next_after":"`+taskB+`"`) {
		t.Fatalf("page 2 %+v", a)
	}
	for _, bad := range []string{`{"limit":0}`, `{"limit":101}`, `{"after":"t_x"}`, `{"after":null}`, `{"cursor":"x"}`} {
		if e := h.ask(toolTaskLs, bad).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("task_ls %s %+v", bad, e)
		}
	}
	for _, c := range []struct {
		args  string
		lines int
	}{{`{"id":"` + taskA + `"}`, contract.DefaultTailLines}, {`{"id":"` + taskA + `","lines":0}`, 0}, {`{"id":"` + taskA + `","lines":200}`, 200}} {
		if a := h.ask(toolTaskShow, c.args); a.isError || gotLines != c.lines || a.text != mustEncode(t, contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: taskView(taskA)}) {
			t.Fatalf("task_show %s: %+v %d", c.args, a, gotLines)
		}
	}
	for _, bad := range []string{`{"id":"` + taskA + `","lines":201}`, `{"id":"` + taskA + `","lines":-1}`, `{"id":"T"}`, `{}`} {
		if e := h.ask(toolTaskShow, bad).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("task_show %s %+v", bad, e)
		}
	}
	a = h.ask(toolTaskLogs, `{"id":"`+taskA+`","late":false}`)
	r, err := contract.ParseTaskLogsResponse([]byte(a.text))
	if a.isError || err != nil || !bytes.Equal(r.Data, data) || !r.Truncated || r.DroppedBytes != 9 {
		t.Fatalf("task_logs %v", err)
	}
	if e := h.ask(toolTaskLogs, `{"id":"`+taskA+`","late":true}`).errorOf(t); e.Code != contract.CodeNotFound || e.Details["reason"] != contract.ReasonNoLateResult {
		t.Fatalf("late %+v", e)
	}
	if h.fake.count("TaskLateLogs") != 1 || h.fake.count("TaskLogs") != 1 {
		t.Fatalf("calls %v", h.fake.called())
	}
	if a := h.ask(toolTaskCancel, `{"id":"`+taskA+`"}`); a.isError || a.text != mustEncode(t, contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: taskA, Accepted: true, Task: taskView(taskA)}) {
		t.Fatalf("task_cancel %+v", a)
	}
	if e := h.ask(toolTaskCancel, `{"id":"x"}`).errorOf(t); e.Code != contract.CodeInvalidArgument {
		t.Fatalf("task_cancel bad %+v", e)
	}
}

// ---- Schemas ----

// validate is a minimal JSON Schema checker for the keywords the tool
// schemas use; it rejects any keyword it does not know.
func validate(s map[string]any, v any, where string) error {
	for k := range s {
		switch k {
		case "type", "properties", "required", "additionalProperties", "minProperties", "pattern", "minLength", "maxLength",
			"minimum", "maximum", "enum", "items", "minItems", "maxItems", "uniqueItems", "description", "default":
		default:
			return fmt.Errorf("%s: unsupported schema keyword %s", where, k)
		}
	}
	num := func(k string) (int64, bool) {
		n, ok := s[k].(json.Number)
		if !ok {
			return 0, false
		}
		i, err := n.Int64()
		return i, err == nil
	}
	switch s["type"] {
	case "object":
		o, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: not an object", where)
		}
		props, _ := s["properties"].(map[string]any)
		for k, pv := range o {
			ps, known := props[k].(map[string]any)
			if !known {
				if s["additionalProperties"] == false {
					return fmt.Errorf("%s: additional property %s", where, k)
				}
				continue
			}
			if err := validate(ps, pv, where+"."+k); err != nil {
				return err
			}
		}
		req, _ := s["required"].([]any)
		for _, r := range req {
			if _, ok := o[r.(string)]; !ok {
				return fmt.Errorf("%s: missing %s", where, r)
			}
		}
		if n, ok := num("minProperties"); ok && int64(len(o)) < n {
			return fmt.Errorf("%s: fewer than %d properties", where, n)
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: not a string", where)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			return fmt.Errorf("%s: pattern", where)
		}
		if n, ok := num("minLength"); ok && int64(utf8.RuneCountInString(str)) < n {
			return fmt.Errorf("%s: too short", where)
		}
		if n, ok := num("maxLength"); ok && int64(utf8.RuneCountInString(str)) > n {
			return fmt.Errorf("%s: too long", where)
		}
		if e, ok := s["enum"].([]any); ok {
			found := false
			for _, x := range e {
				found = found || x == str
			}
			if !found {
				return fmt.Errorf("%s: not in enum", where)
			}
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("%s: not an integer", where)
		}
		i, err := n.Int64()
		if err != nil {
			return fmt.Errorf("%s: not an integer", where)
		}
		if lo, ok := num("minimum"); ok && i < lo {
			return fmt.Errorf("%s: below minimum", where)
		}
		if hi, ok := num("maximum"); ok && i > hi {
			return fmt.Errorf("%s: above maximum", where)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: not a boolean", where)
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: not an array", where)
		}
		if n, ok := num("minItems"); ok && int64(len(a)) < n {
			return fmt.Errorf("%s: too few items", where)
		}
		if n, ok := num("maxItems"); ok && int64(len(a)) > n {
			return fmt.Errorf("%s: too many items", where)
		}
		seen := map[string]bool{}
		for i, x := range a {
			if s["uniqueItems"] == true {
				k := fmt.Sprint(x)
				if seen[k] {
					return fmt.Errorf("%s: duplicate items", where)
				}
				seen[k] = true
			}
			if err := validate(s["items"].(map[string]any), x, fmt.Sprintf("%s[%d]", where, i)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s: unsupported type %v", where, s["type"])
	}
	return nil
}

// FP-1: every tool's schema agrees with the tool's own validation on
// schema-valid and schema-invalid examples; contract-invalid combinations
// that the schema cannot express are still refused by the contract.
func TestSchemas(t *testing.T) {
	h := start(t)
	happy(h.fake)
	h.fake.ws = happyWs
	h.ready()
	h.send(`{"jsonrpc":"2.0","id":"l","method":"tools/list"}`)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	dec := json.NewDecoder(bytes.NewReader(h.nextRaw()))
	dec.UseNumber()
	if err := dec.Decode(&list); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]map[string]any{}
	for _, tl := range list.Result.Tools {
		schemas[tl.Name] = tl.InputSchema
	}
	add := `"id":"w","name":"impl","node":"` + nodeA + `","adapter":"fake","instruction":"/i","runbook":"/r","model":"m","effort":"low","concurrency":1`
	target := `"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a"`
	cases := []struct {
		tool, args string
		schemaOK   bool
		accepted   bool
	}{
		{toolNodeLs, `{}`, true, true},
		{toolNodeLs, `{"x":1}`, false, false},
		{toolNodeShow, `{"id":"` + nodeA + `"}`, true, true},
		{toolNodeShow, `{"id":"n_x"}`, false, false},
		{toolNodeShow, `{}`, false, false},
		{toolRoleAdd, `{` + add + `}`, true, true},
		{toolRoleAdd, `{` + add + `,"timeout":"2h"}`, true, true},
		{toolRoleAdd, `{` + strings.Replace(add, `"concurrency":1`, `"concurrency":0`, 1) + `}`, false, false},
		{toolRoleAdd, `{` + add + `,"timeout":7200}`, false, false},
		{toolRoleAdd, `{` + strings.Replace(add, `"/i"`, `"i"`, 1) + `}`, true, false},    // relative path: contract only
		{toolRoleAdd, `{` + strings.Replace(add, `"low"`, `"max"`, 1) + `}`, true, false}, // effort not of the adapter: contract only
		{toolRoleSet, `{"id":"w","model":"m2"}`, true, true},
		{toolRoleSet, `{"id":"w"}`, false, false},
		{toolRoleSet, `{"id":"w","node":"` + nodeA + `"}`, false, false},
		{toolRoleSet, `{"id":"w","timeout":"abc"}`, true, false}, // duration grammar: contract only
		{toolRoleLs, `{}`, true, true},
		{toolRoleShow, `{"id":"w"}`, true, true},
		{toolRoleShow, `{"id":"W"}`, false, false},
		{toolRoleRm, `{"id":"w","force":true,"operation":"` + opA + `"}`, true, true},
		{toolRoleRm, `{"id":"w","operation":"` + opA + `"}`, true, false}, // operation without force: contract only
		{toolRoleRm, `{"id":"w","force":1}`, false, false},
		{toolDispatch, `{` + target + `}`, true, true},
		{toolDispatch, `{` + target + `,"payload":["p"],"override":{"effort":"high"},"wait":"30s"}`, true, true},
		{toolDispatch, `{` + target + `,"override":{}}`, false, false},
		{toolDispatch, `{"target":{"kind":"any","value":"w"},"goal":"g","acceptance":"a"}`, false, false},
		{toolDispatch, `{` + target + `,"requested_by":{}}`, false, false},
		{toolDispatch, `{"target":{"kind":"id","value":"w"},"goal":" ","acceptance":"a"}`, true, false}, // blank goal: contract only
		{toolDispatch, `{` + target + `,"payload":["` + strings.Repeat("é", 1100) + `"]}`, true, false}, // 1100 characters, 2200 bytes: contract only
		{toolDispatch, `{` + target + `,"wait":"10m"}`, true, false},
		{toolTaskLs, `{"limit":100}`, true, true},
		{toolTaskLs, `{"limit":101}`, false, false},
		{toolTaskLs, `{"after":"t_1"}`, false, false},
		{toolTaskShow, `{"id":"` + taskA + `","lines":0}`, true, true},
		{toolTaskShow, `{"id":"` + taskA + `","lines":201}`, false, false},
		{toolTaskLogs, `{"id":"` + taskA + `","late":true}`, true, true},
		{toolTaskLogs, `{"id":"` + taskA + `","late":"yes"}`, false, false},
		{toolTaskCancel, `{"id":"` + taskA + `"}`, true, true},
		{toolTaskCancel, `{"id":"` + taskA + `","force":true}`, false, false},
		{toolTaskWait, `{"task_ids":["` + taskA + `","` + taskB + `"],"wait":"0"}`, true, true},
		{toolTaskWait, `{"task_ids":[]}`, false, false},
		{toolTaskWait, `{"task_ids":["` + taskA + `","` + taskA + `"]}`, false, false},
		{toolTaskWait, `{"task_ids":["` + taskA + `"],"wait":"-1s"}`, true, false},
	}
	// Iteration 09a: the eight workspace tools.
	cases = append(cases, wsSchemaCases()...)
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.tool] = true
		var v any
		d := json.NewDecoder(strings.NewReader(c.args))
		d.UseNumber()
		d.Decode(&v)
		err := validate(schemas[c.tool], v, c.tool)
		if (err == nil) != c.schemaOK {
			t.Fatalf("%s %s: schema %v", c.tool, c.args, err)
		}
		a := h.ask(c.tool, c.args)
		if a.isError == c.accepted {
			t.Fatalf("%s %s: answer %+v", c.tool, c.args, a)
		}
		if !c.accepted {
			if e := a.errorOf(t); e.Code != contract.CodeInvalidArgument {
				t.Fatalf("%s %s: %+v", c.tool, c.args, e)
			}
		}
	}
	if len(covered) != len(ToolNames) {
		t.Fatalf("schema cases cover %d tools", len(covered))
	}
}

// FP-2: the production factory resolves trust afresh per call over the
// real verified path: a good CA builds a client, a missing CA file and a
// wrong pin are trust_failed, and nothing is cached between calls.
func TestPlaneFactory(t *testing.T) {
	h := testkit.NewHarness(t)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, h.CAPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	f := PlaneFactory(h.URL, ca, "")
	for range 2 {
		c, err := f(context.Background())
		if err != nil {
			t.Fatalf("good CA: %v", err)
		}
		c.Close()
	}
	os.Remove(ca)
	if _, err := f(context.Background()); contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("removed CA file: %v", err)
	}
	if _, err := PlaneFactory(h.URL, "", "sha256:"+strings.Repeat("0", 64))(context.Background()); contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("wrong pin: %v", err)
	}
	if _, err := PlaneFactory("https://127.0.0.1:1", "", "sha256:"+strings.Repeat("0", 64))(context.Background()); contract.CodeOf(err) != contract.CodeUnavailable {
		t.Fatalf("refused: %v", err)
	}
}
