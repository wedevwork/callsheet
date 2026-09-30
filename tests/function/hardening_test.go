package function

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wedevwork/callsheet/internal/cicheck"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 01c function tests: one top-level TestHardening* per FP. They
// read the repository through testkit.MustRepoRoot (never design files),
// inject runners into devcheck, and run package-private contract tests by
// name in compiled package test binaries. They never invoke a full suite,
// stress or devcheck recursively with ExecRunner.

// The literal stress plan and repeat count are the specification oracle.
// Iteration 02 appended ./internal/plane and, by design 02's
// pre-authorized function-binary split, the separate plane function step;
// iteration 02b removed the duplicated FP-6 experiment and the delegated
// plane contracts from the function steps; iteration 02c moved
// processgroup into its own shard of three single-CPU invocations,
// iteration 05b moved ./internal/plane into one likewise, and its sidecar
// follow-up ./internal/sidecar.
const (
	hardeningStressCount    = 20
	hardeningStressPackages = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/contract ./internal/adapter ./internal/mcp ./internal/mcpqual ./internal/workspace"
	hardeningStressPlane1   = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane"
	hardeningStressPlane2   = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/plane"
	hardeningStressPlane4   = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/plane"
	hardeningStressSidecar1 = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar"
	hardeningStressSidecar2 = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar"
	hardeningStressSidecar4 = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar"
	hardeningStressPG1      = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"
	hardeningStressPG2      = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"
	hardeningStressPG4      = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"
	hardeningStressFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"
	hardeningStressPlaneFn  = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function"
	// Iteration 03 appended the node packages and the node function step.
	hardeningStressNodeFn = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"
)

// hardeningPlan is the flattened stress plan in shard/CPU order.
var hardeningPlan = []struct{ name, argv string }{
	{"stress packages", hardeningStressPackages},
	{"stress plane cpu1", hardeningStressPlane1}, {"stress plane cpu2", hardeningStressPlane2}, {"stress plane cpu4", hardeningStressPlane4},
	{"stress sidecar cpu1", hardeningStressSidecar1}, {"stress sidecar cpu2", hardeningStressSidecar2}, {"stress sidecar cpu4", hardeningStressSidecar4},
	{"stress processgroup cpu1", hardeningStressPG1}, {"stress processgroup cpu2", hardeningStressPG2}, {"stress processgroup cpu4", hardeningStressPG4},
	{"stress function", hardeningStressFunction}, {"stress plane function", hardeningStressPlaneFn}, {"stress node function", hardeningStressNodeFn},
}

// hardeningSelectors are the exact -run values of the stress plan with the
// test names each one selects, written out explicitly: the plane selector
// is two levels (parents, then subtests), not a flat alternation.
var hardeningSelectors = map[string][]string{
	"^(TestFP4TransportHarness|TestFP5GitRoundTrip)$": {"TestFP4TransportHarness", "TestFP5GitRoundTrip"},
	"^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$": {
		"TestPlaneState", "TestPlaneTLS", "TestPlaneReissue",
		"paths", "persistence", "locking", "validation", "https-only", "prelisten-validation", "bounded-shutdown", "process"},
	"^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$": {"TestNodeEnrollment", "TestNodeReconnect", "locking", "shutdown"},
}

// contractRun runs only the tests matching run in bin, pkg's compiled test
// binary (testkit.BuildTestBinary), in the package directory. It requires
// exit status 0 and a PASS line for every name in pass, and rejects skips,
// failures and empty runs: a zero exit with no matching test is not
// evidence.
func contractRun(t *testing.T, bin, pkg, run string, env []string, pass ...string) string {
	t.Helper()
	root := testkit.MustRepoRoot(t)
	cmd := exec.Command(bin, "-test.run="+run, "-test.v", "-test.count=1", "-test.timeout=170s")
	cmd.Dir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("%s did not run: %v", pkg, err)
	}
	if err != nil {
		t.Fatalf("%s -test.run=%s failed (%v):\n%s", pkg, run, err, out.String())
	}
	o := out.String()
	for _, bad := range []string{"--- SKIP", "--- FAIL", "testing: warning: no tests to run"} {
		if strings.Contains(o, bad) {
			t.Fatalf("%s -test.run=%s: %q in output:\n%s", pkg, run, bad, o)
		}
	}
	for _, name := range pass {
		if !strings.Contains(o, "--- PASS: "+name+" (") {
			t.Fatalf("%s: no PASS line for %s:\n%s", pkg, name, o)
		}
	}
	return o
}

// FP-1: the registered, bounded stress stage and its exact plans.
func TestHardeningStress(t *testing.T) {
	if devcheck.StressCount != hardeningStressCount {
		t.Fatalf("StressCount = %d", devcheck.StressCount)
	}
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := devcheck.StressSteps(goos)
		if err != nil || len(steps) != len(hardeningPlan) {
			t.Fatalf("%s plan = %+v %v", goos, steps, err)
		}
		for i, want := range hardeningPlan {
			if steps[i].Name != want.name || strings.Join(steps[i].Argv, " ") != want.argv || strings.Join(steps[i].Env, " ") != "CGO_ENABLED=1" {
				t.Fatalf("%s step %d = %+v", goos, i, steps[i])
			}
		}
	}
	if _, err := devcheck.StressSteps("windows"); err == nil {
		t.Fatal("windows stress plan accepted")
	}
	for _, st := range []string{"stress", "stress-packages", "stress-plane-cpu1", "stress-plane", "stress-sidecar-cpu1", "stress-sidecar", "stress-processgroup", "stress-functions"} {
		if !strings.Contains(" "+strings.Join(devcheck.Stages(), " ")+" ", " "+st+" ") {
			t.Fatalf("%s not advertised: %v", st, devcheck.Stages())
		}
	}
	// The native host dispatches every command, shards in order, the plane
	// and sidecar CPU1 singletons alone, the plane and sidecar CPU2/CPU4
	// pairs and processgroup's three concurrently (design 06a-perf), with
	// the parent environment plus CGO_ENABLED=1.
	r := &ciRunner{}
	code, out, errOut := devcheckRun(t, r, "stress")
	if code != 0 || !strings.Contains(out, "stage stress ok") ||
		!sameGroups(r.calls, [][]string{{hardeningStressPackages}, {hardeningStressPlane1}, {hardeningStressPlane2, hardeningStressPlane4},
			{hardeningStressSidecar1}, {hardeningStressSidecar2, hardeningStressSidecar4},
			{hardeningStressPG1, hardeningStressPG2, hardeningStressPG4}, {hardeningStressFunction}, {hardeningStressPlaneFn}, {hardeningStressNodeFn}}) {
		t.Fatalf("%s stress = %d %v %s", runtime.GOOS, code, r.calls, errOut)
	}
	for i, env := range r.envs {
		if env[len(env)-1] != "CGO_ENABLED=1" || !strings.Contains(strings.Join(env, "\n"), "PATH=") {
			t.Fatalf("call %d env = %v", i, env)
		}
	}
	// Fail fast: a failing command or shard never starts the next, and a
	// failing last command fails the stage.
	for _, c := range []struct {
		failOn, step string
		calls        int
	}{{"./internal/spikes/gittransport", "stress packages", 1}, {"-cpu=1 -timeout=6m ./internal/plane", "stress plane cpu1", 2},
		{"-cpu=4 -timeout=6m ./internal/plane", "stress plane cpu4", 4}, {"-cpu=1 -timeout=6m ./internal/sidecar", "stress sidecar cpu1", 5},
		{"-cpu=4 -timeout=6m ./internal/sidecar", "stress sidecar cpu4", 7},
		{"-cpu=4 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu4", 10}, {"TestFP5GitRoundTrip", "stress function", 11},
		{"TestPlaneTLS", "stress plane function", 12}, {"TestNodeReconnect", "stress node function", 13}} {
		r := &ciRunner{failOn: c.failOn}
		code, out, errOut := devcheckRun(t, r, "stress")
		if code != 1 || len(r.calls) != c.calls || !strings.Contains(errOut, "stage stress FAILED: "+c.step+" failed") ||
			!strings.Contains(errOut, "injected child failure") || strings.Contains(out, "stage stress ok") {
			t.Fatalf("fail %s = %d calls=%d %s", c.failOn, code, len(r.calls), errOut)
		}
	}
	// Usage errors and all's unchanged sequence.
	for _, args := range [][]string{{"stress", "extra"}, {"stress", "-o", "x"}, {"stress-plane-cpu1", "-cpu=1"}, {"stress-plane-cpu1", "extra"}, {"stress-plane", "-count=1"},
		{"stress-sidecar-cpu1", "-count=1"}, {"stress-sidecar-cpu1", "-o", "x"}, {"stress-sidecar", "-cpu=4"}, {"stress-processgroup", "-cpu=1"}, {"stress-functions", "-count=1"}} {
		r := &ciRunner{}
		if code, _, _ := devcheckRun(t, r, args...); code != 2 || len(r.calls) != 0 {
			t.Fatalf("%v = %d", args, code)
		}
	}
	r = &ciRunner{coverTotal: "81.0%"}
	if code, _, errOut := devcheckRun(t, r, "all"); code != 0 {
		t.Fatalf("all = %d %s", code, errOut)
	}
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), "-count=20") {
			t.Fatalf("all ran stress: %v", c)
		}
	}
}

// FP-2: every seam's contract test runs linux and darwin subtests on this
// host, without foreign syscalls.
func TestHardeningPlatformSeams(t *testing.T) {
	for _, pkg := range []string{"./internal/cli", "./internal/devcheck", "./internal/spikes/processgroup", "./internal/testkit/fakeadapter"} {
		t.Run(filepath.Base(pkg), func(t *testing.T) {
			bin := testkit.BuildTestBinary(t, pkg, filepath.Base(pkg))
			contractRun(t, bin, pkg, "^TestPlatformSeamContract$", os.Environ(),
				"TestPlatformSeamContract", "TestPlatformSeamContract/linux", "TestPlatformSeamContract/darwin")
		})
	}
}

// validGuardTree is a complete miniature source tree satisfying the fixed
// production guard policy: the six wrappers (iteration 07b added
// cmd/mcpqual's) and the four exempt files.
func validGuardTree() map[string]string {
	return map[string]string{
		"internal/cli/cli.go": "package cli\n\nimport (\n\t\"context\"\n\t\"io\"\n\t\"runtime\"\n)\n\n" +
			"func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {\n\treturn runFor(ctx, runtime.GOOS, args, in, out, errOut)\n}\n",
		"internal/devcheck/devcheck.go": "package devcheck\n\nimport (\n\t\"context\"\n\t\"io\"\n\t\"runtime\"\n)\n\n" +
			"func Run(ctx context.Context, args []string, out, errOut io.Writer, run Runner) int {\n\treturn runFor(ctx, runtime.GOOS, args, out, errOut, run)\n}\n",
		"internal/spikes/processgroup/experiment.go": "//go:build linux || darwin\n\npackage processgroup\n\nimport \"runtime\"\n\n" +
			"func evaluate(r *CaseResult) { evaluateFor(r, runtime.GOOS) }\n\n" +
			"func RunHelper(getenv func(string) string) int {\n\treturn runHelperFor(getenv, runtime.GOOS, runtime.GOARCH)\n}\n",
		"internal/testkit/fakeadapter/fakeadapter.go": "package fakeadapter\n\nimport \"runtime\"\n\n" +
			"func Parse(args []string) (Options, error) {\n\treturn parseFor(args, runtime.GOOS, signalsSupported)\n}\n",
		"cmd/mcpqual/main.go": "package main\n\nimport (\n\t\"io\"\n\t\"runtime\"\n)\n\n" +
			"func run(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {\n" +
			"\treturn runFor(runtime.GOOS, runtime.GOARCH, args, getenv, stdin, stdout, stderr)\n}\n",
		"internal/spikes/processgroup/sys_linux.go":     "//go:build linux\n\npackage processgroup\n",
		"internal/spikes/processgroup/sys_darwin.go":    "//go:build darwin\n\npackage processgroup\n",
		"internal/testkit/fakeadapter/signals_unix.go":  "//go:build linux || darwin\n\npackage fakeadapter\n",
		"internal/testkit/fakeadapter/signals_other.go": "//go:build !linux && !darwin\n\npackage fakeadapter\n",
	}
}

func writeGuardTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		mkdir(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// FP-3: the guard accepts the real tree and a complete valid fixture, and
// rejects an added unapproved read in a file of either platform on any host.
func TestHardeningPlatformGuard(t *testing.T) {
	if err := devcheck.CheckPlatformSources(testkit.MustRepoRoot(t)); err != nil {
		t.Fatalf("repository: %v", err)
	}
	if err := devcheck.CheckPlatformSources(writeGuardTree(t, validGuardTree())); err != nil {
		t.Fatalf("valid fixture: %v", err)
	}
	for _, c := range []struct{ file, tag string }{
		{"internal/extra/host_darwin.go", "darwin"},
		{"internal/extra/host_linux.go", "linux"},
	} {
		files := validGuardTree()
		files[c.file] = "//go:build " + c.tag + "\n\npackage extra\n\nimport \"runtime\"\n\nfunc host() string { return runtime.GOOS }\n"
		err := devcheck.CheckPlatformSources(writeGuardTree(t, files))
		want := c.file + ":7:29: unapproved host OS read runtime.GOOS in func host"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s on %s: %v, want %q", c.file, runtime.GOOS, err, want)
		}
		// Exactly that violation: a missing wrapper cannot satisfy this.
		if strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "1 violation(s)") {
			t.Fatalf("%s: unexpected violations: %v", c.file, err)
		}
	}
}

// FP-4: the independent literal-address oracle and the real SAN-mismatch
// scenario both run and pass.
func TestHardeningSANOracle(t *testing.T) {
	const pkg = "./internal/spikes/gittransport"
	bin := testkit.BuildTestBinary(t, pkg, "gittransport")
	out := contractRun(t, bin, pkg, "^TestSANMismatchHostOracle$", os.Environ(),
		"TestSANMismatchHostOracle", "TestSANMismatchHostOracle/127.0.0.1:1234", "TestSANMismatchHostOracle/127.0.0.2:1234",
		"TestSANMismatchHostOracle/[::1]:1234", "TestSANMismatchHostOracle/not_an_address")
	if strings.Contains(out, "TestGitScenarios") {
		t.Fatalf("oracle run executed other tests:\n%s", out)
	}
	out = contractRun(t, bin, pkg, "^TestGitScenarios$/^tls-san-mismatch$",
		testkit.EnvWithout(os.Environ(), testkit.ProxyVars),
		"TestGitScenarios", "TestGitScenarios/tls-san-mismatch")
	if strings.Contains(out, "=== RUN   TestGitScenarios/seed-and-clone") {
		t.Fatalf("selector ran other scenarios:\n%s", out)
	}
}

// FP-5: scanner failures in either signal log fail the case; normal logs
// still pass.
func TestHardeningSignalEvidence(t *testing.T) {
	var names []string
	for _, sub := range []string{"normal", "leader", "descendant"} {
		for _, goos := range []string{"linux", "darwin"} {
			names = append(names, "TestSignalEvidenceContract/"+sub+"/"+goos)
		}
	}
	const pkg = "./internal/spikes/processgroup"
	contractRun(t, testkit.BuildTestBinary(t, pkg, "processgroup"), pkg, "^TestSignalEvidenceContract$", os.Environ(),
		append([]string{"TestSignalEvidenceContract"}, names...)...)
}

// FP-6: since iteration 02c the stress workers (ten since iteration 05b's
// sidecar follow-up, fourteen since design 06a-perf's CPU1 workers)
// run their exact shard step as their step 3 (the summaries
// ci-linux-stress and ci-macos-stress keep the contexts), the main jobs
// keep their stages without stress,
// removing any check step fails validation, and the advertised stages
// dispatch.
func TestHardeningCIStress(t *testing.T) {
	data := ciWorkflow(t)
	if err := cicheck.ValidateWorkflow(data); err != nil {
		t.Fatalf("checked-in workflow: %v", err)
	}
	stages, err := cicheck.ExtractStages(data)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct{ id, want string }{
		{"linux", "test coverage bench cross"}, {"macos", "native"},
		{"linux-stress-packages", "stress-packages"}, {"linux-stress-plane-cpu1", "stress-plane-cpu1"}, {"linux-stress-plane", "stress-plane"},
		{"linux-stress-sidecar-cpu1", "stress-sidecar-cpu1"}, {"linux-stress-sidecar", "stress-sidecar"},
		{"linux-stress-processgroup", "stress-processgroup"}, {"linux-stress-functions", "stress-functions"},
		{"macos-stress-packages", "stress-packages"}, {"macos-stress-plane-cpu1", "stress-plane-cpu1"}, {"macos-stress-plane", "stress-plane"},
		{"macos-stress-sidecar-cpu1", "stress-sidecar-cpu1"}, {"macos-stress-sidecar", "stress-sidecar"},
		{"macos-stress-processgroup", "stress-processgroup"}, {"macos-stress-functions", "stress-functions"},
	} {
		got := stages[j.id]
		if strings.Join(got, " ") != j.want {
			t.Fatalf("%s stages = %v, want %s", j.id, got, j.want)
		}
		steps := node(t, &doc, "jobs", j.id, "steps")
		last := len(steps.Content) - 1
		if last != 2+len(got) {
			t.Fatalf("%s has %d steps", j.id, last+1)
		}
		for i := 3; i <= last; i++ {
			stage := got[i-3]
			mustReject(t, fmt.Sprintf("%s without step %d", j.id, i), mutated(t, func(r *yaml.Node) {
				s := node(t, r, "jobs", j.id, "steps")
				s.Content = append(s.Content[:i], s.Content[i+1:]...)
			}), fmt.Sprintf("jobs.%s.steps: missing check step for devcheck stage %q", j.id, stage))
		}
		mustReject(t, j.id+" conditional last step", mutated(t, func(r *yaml.Node) {
			setKey(node(t, r, "jobs", j.id, "steps", last), "continue-on-error", "true")
		}), fmt.Sprintf("jobs.%s.steps[%d].continue-on-error: unknown field", j.id, last))
		if strings.HasPrefix(j.want, "stress-") {
			// DW3: the step identity is asserted literally here; the
			// validator checks only job id, context, runner, timeout and stage.
			if last != 3 || node(t, &doc, "jobs", j.id, "steps", 3, "name").Value != "devcheck "+j.want ||
				node(t, &doc, "jobs", j.id, "steps", 3, "run").Value != "go run ./cmd/devcheck "+j.want ||
				node(t, &doc, "jobs", j.id, "steps", 3, "env", "GOPROXY").Value != "off" ||
				node(t, &doc, "jobs", j.id, "steps", 3, "env", "GOSUMDB").Value != "off" ||
				len(node(t, &doc, "jobs", j.id, "steps", 3, "env").Content) != 4 {
				t.Fatalf("%s step 3 differs from the stress step", j.id)
			}
			continue
		}
		for i := range steps.Content {
			if n := steps.Content[i]; strings.Contains(fmt.Sprint(yamlScalars(n)), "stress") {
				t.Fatalf("main job %s still runs stress at step %d", j.id, i)
			}
		}
	}
	workers := 0
	for _, j := range cicheck.Jobs() {
		worker := strings.Contains(j.ID, "-stress-")
		summary := strings.HasSuffix(j.ID, "-stress")
		// A worker's stage is its ID without the leading platform prefix
		// (linux- or macos-), never the text after its last hyphen, which
		// would turn linux-stress-plane-cpu1 into stress-cpu1.
		stage, prefixed := strings.CutPrefix(j.ID, "linux-")
		if !prefixed {
			stage, prefixed = strings.CutPrefix(j.ID, "macos-")
		}
		if worker {
			workers++
		}
		switch {
		case worker && (!prefixed || len(j.Stages) != 1 || j.Stages[0] != stage):
			t.Fatalf("worker %s stages = %v", j.ID, j.Stages)
		case summary && (len(j.Stages) != 0 || len(j.Needs) != 7 || len(node(t, &doc, "jobs", j.ID, "steps").Content) != 1):
			t.Fatalf("summary %s = %+v", j.ID, j)
		case !worker && !summary && strings.Contains(" "+strings.Join(j.Stages, " ")+" ", " stress"):
			t.Fatalf("main job %s stages = %v", j.ID, j.Stages)
		}
	}
	if workers != 14 {
		t.Fatalf("%d worker jobs, want 14", workers)
	}
	// Invocations per stage: full stress 13, the plane and sidecar pairs 2
	// each, the new CPU1 stages 1 each.
	for stage, calls := range map[string]int{"stress": 13, "stress-packages": 1, "stress-plane-cpu1": 1, "stress-plane": 2, "stress-sidecar-cpu1": 1,
		"stress-sidecar": 2, "stress-processgroup": 3, "stress-functions": 3} {
		r := &ciRunner{}
		if code, _, errOut := devcheckRun(t, r, stage); code != 0 || len(r.calls) != calls {
			t.Fatalf("%s dispatch = %d with %d calls %s", stage, code, len(r.calls), errOut)
		}
	}
}

// yamlScalars returns every scalar value below n.
func yamlScalars(n *yaml.Node) []string {
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, yamlScalars(c)...)
	}
	return out
}

var repeatCountRE = regexp.MustCompile(`The project's declared repeat count is (\d+) per CPU setting \((\d+(?:, \d+)*)\)\.`)

// FP-7: docs/ci.md states the policy StressSteps/StressCount implement, the
// platform convention with its guard and exceptions, and the PR #1 narrative.
func TestHardeningDeveloperContract(t *testing.T) {
	stress := docSection(t, "Stress checks")
	flat := strings.Join(strings.Fields(stress), " ")
	m := repeatCountRE.FindStringSubmatch(flat)
	if m == nil {
		t.Fatal("Stress checks does not declare the repeat count")
	}
	if m[1] != fmt.Sprint(devcheck.StressCount) || m[1] != fmt.Sprint(hardeningStressCount) {
		t.Fatalf("documented count %s, StressCount %d", m[1], devcheck.StressCount)
	}
	steps, err := devcheck.StressSteps(runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	// Every command runs the documented count at the documented CPU list,
	// except processgroup's (iteration 02c), plane's (iteration 05b) and
	// sidecar's (its sidecar follow-up), which run one documented setting
	// each and together cover the whole list for their package.
	cpu := "-cpu=" + strings.ReplaceAll(m[2], ", ", ",")
	var pkgs []string
	selectors := map[string]bool{}
	splitCPUs := map[string]map[string]bool{" ./internal/spikes/processgroup": {}, " ./internal/plane": {}, " ./internal/sidecar": {}}
	for _, s := range steps {
		argv := strings.Join(s.Argv, " ")
		split := false
		for suffix, seen := range splitCPUs {
			if !strings.HasSuffix(argv, suffix) {
				continue
			}
			split = true
			for _, c := range strings.Split(m[2], ", ") {
				if strings.Contains(argv, " -cpu="+c+" ") {
					seen[c] = true
				}
			}
		}
		if !split && !strings.Contains(argv, " "+cpu+" ") {
			t.Fatalf("documented %s differs from %s", cpu, argv)
		}
		if !strings.Contains(argv, " -count="+m[1]+" ") {
			t.Fatalf("documented -count=%s differs from %s", m[1], argv)
		}
		// The documented commands are the plan's argv.
		if !strings.Contains(stress, "\n"+argv+"\n") {
			t.Fatalf("Stress checks lacks the command %q", argv)
		}
		for _, a := range s.Argv {
			if strings.HasPrefix(a, "./") {
				pkgs = append(pkgs, a)
			}
			if sel, ok := strings.CutPrefix(a, "-run="); ok {
				names, known := hardeningSelectors[sel]
				if !known {
					t.Fatalf("unexpected stress selector %q", sel)
				}
				selectors[sel] = true
				for _, name := range names {
					requireTerms(t, "Stress checks", stress, "`"+name+"`")
				}
			}
		}
	}
	for pkg, seen := range splitCPUs {
		if len(seen) != len(strings.Split(m[2], ", ")) {
			t.Fatalf("%s runs CPU settings %v, documented %s", pkg, seen, m[2])
		}
	}
	if len(selectors) != len(hardeningSelectors) {
		t.Fatalf("plan selectors %v, want every one of %v", selectors, hardeningSelectors)
	}
	for _, p := range pkgs {
		requireTerms(t, "Stress checks", stress, "`"+strings.TrimPrefix(p, "./")+"`")
	}
	requireTerms(t, "Stress checks", stress,
		"`devcheck stress` for tests in its covered packages", "-race -count=20 -cpu=1,2,4",
		"never retry-until-green", "no count override, no lighter macOS count and no environment-based bypass",
		"`all` does not include stress", "runs both `all` and `stress`",
		"`-timeout=6m`", "15-minute watchdog", "under 10 minutes", "3–10 minutes",
		"moves into its own stress step with its own 6-minute timeout",
		"Measured: Linux", "macOS 02c worker times: pending until the first macOS worker runs of pull request #2",
		"not race-built", "`-cpu` varies the top-level tests' GOMAXPROCS", "native C compiler")
	if strings.Contains(stress, "PLACEHOLDER") {
		t.Fatal("Stress checks still has a placeholder")
	}
	// Obsolete pending instructions of pull request #1 and the old two-job
	// layout are not current policy any more (iteration 02b).
	for _, stale := range []string{"pending until the next `ci-macos` run of pull request #1", "Both CI jobs run stress as their last step", "30-minute `ci-macos` job for setup and native tests"} {
		if strings.Contains(strings.Join(strings.Fields(stress), " "), stale) {
			t.Fatalf("Stress checks keeps the obsolete %q", stale)
		}
	}
	platform := docSection(t, "Platform code")
	requireTerms(t, "Platform code", platform, "explicit `goos` argument",
		"[internal/devcheck/platform.go](../internal/devcheck/platform.go)", "`TestPlatformSourceGuard`",
		"irrespective of build constraints", "never proof of foreign kernel behavior",
		"Exempt files are not scanned for `runtime.GOOS`", "`internal/testkit/fakeadapter/signals_unix.go` must not grow a host branch",
		"compiled for both `linux` and `darwin`",
		"only the next `ci-macos` run proves Darwin runtime behavior")
	for _, f := range []string{"cli/cli.go", "devcheck/devcheck.go", "spikes/processgroup/experiment.go", "testkit/fakeadapter/fakeadapter.go",
		"spikes/processgroup/sys_linux.go", "spikes/processgroup/sys_darwin.go", "testkit/fakeadapter/signals_unix.go", "testkit/fakeadapter/signals_other.go"} {
		requireTerms(t, "Platform code", platform, "`internal/"+f+"`")
	}
	for _, w := range []string{"`Run`", "`evaluate`", "`RunHelper`", "`Parse`", "(`linux`)", "(`darwin`)", "(`linux || darwin`)", "(`!linux && !darwin`)"} {
		requireTerms(t, "Platform code", platform, w)
	}
	if _, err := os.Stat(filepath.Join(testkit.MustRepoRoot(t), "internal", "devcheck", "platform.go")); err != nil {
		t.Fatalf("source-guard link target: %v", err)
	}
	pr := docSection(t, "PR flow")
	requireTerms(t, "PR flow", pr, "iterations 01, 01b and 01c join pull request #1",
		"merge together after both checks, `ci-linux` and `ci-macos`, succeed on its current merge revision")
	if strings.Contains(pr, "land directly on `main`") {
		t.Fatal("PR flow keeps the superseded direct-to-main bootstrap")
	}
	requireTerms(t, "Local verification", docSection(t, "Local verification"), "go run ./cmd/devcheck all", "go run ./cmd/devcheck stress")
}
