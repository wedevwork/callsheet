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
)

// Iteration 02c function tests: one top-level TestStressShard* per FP,
// extended by iteration 05b (the plane shard: four shards, ten steps,
// twelve jobs, four-result summaries), by its sidecar follow-up (the
// sidecar shard: five shards, thirteen steps, fourteen jobs, five-result
// summaries) and by iteration 06a-perf (the plane and sidecar CPU1 shards:
// seven shards, the same thirteen steps, eighteen jobs, seven-result
// summaries) and by the contract headroom fix (the packages shard's
// per-CPU contract group: sixteen steps, the same seven shards and
// eighteen jobs) and by the workspace and mcpqual headroom fix (two more
// per-CPU groups: twenty-two steps, the same shards and jobs). They read tracked artifacts only,
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
// three single-CPU invocations, and ./internal/sidecar likewise by its
// sidecar follow-up; design 06a-perf moved each CPU1 invocation into a
// singleton shard of its own, leaving CPU2 and CPU4 concurrent in plane and
// sidecar; the contract headroom fix moved ./internal/contract out of the
// combined packages command into the packages shard's per-CPU group, run
// after it as three concurrent single-CPU invocations, and the workspace
// and mcpqual headroom fix did the same for ./internal/mcpqual and
// ./internal/workspace: cpuGroups, one group per package, run one after
// another).
var shardPlan = []struct {
	name      string
	parallel  bool
	steps     [][2]string
	cpuGroups [][][2]string
}{
	{"packages", false, [][2]string{{"stress packages", "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication"}},
		[][][2]string{{
			{"stress contract cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/contract"},
			{"stress contract cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/contract"},
			{"stress contract cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/contract"},
		}, {
			{"stress mcpqual cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/mcpqual"},
			{"stress mcpqual cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/mcpqual"},
			{"stress mcpqual cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/mcpqual"},
		}, {
			{"stress workspace cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/workspace"},
			{"stress workspace cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/workspace"},
			{"stress workspace cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/workspace"},
		}}},
	{"plane-cpu1", true, [][2]string{
		{"stress plane cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane"},
	}, nil},
	{"plane", true, [][2]string{
		{"stress plane cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/plane"},
		{"stress plane cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/plane"},
	}, nil},
	{"sidecar-cpu1", true, [][2]string{
		{"stress sidecar cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar"},
	}, nil},
	{"sidecar", true, [][2]string{
		{"stress sidecar cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar"},
		{"stress sidecar cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar"},
	}, nil},
	{"processgroup", true, [][2]string{
		{"stress processgroup cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"},
		{"stress processgroup cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"},
		{"stress processgroup cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"},
	}, nil},
	{"functions", false, [][2]string{
		{"stress function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"},
		{"stress plane function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=" + speedPlaneSelector + " ./tests/function"},
		{"stress node function", "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"},
	}, nil},
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

// shard07a is iteration 07a's literal addition: the MCP server package,
// in the packages shard only.
var shard07a = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/mcp",
}

// shard07b is iteration 07b's literal addition: the qualification harness
// package, in the packages shard only.
var shard07b = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/mcpqual",
}

// shard09a is iteration 09a's literal addition: the workspace hub
// package, in the packages shard only.
var shard09a = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/workspace",
}

// shard09b is iteration 09b's literal addition: the local transfer
// package, in the packages shard only, after the workspace hub.
var shard09b = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/workspacetransfer",
}

// shard10b is iteration 10b's literal addition: the task workspace and
// task publication packages, in the combined packages invocation after
// the local transfers, in that order.
var shard10b = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/taskworkspace ./internal/taskpublication",
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
			for _, cg := range sh.cpuGroups {
				var g []string
				for _, s := range cg {
					g = append(g, s[1])
				}
				groups = append(groups, g)
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

// FP-1: both OS plans are exactly the seven literal shards (sixteen
// steps since the contract headroom fix: the packages shard's combined
// command and its contract per-CPU group), and their normalized union is the 02b multiset plus the literal
// additions of iterations 03, 04, 07a and 07b (42 tuples, each exactly once), with
// nothing duplicated or lost; the complete plane and sidecar packages run
// exactly once per CPU setting: CPU1 only in plane-cpu1 and sidecar-cpu1,
// CPU2 and CPU4 only in plane and sidecar (design 06a-perf).
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
	for _, argv := range shard07a {
		for _, tp := range tuples(t, argv) {
			if want[tp] != 0 {
				t.Fatalf("07a addition %+v overlaps", tp)
			}
			want[tp]++
		}
	}
	if len(want) != 39 {
		t.Fatalf("07a selection = %d tuples, want 04 plus 1 package at 3 CPU settings", len(want))
	}
	for _, argv := range shard07b {
		for _, tp := range tuples(t, argv) {
			if want[tp] != 0 {
				t.Fatalf("07b addition %+v overlaps", tp)
			}
			want[tp]++
		}
	}
	if len(want) != 42 {
		t.Fatalf("07b selection = %d tuples, want 07a plus 1 package at 3 CPU settings", len(want))
	}
	for _, argv := range shard09a {
		for _, tp := range tuples(t, argv) {
			if want[tp] != 0 {
				t.Fatalf("09a addition %+v overlaps", tp)
			}
			want[tp]++
		}
	}
	if len(want) != 45 {
		t.Fatalf("09a selection = %d tuples, want 07b plus 1 package at 3 CPU settings", len(want))
	}
	for _, argv := range shard09b {
		for _, tp := range tuples(t, argv) {
			if want[tp] != 0 {
				t.Fatalf("09b addition %+v overlaps", tp)
			}
			want[tp]++
		}
	}
	if len(want) != 48 {
		t.Fatalf("09b selection = %d tuples, want 09a plus 1 package at 3 CPU settings", len(want))
	}
	for _, argv := range shard10b {
		for _, tp := range tuples(t, argv) {
			if want[tp] != 0 {
				t.Fatalf("10b addition %+v overlaps", tp)
			}
			want[tp]++
		}
	}
	if len(want) != 54 {
		t.Fatalf("10b selection = %d tuples, want 09b plus 2 packages at 3 CPU settings", len(want))
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
			if sh.Name != w.name || sh.Parallel != w.parallel || len(sh.Steps) != len(w.steps) || len(sh.CPUGroups) != len(w.cpuGroups) {
				t.Fatalf("%s shard %d = %+v, want %s parallel=%v", goos, i, sh, w.name, w.parallel)
			}
			gotSteps, wantSteps := slices.Clone(sh.Steps), slices.Clone(w.steps)
			for g := range sh.CPUGroups {
				if len(sh.CPUGroups[g]) != len(w.cpuGroups[g]) {
					t.Fatalf("%s %s per-CPU group %d = %+v", goos, sh.Name, g, sh.CPUGroups[g])
				}
				gotSteps, wantSteps = append(gotSteps, sh.CPUGroups[g]...), append(wantSteps, w.cpuGroups[g]...)
			}
			for j, s := range gotSteps {
				argv := strings.Join(s.Argv, " ")
				if s.Name != wantSteps[j][0] || argv != wantSteps[j][1] || strings.Join(s.Env, " ") != "CGO_ENABLED=1" {
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
				t.Errorf("%s: %+v selected %d times, 02b, 03, 04, 07a, 07b, 09a, 09b and 10b selected it %d times", goos, tp, n, want[tp])
			}
		}
		if len(got) != 54 {
			t.Fatalf("%s: %d distinct tuples, want 54", goos, len(got))
		}
		ownerOf := map[string]string{"1": "-cpu1", "2": "", "4": ""}
		for _, pkg := range []string{"plane", "sidecar"} {
			for _, cpu := range []string{"1", "2", "4"} {
				if tp := (tuple{"./internal/" + pkg, "", cpu, "20"}); got[tp] != 1 || owner[tp] != pkg+ownerOf[cpu] {
					t.Fatalf("%s: %s at -cpu=%s selected %d times, in %q, want once in %s", goos, pkg, cpu, got[tp], owner[tp], pkg+ownerOf[cpu])
				}
			}
		}
		held := map[string][]string{}
		for tp, sh := range owner {
			if tp.pkg == "./internal/plane" || tp.pkg == "./internal/sidecar" {
				held[sh] = append(held[sh], strings.TrimPrefix(tp.pkg, "./internal/")+"@"+tp.cpu)
			}
		}
		for sh, want := range map[string][]string{"plane-cpu1": {"plane@1"}, "plane": {"plane@2", "plane@4"}, "sidecar-cpu1": {"sidecar@1"}, "sidecar": {"sidecar@2", "sidecar@4"}} {
			slices.Sort(held[sh])
			if !slices.Equal(held[sh], want) {
				t.Fatalf("%s: shard %s holds %v, want exactly %v", goos, sh, held[sh], want)
			}
		}
		if len(held) != 4 {
			t.Fatalf("%s: plane or sidecar tuples in shards %v", goos, held)
		}
		// The headroom fixes: ./internal/contract, ./internal/mcpqual and
		// ./internal/workspace each run once per CPU setting, only in their
		// own per-CPU group of the packages shard, in that order.
		for g, pkg := range []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace"} {
			for j, cpu := range []string{"1", "2", "4"} {
				tp := tuple{pkg, "", cpu, "20"}
				if got[tp] != 1 || owner[tp] != "packages" || tuples(t, strings.Join(shards[0].CPUGroups[g][j].Argv, " "))[0] != tp {
					t.Fatalf("%s: %s at -cpu=%s selected %d times in %q, want once in packages per-CPU group %d", goos, pkg, cpu, got[tp], owner[tp], g+1)
				}
			}
		}
		steps, err := devcheck.StressSteps(goos)
		var flatSteps []string
		for _, s := range steps {
			flatSteps = append(flatSteps, strings.Join(s.Argv, " "))
		}
		if err != nil || len(steps) != 22 || !slices.Equal(flatSteps, flat) {
			t.Fatalf("%s StressSteps is not the twenty-two-step flattened shard view: %q", goos, flatSteps)
		}
	}
	// The functions shard keeps 02b's plane selector, whose executable
	// fixture and exclusions TestCISpeedSelection still runs.
	shards, _ := devcheck.StressShards("linux")
	if len(shards) != 7 || shards[6].Name != "functions" || !slices.Contains(shards[6].Steps[1].Argv, "-run="+speedPlaneSelector) {
		t.Fatalf("plane selector changed: %+v", shards)
	}
	// Fresh copies: a mutated plan, nested slices included, never leaks
	// into the next one.
	shards[6].Steps[1].Argv[6] = "-run=^(TestPlaneState)$"
	shards[1].Steps[0].Argv[4] = "-cpu=2"
	shards[2].Steps = shards[2].Steps[:1]
	again, _ := devcheck.StressShards("linux")
	if !slices.Contains(again[6].Steps[1].Argv, "-run="+speedPlaneSelector) || again[1].Steps[0].Argv[4] != "-cpu=1" || len(again[2].Steps) != 2 {
		t.Fatalf("StressShards shares state: %+v", again)
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
// commands and boundaries, runs the one CPU1 invocation of
// stress-plane-cpu1 and stress-sidecar-cpu1 alone and the CPU2/CPU4 pairs
// of stress-plane and stress-sidecar concurrently, joined with ordered logs
// (design 06a-perf), never overlaps two Parallel shards, fails with
// retained logs, and the concurrent coordinator's contract passes by name.
func TestStressShardExecution(t *testing.T) {
	for stage, want := range map[string][][]string{
		"stress":              shardArgv("packages", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "processgroup", "functions"),
		"stress-packages":     shardArgv("packages"),
		"stress-plane-cpu1":   shardArgv("plane-cpu1"),
		"stress-plane":        shardArgv("plane"),
		"stress-sidecar-cpu1": shardArgv("sidecar-cpu1"),
		"stress-sidecar":      shardArgv("sidecar"),
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
	// A failed invocation fails its shard after every invocation of that
	// shard ran (a failed CPU2 still joins CPU4), stops the aggregate before
	// the next shard and keeps its log: a plane CPU1 failure stops full
	// stress before the plane pair, a sidecar CPU1 failure before the
	// sidecar pair.
	for _, c := range []struct {
		stage, failOn, failed string
		want                  [][]string
	}{
		{"stress", "-cpu=1 -timeout=6m ./internal/plane", "stress plane cpu1", shardArgv("packages", "plane-cpu1")},
		{"stress-plane-cpu1", "-cpu=1 ", "stress plane cpu1", shardArgv("plane-cpu1")},
		{"stress", "-cpu=2 -timeout=6m ./internal/plane", "stress plane cpu2", shardArgv("packages", "plane-cpu1", "plane")},
		{"stress-plane", "-cpu=4 ", "stress plane cpu4", shardArgv("plane")},
		{"stress", "-cpu=1 -timeout=6m ./internal/sidecar", "stress sidecar cpu1", shardArgv("packages", "plane-cpu1", "plane", "sidecar-cpu1")},
		{"stress-sidecar-cpu1", "-cpu=1 ", "stress sidecar cpu1", shardArgv("sidecar-cpu1")},
		{"stress", "-cpu=2 -timeout=6m ./internal/sidecar", "stress sidecar cpu2", shardArgv("packages", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar")},
		{"stress-sidecar", "-cpu=2 ", "stress sidecar cpu2", shardArgv("sidecar")},
		{"stress", "-cpu=2 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu2",
			shardArgv("packages", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "processgroup")},
		{"stress-processgroup", "-cpu=4 ", "stress processgroup cpu4", shardArgv("processgroup")},
		{"stress-functions", "TestFP4TransportHarness", "stress function", shardArgv("functions")[:1]},
		// A failed combined invocation never starts the contract group; a
		// failed contract invocation fails the shard after its siblings ran.
		{"stress", "./internal/client", "stress packages", shardArgv("packages")[:1]},
		{"stress", "-cpu=2 -timeout=6m ./internal/contract", "stress contract cpu2", shardArgv("packages")[:2]},
		{"stress-packages", "-cpu=4 -timeout=6m ./internal/contract", "stress contract cpu4", shardArgv("packages")[:2]},
		{"stress", "-cpu=1 -timeout=6m ./internal/mcpqual", "stress mcpqual cpu1", shardArgv("packages")[:3]},
		{"stress-packages", "-cpu=2 -timeout=6m ./internal/workspace", "stress workspace cpu2", shardArgv("packages")},
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
	// Through devcheck.Run, with a separate gate per shard routed by
	// package and exact CPU argument (a CPU1 singleton never waits on its
	// pair's gate, and no gate is reused after its release): the 44 events
	// are packages 0-1, the contract group's three starts 2-4 and ends 5-7,
	// mcpqual's 8-10 and 11-13 and workspace's 14-16 and 17-19 (the
	// headroom fixes), the plane CPU1 singleton's start and end 20-21, the
	// plane pair's two starts 22-23 before either end 24-25, the sidecar
	// CPU1 singleton 26-27, the sidecar pair's starts 28-29 and ends 30-31,
	// processgroup's three starts 32-34 and ends 35-37, and the sequential
	// functions 38-43; no two Parallel shards overlap, and the logs replay
	// under their CPU names in CPU order.
	gates := map[string]*gate{}
	for _, g := range []struct {
		pkg  string
		cpus []string
	}{{"./internal/contract", []string{"1", "2", "4"}}, {"./internal/mcpqual", []string{"1", "2", "4"}}, {"./internal/workspace", []string{"1", "2", "4"}}, {"./internal/plane", []string{"1"}}, {"./internal/plane", []string{"2", "4"}}, {"./internal/sidecar", []string{"1"}},
		{"./internal/sidecar", []string{"2", "4"}}, {"./internal/spikes/processgroup", []string{"1", "2", "4"}}} {
		gt := newGate(len(g.cpus))
		for _, c := range g.cpus {
			gates["-cpu="+c+" "+g.pkg] = gt
		}
	}
	o := &overlapRunner{groups: gates}
	var out, errOut bytes.Buffer
	if code := devcheck.Run(context.Background(), []string{"stress"}, &out, &errOut, o.run); code != 0 {
		t.Fatalf("stress with overlap gates = %d: %s", code, errOut.String())
	}
	o.mu.Lock()
	events := slices.Clone(o.events)
	o.mu.Unlock()
	spans := map[string][]int{}
	for i, e := range events {
		for _, pkg := range []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace", "./internal/plane", "./internal/sidecar", "./internal/spikes/processgroup"} {
			if strings.HasSuffix(e, " "+pkg) {
				spans[pkg] = append(spans[pkg], i)
			}
		}
	}
	if len(events) != 44 || !slices.Equal(spans["./internal/contract"], []int{2, 3, 4, 5, 6, 7}) ||
		!slices.Equal(spans["./internal/mcpqual"], []int{8, 9, 10, 11, 12, 13}) || !slices.Equal(spans["./internal/workspace"], []int{14, 15, 16, 17, 18, 19}) ||
		!slices.Equal(spans["./internal/plane"], []int{20, 21, 22, 23, 24, 25}) || !slices.Equal(spans["./internal/sidecar"], []int{26, 27, 28, 29, 30, 31}) ||
		!slices.Equal(spans["./internal/spikes/processgroup"], []int{32, 33, 34, 35, 36, 37}) {
		t.Fatalf("events = %q", events)
	}
	for _, g := range []struct {
		from, n int
		cpus    []string
		pkg     string
	}{{2, 3, []string{"1", "2", "4"}, "./internal/contract"}, {8, 3, []string{"1", "2", "4"}, "./internal/mcpqual"}, {14, 3, []string{"1", "2", "4"}, "./internal/workspace"},
		{20, 1, []string{"1"}, "./internal/plane"}, {22, 2, []string{"2", "4"}, "./internal/plane"},
		{26, 1, []string{"1"}, "./internal/sidecar"}, {28, 2, []string{"2", "4"}, "./internal/sidecar"}, {32, 3, []string{"1", "2", "4"}, "./internal/spikes/processgroup"}} {
		var starts, ends []string
		for i := g.from; i < g.from+2*g.n; i++ {
			e := events[i]
			cpu := e[strings.Index(e, "-cpu=")+5 : strings.Index(e, " -timeout")]
			if !strings.HasSuffix(e, " "+g.pkg) || !slices.Contains(g.cpus, cpu) {
				t.Fatalf("event %d %q outside its group %s %v: %q", i, e, g.pkg, g.cpus, events)
			}
			if strings.HasPrefix(e, "start ") != (i < g.from+g.n) {
				t.Fatalf("an invocation of %s %v ended before all %d started: %q", g.pkg, g.cpus, g.n, events)
			}
			if i < g.from+g.n {
				starts = append(starts, cpu)
			} else {
				ends = append(ends, cpu)
			}
		}
		slices.Sort(starts)
		slices.Sort(ends)
		if !slices.Equal(starts, g.cpus) || !slices.Equal(ends, g.cpus) {
			t.Fatalf("%s %v: starts %v, ends %v", g.pkg, g.cpus, starts, ends)
		}
	}
	scratch := scratchOf(out.String())
	pos := -1
	for _, shard := range []string{"contract", "mcpqual", "workspace", "plane", "sidecar"} {
		for _, cpu := range []string{"1", "2", "4"} {
			name := "stress " + shard + " cpu" + cpu
			i := strings.Index(out.String(), "devcheck: ---- "+name+" log ("+filepath.Join(scratch, "stress-"+shard+"-cpu"+cpu+".log")+") ----\nok go test -race -count=20 -cpu="+cpu+" -timeout=6m ./internal/"+shard+"\n")
			if i < 0 || i < pos || !strings.Contains(out.String()[i:], "devcheck: "+name+": ok in ") {
				t.Fatalf("%s log not replayed in shard and CPU order:\n%s", name, out.String())
			}
			pos = i
		}
	}
	// The coordinator's contract, by name, with every named subtest: the
	// overlap, failure, watchdog and log contracts of the CPU1 singletons,
	// the CPU2/CPU4 pairs and processgroup, and the wave mechanism on the
	// full three-CPU processgroup shard.
	const pkg = "./internal/devcheck"
	bin := contractBinary(t, pkg)
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
// CPU argument and package (its last argument) name a gate until all of
// that gate's calls have arrived.
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
	key := ""
	for _, x := range argv {
		if strings.HasPrefix(x, "-cpu=") {
			key = x + " " + argv[len(argv)-1]
		}
	}
	if g := r.groups[key]; g != nil {
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

// shardJobs is the literal eighteen-job table (design 02c, Workflow
// topology, with design 05b's plane workers, its sidecar follow-up's
// sidecar workers and design 06a-perf's plane and sidecar CPU1 workers in
// CI-plan order).
var shardJobs = []struct {
	id, name, runner, timeout string
}{
	{"linux", "ci-linux", "ubuntu-24.04", "45"},
	{"macos", "ci-macos", "macos-15", "30"},
	{"linux-stress-packages", "ci-linux-stress-packages", "ubuntu-24.04", "20"},
	{"linux-stress-plane-cpu1", "ci-linux-stress-plane-cpu1", "ubuntu-24.04", "20"},
	{"linux-stress-plane", "ci-linux-stress-plane", "ubuntu-24.04", "20"},
	{"linux-stress-sidecar-cpu1", "ci-linux-stress-sidecar-cpu1", "ubuntu-24.04", "20"},
	{"linux-stress-sidecar", "ci-linux-stress-sidecar", "ubuntu-24.04", "20"},
	{"linux-stress-processgroup", "ci-linux-stress-processgroup", "ubuntu-24.04", "20"},
	{"linux-stress-functions", "ci-linux-stress-functions", "ubuntu-24.04", "20"},
	{"macos-stress-packages", "ci-macos-stress-packages", "macos-15", "20"},
	{"macos-stress-plane-cpu1", "ci-macos-stress-plane-cpu1", "macos-15", "20"},
	{"macos-stress-plane", "ci-macos-stress-plane", "macos-15", "20"},
	{"macos-stress-sidecar-cpu1", "ci-macos-stress-sidecar-cpu1", "macos-15", "20"},
	{"macos-stress-sidecar", "ci-macos-stress-sidecar", "macos-15", "20"},
	{"macos-stress-processgroup", "ci-macos-stress-processgroup", "macos-15", "20"},
	{"macos-stress-functions", "ci-macos-stress-functions", "macos-15", "20"},
	{"linux-stress", "ci-linux-stress", "ubuntu-24.04", "5"},
	{"macos-stress", "ci-macos-stress", "ubuntu-24.04", "5"},
}

// summaryCommand is design 06a-perf's literal seven-result predicate.
const summaryCommand = `test "$PACKAGES_RESULT" = success && test "$PLANE_CPU1_RESULT" = success && test "$PLANE_RESULT" = success && test "$SIDECAR_CPU1_RESULT" = success && test "$SIDECAR_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`

// summaryKeys are the summary step's environment keys in needs order.
var summaryKeys = []string{"PACKAGES_RESULT", "PLANE_CPU1_RESULT", "PLANE_RESULT", "SIDECAR_CPU1_RESULT", "SIDECAR_RESULT", "PROCESSGROUP_RESULT", "FUNCTIONS_RESULT"}

// summaryWorkers are the shard suffixes of a summary's needs, in order.
var summaryWorkers = []string{"packages", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "processgroup", "functions"}

// summaryDeadline bounds the whole summary test, structural checks and
// every shell execution included. It is a hang guard in the sense of
// gate.wait, never a timing oracle: 170 short bash children finish in a
// few seconds, and only a stuck child reaches it.
const summaryDeadline = 20 * time.Second

// FP-3: the actual workflow has the eighteen jobs and two literal
// summaries, each binding its own platform's seven workers in order, and
// the shared seven-result command passes only when all seven workers
// concluded success: a complete proof in 170 bash executions (design
// 06a-perf, D1).
func TestStressShardSummaries(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), summaryDeadline)
	defer cancel()
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
	if !slices.Equal(ids, wantIDs) || len(ids) != 18 {
		t.Fatalf("job ids = %v, want %v", ids, wantIDs)
	}
	// The two main jobs and the fourteen workers are independent: no
	// dependency, condition, matrix or error bypass.
	for _, j := range shardJobs[:16] {
		job := node(t, jobs, j.id)
		for i := 0; i+1 < len(job.Content); i += 2 {
			if k := job.Content[i].Value; k == "needs" || k == "if" || k == "strategy" || k == "continue-on-error" || k == "concurrency" {
				t.Fatalf("%s is not independent: has %s", j.id, k)
			}
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is required to execute the summaries' literal command: %v", err)
	}
	runs := map[string]string{}
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
		var workers []string
		for _, w := range summaryWorkers {
			workers = append(workers, plat+"-stress-"+w)
		}
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
		// Seven ordered key/value pairs: fourteen YAML nodes, each result
		// bound to its own platform's worker in needs order.
		env := node(t, steps, 0, "env")
		if len(env.Content) != 14 {
			t.Fatalf("%s env has %d entries", id, len(env.Content)/2)
		}
		for i, k := range summaryKeys {
			if env.Content[2*i].Value != k {
				t.Fatalf("%s env key %d = %q, want %q", id, i, env.Content[2*i].Value, k)
			}
			if got := env.Content[2*i+1].Value; got != "${{ needs['"+workers[i]+"'].result }}" {
				t.Fatalf("%s %s = %q", id, k, got)
			}
		}
		runs[plat] = node(t, steps, 0, "run").Value
	}
	// Both summaries run the byte-identical specified literal, asserted
	// before the shared predicate is executed once for both.
	if runs["linux"] != summaryCommand || runs["macos"] != summaryCommand || runs["linux"] != runs["macos"] {
		t.Fatalf("summary runs linux %q, macos %q, want %q", runs["linux"], runs["macos"], summaryCommand)
	}
	run := runs["linux"]
	if err := ctx.Err(); err != nil {
		t.Fatalf("summary hang guard (%v) expired before the shell proof: %v", summaryDeadline, err)
	}
	// execute runs the extracted command as Actions' bash shell does, with
	// the seven results bound to the keys, under the test's one deadline.
	// On expiry CommandContext kills the shell and Run waits for it before
	// the failure is reported; no case is skipped or retried.
	executions := 0
	execute := func(tp [7]string) bool {
		t.Helper()
		cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-eo", "pipefail", "-c", run)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for i, k := range summaryKeys {
			cmd.Env = append(cmd.Env, k+"="+tp[i])
		}
		out, err := cmd.CombinedOutput()
		executions++
		if ctxErr := ctx.Err(); ctxErr != nil {
			t.Fatalf("summary hang guard (%v) expired at execution %d %q: %v", summaryDeadline, executions, tp, ctxErr)
		}
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatalf("%q: bash did not run: %v", tp, err)
		}
		if len(out) != 0 {
			t.Fatalf("%q: the predicate printed %q", tp, out)
		}
		return err == nil
	}
	allSuccess := [7]string{"success", "success", "success", "success", "success", "success", "success"}
	// Step 1: all 2^7 boolean-class assignments, each position success or
	// failure; exactly the all-success one exits zero, so every single and
	// multiple failure pattern fails.
	passed := 0
	for mask := 0; mask < 1<<7; mask++ {
		var tp [7]string
		for i := range tp {
			tp[i] = "success"
			if mask&(1<<i) != 0 {
				tp[i] = "failure"
			}
		}
		ok := execute(tp)
		if ok != (tp == allSuccess) {
			t.Fatalf("%q: pass=%v, want %v", tp, ok, tp == allSuccess)
		}
		if ok {
			passed++
		}
	}
	// Step 2: every other result spelling, 7 × 6, in each position with
	// all others success: the remaining standard conclusions and the
	// unusual values each fail.
	odd := []string{"cancelled", "skipped", "", "neutral", "Success", "success "}
	for pos := 0; pos < 7; pos++ {
		for _, v := range odd {
			tp := allSuccess
			tp[pos] = v
			if execute(tp) {
				t.Fatalf("%s=%q with every other result success passed", summaryKeys[pos], v)
			}
		}
	}
	if executions != 128+7*6 || passed != 1 {
		t.Fatalf("%d executions, %d passed", executions, passed)
	}
	t.Logf("summary proof: %d bash executions, %d passed, test elapsed %v (hang guard %v)", executions, passed, time.Since(start).Round(time.Millisecond), summaryDeadline)
}

// FP-4: the validator accepts the actual workflow and rejects the previous
// fourteen-job topology, a removed or miswired worker (the CPU1 workers of
// design 06a-perf included), any removed worker or a weakened summary;
// contract, stages, extracted sequences and both OS plans agree, and the
// four required contexts are unchanged.
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
	if len(contract) != 18 || len(stages) != 18 {
		t.Fatalf("contract %d jobs, workflow %d", len(contract), len(stages))
	}
	// Jobs are not child invocations: fourteen workers run one stage each,
	// and those stages dispatch 44 invocations on the two platforms (26
	// before the contract headroom fix split the contract package per CPU
	// inside stress-packages, 32 before the workspace and mcpqual headroom
	// fix split those two likewise; no job was added).
	workerJobs, invocations := 0, 0
	dispatch := devcheck.Stages()
	for _, goos := range []string{"linux", "darwin"} {
		shards, err := devcheck.StressShards(goos)
		if err != nil || len(shards) != 7 {
			t.Fatal(shards, err)
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
			workerJobs++
			r := &syncRunner{}
			var out, errOut bytes.Buffer
			if code := devcheck.Run(context.Background(), j.Stages[0:1], &out, &errOut, r.run); code != 0 ||
				!sameGroups(r.calls, shardArgv(strings.TrimPrefix(j.Stages[0], "stress-"))) {
				t.Fatalf("%s dispatch = %d %q %s", j.ID, code, r.calls, errOut.String())
			}
			invocations += len(r.calls)
			os.RemoveAll(scratchOf(out.String()))
		}
	}
	if workerJobs != 14 || invocations != 44 {
		t.Fatalf("%d worker jobs dispatch %d invocations, want 14 and 44 (twenty-two per platform)", workerJobs, invocations)
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
	// The previous fourteen-job topology (five workers and five results
	// per platform) is incomplete under the eighteen-job contract.
	previous := mutated(t, func(r *yaml.Node) {
		for _, plat := range []string{"linux", "macos"} {
			for _, sh := range []string{"plane-cpu1", "sidecar-cpu1"} {
				deleteKey(t, node(t, r, "jobs"), plat+"-stress-"+sh)
			}
			id := plat + "-stress"
			needs := node(t, r, "jobs", id, "needs")
			needs.Content = append(append(append([]*yaml.Node{}, needs.Content[0]), needs.Content[2]), needs.Content[4:]...)
			env := node(t, r, "jobs", id, "steps", 0, "env")
			deleteKey(t, env, "PLANE_CPU1_RESULT")
			deleteKey(t, env, "SIDECAR_CPU1_RESULT")
			node(t, r, "jobs", id, "steps", 0, "run").Value = `test "$PACKAGES_RESULT" = success && test "$PLANE_RESULT" = success && test "$SIDECAR_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`
		}
	})
	if ids, err := cicheck.ExtractStages(previous); err != nil || len(ids) != 14 {
		t.Fatalf("previous topology has %d jobs: %v", len(ids), err)
	}
	for _, want := range []string{"jobs.linux-stress-plane-cpu1: missing required field", "jobs.linux-stress-sidecar-cpu1: missing required field",
		"jobs.macos-stress-plane-cpu1: missing required field", "jobs.macos-stress-sidecar-cpu1: missing required field",
		"jobs.linux-stress.needs: must be exactly", "jobs.macos-stress.needs: must be exactly",
		"jobs.linux-stress.steps[0].env.PLANE_CPU1_RESULT: missing required field", "jobs.macos-stress.steps[0].env.SIDECAR_CPU1_RESULT: missing required field",
		"jobs.linux-stress.steps[0].run: must be exactly", "jobs.macos-stress.steps[0].run: must be exactly"} {
		mustReject(t, "previous fourteen-job topology", previous, want)
	}
	// Weakening either summary fails, at the positions of design 05b's
	// plane and sidecar workers (now 2 and 4) and of design 06a-perf's CPU1
	// workers (1 and 3).
	for _, id := range []string{"linux-stress", "macos-stress"} {
		p := "jobs." + id
		other := map[string]string{"linux-stress": "macos", "macos-stress": "linux"}[id]
		setRun := func(v string) []byte {
			return mutated(t, func(r *yaml.Node) { node(t, r, "jobs", id, "steps", 0, "run").Value = v })
		}
		without := func(key string) string {
			return strings.Replace(summaryCommand, ` && test "$`+key+`" = success`, "", 1)
		}
		mustReject(t, id+" always removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs", id), "if") }), p+".if: missing required field")
		mustReject(t, id+" skipped gate", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "if").Value = "${{ needs." + id + "-packages.result == 'success' }}"
		}), p+".if: expressions are not allowed")
		mustReject(t, id+" CPU1 skipped gate", mutated(t, func(r *yaml.Node) {
			node(t, r, "jobs", id, "if").Value = "${{ always() && needs['" + id + "-plane-cpu1'].result == 'success' }}"
		}), p+".if: expressions are not allowed")
		mustReject(t, id+" worker dropped", mutated(t, func(r *yaml.Node) {
			n := node(t, r, "jobs", id, "needs")
			n.Content = n.Content[:2]
		}), p+".needs: must be exactly")
		mustReject(t, id+" bypass", setRun(summaryCommand+" || true"), p+".steps[0].run: must be exactly")
		mustReject(t, id+" check dropped", setRun(without("FUNCTIONS_RESULT")), p+".steps[0].run: must be exactly")
		mustReject(t, id+" five-result summary", setRun(
			`test "$PACKAGES_RESULT" = success && test "$PLANE_RESULT" = success && test "$SIDECAR_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`), p+".steps[0].run: must be exactly")
		mustReject(t, id+" altered conjunction", setRun(strings.Replace(summaryCommand, ` && test "$SIDECAR_CPU1_RESULT"`, ` || test "$SIDECAR_CPU1_RESULT"`, 1)), p+".steps[0].run: must be exactly")
		mustReject(t, id+" mismatched order", setRun(strings.Replace(summaryCommand, `test "$PLANE_CPU1_RESULT" = success && test "$PLANE_RESULT" = success`, `test "$PLANE_RESULT" = success && test "$PLANE_CPU1_RESULT" = success`, 1)), p+".steps[0].run: must be exactly")
		for _, c := range []struct {
			shard, key string
			pos        int
		}{{"plane-cpu1", "PLANE_CPU1_RESULT", 1}, {"plane", "PLANE_RESULT", 2}, {"sidecar-cpu1", "SIDECAR_CPU1_RESULT", 3}, {"sidecar", "SIDECAR_RESULT", 4}} {
			w := id + "-" + c.shard
			mustReject(t, id+" "+c.shard+" comparison omitted", setRun(without(c.key)), p+".steps[0].run: must be exactly")
			mustReject(t, id+" "+c.shard+" result miswired", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "steps", 0, "env", c.key).Value = "${{ needs['" + id + "-packages'].result }}"
			}), p+".steps[0].env."+c.key+": must be")
			mustReject(t, id+" "+c.shard+" outcome not result", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "steps", 0, "env", c.key).Value = "${{ needs['" + w + "'].outcome }}"
			}), p+".steps[0].env."+c.key+": must be")
			mustReject(t, id+" "+c.shard+" result from the other platform", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "steps", 0, "env", c.key).Value = "${{ needs['" + other + "-stress-" + c.shard + "'].result }}"
			}), p+".steps[0].env."+c.key+": expressions are not allowed")
			mustReject(t, id+" "+c.shard+" result removed", mutated(t, func(r *yaml.Node) {
				deleteKey(t, node(t, r, "jobs", id, "steps", 0, "env"), c.key)
			}), p+".steps[0].env."+c.key+": missing required field")
			mustReject(t, id+" "+c.shard+" worker not needed", mutated(t, func(r *yaml.Node) {
				n := node(t, r, "jobs", id, "needs")
				n.Content = append(n.Content[:c.pos:c.pos], n.Content[c.pos+1:]...)
			}), p+".needs: must be exactly")
			mustReject(t, id+" "+c.shard+" worker from the other platform", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "needs").Content[c.pos].Value = other + "-stress-" + c.shard
			}), fmt.Sprintf("%s.needs[%d]: must be", p, c.pos))
		}
		mustReject(t, id+" CPU1 worker after its pair", mutated(t, func(r *yaml.Node) {
			n := node(t, r, "jobs", id, "needs")
			n.Content[1], n.Content[2] = n.Content[2], n.Content[1]
		}), p+".needs[1]: must be")
		mustReject(t, id+" error bypass", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "continue-on-error", "true") }), p+".continue-on-error: unknown field")
		mustReject(t, id+" setup added", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "env", "x") }), p+".env: unknown field")
	}
	// The workers never depend on anything and are never conditional.
	mustReject(t, "worker needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "macos-stress-processgroup"), "needs", "macos") }), "jobs.macos-stress-processgroup.needs: unknown field")
	mustReject(t, "plane worker needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "linux-stress-plane"), "needs", "linux-stress-packages") }), "jobs.linux-stress-plane.needs: unknown field")
	mustReject(t, "sidecar worker needs", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", "macos-stress-sidecar"), "needs", "macos-stress-plane") }), "jobs.macos-stress-sidecar.needs: unknown field")
	mustReject(t, "plane pair needs its CPU1 worker", mutated(t, func(r *yaml.Node) {
		setKey(node(t, r, "jobs", "linux-stress-plane"), "needs", "linux-stress-plane-cpu1")
	}), "jobs.linux-stress-plane.needs: unknown field")
	// The CPU1 workers' and the pairs' own stage, flags, budget, runner,
	// offline check environment, independence and display name are fixed.
	pkgs := map[string]string{"plane-cpu1": "plane", "plane": "plane", "sidecar-cpu1": "sidecar", "sidecar": "sidecar"}
	sibling := map[string]string{"plane-cpu1": "stress-plane", "plane": "stress-plane-cpu1", "sidecar-cpu1": "stress-sidecar", "sidecar": "stress-sidecar-cpu1"}
	for _, shard := range []string{"plane-cpu1", "plane", "sidecar-cpu1", "sidecar"} {
		for _, plat := range []string{"linux", "macos"} {
			id := plat + "-stress-" + shard
			p := "jobs." + id
			stage := "stress-" + shard
			runner := map[string]string{"linux": "ubuntu-24.04", "macos": "macos-15"}[plat]
			mustReject(t, id+" stage", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "steps", 3, "run").Value = "go run ./cmd/devcheck stress-packages"
			}), p+`.steps[3].run: must run devcheck stage "`+stage+`", got "stress-packages"`)
			mustReject(t, id+" sibling stage", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "steps", 3, "run").Value = "go run ./cmd/devcheck " + sibling[shard]
			}), p+`.steps[3].run: must run devcheck stage "`+stage+`", got "`+sibling[shard]+`"`)
			mustReject(t, id+" count", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "steps", 3, "run").Value = "go test -race -count=1 -cpu=4 -timeout=6m ./internal/" + pkgs[shard]
			}), p+`.steps[3].run: must be "go run ./cmd/devcheck `+stage+`"`)
			mustReject(t, id+" cpu", mutated(t, func(r *yaml.Node) {
				node(t, r, "jobs", id, "steps", 3, "run").Value = "go run ./cmd/devcheck " + stage + " -cpu=4"
			}), p+`.steps[3].run: must be "go run ./cmd/devcheck `+stage+`"`)
			mustReject(t, id+" budget", mutated(t, func(r *yaml.Node) { node(t, r, "jobs", id, "timeout-minutes").Value = "30" }), p+`.timeout-minutes: must be "20", got "30"`)
			mustReject(t, id+" runner", mutated(t, func(r *yaml.Node) { node(t, r, "jobs", id, "runs-on").Value = "ubuntu-latest" }),
				p+`.runs-on: must be "`+runner+`", got "ubuntu-latest"`)
			mustReject(t, id+" online check", mutated(t, func(r *yaml.Node) { node(t, r, "jobs", id, "steps", 3, "env", "GOPROXY").Value = "direct" }),
				p+`.steps[3].env.GOPROXY: must be "off", got "direct"`)
			mustReject(t, id+" conditional", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "if", "always()") }), p+".if: unknown field")
			mustReject(t, id+" dependent", mutated(t, func(r *yaml.Node) { setKey(node(t, r, "jobs", id), "needs", plat+"-stress-packages") }), p+".needs: unknown field")
			if err := cicheck.CheckJobNames(map[string][]byte{"ci.yml": data, "other.yml": []byte("jobs:\n  x:\n    name: ci-" + id + "\n")}); err == nil ||
				!strings.Contains(err.Error(), fmt.Sprintf("job name %q is used by both ci.yml jobs.%s and other.yml jobs.x", "ci-"+id, id)) {
				t.Fatalf("%s duplicate display name: %v", id, err)
			}
		}
	}
}

// FP-5: docs/ci.md documents the four required contexts versus the
// fourteen workers, the seven shards with their exact commands (twenty-two
// since the headroom fixes, still the iteration 02b selection), the CPU1 singletons and CPU2/CPU4
// pairs with their log ownership and failure boundaries, why CPU1 is
// isolated (design 06a-perf), the preserved budgets and contexts, estimates
// versus observations, the superseded plane fallback, the history of the
// sidecar follow-up and the owner's fixed first-remote-run rule.
func TestStressShardHandoff(t *testing.T) {
	checks := docSection(t, "Checks")
	requireTerms(t, "Checks", checks,
		"Eighteen fixed jobs run on every trigger",
		"Exactly four of them are the required status check contexts on `main`: `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`",
		"The other fourteen are the stress workers", "seven shards per platform", "not required contexts",
		"| `ci-linux-stress` | `ubuntu-24.04` | 5 min | required summary |", "| `ci-macos-stress` | `ubuntu-24.04` | 5 min | required summary |",
		"succeeds only if the seven Linux workers all concluded `success`", "succeeds only if the seven macOS workers all concluded `success`",
		"if: ${{ always() }}", summaryCommand, "PLANE_CPU1_RESULT: ${{ needs['linux-stress-plane-cpu1'].result }}",
		"PLANE_RESULT: ${{ needs['linux-stress-plane'].result }}", "SIDECAR_CPU1_RESULT: ${{ needs['linux-stress-sidecar-cpu1'].result }}",
		"SIDECAR_RESULT: ${{ needs['linux-stress-sidecar'].result }}",
		"needs: [linux-stress-packages, linux-stress-plane-cpu1, linux-stress-plane, linux-stress-sidecar-cpu1, linux-stress-sidecar, linux-stress-processgroup, linux-stress-functions]",
		"Only all seven results `success` exit zero",
		"`failure`, `cancelled`, `skipped`, an empty or any unknown result in any position fails the summary",
		"never put on the job's `if`", "Summaries hold no stress logs", "all fourteen workers check out",
		"`devcheck stress-plane` on Linux: `internal/plane` CPU 2 and CPU 4 as two concurrent invocations",
		"`devcheck stress-plane-cpu1` on Linux: `internal/plane` at CPU 1, one invocation alone on its worker (iteration 06a-perf)")
	for _, j := range shardJobs[2:16] {
		requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `%s` | %s min | worker |", j.name, j.runner, j.timeout))
	}
	for _, stale := range []string{"An all-success triple alone exits zero", "The other six are the stress workers", "Ten fixed jobs run on every trigger",
		"The other eight are the stress workers", "Twelve fixed jobs run on every trigger", "Only all four results `success` exit zero",
		"Fourteen fixed jobs run on every trigger", "The other ten are the stress workers", "Only all five results `success` exit zero",
		"five shards per platform", "all ten workers check out", "succeeds only if the five Linux workers", "its three CPU settings as concurrent invocations (iteration 05b)"} {
		if strings.Contains(strings.Join(strings.Fields(checks), " "), stale) {
			t.Fatalf("Checks keeps the obsolete %q", stale)
		}
	}
	stress := docSection(t, "Stress checks")
	for _, sh := range shardPlan {
		requireTerms(t, "Stress checks", stress, "`devcheck stress-"+sh.name+"`")
		for _, s := range append(slices.Clone(sh.steps), slices.Concat(sh.cpuGroups...)...) {
			if !strings.Contains(stress, "\n"+s[1]+"\n") {
				t.Fatalf("Stress checks lacks the command %q", s[1])
			}
			requireTerms(t, "Stress checks", stress, "`"+s[0]+"`")
		}
	}
	requireTerms(t, "Stress checks", stress,
		"The project's declared repeat count is 20 per CPU setting (1, 2, 4).",
		"seven shards per platform", "The seven shards run twenty-two commands",
		// The headroom fixes: the packages shard's per-CPU groups.
		"| `packages` | `devcheck stress-packages` | `stress packages`, then `stress contract cpu1`, `cpu2`, `cpu4`, then `stress mcpqual cpu1`, `cpu2`, `cpu4`, then `stress workspace cpu1`, `cpu2`, `cpu4` | one invocation, then one group per split package (`internal/contract`, `internal/mcpqual`, `internal/workspace`), one group after another, each three concurrent invocations, one per CPU setting (the headroom fixes) |",
		"`stressWaves` never applies to them", "`stress-contract-cpu1.log`", "`stress-workspace-cpu4.log`",
		"Why contract, mcpqual and workspace run per CPU setting", "run 36992345488", "run 37009626145",
		// Iteration 10b's r0.6 schedule: workspacetransfer tried a fourth
		// per-CPU group (r0.5) and returned to the combined invocation.
		"a group of its own was later tried and withdrawn", "Then iteration 10b's r0.6 schedule (2026-10-04) returned",
		"at most three invocations run at once on either runner",
		"| `plane-cpu1` | `devcheck stress-plane-cpu1` | `stress plane cpu1` | one invocation, alone on its worker (iteration 06a-perf) |",
		"| `plane` | `devcheck stress-plane` | `stress plane cpu2`, `cpu4` | two concurrent invocations, one per CPU setting (iteration 05b; CPU 1 moved to `plane-cpu1` in iteration 06a-perf) |",
		"| `sidecar-cpu1` | `devcheck stress-sidecar-cpu1` | `stress sidecar cpu1` | one invocation, alone on its worker (iteration 06a-perf) |",
		"| `sidecar` | `devcheck stress-sidecar` | `stress sidecar cpu2`, `cpu4` | two concurrent invocations, one per CPU setting (iteration 05b sidecar follow-up; CPU 1 moved to `sidecar-cpu1` in iteration 06a-perf) |",
		"| `processgroup` | `devcheck stress-processgroup` | `stress processgroup cpu1`, `cpu2`, `cpu4` | three concurrent invocations, one per CPU setting |",
		// Execution, log ownership and failure boundaries.
		"The plane-cpu1, plane, sidecar-cpu1, sidecar and processgroup shards are concurrent", "at most three at once within that shard", "no two shards overlap",
		"`devcheck stress-plane` and `devcheck stress-sidecar` now run CPU 2 and CPU 4 only",
		"run both `devcheck stress-plane-cpu1` and `devcheck stress-plane` (or `devcheck stress`)",
		"`devcheck stress-plane-cpu1` owns only `stress-plane-cpu1.log`", "`devcheck stress-plane` only `stress-plane-cpu2.log` and `stress-plane-cpu4.log`",
		"no log is duplicated into the CPU 2 and CPU 4 worker",
		"A failure in `plane-cpu1` during `devcheck stress` prevents `plane` and every later shard from starting",
		"a failed CPU 2 invocation still waits for CPU 4 before its shard fails",
		"does not cancel its siblings", "replayed from disk", "in CPU order", "`stress-plane-cpu1.log`", "`stress-sidecar-cpu1.log`",
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
		"four vCPUs (`ubuntu-24.04`)", "three M1 cores (`macos-15`, arm64)",
		"record the actual architecture and core count", "`testWait` and helper joins (20 s)", "`TestServerFailureContract/shutdown-deadline`",
		"event synchronization is not immunity to overload", "never retried to green",
		// Why CPU 1 is isolated (design 06a-perf), with its evidence.
		"Plane and sidecar CPU 1 run on workers of their own (iteration 06a-perf)", "competing race binaries",
		"7.829 s", "8.019 s", "167.493 s", "128.827 s", "106.710 s", "118.77 s of CPU samples over 166.32 s",
		"racecall 15.22%", "13.90%", "12.51%", "4.13%", "it does not measure an isolated hosted worker",
		"not an isolated-CPU baseline", "135–168 s",
		"| plane | 223.8 / 173.9 / 149.3 | 322.4 / 229.2 / 197.2 |", "| sidecar | 238.6 / 188.6 / 164.3 | 327.1 / 240.0 / 207.9 |",
		"The historical macOS plane CPU 4 run failed", "1.441", "1.371", "No workload-cost saving is assumed",
		// The superseded plane fallback, kept as history.
		"Pre-authorised plane fallback (superseded)", "`stressWaves`", "It is empty",
		"`devcheck: stress plane cpuN: ok in Xs`", "`devcheck: stress plane cpuN: FAILED after Xs`", "X > 300.0 s", "exactly 300.0 does not",
		"`panic: test timed out after 6m0s`", "Missing logs were an evidence blocker, not a trigger",
		"both a trigger and an investigation blocker", "`\"plane\": {{4}, {1, 2}}`", "`TestStressConcurrencyContract/waves`", "it is not active",
		"The fallback did not trigger", "146.6 s", "202.8 s",
		"the two-step plane shard now rejects it before any scratch directory or child exists", "the wave mechanism is proven on the three-CPU processgroup shard",
		// The sidecar risk, its decided follow-up and the trigger that applied it.
		"Sidecar has its own concurrent shard (iteration 05b sidecar follow-up, applied)", "310 s", "50 s (14%)", "260–310 s post-change estimate",
		"sidecar over 300.0 s", "no temporary workflow is added",
		"the follow-up is a fifth shard running sidecar's three CPU settings as concurrent invocations",
		"A fifth sequential shard is not an option",
		"`ok internal/sidecar 307.788s`", "`247.885s`", "The rule triggered on Linux",
		"`stress-sidecar`", "`ci-linux-stress-sidecar`", "`ci-macos-stress-sidecar`",
		"3 × 20 × 3 = 180 additional children", "in the sidecar shard since the iteration 05b sidecar follow-up",
		"both native hosted sidecar workers must pass", "no further concurrency escalation or timing relaxation is authorised",
		// Estimates versus measurements.
		"Iteration 05b allocation", "explicit planning estimates, not measured speedups or hard upper bounds",
		"about 140–225 s per Linux and 150–255 s per macOS plane invocation", "135 s / 105 s below the 6-minute limit",
		"dividing the hosted alarm by three would be unsound", "about 300–380 s plus summary scheduling",
		"current evidence does not support promising 300 s or less",
		"Iteration 05b sidecar follow-up allocation", "revised planning estimates, not measurements",
		"Iteration 06a-perf allocation", "planning estimates of command time, not job upper bounds",
		"| `stress-plane-cpu1` | about 224 s | about 322 s; desired ≤250 s |", "| `stress-plane` (CPU 2 and 4) | about 174 s | about 229 s |",
		"| `stress-sidecar-cpu1` | about 239 s | about 327 s; desired ≤250 s |", "| `stress-sidecar` (CPU 2 and 4) | about 189 s | about 240 s |",
		"the larger of the historical CPU 2 and CPU 4 invocation times, not their sum",
		"22.46%", "23.57%", "6.95%", "8.29%", "no supplied data proves either speedup", "Do not divide time by three",
		"The split adds four setups and may increase total runner minutes",
		"Measured with iteration 06a-perf (CPU 1 shards)", "local timing is not a hosted macOS qualification",
		"Measured with iteration 05b (plane shard)", "Hosted and local figures come from different machines",
		"it is not a hosted estimate", "Hosted iteration 05b times: measured by run 36325089074",
		"Measured with the iteration 05b sidecar follow-up (sidecar shard)",
		"Hosted run 36325089074", "`devcheck: stress plane cpu1: ok in 146.6s`", "`devcheck: stress plane cpu1: ok in 202.8s`",
		"`devcheck: stress packages: ok in 366.5s`", "`317.9s`", "6 min 42 s", "core count is not printed",
		"Hosted run 36315881191", "395.8 s", "432.6 s",
		"Expected per-job wall-clock after iteration 05b", "planning estimates, not measurements",
		"Expected per-job wall-clock after the iteration 05b sidecar follow-up",
		"Expected per-job wall-clock after iteration 06a-perf",
		// Historical 02c figures stay labelled as history.
		"run 36236333755", "Measured with iteration 02c", "Expected per-job wall-clock after iteration 02c",
		"macOS 02c worker times: pending until")
	expected := stress[strings.Index(stress, "Expected per-job wall-clock after iteration 06a-perf"):]
	for _, j := range shardJobs {
		requireTerms(t, "Stress checks expected times after iteration 06a-perf", expected, "`"+j.name+"` about")
	}
	for _, stale := range []string{"Only the processgroup shard is concurrent", "Why only processgroup is concurrent", "so it and the function commands stay sequential",
		"Sidecar stays in the packages shard", "The four shards run ten commands", "The plane and processgroup shards are concurrent",
		"Hosted iteration 05b times: pending", "Follow-up rule, decided in advance and inactive",
		"The five shards run thirteen commands", "The plane, sidecar and processgroup shards are concurrent", "Pre-authorised plane fallback (inactive)",
		"so plane, sidecar and processgroup each start all three at once", "all five shards in one process under one shared 15-minute watchdog rather than five",
		// Iteration 10b's r0.6 schedule withdrew r0.5's fourth group: the
		// split has been tried and measured, so no next-candidate claim.
		"it is the next candidate for a group of its own", "The seven shards run twenty-five commands",
		"`stress workspacetransfer cpu1`", "`stress-workspacetransfer-cpu1.log`"} {
		if strings.Contains(strings.Join(strings.Fields(stress), " "), stale) {
			t.Fatalf("Stress checks keeps the obsolete %q", stale)
		}
	}
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp, "fourteen worker contexts are not required",
		"no protection change is needed for 02c, 05b, its sidecar follow-up or 06a-perf")
	// The pull request #5 handoff, kept as history: no protection change,
	// all fourteen jobs of that topology.
	handoffOf := func(sub string) string {
		t.Helper()
		i := strings.Index(bp, sub)
		if i < 0 {
			t.Fatalf("Branch protection lacks %q", strings.TrimSpace(sub))
		}
		h := bp[i+len(sub):]
		if j := strings.Index(h, "\n### "); j >= 0 {
			h = h[:j]
		}
		return h
	}
	handoff := handoffOf("\n### Plane and sidecar workers (pull request #5)\n")
	requireTerms(t, "Plane and sidecar workers", handoff, "`ci-linux-stress-plane`", "`ci-macos-stress-plane`",
		"`ci-linux-stress-sidecar`", "`ci-macos-stress-sidecar`",
		"The required contexts stay exactly `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`", "no protection change is made")
	steps := numberedSteps(t, handoff)
	review := stepIndex(t, steps, "REVIEW_APPROVED", "iterations 05 and 05b", "sidecar follow-up", "commit the reviewed code")
	push := stepIndex(t, steps, "Push `iter-05-dispatch`", "pull request #5")
	green := stepIndex(t, steps, "all fourteen jobs", "ten stress workers", "all four required checks", "current PR merge revision")
	verify := stepIndex(t, steps, "owner verifies", "read-only", "same four required contexts", "no plane or sidecar worker")
	merge := stepIndex(t, steps, "Merge iterations 05 and 05b together", "all four checks green")
	if !(review < push && push < green && green < verify && verify < merge) {
		t.Fatalf("handoff order review=%d push=%d green=%d verify=%d merge=%d", review, push, green, verify, merge)
	}
	// The pull request #6 handoff (design 06a-perf): four diagnostic CPU1
	// workers, eighteen jobs, the same four required contexts.
	handoff = handoffOf("\n### CPU1 workers (pull request #6)\n")
	requireTerms(t, "CPU1 workers", handoff, "`ci-linux-stress-plane-cpu1`", "`ci-macos-stress-plane-cpu1`",
		"`ci-linux-stress-sidecar-cpu1`", "`ci-macos-stress-sidecar-cpu1`", "both summaries now take seven results",
		"The required contexts stay exactly `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`", "no protection change is made")
	steps = numberedSteps(t, handoff)
	review = stepIndex(t, steps, "REVIEW_APPROVED", "iteration 06a-perf", "commit the reviewed code")
	push = stepIndex(t, steps, "Push `iter-06-task-control`", "pull request #6")
	green = stepIndex(t, steps, "all eighteen jobs", "fourteen stress workers", "all four required checks", "current PR merge revision")
	verify = stepIndex(t, steps, "owner verifies", "read-only", "same four required contexts", "no CPU1 worker")
	merge = stepIndex(t, steps, "Merge iterations 06a and 06a-perf together", "all four checks green")
	if !(review < push && push < green && green < verify && verify < merge) {
		t.Fatalf("pull request #6 handoff order review=%d push=%d green=%d verify=%d merge=%d", review, push, green, verify, merge)
	}
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "conclusions of all eighteen jobs", "fourteen worker logs (the summaries hold none)",
		"`devcheck: stage stress-plane-cpu1 ok`", "`devcheck: stage stress-plane ok`", "`devcheck: stage stress-sidecar-cpu1 ok`",
		"`devcheck: stage stress-sidecar ok`", "`devcheck: stage stress-processgroup ok`", "every CPU invocation's outcome",
		"`devcheck: stress plane cpuN: ok in Xs`", "`devcheck: stress sidecar cpuN: ok in Xs`", "separately from its go command's build", "pre-authorised plane fallback trigger",
		"sidecar follow-up rule", "core count and architecture", "summary wait", "critical path",
		"failed runs stay in the evidence, never discarded as retries", "any timeout or assertion failure blocks qualification",
		// The owner's fixed first-run rule of design 06a-perf.
		"for iteration 06a-perf", "all eighteen job conclusions", "`devcheck: stress <package> cpu1: ok in Xs`",
		"all four CPU1 invocations, Linux and macOS, plane and sidecar",
		"If every CPU1 invocation passes and is ≤300.0 seconds, and all ordinary correctness, coverage and CI gates pass, this slice is done",
		"A value above 250 but at or below 300 succeeds under the owner's rule; record that the aspirational target was missed",
		"Otherwise report the measured values and failures to the owner", "Missing, cancelled or timed-out invocations do not count as passes",
		"make no automatic further change to topology, waves, flags, counts, timeouts, workload tests or fixtures",
		"Do not discard a failed first run by retrying until green",
		"The ≤250-second macOS CPU1 goal is a first-run hypothesis, not an additional acceptance gate", "There is no two-run requirement")
	local := docSection(t, "Local verification")
	requireTerms(t, "Local verification", local, "go run ./cmd/devcheck stress-packages", "go run ./cmd/devcheck stress-plane-cpu1",
		"go run ./cmd/devcheck stress-sidecar-cpu1", "go run ./cmd/devcheck stress-processgroup", "go run ./cmd/devcheck stress-functions",
		"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run '^TestStressConcurrencyContract$' ./internal/devcheck",
		"without other stress work running on the same host", "all five concurrent shards and the wave mechanism on the processgroup shard")
	for _, stage := range []string{"stress-plane", "stress-sidecar"} {
		if !strings.Contains(local, "\ngo run ./cmd/devcheck "+stage+"\n") {
			t.Fatalf("Local verification lacks the %s stage line", stage)
		}
	}
	if strings.Contains(strings.Join(strings.Fields(local), " "), "all three concurrent shards and the plane fallback's wave mechanism") {
		t.Fatal("Local verification keeps the obsolete concurrent-shard description")
	}
	pr := docSection(t, "PR flow")
	requireTerms(t, "PR flow", pr, "iterations 02, 02b and 02c join pull request #2",
		"Iteration 05b (the plane stress shard) is delivered with iteration 05 in pull request #5", "its sidecar follow-up", "all fourteen jobs",
		"Iteration 06a-perf (the plane and sidecar CPU1 stress workers) is delivered with iteration 06a in pull request #6", "all eighteen jobs")
	// No workflow step can change repository settings.
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "--method", "POST", "curl", "protection", "contents: write", "secrets.", "continue-on-error", "strategy:"} {
		if strings.Contains(wf, forbidden) {
			t.Fatalf("workflow contains %q", forbidden)
		}
	}
}
