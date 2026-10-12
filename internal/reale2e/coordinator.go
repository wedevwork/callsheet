package reale2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// Coordinator setup and headless launch (design 12a-real-e2e, amendment
// A3): the supervisor renders the coordinator's system prompt, its two
// phase prompts and the session-local MCP configuration in the new
// <runtime>/coord directory and launches Claude Code headless itself, in
// two print-mode phases of one session: phase 1 runs the seed push, the
// designer and the design reviewer and ends with a pause result; after the
// owner's yes, phase 2 resumes the same session (--resume with the
// session ID phase 1 reported) for the coder and the code reviewer. The
// owner's configuration stays active: the only additional settings input
// is the inline, session-only {"sandbox":{"enabled":false}}; no permission
// bypass, session pinning, settings-source filter or MCP isolation. The
// real argv is recorded privately with a sanitized rendering.

// SandboxOffSettings is the only additional settings input.
const SandboxOffSettings = `{"sandbox":{"enabled":false}}`

// Phase prompt template names.
const (
	PhasePrompt1 = "coordinator-phase-1.md"
	PhasePrompt2 = "coordinator-phase-2.md"
)

// prepareSeed creates the seed, runs its baseline tests and creates the
// workspace the coordinator pushes it into.
func (s *supervisor) prepareSeed() error {
	gomod, err := readBounded(filepath.Join(s.checkout, "go.mod"), 64<<10)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	ver, err := goDirective(gomod)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	seed, err := createSeed(filepath.Join(s.runtime, "seed"), seedFiles(ver))
	if err != nil {
		return failure("seed_failed", err)
	}
	s.seed = seed
	if err := copyObjects(seed.repo.Storer, s.collect.Storer, []plumbing.Hash{seed.Commit}); err != nil {
		return failure("seed_failed", err)
	}
	s.ev.event(Event{Type: EvSeedCreated})
	run, out, errOut, err := runGoTests(s.command, s.goBin, s.pathEnv, filepath.Join(s.runtime, "baseline"), seed.Files,
		seed.Commit.String(), seed.Tree.String())
	s.baseline = &run
	s.ev.writeJSON("validation/baseline.json", run)
	s.ev.writeFile("validation/baseline-stdout.txt", []byte(toValidUTF8(out)))
	s.ev.writeFile("validation/baseline-stderr.txt", []byte(toValidUTF8(errOut)))
	s.ev.event(Event{Type: EvBaselineTests})
	if err != nil || run.ExitCode != 0 || run.TimedOut {
		return failure("baseline_tests_failed", fmt.Errorf("the seed's baseline tests did not pass (exit %d): %v", run.ExitCode, err))
	}
	suffix, err := s.randomHex(4)
	if err != nil {
		return failure("internal_error", err)
	}
	ctx, cancel := s.opCtx()
	defer cancel()
	ws, err := s.plane.CreateWorkspace(ctx, "e2e-"+suffix)
	if err != nil {
		return failure("workspace_create_failed", err)
	}
	s.workspace = WorkspaceRecord{Name: ws.Name, Instance: ws.Instance, SeedCommit: seed.Commit.String(), SeedTree: seed.Tree.String()}
	s.manifest.Workspace = s.workspace
	return nil
}

// mcpConfig is the session-local MCP configuration.
func mcpConfig(callsheet, url, ca string) ([]byte, error) {
	cfg := map[string]any{"mcpServers": map[string]any{mcpServerName: map[string]any{
		"type": "stdio", "command": callsheet, "args": []string{"mcp", "--plane", url, "--ca", ca, "--wait-call-budget", "10s"}}}}
	return encodeJSON(cfg)
}

// shellQuote quotes one argument for a rendered shell command.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// permissionValues are the fixed inputs of the coordinator's session-only
// allow-list (A3.6, A3.7): the reale2e executable and the runtime
// directory. The Callsheet executable, plane URL and CA reach the wait
// through the run file, never a Bash line.
type permissionValues struct{ reale2e, runtime string }

// symbolicPermissions renders the allow-list template printed before any
// vendor executes.
var symbolicPermissions = permissionValues{reale2e: "R", runtime: "D"}

// AllowListSize is the number of session-only rules (A3.7: six MCP tools,
// four observe, four wait-task and one review-material rule).
const AllowListSize = 15

// waitRuleOffset is the allow-list position of the first wait-task rule.
const waitRuleOffset = 10

// coordinatorMCPTools are the only Callsheet MCP tools the prompt uses, in
// allow-list order.
var coordinatorMCPTools = []string{"ws_push", "dispatch", "task_show", "ws_status", "ws_diff", "ws_pull"}

// ruleMetacharacters may not occur in a fixed allow-rule input: escaping
// them could silently broaden a rule.
const ruleMetacharacters = "*?[](),\r\n\x00"

// errRuleInput is a fixed allow-rule input holding a rule metacharacter.
var errRuleInput = errors.New("a fixed allow-rule input contains a permission-rule metacharacter (* ? [ ] ( ) , CR, LF or NUL)")

// errRuleQuoted is a fixed allow-rule input that only a shell-quoted
// word could hold: no coordinator Bash line may contain a quote (A3.7).
var errRuleQuoted = errors.New("a fixed allow-rule input needs shell quoting, and no coordinator Bash line may contain a quote")

// validRuleInputs reports whether every fixed input is a plain shell word
// without a rule metacharacter.
func validRuleInputs(vals ...string) error {
	for _, v := range vals {
		if strings.ContainsAny(v, ruleMetacharacters) {
			return errRuleInput
		}
		if shellQuote(v) != v {
			return errRuleQuoted
		}
	}
	return nil
}

// allowedTools is the single permission renderer shared by the launch and
// the checker: the 15 session-only rules, in their fixed order. Only the
// task-ID slots hold wildcard text.
func allowedTools(v permissionValues) ([]string, error) {
	if err := validRuleInputs(v.reale2e, v.runtime); err != nil {
		return nil, err
	}
	rules := make([]string, 0, AllowListSize)
	for _, t := range coordinatorMCPTools {
		rules = append(rules, "mcp__"+mcpServerName+"__"+t)
	}
	for _, h := range Hops {
		rules = append(rules, "Bash("+v.reale2e+" observe --run "+v.runtime+" --task * --hop "+h+")")
	}
	for i := range Hops {
		rules = append(rules, "Bash("+strings.Replace(waitCommand(v, i), "TASK_ID", "*", 1)+")")
	}
	return append(rules, "Bash("+v.reale2e+" review-material --run "+v.runtime+")"), nil
}

// waitCommand is hop i's background wait command (A3.7): the one simple
// wait-task helper command, which runs the CLI wait itself. TASK_ID is the
// coordinator's substitution.
func waitCommand(v permissionValues, i int) string {
	return v.reale2e + " wait-task --run " + v.runtime + " --task TASK_ID --hop " + Hops[i]
}

// shellOperators may not occur in a coordinator Bash line (A3.7): a
// permission rule cannot match a redirect, a separator, a pipeline, a
// quote, an expansion or a continuation.
const shellOperators = ";&|<>'\"`$\\\r\n\x00"

// simpleBashLine reports whether line is one simple command a permission
// rule can match: words separated by single spaces, none holding a shell
// operator.
func simpleBashLine(line string) bool {
	return line != "" && !strings.ContainsAny(line, shellOperators) && !strings.Contains(line, "  ") && strings.TrimSpace(line) == line
}

// bashRuleAllows reports whether the Bash rule allows command: the command
// must be a simple Bash line, and each * of the rule matches one nonempty
// word. A compound, redirected or quoted command is never allowed.
func bashRuleAllows(rule, command string) bool {
	body, ok := strings.CutPrefix(rule, "Bash(")
	if body, ok = strings.CutSuffix(body, ")"); !ok || !simpleBashLine(command) || !simpleBashLine(body) {
		return false
	}
	want, got := strings.Split(body, " "), strings.Split(command, " ")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if w != "*" && w != got[i] {
			return false
		}
	}
	return true
}

// promptBashLines are the Bash command lines of a rendered coordinator
// prompt: each line, after an optional "Observe: " label, that starts with
// the reale2e executable.
func promptBashLines(system, reale2e string) []string {
	var out []string
	for _, l := range strings.Split(system, "\n") {
		l = strings.TrimPrefix(l, "Observe: ")
		if strings.HasPrefix(l, reale2e+" ") {
			out = append(out, l)
		}
	}
	return out
}

// permissions are this run's fixed allow-list inputs.
func (s *supervisor) permissions() permissionValues {
	return permissionValues{reale2e: s.w.Executable, runtime: s.runtime}
}

// waitTemplate is hop i's background wait command for this run.
func (s *supervisor) waitTemplate(i int) string {
	return waitCommand(s.permissions(), i)
}

// printPermissionTemplate prints the session prerequisites before any
// vendor executes, with symbolic run values, after rejecting an
// executable a rule could not hold.
func (s *supervisor) printPermissionTemplate() error {
	if err := validRuleInputs(s.w.Executable); err != nil {
		return failure("permission_rule_invalid", err)
	}
	rules, _ := allowedTools(symbolicPermissions)
	s.say("the coordinator session uses session-only flags; no agent configuration or settings file is written")
	s.say("  --settings %s", SandboxOffSettings)
	s.say("  --allowed-tools, exactly these %d rules (R reale2e, D runtime directory):", len(rules))
	for _, r := range rules {
		s.say("    %s", r)
	}
	return nil
}

// renderPermissions renders and prints this run's allow-list once the
// runtime and plane exist, before any vendor runs a task.
func (s *supervisor) renderPermissions() error {
	rules, err := allowedTools(s.permissions())
	if err != nil {
		return failure("permission_rule_invalid", err)
	}
	s.allowed = rules
	s.say("coordinator allow-list for this run (both phases):")
	for _, r := range rules {
		s.say("    %s", r)
	}
	return nil
}

// prepareCoordinator renders the coordinator's system prompt, phase-1
// prompt and MCP configuration, records them and starts the control
// socket.
func (s *supervisor) prepareCoordinator() error {
	rt := s.runtime
	vals := map[string]string{"RUN_ID": s.runID, "RUNTIME": rt, "PLANE_URL": s.dep.url, "CA_PATH": s.dep.caPath, "REALE2E": s.w.Executable,
		"WORKSPACE": s.workspace.Name, "INSTANCE": s.workspace.Instance, "SEED_PATH": s.seed.Path, "SEED_COMMIT": s.workspace.SeedCommit,
		"RECEIPT": filepath.Join(rt, "owner-receipt.json"), "COORD_DIR": filepath.Join(rt, "coord")}
	for i := range Hops {
		vals["WAIT_"+hopDir(i)] = s.waitTemplate(i)
	}
	system, err := renderTemplate(CoordinatorPrompt, s.templates[CoordinatorPrompt], vals)
	if err != nil {
		return failure("templates_invalid", err)
	}
	phase1, err := renderTemplate(PhasePrompt1, s.templates[PhasePrompt1], vals)
	if err != nil {
		return failure("templates_invalid", err)
	}
	s.rendered[CoordinatorPrompt], s.rendered[PhasePrompt1] = system, phase1
	cfg, err := mcpConfig(s.args.callsheet, s.dep.url, s.dep.caPath)
	if err != nil {
		return failure("internal_error", err)
	}
	s.mcpPath = filepath.Join(rt, "mcp.json")
	if err := os.WriteFile(s.mcpPath, cfg, 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	sanitized, err := mcpConfig(s.tok(s.args.callsheet), s.dep.url, s.tok(s.dep.caPath))
	if err != nil {
		return failure("internal_error", err)
	}
	setup := map[string][]byte{"setup/coordinator-prompt.md": system, "setup/" + PhasePrompt1: phase1, "setup/flow.json": s.flowBytes,
		"setup/mcp-config.json": sanitized}
	for _, h := range Hops {
		for _, k := range []string{"instruction", "runbook"} {
			setup["setup/manuals/"+manualName(h, k)] = s.rendered[manualName(h, k)]
		}
	}
	for _, name := range sortedKeys(setup) {
		if err := s.ev.writeFile(name, setup[name]); err != nil {
			return failure("evidence_write_failed", err)
		}
		s.manifest.Rendered = append(s.manifest.Rendered, FileHash{Name: name, SHA256: sha256Hex(setup[name])})
	}
	s.manifest.Templates = templateHashes(s.templates)
	b, _ := encodeJSON(RunFile{Schema: RunFileSchema, RunID: s.runID, Callsheet: s.args.callsheet, Plane: s.dep.url, CA: s.dep.caPath})
	if err := os.WriteFile(filepath.Join(rt, RunFileName), b, 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	if s.ctl, err = listenControl(rt, s.runID, s.handleControl); err != nil {
		return failure("control_socket_failed", err)
	}
	s.ev.event(Event{Type: EvCoordinatorReady})
	return nil
}

// launchArgv is phase p's exact argument vector (after the executable),
// shared by the launch and the checker: the sandbox-off settings, then
// --allowed-tools with its rules as separate elements, terminated by
// --mcp-config.
func launchArgv(p int, prompt string, allowed []string, mcpPath, system, session string) []string {
	argv := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--settings", SandboxOffSettings, "--allowed-tools"}
	argv = append(argv, allowed...)
	argv = append(argv, "--mcp-config", mcpPath, "--append-system-prompt", system)
	if p == 2 {
		argv = append(argv, "--resume", session)
	}
	return argv
}

// phaseArgv is phase p's exact argument vector (after the executable).
func (s *supervisor) phaseArgv(p int, prompt []byte) []string {
	return launchArgv(p, string(prompt), s.allowed, s.mcpPath, string(s.rendered[CoordinatorPrompt]), s.sessionID)
}

// launchPhase starts coordinator phase p (1 or 2) headless, with the
// owner's inherited environment, in the coordinator directory, its stdin
// closed and its streams captured within their bounds.
func (s *supervisor) launchPhase(p int, prompt []byte) error {
	coord := filepath.Join(s.runtime, "coord")
	argv := s.phaseArgv(p, prompt)
	pr := &phaseRun{phase: p, stdout: newLimitBuffer(maxTextFile), stderr: newLimitBuffer(maxStderr)}
	spec := ProcSpec{Path: s.args.claude, Args: argv, Env: s.w.Environ, Dir: coord, Stdout: pr.stdout, Stderr: pr.stderr}
	proc, err := s.w.Launcher.Start(spec)
	if err != nil {
		return failure("coordinator_start_failed", err)
	}
	pr.proc = proc
	s.phases[p-1] = pr
	pr.startedSeq = s.track(&child{name: phaseHandle(p), spec: spec, proc: proc, logs: newLineLog()}, EvPhaseStarted)
	rec := LaunchRecord{Schema: LaunchSchema, Phase: p, Executable: s.args.claude, Argv: argv, Cwd: coord, PromptSHA256: sha256Hex(prompt),
		SystemSHA256: sha256Hex(s.rendered[CoordinatorPrompt]), StartedSeq: pr.startedSeq, Display: []string{s.tok(s.args.claude)}, WaitTemplates: []string{}}
	for i := range Hops {
		rec.WaitTemplates = append(rec.WaitTemplates, s.waitTemplate(i))
	}
	if p == 2 {
		rec.ResumeSession = s.sessionID
	}
	for i, a := range argv {
		switch {
		case i > 0 && argv[i-1] == "-p":
			a = "<phase-" + fmt.Sprint(p) + " prompt sha256=" + rec.PromptSHA256 + ">"
		case i > 0 && argv[i-1] == "--append-system-prompt":
			a = "<coordinator instructions sha256=" + rec.SystemSHA256 + ">"
		default:
			a = s.tok(a)
		}
		rec.Display = append(rec.Display, a)
	}
	s.launches[p-1] = &rec
	if err := s.ev.writeJSON(launchFiles[p-1], rec); err != nil {
		return failure("evidence_write_failed", err)
	}
	s.say("coordinator phase %d started (pid %d)", p, proc.PID())
	return nil
}

// maxStderr bounds a phase's retained stderr.
const maxStderr = 1 << 20

// phaseRun is one coordinator phase's live state.
type phaseRun struct {
	phase      int
	proc       Proc
	stdout     *limitBuffer
	stderr     *limitBuffer
	startedSeq int
	handled    bool
	stopped    bool
	rec        PhaseProcess
}

// maxProjectDirs bounds the transcript search's directory entries.
const maxProjectDirs = 10000

// errTranscript is a transcript that cannot be located unambiguously.
var errTranscript = errors.New("the coordinator transcript was not found exactly once")

// findTranscript locates <session-id>.jsonl read-only below the protected
// Claude home's projects directory: one regular, non-symlinked file in a
// real project directory, never guessed from the encoded directory name.
// Only the vendor home itself is resolved (A3.4); no component below it
// may be a link, so the transcript is contained in the protected root the
// baseline scanner walked. It returns the file and its slash path relative
// to that root (the baseline record's path).
func findTranscript(home, sid string) (string, string, error) {
	root, err := filepath.EvalSymlinks(filepath.Join(home, ".claude"))
	if err != nil {
		return "", "", err
	}
	projects := filepath.Join(root, "projects")
	if st, err := os.Lstat(projects); err != nil {
		return "", "", err
	} else if !st.IsDir() {
		return "", "", errTranscript
	}
	ents, err := readDirBounded(openDir, projects, maxProjectDirs, nil)
	if errors.Is(err, errDirBudget) {
		return "", "", errTranscript
	}
	if err != nil {
		return "", "", err
	}
	var found []string
	for _, e := range ents {
		if !e.Type().IsDir() {
			continue
		}
		p := filepath.Join(projects, e.Name(), sid+".jsonl")
		st, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !st.Mode().IsRegular() {
			return "", "", errTranscript
		}
		found = append(found, e.Name())
	}
	if len(found) != 1 {
		return "", "", errTranscript
	}
	rel := "projects/" + found[0] + "/" + sid + ".jsonl"
	return filepath.Join(root, filepath.FromSlash(rel)), rel, nil
}

// streamSession returns the session ID of a phase stream's init record.
func streamSession(stream []byte) string {
	d := &deriver{codes: map[string]bool{}}
	for _, r := range d.decodeLines(phaseStreamFiles[0], stream) {
		if r.kind == "init" {
			return r.sessionID
		}
	}
	return ""
}
