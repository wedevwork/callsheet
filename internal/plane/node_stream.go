package plane

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
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
	// Exactly one of v (role_validate), snap (roles_replace), start
	// (task_start, iteration 05), recon (a task_reconcile page, iteration
	// 06a) and ctl (a task_cancel, iteration 06b) is used.
	v      *validation
	snap   roleSnap
	isSnap bool
	start  *startItem
	recon  bool
	final  bool
	ctl    *controlItem
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
	// Task starts (iteration 05): committed starts queued for this
	// attachment, oldest first, and the transport tokens reserved by
	// admissions (queued, not yet queued or being exchanged), at most
	// maxQueuedStarts.
	starts []*startItem
	tokens int
	// recon is this attachment's reconciliation (iteration 06a), session
	// state: no roles_replace, role_validate or task_start is sent before
	// its final reconcile page is acknowledged.
	recon *reconciliation
	// prep is the task_prepared awaiting its durable decision (iteration
	// 10b), session state.
	prep *prepWait
}

// prepWait is one task_prepared whose acknowledgement waits for a
// confirmed publication.
type prepWait struct {
	id   string
	body contract.TaskPreparedBody
	wait <-chan struct{}
}

// ackPrepared writes the release decision; false ends the session.
func (st *nodeStream) ackPrepared(rid string, b contract.TaskPreparedBody, release bool) bool {
	ack := contract.TaskPreparedAckBody{TaskID: b.TaskID, Execution: b.Execution, Release: release}
	if err := st.write(contract.FrameTaskPreparedAck, rid, ack, time.Time{}); err != nil {
		return false
	}
	if release {
		st.svc.event("prepared-released " + st.nodeID + " " + rid + " " + b.TaskID)
	} else {
		st.svc.event("prepared-refused " + st.nodeID + " " + rid + " " + b.TaskID)
	}
	return true
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
	st := &nodeStream{svc: s, ws: ws, ctx: ctx, cancel: cancel, closed: make(chan struct{}), inbox: newInbox(s.clock), kick: make(chan struct{}, 1),
		recon: newReconciliation()}
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
	// outlives this read (clockTimeout's cancel is synchronous). A timeout
	// closes the socket without a close frame. A read already waiting when
	// the deadline fires closes it itself, but a deadline that fired before
	// the read began can return with the socket still open, so the timeout
	// path closes it (CloseNow, which also marks it closing: handleStream's
	// terminate then sends nothing).
	ctx, cancel := clockTimeout(st.ctx, s.clock, streamStepTimeout)
	if s.helloArmed != nil {
		s.helloArmed(ctx)
	}
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
			st.ws.CloseNow()
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
		s.tasks.detached(hello.NodeID, gen)
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

// end marks the session over and completes its waiting validation; every
// start still queued was never written and is rejected as unsent.
func (st *nodeStream) end() {
	st.mu.Lock()
	st.ended = true
	v := st.waiter
	st.waiter = nil
	queued := st.starts
	st.starts = nil
	st.mu.Unlock()
	if v != nil {
		v.complete(disconnected(st.nodeID))
	}
	for _, it := range queued {
		it.res.release()
		st.svc.tasks.startUnsent(it.id)
	}
}

// startReservation is one admission's transport token on an attachment.
// The admission owns it until its start is committed; then the task entry
// and its queued start carry it. Whichever path ends the start (an
// admission failure, a definitely-unsent start, a reply, an ambiguous or
// failed write) releases it; release is exactly once. The task entry keeps
// its reservation for its lifetime, so a released one holds no stream: a
// retired attachment (socket, connection, inbox) is never kept reachable.
type startReservation struct {
	st atomic.Pointer[nodeStream]
}

func newReservation(st *nodeStream) *startReservation {
	r := &startReservation{}
	r.st.Store(st)
	return r
}

// release returns the token and drops the stream; later calls do
// nothing (the swap makes it exactly once).
func (r *startReservation) release() {
	if r == nil {
		return
	}
	if st := r.st.Swap(nil); st != nil {
		st.mu.Lock()
		st.tokens--
		st.mu.Unlock()
	}
}

// reserveStart reserves one transport token for an admission, before its
// publication: at most maxQueuedStarts starts per attachment are pending.
// It returns nil when the queue is full or the session ended.
func (st *nodeStream) reserveStart() *startReservation {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ended || st.tokens >= maxQueuedStarts {
		return nil
	}
	st.tokens++
	return newReservation(st)
}

// enqueueStart queues a committed start; false when the session ended
// (the start was never sent).
func (st *nodeStream) enqueueStart(it *startItem) bool {
	st.mu.Lock()
	if st.ended {
		st.mu.Unlock()
		return false
	}
	st.starts = append(st.starts, it)
	st.mu.Unlock()
	st.svc.event("start-queued " + st.nodeID + " " + it.id)
	signal(st.kick)
	return true
}

// takeStart removes the oldest queued start for dispatch; starts whose
// deadline passed while queued are withdrawn as unsent (the stream stays
// attached). next is the oldest remaining deadline (zero when none).
func (st *nodeStream) takeStart(now time.Time, dispatch bool) (*startItem, time.Time) {
	st.mu.Lock()
	var expired []*startItem
	for len(st.starts) > 0 && !now.Before(st.starts[0].deadline) {
		expired = append(expired, st.starts[0])
		st.starts = st.starts[1:]
	}
	var it *startItem
	if dispatch && len(st.starts) > 0 {
		it = st.starts[0]
		st.starts = st.starts[1:]
	}
	var next time.Time
	if len(st.starts) > 0 {
		next = st.starts[0].deadline
	}
	st.mu.Unlock()
	for _, e := range expired {
		e.res.release()
		st.svc.event("start-expired " + st.nodeID + " " + e.id)
		st.svc.tasks.startUnsent(e.id)
	}
	return it, next
}

func (st *nodeStream) hasStarts() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.starts) > 0
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
	var inflight *planeRequest
	defer func() {
		if inflight != nil {
			inflight.stop()
			if inflight.v != nil {
				inflight.v.complete(disconnected(st.nodeID))
			}
			if inflight.start != nil {
				// The write began and no conclusive reply arrived.
				s.tasks.startUncertain(inflight.start.id)
				inflight.start.res.release()
			}
			if inflight.ctl != nil {
				// Not received here: the next attachment's reconciliation
				// latches the durable intent (stop_control).
				s.tasks.controlDone(inflight.ctl.id, false, time.Time{})
			}
		}
	}()
	p := 0        // plane request counter: p1, p2, ... per connection
	q := 0        // reconcile page counter: r1, r2, ... per connection
	b := 1        // next sidecar request number (heartbeat, task_log, task_result)
	sent := false // whether any snapshot was sent in this session
	lastRev := 0  // revision of the last snapshot sent
	// preferStart alternates a snapshot and task work (a start or a
	// control) when both have work; preferControl alternates controls and
	// starts, one control at most before other work is checked again.
	preferStart := false
	preferControl := true
	lastCtl := ""
	var queueTimer <-chan time.Time
	var queueAt time.Time
	stopQueue := func() bool { return false }
	defer func() { stopQueue() }()
	var ctlTimer <-chan time.Time
	var ctlAt time.Time
	stopCtl := func() bool { return false }
	defer func() { stopCtl() }()
	for {
		if r, ok := st.inbox.take(); ok {
			if !st.handle(r, &inflight, &b) {
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
		// Queued starts whose deadline passed are withdrawn as unsent;
		// the next one's deadline is watched (re-armed only on change).
		if _, next := st.takeStart(s.clock.Now(), false); !next.Equal(queueAt) {
			stopQueue()
			queueTimer, stopQueue, queueAt = nil, func() bool { return false }, next
			if !next.IsZero() {
				queueTimer, stopQueue = s.clock.NewTimerAt(next)
			}
		}
		if inflight == nil && st.recon.final && !st.recon.done {
			// Reconciliation first (attachment order DW5): every reconcile
			// page, one at a time, before any role or task request.
			r, ok := st.dispatchReconcile(&q)
			if !ok {
				return
			}
			inflight = r
			continue
		}
		if inflight == nil && st.recon.done {
			if v := st.takeWaiter(); v != nil {
				r, ok := st.dispatchValidation(v, &p)
				if !ok {
					return
				}
				inflight = r
				continue
			}
			dirty := st.peekDirty()
			starts := st.hasStarts()
			var ctl *controlItem
			if preferControl || !starts {
				var next time.Time
				ctl, next = s.tasks.takeControl(st.nodeID, st.gen, s.clock.Now(), lastCtl)
				if !next.Equal(ctlAt) {
					stopCtl()
					ctlTimer, stopCtl, ctlAt = nil, func() bool { return false }, next
					if !next.IsZero() {
						ctlTimer, stopCtl = s.clock.NewTimerAt(next)
					}
				}
			}
			if ctl != nil && (preferStart || !dirty) {
				preferStart, preferControl, lastCtl = false, false, ctl.id
				r, ok := st.dispatchControl(ctl, &p)
				if !ok {
					return
				}
				inflight = r
				continue
			}
			if ctl != nil {
				// A snapshot goes first: the control is taken again next pass.
				s.tasks.controlDone(ctl.id, false, time.Time{})
			}
			if starts && (preferStart || !dirty) {
				preferStart, preferControl = false, true
				it, _ := st.takeStart(s.clock.Now(), true)
				if it != nil {
					r, ok := st.dispatchStart(it, &p)
					if !ok {
						return
					}
					inflight = r
				}
				continue
			}
			if st.takeDirty() {
				preferStart = true
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
		var prepC <-chan struct{}
		if st.prep != nil {
			prepC = st.prep.wait
		}
		select {
		case <-prepC:
			// A confirmed publication: decide the pending task_prepared again
			// (a durable authorization releases it).
			p := st.prep
			wait, release := s.tasks.preparedRecheck(p.body)
			if wait != nil {
				p.wait = wait
				continue
			}
			st.prep = nil
			if !st.ackPrepared(p.id, p.body, release) {
				return
			}
		case <-st.inbox.ready:
		case <-st.kick:
		case <-timer:
			inflight.expired = true
		case <-queueTimer:
			queueTimer, queueAt = nil, time.Time{}
		case <-ctlTimer:
			ctlTimer, ctlAt = nil, time.Time{}
		case <-st.ctx.Done():
			return
		}
	}
}

// dispatchReconcile sends the next reconcile page with its own
// controlTimeout from now through write and acknowledgement.
func (st *nodeStream) dispatchReconcile(q *int) (*planeRequest, bool) {
	s := st.svc
	rc := st.recon
	if *q >= contract.MaxSafeInteger {
		st.terminate(closePolicy, "request counter exhausted")
		return nil, false
	}
	*q++
	page := rc.pages[rc.acked]
	r := &planeRequest{id: "r" + strconv.Itoa(*q), deadline: s.clock.Now().Add(controlTimeout), recon: true, final: page.Final}
	s.event("reconcile-writing " + st.nodeID + " " + r.id)
	if err := st.write(contract.FrameTaskReconcile, r.id, page, r.deadline); err != nil {
		return nil, false
	}
	s.event("reconcile-sent " + st.nodeID + " " + r.id)
	return st.arm(r), true
}

// peekDirty reports a pending snapshot signal without consuming it.
func (st *nodeStream) peekDirty() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.dirty
}

// dispatchStart sends a committed task's sole start attempt after
// rechecking its never-reset deadline and that it is still queued. A
// start that fails a recheck before any write is unsent; once the write
// began, only a conclusive reply decides (ok=false ends the session).
func (st *nodeStream) dispatchStart(it *startItem, p *int) (*planeRequest, bool) {
	s := st.svc
	if !s.clock.Now().Before(it.deadline) {
		it.res.release()
		s.tasks.startUnsent(it.id)
		return nil, true
	}
	if !s.tasks.claimStart(it.id) {
		it.res.release()
		return nil, true
	}
	id, ok := st.nextID(p)
	if !ok {
		it.res.release()
		s.tasks.startUncertain(it.id)
		return nil, false
	}
	r := &planeRequest{id: id, deadline: it.deadline, start: it}
	s.event("start-writing " + st.nodeID + " " + id + " " + it.id)
	if err := st.write(contract.FrameTaskStart, id, it.body, it.deadline); err != nil {
		it.res.release()
		s.tasks.startUncertain(it.id)
		return nil, false
	}
	s.event("start-sent " + st.nodeID + " " + id + " " + it.id)
	return st.arm(r), true
}

// dispatchControl sends a durable stop intent's task_cancel with its own
// controlTimeout from now through write and acknowledgement, under the
// attachment's single p<n> counter. ok=false ends the session.
func (st *nodeStream) dispatchControl(it *controlItem, p *int) (*planeRequest, bool) {
	s := st.svc
	id, ok := st.nextID(p)
	if !ok {
		s.tasks.controlDone(it.id, false, time.Time{})
		return nil, false
	}
	r := &planeRequest{id: id, deadline: s.clock.Now().Add(controlTimeout), ctl: it}
	s.event("cancel-writing " + st.nodeID + " " + id + " " + it.id)
	if err := st.write(contract.FrameTaskCancel, id, it.body, r.deadline); err != nil {
		s.tasks.controlDone(it.id, false, time.Time{})
		return nil, false
	}
	s.event("cancel-sent " + st.nodeID + " " + id + " " + it.id)
	return st.arm(r), true
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
	return st.handleFrame(r, inflight, hb)
}

// nextSidecarID checks a sidecar request's ID against the single b1, b2,
// ... sequence shared by heartbeats, task_log and task_result.
func (st *nodeStream) nextSidecarID(f contract.NodeFrame, b *int) bool {
	want := "b" + strconv.Itoa(*b)
	if f.RequestID != want {
		st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, f.Type+" request_id must be "+want), "invalid sequence")
		return false
	}
	return true
}

func (st *nodeStream) handleFrame(r readResult, inflight **planeRequest, hb *int) bool {
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
		// A worker without the execution a control named (iteration 06b,
		// unavailable/execution_missing) ends this attachment: the next one
		// reconciles, and only its complete inventory can resolve the task
		// lost; the error itself is never cleanup success.
		reason := ""
		if e, err := contract.ParseErrorBody(f.Body); err == nil {
			reason = errReason(e)
		}
		s.logger.Warn("node reported an error", "node_id", st.nodeID, "reason", contract.SafeText(reason, 64))
		s.event("peer-error " + st.nodeID + " " + reason)
		st.terminate(closePolicy, "peer error")
		return false
	case contract.FrameTaskLog:
		if !st.nextSidecarID(f, hb) {
			return false
		}
		body, err := contract.DecodeTaskLog(f.Body)
		if err == nil {
			var next int
			if st.recon.unknown[body.TaskID] {
				// An execution unknown to this plane: acknowledged, never
				// recorded.
				next = body.Offset + len(body.Data)
				s.event("unknown-output " + st.nodeID + " " + body.TaskID)
			} else {
				next, err = s.tasks.receiveLog(st.nodeID, st.gen, body)
			}
			if err == nil {
				s.event("log-received " + st.nodeID + " " + f.RequestID + " " + body.TaskID)
				if err := st.write(contract.FrameTaskLogAck, f.RequestID, contract.TaskLogAckBody{TaskID: body.TaskID, NextOffset: next}, time.Time{}); err != nil {
					return false
				}
				s.event("log-acked " + st.nodeID + " " + f.RequestID)
				*hb++
				return true
			}
		}
		st.reject(f.RequestID, err, "invalid message")
		return false
	case contract.FrameTaskResult:
		if !st.nextSidecarID(f, hb) {
			return false
		}
		body, err := contract.DecodeTaskResult(f.Body)
		if err == nil {
			var committed bool
			if st.recon.unknown[body.TaskID] {
				// Its outcome is received, never committed: the worker keeps
				// its journal as local evidence.
				s.event("unknown-output " + st.nodeID + " " + body.TaskID)
			} else {
				committed, err = s.tasks.receiveResult(st.nodeID, st.gen, body)
			}
			if err == nil {
				s.event("result-acking " + st.nodeID + " " + f.RequestID + " " + body.TaskID)
				ack := contract.TaskResultAckBody{TaskID: body.TaskID, Digest: body.Digest, Received: true, Committed: committed}
				if err := st.write(contract.FrameTaskResultAck, f.RequestID, ack, time.Time{}); err != nil {
					return false
				}
				s.event("result-acked " + st.nodeID + " " + f.RequestID)
				if committed {
					s.event("result-committed-acked " + st.nodeID + " " + f.RequestID)
				}
				*hb++
				return true
			}
		}
		st.reject(f.RequestID, err, "invalid message")
		return false
	case contract.FrameTaskPrepared:
		// Iteration 10b: a workspace start's preparation outcome, a sidecar
		// request in the b sequence. Its acknowledgement waits for the
		// durable running authorization without blocking the session: the
		// session loop answers once the writer confirms it.
		if !st.nextSidecarID(f, hb) {
			return false
		}
		body, err := contract.DecodeTaskPrepared(f.Body)
		var wait <-chan struct{}
		var release bool
		if err == nil {
			wait, release, err = s.tasks.prepared(st.nodeID, st.gen, body)
		}
		if err != nil {
			st.reject(f.RequestID, err, "invalid message")
			return false
		}
		*hb++
		if wait != nil {
			st.prep = &prepWait{id: f.RequestID, body: body, wait: wait}
			s.event("prepared-waiting " + st.nodeID + " " + f.RequestID)
			return true
		}
		return st.ackPrepared(f.RequestID, body, release)
	case contract.FrameTaskInventory:
		// Inventory pages carry their own strict sequence i1, i2, ...
		// (page 0, 1, ...), in the sidecar's one request slot.
		want := "i" + strconv.Itoa(st.recon.next+1)
		if f.RequestID != want {
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, f.Type+" request_id must be "+want), "invalid sequence")
			return false
		}
		body, err := contract.DecodeTaskInventory(f.Body)
		if err == nil {
			err = s.tasks.inventory(st.nodeID, st.gen, body, st.recon)
		}
		if err != nil {
			st.reject(f.RequestID, err, "invalid message")
			return false
		}
		if err := st.write(contract.FrameTaskInventoryAck, f.RequestID, contract.TaskInventoryAckBody{Page: body.Page, Received: true}, time.Time{}); err != nil {
			return false
		}
		s.event("inventory-acked " + st.nodeID + " " + f.RequestID)
		return true
	case contract.FrameTaskReconcileAck:
		req := *inflight
		if req == nil || !req.recon || req.id != f.RequestID {
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "unsolicited, stale or mismatched "+f.Type+" "+f.RequestID), "invalid sequence")
			return false
		}
		if _, err := contract.DecodeTaskReconcileAck(f.Body); err != nil {
			st.reject(f.RequestID, err, "invalid message")
			return false
		}
		if !r.at.Before(req.deadline) {
			req.expired = true
			return true
		}
		req.stop()
		*inflight = nil
		st.recon.acked++
		s.event("reconcile-acked " + st.nodeID + " " + f.RequestID)
		if req.final {
			st.recon.done = true
			s.reg.markReconciled(st.nodeID, st.gen)
			s.event("reconciled " + st.nodeID)
			st.markDirty() // the initial full snapshot, whatever its revision
		}
		return true
	case contract.FrameTaskCancelAck:
		req := *inflight
		if req == nil || req.ctl == nil || req.id != f.RequestID {
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "unsolicited, stale or mismatched "+f.Type+" "+f.RequestID), "invalid sequence")
			return false
		}
		a, err := contract.DecodeTaskCancelAck(f.Body)
		if err == nil && (a.TaskID != req.ctl.body.TaskID || a.Execution != req.ctl.body.Execution || a.StopID != req.ctl.body.StopID) {
			err = contract.New(contract.CodeInvalidArgument, "task_cancel_ack names another task, execution or stop")
		}
		if err != nil {
			st.reject(f.RequestID, err, "invalid message")
			return false
		}
		if !r.at.Before(req.deadline) {
			req.expired = true
			return true
		}
		req.stop()
		*inflight = nil
		s.tasks.controlDone(req.ctl.id, true, r.at)
		s.event("cancel-acked " + st.nodeID + " " + f.RequestID + " " + req.ctl.id)
		return true
	case contract.FrameTaskStartResult:
		req := *inflight
		if req == nil || req.start == nil || req.id != f.RequestID {
			st.reject(f.RequestID, contract.New(contract.CodeInvalidArgument, "unsolicited, stale or mismatched "+f.Type+" "+f.RequestID), "invalid sequence")
			return false
		}
		res, err := contract.DecodeTaskStartResult(f.Body)
		if err == nil && res.TaskID != req.start.id {
			err = contract.New(contract.CodeInvalidArgument, "task_start_result names another task")
		}
		if err == nil && res.Preparing && req.start.body.Workspace == nil {
			// Only a workspace start answers preparing (protocol 6).
			err = contract.New(contract.CodeInvalidArgument, "task_start_result preparing answers a start without a workspace")
		}
		if err != nil {
			st.reject(f.RequestID, err, "invalid message")
			return false
		}
		// Only a reply completed strictly before the deadline counts.
		if !r.at.Before(req.deadline) {
			req.expired = true
			return true
		}
		req.stop()
		*inflight = nil
		s.tasks.startReplied(req.start.id, st.gen, res.Err, res.Preparing)
		req.start.res.release()
		s.event("start-result " + st.nodeID + " " + f.RequestID)
		return true
	case contract.FrameHeartbeat:
		if !st.nextSidecarID(f, hb) {
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
		if req == nil || req.start != nil || req.recon || req.ctl != nil || req.id != f.RequestID || req.isSnap != (f.Type == contract.FrameRolesReplaceAck) {
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
