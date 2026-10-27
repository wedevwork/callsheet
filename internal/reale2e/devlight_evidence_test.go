package reale2e

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Evidence-level unit tests of the light scenario (design
// 12a-dev-flow-light, Testing plan, Unit tests): the owner intake, the
// executing light review's evidence, replay authority, inert captures, the
// combined exercise's accounting and the scoped qualification. Every
// forged bundle starts from a passing control and recomputes its inventory
// and derived files, so the intended binding, never a stale hash, rejects
// it.

func TestDevLightIntake(t *testing.T) {
	t.Parallel()
	t.Run("authorized", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, lightHappy())
		r.expect(0, "")
		dir := r.f.bundle()
		dec := bundleDoc[devEnvelope[DevIntakeDecision]](t, dir, lightDecisionFile).Data
		rec := bundleDoc[devEnvelope[DevIntakeReceipt]](t, dir, lightReceiptFile).Data
		m := bundleDoc[DevManifest](t, dir, devManifestFile)
		feature := mustRead(t, filepath.Join(dir, filepath.FromSlash(capturedPath(lightFixtureCaptureDir+"/FEATURE.md"))))
		if dec.Kind != intakeLight || dec.Response != "light "+m.RunID || rec.SeedCommit != m.Workspace.SeedCommit || rec.SeedTree != m.Workspace.SeedTree ||
			rec.RequestSHA256 != sha256Hex(feature) || rec.SelectionSHA256 != m.SelectionSHA256 || rec.Scope != lightScope {
			t.Fatalf("decision %+v receipt %+v", dec, rec)
		}
		// Prompt, decision and receipt strictly precede every paid call.
		paid := eventSeqs(t, dir, EvTaskAdmitted, "preflight-", "")
		if len(paid) == 0 || !(rec.PromptSeq < rec.DecisionSeq && rec.DecisionSeq < rec.ReceiptSeq && rec.ReceiptSeq < slices.Min(paid)) {
			t.Fatalf("intake %d %d %d, first paid admission %v", rec.PromptSeq, rec.DecisionSeq, rec.ReceiptSeq, paid)
		}
	})
	for name, c := range map[string]struct {
		intake string
		stop   string
		kind   string
	}{
		"no":       {intakeNo, "owner_no", intakeNo},
		"eof":      {intakeEOF, "owner_eof", intakeEOF},
		"expired":  {"", "owner_expired", intakeExpired},
		"wrong_id": {"wrong", "owner_expired", intakeExpired},
		"buffered": {"buffered", "owner_expired", intakeExpired},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newLightRig(t, lightHappy())
			r.intake = c.intake
			if c.intake == "buffered" {
				// The authorizing line typed before the prompt (during the
				// seed baseline) is never a fresh decision.
				r.intake = ""
				prev := r.f.override
				typed := false
				r.f.override = func(s ProcSpec) (behavior, bool) {
					if !typed && r.d != nil && r.d.s.runID != "" && len(s.Args) > 0 && s.Args[0] == "version" {
						typed = true
						r.term.typeLine("light " + r.d.s.runID)
					}
					return prev(s)
				}
			}
			r.expect(1, c.stop)
			dir := r.f.bundle()
			if dec := bundleDoc[devEnvelope[DevIntakeDecision]](t, dir, lightDecisionFile).Data; dec.Kind != c.kind {
				t.Fatalf("decision %+v", dec)
			}
			if exists(dir, lightReceiptFile) || len(r.f.plane.ids()) != 0 {
				t.Fatalf("a receipt or a paid call without authorization: %d tasks", len(r.f.plane.ids()))
			}
			if code, rep, _ := devCheck(t, dir); code != 1 || devCriterion(rep, "light_intake").Result == ResultCriterionPass && c.kind == intakeLight {
				t.Fatalf("offline %d %v", code, devCodes(rep))
			}
			if c.intake == "buffered" && r.d.ignoredLines == 0 {
				t.Fatal("the buffered line was not discarded")
			}
		})
	}
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"request_digest": func(t *testing.T, dir string) {
			editJSON(t, dir, lightReceiptFile, func(m map[string]any) { m["data"].(map[string]any)["request_sha256"] = strings.Repeat("0", 64) })
		},
		"selection_digest": func(t *testing.T, dir string) {
			editJSON(t, dir, lightReceiptFile, func(m map[string]any) { m["data"].(map[string]any)["selection_sha256"] = strings.Repeat("0", 64) })
		},
		"seed": func(t *testing.T, dir string) {
			editJSON(t, dir, lightReceiptFile, func(m map[string]any) { m["data"].(map[string]any)["seed_commit"] = strings.Repeat("1", 40) })
		},
		"scope": func(t *testing.T, dir string) {
			editJSON(t, dir, lightReceiptFile, func(m map[string]any) { m["data"].(map[string]any)["scope"] = "anything/v1" })
		},
		"wrong_response": func(t *testing.T, dir string) {
			editJSON(t, dir, lightDecisionFile, func(m map[string]any) { m["data"].(map[string]any)["response"] = "yes please" })
		},
		"event_order": func(t *testing.T, dir string) {
			editJSON(t, dir, lightReceiptFile, func(m map[string]any) {
				d := m["data"].(map[string]any)
				d["prompt_seq"], d["decision_seq"] = d["decision_seq"], d["prompt_seq"]
			})
		},
		"receipt_removed": func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, filepath.FromSlash(lightReceiptFile))) },
	} {
		t.Run("forged_"+name, func(t *testing.T) {
			t.Parallel()
			dir := lightPassingBundle(t)
			mutate(t, dir)
			code, rep := lightReforge(t, dir)
			if code != 1 || devCriterion(rep, "light_intake").Result != ResultCriterionFail {
				t.Fatalf("exit %d %v", code, devCodes(rep))
			}
		})
	}
}

func TestDevLightReviewEvidence(t *testing.T) {
	t.Parallel()
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	for name, c := range map[string]struct {
		review devAct
		coder  devAct
		stop   string
		code   string
	}{
		// The full recipe's receipt, another task's argv, a transcript-only
		// claim and a missing helper never admit a light review.
		"full_receipt":   {devAct{marker: markerReviewApproved, alter: "full-receipt"}, devAct{marker: markerImplComplete}, CodeReviewMismatch, "review_command_mismatch"},
		"cross_task":     {devAct{marker: markerReviewApproved, alter: "cross-task"}, devAct{marker: markerImplComplete}, CodeReviewMismatch, "review_command_mismatch"},
		"quoted_only":    {devAct{marker: markerReviewApproved, alter: "prose-only"}, devAct{marker: markerImplComplete}, CodeReviewUnobserved, "review_helper_not_started"},
		"no_helper":      {devAct{marker: markerReviewApproved, alter: "no-helper"}, devAct{marker: markerImplComplete}, CodeReviewUnobserved, "review_helper_not_started"},
		"missing_field":  {devAct{marker: markerReviewApproved, alter: "missing-field"}, devAct{marker: markerImplComplete}, CodeReviewMismatch, "review_report_lines_mismatch"},
		"bad_run_digest": {devAct{marker: markerReviewApproved, alter: "bad-hash"}, devAct{marker: markerImplComplete}, CodeReviewMismatch, "review_report_lines_mismatch"},
		// A failed receipt (the candidate is still defective, or its test
		// has no regression) never admits an approval.
		"failed_receipt_still_defective": {devAct{marker: markerReviewApproved}, devAct{marker: markerImplComplete, alter: "still-defective"}, CodeReviewMismatch,
			"review_approved_failed_run"},
		"failed_receipt_no_regression": {devAct{marker: markerReviewApproved}, devAct{marker: markerImplComplete, alter: "no-regression"}, CodeReviewMismatch,
			"review_approved_failed_run"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newLightRig(t, []devAct{conf, c.coder, c.review})
			r.expect(1, c.stop)
			if !strings.Contains(r.d.stopReason, c.code) {
				t.Fatalf("stop reason %q, want %s", r.d.stopReason, c.code)
			}
			if code, _, _ := devCheck(t, r.f.bundle()); code != 1 {
				t.Fatalf("offline %d", code)
			}
		})
	}
	t.Run("rejection_recheck", func(t *testing.T) {
		t.Parallel()
		// A rejected light review's retained review.md is rechecked with the
		// light recipe: a review rejected for editing main.go has a sound
		// review.md and light receipt (the rest of the result is not
		// retained), never an unreproduced refusal by the full recipe.
		r := newLightRig(t, []devAct{conf, {marker: markerImplComplete}, {marker: markerReviewApproved, alter: "edit-main"}})
		r.expect(1, "publication_rejected")
		code, rep, _ := devCheck(t, r.f.bundle())
		row := rowNamed(bundleDoc[DevAudit](t, r.f.bundle(), "audit.json").Rows, "task/03/publication-rejection-recheck")
		if code != 1 || hasCode(rep, "rejection_unreproduced") || row == nil || !strings.Contains(row.Observation, "passes its review checks") {
			t.Fatalf("exit %d %v row %+v", code, devCodes(rep), row)
		}
	})
	t.Run("failed_receipt_supports_changes", func(t *testing.T) {
		t.Parallel()
		// A failed receipt supports a truthful changes-requested review: the
		// fix loop runs and the corrected candidate is approved.
		r := newLightRig(t, []devAct{conf, {marker: markerImplComplete, alter: "still-defective"}, {marker: markerReviewChanges}, {marker: markerFixesComplete},
			{marker: markerReviewApproved}})
		r.expect(0, "")
	})
	t.Run("derivation", func(t *testing.T) {
		t.Parallel()
		// The stored evidence re-derives exactly; each input binding change
		// is refused (no rerun of any helper).
		dir := lightPassingBundle(t)
		b, err := LoadDevBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := newDevChecker(b)
		c.loadTrees()
		n := 5
		p := doc[devEnvelope[TaskPlan]](b, taskDir(n)+"/plan.json").Data
		v := doc[contract.TaskShowResponse](b, taskDir(n)+"/task.json").Task
		marker, _ := parseTerminalMarker(*v.Result.FinalMessage)
		seed := c.lightSeedCommit()
		review := c.trees[*v.Result.Workspace.Commit]
		in := func() lightReviewInput {
			base := bundleReviewInput(b, taskDir(n), n, &v, b.Manifest.Helper, p.ExpectedBase, seed, c.treeHash[p.ExpectedBase], c.trees[p.ExpectedBase],
				fileContent(c.trees[seed], "main.go"), marker, fileText(review, "review.md"))
			return lightReviewInput{reviewEvidenceInput: base, SeedFiles: c.trees[seed], SeedTree: c.treeHash[seed], SeedCLI: true}
		}
		good := DeriveLightReviewEvidence(in())
		stored := doc[DevReviewRecord](b, taskDir(n)+"/review-evidence.json")
		if good.Status != evidenceVerified || !good.Approving || good.Light == nil || !good.Light.Comparison.Pass || mustJSONString(stored.Evidence) != mustJSONString(good) {
			t.Fatalf("the control %+v", good)
		}
		for name, mut := range map[string]func(x *lightReviewInput){
			"other_candidate": func(x *lightReviewInput) { x.Candidate = strings.Repeat("a", 40) },
			"other_seed":      func(x *lightReviewInput) { x.Initial = strings.Repeat("b", 40) },
			"changed_test": func(x *lightReviewInput) {
				x.CandFiles = withTreeFile(x.CandFiles, "main_test.go", "// changed\n"+string(x.CandFiles["main_test.go"].Content))
			},
			"seed_tree":      func(x *lightReviewInput) { x.SeedTree = strings.Repeat("c", 40) },
			"uncorroborated": func(x *lightReviewInput) { x.SeedCLI = false },
			"truncated_log":  func(x *lightReviewInput) { x.Meta.Truncated = true },
			"no_log":         func(x *lightReviewInput) { x.Log = nil },
			"other_helper":   func(x *lightReviewInput) { x.Helper.Go = "/elsewhere/go" },
			"other_task":     func(x *lightReviewInput) { x.Task = hexID("t_", 77) },
		} {
			x := in()
			mut(&x)
			if ev := DeriveLightReviewEvidence(x); ev.Status == evidenceVerified && ev.Approving {
				t.Errorf("%s: still a verified approval", name)
			}
		}
		// A full derivation of the light receipt is never a substitute.
		if ev := DeriveDevReviewEvidence(in().reviewEvidenceInput); ev.Status == evidenceVerified {
			t.Fatal("the full recipe verified a light review")
		}
	})
	t.Run("trailer", func(t *testing.T) {
		t.Parallel()
		r := newLightRig(t, lightHappy())
		r.expect(0, "")
		var line string
		for _, out := range r.helperOut {
			line = receiptLine(out)
		}
		if !strings.Contains(line, DevLightReviewRunSchema) {
			t.Fatalf("no light receipt: %q", line)
		}
		var run DevLightReviewRun
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			t.Fatal(err)
		}
		trailer, ok := renderLightReviewReportLines(run, []byte(line+"\n"))
		if !ok || !lightReportLinesComplete(trailer) {
			t.Fatalf("trailer %q", trailer)
		}
		var red string
		for _, l := range strings.Split(trailer, "\n") {
			if strings.HasPrefix(l, "Verification exit 1: red") {
				red = l
			}
		}
		if !strings.Contains(red, "expected exit 1") || !strings.Contains(red, "expected assertion failure") || strings.Count(trailer, "Verification exit ") != 6 {
			t.Fatalf("the red line %q of\n%s", red, trailer)
		}
		// An executed failure or refusal renders no approving trailer claim.
		run.Status, run.Code = "failed", "regression_not_green"
		if t2, _ := renderLightReviewReportLines(run, []byte(line+"\n")); strings.Contains(t2, "passed") && !strings.Contains(t2, "failed") {
			t.Fatalf("a failed run's trailer %q", t2)
		}
	})
}

// mustJSONString is v's JSON.
func mustJSONString(v any) string { return string(mustJSON(v)) }

func TestDevLightReplay(t *testing.T) {
	t.Parallel()
	t.Run("control", func(t *testing.T) {
		t.Parallel()
		dir := lightPassingBundle(t)
		code, rep, errOut := devCheck(t, dir)
		if code != 0 || rep.Scope != devLightScope || !slices.Contains(criteriaIDs(rep), "light_intake") || slices.Contains(criteriaIDs(rep), "design_receipt") {
			t.Fatalf("control %d %s %v %s", code, rep.Scope, devCodes(rep), errOut)
		}
		a := bundleDoc[DevAudit](t, dir, "audit.json")
		for _, id := range []string{"intake/authorization", "task/01/triage-brief", "validation/seed-defect", "validation/regression-red", "validation/regression-green",
			"validation/coverage-comparison"} {
			row := rowNamed(a.Rows, id)
			if row == nil || row.Verdict != VerdictCorrect || len(row.Sources) == 0 {
				t.Errorf("audit row %s %+v", id, row)
			}
		}
		var judgments []string
		for _, r := range a.JudgmentRows {
			judgments = append(judgments, r.ID)
			if r.Verdict != VerdictUnobserved {
				t.Errorf("judgment %s is %s", r.ID, r.Verdict)
			}
		}
		for _, want := range []string{"judgment/task/01/triage", "judgment/task/01/brief", "judgment/task/02/regression", "judgment/task/03/review",
			"judgment/task/04/regression", "judgment/task/05/review"} {
			if !slices.Contains(judgments, want) {
				t.Errorf("no judgment row %s in %v", want, judgments)
			}
		}
	})
	for name, c := range map[string]struct {
		mutate func(t *testing.T, dir string)
		crit   string
	}{
		"original_mode": {func(t *testing.T, dir string) {
			editJSON(t, dir, taskDir(2)+"/plan.json", func(m map[string]any) { m["data"].(map[string]any)["original_mode"] = modeImplement })
		}, "transitions"},
		"plan_base": {func(t *testing.T, dir string) {
			editJSON(t, dir, taskDir(1)+"/plan.json", func(m map[string]any) { m["data"].(map[string]any)["expected_base"] = strings.Repeat("e", 40) })
		}, "transitions"},
		"brief_current": {func(t *testing.T, dir string) {
			editJSON(t, dir, taskDir(1)+"/admission.json", func(m map[string]any) { m["brief_current"] = false })
		}, "publications"},
		"candidate": {func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/light-regression.json", func(m map[string]any) { m["candidate"] = strings.Repeat("f", 40) })
		}, "regression_red_green"},
		"red_events": {func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/light-regression.json", func(m map[string]any) { m["red_events"].(map[string]any)["fail"] = []any{} })
		}, "regression_red_green"},
		"coverage_denominator": {func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/light-coverage.json", func(m map[string]any) { m["baseline"].(map[string]any)["total"] = 1 })
		}, "coverage"},
		"coverage_verdict": {func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/light-coverage.json", func(m map[string]any) {
				m["comparison"].(map[string]any)["pass"] = false
				m["comparison"].(map[string]any)["code"] = CodeCoverageDecreased
			})
		}, "coverage"},
		"seed_defect": {func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/light-baseline.json", func(m map[string]any) { m["defect"].(map[string]any)["stderr"] = "" })
		}, "baseline"},
		"scenario_record": {func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/light-scenario.json", func(m map[string]any) { m["source_sha256"] = strings.Repeat("0", 64) })
		}, "publications"},
		"ledger_counter": {func(t *testing.T, dir string) {
			editJSON(t, dir, devLedgerFile, func(m map[string]any) { m["data"].(map[string]any)["code_cycles"] = 2 })
		}, "transitions"},
		"intake_event": {func(t *testing.T, dir string) {
			editJSON(t, dir, lightDecisionFile, func(m map[string]any) { m["data"].(map[string]any)["decision_seq"] = 1 })
		}, "light_intake"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := lightPassingBundle(t)
			c.mutate(t, dir)
			code, rep := lightReforge(t, dir)
			if code != 1 || !slices.Contains(failed(rep), c.crit) {
				t.Fatalf("exit %d failed %v codes %v", code, failed(rep), devCodes(rep))
			}
		})
	}
	for name, c := range map[string]struct {
		mutate func(t *testing.T, dir string)
		code   int
	}{
		// Stale derived files and inventory are never repaired.
		"row_reference": {func(t *testing.T, dir string) {
			editJSON(t, dir, "audit.json", func(m map[string]any) {
				rows := m["rows"].([]any)
				rows[0].(map[string]any)["sources"] = []any{}
			})
		}, 1},
		"canonical_report": {func(t *testing.T, dir string) {
			editJSON(t, dir, "report.json", func(m map[string]any) { m["claim"] = "light passed" })
		}, 1},
		"inventory": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "validation", "light", "red-stderr.txt"), []byte("changed\n"), 0o600)
		}, 1},
		"unknown_light_file": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "validation", "coverage.json"), []byte("{}\n"), 0o600)
		}, 2},
		"duplicate_field": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(lightReceiptFile))
			b := mustRead(t, p)
			os.WriteFile(p, []byte(strings.Replace(string(b), `"run_id":`, `"run_id":"x","run_id":`, 1)), 0o600)
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := lightPassingBundle(t)
			c.mutate(t, dir)
			if code, rep, errOut := devCheck(t, dir); code != c.code {
				t.Fatalf("exit %d want %d %v %s", code, c.code, devCodes(rep), errOut)
			}
		})
	}
}

// criteriaIDs are a report's criterion IDs.
func criteriaIDs(rep DevReport) []string {
	var out []string
	for _, c := range rep.Criteria {
		out = append(out, c.ID)
	}
	return out
}

// rowNamed is the row id (nil: none).
func rowNamed(rows []DevAuditRow, id string) *DevAuditRow {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

func TestDevCapturePaths(t *testing.T) {
	t.Parallel()
	t.Run("mapping", func(t *testing.T) {
		t.Parallel()
		for logical, want := range map[string]string{
			"setup/probe-fixture/main.go":                            "_captured/setup/probe-fixture/main.go.bin",
			"setup/sources/docs/examples/dev-flow/triage-runbook.md": "_captured/setup/sources/docs/examples/dev-flow/triage-runbook.md.bin",
			"setup/light-fixture/go.mod":                             "_captured/setup/light-fixture/go.mod.bin",
		} {
			if got := capturedPath(logical); got != want || capturedPath(logical) != got {
				t.Errorf("%s -> %s", logical, got)
			}
		}
		if foldKey("_captured/A/B.md.bin") != "_captured/a/b.md.bin" || foldKey("_captured/É.bin") != "_captured/É.bin" {
			t.Fatal("the fold key is not ASCII-only")
		}
		if err := checkCaptureFold([]string{"_captured/x/Main.go.bin", "_captured/x/main.go.bin"}); err == nil {
			t.Fatal("a case-fold collision was accepted")
		}
		if err := checkCaptureFold([]string{"_captured/x/main.go.bin", "_captured/x/main_test.go.bin"}); err != nil {
			t.Fatal(err)
		}
		// Every v2 allowlisted capture is unique under folding.
		var all []string
		for _, l := range append(devCaptureLogical(scenarioLight), probeCaptureLogical(probeScopeBoth)...) {
			all = append(all, capturedPath(l))
		}
		slices.Sort(all)
		if err := checkCaptureFold(slices.Compact(all)); err != nil {
			t.Fatal(err)
		}
	})
	playbook := capturedPath("setup/sources/" + DevFlowDir + "/" + DevPlaybook)
	for name, c := range map[string]struct {
		bundle func(t *testing.T) string
		mutate func(t *testing.T, dir string)
		code   int
	}{
		"case_variant": {devPassingBundle, func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(playbook))
			os.Rename(p, filepath.Join(filepath.Dir(p), strings.ToUpper(filepath.Base(p)[:1])+filepath.Base(p)[1:]))
		}, 2},
		"traversal_name": {devPassingBundle, func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, capturedDir, "..bin"), []byte("x"), 0o600)
		}, 2},
		"symlink": {devPassingBundle, func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(playbook))
			os.Rename(p, p+".real")
			os.Symlink(p+".real", p)
		}, 2},
		"wrong_suffix": {devPassingBundle, func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(playbook))
			os.Rename(p, strings.TrimSuffix(p, ".bin")+".txt")
		}, 2},
		"raw_capture": {lightPassingBundle, func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "setup", "light-fixture"), 0o700)
			os.WriteFile(filepath.Join(dir, "setup", "light-fixture", "main.go"), []byte("package main\n"), 0o600)
		}, 2},
		"oversized_capture": {devPassingBundle, func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, filepath.FromSlash(playbook)), make([]byte, maxTextFile+1), 0o600)
		}, 2},
		"go_file": {lightPassingBundle, func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "setup", "x.go"), []byte("package x\n"), 0o600)
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := c.bundle(t)
			c.mutate(t, dir)
			if code, rep, errOut := devCheck(t, dir); code != c.code {
				t.Fatalf("exit %d want %d %v %s", code, c.code, devCodes(rep), errOut)
			}
		})
	}
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"forged_storage_path": func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/sources.json", func(m map[string]any) {
				m["data"].([]any)[0].(map[string]any)["storage_path"] = "_captured/elsewhere.bin"
			})
		},
		"traversal_storage_path": func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/sources.json", func(m map[string]any) {
				m["data"].([]any)[0].(map[string]any)["storage_path"] = "../outside.bin"
			})
		},
		"duplicate_logical": func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/sources.json", func(m map[string]any) {
				d := m["data"].([]any)
				m["data"] = append(d, d[0])
			})
		},
		"altered_bytes": func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, filepath.FromSlash(playbook)), []byte("changed\n"), 0o600)
		},
		"light_fixture_bytes": func(t *testing.T, dir string) {
			if exists(dir, capturedPath(lightFixtureCaptureDir+"/main.go")) {
				os.WriteFile(filepath.Join(dir, filepath.FromSlash(capturedPath(lightFixtureCaptureDir+"/main.go"))), []byte("package main\n"), 0o600)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bundle := devPassingBundle
			if name == "light_fixture_bytes" {
				bundle = lightPassingBundle
			}
			dir := bundle(t)
			mutate(t, dir)
			code, rep := lightReforge(t, dir)
			if code != 1 || !slices.Contains(failed(rep), "provenance") && !slices.Contains(failed(rep), "manifest") && !slices.Contains(failed(rep), "publications") {
				t.Fatalf("exit %d failed %v %v", code, failed(rep), devCodes(rep))
			}
		})
	}
	t.Run("bin_counts_against_budget", func(t *testing.T) {
		t.Parallel()
		// Inert captures share the text budget: filling the remaining
		// non-environment budget with capture bytes is refused.
		dir := devPassingBundle(t)
		b, err := LoadDevBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		var other int64
		for rel, data := range b.Files {
			if !isEnvironmentFile(rel) {
				other += int64(len(data))
			}
		}
		room := maxOtherText - other
		var caps []string
		for rel := range b.Files {
			if strings.HasPrefix(rel, capturedDir+"/") {
				caps = append(caps, rel)
			}
		}
		slices.Sort(caps)
		for _, rel := range caps {
			if room < 0 {
				break
			}
			n := int64(maxTextFile)
			os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), make([]byte, n), 0o600)
			room -= n - int64(len(b.Files[rel]))
		}
		if room >= 0 {
			t.Fatalf("too few captures to exceed the budget (%d left)", room)
		}
		if code, _, errOut := devCheck(t, dir); code != 2 || !strings.Contains(errOut, "budget") && !strings.Contains(errOut, "exceeds") {
			t.Fatalf("exit %d %s", code, errOut)
		}
	})
}

func TestDevExerciseAccounting(t *testing.T) {
	t.Parallel()
	t.Run("happy", func(t *testing.T) {
		t.Parallel()
		root := exerciseBundle(t)
		var out, errb strings.Builder
		if code := CheckExercise(root, &out, &errb); code != 0 {
			t.Fatalf("exit %d %s", code, errb.String())
		}
		var rep DevExerciseReport
		json.Unmarshal([]byte(out.String()), &rep)
		ex := bundleDoc[DevExercise](t, root, devExerciseFile)
		acc := rep.Accounting
		if acc.WorkerCalls != exerciseHappyCalls || acc.MaxWorkerCalls != 33 || acc.PreflightCalls != 10 || acc.WorkflowCalls != 7 || acc.ProviderUsage != usageUnknown ||
			rep.Children[0].WorkerCalls+rep.Children[1].WorkerCalls != acc.WorkerCalls {
			t.Fatalf("accounting %+v", acc)
		}
		// Elapsed and windows derive from the recorded fake-clock times.
		for i, c := range ex.Children {
			start, _ := parseTime(c.Start)
			end, _ := parseTime(c.End)
			deadline, _ := parseTime(c.Deadline)
			fin, _ := parseTime(c.FinalizeBy)
			if !deadline.Equal(start.Add(AttemptBound)) || fin.After(deadline.Add(CleanupBound)) || !end.Before(fin) ||
				*rep.Children[i].ElapsedMS != end.Sub(start).Milliseconds() {
				t.Fatalf("child %s window %+v", c.Scenario, c)
			}
		}
		fe, _ := parseTime(ex.Children[0].End)
		ls, _ := parseTime(ex.Children[1].Start)
		dec, _ := parseTime(ex.Authorization.Decision)
		end, _ := parseTime(ex.End)
		if ls.Before(fe) || end.Sub(dec) > ExerciseBound || ExerciseBound != 184*time.Minute || 2*(AttemptBound+CleanupBound) != ExerciseBound {
			t.Fatalf("order or the 184m arithmetic: %s %s %s", fe, ls, end.Sub(dec))
		}
		start, _ := parseTime(ex.Start)
		if *acc.TotalElapsedMS != end.Sub(start).Milliseconds() || *acc.AuthorizationWaitMS < 0 {
			t.Fatalf("elapsed %+v", acc)
		}
	})
	for name, c := range map[string]struct {
		full, light []devAct
		owner       []string
		auth        string
		fault       string
		code        int
		status      [2]string
		lightStop   string
	}{
		"full_failure_light_pass": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete, alter: "bad-final"}}, lightHappy(),
			[]string{"yes 1"}, authExercise, "", 1, [2]string{childFail, childPass}, ""},
		"owner_refusal":       {happy(), lightHappy(), []string{"no 1"}, authExercise, "", 1, [2]string{childFail, childNotRun}, "full_owner_stop"},
		"unsafe_cleanup":      {happy(), lightHappy(), []string{"yes 1"}, authExercise, "linger", 1, [2]string{childFail, childNotRun}, "full_cleanup_incomplete"},
		"no_authorization":    {happy(), lightHappy(), nil, intakeNo, "", 1, [2]string{childNotRun, childNotRun}, "owner_no"},
		"eof_authorization":   {happy(), lightHappy(), nil, intakeEOF, "", 1, [2]string{childNotRun, childNotRun}, "owner_eof"},
		"finalization_expiry": {happy(), lightHappy(), []string{"yes 1"}, authExercise, "expire", 1, [2]string{childFail, childNotRun}, stopFinalized},
		"light_failure":       {happy(), []devAct{{marker: markerTriageUnconf}}, []string{"yes 1"}, authExercise, "", 1, [2]string{childPass, childFail}, "cause_unconfirmed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			x := newExerciseRig(t, c.full, c.light)
			x.full.owner = c.owner
			x.auth = c.auth
			switch c.fault {
			case "linger":
				x.full.fault = "linger"
			case "expire":
				// Cleanup, collection and the child check share the 2m tail:
				// time spent in the final scan uses all of it.
				x.full.onFinal = func(r *devRig) { r.f.clock.advance(CleanupBound) }
			}
			code := x.run()
			ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
			if code != c.code || ex.Children[0].Status != c.status[0] || ex.Children[1].Status != c.status[1] || ex.Children[1].Stop != c.lightStop {
				t.Fatalf("exit %d children %+v", code, ex.Children)
			}
			if c.fault == "expire" && ex.Children[0].Stop != stopFinalized {
				t.Fatalf("full child stop %s", ex.Children[0].Stop)
			}
			var out, errb strings.Builder
			if got := CheckExercise(x.root(), &out, &errb); got != c.code {
				t.Fatalf("offline %d %s %s", got, errb.String(), out.String())
			}
			var rep DevExerciseReport
			json.Unmarshal([]byte(out.String()), &rep)
			if c.auth != authExercise && (rep.Accounting.WorkerCalls != 0 || len(x.full.f.plane.ids()) != 0) {
				t.Fatalf("a refused exercise made %d calls", rep.Accounting.WorkerCalls)
			}
			// Individual mechanical results stay visible when the aggregate
			// fails.
			if c.status[1] == childPass && rep.Children[1].Mechanical != ResultPass {
				t.Fatalf("the light child's PASS is hidden: %+v", rep.Children)
			}
		})
	}
	t.Run("authorization_window", func(t *testing.T) {
		t.Parallel()
		// A wrong ID is answered and ignored; expiry starts no attempt.
		x := newExerciseRig(t, happy(), lightHappy())
		x.auth = "wrong"
		if code := x.run(); code != 0 || !strings.Contains(x.stdout(), "not an authorization for exercise") {
			t.Fatalf("exit %d", code)
		}
		y := newExerciseRig(t, happy(), lightHappy())
		y.auth = ""
		if code := y.run(); code != 1 || len(y.full.f.plane.ids()) != 0 {
			t.Fatalf("expiry: exit %d, %d calls", code, len(y.full.f.plane.ids()))
		}
		ex := bundleDoc[DevExercise](t, y.root(), devExerciseFile)
		if ex.Authorization.Kind != intakeExpired || ex.Children[0].Stop != "owner_"+intakeExpired {
			t.Fatalf("expiry %+v", ex)
		}
	})
	t.Run("startup_failures", func(t *testing.T) {
		t.Parallel()
		// A world that cannot be built, or an existing evidence root, starts
		// nothing.
		x := newExerciseRig(t, happy(), lightHappy())
		h := x.full.host()
		h.Args[0] = "run-dev-exercise"
		h.NewWorld = func(Host) (*World, func(), error) { return nil, nil, errors.New("no world") }
		if code := MainWith(h); code != 1 {
			t.Fatalf("world error %d", code)
		}
		y := newExerciseRig(t, happy(), lightHappy())
		if err := os.MkdirAll(y.root(), 0o700); err != nil {
			t.Fatal(err)
		}
		if code := y.run(); code != 2 || exists(y.root(), devExerciseFile) {
			t.Fatalf("an existing root: %d", code)
		}
	})
	t.Run("continue_causes", func(t *testing.T) {
		t.Parallel()
		ok := func() *devSupervisor {
			d := &devSupervisor{s: &supervisor{ev: &evidenceWriter{}}, cleanupOK: true}
			return d
		}
		for want, d := range map[string]*devSupervisor{
			"":                           ok(),
			stopFinalized:                func() *devSupervisor { d := ok(); d.finalizeExpired = true; return d }(),
			"full_owner_stop":            func() *devSupervisor { d := ok(); d.stopCode = "owner_no"; return d }(),
			"interrupted":                func() *devSupervisor { d := ok(); d.stopCode = "interrupted"; return d }(),
			"full_cleanup_incomplete":    func() *devSupervisor { d := ok(); d.cleanupOK = false; return d }(),
			"full_protected_change":      func() *devSupervisor { d := ok(); d.protected = true; return d }(),
			"full_evidence_write_failed": func() *devSupervisor { d := ok(); d.s.ev = nil; return d }(),
		} {
			if got := continueCause(d); got != want {
				t.Errorf("cause %q, want %q", got, want)
			}
		}
		// A workflow failure continues.
		d := ok()
		d.stopCode = "publication_rejected"
		if continueCause(d) != "" {
			t.Fatal("a workflow failure stopped the exercise")
		}
	})
	t.Run("finish_root_failures", func(t *testing.T) {
		t.Parallel()
		root := exerciseBundle(t)
		ex := bundleDoc[DevExercise](t, root, devExerciseFile)
		var stderr strings.Builder
		e := &devExerciseRun{h: Host{Stdout: io.Discard, Stderr: &stderr}, w: &World{Clock: newAutoClock()}, root: root, ex: ex}
		// The root record exists already: never overwritten.
		if code := e.finishRoot(nil); code != 1 || !strings.Contains(stderr.String(), "was not written") {
			t.Fatalf("existing record %d %s", code, stderr.String())
		}
		// A derived file exists already: the root fails closed.
		os.Remove(filepath.Join(root, devExerciseFile))
		stderr.Reset()
		if code := e.finishRoot(nil); code != 1 || !strings.Contains(stderr.String(), "root report or audit was not written") {
			t.Fatalf("existing report %d %s", code, stderr.String())
		}
		// A malformed child cannot be aggregated.
		for _, n := range []string{devExerciseFile, "report.json", "report.txt", "audit.json", "audit.md"} {
			os.Remove(filepath.Join(root, n))
		}
		os.WriteFile(filepath.Join(root, filepath.FromSlash(exerciseLightDir), devManifestFile), []byte("{"), 0o600)
		stderr.Reset()
		if code := e.finishRoot(nil); code != 1 || stderr.Len() == 0 {
			t.Fatalf("malformed child %d", code)
		}
	})
	t.Run("root_aggregation_expiry", func(t *testing.T) {
		t.Parallel()
		// The root's aggregation shares the light child's 2m tail: time
		// spent aggregating past it is recorded, and the stored root, its
		// live exit and its offline check all FAIL with
		// finalization_deadline. Within the tail the root passes.
		for name, c := range map[string]struct {
			stage   string
			advance time.Duration
			code    int
		}{"expired_checking": {"check", CleanupBound, 1}, "expired_writing": {"write", CleanupBound, 1}, "within": {"check", time.Second, 0}} {
			x := newExerciseRig(t, happy(), lightHappy())
			once := false
			x.aggregating = func(stage string) {
				if !once && stage == c.stage {
					once = true
					x.full.f.clock.advance(c.advance)
				}
			}
			code := x.run()
			var out, errb strings.Builder
			offline := CheckExercise(x.root(), &out, &errb)
			stored := bundleDoc[DevExerciseReport](t, x.root(), "report.json")
			ex := bundleDoc[DevExercise](t, x.root(), devExerciseFile)
			end, _ := parseTime(ex.End)
			fin, _ := parseTime(ex.Children[1].FinalizeBy)
			if code != c.code || offline != c.code || (stored.Result == ResultPass) != (c.code == 0) || (c.code == 1) != !end.Before(fin) ||
				c.code == 1 && !slices.Contains(stored.Codes, stopFinalized) {
				t.Fatalf("%s: live %d offline %d stored %s %v end %s finalize_by %s", name, code, offline, stored.Result, stored.Codes, ex.End,
					ex.Children[1].FinalizeBy)
			}
		}
	})
	t.Run("late_admission_counted", func(t *testing.T) {
		t.Parallel()
		x := newExerciseRig(t, happy(), lightHappy())
		x.full.onFinal = func(r *devRig) {
			r.mustDispatch(contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "triage"}, Goal: "late", Acceptance: "x",
				Payload: []string{}, RequestedBy: requester})
		}
		if code := x.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		var out, errb strings.Builder
		CheckExercise(x.root(), &out, &errb)
		var rep DevExerciseReport
		json.Unmarshal([]byte(out.String()), &rep)
		if rep.Accounting.RejectedCalls != 1 || rep.Accounting.WorkerCalls != exerciseHappyCalls+1 {
			t.Fatalf("the late call is not accounted: %+v", rep.Accounting)
		}
	})
	for name, c := range map[string]struct {
		mutate func(t *testing.T, root string)
		code   int
		want   string
	}{
		"missing_child": {func(t *testing.T, root string) {
			os.RemoveAll(filepath.Join(root, filepath.FromSlash(exerciseLightDir)))
		}, 2, ""},
		"not_run_claimed": {func(t *testing.T, root string) {
			os.RemoveAll(filepath.Join(root, filepath.FromSlash(exerciseLightDir)))
			editJSON(t, root, devExerciseFile, func(m map[string]any) {
				c := m["children"].([]any)[1].(map[string]any)
				c["status"], c["run_id"], c["manifest_sha256"], c["stop"] = childNotRun, "", "", "skipped"
			})
		}, 1, ""},
		"spliced_child": {func(t *testing.T, root string) {
			r := newLightRig(t, lightHappy())
			r.expect(0, "")
			os.RemoveAll(filepath.Join(root, filepath.FromSlash(exerciseLightDir)))
			if err := copyTree(r.f.bundle(), filepath.Join(root, filepath.FromSlash(exerciseLightDir))); err != nil {
				t.Fatal(err)
			}
		}, 1, "child_identity_mismatch"},
		"changed_selection": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) { m["flow_sha256"] = strings.Repeat("0", 64) })
		}, 1, "harness_mismatch"},
		"forged_authorization": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) { m["authorization"].(map[string]any)["response"] = "exercise all" })
		}, 1, "authorization_invalid"},
		"order": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) {
				full := m["children"].([]any)[0].(map[string]any)
				light := m["children"].([]any)[1].(map[string]any)
				full["end"] = light["end"]
			})
		}, 1, "exercise_order_invalid"},
		"root_finalization": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) {
				light := m["children"].([]any)[1].(map[string]any)
				m["end"] = light["finalize_by"]
			})
		}, 1, stopFinalized},
		"third_child": {func(t *testing.T, root string) {
			editJSON(t, root, devExerciseFile, func(m map[string]any) {
				cs := m["children"].([]any)
				m["children"] = append(cs, cs[1])
			})
		}, 2, ""},
		"stale_root_report": {func(t *testing.T, root string) {
			editJSON(t, root, "report.json", func(m map[string]any) { m["claim"] = "x" })
		}, 1, CodeDerivedMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := exerciseBundle(t)
			c.mutate(t, root)
			if c.code == 1 && name != "stale_root_report" {
				rewriteExerciseDerived(t, root)
			}
			var out, errb strings.Builder
			code := CheckExercise(root, &out, &errb)
			if code != c.code || c.want != "" && !strings.Contains(out.String(), c.want) {
				t.Fatalf("exit %d want %d %q: %s %s", code, c.code, c.want, errb.String(), out.String())
			}
		})
	}
}

// rewriteExerciseDerived recomputes a mutated root's derived files (a
// capable forger).
func rewriteExerciseDerived(t testing.TB, root string) {
	t.Helper()
	for _, n := range []string{"report.json", "report.txt", "audit.json", "audit.md"} {
		os.Remove(filepath.Join(root, n))
	}
	res, err := checkExerciseRoot(root, true)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"report.json": res.reportJSON, "report.txt": []byte(res.reportText), "audit.json": res.auditJSON,
		"audit.md": []byte(res.auditText)} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDevLightQualification(t *testing.T) {
	t.Parallel()
	t.Run("both_and_light", func(t *testing.T) {
		t.Parallel()
		for _, scope := range []string{probeScopeLight, probeScopeBoth} {
			r := probeLightRig(t, scope)
			r.expect(0, "")
			q := r.qualification()
			if q.Scope != scope || !slices.Equal(q.StageCoverage, probeLightStages) || !qualifiedStages(q.Stages, probeLightStages) || q.WorkerCalls != 11 ||
				q.Phases != 3 || q.Light == nil || len(q.Light.Pushes) != 3 || q.Light.Candidate == "" {
				t.Fatalf("%s: %+v", scope, q)
			}
			if !strings.Contains(r.f.stdout.String(), ProbeLightBounds) {
				t.Fatalf("%s: the scoped disclosure was not printed", scope)
			}
			// The probe's tasks are their own namespaces.
			dir := r.f.bundle()
			for _, label := range []string{probeTriageNamespace + "-01", probeLightReviewNamespace + "-01"} {
				if len(eventSeqs(t, dir, EvTaskAdmitted, label, "")) == 0 {
					t.Errorf("%s: no admission of %s", scope, label)
				}
			}
			if exists(dir, "tasks") || exists(dir, devManifestFile) || exists(dir, lightReceiptFile) {
				t.Fatalf("%s: the probe wrote attempt evidence", scope)
			}
		}
	})
	t.Run("full_default", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.expect(0, "")
		q := r.qualification()
		if q.Scope != probeScopeFull || !slices.Equal(q.StageCoverage, probeStages) || q.Light != nil || q.WorkerCalls != 9 {
			t.Fatalf("%+v", q)
		}
	})
	for name, c := range map[string]struct {
		steps  []devAct
		stop   string
		stages map[string]string
	}{
		"triage_unconfirmed": {[]devAct{{}, {}, {}, {marker: markerReviewApproved}, {marker: markerTriageUnconf}}, "probe_triage_unqualified",
			map[string]string{"reviewer_evidence": stagePass, "triage_evidence": stageFail, "light_reviewer_evidence": stageUnreached}},
		"triage_design_yes": {[]devAct{{}, {}, {}, {marker: markerReviewApproved}, {marker: markerTriageConf, designRequired: designRequiredYes}}, "probe_triage_unqualified",
			map[string]string{"triage_evidence": stageFail, "light_reviewer_evidence": stageUnreached}},
		"triage_no_root": {[]devAct{{}, {}, {}, {marker: markerReviewApproved}, {marker: markerTriageConf, designRequired: designRequiredNo, alter: "no-root"}},
			"publication_rejected", map[string]string{"reviewer_evidence": stagePass, "triage_evidence": stageFail, "light_reviewer_evidence": stageUnreached}},
		"light_review_changes": {[]devAct{{}, {}, {}, {marker: markerReviewApproved}, {marker: markerTriageConf, designRequired: designRequiredNo},
			{marker: markerReviewChanges}}, "probe_review_not_approved", map[string]string{"triage_evidence": stagePass, "light_reviewer_evidence": stageFail}},
		"light_review_quoted": {[]devAct{{}, {}, {}, {marker: markerReviewApproved}, {marker: markerTriageConf, designRequired: designRequiredNo},
			{marker: markerReviewApproved, alter: "prose-only"}}, CodeReviewUnobserved, map[string]string{"triage_evidence": stagePass, "light_reviewer_evidence": stageFail}},
		"full_review_fails_first": {[]devAct{{}, {}, {}, {marker: markerReviewApproved, alter: "missing-field"}}, CodeReviewMismatch,
			map[string]string{"reviewer_evidence": stageFail, "triage_evidence": stageUnreached, "light_reviewer_evidence": stageUnreached}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := probeLightRig(t, probeScopeBoth)
			r.steps = c.steps
			r.expect(1, c.stop)
			q := r.qualification()
			got := map[string]string{}
			for _, s := range q.Stages {
				got[s.Name] = s.Result
			}
			for stage, want := range c.stages {
				if got[stage] != want {
					t.Errorf("stage %s %s, want %s (%+v)", stage, got[stage], want, q.Stages)
				}
			}
			if q.Result != ResultBlocked || len(q.Stages) != len(probeLightStages) {
				t.Fatalf("%+v", q)
			}
		})
	}
	t.Run("v1_scope", func(t *testing.T) {
		t.Parallel()
		// A historical v1 record keeps its full-only scope: it checks as
		// such, and a v1 record claiming the light scope is refused.
		dir := devProbeBundle(t)
		downgradeQualificationToV1(t, dir)
		b, err := LoadDevQualification(dir)
		if err != nil {
			t.Fatal(err)
		}
		if codes := CheckDevQualification(b); len(codes) != 0 || b.ver != devBundleV1 {
			t.Fatalf("v1 %v", codes)
		}
		editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["scope"] = probeScopeBoth })
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, "scope_invalid") {
			t.Fatalf("a v1 light claim: %v", codes)
		}
	})
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"call_bound": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["worker_calls"] = 12 })
		},
		"scope_coverage": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["stage_coverage"] = []any{"models"} })
		},
		"candidate_parent": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["light"].(map[string]any)["triage_commit"] = strings.Repeat("a", 40) })
		},
		"push_path": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) {
				m["light"].(map[string]any)["pushes"].([]any)[2].(map[string]any)["path"] = "/elsewhere"
			})
		},
		"push_order": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) {
				p := m["light"].(map[string]any)["pushes"].([]any)
				p[2].(map[string]any)["verified_seq"] = p[1].(map[string]any)["verified_seq"]
			})
		},
		"light_workspace": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) {
				m["light"].(map[string]any)["workspace"].(map[string]any)["instance"] = m["workspace"].(map[string]any)["instance"]
			})
		},
		"fixture": func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, filepath.FromSlash(capturedPath(lightFixtureCaptureDir+"/main.go"))), []byte("package main\n"), 0o600)
		},
	} {
		t.Run("forged_"+name, func(t *testing.T) {
			t.Parallel()
			dir := probeLightBundle(t)
			mutate(t, dir)
			requalify(t, dir)
			if codes := qualificationCodes(t, dir); len(codes) == 0 {
				t.Fatal("the forged record still qualifies")
			}
		})
	}
	t.Run("namespace_borrowing", func(t *testing.T) {
		t.Parallel()
		// A light attempt's task answer substituted into the probe resolves
		// in no probe record.
		light := lightPassingBundle(t)
		c := firstCall(t, light, toolTaskShow, nil)
		dir := probeLightBundle(t)
		lines := phaseStream(t, dir, 3)
		insertAt(t, dir, 3, -1, useLine(sessionOf(lines), "toolu_borrow", toolTaskShow, c.args), resultLine(sessionOf(lines), "toolu_borrow", c.answer, false))
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, devMCPCallInvalid) {
			t.Fatalf("borrowed: %v", codes)
		}
	})
	t.Run("cross_workspace", func(t *testing.T) {
		t.Parallel()
		// The light workspace answered with the full workspace's instance.
		dir := probeLightBundle(t)
		q := bundleDoc[DevQualification](t, dir, devQualificationFile)
		lines := phaseStream(t, dir, 3)
		ans := mustMarshalAny(contract.WorkspaceStatusResponse{Name: q.Light.Workspace.Name, Instance: q.Workspace.Instance, Generation: hexID("", 7),
			DefaultBranch: contract.DefaultBranchRef, Refs: []contract.WorkspaceRef{{Name: contract.DefaultBranchRef, Commit: q.Light.Candidate}}})
		insertAt(t, dir, 3, -1, useLine(sessionOf(lines), "toolu_xws", toolWsStatus, `{"name":"`+q.Light.Workspace.Name+`"}`),
			resultLine(sessionOf(lines), "toolu_xws", ans, false))
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, devMCPCallInvalid) {
			t.Fatalf("cross-workspace: %v", codes)
		}
		// The same call with the light instance is a valid inspection.
		dir = probeLightBundle(t)
		ans = mustMarshalAny(contract.WorkspaceStatusResponse{Name: q.Light.Workspace.Name, Instance: q.Light.Workspace.Instance, Generation: hexID("", 7),
			DefaultBranch: contract.DefaultBranchRef, Refs: []contract.WorkspaceRef{{Name: contract.DefaultBranchRef, Commit: q.Light.Candidate}}})
		insertAt(t, dir, 3, -1, useLine(sessionOf(lines), "toolu_xws", toolWsStatus, `{"name":"`+q.Light.Workspace.Name+`"}`),
			resultLine(sessionOf(lines), "toolu_xws", ans, false))
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); len(codes) != 0 {
			t.Fatalf("a valid light inspection: %v", codes)
		}
	})
	for name, c := range map[string]struct {
		fault string
		stop  string
	}{
		// A repeated candidate push is a retry: never part of a qualified
		// record. A missing light seed push never exposes a task.
		"candidate_twice":    {"candidate-twice", ""},
		"no_light_seed_push": {"no-light-seed-push", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := probeLightRig(t, probeScopeBoth)
			r.fault = c.fault
			code := r.run()
			if code != 1 {
				t.Fatalf("exit %d stop %s", code, r.d.stopCode)
			}
			// The written record never passes its own check.
			if codes := qualificationCodes(t, r.f.bundle()); len(codes) == 0 || !strings.Contains(r.f.stdout.String(), "qualification probe: "+ResultBlocked) {
				t.Fatalf("qualified: %v", codes)
			}
		})
	}
	t.Run("binding", func(t *testing.T) {
		t.Parallel()
		// No model fallback and no workspace substitution: the triage task
		// must run the frozen pair in the light workspace.
		r := probeLightRig(t, probeScopeBoth)
		r.expect(0, "")
		p := r.d.probe
		dir := r.f.bundle()
		shown, err := contract.ParseTaskShowResponse(mustRead(t, filepath.Join(dir, filepath.FromSlash(probeTriageDir+"/task.json"))))
		if err != nil {
			t.Fatal(err)
		}
		v := shown
		plan := bundleDoc[devEnvelope[TaskPlan]](t, dir, probeTriageDir+"/plan.json").Data
		if err := p.binding(v, plan); err != nil {
			t.Fatalf("control: %v", err)
		}
		other := v
		other.Effective.Model = "fallback-model"
		if p.binding(other, plan) == nil {
			t.Fatal("a fallback model was accepted")
		}
		wb := *v.WorkspaceBinding
		wb.Name, wb.Instance = r.d.s.workspace.Name, r.d.s.workspace.Instance
		other = v
		other.WorkspaceBinding = &wb
		if p.binding(other, plan) == nil {
			t.Fatal("the full workspace substituted for the light one")
		}
	})
	t.Run("gates_before_world", func(t *testing.T) {
		t.Parallel()
		for _, args := range [][]string{{"run-dev-light"}, {"run-dev-exercise"}, {"probe-dev-full", "--scenario", "light"}, {"probe-dev-full", "--scenario", "both"},
			{"probe-dev-full", "--scenario=both"}} {
			var out, errb strings.Builder
			world := false
			h := Host{GOOS: "linux", Args: append(args, "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"),
				Getenv: func(k string) string {
					if k == EnvOptIn {
						return "1"
					}
					return ""
				}, LookupEnv: func(k string) (string, bool) { return "true", k == EnvCI }, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
				NewWorld: func(Host) (*World, func(), error) { world = true; return nil, nil, errors.New("no world") }}
			if code := MainWith(h); code != 2 || world {
				t.Errorf("%v: exit %d, world %v", args, code, world)
			}
			want := map[string]string{"run-dev-light": LightBounds, "run-dev-exercise": ExerciseBounds, "probe-dev-full": ProbeLightBounds}[args[0]]
			if !strings.Contains(errb.String(), want) {
				t.Errorf("%v: the selected disclosure was not printed: %s", args, errb.String())
			}
		}
		var errb strings.Builder
		h := Host{GOOS: "linux", Args: []string{"probe-dev-full", "--scenario", "nine", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
			Getenv: func(k string) string {
				if k == EnvOptIn {
					return "1"
				}
				return ""
			}, LookupEnv: func(string) (string, bool) { return "", false }, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &errb,
			NewWorld: func(Host) (*World, func(), error) { t.Fatal("world"); return nil, nil, nil }}
		if code := MainWith(h); code != 2 || !strings.Contains(errb.String(), "--scenario of full, light or both") {
			t.Fatalf("an unknown scope: %d %s", code, errb.String())
		}
	})
}

// downgradeQualificationToV1 rewrites a current full-scope qualification
// into the historical v1 form (a synthetic fixture): captures unpacked at
// their logical paths, v1 schema identifiers, no scope fields or storage
// paths.
func downgradeQualificationToV1(t *testing.T, dir string) {
	t.Helper()
	downgradeCaptures(t, dir)
	editJSON(t, dir, devQualificationFile, func(m map[string]any) {
		delete(m, "scope")
		delete(m, "stage_coverage")
		delete(m, "light")
		for _, f := range m["fixture"].([]any) {
			delete(f.(map[string]any), "storage_path")
		}
	})
	editJSON(t, dir, "setup/sources.json", func(m map[string]any) {
		for _, s := range m["data"].([]any) {
			delete(s.(map[string]any), "storage_path")
		}
	})
	requalify(t, dir)
}
