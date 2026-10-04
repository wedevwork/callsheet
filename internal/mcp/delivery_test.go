package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Iteration 10c (UT-C4): the coordinator's task forms over MCP. ws_pull,
// ws_status and ws_diff stay single flat object schemas (no combinators,
// no top-level required, the exact union of both forms' properties) whose
// descriptions explain the forms; the runtime parsers enforce exactly one
// complete form, no mixed keys and the pagination binding. dispatch gains
// the optional workspace selection, forwarded unchanged.

// Every property of the two-form tools, by tool.
var formProps = map[string][]string{
	toolWsPull:   {"name", "path", "ref", "task_id"},
	toolWsStatus: {"after", "generation", "instance", "limit", "name", "task_id"},
	toolWsDiff:   {"after", "base", "generation", "instance", "limit", "name", "target", "task_id"},
}

func TestDeliverySchemaShape(t *testing.T) {
	if len(ToolNames) != 23 || ToolNames[7] != toolDispatch || ToolNames[19] != toolWsStatus || ToolNames[20] != toolWsDiff || ToolNames[22] != toolWsPull {
		t.Fatalf("tool order %v", ToolNames)
	}
	for tool, want := range formProps {
		s := schemaFor(tool)
		b, _ := json.Marshal(s)
		props, _ := s["properties"].(schema)
		var keys []string
		for k := range props {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		_, required := s["required"]
		if s["type"] != "object" || s["additionalProperties"] != false || required || !slices.Equal(keys, want) {
			t.Fatalf("%s schema %s", tool, b)
		}
		for _, comb := range []string{"oneOf", "anyOf", "allOf", "not", "$ref", "if"} {
			if strings.Contains(string(b), `"`+comb+`"`) {
				t.Fatalf("%s uses %s", tool, comb)
			}
		}
		desc := func(p string) string { return props[p].(schema)["description"].(string) }
		if d := desc("task_id"); !strings.Contains(d, "Task form") || !strings.Contains(d, "excludes") || props["task_id"].(schema)["pattern"] != taskIDPattern {
			t.Fatalf("%s task_id %q", tool, d)
		}
		if d := desc("name"); !strings.Contains(d, "excludes task_id") || !strings.Contains(d, "form") {
			t.Fatalf("%s name %q", tool, d)
		}
	}
	pull := schemaFor(toolWsPull)["properties"].(schema)
	if d := pull["path"].(schema)["description"].(string); !strings.HasPrefix(d, "Optional in both forms") {
		t.Fatalf("pull path %q", d)
	}
	if d := pull["ref"].(schema)["description"].(string); !strings.Contains(d, "with name") || !strings.Contains(d, "bare task ID") || !strings.Contains(d, "refs/heads/t_") {
		t.Fatalf("pull ref %q", d)
	}
	status := schemaFor(toolWsStatus)["properties"].(schema)
	diff := schemaFor(toolWsDiff)["properties"].(schema)
	for _, p := range []string{"after", "instance", "generation", "limit"} {
		if d := status[p].(schema)["description"].(string); !strings.HasPrefix(d, "Name form only") {
			t.Fatalf("status %s %q", p, d)
		}
		if d := diff[p].(schema)["description"].(string); !strings.HasPrefix(d, "Either form") {
			t.Fatalf("diff %s %q", p, d)
		}
	}
	for _, p := range []string{"after", "instance", "generation"} {
		if !strings.Contains(status[p].(schema)["description"].(string), "all or none") || !strings.Contains(diff[p].(schema)["description"].(string), "all or none") {
			t.Fatalf("%s lacks the all-or-none rule", p)
		}
	}
	if !strings.Contains(diff["limit"].(schema)["description"].(string), "independently optional") {
		t.Fatal("diff limit")
	}
	for _, p := range []string{"base", "target"} {
		if d := diff[p].(schema)["description"].(string); !strings.Contains(d, "Explicit form") || !strings.Contains(d, "excludes task_id") {
			t.Fatalf("diff %s %q", p, d)
		}
	}
	// dispatch: three optional properties with their local constraints;
	// the required members are unchanged.
	d := schemaFor(toolDispatch)
	dp := d["properties"].(schema)
	if !slices.Equal(d["required"].([]string), []string{"target", "goal", "acceptance"}) || dp["workspace"].(schema)["pattern"] != slugPattern ||
		dp["workspace_instance"].(schema)["pattern"] != tokenPattern || dp["base"].(schema)["pattern"] != taskBasePattern ||
		!strings.Contains(dp["base"].(schema)["pattern"].(string), "t_[0-9a-f]{32}|") || basePattern == taskBasePattern {
		t.Fatalf("dispatch schema %v", d)
	}
	tools := newTools(NewBudget(DefaultBudget))
	for _, tool := range []string{toolWsStatus, toolWsDiff} {
		if a := toolNamed(tools, tool).annotations; a["readOnlyHint"] != true {
			t.Fatalf("%s annotations %v", tool, a)
		}
	}
	if a := toolNamed(tools, toolWsPull).annotations; a["readOnlyHint"] != false || a["destructiveHint"] != false {
		t.Fatalf("ws_pull annotations %v", a)
	}
}

// deliveryFake scripts the task forms and records every relayed call.
type deliveryFake struct {
	calls []any
}

func (d *deliveryFake) ws(ctx context.Context, method string, args ...any) (any, error) {
	d.calls = append(d.calls, append([]any{method}, args...))
	switch method {
	case "ResolveTaskResult":
		return client.TaskResultSelection{Name: "alpha", Instance: wsInst, Ref: contract.TaskRefPrefix + taskA, Commit: wsHash}, nil
	case "TaskWorkspaceStatus":
		return statusFixture(args[0].(string)), nil
	case "TaskWorkspaceDiff":
		return contract.WorkspaceDiffResponse{Name: "alpha", Instance: wsInst, Generation: wsGen, TargetCommit: wsHash, Changes: []contract.WorkspaceChange{}}, nil
	}
	return happyWs(ctx, method, args...)
}

func statusFixture(id string) contract.TaskWorkspaceStatusResponse {
	p := contract.WorkspacePhaseExecuting
	return contract.TaskWorkspaceStatusResponse{Version: 6, Workspace: contract.TaskWorkspaceStatus{TaskID: id, State: contract.TaskRunning, WorkspacePhase: &p,
		Binding: contract.WorkspaceBinding{Name: "alpha", Instance: wsInst, BaseSelector: contract.SelectorEmpty}}}
}

func TestDeliveryParserCorpus(t *testing.T) {
	h := start(t)
	happy(h.fake)
	df := &deliveryFake{}
	h.fake.ws = df.ws
	var pulls []workspacetransfer.PullRequest
	h.fake.pull = func(_ context.Context, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
		pulls = append(pulls, req)
		return pullResult(req), nil
	}
	var dispatched []contract.DispatchRequest
	h.fake.dispatch = func(_ context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
		dispatched = append(dispatched, req)
		return taskView(taskA), nil
	}
	h.ready()
	task := `"task_id":"` + taskA + `"`
	cont := `"after":"YQ==","instance":"` + wsInst + `","generation":"` + wsGen + `"`
	for _, c := range []struct {
		tool, args string
		ok         bool
	}{
		// ws_pull: both forms, path optional in both.
		{toolWsPull, `{` + task + `}`, true},
		{toolWsPull, `{` + task + `,"path":"out"}`, true},
		{toolWsPull, `{"name":"alpha","ref":"main","path":"out"}`, true},
		{toolWsPull, `{}`, false},
		{toolWsPull, `{"path":"out"}`, false},
		{toolWsPull, `{"name":"alpha"}`, false},
		{toolWsPull, `{"ref":"main"}`, false},
		{toolWsPull, `{` + task + `,"name":"alpha"}`, false},
		{toolWsPull, `{` + task + `,"ref":"main"}`, false},
		{toolWsPull, `{` + task + `,"name":"alpha","ref":"main"}`, false},
		{toolWsPull, `{"task_id":"t_x"}`, false},
		{toolWsPull, `{"task_id":"T_` + strings.Repeat("a", 32) + `"}`, false},
		{toolWsPull, `{"task_id":null}`, false},
		{toolWsPull, `{"task_id":1}`, false},
		{toolWsPull, `{"taskId":"` + taskA + `"}`, false},
		{toolWsPull, `{` + task + `,"content":"x"}`, false},
		{toolWsPull, `{` + task + `,"path":""}`, false},
		// ws_status: name form with paging; task form without.
		{toolWsStatus, `{` + task + `}`, true},
		{toolWsStatus, `{"name":"alpha","limit":3}`, true},
		{toolWsStatus, `{}`, false},
		{toolWsStatus, `{` + task + `,"name":"alpha"}`, false},
		{toolWsStatus, `{` + task + `,"limit":1}`, false},
		{toolWsStatus, `{` + task + `,` + cont + `}`, false},
		{toolWsStatus, `{"name":"alpha","base":"empty"}`, false},
		{toolWsStatus, `{"name":"alpha","target":"main"}`, false},
		{toolWsStatus, `{"name":"alpha","after":"refs/heads/main"}`, false},
		{toolWsStatus, `{"task_id":"x"}`, false},
		{toolWsStatus, `{"task_id":"` + taskA + `","file":"a"}`, false},
		// ws_diff: paging applies to both forms, all or none.
		{toolWsDiff, `{` + task + `}`, true},
		{toolWsDiff, `{` + task + `,` + cont + `,"limit":5}`, true},
		{toolWsDiff, `{` + task + `,"limit":5}`, true},
		{toolWsDiff, `{"name":"alpha","base":"empty","target":"main",` + cont + `}`, true},
		{toolWsDiff, `{}`, false},
		{toolWsDiff, `{` + task + `,"base":"empty"}`, false},
		{toolWsDiff, `{` + task + `,"target":"main"}`, false},
		{toolWsDiff, `{` + task + `,"name":"alpha"}`, false},
		{toolWsDiff, `{` + task + `,"after":"YQ=="}`, false},
		{toolWsDiff, `{` + task + `,"after":"YQ==","instance":"` + wsInst + `"}`, false},
		{toolWsDiff, `{` + task + `,"limit":0}`, false},
		{toolWsDiff, `{` + task + `,"after":"YQ",` + cont[len(`"after":"YQ==",`):] + `}`, false},
		{toolWsDiff, `{"name":"alpha","target":"main"}`, false},
		{toolWsDiff, `{"name":"alpha","base":"empty"}`, false},
		{toolWsDiff, `{"base":"empty","target":"main"}`, false},
		{toolWsDiff, `{"task_id":null}`, false},
	} {
		before := len(df.calls) + len(pulls)
		a := h.ask(c.tool, c.args)
		if a.isError == c.ok {
			t.Fatalf("%s %s: %+v", c.tool, c.args, a)
		}
		if !c.ok {
			if e := a.errorOf(t); e.Code != contract.CodeInvalidArgument {
				t.Fatalf("%s %s: %+v", c.tool, c.args, e)
			}
			if len(df.calls)+len(pulls) != before {
				t.Fatalf("%s %s reached the client", c.tool, c.args)
			}
		}
	}
	// A duplicate member is refused at framing, before any tool runs.
	n := len(df.calls) + len(pulls)
	h.send(`{"jsonrpc":"2.0","id":901,"method":"tools/call","params":{"name":"ws_pull","arguments":{` + task + `,` + task + `}}}`)
	if code := protoCode(t, h.nextRaw()); code != codeParse || len(df.calls)+len(pulls) != n {
		t.Fatalf("duplicate task_id = %d", code)
	}
	// The task forms relay the task ID; the task pull guards the transfer
	// with the resolved pair and keeps the optional path.
	if pulls[0] != (workspacetransfer.PullRequest{Name: "alpha", Ref: contract.TaskRefPrefix + taskA, ExpectedInstance: wsInst, ExpectedCommit: wsHash}) ||
		pulls[1].Path != "out" || !pulls[1].PathSet || pulls[2].ExpectedInstance != "" {
		t.Fatalf("pulls %+v", pulls)
	}
	var methods []string
	for _, c := range df.calls {
		methods = append(methods, c.([]any)[0].(string))
	}
	want := []string{"ResolveTaskResult", "ResolveTaskResult", "TaskWorkspaceStatus", "WorkspaceStatus", "TaskWorkspaceDiff", "TaskWorkspaceDiff", "TaskWorkspaceDiff", "WorkspaceDiff"}
	if !slices.Equal(methods, want) {
		t.Fatalf("relays %v", methods)
	}
	if p := df.calls[5].([]any)[2].(client.TaskDiffPage); p != (client.TaskDiffPage{After: "YQ==", Instance: wsInst, Generation: wsGen, Limit: 5}) {
		t.Fatalf("task diff page %+v", p)
	}
	// Canonical task-ID-shaped explicit selectors are normalized like the
	// CLI; refs/heads/ escapes to a branch.
	df.calls, pulls = nil, nil
	h.ask(toolWsDiff, `{"name":"alpha","base":"`+taskB+`","target":"`+taskA+`"}`)
	h.ask(toolWsPull, `{"name":"alpha","ref":"`+taskA+`"}`)
	h.ask(toolWsPull, `{"name":"alpha","ref":"refs/heads/`+taskA+`"}`)
	if p := df.calls[0].([]any)[2].(client.DiffPage); p.Base != contract.TaskRefPrefix+taskB || p.Target != contract.TaskRefPrefix+taskA {
		t.Fatalf("diff normalization %+v", p)
	}
	if pulls[0].Ref != contract.TaskRefPrefix+taskA || pulls[1].Ref != "refs/heads/"+taskA {
		t.Fatalf("pull normalization %+v", pulls)
	}
	// The exact versioned status envelope is the result text.
	want0, _ := contract.Encode(statusFixture(taskA))
	if a := h.ask(toolWsStatus, `{`+task+`}`); a.text != string(want0) {
		t.Fatalf("status text %s", a.text)
	}
	// dispatch forwards the selection unchanged; scratch stays unchanged.
	base := `"target":{"kind":"id","value":"r"},"goal":"g","acceptance":"a"`
	h.ask(toolDispatch, `{`+base+`,"workspace":"alpha","base":"`+taskB+`","workspace_instance":"`+wsInst+`"}`)
	h.ask(toolDispatch, `{`+base+`,"workspace":"alpha","base":"empty"}`)
	h.ask(toolDispatch, `{`+base+`}`)
	r := dispatched
	if len(r) != 3 || *r[0].Workspace != "alpha" || *r[0].Base != taskB || *r[0].WorkspaceInstance != wsInst || *r[1].Base != "empty" || r[1].WorkspaceInstance != nil ||
		r[2].Workspace != nil || r[2].Base != nil || r[2].WorkspaceInstance != nil {
		t.Fatalf("dispatched %+v", r)
	}
}

// Lookup errors are the safe contract error only; the bounded maximum
// workspace result relays exactly; file-content sentinels never appear.
func TestDeliveryContent(t *testing.T) {
	const sentinel = "SENTINEL-WORKSPACE-FILE-9c1"
	h := start(t)
	happy(h.fake)
	h.fake.ws = func(_ context.Context, method string, args ...any) (any, error) {
		switch method {
		case "ResolveTaskResult", "TaskWorkspaceDiff":
			return nil, &contract.Error{Code: contract.CodeConflict, Message: "task " + taskA + " has no available workspace result",
				Details: map[string]any{"reason": contract.ReasonTaskResultUnavailable}, Cause: errors.New(sentinel)}
		case "TaskWorkspaceStatus":
			return maxStatus(t), nil
		}
		return nil, errors.New(sentinel)
	}
	h.ready()
	for _, c := range []struct{ tool, args string }{{toolWsPull, `{"task_id":"` + taskA + `"}`}, {toolWsDiff, `{"task_id":"` + taskA + `"}`}} {
		a := h.ask(c.tool, c.args)
		if e := a.errorOf(t); e.Code != contract.CodeConflict || e.Details["reason"] != contract.ReasonTaskResultUnavailable || strings.Contains(a.text, sentinel) {
			t.Fatalf("%s %+v", c.tool, a)
		}
	}
	if h.fake.count("Pull") != 0 {
		t.Fatal("a refused task lookup still transferred")
	}
	a := h.ask(toolWsStatus, `{"task_id":"`+taskA+`"}`)
	want, _ := contract.Encode(maxStatus(t))
	if a.isError || a.text != string(want) || len(a.text) > contract.MaxWorkspaceDTOBytes+4<<10 {
		t.Fatalf("max status %d bytes", len(a.text))
	}
	if _, err := contract.ParseTaskWorkspaceStatusResponse([]byte(a.text)); err != nil {
		t.Fatal(err)
	}
}

// maxStatus is a published status whose result DTO holds the most change
// rows that fit 32 KiB.
func maxStatus(t testing.TB) contract.TaskWorkspaceStatusResponse {
	b := contract.WorkspaceBinding{Name: "alpha", Instance: wsInst, BaseSelector: contract.SelectorEmpty}
	c, ref := wsHash, contract.TaskRefPrefix+taskA
	w := contract.TaskWorkspaceResult{Name: "alpha", Instance: wsInst, Publication: contract.PublicationPublished, Commit: &c, Ref: &ref, Diffstat: &contract.TaskDiffstat{Added: 150}}
	mode, n := "100644", int64(contract.MaxSafeInteger)
	var rows []contract.ChangeRow
	for i := range 150 {
		rows = append(rows, contract.ChangeRow{Path: []byte(strings.Repeat("p", 180) + string(rune('A'+i/26)) + string(rune('a'+i%26))), Kind: "added", NewMode: &mode, NewBytes: &n})
	}
	if err := contract.BoundChanges(&w, rows); err != nil || !w.ChangesTruncated {
		t.Fatalf("bound %v", err)
	}
	return contract.TaskWorkspaceStatusResponse{Version: 6, Workspace: contract.TaskWorkspaceStatus{TaskID: taskA, State: contract.TaskSucceeded, Binding: b, Result: &w, Available: true}}
}

// The production transfer service resolves trust per call and relays the
// task lookup and the transfers through the same verified client: argument
// refusals come from the transfer pipeline, the lookup reaches the plane
// (a harness without task routes refuses it), and a missing CA file is
// trust_failed.
func TestPlaneTransferFactory(t *testing.T) {
	h := testkit.NewHarness(t)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, h.CAPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f := PlaneTransferFactory(h.URL, ca, "", "linux", func() []string { return []string{"HOME=/nonexistent"} })
	tr, err := f(ctx, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ResolveTaskResult(ctx, taskA); err == nil {
		t.Fatal("a plane without task routes resolved a task")
	}
	if _, err := tr.Pull(ctx, workspacetransfer.PullRequest{Name: "Bad", Ref: "main"}); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("pull %v", err)
	}
	if _, err := tr.Push(ctx, workspacetransfer.PushRequest{Name: "Bad", Instance: wsInst}); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("push %v", err)
	}
	tr.Close()
	os.Remove(ca)
	if _, err := f(ctx, "/work"); contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("removed CA file: %v", err)
	}
}

// A task pull's lookup holds the call's slot like a transfer: two calls
// saturate the session, cancellation cancels the lookup (no transfer
// starts, no task cancel), and the slot is released only after it
// returned; EOF joins it too.
func TestDeliveryLifetime(t *testing.T) {
	h := start(t)
	h.ready()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	returned := make(chan error, 2)
	h.fake.ws = func(ctx context.Context, method string, _ ...any) (any, error) {
		if method != "ResolveTaskResult" {
			return nil, errUnscripted
		}
		entered <- struct{}{}
		<-ctx.Done()
		<-release
		returned <- ctx.Err()
		return nil, ctx.Err()
	}
	h.call(1, toolWsPull, `{"task_id":"`+taskA+`"}`)
	h.call(2, toolWsPull, `{"task_id":"`+taskB+`"}`)
	for range 2 {
		select {
		case <-entered:
		case <-testCtxWait():
			t.Fatal("lookups not entered")
		}
	}
	h.call(3, toolWsPull, `{"task_id":"`+taskA+`"}`)
	if e := h.answer().errorOf(t); e.Details["reason"] != ReasonCapacity {
		t.Fatalf("third call %+v", e)
	}
	h.await(StageWritten, "n3")
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	if h.ev.peek(hookEvent{StageReleased, "n1"}) || h.ev.peek(hookEvent{StageReleased, "n2"}) {
		t.Fatal("a slot was released before its lookup returned")
	}
	close(release)
	for range 2 {
		if err := <-returned; !errors.Is(err, context.Canceled) {
			t.Fatalf("lookup context %v", err)
		}
	}
	h.await(StageReleased, "n1")
	h.await(StageReleased, "n2")
	if h.fake.count("Pull") != 0 || h.fake.count("CancelTask") != 0 {
		t.Fatalf("calls after cancellation %v", h.fake.called())
	}
	if w := h.clock.Waiters(); len(w) != 0 {
		t.Fatalf("timers armed: %v", w)
	}
	// EOF cancels and joins a running task lookup before the session ends.
	h2 := start(t)
	h2.ready()
	in := make(chan struct{})
	done := false
	h2.fake.ws = func(ctx context.Context, method string, _ ...any) (any, error) {
		close(in)
		<-ctx.Done()
		done = true
		return nil, ctx.Err()
	}
	h2.call(1, toolWsStatus, `{"task_id":"`+taskA+`"}`)
	select {
	case <-in:
	case <-testCtxWait():
		t.Fatal("status not entered")
	}
	h2.inW.Close()
	if code := h2.exit(); code != ExitOK || !done {
		t.Fatalf("exit %d, joined %v", code, done)
	}
}
