package reale2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The supervisor's control socket (design 12a-real-e2e, Implementation
// boundaries): <runtime>/s.sock, mode 0600 in the mode-0700 runtime.
// Each connection carries exactly one newline-delimited JSON request of
// at most 4096 bytes and one response line, within 5 seconds, then
// closes. Observe requests are exactly {"run","op","task","hop"} and
// finish requests exactly {"run","op"}; duplicate or unknown keys,
// malformed JSON, extra frames and unknown operations are refused. A run
// ID other than the live supervisor's cannot mutate state. Responses carry
// only fixed codes, never raw diagnostics.
//
// Amendment A2 adds wait requests, exactly {"run","op":"wait","task",
// "hop"}: the helper half-closes its write side after the request and the
// server requires EOF before executing it, then answers one line and
// closes (the helper requires EOF after it), so a delayed extra frame is
// refused as well as a buffered one. A wait answers pending, terminal
// with the encoded contract.WaitResponse as "result", or an existing
// refusal code; the handoff to the loop and the authoritative fetch share
// one 4s context within the connection's 5s budget.

// Control protocol constants.
const (
	SocketName      = "s.sock"
	RunFileName     = "run.json"
	RunFileSchema   = "callsheet-real-e2e-run/v1"
	maxControlLine  = 4096
	controlExchange = 5 * time.Second
	// waitHandoff bounds a wait's hand-off to the loop plus its
	// authoritative fetch, from the connection's start; the rest of the
	// exchange is left for serialization and the write.
	waitHandoff   = 4 * time.Second
	maxSocketPath = 90
	// maxWaitLine bounds a wait response line (newline included): a
	// terminal answer's nested result has its own contract bound.
	maxWaitLine = contract.MaxWaitResponseBytes + maxControlLine
)

// Response codes.
const (
	CodeAccepted         = "accepted"
	CodeAlreadyObserved  = "already_observed"
	CodeInvalidRequest   = "invalid_request"
	CodeRunMismatch      = "run_mismatch"
	CodeInvalidHop       = "invalid_hop"
	CodeTaskMismatch     = "task_mismatch"
	CodeObservationClash = "observation_conflict"
	CodeOwnerGateClosed  = "owner_gate_closed"
	CodeClosing          = "closing"
	CodeInternalError    = "internal_error"
	// A2 wait answers.
	CodePending                 = "pending"
	CodeTerminal                = "terminal"
	opObserve, opFinish, opWait = "observe", "finish", "wait"
)

// okCodes are the success codes.
var okCodes = []string{CodeAccepted, CodeAlreadyObserved}

// errorCodes are the refusal codes.
var errorCodes = []string{CodeInvalidRequest, CodeRunMismatch, CodeInvalidHop, CodeTaskMismatch, CodeObservationClash, CodeOwnerGateClosed,
	CodeClosing, CodeInternalError}

// ControlRequest is one decoded request.
type ControlRequest struct {
	Run, Op, Task, Hop string
}

// controlResponse is the response line.
type controlResponse struct {
	OK   bool   `json:"ok"`
	Code string `json:"code"`
}

// RunFile is <runtime>/run.json.
type RunFile struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
}

// errSocketPath is a computed socket path over the bound.
var errSocketPath = errors.New("the control socket path exceeds 90 bytes")

// socketPath returns the runtime's socket path within the bound.
func socketPath(runtime string) (string, error) {
	p := filepath.Join(runtime, SocketName)
	if len(p) > maxSocketPath {
		return "", errSocketPath
	}
	return p, nil
}

// parseControlRequest strictly decodes one request line.
func parseControlRequest(line []byte) (ControlRequest, bool) {
	if err := checkDuplicateKeys(line); err != nil {
		return ControlRequest{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
		return ControlRequest{}, false
	}
	str := func(k string) (string, bool) {
		var s string
		v, ok := raw[k]
		if !ok || json.Unmarshal(v, &s) != nil || bytes.HasPrefix(bytes.TrimSpace(v), []byte("null")) {
			return "", false
		}
		return s, true
	}
	var r ControlRequest
	var ok1, ok2 bool
	r.Run, ok1 = str("run")
	r.Op, ok2 = str("op")
	if !ok1 || !ok2 {
		return ControlRequest{}, false
	}
	switch r.Op {
	case opObserve, opWait:
		var ok3, ok4 bool
		r.Task, ok3 = str("task")
		r.Hop, ok4 = str("hop")
		if !ok3 || !ok4 || len(raw) != 4 {
			return ControlRequest{}, false
		}
	case opFinish:
		if len(raw) != 2 {
			return ControlRequest{}, false
		}
	default:
		return ControlRequest{}, false
	}
	return r, true
}

// waitReply is a wait request's answer: pending, terminal with its
// encoded result, or a refusal code.
type waitReply struct {
	code   string
	result []byte
}

// line renders the reply's response line (newline included).
func (r waitReply) line() []byte {
	switch r.code {
	case CodePending:
		return []byte(`{"ok":true,"code":"pending"}` + "\n")
	case CodeTerminal:
		// The encoded result is embedded unchanged (json.Marshal would
		// re-escape it); its own bound was asserted by its encoder.
		out := append([]byte(`{"ok":true,"code":"terminal","result":`), r.result...)
		return append(out, "}\n"...)
	}
	code := r.code
	if !slices.Contains(errorCodes, code) {
		code = CodeInternalError
	}
	b, _ := json.Marshal(controlResponse{OK: false, Code: code})
	return append(b, '\n')
}

// errWorkExpired is the cause with which a wait's shared work budget
// expires (amendment A2.1): only this cause, before the loop took the
// handoff, makes a busy pending.
var errWorkExpired = errors.New("the wait's work budget expired")

// controlServer serves the socket.
type controlServer struct {
	ln     net.Listener
	runID  string
	handle func(ControlRequest) string
	// wait answers wait requests within its context (nil: a wait is an
	// unknown operation).
	wait func(ctx context.Context, req ControlRequest) waitReply
	// clock times a wait's work budget (the supervisor's world clock).
	clock Clock
	// frameRead, when set (tests only, at construction), runs after a
	// wait request's frame was read and before its EOF is required.
	frameRead func()
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// listenControl creates the socket mode 0600 and serves it; handle is
// called for requests whose run ID matches.
func listenControl(runtime, runID string, handle func(ControlRequest) string) (*controlServer, error) {
	return serveControl(runtime, runID, realClock{}, handle, nil)
}

// serveControl is listenControl with a wait handler whose work budget is
// timed by clock; opts adjust the server before it serves (tests).
func serveControl(runtime, runID string, clock Clock, handle func(ControlRequest) string,
	wait func(context.Context, ControlRequest) waitReply, opts ...func(*controlServer)) (*controlServer, error) {
	p, err := socketPath(runtime)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &controlServer{ln: ln, runID: runID, handle: handle, wait: wait, clock: clock, ctx: ctx, cancel: cancel}
	for _, o := range opts {
		o(s)
	}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func (s *controlServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.exchange(conn)
		}()
	}
}

// exchange handles one connection within one absolute 5s budget (the
// real socket deadline, the I/O safeguard); a wait's work budget is the
// first 4s of it on the injected clock, leaving 1s for the answer.
func (s *controlServer) exchange(conn net.Conn) {
	defer conn.Close()
	start, workStart := time.Now(), s.clock.Now()
	conn.SetDeadline(start.Add(controlExchange))
	req, code := s.request(conn)
	if code == "" && req.Op == opWait {
		ctx, release := s.workContext(workStart)
		r := s.wait(ctx, req)
		release()
		conn.Write(r.line())
		return
	}
	if code == "" {
		code = s.handle(req)
	}
	resp := controlResponse{OK: slices.Contains(okCodes, code), Code: code}
	b, _ := json.Marshal(resp)
	conn.Write(append(b, '\n'))
}

// workContext is a wait's shared work budget (amendment A2.1): a child of
// the server's context (cancelled when the server closes) that the
// injected clock cancels with errWorkExpired waitHandoff after the
// exchange started (reading the request used part of it; the handoff and
// a taken fetch share the rest, never reset). release stops the timer,
// joins its watcher and cancels the context.
func (s *controlServer) workContext(start time.Time) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(s.ctx)
	t, stop := s.clock.NewTimer(max(0, waitHandoff-s.clock.Now().Sub(start)))
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-t:
			cancel(errWorkExpired)
		case <-quit:
		}
	}()
	return ctx, func() {
		close(quit)
		stop()
		<-done
		cancel(nil)
	}
}

// request reads and judges one request: the decoded request with an
// empty code to execute it, or a refusal code.
func (s *controlServer) request(conn net.Conn) (ControlRequest, string) {
	br := bufio.NewReaderSize(conn, maxControlLine+1)
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > maxControlLine || br.Buffered() > 0 {
		return ControlRequest{}, CodeInvalidRequest
	}
	req, ok := parseControlRequest(bytes.TrimSuffix(line, []byte("\n")))
	if ok && req.Op == opWait {
		// The single request must be followed by EOF (the helper's
		// half-close): any later byte, even a delayed one, is an extra
		// frame.
		if s.wait == nil {
			return ControlRequest{}, CodeInvalidRequest
		}
		if s.frameRead != nil {
			s.frameRead()
		}
		if n, err := br.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			return ControlRequest{}, CodeInvalidRequest
		}
	}
	switch {
	case !ok:
		return ControlRequest{}, CodeInvalidRequest
	case req.Run != s.runID:
		return ControlRequest{}, CodeRunMismatch
	case (req.Op == opObserve || req.Op == opWait) && hopIndex(req.Hop) < 0:
		return ControlRequest{}, CodeInvalidHop
	case req.Op == opWait && !contract.ValidTaskID(req.Task):
		return ControlRequest{}, CodeInvalidRequest
	}
	return req, ""
}

// close stops accepting, cancels in-flight wait fetches and waits for
// open exchanges (each bounded by its 5s budget).
func (s *controlServer) close() {
	s.ln.Close()
	s.cancel()
	s.wg.Wait()
}

// readRunFile reads the runtime's run ID.
func readRunFile(runtime string) (string, error) {
	data, err := readBounded(filepath.Join(runtime, RunFileName), maxControlLine)
	if err != nil {
		return "", err
	}
	var rf RunFile
	if err := decodeStrict(data, &rf); err != nil {
		return "", err
	}
	if rf.Schema != RunFileSchema || !ValidRunID(rf.RunID) {
		return "", errors.New("the run file is not a version 1 run file")
	}
	return rf.RunID, nil
}

// controlCall sends one request to the live supervisor of runtime and
// returns its response code and success flag.
func controlCall(runtime, op, task, hop string) (string, bool, error) {
	runID, err := readRunFile(runtime)
	if err != nil {
		return "", false, fmt.Errorf("cannot read the run file: %w", err)
	}
	p, err := socketPath(runtime)
	if err != nil {
		return "", false, err
	}
	conn, err := net.DialTimeout("unix", p, controlExchange)
	if err != nil {
		return "", false, fmt.Errorf("cannot reach the supervisor: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(controlExchange))
	req := map[string]string{"run": runID, "op": op}
	if op == opObserve {
		req["task"], req["hop"] = task, hop
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return "", false, fmt.Errorf("cannot send the request: %w", err)
	}
	br := bufio.NewReaderSize(conn, maxControlLine+1)
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > maxControlLine {
		return "", false, errors.New("no complete response from the supervisor")
	}
	var resp controlResponse
	if err := decodeStrict(bytes.TrimSuffix(line, []byte("\n")), &resp); err != nil {
		return "", false, errors.New("malformed response from the supervisor")
	}
	if (resp.OK && !slices.Contains(okCodes, resp.Code)) || (!resp.OK && !slices.Contains(errorCodes, resp.Code)) {
		return "", false, errors.New("unknown response code from the supervisor")
	}
	return resp.Code, resp.OK, nil
}
