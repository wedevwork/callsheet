package reale2e

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The light attempt's offline checks (design 12a-dev-flow-light, Evidence
// protocol; FP-6). The shared checker replays the light policy from the
// seed and the owner's intake, re-admits every publication (a qualified
// brief's root-cause reference resolved in the seed tree), re-derives every
// light review's evidence and recomputes the seed baseline, the red/green
// regression, the final suite, the ten cases and the coverage comparison
// from the captured streams and objects. No subprocess runs.

// devLightCriteria are the light report's criteria, in report order: the
// shared criteria without the design owner receipts, plus light_intake,
// triage_brief and regression_red_green; coverage is the light
// comparison.
var devLightCriteria = []string{"manifest", "provenance", "roles", "preflight", "coordinator_setup", "light_intake", "admissions", "transitions", "waits",
	"publications", "triage_brief", "reviewer_execution", "baseline", "regression_red_green", "final_tests", "coverage", "feature_oracle", "cleanup",
	"owner_environment", "deadlines", "cost"}

// Light report scope.
const devLightScope = "light-flow-mechanics-and-deterministic-results"

// isLight reports a light bundle.
func (c *devChecker) isLight() bool { return c.b.Scenario == scenarioLight }

// criteria are the bundle's report criteria.
func (c *devChecker) criteria() []string {
	if c.isLight() {
		return devLightCriteria
	}
	return devCriteria
}

// scope is the bundle's report scope.
func (c *devChecker) scope() string {
	if c.isLight() {
		return devLightScope
	}
	return devScope
}

// lightClaim states what a light result demonstrates.
func lightClaim(result, label string) string {
	switch {
	case result != ResultPass:
		return "not demonstrated"
	case label == FlowDefault:
		return "the default light development flow's recorded intake, mechanics, publications, reviewer execution, red/green regression, ten cases and nondecreasing coverage passed on this installation; no model judgment is claimed"
	}
	return "an override flow's recorded light mechanics passed; the owner's default pairs are not demonstrated"
}

// lightFixtureFrom is the captured light fixture (nil entries when absent).
func (c *devChecker) lightFixtureFrom() map[string][]byte {
	out := map[string][]byte{}
	for _, n := range lightFixtureFiles {
		if b, ok := c.b.captured(lightFixtureCaptureDir + "/" + n); ok {
			out[n] = b
		}
	}
	return out
}

// checkLightSeed requires the light scenario record to be the captured
// fixture's own provenance (the committed fixture's digests, the exact
// seeded defect line and the case sets), the generated go.mod with the
// recorded Go directive, and the seed tree to be exactly those four files.
func (c *devChecker) checkLightSeed(seed string) {
	rb := c.newRow("setup/seed", "publications",
		"The seed is exactly the captured light fixture (FEATURE.md, main.go and main_test.go from the committed fixture and the generated go.mod), recorded by setup/light-scenario.json with its seeded defect line and case sets", groupSetup)
	defer rb.done()
	files := c.lightFixtureFrom()
	rec := doc[DevLightScenario](c.b, "setup/light-scenario.json")
	if rec == nil || len(files) != len(lightFixtureFiles) {
		rb.miss("light_scenario_missing", c.reached(typeIs(EvSeedCreated)))
		return
	}
	rb.ref("setup/light-scenario.json", nil, "", "")
	for _, n := range lightFixtureFiles {
		rb.ref(capturedPath(lightFixtureCaptureDir+"/"+n), nil, "", seed)
	}
	if want := lightScenarioRecord(c.runID, c.m.Revision, files); !bytes.Equal(mustJSON(want), mustJSON(*rec)) {
		rb.bad("light_scenario_mismatch")
	}
	ver, err := goDirective(files["go.mod"])
	if err != nil || string(files["go.mod"]) != "module example.com/greeting\n\ngo "+ver+"\n" || rec.Defect.Line == 0 {
		rb.bad("seed_mismatch")
	}
	tree := c.trees[seed]
	if len(tree) != len(files) {
		rb.bad("seed_mismatch")
	}
	for n, b := range files {
		if f, ok := tree[n]; !ok || !bytes.Equal(f.Content, b) || f.Mode != filemode.Regular {
			rb.bad("seed_mismatch")
		}
	}
	if c.b.Repo == nil || c.b.Repo.refs["refs/heads/seed"].String() != seed || c.m.Workspace.SeedTree != "" && c.treeHash[seed] != c.m.Workspace.SeedTree {
		rb.bad("seed_mismatch")
	}
	rb.see("light seed %s, defect at main.go:%d", seed, rec.Defect.Line)
}

// lightSeedCommit is the light bundle's seed: the manifest's workspace
// seed, or the evidence repository's seed ref when the attempt stopped
// before its workspace existed.
func (c *devChecker) lightSeedCommit() string {
	if c.m.Workspace.SeedCommit != "" {
		return c.m.Workspace.SeedCommit
	}
	if c.b.Repo != nil {
		if h, ok := c.b.Repo.refs["refs/heads/seed"]; ok {
			return h.String()
		}
	}
	return ""
}

// intakeDigest is the intake receipt's digest as the envelopes name it
// ("" when there is none).
func (c *devChecker) intakeDigest() string {
	r := doc[devEnvelope[DevIntakeReceipt]](c.b, lightReceiptFile)
	if r == nil {
		return ""
	}
	b, err := encodeJSON(r.Data)
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}

// intakeCovers reports whether a valid light intake receipt authorizes a
// light coder ("" when it does).
func (c *devChecker) intakeCovers() string {
	r := doc[devEnvelope[DevIntakeReceipt]](c.b, lightReceiptFile)
	switch {
	case r == nil || r.Data.ReceiptSeq == 0:
		return "no light intake receipt"
	case r.Data.SeedCommit != c.lightSeedCommit() || r.Data.SelectionSHA256 != c.m.SelectionSHA256 || r.Data.Scope != lightScope:
		return "the intake receipt is not bound to this attempt"
	}
	return ""
}

// checkLightIntake judges the owner's light intake: one typed decision
// after a fresh prompt within min(20m, the attempt), bound to the seed,
// the request and the frozen selection; a light decision's receipt, its
// strictly ordered events, and both before any paid preflight or
// workflow admission; a refusal, EOF or expiry stopped the attempt with no
// paid call.
func (c *devChecker) checkLightIntake() {
	rb := c.newRow("intake/authorization", "light_intake",
		"Before any paid call, the owner typed exactly light <attempt-id> (or no <attempt-id>) as a fresh line after the intake prompt, within min(20m, the attempt); the decision and, for light only, the receipt bind the seed commit and tree, the FEATURE.md and selection digests and the scope whitespace-restoration/v1, with strictly ordered prompt, decision and receipt events",
		groupSetup)
	defer rb.done()
	prompt := c.seqOf(func(e Event) bool { return e.Type == EvAwaitingOwner && e.Hop == "intake" && e.Code == intakeLight })
	dec := doc[devEnvelope[DevIntakeDecision]](c.b, lightDecisionFile)
	rec := doc[devEnvelope[DevIntakeReceipt]](c.b, lightReceiptFile)
	rb.at(prompt)
	if dec == nil {
		rb.miss("intake_missing", prompt > 0)
		return
	}
	rb.ref(lightDecisionFile, nil, "", "")
	rb.ref(lightReceiptFile, nil, "", "")
	d := dec.Data
	seed := c.lightSeedCommit()
	fixture := c.lightFixtureFrom()
	run := c.runID
	want := DevIntakeDecision{Scenario: scenarioLight, SeedCommit: seed, SeedTree: c.treeHash[seed], RequestSHA256: sha256Hex(fixture["FEATURE.md"]),
		SelectionSHA256: c.m.SelectionSHA256, Scope: lightScope, Kind: d.Kind, Response: d.Response, PromptSeq: prompt, DecisionSeq: d.DecisionSeq,
		Deadline: d.Deadline, DecisionTime: d.DecisionTime}
	pev, dev := c.eventAt(prompt), c.eventAt(d.DecisionSeq)
	if pev != nil {
		at, _ := parseTime(pev.Time)
		want.Deadline = formatTime(minTime(at.Add(OwnerDecisionBound), c.attemptDeadline()))
	}
	if !bytes.Equal(mustJSON(d), mustJSON(want)) {
		rb.bad("intake_binding_mismatch")
	}
	switch {
	case dev == nil || dev.Type != EvOwnerDecision || dev.Hop != "intake" || dev.Code != d.Kind || dev.Time != d.DecisionTime:
		rb.bad("intake_event_mismatch")
	case prompt == 0 || d.DecisionSeq <= prompt:
		rb.bad("intake_out_of_order")
	}
	if dt, ok := parseTime(d.DecisionTime); ok && pev != nil {
		deadline, _ := parseTime(want.Deadline)
		if d.Kind != intakeExpired && !dt.Before(deadline) || d.Kind == intakeExpired && dt.Before(deadline) {
			rb.bad("intake_deadline_mismatch")
		}
	}
	firstPaid := c.seqOf(func(e Event) bool { return e.Type == EvTaskAdmitted && e.Code == "" })
	switch d.Kind {
	case intakeLight:
		if d.Response != "light "+run {
			rb.bad("intake_response_invalid")
		}
		rcpt := c.seqOf(func(e Event) bool { return e.Type == EvReceiptCreated && e.Hop == "intake" })
		if rec == nil {
			rb.miss("intake_receipt_missing", rcpt > 0)
			return
		}
		r := rec.Data
		wantR := DevIntakeReceipt{Scenario: scenarioLight, SeedCommit: seed, SeedTree: c.treeHash[seed], RequestSHA256: want.RequestSHA256,
			SelectionSHA256: c.m.SelectionSHA256, Scope: lightScope, PromptSeq: prompt, DecisionSeq: d.DecisionSeq, ReceiptSeq: rcpt,
			DecisionTime: d.DecisionTime}
		if r != wantR {
			rb.bad("intake_receipt_mismatch")
		}
		if rcpt <= d.DecisionSeq || firstPaid > 0 && firstPaid < rcpt {
			rb.bad("intake_out_of_order")
		}
		rb.see("light intake at seq %d, receipt at seq %d, before the first paid admission (seq %d)", d.DecisionSeq, rcpt, firstPaid)
	case intakeNo, intakeEOF, intakeExpired:
		wantStop := map[string]string{intakeNo: "owner_no", intakeEOF: "owner_eof", intakeExpired: "owner_expired"}[d.Kind]
		if d.Kind == intakeNo && d.Response != "no "+run || d.Kind != intakeNo && d.Response != "" {
			rb.bad("intake_response_invalid")
		}
		if rec != nil {
			rb.bad("intake_receipt_unauthorized")
		}
		if c.stop == nil || c.stop.code != wantStop || firstPaid > 0 {
			rb.bad("intake_stop_mismatch")
		}
		rb.see("intake %s: stopped with no paid call", d.Kind)
	default:
		rb.bad("intake_binding_mismatch")
	}
}

// triageBriefRow is a qualified light triage's mechanical brief row: its
// one resolving root-cause reference, its confidence and its design field
// (the content quality is a judgment row).
func (c *devChecker) triageBriefRow(n int, p TaskPlan, taskID, commit, marker string, result map[string]TreeFile, ev taskEvents) {
	rb := c.newRow(fmt.Sprintf("task/%02d/triage-brief", n), "triage_brief",
		fmt.Sprintf("Triage task %d's publication carries a nonempty rca.md ending in its marker and, when qualified, a newly published nonempty fix.md with one Confidence line matching the marker, one Design required line and exactly one Root cause reference resolving to a line of the initial seed (lookalike prose ignored)", n),
		groupEvents).task(n, taskID, p.Role, p.Mode, p.Round, p.ExpectedBase, commit).at(ev.consumed)
	defer rb.done()
	rb.ref(taskDir(n)+"/admission.json", nil, taskID, commit)
	if marker != markerTriageConf && marker != markerTriageProb {
		rb.see("%s: rca.md only", marker)
		return
	}
	fix := fileText(result, "fix.md")
	path, line, err := parseRootCause(fix, c.trees[c.lightSeedCommit()])
	conf, cerr := parseConfidence(fix)
	req, rerr := parseDesignRequired(fix)
	switch {
	case err != nil:
		rb.bad("root_cause_invalid")
	case cerr != nil || "TRIAGE_"+conf != marker:
		rb.bad("confidence_invalid")
	case rerr != nil:
		rb.bad("design_required_invalid")
	default:
		rb.see("Root cause: %s:%d, Confidence: %s, Design required: %s", path, line, conf, req)
	}
}

// lightPinnedGo is the startup's observed Go version.
func (c *devChecker) lightPinnedGo() string {
	for _, v := range c.m.Versions {
		if v.Tool == "go" {
			return v.Version
		}
	}
	return ""
}

// lightExecArgv reports a light command record's exact name, argv and
// stream digests.
func lightExecOK(e ExecRecord, name string, argv []string, stdout, stderr []byte, hasOut, hasErr bool) bool {
	return e.Schema == ExecSchema && e.Name == name && slices.Equal(e.Argv, argv) && (!hasOut || e.StdoutSHA256 == sha256Hex(stdout)) &&
		(!hasErr || e.StderrSHA256 == sha256Hex(stderr))
}

// lightCoverArgv is a cover record's argv shape (its profile path is a
// fresh scratch path).
func lightCoverArgvOK(argv []string) bool {
	return len(argv) == len(lightArgvCover) && slices.Equal(argv, lightArgvCover)
}

// checkLightValidation judges the seed baseline, the red/green
// regression, the final suite, the ten cases, the requirement mapping and
// the coverage comparison, every one recomputed from captured bytes.
func (c *devChecker) checkLightValidation() {
	seed := c.lightSeedCommit()
	seedTree := c.trees[seed]
	seedFiles := map[string][]byte{}
	for n, f := range seedTree {
		seedFiles[n] = f.Content
	}
	base := c.lightBaselineRows(seed, seedFiles)
	rp := c.rp
	reached := c.reached(typeIs(EvFinalTests)) || (rp != nil && rp.complete && c.stop == nil)
	cand := ""
	if rp != nil {
		cand = rp.state.CandidateCommit
	}
	tree := c.treeHash[cand]
	files := map[string][]byte{}
	for p, f := range c.trees[cand] {
		files[p] = f.Content
	}
	reg := doc[DevLightRegression](c.b, "validation/light-regression.json")
	cov := doc[DevLightCoverageRecord](c.b, "validation/light-coverage.json")
	ft := doc[TestRun](c.b, "validation/final-test.json")
	cs := doc[CasesRecord](c.b, "validation/cases.json")
	rq := doc[RequirementsRecord](c.b, "validation/requirements.json")
	if reg == nil || cov == nil || ft == nil || cs == nil || rq == nil || !(rp != nil && rp.complete) {
		for _, crit := range []string{"regression_red_green", "final_tests", "coverage", "feature_oracle"} {
			c.absent(crit, "final_validation_missing", reached)
		}
		return
	}
	// The red/green regression.
	red, green := c.b.Files["validation/light/red-tests.jsonl"], c.b.Files["validation/light/green-tests.jsonl"]
	before, after := lightOverlays(seedFiles, files)
	want := DevLightRegression{Schema: c.b.kind("light-regression"), RunID: c.runID, SeedCommit: seed, SeedTree: c.treeHash[seed], Candidate: cand,
		CandidateTree: tree, TestSHA256: sha256Hex(files["main_test.go"]), Before: lightInputs(before), BeforeAfter: reg.BeforeAfter, After: lightInputs(after),
		AfterAfter: reg.AfterAfter, Red: reg.Red, Green: reg.Green, RedEvents: lightTestEventsOf(red), GreenEvents: lightTestEventsOf(green),
		Structure: lightTestStructureOf(seedFiles["main_test.go"], files["main_test.go"]), SeedCLI: base != nil && base.Defect.Exact, Error: reg.Error}
	want.Code = lightRegressionVerdict(want)
	want.Pass = want.Code == ""
	rrb := c.newRow("validation/regression-red", "regression_red_green",
		"The candidate's whole main_test.go, byte for byte over the seed's go.mod and main.go, ran the pinned go test -count=1 -json ./... to exit 1 with exactly TestRun/whitespace failing, the nine other required leaves passing and TestRun and the package failing (no build failure, panic, timeout, signal, skip or truncation), corroborated by the supervisor's seed CLI result; every exported input hashed unchanged",
		groupTail)
	rrb.r.ResultCommit = cand
	rrb.ref("validation/light-regression.json", nil, "", cand)
	rrb.ref("validation/light/red-tests.jsonl", nil, "", cand)
	grb := c.newRow("validation/regression-green", "regression_red_green",
		"The same test file over the candidate's go.mod and main.go ran the pinned go test -count=1 -json ./... to exit 0 with all ten required leaves, TestRun and the package passing and nothing failing or skipped",
		groupTail)
	grb.r.ResultCommit = cand
	grb.ref("validation/light/green-tests.jsonl", nil, "", cand)
	if !bytes.Equal(mustJSON(*reg), mustJSON(want)) {
		rrb.bad("regression_record_mismatch")
	}
	if !lightExecOK(reg.Red, "light_red", lightArgvJSON, red, c.b.Files["validation/light/red-stderr.txt"], true, true) ||
		!lightExecOK(reg.Green, "light_green", lightArgvJSON, green, c.b.Files["validation/light/green-stderr.txt"], true, true) {
		rrb.bad("regression_output_mismatch")
	}
	switch code := judgeRed(reg.Red, want.RedEvents); {
	case code != "":
		rrb.bad(code)
	case !want.SeedCLI:
		rrb.bad(CodeRegressionInvalidRed)
	case !want.Structure.ok():
		rrb.bad(CodeRegressionStructure)
	case !equalInputs(want.Before, reg.BeforeAfter):
		rrb.bad(CodeRegressionInputs)
	default:
		rrb.see("red: %v failing, exit %d", want.RedEvents.Fail, reg.Red.ExitCode)
	}
	switch code := judgeGreen(reg.Green, want.GreenEvents); {
	case code != "":
		grb.bad(code)
	case !equalInputs(want.After, reg.AfterAfter):
		grb.bad(CodeRegressionInputs)
	default:
		grb.see("green: %d leaves passing, exit %d", len(want.GreenEvents.Pass), reg.Green.ExitCode)
	}
	rrb.done()
	grb.done()
	// The final suite, build and cases, bound to the candidate.
	trb := c.newRow("validation/tests", "final_tests",
		"The final candidate (the replay's last implementation-ready commit, not the review publication) passed the pinned go test -count=1 ./..., the build and the ten CLI cases, each bound to its commit and tree within one shared 60s deadline inside the attempt",
		groupTail)
	trb.r.ResultCommit = cand
	envOK := true
	for _, e := range finalTestEnv {
		envOK = envOK && slices.Contains(ft.Env, e)
	}
	switch {
	case !slices.Equal(ft.Argv, lightArgvSuite) || !envOK || ft.Commit != cand || ft.Tree != tree || !slices.Equal(ft.Files, sortedKeys(files)):
		trb.bad("test_binding_mismatch")
	case ft.ExitCode != 0 || ft.Signal != "" || ft.TimedOut || strings.TrimSpace(ft.Toolchain) == "":
		trb.bad("tests_failed")
	case ft.Toolchain != c.lightPinnedGo():
		trb.bad("toolchain_mismatch")
	}
	if cov.FinalStdout != sha256Hex(c.b.Files["validation/final-stdout.txt"]) || cov.FinalStderr != sha256Hex(c.b.Files["validation/final-stderr.txt"]) ||
		cs.Build.StdoutSHA256 != sha256Hex(c.b.Files[buildStdoutFile]) || cs.Build.StderrSHA256 != sha256Hex(c.b.Files[buildStderrFile]) ||
		cov.CandidateExec.StdoutSHA256 != sha256Hex(c.b.Files["validation/coverage-stdout.txt"]) ||
		cov.CandidateExec.StderrSHA256 != sha256Hex(c.b.Files["validation/coverage-stderr.txt"]) {
		trb.bad("validation_output_mismatch")
	}
	if bd := cs.Build; bd.Name != "light_build" || !slices.Equal(bd.Argv, lightArgvBuild) || !cleanRun(bd) || bd.ExitCode != 0 {
		trb.bad("build_failed")
	}
	execs := []ExecRecord{reg.Red, reg.Green, cs.Build, cov.CandidateExec}
	for _, r := range cs.Cases {
		if r.Exec.Started {
			execs = append(execs, r.Exec)
		}
	}
	if code := c.lightExecBinding(execs, cand, tree); code != "" {
		trb.bad(code)
	}
	for _, f := range []string{"validation/final-test.json", "validation/cases.json", "validation/requirements.json"} {
		trb.ref(f, nil, "", cand)
	}
	trb.done()
	c.lightOracleRows(cand, tree, files, cs, rq, green, reg.Green)
	c.lightCoverageRow(cand, tree, files, base, cov, seedFiles)
}

// lightBaselineRows judges the supervisor's seed baseline and the seeded
// defect, recomputed from the retained streams and the seed objects; it
// returns the record (nil when absent).
func (c *devChecker) lightBaselineRows(seed string, seedFiles map[string][]byte) *DevLightBaseline {
	brb := c.newRow("validation/baseline", "baseline",
		"Before the first workflow task, the seed's pinned ordinary suite, JSON run (the nine seed TestRun cases passing, no whitespace case), coverage profile and build ran in the isolated offline environment on an export of exactly the seed, with every input hashed unchanged",
		groupTail)
	defer brb.done()
	b := doc[DevLightBaseline](c.b, "validation/light-baseline.json")
	if b == nil {
		brb.miss("baseline_missing", c.reached(func(e Event) bool { return e.Type == EvBaselineTests && e.Code == "light_baseline" }))
		return nil
	}
	brb.ref("validation/light-baseline.json", nil, "", seed)
	files := lightSourceFiles(seedFiles)
	stream := c.b.Files["validation/light/baseline-tests.jsonl"]
	profile, hasProfile := c.b.Files["validation/light/baseline-coverage.out"]
	want := *b
	want.Schema, want.RunID, want.SeedCommit, want.SeedTree = c.b.kind("light-baseline"), c.runID, seed, c.treeHash[seed]
	want.Module, want.Inputs = modulePathOf(files["go.mod"]), lightInputs(files)
	want.Tests = lightTestEventsOf(stream)
	want.Coverage = nil
	if hasProfile {
		if cv, err := deriveWholeCoverage(files["main.go"], profile, want.Module); err == nil {
			want.Coverage = &cv
		}
	}
	for _, cr := range want.Cases {
		if cr.ID == lightWhitespaceCase {
			want.Defect = LightDefectObservation{Exit: cr.Exec.ExitCode, Stdout: cr.Stdout, Stderr: cr.Stderr,
				Exact: cleanRun(cr.Exec) && cr.Exec.ExitCode == 2 && cr.Stdout == "" && cr.Stderr == lightWhitespaceStderr}
		}
	}
	var err error
	if b.Error != "" {
		err = fmt.Errorf("%s", b.Error)
	}
	want.Code = lightBaselineVerdict(want, err)
	want.Valid = want.Code == ""
	if !bytes.Equal(mustJSON(*b), mustJSON(want)) {
		brb.bad("baseline_record_mismatch")
	}
	if !lightExecOK(b.Suite, "light_baseline_suite", lightArgvSuite, c.b.Files["validation/light/baseline-stdout.txt"],
		c.b.Files["validation/light/baseline-stderr.txt"], true, true) || !lightExecOK(b.JSON, "light_baseline_json", lightArgvJSON, stream, nil, true, false) ||
		!lightExecOK(b.Cover, "light_baseline_cover", lightArgvCover, nil, nil, false, false) || !lightExecOK(b.Version, "light_baseline_version", lightArgvVersion, nil, nil, false, false) {
		brb.bad("baseline_output_mismatch")
	}
	if b.Toolchain != c.lightPinnedGo() {
		brb.bad("toolchain_mismatch")
	}
	for i, fc := range FeatureCases {
		if i >= len(b.Cases) || b.Cases[i].ID != fc.ID {
			brb.bad("baseline_record_mismatch")
			break
		}
		r := b.Cases[i]
		pass, code := evaluateCase(fc, r.Exec, r.Stdout, r.Stderr)
		if pass != r.Pass || code != r.Code || r.Exec.Started && (sha256Hex([]byte(r.Stdout)) != r.Exec.StdoutSHA256 || sha256Hex([]byte(r.Stderr)) != r.Exec.StderrSHA256) {
			brb.bad("baseline_record_mismatch")
		}
	}
	if !want.Valid {
		brb.bad(CodeLightSeedInvalid)
	}
	drb := c.newRow("validation/seed-defect", "baseline",
		"The seed exhibits exactly the specified defect: the nine unaffected CLI cases pass and --name \" \" exits 2 with empty stdout and exactly greeting: --name must not be empty on stderr",
		groupTail)
	drb.ref("validation/light-baseline.json", nil, "", seed)
	if !want.Defect.Exact {
		drb.bad(CodeLightSeedInvalid)
	}
	drb.see("whitespace: exit %d, stdout %q, stderr %q", want.Defect.Exit, auditText(want.Defect.Stdout, 40), auditText(want.Defect.Stderr, 80))
	drb.done()
	return b
}

// lightExecBinding binds the light final validation's commands to the
// candidate, to their own validation events, and to one shared 60s
// deadline from the first of them, inside the attempt.
func (c *devChecker) lightExecBinding(execs []ExecRecord, commit, tree string) string {
	var first, last time.Time
	for _, e := range execs {
		start, end := c.eventAt(e.StartSeq), c.eventAt(e.EndSeq)
		if e.Schema != ExecSchema || e.Commit != commit || e.Tree != tree || start == nil || end == nil || start.Type != EvValidationStarted ||
			end.Type != EvValidationFinished || start.Code != e.Name || end.Code != e.Name || e.StartSeq >= e.EndSeq {
			return "validation_binding_mismatch"
		}
		s, _ := parseTime(start.Time)
		f, _ := parseTime(end.Time)
		if first.IsZero() || s.Before(first) {
			first = s
		}
		if f.After(last) {
			last = f
		}
	}
	if !last.Before(finalDeadline(first, c.attemptDeadline().Add(-AttemptBound))) {
		return "validation_deadline_exceeded"
	}
	return ""
}

// lightOracleRows are the light final oracle's signature, table and case
// rows (the requirement map recomputed from the green stream).
func (c *devChecker) lightOracleRows(cand, tree string, files map[string][]byte, cs *CasesRecord, rq *RequirementsRecord, green []byte, greenExec ExecRecord) {
	want := buildRequirements(files, green, cs.Cases, greenExec, cand)
	srb := c.newRow("validation/signature", "feature_oracle", "main.go declares run(args []string, stdout, stderr io.Writer) int", groupTail)
	srb.ref("validation/requirements.json", nil, "", cand)
	if !want.RunSignature {
		srb.bad(CodeTestMapping)
	}
	if !requirementsEqual(want, *rq) {
		srb.bad("requirements_mismatch")
	}
	srb.done()
	tbl := c.newRow("validation/table", "feature_oracle", "main_test.go has TestRun with a table loop that calls run, and every TestRun/<case-id> passed in the complete green go test -json stream with nothing failing or skipped", groupTail)
	tbl.ref("validation/light/green-tests.jsonl", nil, "", cand)
	if !want.TableTests || !want.TestCallsRun || len(want.MissingTests) > 0 || len(want.FailedTests) > 0 || !want.TestRunPassed {
		tbl.bad(CodeTestMapping)
	}
	tbl.done()
	if len(cs.Cases) != len(FeatureCases) || cs.Commit != cand || cs.Tree != tree {
		c.fail("feature_oracle", CodeCaseFailed)
	}
	for i, fc := range FeatureCases {
		crb := c.newRow("case/"+fc.ID, "feature_oracle", fmt.Sprintf("The built CLI run with argv %q gives %s", fc.Args, caseExpectation(fc)), groupTail)
		crb.ref("validation/cases.json", nil, "", cand)
		if i >= len(cs.Cases) || cs.Cases[i].ID != fc.ID {
			crb.bad(CodeCaseFailed)
			crb.done()
			continue
		}
		r := cs.Cases[i]
		pass, code := evaluateCase(fc, r.Exec, r.Stdout, r.Stderr)
		if pass != r.Pass || code != r.Code || !slices.Equal(r.Exec.Argv, append([]string{"greeting"}, fc.Args...)) || r.Exec.Name != "case_"+fc.ID ||
			(r.Exec.Started && (sha256Hex([]byte(r.Stdout)) != r.Exec.StdoutSHA256 || sha256Hex([]byte(r.Stderr)) != r.Exec.StderrSHA256)) {
			crb.bad("case_record_mismatch")
		}
		if !pass {
			crb.bad(CodeCaseFailed)
		}
		crb.see("exit %d, stdout %q, stderr %d bytes", r.Exec.ExitCode, auditText(r.Stdout, 80), len(r.Stderr))
		crb.done()
	}
}

// lightCoverageRow recomputes both coverage sides from their captured
// profiles and sources and their integer comparison.
func (c *devChecker) lightCoverageRow(cand, tree string, files map[string][]byte, base *DevLightBaseline, cov *DevLightCoverageRecord, seedFiles map[string][]byte) {
	rb := c.newRow("validation/coverage-comparison", "coverage",
		"The seed's and the candidate's whole-production coverage, each recomputed from its own pinned go test -count=1 -covermode=set profile of example.com/greeting/main.go with identical flags and scope, compare as a nondecreasing exact covered-statement fraction (candidateCovered*baselineTotal >= baselineCovered*candidateTotal, positive denominators)",
		groupTail)
	defer rb.done()
	rb.r.ResultCommit = cand
	rb.ref("validation/light-coverage.json", nil, "", cand)
	rb.ref("validation/coverage.out", nil, "", cand)
	rb.ref("validation/light/baseline-coverage.out", nil, "", "")
	var baseCov *LightCoverage
	if base != nil {
		baseCov = base.Coverage
	}
	cmp, candCov, errText := lightCoverageOf(baseCov, files, c.b.Files["validation/coverage.out"], cov.CandidateExec)
	want := *cov
	want.Schema, want.RunID, want.SeedCommit, want.Candidate, want.CandidateTree = c.b.kind("light-coverage"), c.runID, c.lightSeedCommit(), cand, tree
	want.Argv, want.Baseline, want.BaselineTest, want.CandidateTest = lightArgvCover, baseCov, sha256Hex(seedFiles["main_test.go"]), sha256Hex(files["main_test.go"])
	want.Cand, want.Comparison, want.Error = candCov, cmp, errText
	switch {
	case !bytes.Equal(mustJSON(*cov), mustJSON(want)):
		rb.bad("coverage_record_mismatch")
	case cov.CandidateExec.Name != "light_final_cover" || !lightCoverArgvOK(cov.CandidateExec.Argv):
		rb.bad("coverage_binding_mismatch")
	case cmp.Code != "":
		rb.bad(cmp.Code)
	default:
		rb.see("coverage %s from %s (%s >= %s)", coverageFraction(*candCov), coverageFraction(*baseCov), cmp.Left, cmp.Right)
	}
}

// lightJudgmentRows derive the light judgment layer: each triage's causal
// evidence and confidence and its brief content and references, each
// candidate's regression assertions and restoration scope, and each
// review's reasoning and findings (always unobserved here).
func (c *devChecker) lightJudgmentRows() {
	if c.rp == nil {
		return
	}
	for _, t := range c.rp.state.Tasks {
		dir := taskDir(t.Ordinal)
		switch {
		case t.Role == "triage":
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/triage", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The triage's causal evidence supports its root cause and stated confidence", [2]string{"rca.md"}, [2]string{"fix.md"})
			if t.Marker == markerTriageConf || t.Marker == markerTriageProb {
				c.judgmentRow(fmt.Sprintf("judgment/task/%02d/brief", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
					"The brief's Symptom, Evidence, Fix, Blast radius, CI impact, Regression test and Investigation content, its root-cause explanation, decisive quotations, immutable input commit references and other artifact references are present, substantive and resolve",
					[2]string{"fix.md"}, [2]string{"rca.md"})
			}
		case t.Role == "coder" && t.Disposition == dispositionReady:
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/regression", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The candidate's regression asserts the whitespace requirement (exit, stdout and empty stderr) and its restoration is confined to the defect without changing other behavior",
				[2]string{"main.go"}, [2]string{"main_test.go"})
		case t.Role == "code-reviewer":
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/review", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The light review's reasoning about the root cause, the regression's assertions and the restoration is sound, its findings are justified and its verdict is defensible",
				[2]string{"review.md", dir + "/final.txt"})
		}
	}
}

// lightReviewerExecution re-derives a light code review's execution
// evidence and compares it with the stored record.
func (c *devChecker) lightReviewerExecution(n int, p TaskPlan, v *contract.TaskView, marker string, cand map[string]TreeFile, report string, ev taskEvents) DevReviewEvidence {
	dir := taskDir(n)
	rb := c.newRow(fmt.Sprintf("task/%02d/reviewer-execution", n), "reviewer_execution",
		"The light reviewer ran the fixed light verification helper (--scenario light) exactly once, corroborated by one top-level command record in its complete captured log; its light receipt is bound to this task, candidate, seed, Go executable and deadline, its exports, red/green events and both coverage profiles recompute, review.md carries its light report lines and digest, and an approval has its complete successful run",
		groupEvents).task(n, v.TaskID, p.Role, p.Mode, p.Round, p.ExpectedBase, "").at(ev.consumed)
	defer rb.done()
	seed := c.lightSeedCommit()
	base := bundleReviewInput(c.b, dir, n, v, c.m.Helper, p.ExpectedBase, seed, c.treeHash[p.ExpectedBase], cand,
		fileContent(c.trees[seed], "main.go"), marker, report)
	exact := false
	if b := doc[DevLightBaseline](c.b, "validation/light-baseline.json"); b != nil {
		exact = b.Defect.Exact
	}
	derived := DeriveLightReviewEvidence(lightReviewInput{reviewEvidenceInput: base, SeedFiles: c.trees[seed], SeedTree: c.treeHash[seed], SeedCLI: exact})
	c.rp.reviews[n] = derived
	stored := doc[DevReviewRecord](c.b, dir+"/review-evidence.json")
	switch {
	case stored == nil:
		rb.miss("review_evidence_missing", true)
	case !bytes.Equal(mustJSON(stored.Evidence), mustJSON(derived)):
		rb.bad(CodeReviewMismatch)
	}
	if code, stops := derived.blocks(); stops {
		rb.bad(code)
	}
	if derived.Approving && derived.Status != evidenceVerified {
		rb.bad(CodeReviewUnobserved)
	}
	rb.ref(dir+"/logs.txt", nil, v.TaskID, "")
	rb.ref(dir+"/review-evidence.json", nil, v.TaskID, "")
	if derived.Started != nil {
		rb.refs([]SourceRef{{File: derived.Started.File, SHA256: derived.Started.SHA256, FirstLine: derived.Started.FirstLine, LastLine: derived.Completed.LastLine}}, v.TaskID)
	}
	rb.see("evidence %s (%s), run %s", derived.Status, cmpOr(derived.Code, "ok"), cmpOr(derived.RunStatus, "none"))
	return derived
}
