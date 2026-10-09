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
//
// Design decoder-enrollment adds the capture recipes: --help and exec
// --help print a fixed help text, the session finds its probe from the
// real recipes' shapes (claude --mcp-config, codex -c mcp_servers.probe.*,
// grok .grok/config.toml and cursor .cursor/mcp.json in its working
// directory) and its case from the prompt, records its argv, working
// directory and configuration (FAKE_VENDOR_ARGV_LOG), and plays the
// lifecycle scenarios (no probe, no tool, an extra or marker call, a
// flood of either stream, a descendant holding both pipes outside the
// group).
//
// Design decoder-enrollment B1 adds: a requested protocol version
// (FAKE_VENDOR_PROTOCOL) and a client that declines the probe's answer
// and disconnects (FAKE_VENDOR_DECLINE); the B1 recipes' own checks
// (FAKE_VENDOR_RECIPE codex, grok or cursor: the exact scoped invocation
// and configuration, else exit 3 before any probe), Codex's approval
// outcomes and Grok's trust rejection (FAKE_VENDOR_OUTCOME), Grok's
// streaming-json output (format grok-stream) with structured and embedded
// secret canaries and usage, a connection to a separately owned leader
// (FAKE_VENDOR_LEADER, and MODE=leader for that service), and Cursor's
// "mcp enable probe" (FAKE_VENDOR_ENABLE: where it writes).
//
// Design decoder-enrollment B1.5 adds: the terminal observation fixtures
// (FAKE_VENDOR_CUT, limited to timeout or success sessions by
// FAKE_VENDOR_CUT_ON): after the real probe has been reaped, the session
// rewrites its own server-events file to the bounded prefix ending at the
// selected terminal record (or a no-LF, torn, blank-record, before-terminal,
// wrong-ID, duplicate or relabeled variant of it) before it exits, so the
// harness reads exactly those bytes after every writer has stopped; and
// Cursor's per-project approval (FAKE_VENDOR_ENABLE actions project,
// project-dir, project-other and project-extra, written under the fixture
// HOME's .cursor/projects/<slug> derived from the working directory, with
// FAKE_VENDOR_APPROVALS as the file content) and its data directories
// (data, ancestor-chats, chats-link and workspace-flood).
//
// Design decoder-enrollment B2 adds: the three enrolled real output formats
// (FAKE_VENDOR_FORMAT codex-real, grok-real and claude-real: the inspected
// Codex 0.160.0, Grok 1.0.46 and Claude 2.1.292 shapes, with the case and
// nonce of the actual session and prose echoes of the nonce); Cursor's
// project permission file .cursor/cli.json (FAKE_VENDOR_PERMISSION require
// or absent: the session's own check of it before any probe;
// FAKE_VENDOR_ENABLE permission-check, permission-modify and
// permission-remove; FAKE_VENDOR_SESSION_PERMISSION modify or remove during
// the session; the argv log records what the session saw); Cursor's
// per-tool rejection with a final success envelope (SCENARIO rejected, no
// tool call reaches the probe); and a hostile help command that plants
// state in the next case workspace (FAKE_VENDOR_PLANT cli-json or
// cursor-link).
//
// B2 amendment A2 adds Cursor's own project schema check: mcp enable and
// the session exit 1 before doing anything when the working directory's
// .cursor/cli.json lacks a permissions.deny array, with the message
// Cursor Agent 2026.10.01-e373342 printed for the allow-only file.
//
// Design decoder-enrollment B3 adds: the reviewed real Cursor stream
// (FAKE_VENDOR_FORMAT cursor-real: the exact nested success structure with
// an opaque call ID holding one LF, the structured rejection with
// SCENARIO rejected, and an unseen timeout shape) and the worker socket
// residue Cursor leaves after a session (FAKE_VENDOR_RESIDUE): a Unix
// socket worker.sock, alone, in a second directory of the fixture HOME's
// .cursor/projects named from the working directory's project slug, its
// parent part P, the case basename B cut to FAKE_VENDOR_RESIDUE_K (default
// 2) characters and a seven-hex suffix, as observed; the variants full
// (the untruncated slug), regular (a regular file), extra (a sibling file)
// and two (a second candidate). The fake closes its listener without
// unlinking, so no process holds the socket; the harness must never open,
// connect to or remove it, and the test's temporary directory removes it.
// Design decoder-enrollment A3 adds the in-place layout (the exact session
// tree in the case's own full-slug project directory, its session files
// carrying a canary), its defects, both layouts at once, per-case mode
// lists and an observed mode selecting the layout by the test's workspace
// (see leaveResidue).
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	case "bind-socket":
		// A test fixture helper: bind the relative socket name
		// FAKE_VENDOR_SOCKET_NAME in the working directory (its absolute path
		// may exceed the platform's socket path limit) and close it without
		// unlinking, leaving a socket that no process holds.
		if err := bindSocket(fakeEnv("SOCKET_NAME")); err != nil {
			fmt.Fprintln(os.Stderr, "fake vendor: bind-socket:", err)
			return 1
		}
		return 0
	case "descendant":
		// A group member that ignores TERM until KILLed.
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, "descendant-ready")
		os.Stdout.Close()
		fakeSleepForever()
	case "resistant":
		signal.Ignore(syscall.SIGTERM)
	case "holder":
		// Holds the inherited stdout and stderr until killed.
		fakeSleepForever()
	case "leader":
		// A separately owned, pre-existing service (the owner's Grok
		// leader): it accepts connections on its socket and records each.
		return fakeLeader()
	}
	if len(args) == 3 && args[0] == "mcp" && args[1] == "enable" && args[2] == "probe" {
		fakeLog("enable %d", os.Getpid())
		if err := checkProjectSchema(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return fakeEnable()
	}
	if len(args) > 0 && args[0] == "--version" {
		fakeLog("version %d", os.Getpid())
		fmt.Println(fakeEnv("VERSION"))
		fmt.Fprint(os.Stderr, fakeEnv("VERSION_STDERR"))
		return int(fakeInt("VERSION_EXIT"))
	}
	if len(args) > 0 && args[len(args)-1] == "--help" {
		fakeLog("help %d", os.Getpid())
		plantWorkspace()
		help := fakeEnv("HELP")
		if help == "" {
			help = "usage: fake [options] (a fake vendor CLI for mcpqual function tests)"
		}
		fmt.Println(help)
		fmt.Fprint(os.Stderr, fakeEnv("HELP_STDERR"))
		return int(fakeInt("HELP_EXIT"))
	}
	var cfgPath, caseID string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--config", "--mcp-config":
			cfgPath = args[i+1]
		case "--case":
			caseID = args[i+1]
		}
	}
	if caseID == "" {
		caseID = promptCase(args)
	}
	recordArgv(args, cfgPath)
	fakeLog("session %d %s", os.Getpid(), caseID)
	if err := checkProjectSchema(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := checkRecipe(args, caseID); err != nil {
		fmt.Fprintln(os.Stderr, "fake vendor: recipe:", err)
		return 3
	}
	if err := checkPermission(); err != nil {
		fmt.Fprintln(os.Stderr, "fake vendor: permission:", err)
		return 3
	}
	switch fakeEnv("SESSION_PERMISSION") {
	case "modify":
		os.WriteFile(filepath.Join(".cursor", "cli.json"), []byte(`{"permissions":{"allow":["Mcp(probe:*)"]}}`), 0o600)
	case "remove":
		os.Remove(filepath.Join(".cursor", "cli.json"))
	}
	switch fakeEnv("OUTCOME") {
	case "approval-denied":
		// A managed policy still refuses the tool: no call reaches the probe.
		fmt.Println(`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","tool":"slow","status":"failed","error":{"message":"tool call rejected by approval policy"}}}`)
		fmt.Println(`{"type":"turn.completed"}`)
		return 0
	case "unsupported-config":
		fmt.Fprintln(os.Stderr, "Error: unknown variant `approve`, expected one of `auto`, `prompt`, `writes`\nin `mcp_servers.probe.tools.slow.approval_mode`")
		return 1
	case "trust-rejected":
		fmt.Fprintln(os.Stderr, "error: unexpected argument '--trust' found")
		return 2
	}
	if p := fakeEnv("LEADER"); p != "" {
		if err := connectLeader(p); err != nil {
			fmt.Fprintln(os.Stderr, "fake vendor: leader:", err)
			return 1
		}
	}
	switch mode {
	case "hang", "resistant":
		fakeReady("")
		fakeSleepForever()
	case "held":
		// A descendant in its own session (outside the reaped group) keeps
		// both pipes open after the leader exits; the test kills it.
		d := exec.Command(os.Args[0])
		d.Env = append(os.Environ(), "FAKE_VENDOR_MODE=holder")
		d.Stdout, d.Stderr = os.Stdout, os.Stderr
		d.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := d.Start(); err != nil {
			return 1
		}
		fakeLog("escaped %d", d.Process.Pid)
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
	transcript, err := fakeSession(args, cfgPath, caseID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake vendor:", err)
		return 1
	}
	// The probe has been reaped (fakeSession waits for it): the fixture's
	// server events are rewritten only now, after every writer stopped.
	if err := cutEvents(caseID); err != nil {
		fmt.Fprintln(os.Stderr, "fake vendor: cut:", err)
		return 1
	}
	if err := leaveResidue(); err != nil {
		fmt.Fprintln(os.Stderr, "fake vendor: residue:", err)
		return 1
	}
	flood(os.Stdout, "FLOOD_STDOUT")
	os.Stdout.WriteString(transcript)
	flood(os.Stderr, "FLOOD_STDERR")
	fmt.Fprint(os.Stderr, fakeEnv("STDERR"))
	return int(fakeInt("EXIT"))
}

// flood writes FAKE_VENDOR_<k> bytes of padding lines to w.
func flood(w io.Writer, k string) {
	n := fakeInt(k)
	line := strings.Repeat("f", 99) + "\n"
	for ; n > 0; n -= int64(len(line)) {
		io.WriteString(w, line[:min(int64(len(line)), n)])
	}
}

var promptCaseRe = regexp.MustCompile(`"case_id": "([^"]+)"`)

// promptCase finds the case named by the scripted prompt.
func promptCase(args []string) string {
	for _, a := range args {
		if m := promptCaseRe.FindStringSubmatch(a); m != nil {
			return m[1]
		}
	}
	return ""
}

// recordArgv appends the session's argv, working directory and the bytes
// of every configuration it can find to FAKE_VENDOR_ARGV_LOG.
func recordArgv(args []string, cfgPath string) {
	p := fakeEnv("ARGV_LOG")
	if p == "" {
		return
	}
	wd, _ := os.Getwd()
	rec := map[string]any{"argv": args, "cwd": wd, "permission": permissionSeen()}
	for _, c := range []string{cfgPath, filepath.Join(wd, ".grok", "config.toml"), filepath.Join(wd, ".cursor", "mcp.json"), filepath.Join(wd, "probe-config.toml")} {
		if b, err := os.ReadFile(c); c != "" && err == nil {
			rec["config"] = string(b)
			rec["config_path"] = c
			break
		}
	}
	// The case file the probe serves (design decoder-enrollment B1.5 round
	// 3): its cases and delays are the structured evidence of what the
	// session was asked to run.
	if _, srvArgs, err := probeServer(args, cfgPath); err == nil {
		for i, a := range srvArgs {
			if a == "--case-file" && i+1 < len(srvArgs) {
				if b, err := os.ReadFile(srvArgs[i+1]); err == nil {
					rec["case_file"] = string(b)
				}
			}
		}
	}
	b, _ := json.Marshal(rec)
	if f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
		f.Write(append(b, '\n'))
		f.Close()
	}
}

// probeServer finds the probe's command and args in the recipe shapes.
func probeServer(args []string, cfgPath string) (string, []string, error) {
	var server struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		k, v, _ := strings.Cut(args[i+1], "=")
		switch k {
		case "mcp_servers.probe.command":
			json.Unmarshal([]byte(v), &server.Command)
		case "mcp_servers.probe.args":
			json.Unmarshal([]byte(v), &server.Args)
		}
	}
	if server.Command != "" {
		return server.Command, server.Args, nil
	}
	wd, _ := os.Getwd()
	if cfgPath == "" {
		if _, err := os.Stat(filepath.Join(wd, ".cursor", "mcp.json")); err == nil {
			cfgPath = filepath.Join(wd, ".cursor", "mcp.json")
		}
	}
	if cfgPath == "" {
		// The grok shape: a TOML table whose values are JSON literals.
		b, err := os.ReadFile(filepath.Join(wd, ".grok", "config.toml"))
		if err != nil {
			return "", nil, err
		}
		for _, l := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(l, " = ")
			switch k {
			case "command":
				json.Unmarshal([]byte(v), &server.Command)
			case "args":
				json.Unmarshal([]byte(v), &server.Args)
			}
		}
		return server.Command, server.Args, nil
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", nil, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", nil, err
	}
	srv := cfg.MCPServers["probe"]
	return srv.Command, srv.Args, nil
}

// fakeSession runs one MCP session against the configured probe.
func fakeSession(args []string, cfgPath, caseID string) (string, error) {
	var cfg struct {
		TimeoutMS int64 `json:"timeout_ms"`
	}
	if cfgPath != "" {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return "", err
		}
	}
	command, srvArgs, err := probeServer(args, cfgPath)
	if err != nil {
		return "", err
	}
	var caseFile string
	for i, a := range srvArgs {
		if a == "--case-file" && i+1 < len(srvArgs) {
			caseFile = srvArgs[i+1]
		}
		if a == "--events" && i+1 < len(srvArgs) {
			sessionEvents = srvArgs[i+1]
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
	case "noprobe":
		// An unauthenticated CLI or unknown model: no session, no probe.
		return "", nil
	}
	probe := exec.Command(command, srvArgs...)
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
	requested := fakeEnv("PROTOCOL")
	if requested == "" {
		requested = mcpqual.ProtocolVersion
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": requested,
		"capabilities": map[string]any{}, "clientInfo": map[string]any{"name": name, "version": version}}})
	initRes, err := await("1")
	if err != nil {
		return "", err
	}
	var negotiated struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(initRes["result"], &negotiated)
	if fakeEnv("PROTOCOL") != "" {
		fmt.Fprintf(os.Stderr, "negotiated protocol %s for requested %s\n", negotiated.ProtocolVersion, requested)
	}
	if fakeEnv("DECLINE") == "1" && negotiated.ProtocolVersion != requested {
		// A client that cannot use the selected version disconnects.
		return "", fmt.Errorf("the server selected protocol %s; this client supports only %s", negotiated.ProtocolVersion, requested)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if _, err := await("2"); err != nil {
		return "", err
	}
	switch fakeEnv("SCENARIO") {
	case "no-tool":
		return fakeTranscript(format, "no-tool", caseID, ""), nil
	case "rejected":
		// Cursor's per-tool permission refuses the call before it reaches
		// the probe; the run still ends with a success envelope.
		if format == "cursor-real" {
			// The reviewed real rejection shape (design decoder-enrollment B3,
			// FP-22): started, completed with result.rejected, success.
			sessionOutcome = "rejected"
			return cursorRealTranscript("rejected", caseID, ""), nil
		}
		return `{"type":"tool_call","subtype":"completed","call_id":"c1","tool_call":{"mcpToolCall":{"args":{"providerIdentifier":"probe","toolName":"slow","args":{"case_id":` +
			strconv.Quote(caseID) + `}},"result":{"rejected":{"reason":"User rejected MCP: probe-slow"}}}}}` + "\n" +
			`{"type":"result","subtype":"success","is_error":false,"result":"The tool was rejected."}` + "\n", nil
	case "marker":
		send(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": map[string]any{"name": "slow", "arguments": map[string]any{"case_id": caseID + "-marker"}}})
		if _, err := await("9"); err != nil {
			return "", err
		}
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
		sessionOutcome = "timeout"
		return fakeTranscript(format, "timeout", caseID, ""), nil
	}
	res, err := await("3")
	if err != nil {
		return "", err
	}
	if fakeEnv("SCENARIO") == "extra" {
		// A second call of the same case: an extra receipt.
		send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": params})
		if _, err := await("4"); err != nil {
			return "", err
		}
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
	sessionOutcome = "success"
	return fakeTranscript(format, "success", caseID, text), nil
}

// sessionEvents and sessionOutcome are the session's probe events file and
// its outcome (success or timeout), for cutEvents.
var sessionEvents, sessionOutcome string

// cutEvents rewrites the reaped probe's events file (design
// decoder-enrollment B1.5) when FAKE_VENDOR_CUT names a variant and
// FAKE_VENDOR_CUT_ON (when set) names this session's outcome. The
// selected terminal is the case's last completed, cancelled or eof record:
//
//	terminal   the prefix ending at it, LF-terminated (no exit record)
//	nolf       the same without its final LF
//	torn       the same followed by half of the next record, without LF
//	blank      the same followed by a blank record
//	before     the prefix before it
//	wrongid    the prefix with the terminal's request_id replaced by 99
//	dup        the prefix with the terminal record repeated (next seq)
//	cancelled  the prefix with the terminal relabeled cancelled
//	eof        the prefix with the terminal relabeled eof
//	otherrun   the prefix with the call's records under another run ID
//	wrongid-intact  the whole log, the terminal's request ID a string
func cutEvents(caseID string) error {
	mode := fakeEnv("CUT")
	if mode == "" || sessionEvents == "" || fakeEnv("CUT_ON") != "" && fakeEnv("CUT_ON") != sessionOutcome {
		return nil
	}
	raw, err := os.ReadFile(sessionEvents)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	term := -1
	var ev map[string]any
	for i, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			return fmt.Errorf("record %d is not JSON", i+1)
		}
		switch m["kind"] {
		case "completed", "cancelled", "eof":
			if m["case_id"] == caseID {
				term, ev = i, m
			}
		}
	}
	if term < 0 {
		return errors.New("no terminal record of " + caseID)
	}
	encode := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }
	prefix := strings.Join(lines[:term+1], "\n") + "\n"
	switch mode {
	case "terminal":
	case "nolf":
		prefix = strings.TrimSuffix(prefix, "\n")
	case "torn":
		next := `{"seq":99,"kind":"exit","offset_ns":1}`
		if term+1 < len(lines) {
			next = lines[term+1]
		}
		prefix += next[:len(next)/2]
	case "blank":
		prefix += "\n"
	case "before":
		prefix = strings.Join(lines[:term], "\n") + "\n"
	case "wrongid", "cancelled", "eof":
		switch mode {
		case "wrongid":
			ev["request_id"] = 99
		default:
			ev["kind"] = mode
		}
		prefix = strings.Join(append(append([]string(nil), lines[:term]...), encode(ev)), "\n") + "\n"
	case "dup":
		ev["seq"] = ev["seq"].(float64) + 1
		prefix += encode(ev) + "\n"
	case "otherrun":
		// The call's records claim another run than their start's.
		var out []string
		for _, l := range lines[:term+1] {
			var m map[string]any
			json.Unmarshal([]byte(l), &m)
			if k := m["kind"]; k != "start" && k != "initialize" {
				m["run_id"] = "different-instance"
				l = encode(m)
			}
			out = append(out, l)
		}
		prefix = strings.Join(out, "\n") + "\n"
	case "wrongid-intact":
		// The whole intact log, the terminal's request ID changed.
		ev["request_id"] = "3"
		prefix = strings.Join(append(append(append([]string(nil), lines[:term]...), encode(ev)), lines[term+1:]...), "\n") + "\n"
	default:
		return errors.New("unknown cut " + mode)
	}
	return os.WriteFile(sessionEvents, []byte(prefix), 0o600)
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
	if outcome == "no-tool" {
		// The model answered without calling the tool.
		return `{"type":"result","subtype":"success","result":` + q(prose) + `}` + "\n"
	}
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
	case "grok-stream":
		// Grok's streaming-json (ACP-derived, one type-tagged object per
		// line): secret canaries in a structured rawInput member and inside
		// an embedded JSON string of rawOutput, and numeric usage.
		secret := fakeEnv("SECRET")
		lines := []string{`{"type":"text","data":` + q(prose) + `}`,
			`{"type":"tool_call","toolCallId":"call_1","title":"probe__slow","kind":"other","status":"in_progress","toolName":"probe__slow","rawInput":{"case_id":` + q(caseID) + `,"api_key":` + q(secret) + `},"content":[],"locations":[]}`,
			`{"type":"tool_call_update","toolCallId":"call_1","status":"completed","rawOutput":{"content":[{"type":"text","text":` + q(resultText) + `}],"meta":` + q(`{"token":"`+secret+`"}`) + `},"content":[],"locations":[]}`,
			`{"type":"usage","messageId":"resp_1","stopReason":"tool_use","usage":{"input_tokens":812,"output_tokens":45,"cache_read_input_tokens":0},"signature":"sig-1"}`}
		if fakeEnv("MALFORMED") == "1" {
			// A cut line with an escaped credential member: unsafe input.
			lines = append(lines, `{"type":"tool_call_update","rawOutput":"{\"password\":\"`+secret)
		}
		lines = append(lines, `{"type":"end","stopReason":"end_turn","sessionId":"s-1","usage":{"input_tokens":900,"output_tokens":60},"num_turns":2}`)
		return strings.Join(lines, "\n") + "\n"
	case "codex-real", "grok-real", "claude-real":
		return realTranscript(format, outcome, caseID, resultText, prose)
	case "cursor-real":
		return cursorRealTranscript(outcome, caseID, resultText)
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

// realTranscript renders the inspected real format of an enrolled version
// (design decoder-enrollment B2, FP-17) for this session's own case and
// result. Success has the structured result plus prose echoes of it; a
// timeout or an authentication failure is an unseen, unenrolled shape (a
// failed tool item, an is_error result, a failed turn), never a typed
// event the real decoders recognize.
func realTranscript(format, outcome, caseID, resultText, prose string) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	switch format {
	case "codex-real":
		lines := []string{`{"type":"thread.started","thread_id":"t-fake"}`, `{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":` + q(prose) + `}}`}
		start := `{"type":"item.started","item":{"id":"item_1","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":` + q(caseID) +
			`},"result":null,"error":null,"status":"in_progress"}}`
		switch outcome {
		case "auth":
			lines = append(lines, `{"type":"turn.failed","error":{"message":"unauthorized"}}`)
		case "timeout":
			lines = append(lines, start, `{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":`+q(caseID)+
				`},"result":null,"error":{"message":"timed out"},"status":"failed"}}`, `{"type":"turn.completed","usage":{"input_tokens":1}}`)
		default:
			lines = append(lines, start, `{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":`+q(caseID)+
				`},"result":{"content":[{"type":"text","text":`+q(resultText)+`}],"structured_content":null},"error":null,"status":"completed"}}`,
				`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":`+q(resultText)+`}}`, `{"type":"turn.completed","usage":{"input_tokens":1}}`)
		}
		return strings.Join(lines, "\n") + "\n"
	case "grok-real":
		lines := []string{`{"type":"available_commands","tools":["use_tool","probe__slow"],"commands":["compact"]}`, `{"type":"thought","data":` + q(prose) + `}`}
		call := `{"type":"tool_call","toolCallId":"call-fake-0","title":"use_tool","kind":"use_tool","status":"pending","toolName":"use_tool","rawInput":{"tool_name":"probe__slow","tool_input":{"case_id":` +
			q(caseID) + `}},"content":[],"locations":[]}`
		interim := `{"type":"tool_call_update","toolCallId":"call-fake-0","status":null,"content":[],"rawOutput":null,"locations":[]}`
		switch outcome {
		case "auth":
			lines = append(lines, `{"type":"end","stopReason":"refusal","sessionId":"s"}`)
		case "timeout":
			lines = append(lines, call, interim, `{"type":"tool_call_update","toolCallId":"call-fake-0","status":"failed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"ErrOutput":"timed out"}},"locations":[]}`,
				`{"type":"end","stopReason":"end_turn","sessionId":"s"}`)
		default:
			lines = append(lines, call, interim, `{"type":"tool_call_update","toolCallId":"call-fake-0","status":"completed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"OkayOutput":`+
				q(resultText)+`}},"locations":[]}`, `{"type":"text","data":`+q(resultText)+`}`, `{"type":"usage","usage":{"input_tokens":1},"signature":"sig"}`,
				`{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r","total_cost_usd":0.03165128,"total_cost_usd_ticks":316512800,`+
					`"modelUsage":{"grok-4.7-build":{"modelCalls":2,"costUSD":0.03165128}}}`)
		}
		return strings.Join(lines, "\n") + "\n"
	}
	msgs := []string{`{"type":"system","subtype":"init","cwd":"w","session_id":"s","tools":["mcp__probe__slow"]}`}
	call := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":` + q(prose) + `},{"type":"tool_use","id":"toolu_F","name":"mcp__probe__slow","input":{"case_id":` + q(caseID) + `}}]}}`
	// The per-run cost members (design decoder-enrollment B2, amendment A1):
	// numbers as observed, or a string with FAKE_VENDOR_COST=string.
	cost := `0.1227972`
	if fakeEnv("COST") == "string" {
		cost = `"0.12"`
	}
	end := `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":` + q(resultText) + `,"total_cost_usd":` + cost +
		`,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":6,"costUSD":0.1227972,"costBasis":"list"}}}`
	switch outcome {
	case "auth":
		msgs = append(msgs, `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"error","result":`+q(prose)+`}`)
	case "timeout":
		msgs = append(msgs, call, `{"type":"user","message":{"content":[{"tool_use_id":"toolu_F","type":"tool_result","is_error":true,"content":[{"type":"text","text":"MCP error -32001: Request timed out"}]}]}}`, end)
	default:
		msgs = append(msgs, call, `{"type":"user","message":{"content":[{"tool_use_id":"toolu_F","type":"tool_result","content":[{"type":"text","text":`+q(resultText)+`}]}]}}`,
			`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","utilization":0.25,"resetsAt":1791470400},"uuid":"u"}`, end)
	}
	return "[" + strings.Join(msgs, ",") + "]\n"
}

// sameDir reports whether p names the working directory.
func sameDir(p string) bool {
	wd, err1 := os.Stat(".")
	other, err2 := os.Stat(p)
	return err1 == nil && err2 == nil && os.SameFile(wd, other)
}

// flagValue is the argument after the only occurrence of flag (ok false
// when absent or repeated).
func flagValue(args []string, flag string) (string, bool) {
	value, n := "", 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			value, n = args[i+1], n+1
		}
	}
	return value, n == 1
}

// checkRecipe is the B1 recipes' own check of their invocation (design
// decoder-enrollment B1): the exact scoped grant, workspace and
// configuration, and nothing broader, or the session refuses before any
// probe.
func checkRecipe(args []string, caseID string) error {
	for _, a := range args {
		for _, bad := range []string{"--full-auto", "--dangerously", "--yolo", "--approve-mcps", "--ask-for-approval", "approval_policy", "--sandbox", "--force"} {
			if strings.HasPrefix(a, bad) {
				return fmt.Errorf("a broader grant %q", a)
			}
		}
	}
	prompt, _ := flagValue(args, "-p")
	switch fakeEnv("RECIPE") {
	case "codex":
		var overrides []string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-c" {
				overrides = append(overrides, args[i+1])
			}
		}
		ws, ok := flagValue(args, "-C")
		cfg, err := os.ReadFile("probe-config.toml")
		switch {
		case !ok || !sameDir(ws) || err != nil:
			return fmt.Errorf("workspace %q or its probe-config.toml snapshot (%v)", ws, err)
		case len(overrides) != 4 || !strings.HasPrefix(overrides[0], "mcp_servers.probe.command=") || !strings.HasPrefix(overrides[1], "mcp_servers.probe.args=") ||
			overrides[2] != `mcp_servers.probe.enabled_tools=["slow"]` || overrides[3] != `mcp_servers.probe.tools.slow.approval_mode="approve"`:
			return fmt.Errorf("the overrides %q are not exactly the probe.slow grant", overrides)
		case !strings.Contains(string(cfg), "enabled_tools = [\"slow\"]\n[mcp_servers.probe.tools.slow]\napproval_mode = \"approve\"\n"):
			return fmt.Errorf("the configuration snapshot %q", cfg)
		}
	case "grok":
		cwd, ok := flagValue(args, "--cwd")
		format, _ := flagValue(args, "--output-format")
		_, grokHome := os.LookupEnv("GROK_HOME")
		_, cfgErr := os.Stat(filepath.Join(".grok", "config.toml"))
		switch {
		case len(args) == 0 || args[0] != "--trust" || strings.Count(strings.Join(args, "\x00"), "--trust") != 1:
			return fmt.Errorf("no single global --trust in %q", args)
		case !ok || !sameDir(cwd) || cfgErr != nil:
			return fmt.Errorf("--cwd %q is not the workspace with its .grok/config.toml (%v)", cwd, cfgErr)
		case format != "streaming-json" || !strings.Contains(prompt, "Call the MCP tool probe__slow exactly once") || !strings.Contains(prompt, caseID):
			return fmt.Errorf("output %q or prompt %q", format, prompt)
		case os.Getenv("HOME") != fakeEnv("EXPECT_HOME") || grokHome:
			return fmt.Errorf("HOME %q or GROK_HOME changed", os.Getenv("HOME"))
		}
	case "cursor":
		ws, ok := flagValue(args, "--workspace")
		_, approved := os.Stat(filepath.Join(".cursor", "approved-servers.json"))
		if wd, _ := os.Getwd(); approved != nil {
			// Design decoder-enrollment B1.5: the per-project approval.
			slug := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(wd, "/"), "/.work/", "/work/"), "/", "-")
			_, approved = os.Stat(filepath.Join(os.Getenv("HOME"), ".cursor", "projects", slug, "mcp-approvals.json"))
		}
		switch {
		case len(args) == 0 || args[len(args)-1] != "--trust" || !ok || !sameDir(ws):
			return fmt.Errorf("--workspace %q --trust is not the generated workspace", ws)
		case approved != nil:
			return fmt.Errorf("the session started before the workspace approval (%v)", approved)
		case !strings.Contains(prompt, "Call the MCP tool slow exactly once"):
			return fmt.Errorf("prompt %q", prompt)
		}
	}
	return nil
}

// cursorPermission is the exact project permission file of the pinned
// Cursor adapter (design decoder-enrollment B2, FP-20, amendment A2).
const cursorPermission = `{"permissions":{"allow":["Mcp(probe:slow)"],"deny":[]}}`

// permissionSeen is what the working directory's .cursor/cli.json holds:
// "<absent>", or "<mode> <content>" for a regular file ("<not regular>"
// otherwise).
func permissionSeen() string {
	p := filepath.Join(".cursor", "cli.json")
	info, err := os.Lstat(p)
	switch {
	case err != nil:
		return "<absent>"
	case !info.Mode().IsRegular():
		return "<not regular>"
	}
	b, _ := os.ReadFile(p)
	return fmt.Sprintf("%o %s", info.Mode().Perm(), b)
}

// checkProjectSchema mirrors Cursor Agent 2026.10.01-e373342's own
// validation of the working directory's project .cursor/cli.json (design
// decoder-enrollment B2, amendment A2): every command that loads the
// project configuration (mcp enable and the agent session) fails before
// doing anything when the file exists and is not JSON or lacks a
// permissions.deny array, with the vendor's observed message. An absent
// file is no project configuration and passes.
func checkProjectSchema() error {
	p := filepath.Join(".cursor", "cli.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	abs, _ := filepath.Abs(p)
	var cfg struct {
		Permissions *struct {
			Deny json.RawMessage `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("Invalid project config at %s: %v", abs, err)
	}
	received := "undefined"
	if cfg.Permissions != nil && cfg.Permissions.Deny != nil {
		var deny any
		json.Unmarshal(cfg.Permissions.Deny, &deny)
		if _, ok := deny.([]any); ok {
			return nil
		}
		received = "null"
		if deny != nil {
			received = fmt.Sprintf("%T", deny)
		}
	}
	if cfg.Permissions == nil {
		return fmt.Errorf(`Invalid project config at %s: schema validation failed. [{"code":"invalid_type","expected":"object","received":"undefined","path":["permissions"],"message":"Required"}]`, abs)
	}
	return fmt.Errorf(`Invalid project config at %s: schema validation failed. [{"code":"invalid_type","expected":"array","received":%q,"path":["permissions","deny"],"message":"Required"}]`, abs, received)
}

// checkPermission is the Cursor session's own check of its project
// permission file (FAKE_VENDOR_PERMISSION): require the exact bytes in a
// 0600 regular file in the working directory, or require its absence.
func checkPermission() error {
	switch got := permissionSeen(); fakeEnv("PERMISSION") {
	case "require":
		if got != "600 "+cursorPermission {
			return fmt.Errorf("the project permission file is %q", got)
		}
	case "absent":
		if got != "<absent>" {
			return fmt.Errorf("an unexpected project permission file %q", got)
		}
	}
	return nil
}

// plantWorkspace is a hostile help command (FAKE_VENDOR_PLANT): from its
// own working directory (.work/cursor-help) it prepares the next case
// workspace (.work/cursor-capture-setup) with an owner cli.json, or with a
// .cursor that is a symbolic link to a directory elsewhere.
func plantWorkspace() {
	wd, _ := os.Getwd()
	ws := filepath.Join(filepath.Dir(wd), "cursor-capture-setup")
	switch fakeEnv("PLANT") {
	case "cli-json":
		os.MkdirAll(filepath.Join(ws, ".cursor"), 0o700)
		os.WriteFile(filepath.Join(ws, ".cursor", "cli.json"), []byte(`{"permissions":{"deny":["Shell(rm)"]}}`), 0o600)
	case "cursor-link":
		other := filepath.Join(filepath.Dir(wd), "elsewhere")
		os.MkdirAll(other, 0o700)
		os.MkdirAll(ws, 0o700)
		os.Symlink(other, filepath.Join(ws, ".cursor"))
	}
}

// fakeEnable is "mcp enable probe": it writes the approved list where
// FAKE_VENDOR_ENABLE says (the workspace by default, the owner's home or
// the workspace's parent), does nothing, fails, or leaves a symbolic link.
//
// FAKE_VENDOR_ENABLE is a comma-separated list of actions (design
// decoder-enrollment B1.5 adds the project and data ones): project writes
// the fixture HOME's .cursor/projects/<slug>/mcp-approvals.json, <slug>
// being the working directory's path without its leading '/', '/' as '-'
// and the .work component as work (the 2026-10-08 observation), with
// FAKE_VENDOR_APPROVALS or the observed entry as content; project-dir
// creates only that directory; project-other writes the file under
// another slug; project-extra adds a second file to the project
// directory; data adds files under HOME's .cursor/chats and
// .cursor/ai-tracking; ancestor-chats writes the parent's
// .cursor/chats/note.json; chats-link replaces HOME's .cursor/chats with a
// symbolic link; workspace-flood writes FAKE_VENDOR_FLOOD_FILES files
// under the workspace's .cursor.
func fakeEnable() int {
	wd, _ := os.Getwd()
	home := os.Getenv("HOME")
	write := func(dir string) {
		os.MkdirAll(filepath.Join(dir, ".cursor"), 0o700)
		os.WriteFile(filepath.Join(dir, ".cursor", "approved-servers.json"), []byte(`{"approved":["probe"]}`+"\n"), 0o600)
	}
	slug := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(wd, "/"), "/.work/", "/work/"), "/", "-")
	project := func(slug string) string { return filepath.Join(home, ".cursor", "projects", slug) }
	approvals := fakeEnv("APPROVALS")
	if approvals == "" {
		approvals = `["probe-6e58c4b6c129cbd0"]`
	}
	put := func(p, data string) {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(data), 0o600)
	}
	actions := strings.Split(fakeEnv("ENABLE"), ",")
	message := "probe enabled"
	for _, action := range actions {
		switch action {
		case "", "workspace":
			write(wd)
		case "owner":
			write(home)
		case "ancestor":
			write(filepath.Dir(wd))
		case "noop":
			fmt.Println("probe is already enabled")
			return 0
		case "fail":
			fmt.Fprintln(os.Stderr, "error: cannot enable probe")
			return 1
		case "symlink":
			write(wd)
			os.Symlink("/", filepath.Join(wd, ".cursor", "link"))
		case "project":
			put(filepath.Join(project(slug), "mcp-approvals.json"), approvals)
			// Like the real CLI's path-bearing output: the slug encodes the
			// absolute workspace, so evidence must normalize it.
			message = "probe enabled in project " + slug
		case "project-dir":
			os.MkdirAll(project(slug), 0o700)
		case "project-other":
			put(filepath.Join(project(slug+"-other"), "mcp-approvals.json"), approvals)
		case "project-extra":
			put(filepath.Join(project(slug), "mcp-approvals.json"), approvals)
			put(filepath.Join(project(slug), "extra.json"), "{}")
		case "data":
			put(filepath.Join(home, ".cursor", "chats", "enable-chat.jsonl"), `{"chat":"new"}`+"\n")
			put(filepath.Join(home, ".cursor", "ai-tracking", "enable.db"), "tracking")
		case "ancestor-chats":
			put(filepath.Join(filepath.Dir(wd), ".cursor", "chats", "note.json"), "{}")
		case "chats-link":
			os.RemoveAll(filepath.Join(home, ".cursor", "chats"))
			os.Symlink("/", filepath.Join(home, ".cursor", "chats"))
		case "workspace-flood":
			for i := int64(0); i < fakeInt("FLOOD_FILES"); i++ {
				put(filepath.Join(wd, ".cursor", "flood", fmt.Sprintf("f-%d.json", i)), "{}")
			}
		case "permission-check":
			// Design decoder-enrollment B2: the harness file is baseline
			// state, already present when enable runs.
			if got := permissionSeen(); got != "600 "+cursorPermission {
				fmt.Fprintf(os.Stderr, "error: the project permission file is %q\n", got)
				return 1
			}
		case "permission-modify":
			put(filepath.Join(wd, ".cursor", "cli.json"), `{"permissions":{"allow":["Mcp(*:*)"]}}`)
		case "permission-remove":
			os.Remove(filepath.Join(wd, ".cursor", "cli.json"))
		}
	}
	fmt.Println(message)
	return 0
}

// fakeLeader is a pre-existing owner service in the directory
// FAKE_VENDOR_LEADER: forever, it reads one request from req.fifo and
// answers "ok <request>" on ack.fifo, until its owner kills it.
func fakeLeader() int {
	dir := fakeEnv("LEADER")
	fakeReady("leader")
	for {
		req, err := os.ReadFile(filepath.Join(dir, "req.fifo"))
		if err != nil {
			return 1
		}
		if err := os.WriteFile(filepath.Join(dir, "ack.fifo"), append([]byte("ok "), req...), 0o600); err != nil {
			return 1
		}
	}
}

// connectLeader uses the owner's leader: one request and its answer.
func connectLeader(dir string) error {
	req := fmt.Sprintf("session %d\n", os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "req.fifo"), []byte(req), 0o600); err != nil {
		return err
	}
	ack, err := os.ReadFile(filepath.Join(dir, "ack.fifo"))
	if err == nil && string(ack) != "ok "+req {
		err = errors.New("unexpected leader answer " + string(ack))
	}
	return err
}

// cursorRealCallID is the fake's opaque Cursor call ID: two parts joined by
// one LF, as the reviewed capture's.
const cursorRealCallID = "call-fake-0" + "\n" + "fc_fake_0"

// cursorRealTranscript renders the reviewed Cursor 2026.10.01-e373342
// stream-json shapes (design decoder-enrollment B3, FP-22) for this
// session's case: success carries the structured result and a prose echo;
// rejected the structured rejection followed by a success envelope; a
// timeout is an unseen shape (another result alternative), never enrolled.
func cursorRealTranscript(outcome, caseID, resultText string) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	id := q(cursorRealCallID)
	args := `{"name":"probe-slow","args":{"case_id":` + q(caseID) + `},"toolCallId":` + id +
		`,"providerIdentifier":"probe","toolName":"slow","smartModeApprovalOnly":false,"skipApproval":false,"serverIdentifier":"probe"}`
	tool := func(sub, result string) string {
		return `{"type":"tool_call","subtype":"` + sub + `","call_id":` + id + `,"tool_call":{"mcpToolCall":{"args":` + args + result +
			`,"description":"Run the slow probe"},"hookAdditionalContexts":[],"toolCallId":` + id + `,"startedAtMs":"1791494828487"},"model_call_id":"m-fake","session_id":"s-fake","timestamp_ms":1791494828514}`
	}
	lines := []string{`{"type":"system","subtype":"init","apiKeySource":"[REDACTED]","cwd":"w","session_id":"s-fake","model":"Fake","permissionMode":"default"}`,
		`{"type":"thinking","subtype":"delta","text":"The tool may time out.","session_id":"s-fake","timestamp_ms":1}`, tool("started", "")}
	var result string
	switch outcome {
	case "rejected":
		result = `{"rejected":{"reason":"User rejected MCP: probe-slow"}}`
	case "timeout":
		result = `{"error":{"message":"MCP error -32001: Request timed out"}}`
	default:
		result = `{"success":{"content":[{"text":{"text":` + q(resultText) + `}}],"isError":false,"systemReminders":[]}}`
	}
	lines = append(lines, tool("completed", `,"result":`+result),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":`+q(resultText)+`}]},"session_id":"s-fake"}`,
		`{"type":"result","subtype":"success","duration_ms":7,"duration_api_ms":7,"is_error":false,"result":`+q(resultText)+`,"session_id":"s-fake","request_id":"r-fake","usage":{}}`)
	return strings.Join(lines, "\n") + "\n"
}

// leaveResidue leaves the session residue FAKE_VENDOR_RESIDUE names under
// HOME's .cursor/projects, after the session's probe was reaped. The value
// is one mode, or a comma list of case-basename=mode pairs (a case not
// listed leaves none). Modes: attributed or hashed (the hashed second
// directory holding only worker.sock), its defects full, regular, extra and
// two; in_place (the exact in-place tree in the case's own full-slug
// project directory, design decoder-enrollment A3), its defects
// in_place-missing, in_place-extra, in_place-uuid, in_place-link,
// in_place-fifo, in_place-big and in_place-partial; mixed (both layouts);
// observed (in_place when the project slug has at most
// FAKE_VENDOR_RESIDUE_SHORT characters, default 57, else hashed: fixture
// control over the test-selected workspace, never a production switch).
func leaveResidue() error {
	mode := fakeEnv("RESIDUE")
	wd, _ := os.Getwd()
	base := filepath.Base(wd)
	if strings.Contains(mode, "=") {
		pick := ""
		for _, pair := range strings.Split(mode, ",") {
			if b, m, ok := strings.Cut(pair, "="); ok && b == base {
				pick = m
			}
		}
		mode = pick
	}
	if mode == "" {
		return nil
	}
	slug := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(wd, "/"), "/.work/", "/work/"), "/", "-")
	if mode == "observed" {
		short := int(fakeInt("RESIDUE_SHORT"))
		if short == 0 {
			short = 57
		}
		mode = "hashed"
		if len(slug) <= short {
			mode = "in_place"
		}
	}
	projects := filepath.Join(os.Getenv("HOME"), ".cursor", "projects")
	if mode == "in_place" || mode == "mixed" || strings.HasPrefix(mode, "in_place-") {
		if err := leaveInPlace(filepath.Join(projects, slug), slug, wd, strings.TrimPrefix(mode, "in_place-")); err != nil || mode != "mixed" {
			return err
		}
	}
	parent := strings.TrimSuffix(slug, "-"+base)
	k := int(fakeInt("RESIDUE_K"))
	if k == 0 {
		k = 2
	}
	sum := sha256.Sum256([]byte(slug))
	suffix := hex.EncodeToString(sum[:])[:7]
	name := parent + "-" + base[:min(k, len(base))] + "-" + suffix
	if mode == "full" {
		name = slug + "-" + suffix
	}
	dir := filepath.Join(projects, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	switch mode {
	case "regular":
		return os.WriteFile(filepath.Join(dir, "worker.sock"), nil, 0o600)
	case "extra":
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600); err != nil {
			return err
		}
	case "two":
		other := filepath.Join(projects, parent+"-"+base[:min(k+1, len(base))]+"-"+hex.EncodeToString(sum[:])[7:14])
		if err := os.MkdirAll(other, 0o700); err != nil {
			return err
		}
		if err := makeSocket(other); err != nil {
			return err
		}
	}
	return makeSocket(dir)
}

// residueCanary is in every in-place session file the fake writes; it must
// never reach any evidence.
const residueCanary = "fakevendor-session-canary-5d1e"

// leaveInPlace writes the in-place session tree into the case's project
// directory dir (the approval file is enable's), as observed: worker.sock,
// worker.log, repo.json, .workspace-trusted and
// agent-transcripts/<U>/<U>.jsonl, U a lowercase UUID; defect changes it.
func leaveInPlace(dir, slug, wd, defect string) error {
	sum := sha256.Sum256([]byte("transcript " + slug))
	h := hex.EncodeToString(sum[:16])
	uuid := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	file := uuid
	if defect == "uuid" {
		// A different first character, always (the UUID varies per path).
		first := "0"
		if uuid[0] == '0' {
			first = "1"
		}
		file = first + uuid[1:]
	}
	files := map[string]string{
		"worker.log":         "[info] runServer socketPath=" + filepath.Join(dir, "worker.sock") + " " + residueCanary + "\n",
		"repo.json":          `{"id": "` + uuid + `"}`,
		".workspace-trusted": `{"trustedAt": "2026-10-09T12:39:04.000Z", "workspacePath": "` + wd + `", "trustMethod": "cli-flag", "note": "` + residueCanary + `"}`,
		filepath.Join("agent-transcripts", uuid, file+".jsonl"): `{"role":"user","message":"` + residueCanary + `"}` + "\n",
	}
	switch defect {
	case "missing":
		// The transcript directory without its transcript.
		delete(files, filepath.Join("agent-transcripts", uuid, file+".jsonl"))
		if err := os.MkdirAll(filepath.Join(dir, "agent-transcripts", uuid), 0o700); err != nil {
			return err
		}
	case "extra":
		files["state.json"] = `{"note": "` + residueCanary + `"}`
	case "big":
		files["worker.log"] = strings.Repeat("x", 1<<20+1)
	case "partial":
		files = map[string]string{"worker.log": files["worker.log"]}
	}
	for rel, data := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			return err
		}
	}
	switch defect {
	case "link":
		if err := os.Remove(filepath.Join(dir, "repo.json")); err != nil {
			return err
		}
		if err := os.Symlink(".workspace-trusted", filepath.Join(dir, "repo.json")); err != nil {
			return err
		}
	case "fifo":
		if err := syscall.Mkfifo(filepath.Join(dir, "worker.pipe"), 0o600); err != nil {
			return err
		}
	}
	return makeSocket(dir)
}

// bindSocket binds the relative Unix socket name in the working directory
// and closes it without unlinking.
func bindSocket(name string) error {
	l, err := net.Listen("unix", name)
	if err != nil {
		return err
	}
	ul := l.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	return ul.Close()
}

// makeSocket creates dir/worker.sock as a Unix socket that no process
// holds: bound by its short relative name (the absolute path may exceed
// the platform's socket path limit), then closed without unlinking.
func makeSocket(dir string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	defer os.Chdir(wd)
	l, err := net.Listen("unix", "worker.sock")
	if err != nil {
		return err
	}
	ul := l.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	return ul.Close()
}
