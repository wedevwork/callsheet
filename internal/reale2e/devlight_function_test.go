package reale2e

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Function tests of the light development flow (design
// 12a-dev-flow-light, Testing plan, Function tests): one per functional
// point, through the public MainWith and check entries with scripted
// vendors, plus narrowly selected actual local Go fixture runs. No test
// reads design/ or owner evidence.

func TestDevLightFP1(t *testing.T) {
	t.Parallel()
	t.Run("real_seed_baseline", func(t *testing.T) {
		t.Parallel()
		// The committed seed, run with the real toolchain, exhibits exactly
		// the whitespace defect: nine seed tests and nine unaffected CLI
		// cases pass, whitespace exits 2 with the empty-name diagnostic.
		b := lightSeedBaseline(t)
		rec := b.rec
		if !rec.Valid || !rec.Defect.Exact || rec.Defect.Exit != 2 || rec.Defect.Stdout != "" || rec.Defect.Stderr != lightWhitespaceStderr {
			t.Fatalf("baseline %+v", rec)
		}
		passed := 0
		for _, c := range rec.Cases {
			if c.Pass {
				passed++
			}
			if c.ID == lightWhitespaceCase && c.Pass {
				t.Fatal("the seed passes the whitespace case")
			}
		}
		if passed != 9 || len(rec.Tests.Pass) != 9 || len(rec.Tests.Fail) != 0 || rec.Coverage == nil {
			t.Fatalf("cases %d tests %+v", passed, rec.Tests)
		}
		// A known-good tree is not the specified bug: it can never become
		// an easier seed.
		good := runLightBaseline(realLightOptions(t, t.TempDir()), lightGoodCandidate(t, lightFixture(t)))
		if good.rec.Valid || good.rec.Code != CodeLightSeedInvalid {
			t.Fatalf("a known-good seed: %+v", good.rec)
		}
	})
	t.Run("authorized_triage", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, lightHappy())
		r.expect(0, "")
		dir := r.f.bundle()
		rec := bundleDoc[devEnvelope[DevIntakeReceipt]](t, dir, lightReceiptFile).Data
		triage := eventSeqs(t, dir, EvTaskAdmitted, "workflow-01", "")
		if len(triage) == 0 || rec.ReceiptSeq >= triage[0] {
			t.Fatalf("triage admitted at %v before the receipt %d", triage, rec.ReceiptSeq)
		}
		if code, _ := r.offline(); code != 0 {
			t.Fatal("offline")
		}
	})
	for _, intake := range []string{intakeNo, intakeEOF} {
		t.Run("refused_"+intake, func(t *testing.T) {
			t.Parallel()
			r := newLightRig(t, lightHappy())
			r.intake = intake
			r.expect(1, "owner_"+intake)
			if len(r.f.plane.ids()) != 0 || exists(r.f.bundle(), lightReceiptFile) {
				t.Fatalf("%d paid calls after %s", len(r.f.plane.ids()), intake)
			}
			if code, rep, _ := devCheck(t, r.f.bundle()); code != 1 || rep.Result == ResultPass {
				t.Fatalf("offline %d", code)
			}
		})
	}
	t.Run("seed_invalid", func(t *testing.T) {
		t.Parallel()
		// A checkout whose fixture lost the defect stops before the intake
		// with no paid call.
		r := newLightRig(t, lightHappy())
		good := mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(devProbeFixtureDir), "main.go"))
		if err := os.WriteFile(filepath.Join(r.f.checkout, filepath.FromSlash(devLightFixtureDir), "main.go"), good, 0o644); err != nil {
			t.Fatal(err)
		}
		r.expect(1, CodeLightSeedInvalid)
		if len(r.f.plane.ids()) != 0 || exists(r.f.bundle(), lightDecisionFile) {
			t.Fatal("an invalid seed reached the intake or a paid call")
		}
	})
}

func TestDevLightFP2(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	prob := devAct{marker: markerTriageProb, designRequired: designRequiredNo}
	impl, approve := devAct{marker: markerImplComplete}, devAct{marker: markerReviewApproved}
	for name, steps := range map[string][]devAct{
		"initial_confirmed":  {conf, impl, approve},
		"initial_probable":   {prob, impl, approve},
		"recovery_probable":  {conf, {marker: markerQuestions}, prob, impl, approve},
		"recovery_confirmed": {prob, {marker: markerQuestions}, conf, impl, approve},
		"lookalike_prose":    {{marker: markerTriageConf, designRequired: designRequiredNo, alter: "lookalike"}, impl, approve},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newLightRig(t, steps)
			r.expect(0, "")
			if code, _ := r.offline(); code != 0 {
				t.Fatal("offline")
			}
		})
	}
	for name, c := range map[string]struct {
		triage devAct
		stop   string
	}{
		"unconfirmed":     {devAct{marker: markerTriageUnconf}, "cause_unconfirmed"},
		"blocked":         {devAct{marker: markerTriageBlocked}, "obstacle"},
		"stale":           {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "inherited"}, "publication_rejected"},
		"no_reference":    {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "no-root"}, "publication_rejected"},
		"two_references":  {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "two-roots"}, "publication_rejected"},
		"out_of_range":    {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "root-range"}, "publication_rejected"},
		"fenced":          {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "root-fenced"}, "publication_rejected"},
		"prose_only":      {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "lookalike-only"}, "publication_rejected"},
		"other_path":      {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "root-other-path"}, "publication_rejected"},
		"unauthorized":    {devAct{marker: markerTriageConf, designRequired: designRequiredNo, alter: "triage-edits-source"}, "publication_rejected"},
		"unconfirmed_fix": {devAct{marker: markerTriageUnconf, alter: "new-fix"}, "publication_rejected"},
		"design_yes":      {devAct{marker: markerTriageConf, designRequired: designRequiredYes}, stopNeedsDesign},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newLightRig(t, []devAct{c.triage, impl, approve})
			r.expect(1, c.stop)
			for _, d := range r.docs {
				if d.Role == "coder" {
					t.Fatal("a coder was dispatched after an unqualified brief")
				}
			}
			if code, rep, _ := devCheck(t, r.f.bundle()); code != 1 || rep.Result == ResultPass {
				t.Fatalf("offline %d", code)
			}
		})
	}
	t.Run("recovery_reference_in_seed", func(t *testing.T) {
		t.Parallel()
		// A recovery triage's reference still resolves in the immutable
		// seed, not the chain commit it runs on.
		r := newLightRig(t, []devAct{conf, {marker: markerImplComplete}, {marker: markerReviewChanges}, {marker: markerNeedsTriage},
			{marker: markerTriageConf, designRequired: designRequiredNo, alter: "root-range"}})
		r.expect(1, "publication_rejected")
		if !strings.Contains(r.d.stopReason, "is not a line of the seed") {
			t.Fatalf("stop %q", r.d.stopReason)
		}
	})
}

func TestDevLightFP3(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	impl, fixes := devAct{marker: markerImplComplete}, devAct{marker: markerFixesComplete}
	changes, approve := devAct{marker: markerReviewChanges}, devAct{marker: markerReviewApproved}
	for name, c := range map[string]struct {
		steps []devAct
		code  int
		stop  string
		tasks int
	}{
		"happy":         {lightHappy(), 0, "", 3},
		"two_fix_loops": {[]devAct{conf, impl, changes, fixes, changes, fixes, approve}, 0, "", 7},
		"needs_triage":  {[]devAct{conf, impl, changes, {marker: markerNeedsTriage}, conf, fixes, approve}, 0, "", 7},
		"questions":     {[]devAct{conf, {marker: markerQuestions}, conf, impl, approve}, 0, "", 5},
		"cap_exhausted": {[]devAct{conf, impl, changes, fixes, changes, fixes, changes}, 1, stopCap, 7},
		"recovery_cap":  {[]devAct{conf, {marker: markerQuestions}, conf, {marker: markerQuestions}}, 1, stopCap, 4},
		"coder_design":  {[]devAct{conf, {marker: markerNeedsDesign}}, 1, stopNeedsDesign, 2},
		"review_design": {[]devAct{conf, impl, {marker: markerNeedsDesign}}, 1, stopNeedsDesign, 3},
		"environment":   {[]devAct{conf, impl, {marker: markerReviewChanges, blocker: blockerEnvironment}}, 1, "environment_blocker", 3},
		"coder_blocked": {[]devAct{conf, {marker: markerBlocked}}, 1, "obstacle", 2},
		// NEEDS_TRIAGE is not a defect-mode marker: the publication is
		// never admitted.
		"defect_triage": {[]devAct{conf, {marker: markerNeedsTriage}}, 1, "invalid_marker", 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newLightRig(t, c.steps)
			r.expect(c.code, c.stop)
			if got := len(r.d.state.Tasks); got != c.tasks {
				t.Fatalf("%d tasks, want %d", got, c.tasks)
			}
			for _, d := range r.docs {
				if d.Role == "designer" || d.Role == "design-reviewer" {
					t.Fatal("a light attempt dispatched a design role")
				}
			}
			if len(r.d.phases) != 1 {
				t.Fatalf("%d coordinator phases", len(r.d.phases))
			}
			code, rep := r.offline()
			if code != c.code {
				t.Fatalf("offline %d %v", code, devCodes(rep))
			}
			if c.stop == stopNeedsDesign && !strings.Contains(r.f.stderr.String()+r.f.stdout.String(), "needs_design") {
				t.Fatal("no escalation handoff")
			}
		})
	}
}

func TestDevLightFP4(t *testing.T) {
	t.Parallel()
	seed := lightFixture(t)
	cand := lightGoodCandidate(t, seed)
	base := lightSeedBaseline(t)
	if !base.rec.Valid {
		t.Fatalf("baseline %+v", base.rec)
	}
	t.Run("real_candidate", func(t *testing.T) {
		t.Parallel()
		// The candidate-authored regression is red on the seed and green on
		// the candidate; the ten cases and the full suite pass.
		f := runLightFinal(realLightOptions(t, t.TempDir()), strings.Repeat("a", 40), strings.Repeat("b", 40), seed, cand, base.rec)
		if !f.reg.rec.Pass || f.suite.ExitCode != 0 || f.err != nil || !f.coverage.Comparison.Pass {
			t.Fatalf("final %+v %+v %v", f.reg.rec, f.suite, f.err)
		}
		for _, c := range f.cases.Cases {
			if !c.Pass {
				t.Errorf("case %s failed: %+v", c.ID, c)
			}
		}
		if len(f.cases.Cases) != 10 {
			t.Fatalf("%d cases", len(f.cases.Cases))
		}
		// Ten individually broken behaviors fail independently: each case's
		// observed result perturbed alone fails only that case.
		for i, c := range FeatureCases {
			obs := f.cases.Cases[i]
			for _, bad := range []string{"exit", "stdout", "stderr"} {
				r := obs.Exec
				stdout, stderr := obs.Stdout, obs.Stderr
				switch {
				case bad == "exit":
					r.ExitCode++
				case bad == "stdout" && c.Kind == "help", bad == "stderr" && c.Kind == "help":
					stdout, stderr = "", ""
				case bad == "stdout":
					stdout += "x"
				case stderr == "":
					stderr = "noise\n"
				default:
					stderr = ""
				}
				if ok, _ := evaluateCase(c, r, stdout, stderr); ok {
					t.Errorf("case %s accepted a broken %s", c.ID, bad)
				}
			}
			for j, other := range FeatureCases {
				if j != i {
					if ok, _ := evaluateCase(other, f.cases.Cases[j].Exec, f.cases.Cases[j].Stdout, f.cases.Cases[j].Stderr); !ok {
						t.Errorf("breaking %s failed %s", c.ID, other.ID)
					}
				}
			}
		}
	})
	for name, c := range map[string]struct {
		edit func(test string) string
		code string
	}{
		// A different assertion message is still a valid red.
		"other_message": {func(test string) string {
			return strings.ReplaceAll(test, `t.Fatalf("run(%q) wrote %q and %q"`, `t.Fatalf("other wording entirely: %q %q %q"`)
		}, ""},
		// A regression that passes on the seed is not red.
		"passes_on_seed": {func(test string) string {
			return strings.Replace(test, `{"whitespace", []string{"--name", " "}, 0, "Hello,  !\n", false}`,
				`{"whitespace", []string{"--name", "Ada"}, 0, "Hello, Ada!\n", false}`, 1)
		}, CodeRegressionStructure},
		// A test that does not compile is never a valid red.
		"compile_error": {func(test string) string { return test + "\nfunc broken() { undefined() }\n" }, CodeRegressionInvalidRed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(string(cand["main_test.go"]), `t.Fatalf("run(%q) wrote %q and %q"`) {
				t.Fatal("the fixture test's assertion text changed")
			}
			c2 := map[string][]byte{}
			for k, v := range cand {
				c2[k] = v
			}
			c2["main_test.go"] = []byte(c.edit(string(cand["main_test.go"])))
			reg := runLightRegression(realLightOptions(t, t.TempDir()), strings.Repeat("a", 40), strings.Repeat("b", 40), seed, c2, true)
			if reg.rec.Code != c.code {
				t.Fatalf("code %q, want %q (%+v %+v)", reg.rec.Code, c.code, reg.rec.RedEvents, reg.rec.Structure)
			}
		})
	}
	t.Run("not_green", func(t *testing.T) {
		t.Parallel()
		// A candidate that keeps the defect fails green.
		c2 := map[string][]byte{}
		for k, v := range cand {
			c2[k] = v
		}
		c2["main.go"] = seed["main.go"]
		reg := runLightRegression(realLightOptions(t, t.TempDir()), strings.Repeat("a", 40), strings.Repeat("b", 40), seed, c2, true)
		if reg.rec.Code != CodeRegressionNotGreen {
			t.Fatalf("code %q", reg.rec.Code)
		}
	})
}

func TestDevLightFP5(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	t.Run("real_helper", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, lightHappy())
		r.f.goPath = realGo(t)
		r.realHelper = os.Getenv("PATH")
		r.expect(0, "")
		review := r.d.state.Tasks[2]
		out := r.helperOut[review.TaskID]
		var rec DevLightReviewRun
		if err := json.Unmarshal([]byte(receiptLine(out)), &rec); err != nil || rec.Status != reviewPassed || rec.Scenario != scenarioLight ||
			!rec.Comparison.Pass || rec.RedCode != "" || rec.GreenCode != "" || !rec.ScratchRemoved || !rec.InputsUnchanged || len(rec.Commands) != len(lightReviewCommandNames) {
			t.Fatalf("receipt %+v %v", rec, err)
		}
		var ev DevReviewRecord
		if err := json.Unmarshal(r.evidence("tasks/03/review-evidence.json"), &ev); err != nil || ev.Evidence.Status != evidenceVerified || !ev.Evidence.Approving ||
			ev.Evidence.RunSHA256 != sha256Hex([]byte(receiptLine(out)+"\n")) || ev.Evidence.Light == nil {
			t.Fatalf("evidence %+v %v", ev, err)
		}
		if pub := r.d.trees[review.Commit]; len(pub) != len(r.d.trees[r.d.state.Tasks[1].Commit])+1 {
			t.Fatalf("the review published %d files", len(pub))
		}
		if code, _ := r.offline(); code != 0 {
			t.Fatal("offline")
		}
	})
	for alter, stop := range map[string]string{"prose-only": CodeReviewUnobserved, "cross-task": CodeReviewMismatch, "full-receipt": CodeReviewMismatch} {
		t.Run(alter, func(t *testing.T) {
			t.Parallel()
			r := newLightRig(t, []devAct{conf, {marker: markerImplComplete}, {marker: markerReviewApproved, alter: alter}})
			r.expect(1, stop)
			if code, rep := r.stored(); code != 1 || rep.Result == ResultPass {
				t.Fatal("offline accepted")
			}
		})
	}
	t.Run("changed_test", func(t *testing.T) {
		t.Parallel()
		// The receipt's tests are the candidate's: a stored review whose
		// candidate test bytes changed afterwards cannot approve.
		dir := lightPassingBundle(t)
		b, _ := LoadDevBundle(dir)
		c := newDevChecker(b)
		c.loadTrees()
		v := doc[contract.TaskShowResponse](b, taskDir(5)+"/task.json").Task
		p := doc[devEnvelope[TaskPlan]](b, taskDir(5)+"/plan.json").Data
		seed := c.lightSeedCommit()
		changed := withTreeFile(c.trees[p.ExpectedBase], "main_test.go", string(c.trees[p.ExpectedBase]["main_test.go"].Content)+"\n// edited\n")
		base := bundleReviewInput(b, taskDir(5), 5, &v, b.Manifest.Helper, p.ExpectedBase, seed, c.treeHash[p.ExpectedBase], changed,
			fileContent(c.trees[seed], "main.go"), markerReviewApproved, fileText(c.trees[*v.Result.Workspace.Commit], "review.md"))
		if ev := DeriveLightReviewEvidence(lightReviewInput{reviewEvidenceInput: base, SeedFiles: c.trees[seed], SeedTree: c.treeHash[seed], SeedCLI: true}); ev.Status == evidenceVerified {
			t.Fatalf("a changed test verified: %+v", ev)
		}
	})
}

func TestDevLightFP6(t *testing.T) {
	t.Parallel()
	t.Run("saved_bundles", func(t *testing.T) {
		t.Parallel()
		// Saved v2 full and light bundles reproduce their stored report and
		// audit exactly (the check compares them byte for byte).
		for name, dir := range map[string]string{"full": devPassingBundle(t), "light": lightPassingBundle(t)} {
			before := snapshotTree(t, dir)
			var out, errb bytes.Buffer
			if code := CheckEvidence(dir, &out, &errb); code != 0 || !bytes.Equal(out.Bytes(), mustRead(t, filepath.Join(dir, "report.json"))) {
				t.Errorf("%s: exit %d %s", name, code, errb.String())
			}
			if snapshotTree(t, dir) != before {
				t.Errorf("%s: the check changed the bundle", name)
			}
		}
	})
	t.Run("no_subprocess_capability", func(t *testing.T) {
		t.Parallel()
		// The light, probe and exercise checkers import no process, network
		// or plane client package.
		for _, f := range []string{"devlight_check.go", "devlight_probecheck.go", "devexercise_check.go", "devlight_coverage.go"} {
			parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot(t), "internal", "reale2e", f), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				switch strings.Trim(imp.Path.Value, `"`) {
				case "os/exec", "net", "net/http", "syscall", "github.com/wedevwork/callsheet/internal/client":
					t.Errorf("%s imports %s", f, imp.Path.Value)
				}
			}
		}
	})
	for name, c := range map[string]struct {
		dir    func(t *testing.T) string
		mutate func(t *testing.T, dir string)
		code   int
	}{
		"v1_full":       {devPassingBundle, func(t *testing.T, dir string) { downgradeToV1(t, dir) }, 0},
		"legacy_v3":     {passingBundle, func(*testing.T, string) {}, 0},
		"light_control": {lightPassingBundle, func(*testing.T, string) {}, 0},
		"light_authority": {lightPassingBundle, func(t *testing.T, dir string) {
			editJSON(t, dir, lightReceiptFile, func(m map[string]any) { m["data"].(map[string]any)["scope"] = "other" })
			reforge(t, dir)
		}, 1},
		"light_review_forgery": {lightPassingBundle, func(t *testing.T, dir string) {
			editJSON(t, dir, taskDir(5)+"/review-evidence.json", func(m map[string]any) { m["evidence"].(map[string]any)["approving"] = false })
			reforge(t, dir)
		}, 1},
		"light_malformed": {lightPassingBundle, func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, filepath.FromSlash(lightReceiptFile)), []byte("{"), 0o600)
		}, 2},
		"light_unsupported": {lightPassingBundle, func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) { m["scenario"] = "heavy" })
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := c.dir(t)
			c.mutate(t, dir)
			var out, errb bytes.Buffer
			if code := CheckEvidence(dir, &out, &errb); code != c.code {
				t.Fatalf("exit %d want %d %s %s", code, c.code, errb.String(), out.String())
			}
		})
	}
}

func TestDevLightFP7(t *testing.T) {
	t.Parallel()
	goBin := realGo(t)
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/enclosing\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := func() []string {
		cmd := exec.Command(goBin, "list", "./...")
		cmd.Dir = mod
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-buildvcs=false", "GOPROXY=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list: %v\n%s", err, out)
		}
		return strings.Fields(string(out))
	}
	before := list()
	// New full, light and probe bundles under the enclosing module's design
	// directory, without any nested go.mod.
	for name, src := range map[string]string{"full": devPassingBundle(t), "light": lightPassingBundle(t), "probe": probeLightBundle(t)} {
		dst := filepath.Join(mod, "design", "real-e2e-runs", name)
		if err := copyTree(src, dst); err != nil {
			t.Fatal(err)
		}
		filepathWalkGo(t, dst)
	}
	if after := list(); !slices.Equal(before, after) || !slices.Equal(after, []string{"example.com/enclosing"}) {
		t.Fatalf("packages before %v after %v", before, after)
	}
	// A historical v1 bundle is read in place, its bytes unchanged.
	v1 := devPassingBundle(t)
	downgradeToV1(t, v1)
	snap := snapshotTree(t, v1)
	var out, errb bytes.Buffer
	if code := CheckEvidence(v1, &out, &errb); code != 0 || snapshotTree(t, v1) != snap {
		t.Fatalf("v1 %d %s", code, errb.String())
	}
}

// filepathWalkGo fails on any .go file under dir.
func filepathWalkGo(t *testing.T, dir string) {
	t.Helper()
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".go") {
			t.Errorf("a new bundle holds the Go file %s", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDevLightFP8(t *testing.T) {
	t.Parallel()
	t.Run("standalone", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, lightHappy())
		r.expect(0, "")
		if !strings.Contains(r.f.stdout.String(), "light development flow: PASS") {
			t.Fatal("no light PASS line")
		}
		var out, errb bytes.Buffer
		if code := CheckEvidence(r.f.bundle(), &out, &errb); code != 0 {
			t.Fatalf("offline %d %s", code, errb.String())
		}
	})
	t.Run("combined", func(t *testing.T) {
		t.Parallel()
		root := exerciseBundle(t)
		sum := 0
		for _, dir := range []string{exerciseFullDir, exerciseLightDir} {
			child := filepath.Join(root, filepath.FromSlash(dir))
			var out, errb bytes.Buffer
			if code := CheckEvidence(child, &out, &errb); code != 0 {
				t.Fatalf("%s offline %d %s", dir, code, errb.String())
			}
			sum += bundleDoc[DevCost](t, child, devCostFile).WorkerCalls
		}
		var out, errb bytes.Buffer
		if code := CheckEvidence(root, &out, &errb); code != 0 {
			t.Fatalf("root offline %d %s", code, errb.String())
		}
		var rep DevExerciseReport
		json.Unmarshal(out.Bytes(), &rep)
		if rep.Accounting.WorkerCalls != sum || sum != exerciseHappyCalls || rep.Result != ResultPass {
			t.Fatalf("accounting %+v sum %d", rep.Accounting, sum)
		}
	})
	t.Run("full_failure_light_pass", func(t *testing.T) {
		t.Parallel()
		x := newExerciseRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete, alter: "bad-final"}}, lightHappy())
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		var out, errb bytes.Buffer
		if code := CheckEvidence(filepath.Join(x.root(), filepath.FromSlash(exerciseLightDir)), &out, &errb); code != 0 {
			t.Fatalf("the light child %d", code)
		}
		if !strings.Contains(x.stdout(), "combined exercise: FAIL") {
			t.Fatal("no combined FAIL")
		}
	})
	t.Run("unsafe_cleanup", func(t *testing.T) {
		t.Parallel()
		x := newExerciseRig(t, happy(), lightHappy())
		x.full.fault = "linger"
		if code := x.run(); code != 1 || exists(x.root(), exerciseLightDir) {
			t.Fatalf("exit %d, light started %v", code, exists(x.root(), exerciseLightDir))
		}
	})
	t.Run("missing_or_failed_child", func(t *testing.T) {
		t.Parallel()
		root := exerciseBundle(t)
		os.RemoveAll(filepath.Join(root, filepath.FromSlash(exerciseLightDir)))
		var out, errb bytes.Buffer
		if code := CheckEvidence(root, &out, &errb); code == 0 {
			t.Fatal("a root without its light child passed")
		}
		root = exerciseBundle(t)
		editJSON(t, root, devExerciseFile, func(m map[string]any) {
			m["children"].([]any)[1].(map[string]any)["status"] = childFail
		})
		rewriteExerciseDerived(t, root)
		out.Reset()
		if code := CheckEvidence(root, &out, &errb); code != 1 {
			t.Fatalf("a failed child: %d", code)
		}
	})
}

func TestDevLightFP9(t *testing.T) {
	t.Parallel()
	t.Run("stages", func(t *testing.T) {
		t.Parallel()
		r := probeLightRig(t, probeScopeLight)
		r.expect(0, "")
		q := r.qualification()
		var names []string
		for _, s := range q.Stages {
			names = append(names, s.Name+"="+s.Result)
		}
		if !slices.Equal(names, []string{"models=pass", "resume_wake=pass", "print_notification=pass", "reviewer_evidence=pass", "triage_evidence=pass",
			"light_reviewer_evidence=pass"}) {
			t.Fatalf("stages %v", names)
		}
		if codes := qualificationCodes(t, r.f.bundle()); len(codes) != 0 {
			t.Fatalf("codes %v", codes)
		}
		var roles []string
		for _, d := range r.docs {
			roles = append(roles, d.Role)
		}
		if !slices.Equal(roles, []string{"designer", "designer", "designer", "code-reviewer", "triage", "code-reviewer"}) {
			t.Fatalf("roles %v", roles)
		}
	})
	t.Run("successors_not_reached", func(t *testing.T) {
		t.Parallel()
		r := probeLightRig(t, probeScopeBoth)
		r.steps = []devAct{{}, {}, {}, {marker: markerReviewApproved}, {marker: markerTriageUnconf}}
		r.expect(1, "probe_triage_unqualified")
		q := r.qualification()
		if q.Stages[4].Result != stageFail || q.Stages[5].Result != stageUnreached || q.WorkerCalls != 10 {
			t.Fatalf("%+v %d", q.Stages, q.WorkerCalls)
		}
	})
	t.Run("namespace_borrowing", func(t *testing.T) {
		t.Parallel()
		full := devPassingBundle(t)
		c := firstCall(t, full, toolTaskShow, nil)
		dir := probeLightBundle(t)
		lines := phaseStream(t, dir, 3)
		insertAt(t, dir, 3, -1, useLine(sessionOf(lines), "toolu_ns", toolTaskShow, c.args), resultLine(sessionOf(lines), "toolu_ns", c.answer, false))
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, devMCPCallInvalid) {
			t.Fatalf("%v", codes)
		}
	})
	t.Run("historical_scope", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		downgradeQualificationToV1(t, dir)
		b, err := LoadDevQualification(dir)
		if err != nil || b.ver != devBundleV1 {
			t.Fatalf("%v", err)
		}
		q := doc[DevQualification](b, devQualificationFile)
		if q.Scope != "" || q.Light != nil || len(CheckDevQualification(b)) != 0 {
			t.Fatalf("v1 %+v", q)
		}
	})
	t.Run("ci_refused", func(t *testing.T) {
		t.Parallel()
		var errb bytes.Buffer
		h := Host{GOOS: "linux", Args: []string{"probe-dev-full", "--scenario", "both", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
			Getenv: func(k string) string {
				if k == EnvOptIn {
					return "1"
				}
				return ""
			}, LookupEnv: func(k string) (string, bool) { return "1", k == EnvCI }, Stdout: &bytes.Buffer{}, Stderr: &errb,
			NewWorld: func(Host) (*World, func(), error) { t.Error("a world in CI"); return nil, nil, nil }}
		if code := MainWith(h); code != 2 || !strings.Contains(errb.String(), ErrCI.Error()) {
			t.Fatalf("%d %s", code, errb.String())
		}
	})
}

func TestDevLightFP10(t *testing.T) {
	t.Parallel()
	goBin := realGo(t)
	dir := lightPassingBundle(t)
	b, err := LoadDevBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newDevChecker(b)
	c.loadTrees()
	seed := c.lightSeedCommit()
	led := bundleDoc[devEnvelope[DevFullState]](t, dir, devLedgerFile).Data
	tree := func(commit string) map[string][]byte {
		out := map[string][]byte{}
		for n, f := range c.trees[commit] {
			if slices.Contains(lightFixtureFiles, n) && n != "FEATURE.md" {
				out[n] = f.Content
			}
		}
		return out
	}
	seedFiles, candFiles := tree(seed), tree(led.CandidateCommit)
	// The pinned coverage command with the real toolchain on an export.
	profile := func(files map[string][]byte) []byte {
		d := t.TempDir()
		for n, data := range files {
			os.WriteFile(filepath.Join(d, n), data, 0o644)
		}
		p := filepath.Join(t.TempDir(), "coverage.out")
		cmd := exec.Command(goBin, "test", "-count=1", "-covermode=set", "-coverprofile="+p, "./...")
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-buildvcs=false", "GOPROXY=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go test: %v\n%s", err, out)
		}
		return mustRead(t, p)
	}
	seedProfile, candProfile := profile(seedFiles), profile(candFiles)
	reduced := map[string][]byte{}
	for k, v := range candFiles {
		reduced[k] = v
	}
	reduced["main_test.go"] = []byte(strings.Replace(string(candFiles["main_test.go"]), `{"help"`, `// {"help"`, 1))
	if bytes.Equal(reduced["main_test.go"], candFiles["main_test.go"]) {
		t.Fatal("no help row to drop")
	}
	reducedProfile := profile(reduced)
	// feed writes real profiles into the saved bundle with consistently
	// recomputed records (a capable forger can do no more).
	feed := func(t *testing.T, base, cand []byte) string {
		d := lightPassingBundle(t)
		os.WriteFile(filepath.Join(d, "validation", "light", "baseline-coverage.out"), base, 0o600)
		os.WriteFile(filepath.Join(d, "validation", "coverage.out"), cand, 0o600)
		bc, err := deriveWholeCoverage(seedFiles["main.go"], base, "example.com/greeting")
		if err != nil {
			t.Fatal(err)
		}
		editDoc[DevLightBaseline](t, d, "validation/light-baseline.json", func(v *DevLightBaseline) { v.Coverage = &bc })
		editDoc[DevLightCoverageRecord](t, d, "validation/light-coverage.json", func(v *DevLightCoverageRecord) {
			v.Baseline = &bc
			v.Comparison, v.Cand, v.Error = lightCoverageOf(&bc, candFiles, cand, v.CandidateExec)
		})
		return d
	}
	t.Run("real_profiles_pass", func(t *testing.T) {
		t.Parallel()
		d := feed(t, seedProfile, candProfile)
		if code, rep := lightReforge(t, d); code != 0 {
			t.Fatalf("real profiles %d %v", code, devCodes(rep))
		}
		cov := bundleDoc[DevLightCoverageRecord](t, d, "validation/light-coverage.json")
		if !cov.Comparison.Pass || cov.Baseline.Total == 0 || cov.Cand.Total == 0 || cov.Cand.Scope != "example.com/greeting/main.go" {
			t.Fatalf("comparison %+v", cov)
		}
	})
	t.Run("equal_passes", func(t *testing.T) {
		t.Parallel()
		bc, _ := deriveWholeCoverage(seedFiles["main.go"], seedProfile, "example.com/greeting")
		if cmp := compareLightCoverage(bc, bc); !cmp.Pass {
			t.Fatalf("%+v", cmp)
		}
	})
	t.Run("decreased_fails", func(t *testing.T) {
		t.Parallel()
		d := feed(t, seedProfile, reducedProfile)
		code, rep := lightReforge(t, d)
		if code != 1 || !slices.Contains(devCriterion(rep, "coverage").Codes, CodeCoverageDecreased) {
			t.Fatalf("decreased %d %v", code, devCodes(rep))
		}
	})
	for name, mutate := range map[string]func(t *testing.T, d string){
		"missing": func(t *testing.T, d string) { os.Remove(filepath.Join(d, "validation", "coverage.out")) },
		"invalid": func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "validation", "coverage.out"), []byte("mode: count\n"), 0o600)
		},
		"substituted": func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "validation", "coverage.out"), seedProfile, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := feed(t, seedProfile, candProfile)
			mutate(t, d)
			if code, rep := lightReforge(t, d); code == 0 || devCriterion(rep, "coverage").Result == ResultCriterionPass {
				t.Fatalf("%s %d %v", name, code, devCodes(rep))
			}
		})
	}
}
