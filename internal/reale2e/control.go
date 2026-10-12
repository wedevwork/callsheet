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
// boundaries, A3): <runtime>/s.sock, mode 0600 in the mode-0700 runtime.
// Each connection carries exactly one newline-delimited JSON request of
// at most 4096 bytes and one response line, within 5 seconds, then
// closes. Observe requests are exactly {"run","op","task","hop"}; A3.6
// adds capture-wait, exactly {"run","op","hop","code"} with an integer
// code 0..255, and review-material, exactly {"run","op"}. Every other
// operation (A3 removed finish and wait) and any duplicate or unknown
// key, malformed JSON or extra frame is refused. A run ID other than the
// live supervisor's cannot mutate state. Responses carry only fixed
// codes, never raw diagnostics; only a successful review-material
// response adds the admitted coder commit and its three file hashes.

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
	opObserve            = "observe"
	opCapture            = "capture-wait"
	opReview             = "review-material"
)

// requestKeys are each operation's exact request keys.
var requestKeys = map[string][]string{
	opObserve: {"run", "op", "task", "hop"},
	opCapture: {"run", "op", "hop", "code"},
	opReview:  {"run", "op"},
}

// okCodes are the success codes.
var okCodes = []string{CodeAccepted, CodeAlreadyObserved}

// errorCodes are the refusal codes.
var errorCodes = []string{CodeInvalidRequest, CodeRunMismatch, CodeInvalidHop, CodeTaskMismatch, CodeObservationClash, CodeOwnerGateClosed,
	CodeClosing, CodeInternalError}

// ControlRequest is one decoded request; Code is capture-wait's exit
// code.
type ControlRequest struct {
	Run, Op, Task, Hop string
	Code               int
}

// ReviewFile is one review-material file with its expected SHA-256.
type ReviewFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// controlResponse is the response line; Commit and Files are set only by
// a successful review-material.
type controlResponse struct {
	OK     bool         `json:"ok"`
	Code   string       `json:"code"`
	Commit string       `json:"commit,omitempty"`
	Files  []ReviewFile `json:"files,omitempty"`
}

// codeResponse is a response carrying only code.
func codeResponse(code string) controlResponse {
	return controlResponse{OK: slices.Contains(okCodes, code), Code: code}
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

// parseControlRequest strictly decodes one request line: exactly its
// operation's keys, strings except capture-wait's integer code.
func parseControlRequest(line []byte) (ControlRequest, bool) {
	if err := checkDuplicateKeys(line); err != nil {
		return ControlRequest{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
		return ControlRequest{}, false
	}
	null := func(v json.RawMessage) bool { return bytes.HasPrefix(bytes.TrimSpace(v), []byte("null")) }
	str := func(k string) (string, bool) {
		var s string
		v, ok := raw[k]
		if !ok || json.Unmarshal(v, &s) != nil || null(v) {
			return "", false
		}
		return s, true
	}
	var r ControlRequest
	op, ok := str("op")
	keys, known := requestKeys[op]
	if !ok || !known || len(raw) != len(keys) {
		return ControlRequest{}, false
	}
	r.Op = op
	for _, k := range keys {
		switch k {
		case "op":
		case "code":
			v, present := raw[k]
			if !present || null(v) || json.Unmarshal(v, &r.Code) != nil || r.Code < 0 || r.Code > 255 {
				return ControlRequest{}, false
			}
		default:
			s, ok := str(k)
			if !ok {
				return ControlRequest{}, false
			}
			switch k {
			case "run":
				r.Run = s
			case "task":
				r.Task = s
			case "hop":
				r.Hop = s
			}
		}
	}
	return r, true
}

// controlServer serves the socket.
type controlServer struct {
	ln     net.Listener
	runID  string
	handle func(ControlRequest) controlResponse
	wg     sync.WaitGroup
}

// listenControl creates the socket mode 0600 and serves it; handle is
// called for requests whose run ID matches.
func listenControl(runtime, runID string, handle func(ControlRequest) controlResponse) (*controlServer, error) {
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
	resp := s.request(conn)
	resp.OK = slices.Contains(okCodes, resp.Code)
	if !resp.OK {
		resp = codeResponse(resp.Code)
	}
	b, _ := json.Marshal(resp)
	if len(b) >= maxControlLine {
		b, _ = json.Marshal(codeResponse(CodeInternalError))
	}
	conn.Write(append(b, '\n'))
}

// request reads and judges one request.
func (s *controlServer) request(conn net.Conn) controlResponse {
	br := bufio.NewReaderSize(conn, maxControlLine+1)
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > maxControlLine || br.Buffered() > 0 {
		return codeResponse(CodeInvalidRequest)
	}
	req, ok := parseControlRequest(bytes.TrimSuffix(line, []byte("\n")))
	switch {
	case !ok:
		return codeResponse(CodeInvalidRequest)
	case req.Run != s.runID:
		return codeResponse(CodeRunMismatch)
	case req.Op != opReview && hopIndex(req.Hop) < 0:
		return codeResponse(CodeInvalidHop)
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
	data, err := readRuntimeFile(runtime, maxControlLine, RunFileName)
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

// controlCall makes the one exchange of req, its run ID read from the
// runtime's run file, with the live supervisor of runtime and returns the
// operation's strictly decoded response.
func controlCall(runtime string, req ControlRequest) (controlResponse, error) {
	runID, err := readRunFile(runtime)
	if err != nil {
		return controlResponse{}, fmt.Errorf("cannot read the run file: %w", err)
	}
	p, err := socketPath(runtime)
	if err != nil {
		return controlResponse{}, err
	}
	msg := map[string]any{"run": runID, "op": req.Op}
	switch req.Op {
	case opObserve:
		msg["task"], msg["hop"] = req.Task, req.Hop
	case opCapture:
		msg["hop"], msg["code"] = req.Hop, req.Code
	}
	conn, err := net.DialTimeout("unix", p, controlExchange)
	if err != nil {
		return controlResponse{}, fmt.Errorf("cannot reach the supervisor: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(controlExchange))
	b, _ := json.Marshal(msg)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return controlResponse{}, fmt.Errorf("cannot send the request: %w", err)
	}
	br := bufio.NewReaderSize(conn, maxControlLine+1)
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > maxControlLine {
		return controlResponse{}, errors.New("no complete response from the supervisor")
	}
	return decodeControlResponse(req.Op, bytes.TrimSuffix(line, []byte("\n")))
}

// errMalformedResponse is a response outside its operation's shape.
var errMalformedResponse = errors.New("malformed response from the supervisor")

// decodeControlResponse strictly decodes op's response: exactly ok and
// code, plus commit and the three review files for a successful
// review-material only.
func decodeControlResponse(op string, line []byte) (controlResponse, error) {
	var raw map[string]json.RawMessage
	var resp controlResponse
	if checkDuplicateKeys(line) != nil || json.Unmarshal(line, &raw) != nil || decodeStrict(line, &resp) != nil {
		return controlResponse{}, errMalformedResponse
	}
	want := []string{"ok", "code"}
	if op == opReview && resp.OK {
		want = append(want, "commit", "files")
	}
	if len(raw) != len(want) {
		return controlResponse{}, errMalformedResponse
	}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			return controlResponse{}, errMalformedResponse
		}
	}
	if (resp.OK && !slices.Contains(okCodes, resp.Code)) || (!resp.OK && !slices.Contains(errorCodes, resp.Code)) {
		return controlResponse{}, errors.New("unknown response code from the supervisor")
	}
	if op == opReview && resp.OK {
		if !hex40RE.MatchString(resp.Commit) || len(resp.Files) != len(inlinePaths) {
			return controlResponse{}, errMalformedResponse
		}
		for i, f := range resp.Files {
			if f.Path != inlinePaths[i] || !hex64RE.MatchString(f.SHA256) {
				return controlResponse{}, errMalformedResponse
			}
		}
	}
	return resp, nil
}
