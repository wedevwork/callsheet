// Package mcp is the coordinator door (iteration 07a): the local stdio
// MCP server behind "callsheet mcp". It speaks the bounded MCP 2025-06-18
// subset on the standard library (newline-delimited JSON-RPC 2.0,
// initialization, tools/list and tools/call, ping and cancellation) and
// relays the thirteen operator tools and (iteration 09a) the eight
// workspace tools to the verified plane client. It is a
// stateless relay: registry and task truth stay on the plane, nothing is
// cached between calls, and stdout carries the protocol only.
//
// Ownership: one reader goroutine owns stdin, one writer goroutine owns
// stdout (every frame is written serially in chunks), and the session's
// terminal state (its exit code) is decided once, by end.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Protocol bounds.
const (
	// ProtocolVersion is the only supported MCP revision.
	ProtocolVersion = "2025-06-18"
	// MaxLineBytes bounds one inbound line, its newline included.
	MaxLineBytes = 512 << 10
	// MaxDepth bounds the nesting of inbound objects and arrays.
	MaxDepth = 64
	// MaxIDBytes bounds a string request ID.
	MaxIDBytes = 128
	// maxSafeInteger bounds an integer request ID: ±(2^53-1).
	maxSafeInteger = 1<<53 - 1
	// MaxFrameBytes bounds every outgoing frame, its newline included.
	MaxFrameBytes = 100 << 20
	// MaxErrorFrameBytes bounds a tool error frame; a larger one is
	// replaced with the fixed internal error.
	MaxErrorFrameBytes = 512 << 10
	// MaxActiveCalls is the number of tools/call requests served at once.
	MaxActiveCalls = 2
	// maxQueuedControl bounds the small control replies waiting for the
	// writer (tool results are bounded by MaxActiveCalls).
	maxQueuedControl = 8
	// WriteChunk is the largest single write to stdout.
	WriteChunk = 4 << 10
	// WriteIdle is the writer's progress timeout: a frame must make some
	// progress within it, however long the whole frame takes.
	WriteIdle = time.Second
	// maxNotes bounds repeated stderr diagnostics of one kind.
	maxNotes = 16
)

// Exit codes of a session.
const (
	ExitOK = contract.ExitOK
	// ExitFraming ends a session on fatal input framing (an oversized or
	// unterminated line): contract.ExitCode(invalid_argument).
	ExitFraming = 2
	// ExitOutput ends a session on broken, stalled or undeliverable
	// output: contract.ExitCode(unavailable).
	ExitOutput = 5
	// ExitInterrupted ends a session on SIGINT or SIGTERM.
	ExitInterrupted = contract.ExitInterrupted
)

// Clock is the session's time source; tests inject a fake clock whose
// timers fire only when it is advanced.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// RealClock is the production clock.
var RealClock Clock = realClock{}

// Config is one session's configuration and seams.
type Config struct {
	// Factory returns a fresh verified client per admitted tool call.
	Factory Factory
	// Budget is the outer waiting-call budget B (MinBudget..MaxBudget).
	Budget time.Duration
	// Version is serverInfo.version (cli.Version).
	Version string
	// Hostname supplies the coordinator hostname for requested_by.
	Hostname func() (string, error)
	// Clock times budgets and the writer's progress timeout.
	Clock Clock
	// In and Out are the owned stdio streams; Stderr takes diagnostics.
	In     io.Reader
	Out    io.Writer
	Stderr io.Writer
	// CloseIn and CloseOut close the owned streams to unblock a pending
	// Read or Write when the session ends. A nil closer marks a stream
	// that cannot be unblocked: its goroutine is not joined.
	CloseIn  func() error
	CloseOut func() error
	// Hook observes internal events (tests only; nil in production).
	Hook func(stage, id string)
}

// Hook stages.
const (
	StageAdmitted = "admitted"  // a tools/call took a slot
	StageTrust    = "trust"     // the call asks the factory for a client
	StageDecoded  = "decoded"   // the tool's client operation returned
	StageQueue    = "queue"     // immediately before the final recheck and queueing
	StageArmed    = "armed"     // a waiting call's budget timers are registered
	StageReleased = "released"  // a call's slot was released
	StageWritten  = "written"   // a frame was written whole and the progress timer disarmed
	StageDeadline = "deadline"  // a waiting call's delivery deadline D fired (before it is judged)
	StageFlushed  = "flushed"   // a frame was written and judged (before its other bookkeeping)
	StageTimed    = "timed"     // a waiting call's budget timers stopped (its deadlines judged)
	StageInitWait = "init-wait" // notifications/initialized waits for the initialize answer's emission
	StageSampled  = "sampled"   // a frame's final Write returned and its completion instant was sampled
	StageLate     = "late"      // the writer judged its frame's completion late (the session ends)
	StageExpired  = "expired"   // a waiting call's client deadline D-R fired and was handled
	StageFinal    = "final"     // a Write that can finish a call's answer begins
)

// Serve runs one MCP session until EOF (0), SIGINT/SIGTERM through ctx
// (130), fatal framing (2) or broken, stalled or undeliverable output (5),
// and returns that exit code after closing the owned streams and joining
// every goroutine it can unblock. Process exit never cancels a task,
// removes a role or stops the plane.
func Serve(ctx context.Context, cfg Config) int { return newSession(cfg).serve(ctx) }

func (s *session) serve(ctx context.Context) int {
	s.wg.Add(1)
	go s.watchdog()
	go s.writer()
	go s.reader()
	select {
	case <-ctx.Done():
		s.end(ExitInterrupted, "")
	case <-s.endCh:
	}
	s.wg.Wait()
	if s.cfg.CloseOut != nil {
		<-s.writerDone
	}
	if s.cfg.CloseIn != nil {
		<-s.readerDone
	}
	return s.code
}

// Session states.
const (
	stateUninitialized = iota
	stateInitQueued    // the initialize answer is queued or being written
	stateInitialized   // the initialize answer was emitted; awaiting notifications/initialized
	stateReady
)

type session struct {
	cfg    Config
	clock  Clock
	budget Budget
	tools  []*tool
	list   []byte // the tools/list result
	stderr *lockedWriter

	ctx    context.Context
	cancel context.CancelFunc

	endOnce sync.Once
	endCh   chan struct{}
	code    int

	wg         sync.WaitGroup // watchdog, handlers and call timers
	writerDone chan struct{}
	readerDone chan struct{}

	// mu guards the lifecycle, attribution, reservations and slots.
	mu            sync.Mutex
	closing       bool
	state         int
	clientName    string
	clientVersion string
	reserved      map[string]bool
	calls         map[string]*call
	active        int
	// initEmitted is closed once the initialize answer's last byte was
	// written (the state then leaves stateInitQueued).
	initEmitted chan struct{}

	// wmu guards the writer queue. Lock order: a call's mu, then mu or
	// wmu; wmu is never held while taking another lock.
	wmu      sync.Mutex
	queue    []*frame
	nControl int
	wake     chan struct{}

	// The writer's progress watchdog: arm commands are acknowledged once
	// applied, so a timer never outlives the write it guards.
	wd       chan bool
	wdAck    chan struct{}
	progress atomic.Uint64

	notes atomic.Int32
}

func newSession(cfg Config) *session {
	if cfg.Clock == nil {
		cfg.Clock = RealClock
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if cfg.Budget == 0 {
		cfg.Budget = DefaultBudget
	}
	s := &session{cfg: cfg, clock: cfg.Clock, budget: NewBudget(cfg.Budget), stderr: &lockedWriter{w: cfg.Stderr},
		endCh: make(chan struct{}), writerDone: make(chan struct{}), readerDone: make(chan struct{}),
		reserved: map[string]bool{}, calls: map[string]*call{}, initEmitted: make(chan struct{}), wake: make(chan struct{}, 1), wd: make(chan bool), wdAck: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.tools = newTools(s.budget)
	list, err := describeTools(s.tools)
	if err != nil {
		panic("mcp: static tool definitions do not encode: " + err.Error())
	}
	s.list = list
	return s
}

// lockedWriter serializes diagnostics from several goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) line(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	io.WriteString(l.w, "callsheet: mcp: "+msg+"\n")
}

func (s *session) hook(stage, id string) {
	if s.cfg.Hook != nil {
		s.cfg.Hook(stage, id)
	}
}

// note writes a bounded number of repeated diagnostics.
func (s *session) note(msg string) {
	if s.notes.Add(1) <= maxNotes {
		s.stderr.line(msg)
	}
}

// end decides the session's single terminal state: the first cause wins.
// It stops admission, cancels every call's context, discards pending
// replies and closes the owned streams so blocked I/O returns.
func (s *session) end(code int, diag string) {
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.code = code
		s.mu.Unlock()
		if diag != "" {
			s.stderr.line(diag)
		}
		close(s.endCh)
		s.cancel()
		if s.cfg.CloseIn != nil {
			s.cfg.CloseIn()
		}
		if s.cfg.CloseOut != nil {
			s.cfg.CloseOut()
		}
	})
}

func (s *session) ended() bool {
	select {
	case <-s.endCh:
		return true
	default:
		return false
	}
}

// ---- Reader ----

// reader owns stdin: it reads bounded lines and handles each in order;
// handling never blocks on a tool call or the writer.
func (s *session) reader() {
	defer close(s.readerDone)
	br := bufio.NewReaderSize(s.cfg.In, 64<<10)
	var partial []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(partial)+len(chunk) > MaxLineBytes || (err == bufio.ErrBufferFull && len(partial)+len(chunk) >= MaxLineBytes) {
			s.end(ExitFraming, "an input line exceeds 512 KiB; the session ends without reading it further")
			return
		}
		switch {
		case err == bufio.ErrBufferFull:
			partial = append(partial, chunk...)
			continue
		case errors.Is(err, io.EOF):
			if len(partial)+len(chunk) > 0 {
				s.end(ExitFraming, "the input ended inside an unterminated line, which was discarded")
			} else {
				s.end(ExitOK, "")
			}
			return
		case err != nil:
			if !s.ended() {
				s.end(ExitOutput, "cannot read stdin; the session ends")
			}
			return
		}
		line := make([]byte, 0, len(partial)+len(chunk))
		line = append(append(line, partial...), chunk...)
		partial = partial[:0]
		if s.ended() {
			return
		}
		s.handleLine(line)
	}
}

// handleLine handles one complete line (its LF or CRLF included).
func (s *session) handleLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	m, perr := parseMessage(line)
	if perr != nil {
		if !perr.silent {
			s.control(protocolError(perr.id, perr.code, perr.msg), nil)
		}
		return
	}
	switch m.kind {
	case kindResponse:
		s.note("ignored a response from the client: this server sends no requests")
	case kindNotification:
		s.notification(m)
	case kindRequest:
		s.request(m)
	}
}

// ---- Requests and notifications ----

func (s *session) request(m message) {
	id := *m.id
	s.mu.Lock()
	if s.reserved[id.key] {
		s.mu.Unlock()
		s.control(protocolError(&id, codeInvalidRequest, "invalid request: this id belongs to a request that is not answered yet"), nil)
		return
	}
	s.reserved[id.key] = true
	state := s.state
	s.mu.Unlock()
	switch m.method {
	case "initialize":
		s.initialize(id, m.params, state)
	case "ping":
		if m.params != nil && !validMeta(members(m.params)) {
			s.control(protocolError(&id, codeInvalidParams, "invalid params: _meta must be an object"), &id)
			return
		}
		s.control(resultFrame(id, []byte("{}")), &id)
	case "tools/list":
		if state != stateReady {
			s.notReady(id)
			return
		}
		s.toolsList(id, m.params)
	case "tools/call":
		if state != stateReady {
			s.notReady(id)
			return
		}
		s.toolsCall(id, m.params)
	default:
		s.control(protocolError(&id, codeMethodNotFound, "method not found: "+quoteName(m.method)), &id)
	}
}

func (s *session) notReady(id requestID) {
	s.control(protocolError(&id, codeInvalidRequest, "invalid request: the session is not initialized; send initialize, then notifications/initialized"), &id)
}

func validMeta(o map[string]json.RawMessage) bool {
	meta, ok := o["_meta"]
	return !ok || isObject(meta)
}

// initialize negotiates the protocol revision and retains the client's
// name and version, verbatim, for attribution.
func (s *session) initialize(id requestID, params json.RawMessage, state int) {
	if state != stateUninitialized {
		s.control(protocolError(&id, codeInvalidRequest, "invalid request: the session is already initialized"), &id)
		return
	}
	bad := func(msg string) { s.control(protocolError(&id, codeInvalidParams, "invalid params: "+msg), &id) }
	if params == nil {
		bad("initialize needs protocolVersion, capabilities and clientInfo")
		return
	}
	o := members(params)
	if _, ok := contract.JSONString(bytes.TrimSpace(o["protocolVersion"])); !ok {
		bad("protocolVersion must be a string")
		return
	}
	if !isObject(o["capabilities"]) {
		bad("capabilities must be an object")
		return
	}
	if !isObject(o["clientInfo"]) || !validMeta(o) {
		bad("clientInfo must be an object (and _meta an object)")
		return
	}
	info := members(o["clientInfo"])
	name, okName := contract.JSONString(bytes.TrimSpace(info["name"]))
	version, okVersion := contract.JSONString(bytes.TrimSpace(info["version"]))
	if !okName || !okVersion {
		bad("clientInfo name and version must be strings")
		return
	}
	s.mu.Lock()
	if s.state != stateUninitialized {
		s.mu.Unlock()
		s.control(protocolError(&id, codeInvalidRequest, "invalid request: the session is already initialized"), &id)
		return
	}
	s.state = stateInitQueued
	s.clientName, s.clientVersion = name, version
	s.mu.Unlock()
	res, _ := contract.Encode(struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    any    `json:"capabilities"`
		ServerInfo      any    `json:"serverInfo"`
	}{ProtocolVersion, map[string]any{"tools": map[string]bool{"listChanged": false}},
		map[string]string{"name": "callsheet", "version": s.cfg.Version}})
	s.enqueue(&frame{data: resultFrame(id, res), id: &id, emitted: s.initializeEmitted})
}

// initializeEmitted runs at the initialize answer's final-write boundary:
// the session now awaits notifications/initialized.
func (s *session) initializeEmitted() {
	s.mu.Lock()
	if s.state == stateInitQueued {
		s.state = stateInitialized
		close(s.initEmitted)
	}
	s.mu.Unlock()
}

func (s *session) toolsList(id requestID, params json.RawMessage) {
	if params != nil {
		o := members(params)
		for k, v := range o {
			switch k {
			case "_meta":
				if !isObject(v) {
					s.control(protocolError(&id, codeInvalidParams, "invalid params: _meta must be an object"), &id)
					return
				}
			case "cursor":
				c, ok := contract.JSONString(bytes.TrimSpace(v))
				if !ok || c != "" {
					s.control(protocolError(&id, codeInvalidParams, "invalid params: all tools fit one page; there is no cursor"), &id)
					return
				}
			default:
				s.control(protocolError(&id, codeInvalidParams, "invalid params: unknown member "+quoteName(k)), &id)
				return
			}
		}
	}
	s.control(resultFrame(id, s.list), &id)
}

// toolsCall admits a call into one of the two slots or refuses it at once
// (mcp_capacity) without contacting the plane; there is no queue.
func (s *session) toolsCall(id requestID, params json.RawMessage) {
	bad := func(msg string) { s.control(protocolError(&id, codeInvalidParams, "invalid params: "+msg), &id) }
	if params == nil {
		bad("tools/call needs a name")
		return
	}
	o := members(params)
	for k := range o {
		if k != "name" && k != "arguments" && k != "_meta" {
			bad("unknown member " + quoteName(k))
			return
		}
	}
	name, ok := contract.JSONString(bytes.TrimSpace(o["name"]))
	if !ok {
		bad("name must be a string")
		return
	}
	args, hasArgs := o["arguments"]
	if hasArgs && !isObject(args) {
		bad("arguments must be an object")
		return
	}
	if !validMeta(o) {
		bad("_meta must be an object")
		return
	}
	t := toolNamed(s.tools, name)
	if t == nil {
		bad("unknown tool " + quoteName(name))
		return
	}
	if !hasArgs {
		args = json.RawMessage("{}")
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	if s.active >= MaxActiveCalls {
		s.mu.Unlock()
		s.control(errorResultFrame(id, capacityError()), &id)
		return
	}
	s.active++
	c := s.newCall(id, t, args)
	s.calls[id.key] = c
	s.wg.Add(1)
	s.mu.Unlock()
	s.hook(StageAdmitted, id.key)
	go c.run()
}

// notification handles a notification; it never replies.
func (s *session) notification(m message) {
	switch m.method {
	case "notifications/initialized":
		// Ready only after the initialize answer was emitted: an early
		// notification waits (the reader with it) until the writer
		// finished that answer or the session ends; a client that does not
		// read is ended by the writer's progress timer.
		s.mu.Lock()
		queued := s.state == stateInitQueued
		s.mu.Unlock()
		if queued {
			s.hook(StageInitWait, "")
			select {
			case <-s.initEmitted:
			case <-s.endCh:
				return
			}
		}
		s.mu.Lock()
		if s.state == stateInitialized {
			s.state = stateReady
		}
		s.mu.Unlock()
	case "notifications/cancelled":
		if m.params == nil {
			return
		}
		rid, ok := parseID(members(m.params)["requestId"])
		if !ok {
			return
		}
		s.mu.Lock()
		c := s.calls[rid.key]
		s.mu.Unlock()
		if c != nil {
			c.cancelByClient()
		}
	}
}

// ---- Calls ----

var (
	// errCallDone is a finished call's context cause: its handler returned
	// and its answer was written or discarded.
	errCallDone    = errors.New("mcp: the call finished")
	errCancelled   = errors.New("mcp: the client cancelled the request")
	errOwnDeadline = errors.New("mcp: the call's budget passed")
	// errNoTime is a waiting call that has no client time left after
	// trust setup: unavailable without contacting the plane further.
	errNoTime = errors.New("mcp: no time remains in the call budget")
)

// call is one admitted tools/call. Its answer is decided once (settle):
// the tool's result, its budget's expiry or the client's cancellation,
// whichever comes first; a late success never replaces an expiry.
type call struct {
	s     *session
	id    requestID
	tool  *tool
	args  json.RawMessage
	start time.Time

	ctx      context.Context // cancelled by the client or the session's end
	cancel   context.CancelCauseFunc
	opCtx    context.Context // additionally cancelled at D-R
	opCancel context.CancelCauseFunc
	cl       Client

	responded chan struct{} // closed once the answer is written or discarded
	// deliver arbitrates the answer's delivery against D without locks
	// (the deliver* states; see beginFinalWrite).
	deliver atomic.Int32

	mu             sync.Mutex
	isBudgeted     bool
	delivery       time.Time
	clientDeadline time.Time
	settled        bool
	expired        bool
	cancelled      bool
	frame          *frame
	handlerDone    bool
	responseDone   bool
	released       bool
}

func (s *session) newCall(id requestID, t *tool, args json.RawMessage) *call {
	c := &call{s: s, id: id, tool: t, args: args, start: s.clock.Now(), responded: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancelCause(s.ctx)
	c.opCtx, c.opCancel = context.WithCancelCause(c.ctx)
	return c
}

// run is the call's handler goroutine.
func (c *call) run() {
	defer c.s.wg.Done()
	res, err := c.tool.run(c, c.args)
	if c.cl != nil {
		c.cl.Close()
	}
	c.s.hook(StageDecoded, c.id.key)
	c.settle(res, err)
	c.mu.Lock()
	c.handlerDone = true
	c.mu.Unlock()
	c.maybeRelease()
}

// client returns a fresh verified client for this call.
func (c *call) client() (Client, error) {
	c.s.hook(StageTrust, c.id.key)
	cl, err := c.s.cfg.Factory(c.opCtx)
	if err != nil {
		return nil, err
	}
	c.cl = cl
	return cl, nil
}

// budgeted arms a waiting call's deadlines from its admission instant:
// D-R cancels the client work and answers unavailable unless a valid
// answer came first; at D an answer that has not finished writing closes
// the session.
func (c *call) budgeted() {
	c.mu.Lock()
	if c.isBudgeted {
		c.mu.Unlock()
		return
	}
	c.isBudgeted = true
	c.delivery, c.clientDeadline = c.s.budget.Deadlines(c.start)
	c.mu.Unlock()
	now := c.s.clock.Now()
	cdC, stopCD := c.s.clock.NewTimer(max(0, c.clientDeadline.Sub(now)))
	dC, stopD := c.s.clock.NewTimer(max(0, c.delivery.Sub(now)))
	c.s.wg.Add(1)
	go c.timers(cdC, stopCD, dC, stopD)
	c.s.hook(StageArmed, c.id.key)
}

func (c *call) timers(cdC <-chan time.Time, stopCD func() bool, dC <-chan time.Time, stopD func() bool) {
	defer c.s.wg.Done()
	defer c.s.hook(StageTimed, c.id.key)
	defer stopCD()
	defer stopD()
	for {
		select {
		case <-cdC:
			cdC = nil
			c.expire()
			c.s.hook(StageExpired, c.id.key)
		case <-dC:
			c.s.hook(StageDeadline, c.id.key)
			if c.deadlinePassed() {
				c.s.end(ExitOutput, c.lateDiag())
			}
			return
		case <-c.responded:
			// Written (the writer judged it against D at its final write)
			// or discarded (a cancelled call answers nothing).
			return
		case <-c.s.endCh:
			return
		}
	}
}

// Delivery states and their only transitions, each with its one actor
// (tools.md, Cancel and bounded waits; design r0.4):
//   - idle -> writing: the writer, before a Write that can finish the answer;
//   - writing -> done (strictly before D), idle (short return before D) or
//     session end with exit 5 (completion or short return at or after D):
//     the writer, judging the instant it sampled right after that Write;
//   - idle -> missed: D's timer only (it never touches writing);
//   - idle -> discarded: notifications/cancelled, before the first Write.
//
// So the decision never depends on which goroutine runs first.
const (
	deliverIdle      = iota // no Write that can finish the answer is in progress (earlier chunks may be)
	deliverWriting          // a Write that can finish the answer is in progress or has just returned
	deliverDone             // the answer's last byte was written (late if at or after D: the writer ends the session)
	deliverMissed           // D fired while idle: the answer had not finished writing
	deliverDiscarded        // the answer was cancelled before its first Write
)

// beginFinalWrite precedes every Write that can finish the call's answer
// (its last chunk). False means D was already judged missed.
func (c *call) beginFinalWrite() bool {
	return c.deliver.CompareAndSwap(deliverIdle, deliverWriting)
}

// endFinalWrite follows that Write; at is the completion instant,
// sampled right after the Write returned, before anything else (no lock,
// no bookkeeping). The writer alone takes the transitions out of
// deliverWriting (tools.md, Cancel and bounded waits):
//   - done: the last byte was written strictly before D (on time);
//   - done at or after D: late, the session ends with exit 5;
//   - short return before D: back to deliverIdle for the next attempt;
//   - short return at or after D: late, the session ends with exit 5
//     (the deferral is not renewed; no further Write).
//
// isBudgeted and delivery were set before the answer was queued.
func (c *call) endFinalWrite(done bool, at time.Time) (late lateKind) {
	due := c.isBudgeted && !at.Before(c.delivery)
	switch {
	case done:
		c.deliver.Store(deliverDone)
		if due {
			return lateComplete
		}
	case due:
		return lateShort // stays deliverWriting: D's timer never takes it over
	default:
		c.deliver.Store(deliverIdle) // a short write before D: bytes remain
	}
	return notLate
}

// lateKind is the writer's late judgement of a final Write.
type lateKind int

const (
	notLate      lateKind = iota
	lateComplete          // the last byte was written at or after D
	lateShort             // a short return at or after D left the answer incomplete
)

// deadlinePassed judges D when its timer fired. With no Write that can
// finish the answer in progress (and none completed or discarded) the
// answer has not finished writing at D: late. With one in progress D
// defers to the writer, which judges from its own sample; if that Write
// does not return, the writer's one-second progress timer ends the
// session.
func (c *call) deadlinePassed() bool {
	return c.deliver.CompareAndSwap(deliverIdle, deliverMissed)
}

func (c *call) lateDiag() string {
	return "the answer to request " + string(c.id.raw) + " was not delivered within its " + c.s.budget.String() + " call budget; the session ends"
}

// The notes marking the writer's own late judgements in its diagnostics
// (the D timer and the progress watchdog never use them).
const (
	LateWriteNote = "its last byte was written at or after the deadline"
	LateShortNote = "its final write returned short at or after the deadline, leaving it incomplete"
)

func (c *call) lateWriteDiag(k lateKind) string {
	note := LateWriteNote
	if k == lateShort {
		note = LateShortNote
	}
	return "the answer to request " + string(c.id.raw) + " was not delivered within its " + c.s.budget.String() + " call budget (" +
		note + "); the session ends"
}

// available is the plane wait a waiting call may request now, or false
// when no client time remains.
func (c *call) available(dispatch bool) (time.Duration, bool) {
	now := c.s.clock.Now()
	if !now.Before(c.clientDeadline) {
		return 0, false
	}
	return c.s.budget.Available(c.delivery, now, dispatch), true
}

// expire is D-R: cancel the client work and answer unavailable unless the
// call was already answered or cancelled.
func (c *call) expire() {
	c.opCancel(errOwnDeadline)
	f := errorResultFrame(c.id, budgetError(c.tool.name, c.s.budget))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expired = true
	if c.settled || c.cancelled {
		return
	}
	c.settled = true
	c.queueLocked(f)
}

// settle answers the call with the tool's outcome after one recheck,
// immediately before queueing: a cancelled call answers nothing, and a
// waiting call past D-R answers unavailable instead of a late result.
func (c *call) settle(res toolResult, err error) {
	var f []byte
	switch {
	case errors.Is(err, errNoTime):
		f = errorResultFrame(c.id, budgetError(c.tool.name, c.s.budget))
	case err != nil:
		f = errorResultFrame(c.id, toolError(err))
	default:
		f = successFrame(c.id, res)
	}
	c.s.hook(StageQueue, c.id.key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settled || c.cancelled || c.s.ctx.Err() != nil {
		c.settled = true
		return
	}
	c.settled = true
	if c.isBudgeted && (c.expired || !c.s.clock.Now().Before(c.clientDeadline)) {
		f = errorResultFrame(c.id, budgetError(c.tool.name, c.s.budget))
	}
	c.queueLocked(f)
}

// queueLocked queues the call's answer (c.mu held).
func (c *call) queueLocked(data []byte) {
	fr := &frame{data: data, result: true, id: &c.id, call: c}
	c.frame = fr
	c.s.enqueue(fr)
}

// cancelByClient is notifications/cancelled: it cancels only this call's
// context, suppresses its answer unless the answer's write has started
// (bytes cannot be recalled), and never cancels the task itself.
func (c *call) cancelByClient() {
	c.mu.Lock()
	if c.cancelled || c.responseDone {
		c.mu.Unlock()
		return
	}
	if c.frame != nil && !c.s.discard(c.frame) {
		c.mu.Unlock()
		return
	}
	c.cancelled = true
	c.deliver.CompareAndSwap(deliverIdle, deliverDiscarded) // D no longer applies
	c.mu.Unlock()
	c.cancel(errCancelled)
	c.answered()
}

// answered marks the call's answer written or discarded: its ID is free
// again and it can no longer be cancelled.
func (c *call) answered() {
	c.mu.Lock()
	if c.responseDone {
		c.mu.Unlock()
		return
	}
	c.responseDone = true
	c.mu.Unlock()
	s := c.s
	s.mu.Lock()
	if s.calls[c.id.key] == c {
		delete(s.calls, c.id.key)
	}
	delete(s.reserved, c.id.key)
	s.mu.Unlock()
	close(c.responded)
	c.maybeRelease()
}

// maybeRelease frees the call's slot once its handler finished and its
// answer was written or discarded.
func (c *call) maybeRelease() {
	c.mu.Lock()
	if c.released || !c.handlerDone || !c.responseDone {
		c.mu.Unlock()
		return
	}
	c.released = true
	c.mu.Unlock()
	// Both lifecycles ended: detach the call's contexts from the session
	// (a no-op when the client or the session already cancelled them).
	c.cancel(errCallDone)
	c.s.mu.Lock()
	c.s.active--
	c.s.mu.Unlock()
	c.s.hook(StageReleased, c.id.key)
}
