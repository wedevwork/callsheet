package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// Unit tests of the light scenario (design 12a-dev-flow-light, Testing
// plan, Unit tests): the committed seed, the triage admission rule, the
// light transitions, the red/green event judges and the coverage
// comparison, each without a subprocess.

// lightRequestParagraph is the design's exact light request (the first
// paragraph of the seed's FEATURE.md).
const lightRequestParagraph = "Restore the documented preservation of nonempty names, including whitespace-only names. `--name \" \"` currently exits 2 with no stdout and `greeting: --name must not be empty` on stderr. It must exit 0, print `Hello,  !` followed by a newline, and leave stderr empty. Investigate the cause, add the missing regression and restore this behavior without changing the interface or adding dependencies."

func TestDevLightSeed(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	read := func(dir, name string) []byte { return mustRead(t, filepath.Join(root, filepath.FromSlash(dir), name)) }
	probeMain, probeTest := read(devProbeFixtureDir, "main.go"), read(devProbeFixtureDir, "main_test.go")
	seedMain, seedTest, feature := read(devLightFixtureDir, "main.go"), read(devLightFixtureDir, "main_test.go"), read(devLightFixtureDir, "FEATURE.md")
	t.Run("defect_transformation", func(t *testing.T) {
		t.Parallel()
		// Exactly the import of strings and the one pointer-variable
		// empty-name condition.
		if n := strings.Count(string(probeMain), lightKnownGoodLine+"\n"); n != 1 {
			t.Fatalf("the known-good line occurs %d times", n)
		}
		lines := strings.Split(string(seedMain), "\n")
		probeLines := strings.Split(string(probeMain), "\n")
		var added, changed []string
		i, j := 0, 0
		for i < len(probeLines) || j < len(lines) {
			switch {
			case i < len(probeLines) && j < len(lines) && probeLines[i] == lines[j]:
				i, j = i+1, j+1
			case j < len(lines) && lines[j] == "\t\"strings\"":
				added = append(added, lines[j])
				j++
			case i < len(probeLines) && j < len(lines):
				changed = append(changed, probeLines[i]+" => "+lines[j])
				i, j = i+1, j+1
			default:
				t.Fatalf("unaligned at %d/%d", i, j)
			}
		}
		if !slices.Equal(added, []string{"\t\"strings\""}) || !slices.Equal(changed, []string{lightKnownGoodLine + " => " + lightDefectLine}) {
			t.Fatalf("added %q changed %q", added, changed)
		}
		if d := lightDefectOf(seedMain); d.Line == 0 || d.Path != "main.go" {
			t.Fatalf("defect %+v", d)
		}
		if d := lightDefectOf(probeMain); d.Line != 0 {
			t.Fatalf("the known-good fixture has the defect at %d", d.Line)
		}
	})
	t.Run("test_omission", func(t *testing.T) {
		t.Parallel()
		// The nine-case table: the known-good one without only its
		// whitespace row.
		row := "\t\t{\"whitespace\", []string{\"--name\", \" \"}, 0, \"Hello,  !\\n\", false},\n"
		if strings.Count(string(probeTest), row) != 1 || string(seedTest) != strings.Replace(string(probeTest), row, "", 1) {
			t.Fatal("the seed test is not the known-good test without exactly its whitespace row")
		}
		if got := len(tableRows(seedTest)); got != 9 {
			t.Fatalf("seed rows %d", got)
		}
		if got := lightSeedCases(); len(got) != 9 || slices.Contains(got, lightWhitespaceCase) {
			t.Fatalf("seed cases %v", got)
		}
	})
	t.Run("request_and_budgets", func(t *testing.T) {
		t.Parallel()
		contract, err := devFullFeatureRequest()
		if err != nil {
			t.Fatal(err)
		}
		if string(feature) != lightRequestParagraph+"\n\n"+contract {
			t.Fatal("FEATURE.md is not the light request followed by the ten-case contract and budgets")
		}
		for _, leak := range []string{"TrimSpace", "main.go:", "Root cause"} {
			if strings.Contains(string(feature), leak) {
				t.Errorf("FEATURE.md tells triage %q", leak)
			}
		}
		files := lightFixture(t)
		total := 0
		for _, n := range lightFixtureFiles {
			total += len(files[n])
		}
		if total >= 16<<10 {
			t.Fatalf("the four fixture files are %d bytes", total)
		}
		rec := lightScenarioRecord("r", "rev", files)
		if rec.Defect.Line != lightDefectOf(files["main.go"]).Line || len(rec.RequiredCases) != 10 || len(rec.SeedCases) != 9 || rec.Scope != lightScope ||
			len(rec.Files) != 4 || rec.Files[0].StoragePath != capturedPath(lightFixtureCaptureDir+"/FEATURE.md") {
			t.Fatalf("scenario record %+v", rec)
		}
	})
}

// lightSeedTree is the light seed as a tree.
func lightSeedTree(t testing.TB) map[string]TreeFile {
	t.Helper()
	out := map[string]TreeFile{}
	for n, b := range lightFixture(t) {
		out[n] = TreeFile{Path: n, Mode: filemode.Regular, Content: b}
	}
	return out
}

// withTree is base with files added or replaced.
func withTree(base map[string]TreeFile, files map[string]string) map[string]TreeFile {
	out := map[string]TreeFile{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range files {
		out[k] = TreeFile{Path: k, Mode: filemode.Regular, Content: []byte(v)}
	}
	return out
}

// brief is a qualified fix.md around body lines.
func brief(conf string, design string, lines ...string) string {
	return "# Fix brief\n\n" + strings.Join(lines, "\n") + "\n\nConfidence: " + conf + "\nDesign required: " + design + "\n"
}

func TestDevLightBriefAdmission(t *testing.T) {
	t.Parallel()
	seed := lightSeedTree(t)
	defect := lightDefectOf(seed["main.go"].Content).Line
	root := fmt.Sprintf("Root cause: main.go:%d", defect)
	rca := func(marker string) string {
		return "# RCA\n\nThe trimmed check refuses whitespace.\n\nSTATUS: " + marker + "\n"
	}
	admit := func(base map[string]TreeFile, marker string, files map[string]string) (admitResult, error) {
		return AdmitPublication(admitInput{Role: "triage", Mode: modeInvestigate, Marker: marker, Base: treeList(base), Result: treeList(withTree(base, files)),
			Final: "Done.\nSTATUS: " + marker + "\n", TaskID: hexID("t_", 5), Scenario: scenarioLight, Seed: seed})
	}
	for name, c := range map[string]struct {
		marker string
		fix    string
		extra  map[string]string
		ok     bool
	}{
		"confirmed":              {markerTriageConf, brief("CONFIRMED", "no", root), nil, true},
		"probable":               {markerTriageProb, brief("PROBABLE", "no", root), nil, true},
		"design_yes_admitted":    {markerTriageConf, brief("CONFIRMED", "yes", root), nil, true},
		"no_sections_admitted":   {markerTriageConf, root + "\nConfidence: CONFIRMED\nDesign required: no\n", nil, true},
		"test_path":              {markerTriageConf, brief("CONFIRMED", "no", "Root cause: main_test.go:1"), nil, true},
		"last_line":              {markerTriageConf, brief("CONFIRMED", "no", fmt.Sprintf("Root cause: main.go:%d", fileLineCount(seed["main.go"].Content))), nil, true},
		"root_lookalike_prose":   {markerTriageConf, brief("CONFIRMED", "no", "Root cause: the TrimSpace call rejects whitespace", root), nil, true},
		"lookalike_only":         {markerTriageConf, brief("CONFIRMED", "no", "Root cause: the TrimSpace call rejects whitespace"), nil, false},
		"absent_root":            {markerTriageConf, brief("CONFIRMED", "no"), nil, false},
		"two_roots":              {markerTriageConf, brief("CONFIRMED", "no", root, "Root cause: main_test.go:1"), nil, false},
		"same_root_twice":        {markerTriageConf, brief("CONFIRMED", "no", root, root), nil, false},
		"fenced_root":            {markerTriageConf, brief("CONFIRMED", "no", "```", root, "```"), nil, false},
		"indented_root":          {markerTriageConf, brief("CONFIRMED", "no", " "+root), nil, false},
		"trailing_space":         {markerTriageConf, brief("CONFIRMED", "no", root+" "), nil, false},
		"backticks":              {markerTriageConf, brief("CONFIRMED", "no", "`"+root+"`"), nil, false},
		"lowercase":              {markerTriageConf, brief("CONFIRMED", "no", strings.ToLower(root)), nil, false},
		"leading_zero":           {markerTriageConf, brief("CONFIRMED", "no", "Root cause: main.go:0"+fmt.Sprint(defect)), nil, false},
		"zero":                   {markerTriageConf, brief("CONFIRMED", "no", "Root cause: main.go:0"), nil, false},
		"out_of_range":           {markerTriageConf, brief("CONFIRMED", "no", fmt.Sprintf("Root cause: main.go:%d", fileLineCount(seed["main.go"].Content)+1)), nil, false},
		"overflow":               {markerTriageConf, brief("CONFIRMED", "no", "Root cause: main.go:99999999999999999999999"), nil, false},
		"disallowed_path":        {markerTriageConf, brief("CONFIRMED", "no", "Root cause: FEATURE.md:1"), nil, false},
		"disallowed_and_valid":   {markerTriageConf, brief("CONFIRMED", "no", "Root cause: go.mod:1", root), nil, true},
		"absent_confidence":      {markerTriageConf, root + "\nDesign required: no\n", nil, false},
		"duplicate_confidence":   {markerTriageConf, brief("CONFIRMED", "no", root, "Confidence: CONFIRMED"), nil, false},
		"mismatched_confidence":  {markerTriageConf, brief("PROBABLE", "no", root), nil, false},
		"absent_design":          {markerTriageConf, root + "\nConfidence: CONFIRMED\n", nil, false},
		"lookalike_design":       {markerTriageConf, brief("CONFIRMED", "no", root, "Design required: maybe"), nil, false},
		"whitespace_brief":       {markerTriageConf, " \n\t\n", nil, false},
		"source_change":          {markerTriageConf, brief("CONFIRMED", "no", root), map[string]string{"main.go": "package main\n"}, false},
		"config_change":          {markerTriageConf, brief("CONFIRMED", "no", root), map[string]string{"go.mod": "module x\n"}, false},
		"unconfirmed_new_fix":    {markerTriageUnconf, brief("PROBABLE", "no", root), nil, false},
		"blocked_new_fix":        {markerTriageBlocked, brief("PROBABLE", "no", root), nil, false},
		"unconfirmed_rca_only":   {markerTriageUnconf, "", nil, true},
		"extra_path":             {markerTriageConf, brief("CONFIRMED", "no", root), map[string]string{"notes.md": "x\n"}, false},
		"mismatched_rca_marker":  {markerTriageConf, brief("CONFIRMED", "no", root), map[string]string{"rca.md": rca(markerTriageProb)}, false},
		"recovery_resolves_seed": {markerTriageConf, brief("CONFIRMED", "no", root), nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"rca.md": rca(c.marker)}
			if c.fix != "" {
				files["fix.md"] = c.fix
			}
			for k, v := range c.extra {
				files[k] = v
			}
			base := seed
			if name == "recovery_resolves_seed" {
				// Re-triage on a later chain commit whose main.go is much
				// shorter: the reference still resolves in the seed.
				base = withTree(seed, map[string]string{"main.go": "package main\n\nfunc main() {}\n", "rca.md": "old\n", "fix.md": "old\n"})
			}
			res, err := admit(base, c.marker, files)
			if (err == nil) != c.ok {
				t.Fatalf("admitted %v, want %v (%v)", err == nil, c.ok, err)
			}
			if err != nil && strings.Contains(name, "root") && !strings.Contains(err.Error(), "root_cause_invalid") && name != "fenced_root" {
				t.Logf("refusal: %v", err)
			}
			if c.ok && c.fix != "" && !res.BriefCurrent {
				t.Fatal("an admitted qualified brief is not current")
			}
		})
	}
	t.Run("lookalike_refusal_is_missing_reference", func(t *testing.T) {
		t.Parallel()
		_, err := admit(seed, markerTriageConf, map[string]string{"rca.md": rca(markerTriageConf), "fix.md": brief("CONFIRMED", "no", "Root cause: the TrimSpace call rejects whitespace")})
		if err == nil || !strings.Contains(err.Error(), "no root-cause reference line") {
			t.Fatalf("a prose-only brief: %v", err)
		}
	})
	t.Run("inherited_brief", func(t *testing.T) {
		t.Parallel()
		// An inherited fix.md never counts as current: a qualified marker
		// refuses it, an unconfirmed one keeps it unchanged.
		base := withTree(seed, map[string]string{"rca.md": "old\n", "fix.md": brief("CONFIRMED", "no", root)})
		if _, err := admit(base, markerTriageConf, map[string]string{"rca.md": rca(markerTriageConf)}); err == nil {
			t.Fatal("an inherited brief was admitted as current")
		}
		res, err := admit(base, markerTriageUnconf, map[string]string{"rca.md": rca(markerTriageUnconf)})
		if err != nil || res.BriefCurrent {
			t.Fatalf("an unconfirmed publication keeping the brief: %v %v", err, res.BriefCurrent)
		}
	})
	t.Run("full_scenario_ignores_reference", func(t *testing.T) {
		t.Parallel()
		_, err := AdmitPublication(admitInput{Role: "triage", Mode: modeInvestigate, Marker: markerTriageConf, Base: treeList(seed),
			Result: treeList(withTree(seed, map[string]string{"rca.md": rca(markerTriageConf), "fix.md": brief("CONFIRMED", "no")})), Final: "STATUS: " + markerTriageConf + "\n"})
		if err != nil {
			t.Fatalf("the full flow's triage needs no root-cause reference: %v", err)
		}
	})
	t.Run("envelope_lines", func(t *testing.T) {
		t.Parallel()
		// The exact root_cause line in the initial, recovery and probe
		// triage envelopes, with the shipped lines unchanged, within both
		// encodings' budgets.
		st, step := InitialPlanFor(scenarioLight, "s")
		ec := envelopeContext{runID: "20261024T000000Z-01020304", state: st, trees: map[string]map[string]TreeFile{"s": seed}, scenario: []byte("scenario\n"),
			intake: strings.Repeat("e", 64)}
		goal, acc, _, err := buildEnvelope(ec, step.Plan)
		if err != nil {
			t.Fatal(err)
		}
		recovery := *step.Plan
		recovery.Reason = "questions"
		ec.state.NeedsTriage = &Outcome{TaskID: hexID("t_", 2), Commit: "c", Marker: markerQuestions, Final: "Why?\nSTATUS: DESIGN_QUESTIONS\n"}
		rgoal, racc, _, err := buildEnvelope(ec, &recovery)
		if err != nil {
			t.Fatal(err)
		}
		pgoal, pacc := probeTriageEnvelope("20261024T000000Z-01020304", strings.Repeat("a", 40))
		for name, g := range map[string][2]string{"initial": {goal, acc}, "recovery": {rgoal, racc}, "probe": {pgoal, pacc}} {
			for _, line := range []string{lightRootCauseInstruction, triageDesignRequiredInstruction, triageConfidenceInstruction} {
				if strings.Count(g[0], "\n"+line+"\n") != 1 {
					t.Errorf("%s: the line %.40q is not present exactly once", name, line)
				}
			}
			if err := checkTriageTaskBudget(g[0], g[1]); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			enc, _ := json.Marshal(g[0] + g[1])
			if len(g[0])+len(g[1]) > 8192 || len(enc) > 8192+2 {
				t.Errorf("%s: %d raw bytes, %d encoded", name, len(g[0])+len(g[1]), len(enc))
			}
		}
		if !strings.Contains(lightRootCauseInstruction, "Root cause: main.go:N or Root cause: main_test.go:N outside code fences") {
			t.Fatal("the root_cause instruction is not the design's")
		}
		// Over budget, the triage envelope is refused before exposure, not
		// truncated.
		ec.scenario = bytes.Repeat([]byte("x"), 9000)
		if _, _, _, err := buildEnvelope(ec, step.Plan); err == nil || codeOf(err) != CodePromptBudget {
			t.Fatalf("an oversized triage envelope: %v", err)
		}
	})
}

// lightSteps drives NextLight from the initial triage through markers,
// each admitted against the pending plan; it returns the final state, the
// last step and every scheduled plan.
func lightSteps(t *testing.T, acts ...devAct) (DevFullState, StepResult, []TaskPlan) {
	t.Helper()
	st, step := InitialPlanFor(scenarioLight, "seed0")
	plans := []TaskPlan{*step.Plan}
	for i, a := range acts {
		p := st.Pending
		if p == nil {
			t.Fatalf("act %d: nothing pending (%+v)", i, step)
		}
		out := Admitted{Ordinal: p.Ordinal, TaskID: hexID("t_", p.Ordinal), Role: p.Role, Mode: p.Mode, Marker: a.marker, Commit: fmt.Sprintf("%040d", p.Ordinal),
			Final: "STATUS: " + a.marker + "\n", Blocker: cmpOr(a.blocker, blockerNone), DesignRequired: a.designRequired, BriefCurrent: a.alter != "stale"}
		st, step = NextLight(st, out)
		if step.Plan != nil {
			plans = append(plans, *step.Plan)
		}
	}
	return st, step, plans
}

func TestDevLightTransitions(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	prob := devAct{marker: markerTriageProb, designRequired: designRequiredNo}
	impl, fixes := devAct{marker: markerImplComplete}, devAct{marker: markerFixesComplete}
	changes, approve := devAct{marker: markerReviewChanges}, devAct{marker: markerReviewApproved}
	for name, c := range map[string]struct {
		acts   []devAct
		action string
		stop   string
		modes  []string
	}{
		"happy":            {[]devAct{conf, impl, approve}, actionComplete, "", []string{"triage/investigate", "coder/defect", "code-reviewer/light"}},
		"probable":         {[]devAct{prob, impl, approve}, actionComplete, "", nil},
		"two_fix_loops":    {[]devAct{conf, impl, changes, fixes, changes, fixes, approve}, actionComplete, "", []string{"triage/investigate", "coder/defect", "code-reviewer/light", "coder/fix", "code-reviewer/light", "coder/fix", "code-reviewer/light"}},
		"third_fix_capped": {[]devAct{conf, impl, changes, fixes, changes, fixes, changes}, actionStop, stopCap, nil},
		"questions_retriage": {[]devAct{conf, {marker: markerQuestions}, conf, impl, approve}, actionComplete, "",
			[]string{"triage/investigate", "coder/defect", "triage/investigate", "coder/defect", "code-reviewer/light"}},
		"needs_triage_resume_fix": {[]devAct{conf, impl, changes, {marker: markerNeedsTriage}, prob, fixes, approve}, actionComplete, "",
			[]string{"triage/investigate", "coder/defect", "code-reviewer/light", "coder/fix", "triage/investigate", "coder/fix", "code-reviewer/light"}},
		"third_recovery_capped": {[]devAct{conf, {marker: markerQuestions}, conf, {marker: markerQuestions}}, actionStop, stopCap, nil},
		"nine_tasks":            {[]devAct{conf, impl, changes, fixes, changes, {marker: markerNeedsTriage}, conf, fixes, approve}, actionComplete, "", nil},
		"design_yes":            {[]devAct{{marker: markerTriageConf, designRequired: designRequiredYes}}, actionStop, stopNeedsDesign, nil},
		"coder_needs_design":    {[]devAct{conf, {marker: markerNeedsDesign}}, actionStop, stopNeedsDesign, nil},
		"review_needs_design":   {[]devAct{conf, impl, {marker: markerNeedsDesign}}, actionStop, stopNeedsDesign, nil},
		"unconfirmed":           {[]devAct{{marker: markerTriageUnconf}}, actionStop, "cause_unconfirmed", nil},
		"triage_blocked":        {[]devAct{{marker: markerTriageBlocked}}, actionStop, "obstacle", nil},
		"coder_blocked":         {[]devAct{conf, {marker: markerBlocked}}, actionStop, "obstacle", nil},
		"environment":           {[]devAct{conf, impl, {marker: markerReviewChanges, blocker: blockerEnvironment}}, actionStop, "environment_blocker", nil},
		"stale_brief":           {[]devAct{{marker: markerTriageConf, designRequired: designRequiredNo, alter: "stale"}}, actionStop, "stale_brief", nil},
		"defect_needs_triage":   {[]devAct{conf, {marker: markerNeedsTriage}}, actionStop, "invalid_marker", nil},
		"defect_fixes_complete": {[]devAct{conf, fixes}, actionStop, "invalid_marker", nil},
		"triage_bad_field":      {[]devAct{{marker: markerTriageConf, designRequired: "maybe"}}, actionStop, "invalid_field", nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st, step, plans := lightSteps(t, c.acts...)
			if step.Action != c.action || step.Stop != c.stop {
				t.Fatalf("step %+v, want %s %s", step, c.action, c.stop)
			}
			if st.InitialCommit != "seed0" {
				t.Fatalf("the initial commit moved to %s", st.InitialCommit)
			}
			for _, p := range plans {
				if p.OriginalMode != modeDefect || p.Scenario != scenarioLight {
					t.Fatalf("plan %d original mode %s scenario %s", p.Ordinal, p.OriginalMode, p.Scenario)
				}
			}
			if c.modes != nil {
				var got []string
				for _, p := range plans {
					got = append(got, p.Role+"/"+p.Mode)
				}
				if !slices.Equal(got, c.modes) {
					t.Fatalf("modes %v", got)
				}
			}
			if len(st.Tasks) > maxLightTasks || st.CodeCycles > maxCodeCycles || st.Recovery > maxRecoveryTasks {
				t.Fatalf("caps exceeded: %d tasks, %d cycles, %d recovery", len(st.Tasks), st.CodeCycles, st.Recovery)
			}
			if c.stop == stopCap && step.Reason == "" {
				t.Fatal("cap_exhausted names no next task")
			}
		})
	}
	t.Run("no_refund", func(t *testing.T) {
		t.Parallel()
		// A recovery's reservation stays after its triage stops.
		st, _, _ := lightSteps(t, conf, devAct{marker: markerQuestions}, devAct{marker: markerTriageUnconf})
		if st.Recovery != 1 {
			t.Fatalf("recovery %d after a failed re-triage", st.Recovery)
		}
	})
	t.Run("brief_inputs", func(t *testing.T) {
		t.Parallel()
		// A later qualified triage replaces the coder's brief inputs.
		_, _, plans := lightSteps(t, conf, devAct{marker: markerQuestions}, prob)
		last := plans[len(plans)-1]
		var brief []PlanInput
		for _, in := range last.Inputs {
			if in.Kind == "brief" {
				brief = append(brief, in)
			}
		}
		if len(brief) != 1 || brief[0].Ordinal != 3 || brief[0].Path != "fix.md" {
			t.Fatalf("brief inputs %+v", brief)
		}
	})
	t.Run("marker_matrix", func(t *testing.T) {
		t.Parallel()
		all := []string{markerImplComplete, markerFixesComplete, markerQuestions, markerNeedsDesign, markerNeedsTriage, markerBlocked, markerReviewApproved,
			markerReviewChanges, markerTriageConf, markerTriageProb, markerTriageUnconf, markerTriageBlocked, markerDesignDraft}
		want := map[string][]string{
			"coder/" + modeDefect:              {markerImplComplete, markerQuestions, markerNeedsDesign, markerBlocked},
			"coder/" + modeFix:                 {markerFixesComplete, markerQuestions, markerNeedsDesign, markerNeedsTriage, markerBlocked},
			"code-reviewer/" + modeReviewLight: {markerReviewApproved, markerReviewChanges, markerNeedsDesign},
			"triage/" + modeInvestigate:        {markerTriageConf, markerTriageProb, markerTriageUnconf, markerTriageBlocked},
			"designer/draft":                   nil,
		}
		for rm, ok := range want {
			role, mode, _ := strings.Cut(rm, "/")
			for _, m := range all {
				if lightMarkerAllowed(role, mode, m) != slices.Contains(ok, m) {
					t.Errorf("%s %s: allowed %v", rm, m, !slices.Contains(ok, m))
				}
			}
		}
	})
	t.Run("full_unchanged", func(t *testing.T) {
		t.Parallel()
		st, step := InitialPlanFor(scenarioFull, "s")
		if step.Plan.Role != "designer" || st.Scenario != scenarioFull || step.Plan.Scenario != "" || step.Plan.OriginalMode != modeImplement {
			t.Fatalf("the full initial plan %+v %+v", st, step.Plan)
		}
	})
}

// redStream is a go test -json stream of the ten-row table with failing
// leaves and extra raw lines.
func redStream(fail []string, extra ...string) []byte {
	var b strings.Builder
	pkg := "example.com/greeting"
	any := false
	for _, c := range FeatureCases {
		act := "pass"
		if slices.Contains(fail, c.ID) {
			act, any = "fail", true
		}
		fmt.Fprintf(&b, `{"Action":"run","Package":"%s","Test":"TestRun/%s"}`+"\n", pkg, c.ID)
		fmt.Fprintf(&b, `{"Action":"%s","Package":"%s","Test":"TestRun/%s"}`+"\n", act, pkg, c.ID)
	}
	final := "pass"
	if any {
		final = "fail"
	}
	fmt.Fprintf(&b, `{"Action":"%s","Package":"%s","Test":"TestRun"}`+"\n", final, pkg)
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	fmt.Fprintf(&b, `{"Action":"%s","Package":"%s"}`+"\n", final, pkg)
	return []byte(b.String())
}

func TestDevLightRegression(t *testing.T) {
	t.Parallel()
	exit := func(code int) ExecRecord { return ExecRecord{Started: true, ExitCode: code} }
	ws := []string{lightWhitespaceCase}
	for name, c := range map[string]struct {
		rec    ExecRecord
		stream []byte
		red    string
	}{
		"valid_red":          {exit(1), redStream(ws), ""},
		"varied_diagnostic":  {exit(1), redStream(ws, `{"Action":"output","Package":"example.com/greeting","Test":"TestRun/whitespace","Output":"    any wording at all\n"}`), ""},
		"false_red":          {exit(0), redStream(nil), CodeRegressionNotRed},
		"passing_leaf_exit1": {exit(1), redStream(nil), CodeRegressionNotRed},
		"other_failure":      {exit(1), redStream([]string{lightWhitespaceCase, "name"}), CodeRegressionInvalidRed},
		// The regression passes on the seed (another leaf fails): not red.
		"only_other": {exit(1), redStream([]string{"name"}), CodeRegressionNotRed},
		"build_failure": {exit(1), []byte(`{"ImportPath":"example.com/greeting","Action":"build-fail"}` + "\n" + `{"Action":"fail","Package":"example.com/greeting","FailedBuild":"example.com/greeting"}` + "\n"),
			CodeRegressionInvalidRed},
		"panic":     {exit(1), redStream(ws, `{"Action":"output","Package":"example.com/greeting","Test":"TestRun/whitespace","Output":"panic: boom\n"}`), CodeRegressionInvalidRed},
		"timeout":   {exit(1), redStream(ws, `{"Action":"output","Package":"example.com/greeting","Output":"panic: test timed out after 1s\n"}`), CodeRegressionInvalidRed},
		"skip":      {exit(1), redStream(ws, `{"Action":"skip","Package":"example.com/greeting","Test":"TestRun/extra"}`), CodeRegressionInvalidRed},
		"duplicate": {exit(1), redStream(ws, `{"Action":"fail","Package":"example.com/greeting","Test":"TestRun/whitespace"}`), CodeRegressionInvalidRed},
		"missing_leaf": {exit(1), bytes.Replace(redStream(ws), []byte(`{"Action":"pass","Package":"example.com/greeting","Test":"TestRun/name"}`+"\n"), nil, 1),
			CodeRegressionInvalidRed},
		"malformed":           {exit(1), redStream(ws, `not json`), CodeRegressionInvalidRed},
		"signal":              {ExecRecord{Started: true, ExitCode: 1, Signal: "SIGKILL"}, redStream(ws), CodeRegressionInvalidRed},
		"truncated":           {ExecRecord{Started: true, ExitCode: 1, Truncated: true}, redStream(ws), CodeRegressionInvalidRed},
		"timed_out":           {ExecRecord{Started: true, ExitCode: 1, TimedOut: true}, redStream(ws), CodeRegressionInvalidRed},
		"forged_stdout_label": {exit(1), []byte("RED: TestRun/whitespace failed as expected\n"), CodeRegressionInvalidRed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := judgeRed(c.rec, lightTestEventsOf(c.stream)); got != c.red {
				t.Fatalf("red %q, want %q (%+v)", got, c.red, lightTestEventsOf(c.stream))
			}
		})
	}
	t.Run("green", func(t *testing.T) {
		t.Parallel()
		if got := judgeGreen(exit(0), lightTestEventsOf(redStream(nil))); got != "" {
			t.Fatalf("green %q", got)
		}
		for name, c := range map[string]struct {
			rec    ExecRecord
			stream []byte
		}{
			"failing": {exit(1), redStream(ws)}, "exit_mismatch": {exit(1), redStream(nil)},
			"missing_whitespace": {exit(0), bytes.Replace(redStream(nil), []byte(`{"Action":"pass","Package":"example.com/greeting","Test":"TestRun/whitespace"}`+"\n"), nil, 1)},
			"skip":               {exit(0), redStream(nil, `{"Action":"skip","Package":"example.com/greeting","Test":"TestRun/extra"}`)},
		} {
			if got := judgeGreen(c.rec, lightTestEventsOf(c.stream)); got != CodeRegressionNotGreen {
				t.Errorf("%s: green %q", name, got)
			}
		}
	})
	t.Run("record_verdict", func(t *testing.T) {
		t.Parallel()
		seed := lightFixture(t)
		cand := lightGoodCandidate(t, seed)
		before, after := lightOverlays(seed, cand)
		good := DevLightRegression{TestSHA256: sha256Hex(cand["main_test.go"]), Before: lightInputs(before), BeforeAfter: lightInputs(before),
			After: lightInputs(after), AfterAfter: lightInputs(after), Red: exit(1), Green: exit(0), RedEvents: lightTestEventsOf(redStream(ws)),
			GreenEvents: lightTestEventsOf(redStream(nil)), Structure: lightTestStructureOf(seed["main_test.go"], cand["main_test.go"]), SeedCLI: true}
		if got := lightRegressionVerdict(good); got != "" {
			t.Fatalf("the valid record: %q (%+v)", got, good.Structure)
		}
		for name, c := range map[string]struct {
			mut  func(r *DevLightRegression)
			code string
		}{
			"changed_overlay": {func(r *DevLightRegression) { r.BeforeAfter = lightInputs(after) }, CodeRegressionInputs},
			"different_test":  {func(r *DevLightRegression) { r.TestSHA256 = sha256Hex([]byte("x")) }, CodeRegressionInputs},
			"structure": {func(r *DevLightRegression) {
				r.Structure = lightTestStructureOf(seed["main_test.go"], seed["main_test.go"])
			}, CodeRegressionStructure},
			"uncorroborated":  {func(r *DevLightRegression) { r.SeedCLI = false }, CodeRegressionInvalidRed},
			"red_not_red":     {func(r *DevLightRegression) { r.Red, r.RedEvents = exit(0), lightTestEventsOf(redStream(nil)) }, CodeRegressionNotRed},
			"green_not_green": {func(r *DevLightRegression) { r.Green, r.GreenEvents = exit(1), lightTestEventsOf(redStream(ws)) }, CodeRegressionNotGreen},
		} {
			r := good
			c.mut(&r)
			if got := lightRegressionVerdict(r); got != c.code {
				t.Errorf("%s: %q, want %q", name, got, c.code)
			}
		}
	})
	t.Run("structure", func(t *testing.T) {
		t.Parallel()
		seed := lightFixture(t)
		cand := lightGoodCandidate(t, seed)
		if s := lightTestStructureOf(seed["main_test.go"], cand["main_test.go"]); !s.ok() {
			t.Fatalf("the known-good test %+v", s)
		}
		dropped := strings.Replace(string(cand["main_test.go"]), "\t\t{\"name\"", "\t\t// {\"name\"", 1)
		if dropped == string(cand["main_test.go"]) {
			t.Fatal("the name row is not in the known-good test")
		}
		weakened := strings.Replace(string(cand["main_test.go"]), `"Hello,  !\n"`, `"Hello, !\n"`, 1)
		for name, src := range map[string]string{"dropped_seed_row": dropped, "weakened_whitespace": weakened, "not_go": "package"} {
			if s := lightTestStructureOf(seed["main_test.go"], []byte(src)); s.ok() {
				t.Errorf("%s: structure accepted", name)
			}
		}
	})
}

func TestDevLightCoverage(t *testing.T) {
	t.Parallel()
	cov := func(covered, total int) LightCoverage { return LightCoverage{Covered: covered, Total: total} }
	for name, c := range map[string]struct {
		base, cand LightCoverage
		code       string
	}{
		"equal":            {cov(9, 10), cov(18, 20), ""},
		"increased":        {cov(9, 10), cov(10, 10), ""},
		"decreased":        {cov(9, 10), cov(8, 10), CodeCoverageDecreased},
		"rounding_trap":    {cov(2, 3), cov(666666, 1000000), CodeCoverageDecreased},
		"rounding_equal":   {cov(1, 3), cov(333333, 999999), ""},
		"smaller_denom":    {cov(9, 10), cov(9, 9), ""},
		"zero_base":        {cov(0, 0), cov(1, 1), CodeCoverageUnobserved},
		"zero_candidate":   {cov(1, 1), cov(0, 0), CodeCoverageUnobserved},
		"missing_base":     {LightCoverage{}, cov(1, 1), CodeCoverageUnobserved},
		"negative":         {cov(-1, 1), cov(1, 1), CodeCoverageUnobserved},
		"overflow_safe":    {cov(math.MaxInt-1, math.MaxInt), cov(math.MaxInt-2, math.MaxInt-1), CodeCoverageDecreased},
		"overflow_equal":   {cov(math.MaxInt, math.MaxInt), cov(math.MaxInt-1, math.MaxInt-1), ""},
		"covered_gt_total": {cov(11, 10), cov(10, 10), CodeCoverageUnobserved},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := compareLightCoverage(c.base, c.cand)
			if got.Code != c.code || got.Pass != (c.code == "") {
				t.Fatalf("comparison %+v, want %q", got, c.code)
			}
			if c.base.Total > 0 && c.cand.Total > 0 && c.base.Covered >= 0 && c.base.Covered <= c.base.Total && c.cand.Covered <= c.cand.Total {
				l := new(big.Int).Mul(big.NewInt(int64(c.cand.Covered)), big.NewInt(int64(c.base.Total)))
				r := new(big.Int).Mul(big.NewInt(int64(c.base.Covered)), big.NewInt(int64(c.cand.Total)))
				if (l.Cmp(r) >= 0) != got.Pass {
					t.Fatal("the verdict is not the exact integer comparison")
				}
			}
		})
	}
	t.Run("profiles", func(t *testing.T) {
		t.Parallel()
		seed := lightFixture(t)
		main := seed["main.go"]
		good := synthProfile("example.com/greeting", main)
		if c, err := deriveWholeCoverage(main, good, "example.com/greeting"); err != nil || c.Total == 0 || c.Covered > c.Total {
			t.Fatalf("a valid profile: %+v %v", c, err)
		}
		lines := strings.SplitN(string(good), "\n", 3)
		for name, p := range map[string]string{
			"bad_mode":           strings.Replace(string(good), "mode: set", "mode: count", 1),
			"other_path":         strings.ReplaceAll(string(good), "example.com/greeting/main.go", "example.com/greeting/other.go"),
			"bad_span":           lines[0] + "\nexample.com/greeting/main.go:999.1,1000.2 1 1\n",
			"disagreeing_repeat": lines[0] + "\n" + lines[1] + "\n" + lines[1][:len(lines[1])-1] + "9\n",
			"empty":              "",
			"wrong_module":       strings.ReplaceAll(string(good), "example.com/greeting/", "example.com/other/"),
		} {
			if _, err := deriveWholeCoverage(main, []byte(p), "example.com/greeting"); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		// A profile of other source bytes (a substituted profile) does not
		// validate against the recorded source.
		other := []byte(strings.Replace(string(main), "func main() {", "func main() {\n\tif true {\n\t}\n", 1))
		if _, err := deriveWholeCoverage(main, synthProfile("example.com/greeting", other), "example.com/greeting"); err == nil {
			t.Error("a substituted profile validated against the recorded source")
		}
	})
}
