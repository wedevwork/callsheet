package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Unit groups of design 12a-dev-flow-verify for FP-4 and FP-5: the
// verification helper's runner, the reviewer execution evidence, the
// candidate oracle and the changed-code coverage.

// reviewFix is one reviewer checkout pinned at a candidate whose parent is
// the development seed.
type reviewFix struct {
	t          testing.TB
	hub        *memory.Storage
	seed, cand plumbing.Hash
	seedFiles  map[string][]byte
	candFiles  map[string][]byte
	co         string
	spec       reviewHelperSpec
	task       string
	clock      *autoClock
	script     reviewScript
	launcher   *fakeLauncher
}

func newReviewFix(t testing.TB) *reviewFix {
	t.Helper()
	seedFiles, err := devSeedFiles("1.26")
	if err != nil {
		t.Fatal(err)
	}
	hub := memory.NewStorage()
	seed, _ := writeCommitMode(t, hub, seedFiles, nil)
	candFiles := withFiles(seedFiles, map[string][]byte{"main.go": []byte(fixtureMain), "main_test.go": []byte(fixtureTest),
		"design.md": []byte("# Design\n"), "design-review.md": []byte("# Review\n\nSTATUS: DESIGN_REVIEW_APPROVED\n")})
	cand, _ := writeCommitMode(t, hub, candFiles, nil, seed)
	co := t.TempDir()
	pinReviewCheckout(t, co, hub, cand, candFiles)
	f := &reviewFix{t: t, hub: hub, seed: seed, cand: cand, seedFiles: seedFiles, candFiles: candFiles, co: co, task: hexID("t_", 11),
		clock: newAutoClock()}
	f.spec = reviewHelperSpec{Reale2e: "/opt/reale2e/reale2e", Go: stubFile(t, t.TempDir(), "go"), Deadline: "2026-10-10T13:00:00Z"}
	return f
}

// argv is the helper's exact invocation for the fixture.
func (f *reviewFix) argv() []string { return f.spec.argv(f.task, f.cand.String(), f.seed.String()) }

// run runs the helper in-process with the scripted Go.
func (f *reviewFix) run() (DevReviewRun, string, int) {
	f.t.Helper()
	f.launcher = &fakeLauncher{script: f.script.script}
	out, errOut, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: f.launcher, path: "/usr/bin:/bin"}, f.argv())
	var rec DevReviewRun
	if code != 2 {
		if err := json.Unmarshal([]byte(out), &rec); err != nil {
			f.t.Fatalf("no receipt (%d): %v %s %s", code, err, out, errOut)
		}
	}
	return rec, out, code
}

// files is the candidate as tree files.
func (f *reviewFix) treeFiles() map[string]TreeFile {
	out := map[string]TreeFile{}
	for p, b := range f.candFiles {
		out[p] = TreeFile{Path: p, Content: b}
	}
	return out
}

// tree is the candidate's tree hash.
func (f *reviewFix) tree() string {
	f.t.Helper()
	c, err := object.GetCommit(f.hub, f.cand)
	if err != nil {
		f.t.Fatal(err)
	}
	return c.TreeHash.String()
}

func TestDevFullReviewRunner(t *testing.T) {
	t.Parallel()
	t.Run("passes_on_pinned_export", func(t *testing.T) {
		t.Parallel()
		f := newReviewFix(t)
		rec, out, code := f.run()
		if code != 0 || rec.Status != reviewPassed || rec.Code != "" || !rec.ScratchCreated || !rec.ScratchRemoved || !rec.InputsUnchanged ||
			rec.Coverage == nil || !rec.Coverage.Pass || !rec.Tests.Passed || len(rec.Commands) != 4 || rec.Go.SHA256 != sha256Hex(mustRead(t, f.spec.Go)) ||
			rec.Go.Version != "go version go1.26.4 linux/amd64" || rec.InputTree != f.tree() {
			t.Fatalf("receipt %d %s", code, out)
		}
		// Every command ran the pinned Go in after/ with its logical argv,
		// in the private offline environment.
		for i, s := range f.launcher.started {
			c := rec.Commands[i]
			if s.Path != f.spec.Go || s.Dir != rec.Scratch+"/after" || !slices.Equal(append([]string{"go"}, s.Args...), c.Argv) ||
				c.Name != reviewCommandNames[i] || !slices.Equal(s.Env, rec.Env) || !slices.Contains(s.Env, "GOPROXY=off") ||
				!slices.Contains(s.Env, "GOCACHE="+rec.Scratch+"/env/gocache") {
				t.Fatalf("command %d %+v", i, s)
			}
		}
		if _, err := os.Lstat(filepath.Join(f.co, reviewScratch)); err == nil {
			t.Fatal("the scratch survived")
		}
		if len(out) > maxReviewRunBytes || strings.Count(out, "\n") != 1 {
			t.Fatal("the receipt is not one bounded line")
		}
	})
	t.Run("exports_the_candidate", func(t *testing.T) {
		t.Parallel()
		f := newReviewFix(t)
		seen := map[string][]byte{}
		f.script = reviewScript{}
		inner := f.script.script
		f.launcher = &fakeLauncher{script: func(s ProcSpec) behavior {
			if len(seen) == 0 {
				entries, _ := os.ReadDir(s.Dir)
				for _, e := range entries {
					seen[e.Name()] = mustRead(t, filepath.Join(s.Dir, e.Name()))
				}
			}
			return inner(s)
		}}
		out, _, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: f.launcher, path: "/usr/bin:/bin"}, f.argv())
		if code != 0 || len(seen) != len(f.candFiles) {
			t.Fatalf("export %d %v %s", code, len(seen), out)
		}
		for p, b := range f.candFiles {
			if !bytes.Equal(seen[p], b) {
				t.Fatalf("the export's %s is not the pinned candidate", p)
			}
		}
	})
	for name, c := range map[string]struct {
		prepare func(f *reviewFix)
		code    string
	}{
		"preexisting_scratch": {func(f *reviewFix) {
			os.MkdirAll(filepath.Join(f.co, reviewScratch, f.task), 0o700)
		}, "scratch_preexisting"},
		"head_mismatch": {func(f *reviewFix) { f.cand, f.seed = f.seed, f.cand }, "checkout_head_mismatch"},
		"modified_input": {func(f *reviewFix) {
			os.WriteFile(filepath.Join(f.co, "main.go"), []byte("package main\n"), 0o644)
		}, "input_modified"},
		"executable_input": {func(f *reviewFix) { os.Chmod(filepath.Join(f.co, "main.go"), 0o755) }, "input_modified"},
		"missing_input":    {func(f *reviewFix) { os.Remove(filepath.Join(f.co, "design.md")) }, "input_modified"},
		"unexpected_file":  {func(f *reviewFix) { os.WriteFile(filepath.Join(f.co, "notes.md"), []byte("x\n"), 0o644) }, "unexpected_file"},
		"unrelated_initial": {func(f *reviewFix) {
			other, _ := writeCommitMode(f.t, f.hub, map[string][]byte{"x": []byte("y")}, nil)
			copyObjects(f.hub, mustOpenRepo(f.t, f.co).Storer, []plumbing.Hash{other})
			f.seed = other
		}, "initial_source_unrelated"},
		"missing_initial": {func(f *reviewFix) { f.seed = plumbing.NewHash(strings.Repeat("ab", 20)) }, "initial_source_unrelated"},
		"no_git":          {func(f *reviewFix) { os.RemoveAll(filepath.Join(f.co, ".git")) }, "checkout_unsafe"},
		"go_unreadable":   {func(f *reviewFix) { f.spec.Go = filepath.Join(f.t.TempDir(), "go") }, "go_unreadable"},
		"deadline_passed": {func(f *reviewFix) { f.spec.Deadline = "2026-10-10T11:00:00Z" }, "deadline_passed"},
	} {
		t.Run("refuses_"+name, func(t *testing.T) {
			t.Parallel()
			f := newReviewFix(t)
			c.prepare(f)
			before := treeState(t, f.co)
			rec, _, code := f.run()
			if code != 1 || rec.Status != reviewRefused || rec.Code != c.code || len(rec.Commands) != 0 || len(f.launcher.started) != 0 {
				t.Fatalf("receipt %d %+v", code, rec)
			}
			if !maps2Equal(before, treeState(t, f.co)) {
				t.Fatal("a refusal changed the checkout")
			}
		})
	}
	for name, c := range map[string]struct {
		script reviewScript
		code   string
	}{
		"tests_fail": {reviewScript{testExit: 1}, "command_failed"},
		"coverage_below": {reviewScript{profile: func(m string, main []byte) []byte {
			return bytes.ReplaceAll(synthProfile(m, main), []byte(" 1\n"), []byte(" 0\n"))
		}}, "coverage_below_bar"},
		"coverage_garbled": {reviewScript{profile: func(string, []byte) []byte { return []byte("mode: atomic\n") }}, "coverage_unobserved"},
		"subtest_missing": {reviewScript{jsonStream: strings.ReplaceAll(passingTestJSON(), `"Test":"TestRun/unicode"`, `"Test":"TestRun/other"`)},
			"tests_failed"},
		"subtest_skipped": {reviewScript{jsonStream: strings.Replace(passingTestJSON(), `{"Action":"pass","Package":"example.com/greeting","Test":"TestRun/help"`,
			`{"Action":"skip","Package":"example.com/greeting","Test":"TestRun/help"`, 1)}, "tests_failed"},
	} {
		t.Run("fails_"+name, func(t *testing.T) {
			t.Parallel()
			f := newReviewFix(t)
			f.script = c.script
			rec, _, code := f.run()
			if code != 1 || rec.Status != reviewFailed || rec.Code != c.code || !rec.ScratchRemoved {
				t.Fatalf("receipt %d %+v", code, rec)
			}
		})
	}
	t.Run("group_not_gone", func(t *testing.T) {
		t.Parallel()
		// A command whose group outlives every signal: nothing more runs,
		// and the scratch is kept rather than removed under a live child.
		f := newReviewFix(t)
		inner := f.script.script
		f.launcher = &fakeLauncher{script: func(s ProcSpec) behavior {
			b := inner(s)
			if strings.Join(s.Args, " ") == "test ./..." {
				b.lingering, b.ignoreTerm, b.ignoreKill = true, true, true
			}
			return b
		}}
		out, _, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: f.launcher, path: "/usr/bin:/bin"}, f.argv())
		var rec DevReviewRun
		json.Unmarshal([]byte(out), &rec)
		if code != 1 || rec.Code != "process_not_gone" || rec.ScratchRemoved || len(rec.Commands) != 2 {
			t.Fatalf("receipt %d %s", code, out)
		}
	})
	t.Run("invocation", func(t *testing.T) {
		t.Parallel()
		f := newReviewFix(t)
		good := f.argv()
		set := func(flag, v string) []string {
			a := slices.Clone(good)
			a[slices.Index(a, flag)+1] = v
			return a
		}
		for name, argv := range map[string][]string{
			"extra_flag":       append(slices.Clone(good), "--x", "y"),
			"missing_flag":     good[:len(good)-2],
			"bad_task":         set("--task", "t_1"),
			"short_commit":     set("--candidate", f.cand.String()[:12]),
			"upper_commit":     set("--initial-source", strings.ToUpper(f.seed.String())),
			"relative_go":      set("--go", "bin/go"),
			"unclean_go":       set("--go", "/usr/bin/../bin/go"),
			"injected_go":      set("--go", "/usr/bin/go;rm"),
			"spaced_go":        set("--go", "/usr/bin/g o"),
			"offset_deadline":  set("--deadline", "2026-10-10T13:00:00+00:00"),
			"fraction":         set("--deadline", "2026-10-10T13:00:00.5Z"),
			"lower_z":          set("--deadline", "2026-10-10T13:00:00z"),
			"invalid_calendar": set("--deadline", "2026-02-30T13:00:00Z"),
		} {
			out, errOut, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: &fakeLauncher{script: f.script.script}, path: "/bin"}, argv)
			if code != 2 || out != "" || errOut == "" {
				t.Errorf("%s: exit %d %q", name, code, out)
			}
		}
		if _, ok := parseHelperDeadline("2026-10-10T13:00:00Z"); !ok {
			t.Fatal("the canonical deadline is refused")
		}
		at := time.Date(2026, 10, 10, 13, 0, 0, 999_000_000, time.FixedZone("x", 3600))
		if got := formatHelperDeadline(at); got != "2026-10-10T12:00:00Z" {
			t.Fatalf("deadline %s", got)
		}
	})
	t.Run("live_env_refuses_outside_a_checkout", func(t *testing.T) {
		t.Parallel()
		// The live helper world (no injected env) inspects its working
		// directory first; the package directory is no reviewer checkout.
		f := newReviewFix(t)
		var out, errb bytes.Buffer
		code := MainWith(Host{Args: f.argv()[1:], Stdout: &out, Stderr: &errb})
		var rec DevReviewRun
		if err := json.Unmarshal(out.Bytes(), &rec); err != nil || code != 1 || rec.Status != reviewRefused || rec.Code != "checkout_unsafe" {
			t.Fatalf("live helper %d %s %s", code, out.String(), errb.String())
		}
	})
	t.Run("receipt_bound", func(t *testing.T) {
		t.Parallel()
		rec := DevReviewRun{Schema: DevReviewRunSchema, Status: reviewPassed, Commands: []ReviewCommand{{Name: "go_test"}}}
		base, _ := encodeReviewRun(rec)
		// Exactly at the bound the receipt stands; one byte over drops the
		// outputs and fails.
		rec.Commands[0].Stdout = strings.Repeat("a", maxReviewRunBytes-len(base))
		out, code := encodeReviewRun(rec)
		if len(out) != maxReviewRunBytes || code != 0 {
			t.Fatalf("at the bound %d %d", len(out), code)
		}
		rec.Commands[0].Stdout += "a"
		out, code = encodeReviewRun(rec)
		var got DevReviewRun
		json.Unmarshal(out, &got)
		if code != 1 || !got.OutputsOmitted || got.Code != "receipt_overflow" || got.Commands[0].Stdout != "" || len(out) > maxReviewRunBytes {
			t.Fatalf("over the bound %d %+v", code, got)
		}
	})
	t.Run("streams_capped", func(t *testing.T) {
		t.Parallel()
		f := newReviewFix(t)
		inner := f.script.script
		f.launcher = &fakeLauncher{script: func(s ProcSpec) behavior {
			b := inner(s)
			if strings.Join(s.Args, " ") == "test ./..." {
				b.stdout = strings.Repeat("x", reviewStreamCap+1)
			}
			return b
		}}
		out, _, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: f.launcher, path: "/bin"}, f.argv())
		var rec DevReviewRun
		json.Unmarshal([]byte(out), &rec)
		if code != 1 || rec.Code != "output_truncated" || len(rec.Commands[1].Stdout) != reviewStreamCap {
			t.Fatalf("receipt %d %s", code, rec.Code)
		}
	})
}

// mustOpenRepo opens a checkout's repository.
func mustOpenRepo(t testing.TB, dir string) *git.Repository {
	t.Helper()
	r, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// evidenceInput is the trusted derivation input of a helper run in f whose
// output is out, recorded by command.
func (f *reviewFix) evidenceInput(command, out string, exit int, marker string) reviewEvidenceInput {
	log := codexLog(command, out, exit)
	var rec DevReviewRun
	json.Unmarshal([]byte(out), &rec)
	pct := "91.5"
	if rec.Coverage != nil {
		pct = rec.Coverage.Percent
	}
	var exits [2]int
	if c := commandNamed(rec.Commands, "go_test"); c != nil {
		exits[0] = c.Exit
	}
	if c := commandNamed(rec.Commands, "go_test_cover"); c != nil {
		exits[1] = c.Exit
	}
	report := fmt.Sprintf("# Review\n\nVerification exit %d: go test ./...\nVerification exit %d: go test -coverprofile=%s/coverage.out ./...\nCoverage: %s%% of the changed statements\n",
		exits[0], exits[1], rec.Scratch, pct)
	return reviewEvidenceInput{Ordinal: 4, Log: log, LogFile: "tasks/04/logs.txt", Meta: completeMeta(log), Task: f.task, Helper: f.spec,
		Candidate: f.cand.String(), Initial: f.seed.String(), CandTree: f.tree(), CandFiles: f.treeFiles(), SeedMain: f.seedFiles["main.go"],
		AdmittedAt: newAutoClock().Now().Add(-time.Second), Marker: marker, Report: report}
}

// completeMeta is the metadata of a log retained whole.
func completeMeta(log []byte) DevLogMeta {
	n := len(log)
	return DevLogMeta{SourceBytes: n, RetainedBytes: n, DataBytes: n, StoredBytes: n, RawSHA256: sha256Hex(log)}
}

func TestDevFullReviewEvidence(t *testing.T) {
	t.Parallel()
	f := newReviewFix(t)
	_, passOut, code := f.run()
	if code != 0 {
		t.Fatal("the passing run failed")
	}
	plain := strings.Join(f.argv(), " ")
	bash := "/bin/bash -lc '" + plain + "'"
	failF := newReviewFix(t)
	failF.task, failF.spec, failF.cand, failF.seed = f.task, f.spec, f.cand, f.seed
	failF.co = t.TempDir()
	pinReviewCheckout(t, failF.co, f.hub, f.cand, f.candFiles)
	failF.hub, failF.candFiles, failF.seedFiles = f.hub, f.candFiles, f.seedFiles
	failF.script = reviewScript{testExit: 1}
	_, failOut, fcode := failF.run()
	if fcode != 1 {
		t.Fatal("the failing run passed")
	}
	derive := func(command, out string, exit int, marker string, edit func(in *reviewEvidenceInput)) DevReviewEvidence {
		in := f.evidenceInput(command, out, exit, marker)
		if edit != nil {
			edit(&in)
		}
		return DeriveDevReviewEvidence(in)
	}
	t.Run("verified", func(t *testing.T) {
		for _, cmd := range []string{plain, bash} {
			ev := derive(cmd, passOut, 0, markerReviewApproved, nil)
			if ev.Status != evidenceVerified || ev.RunSHA256 != sha256Hex([]byte(passOut)) || ev.Started == nil || ev.Completed == nil ||
				ev.Started.FirstLine != 4 || ev.Completed.FirstLine != 5 || ev.runDigest() != ev.RunSHA256 {
				t.Fatalf("%s: %+v", cmd, ev)
			}
			if _, blocks := ev.blocks(); blocks {
				t.Fatal("a verified approval blocks")
			}
		}
	})
	t.Run("failed_run_permits_changes_requested", func(t *testing.T) {
		ev := derive(plain, failOut, 1, markerReviewChanges, nil)
		if ev.Status != evidenceFailedRun || ev.Code != "command_failed" || ev.runDigest() == "" {
			t.Fatalf("%+v", ev)
		}
		if _, blocks := ev.blocks(); blocks {
			t.Fatal("a failed run blocks changes requested")
		}
		if ev := derive(plain, failOut, 1, markerReviewApproved, nil); ev.Code != "review_approved_failed_run" {
			t.Fatalf("approval over a failed run: %+v", ev)
		}
	})
	split := func(log []byte, f func(lines []string) []string) []byte {
		return []byte(strings.Join(f(strings.SplitAfter(string(log), "\n")), ""))
	}
	for name, c := range map[string]struct {
		edit   func(in *reviewEvidenceInput)
		status string
		code   string
	}{
		"truncated_log":  {func(in *reviewEvidenceInput) { in.Meta.Truncated = true }, evidenceUnobserved, "review_log_incomplete"},
		"dropped_bytes":  {func(in *reviewEvidenceInput) { in.Meta.DroppedBytes = 1 }, evidenceUnobserved, "review_log_incomplete"},
		"stored_changed": {func(in *reviewEvidenceInput) { in.Meta.StoredChanged = true }, evidenceUnobserved, "review_log_incomplete"},
		"merged_garbage": {func(in *reviewEvidenceInput) {
			in.Log = split(in.Log, func(l []string) []string { h := len(l[4]) / 2; l[4] = l[4][:h] + "\nnoise\n" + l[4][h:]; return l })
		}, evidenceUnobserved, "review_log_ambiguous"},
		"prose_only": {func(in *reviewEvidenceInput) {
			in.Log = []byte(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "i", "type": "agent_message", "text": "I ran " + plain}}))
		}, evidenceUnobserved, "review_helper_not_started"},
		"nested_events": {func(in *reviewEvidenceInput) {
			in.Log = []byte(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "i", "type": "command_execution",
				"command": "cat log", "aggregated_output": string(in.Log), "exit_code": 0, "status": "completed"}}))
		}, evidenceUnobserved, "review_helper_not_started"},
		"start_only": {func(in *reviewEvidenceInput) {
			in.Log = split(in.Log, func(l []string) []string { return append(l[:4], l[5:]...) })
		}, evidenceMismatch, "review_command_records_invalid"},
		"twice": {func(in *reviewEvidenceInput) {
			in.Log = append(slices.Clone(in.Log), in.Log...)
		}, evidenceMismatch, "review_command_records_invalid"},
		"wrong_task":      {func(in *reviewEvidenceInput) { in.Task = hexID("t_", 99) }, evidenceMismatch, "review_command_mismatch"},
		"wrong_candidate": {func(in *reviewEvidenceInput) { in.Candidate = f.seed.String() }, evidenceMismatch, "review_command_mismatch"},
		"wrapped": {func(in *reviewEvidenceInput) {
			in.Log = bytes.ReplaceAll(in.Log, []byte(`"command":"`+plain), []byte(`"command":"cd /x && `+plain))
		}, evidenceMismatch, "review_command_mismatch"},
		"receipt_changed": {func(in *reviewEvidenceInput) {
			in.Log = bytes.Replace(in.Log, []byte(`\"status\":\"passed\"`), []byte(`\"status\":\"failed\"`), 1)
		}, evidenceMismatch, "review_status_mismatch"},
		"profile_mismatch": {func(in *reviewEvidenceInput) {
			in.Log = bytes.Replace(in.Log, []byte(`\"present\":true,\"sha256\":\"`), []byte(`\"present\":true,\"sha256\":\"0`), 1)
		}, evidenceMismatch, "review_profile_mismatch"},
		"other_seed":     {func(in *reviewEvidenceInput) { in.SeedMain = []byte(fixtureMain) }, evidenceMismatch, "review_coverage_mismatch"},
		"late_admission": {func(in *reviewEvidenceInput) { in.AdmittedAt = in.AdmittedAt.Add(time.Hour) }, evidenceMismatch, "review_receipt_time"},
		"finished_first": {func(in *reviewEvidenceInput) { in.FinishedAt = in.AdmittedAt }, evidenceMismatch, "review_receipt_time"},
		"report_exit": {func(in *reviewEvidenceInput) {
			in.Report = strings.Replace(in.Report, "Verification exit 0: go test ./...", "Verification exit 1: go test ./...", 1)
		}, evidenceMismatch, "review_report_exit_mismatch"},
		"report_coverage": {func(in *reviewEvidenceInput) {
			in.Report = strings.Replace(in.Report, "Coverage: ", "Coverage: 99.9% and ", 1)
		}, evidenceMismatch, "review_report_coverage_mismatch"},
		"inputs_differ": {func(in *reviewEvidenceInput) {
			in.CandFiles = withTreeFile(in.CandFiles, "design.md", "# Other\n")
		}, evidenceMismatch, "review_inputs_mismatch"},
		"tree_differs": {func(in *reviewEvidenceInput) { in.CandTree = strings.Repeat("0", 40) }, evidenceMismatch, "review_receipt_binding"},
		"go_differs":   {func(in *reviewEvidenceInput) { in.Helper.Go = "/usr/local/go/bin/go" }, evidenceMismatch, "review_command_mismatch"},
		"incomplete": {func(in *reviewEvidenceInput) {
			in.Log = bytes.Replace(in.Log, []byte(`"exit_code":0,"id":"item_1","status":"completed"`), []byte(`"exit_code":null,"id":"item_1","status":"completed"`), 1)
		}, evidenceMismatch, "review_command_incomplete"},
		"oversized_receipt": {func(in *reviewEvidenceInput) {
			in.Log = codexLog(plain, strings.Repeat("a", maxReviewRunBytes)+"\n", 0)
			in.Meta = completeMeta(in.Log)
		}, evidenceUnobserved, "review_receipt_incomplete"},
	} {
		t.Run(name, func(t *testing.T) {
			ev := derive(plain, passOut, 0, markerReviewApproved, c.edit)
			if ev.Status != c.status || ev.Code != c.code {
				t.Fatalf("%s/%s, want %s/%s", ev.Status, ev.Code, c.status, c.code)
			}
			if _, blocks := ev.blocks(); !blocks {
				t.Fatal("an unverified approval does not block")
			}
		})
	}
	t.Run("no_helper_changes_requested", func(t *testing.T) {
		in := f.evidenceInput(plain, passOut, 0, markerReviewChanges)
		in.Log = []byte(codexRecord(map[string]any{"type": "turn.started"}))
		in.Meta = completeMeta(in.Log)
		ev := DeriveDevReviewEvidence(in)
		if _, blocks := ev.blocks(); blocks || ev.Status != evidenceUnobserved || ev.runDigest() != "" {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("hash_field_grammar", func(t *testing.T) {
		files := f.treeFiles()
		var b strings.Builder
		fmt.Fprintf(&b, "# Review\n\nReviewed candidate: %s\n", f.cand)
		for _, p := range devSourceFiles {
			fmt.Fprintf(&b, "Source sha256: %s %s\n", p, sha256Hex(f.candFiles[p]))
		}
		scratch := filepath.Join(f.co, reviewScratch, f.task)
		fmt.Fprintf(&b, "Verification exit 0: go test ./...\nVerification exit 0: go test -coverprofile=%s/coverage.out ./...\n", scratch)
		b.WriteString("Coverage: 91.5% of the changed statements\nCleanup: scratch removed and verified absent (true)\n")
		digest := sha256Hex([]byte(passOut))
		body := b.String()
		ok := body + "Reviewer run sha256: " + digest + "\n\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + markerReviewApproved + "\n"
		if err := ReviewReportOK(ok, markerReviewApproved, f.cand.String(), f.task, files, digest); err != nil {
			t.Fatalf("the canonical report is refused: %v", err)
		}
		for name, report := range map[string]string{
			"absent":   body + "\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + markerReviewApproved + "\n",
			"upper":    body + "Reviewer run sha256: " + strings.ToUpper(digest) + "\n\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + markerReviewApproved + "\n",
			"repeated": body + "Reviewer run sha256: " + digest + "\nReviewer run sha256: " + digest + "\n\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + markerReviewApproved + "\n",
			"other":    body + "Reviewer run sha256: " + strings.Repeat("0", 64) + "\n\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + markerReviewApproved + "\n",
			"suffix":   body + "Reviewer run sha256: " + digest + " (ok)\n\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + markerReviewApproved + "\n",
		} {
			if ReviewReportOK(report, markerReviewApproved, f.cand.String(), f.task, files, digest) == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		// A run the log does not corroborate cannot be named at all.
		if ReviewReportOK(ok, markerReviewApproved, f.cand.String(), f.task, files, "") == nil {
			t.Error("an uncorroborated run was accepted")
		}
	})
}

// withTreeFile returns files with path's content replaced.
func withTreeFile(files map[string]TreeFile, path, content string) map[string]TreeFile {
	out := map[string]TreeFile{}
	for k, v := range files {
		out[k] = v
	}
	out[path] = TreeFile{Path: path, Content: []byte(content)}
	return out
}

// covSrc is a candidate main.go for the coverage derivation.
const covSrc = `package main

var greet = func() int {
	return 1
}

func f() int {
	a := 1
	a++
	a++
	a++
	return a
}

func g() int {
	return 0
}

func main() {}
`

// covProfile is a mode: set profile of src whose blocks are one per
// function body (and the var's literal), counts from cover.
func covProfile(src string, cover map[string]int) []byte {
	p := synthProfile("example.com/greeting", []byte(src))
	lines := strings.Split(strings.TrimSuffix(string(p), "\n"), "\n")
	out := []string{lines[0]}
	names := []string{}
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, "func ") {
			names = append(names, strings.Fields(strings.TrimPrefix(l, "func "))[0])
		}
	}
	for i, l := range lines[1:] {
		f := strings.Fields(l)
		name := strings.TrimSuffix(names[i], "()")
		if n, ok := cover[name]; ok {
			f[2] = itoa(n)
		}
		out = append(out, strings.Join(f, " "))
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

func TestDevFullCoverage(t *testing.T) {
	t.Parallel()
	const module = "example.com/greeting"
	seed := []byte("package main\n\nfunc main() {}\n")
	t.Run("threshold", func(t *testing.T) {
		// f's five statements covered, g's one not: 5/6 passes.
		cov, err := deriveChangedCoverage(seed, []byte(covSrc), covProfile(covSrc, map[string]int{"f": 1, "g": 0}), module)
		if err != nil || !cov.Pass || cov.Covered != 5 || cov.Total != 6 || cov.Percent != "83.3" ||
			!slices.Equal(cov.Functions, []string{"var greet", "func f", "func g"}) {
			t.Fatalf("%+v %v", cov, err)
		}
		// Exactly 80% (four of five) is not above the bar.
		four := strings.Replace(covSrc, "\ta++\n", "", 1)
		cov, err = deriveChangedCoverage(seed, []byte(four), covProfile(four, map[string]int{"f": 1, "g": 0}), module)
		if err != nil || cov.Pass || cov.Covered != 4 || cov.Total != 5 || cov.Percent != "80.0" {
			t.Fatalf("exactly 80: %+v %v", cov, err)
		}
		// The display truncates and never decides: 1/6 is 16.6.
		cov, _ = deriveChangedCoverage(seed, []byte(covSrc), covProfile(covSrc, map[string]int{"f": 0, "g": 1}), module)
		if cov.Percent != "16.6" || cov.Pass {
			t.Fatalf("%+v", cov)
		}
	})
	t.Run("unchanged_function_excluded", func(t *testing.T) {
		// g differs from the seed only by a comment and layout: excluded.
		seedG := []byte("package main\n\n// g is unchanged.\nfunc g() int {\n\n\treturn 0\n}\n\nfunc main() {}\n")
		cov, err := deriveChangedCoverage(seedG, []byte(covSrc), covProfile(covSrc, map[string]int{"f": 1, "g": 0}), module)
		if err != nil || cov.Total != 5 || cov.Covered != 5 || slices.Contains(cov.Functions, "func g") || !slices.Contains(cov.Functions, "var greet") {
			t.Fatalf("%+v %v", cov, err)
		}
	})
	t.Run("changed_initializer", func(t *testing.T) {
		seedVar := []byte("package main\n\nvar greet = func() int {\n\treturn 2\n}\n\nfunc main() {}\n")
		cov, err := deriveChangedCoverage(seedVar, []byte(covSrc), covProfile(covSrc, map[string]int{"f": 1, "g": 1}), module)
		if err != nil || !slices.Contains(cov.Functions, "var greet") {
			t.Fatalf("%+v %v", cov, err)
		}
		same := []byte(covSrc)
		cov, err = deriveChangedCoverage(same, []byte(covSrc), covProfile(covSrc, nil), module)
		if err == nil || !strings.Contains(err.Error(), "no changed executable statement") {
			t.Fatalf("an unchanged candidate %+v %v", cov, err)
		}
	})
	good := covProfile(covSrc, map[string]int{"f": 1})
	lines := strings.SplitAfter(string(good), "\n")
	for name, c := range map[string]struct {
		profile string
		src     string
		module  string
	}{
		"empty":        {"", covSrc, module},
		"no_final_lf":  {strings.TrimSuffix(string(good), "\n"), covSrc, module},
		"mode":         {"mode: count\n" + strings.Join(lines[1:], ""), covSrc, module},
		"malformed":    {string(good) + "garbage\n", covSrc, module},
		"other_file":   {string(good) + strings.Replace(lines[1], module+"/main.go", module+"/x.go", 1), covSrc, module},
		"huge_number":  {"mode: set\n" + module + "/main.go:1.1,1.2 1234567890 1\n", covSrc, module},
		"disagreeing":  {string(good) + strings.Replace(lines[2], " 1\n", " 0\n", 1), covSrc, module},
		"overlap":      {string(good) + module + "/main.go:8.3,9.5 1 1\n", covSrc, module},
		"out_of_range": {string(good) + module + "/main.go:99.1,99.2 1 1\n", covSrc, module},
		"wide_column":  {string(good) + module + "/main.go:20.1,20.90 1 1\n", covSrc, module},
		"reversed":     {"mode: set\n" + module + "/main.go:8.5,8.3 1 1\n", covSrc, module},
		"zero_stmts":   {"mode: set\n" + module + "/main.go:8.2,9.3 0 1\n", covSrc, module},
		"no_decl":      {"mode: set\n" + module + "/main.go:1.1,1.5 1 1\n", covSrc, module},
		"no_module":    {string(good), covSrc, ""},
		"unparsable":   {"mode: set\n", "package main\nfunc {\n", module},
		"missing_func": {"mode: set\n" + lines[1] + lines[3], covSrc, module},
	} {
		t.Run("rejects_"+name, func(t *testing.T) {
			if cov, err := deriveChangedCoverage(seed, []byte(c.src), []byte(c.profile), c.module); err == nil {
				t.Fatalf("accepted %+v", cov)
			}
		})
	}
	t.Run("repeat_counts_once", func(t *testing.T) {
		p := string(good) + lines[2]
		a, err1 := deriveChangedCoverage(seed, []byte(covSrc), good, module)
		b, err2 := deriveChangedCoverage(seed, []byte(covSrc), []byte(p), module)
		if err1 != nil || err2 != nil || a.Total != b.Total || a.Covered != b.Covered {
			t.Fatalf("%+v %+v %v %v", a, b, err1, err2)
		}
		if _, err := deriveChangedCoverage([]byte("package main\nfunc {"), []byte(covSrc), good, module); err == nil {
			t.Fatal("an unparsable seed was accepted")
		}
	})
	// The format the Go toolchain itself writes is exercised by FP-4's real
	// helper run, which derives this coverage from a real profile.
}

func TestDevFullOracle(t *testing.T) {
	t.Parallel()
	t.Run("candidate_after_fix_loop", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		m := bundleDoc[DevManifest](t, dir, devManifestFile)
		_, fix := taskOf(t, dir, 5)
		_, first := taskOf(t, dir, 3)
		cases := bundleDoc[CasesRecord](t, dir, "validation/cases.json")
		if m.FinalCandidate != fix || cases.Commit != fix || fix == first {
			t.Fatalf("the oracle did not judge the fix loop's candidate: %s", m.FinalCandidate)
		}
	})
	t.Run("each_case_independently", func(t *testing.T) {
		t.Parallel()
		// Each case broken alone (its recorded run exits 3, which no case
		// kind accepts, while the record still claims a pass) fails exactly
		// its own case row. The decoded record is replaced in memory, one
		// case at a time, on one loaded bundle.
		dir := devPassingBundle(t)
		b, err := LoadDevBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		orig := doc[CasesRecord](b, "validation/cases.json")
		defer func() { b.docs["validation/cases.json"] = orig }()
		for i, fc := range FeatureCases {
			broken := *orig
			broken.Cases = slices.Clone(orig.Cases)
			broken.Cases[i].Exec.ExitCode = 3
			b.docs["validation/cases.json"] = &broken
			res := checkDevBundle(b, true)
			if res.report.Result == ResultPass || !slices.Contains(devCriterion(res.report, "feature_oracle").Codes, CodeCaseFailed) {
				t.Fatalf("case %s: %v", fc.ID, devCodes(res.report))
			}
			for _, row := range res.audit.Rows {
				if strings.HasPrefix(row.ID, "case/") && (row.ID == "case/"+fc.ID) != (row.Verdict == VerdictIncorrect) {
					t.Fatalf("case %s broken: row %s is %s", fc.ID, row.ID, row.Verdict)
				}
			}
		}
	})
	appendTest := func(line string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			p := filepath.Join(dir, "validation", "tests.jsonl")
			b := mustRead(t, p)
			os.WriteFile(p, append(b, line...), 0o600)
		}
	}
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"extra_failing_test": appendTest(`{"Action":"fail","Package":"example.com/greeting","Test":"TestExtra","Elapsed":0}` + "\n"),
		"skipped_subtest": func(t *testing.T, dir string) {
			rewriteFile(t, dir, "validation/tests.jsonl", `"Action":"pass","Package":"example.com/greeting","Test":"TestRun/unicode"`,
				`"Action":"skip","Package":"example.com/greeting","Test":"TestRun/unicode"`)
		},
		"wrong_profile": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "validation", "coverage.out")
			os.WriteFile(p, bytes.ReplaceAll(mustRead(t, p), []byte(" 1\n"), []byte(" 0\n")), 0o600)
		},
		"wrong_toolchain": func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/final-test.json", func(m map[string]any) { m["toolchain"] = "go version go1.1 linux/amd64" })
		},
		"requirements_claim": func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/requirements.json", func(m map[string]any) { m["run_signature"] = false })
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			mutate(t, dir)
			if code, rep := reforge(t, dir); code == 0 || rep.Result == ResultPass {
				t.Fatalf("accepted: %v", devCodes(rep))
			}
		})
	}
	t.Run("signature_and_table", func(t *testing.T) {
		t.Parallel()
		// A candidate without the run signature, or without its table test,
		// fails the oracle live and offline although every worker approves.
		for alter, want := range map[string]string{"no-signature": CodeTestMapping, "no-table": CodeTestMapping} {
			r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete, alter: alter},
				{marker: markerReviewApproved}})
			r.owner = []string{"yes 1"}
			r.reviewGo.profile = func(m string, main []byte) []byte { return synthProfile(m, main) }
			r.expect(1, want)
			if code, rep := r.stored(); code != 1 || len(devCriterion(rep, "feature_oracle").Codes) == 0 {
				t.Fatalf("%s offline %d %v", alter, code, devCodes(rep))
			}
		}
	})
	t.Run("final_coverage", func(t *testing.T) {
		t.Parallel()
		// The final candidate's own coverage run decides: at or below the
		// bar, or no usable profile, stops the attempt while the review's
		// receipt (its own scratch run) still passed.
		for stop, profile := range map[string]func(string, []byte) []byte{
			CodeCoverageBelow: func(m string, main []byte) []byte {
				return bytes.ReplaceAll(synthProfile(m, main), []byte(" 1\n"), []byte(" 0\n"))
			},
			CodeCoverageUnobserved: func(string, []byte) []byte { return []byte("mode: count\n") },
		} {
			r := newDevRig(t, happy())
			r.owner = []string{"yes 1"}
			r.f.profile = profile
			r.expect(1, stop)
			if code, rep := r.stored(); code != 1 || len(devCriterion(rep, "coverage").Codes) == 0 {
				t.Fatalf("%s: %d %v", stop, code, devCodes(rep))
			}
		}
	})
	t.Run("shared_deadline", func(t *testing.T) {
		t.Parallel()
		start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
		if got := finalDeadline(start, start.Add(-AttemptBound+30*time.Second)); !got.Equal(start.Add(30 * time.Second)) {
			t.Fatalf("the attempt deadline does not win: %s", got)
		}
		if got := finalDeadline(start, start); !got.Equal(start.Add(FinalTestBound)) {
			t.Fatalf("the 60s bound does not win: %s", got)
		}
	})
}
