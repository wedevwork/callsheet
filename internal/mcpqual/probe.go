package mcpqual

// The deterministic probe server (FP-10): stdio newline JSON-RPC 2.0, one
// slow tool whose delay, progress interval and nonce come from the case
// file. Writes of responses and notifications are serialized with the one
// active call's terminal transition under a single mutex, so a completion
// and a cancellation (or EOF) are decided exactly once and no progress is
// written after either.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

const (
	maxIDBytes     = 128
	maxSafeInteger = 1<<53 - 1
)

// ProbeConfig configures one probe session.
type ProbeConfig struct {
	Cases   CaseFile
	In      io.Reader
	Out     io.Writer
	Events  io.Writer
	Clock   Clock
	Version string
	// EventLimit bounds the events file (default MaxEvidenceFileBytes).
	EventLimit int
}

// ErrProbeFatal wraps every error that ended a probe early (oversize or
// truncated frame, write failure, event-log exhaustion); the case is then
// inconclusive.
var ErrProbeFatal = errors.New("probe ended early")

type requestID struct {
	raw []byte
	key string
}

type message struct {
	notification bool
	response     bool
	id           *requestID
	method       string
	params       json.RawMessage
}

type protoErr struct {
	code   int
	msg    string
	id     *requestID
	silent bool
}

// call states.
const (
	callActive = iota
	callDone
)

type slowCall struct {
	id       requestID
	pc       ProbeCase
	token    json.RawMessage
	received time.Time
	due      time.Time
	progress int
	state    int
	stop     chan struct{}
	timerC   <-chan time.Time
	stopT    func() bool
	tickC    <-chan time.Time
	stopTick func()
}

type probe struct {
	cfg         ProbeConfig
	clock       Clock
	start       time.Time
	log         *eventLog
	mu          sync.Mutex // guards everything below and every stdout write
	initialized bool
	ready       bool
	active      *slowCall
	fatal       error
	dead        chan struct{}
	wg          sync.WaitGroup
}

// Serve runs one probe session until EOF or a fatal error. It returns nil
// after a clean EOF and an ErrProbeFatal-wrapped error otherwise; the exit
// event is written either way while the log has room.
func Serve(cfg ProbeConfig) error {
	if cfg.Clock == nil {
		cfg.Clock = RealClock
	}
	if cfg.EventLimit <= 0 {
		cfg.EventLimit = MaxEvidenceFileBytes
	}
	if err := cfg.Cases.Validate(); err != nil {
		return err
	}
	p := &probe{cfg: cfg, clock: cfg.Clock, log: newEventLog(cfg.Events, cfg.EventLimit), dead: make(chan struct{})}
	p.start = p.clock.Now()
	p.event(ProbeEvent{Kind: EvStart})
	type frame struct {
		line []byte
		err  error
	}
	lines := make(chan frame)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		br := bufio.NewReaderSize(cfg.In, MaxFrameBytes+2)
		for {
			line, err := readFrame(br)
			select {
			case lines <- frame{line, err}:
			case <-quit:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-p.dead:
			return p.finish("")
		case f := <-lines:
			if f.err == io.EOF {
				return p.finish(EvEOF)
			}
			if f.err != nil {
				p.die(f.err)
				return p.finish("")
			}
			p.handle(f.line)
		}
	}
}

// readFrame reads one LF-terminated line without its terminator (and an
// optional CR before it), bounded to MaxFrameBytes.
func readFrame(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, fmt.Errorf("an input frame exceeds %d bytes", MaxFrameBytes)
	case err == io.EOF && len(line) > 0:
		return nil, errors.New("a truncated input frame (no line terminator before EOF)")
	case err != nil:
		return nil, err
	}
	line = bytes.TrimSuffix(line[:len(line)-1], []byte{'\r'})
	if len(line) > MaxFrameBytes {
		return nil, fmt.Errorf("an input frame exceeds %d bytes", MaxFrameBytes)
	}
	return append([]byte(nil), line...), nil
}

func (p *probe) offset() int64 { return int64(p.clock.Now().Sub(p.start)) }

// event records ev at the current offset; a full or failed log is fatal.
func (p *probe) event(ev ProbeEvent) {
	p.eventAt(ev, p.offset())
}

func (p *probe) eventAt(ev ProbeEvent, off int64) {
	ev.RunID = p.cfg.Cases.RunID
	ev.OffsetNS = off
	if err := p.log.add(ev); err != nil {
		p.dieLocked(err)
	}
}

// dieLocked records the first fatal error and signals the serve loop; the
// caller may or may not hold mu (it touches only fatal and dead, guarded by
// a once-style check under the log's own ordering).
func (p *probe) dieLocked(err error) {
	if p.fatal == nil {
		p.fatal = err
		close(p.dead)
	}
}

func (p *probe) die(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dieLocked(err)
}

// finish ends the session: an active call ends with EOF (or the fatal
// error), every call goroutine is joined and the exit event written.
func (p *probe) finish(kind string) error {
	p.mu.Lock()
	reason := "eof"
	if p.fatal != nil {
		reason = p.fatal.Error()
		p.eventAt(ProbeEvent{Kind: EvError, Reason: reason}, p.offset())
	}
	if c := p.active; c != nil && c.state == callActive {
		p.endLocked(c)
		ev := ProbeEvent{Kind: EvEOF, CaseID: c.pc.CaseID, RequestID: c.id.raw}
		if kind == "" {
			ev.Kind, ev.Reason = EvCancelled, "fatal"
		}
		p.eventAt(ev, p.offset())
	} else if kind == EvEOF {
		p.eventAt(ProbeEvent{Kind: EvEOF}, p.offset())
	}
	fatal := p.fatal
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.log.add(ProbeEvent{Kind: EvExit, RunID: p.cfg.Cases.RunID, OffsetNS: p.offset(), Reason: reason}); err != nil && fatal == nil {
		fatal = err
	}
	if fatal != nil {
		return fmt.Errorf("%w: %v", ErrProbeFatal, fatal)
	}
	return nil
}

// endLocked makes c terminal: timers stop and its goroutine returns.
func (p *probe) endLocked(c *slowCall) {
	c.state = callDone
	c.stopT()
	if c.stopTick != nil {
		c.stopTick()
	}
	close(c.stop)
}

// writeLocked writes one frame; a failure is fatal.
func (p *probe) writeLocked(b []byte) error {
	if len(b) > MaxFrameBytes+1 {
		err := fmt.Errorf("an output frame of %d bytes exceeds %d", len(b)-1, MaxFrameBytes)
		p.dieLocked(err)
		return err
	}
	if _, err := p.cfg.Out.Write(b); err != nil {
		p.dieLocked(err)
		return err
	}
	return nil
}

func (p *probe) handle(line []byte) {
	msg, perr := parseMessage(line)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fatal != nil {
		return
	}
	if perr != nil {
		if !perr.silent {
			p.writeLocked(errorFrame(perr.id, perr.code, perr.msg))
		}
		return
	}
	if msg.response {
		return // the probe sends no requests; stray responses are ignored
	}
	if msg.notification {
		p.notificationLocked(msg)
		return
	}
	id := *msg.id
	switch msg.method {
	case "initialize":
		p.initializeLocked(id, msg.params)
	case "ping":
		p.writeLocked(resultFrame(id, []byte(`{}`)))
	case "tools/list":
		if !p.ready {
			p.writeLocked(errorFrame(&id, codeInvalidParams, "invalid params: the session is not initialized"))
			return
		}
		p.writeLocked(resultFrame(id, toolsListResult))
	case "tools/call":
		p.callLocked(id, msg.params)
	default:
		p.writeLocked(errorFrame(&id, codeMethodNotFound, "method not found: "+strconv.Quote(msg.method)))
	}
}

var toolsListResult = []byte(`{"tools":[{"name":"slow","description":"Qualification probe: answers after the case file's delay for case_id, with progress when requested. Measurement only.",` +
	`"inputSchema":{"type":"object","properties":{"case_id":{"type":"string"}},"required":["case_id"],"additionalProperties":false}}]}`)

func (p *probe) notificationLocked(msg message) {
	switch msg.method {
	case "notifications/initialized":
		if p.initialized {
			p.ready = true
		}
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.params, &params) != nil {
			return
		}
		id, ok := parseID(params.RequestID)
		c := p.active
		if !ok || c == nil || id.key != c.id.key {
			return
		}
		if c.state != callActive {
			p.event(ProbeEvent{Kind: EvLateCancel, CaseID: c.pc.CaseID, RequestID: c.id.raw})
			return
		}
		p.endLocked(c)
		p.event(ProbeEvent{Kind: EvCancelled, CaseID: c.pc.CaseID, RequestID: c.id.raw})
	}
}

func (p *probe) initializeLocked(id requestID, raw json.RawMessage) {
	if p.initialized {
		p.writeLocked(errorFrame(&id, codeInvalidRequest, "invalid request: already initialized"))
		return
	}
	var params struct {
		ProtocolVersion *string         `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      *struct {
			Name    *string `json:"name"`
			Version *string `json:"version"`
		} `json:"clientInfo"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ProtocolVersion == nil || *params.ProtocolVersion == "" || !isObject(params.Capabilities) || params.ClientInfo == nil ||
		params.ClientInfo.Name == nil || params.ClientInfo.Version == nil {
		p.writeLocked(errorFrame(&id, codeInvalidParams, "invalid params: initialize needs a nonempty protocolVersion string, capabilities and clientInfo name and version strings"))
		return
	}
	// Version negotiation (design decoder-enrollment B1, FP-10; MCP
	// 2025-06-18 lifecycle): the probe supports exactly ProtocolVersion and
	// answers it for any otherwise valid requested version, older, equal or
	// newer, without comparing dates. The client decides whether it can use
	// the answer (it may disconnect); the event records the decision before
	// the response is written, not the client's acceptance.
	requested, selected := *params.ProtocolVersion, ProtocolVersion
	p.initialized = true
	p.event(ProbeEvent{Kind: EvInitialize, ClientName: params.ClientInfo.Name, ClientVersion: params.ClientInfo.Version,
		RequestedProtocolVersion: &requested, SelectedProtocolVersion: &selected})
	res, _ := encodeJSON(map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "mcpqual", "version": p.cfg.Version},
	})
	p.writeLocked(resultFrame(id, res))
}

func (p *probe) callLocked(id requestID, raw json.RawMessage) {
	bad := func(msg string) { p.writeLocked(errorFrame(&id, codeInvalidParams, "invalid params: "+msg)) }
	if !p.ready {
		bad("the session is not initialized")
		return
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil {
		bad("tools/call needs an object")
		return
	}
	for k := range params {
		if k != "name" && k != "arguments" && k != "_meta" {
			bad("unknown member " + strconv.Quote(k))
			return
		}
	}
	var name string
	if json.Unmarshal(params["name"], &name) != nil || name != "slow" {
		bad("unknown tool; the probe has only slow")
		return
	}
	var args map[string]json.RawMessage
	var caseID string
	if json.Unmarshal(params["arguments"], &args) != nil || len(args) != 1 || json.Unmarshal(args["case_id"], &caseID) != nil {
		bad("slow takes exactly {case_id: string}")
		return
	}
	pc, ok := p.cfg.Cases.lookup(caseID)
	if !ok {
		bad("case_id " + strconv.Quote(caseID) + " is not in the case file")
		return
	}
	token, present, err := progressToken(params["_meta"])
	if err != nil {
		bad(err.Error())
		return
	}
	if c := p.active; c != nil && c.state == callActive {
		p.event(ProbeEvent{Kind: EvRejected, CaseID: caseID, RequestID: id.raw, Reason: "concurrent_call"})
		bad("a slow call is already active; this invalidates the measurement")
		return
	}
	now := p.clock.Now()
	c := &slowCall{id: id, pc: pc, token: token, received: now, due: now.Add(time.Duration(pc.DelayMS) * time.Millisecond), stop: make(chan struct{})}
	p.active = c
	pres := present
	p.eventAt(ProbeEvent{Kind: EvReceipt, CaseID: caseID, RequestID: id.raw, TokenPresent: &pres, Token: token}, int64(now.Sub(p.start)))
	if pc.DelayMS == 0 {
		c.stopT = func() bool { return false }
		p.completeLocked(c)
		return
	}
	c.timerC, c.stopT = p.clock.NewTimer(time.Duration(pc.DelayMS) * time.Millisecond)
	if present && pc.ProgressIntervalMS > 0 && pc.DelayMS > 0 {
		c.tickC, c.stopTick = p.clock.NewTicker(time.Duration(pc.ProgressIntervalMS) * time.Millisecond)
	}
	p.wg.Add(1)
	go p.run(c)
}

// progressToken extracts _meta.progressToken, preserving its JSON type:
// a string or a number; null, objects, arrays and booleans are invalid.
func progressToken(meta json.RawMessage) (json.RawMessage, bool, error) {
	if len(meta) == 0 {
		return nil, false, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return nil, false, errors.New("_meta must be an object")
	}
	tok, ok := m["progressToken"]
	if !ok {
		return nil, false, nil
	}
	tok = bytes.TrimSpace(tok)
	if len(tok) > maxTokenBytes {
		return nil, false, fmt.Errorf("progressToken exceeds %d bytes", maxTokenBytes)
	}
	switch c := tok[0]; {
	case c == '"', c == '-', c >= '0' && c <= '9':
		return append(json.RawMessage(nil), tok...), true, nil
	}
	return nil, false, errors.New("progressToken must be a string or a number")
}

// run waits for c's delay, sending progress at each tick while the call is
// active. A tick at or after the due instant completes the call instead
// (once the case's min_progress notifications were sent), so the order in
// which a simultaneously ready tick and timer are received never changes
// the output. With min_progress, completion waits past the due instant for
// the next ticks until that many notifications were written.
func (p *probe) run(c *slowCall) {
	defer p.wg.Done()
	timerC := c.timerC
	for {
		select {
		case <-c.stop:
			return
		case <-timerC:
			if p.complete(c) {
				return
			}
			timerC = nil // due, but gated: the ticks complete it
		case <-c.tickC:
			if p.tick(c) {
				return
			}
		}
	}
}

// gated reports whether c may not complete yet: it still owes progress
// notifications (only possible with a token and an interval).
func (c *slowCall) gated() bool {
	return c.tickC != nil && c.progress < c.pc.MinProgress
}

func (p *probe) tick(c *slowCall) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.state != callActive {
		return true
	}
	due := !p.clock.Now().Before(c.due)
	if due && !c.gated() {
		p.completeLocked(c)
		return true
	}
	c.progress++
	b, _ := encodeJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress",
		"params": map[string]any{"progressToken": c.token, "progress": c.progress}})
	if p.writeLocked(append(b, '\n')) != nil {
		p.endLocked(c)
		return true
	}
	p.event(ProbeEvent{Kind: EvProgress, CaseID: c.pc.CaseID, RequestID: c.id.raw, Progress: c.progress})
	if due && !c.gated() {
		p.completeLocked(c)
		return true
	}
	return false
}

// complete is the one successful terminal transition: the fixed result
// with the nonce and case ID, written once, never after cancellation.
// complete finishes c when its delay has passed; it reports false while
// the min_progress gate still holds the call open.
func (p *probe) complete(c *slowCall) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.state == callActive && c.gated() {
		return false
	}
	p.completeLocked(c)
	return true
}

func (p *probe) completeLocked(c *slowCall) {
	if c.state != callActive {
		return
	}
	p.event(ProbeEvent{Kind: EvScheduled, CaseID: c.pc.CaseID, RequestID: c.id.raw})
	p.endLocked(c)
	text, _ := encodeJSON(map[string]string{"nonce": p.cfg.Cases.Nonce, "case_id": c.pc.CaseID})
	res, _ := encodeJSON(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "isError": false})
	if err := p.writeLocked(resultFrame(c.id, res)); err != nil {
		p.event(ProbeEvent{Kind: EvWriteFail, CaseID: c.pc.CaseID, RequestID: c.id.raw, Reason: err.Error()})
		return
	}
	p.event(ProbeEvent{Kind: EvCompleted, CaseID: c.pc.CaseID, RequestID: c.id.raw, Progress: c.progress})
}

// ---- Codec subset ----

var envelopeKeys = map[string]bool{"jsonrpc": true, "id": true, "method": true, "params": true, "result": true, "error": true}

// parseMessage validates one frame: UTF-8, strict JSON, then the JSON-RPC
// 2.0 envelope; it classifies requests, notifications and responses.
func parseMessage(line []byte) (message, *protoErr) {
	if err := checkJSON(line); err != nil {
		return message{}, &protoErr{code: codeParse, msg: "parse error: " + err.Error()}
	}
	trimmed := bytes.TrimLeft(line, " \t")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return message{}, &protoErr{code: codeInvalidRequest, msg: "invalid request: a message is one JSON object (batches are not supported)"}
	}
	var o map[string]json.RawMessage
	json.Unmarshal(line, &o)
	idRaw, hasID := o["id"]
	methodRaw, hasMethod := o["method"]
	_, hasResult := o["result"]
	_, hasError := o["error"]
	var id *requestID
	if hasID {
		if v, ok := parseID(idRaw); ok {
			id = &v
		}
	}
	if !hasMethod {
		if hasID && (hasResult || hasError) {
			return message{response: true}, nil
		}
		return message{}, &protoErr{code: codeInvalidRequest, msg: "invalid request: no method", id: id}
	}
	notification := !hasID
	if hasID && id == nil {
		return message{}, &protoErr{code: codeInvalidRequest, msg: "invalid request: the id must be a string of at most 128 bytes or an integer within ±(2^53-1)"}
	}
	bad := func(msg string) (message, *protoErr) {
		return message{}, &protoErr{code: codeInvalidRequest, msg: msg, id: id, silent: notification}
	}
	for k := range o {
		if !envelopeKeys[k] {
			return bad("invalid request: unknown member " + strconv.Quote(k))
		}
	}
	var jv, method string
	if json.Unmarshal(o["jsonrpc"], &jv) != nil || jv != "2.0" {
		return bad(`invalid request: jsonrpc must be "2.0"`)
	}
	if json.Unmarshal(methodRaw, &method) != nil {
		return bad("invalid request: method must be a string")
	}
	if hasResult || hasError {
		return bad("invalid request: a request carries no result or error")
	}
	params, hasParams := o["params"]
	if hasParams && !isObject(params) {
		return message{}, &protoErr{code: codeInvalidParams, msg: "invalid params: params must be an object", id: id, silent: notification}
	}
	return message{notification: notification, id: id, method: method, params: params}, nil
}

// parseID accepts a non-null string of at most maxIDBytes or an exact
// integer within ±(2^53-1), keeping its JSON type.
func parseID(raw json.RawMessage) (requestID, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return requestID{}, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || len(s) > maxIDBytes {
			return requestID{}, false
		}
		return requestID{raw: []byte(quoteJSON(s)), key: "s" + s}, true
	}
	for i, c := range raw {
		if !(c >= '0' && c <= '9') && !(i == 0 && c == '-') {
			return requestID{}, false
		}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n > maxSafeInteger || n < -maxSafeInteger || (len(raw) > 1 && raw[0] == '0') || string(raw) == "-0" {
		return requestID{}, false
	}
	v := strconv.FormatInt(n, 10)
	return requestID{raw: []byte(v), key: "n" + v}, true
}

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

func resultFrame(id requestID, result []byte) []byte {
	b := make([]byte, 0, len(id.raw)+len(result)+48)
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, id.raw...)
	b = append(b, `,"result":`...)
	b = append(b, result...)
	return append(b, "}\n"...)
}

func errorFrame(id *requestID, code int, msg string) []byte {
	idRaw := []byte("null")
	if id != nil {
		idRaw = id.raw
	}
	b := make([]byte, 0, 96+len(idRaw)+len(msg))
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, idRaw...)
	b = append(b, `,"error":{"code":`...)
	b = strconv.AppendInt(b, int64(code), 10)
	b = append(b, `,"message":`...)
	b = append(b, quoteJSON(msg)...)
	return append(b, "}}\n"...)
}
