package devcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The literal stress plan is the specification oracle (design 01c, Stress
// execution, extended by design 02's CI plan with ./internal/plane and,
// through its pre-authorized function-binary split, a separate plane
// function step, deduplicated by design 02b: no TestFP6ProcessGroups and
// only the process-boundary plane subtests, then split into three shards by
// design 02c with processgroup's CPU settings as separate invocations, and
// into four by design 05b with ./internal/plane's CPU settings as separate
// invocations of its own shard, into five by design 05b's sidecar
// follow-up with ./internal/sidecar's CPU settings likewise, and into seven
// by design 06a-perf, which isolates the plane and sidecar CPU1 invocations
// in shards of their own while CPU2 and CPU4 stay concurrent in the plane
// and sidecar shards); it is compared against StressShards and StressSteps,
// never derived from them.
const (
	wantStressPackages      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/contract ./internal/adapter ./internal/mcp"
	wantStressPlane1        = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane"
	wantStressPlane2        = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/plane"
	wantStressPlane4        = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/plane"
	wantStressSidecar1      = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar"
	wantStressSidecar2      = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/sidecar"
	wantStressSidecar4      = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/sidecar"
	wantStressPG1           = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"
	wantStressPG2           = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"
	wantStressPG4           = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"
	wantStressFunction      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"
	wantStressPlaneFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function"
	// wantStressNodeFunction is iteration 03's node process-boundary step.
	wantStressNodeFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"
	// wantStressPlan is the flattened StressSteps view in shard/CPU order.
	wantStressPlan = wantStressPackages + "|" + wantStressPlane1 + "|" + wantStressPlane2 + "|" + wantStressPlane4 + "|" +
		wantStressSidecar1 + "|" + wantStressSidecar2 + "|" + wantStressSidecar4 + "|" +
		wantStressPG1 + "|" + wantStressPG2 + "|" + wantStressPG4 + "|" + wantStressFunction + "|" + wantStressPlaneFunction + "|" + wantStressNodeFunction
)

// want03Additions is iteration 03's literal addition to the 02b selection
// (design 03, CI plan): the three node packages completely and the two
// node process-boundary subtests.
var want03Additions = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/client ./internal/sidecar ./internal/contract",
	wantStressNodeFunction,
}

// want04Additions is iteration 04's literal addition (design 04, CI
// plan): the adapter package, complete; no selector or count changes.
var want04Additions = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/adapter",
}

// want07aAdditions is iteration 07a's literal addition (design 07a, CI
// plan): the MCP server package, complete, in the packages shard only; no
// selector or count changes.
var want07aAdditions = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/mcp",
}

// want02bSelection is iteration 02b's literal stress selection, the
// baseline the shards' union must preserve exactly.
var want02bSelection = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/processgroup ./internal/spikes/gittransport ./internal/plane",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function",
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function",
}

// wantStressShards is the literal shard table: name, concurrency and the
// step names with their commands.
var wantStressShards = []struct {
	name     string
	parallel bool
	steps    [][2]string
}{
	{"packages", false, [][2]string{{"stress packages", wantStressPackages}}},
	{"plane-cpu1", true, [][2]string{{"stress plane cpu1", wantStressPlane1}}},
	{"plane", true, [][2]string{{"stress plane cpu2", wantStressPlane2}, {"stress plane cpu4", wantStressPlane4}}},
	{"sidecar-cpu1", true, [][2]string{{"stress sidecar cpu1", wantStressSidecar1}}},
	{"sidecar", true, [][2]string{{"stress sidecar cpu2", wantStressSidecar2}, {"stress sidecar cpu4", wantStressSidecar4}}},
	{"processgroup", true, [][2]string{{"stress processgroup cpu1", wantStressPG1}, {"stress processgroup cpu2", wantStressPG2}, {"stress processgroup cpu4", wantStressPG4}}},
	{"functions", false, [][2]string{{"stress function", wantStressFunction}, {"stress plane function", wantStressPlaneFunction}, {"stress node function", wantStressNodeFunction}}},
}

// Sequential call groups of each stress stage: calls within a group are
// concurrent and compared as a set; the groups are ordered boundaries.
var (
	groupPackages     = []string{wantStressPackages}
	groupPlane1       = []string{wantStressPlane1}
	groupPlane        = []string{wantStressPlane2, wantStressPlane4}
	groupSidecar1     = []string{wantStressSidecar1}
	groupSidecar      = []string{wantStressSidecar2, wantStressSidecar4}
	groupProcessGroup = []string{wantStressPG1, wantStressPG2, wantStressPG4}
	groupFunctions1   = []string{wantStressFunction}
	groupFunctions2   = []string{wantStressPlaneFunction}
	groupFunctions3   = []string{wantStressNodeFunction}
	wantStageGroups   = map[string][][]string{
		"stress": {groupPackages, groupPlane1, groupPlane, groupSidecar1, groupSidecar, groupProcessGroup,
			groupFunctions1, groupFunctions2, groupFunctions3},
		"stress-packages":     {groupPackages},
		"stress-plane-cpu1":   {groupPlane1},
		"stress-plane":        {groupPlane},
		"stress-sidecar-cpu1": {groupSidecar1},
		"stress-sidecar":      {groupSidecar},
		"stress-processgroup": {groupProcessGroup},
		"stress-functions":    {groupFunctions1, groupFunctions2, groupFunctions3},
	}
	// stressShardStages are the seven shard stages in advertised order
	// (design 06a-perf).
	stressShardStages = []string{"stress-packages", "stress-plane-cpu1", "stress-plane", "stress-sidecar-cpu1", "stress-sidecar",
		"stress-processgroup", "stress-functions"}
	// stressStages are "stress" and the seven shard stages.
	stressStages = append([]string{"stress"}, stressShardStages...)
)

// joinedArgv returns the "|"-joined commands of the steps of the shards at
// idx, concatenated in the given order: the command union of split shards
// (design 06a-perf: plane-cpu1 with plane, sidecar-cpu1 with sidecar).
func joinedArgv(shards []StressShard, idx ...int) string {
	var steps []Step
	for _, i := range idx {
		steps = append(steps, shards[i].Steps...)
	}
	return strings.Join(argvOf(steps), "|")
}

// callsMatch compares recorded calls with ordered groups, each group as a
// set.
func callsMatch(got []string, groups [][]string) bool {
	i := 0
	for _, g := range groups {
		if i+len(g) > len(got) {
			return false
		}
		a, b := slices.Clone(got[i:i+len(g)]), slices.Clone(g)
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			return false
		}
		i += len(g)
	}
	return i == len(got)
}

func TestStressPlan(t *testing.T) {
	if StressCount != 20 {
		t.Fatalf("StressCount = %d, the declared repeat count is 20", StressCount)
	}
	for _, goos := range []string{"linux", "darwin"} {
		shards, err := StressShards(goos)
		if err != nil || len(shards) != len(wantStressShards) {
			t.Fatalf("%s: %+v %v", goos, shards, err)
		}
		var flat []string
		for i, want := range wantStressShards {
			sh := shards[i]
			if sh.Name != want.name || sh.Parallel != want.parallel || len(sh.Steps) != len(want.steps) {
				t.Fatalf("%s shard %d = %+v", goos, i, sh)
			}
			for j, ws := range want.steps {
				s := sh.Steps[j]
				if s.Name != ws[0] || strings.Join(s.Argv, " ") != ws[1] || strings.Join(s.Env, " ") != "CGO_ENABLED=1" {
					t.Fatalf("%s %s step %d = %+v", goos, sh.Name, j, s)
				}
				for _, a := range s.Argv {
					if strings.HasPrefix(a, "-bench") || strings.Contains(a, "'") || strings.Contains(a, " ") {
						t.Fatalf("%s %s: unexpected argv element %q", goos, s.Name, a)
					}
				}
				flat = append(flat, ws[1])
			}
		}
		steps, err := StressSteps(goos)
		if err != nil || len(steps) != 13 || strings.Join(argvOf(steps), "|") != wantStressPlan || strings.Join(flat, "|") != wantStressPlan {
			t.Fatalf("%s flattened = %v %v", goos, argvOf(steps), err)
		}
		for i, name := range []string{"stress packages", "stress plane cpu1", "stress plane cpu2", "stress plane cpu4", "stress sidecar cpu1", "stress sidecar cpu2",
			"stress sidecar cpu4", "stress processgroup cpu1", "stress processgroup cpu2", "stress processgroup cpu4", "stress function", "stress plane function",
			"stress node function"} {
			if steps[i].Name != name || strings.Join(steps[i].Env, " ") != "CGO_ENABLED=1" {
				t.Fatalf("%s flattened step %d = %+v", goos, i, steps[i])
			}
		}
	}
	// The plane package (iteration 05b) and the sidecar package (design
	// 05b's sidecar follow-up) are their own shards' packages and are
	// absent from the packages shard; the wave schedule is the inactive
	// default (DW4): no shard is split into waves, so each Parallel shard
	// runs its steps as one concurrent group: the plane and sidecar CPU1
	// singletons (design 06a-perf), the plane and sidecar CPU2/CPU4 pairs
	// and processgroup's three CPU settings. Design 05b's plane fallback
	// {{4}, {1, 2}} is superseded: the two-step plane shard rejects it.
	if stressPlanePackage != "./internal/plane" || slices.Contains(stressPackages, "./internal/plane") {
		t.Fatalf("plane package %q, packages shard %v", stressPlanePackage, stressPackages)
	}
	if stressSidecarPackage != "./internal/sidecar" || slices.Contains(stressPackages, "./internal/sidecar") {
		t.Fatalf("sidecar package %q, packages shard %v", stressSidecarPackage, stressPackages)
	}
	if len(stressWaves) != 0 {
		t.Fatalf("stressWaves = %v, want the empty default (one concurrent wave per Parallel shard)", stressWaves)
	}
	for _, c := range []struct {
		stage string
		size  int
	}{{"stress-plane-cpu1", 1}, {"stress-plane", 2}, {"stress-sidecar-cpu1", 1}, {"stress-sidecar", 2}, {"stress-processgroup", 3}} {
		plan, err := stressPlan("linux", c.stage)
		if err != nil || len(plan) != 1 || !plan[0].Parallel || stressStagePrefix+plan[0].Name != c.stage {
			t.Fatalf("%s plan = %+v %v", c.stage, plan, err)
		}
		if g, err := stressGroups(plan[0], stressWaves[plan[0].Name]); err != nil || len(g) != 1 || len(g[0]) != c.size {
			t.Fatalf("%s groups = %v %v", c.stage, g, err)
		}
	}
	// Returned plans are independent: mutating one never changes the next,
	// including nested step, argv and env slices.
	a, _ := StressShards("linux")
	a[0].Name = "mutated"
	a[1].Parallel = false
	a[1].Steps[0].Argv[3] = "-count=1"
	a[2].Steps[1].Env[0] = "CGO_ENABLED=0"
	a[2].Steps = append(a[2].Steps, a[1].Steps[0])
	a[3].Parallel = false
	a[3].Steps[0].Argv[4] = "-cpu=2"
	a[4].Steps[0].Argv[3] = "-count=1"
	a[5].Steps[1].Argv[4] = "-cpu=1"
	a[6].Steps[1].Argv[6] = "-run=^(TestPlaneState)$"
	a[6].Steps = a[6].Steps[:1]
	a[0].Steps[0].Name = "mutated"
	f, _ := StressSteps("linux")
	f[0].Argv[4] = "-cpu=1"
	f[5].Argv[4] = "-cpu=4"
	f[11].Env[0] = "CGO_ENABLED=0"
	b, _ := StressShards("linux")
	c, _ := StressSteps("linux")
	if b[0].Name != "packages" || !b[1].Parallel || len(b[1].Steps) != 1 || len(b[2].Steps) != 2 || !b[3].Parallel || len(b[6].Steps) != 3 ||
		b[0].Steps[0].Name != "stress packages" ||
		strings.Join(b[1].Steps[0].Argv, " ") != wantStressPlane1 || b[2].Steps[1].Env[0] != "CGO_ENABLED=1" ||
		strings.Join(b[3].Steps[0].Argv, " ") != wantStressSidecar1 || strings.Join(b[4].Steps[0].Argv, " ") != wantStressSidecar2 ||
		strings.Join(b[5].Steps[1].Argv, " ") != wantStressPG2 ||
		strings.Join(b[6].Steps[1].Argv, " ") != wantStressPlaneFunction || strings.Join(argvOf(c), "|") != wantStressPlan || c[11].Env[0] != "CGO_ENABLED=1" {
		t.Fatalf("plan state leaked: %+v", b)
	}
	for _, goos := range []string{"windows", "freebsd", "plan9", "", "Linux"} {
		shards, err := StressShards(goos)
		if err == nil || shards != nil || !strings.Contains(err.Error(), "stress stage is unsupported on") {
			t.Fatalf("StressShards(%q) = %v %v", goos, shards, err)
		}
		steps, err := StressSteps(goos)
		if err == nil || steps != nil || !strings.Contains(err.Error(), "stress stage is unsupported on") {
			t.Fatalf("StressSteps(%q) = %v %v", goos, steps, err)
		}
	}
	if _, err := stressPlan("linux", "stress-bogus"); err == nil {
		t.Fatal("unknown shard stage planned")
	}
}

// selection is one normalized repetition unit: a package, its -run
// selector ("" for the complete package), one CPU setting and the count.
type selection struct{ pkg, selector, cpu, count string }

// normalize expands one argv display into its selection tuples and checks
// the fixed race/timeout flags. It is written independently of stress.go.
func normalize(t *testing.T, argv string) []selection {
	t.Helper()
	f := strings.Fields(argv)
	if len(f) < 7 || f[0] != "go" || f[1] != "test" || f[2] != "-race" || !slices.Contains(f, "-timeout=6m") {
		t.Fatalf("not a race go test with the 6m timeout: %s", argv)
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
		case a == "-timeout=6m":
		case strings.HasPrefix(a, "./"):
			pkgs = append(pkgs, a)
		default:
			t.Fatalf("unexpected argument %q in %s", a, argv)
		}
	}
	var out []selection
	for _, p := range pkgs {
		for _, c := range cpus {
			out = append(out, selection{p, sel, c, count})
		}
	}
	return out
}

// TestStressShardUnion (UT-1) proves the shards' union is exactly the 02b
// selection plus the literal additions of iterations 03 and 04: every
// (package, selector, CPU, count) tuple appears exactly once, the shards
// are disjoint, nothing of 02b is lost, and each tuple is in its shard
// (design 05b: ./internal/plane only in the plane shards; its sidecar
// follow-up: ./internal/sidecar only in the sidecar shards; design
// 06a-perf: CPU1 of each only in plane-cpu1 and sidecar-cpu1, CPU2 and
// CPU4 only in plane and sidecar).
func TestStressShardUnion(t *testing.T) {
	want := map[selection]int{}
	for _, argv := range want02bSelection {
		for _, s := range normalize(t, argv) {
			want[s]++
		}
	}
	if len(want) != (5+2)*3 {
		t.Fatalf("02b baseline has %d tuples", len(want))
	}
	for _, argv := range want03Additions {
		for _, s := range normalize(t, argv) {
			if want[s] != 0 {
				t.Fatalf("03 addition %v overlaps 02b", s)
			}
			want[s]++
		}
	}
	if len(want) != (5+2+3+1)*3 {
		t.Fatalf("03 selection has %d tuples", len(want))
	}
	for _, argv := range want04Additions {
		for _, s := range normalize(t, argv) {
			if want[s] != 0 {
				t.Fatalf("04 addition %v overlaps", s)
			}
			want[s]++
		}
	}
	if len(want) != (5+2+3+1+1)*3 {
		t.Fatalf("04 selection has %d tuples", len(want))
	}
	for _, argv := range want07aAdditions {
		for _, s := range normalize(t, argv) {
			if want[s] != 0 {
				t.Fatalf("07a addition %v overlaps", s)
			}
			want[s]++
		}
	}
	if len(want) != (5+2+3+1+1+1)*3 {
		t.Fatalf("07a selection has %d tuples", len(want))
	}
	for _, goos := range []string{"linux", "darwin"} {
		shards, err := StressShards(goos)
		if err != nil {
			t.Fatal(err)
		}
		got := map[selection]int{}
		owner := map[selection]string{}
		for _, sh := range shards {
			for _, st := range sh.Steps {
				for _, s := range normalize(t, strings.Join(st.Argv, " ")) {
					if prev, dup := owner[s]; dup {
						t.Fatalf("%s: %v selected by both %s and %s", goos, s, prev, sh.Name)
					}
					owner[s] = sh.Name
					got[s]++
				}
			}
		}
		for s, n := range want {
			if got[s] != n {
				t.Errorf("%s: %v selected %d times, want %d", goos, s, got[s], n)
			}
		}
		for s := range got {
			if want[s] == 0 {
				t.Errorf("%s: %v is not in the 02b, 03, 04 or 07a selection", goos, s)
			}
		}
		for s, sh := range owner {
			want := "packages"
			switch {
			case s.selector != "":
				want = "functions"
			case s.pkg == "./internal/spikes/processgroup":
				want = "processgroup"
			case s.pkg == "./internal/plane" && s.cpu == "1":
				want = "plane-cpu1"
			case s.pkg == "./internal/plane":
				want = "plane"
			case s.pkg == "./internal/sidecar" && s.cpu == "1":
				want = "sidecar-cpu1"
			case s.pkg == "./internal/sidecar":
				want = "sidecar"
			}
			if sh != want {
				t.Errorf("%s: %v placed in shard %s, want %s", goos, s, sh, want)
			}
		}
		for _, c := range []struct{ pkg, cpu, shard string }{
			{"./internal/plane", "1", "plane-cpu1"}, {"./internal/plane", "2", "plane"}, {"./internal/plane", "4", "plane"},
			{"./internal/sidecar", "1", "sidecar-cpu1"}, {"./internal/sidecar", "2", "sidecar"}, {"./internal/sidecar", "4", "sidecar"},
		} {
			if s := (selection{c.pkg, "", c.cpu, "20"}); got[s] != 1 || owner[s] != c.shard {
				t.Errorf("%s: %s at -cpu=%s selected %d times in %q, want once in %s", goos, c.pkg, c.cpu, got[s], owner[s], c.shard)
			}
		}
		// Each isolated or paired shard holds precisely its own tuples.
		held := map[string][]string{}
		for s, sh := range owner {
			if s.pkg == "./internal/plane" || s.pkg == "./internal/sidecar" {
				held[sh] = append(held[sh], s.cpu)
			}
		}
		for sh, want := range map[string]string{"plane-cpu1": "1", "plane": "2 4", "sidecar-cpu1": "1", "sidecar": "2 4"} {
			slices.Sort(held[sh])
			if strings.Join(held[sh], " ") != want {
				t.Errorf("%s: shard %s holds CPU settings %v, want %s", goos, sh, held[sh], want)
			}
		}
	}
}

// splitRun mirrors the testing package's -run splitting: a pattern splits
// into per-level patterns only at a "/" outside parentheses and brackets.
// It is written independently of StressSteps.
func splitRun(pattern string) []string {
	var levels []string
	var cs, cp int
	start := 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '[':
			cs++
		case ']':
			if cs--; cs < 0 {
				cs = 0
			}
		case '(':
			if cs == 0 {
				cp++
			}
		case ')':
			if cs == 0 {
				cp--
			}
		case '\\':
			i++
		case '/':
			if cs == 0 && cp == 0 {
				levels = append(levels, pattern[start:i])
				start = i + 1
			}
		}
	}
	return append(levels, pattern[start:])
}

// runValue returns the -run value of a planned step ("" if none).
func runValue(s Step) string {
	for _, a := range s.Argv {
		if v, ok := strings.CutPrefix(a, "-run="); ok {
			return v
		}
	}
	return ""
}

// TestStressSelectorComponents (UT-1) checks the selectors' per-level
// semantics: every selected name matches its level, and every excluded
// name, near-prefix and near-suffix negatives and the contracts boundaries
// included, does not.
func TestStressSelectorComponents(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := StressSteps(goos)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps[:10] {
			if runValue(s) != "" {
				t.Fatalf("%s: %s has a selector: %v", goos, s.Name, s.Argv)
			}
		}
		fn, plane, nodes := splitRun(runValue(steps[10])), splitRun(runValue(steps[11])), splitRun(runValue(steps[12]))
		if len(fn) != 1 || len(plane) != 2 || len(nodes) != 2 {
			t.Fatalf("%s: levels function=%q plane=%q node=%q", goos, fn, plane, nodes)
		}
		for _, c := range []struct {
			level    string
			pattern  string
			selected []string
			excluded []string
		}{
			{"function", fn[0],
				[]string{"TestFP4TransportHarness", "TestFP5GitRoundTrip"},
				[]string{"TestFP6ProcessGroups", "TestFP4TransportHarnessX", "XTestFP5GitRoundTrip", "TestFP4", "TestFP1Cli", "TestPlaneState", "TestCIWorkflowContract", ""}},
			{"plane parents", plane[0],
				[]string{"TestPlaneState", "TestPlaneTLS", "TestPlaneReissue"},
				[]string{"TestPlaneStatus", "TestPlaneStateX", "TestPlaneTLSX", "XTestPlaneTLS", "TestPlaneReissueFailure", "TestPlaneInit", "TestPlaneBind",
					"TestPlaneCommands", "TestPlanePlatform", "TestFP6ProcessGroups", "TestPlane", ""}},
			{"plane subtests", plane[1],
				[]string{"paths", "persistence", "locking", "validation", "https-only", "prelisten-validation", "bounded-shutdown", "process"},
				[]string{"contracts", "processes", "subprocess", "path", "pathsx", "lock", "https", "https-only-x", "prelisten", "bounded-shutdown#01", "validation2",
					"issuance", "fingerprint", "restart-invariance", "inspection", "expiry-warnings", "cooperative", "resistant", "leader-exits-first", ""}},
			{"node parents", nodes[0],
				[]string{"TestNodeEnrollment", "TestNodeReconnect"},
				[]string{"TestNodeTrust", "TestNodeProtocol", "TestNodeLease", "TestNodeRegistry", "TestNodeDiscovery", "TestNodePlatform", "TestNodeEnrollmentX", "XTestNodeReconnect", "TestNode", "TestPlaneState", ""}},
			{"node subtests", nodes[1],
				[]string{"locking", "shutdown"},
				[]string{"restart", "disconnect", "identity", "recovery", "lock", "shutdown2", "locking#01", "expiry", "return", "contracts", ""}},
		} {
			re, err := regexp.Compile(c.pattern)
			if err != nil {
				t.Fatalf("%s %s: %v", goos, c.level, err)
			}
			for _, n := range c.selected {
				if !re.MatchString(n) {
					t.Errorf("%s %s %q does not select %q", goos, c.level, c.pattern, n)
				}
			}
			for _, n := range c.excluded {
				if re.MatchString(n) {
					t.Errorf("%s %s %q selects excluded %q", goos, c.level, c.pattern, n)
				}
			}
		}
	}
	// The splitter itself: an unparenthesized slash separates levels, so the
	// plane selector is one two-level pattern, never a flat alternation; a
	// slash inside a group or class, or escaped, does not split.
	if got := splitRun("^(A|B)$/^(x|y)$"); len(got) != 2 || got[0] != "^(A|B)$" || got[1] != "^(x|y)$" {
		t.Fatalf("splitRun = %q", got)
	}
	if got := splitRun("^(a/b|[/])$"); len(got) != 1 {
		t.Fatalf("slash inside groups split: %q", got)
	}
	if got := splitRun(`^a\/b$`); len(got) != 1 {
		t.Fatalf("escaped slash split: %q", got)
	}
}

func TestStressStageDispatch(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, stage := range stressStages {
			f := &fakeRunner{}
			code, out, errOut := runDriver(t, goos, f, stage)
			if code != 0 || !strings.Contains(out, "stage "+stage+" ok") {
				t.Fatalf("%s %s = %d %s", goos, stage, code, errOut)
			}
			if !callsMatch(f.argvs(), wantStageGroups[stage]) {
				t.Fatalf("%s %s calls = %q", goos, stage, f.argvs())
			}
			for _, c := range f.calls {
				if c.dir != "" || !strings.Contains(strings.Join(c.env, "\n"), "PATH=") {
					t.Fatalf("%s: child must run in the caller's cwd with the parent environment", goos)
				}
				if !strings.HasSuffix(strings.Join(c.env, "\n"), "CGO_ENABLED=1") {
					t.Fatalf("%s: CGO_ENABLED=1 must override the parent environment", goos)
				}
			}
			for _, g := range wantStageGroups[stage] {
				for _, argv := range g {
					if !strings.Contains(out, ": "+argv+"\n") && !strings.Contains(out, ": "+argv+" (concurrent, log ") {
						t.Fatalf("%s %s: command %s not logged: %s", goos, stage, argv, out)
					}
				}
			}
			if _, err := os.Stat(scratchFrom(out)); !os.IsNotExist(err) {
				t.Fatalf("%s %s: successful scratch not deleted", goos, stage)
			}
		}
	}
	// all stays test, coverage, bench, cross: stress is explicit.
	f := &fakeRunner{coverTotal: "81%", cmdList: cmdList, profile: goodProfile}
	// test(2) + coverage(3) + bench(6, iteration 07a) + cross(12).
	if code, _, errOut := runDriver(t, "linux", f, "all"); code != 0 || len(f.calls) != 23 {
		t.Fatalf("all = %d with %d calls %s", code, len(f.calls), errOut)
	}
	for _, c := range f.argvs() {
		if strings.Contains(c, "-count=20") || strings.Contains(c, "-cpu=") {
			t.Fatalf("all ran a stress command: %s", c)
		}
	}
	// No operands, count, CPU or output flags, for every stress stage.
	for _, stage := range stressStages {
		for _, extra := range [][]string{{"extra"}, {"-o", "x"}, {"-count=1"}, {"-cpu=1"}, {"-race"}, {"packages"}} {
			f := &fakeRunner{}
			args := append([]string{stage}, extra...)
			if code, out, errOut := runDriver(t, "linux", f, args...); code != 2 || !strings.Contains(errOut, "invalid arguments for "+stage) || len(f.calls) != 0 || out != "" {
				t.Fatalf("%v = %d %s", args, code, errOut)
			}
		}
	}
	// Genuinely unknown stages, the near neighbours of the plane and
	// sidecar stages (design 06a-perf: only CPU1 has a stage of its own)
	// included.
	for _, bogus := range []string{"stress-planes", "stress-plane-cpu2", "stress-plane-cpu4", "stress-plane-cpu", "stress-plane-cpu1x", "stress-plane-cpu12",
		"stress-sidecars", "stress-sidecar-cpu4", "stress-sidecar-cpu2", "stress-sidecar-cpu", "stress-cpu1", "stress-processgroup-cpu1", "stress-package", "stress-",
		"stress-functions2"} {
		if code, _, errOut := runDriver(t, "linux", &fakeRunner{}, bogus); code != 2 || !strings.Contains(errOut, "unknown subcommand") {
			t.Fatalf("%s = %d %s", bogus, code, errOut)
		}
	}
}

func TestStressFailFastRetainsLogs(t *testing.T) {
	for _, c := range []struct {
		stage, fail, failed string
		calls               int
	}{
		{"stress", "./internal/testkit", "stress packages", 1},
		{"stress", "-cpu=1 -timeout=6m ./internal/plane", "stress plane cpu1", 2},
		{"stress", "-cpu=2 -timeout=6m ./internal/plane", "stress plane cpu2", 4},
		{"stress", "-cpu=1 -timeout=6m ./internal/sidecar", "stress sidecar cpu1", 5},
		{"stress", "-cpu=2 -timeout=6m ./internal/sidecar", "stress sidecar cpu2", 7},
		{"stress", "-cpu=2 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu2", 10},
		{"stress", "TestFP4TransportHarness", "stress function", 11},
		{"stress", "TestPlaneState", "stress plane function", 12},
		{"stress-packages", "./internal/contract", "stress packages", 1},
		{"stress-plane-cpu1", "-cpu=1 ", "stress plane cpu1", 1},
		{"stress-plane", "-cpu=4 ", "stress plane cpu4", 2},
		{"stress-sidecar-cpu1", "-cpu=1 ", "stress sidecar cpu1", 1},
		{"stress-sidecar", "-cpu=4 ", "stress sidecar cpu4", 2},
		{"stress-processgroup", "-cpu=4 ", "stress processgroup cpu4", 3},
		{"stress-functions", "TestFP5GitRoundTrip", "stress function", 1},
		{"stress-functions", "TestPlaneTLS", "stress plane function", 2},
		{"stress", "TestNodeEnrollment", "stress node function", 13},
		{"stress-functions", "TestNodeReconnect", "stress node function", 3},
	} {
		f := &fakeRunner{fail: c.fail}
		code, out, errOut := runDriver(t, "linux", f, c.stage)
		scratch := scratchFrom(out)
		if code != 1 || len(f.calls) != c.calls || !strings.Contains(errOut, "stage "+c.stage+" FAILED: "+c.failed+" failed: go test -race") ||
			!strings.Contains(errOut, "boom from child") || !strings.Contains(errOut, "logs retained in "+scratch) || strings.Contains(out, "stage "+c.stage+" ok") {
			t.Fatalf("%s fail %s = %d calls=%d %s", c.stage, c.fail, code, len(f.calls), errOut)
		}
		log := filepath.Join(scratch, strings.ReplaceAll(c.failed, " ", "-")+".log")
		if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "boom from child") {
			t.Fatalf("failure log %s: %q %v", log, b, err)
		}
		os.RemoveAll(scratch)
	}
}

func TestUnsupportedStressCreatesNoScratch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, stage := range stressStages {
		for _, goos := range []string{"windows", "freebsd", ""} {
			f := &fakeRunner{}
			code, out, errOut := runDriver(t, goos, f, stage)
			if code != 1 || len(f.calls) != 0 || !strings.Contains(errOut, "stage "+stage+" FAILED") || !strings.Contains(errOut, "unsupported on") {
				t.Fatalf("%q %s = %d calls=%d %s", goos, stage, code, len(f.calls), errOut)
			}
			if strings.Contains(errOut, "logs retained") || strings.Contains(out, "devcheck: scratch") {
				t.Fatalf("%q %s claims a scratch dir: out=%q err=%q", goos, stage, out, errOut)
			}
			if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
				t.Fatalf("%q %s created %v", goos, stage, entries)
			}
		}
	}
}

// selectorOutputRunner writes out to the stdout of every call whose argv
// contains match.
type selectorOutputRunner struct {
	match string
	out   []string
}

func (r *selectorOutputRunner) run(_ context.Context, argv, _ []string, _ string, stdout, _ io.Writer) error {
	if strings.Contains(strings.Join(argv, " "), r.match) {
		for _, s := range r.out {
			io.WriteString(stdout, s)
		}
	}
	return nil
}

// TestStressNoTestsGuard (DS1): a selector step whose go test reports no
// tests to run fails, however the report is split across writes.
func TestStressNoTestsGuard(t *testing.T) {
	for _, c := range []struct {
		match, failed string
		out           []string
	}{
		{"TestFP4TransportHarness", "stress function", []string{"ok  \tgithub.com/wedevwork/callsheet/tests/function\t0.1s [no tests to run]\n"}},
		{"TestPlaneState", "stress plane function", []string{"testing: warning: no tes", "ts to run\nPASS\n"}},
		{"TestPlaneState", "stress plane function", []string{"no tests ", "t", "o run"}},
	} {
		r := &selectorOutputRunner{match: c.match, out: c.out}
		code, out, errOut := runDriver(t, "darwin", r, "stress-functions")
		if code != 1 || !strings.Contains(errOut, "stage stress-functions FAILED: "+c.failed+" failed") || !strings.Contains(errOut, errNoTests.Error()) {
			t.Fatalf("%q = %d %s", c.out, code, errOut)
		}
		os.RemoveAll(scratchFrom(out))
	}
	// Ordinary output passes.
	r := &selectorOutputRunner{match: "./tests/function", out: []string{"ok  \tgithub.com/wedevwork/callsheet/tests/function\t30.0s\n"}}
	if code, _, errOut := runDriver(t, "linux", r, "stress-functions"); code != 0 {
		t.Fatalf("ordinary output = %d %s", code, errOut)
	}
	g := &noTestsGuard{}
	for _, b := range []byte("xx no tests to ru") {
		g.Write([]byte{b})
	}
	if g.seen() {
		t.Fatal("partial marker detected")
	}
	g.Write([]byte("n"))
	if !g.seen() {
		t.Fatal("marker split into single bytes not detected")
	}
}

// ctxRecorder records each child's context and returns what next says.
type ctxRecorder struct {
	mu   sync.Mutex
	ctxs []context.Context
	next func(ctx context.Context, argv []string) error
}

func (r *ctxRecorder) run(ctx context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
	r.mu.Lock()
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
	if r.next != nil {
		return r.next(ctx, argv)
	}
	return nil
}

func (r *ctxRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ctxs)
}

func stressDriver(t *testing.T, ctx context.Context, run Runner) *driver {
	return &driver{ctx: ctx, run: run, out: io.Discard, errOut: io.Discard, scratch: t.TempDir(), goos: "linux"}
}

func TestStressWatchdog(t *testing.T) {
	shards, _ := StressShards("linux")
	// Every child runs under a watchdog deadline stressWatchdog from now,
	// and the watchdog context is canceled once the stage returns. "stress"
	// shares one watchdog across its shards (DS2): one context for all
	// thirteen.
	r := &ctxRecorder{}
	before := time.Now()
	if err := stressDriver(t, context.Background(), r.run).stress(shards); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(r.ctxs) != 13 {
		t.Fatalf("calls = %d", len(r.ctxs))
	}
	for i, ctx := range r.ctxs {
		dl, ok := ctx.Deadline()
		if !ok || dl.Before(before.Add(stressWatchdog)) || dl.After(after.Add(stressWatchdog)) {
			t.Fatalf("child %d deadline = %v (ok=%v), want %v from start", i, dl, ok, stressWatchdog)
		}
		if ctx != r.ctxs[0] {
			t.Fatalf("child %d does not share the stage's single watchdog", i)
		}
		if ctx.Err() == nil {
			t.Fatalf("child %d context still live after the stage returned", i)
		}
	}
	if stressWatchdog != 15*time.Minute {
		t.Fatalf("watchdog = %v", stressWatchdog)
	}
	// A single shard gets its own watchdog.
	for _, sh := range shards {
		r = &ctxRecorder{}
		before = time.Now()
		if err := stressDriver(t, context.Background(), r.run).stress([]StressShard{sh}); err != nil {
			t.Fatal(err)
		}
		if dl, _ := r.ctxs[0].Deadline(); len(r.ctxs) != len(sh.Steps) || dl.Before(before.Add(stressWatchdog)) {
			t.Fatalf("%s: %d calls, deadline %v", sh.Name, len(r.ctxs), dl)
		}
	}
	// An earlier caller deadline wins, for the concurrent shard too.
	parentDL := time.Now().Add(time.Minute)
	parent, cancel := context.WithDeadline(context.Background(), parentDL)
	defer cancel()
	r = &ctxRecorder{}
	if err := stressDriver(t, parent, r.run).stress(shards); err != nil {
		t.Fatal(err)
	}
	for i, ctx := range r.ctxs {
		if dl, _ := ctx.Deadline(); !dl.Equal(parentDL) {
			t.Fatalf("child %d deadline = %v, want the caller's %v", i, dl, parentDL)
		}
	}
	if parent.Err() != nil {
		t.Fatal("stress canceled its caller's context")
	}
	// A later caller deadline does not extend the watchdog.
	late, cancelLate := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancelLate()
	r = &ctxRecorder{}
	stressDriver(t, late, r.run).stress(shards)
	if dl, _ := r.ctxs[0].Deadline(); dl.After(time.Now().Add(stressWatchdog)) {
		t.Fatalf("deadline %v exceeds the watchdog", dl)
	}
}

func TestStressCancellationAndExpiry(t *testing.T) {
	shards, _ := StressShards("linux")
	// A canceled caller starts no child at all, whichever shard is first.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		shards []StressShard
		first  string
	}{{shards, "stress packages:"}, {shards[1:2], "stress plane-cpu1:"}, {shards[2:3], "stress plane:"}, {shards[3:4], "stress sidecar-cpu1:"},
		{shards[4:5], "stress sidecar:"}, {shards[5:6], "stress processgroup:"}, {shards[6:], "stress function:"}} {
		r := &ctxRecorder{}
		err := stressDriver(t, canceled, r.run).stress(c.shards)
		if !errors.Is(err, context.Canceled) || len(r.ctxs) != 0 || !strings.Contains(err.Error(), "ended before "+c.first) {
			t.Fatalf("canceled = %v after %d calls", err, len(r.ctxs))
		}
	}
	// Caller cancellation during a child reaches the child's context; the
	// child's error fails the stage and the next step never starts.
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	r := &ctxRecorder{next: func(ctx context.Context, _ []string) error {
		cancelParent()
		<-ctx.Done()
		return ctx.Err()
	}}
	err := stressDriver(t, parent, r.run).stress(shards)
	var se *StepError
	if !errors.As(err, &se) || se.Step.Name != "stress packages" || !errors.Is(err, context.Canceled) || len(r.ctxs) != 1 {
		t.Fatalf("propagated cancel = %v after %d calls", err, len(r.ctxs))
	}
	// An expired deadline is a failure even if the child reports success.
	// The deadline expires while the first child runs, under the test's
	// control rather than a wall-clock race.
	expiring := newExpiringCtx()
	r = &ctxRecorder{next: func(ctx context.Context, _ []string) error {
		expiring.expire()
		<-ctx.Done()
		return nil
	}}
	err = stressDriver(t, expiring, r.run).stress(shards)
	if !errors.Is(err, context.DeadlineExceeded) || len(r.ctxs) != 1 || !strings.Contains(err.Error(), "ended during stress packages") {
		t.Fatalf("expired = %v after %d calls", err, len(r.ctxs))
	}
	// Expiry during a concurrent shard: all of its invocations have started
	// (a barrier of the shard's size: one for a CPU1 singleton, two for a
	// CPU2/CPU4 pair, three for processgroup), every Runner returns nil
	// after expiry, the shard still fails and the next shard never starts.
	// plane-cpu1 follows packages in the whole plan, plane follows
	// plane-cpu1, sidecar-cpu1 follows plane, sidecar follows sidecar-cpu1,
	// and processgroup precedes functions.
	for _, c := range []struct {
		name   string
		group  []string
		shards []StressShard
		calls  int
	}{{"plane-cpu1", groupPlane1, shards, 2}, {"plane", groupPlane, shards, 4}, {"sidecar-cpu1", groupSidecar1, shards[1:], 4},
		{"sidecar", groupSidecar, shards[1:], 6}, {"processgroup", groupProcessGroup, shards[5:], 3}} {
		expiring := newExpiringCtx()
		b := newBarrier(len(c.group))
		r := &ctxRecorder{next: func(ctx context.Context, argv []string) error {
			if !slices.Contains(c.group, strings.Join(argv, " ")) {
				return nil
			}
			if err := b.wait(); err != nil {
				return err
			}
			expiring.expire()
			<-ctx.Done()
			return nil
		}}
		err := stressDriver(t, expiring, r.run).stress(c.shards)
		if !errors.Is(err, context.DeadlineExceeded) || r.count() != c.calls || !strings.Contains(err.Error(), "stress watchdog (15m0s) ended during stress "+c.name+":") {
			t.Fatalf("expired %s = %v after %d calls", c.name, err, r.count())
		}
	}
	// Through dispatch: no success message after an expired watchdog.
	expiring = newExpiringCtx()
	r = &ctxRecorder{next: func(ctx context.Context, _ []string) error {
		expiring.expire()
		<-ctx.Done()
		return nil
	}}
	var out, errOut bytes.Buffer
	code := runFor(expiring, "darwin", []string{"stress"}, &out, &errOut, r.run)
	if code != 1 || len(r.ctxs) != 1 || strings.Contains(out.String(), "stage stress ok") ||
		!strings.Contains(errOut.String(), "stage stress FAILED: devcheck: stress watchdog (15m0s) ended during stress packages: context deadline exceeded") {
		t.Fatalf("dispatch expired = %d after %d calls: %s", code, len(r.ctxs), errOut.String())
	}
	os.RemoveAll(scratchFrom(out.String()))
}

// expiringCtx is a parent context whose deadline the test expires on demand;
// derived contexts are then done with context.DeadlineExceeded.
type expiringCtx struct {
	context.Context
	once sync.Once
	done chan struct{}
}

func newExpiringCtx() *expiringCtx {
	return &expiringCtx{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringCtx) expire()               { c.once.Do(func() { close(c.done) }) }
func (c *expiringCtx) Done() <-chan struct{} { return c.done }
func (c *expiringCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// countdownCtx reports no error for its first n Err calls and expires
// (DeadlineExceeded, Done closed) on the next: it expires between two of
// the coordinator's launch checks, deterministically.
type countdownCtx struct {
	context.Context
	mu   sync.Mutex
	n    int
	once sync.Once
	done chan struct{}
}

func newCountdownCtx(n int) *countdownCtx {
	return &countdownCtx{Context: context.Background(), n: n, done: make(chan struct{})}
}

func (c *countdownCtx) Done() <-chan struct{} { return c.done }
func (c *countdownCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n > 0 {
		c.n--
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.DeadlineExceeded
}

// barrier releases its waiters once n have arrived; a wait that is not
// released within the hang guard fails instead of blocking forever. It is
// a deadlock guard, never a timing oracle: correct code releases at once.
type barrier struct {
	mu      sync.Mutex
	n       int
	arrived int
	release chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, release: make(chan struct{})} }

func (b *barrier) wait() error {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("barrier not released: the invocations did not overlap")
	}
}

// memLogs is an in-memory stressLogs with injectable failures.
type memLogs struct {
	mu        sync.Mutex
	files     map[string]*memFile
	createErr map[string]error
	writeErr  map[string]error
	closeErr  map[string]error
	openErr   map[string]error
	readErr   map[string]error
	// writeErr fails, and shortWrite shortens by one byte without an
	// error, only the first write to a log; later writes succeed, so a
	// failure seen by a later write can only come from syncLog's sticky
	// error.
	shortWrite map[string]bool
	// readCloseErr is returned by the replay reader's Close.
	readCloseErr map[string]error
}

type memFile struct {
	logs    *memLogs
	name    string
	buf     bytes.Buffer
	closed  bool
	written bool // a write has been attempted
}

func (m *memLogs) Create(name string) (io.WriteCloser, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.createErr[name]; err != nil {
		return nil, "mem:" + name, err
	}
	if m.files == nil {
		m.files = map[string]*memFile{}
	}
	f := &memFile{logs: m, name: name}
	m.files["mem:"+name] = f
	return f, "mem:" + name, nil
}

func (f *memFile) Write(p []byte) (int, error) {
	f.logs.mu.Lock()
	defer f.logs.mu.Unlock()
	first := !f.written
	f.written = true
	if err := f.logs.writeErr[f.name]; err != nil && first {
		return 0, err
	}
	if f.logs.shortWrite[f.name] && first && len(p) > 0 {
		return f.buf.Write(p[:len(p)-1])
	}
	return f.buf.Write(p)
}

func (f *memFile) Close() error {
	f.logs.mu.Lock()
	defer f.logs.mu.Unlock()
	f.closed = true
	return f.logs.closeErr[f.name]
}

type memReader struct {
	io.Reader
	err      error
	closeErr error
}

func (r *memReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	return r.Reader.Read(p)
}
func (r *memReader) Close() error { return r.closeErr }

func (m *memLogs) Open(path string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := strings.TrimPrefix(path, "mem:")
	if err := m.openErr[name]; err != nil {
		return nil, err
	}
	f := m.files[path]
	return &memReader{Reader: bytes.NewReader(f.buf.Bytes()), err: m.readErr[name], closeErr: m.readCloseErr[name]}, nil
}

func (m *memLogs) allClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.files {
		if !f.closed {
			return false
		}
	}
	return true
}

// failingWriter fails every write after the first ok ones.
type failingWriter struct {
	ok  int
	buf bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.ok <= 0 {
		return 0, errors.New("output closed")
	}
	w.ok--
	return w.buf.Write(p)
}

// parallelShard is one Parallel shard for the coordinator contract: its
// name, its steps from StressShards and its literal call group.
type parallelShard struct {
	name  string
	steps []Step
	group []string
}

// parallelShards returns the five Parallel shards at indices 1 to 5 of the
// plan (design 06a-perf): the plane CPU1 singleton, the plane CPU2/CPU4 pair
// (iteration 05b), the sidecar CPU1 singleton, the sidecar pair (design
// 05b's sidecar follow-up) and processgroup's three (iteration 02c), of
// sizes 1, 2, 1, 2 and 3, with their literal call groups.
func parallelShards(t *testing.T) []parallelShard {
	t.Helper()
	shards, err := StressShards("linux")
	if err != nil || len(shards) != 7 {
		t.Fatalf("shards = %+v %v", shards, err)
	}
	want := []parallelShard{{"plane-cpu1", nil, groupPlane1}, {"plane", nil, groupPlane}, {"sidecar-cpu1", nil, groupSidecar1},
		{"sidecar", nil, groupSidecar}, {"processgroup", nil, groupProcessGroup}}
	for i := range want {
		sh := shards[i+1]
		if sh.Name != want[i].name || !sh.Parallel || len(sh.Steps) != []int{1, 2, 1, 2, 3}[i] || strings.Join(argvOf(sh.Steps), "|") != strings.Join(want[i].group, "|") {
			t.Fatalf("shard %d = %+v, want %s", i+1, sh, want[i].name)
		}
		want[i].steps = sh.Steps
	}
	return want
}

// processgroupSteps returns the steps of the full three-CPU processgroup
// shard, selected by name.
func processgroupSteps(t *testing.T) []Step {
	t.Helper()
	for _, pc := range parallelShards(t) {
		if pc.name == "processgroup" {
			return pc.steps
		}
	}
	t.Fatal("no processgroup shard")
	return nil
}

// setStressWaves replaces the package's wave schedule for one test and
// restores it afterwards. Tests in this package never run in parallel, so
// the driver reads the schedule only while the test that set it runs.
func setStressWaves(t *testing.T, waves map[string][][]int) {
	t.Helper()
	prev := stressWaves
	stressWaves = waves
	t.Cleanup(func() { stressWaves = prev })
}

// processgroupWaves is a literal two-wave schedule of the full three-CPU
// processgroup shard: CPU 4 alone, then CPU 1 and 2 together. It exercises
// the generic wave mechanism (design 06a-perf moved it from design 05b's
// superseded plane fallback, which the two-step plane shard now rejects);
// no shard's schedule is active.
var processgroupWaves = map[string][][]int{"processgroup": {{4}, {1, 2}}}

// writeResult is one recorded Write outcome.
type writeResult struct {
	n   int
	err error
}

// requireSecondWrites requires exactly the recorded second-write results:
// the byte count, and the error (by errors.Is, or nil).
func requireSecondWrites(t *testing.T, name string, got, want map[string]writeResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: second writes %v, want %v", name, got, want)
	}
	for cmd, w := range want {
		g, ok := got[cmd]
		if !ok || g.n != w.n || (w.err == nil) != (g.err == nil) || (w.err != nil && !errors.Is(g.err, w.err)) {
			t.Fatalf("%s: second write of %s = (%d, %v), want (%d, %v)", name, cmd, g.n, g.err, w.n, w.err)
		}
	}
}

// requireOrder requires every needle in s, in the given order.
func requireOrder(t *testing.T, what, s string, needles ...string) {
	t.Helper()
	pos := -1
	for _, n := range needles {
		i := strings.Index(s, n)
		if i < 0 || i < pos {
			t.Fatalf("%s: %q missing or out of order in:\n%s", what, n, s)
		}
		pos = i
	}
}

// TestStressConcurrencyContract is the FP-2 (iterations 02c, 05b and
// 06a-perf) contract of the concurrent coordinator runConcurrentCPU for the
// five Parallel shards, the plane and sidecar CPU1 singletons, the plane and
// sidecar CPU2/CPU4 pairs and processgroup, and of the static wave schedule
// (stressWaves), executed by name from tests/function
// (TestStressShardExecution). Its subtests overlap, failure, watchdog, logs
// and waves use channel barriers, never sleeps, as their correctness
// oracle. Do not rename or skip them.
func TestStressConcurrencyContract(t *testing.T) {
	t.Run("overlap", func(t *testing.T) {
		var mu sync.Mutex
		for _, pc := range parallelShards(t) {
			active, peak := 0, 0
			var argvs []string
			b := newBarrier(len(pc.steps))
			run := func(_ context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
				mu.Lock()
				active++
				peak = max(peak, active)
				argvs = append(argvs, strings.Join(argv, " "))
				mu.Unlock()
				err := b.wait()
				mu.Lock()
				active--
				mu.Unlock()
				return err
			}
			var out bytes.Buffer
			if err := runConcurrentCPU(context.Background(), run, pc.steps, &memLogs{}, &out); err != nil {
				t.Fatalf("%s: %d overlapping invocations: %v", pc.name, len(pc.steps), err)
			}
			if peak != len(pc.steps) || active != 0 || !callsMatch(argvs, [][]string{pc.group}) {
				t.Fatalf("%s: peak %d, active %d, calls %q", pc.name, peak, active, argvs)
			}
		}
		steps := processgroupSteps(t)
		// The bound: more than three, or none, is rejected before any call.
		calls := 0
		counting := func(context.Context, []string, []string, string, io.Writer, io.Writer) error { calls++; return nil }
		logs := &memLogs{}
		for _, bad := range [][]Step{append(slices.Clone(steps), steps[0]), nil} {
			if err := runConcurrentCPU(context.Background(), counting, bad, logs, io.Discard); err == nil || !strings.Contains(err.Error(), "1 to 3 invocations") {
				t.Fatalf("%d steps: %v", len(bad), err)
			}
		}
		if calls != 0 || len(logs.files) != 0 {
			t.Fatalf("rejected plan made %d calls, %d logs", calls, len(logs.files))
		}
		// Sequential boundaries through dispatch (design 06a-perf, 26
		// events): packages (events 0-1) ends before plane CPU1 starts; the
		// plane CPU1 singleton starts and ends (2-3) before the plane pair
		// starts; both plane pair calls start (4-5, their own barrier of two)
		// before either ends (6-7); the sidecar CPU1 singleton (8-9) and the
		// sidecar pair (starts 10-11, ends 12-13, a separate barrier) follow
		// likewise; all three processgroup calls overlap (a barrier of three,
		// never a released one) and end (14-19) before the functions shard
		// starts, which runs in order (20-25). Each gate is routed by the
		// exact command, so a singleton never waits on a pair's barrier, and
		// no two shards overlap.
		var events []string
		gates := map[string]*barrier{wantStressPlane1: newBarrier(1), wantStressSidecar1: newBarrier(1)}
		for _, g := range [][]string{groupPlane, groupSidecar, groupProcessGroup} {
			b := newBarrier(len(g))
			for _, a := range g {
				gates[a] = b
			}
		}
		rec := func(_ context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
			a := strings.Join(argv, " ")
			mu.Lock()
			events = append(events, "start "+a)
			mu.Unlock()
			var err error
			if b := gates[a]; b != nil {
				err = b.wait()
			}
			mu.Lock()
			events = append(events, "end "+a)
			mu.Unlock()
			return err
		}
		var dout, derr bytes.Buffer
		if code := runFor(context.Background(), "darwin", []string{"stress"}, &dout, &derr, rec); code != 0 {
			t.Fatalf("stress = %d %s", code, derr.String())
		}
		if len(events) != 26 || events[0] != "start "+wantStressPackages || events[1] != "end "+wantStressPackages ||
			events[2] != "start "+wantStressPlane1 || events[3] != "end "+wantStressPlane1 ||
			events[8] != "start "+wantStressSidecar1 || events[9] != "end "+wantStressSidecar1 ||
			events[20] != "start "+wantStressFunction || events[21] != "end "+wantStressFunction ||
			events[22] != "start "+wantStressPlaneFunction || events[23] != "end "+wantStressPlaneFunction ||
			events[24] != "start "+wantStressNodeFunction || events[25] != "end "+wantStressNodeFunction {
			t.Fatalf("boundaries: %q", events)
		}
		for _, g := range []struct {
			name  string
			from  int
			group []string
		}{{"plane pair", 4, groupPlane}, {"sidecar pair", 10, groupSidecar}, {"processgroup", 14, groupProcessGroup}} {
			n := len(g.group)
			var starts, ends []string
			for _, e := range events[g.from : g.from+n] {
				starts = append(starts, strings.TrimPrefix(e, "start "))
			}
			for _, e := range events[g.from+n : g.from+2*n] {
				ends = append(ends, strings.TrimPrefix(e, "end "))
			}
			if !callsMatch(starts, [][]string{g.group}) || !callsMatch(ends, [][]string{g.group}) {
				t.Fatalf("%s calls did not all start before any ended: %q", g.name, events)
			}
		}
		os.RemoveAll(scratchFrom(dout.String()))
	})

	t.Run("failure", func(t *testing.T) {
		// The failing subsets of each shard size: every nonempty one for the
		// singleton and the pair, and for three each single failure, a split
		// and all.
		subsets := map[int][][]int{1: {{0}}, 2: {{0}, {1}, {0, 1}}, 3: {{0}, {1}, {2}, {0, 2}, {0, 1, 2}}}
		for _, pc := range parallelShards(t) {
			steps := pc.steps
			n := len(steps)
			for _, failing := range subsets[n] {
				fails := map[string]bool{}
				for _, i := range failing {
					fails[steps[i].Name] = true
				}
				b := newBarrier(n)
				failedDone := make(chan struct{})
				var once sync.Once
				var remaining atomic.Int32
				remaining.Store(int32(len(failing)))
				var mu sync.Mutex
				ctxs := map[string]context.Context{}
				run := func(ctx context.Context, argv, _ []string, _ string, stdout, _ io.Writer) error {
					name := ""
					for _, s := range steps {
						if strings.Join(s.Argv, " ") == strings.Join(argv, " ") {
							name = s.Name
						}
					}
					mu.Lock()
					ctxs[name] = ctx
					mu.Unlock()
					if err := b.wait(); err != nil {
						return err
					}
					if fails[name] {
						fmt.Fprintf(stdout, "boom from %s\n", name)
						if remaining.Add(-1) == 0 {
							defer once.Do(func() { close(failedDone) })
						}
						return errors.New("exit status 1")
					}
					// A sibling finishes after every failure returned, and its
					// context is still live: failure cancels nothing.
					<-failedDone
					if ctx.Err() != nil {
						return fmt.Errorf("%s canceled by a sibling failure", name)
					}
					fmt.Fprintf(stdout, "ok from %s\n", name)
					return nil
				}
				var out bytes.Buffer
				err := runConcurrentCPU(context.Background(), run, steps, &memLogs{}, &out)
				if err == nil || len(ctxs) != n {
					t.Fatalf("%s failing %v: %v with %d calls", pc.name, failing, err, len(ctxs))
				}
				msg := err.Error()
				last := -1
				for _, s := range steps {
					i := strings.Index(msg, s.Name+" failed: "+strings.Join(s.Argv, " ")+": exit status 1\nboom from "+s.Name)
					if fails[s.Name] != (i >= 0) {
						t.Fatalf("%s failing %v: %s reported=%v:\n%s", pc.name, failing, s.Name, i >= 0, msg)
					}
					if i >= 0 {
						if i < last {
							t.Fatalf("failures not in plan order:\n%s", msg)
						}
						last = i
					}
					if !fails[s.Name] && !strings.Contains(out.String(), "ok from "+s.Name) {
						t.Fatalf("sibling %s did not finish:\n%s", s.Name, out.String())
					}
				}
				for name, ctx := range ctxs {
					if ctx.Err() != nil {
						t.Fatalf("%s's context was canceled", name)
					}
				}
			}
		}
		// Through dispatch, a failed invocation fails its shard after every
		// invocation of that shard ran (a failed CPU2 still joins CPU4), and
		// the next shard never starts: a plane CPU1 failure stops the stage
		// before the plane pair, a sidecar CPU1 failure before the sidecar
		// pair.
		for _, c := range []struct {
			fail, failed, ok string
			groups           [][]string
		}{
			{"-cpu=1 -timeout=6m ./internal/plane", "stress plane cpu1", "", [][]string{groupPackages, groupPlane1}},
			{"-cpu=2 -timeout=6m ./internal/plane", "stress plane cpu2", "stress plane cpu4", [][]string{groupPackages, groupPlane1, groupPlane}},
			{"-cpu=4 -timeout=6m ./internal/plane", "stress plane cpu4", "stress plane cpu2", [][]string{groupPackages, groupPlane1, groupPlane}},
			{"-cpu=1 -timeout=6m ./internal/sidecar", "stress sidecar cpu1", "", [][]string{groupPackages, groupPlane1, groupPlane, groupSidecar1}},
			{"-cpu=2 -timeout=6m ./internal/sidecar", "stress sidecar cpu2", "stress sidecar cpu4", [][]string{groupPackages, groupPlane1, groupPlane, groupSidecar1, groupSidecar}},
			{"-cpu=1 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu1", "stress processgroup cpu4",
				[][]string{groupPackages, groupPlane1, groupPlane, groupSidecar1, groupSidecar, groupProcessGroup}},
		} {
			f := &fakeRunner{fail: c.fail}
			code, out, errOut := runDriver(t, "linux", f, "stress")
			if code != 1 || !callsMatch(f.argvs(), c.groups) || !strings.Contains(errOut, "stage stress FAILED: "+c.failed+" failed") ||
				strings.Count(errOut, " failed: ") != 1 || (c.ok != "" && !strings.Contains(out, "devcheck: "+c.ok+": ok in ")) {
				t.Fatalf("dispatch %s = %d %q %s", c.fail, code, f.argvs(), errOut)
			}
			os.RemoveAll(scratchFrom(out))
		}
	})

	t.Run("watchdog", func(t *testing.T) {
		for _, pc := range parallelShards(t) {
			steps := pc.steps
			// A context already done starts nothing and creates no log.
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			calls := 0
			logs := &memLogs{}
			err := runConcurrentCPU(canceled, func(context.Context, []string, []string, string, io.Writer, io.Writer) error { calls++; return nil }, steps, logs, io.Discard)
			if !errors.Is(err, context.Canceled) || calls != 0 || len(logs.files) != 0 || !strings.Contains(err.Error(), "ended before "+steps[0].Name) {
				t.Fatalf("%s pre-canceled = %v, %d calls, %d logs", pc.name, err, calls, len(logs.files))
			}
			// Cancellation after every invocation started reaches every call;
			// the coordinator joins them all before returning, and fails even
			// though the Runners other than CPU2's return nil after the
			// cancellation (a CPU1 singleton's only Runner returns nil).
			n := len(steps)
			parent, cancelParent := context.WithCancel(context.Background())
			b := newBarrier(n)
			var returned atomic.Int32
			run := func(ctx context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
				defer returned.Add(1)
				if err := b.wait(); err != nil {
					return err
				}
				cancelParent()
				<-ctx.Done()
				if strings.Contains(strings.Join(argv, " "), "-cpu=2 ") {
					return ctx.Err()
				}
				return nil
			}
			logs = &memLogs{}
			err = runConcurrentCPU(parent, run, steps, logs, io.Discard)
			cancelParent()
			if got := returned.Load(); got != int32(n) {
				t.Fatalf("%s: coordinator returned with %d of %d calls joined", pc.name, got, n)
			}
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "context ended while the concurrent invocations ran") || !logs.allClosed() {
				t.Fatalf("%s canceled during run = %v", pc.name, err)
			}
			for _, s := range steps {
				cpu2 := strings.Contains(strings.Join(s.Argv, " "), "-cpu=2 ")
				if strings.Contains(err.Error(), s.Name+" failed") != cpu2 {
					t.Fatalf("%s canceled during run: %s reported failed=%v:\n%v", pc.name, s.Name, !cpu2, err)
				}
			}
			// Expiry between launches. With two or more invocations the
			// first starts, then the deadline passes: the rest never start,
			// and the first's nil result does not pass. The singleton has no
			// second launch, so its deadline passes between preparation and
			// its only launch: nothing starts and it fails as not started.
			var started []string
			var mu sync.Mutex
			launched := min(n-1, 1)
			ctx := newCountdownCtx(1 + launched)
			logs = &memLogs{}
			err = runConcurrentCPU(ctx, func(ctx context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
				mu.Lock()
				started = append(started, strings.Join(argv, " "))
				mu.Unlock()
				<-ctx.Done()
				return nil
			}, steps, logs, io.Discard)
			if !errors.Is(err, context.DeadlineExceeded) || len(started) != launched || (launched == 1 && started[0] != pc.group[0]) || !logs.allClosed() {
				t.Fatalf("%s expiry during launch = %v, started %q", pc.name, err, started)
			}
			for i := launched; i < n; i++ {
				if !strings.Contains(err.Error(), steps[i].Name+" failed: "+pc.group[i]+": not started: context deadline exceeded") {
					t.Fatalf("%s expiry during launch: %s not reported as not started:\n%v", pc.name, steps[i].Name, err)
				}
			}
			if launched == 1 && strings.Contains(err.Error(), steps[0].Name+" failed") {
				t.Fatalf("%s expiry during launch: the started %s reported failed:\n%v", pc.name, steps[0].Name, err)
			}
			// Expiry after every Runner returned nil still fails: the
			// deadline passes at the check after the join (one check before
			// preparation and one before each launch pass).
			ctx = newCountdownCtx(1 + n)
			err = runConcurrentCPU(ctx, func(context.Context, []string, []string, string, io.Writer, io.Writer) error { return nil }, steps, &memLogs{}, io.Discard)
			if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "failed:") {
				t.Fatalf("%s expiry despite nil results = %v", pc.name, err)
			}
		}
	})

	t.Run("logs", func(t *testing.T) {
		for _, pc := range parallelShards(t) {
			steps := pc.steps
			// Completion order is the reverse of plan order (for example
			// cpu4, cpu2, cpu1); replay is still in plan order and each log
			// holds only its own output, written through both stdout and
			// stderr.
			b := newBarrier(len(steps))
			done := map[string]chan struct{}{}
			for _, s := range steps {
				done[s.Name] = make(chan struct{})
			}
			after := map[string]string{}
			for i := 0; i+1 < len(steps); i++ {
				after[steps[i].Name] = steps[i+1].Name
			}
			name := func(argv []string) string {
				for _, s := range steps {
					if strings.Join(s.Argv, " ") == strings.Join(argv, " ") {
						return s.Name
					}
				}
				return "?"
			}
			run := func(_ context.Context, argv, _ []string, _ string, stdout, stderr io.Writer) error {
				n := name(argv)
				defer close(done[n])
				if err := b.wait(); err != nil {
					return err
				}
				if prev, ok := after[n]; ok {
					<-done[prev]
				}
				for i := 0; i < 50; i++ {
					fmt.Fprintf(stdout, "%s out %d\n", n, i)
					fmt.Fprintf(stderr, "%s err %d\n", n, i)
				}
				return nil
			}
			scratch := t.TempDir()
			var out bytes.Buffer
			if err := runConcurrentCPU(context.Background(), run, steps, dirLogs(scratch), &out); err != nil {
				t.Fatal(err)
			}
			o := out.String()
			pos := -1
			for _, s := range steps {
				path := filepath.Join(scratch, strings.ReplaceAll(s.Name, " ", "-")+".log")
				i := strings.Index(o, "devcheck: "+s.Name+": "+strings.Join(s.Argv, " ")+" (concurrent, log "+path+")\n")
				if i < 0 || i < pos {
					t.Fatalf("announcement of %s missing or out of order:\n%s", s.Name, o)
				}
				pos = i
			}
			for _, s := range steps {
				path := filepath.Join(scratch, strings.ReplaceAll(s.Name, " ", "-")+".log")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				log := string(b)
				for i := 0; i < 50; i++ {
					if !strings.Contains(log, fmt.Sprintf("%s out %d\n", s.Name, i)) || !strings.Contains(log, fmt.Sprintf("%s err %d\n", s.Name, i)) {
						t.Fatalf("%s log lacks line %d", s.Name, i)
					}
				}
				if strings.Count(log, "\n") != 100 {
					t.Fatalf("%s log holds foreign output:\n%s", s.Name, log)
				}
				heading := strings.Index(o, "devcheck: ---- "+s.Name+" log ("+path+") ----\n")
				if heading < pos {
					t.Fatalf("replay of %s missing or out of plan order:\n%s", s.Name, o)
				}
				body := o[heading:]
				outcome := strings.Index(body, "devcheck: "+s.Name+": ok in ")
				if outcome < 0 || !strings.Contains(body[:outcome], log) {
					t.Fatalf("%s replay is not its log followed by its outcome:\n%s", s.Name, o)
				}
				pos = heading + outcome
			}
		}
		steps := processgroupSteps(t)
		// Injected log failures fail the shard even though every child
		// exited zero, naming the command; a failed preparation starts
		// nothing and closes the logs already created. The coordinator is
		// shared by the three Parallel shards, so its fault coverage runs
		// once.
		ok := func(_ context.Context, _, _ []string, _ string, stdout, _ io.Writer) error {
			_, err := io.WriteString(stdout, "output\n")
			return err
		}
		cpu2 := "stress processgroup cpu2"
		for _, c := range []struct {
			name   string
			logs   *memLogs
			out    io.Writer
			want   string
			calls  bool
			closed bool
		}{
			{"create", &memLogs{createErr: map[string]error{cpu2: errors.New("disk full")}}, io.Discard, cpu2 + " failed: " + wantStressPG2 + ": create log mem:" + cpu2 + ": disk full", false, true},
			{"write", &memLogs{writeErr: map[string]error{cpu2: errors.New("disk full")}}, io.Discard, cpu2 + " failed: " + wantStressPG2 + ": write log mem:" + cpu2 + ": disk full", true, true},
			{"close", &memLogs{closeErr: map[string]error{cpu2: errors.New("bad close")}}, io.Discard, cpu2 + " failed: " + wantStressPG2 + ": close log mem:" + cpu2 + ": bad close", true, true},
			{"open", &memLogs{openErr: map[string]error{cpu2: errors.New("vanished")}}, io.Discard, cpu2 + " failed: " + wantStressPG2 + ": read log mem:" + cpu2 + ": vanished", true, true},
			{"read", &memLogs{readErr: map[string]error{cpu2: errors.New("io error")}}, io.Discard, cpu2 + " failed: " + wantStressPG2 + ": replay log mem:" + cpu2 + ": io error", true, true},
			{"output", &memLogs{}, &failingWriter{ok: 3}, "stress processgroup cpu1 failed: " + wantStressPG1 + ": replay log mem:stress processgroup cpu1: output closed", true, true},
		} {
			calls := 0
			var mu sync.Mutex
			run := func(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
				mu.Lock()
				calls++
				mu.Unlock()
				return ok(ctx, argv, env, dir, stdout, stderr)
			}
			err := runConcurrentCPU(context.Background(), run, steps, c.logs, c.out)
			if err == nil || !strings.Contains(err.Error(), c.want) || (calls == 3) != c.calls || (!c.calls && calls != 0) || c.logs.allClosed() != c.closed {
				t.Fatalf("%s: %v after %d calls (closed=%v)", c.name, err, calls, c.logs.allClosed())
			}
		}
		// The rarer log failures, each with its documented outcome: the
		// shard fails although every child exited zero, and the failures
		// appear in plan order, each naming its command.
		cpu1, cpu4 := "stress processgroup cpu1", "stress processgroup cpu4"
		named := func(name, argv, msg string) string { return name + " failed: " + argv + ": " + msg }
		diskFull, badClose := errors.New("disk full"), errors.New("bad close")
		for _, c := range []struct {
			name  string
			logs  *memLogs
			want  []string // in plan order
			calls int
		}{
			// (a) A failed write sticks: the second write of the same
			// invocation is refused with the first error, never retried.
			{"sticky write error", &memLogs{writeErr: map[string]error{cpu1: diskFull, cpu4: diskFull}},
				[]string{named(cpu1, wantStressPG1, "write log mem:"+cpu1+": disk full"), named(cpu4, wantStressPG4, "write log mem:"+cpu4+": disk full")}, 3},
			// (b) A short write without an error is a write failure.
			{"short write", &memLogs{shortWrite: map[string]bool{cpu2: true}},
				[]string{named(cpu2, wantStressPG2, "write log mem:"+cpu2+": short write")}, 3},
			// (c) A prepared log that fails to close after another log's
			// creation failed: nothing starts, and create and close
			// failures are reported together in plan order.
			{"close after failed preparation", &memLogs{createErr: map[string]error{cpu2: diskFull}, closeErr: map[string]error{cpu1: badClose, cpu4: badClose}},
				[]string{named(cpu1, wantStressPG1, "close log mem:"+cpu1+": bad close"), named(cpu2, wantStressPG2, "create log mem:"+cpu2+": disk full"),
					named(cpu4, wantStressPG4, "close log mem:"+cpu4+": bad close")}, 0},
			// (d) The replay reader's Close failing, after a complete
			// replay, fails the invocation.
			{"replay close", &memLogs{readCloseErr: map[string]error{cpu4: badClose}},
				[]string{named(cpu4, wantStressPG4, "close log mem:"+cpu4+" after replay: bad close")}, 3},
		} {
			var mu sync.Mutex
			calls := 0
			// second records (n, err) of every invocation's second write
			// (iteration 03, S1'), keyed by its command.
			second := map[string]writeResult{}
			run := func(_ context.Context, argv, _ []string, _ string, stdout, _ io.Writer) error {
				n := strings.Join(argv, " ")
				mu.Lock()
				calls++
				mu.Unlock()
				io.WriteString(stdout, "first\n")
				k, err := io.WriteString(stdout, "second\n")
				mu.Lock()
				second[n] = writeResult{k, err}
				mu.Unlock()
				return nil // every child exits zero
			}
			var out bytes.Buffer
			err := runConcurrentCPU(context.Background(), run, steps, c.logs, &out)
			if err == nil || calls != c.calls || !c.logs.allClosed() {
				t.Fatalf("%s: %v after %d calls (closed=%v)", c.name, err, calls, c.logs.allClosed())
			}
			msg := err.Error()
			pos := -1
			for _, w := range c.want {
				i := strings.Index(msg, w)
				if i < 0 || i < pos {
					t.Fatalf("%s: %q missing or out of plan order in:\n%s", c.name, w, msg)
				}
				pos = i
			}
			if strings.Count(msg, " failed: ") != len(c.want) {
				t.Fatalf("%s: unexpected failures:\n%s", c.name, msg)
			}
			switch c.name {
			case "sticky write error":
				// The log would accept the second write, yet it returned
				// the first error with no bytes: syncLog remembered it, and
				// that output never reached the log. The unaffected
				// invocation's second write succeeded in full.
				requireSecondWrites(t, c.name, second, map[string]writeResult{
					wantStressPG1: {0, diskFull}, wantStressPG2: {len("second\n"), nil}, wantStressPG4: {0, diskFull}})
				for _, n := range []string{cpu1, cpu4} {
					if b := c.logs.files["mem:"+n].buf.String(); b != "" {
						t.Fatalf("%s log after a failed first write = %q", n, b)
					}
				}
				if b := c.logs.files["mem:"+cpu2].buf.String(); b != "first\nsecond\n" {
					t.Fatalf("%s log = %q", cpu2, b)
				}
			case "short write":
				// Likewise, the second write would succeed; the short first
				// write is remembered, the second returns zero bytes and
				// io.ErrShortWrite, and nothing more is written.
				requireSecondWrites(t, c.name, second, map[string]writeResult{
					wantStressPG1: {len("second\n"), nil}, wantStressPG2: {0, io.ErrShortWrite}, wantStressPG4: {len("second\n"), nil}})
				if b := c.logs.files["mem:"+cpu2].buf.String(); b != "first" {
					t.Fatalf("%s log after a short first write = %q", cpu2, b)
				}
			case "close after failed preparation":
				if out.Len() != 0 || len(second) != 0 {
					t.Fatalf("announced, replayed or wrote without children:\n%s %v", out.String(), second)
				}
			case "replay close":
				requireSecondWrites(t, c.name, second, map[string]writeResult{
					wantStressPG1: {len("second\n"), nil}, wantStressPG2: {len("second\n"), nil}, wantStressPG4: {len("second\n"), nil}})
				// The replay itself completed before the Close failure.
				if !strings.Contains(out.String(), "devcheck: ---- "+cpu4+" log (mem:"+cpu4+") ----\nfirst\nsecond\ndevcheck: "+cpu4+": FAILED after ") {
					t.Fatalf("replay:\n%s", out.String())
				}
			}
		}
		// Scratch: kept with its path on failure, removed after success, for
		// every Parallel shard stage. Each stage owns only the logs of its
		// own steps: the CPU1 stage its CPU1 log, the pair stage its CPU2
		// and CPU4 logs.
		for _, c := range []struct {
			stage, fail, failedLog string
			logs                   []string
		}{
			{"stress-plane-cpu1", "-cpu=1 ", "stress-plane-cpu1.log", []string{"stress-plane-cpu1.log"}},
			{"stress-plane", "-cpu=2 ", "stress-plane-cpu2.log", []string{"stress-plane-cpu2.log", "stress-plane-cpu4.log"}},
			{"stress-sidecar-cpu1", "-cpu=1 ", "stress-sidecar-cpu1.log", []string{"stress-sidecar-cpu1.log"}},
			{"stress-sidecar", "-cpu=2 ", "stress-sidecar-cpu2.log", []string{"stress-sidecar-cpu2.log", "stress-sidecar-cpu4.log"}},
			{"stress-processgroup", "-cpu=2 ", "stress-processgroup-cpu2.log",
				[]string{"stress-processgroup-cpu1.log", "stress-processgroup-cpu2.log", "stress-processgroup-cpu4.log"}},
		} {
			f := &fakeRunner{fail: c.fail}
			code, dout, errOut := runDriver(t, "linux", f, c.stage)
			scratch := scratchFrom(dout)
			if code != 1 || !strings.Contains(errOut, "logs retained in "+scratch) {
				t.Fatalf("%s failure = %d %s", c.stage, code, errOut)
			}
			if b, err := os.ReadFile(filepath.Join(scratch, c.failedLog)); err != nil || !strings.Contains(string(b), "boom from child") {
				t.Fatalf("%s failed log %s: %q %v", c.stage, c.failedLog, b, err)
			}
			entries, _ := os.ReadDir(scratch)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if !slices.Equal(names, c.logs) {
				t.Fatalf("%s logs = %v, want %v", c.stage, names, c.logs)
			}
			os.RemoveAll(scratch)
			code, dout, errOut = runDriver(t, "linux", &fakeRunner{}, c.stage)
			if _, err := os.Stat(scratchFrom(dout)); code != 0 || !os.IsNotExist(err) {
				t.Fatalf("%s success = %d %s, scratch %v", c.stage, code, errOut, err)
			}
		}
		// A scratch that cannot hold the logs starts nothing.
		shards, _ := StressShards("linux")
		for _, sh := range shards[1:6] {
			f := &fakeRunner{}
			d := &driver{ctx: context.Background(), run: f.run, out: io.Discard, errOut: io.Discard, scratch: filepath.Join(t.TempDir(), "missing"), goos: "linux"}
			if err := d.stress([]StressShard{sh}); err == nil || len(f.calls) != 0 || !strings.Contains(err.Error(), "create log") {
				t.Fatalf("%s missing scratch = %v after %d calls", sh.Name, err, len(f.calls))
			}
		}
	})

	// waves is the static wave schedule (iteration 05b, DW4). No schedule is
	// active (stressWaves is empty, pinned by TestStressPlan); this subtest
	// activates a literal schedule for itself only and proves the generic
	// mechanism on the real full three-CPU processgroup shard (design
	// 06a-perf; design 05b's plane fallback is superseded): CPU 4 runs alone
	// and ends before either smaller invocation starts, CPU 1 and 2 overlap
	// with their own barrier, a first-wave failure, log error or
	// cancellation prevents the second wave, cancellation joins started
	// calls and logs replay by wave, while the plane and sidecar singletons
	// and pairs keep their ordinary groups. A schedule for a CPU1 singleton
	// or a CPU2/CPU4 pair is rejected by the unchanged size guard before any
	// scratch or child exists.
	t.Run("waves", func(t *testing.T) {
		shards, _ := StressShards("linux")
		plane1, plane, sidecar1, sidecar, pgShard := shards[1], shards[2], shards[3], shards[4], shards[5]
		if plane1.Name != "plane-cpu1" || plane.Name != "plane" || sidecar1.Name != "sidecar-cpu1" || sidecar.Name != "sidecar" || pgShard.Name != "processgroup" {
			t.Fatalf("shards = %+v", shards)
		}
		// Default: one group of every step, in plan order, for every
		// Parallel shard.
		for _, sh := range shards[1:6] {
			g, err := stressGroups(sh, nil)
			if err != nil || len(g) != 1 || strings.Join(argvOf(g[0]), "|") != strings.Join(argvOf(sh.Steps), "|") {
				t.Fatalf("%s default groups = %v %v", sh.Name, g, err)
			}
		}
		// The schedule partitions exactly processgroup's steps: CPU 4, then
		// 1 and 2.
		g, err := stressGroups(pgShard, processgroupWaves["processgroup"])
		if err != nil || len(g) != 2 || strings.Join(argvOf(g[0]), "|") != wantStressPG4 ||
			strings.Join(argvOf(g[1]), "|") != wantStressPG1+"|"+wantStressPG2 || g[0][0].Name != "stress processgroup cpu4" {
			t.Fatalf("processgroup waves = %v %v", g, err)
		}
		// Invalid schedules are rejected before anything runs: every
		// schedule category on the full three-CPU shard, and the size guard
		// for every shard that is not one step per CPU setting (the packages
		// shard, both CPU1 singletons, both pairs, design 05b's superseded
		// plane fallback and a truncated processgroup).
		for _, c := range []struct {
			sh    StressShard
			waves [][]int
			want  string
		}{
			{pgShard, [][]int{{4}, {1}}, "CPU setting 2 is not scheduled"},
			{pgShard, [][]int{{4, 4}, {1, 2}}, "CPU setting 4 appears twice"},
			{pgShard, [][]int{{3}, {1, 2, 4}}, "CPU setting 3 is not in [1 2 4]"},
			{pgShard, [][]int{{}, {1, 2, 4}}, "a wave runs 1 to 3 invocations, got 0"},
			{pgShard, [][]int{{1, 2, 4, 1}}, "a wave runs 1 to 3 invocations, got 4"},
			{shards[0], [][]int{{1}, {2}, {4}}, "not a Parallel shard of one step per CPU setting"},
			{plane1, [][]int{{1}}, "not a Parallel shard of one step per CPU setting"},
			{sidecar1, [][]int{{1}}, "not a Parallel shard of one step per CPU setting"},
			{plane, [][]int{{4}, {2}}, "not a Parallel shard of one step per CPU setting"},
			{plane, [][]int{{4}, {1, 2}}, "not a Parallel shard of one step per CPU setting"},
			{sidecar, [][]int{{2}, {4}}, "not a Parallel shard of one step per CPU setting"},
			{StressShard{Name: "plane", Parallel: true, Steps: plane.Steps[:2]}, [][]int{{1}, {2}}, "not a Parallel shard of one step per CPU setting"},
			{StressShard{Name: "processgroup", Parallel: true, Steps: pgShard.Steps[:2]}, [][]int{{1}, {2}}, "not a Parallel shard of one step per CPU setting"},
		} {
			if g, err := stressGroups(c.sh, c.waves); err == nil || g != nil || !strings.Contains(err.Error(), "invalid stress waves") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s %v = %v %v", c.sh.Name, c.waves, g, err)
			}
		}
		// Through dispatch, a configured invalid schedule fails the stage
		// before a scratch directory or child exists: a schedule on a
		// singleton or pair, the superseded plane fallback, and an invalid
		// processgroup schedule.
		for _, c := range []struct {
			waves  map[string][][]int
			stages []string
		}{
			{map[string][][]int{"plane": {{4}, {2}}}, []string{"stress", "stress-plane"}},
			{map[string][][]int{"plane": {{4}, {1, 2}}}, []string{"stress", "stress-plane"}},
			{map[string][][]int{"plane-cpu1": {{1}}}, []string{"stress", "stress-plane-cpu1"}},
			{map[string][][]int{"sidecar-cpu1": {{1}}}, []string{"stress", "stress-sidecar-cpu1"}},
			{map[string][][]int{"sidecar": {{2}, {4}}}, []string{"stress", "stress-sidecar"}},
			{map[string][][]int{"processgroup": {{4}, {1}}}, []string{"stress", "stress-processgroup"}},
		} {
			for _, stage := range c.stages {
				setStressWaves(t, c.waves)
				tmp := t.TempDir()
				t.Setenv("TMPDIR", tmp)
				f := &fakeRunner{}
				code, out, errOut := runDriver(t, "linux", f, stage)
				if entries, _ := os.ReadDir(tmp); code != 1 || len(f.calls) != 0 || out != "" || len(entries) != 0 || !strings.Contains(errOut, "stage "+stage+" FAILED: devcheck: invalid stress waves") {
					t.Fatalf("invalid waves %v %s = %d, %d calls, %d entries: %s", c.waves, stage, code, len(f.calls), len(entries), errOut)
				}
			}
		}
		// A schedule for one shard never affects another stage, and the
		// driver rejects an invalid schedule before its first child too.
		setStressWaves(t, map[string][][]int{"plane": {{4}, {2}}})
		f := &fakeRunner{}
		code, out, errOut := runDriver(t, "linux", f, "stress-processgroup")
		if code != 0 || !callsMatch(f.argvs(), [][]string{groupProcessGroup}) {
			t.Fatalf("processgroup with plane waves = %d %q %s", code, f.argvs(), errOut)
		}
		os.RemoveAll(scratchFrom(out))
		f = &fakeRunner{}
		if err := stressDriver(t, context.Background(), f.run).stress(shards); err == nil || len(f.calls) != 0 || !strings.Contains(err.Error(), "invalid stress waves") {
			t.Fatalf("driver with invalid waves = %v after %d calls", err, len(f.calls))
		}

		setStressWaves(t, processgroupWaves)
		// The flattened inspection view is unchanged by the schedule.
		if steps, err := StressSteps("darwin"); err != nil || strings.Join(argvOf(steps), "|") != wantStressPlan {
			t.Fatalf("flattened plan under waves = %v %v", argvOf(steps), err)
		}
		// Full local dispatch: still 26 start/end events. The plane and
		// sidecar singletons and pairs keep their ordinary groups (2-3,
		// 4-7, 8-9, 10-13); processgroup occupies 14-19 as CPU 4's start
		// and end, then the pair's starts and ends; functions 20-25.
		var mu sync.Mutex
		var events []string
		gates := map[string]*barrier{}
		for _, grp := range [][]string{groupPlane, groupSidecar, {wantStressPG1, wantStressPG2}} {
			b := newBarrier(len(grp))
			for _, a := range grp {
				gates[a] = b
			}
		}
		rec := func(_ context.Context, argv, _ []string, _ string, stdout, _ io.Writer) error {
			a := strings.Join(argv, " ")
			mu.Lock()
			events = append(events, "start "+a)
			mu.Unlock()
			fmt.Fprintf(stdout, "output of %s\n", a)
			var err error
			if b := gates[a]; b != nil {
				err = b.wait()
			}
			mu.Lock()
			events = append(events, "end "+a)
			mu.Unlock()
			return err
		}
		var dout, derr bytes.Buffer
		if code := runFor(context.Background(), "darwin", []string{"stress"}, &dout, &derr, rec); code != 0 {
			t.Fatalf("stress with processgroup waves = %d %s", code, derr.String())
		}
		if len(events) != 26 || events[0] != "start "+wantStressPackages || events[1] != "end "+wantStressPackages ||
			events[2] != "start "+wantStressPlane1 || events[3] != "end "+wantStressPlane1 ||
			events[8] != "start "+wantStressSidecar1 || events[9] != "end "+wantStressSidecar1 ||
			events[14] != "start "+wantStressPG4 || events[15] != "end "+wantStressPG4 ||
			events[20] != "start "+wantStressFunction || events[25] != "end "+wantStressNodeFunction {
			t.Fatalf("wave boundaries: %q", events)
		}
		for _, w := range []struct {
			from  int
			group []string
		}{{4, groupPlane}, {10, groupSidecar}, {16, []string{wantStressPG1, wantStressPG2}}} {
			n := len(w.group)
			var starts, ends []string
			for _, e := range events[w.from : w.from+n] {
				starts = append(starts, strings.TrimPrefix(e, "start "))
			}
			for _, e := range events[w.from+n : w.from+2*n] {
				ends = append(ends, strings.TrimPrefix(e, "end "))
			}
			if !callsMatch(starts, [][]string{w.group}) || !callsMatch(ends, [][]string{w.group}) {
				t.Fatalf("group at %d did not all start before any ended: %q", w.from, events)
			}
		}
		// Logs replay by wave: CPU 4's announcement, replay and outcome,
		// then the pair's announcements, then their replays in CPU order.
		o := dout.String()
		scratch := scratchFrom(o)
		log := func(n int) string { return filepath.Join(scratch, fmt.Sprintf("stress-processgroup-cpu%d.log", n)) }
		requireOrder(t, "wave log order", o,
			"devcheck: stress plane cpu1: ",
			"devcheck: stress sidecar cpu4: ",
			"devcheck: stress processgroup cpu4: "+wantStressPG4+" (concurrent, log "+log(4)+")\n",
			"devcheck: ---- stress processgroup cpu4 log ("+log(4)+") ----\noutput of "+wantStressPG4+"\n",
			"devcheck: stress processgroup cpu4: ok in ",
			"devcheck: stress processgroup cpu1: "+wantStressPG1+" (concurrent, log "+log(1)+")\n",
			"devcheck: stress processgroup cpu2: "+wantStressPG2+" (concurrent, log "+log(2)+")\n",
			"devcheck: ---- stress processgroup cpu1 log ("+log(1)+") ----\noutput of "+wantStressPG1+"\n",
			"devcheck: stress processgroup cpu1: ok in ",
			"devcheck: ---- stress processgroup cpu2 log ("+log(2)+") ----\noutput of "+wantStressPG2+"\n",
			"devcheck: stress processgroup cpu2: ok in ",
			"devcheck: stress function: ",
			"devcheck: stage stress ok")
		// The shard stage runs exactly its two waves.
		f = &fakeRunner{}
		code, out, errOut = runDriver(t, "linux", f, "stress-processgroup")
		if code != 0 || !callsMatch(f.argvs(), [][]string{{wantStressPG4}, {wantStressPG1, wantStressPG2}}) {
			t.Fatalf("stress-processgroup with waves = %d %q %s", code, f.argvs(), errOut)
		}
		os.RemoveAll(scratchFrom(out))
		// A first-wave failure suppresses the second wave and every later
		// shard; a second-wave failure still collects its sibling.
		before := [][]string{groupPackages, groupPlane1, groupPlane, groupSidecar1, groupSidecar}
		for _, c := range []struct {
			stage, fail, failed string
			groups              [][]string
			ok                  []string
		}{
			{"stress-processgroup", "-cpu=4 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu4", [][]string{{wantStressPG4}}, nil},
			{"stress", "-cpu=4 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu4", append(slices.Clone(before), []string{wantStressPG4}), nil},
			{"stress", "-cpu=1 -timeout=6m ./internal/spikes/processgroup", "stress processgroup cpu1",
				append(slices.Clone(before), []string{wantStressPG4}, []string{wantStressPG1, wantStressPG2}), []string{"stress processgroup cpu4", "stress processgroup cpu2"}},
		} {
			f := &fakeRunner{fail: c.fail}
			code, out, errOut := runDriver(t, "linux", f, c.stage)
			if code != 1 || !callsMatch(f.argvs(), c.groups) || !strings.Contains(errOut, "stage "+c.stage+" FAILED: "+c.failed+" failed") ||
				strings.Count(errOut, " failed: ") != 1 || !strings.Contains(errOut, "logs retained in ") {
				t.Fatalf("%s failing %s = %d %q %s", c.stage, c.fail, code, f.argvs(), errOut)
			}
			for _, n := range c.ok {
				if !strings.Contains(out, "devcheck: "+n+": ok in ") {
					t.Fatalf("%s did not finish:\n%s", n, out)
				}
			}
			if len(c.ok) == 0 && (strings.Contains(out, "stress processgroup cpu1") || strings.Contains(out, "stress function")) {
				t.Fatalf("second wave or later shard announced after a first-wave failure:\n%s", out)
			}
			os.RemoveAll(scratchFrom(out))
		}
		// A log that cannot be created fails its wave: in the first wave
		// nothing starts at all; in the second, only the first wave ran.
		for _, c := range []struct {
			blocked string
			calls   []string
		}{{"stress-processgroup-cpu4.log", nil}, {"stress-processgroup-cpu2.log", []string{wantStressPG4}}} {
			scratch := t.TempDir()
			if err := os.Mkdir(filepath.Join(scratch, c.blocked), 0o755); err != nil {
				t.Fatal(err)
			}
			f := &fakeRunner{}
			d := &driver{ctx: context.Background(), run: f.run, out: io.Discard, errOut: io.Discard, scratch: scratch, goos: "linux"}
			if err := d.stress([]StressShard{pgShard}); err == nil || !strings.Contains(err.Error(), "create log") || !slices.Equal(f.argvs(), c.calls) {
				t.Fatalf("blocked %s = %v, calls %q", c.blocked, err, f.argvs())
			}
		}
		// A replay failure in the first wave fails it although its child
		// exited zero, and the second wave never starts.
		f = &fakeRunner{}
		d := &driver{ctx: context.Background(), run: f.run, out: &failingWriter{ok: 1}, errOut: io.Discard, scratch: t.TempDir(), goos: "linux"}
		if err := d.stress([]StressShard{pgShard}); err == nil || !strings.Contains(err.Error(), "stress processgroup cpu4 failed: "+wantStressPG4+": replay log") ||
			!slices.Equal(f.argvs(), []string{wantStressPG4}) {
			t.Fatalf("first-wave replay failure = %v, calls %q", err, f.argvs())
		}
		// Cancellation during the first wave joins it and starts no second
		// wave; during the second it reaches both calls, joins both and
		// fails although they return nil. The next shard never starts.
		functions := shards[6]
		for _, c := range []struct {
			cancelOn string
			wave     string
			calls    int
		}{{wantStressPG4, "wave 1", 1}, {wantStressPG2, "wave 2", 3}} {
			parent, cancelParent := context.WithCancel(context.Background())
			b := newBarrier(2)
			var returned atomic.Int32
			r := &ctxRecorder{next: func(ctx context.Context, argv []string) error {
				defer returned.Add(1)
				a := strings.Join(argv, " ")
				if a == wantStressPG1 || a == wantStressPG2 {
					if err := b.wait(); err != nil {
						return err
					}
				}
				if a == c.cancelOn {
					cancelParent()
				}
				if a == c.cancelOn || (c.wave == "wave 2" && a == wantStressPG1) {
					<-ctx.Done()
				}
				return nil
			}}
			err := stressDriver(t, parent, r.run).stress([]StressShard{pgShard, functions})
			cancelParent()
			if !errors.Is(err, context.Canceled) || r.count() != c.calls || returned.Load() != int32(c.calls) ||
				!strings.Contains(err.Error(), "stress watchdog (15m0s) ended during stress processgroup "+c.wave) || strings.Contains(err.Error(), "function") {
				t.Fatalf("canceled in %s = %v after %d calls, %d returned", c.wave, err, r.count(), returned.Load())
			}
		}
		// A context already done starts no wave.
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		r := &ctxRecorder{}
		if err := stressDriver(t, canceled, r.run).stress([]StressShard{pgShard}); !errors.Is(err, context.Canceled) || r.count() != 0 ||
			!strings.Contains(err.Error(), "ended before stress processgroup wave 1") {
			t.Fatalf("pre-canceled waves = %v after %d calls", err, r.count())
		}
	})
}

// TestPlatformSeamContract is the FP-2 contract test for devcheck, executed
// by name from tests/function (TestHardeningPlatformSeams). Its linux and
// darwin subtests exercise every OS-dependent plan and the runFor dispatch
// on any host; unsupported values are rejected. Do not rename or skip.
func TestPlatformSeamContract(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			test := TestSteps(goos)
			wantTest := map[string]int{"linux": 2, "darwin": 1}[goos]
			if len(test) != wantTest {
				t.Fatalf("TestSteps(%s) = %d steps", goos, len(test))
			}
			native, nerr := NativeSteps(goos)
			if (goos == "darwin") != (nerr == nil) || (goos == "darwin" && len(native) != 1) {
				t.Fatalf("NativeSteps(%s) = %v %v", goos, native, nerr)
			}
			stress, serr := StressSteps(goos)
			shards, sherr := StressShards(goos)
			if serr != nil || len(stress) != 13 || sherr != nil || len(shards) != 7 {
				t.Fatalf("StressSteps(%s) = %v %v, shards %v %v", goos, stress, serr, shards, sherr)
			}
			for stage, want := range map[string]int{"test": wantTest, "stress": 13, "stress-packages": 1, "stress-plane-cpu1": 1, "stress-plane": 2,
				"stress-sidecar-cpu1": 1, "stress-sidecar": 2, "stress-processgroup": 3, "stress-functions": 3, "native": len(native)} {
				f := &fakeRunner{native: stream(qualification()...)}
				code, out, errOut := runDriver(t, goos, f, stage)
				wantCode := 0
				if stage == "native" && goos != "darwin" {
					wantCode = 1
				}
				if code != wantCode || len(f.calls) != want {
					t.Fatalf("runFor(%s, %s) = %d with %d calls: %s", goos, stage, code, len(f.calls), errOut)
				}
				os.RemoveAll(scratchFrom(out))
			}
		})
	}
	for _, goos := range []string{"windows", "freebsd", ""} {
		if _, err := StressSteps(goos); err == nil {
			t.Fatalf("StressSteps(%q) accepted", goos)
		}
		if _, err := StressShards(goos); err == nil {
			t.Fatalf("StressShards(%q) accepted", goos)
		}
		if _, err := NativeSteps(goos); err == nil {
			t.Fatalf("NativeSteps(%q) accepted", goos)
		}
		if len(TestSteps(goos)) != 1 {
			t.Fatalf("TestSteps(%q) must be the plain suite only", goos)
		}
	}
}
