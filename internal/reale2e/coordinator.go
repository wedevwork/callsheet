package reale2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// Coordinator setup (design 12a-real-e2e, Coordinator setup and
// provenance): the seed repository and its baseline tests, a fresh
// workspace pinned to one instance, the session-local MCP configuration,
// the rendered coordinator prompt and the exact launch command with a
// fresh session ID. The coordinator then works only from those; the
// supervisor never dispatches a feature hop.

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
	cfg := map[string]any{"mcpServers": map[string]any{"callsheet": map[string]any{
		"type": "stdio", "command": callsheet, "args": []string{"mcp", "--plane", url, "--ca", ca, "--wait-call-budget", "10s"}}}}
	return encodeJSON(cfg)
}

// shellQuote quotes one argument for the printed launch command.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// prepareCoordinator renders the coordinator's setup, records it, starts
// the control socket and prints the launch instructions.
func (s *supervisor) prepareCoordinator() error {
	sid, err := newSessionID(s.w.Rand)
	if err != nil {
		return failure("internal_error", err)
	}
	s.sessionID = sid
	rt := s.runtime
	vals := map[string]string{"RUN_ID": s.runID, "RUNTIME": rt, "PLANE_URL": s.dep.url, "CA_PATH": s.dep.caPath, "CALLSHEET": s.args.callsheet,
		"REALE2E": s.w.Executable, "WORKSPACE": s.workspace.Name, "INSTANCE": s.workspace.Instance, "SEED_PATH": s.seed.Path,
		"SEED_COMMIT": s.workspace.SeedCommit, "WAITS_DIR": filepath.Join(rt, "waits"), "RECEIPT": filepath.Join(rt, "owner-receipt.json"),
		"OWNER_DIR": filepath.Join(rt, "owner"), "COORD_DIR": filepath.Join(rt, "coord")}
	prompt, err := renderTemplate(CoordinatorPrompt, s.templates[CoordinatorPrompt], vals)
	if err != nil {
		return failure("templates_invalid", err)
	}
	s.rendered[CoordinatorPrompt] = prompt
	cfg, err := mcpConfig(s.args.callsheet, s.dep.url, s.dep.caPath)
	if err != nil {
		return failure("internal_error", err)
	}
	promptPath, cfgPath := filepath.Join(rt, CoordinatorPrompt), filepath.Join(rt, "mcp.json")
	for _, f := range []struct {
		p string
		b []byte
	}{{promptPath, prompt}, {cfgPath, cfg}} {
		if err := os.WriteFile(f.p, f.b, 0o600); err != nil {
			return failure("runtime_unavailable", err)
		}
	}
	argv := []string{s.args.claude, "--session-id", sid, "--mcp-config", cfgPath, "--strict-mcp-config", "--append-system-prompt",
		"$(cat " + promptPath + ")"}
	launch := LaunchRecord{Schema: LaunchSchema, SessionID: sid, CoordinatorDir: s.tok(filepath.Join(rt, "coord"))}
	for _, a := range argv {
		launch.Argv = append(launch.Argv, s.tok(a))
	}
	sanitized, err := mcpConfig(s.tok(s.args.callsheet), s.dep.url, s.tok(s.dep.caPath))
	if err != nil {
		return failure("internal_error", err)
	}
	setup := map[string][]byte{"setup/coordinator-prompt.md": prompt, "setup/flow.json": s.flowBytes, "setup/mcp-config.json": sanitized}
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
	if err := s.ev.writeJSON("setup/launch.json", launch); err != nil {
		return failure("evidence_write_failed", err)
	}
	b, _ := json.Marshal(RunFile{Schema: RunFileSchema, RunID: s.runID})
	if err := os.WriteFile(filepath.Join(rt, RunFileName), append(b, '\n'), 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	if s.ctl, err = listenControl(rt, s.runID, s.handleControl); err != nil {
		return failure("control_socket_failed", err)
	}
	s.ev.event(Event{Type: EvCoordinatorReady})
	// From here the owner may start the coordinator: its exit evidence is
	// required before cleanup is complete.
	s.coordinatorReady = true
	quoted := make([]string, len(argv)-1)
	for i, a := range argv[:len(argv)-1] {
		quoted[i] = shellQuote(a)
	}
	s.say("ready for the coordinator. In a new terminal, start a fresh Claude Code session (never resume):")
	s.say("  cd %s && %s \"$(cat %s)\"", shellQuote(filepath.Join(rt, "coord")), strings.Join(quoted, " "), shellQuote(promptPath))
	s.say("seed %s at %s; workspace %s instance %s", s.seed.Path, s.workspace.SeedCommit, s.workspace.Name, s.workspace.Instance)
	s.say("this terminal takes the owner decision; finish with: %s finish --run %s", s.w.Executable, rt)
	return nil
}
