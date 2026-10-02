package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// rawID returns a stdout line's id member as raw JSON.
func rawID(t *testing.T, line []byte) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("line %q: %v", line, err)
	}
	return string(m["id"])
}

// protoCode returns a line's JSON-RPC error code (0 for none).
func protoCode(t *testing.T, line []byte) int {
	t.Helper()
	var m struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("line %q: %v", line, err)
	}
	if m.Error == nil {
		return 0
	}
	return m.Error.Code
}

// FP-1: framing, exact IDs, duplicate keys, UTF-8, depth and envelope
// errors; a malformed line never ends the session.
func TestProtocolFraming(t *testing.T) {
	h := start(t)
	deep := strings.Repeat(`{"a":`, MaxDepth-3) + "1" + strings.Repeat("}", MaxDepth-3) // depth 64 at the innermost object
	tooDeep := strings.Repeat(`[`, MaxDepth-1) + strings.Repeat(`]`, MaxDepth-1)        // depth 65
	for _, c := range []struct {
		line string
		code int
		id   string
	}{
		{"garbage", codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping"`, codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","id":2}`, codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":{"b":1,"b":2}}}`, codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":"\ud800"}}`, codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":"\udc00\ud800"}}`, codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":"x` + "\t" + `y"}}`, codeParse, "null"},
		{"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"p\xffing\"}", codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":` + tooDeep + `}}`, codeParse, "null"},
		{`{"jsonrpc":"2.0","id":1,"method":"ping"} {}`, codeParse, "null"},
		{`{"jsonrpc":"2.0","id":01,"method":"ping"}`, codeParse, "null"},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, codeInvalidRequest, "null"},
		{`"ping"`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":1e3,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":9007199254740992,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":-9007199254740992,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":true,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":"` + strings.Repeat("x", MaxIDBytes+1) + `","method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"1.0","id":7,"method":"ping"}`, codeInvalidRequest, "7"},
		{`{"id":7,"method":"ping"}`, codeInvalidRequest, "7"},
		{`{"jsonrpc":"2.0","id":8,"method":1}`, codeInvalidRequest, "8"},
		{`{"jsonrpc":"2.0","id":9,"method":"ping","extra":1}`, codeInvalidRequest, "9"},
		{`{"jsonrpc":"2.0","id":10,"method":"ping","params":[1]}`, codeInvalidParams, "10"},
		{`{"jsonrpc":"2.0","id":11,"method":"ping","result":{}}`, codeInvalidRequest, "11"},
		{`{"jsonrpc":"2.0","id":12}`, codeInvalidRequest, "12"},
		{`{"jsonrpc":"2.0"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":13,"method":"shutdown"}`, codeMethodNotFound, "13"},
		{`{"jsonrpc":"2.0","id":14,"method":"resources/list"}`, codeMethodNotFound, "14"},
		{`{"jsonrpc":"2.0","id":15,"method":"ping","params":{"_meta":1}}`, codeInvalidParams, "15"},
	} {
		h.send(c.line)
		line := h.nextRaw()
		if got, id := protoCode(t, line), rawID(t, line); got != c.code || id != c.id {
			t.Fatalf("%.80q = %s", c.line, line)
		}
	}
	// Silence: malformed and unknown notifications and client responses
	// never get replies; the next reply is the following ping's.
	for _, n := range []string{
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`,
		`{"jsonrpc":"1.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":[1]}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","a":1,"a":2}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","extra":true}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":null}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled"}`,
		`{"jsonrpc":"2.0","id":5,"result":{}}`,
		`{"jsonrpc":"2.0","id":6,"error":{"code":1,"message":"x"}}`,
	} {
		h.send(n)
	}
	h.send(`{"jsonrpc":"2.0","id":"after","method":"ping"}`)
	if line := h.nextRaw(); string(line) != `{"jsonrpc":"2.0","id":"after","result":{}}`+"\n" {
		t.Fatalf("after silence: %s", line)
	}
	if !strings.Contains(h.stderr.String(), "ignored a response from the client") {
		t.Fatalf("stderr %q", h.stderr.String())
	}
	// Accepted IDs keep their type and value; CRLF and a maximal nesting
	// depth are accepted.
	for _, c := range []struct{ id, want string }{
		{`"abc"`, `"abc"`}, {`"1"`, `"1"`}, {`1`, `1`}, {`-9007199254740991`, `-9007199254740991`}, {`9007199254740991`, `9007199254740991`},
		{`""`, `""`}, {`"é"`, `"é"`}, {`"` + strings.Repeat("é", MaxIDBytes/2) + `"`, `"` + strings.Repeat("é", MaxIDBytes/2) + `"`}, {`0`, `0`},
	} {
		h.send(`{"jsonrpc":"2.0","id":` + c.id + `,"method":"ping","params":{"_meta":{"deep":` + deep + `}}}` + "\r")
		if line := h.nextRaw(); string(line) != `{"jsonrpc":"2.0","id":`+c.want+`,"result":{}}`+"\n" {
			t.Fatalf("id %s: %s", c.id, line)
		}
	}
}

// FP-1: the line bound counts the newline; one byte more ends the session
// with exit 2 and a diagnostic, never draining the line.
func TestProtocolLineBound(t *testing.T) {
	h := start(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	h.send(ping + strings.Repeat(" ", MaxLineBytes-1-len(ping)))
	if line := h.nextRaw(); string(line) != `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n" {
		t.Fatalf("maximal line: %s", line)
	}
	go io.WriteString(h.inW, ping+strings.Repeat(" ", MaxLineBytes-len(ping))+"\n")
	if code := h.exit(); code != ExitFraming || !strings.Contains(h.stderr.String(), "exceeds 512 KiB") {
		t.Fatalf("oversized line: %d %q", code, h.stderr.String())
	}
	// An unterminated final line is discarded: exit 2.
	h2 := start(t)
	io.WriteString(h2.inW, ping)
	h2.inW.Close()
	if code := h2.exit(); code != ExitFraming || !strings.Contains(h2.stderr.String(), "unterminated") {
		t.Fatalf("unterminated line: %d %q", code, h2.stderr.String())
	}
	// A clean EOF exits 0 without diagnostics.
	h3 := start(t)
	h3.inW.Close()
	if code := h3.exit(); code != ExitOK || h3.stderr.String() != "" {
		t.Fatalf("clean EOF: %d %q", code, h3.stderr.String())
	}
}

// FP-1: the initialization state table and version negotiation.
func TestProtocolLifecycle(t *testing.T) {
	h := start(t)
	expect := func(line string, code int) map[string]any {
		t.Helper()
		h.send(line)
		raw := h.nextRaw()
		if got := protoCode(t, raw); got != code {
			t.Fatalf("%s = %s", line, raw)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		return m
	}
	expect(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, 0)
	expect(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, codeInvalidRequest)
	expect(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"node_ls"}}`, codeInvalidRequest)
	expect(`{"jsonrpc":"2.0","id":4,"method":"initialize"}`, codeInvalidParams)
	expect(`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"a","version":"b"}}}`, codeInvalidParams)
	expect(`{"jsonrpc":"2.0","id":6,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":[],"clientInfo":{"name":"a","version":"b"}}}`, codeInvalidParams)
	expect(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"a"}}}`, codeInvalidParams)
	expect(`{"jsonrpc":"2.0","id":8,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":"a"}}`, codeInvalidParams)
	// Another revision is answered with the supported one; standard
	// metadata (title, _meta, unknown capabilities) is accepted.
	m := expect(`{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{"roots":{"listChanged":true},"sampling":{},"x-future":{}},`+
		`"clientInfo":{"name":"Claude Code","title":"Claude","version":"2.1 beta"},"_meta":{"progressToken":1}}}`, 0)
	want := `{"capabilities":{"tools":{"listChanged":false}},"protocolVersion":"2025-06-18","serverInfo":{"name":"callsheet","version":"test-1"}}`
	if b, _ := json.Marshal(m["result"]); string(b) != want {
		t.Fatalf("initialize result %s", b)
	}
	expect(`{"jsonrpc":"2.0","id":10,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"a","version":"b"}}}`, codeInvalidRequest)
	// Awaiting notifications/initialized: only ping.
	expect(`{"jsonrpc":"2.0","id":11,"method":"tools/list"}`, codeInvalidRequest)
	expect(`{"jsonrpc":"2.0","id":12,"method":"ping","params":{}}`, 0)
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	expect(`{"jsonrpc":"2.0","id":13,"method":"tools/list"}`, 0)
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	expect(`{"jsonrpc":"2.0","id":14,"method":"tools/list","params":{"_meta":{}}}`, 0)
	expect(`{"jsonrpc":"2.0","id":15,"method":"shutdown"}`, codeMethodNotFound)
	// The retained attribution is the verbatim clientInfo name and
	// version, whatever their grammar.
	rb, err := (&session{cfg: Config{Hostname: func() (string, error) { return "h", nil }}, clientName: "a b", clientVersion: "1"}).requester()
	if err == nil || rb.Name != "a b" {
		t.Fatalf("requester %+v %v", rb, err)
	}
	// A matching revision is echoed.
	h2 := start(t)
	h2.send(initLine)
	if r := h2.next(); r["result"].(map[string]any)["protocolVersion"] != ProtocolVersion {
		t.Fatalf("echo %v", r)
	}
}

// FP-1: discovery lists the twenty-three tools in order with materialized
// local schemas and truthful annotations; there is one page.
func TestProtocolDiscovery(t *testing.T) {
	for _, b := range []time.Duration{DefaultBudget, time.Second, 90 * time.Second, 5 * time.Minute} {
		h := start(t, withBudget(b))
		h.ready()
		h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":""}}`)
		line := h.nextRaw()
		if bytes.Contains(line, []byte("$ref")) || bytes.Contains(line, []byte("nextCursor")) || bytes.Contains(line, []byte("outputSchema")) {
			t.Fatalf("tools/list holds a reference, cursor or output schema")
		}
		var r struct {
			Result struct {
				Tools []struct {
					Name        string                     `json:"name"`
					Description string                     `json:"description"`
					InputSchema map[string]json.RawMessage `json:"inputSchema"`
					Annotations map[string]bool            `json:"annotations"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range r.Result.Tools {
			names = append(names, tl.Name)
			if string(tl.InputSchema["type"]) != `"object"` || string(tl.InputSchema["additionalProperties"]) != "false" {
				t.Fatalf("%s schema %v", tl.Name, tl.InputSchema)
			}
			if !strings.Contains(tl.Description, "reflect the plane at the time of this call") || !strings.Contains(tl.Description, "untrusted worker text") {
				t.Fatalf("%s description %q", tl.Name, tl.Description)
			}
			waiting := tl.Name == toolTaskWait || tl.Name == toolDispatch
			if waiting != strings.Contains(tl.Description, "B="+NewBudget(b).String()+". "+BudgetDeferralNote+" "+UnverifiedNotice) ||
				waiting != strings.Contains(tl.Description, UnverifiedNotice) || strings.Contains(tl.Description, "ends within it") {
				t.Fatalf("%s budget description %q", tl.Name, tl.Description)
			}
			mutating := map[string]bool{toolRoleAdd: true, toolRoleSet: true, toolRoleRm: true, toolDispatch: true, toolTaskCancel: true,
				toolWsCreate: true, toolWsRm: true, toolWsPrune: true, toolWsRefSet: true, toolWsPush: true, toolWsPull: true}[tl.Name]
			if tl.Annotations["readOnlyHint"] == mutating || tl.Annotations["openWorldHint"] {
				t.Fatalf("%s annotations %v", tl.Name, tl.Annotations)
			}
			if mutating && tl.Annotations["idempotentHint"] != (tl.Name == toolTaskCancel) {
				t.Fatalf("%s idempotence %v", tl.Name, tl.Annotations)
			}
		}
		if strings.Join(names, ",") != strings.Join(ToolNames, ",") || len(names) != 23 {
			t.Fatalf("tools %v", names)
		}
		for i, bad := range []string{`{"cursor":"page2"}`, `{"cursor":1}`, `{"limit":1}`, `{"_meta":[]}`} {
			h.send(`{"jsonrpc":"2.0","id":` + itoa(2+i) + `,"method":"tools/list","params":` + bad + `}`)
			if code := protoCode(t, h.nextRaw()); code != codeInvalidParams {
				t.Fatalf("tools/list %s = %d", bad, code)
			}
		}
	}
}

// FP-1: tools/call envelope errors are protocol errors; bad arguments of
// a known tool are tool errors the coordinator can correct.
func TestProtocolCallEnvelope(t *testing.T) {
	h := start(t)
	h.ready()
	for i, p := range []string{``, `,"params":{}`, `,"params":{"name":1}`, `,"params":{"name":"nope"}`, `,"params":{"name":"node_ls","arguments":[]}`,
		`,"params":{"name":"node_ls","extra":1}`, `,"params":{"name":"node_ls","_meta":1}`} {
		h.send(`{"jsonrpc":"2.0","id":` + itoa(i+1) + `,"method":"tools/call"` + p + `}`)
		if code := protoCode(t, h.nextRaw()); code != codeInvalidParams {
			t.Fatalf("tools/call %q = %d", p, code)
		}
	}
	h.send(`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"node_show","arguments":{"id":"nope"},"_meta":{"progressToken":"p"}}}`)
	if e := h.answer().errorOf(t); e.Code != contract.CodeInvalidArgument {
		t.Fatalf("bad argument %+v", e)
	}
	if h.factory.Load() != 0 {
		t.Fatalf("invalid calls resolved trust %d times", h.factory.Load())
	}
}

// A client that waits for each answer before sending the next call must
// not be refused for capacity. The slot is freed as the answer is
// written, including for calls that never reach the plane.
func TestSequentialCallsReuseTheSlot(t *testing.T) {
	h := start(t)
	h.ready()
	for i := range 50 {
		e := h.ask(toolRoleAdd, `{}`).errorOf(t)
		if e.Code != contract.CodeInvalidArgument || e.Details["reason"] == ReasonCapacity {
			t.Fatalf("call %d: %+v", i, e)
		}
	}
	if h.factory.Load() != 0 {
		t.Fatalf("invalid calls resolved trust %d times", h.factory.Load())
	}
}

// FP-1: two calls at most; a third is refused at once without contacting
// the plane; lifecycle and ping stay responsive; released slots are reused.
func TestProtocolSaturation(t *testing.T) {
	h := start(t, withHook(nil))
	g := newGate()
	h.fake.listNodes = func(ctx context.Context) ([]contract.Node, error) {
		if err := g.wait(ctx); err != nil {
			return nil, err
		}
		return []contract.Node{}, nil
	}
	h.ready()
	h.call(1, toolNodeLs, "")
	h.call(2, toolNodeLs, "")
	g.awaitEntered(t)
	g.awaitEntered(t)
	h.call(3, toolNodeLs, "{}")
	a := h.answer()
	if e := a.errorOf(t); a.id != json.Number("3") || e.Code != contract.CodeUnavailable || e.Details["reason"] != ReasonCapacity {
		t.Fatalf("third call %+v %+v", a, e)
	}
	if h.factory.Load() != 2 {
		t.Fatalf("factory calls %d", h.factory.Load())
	}
	h.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if rawID(t, h.nextRaw()) != `"p"` {
		t.Fatal("ping not answered during saturation")
	}
	close(g.release)
	seen := map[any]bool{}
	for range 2 {
		a := h.answer()
		if a.isError || a.text != `{"version":5,"nodes":[]}` {
			t.Fatalf("answer %+v", a)
		}
		seen[a.id] = true
	}
	if !seen[json.Number("1")] || !seen[json.Number("2")] {
		t.Fatalf("answers %v", seen)
	}
	h.await(StageReleased, "n1")
	h.await(StageReleased, "n2")
	h.call(4, toolNodeLs, "")
	if a := h.answer(); a.isError {
		t.Fatalf("reused slot %+v", a)
	}
}

// FP-1: one serialized writer: concurrent large answers and control
// replies never interleave; every line is one complete frame.
func TestProtocolWriterSerialization(t *testing.T) {
	h := start(t, withRealClock())
	data := bytes.Repeat([]byte("\"\\\x00\xff\n"), 8<<10) // 40 KiB raw, far more escaped: many chunks per answer
	h.fake.taskLogs = func(ctx context.Context, id string) (contract.TaskLogsResponse, error) {
		return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id, Data: data, RetainedBytes: len(data), SourceBytes: len(data)}, nil
	}
	h.ready()
	id := "t_" + strings.Repeat("a", 32)
	// Rounds of two large answers and four pings (within the bounded
	// control queue) race for the single writer.
	for round := range 3 {
		for i := range 2 {
			h.call(round*2+i+1, toolTaskLogs, `{"id":"`+id+`"}`)
			h.send(`{"jsonrpc":"2.0","id":"p` + itoa(round*4+2*i) + `","method":"ping"}`)
			h.send(`{"jsonrpc":"2.0","id":"p` + itoa(round*4+2*i+1) + `","method":"ping"}`)
		}
		logs, pings := 0, 0
		for logs+pings < 6 {
			line := h.nextRaw()
			if !json.Valid(line) {
				t.Fatalf("interleaved frame of %d bytes", len(line))
			}
			if bytes.Contains(line, []byte(`"result":{}`)) {
				pings++
				continue
			}
			var m struct {
				Result struct {
					Content []struct{ Text string } `json:"content"`
					IsError bool                    `json:"isError"`
				} `json:"result"`
			}
			json.Unmarshal(line, &m)
			r, err := contract.ParseTaskLogsResponse([]byte(m.Result.Content[0].Text))
			if m.Result.IsError || err != nil || !bytes.Equal(r.Data, data) {
				t.Fatalf("logs %v %s", err, line[:min(len(line), 200)])
			}
			logs++
		}
		if logs != 2 || pings != 4 {
			t.Fatalf("round %d: %d logs %d pings", round, logs, pings)
		}
		h.await(StageReleased, "n"+itoa(round*2+1))
		h.await(StageReleased, "n"+itoa(round*2+2))
	}
}

// FP-1/FP-8: cancellation cancels only that call's context, suppresses its
// answer, frees its slot and ID, and never cancels the task.
func TestProtocolCancellation(t *testing.T) {
	h := start(t, withHook(nil))
	g := newGate()
	var mu sync.Mutex
	var ctxErrs []error
	h.fake.showTask = func(ctx context.Context, id string, _ int) (contract.TaskView, error) {
		err := g.wait(ctx)
		mu.Lock()
		ctxErrs = append(ctxErrs, context.Cause(ctx))
		mu.Unlock()
		return contract.TaskView{}, err
	}
	h.ready()
	args := `{"id":"t_` + strings.Repeat("b", 32) + `"}`
	h.call(1, toolTaskShow, args)
	g.awaitEntered(t)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99,"reason":"unknown"}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"1"}}`)
	// A duplicate of the active ID is refused and does not replace the
	// original cancellation handle.
	h.call(1, toolTaskShow, args)
	if line := h.nextRaw(); protoCode(t, line) != codeInvalidRequest || rawID(t, line) != "1" {
		t.Fatalf("duplicate id %s", line)
	}
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"user"}}`)
	h.await(StageReleased, "n1")
	mu.Lock()
	if len(ctxErrs) != 1 || !errors.Is(ctxErrs[0], errCancelled) {
		t.Fatalf("context causes %v", ctxErrs)
	}
	mu.Unlock()
	// The ID is free again; an answered call can no longer be cancelled.
	h.fake.showTask = func(ctx context.Context, id string, _ int) (contract.TaskView, error) {
		return contract.TaskView{TaskID: id}, nil
	}
	h.call(1, toolTaskShow, args)
	if a := h.answer(); a.isError || a.id != json.Number("1") {
		t.Fatalf("reused id %+v", a)
	}
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	h.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if line := h.nextRaw(); rawID(t, line) != `"p"` {
		t.Fatalf("after cancel: %s", line)
	}
	if n := h.fake.count("CancelTask"); n != 0 {
		t.Fatalf("CancelTask called %d times", n)
	}
	h.inW.Close()
	if code := h.exit(); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	for line := range h.lines {
		t.Fatalf("unexpected answer after cancellation: %s", line)
	}
}

// ctlWriter is a pipe-like stdout with a fixed capacity that the test
// drains explicitly; writes block while it is full and may be short.
// Waiting is event-driven (its condition variable), never polled, and
// newlines are counted as bytes arrive, never re-scanned.
type ctlWriter struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	cap     int
	closed  bool
	got     bytes.Buffer
	scanned int // bytes of got already searched for a newline
	lines   int // newlines written
	taken   int // lines returned by line
	writes  int
	calls   int  // Write calls entered
	waiting bool // a Write waits for room
}

func newCtlWriter(capacity int) *ctlWriter {
	w := &ctlWriter{cap: capacity}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *ctlWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	for len(w.buf) >= w.cap && !w.closed {
		w.waiting = true
		w.cond.Broadcast()
		w.cond.Wait()
	}
	w.waiting = false
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n := min(len(p), w.cap-len(w.buf))
	w.buf = append(w.buf, p[:n]...)
	w.lines += bytes.Count(p[:n], []byte{'\n'})
	w.writes++
	w.cond.Broadcast()
	return n, nil
}

func (w *ctlWriter) Close() error {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
	return nil
}

// drain moves up to n buffered bytes to got.
func (w *ctlWriter) drain(n int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.drainLocked(n)
}

func (w *ctlWriter) drainLocked(n int) int {
	n = min(n, len(w.buf))
	w.got.Write(w.buf[:n])
	w.buf = w.buf[n:]
	w.cond.Broadcast()
	return n
}

// waitLocked blocks (mu held) until cond holds; the test fails after
// testWait (a failure bound: every change of state broadcasts).
func (w *ctlWriter) waitLocked(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if cond() {
		return
	}
	expired := false
	tm := time.AfterFunc(testWait, func() {
		w.mu.Lock()
		expired = true
		w.cond.Broadcast()
		w.mu.Unlock()
	})
	defer tm.Stop()
	for !cond() {
		if expired {
			t.Fatal(what)
		}
		w.cond.Wait()
	}
}

func (w *ctlWriter) fullLocked() bool { return len(w.buf) >= w.cap && w.waiting }

// awaitFull waits until the pipe is full and a Write waits for room (the
// writer's timer re-arm after its last progress was applied before it).
func (w *ctlWriter) awaitFull(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waitLocked(t, "the pipe never filled", w.fullLocked)
}

// awaitFullOrLine waits until the pipe is full (the writer blocks; false)
// or a complete line not yet returned by line was written (true).
func (w *ctlWriter) awaitFullOrLine(t *testing.T) bool {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waitLocked(t, "the pipe neither filled nor received a line", func() bool { return w.lines > w.taken || w.fullLocked() })
	return w.lines > w.taken
}

// line drains until one complete line was received and returns it.
func (w *ctlWriter) line(t *testing.T) []byte {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if i := bytes.IndexByte(w.got.Bytes()[w.scanned:], '\n'); i >= 0 {
			l := append([]byte(nil), w.got.Next(w.scanned+i+1)...)
			w.scanned = 0
			w.taken++
			return l
		}
		w.scanned = w.got.Len()
		w.waitLocked(t, "no line", func() bool { return len(w.buf) > 0 || w.closed })
		if w.drainLocked(len(w.buf)) == 0 {
			t.Fatal("no line: the output was closed")
		}
	}
}

// armedAt is an AwaitWaiter condition: a timer due exactly at at.
func armedAt(at time.Time, d time.Duration) func([]testkit.Waiter) bool {
	return func(ws []testkit.Waiter) bool {
		for _, w := range ws {
			if !w.Ticker && w.At.Equal(at) && w.Duration == d {
				return true
			}
		}
		return false
	}
}

func readyOn(t *testing.T, h *harness, w *ctlWriter) {
	t.Helper()
	h.send(initLine)
	if l := w.line(t); !bytes.Contains(l, []byte(`"id":"init"`)) {
		t.Fatalf("initialize %s", l)
	}
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// FP-8: the writer's timer measures lack of progress, not frame duration:
// a 64 KiB pipe drained 4 KiB at a time, 900 ms apart, delivers a frame
// taking far longer than 1 s; a reader that stops is stalled after 1 s
// without progress (exit 5, the blocked write interrupted and joined).
func TestWriterProgressTimer(t *testing.T) {
	w := newCtlWriter(64 << 10)
	h := start(t, withOutput(w, w.Close))
	data := bytes.Repeat([]byte{0xfe, '"'}, 48<<10)
	h.fake.taskLogs = func(ctx context.Context, id string) (contract.TaskLogsResponse, error) {
		return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id, Data: data, RetainedBytes: len(data), SourceBytes: len(data)}, nil
	}
	readyOn(t, h, w)
	args := `{"id":"t_` + strings.Repeat("c", 32) + `"}`
	h.call(1, toolTaskLogs, args)
	for !w.awaitFullOrLine(t) {
		// The last positive write armed the timer one second from now.
		if err := h.clock.AwaitWaiter(testWait, armedAt(h.clock.Now().Add(WriteIdle), WriteIdle)); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(900 * time.Millisecond)
		w.drain(4 << 10)
	}
	line := w.line(t)
	var m struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("frame: %v", err)
	}
	if r, err := contract.ParseTaskLogsResponse([]byte(m.Result.Content[0].Text)); err != nil || !bytes.Equal(r.Data, data) {
		t.Fatalf("logs %v", err)
	}
	if h.clock.Now().Sub(epoch) < 10*WriteIdle {
		t.Fatalf("the paced frame took only %v", h.clock.Now().Sub(epoch))
	}
	select {
	case c := <-h.code:
		t.Fatalf("session ended %d during paced progress", c)
	default:
	}
	// A reader that stops: one second without progress ends the session.
	h.call(2, toolTaskLogs, args)
	w.awaitFull(t)
	if err := h.clock.AwaitWaiter(testWait, testkit.HasTimer(WriteIdle)); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(WriteIdle - time.Nanosecond)
	select {
	case c := <-h.code:
		t.Fatalf("stalled before 1 s: %d", c)
	default:
	}
	h.clock.Advance(time.Nanosecond)
	if code := h.exit(); code != ExitOutput || !strings.Contains(h.stderr.String(), "made no progress for 1s") {
		t.Fatalf("stall %d %q", code, h.stderr.String())
	}
}

// FP-8: a zero-byte write never resets the progress timer.
func TestWriterZeroWrites(t *testing.T) {
	zw := &zeroWriter{}
	h := start(t, withOutput(zw, zw.Close))
	h.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if err := h.clock.AwaitWaiter(testWait, testkit.HasTimer(WriteIdle)); err != nil {
		t.Fatal(err)
	}
	for zw.count() < 3 {
		time.Sleep(time.Millisecond)
	}
	h.clock.Advance(WriteIdle)
	if code := h.exit(); code != ExitOutput {
		t.Fatalf("zero writes: %d", code)
	}
}

type zeroWriter struct {
	mu     sync.Mutex
	n      int
	closed bool
}

func (z *zeroWriter) Write(p []byte) (int, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.closed {
		return 0, io.ErrClosedPipe
	}
	z.n++
	return 0, nil
}

func (z *zeroWriter) count() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.n
}

func (z *zeroWriter) Close() error {
	z.mu.Lock()
	z.closed = true
	z.mu.Unlock()
	return nil
}

// FP-8: a broken stdout (EPIPE) ends the session with exit 5 and a
// diagnostic.
func TestWriterBrokenPipe(t *testing.T) {
	h := start(t, withOutput(epipeWriter{}, func() error { return nil }))
	h.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if code := h.exit(); code != ExitOutput || !strings.Contains(h.stderr.String(), "cannot write to stdout") {
		t.Fatalf("EPIPE %d %q", code, h.stderr.String())
	}
}

type epipeWriter struct{}

func (epipeWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }

// FP-8: replies beyond the bounded queue close the session instead of
// blocking the reader.
func TestWriterQueueBound(t *testing.T) {
	w := newCtlWriter(1)
	h := start(t, withOutput(w, w.Close))
	for i := range maxQueuedControl + 2 {
		io.WriteString(h.inW, `{"jsonrpc":"2.0","id":`+itoa(i)+`,"method":"ping"}`+"\n")
	}
	if code := h.exit(); code != ExitOutput || !strings.Contains(h.stderr.String(), "not reading its replies") {
		t.Fatalf("overload %d %q", code, h.stderr.String())
	}
}

// FP-8: EOF, the process's signal context and blocked streams: every
// path cancels the active call, answers nothing more, never cancels a
// task and joins the reader and writer.
func TestSessionEnd(t *testing.T) {
	for _, c := range []struct {
		name string
		stop func(h *harness)
		code int
	}{
		{"eof", func(h *harness) { h.inW.Close() }, ExitOK},
		{"signal", func(h *harness) { h.cancel() }, ExitInterrupted},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t)
			g := newGate()
			causes := make(chan error, 1)
			h.fake.waitTasks = func(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
				err := g.wait(ctx)
				causes <- ctx.Err()
				return contract.WaitResponse{}, err
			}
			h.ready()
			h.call(1, toolTaskWait, `{"task_ids":["t_`+strings.Repeat("d", 32)+`"]}`)
			g.awaitEntered(t)
			c.stop(h)
			if code := h.exit(); code != c.code {
				t.Fatalf("exit %d", code)
			}
			if err := <-causes; err == nil {
				t.Fatal("the active call's context was not cancelled")
			}
			if h.fake.count("CancelTask") != 0 || h.fake.count("RemoveRole") != 0 {
				t.Fatalf("session end touched the plane: %v", h.fake.called())
			}
		})
	}
	t.Run("blocked-writer", func(t *testing.T) {
		w := newCtlWriter(8)
		h := start(t, withOutput(w, w.Close))
		h.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		w.awaitFull(t)
		h.cancel()
		if code := h.exit(); code != ExitInterrupted {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("unclosable", func(t *testing.T) {
		// Streams without closers are never waited for.
		inR, inW := io.Pipe()
		defer inW.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() {
			done <- Serve(ctx, Config{Factory: func(context.Context) (Client, error) { return nil, errUnscripted }, Version: "v",
				Hostname: func() (string, error) { return "h", nil }, In: inR, Out: io.Discard})
		}()
		cancel()
		select {
		case code := <-done:
			if code != ExitInterrupted {
				t.Fatalf("exit %d", code)
			}
		case <-time.After(testWait):
			t.Fatal("Serve waited for an unclosable reader")
		}
	})
	t.Run("read-error", func(t *testing.T) {
		code := Serve(context.Background(), Config{Factory: func(context.Context) (Client, error) { return nil, errUnscripted }, Version: "v",
			Hostname: func() (string, error) { return "h", nil }, In: errReader{}, Out: io.Discard, CloseIn: func() error { return nil }})
		if code != ExitOutput {
			t.Fatalf("exit %d", code)
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("EIO") }

// FP-1: the strict scanner and the string escaper.
func TestJSONHelpers(t *testing.T) {
	for _, ok := range []string{`{}`, `[]`, `{"a":[1,-2.5e+3,true,false,null,"é😀\n\/"]}`, ` 0 `, `"x"`} {
		if err := checkJSON([]byte(ok)); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{``, `{`, `{"a"}`, `{"a":1,}`, `[1,]`, `tru`, `-`, `1.`, `1e`, `"\x"`, `"\u12"`, `{1:2}`, `[1 2]`, `{"a":1 "b":2}`, `"abc`, `"\ud800A"`} {
		if err := checkJSON([]byte(bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	text := []byte("a\"b\\c\n\r\t\x01\x7f\u0085  é😀")
	esc := appendEscaped(nil, text)
	if len(esc) != escapedLen(text) {
		t.Fatalf("escapedLen %d != %d", escapedLen(text), len(esc))
	}
	var back string
	if err := json.Unmarshal(append(append([]byte(`"`), esc...), '"'), &back); err != nil || back != string(text) {
		t.Fatalf("round trip %q %v", back, err)
	}
	if bytes.ContainsAny(esc, "\x01\x7f\n") || bytes.Contains(esc, []byte(" ")) || bytes.Contains(esc, []byte("\u0085")) {
		t.Fatalf("raw controls in %q", esc)
	}
	if containsNull([]byte(`{"a":"null","b":"\"null"}`)) || !containsNull([]byte(`{"a":[1,null]}`)) {
		t.Fatal("containsNull")
	}
	if describeWriteError(io.ErrClosedPipe) != "the pipe is closed" || len(describeWriteError(errors.New(strings.Repeat("x", 300)))) > 120 {
		t.Fatal("describeWriteError")
	}
	if formatDuration(90*time.Second) != "90s" || formatDuration(1500*time.Millisecond) != "1.5s" || formatDuration(2*time.Minute) != "2m" {
		t.Fatal("formatDuration")
	}
}

// FP-1: a cancelled call's answer that is queued behind another frame is
// discarded before its write starts; an answer whose write started is
// never recalled.
func TestCancelDiscardsQueuedAnswer(t *testing.T) {
	w := newCtlWriter(16)
	h := start(t, withOutput(w, w.Close))
	h.fake.showTask = func(_ context.Context, id string, _ int) (contract.TaskView, error) {
		return contract.TaskView{TaskID: id}, nil
	}
	readyOn(t, h, w)
	h.send(`{"jsonrpc":"2.0","id":"p1","method":"ping"}`)
	w.awaitFull(t) // the writer is blocked inside the ping's frame
	h.call(1, toolTaskShow, `{"id":"`+taskA+`"}`)
	queued := func() int {
		h.s.wmu.Lock()
		defer h.s.wmu.Unlock()
		return len(h.s.queue)
	}
	deadline := time.Now().Add(testWait)
	for queued() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the answer was never queued")
		}
		time.Sleep(time.Millisecond)
	}
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	h.await(StageReleased, "n1")
	if queued() != 0 {
		t.Fatal("the cancelled answer is still queued")
	}
	if l := w.line(t); !bytes.Contains(l, []byte(`"id":"p1"`)) {
		t.Fatalf("first line %s", l)
	}
	h.send(`{"jsonrpc":"2.0","id":"p2","method":"ping"}`)
	if l := w.line(t); !bytes.Contains(l, []byte(`"id":"p2"`)) {
		t.Fatalf("the cancelled answer was written: %s", l)
	}
	// Once its write started, an answer is delivered whole.
	h.call(2, toolTaskShow, `{"id":"`+taskA+`"}`)
	w.awaitFull(t)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	h.send(`{"jsonrpc":"2.0","id":"p3","method":"ping"}`)
	if l := w.line(t); !bytes.Contains(l, []byte(`"id":2`)) || !json.Valid(l) {
		t.Fatalf("started answer %s", l)
	}
	if l := w.line(t); !bytes.Contains(l, []byte(`"id":"p3"`)) {
		t.Fatalf("after %s", l)
	}
}

// FP-8: every goroutine of a session is joined before Serve returns, with
// a blocked writer, a blocked reader, an active call and its budget
// timers when the signal context ends it.
func TestServeJoinsGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	inR, inW := io.Pipe()
	w := newCtlWriter(8)
	clock := testkit.NewFakeClock(epoch)
	g := newGate()
	fake := &fakeClient{}
	fake.waitTasks = func(ctx context.Context, _ []string, _ time.Duration) (contract.WaitResponse, error) {
		return contract.WaitResponse{}, g.wait(ctx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Serve(ctx, Config{Factory: func(context.Context) (Client, error) { return fake, nil }, Budget: DefaultBudget, Version: "v",
			Hostname: func() (string, error) { return "h", nil }, Clock: clock, In: inR, Out: w, CloseIn: inR.Close, CloseOut: w.Close})
	}()
	io.WriteString(inW, initLine+"\n")
	w.awaitFull(t) // the writer is blocked inside the initialize answer
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	w.line(t) // the initialize answer, emitted whole: the session becomes ready
	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task_wait","arguments":{"task_ids":["`+taskA+`"]}}}`+"\n")
	g.awaitEntered(t)
	cancel()
	select {
	case code := <-done:
		if code != ExitInterrupted {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(testWait):
		t.Fatal("Serve did not return")
	}
	inW.Close()
	deadline := time.Now().Add(testWait)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines remain after Serve returned (%d before)", runtime.NumGoroutine(), before)
		}
		time.Sleep(time.Millisecond)
	}
}
