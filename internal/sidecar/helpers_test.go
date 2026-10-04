package sidecar

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/plane"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// testCLIBinaryEnv names an already built callsheet binary. It is read
// only here, in test code: the function-test wrapper sets it so a
// delegated contract reuses the binary it built once. Without it the
// package builds its own once per test process (cliFixture).
const testCLIBinaryEnv = "CALLSHEET_TEST_CLI_BINARY"

// lockHelperEnv makes the test binary hold the sidecar state lock on the
// named root until stdin reaches EOF (TestSidecarNativeStateContract).
const lockHelperEnv = "CALLSHEET_SIDECAR_LOCK_HELPER"

// testWait bounds how long a test waits for an event (real time); product
// timing in these tests runs on the fake clock.
const testWait = 30 * time.Second

var bg = context.Background()

// cliFixture is the process-lifetime callsheet binary: built at most once
// per test process into fixtureDir, removed by TestMain at teardown.
var (
	cliOnce    sync.Once
	cliPath    string
	cliErr     error
	fixtureDir string
)

func TestMain(m *testing.M) {
	if root := os.Getenv(lockHelperEnv); root != "" {
		os.Exit(lockHelper(root))
	}
	if cfg := os.Getenv(FixtureEnv); cfg != "" {
		os.Exit(runFixture(cfg))
	}
	if len(os.Args) == 2 && os.Args[1] == GuardianToken {
		os.Exit(runInjectedGuardian(os.Args[1:]))
	}
	if os.Getenv(probeCalibrationEnv) != "" {
		os.Exit(probeCalibration())
	}
	code := m.Run()
	if fixtureDir != "" {
		os.RemoveAll(fixtureDir)
	}
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

// cliBinary returns the callsheet binary for plane subprocesses.
func cliBinary(t testing.TB) string {
	t.Helper()
	cliOnce.Do(func() {
		if p := os.Getenv(testCLIBinaryEnv); p != "" {
			cliPath = p
			return
		}
		fixtureDir, cliErr = os.MkdirTemp("", "callsheet-sidecar-fixture-")
		if cliErr == nil {
			cliPath, cliErr = testkit.BuildBinaryAt(fixtureDir, "./cmd/callsheet", "callsheet")
		}
	})
	if cliErr != nil {
		t.Fatalf("building the callsheet fixture: %v", cliErr)
	}
	return cliPath
}

func lockHelper(root string) int {
	lk, err := layout{root: root}.acquire()
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return 3
	}
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
	lk.release()
	return 0
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func wantCode(t testing.TB, err error, code contract.Code, substr ...string) {
	t.Helper()
	if contract.CodeOf(err) != code {
		t.Fatalf("err = %v (code %q), want %q", err, contract.CodeOf(err), code)
	}
	for _, s := range substr {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("err = %v, want it to contain %q", err, s)
		}
	}
}

// failAt injects err at exactly one (op, name) boundary.
func failAt(op, name string) func(string, string) error {
	return func(o, n string) error {
		if o == op && n == name {
			return fmt.Errorf("injected %s failure at %s", o, n)
		}
		return nil
	}
}

// testDeps are production deps (real client, filesystem and entropy) with
// a fake clock and a fixed jitter sample.
func testDeps(clk clock) *deps {
	d := defaultDeps()
	if clk != nil {
		d.clock = clk
	}
	d.jitter = func() float64 { return 0.5 }
	return d
}

// syncLog is a concurrency-safe log capture that signals records.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
	sig chan struct{}
}

func newSyncLog() *syncLog { return &syncLog{sig: make(chan struct{}, 1)} }

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.buf.Write(p)
	select {
	case l.sig <- struct{}{}:
	default:
	}
	return n, err
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// await waits (bounded) until pred holds for the captured text.
func (l *syncLog) await(t testing.TB, what string, pred func(string) bool) string {
	t.Helper()
	deadline := time.After(testWait)
	for {
		s := l.String()
		if pred(s) {
			return s
		}
		select {
		case <-l.sig:
		case <-deadline:
			// A record and the deadline may be ready together: recheck
			// before failing.
			if s := l.String(); pred(s) {
				return s
			}
			t.Fatalf("no %s in:\n%s", what, l.String())
		}
	}
}

// listenAddr extracts the bind of the first listening record.
func listenAddr(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "listening" {
			if b, ok := rec["bind"].(string); ok {
				return b
			}
		}
	}
	return ""
}

// inPlane is a real plane served in this process.
type inPlane struct {
	root, url, caFile string
	logs              *syncLog
	cancel            context.CancelFunc
	done              chan error
}

// startPlane initializes and serves a plane (SAN 127.0.0.1) in-process.
func startPlane(t testing.TB) *inPlane {
	t.Helper()
	p := &inPlane{root: filepath.Join(t.TempDir(), "plane"), logs: newSyncLog(), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(bg)
	p.cancel = cancel
	logger := slog.New(slog.NewJSONHandler(p.logs, nil))
	go func() {
		p.done <- plane.Run(ctx, plane.RunOptions{StateDir: p.root, Bind: "127.0.0.1:0", BindSet: true, SANs: []string{"127.0.0.1"}, SANsSet: true, Logger: logger})
	}()
	var addr string
	p.logs.await(t, "listening record", func(s string) bool { addr = listenAddr(s); return addr != "" })
	p.url = "https://" + addr
	p.caFile = filepath.Join(p.root, "pki", "ca.crt")
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *inPlane) stop(t testing.TB) {
	t.Helper()
	p.cancel()
	select {
	case <-p.done:
		p.done <- nil
	case <-time.After(testWait):
		t.Error("plane did not stop")
	}
}

func (p *inPlane) client(t testing.TB) *client.Client {
	t.Helper()
	ca, err := os.ReadFile(p.caFile)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(p.url, client.Trust{CAPEM: ca})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// fakePlane is a scriptable node-stream server trusted through a fixture
// CA: each upgraded connection is handed to the test.
type fakePlane struct {
	url   string
	caPEM []byte
	conns chan *fakeConn
	// ev, when set (role runs), lets the connection helpers wait for the
	// sidecar's write of a message they read to return.
	ev *events
	// refuse, when set, answers upgrades with that HTTP status; onUpgrade,
	// when set, runs in the handler before an upgrade is answered (the
	// sidecar's dial is then in progress).
	mu        sync.Mutex
	refuse    int
	onUpgrade func()
	// autoAck makes every accepted connection's dispatcher acknowledge the
	// sidecar's heartbeats itself (iteration 10a; role and task runs).
	// Without it, heartbeats are delivered like any message and the test
	// acknowledges them (the manual mode of the ack-loss, coalescing and
	// deadline tests, and of runs without roles).
	autoAck bool
}

type fakeConn struct {
	t    testing.TB
	ev   *events
	ws   *websocket.Conn
	done chan struct{}
	once sync.Once
	// in receives every message, then the terminal read error: like a
	// real plane, the fake always reads, so close handshakes complete.
	// In auto-acknowledgement mode heartbeats are not delivered here.
	in chan readResult
	// held are sidecar task frames read past while waiting for another
	// reply (a reconciled attachment's output may precede the snapshot's
	// acknowledgement); recv returns them first.
	held []contract.NodeFrame
	// Iteration 10a's shared receive dispatcher: in auto mode, the read
	// goroutine recognizes every heartbeat by its type and request ID,
	// validates its sequence and revision order, acknowledges it at once
	// and queues the observation on beats (in order) for assertions.
	auto       atomic.Bool
	beats      chan beatObs
	bmu        sync.Mutex
	last       beatObs
	ackedK     []int
	minSession int
}

// beatObs is one heartbeat the dispatcher observed and acknowledged: its
// request number and body.
type beatObs struct {
	k    int
	body contract.HeartbeatBody
	raw  json.RawMessage
}

func (c *fakeConn) finish() { c.once.Do(func() { close(c.done) }) }

func startFakePlane(t testing.TB) *fakePlane {
	t.Helper()
	ca, err := testkit.NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePlane{caPEM: ca.CertPEM, conns: make(chan *fakeConn, 16)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fp.url = "https://" + ln.Addr().String()
	srv := &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		refuse, hook := fp.refuse, fp.onUpgrade
		fp.mu.Unlock()
		if hook != nil && refuse == 0 {
			hook()
		}
		if refuse != 0 {
			w.WriteHeader(refuse)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		c := &fakeConn{t: t, ws: ws, done: make(chan struct{}), in: make(chan readResult, 64), beats: make(chan beatObs, 1024)}
		fp.mu.Lock()
		c.auto.Store(fp.autoAck)
		fp.mu.Unlock()
		go func() {
			for {
				typ, b, err := ws.Read(context.Background())
				if err == nil && c.auto.Load() && c.dispatch(typ, b) {
					continue
				}
				c.in <- readResult{typ: typ, data: b, err: err}
				if err != nil {
					return
				}
			}
		}()
		fp.conns <- c
		<-c.done
		ws.CloseNow()
	})}
	done := make(chan struct{})
	go func() {
		srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
		close(done)
	}()
	t.Cleanup(func() {
		srv.Close()
	drain:
		for {
			select {
			case c := <-fp.conns:
				c.finish()
			default:
				break drain
			}
		}
		<-done
	})
	return fp
}

func (fp *fakePlane) setRefuse(status int) {
	fp.mu.Lock()
	fp.refuse = status
	fp.mu.Unlock()
}

// accept returns the next upgraded connection.
func (fp *fakePlane) accept(t testing.TB) *fakeConn {
	t.Helper()
	select {
	case c := <-fp.conns:
		t.Cleanup(c.finish)
		if fp.ev != nil {
			fp.ev.drainWritten()
			// Every acknowledgement of an earlier session was emitted
			// before this connection's: its own are the later sessions'.
			c.minSession = fp.ev.drainAcks() + 1
			c.ev = fp.ev
		}
		return c
	case <-time.After(testWait):
		t.Fatal("the sidecar did not connect")
		return nil
	}
}

// recv reads one sidecar message (bounded) and decodes its envelope.
func (c *fakeConn) next() readResult {
	c.t.Helper()
	select {
	case r := <-c.in:
		return r
	case <-time.After(testWait):
		c.t.Fatal("no message from the sidecar")
		return readResult{}
	}
}

func (c *fakeConn) recv() contract.NodeFrame {
	c.t.Helper()
	if len(c.held) > 0 {
		f := c.held[0]
		c.held = c.held[1:]
		return f
	}
	r := c.next()
	if r.err != nil {
		c.t.Fatalf("fake plane read: %v", r.err)
	}
	if r.typ != websocket.MessageText {
		c.t.Fatal("binary from the sidecar")
	}
	f, err := contract.DecodeFrame(r.data, contract.FromSidecar)
	if err != nil {
		c.t.Fatalf("sidecar message %s: %v", r.data, err)
	}
	return f
}

func (c *fakeConn) expect(typ, rid string) contract.NodeFrame {
	c.t.Helper()
	f := c.recv()
	if f.Type != typ || f.RequestID != rid {
		c.t.Fatalf("got %s %s (%s), want %s %s", f.Type, f.RequestID, f.Body, typ, rid)
	}
	return f
}

func (c *fakeConn) sendRaw(b []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(bg, testWait)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("fake plane write: %v", err)
	}
}

func (c *fakeConn) send(version int, typ, rid string, body any) {
	c.t.Helper()
	b, err := contract.EncodeFrame(version, typ, rid, body)
	if err != nil {
		c.t.Fatal(err)
	}
	c.sendRaw(b)
}

// helloOK answers the hello.
func (c *fakeConn) helloOK(id string) {
	c.t.Helper()
	f := c.expect(contract.FrameHello, "h1")
	h, err := contract.DecodeHello(f.Body)
	if err != nil || h.NodeID != id {
		c.t.Fatalf("hello %+v %v", h, err)
	}
	c.send(contract.ProtocolVersion, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS})
}

// ack answers heartbeat k (in auto mode: requires the dispatcher's next
// observation to be k, already acknowledged).
func (c *fakeConn) ack(k int) {
	c.t.Helper()
	if c.auto.Load() {
		c.beatK(k)
		return
	}
	rid := "b" + strconv.Itoa(k)
	c.expect(contract.FrameHeartbeat, rid)
	c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, rid, nil)
}

// dispatch is the auto mode's read-goroutine step for one message: a
// well-formed heartbeat later in the sequence than the previous one, at no
// older revision, is acknowledged and queued; anything else is delivered
// to the test (which fails on an unexpected message).
func (c *fakeConn) dispatch(typ websocket.MessageType, b []byte) bool {
	if typ != websocket.MessageText {
		return false
	}
	f, err := contract.DecodeFrame(b, contract.FromSidecar)
	if err != nil || f.Type != contract.FrameHeartbeat || !strings.HasPrefix(f.RequestID, "b") {
		return false
	}
	k, err := strconv.Atoi(f.RequestID[1:])
	if err != nil {
		return false
	}
	hb, err := contract.DecodeHeartbeat(f.Body)
	if err != nil {
		return false
	}
	c.bmu.Lock()
	ok := k > c.last.k && (c.last.k == 0 || hb.RolesRevision >= c.last.body.RolesRevision)
	if ok {
		c.last = beatObs{k: k, body: hb}
		c.ackedK = append(c.ackedK, k)
	}
	c.bmu.Unlock()
	if !ok {
		return false
	}
	ack, err := contract.EncodeFrame(contract.ProtocolVersion, contract.FrameHeartbeatAck, f.RequestID, nil)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(bg, testWait)
	c.ws.Write(ctx, websocket.MessageText, ack)
	cancel()
	c.beats <- beatObs{k: k, body: hb, raw: f.Body}
	return true
}

// setManual switches the dispatcher to manual acknowledgement: later
// heartbeats are delivered like any message. Call it while no heartbeat
// is in flight.
func (c *fakeConn) setManual() { c.auto.Store(false) }

// setAuto switches the dispatcher back to auto acknowledgement (before
// the next heartbeat can arrive).
func (c *fakeConn) setAuto() { c.auto.Store(true) }

// nextBeat returns the dispatcher's next queued observation (in order).
func (c *fakeConn) nextBeat() beatObs {
	c.t.Helper()
	select {
	case b := <-c.beats:
		return b
	case <-time.After(testWait):
		c.t.Fatal("no heartbeat from the sidecar")
		return beatObs{}
	}
}

// beatK requires the next queued observation to be heartbeat k.
func (c *fakeConn) beatK(k int) contract.HeartbeatBody {
	c.t.Helper()
	b := c.nextBeat()
	if b.k != k {
		c.t.Fatalf("heartbeat b%d (%+v), want b%d", b.k, b.body, k)
	}
	return b.body
}

// awaitBeat consumes queued observations until one satisfies pred.
func (c *fakeConn) awaitBeat(pred func(beatObs) bool) beatObs {
	c.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case b := <-c.beats:
			if pred(b) {
				return b
			}
		case <-deadline:
			c.t.Fatal("no matching heartbeat from the sidecar")
			return beatObs{}
		}
	}
}

// awaitLatest waits until the latest observation so far satisfies pred
// (earlier ones are consumed; a stale match followed by a mismatch keeps
// waiting) and returns it.
func (c *fakeConn) awaitLatest(pred func(beatObs) bool) beatObs {
	c.t.Helper()
	deadline := time.After(testWait)
	var latest *beatObs
	for {
		select {
		case b := <-c.beats:
			latest = &b
			continue
		default:
		}
		if latest != nil && pred(*latest) {
			return *latest
		}
		select {
		case b := <-c.beats:
			latest = &b
		case <-deadline:
			c.t.Fatalf("no heartbeat in the expected state (latest %+v)", latest)
			return beatObs{}
		}
	}
}

// drainBeats drops every queued observation.
func (c *fakeConn) drainBeats() {
	for {
		select {
		case <-c.beats:
		default:
			return
		}
	}
}

// noBeat requires no queued observation.
func (c *fakeConn) noBeat() {
	c.t.Helper()
	select {
	case b := <-c.beats:
		c.t.Fatalf("unexpected heartbeat b%d %+v", b.k, b.body)
	default:
	}
}

// settle waits until the sidecar processed every acknowledgement the
// dispatcher sent so far: only then may a test move the clock past an
// exchange deadline.
func (c *fakeConn) settle() {
	c.t.Helper()
	c.bmu.Lock()
	ks := slices.Clone(c.ackedK)
	c.bmu.Unlock()
	for _, k := range ks {
		c.ev.awaitAck(c.t, c.minSession, k)
	}
}

// closed reads until the socket ends and returns its close status.
func (c *fakeConn) closed() websocket.StatusCode {
	c.t.Helper()
	for {
		if r := c.next(); r.err != nil {
			return websocket.CloseStatus(r.err)
		}
	}
}

// writeState writes identity.json and enrollment.json directly.
func writeState(t testing.TB, root, id, url string, caPEM []byte) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	var e enrollmentFile
	e.SchemaVersion, e.PlaneURL, e.CAPEM = SchemaVersion, url, string(caPEM)
	c, err := client.New(url, client.Trust{CAPEM: caPEM})
	if err != nil {
		t.Fatal(err)
	}
	e.CAFingerprint = c.Trust().Fingerprint
	os.WriteFile(filepath.Join(root, identityName), encode(identityFile{SchemaVersion: SchemaVersion, NodeID: id}), 0o600)
	os.WriteFile(filepath.Join(root, enrollmentName), encode(e), 0o600)
}

// events collects observed transitions.
type events struct {
	ch chan event
	// written receives the write-completion events (ack-written, replied)
	// a second time, so a test can wait for a write to return without
	// consuming the main stream.
	written chan event
	// collected receives every worker collection (iteration 05) a second
	// time; gone remembers the IDs read from it (test goroutine only).
	collected chan event
	gone      map[string]bool
	// cycles receives every completed ready-check cycle a second time
	// (awaitCycleSince), without consuming the main stream.
	cycles chan event
	// ackc receives every processed heartbeat acknowledgement a second
	// time (iteration 10a: awaitAck); amu guards acked, the (session, k)
	// pairs read from it, and maxSession.
	ackc       chan event
	amu        sync.Mutex
	acked      map[[2]int]bool
	maxSession int
	// armed is each session's latest periodic heartbeat timer arming
	// (evHeartbeatArmed, never on the main stream, so it adds nothing to
	// a caller's queue bounds); armSig is closed and replaced on each.
	armMu  sync.Mutex
	armed  map[int]event
	armSig chan struct{}
	// replied records the task IDs whose start reply's write returned
	// (evStartReplied, also on the main stream), so a test can wait for
	// that write without consuming the stream (awaitStartReplied); repSig
	// is closed and replaced on each.
	repMu   sync.Mutex
	replied map[string]bool
	repSig  chan struct{}
}

func observe(d *deps) *events {
	e := &events{ch: make(chan event, 4096), written: make(chan event, 4096), collected: make(chan event, 4096), gone: map[string]bool{},
		cycles: make(chan event, 4096), ackc: make(chan event, 4096), acked: map[[2]int]bool{}, armed: map[int]event{}, armSig: make(chan struct{}),
		replied: map[string]bool{}, repSig: make(chan struct{})}
	// A tap the test installed before (deps adjustments) still sees
	// every event first.
	tap := d.observe
	d.observe = func(ev event) {
		if tap != nil {
			tap(ev)
		}
		if ev.kind == evHeartbeatArmed {
			e.armMu.Lock()
			e.armed[ev.session] = ev
			close(e.armSig)
			e.armSig = make(chan struct{})
			e.armMu.Unlock()
			return
		}
		if ev.kind == evStartReplied {
			e.repMu.Lock()
			e.replied[ev.id] = true
			close(e.repSig)
			e.repSig = make(chan struct{})
			e.repMu.Unlock()
		}
		e.ch <- ev
		if ev.kind == evAck {
			e.ackc <- ev
		}
		if ev.kind == evAckWritten || ev.kind == evReplied {
			e.written <- ev
		}
		if ev.kind == evTaskCollected {
			e.collected <- ev
		}
		if ev.kind == evCycleDone {
			e.cycles <- ev
		}
	}
	return e
}

// awaitStartReplied waits until the write of task id's start reply
// returned (its bytes may reach the plane while that write, bounded by the
// clock, is still active: a test that read the reply must not move the
// clock before then). It does not consume the main event stream, so it
// holds whichever of the reply and a concurrent worker event came first.
func (e *events) awaitStartReplied(t testing.TB, id string) {
	t.Helper()
	deadline := time.After(testWait)
	for {
		e.repMu.Lock()
		sig, done := e.repSig, e.replied[id]
		e.repMu.Unlock()
		if done {
			return
		}
		select {
		case <-sig:
		case <-deadline:
			e.repMu.Lock()
			done = e.replied[id]
			e.repMu.Unlock()
			if done {
				return
			}
			t.Fatalf("the start reply write of %s did not return", id)
		}
	}
}

// awaitWritten waits until the sidecar's write of message id (kind
// evAckWritten or evReplied) returned: a test that read the message must
// not move the clock before then, or it may fire that write's own bound.
func (e *events) awaitWritten(t testing.TB, kind eventKind, id string) {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case ev := <-e.written:
			if ev.kind == kind && ev.id == id {
				return
			}
		case <-deadline:
			t.Fatalf("the %s write of %s did not return", kind, id)
		}
	}
}

// discard drops the events already recorded on the main stream and on
// the cycle, collection and acknowledgement copies (never the write
// completions, which the connection helpers consume), and returns how many
// the main stream held: a long-running caller (a benchmark) keeps the
// recorder's queues bounded with it.
func (e *events) discard() int {
	n := len(e.ch)
	for _, c := range []chan event{e.ch, e.cycles, e.collected, e.ackc} {
	drain:
		for {
			select {
			case <-c:
			default:
				break drain
			}
		}
	}
	return n
}

// noteAck records one processed acknowledgement (under amu).
func (e *events) noteAck(ev event) {
	e.acked[[2]int{ev.session, ev.acks}] = true
	e.maxSession = max(e.maxSession, ev.session)
}

// drainAcks records every acknowledgement event already emitted and
// returns the latest session seen.
func (e *events) drainAcks() int {
	e.amu.Lock()
	defer e.amu.Unlock()
	for {
		select {
		case ev := <-e.ackc:
			e.noteAck(ev)
		default:
			return e.maxSession
		}
	}
}

// awaitAck waits until a session numbered minSession or later processed
// the acknowledgement of its heartbeat k (read from the acks copy, never
// consuming the main stream).
func (e *events) awaitAck(t testing.TB, minSession, k int) {
	t.Helper()
	deadline := time.After(testWait)
	for {
		e.amu.Lock()
		for s := minSession; s <= e.maxSession; s++ {
			if e.acked[[2]int{s, k}] {
				e.amu.Unlock()
				return
			}
		}
		e.amu.Unlock()
		select {
		case ev := <-e.ackc:
			e.amu.Lock()
			e.noteAck(ev)
			e.amu.Unlock()
		case <-deadline:
			t.Fatalf("the acknowledgement of heartbeat b%d was never processed", k)
		}
	}
}

// awaitHeartbeatArmed waits until a session numbered minSession or later
// armed its periodic heartbeat timer for exactly instant at after request
// k's exchange completed (that session's latest arming): no exchange is
// outstanding then, and advancing the clock to at fires that timer.
func (e *events) awaitHeartbeatArmed(t testing.TB, minSession, k int, at time.Time) {
	t.Helper()
	deadline := time.After(testWait)
	for {
		e.armMu.Lock()
		sig := e.armSig
		var latest []event
		for s, ev := range e.armed {
			if s >= minSession {
				if ev.acks == k && ev.at.Equal(at) {
					e.armMu.Unlock()
					return
				}
				latest = append(latest, ev)
			}
		}
		e.armMu.Unlock()
		select {
		case <-sig:
		case <-deadline:
			e.armMu.Lock()
			for s, ev := range e.armed {
				if s >= minSession && ev.acks == k && ev.at.Equal(at) {
					e.armMu.Unlock()
					return
				}
			}
			e.armMu.Unlock()
			t.Fatalf("the heartbeat timer was never armed for %v after b%d (latest armings %+v)", at, k, latest)
		}
	}
}

// awaitCycleSince waits until a ready-check cycle of revision rev that
// started at or after since completed (read from the cycles copy).
func (e *events) awaitCycleSince(t testing.TB, rev int, since time.Time) event {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case ev := <-e.cycles:
			if ev.rev == rev && !ev.at.Before(since) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no ready-check cycle of revision %d since %v", rev, since)
		}
	}
}

// drainWritten drops the previous sessions' write completions: a session
// joins its writes before the sidecar reconnects.
func (e *events) drainWritten() {
	for {
		select {
		case <-e.written:
		default:
			return
		}
	}
}

// await returns the next event of kind, skipping others.
func (e *events) await(t testing.TB, kind eventKind) event {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case ev := <-e.ch:
			if ev.kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event", kind)
		}
	}
}

// running is Run in the background.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func startRun(t testing.TB, d *deps, root string, logger *slog.Logger) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- d.run(ctx, RunOptions{StateDir: root, SoftwareVersion: "test-1", Logger: logger}) }()
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

// result waits for Run to return.
func (r *running) result(t testing.TB) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(testWait):
		t.Fatal("run did not return")
		return nil
	}
}

// readLine reads one line from r with a bounded wait.
func readLine(t testing.TB, r io.Reader) string {
	t.Helper()
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		return s
	case <-time.After(testWait):
		t.Fatal("no line")
		return ""
	}
}
