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

// functionLiteral pins the function test package's import path
// independently of production (design 12a-linux-test-budget).
const functionLiteral = "github.com/wedevwork/callsheet/tests/function"

// untaggedSteps are a plan's untagged race steps (race) or untagged
// ordinary steps (!race), in order. The tagged sidecar contract checks are
// separate and not whole-module passes.
func untaggedSteps(plan []Step, race bool) []Step {
	var out []Step
	for _, s := range plan {
		if slices.Contains(s.Argv, "-race") != race || slices.ContainsFunc(s.Argv, func(a string) bool { return strings.HasPrefix(a, "-tags=") }) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// stepPatterns are a go test step's package patterns: its arguments after
// "go test" that are not flags.
func stepPatterns(s Step) []string {
	var pats []string
	for _, a := range s.Argv[2:] {
		if !strings.HasPrefix(a, "-") {
			pats = append(pats, a)
		}
	}
	return pats
}

// raceSelections are a plan's untagged race steps' package patterns, one
// list per step.
func raceSelections(plan []Step) [][]string {
	var out [][]string
	for _, s := range untaggedSteps(plan, true) {
		out = append(out, stepPatterns(s))
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
// or selected by two steps, fails here. Design 12a-linux-test-budget:
// tests/function is selected only by its own 300-second race step, which
// selects it alone, and the 180-second shared race step selects neither
// it nor internal/reale2e.
func TestRaceSelectionCoversModule(t *testing.T) {
	all := goList(t, "./...")
	reale2e := goList(t, RaceReale2ePackage)
	if len(reale2e) != 1 || reale2e[0] != reale2eLiteral {
		t.Fatalf("the reale2e pattern selects %v", reale2e)
	}
	if !slices.Contains(all, functionLiteral) {
		t.Fatalf("go list ./... lacks %s: %v", functionLiteral, all)
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
	// functionOnly checks the dedicated function race step: exactly one
	// 300 s race step, selecting tests/function alone; no other race step
	// selects tests/function, and the 180 s one selects no dedicated
	// package.
	functionOnly := func(plan []Step) []string {
		var bad []string
		dedicated := 0
		for _, s := range untaggedSteps(plan, true) {
			pkgs := goList(t, stepPatterns(s)...)
			own := slices.Contains(s.Argv, "-timeout=300s")
			if own {
				dedicated++
				if !slices.Equal(pkgs, []string{functionLiteral}) {
					bad = append(bad, fmt.Sprintf("the 300s race step %q selects %v, want %s alone", s.Name, pkgs, functionLiteral))
				}
			} else if slices.Contains(pkgs, functionLiteral) {
				bad = append(bad, fmt.Sprintf("race step %q selects %s outside its 300s step", s.Name, functionLiteral))
			}
			if slices.Contains(s.Argv, "-timeout=180s") && (slices.Contains(pkgs, functionLiteral) || slices.Contains(pkgs, reale2eLiteral)) {
				bad = append(bad, fmt.Sprintf("the 180s race step %q selects a dedicated package", s.Name))
			}
		}
		if dedicated != 1 {
			bad = append(bad, fmt.Sprintf("%d 300s race steps, want 1", dedicated))
		}
		return bad
	}
	for _, ci := range []bool{false, true} {
		plan := testSteps("linux", ci)
		for _, b := range check(ci, raceCounts(t, plan)) {
			t.Errorf("ci=%t: %s", ci, b)
		}
		for _, b := range functionOnly(plan) {
			t.Errorf("ci=%t: %s", ci, b)
		}
	}
	// A planted step overlapping the shared one is caught.
	planted := append(testSteps("linux", false), Step{Name: "test -race planted", Argv: []string{"go", "test", "-race", "./internal/mcp/..."}})
	if bad := check(false, raceCounts(t, planted)); len(bad) != 1 || !strings.Contains(bad[0], "/internal/mcp selected by 2 race steps") {
		t.Errorf("a planted overlapping step: %v", bad)
	}
	// The former ./tests/... shared race pattern puts tests/function in two
	// race steps, one of them the 180 s one.
	former := testSteps("linux", false)
	former[3].Argv = append(former[3].Argv, "./tests/...")
	if bad := check(false, raceCounts(t, former)); len(bad) != 1 || !strings.Contains(bad[0], functionLiteral+" selected by 2 race steps") {
		t.Errorf("a planted ./tests/... shared race pattern: %v", bad)
	}
	if bad := functionOnly(former); len(bad) != 2 || !strings.Contains(strings.Join(bad, "\n"), "outside its 300s step") ||
		!strings.Contains(strings.Join(bad, "\n"), "the 180s race step") {
		t.Errorf("a planted ./tests/... shared race pattern: %v", bad)
	}
	// A function race step that selects more than tests/function.
	wide := testSteps("linux", false)
	wide[4].Argv = append(wide[4].Argv, "./internal/mcp/...")
	if bad := functionOnly(wide); len(bad) != 1 || !strings.Contains(bad[0], "want "+functionLiteral+" alone") {
		t.Errorf("a planted wide function race step: %v", bad)
	}
	for _, list := range [][]string{RaceSharedPackages, TestSharedPackages} {
		sorted := append([]string(nil), list...)
		sort.Strings(sorted)
		if strings.Join(sorted, " ") != strings.Join(list, " ") {
			t.Errorf("the shared patterns are not sorted: %v", list)
		}
	}
}

// TestOrdinarySelectionCoversModule keeps the ordinary suite whole on Linux
// and Darwin in both modes (design 12a-linux-test-budget): the planned
// untagged, non-race steps, each resolved by its own go list, select every
// package of go list ./... exactly once and nothing else; internal/reale2e
// and tests/function are each the sole package of their dedicated
// 360-second step, and the 180-second step selects neither. A package added
// outside the explicit shared list (tests/<new> included), an omitted
// dedicated step or a package selected twice fails here.
func TestOrdinarySelectionCoversModule(t *testing.T) {
	all := goList(t, "./...")
	if !slices.Contains(all, reale2eLiteral) || !slices.Contains(all, functionLiteral) {
		t.Fatalf("go list ./... lacks reale2e or function: %v", all)
	}
	dedicated := map[string]string{"test reale2e": reale2eLiteral, "test function": functionLiteral}
	timeouts := map[string]string{"test": "-timeout=180s", "test reale2e": "-timeout=360s", "test function": "-timeout=360s"}
	check := func(all []string, plan []Step) []string {
		var bad []string
		steps := untaggedSteps(plan, false)
		if len(steps) != 3 {
			bad = append(bad, fmt.Sprintf("%d ordinary steps, want 3", len(steps)))
		}
		counts := map[string]int{}
		named := map[string][]string{}
		for _, s := range steps {
			pkgs := goList(t, stepPatterns(s)...)
			named[s.Name] = pkgs
			for _, p := range pkgs {
				counts[p]++
			}
			if want, ok := timeouts[s.Name]; !ok || !slices.Contains(s.Argv, want) {
				bad = append(bad, fmt.Sprintf("ordinary step %q has no expected bound in %v", s.Name, s.Argv))
			}
		}
		for _, p := range all {
			if counts[p] != 1 {
				bad = append(bad, fmt.Sprintf("%s selected by %d ordinary steps, want 1", p, counts[p]))
			}
			delete(counts, p)
		}
		for p, n := range counts {
			bad = append(bad, fmt.Sprintf("%s (outside ./...) selected by %d ordinary steps", p, n))
		}
		shared, ok := named["test"]
		if !ok {
			bad = append(bad, "no shared 180s test step")
		}
		for name, pkg := range dedicated {
			if got, ok := named[name]; !ok {
				bad = append(bad, "no dedicated "+name+" step")
			} else if !slices.Equal(got, []string{pkg}) {
				bad = append(bad, fmt.Sprintf("the dedicated %s step selects %v, want %s alone", name, got, pkg))
			}
			if slices.Contains(shared, pkg) {
				bad = append(bad, "the 180s test step selects "+pkg)
			}
		}
		slices.Sort(bad)
		return bad
	}
	// The actual plans: both platform arguments, both modes.
	for _, goos := range []string{"linux", "darwin"} {
		for _, ci := range []bool{false, true} {
			if bad := check(all, testSteps(goos, ci)); len(bad) != 0 {
				t.Errorf("%s ci=%t: the ordinary selections do not cover the module once each:\n%s", goos, ci, strings.Join(bad, "\n"))
			}
		}
	}
	// Planted negatives, each a copy of the real local Linux plan with one
	// change.
	planted := func(edit func([]Step) []Step) []Step { return edit(testSteps("linux", false)) }
	has := func(bad []string, want string) bool {
		return slices.ContainsFunc(bad, func(b string) bool { return strings.Contains(b, want) })
	}
	// An overlap: the 180 s step selects ./... again.
	overlap := check(all, planted(func(p []Step) []Step {
		p[0].Argv = []string{"go", "test", "-count=1", "-timeout=180s", "./..."}
		return p
	}))
	if len(overlap) != 4 || !has(overlap, reale2eLiteral+" selected by 2 ordinary steps") || !has(overlap, functionLiteral+" selected by 2 ordinary steps") ||
		!has(overlap, "the 180s test step selects "+reale2eLiteral) || !has(overlap, "the 180s test step selects "+functionLiteral) {
		t.Errorf("a planted ./... overlap: %v", overlap)
	}
	// A missing shared subtree.
	missing := check(all, planted(func(p []Step) []Step {
		p[0].Argv = slices.DeleteFunc(p[0].Argv, func(s string) bool { return s == "./internal/workspace/..." })
		return p
	}))
	if len(missing) != 1 || !has(missing, "/internal/workspace selected by 0 ordinary steps") {
		t.Errorf("a planted missing shared subtree: %v", missing)
	}
	// An omitted dedicated command.
	omitted := check(all, planted(func(p []Step) []Step {
		return slices.DeleteFunc(p, func(s Step) bool { return s.Name == "test function" })
	}))
	if len(omitted) != 3 || !has(omitted, "2 ordinary steps, want 3") || !has(omitted, functionLiteral+" selected by 0 ordinary steps") ||
		!has(omitted, "no dedicated test function step") {
		t.Errorf("a planted omitted dedicated command: %v", omitted)
	}
	// A wrong dedicated selection.
	wrong := check(all, planted(func(p []Step) []Step {
		p[1].Argv = append(p[1].Argv, "./internal/mcp/...")
		return p
	}))
	if len(wrong) != 2 || !has(wrong, "/internal/mcp selected by 2 ordinary steps") || !has(wrong, "the dedicated test reale2e step selects") {
		t.Errorf("a planted wrong dedicated selection: %v", wrong)
	}
	// A newly added untagged package outside the explicit inventory, here
	// a tests/<new> package as go list ./... would report it, fails until
	// the inventory is updated.
	added := check(append(slices.Clone(all), "github.com/wedevwork/callsheet/tests/new"), testSteps("linux", false))
	if len(added) != 1 || !has(added, "github.com/wedevwork/callsheet/tests/new selected by 0 ordinary steps") {
		t.Errorf("a new tests/<new> package: %v", added)
	}
	// Mutating returned plans alters neither later plans nor the declared
	// package lists.
	checkFreshPlans(t)
}

// checkFreshPlans mutates every argv and environment entry of every
// returned task-platform plan and requires the next plans to equal the
// independent literals and the declared pattern lists to be unchanged.
func checkFreshPlans(t *testing.T) {
	t.Helper()
	shared, race := slices.Clone(TestSharedPackages), slices.Clone(RaceSharedPackages)
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, ci := range []bool{false, true} {
			p := testSteps(goos, ci)
			for i := range p {
				for j := range p[i].Argv {
					p[i].Argv[j] = "mutated"
				}
				for j := range p[i].Env {
					p[i].Env[j] = "MUTATED=1"
				}
			}
		}
	}
	for name, c := range map[string]struct {
		plan []Step
		want string
	}{
		"linux local": {testSteps("linux", false), wantTestPlanLinux}, "linux ci": {testSteps("linux", true), wantTestPlanLinuxCI},
		"darwin local": {testSteps("darwin", false), wantTestPlanDarwin}, "darwin ci": {testSteps("darwin", true), wantTestPlanDarwin},
		"windows": {testSteps("windows", true), wantTestOther},
	} {
		if got := strings.Join(argvOf(c.plan), "|"); got != c.want {
			t.Errorf("%s plan after mutation = %s", name, got)
		}
		for _, s := range c.plan {
			if env := strings.Join(s.Env, " "); env != "" && env != "CGO_ENABLED=1" {
				t.Errorf("%s step %q env after mutation = %s", name, s.Name, env)
			}
		}
	}
	if !slices.Equal(TestSharedPackages, shared) || !slices.Equal(RaceSharedPackages, race) ||
		"go test -count=1 -timeout=180s "+strings.Join(TestSharedPackages, " ") != wantTestOrdinaryShared ||
		len(TestSharedPackages) != 18 || len(RaceSharedPackages) != 19 || RaceSharedPackages[18] != "./tests/..." {
		t.Errorf("the declared pattern lists changed: %v %v", TestSharedPackages, RaceSharedPackages)
	}
}

// TestTestPlanModes pins the two plans: the only difference is the CI
// plan's omission of the dedicated reale2e race step on Linux. Design
// 12a-linux-test-budget: Linux and Darwin run the three ordinary steps in
// both modes, Linux then the shared and function race steps; every other
// host keeps the single ./... command.
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
	if len(local) != 8 || len(ci) != 7 || len(testSteps("darwin", false)) != 4 || len(testSteps("darwin", true)) != 4 {
		t.Fatalf("plan lengths: linux local %d, ci %d", len(local), len(ci))
	}
	for mode, plan := range map[string][]Step{"local": local, "ci": ci, "darwin local": testSteps("darwin", false), "darwin ci": testSteps("darwin", true)} {
		for i, c := range []struct{ name, argv string }{{"test", wantTestOrdinaryShared}, {"test reale2e", wantTestOrdinaryReale2e}, {"test function", wantTestOrdinaryFunction}} {
			if plan[i].Name != c.name || strings.Join(plan[i].Argv, " ") != c.argv || len(plan[i].Env) != 0 {
				t.Errorf("%s: ordinary step %d %+v", mode, i, plan[i])
			}
		}
	}
	for mode, plan := range map[string][]Step{"local": local, "ci": ci} {
		for i, c := range []struct{ name, argv string }{{"test -race", wantTestRace}, {"test -race function", wantTestRaceFunction}} {
			if s := plan[3+i]; s.Name != c.name || strings.Join(s.Argv, " ") != c.argv || strings.Join(s.Env, " ") != "CGO_ENABLED=1" {
				t.Errorf("%s: race step %d %+v", mode, 3+i, s)
			}
		}
		if s := step(plan, "test -race"); s == nil || !slices.Contains(s.Argv, "-timeout=180s") || !slices.Contains(s.Argv, "-p=1") ||
			slices.Contains(s.Argv, RaceReale2ePackage) || slices.Contains(s.Argv, "./tests/function") || slices.Contains(s.Argv, "./tests/...") {
			t.Errorf("%s: the shared race step %+v", mode, s)
		}
	}
	if s := step(local, "test -race reale2e"); s == nil || strings.Join(s.Argv, " ") != wantTestRaceReale2e || strings.Join(s.Env, " ") != "CGO_ENABLED=1" {
		t.Errorf("local: the dedicated reale2e race step %+v", s)
	}
	// The CI plan has no reale2e race child. The ordinary plan's dedicated
	// reale2e step names the package too, so the race child is identified
	// by -race as well as its name.
	for _, s := range ci {
		if s.Name == "test -race reale2e" || (slices.Contains(s.Argv, "-race") && slices.Contains(s.Argv, RaceReale2ePackage)) {
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
	for _, s := range testSteps("darwin", false) {
		if slices.Contains(s.Argv, "-race") || len(s.Env) != 0 {
			t.Errorf("darwin step %+v runs the race detector", s)
		}
	}
	if strings.Join(argvOf(TestSteps("linux")), "|") != strings.Join(argvOf(local), "|") {
		t.Error("TestSteps is not the local plan")
	}
	// Other hosts keep the single whole-module ordinary command.
	for _, goos := range []string{"windows", "freebsd", ""} {
		for _, mode := range []bool{false, true} {
			if p := testSteps(goos, mode); len(p) != 1 || p[0].Name != "test" || strings.Join(p[0].Argv, " ") != wantTestOther || len(p[0].Env) != 0 {
				t.Errorf("%q ci=%t plan %+v", goos, mode, p)
			}
		}
	}
	checkFreshPlans(t)
	// macOS native still runs internal/reale2e, without the race detector,
	// as its own second command after every other package (design
	// 12a-native-reale2e-budget); neither inherits -race or -p=1.
	native, err := NativeSteps("darwin")
	if err != nil || len(native) != 2 || slices.Contains(native[0].Argv, "./...") || slices.Contains(native[0].Argv, RaceReale2ePackage) ||
		!slices.Contains(native[0].Argv, "-timeout=300s") ||
		strings.Join(native[1].Argv, " ") != "go test -json -tags=realadaptercheck -count=1 -timeout=600s ./internal/reale2e" {
		t.Errorf("native %v %v", native, err)
	}
	for _, s := range native {
		if slices.Contains(s.Argv, "-race") || slices.Contains(s.Argv, "-p=1") || len(s.Env) != 0 {
			t.Errorf("native step %+v inherits the race step's flags", s)
		}
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
				// The race child is identified with -race: the ordinary
				// dedicated reale2e command also ends in its path.
				calls := f.argvs()
				if hasOwn := slices.Contains(calls, wantTestRaceReale2e); hasOwn == c.ci {
					t.Fatalf("ci=%t, the dedicated reale2e race step ran %t", c.ci, hasOwn)
				}
				for _, w := range []string{wantTestOrdinaryShared, wantTestOrdinaryReale2e, wantTestOrdinaryFunction, wantTestRace, wantTestRaceFunction} {
					if !slices.Contains(calls, w) {
						t.Fatalf("the step %q is missing: %v", w, calls)
					}
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
