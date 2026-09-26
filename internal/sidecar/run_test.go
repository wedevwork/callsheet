package sidecar

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// jitterDelay is backoff with the tests' fixed jitter sample 0.5.
func jitterDelay(attempt int) time.Duration { return backoff(attempt, 0.5) }

// fakeRun is Run against a fake plane with a fake clock.
type fakeRun struct {
	fp   *fakePlane
	clk  *testkit.FakeClock
	d    *deps
	ev   *events
	logs *syncLog
	run  *running
	root string
}

func startFakeRun(t *testing.T, fp *fakePlane, url string, caPEM []byte) *fakeRun {
	t.Helper()
	f := &fakeRun{fp: fp, clk: testkit.NewFakeClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)), logs: newSyncLog(), root: newRoot(t)}
	f.d = testDeps(f.clk)
	f.ev = observe(f.d)
	writeState(t, f.root, testID, url, caPEM)
	f.run = startRun(t, f.d, f.root, slog.New(slog.NewJSONHandler(f.logs, nil)))
	return f
}

// advanceBackoff waits for the backoff event and its timer, checks the
// delay, then advances past it.
func (f *fakeRun) advanceBackoff(t *testing.T, want time.Duration) {
	t.Helper()
	ev := f.ev.await(t, evBackoff)
	if ev.delay != want {
		t.Fatalf("backoff %v, want %v", ev.delay, want)
	}
	if err := f.clk.AwaitWaiter(testWait, testkit.HasTimer(want)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(want)
}

// awaitReply waits until the session's request (hello for 0, heartbeat
// acks otherwise) was written and it waits for the reply: from then on the
// only 5 s timer is the reply wait, not the write's own bound.
func (f *fakeRun) awaitReply(t *testing.T, acks int) {
	t.Helper()
	for {
		if ev := f.ev.await(t, evAwaitReply); ev.acks == acks {
			return
		}
	}
}

// tick advances one heartbeat interval once the session, after an
// acknowledgement, waits for the next heartbeat's due time.
func (f *fakeRun) tick(t *testing.T) {
	t.Helper()
	if err := f.clk.AwaitWaiter(testWait, testkit.HasTimer(heartbeatInterval)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(heartbeatInterval)
}

// stopped cancels Run and requires a clean cancellation with no clock
// waiter left behind.
func (f *fakeRun) stopped(t *testing.T) {
	t.Helper()
	f.run.cancel()
	if err := f.run.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v", err)
	}
	if ws := f.clk.Waiters(); len(ws) != 0 {
		t.Fatalf("timers or tickers leaked: %v", ws)
	}
}

// TestBackoffAndTimers asserts the real schedule, jitter bounds, cap and
// the production timer durations.
func TestBackoffAndTimers(t *testing.T) {
	base := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, b := range base {
		if lo := backoff(i, 0); lo != b*8/10 {
			t.Fatalf("attempt %d low = %v", i, lo)
		}
		if hi := backoff(i, 0.9999999); hi >= b || hi < b*99/100 {
			t.Fatalf("attempt %d high = %v", i, hi)
		}
		if mid := backoff(i, 0.5); mid != b*9/10 {
			t.Fatalf("attempt %d mid = %v", i, mid)
		}
	}
	if backoff(1000, 1) != 30*time.Second {
		t.Fatal("cap")
	}
	if heartbeatInterval != 5*time.Second || stepTimeout != 5*time.Second || stableAcks != 3 || closeGrace != time.Second || defaultDeps().closeGrace != time.Second {
		t.Fatal("protocol-1 timing constants changed")
	}
	for _, u := range []float64{defaultDeps().jitter(), defaultDeps().jitter()} {
		if u < 0 || u >= 1 {
			t.Fatalf("jitter sample %v", u)
		}
	}
	// The real clock adapters fire and stop.
	var c clock = realClock{}
	tc, stop := c.NewTimer(time.Millisecond)
	<-tc
	if stop() {
		t.Fatal("stopped a fired timer")
	}
	kc, kstop := c.NewTicker(time.Millisecond)
	<-kc
	kstop()
	if c.Now().IsZero() {
		t.Fatal("now")
	}
	ctx, cancel := clockTimeout(bg, c, time.Millisecond)
	defer cancel()
	<-ctx.Done()
	for _, code := range []contract.Code{contract.CodeProtocolMismatch, contract.CodeInvalidArgument, contract.CodeNotFound, contract.CodeTrustFailed} {
		if !terminal(contract.New(code, "")) {
			t.Fatalf("%s must be terminal", code)
		}
	}
	for _, code := range []contract.Code{contract.CodeConflict, contract.CodeUnavailable, contract.CodeInternal} {
		if terminal(contract.New(code, "")) {
			t.Fatalf("%s must be retried", code)
		}
	}
}

// TestReconnectUnavailable: an absent plane is retried forever on the
// capped schedule; Run never exits for it.
func TestReconnectUnavailable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	fp := startFakePlane(t)
	f := startFakeRun(t, nil, "https://"+addr, fp.caPEM)
	f.ev.await(t, evStarted)
	for i := range 8 {
		ev := f.ev.await(t, evEnded)
		wantCode(t, ev.err, contract.CodeUnavailable, "cannot reach the plane")
		f.advanceBackoff(t, jitterDelay(i))
	}
	if !strings.Contains(f.logs.String(), `"retry_in_ms":27000`) {
		t.Fatalf("capped retry not logged: %s", f.logs.String())
	}
	f.stopped(t)
}

// TestReconnectResetAfterThirdAck: the backoff resets only after three
// consecutive acknowledgements on one connection.
func TestReconnectResetAfterThirdAck(t *testing.T) {
	fp := startFakePlane(t)
	f := startFakeRun(t, fp, fp.url, fp.caPEM)
	for s, acks := range []int{1, 2, 3, 2} {
		c := fp.accept(t)
		c.helloOK(testID)
		for k := 1; k <= acks; k++ {
			if k > 1 {
				f.tick(t)
			}
			c.ack(k)
			if ev := f.ev.await(t, evAck); ev.acks != k || ev.session != s+1 {
				t.Fatalf("ack event %+v", ev)
			}
		}
		c.finish()
		ev := f.ev.await(t, evEnded)
		wantCode(t, ev.err, contract.CodeUnavailable, "lost")
		// Sessions 1 and 2 back off 1 s then 2 s; the third's three
		// acknowledgements reset the schedule to 1 s, then 2 s again.
		f.advanceBackoff(t, jitterDelay([]int{0, 1, 0, 1}[s]))
	}
	c := fp.accept(t)
	c.helloOK(testID)
	c.ack(1)
	// Cancel only once the sidecar processed that acknowledgement: no
	// write is in flight, so the shutdown close frame can be sent.
	if ev := f.ev.await(t, evAck); ev.session != 5 || ev.acks != 1 {
		t.Fatalf("final ack event %+v", ev)
	}
	logs := f.logs.String()
	for _, want := range []string{`"msg":"sidecar starting"`, `"msg":"connected"`, `"msg":"heartbeat acknowledged"`, `"msg":"disconnected; retrying"`, `"node_id":"` + testID + `"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s:\n%s", want, logs)
		}
	}
	f.stopped(t)
	if st := c.closed(); st != websocket.StatusGoingAway {
		t.Fatalf("shutdown close = %v", st)
	}
}

// TestHeartbeatsCoalesce: heartbeat intervals missed while the sidecar was
// busy do not queue; one current heartbeat follows, and the cadence
// continues from it.
func TestHeartbeatsCoalesce(t *testing.T) {
	fp := startFakePlane(t)
	f := startFakeRun(t, fp, fp.url, fp.caPEM)
	c := fp.accept(t)
	c.helloOK(testID)
	c.ack(1)
	f.ev.await(t, evAck)
	if err := f.clk.AwaitWaiter(testWait, testkit.HasTimer(heartbeatInterval)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(4 * heartbeatInterval) // four intervals, one heartbeat
	c.ack(2)
	f.ev.await(t, evAck)
	// The next heartbeat is due a full interval after b2, not at once.
	if err := f.clk.AwaitWaiter(testWait, testkit.HasTimer(heartbeatInterval)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(heartbeatInterval - time.Nanosecond)
	f.clk.Advance(time.Nanosecond)
	c.ack(3) // b3, never b3..b5 from queued intervals
	if ev := f.ev.await(t, evStable); ev.acks != 3 {
		t.Fatalf("stable %+v", ev)
	}
	f.stopped(t)
}

// TestReconnectTerminal: configuration errors end Run with their codes;
// a duplicate stream and a refused 503 upgrade are retried.
func TestReconnectTerminal(t *testing.T) {
	for _, c := range []struct {
		name  string
		plane func(t *testing.T, c *fakeConn)
		code  contract.Code
		msg   string
		reply bool // the sidecar answers with an error message and 1008
	}{
		{"mismatch", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.send(2, contract.FrameError, "h1", contract.VersionMismatch(2, 1))
		}, contract.CodeProtocolMismatch, "protocol version mismatch: local=1 remote=2", false},
		{"mismatch-v1", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.send(1, contract.FrameError, "h1", contract.VersionMismatch(3, 1))
		}, contract.CodeProtocolMismatch, "local=1 remote=3", false},
		{"future-hello-ok", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.send(2, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: 5000, LeaseMS: 15000})
		}, contract.CodeProtocolMismatch, "local=1 remote=2", false},
		{"unknown-node", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.send(1, contract.FrameError, "h1", contract.New(contract.CodeNotFound, "node is not enrolled"))
		}, contract.CodeNotFound, "the plane refused the stream: node is not enrolled", false},
		{"bad-hello-ok", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.send(1, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: 5000, LeaseMS: 3})
		}, contract.CodeInvalidArgument, "lease_ms=15000", true},
		{"stale-ack", func(t *testing.T, c *fakeConn) {
			c.helloOK(testID)
			c.expect(contract.FrameHeartbeat, "b1")
			c.send(1, contract.FrameHeartbeatAck, "b7", nil)
		}, contract.CodeInvalidArgument, "stale acknowledgement b7", true},
		{"wrong-type", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.send(1, contract.FrameHeartbeatAck, "h1", nil)
		}, contract.CodeInvalidArgument, "unexpected heartbeat_ack", true},
		{"bad-ack-body", func(t *testing.T, c *fakeConn) {
			c.helloOK(testID)
			c.expect(contract.FrameHeartbeat, "b1")
			c.sendRaw([]byte(`{"version":1,"type":"heartbeat_ack","request_id":"b1","body":{"x":1}}`))
		}, contract.CodeInvalidArgument, "unknown field", true},
		{"malformed", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.sendRaw([]byte(`{"version":1,"type":"hello_ok"`))
		}, contract.CodeInvalidArgument, "malformed", true},
		{"bad-error-body", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.sendRaw([]byte(`{"version":1,"type":"error","request_id":"h1","body":{"error":{"code":"bogus","message":"m"}}}`))
		}, contract.CodeInvalidArgument, "unknown error code", true},
		{"binary", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			ctx, cancel := context.WithTimeout(bg, testWait)
			defer cancel()
			c.ws.Write(ctx, websocket.MessageBinary, []byte("x"))
		}, contract.CodeInvalidArgument, "binary", true},
		{"oversized", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			ctx, cancel := context.WithTimeout(bg, testWait)
			defer cancel()
			c.ws.Write(ctx, websocket.MessageText, make([]byte, contract.MaxFrameBytes+1))
		}, contract.CodeInvalidArgument, "larger than 16384 bytes", false},
		{"unsolicited", func(t *testing.T, c *fakeConn) {
			c.helloOK(testID)
			c.ack(1)
			c.send(1, contract.FrameHeartbeatAck, "b1", nil)
		}, contract.CodeInvalidArgument, "unexpected heartbeat_ack", true},
		{"peer-rejected", func(t *testing.T, c *fakeConn) {
			c.expect(contract.FrameHello, "h1")
			c.ws.Close(websocket.StatusMessageTooBig, "too big")
		}, contract.CodeInvalidArgument, "same callsheet version", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fp := startFakePlane(t)
			f := startFakeRun(t, fp, fp.url, fp.caPEM)
			conn := fp.accept(t)
			c.plane(t, conn)
			err := f.run.result(t)
			wantCode(t, err, c.code, c.msg)
			if c.reply {
				f := conn.recv()
				if f.Type != contract.FrameError {
					t.Fatalf("sidecar reply %+v", f)
				}
				if e, _ := contract.ParseErrorBody(f.Body); e == nil || e.Code != contract.CodeInvalidArgument {
					t.Fatalf("sidecar error body %s", f.Body)
				}
				if st := conn.closed(); st != websocket.StatusPolicyViolation {
					t.Fatalf("sidecar close = %v", st)
				}
			}
			if c.code == contract.CodeProtocolMismatch && (!strings.Contains(f.logs.String(), `"local_version":1`) || !strings.Contains(f.logs.String(), `"msg":"connection failed permanently"`)) {
				t.Fatalf("mismatch logs: %s", f.logs.String())
			}
			if ws := f.clk.Waiters(); len(ws) != 0 {
				t.Fatalf("waiters leaked: %v", ws)
			}
		})
	}
	t.Run("conflict-retried", func(t *testing.T) {
		fp := startFakePlane(t)
		f := startFakeRun(t, fp, fp.url, fp.caPEM)
		c := fp.accept(t)
		c.expect(contract.FrameHello, "h1")
		c.send(1, contract.FrameError, "h1", contract.New(contract.CodeConflict, "node already has an active stream"))
		f.advanceBackoff(t, jitterDelay(0))
		fp.accept(t).helloOK(testID)
		f.ev.await(t, evConnected)
		f.stopped(t)
	})
	t.Run("upgrade-refused", func(t *testing.T) {
		fp := startFakePlane(t)
		fp.setRefuse(http.StatusServiceUnavailable)
		f := startFakeRun(t, fp, fp.url, fp.caPEM)
		wantCode(t, f.ev.await(t, evEnded).err, contract.CodeUnavailable, "503")
		fp.setRefuse(http.StatusNotFound)
		f.advanceBackoff(t, jitterDelay(0))
		wantCode(t, f.run.result(t), contract.CodeInvalidArgument, "404")
	})
	t.Run("trust", func(t *testing.T) {
		fp := startFakePlane(t)
		other := startFakePlane(t)
		f := startFakeRun(t, fp, fp.url, other.caPEM)
		wantCode(t, f.run.result(t), contract.CodeTrustFailed, "connection not trusted")
	})
}

// TestReconnectTimeouts: a plane that never answers hello, or never
// acknowledges, is abandoned after 5 s on the injected clock and retried.
func TestReconnectTimeouts(t *testing.T) {
	fp := startFakePlane(t)
	f := startFakeRun(t, fp, fp.url, fp.caPEM)
	c := fp.accept(t)
	c.expect(contract.FrameHello, "h1")
	// The reply wait, not the hello write's own 5 s bound.
	f.awaitReply(t, 0)
	if err := f.clk.AwaitWaiter(testWait, testkit.HasTimer(stepTimeout)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(stepTimeout)
	wantCode(t, f.ev.await(t, evEnded).err, contract.CodeUnavailable, "did not answer hello")
	f.advanceBackoff(t, jitterDelay(0))
	c = fp.accept(t)
	c.helloOK(testID)
	c.expect(contract.FrameHeartbeat, "b1") // blackhole: never acknowledged
	f.awaitReply(t, 1)
	if err := f.clk.AwaitWaiter(testWait, testkit.HasTimer(stepTimeout)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(stepTimeout)
	wantCode(t, f.ev.await(t, evEnded).err, contract.CodeUnavailable, "no heartbeat acknowledgement")
	f.advanceBackoff(t, jitterDelay(1))
	fp.accept(t).helloOK(testID)
	f.ev.await(t, evConnected)
	f.stopped(t)
}

// TestRunCancellation: cancellation wins while dialing, reading, waiting
// for a tick, backing off and writing, and leaves nothing behind.
func TestRunCancellation(t *testing.T) {
	t.Run("dialing", func(t *testing.T) {
		// A listener that accepts and never speaks TLS.
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer ln.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			c, err := ln.Accept()
			if err == nil {
				accepted <- c
			}
		}()
		fp := startFakePlane(t)
		f := startFakeRun(t, nil, "https://"+ln.Addr().String(), fp.caPEM)
		select {
		case c := <-accepted:
			defer c.Close()
		case <-time.After(testWait):
			t.Fatal("no dial")
		}
		f.stopped(t)
	})
	t.Run("reading", func(t *testing.T) {
		fp := startFakePlane(t)
		f := startFakeRun(t, fp, fp.url, fp.caPEM)
		c := fp.accept(t)
		c.helloOK(testID)
		c.expect(contract.FrameHeartbeat, "b1")
		// Cancel while reading: after b1's write returned (a 5 s timer
		// alone could still be the write's own bound).
		f.awaitReply(t, 1)
		f.stopped(t)
		if st := c.closed(); st != websocket.StatusGoingAway {
			t.Fatalf("close = %v", st)
		}
	})
	t.Run("backoff", func(t *testing.T) {
		fp := startFakePlane(t)
		f := startFakeRun(t, fp, fp.url, fp.caPEM)
		fp.accept(t).finish()
		f.ev.await(t, evBackoff)
		f.stopped(t)
	})
	t.Run("before-start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if err := testDeps(nil).run(ctx, RunOptions{StateDir: newRoot(t), SoftwareVersion: "dev"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("run = %v", err)
		}
	})
	t.Run("writing", func(t *testing.T) {
		// A net.Pipe peer that never reads blocks the writer.
		clk := testkit.NewFakeClock(time.Now())
		d := testDeps(clk)
		ws, stop := pipeStream(t)
		defer stop()
		s := d.newSessionConn(ws)
		ctx, cancel := context.WithCancel(bg)
		done := make(chan error, 1)
		go func() { done <- s.write(ctx, contract.FrameHeartbeat, "b1", contract.HeartbeatBody{}) }()
		if err := clk.AwaitWaiter(testWait, testkit.HasTimer(stepTimeout)); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled write = %v", err)
		}
		s.close(0, "")
		// The injected write bound also releases a blocked write.
		ws2, stop2 := pipeStream(t)
		defer stop2()
		s2 := d.newSessionConn(ws2)
		go func() { done <- s2.write(bg, contract.FrameHeartbeat, "b1", contract.HeartbeatBody{}) }()
		if err := clk.AwaitWaiter(testWait, testkit.HasTimer(stepTimeout)); err != nil {
			t.Fatal(err)
		}
		clk.Advance(stepTimeout)
		wantCode(t, <-done, contract.CodeUnavailable, "blocked")
		s2.close(websocket.StatusGoingAway, "bye")
		if ws := clk.Waiters(); len(ws) != 0 {
			t.Fatalf("waiters leaked: %v", ws)
		}
		if err := s2.write(bg, contract.FrameHeartbeat, strings.Repeat("x", contract.MaxFrameBytes), nil); contract.CodeOf(err) != contract.CodeInternal {
			t.Fatalf("oversized outbound = %v", err)
		}
	})
}

// pipeStream returns a client WebSocket over net.Pipe whose server side
// completes the upgrade and then never reads, so writes block.
func pipeStream(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	c1, c2 := net.Pipe()
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-release
		ws.CloseNow()
	})}
	go srv.Serve(&oneConnListener{c: c2, done: make(chan struct{})})
	hc := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return c1, nil }}}
	ctx, cancel := context.WithTimeout(bg, testWait)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://pipe/", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	return ws, func() { close(release); c1.Close(); c2.Close(); srv.Close() }
}

// oneConnListener serves exactly one connection.
type oneConnListener struct {
	c    net.Conn
	once bool
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if !l.once {
		l.once = true
		return l.c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// TestRunStartup covers Run's state checks before any connection.
func TestRunStartup(t *testing.T) {
	d := testDeps(nil)
	missing := newRoot(t)
	wantCode(t, d.run(bg, RunOptions{StateDir: missing, SoftwareVersion: "dev"}), contract.CodeNotFound, "run callsheet sidecar enroll")
	empty := newRoot(t)
	mkdir0700(t, empty)
	wantCode(t, d.run(bg, RunOptions{StateDir: empty, SoftwareVersion: "dev"}), contract.CodeNotFound, "run callsheet sidecar enroll")
	wantCode(t, d.run(bg, RunOptions{StateDir: empty, SoftwareVersion: "a b"}), contract.CodeInvalidArgument, "software version")
	fp := startFakePlane(t)
	bad := newRoot(t)
	writeState(t, bad, testID, fp.url, fp.caPEM)
	writeFile0600(t, filepath.Join(bad, enrollmentName), "{")
	wantCode(t, d.run(bg, RunOptions{StateDir: bad, SoftwareVersion: "dev"}), contract.CodeConflict, "invalid sidecar state")
	writeFile0600(t, filepath.Join(bad, identityName), "{")
	wantCode(t, d.run(bg, RunOptions{StateDir: bad, SoftwareVersion: "dev"}), contract.CodeConflict, "invalid sidecar state")
	unexpected := newRoot(t)
	mkdir0700(t, unexpected)
	writeFile0600(t, filepath.Join(unexpected, "x"), "")
	wantCode(t, d.run(bg, RunOptions{StateDir: unexpected, SoftwareVersion: "dev"}), contract.CodeConflict, "unexpected")
	// The public entry points use the real dependencies.
	wantCode(t, Run(bg, RunOptions{StateDir: missing, SoftwareVersion: "dev"}), contract.CodeNotFound)
	if _, err := Enroll(bg, EnrollOptions{StateDir: missing, PlaneURL: fp.url, SoftwareVersion: "dev"}); contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("Enroll without trust = %v", err)
	}
	// A client that cannot be built is reported as is.
	d2 := testDeps(nil)
	d2.newClient = func(string, clientTrust) (planeClient, error) {
		return nil, contract.New(contract.CodeTrustFailed, "x")
	}
	good := newRoot(t)
	writeState(t, good, testID, fp.url, fp.caPEM)
	wantCode(t, d2.run(bg, RunOptions{StateDir: good, SoftwareVersion: "dev"}), contract.CodeTrustFailed)
}

type clientTrust = client.Trust

func mkdir0700(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile0600(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
