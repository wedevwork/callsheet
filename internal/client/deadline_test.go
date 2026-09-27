package client

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// exchangeBound bounds a real exchange made by lateTransport (FetchCA's
// pinned round trip), independently of the short operation deadline under
// test.
const exchangeBound = 10 * time.Second

// lateTransport hands back its response only after the request's context
// has ended: the response net/http delivers from Do when it races the
// operation deadline (the hung-plane CI failure). When exchange is set,
// that real round trip runs first under its own exchangeBound, not the
// request context. When cancelCaller is set, it runs once the response is
// ready, so the operation context ends by the caller's own cancellation.
type lateTransport struct {
	exchange     http.RoundTripper
	cancelCaller context.CancelFunc
	respond      func(*http.Request) *http.Response
	delivered    atomic.Bool
}

func (l *lateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if l.exchange != nil {
		ectx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), exchangeBound)
		defer cancel()
		resp, err := l.exchange.RoundTrip(req.WithContext(ectx))
		if err != nil {
			return nil, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if l.cancelCaller != nil {
		l.cancelCaller()
	}
	<-req.Context().Done()
	l.delivered.Store(true)
	return l.respond(req), nil
}

// lateBody is a response body (a ReadWriteCloser, as a 101 upgrade needs)
// that records its close.
type lateBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b *lateBody) Write(p []byte) (int, error) { return len(p), nil }
func (b *lateBody) Close() error                { b.closed.Store(true); return nil }

// implicit200 is the response net/http completes for a handler that
// returns without writing: 200, Content-Length 0, Date, and no protocol
// header.
var implicit200 = http.Header{"Content-Length": {"0"}, "Date": {"Sun, 27 Sep 2026 02:43:20 GMT"}}

func lateResponse(status int, h http.Header, body string, closed *atomic.Bool) func(*http.Request) *http.Response {
	return func(req *http.Request) *http.Response {
		hdr := h.Clone()
		if status == http.StatusSwitchingProtocols {
			sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			hdr.Set("Sec-WebSocket-Accept", base64.StdEncoding.EncodeToString(sum[:]))
		}
		return &http.Response{
			Status: strconv.Itoa(status) + " " + http.StatusText(status), StatusCode: status,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: hdr,
			Body: &lateBody{Reader: strings.NewReader(body), closed: closed}, ContentLength: int64(len(body)), Request: req,
		}
	}
}

// shortenOperationTimeout sets operationTimeout to d for the test.
func shortenOperationTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := operationTimeout
	operationTimeout = d
	t.Cleanup(func() { operationTimeout = old })
}

// wantLateTimeout requires that the late response was delivered (a case
// that never reached it has not tested anything), the operation deadline's
// classification of it (unavailable, "timed out") and its body closed.
func wantLateTimeout(t *testing.T, name string, lt *lateTransport, closed *atomic.Bool, err error) {
	t.Helper()
	if !lt.delivered.Load() {
		t.Errorf("%s: the late response was never delivered (err = %v); the case did not exercise the late-response path", name, err)
		return
	}
	if contract.CodeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("%s: a response delivered after the operation deadline = %v (code %q), want unavailable \"timed out\"", name, err, contract.CodeOf(err))
	}
	if !closed.Load() {
		t.Errorf("%s: the late response body was not closed", name)
	}
}

// TestRequestLateResponse is the deterministic regression for the
// hung-plane failure: a response that Do delivers after the operation
// deadline is a timeout, not a malformed plane, and after the caller's
// own cancellation it is that cancellation, unchanged.
func TestRequestLateResponse(t *testing.T) {
	ca := newTestCA(t)
	c, err := New("https://127.0.0.1:1", Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	shortenOperationTimeout(t, 50*time.Millisecond)
	for name, r := range map[string]struct {
		h    http.Header
		body string
	}{
		"implicit 200": {implicit200, ""},
		"well-formed":  {http.Header{contract.ProtocolHeader: {"3"}, "Content-Type": {"application/json"}}, `{"version":3,"nodes":[]}`},
	} {
		var closed atomic.Bool
		lt := &lateTransport{respond: lateResponse(200, r.h, r.body, &closed)}
		c.http.Transport = lt
		_, err := c.ListNodes(bg)
		wantLateTimeout(t, name, lt, &closed, err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var closed atomic.Bool
	lt := &lateTransport{cancelCaller: cancel, respond: lateResponse(200, implicit200, "", &closed)}
	c.http.Transport = lt
	if _, err := c.ListNodes(ctx); !lt.delivered.Load() || err != context.Canceled || !closed.Load() {
		t.Fatalf("a response delivered after the caller's cancellation = %v (delivered %v, body closed %v), want context.Canceled unchanged", err, lt.delivered.Load(), closed.Load())
	}
}

// TestFetchCALateResponse: the pinned bootstrap classifies a CA response
// delivered after its operation deadline, or after the caller's
// cancellation, the same way (the pinned exchange itself is real).
func TestFetchCALateResponse(t *testing.T) {
	ca := newTestCA(t)
	pin := Fingerprint(ca.cert.Raw)
	s := startServer(t, ca.leaf(t, true), planeHandler(ca.pem, nil))
	var lt *lateTransport
	orig := fetchCAClient
	t.Cleanup(func() { fetchCAClient = orig })
	fetchCAClient = func(tr *http.Transport) *http.Client {
		// The real pinned exchange gets its own generous bounds, so it
		// never competes with the short operation deadline under test;
		// only the operation context carries that deadline.
		tr.DialContext = (&net.Dialer{Timeout: exchangeBound}).DialContext
		tr.TLSHandshakeTimeout = exchangeBound
		tr.ResponseHeaderTimeout = exchangeBound
		hc := orig(tr)
		lt.exchange = tr
		hc.Transport = lt
		return hc
	}
	shortenOperationTimeout(t, 50*time.Millisecond)
	for name, r := range map[string]struct {
		h    http.Header
		body string
	}{
		"implicit 200": {implicit200, ""},
		"the CA":       {http.Header{}, string(ca.pem)},
	} {
		var closed atomic.Bool
		lt = &lateTransport{respond: lateResponse(200, r.h, r.body, &closed)}
		_, err := FetchCA(bg, s.url, pin)
		wantLateTimeout(t, name, lt, &closed, err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var closed atomic.Bool
	lt = &lateTransport{cancelCaller: cancel, respond: lateResponse(200, http.Header{}, string(ca.pem), &closed)}
	if _, err := FetchCA(ctx, s.url, pin); !lt.delivered.Load() || err != context.Canceled || !closed.Load() {
		t.Fatalf("a CA response delivered after the caller's cancellation = %v (delivered %v, body closed %v), want context.Canceled unchanged", err, lt.delivered.Load(), closed.Load())
	}
}

// TestDialNodeStreamLateResponse: a handshake answer delivered after the
// operation deadline is a timeout, whether it is a refusal or an upgrade
// (whose connection is closed, never returned), and after the caller's
// cancellation it is that cancellation, unchanged.
func TestDialNodeStreamLateResponse(t *testing.T) {
	ca := newTestCA(t)
	c, err := New("https://127.0.0.1:1", Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	shortenOperationTimeout(t, 50*time.Millisecond)
	upgrade := http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}
	for name, r := range map[string]struct {
		status int
		h      http.Header
	}{
		"implicit 200": {200, implicit200},
		"upgrade":      {101, upgrade},
	} {
		var closed atomic.Bool
		lt := &lateTransport{respond: lateResponse(r.status, r.h, "", &closed)}
		c.http.Transport = lt
		ws, err := c.DialNodeStream(bg)
		if ws != nil {
			ws.CloseNow()
			t.Errorf("%s: a connection upgraded after the operation deadline was returned", name)
		}
		wantLateTimeout(t, name, lt, &closed, err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var closed atomic.Bool
	lt := &lateTransport{cancelCaller: cancel, respond: lateResponse(101, upgrade, "", &closed)}
	c.http.Transport = lt
	ws, err := c.DialNodeStream(ctx)
	if ws != nil {
		ws.CloseNow()
	}
	if !lt.delivered.Load() || err != context.Canceled || !closed.Load() {
		t.Fatalf("an upgrade delivered after the caller's cancellation = %v (delivered %v, body closed %v), want context.Canceled unchanged", err, lt.delivered.Load(), closed.Load())
	}
}
