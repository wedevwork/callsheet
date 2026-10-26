package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/sidecar"
)

// Function tests of design 12a-3-cursor-worker, slice 3a (3a.md
// traceability table): one per local FP, on the one shared probe rig with
// injected clock, vendors, plane, push and filesystem.

// stubVersion is an executable printing line for --version.
func stubVersion(t *testing.T, line string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+line+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// cursorFlowBytes is the shipped alternate flow.
func cursorFlowBytes(t testing.TB) []byte {
	return mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(DevFullDir), "flow-cursor-coder.json"))
}

func TestCursor3aFP1(t *testing.T) {
	t.Parallel()
	// The alternate flow is accepted and the default is exactly preserved.
	flow, err := ParseDevFullFlow(cursorFlowBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	def := DefaultDevFullFlow()
	for i, r := range flow.Roles {
		want := def.Roles[i]
		if r.ID == HopCoder {
			want = FlowRole{ID: HopCoder, Adapter: adapter.CursorID, Model: "grok-4.7", Effort: "high"}
		}
		if r != want {
			t.Fatalf("role %d %+v want %+v", i, r, want)
		}
	}
	if flow.Schema != DevFullFlowSchema || flow.Kind() != FlowOverride || !flow.CursorCoder() || def.CursorCoder() {
		t.Fatalf("flow %+v kind %s", flow, flow.Kind())
	}
	shipped, err := ParseDevFullFlow(mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(DevFullDir), FlowFile)))
	if err != nil || shipped.Kind() != FlowDefault || !slices.Equal(shipped.Roles, def.Roles) {
		t.Fatalf("the default flow changed: %v %+v", err, shipped)
	}
	sel, _ := flow.SelectionCanonical()
	dsel, _ := def.SelectionCanonical()
	if bytes.Equal(sel, dsel) || !bytes.Contains(sel, []byte(`{"id":"coder","adapter":"cursor","model":"grok-4.7","effort":"high"}`)) {
		t.Fatalf("selection %s", sel)
	}
	// The live probe labels the selection override-flow with its exact
	// canonical selection hash.
	cr := newCursorRig(t, "")
	cr.expect(1, "")
	if o := cr.observation(); o.FlowLabel != FlowOverride || o.SelectionSHA256 != sha256Hex(sel) || !slices.Equal(o.Roles, flow.Roles) {
		t.Fatalf("receipt selection %s %s", o.FlowLabel, o.SelectionSHA256)
	}
	t.Run("recipe_posture_unchanged", func(t *testing.T) {
		t.Parallel()
		id := hexID("t_", 1)
		inv, err := adapter.NewCursor("").Invocation(adapter.TaskInput{TaskID: id, Model: "grok-4.7", Effort: "high", Prompt: []byte("the composed prompt")})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(inv.Argv, []string{"-p", "--trust", "--auto-review", "--output-format", "json", "--model", "grok-4.7-high", "the composed prompt"}) ||
			len(inv.Stdin) != 0 || inv.FinalFile != "" {
			t.Fatalf("recipe %q stdin %q", inv.Argv, inv.Stdin)
		}
		for _, forbidden := range []string{"--force", "--yolo", "--approve-mcps", "--sandbox"} {
			if slices.Contains(inv.Argv, forbidden) {
				t.Fatalf("forbidden %s", forbidden)
			}
		}
		if _, err := adapter.NewCursor("").Invocation(adapter.TaskInput{TaskID: id, Model: "grok-4.7", Effort: "high",
			Prompt: bytes.Repeat([]byte("x"), adapter.MaxCursorPromptBytes+1)}); err == nil {
			t.Fatal("an oversized prompt was accepted")
		}
		if min, ok := adapter.MinimumVersion(adapter.CursorID); !ok || min != "2026.10.01-e373342" {
			t.Fatalf("minimum %s", min)
		}
		if sidecar.CursorVendorWarning != "cursor adapter enabled: on Linux, tasks run with --trust --auto-review; prompts are passed in argv and may "+
			"be visible to process inspection; the prompt limit is 32 KiB; Cursor records trust and maintains state outside the task directory" {
			t.Fatal("the startup posture text changed")
		}
		doc := string(mustRead(t, filepath.Join(repoRoot(t), "docs", "real-adapters.md")))
		for _, s := range []string{"it is a\npermission policy, not evidence of OS confinement", "Run sidecars under a dedicated OS user",
			"Callsheet never passes `--force`, `--yolo`, `--approve-mcps` or\n`--sandbox`"} {
			if !strings.Contains(doc, s) {
				t.Fatalf("docs/real-adapters.md lost %q", s)
			}
		}
	})
	t.Run("old_version_and_non_linux", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := probeVendor(context.Background(), adapter.CursorID, stubVersion(t, "2026.09.30-abcdef0"), dir); err == nil {
			t.Fatal("an old Cursor version was eligible")
		}
		if err := probeVendor(context.Background(), adapter.CursorID, stubVersion(t, fakeCursorVersion), dir); err != nil {
			t.Fatalf("the observed installation %s: %v", fakeCursorVersion, err)
		}
		if err := flow.CheckDevPosture("darwin"); err == nil || DefaultDevFullFlow().CheckDevPosture("darwin") != nil {
			t.Fatalf("non-Linux posture %v", err)
		}
		if adapter.ValidateWorkerPosture(adapter.CursorID, "darwin") == nil || adapter.ValidateWorkerPosture(adapter.CursorID, "linux") != nil {
			t.Fatal("cursor posture")
		}
	})
}

func TestCursor3aFP2(t *testing.T) {
	t.Parallel()
	t.Run("four_node_probe", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.expect(1, "")
		var sc []string
		for _, a := range sidecarArgs(cr.f) {
			sc = append(sc, strings.Join(a[4:], " "))
		}
		want := []string{"--codex-adapter " + fakeCodex, "--claude-adapter " + fakeClaude, "--grok-adapter " + fakeGrok, "--cursor-adapter " + fakeCursor}
		if !slices.Equal(sc, want) {
			t.Fatalf("sidecars %q", sc)
		}
		// Each sidecar started then restarted by its handle, and stopped.
		n := 0
		for _, s := range cr.f.launcher.specsOf(fakeCallsheet) {
			if len(s.Args) > 1 && s.Args[1] == "run" && slices.Contains(s.Args, "--cursor-adapter") {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("n-cursor started %d times", n)
		}
		if node, a := cr.roleOnNode(HopCoder); node != hexID("n_", 4) || a != adapter.CursorID {
			t.Fatalf("coder on %s %s", node, a)
		}
		for _, id := range []string{HopDesigner, HopCodeReviewer, "triage"} {
			if node, _ := cr.roleOnNode(id); node == hexID("n_", 4) {
				t.Fatalf("%s registered on n-cursor", id)
			}
		}
		if len(cr.d.s.preflight) != 5 {
			t.Fatalf("%d preflights", len(cr.d.s.preflight))
		}
		var pf DevPreflightRecord
		if err := cr.data("preflight/03/observation.json", &pf); err != nil || pf.Role != HopCoder || pf.Adapter != adapter.CursorID || pf.Model != "grok-4.7" ||
			pf.Effort != "high" {
			t.Fatalf("coder preflight %+v %v", pf, err)
		}
		var tools []string
		for _, v := range cr.observation().Versions {
			tools = append(tools, v.Tool)
		}
		if !slices.Equal(tools, []string{"claude", "codex", "grok", "cursor", "go", "git"}) {
			t.Fatalf("versions %v", tools)
		}
		cl := cr.cleanupRecord()
		stopped := 0
		for _, p := range cl.Processes {
			if p.Name == "sidecar-cursor" && p.ExitVerified {
				stopped++
			}
		}
		if !cl.Complete || stopped != 2 {
			t.Fatalf("cleanup %+v", cl)
		}
		// The coder task's effective selection, bound to its node's adapter.
		var v contract.TaskShowResponse
		json.Unmarshal(cr.evidence(probeCoderDir+"/task.json"), &v)
		if v.Task.Role.Adapter != adapter.CursorID || v.Task.Effective.Model != "grok-4.7" || v.Task.Effective.Effort != "high" {
			t.Fatalf("coder task %+v", v.Task.Role)
		}
		if !slices.Equal(sidecarAdapters, []string{"codex", "claude", "grok"}) {
			t.Fatalf("the package sidecar list changed: %v", sidecarAdapters)
		}
	})
	t.Run("default_three_node", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.expect(0, "")
		if a := sidecarArgs(r.f); len(a) != 3 || slices.ContainsFunc(a, func(x []string) bool { return slices.Contains(x, "--cursor-adapter") }) {
			t.Fatalf("default sidecars %q", a)
		}
		if q := r.qualification(); len(q.Versions) != 5 || q.Result != ResultQualified {
			t.Fatalf("default qualification %+v", q.Versions)
		}
	})
	// Refusals before any world: binding, redirected configuration homes,
	// and the future Cursor attempts.
	type refusal struct {
		cmd  string
		flow bool
		// badFlow is a malformed ("malformed") or missing ("missing") --flow.
		badFlow string
		args    []string
		env     map[string]string
		code    int
		token   string
	}
	for name, c := range map[string]refusal{
		"missing_cursor":    {cmd: "probe-dev-full", flow: true, code: 2, token: CodeCursorAdapterRequired},
		"unused_cursor":     {cmd: "probe-dev-full", args: []string{"--cursor", fakeCursor}, code: 2, token: CodeCursorAdapterUnused},
		"relative_cursor":   {cmd: "probe-dev-full", flow: true, args: []string{"--cursor", "cursor-agent"}, code: 2, token: "at most one --cursor"},
		"empty_cursor":      {cmd: "probe-dev-full", flow: true, args: []string{"--cursor="}, code: 2, token: "at most one --cursor"},
		"duplicate_cursor":  {cmd: "probe-dev-full", flow: true, args: []string{"--cursor", fakeCursor, "--cursor", fakeCursor}, code: 2, token: "at most one --cursor"},
		"cursor_config_dir": {cmd: "probe-dev-full", flow: true, args: []string{"--cursor", fakeCursor}, env: map[string]string{EnvCursorConfigDir: ""}, code: 2, token: "cursor_config_dir_unsupported: CURSOR_CONFIG_DIR is set"},
		"xdg_config_home":   {cmd: "probe-dev-full", flow: true, args: []string{"--cursor", fakeCursor}, env: map[string]string{EnvXDGConfigHome: "/secret/value"}, code: 2, token: "cursor_config_dir_unsupported: XDG_CONFIG_HOME is set"},
		"run":               {cmd: "run", flow: true, code: 2, token: CodeCursorPending3b},
		"run_dev_full":      {cmd: "run-dev-full", flow: true, code: 2, token: CodeCursorPending3b},
		"run_dev_light":     {cmd: "run-dev-light", flow: true, code: 2, token: CodeCursorPending3b},
		"run_dev_exercise":  {cmd: "run-dev-exercise", flow: true, code: 2, token: CodeCursorPending3b},
		"run_cursor_flag":   {cmd: "run", flow: true, args: []string{"--cursor", fakeCursor}, code: 2, token: "run needs absolute"},
		// W2 (review round 1): an unreadable or malformed --flow with --cursor
		// is the flow's own error, never an unused adapter.
		"malformed_flow_cursor": {cmd: "probe-dev-full", badFlow: "malformed", args: []string{"--cursor", fakeCursor}, code: 2, token: "refused: flow_invalid: "},
		"missing_flow_cursor":   {cmd: "probe-dev-full", badFlow: "missing", args: []string{"--cursor", fakeCursor}, code: 2, token: "refused: flow_invalid: "},
		"malformed_flow_ci_env": {cmd: "probe-dev-full", badFlow: "malformed", args: []string{"--cursor", fakeCursor}, env: map[string]string{EnvClaudeConfigDir: ""},
			code: 2, token: "claude_config_dir_unsupported"},
		"claude_config_first": {cmd: "probe-dev-full", flow: true, env: map[string]string{EnvClaudeConfigDir: "", EnvCursorConfigDir: ""}, code: 2,
			token: "claude_config_dir_unsupported"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			copyDevDocs(t, f.checkout)
			if c.flow {
				f.flow = filepath.Join(f.checkout, filepath.FromSlash(DevFullDir), "flow-cursor-coder.json")
			}
			switch c.badFlow {
			case "malformed":
				f.flow = filepath.Join(f.checkout, "bad-flow.json")
				os.WriteFile(f.flow, []byte(`{"schema":"callsheet-dev-full-flow/v1","roles":[`), 0o600)
			case "missing":
				f.flow = filepath.Join(f.checkout, "missing-flow.json")
			}
			for k, v := range c.env {
				f.env[k] = v
			}
			h := f.host()
			h.legacy = false
			h.Args = append(f.args(), c.args...)
			h.Args[0] = c.cmd
			h.NewWorld = func(Host) (*World, func(), error) {
				t.Error("a refused command built a world")
				return nil, nil, errors.New("refused")
			}
			if code := MainWith(h); code != c.code || !strings.Contains(f.stderr.String(), c.token) {
				t.Fatalf("exit %d stderr %s", code, f.stderr.String())
			}
			if c.badFlow != "" && strings.Contains(f.stderr.String(), CodeCursorAdapterUnused) {
				t.Fatalf("an invalid flow reported as an unused adapter: %s", f.stderr.String())
			}
			if strings.Contains(f.stderr.String(), "/secret/value") || len(f.launcher.argv()) != 0 {
				t.Fatal("a refused command leaked a value or started a process")
			}
		})
	}
	t.Run("ci_gate_first", func(t *testing.T) {
		t.Parallel()
		var out, errb bytes.Buffer
		h := Host{GOOS: "linux", Args: []string{"probe-dev-full", "--cursor", "relative", "--evidence", "/e"},
			Getenv: func(k string) string { return map[string]string{EnvOptIn: "1"}[k] }, LookupEnv: func(k string) (string, bool) { return "", k == EnvCI },
			Stdout: &out, Stderr: &errb, NewWorld: func(Host) (*World, func(), error) { t.Error("built a world"); return nil, nil, nil }}
		if code := MainWith(h); code != 2 || !strings.Contains(errb.String(), ErrCI.Error()) || !strings.Contains(errb.String(), "Cursor diagnostic probe") {
			t.Fatalf("CI gate %d %s", code, errb.String())
		}
	})
}

func TestCursor3aFP3(t *testing.T) {
	t.Parallel()
	t.Run("bounded_report_only", func(t *testing.T) {
		t.Parallel()
		// An arbitrarily large, foreign and unreadable host: bounded samples,
		// flags only, and the probe still observes every stage.
		cr := newCursorRig(t, "")
		cache := bigDir(2_000_000, 2_000_000)
		cache.kids = nil
		tmp := &synthNode{typ: actDir, uid: 0, ino: 2, kids: []synthKid{{"theirs", &synthNode{typ: actDir, uid: 0, ino: 3}},
			{"locked", &synthNode{typ: actDir, uid: 1000, ino: 4, openErr: syscall.EACCES}}, {"gone", &synthNode{typ: actDir, lstatErr: syscall.ENOENT}}}}
		fsys := &synthFS{t: t, roots: map[string]*synthNode{cr.roots[0].Path: cache, cr.roots[1].Path: tmp}}
		cr.activityFS = fsys
		cr.probeHook = func(p *devProbe) {
			p.cursor.roots, p.cursor.fs, p.cursor.uid = cr.roots, fsys, 1000
		}
		cr.expect(1, "")
		o := cr.observation()
		if o.Result != ResultObserved || o.Blocker != CodeCursorQualificationPending {
			t.Fatalf("receipt %s %s", o.Result, o.Blocker)
		}
		if c := fsys.consumed.Load(); c > 2*3*(65+64*65) {
			t.Fatalf("consumed %d", c)
		}
		for _, phase := range []string{"before", "after"} {
			a := cr.activity(phase)
			c, tm, v := a.Roots[0], a.Roots[1], a.Roots[2]
			if c.Scope != activityScopeCache || !c.Flags.EntryLimited || c.Exhaustive || len(c.Rows) != 64 || tm.Status != actForeign ||
				tm.Flags.ForeignEntries != 1 || tm.Flags.UnreadableEntries != 1 || tm.Flags.RacedEntries != 1 || v.Status != actAbsent {
				t.Fatalf("%s %+v / %+v / %+v", phase, c.Flags, tm, v.Status)
			}
		}
		text := string(cr.evidence(cursorObservationText))
		// W1 (review round 1): a sampled directory of more than 64 children
		// discloses its own row-level flags in the text.
		row := "before cache $HOME/.cache row e0000000: type directory, status ok; sampled children file=0 directory=64 symlink=0 other=0 unknown=0 bytes 0; " +
			"children exhaustive false; flags entry_limited,depth_limited"
		for _, s := range []string{row, "entry_limited", "foreign_entries=1", "not attribution to Cursor", "no confinement claim", "status absent",
			"row theirs: type directory, status foreign"} {
			if !strings.Contains(text, s) {
				t.Fatalf("text lacks %q:\n%s", s, text)
			}
		}
	})
	// protected_changes_still_fail: each protected owner change made while
	// the probe runs keeps its original protected failure, whatever the
	// samples report; a repository under the sampled cache or tmp root is
	// still the independent scanner's.
	for _, name := range []string{"cursor_mcp", "claude_settings", "claude_skill", "repo_under_cache", "repo_under_tmp"} {
		t.Run("protected_changes_still_fail/"+name, func(t *testing.T) {
			t.Parallel()
			cr := newCursorRig(t, "")
			// want is the protected change's scope and path suffix.
			var repo, scope, want string
			switch name {
			case "repo_under_cache", "repo_under_tmp":
				parent := cr.roots[0].Path
				if name == "repo_under_tmp" {
					parent = cr.roots[1].Path
				}
				repo = filepath.Join(parent, "proj")
				newRepo(t, repo)
				cr.f.git = realGit(t)
				cr.f.roots = []string{parent}
				want = "/proj"
			case "cursor_mcp":
				scope, want = "cursor", "mcp.json"
			case "claude_settings":
				scope, want = "claude", "settings.json"
			case "claude_skill":
				scope, want = "claude", "skills/a/SKILL.md"
			}
			cr.act.during = func(cr *cursorRig) {
				switch name {
				case "cursor_mcp":
					write(t, filepath.Join(cr.f.home, ".cursor", "mcp.json"), "{}")
				case "claude_settings":
					write(t, filepath.Join(cr.f.home, ".claude", "settings.json"), `{"permissions":{"allow":["Bash(*)"]}}`+"\n")
				case "claude_skill":
					write(t, filepath.Join(cr.f.home, ".claude", "skills", "a", "SKILL.md"), "changed skill\n")
				default:
					write(t, filepath.Join(repo, "a.txt"), "changed\n")
				}
			}
			cr.expect(1, "")
			o := cr.observation()
			if o.Result != ResultBlocked || o.Blocker != CodeEnvModified || o.Environment.ProtectedChanges == 0 || !cr.d.protected {
				t.Fatalf("receipt %s %s %+v", o.Result, o.Blocker, o.Environment)
			}
			var diff EnvDiff
			json.Unmarshal(cr.evidence("environment/diff.json"), &diff)
			found := false
			for _, c := range diff.Protected {
				found = found || c.Scope == scope && strings.HasSuffix(c.Path, want)
			}
			if !found {
				t.Fatalf("%s not a protected change: %+v", want, diff.Protected)
			}
			if !strings.Contains(cr.f.stdout.String(), "Cursor probe observations: BLOCKED ("+CodeEnvModified+")") {
				t.Fatalf("stdout %s", cr.f.stdout.String())
			}
			cr.noQualification()
		})
	}
}

func TestCursor3aFP4(t *testing.T) {
	t.Parallel()
	t.Run("repair_observed", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.expect(1, "")
		c := cr.d.probe.cursor
		if len(cr.coderReqs) != 1 {
			t.Fatalf("coder dispatches %d", len(cr.coderReqs))
		}
		req := cr.coderReqs[0]
		if req.Acceptance != cursorProbeAcceptance || req.Acceptance == devAcceptance(&TaskPlan{Role: HopCoder, Mode: modeImplement}) ||
			len(req.Payload) != 0 || req.Override != nil || *req.Base != c.inputCommit.String() || *req.Workspace != c.ws.Name ||
			*req.WorkspaceInstance != c.ws.Instance {
			t.Fatalf("dispatch %+v", req)
		}
		for _, s := range []string{cursorProbeAuthority, "mode: implement", "fp_index: FP-1 greeting (assigned; its function test is the existing main_test.go TestRun greeting subtests); FP-2 errors and help (not assigned",
			"allowed_outputs: main.go only", "- main_test.go " + sha256Hex(c.input["main_test.go"]), "- go.mod " + sha256Hex(c.input["go.mod"]),
			"governing_design: design.md sha256 " + sha256Hex(c.input["design.md"]), "require 100% pass and coverage above 80%"} {
			if !strings.Contains(req.Goal, s) {
				t.Fatalf("goal lacks %q:\n%s", s, req.Goal)
			}
		}
		// The fixture: one exact Pending prefix, the known-good expectation
		// captured inertly before the worker ran.
		in, exp := string(c.input["main.go"]), string(c.expected["main.go"])
		if strings.Count(in, `"Pending, %s!\n"`) != 1 || strings.Contains(in, `"Hello, `) || !strings.Contains(exp, `"Hello, %s!\n"`) ||
			!bytes.Equal(cr.evidence(capturedPath(probeCoderDir+"/expected/main.go")), c.expected["main.go"]) ||
			!bytes.Equal(cr.evidence(capturedPath(probeCoderDir+"/input/main.go")), c.input["main.go"]) {
			t.Fatal("fixture")
		}
		var pub cursorPublicationRecord
		if err := cr.data(probeCoderDir+"/publication.json", &pub); err != nil || !pub.Admitted || !slices.Equal(pub.Changed, []string{"main.go"}) ||
			pub.Parent != c.inputCommit.String() {
			t.Fatalf("publication %+v %v", pub, err)
		}
		var cov DevCoverageRecord
		json.Unmarshal(cr.evidence(probeCoderDir+"/validation/coverage.json"), &cov)
		var cases CasesRecord
		json.Unmarshal(cr.evidence(probeCoderDir+"/validation/cases.json"), &cases)
		if cov.Coverage == nil || !cov.Coverage.Pass || cov.Commit != pub.Commit || len(cases.Cases) != len(FeatureCases) {
			t.Fatalf("validation %+v %d", cov, len(cases.Cases))
		}
		o := cr.observation()
		if !o.Coder.PublicationAdmitted || !o.Coder.ValidationPassed || o.Coder.Marker != markerImplComplete || stageNamed(o, stageCoderEvidence).Result != stagePass ||
			o.Coder.ResultCommit != pub.Commit {
			t.Fatalf("coder observation %+v", o.Coder)
		}
		if len(cr.pushes) != 1 || cr.pushes[0].Branch != "main" || !cr.pushes[0].PathSet || cr.pushes[0].Name != c.ws.Name ||
			!filepath.IsAbs(cr.pushes[0].Path) || !contract.ValidWorkspaceName(c.ws.Name) || !strings.HasPrefix(c.ws.Name, "e2e-probe-coder-") {
			t.Fatalf("pushes %+v", cr.pushes)
		}
	})
	for name, c := range map[string]struct {
		act  cursorAct
		code string
	}{
		"no_op": {cursorAct{edit: func(f map[string][]byte) {
			f["main.go"] = bytes.Replace(f["main.go"], []byte(`"Hello, `), []byte(`"Pending, `), 1)
		}}, CodeProbeCoderPublication},
		"test_edit":    {cursorAct{edit: func(f map[string][]byte) { f["main_test.go"] = append(f["main_test.go"], "\n// edited\n"...) }}, CodeProbeCoderPublication},
		"wrong_parent": {cursorAct{parent: plumbing.NewHash("1111111111111111111111111111111111111111")}, CodeProbeCoderPublication},
		"forged_success": {cursorAct{edit: func(f map[string][]byte) {
			f["main.go"] = bytes.Replace(f["main.go"], []byte(`"Hello, `), []byte(`"Hi, `), 1)
		}}, CodeProbeCoderPublication},
		"extra_file":      {cursorAct{edit: func(f map[string][]byte) { f["NOTES.md"] = []byte("x\n") }}, CodeProbeCoderPublication},
		"executable_mode": {cursorAct{mode: filemode.Executable}, CodeProbeCoderPublication},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cr := newCursorRig(t, "")
			cr.act = c.act
			o := cr.coderStopped(c.code, "task.json", "final.txt", "logs.txt", "verdict.json")
			if o.Coder.PublicationAdmitted || o.Coder.ValidationPassed {
				t.Fatalf("admitted %+v", o.Coder)
			}
		})
	}
	t.Run("validation_failed", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.f.testActions["name"] = "fail"
		o := cr.coderStopped(CodeProbeCoderValidation, "publication.json", "validation/requirements.json", "validation/tests.jsonl")
		if !o.Coder.PublicationAdmitted || o.Coder.ValidationPassed {
			t.Fatalf("coder %+v", o.Coder)
		}
	})
	t.Run("push_lost", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.pushErr = errors.New("the answer was lost")
		cr.expect(1, CodeProbeCoderPush)
		if len(cr.pushes) != 1 || len(cr.coderTasks) != 0 {
			t.Fatalf("pushes %d dispatches %d: a retry", len(cr.pushes), len(cr.coderTasks))
		}
	})
	t.Run("push_wrong_commit", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.pushEdit = func(res *contract.WorkspacePushResult) { res.Commit = strings.Repeat("2", 40) }
		cr.expect(1, CodeProbeCoderPush)
	})
	t.Run("result_collection_failed", func(t *testing.T) {
		t.Parallel()
		// C2 (review round 1): the published result's object closure cannot
		// be retained as evidence; the probe stops BLOCKED with the original
		// error, never an observation without its Git evidence.
		cr := newCursorRig(t, "")
		cr.act.during = func(cr *cursorRig) { cr.d.s.collect = failingCollect(t, cr.d.s.collect) }
		o := cr.coderStopped("evidence_write_failed", "task.json", "final.txt", "verdict.json")
		var v cursorCoderVerdict
		if err := cr.data(probeCoderDir+"/verdict.json", &v); err != nil || !strings.Contains(v.Reason, errInjectedStore.Error()) ||
			!strings.Contains(cr.d.stopReason, errInjectedStore.Error()) || o.Coder.ValidationPassed || o.Coder.PublicationAdmitted {
			t.Fatalf("verdict %+v stop %q coder %+v", v, cr.d.stopReason, o.Coder)
		}
		if _, err := os.Stat(filepath.Join(cr.f.evidence, filepath.FromSlash(probeCoderRepoDir), "refs", "heads", "result")); err == nil {
			t.Fatal("a result ref without its retained objects")
		}
	})
}

func TestCursor3aFP5(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		act       cursorAct
		code      string
		artifacts []string
	}{
		"preamble_no_status": {cursorAct{final: "I would need approval to run go test.\n"}, "invalid_marker", []string{"final.txt", "task.json", "logs.txt"}},
		"duplicate_status":   {cursorAct{final: "STATUS: IMPL_COMPLETE\nDone.\nSTATUS: IMPL_COMPLETE\n"}, "invalid_marker", []string{"final.txt"}},
		"empty_final":        {cursorAct{final: "\n"}, "invalid_marker", []string{"final.txt"}},
		"truncated":          {cursorAct{truncated: true}, "final_message_missing", []string{"task.json"}},
		"missing_final":      {cursorAct{nofinal: true}, "final_message_missing", []string{"task.json"}},
		"blocked":            {cursorAct{final: "go test needs an approval I cannot give.\nSTATUS: BLOCKED\n", nopublish: true}, CodeProbeCoderUnqualified, []string{"final.txt"}},
		"design_questions":   {cursorAct{final: "Which prefix?\nSTATUS: DESIGN_QUESTIONS\n"}, CodeProbeCoderUnqualified, []string{"final.txt"}},
		"nonzero":            {cursorAct{state: contract.TaskFailed}, "task_failed", []string{"task.json", "logs.txt"}},
		"pending_deadline":   {cursorAct{pending: true}, "deadline_exceeded", []string{"task.json"}},
		"no_publication":     {cursorAct{nopublish: true}, CodeProbeCoderPublication, []string{"final.txt"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cr := newCursorRig(t, "")
			cr.act = c.act
			o := cr.coderStopped(c.code, c.artifacts...)
			if name == "pending_deadline" {
				// Cancelled through the plane, settled, never answered.
				if !slices.Contains(cr.f.plane.cancels, cr.coderTasks[0]) || cr.terminalState(cr.coderTasks[0]) != contract.TaskCancelled {
					t.Fatalf("pending task %s", cr.terminalState(cr.coderTasks[0]))
				}
			}
			if strings.HasPrefix(name, "blocked") || name == "design_questions" {
				var v cursorCoderVerdict
				if err := cr.data(probeCoderDir+"/verdict.json", &v); err != nil || v.Marker == "" || v.Code != CodeProbeCoderUnqualified {
					t.Fatalf("verdict %+v %v", v, err)
				}
			}
			if o.Calls.Coder != 1 || o.Calls.Preflights != 5 || o.Calls.WorkerCalls != 6 {
				t.Fatalf("calls %+v", o.Calls)
			}
		})
	}
	t.Run("partial_sample_is_not_a_failure", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.output = activitySkeleton + 1
		for i := 0; i < 70; i++ {
			os.MkdirAll(filepath.Join(cr.roots[1].Path, "d"+itoa(i)), 0o700)
		}
		cr.expect(1, "")
		a := cr.activity("before")
		if o := cr.observation(); o.Result != ResultObserved || !a.Roots[0].Flags.OutputLimited || !a.Roots[1].Flags.EntryLimited {
			t.Fatalf("partial sample %+v %+v", a.Roots[0].Flags, a.Roots[1].Flags)
		}
	})
}

func TestCursor3aFP6(t *testing.T) {
	t.Parallel()
	cr := newCursorRig(t, probeScopeBoth)
	cr.expect(1, "")
	cr.noQualification()
	o := cr.observation()
	if o.Schema != CursorObservationSchema || o.Slice != "3a" || o.Result != ResultObserved || o.Qualification || o.Blocker != CodeCursorQualificationPending ||
		!qualifiedStages(o.Stages, cursorProbeStagesFor(probeScopeBoth)) || len(o.Stages) != 7 || o.Calls.WorkerCalls != 12 || o.Calls.MaxWorkerCalls != 12 ||
		o.Calls.Preflights != 5 || o.Calls.Coder != 1 || o.Calls.Coordinator != 6 || o.Phases != 3 || !o.Cleanup.Complete || o.StopCode != nil ||
		!o.Environment.Complete || o.Environment.ProtectedChanges != 0 || o.Scope != probeScopeBoth {
		t.Fatalf("receipt %+v", o)
	}
	out := cr.f.stdout.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[len(lines)-2] != "Cursor probe observations: OBSERVED" ||
		lines[len(lines)-1] != "qualification probe: BLOCKED (cursor_qualification_pending: independent Cursor evidence checking is deferred to slice 3b)" ||
		strings.Contains(out, "qualification probe: "+ResultQualified) {
		t.Fatalf("stdout tail %q", lines[len(lines)-2:])
	}
	for _, a := range []string{cursorObservationText, cursorActivityFiles[0], cursorActivityFiles[1], "cost.json", probeCoderRepoDir + "/refs/heads/result"} {
		cr.evidence(a)
		if !slices.Contains(o.Artifacts, a) && a != cursorObservationText {
			t.Fatalf("receipt does not reference %s", a)
		}
	}
	for _, a := range o.Artifacts {
		if filepath.IsAbs(a) || strings.Contains(a, "..") {
			t.Fatalf("unsafe artifact reference %s", a)
		}
	}
	// The offline check refuses the diagnostic before routing, criteria,
	// the missing qualification/manifest and the Cursor-inclusive version
	// list, in the report and on stderr, exit 2.
	check := func(dir string) (int, DevReport, string) {
		var out, errb bytes.Buffer
		code := CheckEvidence(dir, &out, &errb)
		var rep DevReport
		json.Unmarshal(out.Bytes(), &rep)
		return code, rep, errb.String()
	}
	if _, err := os.Stat(filepath.Join(cr.f.evidence, devManifestFile)); err == nil {
		t.Fatal("a diagnostic wrote a manifest")
	}
	code, rep, errOut := check(cr.f.bundle())
	if code != 2 || rep.Error != CodeDiagnosticNotQualification || len(rep.Criteria) != 0 ||
		errOut != "reale2e check: diagnostic_not_qualification: slice 3a observations are not qualification evidence\n" {
		t.Fatalf("check %d %+v %q", code, rep.Error, errOut)
	}
	// Even next to a (forged) qualification record and manifest whose
	// version lists include cursor, the refusal comes first.
	forged := filepath.Join(t.TempDir(), "forged")
	if err := copyTree(cr.f.bundle(), forged); err != nil {
		t.Fatal(err)
	}
	q := DevQualification{Schema: devQualificationSchema, Result: ResultQualified, Versions: o.Versions}
	b, _ := encodeJSON(q)
	os.WriteFile(filepath.Join(forged, devQualificationFile), b, 0o600)
	m, _ := encodeJSON(DevManifest{Schema: DevBundleSchema, Scenario: scenarioFull, Versions: o.Versions})
	os.WriteFile(filepath.Join(forged, devManifestFile), m, 0o600)
	if code, rep, _ := check(forged); code != 2 || rep.Error != CodeDiagnosticNotQualification || len(rep.Criteria) != 0 {
		t.Fatalf("forged check %d %s", code, rep.Error)
	}
	// The default gate is intact: the default probe still qualifies and
	// its record is still refused by check as before.
	t.Run("default_gate_intact", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.expect(0, "")
		if q := r.qualification(); q.Result != ResultQualified || !qualifiedStages(q.Stages, probeStages) || q.WorkerCalls != 9 {
			t.Fatalf("default %+v", q)
		}
		if _, err := os.Stat(filepath.Join(r.f.evidence, cursorObservationFile)); err == nil {
			t.Fatal("a default probe wrote a Cursor observation")
		}
		if code, rep, errOut := check(r.f.bundle()); code != 2 || rep.Error == CodeDiagnosticNotQualification || !strings.Contains(errOut, "qualification record") {
			t.Fatalf("default check %d %s %s", code, rep.Error, errOut)
		}
		if codes := qualificationCodes(t, r.f.bundle()); len(codes) != 0 {
			t.Fatalf("default qualification codes %v", codes)
		}
	})
	t.Run("help_and_docs", func(t *testing.T) {
		t.Parallel()
		for _, s := range []string{"[--cursor ABS]", "flow-cursor-coder.json", CodeCursorQualificationPending, CodeCursorPending3b, CodeDiagnosticNotQualification,
			"no jail or\n      confinement claim", "never QUALIFIED and exits 1"} {
			if !strings.Contains(Usage, s) {
				t.Fatalf("Usage lacks %q", s)
			}
		}
		doc := string(mustRead(t, filepath.Join(repoRoot(t), "docs", "real-e2e.md")))
		for _, s := range []string{"### Cursor coder diagnostic probe (slice 3a)", "diagnostic-only disclosure", "There is no jail and no confinement claim",
			"`cursor_execution_pending_3b`", "`cursor_adapter_required`", "`cursor_config_dir_unsupported`", "never `qualification.json`, and always exits 1",
			"`diagnostic_not_qualification`", "report-only observations of all processes, not attribution to Cursor", "slice 3b requires a fresh complete probe",
			"at most ten worker calls for `--scenario full`, twelve for `light` or `both`"} {
			if !strings.Contains(doc, s) {
				t.Fatalf("docs/real-e2e.md lacks %q", s)
			}
		}
	})
}
