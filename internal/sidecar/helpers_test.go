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
	"strconv"
	"strings"
	"sync"
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
	code := m.Run()
	if fixtureDir != "" {
		os.RemoveAll(fixtureDir)
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

func wantCode(t *testing.T, err error, code contract.Code, substr ...string) {
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
func (l *syncLog) await(t *testing.T, what string, pred func(string) bool) string {
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
func startPlane(t *testing.T) *inPlane {
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

func (p *inPlane) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	select {
	case <-p.done:
		p.done <- nil
	case <-time.After(testWait):
		t.Error("plane did not stop")
	}
}

func (p *inPlane) client(t *testing.T) *client.Client {
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
	// refuse, when set, answers upgrades with that HTTP status.
	mu     sync.Mutex
	refuse int
}

type fakeConn struct {
	t    *testing.T
	ws   *websocket.Conn
	done chan struct{}
	once sync.Once
	// in receives every message, then the terminal read error: like a
	// real plane, the fake always reads, so close handshakes complete.
	in chan readResult
}

func (c *fakeConn) finish() { c.once.Do(func() { close(c.done) }) }

func startFakePlane(t *testing.T) *fakePlane {
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
		refuse := fp.refuse
		fp.mu.Unlock()
		if refuse != 0 {
			w.WriteHeader(refuse)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		c := &fakeConn{t: t, ws: ws, done: make(chan struct{}), in: make(chan readResult, 64)}
		go func() {
			for {
				typ, b, err := ws.Read(context.Background())
				c.in <- readResult{typ, b, err}
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
func (fp *fakePlane) accept(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-fp.conns:
		t.Cleanup(c.finish)
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
	c.send(1, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS})
}

// ack answers heartbeat k.
func (c *fakeConn) ack(k int) {
	c.t.Helper()
	rid := "b" + strconv.Itoa(k)
	c.expect(contract.FrameHeartbeat, rid)
	c.send(1, contract.FrameHeartbeatAck, rid, nil)
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
func writeState(t *testing.T, root, id, url string, caPEM []byte) {
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
}

func observe(d *deps) *events {
	e := &events{ch: make(chan event, 256)}
	d.observe = func(ev event) { e.ch <- ev }
	return e
}

// await returns the next event of kind, skipping others.
func (e *events) await(t *testing.T, kind eventKind) event {
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

func startRun(t *testing.T, d *deps, root string, logger *slog.Logger) *running {
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
func (r *running) result(t *testing.T) error {
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
func readLine(t *testing.T, r io.Reader) string {
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
