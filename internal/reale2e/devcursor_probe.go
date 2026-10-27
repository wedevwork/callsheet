package reale2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The Cursor-coder qualification probe (design 12a-3-cursor-worker,
// 3a.md, completed by slice 3b). The optional cursor / grok-4.7 / high
// coder selection takes an explicit --cursor executable: the probe deploys
// a fourth sidecar (n-cursor, the coder's node), keeps five authentication
// preflights (the coder's on Cursor), executes one supervisor-owned coder
// fixture task after the models stage and before any coordinator phase,
// then the existing wake, print-notification, reviewer and (light/both)
// triage and light-review stages unchanged. Since slice 3b it writes
// qualification.json under callsheet-dev-cursor-qualification/v1 and is
// QUALIFIED (exit 0) only when that written record passes the independent
// Cursor checker (devcursor_check.go); a slice 3a diagnostic receipt never
// qualifies. The Cursor recipe, minimum version and posture are the
// adapter's, unchanged: no jail, no sandbox, force, yolo or approve-mcps
// flag, no canary and no configuration write.

// Cursor probe names, codes and bounds.
const (
	probeCoderNamespace = "probe-coder"
	probeCoderLabel     = probeCoderNamespace + "-01"
	probeCoderDir       = "probe/coder/01"
	probeCoderRepoDir   = "workspace/coder/repo.git"
	stageCoderEvidence  = "coder_evidence"
	// Paid worker call bounds of the Cursor probe: the default probe's plus
	// the one coder task.
	probeMaxCallsCursorFull  = probeMaxCallsFull + 1
	probeMaxCallsCursorLight = probeMaxCallsLight + 1
	// probeCoderPushBound bounds the supervisor's coder input push.
	probeCoderPushBound = time.Minute

	EnvCursorConfigDir = "CURSOR_CONFIG_DIR"
	EnvXDGConfigHome   = "XDG_CONFIG_HOME"

	CodeCursorAdapterRequired = "cursor_adapter_required"
	CodeCursorAdapterUnused   = "cursor_adapter_unused"
	CodeCursorConfigDir       = "cursor_config_dir_unsupported"
	// CodeCursorPending3b and CodeCursorQualificationPending are the slice
	// 3a refusal and diagnostic-blocker names, retired by slice 3b: no
	// command emits them any more (historical receipts keep theirs).
	CodeCursorPending3b            = "cursor_execution_pending_3b"
	CodeCursorQualificationPending = "cursor_qualification_pending"
	CodeProbeCoderUnqualified      = "probe_coder_unqualified"
	CodeProbeCoderPublication      = "probe_coder_publication_invalid"
	CodeProbeCoderValidation       = "probe_coder_validation_failed"
	CodeProbeCoderPush             = "probe_coder_push_failed"
)

// cursorSidecars are a Cursor probe's four sidecars in start order; the
// idle Grok node stays deployed. cursorVersionVendors are its vendor
// version records in order (go and git follow).
var (
	cursorSidecars       = []string{"codex", "claude", "grok", "cursor"}
	cursorVersionVendors = []string{"claude", "codex", "grok", "cursor"}
	defaultVersionVendor = []string{"claude", "codex", "grok"}
)

// cursorProbeAcceptance is the coder fixture task's acceptance (never
// devAcceptance, whose implement bar allows main_test.go and assumes
// workflow-approved artifacts).
const cursorProbeAcceptance = "Probe-only implement fixture: the supplied design.md is the governing design and only its FP-1 greeting prefix repair is authorized. The existing main_test.go TestRun greeting subtests are FP-1's function test; preserve all existing test cases and bytes. Publish only main.go. Run the existing tests and coverage; require 100% pass and coverage above 80%. Do not add a test, report or approval artifact. Finish with exactly one standalone STATUS: IMPL_COMPLETE only when done, otherwise STATUS: BLOCKED or STATUS: DESIGN_QUESTIONS."

// cursorProbeAuthority is the envelope's narrower explicit authority.
const cursorProbeAuthority = "This is an owner-authorized diagnostic fixture, not an owner feature workflow. The owner's invocation authorizes only this pinned harness fixture and FP-1 repair; it supplies the implement mode's authorization for this probe. No workflow design audit or owner receipt exists or should be invented. The supplied design.md is the governing design. Its existing main_test.go TestRun greeting table is the assigned FP's function test; do not add another test or change its bytes. Input digests below bind the authorized artifacts. If you cannot meet this probe acceptance, return an honest blocker."

// The fixture's one exact repair: the good greeting line, and the input's
// "Pending, " prefix in its place.
const (
	probeCoderGoodLine   = `fmt.Fprintf(stdout, "Hello, %s!\n", *name)`
	probeCoderGoodPrefix = `"Hello, `
	probeCoderBadPrefix  = `"Pending, `
)

// probeCoderFixtureFiles are the coder input's files: the review probe's
// four plus the development seed's FEATURE.md and go.mod.
var probeCoderFixtureFiles = []string{"FEATURE.md", "design.md", "go.mod", "main.go", "main_test.go", "revision-history.md"}

// ProbeCursorBounds and ProbeCursorLightBounds are the Cursor probe's
// cost and time disclosure.
const (
	ProbeCursorBounds = `Cost and time bounds of the Cursor coder qualification probe, --scenario full (policy, not measurements):
  - five paid authentication preflight tasks (the coder's on Cursor), each at most 2m; one
    supervisor-owned Cursor coder fixture task, at most 10m from admission through publication,
    no retry, then its independent validation within one shared 60s budget; three harmless
    designer wake tasks and one code-reviewer task, each at most 10m: at most ten paid worker
    calls, failed calls included, no retries;
  - the headless Claude Code coordinator adds vendor-dependent turns in up to three phases;
  - each owner continuation window at most 20m; the probe at most 90m from preflight start
    (the first owner-environment fingerprint included), then at most 2m of cleanup;
  - a failed stage stops the probe; no token-price ceiling is claimed; QUALIFIED only after the
    written record passes the independent Cursor evidence check, BLOCKED otherwise.
`
	ProbeCursorLightBounds = `Cost and time bounds of the Cursor coder qualification probe, --scenario light or both (policy, not measurements):
  - five paid authentication preflight tasks (the coder's on Cursor), each at most 2m; one
    supervisor-owned Cursor coder fixture task, at most 10m from admission through publication,
    no retry, then its independent validation within one shared 60s budget; three harmless
    designer wake tasks, one code-reviewer task on the full fixture, one triage task and one
    light code-reviewer task, each at most 10m: at most twelve paid worker calls, failed calls
    included, no retries;
  - the headless Claude Code coordinator adds vendor-dependent turns in up to three phases;
  - each owner continuation window at most 20m; the probe at most 90m from preflight start
    (the first owner-environment fingerprint included), then at most 2m of cleanup;
  - a failed stage stops the probe; no token-price ceiling is claimed; QUALIFIED only after the
    written record passes the independent Cursor evidence check, BLOCKED otherwise.
`
	cursorProbeDisclosure = "Cursor probe: QUALIFIED only after independent evidence checking; BLOCKED otherwise. Exit 0 qualified, 1 blocked, 2 usage/gate error. " +
		"No jail and no confinement claim: Cursor runs --trust --auto-review with the sidecar user's access; shallow cache/tmp samples are " +
		"report-only observations of all processes. Historical slice 3a observations are not qualification."
	// cursorAttemptDisclosure is a Cursor attempt's added disclosure.
	cursorAttemptDisclosure = "Optional Cursor coder (cursor grok-4.7/high, coder only): checked " + DevCursorBundleSchema + " evidence; Linux only; " +
		"unchanged --trust --auto-review recipe; no jail or confinement claim; cache/tmp samples are report-only observations of all processes. " +
		"Fresh qualification is an operator prerequisite, not a startup file lookup."
)

// CursorProbeBoundsFor is a Cursor probe scope's disclosure.
func CursorProbeBoundsFor(scope string) string {
	if probeLightScope(scope) {
		return ProbeCursorLightBounds
	}
	return ProbeCursorBounds
}

// cursorProbeStagesFor is a Cursor probe scope's stages in order: the
// scope's stages with coder_evidence after models.
func cursorProbeStagesFor(scope string) []string {
	s := probeStagesFor(scope)
	return slices.Concat(s[:1], []string{stageCoderEvidence}, s[1:])
}

// cursorProbeMaxCalls is a Cursor probe scope's paid worker call bound.
func cursorProbeMaxCalls(scope string) int {
	if probeLightScope(scope) {
		return probeMaxCallsCursorLight
	}
	return probeMaxCallsCursorFull
}

// cursorProbe is the Cursor qualification probe's coder fixture state (the
// run's binding and samples are the supervisor's cursorRun).
type cursorProbe struct {
	input, expected map[string][]byte
	repoPath        string
	inputCommit     plumbing.Hash
	inputTree       plumbing.Hash
	ws              WorkspaceRecord
	task            string
	marker          string
	resultCommit    string
	resultTree      string
	resultPulled    bool
	resultFiles     map[string][]byte
	admitted        bool
	validated       bool
	coverage        *DevCoverage
	reason          string
	// report is the written record's independent check (the offline
	// command's own pure result).
	report *DevCursorQualificationReport
}

// cursorArgPresent reports a --cursor flag among args (read before the
// gate only to select the disclosure it prints).
func cursorArgPresent(args []string) bool {
	for _, a := range args {
		if a == "--cursor" || a == "-cursor" || strings.HasPrefix(a, "--cursor=") || strings.HasPrefix(a, "-cursor=") {
			return true
		}
	}
	return false
}

// peekCursorSelection reads an explicit --flow read-only and reports a
// valid development flow whose coder is Cursor, or the flow's own read or
// parse error (no --flow: the default selection, never Cursor).
func peekCursorSelection(flowPath string) (bool, error) {
	if flowPath == "" {
		return false, nil
	}
	data, err := readBounded(flowPath, maxFlowBytes)
	if err != nil {
		return false, fmt.Errorf("flow: %w", err)
	}
	f, err := ParseDevFullFlow(data)
	if err != nil {
		return false, err
	}
	return f.CursorCoder(), nil
}

// cursorBinding decides the Cursor selection's command binding before any
// capability exists (after the gate and the CLAUDE_CONFIG_DIR refusal),
// for run, run-dev-full, run-dev-light, run-dev-exercise and
// probe-dev-full alike (design 12a-3-cursor-worker, slice 3b FP-3): a
// --cursor beside a flow that does not read or parse is that flow's own
// error (flow_invalid); a Cursor flow requires --cursor and any other flow
// refuses it; a Cursor flow refuses an inherited CURSOR_CONFIG_DIR or
// XDG_CONFIG_HOME, even empty, naming the variable but never its value.
// 0 continues; 2 refuses.
func cursorBinding(h Host, a runArgs) int {
	cmd := h.Args[0]
	sel, perr := peekCursorSelection(a.flow)
	refuse := func(code, msg string) int {
		fmt.Fprintf(h.Stderr, "reale2e %s refused: %s: %s\n", cmd, code, msg)
		return 2
	}
	switch {
	case perr != nil && a.cursor != "":
		// The --cursor binding cannot be decided on a flow that does not
		// read or parse: its own error, not an unused adapter. Without
		// --cursor the run's selection loading reports it as before.
		return refuse("flow_invalid", "the --flow selection could not be read as a development flow, so the --cursor binding is undecided: "+
			contract.SafeText(perr.Error(), 300))
	case sel && a.cursor == "":
		return refuse(CodeCursorAdapterRequired, "the selected coder uses the cursor adapter; pass --cursor with the absolute path of the Cursor executable")
	case !sel && a.cursor != "":
		return refuse(CodeCursorAdapterUnused, "--cursor was given but the selected flow's coder does not use the cursor adapter")
	case !sel:
		return 0
	}
	for _, name := range []string{EnvCursorConfigDir, EnvXDGConfigHome} {
		if _, set := h.LookupEnv(name); set {
			return refuse(CodeCursorConfigDir, name+" is set, so the Cursor executable's effective configuration could differ from the inventoried "+
				"~/.cursor; unset it yourself and start again")
		}
	}
	return 0
}

// ---- Coder stage ----

// cursorCoderStage runs the supervisor-owned coder fixture task and
// records coder_evidence: pass only for a bound, complete IMPL_COMPLETE
// whose publication is exactly the known-good repair and passes the
// independent validation. A failure stops the probe before any
// coordinator phase.
func (d *devSupervisor) cursorCoderStage() error {
	c := d.probe.cursor
	err := d.runCursorCoder()
	code := ""
	if err != nil {
		code = codeOf(err)
		c.reason = contract.SafeText(err.Error(), 400)
	}
	d.writeEvidenceJSON(probeCoderDir+"/verdict.json", envelope("probe-coder-verdict", d.s.runID, cursorCoderVerdict{Task: c.task, Marker: c.marker,
		Code: code, Reason: c.reason, InputCommit: hashOrEmpty(c.inputCommit), ResultCommit: c.resultCommit, PublicationAdmitted: c.admitted,
		ValidationPassed: c.validated}))
	if err != nil {
		d.probe.setStage(stageCoderEvidence, stageFail, code)
		return err
	}
	if d.stopCode != "" {
		d.probe.setStage(stageCoderEvidence, stageFail, d.stopCode)
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	d.probe.setStage(stageCoderEvidence, stagePass, "")
	d.s.say("probe stage %s observed: task %s published the exact repair and passed independent validation (qualification still needs the record's independent check)",
		stageCoderEvidence, c.task)
	return nil
}

// cursorCoderVerdict is probe/coder/01/verdict.json's data.
type cursorCoderVerdict struct {
	Task                string `json:"task"`
	Marker              string `json:"marker"`
	Code                string `json:"code"`
	Reason              string `json:"reason"`
	InputCommit         string `json:"input_commit"`
	ResultCommit        string `json:"result_commit"`
	PublicationAdmitted bool   `json:"publication_admitted"`
	ValidationPassed    bool   `json:"validation_passed"`
}

// hashOrEmpty is h's text, "" for the zero hash.
func hashOrEmpty(h plumbing.Hash) string {
	if h.IsZero() {
		return ""
	}
	return h.String()
}

// halt is the current stop as an error (nil: none).
func (d *devSupervisor) halt() error {
	if d.halted() && d.stopCode != "" {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	return nil
}

// runCursorCoder is the coder stage's work in order: fixture, push,
// dispatch, terminal wait, verdict, publication and validation.
func (d *devSupervisor) runCursorCoder() error {
	c := d.probe.cursor
	if err := d.cursorCoderFixture(); err != nil {
		return err
	}
	if err := d.pushCoderInput(); err != nil {
		return err
	}
	goal := cursorProbeEnvelope(d.s.runID, c.ws, c.inputCommit.String(), c.input)
	instr, runbook, err := d.loaded.ManualPair(HopCoder)
	if err != nil {
		return failure("templates_missing", err)
	}
	if err := checkCursorProbeBudget(instr, runbook, goal, cursorProbeAcceptance); err != nil {
		return failure(CodePromptBudget, err)
	}
	base := c.inputCommit.String()
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: HopCoder}, Goal: goal, Acceptance: cursorProbeAcceptance,
		RequestedBy: requester, Workspace: &c.ws.Name, WorkspaceInstance: &c.ws.Instance, Base: &base}
	if !d.writeEvidenceJSON(probeCoderDir+"/request.json", req) {
		return d.halt()
	}
	sent := d.s.w.Clock.Now()
	id, err := d.s.dispatchOwn(req, sent.Add(FeatureTaskBound))
	if err != nil {
		return err
	}
	c.task = id
	d.s.known[id] = probeCoderLabel
	d.s.ev.event(Event{Type: EvTaskAdmitted, Hop: probeCoderLabel, Task: id})
	v, err := d.s.awaitTerminal(id, FeatureTaskBound, sent)
	if v != nil {
		d.s.storeTask(probeCoderDir, *v)
		d.storeDevLogs(probeCoderDir, id)
	}
	if err != nil {
		return err
	}
	if err := d.halt(); err != nil {
		return err
	}
	if err := d.coderTaskVerdict(*v, req); err != nil {
		return err
	}
	admittedAt, ok := contract.ParseTime(v.CreatedAt)
	if !ok {
		admittedAt = sent
	}
	if err := d.admitCoderPublication(*v, admittedAt); err != nil {
		return err
	}
	return d.validateCoderResult()
}

// cursorCoderFixture builds the fixture: the review probe's good
// main.go, main_test.go, design.md and revision-history.md plus the
// development seed's FEATURE.md and go.mod; the input replaces exactly one
// "Hello, " greeting prefix with "Pending, " in the one greeting line. The
// input and the independently known good main.go are captured inertly
// before anything runs, and the input is committed in a runtime-local
// repository.
func (d *devSupervisor) cursorCoderFixture() error {
	c := d.probe.cursor
	good := map[string][]byte{}
	for _, name := range probeFixtureFiles {
		b, err := readLiteral(filepath.Join(d.s.checkout, filepath.FromSlash(devProbeFixtureDir), name), maxArtifactBytes)
		if err != nil {
			return failure("fixture_missing", fmt.Errorf("coder probe fixture %s: %w", name, err))
		}
		good[name] = b
	}
	gomod, err := readBounded(filepath.Join(d.s.checkout, "go.mod"), 64<<10)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	ver, err := goDirective(gomod)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	seed, err := devSeedFiles(ver)
	if err != nil {
		return failure("seed_failed", err)
	}
	pending, err := probeCoderInputMain(good["main.go"])
	if err != nil {
		return failure("fixture_missing", err)
	}
	c.input = map[string][]byte{"FEATURE.md": seed["FEATURE.md"], "go.mod": seed["go.mod"], "main.go": pending, "main_test.go": good["main_test.go"],
		"design.md": good["design.md"], "revision-history.md": good["revision-history.md"]}
	c.expected = map[string][]byte{"main.go": good["main.go"]}
	rec := cursorCoderFixtureRecord{Line: probeCoderGoodLine, From: probeCoderGoodPrefix, To: probeCoderBadPrefix, Input: []DevProbeSource{},
		Expected: []DevProbeSource{}}
	for _, name := range probeCoderFixtureFiles {
		b := c.input[name]
		path := probeCoderDir + "/input/" + name
		rec.Input = append(rec.Input, DevProbeSource{Path: path, Bytes: len(b), SHA256: sha256Hex(b), Origin: probeCoderOrigin(name), StoragePath: capturedPath(path)})
		if !d.writeEvidence(capturedPath(path), b) {
			return d.halt()
		}
	}
	path := probeCoderDir + "/expected/main.go"
	rec.Expected = append(rec.Expected, DevProbeSource{Path: path, Bytes: len(c.expected["main.go"]), SHA256: sha256Hex(c.expected["main.go"]),
		Origin: devProbeFixtureDir + "/main.go", StoragePath: capturedPath(path)})
	if !d.writeEvidence(capturedPath(path), c.expected["main.go"]) || !d.writeEvidenceJSON(probeCoderDir+"/fixture.json", envelope("probe-coder-fixture", d.s.runID, rec)) {
		return d.halt()
	}
	c.repoPath = filepath.Join(d.s.runtime, "probe-coder-input")
	repo, err := createSeed(c.repoPath, c.input)
	if err != nil {
		return failure("seed_failed", err)
	}
	c.inputCommit, c.inputTree = repo.Commit, repo.Tree
	if err := copyObjects(repo.repo.Storer, d.s.collect.Storer, []plumbing.Hash{repo.Commit}); err != nil {
		return failure("seed_failed", err)
	}
	return nil
}

// cursorCoderFixtureRecord is probe/coder/01/fixture.json's data.
type cursorCoderFixtureRecord struct {
	Line     string           `json:"line"`
	From     string           `json:"from"`
	To       string           `json:"to"`
	Input    []DevProbeSource `json:"input"`
	Expected []DevProbeSource `json:"expected"`
}

// probeCoderOrigin is an input file's source.
func probeCoderOrigin(name string) string {
	switch name {
	case "FEATURE.md", "go.mod":
		return "internal/reale2e/seed.go devSeedFiles"
	case "main.go":
		return devProbeFixtureDir + "/main.go with the greeting prefix replaced"
	}
	return devProbeFixtureDir + "/" + name
}

// probeCoderInputMain is the input main.go: the good one with exactly one
// greeting line's "Hello, " prefix replaced by "Pending, ".
func probeCoderInputMain(good []byte) ([]byte, error) {
	if n := bytes.Count(good, []byte(probeCoderGoodLine)); n != 1 {
		return nil, fmt.Errorf("the coder probe fixture's greeting line occurs %d times, not once", n)
	}
	bad := strings.Replace(probeCoderGoodLine, probeCoderGoodPrefix, probeCoderBadPrefix, 1)
	return bytes.Replace(good, []byte(probeCoderGoodLine), []byte(bad), 1), nil
}

// pushCoderInput creates the coder's own workspace and pushes the input
// commit with the supervisor's push, once: the response and the
// workspace's main branch must name exactly the input commit in the
// created instance. A lost response fails without a retry.
func (d *devSupervisor) pushCoderInput() error {
	c := d.probe.cursor
	suffix, err := d.s.randomHex(4)
	if err != nil {
		return failure("internal_error", err)
	}
	ctx, cancel := d.s.opCtx()
	ws, err := d.s.plane.CreateWorkspace(ctx, "e2e-"+probeCoderNamespace+"-"+suffix)
	cancel()
	if err != nil {
		return failure("workspace_create_failed", err)
	}
	c.ws = WorkspaceRecord{Name: ws.Name, Instance: ws.Instance, SeedCommit: c.inputCommit.String(), SeedTree: c.inputTree.String()}
	req := workspacetransfer.PushRequest{Name: ws.Name, Instance: ws.Instance, Branch: "main", Path: c.repoPath, PathSet: true}
	if !d.writeEvidenceJSON(probeCoderDir+"/push-request.json", cursorPushRecord{Name: req.Name, Instance: req.Instance, Branch: req.Branch,
		Path: d.s.tok(req.Path), Commit: c.inputCommit.String()}) {
		return d.halt()
	}
	if d.s.w.Push == nil {
		return failure(CodeProbeCoderPush, errors.New("the world has no supervisor push"))
	}
	pctx, pcancel := context.WithTimeout(d.s.ctx, max(0, minTime(d.s.w.Clock.Now().Add(probeCoderPushBound), d.attemptDeadline()).Sub(d.s.w.Clock.Now())))
	res, err := d.s.w.Push(pctx, d.s.plane, req)
	pcancel()
	resp := cursorPushResponse{Result: res}
	if err != nil {
		resp.Error = contract.SafeText(err.Error(), 400)
	}
	if !d.writeEvidenceJSON(probeCoderDir+"/push-response.json", resp) {
		return d.halt()
	}
	switch {
	case err != nil:
		return failure(CodeProbeCoderPush, fmt.Errorf("the coder input push failed (no retry): %w", err))
	case res.Name != ws.Name || res.Instance != ws.Instance || res.Branch != contract.DefaultBranchRef || res.Commit != c.inputCommit.String():
		return failure(CodeProbeCoderPush, fmt.Errorf("the coder input push answered %s %s %s, not the input commit", contract.SafeText(res.Name, 80),
			contract.SafeText(res.Branch, 80), contract.SafeText(res.Commit, 64)))
	}
	ok, err := d.verifyMain(c.ws, c.inputCommit.String(), "")
	if err != nil {
		return err
	}
	if !ok {
		return failure(CodeProbeCoderPush, errors.New("the coder workspace's main branch does not hold the input commit"))
	}
	return nil
}

// cursorPushRecord is probe/coder/01/push-request.json.
type cursorPushRecord struct {
	Name     string `json:"name"`
	Instance string `json:"instance"`
	Branch   string `json:"branch"`
	Path     string `json:"path"`
	Commit   string `json:"commit"`
}

// cursorPushResponse is probe/coder/01/push-response.json.
type cursorPushResponse struct {
	Result contract.WorkspacePushResult `json:"result"`
	Error  string                       `json:"error"`
}

// cursorProbeEnvelope is the coder fixture task's goal: the explicit
// owner-authorized probe authority, then the pinned mode, run, workspace,
// base, governing design and FP index, input digests, the exact repair,
// the allowed output, the required commands and no permission repair.
func cursorProbeEnvelope(runID string, ws WorkspaceRecord, base string, input map[string][]byte) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	line("Callsheet Cursor coder diagnostic probe (%s). Run %s.", probeCoderLabel, runID)
	line("%s", cursorProbeAuthority)
	line("mode: implement (probe fixture, namespace %s; not a workflow task)", probeCoderNamespace)
	line("run_id: %s", runID)
	line("workspace: %s instance %s", ws.Name, ws.Instance)
	line("input_commit: %s (your checkout's base)", base)
	line("governing_design: design.md sha256 %s; revision-history.md sha256 %s", sha256Hex(input["design.md"]), sha256Hex(input["revision-history.md"]))
	line("fp_index: FP-1 greeting (assigned; its function test is the existing main_test.go TestRun greeting subtests); FP-2 errors and help (not assigned; its behavior and tests stay unchanged)")
	line("input_artifacts (path sha256 at input_commit):")
	for _, name := range probeCoderFixtureFiles {
		line("- %s %s", name, sha256Hex(input[name]))
	}
	line("repair: in main.go the greeting line is %s; replace exactly its %s prefix with %s and change nothing else", strings.Replace(probeCoderGoodLine,
		probeCoderGoodPrefix, probeCoderBadPrefix, 1), probeCoderBadPrefix, probeCoderGoodPrefix)
	line("allowed_outputs: main.go only; every other path stays byte-identical with the same mode; add or delete no path")
	line("commands: go test ./... and go test -coverprofile=<task scratch path>/coverage.out ./...; run both and require 100%% pass and coverage above 80%%")
	line("permissions: change no permission, approval or configuration setting and repair nothing in the environment; a refused action is a blocker")
	line("time_policy: 10m from admission through publication; never raise a timeout, weaken an assertion or skip a test")
	return b.String()
}

// checkCursorProbeBudget is the coder task's pre-dispatch bound: the raw
// goal (contract.MaxGoalBytes) and acceptance, and the composed prompt the
// sidecar passes to Cursor (both manuals, goal and acceptance plus the
// shared envelope reserve), raw and JSON-encoded, within
// adapter.MaxCursorPromptBytes. Oversized text is refused, never
// truncated, and the allowance is not raised.
func checkCursorProbeBudget(instruction, runbook []byte, goal, acceptance string) error {
	if err := checkGoalBudget(goal); err != nil {
		return err
	}
	if !utf8.ValidString(acceptance) || len(acceptance) > contract.MaxAcceptanceBytes {
		return fmt.Errorf("%w: acceptance is %d bytes", errPromptBudget, len(acceptance))
	}
	parts := [][]byte{instruction, runbook, []byte(goal), []byte(acceptance)}
	raw, enc := devPromptReserve, devPromptReserve
	for _, p := range parts {
		n, err := jsonContentLen(p)
		if err != nil {
			return err
		}
		raw, enc = raw+len(p), enc+n
	}
	switch {
	case raw > adapter.MaxCursorPromptBytes:
		return fmt.Errorf("%w: raw Cursor prompt %d", errPromptBudget, raw)
	case enc > adapter.MaxCursorPromptBytes:
		return fmt.Errorf("%w: encoded Cursor prompt %d", errPromptBudget, enc)
	}
	return nil
}

// coderTaskVerdict judges the terminal coder task: the exact selected
// role, adapter and effective pair, the exact request and workspace
// binding, success with a complete final, a single role-valid STATUS and
// IMPL_COMPLETE (any other valid marker is unqualified, retained), and a
// publication.
func (d *devSupervisor) coderTaskVerdict(v contract.TaskView, req contract.DispatchRequest) error {
	c := d.probe.cursor
	role, _ := d.flow.Role(HopCoder)
	b, r := v.WorkspaceBinding, v.Result
	switch {
	case v.Role.ID != HopCoder || v.Role.Adapter != role.Adapter || v.Request.Override != nil || v.Effective.Model != role.Model ||
		v.Effective.Effort != role.Effort:
		return failure("binding_rejected", fmt.Errorf("coder probe task %s ran %s %s/%s", v.TaskID, v.Role.Adapter, v.Effective.Model, v.Effective.Effort))
	case v.Request.Goal != req.Goal || v.Request.Acceptance != req.Acceptance || len(v.Request.Payload) != 0:
		return failure("binding_rejected", fmt.Errorf("coder probe task %s's request is not the supervisor's", v.TaskID))
	case b == nil || b.Name != c.ws.Name || b.Instance != c.ws.Instance || b.BaseCommit == nil || *b.BaseCommit != c.inputCommit.String():
		return failure("binding_rejected", fmt.Errorf("coder probe task %s's workspace binding is not the coder workspace and input", v.TaskID))
	case v.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0:
		return failure("task_failed", fmt.Errorf("coder probe task %s ended %s", v.TaskID, v.State))
	case r.FinalMessage == nil || r.FinalMessageTruncated:
		return failure("final_message_missing", fmt.Errorf("coder probe task %s has no complete final answer (%s)", v.TaskID, probeCoderDir+"/task.json"))
	}
	marker, err := parseTerminalMarker(*r.FinalMessage)
	if err != nil || !markerAllowed(HopCoder, modeImplement, marker) {
		return failure("invalid_marker", fmt.Errorf("coder probe task %s's final answer has no single role-valid STATUS line (%s)", v.TaskID, probeCoderDir+"/final.txt"))
	}
	c.marker = marker
	if marker != markerImplComplete {
		return failure(CodeProbeCoderUnqualified, fmt.Errorf("coder probe task %s ended STATUS: %s (retained; the probe never enters a design or fix loop)", v.TaskID, marker))
	}
	if w := r.Workspace; w == nil || w.Publication != contract.PublicationPublished || w.Commit == nil {
		return failure(CodeProbeCoderPublication, fmt.Errorf("coder probe task %s published no result", v.TaskID))
	}
	return nil
}

// admitCoderPublication pulls the published result into a fresh runtime
// repository and admits it strictly: one parent equal to the input, the
// existing tree, budget and coder admission checks, then exactly main.go
// changed, byte-identical to the known-good main.go, within the task's
// 10m admission-through-publication bound.
func (d *devSupervisor) admitCoderPublication(v contract.TaskView, admittedAt time.Time) error {
	c := d.probe.cursor
	commit := *v.Result.Workspace.Commit
	c.resultCommit = commit
	dir := filepath.Join(d.s.runtime, "probe-coder-result")
	if _, err := git.PlainInit(dir, false); err != nil {
		return failure("runtime_unavailable", err)
	}
	deadline := minTime(admittedAt.Add(FeatureTaskBound), d.attemptDeadline())
	ctx, cancel := context.WithTimeout(d.s.ctx, max(0, deadline.Sub(d.s.w.Clock.Now())))
	err := d.s.puller.PullResult(ctx, v.TaskID, dir)
	cancel()
	if err != nil {
		return failure(CodeProbeCoderPublication, fmt.Errorf("publication: the result pull failed: %w", err))
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return failure(CodeProbeCoderPublication, err)
	}
	result, parent, tree, err := readCommitTree(repo, commit)
	if err != nil {
		return failure(CodeProbeCoderPublication, err)
	}
	c.resultTree = tree
	// The result's object closure is required evidence: a failed copy is
	// the existing evidence failure, stopping the probe with the original
	// error retained (never an observation without its Git evidence).
	if err := copyObjects(repo.Storer, d.s.collect.Storer, []plumbing.Hash{plumbing.NewHash(commit)}); err != nil {
		return failure("evidence_write_failed", fmt.Errorf("the coder result %s could not be retained as evidence: %w", commit, err))
	}
	c.resultPulled = true
	var base []TreeFile
	for _, name := range probeCoderFixtureFiles {
		base = append(base, TreeFile{Path: name, Mode: filemode.Regular, Content: c.input[name]})
	}
	rec := cursorPublicationRecord{Commit: commit, Parent: parent, Tree: tree, Changed: []string{}, Files: []DevProbeSource{}}
	bmap, _ := indexTree(base)
	rmap, ierr := indexTree(result)
	if ierr == nil {
		rec.Changed = append(rec.Changed, changedPaths2(bmap, rmap)...)
		for _, name := range sortedTree(rmap) {
			rec.Files = append(rec.Files, DevProbeSource{Path: name, Bytes: len(rmap[name].Content), SHA256: sha256Hex(rmap[name].Content), Origin: commit})
		}
	}
	verdict := func() error {
		switch {
		case parent != c.inputCommit.String():
			return fmt.Errorf("publication: parent %s is not the input commit %s", parent, c.inputCommit)
		case ierr != nil:
			return ierr
		}
		if err := AdmitTree(HopCoder, modeImplement, markerImplComplete, base, result); err != nil {
			return err
		}
		switch {
		case !slices.Equal(rec.Changed, []string{"main.go"}):
			return fmt.Errorf("publication: the changed paths are %v, not exactly main.go", rec.Changed)
		case !bytes.Equal(rmap["main.go"].Content, c.expected["main.go"]):
			return errors.New("publication: main.go is not byte-identical to the known-good repair")
		case d.expired(admittedAt.Add(FeatureTaskBound)):
			return fmt.Errorf("publication: admitted after the task's %s bound", FeatureTaskBound)
		}
		return nil
	}()
	if verdict != nil {
		rec.Rejection = contract.SafeText(verdict.Error(), 400)
	}
	rec.Admitted = verdict == nil
	if !d.writeEvidenceJSON(probeCoderDir+"/publication.json", envelope("probe-coder-publication", d.s.runID, rec)) {
		return d.halt()
	}
	if verdict != nil {
		return failure(CodeProbeCoderPublication, verdict)
	}
	c.admitted, c.resultFiles = true, map[string][]byte{}
	for name, f := range rmap {
		c.resultFiles[name] = f.Content
	}
	return nil
}

// cursorPublicationRecord is probe/coder/01/publication.json's data.
type cursorPublicationRecord struct {
	Commit    string           `json:"commit"`
	Parent    string           `json:"parent"`
	Tree      string           `json:"tree"`
	Changed   []string         `json:"changed"`
	Files     []DevProbeSource `json:"files"`
	Admitted  bool             `json:"admitted"`
	Rejection string           `json:"rejection"`
}

// probeCoderValidationFiles are the coder validation records, relative to
// probeCoderDir/validation.
var probeCoderValidationFiles = []string{"final-test.json", "final-stdout.txt", "final-stderr.txt", "cases.json", "build-stdout.txt",
	"build-stderr.txt", "tests.jsonl", "test-json-stderr.txt", "requirements.json", "coverage.out", "coverage-stdout.txt", "coverage-stderr.txt",
	"coverage.json"}

// validateCoderResult exports the admitted result to a fresh runtime
// directory and runs the full fixture oracle (tests, build, the ten CLI
// cases, JSON case accounting and changed-code coverage above 80%) within
// one shared 60s budget clipped by the probe deadline, retaining every
// record. This proves the published edit and independent validation, not
// that Cursor executed each command it reported.
func (d *devSupervisor) validateCoderResult() error {
	c := d.probe.cursor
	// The admitted result's own bytes (never the expected copy).
	files := c.resultFiles
	start := d.s.w.Clock.Now()
	mark := func(typ, name string) int {
		return d.s.ev.event(Event{Type: typ, Hop: probeCoderLabel, Code: name}).Seq
	}
	v := runFinalValidationWith(validationOptions{run: d.s.command, clock: d.s.w.Clock, goBin: d.s.goBin, pathEnv: d.s.pathEnv,
		dir: filepath.Join(d.s.runtime, "probe-coder-validation"), files: files, commit: c.resultCommit, tree: c.resultTree, mark: mark,
		deadline: finalDeadline(start, d.s.attempt), allowed: devTreePolicy(), cover: true})
	cov := DevCoverageRecord{Schema: devKind("coverage"), RunID: d.s.runID, Commit: c.resultCommit, Tree: c.resultTree, Profile: sha256Hex(v.Profile),
		FinalStdout: sha256Hex([]byte(toValidUTF8(v.TestOut))), FinalStderr: sha256Hex([]byte(toValidUTF8(v.TestErr)))}
	if v.Cover != nil {
		cov.Exec = *v.Cover
	} else {
		cov.Exec = ExecRecord{Schema: ExecSchema, Name: "go_test_cover", Argv: validationArgvCover, Commit: c.resultCommit, Tree: c.resultTree}
	}
	if cv, err := deriveChangedCoverage(c.input["main.go"], files["main.go"], v.Profile, modulePathOf(files["go.mod"])); err == nil {
		cov.Coverage = &cv
		c.coverage = &cv
	} else {
		cov.Error = contract.SafeText(err.Error(), 200)
	}
	dir := probeCoderDir + "/validation/"
	data := map[string][]byte{"final-stdout.txt": []byte(toValidUTF8(v.TestOut)), "final-stderr.txt": []byte(toValidUTF8(v.TestErr)),
		"build-stdout.txt": []byte(toValidUTF8(v.BuildOut)), "build-stderr.txt": []byte(toValidUTF8(v.BuildErr)), "tests.jsonl": []byte(toValidUTF8(v.TestJSON)),
		"test-json-stderr.txt": []byte(toValidUTF8(v.TestJSONEr)), "coverage-stdout.txt": []byte(toValidUTF8(v.CoverOut)),
		"coverage-stderr.txt": []byte(toValidUTF8(v.CoverErr))}
	if v.Profile != nil {
		data["coverage.out"] = []byte(toValidUTF8(v.Profile))
	}
	docs := map[string]any{"final-test.json": v.Test, "cases.json": v.Cases, "requirements.json": v.Reqs, "coverage.json": cov}
	for _, name := range probeCoderValidationFiles {
		ok := true
		if b, has := data[name]; has {
			ok = d.writeEvidence(dir+name, b)
		} else if x, has := docs[name]; has {
			ok = d.writeEvidenceJSON(dir+name, x)
		}
		if !ok {
			return d.halt()
		}
	}
	if err := coderValidationVerdict(v, cov); err != nil {
		return failure(CodeProbeCoderValidation, err)
	}
	c.validated = true
	return nil
}

// coderValidationVerdict is the full fixture oracle's verdict on one
// validation (nil: passed), naming the existing validation code.
func coderValidationVerdict(v finalValidation, cov DevCoverageRecord) error {
	casesOK := v.Cases.Build.Started && v.Cases.Build.ExitCode == 0 && len(v.Cases.Cases) == len(FeatureCases)
	for _, c := range v.Cases.Cases {
		casesOK = casesOK && c.Pass
	}
	switch {
	case v.Err != nil || v.Test.ExitCode != 0 || v.Test.TimedOut || v.Test.Toolchain == "":
		return fmt.Errorf("final_tests_failed: go test exited %d: %v", v.Test.ExitCode, v.Err)
	case !casesOK:
		return errors.New(CodeCaseFailed + ": a CLI feature case did not produce its required result")
	case len(v.Reqs.MissingTests) > 0 || len(v.Reqs.FailedTests) > 0 || !v.Reqs.RunSignature || !v.Reqs.TableTests || !v.Reqs.TestCallsRun:
		return errors.New(CodeTestMapping + ": a required TestRun/<case-id> subtest or the run/table structure is missing or failing")
	case !v.Reqs.TestRunPassed:
		return fmt.Errorf("%s: go test -json did not pass (failures %v)", CodeValidationFail, v.Reqs.TestFailures)
	case v.Cover == nil || !v.Cover.Started || v.Cover.ExitCode != 0 || v.Cover.TimedOut || cov.Coverage == nil:
		return errors.New(CodeCoverageUnobserved + ": the coverage run gave no usable profile")
	case !cov.Coverage.Pass:
		return fmt.Errorf("%s: changed-code coverage %d/%d is not above 80%%", CodeCoverageBelow, cov.Coverage.Covered, cov.Coverage.Total)
	}
	return nil
}

// writeCursorCoderRepository retains the coder input and, when it was
// pulled, the published result with their object closure in their own
// evidence repository (HEAD at the input). Source provenance and object
// closure are retained for diagnosis, not independently rechecked here.
func (d *devSupervisor) writeCursorCoderRepository() {
	if d.probe == nil || d.probe.cursor == nil || d.probe.cursor.inputCommit.IsZero() {
		return
	}
	c := d.probe.cursor
	refs := map[string]plumbing.Hash{"refs/heads/input": c.inputCommit}
	if c.resultPulled {
		refs["refs/heads/result"] = plumbing.NewHash(c.resultCommit)
	}
	d.writeRefRepoHead(probeCoderRepoDir, refs, "refs/heads/input")
}

// ---- Layout ----

var (
	probeCoderFileRE = regexp.MustCompile(`^probe/coder/01/(request\.json|fixture\.json|push-request\.json|push-response\.json|task\.json|final\.txt|logs\.txt|logs\.json|publication\.json|verdict\.json)$`)
	probeCoderRefRE  = regexp.MustCompile(`^refs/heads/(input|result)$`)
)

// cursorActivityFiles are the two shallow sample records.
var cursorActivityFiles = []string{"environment/cursor-activity-before.json", "environment/cursor-activity-after.json"}

// cursorProbeCaptures are the coder fixture's physical capture paths.
func cursorProbeCaptures() map[string]bool {
	m := map[string]bool{capturedPath(probeCoderDir + "/expected/main.go"): true}
	for _, name := range probeCoderFixtureFiles {
		m[capturedPath(probeCoderDir+"/input/"+name)] = true
	}
	return m
}

// cursorProbeLayout is the Cursor probe's closed file set: the current
// qualification layout of its scope plus the coder stage's records,
// captures and evidence repository and the two shallow samples. Its
// document dispatch is the base layout's permissive one; only
// cursorEvidenceLayout (devcursor_check.go), which types every added JSON
// record, admits Cursor qualification evidence.
func cursorProbeLayout(scope string) devLayout {
	l := probeLayoutFor(devBundleV2, scope)
	file, dir := l.file, l.dir
	caps := cursorProbeCaptures()
	capDirs := captureDirs(caps)
	validation := map[string]bool{}
	for _, n := range probeCoderValidationFiles {
		validation[probeCoderDir+"/validation/"+n] = true
	}
	l.file = func(rel string) bool {
		switch {
		case caps[rel], validation[rel], probeCoderFileRE.MatchString(rel), slices.Contains(cursorActivityFiles, rel):
			return true
		}
		return file(rel)
	}
	l.dir = func(rel string) bool {
		switch rel {
		case "probe", "probe/coder", probeCoderDir, probeCoderDir + "/validation", "workspace/coder", probeCoderRepoDir:
			return true
		}
		return capDirs[rel] || dir(rel)
	}
	repos := l.repoSpecs()
	l.repos = append(slices.Clone(repos), devRepoSpec{dir: probeCoderRepoDir, ref: func(n string) bool { return probeCoderRefRE.MatchString(n) },
		head: "refs/heads/input"})
	return l
}
