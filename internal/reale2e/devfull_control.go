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
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Development control helpers (design 12a-dev-flow-harness, Task identity
// and ledger). They refuse a legacy run file, and the legacy helpers refuse
// a development run file. No helper dispatches: dev-next only names a
// prepared dispatch document. Every request is served by the supervisor's
// single loop, never concurrently.

// Development response codes.
const (
	CodePrepared        = "prepared"
	CodePaused          = "paused"
	CodeComplete        = "complete"
	CodePending         = "pending"
	CodeSeedUnpublished = "seed_unpublished"
	CodePromptBudget    = "prompt_budget_exceeded"
	CodeDuplicateWait   = "duplicate_wait"
)

// devOKCodes and devErrorCodes are the development socket's closed codes.
var (
	devOKCodes    = []string{CodePrepared, CodePaused, CodeComplete, CodePending, CodeAccepted, CodeAlreadyObserved, CodeCandidateUnpublished}
	devErrorCodes = []string{CodeInvalidRequest, CodeRunMismatch, CodeTaskMismatch, CodeObservationClash, CodeOwnerGateClosed, CodeClosing,
		CodeInternalError, CodeSeedUnpublished, CodePromptBudget, CodeDuplicateWait}
)

// devCode is a development response carrying only code.
func devCode(code string) controlResponse {
	return controlResponse{OK: slices.Contains(devOKCodes, code), Code: code}
}

// devDispatchDoc is the bounded document dev-next prepares. The socket
// response carries only its path and digest.
type devDispatchDoc struct {
	Schema     string              `json:"schema"`
	Ordinal    int                 `json:"ordinal"`
	Role       string              `json:"role"`
	Mode       string              `json:"mode"`
	Target     contract.TaskTarget `json:"target"`
	Goal       string              `json:"goal"`
	Acceptance string              `json:"acceptance"`
	Payload    []string            `json:"payload"`
	Workspace  string              `json:"workspace"`
	Instance   string              `json:"workspace_instance"`
	Base       string              `json:"base"`
}

// marshalDispatch renders a dispatch document without HTML escaping, with
// one final newline.
func marshalDispatch(doc devDispatchDoc) ([]byte, error) {
	if doc.Payload == nil {
		doc.Payload = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// exposeDispatch writes body (new, mode 0600) only when the goal and the
// document both fit, and returns the SHA-256 of exactly the bytes written.
// An oversized plan produces no file.
func exposeDispatch(dir string, ordinal int, goal string, body []byte) (string, string, error) {
	if err := checkGoalBudget(goal); err != nil || len(body) > maxDispatchDocByte {
		return "", "", errPromptBudget
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%02d.json", ordinal))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", err
	}
	_, werr := f.Write(body)
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", "", err
	}
	return path, sha256Hex(body), nil
}

// readDevRunFile reads a development run file. A legacy run file fails.
func readDevRunFile(runtime string) (DevRunFile, error) {
	data, err := readRuntimeFile(runtime, maxControlLine, RunFileName)
	if err != nil {
		return DevRunFile{}, err
	}
	var rf DevRunFile
	if err := decodeStrict(data, &rf); err != nil {
		return DevRunFile{}, err
	}
	if rf.Schema != DevRunSchema || !ValidRunID(rf.RunID) || !devRunScenarios[rf.Scenario] || !filepath.IsAbs(rf.Callsheet) || !filepath.IsAbs(rf.CA) ||
		rf.Plane == "" {
		return DevRunFile{}, errors.New("the run file is not a development run file")
	}
	return rf, nil
}

// liveDevRuntime validates a development helper's runtime.
func liveDevRuntime(runtime string) (DevRunFile, error) {
	if !filepath.IsAbs(runtime) || filepath.Clean(runtime) != runtime {
		return DevRunFile{}, errUnsafePath
	}
	if st, err := os.Lstat(runtime); err != nil {
		return DevRunFile{}, err
	} else if !st.IsDir() {
		return DevRunFile{}, errUnsafePath
	}
	rf, err := readDevRunFile(runtime)
	if err != nil {
		return DevRunFile{}, err
	}
	marker, err := readRuntimeFile(runtime, maxControlLine, ownerMarker)
	if err != nil {
		return DevRunFile{}, err
	}
	if string(marker) != rf.RunID+"\n" {
		return DevRunFile{}, errors.New("the runtime's ownership marker does not name its run")
	}
	return rf, nil
}

// devOrdinalOK accepts a canonical ordinal from 1 to 14. Leading zeros,
// signs, and surrounding space are refused.
func devOrdinalOK(s string) (int, bool) {
	n, ok := atoi(s)
	if !ok || n < 1 || n > maxWorkflowTasks || s != itoa(n) {
		return 0, false
	}
	return n, true
}

// devControlCall exchanges one development request.
func devControlCall(runtime string, req ControlRequest) (controlResponse, error) {
	rf, err := readDevRunFile(runtime)
	if err != nil {
		return controlResponse{}, fmt.Errorf("cannot read the run file: %w", err)
	}
	p, err := socketPath(runtime)
	if err != nil {
		return controlResponse{}, err
	}
	msg := map[string]string{"run": rf.RunID, "op": req.Op}
	if req.Op != opDevNext {
		msg["task"], msg["ordinal"] = req.Task, req.Ordinal
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
	return decodeDevResponse(req.Op, bytes.TrimSuffix(line, []byte("\n")))
}

// decodeDevResponse checks a development response's exact shape: a
// prepared dev-next carries a path, digest, ordinal, role and mode, a
// paused one its gate summary; other answers carry only ok and code.
func decodeDevResponse(op string, line []byte) (controlResponse, error) {
	var raw map[string]json.RawMessage
	var resp controlResponse
	if checkDuplicateKeys(line) != nil || json.Unmarshal(line, &raw) != nil || decodeStrict(line, &resp) != nil {
		return controlResponse{}, errMalformedResponse
	}
	want := []string{"ok", "code"}
	switch {
	case op == opDevNext && resp.OK && resp.Code == CodePrepared:
		want = append(want, "path", "digest", "ordinal", "role", "mode")
	case op == opDevNext && resp.OK && (resp.Code == CodePaused || resp.Code == CodeCandidateUnpublished):
		want = append(want, "summary")
	}
	if len(raw) != len(want) {
		return controlResponse{}, errMalformedResponse
	}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			return controlResponse{}, errMalformedResponse
		}
	}
	if resp.OK != slices.Contains(devOKCodes, resp.Code) || (!resp.OK && !slices.Contains(devErrorCodes, resp.Code)) {
		return controlResponse{}, errors.New("unknown response code from the supervisor")
	}
	if op != opDevNext && (resp.Code == CodePrepared || resp.Code == CodePaused || resp.Code == CodeComplete || resp.Code == CodePending ||
		resp.Code == CodeCandidateUnpublished) {
		return controlResponse{}, errMalformedResponse
	}
	switch {
	case op == opDevNext && resp.OK && resp.Code == CodePrepared:
		if !filepath.IsAbs(resp.Path) || !hex64RE.MatchString(resp.Digest) || resp.Ordinal < 1 || resp.Ordinal > maxWorkflowTasks {
			return controlResponse{}, errMalformedResponse
		}
	case op == opDevNext && resp.OK && resp.Code == CodePaused:
		// The helper relays a full-flow gate or a qualification probe's
		// gate; the supervisor, which knows its mode, judges which one the
		// phase must end with.
		_, _, ferr := parsePhaseGate(resp.Summary)
		_, complete, perr := parseProbePhaseGate(resp.Summary)
		if ferr != nil && (perr != nil || complete) {
			return controlResponse{}, errMalformedResponse
		}
	case op == opDevNext && resp.OK && resp.Code == CodeCandidateUnpublished:
		// The light probe's prepared candidate commit to push.
		if !contract.ValidCommitHash(resp.Summary) {
			return controlResponse{}, errMalformedResponse
		}
	}
	return resp, nil
}

// devNextHelper is reale2e dev-next: it prints the prepared document's
// path and digest, the gate line to end the phase with, or complete.
func devNextHelper(h Host) int {
	v, ok := helperFlags(h.Args[1:], "run")
	if !ok || !filepath.IsAbs(v["run"]) {
		return usageError(h, "dev-next needs exactly an absolute --run runtime directory")
	}
	if _, err := liveDevRuntime(v["run"]); err != nil {
		fmt.Fprintf(h.Stderr, "reale2e dev-next: %v\n", err)
		return 2
	}
	resp, err := devControlCall(v["run"], ControlRequest{Op: opDevNext})
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e dev-next: %v\n", err)
		return 2
	}
	switch resp.Code {
	case CodePrepared:
		fmt.Fprintf(h.Stdout, "%s %s %s\n", resp.Code, resp.Path, resp.Digest)
	case CodePaused, CodeCandidateUnpublished:
		fmt.Fprintf(h.Stdout, "%s %s\n", resp.Code, resp.Summary)
	default:
		fmt.Fprintln(h.Stdout, resp.Code)
	}
	if !resp.OK {
		return 1
	}
	return 0
}

// devObserveHelper is reale2e dev-observe and dev-wait-task. dev-wait-task
// runs the CLI wait only after the supervisor accepted the slot's single
// wait, and captures its streams and exit under coord/waits/NN.
func devObserveHelper(h Host, op string) int {
	v, ok := helperFlags(h.Args[1:], "run", "ordinal", "task")
	n, nord := devOrdinalOK(v["ordinal"])
	if !ok || !filepath.IsAbs(v["run"]) || !nord || !contract.ValidTaskID(v["task"]) {
		return usageError(h, op+" needs exactly an absolute --run runtime directory, a canonical --ordinal from 1 to 14 and a --task ID")
	}
	rf, err := liveDevRuntime(v["run"])
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e %s: %v\n", op, err)
		return 2
	}
	resp, err := devControlCall(v["run"], ControlRequest{Op: op, Task: v["task"], Ordinal: v["ordinal"]})
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e %s: %v\n", op, err)
		return 2
	}
	if !resp.OK {
		fmt.Fprintf(h.Stderr, "reale2e %s: refused: %s\n", op, resp.Code)
		return 1
	}
	if op != opDevWait {
		fmt.Fprintln(h.Stdout, resp.Code)
		return 0
	}
	legacy := RunFile{Schema: RunFileSchema, RunID: rf.RunID, Callsheet: rf.Callsheet, Plane: rf.Plane, CA: rf.CA}
	code, err := runWait(v["run"], legacy, n-1, v["task"])
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e %s: %v\n", op, err)
		return 2
	}
	if err := captureFiles(v["run"], n-1, code, h.Stdout, h.Stderr); err != nil {
		fmt.Fprintf(h.Stderr, "reale2e %s: %v\n", op, err)
		return 2
	}
	return code
}
