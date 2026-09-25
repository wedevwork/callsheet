// Package testkit provides reusable test fixtures: a generated fixture CA, an
// in-process TLS plane/sidecar transport harness, repository discovery and
// helper-binary builds. Production packages must not import it.
package testkit

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

const (
	// MaxFrameBytes bounds a single harness frame.
	MaxFrameBytes = 64 << 10
	// HandshakeTimeout bounds the hello exchange and readiness.
	HandshakeTimeout = 5 * time.Second
	// JoinTimeout bounds Close's goroutine join.
	JoinTimeout = 5 * time.Second
	// GitPrefix is where an optional spike handler is mounted.
	GitPrefix = "/test/git/"
	// NodeStreamPath is the WSS endpoint dialled by the sidecar fixture.
	NodeStreamPath = "/api/v1/node-stream"
)

// Frame is a test-only harness message. It does not mimic task dispatch.
type Frame struct {
	Version   int             `json:"version"`
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Body      json.RawMessage `json:"body"`
}

type harnessConfig struct {
	git      http.Handler
	gitCount int
	gitNil   bool
}

// Option configures NewHarness before the listener starts.
type Option func(*harnessConfig)

// WithGitHandler mounts h at /test/git/ with the full request path (no
// StripPrefix). Nil or repeated handlers are setup errors.
func WithGitHandler(h http.Handler) Option {
	return func(c *harnessConfig) {
		c.gitCount++
		if h == nil {
			c.gitNil = true
			return
		}
		c.git = h
	}
}

func (c *harnessConfig) validate() error {
	if c.gitNil {
		return errors.New("testkit: WithGitHandler(nil)")
	}
	if c.gitCount > 1 {
		return errors.New("testkit: multiple git handlers supplied")
	}
	return nil
}

// Harness is an isolated in-process TLS server with a verified client.
type Harness struct {
	Root   string
	URL    string
	CAPEM  []byte
	Client *http.Client
	Errs   <-chan error
	Done   <-chan struct{}

	ca       *FixtureCA
	ctx      context.Context
	cancel   context.CancelFunc
	server   *http.Server
	listener net.Listener
	wg       sync.WaitGroup
	errs     chan error
	done     chan struct{}
	logBuf   syncBuffer

	mu        sync.Mutex
	conns     map[*websocket.Conn]struct{}
	closed    bool
	closeOnce sync.Once
	closeErr  error
	joinLimit time.Duration
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// NewHarness starts a harness owned by t. Close is registered with t.Cleanup
// immediately; setup errors fail t after resources are released.
func NewHarness(t testing.TB, opts ...Option) *Harness {
	t.Helper()
	h, err := newHarness(t, opts...)
	if err != nil {
		t.Fatalf("testkit: harness setup: %v", err)
	}
	return h
}

func newHarness(t testing.TB, opts ...Option) (*Harness, error) {
	h := &Harness{
		errs:      make(chan error, 64),
		done:      make(chan struct{}),
		conns:     map[*websocket.Conn]struct{}{},
		joinLimit: JoinTimeout,
	}
	h.Errs, h.Done = h.errs, h.done
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("testkit: harness close: %v", err)
		}
	})
	fail := func(err error) (*Harness, error) {
		h.Close()
		return nil, err
	}
	var cfg harnessConfig
	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return fail(err)
	}
	h.Root = t.TempDir()
	if err := os.Chmod(h.Root, 0o700); err != nil {
		return fail(err)
	}
	ca, err := NewFixtureCA()
	if err != nil {
		return fail(err)
	}
	h.ca = ca
	h.CAPEM = ca.CertPEM
	leaf, err := ca.ServerCertificate()
	if err != nil {
		return fail(err)
	}
	h.Client = ca.Client()

	mux := http.NewServeMux()
	mux.HandleFunc("/test/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc(NodeStreamPath, h.serveNodeStream)
	if cfg.git != nil {
		mux.Handle(GitPrefix, cfg.git)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	tlsConf := &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	h.listener = tls.NewListener(ln, tlsConf)
	h.server = &http.Server{
		Handler:           h.gate(mux),
		ErrorLog:          log.New(&h.logBuf, "harness: ", 0),
		ReadHeaderTimeout: HandshakeTimeout,
		BaseContext:       func(net.Listener) context.Context { return h.ctx },
	}
	h.URL = "https://" + ln.Addr().String()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		if err := h.server.Serve(h.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.reportErr(fmt.Errorf("serve: %w", err))
		}
	}()
	return h, nil
}

// ServerLog returns captured server error-log output (e.g. TLS handshake
// failures from negative tests).
func (h *Harness) ServerLog() string { return h.logBuf.String() }

// CA returns the fixture CA that signed the server certificate.
func (h *Harness) CA() *FixtureCA { return h.ca }

// Addr returns the listener's host:port.
func (h *Harness) Addr() string { return strings.TrimPrefix(h.URL, "https://") }

func (h *Harness) reportErr(err error) {
	select {
	case h.errs <- err:
	default:
	}
}

func (h *Harness) track(c *websocket.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

func (h *Harness) untrack(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// gate counts every in-flight request as a fixture goroutine so Close can
// join them; requests arriving after Close are refused.
func (h *Harness) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			http.Error(w, "harness closed", http.StatusServiceUnavailable)
			return
		}
		h.wg.Add(1)
		h.mu.Unlock()
		defer h.wg.Done()
		next.ServeHTTP(w, r)
	})
}

// goTracked runs f as a joined fixture goroutine unless the harness closed.
func (h *Harness) goTracked(f func()) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		f()
	}()
	return true
}

// Close cancels the shared context, closes tracked websockets, the listener
// and idle client connections, and joins fixture goroutines within 5s. It is
// idempotent and returns the same result on every call.
func (h *Harness) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		conns := make([]*websocket.Conn, 0, len(h.conns))
		for c := range h.conns {
			conns = append(conns, c)
		}
		h.mu.Unlock()
		h.cancel()
		for _, c := range conns {
			c.CloseNow()
		}
		var errs []error
		if h.server != nil {
			if err := h.server.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if h.Client != nil {
			h.Client.CloseIdleConnections()
		}
		joined := make(chan struct{})
		go func() {
			h.wg.Wait()
			close(joined)
		}()
		select {
		case <-joined:
			close(h.errs)
			close(h.done)
		case <-time.After(h.joinLimit):
			errs = append(errs, fmt.Errorf("fixture goroutines did not join within %v", h.joinLimit))
		}
		h.closeErr = errors.Join(errs...)
	})
	return h.closeErr
}

func (h *Harness) serveNodeStream(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	if !h.track(c) {
		c.CloseNow()
		return
	}
	defer h.untrack(c)
	c.SetReadLimit(MaxFrameBytes)
	ctx := h.ctx

	hctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	hello, err := readFrame(hctx, c)
	cancel()
	if err != nil {
		h.reportPeerErr("hello", err)
		c.CloseNow()
		return
	}
	if hello.Type != "hello" || hello.Version != contract.ProtocolVersion {
		details := map[string]any{"local_version": contract.ProtocolVersion, "remote_version": hello.Version}
		msg := "protocol version mismatch"
		if hello.Type != "hello" {
			msg = "first frame must be hello"
		}
		body, _ := json.Marshal(&contract.Error{Code: contract.CodeProtocolMismatch, Message: msg, Details: details})
		wctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
		writeFrame(wctx, c, Frame{Version: contract.ProtocolVersion, Type: "error", RequestID: hello.RequestID, Body: body})
		cancel()
		c.Close(websocket.StatusPolicyViolation, "protocol mismatch")
		return
	}
	if err := writeFrame(ctx, c, Frame{Version: hello.Version, Type: "hello_ok", RequestID: hello.RequestID, Body: json.RawMessage(`{}`)}); err != nil {
		h.reportPeerErr("hello_ok", err)
		return
	}
	for {
		f, err := readFrame(ctx, c)
		if err != nil {
			h.reportPeerErr("read", err)
			c.CloseNow()
			return
		}
		reply := Frame{Version: contract.ProtocolVersion, RequestID: f.RequestID}
		if f.Type == "echo" {
			reply.Type, reply.Body = "echo_ok", f.Body
		} else {
			body, _ := json.Marshal(&contract.Error{Code: contract.CodeInvalidArgument, Message: "unsupported test frame type"})
			reply.Type, reply.Body = "error", body
		}
		if err := writeFrame(ctx, c, reply); err != nil {
			h.reportPeerErr("write", err)
			return
		}
	}
}

// reportPeerErr publishes unexpected server-side errors; clean closure and
// harness shutdown are not errors.
func (h *Harness) reportPeerErr(stage string, err error) {
	if h.ctx.Err() != nil {
		return
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	h.reportErr(fmt.Errorf("node-stream %s: %w", stage, err))
}

func readFrame(ctx context.Context, c *websocket.Conn) (Frame, error) {
	typ, b, err := c.Read(ctx)
	if err != nil {
		return Frame{}, err
	}
	if typ != websocket.MessageText {
		return Frame{}, errors.New("binary frames are not supported")
	}
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return Frame{}, fmt.Errorf("decode frame: %w", err)
	}
	return f, nil
}

func writeFrame(ctx context.Context, c *websocket.Conn, f Frame) error {
	if f.Body == nil {
		f.Body = json.RawMessage(`{}`)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > MaxFrameBytes {
		return fmt.Errorf("frame exceeds %d bytes", MaxFrameBytes)
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// SidecarConn is the outbound sidecar fixture peer.
type SidecarConn struct {
	h      *Harness
	conn   *websocket.Conn
	frames chan Frame
	readEr chan error
	done   chan struct{}
	once   sync.Once
}

// DialSidecar connects outbound to the node stream using the verified
// client, completes the hello for version and returns the tracked peer.
func (h *Harness) DialSidecar(ctx context.Context, version int) (*SidecarConn, error) {
	return h.dialSidecar(ctx, h.Client, version)
}

func (h *Harness) dialSidecar(ctx context.Context, client *http.Client, version int) (*SidecarConn, error) {
	hctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	wsURL := "wss://" + h.Addr() + NodeStreamPath
	c, _, err := websocket.Dial(hctx, wsURL, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, classifyDialErr(err)
	}
	c.SetReadLimit(MaxFrameBytes)
	if !h.track(c) {
		c.CloseNow()
		return nil, errors.New("testkit: harness closed")
	}
	fail := func(err error) (*SidecarConn, error) {
		c.CloseNow()
		h.untrack(c)
		return nil, err
	}
	if err := writeFrame(hctx, c, Frame{Version: version, Type: "hello", RequestID: "h1", Body: json.RawMessage(`{}`)}); err != nil {
		return fail(err)
	}
	reply, err := readFrame(hctx, c)
	if err != nil {
		return fail(err)
	}
	switch {
	case reply.Type == "error":
		return fail(decodeWireError(reply.Body))
	case reply.Type != "hello_ok" || reply.RequestID != "h1" || reply.Version != version:
		return fail(contract.New(contract.CodeProtocolMismatch, fmt.Sprintf("unexpected hello reply %q", reply.Type)))
	}
	s := &SidecarConn{h: h, conn: c, frames: make(chan Frame, 16), readEr: make(chan error, 1), done: make(chan struct{})}
	if !h.goTracked(s.readLoop) {
		return fail(errors.New("testkit: harness closed"))
	}
	return s, nil
}

func classifyDialErr(err error) error {
	var uerr x509.UnknownAuthorityError
	var herr x509.HostnameError
	var cerr *tls.CertificateVerificationError
	if errors.As(err, &uerr) || errors.As(err, &herr) || errors.As(err, &cerr) {
		return contract.Wrap(contract.CodeTrustFailed, "server certificate verification failed", err)
	}
	return contract.Wrap(contract.CodeUnavailable, "node stream dial failed", err)
}

func (s *SidecarConn) readLoop() {
	defer close(s.done)
	for {
		f, err := readFrame(s.h.ctx, s.conn)
		if err != nil {
			s.readEr <- err
			return
		}
		select {
		case s.frames <- f:
		case <-s.h.ctx.Done():
			return
		}
	}
}

// Echo sends a test-only echo frame and returns the correlated body.
func (s *SidecarConn) Echo(ctx context.Context, requestID string, body json.RawMessage) (json.RawMessage, error) {
	// A done context must win deterministically: the write can still succeed
	// with it (and the library then closes the conn), and once a reply or a
	// read error is ready the select below picks among ready cases at random.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := writeFrame(ctx, s.conn, Frame{Version: contract.ProtocolVersion, Type: "echo", RequestID: requestID, Body: body}); err != nil {
		return nil, err
	}
	select {
	case f := <-s.frames:
		if f.RequestID != requestID {
			return nil, fmt.Errorf("correlation mismatch: got request_id %q, want %q", f.RequestID, requestID)
		}
		if f.Type == "error" {
			return nil, decodeWireError(f.Body)
		}
		if f.Type != "echo_ok" {
			return nil, fmt.Errorf("unexpected reply type %q", f.Type)
		}
		return f.Body, nil
	case err := <-s.readEr:
		s.readEr <- err
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close closes the peer normally and joins its reader.
func (s *SidecarConn) Close() error {
	var err error
	s.once.Do(func() {
		err = s.conn.Close(websocket.StatusNormalClosure, "")
		if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
			err = nil
		}
		s.conn.CloseNow()
		<-s.done
		s.h.untrack(s.conn)
	})
	return err
}

func decodeWireError(body json.RawMessage) error {
	var w struct {
		Error struct {
			Code    contract.Code  `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &w); err != nil || w.Error.Code == "" {
		return contract.New(contract.CodeInternal, "malformed error frame")
	}
	return &contract.Error{Code: w.Error.Code, Message: w.Error.Message, Details: w.Error.Details}
}
