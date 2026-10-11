package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRunbookSetup (FP-3, amendment A3): one MainWith run launches the
// headless coordinator itself in two print-mode phases of one session:
// phase 1 with the exact configuration-preserving argv (stream-json, the
// one session-only sandbox-off settings argument, the generated MCP
// configuration, the rendered instructions; no bypass, permission mode,
// tool or setting-source override, session pinning or MCP isolation),
// the owner's inherited environment and no token; phase 2 resumes the
// session phase 1 reported, automatically after the yes, with the same
// configuration; the run completes, collects, checks and audits with no
// owner file and no finish command. A CLAUDE_CONFIG_DIR, even empty, is
// refused before any capability or fingerprint exists.
func TestRunbookSetup(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	var mcp []byte
	var runtimeEntries []string
	f.coord.before = func(c *coordScript) bool {
		if mcp == nil {
			mcp = mustRead(t, filepath.Join(c.s.runtime, "mcp.json"))
			es, _ := os.ReadDir(c.s.runtime)
			for _, e := range es {
				runtimeEntries = append(runtimeEntries, e.Name())
			}
		}
		return false
	}
	if code := f.attempt(); code != 0 {
		t.Fatalf("attempt = %d\n%s\n%s", code, f.stdout.String(), f.stderr.String())
	}
	b := f.bundle()
	system := string(mustRead(t, filepath.Join(b, "setup", "coordinator-prompt.md")))
	prompts := [2]string{string(mustRead(t, filepath.Join(b, "setup", PhasePrompt1))), string(mustRead(t, filepath.Join(b, "setup", PhasePrompt2)))}
	specs := f.launcher.phaseSpecs()
	if len(specs) != 2 {
		t.Fatalf("coordinator launches %v", f.launcher.argv())
	}
	coord := filepath.Join(f.sup.runtime, "coord")
	mcpPath := filepath.Join(f.sup.runtime, "mcp.json")
	for i, s := range specs {
		want := expectedArgv(i+1, prompts[i], mcpPath, system, map[bool]string{false: "", true: testSession}[i == 1])
		if !slices.Equal(s.Args, want) || s.Dir != coord || !slices.Equal(s.Env, f.world().Environ) {
			t.Fatalf("phase %d launch %q in %s env %v", i+1, s.Args, s.Dir, s.Env)
		}
		joined := strings.Join(s.Args, "\x00")
		for _, banned := range []string{"--dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--strict-mcp-config", "--setting-sources",
			"--session-id", "--continue", "--fork-session", "--bare", "--allowedTools", "--disallowedTools", "--input-format", "CLAUDE_CONFIG_DIR", "TOKEN"} {
			if strings.Contains(joined, banned) {
				t.Fatalf("phase %d argv carries %q", i+1, banned)
			}
		}
		if n := strings.Count(joined, "--settings"); n != 1 || s.Args[slices.Index(s.Args, "--settings")+1] != SandboxOffSettings {
			t.Fatalf("phase %d settings arguments %d", i+1, n)
		}
	}
	// The rendered inputs: concrete values only, the session-local MCP
	// configuration, the four direct wait templates.
	if strings.Contains(system, "{{") || strings.Contains(prompts[0], "{{") || strings.Contains(prompts[1], "{{") {
		t.Fatal("an unrendered placeholder")
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcp, &cfg); err != nil || len(cfg.MCPServers) != 1 || cfg.MCPServers["callsheet"].Command != fakeCallsheet ||
		!slices.Equal(cfg.MCPServers["callsheet"].Args[len(cfg.MCPServers["callsheet"].Args)-2:], []string{"--wait-call-budget", "10s"}) {
		t.Fatalf("mcp config %s %v", mcp, err)
	}
	var l1, l2 LaunchRecord
	readDoc(t, filepath.Join(b, filepath.FromSlash(launchFiles[0])), &l1)
	readDoc(t, filepath.Join(b, filepath.FromSlash(launchFiles[1])), &l2)
	for i := range Hops {
		w := l1.WaitTemplates[i]
		if !strings.Contains(system, w) || !strings.Contains(w, fakeCallsheet+" task wait TASK_ID --until-done --json --plane ") || strings.Contains(w, "&") ||
			!strings.HasSuffix(w, "; exit $rc") || !strings.Contains(w, filepath.Join("waits", hopDir(i), "exit-code")) {
			t.Fatalf("wait template %d %q", i, w)
		}
	}
	if l2.ResumeSession != testSession || l1.ResumeSession != "" || l1.Executable != fakeClaude {
		t.Fatalf("launch records %+v %+v", l1, l2)
	}
	display := strings.Join(l1.Display, " ")
	if strings.Contains(display, f.sup.runtime) || strings.Contains(display, prompts[0]) || !strings.Contains(display, "$RUN/mcp.json") ||
		!strings.Contains(display, "<phase-1 prompt sha256="+sha256Hex([]byte(prompts[0]))+">") {
		t.Fatalf("sanitized launch %q", display)
	}
	// No owner-written file: the runtime held only harness-owned entries.
	for _, e := range runtimeEntries {
		if !slices.Contains([]string{ownerMarker, "coord", "waits", "collect", "review", "seed", "baseline", "mcp.json", RunFileName, SocketName,
			"p", "m"}, e) && !strings.HasPrefix(e, "n-") {
			t.Errorf("unexpected runtime entry %s", e)
		}
	}
	// The session evidence and the machine-derived map, then PASS.
	for _, name := range []string{phaseStreamFiles[0], phaseStreamFiles[1], transcriptFiles[0], transcriptFiles[1], eventMapFile, coordinatorExitFile, "audit.json", "audit.md"} {
		if _, err := os.Stat(filepath.Join(b, filepath.FromSlash(name))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var em EventMap
	readDoc(t, filepath.Join(b, filepath.FromSlash(eventMapFile)), &em)
	if em.Source != MapSourceDerived || em.SessionID != testSession || len(em.Findings) != 0 || len(em.Init) != 2 {
		t.Fatalf("event map %+v", em)
	}
	if r := f.report(); r.Result != ResultPass || r.Judgments != PendingReview {
		t.Fatalf("report %v", failedCodes(r))
	}
	out := f.stdout.String()
	if !strings.Contains(out, JudgmentReportLine) || !strings.Contains(out, filepath.Join(b, "audit.md")) || !strings.Contains(out, filepath.Join(b, "report.txt")) {
		t.Fatalf("final output:\n%s", out)
	}
	// CLAUDE_CONFIG_DIR, even empty, is refused right after the gate.
	for _, v := range []string{"", "/elsewhere"} {
		g := newFakeWorld(t)
		g.env[EnvClaudeConfigDir] = v
		h := g.host()
		h.NewWorld = func(Host) (*World, func(), error) { t.Fatal("a world was built"); return nil, nil, nil }
		if code := MainWith(h); code != 2 || !strings.Contains(g.stderr.String(), "claude_config_dir_unsupported") {
			t.Fatalf("CLAUDE_CONFIG_DIR=%q = %d %q", v, code, g.stderr.String())
		}
		if _, err := os.Stat(g.evidence); !os.IsNotExist(err) {
			t.Fatal("evidence created")
		}
	}
}

// TestSetupUnits (UT-3): rendering is deterministic and refuses unknown or
// unset placeholders; hashes follow the template order; shell quoting; the
// transcript is located read-only exactly once (missing, ambiguous,
// non-regular and symlinked candidates refused); a stream's session ID
// comes from its init record only.
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
	if len(hs) != len(TemplateNames()) || hs[0].Name != FlowFile || hs[0].SHA256 != sha256Hex([]byte(FlowFile)) || hs[2].Name != PhasePrompt1 {
		t.Fatalf("hashes %v", hs)
	}
	if _, err := loadTemplates(t.TempDir()); err == nil {
		t.Fatal("missing templates loaded")
	}
	if q := shellQuote("a b'c"); q != `'a b'\''c'` || shellQuote("/x/y") != "/x/y" || shellQuote("") != "''" {
		t.Fatalf("quote %q", q)
	}
	// The transcript search.
	home := t.TempDir()
	if _, _, err := findTranscript(home, testSession); err == nil {
		t.Fatal("found in a home without projects")
	}
	home, _ = filepath.EvalSymlinks(home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	write(t, filepath.Join(home, ".claude", "projects"), "a file")
	if _, _, err := findTranscript(home, testSession); err == nil {
		t.Fatal("found in a projects file")
	}
	os.Remove(filepath.Join(home, ".claude", "projects"))
	proj := filepath.Join(home, ".claude", "projects")
	os.MkdirAll(filepath.Join(proj, "a"), 0o700)
	os.MkdirAll(filepath.Join(proj, "b"), 0o700)
	os.WriteFile(filepath.Join(proj, "stray.jsonl"), nil, 0o600)
	if _, _, err := findTranscript(home, testSession); err == nil {
		t.Fatal("found nothing")
	}
	one := filepath.Join(proj, "a", testSession+".jsonl")
	os.WriteFile(one, []byte("{}\n"), 0o600)
	if p, rel, err := findTranscript(home, testSession); err != nil || p != one || rel != "projects/a/"+testSession+".jsonl" {
		t.Fatalf("unique transcript = %q %q %v", p, rel, err)
	}
	os.Symlink(one, filepath.Join(proj, "b", testSession+".jsonl"))
	if _, _, err := findTranscript(home, testSession); err == nil {
		t.Fatal("a symlinked candidate was accepted")
	}
	os.Remove(filepath.Join(proj, "b", testSession+".jsonl"))
	os.WriteFile(filepath.Join(proj, "b", testSession+".jsonl"), []byte("{}\n"), 0o600)
	if _, _, err := findTranscript(home, testSession); err == nil {
		t.Fatal("an ambiguous transcript was accepted")
	}
	// C11: the projects directory is read in bounded chunks.
	e := &endlessDirs{t: t, big: map[string]bool{"projects": true}}
	if _, err := readDirBounded(e.open, "projects", maxProjectDirs, nil); !errors.Is(err, errDirBudget) || e.read > maxProjectDirs+dirChunk {
		t.Fatalf("an endless projects directory: %v after %d entries", err, e.read)
	}
	if _, err := readDirBounded(e.open, "projects", maxProjectDirs, func() bool { return true }); !errors.Is(err, errDirDeadline) {
		t.Fatalf("a read past the deadline: %v", err)
	}
	// C9: a symlinked projects directory is never followed out of the
	// protected home; a symlinked vendor home is resolved (A3.4).
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(outside, "a"), 0o700)
	os.WriteFile(filepath.Join(outside, "a", testSession+".jsonl"), []byte("{}\n"), 0o600)
	lhome := t.TempDir()
	os.MkdirAll(filepath.Join(lhome, ".claude"), 0o700)
	os.Symlink(outside, filepath.Join(lhome, ".claude", "projects"))
	if p, _, err := findTranscript(lhome, testSession); err == nil {
		t.Fatalf("a transcript through a symlinked projects directory: %s", p)
	}
	vhome, real := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(real, "projects", "a"), 0o700)
	os.WriteFile(filepath.Join(real, "projects", "a", testSession+".jsonl"), []byte("{}\n"), 0o600)
	os.Symlink(real, filepath.Join(vhome, ".claude"))
	creal, _ := filepath.EvalSymlinks(real)
	if p, rel, err := findTranscript(vhome, testSession); err != nil || p != filepath.Join(creal, "projects", "a", testSession+".jsonl") || rel != "projects/a/"+testSession+".jsonl" {
		t.Fatalf("a symlinked vendor home = %q %q %v", p, rel, err)
	}
	if streamSession([]byte(`{"type":"assistant","session_id":"x"}`+"\n"+`{"type":"system","subtype":"init","session_id":"`+testSession+`"}`+"\n")) != testSession ||
		streamSession([]byte("not json\n")) != "" {
		t.Fatal("stream session")
	}
}
