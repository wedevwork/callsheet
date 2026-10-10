package reale2e

import (
	"bytes"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// funcBody renders a top-level function's body statements.
func funcBody(t *testing.T, fset *token.FileSet, f *ast.File, name string) []string {
	t.Helper()
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name || fd.Recv != nil {
			continue
		}
		var out []string
		for _, st := range fd.Body.List {
			var b bytes.Buffer
			printer.Fprint(&b, fset, st)
			out = append(out, b.String())
		}
		return out
	}
	t.Fatalf("no func %s", name)
	return nil
}

// TestOfflineBoundary (FP-9): the default entry and the checker run with
// no vendor access; the reale2e-tagged command only forwards host inputs
// (the seventh platform wrapper) and builds; every new untagged file is a
// whole-file coverage entry; the shipped templates satisfy their
// contracts; the native inventory stays 429 and no stress step selects
// this package.
func TestOfflineBoundary(t *testing.T) {
	t.Parallel()
	root := testkit.MustRepoRoot(t)
	// The tagged entry: build constraint, forwarding-only bodies.
	src := filepath.Join(root, "cmd", "reale2e", "main.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if x := buildConstraint(f); x == nil || x.String() != "reale2e" {
		t.Fatalf("cmd/reale2e build constraint %v", x)
	}
	for fn, want := range map[string][]string{
		"main":   {"os.Exit(run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))"},
		"run":    {"return runFor(runtime.GOOS, runtime.GOARCH, args, getenv, stdin, stdout, stderr)"},
		"runFor": {"return reale2e.Main(goos, goarch, args, getenv, os.LookupEnv, stdin, stdout, stderr)"},
	} {
		if got := funcBody(t, fset, f, fn); !slices.Equal(got, want) {
			t.Errorf("cmd/reale2e %s body %q, want %q", fn, got, want)
		}
	}
	if err := devcheck.CheckPlatformSources(root); err != nil {
		t.Fatalf("platform guard: %v", err)
	}
	// It compiles with its tag (and is absent from default-tag listings).
	out := filepath.Join(t.TempDir(), "reale2e")
	cmd := exec.Command("go", "build", "-tags=reale2e", "-o", out, "./cmd/reale2e")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tagged build: %v\n%s", err, b)
	}
	// No other new runtime.GOOS read: the package decides from the
	// supplied goos only (the platform guard above scans it too).
	// The checker reaches no process, network or plane client.
	checker := []string{"check.go", "bundle.go", "report.go", "gitrepo.go", "schema.go", "strictjson.go"}
	for _, name := range checker {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "reale2e", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os/exec" || p == "net" || p == "net/http" || strings.HasSuffix(p, "/internal/client") || strings.HasSuffix(p, "/internal/adapter") {
				t.Errorf("checker file %s imports %s", name, p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && slices.Contains([]string{"Launcher", "ExecLauncher", "runCommand", "World", "Plane"}, id.Name) {
				t.Errorf("checker file %s references %s", name, id.Name)
			}
			return true
		})
	}
	// Coverage registration: every untagged production file, whole, both
	// hosts; platform.go too.
	entries := map[string]devcheck.CoverageEntry{}
	for _, e := range devcheck.WorkspaceCoverageManifest {
		entries[strings.TrimPrefix(e.File, "github.com/wedevwork/callsheet/")] = e
	}
	files, _ := filepath.Glob(filepath.Join(root, "internal", "reale2e", "*.go"))
	n := 0
	for _, p := range append(files, filepath.Join(root, "internal", "devcheck", "platform.go")) {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		n++
		rel, _ := filepath.Rel(root, p)
		e, ok := entries[filepath.ToSlash(rel)]
		if !ok || e.Group != devcheck.GroupChanged || e.OS != "" || len(e.Ranges) != 0 {
			t.Errorf("%s is not a whole-file changed coverage entry for both hosts: %+v", rel, e)
		}
	}
	if n < 10 {
		t.Fatalf("only %d production files found", n)
	}
	if _, ok := entries["cmd/reale2e/main.go"]; ok {
		t.Error("the tagged forwarding entry is listed for coverage")
	}
	// Invariants observed through the existing gates.
	if got := len(devcheck.NativeRequiredTests()); got != 429 {
		t.Fatalf("native inventory %d, want 429", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := devcheck.StressSteps(goos)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			if strings.Contains(strings.Join(s.Argv, " "), "reale2e") {
				t.Fatalf("stress step %s selects reale2e", s.Name)
			}
		}
	}
	shippedTemplates(t, root)
}

// buildConstraint returns a file's //go:build expression.
func buildConstraint(f *ast.File) constraint.Expr {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				x, _ := constraint.Parse(c.Text)
				return x
			}
		}
	}
	return nil
}

// shippedTemplates checks the docs/examples contracts (UT-8).
func shippedTemplates(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(ExamplesDir))
	tpl, err := loadTemplates(dir)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := ParseFlow(tpl[FlowFile])
	if err != nil || flow.Kind() != FlowDefault {
		t.Fatalf("shipped flow %+v %v", flow, err)
	}
	all := map[string]string{}
	for _, p := range Placeholders {
		all[p] = "<" + p + ">"
	}
	manualBytes := 0
	for _, h := range Hops {
		for _, k := range []string{"instruction", "runbook"} {
			n := manualName(h, k)
			if _, err := renderTemplate(n, tpl[n], map[string]string{"RUN_ID": "r"}); err != nil {
				t.Errorf("manual %s: %v", n, err)
			}
			if h == HopCodeReviewer {
				manualBytes += len(tpl[n])
			}
		}
		runbook := string(tpl[manualName(h, "runbook")])
		for _, m := range hopMarkers[h] {
			if !strings.Contains(runbook, m) {
				t.Errorf("%s runbook lacks %q", h, m)
			}
		}
	}
	// The largest goal the plane admits, the reviewer's manuals and the
	// envelope fit the 32 KiB Grok prompt.
	if contract.MaxGoalBytes+contract.MaxAcceptanceBytes+manualBytes+grokEnvelopeAllowance > adapter.MaxGrokPromptBytes+contract.MaxAcceptanceBytes {
		t.Fatalf("the reviewer's manuals leave no room: %d bytes", manualBytes)
	}
	prompt, err := renderTemplate(CoordinatorPrompt, tpl[CoordinatorPrompt], all)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"run_in_background: true", `never pass "wait" or "override"`, "<RUNTIME>", "continue after recorded decision",
		"<RECEIPT>", "sha256sum", "BEGIN FILE", "END FILE", "task_wait once", "never run the finish command"} {
		if !bytes.Contains(prompt, []byte(want)) {
			t.Errorf("the coordinator prompt lacks %q", want)
		}
	}
	for i, h := range Hops {
		if !bytes.Contains(prompt, []byte(`callsheet-real-e2e run=<RUN_ID> hop=`+h)) || !bytes.Contains(prompt, []byte("<WAITS_DIR>/"+hopDir(i)+"/exit-code")) {
			t.Errorf("the coordinator prompt lacks hop %s's marker or wait record", h)
		}
	}
	for _, p := range Placeholders {
		if p != "OWNER_DIR" && !bytes.Contains(tpl[CoordinatorPrompt], []byte("{{"+p+"}}")) {
			t.Errorf("the coordinator prompt never uses %s", p)
		}
	}
	// The runbook is the single entry point: it links every shipped file
	// and covers the required topics.
	doc, err := os.ReadFile(filepath.Join(root, "docs", "real-e2e.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range TemplateNames() {
		if !bytes.Contains(doc, []byte("examples/real-e2e/"+n)) {
			t.Errorf("docs/real-e2e.md does not link %s", n)
		}
	}
	for _, want := range []string{"## Costs and bounds", "## Prerequisites", "## Roles, models and efforts", "## The feature", "## Running it", "## Your decision",
		"## Finishing and evidence", "## Failure, rerun and cleanup", "## Limitations", EnvOptIn + "=1", "reale2e check --evidence", "reale2e finish --run",
		"--strict-mcp-config", "--bare", "transcript.jsonl", "event-map.json", "setup-attestation.json", "coordinator-exit.json", "yes <run-id>"} {
		if !bytes.Contains(doc, []byte(want)) {
			t.Errorf("docs/real-e2e.md lacks %q", want)
		}
	}
	for _, schema := range []string{AttestationSchema, EventMapSchema, CoordExitSchema} {
		if !bytes.Contains(doc, []byte(schema)) {
			t.Errorf("docs/real-e2e.md lacks the %s example", schema)
		}
	}
}

// TestUnitContracts (UT-8): the live wiring, flow validation, strict JSON
// and small helpers.
func TestUnitContracts(t *testing.T) {
	t.Parallel()
	w, release, err := LiveWorld(Host{GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if w.TempRoot != "/tmp" || w.Poll <= 0 || w.Cwd == "" || w.Executable == "" || w.Context == nil {
		t.Fatalf("live world %+v", w)
	}
	release()
	if _, _, _, err := w.Connect("https://127.0.0.1:1", []byte("not a certificate")); err == nil {
		t.Fatal("a bad CA connected")
	}
	ca, err := testkit.NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	{
		pl, pu, closer, err := w.Connect("https://127.0.0.1:1", ca.CertPEM)
		if err != nil {
			t.Fatal(err)
		}
		defer closer()
		if _, err := pl.ListRoles(t.Context()); err == nil {
			t.Fatal("an unreachable plane answered")
		}
		if err := pu.PullResult(t.Context(), hexID("t_", 1), t.TempDir()); err == nil {
			t.Fatal("an unreachable plane pulled")
		}
	}
	if env := isolatedGitEnv([]string{"PATH=/bin", "HOME=/h"}); !slices.Equal(env, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/bin"}) {
		t.Fatal(env)
	}
	// Flow validation reuses the adapters' selection policy.
	for name, mutate := range map[string]func(f *Flow){
		"schema":  func(f *Flow) { f.Schema = "x" },
		"count":   func(f *Flow) { f.Roles = f.Roles[:3] },
		"order":   func(f *Flow) { f.Roles[0], f.Roles[1] = f.Roles[1], f.Roles[0] },
		"adapter": func(f *Flow) { f.Roles[3].Adapter = "claude" },
		"effort":  func(f *Flow) { f.Roles[1].Effort = "ultra" },
		"model":   func(f *Flow) { f.Roles[2].Model = " " },
	} {
		f := DefaultFlow()
		mutate(&f)
		if f.Validate() == nil {
			t.Errorf("%s: invalid flow accepted", name)
		}
	}
	over := DefaultFlow()
	over.Roles[2].Effort = "max"
	if over.Validate() != nil || over.Kind() != FlowOverride {
		t.Fatal("an override flow is not labelled")
	}
	short := DefaultFlow()
	short.Roles = short.Roles[:2]
	if short.Kind() != FlowOverride {
		t.Fatal("a short flow is the default")
	}
	if _, ok := over.Role("planner"); ok {
		t.Fatal("unknown role found")
	}
	if _, err := ParseFlow(bytes.Repeat([]byte(" "), maxFlowBytes+1)); err == nil {
		t.Fatal("an oversized flow parsed")
	}
	if _, err := ParseFlow([]byte(`{"schema":"x"}{}`)); err == nil {
		t.Fatal("trailing data parsed")
	}
	// Strict JSON.
	var v struct{ A int }
	for _, bad := range []string{"\xff", `{"A":1,"A":2}`, `{"A":1} 2`, `{"B":1}`, `{"A":[1,{"x":1,"x":2}]}`, `{`, `{"A":1`} {
		if err := decodeStrict([]byte(bad), &v); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
	if err := checkDuplicateKeys([]byte(`[{"a":1},{"a":2}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := encodeJSON(func() {}); err == nil {
		t.Fatal("a function encoded")
	}
	// Small helpers.
	if lastNonEmptyLine("a\nb  \r\n\n  \n") != "b" || lastNonEmptyLine("") != "" || finalMarker(HopCoder, "x") != "" {
		t.Fatal("markers")
	}
	if hopIndex("x") != -1 || within("/a/b", "/a/b/c") || !within("/a/b/c", "/a/b") {
		t.Fatal("helpers")
	}
	if _, err := newRunID(time.Now(), failingReader{}); err == nil {
		t.Fatal("a run ID without entropy")
	}
	if id, err := newRunID(time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC), &countingRand{}); err != nil || id != "20261010T010203Z-01020304" || !ValidRunID(id) {
		t.Fatalf("run ID %q %v", id, err)
	}
}
