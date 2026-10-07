package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Design decoder-enrollment unit tests UT-1 (capture invocation and plan)
// and UT-2 (recipes and version identity), on the injected capture world.

// capSignaler signals the capture world's groups: a running fake ends on
// TERM or KILL, every other group is already gone.
type capSignaler struct{ w *capWorld }

func (s capSignaler) Signal(pid int, sig syscall.Signal) error {
	s.w.mu.Lock()
	var p *capProc
	for _, c := range s.w.procs {
		if c.pgid == -pid {
			p = c
		}
	}
	s.w.mu.Unlock()
	if p == nil {
		return syscall.ESRCH
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gone {
		return syscall.ESRCH
	}
	if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
		p.gone = true
		name := "terminated"
		p.signal = &name
		close(p.exited)
	}
	return nil
}

// capEnv is the CLI environment over the capture world, with no CI.
func capEnv(t *testing.T, w *capWorld, args ...string) Env {
	var out, errOut bytes.Buffer
	return Env{GOOS: "linux", GOARCH: "amd64", Args: args, Getenv: func(string) string { return "" }, LookupEnv: func(string) (string, bool) { return "", false },
		Environ: []string{"PATH=/usr/bin"}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Signaler: capSignaler{w}, Launcher: w, Clock: w.clock,
		Registry: panicRegistry(), Hostname: "host-1", Executable: "/opt/mcpqual", Home: "/home/owner", User: "owner", Now: func() time.Time { return epoch },
		Rand: bytes.NewReader(bytes.Repeat([]byte{9}, 64)), Notify: func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }}
}

// panicRegistry is the decoder spy: capture must never select or run a
// decoder, so any decode panics.
func panicRegistry() Registry {
	reg := DefaultRegistry()
	for name, s := range reg {
		s.decode = func(Transcript) Decoded { panic("capture invoked decoder " + name) }
		reg[name] = s
	}
	return reg
}

// writeCapPlan writes a plan file whose executables are real files under
// <dir>/fake (the CLI hashes them; the fake world never runs them).
func writeCapPlan(t *testing.T, b []byte) string {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "fake"), 0o700)
	for id := range clientDecoders {
		os.WriteFile(filepath.Join(dir, "fake", id), []byte("#!/bin/false\n"), 0o600)
	}
	b = bytes.ReplaceAll(b, []byte(`"/fake/`), []byte(`"`+filepath.Join(dir, "fake")+`/`))
	p := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func cliOut(env Env) (string, string) {
	return env.Stdout.(*bytes.Buffer).String(), env.Stderr.(*bytes.Buffer).String()
}

// UT-1: CLI parsing, CI presence (absent, set, empty), no opt-in, zero
// launches before any refusal, an unknown exact version captured with no
// decoder, and qualify's unchanged exact selection.
func TestCaptureInvocation(t *testing.T) {
	plan := writeCapPlan(t, filledPlan(t, nil, "claude"))
	out := func() string { return filepath.Join(t.TempDir(), "out") }
	refusals := []struct {
		name   string
		mutate func(*Env)
		args   []string
		want   string
	}{
		{"ci-set", func(e *Env) { e.LookupEnv = func(k string) (string, bool) { return "1", k == "CI" } }, nil, "capture refused: CI is set"},
		{"ci-empty", func(e *Env) { e.LookupEnv = func(k string) (string, bool) { return "", k == "CI" } }, nil, "capture refused: CI is set"},
		{"ci-getenv", func(e *Env) { e.Getenv = func(k string) string { return map[string]string{"CI": "true"}[k] } }, nil, "capture refused: CI is set"},
		{"no-lookup", func(e *Env) { e.LookupEnv = nil }, nil, "capture refused: CI is set"},
		{"opt-in-denied", nil, []string{"capture", "--plan", plan, "--out", out()}, "capture needs --allow-model-calls"},
		{"windows", func(e *Env) { e.GOOS = "windows" }, nil, "linux or darwin only"},
		{"relative-plan", nil, []string{"capture", "--plan", "plan.json", "--out", out(), "--allow-model-calls"}, "absolute clean path"},
		{"no-out", nil, []string{"capture", "--plan", plan, "--allow-model-calls"}, "--out is required"},
		{"missing-plan", nil, []string{"capture", "--plan", "/nonexistent/plan.json", "--out", out(), "--allow-model-calls"}, "no such file"},
		{"bad-flag", nil, []string{"capture", "--publish-catalog", "/r"}, "flag provided but not defined"},
		{"extra-arg", nil, []string{"capture", "--plan", plan, "--out", out(), "--allow-model-calls", "x"}, "unexpected arguments"},
		{"invalid-plan", nil, []string{"capture", "--plan", writeCapPlan(t, filledPlan(t, func(c map[string]any) { c["driver"] = "direct" }, "claude")), "--out", out(), "--allow-model-calls"},
			"a direct driver takes no model"},
		{"relative-out", nil, []string{"capture", "--plan", plan, "--out", "rel", "--allow-model-calls"}, "absolute clean path"},
	}
	for _, tc := range refusals {
		w := newCapWorld(t)
		args := tc.args
		o := out()
		if args == nil {
			args = []string{"capture", "--plan", "/does/not/exist.json", "--out", o, "--allow-model-calls"}
		}
		env := capEnv(t, w, args...)
		if tc.mutate != nil {
			tc.mutate(&env)
		}
		code := Main(context.Background(), env)
		stdout, stderr := cliOut(env)
		if code != 2 || !strings.Contains(stderr, tc.want) || len(w.kinds()) != 0 {
			t.Fatalf("%s: %d %q %q launches %v", tc.name, code, stdout, stderr, w.kinds())
		}
		if _, err := os.Stat(o); err == nil {
			t.Fatalf("%s: an output directory was created", tc.name)
		}
	}
	// The usage names the capture command.
	w := newCapWorld(t)
	env := capEnv(t, w, "help")
	if Main(context.Background(), env) != 0 || !strings.Contains(env.Stdout.(*bytes.Buffer).String(), "mcpqual capture --plan ABSOLUTE_JSON --out ABSOLUTE_NEW_OR_EMPTY_DIR --allow-model-calls") {
		t.Fatal("usage lacks capture")
	}
	// An unknown exact version (no registry entry) is captured; no decoder
	// is consulted (the registry's decoders panic).
	o := out()
	env = capEnv(t, w, "capture", "--plan", plan, "--out", o, "--allow-model-calls")
	code := Main(context.Background(), env)
	stdout, stderr := cliOut(env)
	if code != 0 || !strings.Contains(stdout, "capture complete; vendor behavior not evaluated") || !strings.Contains(stderr, "setup only; default phase not executed") ||
		!strings.Contains(stderr, "1 session per client, 120000 ms per session, 180000 ms per client") || !slices.Equal(w.kinds(), []string{"claude:version", "claude:help", "claude:session"}) {
		t.Fatalf("unknown version capture = %d %q %q %v", code, stdout, stderr, w.kinds())
	}
	if _, err := ValidateCaptureBundle(os.DirFS(o), "."); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"report.json", "catalog-patch.json"} {
		if _, err := os.Stat(filepath.Join(o, f)); err == nil {
			t.Fatalf("capture wrote %s", f)
		}
	}
	// qualify keeps its exact selection: the same unknown version is
	// refused before any session.
	qw := newWorld(t, vendorModel{version: capVersions["claude"]})
	qenv, _, _ := testEnv(t, qw, "qualify", "--plan", writePlan(t, modelPlan()), "--out", out(), "--allow-model-calls")
	r := run(qenv)
	if r.code != 5 || !strings.Contains(r.stdout, "partial") || len(qw.sessions()) != 0 {
		t.Fatalf("qualify of an unknown version = %+v", r)
	}
	// qualify refuses an empty CI= too (the presence rule), launching
	// nothing.
	qw = newWorld(t, fullModel())
	qenv, _, _ = testEnv(t, qw, "qualify", "--plan", writePlan(t, modelPlan()), "--out", out(), "--allow-model-calls")
	qenv.LookupEnv = func(k string) (string, bool) { return "", k == "CI" }
	if r := run(qenv); r.code != 2 || !strings.Contains(r.stderr, "qualify refused: CI is set") || len(qw.launches) != 0 {
		t.Fatalf("qualify with CI= = %+v", r)
	}
	// An interrupted capture exits 130; an unusable output directory is an
	// invalid invocation; a filesystem failure is exit 1.
	w = newCapWorld(t)
	env = capEnv(t, w, "capture", "--plan", plan, "--out", out(), "--allow-model-calls")
	env.Notify = func(ctx context.Context) (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c, cancel
	}
	if code := Main(context.Background(), env); code != 130 || len(w.kinds()) != 0 {
		t.Fatalf("interrupted capture = %d %v", code, w.kinds())
	}
	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "x"), nil, 0o600)
	env = capEnv(t, newCapWorld(t), "capture", "--plan", plan, "--out", full, "--allow-model-calls")
	if code := Main(context.Background(), env); code != 2 {
		t.Fatalf("nonempty --out = %d", code)
	}
	w = newCapWorld(t)
	o = out()
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "version" {
			os.MkdirAll(filepath.Join(o, "clients", "claude", FileVersionStdout), 0o700)
		}
		return w.defaults(spec)
	}
	env = capEnv(t, w, "capture", "--plan", plan, "--out", o, "--allow-model-calls")
	if code := Main(context.Background(), env); code != 1 {
		t.Fatalf("filesystem failure = %d", code)
	}
	if _, err := os.Stat(filepath.Join(o, CaptureManifestName)); err == nil {
		t.Fatal("a manifest was written after a filesystem failure")
	}
	env = capEnv(t, newCapWorld(t), "capture", "--plan", plan, "--out", out(), "--allow-model-calls")
	env.Rand = strings.NewReader("")
	if code := Main(context.Background(), env); code != 1 {
		t.Fatalf("no randomness = %d", code)
	}
}

// UT-1: ParseCapturePlan keeps every structural check of ParsePlan and
// the capture-only rules, consulting no registry.
func TestParseCapturePlan(t *testing.T) {
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		p := capPlan(t, id)
		if !p.HasIgnoredDefault() || p.Clients[0].DecoderFixture != clientDecoders[id]+"/synthetic" {
			t.Fatalf("%s: %+v", id, p.Clients[0])
		}
		// A fixture no registry knows is inert metadata for capture.
		pp := *p
		pp.Clients = []PlanClient{p.Clients[0]}
		pp.Clients[0].DecoderFixture = "anything/at-all"
		if err := pp.validate(planCheck{capture: true}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if p := mustCapturePlan(t, filledPlan(t, func(c map[string]any) { c["phases"] = map[string]any{} }, "claude")); p.HasIgnoredDefault() {
		t.Fatal("empty phases named a default phase")
	}
	if p := capPlan(t, "claude", "codex", "grok", "cursor"); len(p.Clients) != 4 {
		t.Fatal("four clients")
	}
	// The rules on struct copies of the parsed template (each mutation
	// assigns whole fields, never into the shared plan).
	base := capPlan(t, "claude")
	for name, tc := range map[string]struct {
		mutate func(c *PlanClient)
		want   string
	}{
		"direct":          {func(c *PlanClient) { c.Driver = DriverDirect }, "a direct driver takes no model"},
		"direct-no-model": {func(c *PlanClient) { c.Driver, c.Model = DriverDirect, "" }, "capture needs driver"},
		"no-model":        {func(c *PlanClient) { c.Model = "" }, "explicit model"},
		"model-not-argv":  {func(c *PlanClient) { c.Model = "other-model" }, "must appear in session.argv"},
		"no-help":         {func(c *PlanClient) { c.HelpArgv = nil }, "capture needs a help_argv"},
		"bad-help":        {func(c *PlanClient) { c.HelpArgv = []string{"--login"} }, "not an allowed metadata command"},
		"raised":          {func(c *PlanClient) { c.Config.Raised = &ConfigRecipe{Path: "r.json", Content: "{}"} }, "no raised configuration"},
		"override":        {func(c *PlanClient) { c.Override = &Override{Setting: "s", Value: "v", Explanation: "e"} }, "no raised configuration or override"},
		"progress": {func(c *PlanClient) {
			c.Phases = Phases{Default: &DefaultPhase{DelaysMS: []int64{15000}}, Progress: &struct{}{}}
		}, "no override, progress or absolute"},
		"absolute": {func(c *PlanClient) {
			c.Phases = Phases{Default: &DefaultPhase{DelaysMS: []int64{15000}}, Progress: &struct{}{}, Absolute: &DelayPhase{DelayMS: 60000}}
		}, "no override, progress or absolute"},
		"other-delays":  {func(c *PlanClient) { c.Phases = defaultOnly(5000, 15000) }, "exactly phases.default.delays_ms [15000]"},
		"empty-default": {func(c *PlanClient) { c.Phases = Phases{Default: &DefaultPhase{}} }, "exactly phases.default.delays_ms [15000]"},
		"wrong-decoder": {func(c *PlanClient) { c.Decoder = "codex-jsonl" }, "want claude-json"},
		"no-fixture":    {func(c *PlanClient) { c.DecoderFixture = " " }, "decoder_fixture is required"},
		"relative-exe":  {func(c *PlanClient) { c.Executable = "claude" }, "absolute clean path"},
		"placeholder":   {func(c *PlanClient) { c.ExpectedVersion = "<exact-cli-version>" }, "unfilled owner placeholder"},
		"unknown-ph":    {func(c *PlanClient) { c.Session.Argv = []string{"-p", "{prompt}", "{bogus}", "--model", c.Model} }, "unknown placeholder"},
		"ci-env":        {func(c *PlanClient) { c.Env = map[string]string{"CI": "1"} }, "CI can never be set"},
		"bad-env":       {func(c *PlanClient) { c.Env = map[string]string{"1BAD": "x"} }, "invalid variable"},
		"version-argv":  {func(c *PlanClient) { c.VersionArgv = []string{"-V"} }, "not an allowed metadata command"},
		"unknown-id":    {func(c *PlanClient) { c.ID = "zed" }, "unknown id"},
	} {
		p := *base
		p.Clients = []PlanClient{base.Clients[0]}
		tc.mutate(&p.Clients[0])
		if err := p.validate(planCheck{capture: true}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseCapturePlan(filledPlan(t, func(c map[string]any) { c["surprise"] = true }, "claude")); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := ParseCapturePlan(bytes.Repeat([]byte(" "), MaxPlanBytes+1)); err == nil {
		t.Fatal("oversized plan accepted")
	}
	dup := filledPlan(t, nil, "claude", "claude")
	if _, err := ParseCapturePlan(dup); err == nil || !strings.Contains(err.Error(), "duplicate client") {
		t.Fatalf("duplicate: %v", err)
	}
	// Qualification still requires a registry fixture.
	if _, err := ParsePlan(filledPlan(t, func(c map[string]any) { c["decoder_fixture"] = "claude-json/unknown" }, "claude"), DefaultRegistry()); err == nil {
		t.Fatal("qualification accepted an untested fixture")
	}
}

// UT-2: the four byte-exact recipes from the shipped templates: version,
// help, then exactly one zero-delay setup session (no default phase, the
// short plan's three-session limit lowered to one), with its exact argv,
// workspace, environment and rendered probe configuration.
func TestCaptureRecipes(t *testing.T) {
	const m = "fake-model-1"
	want := map[string]func(ws string) ([]string, []string, string, string){
		"claude": func(ws string) ([]string, []string, string, string) {
			return []string{"--help"}, []string{"-p", Prompt("claude-capture-setup"), "--output-format", "json", "--verbose", "--model", m, "--mcp-config", ws + "/probe-mcp.json",
					"--strict-mcp-config", "--allowed-tools", "mcp__probe__slow"}, "probe-mcp.json",
				`{"mcpServers":{"probe":{"command":"/opt/mcpqual","args":["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]}}}`
		},
		"codex": func(ws string) ([]string, []string, string, string) {
			return []string{"exec", "--help"}, []string{"exec", "--json", "--skip-git-repo-check", "-C", ws, "-m", m, "-c", `mcp_servers.probe.command="/opt/mcpqual"`, "-c",
					`mcp_servers.probe.args=["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]`, Prompt("codex-capture-setup")}, "probe-config.toml",
				"[mcp_servers.probe]\ncommand = \"/opt/mcpqual\"\nargs = [\"serve\", \"--case-file\", \"" + ws + "/case.json\", \"--events\", \"" + ws + "/server-events.jsonl\"]\n"
		},
		"grok": func(ws string) ([]string, []string, string, string) {
			return []string{"--help"}, []string{"-p", Prompt("grok-capture-setup"), "--output-format", "json", "-m", m}, ".grok/config.toml",
				"[mcp_servers.probe]\ncommand = \"/opt/mcpqual\"\nargs = [\"serve\", \"--case-file\", \"" + ws + "/case.json\", \"--events\", \"" + ws + "/server-events.jsonl\"]\n"
		},
		"cursor": func(ws string) ([]string, []string, string, string) {
			return []string{"--help"}, []string{"-p", Prompt("cursor-capture-setup"), "--output-format", "stream-json", "--model", m}, ".cursor/mcp.json",
				`{"mcpServers":{"probe":{"command":"/opt/mcpqual","args":["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]}}}`
		},
	}
	cases := []string{"claude", "codex", "grok", "cursor"}
	// The recipe run is input only (its launches and files), built once
	// per test process; the lifecycle tests repeat the launch paths.
	type recipeRun struct {
		w         *capWorld
		c         *CaptureRunner
		man       *CaptureManifest
		b         *CaptureBundle
		cfgSeen   map[string]string
		caseFiles map[string]CaseFile
		log       string
	}
	rr := sharedValue("recipes", func() *recipeRun {
		rr := &recipeRun{w: newCapWorld(t), cfgSeen: map[string]string{}, caseFiles: map[string]CaseFile{}}
		w := rr.w
		w.script = func(spec ProcSpec) capBehavior {
			if launchKind(spec) == "session" {
				id := clientOf(spec)
				_, _, rel, _ := want[id](spec.Dir)
				b, _ := os.ReadFile(filepath.Join(spec.Dir, filepath.FromSlash(rel)))
				raw, _ := os.ReadFile(filepath.Join(spec.Dir, "case.json"))
				cf, _ := ParseCaseFile(raw)
				w.mu.Lock()
				rr.cfgSeen[id], rr.caseFiles[id] = string(b), cf
				w.mu.Unlock()
			}
			return w.defaults(spec)
		}
		var log bytes.Buffer
		rr.c = newCapRunner(t, w, capPlan(t, cases...))
		rr.c.OutDir = filepath.Join(sharedTempDir(t), "recipes")
		rr.c.Log = &log
		rr.man, rr.b = validatedCapture(t, rr.c, context.Background())
		rr.log = log.String()
		return rr
	})
	w, c, man, b, cfgSeen, caseFiles := rr.w, rr.c, rr.man, rr.b, rr.cfgSeen, rr.caseFiles
	var log bytes.Buffer
	log.WriteString(rr.log)
	var specs []ProcSpec
	w.mu.Lock()
	specs = append(specs, w.launches...)
	w.mu.Unlock()
	// Version, help and one setup session per client, sequentially; the
	// short plans' three-session limit is lowered to one and their
	// default phase never runs.
	if man.State != CaptureComplete || man.ExitCode() != 0 || len(specs) != 12 || man.Limits.SessionsPerClient != 1 || man.Limits.Clients != 4 ||
		!strings.Contains(log.String(), "setup only; default phase not executed") || !strings.Contains(log.String(), "at most 4 clients sequentially") ||
		!strings.Contains(log.String(), "planned operational deadline 720000 ms plus bounded cleanup of up to 12 process groups") {
		t.Fatalf("four recipes: %s %v %q", man.State, w.kinds(), log.String())
	}
	done := 0
	for i, id := range cases {
		ws := filepath.Join(c.OutDir, workDir, id+"-capture-setup")
		help, argv, rel, cfg := want[id](ws)
		cc := capClient(t, man, id)
		v, h, s := specs[3*i], specs[3*i+1], specs[3*i+2]
		if cc.State != CaptureComplete || !slices.Equal(v.Args, []string{"--version"}) || !slices.Equal(h.Args, help) || !slices.Equal(s.Args, argv) {
			t.Fatalf("%s: argv %q / %q / %q (%q)", id, v.Args, h.Args, s.Args, reasonOf(cc))
		}
		if s.Dir != ws || s.Path != "/fake/"+id || !s.CaptureStderr || s.StderrPath != "" || slices.ContainsFunc(s.Env, func(kv string) bool { return strings.HasPrefix(kv, "CI=") }) ||
			!v.CaptureStderr || !h.CaptureStderr {
			t.Fatalf("%s: launch %+v", id, s)
		}
		if cfgSeen[id] != cfg || cc.Config.Path != rel || cc.Config.TimeoutOverride != TimeoutOverrideNone {
			t.Fatalf("%s: config %q, want %q", id, cfgSeen[id], cfg)
		}
		cf := caseFiles[id]
		if len(cf.Cases) != 2 || cf.Cases[0] != (ProbeCase{CaseID: id + "-capture-setup"}) || cf.Cases[1].CaseID != id+"-capture-setup-marker" || cf.Nonce != "noncecap1" || cf.RunID != "run-cap-1" {
			t.Fatalf("%s: case file %+v", id, cf)
		}
		// The manifest records the labeled argv and configuration.
		labeled := strings.ReplaceAll(strings.Join(argv, "\x00"), ws, "<workspace>")
		labeled = strings.ReplaceAll(labeled, "/opt/mcpqual", "<server>")
		if strings.Join(cc.Argv, "\x00") != labeled || cc.Model != m || cc.CaseID != id+"-capture-setup" || cc.Nonce != "noncecap1" {
			t.Fatalf("%s: manifest argv %q", id, cc.Argv)
		}
		cfgFile := string(b.Files[ClientFile(id, FileConfig)])
		if strings.Contains(cfgFile, ws) || !strings.Contains(cfgFile, "<workspace>/case.json") || !strings.Contains(cfgFile, "<server>") {
			t.Fatalf("%s: config.txt %q", id, cfgFile)
		}
		done++
	}
	if done != 4 {
		t.Fatal("recipe inventory")
	}
}

// UT-2: exact version identity (trimmed, one nonempty line, byte-equal,
// never stripped) and metadata failures: no help or session follows a
// failed or different version, and no session follows a failed help.
func TestCaptureVersionIdentity(t *testing.T) {
	exact := capVersions["grok"]
	for name, tc := range map[string]struct {
		stdout string
		ok     bool
	}{
		"exact":     {exact + "\n", true},
		"trimmed":   {"  " + exact + "  \n\n", true},
		"suffix":    {exact + " extra\n", false},
		"stripped":  {"grok 1.0.46\n", false},
		"build":     {"grok 1.0.46 (0000000000ab) [stable]\n", false},
		"multiline": {exact + "\nwarning: update available\n", false},
		"empty":     {"\n", false},
	} {
		got, single := observedVersion(tc.stdout)
		if (single && got == exact) != tc.ok {
			t.Fatalf("%s: %q %v", name, got, single)
		}
	}
	// metadataReason, in its order: launch, cleanup, interruption, held
	// stream, budget, truncation, then success.
	zero := 0
	one := 1
	for name, tc := range map[string]struct {
		run  caseRun
		want string
	}{
		"absent":    {caseRun{launchErr: fmt.Errorf("x: %w", os.ErrNotExist)}, ReasonAbsentBinary},
		"launch":    {caseRun{launchErr: fmt.Errorf("exec format error")}, ReasonLaunchFailed},
		"cleanup":   {caseRun{exit: &zero, cleanup: CaseCleanup{Error: sptr("x")}}, ReasonCleanupFailed},
		"interrupt": {caseRun{exit: &zero, interrupted: true}, ReasonInterrupted},
		"held":      {caseRun{exit: &zero, stderrHeld: true}, ReasonStreamHeld + ": the help command"},
		"budget":    {caseRun{exit: &zero, watchdog: true, budgetCapped: true}, ReasonBudget + ": max_client_ms"},
		"truncated": {caseRun{exit: &zero, stderrCut: true}, ReasonEvidenceTruncated + ": the help command"},
		"exit":      {caseRun{exit: &one}, ReasonMetadataFailed + ": the help command did not succeed"},
		"ok":        {caseRun{exit: &zero}, ""},
	} {
		if got := metadataReason(CaptureStage{State: StageRan}, tc.run, "help"); got != tc.want {
			t.Fatalf("%s: %q", name, got)
		}
	}
	// A different exact version keeps its streams and nothing follows; a
	// failed help command prevents the session; neither stops the next
	// client.
	w := newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		switch {
		case clientOf(spec) == "grok" && launchKind(spec) == "version":
			return capBehavior{stdout: exact + "\nwarning: update available\n", stderr: "a version note\n"}
		case clientOf(spec) == "claude" && launchKind(spec) == "help":
			return capBehavior{stdout: "usage\n", exit: 2}
		}
		return w.defaults(spec)
	}
	man, b := runCapture(t, newCapRunner(t, w, capPlan(t, "grok", "claude")), context.Background())
	cc := capClient(t, man, "grok")
	if reasonOf(cc) != ReasonVersionMismatch || cc.Help.State != StageNotRun || cc.Session.State != StageNotRun ||
		man.ExitCode() != 5 || *cc.ObservedVersion != exact || string(b.Files[ClientFile("grok", FileVersionStderr)]) != "a version note\n" ||
		!strings.Contains(string(b.Files[ClientFile("grok", FileVersionStdout)]), "warning: update available") {
		t.Fatalf("mismatch: %q %v", reasonOf(cc), w.kinds())
	}
	if cc := capClient(t, man, "claude"); reasonOf(cc) != ReasonMetadataFailed+": the help command did not succeed" || cc.Session.State != StageNotRun ||
		!slices.Equal(w.kinds(), []string{"grok:version", "claude:version", "claude:help"}) {
		t.Fatalf("help failure: %q %v", reasonOf(cc), w.kinds())
	}
	// An unreadable executable is never launched; a launch failure is
	// recorded without files.
	w = newCapWorld(t)
	c := newCapRunner(t, w, mustCapturePlan(t, filledPlan(t, func(cl map[string]any) { cl["executable"] = "/missing/claude" }, "claude")))
	man, _ = runCapture(t, c, context.Background())
	if cc := capClient(t, man, "claude"); reasonOf(cc) != ReasonAbsentBinary || cc.ExecutableSHA256 != nil || len(w.kinds()) != 0 {
		t.Fatalf("absent executable: %q %v", reasonOf(cc), w.kinds())
	}
	if st := stageOf(caseRun{launchErr: os.ErrNotExist}, time.Second); st.State != StageLaunchFailed || *st.Reason != ReasonAbsentBinary || st.Cleanup != nil {
		t.Fatalf("launch failure stage %+v", st)
	}
	// A metadata watchdog within the client deadline: the hanging version
	// command is reaped once its 30 s timer (armed first) fires.
	w = newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "version" {
			return capBehavior{hang: true}
		}
		return w.defaults(spec)
	}
	c = newCapRunner(t, w, capPlan(t, "claude"))
	done := make(chan *CaptureManifest, 1)
	go func() { m, _ := c.Run(context.Background()); done <- m }()
	awaitTimer(t, w.clock, versionWatchdog)
	w.clock.Advance(versionWatchdog)
	man = <-done
	if cc := capClient(t, man, "claude"); !strings.HasPrefix(reasonOf(cc), ReasonMetadataFailed) || !cc.Version.Watchdog || cc.Version.WatchdogMS != 30000 {
		t.Fatalf("metadata watchdog: %q %+v", reasonOf(cc), cc.Version)
	}
}

// UT-2: a short plan's limits lower the capture caps, never raise them;
// four clients run sequentially with their own client budgets.
func TestCaptureLimits(t *testing.T) {
	low := filledPlan(t, nil, "claude")
	var p map[string]any
	json.Unmarshal(low, &p)
	p["limits"] = map[string]any{"max_sessions_per_client": 1, "max_case_ms": 20000, "max_client_ms": 50000}
	b, _ := json.Marshal(p)
	c := newCapRunner(t, newCapWorld(t), mustCapturePlan(t, b))
	if l := c.EffectiveLimits(); l.SessionMS != 20000 || l.ClientMS != 50000 || l.MetadataMS != 20000 || l.SessionsPerClient != 1 {
		t.Fatalf("lowered limits %+v", l)
	}
	p["limits"] = map[string]any{"max_sessions_per_client": 12, "max_case_ms": 900000, "max_client_ms": 5400000}
	b, _ = json.Marshal(p)
	c = newCapRunner(t, newCapWorld(t), mustCapturePlan(t, b))
	if l := c.EffectiveLimits(); l.SessionMS != CaptureMaxSessionMS || l.ClientMS != CaptureMaxClientMS || l.MetadataMS != 30000 || l.SessionsPerClient != 1 {
		t.Fatalf("raised limits %+v", l)
	}
	if l := (CaptureLimits{Stream: 10, File: 1 << 40}).orDefault(); l.Stream != 10 || l.File != MaxEvidenceFileBytes || l.Bundle != 256<<20 {
		t.Fatalf("byte limits %+v", l)
	}
	if p := mustCapturePlan(t, filledPlan(t, func(cl map[string]any) { cl["phases"] = map[string]any{} }, "claude")); p.HasIgnoredDefault() {
		t.Fatal("an empty phases object named a default phase")
	}
	// A client whose budget is spent by its metadata gets no session.
	json.Unmarshal(filledPlan(t, nil, "claude"), &p)
	p["limits"] = map[string]any{"max_sessions_per_client": 1, "max_case_ms": 1000, "max_client_ms": 1000}
	b, _ = json.Marshal(p)
	w := newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "help" {
			w.clock.Advance(time.Second)
		}
		return w.defaults(spec)
	}
	man, _ := runCapture(t, newCapRunner(t, w, mustCapturePlan(t, b)), context.Background())
	if cc := capClient(t, man, "claude"); !strings.HasPrefix(reasonOf(cc), ReasonBudget) || cc.Session.State != StagePrepared || len(w.sessions()) != 0 {
		t.Fatalf("spent budget: %q %+v", reasonOf(cc), cc.Session)
	}
}

func discard() io.Writer { return io.Discard }
