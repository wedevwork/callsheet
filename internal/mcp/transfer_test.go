package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Iteration 09b (UT-8): the local transfer tools.

func pushResult(req workspacetransfer.PushRequest) contract.WorkspacePushResult {
	branch := "refs/heads/main"
	if req.Branch != "" {
		branch, _ = contract.NormalizeBranch(req.Branch, "branch")
	}
	return contract.WorkspacePushResult{Name: req.Name, Instance: req.Instance, Branch: branch, Commit: wsHash, SourceKind: contract.KindGit, Changed: true}
}

func pullResult(req workspacetransfer.PullRequest) contract.WorkspacePullResult {
	sel, _ := contract.ParseSelector(req.Ref, "ref", false)
	return contract.WorkspacePullResult{Name: req.Name, Instance: wsInst, Selector: sel.Value, Commit: wsHash, DestinationKind: contract.KindFolder, Changed: true}
}

// happyTransfers answers every transfer with a valid result.
func happyTransfers(f *fakeClient) {
	f.push = func(_ context.Context, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error) {
		return pushResult(req), nil
	}
	f.pull = func(_ context.Context, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
		return pullResult(req), nil
	}
}

// transferSchemaCases are TestSchemas's transfer examples.
func transferSchemaCases() []struct {
	tool, args string
	schemaOK   bool
	accepted   bool
} {
	inst := `"instance":"` + wsInst + `"`
	return []struct {
		tool, args string
		schemaOK   bool
		accepted   bool
	}{
		{toolWsPush, `{"name":"alpha",` + inst + `}`, true, true},
		{toolWsPush, `{"name":"alpha",` + inst + `,"branch":"topic/x","path":"src"}`, true, true},
		{toolWsPush, `{"name":"alpha",` + inst + `,"branch":"refs/heads/main","path":"/abs/dir"}`, true, true},
		{toolWsPush, `{"name":"alpha"}`, false, false},
		{toolWsPush, `{"name":"alpha",` + inst + `,"branch":"Main"}`, false, false},
		{toolWsPush, `{"name":"alpha",` + inst + `,"branch":""}`, false, false},
		{toolWsPush, `{"name":"alpha",` + inst + `,"path":""}`, false, false},
		{toolWsPush, `{"name":"alpha",` + inst + `,"path":"a\u0000b"}`, true, false}, // NUL: contract only
		{toolWsPush, `{"name":"alpha",` + inst + `,"path":"` + strings.Repeat("a", 4096) + `"}`, false, false},
		{toolWsPush, `{"name":"alpha",` + inst + `,"branch":"x.lock"}`, true, false}, // contract only
		{toolWsPush, `{"name":"alpha",` + inst + `,"force":true}`, false, false},
		{toolWsPush, `{"name":"alpha",` + inst + `,"commit":true}`, false, false},
		{toolWsPull, `{"name":"alpha","ref":"main"}`, true, true},
		{toolWsPull, `{"name":"alpha","ref":"` + wsHash + `","path":"out"}`, true, true},
		{toolWsPull, `{"name":"alpha","ref":"refs/callsheet/tasks/t_` + strings.Repeat("a", 32) + `"}`, true, true},
		{toolWsPull, `{"name":"alpha","ref":"t_` + strings.Repeat("a", 32) + `"}`, true, true}, // iteration 10c: that task's ref
		{toolWsPull, `{"name":"alpha"}`, true, false},                                          // missing ref: a runtime form rule (flat schema)
		{toolWsPull, `{"name":"alpha","ref":"HEAD~1"}`, false, false},
		{toolWsPull, `{"name":"alpha","ref":"abc123"}`, true, true}, // a branch named abc123 (no short hashes)
		{toolWsPull, `{"name":"alpha","ref":"main","path":""}`, false, false},
		{toolWsPull, `{"name":"alpha","ref":"main","content":"x"}`, false, false},
	}
}

// UT-8: discovery lists ws_push and ws_pull last, in that order, with
// mutating non-idempotent non-destructive hints and local-file
// descriptions (never 09a's plane-only wording).
func TestTransferDiscovery(t *testing.T) {
	if n := len(ToolNames); n != 23 || ToolNames[21] != toolWsPush || ToolNames[22] != toolWsPull {
		t.Fatalf("tool order %v", ToolNames)
	}
	h := start(t)
	h.ready()
	h.send(`{"jsonrpc":"2.0","id":"l","method":"tools/list"}`)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Annotations map[string]bool `json:"annotations"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(h.nextRaw(), &list); err != nil {
		t.Fatal(err)
	}
	tools := list.Result.Tools
	if len(tools) != 23 {
		t.Fatalf("%d tools", len(tools))
	}
	for i, want := range []string{toolWsPush, toolWsPull} {
		tl := tools[21+i]
		if tl.Name != want {
			t.Fatalf("tool %d = %s", 21+i, tl.Name)
		}
		a := tl.Annotations
		if a["readOnlyHint"] || a["destructiveHint"] || a["idempotentHint"] || a["openWorldHint"] {
			t.Fatalf("%s annotations %v", want, a)
		}
		d := tl.Description
		verb := map[string]string{toolWsPush: "READS local files", toolWsPull: "WRITES local files"}[want]
		if !strings.Contains(d, verb) || !strings.Contains(d, "machine running callsheet mcp") || strings.Contains(d, "never on a local repository") ||
			!strings.Contains(d, "long time") {
			t.Fatalf("%s description %q", want, d)
		}
		if strings.Contains(string(tl.InputSchema), "content") || strings.Contains(string(tl.InputSchema), "force") {
			t.Fatalf("%s schema %s", want, tl.InputSchema)
		}
	}
	if !strings.Contains(tools[21].Description, "commit first") || !strings.Contains(tools[21].Description, "ws_ref_set") {
		t.Fatalf("push guidance %q", tools[21].Description)
	}
}

// UT-8: the tools relay one transfer each with the validated arguments,
// the server's working directory captured at the call, and a fresh
// transfer service closed after the call; results are the exact DTO.
func TestTransferTools(t *testing.T) {
	h := start(t)
	h.ready()
	var gotPush []workspacetransfer.PushRequest
	var gotPull []workspacetransfer.PullRequest
	h.fake.push = func(_ context.Context, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error) {
		gotPush = append(gotPush, req)
		return pushResult(req), nil
	}
	h.fake.pull = func(_ context.Context, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
		gotPull = append(gotPull, req)
		return pullResult(req), nil
	}
	a := h.ask(toolWsPush, `{"name":"alpha","instance":"`+wsInst+`"}`)
	want, _ := contract.Encode(pushResult(workspacetransfer.PushRequest{Name: "alpha", Instance: wsInst}))
	if a.isError || a.text != string(want) {
		t.Fatalf("push %+v", a)
	}
	a = h.ask(toolWsPush, `{"name":"alpha","instance":"`+wsInst+`","branch":"topic","path":"rel/dir"}`)
	if a.isError || gotPush[1] != (workspacetransfer.PushRequest{Name: "alpha", Instance: wsInst, Branch: "topic", Path: "rel/dir", PathSet: true}) {
		t.Fatalf("push relay %+v %v", a, gotPush)
	}
	a = h.ask(toolWsPull, `{"name":"alpha","ref":"main","path":"out"}`)
	// Iteration 10c: the explicit form relays the canonical selector.
	if a.isError || gotPull[0] != (workspacetransfer.PullRequest{Name: "alpha", Ref: "refs/heads/main", Path: "out", PathSet: true}) {
		t.Fatalf("pull relay %+v %v", a, gotPull)
	}
	var pr contract.WorkspacePullResult
	if err := json.Unmarshal([]byte(a.text), &pr); err != nil || pr.Validate() != nil || pr.LocalRef != nil {
		t.Fatalf("pull result %q %v", a.text, err)
	}
	h.fake.mu.Lock()
	cwds := append([]string(nil), h.fake.cwds...)
	h.fake.mu.Unlock()
	if len(cwds) != 3 || cwds[0] != "/work/cwd" {
		t.Fatalf("cwds %v", cwds)
	}
	// Invalid arguments are tool errors before any transfer.
	before := h.fake.count("Push") + h.fake.count("Pull")
	for _, c := range []struct{ tool, args string }{
		{toolWsPush, `{"name":"alpha","instance":"` + wsInst + `","branch":null}`},
		{toolWsPush, `{"name":"alpha","instance":"` + wsInst + `","path":"a\u0000b"}`},
		{toolWsPull, `{"name":"alpha","ref":"main","path":"\u0000"}`},
		{toolWsPull, `{"name":"alpha","ref":"main","path":1}`},
	} {
		if e := h.ask(c.tool, c.args).errorOf(t); e.Code != contract.CodeInvalidArgument {
			t.Fatalf("%s %s: %+v", c.tool, c.args, e)
		}
	}
	if n := h.fake.count("Push") + h.fake.count("Pull"); n != before {
		t.Fatalf("invalid arguments reached the transfer: %d", n-before)
	}
	// A darwin server applies the darwin path limit.
	h2 := start(t, func(h *harness) { h.cfg.GOOS = "darwin" })
	h2.ready()
	happyTransfers(h2.fake)
	if e := h2.ask(toolWsPull, `{"name":"alpha","ref":"main","path":"`+strings.Repeat("a", 1024)+`"}`).errorOf(t); e.Code != contract.CodeInvalidArgument {
		t.Fatalf("darwin path limit %+v", e)
	}
	if a := h2.ask(toolWsPull, `{"name":"alpha","ref":"main","path":"`+strings.Repeat("a", 1023)+`"}`); a.isError {
		t.Fatalf("darwin 1023 %+v", a)
	}
}

// UT-8: transfer errors are the safe contract error only: codes and
// reasons pass through, causes, library text and content sentinels never
// appear; a factory failure (trust) is a tool error and the session stays
// usable.
func TestTransferErrors(t *testing.T) {
	const sentinel = "SENTINEL-CONTENT-4b1d"
	h := start(t)
	h.ready()
	h.fake.push = func(context.Context, workspacetransfer.PushRequest) (contract.WorkspacePushResult, error) {
		return contract.WorkspacePushResult{}, &contract.Error{Code: contract.CodeConflict, Message: contract.DirtyMessage,
			Details: map[string]any{"reason": contract.ReasonDirtySource}, Cause: errors.New(sentinel)}
	}
	a := h.ask(toolWsPush, `{"name":"alpha","instance":"`+wsInst+`"}`)
	e := a.errorOf(t)
	if e.Code != contract.CodeConflict || e.Details["reason"] != contract.ReasonDirtySource || strings.Contains(a.text, sentinel) ||
		!strings.Contains(e.Message, "commit first") {
		t.Fatalf("dirty %+v %q", e, a.text)
	}
	h.fake.pull = func(context.Context, workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
		return contract.WorkspacePullResult{}, errors.New(sentinel + " raw library error")
	}
	a = h.ask(toolWsPull, `{"name":"alpha","ref":"main"}`)
	if e := a.errorOf(t); e.Code != contract.CodeInternal || strings.Contains(a.text, sentinel) {
		t.Fatalf("raw error %+v", e)
	}
	h.cfg.Transfer = nil
	h3 := start(t, func(h *harness) {
		h.cfg.Transfer = func(context.Context, string) (Transfers, error) {
			return nil, contract.New(contract.CodeTrustFailed, "connection not trusted: test")
		}
	})
	h3.ready()
	if e := h3.ask(toolWsPull, `{"name":"alpha","ref":"main"}`).errorOf(t); e.Code != contract.CodeTrustFailed {
		t.Fatalf("trust %+v", e)
	}
	if e := h3.ask(toolWsPush, `{"name":"alpha","instance":"`+wsInst+`"}`).errorOf(t); e.Code != contract.CodeTrustFailed {
		t.Fatalf("trust again %+v", e)
	}
	h4 := start(t, func(h *harness) { h.cfg.Transfer, h.cfg.Cwd = nil, nil })
	h4.ready()
	if e := h4.ask(toolWsPull, `{"name":"alpha","ref":"main"}`).errorOf(t); e.Code != contract.CodeInternal {
		t.Fatalf("unconfigured %+v", e)
	}
}

// UT-8: a transfer holds its admission slot until it returns (two calls
// saturate the session; a third is refused with mcp_capacity), a client
// cancellation cancels the transfer's context and the slot is released
// only after the transfer returned; no waiting-call timer is armed.
func TestTransferCancellationJoins(t *testing.T) {
	h := start(t)
	h.ready()
	entered := make(chan string, 2)
	release := make(chan struct{})
	returned := make(chan error, 2)
	h.fake.pull = func(ctx context.Context, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
		entered <- req.Ref
		<-ctx.Done()
		<-release
		returned <- ctx.Err()
		return contract.WorkspacePullResult{}, ctx.Err()
	}
	h.call(1, toolWsPull, `{"name":"alpha","ref":"a"}`)
	h.call(2, toolWsPull, `{"name":"alpha","ref":"b"}`)
	for range 2 {
		select {
		case <-entered:
		case <-testCtxWait():
			t.Fatal("transfers not entered")
		}
	}
	h.call(3, toolWsPull, `{"name":"alpha","ref":"c"}`)
	if e := h.answer().errorOf(t); e.Details["reason"] != ReasonCapacity {
		t.Fatalf("third call %+v", e)
	}
	// The answer is readable before its Write returns and the output
	// writer's watchdog is disarmed (code review r1 C8): wait for that.
	h.await(StageWritten, "n3")
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	if h.ev.peek(hookEvent{StageReleased, "n1"}) || h.ev.peek(hookEvent{StageReleased, "n2"}) {
		t.Fatal("a slot was released before its transfer returned")
	}
	close(release)
	for range 2 {
		if err := <-returned; !errors.Is(err, context.Canceled) {
			t.Fatalf("transfer context %v", err)
		}
	}
	h.await(StageReleased, "n1")
	h.await(StageReleased, "n2")
	if h.ev.peek(hookEvent{StageArmed, "n1"}) || h.ev.peek(hookEvent{StageArmed, "n2"}) {
		t.Fatal("a transfer armed a waiting-call budget")
	}
	if w := h.clock.Waiters(); len(w) != 0 {
		t.Fatalf("timers armed: %v", w)
	}
	// The session is usable again.
	happyTransfers(h.fake)
	if a := h.ask(toolWsPull, `{"name":"alpha","ref":"d"}`); a.isError {
		t.Fatalf("after cancellation %+v", a)
	}
}

// UT-8: EOF ends the session: a running transfer is cancelled and joined
// before the session returns.
func TestTransferEOFJoins(t *testing.T) {
	h := start(t)
	h.ready()
	entered := make(chan struct{})
	var done bool
	h.fake.push = func(ctx context.Context, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error) {
		close(entered)
		<-ctx.Done()
		done = true
		return contract.WorkspacePushResult{}, ctx.Err()
	}
	h.call(1, toolWsPush, `{"name":"alpha","instance":"`+wsInst+`"}`)
	select {
	case <-entered:
	case <-testCtxWait():
		t.Fatal("push not entered")
	}
	h.inW.Close()
	if code := h.exit(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !done {
		t.Fatal("the session ended before the transfer returned")
	}
}

func testCtxWait() <-chan struct{} {
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	go func() { <-ctx.Done(); cancel() }()
	return ctx.Done()
}
