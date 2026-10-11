package reale2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestRunbookSetup (FP-3): the shipped inputs render into a clean session
// setup (prompt with concrete values only, session-local MCP config, a
// fresh session launch with no resume, bare or bypass flag), every source
// template and rendered file is hashed, the bundle's provenance verifies,
// and an omitted or extra setup input fails.
func TestRunbookSetup(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	var runtimeSettings string
	var runtimeSettingsMode os.FileMode
	f.coord.before = func(c *coordScript) bool {
		if runtimeSettings == "" {
			p := filepath.Join(c.s.runtime, SettingsFile)
			runtimeSettings = string(mustRead(t, p))
			if st, err := os.Stat(p); err == nil {
				runtimeSettingsMode = st.Mode().Perm()
			}
		}
		return false
	}
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
		"ws_push", "dispatch", "task_show", "ws_status", "ws_diff", "ws_pull", "run_in_background: true"} {
		if !bytes.Contains(prompt, []byte(want)) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	// A2: all four background waits are the relay's, rendered with the
	// concrete helper, runtime and wait paths; no direct plane wait is left.
	waits := filepath.Join(s.runtime, "waits")
	for i, h := range Hops {
		line := fakeReale2e + " wait --run " + s.runtime + " --task TASK_ID --hop " + h + " >" + waits + "/" + hopDir(i) + "/result.json 2>" + waits + "/" +
			hopDir(i) + "/error.txt; echo $? >" + waits + "/" + hopDir(i) + "/exit-code\n"
		if bytes.Count(prompt, []byte(line)) != 1 {
			t.Errorf("the prompt lacks hop %s's wait line %q", h, line)
		}
	}
	if bytes.Contains(prompt, []byte("task wait")) || bytes.Contains(prompt, []byte("--until-done")) || bytes.Count(prompt, []byte(" wait --run ")) != 4 {
		t.Error("the prompt keeps a direct plane wait")
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
	if len(names) != 4+2*len(Hops) || !slices.Contains(names, "setup/coordinator-prompt.md") || !slices.Contains(names, "setup/mcp-config.json") ||
		!slices.Contains(names, SettingsEvidence) {
		t.Fatalf("rendered %v", names)
	}
	// A2: the settings snapshot is the supplied file's exact bytes, in the
	// evidence and (mode 0600) in the runtime the coordinator is launched
	// from; the manifest holds its hash, never its content.
	snap, _ := os.ReadFile(filepath.Join(b, filepath.FromSlash(SettingsEvidence)))
	if string(snap) != testSettings || bytes.Contains(mustRead(t, filepath.Join(b, "manifest.json")), []byte("allowAllUnixSockets")) {
		t.Fatalf("settings snapshot %q", snap)
	}
	for _, r := range m.Rendered {
		if r.Name == SettingsEvidence && r.SHA256 != sha256Hex([]byte(testSettings)) {
			t.Fatalf("settings hash %s", r.SHA256)
		}
	}
	if runtimeSettings != testSettings || runtimeSettingsMode != 0o600 {
		t.Fatalf("runtime settings %q mode %v", runtimeSettings, runtimeSettingsMode)
	}
	var l LaunchRecord
	readDoc(t, filepath.Join(b, "setup", "launch.json"), &l)
	want := []string{fakeClaude, "--setting-sources", "project,local", "--settings", "$RUN/coordinator-settings.json", "--mcp-config", "$RUN/mcp.json",
		"--strict-mcp-config", "--session-id", s.sessionID, "--append-system-prompt", "$(cat $RUN/coordinator-prompt.md)"}
	if !slices.Equal(l.Argv, want) || l.SessionID != s.sessionID || !sessionIDRE.MatchString(l.SessionID) || l.CoordinatorDir != "$RUN/coord" {
		t.Fatalf("launch %+v", l)
	}
	// The printed command keeps the existing shell quoting convention.
	printed := fakeClaude + " --setting-sources 'project,local' --settings " + filepath.Join(s.runtime, SettingsFile) + " --mcp-config " +
		filepath.Join(s.runtime, "mcp.json") + " --strict-mcp-config --session-id " + s.sessionID + " --append-system-prompt \"$(cat "
	if !strings.Contains(f.stdout.String(), printed) {
		t.Fatalf("the printed launch lacks %q", printed)
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
	t.Run("A2 settings and launch", testA2Setup)
	t.Run("A2 runbook", testA2Runbook)
}

// The runbook's three owner-terminal snippets, byte for byte as design
// 12a-real-e2e amendment A2 gives them. Tests compare them with the
// runbook and never execute them (no python3 test prerequisite, no home
// directory writes).
const (
	settingsSnippet = `python3 - /ABS/coordinator-settings.json <<'PYSETTINGS'
import json, os, pathlib, sys
source = json.loads((pathlib.Path.home() / '.claude/settings.json').read_text())
value = source['sandbox']
assert isinstance(value, dict) and value.get('enabled') is True
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as f:
    json.dump({'sandbox': value}, f, indent=2)
    f.write('\n')
PYSETTINGS
`
	moveSnippet = `python3 - <<'PYMOVE'
import os, pathlib, stat
p = pathlib.Path.home() / '.claude/CLAUDE.md'
b = p.with_name('CLAUDE.md.reale2e-backup')
if os.path.lexists(b):
    raise SystemExit('refusing existing CLAUDE.md backup')
if os.path.lexists(p):
    if not stat.S_ISREG(p.lstat().st_mode):
        raise SystemExit('CLAUDE.md must be a regular file')
    os.link(p, b)
    p.unlink()
PYMOVE
`
	restoreSnippet = `python3 - <<'PYRESTORE'
import os, pathlib, stat
p = pathlib.Path.home() / '.claude/CLAUDE.md'
b = p.with_name('CLAUDE.md.reale2e-backup')
if os.path.lexists(b):
    if not stat.S_ISREG(b.lstat().st_mode):
        raise SystemExit('backup must be a regular file')
    os.link(b, p)
    b.unlink()
PYRESTORE
`
)

// testA2Setup (FP-3, amendment A2): invalid, missing or unsafe coordinator
// settings stop the run before any executable is probed or anything is
// deployed; a missing or relative --coordinator-settings is a usage
// error; a runtime copy changed or removed during the session fails
// setup; shell-sensitive helper paths are rendered quoted; and every new
// launch or snapshot predicate fails its recorded bundle.
func testA2Setup(t *testing.T) {
	for name, prep := range map[string]func(p string){
		"missing":   func(p string) { os.Remove(p) },
		"symlink":   func(p string) { os.Rename(p, p+".real"); os.Symlink(p+".real", p) },
		"directory": func(p string) { os.Remove(p); os.Mkdir(p, 0o700) },
		"disabled":  func(p string) { os.WriteFile(p, []byte(`{"sandbox":{"enabled":false}}`), 0o600) },
		"oversized": func(p string) {
			os.WriteFile(p, []byte(`{"sandbox":{"enabled":true,"x":"`+strings.Repeat("x", maxSettingsBytes)+`"}}`), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			prep(f.settings)
			if code := f.attempt(); code != 1 || !strings.Contains(f.stderr.String(), "coordinator_settings_invalid") {
				t.Fatalf("attempt = %d %s", code, f.stderr.String())
			}
			if started := f.launcher.argv(); len(started) != 0 || len(f.plane.roles) != 0 {
				t.Fatalf("invalid settings still started %v and registered %d roles", started, len(f.plane.roles))
			}
		})
	}
	for _, extra := range [][]string{nil, {"--coordinator-settings", "relative.json"}, {"--coordinator-settings", ""}} {
		f := newFakeWorld(t)
		h := f.host()
		h.Args = append(slices.Clone(h.Args[:len(h.Args)-2]), extra...)
		h.NewWorld = func(Host) (*World, func(), error) { t.Fatal("world built"); return nil, nil, nil }
		if code := MainWith(h); code != 2 || !strings.Contains(f.stderr.String(), "--coordinator-settings") {
			t.Errorf("%v = %d %q", extra, code, f.stderr.String())
		}
	}
	for name, tc := range map[string]struct {
		mutate  func(p string)
		failHop int
	}{
		"runtime copy changed": {func(p string) { os.WriteFile(p, []byte(`{"sandbox":{"enabled":true}}`+"\n"), 0o600) }, -1},
		"runtime copy removed": {func(p string) { os.Remove(p) }, -1},
		// Recorded for the setup criterion even after an earlier failure.
		"changed after a failed hop": {func(p string) { os.WriteFile(p, []byte(`{}`), 0o600) }, 0},
	} {
		mutate := tc.mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.coord.failHop = tc.failHop
			done := false
			f.coord.before = func(c *coordScript) bool {
				if !done {
					done = true
					mutate(filepath.Join(c.s.runtime, SettingsFile))
				}
				return false
			}
			if code := f.attempt(); code != 1 {
				t.Fatalf("attempt = %d", code)
			}
			if c := criterion(f.report(), CritSetup); c.Result != "fail" || !slices.Contains(c.Codes, codeSettingsModified) {
				t.Fatalf("setup %+v", c)
			}
		})
	}
	t.Run("quoted paths", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.executable = "/fake/bin/real e2e"
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %s", code, f.stderr.String())
		}
		prompt := mustRead(t, filepath.Join(f.bundle(), "setup", "coordinator-prompt.md"))
		if !bytes.Contains(prompt, []byte("'/fake/bin/real e2e' wait --run "+f.sup.runtime+" --task TASK_ID --hop designer >")) {
			t.Fatal("the helper path is not quoted in the wait line")
		}
	})
	for _, m := range a2SetupMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			b := passingBundle(t)
			m.mutate(t, b)
			code, r, _ := checkBundle(t, b)
			requireCode(t, m.name, code, r, m.crit, m.code)
		})
	}
}

// testA2Runbook (FP-3, amendment A2): the runbook carries the settings,
// move and restore snippets byte for byte, in the required order: settings
// before the supervisor starts with them, the move before the launch, the
// built-in checks before the start instruction, and the restore after the
// session closes, before the attestation and finish.
func testA2Runbook(t *testing.T) {
	t.Parallel()
	doc := string(mustRead(t, filepath.Join(testkit.MustRepoRoot(t), "docs", "real-e2e.md")))
	at := func(s string) int {
		t.Helper()
		if strings.Count(doc, s) != 1 {
			t.Fatalf("docs/real-e2e.md has %d copies of %q", strings.Count(doc, s), s)
		}
		return strings.Index(doc, s)
	}
	order := []int{
		at("```sh\n" + settingsSnippet + "```\n"),
		at("--coordinator-settings /ABS/coordinator-settings.json"),
		at("No other Claude Code session may run during the exercise"),
		at("```sh\n" + moveSnippet + "```\n"),
		at("--setting-sources project,local --settings <runtime>/coordinator-settings.json"),
		at("run the free built-in checks `/mcp`, `/context` and `/sandbox`"),
		at(`(for example "start the run")`),
		at("```sh\n" + restoreSnippet + "```\n"),
		at("`instruction_sources_checked` true attests"),
		at("Then run `reale2e finish --run <runtime>`"),
	}
	if !slices.IsSorted(order) {
		t.Fatalf("runbook steps out of order: %v", order)
	}
	for _, want := range []string{"`--safe-mode`", "`--disable-slash-commands`", "`--bare`", "reale2e wait --run <runtime> --task ID --hop NAME",
		"exits 1; usage errors exit 2", "before `reale2e finish`"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/real-e2e.md lacks %q", want)
		}
	}
	note := string(mustRead(t, filepath.Join(testkit.MustRepoRoot(t), "docs", "coordinator.md")))
	for _, want := range []string{"cannot reach a plane listening on the host's loopback address", "unsandboxed CLI execution path or a relay",
		"`reale2e wait`", "changes no support catalog entry"} {
		if !strings.Contains(note, want) {
			t.Errorf("docs/coordinator.md lacks %q", want)
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
	// A2: the coordinator settings snapshot's strict sandbox-only schema.
	for _, ok := range []string{testSettings, `{"sandbox":{"enabled":true}}`, ` {"sandbox" : {"enabled" : true , "x":{"y":[1,{"z":null}]}}} ` + "\n"} {
		if err := ValidateCoordinatorSettings([]byte(ok)); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"invalid JSON":        `{"sandbox":{"enabled":true}`,
		"invalid UTF-8":       `{"sandbox":{"enabled":true,"x":"` + "\xff" + `"}}`,
		"duplicate root":      `{"sandbox":{"enabled":true},"sandbox":{"enabled":true}}`,
		"duplicate nested":    `{"sandbox":{"enabled":true,"enabled":true}}`,
		"duplicate deep":      `{"sandbox":{"enabled":true,"n":{"a":1,"a":2}}}`,
		"trailing data":       `{"sandbox":{"enabled":true}} {}`,
		"trailing garbage":    `{"sandbox":{"enabled":true}}x`,
		"size overflow":       `{"sandbox":{"enabled":true,"x":"` + strings.Repeat("x", maxSettingsBytes) + `"}}`,
		"array sandbox":       `{"sandbox":[true]}`,
		"null sandbox":        `{"sandbox":null}`,
		"string sandbox":      `{"sandbox":"on"}`,
		"extra root key":      `{"sandbox":{"enabled":true},"permissions":{}}`,
		"no sandbox":          `{"permissions":{}}`,
		"empty object":        `{}`,
		"null root":           `null`,
		"array root":          `[{"sandbox":{"enabled":true}}]`,
		"disabled":            `{"sandbox":{"enabled":false}}`,
		"string enabled":      `{"sandbox":{"enabled":"true"}}`,
		"number enabled":      `{"sandbox":{"enabled":1}}`,
		"null enabled":        `{"sandbox":{"enabled":null}}`,
		"missing enabled":     `{"sandbox":{"network":{}}}`,
		"case-folded sandbox": `{"Sandbox":{"enabled":true}}`,
		"empty":               ``,
	} {
		if err := ValidateCoordinatorSettings([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "settings.json")
	os.WriteFile(good, []byte(testSettings), 0o600)
	if b, err := readCoordinatorSettings(good); err != nil || string(b) != testSettings {
		t.Fatalf("read %q %v", b, err)
	}
	os.Symlink(good, filepath.Join(dir, "link.json"))
	os.Mkdir(filepath.Join(dir, "dir.json"), 0o700)
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, []byte(`{"sandbox":{"enabled":true,"x":"`+strings.Repeat("x", maxSettingsBytes)+`"}}`), 0o600)
	for _, p := range []string{filepath.Join(dir, "missing.json"), filepath.Join(dir, "link.json"), filepath.Join(dir, "dir.json"), big} {
		if _, err := readCoordinatorSettings(p); err == nil {
			t.Errorf("%s read", p)
		}
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
