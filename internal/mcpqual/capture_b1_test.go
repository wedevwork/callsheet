package mcpqual

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Design decoder-enrollment B1 unit tests: UT-10 (capture-side protocol
// evidence), UT-11 (Codex per-tool approval), UT-12 (Grok trusted recipe,
// placement gate, stream redaction and leader ownership) and UT-13 (Cursor
// trusted recipe and approval preparation), on the injected capture world
// with tiny injected filesystems and inventories. No process starts, no
// vendor CLI runs and no clock advances unless a test advances it.

// b1Capture runs plan p on w (mutate adjusts the runner) and returns the
// manifest and the bundle validated from disk.
func b1Capture(t *testing.T, w *capWorld, p *Plan, mutate func(*CaptureRunner)) (*CaptureManifest, *CaptureBundle, *CaptureRunner) {
	t.Helper()
	c := newCapRunner(t, w, p)
	if mutate != nil {
		mutate(c)
	}
	man, _ := runCapture(t, c, context.Background())
	b, err := ValidateCaptureBundle(os.DirFS(c.OutDir), ".")
	if err != nil {
		t.Fatal(err)
	}
	return man, b, c
}

// session scripts only the session launch of w.
func (w *capWorld) session(b func(spec ProcSpec) capBehavior) {
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "session" {
			return b(spec)
		}
		return w.defaults(spec)
	}
}

// UT-10: a capture keeps the probe's negotiation evidence in the raw,
// sanitized server events (hashed in the inventory) while the manifest's
// clientInfo summary is unchanged; a log whose selected version is not
// the probe's is invalid evidence.
func TestCaptureNegotiationEvidence(t *testing.T) {
	negotiated := func(selected string) func(CaseFile, string) string {
		return func(cf CaseFile, id string) string {
			name, ver, req := "codex-cli", "0.160.0", "2099-01-01"
			return probeLog(cf.RunID, ProbeEvent{Kind: EvStart},
				ProbeEvent{Kind: EvInitialize, ClientName: &name, ClientVersion: &ver, RequestedProtocolVersion: &req, SelectedProtocolVersion: &selected},
				ProbeEvent{Kind: EvReceipt, CaseID: id, RequestID: json.RawMessage(`1`)}, ProbeEvent{Kind: EvCompleted, CaseID: id, RequestID: json.RawMessage(`1`)},
				ProbeEvent{Kind: EvEOF}, ProbeEvent{Kind: EvExit, Reason: "eof"})
		}
	}
	w := newCapWorld(t)
	w.session(func(ProcSpec) capBehavior {
		return capBehavior{stdout: capTranscript, events: negotiated(ProtocolVersion)}
	})
	man, b, _ := b1Capture(t, w, capPlan(t, "codex"), nil)
	cc := capClient(t, man, "codex")
	server := string(b.Files[ClientFile("codex", FileServerEvents)])
	if cc.State != CaptureComplete || *cc.Probe.ClientName != "codex-cli" || !strings.Contains(server, `"requested_protocol_version":"2099-01-01","selected_protocol_version":"2025-06-18"`) {
		t.Fatalf("negotiated capture: %q %s", reasonOf(cc), server)
	}
	if err := RedactionFixedPoint(FileServerEvents, b.Files[ClientFile("codex", FileServerEvents)], NewCaptureRedactor(nil, nil)); err != nil {
		t.Fatal(err)
	}
	w = newCapWorld(t)
	w.session(func(ProcSpec) capBehavior {
		return capBehavior{stdout: capTranscript, events: negotiated("2024-11-05")}
	})
	man, _, _ = b1Capture(t, w, capPlan(t, "codex"), nil)
	if cc := capClient(t, man, "codex"); reasonOf(cc) != ReasonProbeAnomaly+": events_invalid" {
		t.Fatalf("forged selection: %q", reasonOf(cc))
	}
}

// UT-11: the Codex session grants the recognized per-tool approval value
// to probe.slow only, for this invocation (argv), with the TOML snapshot as
// evidence; nothing broader. A build that rejects the key, or a managed
// policy that still denies the call, is a partial capture with exactly one
// session (no fallback, no retry).
func TestCaptureCodexApproval(t *testing.T) {
	w := newCapWorld(t)
	man, b, c := b1Capture(t, w, capPlan(t, "codex"), nil)
	cc := capClient(t, man, "codex")
	s := w.sessions()[0]
	var overrides []string
	for i, a := range s.Args {
		if a == "-c" {
			overrides = append(overrides, s.Args[i+1])
		}
	}
	ws := filepath.Join(c.OutDir, workDir, "codex-capture-setup")
	want := []string{`mcp_servers.probe.command="/opt/mcpqual"`,
		`mcp_servers.probe.args=["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]`,
		`mcp_servers.probe.enabled_tools=["slow"]`, `mcp_servers.probe.tools.slow.approval_mode="approve"`}
	if cc.State != CaptureComplete || !slices.Equal(overrides, want) {
		t.Fatalf("codex overrides %q (%q)", overrides, reasonOf(cc))
	}
	for _, a := range s.Args {
		for _, bad := range []string{"--full-auto", "--approve", "--dangerously", "approval_policy", "sandbox", "--yolo"} {
			if strings.Contains(a, bad) {
				t.Fatalf("a broader grant %q", a)
			}
		}
	}
	if n := strings.Count(strings.Join(s.Args, "\x00"), "approval_mode"); n != 1 {
		t.Fatalf("%d approval settings", n)
	}
	cfg := string(b.Files[ClientFile("codex", FileConfig)])
	if cfg != "[mcp_servers.probe]\ncommand = \"<server>\"\nargs = [\"serve\", \"--case-file\", \"<workspace>/case.json\", \"--events\", \"<workspace>/server-events.jsonl\"]\n"+
		"enabled_tools = [\"slow\"]\n[mcp_servers.probe.tools.slow]\napproval_mode = \"approve\"\n" ||
		*cc.Config.Prerequisite != "Invocation-only approval for probe.slow; no server-wide approval or timeout override." ||
		!slices.Contains(cc.Argv, `mcp_servers.probe.tools.slow.approval_mode="approve"`) || !slices.Contains(cc.Argv, `mcp_servers.probe.enabled_tools=["slow"]`) {
		t.Fatalf("codex provenance %q %q", cfg, cc.Argv)
	}
	for name, sb := range map[string]capBehavior{
		"unsupported-config": {stderr: "Error: unknown variant `approve`, expected one of `auto`, `prompt`\nin `mcp_servers.probe.tools.slow.approval_mode`\n", exit: 1},
		"managed-denial": {stdout: `{"type":"item.completed","item":{"type":"mcp_tool_call","tool":"slow","status":"failed","error":{"message":"approval policy denied the call"}}}` + "\n",
			events: probeWith(exitEv())},
	} {
		w := newCapWorld(t)
		w.session(func(ProcSpec) capBehavior { return sb })
		man, _, _ := b1Capture(t, w, capPlan(t, "codex"), nil)
		cc := capClient(t, man, "codex")
		if reasonOf(cc) != ReasonProbeNotObserved || man.ExitCode() != 5 || len(w.sessions()) != 1 {
			t.Fatalf("%s: %q, %d sessions", name, reasonOf(cc), len(w.sessions()))
		}
	}
}

// UT-12/UT-13: the shared trusted-recipe validator: an exact --trust or a
// --trust= prefix (any value, also in either configuration's appended
// argv) enters it, and anything but the canonical recipe is refused with
// the exact wording; legacy plans keep their validation.
func TestTrustedRecipeValidator(t *testing.T) {
	for _, id := range []string{"grok", "cursor"} {
		base := capPlan(t, id).Clients[0]
		if entered, err := base.trustedRecipe(); !entered || err != nil {
			t.Fatalf("%s canonical: %v %v", id, entered, err)
		}
		refusal := ErrGrokTrustedRecipe
		if id == "cursor" {
			refusal = ErrCursorTrustedRecipe
		}
		argv := slices.Clone(base.Session.Argv)
		swap := func(old, new string) []string {
			out := slices.Clone(argv)
			out[slices.Index(out, old)] = new
			return out
		}
		cases := map[string]func(c *PlanClient){
			"trust-true":      func(c *PlanClient) { c.Session.Argv = swap("--trust", "--trust=true") },
			"trust-false":     func(c *PlanClient) { c.Session.Argv = swap("--trust", "--trust=false") },
			"trust-empty":     func(c *PlanClient) { c.Session.Argv = swap("--trust", "--trust=") },
			"duplicate-trust": func(c *PlanClient) { c.Session.Argv = append(slices.Clone(argv), "--trust") },
			"appended-trust":  func(c *PlanClient) { c.Config.Default.Argv = []string{"--trust"} },
			"appended-other":  func(c *PlanClient) { c.Config.Default.Argv = []string{"--verbose"} },
			"raised":          func(c *PlanClient) { c.Config.Raised = &ConfigRecipe{Path: c.Config.Default.Path, Content: "x"} },
			"raised-trust": func(c *PlanClient) {
				c.Session.Argv = []string{"-p", "{prompt}", "-m", c.Model}
				c.Config.Raised = &ConfigRecipe{Path: "r", Content: "x", Argv: []string{"--trust=1"}}
			},
			"recipe-env": func(c *PlanClient) { c.Config.Default.Env = map[string]string{"X": "1"} },
			"home":       func(c *PlanClient) { c.Env = map[string]string{"HOME": "/elsewhere"} },
			"path":       func(c *PlanClient) { c.Config.Default.Path = "elsewhere/" + c.Config.Default.Path },
			"model":      func(c *PlanClient) { c.Model = "other" },
			"extra-arg":  func(c *PlanClient) { c.Session.Argv = append(slices.Clone(argv), "--yolo") },
			"missing-arg": func(c *PlanClient) {
				i := slices.Index(argv, "--output-format")
				c.Session.Argv = slices.Delete(slices.Clone(argv), i, i+2)
			},
		}
		if id == "grok" {
			cases["grok-home"] = func(c *PlanClient) { c.Env = map[string]string{"GROK_HOME": "/elsewhere"} }
			cases["cwd-spelling"] = func(c *PlanClient) { c.Session.Argv = swap("--cwd", "--cwd={workspace}") }
			cases["duplicate-cwd"] = func(c *PlanClient) { c.Session.Argv = append(slices.Clone(argv), "--cwd", "/") }
			cases["other-cwd"] = func(c *PlanClient) { c.Session.Argv = swap("{workspace}", "/home/owner") }
		} else {
			cases["saved-workspace"] = func(c *PlanClient) { c.Session.Argv = swap("{workspace}", "my-saved-workspace") }
			cases["extra-workspace"] = func(c *PlanClient) { c.Session.Argv = append(slices.Clone(argv), "--workspace", "/") }
			cases["approve-mcps"] = func(c *PlanClient) { c.Session.Argv = append(slices.Clone(argv), "--approve-mcps") }
			cases["force"] = func(c *PlanClient) { c.Session.Argv = append(slices.Clone(argv), "--force") }
		}
		for name, mutate := range cases {
			c := base
			c.Config.Default.Env = nil
			c.Env = maps(base.Env)
			mutate(&c)
			if entered, err := c.trustedRecipe(); !entered || !errors.Is(err, refusal) {
				t.Fatalf("%s %s: %v %v", id, name, entered, err)
			}
			// A model missing from the argv is already refused by the
			// generic model rule, before the validator's wording.
			p := &Plan{Version: 1, Clients: []PlanClient{c}}
			if err := p.validate(planCheck{capture: true}); err == nil || !strings.Contains(err.Error(), refusal.Error()) && name != "model" {
				t.Fatalf("%s %s: plan %v", id, name, err)
			}
		}
		// Case-sensitive bytes: --Trust is not the flag, so the plan stays
		// legacy (validated as before).
		legacy := base
		legacy.Session.Argv = []string{"-p", "{prompt}", "--Trust", "-m", base.Model}
		if entered, err := legacy.trustedRecipe(); entered || err != nil {
			t.Fatalf("%s legacy: %v %v", id, entered, err)
		}
	}
	// Other clients never enter it, whatever their argv.
	c := capPlan(t, "claude").Clients[0]
	c.Session.Argv = append(slices.Clone(c.Session.Argv), "--trust")
	if entered, err := c.trustedRecipe(); entered || err != nil {
		t.Fatal("claude entered the trusted-recipe validator")
	}
	// The CLI refuses a non-canonical trusted plan with exit 2 before any
	// launch, with the validator's wording.
	for _, tc := range []struct{ id, want string }{{"grok", ErrGrokTrustedRecipe.Error()}, {"cursor", ErrCursorTrustedRecipe.Error()}} {
		plan := writeCapPlan(t, filledPlan(t, func(c map[string]any) {
			argv := c["session"].(map[string]any)["argv"].([]any)
			for i, a := range argv {
				if a == "--trust" {
					argv[i] = "--trust=true"
				}
			}
		}, tc.id))
		w := newCapWorld(t)
		env := capEnv(t, w, "capture", "--plan", plan, "--out", filepath.Join(t.TempDir(), "out"), "--allow-model-calls")
		code := Main(context.Background(), env)
		if _, stderr := cliOut(env); code != 2 || !strings.Contains(stderr, tc.want) || len(w.kinds()) != 0 {
			t.Fatalf("%s: %d %q %v", tc.id, code, stderr, w.kinds())
		}
	}
	// A runner given an unvalidated non-canonical plan refuses it too.
	p := *capPlan(t, "grok")
	p.Clients = []PlanClient{p.Clients[0]}
	p.Clients[0].Session.Argv = append(slices.Clone(p.Clients[0].Session.Argv), "--yolo")
	w := newCapWorld(t)
	if _, err := newCapRunner(t, w, &p).Run(context.Background()); err == nil || !strings.Contains(err.Error(), ErrGrokTrustedRecipe.Error()) || len(w.kinds()) != 0 {
		t.Fatalf("runner: %v", err)
	}
}

func maps(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// memPlacement is a tiny injected placement view: directories, files,
// symbolic links and lookup failures by absolute path, and resolved
// aliases.
type memPlacement struct {
	entries map[string]fs.FileMode
	errs    map[string]error
	links   map[string]string
}

func newMemPlacement(dirs ...string) *memPlacement {
	m := &memPlacement{entries: map[string]fs.FileMode{}, errs: map[string]error{}, links: map[string]string{}}
	for _, d := range dirs {
		for p := d; ; p = filepath.Dir(p) {
			m.entries[p] = fs.ModeDir
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	return m
}

func (m *memPlacement) Lstat(p string) (os.FileInfo, error) {
	if err, ok := m.errs[p]; ok {
		return nil, err
	}
	if mode, ok := m.entries[p]; ok {
		return memInfo{name: filepath.Base(p), mode: mode | 0o755}, nil
	}
	return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
}

func (m *memPlacement) EvalSymlinks(p string) (string, error) {
	if err, ok := m.errs["eval:"+p]; ok {
		return "", err
	}
	for from, to := range m.links {
		if p == from || strings.HasPrefix(p, from+"/") {
			return to + strings.TrimPrefix(p, from), nil
		}
	}
	return p, nil
}

// memInfo is an in-memory FileInfo (no Sys: compared by attributes).
type memInfo struct {
	name  string
	mode  fs.FileMode
	size  int64
	mtime time.Time
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return i.mode }
func (i memInfo) ModTime() time.Time { return i.mtime }
func (i memInfo) IsDir() bool        { return i.mode.IsDir() }
func (i memInfo) Sys() any           { return nil }

// UT-12: the placement gate's rules on tiny injected filesystems: a .git
// of any type (directory, linked-worktree or submodule file, symbolic
// link) anywhere on the lexical or resolved chain, an ancestor
// .grok/config.toml (including out itself on the first check), a .grok
// symbolic link or file, lookup and resolution failures, a non-directory
// component and the 256-directory bound reject; a clean external output
// passes; the workspace's own generated configuration is the sole
// exception, on the recheck only. Either Git redirection key in the
// environment rejects on presence.
func TestGrokPlacementGate(t *testing.T) {
	const out = "/srv/capture/out"
	ws := out + "/.work/grok-capture-setup"
	clean := func() *memPlacement {
		m := newMemPlacement(ws + "/.grok")
		m.entries[ws+"/.grok/config.toml"] = 0
		return m
	}
	for _, start := range []struct {
		dir string
		ws  bool
	}{{out, false}, {ws, true}} {
		if err := checkGrokPlacement(clean(), start.dir, start.ws, []string{"PATH=/bin", "HOME=/home/owner"}); err != nil {
			t.Fatalf("clean %s: %v", start.dir, err)
		}
	}
	type variant struct {
		mutate func(m *memPlacement)
		start  string
		ws     bool
		want   string
	}
	for name, v := range map[string]variant{
		"git-dir":            {func(m *memPlacement) { m.entries["/srv/.git"] = fs.ModeDir }, out, false, "a .git entry 2 levels up"},
		"git-worktree-file":  {func(m *memPlacement) { m.entries["/srv/capture/.git"] = 0 }, out, false, "a .git entry 1 levels up"},
		"git-symlink":        {func(m *memPlacement) { m.entries["/.git"] = fs.ModeSymlink }, out, false, "a .git entry 3 levels up"},
		"git-in-workspace":   {func(m *memPlacement) { m.entries[ws+"/.git"] = 0 }, ws, true, "a .git entry 0 levels up"},
		"git-in-work":        {func(m *memPlacement) { m.entries[out+"/.work/.git"] = fs.ModeDir }, ws, true, "a .git entry 1 levels up"},
		"ancestor-config":    {func(m *memPlacement) { m.entries["/srv/.grok"] = fs.ModeDir; m.entries["/srv/.grok/config.toml"] = 0 }, ws, true, ".grok/config.toml entry 4 levels up"},
		"config-on-out":      {func(m *memPlacement) { m.entries[out+"/.grok"] = fs.ModeDir; m.entries[out+"/.grok/config.toml"] = 0 }, out, false, ".grok/config.toml entry 0 levels up"},
		"config-dir-entry":   {func(m *memPlacement) { m.entries["/.grok"] = fs.ModeDir; m.entries["/.grok/config.toml"] = fs.ModeDir }, out, false, ".grok/config.toml entry 3 levels up"},
		"grok-symlink":       {func(m *memPlacement) { m.entries["/srv/.grok"] = fs.ModeSymlink }, out, false, "a .grok 2 levels up is a symbolic link"},
		"grok-file":          {func(m *memPlacement) { m.entries["/srv/.grok"] = 0 }, out, false, "a .grok 2 levels up is a symbolic link or not a directory"},
		"workspace-grok-sym": {func(m *memPlacement) { m.entries[ws+"/.grok"] = fs.ModeSymlink }, ws, true, "a .grok 0 levels up"},
		"git-lookup-error":   {func(m *memPlacement) { m.errs["/srv/.git"] = syscall.EACCES }, out, false, "a .git entry 2 levels up (or its lookup failed)"},
		"grok-lookup-error":  {func(m *memPlacement) { m.errs["/srv/.grok"] = syscall.EIO }, out, false, "the .grok lookup 2 levels up failed"},
		"config-lookup-err": {func(m *memPlacement) {
			m.entries["/srv/.grok"] = fs.ModeDir
			m.errs["/srv/.grok/config.toml"] = syscall.ENOTDIR
		}, out, false, "config.toml entry 2 levels up (or its lookup failed)"},
		"component-error": {func(m *memPlacement) { m.errs["/srv/capture"] = syscall.EACCES }, out, false, "a component 1 levels up cannot be looked up"},
		"component-file":  {func(m *memPlacement) { m.entries["/srv/capture"] = 0 }, out, false, "a component 1 levels up is not a directory"},
		"unresolved":      {func(m *memPlacement) { m.errs["eval:"+out] = syscall.ENOENT }, out, false, "does not resolve"},
		"relative":        {func(*memPlacement) {}, "srv/out", false, "not absolute"},
		"resolved-symlink": {func(m *memPlacement) {
			m.entries["/srv/capture"] = fs.ModeSymlink
			m.links["/srv/capture"] = "/srv/capture"
		}, out, false, "resolved chain: a component 1 levels up"},
		// A symlinked out whose target lies in a work tree: the lexical
		// chain is clean, the resolved chain is not.
		"symlink-alias": {func(m *memPlacement) {
			m.entries["/srv/capture"] = fs.ModeSymlink
			m.links["/srv/capture"] = "/repo/sub"
			for _, d := range []string{"/repo", "/repo/sub", "/repo/sub/out"} {
				m.entries[d] = fs.ModeDir
			}
			m.entries["/repo/.git"] = fs.ModeDir
		}, out, false, "resolved chain: a .git entry 2 levels up"},
	} {
		m := clean()
		v.mutate(m)
		err := checkGrokPlacement(m, v.start, v.ws, nil)
		if err == nil || !strings.Contains(err.Error(), v.want) {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(err.Error(), "/srv") || strings.Contains(err.Error(), "/repo") {
			t.Fatalf("%s: the diagnostic names a path: %v", name, err)
		}
	}
	// The bound: a chain of 256 directories passes, 257 is exhausted.
	for depth, ok := range map[int]bool{255: true, 256: false} {
		deep := "/" + strings.TrimSuffix(strings.Repeat("d/", depth), "/")
		if err := checkGrokPlacement(newMemPlacement(deep), deep, false, nil); (err == nil) != ok || !ok && !strings.Contains(err.Error(), "passed 256 directories") {
			t.Fatalf("depth %d: %v", depth+1, err)
		}
	}
	// The environment: presence of either key, even empty or overridden to
	// empty, rejects at both check points; only absence passes.
	for name, env := range map[string][]string{
		"inherited":     {"PATH=/bin", "GIT_DIR=/repo/.git"},
		"plan-only":     {"PATH=/bin", "GIT_WORK_TREE=/repo"},
		"duplicate":     {"GIT_DIR=/a", "GIT_DIR=/b"},
		"empty":         {"GIT_DIR="},
		"to-empty":      {"GIT_WORK_TREE=/repo", "GIT_WORK_TREE="},
		"no-equal-sign": {"GIT_DIR"},
	} {
		for _, start := range []struct {
			dir string
			ws  bool
		}{{out, false}, {ws, true}} {
			if err := checkGrokPlacement(clean(), start.dir, start.ws, env); err == nil || !strings.Contains(err.Error(), "present in the child environment") {
				t.Fatalf("%s at %s: %v", name, start.dir, err)
			}
		}
	}
	if err := checkGrokPlacement(clean(), out, false, []string{"GIT_DIRX=1", "XGIT_DIR=1", "git_dir=1", "GIT_AUTHOR_NAME=x"}); err != nil {
		t.Fatalf("absent keys: %v", err)
	}
}

// UT-12: a nil placement dependency is the operating system's view, shown
// on fixture-local entries only (never relying on the host's ancestors).
func TestGrokPlacementOS(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o700)
	os.MkdirAll(filepath.Join(dir, "real", "sub"), 0o700)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "alias"))
	view := placementFS(nil)
	if _, ok := view.(osPlacementFS); !ok {
		t.Fatalf("nil selects %T", view)
	}
	got, err1 := view.Lstat(filepath.Join(dir, ".git"))
	want, err2 := os.Lstat(filepath.Join(dir, ".git"))
	if err1 != nil || err2 != nil || !os.SameFile(got, want) {
		t.Fatalf("lstat %v %v", err1, err2)
	}
	if info, err := view.Lstat(filepath.Join(dir, "alias")); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("lstat follows a link: %v", err)
	}
	r1, _ := view.EvalSymlinks(filepath.Join(dir, "alias", "sub"))
	r2, _ := filepath.EvalSymlinks(filepath.Join(dir, "alias", "sub"))
	if r1 != r2 || r1 == "" {
		t.Fatalf("resolution %q %q", r1, r2)
	}
	// The real gate rejects the fixture-local marker whatever lies above.
	if err := checkGrokPlacement(nil, filepath.Join(dir, "alias", "sub"), false, nil); err == nil || !strings.Contains(err.Error(), ".git entry 2 levels up") {
		t.Fatalf("os gate: %v", err)
	}
}

// spyPlacement records every lookup the gate makes.
type spyPlacement struct {
	GrokPlacementFS
	calls *int
}

func (s spyPlacement) Lstat(p string) (os.FileInfo, error) {
	*s.calls++
	return s.GrokPlacementFS.Lstat(p)
}
func (s spyPlacement) EvalSymlinks(p string) (string, error) {
	*s.calls++
	return s.GrokPlacementFS.EvalSymlinks(p)
}

// UT-12: the gate in capture. A Git key in the effective child
// environment (inherited, plan-only, overlaid, empty) refuses the trusted
// Grok plan before any launch; a .git appearing between the checks stops
// it before the model session; legacy Grok plans and other clients never
// consult the gate.
func TestGrokPlacementCapture(t *testing.T) {
	trustedPlan := func(env map[string]string) *Plan {
		return mustCapturePlan(t, filledPlan(t, func(c map[string]any) {
			e := map[string]any{}
			for k, v := range env {
				e[k] = v
			}
			c["env"] = e
		}, "grok"))
	}
	for name, tc := range map[string]struct {
		base []string
		plan map[string]string
	}{
		"inherited":  {[]string{"GIT_DIR=/repo/.git"}, nil},
		"plan-only":  {nil, map[string]string{"GIT_WORK_TREE": "/repo"}},
		"overlaid":   {[]string{"GIT_DIR=/a"}, map[string]string{"GIT_DIR": "/b"}},
		"empty":      {[]string{"GIT_WORK_TREE="}, nil},
		"to-empty":   {[]string{"GIT_DIR=/a"}, map[string]string{"GIT_DIR": ""}},
		"plan-empty": {nil, map[string]string{"GIT_DIR": ""}},
	} {
		w := newCapWorld(t)
		man, _, _ := b1Capture(t, w, trustedPlan(tc.plan), func(c *CaptureRunner) { c.BaseEnv = append(c.BaseEnv, tc.base...) })
		cc := capClient(t, man, "grok")
		if !strings.HasPrefix(reasonOf(cc), ReasonGrokPlacement+": GIT_") || len(w.kinds()) != 0 || man.ExitCode() != 5 ||
			cc.Version.State != StageNotRun || cc.Help.State != StageNotRun || cc.Session.State != StageNotRun {
			t.Fatalf("%s: %q %v", name, reasonOf(cc), w.kinds())
		}
	}
	// The recheck: a .git planted in the workspace's parent after the first
	// check (here by the help command) stops the session; version and help
	// ran, the session was prepared and never launched, and the next client
	// still runs.
	w := newCapWorld(t)
	var out string
	w.script = func(spec ProcSpec) capBehavior {
		if clientOf(spec) == "grok" && launchKind(spec) == "help" {
			os.MkdirAll(filepath.Join(out, workDir, ".git"), 0o700)
		}
		return w.defaults(spec)
	}
	man, _, _ := b1Capture(t, w, capPlan(t, "grok", "claude"), func(c *CaptureRunner) { out = c.OutDir })
	if cc := capClient(t, man, "grok"); !strings.HasPrefix(reasonOf(cc), ReasonGrokPlacement+": lexical chain: a .git entry 1 levels up") ||
		cc.Session.State != StagePrepared || !slices.Equal(w.kinds(), []string{"grok:version", "grok:help", "claude:version", "claude:help", "claude:session"}) ||
		capClient(t, man, "claude").State != CaptureComplete {
		t.Fatalf("recheck: %q %v", reasonOf(cc), w.kinds())
	}
	// Legacy Grok (no --trust) and the other clients never consult the
	// gate, even with a Git key present.
	legacy := mustCapturePlan(t, filledPlan(t, func(c map[string]any) {
		c["session"] = map[string]any{"argv": []any{"-p", "{prompt}", "--output-format", "json", "-m", "fake-model-1"}}
	}, "grok", "claude", "codex", "cursor"))
	calls := 0
	w = newCapWorld(t)
	man, _, _ = b1Capture(t, w, legacy, func(c *CaptureRunner) {
		c.BaseEnv = append(c.BaseEnv, "GIT_DIR=/repo/.git")
		c.GrokPlacementFS = spyPlacement{c.GrokPlacementFS, &calls}
	})
	if calls != 0 || man.State != CaptureComplete {
		t.Fatalf("legacy and other clients: %d lookups, %s", calls, man.State)
	}
	// The trusted plan's session environment is BaseEnv plus the plan's
	// variables only: no HOME or GROK_HOME of the harness's own.
	w = newCapWorld(t)
	man, _, c := b1Capture(t, w, capPlan(t, "grok"), nil)
	s := w.sessions()[0]
	if !slices.Equal(s.Env, metadataEnv(c.BaseEnv, &c.Plan.Clients[0])) || slices.ContainsFunc(s.Env, func(kv string) bool {
		return strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "GROK_HOME=")
	}) || capClient(t, man, "grok").State != CaptureComplete || !slices.Equal(w.kinds(), []string{"grok:version", "grok:help", "grok:session"}) {
		t.Fatalf("grok environment %q %v", s.Env, w.kinds())
	}
	if cc := capClient(t, man, "grok"); !slices.Contains(cc.Argv, PromptForClient("grok", "grok-capture-setup")) || !strings.Contains(PromptForClient("grok", "x"), "tool probe__slow exactly once") ||
		*cc.Config.Prerequisite != "Trust only the generated workspace; use normal Grok login; project probe config, probe__slow and streaming-json; no timeout override." {
		t.Fatalf("grok provenance %q", cc.Argv)
	}
}

// UT-12: the prompt names Grok's qualified tool from a parameter, never by
// rewriting text: a case ID that contains "slow" is kept exactly.
func TestPromptForClient(t *testing.T) {
	for _, id := range []string{"claude", "codex", "cursor"} {
		if PromptForClient(id, "slow tool slow") != Prompt("slow tool slow") {
			t.Fatalf("%s prompt changed", id)
		}
	}
	got := PromptForClient("grok", "the tool slow")
	if got != strings.Replace(Prompt("the tool slow"), "MCP tool slow", "MCP tool probe__slow", 1) || !strings.Contains(got, `{"case_id": "the tool slow"}`) {
		t.Fatalf("grok prompt %q", got)
	}
}

// UT-12: Grok's streaming-json lines go through the unchanged per-line
// capture policy: structured rawInput/rawOutput secrets and secrets inside
// embedded JSON strings are redacted by decoded content, numeric usage and
// boolean fields are kept, offsets stay in order, the result is a fixed
// point; a cut escaped credential line is omitted and the capture is
// partial and not enrollable.
func TestGrokStreamRedaction(t *testing.T) {
	const secret = "grok-planted-secret-value"
	lines := []string{
		`{"type":"tool_call","toolCallId":"call_1","toolName":"probe__slow","kind":"other","status":"in_progress","rawInput":{"case_id":"grok-capture-setup","api_key":"` + secret + `"},"content":[],"locations":[]}`,
		`{"type":"tool_call_update","toolCallId":"call_1","status":"completed","rawOutput":{"content":[{"type":"text","text":"{\"nonce\":\"noncecap1\",\"token\":\"` + secret + `\"}"}]},"content":[],"locations":[]}`,
		`{"type":"text","data":"Bearer ` + secret + `"}`,
		`{"type":"usage","messageId":"resp_1","stopReason":"end_turn","usage":{"input_tokens":812,"output_tokens":45,"cache_read_input_tokens":0},"token_present":true,"signature":"sig-1"}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"s-1","num_turns":2}`,
	}
	w := newCapWorld(t)
	w.session(func(ProcSpec) capBehavior {
		return capBehavior{stdout: strings.Join(lines, "\n") + "\n", events: goodEvents}
	})
	man, b, _ := b1Capture(t, w, capPlan(t, "grok"), nil)
	vendor := b.Files[ClientFile("grok", FileVendorEvents)]
	tr, err := ReplayTranscript(vendor)
	if err != nil || len(tr.Lines) != 5 || capClient(t, man, "grok").State != CaptureComplete {
		t.Fatalf("grok stream: %v %d %q", err, len(tr.Lines), reasonOf(capClient(t, man, "grok")))
	}
	var decoded strings.Builder
	for i, l := range tr.Lines {
		if i > 0 && l.OffsetNS < tr.Lines[i-1].OffsetNS {
			t.Fatal("offsets out of order")
		}
		decoded.Write(l.Data)
	}
	d := decoded.String()
	for _, want := range []string{`"input_tokens":812`, `"output_tokens":45`, `"token_present":true`, `"cache_read_input_tokens":0`, `"api_key":"[REDACTED]"`, `\"token\":\"[REDACTED]\"`,
		`Bearer [REDACTED]`, `"toolName":"probe__slow"`, `\"nonce\":\"noncecap1\"`, `"signature":"sig-1"`} {
		if !strings.Contains(d, want) {
			t.Fatalf("decoded lines lack %s: %s", want, d)
		}
	}
	if strings.Contains(d, secret) || bytes.Contains(vendor, []byte(secret)) {
		t.Fatal("a secret survived")
	}
	if err := RedactionFixedPoint(ClientFile("grok", FileVendorEvents), vendor, NewCaptureRedactor(nil, nil)); err != nil {
		t.Fatal(err)
	}
	// A cut, escaped credential line is omitted whole.
	cut := `{"type":"tool_call_update","rawOutput":"{\"password\":\"` + secret
	w = newCapWorld(t)
	w.session(func(ProcSpec) capBehavior {
		return capBehavior{stdout: lines[0] + "\n" + cut + "\n" + lines[4] + "\n", events: goodEvents}
	})
	man, b, _ = b1Capture(t, w, capPlan(t, "grok"), nil)
	cc := capClient(t, man, "grok")
	if reasonOf(cc) != ReasonEvidenceOmitted || streamRecord(cc, FileVendorEvents).OmittedLines != 1 || bytes.Contains(b.Files[ClientFile("grok", FileVendorEvents)], []byte(secret)) {
		t.Fatalf("cut line: %q %+v", reasonOf(cc), cc.Streams)
	}
}

func streamRecord(cc CaptureClient, name string) CaptureStream {
	for _, s := range cc.Streams {
		if s.Path == ClientFile(cc.ID, name) {
			return s
		}
	}
	return CaptureStream{OmittedLines: -1}
}

// UT-12: the Grok session's own group is reaped when its launcher exits
// first, and only it: a separately owned, pre-existing leader service
// (never launched by capture) is never signalled or reaped.
func TestGrokLeaderUntouched(t *testing.T) {
	w := newCapWorld(t)
	leader := &capProc{pgid: 9999, exited: make(chan struct{})}
	var reaped []int
	w.onReap = func(spec ProcSpec) {}
	w.leaderFirst = func(spec ProcSpec) bool { return launchKind(spec) == "session" }
	c := newCapRunner(t, w, capPlan(t, "grok"))
	inner := c.Reaper
	c.Reaper = reaperFunc(func(p Proc) CaseCleanup { reaped = append(reaped, p.PGID()); return inner.Reap(p) })
	man, _ := runCapture(t, c, context.Background())
	cc := capClient(t, man, "grok")
	select {
	case <-leader.exited:
		t.Fatal("the owner's leader was ended")
	default:
	}
	if cc.State != CaptureComplete || !cc.Session.Cleanup.LeaderExitedFirst || !cc.Session.Cleanup.KillSent || slices.Contains(reaped, leader.pgid) || len(reaped) != 3 {
		t.Fatalf("leader first: %+v reaped %v", cc.Session.Cleanup, reaped)
	}
}

type reaperFunc func(Proc) CaseCleanup

func (f reaperFunc) Reap(p Proc) CaseCleanup { return f(p) }

// memFS is a tiny injected approval filesystem: entries by absolute path
// (directories implicit in their children too), with injectable lookup,
// listing and open failures, and racy files whose size changes while read.
type memFS struct {
	nodes map[string]*memNode
	links map[string]string
}

type memNode struct {
	mode                         fs.FileMode
	data                         string
	lstatErr, openErr, listError error
	racy                         bool
}

func newMemFS() *memFS { return &memFS{nodes: map[string]*memNode{}, links: map[string]string{}} }

func (m *memFS) mkdirs(p string) {
	for d := p; ; d = filepath.Dir(d) {
		if _, ok := m.nodes[d]; !ok {
			m.nodes[d] = &memNode{mode: fs.ModeDir | 0o700}
		}
		if d == filepath.Dir(d) {
			return
		}
	}
}

func (m *memFS) file(p, data string) *memNode {
	m.mkdirs(filepath.Dir(p))
	n := &memNode{mode: 0o600, data: data}
	m.nodes[p] = n
	return n
}

func (m *memFS) clone() *memFS {
	c := newMemFS()
	for k, v := range m.nodes {
		n := *v
		c.nodes[k] = &n
	}
	for k, v := range m.links {
		c.links[k] = v
	}
	return c
}

func (m *memFS) info(p string, n *memNode) memInfo {
	return memInfo{name: filepath.Base(p), mode: n.mode, size: int64(len(n.data)), mtime: epoch}
}

func (m *memFS) Lstat(p string) (fs.FileInfo, error) {
	n, ok := m.nodes[p]
	switch {
	case !ok:
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
	case n.lstatErr != nil:
		return nil, n.lstatErr
	}
	return m.info(p, n), nil
}

func (m *memFS) ReadDir(p string) ([]fs.DirEntry, error) {
	if n, ok := m.nodes[p]; ok && n.listError != nil {
		return nil, n.listError
	}
	var out []fs.DirEntry
	for k, n := range m.nodes {
		if filepath.Dir(k) == p && k != p {
			out = append(out, fs.FileInfoToDirEntry(m.info(k, n)))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() > out[j].Name() })
	return out, nil
}

func (m *memFS) Open(p string) (approvalFile, error) {
	n, ok := m.nodes[p]
	switch {
	case !ok:
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	case n.openErr != nil:
		return nil, n.openErr
	}
	return &memFile{Reader: strings.NewReader(n.data), info: m.info(p, n), racy: n.racy}, nil
}

func (m *memFS) EvalSymlinks(p string) (string, error) {
	if to, ok := m.links[p]; ok {
		return to, nil
	}
	return p, nil
}

type memFile struct {
	io.Reader
	info  memInfo
	racy  bool
	stats int
}

func (f *memFile) Stat() (fs.FileInfo, error) {
	f.stats++
	if f.racy && f.stats > 1 {
		i := f.info
		i.size++
		return i, nil
	}
	return f.info, nil
}

func (f *memFile) Close() error { return nil }

// UT-13: the bounded, keyed, no-follow inventories on injected tiny trees:
// complete, absent, created, changed and deleted roots and files, their
// kinds and labels; a symbolic link, special, unreadable or racy file, a
// lookup or listing failure and each bound at N+1 (but not N) fail closed;
// an incomplete post-inventory never reports removals it did not see;
// fingerprints are keyed per scanner and never part of a change.
func TestApprovalScanner(t *testing.T) {
	const ws, home = "/c/out/.work/cursor-capture-setup", "/home/owner"
	roots := []scanRoot{{label: labelWorkspace, path: ws}, {label: labelHomeCursor, path: home + "/.cursor"}, {label: "<ancestor-1>/.cursor", path: "/c/out/.work/.cursor"}}
	base := newMemFS()
	base.file(ws+"/case.json", `{"x":1}`)
	base.file(ws+"/.cursor/mcp.json", `{"mcpServers":{}}`)
	base.file(home+"/.cursor/cli-config.json", `{"token":"owner-secret-1"}`)
	sc := func(lim ApprovalScanLimits) *approvalScanner {
		s, err := newApprovalScanner(nil, lim)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	scan := func(m *memFS, lim ApprovalScanLimits) *snapshot {
		s := sc(lim)
		s.fs = m
		return s.snapshot(roots)
	}
	pre := scan(base, ApprovalScanLimits{})
	// ws, its .cursor, two files; home .cursor and its file; the ancestor
	// root is absent.
	if !pre.complete || pre.count != 6 || len(pre.entries) != 6 || pre.bytes != int64(len(`{"x":1}`)+len(`{"mcpServers":{}}`)+len(`{"token":"owner-secret-1"}`)) {
		t.Fatalf("pre %+v", pre)
	}
	after := base.clone()
	after.file(ws+"/.cursor/approved.json", `["probe"]`)
	after.file(ws+"/.cursor/mcp.json", `{"mcpServers":{"p":1}}`)
	after.nodes[home+"/.cursor/cli-config.json"].mode = 0o644
	delete(after.nodes, ws+"/case.json")
	after.file("/c/out/.work/.cursor/new", "x")
	s := sc(ApprovalScanLimits{})
	s.fs = base
	p1 := s.snapshot(roots)
	s.fs = after
	got := diffSnapshots(p1, s.snapshot(roots))
	want := []approvalChange{
		{CaptureApprovalChange{"<ancestor-1>/.cursor", ChangeAdded}, false}, {CaptureApprovalChange{"<ancestor-1>/.cursor/new", ChangeAdded}, true},
		{CaptureApprovalChange{"<home>/.cursor/cli-config.json", ChangeModified}, true},
		{CaptureApprovalChange{"<workspace>/.cursor/approved.json", ChangeAdded}, true}, {CaptureApprovalChange{"<workspace>/.cursor/mcp.json", ChangeModified}, true},
		{CaptureApprovalChange{"<workspace>/case.json", ChangeRemoved}, true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff %+v", got)
	}
	// Same size, different bytes: a modification by digest alone; a type
	// change is a modification of a non-regular entry; a deleted root.
	same := base.clone()
	same.file(ws+"/case.json", `{"y":1}`)
	same.nodes[ws+"/.cursor/mcp.json"].mode = fs.ModeDir | 0o700
	delete(same.nodes, home+"/.cursor")
	delete(same.nodes, home+"/.cursor/cli-config.json")
	s.fs = same
	got = diffSnapshots(p1, s.snapshot(roots))
	want = []approvalChange{{CaptureApprovalChange{"<home>/.cursor", ChangeRemoved}, false}, {CaptureApprovalChange{"<home>/.cursor/cli-config.json", ChangeRemoved}, true},
		{CaptureApprovalChange{"<workspace>/.cursor/mcp.json", ChangeModified}, false}, {CaptureApprovalChange{"<workspace>/case.json", ChangeModified}, true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff %+v", got)
	}
	// Keys are per scanner: the same bytes get different fingerprints, and
	// a change carries only its path and kind.
	a, b := scan(base, ApprovalScanLimits{}), scan(base, ApprovalScanLimits{})
	if a.entries[ws+"/case.json"].digest == b.entries["<workspace>/case.json"].digest || a.entries["<workspace>/case.json"].digest == b.entries["<workspace>/case.json"].digest {
		t.Fatal("the fingerprints are not keyed per scanner")
	}
	if fields := reflect.TypeOf(CaptureApprovalChange{}).NumField(); fields != 2 {
		t.Fatalf("a change exports %d fields", fields)
	}
	// Every unsafe observation fails closed.
	for name, mutate := range map[string]func(m *memFS){
		"symlink":     func(m *memFS) { m.nodes[ws+"/.cursor/link"] = &memNode{mode: fs.ModeSymlink | 0o777} },
		"special":     func(m *memFS) { m.nodes[home+"/.cursor/fifo"] = &memNode{mode: fs.ModeNamedPipe | 0o600} },
		"unreadable":  func(m *memFS) { m.nodes[ws+"/case.json"].openErr = syscall.EACCES },
		"racy":        func(m *memFS) { m.nodes[ws+"/case.json"].racy = true },
		"lookup":      func(m *memFS) { m.nodes[ws+"/.cursor"].lstatErr = syscall.EIO },
		"root-lookup": func(m *memFS) { m.nodes[home+"/.cursor"].lstatErr = syscall.EACCES },
		"listing":     func(m *memFS) { m.nodes[home+"/.cursor"].listError = syscall.EACCES },
		"root-link":   func(m *memFS) { m.nodes[home+"/.cursor"].mode = fs.ModeSymlink | 0o777 },
	} {
		m := base.clone()
		mutate(m)
		if snap := scan(m, ApprovalScanLimits{}); snap.complete || snap.why == "" {
			t.Fatalf("%s: complete", name)
		}
	}
	// An incomplete post-inventory reports what it saw, never removals.
	m := base.clone()
	m.nodes[home+"/.cursor"].listError = syscall.EACCES
	delete(m.nodes, ws+"/case.json")
	s.fs = m
	post := s.snapshot(roots)
	for _, ch := range diffSnapshots(p1, post) {
		if ch.Kind == ChangeRemoved {
			t.Fatalf("an unobserved removal %+v", ch)
		}
	}
	// The bounds at N pass and at N+1 fail: six entries, 50 bytes, 26 per
	// file.
	for name, tc := range map[string]struct {
		ok, over ApprovalScanLimits
	}{
		"entries":    {ApprovalScanLimits{Entries: 6}, ApprovalScanLimits{Entries: 5}},
		"bytes":      {ApprovalScanLimits{Bytes: pre.bytes}, ApprovalScanLimits{Bytes: pre.bytes - 1}},
		"file-bytes": {ApprovalScanLimits{FileBytes: int64(len(`{"token":"owner-secret-1"}`))}, ApprovalScanLimits{FileBytes: int64(len(`{"token":"owner-secret-1"}`)) - 1}},
	} {
		if !scan(base, tc.ok).complete || scan(base, tc.over).complete {
			t.Fatalf("%s bound", name)
		}
	}
	if l := (ApprovalScanLimits{Entries: 1 << 30, Bytes: -1}).orDefault(); l != DefaultApprovalScanLimits() {
		t.Fatalf("limits %+v", l)
	}
	// The roots: workspace, the owner's .cursor and every ancestor's, on
	// the lexical and the resolved chain, deduplicated (the owner's home
	// is an ancestor here).
	rm := newMemFS()
	rm.links["/u/owner/w/ws"] = "/real/ws"
	rs, err := approvalRoots(rm, "/u/owner/w/ws", "/u/owner")
	var labels []string
	for _, r := range rs {
		labels = append(labels, r.label+"="+r.path)
	}
	if err != nil || !slices.Equal(labels, []string{"<workspace>=/u/owner/w/ws", "<home>/.cursor=/u/owner/.cursor", "<ancestor-1>/.cursor=/u/owner/w/.cursor",
		"<ancestor-3>/.cursor=/u/.cursor", "<ancestor-4>/.cursor=/.cursor", "<ancestor-1>/.cursor=/real/.cursor"}) {
		t.Fatalf("roots %v %v", labels, err)
	}
	for name, home := range map[string]string{"empty": "", "relative": "home"} {
		if _, err := approvalRoots(rm, "/u/owner/w/ws", home); err == nil {
			t.Fatalf("%s home accepted", name)
		}
	}
	if _, err := approvalRoots(&memPlacementApproval{}, "/x", "/h"); err == nil {
		t.Fatal("an unresolved workspace accepted")
	}
	if _, err := approvalRoots(nil, t.TempDir(), "/h"); err != nil {
		t.Fatalf("os roots: %v", err)
	}
	// A nil filesystem is the operating system's, on fixture-local roots: a
	// no-follow open refuses a link swapped in for a looked-up file.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ws", ".cursor"), 0o700)
	os.WriteFile(filepath.Join(dir, "ws", ".cursor", "a.json"), []byte("1"), 0o600)
	osScan, _ := newApprovalScanner(nil, ApprovalScanLimits{})
	snap := osScan.snapshot([]scanRoot{{label: labelWorkspace, path: filepath.Join(dir, "ws")}, {label: labelHomeCursor, path: filepath.Join(dir, "absent")}})
	if !snap.complete || snap.count != 3 || snap.entries["<workspace>/.cursor/a.json"].size != 1 {
		t.Fatalf("os snapshot %+v", snap)
	}
	os.Symlink(filepath.Join(dir, "ws", ".cursor", "a.json"), filepath.Join(dir, "link"))
	if _, err := (osApprovalFS{}).Open(filepath.Join(dir, "link")); err == nil {
		t.Fatal("the no-follow open followed a link")
	}
	if _, why := osScan.digest(hmacFor(osScan), filepath.Join(dir, "link"), mustLstat(t, filepath.Join(dir, "ws", ".cursor", "a.json"))); why == "" {
		t.Fatal("a swapped-in link was hashed")
	}
}

func hmacFor(sc *approvalScanner) hash.Hash { return hmac.New(sha256.New, sc.key) }

func mustLstat(t *testing.T, p string) fs.FileInfo {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// UT-13: when the workspace's resolved chain is longer than its lexical
// one, two distinct ancestor roots share a label: their inventories never
// merge (each has its own key), and a change list that cannot tell them
// apart is recorded once, incomplete, and still blocks as outside.
func TestCursorApprovalSharedLabels(t *testing.T) {
	if recordedPath("<ancestor-4>/.cursor"+rootKeySep+"7/x/y") != "<ancestor-4>/.cursor/x/y" || recordedPath("<ancestor-4>/.cursor"+rootKeySep+"7") != "<ancestor-4>/.cursor" ||
		recordedPath("<workspace>/a") != "<workspace>/a" {
		t.Fatal("recorded paths")
	}
	base := t.TempDir()
	deep := filepath.Join(base, "deep", "a", "b")
	os.MkdirAll(deep, 0o700)
	os.Symlink(deep, filepath.Join(base, "link"))
	w := newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "enable" {
			return capBehavior{writes: map[string]*string{filepath.Join(base, ".cursor", "x"): strp("1"), filepath.Join(base, "deep", "a", ".cursor", "x"): strp("1")}}
		}
		return w.defaults(spec)
	}
	man, _, _ := b1Capture(t, w, capPlan(t, "cursor"), func(c *CaptureRunner) { c.OutDir, c.Home = filepath.Join(base, "link", "out"), t.TempDir() })
	cc := capClient(t, man, "cursor")
	a := cc.Approval
	if !strings.HasPrefix(reasonOf(cc), ReasonCursorOutside) || a.Scope != ScopeOutside || a.InventoryComplete ||
		!slices.Equal(a.Changes, []CaptureApprovalChange{{"<ancestor-4>/.cursor", ChangeAdded}, {"<ancestor-4>/.cursor/x", ChangeAdded}}) {
		t.Fatalf("shared labels: %q %+v", reasonOf(cc), a)
	}
	ws := filepath.Join(base, "link", "out", workDir, "ws")
	if _, err := approvalRoots(nil, ws, "/h"); err == nil {
		t.Fatal("an unresolvable workspace was accepted")
	}
	os.MkdirAll(ws, 0o700)
	roots, err := approvalRoots(nil, ws, "/h")
	keys, disambiguated := map[string]bool{}, 0
	for _, r := range roots {
		k := r.label
		if r.key != "" {
			k, disambiguated = r.key, disambiguated+1
		}
		if keys[k] {
			t.Fatalf("duplicate key %q", k)
		}
		keys[k] = true
	}
	if err != nil || disambiguated == 0 {
		t.Fatalf("roots %v %+v", err, roots)
	}
}

// memPlacementApproval resolves nothing.
type memPlacementApproval struct{ memFS }

func (*memPlacementApproval) EvalSymlinks(p string) (string, error) {
	return "", &fs.PathError{Op: "evalsymlinks", Path: p, Err: fs.ErrNotExist}
}

// cursorRun is one capture of the trusted Cursor recipe on the capture
// world with enable scripted by enable (nil: the default workspace
// approval) and the owner's home inside the test's temporary directory.
func cursorRun(t *testing.T, enable func(home string, spec ProcSpec) capBehavior, mutate func(c *CaptureRunner), ids ...string) (*capWorld, *CaptureManifest, *CaptureBundle, *CaptureRunner) {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"cursor"}
	}
	w := newCapWorld(t)
	home := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o700)
	os.WriteFile(filepath.Join(home, ".cursor", "cli-config.json"), []byte(`{"token":"owner-secret-1"}`), 0o600)
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "enable" && enable != nil {
			return enable(home, spec)
		}
		return w.defaults(spec)
	}
	man, b, c := b1Capture(t, w, capPlan(t, ids...), func(c *CaptureRunner) {
		c.Home = home
		if mutate != nil {
			mutate(c)
		}
	})
	return w, man, b, c
}

// UT-13: the approval stage in capture. A workspace approval lets the one
// session start after it, with exact provenance; every other observation
// (an owner or ancestor change, no change, a non-file or non-.cursor
// change, a failed, timed-out or interrupted enable, an incomplete
// inventory before or after, a cleanup failure) stops before the model
// session, without restoring anything or retrying; a failed pre-scan
// launches no enable at all.
func TestCursorApprovalCapture(t *testing.T) {
	w, man, b, c := cursorRun(t, nil, nil)
	cc := capClient(t, man, "cursor")
	a := cc.Approval
	if cc.State != CaptureComplete || a == nil || a.Scope != ScopeWorkspaceOnly || a.Reason != nil || !a.InventoryComplete || a.Stage.State != StageRan ||
		a.Stage.WatchdogMS != 30000 || a.Cwd != "<workspace>" || !slices.Equal(a.Argv, []string{"mcp", "enable", "probe"}) ||
		!slices.Equal(a.Changes, []CaptureApprovalChange{{"<workspace>/" + approvedFile, ChangeAdded}}) ||
		!slices.Equal(w.kinds(), []string{"cursor:version", "cursor:help", "cursor:enable", "cursor:session"}) {
		t.Fatalf("scoped approval: %q %+v %v", reasonOf(cc), a, w.kinds())
	}
	if string(b.Files[ClientFile("cursor", FileApprovalStdout)]) != "probe enabled\n" || len(b.Manifest.Files) != 11 ||
		*cc.Config.Prerequisite != "Trust only the generated workspace; capture runs mcp enable probe there and verifies workspace-only file changes before the model session; never --approve-mcps." {
		t.Fatalf("approval evidence %v", b.Manifest.Files)
	}
	// The manifest carries no fingerprint, digest or owner file content.
	if bytes.Contains(b.ManifestBytes, []byte("owner-secret-1")) || bytes.Contains(b.ManifestBytes, []byte("cli-config")) || bytes.Contains(b.ManifestBytes, []byte(c.Home)) {
		t.Fatal("the manifest exports owner data")
	}
	var raw map[string]any
	json.Unmarshal(b.ManifestBytes, &raw)
	approval := raw["clients"].([]any)[0].(map[string]any)["approval"].(map[string]any)
	keys := []string{}
	for k := range approval {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Design decoder-enrollment B1.5 (FP-16): every new record names its
	// inventory policy and excluded paths; a workspace-only record has no
	// project object.
	if !slices.Equal(keys, []string{"argv", "changes", "cwd", "excluded_paths", "inventory_complete", "inventory_policy", "reason", "scope", "stage"}) {
		t.Fatalf("approval fields %v", keys)
	}
	// config.txt is the configuration re-read after the enable (here
	// rewritten by it), labeled.
	_, _, b, _ = cursorRun(t, func(_ string, spec ProcSpec) capBehavior {
		return capBehavior{writes: map[string]*string{".cursor/mcp.json": strp(`{"mcpServers":{"probe":{"command":"/opt/mcpqual","approved":true,"cwd":"` + spec.Dir + `"}}}`)}}
	}, nil)
	if cfg := string(b.Files[ClientFile("cursor", FileConfig)]); cfg != `{"mcpServers":{"probe":{"command":"<server>","approved":true,"cwd":"<workspace>"}}}` {
		t.Fatalf("re-read config %q", cfg)
	}
	type outcome struct {
		enable   func(home string, spec ProcSpec) capBehavior
		mutate   func(c *CaptureRunner)
		reason   string
		scope    string
		launched []string
		changes  []CaptureApprovalChange
	}
	stopped := []string{"cursor:version", "cursor:help", "cursor:enable"}
	for name, tc := range map[string]outcome{
		"owner-change": {func(home string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(home, ".cursor", "approved.json"): strp("probe"), approvedFile: strp("probe")}}
		}, nil, ReasonCursorOutside, ScopeOutside, stopped,
			[]CaptureApprovalChange{{"<home>/.cursor/approved.json", ChangeAdded}, {"<workspace>/" + approvedFile, ChangeAdded}}},
		"ancestor-change": {func(_ string, spec ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(filepath.Dir(spec.Dir), ".cursor", "x"): strp("probe")}}
		}, nil, ReasonCursorOutside, ScopeOutside, stopped,
			[]CaptureApprovalChange{{"<ancestor-1>/.cursor", ChangeAdded}, {"<ancestor-1>/.cursor/x", ChangeAdded}}},
		"owner-delete": {func(home string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(home, ".cursor", "cli-config.json"): nil}}
		}, nil, ReasonCursorOutside, ScopeOutside, stopped, []CaptureApprovalChange{{"<home>/.cursor/cli-config.json", ChangeRemoved}}},
		"no-op": {func(string, ProcSpec) capBehavior { return capBehavior{} }, nil, ReasonCursorScopeUnverified + ": no change", ScopeUnverifiable, stopped,
			[]CaptureApprovalChange{}},
		"directory": {func(string, ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{".cursor/state/x.json": strp("1")}}
		}, nil, ReasonCursorScopeUnverified + ": a change is not a regular file", ScopeUnverifiable, stopped,
			[]CaptureApprovalChange{{"<workspace>/.cursor/state", ChangeAdded}, {"<workspace>/.cursor/state/x.json", ChangeAdded}}},
		"outside-dot-cursor": {func(string, ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{"approved.json": strp("1")}}
		}, nil, ReasonCursorScopeUnverified + ": a change is not a regular file", ScopeUnverifiable, stopped,
			[]CaptureApprovalChange{{"<workspace>/approved.json", ChangeAdded}}},
		"enable-failed": {func(string, ProcSpec) capBehavior {
			return capBehavior{stderr: "error: unknown command enable\n", exit: 1, writes: map[string]*string{approvedFile: strp("1")}}
		}, nil, ReasonCursorApprovalFailed, ScopeUnverifiable, stopped, []CaptureApprovalChange{{"<workspace>/" + approvedFile, ChangeAdded}}},
		"post-symlink": {func(_ string, spec ProcSpec) capBehavior {
			os.MkdirAll(filepath.Join(spec.Dir, ".cursor"), 0o700)
			os.Symlink("/etc/passwd", filepath.Join(spec.Dir, ".cursor", "link"))
			return capBehavior{}
		}, nil, ReasonCursorScopeUnverified + ": the inventory after the command is incomplete (a symbolic link", ScopeUnverifiable, stopped,
			[]CaptureApprovalChange{{"<workspace>/.cursor/link", ChangeAdded}}},
		"post-bound": {nil, func(c *CaptureRunner) { c.ApprovalLimits = ApprovalScanLimits{Entries: 6} },
			// The cut post-inventory keeps the difference it saw.
			ReasonCursorScopeUnverified + ": the inventory after the command is incomplete (the entry bound", ScopeUnverifiable, stopped,
			[]CaptureApprovalChange{{"<workspace>/" + approvedFile, ChangeAdded}}},
		"pre-symlink": {nil, func(c *CaptureRunner) { os.Symlink("/etc", filepath.Join(c.Home, ".cursor", "etc")) },
			ReasonCursorScopeUnverified + ": the inventory before the command is incomplete (a symbolic link", ScopeUnverifiable, []string{"cursor:version", "cursor:help"},
			[]CaptureApprovalChange{}},
		"pre-bound": {nil, func(c *CaptureRunner) { c.ApprovalLimits = ApprovalScanLimits{Entries: 5} },
			ReasonCursorScopeUnverified + ": the inventory before the command is incomplete (the entry bound", ScopeUnverifiable, []string{"cursor:version", "cursor:help"},
			[]CaptureApprovalChange{}},
		"pre-byte-bound": {nil, func(c *CaptureRunner) { c.ApprovalLimits = ApprovalScanLimits{FileBytes: 4} },
			ReasonCursorScopeUnverified + ": the inventory before the command is incomplete (the per-file byte bound", ScopeUnverifiable, []string{"cursor:version", "cursor:help"},
			[]CaptureApprovalChange{}},
		"no-home": {nil, func(c *CaptureRunner) { c.Home = "" }, ReasonCursorScopeUnverified + ": the home directory is unknown", ScopeUnverifiable,
			[]string{"cursor:version", "cursor:help"}, []CaptureApprovalChange{}},
		"enable-absent": {func(string, ProcSpec) capBehavior { return capBehavior{startErr: os.ErrNotExist} }, nil,
			ReasonCursorApprovalFailed + ": the enable command did not launch (absent_binary)", ScopeNotChecked, stopped, []CaptureApprovalChange{}},
	} {
		w, man, b, c := cursorRun(t, tc.enable, tc.mutate)
		cc := capClient(t, man, "cursor")
		a := cc.Approval
		if !strings.HasPrefix(reasonOf(cc), tc.reason) || a.Scope != tc.scope || *a.Reason != reasonOf(cc) || !slices.Equal(w.kinds(), tc.launched) ||
			!slices.Equal(a.Changes, tc.changes) || cc.Session.State != StagePrepared || man.ExitCode() != 5 {
			t.Fatalf("%s: %q %+v %v", name, reasonOf(cc), a, w.kinds())
		}
		_, launched := b.Files[ClientFile("cursor", FileApprovalStdout)]
		if launched != (a.Stage.State == StageRan) || b.Files[ClientFile("cursor", FileConfig)] == nil {
			t.Fatalf("%s: approval files %v", name, b.Manifest.Files)
		}
		// Nothing is restored: an outside write stays for the owner.
		if name == "owner-change" {
			if _, err := os.Stat(filepath.Join(c.Home, ".cursor", "approved.json")); err != nil {
				t.Fatal("the outside write was undone")
			}
		}
	}
	// The enable's watchdog is min(30 s, the client's remaining time); a
	// hanging enable is reaped and fails the approval.
	w = newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "enable" {
			return capBehavior{hang: true}
		}
		return w.defaults(spec)
	}
	// The metadata watchdogs are 20 s (the case limit) and the enable's is
	// the client's remaining 25 s, so the timer awaited after the enable's
	// own launch is unambiguously the enable's (code review B1 round 1, C3:
	// never a metadata timer a scheduler happened to park).
	p := mustCapturePlan(t, filledPlan(t, func(c map[string]any) {}, "cursor"))
	p.Limits = &Limits{MaxSessionsPerClient: 1, MaxCaseMS: 20000, MaxClientMS: 25000}
	cr := newCapRunner(t, w, p)
	cr.Home = t.TempDir()
	done := make(chan *CaptureManifest, 1)
	go func() { m, _ := cr.Run(context.Background()); done <- m }()
	w.awaitStart("enable")
	awaitTimer(t, w.clock, 25*time.Second)
	w.clock.Advance(25 * time.Second)
	man = <-done
	if cc := capClient(t, man, "cursor"); !strings.HasPrefix(reasonOf(cc), ReasonCursorApprovalFailed) || !cc.Approval.Stage.Watchdog || cc.Approval.Stage.WatchdogMS != 25000 ||
		cc.Version.Watchdog || cc.Help.Watchdog || len(w.sessions()) != 0 || !slices.Equal(w.kinds(), []string{"cursor:version", "cursor:help", "cursor:enable"}) {
		t.Fatalf("enable watchdog: %q %+v", reasonOf(cc), cc.Approval.Stage)
	}
	// An interrupted enable and a failed enable cleanup stop the run's
	// remaining clients.
	ctx, cancel := context.WithCancel(context.Background())
	w = newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "enable" {
			return capBehavior{hang: true}
		}
		return w.defaults(spec)
	}
	cr = newCapRunner(t, w, capPlan(t, "cursor", "claude"))
	cr.Home = t.TempDir()
	go func() { m, _ := cr.Run(ctx); done <- m }()
	w.awaitStart("enable")
	cancel()
	man = <-done
	if cc := capClient(t, man, "cursor"); reasonOf(cc) != ReasonInterrupted || man.State != CaptureInterrupted || capClient(t, man, "claude").State != CaptureNotRun ||
		cc.Approval.Reason == nil || len(w.sessions()) != 0 {
		t.Fatalf("interrupted enable: %q %s", reasonOf(cc), man.State)
	}
	w, man, _, _ = cursorRun(t, nil, func(c *CaptureRunner) {
		inner := c.Reaper
		c.Reaper = reaperFunc(func(p Proc) CaseCleanup {
			cc := inner.Reap(p)
			if p.PGID() == 3003 {
				return CaseCleanup{TermSent: true, KillSent: true, Error: sptr("cleanup: injected")}
			}
			return cc
		})
	}, "cursor", "claude")
	if cc := capClient(t, man, "cursor"); reasonOf(cc) != ReasonCleanupFailed || capClient(t, man, "claude").State != CaptureNotRun || man.Cleanup.OK || len(w.sessions()) != 0 {
		t.Fatalf("enable cleanup failure: %q %v", reasonOf(cc), w.kinds())
	}
	// A cut or omitted approval stream refuses completeness (the session
	// still runs); a later MCP refusal stays partial; a legacy Cursor plan
	// has no approval stage.
	for name, tc := range map[string]struct {
		enable  capBehavior
		session *capBehavior
		reason  string
	}{
		"stream-cut":  {capBehavior{stdout: strings.Repeat("x", 4096) + "\n", writes: map[string]*string{approvedFile: strp("1")}}, nil, ReasonEvidenceTruncated},
		"stream-omit": {capBehavior{stdout: "path C:\\x\n", writes: map[string]*string{approvedFile: strp("1")}}, nil, ReasonEvidenceOmitted},
		"mcp-denied":  {capBehavior{writes: map[string]*string{approvedFile: strp("1")}}, &capBehavior{stdout: capTranscript, events: probeWith(exitEv())}, ReasonProbeNotObserved},
	} {
		w := newCapWorld(t)
		w.script = func(spec ProcSpec) capBehavior {
			switch {
			case launchKind(spec) == "enable":
				return tc.enable
			case launchKind(spec) == "session" && tc.session != nil:
				return *tc.session
			}
			return w.defaults(spec)
		}
		man, _, _ := b1Capture(t, w, capPlan(t, "cursor"), func(c *CaptureRunner) { c.Home = t.TempDir(); c.Limits = CaptureLimits{Stream: 1024} })
		if cc := capClient(t, man, "cursor"); reasonOf(cc) != tc.reason || cc.Approval.Scope != ScopeWorkspaceOnly || len(w.sessions()) != 1 {
			t.Fatalf("%s: %q %v", name, reasonOf(cc), w.kinds())
		}
	}
	legacy := mustCapturePlan(t, filledPlan(t, func(c map[string]any) {
		c["session"] = map[string]any{"argv": []any{"-p", "{prompt}", "--output-format", "stream-json", "--model", "fake-model-1"}}
	}, "cursor"))
	w = newCapWorld(t)
	man, _, _ = b1Capture(t, w, legacy, nil)
	if cc := capClient(t, man, "cursor"); cc.Approval != nil || cc.State != CaptureComplete || slices.Contains(w.kinds(), "cursor:enable") {
		t.Fatalf("legacy cursor: %+v %v", cc.Approval, w.kinds())
	}
	// An evidence write failure in the stage ends the capture (exit 1),
	// before the session.
	w = newCapWorld(t)
	var out string
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "enable" {
			os.MkdirAll(filepath.Join(out, "clients", "cursor", FileApprovalStdout), 0o700)
		}
		return w.defaults(spec)
	}
	c = newCapRunner(t, w, capPlan(t, "cursor"))
	c.Home, out = t.TempDir(), c.OutDir
	if _, err := c.Run(context.Background()); !errors.Is(err, errCaptureIO) || len(w.sessions()) != 0 {
		t.Fatalf("write failure: %v %v", err, w.kinds())
	}
	// A generated configuration the enable made unreadable as a bounded
	// regular file blocks the session.
	_, man, _, _ = cursorRun(t, func(string, ProcSpec) capBehavior {
		return capBehavior{writes: map[string]*string{".cursor/mcp.json": strp(strings.Repeat(" ", MaxCaseFileBytes+1))}}
	}, nil)
	if cc := capClient(t, man, "cursor"); !strings.HasPrefix(reasonOf(cc), ReasonConfigFailure) || cc.Approval.Scope != ScopeWorkspaceOnly || cc.Session.State != StagePrepared {
		t.Fatalf("config re-read: %q", reasonOf(cc))
	}
}

// UT-13: the strict schema of the approval record and its files: present
// exactly for a Cursor recipe asking for --trust (older manifests have
// neither), the fixed command, consistent stage, scope, reason, changes
// and files, and never a model session after a scope other than
// workspace_only.
func TestCaptureApprovalManifest(t *testing.T) {
	_, man, b, _ := cursorRun(t, nil, nil)
	files := map[string]EvidenceRef{}
	for _, f := range man.Files {
		files[f.Path] = f
	}
	clone := func() *CaptureManifest {
		var m CaptureManifest
		if err := json.Unmarshal(b.ManifestBytes, &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	if m, err := ParseCaptureManifest(b.ManifestBytes); err != nil || m.Clients[0].Approval == nil {
		t.Fatalf("parse: %v", err)
	}
	for name, mutate := range map[string]func(m *CaptureManifest){
		"no-record":       func(m *CaptureManifest) { m.Clients[0].Approval = nil },
		"argv":            func(m *CaptureManifest) { m.Clients[0].Approval.Argv = []string{"mcp", "enable", "--all"} },
		"cwd":             func(m *CaptureManifest) { m.Clients[0].Approval.Cwd = "<home>" },
		"nil-changes":     func(m *CaptureManifest) { m.Clients[0].Approval.Changes = nil },
		"no-changes":      func(m *CaptureManifest) { m.Clients[0].Approval.Changes = []CaptureApprovalChange{} },
		"outside-change":  func(m *CaptureManifest) { m.Clients[0].Approval.Changes[0].Path = "<home>/.cursor/x" },
		"bad-path":        func(m *CaptureManifest) { m.Clients[0].Approval.Changes[0].Path = "/home/owner/.cursor/x" },
		"escaping-path":   func(m *CaptureManifest) { m.Clients[0].Approval.Changes[0].Path = "<workspace>/../x" },
		"bad-kind":        func(m *CaptureManifest) { m.Clients[0].Approval.Changes[0].Kind = "renamed" },
		"duplicate":       func(m *CaptureManifest) { a := m.Clients[0].Approval; a.Changes = append(a.Changes, a.Changes[0]) },
		"unknown-scope":   func(m *CaptureManifest) { m.Clients[0].Approval.Scope = "global" },
		"reason-on-ok":    func(m *CaptureManifest) { m.Clients[0].Approval.Reason = sptr("x") },
		"incomplete-inv":  func(m *CaptureManifest) { m.Clients[0].Approval.InventoryComplete = false },
		"not-run-stage":   func(m *CaptureManifest) { m.Clients[0].Approval.Stage = notRunStage("x") },
		"failed-enable":   func(m *CaptureManifest) { one := 1; m.Clients[0].Approval.Stage.Exit = &one },
		"prepared-stage":  func(m *CaptureManifest) { m.Clients[0].Approval.Stage.State = StagePrepared },
		"session-after":   func(m *CaptureManifest) { a := m.Clients[0].Approval; a.Scope, a.Reason = ScopeUnverifiable, sptr("x") },
		"other-client":    func(m *CaptureManifest) { m.Clients[0].Argv = m.Clients[0].Argv[:len(m.Clients[0].Argv)-1] },
		"not-checked-ran": func(m *CaptureManifest) { a := m.Clients[0].Approval; a.Scope, a.Reason = ScopeNotChecked, sptr("x") },
		"empty-reason":    func(m *CaptureManifest) { m.Clients[0].Approval.Reason = sptr("") },
	} {
		m := clone()
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A record on another client, and approval files without a record.
	m := clone()
	m.Clients[0].ID, m.Clients[0].Decoder, m.Clients[0].CaseID = "claude", "claude-json", "claude-capture-setup"
	if err := m.Clients[0].validate(map[string]EvidenceRef{}, m.Limits); err == nil || !strings.Contains(err.Error(), "approval record belongs only") {
		t.Fatalf("other client: %v", err)
	}
	if _, _, ok := payloadOwner(ClientFile("claude", FileApprovalStdout)); ok {
		t.Fatal("an approval file is allowed for claude")
	}
	m = clone()
	c := &m.Clients[0]
	c.Approval, c.Argv = nil, c.Argv[:len(c.Argv)-1]
	if err := c.validate(files, m.Limits); err == nil || !strings.Contains(err.Error(), "without an approval record") {
		t.Fatalf("orphan approval files: %v", err)
	}
	// Consistent failure records validate: not checked (unlaunched),
	// unverifiable without a launch, outside with a launch; none may be
	// followed by a session.
	stop := func(m *CaptureManifest, a *CaptureApproval) {
		c := &m.Clients[0]
		c.Approval, c.State, c.Reason = a, CapturePartial, sptr("stopped")
		c.Session = CaptureStage{State: StagePrepared, Reason: sptr("stopped")}
		c.Probe, c.ProbeReason = nil, sptr("the session did not run")
		var kept []EvidenceRef
		for _, f := range m.Files {
			if base := filepath.Base(f.Path); base != FileVendorEvents && base != FileVendorStderr && base != FileServerEvents && (a.Stage.State == StageRan || !slices.Contains(CaptureApprovalFiles, base)) {
				kept = append(kept, f)
			}
		}
		m.Files = kept
		var streams []CaptureStream
		for _, s := range c.Streams {
			if _, ok := files[s.Path]; ok && slices.ContainsFunc(kept, func(f EvidenceRef) bool { return f.Path == s.Path }) {
				streams = append(streams, s)
			}
		}
		c.Streams = streams
		m.State, m.Reason = CapturePartial, sptr("client cursor is partial")
	}
	ran := clone().Clients[0].Approval.Stage
	for name, a := range map[string]*CaptureApproval{
		"not-checked":  newApproval("not reached"),
		"unverifiable": {Argv: CursorApprovalArgv(), Cwd: "<workspace>", Stage: notRunStage("x"), Scope: ScopeUnverifiable, Reason: sptr("x"), Changes: []CaptureApprovalChange{}},
		"outside": {Argv: CursorApprovalArgv(), Cwd: "<workspace>", Stage: ran, Scope: ScopeOutside, Reason: sptr("x"), InventoryComplete: true,
			Changes: []CaptureApprovalChange{{"<ancestor-12>/.cursor/a", ChangeModified}, {"<home>/.cursor", ChangeAdded}}},
	} {
		m := clone()
		stop(m, a)
		if err := m.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, a := range map[string]*CaptureApproval{
		"unverifiable-outside": {Argv: CursorApprovalArgv(), Cwd: "<workspace>", Stage: ran, Scope: ScopeUnverifiable, Reason: sptr("x"), Changes: []CaptureApprovalChange{{"<home>/.cursor", ChangeAdded}}},
		"unverifiable-changes": {Argv: CursorApprovalArgv(), Cwd: "<workspace>", Stage: notRunStage("x"), Scope: ScopeUnverifiable, Reason: sptr("x"), Changes: []CaptureApprovalChange{{"<workspace>/x", ChangeAdded}}},
		"outside-in-workspace": {Argv: CursorApprovalArgv(), Cwd: "<workspace>", Stage: ran, Scope: ScopeOutside, Reason: sptr("x"), Changes: []CaptureApprovalChange{{"<workspace>/x", ChangeAdded}}},
	} {
		m := clone()
		stop(m, a)
		if err := m.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A manifest written before B1 (a Cursor recipe without --trust and no
	// record) stays readable.
	w := newCapWorld(t)
	legacy := mustCapturePlan(t, filledPlan(t, func(c map[string]any) {
		c["session"] = map[string]any{"argv": []any{"-p", "{prompt}", "--output-format", "stream-json", "--model", "fake-model-1"}}
	}, "cursor"))
	_, lb, _ := b1Capture(t, w, legacy, nil)
	// The record's absence is checked structurally, on the decoded client
	// (code review B1.5 round 3: never a byte search of JSON).
	var legacyRaw struct {
		Clients []map[string]json.RawMessage `json:"clients"`
	}
	if _, err := ParseCaptureManifest(lb.ManifestBytes); err != nil || json.Unmarshal(lb.ManifestBytes, &legacyRaw) != nil || len(legacyRaw.Clients) != 1 {
		t.Fatalf("legacy manifest: %v", err)
	}
	if _, ok := legacyRaw.Clients[0]["approval"]; ok {
		t.Fatal("a legacy manifest carries an approval record")
	}
	// The redaction fixed point covers the approval record and files.
	red := NewCaptureRedactor(nil, nil)
	for p, data := range b.Files {
		if err := RedactionFixedPoint(p, data, red); err != nil {
			t.Fatal(err)
		}
	}
	if err := RedactionFixedPoint(CaptureManifestName, b.ManifestBytes, red); err != nil {
		t.Fatal(err)
	}
	if err := RedactionFixedPoint(CaptureManifestName, bytes.Replace(b.ManifestBytes, []byte(approvedFile), []byte(".cursor/token=leaked-value"), 1), red); err == nil {
		t.Fatal("a residual credential in an approval path is a fixed point")
	}
	if !validChangePath("<ancestor-3>/.cursor") || validChangePath("<ancestor-0>/.cursor") || validChangePath("<workspace>/") || validChangePath("<home>/x") {
		t.Fatal("change path grammar")
	}
}

// Code review B1 round 1, W1: the unit view's boundary is a test-owned
// fixture root. Markers above it (here in a directory that, like a
// developer's TMPDIR, encloses the fixture) are modelled clean, so the
// trusted Grok and Cursor captures complete; a view anchored at that
// enclosing directory instead sees them and refuses; the same markers at
// the fixture root are always seen.
func TestTempViewBoundary(t *testing.T) {
	plant := func(dir string) {
		os.MkdirAll(filepath.Join(dir, ".git"), 0o700)
		os.MkdirAll(filepath.Join(dir, ".grok"), 0o700)
		os.WriteFile(filepath.Join(dir, ".grok", "config.toml"), nil, 0o600)
		os.MkdirAll(filepath.Join(dir, ".cursor"), 0o700)
	}
	run := func(view tempView, fixture string) *CaptureManifest {
		w := newCapWorld(t)
		man, _, _ := b1Capture(t, w, capPlan(t, "grok", "cursor"), func(c *CaptureRunner) {
			c.OutDir, c.Home = filepath.Join(fixture, "out"), filepath.Join(fixture, "home")
			c.GrokPlacementFS, c.approvalFS = view, view
		})
		return man
	}
	enclosing := t.TempDir()
	plant(enclosing)
	fixture := filepath.Join(enclosing, "fixture")
	os.MkdirAll(filepath.Join(fixture, "home"), 0o700)
	if man := run(newTempViewAt(fixture), fixture); man.State != CaptureComplete {
		t.Fatalf("fixture-root view: %s %q %q", man.State, reasonOf(capClient(t, man, "grok")), reasonOf(capClient(t, man, "cursor")))
	}
	inner := filepath.Join(enclosing, "fixture2")
	os.MkdirAll(filepath.Join(inner, "home"), 0o700)
	if man := run(newTempViewAt(enclosing), inner); !strings.HasPrefix(reasonOf(capClient(t, man, "grok")), ReasonGrokPlacement) {
		t.Fatalf("enclosing-root view: %q", reasonOf(capClient(t, man, "grok")))
	}
	marked := filepath.Join(enclosing, "fixture3")
	os.MkdirAll(filepath.Join(marked, "home"), 0o700)
	plant(marked)
	if man := run(newTempViewAt(marked), marked); !strings.HasPrefix(reasonOf(capClient(t, man, "grok")), ReasonGrokPlacement+": lexical chain: a .git entry 1 levels up") {
		t.Fatalf("markers at the fixture root: %q", reasonOf(capClient(t, man, "grok")))
	}
}

// Code review B1 round 1, C1: a raw manifest with any approval or stage
// pointer member set to null is refused with an error, never a panic.
func TestCaptureApprovalNullMembers(t *testing.T) {
	_, _, b, _ := cursorRun(t, nil, nil)
	if _, err := ParseCaptureManifest(b.ManifestBytes); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"approval", "stage", "cleanup"}, {"approval", "stage", "exit"}, {"approval", "stage"}, {"approval", "changes"}, {"approval", "argv"}, {"approval"},
		{"session", "cleanup"}, {"session", "exit"}, {"version", "cleanup"}, {"version", "exit"}, {"help", "cleanup"}, {"help", "exit"}, {"probe"},
		{"observed_version"}, {"executable_sha256"}, {"streams"},
	} {
		var m map[string]any
		if err := json.Unmarshal(b.ManifestBytes, &m); err != nil {
			t.Fatal(err)
		}
		obj := m["clients"].([]any)[0].(map[string]any)
		for _, k := range path[:len(path)-1] {
			obj = obj[k].(map[string]any)
		}
		obj[path[len(path)-1]] = nil
		raw, _ := json.Marshal(m)
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = nil
					t.Fatalf("%v: ParseCaptureManifest panicked: %v", path, r)
				}
			}()
			_, err = ParseCaptureManifest(raw)
			return err
		}()
		if err == nil {
			t.Fatalf("%v: a null member was accepted", path)
		}
	}
}

// Production wiring pin (design decoder-enrollment B1 sign-off): the
// capture runner the CLI builds from Env leaves the placement and approval
// filesystems at their operating-system defaults, and Env has no field
// that could carry a placement view.
func TestCaptureProductionWiring(t *testing.T) {
	env := capEnv(t, newCapWorld(t))
	c := newCaptureRunner(env, capPlan(t, "grok"), "/abs/out", CleanupPolicy{Grace: CleanupGrace, Limit: CleanupLimit, Poll: CleanupPoll}, bytes.Repeat([]byte{7}, 19))
	if c.GrokPlacementFS != nil || c.approvalFS != nil || c.ApprovalLimits != (ApprovalScanLimits{}) || c.Home != env.Home || c.OutDir != "/abs/out" {
		t.Fatalf("production runner %+v", c)
	}
	view := reflect.TypeOf((*GrokPlacementFS)(nil)).Elem()
	scan := reflect.TypeOf((*approvalFS)(nil)).Elem()
	et := reflect.TypeOf(Env{})
	for i := 0; i < et.NumField(); i++ {
		f := et.Field(i)
		if f.Type == view || f.Type.Implements(view) || f.Type.Implements(scan) || strings.Contains(strings.ToLower(f.Name), "placement") {
			t.Fatalf("Env field %s could carry a placement view", f.Name)
		}
	}
}
