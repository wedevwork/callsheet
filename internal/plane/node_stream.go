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
	// controlTimeout bounds one plane request (iteration 04): a snapshot
	// from slot reservation through write and acknowledgement, and a
	// validation from submission (waiting included) through its result.
	controlTimeout = 4 * time.Second
)

// connKey stores the accepted connection in each request's context
// (http.Server.ConnContext), so the stream handler can clear inherited
// deadlines and force-close a blocked socket.
type connKey struct{}

// readResult is one message (or the terminal read error) with the instant
// its read completed, captured by the reader before it is announced.
type readResult struct {
	typ  websocket.MessageType
	data []byte
	err  error
	at   time.Time
}

// inbox is the reader's bounded handoff to the session: one delivered
// message plus the one the blocked reader already completed. Each
// message's completion instant is sampled and published in the same
// critical section, so once the session finds the inbox empty after a
// deadline fired, any later message completed at or after it.
type inbox struct {
	clock   nodeClock
	mu      sync.Mutex
	item    *readResult
	pending *readResult
	ready   chan struct{}
	space   chan struct{}
}

func newInbox(clock nodeClock) *inbox {
	return &inbox{clock: clock, ready: make(chan struct{}, 1), space: make(chan struct{}, 1)}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// put publishes r, stamped now, and blocks while the inbox is full. It
// returns false if ctx ended first.
func (b *inbox) put(ctx context.Context, r readResult) bool {
	b.mu.Lock()
	r.at = b.clock.Now()
	if b.item == nil {
		b.item = &r
		b.mu.Unlock()
		signal(b.ready)
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

// take returns the next message, if any, promoting the pending one.
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
	signal(b.space)
	if more {
		signal(b.ready)
	}
	return r, true
}

// validation is one mutation's pending or in-flight role validation. done
// receives its outcome exactly once (nil is success).
type validation struct {
	cfg      contract.RoleConfig
	deadline time.Time
	// ctx is the caller's: a validation whose caller was canceled is never
	// dispatched, whoever notices first (the caller withdrawing it or the
	// session claiming the slot).
	ctx  context.Context
	done chan error
	once sync.Once
}

func (v *validation) complete(err error) { v.once.Do(func() { v.done <- err }) }

// planeRequest is the session's one in-flight plane request.
type planeRequest struct {
	id       string
	deadline time.Time
	timer    <-chan time.Time
	stop     func() bool
	expired  bool
	// Exactly one of v (role_validate) and snap (roles_replace) is used.
	v      *validation
	snap   roleSnap
	isSnap bool
}

// nodeStream is one upgraded node connection: the handler goroutine reads
// the hello, then a reader goroutine feeds the session goroutine (the
// handler), which alone owns protocol state and writes.
type nodeStream struct {
	svc *nodeService
	ws  *websocket.Conn
	raw net.Conn
	// ctx is canceled only to force the socket closed.
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	closed    chan struct{}

	nodeID string
	gen    uint64
	inbox  *inbox
	kick   chan struct{}

	// mu arbitrates the control slot with mutation handlers: the one
	// pending (not yet dispatched) validation and the snapshot-dirty
	// signal. The in-flight request itself is session state.
	mu     sync.Mutex
	ended  bool
	waiter *validation
	dirty  bool
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
	st := &nodeStream{svc: s, ws: ws, ctx: ctx, cancel: cancel, closed: make(chan struct{}), inbox: newInbox(s.clock), kick: make(chan struct{}, 1)}
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

// write sends one encoded message, bounded by streamStepTimeout on the
// node clock and further by the owning operation's deadline (zero: none).
// A blocked peer is canceled and closed.
func (st *nodeStream) write(typ, requestID string, body any, deadline time.Time) error {
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, requestID, body)
	if err != nil {
		return err
	}
	now := st.svc.clock.Now()
	at := now.Add(streamStepTimeout)
	if !deadline.IsZero() && deadline.Before(at) {
		at = deadline
	}
	if !at.After(now) {
		return context.DeadlineExceeded
	}
	ctx, cancel := clockDeadline(st.ctx, st.svc.clock, at)
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
	st.write(contract.FrameError, requestID, &contract.Error{Code: ce.Code, Message: ce.Message, Details: ce.Details}, time.Time{})
	st.terminate(closePolicy, reason)
}

// serve runs the protocol: hello within streamStepTimeout, attach, then the
// session until the socket ends.
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
	st.nodeID = hello.NodeID
	gen, err := s.reg.attach(hello.NodeID, hello.SoftwareVersion, contract.ProtocolVersion, func() { st.terminate(closePolicy, "lease expired") }, st)
	if err != nil {
		st.reject(f.RequestID, err, reasonFor(err))
		return
	}
	st.gen = gen
	// Nothing may queue behind a finished session: submissions fail from
	// here on, every waiting or in-flight validation is completed, and the
	// attachment is cleared.
	detach := func() {
		st.end()
		s.reg.Detach(hello.NodeID, gen)
		s.logger.Info("node disconnected", "node_id", hello.NodeID)
		s.event("detached " + hello.NodeID)
	}
	if err := st.write(contract.FrameHelloOK, f.RequestID, contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS}, time.Time{}); err != nil {
		detach()
		return
	}
	s.logger.Info("node connected", "node_id", hello.NodeID, "software_version", hello.SoftwareVersion)
	s.event("attached " + hello.NodeID)
	// deliver ends when the session does, so a reader holding a message
	// for a finished session stops instead of blocking.
	deliver, stopDelivery := context.WithCancel(st.ctx)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			typ, data, err := st.ws.Read(st.ctx)
			if !st.inbox.put(deliver, readResult{typ: typ, data: data, err: err}) || err != nil {
				return
			}
		}
	}()
	st.session()
	detach()
	stopDelivery()
	// Close (gracefully when the socket allows, else forced after the
	// grace); either ends a reader still blocked in Read. Join it.
	st.terminate(closeGoingAway, "stream closed")
	<-readerDone
}

// end marks the session over and completes its waiting validation.
func (st *nodeStream) end() {
	st.mu.Lock()
	st.ended = true
	v := st.waiter
	st.waiter = nil
	st.mu.Unlock()
	if v != nil {
		v.complete(disconnected(st.nodeID))
	}
}

func disconnected(node string) error {
	return contract.RoleError(contract.CodeUnavailable, "", node, "", contract.ReasonNodeDisconnected, "node %s disconnected during the role validation; nothing was changed, retry", node)
}

func validationTimeout(node string) error {
	return contract.RoleError(contract.CodeUnavailable, "", node, "", contract.ReasonValidationTimeout, "node %s did not complete the role validation within %v; nothing was changed, retry", node, controlTimeout)
}

// markDirty signals that a newer desired snapshot may exist.
func (st *nodeStream) markDirty() {
	st.mu.Lock()
	st.dirty = true
	st.mu.Unlock()
	signal(st.kick)
}

// validate submits cfg for validation by this node and waits for the
// outcome. deadline bounds the whole wait (behind a background snapshot
// included), write and reply and is never reset. A validation still
// waiting when its deadline passes or ctx ends is removed and returns
// unavailable/validation_timeout without sending anything; the stream
// stays attached. One already sent is decided by the session: by the
// reply's completion instant (strictly before the deadline) or, at expiry,
// by closing the stream.
func (st *nodeStream) validate(ctx context.Context, cfg contract.RoleConfig, deadline time.Time) error {
	clock := st.svc.clock
	v := &validation{cfg: cfg, deadline: deadline, ctx: ctx, done: make(chan error, 1)}
	st.mu.Lock()
	switch {
	case st.ended:
		st.mu.Unlock()
		return disconnected(st.nodeID)
	case st.waiter != nil:
		st.mu.Unlock()
		return contract.RoleError(contract.CodeUnavailable, "", st.nodeID, "", contract.ReasonBusy, "another role validation is waiting for node %s; retry", st.nodeID)
	}
	st.waiter = v
	st.mu.Unlock()
	st.svc.event("validate-queued " + st.nodeID)
	signal(st.kick)
	timer, stop := clock.NewTimerAt(deadline)
	defer stop()
	select {
	case err := <-v.done:
		return err
	case <-timer:
	case <-ctx.Done():
	}
	st.svc.event("validate-withdrawing " + st.nodeID)
	st.mu.Lock()
	if st.waiter == v {
		st.waiter = nil
		st.mu.Unlock()
		st.svc.event("validate-withdrawn " + st.nodeID)
		return validationTimeout(st.nodeID)
	}
	st.mu.Unlock()
	return <-v.done
}

// takeWaiter removes the pending validation for dispatch. The removal is
// under mu, like the caller's own withdrawal, so exactly one of them owns
// it. One whose caller was already canceled is completed
// (unavailable/validation_timeout) instead and nil is returned: it is
// never sent and the stream stays attached.
func (st *nodeStream) takeWaiter() *validation {
	st.mu.Lock()
	v := st.waiter
	st.waiter = nil
	st.mu.Unlock()
	if v != nil && st.canceled(v) {
		return nil
	}
	return v
}

// canceled completes v without sending it when its caller was canceled.
func (st *nodeStream) canceled(v *validation) bool {
	if v.ctx == nil || v.ctx.Err() == nil {
		return false
	}
	v.complete(validationTimeout(st.nodeID))
	st.svc.event("validate-canceled " + st.nodeID)
	return true
}

func (st *nodeStream) takeDirty() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	d := st.dirty
	st.dirty = false
	return d
}

// session owns the protocol after hello_ok: it handles every delivered
// message in order (heartbeats acknowledged at once), keeps at most one
// plane request in flight, gives a waiting mutation validation the free
// control slot before any dirty snapshot, and ends (closing the stream) on
// a protocol error, a transport failure or an in-flight request's expiry.
func (st *nodeStream) session() {
	s := st.svc
	st.markDirty() // the initial full snapshot, whatever its revision
	var inflight *planeRequest
	defer func() {
		if inflight != nil {
			inflight.stop()
			if inflight.v != nil {
				inflight.v.complete(disconnected(st.nodeID))
			}
		}
	}()
	p := 0        // plane request counter: p1, p2, ... per connection
	hb := 1       // next heartbeat number
	sent := false // whether any snapshot was sent in this session
	lastRev := 0  // revision of the last snapshot sent
	for {
		if r, ok := st.inbox.take(); ok {
			if !st.handle(r, &inflight, &hb) {
				return
			}
			continue
		}
		if inflight != nil && inflight.expired {
			// The deadline fired and no message completed before it.
			if inflight.v != nil {
				inflight.v.complete(validationTimeout(st.nodeID))
			}
			s.logger.Warn("node stream request timed out", "node_id", st.nodeID, "request_id", inflight.id)
			s.event("request-expired " + st.nodeID + " " + inflight.id)
			st.terminate(closePolicy, "request timeout")
			return
		}
		if inflight == nil {
			if v := st.takeWaiter(); v != nil {
				r, ok := st.dispatchValidation(v, &p)
				if !ok {
					return
				}
				inflight = r
				continue
			}
			if st.takeDirty() {
				snap, ok := s.reg.desiredFor(st.nodeID, st.gen)
				if ok && (!sent || snap.rev > lastRev) {
					r, ok := st.dispatchSnapshot(snap, &p)
					if !ok {
						return
					}
					inflight, sent, lastRev = r, true, snap.rev
				}
				continue
			}
		}
		var timer <-chan time.Time
		if inflight != nil && !inflight.expired {
			timer = inflight.timer
		}
		select {
		case <-st.inbox.ready:
		case <-st.kick:
		case <-timer:
			inflight.expired = true
		case <-st.ctx.Done():
			return
		}
	}
}

// nextID assigns the next plane request ID, or fails on exhaustion.
func (st *nodeStream) nextID(p *int) (string, bool) {
	if *p >= contract.MaxSafeInteger {
		st.svc.logger.Warn("node stream request counter exhausted", "node_id", st.nodeID)
		st.terminate(closePolicy, "request counter exhausted")
		return "", false
	}
	*p++
	return "p" + strconv.Itoa(*p), true
}

// arm starts the in-flight request's timer at its absolute deadline (one
// already passed fires at once).
func (st *nodeStream) arm(r *planeRequest) *planeRequest {
	r.timer, r.stop = st.svc.clock.NewTimerAt(r.deadline)
	return r
}

// dispatchValidation sends a waiting validation after rechecking its
// deadline, the attachment generation and the lease. A validation that
// fails a recheck is completed without being sent, and the stream stays
// attached. ok=false ends the session (the write failed).
func (st *nodeStream) dispatchValidation(v *validation, p *int) (*planeRequest, bool) {
	s := st.svc
	if !s.clock.Now().Before(v.deadline) {
		v.complete(validationTimeout(st.nodeID))
		s.event("validate-expired " + st.nodeID)
		return nil, true
	}
	if err := s.reg.stillTarget(st.nodeID, st.gen); err != nil {
		v.complete(err)
		return nil, true
	}
	if st.canceled(v) { // recheck immediately before the write
		return nil, true
	}
	id, ok := st.nextID(p)
	if !ok {
		v.complete(disconnected(st.nodeID))
		return nil, false
	}
	r := &planeRequest{id: id, deadline: v.deadline, v: v}
	s.event("validate-writing " + st.nodeID + " " + id)
	if err := st.write(contract.FrameRoleValidate, id, contract.RoleValidateBody{Role: v.cfg.Resolved()}, v.deadline); err != nil {
		if !s.clock.Now().Before(v.deadline) {
			v.complete(validationTimeout(st.nodeID))
		} else {
			v.complete(disconnected(st.nodeID))
		}
		return nil, false
	}
	s.event("validate-sent " + st.nodeID + " " + id)
	return st.arm(r), true
}

// dispatchSnapshot reserves the slot for snap, with its own controlTimeout
// from now through write and acknowledgement.
func (st *nodeStream) dispatchSnapshot(snap roleSnap, p *int) (*planeRequest, bool) {
	s := st.svc
	id, ok := st.nextID(p)
	if !ok {
		return nil, false
	}
	r := &planeRequest{id: id, deadline: s.clock.Now().Add(controlTimeout), snap: snap, isSnap: true}
	s.event("replace-writing " + st.nodeID + " " + id)
	if err := st.write(contract.FrameRolesReplace, id, contract.RolesReplaceBody{Revision: snap.rev, Roles: snap.roles}, r.deadline); err != nil {
		return nil, false
	}
	s.event("replace-sent " + st.nodeID + " " + id + " " + strconv.Itoa(snap.rev))
	return st.arm(r), true
}

// handle processes one delivered message; false ends the session.
func (st *nodeStream) handle(r readResult, inflight **planeRequest, hb *int) bool {
	s := st.svc
	if r.err != nil {
		return false
	}
	if r.typ != websocket.MessageText {
		st.terminate(closeUnsupported, "binary messages are not supported")
		return false
	}
	f, err := contract.DecodeFrame(r.data, contract.FromSidecar)
	if err != nil {
		st.reject(f.RequestID, err, reasonFor(err))
		return false
	}
	switch f.Type {
	case contract.FrameError:
		s.logger.Warn("node reported an error", "node_id", st.nodeID)
		st.terminate(closePolicy, "peer error")
		return false
	case contract.FrameHeartbeat:
		want := "b" + strconv.Itoa(*hb)
		if f.RequestID != want {
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "heartbeat request_id must be "+want), "invalid sequence")
			return false
		}
		body, err := contract.DecodeHeartbeat(f.Body)
		if err != nil {
			st.reject(f.RequestID, err, "invalid message")
			return false
		}
		if err := s.reg.heartbeat(st.nodeID, st.gen, body); err != nil {
			st.reject(f.RequestID, err, reasonFor(err))
			return false
		}
		s.event("acking " + st.nodeID + " " + f.RequestID)
		if err := st.write(contract.FrameHeartbeatAck, f.RequestID, nil, time.Time{}); err != nil {
			return false
		}
		s.event("ack " + st.nodeID + " " + f.RequestID)
		*hb++
		return true
	case contract.FrameRoleValidateResult, contract.FrameRolesReplaceAck:
		req := *inflight
		if req == nil || req.id != f.RequestID || req.isSnap != (f.Type == contract.FrameRolesReplaceAck) {
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "unsolicited, stale or mismatched "+f.Type+" "+f.RequestID), "invalid sequence")
			return false
		}
		if req.isSnap {
			rev, err := contract.DecodeRolesReplaceAck(f.Body)
			if err == nil && rev != req.snap.rev {
				err = contract.New(contract.CodeInvalidArgument, "roles_replace_ack names revision "+strconv.Itoa(rev)+"; want "+strconv.Itoa(req.snap.rev))
			}
			if err != nil {
				st.reject(f.RequestID, err, "invalid message")
				return false
			}
		}
		var result *contract.Error
		if !req.isSnap {
			if result, err = contract.DecodeRoleValidateResult(f.Body); err != nil {
				st.reject(f.RequestID, err, "invalid message")
				return false
			}
		}
		// Success only if the reply completed strictly before the deadline.
		if !r.at.Before(req.deadline) {
			req.expired = true
			return true
		}
		req.stop()
		*inflight = nil
		if req.isSnap {
			s.reg.ackSnapshot(st.nodeID, st.gen, req.snap)
			s.event("replace-acked " + st.nodeID + " " + f.RequestID + " " + strconv.Itoa(req.snap.rev))
			return true
		}
		s.event("validate-result " + st.nodeID + " " + f.RequestID)
		req.v.complete(validationOutcome(st.nodeID, req.v.cfg.ID, result))
		return true
	}
	st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "unexpected "+f.Type+" message after hello"), "invalid message")
	return false
}

// validationOutcome maps a sidecar's validation result to the mutation's
// error, forwarding only known safe detail values.
func validationOutcome(node, role string, e *contract.Error) error {
	if e == nil {
		return nil
	}
	field, _ := e.Details["field"].(string)
	switch field {
	case "id", "name", "node", "adapter", "instruction", "runbook", "model", "effort", "concurrency", "timeout":
	default:
		field = ""
	}
	reason, _ := e.Details["reason"].(string)
	switch reason {
	case contract.ReasonBusy, contract.ReasonValidationTimeout, contract.ReasonManualUnreadable, contract.ReasonManualNotRegular,
		contract.ReasonAdapterDisabled, contract.ReasonProbeFailed:
	default:
		reason = ""
	}
	return contract.RoleError(e.Code, role, node, field, reason, "node %s rejected role %s: %s", node, role, e.Message)
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
