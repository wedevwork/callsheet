package reale2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The scoped qualification probe (design 12a-dev-flow-light,
// Qualification; FP-9): probe-dev-full --scenario full|light|both. Light
// and both have identical stage coverage: the full probe's stages, then
// in the last phase one probe-triage task on the immutable light seed and,
// after one harness-prepared push of a known-good candidate descending
// from the admitted triage publication, one probe-light-review task that
// runs the exact light helper. The probe owns two workspaces (the full
// fixture's and the light seed's); its tasks are their own namespaces and
// never satisfy attempt evidence.

// Probe scopes.
const (
	probeScopeFull  = "full"
	probeScopeLight = "light"
	probeScopeBoth  = "both"
)

// Light probe namespaces, ordinals, directories and repositories.
const (
	probeTriageNamespace      = "probe-triage"
	probeLightReviewNamespace = "probe-light-review"
	probeTriageOrdinal        = 5
	probeLightReviewOrdinal   = 6
	probeTriageDir            = "probe/triage/01"
	probeLightReviewDir       = "probe/light-reviewer/01"
	probeFullRepoDir          = "workspace/full/repo.git"
	probeLightRepoDir         = "workspace/light/repo.git"
	// Push stages: before the first dispatch, or after the triage stage.
	pushAfterSeed   = "seed"
	pushAfterTriage = "triage"
	// Paid worker call bounds.
	probeMaxCallsFull  = 9
	probeMaxCallsLight = 11
)

// CodeCandidateUnpublished is dev-next's answer (exit 0) while the light
// probe's prepared candidate is not yet on the light workspace's main
// branch; its summary names the candidate commit to push.
const CodeCandidateUnpublished = "candidate_unpublished"

// probeLightStages are the light and both scopes' stages in order.
var probeLightStages = []string{"models", "resume_wake", "print_notification", "reviewer_evidence", "triage_evidence", "light_reviewer_evidence"}

// probeScopeOK reports a valid --scenario value.
func probeScopeOK(s string) bool {
	return s == probeScopeFull || s == probeScopeLight || s == probeScopeBoth
}

// probeLightScope reports a scope with the light stages.
func probeLightScope(s string) bool { return s == probeScopeLight || s == probeScopeBoth }

// probeStagesFor is a scope's stage coverage in order.
func probeStagesFor(scope string) []string {
	if probeLightScope(scope) {
		return probeLightStages
	}
	return probeStages
}

// probeMaxCalls is a scope's paid worker call bound.
func probeMaxCalls(scope string) int {
	if probeLightScope(scope) {
		return probeMaxCallsLight
	}
	return probeMaxCallsFull
}

// ProbeLightBounds is the light and both probe's disclosure.
const ProbeLightBounds = `Cost and time bounds of the scoped qualification probe, --scenario light or both (policy, not measurements):
  - five paid authentication preflight tasks, each at most 2m; three harmless designer wake
    tasks, one code-reviewer task on the full fixture, one triage task on the light seed and
    one light code-reviewer task on a harness-authored known-good light candidate, each at
    most 10m: at most eleven paid worker calls, no retries; light and both have identical
    stage coverage (the full reviewer stage included);
  - the headless Claude Code coordinator adds vendor-dependent turns in up to three phases;
  - each owner continuation window at most 20m; the probe at most 90m from preflight start
    (the first owner-environment fingerprint included), then at most 2m of cleanup;
  - a failed stage stops the probe; no token-price ceiling is claimed; a qualification is
    disclosed separately and never counted in an attempt's or exercise's paid calls.
`

// ProbeBoundsFor is a scope's disclosure.
func ProbeBoundsFor(scope string) string {
	if probeLightScope(scope) {
		return ProbeLightBounds
	}
	return ProbeBounds
}

// devProbeLightSystemPrompt is the light and both probe's coordinator
// instructions (the full probe keeps devProbeSystemPrompt unchanged).
const devProbeLightSystemPrompt = `Qualification probe instructions (a harness-owned constant). This session qualifies the installation for run {{RUN_ID}} with scope {{PROBE_SCOPE}}; it is not feature work and it is not the development workflow. Do not design, implement, investigate or review anything yourself, do not answer or synthesize an owner decision, an intake or a design receipt, and do not follow any workflow transition of the playbook: only {{REALE2E}} dev-next names a task you may dispatch, and it names only probe tasks.

The commands are {{REALE2E}} dev-next --run {{RUNTIME}}, {{REALE2E}} dev-observe --run {{RUNTIME}} --ordinal N --task ID and exactly one harness-managed background command {{REALE2E}} dev-wait-task --run {{RUNTIME}} --task ID --ordinal N per dispatched task, with no &, setsid, redirect, pipe or detached shell. Start each wait with one Bash call passing exactly command (that command), description (a short string) and run_in_background: true, with no timeout and no other key. For prepared PATH SHA256, read the document, check its SHA-256 and dispatch through MCP with exactly its target, goal and acceptance, and its workspace, workspace_instance and base only when it names them, with no payload. Report the admitted task with dev-observe, start its wait, and when its completion notification arrives, consume it with one Callsheet task_show request {"id": "<that task id>", "lines": 0} before anything else; then ask dev-next again. When dev-next answers pending, ask again; never dispatch a task twice.

Seed pushes: in the first phase, before the first dev-next, call ws_push {"name":"{{WORKSPACE}}","instance":"{{INSTANCE}}","branch":"main","path":"{{SEED_PATH}}"} (its commit must be {{SEED_COMMIT}}), then ws_push {"name":"{{LIGHT_WORKSPACE}}","instance":"{{LIGHT_INSTANCE}}","branch":"main","path":"{{LIGHT_SEED_PATH}}"} (its commit must be {{LIGHT_SEED_COMMIT}}). Until the supervisor sees both commits on their workspaces' main branches, dev-next answers seed_unpublished with exit status 1, and calling it earlier fails the probe. Candidate push: in the last phase, after the probe-triage task, dev-next answers candidate_unpublished C with exit status 0 once the harness has prepared the light candidate C; then call ws_push {"name":"{{LIGHT_WORKSPACE}}","instance":"{{LIGHT_INSTANCE}}","branch":"main","path":"{{LIGHT_CANDIDATE_PATH}}"} exactly once (its commit must be C) and ask dev-next again. A tool denial stops the probe; never widen permissions or edit any configuration.

Callsheet tools: this probe permits exactly these nine Callsheet MCP tools: dispatch, task_ls, task_show, task_logs, task_cancel, ws_status, ws_diff, ws_push and ws_pull. Every other Callsheet tool is forbidden. task_wait is forbidden in this exercise: the shared playbook's task_wait fallback for an unqualified wake does not apply, and an unqualified wake ends the phase. dispatch never takes a wait. ws_push is only these three pushes: the two seed pushes before the first dispatch and the one candidate push after candidate_unpublished; never another push, a retry or another path. task_cancel is only for this probe's own dispatched tasks when the probe stops.
`

// DevProbeLight is a light or both qualification's light record.
type DevProbeLight struct {
	Workspace     WorkspaceRecord  `json:"workspace"`
	Seed          string           `json:"seed"`
	SeedTree      string           `json:"seed_tree"`
	TriageTask    string           `json:"triage_task"`
	TriageCommit  string           `json:"triage_commit"`
	Candidate     string           `json:"candidate"`
	CandidateTree string           `json:"candidate_tree"`
	ReviewTask    string           `json:"review_task"`
	ReviewCommit  string           `json:"review_commit"`
	Fixture       []DevProbeSource `json:"fixture"`
	Pushes        []DevProbePush   `json:"pushes"`
}

// DevProbePush is one planned workspace push of the light probe, in
// order: the full fixture's and the light seed's before the first dispatch
// and the light candidate's after the triage stage; its returned commit
// and tree, and the event sequence at which the supervisor saw it on the
// workspace's main branch (0: never).
type DevProbePush struct {
	Order       int    `json:"order"`
	Workspace   string `json:"workspace"`
	Instance    string `json:"instance"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	Commit      string `json:"commit"`
	Tree        string `json:"tree"`
	After       string `json:"after"`
	VerifiedSeq int    `json:"verified_seq"`
}

// probeLight is the light probe's state.
type probeLight struct {
	files        map[string][]byte
	seed         *seedRepo
	ws           WorkspaceRecord
	candPath     string
	candFiles    map[string][]byte
	triageTask   string
	triage       string
	cand         plumbing.Hash
	candTree     plumbing.Hash
	reviewTask   string
	reviewC      string
	seedVerified bool
	candVerified bool
	pushes       []DevProbePush
}

// lightFixture prepares the light scope's second workspace: the committed
// light seed (captured inertly), its known-good candidate files and the
// light workspace the coordinator pushes the seed to.
func (p *devProbe) lightFixture() error {
	if !probeLightScope(p.scope) {
		return nil
	}
	d := p.d
	files, tree, err := d.readLightFixture()
	if err != nil {
		return err
	}
	l := &probeLight{files: files}
	p.light = l
	if err := d.captureLightFixture(files); err != nil {
		return err
	}
	seed, err := createSeed(filepath.Join(d.s.runtime, "probe-light-seed"), files)
	if err != nil {
		return failure("seed_failed", err)
	}
	l.seed = seed
	if err := copyObjects(seed.repo.Storer, d.s.collect.Storer, []plumbing.Hash{seed.Commit}); err != nil {
		return failure("seed_failed", err)
	}
	d.trees[seed.Commit.String()] = tree
	l.candFiles = map[string][]byte{}
	for k, v := range files {
		l.candFiles[k] = v
	}
	// The known-good restoration: the review probe's fixture source and
	// its complete ten-case test.
	l.candFiles["main.go"], l.candFiles["main_test.go"] = p.fixture["main.go"], p.fixture["main_test.go"]
	suffix, err := d.s.randomHex(4)
	if err != nil {
		return failure("internal_error", err)
	}
	ctx, cancel := d.s.opCtx()
	defer cancel()
	ws, err := d.s.plane.CreateWorkspace(ctx, "e2e-probe-light-"+suffix)
	if err != nil {
		return failure("workspace_create_failed", err)
	}
	l.ws = WorkspaceRecord{Name: ws.Name, Instance: ws.Instance, SeedCommit: seed.Commit.String(), SeedTree: seed.Tree.String()}
	l.candPath = filepath.Join(d.s.runtime, "probe-light-candidate")
	full := d.s.workspace
	l.pushes = []DevProbePush{
		{Order: 1, Workspace: full.Name, Instance: full.Instance, Branch: contract.DefaultBranchRef, Path: p.repoPath, Commit: full.SeedCommit, Tree: full.SeedTree,
			After: pushAfterSeed},
		{Order: 2, Workspace: l.ws.Name, Instance: l.ws.Instance, Branch: contract.DefaultBranchRef, Path: seed.Path, Commit: l.ws.SeedCommit, Tree: l.ws.SeedTree,
			After: pushAfterSeed},
		{Order: 3, Workspace: l.ws.Name, Instance: l.ws.Instance, Branch: contract.DefaultBranchRef, Path: l.candPath, After: pushAfterTriage},
	}
	return nil
}

// lightValues adds the light scope's prompt values.
func (p *devProbe) lightValues(vals map[string]string) {
	if p.light == nil {
		return
	}
	l := p.light
	vals["PROBE_SCOPE"] = p.scope
	vals["LIGHT_WORKSPACE"], vals["LIGHT_INSTANCE"] = l.ws.Name, l.ws.Instance
	vals["LIGHT_SEED_PATH"], vals["LIGHT_SEED_COMMIT"], vals["LIGHT_CANDIDATE_PATH"] = l.seed.Path, l.ws.SeedCommit, l.candPath
}

// systemTemplate is the scope's system prompt constant and its name.
func probeSystemTemplate(scope string) (string, string) {
	if probeLightScope(scope) {
		return devProbeLightSystemPrompt, "devProbeLightSystemPrompt"
	}
	return devProbeSystemPrompt, "devProbeSystemPrompt"
}

// triageStep schedules the probe-triage task on the light seed: the
// light seed becomes the state's immutable initial source.
func (p *devProbe) triageStep(st DevFullState) (DevFullState, StepResult) {
	l := p.light
	seed := l.ws.SeedCommit
	st.InitialCommit, st.ChainCommit, st.CandidateCommit = seed, seed, ""
	plan := TaskPlan{Ordinal: probeTriageOrdinal, Role: "triage", Mode: modeInvestigate, OriginalMode: probeTriageNamespace, Reason: "probe",
		Scenario: scenarioLight, Inputs: []PlanInput{}, Allowed: []string{"rca.md", "fix.md"}, ExpectedBase: seed}
	plan.Goal, plan.Acceptance = probeTriageEnvelope(p.d.s.runID, seed)
	st.Pending = &plan
	return st, StepResult{Action: actionDispatch, Plan: &plan}
}

// probeTriageEnvelope is the probe-triage task's goal and acceptance (the
// checker rebuilds it from the record).
func probeTriageEnvelope(run, seed string) (string, string) {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	line("Callsheet qualification triage probe. Run %s.", run)
	line("This is a harness-owned light qualification fixture, not an owner-authorized light attempt: there is no owner intake. It qualifies the triage publication and its admission only.")
	line("initial_source_commit: %s (the buggy light seed; it never changes)", seed)
	line("feature_request: FEATURE.md (workspace root; its first paragraph is the light request)")
	line("allowed_outputs: rca.md and fix.md; source, tests and configuration are read-only")
	line("%s", lightTriagePreservedInputs)
	line("evidence_location: %s", lightTriageEvidenceLocation)
	line("%s", lightTriageScratchPolicy)
	line("investigation: find the cause of the light request in the seed")
	line("%s", triageDesignRequiredInstruction)
	line("%s", triageConfidenceInstruction)
	line("%s", lightRootCauseInstruction)
	return b.String(), lightAcceptance(&TaskPlan{Role: "triage", Mode: modeInvestigate})
}

// lightReviewStep schedules the probe-light-review task on the pushed
// known-good candidate.
func (p *devProbe) lightReviewStep(st DevFullState) (DevFullState, StepResult) {
	l := p.light
	cand := l.cand.String()
	st.CandidateCommit, st.ChainCommit = cand, cand
	plan := TaskPlan{Ordinal: probeLightReviewOrdinal, Role: HopCodeReviewer, Mode: modeReviewLight, OriginalMode: probeLightReviewNamespace, Reason: "probe",
		Scenario: scenarioLight, Inputs: []PlanInput{}, Allowed: []string{"review.md"}, ExpectedBase: cand}
	plan.Goal, plan.Acceptance = probeLightReviewEnvelope(p.d.s.runID, p.d.helper, cand, l.ws.SeedCommit, l.triageTask, l.triage)
	st.Pending = &plan
	return st, StepResult{Action: actionDispatch, Plan: &plan}
}

// probeLightReviewEnvelope is the probe-light-review task's goal and
// acceptance (the checker rebuilds it from the record).
func probeLightReviewEnvelope(run string, helper reviewHelperSpec, cand, seed, triageTask, triage string) (string, string) {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	line("Callsheet qualification light reviewer probe. Run %s.", run)
	line("This is a harness-authored known-good light candidate, not an owner-authorized restoration or a coder's work: there is no owner intake. It qualifies the light reviewer's execution evidence only.")
	line("candidate_commit: %s (the harness-authored known-good candidate; its single parent is the admitted probe-triage publication)", cand)
	line("initial_source_commit: %s (the buggy light seed)", seed)
	line("brief: fix.md and rca.md of triage task %s commit %s", triageTask, triage)
	line("allowed_outputs: review.md; every other path stays byte-identical with the same mode")
	line("verification_helper: run exactly this one command once, from your checkout's root, with your actual task ID substituted and nothing else changed (no prefix, redirect, pipe or wrapper of your own):")
	line("  %s", helper.commandFor(scenarioLight, "<your task ID>", cand, seed))
	line("%s", lightReviewFieldsInstruction)
	line("review_blocker: exactly one line CALLSHEET_REVIEW_BLOCKER: none (or environment, only with REVIEW_CHANGES_REQUESTED) immediately before the terminal STATUS line, in both review.md and the final answer")
	return b.String(), lightAcceptance(&TaskPlan{Role: HopCodeReviewer, Mode: modeReviewLight})
}

// afterTriage admits the probe-triage outcome: only a current qualified
// brief with Design required: no passes the triage stage, which then
// prepares the known-good candidate and schedules the light review.
func (p *devProbe) afterTriage(st DevFullState, out Admitted) (DevFullState, StepResult) {
	l := p.light
	l.triageTask, l.triage = out.TaskID, out.Commit
	if (out.Marker != markerTriageConf && out.Marker != markerTriageProb) || out.DesignRequired != designRequiredNo || !out.BriefCurrent {
		p.setStage("triage_evidence", stageFail, "probe_triage_unqualified")
		return st, StepResult{Action: actionStop, Stop: "probe_triage_unqualified",
			Reason: fmt.Sprintf("the probe triage %s published no current qualified brief with Design required: no (%s)", out.TaskID, out.Marker)}
	}
	if err := p.buildCandidate(); err != nil {
		p.setStage("triage_evidence", stageFail, "candidate_failed")
		return st, StepResult{Action: actionStop, Stop: "candidate_failed", Reason: err.Error()}
	}
	p.setStage("triage_evidence", stagePass, "")
	return p.lightReviewStep(st)
}

// buildCandidate constructs the known-good light candidate in its own
// private repository: the admitted triage commit's tree with only main.go
// and main_test.go replaced by the known-good bytes, its single parent the
// triage commit, checked out cleanly at HEAD for the coordinator's push.
func (p *devProbe) buildCandidate() error {
	d, l := p.d, p.light
	triage := plumbing.NewHash(l.triage)
	base, ok := d.trees[l.triage]
	if !ok {
		return errors.New("the admitted triage tree is unavailable")
	}
	if _, err := os.Lstat(l.candPath); err == nil {
		return errors.New("the candidate repository already exists")
	}
	r, err := git.PlainInit(l.candPath, false)
	if err != nil {
		return err
	}
	if err := copyObjects(d.s.collect.Storer, r.Storer, []plumbing.Hash{triage}); err != nil {
		return err
	}
	head, err := r.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return err
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(head.Target(), triage)); err != nil {
		return err
	}
	wt, err := r.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Reset(&git.ResetOptions{Commit: triage, Mode: git.HardReset}); err != nil {
		return err
	}
	for _, name := range []string{"main.go", "main_test.go"} {
		if err := os.WriteFile(filepath.Join(l.candPath, name), l.candFiles[name], 0o644); err != nil {
			return err
		}
		if _, err := wt.Add(name); err != nil {
			return err
		}
	}
	sig := seedAuthor
	h, err := wt.Commit("Probe light candidate: a harness-authored known-good restoration (not a coder's work)\n",
		&git.CommitOptions{Author: &sig, Committer: &sig, Parents: []plumbing.Hash{triage}})
	if err != nil {
		return err
	}
	c, err := r.CommitObject(h)
	if err != nil {
		return err
	}
	if err := copyObjects(r.Storer, d.s.collect.Storer, []plumbing.Hash{h}); err != nil {
		return err
	}
	tree := map[string]TreeFile{}
	for name, f := range base {
		tree[name] = f
	}
	for _, name := range []string{"main.go", "main_test.go"} {
		tree[name] = TreeFile{Path: name, Mode: filemode.Regular, Content: l.candFiles[name]}
	}
	l.cand, l.candTree = h, c.TreeHash
	d.trees[h.String()] = tree
	d.commits = append(d.commits, devCommit{Ordinal: 0, Task: "", Commit: h.String(), Parent: l.triage, Tree: c.TreeHash.String()})
	l.pushes[2].Commit, l.pushes[2].Tree = h.String(), c.TreeHash.String()
	return nil
}

// afterLightReview admits the probe-light-review outcome: its verified
// approving execution evidence completes the probe.
func (p *devProbe) afterLightReview(st DevFullState, out Admitted) (DevFullState, StepResult) {
	l := p.light
	l.reviewTask, l.reviewC = out.TaskID, out.Commit
	if out.Marker != markerReviewApproved {
		p.setStage("light_reviewer_evidence", stageFail, "probe_review_not_approved")
		return st, StepResult{Action: actionStop, Stop: "probe_review_not_approved", Reason: "the light reviewer probe " + out.TaskID + " did not approve"}
	}
	st.Complete = true
	p.setStage("light_reviewer_evidence", stagePass, "")
	return st, StepResult{Action: actionComplete}
}

// gate is dev-next's light probe gate: both initial pushes before any
// exposure, and the prepared candidate on the light workspace's main
// branch before the light review is exposed (candidate_unpublished, exit
// 0, until then). gated reports an answer to return instead.
func (p *devProbe) gate(sl *devSlot) (controlResponse, bool) {
	d, l := p.d, p.light
	if !l.seedVerified {
		ok, err := d.verifyMain(l.ws, l.ws.SeedCommit, "")
		if err != nil {
			d.fail(err)
			return devCode(CodeClosing), true
		}
		if !ok {
			return devCode(CodeSeedUnpublished), true
		}
		l.seedVerified = true
		l.pushes[1].VerifiedSeq = d.s.ev.event(Event{Type: EvSeedCreated, Code: "light_seed_pushed"}).Seq
	}
	if sl != nil && sl.plan.Ordinal == probeLightReviewOrdinal && !l.candVerified {
		ok, err := d.verifyMain(l.ws, l.cand.String(), l.ws.SeedCommit)
		if err != nil {
			d.fail(err)
			return devCode(CodeClosing), true
		}
		if !ok {
			return controlResponse{OK: true, Code: CodeCandidateUnpublished, Summary: l.cand.String()}, true
		}
		l.candVerified = true
		l.pushes[2].VerifiedSeq = d.s.ev.event(Event{Type: EvSeedCreated, Code: "light_candidate_pushed"}).Seq
	}
	return controlResponse{}, false
}

// planWorkspace is a plan's workspace: the light probe's light workspace
// for its triage and light review, otherwise the attempt's.
func (d *devSupervisor) planWorkspace(plan *TaskPlan) WorkspaceRecord {
	if p := d.probe; p != nil && p.light != nil && plan.Ordinal >= probeTriageOrdinal {
		return p.light.ws
	}
	return d.s.workspace
}

// lightSeedTree is the light seed's tree hash (the light attempt's seed,
// or the light probe's).
func (d *devSupervisor) lightSeedTree() string {
	if p := d.probe; p != nil && p.light != nil {
		return p.light.ws.SeedTree
	}
	return d.s.seed.Tree.String()
}

// lightSeedCLI is the supervisor's seed CLI corroboration of a light
// review: the light attempt's baseline; the probe's committed fixture,
// whose exact defect its provenance proves.
func (d *devSupervisor) lightSeedCLI() bool {
	if d.light != nil {
		return d.light.st.baseline.rec.Defect.Exact
	}
	return d.probe != nil && d.probe.light != nil
}

// verifyMain reports whether ws's main branch holds want (false while it
// still holds prev, "" none); any other commit or instance stops.
func (d *devSupervisor) verifyMain(ws WorkspaceRecord, want, prev string) (bool, error) {
	page := client.StatusPage{Limit: contract.MaxWorkspaceLimit}
	for {
		ctx, cancel := d.s.opCtx()
		r, err := d.s.plane.WorkspaceStatus(ctx, ws.Name, page)
		cancel()
		if err != nil {
			return false, failure("seed_unverified", err)
		}
		if r.Instance != ws.Instance {
			return false, failure("seed_unverified", errors.New("the workspace instance changed"))
		}
		for _, ref := range r.Refs {
			if ref.Name != contract.DefaultBranchRef {
				continue
			}
			switch ref.Commit {
			case want:
				return true, nil
			case prev:
				return false, nil
			}
			return false, failure("seed_mismatch", fmt.Errorf("the workspace %s's main branch is %s, not %s", ws.Name, contract.SafeText(ref.Commit, 64), want))
		}
		if r.NextAfter == nil {
			return false, nil
		}
		page = client.StatusPage{After: *r.NextAfter, Instance: r.Instance, Generation: r.Generation, Limit: contract.MaxWorkspaceLimit}
	}
}

// lightRecord is the qualification's light record (nil: full scope).
func (p *devProbe) lightRecord() *DevProbeLight {
	l := p.light
	if l == nil {
		return nil
	}
	rec := &DevProbeLight{Workspace: l.ws, Seed: l.ws.SeedCommit, SeedTree: l.ws.SeedTree, TriageTask: l.triageTask, TriageCommit: l.triage,
		ReviewTask: l.reviewTask, ReviewCommit: l.reviewC, Fixture: lightScenarioRecord("", "", l.files).Files, Pushes: append([]DevProbePush{}, l.pushes...)}
	if p.d.seedVerified {
		rec.Pushes[0].VerifiedSeq = p.d.seedPushSeq
	}
	if !l.cand.IsZero() {
		rec.Candidate, rec.CandidateTree = l.cand.String(), l.candTree.String()
	}
	return rec
}

// writeProbeLightRepository writes the light probe's two repositories:
// the full fixture's (seed, candidate, review) and the light workspace's
// (seed, triage, candidate, review as reached), each with HEAD at its seed.
func (d *devSupervisor) writeProbeLightRepository() {
	p := d.probe
	l := p.light
	full := map[string]plumbing.Hash{"refs/heads/seed": p.seed, "refs/heads/candidate": p.cand}
	if p.reviewC != "" {
		full["refs/heads/review"] = plumbing.NewHash(p.reviewC)
	}
	if !d.writeRefRepo(probeFullRepoDir, full) {
		return
	}
	light := map[string]plumbing.Hash{"refs/heads/seed": l.seed.Commit}
	for ref, c := range map[string]string{"refs/heads/triage": l.triage, "refs/heads/review": l.reviewC} {
		if c != "" {
			light[ref] = plumbing.NewHash(c)
		}
	}
	if !l.cand.IsZero() {
		light["refs/heads/candidate"] = l.cand
	}
	d.writeRefRepo(probeLightRepoDir, light)
}

// writeRefRepo writes a bare evidence repository at rel holding refs'
// object closure, HEAD at refs/heads/seed.
func (d *devSupervisor) writeRefRepo(rel string, refs map[string]plumbing.Hash) bool {
	r, err := git.PlainInit(filepath.Join(d.s.ev.root, filepath.FromSlash(rel)), true)
	if err != nil {
		d.stop("evidence_write_failed", err.Error())
		return false
	}
	var roots []plumbing.Hash
	for _, name := range sortedRefNames(refs) {
		roots = append(roots, refs[name])
	}
	if err := copyObjects(d.s.collect.Storer, r.Storer, roots); err != nil {
		d.stop("evidence_write_failed", err.Error())
		return false
	}
	for name, h := range refs {
		if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), h)); err != nil {
			d.stop("evidence_write_failed", err.Error())
			return false
		}
	}
	if err := r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/seed")); err != nil {
		d.stop("evidence_write_failed", err.Error())
		return false
	}
	return true
}

// sortedRefNames is refs' names in order.
func sortedRefNames(refs map[string]plumbing.Hash) []string {
	out := make([]string, 0, len(refs))
	for k := range refs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
