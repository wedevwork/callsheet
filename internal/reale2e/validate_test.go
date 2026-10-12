package reale2e

import (
	"go/ast"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// fixtureFiles is a tested tree of the fixture program at go version ver.
func fixtureFiles(ver, main, test string) map[string][]byte {
	return map[string][]byte{"go.mod": []byte("module example.com/greeting\n\ngo " + ver + "\n"), "main.go": []byte(main), "main_test.go": []byte(test),
		"FEATURE.md": []byte(FeatureRequest), "design.md": designDoc, "design-review.md": designReview}
}

// repoGoVersion is this repository's go directive (the fixture's).
func repoGoVersion(t *testing.T) string {
	t.Helper()
	ver, err := goDirective(mustRead(t, filepath.Join(testkit.MustRepoRoot(t), "go.mod")))
	if err != nil {
		t.Fatal(err)
	}
	return ver
}

// realRunner runs commands as real supervised process groups.
func realRunner() commandRunner {
	return func(s ProcSpec, timeout time.Duration, limit int) (CommandResult, error) {
		return runCommand(ExecLauncher{}, realClock{}, s, timeout, limit)
	}
}

// seqMark is a validation event counter.
func seqMark() func(string, string) int {
	n := 0
	return func(string, string) int { n++; return n }
}

// brokenMain drops non-ASCII bytes of the name and accepts an empty name:
// a coder that misses the Unicode and empty-name requirements.
var brokenMain = strings.Replace(strings.Replace(fixtureMain, `	if *name == "" {
		fmt.Fprintln(stderr, "greeting: --name must not be empty")
		return 2
	}
`, "", 1), `fmt.Fprintf(stdout, "Hello, %s!\n", *name)`, `fmt.Fprintf(stdout, "Hello, %s!\n", strings.Map(func(r rune) rune {
		if r > 127 {
			return '?'
		}
		return r
	}, *name))`, 1)

// brokenTest omits the unicode and empty_name subtests.
var brokenTest = strings.Replace(strings.Replace(fixtureTest, `		{"unicode", []string{"--name", "世界"}, 0, "Hello, 世界!\n", false},
`, "", 1), `		{"empty_name", []string{"--name", ""}, 2, "", true},
`, "", 1)

// TestFinalValidationRunner (UT-12, FP-7): the live collector's actual
// local build and case runner on fixture code, with no vendor or network
// access: the conforming fixture passes every case and maps all ten
// TestRun subtests; a fixture missing the Unicode and empty-name
// requirements fails exactly those cases and lacks those subtests. The
// shared 60s budget bounds every command and each process group is
// stopped by its handle.
func TestFinalValidationRunner(t *testing.T) {
	// Not parallel with the other tests: its real Go builds would load the
	// process-timing tests that run beside them.
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go toolchain is a development prerequisite: %v", err)
	}
	ver := repoGoVersion(t)
	broken := strings.Replace(brokenMain, "\t\"os\"\n", "\t\"os\"\n\t\"strings\"\n", 1)
	// The first case runs alone on a cold private cache; the others start
	// from their own copy of the cache it leaves (the standard library
	// compiled once), every command still executing for each.
	coldDir := t.TempDir()
	for i, tc := range []struct {
		name         string
		main, test   string
		failed       []string
		missingTests []string
		// badAncestor exports below a directory whose .git Git rejects:
		// the throwaway build never consults ancestor VCS state.
		badAncestor bool
	}{
		{"conforming", fixtureMain, fixtureTest, nil, nil, false},
		{"missing requirement", broken, brokenTest, []string{"unicode", "empty_name"}, []string{"TestRun/unicode", "TestRun/empty_name"}, false},
		{"invalid git ancestor", fixtureMain, fixtureTest, nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if i > 0 {
				t.Parallel()
			}
			files := fixtureFiles(ver, tc.main, tc.test)
			dir := coldDir
			if i > 0 {
				dir = t.TempDir()
			}
			if tc.badAncestor {
				if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(dir, "export")
			}
			if i > 0 {
				if err := copyTree(filepath.Join(coldDir, "gocache"), filepath.Join(dir, "gocache")); err != nil {
					t.Fatal(err)
				}
			}
			v := runFinalValidation(realRunner(), realClock{}, goBin, os.Getenv("PATH"), dir, files, strings.Repeat("a", 40), strings.Repeat("b", 40), seqMark())
			if v.Err != nil || v.Test.ExitCode != 0 || v.Test.Toolchain == "" || !v.Cases.Build.Started || v.Cases.Build.ExitCode != 0 {
				t.Fatalf("validation: err %v test %+v build %+v\n%s%s\nbuild: %s%s", v.Err, v.Test, v.Cases.Build, v.TestOut, v.TestErr, v.BuildOut, v.BuildErr)
			}
			var failed []string
			for _, c := range v.Cases.Cases {
				if !c.Exec.Started || c.Exec.TimedOut || c.Exec.DeadlineMS <= 0 || c.Exec.DeadlineMS > FinalTestBound.Milliseconds() {
					t.Fatalf("case %s not executed within the budget: %+v", c.ID, c.Exec)
				}
				if !c.Pass {
					failed = append(failed, c.ID)
				}
			}
			if !slices.Equal(failed, tc.failed) {
				t.Fatalf("failed cases %v, want %v", failed, tc.failed)
			}
			r := v.Reqs
			if !r.RunSignature || !r.TableTests || !r.TestCallsRun || len(r.FailedTests) != 0 || !slices.Equal(r.MissingTests, tc.missingTests) || len(r.Rows) != len(FeatureCases) {
				t.Fatalf("requirements %+v", r)
			}
			if !requirementsEqual(r, buildRequirements(files, v.TestJSON, v.Cases.Cases, r.TestRun, r.Commit)) {
				t.Fatal("the stored map is not its pure recomputation")
			}
		})
	}
}

// TestValidationUnits (UT-12): case evaluation per kind, the source
// structure checks, the go test -json map (missing, failed, skipped and
// misnamed subtests), the exhausted budget (nothing executed) and an
// export of an unexpected file (nothing executed).
func TestValidationUnits(t *testing.T) {
	t.Parallel()
	started := ExecRecord{Started: true}
	exit2 := ExecRecord{Started: true, ExitCode: 2}
	for _, tc := range []struct {
		c              FeatureCase
		exec           ExecRecord
		stdout, stderr string
		pass           bool
		code           string
	}{
		{FeatureCases[0], started, "Hello, world!\n", "", true, ""},
		{FeatureCases[0], started, "Hello, world!\n", "warn\n", false, "unexpected_result"},
		{FeatureCases[3], started, "Hello, ??!\n", "", false, "unexpected_result"},
		{FeatureCases[5], exit2, "", "bad\n", true, ""},
		{FeatureCases[5], exit2, "Hello, !\n", "bad\n", false, "unexpected_result"},
		{FeatureCases[5], exit2, "", " \n", false, "unexpected_result"},
		{FeatureCases[9], started, "", "Usage: -name string\n", true, ""},
		{FeatureCases[9], started, "usage\n", "", false, "unexpected_result"},
		{FeatureCases[0], ExecRecord{}, "", "", false, "case_not_executed"},
		{FeatureCases[0], ExecRecord{Started: true, TimedOut: true}, "", "", false, "case_not_executed"},
		{FeatureCases[0], ExecRecord{Started: true, Truncated: true}, "", "", false, "case_not_executed"},
		{FeatureCases[0], ExecRecord{Started: true, Signal: "killed"}, "", "", false, "case_not_executed"},
	} {
		if pass, code := evaluateCase(tc.c, tc.exec, tc.stdout, tc.stderr); pass != tc.pass || code != tc.code {
			t.Errorf("%s %+v %q %q = %v %q", tc.c.ID, tc.exec, tc.stdout, tc.stderr, pass, code)
		}
	}
	// Source structure.
	for src, want := range map[string][]string{
		fixtureMain: nil,
		"package main\n\nfunc run(args []string, stdout io.Writer) int { return 0 }\n":                      {"run_signature_mismatch"},
		"package main\n\nfunc run(args []string, stdout, stderr io.Writer) {}\n":                            {"run_signature_mismatch"},
		"package main\n\nfunc run(args []string, stdout, stderr *os.File) int { return 0 }\n":               {"run_signature_mismatch"},
		"package main\n\nfunc (x T) run(args []string, stdout, stderr io.Writer) int { return 0 }\n":        {"run_missing"},
		"package main\n\nfunc main() {}\n":                                                                  {"run_missing"},
		"package main\n\nfunc run(\n":                                                                       {"main_go_unparsable"},
		"package main\n\nfunc run(args [2]string, stdout, stderr io.Writer) (code int) { return 0 }\n":      {"run_signature_mismatch"},
		"package main\n\nfunc run(args []string, stdout, stderr io.Writer) (code int) { return 0 }\n":       nil,
		"package main\n\nfunc run(args []string, stdout, stderr io.Writer) (a, b int) { return 0, 0 }\n":    {"run_signature_mismatch"},
		"package main\n\nfunc run(args []string, stdout, stderr io.Writer) map[string]int { return nil }\n": {"run_signature_mismatch"},
	} {
		ok, f := runSignature([]byte(src), nil)
		if ok != (want == nil) || !slices.Equal(f, want) {
			t.Errorf("runSignature(%q) = %v %v, want %v", src, ok, f, want)
		}
	}
	for src, want := range map[string][]string{
		fixtureTest: nil,
		"package main\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) { run(nil, nil, nil) }\n":                          {"table_loop_missing"},
		"package main\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) { for range []int{1} {} }\n":                       {"run_not_called"},
		"package main\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) { for range []int{1} { run(nil, nil, nil) } }\n": {"test_run_missing"},
		"package main\n\nfunc TestRun(": {"main_test_go_unparsable"},
	} {
		loop, calls, f := tableTests([]byte(src), nil)
		if (loop && calls) != (want == nil) || !slices.Equal(f, want) {
			t.Errorf("tableTests(%q) = %v %v %v, want %v", src, loop, calls, f, want)
		}
	}
	// The go test -json map.
	f := newFakeWorld(t)
	f.testActions = map[string]string{"spaces": "skip", "unicode": "fail", "help": ""}
	files := fixtureFiles("1.26.0", fixtureMain, fixtureTest)
	r := buildRequirements(files, []byte(f.testJSON()+"not json\n"), nil, ExecRecord{}, "c")
	if !slices.Equal(r.MissingTests, []string{"TestRun/help"}) || !slices.Equal(r.FailedTests, []string{"TestRun/spaces", "TestRun/unicode"}) ||
		r.Rows[9].MappingFound || r.Rows[2].TestResult != "skip" || len(r.Rows[0].TestLines) != 2 || r.Rows[0].RequestSHA != sha256Hex([]byte(FeatureRequest)) {
		t.Fatalf("requirements %+v", r)
	}
	// An exhausted budget executes nothing: every command times out
	// unstarted and every case is unexecuted.
	clock := newAutoClock()
	ran := 0
	never := func(s ProcSpec, timeout time.Duration, limit int) (CommandResult, error) {
		ran++
		clock.advance(2 * FinalTestBound)
		return CommandResult{Duration: 2 * FinalTestBound, TimedOut: true}, nil
	}
	v := runFinalValidation(never, clock, fakeGo, "/fake/bin", t.TempDir(), files, "c", "t", seqMark())
	if ran != 1 || v.Cases.Build.Started || !v.Cases.Build.TimedOut || v.Cases.Cases[0].Pass || v.Cases.Cases[0].Code != "case_not_executed" || !v.Reqs.TestRun.TimedOut {
		t.Fatalf("exhausted budget: ran %d %+v", ran, v.Cases)
	}
	// An unexpected file in the tree: nothing is executed.
	bad := withFiles(files, map[string][]byte{"Makefile": []byte("all:\n")})
	v = runFinalValidation(func(ProcSpec, time.Duration, int) (CommandResult, error) {
		t.Fatal("a command ran for an unexpected tree")
		return CommandResult{}, nil
	}, clock, fakeGo, "/fake/bin", t.TempDir(), bad, "c", "t", seqMark())
	if v.Err == nil || len(v.Reqs.Rows) != len(FeatureCases) || len(v.Cases.Cases) != 0 {
		t.Fatalf("unexpected tree: %+v", v)
	}
	if got := offlineEnv(testEnv("/d", "/bin")); !slices.Equal(got, finalTestEnv) {
		t.Fatalf("offline env %v", got)
	}
	if exprString(&ast.StarExpr{X: ast.NewIdent("T")}) != "*T" || exprString(&ast.ArrayType{Len: ast.NewIdent("N"), Elt: ast.NewIdent("T")}) != "?" {
		t.Fatal("pointer rendering")
	}
}

// testCollectorValidation (FP-7) runs the live collector's final
// validation through complete offline attempts on fixture commits, then
// checks the bundles: a coder missing a requirement (a wrong Unicode
// case, an accepted empty name) or a test mapping defect (missing,
// skipped, failed or misnamed subtest) fails result_correctness with its
// fixed code, the check executing nothing.
func testCollectorValidation(t *testing.T) {
	// otherFailure is a failing test outside the ten required subtests and
	// its failing package; the go test -json run exits 1.
	otherFailure := func(f *fakeWorld) {
		f.jsonExtra = `{"Action":"run","Package":"example.com/greeting/util","Test":"TestOther"}` + "\n" +
			`{"Action":"fail","Package":"example.com/greeting/util","Test":"TestOther","Elapsed":0}` + "\n" +
			`{"Action":"fail","Package":"example.com/greeting/util","Elapsed":0}` + "\n"
		f.jsonExit = 1
	}
	for name, tc := range map[string]struct {
		set   func(f *fakeWorld)
		fail  string
		codes map[string]string
	}{
		"missing unicode": {func(f *fakeWorld) { f.cases["unicode"] = behavior{stdout: "Hello, ??!\n"} }, CodeCaseFailed, map[string]string{CritResult: CodeCaseFailed}},
		"missing error":   {func(f *fakeWorld) { f.cases["empty_name"] = behavior{stdout: "Hello, !\n"} }, CodeCaseFailed, map[string]string{CritResult: CodeCaseFailed}},
		"missing subtest": {func(f *fakeWorld) { f.testActions["unicode"] = "" }, CodeTestMapping, map[string]string{CritResult: CodeTestMapping}},
		"skipped subtest": {func(f *fakeWorld) { f.testActions["help"] = "skip" }, CodeTestMapping, map[string]string{CritResult: CodeTestMapping}},
		"failed subtest":  {func(f *fakeWorld) { f.testActions["positional"] = "fail" }, CodeTestMapping, map[string]string{CritResult: CodeReviewContra}},
		// The ten required subtests pass but another test and package
		// fail: the run fails and the approving review contradicts it.
		"other test failing": {otherFailure, CodeValidationFail,
			map[string]string{CritFinalTests: CodeValidationFail, CritResult: CodeReviewContra}},
		// A clean stream but a failing exit (or an unparseable line) is
		// no successful JSON run.
		"json run exit": {func(f *fakeWorld) { f.jsonExit = 1 }, CodeValidationFail,
			map[string]string{CritFinalTests: CodeValidationFail, CritResult: CodeReviewContra}},
		"json run malformed": {func(f *fakeWorld) { f.jsonExtra = "FAIL\texample.com/greeting [build failed]\n" }, CodeValidationFail,
			map[string]string{CritFinalTests: CodeValidationFail, CritResult: CodeReviewContra}},
		// W3: a failed build keeps its output and names it.
		"build failed": {func(f *fakeWorld) { f.build = behavior{stdout: "building\n", stderr: buildVCSError, exit: 1} }, CodeCaseFailed,
			map[string]string{CritFinalTests: "validation_binding_mismatch"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			tc.set(f)
			if code := f.attempt(); code != 1 || f.sup.failCode != tc.fail {
				t.Fatalf("attempt = %d %q, want %s", code, f.sup.failCode, tc.fail)
			}
			code, r, _ := checkBundle(t, f.bundle())
			for crit, want := range tc.codes {
				requireCode(t, name, code, r, crit, want)
			}
			if name == "build failed" {
				if !strings.Contains(f.stderr.String(), "the CLI build exited 1 (timed out false); "+buildStderrFile+": error obtaining VCS status: exit status 128") {
					t.Fatalf("the failure does not show the build output:\n%s", f.stderr.String())
				}
				if string(mustRead(t, filepath.Join(f.bundle(), buildStderrFile))) != buildVCSError ||
					string(mustRead(t, filepath.Join(f.bundle(), buildStdoutFile))) != "building\n" {
					t.Fatal("the build output was not retained")
				}
			}
		})
	}
}

// buildVCSError is the build failure an invalid Git ancestor caused.
const buildVCSError = "error obtaining VCS status: exit status 128\n\tUse -buildvcs=false to disable VCS stamping.\n"
