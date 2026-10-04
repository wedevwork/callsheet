package sidecar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// writeHold keeps one sidecar write active after its bytes were delivered
// to the plane: the first sidecar message matching match is written
// through, then its connection write blocks until the test releases it (or
// the connection is closed). Whatever bounds that write stays armed
// meanwhile, so a clock move past its bound closes the connection.
type writeHold struct {
	clk   clock
	match func(contract.NodeFrame) bool

	once    sync.Once
	held    chan struct{} // closed once a write is held
	release chan struct{} // closed to let it return
	relOnce sync.Once

	mu     sync.Mutex
	at     time.Time // clock when the write was held
	end    time.Time // clock when it returned
	closed bool      // it returned because the connection closed
	done   bool
}

func newWriteHold(match func(contract.NodeFrame) bool) *writeHold {
	return &writeHold{match: match, held: make(chan struct{}), release: make(chan struct{})}
}

// install makes d's node stream connections hold through h, timed on d's
// clock, for the fake plane fp.
func (h *writeHold) install(fp *fakePlane) func(d *deps) {
	return func(d *deps) {
		h.clk = d.clock
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(fp.caPEM)
		d.newClient = func(string, clientTrust) (planeClient, error) {
			return &holdPlane{url: fp.url, tls: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, h: h}, nil
		}
	}
}

// wait returns once a write is held (bounded).
func (h *writeHold) wait(t testing.TB) {
	t.Helper()
	h.waitBy(t, time.After(testWait))
}

// waitBy returns once a write is held unless timeout fires first; at the
// timeout h.held is rechecked before t fails.
func (h *writeHold) waitBy(t testing.TB, timeout <-chan time.Time) {
	t.Helper()
	testkit.WithinBy(t, h.held, timeout, "a sidecar write held after its delivery")
}

// TestWriteHoldWaitAtBound is the C14 regression for writeHold.wait
// (iteration 10b review r6): a write held and the wait's bound both ready
// must never fail the wait, whichever the select takes; each attempt
// leaves both ready. With nothing held, the expired bound still fails it.
func TestWriteHoldWaitAtBound(t *testing.T) {
	t.Parallel()
	t.Run("both-ready", func(t *testing.T) {
		t.Parallel()
		for i := range 100 {
			h := newWriteHold(nil)
			close(h.held)
			if testkit.Fails(func(tb testing.TB) { h.waitBy(tb, testkit.Fired()) }) {
				t.Fatalf("attempt %d: the wait failed although the write was held", i+1)
			}
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		h := newWriteHold(nil)
		if !testkit.Fails(func(tb testing.TB) { h.waitBy(tb, testkit.Fired()) }) {
			t.Fatal("the wait passed with no write held")
		}
	})
}

// let releases the held write (idempotent).
func (h *writeHold) let() { h.relOnce.Do(func() { close(h.release) }) }

// check requires that a write was held, returned on its release (the
// connection open) and that the clock did not move while it was active.
func (h *writeHold) check(t testing.TB) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !h.done:
		t.Fatal("the held write did not return")
	case h.closed:
		t.Fatalf("the held write ended with its connection closed (clock %v, held at %v)", h.end, h.at)
	case !h.end.Equal(h.at):
		t.Fatalf("the clock moved from %v to %v while the held write was active", h.at, h.end)
	}
}

// hold runs in the sidecar's connection write after p was written through.
func (h *writeHold) hold(p []byte, closed <-chan struct{}) {
	f, ok := clientFrame(p)
	if !ok || !h.match(f) {
		return
	}
	first := false
	h.once.Do(func() { first = true })
	if !first {
		return
	}
	h.mu.Lock()
	h.at = h.clk.Now()
	h.mu.Unlock()
	close(h.held)
	byClose := false
	select {
	case <-h.release:
	case <-closed:
		byClose = true
	}
	h.mu.Lock()
	h.end, h.closed, h.done = h.clk.Now(), byClose, true
	h.mu.Unlock()
}

// clientFrame decodes p when it is exactly one complete, masked text frame
// a WebSocket client wrote (the whole flushed message).
func clientFrame(p []byte) (contract.NodeFrame, bool) {
	if len(p) < 2 || p[0] != 0x81 || p[1]&0x80 == 0 {
		return contract.NodeFrame{}, false
	}
	n, i := int(p[1]&0x7f), 2
	switch n {
	case 126:
		if len(p) < 4 {
			return contract.NodeFrame{}, false
		}
		n, i = int(binary.BigEndian.Uint16(p[2:4])), 4
	case 127:
		if len(p) < 10 {
			return contract.NodeFrame{}, false
		}
		n, i = int(binary.BigEndian.Uint64(p[2:10])), 10
	}
	if n < 0 || len(p) != i+4+n {
		return contract.NodeFrame{}, false
	}
	key, payload := p[i:i+4], make([]byte, n)
	for j := range payload {
		payload[j] = p[i+4+j] ^ key[j%4]
	}
	f, err := contract.DecodeFrame(payload, contract.FromSidecar)
	return f, err == nil
}

// holdPlane is a planeClient for a fake plane whose node stream runs over
// a holdConn.
type holdPlane struct {
	url string
	tls *tls.Config
	h   *writeHold
}

func (p *holdPlane) EnrollNode(context.Context, string, string) (contract.Node, error) {
	return contract.Node{}, errors.New("not supported")
}

func (p *holdPlane) Close() {}

func (p *holdPlane) DialNodeStream(ctx context.Context) (*websocket.Conn, error) {
	hc := &http.Client{Transport: &http.Transport{DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&tls.Dialer{Config: p.tls}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &holdConn{Conn: c, h: p.h, closed: make(chan struct{})}, nil
	}}}
	ws, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(p.url, "https")+contract.PathNodeStream,
		&websocket.DialOptions{HTTPClient: hc, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(contract.MaxFrameBytes)
	return ws, nil
}

// holdConn is the sidecar side of the node stream below the WebSocket
// (plaintext frames, above TLS).
type holdConn struct {
	net.Conn
	h      *writeHold
	closed chan struct{}
	once   sync.Once
}

func (c *holdConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil {
		c.h.hold(p, c.closed)
	}
	return n, err
}

func (c *holdConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
