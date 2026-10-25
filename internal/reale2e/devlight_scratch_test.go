package reale2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Tests of design 12a-light-triage-scratch: one workspace-local scratch
// contract on every light triage path (FP-1) and its cleanup with the
// unchanged report-only publication boundary (FP-2). The expected text is
// a set of independent literals, never derived from the production
// constants. No test reads design/, owner configuration or a vendor.

// The design's exact compact fields (Scratch envelope, with the
// post-final DS1 runbook pointer) and the baseline lines they keep.
const (
	wantScratchRecipe      = "test -d .callsheet-triage-scratch && test ! -L .callsheet-triage-scratch && chmod -R u+w .callsheet-triage-scratch && rm -r .callsheet-triage-scratch && test ! -e .callsheet-triage-scratch"
	wantScratchPolicy      = "scratch_policy: " + wantScratchRecipe
	wantScratchEvidence    = "evidence_location: none; see triage-runbook.md"
	wantScratchPreserved   = "preserved_inputs: unchanged; add no other path"
	wantProbeAllowed       = "allowed_outputs: rca.md and fix.md; source, tests and configuration are read-only"
	wantLightInvestigation = "investigation: find the cause of the light request in the seed; source, tests and configuration are read-only for triage"
	wantProbeInvestigation = "investigation: find the cause of the light request in the seed"
	// wantTriageAcceptance is the baseline light triage acceptance, byte
	// for byte (it stays unchanged).
	wantTriageAcceptance = "Publish rca.md; publish fix.md only at CONFIRMED or PROBABLE, with exactly one Design required: yes or Design required: no line, one matching Confidence: CONFIRMED or Confidence: PROBABLE line and the one root_cause reference line. Source, tests and configuration are read-only. End with exactly one of STATUS: TRIAGE_CONFIRMED, STATUS: TRIAGE_PROBABLE, STATUS: TRIAGE_UNCONFIRMED or STATUS: TRIAGE_BLOCKED."
	// baselinePreserved is the unchanged non-triage light preservation
	// line; baselineFullTriageEvidence the unchanged full-flow triage
	// evidence location.
	baselinePreserved          = "preserved_inputs: every other path stays byte-identical with the same mode; add or delete no other path; FEATURE.md and go.mod never change"
	baselineFullTriageEvidence = "evidence_location: none: embed bounded decisive excerpts in rca.md and fix.md (no evidence directory in this fixture)"
	scratchName                = ".callsheet-triage-scratch"
)

// checkScratchEnvelope checks one light triage envelope ("initial",
// "recovery" or "probe") for the literal scratch contract, its exact
// splice and single-LF separators, and the unchanged acceptance.
func checkScratchEnvelope(goal, acc, kind string) error {
	if acc != wantTriageAcceptance {
		return fmt.Errorf("%s: the acceptance changed: %q", kind, acc)
	}
	for _, l := range []string{wantScratchPolicy, wantScratchEvidence, wantScratchPreserved} {
		if n := strings.Count(goal, "\n"+l+"\n"); n != 1 {
			return fmt.Errorf("%s: the line %.48q occurs %d times", kind, l, n)
		}
	}
	if n := strings.Count(goal, "scratch_policy:"); n != 1 {
		return fmt.Errorf("%s: %d scratch_policy fields", kind, n)
	}
	// The root-directory and symlink guards precede chmod, the non-force
	// removal and the absence check follow it, in one && chain.
	pol := goalField(goal, "scratch_policy")
	parts := strings.Split(pol, " && ")
	if !slices.Equal(parts, []string{"test -d " + scratchName, "test ! -L " + scratchName, "chmod -R u+w " + scratchName, "rm -r " + scratchName, "test ! -e " + scratchName}) {
		return fmt.Errorf("%s: the removal recipe is %q", kind, pol)
	}
	for _, gone := range []string{"every other path stays byte-identical", "add or delete no other path", "no evidence directory in this fixture", "/tmp", "rm -rf", "rm -fr"} {
		if strings.Contains(goal, gone) {
			return fmt.Errorf("%s: the goal still says %q", kind, gone)
		}
	}
	switch kind {
	case "initial":
		splice := "\n" + wantScratchEvidence + "\n" + wantScratchPolicy + "\n" + wantLightInvestigation + "\n"
		if strings.Count(goal, splice) != 1 {
			return fmt.Errorf("initial: scratch_policy is not spliced between evidence_location and investigation")
		}
	case "recovery":
		if strings.Count(goal, "\n"+wantScratchEvidence+"\nfindings: ") != 1 {
			return fmt.Errorf("recovery: the findings block does not follow evidence_location")
		}
		re := regexp.MustCompile(`\n~{3,}\n` + regexp.QuoteMeta(wantScratchPolicy+"\n"+wantLightInvestigation+"\n"))
		if len(re.FindAllStringIndex(goal, -1)) != 1 {
			return fmt.Errorf("recovery: scratch_policy does not follow the closing findings fence before investigation")
		}
	case "probe":
		splice := "\n" + wantProbeAllowed + "\n" + wantScratchPreserved + "\n" + wantScratchEvidence + "\n" + wantScratchPolicy + "\n" + wantProbeInvestigation + "\n"
		if strings.Count(goal, splice) != 1 {
			return fmt.Errorf("probe: the allowed_outputs, preserved_inputs, evidence_location and scratch_policy lines are not spliced before investigation")
		}
		return nil
	default:
		return fmt.Errorf("unknown kind %s", kind)
	}
	// Real triage: the preservation line replaces the baseline one in
	// place, between allowed_outputs and commands.
	re := regexp.MustCompile(`\nallowed_outputs: [^\n]+\n` + regexp.QuoteMeta(wantScratchPreserved) + `\ncommands: `)
	if len(re.FindAllStringIndex(goal, -1)) != 1 {
		return fmt.Errorf("%s: preserved_inputs is not the line after allowed_outputs", kind)
	}
	return nil
}

// checkNoScratch checks a non-triage (or full-flow) envelope carries no
// scratch contract.
func checkNoScratch(goal string) error {
	for _, l := range []string{"scratch_policy", scratchName, wantScratchPreserved, wantScratchEvidence} {
		if strings.Contains(goal, l) {
			return fmt.Errorf("a non-light-triage envelope says %q", l)
		}
	}
	return nil
}

// triageKind is a dispatched light triage document's kind.
func triageKind(doc devDispatchDoc) string {
	switch goalField(doc.Goal, "reason") {
	case "":
		return "probe"
	case "initial":
		return "initial"
	}
	return "recovery"
}

// checkRigScratch checks every dispatched document of a rig: each triage
// against the scratch contract, as it reached the plane, and every other
// role without one. It returns the triage kinds in order.
func checkRigScratch(t *testing.T, docs []devDispatchDoc, plane *fakePlane) []string {
	t.Helper()
	var kinds []string
	requests := map[string]string{}
	for _, id := range plane.ids() {
		v, err := plane.ShowTask(context.Background(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		requests[v.Request.Goal] = v.Request.Acceptance
	}
	for _, d := range docs {
		if d.Role != "triage" {
			if err := checkNoScratch(d.Goal); err != nil {
				t.Errorf("%s ordinal %d: %v", d.Role, d.Ordinal, err)
			}
			continue
		}
		kind := triageKind(d)
		kinds = append(kinds, kind)
		if err := checkScratchEnvelope(d.Goal, d.Acceptance, kind); err != nil {
			t.Errorf("ordinal %d: %v", d.Ordinal, err)
		}
		if acc, ok := requests[d.Goal]; !ok || acc != d.Acceptance {
			t.Errorf("ordinal %d: the dispatched task is not the prepared envelope", d.Ordinal)
		}
	}
	return kinds
}

// scratchBudgetFixture is the measured sizing fixture (design
// 12a-light-triage-scratch, Measured byte budget): the shipped light
// scenario, full-length IDs and digests, a diagnostic recovery with eight
// artifacts, five recovery inputs and a fenced 1050-byte coder final.
func scratchBudgetFixture(t testing.TB) (envelopeContext, TaskPlan, map[string]TreeFile, []PlanInput) {
	t.Helper()
	scenario := mustRead(t, filepath.Join(repoRoot(t), "docs", "examples", "real-e2e", "dev-full", "scenario-light.md"))
	if len(scenario) != 1743 {
		t.Fatalf("the light scenario is %d bytes, not the measured 1743", len(scenario))
	}
	hash, task := strings.Repeat("a", 40), "t_"+strings.Repeat("b", 32)
	final := strings.Repeat("x", 1024) + "\nSTATUS: DESIGN_QUESTIONS\n"
	ec := envelopeContext{runID: "20261025T000000Z-01020304", intake: strings.Repeat("e", 64), scenario: scenario,
		state: DevFullState{InitialCommit: hash, NeedsTriage: &Outcome{TaskID: task, Commit: hash, Marker: markerQuestions, Final: final}}}
	p := TaskPlan{ExpectedBase: hash, Mode: modeInvestigate, OriginalMode: modeDefect, Reason: "questions", Role: "triage", Scenario: scenarioLight,
		RecoveryRound: 2, Ordinal: 7, Round: 2, Diagnostic: true, Allowed: []string{"rca.md", "fix.md (TRIAGE_CONFIRMED or TRIAGE_PROBABLE only)"}}
	base := map[string]TreeFile{}
	for _, n := range []string{"FEATURE.md", "go.mod", "main.go", "main_test.go", "rca.md", "fix.md", "review.md", "design-questions.md"} {
		base[n] = TreeFile{Path: n, Mode: filemode.Regular, Content: []byte("synthetic\n")}
	}
	var inputs []PlanInput
	for i, kind := range []string{"findings", "questions", "review", "prior-brief", "prior-rca"} {
		inputs = append(inputs, PlanInput{Kind: kind, Task: task, Commit: hash, Path: []string{"", "design-questions.md", "review.md", "fix.md", "rca.md"}[i],
			SHA256: strings.Repeat("c", 64), Ordinal: i + 1})
	}
	return ec, p, base, inputs
}

// envelopeSize is goal plus acceptance, raw and JSON-encoded content.
func envelopeSize(t testing.TB, goal, acc string) (int, int) {
	t.Helper()
	eg, err := jsonContentLen([]byte(goal))
	if err != nil {
		t.Fatal(err)
	}
	ea, err := jsonContentLen([]byte(acc))
	if err != nil {
		t.Fatal(err)
	}
	return len(goal) + len(acc), eg + ea
}

// checkScratchBudgets pins the rendered counts of every measured workflow
// fixture (post-final DS1: worst 7879 raw / 7938 encoded), the 200-byte
// encoded reserve, the admitted boundary and the overflow refusals.
func checkScratchBudgets(t *testing.T) {
	t.Helper()
	ec, p, base, inputs := scratchBudgetFixture(t)
	measure := func(name, goal, acc, kind string, err error, raw, enc int) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if kind != "" {
			if e := checkScratchEnvelope(goal, acc, kind); e != nil {
				t.Errorf("%s: %v", name, e)
			}
		}
		gr, ge := envelopeSize(t, goal, acc)
		if gr != raw || ge != enc {
			t.Errorf("%s: %d raw / %d encoded bytes, want %d / %d", name, gr, ge, raw, enc)
		}
		if triageTaskCeiling-ge < 200 {
			t.Errorf("%s: %d encoded bytes of headroom, under the 200-byte reserve", name, triageTaskCeiling-ge)
		}
		if err := checkTriageTaskBudget(goal, acc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	goal, acc, err := buildLightEnvelope(ec, &p, base, inputs)
	measure("recovery/questions", goal, acc, "recovery", err, 7879, 7938)
	if triageTaskCeiling-7879 != 313 || triageTaskCeiling-7938 != 254 {
		t.Fatal("the worst fixture's headroom is not 313 / 254")
	}
	// The admitted boundary: 254 more ASCII bytes in the recovery final
	// reach exactly 8192 encoded; one more is refused before exposure.
	worst := ec.state.NeedsTriage.Final
	ec.state.NeedsTriage.Final = strings.Repeat("x", 254) + worst
	goal, acc, err = buildLightEnvelope(ec, &p, base, inputs)
	if err != nil {
		t.Fatalf("the boundary was refused: %v", err)
	}
	if r, e := envelopeSize(t, goal, acc); r != 8133 || e != 8192 {
		t.Fatalf("the boundary is %d raw / %d encoded, want 8133 / 8192", r, e)
	}
	for name, final := range map[string]string{
		"boundary+1": strings.Repeat("x", 255) + worst,
		// Escape-heavy findings fit raw but not encoded.
		"escape_heavy": strings.Repeat(`"`, 200) + worst,
		"raw_overflow": strings.Repeat("x", 314) + worst,
	} {
		ec.state.NeedsTriage.Final = final
		if g, a, err := buildLightEnvelope(ec, &p, base, inputs); err == nil || codeOf(err) != CodePromptBudget || g != "" || a != "" {
			t.Errorf("%s: %v (goal %d bytes)", name, err, len(g))
		}
	}
	if r := len(strings.Repeat(`"`, 200)) + 7879; r > triageTaskCeiling {
		t.Fatalf("the escape-heavy case is not raw-admissible (%d)", r)
	}
	// NEEDS_TRIAGE recovery: its own marker and final, four inputs.
	ec.state.NeedsTriage.Final = strings.Repeat("x", 1024) + "\nSTATUS: NEEDS_TRIAGE\n"
	ec.state.NeedsTriage.Marker, p.Reason = markerNeedsTriage, "needs_triage"
	four := append(slices.Clone(inputs[:1]), inputs[2:]...)
	goal, acc, err = buildLightEnvelope(ec, &p, base, four)
	measure("recovery/needs_triage", goal, acc, "recovery", err, 7671, 7729)
	// Initial: four seed artifacts, no findings or inputs.
	ec.state.NeedsTriage = nil
	p.Reason, p.RecoveryRound, p.Ordinal, p.Round, p.Diagnostic = "initial", 0, 1, 0, false
	seedOnly := map[string]TreeFile{}
	for _, n := range []string{"FEATURE.md", "go.mod", "main.go", "main_test.go"} {
		seedOnly[n] = base[n]
	}
	goal, acc, err = buildLightEnvelope(ec, &p, seedOnly, nil)
	measure("initial", goal, acc, "initial", err, 5323, 5368)
	goal, acc = probeTriageEnvelope(ec.runID, strings.Repeat("a", 40))
	measure("probe", goal, acc, "probe", nil, 2250, 2262)
}

func TestLightTriageScratchEnvelope(t *testing.T) {
	t.Parallel()
	seed := lightSeedTree(t)
	ec := envelopeContext{runID: "20261025T000000Z-01020304", trees: map[string]map[string]TreeFile{"s": seed}, scenario: []byte("scenario\n"),
		intake: strings.Repeat("e", 64)}
	t.Run("shared_rendering", func(t *testing.T) {
		t.Parallel()
		// Initial, both recoveries and the probe render the same fields.
		st, step := InitialPlanFor(scenarioLight, "s")
		ec := ec
		ec.state = st
		goal, acc, _, err := buildEnvelope(ec, step.Plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkScratchEnvelope(goal, acc, "initial"); err != nil {
			t.Error(err)
		}
		for _, c := range []struct{ marker, reason string }{{markerQuestions, "questions"}, {markerNeedsTriage, "needs_triage"}} {
			p := *step.Plan
			p.Reason = c.reason
			ec.state.NeedsTriage = &Outcome{TaskID: hexID("t_", 2), Commit: "c", Marker: c.marker, Final: "~~~~\nWhy?\nSTATUS: " + c.marker + "\n"}
			goal, acc, _, err := buildEnvelope(ec, &p)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkScratchEnvelope(goal, acc, "recovery"); err != nil {
				t.Errorf("%s: %v", c.reason, err)
			}
			// The worker's findings stay data: the splice follows the
			// renderer's own (longer) fence.
			if !strings.Contains(goal, "~~~~~\n~~~~\nWhy?\nSTATUS: "+c.marker+"\n~~~~~\n"+wantScratchPolicy+"\n") {
				t.Errorf("%s: the findings fence is not followed by scratch_policy", c.reason)
			}
		}
		pg, pa := probeTriageEnvelope(ec.runID, strings.Repeat("a", 40))
		if err := checkScratchEnvelope(pg, pa, "probe"); err != nil {
			t.Error(err)
		}
		// Rendering is pure: no host path, task ID or filesystem input.
		again, _ := probeTriageEnvelope(ec.runID, strings.Repeat("a", 40))
		if again != pg || strings.Contains(pg, string(filepath.Separator)+"tmp") {
			t.Fatal("the probe envelope is not deterministic")
		}
	})
	t.Run("budgets", func(t *testing.T) {
		t.Parallel()
		checkScratchBudgets(t)
	})
	t.Run("non_triage_unchanged", func(t *testing.T) {
		t.Parallel()
		conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
		_, _, plans := lightSteps(t, conf, devAct{marker: markerImplComplete}, devAct{marker: markerReviewChanges})
		ec := ec
		ec.state = DevFullState{InitialCommit: "s", Triage: &Outcome{TaskID: hexID("t_", 1), Commit: "t"}, CandidateCommit: "c", CandidateTask: hexID("t_", 2),
			LastReview: &Outcome{TaskID: hexID("t_", 3), Commit: "r"}}
		var roles []string
		for _, p := range plans[1:] {
			roles = append(roles, p.Role+"/"+p.Mode)
			goal, _, err := buildLightEnvelope(ec, &p, seed, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkNoScratch(goal); err != nil {
				t.Errorf("%s/%s: %v", p.Role, p.Mode, err)
			}
			if strings.Count(goal, "\n"+baselinePreserved+"\n") != 1 {
				t.Errorf("%s/%s: the preservation line changed", p.Role, p.Mode)
			}
		}
		if !slices.Equal(roles, []string{"coder/defect", "code-reviewer/light", "coder/fix"}) {
			t.Fatalf("roles %v", roles)
		}
		// The full flow's triage keeps its envelope (a full-flow scratch
		// envelope is a separate design).
		full := envelopeContext{runID: ec.runID, trees: ec.trees, scenario: ec.scenario,
			state: DevFullState{InitialCommit: "s", NeedsTriage: &Outcome{TaskID: hexID("t_", 4), Commit: "c", Marker: markerNeedsTriage, Final: "F1\nSTATUS: NEEDS_TRIAGE\n"}}}
		goal, _, _, err := buildEnvelope(full, &TaskPlan{Role: "triage", Mode: modeInvestigate, Reason: "needs_triage", ExpectedBase: "s", Allowed: []string{"rca.md", "fix.md"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := checkNoScratch(goal); err != nil || strings.Count(goal, "\n"+baselineFullTriageEvidence+"\n") != 1 {
			t.Errorf("the full-flow triage envelope changed: %v", err)
		}
	})
	t.Run("allowed_outputs", func(t *testing.T) {
		t.Parallel()
		// Scratch is never an allowed output of any light plan or the
		// probe, and admission has no scratch exemption.
		conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
		_, _, plans := lightSteps(t, conf, devAct{marker: markerQuestions}, conf)
		for _, p := range plans {
			for _, a := range p.Allowed {
				if strings.Contains(a, "scratch") {
					t.Errorf("%s ordinal %d allows %q", p.Role, p.Ordinal, a)
				}
			}
		}
		for _, m := range []string{markerTriageConf, markerTriageProb, markerTriageUnconf, markerTriageBlocked} {
			if allowed := devAllowedChanges("triage", m); allowed[scratchName] || len(allowed) > 2 {
				t.Errorf("%s: allowed %v", m, allowed)
			}
		}
	})
}

func TestLightTriageScratchAdmission(t *testing.T) {
	t.Parallel()
	seed := lightSeedTree(t)
	root := fmt.Sprintf("Root cause: main.go:%d", lightDefectOf(seed["main.go"].Content).Line)
	rca := func(marker string) string {
		return "# RCA\n\nThe trimmed check refuses whitespace.\n\nSTATUS: " + marker + "\n"
	}
	reports := func(marker string) map[string]TreeFile {
		out := withTree(seed, map[string]string{"rca.md": rca(marker)})
		if marker == markerTriageConf || marker == markerTriageProb {
			out = withTree(out, map[string]string{"fix.md": brief(strings.TrimPrefix(marker, "TRIAGE_"), "no", root)})
		}
		return out
	}
	admit := func(marker string, result []TreeFile) error {
		_, err := AdmitPublication(admitInput{Role: "triage", Mode: modeInvestigate, Marker: marker, Base: treeList(seed), Result: result,
			Final: "Done.\nSTATUS: " + marker + "\n", TaskID: hexID("t_", 5), Scenario: scenarioLight, Seed: seed})
		return err
	}
	for _, m := range []string{markerTriageConf, markerTriageProb, markerTriageUnconf, markerTriageBlocked} {
		t.Run("control_"+m, func(t *testing.T) {
			t.Parallel()
			// A clean report-only result (scratch removed) is admitted.
			if err := admit(m, treeList(reports(m))); err != nil {
				t.Fatalf("a clean report-only publication: %v", err)
			}
		})
	}
	file := func(path string, mode filemode.FileMode, content string) TreeFile {
		return TreeFile{Path: path, Mode: mode, Content: []byte(content)}
	}
	for name, c := range map[string]struct {
		marker string
		extra  []TreeFile
		edit   func(map[string]TreeFile)
	}{
		"scratch_child":  {markerTriageConf, []TreeFile{file(scratchName+"/raw/evidence.txt", filemode.Regular, "raw\n")}, nil},
		"scratch_copy":   {markerTriageProb, []TreeFile{file(scratchName+"/copy/main.go", filemode.Regular, "package main\n")}, nil},
		"dotted_root":    {markerTriageConf, []TreeFile{file(scratchName, filemode.Regular, "left\n")}, nil},
		"other_dotted":   {markerTriageUnconf, []TreeFile{file(".triage", filemode.Regular, "left\n")}, nil},
		"unrelated_file": {markerTriageConf, []TreeFile{file("notes.txt", filemode.Regular, "x\n")}, nil},
		"source_change": {markerTriageConf, nil, func(m map[string]TreeFile) {
			m["main_test.go"] = file("main_test.go", filemode.Regular, "package main\n")
		}},
		"mode_change": {markerTriageConf, nil, func(m map[string]TreeFile) {
			f := m["main.go"]
			f.Mode = filemode.Executable
			m["main.go"] = f
		}},
		"unqualified_new_fix": {markerTriageBlocked, nil, func(m map[string]TreeFile) {
			m["fix.md"] = file("fix.md", filemode.Regular, brief("PROBABLE", "no", root))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tree := reports(c.marker)
			if c.edit != nil {
				c.edit(tree)
			}
			if err := admit(c.marker, append(treeList(tree), c.extra...)); err == nil {
				t.Fatal("the publication was admitted")
			}
		})
	}
	t.Run("commit_tree", func(t *testing.T) {
		t.Parallel()
		// An actual commit holding leftover scratch has a root directory
		// (or a root symlink) entry: reading it refuses the publication
		// before admission, whatever the directory's name.
		st := memory.NewStorage()
		files := map[string][]byte{}
		for n, f := range seed {
			files[n] = f.Content
		}
		repo, err := git.Init(st, nil)
		if err != nil {
			t.Fatal(err)
		}
		parent, _ := writeCommitMode(t, st, files, nil)
		clean := map[string][]byte{"rca.md": []byte(rca(markerTriageUnconf))}
		for n, b := range files {
			clean[n] = b
		}
		h := writeNestedCommit(t, st, clean, parent)
		if got, _, _, err := readCommitTree(repo, h.String()); err != nil || len(got) != len(clean) {
			t.Fatalf("a clean report-only commit: %v", err)
		}
		for _, dir := range []string{scratchName + "/raw/evidence.txt", "elsewhere/raw/evidence.txt"} {
			left := map[string][]byte{dir: []byte("raw\n")}
			for n, b := range clean {
				left[n] = b
			}
			h := writeNestedCommit(t, st, left, parent)
			if _, _, _, err := readCommitTree(repo, h.String()); err == nil || !strings.Contains(err.Error(), "is not a regular nonexecutable file") {
				t.Errorf("%s: %v", dir, err)
			}
		}
		link := map[string][]byte{scratchName: []byte("/tmp/elsewhere")}
		for n, b := range clean {
			link[n] = b
		}
		lh, _ := writeCommitMode(t, st, link, map[string]filemode.FileMode{scratchName: filemode.Symlink}, parent)
		if _, _, _, err := readCommitTree(repo, lh.String()); err == nil {
			t.Error("a scratch symlink was read as a publication")
		}
	})
	t.Run("recipe_and_runbook", func(t *testing.T) {
		t.Parallel()
		// The goal carries the literal removal recipe, the shipped runbook
		// the mandatory pre-answer absence; acceptance stays unchanged.
		st, step := InitialPlanFor(scenarioLight, "s")
		goal, acc, _, err := buildEnvelope(envelopeContext{runID: "r", state: st, trees: map[string]map[string]TreeFile{"s": seed}, scenario: []byte("s\n")}, step.Plan)
		if err != nil {
			t.Fatal(err)
		}
		if goalField(goal, "scratch_policy") != wantScratchRecipe || acc != wantTriageAcceptance {
			t.Fatalf("scratch_policy %q", goalField(goal, "scratch_policy"))
		}
		runbook := string(mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(DevFlowDir), "triage-runbook.md")))
		for _, want := range []string{"Scratch must be absent before the final answer.", wantScratchRecipe} {
			if !strings.Contains(runbook, want) {
				t.Errorf("the runbook lacks %q", want)
			}
		}
	})
}

// runRecipe runs the recipe with sh in dir.
func runRecipe(t *testing.T, dir, recipe string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", recipe)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("recipe: %v %s", err, out)
	}
	return err
}

// unlockTree makes every directory under dir writable again (the test's
// own cleanup of read-only fixtures; never the recipe under test).
func unlockTree(t *testing.T, dir string) {
	t.Cleanup(func() {
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}

func TestLightScratchFP1(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	impl, fixes := devAct{marker: markerImplComplete}, devAct{marker: markerFixesComplete}
	changes, approve := devAct{marker: markerReviewChanges}, devAct{marker: markerReviewApproved}
	t.Run("initial_recovery", func(t *testing.T) {
		t.Parallel()
		// Initial and both recovery reasons, re-triage included, through
		// run-dev-light and the offline check.
		for name, steps := range map[string][]devAct{
			"questions":    {conf, {marker: markerQuestions}, conf, impl, approve},
			"needs_triage": {conf, impl, changes, {marker: markerNeedsTriage}, conf, fixes, approve},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newLightRig(t, steps)
				r.expect(0, "")
				if kinds := checkRigScratch(t, r.docs, r.f.plane); !slices.Equal(kinds, []string{"initial", "recovery"}) {
					t.Fatalf("triage kinds %v", kinds)
				}
				if code, rep := r.offline(); code != 0 {
					t.Fatalf("offline %d %v", code, devCodes(rep))
				}
			})
		}
	})
	t.Run("standalone_light", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, lightHappy())
		r.expect(0, "")
		if kinds := checkRigScratch(t, r.docs, r.f.plane); !slices.Equal(kinds, []string{"initial"}) {
			t.Fatalf("triage kinds %v", kinds)
		}
		if code, rep := r.offline(); code != 0 {
			t.Fatalf("offline %d %v", code, devCodes(rep))
		}
	})
	t.Run("exercise_light_child", func(t *testing.T) {
		t.Parallel()
		x := newExerciseRig(t, happy(), lightHappy())
		if code := x.run(); code != 0 {
			t.Fatalf("exit %d: %s", code, x.full.f.stderr.String())
		}
		if kinds := checkRigScratch(t, x.light.docs, x.plane[scenarioLight]); !slices.Equal(kinds, []string{"initial"}) {
			t.Fatalf("light child triage kinds %v", kinds)
		}
		for _, d := range x.full.docs {
			if err := checkNoScratch(d.Goal); err != nil {
				t.Errorf("full child %s: %v", d.Role, err)
			}
		}
		var out, errb bytes.Buffer
		if code := CheckEvidence(x.root(), &out, &errb); code != 0 {
			t.Fatalf("root offline %d %s", code, errb.String())
		}
	})
	t.Run("probe", func(t *testing.T) {
		t.Parallel()
		r := probeLightRig(t, probeScopeBoth)
		r.expect(0, "")
		if kinds := checkRigScratch(t, r.docs, r.f.plane); !slices.Equal(kinds, []string{"probe"}) {
			t.Fatalf("triage kinds %v", kinds)
		}
		if codes := qualificationCodes(t, r.f.bundle()); len(codes) != 0 {
			t.Fatalf("codes %v", codes)
		}
	})
}

func TestLightScratchFP2(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	impl, approve := devAct{marker: markerImplComplete}, devAct{marker: markerReviewApproved}
	t.Run("envelope_recipe", func(t *testing.T) {
		t.Parallel()
		// The dispatched light and probe triage envelopes carry the literal
		// guarded removal recipe.
		seed := lightSeedTree(t)
		st, step := InitialPlanFor(scenarioLight, "s")
		goal, _, _, err := buildEnvelope(envelopeContext{runID: "r", state: st, trees: map[string]map[string]TreeFile{"s": seed}, scenario: []byte("s\n")}, step.Plan)
		if err != nil {
			t.Fatal(err)
		}
		pg, _ := probeTriageEnvelope("r", strings.Repeat("a", 40))
		for name, g := range map[string]string{"light": goal, "probe": pg} {
			if goalField(g, "scratch_policy") != wantScratchRecipe {
				t.Errorf("%s: scratch_policy %q", name, goalField(g, "scratch_policy"))
			}
		}
	})
	for name, c := range map[string]struct {
		alter, path string
	}{
		"control":        {"", ""},
		"scratch_left":   {"scratch-left", scratchName + "/raw/evidence.txt"},
		"scratch_file":   {"scratch-file", scratchName},
		"unrelated_path": {"unrelated-path", "notes.txt"},
	} {
		t.Run("light_"+name, func(t *testing.T) {
			t.Parallel()
			triage := conf
			triage.alter = c.alter
			r := newLightRig(t, []devAct{triage, impl, approve})
			if c.alter == "" {
				r.expect(0, "")
				if code, _ := r.offline(); code != 0 {
					t.Fatal("offline")
				}
				return
			}
			r.expect(1, "publication_rejected")
			t.Logf("refusal: %s", r.d.stopReason)
			// The rejected tree really held the leftover path.
			held := false
			for _, files := range r.published {
				_, ok := files[c.path]
				held = held || ok
			}
			if !held {
				t.Fatalf("no publication held %s", c.path)
			}
			for _, d := range r.docs {
				if d.Role != "triage" {
					t.Fatalf("a %s was dispatched after a rejected triage publication", d.Role)
				}
			}
			if code, rep, _ := devCheck(t, r.f.bundle()); code != 1 || rep.Result == ResultPass {
				t.Fatalf("offline %d %v", code, devCodes(rep))
			}
		})
	}
	t.Run("probe_scratch_left", func(t *testing.T) {
		t.Parallel()
		r := probeLightRig(t, probeScopeBoth)
		r.steps[4].alter = "scratch-left"
		r.expect(1, "publication_rejected")
		q := r.qualification()
		got := map[string]string{}
		for _, s := range q.Stages {
			got[s.Name] = s.Result
		}
		if q.Result != ResultBlocked || got["triage_evidence"] != stageFail || got["light_reviewer_evidence"] != stageUnreached {
			t.Fatalf("%+v", q.Stages)
		}
		var roles []string
		for _, d := range r.docs {
			roles = append(roles, d.Role)
		}
		if !slices.Equal(roles, []string{"designer", "designer", "designer", "code-reviewer", "triage"}) {
			t.Fatalf("roles %v: the light reviewer was reached", roles)
		}
		b, err := LoadDevQualification(r.f.bundle())
		if err != nil {
			t.Fatal(err)
		}
		if reloaded := doc[DevQualification](b, devQualificationFile); reloaded == nil || reloaded.Result != ResultBlocked {
			t.Fatal("the stored qualification does not keep the rejection")
		}
	})
	recipe := func(t *testing.T) string {
		t.Helper()
		pg, _ := probeTriageEnvelope("r", strings.Repeat("a", 40))
		got := goalField(pg, "scratch_policy")
		if got != wantScratchRecipe {
			t.Fatalf("the rendered recipe is %q", got)
		}
		return got
	}
	t.Run("cleanup_read_only_cache", func(t *testing.T) {
		t.Parallel()
		// Module-cache-like 0555 directories and 0444 files: the rendered
		// recipe removes them and verifies absence.
		co := t.TempDir()
		unlockTree(t, co)
		mod := filepath.Join(co, scratchName, "gomodcache", "example.com", "m@v1")
		if err := os.MkdirAll(mod, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{filepath.Join(mod, "go.mod"), filepath.Join(co, scratchName, "raw.txt")} {
			if err := os.WriteFile(f, []byte("module m\n"), 0o444); err != nil {
				t.Fatal(err)
			}
		}
		for _, d := range []string{mod, filepath.Dir(mod)} {
			if err := os.Chmod(d, 0o555); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(co, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runRecipe(t, co, recipe(t)); err != nil {
			t.Fatalf("the recipe failed: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(co, scratchName)); !os.IsNotExist(err) {
			t.Fatalf("scratch is still present: %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(co, "main.go")); err != nil || string(b) != "package main\n" {
			t.Fatal("the checkout's file changed")
		}
	})
	t.Run("root_symlink_refused", func(t *testing.T) {
		t.Parallel()
		// A scratch-root symlink stops the chain before chmod: the link
		// and its target's modes and content are unchanged.
		co, elsewhere := t.TempDir(), t.TempDir()
		unlockTree(t, elsewhere)
		target := filepath.Join(elsewhere, "target")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(target, "file")
		if err := os.WriteFile(f, []byte("unchanged\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o555); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(co, scratchName)); err != nil {
			t.Fatal(err)
		}
		if err := runRecipe(t, co, recipe(t)); err == nil {
			t.Fatal("the recipe followed a scratch-root symlink")
		}
		if fi, err := os.Lstat(filepath.Join(co, scratchName)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link was changed: %v", err)
		}
		di, _ := os.Stat(target)
		fi, _ := os.Stat(f)
		b, _ := os.ReadFile(f)
		if di.Mode().Perm() != 0o555 || fi.Mode().Perm() != 0o444 || string(b) != "unchanged\n" {
			t.Fatalf("the target changed: %v %v %q", di.Mode(), fi.Mode(), b)
		}
	})
	t.Run("absent_or_file_refused", func(t *testing.T) {
		t.Parallel()
		// No scratch, or a plain file at its name, fails the first guard.
		co := t.TempDir()
		if err := runRecipe(t, co, recipe(t)); err == nil {
			t.Fatal("the recipe succeeded without a scratch directory")
		}
		p := filepath.Join(co, scratchName)
		if err := os.WriteFile(p, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runRecipe(t, co, recipe(t)); err == nil {
			t.Fatal("the recipe removed a plain file")
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != "keep\n" {
			t.Fatal("the plain file changed")
		}
	})
}
