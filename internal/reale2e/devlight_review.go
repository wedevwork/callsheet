package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The light reviewer helper and its evidence (design 12a-dev-flow-light,
// Light reviewer helper; FP-5). dev-review-verify --scenario light runs the
// fixed light recipe from the code reviewer's checkout: under a fresh
// .callsheet-review-scratch/<task-id>/ it exports the seed to baseline/,
// the seed with the candidate's main_test.go to before/ and the candidate
// to after/, then runs go version, the baseline suite and coverage, the
// red (before) and green (after) JSON runs, the after suite and coverage,
// removes only its scratch and proves its inputs unchanged. Its receipt is
// callsheet-dev-light-review-run/v1 (the full receipt keeps
// callsheet-dev-full-review-run/v1), followed by the light report-line
// trailer. The derivation recomputes the tests, red/green and coverage
// from the receipt's command bytes and the Git sources; a receipt quoted
// by the model is never sufficient.

// DevLightReviewRunSchema is the light helper receipt's schema.
const DevLightReviewRunSchema = "callsheet-dev-light-review-run/v1"

// lightReviewCommandNames are the light helper's commands in order.
var lightReviewCommandNames = []string{"go_version", "baseline_suite", "baseline_cover", "red", "green", "after_suite", "after_cover"}

// lightReviewLogicalArgv is a light command's logical argv for scratch.
func lightReviewLogicalArgv(name, scratch string) []string {
	cover := func(dir string) []string {
		return []string{"go", "test", "-count=1", "-covermode=set", "-coverprofile=" + scratch + "/" + dir + "/coverage.out", "./..."}
	}
	switch name {
	case "go_version":
		return slices.Clone(lightArgvVersion)
	case "baseline_suite", "after_suite":
		return slices.Clone(lightArgvSuite)
	case "baseline_cover":
		return cover("baseline-cover")
	case "after_cover":
		return cover("after-cover")
	}
	return slices.Clone(lightArgvJSON)
}

// lightReviewCwd is a light command's working directory under scratch.
func lightReviewCwd(name, scratch string) string {
	switch name {
	case "baseline_suite", "baseline_cover":
		return scratch + "/baseline"
	case "red":
		return scratch + "/before"
	}
	return scratch + "/after"
}

// LightExport is one light export's input manifest.
type LightExport struct {
	Name string `json:"name"`
	// Inputs are the export's files from the pinned objects; Created and
	// After every entry the export held once written and after the
	// commands ran (review 2d.4 round 1, C2).
	Inputs  []LightInput `json:"inputs"`
	Created []LightInput `json:"created"`
	After   []LightInput `json:"after"`
}

// DevLightReviewRun is the light helper's JSON receipt.
type DevLightReviewRun struct {
	Schema            string                  `json:"schema"`
	Scenario          string                  `json:"scenario"`
	Task              string                  `json:"task"`
	Candidate         string                  `json:"candidate"`
	InitialSource     string                  `json:"initial_source"`
	InputTree         string                  `json:"input_tree"`
	SeedTree          string                  `json:"seed_tree"`
	Inputs            []ReviewInputFile       `json:"inputs"`
	InputsAfter       []ReviewInputFile       `json:"inputs_after"`
	FixSHA256         string                  `json:"fix_sha256"`
	RCASHA256         string                  `json:"rca_sha256"`
	TestSHA256        string                  `json:"test_sha256"`
	Exports           []LightExport           `json:"exports"`
	Go                ReviewTool              `json:"go"`
	Env               []string                `json:"env"`
	Start             string                  `json:"start"`
	End               string                  `json:"end"`
	Deadline          string                  `json:"deadline"`
	EffectiveDeadline string                  `json:"effective_deadline"`
	Scratch           string                  `json:"scratch"`
	Commands          []ReviewCommand         `json:"commands"`
	BaselineProfile   ReviewProfile           `json:"baseline_profile"`
	CandidateProfile  ReviewProfile           `json:"candidate_profile"`
	BaselineCoverage  *LightCoverage          `json:"baseline_coverage"`
	CandidateCoverage *LightCoverage          `json:"candidate_coverage"`
	Comparison        LightCoverageComparison `json:"comparison"`
	Red               LightTestEvents         `json:"red"`
	Green             LightTestEvents         `json:"green"`
	RedCode           string                  `json:"red_code"`
	GreenCode         string                  `json:"green_code"`
	ScratchCreated    bool                    `json:"scratch_created"`
	ScratchRemoved    bool                    `json:"scratch_removed"`
	InputsUnchanged   bool                    `json:"inputs_unchanged"`
	OutputsOmitted    bool                    `json:"outputs_omitted"`
	Status            string                  `json:"status"`
	Code              string                  `json:"code"`
	RejectedEntry     *ReviewRejectedEntry    `json:"rejected_entry,omitempty"`
}

// lightExportFiles are the light recipe's three exports from the seed and
// candidate trees: the seed, the seed with the candidate's test, and the
// candidate.
func lightExportFiles(seed, cand map[string]TreeFile) [][2]any {
	files := func(t map[string]TreeFile) map[string][]byte {
		out := map[string][]byte{}
		for n, f := range t {
			if n != "review.md" {
				out[n] = f.Content
			}
		}
		return out
	}
	s, c := files(seed), files(cand)
	before, _ := lightOverlays(s, c)
	return [][2]any{{"baseline", s}, {"before", before}, {"after", c}}
}

// runLightReviewVerify is the light helper's whole run.
func runLightReviewVerify(env reviewEnv, a reviewArgs) DevLightReviewRun {
	start := env.clock.Now()
	eff := minTime(a.at, start.Add(reviewHelperBound))
	scratchRoot := filepath.Join(env.cwd, reviewScratch)
	scratch := filepath.Join(scratchRoot, a.task)
	rec := DevLightReviewRun{Schema: DevLightReviewRunSchema, Scenario: scenarioLight, Task: a.task, Candidate: a.candidate, InitialSource: a.initial,
		Inputs: []ReviewInputFile{}, InputsAfter: []ReviewInputFile{}, Exports: []LightExport{}, Env: []string{}, Start: formatTime(start),
		Deadline: a.deadline, EffectiveDeadline: formatTime(eff), Scratch: scratch, Commands: []ReviewCommand{}, Go: ReviewTool{Path: a.goBin},
		Red: lightTestEventsOf(nil), Green: lightTestEventsOf(nil)}
	finish := func(status, code string) DevLightReviewRun {
		if rec.Status == "" {
			rec.Status, rec.Code = status, code
		}
		rec.End = formatTime(env.clock.Now())
		return rec
	}
	refuse := func(code string) DevLightReviewRun { return finish(reviewRefused, code) }
	in, err := inspectReviewCheckout(env.cwd, a)
	if err != nil {
		rec.RejectedEntry = in.rejected
		return refuse(codeOf(err))
	}
	rec.InputTree, rec.SeedTree, rec.Inputs = in.tree, in.seedTree, in.inputs
	rec.FixSHA256, rec.RCASHA256, rec.TestSHA256 = fileSHA(in.files, "fix.md"), fileSHA(in.files, "rca.md"), fileSHA(in.files, "main_test.go")
	gob, err := readBounded(a.goBin, 1<<30)
	if err != nil {
		return refuse("go_unreadable")
	}
	rec.Go.SHA256 = sha256Hex(gob)
	if env.clock.Now().Compare(eff) >= 0 {
		return refuse("deadline_passed")
	}
	if err := os.Mkdir(scratchRoot, 0o700); err != nil {
		return refuse("scratch_unavailable")
	}
	rec.ScratchCreated = true
	failed, later := "", ""
	fail := func(code string) {
		if failed == "" {
			failed = code
		}
	}
	// failLater records an ordinary command's nonzero exit, reported only
	// when no more specific judgment (red, green, coverage) explains it.
	failLater := func(code string) {
		if later == "" {
			later = code
		}
	}
	for _, e := range lightExportFiles(in.seedFiles, in.files) {
		name, files := e[0].(string), e[1].(map[string][]byte)
		ex := LightExport{Name: name, Inputs: lightInputs(files), After: []LightInput{}}
		if err := exportLightTree(filepath.Join(scratch, name), files); err != nil {
			fail("export_failed")
		}
		// The export as written, against the pinned objects.
		ex.Created = exportManifest(filepath.Join(scratch, name))
		if !slices.Equal(ex.Created, ex.Inputs) {
			fail("export_mismatch")
		}
		rec.Exports = append(rec.Exports, ex)
	}
	envDir := filepath.Join(scratch, "env")
	for _, d := range []string{"gocache", "gomodcache", "gopath", "home", "tmp", "baseline-cover", "after-cover"} {
		dir := filepath.Join(envDir, d)
		if strings.HasSuffix(d, "-cover") {
			dir = filepath.Join(scratch, d)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fail("scratch_unavailable")
		}
	}
	rec.Env = testEnv(envDir, env.path)
	gone := true
	if failed == "" {
		for _, name := range lightReviewCommandNames {
			c := runLightReviewCommand(env, a.goBin, name, scratch, rec.Env, eff)
			rec.Commands = append(rec.Commands, c)
			gone = gone && (c.ProcessGone || !c.Started)
			ran := c.Started && !c.TimedOut && c.ProcessGone
			switch {
			case !c.Started:
				fail("command_not_started")
			case c.TimedOut:
				fail("command_timed_out")
			case !c.ProcessGone:
				fail("process_not_gone")
			case (name == "baseline_suite" || name == "baseline_cover") && (c.Exit != 0 || c.Signal != "" || c.Truncated):
				// Dependent commands stop after an invalid baseline.
				fail("baseline_invalid")
				ran = false
			case c.Truncated:
				fail("output_truncated")
			case c.Signal != "":
				fail("command_failed")
			case c.Exit != 0 && name != "red" && name != "green":
				// The red and green runs are judged from their JSON events
				// below (their exits are part of those judgments).
				failLater("command_failed")
			}
			if !ran {
				break
			}
		}
	}
	// Every export after the commands: a worker-run command that changed,
	// removed or added an entry fails the run.
	for i := range rec.Exports {
		rec.Exports[i].After = exportManifest(filepath.Join(scratch, rec.Exports[i].Name))
		if !slices.Equal(rec.Exports[i].After, rec.Exports[i].Inputs) {
			fail("export_changed")
		}
	}
	if len(rec.Commands) > 0 {
		rec.Go.Version = strings.TrimSpace(rec.Commands[0].Stdout)
	}
	readProfile := func(dir string) ReviewProfile {
		if p, err := readBounded(filepath.Join(scratch, dir, "coverage.out"), maxProfileBytes); err == nil {
			return ReviewProfile{Present: true, SHA256: sha256Hex(p), Text: string(p)}
		}
		return ReviewProfile{}
	}
	rec.BaselineProfile, rec.CandidateProfile = readProfile("baseline-cover"), readProfile("after-cover")
	lightReviewDerive(&rec, fileContent(in.seedFiles, "main.go"), fileContent(in.files, "main.go"), modulePathOf(fileContent(in.files, "go.mod")))
	switch {
	case rec.BaselineCoverage == nil || rec.CandidateCoverage == nil || rec.Comparison.Code == CodeCoverageUnobserved:
		fail(CodeCoverageUnobserved)
	case !rec.Comparison.Pass:
		fail(CodeCoverageDecreased)
	}
	if failed == "" && len(rec.Commands) == len(lightReviewCommandNames) {
		if rec.RedCode != "" {
			fail(rec.RedCode)
		}
		if rec.GreenCode != "" {
			fail(rec.GreenCode)
		}
	}
	if later != "" {
		fail(later)
	}
	if gone {
		if err := os.RemoveAll(scratchRoot); err == nil {
			_, err := os.Lstat(scratchRoot)
			rec.ScratchRemoved = errors.Is(err, fs.ErrNotExist)
		}
	}
	if !rec.ScratchRemoved {
		fail("cleanup_unverified")
	}
	if now, rejected, err := snapshotReviewInputs(env.cwd, in.files); rejected != nil {
		rec.RejectedEntry = rejected
		fail("unexpected_file")
	} else if err == nil {
		rec.InputsAfter = now
		rec.InputsUnchanged = slices.Equal(now, rec.Inputs)
	}
	if !rec.InputsUnchanged {
		fail("inputs_changed")
	}
	if failed != "" {
		return finish(reviewFailed, failed)
	}
	return finish(reviewPassed, "")
}

// lightReviewDerive fills a light receipt's derived claims from its
// command bytes and profiles: the red and green events and judgments and
// both coverage sides with their comparison. The helper and the
// derivation share it.
func lightReviewDerive(rec *DevLightReviewRun, seedMain, candMain []byte, module string) {
	rec.Red, rec.Green = lightTestEventsOf(nil), lightTestEventsOf(nil)
	rec.RedCode, rec.GreenCode = CodeRegressionInvalidRed, CodeRegressionNotGreen
	if c := commandNamed(rec.Commands, "red"); c != nil {
		rec.Red = lightTestEventsOf([]byte(c.Stdout))
		rec.RedCode = judgeRed(reviewExec(*c), rec.Red)
	}
	if c := commandNamed(rec.Commands, "green"); c != nil {
		rec.Green = lightTestEventsOf([]byte(c.Stdout))
		rec.GreenCode = judgeGreen(reviewExec(*c), rec.Green)
	}
	rec.BaselineCoverage, rec.CandidateCoverage = nil, nil
	if p := rec.BaselineProfile; p.Present {
		if c, err := deriveWholeCoverage(seedMain, []byte(p.Text), module); err == nil {
			rec.BaselineCoverage = &c
		}
	}
	if p := rec.CandidateProfile; p.Present {
		if c, err := deriveWholeCoverage(candMain, []byte(p.Text), module); err == nil {
			rec.CandidateCoverage = &c
		}
	}
	rec.Comparison = LightCoverageComparison{Code: CodeCoverageUnobserved}
	if rec.BaselineCoverage != nil && rec.CandidateCoverage != nil {
		rec.Comparison = compareLightCoverage(*rec.BaselineCoverage, *rec.CandidateCoverage)
	}
}

// reviewExec is a helper command as an ExecRecord for the shared judges.
func reviewExec(c ReviewCommand) ExecRecord {
	return ExecRecord{Started: c.Started, ExitCode: c.Exit, Signal: c.Signal, TimedOut: c.TimedOut, Truncated: c.Truncated}
}

// exportLightTree writes files into the new directory dir.
func exportLightTree(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range sortedKeys(files) {
		if err := writeExclusive(filepath.Join(dir, name), files[name], 0o600); err != nil {
			return err
		}
	}
	return nil
}

// runLightReviewCommand runs one fixed light command before the effective
// deadline.
func runLightReviewCommand(env reviewEnv, goBin, name, scratch string, procEnv []string, eff time.Time) ReviewCommand {
	logical := lightReviewLogicalArgv(name, scratch)
	dir := lightReviewCwd(name, scratch)
	c := ReviewCommand{Name: name, Argv: logical, Cwd: dir, Start: formatTime(env.clock.Now())}
	left := eff.Sub(env.clock.Now())
	if left <= 0 {
		c.TimedOut, c.End = true, c.Start
		return c
	}
	res, err := runCommand(env.launcher, env.clock, ProcSpec{Path: goBin, Args: logical[1:], Env: procEnv, Dir: dir}, left, reviewStreamCap)
	c.End = formatTime(env.clock.Now())
	c.Started = err == nil || res.Proc != nil
	c.ProcessGone = c.Started && err == nil
	c.Exit, c.Signal, c.TimedOut, c.Truncated = res.ExitCode, res.Signal, res.TimedOut, res.Truncated
	c.Stdout, c.Stderr = toValidUTF8(res.Stdout), toValidUTF8(res.Stderr)
	return c
}

// encodeLightReviewRun renders the light receipt as one compact JSON line
// within its bound: an over-bound receipt drops the command outputs and
// profiles and fails closed (its claims can then not be recomputed).
func encodeLightReviewRun(rec DevLightReviewRun) ([]byte, DevLightReviewRun, int) {
	enc := func(r DevLightReviewRun) []byte {
		b, _ := json.Marshal(r)
		return append(b, '\n')
	}
	out := enc(rec)
	if len(out) > maxReviewRunBytes {
		rec.OutputsOmitted, rec.Status, rec.Code = true, reviewFailed, "receipt_overflow"
		// A copy: the caller's commands keep their outputs.
		rec.Commands = slices.Clone(rec.Commands)
		for i := range rec.Commands {
			rec.Commands[i].Stdout, rec.Commands[i].Stderr = "", ""
		}
		rec.BaselineProfile.Text, rec.CandidateProfile.Text = "", ""
		out = enc(rec)
	}
	if rec.Status == reviewPassed {
		return out, rec, 0
	}
	return out, rec, 1
}

// lightHelperStdout is the light helper's whole stdout: the finalized
// receipt line, then its paste-ready light report lines (fail-closed like
// the full helper's).
func lightHelperStdout(rec DevLightReviewRun) ([]byte, int) {
	out, final, code := encodeLightReviewRun(rec)
	trailer, ok := renderLightReviewReportLines(final, out)
	if !ok || final.Status == reviewPassed && !lightReportLinesComplete(trailer) {
		if final.Status == reviewPassed {
			final.Status, final.Code = reviewFailed, "report_lines_invalid"
		}
		out, final, code = encodeLightReviewRun(final)
		if trailer, ok = renderLightReviewReportLines(final, out); !ok {
			trailer = ""
		}
	}
	return append(out, trailer...), code
}

// lightExitLabels are the six light verification lines' labels, by
// command.
var lightExitLabels = map[string]string{"baseline_suite": "baseline suite", "baseline_cover": "baseline coverage", "red": "red",
	"green": "green", "after_suite": "final suite", "after_cover": "candidate coverage"}

// lightRedExpectation is the red verification line's stated expectation.
const lightRedExpectation = " (expected exit 1: expected assertion failure of TestRun/whitespace on the seed)"

// lightTrailerKeys are the light report-line fields, in rendering order.
var lightTrailerKeys = []string{"Scenario:", "Reviewed candidate:", "Initial source:", "Source sha256:", "Regression test sha256:", "Verification exit",
	"Coverage comparison:", "Cleanup:", reviewRunField}

// renderLightReviewReportLines is the one pure light report-line renderer
// the helper's emission and the derivation share: the scenario, the
// candidate and seed, the four source and the test digests, the six
// verification lines with their recorded exits (each command's exact
// logical argv in the receipt's task scratch; the red line states its
// expected exit 1), the integer coverage comparison, the cleanup booleans
// and the receipt digest. It renders only facts the receipt has and
// reports false when the lines exceed their bound.
func renderLightReviewReportLines(run DevLightReviewRun, receipt []byte) (string, bool) {
	var b strings.Builder
	b.WriteString("Scenario: light\n")
	if hex40RE.MatchString(run.Candidate) {
		fmt.Fprintf(&b, "Reviewed candidate: %s\n", run.Candidate)
	}
	if hex40RE.MatchString(run.InitialSource) {
		fmt.Fprintf(&b, "Initial source: %s\n", run.InitialSource)
	}
	for _, p := range devSourceFiles {
		for _, in := range run.Inputs {
			if in.Path == p && hex64RE.MatchString(in.SHA256) {
				fmt.Fprintf(&b, "Source sha256: %s %s\n", p, in.SHA256)
				break
			}
		}
	}
	if hex64RE.MatchString(run.TestSHA256) {
		fmt.Fprintf(&b, "Regression test sha256: main_test.go %s\n", run.TestSHA256)
	}
	scratchOK := contract.ValidTaskID(run.Task) && filepath.IsAbs(run.Scratch) && filepath.Clean(run.Scratch) == run.Scratch &&
		strings.HasSuffix(run.Scratch, "/"+reviewScratch+"/"+run.Task) && !strings.ContainsAny(run.Scratch, "\r\n\x00")
	for _, name := range lightReviewCommandNames[1:] {
		var c *ReviewCommand
		n := 0
		for i := range run.Commands {
			if run.Commands[i].Name == name {
				c, n = &run.Commands[i], n+1
			}
		}
		if n != 1 || !c.Started || c.Exit < 0 || c.Exit > 999 || !scratchOK || !slices.Equal(c.Argv, lightReviewLogicalArgv(name, run.Scratch)) {
			continue
		}
		suffix := ""
		if name == "red" {
			suffix = lightRedExpectation
		}
		fmt.Fprintf(&b, "Verification exit %d: %s: %s%s\n", c.Exit, lightExitLabels[name], strings.Join(c.Argv, " "), suffix)
	}
	if run.BaselineCoverage != nil && run.CandidateCoverage != nil && run.Comparison.Code != CodeCoverageUnobserved {
		verdict := "nondecreasing"
		if !run.Comparison.Pass {
			verdict = "decreased"
		}
		fmt.Fprintf(&b, "Coverage comparison: candidate %s, baseline %s of %s; %s >= %s is %t (%s)\n", coverageFraction(*run.CandidateCoverage),
			coverageFraction(*run.BaselineCoverage), run.CandidateCoverage.Scope, run.Comparison.Left, run.Comparison.Right, run.Comparison.Pass, verdict)
	}
	fmt.Fprintf(&b, "Cleanup: scratch_created=%t; scratch_removed=%t; inputs_unchanged=%t\n", run.ScratchCreated, run.ScratchRemoved, run.InputsUnchanged)
	fmt.Fprintf(&b, "%s %s\n", reviewRunField, sha256Hex(receipt))
	out := b.String()
	return out, len(out) <= maxReviewTrailerBytes
}

// lightReportLinesComplete reports a trailer with every field a successful
// light receipt must supply.
func lightReportLinesComplete(trailer string) bool {
	count := func(prefix string) int {
		n := 0
		for _, l := range strings.Split(trailer, "\n") {
			if strings.HasPrefix(l, prefix) {
				n++
			}
		}
		return n
	}
	return count("Reviewed candidate: ") == 1 && count("Initial source: ") == 1 && count("Source sha256: ") == len(devSourceFiles) &&
		count("Regression test sha256: ") == 1 && count("Verification exit ") == len(lightReviewCommandNames)-1 && count("Coverage comparison: ") == 1
}

// checkLightTrailerClaims requires review.md to carry every rendered
// light report line's field exactly as rendered, once each, with no other
// line of a rendered field's key.
func checkLightTrailerClaims(report, trailer string) string {
	want := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSuffix(trailer, "\n"), "\n") {
		for _, k := range lightTrailerKeys {
			if v, ok := strings.CutPrefix(l, k+" "); ok {
				want[k] = append(want[k], v)
				break
			}
		}
	}
	for k, vs := range want {
		got, bad := fieldValues(report, k)
		got, vs = slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(vs))
		if bad || !slices.Equal(got, vs) || len(slices.Compact(slices.Clone(got))) != len(got) {
			return "review_report_lines_mismatch"
		}
	}
	return ""
}

// lightReviewExitRE is one light verification line's value.
var lightReviewExitRE = regexp.MustCompile(`^([0-9]{1,3}): (baseline suite|baseline coverage|red|green|final suite|candidate coverage): (go test .+)$`)

// LightReviewReportOK checks the fields the supervisor requires of a light
// review.md (dispatched by plan mode, never by the report's wording): the
// terminal marker, the reviewer run digest the captured log corroborates,
// the scenario, the reviewed candidate, every candidate source digest, the
// six verification lines in this task's scratch (each label once), the
// coverage comparison and the cleanup result.
func LightReviewReportOK(report, marker, candidate, taskID string, candidateFiles map[string]TreeFile, runDigest string) error {
	got, err := parseTerminalMarker(report)
	if err != nil || got != marker {
		return fmt.Errorf("publication: review.md does not end with the final marker %s", marker)
	}
	runs, bad := fieldValues(report, reviewRunField)
	switch {
	case bad || len(runs) > 1 || len(runs) == 1 && !hex64RE.MatchString(runs[0]):
		return fmt.Errorf("publication: review.md has a malformed or repeated %q line", reviewRunField)
	case runDigest == "" && len(runs) != 0:
		return fmt.Errorf("publication: review.md names a reviewer run the captured log does not corroborate")
	case runDigest == "" && marker == markerReviewApproved:
		return fmt.Errorf("publication: an approving review.md has no corroborated reviewer run")
	case runDigest != "" && (len(runs) != 1 || runs[0] != runDigest):
		return fmt.Errorf("publication: review.md does not name the reviewer run sha256 %s", runDigest)
	}
	one := func(key string) (string, error) {
		vals, bad := fieldValues(report, key)
		if bad || len(vals) != 1 {
			return "", fmt.Errorf("publication: review.md needs exactly one %q line", key)
		}
		return vals[0], nil
	}
	if v, err := one("Scenario:"); err != nil || v != scenarioLight {
		return fmt.Errorf("publication: review.md does not name the light scenario")
	}
	v, err := one("Reviewed candidate:")
	if err != nil {
		return err
	}
	if !hex40RE.MatchString(candidate) || v != candidate {
		return fmt.Errorf("publication: review.md does not bind the candidate commit %s", candidate)
	}
	sources, bad := fieldValues(report, "Source sha256:")
	want := map[string]string{}
	for _, p := range devSourceFiles {
		if f, ok := candidateFiles[p]; ok {
			want[p] = sha256Hex(f.Content)
		}
	}
	seen := map[string]bool{}
	for _, s := range sources {
		p, sum, ok := strings.Cut(s, " ")
		if !ok || want[p] == "" || want[p] != sum || seen[p] {
			bad = true
		}
		seen[p] = true
	}
	if bad || len(seen) != len(want) {
		return fmt.Errorf("publication: review.md does not record every candidate source hash")
	}
	exits, bad := fieldValues(report, "Verification exit")
	labels := map[string]int{}
	for _, e := range exits {
		m := lightReviewExitRE.FindStringSubmatch(e)
		if m == nil || !strings.Contains(m[3], "./...") || strings.Contains(m[3], "coverprofile") && !strings.Contains(m[3], reviewScratch+"/"+taskID+"/") {
			bad = true
			continue
		}
		labels[m[2]]++
	}
	if bad || len(labels) != len(lightExitLabels) {
		return fmt.Errorf("publication: review.md does not record the six light verification commands and exits in this task's scratch")
	}
	for _, n := range labels {
		if n != 1 {
			return fmt.Errorf("publication: review.md repeats a light verification line")
		}
	}
	if _, err := one("Coverage comparison:"); err != nil {
		return fmt.Errorf("publication: review.md does not record the coverage comparison")
	}
	if c, err := one("Cleanup:"); err != nil || strings.TrimSpace(c) == "" {
		return fmt.Errorf("publication: review.md does not record the cleanup result")
	}
	return nil
}

// lightReviewInput is the light derivation's trusted input: the shared
// review input, the seed tree and the supervisor's independent seed CLI
// corroboration.
type lightReviewInput struct {
	reviewEvidenceInput
	SeedFiles map[string]TreeFile
	SeedTree  string
	SeedCLI   bool
}

// DeriveLightReviewEvidence derives one light code review's execution
// evidence: the shared top-level vendor record binding with the exact
// light argv, then the light receipt cross-checked against the plan, task
// and objects with its tests, red/green and coverage recomputed.
func DeriveLightReviewEvidence(in lightReviewInput) DevReviewEvidence {
	ev := DevReviewEvidence{Ordinal: in.Ordinal, Task: in.Task, Marker: in.Marker, LogSHA256: sha256Hex(in.Log), LogBytes: len(in.Log),
		Approving: in.Marker == markerReviewApproved}
	set := func(status, code string) DevReviewEvidence {
		ev.Status, ev.Code = status, code
		return ev
	}
	if !in.Meta.complete() {
		return set(evidenceUnobserved, "review_log_incomplete")
	}
	cmds, err := parseCodexCommands(in.Log)
	if err != nil {
		return set(evidenceUnobserved, "review_log_ambiguous")
	}
	argv := in.Helper.argvFor(scenarioLight, in.Task, in.Candidate, in.Initial)
	var started, completed []codexCommand
	for _, c := range cmds {
		if !mentionsHelper(c.command) {
			continue
		}
		if !helperInvocation(c.command, argv) {
			return set(evidenceMismatch, "review_command_mismatch")
		}
		if c.kind == "item.started" {
			started = append(started, c)
		} else {
			completed = append(completed, c)
		}
	}
	if len(started) == 0 && len(completed) == 0 {
		return set(evidenceUnobserved, "review_helper_not_started")
	}
	if len(started) != 1 || len(completed) != 1 || started[0].id != completed[0].id || started[0].command != completed[0].command ||
		started[0].line >= completed[0].line {
		return set(evidenceMismatch, "review_command_records_invalid")
	}
	s, c := started[0], completed[0]
	ev.ItemID, ev.Command = c.id, c.command
	ev.Started = &SourceRange{File: in.LogFile, SHA256: ev.LogSHA256, FirstLine: s.line, LastLine: s.line}
	ev.Completed = &SourceRange{File: in.LogFile, SHA256: ev.LogSHA256, FirstLine: c.line, LastLine: c.line}
	ev.HelperExit = c.exit
	if c.status != "completed" || c.exit == nil {
		return set(evidenceMismatch, "review_command_incomplete")
	}
	out := []byte(c.output)
	i := bytes.IndexByte(out, '\n')
	if i < 0 {
		ev.RunSHA256, ev.RunBytes = sha256Hex(out), len(out)
		return set(evidenceUnobserved, "review_receipt_incomplete")
	}
	receipt, trailer := out[:i+1], out[i+1:]
	ev.RunSHA256, ev.RunBytes = sha256Hex(receipt), len(receipt)
	if len(receipt) > maxReviewRunBytes || len(trailer) > maxReviewTrailerBytes {
		return set(evidenceUnobserved, "review_receipt_incomplete")
	}
	var run DevLightReviewRun
	if err := decodeStrict(out[:i], &run); err != nil || run.Schema != DevLightReviewRunSchema || run.Scenario != scenarioLight {
		// A full receipt (or any other) never substitutes for a light one.
		return set(evidenceMismatch, "review_receipt_scenario")
	}
	ev.RunStatus = run.Status
	if code := checkLightReviewRun(in, run, *c.exit); code != "" {
		return set(evidenceMismatch, code)
	}
	ev.RejectedEntry = run.RejectedEntry
	ev.Light = &LightReviewFacts{Comparison: run.Comparison, Baseline: run.BaselineCoverage, Candidate: run.CandidateCoverage, RedCode: run.RedCode,
		GreenCode: run.GreenCode}
	if want, ok := renderLightReviewReportLines(run, receipt); !ok || want != string(trailer) {
		return set(evidenceMismatch, "review_report_lines_mismatch")
	}
	if code := checkLightTrailerClaims(in.Report, string(trailer)); code != "" {
		return set(evidenceMismatch, code)
	}
	if run.Status != reviewPassed {
		if ev.Approving {
			return set(evidenceMismatch, "review_approved_failed_run")
		}
		return set(evidenceFailedRun, run.Code)
	}
	if !in.SeedCLI {
		// The receipt's red is corroborated by the supervisor's own seed
		// CLI whitespace result, or an approval has no verified red.
		return set(evidenceMismatch, CodeRegressionInvalidRed)
	}
	return set(evidenceVerified, "")
}

// LightReviewFacts are a light review evidence's derived facts.
type LightReviewFacts struct {
	Comparison LightCoverageComparison `json:"comparison"`
	Baseline   *LightCoverage          `json:"baseline"`
	Candidate  *LightCoverage          `json:"candidate"`
	RedCode    string                  `json:"red_code"`
	GreenCode  string                  `json:"green_code"`
}

// checkLightReviewRun cross-checks a light receipt against the trusted
// plan, task and objects and recomputes its derived claims.
func checkLightReviewRun(in lightReviewInput, run DevLightReviewRun, exit int) string {
	refused := run.Status == reviewRefused
	switch {
	case run.Task != in.Task || run.Candidate != in.Candidate || run.InitialSource != in.Initial:
		return "review_receipt_binding"
	case run.Go.Path != in.Helper.Go || run.Deadline != in.Helper.Deadline:
		return "review_receipt_binding"
	case !hex64RE.MatchString(run.Go.SHA256) && (!refused || run.Go.SHA256 != ""):
		return "review_receipt_binding"
	case run.InputTree != in.CandTree && (!refused || run.InputTree != ""):
		return "review_receipt_binding"
	case run.SeedTree != in.SeedTree && (!refused || run.SeedTree != ""):
		return "review_receipt_binding"
	case !filepath.IsAbs(run.Scratch) || filepath.Clean(run.Scratch) != run.Scratch || !strings.HasSuffix(run.Scratch, "/"+reviewScratch+"/"+in.Task):
		return "review_receipt_binding"
	}
	inputs := []ReviewInputFile{}
	for _, name := range sortedTree(in.CandFiles) {
		if name != "review.md" {
			inputs = append(inputs, ReviewInputFile{Path: name, SHA256: sha256Hex(in.CandFiles[name].Content), Mode: filemode.Regular.String()})
		}
	}
	if !slices.Equal(run.Inputs, inputs) && (!refused || len(run.Inputs) != 0) {
		return "review_inputs_mismatch"
	}
	if !refused && (run.FixSHA256 != fileSHA(in.CandFiles, "fix.md") || run.RCASHA256 != fileSHA(in.CandFiles, "rca.md") ||
		run.TestSHA256 != fileSHA(in.CandFiles, "main_test.go")) {
		return "review_inputs_mismatch"
	}
	// The exports: exactly the recipe's three, from the Git sources, on
	// every run that created its scratch (none on a refusal); intact only
	// when each held exactly those files once written and after the
	// commands.
	exportsIntact := len(run.Exports) == 3
	if refused {
		if len(run.Exports) != 0 {
			return "review_inputs_mismatch"
		}
	} else {
		want := lightExportFiles(in.SeedFiles, in.CandFiles)
		if len(run.Exports) != len(want) {
			return "review_inputs_mismatch"
		}
		for i, e := range want {
			x := run.Exports[i]
			if x.Name != e[0].(string) || !slices.Equal(x.Inputs, lightInputs(e[1].(map[string][]byte))) || x.Created == nil || x.After == nil {
				return "review_inputs_mismatch"
			}
			exportsIntact = exportsIntact && slices.Equal(x.Created, x.Inputs) && slices.Equal(x.After, x.Inputs)
		}
	}
	start, ok1 := parseTime(run.Start)
	end, ok2 := parseTime(run.End)
	deadline, ok3 := parseHelperDeadline(run.Deadline)
	if !ok1 || !ok2 || !ok3 || end.Before(start) || start.Before(in.AdmittedAt) || !end.Before(in.AdmittedAt.Add(FeatureTaskBound)) ||
		end.After(deadline) || (!in.FinishedAt.IsZero() && end.After(in.FinishedAt)) {
		return "review_receipt_time"
	}
	last := start
	for i, c := range run.Commands {
		if i >= len(lightReviewCommandNames) || c.Name != lightReviewCommandNames[i] || !slices.Equal(c.Argv, lightReviewLogicalArgv(c.Name, run.Scratch)) ||
			c.Cwd != lightReviewCwd(c.Name, run.Scratch) {
			return "review_command_mismatch"
		}
		cs, ok1 := parseTime(c.Start)
		ce, ok2 := parseTime(c.End)
		if !ok1 || !ok2 || cs.Before(last) || ce.Before(cs) || end.Before(ce) {
			return "review_receipt_time"
		}
		last = ce
	}
	for _, p := range []ReviewProfile{run.BaselineProfile, run.CandidateProfile} {
		if p.Present && p.SHA256 != sha256Hex([]byte(p.Text)) || !p.Present && (p.SHA256 != "" || p.Text != "") {
			return "review_profile_mismatch"
		}
	}
	derived := run
	lightReviewDerive(&derived, fileContent(in.SeedFiles, "main.go"), fileContent(in.CandFiles, "main.go"), modulePathOf(fileContent(in.CandFiles, "go.mod")))
	switch {
	case !bytes.Equal(mustJSON(derived.Red), mustJSON(run.Red)) || !bytes.Equal(mustJSON(derived.Green), mustJSON(run.Green)) ||
		derived.RedCode != run.RedCode || derived.GreenCode != run.GreenCode:
		return "review_tests_mismatch"
	case !bytes.Equal(mustJSON(derived.BaselineCoverage), mustJSON(run.BaselineCoverage)) ||
		!bytes.Equal(mustJSON(derived.CandidateCoverage), mustJSON(run.CandidateCoverage)) ||
		!bytes.Equal(mustJSON(derived.Comparison), mustJSON(run.Comparison)):
		return "review_coverage_mismatch"
	}
	ok := len(run.Commands) == len(lightReviewCommandNames) && run.RedCode == "" && run.GreenCode == "" && run.Comparison.Pass &&
		run.ScratchCreated && run.ScratchRemoved && run.InputsUnchanged && slices.Equal(run.InputsAfter, run.Inputs) && !run.OutputsOmitted && exportsIntact
	for _, c := range run.Commands {
		want := 0
		if c.Name == "red" {
			want = 1
		}
		ok = ok && c.Started && !c.TimedOut && !c.Truncated && c.ProcessGone && c.Exit == want && c.Signal == ""
	}
	if (run.Status == reviewPassed) != ok || (run.Status == reviewPassed) != (exit == 0) || (exit != 0 && exit != 1) {
		return "review_status_mismatch"
	}
	if run.Status != reviewPassed && run.Status != reviewFailed && run.Status != reviewRefused {
		return "review_status_mismatch"
	}
	if e := run.RejectedEntry; e != nil && (run.Status == reviewPassed || e.Name == "" || e.Name == "." || e.Name == ".." ||
		strings.ContainsAny(e.Name, "/\x00") || !slices.Contains(reviewEntryKinds, e.Kind)) {
		return "review_rejected_entry_invalid"
	}
	return ""
}
