package reale2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Unit groups of design 12a-dev-flow-light for FP-5: the light
// verification helper's runner (refusals, executed failures, cleanup and
// input guarantees), its receipt and report-line bounds, the light
// review.md field check and the light execution evidence derivation, all
// with the semantic fake Go and no subprocess.

// lightReviewFix is one light reviewer checkout pinned at a candidate (the
// known-good restoration with a triage brief) whose parent is the light
// seed.
type lightReviewFix struct {
	t          testing.TB
	hub        *memory.Storage
	seed, cand plumbing.Hash
	seedFiles  map[string][]byte
	candFiles  map[string][]byte
	co         string
	spec       reviewHelperSpec
	task       string
	clock      *autoClock
	launcher   *fakeLauncher
	// wrap, when set, wraps the scripted Go.
	wrap func(s ProcSpec, inner func(ProcSpec) behavior) behavior
}

func newLightReviewFix(t testing.TB, cand func(seed map[string][]byte) map[string][]byte) *lightReviewFix {
	t.Helper()
	seedFiles := lightFixture(t)
	hub := memory.NewStorage()
	seed, _ := writeCommitMode(t, hub, seedFiles, nil)
	candFiles := lightGoodCandidate(t, seedFiles)
	candFiles["rca.md"] = []byte("# RCA\n\nThe trimmed check refuses whitespace.\n\nSTATUS: TRIAGE_CONFIRMED\n")
	candFiles["fix.md"] = []byte("# Fix\n\nRoot cause: main.go:28\nConfidence: CONFIRMED\nDesign required: no\n")
	if cand != nil {
		candFiles = cand(candFiles)
	}
	c, _ := writeCommitMode(t, hub, candFiles, nil, seed)
	co := physicalTempDir(t)
	pinReviewCheckout(t, co, hub, c, candFiles)
	f := &lightReviewFix{t: t, hub: hub, seed: seed, cand: c, seedFiles: seedFiles, candFiles: candFiles, co: co, task: hexID("t_", 21), clock: newAutoClock()}
	f.spec = reviewHelperSpec{Reale2e: "/opt/reale2e/reale2e", Go: stubFile(t, t.TempDir(), "go"), Deadline: "2026-10-10T13:00:00Z"}
	return f
}

// argv is the light helper's exact invocation.
func (f *lightReviewFix) argv() []string {
	return f.spec.argvFor(scenarioLight, f.task, f.cand.String(), f.seed.String())
}

// run runs the light helper in-process with the scripted Go.
func (f *lightReviewFix) run() (DevLightReviewRun, string, int) {
	f.t.Helper()
	inner := reviewScript{}.script
	f.launcher = &fakeLauncher{script: func(s ProcSpec) behavior {
		if f.wrap != nil {
			return f.wrap(s, inner)
		}
		return inner(s)
	}}
	out, errOut, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: f.launcher, path: "/usr/bin:/bin"}, f.argv())
	var rec DevLightReviewRun
	if code != 2 {
		if err := json.Unmarshal([]byte(receiptLine(out)), &rec); err != nil {
			f.t.Fatalf("no light receipt (%d): %v %s %s", code, err, out, errOut)
		}
	}
	return rec, out, code
}

// treeFiles and tree are the candidate as tree files and its tree hash;
// seedTreeFiles and seedTree the seed's.
func (f *lightReviewFix) treeFiles() map[string]TreeFile {
	out := map[string]TreeFile{}
	for p, b := range f.candFiles {
		out[p] = TreeFile{Path: p, Content: b}
	}
	return out
}

func (f *lightReviewFix) seedTreeFiles() map[string]TreeFile {
	out := map[string]TreeFile{}
	for p, b := range f.seedFiles {
		out[p] = TreeFile{Path: p, Content: b}
	}
	return out
}

func (f *lightReviewFix) treeOf(h plumbing.Hash) string {
	f.t.Helper()
	c, err := object.GetCommit(f.hub, h)
	if err != nil {
		f.t.Fatal(err)
	}
	return c.TreeHash.String()
}

// evidenceInput is the light derivation input of a captured run.
func (f *lightReviewFix) evidenceInput(out string, exit int, marker string) lightReviewInput {
	log := codexLog("/bin/bash -lc '"+strings.Join(f.argv(), " ")+"'", out, exit)
	report := "# Light review\n\n"
	if _, trailer, _ := strings.Cut(out, "\n"); trailer != "" {
		report += trailer
	}
	base := reviewEvidenceInput{Ordinal: 3, Log: log, LogFile: "tasks/03/logs.txt", Meta: completeMeta(log), Task: f.task, Helper: f.spec,
		Candidate: f.cand.String(), Initial: f.seed.String(), CandTree: f.treeOf(f.cand), CandFiles: f.treeFiles(), SeedMain: f.seedFiles["main.go"],
		AdmittedAt: newAutoClock().Now().Add(-1), Marker: marker, Report: report}
	return lightReviewInput{reviewEvidenceInput: base, SeedFiles: f.seedTreeFiles(), SeedTree: f.treeOf(f.seed), SeedCLI: true}
}

func TestDevLightReviewRunner(t *testing.T) {
	t.Parallel()
	t.Run("passes", func(t *testing.T) {
		t.Parallel()
		f := newLightReviewFix(t, nil)
		rec, out, code := f.run()
		if code != 0 || rec.Status != reviewPassed || rec.Code != "" || !rec.ScratchCreated || !rec.ScratchRemoved || !rec.InputsUnchanged ||
			len(rec.Commands) != len(lightReviewCommandNames) || !rec.Comparison.Pass || rec.RedCode != "" || rec.GreenCode != "" ||
			rec.InputTree != f.treeOf(f.cand) || rec.SeedTree != f.treeOf(f.seed) || len(rec.Exports) != 3 || rec.TestSHA256 != sha256Hex(f.candFiles["main_test.go"]) {
			t.Fatalf("receipt %d %s", code, out)
		}
		// Every command ran the pinned Go with its logical argv in its fixed
		// export.
		for i, s := range f.launcher.started {
			c := rec.Commands[i]
			if s.Path != f.spec.Go || s.Dir != lightReviewCwd(c.Name, rec.Scratch) || !slices.Equal(append([]string{"go"}, s.Args...), c.Argv) ||
				!slices.Equal(c.Argv, lightReviewLogicalArgv(c.Name, rec.Scratch)) || !slices.Contains(s.Env, "GOPROXY=off") {
				t.Fatalf("command %d %+v", i, s)
			}
		}
		for _, e := range rec.Exports {
			if e.Name == "before" && inputSHA(e.Inputs, "main.go") != sha256Hex(f.seedFiles["main.go"]) ||
				e.Name == "before" && inputSHA(e.Inputs, "main_test.go") != sha256Hex(f.candFiles["main_test.go"]) {
				t.Fatalf("the before export %+v", e)
			}
		}
		line, trailer, _ := strings.Cut(out, "\n")
		want, ok := renderLightReviewReportLines(rec, []byte(line+"\n"))
		if !ok || trailer != want || !lightReportLinesComplete(trailer) {
			t.Fatalf("trailer %q want %q", trailer, want)
		}
		if _, err := os.Lstat(filepath.Join(f.co, reviewScratch)); err == nil {
			t.Fatal("the scratch survived")
		}
	})
	for name, c := range map[string]struct {
		prepare func(f *lightReviewFix)
		cand    func(map[string][]byte) map[string][]byte
		code    int
		status  string
		want    string
	}{
		"pre_existing_scratch": {func(f *lightReviewFix) { os.Mkdir(filepath.Join(f.co, reviewScratch), 0o700) }, nil, 1, reviewRefused, ""},
		"go_unreadable":        {func(f *lightReviewFix) { f.spec.Go = filepath.Join(t.TempDir(), "absent-go") }, nil, 1, reviewRefused, "go_unreadable"},
		"deadline_passed":      {func(f *lightReviewFix) { f.spec.Deadline = "2026-10-10T12:00:00Z" }, nil, 1, reviewRefused, "deadline_passed"},
		"unknown_seed":         {func(f *lightReviewFix) { f.seed = plumbing.NewHash(strings.Repeat("a", 40)) }, nil, 1, reviewRefused, ""},
		"still_defective": {nil, func(m map[string][]byte) map[string][]byte {
			m["main.go"] = lightFixture(t)["main.go"]
			return m
		}, 1, reviewFailed, CodeRegressionNotGreen},
		"no_regression": {nil, func(m map[string][]byte) map[string][]byte {
			m["main_test.go"] = lightFixture(t)["main_test.go"]
			return m
		}, 1, reviewFailed, CodeRegressionNotRed},
		"baseline_invalid": {func(f *lightReviewFix) {
			f.wrap = func(s ProcSpec, inner func(ProcSpec) behavior) behavior {
				if strings.HasSuffix(s.Dir, "/baseline") && strings.Join(s.Args, " ") == "test -count=1 ./..." {
					return behavior{stdout: "FAIL\n", exit: 1}
				}
				return inner(s)
			}
		}, nil, 1, reviewFailed, "baseline_invalid"},
		"coverage_unobserved": {func(f *lightReviewFix) {
			f.wrap = func(s ProcSpec, inner func(ProcSpec) behavior) behavior {
				b := inner(s)
				if strings.Contains(strings.Join(s.Args, " "), "-coverprofile=") && strings.HasSuffix(s.Dir, "/after") {
					b.after = nil
				}
				return b
			}
		}, nil, 1, reviewFailed, CodeCoverageUnobserved},
		"coverage_decreased": {func(f *lightReviewFix) {
			f.wrap = func(s ProcSpec, inner func(ProcSpec) behavior) behavior {
				b := inner(s)
				if p, ok := strings.CutPrefix(strings.Join(s.Args, " "), "test -count=1 -covermode=set -coverprofile="); ok && strings.HasSuffix(s.Dir, "/after") {
					out := strings.TrimSuffix(p, " ./...")
					dir := s.Dir
					b.after = func() {
						main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
						prof := strings.ReplaceAll(string(synthProfile("example.com/greeting", main)), " 1\n", " 0\n")
						os.WriteFile(out, []byte(prof), 0o600)
					}
				}
				return b
			}
		}, nil, 1, reviewFailed, CodeCoverageDecreased},
		"inputs_changed": {func(f *lightReviewFix) {
			co := f.co
			f.wrap = func(s ProcSpec, inner func(ProcSpec) behavior) behavior {
				if strings.HasSuffix(s.Dir, "/after") && strings.Join(s.Args, " ") == "test -count=1 -json ./..." {
					os.WriteFile(filepath.Join(co, "main.go"), []byte("package main\n"), 0o644)
				}
				return inner(s)
			}
		}, nil, 1, reviewFailed, "inputs_changed"},
		"unexpected_file": {func(f *lightReviewFix) {
			co := f.co
			f.wrap = func(s ProcSpec, inner func(ProcSpec) behavior) behavior {
				if strings.HasSuffix(s.Dir, "/after") && strings.Join(s.Args, " ") == "test -count=1 -json ./..." {
					os.WriteFile(filepath.Join(co, "stray.txt"), []byte("x\n"), 0o644)
				}
				return inner(s)
			}
		}, nil, 1, reviewFailed, "unexpected_file"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLightReviewFix(t, c.cand)
			if c.prepare != nil {
				c.prepare(f)
			}
			rec, out, code := f.run()
			if code != c.code || rec.Status != c.status || c.want != "" && rec.Code != c.want {
				t.Fatalf("exit %d status %s code %s\n%s", code, rec.Status, rec.Code, out)
			}
			if c.status == reviewRefused && len(rec.Commands) != 0 {
				t.Fatalf("a refusal ran %d commands", len(rec.Commands))
			}
			if name == "baseline_invalid" && len(rec.Commands) != 2 {
				t.Fatalf("dependent commands ran after an invalid baseline: %d", len(rec.Commands))
			}
			if name == "pre_existing_scratch" {
				if _, err := os.Stat(filepath.Join(f.co, reviewScratch)); err != nil {
					t.Fatal("the helper removed a scratch it did not create")
				}
			}
		})
	}
	t.Run("receipt_overflow", func(t *testing.T) {
		t.Parallel()
		f := newLightReviewFix(t, nil)
		rec, _, _ := f.run()
		rec.Commands[0].Stdout = strings.Repeat("x", maxReviewRunBytes)
		out, final, code := encodeLightReviewRun(rec)
		if rec.Commands[0].Stdout == "" {
			t.Fatal("the overflow encoding cleared the caller's outputs")
		}
		if code != 1 || final.Status != reviewFailed || final.Code != "receipt_overflow" || !final.OutputsOmitted || len(out) > maxReviewRunBytes {
			t.Fatalf("overflow %d %+v", code, final.Code)
		}
		stdout, code := lightHelperStdout(rec)
		if code != 1 || !strings.Contains(string(stdout), "receipt_overflow") {
			t.Fatalf("helper stdout %d", code)
		}
	})
}

func TestDevLightReviewReport(t *testing.T) {
	t.Parallel()
	f := newLightReviewFix(t, nil)
	rec, out, code := f.run()
	if code != 0 {
		t.Fatalf("control %d", code)
	}
	line, trailer, _ := strings.Cut(out, "\n")
	digest := sha256Hex([]byte(line + "\n"))
	report := "# Light review\n\n" + trailer + "\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n"
	files := f.treeFiles()
	if err := LightReviewReportOK(report, markerReviewApproved, f.cand.String(), f.task, files, digest); err != nil {
		t.Fatalf("control: %v", err)
	}
	drop := func(prefix string) func(string) string {
		return func(r string) string {
			var keep []string
			for _, l := range strings.Split(r, "\n") {
				if !strings.HasPrefix(l, prefix) {
					keep = append(keep, l)
				}
			}
			return strings.Join(keep, "\n")
		}
	}
	dup := func(prefix string) func(string) string {
		return func(r string) string {
			for _, l := range strings.Split(r, "\n") {
				if strings.HasPrefix(l, prefix) {
					return strings.Replace(r, l+"\n", l+"\n"+l+"\n", 1)
				}
			}
			return r
		}
	}
	for name, c := range map[string]struct {
		edit   func(string) string
		marker string
		digest string
	}{
		"marker":           {func(r string) string { return r }, markerReviewChanges, digest},
		"no_digest_line":   {drop(reviewRunField), markerReviewApproved, digest},
		"repeated_digest":  {dup(reviewRunField), markerReviewApproved, digest},
		"other_digest":     {func(r string) string { return r }, markerReviewApproved, strings.Repeat("0", 64)},
		"uncorroborated":   {func(r string) string { return r }, markerReviewApproved, ""},
		"no_scenario":      {drop("Scenario:"), markerReviewApproved, digest},
		"full_scenario":    {func(r string) string { return strings.Replace(r, "Scenario: light", "Scenario: full", 1) }, markerReviewApproved, digest},
		"no_candidate":     {drop("Reviewed candidate:"), markerReviewApproved, digest},
		"other_candidate":  {func(r string) string { return strings.Replace(r, f.cand.String(), strings.Repeat("b", 40), 1) }, markerReviewApproved, digest},
		"missing_source":   {drop("Source sha256: main.go"), markerReviewApproved, digest},
		"duplicate_source": {dup("Source sha256: main.go"), markerReviewApproved, digest},
		"wrong_source_hash": {func(r string) string {
			return strings.Replace(r, sha256Hex(f.candFiles["main.go"]), strings.Repeat("1", 64), 1)
		}, markerReviewApproved, digest},
		"missing_red":     {drop("Verification exit 1: red"), markerReviewApproved, digest},
		"duplicate_green": {dup("Verification exit 0: green"), markerReviewApproved, digest},
		"other_scratch": {func(r string) string {
			return strings.ReplaceAll(r, reviewScratch+"/"+f.task+"/", reviewScratch+"/"+hexID("t_", 99)+"/")
		}, markerReviewApproved, digest},
		"malformed_exit": {func(r string) string {
			return strings.Replace(r, "Verification exit 0: green", "Verification exit zero: green", 1)
		}, markerReviewApproved, digest},
		"no_coverage": {drop("Coverage comparison:"), markerReviewApproved, digest},
		"no_cleanup":  {drop("Cleanup:"), markerReviewApproved, digest},
	} {
		if err := LightReviewReportOK(c.edit(report), c.marker, f.cand.String(), f.task, files, c.digest); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A changes-requested review without a corroborated run may report
	// the helper could not start (no run line).
	noRun := drop(reviewRunField)(strings.Replace(report, "STATUS: REVIEW_APPROVED", "STATUS: REVIEW_CHANGES_REQUESTED", 1))
	if err := LightReviewReportOK(noRun, markerReviewChanges, f.cand.String(), f.task, files, ""); err != nil {
		t.Fatalf("an uncorroborated changes-requested review: %v", err)
	}
	if !strings.Contains(trailer, lightRedExpectation) || rec.Scenario != scenarioLight {
		t.Fatal("the red expectation is not rendered")
	}
}

func TestDevLightReviewDerivation(t *testing.T) {
	t.Parallel()
	f := newLightReviewFix(t, nil)
	_, out, code := f.run()
	if code != 0 {
		t.Fatalf("control %d", code)
	}
	control := DeriveLightReviewEvidence(f.evidenceInput(out, 0, markerReviewApproved))
	if control.Status != evidenceVerified || !control.Approving || control.Light == nil {
		t.Fatalf("control %+v", control)
	}
	line, trailer, _ := strings.Cut(out, "\n")
	var run DevLightReviewRun
	json.Unmarshal([]byte(line), &run)
	reencode := func(mut func(r *DevLightReviewRun)) string {
		r := run
		r.Commands = slices.Clone(run.Commands)
		mut(&r)
		b, _ := json.Marshal(r)
		tr, _ := renderLightReviewReportLines(r, append(b, '\n'))
		return string(b) + "\n" + tr
	}
	for name, c := range map[string]struct {
		in   func() lightReviewInput
		code string
	}{
		"incomplete_log": {func() lightReviewInput {
			x := f.evidenceInput(out, 0, markerReviewApproved)
			x.Meta.Truncated = true
			return x
		}, "review_log_incomplete"},
		"not_started": {func() lightReviewInput {
			x := f.evidenceInput(out, 0, markerReviewApproved)
			x.Log = codexLog("/bin/bash -lc 'go test ./...'", "ok\n", 0)
			x.Meta = completeMeta(x.Log)
			return x
		}, "review_helper_not_started"},
		"full_argv": {func() lightReviewInput {
			x := f.evidenceInput(out, 0, markerReviewApproved)
			x.Log = codexLog("/bin/bash -lc '"+strings.Join(f.spec.argv(f.task, f.cand.String(), f.seed.String()), " ")+"'", out, 0)
			x.Meta = completeMeta(x.Log)
			return x
		}, "review_command_mismatch"},
		"no_receipt_line": {func() lightReviewInput {
			return f.evidenceInput(strings.TrimSuffix(line, "\n"), 0, markerReviewApproved)
		}, "review_receipt_incomplete"},
		"full_receipt": {func() lightReviewInput {
			full, _ := json.Marshal(DevReviewRun{Schema: DevReviewRunSchema, Task: f.task})
			return f.evidenceInput(string(full)+"\n"+trailer, 0, markerReviewApproved)
		}, "review_receipt_scenario"},
		"candidate_tree": {func() lightReviewInput {
			x := f.evidenceInput(out, 0, markerReviewApproved)
			x.CandTree = strings.Repeat("c", 40)
			return x
		}, "review_receipt_binding"},
		"seed_tree": {func() lightReviewInput {
			x := f.evidenceInput(out, 0, markerReviewApproved)
			x.SeedTree = strings.Repeat("d", 40)
			return x
		}, "review_receipt_binding"},
		"go_binary": {func() lightReviewInput {
			return f.evidenceInput(reencode(func(r *DevLightReviewRun) { r.Go.SHA256 = "x" }), 0, markerReviewApproved)
		}, "review_receipt_binding"},
		"forged_inputs": {func() lightReviewInput {
			return f.evidenceInput(reencode(func(r *DevLightReviewRun) { r.Inputs = r.Inputs[1:] }), 0, markerReviewApproved)
		}, ""},
		"forged_red": {func() lightReviewInput {
			return f.evidenceInput(reencode(func(r *DevLightReviewRun) { r.RedCode = CodeRegressionNotRed }), 0, markerReviewApproved)
		}, ""},
		"forged_comparison": {func() lightReviewInput {
			return f.evidenceInput(reencode(func(r *DevLightReviewRun) { r.Comparison.Pass = false }), 0, markerReviewApproved)
		}, ""},
		"forged_scratch_flag": {func() lightReviewInput {
			return f.evidenceInput(reencode(func(r *DevLightReviewRun) { r.ScratchRemoved = false }), 0, markerReviewApproved)
		}, ""},
		"trailer_edit": {func() lightReviewInput {
			return f.evidenceInput(line+"\n"+strings.Replace(trailer, "Scenario: light", "Scenario: light (verified)", 1), 0, markerReviewApproved)
		}, "review_report_lines_mismatch"},
		"report_missing_line": {func() lightReviewInput {
			x := f.evidenceInput(out, 0, markerReviewApproved)
			x.Report = strings.Replace(x.Report, "Coverage comparison:", "Coverage summary:", 1)
			return x
		}, "review_report_lines_mismatch"},
		"approved_failed_run": {func() lightReviewInput {
			return f.evidenceInput(reencode(func(r *DevLightReviewRun) {
				r.Status, r.Code = reviewFailed, "inputs_changed"
				r.InputsUnchanged = false
			}), 1,
				markerReviewApproved)
		}, ""},
		"uncorroborated_red": {func() lightReviewInput {
			x := f.evidenceInput(out, 0, markerReviewApproved)
			x.SeedCLI = false
			return x
		}, CodeRegressionInvalidRed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev := DeriveLightReviewEvidence(c.in())
			if ev.Status == evidenceVerified || c.code != "" && ev.Code != c.code {
				t.Fatalf("%s: %s %s, want %s", name, ev.Status, ev.Code, c.code)
			}
		})
	}
	t.Run("failed_run_supports_changes", func(t *testing.T) {
		t.Parallel()
		g := newLightReviewFix(t, func(m map[string][]byte) map[string][]byte {
			m["main.go"] = lightFixture(t)["main.go"]
			return m
		})
		_, failedOut, code := g.run()
		if code != 1 {
			t.Fatalf("%d", code)
		}
		x := g.evidenceInput(failedOut, 1, markerReviewChanges)
		if ev := DeriveLightReviewEvidence(x); ev.Status != evidenceFailedRun || ev.Code != CodeRegressionNotGreen || ev.Approving {
			t.Fatalf("a failed run with changes requested: %+v", ev)
		}
		x.Marker = markerReviewApproved
		if ev := DeriveLightReviewEvidence(x); ev.Code != "review_approved_failed_run" {
			t.Fatalf("a failed run approving: %+v", ev)
		}
	})
	if fmt.Sprint(control.Started) == "" {
		t.Fatal("no started record")
	}
}
