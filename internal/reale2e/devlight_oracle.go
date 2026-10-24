package reale2e

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The light oracle (design 12a-dev-flow-light, Light oracle; FP-4). Three
// distinct evidence producers exist: the supervisor's seed baseline (before
// the first workflow task), the executing reviewer, and the supervisor's
// final validation (after an approving light review's consumed wait and
// clean phase exit). Every command is a fixed recipe run in a fresh private
// export with the isolated offline Go environment; no report-supplied
// command or test expression is ever executed. Regression red/green uses
// the candidate's whole main_test.go byte-identically over the seed source
// (before) and over the candidate source (after), judged from the go test
// -json event stream, never from prose or assertion wording.

// The light oracle's pinned commands (shared by the supervisor, the
// reviewer helper and the replay derivation; $PROFILE and $BIN are fresh
// scratch paths).
var (
	lightArgvVersion = []string{"go", "version"}
	lightArgvSuite   = []string{"go", "test", "-count=1", "./..."}
	lightArgvJSON    = []string{"go", "test", "-count=1", "-json", "./..."}
	lightArgvCover   = []string{"go", "test", "-count=1", "-covermode=set", "-coverprofile=$PROFILE", "./..."}
	lightArgvBuild   = validationArgvBuild
)

// Light oracle codes.
const (
	CodeLightSeedInvalid      = "light_seed_invalid"
	CodeRegressionNotRed      = "regression_not_red"
	CodeRegressionInvalidRed  = "regression_invalid_red"
	CodeRegressionNotGreen    = "regression_not_green"
	CodeRegressionStructure   = "regression_structure_invalid"
	CodeRegressionInputs      = "regression_inputs_changed"
	lightWhitespaceCase       = "whitespace"
	lightWhitespaceStderr     = "greeting: --name must not be empty\n"
	lightBaselineBound        = 60 * time.Second
	lightJSONLimit            = 4 << 20
	lightStreamLimit          = 1 << 20
	lightRegressionInputLimit = 32 << 10
)

// lightSeedCases are the nine seed TestRun cases (the ten feature cases
// without whitespace), in order.
func lightSeedCases() []string {
	var out []string
	for _, c := range FeatureCases {
		if c.ID != lightWhitespaceCase {
			out = append(out, c.ID)
		}
	}
	return out
}

// LightInput is one exported input's path and digest.
type LightInput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// lightInputs digests files in path order.
func lightInputs(files map[string][]byte) []LightInput {
	out := []LightInput{}
	for _, n := range sortedKeys(files) {
		out = append(out, LightInput{Path: n, SHA256: sha256Hex(files[n])})
	}
	return out
}

// readBackInputs re-digests the exported files of src after the commands
// (a missing or unreadable file digests as empty).
func readBackInputs(src string, files map[string][]byte) []LightInput {
	out := []LightInput{}
	for _, n := range sortedKeys(files) {
		b, err := readBounded(filepath.Join(src, n), lightRegressionInputLimit)
		sum := ""
		if err == nil {
			sum = sha256Hex(b)
		}
		out = append(out, LightInput{Path: n, SHA256: sum})
	}
	return out
}

// LightTestEvents is the judgment-relevant summary of one go test -json
// stream: every leaf test's single final action, the TestRun parent's and
// the package's, and the stream's failure kinds.
type LightTestEvents struct {
	Pass      []string `json:"pass"`
	Fail      []string `json:"fail"`
	Skip      []string `json:"skip"`
	Duplicate []string `json:"duplicate"`
	Parent    string   `json:"parent"`
	Package   string   `json:"package"`
	Malformed bool     `json:"malformed"`
	Build     bool     `json:"build_failed"`
	Panic     bool     `json:"panic"`
	Timeout   bool     `json:"timeout"`
}

// lightEvent is one go test -json event's judged fields.
type lightEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	Output      string `json:"Output"`
	FailedBuild string `json:"FailedBuild"`
	ImportPath  string `json:"ImportPath"`
}

// lightTestEventsOf summarizes stream: a test with subtests is a parent,
// every other test a leaf; a leaf with more than one final action is a
// duplicate.
func lightTestEventsOf(stream []byte) LightTestEvents {
	ev := LightTestEvents{Pass: []string{}, Fail: []string{}, Skip: []string{}, Duplicate: []string{}}
	final := map[string][]string{}
	var names []string
	for _, l := range strings.Split(string(stream), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var e lightEvent
		if json.Unmarshal([]byte(l), &e) != nil || e.Action == "" {
			ev.Malformed = true
			continue
		}
		switch {
		case e.Action == "build-fail", e.Action == "build-output", e.FailedBuild != "":
			ev.Build = true
		case strings.HasPrefix(e.Output, "panic: test timed out"):
			ev.Timeout = true
		case strings.HasPrefix(e.Output, "panic:"):
			ev.Panic = true
		}
		if e.Action != "pass" && e.Action != "fail" && e.Action != "skip" {
			continue
		}
		if e.Test == "" {
			ev.Package = e.Action
			continue
		}
		if _, seen := final[e.Test]; !seen {
			names = append(names, e.Test)
		}
		final[e.Test] = append(final[e.Test], e.Action)
	}
	parent := map[string]bool{}
	for _, n := range names {
		if i := strings.LastIndex(n, "/"); i > 0 {
			parent[n[:i]] = true
		}
	}
	for _, n := range names {
		acts := final[n]
		if n == "TestRun" {
			ev.Parent = acts[len(acts)-1]
		}
		if parent[n] {
			continue
		}
		if len(acts) > 1 {
			ev.Duplicate = append(ev.Duplicate, n)
		}
		switch acts[len(acts)-1] {
		case "pass":
			ev.Pass = append(ev.Pass, n)
		case "fail":
			ev.Fail = append(ev.Fail, n)
		case "skip":
			ev.Skip = append(ev.Skip, n)
		}
	}
	for _, s := range [][]string{ev.Pass, ev.Fail, ev.Skip, ev.Duplicate} {
		sort.Strings(s)
	}
	return ev
}

// cleanRun reports a command that ran to a verified, untruncated exit
// without a signal or timeout.
func cleanRun(r ExecRecord) bool {
	return r.Started && !r.TimedOut && r.Signal == "" && !r.Truncated
}

// judgeRed decides the before run (seed source with the candidate's test):
// exit 1, exactly TestRun/whitespace failing among the leaves, the other
// nine required leaves passing, TestRun and the package failing, and no
// build failure, panic, timeout, signal, skip, duplicate, truncation or
// malformed event. A run whose whitespace leaf does not fail is not red; a
// failure of another kind is an invalid red.
func judgeRed(r ExecRecord, ev LightTestEvents) string {
	ws := "TestRun/" + lightWhitespaceCase
	if !slices.Contains(ev.Fail, ws) && cleanRun(r) && r.ExitCode == 0 {
		return CodeRegressionNotRed
	}
	if !slices.Contains(ev.Fail, ws) && slices.Contains(ev.Pass, ws) && cleanRun(r) && !ev.Build && !ev.Panic && !ev.Timeout && !ev.Malformed {
		return CodeRegressionNotRed
	}
	ok := cleanRun(r) && r.ExitCode == 1 && !ev.Malformed && !ev.Build && !ev.Panic && !ev.Timeout && len(ev.Skip) == 0 && len(ev.Duplicate) == 0 &&
		slices.Equal(ev.Fail, []string{ws}) && ev.Parent == "fail" && ev.Package == "fail"
	for _, id := range lightSeedCases() {
		ok = ok && slices.Contains(ev.Pass, "TestRun/"+id)
	}
	if !ok {
		return CodeRegressionInvalidRed
	}
	return ""
}

// judgeGreen decides the after run (candidate source with the same test):
// exit 0 and all ten required leaves, TestRun and the package passing,
// with no failure, skip, duplicate or malformed event.
func judgeGreen(r ExecRecord, ev LightTestEvents) string {
	ok := cleanRun(r) && r.ExitCode == 0 && !ev.Malformed && !ev.Build && !ev.Panic && !ev.Timeout && len(ev.Fail) == 0 && len(ev.Skip) == 0 &&
		len(ev.Duplicate) == 0 && ev.Parent == "pass" && ev.Package == "pass"
	for _, c := range FeatureCases {
		ok = ok && slices.Contains(ev.Pass, "TestRun/"+c.ID)
	}
	if !ok {
		return CodeRegressionNotGreen
	}
	return ""
}

// LightTestStructure is the candidate test's mechanical shape: TestRun
// ranges over a table and calls run, every seed table row is retained
// token for token, and one row carries the whitespace literals.
type LightTestStructure struct {
	Parsed     bool     `json:"parsed"`
	Table      bool     `json:"table"`
	CallsRun   bool     `json:"calls_run"`
	SeedRows   int      `json:"seed_rows"`
	Whitespace bool     `json:"whitespace"`
	Findings   []string `json:"findings"`
}

// ok reports the complete required structure.
func (s LightTestStructure) ok() bool {
	return s.Parsed && s.Table && s.CallsRun && s.SeedRows == len(lightSeedCases()) && s.Whitespace && len(s.Findings) == 0
}

// lightTestStructureOf checks the candidate main_test.go against the seed
// main_test.go mechanically (semantic assertion quality stays a judgment).
func lightTestStructureOf(seedTest, candTest []byte) LightTestStructure {
	s := LightTestStructure{Findings: []string{}}
	seedRows := tableRows(seedTest)
	cand := tableRows(candTest)
	if cand == nil {
		s.Findings = append(s.Findings, "test_table_missing")
		return s
	}
	s.Parsed = true
	s.Table, s.CallsRun, s.Findings = tableTests(candTest, s.Findings)
	have := map[string]int{}
	for _, r := range cand {
		have[r.tokens]++
	}
	for _, r := range seedRows {
		if have[r.tokens] > 0 {
			have[r.tokens]--
			s.SeedRows++
		}
	}
	for _, r := range cand {
		if r.whitespace() {
			s.Whitespace = true
		}
	}
	if s.SeedRows != len(lightSeedCases()) {
		s.Findings = append(s.Findings, "seed_rows_changed")
	}
	if !s.Whitespace {
		s.Findings = append(s.Findings, "whitespace_row_missing")
	}
	return s
}

// tableRow is one TestRun table element: its token stream and its basic
// literal values.
type tableRow struct {
	tokens   string
	literals []string
	args     [][]string
}

// whitespace reports the required whitespace row literals: the case ID,
// the argument list --name " ", exit 0 and stdout "Hello,  !\n".
func (r tableRow) whitespace() bool {
	hasArgs := false
	for _, a := range r.args {
		hasArgs = hasArgs || slices.Equal(a, []string{"--name", " "})
	}
	return hasArgs && slices.Contains(r.literals, `"whitespace"`) && slices.Contains(r.literals, "0") && slices.Contains(r.literals, `"Hello,  !\n"`)
}

// tableRows returns TestRun's table elements (nil when the test or its
// table cannot be found).
func tableRows(src []byte) []tableRow {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main_test.go", src, 0)
	if err != nil {
		return nil
	}
	var out []tableRow
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestRun" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if _, slice := lit.Type.(*ast.ArrayType); !slice || out != nil {
				return true
			}
			elts := []tableRow{}
			for _, e := range lit.Elts {
				el, ok := e.(*ast.CompositeLit)
				if !ok {
					continue
				}
				s, end := fset.Position(el.Pos()), fset.Position(el.End())
				row := tableRow{tokens: tokenStream(src[s.Offset:end.Offset])}
				ast.Inspect(el, func(m ast.Node) bool {
					switch x := m.(type) {
					case *ast.BasicLit:
						row.literals = append(row.literals, x.Value)
					case *ast.Ident:
						if x.Name == "false" || x.Name == "true" || x.Name == "nil" {
							row.literals = append(row.literals, x.Name)
						}
					case *ast.CompositeLit:
						if x == el {
							return true
						}
						var args []string
						for _, a := range x.Elts {
							if b, ok := a.(*ast.BasicLit); ok && b.Kind == token.STRING {
								if v, err := unquote(b.Value); err == nil {
									args = append(args, v)
								}
							}
						}
						row.args = append(row.args, args)
					}
					return true
				})
				elts = append(elts, row)
			}
			if len(elts) > 0 {
				out = elts
			}
			return true
		})
		return out
	}
	return nil
}

// unquote decodes a Go string literal.
func unquote(lit string) (string, error) { return strconv.Unquote(lit) }

// LightDefectObservation is the seed CLI's whitespace result.
type LightDefectObservation struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// Exact reports exit 2, empty stdout and exactly the seeded
	// diagnostic on stderr: the whitespace defect.
	Exact bool `json:"exact"`
}

// DevLightBaseline is validation/light-baseline.json: the supervisor's
// seed baseline before the first workflow task (the light replacement of
// the full seed baseline, not a paid task).
type DevLightBaseline struct {
	Schema      string                 `json:"schema"`
	RunID       string                 `json:"run_id"`
	SeedCommit  string                 `json:"seed_commit"`
	SeedTree    string                 `json:"seed_tree"`
	Module      string                 `json:"module"`
	Inputs      []LightInput           `json:"inputs"`
	InputsAfter []LightInput           `json:"inputs_after"`
	Toolchain   string                 `json:"toolchain"`
	Env         []string               `json:"env"`
	Start       string                 `json:"start"`
	Deadline    string                 `json:"deadline"`
	Version     ExecRecord             `json:"version"`
	Suite       ExecRecord             `json:"suite"`
	JSON        ExecRecord             `json:"json"`
	Cover       ExecRecord             `json:"cover"`
	Build       ExecRecord             `json:"build"`
	Cases       []CaseResult           `json:"cases"`
	Tests       LightTestEvents        `json:"tests"`
	Coverage    *LightCoverage         `json:"coverage"`
	Defect      LightDefectObservation `json:"defect"`
	Valid       bool                   `json:"valid"`
	Code        string                 `json:"code"`
	Error       string                 `json:"error"`
}

// lightBaselineRun is the baseline's record and its retained streams.
type lightBaselineRun struct {
	rec                       DevLightBaseline
	suiteOut, suiteErr, jsonS []byte
	profile                   []byte
}

// lightRunOptions are one light oracle run's inputs.
type lightRunOptions struct {
	run          commandRunner
	clock        Clock
	goBin        string
	pathEnv      string
	dir          string
	commit, tree string
	deadline     time.Time
	mark         func(typ, name string) int
}

// lightSourceFiles keeps a tree's four source fixture files.
func lightSourceFiles(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for _, n := range devSourceFiles {
		if b, ok := files[n]; ok {
			out[n] = b
		}
	}
	return out
}

// runLightBaseline exports the seed and runs, within the deadline, the
// pinned go version, ordinary suite, JSON run, coverage, build and the ten
// CLI cases, then judges the defect: the nine seed tests pass, the nine
// unaffected CLI cases pass and whitespace exits 2 with no stdout and
// exactly the seeded diagnostic. Anything else is light_seed_invalid.
func runLightBaseline(o lightRunOptions, seed map[string][]byte) lightBaselineRun {
	files := lightSourceFiles(seed)
	out := lightBaselineRun{rec: DevLightBaseline{Schema: devKind("light-baseline"), SeedCommit: o.commit, SeedTree: o.tree,
		Module: modulePathOf(files["go.mod"]), Inputs: lightInputs(files), InputsAfter: []LightInput{}, Env: []string{}, Cases: []CaseResult{},
		Start: formatTime(o.clock.Now()), Deadline: formatTime(o.deadline)}}
	rec := &out.rec
	src, env, err := exportProjectAllowed(o.dir, files, o.pathEnv, devTreePolicy())
	if err != nil {
		rec.Code, rec.Error = CodeLightSeedInvalid, "export: "+err.Error()
		return out
	}
	rec.Env = offlineEnv(env)
	v := &validator{run: o.run, clock: o.clock, deadline: o.deadline, env: env, src: src, commit: o.commit, tree: o.tree, mark: o.mark}
	var res CommandResult
	rec.Version, res = v.exec("light_baseline_version", o.goBin, lightArgvVersion, lightArgvVersion[1:], env, 4096)
	rec.Toolchain = strings.TrimSpace(string(res.Stdout))
	rec.Suite, res = v.exec("light_baseline_suite", o.goBin, lightArgvSuite, lightArgvSuite[1:], env, lightStreamLimit)
	out.suiteOut, out.suiteErr = res.Stdout, res.Stderr
	rec.JSON, res = v.exec("light_baseline_json", o.goBin, lightArgvJSON, lightArgvJSON[1:], env, lightJSONLimit)
	out.jsonS = res.Stdout
	rec.Tests = lightTestEventsOf(res.Stdout)
	profile := filepath.Join(o.dir, "cover", "coverage.out")
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil && v.err == nil {
		v.err = err
	}
	args := slices.Clone(lightArgvCover[1:])
	args[slices.Index(args, "-coverprofile=$PROFILE")] = "-coverprofile=" + profile
	rec.Cover, _ = v.exec("light_baseline_cover", o.goBin, lightArgvCover, args, env, lightStreamLimit)
	if b, err := readBounded(profile, maxProfileBytes); err == nil {
		out.profile = b
		if c, err := deriveWholeCoverage(files["main.go"], b, rec.Module); err == nil {
			rec.Coverage = &c
		}
	}
	rec.Build, rec.Cases, _, _ = lightBuildCases(v, o, env)
	rec.InputsAfter = readBackInputs(src, files)
	for _, c := range rec.Cases {
		if c.ID == lightWhitespaceCase {
			rec.Defect = LightDefectObservation{Exit: c.Exec.ExitCode, Stdout: c.Stdout, Stderr: c.Stderr,
				Exact: cleanRun(c.Exec) && c.Exec.ExitCode == 2 && c.Stdout == "" && c.Stderr == lightWhitespaceStderr}
		}
	}
	rec.Code = lightBaselineVerdict(*rec, v.err)
	rec.Valid = rec.Code == ""
	if v.err != nil {
		rec.Error = v.err.Error()
	}
	return out
}

// lightBaselineVerdict judges a baseline record ("" valid).
func lightBaselineVerdict(rec DevLightBaseline, err error) string {
	ok := err == nil && cleanRun(rec.Version) && rec.Version.ExitCode == 0 && cleanRun(rec.Suite) && rec.Suite.ExitCode == 0 &&
		cleanRun(rec.JSON) && rec.JSON.ExitCode == 0 && rec.Tests.Package == "pass" && rec.Tests.Parent == "pass" && len(rec.Tests.Fail) == 0 &&
		len(rec.Tests.Skip) == 0 && len(rec.Tests.Duplicate) == 0 && !rec.Tests.Malformed && cleanRun(rec.Cover) && rec.Cover.ExitCode == 0 &&
		rec.Coverage != nil && cleanRun(rec.Build) && rec.Build.ExitCode == 0 && len(rec.Cases) == len(FeatureCases) && rec.Defect.Exact &&
		slices.Equal(rec.Inputs, rec.InputsAfter)
	for _, id := range lightSeedCases() {
		ok = ok && slices.Contains(rec.Tests.Pass, "TestRun/"+id)
	}
	ok = ok && !slices.Contains(rec.Tests.Pass, "TestRun/"+lightWhitespaceCase) && !slices.Contains(rec.Tests.Fail, "TestRun/"+lightWhitespaceCase)
	for _, c := range rec.Cases {
		if c.ID != lightWhitespaceCase {
			ok = ok && c.Pass
		}
	}
	if !ok {
		return CodeLightSeedInvalid
	}
	return ""
}

// lightBuildCases builds the program and runs the ten fixed CLI cases,
// returning the build's retained streams too.
func lightBuildCases(v *validator, o lightRunOptions, env []string) (ExecRecord, []CaseResult, []byte, []byte) {
	bin := filepath.Join(o.dir, "bin", "greeting")
	buildArgs := slices.Clone(lightArgvBuild[1:])
	buildArgs[slices.Index(buildArgs, "$BIN")] = bin
	build, bres := v.exec("light_build", o.goBin, lightArgvBuild, buildArgs, env, maxBuildOutput)
	built := cleanRun(build) && build.ExitCode == 0
	caseEnv := []string{"PATH=" + o.pathEnv, "HOME=" + filepath.Join(o.dir, "home"), "TMPDIR=" + filepath.Join(o.dir, "tmp")}
	cases := []CaseResult{}
	for _, c := range FeatureCases {
		r := CaseResult{ID: c.ID, Exec: ExecRecord{Schema: ExecSchema, Name: "case_" + c.ID, Argv: append([]string{"greeting"}, c.Args...), Commit: o.commit, Tree: o.tree}}
		if built {
			var res CommandResult
			r.Exec, res = v.exec("case_"+c.ID, bin, append([]string{"greeting"}, c.Args...), c.Args, caseEnv, maxCaseOutput)
			r.Stdout, r.Stderr = toValidUTF8(res.Stdout), toValidUTF8(res.Stderr)
		}
		r.Pass, r.Code = evaluateCase(c, r.Exec, r.Stdout, r.Stderr)
		cases = append(cases, r)
	}
	return build, cases, bres.Stdout, bres.Stderr
}

// DevLightRegression is validation/light-regression.json: the candidate's
// whole test file over the seed source (red) and over the candidate
// source (green), with every exported input hashed before and after.
type DevLightRegression struct {
	Schema        string             `json:"schema"`
	RunID         string             `json:"run_id"`
	SeedCommit    string             `json:"seed_commit"`
	SeedTree      string             `json:"seed_tree"`
	Candidate     string             `json:"candidate"`
	CandidateTree string             `json:"candidate_tree"`
	TestSHA256    string             `json:"test_sha256"`
	Before        []LightInput       `json:"before"`
	BeforeAfter   []LightInput       `json:"before_after"`
	After         []LightInput       `json:"after"`
	AfterAfter    []LightInput       `json:"after_after"`
	Red           ExecRecord         `json:"red"`
	Green         ExecRecord         `json:"green"`
	RedEvents     LightTestEvents    `json:"red_events"`
	GreenEvents   LightTestEvents    `json:"green_events"`
	Structure     LightTestStructure `json:"structure"`
	// SeedCLI is the supervisor's independent seed whitespace result the
	// red event corroborates.
	SeedCLI bool   `json:"seed_cli_corroborated"`
	Pass    bool   `json:"pass"`
	Code    string `json:"code"`
	Error   string `json:"error"`
}

// lightOverlays are the before and after exports: the seed's go.mod and
// main.go, and the candidate's, each with the candidate's main_test.go
// byte for byte.
func lightOverlays(seed, cand map[string][]byte) (before, after map[string][]byte) {
	before = map[string][]byte{"go.mod": seed["go.mod"], "main.go": seed["main.go"], "main_test.go": cand["main_test.go"]}
	after = map[string][]byte{"go.mod": cand["go.mod"], "main.go": cand["main.go"], "main_test.go": cand["main_test.go"]}
	return before, after
}

// lightRegressionRun is the regression record and its retained streams.
type lightRegressionRun struct {
	rec                DevLightRegression
	redOut, redErr     []byte
	greenOut, greenErr []byte
}

// runLightRegression runs the fixed red then green JSON commands in two
// fresh exports and judges them with the structure, input identity and the
// supervisor's seed CLI corroboration.
func runLightRegression(o lightRunOptions, seedCommit, seedTree string, seed, cand map[string][]byte, seedCLI bool) lightRegressionRun {
	out := lightRegressionRun{rec: DevLightRegression{Schema: devKind("light-regression"), SeedCommit: seedCommit, SeedTree: seedTree, Candidate: o.commit,
		CandidateTree: o.tree, TestSHA256: sha256Hex(cand["main_test.go"]), SeedCLI: seedCLI}}
	rec := &out.rec
	before, after := lightOverlays(seed, cand)
	rec.Before, rec.After = lightInputs(before), lightInputs(after)
	rec.BeforeAfter, rec.AfterAfter = []LightInput{}, []LightInput{}
	rec.Structure = lightTestStructureOf(seed["main_test.go"], cand["main_test.go"])
	var errs []string
	run := func(name, dir string, files map[string][]byte) (ExecRecord, CommandResult, []LightInput) {
		src, env, err := exportProjectAllowed(dir, files, o.pathEnv, devTreePolicy())
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			return ExecRecord{Schema: ExecSchema, Name: name, Argv: lightArgvJSON}, CommandResult{}, []LightInput{}
		}
		v := &validator{run: o.run, clock: o.clock, deadline: o.deadline, env: env, src: src, commit: o.commit, tree: o.tree, mark: o.mark}
		r, res := v.exec(name, o.goBin, lightArgvJSON, lightArgvJSON[1:], env, lightJSONLimit)
		if v.err != nil {
			errs = append(errs, v.err.Error())
		}
		return r, res, readBackInputs(src, files)
	}
	var res CommandResult
	rec.Red, res, rec.BeforeAfter = run("light_red", filepath.Join(o.dir, "before"), before)
	out.redOut, out.redErr = res.Stdout, res.Stderr
	rec.RedEvents = lightTestEventsOf(res.Stdout)
	rec.Green, res, rec.AfterAfter = run("light_green", filepath.Join(o.dir, "after"), after)
	out.greenOut, out.greenErr = res.Stdout, res.Stderr
	rec.GreenEvents = lightTestEventsOf(res.Stdout)
	rec.Code = lightRegressionVerdict(*rec)
	rec.Pass = rec.Code == ""
	rec.Error = strings.Join(errs, "; ")
	return out
}

// lightRegressionVerdict judges a regression record ("" passed).
func lightRegressionVerdict(rec DevLightRegression) string {
	switch {
	case !slices.Equal(rec.Before, rec.BeforeAfter) || !slices.Equal(rec.After, rec.AfterAfter) || rec.TestSHA256 == "" ||
		inputSHA(rec.Before, "main_test.go") != rec.TestSHA256 || inputSHA(rec.After, "main_test.go") != rec.TestSHA256:
		return CodeRegressionInputs
	case !rec.Structure.ok():
		return CodeRegressionStructure
	}
	if code := judgeRed(rec.Red, rec.RedEvents); code != "" {
		return code
	}
	if !rec.SeedCLI {
		// The red event is corroborated by the supervisor's own seed CLI
		// whitespace result, or it is not a valid red.
		return CodeRegressionInvalidRed
	}
	return judgeGreen(rec.Green, rec.GreenEvents)
}

// inputSHA is a digest list's entry for path ("" when absent).
func inputSHA(in []LightInput, path string) string {
	for _, i := range in {
		if i.Path == path {
			return i.SHA256
		}
	}
	return ""
}

// equalInputs reports two digest lists equal (bytes.Equal of their JSON).
func equalInputs(a, b []LightInput) bool { return bytes.Equal(mustJSON(a), mustJSON(b)) }

// DevLightCoverageRecord is validation/light-coverage.json: the seed and
// candidate whole-production coverage with identical flags and scope, the
// recorded profile, source and test identities, and the integer
// comparison.
type DevLightCoverageRecord struct {
	Schema        string                  `json:"schema"`
	RunID         string                  `json:"run_id"`
	SeedCommit    string                  `json:"seed_commit"`
	Candidate     string                  `json:"candidate"`
	CandidateTree string                  `json:"candidate_tree"`
	Argv          []string                `json:"argv"`
	Baseline      *LightCoverage          `json:"baseline"`
	BaselineTest  string                  `json:"baseline_test_sha256"`
	Cand          *LightCoverage          `json:"candidate_coverage"`
	CandidateTest string                  `json:"candidate_test_sha256"`
	CandidateExec ExecRecord              `json:"candidate_exec"`
	Comparison    LightCoverageComparison `json:"comparison"`
	// FinalStdout and FinalStderr are the SHA-256 of the final ordinary
	// suite's captured streams (validation/final-stdout.txt and
	// final-stderr.txt), which its legacy-shaped run record does not hash.
	FinalStdout string `json:"final_stdout_sha256"`
	FinalStderr string `json:"final_stderr_sha256"`
	Error       string `json:"error"`
}

// lightFinalRun is the light final validation's records and retained
// streams.
type lightFinalRun struct {
	reg                lightRegressionRun
	suite              TestRun
	suiteOut, suiteErr []byte
	cases              CasesRecord
	buildOut, buildErr []byte
	reqs               RequirementsRecord
	cover              ExecRecord
	coverOut, coverErr []byte
	profile            []byte
	coverage           DevLightCoverageRecord
	err                error
}

// runLightFinal is the light final validation within one shared deadline:
// the red/green regression, then the candidate's ordinary suite, coverage,
// build and ten CLI cases in a fresh candidate export, the requirement
// mapping from the green event stream, and the coverage comparison against
// the captured baseline.
func runLightFinal(o lightRunOptions, seedCommit, seedTree string, seed, cand map[string][]byte, baseline DevLightBaseline) lightFinalRun {
	var out lightFinalRun
	out.reg = runLightRegression(lightRunOptions{run: o.run, clock: o.clock, goBin: o.goBin, pathEnv: o.pathEnv, dir: filepath.Join(o.dir, "regression"),
		commit: o.commit, tree: o.tree, deadline: o.deadline, mark: o.mark}, seedCommit, seedTree, seed, cand, baseline.Defect.Exact)
	files := map[string][]byte{}
	for k, b := range cand {
		files[k] = b
	}
	dir := filepath.Join(o.dir, "candidate")
	out.suite = TestRun{Schema: TestRunSchema, Argv: append([]string(nil), lightArgvSuite...), Commit: o.commit, Tree: o.tree, Files: sortedKeys(files)}
	out.cases = CasesRecord{Schema: CasesSchema, Commit: o.commit, Tree: o.tree, Cases: []CaseResult{}}
	out.coverage = DevLightCoverageRecord{Schema: devKind("light-coverage"), SeedCommit: seedCommit, Candidate: o.commit, CandidateTree: o.tree,
		Argv: lightArgvCover, Baseline: baseline.Coverage, BaselineTest: sha256Hex(seed["main_test.go"]), CandidateTest: sha256Hex(cand["main_test.go"])}
	src, env, err := exportProjectAllowed(dir, files, o.pathEnv, devTreePolicy())
	if err != nil {
		out.err = err
		out.suite.ExitCode = -1
		out.coverage.Error = "export: " + err.Error()
		out.coverage.Comparison = LightCoverageComparison{Code: CodeCoverageUnobserved}
		out.reqs = buildRequirements(files, nil, nil, ExecRecord{Schema: ExecSchema, Name: "light_green", Commit: o.commit, Tree: o.tree}, o.commit)
		return out
	}
	out.suite.Env = offlineEnv(env)
	v := &validator{run: o.run, clock: o.clock, deadline: o.deadline, env: env, src: src, commit: o.commit, tree: o.tree, mark: o.mark}
	verRec, ver := v.exec("light_final_version", o.goBin, lightArgvVersion, lightArgvVersion[1:], env, 4096)
	out.suite.Toolchain = strings.TrimSpace(string(ver.Stdout))
	rec, res := v.exec("light_final_suite", o.goBin, lightArgvSuite, lightArgvSuite[1:], env, lightStreamLimit)
	out.suite.ExitCode, out.suite.Signal, out.suite.TimedOut, out.suite.DurationMS = rec.ExitCode, rec.Signal, rec.TimedOut, rec.DurationMS
	if !verRec.Started || !rec.Started {
		out.suite.ExitCode = -1
	}
	out.suiteOut, out.suiteErr = res.Stdout, res.Stderr
	out.coverage.FinalStdout, out.coverage.FinalStderr = sha256Hex([]byte(toValidUTF8(res.Stdout))), sha256Hex([]byte(toValidUTF8(res.Stderr)))
	profile := filepath.Join(o.dir, "cover", "coverage.out")
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil && v.err == nil {
		v.err = err
	}
	args := slices.Clone(lightArgvCover[1:])
	args[slices.Index(args, "-coverprofile=$PROFILE")] = "-coverprofile=" + profile
	out.cover, res = v.exec("light_final_cover", o.goBin, lightArgvCover, args, env, lightStreamLimit)
	out.coverOut, out.coverErr = res.Stdout, res.Stderr
	out.coverage.CandidateExec = out.cover
	if b, err := readBounded(profile, maxProfileBytes); err == nil {
		out.profile = b
	}
	out.cases.Build, out.cases.Cases, out.buildOut, out.buildErr = lightBuildCases(v, lightRunOptions{dir: o.dir, pathEnv: o.pathEnv, goBin: o.goBin,
		commit: o.commit, tree: o.tree}, env)
	out.reqs = buildRequirements(files, out.reg.greenOut, out.cases.Cases, out.reg.rec.Green, o.commit)
	out.coverage.Comparison, out.coverage.Cand, out.coverage.Error = lightCoverageOf(baseline.Coverage, cand, out.profile, out.cover)
	out.err = v.err
	return out
}

// lightCoverageOf derives the candidate side and compares it with the
// baseline side: a missing or invalid profile or side is unobserved.
func lightCoverageOf(base *LightCoverage, cand map[string][]byte, profile []byte, rec ExecRecord) (LightCoverageComparison, *LightCoverage, string) {
	if base == nil {
		return LightCoverageComparison{Code: CodeCoverageUnobserved}, nil, "no baseline coverage"
	}
	if profile == nil || !cleanRun(rec) || rec.ExitCode != 0 {
		return LightCoverageComparison{Code: CodeCoverageUnobserved}, nil, "the candidate coverage run gave no profile"
	}
	c, err := deriveWholeCoverage(cand["main.go"], profile, modulePathOf(cand["go.mod"]))
	if err != nil {
		return LightCoverageComparison{Code: CodeCoverageUnobserved}, nil, err.Error()
	}
	if c.Module != base.Module || c.Scope != base.Scope {
		return LightCoverageComparison{Code: CodeCoverageUnobserved}, &c, "the coverage scopes differ"
	}
	return compareLightCoverage(*base, c), &c, ""
}
