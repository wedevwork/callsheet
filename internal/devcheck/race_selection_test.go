package devcheck

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// raceSelections are a plan's untagged race steps' package patterns, one
// list per step (the tagged sidecar contract race check is separate and
// not a whole-module pass).
func raceSelections(plan []Step) [][]string {
	var out [][]string
	for _, s := range plan {
		if !slices.Contains(s.Argv, "-race") || slices.ContainsFunc(s.Argv, func(a string) bool { return strings.HasPrefix(a, "-tags=") }) {
			continue
		}
		var pats []string
		for _, a := range s.Argv[2:] {
			if !strings.HasPrefix(a, "-") {
				pats = append(pats, a)
			}
		}
		out = append(out, pats)
	}
	return out
}

// goList resolves patterns to packages with go list from the module root
// (a pattern selecting nothing warns on stderr, which it refuses).
func goList(t *testing.T, patterns ...string) []string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("go", append([]string{"list"}, patterns...)...)
	cmd.Dir, cmd.Stderr = filepath.Join("..", ".."), &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() != 0 {
		t.Fatalf("go list %v: %v %s", patterns, err, stderr.String())
	}
	return strings.Fields(string(out))
}

// raceCounts is how many of a plan's race steps select each package: each
// step's patterns resolve separately (go list deduplicates within one
// invocation, never across steps).
func raceCounts(t *testing.T, plan []Step) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, pats := range raceSelections(plan) {
		for _, p := range goList(t, pats...) {
			counts[p]++
		}
	}
	return counts
}

// TestRaceSelectionCoversModule keeps the Linux race suite whole in both
// modes (design 12a-dev-flow-verify r0.7): locally the planned untagged
// race steps select every package of go list ./... exactly once; in CI
// they select every package but internal/reale2e exactly once and
// internal/reale2e not at all. A package added outside the shared list,
// or selected by two steps, fails here.
func TestRaceSelectionCoversModule(t *testing.T) {
	all := goList(t, "./...")
	reale2e := goList(t, RaceReale2ePackage)
	if len(reale2e) != 1 || !strings.HasSuffix(reale2e[0], "/internal/reale2e") {
		t.Fatalf("the reale2e pattern selects %v", reale2e)
	}
	check := func(ci bool, counts map[string]int) []string {
		var bad []string
		for _, p := range all {
			want := 1
			if ci && p == reale2e[0] {
				want = 0
			}
			if counts[p] != want {
				bad = append(bad, fmt.Sprintf("%s selected by %d race steps, want %d", p, counts[p], want))
			}
			delete(counts, p)
		}
		for p, n := range counts {
			bad = append(bad, fmt.Sprintf("%s (outside ./...) selected by %d race steps", p, n))
		}
		return bad
	}
	for _, ci := range []bool{false, true} {
		for _, b := range check(ci, raceCounts(t, testSteps("linux", ci))) {
			t.Errorf("ci=%t: %s", ci, b)
		}
	}
	// A planted step overlapping the shared one is caught.
	planted := append(testSteps("linux", false), Step{Name: "test -race planted", Argv: []string{"go", "test", "-race", "./internal/mcp/..."}})
	if bad := check(false, raceCounts(t, planted)); len(bad) != 1 || !strings.Contains(bad[0], "/internal/mcp selected by 2 race steps") {
		t.Errorf("a planted overlapping step: %v", bad)
	}
	sorted := append([]string(nil), RaceSharedPackages...)
	sort.Strings(sorted)
	if strings.Join(sorted, " ") != strings.Join(RaceSharedPackages, " ") {
		t.Errorf("the shared race patterns are not sorted: %v", RaceSharedPackages)
	}
}

// TestTestPlanModes pins the two plans: the only difference is the CI
// plan's omission of the dedicated reale2e race step on Linux.
func TestTestPlanModes(t *testing.T) {
	local, ci := testSteps("linux", false), testSteps("linux", true)
	step := func(plan []Step, name string) *Step {
		for i := range plan {
			if plan[i].Name == name {
				return &plan[i]
			}
		}
		return nil
	}
	for mode, plan := range map[string][]Step{"local": local, "ci": ci} {
		if s := step(plan, "test"); s == nil || strings.Join(s.Argv, " ") != "go test -count=1 -timeout=180s ./..." {
			t.Errorf("%s: the ordinary ./... step %+v", mode, s)
		}
		if s := step(plan, "test -race"); s == nil || !slices.Contains(s.Argv, "-timeout=180s") || !slices.Contains(s.Argv, "-p=1") ||
			slices.Contains(s.Argv, RaceReale2ePackage) {
			t.Errorf("%s: the shared race step %+v", mode, s)
		}
	}
	if s := step(local, "test -race reale2e"); s == nil || strings.Join(s.Argv, " ") != "go test -race -count=1 -timeout=360s ./internal/reale2e" {
		t.Errorf("local: the dedicated reale2e race step %+v", s)
	}
	for _, s := range ci {
		if slices.Contains(s.Argv, RaceReale2ePackage) {
			t.Errorf("ci: a dedicated reale2e race command %v", s.Argv)
		}
	}
	var rest []Step
	for _, s := range local {
		if s.Name != "test -race reale2e" {
			rest = append(rest, s)
		}
	}
	if len(ci) != len(local)-1 || strings.Join(argvOf(rest), "|") != strings.Join(argvOf(ci), "|") {
		t.Errorf("the plans differ beyond the reale2e race step:\n%v\n%v", argvOf(local), argvOf(ci))
	}
	if strings.Join(argvOf(testSteps("darwin", true)), "|") != strings.Join(argvOf(testSteps("darwin", false)), "|") {
		t.Error("the darwin plans differ by mode")
	}
	if strings.Join(argvOf(TestSteps("linux")), "|") != strings.Join(argvOf(local), "|") {
		t.Error("TestSteps is not the local plan")
	}
	// macOS native still runs internal/reale2e in its ordinary suite.
	native, err := NativeSteps("darwin")
	if err != nil || len(native) != 1 || !slices.Contains(native[0].Argv, "./...") || slices.Contains(native[0].Argv, "-race") {
		t.Errorf("native %v %v", native, err)
	}
}

// TestTestPlanModeDriver drives the test stage through runWith with a
// controlled CI lookup: an absent CI is local mode; a present CI (empty,
// true, false or 0) is CI mode. Exactly one mode line is printed, before
// the stage's first child command, also through devcheck all.
func TestTestPlanModeDriver(t *testing.T) {
	absent := func(string) (string, bool) { return "", false }
	present := func(v string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k == "CI" {
				return v, true
			}
			return "", false
		}
	}
	if ciPresent(absent) || !ciPresent(nil) {
		t.Fatal("ciPresent: an absent CI is local, a missing lookup CI")
	}
	for name, c := range map[string]struct {
		lookup func(string) (string, bool)
		ci     bool
	}{
		"absent": {absent, false}, "empty": {present(""), true}, "true": {present("true"), true}, "false": {present("false"), true}, "zero": {present("0"), true},
	} {
		for _, stage := range []string{"test", "all"} {
			t.Run(name+"_"+stage, func(t *testing.T) {
				f := &fakeRunner{coverTotal: "81%", cmdList: cmdList, profile: goodProfile}
				cr := newContainerRunnerFixture(t, f).(*containerRunner)
				var out, errOut bytes.Buffer
				code := runWith(context.Background(), "linux", c.lookup, []string{stage}, &out, &errOut, cr.run, testOpts(t))
				if code != 0 {
					t.Fatalf("%s = %d %s", stage, code, errOut.String())
				}
				os.RemoveAll(scratchFrom(out.String()))
				want, other := TestPlanLocalLine, TestPlanCILine
				if c.ci {
					want, other = TestPlanCILine, TestPlanLocalLine
				}
				text := out.String()
				first := strings.Index(text, "devcheck: test: ")
				if strings.Count(text, want+"\n") != 1 || strings.Contains(text, other) || first < 0 || strings.Index(text, want+"\n") > first {
					t.Fatalf("mode lines in:\n%s", text)
				}
				calls := strings.Join(f.argvs(), "|")
				if hasOwn := strings.Contains(calls, "-timeout=360s ./internal/reale2e"); hasOwn == c.ci {
					t.Fatalf("ci=%t, the dedicated reale2e race step ran %t", c.ci, hasOwn)
				}
				if !strings.Contains(calls, "go test -count=1 -timeout=180s ./...") || !strings.Contains(calls, wantTestRace) {
					t.Fatalf("the ordinary or shared race step is missing: %s", calls)
				}
			})
		}
	}
	// A darwin test stage prints its mode line too.
	var out, errOut bytes.Buffer
	if code := runWith(context.Background(), "darwin", present("true"), []string{"test"}, &out, &errOut, (&fakeRunner{}).run, testOpts(t)); code != 0 ||
		strings.Count(out.String(), TestPlanCILine+"\n") != 1 {
		t.Fatalf("darwin test = %d %s %s", code, out.String(), errOut.String())
	}
	os.RemoveAll(scratchFrom(out.String()))
}
