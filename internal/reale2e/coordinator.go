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

// waitTemplate is hop i's background wait command: the plain CLI wait
// with stdout and stderr redirected to the hop's files, its status saved
// to a third file and returned. TASK_ID is the coordinator's substitution.
func (s *supervisor) waitTemplate(i int) string {
	dir := filepath.Join(s.runtime, "waits", hopDir(i))
	q := shellQuote
	return q(s.args.callsheet) + " task wait TASK_ID --until-done --json --plane " + q(s.dep.url) + " --ca " + q(s.dep.caPath) +
		" >" + q(filepath.Join(dir, "result.json")) + " 2>" + q(filepath.Join(dir, "error.txt")) + "; rc=$?; echo $rc >" +
		q(filepath.Join(dir, "exit-code")) + "; exit $rc"
}

// prepareCoordinator renders the coordinator's system prompt, phase-1
// prompt and MCP configuration, records them and starts the control
// socket.
func (s *supervisor) prepareCoordinator() error {
	rt := s.runtime
	q := shellQuote
	vals := map[string]string{"RUN_ID": s.runID, "RUNTIME": q(rt), "PLANE_URL": s.dep.url, "CA_PATH": s.dep.caPath, "CALLSHEET": s.args.callsheet,
		"REALE2E": q(s.w.Executable), "WORKSPACE": s.workspace.Name, "INSTANCE": s.workspace.Instance, "SEED_PATH": s.seed.Path,
		"SEED_COMMIT": s.workspace.SeedCommit, "WAITS_DIR": q(filepath.Join(rt, "waits")), "RECEIPT": filepath.Join(rt, "owner-receipt.json"),
		"COORD_DIR": filepath.Join(rt, "coord")}
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
	b, _ := encodeJSON(RunFile{Schema: RunFileSchema, RunID: s.runID})
	if err := os.WriteFile(filepath.Join(rt, RunFileName), b, 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	if s.ctl, err = listenControl(rt, s.runID, s.handleControl); err != nil {
		return failure("control_socket_failed", err)
	}
	s.ev.event(Event{Type: EvCoordinatorReady})
	return nil
}

// phaseArgv is phase p's exact argument vector (after the executable).
func (s *supervisor) phaseArgv(p int, prompt []byte) []string {
	argv := []string{"-p", string(prompt), "--output-format", "stream-json", "--verbose", "--settings", SandboxOffSettings,
		"--mcp-config", s.mcpPath, "--append-system-prompt", string(s.rendered[CoordinatorPrompt])}
	if p == 2 {
		argv = append(argv, "--resume", s.sessionID)
	}
	return argv
}

// launchPhase starts coordinator phase p (1 or 2) headless, with the
// owner's inherited environment, in the coordinator directory, its stdin
// closed and its streams captured within their bounds.
func (s *supervisor) launchPhase(p int, prompt []byte) error {
	coord := filepath.Join(s.runtime, "coord")
	argv := s.phaseArgv(p, prompt)
	pr := &phaseRun{phase: p, stdout: newLimitBuffer(maxTextFile), stderr: newLimitBuffer(maxStderr)}
	spec := ProcSpec{Path: s.args.claude, Args: argv, Env: s.w.Environ, Dir: coord, Stdout: pr.stdout, Stderr: pr.stderr}
	ev := s.ev.event(Event{Type: EvPhaseStarted, Handle: phaseHandle(p)})
	pr.startedSeq = ev.Seq
	proc, err := s.w.Launcher.Start(spec)
	if err != nil {
		return failure("coordinator_start_failed", err)
	}
	pr.proc = proc
	s.phases[p-1] = pr
	s.track(&child{name: phaseHandle(p), spec: spec, proc: proc, logs: newLineLog()})
	rec := LaunchRecord{Schema: LaunchSchema, Phase: p, Executable: s.args.claude, Argv: argv, Cwd: coord, PromptSHA256: sha256Hex(prompt),
		SystemSHA256: sha256Hex(s.rendered[CoordinatorPrompt]), StartedSeq: ev.Seq, Display: []string{s.tok(s.args.claude)}, WaitTemplates: []string{}}
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
