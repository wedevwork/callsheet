package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Deterministic feature checks (design 12a-real-e2e, amendment A3, FP-7):
// after the fourth publication the collector runs, within the one shared
// final-validation budget (FinalTestBound), the existing go test run, a
// build of the CLI into the runtime, the ten fixed argv cases run directly
// (never through a shell) and go test -json; it records every command's
// argv, input commit and tree, output hashes, start/end sequence, exit and
// deadline, and generates the requirement-to-test map from the fixed case
// IDs. The pure parts (case expectations, the map and the source checks)
// are shared with the offline checker, which re-derives them from the
// stored evidence and never executes anything.

// FeatureCase is one fixed CLI case.
type FeatureCase struct {
	ID     string
	Args   []string
	Exit   int
	Stdout string
	// kind: "ok" (exact stdout, empty stderr), "error" (empty stdout,
	// nonempty stderr), "help" (nonempty usage mentioning name).
	Kind string
}

// FeatureCases are the fixed cases, in order; each requires the test
// TestRun/<ID>.
var FeatureCases = []FeatureCase{
	{ID: "no_args", Args: []string{}, Exit: 0, Stdout: "Hello, world!\n", Kind: "ok"},
	{ID: "name", Args: []string{"--name", "Ada"}, Exit: 0, Stdout: "Hello, Ada!\n", Kind: "ok"},
	{ID: "spaces", Args: []string{"--name", "Ada Lovelace"}, Exit: 0, Stdout: "Hello, Ada Lovelace!\n", Kind: "ok"},
	{ID: "unicode", Args: []string{"--name", "世界"}, Exit: 0, Stdout: "Hello, 世界!\n", Kind: "ok"},
	{ID: "whitespace", Args: []string{"--name", " "}, Exit: 0, Stdout: "Hello,  !\n", Kind: "ok"},
	{ID: "empty_name", Args: []string{"--name", ""}, Exit: 2, Kind: "error"},
	{ID: "unknown_flag", Args: []string{"--unknown"}, Exit: 2, Kind: "error"},
	{ID: "missing_value", Args: []string{"--name"}, Exit: 2, Kind: "error"},
	{ID: "positional", Args: []string{"Ada"}, Exit: 2, Kind: "error"},
	{ID: "help", Args: []string{"--help"}, Exit: 0, Kind: "help"},
}

// Validation codes.
const (
	CodeCaseFailed     = "feature_case_failed"
	CodeTestMapping    = "test_mapping_missing"
	CodeValidationFail = "final_validation_failed"
	CodeReviewContra   = "review_test_contradiction"
)

// maxCaseOutput bounds a case's stored stdout and stderr each.
const maxCaseOutput = 4 << 10

// evaluateCase judges one case's observed result.
func evaluateCase(c FeatureCase, exec ExecRecord, stdout, stderr string) (bool, string) {
	if !exec.Started || exec.TimedOut || exec.Signal != "" || exec.Truncated {
		return false, "case_not_executed"
	}
	switch c.Kind {
	case "ok":
		if exec.ExitCode == 0 && stdout == c.Stdout && stderr == "" {
			return true, ""
		}
	case "error":
		if exec.ExitCode == 2 && stdout == "" && strings.TrimSpace(stderr) != "" {
			return true, ""
		}
	case "help":
		if exec.ExitCode == 0 && strings.Contains(stdout+stderr, "name") && strings.TrimSpace(stdout+stderr) != "" {
			return true, ""
		}
	}
	return false, "unexpected_result"
}

// validationEnv returns the offline Go environment of dir.
func validationEnv(dir, path string) []string { return testEnv(dir, path) }

// validator runs final validation within one deadline.
type validator struct {
	run      commandRunner
	clock    Clock
	deadline time.Time
	env      []string
	src      string
	commit   string
	tree     string
	mark     func(typ, name string) int
	// err is the first command that could not be run to a verified end
	// (a start failure or a group that could not be proved gone).
	err error
}

// exec runs one command within the remaining budget and records it.
func (v *validator) exec(name, path string, logical, args []string, env []string, limit int) (ExecRecord, CommandResult) {
	rec := ExecRecord{Schema: ExecSchema, Name: name, Argv: logical, Commit: v.commit, Tree: v.tree}
	left := v.deadline.Sub(v.clock.Now())
	rec.DeadlineMS = left.Milliseconds()
	if left <= 0 {
		rec.TimedOut = true
		return rec, CommandResult{}
	}
	rec.StartSeq = v.mark(EvValidationStarted, name)
	res, err := v.run(ProcSpec{Path: path, Args: args, Env: env, Dir: v.src}, left, limit)
	rec.EndSeq = v.mark(EvValidationFinished, name)
	rec.Started = err == nil || res.Proc != nil
	if err != nil && v.err == nil {
		v.err = fmt.Errorf("%s: %w", name, err)
	}
	rec.ExitCode, rec.Signal, rec.TimedOut, rec.DurationMS, rec.Truncated = res.ExitCode, res.Signal, res.TimedOut, res.Duration.Milliseconds(), res.Truncated
	rec.StdoutSHA256, rec.StderrSHA256 = sha256Hex(res.Stdout), sha256Hex(res.Stderr)
	rec.StdoutBytes, rec.StderrBytes = len(res.Stdout), len(res.Stderr)
	return rec, res
}

// finalValidation is the outcome of the collector's final validation.
type finalValidation struct {
	Test       TestRun
	TestOut    []byte
	TestErr    []byte
	Cases      CasesRecord
	TestJSON   []byte
	TestJSONEr []byte
	// BuildOut and BuildErr are the build's output, each within
	// maxBuildOutput.
	BuildOut []byte
	BuildErr []byte
	Reqs     RequirementsRecord
	// Cover, CoverOut, CoverErr and Profile are the development path's
	// coverage run (design 12a-dev-flow-verify, Candidate oracle): its
	// record, output and the profile it wrote (nil when it wrote none).
	Cover    *ExecRecord
	CoverOut []byte
	CoverErr []byte
	Profile  []byte
	Err      error
}

// maxBuildOutput bounds each retained build output stream.
const maxBuildOutput = 1 << 16

// maxProfileBytes bounds a coverage profile read back.
const maxProfileBytes = 1 << 20

// validationArgvCover is the development final validation's coverage run
// ($PROFILE: the profile's absolute path outside the exported source).
var validationArgvCover = []string{"go", "test", "-buildvcs=false", "-count=1", "-covermode=set", "-coverprofile=$PROFILE", "./..."}

// runFinalValidation exports files into dir and runs every final
// validation command within FinalTestBound from now.
func runFinalValidation(run commandRunner, clock Clock, goBin, pathEnv, dir string, files map[string][]byte, commit, tree string,
	mark func(typ, name string) int) finalValidation {
	return runFinalValidationWith(validationOptions{run: run, clock: clock, goBin: goBin, pathEnv: pathEnv, dir: dir, files: files, commit: commit,
		tree: tree, mark: mark, deadline: clock.Now().Add(FinalTestBound), allowed: projectFiles})
}

// validationOptions are one final validation's inputs: the explicit
// shared deadline, the tree policy of the exported files and, for the
// development path, the coverage run.
type validationOptions struct {
	run          commandRunner
	clock        Clock
	goBin        string
	pathEnv      string
	dir          string
	files        map[string][]byte
	commit, tree string
	mark         func(typ, name string) int
	deadline     time.Time
	allowed      map[string]bool
	cover        bool
}

// runFinalValidationWith exports files into dir and runs every final
// validation command within the shared deadline.
func runFinalValidationWith(o validationOptions) finalValidation {
	run, clock, goBin, pathEnv, dir, files, commit, tree, mark := o.run, o.clock, o.goBin, o.pathEnv, o.dir, o.files, o.commit, o.tree, o.mark
	out := finalValidation{Cases: CasesRecord{Schema: CasesSchema, Commit: commit, Tree: tree, Cases: []CaseResult{}}}
	deadline := o.deadline
	src, env, err := exportProjectAllowed(dir, files, pathEnv, o.allowed)
	out.Test = TestRun{Schema: TestRunSchema, Argv: append([]string(nil), testArgv...), Commit: commit, Tree: tree, Files: sortedKeys(files), Env: offlineEnv(env)}
	if err != nil {
		out.Err = err
		out.Reqs = buildRequirements(files, nil, out.Cases.Cases, ExecRecord{Schema: ExecSchema, Name: "test_json", Commit: commit, Tree: tree}, commit)
		return out
	}
	v := &validator{run: run, clock: clock, deadline: deadline, env: env, src: src, commit: commit, tree: tree, mark: mark}
	verRec, ver := v.exec("go_version", goBin, []string{"go", "version"}, []string{"version"}, env, 4096)
	out.Test.Toolchain = strings.TrimSpace(string(ver.Stdout))
	testRec, test := v.exec("go_test", goBin, testArgv, testArgv[1:], env, 1<<20)
	out.Test.ExitCode, out.Test.Signal, out.Test.TimedOut, out.Test.DurationMS = testRec.ExitCode, testRec.Signal, testRec.TimedOut, testRec.DurationMS
	if !verRec.Started || !testRec.Started {
		out.Test.ExitCode = -1
	}
	out.TestOut, out.TestErr = test.Stdout, test.Stderr
	bin := filepath.Join(dir, "bin", "greeting")
	buildArgs := slices.Clone(validationArgvBuild[1:])
	buildArgs[slices.Index(buildArgs, "$BIN")] = bin
	var build CommandResult
	out.Cases.Build, build = v.exec("go_build", goBin, validationArgvBuild, buildArgs, env, maxBuildOutput)
	out.BuildOut, out.BuildErr = build.Stdout, build.Stderr
	built := out.Cases.Build.Started && out.Cases.Build.ExitCode == 0 && !out.Cases.Build.TimedOut
	caseEnv := []string{"PATH=" + pathEnv, "HOME=" + filepath.Join(dir, "home"), "TMPDIR=" + filepath.Join(dir, "tmp")}
	for _, c := range FeatureCases {
		r := CaseResult{ID: c.ID, Exec: ExecRecord{Schema: ExecSchema, Name: "case_" + c.ID, Argv: append([]string{"greeting"}, c.Args...), Commit: commit, Tree: tree}}
		if built {
			var res CommandResult
			r.Exec, res = v.exec("case_"+c.ID, bin, append([]string{"greeting"}, c.Args...), c.Args, caseEnv, maxCaseOutput)
			r.Stdout, r.Stderr = toValidUTF8(res.Stdout), toValidUTF8(res.Stderr)
		}
		r.Pass, r.Code = evaluateCase(c, r.Exec, r.Stdout, r.Stderr)
		out.Cases.Cases = append(out.Cases.Cases, r)
	}
	jsonRec, tj := v.exec("go_test_json", goBin, validationArgvJSON, validationArgvJSON[1:], env, 4<<20)
	out.TestJSON, out.TestJSONEr = tj.Stdout, tj.Stderr
	out.Reqs = buildRequirements(files, out.TestJSON, out.Cases.Cases, jsonRec, commit)
	if o.cover {
		profile := filepath.Join(dir, "cover", "coverage.out")
		args := slices.Clone(validationArgvCover[1:])
		if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil && v.err == nil {
			v.err = err
		}
		args[slices.Index(args, "-coverprofile=$PROFILE")] = "-coverprofile=" + profile
		rec, res := v.exec("go_test_cover", goBin, validationArgvCover, args, env, 1<<20)
		out.Cover, out.CoverOut, out.CoverErr = &rec, res.Stdout, res.Stderr
		if b, err := readBounded(profile, maxProfileBytes); err == nil {
			out.Profile = b
		}
	}
	out.Err = v.err
	return out
}

// offlineEnv keeps the four offline Go settings of an environment.
func offlineEnv(env []string) []string {
	var out []string
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == "GOTOOLCHAIN" || k == "GOPROXY" || k == "GOSUMDB" || k == "GOWORK" {
			out = append(out, e)
		}
	}
	return out
}

// testEvent is one go test -json event.
type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

// testRunVerdict judges the whole go test -json run, not only the ten
// required subtests: it passed only when the command ran to a clean,
// untruncated exit 0 and every stream line is a test event with no
// failing test or package. failures names each failing test and package.
func testRunVerdict(rec ExecRecord, stream []byte) (bool, []string) {
	wellFormed := true
	seen := map[string]bool{}
	failures := []string{}
	for _, l := range strings.Split(string(stream), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var e testEvent
		if json.Unmarshal([]byte(l), &e) != nil || e.Action == "" {
			wellFormed = false
			continue
		}
		if e.Action != "fail" {
			continue
		}
		name := "package " + e.Package
		if e.Test != "" {
			name = e.Package + " " + e.Test
		}
		if !seen[name] {
			seen[name] = true
			failures = append(failures, name)
		}
	}
	sort.Strings(failures)
	passed := rec.Started && rec.ExitCode == 0 && rec.Signal == "" && !rec.TimedOut && !rec.Truncated && wellFormed && len(failures) == 0
	return passed, failures
}

// testResults maps each test name to its final action and line span in a
// go test -json stream.
func testResults(data []byte) map[string]struct {
	action      string
	first, last int
} {
	out := map[string]struct {
		action      string
		first, last int
	}{}
	for i, l := range strings.Split(string(data), "\n") {
		var e testEvent
		if l == "" || json.Unmarshal([]byte(l), &e) != nil || e.Test == "" {
			continue
		}
		r := out[e.Test]
		if r.first == 0 {
			r.first = i + 1
		}
		r.last = i + 1
		switch e.Action {
		case "pass", "fail", "skip":
			r.action = e.Action
		}
		out[e.Test] = r
	}
	return out
}

// buildRequirements derives the requirement-to-test map and the source
// structure findings (pure: the checker recomputes it from evidence).
func buildRequirements(files map[string][]byte, testJSON []byte, cases []CaseResult, testRun ExecRecord, commit string) RequirementsRecord {
	r := RequirementsRecord{Schema: RequirementsSchema, Commit: commit, TestRun: testRun, Rows: []RequirementRow{}, MissingTests: []string{},
		FailedTests: []string{}, ParseFindings: []string{}}
	r.RunSignature, r.ParseFindings = runSignature(files["main.go"], r.ParseFindings)
	r.TableTests, r.TestCallsRun, r.ParseFindings = tableTests(files["main_test.go"], r.ParseFindings)
	r.TestRunPassed, r.TestFailures = testRunVerdict(testRun, testJSON)
	results := testResults(testJSON)
	byID := map[string]CaseResult{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	for _, c := range FeatureCases {
		name := "TestRun/" + c.ID
		tr, found := results[name]
		row := RequirementRow{CaseID: c.ID, RequestPath: "FEATURE.md", RequestSHA: sha256Hex(files["FEATURE.md"]), DesignPath: "design.md",
			DesignSHA: sha256Hex(files["design.md"]), MainSHA: sha256Hex(files["main.go"]), TestSHA: sha256Hex(files["main_test.go"]),
			TestName: name, TestLines: []int{}, TestResult: "missing", CLICase: c.ID, CLIPass: byID[c.ID].Pass, MappingFound: found}
		if found {
			row.TestLines, row.TestResult = []int{tr.first, tr.last}, tr.action
			if tr.action != "pass" {
				r.FailedTests = append(r.FailedTests, name)
			}
		} else {
			r.MissingTests = append(r.MissingTests, name)
		}
		r.Rows = append(r.Rows, row)
	}
	return r
}

// runSignature reports func run(args []string, stdout, stderr io.Writer) int.
func runSignature(src []byte, findings []string) (bool, []string) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		return false, append(findings, "main_go_unparsable")
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "run" {
			continue
		}
		var params []string
		for _, p := range fn.Type.Params.List {
			n := max(1, len(p.Names))
			for i := 0; i < n; i++ {
				params = append(params, exprString(p.Type))
			}
		}
		res := fn.Type.Results
		ok = len(params) == 3 && params[0] == "[]string" && params[1] == "io.Writer" && params[2] == "io.Writer" &&
			res != nil && len(res.List) == 1 && len(res.List[0].Names) <= 1 && exprString(res.List[0].Type) == "int"
		if ok {
			return true, findings
		}
		return false, append(findings, "run_signature_mismatch")
	}
	return false, append(findings, "run_missing")
}

// tableTests reports a TestRun(t *testing.T) with a range loop (the
// table) and a call to run.
func tableTests(src []byte, findings []string) (bool, bool, []string) {
	f, err := parser.ParseFile(token.NewFileSet(), "main_test.go", src, 0)
	if err != nil {
		return false, false, append(findings, "main_test_go_unparsable")
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestRun" || fn.Body == nil {
			continue
		}
		loop, calls := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.RangeStmt:
				loop = true
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "run" {
					calls = true
				}
			}
			return true
		})
		if !loop {
			findings = append(findings, "table_loop_missing")
		}
		if !calls {
			findings = append(findings, "run_not_called")
		}
		return loop, calls, findings
	}
	return false, false, append(findings, "test_run_missing")
}

// exprString renders a type expression.
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		if x.Len == nil {
			return "[]" + exprString(x.Elt)
		}
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	}
	return "?"
}

// requirementsEqual compares two maps exactly.
func requirementsEqual(a, b RequirementsRecord) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
