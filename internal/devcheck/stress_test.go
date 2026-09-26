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
// design 02c with processgroup's CPU settings as separate invocations); it
// is compared against StressShards and StressSteps, never derived from them.
const (
	wantStressPackages      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/plane ./internal/client ./internal/sidecar ./internal/contract"
	wantStressPG1           = "go test -race -count=20 -cpu=1 -timeout=6m ./internal/spikes/processgroup"
	wantStressPG2           = "go test -race -count=20 -cpu=2 -timeout=6m ./internal/spikes/processgroup"
	wantStressPG4           = "go test -race -count=20 -cpu=4 -timeout=6m ./internal/spikes/processgroup"
	wantStressFunction      = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"
	wantStressPlaneFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function"
	// wantStressNodeFunction is iteration 03's node process-boundary step.
	wantStressNodeFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"
	// wantStressPlan is the flattened StressSteps view in shard/CPU order.
	wantStressPlan = wantStressPackages + "|" + wantStressPG1 + "|" + wantStressPG2 + "|" + wantStressPG4 + "|" + wantStressFunction + "|" + wantStressPlaneFunction + "|" + wantStressNodeFunction
)

// want03Additions is iteration 03's literal addition to the 02b selection
// (design 03, CI plan): the three node packages completely and the two
// node process-boundary subtests.
var want03Additions = []string{
	"go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/client ./internal/sidecar ./internal/contract",
	wantStressNodeFunction,
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
	{"processgroup", true, [][2]string{{"stress processgroup cpu1", wantStressPG1}, {"stress processgroup cpu2", wantStressPG2}, {"stress processgroup cpu4", wantStressPG4}}},
	{"functions", false, [][2]string{{"stress function", wantStressFunction}, {"stress plane function", wantStressPlaneFunction}, {"stress node function", wantStressNodeFunction}}},
}

// Sequential call groups of each stress stage: calls within a group are
// concurrent and compared as a set; the groups are ordered boundaries.
var (
	groupPackages     = []string{wantStressPackages}
	groupProcessGroup = []string{wantStressPG1, wantStressPG2, wantStressPG4}
	groupFunctions1   = []string{wantStressFunction}
	groupFunctions2   = []string{wantStressPlaneFunction}
	groupFunctions3   = []string{wantStressNodeFunction}
	wantStageGroups   = map[string][][]string{
		"stress":              {groupPackages, groupProcessGroup, groupFunctions1, groupFunctions2, groupFunctions3},
		"stress-packages":     {groupPackages},
		"stress-processgroup": {groupProcessGroup},
		"stress-functions":    {groupFunctions1, groupFunctions2, groupFunctions3},
	}
)

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
		if err != nil || len(steps) != 7 || strings.Join(argvOf(steps), "|") != wantStressPlan || strings.Join(flat, "|") != wantStressPlan {
			t.Fatalf("%s flattened = %v %v", goos, argvOf(steps), err)
		}
		for i, name := range []string{"stress packages", "stress processgroup cpu1", "stress processgroup cpu2", "stress processgroup cpu4", "stress function", "stress plane function", "stress node function"} {
			if steps[i].Name != name || strings.Join(steps[i].Env, " ") != "CGO_ENABLED=1" {
				t.Fatalf("%s flattened step %d = %+v", goos, i, steps[i])
			}
		}
	}
	// Returned plans are independent: mutating one never changes the next,
	// including nested step, argv and env slices.
	a, _ := StressShards("linux")
	a[0].Name = "mutated"
	a[1].Parallel = false
	a[1].Steps[0].Argv[3] = "-count=1"
	a[1].Steps[2].Env[0] = "CGO_ENABLED=0"
	a[2].Steps[1].Argv[6] = "-run=^(TestPlaneState)$"
	a[2].Steps = a[2].Steps[:1]
	a[0].Steps[0].Name = "mutated"
	f, _ := StressSteps("linux")
	f[0].Argv[4] = "-cpu=1"
	f[5].Env[0] = "CGO_ENABLED=0"
	b, _ := StressShards("linux")
	c, _ := StressSteps("linux")
	if b[0].Name != "packages" || !b[1].Parallel || len(b[2].Steps) != 3 || b[0].Steps[0].Name != "stress packages" ||
		strings.Join(b[1].Steps[0].Argv, " ") != wantStressPG1 || b[1].Steps[2].Env[0] != "CGO_ENABLED=1" ||
		strings.Join(b[2].Steps[1].Argv, " ") != wantStressPlaneFunction || strings.Join(argvOf(c), "|") != wantStressPlan || c[5].Env[0] != "CGO_ENABLED=1" {
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
// selection plus iteration 03's literal additions: every (package,
// selector, CPU, count) tuple appears exactly once, the shards are
// disjoint and nothing of 02b is lost.
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
				t.Errorf("%s: %v is not in the 02b or 03 selection", goos, s)
			}
		}
		for s, sh := range owner {
			if (s.pkg == "./internal/spikes/processgroup") != (sh == "processgroup") || (s.selector != "") != (sh == "functions") {
				t.Errorf("%s: %v placed in shard %s", goos, s, sh)
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
		for _, s := range steps[:4] {
			if runValue(s) != "" {
				t.Fatalf("%s: %s has a selector: %v", goos, s.Name, s.Argv)
			}
		}
		fn, plane, nodes := splitRun(runValue(steps[4])), splitRun(runValue(steps[5])), splitRun(runValue(steps[6]))
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
		for _, stage := range []string{"stress", "stress-packages", "stress-processgroup", "stress-functions"} {
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
	if code, _, errOut := runDriver(t, "linux", f, "all"); code != 0 || len(f.calls) != 20 {
		t.Fatalf("all = %d with %d calls %s", code, len(f.calls), errOut)
	}
	for _, c := range f.argvs() {
		if strings.Contains(c, "-count=20") || strings.Contains(c, "-cpu=") {
			t.Fatalf("all ran a stress command: %s", c)
		}
	}
	// No operands, count, CPU or output flags, for every stress stage.
	for _, stage := range []string{"stress", "stress-packages", "stress-processgroup", "stress-functions"} {
		for _, extra := range [][]string{{"extra"}, {"-o", "x"}, {"-count=1"}, {"-cpu=1"}, {"-race"}, {"packages"}} {
			f := &fakeRunner{}
			args := append([]string{stage}, extra...)
			if code, out, errOut := runDriver(t, "linux", f, args...); code != 2 || !strings.Contains(errOut, "invalid arguments for "+stage) || len(f.calls) != 0 || out != "" {
				t.Fatalf("%v = %d %s", args, code, errOut)
			}
		}
	}
	for _, bogus := range []string{"stress-plane", "stress-package", "stress-", "stress-functions2"} {
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
		{"stress", "-cpu=2 ", "stress processgroup cpu2", 4},
		{"stress", "TestFP4TransportHarness", "stress function", 5},
		{"stress", "TestPlaneState", "stress plane function", 6},
		{"stress-packages", "./internal/plane", "stress packages", 1},
		{"stress-processgroup", "-cpu=4 ", "stress processgroup cpu4", 3},
		{"stress-functions", "TestFP5GitRoundTrip", "stress function", 1},
		{"stress-functions", "TestPlaneTLS", "stress plane function", 2},
		{"stress", "TestNodeEnrollment", "stress node function", 7},
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
	for _, stage := range []string{"stress", "stress-packages", "stress-processgroup", "stress-functions"} {
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
	// shares one watchdog across its shards (DS2): one context for all seven.
	r := &ctxRecorder{}
	before := time.Now()
	if err := stressDriver(t, context.Background(), r.run).stress(shards); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(r.ctxs) != 7 {
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
	}{{shards, "stress packages"}, {shards[1:2], "stress processgroup"}, {shards[2:], "stress function"}} {
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
	// Expiry during the concurrent shard: all three have started (a
	// barrier), every Runner returns nil after expiry, the shard still
	// fails and the functions shard never starts.
	expiring = newExpiringCtx()
	arrived := make(chan struct{}, 3)
	r = &ctxRecorder{next: func(ctx context.Context, argv []string) error {
		if !strings.Contains(strings.Join(argv, " "), "processgroup") {
			return nil
		}
		arrived <- struct{}{}
		if len(arrived) == 3 {
			expiring.expire()
		}
		<-ctx.Done()
		return nil
	}}
	err = stressDriver(t, expiring, r.run).stress(shards)
	if !errors.Is(err, context.DeadlineExceeded) || len(r.ctxs) != 4 || !strings.Contains(err.Error(), "stress watchdog (15m0s) ended during stress processgroup") {
		t.Fatalf("expired processgroup = %v after %d calls", err, len(r.ctxs))
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

func processgroupSteps(t *testing.T) []Step {
	t.Helper()
	shards, err := StressShards("linux")
	if err != nil || !shards[1].Parallel {
		t.Fatalf("shards = %+v %v", shards, err)
	}
	return shards[1].Steps
}

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

// TestStressConcurrencyContract is the FP-2 (iteration 02c) contract of the
// concurrent coordinator runConcurrentCPU, executed by name from
// tests/function (TestStressShardExecution). Its subtests overlap, failure,
// watchdog and logs use channel barriers, never sleeps, as their
// correctness oracle. Do not rename or skip them.
func TestStressConcurrencyContract(t *testing.T) {
	t.Run("overlap", func(t *testing.T) {
		steps := processgroupSteps(t)
		var mu sync.Mutex
		active, peak := 0, 0
		var argvs []string
		b := newBarrier(3)
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
		if err := runConcurrentCPU(context.Background(), run, steps, &memLogs{}, &out); err != nil {
			t.Fatalf("three overlapping invocations: %v", err)
		}
		if peak != 3 || active != 0 || !callsMatch(argvs, [][]string{groupProcessGroup}) {
			t.Fatalf("peak %d, active %d, calls %q", peak, active, argvs)
		}
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
		// Sequential boundaries through dispatch: packages ends before any
		// processgroup call starts, all three processgroup calls overlap and
		// end before the functions shard starts, which runs in order.
		var events []string
		pg := newBarrier(3)
		rec := func(_ context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
			a := strings.Join(argv, " ")
			mu.Lock()
			events = append(events, "start "+a)
			mu.Unlock()
			var err error
			if strings.Contains(a, "processgroup") {
				err = pg.wait()
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
		if len(events) != 14 || events[0] != "start "+wantStressPackages || events[1] != "end "+wantStressPackages ||
			events[8] != "start "+wantStressFunction || events[9] != "end "+wantStressFunction ||
			events[10] != "start "+wantStressPlaneFunction || events[11] != "end "+wantStressPlaneFunction ||
			events[12] != "start "+wantStressNodeFunction || events[13] != "end "+wantStressNodeFunction {
			t.Fatalf("boundaries: %q", events)
		}
		var starts, ends []string
		for _, e := range events[2:5] {
			starts = append(starts, strings.TrimPrefix(e, "start "))
		}
		for _, e := range events[5:8] {
			ends = append(ends, strings.TrimPrefix(e, "end "))
		}
		if !callsMatch(starts, [][]string{groupProcessGroup}) || !callsMatch(ends, [][]string{groupProcessGroup}) {
			t.Fatalf("processgroup calls did not all start before any ended: %q", events)
		}
	})

	t.Run("failure", func(t *testing.T) {
		steps := processgroupSteps(t)
		for _, failing := range [][]int{{0}, {1}, {2}, {0, 2}, {0, 1, 2}} {
			fails := map[string]bool{}
			for _, i := range failing {
				fails[steps[i].Name] = true
			}
			b := newBarrier(3)
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
			if err == nil || len(ctxs) != 3 {
				t.Fatalf("failing %v: %v with %d calls", failing, err, len(ctxs))
			}
			msg := err.Error()
			last := -1
			for _, s := range steps {
				i := strings.Index(msg, s.Name+" failed: "+strings.Join(s.Argv, " ")+": exit status 1\nboom from "+s.Name)
				if fails[s.Name] != (i >= 0) {
					t.Fatalf("failing %v: %s reported=%v:\n%s", failing, s.Name, i >= 0, msg)
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
		// Through dispatch, a failed processgroup invocation fails the
		// shard after all three ran, and the next shard never starts.
		f := &fakeRunner{fail: "-cpu=1 "}
		code, out, errOut := runDriver(t, "linux", f, "stress")
		if code != 1 || !callsMatch(f.argvs(), [][]string{groupPackages, groupProcessGroup}) || !strings.Contains(errOut, "stage stress FAILED: stress processgroup cpu1 failed") ||
			strings.Contains(errOut, "cpu2 failed") || !strings.Contains(out, "devcheck: stress processgroup cpu4: ok in ") {
			t.Fatalf("dispatch = %d %q %s", code, f.argvs(), errOut)
		}
		os.RemoveAll(scratchFrom(out))
	})

	t.Run("watchdog", func(t *testing.T) {
		steps := processgroupSteps(t)
		// A context already done starts nothing and creates no log.
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		logs := &memLogs{}
		err := runConcurrentCPU(canceled, func(context.Context, []string, []string, string, io.Writer, io.Writer) error { calls++; return nil }, steps, logs, io.Discard)
		if !errors.Is(err, context.Canceled) || calls != 0 || len(logs.files) != 0 || !strings.Contains(err.Error(), "ended before stress processgroup cpu1") {
			t.Fatalf("pre-canceled = %v, %d calls, %d logs", err, calls, len(logs.files))
		}
		// Cancellation after all three started reaches every call; the
		// coordinator joins all three before returning, and fails even
		// though two Runners return nil after the cancellation.
		parent, cancelParent := context.WithCancel(context.Background())
		defer cancelParent()
		b := newBarrier(3)
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
		if n := returned.Load(); n != 3 {
			t.Fatalf("coordinator returned with %d of 3 calls joined", n)
		}
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "context ended while the concurrent invocations ran") ||
			!strings.Contains(err.Error(), "stress processgroup cpu2 failed") || strings.Contains(err.Error(), "cpu1 failed") || !logs.allClosed() {
			t.Fatalf("canceled during run = %v", err)
		}
		// Expiry between launches: cpu1 starts, then the deadline passes;
		// cpu2 and cpu4 never start, and cpu1's nil result does not pass.
		var started []string
		var mu sync.Mutex
		ctx := newCountdownCtx(2)
		logs = &memLogs{}
		err = runConcurrentCPU(ctx, func(ctx context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
			mu.Lock()
			started = append(started, strings.Join(argv, " "))
			mu.Unlock()
			<-ctx.Done()
			return nil
		}, steps, logs, io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) || len(started) != 1 || started[0] != wantStressPG1 ||
			!strings.Contains(err.Error(), "stress processgroup cpu2 failed: "+wantStressPG2+": not started: context deadline exceeded") ||
			!strings.Contains(err.Error(), "stress processgroup cpu4 failed: "+wantStressPG4+": not started") || strings.Contains(err.Error(), "cpu1 failed") || !logs.allClosed() {
			t.Fatalf("expiry during launch = %v, started %q", err, started)
		}
		// Expiry after every Runner returned nil still fails.
		ctx = newCountdownCtx(4)
		err = runConcurrentCPU(ctx, func(context.Context, []string, []string, string, io.Writer, io.Writer) error { return nil }, steps, &memLogs{}, io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "failed:") {
			t.Fatalf("expiry despite nil results = %v", err)
		}
	})

	t.Run("logs", func(t *testing.T) {
		steps := processgroupSteps(t)
		// Completion order is the reverse of plan order (cpu4, cpu2, cpu1);
		// replay is still in plan order and each log holds only its own
		// output, written through both stdout and stderr.
		b := newBarrier(3)
		done := map[string]chan struct{}{}
		for _, s := range steps {
			done[s.Name] = make(chan struct{})
		}
		after := map[string]string{"stress processgroup cpu1": "stress processgroup cpu2", "stress processgroup cpu2": "stress processgroup cpu4"}
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
		// Injected log failures fail the shard even though every child
		// exited zero, naming the command; a failed preparation starts
		// nothing and closes the logs already created.
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
		// Scratch: kept with its path on failure, removed after success.
		f := &fakeRunner{fail: "-cpu=2 "}
		code, dout, errOut := runDriver(t, "linux", f, "stress-processgroup")
		scratch = scratchFrom(dout)
		if code != 1 || !strings.Contains(errOut, "logs retained in "+scratch) {
			t.Fatalf("failure = %d %s", code, errOut)
		}
		if b, err := os.ReadFile(filepath.Join(scratch, "stress-processgroup-cpu2.log")); err != nil || !strings.Contains(string(b), "boom from child") {
			t.Fatalf("failed log: %q %v", b, err)
		}
		os.RemoveAll(scratch)
		code, dout, errOut = runDriver(t, "linux", &fakeRunner{}, "stress-processgroup")
		if _, err := os.Stat(scratchFrom(dout)); code != 0 || !os.IsNotExist(err) {
			t.Fatalf("success = %d %s, scratch %v", code, errOut, err)
		}
		// A scratch that cannot hold the logs starts nothing.
		f = &fakeRunner{}
		d := &driver{ctx: context.Background(), run: f.run, out: io.Discard, errOut: io.Discard, scratch: filepath.Join(t.TempDir(), "missing"), goos: "linux"}
		shards, _ := StressShards("linux")
		if err := d.stress(shards[1:2]); err == nil || len(f.calls) != 0 || !strings.Contains(err.Error(), "create log") {
			t.Fatalf("missing scratch = %v after %d calls", err, len(f.calls))
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
			if serr != nil || len(stress) != 7 || sherr != nil || len(shards) != 3 {
				t.Fatalf("StressSteps(%s) = %v %v, shards %v %v", goos, stress, serr, shards, sherr)
			}
			for stage, want := range map[string]int{"test": wantTest, "stress": 7, "stress-packages": 1, "stress-processgroup": 3, "stress-functions": 3, "native": len(native)} {
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
