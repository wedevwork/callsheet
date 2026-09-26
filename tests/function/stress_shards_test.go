package function

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wedevwork/callsheet/internal/cicheck"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 02c function tests: one top-level TestStressShard* per FP. They
// read tracked artifacts only, inject runners into devcheck, run the
// coordinator's contract by name in a compiled devcheck test binary and the
// summaries' literal command in local bash, and never contact GitHub or run
// a real suite, stress or devcheck recursively.

// shard02b is iteration 02b's literal stress selection, the independent
// baseline of the shard union: five packages and two selectors, each at
// -count=20 for every CPU setting 1, 2 and 4.
var shard02b = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/processgroup ./internal/spikes/gittransport ./internal/plane",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=" + speedPlaneSelector + " ./tests/function",
}

// shardPlan is the literal 02c shard table (design 02c, Shard plans).
var shardPlan = []struct {
	name     string
	parallel bool
	steps    [][2]string
}{
	{"packages", false, [][2]string{{"stress packages", "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/plane"}}},
	{"processgroup", true, [][2]string{
		{"stress processgroup cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"},
		{"stress processgroup cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"},
		{"stress processgroup cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"},
	}},
	{"functions", false, [][2]string{
		{"stress function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"},
		{"stress plane function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=" + speedPlaneSelector + " ./tests/function"},
	}},
}

// shardArgv returns the literal commands of the named shards, in order.
func shardArgv(names ...string) [][]string {
	var groups [][]string
	for _, n := range names {
		for _, sh := range shardPlan {
			if sh.name != n {
				continue
			}
			if sh.parallel {
				var g []string
				for _, s := range sh.steps {
					g = append(g, s[1])
				}
				groups = append(groups, g)
				continue
			}
			for _, s := range sh.steps {
				groups = append(groups, []string{s[1]})
			}
		}
	}
	return groups
}

// sameGroups compares calls with ordered groups, each group as a set.
func sameGroups(calls [][]string, groups [][]string) bool {
	i := 0
	for _, g := range groups {
		if i+len(g) > len(calls) {
			return false
		}
		var got []string
		for _, c := range calls[i : i+len(g)] {
			got = append(got, strings.Join(c, " "))
		}
		want := slices.Clone(g)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			return false
		}
		i += len(g)
	}
	return i == len(calls)
}

// tuple is one normalized repetition unit of a stress command.
type tuple struct{ pkg, selector, cpu, count string }

// tuples expands one argv display into (package, selector, CPU, count)
// tuples, requiring -race and the 6-minute binary timeout.
func tuples(t *testing.T, argv string) []tuple {
	t.Helper()
	f := strings.Fields(argv)
	if len(f) < 7 || strings.Join(f[:3], " ") != "go test -race" || !slices.Contains(f, "-timeout=6m") {
		t.Fatalf("not a race go test with -timeout=6m: %s", argv)
	}
	var count, sel string
	var cpus, pkgs []string
	for _, a := range f[3:] {
		switch {
		case strings.HasPrefix(a, "-count="):
			count = strings.TrimPrefix(a, "-count=")
		case strings.HasPrefix(a, "-cpu="):
			cpus = strings.Split(strings.TrimPrefix(a, "-cpu="), ",")
		case strings.HasPrefix(a, "-run="):
			sel = strings.TrimPrefix(a, "-run=")
		case strings.HasPrefix(a, "./"):
			pkgs = append(pkgs, a)
		case a == "-timeout=6m":
		default:
			t.Fatalf("unexpected argument %q in %s", a, argv)
		}
	}
	var out []tuple
	for _, p := range pkgs {
		for _, c := range cpus {
			out = append(out, tuple{p, sel, c, count})
		}
	}
	return out
}

// FP-1: both OS plans are exactly the three literal shards, and their
// normalized union is the 02b multiset, with nothing duplicated or lost.
func TestStressShardSelection(t *testing.T) {
	want := map[tuple]int{}
	for _, argv := range shard02b {
		for _, tp := range tuples(t, argv) {
			want[tp]++
		}
	}
	if len(want) != 21 {
		t.Fatalf("02b baseline = %d tuples, want 5 packages and 2 selectors at 3 CPU settings", len(want))
	}
	for _, goos := range []string{"linux", "darwin"} {
		shards, err := devcheck.StressShards(goos)
		if err != nil || len(shards) != len(shardPlan) {
			t.Fatalf("%s shards = %+v %v", goos, shards, err)
		}
		got := map[tuple]int{}
		var flat []string
		for i, sh := range shards {
			w := shardPlan[i]
			if sh.Name != w.name || sh.Parallel != w.parallel || len(sh.Steps) != len(w.steps) {
				t.Fatalf("%s shard %d = %+v, want %s parallel=%v", goos, i, sh, w.name, w.parallel)
			}
			for j, s := range sh.Steps {
				argv := strings.Join(s.Argv, " ")
				if s.Name != w.steps[j][0] || argv != w.steps[j][1] || strings.Join(s.Env, " ") != "CGO_ENABLED=1" {
					t.Fatalf("%s %s step %d = %+v", goos, sh.Name, j, s)
				}
				for _, tp := range tuples(t, argv) {
					if tp.count != fmt.Sprint(devcheck.StressCount) || tp.count != "20" {
						t.Fatalf("%s %s: count %s", goos, s.Name, tp.count)
					}
					got[tp]++
				}
				flat = append(flat, argv)
			}
		}
		for tp, n := range want {
			if got[tp] != n {
				t.Errorf("%s: %+v selected %d times, want %d", goos, tp, got[tp], n)
			}
		}
		for tp, n := range got {
			if want[tp] != n {
				t.Errorf("%s: %+v selected %d times, 02b selected it %d times", goos, tp, n, want[tp])
			}
		}
		steps, err := devcheck.StressSteps(goos)
		var flatSteps []string
		for _, s := range steps {
			flatSteps = append(flatSteps, strings.Join(s.Argv, " "))
		}
		if err != nil || !slices.Equal(flatSteps, flat) {
			t.Fatalf("%s StressSteps is not the flattened shard view: %q", goos, flatSteps)
		}
	}
	// The functions shard keeps 02b's plane selector, whose executable
	// fixture and exclusions TestCISpeedSelection still runs.
	shards, _ := devcheck.StressShards("linux")
	if !slices.Contains(shards[2].Steps[1].Argv, "-run="+speedPlaneSelector) {
		t.Fatalf("plane selector changed: %v", shards[2].Steps[1].Argv)
	}
	for _, goos := range []string{"windows", "freebsd", ""} {
		if _, err := devcheck.StressShards(goos); err == nil {
			t.Fatalf("StressShards(%q) accepted", goos)
		}
	}
}

// syncRunner is a race-safe injected Runner: it records every call under
// its mutex (never held while the call writes output) and fails calls
// whose argv contains failOn.
type syncRunner struct {
	mu     sync.Mutex
	calls  [][]string
	failOn string
}

func (r *syncRunner) run(_ context.Context, argv, _ []string, _ string, stdout, stderr io.Writer) error {
	r.mu.Lock()
	r.calls = append(r.calls, argv)
	r.mu.Unlock()
	if r.failOn != "" && strings.Contains(strings.Join(argv, " "), r.failOn) {
		fmt.Fprintf(stderr, "injected failure: %s\n", strings.Join(argv, " "))
		return errors.New("exit status 1")
	}
	fmt.Fprintf(stdout, "ok %s\n", strings.Join(argv, " "))
	return nil
}

// FP-2: devcheck.Run executes the aggregate and each shard with the exact
// commands and boundaries, fails with retained logs, and the concurrent
// coordinator's contract passes by name.
func TestStressShardExecution(t *testing.T) {
	for stage, want := range map[string][][]string{
		"stress":              shardArgv("packages", "processgroup", "functions"),
		"stress-packages":     shardArgv("packages"),
		"stress-processgroup": shardArgv("processgroup"),
		"stress-functions":    shardArgv("functions"),
	} {
		r := &syncRunner{}
		var out, errOut bytes.Buffer
		code := devcheck.Run(context.Background(), []string{stage}, &out, &errOut, r.run)
		scratch := scratchOf(out.String())
		if code != 0 || !strings.Contains(out.String(), "devcheck: stage "+stage+" ok") || !sameGroups(r.calls, want) {
			t.Fatalf("%s = %d, calls %q: %s", stage, code, r.calls, errOut.String())
		}
		for _, g := range want {
			for _, argv := range g {
				if !strings.Contains(out.String(), "ok "+argv+"\n") {
					t.Fatalf("%s: output of %s not shown:\n%s", stage, argv, out.String())
				}
			}
		}
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			t.Fatalf("%s: successful scratch %s not removed", stage, scratch)
		}
	}
	// A failed processgroup invocation fails the shard after all three
	// ran, stops the aggregate before functions and keeps its log.
	for _, c := range []struct {
		stage, failOn, failed string
		want                  [][]string
	}{
		{"stress", "-cpu=2 ", "stress processgroup cpu2", shardArgv("packages", "processgroup")},
		{"stress-processgroup", "-cpu=4 ", "stress processgroup cpu4", shardArgv("processgroup")},
		{"stress-functions", "TestFP4TransportHarness", "stress function", shardArgv("functions")[:1]},
		{"stress", "./internal/plane", "stress packages", shardArgv("packages")},
	} {
		r := &syncRunner{failOn: c.failOn}
		var out, errOut bytes.Buffer
		code := devcheck.Run(context.Background(), []string{c.stage}, &out, &errOut, r.run)
		scratch := scratchOf(out.String())
		if code != 1 || !sameGroups(r.calls, c.want) || !strings.Contains(errOut.String(), "stage "+c.stage+" FAILED: "+c.failed+" failed: go test -race") ||
			!strings.Contains(errOut.String(), "logs retained in "+scratch) {
			t.Fatalf("%s failing %s = %d, calls %q: %s", c.stage, c.failOn, code, r.calls, errOut.String())
		}
		log, err := os.ReadFile(filepath.Join(scratch, strings.ReplaceAll(c.failed, " ", "-")+".log"))
		if err != nil || !strings.Contains(string(log), "injected failure") {
			t.Fatalf("%s: failure log %q %v", c.failed, log, err)
		}
		os.RemoveAll(scratch)
	}
	// The coordinator's contract, by name, with every named subtest.
	const pkg = "./internal/devcheck"
	bin := testkit.BuildTestBinary(t, pkg, "devcheck-stress-contract")
	contractRun(t, bin, pkg, "^TestStressConcurrencyContract$", os.Environ(),
		"TestStressConcurrencyContract", "TestStressConcurrencyContract/overlap", "TestStressConcurrencyContract/failure",
		"TestStressConcurrencyContract/watchdog", "TestStressConcurrencyContract/logs")
}

func scratchOf(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "devcheck: scratch "); ok {
			return p
		}
	}
	return ""
}

// shardJobs is the literal ten-job table (design 02c, Workflow topology).
var shardJobs = []struct {
	id, name, runner, timeout string
}{
	{"linux", "ci-linux", "ubuntu-24.04", "45"},
	{"macos", "ci-macos", "macos-15", "30"},
	{"linux-stress-packages", "ci-linux-stress-packages", "ubuntu-24.04", "20"},
	{"linux-stress-processgroup", "ci-linux-stress-processgroup", "ubuntu-24.04", "20"},
	{"linux-stress-functions", "ci-linux-stress-functions", "ubuntu-24.04", "20"},
	{"macos-stress-packages", "ci-macos-stress-packages", "macos-15", "20"},
	{"macos-stress-processgroup", "ci-macos-stress-processgroup", "macos-15", "20"},
	{"macos-stress-functions", "ci-macos-stress-functions", "macos-15", "20"},
	{"linux-stress", "ci-linux-stress", "ubuntu-24.04", "5"},
	{"macos-stress", "ci-macos-stress", "ubuntu-24.04", "5"},
}

const summaryCommand = `test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`

// summaryKeys are the summary step's environment keys in needs order.
var summaryKeys = []string{"PACKAGES_RESULT", "PROCESSGROUP_RESULT", "FUNCTIONS_RESULT"}

// FP-3: the actual workflow has the ten jobs and two literal summaries,
// and each summary's own command passes only when all three workers
// concluded success.
func TestStressShardSummaries(t *testing.T) {
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
	var wantIDs []string
	for _, j := range shardJobs {
		wantIDs = append(wantIDs, j.id)
		job := node(t, jobs, j.id)
		if node(t, job, "name").Value != j.name || node(t, job, "runs-on").Value != j.runner || node(t, job, "timeout-minutes").Value != j.timeout {
			t.Fatalf("%s identity differs from the table", j.id)
		}
	}
	if !slices.Equal(ids, wantIDs) {
		t.Fatalf("job ids = %v, want %v", ids, wantIDs)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is required to execute the summaries' literal command: %v", err)
	}
	results := []string{"success", "failure", "cancelled", "skipped"}
	for _, plat := range []string{"linux", "macos"} {
		id := plat + "-stress"
		job := node(t, jobs, id)
		var keys []string
		for i := 0; i+1 < len(job.Content); i += 2 {
			keys = append(keys, job.Content[i].Value)
		}
		if strings.Join(keys, " ") != "name runs-on timeout-minutes needs if defaults steps" {
			t.Fatalf("%s fields = %v", id, keys)
		}
		workers := []string{plat + "-stress-packages", plat + "-stress-processgroup", plat + "-stress-functions"}
		var needs []string
		for _, n := range node(t, job, "needs").Content {
			needs = append(needs, n.Value)
		}
		if !slices.Equal(needs, workers) || node(t, job, "if").Value != "${{ always() }}" || node(t, job, "defaults", "run", "shell").Value != "bash" {
			t.Fatalf("%s needs %v, if %q", id, needs, node(t, job, "if").Value)
		}
		steps := node(t, job, "steps")
		if len(steps.Content) != 1 || node(t, steps, 0, "name").Value != "Require every stress shard" {
			t.Fatalf("%s steps = %d", id, len(steps.Content))
		}
		env := node(t, steps, 0, "env")
		if len(env.Content) != 6 {
			t.Fatalf("%s env has %d entries", id, len(env.Content)/2)
		}
		for i, k := range summaryKeys {
			if got := node(t, env, k).Value; got != "${{ needs['"+workers[i]+"'].result }}" {
				t.Fatalf("%s %s = %q", id, k, got)
			}
		}
		run := node(t, steps, 0, "run").Value
		if run != summaryCommand {
			t.Fatalf("%s run = %q", id, run)
		}
		// Execute the extracted command as Actions' bash shell does, for
		// every combination of the four conclusions, then for an empty and
		// an unknown value in each position.
		var tuples [][3]string
		for _, a := range results {
			for _, b := range results {
				for _, c := range results {
					tuples = append(tuples, [3]string{a, b, c})
				}
			}
		}
		for pos := 0; pos < 3; pos++ {
			for _, odd := range []string{"", "neutral", "Success", "success "} {
				tp := [3]string{"success", "success", "success"}
				tp[pos] = odd
				tuples = append(tuples, tp)
			}
		}
		passed := 0
		for _, tp := range tuples {
			cmd := exec.Command(bash, "--noprofile", "--norc", "-eo", "pipefail", "-c", run)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			for i, k := range summaryKeys {
				cmd.Env = append(cmd.Env, k+"="+tp[i])
			}
			out, err := cmd.CombinedOutput()
			var ee *exec.ExitError
			if err != nil && !errors.As(err, &ee) {
				t.Fatalf("%s %q: bash did not run: %v", id, tp, err)
			}
			want := tp == [3]string{"success", "success", "success"}
			if (err == nil) != want {
				t.Fatalf("%s %q: exit error %v, want pass=%v (%s)", id, tp, err, want, out)
			}
			if err == nil {
				passed++
			}
		}
		if len(tuples) != 64+12 || passed != 1 {
			t.Fatalf("%s: %d tuples, %d passed", id, len(tuples), passed)
		}
	}
}

// FP-4: the validator accepts the actual workflow and rejects a removed
// worker or a weakened summary; contract, stages, extracted sequences and
// both OS plans agree.
func TestStressShardPolicy(t *testing.T) {
	data := ciWorkflow(t)
	if err := cicheck.ValidateWorkflow(data); err != nil {
		t.Fatal(err)
	}
	stages, err := cicheck.ExtractStages(data)
	if err != nil {
		t.Fatal(err)
	}
	contract := cicheck.Jobs()
	if len(contract) != 10 || len(stages) != 10 {
		t.Fatalf("contract %d jobs, workflow %d", len(contract), len(stages))
	}
	dispatch := devcheck.Stages()
	for _, goos := range []string{"linux", "darwin"} {
		shards, err := devcheck.StressShards(goos)
		if err != nil {
			t.Fatal(err)
		}
		for _, plat := range []string{"linux", "macos"} {
			var workers []string
			for _, sh := range shards {
				stage := "stress-" + sh.Name
				id := plat + "-stress-" + sh.Name
				if !slices.Contains(dispatch, stage) || !slices.Equal(stages[id], []string{stage}) {
					t.Fatalf("%s: worker %s runs %v, want stage %s in %v", goos, id, stages[id], stage, dispatch)
				}
				workers = append(workers, id)
			}
			for _, j := range contract {
				if j.ID == plat+"-stress" && (!slices.Equal(j.Needs, workers) || len(j.Stages) != 0 || len(stages[j.ID]) != 0 || !j.Required) {
					t.Fatalf("%s summary %+v, extracted %v, want needs %v", goos, j, stages[j.ID], workers)
				}
			}
		}
	}
	var required []string
	for _, j := range contract {
		if !slices.Equal(stages[j.ID], j.Stages) && !(len(j.Stages) == 0 && len(stages[j.ID]) == 0) {
			t.Fatalf("%s: contract %v, workflow %v", j.ID, j.Stages, stages[j.ID])
		}
		if j.Required {
			required = append(required, j.Name)
		}
		// Each worker's stage dispatches exactly its shard.
		if len(j.Stages) == 1 && strings.HasPrefix(j.Stages[0], "stress-") {
			r := &syncRunner{}
			var out, errOut bytes.Buffer
			if code := devcheck.Run(context.Background(), j.Stages[0:1], &out, &errOut, r.run); code != 0 ||
				!sameGroups(r.calls, shardArgv(strings.TrimPrefix(j.Stages[0], "stress-"))) {
				t.Fatalf("%s dispatch = %d %q %s", j.ID, code, r.calls, errOut.String())
			}
			os.RemoveAll(scratchOf(out.String()))
		}
	}
	if strings.Join(required, ",") != "ci-linux,ci-macos,ci-linux-stress,ci-macos-stress" || !slices.Equal(required, cicheck.RequiredChecks()) {
		t.Fatalf("required = %v", required)
	}
	// Removing any worker fails validation, and a removed summary also
	// loses its required context.
	for _, j := range contract {
		id := j.ID
		removed := mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs"), id) })
		mustReject(t, id+" removed", removed, "jobs."+id+": missing required field")
		if j.Required {
			if err := cicheck.CheckJobNames(map[string][]byte{"ci.yml": removed}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("required check %q is not defined", j.Name)) {
				t.Fatalf("%s removed: job names %v", id, err)
			}
		}
	}
	// Weakening either summary fails.
	for _, id := range []string{"linux-stress", "macos-stress"} {
		p := "jobs." + id
		mustReject(t, id+" always removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs", id), "if") }), p+".if: missing required field")
		mustReject(t, id+" skipped gate", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "if").Value = "${{ needs." + id + "-packages.result == 'success' }}"
		}), p+".if: expressions are not allowed")
		mustReject(t, id+" worker dropped", mutated(t, func(r *yaml.Node) {
			n := node(t, r, "jobs", id, "needs")
			n.Content = n.Content[:2]
		}), p+".needs: must be exactly")
		mustReject(t, id+" bypass", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "steps", 0, "run").Value = summaryCommand + " || true"
		}), p+".steps[0].run: must be exactly")
		mustReject(t, id+" check dropped", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "steps", 0, "run").Value = `test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success`
		}), p+".steps[0].run: must be exactly")
		mustReject(t, id+" error bypass", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "continue-on-error", "true") }), p+".continue-on-error: unknown field")
		mustReject(t, id+" setup added", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "env", "x") }), p+".env: unknown field")
	}
	// The workers never depend on anything.
	mustReject(t, "worker needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "macos-stress-processgroup"), "needs", "macos") }), "jobs.macos-stress-processgroup.needs: unknown field")
}

// FP-5: docs/ci.md documents the contexts versus the workers, the exact
// commands, count and budgets, measured versus estimated times, the local
// and remote evidence and the ordered handoff.
func TestStressShardHandoff(t *testing.T) {
	checks := docSection(t, "Checks")
	requireTerms(t, "Checks", checks,
		"Exactly four of them are the required status check contexts on `main`: `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`",
		"six are the stress workers", "not required contexts",
		"| `ci-linux-stress` | `ubuntu-24.04` | 5 min | required summary |", "| `ci-macos-stress` | `ubuntu-24.04` | 5 min | required summary |",
		"if: ${{ always() }}", summaryCommand,
		"An all-success triple alone exits zero", "`failure`, `cancelled`, `skipped`, an empty or any unknown result fails the summary",
		"never put on the job's `if`", "Summaries hold no stress logs")
	for _, j := range shardJobs[2:8] {
		requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `%s` | %s min | worker |", j.name, j.runner, j.timeout))
	}
	stress := docSection(t, "Stress checks")
	for _, sh := range shardPlan {
		requireTerms(t, "Stress checks", stress, "`devcheck stress-"+sh.name+"`")
		for _, s := range sh.steps {
			if !strings.Contains(stress, "\n"+s[1]+"\n") {
				t.Fatalf("Stress checks lacks the command %q", s[1])
			}
			requireTerms(t, "Stress checks", stress, "`"+s[0]+"`")
		}
	}
	requireTerms(t, "Stress checks", stress,
		"The project's declared repeat count is 20 per CPU setting (1, 2, 4).",
		"three concurrent invocations, one per CPU setting", "at most three at once",
		"does not cancel its siblings", "replayed from disk", "in CPU order",
		"up to three test binaries (and their descendants) can remain as orphans", "there is no process-tree kill",
		"`no tests to run`", "exactly the iteration 02b selection",
		"`-timeout=6m`", "Each shard stage, that is each worker job, has its own",
		"one shared 15-minute watchdog", "not evidence about any hosted worker",
		"Worker jobs: 20 minutes each", "Summary jobs: 5 minutes each",
		"about 4–5 minutes", "not an acceptance threshold and not a measurement",
		"380 ms", "620 ms", "a revised bound requires a design revision",
		"run 36236333755", "`ci-linux` 101 s, `ci-macos` 49 s, `ci-linux-stress` 350 s and `ci-macos-stress` 568 s",
		"227 s / 298 s", "211 s / 234 s", "185 s / 239 s", "41 s / 76 s", "63 s / 163 s",
		"supersedes iteration 02b's pending first-run timing language",
		"Measured with iteration 02c", "Expected per-job wall-clock after iteration 02c", "planning estimates, not measurements",
		"Hosted and local figures come from different machines",
		"macOS 02c worker times: pending until")
	for _, j := range shardJobs {
		requireTerms(t, "Stress checks expected times", stress, "`"+j.name+"` about")
	}
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp, "no protection change is needed for 02c", "six worker contexts are not required")
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
	review := stepIndex(t, steps, "REVIEW_APPROVED", "iterations 02, 02b and 02c", "commit the reviewed code")
	push := stepIndex(t, steps, "Push `iter-02-plane-trust`")
	green := stepIndex(t, steps, "all ten jobs", "six stress workers", "all four required checks", "current PR merge revision")
	add := stepIndex(t, steps, "Conditional 02b prerequisite", "only if", "not yet required")
	verify := stepIndex(t, steps, "owner verifies", "read-only", "source bindings")
	merge := stepIndex(t, steps, "Merge iterations 02, 02b and 02c together", "all four checks green")
	if !(review < push && push < green && green < add && add < verify && verify < merge) {
		t.Fatalf("handoff order review=%d push=%d green=%d add=%d verify=%d merge=%d", review, push, green, add, verify, merge)
	}
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "conclusions of all ten jobs", "six worker logs (the summaries hold none)",
		"`devcheck: stage stress-processgroup ok`", "every CPU invocation's outcome", "summary wait", "overall workflow critical path",
		"without weakening tests")
	local := docSection(t, "Local verification")
	requireTerms(t, "Local verification", local, "go run ./cmd/devcheck stress-packages", "go run ./cmd/devcheck stress-processgroup",
		"go run ./cmd/devcheck stress-functions", "go test -race -count=20 -cpu=1,2,4 -run '^TestStressConcurrencyContract$' ./internal/devcheck",
		"without other stress work running on the same host")
	requireTerms(t, "PR flow", docSection(t, "PR flow"), "iterations 02, 02b and 02c join pull request #2")
	// No workflow step can change repository settings.
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "--method", "POST", "curl", "protection", "contents: write", "secrets.", "continue-on-error", "strategy:"} {
		if strings.Contains(wf, forbidden) {
			t.Fatalf("workflow contains %q", forbidden)
		}
	}
}
