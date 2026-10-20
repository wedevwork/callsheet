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
	at                                        time.Time
}

// parseReviewArgs validates the helper's exact flags.
func parseReviewArgs(args []string) (reviewArgs, error) {
	v, ok := helperFlags(args, "task", "candidate", "initial-source", "go", "deadline")
	if !ok {
		return reviewArgs{}, errors.New("dev-review-verify needs exactly --task ID --candidate COMMIT --initial-source COMMIT --go ABS --deadline RFC3339")
	}
	a := reviewArgs{task: v["task"], candidate: v["candidate"], initial: v["initial-source"], goBin: v["go"], deadline: v["deadline"]}
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
	rec := runReviewVerify(*env, a)
	out, code := encodeReviewRun(rec)
	h.Stdout.Write(out)
	return code
}

// encodeReviewRun renders the receipt as one compact JSON line within its
// bound; an over-bound receipt drops the command outputs and fails.
func encodeReviewRun(rec DevReviewRun) ([]byte, int) {
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
		return out, 0
	}
	return out, 1
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
	if now, err := snapshotReviewInputs(env.cwd); err == nil {
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
}

// inspectReviewCheckout validates the checkout before anything runs: a
// real directory, no pre-existing scratch, HEAD at the candidate, both
// commits present with the initial source an ancestor, every input equal
// to the pinned candidate (review.md may be in progress) and nothing
// else.
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
	for _, f := range seed {
		if f.Path == "main.go" {
			rc.seedMain = f.Content
		}
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return rc, failure("checkout_unsafe", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if name == ".git" {
			continue
		}
		f, inCandidate := rc.files[name]
		if !inCandidate && name != "review.md" {
			return rc, failure("unexpected_file", fmt.Errorf("unexpected checkout entry %s", contract.SafeText(name, 64)))
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
	rc.inputs, err = snapshotReviewInputs(cwd)
	if err != nil {
		return rc, failure("input_modified", err)
	}
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
// .git with its digest and mode.
func snapshotReviewInputs(cwd string) ([]ReviewInputFile, error) {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return nil, err
	}
	out := []ReviewInputFile{}
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || name == "review.md" || name == reviewScratch {
			continue
		}
		st, err := os.Lstat(filepath.Join(cwd, name))
		if err != nil {
			return nil, err
		}
		b, err := readBounded(filepath.Join(cwd, name), maxArtifactBytes)
		if err != nil {
			return nil, err
		}
		out = append(out, ReviewInputFile{Path: name, SHA256: sha256Hex(b), Mode: gitModeOf(st)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
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
