package reale2e

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// auditOrder is the fixed ordered row inventory.
func auditOrder() []string {
	out := []string{"preflight", "deployment_roles", "coordinator_phase1"}
	for i := range Hops {
		n := "hop" + hopDir(i)
		if i == 2 {
			out = append(out, "signoff", "coordinator_phase2")
		}
		out = append(out, n+"_dispatch", n+"_wait", n+"_result")
		switch i {
		case 0:
			for _, c := range FeatureCases {
				out = append(out, n+"_design_case_"+c.ID)
			}
			out = append(out, n+"_design_run_signature", n+"_design_table_tests")
		case 1:
			out = append(out, n+"_review_verdict")
		case 2:
			for _, c := range FeatureCases {
				out = append(out, n+"_case_"+c.ID)
			}
			for _, c := range FeatureCases {
				out = append(out, n+"_test_"+c.ID)
			}
			out = append(out, n+"_run_signature", n+"_table_tests", n+"_assertions", n+"_completeness")
		case 3:
			out = append(out, n+"_review_marker", n+"_inline_sources", n+"_verdict_tests", n+"_review_quality")
		}
	}
	return append(out, "final_tests", "cleanup", "owner_environment")
}

// auditOf loads a bundle and builds its audit from its recomputed report.
func auditOf(t *testing.T, b string) (Audit, Report) {
	t.Helper()
	bl, err := LoadBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	r := BuildReport(bl, Check(bl))
	return BuildAudit(bl, r), r
}

// rowByID returns an audit row.
func rowByID(t *testing.T, a Audit, id string) AuditRow {
	t.Helper()
	for _, r := range a.Rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no audit row %s", id)
	return AuditRow{}
}

// TestPostRunAudit (FP-12): the pure audit over captured evidence: every
// fixed row in order, positive complete evidence correct, contradictions
// incorrect with their failure codes, missing proof unobserved, judgment
// rows always pending owner/coordinator review (approving prose and green
// CLI cases notwithstanding), stored derived rows rechecked, an appended
// audit-review.md leaving the original evidence valid, and no process
// executed.
func TestPostRunAudit(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		b := passingBundle(t)
		a, r := auditOf(t, b)
		var ids []string
		for _, row := range a.Rows {
			ids = append(ids, row.ID)
			switch row.Layer {
			case LayerMechanical:
				if row.Verdict != VerdictCorrect || row.Correction != "" || len(row.Evidence) == 0 {
					t.Errorf("mechanical row %+v", row)
				}
			case LayerJudgment:
				if row.Verdict != VerdictUnobserved || row.Correction != PendingReview || len(row.FPs) == 0 {
					t.Errorf("judgment row %+v", row)
				}
			default:
				t.Errorf("row %s layer %q", row.ID, row.Layer)
			}
			for _, e := range row.Evidence {
				if !hex64RE.MatchString(e.SHA256) {
					t.Errorf("row %s evidence %+v", row.ID, e)
				}
			}
		}
		if !slices.Equal(ids, auditOrder()) {
			t.Fatalf("rows %v\nwant %v", ids, auditOrder())
		}
		data, _ := encodeJSON(r)
		if a.Schema != AuditSchema || a.Scope != AuditScope || a.Mechanical != ResultPass || a.ResultReview != PendingReview || a.ReportSHA256 != sha256Hex(data) ||
			len(a.Sources) != len(r.Artifacts) || a.RunID != r.RunID {
			t.Fatalf("audit header %+v", a)
		}
		// The stored files are exactly this derivation and its rendering.
		stored, _ := encodeJSON(a)
		if !bytes.Equal(stored, mustRead(t, filepath.Join(b, "audit.json"))) || string(mustRead(t, filepath.Join(b, "audit.md"))) != RenderAudit(a) {
			t.Fatal("the stored audit is not its derivation")
		}
		md := RenderAudit(a)
		last := 0
		for _, id := range ids {
			i := strings.Index(md, ". "+id+" (")
			if i < last {
				t.Fatalf("audit.md order at %s", id)
			}
			last = i
		}
		if !strings.Contains(md, PendingReview) || !strings.Contains(md, "audit-review.md") {
			t.Fatal("audit.md does not state the pending review")
		}
		// Building it twice is byte-identical.
		again, _ := auditOf(t, b)
		if !bytes.Equal(mustJSON(a), mustJSON(again)) {
			t.Fatal("the audit is not deterministic")
		}
	})
	t.Run("an external review is appended", func(t *testing.T) {
		t.Parallel()
		b := passingBundle(t)
		before := treeDigest(t, b)
		writeBundleFile(t, b, "audit-review.md", []byte("# Review\nrun x, row hop01_design_case_unicode: unobserved -> correct (owner)\n"))
		code, r, _ := checkBundle(t, b)
		if code != 0 || r.Result != ResultPass || slices.ContainsFunc(r.Artifacts, func(f FileHash) bool { return f.Name == "audit-review.md" }) {
			t.Fatalf("with audit-review.md = %d %v", code, failedCodes(r))
		}
		removeBundleFile(t, b, "audit-review.md")
		if treeDigest(t, b) != before {
			t.Fatal("the review changed the original evidence")
		}
	})
	t.Run("falsified rows", func(t *testing.T) {
		t.Parallel()
		for name, f := range map[string]func(a *Audit){
			"verdict promoted": func(a *Audit) {
				for i := range a.Rows {
					if a.Rows[i].Layer == LayerJudgment {
						a.Rows[i].Verdict, a.Rows[i].Correction = VerdictCorrect, ""
					}
				}
			},
			"row omitted":    func(a *Audit) { a.Rows = a.Rows[1:] },
			"report digest":  func(a *Audit) { a.ReportSHA256 = zeros64 },
			"evidence moved": func(a *Audit) { a.Rows[0].Evidence = a.Rows[1].Evidence },
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				b := passingBundle(t)
				editDoc(t, b, "audit.json", f)
				code, r, _ := checkBundle(t, b)
				requireCode(t, name, code, r, CritResult, CodeDerivedMismatch)
			})
		}
		t.Run("audit missing", func(t *testing.T) {
			t.Parallel()
			b := passingBundle(t)
			removeBundleFile(t, b, "audit.json")
			code, r, _ := checkBundle(t, b)
			requireCode(t, "audit missing", code, r, CritResult, CodeDerivedMismatch)
		})
	})
	t.Run("contradictions", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			mutate func(t *testing.T, b string)
			row    string
			code   string
		}{
			"wrong pair": {func(t *testing.T, b string) {
				editTask(t, b, "hops/03", func(v *contract.TaskView) { v.Effective.Model = "claude-other" })
			}, "hop03_result", "selection_mismatch"},
			"wrong base": {func(t *testing.T, b string) {
				rebindHop(t, b, 1, func(w *contract.WorkspaceBinding) { w.BaseCommit = ptr(zeros40); w.BaseSelector = zeros40 })
			}, "hop02_dispatch", "base_binding_mismatch"},
			"wrong instance": {func(t *testing.T, b string) {
				rebindHop(t, b, 3, func(w *contract.WorkspaceBinding) { w.Instance = strings.Repeat("e", 32) })
			}, "hop04_dispatch", "workspace_instance_mismatch"},
			"disallowed path": {func(t *testing.T, b string) {
				files, _ := hopFilesAt(t, b, 1)
				_, seed := hopFilesAt(t, b, 0)
				addCommit(t, evidenceRepo(t, b), evidenceRefs[1], withFiles(files, map[string][]byte{"go.mod": []byte("module x\n")}), seed)
			}, "hop01_result", "disallowed_change"},
			"marker": {func(t *testing.T, b string) { setFinal(t, b, "hops/02", "Approved, I think.\n") }, "hop02_result", "final_marker_missing"},
			"truncation": {func(t *testing.T, b string) {
				editTask(t, b, "hops/01", func(v *contract.TaskView) { v.Result.FinalMessageTruncated = true })
			}, "hop01_result", "hop_not_succeeded"},
			"approval with failing tests": {func(t *testing.T, b string) {
				editDoc(t, b, "validation/final-test.json", func(r *TestRun) { r.ExitCode = 1 })
			}, "hop04_verdict_tests", CodeReviewContra},
			"missing unicode case": {func(t *testing.T, b string) {
				editCase(t, b, "unicode", func(c *CaseResult) {
					c.Stdout = "Hello, ??!\n"
					c.Exec.StdoutSHA256 = sha256Hex([]byte(c.Stdout))
					c.Pass, c.Code = false, "unexpected_result"
				})
			}, "hop03_case_unicode", CodeCaseFailed},
			"missing error subtest": {func(t *testing.T, b string) {
				lines := strings.Split(string(mustRead(t, filepath.Join(b, "validation", "tests.jsonl"))), "\n")
				lines = slices.DeleteFunc(lines, func(l string) bool { return strings.Contains(l, `"TestRun/empty_name"`) })
				writeBundleFile(t, b, "validation/tests.jsonl", []byte(strings.Join(lines, "\n")))
			}, "hop03_test_empty_name", CodeTestMapping},
			"inline source": {func(t *testing.T, b string) {
				editDoc(t, b, "hops/04/dispatch.json", func(d *DispatchRecord) { d.InlineFiles[2].SHA256 = zeros64 })
			}, "hop04_inline_sources", "inline_source_mismatch"},
			"protected change": {func(t *testing.T, b string) {
				rewriteEnvironment(t, b, func(e *envEdit) { e.change("claude", "CLAUDE.md", func(r *EnvRecord) { r.SHA256 = zeros64 }) })
			}, "owner_environment", CodeEnvModified},
			"phase exit": {func(t *testing.T, b string) {
				editDoc(t, b, coordinatorExitFile, func(c *CoordinatorExit) { c.Phases[1].ExitCode = ptr(3) })
			}, "coordinator_phase2", "coordinator_exit_nonzero"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				b := passingBundle(t)
				tc.mutate(t, b)
				a, _ := auditOf(t, b)
				row := rowByID(t, a, tc.row)
				if row.Verdict != VerdictIncorrect || !slices.Contains(row.Criteria, "code:"+tc.code) || !strings.Contains(row.Correction, tc.code) ||
					a.Mechanical != ResultFail {
					t.Fatalf("row %+v (mechanical %s)", row, a.Mechanical)
				}
				// A3.4: the environment row reports the detailed entries.
				if tc.row == "owner_environment" && !strings.Contains(row.Observed, "1 protected changes") ||
					tc.row == "owner_environment" && !strings.Contains(row.Observed, "differing entries detailed in diff.json") {
					t.Fatalf("environment row %q", row.Observed)
				}
				// The other deterministic case rows stay correct.
				if tc.row == "hop03_case_unicode" && rowByID(t, a, "hop03_case_name").Verdict != VerdictCorrect {
					t.Fatal("an unrelated case row changed")
				}
				// Judgments stay pending whatever the mechanics say.
				if r := rowByID(t, a, "hop03_assertions"); r.Verdict != VerdictUnobserved {
					t.Fatalf("judgment %+v", r)
				}
				code, rep, _ := checkBundle(t, b)
				if code != 1 || criterion(rep, CritResult).Result != "fail" {
					t.Fatalf("check = %d %v", code, failedCodes(rep))
				}
			})
		}
	})
	t.Run("early failure", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.term.lines = []string{"no"}
		f.coord.hopFiles[0] = map[string][]byte{"design.md": []byte("# Design\n\nIt covers every case, approved, all correct.\n\nSTATUS: DESIGN_DRAFT\n")}
		if code := f.attempt(); code != 1 {
			t.Fatalf("attempt = %d", code)
		}
		b := f.bundle()
		var a Audit
		readDoc(t, filepath.Join(b, "audit.json"), &a)
		var ids []string
		for _, r := range a.Rows {
			ids = append(ids, r.ID)
		}
		if !slices.Equal(ids, auditOrder()) || a.Mechanical != ResultFail {
			t.Fatalf("partial audit %v %s", ids, a.Mechanical)
		}
		for id, want := range map[string]string{"preflight": VerdictCorrect, "hop02_result": VerdictCorrect, "signoff": VerdictIncorrect,
			"coordinator_phase2": VerdictUnobserved, "hop03_dispatch": VerdictUnobserved, "hop04_result": VerdictUnobserved, "final_tests": VerdictUnobserved,
			"hop03_case_unicode": VerdictUnobserved, "hop01_design_case_unicode": VerdictUnobserved, "owner_environment": VerdictCorrect} {
			if r := rowByID(t, a, id); r.Verdict != want {
				t.Errorf("%s = %s (%s), want %s", id, r.Verdict, r.Observed, want)
			}
		}
		if r := rowByID(t, a, "hop01_design_case_unicode"); r.Correction != PendingReview {
			t.Fatalf("approving prose promoted a judgment: %+v", r)
		}
		out := f.stdout.String()
		if !strings.Contains(out, filepath.Join(b, "audit.md")) || !strings.Contains(out, "FAIL") {
			t.Fatalf("final output:\n%s", out)
		}
		if code, _, _ := checkBundle(t, b); code != 1 {
			t.Fatalf("check of the failed bundle = %d", code)
		}
	})
	t.Run("no evidence, no audit", func(t *testing.T) {
		t.Parallel()
		// An unusable evidence destination: FAIL with that cause and no
		// forbidden file is created to manufacture an audit.
		f := newFakeWorld(t)
		f.evidence = filepath.Join(t.TempDir(), "outside")
		if code := f.attempt(); code != 2 || !strings.Contains(f.stderr.String(), "--evidence") {
			t.Fatalf("attempt = %d %q", code, f.stderr.String())
		}
		if _, err := os.Stat(f.evidence); !os.IsNotExist(err) {
			t.Fatal("an evidence directory was created")
		}
	})
}
