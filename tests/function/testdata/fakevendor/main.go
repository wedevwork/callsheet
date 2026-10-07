// Command fakevendor is the fake vendor CLI of the iteration 07b function
// tests, built once per test process (tests/function builds it by explicit
// path; testdata keeps it out of ./... and of coverage). It is launched only
// by the real cmd/mcpqual through a plan naming its absolute path; it never
// runs a model or an installed vendor CLI.
//
// As a session it reads the plan-rendered probe configuration, starts the
// real "mcpqual serve" probe it names (in its own process group), speaks MCP
// to it and prints a transcript in its vendor's format. Its outcome is
// rule-based on the case file's delay, never on scheduling: with timeout T
// (silent, raised by a config "timeout_ms", or the absolute cap under
// progress when progress resets), a delay below T waits for the result and a
// delay at or above T cancels after T, so a slow machine cannot turn a
// success into a timeout. Its settings are FAKE_VENDOR_* variables supplied
// as explicit plan data.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
)

func fakeEnv(k string) string { return os.Getenv("FAKE_VENDOR_" + k) }

func fakeInt(k string) int64 {
	n, _ := strconv.ParseInt(fakeEnv(k), 10, 64)
	return n
}

// fakeLog appends one line to the launch log.
func fakeLog(format string, a ...any) {
	if p := fakeEnv("LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			fmt.Fprintf(f, format+"\n", a...)
			f.Close()
		}
	}
}

// fakeReady tells the test (through its FIFO) that signal dispositions
// and descendants are in place.
func fakeReady(extra string) {
	if p := fakeEnv("READY"); p != "" {
		if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			fmt.Fprintf(f, "ready %d %s\n", os.Getpid(), extra)
			f.Close()
		}
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	mode := fakeEnv("MODE")
	switch mode {
	case "descendant":
		// A group member that ignores TERM until KILLed.
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, "descendant-ready")
		os.Stdout.Close()
		fakeSleepForever()
	case "resistant":
		signal.Ignore(syscall.SIGTERM)
	}
	if len(args) > 0 && args[0] == "--version" {
		fakeLog("version %d", os.Getpid())
		fmt.Println(fakeEnv("VERSION"))
		return 0
	}
	var cfgPath, caseID string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--config":
			cfgPath = args[i+1]
		case "--case":
			caseID = args[i+1]
		}
	}
	fakeLog("session %d %s", os.Getpid(), caseID)
	switch mode {
	case "hang", "resistant":
		fakeReady("")
		fakeSleepForever()
	case "parent-exits-first":
		d := exec.Command(os.Args[0])
		d.Env = append(os.Environ(), "FAKE_VENDOR_MODE=descendant")
		out, _ := d.StdoutPipe()
		if err := d.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		line, _ := bufio.NewReader(out).ReadString('\n')
		if line != "descendant-ready\n" {
			return 1
		}
		fakeLog("descendant %d", d.Process.Pid)
	}
	transcript, err := fakeSession(cfgPath, caseID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake vendor:", err)
		return 1
	}
	os.Stdout.WriteString(transcript)
	return 0
}

// fakeSession runs one MCP session against the configured probe.
func fakeSession(cfgPath, caseID string) (string, error) {
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
		TimeoutMS int64 `json:"timeout_ms"`
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", err
	}
	srv := cfg.MCPServers["probe"]
	var caseFile string
	for i, a := range srv.Args {
		if a == "--case-file" && i+1 < len(srv.Args) {
			caseFile = srv.Args[i+1]
		}
	}
	cfRaw, err := os.ReadFile(caseFile)
	if err != nil {
		return "", err
	}
	cf, err := mcpqual.ParseCaseFile(cfRaw)
	if err != nil {
		return "", err
	}
	var delay int64
	for _, c := range cf.Cases {
		if c.CaseID == caseID {
			delay = c.DelayMS
		}
	}
	format := fakeEnv("FORMAT")
	switch fakeEnv("SCENARIO") {
	case "auth":
		return fakeTranscript(format, "auth", caseID, ""), nil
	}
	probe := exec.Command(srv.Command, srv.Args...)
	in, _ := probe.StdinPipe()
	outPipe, _ := probe.StdoutPipe()
	probe.Stderr = os.Stderr
	if err := probe.Start(); err != nil {
		return "", err
	}
	defer probe.Wait()
	defer in.Close()
	lines := make(chan map[string]json.RawMessage, 1024)
	go func() {
		br := bufio.NewReader(outPipe)
		for {
			l, err := br.ReadBytes('\n')
			if len(l) > 0 {
				var m map[string]json.RawMessage
				if json.Unmarshal(l, &m) == nil {
					lines <- m
				}
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	send := func(v any) {
		b, _ := json.Marshal(v)
		in.Write(append(b, '\n'))
	}
	await := func(id string) (map[string]json.RawMessage, error) {
		for m := range lines {
			if string(m["id"]) == id {
				return m, nil
			}
		}
		return nil, io.ErrUnexpectedEOF
	}
	name, version := fakeEnv("CLIENT_NAME"), fakeEnv("CLIENT_VERSION")
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": mcpqual.ProtocolVersion,
		"capabilities": map[string]any{}, "clientInfo": map[string]any{"name": name, "version": version}}})
	if _, err := await("1"); err != nil {
		return "", err
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if _, err := await("2"); err != nil {
		return "", err
	}
	token := fakeEnv("TOKEN") == "1"
	params := map[string]any{"name": "slow", "arguments": map[string]any{"case_id": caseID}}
	if token {
		params["_meta"] = map[string]any{"progressToken": "tok-" + caseID}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": params})
	// The rule: which timeout applies, and whether this delay reaches it.
	limit := fakeInt("TIMEOUT_MS")
	if cfg.TimeoutMS > 0 {
		limit = cfg.TimeoutMS
	}
	pc, _ := findCase(cf, caseID)
	if token && pc.ProgressIntervalMS > 0 && fakeEnv("RESETS") == "1" {
		limit = fakeInt("CAP_MS")
	}
	if limit > 0 && delay >= limit {
		sent := time.Now()
		if token && pc.ProgressIntervalMS > 0 {
			// Under progress, the cap is measured only once progress is
			// flowing: wait (event-driven) for the first notification,
			// so a stalled probe cannot yield a cap with no progress.
			for m := range lines {
				if string(m["method"]) == `"notifications/progress"` {
					break
				}
			}
		}
		time.Sleep(time.Duration(limit)*time.Millisecond - time.Since(sent))
		send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 3, "reason": "timeout"}})
		return fakeTranscript(format, "timeout", caseID, ""), nil
	}
	res, err := await("3")
	if err != nil {
		return "", err
	}
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(res["result"], &r)
	text := ""
	if len(r.Content) == 1 {
		text = r.Content[0].Text
	}
	return fakeTranscript(format, "success", caseID, text), nil
}

// fakeSleepForever blocks until a signal ends the process (a bare select
// would be a runtime deadlock exit).
func fakeSleepForever() {
	for {
		time.Sleep(time.Hour)
	}
}

func findCase(cf mcpqual.CaseFile, id string) (mcpqual.ProbeCase, bool) {
	for _, c := range cf.Cases {
		if c.CaseID == id {
			return c, true
		}
	}
	return mcpqual.ProbeCase{}, false
}

// fakeTranscript renders a transcript in the vendor's synthetic format
// (the decoders' fixture contracts). Its model prose mentions a timeout
// and may carry a secret; neither may survive into evidence as a timeout
// or unredacted.
func fakeTranscript(format, outcome, caseID, resultText string) string {
	prose := "The tool call may have timed out; MCP error -32001 is a timeout. " + fakeEnv("SECRET")
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	switch format {
	case "claude-json":
		// FAKE_VENDOR_TOOL_ID_REPEAT sizes the tool-use ID (a count, since
		// an environment value cannot hold megabytes).
		toolID := q("toolu_1")
		if n, err := strconv.Atoi(fakeEnv("TOOL_ID_REPEAT")); err == nil && n > 0 {
			toolID = q(strings.Repeat("x", n))
		}
		msgs := []string{`{"type":"system","subtype":"init"}`}
		switch outcome {
		case "auth":
			msgs = append(msgs, `{"type":"result","subtype":"error_authentication","is_error":true,"result":`+q(prose)+`}`)
		default:
			msgs = append(msgs, `{"type":"assistant","message":{"content":[{"type":"text","text":`+q(prose)+`},{"type":"tool_use","id":`+toolID+`,"name":"mcp__probe__slow","input":{"case_id":`+q(caseID)+`}}]}}`)
			block := `{"type":"tool_result","tool_use_id":` + toolID + `,"is_error":false,"content":[{"type":"text","text":` + q(resultText) + `}]}`
			if outcome == "timeout" {
				block = `{"type":"tool_result","tool_use_id":` + toolID + `,"is_error":true,"content":[{"type":"text","text":"MCP error -32001: Request timed out"}]}`
			}
			msgs = append(msgs, `{"type":"user","message":{"content":[`+block+`]}}`, `{"type":"result","subtype":"success","is_error":false,"result":`+q(prose)+`}`)
		}
		return "[" + strings.Join(msgs, ",") + "]\n"
	case "codex-jsonl":
		lines := []string{`{"type":"thread.started","thread_id":"t"}`, `{"type":"item.completed","item":{"id":"m","type":"agent_message","text":` + q(prose) + `}}`}
		switch outcome {
		case "auth":
			lines = append(lines, `{"type":"turn.failed","error":{"code":"unauthorized"}}`)
		case "timeout":
			lines = append(lines, `{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","tool":"slow","arguments":{"case_id":`+q(caseID)+`}}}`,
				`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","tool":"slow","status":"failed","error":{"code":"timeout"}}}`, `{"type":"turn.completed"}`)
		default:
			lines = append(lines, `{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","tool":"slow","arguments":{"case_id":`+q(caseID)+`}}}`,
				`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","tool":"slow","status":"completed","result":{"content":[{"type":"text","text":`+q(resultText)+`}]}}}`,
				`{"type":"turn.completed"}`)
		}
		return strings.Join(lines, "\n") + "\n"
	case "grok-json":
		call := `{"type":"tool_call","id":"g1","name":"slow","arguments":{"case_id":` + q(caseID) + `}}`
		switch outcome {
		case "auth":
			return `{"status":"error","error":{"kind":"auth"},"response":` + q(prose) + `,"events":[]}` + "\n"
		case "timeout":
			return `{"status":"success","response":` + q(prose) + `,"events":[` + call + `,{"type":"tool_result","id":"g1","content":"","error":{"kind":"mcp_timeout"}}]}` + "\n"
		}
		return `{"status":"success","response":` + q(prose) + `,"events":[` + call + `,{"type":"tool_result","id":"g1","content":` + q(resultText) + `,"error":null}]}` + "\n"
	case "cursor-jsonl":
		lines := []string{`{"type":"system","subtype":"init"}`, `{"type":"assistant","message":{"content":[{"type":"text","text":` + q(prose) + `}]}}`}
		start := `{"type":"tool_call","subtype":"started","call_id":"c1","tool_call":{"mcpToolCall":{"args":{"toolName":"slow","args":{"case_id":` + q(caseID) + `}}}}}`
		switch outcome {
		case "auth":
			lines = append(lines, `{"type":"result","subtype":"error","error_kind":"authentication","is_error":true}`)
		case "timeout":
			lines = append(lines, start, `{"type":"tool_call","subtype":"completed","call_id":"c1","tool_call":{"mcpToolCall":{"result":{"error":{"code":"timeout"}}}}}`,
				`{"type":"result","subtype":"success","is_error":false}`)
		default:
			lines = append(lines, start, `{"type":"tool_call","subtype":"completed","call_id":"c1","tool_call":{"mcpToolCall":{"result":{"success":{"content":[{"type":"text","text":`+q(resultText)+`}]}}}}}`,
				`{"type":"result","subtype":"success","is_error":false}`)
		}
		return strings.Join(lines, "\n") + "\n"
	}
	return ""
}
