package plane

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Close codes and the per-message bounds of the node stream.
const (
	closeGoingAway   = websocket.StatusGoingAway       // 1001, successful shutdown
	closePolicy      = websocket.StatusPolicyViolation // 1008, protocol errors
	closeUnsupported = websocket.StatusUnsupportedData // 1003, binary messages
	// streamStepTimeout bounds the first hello and each write.
	streamStepTimeout = 5 * time.Second
	// streamCloseGrace bounds a graceful close before a forced close.
	streamCloseGrace = time.Second
	// invalidRequestID answers a message whose own request ID is invalid.
	invalidRequestID = "invalid"
)

// connKey stores the accepted connection in each request's context
// (http.Server.ConnContext), so the stream handler can clear inherited
// deadlines and force-close a blocked socket.
type connKey struct{}

// nodeStream is one upgraded node connection: one reader (the handler
// goroutine) and serialized writes, at most one request in flight.
type nodeStream struct {
	svc *nodeService
	ws  *websocket.Conn
	raw net.Conn
	// ctx is canceled only to force the socket closed.
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	closed    chan struct{}
}

// handleStream serves GET /api/v1/node-stream. Browser origins, other
// methods and query strings are refused before the upgrade; after
// shutdown began the upgrade is unavailable.
func (s *nodeService) handleStream(w http.ResponseWriter, r *http.Request) {
	// Node clients require the protocol header on every response,
	// including this handler's own refusals.
	w.Header().Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
	switch {
	case r.Header.Get("Origin") != "":
		writeError(w, contract.New(contract.CodeInvalidArgument, "browser origins are not accepted on the node stream"))
		return
	case r.Method != http.MethodGet:
		writeError(w, contract.New(contract.CodeInvalidArgument, "method not allowed; use GET"))
		return
	case r.URL.RawQuery != "" || r.URL.ForceQuery:
		writeError(w, contract.New(contract.CodeInvalidArgument, "query strings are not accepted"))
		return
	}
	if s.isClosed() {
		writeError(w, contract.New(contract.CodeUnavailable, "the plane is shutting down"))
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return // Accept wrote the response
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &nodeStream{svc: s, ws: ws, ctx: ctx, cancel: cancel, closed: make(chan struct{})}
	st.raw, _ = r.Context().Value(connKey{}).(net.Conn)
	if st.raw != nil {
		// The ten-second HTTP bounds must not kill a healthy long-lived
		// stream; explicit stream timeouts bound it instead.
		st.raw.SetDeadline(time.Time{})
	}
	ws.SetReadLimit(contract.MaxFrameBytes)
	if !s.admit(st) {
		st.terminate(closeGoingAway, "plane shutting down")
		<-st.closed
		return
	}
	defer s.release(st)
	st.serve()
	st.terminate(closeGoingAway, "stream closed")
	<-st.closed
}

// terminate closes the socket once: a close frame with code and a fixed
// reason, bounded by closeGrace, then a forced close. It returns at once;
// st.closed closes when the socket is closed.
func (st *nodeStream) terminate(code websocket.StatusCode, reason string) {
	st.closeOnce.Do(func() {
		go func() {
			defer close(st.closed)
			done := make(chan struct{})
			go func() {
				st.ws.Close(code, reason)
				close(done)
			}()
			t := time.NewTimer(st.svc.closeGrace)
			defer t.Stop()
			select {
			case <-done:
				return
			case <-t.C:
			}
			st.force()
			<-done
		}()
	})
}

// force closes the socket at once, releasing any blocked reader or writer.
func (st *nodeStream) force() {
	st.cancel()
	if st.raw != nil {
		st.raw.Close()
	}
}

// read returns the next text message. Binary messages close with 1003;
// oversized ones were already closed with 1009 by the read limit.
func (st *nodeStream) read() ([]byte, bool) {
	typ, data, err := st.ws.Read(st.ctx)
	if err != nil {
		return nil, false
	}
	if typ != websocket.MessageText {
		st.terminate(closeUnsupported, "binary messages are not supported")
		return nil, false
	}
	return data, true
}

// write sends one encoded message with a streamStepTimeout deadline on the
// node clock; a blocked peer is canceled and closed.
func (st *nodeStream) write(typ, requestID string, body any) error {
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, requestID, body)
	if err != nil {
		return err
	}
	ctx, cancel := clockTimeout(st.ctx, st.svc.clock, streamStepTimeout)
	defer cancel()
	return st.ws.Write(ctx, websocket.MessageText, b)
}

// reject answers a protocol failure with a bounded error message (when the
// socket allows) and closes with 1008 and a fixed reason.
func (st *nodeStream) reject(requestID string, err error, reason string) {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		ce = contract.Wrap(contract.CodeInternal, "internal error", err)
	}
	if !contract.ValidRequestID(requestID) {
		requestID = invalidRequestID
	}
	if ce.Code == contract.CodeProtocolMismatch {
		remote, _ := ce.DetailInt("remote_version")
		st.svc.logger.Warn("protocol version mismatch", "local_version", contract.ProtocolVersion, "remote_version", remote, "path", contract.PathNodeStream)
	} else {
		st.svc.logger.Warn("node stream rejected", "reason", reason, "error", ce)
	}
	st.write(contract.FrameError, requestID, &contract.Error{Code: ce.Code, Message: ce.Message, Details: ce.Details})
	st.terminate(closePolicy, reason)
}

// serve runs the protocol: hello within streamStepTimeout, attach, then
// strictly sequenced heartbeats b1, b2, ... each answered after the lease
// update. It returns when the socket ends.
func (st *nodeStream) serve() {
	s := st.svc
	// The hello read itself carries the node-clock deadline, so the read's
	// own result decides: a frame that Read returned is always processed,
	// and only a read that returned no frame is a timeout. Each frame read
	// clears its cancellation before returning, so the deadline firing
	// after a successful read cannot close the socket, and no goroutine
	// outlives this read (clockTimeout's cancel is synchronous). A timed-out
	// read has already closed the socket, so no close frame can follow.
	ctx, cancel := clockTimeout(st.ctx, s.clock, streamStepTimeout)
	typ, data, err := st.ws.Read(ctx)
	if err == nil && s.helloRead != nil {
		s.helloRead(ctx)
	}
	// Sample the deadline before cancel, which always cancels ctx: only a
	// failed read whose own deadline had fired, while the stream itself is
	// still live, is a hello timeout (not a peer drop or a 1009 close).
	timedOut := err != nil && ctx.Err() != nil && st.ctx.Err() == nil
	cancel()
	if err != nil {
		if timedOut {
			s.logger.Warn("node stream rejected", "reason", "hello timeout")
			s.event("hello timeout")
		}
		s.event("hello failed")
		return
	}
	if typ != websocket.MessageText {
		st.terminate(closeUnsupported, "binary messages are not supported")
		return
	}
	f, err := contract.DecodeFrame(data, contract.FromSidecar)
	if err != nil {
		st.reject(f.RequestID, err, reasonFor(err))
		return
	}
	if f.Type != contract.FrameHello {
		st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "the first message must be hello"), "invalid message")
		return
	}
	hello, err := contract.DecodeHello(f.Body)
	if err != nil {
		st.reject(f.RequestID, err, "invalid message")
		return
	}
	gen, err := s.reg.Attach(hello.NodeID, hello.SoftwareVersion, contract.ProtocolVersion, func() { st.terminate(closePolicy, "lease expired") })
	if err != nil {
		st.reject(f.RequestID, err, reasonFor(err))
		return
	}
	defer func() {
		s.reg.Detach(hello.NodeID, gen)
		s.logger.Info("node disconnected", "node_id", hello.NodeID)
		s.event("detached " + hello.NodeID)
	}()
	if err := st.write(contract.FrameHelloOK, f.RequestID, contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS}); err != nil {
		return
	}
	s.logger.Info("node connected", "node_id", hello.NodeID, "software_version", hello.SoftwareVersion)
	s.event("attached " + hello.NodeID)
	for n := 1; ; n++ {
		data, ok := st.read()
		if !ok {
			return
		}
		f, err := contract.DecodeFrame(data, contract.FromSidecar)
		if err != nil {
			st.reject(f.RequestID, err, reasonFor(err))
			return
		}
		switch want := "b" + strconv.Itoa(n); {
		case f.Type == contract.FrameError:
			s.logger.Warn("node reported an error", "node_id", hello.NodeID)
			st.terminate(closePolicy, "peer error")
			return
		case f.Type != contract.FrameHeartbeat:
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "unexpected "+f.Type+" message after hello"), "invalid message")
			return
		case f.RequestID != want:
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "heartbeat request_id must be "+want), "invalid sequence")
			return
		}
		hb, err := contract.DecodeHeartbeat(f.Body)
		if err != nil {
			st.reject(f.RequestID, err, "invalid message")
			return
		}
		if err := s.reg.Heartbeat(hello.NodeID, gen, hb.Roles); err != nil {
			st.reject(f.RequestID, err, reasonFor(err))
			return
		}
		s.event("acking " + hello.NodeID + " " + f.RequestID)
		if err := st.write(contract.FrameHeartbeatAck, f.RequestID, nil); err != nil {
			return
		}
		s.event("ack " + hello.NodeID + " " + f.RequestID)
	}
}

// reasonFor is the fixed close reason for an error code.
func reasonFor(err error) string {
	switch contract.CodeOf(err) {
	case contract.CodeProtocolMismatch:
		return "protocol version mismatch"
	case contract.CodeNotFound:
		return "unknown node"
	case contract.CodeConflict:
		return "duplicate stream"
	case contract.CodeUnavailable:
		return "stream not current"
	}
	return "invalid message"
}
