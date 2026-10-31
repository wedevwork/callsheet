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
// per-CPU groups: twenty-two steps, the same shards and jobs) and by the
// stress worker rebalance (the group-only packages-cpu shard: eight shards,
// the same twenty-two steps, twenty jobs, eight-result summaries; that
// iteration's mapping, kept as history, was FP-1 TestStressShardSelection,
// FP-2 TestStressShardSummaries, FP-3 TestStressShardHandoff and FP-4
// TestStressShardExecution). Design 12a-stress-sidecar-margin maps FP-1
// Selection, FP-2 Execution/lifecycle, FP-3 Summaries, FP-4 Handoff and
// FP-5 Execution/dispatch: nine shards, the same twenty-two steps, twenty-two
// jobs and nine-result summaries, with sidecar CPU4 on its own worker. They read tracked artifacts only,
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
// singleton shard of its own, leaving CPU2 and CPU4 concurrent in plane and,
// until design 12a-stress-sidecar-margin, in sidecar; that design leaves
// plane's pair concurrent and gives sidecar CPU2 and CPU4 singleton shards;
// the contract headroom fix moved ./internal/contract out of the
// combined packages command into the packages shard's per-CPU group, run
// after it as three concurrent single-CPU invocations, and the workspace
// and mcpqual headroom fix did the same for ./internal/mcpqual and
// ./internal/workspace: cpuGroups, one group per package, run one after
// another; the stress worker rebalance moved the three groups unchanged
// into the group-only packages-cpu shard, leaving packages with the
// combined command alone; design 12a-mcpqual-stress-budget gives the three
// mcpqual binaries -timeout=8m, design 12a-stress-packages-budget the
// combined packages command's binaries as well, and design
// 12a-stress-sidecar-cpu1-budget the sidecar CPU1 command; sidecar CPU2 and
// CPU4 keep -timeout=6m).
var shardPlan = []struct {
	name      string
	parallel  bool
	steps     [][2]string
	cpuGroups [][][2]string
}{
	{"packages", false, [][2]string{{"stress packages", "go test -race -count=20 -cpu=1,2,4 -timeout=8m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication"}}, nil},
	{"packages-cpu", false, nil,
		[][][2]string{{
			{"stress contract cpu1", "go test -race -count=20 -cpu=1 -timeout=6m ./internal/contract"},
			{"stress contract cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/contract"},
			{"stress contract cpu4", "go test -race -count=20 -cpu=4 -timeout=6m ./internal/contract"},
		}, {
			{"stress mcpqual cpu1", "go test -race -count=20 -cpu=1 -timeout=8m ./internal/mcpqual"},
			{"stress mcpqual cpu2", "go test -race -count=20 -cpu=2 -timeout=8m ./internal/mcpqual"},
			{"stress mcpqual cpu4", "go test -race -count=20 -cpu=4 -timeout=8m ./internal/mcpqual"},
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
		{"stress sidecar cpu1", "go test -race -count=20 -cpu=1 -timeout=8m ./internal/sidecar"},
	}, nil},
	{"sidecar", true, [][2]string{
		{"stress sidecar cpu2", "go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar"},
	}, nil},
	{"sidecar-cpu4", true, [][2]string{
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

// shardFlat22 is the literal flattened stress selection in execution
// order, written independently of shardPlan: twenty-two commands,
// unchanged by the stress worker rebalance, which moved only command
// ownership (one to packages, nine to packages-cpu); the sidecar CPU1
// command (index 13) carries -timeout=8m since design
// 12a-stress-sidecar-cpu1-budget.
var shardFlat22 = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=8m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication",
	"go test -race -count=20 -cpu=1 -timeout=6m ./internal/contract",
	"go test -race -count=20 -cpu=2 -timeout=6m ./internal/contract",
	"go test -race -count=20 -cpu=4 -timeout=6m ./internal/contract",
	"go test -race -count=20 -cpu=1 -timeout=8m ./internal/mcpqual",
	"go test -race -count=20 -cpu=2 -timeout=8m ./internal/mcpqual",
	"go test -race -count=20 -cpu=4 -timeout=8m ./internal/mcpqual",
	"go test -race -count=20 -cpu=1 -timeout=6m ./internal/workspace",
	"go test -race -count=20 -cpu=2 -timeout=6m ./internal/workspace",
	"go test -race -count=20 -cpu=4 -timeout=6m ./internal/workspace",
	"go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane",
	"go test -race -count=20 -cpu=2 -timeout=6m ./internal/plane",
	"go test -race -count=20 -cpu=4 -timeout=6m ./internal/plane",
	"go test -race -count=20 -cpu=1 -timeout=8m ./internal/sidecar",
	"go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar",
	"go test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar",
	"go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup",
	"go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup",
	"go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function",
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
// tuples, requiring -race and exactly one binary timeout flag,
// -timeout=<timeout> (any other -timeout value is an unexpected argument).
// The historical selections pass "6m"; current-plan callers choose with
// shardTimeoutOf.
func tuples(t *testing.T, argv, timeout string) []tuple {
	t.Helper()
	f := strings.Fields(argv)
	flag := "-timeout=" + timeout
	if len(f) < 7 || strings.Join(f[:3], " ") != "go test -race" || !slices.Contains(f, flag) {
		t.Fatalf("not a race go test with %s: %s", flag, argv)
	}
	var count, sel string
	var cpus, pkgs []string
	timeouts := 0
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
		case a == flag:
			if timeouts++; timeouts > 1 {
				t.Fatalf("more than one %s in %s", flag, argv)
			}
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

// shardTimeoutOf is the binary timeout a current stress command must carry
// (design 12a-mcpqual-stress-budget, extended by designs
// 12a-stress-packages-budget and 12a-stress-sidecar-cpu1-budget), written
// independently of devcheck: "8m" exactly when the command's only package
// is ./internal/mcpqual, its packages are exactly the combined packages
// command's nine, in order, or its only package is ./internal/sidecar and
// it has the exact token -cpu=1 (not -cpu=1,2,4, not a substring); "6m"
// for every other command, ./internal/workspacetransfer alone and sidecar
// CPU2 and CPU4 included.
func shardTimeoutOf(argv string) string {
	var pkgs []string
	fields := strings.Fields(argv)
	for _, a := range fields {
		if strings.HasPrefix(a, "./") {
			pkgs = append(pkgs, a)
		}
	}
	if slices.Equal(pkgs, []string{"./internal/mcpqual"}) || slices.Equal(pkgs, []string{"./internal/testkit", "./internal/testkit/fakeadapter", "./internal/spikes/gittransport",
		"./internal/client", "./internal/adapter", "./internal/mcp", "./internal/workspacetransfer", "./internal/taskworkspace", "./internal/taskpublication"}) ||
		(slices.Equal(pkgs, []string{"./internal/sidecar"}) && slices.Contains(fields, "-cpu=1")) {
		return "8m"
	}
	return "6m"
}

// FP-1: both OS plans are exactly the eight literal shards (the stress
// worker rebalance: packages owns the one combined command, packages-cpu
// the nine per-CPU invocations as three groups; twenty-two steps in all,
// in the unchanged flattened order), and their normalized union is the
// 02b multiset plus the literal additions of iterations 03, 04, 07a, 07b,
// 09a, 09b and 10b (54 tuples, each exactly once), with nothing duplicated
// or lost; the complete plane and sidecar packages run exactly once per
// CPU setting: CPU1 only in plane-cpu1 and sidecar-cpu1, CPU2 and CPU4
// only in plane and sidecar (design 06a-perf).
func TestStressShardSelection(t *testing.T) {
	want := map[tuple]int{}
	for _, argv := range shard02b {
		for _, tp := range tuples(t, argv, "6m") {
			want[tp]++
		}
	}
	if len(want) != 21 {
		t.Fatalf("02b baseline = %d tuples, want 5 packages and 2 selectors at 3 CPU settings", len(want))
	}
	for _, argv := range shard03 {
		for _, tp := range tuples(t, argv, "6m") {
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
		for _, tp := range tuples(t, argv, "6m") {
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
		for _, tp := range tuples(t, argv, "6m") {
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
		for _, tp := range tuples(t, argv, "6m") {
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
		for _, tp := range tuples(t, argv, "6m") {
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
		for _, tp := range tuples(t, argv, "6m") {
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
		for _, tp := range tuples(t, argv, "6m") {
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
		timeouts := map[string][]string{}
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
				timeouts[shardTimeoutOf(argv)] = append(timeouts[shardTimeoutOf(argv)], s.Name)
				for _, tp := range tuples(t, argv, shardTimeoutOf(argv)) {
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
		// Design 12a-mcpqual-stress-budget, extended by designs
		// 12a-stress-packages-budget and 12a-stress-sidecar-cpu1-budget:
		// exactly five commands, the combined packages command, the three
		// mcpqual commands and the sidecar CPU1 command, carry -timeout=8m;
		// the other seventeen, by name, keep -timeout=6m (tuples checked
		// each command's one flag). Commands are counted, not binaries.
		if len(timeouts) != 2 || !slices.Equal(timeouts["8m"], []string{"stress packages", "stress mcpqual cpu1", "stress mcpqual cpu2", "stress mcpqual cpu4", "stress sidecar cpu1"}) ||
			!slices.Equal(timeouts["6m"], []string{"stress contract cpu1", "stress contract cpu2", "stress contract cpu4", "stress workspace cpu1", "stress workspace cpu2", "stress workspace cpu4",
				"stress plane cpu1", "stress plane cpu2", "stress plane cpu4", "stress sidecar cpu2", "stress sidecar cpu4",
				"stress processgroup cpu1", "stress processgroup cpu2", "stress processgroup cpu4", "stress function", "stress plane function", "stress node function"}) {
			t.Fatalf("%s: commands per timeout %v, want the packages, three mcpqual and sidecar CPU1 commands at 8m and the other seventeen at 6m", goos, timeouts)
		}
		// Design 12a-stress-sidecar-cpu1-budget (FP-1): the sidecar-cpu1
		// shard is exactly this one literal command on both systems, and
		// the other twenty-one commands are unchanged.
		if sc := shards[4]; sc.Name != "sidecar-cpu1" || !sc.Parallel || len(sc.Steps) != 1 || sc.CPUGroups != nil || sc.Steps[0].Name != "stress sidecar cpu1" ||
			strings.Join(sc.Steps[0].Argv, " ") != "go test -race -count=20 -cpu=1 -timeout=8m ./internal/sidecar" {
			t.Fatalf("%s: sidecar-cpu1 shard = %+v", goos, sc)
		}
		if len(flat) != 22 || !slices.Equal(slices.Concat(flat[:13], flat[14:]), slices.Concat(shardFlat22[:13], shardFlat22[14:])) ||
			flat[13] != "go test -race -count=20 -cpu=1 -timeout=8m ./internal/sidecar" ||
			flat[14] != "go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar" || flat[15] != "go test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar" {
			t.Fatalf("%s: the sidecar CPU1 command or the other twenty-one commands changed: %q", goos, flat)
		}
		// Design 12a-stress-packages-budget (FP-1): the combined packages
		// command is exactly this literal argv, all nine binaries at
		// -timeout=8m, and the other twenty-one commands are unchanged.
		if got := strings.Join(shards[0].Steps[0].Argv, " "); got != "go test -race -count=20 -cpu=1,2,4 -timeout=8m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication" {
			t.Fatalf("%s: stress packages argv = %s", goos, got)
		}
		if len(flat) != 22 || !slices.Equal(flat[1:], shardFlat22[1:]) || strings.Contains(strings.Join(flat[1:], "|"), "./internal/workspacetransfer") {
			t.Fatalf("%s: the other twenty-one commands changed: %q", goos, flat)
		}
		ownerOf := map[string]string{"1": "-cpu1", "2": "", "4": ""}
		for _, pkg := range []string{"plane", "sidecar"} {
			for _, cpu := range []string{"1", "2", "4"} {
				wantShard := pkg + ownerOf[cpu]
				if pkg == "sidecar" && cpu == "4" {
					wantShard = "sidecar-cpu4"
				}
				if tp := (tuple{"./internal/" + pkg, "", cpu, "20"}); got[tp] != 1 || owner[tp] != wantShard {
					t.Fatalf("%s: %s at -cpu=%s selected %d times, in %q, want once in %s", goos, pkg, cpu, got[tp], owner[tp], wantShard)
				}
			}
		}
		held := map[string][]string{}
		for tp, sh := range owner {
			if tp.pkg == "./internal/plane" || tp.pkg == "./internal/sidecar" {
				held[sh] = append(held[sh], strings.TrimPrefix(tp.pkg, "./internal/")+"@"+tp.cpu)
			}
		}
		for sh, want := range map[string][]string{"plane-cpu1": {"plane@1"}, "plane": {"plane@2", "plane@4"}, "sidecar-cpu1": {"sidecar@1"}, "sidecar": {"sidecar@2"}, "sidecar-cpu4": {"sidecar@4"}} {
			slices.Sort(held[sh])
			if !slices.Equal(held[sh], want) {
				t.Fatalf("%s: shard %s holds %v, want exactly %v", goos, sh, held[sh], want)
			}
		}
		if len(held) != 5 {
			t.Fatalf("%s: plane or sidecar tuples in shards %v", goos, held)
		}
		// The headroom fixes: ./internal/contract, ./internal/mcpqual and
		// ./internal/workspace each run once per CPU setting, only in their
		// own per-CPU group of the packages-cpu shard (the stress worker
		// rebalance), in that order.
		for g, pkg := range []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace"} {
			for j, cpu := range []string{"1", "2", "4"} {
				tp := tuple{pkg, "", cpu, "20"}
				argv := strings.Join(shards[1].CPUGroups[g][j].Argv, " ")
				if got[tp] != 1 || owner[tp] != "packages-cpu" || tuples(t, argv, shardTimeoutOf(argv))[0] != tp {
					t.Fatalf("%s: %s at -cpu=%s selected %d times in %q, want once in packages-cpu per-CPU group %d", goos, pkg, cpu, got[tp], owner[tp], g+1)
				}
			}
		}
		// Command ownership (the stress worker rebalance): packages runs
		// exactly the one combined command, its nine packages at
		// -cpu=1,2,4, and no group; packages-cpu runs exactly nine
		// single-CPU commands, as three groups and no step; no other shard
		// owns a group or a split package.
		packages, packagesCPU := shards[0], shards[1]
		if packages.Name != "packages" || packages.Parallel || len(packages.Steps) != 1 || packages.CPUGroups != nil ||
			len(tuples(t, strings.Join(packages.Steps[0].Argv, " "), "8m")) != 9*3 {
			t.Fatalf("%s packages shard = %+v, want the combined command alone", goos, packages)
		}
		nine := 0
		for _, g := range packagesCPU.CPUGroups {
			nine += len(g)
		}
		if packagesCPU.Name != "packages-cpu" || packagesCPU.Parallel || packagesCPU.Steps != nil || len(packagesCPU.CPUGroups) != 3 || nine != 9 {
			t.Fatalf("%s packages-cpu shard = %+v, want three groups of three and no step", goos, packagesCPU)
		}
		for _, sh := range shards[2:] {
			if sh.CPUGroups != nil {
				t.Fatalf("%s: shard %s owns a per-CPU group", goos, sh.Name)
			}
		}
		for tp, sh := range owner {
			split := tp.pkg == "./internal/contract" || tp.pkg == "./internal/mcpqual" || tp.pkg == "./internal/workspace"
			if split != (sh == "packages-cpu") {
				t.Fatalf("%s: %+v in shard %s", goos, tp, sh)
			}
		}
		// Every command keeps -race, -count=20 and its binary limit: eight
		// minutes for the mcpqual commands (design 12a-mcpqual-stress-budget)
		// and the combined packages command (design
		// 12a-stress-packages-budget) and the sidecar CPU1 command (design
		// 12a-stress-sidecar-cpu1-budget), six for every other
		// (CGO_ENABLED=1 is checked per step above).
		for _, a := range flat {
			if !strings.HasPrefix(a, "go test -race -count=20 -cpu=") || !strings.Contains(a, " -timeout="+shardTimeoutOf(a)+" ") || strings.Count(a, "-timeout") != 1 {
				t.Fatalf("%s: flags changed: %s", goos, a)
			}
		}
		steps, err := devcheck.StressSteps(goos)
		var flatSteps []string
		for _, s := range steps {
			flatSteps = append(flatSteps, strings.Join(s.Argv, " "))
		}
		// The flattened order is unchanged by the rebalance: the combined
		// command first, then contract, mcpqual and workspace at CPU 1, 2
		// and 4, as before the move.
		if err != nil || len(steps) != 22 || !slices.Equal(flatSteps, flat) || !slices.Equal(flatSteps, shardFlat22) {
			t.Fatalf("%s StressSteps is not the twenty-two-step flattened shard view: %q", goos, flatSteps)
		}
	}
	// The functions shard keeps 02b's plane selector, whose executable
	// fixture and exclusions TestCISpeedSelection still runs.
	shards, _ := devcheck.StressShards("linux")
	if len(shards) != 9 || shards[8].Name != "functions" || !slices.Contains(shards[8].Steps[1].Argv, "-run="+speedPlaneSelector) {
		t.Fatalf("plane selector changed: %+v", shards)
	}
	// Fresh copies: a mutated plan, nested slices included, never leaks
	// into the next one.
	shards[8].Steps[1].Argv[6] = "-run=^(TestPlaneState)$"
	shards[2].Steps[0].Argv[4] = "-cpu=2"
	shards[3].Steps = shards[3].Steps[:1]
	shards[1].CPUGroups[1][2].Argv[4] = "-cpu=1"
	shards[1].CPUGroups = shards[1].CPUGroups[:1]
	again, _ := devcheck.StressShards("linux")
	if !slices.Contains(again[8].Steps[1].Argv, "-run="+speedPlaneSelector) || again[2].Steps[0].Argv[4] != "-cpu=1" || len(again[3].Steps) != 2 ||
		len(again[1].CPUGroups) != 3 || again[1].CPUGroups[1][2].Argv[4] != "-cpu=4" {
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

// FP-2 (FP-4 of the stress worker rebalance): devcheck.Run executes the
// aggregate and each shard with the exact commands and boundaries, runs the
// one combined command of stress-packages alone and the three per-CPU
// groups of the group-only stress-packages-cpu one after another, each
// joined before the next, runs the one CPU1 invocation of
// stress-plane-cpu1, stress-sidecar-cpu1, stress-sidecar and
// stress-sidecar-cpu4 alone and the CPU2/CPU4 pair of stress-plane
// concurrently, joined with ordered logs
// (design 06a-perf), never overlaps two shards, gives each shard stage its
// own watchdog and "stress" one shared watchdog, fails with retained logs,
// and the concurrent coordinator's contracts pass by name.
func TestStressShardExecution(t *testing.T) {
	t.Run("dispatch", func(t *testing.T) {
		for stage, want := range map[string][][]string{
			"stress":              shardArgv("packages", "packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "sidecar-cpu4", "processgroup", "functions"),
			"stress-packages":     shardArgv("packages"),
			"stress-packages-cpu": shardArgv("packages-cpu"),
			"stress-plane-cpu1":   shardArgv("plane-cpu1"),
			"stress-plane":        shardArgv("plane"),
			"stress-sidecar-cpu1": shardArgv("sidecar-cpu1"),
			"stress-sidecar":      shardArgv("sidecar"),
			"stress-sidecar-cpu4": shardArgv("sidecar-cpu4"),
			"stress-processgroup": shardArgv("processgroup"),
			"stress-functions":    shardArgv("functions"),
		} {
			r := &syncRunner{}
			var out, errOut bytes.Buffer
			code := devcheck.Run(context.Background(), []string{stage}, &out, &errOut, r.run, evidenceOpts(t))
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
		stages := devcheck.Stages()
		cpu4 := 0
		at := -1
		for i, s := range stages {
			if s == "stress-sidecar-cpu4" {
				cpu4++
				at = i
			}
		}
		if cpu4 != 1 || at < 1 || stages[at-1] != "stress-sidecar" || stages[at+1] != "stress-processgroup" {
			t.Fatalf("advertised stages = %v", stages)
		}
		for _, args := range [][]string{{"stress-sidecar-cpu4", "extra"}, {"stress-sidecar-cpu4", "-cpu=1"}, {"stress-sidecar-cpu4", "-count=20"}, {"stress-sidecar", "-cpu=4"}} {
			r := &syncRunner{}
			var out, errOut bytes.Buffer
			if code := devcheck.Run(context.Background(), args, &out, &errOut, r.run, evidenceOpts(t)); code != 2 || len(r.calls) != 0 {
				t.Fatalf("%v = %d after %d calls: %s", args, code, len(r.calls), errOut.String())
			}
		}
		for _, goos := range []string{"windows", "freebsd", ""} {
			if _, err := devcheck.StressShards(goos); err == nil {
				t.Fatalf("StressShards(%q) planned a launch", goos)
			}
		}
		const pkg = "./internal/devcheck"
		contractRun(t, contractBinary(t, pkg), pkg, "^TestUnsupportedStressCreatesNoScratch$", os.Environ(), "TestUnsupportedStressCreatesNoScratch")
	})
	t.Run("lifecycle", func(t *testing.T) {
		// A failed invocation fails its shard after every invocation of that
		// shard ran (a failed plane CPU2 still joins CPU4), stops the aggregate
		// before the next shard and keeps its log: a plane CPU1 failure stops
		// full stress before the plane pair, a sidecar CPU2 failure before
		// sidecar CPU4, and a sidecar CPU4 failure before processgroup. Every
		// aggregate prefix includes packages-cpu right after packages (the
		// stress worker rebalance).
		for _, c := range []struct {
			stage, failOn, failed string
			want                  [][]string
		}{
			{"stress", "-cpu=1 -timeout=6m ./internal/plane", "stress plane cpu1", shardArgv("packages", "packages-cpu", "plane-cpu1")},
			{"stress-plane-cpu1", "-cpu=1 ", "stress plane cpu1", shardArgv("plane-cpu1")},
			{"stress", "-cpu=2 -timeout=6m ./internal/plane", "stress plane cpu2", shardArgv("packages", "packages-cpu", "plane-cpu1", "plane")},
			{"stress-plane", "-cpu=4 ", "stress plane cpu4", shardArgv("plane")},
			{"stress", "-cpu=1 -timeout=8m ./internal/sidecar", "stress sidecar cpu1", shardArgv("packages", "packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1")},
			{"stress-sidecar-cpu1", "-cpu=1 ", "stress sidecar cpu1", shardArgv("sidecar-cpu1")},
			{"stress", "-cpu=2 -timeout=6m ./internal/sidecar", "stress sidecar cpu2", shardArgv("packages", "packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar")},
			{"stress-sidecar", "-cpu=2 ", "stress sidecar cpu2", shardArgv("sidecar")},
			{"stress", "-cpu=4 -timeout=6m ./internal/sidecar", "stress sidecar cpu4", shardArgv("packages", "packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "sidecar-cpu4")},
			{"stress-sidecar-cpu4", "-cpu=4 ", "stress sidecar cpu4", shardArgv("sidecar-cpu4")},
			{"stress", "-cpu=2 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu2",
				shardArgv("packages", "packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "sidecar-cpu4", "processgroup")},
			{"stress-processgroup", "-cpu=4 ", "stress processgroup cpu4", shardArgv("processgroup")},
			{"stress-functions", "TestFP4TransportHarness", "stress function", shardArgv("functions")[:1]},
			{"stress", "TestNodeEnrollment", "stress node function",
				shardArgv("packages", "packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "sidecar-cpu4", "processgroup", "functions")},
			// The stress worker rebalance's routing table: a failed packages
			// shard never starts packages-cpu; a failed group member fails
			// packages-cpu after its group joined, before the next group or
			// shard; standalone, packages-cpu's first group starts at once.
			{"stress", "./internal/client", "stress packages", shardArgv("packages")},
			{"stress-packages", "./internal/client", "stress packages", shardArgv("packages")},
			{"stress", "-cpu=2 -timeout=6m ./internal/contract", "stress contract cpu2", shardArgv("packages", "packages-cpu")[:2]},
			{"stress-packages-cpu", "-cpu=4 -timeout=6m ./internal/contract", "stress contract cpu4", shardArgv("packages-cpu")[:1]},
			{"stress", "-cpu=1 -timeout=8m ./internal/mcpqual", "stress mcpqual cpu1", shardArgv("packages", "packages-cpu")[:3]},
			{"stress-packages-cpu", "-cpu=2 -timeout=8m ./internal/mcpqual", "stress mcpqual cpu2", shardArgv("packages-cpu")[:2]},
			{"stress-packages-cpu", "-cpu=2 -timeout=6m ./internal/workspace", "stress workspace cpu2", shardArgv("packages-cpu")},
			{"stress", "-cpu=4 -timeout=6m ./internal/workspace", "stress workspace cpu4", shardArgv("packages", "packages-cpu")},
		} {
			r := &syncRunner{failOn: c.failOn}
			var out, errOut bytes.Buffer
			code := devcheck.Run(context.Background(), []string{c.stage}, &out, &errOut, r.run, evidenceOpts(t))
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
		// are the packages shard 0-1, then the packages-cpu shard (the stress
		// worker rebalance): the contract group's three starts 2-4 and ends 5-7,
		// mcpqual's 8-10 and 11-13 and workspace's 14-16 and 17-19 (the
		// headroom fixes); packages ends before any packages-cpu start, and
		// every packages-cpu group ends before plane-cpu1 starts; then the
		// plane CPU1 singleton's start and end 20-21, the
		// plane pair's two starts 22-23 before either end 24-25, the sidecar
		// CPU1 singleton 26-27, the sidecar CPU2 singleton 28-29 and the
		// sidecar CPU4 singleton 30-31,
		// processgroup's three starts 32-34 and ends 35-37, and the sequential
		// functions 38-43; no two Parallel shards overlap, and the logs replay
		// under their CPU names in CPU order.
		gates := map[string]*gate{}
		for _, g := range []struct {
			pkg  string
			cpus []string
		}{{"./internal/contract", []string{"1", "2", "4"}}, {"./internal/mcpqual", []string{"1", "2", "4"}}, {"./internal/workspace", []string{"1", "2", "4"}}, {"./internal/plane", []string{"1"}}, {"./internal/plane", []string{"2", "4"}}, {"./internal/sidecar", []string{"1"}},
			{"./internal/sidecar", []string{"2"}}, {"./internal/sidecar", []string{"4"}}, {"./internal/spikes/processgroup", []string{"1", "2", "4"}}} {
			gt := newGate(len(g.cpus))
			for _, c := range g.cpus {
				gates["-cpu="+c+" "+g.pkg] = gt
			}
		}
		o := &overlapRunner{groups: gates}
		var out, errOut bytes.Buffer
		if code := devcheck.Run(context.Background(), []string{"stress"}, &out, &errOut, o.run, evidenceOpts(t)); code != 0 {
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
		// The packages / packages-cpu / plane-cpu1 boundaries, explicitly.
		combined := shardArgv("packages")[0][0]
		if events[0] != "start "+combined || events[1] != "end "+combined {
			t.Fatalf("packages shard is not events 0-1: %q", events)
		}
		firstCPU, lastCPU := len(events), -1
		for _, pkg := range []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace"} {
			firstCPU, lastCPU = min(firstCPU, spans[pkg][0]), max(lastCPU, spans[pkg][len(spans[pkg])-1])
		}
		if firstCPU <= 1 || lastCPU >= spans["./internal/plane"][0] {
			t.Fatalf("packages-cpu spans %d-%d overlap packages (0-1) or plane-cpu1 (%d): %q", firstCPU, lastCPU, spans["./internal/plane"][0], events)
		}
		for _, g := range []struct {
			from, n int
			cpus    []string
			pkg     string
		}{{2, 3, []string{"1", "2", "4"}, "./internal/contract"}, {8, 3, []string{"1", "2", "4"}, "./internal/mcpqual"}, {14, 3, []string{"1", "2", "4"}, "./internal/workspace"},
			{20, 1, []string{"1"}, "./internal/plane"}, {22, 2, []string{"2", "4"}, "./internal/plane"},
			{26, 1, []string{"1"}, "./internal/sidecar"}, {28, 1, []string{"2"}, "./internal/sidecar"}, {30, 1, []string{"4"}, "./internal/sidecar"}, {32, 3, []string{"1", "2", "4"}, "./internal/spikes/processgroup"}} {
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
				// Design 12a-mcpqual-stress-budget: of these per-CPU
				// binaries, the mcpqual ones run with -timeout=8m, and
				// since design 12a-stress-sidecar-cpu1-budget the sidecar
				// CPU1 one too; the others, sidecar CPU2 and CPU4
				// included, keep -timeout=6m.
				timeout := "6m"
				if shard == "mcpqual" || (shard == "sidecar" && cpu == "1") {
					timeout = "8m"
				}
				name := "stress " + shard + " cpu" + cpu
				i := strings.Index(out.String(), "devcheck: ---- "+name+" log ("+filepath.Join(scratch, "stress-"+shard+"-cpu"+cpu+".log")+") ----\nok go test -race -count=20 -cpu="+cpu+" -timeout="+timeout+" ./internal/"+shard+"\n")
				if i < 0 || i < pos || !strings.Contains(out.String()[i:], "devcheck: "+name+": ok in ") {
					t.Fatalf("%s log not replayed in shard and CPU order:\n%s", name, out.String())
				}
				pos = i
			}
		}
		// The group-only packages-cpu stage (the stress worker rebalance): 18
		// events, three groups one after another, each group's three starts
		// before any of its ends (at most three active), and each group joined
		// before the next starts. The darwin dispatch path of every stage runs
		// by name below (TestPlatformSeamContract/darwin).
		if devcheck.Stages()[7] != "stress-packages" || devcheck.Stages()[8] != "stress-packages-cpu" {
			t.Fatalf("stages = %v", devcheck.Stages())
		}
		cpuGates := map[string]*gate{}
		for _, pkg := range []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace"} {
			gt := newGate(3)
			for _, c := range []string{"1", "2", "4"} {
				cpuGates["-cpu="+c+" "+pkg] = gt
			}
		}
		oc := &overlapRunner{groups: cpuGates}
		var cout, cerr bytes.Buffer
		if code := devcheck.Run(context.Background(), []string{"stress-packages-cpu"}, &cout, &cerr, oc.run, evidenceOpts(t)); code != 0 {
			t.Fatalf("stress-packages-cpu with gates = %d: %s", code, cerr.String())
		}
		oc.mu.Lock()
		ev := slices.Clone(oc.events)
		oc.mu.Unlock()
		if len(ev) != 18 {
			t.Fatalf("stress-packages-cpu events = %q", ev)
		}
		active, peak := 0, 0
		for g, pkg := range []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace"} {
			for i := 6 * g; i < 6*g+6; i++ {
				if !strings.HasSuffix(ev[i], " "+pkg) || strings.HasPrefix(ev[i], "start ") != (i < 6*g+3) {
					t.Fatalf("event %d %q is outside group %s or ends before all three started: %q", i, ev[i], pkg, ev)
				}
				if strings.HasPrefix(ev[i], "start ") {
					active++
				} else {
					active--
				}
				peak = max(peak, active)
			}
		}
		if peak != 3 || active != 0 {
			t.Fatalf("peak %d active invocations, %d left", peak, active)
		}
		os.RemoveAll(scratchOf(cout.String()))
		// Watchdogs: each shard stage has its own (stress-packages and
		// stress-packages-cpu each get a deadline stressWatchdog after their own
		// start), and "stress" shares one across all nine shards with no reset
		// at the packages/packages-cpu boundary: every child of the aggregate
		// sees the same deadline.
		for _, stage := range []string{"stress-packages", "stress-packages-cpu", "stress"} {
			d := &deadlineRunner{}
			before := time.Now()
			var out, errOut bytes.Buffer
			if code := devcheck.Run(context.Background(), []string{stage}, &out, &errOut, d.run, evidenceOpts(t)); code != 0 {
				t.Fatalf("%s = %d: %s", stage, code, errOut.String())
			}
			after := time.Now()
			want := map[string]int{"stress-packages": 1, "stress-packages-cpu": 9, "stress": 22}[stage]
			if len(d.deadlines) != want {
				t.Fatalf("%s: %d children, want %d", stage, len(d.deadlines), want)
			}
			for i, dl := range d.deadlines {
				if dl.IsZero() || dl.Before(before.Add(15*time.Minute)) || dl.After(after.Add(15*time.Minute)) || !dl.Equal(d.deadlines[0]) {
					t.Fatalf("%s child %d deadline %v, want one shared deadline 15m after the stage started (%v..%v)", stage, i, dl, before, after)
				}
			}
		}
		// The coordinator's contracts, by name, with every named subtest: the
		// overlap, failure, watchdog and log contracts of the packages-cpu
		// groups, the CPU1 singletons, the CPU2/CPU4 pairs and processgroup, the
		// wave mechanism on the full three-CPU processgroup shard, and the
		// injected-runner watchdog and cancellation contracts (the group-only
		// packages-cpu shard's cancellation and expiry included), and both OS
		// dispatch paths of every stress stage (TestPlatformSeamContract's linux
		// and darwin subtests: stress-packages one invocation, stress-packages-cpu
		// nine).
		const pkg = "./internal/devcheck"
		bin := contractBinary(t, pkg)
		contractRun(t, bin, pkg, "^(TestStressConcurrencyContract|TestStressWatchdog|TestStressCancellationAndExpiry|TestPlatformSeamContract)$", os.Environ(),
			"TestStressConcurrencyContract", "TestStressConcurrencyContract/overlap", "TestStressConcurrencyContract/failure",
			"TestStressConcurrencyContract/watchdog", "TestStressConcurrencyContract/logs", "TestStressConcurrencyContract/waves",
			"TestStressWatchdog", "TestStressCancellationAndExpiry", "TestPlatformSeamContract/linux", "TestPlatformSeamContract/darwin")
	})
}

// deadlineRunner records each child's context deadline (zero if none).
type deadlineRunner struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *deadlineRunner) run(ctx context.Context, _, _ []string, _ string, _, _ io.Writer) error {
	dl, _ := ctx.Deadline()
	r.mu.Lock()
	r.deadlines = append(r.deadlines, dl)
	r.mu.Unlock()
	return nil
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

// shardJobs is the literal twenty-two-job table (design 02c, Workflow
// topology, with design 05b's plane workers, its sidecar follow-up's
// sidecar workers, design 06a-perf's plane and sidecar CPU1 workers and the
// stress worker rebalance's packages-cpu workers in CI-plan order).
var shardJobs = []struct {
	id, name, runner, timeout string
}{
	{"linux", "ci-linux", "ubuntu-24.04", "45"},
	{"macos", "ci-macos", "macos-15", "30"},
	{"linux-stress-packages", "ci-linux-stress-packages", "ubuntu-24.04", "20"},
	{"linux-stress-packages-cpu", "ci-linux-stress-packages-cpu", "ubuntu-24.04", "20"},
	{"linux-stress-plane-cpu1", "ci-linux-stress-plane-cpu1", "ubuntu-24.04", "20"},
	{"linux-stress-plane", "ci-linux-stress-plane", "ubuntu-24.04", "20"},
	{"linux-stress-sidecar-cpu1", "ci-linux-stress-sidecar-cpu1", "ubuntu-24.04", "20"},
	{"linux-stress-sidecar", "ci-linux-stress-sidecar", "ubuntu-24.04", "20"},
	{"linux-stress-sidecar-cpu4", "ci-linux-stress-sidecar-cpu4", "ubuntu-24.04", "20"},
	{"linux-stress-processgroup", "ci-linux-stress-processgroup", "ubuntu-24.04", "20"},
	{"linux-stress-functions", "ci-linux-stress-functions", "ubuntu-24.04", "20"},
	{"macos-stress-packages", "ci-macos-stress-packages", "macos-15", "20"},
	{"macos-stress-packages-cpu", "ci-macos-stress-packages-cpu", "macos-15", "20"},
	{"macos-stress-plane-cpu1", "ci-macos-stress-plane-cpu1", "macos-15", "20"},
	{"macos-stress-plane", "ci-macos-stress-plane", "macos-15", "20"},
	{"macos-stress-sidecar-cpu1", "ci-macos-stress-sidecar-cpu1", "macos-15", "20"},
	{"macos-stress-sidecar", "ci-macos-stress-sidecar", "macos-15", "20"},
	{"macos-stress-sidecar-cpu4", "ci-macos-stress-sidecar-cpu4", "macos-15", "20"},
	{"macos-stress-processgroup", "ci-macos-stress-processgroup", "macos-15", "20"},
	{"macos-stress-functions", "ci-macos-stress-functions", "macos-15", "20"},
	{"linux-stress", "ci-linux-stress", "ubuntu-24.04", "5"},
	{"macos-stress", "ci-macos-stress", "ubuntu-24.04", "5"},
}

// summaryCommand is the literal nine-result predicate (the stress worker
// rebalance's eight results plus SIDECAR_CPU4_RESULT).
const summaryCommand = `test "$PACKAGES_RESULT" = success && test "$PACKAGES_CPU_RESULT" = success && test "$PLANE_CPU1_RESULT" = success && test "$PLANE_RESULT" = success && test "$SIDECAR_CPU1_RESULT" = success && test "$SIDECAR_RESULT" = success && test "$SIDECAR_CPU4_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`

// summaryKeys are the summary step's environment keys in needs order.
var summaryKeys = []string{"PACKAGES_RESULT", "PACKAGES_CPU_RESULT", "PLANE_CPU1_RESULT", "PLANE_RESULT", "SIDECAR_CPU1_RESULT", "SIDECAR_RESULT", "SIDECAR_CPU4_RESULT", "PROCESSGROUP_RESULT", "FUNCTIONS_RESULT"}

// summaryWorkers are the shard suffixes of a summary's needs, in order.
var summaryWorkers = []string{"packages", "packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "sidecar-cpu4", "processgroup", "functions"}

// summaryDeadline bounds the whole summary test, structural checks and
// every shell execution included. It is a hang guard in the sense of
// gate.wait, never a timing oracle: 566 short bash children finish in a
// few seconds, and only a stuck child reaches it.
const summaryDeadline = 20 * time.Second

// FP-3 (FP-2 of the stress worker rebalance): the actual workflow has the
// twenty-two jobs and two literal summaries, each binding its own platform's
// nine workers in order, the four required contexts are unchanged, and
// the shared nine-result command passes only when all nine workers
// concluded success: a complete proof in 566 bash executions (2^9
// success/failure assignments and six other spellings in each of the
// nine positions; design 06a-perf, D1, extended by design
// 12a-stress-sidecar-margin).
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
	if !slices.Equal(ids, wantIDs) || len(ids) != 22 {
		t.Fatalf("job ids = %v, want %v", ids, wantIDs)
	}
	if got := strings.Join(cicheck.RequiredChecks(), ","); got != "ci-linux,ci-macos,ci-linux-stress,ci-macos-stress" {
		t.Fatalf("required contexts = %s", got)
	}
	// The new workers copy their platform's packages worker exactly except
	// for ID, name, check-step name and command.
	for _, plat := range []string{"linux", "macos"} {
		a, b := node(t, jobs, plat+"-stress-packages"), node(t, jobs, plat+"-stress-packages-cpu")
		ca, err := yaml.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		cb, err := yaml.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.NewReplacer("ci-"+plat+"-stress-packages\n", "ci-"+plat+"-stress-packages-cpu\n",
			"devcheck stress-packages\n", "devcheck stress-packages-cpu\n").Replace(string(ca))
		if string(cb) != want {
			t.Fatalf("%s-stress-packages-cpu is not a copy of %s-stress-packages:\n%s\nwant:\n%s", plat, plat, cb, want)
		}
		if node(t, b, "steps", 3, "run").Value != "go run ./cmd/devcheck stress-packages-cpu" || node(t, b, "steps", 3, "name").Value != "devcheck stress-packages-cpu" {
			t.Fatalf("%s-stress-packages-cpu check step = %q", plat, node(t, b, "steps", 3, "run").Value)
		}
	}
	// The two main jobs and the eighteen workers are independent: no
	// dependency, condition, matrix or error bypass. The slice covers
	// both main jobs and all eighteen workers and excludes both summaries.
	for _, j := range shardJobs[:20] {
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
		// Nine ordered key/value pairs: eighteen YAML nodes, each result
		// bound to its own platform's worker in needs order.
		env := node(t, steps, 0, "env")
		if len(env.Content) != 18 {
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
	// the nine results bound to the keys, under the test's one deadline.
	// On expiry CommandContext kills the shell and Run waits for it before
	// the failure is reported; no case is skipped or retried.
	executions := 0
	execute := func(tp [9]string) bool {
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
	allSuccess := [9]string{"success", "success", "success", "success", "success", "success", "success", "success", "success"}
	// Step 1: all 2^9 boolean-class assignments, each position success or
	// failure; exactly the all-success one exits zero, so every single and
	// multiple failure pattern fails.
	passed := 0
	for mask := 0; mask < 1<<9; mask++ {
		var tp [9]string
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
	// Step 2: every other result spelling, 9 × 6, in each position with
	// all others success: the remaining standard conclusions and the
	// unusual values each fail.
	odd := []string{"cancelled", "skipped", "", "neutral", "Success", "success "}
	for pos := 0; pos < 9; pos++ {
		for _, v := range odd {
			tp := allSuccess
			tp[pos] = v
			if execute(tp) {
				t.Fatalf("%s=%q with every other result success passed", summaryKeys[pos], v)
			}
		}
	}
	if executions != 512+9*6 || passed != 1 {
		t.Fatalf("%d executions, %d passed", executions, passed)
	}
	t.Logf("summary proof: %d bash executions, %d passed, test elapsed %v (hang guard %v)", executions, passed, time.Since(start).Round(time.Millisecond), summaryDeadline)
}

// FP-4: the validator accepts the actual workflow and rejects the previous
// eighteen-job topology (design 06a-perf, before the stress worker
// rebalance's packages-cpu workers), a removed or miswired worker (the CPU1
// workers of design 06a-perf and the packages-cpu workers included), any
// removed worker or a weakened summary; contract, stages, extracted
// sequences and both OS plans agree, and the four required contexts are
// unchanged.
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
	if len(contract) != 22 || len(stages) != 22 {
		t.Fatalf("contract %d jobs, workflow %d", len(contract), len(stages))
	}
	// Jobs are not child invocations: eighteen workers run one stage each,
	// and those stages dispatch 44 invocations on the two platforms (26
	// before the contract headroom fix split the contract package per CPU
	// inside stress-packages, 32 before the workspace and mcpqual headroom
	// fix split those two likewise; the stress worker rebalance added two
	// jobs and no invocation).
	workerJobs, invocations := 0, 0
	dispatch := devcheck.Stages()
	for _, goos := range []string{"linux", "darwin"} {
		shards, err := devcheck.StressShards(goos)
		if err != nil || len(shards) != 9 {
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
			if code := devcheck.Run(context.Background(), j.Stages[0:1], &out, &errOut, r.run, evidenceOpts(t)); code != 0 ||
				!sameGroups(r.calls, shardArgv(strings.TrimPrefix(j.Stages[0], "stress-"))) {
				t.Fatalf("%s dispatch = %d %q %s", j.ID, code, r.calls, errOut.String())
			}
			invocations += len(r.calls)
			os.RemoveAll(scratchOf(out.String()))
		}
	}
	if workerJobs != 18 || invocations != 44 {
		t.Fatalf("%d worker jobs dispatch %d invocations, want 18 and 44 (twenty-two per platform)", workerJobs, invocations)
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
	// The previous eighteen-job topology (design 06a-perf: seven workers
	// and seven results per platform) is incomplete under the twenty-job
	// contract.
	previous := mutated(t, func(r *yaml.Node) {
		for _, plat := range []string{"linux", "macos"} {
			deleteKey(t, node(t, r, "jobs"), plat+"-stress-packages-cpu")
			deleteKey(t, node(t, r, "jobs"), plat+"-stress-sidecar-cpu4")
			id := plat + "-stress"
			needs := node(t, r, "jobs", id, "needs")
			var kept []*yaml.Node
			for _, n := range needs.Content {
				if n.Value == plat+"-stress-packages-cpu" || n.Value == plat+"-stress-sidecar-cpu4" {
					continue
				}
				kept = append(kept, n)
			}
			needs.Content = kept
			deleteKey(t, node(t, r, "jobs", id, "steps", 0, "env"), "PACKAGES_CPU_RESULT")
			deleteKey(t, node(t, r, "jobs", id, "steps", 0, "env"), "SIDECAR_CPU4_RESULT")
			run := strings.Replace(summaryCommand, ` && test "$PACKAGES_CPU_RESULT" = success`, "", 1)
			node(t, r, "jobs", id, "steps", 0, "run").Value = strings.Replace(run, ` && test "$SIDECAR_CPU4_RESULT" = success`, "", 1)
		}
	})
	if ids, err := cicheck.ExtractStages(previous); err != nil || len(ids) != 18 {
		t.Fatalf("previous topology has %d jobs: %v", len(ids), err)
	}
	for _, want := range []string{"jobs.linux-stress-packages-cpu: missing required field", "jobs.macos-stress-packages-cpu: missing required field",
		"jobs.linux-stress.needs: must be exactly", "jobs.macos-stress.needs: must be exactly",
		"jobs.linux-stress.steps[0].env.PACKAGES_CPU_RESULT: missing required field", "jobs.macos-stress.steps[0].env.PACKAGES_CPU_RESULT: missing required field",
		"jobs.linux-stress.steps[0].run: must be exactly", "jobs.macos-stress.steps[0].run: must be exactly"} {
		mustReject(t, "previous eighteen-job topology", previous, want)
	}
	// Weakening either summary fails, at the positions of the stress
	// worker rebalance's packages-cpu worker (1), design 05b's plane and
	// sidecar workers (now 3 and 5) and design 06a-perf's CPU1 workers (now
	// 2 and 4).
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
		mustReject(t, id+" seven-result summary", setRun(strings.Replace(summaryCommand, ` && test "$PACKAGES_CPU_RESULT" = success`, "", 1)), p+".steps[0].run: must be exactly")
		mustReject(t, id+" altered conjunction", setRun(strings.Replace(summaryCommand, ` && test "$SIDECAR_CPU1_RESULT"`, ` || test "$SIDECAR_CPU1_RESULT"`, 1)), p+".steps[0].run: must be exactly")
		mustReject(t, id+" packages-cpu conjunction altered", setRun(strings.Replace(summaryCommand, ` && test "$PACKAGES_CPU_RESULT"`, ` || test "$PACKAGES_CPU_RESULT"`, 1)), p+".steps[0].run: must be exactly")
		mustReject(t, id+" mismatched order", setRun(strings.Replace(summaryCommand, `test "$PLANE_CPU1_RESULT" = success && test "$PLANE_RESULT" = success`, `test "$PLANE_RESULT" = success && test "$PLANE_CPU1_RESULT" = success`, 1)), p+".steps[0].run: must be exactly")
		mustReject(t, id+" packages-cpu before packages", setRun(strings.Replace(summaryCommand, `test "$PACKAGES_RESULT" = success && test "$PACKAGES_CPU_RESULT" = success`, `test "$PACKAGES_CPU_RESULT" = success && test "$PACKAGES_RESULT" = success`, 1)), p+".steps[0].run: must be exactly")
		for _, c := range []struct {
			shard, key string
			pos        int
		}{{"packages-cpu", "PACKAGES_CPU_RESULT", 1}, {"plane-cpu1", "PLANE_CPU1_RESULT", 2}, {"plane", "PLANE_RESULT", 3}, {"sidecar-cpu1", "SIDECAR_CPU1_RESULT", 4}, {"sidecar", "SIDECAR_RESULT", 5}} {
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
			n.Content[2], n.Content[3] = n.Content[3], n.Content[2]
		}), p+".needs[2]: must be")
		mustReject(t, id+" packages-cpu worker before packages", mutated(t, func(r *yaml.Node) {
			n := node(t, r, "jobs", id, "needs")
			n.Content[0], n.Content[1] = n.Content[1], n.Content[0]
		}), p+".needs[0]: must be")
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
	mustReject(t, "packages-cpu worker needs packages", mutated(t, func(r *yaml.Node) {
		setKey(node(t, r, "jobs", "macos-stress-packages-cpu"), "needs", "macos-stress-packages")
	}), "jobs.macos-stress-packages-cpu.needs: unknown field")
	mustReject(t, "packages worker runs packages-cpu", mutated(t, func(r *yaml.Node) {
		node(t, r, "jobs", "linux-stress-packages", "steps", 3, "run").Value = "go run ./cmd/devcheck stress-packages-cpu"
	}), `jobs.linux-stress-packages.steps[3].run: must run devcheck stage "stress-packages", got "stress-packages-cpu"`)
	mustReject(t, "sidecar worker runs sidecar-cpu4", mutated(t, func(r *yaml.Node) {
		node(t, r, "jobs", "linux-stress-sidecar", "steps", 3, "run").Value = "go run ./cmd/devcheck stress-sidecar-cpu4"
	}), `jobs.linux-stress-sidecar.steps[3].run: must run devcheck stage "stress-sidecar", got "stress-sidecar-cpu4"`)
	// The packages-cpu workers', the CPU1 workers' and the pairs' own
	// stage, flags, budget, runner, offline check environment, independence
	// and display name are fixed.
	pkgs := map[string]string{"packages-cpu": "contract", "plane-cpu1": "plane", "plane": "plane", "sidecar-cpu1": "sidecar", "sidecar": "sidecar", "sidecar-cpu4": "sidecar"}
	sibling := map[string]string{"packages-cpu": "stress-packages", "plane-cpu1": "stress-plane", "plane": "stress-plane-cpu1", "sidecar-cpu1": "stress-sidecar", "sidecar": "stress-sidecar-cpu1", "sidecar-cpu4": "stress-sidecar"}
	for _, shard := range []string{"packages-cpu", "plane-cpu1", "plane", "sidecar-cpu1", "sidecar", "sidecar-cpu4"} {
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

// FP-4 of iteration 12a-stress-sidecar-margin (historically FP-5, and FP-3
// of the stress worker rebalance): docs/maintainers/ci.md documents the four required
// contexts versus the eighteen workers, the nine shards with their exact
// commands (twenty-two since the headroom fixes, still the iteration 02b
// selection) and command ownership (packages one, packages-cpu nine,
// sidecar CPU2 and sidecar CPU4 one each), the CPU1 singletons, the plane
// CPU2/CPU4 pair and the sidecar singletons with their log ownership and
// failure boundaries, why CPU1 is isolated (design 06a-perf), why the
// per-CPU groups have their own worker (the stress worker rebalance) and
// why sidecar CPU4 has its own worker (iteration 12a-stress-sidecar-margin),
// the preserved budgets and contexts with the rebalance's 675-second review
// target and planning envelopes, the isolation measurements and sensitivity
// calculations, estimates versus observations, the superseded plane
// fallback, the history of the sidecar follow-up, the owner's fixed
// first-remote-run rule and this iteration's first-remote-run evidence
// checklist (instructions; hosted qualification is unobserved locally).

func TestStressShardHandoff(t *testing.T) {
	checks := docSection(t, "Checks")
	requireTerms(t, "Checks", checks,
		"Twenty-two fixed jobs run on every trigger",
		"Exactly four of them are the required status check contexts on `main`: `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`",
		"The other eighteen are the stress workers", "nine shards per platform", "not required contexts",
		"the two packages-cpu workers since the stress worker rebalance of 2026-10-07",
		"the two sidecar CPU4 workers since iteration 12a-stress-sidecar-margin",
		"| `ci-linux-stress` | `ubuntu-24.04` | 5 min | required summary |", "| `ci-macos-stress` | `ubuntu-24.04` | 5 min | required summary |",
		"succeeds only if the nine Linux workers all concluded `success`", "succeeds only if the nine macOS workers all concluded `success`",
		"if: ${{ always() }}", summaryCommand, "PACKAGES_CPU_RESULT: ${{ needs['linux-stress-packages-cpu'].result }}",
		"PLANE_CPU1_RESULT: ${{ needs['linux-stress-plane-cpu1'].result }}",
		"PLANE_RESULT: ${{ needs['linux-stress-plane'].result }}", "SIDECAR_CPU1_RESULT: ${{ needs['linux-stress-sidecar-cpu1'].result }}",
		"SIDECAR_RESULT: ${{ needs['linux-stress-sidecar'].result }}",
		"SIDECAR_CPU4_RESULT: ${{ needs['linux-stress-sidecar-cpu4'].result }}",
		"needs: [linux-stress-packages, linux-stress-packages-cpu, linux-stress-plane-cpu1, linux-stress-plane, linux-stress-sidecar-cpu1, linux-stress-sidecar, linux-stress-sidecar-cpu4, linux-stress-processgroup, linux-stress-functions]",
		"Only all nine results `success` exit zero",
		"`failure`, `cancelled`, `skipped`, an empty or any unknown result in any position fails the summary",
		"never put on the job's `if`", "Summaries hold no stress logs", "all eighteen workers check out",
		"`devcheck stress-packages` on Linux: the combined nine-package invocation alone",
		"`devcheck stress-packages-cpu` on Linux: the `internal/contract`, `internal/mcpqual` and `internal/workspace` per-CPU groups",
		"so the packages workers now run the combined invocation only",
		"`devcheck stress-plane` on Linux: `internal/plane` CPU 2 and CPU 4 as two concurrent invocations",
		"`devcheck stress-plane-cpu1` on Linux: `internal/plane` at CPU 1, one invocation alone on its worker (iteration 06a-perf)",
		// Historical dated quotations stay (design stress-rebalance).
		"CI stays 18 jobs")
	for _, j := range shardJobs[2:20] {
		requireTerms(t, "Checks", checks, fmt.Sprintf("| `%s` | `%s` | %s min | worker |", j.name, j.runner, j.timeout))
	}
	for _, stale := range []string{"An all-success triple alone exits zero", "The other six are the stress workers", "Ten fixed jobs run on every trigger",
		"The other eight are the stress workers", "Twelve fixed jobs run on every trigger", "Only all four results `success` exit zero",
		"Fourteen fixed jobs run on every trigger", "The other ten are the stress workers", "Only all five results `success` exit zero",
		"five shards per platform", "all ten workers check out", "succeeds only if the five Linux workers", "its three CPU settings as concurrent invocations (iteration 05b)",
		// The stress worker rebalance: design 06a-perf's eighteen-job layout.
		"Eighteen fixed jobs run on every trigger", "The other fourteen are the stress workers", "seven shards per platform",
		"succeeds only if the seven Linux workers", "succeeds only if the seven macOS workers", "Only all seven results `success` exit zero",
		"all fourteen workers check out", "The two main jobs and the fourteen workers start together",
		"each on its own platform's seven workers", "needs: [linux-stress-packages, linux-stress-plane-cpu1,",
		"Twenty fixed jobs run on every trigger", "The other sixteen are the stress workers", "eight shards per platform",
		"succeeds only if the eight Linux workers", "succeeds only if the eight macOS workers", "Only all eight results `success` exit zero",
		"all sixteen workers check out", "The two main jobs and the sixteen workers start together",
		"each on its own platform's eight workers"} {
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
		"nine shards per platform", "The nine shards run twenty-two commands",
		"`ci-linux-stress-packages`, `ci-linux-stress-packages-cpu`", "the nine `ci-macos-stress-*` workers run the same stages on macOS",
		// The stress worker rebalance: command ownership, one and nine.
		"| `packages` | `devcheck stress-packages` | `stress packages` | one invocation, the combined nine-package command alone (since the stress worker rebalance) |",
		"| `packages-cpu` | `devcheck stress-packages-cpu` | `stress contract cpu1`, `cpu2`, `cpu4`, then `stress mcpqual cpu1`, `cpu2`, `cpu4`, then `stress workspace cpu1`, `cpu2`, `cpu4` | one group per split package (`internal/contract`, `internal/mcpqual`, `internal/workspace`), one group after another, each three concurrent invocations, one per CPU setting (the headroom fixes; their own shard since the stress worker rebalance) |",
		"The stress worker rebalance (2026-10-07) also changed no command, flag, selection or order, only command ownership",
		"the packages shard (`devcheck stress-packages`) now owns one command", "(`devcheck stress-packages-cpu`) owns nine",
		"packages-cpu contains all three per-CPU groups at CPU 1, 2 and 4; unlike plane-cpu1 and sidecar-cpu1 it is not a CPU1-only shard, and there is no `packages-cpu1` stage",
		"`internal/workspacetransfer` stays in the combined invocation",
		"the stress worker rebalance left the twenty-two and their order unchanged",
		"`devcheck stress` runs the shards in order, packages, packages-cpu, plane-cpu1, plane, sidecar-cpu1, sidecar, sidecar-cpu4, processgroup, functions",
		"in `devcheck stress` a failed packages shard never starts packages-cpu",
		"owned by `devcheck stress-packages-cpu`",
		"run both `devcheck stress-packages` and `devcheck stress-packages-cpu` (or `devcheck stress`); each stage then has its own 15-minute watchdog",
		// Why the per-CPU groups have a worker of their own, appended to
		// the preserved headroom and r0.5/r0.6 history.
		"Then the stress worker rebalance (2026-10-07, design stress-rebalance r0.2, owner sign-off 2026-10-07)",
		"main run 37596057274", "run 37640343460", "killed at 143 s (a censored observation, not a completion or an upper bound)",
		"It adds two jobs (18 to 20) and no required context",
		"The per-worker review target for this rebalance is 675 seconds (75% of the 900 s watchdog) on a slow runner, a review target and not a runtime timeout",
		// Budgets: per-stage and aggregate watchdogs and the planning
		// allocation with the owner's dispositions.
		"(`devcheck stress-packages` and `devcheck stress-packages-cpu` each one, since the stress worker rebalance)",
		"nine shards in one process, one after another, under one shared 15-minute watchdog rather than nine, with no reset at the packages/packages-cpu boundary",
		"112.120 seconds", "300 package seconds",
		"157.919 / 169.0", "181.557 / 191.7", "379.006 / −19.006",
		"ASSUMPTION (unverified): historical shared-worker slowdown factors transfer",
		"run 38931890238", "go1.26.0 darwin/arm64", "Do not promote 256, 265 or 341 seconds",
		"Stress worker rebalance allocation (design stress-rebalance r0.2",
		"| macOS packages | combined observations 407–574 | 574 + 45 allowance = 619 (10.3 min) |",
		"| macOS packages-cpu | completed-run CPU1 group sums 296–333, plus overhead | 123 contract + 130 mcpqual + 191 workspace + 45 allowance = 489 (8.2 min) |",
		"| Linux packages | approximately 436–441 before allowance | 441 × 1.5 + 15 ≈ 677 (11.3 min) |",
		"| Linux packages-cpu | approximately 297–302 before allowance | 302 × 1.5 + 45 = 498 (8.3 min) |",
		"The existing under-10-minute diagnostic target remains aspirational", "730 + 45 = 775 s", "about 791 s (88%)",
		"accepted as a marginal target exception pending the first remote run", "no saving credited to it",
		"The pull request must replace these proxies with actual values",
		// The rebalance's first remote run, recorded as measurements apart
		// from the projections (pull request #31's run).
		"Hosted run 37659127131 (pull request #31, the stress worker rebalance at 9c749ac", "all twenty jobs green",
		"Linux `devcheck: stress packages: ok in 304.0s`", "macOS `devcheck: stress packages: ok in 441.7s`",
		"`internal/workspacetransfer` 175.590 s", "`internal/workspacetransfer` 256.266 s",
		"Linux contract 140.7/138.1/141.2 s", "mcpqual 114.7/115.0/114.6 s", "workspace 104.9/106.5/108.8 s",
		"macOS contract 101.2/97.4/96.9 s", "mcpqual 121.7/116.5/114.7 s", "workspace 132.6/130.1/125.5 s",
		"every affected stage is below the 675-second review target", "Every binary is below its 360-second limit",
		"One green run validates execution but does not establish a slow-runner upper bound",
		"Hosted stress worker rebalance times: measured by run 37659127131 (above)",
		"`stressWaves` never applies to them", "`stress-contract-cpu1.log`", "`stress-workspace-cpu4.log`",
		"Why contract, mcpqual and workspace run per CPU setting", "run 36992345488", "run 37009626145",
		// Iteration 10b's r0.6 schedule: workspacetransfer tried a fourth
		// per-CPU group (r0.5) and returned to the combined invocation.
		"a group of its own was later tried and withdrawn", "Then iteration 10b's r0.6 schedule (2026-10-04) returned",
		"at most three invocations run at once on either runner",
		"| `plane-cpu1` | `devcheck stress-plane-cpu1` | `stress plane cpu1` | one invocation, alone on its worker (iteration 06a-perf) |",
		"| `plane` | `devcheck stress-plane` | `stress plane cpu2`, `cpu4` | two concurrent invocations, one per CPU setting (iteration 05b; CPU 1 moved to `plane-cpu1` in iteration 06a-perf) |",
		"| `sidecar-cpu1` | `devcheck stress-sidecar-cpu1` | `stress sidecar cpu1` | one invocation, alone on its worker (iteration 06a-perf) |",
		"| `sidecar` | `devcheck stress-sidecar` | `stress sidecar cpu2` | one invocation, alone on its worker (iteration 12a-stress-sidecar-margin; CPU 2 and CPU 4 were concurrent until then, since the iteration 05b sidecar follow-up; CPU 1 moved to `sidecar-cpu1` in iteration 06a-perf) |",
		"| `sidecar-cpu4` | `devcheck stress-sidecar-cpu4` | `stress sidecar cpu4` | one invocation, alone on its worker (iteration 12a-stress-sidecar-margin) |",
		"| `processgroup` | `devcheck stress-processgroup` | `stress processgroup cpu1`, `cpu2`, `cpu4` | three concurrent invocations, one per CPU setting |",
		// Execution, log ownership and failure boundaries.
		"The plane-cpu1, plane, sidecar-cpu1, sidecar, sidecar-cpu4 and processgroup shards are concurrent", "at most three at once within that shard", "no two shards overlap",
		"Since iteration 06a-perf `devcheck stress-plane` runs CPU 2 and CPU 4 only",
		"`devcheck stress-sidecar` runs CPU 2 only", "there is no `stress-sidecar-cpu2` stage",
		"run both `devcheck stress-plane-cpu1` and `devcheck stress-plane` (or `devcheck stress`)",
		"`devcheck stress-plane-cpu1` owns only `stress-plane-cpu1.log`", "`devcheck stress-plane` only `stress-plane-cpu2.log` and `stress-plane-cpu4.log`",
		"`devcheck stress-sidecar` only `stress-sidecar-cpu2.log`", "`devcheck stress-sidecar-cpu4` only `stress-sidecar-cpu4.log`",
		"no sidecar log is duplicated into the other sidecar worker",
		"A failure in `plane-cpu1` during `devcheck stress` prevents `plane` and every later shard from starting",
		"a failed CPU 2 invocation of the plane pair still waits for CPU 4 before its shard fails",
		"A failed sidecar CPU 2 invocation does not start the sidecar CPU 4 shard",
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
	if strings.Contains(strings.Join(strings.Fields(stress), " "), "No complete aggregate baseline is available") {
		t.Fatal("Stress checks still claims no complete aggregate baseline is available")
	}
	// The six anchors have to sit together in the budgets comparison.
	// sidecar-cpu1 and the shared watchdog also appear in older Stress
	// checks text, so a section-wide contains would accept a scatter.
	flatStress := strings.Join(strings.Fields(stress), " ")
	baselineStart := strings.Index(flatStress, "The failed main baseline and the implementation attempt are the comparison.")
	baselineEnd := strings.Index(flatStress, "Two additional worker setups, one per")
	if baselineStart < 0 || baselineEnd <= baselineStart {
		t.Fatal("Stress checks lacks the aggregate baseline comparison")
	}
	baseline := flatStress[baselineStart:baselineEnd]
	requireTerms(t, "Stress checks aggregate baseline", baseline,
		"901", "900.12", "sidecar-cpu1", "one shared 15-minute watchdog",
		"standalone stages", "failed, incomplete aggregate run",
		"unchanged main `4c25084`", "ended at 901s",
		"uncommitted implementation ended at 900.12s", "during sidecar-cpu1")
	expected := stress[strings.Index(stress, "Expected per-job wall-clock after the stress worker rebalance"):strings.Index(stress, "Expected per-job wall-clock after iteration 06a-perf")]
	for _, j := range shardJobs {
		if strings.HasSuffix(j.id, "-sidecar-cpu4") {
			continue // the rebalance history predates the sidecar CPU4 workers
		}
		requireTerms(t, "Stress checks expected times after the stress worker rebalance", expected, "`"+j.name+"` about")
	}
	expected = stress[strings.Index(stress, "Expected per-job wall-clock after iteration 06a-perf"):]
	for _, j := range shardJobs {
		if strings.HasSuffix(j.id, "-packages-cpu") || strings.HasSuffix(j.id, "-sidecar-cpu4") {
			continue // the 06a-perf history predates the packages-cpu and sidecar CPU4 workers
		}
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
		"`stress workspacetransfer cpu1`", "`stress-workspacetransfer-cpu1.log`",
		// The stress worker rebalance: the seven-shard packages layout.
		"In CI it runs as seven shards per platform", "The seven shards run twenty-two commands",
		"| `packages` | `devcheck stress-packages` | `stress packages`, then `stress contract cpu1`",
		"The packages shard runs `stress packages` first and then, only if it succeeded, its per-CPU groups",
		"owned by `devcheck stress-packages`, replayed", "The packages shard's per-CPU groups are not a wave schedule",
		"runs all seven shards in one process, one after another, under one shared 15-minute watchdog rather than seven",
		"the seven `ci-macos-stress-*` workers run the same stages",
		"eight shards per platform", "The eight shards run twenty-two commands", "the eight `ci-macos-stress-*` workers run the same stages",
		"`devcheck stress-plane` and `devcheck stress-sidecar` now run CPU 2 and CPU 4 only",
		"no log is duplicated into the CPU 2 and CPU 4 worker",
		"| `sidecar` | `devcheck stress-sidecar` | `stress sidecar cpu2`, `cpu4` | two concurrent invocations",
		// The rebalance's hosted run exists (run 37659127131).
		"Hosted stress worker rebalance times: pending", "pending (the implementation pull request's first remote run), not passed"} {
		if strings.Contains(strings.Join(strings.Fields(stress), " "), stale) {
			t.Fatalf("Stress checks keeps the obsolete %q", stale)
		}
	}
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp, "eighteen worker contexts are not required",
		"the two packages-cpu workers since the stress worker rebalance",
		"the two sidecar CPU4 workers since iteration 12a-stress-sidecar-margin",
		"no protection change is needed for 02c, 05b, its sidecar follow-up, 06a-perf, the stress worker rebalance or iteration 12a-stress-sidecar-margin",
		"`ci-linux-stress-sidecar-cpu4`", "`ci-macos-stress-sidecar-cpu4`", "both summaries now take nine results",
		"no administrative action is required")
	if strings.Contains(strings.Join(strings.Fields(bp), " "), "fourteen worker contexts are not required") ||
		strings.Contains(strings.Join(strings.Fields(bp), " "), "sixteen worker contexts are not required") {
		t.Fatal("Branch protection keeps an obsolete worker count")
	}
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
	// The stress worker rebalance's handoff: two diagnostic packages-cpu
	// workers, twenty jobs, the same four required contexts.
	handoff = handoffOf("\n### packages-cpu workers (stress worker rebalance)\n")
	requireTerms(t, "packages-cpu workers", handoff, "`ci-linux-stress-packages-cpu`", "`ci-macos-stress-packages-cpu`",
		"both summaries now take eight results", "The new workers are diagnostic checks, not required contexts",
		"The required contexts stay exactly `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`", "no protection change is made")
	steps = numberedSteps(t, handoff)
	review = stepIndex(t, steps, "REVIEW_APPROVED", "stress worker rebalance", "commit the reviewed code")
	push = stepIndex(t, steps, "Push `stress-rebalance`")
	green = stepIndex(t, steps, "all twenty jobs", "sixteen stress workers", "all four required checks", "current PR merge revision")
	verify = stepIndex(t, steps, "owner verifies", "read-only", "same four required contexts", "no packages-cpu worker")
	merge = stepIndex(t, steps, "Merge only with all four checks green", "first-remote-run evidence recorded")
	if !(review < push && push < green && green < verify && verify < merge) {
		t.Fatalf("packages-cpu handoff order review=%d push=%d green=%d verify=%d merge=%d", review, push, green, verify, merge)
	}
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "conclusions of all twenty-two jobs", "eighteen worker logs (the summaries hold none)",
		"`devcheck: stage stress-sidecar-cpu4 ok`", "`devcheck: stage stress-packages-cpu ok`",
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
		"The ≤250-second macOS CPU1 goal is a first-run hypothesis, not an additional acceptance gate", "There is no two-run requirement",
		// The stress worker rebalance's delivery gate: its evidence
		// checklist (instructions, not measurements; hosted evidence
		// pending at local review stays pending, not passed), then the
		// record of the run that met it, whose figures live in Stress
		// checks.
		"for the stress worker rebalance (design stress-rebalance, delivery gate; hosted evidence pending at local code review remains pending, not passed, and is outside the implementation acceptance bar)",
		"collect the entire successful run of the implementation pull request on both platforms", "every stage's elapsed time",
		"the combined command's time and each of its nine binaries' times", "all nine per-CPU invocation times", "each group's maximum",
		"job setup duration", "the setup-go build cache state", "the runner's architecture and core count", "both summary outcomes and the workflow critical path",
		"Preserve failed runs too", "with projections distinguished from measurements",
		"comparing each affected stage (`stress-packages` and `stress-packages-cpu`, Linux and macOS) to the 675-second review target and each binary to its 360-second limit",
		"any timeout or assertion failure blocks qualification",
		"A green fast-runner pull request validates execution but does not establish a slow-runner upper bound; record that uncertainty explicitly",
		"never a raised timeout, a reduced count or a weakened test",
		"Recorded: hosted run 37659127131", "(Stress checks, Hosted run 37659127131)",
		"for iteration 12a-stress-sidecar-margin",
		"failed, incomplete aggregate run", "never relabelled successful",
		"standalone stages", "raw/local-aggregate-main-4c25084-taskset0-2.log",
		"raw/coder-impl-needs-design-report.txt", "not the triage host",
		"ASSUMPTION (unverified): hardware identity and affinity as reported",
		"300 package seconds", "hosted qualification is pending, not passed")
	// Design 12a-mcpqual-stress-budget (FP-2): the dated first-remote-run
	// checklist: the 390-second threshold and 90-second margin over two
	// hosted macOS samples, package seconds rather than command wall time,
	// the unchanged 15-minute and 20-minute budgets with their projections,
	// and the pending-evidence rule. Its mcpqual-only Budgets sentence was
	// replaced by design 12a-stress-packages-budget's policy sentence
	// (stressPackagesBudgetHandoff); this checklist stays.
	requireTerms(t, "First remote run", first,
		"Mcpqual qualification requires two samples on separate macos-15 allocations of the same implementation SHA, with CPU1/2/4 each reporting an ok package result at or below 390.000 seconds, leaving at least 90 seconds of margin.",
		"Use package seconds, not devcheck command wall time, which includes the build; obtain sample two by re-running the PR's ci-macos-stress-packages-cpu job with gh run rerun --job and record both attempts here.",
		"The 15-minute watchdog and 20-minute job timeout stay unchanged: the conditional stage projection is 824.0 seconds, or 734.0 seconds at the qualification threshold, and the conservative job projection including observed setup is 855 seconds.",
		"Hosted qualification is pending until both samples are recorded; pending evidence does not withhold REVIEW_APPROVED, the owner closes the gate at delivery sign-off, and a threshold breach or non-ok result reopens the design, not the implementation review.")
	if strings.Contains(strings.Join(strings.Fields(stress), " "), "Each test binary: `-timeout=6m`.") {
		t.Fatal("Stress checks keeps the universal six-minute binary budget")
	}
	// The package-split rule applies to default-budget packages only and
	// preserves the mcpqual 8m exception (code review W1).
	requireTerms(t, "Stress checks", stress,
		"If a default-budget package's test binary exceeds its 6-minute timeout, that package moves into its own stress step with its own 6-minute timeout (the `internal/mcpqual` stress binaries keep their 8-minute timeout of design 12a-mcpqual-stress-budget)")
	if strings.Contains(strings.Join(strings.Fields(stress), " "), "12a-mcpqual-stress-budget), that package moves into its own stress step with its own 6-minute timeout") {
		t.Fatal("Stress checks keeps the split rule that gives an mcpqual binary a 6-minute timeout")
	}
	stressPackagesBudgetHandoff(t, stress, first)
	sidecarCPU1BudgetHandoff(t, stress, first)
	mcpqualBudgetJobTimeouts(t, stress)
	for _, stale := range []string{"conclusions of all eighteen jobs: all four checks", "stress evidence from the fourteen worker logs", "the actual job and step times of all eighteen jobs",
		"conclusions of all twenty jobs", "sixteen worker logs (the summaries hold none)", "the actual job and step times of all twenty jobs"} {
		if strings.Contains(strings.Join(strings.Fields(first), " "), stale) {
			t.Fatalf("First remote run keeps the obsolete %q", stale)
		}
	}
	local := docSection(t, "Local verification")
	requireTerms(t, "Local verification", local, "go run ./cmd/devcheck stress-packages", "go run ./cmd/devcheck stress-plane-cpu1",
		"go run ./cmd/devcheck stress-sidecar-cpu1", "go run ./cmd/devcheck stress-sidecar-cpu4", "go run ./cmd/devcheck stress-processgroup", "go run ./cmd/devcheck stress-functions",
		"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run '^TestStressConcurrencyContract$' ./internal/devcheck",
		"without other stress work running on the same host", "all six concurrent shards, the packages-cpu groups and the wave mechanism on the processgroup shard",
		"`devcheck stress-packages` runs the combined invocation only; add `devcheck stress-packages-cpu`",
		"`devcheck stress-sidecar` runs CPU 2 only", "Run all three sidecar settings",
		"bounded convenience command", "standalone stages",
		"failed, incomplete aggregate run", "never relabelled successful",
		"raw/local-aggregate-main-4c25084-taskset0-2.log",
		"raw/coder-impl-needs-design-report.txt", "not the triage host",
		"ASSUMPTION (unverified): hardware identity and affinity as reported",
		"300 package seconds", "Darwin `devcheck native` is unobserved")
	for _, stage := range []string{"stress-packages", "stress-packages-cpu", "stress-plane", "stress-sidecar", "stress-sidecar-cpu4"} {
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
		"Iteration 06a-perf (the plane and sidecar CPU1 stress workers) is delivered with iteration 06a in pull request #6", "all eighteen jobs",
		"The stress worker rebalance (the packages-cpu stress workers) is delivered in its own pull request (branch `stress-rebalance`)",
		"It adds two worker jobs and changes no required context", "the workflow now has twenty jobs", "all twenty jobs",
		"Iteration 12a-stress-sidecar-margin (the sidecar CPU4 stress workers)", "the workflow now has twenty-two jobs", "all twenty-two jobs")
	// No workflow step can change repository settings.
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "--method", "POST", "curl", "protection", "contents: write", "secrets.", "continue-on-error", "strategy:"} {
		if strings.Contains(wf, forbidden) {
			t.Fatalf("workflow contains %q", forbidden)
		}
	}
}

// mcpqualBudgetJobTimeouts pins the post-final erratum to design
// 12a-mcpqual-stress-budget's dated Stress checks block (owner decision
// 2026-10-26, delivered with design 12a-triage-scratch-guard-wording): only
// the eighteen stress workers have 20-minute job timeouts
// (.github/workflows/ci.yml), so the block states each job's timeout as the
// 12a-stress-packages-budget block does and no longer gives all twenty-two
// jobs 20 minutes.
func mcpqualBudgetJobTimeouts(t *testing.T, stress string) {
	t.Helper()
	flat := strings.Join(strings.Fields(stress), " ")
	const heading = "Why the mcpqual stress binaries have eight minutes (design 12a-mcpqual-stress-budget, owner decision and sign-off 2026-10-21)."
	start := strings.Index(flat, heading)
	if start < 0 {
		t.Fatalf("Stress checks lacks the dated block %q", heading)
	}
	block := flat[start:]
	end := strings.Index(block, "Why the combined packages command has eight minutes")
	if end < 0 {
		t.Fatal("the 12a-mcpqual-stress-budget block does not end before the 12a-stress-packages-budget block")
	}
	block = block[:end]
	requireTerms(t, "the 12a-mcpqual-stress-budget block", block,
		"`-count=20`, the 15-minute watchdog and the twenty-two jobs with their timeouts (20 minutes for each of the eighteen stress workers, 45 minutes for `ci-linux`, 30 minutes for `ci-macos` and 5 minutes for each of the two summaries) are unchanged.")
	if strings.Contains(block, "twenty-two jobs with their 20-minute timeouts") {
		t.Fatal("the 12a-mcpqual-stress-budget block gives all twenty-two jobs 20-minute timeouts")
	}
}

// stressPackagesBudgetHandoff is design 12a-stress-packages-budget's FP-2
// handoff, called by TestStressShardHandoff with the Stress checks and First
// remote run sections: the exact policy sentence and its two constants in
// Stress checks, the dated budget explanation (scoped to its own block: the
// samples and their provenance caveat, the censored-alarm and estimate
// arithmetic, the 390.000-second and 90-second threshold, package seconds
// rather than command wall, the 734.1/963.0 conditional projections under
// the 900/1200 limits, the unchanged aggregate behavior and the
// supersession line), the split rule's exclusions, and the First remote run
// checklist (scoped to its own entry) with its pending contract.
func stressPackagesBudgetHandoff(t *testing.T, stress, first string) {
	t.Helper()
	flat := strings.Join(strings.Fields(stress), " ")
	// The current policy, constants, counts and split-rule exclusion as
	// updated by design 12a-stress-sidecar-cpu1-budget (sidecar CPU1 is the
	// third exception: three constants, five commands at 8m and seventeen
	// at 6m).
	requireTerms(t, "Stress checks", stress,
		sidecarCPU1Policy,
		"`stressPackagesTestTimeout` (design 12a-stress-packages-budget", "`stressMcpqualTestTimeout` (design 12a-mcpqual-stress-budget",
		"three separate static constants in `internal/devcheck/stress.go`", "no option or environment variable changes any value",
		"so `internal/workspacetransfer` or any other of its packages considered alone keeps six minutes",
		"design 12a-stress-packages-budget (2026-10-25) changed the same flag of one more, the combined `stress packages` command, for all nine of its binaries, and design 12a-stress-sidecar-cpu1-budget (2026-10-27) changed it for one more, the `stress sidecar cpu1` command only; five commands carry `-timeout=8m` and the other seventeen, sidecar CPU2 and CPU4 included, carry `-timeout=6m`",
		// The split rule: the preserved clause (checked above) and the
		// appended exclusion of all three exceptions.
		"This default-budget split rule excludes the sidecar CPU1 command, the combined `stress packages` command and the `internal/mcpqual` stress binaries; a breach of any of these exceptions requires investigation/design revision, not an automatic split (such as a transfer split) or another timeout increase.")
	for _, stale := range []string{
		"Each test binary: `-timeout=6m`, except `internal/mcpqual` stress binaries: `-timeout=8m`.",
		"Each test binary: `-timeout=6m`, except the combined `stress packages` command's binaries and `internal/mcpqual` stress binaries: `-timeout=8m`",
		"two separate static constants", "the other eighteen commands carry `-timeout=6m`",
		"This default-budget split rule excludes the combined `stress packages` command and the `internal/mcpqual` stress binaries;",
		"and every other command keeps `-timeout=6m` (see Budgets)",
		"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication",
	} {
		if strings.Contains(flat, stale) {
			t.Fatalf("Stress checks keeps the superseded %q", stale)
		}
	}
	const heading = "Why the combined packages command has eight minutes (design 12a-stress-packages-budget, owner decision and sign-off 2026-10-25)."
	start := strings.Index(flat, heading)
	if start < 0 {
		t.Fatalf("Stress checks lacks the dated block %q", heading)
	}
	block := flat[start:]
	// Design 12a-stress-sidecar-cpu1-budget places its own dated block
	// between this one and Race and CPU scope.
	end := strings.Index(block, sidecarCPU1BudgetHeading)
	if end < 0 {
		t.Fatal("the 12a-stress-packages-budget block does not end before the 12a-stress-sidecar-cpu1-budget block")
	}
	block = block[:end]
	requireTerms(t, "the 12a-stress-packages-budget block", block,
		"run [40224728895](https://github.com/wedevwork/callsheet/actions/runs/40224728895)", "job 121263572346", "`panic: test timed out after 6m0s`",
		"/tmp/triage/20261025-203310-main-macos-stress-workspacetransfer/", "coordinator-supplied historical package observations recorded in `raw/margin-calc.txt`, not newly fetched logs",
		// The samples, the censored failure and the supplement.
		"| Run / sample | workspacetransfer seconds | taskworkspace seconds | taskpublication seconds | 480 minus slowest completed package shown |",
		"| 40131356740 | 169.165 | 144.140 | 139.131 | 310.835 |", "| 40215191666 | 197.284 | 159.284 | 173.018 | 282.716 |",
		"| 40193461322 | 216.828 | 172.276 | 182.334 | 263.172 |", "| 40190954022 | 240.519 | 200.576 | 196.813 | 239.481 |",
		"| 40133334715 | 292.704 | 247.343 | 246.274 | 187.296 |", "| 40224728895 attempt 1 | FAIL 360.050, censored | 304.360 | 309.822 | 170.178 (siblings only) |",
		"| 40224728895 attempt 2, coordinator supplement | 223.049 | 184.572 | 193.481 | 256.951 |",
		"ASSUMPTION (unverified provenance)", "job 121269084389", "the bundle holds no matching lines",
		"the coordinator's saved log is attached to the implementing pull request and its job, SHA and those lines are matched",
		"Its 394-second stage is not package seconds and is never substituted for them", "not needed to select the cap",
		// The arithmetic and the threshold.
		"leaves 187.296 s under 480", "480 - 360.050 = 119.950 s is the distance from an alarm endpoint, not a measured completion margin",
		"368.478–369.054 s, leaving 111.522–110.946 s", "383.070 s, leaves 96.930 s", "planning allowances, not observed completion times or bounds",
		"Seven minutes would leave only 36.930 s above that high estimate",
		"Qualification threshold: 390.000 package seconds", "at least 90 s (18.75% of 480) of measured margin in an accepted sample",
		"not a prediction and not a new CI gate", "all nine must be `ok` below 480", "No finite cap guarantees runner speed",
		"292.704 s is only the sampled maximum, not every hosted run's maximum",
		// Package seconds versus wall time.
		"Package seconds, not devcheck command wall time", `"If a test binary runs longer than duration d, panic."`,
		"bounds each binary, not the whole invocation, its compilation or the staggered package starts",
		// Stage and job fit.
		"614.1 - 360 = 254.1 s", "843 - 614.1 = 228.9 s", "The packages worker runs no other devcheck stage",
		"| Scenario | Conditional packages stage | Margin under 900s | Whole job, adding 228.9s outside stage | Margin under 1200s |",
		"| Estimated 369s transfer | 623.1 | 276.9 | 852.0 | 348.0 |", "| 390s qualification target | 644.1 | 255.9 | 873.0 | 327.0 |",
		"| Full 480s critical binary allowance | 734.1 | 165.9 | 963.0 | 237.0 |",
		"ASSUMPTION (unverified): the critical-path substitution holds", "734.1 s and 963.0 s are the worst listed planning scenario, not a mathematical worst-case bound",
		"the unchanged watchdog enforces termination", "invalidates that projection for owner review even when the hard limits pass",
		"675-second stage target stays diagnostic; 734.1 s deliberately exceeds it",
		// Unchanged surroundings and aggregate behavior.
		"the 15-minute watchdog (one per stage, one shared by the aggregate), the twenty-two jobs with their timeouts (20 minutes for each of the eighteen stress workers, 45 minutes for `ci-linux`, 30 minutes for `ci-macos` and 5 minutes for each of the two summaries) and the required contexts are unchanged",
		"Aggregate `devcheck stress` behavior is unchanged: one shared 15-minute (900 s) watchdog across all nine shards",
		"up to 120 s more binary allowance on its first command, and no full-aggregate worst-case fit is promised",
		"remains a failed, incomplete run, not hosted-worker evidence", "a new aggregate regression on a previously completing host is not waived",
		// Supersession.
		"This supersedes the mcpqual block’s statement ‘every other stress binary keeps `-timeout=6m`’ for the combined `stress packages` command only.",
		"that design's exclusion of a per-CPU `internal/workspacetransfer` split stays in force",
		"hosted verification is pending, not passed")
	// Code review W1: only the eighteen stress workers have 20-minute job
	// timeouts (.github/workflows/ci.yml), not all twenty-two jobs.
	if strings.Contains(block, "twenty-two jobs with their 20-minute timeouts") {
		t.Fatal("the 12a-stress-packages-budget block gives all twenty-two jobs 20-minute timeouts")
	}
	// The mcpqual block keeps its own statement, now superseded only for
	// the combined command.
	requireTerms(t, "Stress checks", stress, "every other stress binary keeps `-timeout=6m`")
	const entry = "- for 12a-stress-packages-budget (2026-10-25;"
	i := strings.Index(first, entry)
	if i < 0 {
		t.Fatalf("First remote run lacks %q", entry)
	}
	checklist := first[i+2:]
	if j := strings.Index(checklist, "\n- "); j >= 0 {
		checklist = checklist[:j]
	}
	if j := strings.Index(checklist, "\n\n"); j >= 0 {
		checklist = checklist[:j]
	}
	requireTerms(t, "the 12a-stress-packages-budget checklist", checklist,
		"owner-closed delivery verification, outside the implementation acceptance bar",
		"records on the implementing pull request the first completed implementation run's `ci-macos-stress-packages` (macos-15) and `ci-linux-stress-packages` (ubuntu-24.04)",
		"the run, job and attempt links", "the pull request head SHA and the actual checked-out SHA, recorded separately",
		"the runner image, architecture and core count, the Go version and the cache context",
		"`internal/workspacetransfer`, `internal/taskworkspace` and `internal/taskpublication`", "record `N` and `480 - N` for each",
		"require each `N` at or below 390.000 seconds", "all nine packages succeeded and the actual maximum package result with its margin",
		"Use package seconds, not devcheck command wall time",
		"Record the command wall, the whole stage, the setup and tail and the whole-job durations separately",
		"require the stage below 900 seconds and the job below 1200 seconds", "the 734.1-second stage and 963.0-second job projections",
		"Preserve failed attempts", "Missing, cached, skipped, cancelled, failed or timed-out output never qualifies",
		"Record the complete workflow's success and both summaries' success", "No repeat-until-green sampling and no new workflow gate",
		"match job 121269084389, SHA `f2ee9d2` and its three package lines first",
		"Pending hosted evidence does not block implementation review once its automated bar passes; until recorded it is pending, not passed",
		"record the first post-merge run with its actual SHA on that pull request", "The owner closes delivery verification",
		"never an automatic budget increase or a weakened test", "a successful sample above a projection invalidates that projection",
		"Local exact-argv tests cannot prove hosted margin")
	if strings.Contains(strings.Join(strings.Fields(checklist), " "), "Hosted verification: passed") {
		t.Fatal("the 12a-stress-packages-budget checklist records pending evidence as passed")
	}
	// The owner's 2026-10-26 closure of both stress-packages jobs (design
	// 12a-probe11-closeout), scoped to this entry; closure is an attributed
	// record, never a bare passed claim.
	requireHostedClosure(t, first, stressPackagesClosure)
}

// Design 12a-stress-sidecar-cpu1-budget's verbatim documentation pins
// (Budget qualification, Documentation): the current policy sentence, the
// dated block's heading, its supersession sentence (with U+2019 in
// "blocks’", byte for byte as in docs/maintainers/ci.md, the post-final DS1
// clarification) and the First remote run entry's prefix.
const (
	sidecarCPU1Policy          = "Each test binary: `-timeout=6m`, except sidecar CPU1, the combined `stress packages` command's binaries and `internal/mcpqual` stress binaries: `-timeout=8m`."
	sidecarCPU1BudgetHeading   = "Why the sidecar CPU1 stress binary has eight minutes (design 12a-stress-sidecar-cpu1-budget, owner decision 2026-10-27)."
	sidecarCPU1Supersession    = "This supersedes the unchanged-timeout policy of design 12a-stress-sidecar-margin and the mcpqual and combined-packages budget blocks’ statements that every other stress command or binary keeps `-timeout=6m`, for sidecar CPU1 only; sidecar CPU2 and CPU4 keep `-timeout=6m`."
	sidecarCPU1ChecklistPrefix = "- for 12a-stress-sidecar-cpu1-budget (2026-10-27;"
)

// sidecarCPU1BudgetHandoff is design 12a-stress-sidecar-cpu1-budget's FP-2
// handoff, called by TestStressShardHandoff with the Stress checks and First
// remote run sections: the exact current policy sentence; the dated block,
// scoped from its exact heading to Race and CPU scope (both markers are
// required before any term is checked, so an adjacent block cannot satisfy
// it), with the evidence and its provenance caveat, the correctly
// attributed step-wall distributions, the 390.000-second and 90-second
// package threshold, package seconds rather than wall time, the 493.8/535.8
// conditional projections under the 900/1200 limits, the aggregate
// distinction and the verbatim supersession sentence; and the First remote
// run checklist, scoped from its exact prefix to the next list entry or
// blank line, with two macOS samples plus one Linux sample and the pull
// request and pending contract.
func sidecarCPU1BudgetHandoff(t *testing.T, stress, first string) {
	t.Helper()
	// DS1: the Go literal carries U+2019 (bytes E2 80 99) in "blocks’", as
	// docs/maintainers/ci.md does; requireTerms then matches the docs byte for byte.
	if !strings.Contains(sidecarCPU1Supersession, "budget blocks\xe2\x80\x99 statements") || strings.Contains(sidecarCPU1Supersession, "blocks' ") {
		t.Fatal("the supersession sentence literal does not use U+2019 in blocks’")
	}
	// The flattened-order listing carries the new CPU1 line, and CPU2 and
	// CPU4 keep six minutes.
	if !strings.Contains(stress, "\ngo test -race -count=20 -cpu=1 -timeout=8m ./internal/sidecar\ngo test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar\ngo test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar\n") ||
		strings.Contains(stress, "\ngo test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar\n") {
		t.Fatal("Stress checks does not list the sidecar CPU1 command at -timeout=8m beside CPU2 and CPU4 at -timeout=6m")
	}
	requireTerms(t, "Stress checks", stress, sidecarCPU1Policy,
		"`stressSidecarCPU1TestTimeout` (design 12a-stress-sidecar-cpu1-budget",
		"`stressPackageTimeout` still returns six minutes for `internal/sidecar` and sidecar CPU2 and CPU4 keep `-timeout=6m`",
		"The three exceptions are three separate static constants",
		"Five commands carry `-timeout=8m` (the combined `stress packages` command, the three `internal/mcpqual` commands and `stress sidecar cpu1`) and the other seventeen `-timeout=6m`; the 15-minute watchdog and the 20-minute worker job timeouts are unchanged.",
		"`stressTestTimeout` keeps `-timeout=6m` for every other binary, sidecar CPU2 and CPU4 included.")
	flat := strings.Join(strings.Fields(stress), " ")
	for _, wrong := range []string{"-cpu=2 -timeout=8m ./internal/sidecar", "-cpu=4 -timeout=8m ./internal/sidecar"} {
		if strings.Contains(flat, wrong) {
			t.Fatalf("Stress checks gives sidecar CPU2 or CPU4 eight minutes: %q", wrong)
		}
	}
	start := strings.Index(flat, sidecarCPU1BudgetHeading)
	if start < 0 {
		t.Fatalf("Stress checks lacks the dated block %q", sidecarCPU1BudgetHeading)
	}
	block := flat[start:]
	end := strings.Index(block, "Race and CPU scope:")
	if end < 0 {
		t.Fatal("the 12a-stress-sidecar-cpu1-budget block does not end before Race and CPU scope")
	}
	block = block[:end]
	if strings.Count(flat, sidecarCPU1BudgetHeading) != 1 || strings.Index(flat, "Why the combined packages command has eight minutes") > start {
		t.Fatal("the 12a-stress-sidecar-cpu1-budget block is not the one block after the 12a-stress-packages-budget block")
	}
	requireTerms(t, "the 12a-stress-sidecar-cpu1-budget block", block,
		// The failure and its evidence.
		"run [40412993068](https://github.com/wedevwork/callsheet/actions/runs/40412993068)", "attempt 1 failed `ci-macos-stress-sidecar-cpu1` on macos-15 at main `dc1522c`",
		"`go test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar`", "`panic: test timed out after 6m0s`",
		"`FAIL github.com/wedevwork/callsheet/internal/sidecar 360.214s`", "`devcheck: stress sidecar cpu1: FAILED after 371.2s`",
		"not a hang or a new hotspot", "`git diff --stat 5fb2c40..dc1522c -- internal/sidecar` is empty", "last changed at `db0c361`",
		"/tmp/triage/20261026-235705-main-macos-stress-sidecar-cpu1/",
		// The samples and the censored failure.
		"| Run / sample | CPU1 package seconds N | 480 - N seconds | Provenance |",
		"| 40334465524 | 189.607 | 290.393 |", "| 40383771739 | 258.621 | 221.379 |", "| 40297225653 | 279.092 | 200.908 |",
		"| 40404656520 | 280.364 | 199.636 |", "| 40412993068 attempt 2 | 283.934 | 196.066 | Coordinator supplement; unverified provenance |",
		"| 40412993068 attempt 1 | FAIL 360.214, censored | 119.786 | Alarm endpoint, NOT completion margin |",
		// The change and the unchanged surroundings.
		"Only the sidecar CPU1 command, `stress sidecar cpu1` of the sidecar-cpu1 shard, gets `-timeout=8m` (480 s), on Linux and macOS alike",
		"`go test -race -count=20 -cpu=1 -timeout=8m ./internal/sidecar`", "Sidecar CPU2 and CPU4 remain separate singleton workers at `-timeout=6m`",
		"the twenty-two jobs with their timeouts (20 minutes for each of the eighteen stress workers, 45 minutes for `ci-linux`, 30 minutes for `ci-macos` and 5 minutes for each of the two summaries) and the required contexts are unchanged",
		sidecarCPU1Supersession,
		"isolation topology, lifecycle, CPU2 and CPU4 budgets and qualification history remain intact", "are historical and keep the limits they had at the time",
		// The provenance caveat.
		"attributed coordinator observations recorded in `MANIFEST.md`, not independently fetched package logs",
		"ASSUMPTION (unverified provenance): 283.934 s is the `ok` package duration of attempt 2 (job 121899307717)",
		"That supplement is absent from the bundle", "proves only a successful 299-second step",
		"its saved log is attached to the implementing pull request and the run, job, attempt, SHA and exact package line are matched",
		"No historical sample substitutes for implementation qualification", "not needed to choose the owner-set cap",
		// The distributions, correctly attributed.
		"all 34 successful step walls: median 306s, p90 342s, max 379s", "the 17 post-isolation steps: median 306s, p90 338s, max 348s",
		"The 379-second maximum predates the CPU4 isolation", "the archive does not specify its percentile or rounding convention",
		"this is a sample, not a population bound",
		// The threshold.
		"Qualification threshold: 390.000 package seconds", "480 - 390 = 90 s, 18.75% of the cap",
		"not a new automatic CI gate and not a prediction", "109.636 s above the largest archived completed package observation (280.364 s)",
		"106.066 s above the unverified supplement", "29.786 s above the censored alarm endpoint",
		"11 s (all 34) and 42 s (post-isolation), but those are cross-metric comparisons, NOT observed package margins",
		"No finite cap guarantees runner speed", "the failed runner's missing tail is unknown",
		// Package seconds versus wall time.
		"Package seconds, not devcheck command wall time", `"If a test binary runs longer than duration d, panic."`,
		"The flag bounds the binary, not the build or launcher time", "step minus package is 13.786 s, rounded 13.8 s",
		"Do not subtract this allowance from other steps to invent package observations", "qualification uses the actual `ok ... Ns` line",
		// Shard and job fit.
		"add zero sequential steps to this worker", "5m 41s = 341 s", "341 - 299 = 42 s",
		"| Scenario | Conditional step/stage envelope N + 13.8 | Margin under 900s | Whole-job envelope + 42 | Margin under 1200s |",
		"| 390s qualification target | 403.8 | 496.2 | 445.8 | 754.2 |", "| Full 480s binary allowance | 493.8 | 406.2 | 535.8 | 664.2 |",
		"ASSUMPTION (unverified): future build, launcher and replay cost stays within 13.8 s and setup and tail within 42 s",
		"applies that envelope to Linux provisionally too", "not mathematical upper bounds or independent Linux observations",
		"942 s, leaving 258 s under 1200", "No outer budget change is justified",
		"A successful run above a projection invalidates that projection for owner review even when it meets the hard limits",
		// The aggregate distinction.
		"one shared 15-minute (900 s) watchdog context is created before all nine shards and is not reset", "480 - 360 = 120 s",
		"capped by the caller's earlier deadline", "261.5 + 65.0 + 155.1 + 64.5 + 172.2 + 149.2 = 867.5 s", "leaving only 32.5 s",
		"remains a failed, incomplete aggregate, not hosted-worker evidence", "neither 390 s nor 480 s fits that remainder",
		"No full-aggregate worst-case fit is promised", "a new aggregate regression on a previously completing host is not waived",
		"hosted verification is pending, not passed")
	for _, wrong := range []string{"all 34 successful step walls: median 306s, p90 338s", "Hosted verification: passed", "hosted verification passed",
		"493.8 s and 535.8 s are upper bounds", "the 13.8 s residual is a measured bound"} {
		if strings.Contains(block, wrong) {
			t.Fatalf("the 12a-stress-sidecar-cpu1-budget block states %q", wrong)
		}
	}
	i := strings.Index(first, sidecarCPU1ChecklistPrefix)
	if i < 0 {
		t.Fatalf("First remote run lacks %q", sidecarCPU1ChecklistPrefix)
	}
	checklist := first[i+2:]
	if j := strings.Index(checklist, "\n- "); j >= 0 {
		checklist = checklist[:j]
	}
	if j := strings.Index(checklist, "\n\n"); j >= 0 {
		checklist = checklist[:j]
	}
	requireTerms(t, "the 12a-stress-sidecar-cpu1-budget checklist", checklist,
		"owner-closed delivery verification, outside the implementation acceptance bar",
		"records on the implementing pull request the first completed implementation workflow's `ci-macos-stress-sidecar-cpu1` (macos-15) and `ci-linux-stress-sidecar-cpu1` (ubuntu-24.04), plus a planned second macOS allocation at the same checked-out implementation SHA: two macOS samples and one Linux sample",
		"the run, job and attempt links", "the pull request head SHA and the actual checked-out SHA, recorded separately",
		"the runner image, architecture and core count, the Go version and the cache context",
		"the literal argv `go test -race -count=20 -cpu=1 -timeout=8m ./internal/sidecar`", "the complete `ok github.com/wedevwork/callsheet/internal/sidecar Ns` package line",
		"require each `N` at or below 390.000 seconds, leaving `480 - N` of at least 90 seconds", "Use package seconds, not devcheck command wall time",
		"Record the command wall, the complete stage, the setup and tail and the whole-job durations separately",
		"require the stage below 900 seconds and the job below 1200 seconds", "the 493.8-second stage and 535.8-second job projections",
		"gh run view <run-id> --repo wedevwork/callsheet --json jobs --jq '.jobs[] | {name, databaseId}'",
		"gh run rerun <run-id> --job <databaseId> --repo wedevwork/callsheet", "without changing workflow YAML",
		"Verify the same checked-out SHA, distinct allocations and attempts, and unchanged setup and workload",
		"Preserve failed attempts", "Missing, cached, skipped, cancelled, failed or timed-out output never qualifies",
		"Record the complete workflow's success and both summaries' success", "No repeat-until-green sampling and no new workflow gate",
		"match run 40412993068, job 121899307717, attempt 2, SHA `dc1522c` and its exact package line first",
		"Pending hosted evidence does not block implementation review once its automated bar passes; until recorded it is pending, not passed",
		"record the first post-merge workflow and its planned macOS repeat with their actual SHA on that pull request",
		"never combine pre-merge and post-merge macOS samples from different checked-out revisions", "The owner closes delivery verification",
		"never an automatic budget increase or a weakened test", "a successful sample above a projection invalidates that projection",
		// Design 12a-3-cursor-worker, slice 3b FP-6: the owner's closure, its
		// recorded package seconds immediately before it, replaces the pending
		// status.
		"Local exact-argv tests cannot prove hosted headroom", sidecarCPU1Timings, sidecarCPU1Closure.closure)
	if strings.Contains(strings.Join(strings.Fields(checklist), " "), "Hosted verification: passed") {
		t.Fatal("the 12a-stress-sidecar-cpu1-budget checklist records pending evidence as passed")
	}
}
