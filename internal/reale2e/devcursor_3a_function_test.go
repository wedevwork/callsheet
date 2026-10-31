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
// injected clock, vendors, plane, push and filesystem. Slice 3b replaced
// the probe's live diagnostic finalization with checked qualification:
// only the obsolete assertions that every Cursor probe stays diagnostic
// changed (a fresh probe now writes a Cursor qualification record and
// exits 0 only when its independent check qualifies it); the diagnostic
// receipt's refusal is kept as a historical-fixture test.

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
	cr.expect(0, "")
	if q := cr.cursorQual(); q.FlowLabel != FlowOverride || q.SelectionSHA256 != sha256Hex(sel) || !slices.Equal(q.Roles, flow.Roles) {
		t.Fatalf("record selection %s %s", q.FlowLabel, q.SelectionSHA256)
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
		cr.expect(0, "")
		var sc []string
		for _, a := range sidecarArgs(cr.f) {
			sc = append(sc, strings.Join(a[4:], " "))
		}
		want := []string{"--codex-adapter " + fakeCodex, "--claude-adapter " + fakeClaude, "--grok-adapter " + fakeGrok, "--cursor-adapter " + cr.exe}
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
		for _, v := range cr.cursorQual().Versions {
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
	// Refusals before any world: binding and redirected configuration
	// homes. Slice 3b superseded the attempt commands' pending refusal:
	// they bind a Cursor flow exactly as the probe does (the complete
	// entrypoint matrix is TestCursorExecutionBindingUnit).
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
		"run":               {cmd: "run", flow: true, code: 2, token: CodeCursorAdapterRequired},
		"run_dev_full":      {cmd: "run-dev-full", flow: true, code: 2, token: CodeCursorAdapterRequired},
		"run_dev_light":     {cmd: "run-dev-light", flow: true, code: 2, token: CodeCursorAdapterRequired},
		"run_dev_exercise":  {cmd: "run-dev-exercise", flow: true, code: 2, token: CodeCursorAdapterRequired},
		"run_cursor_flag":   {cmd: "run", args: []string{"--cursor", fakeCursor}, code: 2, token: CodeCursorAdapterUnused},
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
		if code := MainWith(h); code != 2 || !strings.Contains(errb.String(), ErrCI.Error()) || !strings.Contains(errb.String(), "Cursor coder qualification probe") {
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
		uid := uint32(1000)
		cr.uid = &uid
		// Sample quality is report-only: the probe still qualifies.
		cr.expect(0, "")
		if q := cr.cursorQual(); q.Result != ResultQualified {
			t.Fatalf("record %s", q.Result)
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
			// W1 (review round 1): a sampled directory of more than 64
			// children keeps its own row-level flags.
			row := c.Rows[0]
			if row.Name != "e0000000" || !row.Flags.EntryLimited || !row.Flags.DepthLimited || row.ChildrenExhaustive || row.ObservedCounts.Directory != 64 {
				t.Fatalf("%s row %+v", phase, row)
			}
			if !slices.ContainsFunc(tm.Rows, func(r ActivityRow) bool { return r.Name == "theirs" && r.Status == actForeign }) {
				t.Fatalf("%s tmp rows %+v", phase, tm.Rows)
			}
		}
		// The checked report keeps the samples report-only.
		if code, rep, _ := cr.qualifiedCheck(); code != 0 || rep.Result != ResultQualified || rep.Activity.Before != cursorActivityFiles[0] {
			t.Fatalf("check %d %+v", code, rep)
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
			if q := cr.blocked(CodeEnvModified); q.Result != ResultBlocked || !cr.d.protected {
				t.Fatalf("record %s protected %t", q.Result, cr.d.protected)
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
		})
	}
}

func TestCursor3aFP4(t *testing.T) {
	t.Parallel()
	t.Run("repair_observed", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.expect(0, "")
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
		q, v := cr.cursorQual(), cr.coderVerdict()
		if !v.PublicationAdmitted || !v.ValidationPassed || v.Marker != markerImplComplete || stageOf(q, stageCoderEvidence).Result != stagePass ||
			q.Cursor.ResultCommit != pub.Commit || v.ResultCommit != pub.Commit {
			t.Fatalf("coder verdict %+v record %+v", v, q.Cursor)
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
			cr.coderStopped(c.code, "task.json", "final.txt", "logs.txt", "verdict.json")
			if v := cr.coderVerdict(); v.PublicationAdmitted || v.ValidationPassed {
				t.Fatalf("admitted %+v", v)
			}
		})
	}
	t.Run("validation_failed", func(t *testing.T) {
		t.Parallel()
		cr := newCursorRig(t, "")
		cr.f.testActions["name"] = "fail"
		cr.coderStopped(CodeProbeCoderValidation, "publication.json", "validation/requirements.json", "validation/tests.jsonl")
		if v := cr.coderVerdict(); !v.PublicationAdmitted || v.ValidationPassed {
			t.Fatalf("coder %+v", v)
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
		cr.coderStopped("evidence_write_failed", "task.json", "final.txt", "verdict.json")
		if v := cr.coderVerdict(); !strings.Contains(v.Reason, errInjectedStore.Error()) || !strings.Contains(cr.d.stopReason, errInjectedStore.Error()) ||
			v.ValidationPassed || v.PublicationAdmitted {
			t.Fatalf("verdict %+v stop %q", v, cr.d.stopReason)
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
			q := cr.coderStopped(c.code, c.artifacts...)
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
			// Failed calls count: five preflights and the one coder call.
			cost, coder := cr.cursorCost(), 0
			for _, task := range cost.Tasks {
				if task.Label == probeCoderLabel {
					coder++
				}
			}
			if coder != 1 || len(cost.PreflightTasks) != 5 || cost.WorkerCalls != 6 || q.WorkerCalls != 6 {
				t.Fatalf("calls %+v", cost)
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
		cr.expect(0, "")
		a := cr.activity("before")
		if q := cr.cursorQual(); q.Result != ResultQualified || !a.Roots[0].Flags.OutputLimited || !a.Roots[1].Flags.EntryLimited {
			t.Fatalf("partial sample %+v %+v", a.Roots[0].Flags, a.Roots[1].Flags)
		}
	})
}

// cursorDocSections splits docs/maintainers/real-e2e.md's Cursor section: its current
// procedure (from "## Cursor coder qualification" to its historical
// subsection) and the "### Historical slice 3a diagnostic observation"
// subsection (to the next level-two heading).
func cursorDocSections(t testing.TB, doc string) (string, string) {
	t.Helper()
	const head, hist = "\n## Cursor coder qualification\n", "\n### Historical slice 3a diagnostic observation\n"
	i, j := strings.Index(doc, head), strings.Index(doc, hist)
	if i < 0 || j < i || strings.Count(doc, head) != 1 || strings.Count(doc, hist) != 1 {
		t.Fatal("docs/maintainers/real-e2e.md lacks its single Cursor section and historical subsection")
	}
	rest := doc[j+len(hist):]
	if k := strings.Index(rest, "\n## "); k >= 0 {
		rest = rest[:k]
	}
	return doc[i:j], rest
}

// historicalReceipt writes a slice 3a diagnostic receipt shaped like the
// owner's 2026-10-27 D1 observation (OBSERVED, never qualification) into
// dir: a historical fixture, never produced by the 3b probe.
func historicalReceipt(t *testing.T, dir string) CursorProbeObservation {
	t.Helper()
	versions := []VersionRecord{{Tool: "claude"}, {Tool: "codex"}, {Tool: "grok"}, {Tool: "cursor", Version: fakeCursorVersion}, {Tool: "go"}, {Tool: "git"}}
	o := CursorProbeObservation{Schema: CursorObservationSchema, RunID: "20261027T000000Z-00000000", Slice: "3a", Result: ResultObserved,
		Blocker: CodeCursorQualificationPending, Revision: "dc1522cb82603602098657537238396dc96fce62", Versions: versions, Scope: probeScopeBoth,
		Calls: CursorProbeCalls{WorkerCalls: 12, MaxWorkerCalls: 12, Preflights: 5, Coder: 1, Coordinator: 6}, Phases: 3,
		Coder:       CursorCoderObservation{Marker: markerImplComplete, PublicationAdmitted: true, ValidationPassed: true, ChangedCoverage: "100.0% (15/15)"},
		Environment: CursorEnvObservation{Complete: true, Codes: []string{}}, Cleanup: CursorCleanupStatus{Complete: true, RuntimeRemoved: true},
		Activity: CursorActivitySummary{Changes: []ActivityChange{}}, Artifacts: []string{}}
	b, err := encodeJSON(o)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o700)
	write(t, filepath.Join(dir, cursorObservationFile), string(b))
	write(t, filepath.Join(dir, cursorObservationText), "Cursor probe observations: OBSERVED\n")
	return o
}

func TestCursor3aFP6(t *testing.T) {
	t.Parallel()
	// A historical slice 3a receipt (the live diagnostic finalizer was
	// replaced by checked qualification in slice 3b; its refusal stays):
	// the offline check refuses it before routing, criteria, the missing
	// qualification/manifest and the Cursor-inclusive version list, in the
	// report and on stderr, exit 2.
	check := func(dir string) (int, DevReport, string) {
		var out, errb bytes.Buffer
		code := CheckEvidence(dir, &out, &errb)
		var rep DevReport
		json.Unmarshal(out.Bytes(), &rep)
		return code, rep, errb.String()
	}
	hist := filepath.Join(t.TempDir(), "d1")
	o := historicalReceipt(t, hist)
	code, rep, errOut := check(hist)
	if code != 2 || rep.Error != CodeDiagnosticNotQualification || len(rep.Criteria) != 0 ||
		errOut != "reale2e check: diagnostic_not_qualification: slice 3a observations are not qualification evidence\n" {
		t.Fatalf("check %d %+v %q", code, rep.Error, errOut)
	}
	// Even next to a (forged) default or Cursor qualification record and a
	// manifest whose version lists include cursor, the refusal comes first,
	// in the command and in the library loader alike.
	for _, schema := range []string{devQualificationSchema, DevCursorQualificationSchema} {
		forged := filepath.Join(t.TempDir(), "forged")
		if err := copyTree(hist, forged); err != nil {
			t.Fatal(err)
		}
		q := DevQualification{Schema: schema, Result: ResultQualified, Versions: o.Versions, Scope: probeScopeBoth}
		b, _ := encodeJSON(q)
		os.WriteFile(filepath.Join(forged, devQualificationFile), b, 0o600)
		m, _ := encodeJSON(DevManifest{Schema: DevBundleSchema, Scenario: scenarioFull, Versions: o.Versions})
		os.WriteFile(filepath.Join(forged, devManifestFile), m, 0o600)
		if code, rep, _ := check(forged); code != 2 || rep.Error != CodeDiagnosticNotQualification || len(rep.Criteria) != 0 {
			t.Fatalf("%s: forged check %d %s", schema, code, rep.Error)
		}
		if _, err := LoadDevQualification(forged); err == nil || !strings.Contains(err.Error(), CodeDiagnosticNotQualification) {
			t.Fatalf("%s: the library loader accepted a diagnostic bundle: %v", schema, err)
		}
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
		// The retained pins and slice 3b's replacements of the current-live
		// ones (3b.md, Superseded shipped 3a help and documentation pins).
		for _, s := range []string{"[--cursor ABS]", "flow-cursor-coder.json", CodeDiagnosticNotQualification, "no jail or\n      confinement claim",
			"Optional Cursor coder: --flow ABS/flow-cursor-coder.json --cursor ABS.",
			"Cursor probe: QUALIFIED only after independent evidence checking; BLOCKED otherwise.", "Exit 0 qualified, 1 blocked, 2 usage/gate error.",
			"Historical slice 3a observations are not qualification."} {
			if !strings.Contains(Usage, s) {
				t.Fatalf("Usage lacks %q", s)
			}
		}
		for _, s := range []string{CodeCursorPending3b, CodeCursorQualificationPending, "never QUALIFIED and exits 1"} {
			if strings.Contains(Usage, s) {
				t.Fatalf("Usage keeps the obsolete current-live %q", s)
			}
		}
		doc := string(mustRead(t, filepath.Join(repoRoot(t), "docs", "maintainers", "real-e2e.md")))
		current, historical := cursorDocSections(t, doc)
		if strings.Contains(doc, "### Cursor coder diagnostic probe (slice 3a)") {
			t.Fatal("docs/maintainers/real-e2e.md keeps the slice 3a heading")
		}
		for _, s := range []string{"There is no jail and no confinement claim", "`cursor_adapter_required`", "`cursor_config_dir_unsupported`",
			"`diagnostic_not_qualification`", "report-only observations of all processes, not attribution to Cursor",
			"at most ten worker calls for `--scenario full`, twelve for `light` or `both`",
			"The Cursor selection is accepted by run, run-dev-full, run-dev-light and run-dev-exercise only with an explicit --cursor executable.",
			"A fresh Cursor probe writes qualification.json and exits 0 only after its independent check qualifies it.",
			"A slice 3a observation cannot qualify Cursor; a fresh complete probe and its independent checker are required before any Cursor attempt."} {
			if !strings.Contains(current, s) {
				t.Fatalf("the current Cursor procedure lacks %q", s)
			}
		}
		for _, s := range []string{"The 2026-10-27 slice 3a receipt was a diagnostic-only observation, not qualification.",
			"The historical slice 3a probe wrote cursor-probe-observation.json, never qualification.json, and exited 1 even when all stages were observed."} {
			if !strings.Contains(historical, s) {
				t.Fatalf("the historical slice 3a subsection lacks %q", s)
			}
		}
		// No obsolete current-live claim survives in the current procedure,
		// and the retired refusal code nowhere in the section.
		for _, s := range []string{"diagnostic-only disclosure", "never `qualification.json`, and always exits 1", "slice 3b requires a fresh complete probe",
			"`cursor_execution_pending_3b`", "cursor_execution_pending_3b", "refuse a Cursor flow before anything starts"} {
			if strings.Contains(current, s) {
				t.Fatalf("the current Cursor procedure keeps the obsolete %q", s)
			}
		}
		if strings.Contains(historical, "cursor_execution_pending_3b") {
			t.Fatal("the historical subsection keeps the retired refusal code")
		}
	})
}
