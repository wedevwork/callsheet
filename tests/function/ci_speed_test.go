package function

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wedevwork/callsheet/internal/cicheck"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 02b function tests: one top-level TestCISpeed* per FP. They read
// tracked code and docs only, never contact GitHub and never run a real
// suite, stress or devcheck recursively; the one child go test runs a
// standard-library-only fixture module in a temporary directory.

// The literal deduplicated stress plan (design 02b, Stress selection), as
// sharded by design 02c: processgroup left the packages command for three
// single-CPU invocations; the function commands are unchanged.
const (
	speedPackages      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/plane"
	speedPG1           = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"
	speedPG2           = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"
	speedPG4           = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"
	speedFunction      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"
	speedPlaneSelector = "^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$"
	speedPlaneFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=" + speedPlaneSelector + " ./tests/function"
)

// speedSubtests are the direct subtests of the three stressed plane
// parents, in source order: the retained process-boundary scenarios, then
// each parent's excluded "contracts" boundary.
var speedSubtests = map[string][]string{
	"TestPlaneState":   {"paths", "persistence", "locking", "validation", "contracts"},
	"TestPlaneTLS":     {"https-only", "prelisten-validation", "bounded-shutdown", "contracts"},
	"TestPlaneReissue": {"process", "contracts"},
}

// speedContracts are the delegated contracts each parent's "contracts"
// subtest runs (and nothing else in the parent may run).
var speedContracts = map[string][]string{
	"TestPlaneState":   {"TestNativeStateContract", "TestStateFailureContract"},
	"TestPlaneTLS":     {"TestServerFailureContract"},
	"TestPlaneReissue": {"TestReissueFailureContract"},
}

// speedSelected are the eight subtests the plane stress step selects.
var speedSelected = []string{
	"TestPlaneState/paths", "TestPlaneState/persistence", "TestPlaneState/locking", "TestPlaneState/validation",
	"TestPlaneTLS/https-only", "TestPlaneTLS/prelisten-validation", "TestPlaneTLS/bounded-shutdown",
	"TestPlaneReissue/process",
}

// speedNative is the 28-name native qualification list (design 02b, Policy
// consistency), written out independently of devcheck.
var speedNative = []string{
	"TestFP6ProcessGroups", "TestFP6ProcessGroups/cooperative", "TestFP6ProcessGroups/resistant", "TestFP6ProcessGroups/leader-exits-first",
	"TestPlaneCommands",
	"TestPlaneState", "TestPlaneState/paths", "TestPlaneState/persistence", "TestPlaneState/locking", "TestPlaneState/validation", "TestPlaneState/contracts",
	"TestPlaneBind",
	"TestPlaneInit", "TestPlaneInit/issuance", "TestPlaneInit/fingerprint", "TestPlaneInit/restart-invariance",
	"TestPlaneTLS", "TestPlaneTLS/https-only", "TestPlaneTLS/prelisten-validation", "TestPlaneTLS/bounded-shutdown", "TestPlaneTLS/contracts",
	"TestPlaneReissue", "TestPlaneReissue/process", "TestPlaneReissue/contracts",
	"TestPlaneStatus", "TestPlaneStatus/inspection", "TestPlaneStatus/expiry-warnings",
	"TestPlanePlatform",
}

// speedJobs is the table of ordinary jobs: the two main jobs (design 02b,
// Workflow topology) and the six stress workers that replaced its two
// stress jobs (design 02c). The two summaries that keep the stress
// contexts are checked by TestStressShardSummaries.
var speedJobs = []struct {
	id, name, runner, timeout string
	checks                    []string
}{
	{"linux", "ci-linux", "ubuntu-24.04", "45", []string{"go run ./cmd/devcheck test", "go run ./cmd/devcheck coverage", "go run ./cmd/devcheck bench", "go run ./cmd/devcheck cross"}},
	{"macos", "ci-macos", "macos-15", "30", []string{"go run ./cmd/devcheck native"}},
	{"linux-stress-packages", "ci-linux-stress-packages", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-packages"}},
	{"linux-stress-processgroup", "ci-linux-stress-processgroup", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-processgroup"}},
	{"linux-stress-functions", "ci-linux-stress-functions", "ubuntu-24.04", "20", []string{"go run ./cmd/devcheck stress-functions"}},
	{"macos-stress-packages", "ci-macos-stress-packages", "macos-15", "20", []string{"go run ./cmd/devcheck stress-packages"}},
	{"macos-stress-processgroup", "ci-macos-stress-processgroup", "macos-15", "20", []string{"go run ./cmd/devcheck stress-processgroup"}},
	{"macos-stress-functions", "ci-macos-stress-functions", "macos-15", "20", []string{"go run ./cmd/devcheck stress-functions"}},
}

// tRunName returns the literal name and body of a t.Run("name", func...)
// expression statement, or ok=false.
func tRunName(st ast.Stmt) (string, *ast.FuncLit, bool) {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return "", nil, false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return "", nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Run" {
		return "", nil, false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "t" {
		return "", nil, false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	fn, fok := call.Args[1].(*ast.FuncLit)
	if !ok || !fok || lit.Kind != token.STRING {
		return "", nil, false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", nil, false
	}
	return name, fn, true
}

// delegatedCall reports whether call is a delegated contract invocation
// (planeContract) or a contract build (testkit.BuildTestBinary), and for a
// planeContract call the contract name.
func delegatedCall(call *ast.CallExpr) (kind, contract string) {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		if f.Name == "planeContract" && len(call.Args) >= 4 {
			if lit, ok := call.Args[3].(*ast.BasicLit); ok {
				name, _ := strconv.Unquote(lit.Value)
				return "planeContract", name
			}
			return "planeContract", ""
		}
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok && x.Name == "testkit" && f.Sel.Name == "BuildTestBinary" {
			return "BuildTestBinary", ""
		}
	}
	return "", ""
}

// FP-1: the exact deduplicated selection, the real test organization it
// relies on, and the selector's execution semantics on a fixture.
func TestCISpeedSelection(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := devcheck.StressSteps(goos)
		if err != nil || len(steps) != 6 {
			t.Fatalf("%s: %+v %v", goos, steps, err)
		}
		for i, want := range []struct{ name, argv string }{{"stress packages", speedPackages}, {"stress processgroup cpu1", speedPG1},
			{"stress processgroup cpu2", speedPG2}, {"stress processgroup cpu4", speedPG4}, {"stress function", speedFunction}, {"stress plane function", speedPlaneFunction}} {
			if steps[i].Name != want.name || strings.Join(steps[i].Argv, " ") != want.argv || strings.Join(steps[i].Env, " ") != "CGO_ENABLED=1" {
				t.Fatalf("%s step %d = %+v, want %s: %s", goos, i, steps[i], want.name, want.argv)
			}
		}
		// FP-6 is no longer repeated by a function step; its experiment
		// stays repeated through the complete processgroup package.
		for _, s := range steps[4:] {
			if strings.Contains(strings.Join(s.Argv, " "), "TestFP6ProcessGroups") {
				t.Fatalf("%s: %s still selects TestFP6ProcessGroups", goos, s.Name)
			}
		}
		for _, s := range steps[1:4] {
			if !slices.Contains(s.Argv, "./internal/spikes/processgroup") || len(s.Argv) != 7 {
				t.Fatalf("%s %s lost the complete processgroup package: %v", goos, s.Name, s.Argv)
			}
		}
		if !slices.Contains(steps[0].Argv, "./internal/plane") {
			t.Fatalf("%s packages step lost plane: %v", goos, steps[0].Argv)
		}
	}

	// The real plane tests: exact direct subtests, and every delegated
	// contract call and contract build inside the contracts callbacks only.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(testkit.MustRepoRoot(t), "tests", "function", "plane_trust_test.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || speedSubtests[fd.Name.Name] == nil {
			continue
		}
		parent := fd.Name.Name
		found[parent] = true
		var subs []string
		bodies := map[string]*ast.FuncLit{}
		for _, st := range fd.Body.List {
			if name, fn, ok := tRunName(st); ok {
				subs = append(subs, name)
				bodies[name] = fn
			}
		}
		if !slices.Equal(subs, speedSubtests[parent]) {
			t.Fatalf("%s subtests = %q, want %q", parent, subs, speedSubtests[parent])
		}
		var contracts []string
		builds := 0
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			kind, contract := delegatedCall(call)
			if kind == "" {
				return true
			}
			c := bodies["contracts"]
			if call.Pos() < c.Pos() || call.End() > c.End() {
				t.Fatalf("%s: %s at %s is outside its contracts subtest", parent, kind, fset.Position(call.Pos()))
			}
			if kind == "BuildTestBinary" {
				builds++
			} else {
				contracts = append(contracts, contract)
			}
			return true
		})
		if builds != 1 || !slices.Equal(contracts, speedContracts[parent]) {
			t.Fatalf("%s contracts subtest: %d builds, contracts %q, want 1 build and %q", parent, builds, contracts, speedContracts[parent])
		}
	}
	if len(found) != 3 {
		t.Fatalf("plane parents found: %v", found)
	}

	// Execute the actual selector once against a standard-library-only
	// fixture with the same names, near-prefix neighbours and FP-6.
	var selector string
	steps, _ := devcheck.StressSteps(runtime.GOOS)
	for _, a := range steps[5].Argv {
		if v, ok := strings.CutPrefix(a, "-run="); ok {
			selector = v
		}
	}
	if selector != speedPlaneSelector {
		t.Fatalf("host plane selector = %q", selector)
	}
	ran, logged := runSelectorFixture(t, selector)
	var wantRan []string
	for _, p := range []string{"TestPlaneState", "TestPlaneTLS", "TestPlaneReissue"} {
		wantRan = append(wantRan, p)
		for _, s := range speedSelected {
			if strings.HasPrefix(s, p+"/") {
				wantRan = append(wantRan, s)
			}
		}
	}
	slices.Sort(wantRan)
	if !slices.Equal(ran, wantRan) {
		t.Fatalf("fixture passed %q, want exactly %q", ran, wantRan)
	}
	wantLog := []string{"setup TestPlaneState"}
	for _, s := range speedSelected {
		if s == "TestPlaneTLS/https-only" {
			wantLog = append(wantLog, "setup TestPlaneTLS")
		}
		if s == "TestPlaneReissue/process" {
			wantLog = append(wantLog, "setup TestPlaneReissue")
		}
		wantLog = append(wantLog, "child "+s)
	}
	if !slices.Equal(logged, wantLog) {
		t.Fatalf("fixture log %q, want %q", logged, wantLog)
	}
}

// selectorFixture mirrors the plane parents' subtest names (contracts
// included), plus FP-6, near-prefix and unrelated tests. Every parent logs
// its setup and every child its run to $SELECTOR_FIXTURE_LOG.
const selectorFixture = `package fixture

import (
	"os"
	"testing"
)

func record(t *testing.T, line string) {
	f, err := os.OpenFile(os.Getenv("SELECTOR_FIXTURE_LOG"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func parent(t *testing.T, subs ...string) {
	record(t, "setup "+t.Name())
	for _, s := range subs {
		t.Run(s, func(t *testing.T) { record(t, "child "+t.Name()) })
	}
}

func TestPlaneState(t *testing.T) {
	parent(t, "paths", "persistence", "locking", "validation", "contracts")
}
func TestPlaneTLS(t *testing.T) {
	parent(t, "https-only", "prelisten-validation", "bounded-shutdown", "contracts")
}
func TestPlaneReissue(t *testing.T)    { parent(t, "process", "contracts") }
func TestFP6ProcessGroups(t *testing.T) { parent(t, "cooperative", "resistant", "leader-exits-first") }
func TestPlaneStatus(t *testing.T)     { parent(t, "inspection", "expiry-warnings", "process", "paths") }
func TestPlaneStateExtra(t *testing.T) { parent(t, "paths", "process") }
func TestPlaneInit(t *testing.T)       { parent(t, "issuance", "fingerprint", "restart-invariance") }
func TestFP4TransportHarness(t *testing.T) { parent(t, "paths") }
`

// runSelectorFixture runs go test -json -run=selector once in a temporary
// fixture module and returns the sorted tests with a pass event (after
// checking each has a run event and nothing failed or skipped) and the
// fixture's setup/child log lines in execution order.
func runSelectorFixture(t *testing.T, selector string) (passed, logged []string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":           "module example.com/selectorfixture\n\ngo 1.22\n",
		"selector_test.go": selectorFixture,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(t.TempDir(), "fixture.log")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, testkit.GoTool(), "test", "-json", "-count=1", "-run="+selector, ".")
	cmd.Dir = dir
	// The fixture has no dependencies: inherited build flags and workspaces
	// are dropped, the toolchain stays local and nothing is downloaded.
	cmd.Env = testkit.EnvWithout(os.Environ(), []string{"GOFLAGS", "GOWORK", "GOTOOLCHAIN", "SELECTOR_FIXTURE_LOG"},
		"GOWORK=off", "GOTOOLCHAIN=local", "SELECTOR_FIXTURE_LOG="+logPath)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture go test: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	runs := map[string]bool{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var ev struct{ Action, Test string }
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("fixture events: %v", err)
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "run":
			runs[ev.Test] = true
		case "pass":
			if !runs[ev.Test] {
				t.Fatalf("fixture: pass without run for %s", ev.Test)
			}
			passed = append(passed, ev.Test)
		case "fail", "skip":
			t.Fatalf("fixture: %s %s", ev.Action, ev.Test)
		}
	}
	for name := range runs {
		if !slices.Contains(passed, name) {
			t.Fatalf("fixture: %s ran without passing", name)
		}
	}
	slices.Sort(passed)
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fixture log: %v", err)
	}
	return passed, strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// FP-2: the actual workflow's main jobs and, since iteration 02c, its six
// stress workers are independent, with identical pinned setup and exact
// check commands; the two summaries follow them.
func TestCISpeedJobs(t *testing.T) {
	data := ciWorkflow(t)
	if err := cicheck.ValidateWorkflow(data); err != nil {
		t.Fatalf("checked-in workflow: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	jobs := node(t, &doc, "jobs")
	var ids []string
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		ids = append(ids, jobs.Content[i].Value)
	}
	if strings.Join(ids, " ") != "linux macos linux-stress-packages linux-stress-processgroup linux-stress-functions "+
		"macos-stress-packages macos-stress-processgroup macos-stress-functions linux-stress macos-stress" {
		t.Fatalf("job ids = %v", ids)
	}
	var names []string
	var setup string
	for _, j := range speedJobs {
		job := node(t, &doc, "jobs", j.id)
		var keys []string
		for i := 0; i+1 < len(job.Content); i += 2 {
			keys = append(keys, job.Content[i].Value)
		}
		slices.Sort(keys)
		// No needs, condition, matrix, concurrency, bypass or permission
		// override: exactly the contract's keys.
		if strings.Join(keys, " ") != "defaults env name runs-on steps timeout-minutes" {
			t.Fatalf("%s keys = %v", j.id, keys)
		}
		if node(t, job, "name").Value != j.name || node(t, job, "runs-on").Value != j.runner || node(t, job, "timeout-minutes").Value != j.timeout {
			t.Fatalf("%s identity differs from the table", j.id)
		}
		names = append(names, j.name)
		steps := node(t, job, "steps")
		if len(steps.Content) != 3+len(j.checks) {
			t.Fatalf("%s has %d steps", j.id, len(steps.Content))
		}
		// Identical pinned setup in every job.
		var b bytes.Buffer
		enc := yaml.NewEncoder(&b)
		for i := 0; i < 3; i++ {
			if err := enc.Encode(steps.Content[i]); err != nil {
				t.Fatal(err)
			}
		}
		if setup == "" {
			setup = b.String()
		} else if b.String() != setup {
			t.Fatalf("%s setup differs:\n%s\nwant\n%s", j.id, b.String(), setup)
		}
		if co, sg := node(t, steps, 0, "uses"), node(t, steps, 1, "uses"); co.Value != checkoutPin || co.LineComment != "# v6.0.2" || sg.Value != setupGoPin || sg.LineComment != "# v6.3.0" {
			t.Fatalf("%s pins = %s %s", j.id, co.Value, sg.Value)
		}
		for i, want := range j.checks {
			st := steps.Content[3+i]
			if node(t, st, "run").Value != want || node(t, st, "env", "GOPROXY").Value != "off" || node(t, st, "env", "GOSUMDB").Value != "off" ||
				len(node(t, st, "env").Content) != 4 {
				t.Fatalf("%s check %d differs from %q", j.id, i, want)
			}
		}
	}
	if strings.Join(names[:2], ",") != strings.Join(cicheck.RequiredChecks()[:2], ",") || len(names) != 8 {
		t.Fatalf("contexts %v, contract %v", names, cicheck.RequiredChecks())
	}
	for _, top := range []string{"concurrency", "env", "defaults"} {
		if child := func() *yaml.Node {
			root := doc.Content[0]
			for i := 0; i+1 < len(root.Content); i += 2 {
				if root.Content[i].Value == top {
					return root.Content[i+1]
				}
			}
			return nil
		}(); child != nil {
			t.Fatalf("workflow has top-level %s", top)
		}
	}
	// Each stress job removed, or a worker made dependent, is rejected; a
	// removed summary also loses its required context.
	for _, id := range []string{"linux-stress", "macos-stress"} {
		mustReject(t, id+" removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs"), id) }), "jobs."+id+": missing required field")
		mustReject(t, id+" needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id+"-packages"), "needs", "linux") }), "jobs."+id+"-packages.needs: unknown field")
		removed := mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs"), id) })
		if err := cicheck.CheckJobNames(map[string][]byte{"ci.yml": removed}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("required check %q is not defined", "ci-"+id)) {
			t.Fatalf("%s removed: job names %v", id, err)
		}
	}
	mustReject(t, "main job waits for stress", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "linux"), "needs", "linux-stress") }), "jobs.linux.needs: unknown field")
}

// qualifyingStream is a complete synthetic native stream for every
// required name except drop.
func qualifyingStream(drop string) string {
	pkg := devcheck.NativePackage
	evs := []map[string]any{synth("start", pkg, "")}
	for _, name := range speedNative {
		if name != drop {
			evs = append(evs, synth("run", pkg, name), synth("pass", pkg, name))
		}
	}
	return events(append(evs, synth("pass", pkg, ""))...)
}

// FP-3: validator, plans, native evidence and documentation agree.
func TestCISpeedPolicy(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		if got := devcheck.NativeRequiredTests(); len(got) != 28 || !slices.Equal(got, speedNative) {
			t.Fatalf("native required = %v", got)
		}
		if err := devcheck.CheckNativeResults("darwin", strings.NewReader(qualifyingStream(""))); err != nil {
			t.Fatalf("complete evidence: %v", err)
		}
		for _, name := range []string{"TestPlaneState/contracts", "TestPlaneTLS/contracts", "TestPlaneReissue/process", "TestPlaneReissue/contracts"} {
			err := devcheck.CheckNativeResults("darwin", strings.NewReader(qualifyingStream(name)))
			if err == nil || !strings.Contains(err.Error(), name+" has no run event") || !strings.Contains(err.Error(), "native qualification unobserved") {
				t.Fatalf("missing %s: %v", name, err)
			}
		}
		fixture := string(repoFile(t, "internal/devcheck/testdata/cli-go-test.jsonl"))
		if err := devcheck.CheckNativeResults("darwin", strings.NewReader(fixture)); err == nil || !strings.Contains(err.Error(), "TestPlaneReissue/contracts has no run event") {
			t.Fatalf("recorded CLI-only fixture: %v", err)
		}
	})
	t.Run("workflow", func(t *testing.T) {
		data := ciWorkflow(t)
		if err := cicheck.ValidateWorkflow(data); err != nil {
			t.Fatal(err)
		}
		stages, err := cicheck.ExtractStages(data)
		if err != nil {
			t.Fatal(err)
		}
		contract := cicheck.Jobs()
		if len(contract) != len(speedJobs)+2 || len(stages) != len(speedJobs)+2 {
			t.Fatalf("contract %d jobs, workflow %d, table %d plus two summaries", len(contract), len(stages), len(speedJobs))
		}
		for i, j := range speedJobs {
			var want []string
			for _, c := range j.checks {
				want = append(want, strings.TrimPrefix(c, "go run ./cmd/devcheck "))
			}
			c := contract[i]
			if c.ID != j.id || c.Name != j.name || c.RunsOn != j.runner || fmt.Sprint(c.TimeoutMinutes) != j.timeout ||
				!slices.Equal(c.Stages, want) || !slices.Equal(stages[j.id], want) {
				t.Fatalf("%s: contract %+v, workflow %v, want %v", j.id, c, stages[j.id], want)
			}
		}
		// The complete local stress stage dispatches the whole plan, with
		// processgroup's three invocations concurrent (compared as a set).
		r := &ciRunner{}
		if code, out, errOut := devcheckRun(t, r, "stress"); code != 0 || !strings.Contains(out, "stage stress ok") ||
			!sameGroups(r.calls, [][]string{{speedPackages}, {speedPG1, speedPG2, speedPG4}, {speedFunction}, {speedPlaneFunction}}) {
			t.Fatalf("stress dispatch = %d %v %s", code, r.calls, errOut)
		}
		// Drift: stress back in a main job is rejected.
		mustReject(t, "stress in ci-macos", mutated(t, func(r *yaml.Node) {
			steps := node(t, r, "jobs", "macos", "steps")
			steps.Content = append(steps.Content, node(t, r, "jobs", "macos-stress-packages", "steps", 3))
		}), "jobs.macos.steps[4]: unexpected extra step")
	})
	t.Run("docs", func(t *testing.T) {
		linux, _ := devcheck.StressSteps("linux")
		darwin, _ := devcheck.StressSteps("darwin")
		stress := docSection(t, "Stress checks")
		for i := range linux {
			l, d := strings.Join(linux[i].Argv, " "), strings.Join(darwin[i].Argv, " ")
			if l != d {
				t.Fatalf("plans differ at step %d: %s / %s", i, l, d)
			}
			if !strings.Contains(stress, "\n"+l+"\n") {
				t.Fatalf("Stress checks lacks the command %q", l)
			}
		}
		requireTerms(t, "Stress checks", stress, "`stress packages`", "`stress processgroup cpu1`", "`stress function`", "`stress plane function`",
			"`TestFP6ProcessGroups`", "`TestExperiment`", "`TestNativeStateContract`", "`TestStateFailureContract`",
			"`TestServerFailureContract`", "`TestReissueFailureContract`", "`contracts`", "`process`",
			"quote the entire `-run` argument")
		checks := docSection(t, "Checks")
		for _, j := range speedJobs {
			requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `%s` | %s min |", j.name, j.runner, j.timeout))
		}
		for _, name := range []string{"ci-linux-stress", "ci-macos-stress"} {
			requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `ubuntu-24.04` | 5 min |", name))
		}
		for _, name := range speedNative {
			requireTerms(t, "Checks", checks, "`"+name[strings.LastIndex(name, "/")+1:]+"`")
		}
		local := docSection(t, "Local verification")
		requireTerms(t, "Local verification", local, "go run ./cmd/devcheck all", "go run ./cmd/devcheck stress",
			"go test -json -count=1 -run '"+speedPlaneSelector+"' ./tests/function")
	})
}

// FP-4: the documented timing evidence and the owner-operated handoff for
// pull request #2. This proves instructions, not remote state.
func TestCISpeedHandoff(t *testing.T) {
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp,
		"`ci-linux`, `ci-macos`, `ci-linux-stress`, `ci-macos-stress`",
		"owner-only", "never executed by CI, tests or this flow",
		"https://docs.github.com/en/rest/branches/branch-protection#add-status-check-contexts",
		"preserving the existing contexts",
		"gh api repos/wedevwork/callsheet/branches/main/protection",
		"before and after", "all four contexts", "strict", "enforce_admins", "rulesets",
		"stop the handoff", "A green workflow alone does not prove protection")
	if !strings.Contains(bp, "\n"+ownerAddContexts) {
		t.Fatalf("Branch protection lacks the exact additive command:\n%s", ownerAddContexts)
	}
	const sub = "\n### Adding the stress contexts (pull request #2)\n"
	i := strings.Index(bp, sub)
	if i < 0 {
		t.Fatalf("Branch protection lacks %q", strings.TrimSpace(sub))
	}
	handoff := bp[i+len(sub):]
	if j := strings.Index(handoff, "\n### "); j >= 0 {
		handoff = handoff[:j]
	}
	steps := numberedSteps(t, handoff)
	review := stepIndex(t, steps, "REVIEW_APPROVED", "commit the reviewed code")
	push := stepIndex(t, steps, "Push `iter-02-plane-trust`")
	green := stepIndex(t, steps, "all ten jobs", "`ci-linux-stress`", "`ci-macos-stress`", "current PR merge revision")
	add := stepIndex(t, steps, "owner adds", "`ci-linux-stress`", "`ci-macos-stress`", "after they have reported")
	verify := stepIndex(t, steps, "owner verifies", "read-only")
	merge := stepIndex(t, steps, "Merge iterations 02, 02b and 02c together", "all four checks green")
	if !(review < push && push < green && green < add && add < verify && verify < merge) {
		t.Fatalf("handoff order review=%d push=%d green=%d add=%d verify=%d merge=%d", review, push, green, add, verify, merge)
	}
	requireTerms(t, "handoff", handoff, "skipped, canceled, pending or unobserved result qualifies",
		"later push requires fresh current-revision evidence", "pull request #1 protection handoff remains a prerequisite")
	requireTerms(t, "PR flow", docSection(t, "PR flow"), "iterations 02, 02b and 02c join pull request #2")

	// Timing provenance: the 02b-era hosted baseline, local measurement,
	// estimate and target stay as history; iteration 02c's measured hosted
	// baseline (run 36236333755) supersedes 02b's pending language.
	stress := docSection(t, "Stress checks")
	requireTerms(t, "Stress checks", stress,
		"`ci-linux` 685 s", "stress 576 s", "`ci-macos` 889 s", "stress 806 s",
		"5–6 minutes", "an optimization target, not a measurement",
		"Measured with iteration 02b (deduplicated)", "go1.26.4 linux/amd64",
		"Expected per-job wall-clock after iteration 02b", "estimate", "806 s of the 900 s watchdog",
		"since measured by run 36236333755")
	if strings.Contains(stress, "pending until the first `ci-macos-stress` run of pull request #2") {
		t.Fatal("Stress checks keeps 02b's superseded pending first-run language")
	}
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "critical path", "actual job and step times",
		"compare each worker with the expected per-job wall-clock")
	// Nothing in the workflow can change repository settings.
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "--method", "POST", "curl", "protection", "contents: write", "secrets."} {
		if strings.Contains(wf, forbidden) {
			t.Fatalf("workflow contains %q", forbidden)
		}
	}
}
