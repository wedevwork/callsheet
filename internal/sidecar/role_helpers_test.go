package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// scriptAdapter is an injected fake adapter: its probe counts calls,
// optionally blocks until released (or canceled) and returns a scripted
// result. Descriptor matches the product fake.
type scriptAdapter struct {
	mu      sync.Mutex
	probes  int
	fail    error
	block   chan struct{}
	started chan string
	exes    []string
	// stuck makes a blocked probe ignore cancellation, like a local
	// filesystem call that has not returned.
	stuck bool
}

func newScript() *scriptAdapter { return &scriptAdapter{started: make(chan string, 64)} }

func (a *scriptAdapter) Descriptor() adapter.Descriptor {
	return adapter.Descriptor{ID: adapter.FakeID, Efforts: []string{"low", "medium", "high"}, TestOnly: true}
}

func (a *scriptAdapter) Probe(ctx context.Context, exe string) error {
	a.mu.Lock()
	a.probes++
	a.exes = append(a.exes, exe)
	block, fail, stuck := a.block, a.fail, a.stuck
	a.mu.Unlock()
	a.started <- exe
	if block != nil {
		var canceled <-chan struct{}
		if !stuck {
			canceled = ctx.Done()
		}
		select {
		case <-block:
		case <-canceled:
			return ctx.Err()
		}
	}
	return fail
}

// set scripts the next probes and forgets start signals of earlier ones,
// so awaitProbe waits for a probe started after it.
func (a *scriptAdapter) set(fail error, block chan struct{}) {
	a.mu.Lock()
	a.fail, a.block = fail, block
	a.mu.Unlock()
	for {
		select {
		case <-a.started:
			continue
		default:
		}
		return
	}
}

// setStuck makes blocked probes ignore (or honor) cancellation.
func (a *scriptAdapter) setStuck(stuck bool) {
	a.mu.Lock()
	a.stuck = stuck
	a.mu.Unlock()
}

func (a *scriptAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probes
}

// awaitProbe waits for the next probe start.
func (a *scriptAdapter) awaitProbe(t testing.TB) string {
	t.Helper()
	select {
	case exe := <-a.started:
		return exe
	case <-time.After(testWait):
		t.Fatal("no probe started")
		return ""
	}
}

// Invocation and NewFinalExtractor are the product fake's task boundary
// (pure: no file or process).
func (a *scriptAdapter) Invocation(in adapter.TaskInput) (adapter.Invocation, error) {
	return adapter.NewFake("").Invocation(in)
}

func (a *scriptAdapter) NewFinalExtractor() adapter.FinalExtractor {
	return adapter.NewFake("").NewFinalExtractor()
}

func (a *scriptAdapter) registry() adapter.Registry {
	r, err := adapter.NewRegistry(a)
	if err != nil {
		panic(err)
	}
	return r
}

// fakeExe is the injected tests' executable path (never executed).
const fakeExe = "/opt/fake-adapter/bin/fake adapter"

// manuals writes an instruction and runbook for id under dir and returns
// their paths.
func manuals(t testing.TB, dir, id, content string) (string, string) {
	t.Helper()
	d := filepath.Join(dir, id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	ins, run := filepath.Join(d, "instruction.md"), filepath.Join(d, "runbook.md")
	for _, p := range []string{ins, run} {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ins, run
}

// roleConfig is a resolved configuration for this test node.
func roleConfig(id, ins, run string) contract.RoleConfig {
	return contract.RoleConfig{ID: id, Name: "coder", Node: testID, Adapter: "fake", Instruction: ins, Runbook: run,
		Model: "example model", Effort: "medium", Concurrency: 2, Timeout: 2 * time.Hour, HasTimeout: true}
}

// roleRun is a sidecar Run against a fake plane with a fake clock, an
// injected scripted adapter and (optionally) the fake adapter enabled.
type roleRun struct {
	*fakeRun
	script *scriptAdapter
	dir    string
}

func startRoleRun(t testing.TB, fp *fakePlane, enabled bool) *roleRun {
	t.Helper()
	return startRoleRunWith(t, fp, enabled, nil)
}

// startRoleRunWith is startRoleRun with prepared deps adjustments.
func startRoleRunWith(t testing.TB, fp *fakePlane, enabled bool, adjust func(d *deps)) *roleRun {
	t.Helper()
	rr := &roleRun{script: newScript(), dir: t.TempDir()}
	f := &fakeRun{fp: fp, clk: testkit.NewFakeClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)), logs: newSyncLog(), root: newRoot(t)}
	f.d = testDeps(f.clk)
	f.d.adapters = func(string) adapter.Registry { return rr.script.registry() }
	if adjust != nil {
		adjust(f.d)
	}
	f.ev = observe(f.d)
	if fp != nil {
		fp.ev = f.ev
		fp.mu.Lock()
		fp.autoAck = true
		fp.mu.Unlock()
	}
	url, ca := "https://127.0.0.1:1", []byte(nil)
	if fp != nil {
		url, ca = fp.url, fp.caPEM
	} else {
		fixture, err := testkit.NewFixtureCA()
		if err != nil {
			t.Fatal(err)
		}
		ca = fixture.CertPEM
	}
	writeState(t, f.root, testID, url, ca)
	exe := ""
	if enabled {
		exe = fakeExe
	}
	f.run = startRunOpts(t, f.d, RunOptions{StateDir: f.root, SoftwareVersion: "test-1", Logger: slog.New(slog.NewJSONHandler(f.logs, nil)), FakeAdapterPath: exe})
	rr.fakeRun = f
	return rr
}

// startRunOpts is startRun with complete options.
func startRunOpts(t testing.TB, d *deps, o RunOptions) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- d.run(ctx, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(testWait):
			t.Error("run did not stop")
		}
	})
	return r
}

// awaitKind returns the next event of kind (skipping others) satisfying
// match, if given.
func (e *events) awaitMatch(t testing.TB, kind eventKind, match func(event) bool) event {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case ev := <-e.ch:
			if ev.kind == kind && (match == nil || match(ev)) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no matching %s event", kind)
		}
	}
}

// connect answers hello, acknowledges heartbeat b1 at revision 0 and
// reconciles an empty inventory (protocol 4: nothing else of the plane's
// precedes it).
func (c *fakeConn) connect() {
	c.t.Helper()
	c.helloOK(testID)
	c.heartbeatAt(1, 0)
	c.reconcileEmpty()
}

// heartbeatAt reads heartbeat k, checks its revision and acknowledges it
// (in auto mode: the dispatcher's next observation, already acknowledged).
func (c *fakeConn) heartbeatAt(k, rev int) contract.HeartbeatBody {
	c.t.Helper()
	var b contract.HeartbeatBody
	if c.auto.Load() {
		b = c.beatK(k)
	} else {
		b = c.readHeartbeat(k)
	}
	if b.RolesRevision != rev {
		c.t.Fatalf("heartbeat b%d revision %d, want %d", k, b.RolesRevision, rev)
	}
	if !c.auto.Load() {
		c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b"+strconv.Itoa(k), nil)
	}
	return b
}

// beat acknowledges heartbeat k (checking its revision) and waits until the
// sidecar processed that acknowledgement, so the test may move the clock.
func (rr *roleRun) beat(c *fakeConn, k, rev int) contract.HeartbeatBody {
	c.t.Helper()
	b := c.heartbeatAt(k, rev)
	if c.auto.Load() {
		rr.ev.awaitAck(c.t, c.minSession, k)
	} else {
		rr.ev.awaitMatch(c.t, evAck, func(ev event) bool { return ev.acks == k })
	}
	return b
}

// report waits until the latest heartbeat observation is at revision rev
// with statuses satisfying pred (a prompt readiness report or a periodic
// heartbeat; earlier observations are consumed) and the sidecar processed
// its acknowledgement, and returns it.
func (rr *roleRun) report(c *fakeConn, rev int, pred func(map[string]bool) bool) beatObs {
	c.t.Helper()
	b := c.awaitLatest(func(b beatObs) bool { return b.body.RolesRevision == rev && pred(statuses(b.body)) })
	rr.ev.awaitAck(c.t, c.minSession, b.k)
	return b
}

// readHeartbeat reads heartbeat k without acknowledging it (manual mode).
func (c *fakeConn) readHeartbeat(k int) contract.HeartbeatBody {
	c.t.Helper()
	if c.auto.Load() {
		c.t.Fatal("readHeartbeat needs the manual acknowledgement mode")
	}
	f := c.expect(contract.FrameHeartbeat, "b"+strconv.Itoa(k))
	b, err := contract.DecodeHeartbeat(f.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

// replace sends a full snapshot rid.
func (c *fakeConn) replace(rid string, rev int, roles ...contract.RoleConfig) {
	c.t.Helper()
	body := contract.RolesReplaceBody{Revision: rev}
	for i, r := range roles {
		body.Roles = append(body.Roles, contract.RoleRecord{RoleConfig: r, RegistrationOrder: i + 1})
	}
	c.send(contract.ProtocolVersion, contract.FrameRolesReplace, rid, body)
}

// expectReplaceAck reads the acknowledgement of rid; task output and
// results a reconciled attachment sent first are held for later reads.
func (c *fakeConn) expectReplaceAck(rid string, rev int) {
	c.t.Helper()
	var skipped []contract.NodeFrame
	var f contract.NodeFrame
	for {
		f = c.recv()
		if f.Type == contract.FrameTaskLog || f.Type == contract.FrameTaskResult {
			skipped = append(skipped, f)
			continue
		}
		break
	}
	c.held = append(skipped, c.held...)
	if f.Type != contract.FrameRolesReplaceAck || f.RequestID != rid {
		c.t.Fatalf("got %s %s (%s), want %s %s", f.Type, f.RequestID, f.Body, contract.FrameRolesReplaceAck, rid)
	}
	if got, err := contract.DecodeRolesReplaceAck(f.Body); err != nil || got != rev {
		c.t.Fatalf("ack %s = %d %v, want %d", rid, got, err, rev)
	}
	if c.ev != nil {
		c.ev.awaitWritten(c.t, evAckWritten, rid)
	}
}

// validate sends role_validate rid for cfg.
func (c *fakeConn) validate(rid string, cfg contract.RoleConfig) {
	c.t.Helper()
	c.send(contract.ProtocolVersion, contract.FrameRoleValidate, rid, contract.RoleValidateBody{Role: cfg})
}

// result reads validation result rid.
func (c *fakeConn) result(rid string) *contract.Error {
	c.t.Helper()
	f := c.expect(contract.FrameRoleValidateResult, rid)
	e, err := contract.DecodeRoleValidateResult(f.Body)
	if err != nil {
		c.t.Fatalf("result %s: %v", f.Body, err)
	}
	if c.ev != nil {
		c.ev.awaitWritten(c.t, evReplied, rid)
	}
	return e
}

// statuses maps a heartbeat's role readiness by ID.
func statuses(b contract.HeartbeatBody) map[string]bool {
	out := map[string]bool{}
	for _, r := range b.Roles {
		out[r.RoleID] = r.CanAccept
	}
	return out
}

func wantResult(t testing.TB, e *contract.Error, code contract.Code, field, reason string) {
	t.Helper()
	if e == nil || e.Code != code {
		t.Fatalf("result = %v, want %s", e, code)
	}
	if f, _ := e.Details["field"].(string); f != field {
		t.Fatalf("result field %v, want %q (%v)", e.Details, field, e)
	}
	if r, _ := e.Details["reason"].(string); r != reason {
		t.Fatalf("result reason %v, want %q (%v)", e.Details, reason, e)
	}
}

// pipePlane is a planeClient whose node stream is a WebSocket over
// net.Pipe: nothing is buffered, so a peer that stops reading blocks the
// sidecar's writes. Each dial is handed to the test.
type pipePlane struct {
	conns chan *pipeConn
}

type pipeConn struct {
	ws      *websocket.Conn
	release func()
}

func newPipePlane() *pipePlane { return &pipePlane{conns: make(chan *pipeConn, 4)} }

func (p *pipePlane) EnrollNode(context.Context, string, string) (contract.Node, error) {
	return contract.Node{}, errors.New("not supported")
}

func (p *pipePlane) Close() {}

func (p *pipePlane) DialNodeStream(ctx context.Context) (*websocket.Conn, error) {
	c1, c2 := net.Pipe()
	accepted := make(chan *websocket.Conn, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- ws // hijacked: returning leaves the connection open
	})}
	ln := &oneConnListener{c: c2, done: make(chan struct{})}
	go srv.Serve(ln)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return c1, nil }}}
	ws, _, err := websocket.Dial(ctx, "ws://pipe/", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(contract.MaxFrameBytes)
	server := <-accepted
	p.conns <- &pipeConn{ws: server, release: func() { c1.Close(); c2.Close(); ln.Close() }}
	return ws, nil
}

// accept returns the next dialed pipe connection.
func (p *pipePlane) accept(t testing.TB) *pipeConn {
	t.Helper()
	select {
	case c := <-p.conns:
		t.Cleanup(c.release)
		return c
	case <-time.After(testWait):
		t.Fatal("the sidecar did not dial")
		return nil
	}
}

func (c *pipeConn) read(t testing.TB) contract.NodeFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, testWait)
	defer cancel()
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		t.Fatalf("pipe read: %v", err)
	}
	f, err := contract.DecodeFrame(b, contract.FromSidecar)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (c *pipeConn) send(t testing.TB, typ, rid string, body any) {
	t.Helper()
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, rid, body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, testWait)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// reconcileEmpty acknowledges an empty final inventory page i1 and sends
// the empty final reconcile page r1, reading its acknowledgement.
func (c *pipeConn) reconcileEmpty(t testing.TB) {
	t.Helper()
	f := c.read(t)
	b, err := contract.DecodeTaskInventory(f.Body)
	if f.Type != contract.FrameTaskInventory || f.RequestID != "i1" || err != nil || !b.Final || len(b.Entries) != 0 {
		t.Fatalf("inventory = %+v %v", f, err)
	}
	c.send(t, contract.FrameTaskInventoryAck, "i1", contract.TaskInventoryAckBody{Page: 0, Received: true})
	c.send(t, contract.FrameTaskReconcile, "r1", contract.TaskReconcileBody{Final: true})
	if f := c.read(t); f.Type != contract.FrameTaskReconcileAck || f.RequestID != "r1" {
		t.Fatalf("reconcile ack = %+v", f)
	}
}
