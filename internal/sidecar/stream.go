package sidecar

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Stream timing on the sidecar side.
const (
	heartbeatInterval = contract.HeartbeatIntervalMS * time.Millisecond
	// stepTimeout bounds hello_ok, each heartbeat's send/ack exchange and
	// each write.
	stepTimeout = 5 * time.Second
	// helloID is the hello's request ID; heartbeats use b1, b2, ...
	helloID = "h1"
	// invalidRequestID answers a message whose own request ID is invalid.
	invalidRequestID = "invalid"
)

func asContract(err error, ce **contract.Error) bool { return errors.As(err, ce) }

// readResult is one message (or the terminal error) from the reader, with
// the instant its read completed.
type readResult struct {
	typ  websocket.MessageType
	data []byte
	err  error
	at   time.Time
}

// inbox is the reader's bounded handoff to the session: one delivered
// message plus the one a blocked reader already completed. A message's
// completion instant is sampled and published in one critical section, so
// once the session finds the inbox empty after a deadline fired, any later
// message completed at or after that deadline.
type inbox struct {
	clock   clock
	mu      sync.Mutex
	item    *readResult
	pending *readResult
	ready   chan struct{}
	space   chan struct{}
}

func newInbox(c clock) *inbox {
	return &inbox{clock: c, ready: make(chan struct{}, 1), space: make(chan struct{}, 1)}
}

func notify(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (b *inbox) put(ctx context.Context, r readResult) bool {
	b.mu.Lock()
	r.at = b.clock.Now()
	if b.item == nil {
		b.item = &r
		b.mu.Unlock()
		notify(b.ready)
		return true
	}
	p := &r
	b.pending = p
	for b.pending == p {
		b.mu.Unlock()
		select {
		case <-b.space:
		case <-ctx.Done():
			return false
		}
		b.mu.Lock()
	}
	b.mu.Unlock()
	return true
}

func (b *inbox) take() (readResult, bool) {
	b.mu.Lock()
	if b.item == nil {
		b.mu.Unlock()
		return readResult{}, false
	}
	r := *b.item
	b.item, b.pending = b.pending, nil
	more := b.item != nil
	b.mu.Unlock()
	notify(b.space)
	if more {
		notify(b.ready)
	}
	return r, true
}

// sessionConn is one connection: one reader goroutine delivering into the
// bounded inbox and writes from the session goroutine only.
type sessionConn struct {
	d  *deps
	ws *websocket.Conn
	// ctx is the connection's own context: canceling it forces the socket
	// closed. It is not derived from Run's context, so shutdown can still
	// send a close frame.
	ctx    context.Context
	cancel context.CancelFunc

	in         *inbox
	readerDone chan struct{}
	closeOnce  sync.Once
}

func (d *deps) newSessionConn(ws *websocket.Conn) *sessionConn {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sessionConn{d: d, ws: ws, ctx: ctx, cancel: cancel, in: newInbox(d.clock), readerDone: make(chan struct{})}
	go func() {
		defer close(s.readerDone)
		for {
			typ, data, err := ws.Read(ctx)
			if !s.in.put(ctx, readResult{typ: typ, data: data, err: err}) || err != nil {
				return
			}
		}
	}()
	return s
}

// close ends the connection once: with a close frame (code, fixed reason)
// bounded by closeGrace, then forced; or at once when code is 0. It then
// joins the reader.
func (s *sessionConn) close(code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		if code == 0 {
			s.cancel()
			s.ws.CloseNow()
		} else {
			done := make(chan struct{})
			go func() {
				s.ws.Close(code, reason)
				close(done)
			}()
			t := time.NewTimer(s.d.closeGrace)
			select {
			case <-done:
			case <-t.C:
				s.cancel()
				<-done
			}
			t.Stop()
		}
		s.cancel()
		<-s.readerDone
	})
}

// write sends one message, bounded by stepTimeout on the injected clock;
// Run's cancellation or a forced close aborts it.
func (s *sessionConn) write(ctx context.Context, typ, requestID string, body any) error {
	return s.writeBy(ctx, time.Time{}, typ, requestID, body)
}

// writeBy is write with its bound shortened to the owning operation's
// absolute deadline (zero: none).
func (s *sessionConn) writeBy(ctx context.Context, deadline time.Time, typ, requestID string, body any) error {
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, requestID, body)
	if err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode a "+typ+" message", err)
	}
	now := s.d.clock.Now()
	at := now.Add(stepTimeout)
	if !deadline.IsZero() && deadline.Before(at) {
		at = deadline
	}
	if !at.After(now) {
		return contract.New(contract.CodeUnavailable, "no heartbeat acknowledgement within "+stepTimeout.String())
	}
	bound := at.Sub(now)
	wctx, cancel := clockDeadline(ctx, s.d.clock, at)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if err := s.ws.Write(wctx, websocket.MessageText, b); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return contract.Wrap(contract.CodeUnavailable, "cannot send to the plane: the connection was lost or blocked for "+bound.String(), err)
	}
	return nil
}

// errNoReply is a missed hello_ok.
var errNoReply = errors.New("no reply")

// await waits for the next message, at most timeout on the injected
// clock. Cancellation wins: ctx is checked after every wake-up.
func (s *sessionConn) await(ctx context.Context, timeout time.Duration) (readResult, error) {
	timer, stop := s.d.clock.NewTimer(timeout)
	defer stop()
	for {
		if r, ok := s.in.take(); ok {
			if cerr := ctx.Err(); cerr != nil {
				return readResult{}, cerr
			}
			return r, nil
		}
		select {
		case <-s.in.ready:
			continue
		case <-timer:
		case <-ctx.Done():
		}
		if cerr := ctx.Err(); cerr != nil {
			return readResult{}, cerr
		}
		// The timer fired: a message that completed before it still wins.
		if r, ok := s.in.take(); ok {
			return r, nil
		}
		return readResult{}, errNoReply
	}
}

// protocolError marks a defect the sidecar detected in the plane's
// messages: it answers with an error message and closes 1008.
type protocolError struct {
	requestID string
	err       *contract.Error
}

func (e *protocolError) Error() string { return e.err.Error() }
func (e *protocolError) Unwrap() error { return e.err }

func invalid(requestID, msg string) error {
	return &protocolError{requestID: requestID, err: contract.New(contract.CodeInvalidArgument, msg)}
}

// lost classifies a read error: the plane going away is unavailable
// (retried); an oversized or unsupported message is invalid (terminal).
func lost(err error) error {
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return contract.Wrap(contract.CodeInvalidArgument, "the plane sent a message larger than "+strconv.Itoa(contract.MaxFrameBytes)+" bytes", err)
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusGoingAway:
		return contract.Wrap(contract.CodeUnavailable, "the plane closed the stream: it is shutting down", err)
	case websocket.StatusPolicyViolation:
		return contract.Wrap(contract.CodeUnavailable, "the plane closed the stream (policy)", err)
	case websocket.StatusMessageTooBig, websocket.StatusUnsupportedData:
		return contract.Wrap(contract.CodeInvalidArgument, "the plane rejected a message this sidecar sent; check that plane and sidecar are the same callsheet version", err)
	}
	return contract.Wrap(contract.CodeUnavailable, "the connection to the plane was lost", err)
}

// decode validates one delivered message's envelope. A plane error message
// is returned as its contract error; a version mismatch is reported with
// local=this sidecar and remote=the plane, recognized even though the
// plane's envelope carries its own version.
func decode(r readResult) (contract.NodeFrame, error) {
	if r.err != nil {
		return contract.NodeFrame{}, lost(r.err)
	}
	if r.typ != websocket.MessageText {
		return contract.NodeFrame{}, invalid(invalidRequestID, "the plane sent a binary message")
	}
	f, err := contract.DecodeFrame(r.data, contract.FromPlane)
	if err != nil {
		var ce *contract.Error
		if asContract(err, &ce) && ce.Code == contract.CodeProtocolMismatch {
			return f, ce
		}
		return f, &protocolError{requestID: f.RequestID, err: ce}
	}
	if f.Type == contract.FrameError {
		pe, err := contract.ParseErrorBody(f.Body)
		if err != nil {
			return f, &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
		}
		if pe.Code == contract.CodeProtocolMismatch {
			if remote, ok := pe.DetailInt("local_version"); ok {
				return f, contract.VersionMismatch(contract.ProtocolVersion, remote)
			}
		}
		return f, &contract.Error{Code: pe.Code, Message: "the plane refused the stream: " + pe.Message, Details: pe.Details}
	}
	return f, nil
}

// expect validates one delivered message as typ with requestID.
func expect(r readResult, typ, requestID string) (contract.NodeFrame, error) {
	f, err := decode(r)
	if err != nil {
		return f, err
	}
	if f.Type != typ {
		return f, invalid(f.RequestID, "unexpected "+f.Type+" message from the plane")
	}
	if f.RequestID != requestID {
		return f, invalid(f.RequestID, "unknown or stale acknowledgement "+f.RequestID+" (want "+requestID+")")
	}
	return f, nil
}

// session runs one connection: dial, hello, hello_ok, then the duplex
// session (heartbeats b1, b2, ... one outstanding at a time, the first
// immediately and then every heartbeatInterval, coalescing; plane requests
// p1, p2, ... answered one at a time). It returns why the connection
// ended.
func (d *deps) session(ctx context.Context, c planeClient, n int, id, sw string, env roleEnv, w *workers, logger *slog.Logger, onStable func()) (err error) {
	ws, err := c.DialNodeStream(ctx)
	if err != nil {
		return err
	}
	d.emit(event{kind: evDialed, session: n})
	s := d.newSessionConn(ws)
	defer func() {
		var pe *protocolError
		switch {
		case ctx.Err() != nil:
			s.close(websocket.StatusGoingAway, "sidecar shutting down")
		case errors.As(err, &pe):
			rid := pe.requestID
			if !contract.ValidRequestID(rid) {
				rid = invalidRequestID
			}
			wctx, cancel := context.WithCancel(context.Background())
			s.write(wctx, contract.FrameError, rid, pe.err)
			cancel()
			s.close(websocket.StatusPolicyViolation, "invalid message")
			err = pe.err
		default:
			s.close(0, "")
		}
	}()
	if err := s.write(ctx, contract.FrameHello, helloID, contract.HelloBody{NodeID: id, SoftwareVersion: sw}); err != nil {
		return err
	}
	d.emit(event{kind: evAwaitReply, session: n})
	r, err := s.await(ctx, stepTimeout)
	if errors.Is(err, errNoReply) {
		return contract.New(contract.CodeUnavailable, "the plane did not answer hello within "+stepTimeout.String())
	}
	if err != nil {
		return err
	}
	f, err := expect(r, contract.FrameHelloOK, helloID)
	if err != nil {
		return err
	}
	if _, err := contract.DecodeHelloOK(f.Body); err != nil {
		return &protocolError{requestID: f.RequestID, err: err.(*contract.Error)}
	}
	logger.Info("connected", "session", n)
	d.emit(event{kind: evConnected, session: n})
	jobs, cancelJobs := context.WithCancel(ctx)
	rs := &roleSession{d: d, s: s, ctx: ctx, n: n, id: id, env: env, w: w, logger: logger, onStable: onStable, jobs: jobs}
	defer rs.cleanup(cancelJobs)
	return rs.run()
}
