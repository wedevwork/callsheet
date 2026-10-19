package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The owner-only qualification probe (design 12a-dev-flow-verify,
// Qualification probe): reale2e probe-dev-full, under the same gate,
// session-only launch recipe, bounds, fingerprinting and cleanup as run,
// in three sequential stages of one bounded invocation that stops at the
// first failure: the five frozen selections registered, readiness-checked
// and authenticated; three coordinator phases resuming the same session,
// each with one harmless no-workspace designer wake task and two owner
// continuation windows between them; and the print-stream notification
// evidence plus one code-reviewer task on a harness-authored known-good
// fixture that must run the verification helper and publish only
// review.md. Its record is qualification.json (never a full-flow
// manifest); it prints QUALIFIED or BLOCKED, never a workflow PASS: exit
// 0 qualified, 1 blocked, 2 usage or gate error. Probe tasks are their own
// namespaces (probe-wake, probe-review) and can never satisfy a full-flow
// slot; there is no feature work, no Next transition and no design
// receipt.

// Qualification record and layout names.
const (
	devQualificationFile  = "qualification.json"
	DevQualificationKind  = "qualification"
	devProbeFixtureDir    = "internal/reale2e/testdata/devfull-review-probe"
	maxProbePhases        = 3
	pauseProbe            = "probe"
	probeWakeNamespace    = "probe-wake"
	probeReviewNamespace  = "probe-review"
	probeReviewerOrdinal  = 4
	probeSystemSourceFile = "setup/probe-system-source.md"
	probePhaseSourceFile  = "setup/probe-phase-source.md"
	probeSystemFile       = "setup/probe-system.md"
)

// probeFixtureFiles are the known-good fixture's files.
var probeFixtureFiles = []string{"main.go", "main_test.go", "design.md", "revision-history.md"}

// ProbeBounds is the probe's cost and time disclosure.
const ProbeBounds = `Cost and time bounds of the qualification probe (policy, not measurements):
  - five paid authentication preflight tasks, each at most 2m; three harmless designer wake
    tasks and one code-reviewer task on a harness-owned fixture, each at most 10m: at most
    nine paid worker calls, no retries;
  - the headless Claude Code coordinator adds vendor-dependent turns in up to three phases;
  - each owner continuation window at most 20m; the probe at most 90m from preflight start
    (the first owner-environment fingerprint included), then at most 2m of cleanup;
  - a failed stage stops the probe; no token-price ceiling is claimed.
`

// devProbeSystemPrompt is the probe's qualification-only coordinator
// instructions, appended to the captured shared playbook.
const devProbeSystemPrompt = `Qualification probe instructions (a harness-owned constant). This session qualifies the installation for run {{RUN_ID}}; it is not feature work and it is not the development workflow. Do not design, implement or review any feature, do not answer or synthesize an owner decision or a design receipt, and do not follow any workflow transition of the playbook: only {{REALE2E}} dev-next names a task you may dispatch, and it names only probe tasks.

The commands are {{REALE2E}} dev-next --run {{RUNTIME}}, {{REALE2E}} dev-observe --run {{RUNTIME}} --ordinal N --task ID and exactly one harness-managed background command {{REALE2E}} dev-wait-task --run {{RUNTIME}} --task ID --ordinal N per dispatched task, with no &, setsid, redirect, pipe or detached shell. For prepared PATH SHA256, read the document, check its SHA-256 and dispatch through MCP with exactly its target, goal and acceptance, and its workspace, workspace_instance and base only when it names them, with no payload. Report the admitted task with dev-observe, start its wait, and when its completion notification arrives, consume it with one task_show of that task before anything else; then ask dev-next again. When dev-next answers pending, ask again; never dispatch a task twice.

If dev-next answers seed_unpublished, first call ws_push {"name":"{{WORKSPACE}}","instance":"{{INSTANCE}}","branch":"main","path":"{{SEED_PATH}}"}; its commit must be {{SEED_COMMIT}}. A tool denial stops the probe; never widen permissions or edit any configuration.

Callsheet tools: this probe permits exactly these nine Callsheet MCP tools: dispatch, task_ls, task_show, task_logs, task_cancel, ws_status, ws_diff, ws_push and ws_pull. Every other Callsheet tool is forbidden. task_wait is forbidden in this exercise: the shared playbook's task_wait fallback for an unqualified wake does not apply, and an unqualified wake ends the phase. dispatch never takes a wait. ws_push is only the seed push above, before the first dispatch. task_cancel is only for this probe's own dispatched tasks when the probe stops.
`

// devProbePhasePrompt is each probe phase's prompt.
const devProbePhasePrompt = `Qualification probe {{RUN_ID}}, phase {{PHASE}} of 3. Runtime {{RUNTIME}}. Use {{REALE2E}} dev-next, dev-observe and dev-wait-task with that runtime, exactly as your instructions say. This phase's probe tasks: {{PROBE_TASKS}}. When dev-next answers paused or complete, end the phase: your final result is exactly the one line {{PHASE_END}} and nothing else.
`

// probePromptKeys are the probe prompts' only placeholders.
var probePromptKeys = append(slices.Clone(DevPromptKeys), "PHASE", "PROBE_TASKS", "PHASE_END")

// devProbe is one qualification probe's state.
type devProbe struct {
	d        *devSupervisor
	fixture  map[string][]byte
	seed     plumbing.Hash
	cand     plumbing.Hash
	candTree plumbing.Hash
	repoPath string
	prompts  map[int][]byte
	system   []byte
	decided  int
	wakes    int
	reviewed bool
	review   *DevReviewEvidence
	reviewC  string
	stages   []DevProbeStage
	result   string
}

// DevProbeStage is one qualification stage's result.
type DevProbeStage struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Code   string `json:"code"`
}

// DevProbeSource is one probe prompt or fixture file's provenance.
type DevProbeSource struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
	Origin string `json:"origin"`
}

// DevQualification is qualification.json.
type DevQualification struct {
	Schema           string              `json:"schema"`
	RunID            string              `json:"run_id"`
	Result           string              `json:"result"`
	Revision         string              `json:"revision"`
	CallsheetVersion string              `json:"callsheet_version"`
	GOOS             string              `json:"goos"`
	GOARCH           string              `json:"goarch"`
	Gate             GateRecord          `json:"gate"`
	Versions         []VersionRecord     `json:"versions"`
	FlowLabel        string              `json:"flow_label"`
	SelectionSHA256  string              `json:"selection_sha256"`
	Roles            []FlowRole          `json:"roles"`
	Executable       DevExecutableRecord `json:"executable"`
	Helper           reviewHelperSpec    `json:"helper"`
	SessionID        string              `json:"session_id"`
	Workspace        WorkspaceRecord     `json:"workspace"`
	ProtectedRoots   []string            `json:"protected_roots"`
	ProtectDepth     int                 `json:"protect_depth"`
	Candidate        string              `json:"candidate"`
	ReviewCommit     string              `json:"review_commit"`
	Stages           []DevProbeStage     `json:"stages"`
	Prompts          []DevProbeSource    `json:"prompts"`
	Fixture          []DevProbeSource    `json:"fixture"`
	WorkerCalls      int                 `json:"worker_calls"`
	Phases           int                 `json:"phases"`
	StopCode         *string             `json:"stop_code"`
	StopSeq          *int                `json:"stop_seq"`
	AttemptStart     string              `json:"attempt_start"`
	AttemptDeadline  string              `json:"attempt_deadline"`
	Render           map[string]string   `json:"render"`
	Inventory        []InventoryEntry    `json:"inventory"`
}

// Probe results.
const (
	ResultQualified = "QUALIFIED"
	ResultBlocked   = "BLOCKED"
	stagePass       = "pass"
	stageFail       = "fail"
	stageUnreached  = "not_reached"
)

// Qualification stages in order.
var probeStages = []string{"models", "resume_wake", "print_notification", "reviewer_evidence"}

// runProbeLive builds the world and runs one qualification probe.
func runProbeLive(h Host, a runArgs) int {
	w, release, err := h.NewWorld(h)
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e probe-dev-full: %v\n", err)
		return 1
	}
	defer release()
	s := &supervisor{stdout: h.Stdout, stderr: h.Stderr, goos: h.GOOS, goarch: h.GOARCH, w: w, args: a, ctx: w.Context,
		known: map[string]string{}, stoppedSet: map[*child]bool{}, reqs: make(chan ctrlMsg), loopDone: make(chan struct{})}
	d := &devSupervisor{s: s, manuals: map[string][2]string{}, receiptBytes: map[int][]byte{}, trees: map[string]map[string]TreeFile{},
		finals: map[string]string{}, sweeps: sweepRecorder{clock: w.Clock}}
	d.probe = &devProbe{d: d, prompts: map[int][]byte{}}
	if h.observeProbe != nil {
		h.observeProbe(d.probe)
	}
	if h.observeDev != nil {
		h.observeDev(d)
	}
	return d.run()
}

// probeStart is the probe's setup through its first phase: the models
// stage, then the fixture, prompts and control.
func (d *devSupervisor) probeStart() error {
	steps := []func() error{d.s.printPermissionTemplate, d.hashExecutable, d.s.protectedRoots, d.s.baselineSnapshot, d.loadSelection,
		d.versionProbes, d.s.createRuntime, d.s.renderPermissions, d.extraDirs, d.deployDev, d.preflightDev, d.probe.models, d.probe.fixtureDev,
		d.probe.openControl, d.activateTerminal, d.probe.firstPlan, d.launchPhase}
	for _, f := range steps {
		switch {
		case d.s.ctx.Err() != nil:
			return failure("interrupted", d.s.ctx.Err())
		case d.expired(d.attemptDeadline()):
			return failure("attempt_timeout", errors.New("the probe reached its 90m bound"))
		}
		if err := f(); err != nil {
			return err
		}
		if d.stopCode != "" {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	return nil
}

// models records the models stage (its five preflights passed).
func (p *devProbe) models() error {
	p.setStage("models", stagePass, "")
	return nil
}

// setStage records stage name's result once.
func (p *devProbe) setStage(name, result, code string) {
	for _, s := range p.stages {
		if s.Name == name {
			return
		}
	}
	p.stages = append(p.stages, DevProbeStage{Name: name, Result: result, Code: code})
}

// fixtureDev reads the bounded known-good fixture from the checkout,
// captures it, creates the seed (generated FEATURE.md and go.mod) and one
// candidate child commit replacing main/test and adding the fixture's
// design and history, and creates the separate probe workspace the
// coordinator pushes the pinned candidate to.
func (p *devProbe) fixtureDev() error {
	d := p.d
	p.fixture = map[string][]byte{}
	for _, name := range probeFixtureFiles {
		b, err := readLiteral(filepath.Join(d.s.checkout, filepath.FromSlash(devProbeFixtureDir), name), maxArtifactBytes)
		if err != nil {
			return failure("fixture_missing", fmt.Errorf("probe fixture %s: %w", name, err))
		}
		p.fixture[name] = b
		if !d.writeEvidence("setup/probe-fixture/"+name, b) {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	gomod, err := readBounded(filepath.Join(d.s.checkout, "go.mod"), 64<<10)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	ver, err := goDirective(gomod)
	if err != nil {
		return failure("checkout_unreadable", err)
	}
	files, err := devSeedFiles(ver)
	if err != nil {
		return failure("seed_failed", err)
	}
	p.repoPath = filepath.Join(d.s.runtime, "probe-fixture")
	seed, err := createSeed(p.repoPath, files)
	if err != nil {
		return failure("seed_failed", err)
	}
	d.s.seed = seed
	p.seed = seed.Commit
	cand := map[string][]byte{}
	for k, v := range files {
		cand[k] = v
	}
	for k, v := range p.fixture {
		cand[k] = v
	}
	if p.cand, p.candTree, err = commitFiles2(seed.repo, p.repoPath, cand, seed.Commit); err != nil {
		return failure("seed_failed", err)
	}
	if err := copyObjects(seed.repo.Storer, d.s.collect.Storer, []plumbing.Hash{p.cand}); err != nil {
		return failure("seed_failed", err)
	}
	for commit, fs := range map[string]map[string][]byte{seed.Commit.String(): files, p.cand.String(): cand} {
		t := map[string]TreeFile{}
		for name, b := range fs {
			t[name] = TreeFile{Path: name, Mode: filemode.Regular, Content: b}
		}
		d.trees[commit] = t
	}
	d.commits = append(d.commits, devCommit{Ordinal: 0, Task: "", Commit: p.cand.String(), Parent: seed.Commit.String(), Tree: p.candTree.String()})
	d.s.ev.event(Event{Type: EvSeedCreated})
	suffix, err := d.s.randomHex(4)
	if err != nil {
		return failure("internal_error", err)
	}
	ctx, cancel := d.s.opCtx()
	defer cancel()
	ws, err := d.s.plane.CreateWorkspace(ctx, "e2e-probe-"+suffix)
	if err != nil {
		return failure("workspace_create_failed", err)
	}
	// The coordinator pushes the pinned candidate as the workspace's main
	// branch; dev-next exposes nothing until it is there.
	d.s.workspace = WorkspaceRecord{Name: ws.Name, Instance: ws.Instance, SeedCommit: p.cand.String(), SeedTree: p.candTree.String()}
	d.state = DevFullState{RunID: d.s.runID, Scenario: "probe", InitialCommit: seed.Commit.String(), ChainCommit: p.cand.String(),
		CandidateCommit: p.cand.String()}
	return nil
}

// commitFiles2 commits files (replacing the worktree's) as a child of
// parent in the repository at dir, returning the commit and its tree.
func commitFiles2(r *git.Repository, dir string, files map[string][]byte, parent plumbing.Hash) (plumbing.Hash, plumbing.Hash, error) {
	wt, err := r.Worktree()
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, err
	}
	for _, name := range sortedKeys(files) {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		if _, err := wt.Add(name); err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
	}
	sig := seedAuthor
	h, err := wt.Commit("Probe fixture: a harness-authored known-good candidate (not an owner-approved feature)\n",
		&git.CommitOptions{Author: &sig, Committer: &sig, Parents: []plumbing.Hash{parent}})
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, err
	}
	c, err := r.CommitObject(h)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, err
	}
	return h, c.TreeHash, nil
}

// renderProbePrompt replaces placeholders using only probePromptKeys.
func renderProbePrompt(name, tpl string, vals map[string]string) ([]byte, error) {
	allowed := map[string]bool{}
	for _, k := range probePromptKeys {
		allowed[k] = true
	}
	var bad []string
	out := placeholderRE.ReplaceAllFunc([]byte(tpl), func(m []byte) []byte {
		k := string(placeholderRE.FindSubmatch(m)[1])
		v, ok := vals[k]
		if !allowed[k] || !ok || v == "" {
			bad = append(bad, k)
			return m
		}
		return []byte(v)
	})
	if len(bad) > 0 {
		return nil, fmt.Errorf("probe prompt %s: unknown or unset placeholder(s) %v", name, bad)
	}
	return out, nil
}

// probePhaseValues are phase n's prompt values.
func probePhaseValues(vals map[string]string, n int) map[string]string {
	out := map[string]string{}
	for k, v := range vals {
		out[k] = v
	}
	out["PHASE"] = itoa(n)
	out["PHASE_END"] = probePhaseEnd(n)
	out["PROBE_TASKS"] = fmt.Sprintf("one harmless designer wake task (namespace %s, ordinal %d)", probeWakeNamespace, n)
	if n == maxProbePhases {
		out["PROBE_TASKS"] += fmt.Sprintf(", then one code-reviewer probe task on the known-good fixture (namespace %s, ordinal %d)", probeReviewNamespace, probeReviewerOrdinal)
	}
	return out
}

// probePhaseEnd is phase n's exact final result.
func probePhaseEnd(n int) string {
	if n == maxProbePhases {
		return "probe=complete"
	}
	return fmt.Sprintf("gate=probe generation=%d", n)
}

// openControl writes the run file and MCP configuration, renders the
// probe's system input and its three phase inputs, records their
// provenance and opens the control socket.
func (p *devProbe) openControl() error {
	d := p.d
	b, err := encodeJSON(DevRunFile{Schema: DevRunSchema, RunID: d.s.runID, Callsheet: d.s.args.callsheet, Plane: d.s.dep.url, CA: d.s.dep.caPath})
	if err != nil {
		return failure("runtime_unavailable", err)
	}
	if err := os.WriteFile(filepath.Join(d.s.runtime, RunFileName), b, 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	cfg, err := mcpConfig(d.s.args.callsheet, d.s.dep.url, d.s.dep.caPath)
	if err != nil {
		return failure("runtime_unavailable", err)
	}
	d.s.mcpPath = filepath.Join(d.s.runtime, "mcp.json")
	if err := os.WriteFile(d.s.mcpPath, cfg, 0o600); err != nil {
		return failure("runtime_unavailable", err)
	}
	vals := map[string]string{"RUN_ID": d.s.runID, "RUNTIME": d.s.runtime, "PLANE_URL": d.s.dep.url, "CA_PATH": d.s.dep.caPath,
		"REALE2E": d.s.w.Executable, "WORKSPACE": d.s.workspace.Name, "INSTANCE": d.s.workspace.Instance, "SEED_PATH": p.repoPath,
		"SEED_COMMIT": p.cand.String(), "COORD_DIR": filepath.Join(d.s.runtime, "coord")}
	d.manifest.Render = vals
	d.helper = reviewHelperSpec{Reale2e: d.s.w.Executable, Go: d.s.goBin, Deadline: formatHelperDeadline(d.attemptDeadline())}
	d.manifest.Helper = d.helper
	sys, err := renderProbePrompt("devProbeSystemPrompt", devProbeSystemPrompt, vals)
	if err != nil {
		return failure("prompt_render_failed", err)
	}
	p.system = append(append(append([]byte(nil), d.loaded.Files[DevPlaybook]...), '\n'), sys...)
	for n := 1; n <= maxProbePhases; n++ {
		if p.prompts[n], err = renderProbePrompt("devProbePhasePrompt", devProbePhasePrompt, probePhaseValues(vals, n)); err != nil {
			return failure("prompt_render_failed", err)
		}
	}
	d.s.rendered = map[string][]byte{CoordinatorPrompt: p.system}
	sanitized, err := mcpConfig(d.s.tok(d.s.args.callsheet), d.s.dep.url, d.s.tok(d.s.dep.caPath))
	if err != nil {
		return failure("internal_error", err)
	}
	writes := map[string][]byte{probeSystemSourceFile: []byte(devProbeSystemPrompt), probePhaseSourceFile: []byte(devProbePhasePrompt),
		probeSystemFile: p.system, "setup/mcp-config.json": sanitized}
	for n := 1; n <= maxProbePhases; n++ {
		writes[fmt.Sprintf("setup/probe-phase-%d.md", n)] = p.prompts[n]
	}
	for _, rel := range sortedKeys(writes) {
		if !d.writeEvidence(rel, writes[rel]) {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	ctl, err := listenDevControl(d.s.runtime, d.s.runID, d.s.handleControl)
	if err != nil {
		return failure("control_socket_failed", err)
	}
	d.s.ctl = ctl
	d.s.ev.event(Event{Type: EvCoordinatorReady})
	return nil
}

// probePrompts is the qualification record's prompt provenance.
func (p *devProbe) probePrompts() []DevProbeSource {
	origin := "internal/reale2e/devfull_probe.go:"
	out := []DevProbeSource{
		{Path: probeSystemSourceFile, Bytes: len(devProbeSystemPrompt), SHA256: sha256Hex([]byte(devProbeSystemPrompt)), Origin: origin + "devProbeSystemPrompt"},
		{Path: probePhaseSourceFile, Bytes: len(devProbePhasePrompt), SHA256: sha256Hex([]byte(devProbePhasePrompt)), Origin: origin + "devProbePhasePrompt"},
	}
	if p.system != nil {
		out = append(out, DevProbeSource{Path: probeSystemFile, Bytes: len(p.system), SHA256: sha256Hex(p.system),
			Origin: DevFlowDir + "/" + DevPlaybook + " + " + origin + "devProbeSystemPrompt"})
	}
	for n := 1; n <= maxProbePhases; n++ {
		if b := p.prompts[n]; b != nil {
			out = append(out, DevProbeSource{Path: fmt.Sprintf("setup/probe-phase-%d.md", n), Bytes: len(b), SHA256: sha256Hex(b), Origin: origin + "devProbePhasePrompt"})
		}
	}
	return out
}

// probeFixtureRecords is the fixture's provenance.
func (p *devProbe) probeFixtureRecords() []DevProbeSource {
	out := []DevProbeSource{}
	for _, name := range probeFixtureFiles {
		if b, ok := p.fixture[name]; ok {
			out = append(out, DevProbeSource{Path: "setup/probe-fixture/" + name, Bytes: len(b), SHA256: sha256Hex(b), Origin: devProbeFixtureDir + "/" + name})
		}
	}
	return out
}

// Probe wake task text.
const (
	probeWakeGoal       = "Callsheet qualification wake probe. Run RUN_ID, phase P. Do not use tools, read or write files, design a feature, or produce a design artifact. Reply with CALLSHEET_PROBE_OK."
	probeWakeAcceptance = "A complete nonempty answer from the configured designer selection; no workspace or publication. Token echo is information only."
)

// wakeGoal is phase n's wake goal.
func wakeGoal(run string, n int) string {
	return strings.NewReplacer("RUN_ID", run, "phase P.", "phase "+itoa(n)+".").Replace(probeWakeGoal)
}

// firstPlan schedules phase 1's wake task.
func (p *devProbe) firstPlan() error {
	st, step := p.wakeStep(p.d.state, 1)
	p.d.apply(st, step)
	return nil
}

// wakeStep schedules phase n's wake task.
func (p *devProbe) wakeStep(st DevFullState, n int) (DevFullState, StepResult) {
	plan := TaskPlan{Ordinal: n, Role: HopDesigner, Mode: probeWakeNamespace, OriginalMode: probeWakeNamespace, Reason: "probe", Generation: n,
		Inputs: []PlanInput{}, Allowed: []string{}, Goal: wakeGoal(p.d.s.runID, n), Acceptance: probeWakeAcceptance}
	st.Pending = &plan
	return st, StepResult{Action: actionDispatch, Plan: &plan}
}

// reviewStep schedules the reviewer probe.
func (p *devProbe) reviewStep(st DevFullState) (DevFullState, StepResult) {
	plan := TaskPlan{Ordinal: probeReviewerOrdinal, Role: HopCodeReviewer, Mode: modeReviewFull, OriginalMode: probeReviewNamespace, Reason: "probe",
		Inputs: []PlanInput{}, Allowed: []string{"review.md"}, ExpectedBase: p.cand.String()}
	plan.Goal, plan.Acceptance = p.reviewEnvelope()
	st.Pending = &plan
	return st, StepResult{Action: actionDispatch, Plan: &plan}
}

// reviewEnvelope is the reviewer probe's goal and acceptance.
func (p *devProbe) reviewEnvelope() (string, string) {
	d := p.d
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	line("Callsheet qualification reviewer probe. Run %s.", d.s.runID)
	line("This is a harness-authored verification fixture, not an owner-approved feature: there is no owner receipt and no design audit. It qualifies the reviewer's execution evidence only.")
	line("candidate_commit: %s (the pinned fixture candidate)", p.cand)
	line("initial_source_commit: %s", p.seed)
	line("design_path: design.md (the fixture's own design)")
	line("allowed_outputs: review.md; every other path stays byte-identical with the same mode")
	line("verification_helper: run exactly this one command once, from your checkout's root, with your actual task ID substituted and nothing else changed:")
	line("  %s", d.helper.command("<your task ID>", p.cand.String(), p.seed.String()))
	line("review_fields: review.md has these plain lines outside code fences, exact case and spacing:")
	line("  Reviewed candidate: <candidate_commit>")
	line("  Source sha256: <path> <sha256> (one line each for FEATURE.md, go.mod, main.go and main_test.go at the candidate)")
	line("  Verification exit <exit>: go test ./...")
	line("  Verification exit <exit>: go test -coverprofile=%s/<your task ID>/coverage.out ./... (the helper's absolute scratch path)", reviewScratch)
	line("  Coverage: <the helper's coverage percent>%% <scope>")
	line("  Cleanup: <result>")
	line("  Reviewer run sha256: <64 lowercase hex: the SHA-256 of the helper's complete JSON output, including its final LF>")
	line("review_blocker: exactly one line CALLSHEET_REVIEW_BLOCKER: none (or environment, only with REVIEW_CHANGES_REQUESTED) immediately before the terminal STATUS line, in both review.md and the final answer")
	return b.String(), devAcceptance(&TaskPlan{Role: HopCodeReviewer, Mode: modeReviewFull})
}

// label is a probe plan's evidence label.
func (p *devProbe) label(plan *TaskPlan) string {
	if plan.Role == HopCodeReviewer {
		return probeReviewNamespace + "-01"
	}
	return probeWakeNamespace + "-" + hopDir(plan.Ordinal-1)
}

// dir is a probe plan's evidence directory.
func (p *devProbe) dir(plan *TaskPlan) string {
	if plan.Role == HopCodeReviewer {
		return "probe/reviewer/01"
	}
	return "probe/wake/" + hopDir(plan.Ordinal-1)
}

// binding compares a probe task with its plan: the exact request, the
// frozen selection, no override; no workspace for a wake, the pinned probe
// workspace and candidate for the reviewer.
func (p *devProbe) binding(v contract.TaskView, plan TaskPlan) error {
	d := p.d
	role, ok := d.flow.Role(plan.Role)
	switch {
	case !ok || v.Role.ID != plan.Role || v.Role.Adapter != role.Adapter:
		return errors.New("the task's role is not the plan's")
	case v.Request.Goal != plan.Goal || v.Request.Acceptance != plan.Acceptance || len(v.Request.Payload) != 0 || v.Request.Override != nil:
		return errors.New("the dispatched request is not the prepared plan")
	case v.Effective.Model != role.Model || v.Effective.Effort != role.Effort:
		return fmt.Errorf("the effective pair is %s/%s", v.Effective.Model, v.Effective.Effort)
	}
	b := v.WorkspaceBinding
	if plan.Role == HopDesigner {
		if b != nil {
			return errors.New("a wake probe used a workspace")
		}
		return nil
	}
	if b == nil || b.Name != d.s.workspace.Name || b.Instance != d.s.workspace.Instance || b.BaseCommit == nil || *b.BaseCommit != plan.ExpectedBase {
		return errors.New("the workspace binding is not the probe's")
	}
	return nil
}

// admitWake admits a wake task's answer (like an authentication answer:
// success, a complete nonempty final message; the token echo is
// information only) and selects the next probe step.
func (p *devProbe) admitWake(v contract.TaskView, consumedSeq int) {
	d := p.d
	sl := d.slot
	plan := *sl.plan
	if err := p.binding(v, plan); err != nil {
		d.stop("binding_rejected", err.Error())
		return
	}
	r := v.Result
	if v.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0 || r.FinalMessage == nil || r.FinalMessageTruncated ||
		!authAnswerPresent(*r.FinalMessage) {
		d.stop("probe_wake_failed", "wake probe "+v.TaskID+" gave no complete nonempty answer")
		return
	}
	sel, _ := d.flow.Role(plan.Role)
	out := Admitted{Ordinal: plan.Ordinal, TaskID: v.TaskID, Role: plan.Role, Mode: plan.Mode, Selection: sel, Final: *r.FinalMessage, CreatedAt: v.CreatedAt,
		AdmittedSeq: sl.admittedSeq, ObservedSeq: sl.observedSeq, ConsumedSeq: consumedSeq, Artifacts: map[string]string{}}
	if v.FinishedAt != nil {
		out.FinishedAt = *v.FinishedAt
	}
	if d.deadlines() {
		return
	}
	st, step := p.next(d.state, out)
	d.slot = nil
	d.apply(st, step)
	d.writeEvidenceJSON(p.dir(&plan)+"/record.json", envelope("task-record", d.s.runID, st.Tasks[len(st.Tasks)-1]))
}

// next is the probe's fixed plan provider: wake 1, the gate, wake 2, the
// gate, wake 3, the reviewer probe, completion. It never calls Next.
func (p *devProbe) next(st DevFullState, out Admitted) (DevFullState, StepResult) {
	pending := st.Pending
	st.Tasks = append(slices.Clip(st.Tasks), TaskRecord{Ordinal: out.Ordinal, TaskID: out.TaskID, Role: out.Role, Mode: out.Mode, Reason: "probe",
		Selection: out.Selection, Workspace: out.Workspace, Instance: out.Instance, Base: out.Base, Commit: out.Commit, Tree: out.Tree,
		Marker: out.Marker, Disposition: dispositionReport, Artifacts: out.Artifacts, CreatedAt: out.CreatedAt, FinishedAt: out.FinishedAt,
		AdmittedSeq: out.AdmittedSeq, ObservedSeq: out.ObservedSeq, ConsumedSeq: out.ConsumedSeq})
	st.Pending = nil
	switch {
	case pending.Role == HopDesigner && out.Ordinal < maxProbePhases:
		p.wakes = out.Ordinal
		return st, StepResult{Action: actionPause, Pause: pauseProbe, Generation: out.Ordinal}
	case pending.Role == HopDesigner:
		p.wakes = out.Ordinal
		p.setStage("resume_wake", stagePass, "")
		p.setStage("print_notification", stagePass, "")
		return p.reviewStep(st)
	}
	p.reviewed = true
	st.Complete, st.ReviewTask, st.ReviewCommit = true, out.TaskID, out.Commit
	p.reviewC = out.Commit
	p.setStage("reviewer_evidence", stagePass, "")
	return st, StepResult{Action: actionComplete}
}

// decision applies one continuation decision: continue P schedules the
// next phase's wake; no P stops. It creates no design receipt and never
// resets the attempt clock.
func (p *devProbe) decision(kind string, gen int) (DevFullState, StepResult) {
	if kind != "continue" {
		return p.d.state, StepResult{Action: actionStop, Stop: "probe_declined", Reason: fmt.Sprintf("the owner stopped qualification after phase %d", gen)}
	}
	return p.wakeStep(p.d.state, gen+1)
}

// probeGateRE and probeCompleteRE are a probe phase's only final results.
var (
	probeGateRE = regexp.MustCompile(`^gate=probe generation=([12])$`)
)

// parseProbePhaseGate reads a probe phase's complete final result: exactly
// one plain line gate=probe generation=N (N 1 or 2) or probe=complete,
// with only a final LF or CRLF optional. complete reports probe=complete.
func parseProbePhaseGate(text string) (gen int, complete bool, err error) {
	t := strings.TrimSuffix(text, "\n")
	t = strings.TrimSuffix(t, "\r")
	switch {
	case strings.ContainsAny(t, "\r\n"):
		return 0, false, errors.New("a probe phase result is one line")
	case t == "probe=complete":
		return 0, true, nil
	}
	m := probeGateRE.FindStringSubmatch(t)
	if m == nil {
		return 0, false, errors.New("not a probe gate")
	}
	return int(m[1][0] - '0'), false, nil
}

// probeLineKind classifies one owner line in a probe window: exactly
// "continue P" or "no P" for its own phase (only the line terminator is
// removed; nothing is trimmed).
func probeLineKind(line string, phase int) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	switch line {
	case "continue " + itoa(phase):
		return "continue", true
	case "no " + itoa(phase):
		return "no", true
	}
	return "", false
}

// probeDir maps a plan to its evidence directory and label (the full flow
// uses tasks/NN and workflow-NN).
func (d *devSupervisor) planDir(plan *TaskPlan) string {
	if d.probe != nil {
		return d.probe.dir(plan)
	}
	return taskDir(plan.Ordinal)
}

func (d *devSupervisor) planLabel(plan *TaskPlan) string {
	if d.probe != nil {
		return d.probe.label(plan)
	}
	return workflowLabel(plan.Ordinal)
}

// probeFinish writes the qualification record with its inventory and
// decides QUALIFIED or BLOCKED, fail-closed: every stage passed, cleanup
// was complete, the owner environment is unchanged and the record's own
// offline check holds.
//
// The record's paid-call count is the derived accounting's, from the
// admissions and settlements (failed and interrupted calls included).
func (d *devSupervisor) probeFinish(cost DevCost) bool {
	p := d.probe
	for _, name := range probeStages {
		result, code := stageUnreached, d.stopCode
		if d.stopCode == "" {
			result, code = stageFail, "stage_unobserved"
		}
		if name == "models" && len(d.s.preflight) < len(DevFullRoleIDs) {
			result = stageFail
		}
		p.setStage(name, result, code)
	}
	q := DevQualification{Schema: devKind(DevQualificationKind), RunID: d.s.runID, Result: ResultBlocked, Revision: d.manifest.Revision,
		CallsheetVersion: d.manifest.CallsheetVersion, GOOS: d.s.goos, GOARCH: d.s.goarch, Gate: d.manifest.Gate, Versions: d.manifest.Versions,
		FlowLabel: d.flowLabel, SelectionSHA256: d.manifest.SelectionSHA256, Roles: d.manifest.Roles, Executable: d.manifest.Executable,
		Helper: d.helper, SessionID: d.sessionID, Workspace: d.s.workspace, Candidate: p.cand.String(), ReviewCommit: p.reviewC,
		ProtectedRoots: append([]string{}, d.s.manifest.ProtectedRoots...), ProtectDepth: d.s.manifest.ProtectDepth,
		Stages: orderedStages(p.stages), Prompts: p.probePrompts(), Fixture: p.probeFixtureRecords(), Phases: len(d.phases),
		WorkerCalls: cost.WorkerCalls, AttemptStart: d.manifest.AttemptStart,
		AttemptDeadline: d.manifest.AttemptDeadline, Render: d.manifest.Render, Inventory: []InventoryEntry{}}
	if p.cand.IsZero() {
		q.Candidate = ""
	}
	if b, err := readBounded(d.s.w.Executable, 1<<30); err == nil {
		q.Executable.After = sha256Hex(b)
	}
	if d.stopCode != "" {
		code, seq := d.stopCode, d.stopSeq
		q.StopCode, q.StopSeq = &code, &seq
	}
	if qualifiedStages(q.Stages) && d.cleanupOK && !d.protected && d.stopCode == "" && d.executed {
		q.Result = ResultQualified
	}
	inv, err := diskInventory(d.s.ev.root)
	if err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e probe-dev-full: evidence collection failed: %v\n", err)
		return false
	}
	q.Inventory = inv
	if err := d.s.ev.writeJSON(devQualificationFile, q); err != nil {
		fmt.Fprintf(d.s.stderr, "reale2e probe-dev-full: %s not written: %v\n", devQualificationFile, err)
		return false
	}
	// Fail closed: the written record must pass its own offline check.
	if q.Result == ResultQualified {
		b, err := LoadDevQualification(d.s.ev.root)
		if err != nil {
			fmt.Fprintf(d.s.stderr, "reale2e probe-dev-full: the qualification record does not load: %v\n", err)
			return false
		}
		if codes := CheckDevQualification(b); len(codes) > 0 {
			fmt.Fprintf(d.s.stderr, "reale2e probe-dev-full: the qualification record fails its check: %v\n", codes)
			return false
		}
	}
	for _, s := range q.Stages {
		d.s.say("probe stage %s: %s %s", s.Name, s.Result, s.Code)
	}
	return q.Result == ResultQualified
}

// orderedStages is stages in probeStages order.
func orderedStages(stages []DevProbeStage) []DevProbeStage {
	out := []DevProbeStage{}
	for _, name := range probeStages {
		for _, s := range stages {
			if s.Name == name {
				out = append(out, s)
			}
		}
	}
	return out
}

// qualifiedStages reports every stage passed, in order.
func qualifiedStages(stages []DevProbeStage) bool {
	if len(stages) != len(probeStages) {
		return false
	}
	for i, s := range stages {
		if s.Name != probeStages[i] || s.Result != stagePass {
			return false
		}
	}
	return true
}

// writeProbeRepository writes the qualification repository: the seed,
// candidate and review object closure under refs/heads/seed, candidate and
// review (as reached), HEAD at the seed.
func (d *devSupervisor) writeProbeRepository() {
	p := d.probe
	if d.s.seed == nil || p.cand.IsZero() {
		return
	}
	dir := filepath.Join(d.s.ev.root, "workspace", "repo.git")
	r, err := git.PlainInit(dir, true)
	if err != nil {
		d.stop("evidence_write_failed", err.Error())
		return
	}
	refs := map[string]plumbing.Hash{"refs/heads/seed": p.seed, "refs/heads/candidate": p.cand}
	roots := []plumbing.Hash{p.cand}
	if p.reviewC != "" {
		refs["refs/heads/review"] = plumbing.NewHash(p.reviewC)
		roots = append(roots, plumbing.NewHash(p.reviewC))
	}
	if err := copyObjects(d.s.collect.Storer, r.Storer, roots); err != nil {
		d.stop("evidence_write_failed", err.Error())
		return
	}
	for name, h := range refs {
		if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), h)); err != nil {
			d.stop("evidence_write_failed", err.Error())
			return
		}
	}
	if err := r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/seed")); err != nil {
		d.stop("evidence_write_failed", err.Error())
		return
	}
	tm := DevTreeManifest{SeedCommit: p.seed.String(), SeedTree: d.s.seed.Tree.String(), Tasks: append([]devCommit{}, d.commits...)}
	if err := d.s.ev.writeJSON(devTreesFile, envelope("trees", d.s.runID, tm)); err != nil {
		d.stop("evidence_write_failed", err.Error())
	}
}

// The closed qualification layout.

var (
	probeWakeRE     = regexp.MustCompile(`^probe/wake/0[1-3]/(plan\.json|dispatch\.json|task\.json|final\.txt|observation\.json|record\.json|wait\.json|wait-result\.json|wait-stderr\.txt|wait-exit\.txt|logs\.txt|logs\.json)$`)
	probeReviewRE   = regexp.MustCompile(`^probe/reviewer/01/(plan\.json|dispatch\.json|task\.json|final\.txt|observation\.json|admission\.json|record\.json|wait\.json|wait-result\.json|wait-stderr\.txt|wait-exit\.txt|logs\.txt|logs\.json|review-evidence\.json)$`)
	probeDecisionRE = regexp.MustCompile(`^probe/decisions/0[12]\.json$`)
	probeSessionRE  = regexp.MustCompile(`^session/(phase-[1-3]\.stdout\.jsonl|phase-[1-3]\.stderr\.txt|transcript-phase-[1-3]\.jsonl|coordinator-exit\.json)$`)
	probeSetupRE    = regexp.MustCompile(`^setup/(launch-phase-[1-3]\.json|probe-phase-[1-3]\.md)$`)
	probeRefRE      = regexp.MustCompile(`^refs/heads/(seed|candidate|review)$`)
	probeDirRE      = regexp.MustCompile(`^probe/(wake/0[1-3]|reviewer/01)$`)
)

// probeFileAllowed reports a closed qualification layout file.
func probeFileAllowed(rel string) bool {
	switch rel {
	case devQualificationFile, "events.jsonl", devAdmissionsFile, devCleanupFile, devCostFile, devTreesFile, "environment/diff.json",
		"setup/sources.json", "setup/installed-manuals.json", "setup/flow-label.json", "setup/feature-request.json", "setup/flow.json",
		"setup/roles.json", "setup/mcp-config.json", "setup/seed-push.json", probeSystemSourceFile, probePhaseSourceFile, probeSystemFile:
		return true
	}
	for _, name := range probeFixtureFiles {
		if rel == "setup/probe-fixture/"+name {
			return true
		}
	}
	switch {
	case devPreflightRE.MatchString(rel), probeWakeRE.MatchString(rel), probeReviewRE.MatchString(rel), probeDecisionRE.MatchString(rel),
		probeSessionRE.MatchString(rel), probeSetupRE.MatchString(rel):
		return true
	case slices.Contains(envFiles("before"), rel) || slices.Contains(envFiles("after"), rel):
		return true
	}
	return devSourcePaths()[rel]
}

// probeDirAllowed reports a closed qualification layout directory.
func probeDirAllowed(rel string) bool {
	switch rel {
	case "probe", "probe/wake", "probe/reviewer", "probe/decisions", "setup/probe-fixture":
		return true
	}
	if probeDirRE.MatchString(rel) {
		return true
	}
	return devDirAllowed(rel) && !strings.HasPrefix(rel, "tasks") && rel != "transitions" && rel != "decisions" && rel != "receipts" && rel != "validation"
}

// probeDocType is a qualification record's strict type at rel.
func probeDocType(rel string) (any, string) {
	switch {
	case rel == devQualificationFile:
		return &DevQualification{}, devKind(DevQualificationKind)
	case probeDecisionRE.MatchString(rel):
		return &devEnvelope[DevDecision]{}, devKind("decision")
	case strings.HasPrefix(rel, "probe/"):
		return devDocType("tasks/01/" + filepath.Base(rel))
	}
	return devDocType(rel)
}

func (m *DevQualification) schemaID() string { return m.Schema }
func (m *DevQualification) runIDOf() string  { return m.RunID }

// probeLayout is the qualification record's closed layout.
var probeLayout = devLayout{file: probeFileAllowed, dir: probeDirAllowed, doc: probeDocType, ref: func(n string) bool { return probeRefRE.MatchString(n) }}

// LoadDevQualification loads a qualification evidence directory under its
// closed layout (the full-flow loader never admits it).
func LoadDevQualification(root string) (*DevBundle, error) {
	return loadDevLayout(root, probeLayout)
}

// CheckDevQualification re-checks a QUALIFIED record from its captured
// evidence and returns every contradiction (none: it holds): the
// inventory, the prompts' provenance from the constants, the captured
// fixture, every stage passed, the three wakes' qualified print-stream
// waits, the reviewer's re-derived execution evidence, cleanup and the
// owner environment.
func CheckDevQualification(b *DevBundle) []string {
	var codes []string
	bad := func(code string) {
		if !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	q := doc[DevQualification](b, devQualificationFile)
	if q == nil {
		return []string{"qualification_missing"}
	}
	if q.Result != ResultQualified || !qualifiedStages(q.Stages) || q.StopCode != nil {
		bad("not_qualified")
	}
	if !bytes.Equal(mustJSON(b.inventory()), mustJSON(q.Inventory)) {
		bad("inventory_mismatch")
	}
	if !bytes.Equal(b.Files[probeSystemSourceFile], []byte(devProbeSystemPrompt)) || !bytes.Equal(b.Files[probePhaseSourceFile], []byte(devProbePhasePrompt)) {
		bad("prompt_provenance_mismatch")
	}
	sys, err := renderProbePrompt("devProbeSystemPrompt", devProbeSystemPrompt, q.Render)
	play := b.Files["setup/sources/"+DevFlowDir+"/"+DevPlaybook]
	if err != nil || !bytes.Equal(b.Files[probeSystemFile], append(append(append([]byte(nil), play...), '\n'), sys...)) {
		bad("prompt_provenance_mismatch")
	}
	for n := 1; n <= maxProbePhases; n++ {
		want, err := renderProbePrompt("devProbePhasePrompt", devProbePhasePrompt, probePhaseValues(q.Render, n))
		if err != nil || !bytes.Equal(b.Files[fmt.Sprintf("setup/probe-phase-%d.md", n)], want) {
			bad("prompt_provenance_mismatch")
		}
	}
	for _, f := range q.Fixture {
		if data, ok := b.Files[f.Path]; !ok || sha256Hex(data) != f.SHA256 || len(data) != f.Bytes {
			bad("fixture_mismatch")
		}
	}
	if len(q.Fixture) != len(probeFixtureFiles) {
		bad("fixture_mismatch")
	}
	sid := ""
	for p := 1; p <= maxProbePhases; p++ {
		data, ok := b.Files[devStreamFile(p)]
		if !ok {
			bad("phase_missing")
			continue
		}
		s := decodeDevStreamAs(devStreamFile(p), data, sid)
		if sid == "" {
			sid = s.sessionID
		}
		if s.problem != "" || !s.resultOK {
			bad("phase_stream_invalid")
		}
		end := strings.TrimSuffix(strings.TrimSuffix(s.resultText, "\n"), "\r")
		if end != probePhaseEnd(p) {
			bad("phase_result")
		}
	}
	if sid == "" || sid != q.SessionID {
		bad("session_mismatch")
	}
	for n := 1; n <= maxProbePhases; n++ {
		dir := "probe/wake/" + hopDir(n-1)
		v := doc[contract.TaskShowResponse](b, dir+"/task.json")
		if v == nil {
			bad("wake_missing")
			continue
		}
		recs, _ := probeRecords(b, n)
		w, src, problem := deriveDevWait(recs, devWaitCommand(q.Render["REALE2E"], q.Render["RUNTIME"], v.Task.TaskID, n), v.Task.TaskID)
		if problem != "" || !w.complete() || src.wake.File != devStreamFile(n) {
			bad("print_notification_unobserved")
		}
		if v.Task.WorkspaceBinding != nil || v.Task.Request.Goal != wakeGoal(q.RunID, n) || v.Task.Request.Acceptance != probeWakeAcceptance ||
			v.Task.Request.Override != nil || len(v.Task.Request.Payload) != 0 {
			bad("wake_binding_mismatch")
		}
	}
	// The reviewer probe's execution, re-derived from its primary records
	// against the pinned objects and its own task (never the stored
	// verdict alone), equal to the stored evidence.
	if code := probeReviewerCheck(b, q); code != "" {
		bad(code)
	}
	// The derived accounting holds every paid call the record counts and
	// every settled task.
	if cost := doc[DevCost](b, devCostFile); cost == nil {
		bad("cost_mismatch")
	} else {
		want := deriveDevCost(b, q.RunID)
		accounted := map[string]bool{}
		for _, t := range want.Tasks {
			accounted[t.Task] = true
		}
		settledOK := true
		if cl := doc[devEnvelope[DevCleanup]](b, devCleanupFile); cl != nil {
			for _, st := range cl.Data.Settled {
				settledOK = settledOK && accounted[st.Task]
			}
		}
		if !bytes.Equal(mustJSON(*cost), mustJSON(want)) || want.WorkerCalls != q.WorkerCalls || !settledOK {
			bad("cost_mismatch")
		}
	}
	if !mcpConfigOK(b.Files["setup/mcp-config.json"]) {
		bad("mcp_config_invalid")
	}
	// Every recorded Callsheet call of the probe phases holds the
	// exercise's call policy and bindings against the qualification's own
	// namespace authority; every probe task's recorded MCP dispatch is
	// exactly its prepared document, answered strictly, in its own phase
	// (the full flow's reconciliation).
	if len(probeMCPProblems(b, q)) > 0 {
		bad(devMCPCallInvalid)
	}
	for _, dir := range []string{"probe/wake/01", "probe/wake/02", "probe/wake/03", "probe/reviewer/01"} {
		obs := doc[DevObservation](b, dir+"/observation.json")
		if obs == nil {
			continue
		}
		dd := doc[devDispatchDoc](b, dir+"/dispatch.json")
		recs, err := probeRecords(b, obs.Phase)
		if dd == nil || err != nil {
			bad("dispatch_mismatch")
			continue
		}
		var auth *contract.TaskView
		if v := doc[contract.TaskShowResponse](b, dir+"/task.json"); v != nil {
			auth = &v.Task
		}
		if _, _, _, problem := dispatchFacts(recs, dd, auth, obs.Task); problem != "" {
			bad("dispatch_mismatch")
		}
	}
	// Every record names this run.
	for _, v := range b.docs {
		if id, has := runIDOf(v); has && id != q.RunID {
			bad("run_id_mismatch")
		}
	}
	if cl := doc[devEnvelope[DevCleanup]](b, devCleanupFile); cl == nil || !cl.Data.Complete {
		bad("cleanup_incomplete")
	}
	env := envCheckInput{manifest: true, roots: q.ProtectedRoots, depth: q.ProtectDepth, files: b.Files, diff: doc[EnvDiff](b, "environment/diff.json"),
		notReached: func() bool { return false }}
	checkOwnerEnvironment(env, func(code string) { bad(code) })
	return codes
}

// probeReviewerCheck re-derives the reviewer probe's execution evidence
// from its log, its TaskLogs metadata, its authoritative task and the
// evidence repository's pinned seed, candidate and review commits, and
// requires a verified approval equal to the stored evidence ("" when it
// holds).
func probeReviewerCheck(b *DevBundle, q *DevQualification) string {
	const dir = "probe/reviewer/01"
	v := doc[contract.TaskShowResponse](b, dir+"/task.json")
	plan := doc[devEnvelope[TaskPlan]](b, dir+"/plan.json")
	stored := doc[DevReviewRecord](b, dir+"/review-evidence.json")
	if v == nil || plan == nil || stored == nil || b.Repo == nil {
		return "reviewer_evidence_unobserved"
	}
	if _, ok := b.Files[dir+"/logs.txt"]; !ok {
		return "reviewer_evidence_unobserved"
	}
	tree := func(ref string) (string, string, map[string]TreeFile, bool) {
		h, ok := b.Repo.refs["refs/heads/"+ref]
		if !ok {
			return "", "", nil, false
		}
		cm, err := b.Repo.repo.CommitObject(h)
		if err != nil {
			return "", "", nil, false
		}
		files, err := readTree(b.Repo.repo, cm, ref)
		if err != nil {
			return "", "", nil, false
		}
		m := map[string]TreeFile{}
		for _, f := range files {
			m[f.Path] = f
		}
		return h.String(), cm.TreeHash.String(), m, true
	}
	seed, _, seedFiles, ok1 := tree("seed")
	cand, candTree, candFiles, ok2 := tree("candidate")
	review, _, reviewFiles, ok3 := tree("review")
	if !ok1 || !ok2 || !ok3 || cand != q.Candidate || review != q.ReviewCommit || plan.Data.ExpectedBase != cand {
		return "reviewer_binding_mismatch"
	}
	r := v.Task.Result
	if r == nil || r.FinalMessage == nil || r.Workspace == nil || r.Workspace.Commit == nil || *r.Workspace.Commit != review ||
		v.Task.Request.Override != nil || len(v.Task.Request.Payload) != 0 {
		return "reviewer_binding_mismatch"
	}
	marker, _ := parseTerminalMarker(*r.FinalMessage)
	in := bundleReviewInput(b, dir, plan.Data.Ordinal, &v.Task, q.Helper, cand, seed, candTree, candFiles, fileContent(seedFiles, "main.go"), marker,
		fileText(reviewFiles, "review.md"))
	derived := DeriveDevReviewEvidence(in)
	switch {
	case derived.Status != evidenceVerified || !derived.Approving:
		return "reviewer_evidence_unobserved"
	case !bytes.Equal(mustJSON(stored.Evidence), mustJSON(derived)):
		return "reviewer_evidence_mismatch"
	}
	return ""
}

// probeMCPProblems judges the qualification's recorded Callsheet calls
// against its own namespace: the probe-wake and probe-review records, the
// probe workspace and seed, and the qualification record's stop
// corroborated by its attempt_failed event.
func probeMCPProblems(b *DevBundle, q *DevQualification) map[int]string {
	in := mcpAuthorityInput{mainRef: "refs/heads/candidate", goos: q.GOOS, ws: q.Workspace, seedPath: q.Render["SEED_PATH"], stopped: probeStopped(b, q), dirs: map[int]string{},
		labels: map[string]int{}}
	for n := 1; n <= maxProbePhases; n++ {
		in.dirs[n] = "probe/wake/" + hopDir(n-1)
		in.labels[probeWakeNamespace+"-"+hopDir(n-1)] = n
	}
	in.dirs[maxProbePhases+1] = "probe/reviewer/01"
	in.labels[probeReviewNamespace+"-01"] = maxProbePhases + 1
	return mcpVerdict(mcpPhaseRecords(b, maxProbePhases), buildMCPAuthority(b, in))
}

// probeStopped reports the qualification record's stop corroborated by
// its first attempt_failed event (code and sequence).
func probeStopped(b *DevBundle, q *DevQualification) bool {
	if q.StopCode == nil || q.StopSeq == nil {
		return false
	}
	for _, e := range b.Events {
		if e.Type == EvAttemptFailed {
			return e.Seq == *q.StopSeq && e.Code == *q.StopCode
		}
	}
	return false
}

// probeRecords decodes probe phase n's stream records.
func probeRecords(b *DevBundle, n int) ([]sessionRecord, error) {
	data, ok := b.Files[devStreamFile(n)]
	if !ok {
		return nil, errors.New("no stream")
	}
	d := &deriver{codes: map[string]bool{}}
	return d.decodeLines(devStreamFile(n), data), nil
}
