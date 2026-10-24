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
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The fixed reviewer verification helper (design 12a-dev-flow-verify,
// Reviewer evidence): reale2e dev-review-verify, run by the code reviewer
// once from its task checkout. It is a fixture-specific test runner, not a
// reviewer: it validates its exact arguments and the checkout before
// running anything, exports the pinned candidate with bounded go-git
// object reads into a fresh .callsheet-review-scratch/<task>/after/, runs
// the two parent-required test commands and go test -json there with the
// pinned Go executable in an isolated offline environment with fresh
// caches, derives the changed-code coverage, removes only the scratch it
// created and verifies its absence and the unchanged inputs, then prints
// one bounded JSON receipt (exit 0 passed, 1 executed failure or refusal,
// 2 invalid invocation). It never runs a command or path taken from a
// report.

// Reviewer evidence schemas and bounds.
const (
	DevReviewRunSchema = "callsheet-dev-full-review-run/v1"
	maxReviewRunBytes  = 64 << 10
	reviewHelperBound  = 10 * time.Minute
	// reviewStreamCap bounds each captured command stream (the receipt as
	// a whole stays within maxReviewRunBytes).
	reviewStreamCap = 16 << 10
	// helperDeadlineLayout is the only accepted --deadline spelling.
	helperDeadlineLayout = "2006-01-02T15:04:05Z"
)

// Reviewer evidence codes.
const (
	CodeReviewUnobserved = "review_execution_unobserved"
	CodeReviewMismatch   = "review_evidence_mismatch"
)

// reviewHelperSpec is the envelope's fixed helper recipe: the reale2e
// executable, the pinned Go executable and the attempt deadline.
type reviewHelperSpec struct {
	Reale2e  string `json:"reale2e"`
	Go       string `json:"go"`
	Deadline string `json:"deadline"`
}

// argv is the helper's exact argument vector for task.
func (h reviewHelperSpec) argv(task, candidate, initial string) []string {
	return []string{h.Reale2e, "dev-review-verify", "--task", task, "--candidate", candidate, "--initial-source", initial, "--go", h.Go,
		"--deadline", h.Deadline}
}

// command is argv as one simple command line.
func (h reviewHelperSpec) command(task, candidate, initial string) string {
	return strings.Join(h.argv(task, candidate, initial), " ")
}

// argvFor is scenario's exact helper argv: a light plan's appends
// --scenario light (the full flow's stays the historical argv).
func (h reviewHelperSpec) argvFor(scenario, task, candidate, initial string) []string {
	argv := h.argv(task, candidate, initial)
	if scenario == scenarioLight {
		argv = append(argv, "--scenario", scenarioLight)
	}
	return argv
}

// commandFor is argvFor as one simple command line.
func (h reviewHelperSpec) commandFor(scenario, task, candidate, initial string) string {
	return strings.Join(h.argvFor(scenario, task, candidate, initial), " ")
}

// formatHelperDeadline renders t for --deadline: UTC, whole seconds,
// rounded down (never extended).
func formatHelperDeadline(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(helperDeadlineLayout)
}

// helperDeadlineRE is the exact --deadline spelling.
var helperDeadlineRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// parseHelperDeadline accepts exactly RFC 3339 UTC at second precision
// with a Z suffix; another spelling of the same instant is refused.
func parseHelperDeadline(s string) (time.Time, bool) {
	if !helperDeadlineRE.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(helperDeadlineLayout, s)
	return t, err == nil && t.Format(helperDeadlineLayout) == s
}

// ReviewInputFile is one checkout input's digest and mode.
type ReviewInputFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

// ReviewTool is the pinned Go executable.
type ReviewTool struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

// ReviewCommand is one executed helper command.
type ReviewCommand struct {
	Name        string   `json:"name"`
	Argv        []string `json:"argv"`
	Cwd         string   `json:"cwd"`
	Started     bool     `json:"started"`
	Exit        int      `json:"exit"`
	Signal      string   `json:"signal"`
	TimedOut    bool     `json:"timed_out"`
	Truncated   bool     `json:"truncated"`
	ProcessGone bool     `json:"process_gone"`
	Stdout      string   `json:"stdout"`
	Stderr      string   `json:"stderr"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
}

// ReviewProfile is the coverage profile the helper read back.
type ReviewProfile struct {
	Present bool   `json:"present"`
	SHA256  string `json:"sha256"`
	Text    string `json:"text"`
}

// ReviewTests is the go test -json verdict.
type ReviewTests struct {
	Required []string `json:"required"`
	Missing  []string `json:"missing"`
	Failures []string `json:"failures"`
	Passed   bool     `json:"passed"`
}

// DevReviewRun is the helper's JSON receipt.
type DevReviewRun struct {
	Schema            string            `json:"schema"`
	Task              string            `json:"task"`
	Candidate         string            `json:"candidate"`
	InitialSource     string            `json:"initial_source"`
	InputTree         string            `json:"input_tree"`
	Inputs            []ReviewInputFile `json:"inputs"`
	InputsAfter       []ReviewInputFile `json:"inputs_after"`
	Go                ReviewTool        `json:"go"`
	Env               []string          `json:"env"`
	Start             string            `json:"start"`
	End               string            `json:"end"`
	Deadline          string            `json:"deadline"`
	EffectiveDeadline string            `json:"effective_deadline"`
	Scratch           string            `json:"scratch"`
	Commands          []ReviewCommand   `json:"commands"`
	Profile           ReviewProfile     `json:"profile"`
	Coverage          *DevCoverage      `json:"coverage"`
	Tests             ReviewTests       `json:"tests"`
	ScratchCreated    bool              `json:"scratch_created"`
	ScratchRemoved    bool              `json:"scratch_removed"`
	InputsUnchanged   bool              `json:"inputs_unchanged"`
	OutputsOmitted    bool              `json:"outputs_omitted"`
	Status            string            `json:"status"`
	Code              string            `json:"code"`
	// RejectedEntry names the checkout entry of an unexpected_file
	// refusal or final-check failure (design 12a-dev-flow-verify r0.10;
	// absent otherwise, and from older receipts).
	RejectedEntry *ReviewRejectedEntry `json:"rejected_entry,omitempty"`
	// Export is the after/ export's manifests (absent from older
	// receipts and from a refusal).
	Export *ReviewExport `json:"export,omitempty"`
}

// ReviewRejectedEntry is an unexpected_file receipt's rejected checkout
// entry: its exact root basename and its kind (reviewEntryKinds).
type ReviewRejectedEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Review run statuses.
const (
	reviewPassed  = "passed"
	reviewFailed  = "failed"
	reviewRefused = "refused"
)

// reviewCommandNames are the helper's commands in order.
var reviewCommandNames = []string{"go_version", "go_test", "go_test_cover", "go_test_json"}

// reviewLogicalArgv is command name's logical argv (go first) for scratch.
func reviewLogicalArgv(name, scratch string) []string {
	switch name {
	case "go_version":
		return []string{"go", "version"}
	case "go_test":
		return []string{"go", "test", "./..."}
	case "go_test_cover":
		return []string{"go", "test", "-coverprofile=" + scratch + "/coverage.out", "./..."}
	}
	return []string{"go", "test", "-json", "-count=1", "./..."}
}

// reviewEnv is the helper's injected world: its checkout, clock, process
// launcher and PATH.
type reviewEnv struct {
	cwd      string
	clock    Clock
	launcher Launcher
	path     string
}

// liveReviewEnv is the live helper world.
func liveReviewEnv() (reviewEnv, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return reviewEnv{}, err
	}
	return reviewEnv{cwd: cwd, clock: realClock{}, launcher: ExecLauncher{}, path: os.Getenv("PATH")}, nil
}

// reviewArgs are the helper's validated arguments.
type reviewArgs struct {
	task, candidate, initial, goBin, deadline string
	// scenario is full (also when --scenario is omitted: the historical
	// invocation) or light.
	scenario string
	at       time.Time
}

// parseReviewArgs validates the helper's exact flags.
func parseReviewArgs(args []string) (reviewArgs, error) {
	v, ok := helperFlags(args, "task", "candidate", "initial-source", "go", "deadline", "scenario")
	if !ok {
		v, ok = helperFlags(args, "task", "candidate", "initial-source", "go", "deadline")
	}
	if !ok {
		return reviewArgs{}, errors.New("dev-review-verify needs exactly --task ID --candidate COMMIT --initial-source COMMIT --go ABS --deadline RFC3339 [--scenario full|light]")
	}
	a := reviewArgs{task: v["task"], candidate: v["candidate"], initial: v["initial-source"], goBin: v["go"], deadline: v["deadline"],
		scenario: cmpOr(v["scenario"], scenarioFull)}
	if s, set := v["scenario"]; set && s != scenarioFull && s != scenarioLight {
		return a, errors.New("--scenario must be full or light")
	}
	var dok bool
	a.at, dok = parseHelperDeadline(a.deadline)
	switch {
	case !contract.ValidTaskID(a.task):
		return a, errors.New("--task is not a task ID")
	case !hex40RE.MatchString(a.candidate) || !hex40RE.MatchString(a.initial):
		return a, errors.New("--candidate and --initial-source must be full lowercase commit IDs")
	case !filepath.IsAbs(a.goBin) || filepath.Clean(a.goBin) != a.goBin || validRuleInputs(a.goBin) != nil:
		return a, errors.New("--go must be an absolute plain path")
	case !dok:
		return a, errors.New("--deadline must be RFC 3339 UTC at second precision (YYYY-MM-DDTHH:MM:SSZ)")
	}
	return a, nil
}

// reviewVerifyHelper is reale2e dev-review-verify.
func reviewVerifyHelper(h Host) int {
	a, err := parseReviewArgs(h.Args[1:])
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e dev-review-verify: %v\n", err)
		return 2
	}
	env := h.review
	if env == nil {
		live, err := liveReviewEnv()
		if err != nil {
			fmt.Fprintf(h.Stderr, "reale2e dev-review-verify: %v\n", err)
			return 2
		}
		env = &live
	}
	// The fixed recipe is selected by the validated scenario, never by
	// bundle or checkout text.
	if a.scenario == scenarioLight {
		out, code := lightHelperStdout(runLightReviewVerify(*env, a))
		h.Stdout.Write(out)
		return code
	}
	rec := runReviewVerify(*env, a)
	out, code := reviewHelperStdout(rec)
	h.Stdout.Write(out)
	return code
}

// reviewHelperStdout is the helper's whole stdout (design 12a-dev-flow-verify
// r0.11): the finalized, bounded JSON receipt line, then the paste-ready
// report-line trailer the shared renderer derives from it. A receipt
// whose trailer cannot be rendered completely within its bound fails
// closed: a passing receipt becomes a failed one (report_lines_invalid)
// and is finalized again, and a trailer that still cannot be rendered is
// omitted rather than emitted partially.
func reviewHelperStdout(rec DevReviewRun) ([]byte, int) {
	out, final, code := encodeReviewRunRec(rec)
	trailer, ok := renderReviewReportLines(final, out)
	if !ok || final.Status == reviewPassed && !reportLinesComplete(trailer) {
		if final.Status == reviewPassed {
			final.Status, final.Code = reviewFailed, "report_lines_invalid"
		}
		out, final, code = encodeReviewRunRec(final)
		if trailer, ok = renderReviewReportLines(final, out); !ok {
			trailer = ""
		}
	}
	return append(out, trailer...), code
}

// encodeReviewRun renders the receipt as one compact JSON line within its
// bound; an over-bound receipt drops the command outputs and fails.
func encodeReviewRun(rec DevReviewRun) ([]byte, int) {
	out, _, code := encodeReviewRunRec(rec)
	return out, code
}

// encodeReviewRunRec is encodeReviewRun returning the finalized receipt.
func encodeReviewRunRec(rec DevReviewRun) ([]byte, DevReviewRun, int) {
	enc := func(r DevReviewRun) []byte {
		b, _ := json.Marshal(r)
		return append(b, '\n')
	}
	out := enc(rec)
	if len(out) > maxReviewRunBytes {
		rec.OutputsOmitted, rec.Status, rec.Code = true, reviewFailed, "receipt_overflow"
		for i := range rec.Commands {
			rec.Commands[i].Stdout, rec.Commands[i].Stderr = "", ""
		}
		rec.Profile.Text = ""
		out = enc(rec)
	}
	if rec.Status == reviewPassed {
		return out, rec, 0
	}
	return out, rec, 1
}

// runReviewVerify is the helper's whole run.
func runReviewVerify(env reviewEnv, a reviewArgs) DevReviewRun {
	start := env.clock.Now()
	eff := minTime(a.at, start.Add(reviewHelperBound))
	scratchRoot := filepath.Join(env.cwd, reviewScratch)
	scratch := filepath.Join(scratchRoot, a.task)
	rec := DevReviewRun{Schema: DevReviewRunSchema, Task: a.task, Candidate: a.candidate, InitialSource: a.initial, Inputs: []ReviewInputFile{},
		InputsAfter: []ReviewInputFile{}, Env: []string{}, Start: formatTime(start), Deadline: a.deadline, EffectiveDeadline: formatTime(eff),
		Scratch: scratch, Commands: []ReviewCommand{}, Tests: ReviewTests{Required: requiredTestNames(), Missing: []string{}, Failures: []string{}},
		Go: ReviewTool{Path: a.goBin}}
	finish := func(status, code string) DevReviewRun {
		if rec.Status == "" {
			rec.Status, rec.Code = status, code
		}
		rec.End = formatTime(env.clock.Now())
		return rec
	}
	refuse := func(code string) DevReviewRun { return finish(reviewRefused, code) }
	in, err := inspectReviewCheckout(env.cwd, a)
	if err != nil {
		// The rejected entry of an unexpected_file refusal is kept beside
		// the code (r0.10).
		rec.RejectedEntry = in.rejected
		return refuse(codeOf(err))
	}
	rec.InputTree, rec.Inputs = in.tree, in.inputs
	gob, err := readBounded(a.goBin, 1<<30)
	if err != nil {
		return refuse("go_unreadable")
	}
	rec.Go.SHA256 = sha256Hex(gob)
	if env.clock.Now().Compare(eff) >= 0 {
		return refuse("deadline_passed")
	}
	// The scratch: created fresh (refused above when it existed), the
	// candidate exported into after/, the caches private to it.
	if err := os.Mkdir(scratchRoot, 0o700); err != nil {
		return refuse("scratch_unavailable")
	}
	rec.ScratchCreated = true
	after := filepath.Join(scratch, "after")
	failed := ""
	fail := func(code string) {
		if failed == "" {
			failed = code
		}
	}
	if err := exportReviewTree(after, in.files); err != nil {
		fail("export_failed")
	}
	// The export as written, against the pinned objects (C2).
	wantExport := treeInputs(in.files)
	rec.Export = &ReviewExport{Created: exportManifest(after), After: []LightInput{}}
	if !slices.Equal(rec.Export.Created, wantExport) {
		fail("export_mismatch")
	}
	envDir := filepath.Join(scratch, "env")
	for _, d := range []string{"gocache", "gomodcache", "gopath", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(envDir, d), 0o700); err != nil {
			fail("scratch_unavailable")
		}
	}
	rec.Env = testEnv(envDir, env.path)
	gone := true
	if failed == "" {
		for _, name := range reviewCommandNames {
			c := runReviewCommand(env, a.goBin, name, scratch, after, rec.Env, eff)
			rec.Commands = append(rec.Commands, c)
			gone = gone && (c.ProcessGone || !c.Started)
			switch {
			case !c.Started:
				fail("command_not_started")
			case c.TimedOut:
				fail("command_timed_out")
			case !c.ProcessGone:
				fail("process_not_gone")
			case c.Truncated:
				fail("output_truncated")
			case c.Exit != 0 || c.Signal != "":
				fail("command_failed")
			}
			if !c.Started || c.TimedOut || !c.ProcessGone {
				break
			}
		}
	}
	// The export after the commands: a worker-run command that changed,
	// removed or added an entry fails the run (C2).
	rec.Export.After = exportManifest(after)
	if !slices.Equal(rec.Export.After, wantExport) {
		fail("export_changed")
	}
	if len(rec.Commands) > 0 {
		rec.Go.Version = strings.TrimSpace(rec.Commands[0].Stdout)
	}
	if p, err := readBounded(filepath.Join(scratch, "coverage.out"), maxProfileBytes); err == nil {
		rec.Profile = ReviewProfile{Present: true, SHA256: sha256Hex(p), Text: string(p)}
	}
	if rec.Profile.Present {
		cov, err := deriveChangedCoverage(in.seedMain, fileContent(in.files, "main.go"), []byte(rec.Profile.Text), modulePathOf(fileContent(in.files, "go.mod")))
		if err == nil {
			rec.Coverage = &cov
		}
	}
	switch {
	case rec.Coverage == nil:
		fail("coverage_unobserved")
	case !rec.Coverage.Pass:
		fail("coverage_below_bar")
	}
	if c := commandNamed(rec.Commands, "go_test_json"); c != nil {
		rec.Tests = reviewTestsOf([]byte(c.Stdout), *c)
	}
	if !rec.Tests.Passed {
		fail("tests_failed")
	}
	// Remove only the scratch this run created, and only once every child
	// was proved gone; then verify its absence and the unchanged inputs.
	if gone {
		if err := os.RemoveAll(scratchRoot); err == nil {
			_, err := os.Lstat(scratchRoot)
			rec.ScratchRemoved = errors.Is(err, fs.ErrNotExist)
		}
	}
	if !rec.ScratchRemoved {
		fail("cleanup_unverified")
	}
	// The final check classifies the checkout's entries as the initial
	// inspection did: an entry turned invalid fails unexpected_file with
	// the entry, never inputs unchanged.
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

// commandNamed returns the command record named name.
func commandNamed(cs []ReviewCommand, name string) *ReviewCommand {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

// requiredTestNames are the ten required TestRun subtests.
func requiredTestNames() []string {
	var out []string
	for _, c := range FeatureCases {
		out = append(out, "TestRun/"+c.ID)
	}
	return out
}

// reviewTestsOf judges a go test -json stream: the whole run passed and
// every required subtest passed (none skipped or missing).
func reviewTestsOf(stream []byte, c ReviewCommand) ReviewTests {
	t := ReviewTests{Required: requiredTestNames(), Missing: []string{}, Failures: []string{}}
	ex := ExecRecord{Started: c.Started, ExitCode: c.Exit, Signal: c.Signal, TimedOut: c.TimedOut, Truncated: c.Truncated}
	passed, failures := testRunVerdict(ex, stream)
	t.Failures = failures
	results := testResults(stream)
	for _, name := range t.Required {
		if r, ok := results[name]; !ok || r.action != "pass" {
			t.Missing = append(t.Missing, name)
		}
	}
	t.Passed = passed && len(t.Missing) == 0
	return t
}

// runReviewCommand runs one fixed helper command in dir before the
// effective deadline.
func runReviewCommand(env reviewEnv, goBin, name, scratch, dir string, procEnv []string, eff time.Time) ReviewCommand {
	logical := reviewLogicalArgv(name, scratch)
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

// reviewCheckout is the validated checkout.
type reviewCheckout struct {
	tree     string
	files    map[string]TreeFile
	inputs   []ReviewInputFile
	seedMain []byte
	// seedFiles and seedTree are the initial source's tree (the light
	// recipe's baseline and before exports).
	seedFiles map[string]TreeFile
	seedTree  string
	// rejected is an unexpected_file refusal's entry.
	rejected *ReviewRejectedEntry
}

// inspectReviewCheckout validates the checkout before anything runs: a
// real directory, no pre-existing scratch, HEAD at the candidate, both
// commits present with the initial source an ancestor, every input equal
// to the pinned candidate (review.md may be in progress) and nothing
// else but the checkout-only entries (checkoutOnly).
func inspectReviewCheckout(cwd string, a reviewArgs) (reviewCheckout, error) {
	var rc reviewCheckout
	canon, err := filepath.EvalSymlinks(cwd)
	if err != nil || canon != cwd {
		return rc, failure("checkout_unsafe", errors.New("the checkout path is not canonical"))
	}
	if st, err := os.Lstat(filepath.Join(cwd, ".git")); err != nil || !st.IsDir() {
		return rc, failure("checkout_unsafe", errors.New("the checkout has no real .git directory"))
	}
	if _, err := os.Lstat(filepath.Join(cwd, reviewScratch)); !errors.Is(err, fs.ErrNotExist) {
		return rc, failure("scratch_preexisting", errors.New("the review scratch exists already"))
	}
	repo, err := git.PlainOpen(cwd)
	if err != nil {
		return rc, failure("checkout_unsafe", err)
	}
	head, err := repo.Head()
	if err != nil || head.Hash().String() != a.candidate {
		return rc, failure("checkout_head_mismatch", errors.New("HEAD is not the candidate"))
	}
	cand, err := repo.CommitObject(plumbing.NewHash(a.candidate))
	if err != nil {
		return rc, failure("commit_missing", err)
	}
	files, err := readTree(repo, cand, "candidate")
	if err != nil {
		return rc, failure("candidate_invalid", err)
	}
	rc.tree, rc.files = cand.TreeHash.String(), map[string]TreeFile{}
	for _, f := range files {
		rc.files[f.Path] = f
	}
	initial, err := repo.CommitObject(plumbing.NewHash(a.initial))
	if err != nil || !isAncestor(repo, initial.Hash, cand) {
		return rc, failure("initial_source_unrelated", errors.New("the initial source is not an ancestor of the candidate"))
	}
	seed, err := readTree(repo, initial, "initial source")
	if err != nil {
		return rc, failure("candidate_invalid", err)
	}
	rc.seedFiles, rc.seedTree = map[string]TreeFile{}, initial.TreeHash.String()
	for _, f := range seed {
		rc.seedFiles[f.Path] = f
		if f.Path == "main.go" {
			rc.seedMain = f.Content
		}
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return rc, failure("checkout_unsafe", err)
	}
	seen := map[string]bool{}
	runtimeSeen := false
	for _, e := range entries {
		name := e.Name()
		if name == ".git" {
			continue
		}
		f, inCandidate := rc.files[name]
		if !inCandidate && name != "review.md" {
			runtime, rejected := checkoutOnly(cwd, name, runtimeSeen)
			if rejected != nil {
				rc.rejected = rejected
				return rc, failure("unexpected_file", fmt.Errorf("unexpected checkout entry %s", contract.SafeText(name, 64)))
			}
			runtimeSeen = runtimeSeen || runtime
			continue
		}
		st, err := os.Lstat(filepath.Join(cwd, name))
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 != 0 {
			return rc, failure("input_modified", fmt.Errorf("%s is not a regular nonexecutable file", contract.SafeText(name, 64)))
		}
		if name == "review.md" {
			continue
		}
		b, err := readBounded(filepath.Join(cwd, name), maxArtifactBytes)
		if err != nil || !bytes.Equal(b, f.Content) {
			return rc, failure("input_modified", fmt.Errorf("%s differs from the candidate", contract.SafeText(name, 64)))
		}
		seen[name] = true
	}
	for name := range rc.files {
		if !seen[name] && name != "review.md" {
			return rc, failure("input_modified", fmt.Errorf("%s is missing", contract.SafeText(name, 64)))
		}
	}
	inputs, rejected, err := snapshotReviewInputs(cwd, rc.files)
	switch {
	case rejected != nil:
		rc.rejected = rejected
		return rc, failure("unexpected_file", fmt.Errorf("unexpected checkout entry %s", contract.SafeText(rejected.Name, 64)))
	case err != nil:
		return rc, failure("input_modified", err)
	}
	rc.inputs = inputs
	return rc, nil
}

// isAncestor reports whether a is c or one of its ancestors (bounded).
func isAncestor(repo *git.Repository, a plumbing.Hash, c *object.Commit) bool {
	cur := c
	for i := 0; i <= 64; i++ {
		if cur.Hash == a {
			return true
		}
		if len(cur.ParentHashes) != 1 {
			return false
		}
		p, err := repo.CommitObject(cur.ParentHashes[0])
		if err != nil {
			return false
		}
		cur = p
	}
	return false
}

// snapshotReviewInputs lists every checkout input but review.md and
// .git with its digest and mode. An entry outside the candidate (files)
// is a checkout-only entry, validated and left out, or the rejected entry
// (design 12a-dev-flow-verify r0.10).
func snapshotReviewInputs(cwd string, files map[string]TreeFile) ([]ReviewInputFile, *ReviewRejectedEntry, error) {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return nil, nil, err
	}
	out := []ReviewInputFile{}
	runtimeSeen := false
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || name == "review.md" || name == reviewScratch {
			continue
		}
		if _, inCandidate := files[name]; !inCandidate {
			runtime, rejected := checkoutOnly(cwd, name, runtimeSeen)
			if rejected != nil {
				return nil, rejected, nil
			}
			runtimeSeen = runtimeSeen || runtime
			continue
		}
		st, err := os.Lstat(filepath.Join(cwd, name))
		if err != nil {
			return nil, nil, err
		}
		b, err := readBounded(filepath.Join(cwd, name), maxArtifactBytes)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, ReviewInputFile{Path: name, SHA256: sha256Hex(b), Mode: gitModeOf(st)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil, nil
}

// reviewPlaceholders are the root names Codex may leave in a checkout,
// tolerated only as empty real directories (owner decision 2026-10-23).
var reviewPlaceholders = map[string]bool{".agents": true, ".aws": true, ".codex": true}

// reviewEntryKinds are a rejected entry's kinds.
var reviewEntryKinds = []string{"directory", "regular", "symlink", "other", "unknown"}

// entryKind is a checkout entry's kind by Lstat ("unknown" when it cannot
// be determined); a link is never followed.
func entryKind(p string) string {
	st, err := os.Lstat(p)
	switch {
	case err != nil:
		return "unknown"
	case st.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case st.IsDir():
		return "directory"
	case st.Mode().IsRegular():
		return "regular"
	}
	return "other"
}

// checkoutOnly judges a root checkout entry outside the candidate tree
// (never .git or review.md) under design 12a-dev-flow-verify r0.10: at
// most one real directory named by contract.ValidRuntimeDir (Callsheet's
// runtime directory: its contents are never traversed, read or hashed),
// and .agents, .aws and .codex only as real directories with no entries
// at all. runtime reports an accepted runtime directory; anything else,
// a second runtime directory included, is the rejected entry. A read or
// stat error is a rejection, never an empty placeholder.
func checkoutOnly(cwd, name string, runtimeSeen bool) (runtime bool, rejected *ReviewRejectedEntry) {
	p := filepath.Join(cwd, name)
	kind := entryKind(p)
	reject := &ReviewRejectedEntry{Name: name, Kind: kind}
	switch {
	case contract.ValidRuntimeDir(name):
		if kind != "directory" || runtimeSeen {
			return false, reject
		}
		return true, nil
	case reviewPlaceholders[name]:
		if kind != "directory" {
			return false, reject
		}
		if es, err := os.ReadDir(p); err != nil || len(es) != 0 {
			return false, reject
		}
		return false, nil
	}
	return false, reject
}

// gitModeOf is a checkout entry's Git mode: 100644 for a regular
// nonexecutable file, 100755 for an executable one, else its file type.
func gitModeOf(st fs.FileInfo) string {
	switch {
	case st.Mode().IsRegular() && st.Mode().Perm()&0o111 == 0:
		return filemode.Regular.String()
	case st.Mode().IsRegular():
		return filemode.Executable.String()
	}
	return st.Mode().Type().String()
}

// ReviewExport is a scratch export's manifest when it was created and
// after the commands ran (review 2d.4 round 1, C2): every entry the
// export holds, each regular file with its SHA-256. Older receipts have
// none.
type ReviewExport struct {
	Created []LightInput `json:"created"`
	After   []LightInput `json:"after"`
}

// maxExportEntryBytes bounds one export file a manifest digests.
const maxExportEntryBytes = 1 << 20

// exportManifest digests every entry an export directory holds, in path
// order: each regular file by its slash path and SHA-256 (no digest when
// unreadable or over its bound), any other non-directory entry by its path
// and kind with no digest. A missing directory holds nothing.
func exportManifest(dir string) []LightInput {
	out := []LightInput{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			out = append(out, LightInput{Path: rel + " (" + d.Type().String() + ")"})
			return nil
		}
		in := LightInput{Path: rel}
		if b, err := readBounded(p, maxExportEntryBytes); err == nil {
			in.SHA256 = sha256Hex(b)
		}
		out = append(out, in)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// treeInputs is a tree's files as an export manifest.
func treeInputs(files map[string]TreeFile) []LightInput {
	m := map[string][]byte{}
	for n, f := range files {
		m[n] = f.Content
	}
	return lightInputs(m)
}

// exportReviewTree writes the pinned tree into the new directory dir.
func exportReviewTree(dir string, files map[string]TreeFile) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range sortedTree(files) {
		f := files[name]
		if f.Mode != filemode.Regular {
			return errors.New("not a regular file")
		}
		if err := writeExclusive(filepath.Join(dir, name), f.Content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// fileContent is a tree file's bytes (nil when absent).
func fileContent(files map[string]TreeFile, name string) []byte {
	if f, ok := files[name]; ok {
		return f.Content
	}
	return nil
}
