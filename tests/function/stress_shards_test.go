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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wedevwork/callsheet/internal/cicheck"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 02c function tests: one top-level TestStressShard* per FP,
// extended by iteration 05b (the plane shard: four shards, ten steps,
// twelve jobs, four-result summaries). They read tracked artifacts only,
// inject runners into devcheck, run the coordinator's contract by name in a
// compiled devcheck test binary and the summaries' literal command in local
// bash, and never contact GitHub or run a real suite, stress or devcheck
// recursively.

// shard02b is iteration 02b's literal stress selection, the independent
// baseline of the shard union: five packages and two selectors, each at
// -count=20 for every CPU setting 1, 2 and 4.
var shard02b = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/processgroup ./internal/spikes/gittransport ./internal/plane",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=" + speedPlaneSelector + " ./tests/function",
}

// shardPlan is the literal shard table (design 02c, Shard plans, with
// design 05b's plane shard: ./internal/plane left the packages command for
// three single-CPU invocations).
var shardPlan = []struct {
	name     string
	parallel bool
	steps    [][2]string
}{
	{"packages", false, [][2]string{{"stress packages", "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/sidecar ./internal/contract ./internal/adapter"}}},
	{"plane", true, [][2]string{
		{"stress plane cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane"},
		{"stress plane cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/plane"},
		{"stress plane cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/plane"},
	}},
	{"processgroup", true, [][2]string{
		{"stress processgroup cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"},
		{"stress processgroup cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"},
		{"stress processgroup cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"},
	}},
	{"functions", false, [][2]string{
		{"stress function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"},
		{"stress plane function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=" + speedPlaneSelector + " ./tests/function"},
		{"stress node function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"},
	}},
}

// shard03 is iteration 03's literal addition to the 02b selection: the
// three node packages and the node process-boundary selector.
var shard03 = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/client ./internal/sidecar ./internal/contract",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function",
}

// shard04 is iteration 04's literal addition: the adapter package.
var shard04 = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/adapter",
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

// FP-1: both OS plans are exactly the four literal shards (ten steps), and
// their normalized union is the 02b multiset plus the literal additions of
// iterations 03 and 04, with nothing duplicated or lost; the complete plane
// package runs exactly once per CPU setting, in the plane shard only.
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
	for _, argv := range shard03 {
		for _, tp := range tuples(t, argv) {
			if want[tp] != 0 {
				t.Fatalf("03 addition %+v overlaps 02b", tp)
			}
			want[tp]++
		}
	}
	if len(want) != 33 {
		t.Fatalf("03 selection = %d tuples, want 02b plus 3 packages and 1 selector at 3 CPU settings", len(want))
	}
	for _, argv := range shard04 {
		for _, tp := range tuples(t, argv) {
			if want[tp] != 0 {
				t.Fatalf("04 addition %+v overlaps", tp)
			}
			want[tp]++
		}
	}
	if len(want) != 36 {
		t.Fatalf("04 selection = %d tuples, want 03 plus 1 package at 3 CPU settings", len(want))
	}
	for _, goos := range []string{"linux", "darwin"} {
		shards, err := devcheck.StressShards(goos)
		if err != nil || len(shards) != len(shardPlan) {
			t.Fatalf("%s shards = %+v %v", goos, shards, err)
		}
		got := map[tuple]int{}
		owner := map[tuple]string{}
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
					owner[tp] = sh.Name
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
				t.Errorf("%s: %+v selected %d times, 02b and 03 selected it %d times", goos, tp, n, want[tp])
			}
		}
		for _, cpu := range []string{"1", "2", "4"} {
			if tp := (tuple{"./internal/plane", "", cpu, "20"}); got[tp] != 1 || owner[tp] != "plane" {
				t.Fatalf("%s: plane at -cpu=%s selected %d times, in %q", goos, cpu, got[tp], owner[tp])
			}
		}
		for tp, sh := range owner {
			if tp.pkg == "./internal/plane" && sh != "plane" {
				t.Fatalf("%s: %+v outside the plane shard (%s)", goos, tp, sh)
			}
		}
		steps, err := devcheck.StressSteps(goos)
		var flatSteps []string
		for _, s := range steps {
			flatSteps = append(flatSteps, strings.Join(s.Argv, " "))
		}
		if err != nil || len(steps) != 10 || !slices.Equal(flatSteps, flat) {
			t.Fatalf("%s StressSteps is not the ten-step flattened shard view: %q", goos, flatSteps)
		}
	}
	// The functions shard keeps 02b's plane selector, whose executable
	// fixture and exclusions TestCISpeedSelection still runs.
	shards, _ := devcheck.StressShards("linux")
	if !slices.Contains(shards[3].Steps[1].Argv, "-run="+speedPlaneSelector) {
		t.Fatalf("plane selector changed: %v", shards[3].Steps[1].Argv)
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
// commands and boundaries, runs stress-plane's three invocations
// concurrently and joined with ordered logs, never overlaps the two
// Parallel shards, fails with retained logs, and the concurrent
// coordinator's contract passes by name.
func TestStressShardExecution(t *testing.T) {
	for stage, want := range map[string][][]string{
		"stress":              shardArgv("packages", "plane", "processgroup", "functions"),
		"stress-packages":     shardArgv("packages"),
		"stress-plane":        shardArgv("plane"),
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
	// A failed plane or processgroup invocation fails its shard after all
	// three ran, stops the aggregate before the next shard and keeps its
	// log.
	for _, c := range []struct {
		stage, failOn, failed string
		want                  [][]string
	}{
		{"stress", "-cpu=2 -timeout=6m ./internal/plane", "stress plane cpu2", shardArgv("packages", "plane")},
		{"stress-plane", "-cpu=4 ", "stress plane cpu4", shardArgv("plane")},
		{"stress", "-cpu=2 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu2", shardArgv("packages", "plane", "processgroup")},
		{"stress-processgroup", "-cpu=4 ", "stress processgroup cpu4", shardArgv("processgroup")},
		{"stress-functions", "TestFP4TransportHarness", "stress function", shardArgv("functions")[:1]},
		{"stress", "./internal/sidecar", "stress packages", shardArgv("packages")},
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
	// Through devcheck.Run: all three plane invocations have started before
	// any ends (a barrier), and so have processgroup's; plane's six events
	// all precede processgroup's six (the Parallel shards never overlap);
	// the logs replay under their CPU names in CPU order.
	o := &overlapRunner{groups: map[string]*gate{"./internal/plane": newGate(3), "./internal/spikes/processgroup": newGate(3)}}
	var out, errOut bytes.Buffer
	if code := devcheck.Run(context.Background(), []string{"stress"}, &out, &errOut, o.run); code != 0 {
		t.Fatalf("stress with overlap barriers = %d: %s", code, errOut.String())
	}
	o.mu.Lock()
	events := slices.Clone(o.events)
	o.mu.Unlock()
	var pl, pg []int
	for i, e := range events {
		switch {
		case strings.HasSuffix(e, " ./internal/plane"):
			pl = append(pl, i)
		case strings.HasSuffix(e, " ./internal/spikes/processgroup"):
			pg = append(pg, i)
		}
	}
	if len(events) != 20 || !slices.Equal(pl, []int{2, 3, 4, 5, 6, 7}) || !slices.Equal(pg, []int{8, 9, 10, 11, 12, 13}) {
		t.Fatalf("events = %q", events)
	}
	for _, from := range []int{2, 8} {
		for i := from; i < from+6; i++ {
			if strings.HasPrefix(events[i], "start ") != (i < from+3) {
				t.Fatalf("a Parallel shard's invocation ended before all three started: %q", events)
			}
		}
	}
	scratch := scratchOf(out.String())
	pos := -1
	for _, cpu := range []string{"1", "2", "4"} {
		name := "stress plane cpu" + cpu
		i := strings.Index(out.String(), "devcheck: ---- "+name+" log ("+filepath.Join(scratch, "stress-plane-cpu"+cpu+".log")+") ----\nok go test -race -count=20 -cpu="+cpu+" -timeout=6m ./internal/plane\n")
		if i < 0 || i < pos || !strings.Contains(out.String()[i:], "devcheck: "+name+": ok in ") {
			t.Fatalf("%s log not replayed in CPU order:\n%s", name, out.String())
		}
		pos = i
	}
	// The coordinator's contract, by name, with every named subtest: the
	// plane and processgroup overlap, failure, watchdog and log contracts
	// and the inactive fallback's wave mechanism.
	const pkg = "./internal/devcheck"
	bin := testkit.BuildTestBinary(t, pkg, "devcheck-stress-contract")
	contractRun(t, bin, pkg, "^TestStressConcurrencyContract$", os.Environ(),
		"TestStressConcurrencyContract", "TestStressConcurrencyContract/overlap", "TestStressConcurrencyContract/failure",
		"TestStressConcurrencyContract/watchdog", "TestStressConcurrencyContract/logs", "TestStressConcurrencyContract/waves")
}

// gate releases its waiters once n have arrived; a wait that is not
// released within the hang guard fails instead of blocking forever. It is a
// deadlock guard, never a timing oracle: correct code releases at once.
type gate struct {
	mu      sync.Mutex
	n       int
	arrived int
	release chan struct{}
}

func newGate(n int) *gate { return &gate{n: n, release: make(chan struct{})} }

func (g *gate) wait() error {
	g.mu.Lock()
	g.arrived++
	if g.arrived == g.n {
		close(g.release)
	}
	g.mu.Unlock()
	select {
	case <-g.release:
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("gate not released: the invocations did not overlap")
	}
}

// overlapRunner records start and end events and holds each call whose
// last argument names a gated package until all of that package's calls
// have arrived.
type overlapRunner struct {
	mu     sync.Mutex
	events []string
	groups map[string]*gate
}

func (r *overlapRunner) run(_ context.Context, argv, _ []string, _ string, stdout, _ io.Writer) error {
	a := strings.Join(argv, " ")
	r.mu.Lock()
	r.events = append(r.events, "start "+a)
	r.mu.Unlock()
	var err error
	if g := r.groups[argv[len(argv)-1]]; g != nil {
		err = g.wait()
	}
	fmt.Fprintf(stdout, "ok %s\n", a)
	r.mu.Lock()
	r.events = append(r.events, "end "+a)
	r.mu.Unlock()
	return err
}

func scratchOf(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "devcheck: scratch "); ok {
			return p
		}
	}
	return ""
}

// shardJobs is the literal twelve-job table (design 02c, Workflow topology,
// with design 05b's plane workers in CI-plan order).
var shardJobs = []struct {
	id, name, runner, timeout string
}{
	{"linux", "ci-linux", "ubuntu-24.04", "45"},
	{"macos", "ci-macos", "macos-15", "30"},
	{"linux-stress-packages", "ci-linux-stress-packages", "ubuntu-24.04", "20"},
	{"linux-stress-plane", "ci-linux-stress-plane", "ubuntu-24.04", "20"},
	{"linux-stress-processgroup", "ci-linux-stress-processgroup", "ubuntu-24.04", "20"},
	{"linux-stress-functions", "ci-linux-stress-functions", "ubuntu-24.04", "20"},
	{"macos-stress-packages", "ci-macos-stress-packages", "macos-15", "20"},
	{"macos-stress-plane", "ci-macos-stress-plane", "macos-15", "20"},
	{"macos-stress-processgroup", "ci-macos-stress-processgroup", "macos-15", "20"},
	{"macos-stress-functions", "ci-macos-stress-functions", "macos-15", "20"},
	{"linux-stress", "ci-linux-stress", "ubuntu-24.04", "5"},
	{"macos-stress", "ci-macos-stress", "ubuntu-24.04", "5"},
}

const summaryCommand = `test "$PACKAGES_RESULT" = success && test "$PLANE_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`

// summaryKeys are the summary step's environment keys in needs order.
var summaryKeys = []string{"PACKAGES_RESULT", "PLANE_RESULT", "PROCESSGROUP_RESULT", "FUNCTIONS_RESULT"}

// FP-3: the actual workflow has the twelve jobs and two literal summaries,
// and each summary's own command passes only when all four workers
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
		workers := []string{plat + "-stress-packages", plat + "-stress-plane", plat + "-stress-processgroup", plat + "-stress-functions"}
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
		if len(env.Content) != 8 {
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
		// every combination of the four conclusions in the four positions
		// (4^4), then for an empty, an unknown, a case-altered and a
		// whitespace value in each position with every other one success.
		var tuples [][4]string
		for _, a := range results {
			for _, b := range results {
				for _, c := range results {
					for _, d := range results {
						tuples = append(tuples, [4]string{a, b, c, d})
					}
				}
			}
		}
		for pos := 0; pos < 4; pos++ {
			for _, odd := range []string{"", "neutral", "Success", "success "} {
				tp := [4]string{"success", "success", "success", "success"}
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
			want := tp == [4]string{"success", "success", "success", "success"}
			if (err == nil) != want {
				t.Fatalf("%s %q: exit error %v, want pass=%v (%s)", id, tp, err, want, out)
			}
			if err == nil {
				passed++
			}
		}
		if len(tuples) != 256+16 || passed != 1 {
			t.Fatalf("%s: %d tuples, %d passed", id, len(tuples), passed)
		}
	}
}

// FP-4: the validator accepts the actual workflow and rejects a removed or
// miswired plane worker, any removed worker or a weakened summary;
// contract, stages, extracted sequences and both OS plans agree.
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
	if len(contract) != 12 || len(stages) != 12 {
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
			node(t, r, "jobs", id, "steps", 0, "run").Value = `test "$PACKAGES_RESULT" = success && test "$PLANE_RESULT" = success && test "$PROCESSGROUP_RESULT" = success`
		}), p+".steps[0].run: must be exactly")
		// Design 05b: the plane worker, its result and its comparison.
		mustReject(t, id+" plane comparison omitted", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "steps", 0, "run").Value = `test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`
		}), p+".steps[0].run: must be exactly")
		mustReject(t, id+" plane result miswired", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "steps", 0, "env", "PLANE_RESULT").Value = "${{ needs['" + id + "-packages'].result }}"
		}), p+".steps[0].env.PLANE_RESULT: must be")
		mustReject(t, id+" plane result removed", mutated(t, func(r *yaml.Node) {
			deleteKey(t, node(t, r, "jobs", id, "steps", 0, "env"), "PLANE_RESULT")
		}), p+".steps[0].env.PLANE_RESULT: missing required field")
		mustReject(t, id+" plane worker not needed", mutated(t, func(r *yaml.Node) {
			n := node(t, r, "jobs", id, "needs")
			n.Content = append(n.Content[:1:1], n.Content[2:]...)
		}), p+".needs: must be exactly")
		mustReject(t, id+" plane worker from the other platform", mutated(t, func(r *yaml.Node) {
			other := map[string]string{"linux-stress": "macos", "macos-stress": "linux"}[id]
			node(t, r, "jobs", id, "needs").Content[1].Value = other + "-stress-plane"
		}), p+".needs[1]: must be")
		mustReject(t, id+" error bypass", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "continue-on-error", "true") }), p+".continue-on-error: unknown field")
		mustReject(t, id+" setup added", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "env", "x") }), p+".env: unknown field")
	}
	// The workers never depend on anything.
	mustReject(t, "worker needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "macos-stress-processgroup"), "needs", "macos") }), "jobs.macos-stress-processgroup.needs: unknown field")
	mustReject(t, "plane worker needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "linux-stress-plane"), "needs", "linux-stress-packages") }), "jobs.linux-stress-plane.needs: unknown field")
	// The plane worker's own stage, budget and runner are fixed.
	for _, plat := range []string{"linux", "macos"} {
		id := plat + "-stress-plane"
		p := "jobs." + id
		mustReject(t, id+" stage", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "steps", 3, "run").Value = "go run ./cmd/devcheck stress-packages"
		}), p+`.steps[3].run: must run devcheck stage "stress-plane", got "stress-packages"`)
		mustReject(t, id+" count", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "steps", 3, "run").Value = "go test -race -count=1 -cpu=4 -timeout=6m ./internal/plane"
		}), p+`.steps[3].run: must be "go run ./cmd/devcheck stress-plane"`)
		mustReject(t, id+" cpu", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "steps", 3, "run").Value = "go run ./cmd/devcheck stress-plane -cpu=4"
		}), p+`.steps[3].run: must be "go run ./cmd/devcheck stress-plane"`)
		mustReject(t, id+" budget", mutated(t, func(r *yaml.Node) { node(t, r, "jobs", id, "timeout-minutes").Value = "30" }), p+`.timeout-minutes: must be "20", got "30"`)
	}
}

// FP-5: docs/ci.md documents the four required contexts versus the eight
// workers, the four shards with their exact commands, the plane invocation
// and log evidence, the preserved budgets and contexts, measured versus
// estimated times, the runner core assumptions, the sidecar risk with its
// decided follow-up, the inactive plane fallback and the pending hosted
// evidence rules.
func TestStressShardHandoff(t *testing.T) {
	checks := docSection(t, "Checks")
	requireTerms(t, "Checks", checks,
		"Twelve fixed jobs run on every trigger",
		"Exactly four of them are the required status check contexts on `main`: `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`",
		"The other eight are the stress workers", "four shards per platform", "not required contexts",
		"| `ci-linux-stress` | `ubuntu-24.04` | 5 min | required summary |", "| `ci-macos-stress` | `ubuntu-24.04` | 5 min | required summary |",
		"succeeds only if the four Linux workers all concluded `success`", "succeeds only if the four macOS workers all concluded `success`",
		"if: ${{ always() }}", summaryCommand, "PLANE_RESULT: ${{ needs['linux-stress-plane'].result }}",
		"Only all four results `success` exit zero",
		"`failure`, `cancelled`, `skipped`, an empty or any unknown result in any position fails the summary",
		"never put on the job's `if`", "Summaries hold no stress logs", "all eight workers check out")
	for _, j := range shardJobs[2:10] {
		requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `%s` | %s min | worker |", j.name, j.runner, j.timeout))
	}
	for _, stale := range []string{"An all-success triple alone exits zero", "The other six are the stress workers", "Ten fixed jobs run on every trigger"} {
		if strings.Contains(strings.Join(strings.Fields(checks), " "), stale) {
			t.Fatalf("Checks keeps the obsolete %q", stale)
		}
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
		"four shards per platform", "The four shards run ten commands",
		"| `plane` | `devcheck stress-plane` | `stress plane cpu1`, `cpu2`, `cpu4` | three concurrent invocations, one per CPU setting",
		"The plane and processgroup shards are concurrent", "at most three at once within that shard", "two shards never overlap",
		"does not cancel its siblings", "replayed from disk", "in CPU order", "`stress-plane-cpu1.log`",
		"`devcheck: stress plane cpu4: ok in 97.3s`", "includes the go command's build",
		"up to three test binaries (and their descendants) can remain as orphans", "there is no process-tree kill",
		"`no tests to run`", "exactly the iteration 02b selection",
		"`-timeout=6m`", "Each shard stage, that is each worker job, has its own",
		"one shared 15-minute watchdog", "not evidence about any hosted worker",
		"Worker jobs: 20 minutes each", "Summary jobs: 5 minutes each",
		"380 ms", "620 ms", "a revised bound requires a design revision",
		// Why plane is concurrent: the censored, contended hosted evidence.
		"run 36315881191", "`FAIL internal/plane 360.096s`", "`360.066s`", "censored, contended observation",
		"its `-p` default is GOMAXPROCS", "A lone sequential plane worker was rejected",
		// Core assumptions and the remaining real-time bounds.
		"four vCPUs (`ubuntu-24.04`)", "three M1 cores (`macos-15`, arm64)", "1 + 2 + 4 permits seven Go execution threads",
		"record the actual architecture and core count", "`testWait` and helper joins (20 s)", "`TestServerFailureContract/shutdown-deadline`",
		"event synchronization is not immunity to overload", "never retried to green",
		// The inactive, pre-authorised fallback.
		"Pre-authorised plane fallback (inactive)", "`stressWaves`", "It is empty",
		"`devcheck: stress plane cpuN: ok in Xs`", "`devcheck: stress plane cpuN: FAILED after Xs`", "X > 300.0 s", "exactly 300.0 does not",
		"`panic: test timed out after 6m0s`", "Missing logs are an evidence blocker, not a trigger",
		"both a trigger and an investigation blocker", "`\"plane\": {{4}, {1, 2}}`", "`TestStressConcurrencyContract/waves`", "it is not active",
		// The sidecar risk and its decided follow-up.
		"Sidecar stays in the packages shard", "310 s", "50 s (14%)", "260–310 s post-change estimate",
		"sidecar over 300.0 s", "no temporary workflow is added",
		"the follow-up is a fifth shard running sidecar's three CPU settings as concurrent invocations",
		"A fifth sequential shard is not an option",
		"`stress-sidecar`", "`ci-linux-stress-sidecar`", "`ci-macos-stress-sidecar`",
		// Estimates versus measurements.
		"Iteration 05b allocation", "explicit planning estimates, not measured speedups or hard upper bounds",
		"about 140–225 s per Linux and 150–255 s per macOS plane invocation", "135 s / 105 s below the 6-minute limit",
		"dividing the hosted alarm by three would be unsound", "about 300–380 s plus summary scheduling",
		"current evidence does not support promising 300 s or less",
		"Measured with iteration 05b (plane shard)", "Hosted and local figures come from different machines",
		"it is not a hosted estimate", "Hosted iteration 05b times: pending",
		"Hosted run 36315881191", "395.8 s", "432.6 s",
		"Expected per-job wall-clock after iteration 05b", "planning estimates, not measurements",
		// Historical 02c figures stay labelled as history.
		"run 36236333755", "Measured with iteration 02c", "Expected per-job wall-clock after iteration 02c",
		"macOS 02c worker times: pending until")
	for _, j := range shardJobs {
		requireTerms(t, "Stress checks expected times", stress, "`"+j.name+"` about")
	}
	for _, stale := range []string{"Only the processgroup shard is concurrent", "Why only processgroup is concurrent", "so it and the function commands stay sequential"} {
		if strings.Contains(strings.Join(strings.Fields(stress), " "), stale) {
			t.Fatalf("Stress checks keeps the obsolete %q", stale)
		}
	}
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp, "eight worker contexts are not required", "no protection change is needed for 02c or 05b")
	// The pull request #5 handoff: no protection change, all twelve jobs.
	const sub = "\n### Plane workers (pull request #5)\n"
	i := strings.Index(bp, sub)
	if i < 0 {
		t.Fatalf("Branch protection lacks %q", strings.TrimSpace(sub))
	}
	handoff := bp[i+len(sub):]
	if j := strings.Index(handoff, "\n### "); j >= 0 {
		handoff = handoff[:j]
	}
	requireTerms(t, "Plane workers", handoff, "`ci-linux-stress-plane`", "`ci-macos-stress-plane`",
		"The required contexts stay exactly `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`", "no protection change is made")
	steps := numberedSteps(t, handoff)
	review := stepIndex(t, steps, "REVIEW_APPROVED", "iterations 05 and 05b", "commit the reviewed code")
	push := stepIndex(t, steps, "Push `iter-05-dispatch`", "pull request #5")
	green := stepIndex(t, steps, "all twelve jobs", "eight stress workers", "all four required checks", "current PR merge revision")
	verify := stepIndex(t, steps, "owner verifies", "read-only", "same four required contexts", "neither plane worker")
	merge := stepIndex(t, steps, "Merge iterations 05 and 05b together", "all four checks green")
	if !(review < push && push < green && green < verify && verify < merge) {
		t.Fatalf("handoff order review=%d push=%d green=%d verify=%d merge=%d", review, push, green, verify, merge)
	}
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "conclusions of all twelve jobs", "eight worker logs (the summaries hold none)",
		"`devcheck: stage stress-plane ok`", "`devcheck: stage stress-processgroup ok`", "every CPU invocation's outcome",
		"`devcheck: stress plane cpuN: ok in Xs`", "separately from its go command's build", "pre-authorised plane fallback trigger",
		"sidecar follow-up rule", "core count and architecture", "summary wait", "critical path",
		"failed runs stay in the evidence, never discarded as retries", "any timeout or assertion failure blocks qualification")
	local := docSection(t, "Local verification")
	requireTerms(t, "Local verification", local, "go run ./cmd/devcheck stress-packages", "go run ./cmd/devcheck stress-plane",
		"go run ./cmd/devcheck stress-processgroup", "go run ./cmd/devcheck stress-functions",
		"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run '^TestStressConcurrencyContract$' ./internal/devcheck",
		"without other stress work running on the same host", "both concurrent shards and the plane fallback's wave mechanism")
	pr := docSection(t, "PR flow")
	requireTerms(t, "PR flow", pr, "iterations 02, 02b and 02c join pull request #2",
		"Iteration 05b (the plane stress shard) is delivered with iteration 05 in pull request #5", "all twelve jobs")
	// No workflow step can change repository settings.
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "--method", "POST", "curl", "protection", "contents: write", "secrets.", "continue-on-error", "strategy:"} {
		if strings.Contains(wf, forbidden) {
			t.Fatalf("workflow contains %q", forbidden)
		}
	}
}
