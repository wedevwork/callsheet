package reale2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
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

// Control protocol constants.
const (
	SocketName      = "s.sock"
	RunFileName     = "run.json"
	RunFileSchema   = "callsheet-real-e2e-run/v1"
	maxControlLine  = 4096
	controlExchange = 5 * time.Second
	maxSocketPath   = 90
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
	opObserve, opFinish  = "observe", "finish"
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
	case opObserve:
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

// controlServer serves the socket.
type controlServer struct {
	ln     net.Listener
	runID  string
	handle func(ControlRequest) string
	wg     sync.WaitGroup
}

// listenControl creates the socket mode 0600 and serves it; handle is
// called for requests whose run ID matches.
func listenControl(runtime, runID string, handle func(ControlRequest) string) (*controlServer, error) {
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
	s := &controlServer{ln: ln, runID: runID, handle: handle}
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

// exchange handles one connection.
func (s *controlServer) exchange(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(controlExchange))
	code := s.request(conn)
	resp := controlResponse{OK: slices.Contains(okCodes, code), Code: code}
	b, _ := json.Marshal(resp)
	conn.Write(append(b, '\n'))
}

// request reads and judges one request.
func (s *controlServer) request(conn net.Conn) string {
	br := bufio.NewReaderSize(conn, maxControlLine+1)
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > maxControlLine || br.Buffered() > 0 {
		return CodeInvalidRequest
	}
	req, ok := parseControlRequest(bytes.TrimSuffix(line, []byte("\n")))
	switch {
	case !ok:
		return CodeInvalidRequest
	case req.Run != s.runID:
		return CodeRunMismatch
	case req.Op == opObserve && hopIndex(req.Hop) < 0:
		return CodeInvalidHop
	}
	return s.handle(req)
}

// close stops accepting and waits for open exchanges.
func (s *controlServer) close() {
	s.ln.Close()
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
