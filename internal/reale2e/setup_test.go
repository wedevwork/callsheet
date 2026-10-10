package reale2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRunbookSetup (FP-3): the shipped inputs render into a clean session
// setup (prompt with concrete values only, session-local MCP config, a
// fresh session launch with no resume, bare or bypass flag), every source
// template and rendered file is hashed, the bundle's provenance verifies,
// and an omitted or extra setup input fails.
func TestRunbookSetup(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	if code := f.attempt(); code != 0 {
		t.Fatalf("attempt = %d %s", code, f.stderr.String())
	}
	s, b := f.sup, f.bundle()
	prompt, _ := os.ReadFile(filepath.Join(b, "setup", "coordinator-prompt.md"))
	if placeholderRE.Match(prompt) {
		t.Fatal("the rendered prompt keeps a placeholder")
	}
	for _, want := range []string{s.runID, s.dep.url, s.dep.caPath, fakeCallsheet, fakeReale2e, s.workspace.Name, s.workspace.Instance,
		s.workspace.SeedCommit, s.seed.Path, filepath.Join(s.runtime, "waits"), filepath.Join(s.runtime, "owner-receipt.json"),
		"ws_push", "dispatch", "task_show", "ws_status", "ws_diff", "ws_pull", "run_in_background: true", "--until-done"} {
		if !bytes.Contains(prompt, []byte(want)) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	var m Manifest
	readDoc(t, filepath.Join(b, "manifest.json"), &m)
	if len(m.Templates) != len(TemplateNames()) {
		t.Fatalf("templates %v", m.Templates)
	}
	for _, th := range m.Templates {
		src, err := os.ReadFile(filepath.Join(f.checkout, filepath.FromSlash(ExamplesDir), th.Name))
		if err != nil || sha256Hex(src) != th.SHA256 {
			t.Fatalf("template %s hash %s: %v", th.Name, th.SHA256, err)
		}
	}
	var names []string
	for _, r := range m.Rendered {
		names = append(names, r.Name)
	}
	if len(names) != 3+2*len(Hops) || !slices.Contains(names, "setup/coordinator-prompt.md") || !slices.Contains(names, "setup/mcp-config.json") {
		t.Fatalf("rendered %v", names)
	}
	var l LaunchRecord
	readDoc(t, filepath.Join(b, "setup", "launch.json"), &l)
	want := []string{fakeClaude, "--session-id", s.sessionID, "--mcp-config", "$RUN/mcp.json", "--strict-mcp-config", "--append-system-prompt",
		"$(cat $RUN/coordinator-prompt.md)"}
	if !slices.Equal(l.Argv, want) || l.SessionID != s.sessionID || !sessionIDRE.MatchString(l.SessionID) || l.CoordinatorDir != "$RUN/coord" {
		t.Fatalf("launch %+v", l)
	}
	var cfg map[string]map[string]struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	live, _ := os.ReadFile(filepath.Join(s.runtimeOrKept(t), "mcp.json"))
	if err := json.Unmarshal(live, &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg["mcpServers"]["callsheet"]
	if srv.Command != fakeCallsheet || !slices.Equal(srv.Args, []string{"mcp", "--plane", s.dep.url, "--ca", s.dep.caPath, "--wait-call-budget", "10s"}) {
		t.Fatalf("mcp config %+v", srv)
	}
	sanitized, _ := os.ReadFile(filepath.Join(b, "setup", "mcp-config.json"))
	if bytes.Contains(sanitized, []byte(s.runtime)) || !bytes.Contains(sanitized, []byte("$RUN/p/pki/ca.crt")) {
		t.Fatalf("sanitized config %s", sanitized)
	}
	if out := f.stdout.String(); !strings.Contains(out, "--session-id "+s.sessionID) || !strings.Contains(out, "never resume") {
		t.Fatal("no launch instructions")
	}
	// Provenance verifies; omitted or extra setup fails.
	if code, _, _ := checkBundle(t, passingBundle(t)); code != 0 {
		t.Fatal("the passing bundle failed")
	}
	for name, mutate := range map[string]func(b string){
		"omitted attestation": func(b string) { removeBundleFile(t, b, "session/setup-attestation.json") },
		"extra input": func(b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.SetupInputs = append(a.SetupInputs, "project-claude-md") })
		},
		"omitted transcript": func(b string) { removeBundleFile(t, b, "session/transcript.jsonl") },
		"resumed": func(b string) {
			editDoc(t, b, "setup/launch.json", func(l *LaunchRecord) { l.Argv = append(l.Argv, "--continue") })
		},
	} {
		b := passingBundle(t)
		mutate(b)
		if code, r, _ := checkBundle(t, b); code != 1 || criterion(r, CritSetup).Result != "fail" {
			t.Errorf("%s = %d %v", name, code, failedCodes(r))
		}
	}
}

// runtimeOrKept returns a runtime directory still holding the live files:
// the attempt removed it, so the test re-renders into a fresh one.
func (s *supervisor) runtimeOrKept(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(s.runtime); err == nil {
		return s.runtime
	}
	dir := t.TempDir()
	cfg, err := mcpConfig(s.args.callsheet, s.dep.url, s.dep.caPath)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "mcp.json"), cfg, 0o600)
	return dir
}

// TestSetupUnits (UT-3): rendering is deterministic and refuses unknown or
// unset placeholders; hashes follow the template order; resumed sessions,
// missing provenance, missing background notifications, invalid spans and
// unqualified long polls fail.
func TestSetupUnits(t *testing.T) {
	t.Parallel()
	vals := map[string]string{"RUN_ID": "r1", "PLANE_URL": "https://127.0.0.1:1"}
	a, err := renderTemplate("x", []byte("run {{RUN_ID}} at {{PLANE_URL}} {{RUN_ID}}"), vals)
	b, _ := renderTemplate("x", []byte("run {{RUN_ID}} at {{PLANE_URL}} {{RUN_ID}}"), vals)
	if err != nil || string(a) != "run r1 at https://127.0.0.1:1 r1" || !bytes.Equal(a, b) {
		t.Fatalf("render = %q %v", a, err)
	}
	for _, tpl := range []string{"{{SECRET}}", "{{CA_PATH}}", "{{}}"} {
		if _, err := renderTemplate("x", []byte(tpl), vals); err == nil {
			t.Errorf("%s rendered", tpl)
		}
	}
	tpls := map[string][]byte{}
	for _, n := range TemplateNames() {
		tpls[n] = []byte(n)
	}
	hs := templateHashes(tpls)
	if len(hs) != len(TemplateNames()) || hs[0].Name != FlowFile || hs[0].SHA256 != sha256Hex([]byte(FlowFile)) {
		t.Fatalf("hashes %v", hs)
	}
	if _, err := loadTemplates(t.TempDir()); err == nil {
		t.Fatal("missing templates loaded")
	}
	if q := shellQuote("a b'c"); q != `'a b'\''c'` || shellQuote("/x/y") != "/x/y" || shellQuote("") != "''" {
		t.Fatalf("quote %q", q)
	}
	if id, err := newSessionID(&countingRand{}); err != nil || !sessionIDRE.MatchString(id) {
		t.Fatalf("session ID %q %v", id, err)
	}
	if _, err := newSessionID(failingReader{}); err == nil {
		t.Fatal("no entropy, still a session")
	}
	for name, m := range map[string]struct {
		crit, code string
		f          func(b string)
	}{
		"resumed": {CritSetup, "session_resumed", func(b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.Resumed = true })
		}},
		"missing launch": {CritSetup, "launch_missing", func(b string) { removeBundleFile(t, b, "setup/launch.json") }},
		"missing wake": {CritEventMap, "event_map_incomplete", func(b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events = slices.Delete(m.Events, 4, 5) })
		}},
		"invalid span": {CritEventMap, "transcript_span_invalid", func(b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events[0].Lines = []int{0, 1} })
		}},
		"unqualified poll": {CritWaits, "unqualified_long_poll", func(b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events = append(m.Events, MapEvent{Kind: MapMCPWait, Lines: []int{1, 1}}) })
		}},
		"unknown run": {CritEventMap, "event_map_binding_mismatch", func(b string) { editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.RunID = "x" }) }},
		"session mismatch": {CritSetup, "attestation_binding_mismatch", func(b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.SessionID = "x" })
		}},
	} {
		b := passingBundle(t)
		m.f(b)
		code, r, _ := checkBundle(t, b)
		requireCode(t, name, code, r, m.crit, m.code)
	}
}
