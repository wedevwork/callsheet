package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The light attempt's supervisor steps (design 12a-dev-flow-light,
// Scenario and intake; FP-1). A light attempt reuses the development
// supervisor end to end: the same deployment, preflights, control socket,
// loop, wait consumption, publication inspection, rejection retention,
// cleanup and checked evidence. Only the scenario steps differ: the
// committed light seed and its supervisor baseline (the seed must exhibit
// exactly the whitespace defect), the owner's light intake before any paid
// call, the light transition policy, the executing light review and the
// light final validation.

// LightBounds is the light attempt's cost and time disclosure.
const LightBounds = `Cost and time bounds of the light development flow (policy, not measurements):
  - five paid authentication preflight tasks (one per role pair), each at most 2m;
  - three paid workflow tasks on the happy path (triage, the defect coder, the light review);
    at most two additional fix/review pairs and at most two recovery dispatches: at most nine
    workflow tasks, each at most 10m from admission through publication: at most 14 paid
    worker calls including the five preflights;
  - one headless Claude Code coordinator phase after the owner's light intake, with
    vendor-dependent turns; there is no extra paid coordinator preflight;
  - the intake window at most 20m; the whole attempt at most 90m from preflight start (the
    first owner-environment fingerprint and the intake included); the final validation shares
    one 60s budget; then at most 2m of cleanup;
  - no automatic model retries and no escalation design or implementation; no token-price
    ceiling is claimed.
`

// lightScope is the light intake's exact authorized scope identifier.
const lightScope = "whitespace-restoration/v1"

// lightScopeText is the authorized output scope shown at intake.
const lightScopeText = "investigation by the frozen triage selection (rca.md and a qualified fix.md), restoration of the whitespace behavior by the frozen coder in defect mode (main.go and main_test.go only), bounded fix and re-triage loops and the executing light review; never a design or an escalation implementation"

// lightDefectLine and lightKnownGoodLine are the seeded defect's source
// line and its known-good original.
const (
	lightDefectLine    = `	if strings.TrimSpace(*name) == "" {`
	lightKnownGoodLine = `	if *name == "" {`
)

// DevLightScenario is setup/light-scenario.json.
type DevLightScenario struct {
	Schema        string           `json:"schema"`
	RunID         string           `json:"run_id"`
	FixtureDir    string           `json:"fixture_dir"`
	Revision      string           `json:"revision"`
	Files         []DevProbeSource `json:"files"`
	SourceSHA256  string           `json:"source_sha256"`
	TestSHA256    string           `json:"test_sha256"`
	RequestSHA256 string           `json:"request_sha256"`
	Defect        LightDefect      `json:"defect"`
	SeedCases     []string         `json:"seed_cases"`
	RequiredCases []string         `json:"required_cases"`
	Scope         string           `json:"scope"`
}

// LightDefect is the exact seeded defect.
type LightDefect struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Seeded    string `json:"seeded"`
	KnownGood string `json:"known_good"`
	Case      string `json:"case"`
	Stderr    string `json:"stderr"`
}

// lightDefectOf locates the seeded defect line in main.go (0: absent or
// not unique).
func lightDefectOf(main []byte) LightDefect {
	d := LightDefect{Path: "main.go", Seeded: lightDefectLine, KnownGood: lightKnownGoodLine, Case: lightWhitespaceCase, Stderr: lightWhitespaceStderr}
	lines := strings.Split(string(main), "\n")
	for i, l := range lines {
		if l == lightDefectLine {
			if d.Line != 0 {
				return LightDefect{Path: "main.go", Seeded: lightDefectLine, KnownGood: lightKnownGoodLine, Case: lightWhitespaceCase, Stderr: lightWhitespaceStderr}
			}
			d.Line = i + 1
		}
	}
	return d
}

// lightScenarioRecord is the light scenario record of a fixture.
func lightScenarioRecord(run, revision string, files map[string][]byte) DevLightScenario {
	rec := DevLightScenario{Schema: devKind("light-scenario"), RunID: run, FixtureDir: devLightFixtureDir, Revision: revision, Files: []DevProbeSource{},
		SourceSHA256: sha256Hex(files["main.go"]), TestSHA256: sha256Hex(files["main_test.go"]), RequestSHA256: sha256Hex(files["FEATURE.md"]),
		Defect: lightDefectOf(files["main.go"]), SeedCases: lightSeedCases(), RequiredCases: []string{}, Scope: lightScope}
	for _, c := range FeatureCases {
		rec.RequiredCases = append(rec.RequiredCases, c.ID)
	}
	for _, n := range lightFixtureFiles {
		origin := devLightFixtureDir + "/" + n
		if n == "go.mod" {
			origin = "generated: module example.com/greeting with the checkout's go directive"
		}
		logical := lightFixtureCaptureDir + "/" + n
		rec.Files = append(rec.Files, DevProbeSource{Path: logical, Bytes: len(files[n]), SHA256: sha256Hex(files[n]), Origin: origin,
			StoragePath: capturedPath(logical)})
	}
	return rec
}

// DevIntakeDecision is intake/decision.json's data: the owner's typed
// light intake decision, bound to the prepared seed, request and frozen
// selection (kind light, no, eof or expired).
type DevIntakeDecision struct {
	Scenario        string `json:"scenario"`
	SeedCommit      string `json:"seed_commit"`
	SeedTree        string `json:"seed_tree"`
	RequestSHA256   string `json:"request_sha256"`
	SelectionSHA256 string `json:"selection_sha256"`
	Scope           string `json:"scope"`
	Kind            string `json:"kind"`
	Response        string `json:"response"`
	PromptSeq       int    `json:"prompt_seq"`
	DecisionSeq     int    `json:"decision_seq"`
	Deadline        string `json:"deadline"`
	DecisionTime    string `json:"decision_time"`
}

// DevIntakeReceipt is intake/receipt.json's data, written only for a
// valid light decision, before any paid call.
type DevIntakeReceipt struct {
	Scenario        string `json:"scenario"`
	SeedCommit      string `json:"seed_commit"`
	SeedTree        string `json:"seed_tree"`
	RequestSHA256   string `json:"request_sha256"`
	SelectionSHA256 string `json:"selection_sha256"`
	Scope           string `json:"scope"`
	PromptSeq       int    `json:"prompt_seq"`
	DecisionSeq     int    `json:"decision_seq"`
	ReceiptSeq      int    `json:"receipt_seq"`
	DecisionTime    string `json:"decision_time"`
}

// Intake record paths and decision kinds.
const (
	lightDecisionFile = "intake/decision.json"
	lightReceiptFile  = "intake/receipt.json"
	intakeLight       = "light"
	intakeNo          = "no"
	intakeEOF         = "eof"
	intakeExpired     = "expired"
	maxIntakeResponse = 256
)

// devLightState is the light attempt's scenario state (the devLight
// pointer of devSupervisor).
type devLightState struct {
	files        map[string][]byte
	scenario     DevLightScenario
	baseline     lightBaselineRun
	receipt      *DevIntakeReceipt
	receiptBytes []byte
}

// runDevLightLive builds the world and runs one checked light attempt.
func runDevLightLive(h Host, a runArgs) int {
	w, release, err := h.NewWorld(h)
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e run-dev-light: %v\n", err)
		return 1
	}
	defer release()
	return newDevLightSupervisor(h, w, a).run()
}

// newDevSupervisor is a fresh development supervisor over w. A validated
// --cursor (the command bound the Cursor selection and executable before
// this world existed) allocates the run's own Cursor state and its
// immutable four-sidecar list (design 12a-3-cursor-worker, slice 3b): a
// standalone attempt, a probe and each exercise child alike.
func newDevSupervisor(h Host, w *World, a runArgs) *devSupervisor {
	s := &supervisor{stdout: h.Stdout, stderr: h.Stderr, goos: h.GOOS, goarch: h.GOARCH, w: w, args: a, ctx: w.Context,
		known: map[string]string{}, stoppedSet: map[*child]bool{}, reqs: make(chan ctrlMsg), loopDone: make(chan struct{}), envPolicy: envPolicyDev}
	d := &devSupervisor{s: s, manuals: map[string][2]string{}, receiptBytes: map[int][]byte{}, trees: map[string]map[string]TreeFile{},
		finals: map[string]string{}, sweeps: sweepRecorder{clock: w.Clock}}
	if a.cursor != "" {
		d.cursor = newCursorRun(w, a.cursor)
		s.sidecars = slices.Clone(cursorSidecars)
	}
	return d
}

// newDevLightSupervisor is a light attempt's supervisor (observed by the
// host's test observer).
func newDevLightSupervisor(h Host, w *World, a runArgs) *devSupervisor {
	d := newDevSupervisor(h, w, a)
	d.light = &devLight{d: d}
	if h.observeDev != nil {
		h.observeDev(d)
	}
	return d
}

// lightStart is the light attempt's setup to its single coordinator phase:
// the light seed and its baseline, then the owner's light intake, and only
// then deployment and the paid preflights.
func (d *devSupervisor) lightStart() error {
	l := d.light
	steps := d.cursorSteps([]func() error{d.s.printPermissionTemplate, d.hashExecutable, d.s.protectedRoots, d.s.baselineSnapshot, d.loadSelection,
		d.versionProbes, d.s.createRuntime, d.s.renderPermissions, d.extraDirs, l.seed, d.activateTerminal, l.intake, d.deployDev, d.preflightDev,
		l.workspace, d.openControl, d.initialPlan, d.launchPhase})
	for _, f := range steps {
		switch {
		case d.s.ctx.Err() != nil:
			return failure("interrupted", d.s.ctx.Err())
		case d.expired(d.attemptDeadline()):
			return failure("attempt_timeout", errors.New("the attempt reached its 90m bound"))
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

// seed prepares the immutable light seed from the committed fixture (the
// generated go.mod with the checkout's go directive), captures it inertly,
// records the scenario and runs the supervisor baseline: a seed not
// exhibiting exactly the whitespace defect stops as light_seed_invalid.
func (l *devLight) seed() error {
	d := l.d
	files, tree, err := d.readLightFixture()
	if err != nil {
		return err
	}
	st := &devLightState{files: files}
	l.st = st
	if err := d.captureLightFixture(files); err != nil {
		return err
	}
	seed, err := createSeed(filepath.Join(d.s.runtime, "seed"), st.files)
	if err != nil {
		return failure("seed_failed", err)
	}
	d.s.seed = seed
	if err := copyObjects(seed.repo.Storer, d.s.collect.Storer, []plumbing.Hash{seed.Commit}); err != nil {
		return failure("seed_failed", err)
	}
	d.s.ev.event(Event{Type: EvSeedCreated})
	d.trees[seed.Commit.String()] = tree
	st.scenario = lightScenarioRecord(d.s.runID, d.manifest.Revision, st.files)
	if !d.writeEvidenceJSON("setup/light-scenario.json", st.scenario) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	start := d.s.w.Clock.Now()
	opts := lightRunOptions{run: d.s.command, clock: d.s.w.Clock, goBin: d.s.goBin, pathEnv: d.s.pathEnv, dir: filepath.Join(d.s.runtime, "light-baseline"),
		commit: seed.Commit.String(), tree: seed.Tree.String(), deadline: minTime(start.Add(lightBaselineBound), d.attemptDeadline()),
		mark: func(typ, name string) int { return d.s.ev.event(Event{Type: typ, Code: name}).Seq }}
	st.baseline = runLightBaseline(opts, st.files)
	st.baseline.rec.RunID = d.s.runID
	if !d.writeLightBaseline(st.baseline) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	d.s.ev.event(Event{Type: EvBaselineTests, Code: "light_baseline"})
	if !st.baseline.rec.Valid {
		return failure(CodeLightSeedInvalid, fmt.Errorf("the light seed does not exhibit exactly the whitespace defect (%s)", contract.SafeText(st.baseline.rec.Error, 200)))
	}
	return nil
}

// readLightFixture reads the committed light fixture with its generated
// go.mod (the checkout's go directive) within the fixture budgets.
func (d *devSupervisor) readLightFixture() (map[string][]byte, map[string]TreeFile, error) {
	files := map[string][]byte{}
	for _, n := range []string{"FEATURE.md", "main.go", "main_test.go"} {
		b, err := readLiteral(filepath.Join(d.s.checkout, filepath.FromSlash(devLightFixtureDir), n), maxArtifactBytes)
		if err != nil {
			return nil, nil, failure("fixture_missing", fmt.Errorf("light fixture %s: %w", n, err))
		}
		files[n] = b
	}
	gomod, err := readBounded(filepath.Join(d.s.checkout, "go.mod"), 64<<10)
	if err != nil {
		return nil, nil, failure("checkout_unreadable", err)
	}
	ver, err := goDirective(gomod)
	if err != nil {
		return nil, nil, failure("checkout_unreadable", err)
	}
	files["go.mod"] = []byte("module example.com/greeting\n\ngo " + ver + "\n")
	tree := map[string]TreeFile{}
	for name, b := range files {
		tree[name] = TreeFile{Path: name, Mode: filemode.Regular, Content: b}
	}
	if err := budgets(tree); err != nil {
		return nil, nil, failure(CodeLightSeedInvalid, err)
	}
	return files, tree, nil
}

// captureLightFixture writes the four light fixture files as inert
// captures, refusing a case-fold collision before any write.
func (d *devSupervisor) captureLightFixture(files map[string][]byte) error {
	var physical []string
	for _, n := range lightFixtureFiles {
		physical = append(physical, capturedPath(lightFixtureCaptureDir+"/"+n))
	}
	if err := checkCaptureFold(physical); err != nil {
		return failure(CodeLightSeedInvalid, err)
	}
	for _, n := range lightFixtureFiles {
		if !d.writeEvidence(capturedPath(lightFixtureCaptureDir+"/"+n), files[n]) {
			return failure(d.stopCode, errors.New(d.stopReason))
		}
	}
	return nil
}

// writeLightBaseline writes the baseline record and its retained streams.
func (d *devSupervisor) writeLightBaseline(b lightBaselineRun) bool {
	ok := d.writeEvidenceJSON("validation/light-baseline.json", b.rec)
	for rel, data := range map[string][]byte{"validation/light/baseline-tests.jsonl": b.jsonS, "validation/light/baseline-stdout.txt": b.suiteOut,
		"validation/light/baseline-stderr.txt": b.suiteErr} {
		ok = ok && d.writeEvidence(rel, []byte(toValidUTF8(data)))
	}
	if b.profile != nil {
		ok = ok && d.writeEvidence("validation/light/baseline-coverage.out", []byte(toValidUTF8(b.profile)))
	}
	return ok
}

// intake asks the owner for the light intake: the attempt ID, the seed, the
// request and selection digests, the authorized scope and the disclosure,
// then exactly "light <attempt-id>" or "no <attempt-id>", read only as a
// fresh line after the prompt within min(20m, the remaining attempt).
// EOF, expiry, a wrong ID, buffered input and model text never authorize;
// only a valid light decision persists the receipt, before any paid call.
func (l *devLight) intake() error {
	d := l.d
	st := l.st
	run := d.s.runID
	seed := d.s.seed
	request := sha256Hex(st.files["FEATURE.md"])
	fmt.Fprint(d.s.stdout, LightBounds)
	d.s.say("LIGHT INTAKE NEEDED for attempt %s (within 20m): seed commit %s, tree %s; FEATURE.md sha256 %s; role selection sha256 %s", run, seed.Commit, seed.Tree,
		request, d.manifest.SelectionSHA256)
	d.s.say("authorized scope %s: %s", lightScope, lightScopeText)
	ev := d.s.ev.event(Event{Type: EvAwaitingOwner, Hop: "intake", Code: intakeLight})
	at, ok := parseTime(ev.Time)
	if !ok {
		at = d.s.w.Clock.Now()
	}
	deadline := minTime(at.Add(OwnerDecisionBound), d.attemptDeadline())
	var epoch uint64
	if d.term != nil {
		epoch = d.term.OpenWindow()
	}
	d.discardPending()
	fmt.Fprintf(d.s.stdout, "Authorize light investigation and restoration for attempt %s? Type exactly \"light %s\" or \"no %s\".\n", run, run, run)
	if l.prompted != nil {
		l.prompted()
	}
	dec := DevIntakeDecision{Scenario: scenarioLight, SeedCommit: seed.Commit.String(), SeedTree: seed.Tree.String(), RequestSHA256: request,
		SelectionSHA256: d.manifest.SelectionSHA256, Scope: lightScope, PromptSeq: ev.Seq, Deadline: formatTime(deadline)}
	kind, response := l.awaitIntake(epoch, deadline)
	dec.Kind, dec.Response = kind, response
	dev := d.s.ev.event(Event{Type: EvOwnerDecision, Hop: "intake", Code: kind})
	dec.DecisionSeq, dec.DecisionTime = dev.Seq, dev.Time
	if !d.writeEvidenceJSON(lightDecisionFile, envelope("intake-decision", run, dec)) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	switch kind {
	case intakeNo:
		return failure("owner_no", errors.New("the owner declined the light intake"))
	case intakeEOF:
		return failure("owner_eof", errors.New("the operator terminal closed before the light intake"))
	case intakeExpired:
		return failure("owner_expired", errors.New("no light intake decision within its window"))
	}
	rev := d.s.ev.event(Event{Type: EvReceiptCreated, Hop: "intake"})
	r := DevIntakeReceipt{Scenario: scenarioLight, SeedCommit: dec.SeedCommit, SeedTree: dec.SeedTree, RequestSHA256: request,
		SelectionSHA256: dec.SelectionSHA256, Scope: lightScope, PromptSeq: dec.PromptSeq, DecisionSeq: dec.DecisionSeq, ReceiptSeq: rev.Seq,
		DecisionTime: dec.DecisionTime}
	if !(r.PromptSeq > 0 && r.DecisionSeq > r.PromptSeq && r.ReceiptSeq > r.DecisionSeq) {
		return failure("receipt_order", fmt.Errorf("the intake events are not strictly ordered: %d %d %d", r.PromptSeq, r.DecisionSeq, r.ReceiptSeq))
	}
	b, err := encodeJSON(r)
	if err != nil {
		return failure("evidence_write_failed", err)
	}
	if err := writePrivateReadOnly(filepath.Join(d.s.runtime, "intake", "receipt.json"), b); err != nil {
		return failure("runtime_unavailable", err)
	}
	if !d.writeEvidenceJSON(lightReceiptFile, envelope("intake-receipt", run, r)) {
		return failure(d.stopCode, errors.New(d.stopReason))
	}
	st.receipt, st.receiptBytes = &r, b
	d.s.say("light intake recorded for attempt %s; the paid preflights start now", run)
	return nil
}

// awaitIntake reads fresh terminal lines until the light decision, EOF or
// the deadline (now >= deadline is expired), on the injected clock.
func (l *devLight) awaitIntake(epoch uint64, deadline time.Time) (kind, response string) {
	d := l.d
	run := d.s.runID
	for {
		if d.expired(deadline) {
			return intakeExpired, ""
		}
		if d.lines == nil || d.termEOF {
			return intakeEOF, ""
		}
		t, stop := d.s.w.Clock.NewTimer(min(d.s.w.Poll, max(time.Millisecond, deadline.Sub(d.s.w.Clock.Now()))))
		select {
		case in, ok := <-d.lines:
			stop()
			if !ok {
				d.lines, d.termEOF = nil, true
				return intakeEOF, ""
			}
			if d.expired(deadline) {
				return intakeExpired, ""
			}
			if in.epoch != epoch {
				d.ignoredLines++
				d.s.say("input read before the light intake prompt is discarded; the window's deadline is unchanged")
				continue
			}
			line := strings.TrimSuffix(in.line, "\r")
			switch line {
			case "light " + run:
				return intakeLight, line
			case "no " + run:
				return intakeNo, line
			}
			d.s.say("not a light intake decision for attempt %s; the window's deadline is unchanged", run)
		case <-t:
		case <-d.s.ctx.Done():
			stop()
			return intakeEOF, ""
		}
	}
}

// workspace creates the light attempt's workspace the coordinator pushes
// the seed into (after the paid preflights passed).
func (l *devLight) workspace() error {
	d := l.d
	suffix, err := d.s.randomHex(4)
	if err != nil {
		return failure("internal_error", err)
	}
	ctx, cancel := d.s.opCtx()
	defer cancel()
	ws, err := d.s.plane.CreateWorkspace(ctx, "e2e-light-"+suffix)
	if err != nil {
		return failure("workspace_create_failed", err)
	}
	d.s.workspace = WorkspaceRecord{Name: ws.Name, Instance: ws.Instance, SeedCommit: d.s.seed.Commit.String(), SeedTree: d.s.seed.Tree.String()}
	return nil
}

// checkIntake verifies the active light intake receipt for a coder plan
// or admission: present, bound to this run's seed, request and selection,
// and its read-only runtime file unchanged.
func (l *devLight) checkIntake() error {
	d := l.d
	st := l.st
	r := st.receipt
	switch {
	case r == nil || r.ReceiptSeq == 0:
		return errors.New("no light intake receipt")
	case r.SeedCommit != d.state.InitialCommit || r.SelectionSHA256 != d.manifest.SelectionSHA256 || r.Scope != lightScope:
		return errors.New("the intake receipt is not bound to this attempt's seed and selection")
	}
	b, err := readBounded(filepath.Join(d.s.runtime, "intake", "receipt.json"), 64<<10)
	if err != nil || !bytes.Equal(b, st.receiptBytes) {
		return errors.New("the read-only intake receipt changed")
	}
	return nil
}

// runLightFinalValidation validates the light final candidate within the
// shared 60s budget and reports whether it passed (a failure stops the
// attempt; nothing is retried).
func (d *devSupervisor) runLightFinalValidation() bool {
	st := d.light.st
	cand := d.state.CandidateCommit
	tree, ok := d.trees[cand]
	treeHash := d.candidateTree(cand)
	seedTree := d.trees[d.state.InitialCommit]
	if !ok || treeHash == "" || seedTree == nil {
		d.stop("final_tests_failed", "the final candidate is not an admitted publication")
		return false
	}
	if err := budgets(tree); err != nil {
		d.stop("final_tests_failed", err.Error())
		return false
	}
	files, seed := map[string][]byte{}, map[string][]byte{}
	for n, f := range tree {
		files[n] = f.Content
	}
	for n, f := range seedTree {
		seed[n] = f.Content
	}
	start := d.s.w.Clock.Now()
	opts := lightRunOptions{run: d.s.command, clock: d.s.w.Clock, goBin: d.s.goBin, pathEnv: d.s.pathEnv, dir: filepath.Join(d.s.runtime, "final-light"),
		commit: cand, tree: treeHash, deadline: minTime(start.Add(FinalTestBound), d.attemptDeadline()),
		mark: func(typ, name string) int { return d.s.ev.event(Event{Type: typ, Code: name}).Seq }}
	v := runLightFinal(opts, d.state.InitialCommit, d.s.seed.Tree.String(), seed, files, st.baseline.rec)
	d.writeLightFinal(v)
	d.s.ev.event(Event{Type: EvFinalTests, Hop: workflowLabel(len(d.state.Tasks)), Task: d.state.ReviewTask})
	if d.halted() {
		return false
	}
	casesOK := cleanRun(v.cases.Build) && v.cases.Build.ExitCode == 0 && len(v.cases.Cases) == len(FeatureCases)
	for _, c := range v.cases.Cases {
		casesOK = casesOK && c.Pass
	}
	switch {
	case !v.reg.rec.Pass:
		d.stop(v.reg.rec.Code, "the candidate's regression is not red on the seed and green on the candidate")
	case v.err != nil || v.suite.ExitCode != 0 || v.suite.TimedOut || v.suite.Toolchain == "":
		d.stop("final_tests_failed", fmt.Sprintf("go test exited %d: %v", v.suite.ExitCode, v.err))
	case !casesOK:
		d.stop(CodeCaseFailed, "a CLI feature case did not produce its required result")
	case len(v.reqs.MissingTests) > 0 || len(v.reqs.FailedTests) > 0 || !v.reqs.RunSignature || !v.reqs.TableTests || !v.reqs.TestCallsRun:
		d.stop(CodeTestMapping, "a required TestRun/<case-id> subtest or the run/table structure is missing or failing")
	case v.coverage.Comparison.Code != "":
		d.stop(v.coverage.Comparison.Code, "the candidate's whole-production coverage fraction is not nondecreasing from the seed's")
	default:
		d.s.say("final light validation of candidate %s passed (coverage %s from %s)", cand, coverageFraction(*v.coverage.Cand),
			coverageFraction(*v.coverage.Baseline))
		return true
	}
	return false
}

// writeLightFinal writes every light final validation record that exists.
func (d *devSupervisor) writeLightFinal(v lightFinalRun) {
	run := d.s.runID
	v.reg.rec.RunID, v.coverage.RunID = run, run
	var errs []error
	w := func(rel string, data []byte) { errs = append(errs, d.s.ev.writeFile(rel, []byte(toValidUTF8(data)))) }
	j := func(rel string, x any) { errs = append(errs, d.s.ev.writeJSON(rel, x)) }
	j("validation/light-regression.json", v.reg.rec)
	w("validation/light/red-tests.jsonl", v.reg.redOut)
	w("validation/light/red-stderr.txt", v.reg.redErr)
	w("validation/light/green-tests.jsonl", v.reg.greenOut)
	w("validation/light/green-stderr.txt", v.reg.greenErr)
	j("validation/final-test.json", v.suite)
	w("validation/final-stdout.txt", v.suiteOut)
	w("validation/final-stderr.txt", v.suiteErr)
	j("validation/cases.json", v.cases)
	w(buildStdoutFile, v.buildOut)
	w(buildStderrFile, v.buildErr)
	j("validation/requirements.json", v.reqs)
	if v.profile != nil {
		w("validation/coverage.out", v.profile)
	}
	w("validation/coverage-stdout.txt", v.coverOut)
	w("validation/coverage-stderr.txt", v.coverErr)
	j("validation/light-coverage.json", v.coverage)
	if err := errors.Join(errs...); err != nil {
		d.stop("evidence_write_failed", err.Error())
	}
}
