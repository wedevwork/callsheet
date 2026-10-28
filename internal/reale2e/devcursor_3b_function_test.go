package reale2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Function tests of design 12a-3-cursor-worker, slice 3b (3b.md
// traceability table, local FP-1..FP-3; FP-4..FP-6 are tests/function).
// They drive MainWith through the offline rigs with injected clock,
// vendors, plane, push and filesystem; no paid call, real vendor or
// sleep. None is a live qualification.

func TestCursor3bFP1Qualification(t *testing.T) {
	t.Parallel()
	// A fresh Cursor both probe writes the new qualification record, and
	// its independent check, live and offline alike, qualifies it.
	cr := newCursorRig(t, probeScopeBoth)
	cr.expect(0, "")
	q := cr.cursorQual()
	if q.Result != ResultQualified || q.Scope != probeScopeBoth || !qualifiedStages(q.Stages, cursorProbeStagesFor(probeScopeBoth)) || len(q.Stages) != 7 ||
		q.WorkerCalls != 12 || q.Phases != 3 || q.StopCode != nil {
		t.Fatalf("record %+v", q)
	}
	bind := q.Cursor.Binding
	if bind.Path != cr.d.s.tok(cr.fx.resolved(cr.exe)) || bind.BeforeSHA256 != sha256Hex(mustRead(t, cr.exe)) || bind.AfterSHA256 != bind.BeforeSHA256 ||
		q.Cursor.Task != cr.coderTasks[0] || q.Cursor.InputCommit != q.Cursor.Workspace.SeedCommit || q.Cursor.ResultCommit == "" {
		t.Fatalf("cursor object %+v", q.Cursor)
	}
	lines := strings.Split(strings.TrimSpace(cr.f.stdout.String()), "\n")
	if lines[len(lines)-1] != "qualification probe: "+ResultQualified || !cr.d.passed {
		t.Fatalf("stdout tail %q", lines[len(lines)-1])
	}
	if _, err := os.Stat(filepath.Join(cr.f.evidence, cursorObservationFile)); err == nil {
		t.Fatal("a fresh probe wrote a diagnostic receipt")
	}
	code, rep, errOut := cr.qualifiedCheck()
	if code != 0 || rep.Result != ResultQualified || rep.RunID != q.RunID || rep.Scope != probeScopeBoth || len(rep.Codes) != 0 || rep.Activity.After == "" {
		t.Fatalf("offline check %d %+v %s", code, rep, errOut)
	}
	// Forged publication, selection and validation, and a historical
	// diagnostic receipt beside the record, never qualify.
	for name, c := range map[string]struct {
		edit func(t *testing.T, dir string)
		exit int
		code string
	}{
		"publication": {func(t *testing.T, dir string) {
			editJSON(t, dir, probeCoderDir+"/publication.json", func(m map[string]any) { jobj(m, "data")["changed"] = []any{"main.go", "design.md"} })
		}, 1, CodeCursorPublicationMismatch},
		"selection": {func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/flow.json", func(m map[string]any) { jarr(m, "roles")[2].(map[string]any)["model"] = "grok-4.6" })
		}, 1, CodeCursorSelectionMismatch},
		"validation": {func(t *testing.T, dir string) {
			editJSON(t, dir, probeCoderDir+"/validation/requirements.json", func(m map[string]any) { m["test_run_passed"] = false })
		}, 1, CodeCursorValidationMismatch},
		"diagnostic": {func(t *testing.T, dir string) { historicalReceipt(t, dir) }, 2, CodeDiagnosticNotQualification},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "q")
			if err := copyTree(cr.f.bundle(), dir); err != nil {
				t.Fatal(err)
			}
			c.edit(t, dir)
			requalify(t, dir)
			var out, errb bytes.Buffer
			code := CheckEvidence(dir, &out, &errb)
			if code != c.exit || !strings.Contains(out.String()+errb.String(), c.code) || strings.Contains(out.String(), `"result": "QUALIFIED"`) {
				t.Fatalf("exit %d %s %s", code, out.String(), errb.String())
			}
		})
	}
}

func TestCursor3bFP2Attempts(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{scenarioFull, scenarioLight} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			// Fresh Cursor attempts pass their own check, reproduce their
			// canonical report and audit offline, run the coder on Cursor
			// with no override or extra call, and stay PASS with truncated
			// (report-only) samples.
			r := newCursorFullRig(t, happy())
			calls := 9
			if scenario == scenarioLight {
				r, calls = newCursorLightRig(t, lightHappy()), 8
			}
			prev := r.devHook
			r.devHook = func(d *devSupervisor) {
				prev(d)
				d.cursor.output = activitySkeleton + 1
			}
			r.expect(0, "")
			dir := r.f.bundle()
			code, rep, errOut := devCheck(t, dir)
			b, err := LoadDevBundle(dir)
			if err != nil || code != 0 || rep.Result != ResultPass || !b.cursor || rep.Schema != devCursorReportSchema || rep.Activity == nil ||
				rep.WorkerCalls != calls {
				t.Fatalf("check %d %v %s %v", code, devCodes(rep), errOut, err)
			}
			res := checkDevBundle(b, true)
			if !bytes.Equal(res.reportJSON, b.Files["report.json"]) || !bytes.Equal(res.auditJSON, b.Files["audit.json"]) {
				t.Fatal("the stored report or audit is not the canonical recomputation")
			}
			coder := 0
			for _, n := range b.ordinalsOf("tasks") {
				v := doc[contract.TaskShowResponse](b, taskDir(n)+"/task.json")
				if v != nil && v.Task.Role.ID == HopCoder {
					coder++
					if v.Task.Role.Adapter != adapter.CursorID || v.Task.Effective.Model != "grok-4.7" || v.Task.Effective.Effort != "high" ||
						v.Task.Request.Override != nil {
						t.Fatalf("coder task %+v %+v", v.Task.Role, v.Task.Effective)
					}
				}
			}
			var a ActivityAggregate
			if err := decodeStrict(r.evidence(cursorActivityFiles[0]), &a); coder == 0 || err != nil || !a.Roots[0].Flags.OutputLimited {
				t.Fatalf("coder tasks %d, sample %v", coder, err)
			}
		})
	}
	t.Run("protected_change_fails", func(t *testing.T) {
		t.Parallel()
		r := newCursorLightRig(t, lightHappy())
		done := false
		r.onTick = func(r *devRig) {
			if !done {
				done = true
				write(t, filepath.Join(r.f.home, ".claude", "settings.json"), `{"permissions":{"allow":["Bash(*)"]}}`+"\n")
			}
		}
		r.expect(1, "")
		if !slices.Contains(devCodes(r.report())["owner_environment"], CodeEnvModified) {
			t.Fatalf("codes %v", devCodes(r.report()))
		}
	})
	t.Run("default_formats_keep_scope", func(t *testing.T) {
		t.Parallel()
		for _, dir := range []string{devPassingBundle(t), lightPassingBundle(t)} {
			code, rep, errOut := devCheck(t, dir)
			if code != 0 || rep.Schema != devKind("report") || rep.Activity != nil || strings.Contains(string(mustRead(t, filepath.Join(dir, "report.json"))), "activity") {
				t.Fatalf("default %d %s %s", code, rep.Schema, errOut)
			}
		}
	})
}

func TestCursor3bFP3Exercise(t *testing.T) {
	t.Parallel()
	// The existing combined command with the Cursor selection: one frozen
	// selection and executable binding for both independent children, the
	// full design gate, the light intake and both reviewers reached, and
	// root and children agreeing.
	x, exe := newCursorExerciseRig(t, happy(), lightHappy())
	if code := x.run(); code != 0 {
		t.Fatalf("exit %d\n%s", code, x.stdout())
	}
	root := x.root()
	ex := bundleDoc[DevExercise](t, root, devExerciseFile)
	if ex.Schema != DevCursorExerciseSchema || ex.Cursor == nil || ex.Cursor.BeforeSHA256 != sha256Hex(mustRead(t, exe)) ||
		ex.Cursor.AfterSHA256 != ex.Cursor.BeforeSHA256 || ex.Children[0].Status != childPass || ex.Children[1].Status != childPass ||
		ex.Children[0].RunID == ex.Children[1].RunID {
		t.Fatalf("root %+v", ex)
	}
	for _, c := range ex.Children {
		dir := filepath.Join(root, filepath.FromSlash(c.Dir))
		m := bundleDoc[DevManifest](t, dir, devManifestFile)
		if m.Schema != DevCursorBundleSchema || m.Cursor == nil || *m.Cursor != (DevCursorBinding{Path: ex.Cursor.Path, BeforeSHA256: ex.Cursor.BeforeSHA256,
			AfterSHA256: ex.Cursor.BeforeSHA256}) || m.FlowSHA256 != ex.FlowSHA256 {
			t.Fatalf("%s manifest %+v", c.Dir, m.Cursor)
		}
		if code, rep, errOut := devCheck(t, dir); code != 0 || rep.Result != ResultPass {
			t.Fatalf("%s check %d %v %s", c.Dir, code, devCodes(rep), errOut)
		}
		// Its own five preflights and four sidecars.
		b, err := LoadDevBundle(dir)
		if err != nil || countEvents(b, func(e Event) bool { return e.Type == EvPreflightFinished }) != 5 || !cursorSidecarsOK(b) {
			t.Fatalf("%s: preflights or sidecars %v", c.Dir, err)
		}
	}
	// The full design gate's receipt and the light intake receipt, each
	// child's verified reviewer evidence.
	for _, rel := range []string{exerciseFullDir + "/receipts/01.json", exerciseLightDir + "/" + lightReceiptFile} {
		if !exists(root, rel) {
			t.Fatalf("gate %s not reached", rel)
		}
	}
	var out, errb strings.Builder
	if code := CheckExercise(root, &out, &errb); code != 0 {
		t.Fatalf("root check %d %s %s", code, out.String(), errb.String())
	}
	var rep DevExerciseReport
	json.Unmarshal([]byte(out.String()), &rep)
	if rep.Schema != devCursorExerciseReportSchema || rep.Accounting.WorkerCalls != exerciseHappyCalls || rep.Accounting.PreflightCalls != 10 ||
		rep.Children[0].PreflightCalls != 5 || rep.Children[1].PreflightCalls != 5 {
		t.Fatalf("root report %+v", rep)
	}
	// A failed or unsafe child never yields a root PASS.
	t.Run("failed_child", func(t *testing.T) {
		t.Parallel()
		y, _ := newCursorExerciseRig(t, happy(), []devAct{{marker: markerTriageUnconf}})
		if code := y.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		if ex := bundleDoc[DevExercise](t, y.root(), devExerciseFile); ex.Children[0].Status != childPass || ex.Children[1].Status != childFail {
			t.Fatalf("children %+v", ex.Children)
		}
	})
	t.Run("unsafe_child", func(t *testing.T) {
		t.Parallel()
		y, _ := newCursorExerciseRig(t, happy(), lightHappy())
		y.full.fault = "linger"
		if code := y.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		if ex := bundleDoc[DevExercise](t, y.root(), devExerciseFile); ex.Children[1].Status != childNotRun || ex.Children[1].Stop != "full_cleanup_incomplete" {
			t.Fatalf("children %+v", ex.Children)
		}
	})
}
