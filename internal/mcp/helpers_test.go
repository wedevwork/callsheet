package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// testWait bounds every wait of a test on an event (a failure bound, never
// a scheduling assumption).
const testWait = 20 * time.Second

var epoch = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// syncBuffer is a concurrency-safe stderr sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeClient is a scriptable plane client. Every method records its name;
// an unset behaviour answers a fixed internal error.
type fakeClient struct {
	mu     sync.Mutex
	calls  []string
	closes int

	listNodes    func(context.Context) ([]contract.Node, error)
	showNode     func(context.Context, string) (contract.Node, error)
	addRole      func(context.Context, contract.RoleConfig) (contract.RoleView, error)
	setRole      func(context.Context, string, contract.RolePatch) (contract.RoleView, error)
	listRoles    func(context.Context) ([]contract.RoleView, error)
	showRole     func(context.Context, string) (contract.RoleView, error)
	removeRole   func(context.Context, string, bool, string) (contract.RoleRemoveResult, error)
	dispatch     func(context.Context, contract.DispatchRequest) (contract.TaskView, error)
	dispatchWait func(context.Context, contract.DispatchRequest, time.Duration) (contract.DispatchResponse, error)
	listTasks    func(context.Context, string, int) ([]contract.TaskSummary, *string, error)
	showTask     func(context.Context, string, int) (contract.TaskView, error)
	taskLogs     func(context.Context, string) (contract.TaskLogsResponse, error)
	taskLateLogs func(context.Context, string) (contract.TaskLogsResponse, error)
	cancelTask   func(context.Context, string) (contract.CancelResponse, error)
	waitTasks    func(context.Context, []string, time.Duration) (contract.WaitResponse, error)
}

var errUnscripted = errors.New("fake: unscripted operation")

func (f *fakeClient) record(m string) {
	f.mu.Lock()
	f.calls = append(f.calls, m)
	f.mu.Unlock()
}

func (f *fakeClient) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeClient) count(m string) int {
	n := 0
	for _, c := range f.called() {
		if c == m {
			n++
		}
	}
	return n
}

func (f *fakeClient) Close() {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
}

func (f *fakeClient) ListNodes(ctx context.Context) ([]contract.Node, error) {
	f.record("ListNodes")
	if f.listNodes == nil {
		return nil, errUnscripted
	}
	return f.listNodes(ctx)
}

func (f *fakeClient) ShowNode(ctx context.Context, id string) (contract.Node, error) {
	f.record("ShowNode")
	if f.showNode == nil {
		return contract.Node{}, errUnscripted
	}
	return f.showNode(ctx, id)
}

func (f *fakeClient) AddRole(ctx context.Context, rc contract.RoleConfig) (contract.RoleView, error) {
	f.record("AddRole")
	if f.addRole == nil {
		return contract.RoleView{}, errUnscripted
	}
	return f.addRole(ctx, rc)
}

func (f *fakeClient) SetRole(ctx context.Context, id string, p contract.RolePatch) (contract.RoleView, error) {
	f.record("SetRole")
	if f.setRole == nil {
		return contract.RoleView{}, errUnscripted
	}
	return f.setRole(ctx, id, p)
}

func (f *fakeClient) ListRoles(ctx context.Context) ([]contract.RoleView, error) {
	f.record("ListRoles")
	if f.listRoles == nil {
		return nil, errUnscripted
	}
	return f.listRoles(ctx)
}

func (f *fakeClient) ShowRole(ctx context.Context, id string) (contract.RoleView, error) {
	f.record("ShowRole")
	if f.showRole == nil {
		return contract.RoleView{}, errUnscripted
	}
	return f.showRole(ctx, id)
}

func (f *fakeClient) RemoveRole(ctx context.Context, id string, force bool, op string) (contract.RoleRemoveResult, error) {
	f.record("RemoveRole")
	if f.removeRole == nil {
		return contract.RoleRemoveResult{}, errUnscripted
	}
	return f.removeRole(ctx, id, force, op)
}

func (f *fakeClient) Dispatch(ctx context.Context, req contract.DispatchRequest) (contract.TaskView, error) {
	f.record("Dispatch")
	if f.dispatch == nil {
		return contract.TaskView{}, errUnscripted
	}
	return f.dispatch(ctx, req)
}

func (f *fakeClient) DispatchWithWait(ctx context.Context, req contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error) {
	f.record("DispatchWithWait")
	if f.dispatchWait == nil {
		return contract.DispatchResponse{}, errUnscripted
	}
	return f.dispatchWait(ctx, req, wait)
}

func (f *fakeClient) ListTasks(ctx context.Context, after string, limit int) ([]contract.TaskSummary, *string, error) {
	f.record("ListTasks")
	if f.listTasks == nil {
		return nil, nil, errUnscripted
	}
	return f.listTasks(ctx, after, limit)
}

func (f *fakeClient) ShowTask(ctx context.Context, id string, lines int) (contract.TaskView, error) {
	f.record("ShowTask")
	if f.showTask == nil {
		return contract.TaskView{}, errUnscripted
	}
	return f.showTask(ctx, id, lines)
}

func (f *fakeClient) TaskLogs(ctx context.Context, id string) (contract.TaskLogsResponse, error) {
	f.record("TaskLogs")
	if f.taskLogs == nil {
		return contract.TaskLogsResponse{}, errUnscripted
	}
	return f.taskLogs(ctx, id)
}

func (f *fakeClient) TaskLateLogs(ctx context.Context, id string) (contract.TaskLogsResponse, error) {
	f.record("TaskLateLogs")
	if f.taskLateLogs == nil {
		return contract.TaskLogsResponse{}, errUnscripted
	}
	return f.taskLateLogs(ctx, id)
}

func (f *fakeClient) CancelTask(ctx context.Context, id string) (contract.CancelResponse, error) {
	f.record("CancelTask")
	if f.cancelTask == nil {
		return contract.CancelResponse{}, errUnscripted
	}
	return f.cancelTask(ctx, id)
}

func (f *fakeClient) WaitTasks(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
	f.record("WaitTasks")
	if f.waitTasks == nil {
		return contract.WaitResponse{}, errUnscripted
	}
	return f.waitTasks(ctx, ids, wait)
}

// gate holds a scripted operation until released or its context ends,
// reporting entry on entered.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan struct{}, 16), release: make(chan struct{})} }

// wait is the held operation's body: nil when released, else ctx's error.
func (g *gate) wait(ctx context.Context) error {
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(testWait):
		t.Fatal("the held operation was never entered")
	}
}

// harness runs one session over in-memory pipes with a fake clock and a
// fake client.
type harness struct {
	t       testing.TB
	clock   *testkit.FakeClock
	fake    *fakeClient
	inW     *io.PipeWriter
	lines   chan []byte
	stderr  *syncBuffer
	code    chan int
	factory atomic.Int32
	ev      *eventSet
	cfg     Config
	cancel  context.CancelFunc
	nextID  int
	block   func(stage, id string)
	s       *session
}

type hookEvent struct{ stage, id string }

type hopt func(*harness)

func withBudget(b time.Duration) hopt { return func(h *harness) { h.cfg.Budget = b } }

func withHostname(f func() (string, error)) hopt { return func(h *harness) { h.cfg.Hostname = f } }

// withOutput replaces stdout (the caller reads it).
func withOutput(w io.Writer, closer func() error) hopt {
	return func(h *harness) { h.cfg.Out, h.cfg.CloseOut = w, closer }
}

// withHook runs block for every hook event after it is recorded (every
// harness records its events).
func withHook(block func(stage, id string)) hopt {
	return func(h *harness) { h.block = block }
}

func withFactory(f Factory) hopt { return func(h *harness) { h.cfg.Factory = f } }

func withRealClock() hopt { return func(h *harness) { h.cfg.Clock = RealClock } }

// start runs a session; stdout lines arrive on h.lines unless withOutput
// replaced stdout.
func start(t testing.TB, opts ...hopt) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{t: t, clock: testkit.NewFakeClock(epoch), fake: &fakeClient{}, inW: inW, lines: make(chan []byte, 4096),
		stderr: &syncBuffer{}, code: make(chan int, 1), ev: newEventSet()}
	h.cfg = Config{Budget: DefaultBudget, Version: "test-1", Hostname: func() (string, error) { return "coord-host", nil }, Clock: h.clock,
		In: inR, Out: outW, Stderr: h.stderr, CloseIn: inR.Close, CloseOut: outW.Close}
	h.cfg.Factory = func(context.Context) (Client, error) {
		h.factory.Add(1)
		return h.fake, nil
	}
	h.cfg.Hook = func(stage, id string) {
		h.ev.record(hookEvent{stage, id})
		if h.block != nil {
			h.block(stage, id)
		}
	}
	for _, o := range opts {
		o(h)
	}
	if h.cfg.Out == io.Writer(outW) {
		go func() {
			br := bufio.NewReader(outR)
			for {
				line, err := br.ReadBytes('\n')
				if len(line) > 0 {
					h.lines <- line
				}
				if err != nil {
					close(h.lines)
					return
				}
			}
		}()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.s = newSession(h.cfg)
	go func() { h.code <- h.s.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		inW.Close()
		select {
		case <-h.code:
		case <-time.After(testWait):
			t.Error("the session did not end")
		}
	})
	return h
}

func (h *harness) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.inW, line+"\n"); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

// next returns the next stdout line decoded.
func (h *harness) next() map[string]any {
	h.t.Helper()
	line := h.nextRaw()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		h.t.Fatalf("stdout line is not JSON: %q", line)
	}
	return m
}

func (h *harness) nextRaw() []byte {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			h.t.Fatal("stdout ended")
		}
		if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
			h.t.Fatalf("stdout frame is not one line: %q", line)
		}
		return line
	case <-time.After(testWait):
		h.t.Fatal("no stdout line")
	}
	return nil
}

// exit waits for the session's exit code.
func (h *harness) exit() int {
	h.t.Helper()
	select {
	case c := <-h.code:
		h.code <- c
		return c
	case <-time.After(testWait):
		h.t.Fatal("the session did not end")
		return -1
	}
}

// await waits for a hook event; each recorded event satisfies one await.
func (h *harness) await(stage, id string) {
	h.t.Helper()
	if !h.ev.take(hookEvent{stage, id}, testWait) {
		h.t.Fatalf("hook %s %s not observed", stage, id)
	}
}

// eventSet counts recorded hook events.
type eventSet struct {
	mu     sync.Mutex
	cond   *sync.Cond
	counts map[hookEvent]int
}

func newEventSet() *eventSet {
	e := &eventSet{counts: map[hookEvent]int{}}
	e.cond = sync.NewCond(&e.mu)
	return e
}

func (e *eventSet) record(ev hookEvent) {
	e.mu.Lock()
	e.counts[ev]++
	e.mu.Unlock()
	e.cond.Broadcast()
}

// peek reports whether ev was recorded (without consuming it).
func (e *eventSet) peek(ev hookEvent) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.counts[ev] > 0
}

// take consumes one occurrence of ev, waiting at most timeout.
func (e *eventSet) take(ev hookEvent, timeout time.Duration) bool {
	expired := false
	t := time.AfterFunc(timeout, func() {
		e.mu.Lock()
		expired = true
		e.mu.Unlock()
		e.cond.Broadcast()
	})
	defer t.Stop()
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.counts[ev] == 0 {
		if expired {
			return false
		}
		e.cond.Wait()
	}
	if e.counts[ev]--; e.counts[ev] == 0 {
		delete(e.counts, ev)
	}
	return true
}

const initLine = `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-client","version":"1.2.3"}}}`

// ready initializes the session.
func (h *harness) ready() {
	h.t.Helper()
	h.send(initLine)
	if r := h.next(); r["id"] != "init" || r["result"] == nil {
		h.t.Fatalf("initialize = %v", r)
	}
	h.await(StageWritten, "sinit")
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// call sends tools/call with integer id and arguments (JSON).
func (h *harness) call(id int, name, args string) {
	h.t.Helper()
	p := `{"name":"` + name + `"`
	if args != "" {
		p += `,"arguments":` + args
	}
	h.send(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":` + p + `}}`)
}

func itoa(n int) string { return strconv.Itoa(n) }

// toolAnswer is a decoded tool result.
type toolAnswer struct {
	id      any
	isError bool
	text    string
}

// answer reads the next line as a tool result.
func (h *harness) answer() toolAnswer {
	h.t.Helper()
	m := h.next()
	res, ok := m["result"].(map[string]any)
	if !ok {
		h.t.Fatalf("not a tool result: %v", m)
	}
	content := res["content"].([]any)
	if len(content) != 1 {
		h.t.Fatalf("content %v", content)
	}
	item := content[0].(map[string]any)
	if item["type"] != "text" || len(res) != 2 {
		h.t.Fatalf("result shape %v", res)
	}
	return toolAnswer{id: m["id"], isError: res["isError"].(bool), text: item["text"].(string)}
}

// errorOf decodes a tool error's contract error.
func (a toolAnswer) errorOf(t *testing.T) *contract.Error {
	t.Helper()
	if !a.isError {
		t.Fatalf("not a tool error: %s", a.text)
	}
	e, err := contract.ParseErrorBody([]byte(a.text))
	if err != nil {
		t.Fatalf("tool error text %q: %v", a.text, err)
	}
	return e
}
