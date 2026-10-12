package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
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
	var runtimeEntries, coordEntries []string
	f.coord.before = func(c *coordScript) bool {
		if mcp == nil {
			mcp = mustRead(t, filepath.Join(c.s.runtime, "mcp.json"))
			es, _ := os.ReadDir(c.s.runtime)
			for _, e := range es {
				runtimeEntries = append(runtimeEntries, e.Name())
			}
			ws, _ := os.ReadDir(filepath.Join(c.s.runtime, "coord", "waits"))
			for _, e := range ws {
				coordEntries = append(coordEntries, e.Name())
			}
		}
		return false
	}
	// A3.6: the session prerequisites are printed before any vendor
	// executes, the rendered list before phase 1 starts.
	var atFirstVendor, atPhase1 *string
	f.override = func(s ProcSpec) (behavior, bool) {
		out := f.stdout.String()
		if atFirstVendor == nil && slices.Contains([]string{fakeCallsheet, fakeClaude, fakeCodex, fakeGrok}, s.Path) {
			atFirstVendor = &out
		}
		if atPhase1 == nil && s.Path == fakeClaude && len(s.Args) > 0 && s.Args[0] == "-p" {
			atPhase1 = &out
		}
		return behavior{}, false
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
	rt := f.sup.runtime
	coord := filepath.Join(rt, "coord")
	mcpPath := filepath.Join(rt, "mcp.json")
	// The 16 rules, enumerated independently of the renderer.
	u, a := f.sup.dep.url, f.sup.dep.caPath
	rules := []string{"mcp__callsheet__ws_push", "mcp__callsheet__dispatch", "mcp__callsheet__task_show", "mcp__callsheet__ws_status",
		"mcp__callsheet__ws_diff", "mcp__callsheet__ws_pull", "Bash(" + fakeCallsheet + " task wait * --until-done --json --plane " + u + " --ca " + a + ")"}
	for _, h := range []string{"designer", "design-reviewer", "coder", "code-reviewer"} {
		rules = append(rules, "Bash("+fakeReale2e+" observe --run "+rt+" --task * --hop "+h+")")
	}
	for _, h := range []string{"designer", "design-reviewer", "coder", "code-reviewer"} {
		rules = append(rules, "Bash("+fakeReale2e+" capture-wait --run "+rt+" --hop "+h+" --code *)")
	}
	rules = append(rules, "Bash("+fakeReale2e+" review-material --run "+rt+")")
	for i, s := range specs {
		want := append([]string{"-p", prompts[i], "--output-format", "stream-json", "--verbose", "--settings", `{"sandbox":{"enabled":false}}`, "--allowed-tools"},
			rules...)
		want = append(want, "--mcp-config", mcpPath, "--append-system-prompt", system)
		if i == 1 {
			want = append(want, "--resume", testSession)
		}
		if !slices.Equal(s.Args, want) || s.Dir != coord || !slices.Equal(s.Env, f.world().Environ) {
			t.Fatalf("phase %d launch %q in %s env %v", i+1, s.Args, s.Dir, s.Env)
		}
		joined := strings.Join(s.Args, "\x00")
		for _, banned := range []string{"--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions",
			"--strict-mcp-config", "--setting-sources", "--session-id", "--continue", "--fork-session", "--bare", "--allowedTools", "--disallowedTools",
			"--tools", "--input-format", "\x00Bash\x00", "Bash(*)", "mcp__callsheet__*", "\x00Read", "CLAUDE_CONFIG_DIR", "TOKEN"} {
			if strings.Contains(joined, banned) {
				t.Fatalf("phase %d argv carries %q", i+1, banned)
			}
		}
		if n := strings.Count(joined, "--settings"); n != 1 || s.Args[slices.Index(s.Args, "--settings")+1] != SandboxOffSettings ||
			strings.Count(joined, "--allowed-tools") != 1 {
			t.Fatalf("phase %d settings arguments %d", i+1, n)
		}
	}
	if atFirstVendor == nil || atPhase1 == nil || !strings.Contains(*atFirstVendor, "--settings "+SandboxOffSettings) ||
		!strings.Contains(*atFirstVendor, "Bash(R observe --run D --task * --hop designer)") || strings.Contains(*atFirstVendor, rt) {
		t.Fatalf("output before the first vendor execution:\n%v", atFirstVendor)
	}
	for _, r := range rules {
		if !strings.Contains(*atPhase1, "    "+r+"\n") {
			t.Fatalf("rule %q not printed before phase 1:\n%s", r, *atPhase1)
		}
	}
	if strings.Contains(*atPhase1, prompts[0]) || strings.Contains(*atPhase1, "TOKEN") {
		t.Fatal("prompt contents printed")
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
	for i, h := range []string{"designer", "design-reviewer", "coder", "code-reviewer"} {
		dir := rt + "/coord/waits/0" + string(rune('1'+i))
		want := fakeCallsheet + " task wait TASK_ID --until-done --json --plane " + u + " --ca " + a + " >" + dir + "/result.json 2>" + dir + "/error.txt; " +
			fakeReale2e + " capture-wait --run " + rt + " --hop " + h + ` --code "$?"`
		if w := l1.WaitTemplates[i]; w != want || !strings.Contains(system, w) || !slices.Equal(l2.WaitTemplates, l1.WaitTemplates) {
			t.Fatalf("wait template %d %q", i, w)
		}
	}
	if !slices.Equal(coordEntries, []string{"01", "02", "03", "04"}) {
		t.Fatalf("precreated wait directories %v", coordEntries)
	}
	// The shipped prompt names only the six MCP tools, the direct wait and
	// the named helpers: no formatting loop or file-read utilities.
	if !strings.Contains(system, fakeReale2e+" review-material --run "+rt+"\n") || !strings.Contains(system, fakeReale2e+" observe --run "+rt+" --task TASK_ID --hop designer") {
		t.Fatal("the helper commands are not rendered")
	}
	for _, old := range []string{"sha256sum", "printf", "cut -d", "cat ", "review-files.txt", "for f in", "rc=$?", "echo "} {
		if strings.Contains(system, old) {
			t.Errorf("the coordinator prompt still uses %q", old)
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
		if !slices.Contains([]string{ownerMarker, "coord", "collect", "review", "seed", "baseline", "mcp.json", RunFileName, SocketName,
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
	// A3.6: the checker rebuilds both phases' allow-lists from the bundle
	// and refuses any other, the launch records' own hashes rebound.
	t.Run("allow-list mutations", func(t *testing.T) {
		t.Parallel()
		loaded, err := LoadBundle(passingBundle(t))
		if err != nil {
			t.Fatal(err)
		}
		ref := newChecker(loaded)
		if !ref.launchValid(1) || !ref.launchValid(2) {
			t.Fatal("the passing launches are invalid")
		}
		for name, tc := range map[string]struct {
			phase int
			edit  func(a []string, s int) []string
		}{
			"rule removed": {1, func(a []string, s int) []string { return slices.Delete(a, s+9, s+10) }},
			"wait widened": {1, func(a []string, s int) []string {
				a[s+6] = strings.Replace(a[s+6], " --until-done --json", "", 1)
				return a
			}},
			"observe widened": {1, func(a []string, s int) []string {
				a[s+7] = strings.Replace(a[s+7], "--hop designer", "--hop *", 1)
				return a
			}},
			"capture widened": {1, func(a []string, s int) []string { a[s+11] = strings.Replace(a[s+11], " --code *", " *", 1); return a }},
			"rule duplicated": {1, func(a []string, s int) []string { return slices.Insert(a, s+1, a[s]) }},
			"rules reordered": {1, func(a []string, s int) []string { a[s], a[s+1] = a[s+1], a[s]; return a }},
			"fixed path changed": {1, func(a []string, s int) []string {
				a[s+15] = strings.Replace(a[s+15], "--run /", "--run /tmp/other/", 1)
				return a
			}},
			"fixed option changed": {1, func(a []string, s int) []string {
				a[s+6] = strings.Replace(a[s+6], "--ca ", "--ca /tmp/other.crt --x ", 1)
				return a
			}},
			"general Bash":     {1, func(a []string, s int) []string { return slices.Insert(a, s+16, "Bash") }},
			"Read":             {1, func(a []string, s int) []string { return slices.Insert(a, s+16, "Read") }},
			"MCP wildcard":     {1, func(a []string, s int) []string { a[s] = "mcp__callsheet__*"; return a }},
			"skip permissions": {1, func(a []string, s int) []string { return append(a, "--dangerously-skip-permissions") }},
			"allow skip":       {1, func(a []string, s int) []string { return append(a, "--allow-dangerously-skip-permissions") }},
			"bypass mode":      {1, func(a []string, s int) []string { return append(a, "--permission-mode", "bypassPermissions") }},
			"settings widened": {1, func(a []string, s int) []string {
				a[s-2] = `{"sandbox":{"enabled":false},"permissions":{"defaultMode":"bypassPermissions"}}`
				return a
			}},
			"allow-list removed":   {1, func(a []string, s int) []string { return slices.Delete(a, s-1, s+16) }},
			"phase 2 rule removed": {2, func(a []string, s int) []string { return slices.Delete(a, s+15, s+16) }},
			"phase 2 bypass":       {2, func(a []string, s int) []string { return append(a, "--dangerously-skip-permissions") }},
		} {
			// Every mutation against the loaded passing bundle's launch
			// predicate; a few through the whole check with the manifest
			// rebound, so the hash binding never hides one.
			c := *ref
			nb := *loaded
			l := *nb.Launches[tc.phase-1]
			s := slices.Index(l.Argv, "--allowed-tools") + 1
			l.Argv = tc.edit(slices.Clone(l.Argv), s)
			nb.Launches[tc.phase-1] = &l
			c.b = &nb
			if c.launchValid(tc.phase) {
				t.Errorf("%s: the launch shape is valid", name)
			}
			if name != "rule removed" && name != "skip permissions" && name != "phase 2 rule removed" {
				continue
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				b := passingBundle(t)
				editDoc(t, b, launchFiles[tc.phase-1], func(l *LaunchRecord) {
					s := slices.Index(l.Argv, "--allowed-tools") + 1
					l.Argv = tc.edit(l.Argv, s)
				})
				editDoc(t, b, "manifest.json", func(m *Manifest) {
					for i := range m.Launches {
						m.Launches[i].SHA256 = sha256Hex(mustRead(t, filepath.Join(b, filepath.FromSlash(m.Launches[i].Name))))
					}
				})
				_, r, _ := checkBundle(t, b)
				if c := criterion(r, CritSetup); !slices.Contains(c.Codes, "launch_shape_invalid") || criterion(r, CritManifest).Result != "pass" {
					t.Fatalf("setup %+v (all %v)", c, failedCodes(r))
				}
			})
		}
	})
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
	t.Run("capture-wait helper", captureHelperCases)
	t.Run("review-material helper", reviewHelperCases)
	t.Run("rule inputs", func(t *testing.T) {
		t.Parallel()
		// A3.6: a value a rule cannot hold literally is refused before
		// any rule is rendered.
		v := permissionValues{callsheet: "/bin/callsheet", reale2e: "/bin/reale2e", url: "https://127.0.0.1:1", ca: "/rt/ca.crt", runtime: "/rt"}
		if rules, err := allowedTools(v); err != nil || len(rules) != 16 {
			t.Fatalf("rules %d %v", len(rules), err)
		}
		for _, bad := range []string{"/bin/call*sheet", "/bin/a(b)", "/bin/a?", "/bin/[x]", "/bin/a,b", "/bin/a\nb", "/bin/a\x00"} {
			w := v
			w.reale2e = bad
			if _, err := allowedTools(w); !errors.Is(err, errRuleInput) {
				t.Errorf("%q rendered: %v", bad, err)
			}
		}
		w := v
		w.runtime = "/tmp/run dir"
		if rules, err := allowedTools(w); err != nil || !strings.Contains(rules[15], "--run '/tmp/run dir'") {
			t.Fatalf("a quoted runtime %q %v", rules, err)
		}
		f := newFakeWorld(t)
		f.executable = "/fake/bin/re*ale2e"
		if code := f.attempt(); code == 0 || !strings.Contains(f.stderr.String()+f.stdout.String(), "permission_rule_invalid") || len(f.launcher.argv()) != 0 {
			t.Fatalf("an unquotable executable = %d, launched %v", code, f.launcher.argv())
		}
	})
}

// helperRun is the given reale2e helper command through MainWith.
func helperRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := MainWith(Host{GOOS: "linux", Args: args, Getenv: func(string) string { return "" }, LookupEnv: func(string) (string, bool) { return "", true },
		Stdout: &out, Stderr: &errOut, NewWorld: func(Host) (*World, func(), error) { t.Fatal("world built"); return nil, nil, nil }})
	return code, out.String(), errOut.String()
}

// helperRunID is the live runtime's run in the helper cases.
const helperRunID = "20261010T120000Z-0a0b0c0d"

// helperRuntime is a live runtime for the coordinator's helpers: its run
// file, ownership marker, wait and pull directories and a control socket
// answering with handle.
func helperRuntime(t *testing.T, handle func(ControlRequest) controlResponse) string {
	t.Helper()
	rt := shortTemp(t)
	write(t, filepath.Join(rt, RunFileName), `{"schema":"`+RunFileSchema+`","run_id":"`+helperRunID+`"}`+"\n")
	write(t, filepath.Join(rt, ownerMarker), helperRunID+"\n")
	for i := range Hops {
		os.MkdirAll(waitDir(rt, i), 0o700)
	}
	os.MkdirAll(filepath.Join(rt, "coord", "coder"), 0o700)
	srv, err := listenControl(rt, helperRunID, handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.close)
	return rt
}

// captureHelperCases (UT-3, A3.6): capture-wait saves and returns the
// wait's status only after the supervisor admits the hop's capture, never
// overwrites, and refuses bad options, stale runs and unsafe or oversized
// files without writing an exit artifact.
func captureHelperCases(t *testing.T) {
	t.Parallel()
	accept := func(r ControlRequest) controlResponse {
		if r.Op == opCapture && r.Hop == HopCoder {
			return codeResponse(CodeAccepted)
		}
		return codeResponse(CodeTaskMismatch)
	}
	const result, stderr = `{"status":"terminal"}` + "\n", "warning\n"
	fresh := func() (string, string) {
		rt := helperRuntime(t, accept)
		dir := waitDir(rt, 2)
		write(t, filepath.Join(dir, "result.json"), result)
		write(t, filepath.Join(dir, "error.txt"), stderr)
		return rt, dir
	}
	for _, c := range []struct {
		args []string
		want int
	}{{[]string{"--code", "0"}, 0}, {[]string{"--code", "7"}, 7}, {[]string{"--code=255"}, 255}} {
		rt, dir := fresh()
		code, out, errOut := helperRun(t, append([]string{"capture-wait", "--run", rt, "--hop", HopCoder}, c.args...)...)
		if code != c.want || out != result || errOut != stderr || string(mustRead(t, filepath.Join(dir, "exit-code"))) != strconv.Itoa(c.want)+"\n" {
			t.Fatalf("capture %v = %d %q %q", c.args, code, out, errOut)
		}
		// Never overwritten.
		if code, _, _ := helperRun(t, "capture-wait", "--run", rt, "--hop", HopCoder, "--code", "1"); code != 2 ||
			string(mustRead(t, filepath.Join(dir, "exit-code"))) != strconv.Itoa(c.want)+"\n" {
			t.Fatalf("a second capture = %d", code)
		}
	}
	noExit := func(name, dir string) {
		t.Helper()
		if _, err := os.Lstat(filepath.Join(dir, "exit-code")); !os.IsNotExist(err) {
			t.Errorf("%s wrote an exit artifact", name)
		}
	}
	// Refused by the supervisor: exit 1, nothing written.
	rt, dir := fresh()
	if code, out, errOut := helperRun(t, "capture-wait", "--run", rt, "--hop", HopDesigner, "--code", "0"); code != 1 || out != "" ||
		!strings.Contains(errOut, "refused: task_mismatch") {
		t.Fatalf("refused capture = %d %q %q", code, out, errOut)
	}
	noExit("refusal", waitDir(rt, 0))
	// Options: exactly --run, --hop and a canonical --code of 0..255.
	for _, args := range [][]string{{"--hop", HopCoder}, {"--hop", HopCoder, "--code", "256"}, {"--hop", HopCoder, "--code", "-1"},
		{"--hop", HopCoder, "--code", "07"}, {"--hop", HopCoder, "--code", "+7"}, {"--hop", HopCoder, "--code", "x"}, {"--hop", HopCoder, "--code", ""},
		{"--hop", HopCoder, "--code", "0", "--task", "t"}, {"--hop", HopCoder, "--hop", HopCoder, "--code", "0"}, {"--hop", HopCoder, "--code", "0", "extra"},
		{"--hop", "planner", "--code", "0"}, {"--hop", HopCoder, "--code"}, {"-hop", HopCoder, "--code", "0"}} {
		if code, _, errOut := helperRun(t, append([]string{"capture-wait", "--run", rt}, args...)...); code != 2 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v = %d %q", args, code, errOut)
		}
	}
	if code, _, errOut := helperRun(t, "capture-wait", "--run", "rel/rt", "--hop", HopCoder, "--code", "0"); code != 2 || !strings.Contains(errOut, "usage:") {
		t.Errorf("relative runtime = %d %q", code, errOut)
	}
	noExit("usage", dir)
	// Not the live runtime: unclean, a link, a stale marker, a run file
	// that is a link, a run the supervisor does not serve.
	link := filepath.Join(shortTemp(t), "rt")
	os.Symlink(rt, link)
	for name, run := range map[string]string{"unclean": rt + "/.", "link": link} {
		if code, _, _ := helperRun(t, "capture-wait", "--run", run, "--hop", HopCoder, "--code", "0"); code != 2 {
			t.Errorf("%s runtime = %d", name, code)
		}
	}
	stale, sdir := fresh()
	write(t, filepath.Join(stale, ownerMarker), "20200101T000000Z-00000000\n")
	if code, _, _ := helperRun(t, "capture-wait", "--run", stale, "--hop", HopCoder, "--code", "0"); code != 2 {
		t.Errorf("stale marker = %d", code)
	}
	noExit("stale marker", sdir)
	other, odir := fresh()
	write(t, filepath.Join(other, RunFileName), `{"schema":"`+RunFileSchema+`","run_id":"20200101T000000Z-00000000"}`+"\n")
	write(t, filepath.Join(other, ownerMarker), "20200101T000000Z-00000000\n")
	if code, _, errOut := helperRun(t, "capture-wait", "--run", other, "--hop", HopCoder, "--code", "0"); code != 1 || !strings.Contains(errOut, "run_mismatch") {
		t.Errorf("another run = %d %q", code, errOut)
	}
	noExit("another run", odir)
	runLink, _ := fresh()
	os.Rename(filepath.Join(runLink, RunFileName), filepath.Join(runLink, "run.real"))
	os.Symlink(filepath.Join(runLink, "run.real"), filepath.Join(runLink, RunFileName))
	if code, _, _ := helperRun(t, "capture-wait", "--run", runLink, "--hop", HopCoder, "--code", "0"); code != 2 {
		t.Errorf("a linked run file = %d", code)
	}
	// Unsafe or oversized wait files: refused, no exit artifact.
	for name, f := range map[string]func(rt, dir string){
		"result missing": func(rt, dir string) { os.Remove(filepath.Join(dir, "result.json")) },
		"stderr missing": func(rt, dir string) { os.Remove(filepath.Join(dir, "error.txt")) },
		"result link": func(rt, dir string) {
			os.Rename(filepath.Join(dir, "result.json"), filepath.Join(rt, "elsewhere.json"))
			os.Symlink(filepath.Join(rt, "elsewhere.json"), filepath.Join(dir, "result.json"))
		},
		"result fifo": func(rt, dir string) {
			os.Remove(filepath.Join(dir, "result.json"))
			if err := syscall.Mkfifo(filepath.Join(dir, "result.json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory link": func(rt, dir string) {
			os.Rename(dir, dir+".real")
			os.Symlink(dir+".real", dir)
		},
		"exit-code link": func(rt, dir string) { os.Symlink(filepath.Join(rt, "target"), filepath.Join(dir, "exit-code")) },
		"oversized":      func(rt, dir string) { truncateTo(t, filepath.Join(dir, "result.json"), maxTextFile+1) },
	} {
		rt, dir := fresh()
		f(rt, dir)
		if code, out, _ := helperRun(t, "capture-wait", "--run", rt, "--hop", HopCoder, "--code", "0"); code != 2 || out != "" {
			t.Errorf("%s = %d %q", name, code, out)
		}
		if name != "exit-code link" && name != "directory link" {
			noExit(name, dir)
		}
		if _, err := os.Lstat(filepath.Join(rt, "target")); !os.IsNotExist(err) {
			t.Errorf("%s: a link was followed", name)
		}
	}
	// No supervisor: a transport error.
	gone := shortTemp(t)
	write(t, filepath.Join(gone, RunFileName), `{"schema":"`+RunFileSchema+`","run_id":"`+helperRunID+`"}`+"\n")
	write(t, filepath.Join(gone, ownerMarker), helperRunID+"\n")
	if code, _, errOut := helperRun(t, "capture-wait", "--run", gone, "--hop", HopCoder, "--code", "0"); code != 2 || !strings.Contains(errOut, "cannot reach the supervisor") {
		t.Errorf("no supervisor = %d %q", code, errOut)
	}
}

// reviewHelperCases (UT-3, A3.6): review-material prints the three pulled
// coder files in the labelled format, exactly as the supervisor hashed the
// admitted commit, or nothing at all.
func reviewHelperCases(t *testing.T) {
	t.Parallel()
	files := map[string][]byte{"design.md": []byte("# Design\n"), "main.go": []byte("package main\n"), "main_test.go": []byte("package main\n\n// test\n")}
	commit := strings.Repeat("c", 40)
	answer := func(gate bool) func(ControlRequest) controlResponse {
		return func(r ControlRequest) controlResponse {
			if r.Op != opReview || !gate {
				return codeResponse(CodeOwnerGateClosed)
			}
			resp := controlResponse{OK: true, Code: CodeAccepted, Commit: commit}
			for _, p := range []string{"design.md", "main.go", "main_test.go"} {
				resp.Files = append(resp.Files, ReviewFile{Path: p, SHA256: sha256Hex(files[p])})
			}
			return resp
		}
	}
	fresh := func(gate bool) string {
		rt := helperRuntime(t, answer(gate))
		for p, b := range files {
			write(t, filepath.Join(rt, "coord", "coder", p), string(b))
		}
		return rt
	}
	var want strings.Builder
	for _, p := range []string{"design.md", "main.go", "main_test.go"} {
		fmt.Fprintf(&want, "----- BEGIN FILE %s sha256=%s -----\n%s\n----- END FILE %s -----\n", p, sha256Hex(files[p]), files[p], p)
	}
	rt := fresh(true)
	if code, out, errOut := helperRun(t, "review-material", "--run", rt); code != 0 || out != want.String() || errOut != "" {
		t.Fatalf("review-material = %d %q %q", code, out, errOut)
	}
	if code, out, _ := helperRun(t, "review-material", "--run="+rt); code != 0 || out != want.String() {
		t.Fatalf("inline option = %d", code)
	}
	// The gate closed or no admitted coder result: refused, no output.
	if code, out, errOut := helperRun(t, "review-material", "--run", fresh(false)); code != 1 || out != "" || !strings.Contains(errOut, "owner_gate_closed") {
		t.Fatalf("gate closed = %d %q %q", code, out, errOut)
	}
	for _, args := range [][]string{{}, {"--run", rt, "--run", rt}, {"--run", rt, "--hop", HopCoder}, {"--run", rt, "x"}, {"--run", "rel"}} {
		if code, out, errOut := helperRun(t, append([]string{"review-material"}, args...)...); code != 2 || out != "" || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v = %d %q", args, code, errOut)
		}
	}
	// An altered, missing, linked or oversized pulled file: no partial
	// output.
	for name, c := range map[string]struct {
		f    func(dir string)
		code int
	}{
		"altered": {func(dir string) { write(t, filepath.Join(dir, "main.go"), "package evil\n") }, 1},
		"missing": {func(dir string) { os.Remove(filepath.Join(dir, "main_test.go")) }, 2},
		"file link": {func(dir string) {
			os.Rename(filepath.Join(dir, "main.go"), filepath.Join(dir, "..", "main.go"))
			os.Symlink(filepath.Join(dir, "..", "main.go"), filepath.Join(dir, "main.go"))
		}, 2},
		"directory link": {func(dir string) {
			os.Rename(dir, dir+".real")
			os.Symlink(dir+".real", dir)
		}, 2},
		"oversized": {func(dir string) { truncateTo(t, filepath.Join(dir, "design.md"), projectBound+1) }, 2},
	} {
		rt := fresh(true)
		c.f(filepath.Join(rt, "coord", "coder"))
		if code, out, _ := helperRun(t, "review-material", "--run", rt); code != c.code || out != "" {
			t.Errorf("%s = %d %q", name, code, out)
		}
	}
	// The labelled output itself over the reviewer's prompt bound, and a
	// supervisor answer naming other paths.
	dir := t.TempDir()
	big := map[string][]byte{"design.md": bytes.Repeat([]byte("d"), adapter.MaxGrokPromptBytes/2), "main.go": bytes.Repeat([]byte("m"), adapter.MaxGrokPromptBytes/2),
		"main_test.go": []byte("t")}
	var sums []ReviewFile
	for _, p := range []string{"design.md", "main.go", "main_test.go"} {
		write(t, filepath.Join(dir, "coord", "coder", p), string(big[p]))
		sums = append(sums, ReviewFile{Path: p, SHA256: sha256Hex(big[p])})
	}
	if _, err := reviewOutput(dir, sums); !errors.Is(err, errHelperBound) {
		t.Fatalf("an oversized review: %v", err)
	}
	if _, err := reviewOutput(dir, []ReviewFile{sums[0], {Path: "../x", SHA256: zeros64}, sums[2]}); !errors.Is(err, errReviewMismatch) {
		t.Fatalf("a traversal path: %v", err)
	}
	if _, err := reviewOutput(dir, sums[:2]); !errors.Is(err, errReviewMismatch) {
		t.Fatalf("two files: %v", err)
	}
}
