package plane

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	pclient "github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// testWait bounds how long a test waits for an event (real time); it never
// drives product timing, which uses the fake clock.
const testWait = 20 * time.Second

// recConn records the deadlines set on an accepted connection and can
// block its writes, to prove cleared HTTP deadlines and bounded writes.
type recConn struct {
	net.Conn
	mu       sync.Mutex
	readDL   []time.Time
	writeDL  []time.Time
	blocking atomic.Bool
	closed   chan struct{}
	once     sync.Once
}

func (c *recConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDL, c.writeDL = append(c.readDL, t), append(c.writeDL, t)
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *recConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDL = append(c.readDL, t)
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

func (c *recConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDL = append(c.writeDL, t)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *recConn) Write(b []byte) (int, error) {
	if c.blocking.Load() {
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(b)
}

func (c *recConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// lastDeadlines returns the most recent read and write deadlines.
func (c *recConn) lastDeadlines() (time.Time, time.Time, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var r, w time.Time
	if len(c.readDL) > 0 {
		r = c.readDL[len(c.readDL)-1]
	}
	if len(c.writeDL) > 0 {
		w = c.writeDL[len(c.writeDL)-1]
	}
	return r, w, len(c.readDL), len(c.writeDL)
}

type recListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*recConn
}

func (l *recListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	rc := &recConn{Conn: c, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, rc)
	l.mu.Unlock()
	return rc, nil
}

func (l *recListener) all() []*recConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*recConn(nil), l.conns...)
}

// nodePlane is a served plane with a fake node clock and known nodes.
type nodePlane struct {
	*served
	d      *deps
	root   string
	clk    *testkit.FakeClock
	lis    *recListener
	logs   *logBuffer
	url    string
	trust  pclient.Trust
	cl     *pclient.Client
	events chan string
}

// startNodePlane initializes a plane with SAN 127.0.0.1, writes node
// records for ids, and serves it with a fake node clock.
func startNodePlane(t *testing.T, ids ...string) *nodePlane {
	t.Helper()
	return startNodePlaneWith(t, testDeps(t), ids...)
}

// startNodePlaneWith is startNodePlane with prepared deps.
func startNodePlaneWith(t *testing.T, d *deps, ids ...string) *nodePlane {
	t.Helper()
	root := freshRoot(t)
	for _, id := range ids {
		writeNodeRecord(t, root, id+".json", encodeNodeRecord(id, t0), 0o600)
	}
	np := &nodePlane{d: d, root: root, clk: testkit.NewFakeClock(t0), lis: &recListener{}, logs: &logBuffer{}, events: make(chan string, 64)}
	d.nodeClock = np.clk
	d.streamEvents = func(e string) {
		select {
		case np.events <- e:
		default:
		}
	}
	d.listen = func(n, a string) (net.Listener, error) {
		ln, err := net.Listen(n, a)
		np.lis.Listener = ln
		return np.lis, err
	}
	np.served = serveBG(t, d, RunOptions{StateDir: root, Logger: np.logs.logger()})
	ca, err := os.ReadFile(layout{root: root}.path(caCertName))
	if err != nil {
		t.Fatal(err)
	}
	np.url = "https://" + np.addr.String()
	np.trust = pclient.Trust{CAPEM: ca}
	np.cl, err = pclient.New(np.url, np.trust)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(np.cl.Close)
	return np
}

// awaitEvent waits for a stream event with the given prefix.
func (np *nodePlane) awaitEvent(t *testing.T, want string) {
	t.Helper()
	timeout := time.After(testWait)
	for {
		select {
		case e := <-np.events:
			if e == want {
				return
			}
		case <-timeout:
			// The event and the deadline may be ready together: check the
			// events already delivered before failing.
			for {
				select {
				case e := <-np.events:
					if e == want {
						return
					}
					continue
				default:
				}
				t.Fatalf("no stream event %q", want)
			}
		}
	}
}

// peer is a test-driven node stream speaking raw wire messages.
type peer struct {
	t *testing.T
	c *websocket.Conn
}

func (np *nodePlane) dial(t *testing.T) *peer {
	t.Helper()
	c, err := np.cl.DialNodeStream(bg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &peer{t: t, c: c}
}

func (p *peer) sendRaw(b []byte) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(bg, testWait)
	defer cancel()
	if err := p.c.Write(ctx, websocket.MessageText, b); err != nil {
		p.t.Fatalf("send: %v", err)
	}
}

func (p *peer) send(version int, typ, rid string, body any) {
	p.t.Helper()
	b, err := contract.EncodeFrame(version, typ, rid, body)
	if err != nil {
		p.t.Fatal(err)
	}
	p.sendRaw(b)
}

func (p *peer) hello(id string) {
	p.send(contract.ProtocolVersion, contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "test-1"})
}

// recv reads one plane message (bounded wait) and decodes its envelope.
func (p *peer) recv() contract.NodeFrame {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(bg, testWait)
	defer cancel()
	typ, b, err := p.c.Read(ctx)
	if err != nil {
		p.t.Fatalf("recv: %v", err)
	}
	if typ != websocket.MessageText {
		p.t.Fatalf("binary message from the plane")
	}
	f, err := contract.DecodeFrame(b, contract.FromPlane)
	if err != nil {
		p.t.Fatalf("plane message %s: %v", b, err)
	}
	return f
}

func (p *peer) expect(typ, rid string) contract.NodeFrame {
	p.t.Helper()
	f := p.recv()
	if f.Type != typ || f.RequestID != rid {
		p.t.Fatalf("got %s %s (%s), want %s %s", f.Type, f.RequestID, f.Body, typ, rid)
	}
	return f
}

// expectError reads an error message and returns its contract error.
func (p *peer) expectError(rid string, code contract.Code) *contract.Error {
	p.t.Helper()
	f := p.expect(contract.FrameError, rid)
	e, err := contract.ParseErrorBody(f.Body)
	if err != nil || e.Code != code {
		p.t.Fatalf("error body %s: %v, want %s", f.Body, err, code)
	}
	return e
}

// closed waits for the socket to end and returns its close status (-1
// without a close frame).
func (p *peer) closed() websocket.StatusCode {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(bg, testWait)
	defer cancel()
	for {
		_, _, err := p.c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				p.t.Fatal("the plane did not close the stream")
			}
			return websocket.CloseStatus(err)
		}
	}
}

// closeAsync reads until the socket ends and delivers its close status.
func (p *peer) closeAsync() chan websocket.StatusCode {
	c := make(chan websocket.StatusCode, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, testWait)
		defer cancel()
		for {
			if _, _, err := p.c.Read(ctx); err != nil {
				c <- websocket.CloseStatus(err)
				return
			}
		}
	}()
	return c
}

// released checks that every handler was joined: the port and the state
// lock are free.
func (np *nodePlane) released(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", np.addr.String())
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	ln.Close()
	lk, err := layout{root: np.root}.acquire()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	lk.release()
}

// connect completes hello and n heartbeats (b1..bn).
func (p *peer) connect(id string, n int) {
	p.t.Helper()
	p.hello(id)
	p.expect(contract.FrameHelloOK, "h1")
	for i := 1; i <= n; i++ {
		p.heartbeat(i)
	}
}

func (p *peer) heartbeat(i int) {
	p.t.Helper()
	rid := "b" + strconv.Itoa(i)
	p.send(contract.ProtocolVersion, contract.FrameHeartbeat, rid, contract.HeartbeatBody{})
	f := p.expect(contract.FrameHeartbeatAck, rid)
	if err := contract.DecodeAck(f.Body); err != nil {
		p.t.Fatal(err)
	}
}

func (np *nodePlane) show(t *testing.T, id string) contract.Node {
	t.Helper()
	n, err := np.cl.ShowNode(bg, id)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	return n
}

// TestNodeLeaseContract is delegated from tests/function (TestNodeLease).
// It serves the production TLS and node handlers with a fake node clock
// injected through deps, drives them with the exported client and wire
// messages, and observes liveness over HTTP at the exact deadline. Do not
// rename or skip its subtests.
func TestNodeLeaseContract(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		np := startNodePlane(t)
		if _, err := np.cl.EnrollNode(bg, idA, "test-1"); err != nil {
			t.Fatal(err)
		}
		if n := np.show(t, idA); n.Liveness != contract.LivenessOffline || n.LastSeen != nil {
			t.Fatalf("enrolled = %+v", n)
		}
		p := np.dial(t)
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		if n := np.show(t, idA); n.Liveness != contract.LivenessOffline {
			t.Fatalf("hello made the node online: %+v", n)
		}
		np.clk.Advance(2 * time.Second)
		hbAt := np.clk.Now()
		p.heartbeat(1)
		n := np.show(t, idA)
		if n.Liveness != contract.LivenessOnline || !n.LastSeen.Equal(hbAt) || *n.ProtocolVersion != 1 || *n.SoftwareVersion != "test-1" {
			t.Fatalf("after heartbeat = %+v", n)
		}
		// A closed socket keeps the lease until its deadline.
		p.c.Close(websocket.StatusNormalClosure, "")
		np.awaitEvent(t, "detached "+idA)
		np.clk.Advance(leaseDuration - time.Nanosecond)
		if n := np.show(t, idA); n.Liveness != contract.LivenessOnline {
			t.Fatalf("offline before the deadline after close: %+v", n)
		}
		// Exactly at the deadline HTTP reports offline, whatever the sweep.
		np.clk.Advance(time.Nanosecond)
		n = np.show(t, idA)
		if n.Liveness != contract.LivenessOffline || !n.LastSeen.Equal(hbAt) || *n.SoftwareVersion != "test-1" {
			t.Fatalf("at the deadline = %+v", n)
		}
		list, err := np.cl.ListNodes(bg)
		if err != nil || len(list) != 1 || list[0].Liveness != contract.LivenessOffline {
			t.Fatalf("list = %+v %v", list, err)
		}
	})
	t.Run("return", func(t *testing.T) {
		np := startNodePlane(t)
		if _, err := np.cl.EnrollNode(bg, idA, "test-1"); err != nil {
			t.Fatal(err)
		}
		p := np.dial(t)
		p.connect(idA, 1)
		// Advance only after b1's ack write returned, so the advance
		// cannot fire that write's own 5 s bound.
		np.awaitEvent(t, "ack "+idA+" b1")
		first := *np.show(t, idA).LastSeen
		// The stream stays open but stops heartbeating: the lease expires
		// and the plane evicts and closes it.
		np.clk.Advance(leaseDuration)
		if n := np.show(t, idA); n.Liveness != contract.LivenessOffline {
			t.Fatalf("expired = %+v", n)
		}
		if st := p.closed(); st != websocket.StatusPolicyViolation {
			t.Fatalf("evicted stream closed with %v", st)
		}
		// A new session and heartbeat restore online, same identity.
		np.clk.Advance(time.Second)
		p2 := np.dial(t)
		p2.connect(idA, 1)
		n := np.show(t, idA)
		if n.ID != idA || n.Liveness != contract.LivenessOnline || !n.LastSeen.After(first) {
			t.Fatalf("returned = %+v", n)
		}
	})
}

// TestNodeAPI is UT-NodeCLI/API's plane part: roster routes, versions,
// methods, queries, errors and no request echo.
func TestNodeAPI(t *testing.T) {
	np := startNodePlane(t)
	list, err := np.cl.ListNodes(bg)
	if err != nil || len(list) != 0 {
		t.Fatalf("empty roster = %v %v", list, err)
	}
	for _, id := range []string{idC, idA, idB} {
		n, err := np.cl.EnrollNode(bg, id, "v1")
		if err != nil || n.ID != id || n.Liveness != contract.LivenessOffline {
			t.Fatalf("enroll %s = %+v %v", id, n, err)
		}
	}
	if !bytes.Contains([]byte(np.logs.String()), []byte(`"msg":"node enrolled"`)) {
		t.Fatalf("enrollment not logged: %s", np.logs.String())
	}
	list, err = np.cl.ListNodes(bg)
	if err != nil || len(list) != 3 || list[0].ID != idA || list[1].ID != idB || list[2].ID != idC {
		t.Fatalf("sorted roster = %+v %v", list, err)
	}
	if _, err := np.cl.ShowNode(bg, "n_ffffffffffffffffffffffffffffffff"); !contract.IsCode(err, contract.CodeNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := np.cl.ShowNode(bg, "bad"); !contract.IsCode(err, contract.CodeInvalidArgument) {
		t.Fatalf("invalid = %v", err)
	}
	httpc := keepAliveClient(t, readCA(t, np.root))
	do := func(method, path, version string, extra map[string]string, body string) (int, string, string) {
		req, _ := newRequest(method, np.url+path, body)
		if version != "" {
			req.Header.Set(contract.ProtocolHeader, version)
		}
		for k, v := range extra {
			req.Header.Add(k, v)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(bytes.Buffer)
		b.ReadFrom(resp.Body)
		return resp.StatusCode, resp.Header.Get(contract.ProtocolHeader), b.String()
	}
	for _, c := range []struct {
		method, path, version string
		extra                 map[string]string
		body                  string
		status                int
		code                  contract.Code
	}{
		{"GET", contract.PathNodes, "", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes, "x", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes, "1", map[string]string{contract.ProtocolHeader: "1"}, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes, "2", nil, "", 409, contract.CodeProtocolMismatch},
		{"GET", contract.PathNodes + "?all=SECRET-QUERY", "1", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes + "?", "1", nil, "", 400, contract.CodeInvalidArgument},
		{"POST", contract.PathNodes, "1", nil, "SECRET-BODY", 400, contract.CodeInvalidArgument},
		{"DELETE", contract.PathNodes + "/" + idA, "1", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes + "/SECRET-ID", "1", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes + "/", "1", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes + "/" + idA + "/x", "1", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodes + "/n_ffffffffffffffffffffffffffffffff", "1", nil, "", 404, contract.CodeNotFound},
		{"GET", contract.PathEnroll, "1", nil, "", 400, contract.CodeInvalidArgument},
		{"POST", contract.PathEnroll, "1", nil, "SECRET-BODY", 400, contract.CodeInvalidArgument},
		{"POST", contract.PathEnroll, "1", nil, `{"node_id":"` + idA + `","software_version":"v","x":"SECRET-BODY"}`, 400, contract.CodeInvalidArgument},
		{"POST", contract.PathEnroll, "2", nil, `{}`, 409, contract.CodeProtocolMismatch},
		{"POST", contract.PathCA, "", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", "/api/v1/nodez", "1", nil, "", 404, contract.CodeNotFound},
		{"GET", contract.PathNodeStream, "", map[string]string{"Origin": "https://evil.example"}, "", 400, contract.CodeInvalidArgument},
		{"POST", contract.PathNodeStream, "", nil, "", 400, contract.CodeInvalidArgument},
		{"GET", contract.PathNodeStream + "?x=1", "", nil, "", 400, contract.CodeInvalidArgument},
	} {
		status, hdr, body := do(c.method, c.path, c.version, c.extra, c.body)
		e, err := contract.ParseErrorBody(bytes.TrimSpace([]byte(body)))
		if status != c.status || err != nil || e.Code != c.code || bytes.Contains([]byte(body), []byte("SECRET")) {
			t.Fatalf("%s %s v=%q = %d %q (%v)", c.method, c.path, c.version, status, body, err)
		}
		if c.path != contract.PathCA && c.path != "/api/v1/nodez" && hdr != "1" {
			t.Fatalf("%s %s: response protocol header %q", c.method, c.path, hdr)
		}
		if c.code == contract.CodeProtocolMismatch {
			if e.Message != "protocol version mismatch: local=1 remote=2" {
				t.Fatalf("mismatch message %q", e.Message)
			}
			if l, _ := e.DetailInt("local_version"); l != 1 {
				t.Fatal("details")
			}
		}
	}
	if !bytes.Contains([]byte(np.logs.String()), []byte(`"local_version":1,"remote_version":2`)) {
		t.Fatalf("mismatch not logged: %s", np.logs.String())
	}
	// An oversized enrollment body is refused by the bounded reader (checked
	// on the handler: over the network net/http lingers on such a
	// connection before closing it).
	svc := newNodeService(np.srvReg(t), np.clk, discardLogger(), nil, time.Second)
	req, _ := newRequest("POST", contract.PathEnroll, `{"node_id":"`+idA+`","software_version":"`+strings.Repeat("v", 5000)+`"}`)
	req.Header.Set(contract.ProtocolHeader, "1")
	rec := httptest.NewRecorder()
	svc.handleNodes(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "larger than 4096 bytes") {
		t.Fatalf("oversized enrollment = %d %s", rec.Code, rec.Body.String())
	}
	// The CA route serves the public CA only; health is unchanged.
	status, _, body := do("GET", contract.PathCA, "", nil, "")
	ca, _ := os.ReadFile(layout{root: np.root}.path(caCertName))
	if status != 200 || body != string(ca) || bytes.Contains([]byte(body), []byte("PRIVATE")) {
		t.Fatalf("ca = %d %q", status, body)
	}
	status, _, body = do("GET", HealthPath, "", nil, "")
	if status != 200 || body != healthBody {
		t.Fatalf("health = %d %q", status, body)
	}
	// Enrollment status codes: 201 new, 200 existing.
	status, _, body = do("POST", contract.PathEnroll, "1", nil, `{"node_id":"n_dddddddddddddddddddddddddddddddd","software_version":"v"}`)
	if status != 201 {
		t.Fatalf("new enroll = %d %s", status, body)
	}
	status, _, _ = do("POST", contract.PathEnroll, "1", nil, `{"node_id":"n_dddddddddddddddddddddddddddddddd","software_version":"v"}`)
	if status != 200 {
		t.Fatalf("repeat enroll = %d", status)
	}
	// The response JSON is exactly the envelope with a final LF.
	status, _, body = do("GET", contract.PathNodes+"/"+idA, "1", nil, "")
	if status != 200 || body != `{"version":1,"node":{"id":"`+idA+`","liveness":"offline","last_seen":null,"protocol_version":null,"software_version":null,"roles":[]}}`+"\n" {
		t.Fatalf("show body %q", body)
	}
}

// TestNodeStreamProtocol is UT-Stream's handshake part: hello timeout,
// version mismatch before any mutation, first-message and sequence rules,
// unknown and duplicate nodes, binary and oversized messages.
func TestNodeStreamProtocol(t *testing.T) {
	np := startNodePlane(t, idA, idB)
	t.Run("hello-timeout", func(t *testing.T) {
		p := np.dial(t)
		if err := np.clk.AwaitWaiter(testWait, testkit.HasTimer(streamStepTimeout)); err != nil {
			t.Fatal(err)
		}
		np.clk.Advance(streamStepTimeout)
		// The deadline cancels the pending read, which closes the socket
		// at once: no close frame, and nothing was attached.
		if st := p.closed(); st != -1 {
			t.Fatalf("hello timeout close = %v, want a closed socket without a close frame", st)
		}
		np.awaitEvent(t, "hello timeout")
		if !strings.Contains(np.logs.String(), `"reason":"hello timeout"`) {
			t.Fatalf("hello timeout not logged: %s", np.logs.String())
		}
	})
	t.Run("hello-at-deadline", func(t *testing.T) {
		// W1 regression: the hello deadline fires at the moment the hello
		// has been read (the hook runs after Read returned a frame and
		// waits until the deadline canceled the read's context). A hello
		// that Read returned is always processed: never closed as a
		// timeout by anything that runs after the read.
		d := testDeps(t)
		fired := make(chan struct{}, 1)
		var clk *testkit.FakeClock
		d.streamHelloRead = func(ctx context.Context) {
			clk.Advance(streamStepTimeout)
			<-ctx.Done()
			fired <- struct{}{}
		}
		np := startNodePlaneWith(t, d, idA)
		clk = np.clk
		p := np.dial(t)
		p.hello(idA)
		p.expect(contract.FrameHelloOK, "h1")
		select {
		case <-fired:
		default:
			t.Fatal("the deadline did not fire at the hello read")
		}
		p.heartbeat(1)
		if n := np.show(t, idA); n.Liveness != contract.LivenessOnline {
			t.Fatalf("after a hello read at the deadline = %+v", n)
		}
		if strings.Contains(np.logs.String(), "hello timeout") {
			t.Fatalf("a read hello was treated as a timeout: %s", np.logs.String())
		}
	})
	t.Run("version", func(t *testing.T) {
		p := np.dial(t)
		p.send(2, contract.FrameHello, "h9", map[string]any{"node_id": idA, "future": true})
		f := p.recv()
		e, err := contract.ParseErrorBody(f.Body)
		if f.Version != 1 || f.Type != contract.FrameError || f.RequestID != "h9" || err != nil || e.Code != contract.CodeProtocolMismatch ||
			e.Message != "protocol version mismatch: local=1 remote=2" {
			t.Fatalf("mismatch = %+v %v %v", f, e, err)
		}
		if r, _ := e.DetailInt("remote_version"); r != 2 {
			t.Fatal("details")
		}
		if st := p.closed(); st != websocket.StatusPolicyViolation {
			t.Fatalf("close = %v", st)
		}
		if n := np.show(t, idA); n.Liveness != contract.LivenessOffline || n.SoftwareVersion != nil {
			t.Fatalf("mismatch mutated the registry: %+v", n)
		}
		if !strings.Contains(np.logs.String(), `"msg":"protocol version mismatch","component":"plane","local_version":1,"remote_version":2,"path":"/api/v1/node-stream"`) {
			t.Fatalf("mismatch not logged with both versions: %s", np.logs.String())
		}
		// Nothing was attached: a correct hello succeeds at once.
		p2 := np.dial(t)
		p2.hello(idA)
		p2.expect(contract.FrameHelloOK, "h1")
		p2.c.Close(websocket.StatusNormalClosure, "")
		np.awaitEvent(t, "detached "+idA)
	})
	for _, c := range []struct {
		name  string
		first []byte
		rid   string
		code  contract.Code
	}{
		{"heartbeat-first", mustFrame(t, contract.FrameHeartbeat, "b1", contract.HeartbeatBody{}), "b1", contract.CodeInvalidArgument},
		{"unknown-node", mustFrame(t, contract.FrameHello, "h1", contract.HelloBody{NodeID: idC, SoftwareVersion: "v"}), "h1", contract.CodeNotFound},
		{"bad-hello", mustFrame(t, contract.FrameHello, "h1", map[string]any{"node_id": idA}), "h1", contract.CodeInvalidArgument},
		{"malformed", []byte(`{"version":1,`), invalidRequestID, contract.CodeInvalidArgument},
		{"wrong-direction", mustFrame(t, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: 5000, LeaseMS: 15000}), "h1", contract.CodeInvalidArgument},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := np.dial(t)
			p.sendRaw(c.first)
			p.expectError(c.rid, c.code)
			if st := p.closed(); st != websocket.StatusPolicyViolation {
				t.Fatalf("close = %v", st)
			}
		})
	}
	t.Run("sequence", func(t *testing.T) {
		for _, c := range []struct {
			name string
			send func(p *peer)
			rid  string
		}{
			{"skip", func(p *peer) { p.send(1, contract.FrameHeartbeat, "b2", contract.HeartbeatBody{}) }, "b2"},
			{"duplicate", func(p *peer) {
				p.heartbeat(1)
				p.send(1, contract.FrameHeartbeat, "b1", contract.HeartbeatBody{})
			}, "b1"},
			{"hello-again", func(p *peer) { p.hello(idA) }, "h1"},
			{"roles", func(p *peer) {
				p.sendRaw([]byte(`{"version":1,"type":"heartbeat","request_id":"b1","body":{"roles":[{"role_id":"r","inflight":0,"concurrency":1,"can_accept":true}]}}`))
			}, "b1"},
		} {
			p := np.dial(t)
			p.hello(idB)
			p.expect(contract.FrameHelloOK, "h1")
			c.send(p)
			p.expectError(c.rid, contract.CodeInvalidArgument)
			if st := p.closed(); st != websocket.StatusPolicyViolation {
				t.Fatalf("%s close = %v", c.name, st)
			}
			np.awaitEvent(t, "detached "+idB)
		}
	})
	t.Run("peer-error", func(t *testing.T) {
		p := np.dial(t)
		p.connect(idB, 1)
		p.send(1, contract.FrameError, "b2", contract.New(contract.CodeInternal, "sidecar trouble"))
		if st := p.closed(); st != websocket.StatusPolicyViolation {
			t.Fatalf("close = %v", st)
		}
		np.awaitEvent(t, "detached "+idB)
	})
	t.Run("duplicate", func(t *testing.T) {
		p := np.dial(t)
		p.connect(idA, 1)
		p2 := np.dial(t)
		p2.hello(idA)
		p2.expectError("h1", contract.CodeConflict)
		if st := p2.closed(); st != websocket.StatusPolicyViolation {
			t.Fatalf("duplicate close = %v", st)
		}
		// The first stream is unaffected.
		p.heartbeat(2)
		p.c.Close(websocket.StatusNormalClosure, "")
		np.awaitEvent(t, "detached "+idA)
		// Once it closed, a replacement is admitted immediately.
		p3 := np.dial(t)
		p3.connect(idA, 1)
		p3.c.Close(websocket.StatusNormalClosure, "")
		np.awaitEvent(t, "detached "+idA)
	})
	t.Run("binary", func(t *testing.T) {
		p := np.dial(t)
		ctx, cancel := context.WithTimeout(bg, testWait)
		defer cancel()
		p.c.Write(ctx, websocket.MessageBinary, []byte("x"))
		if st := p.closed(); st != websocket.StatusUnsupportedData {
			t.Fatalf("binary close = %v", st)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		np := startNodePlane(t) // its own logs: no other subtest's timeout
		p := np.dial(t)
		ctx, cancel := context.WithTimeout(bg, testWait)
		defer cancel()
		p.c.Write(ctx, websocket.MessageText, bytes.Repeat([]byte(" "), contract.MaxFrameBytes+1))
		if st := p.closed(); st != websocket.StatusMessageTooBig {
			t.Fatalf("oversized close = %v", st)
		}
		// A 1009 close is not a hello timeout: the node clock never moved.
		np.awaitEvent(t, "hello failed")
		if strings.Contains(np.logs.String(), "hello timeout") {
			t.Fatalf("an oversized hello was reported as a timeout: %s", np.logs.String())
		}
	})
	t.Run("peer-drop", func(t *testing.T) {
		// A peer that drops before hello, with the node clock never
		// advanced, is not a hello timeout.
		np := startNodePlane(t)
		p := np.dial(t)
		p.c.CloseNow()
		np.awaitEvent(t, "hello failed")
		if strings.Contains(np.logs.String(), "hello timeout") {
			t.Fatalf("a dropped peer was reported as a hello timeout: %s", np.logs.String())
		}
	})
	t.Run("first-heartbeat-timeout", func(t *testing.T) {
		np := startNodePlane(t, idB)
		p := np.dial(t)
		p.hello(idB)
		p.expect(contract.FrameHelloOK, "h1")
		// Advance only after the hello_ok write returned (the attached
		// event), so the advance cannot fire that write's own 5 s bound.
		np.awaitEvent(t, "attached "+idB)
		np.clk.Advance(firstHeartbeatWindow)
		if st := p.closed(); st != websocket.StatusPolicyViolation {
			t.Fatalf("first heartbeat timeout close = %v", st)
		}
		if n := np.show(t, idB); n.Liveness != contract.LivenessOffline || n.LastSeen != nil {
			t.Fatalf("no heartbeat, yet %+v", n)
		}
	})
}

func mustFrame(t *testing.T, typ, rid string, body any) []byte {
	t.Helper()
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, rid, body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestNodeStreamLifecycle is UT-Stream's connection part: cleared HTTP
// deadlines keep a heartbeating stream alive past the HTTP bounds, a
// blocked peer is closed after the write bound, and shutdown joins every
// upgraded handler (with a forced close for a blocked one).
func TestNodeStreamLifecycle(t *testing.T) {
	t.Run("deadlines", func(t *testing.T) {
		np := startNodePlane(t, idA)
		p := np.dial(t)
		p.connect(idA, 1)
		conns := np.lis.all()
		if len(conns) != 1 {
			t.Fatalf("%d connections", len(conns))
		}
		r, w, nr, nw := conns[0].lastDeadlines()
		if nr == 0 || nw == 0 || !r.IsZero() || !w.IsZero() {
			t.Fatalf("after upgrade the last deadlines are %v/%v (%d/%d sets); want zero", r, w, nr, nw)
		}
		// Well past the ten-second HTTP bounds on the stream clock, the
		// heartbeating connection stays attached and online.
		for i := 2; i <= 6; i++ {
			// Advance only after the previous ack's write returned, so the
			// advance cannot fire that write's own 5 s bound.
			np.awaitEvent(t, "ack "+idA+" b"+strconv.Itoa(i-1))
			np.clk.Advance(heartbeatInterval)
			p.heartbeat(i)
		}
		if n := np.show(t, idA); n.Liveness != contract.LivenessOnline {
			t.Fatalf("after 25 s = %+v", n)
		}
		if _, _, nr2, nw2 := conns[0].lastDeadlines(); nr2 != nr || nw2 != nw {
			t.Fatal("deadlines were set again after the upgrade")
		}
	})
	t.Run("blocked-write", func(t *testing.T) {
		np := startNodePlane(t, idA)
		p := np.dial(t)
		p.connect(idA, 1)
		conn := np.lis.all()[0]
		conn.blocking.Store(true)
		p.send(1, contract.FrameHeartbeat, "b2", contract.HeartbeatBody{})
		// Only once b2's ack write starts is the one 5 s timer its write
		// bound (b1's ack write, and its timer, ended before b2 was read).
		np.awaitEvent(t, "acking "+idA+" b2")
		if err := np.clk.AwaitWaiter(testWait, testkit.HasTimer(streamStepTimeout)); err != nil {
			t.Fatal(err)
		}
		np.clk.Advance(streamStepTimeout)
		np.awaitEvent(t, "detached "+idA)
		p.closed()
		// The lease stays authoritative after the forced close.
		if n := np.show(t, idA); n.Liveness != contract.LivenessOnline {
			t.Fatalf("after blocked peer = %+v", n)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		np := startNodePlane(t, idA)
		p := np.dial(t)
		p.connect(idA, 1)
		idle := np.dial(t) // upgraded, no hello yet
		// Real sidecars always read, so they answer the close frame.
		pc, ic := p.closeAsync(), idle.closeAsync()
		start := time.Now()
		if err := np.stop(t); !errors.Is(err, context.Canceled) {
			t.Fatalf("run = %v", err)
		}
		if el := time.Since(start); el > shutdownTimeout {
			t.Fatalf("shutdown took %v", el)
		}
		for _, c := range []chan websocket.StatusCode{pc, ic} {
			if st := <-c; st != websocket.StatusGoingAway {
				t.Fatalf("clean peer close = %v", st)
			}
		}
		np.released(t)
	})
	t.Run("shutdown-blocked", func(t *testing.T) {
		d := testDeps(t)
		d.streamCloseGrace = 50 * time.Millisecond
		np := startNodePlaneWith(t, d, idA)
		blocked := np.dial(t)
		blocked.connect(idA, 1)
		np.lis.all()[0].blocking.Store(true)
		start := time.Now()
		if err := np.stop(t); !errors.Is(err, context.Canceled) {
			t.Fatalf("run = %v", err)
		}
		if el := time.Since(start); el > shutdownTimeout {
			t.Fatalf("forced shutdown took %v", el)
		}
		blocked.closed()
		np.released(t)
	})
	t.Run("admission", func(t *testing.T) {
		r, clk := newRegistry(t)
		svc := newNodeService(r, clk, discardLogger(), nil, time.Second)
		svc.shutdown(time.Now().Add(time.Second))
		rec := &respRecorder{h: map[string][]string{}}
		svc.handleStream(rec, newGet(t, contract.PathNodeStream))
		if rec.code != 503 || rec.h.Get(contract.ProtocolHeader) != "1" {
			t.Fatalf("stream after shutdown = %d, protocol header %q", rec.code, rec.h.Get(contract.ProtocolHeader))
		}
		if svc.admit(&nodeStream{}) {
			t.Fatal("admitted after shutdown")
		}
	})
}

// srvReg loads a separate registry on the plane's root (read-only use).
func (np *nodePlane) srvReg(t *testing.T) *nodeRegistry {
	t.Helper()
	r, err := loadNodeRegistry(layout{root: np.root}, np.clk)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newRequest(method, u, body string) (*http.Request, error) {
	return http.NewRequest(method, u, strings.NewReader(body))
}

func newGet(t *testing.T, path string) *http.Request {
	t.Helper()
	return &http.Request{Method: http.MethodGet, URL: &url.URL{Path: path}, Header: http.Header{}}
}

// keepAliveClient trusts ca and reuses connections (one handshake for many
// requests).
func keepAliveClient(t *testing.T, ca *x509.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, Proxy: nil}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: testWait}
}
