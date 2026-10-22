package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The eight function tests of design 12a-dev-flow-verify, one per FP.
// They drive MainWith through the offline rig and judge saved bundles with
// the offline check; none is a live pass, and none runs a vendor.

// offline is the offline check of the rig's bundle, which must equal the
// run's own derived report (the stored report and audit reproduce).
func (r *devRig) offline() (int, DevReport) {
	r.t.Helper()
	code, rep, errOut := devCheck(r.t, r.f.bundle())
	stored := r.report()
	if !bytes.Equal(mustJSON(rep), mustJSON(stored)) {
		r.t.Fatalf("the offline check does not repeat the stored report:\n%s\nstderr %s", mustJSON(rep), errOut)
	}
	return code, rep
}

// stored is the attempt's own checked verdict: the report its collector
// derived (FP-1's tests prove an offline check repeats it byte for byte)
// and the exit it implies.
func (r *devRig) stored() (int, DevReport) {
	r.t.Helper()
	rep := r.report()
	if rep.Result == ResultPass {
		return 0, rep
	}
	return 1, rep
}

func TestDevFullVerifyFP1(t *testing.T) {
	t.Parallel()
	t.Run("versioned_bundle", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.expect(0, "")
		if m := r.manifest(); m.Schema != DevBundleSchema || m.Scenario != scenarioFull || len(m.Inventory) == 0 || m.Executable.Before != m.Executable.After {
			t.Fatalf("manifest %+v", m)
		}
		if _, err := os.Stat(filepath.Join(r.f.evidence, DevExecutionFile)); err == nil {
			t.Fatal("a new run wrote the retired execution record")
		}
		code, rep := r.offline()
		if code != 0 || rep.Result != ResultPass {
			t.Fatalf("offline check %d %+v", code, devCodes(rep))
		}
		// The audit repeats too, bound to the report.
		b, err := LoadDevBundle(r.f.bundle())
		if err != nil {
			t.Fatal(err)
		}
		_, audit := CheckDevBundle(b)
		if !bytes.Equal(mustJSON(audit), r.evidence("audit.json")) || audit.ReportSHA256 != sha256Hex(r.evidence("report.json")) {
			t.Fatal("the stored audit is not the canonical one")
		}
	})
	t.Run("forged_and_mixed", func(t *testing.T) {
		t.Parallel()
		for name, c := range map[string]struct {
			mutate func(t *testing.T, dir string)
			want   int
		}{
			"execution_only": {func(t *testing.T, dir string) {
				os.WriteFile(filepath.Join(dir, DevExecutionFile), []byte(`{"schema":"`+DevExecutionSchema+`"}`+"\n"), 0o600)
			}, 2},
			"execution_manifest": {func(t *testing.T, dir string) {
				editJSON(t, dir, devManifestFile, func(m map[string]any) { m["schema"] = DevExecutionSchema })
			}, 2},
			"legacy_v1_manifest": {func(t *testing.T, dir string) {
				editJSON(t, dir, devManifestFile, func(m map[string]any) { m["schema"] = ManifestSchemaV1 })
			}, 2},
			"unknown_version": {func(t *testing.T, dir string) {
				editJSON(t, dir, devManifestFile, func(m map[string]any) { m["schema"] = "callsheet-dev-full-evidence/v2" })
			}, 2},
			"tampered_report": {func(t *testing.T, dir string) {
				editJSON(t, dir, "report.json", func(m map[string]any) { m["claim"] = "everything was judged" })
			}, 1},
			"tampered_audit_text": {func(t *testing.T, dir string) {
				os.WriteFile(filepath.Join(dir, "audit.md"), []byte("# all correct\n"), 0o600)
			}, 1},
			"missing_report": {func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "report.json")) }, 1},
		} {
			dir := devPassingBundle(t)
			c.mutate(t, dir)
			code, rep, _ := devCheck(t, dir)
			if code != c.want || rep.Result == ResultPass {
				t.Errorf("%s: exit %d want %d (%v)", name, code, c.want, devCodes(rep))
			}
			if c.want == 1 && !slices.Contains(devCriterion(rep, "manifest").Codes, CodeDerivedMismatch) {
				t.Errorf("%s: no derived_report_mismatch: %v", name, devCodes(rep))
			}
		}
	})
	t.Run("legacy_v3", func(t *testing.T) {
		t.Parallel()
		// A synthetic legacy bundle keeps its unchanged legacy result.
		dir := passingBundle(t)
		code, r, _ := checkBundle(t, dir)
		b, err := LoadBundle(dir)
		if err != nil || code != 0 || r.Result != ResultPass || !bytes.Equal(mustJSON(r), mustJSON(BuildReport(b, Check(b)))) {
			t.Fatalf("legacy check %d %v", code, err)
		}
	})
	t.Run("mcp_controls_repeat", func(t *testing.T) {
		t.Parallel()
		// The MCP positive controls (optional, partial and no coordinator
		// inspection): the public check leaves every input byte unchanged
		// and repeats its canonical report.
		for _, mode := range []string{inspectFull, inspectPartial, inspectNone} {
			dir := devPassingBundleOf(t, mode)
			before := treeState(t, dir)
			code1, rep1, _ := devCheck(t, dir)
			code2, rep2, _ := devCheck(t, dir)
			if code1 != 0 || code2 != 0 || !bytes.Equal(mustJSON(rep1), mustJSON(rep2)) || !bytes.Equal(bytes.TrimSpace(mustJSON(rep1)), bytes.TrimSpace(mustRead(t, filepath.Join(dir, "report.json")))) {
				t.Fatalf("%q: %d %d, reports differ or are not the stored one", mode, code1, code2)
			}
			if after := treeState(t, dir); !maps2Equal(before, after) {
				t.Fatalf("%q: the check changed the evidence", mode)
			}
		}
	})
	t.Run("check_cannot_launch", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		before := treeState(t, dir)
		var out, errb bytes.Buffer
		h := Host{Args: []string{"check", "--evidence", dir}, Stdout: &out, Stderr: &errb,
			NewWorld: func(Host) (*World, func(), error) { t.Error("check built a world"); return nil, nil, nil }}
		if code := MainWith(h); code != 0 {
			t.Fatalf("check %d %s", code, errb.String())
		}
		if after := treeState(t, dir); !maps2Equal(before, after) {
			t.Fatal("the check changed the evidence")
		}
	})
}

// maps2Equal compares two string maps.
func maps2Equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestDevFullVerifyFP2(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		steps []devAct
		owner []string
		code  int
		stop  string
	}{
		// Both loops with a triage recovery inside the code loop: a design
		// revision, a changes-requested review, a fix needing triage, the
		// probable cause's fix and the approving review.
		"both_loops_triage_recovery": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignChanges}, {marker: markerDesignDraft},
			{marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges}, {marker: markerNeedsTriage},
			{marker: markerTriageProb, designRequired: designRequiredNo}, {marker: markerFixesComplete}, {marker: markerReviewApproved}},
			[]string{"yes 2"}, 0, ""},
		// Owner renewal twice: the coder's questions and then its escalation
		// each return to the designer and a new generation's approval.
		"owner_renewal_escalation": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerNeedsDesign}, {marker: markerDesignDraft},
			{marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved}}, []string{"yes 1", "yes 2", "yes 3"}, 0, ""},
		"design_cap": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignChanges}, {marker: markerDesignDraft}, {marker: markerDesignChanges},
			{marker: markerDesignDraft}, {marker: markerDesignChanges}}, nil, 1, stopCap},
		"code_cap": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerFixesComplete}, {marker: markerReviewChanges}, {marker: markerFixesComplete}, {marker: markerReviewChanges}}, []string{"yes 1"}, 1, stopCap},
		"unsupported_cause": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerNeedsTriage}, {marker: markerTriageUnconf}}, []string{"yes 1"}, 1, "cause_unconfirmed"},
		"stale_approval": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
			{marker: markerDesignDraft, alter: "same-design"}, {marker: markerDesignApproved}}, []string{"yes 1", "yes 1"}, 1, "owner_eof"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, c.steps)
			r.owner = c.owner
			if name == "stale_approval" {
				// An old-generation yes for a byte-identical redraft approves
				// nothing; then the terminal closes.
				r.onTick = func(r *devRig) {
					if w := r.d.window; w != nil && w.gen == 2 && r.answered[w] && r.fault == "" {
						r.fault = "closed"
						r.term.close()
					}
				}
			}
			r.expect(c.code, c.stop)
			// The attempt's own checked verdict (FP-1 and the offline replay
			// prove an offline check repeats it byte for byte).
			code, rep := r.stored()
			if code != c.code {
				t.Fatalf("offline %d want %d: %v", code, c.code, devCodes(rep))
			}
			if c.code != 0 {
				// An honest stop: the policy codes only, the rest not reached
				// or passing, never an invented failure of the transitions.
				if rep.Stop != c.stop || len(devCriterion(rep, "transitions").Codes) != 0 {
					t.Fatalf("stop %q transitions %v", rep.Stop, devCodes(rep))
				}
			}
			// The offline check repeated the stored audit byte for byte; its
			// rows are unique and cite the bundle's own bytes.
			audit := bundleDoc[DevAudit](t, r.f.bundle(), "audit.json")
			ids := map[string]bool{}
			for _, row := range append(slices.Clone(audit.Rows), audit.JudgmentRows...) {
				if ids[row.ID] {
					t.Fatalf("row %s repeats", row.ID)
				}
				ids[row.ID] = true
				for _, s := range row.Sources {
					if s.Commit != "" {
						continue
					}
					if data, err := os.ReadFile(filepath.Join(r.f.bundle(), filepath.FromSlash(s.File))); err != nil || sha256Hex(data) != s.SHA256 {
						t.Fatalf("row %s cites %s with a wrong digest", row.ID, s.File)
					}
				}
			}
			for _, task := range r.d.state.Tasks {
				for _, kind := range []string{"dispatch", "wait", "publication", "marker"} {
					if !ids[fmt.Sprintf("task/%02d/%s", task.Ordinal, kind)] {
						t.Fatalf("no %s row for task %d", kind, task.Ordinal)
					}
				}
				if task.Role == "code-reviewer" && !ids[fmt.Sprintf("task/%02d/reviewer-execution", task.Ordinal)] {
					t.Fatalf("no reviewer row for task %d", task.Ordinal)
				}
			}
			for i := range r.d.state.Transitions {
				if !ids[fmt.Sprintf("transition/%02d/route", i+1)] {
					t.Fatalf("no row for transition %d", i+1)
				}
			}
			for _, j := range audit.JudgmentRows {
				if j.Layer != LayerJudgment || j.Verdict != VerdictUnobserved {
					t.Fatalf("judgment row %+v", j)
				}
			}
			if name == "stale_approval" {
				st := r.d.state
				if len(st.Receipts) != 1 || st.Receipts[0].Valid || st.Generation != 2 {
					t.Fatalf("a byte-identical redraft kept its approval: %+v", st.Receipts)
				}
			}
		})
	}
	// The recorded Callsheet MCP calls (design r0.6): the complete positive
	// attempts with optional, partial and no coordinator inspection; one
	// positive and one binding rejection per permitted call form through
	// public check; and every enumerated bundle-level scenario.
	t.Run("mcp_positive_attempts", func(t *testing.T) { t.Parallel(); testMCPPositiveAttempts(t) })
	t.Run("mcp_wiring", func(t *testing.T) { t.Parallel(); testMCPWiring(t) })
	t.Run("mcp_scenarios", func(t *testing.T) { t.Parallel(); testMCPScenarios(t) })
	// Design 12a-dev-flow-verify r0.8: the system task_notification wake
	// completes every slot of the normal workflow (never waiting until the
	// task bound) and the offline check re-derives the same wake facts.
	t.Run("system_wake", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.wakeForm = "system"
		r.expect(0, "")
		if code, rep := r.offline(); code != 0 {
			t.Fatalf("offline %v", devCodes(rep))
		}
		checkSystemWakes(t, r, len(r.d.state.Tasks))
	})
	// A refused notification stops the attempt at its first observed
	// complete record, before the slot's bound, in both forms; the valid
	// record after it repairs nothing, and the offline check names the
	// same record.
	for _, form := range []string{"", "system"} {
		for _, alter := range []string{"bad-wake", "failed-wake", "early-fail", "malformed-wake"} {
			t.Run("refused_"+cmpOr(form, "xml")+"_"+alter, func(t *testing.T) {
				t.Parallel()
				r := newDevRig(t, []devAct{{marker: markerDesignDraft, alter: alter}})
				r.wakeForm = form
				r.expect(1, devWakeUnsupported)
				checkRefusedWake(t, r)
				code, rep := r.offline()
				if waits := devCriterion(rep, "waits").Codes; code != 1 || slices.Contains(waits, "event_map_mismatch") {
					t.Fatalf("offline %v", devCodes(rep))
				}
				// The re-derived event map names the same record: the
				// phase's own refusal (a notification's shape or session),
				// else the slot's.
				em := bundleDoc[DevEventMap](t, r.f.bundle(), devEventMapFile)
				src, problem := em.Phases[0].ProblemSource, em.Phases[0].Problem
				if alter != "malformed-wake" {
					if len(em.Tasks) != 1 || em.Phases[0].Problem != "" {
						t.Fatalf("event map %+v %+v", em.Phases, em.Tasks)
					}
					src, problem = em.Tasks[0].ProblemSource, em.Tasks[0].Problem
					if !slices.Contains(devCriterion(rep, "waits").Codes, devWakeUnsupported) {
						t.Fatalf("offline %v", devCodes(rep))
					}
				} else if !slices.Contains(devCriterion(rep, "coordinator_setup").Codes, devWakeUnsupported) {
					t.Fatalf("offline %v", devCodes(rep))
				}
				if problem != devWakeUnsupported || src == nil || src.FirstLine != r.refusedLine || src.File != devStreamFile(1) {
					t.Fatalf("event map %+v %+v", em.Phases, em.Tasks)
				}
			})
		}
	}
	// Code review C1: a complete invalid notification observed while the
	// consumed wait's publication is being inspected (its pull held until
	// the attempt stops) is refused at once, never left to the bound.
	for _, form := range []string{"", "system"} {
		t.Run("refused_during_inspection_"+cmpOr(form, "xml"), func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, []devAct{{marker: markerDesignDraft}})
			r.wakeForm = form
			r.tickDuringPull = true
			// The phase stream is taken on the supervisor's goroutine; the
			// held pull writes the duplicate through it (the buffer is
			// synchronized, the phase list is not).
			var stream *limitBuffer
			r.onTick = func(r *devRig) {
				if p := r.d.current(); p != nil && r.d.inspection == nil {
					stream = p.stdout
				}
			}
			r.pullCtx = func(ctx context.Context, r *devRig) error {
				stream.Write(r.record(r.notification(r.waitHandle(), r.use, "completed")))
				r.refusedLine = bytes.Count(stream.Bytes(), []byte("\n"))
				// Held until the attempt stops; the real-time bound only
				// turns a missed refusal into a failure instead of a hang.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(30 * time.Second):
					return errors.New("the held pull was never cancelled")
				}
			}
			r.expect(1, devWakeUnsupported)
			if r.d.inspection == nil {
				t.Fatal("no inspection was running")
			}
			checkRefusedWake(t, r)
		})
	}
	// Design 12a-dev-flow-verify r0.9: the wait start's optional Bash
	// timeout. A positive integer token (the captured 900000, 1, beyond
	// uint64) is accepted and the workflow completes, in both wake forms;
	// any other value fails wait_command_mismatch at the start; a stopped
	// wait (its timeout) is refused at once while the worker still runs;
	// a prior slot's grammar command with an invalid input is never an
	// unrelated job.
	for name, c := range map[string]struct{ timeout, form string }{
		"captured_system": {"900000", "system"}, "captured_xml": {"900000", ""}, "one": {"1", "system"}, "beyond_uint64": {"99999999999999999999999", ""},
	} {
		t.Run("timeout_"+name, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, happy())
			r.owner = []string{"yes 1"}
			r.wakeForm, r.waitTimeout = c.form, c.timeout
			r.expect(0, "")
			if code, rep := r.offline(); code != 0 {
				t.Fatalf("offline %v", devCodes(rep))
			}
			if c.form == "system" {
				checkSystemWakes(t, r, len(r.d.state.Tasks))
			}
		})
	}
	for _, timeout := range []string{"1.5", "1e3", "0", "-1"} {
		t.Run("timeout_invalid_"+timeout, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, []devAct{{marker: markerDesignDraft}})
			r.waitTimeout = timeout
			r.expect(1, devWaitMismatch)
			code, rep := r.offline()
			if waits := devCriterion(rep, "waits").Codes; code != 1 || !slices.Contains(waits, devWaitMismatch) || slices.Contains(waits, "event_map_mismatch") {
				t.Fatalf("offline %v", devCodes(rep))
			}
		})
	}
	for _, form := range []string{"", "system"} {
		t.Run("timeout_stopped_"+cmpOr(form, "xml"), func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, []devAct{{marker: markerDesignDraft, alter: "early-stopped"}})
			r.wakeForm, r.waitTimeout = form, "900000"
			r.expect(1, devWakeUnsupported)
			checkRefusedWake(t, r)
			if code, rep := r.offline(); code != 1 || !slices.Contains(devCriterion(rep, "waits").Codes, devWakeUnsupported) {
				t.Fatalf("offline %v", devCodes(rep))
			}
		})
	}
	t.Run("prior_slot_invalid", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved, alter: "prior-invalid"}})
		r.expect(1, devWaitMismatch)
		code, rep := r.offline()
		if waits := devCriterion(rep, "waits").Codes; code != 1 || !slices.Contains(waits, devWaitMismatch) || slices.Contains(waits, "event_map_mismatch") {
			t.Fatalf("offline %v", devCodes(rep))
		}
		em := bundleDoc[DevEventMap](t, r.f.bundle(), devEventMapFile)
		if len(em.Tasks) != 2 || em.Tasks[1].Problem != devWaitMismatch {
			t.Fatalf("event map %+v", em.Tasks)
		}
	})
	t.Run("deadline_without_wake", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft, alter: "deadline-no-wake"}})
		r.expect(1, "task_timeout")
		id := r.workflowTasks()[0]
		if r.d.stopReason != "workflow task "+id+" reached its 10m bound after admission before wait consumption and publication admission completed" {
			t.Fatalf("stop reason %q", r.d.stopReason)
		}
	})
}

// checkSystemWakes checks a passing attempt's first n slots each woke on
// its system task_notification record, by the stored and re-derived event
// map alike.
func checkSystemWakes(t *testing.T, r *devRig, n int) {
	t.Helper()
	stored := bundleDoc[DevEventMap](t, r.f.bundle(), devEventMapFile)
	if len(stored.Tasks) < n {
		t.Fatalf("event map tasks %d", len(stored.Tasks))
	}
	for _, tf := range stored.Tasks[:n] {
		if !tf.Wait.complete() || tf.Problem != "" || len(tf.WaitSources) != 4 {
			t.Fatalf("task %d wait %+v %s", tf.Ordinal, tf.Wait, tf.Problem)
		}
		wake := tf.WaitSources[2]
		lines := strings.Split(string(r.evidence(wake.File)), "\n")
		if !strings.Contains(lines[wake.FirstLine-1], `"subtype":"task_notification"`) {
			t.Fatalf("task %d woke on %s", tf.Ordinal, lines[wake.FirstLine-1])
		}
	}
}

// checkRefusedWake checks an attempt stopped at its refused notification:
// the reason names the record, before the slot's 10m bound.
func checkRefusedWake(t *testing.T, r *devRig) {
	t.Helper()
	sl := r.d.slot
	want := fmt.Sprintf(": the notification at %s line %d", devStreamFile(1), r.refusedLine)
	if r.refusedLine == 0 || !strings.HasSuffix(r.d.stopReason, want) {
		t.Fatalf("stop reason %q want suffix %q", r.d.stopReason, want)
	}
	if sl == nil || sl.admittedAt.IsZero() || !r.d.s.w.Clock.Now().Before(sl.admittedAt.Add(FeatureTaskBound)) {
		t.Fatal("the refusal waited for the task bound")
	}
}

func TestDevFullVerifyFP3(t *testing.T) {
	t.Parallel()
	// Each role publishes its authorized report, then a malicious change:
	// the live admission rejects it and the offline check agrees.
	for name, c := range map[string]struct {
		steps []devAct
		owner []string
	}{
		"designer_source":         {[]devAct{{marker: markerDesignDraft, alter: "edit-main"}}, nil},
		"design_reviewer_scratch": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved, alter: "scratch-pub"}}, nil},
		"coder_mode":              {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete, alter: "exec-mode"}}, []string{"yes 1"}},
		"reviewer_source": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete},
			{marker: markerReviewApproved, alter: "edit-main"}}, []string{"yes 1"}},
		"triage_source": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerNeedsTriage}, {marker: markerTriageConf, designRequired: designRequiredNo, alter: "edit-main"}}, []string{"yes 1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, c.steps)
			r.owner = c.owner
			r.expect(1, "publication_rejected")
			code, rep := r.stored()
			if code != 1 || rep.Stop != "publication_rejected" {
				t.Fatalf("offline %d %v", code, devCodes(rep))
			}
		})
	}
	t.Run("forged_records", func(t *testing.T) {
		t.Parallel()
		// A role's publication is replaced by a malicious tree with every
		// stored record naming it, the inventory and the derived summaries
		// recomputed: the complete-tree recheck still rejects it.
		type edit = func(files map[string][]byte) map[string]filemode.FileMode
		for name, c := range map[string]struct {
			ordinal int
			edit    edit
		}{
			"designer_source": {1, func(f map[string][]byte) map[string]filemode.FileMode {
				f["main.go"] = []byte("package main\n\nfunc main() {}\n")
				return nil
			}},
			"design_reviewer_scratch": {2, func(f map[string][]byte) map[string]filemode.FileMode {
				f[reviewScratch+"/t/after/main.go"] = f["main.go"]
				return nil
			}},
			"coder_mode": {3, func(f map[string][]byte) map[string]filemode.FileMode {
				return map[string]filemode.FileMode{"main.go": filemode.Executable}
			}},
			"reviewer_source": {6, func(f map[string][]byte) map[string]filemode.FileMode {
				f["main.go"] = []byte("package main\n\nfunc main() {}\n")
				return nil
			}},
			"reviewer_mode": {6, func(f map[string][]byte) map[string]filemode.FileMode {
				return map[string]filemode.FileMode{"main_test.go": filemode.Executable}
			}},
			"reviewer_extra": {6, func(f map[string][]byte) map[string]filemode.FileMode {
				f["notes.md"] = []byte("x\n")
				return nil
			}},
			"reviewer_deleted": {6, func(f map[string][]byte) map[string]filemode.FileMode {
				delete(f, "design.md")
				return nil
			}},
		} {
			dir := devPassingBundle(t)
			code, rep := forgeTask(t, dir, c.ordinal, c.edit)
			if name == "reviewer_source" {
				// Through the saved bundle and the command's check.
				code, rep, _ = devCheck(t, dir)
			}
			if code == 0 || rep.Result == ResultPass || len(devCriterion(rep, "publications").Codes) == 0 {
				t.Errorf("%s: exit %d %v", name, code, devCodes(rep))
			}
		}
	})
}

func TestDevFullVerifyFP4(t *testing.T) {
	t.Run("real_helper", func(t *testing.T) {
		t.Parallel()
		goBin, err := exec.LookPath("go")
		if err != nil {
			t.Fatalf("the local Go toolchain is required: %v", err)
		}
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.f.goPath = goBin
		r.realHelper = os.Getenv("PATH")
		r.expect(0, "")
		review := r.d.state.Tasks[3]
		rec := r.receipts[review.TaskID]
		if rec.Status != reviewPassed || rec.Coverage == nil || !rec.Coverage.Pass || !rec.ScratchRemoved || !rec.Tests.Passed ||
			!strings.HasPrefix(rec.Go.Version, "go version") || len(rec.Commands) != 4 {
			t.Fatalf("receipt %+v", rec)
		}
		var ev DevReviewRecord
		if err := json.Unmarshal(r.evidence("tasks/04/review-evidence.json"), &ev); err != nil || ev.Evidence.Status != evidenceVerified ||
			ev.Evidence.RunSHA256 != sha256Hex([]byte(r.helperOut[review.TaskID])) || ev.Evidence.Started == nil {
			t.Fatalf("evidence %+v %v", ev, err)
		}
		// Only review.md changed.
		if pub := r.d.trees[review.Commit]; len(pub) != len(r.d.trees[r.d.state.Tasks[2].Commit])+1 {
			t.Fatalf("the review published %d files", len(pub))
		}
		if code, _ := r.offline(); code != 0 {
			t.Fatal("offline")
		}
	})
	// Through the attempt: a prose-only approval and a cross-task receipt
	// (TestDevFullReviewEvidence covers every other forged record shape on
	// the pure derivation the attempt and the check share).
	for alter, stop := range map[string]string{"prose-only": CodeReviewUnobserved, "cross-task": CodeReviewMismatch} {
		t.Run(alter, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved, alter: alter}})
			r.owner = []string{"yes 1"}
			r.expect(1, stop)
			if code, rep := r.stored(); code != 1 || rep.Result == ResultPass {
				t.Fatal("offline accepted")
			}
		})
	}
	t.Run("failed_run_changes_requested", func(t *testing.T) {
		t.Parallel()
		// A complete failed receipt supports changes requested: a legal
		// nonterminal transition, then the fix and an approving review.
		r := newDevRig(t, devLoopSteps())
		r.owner = []string{"yes 1"}
		failing := true
		r.onTick = func(r *devRig) {
			if failing && len(r.d.state.Tasks) == 3 {
				r.reviewGo.testExit = 1
			}
			if failing && len(r.d.state.Tasks) == 4 {
				failing, r.reviewGo.testExit = false, 0
			}
		}
		r.expect(0, "")
		first := r.receipts[r.d.state.Tasks[3].TaskID]
		if first.Status != reviewFailed || first.Code != "command_failed" {
			t.Fatalf("first receipt %+v", first)
		}
		if code, _ := r.offline(); code != 0 {
			t.Fatal("offline")
		}
	})
}

func TestDevFullVerifyFP5(t *testing.T) {
	t.Run("real_oracle_after_fix_loop", func(t *testing.T) {
		t.Parallel()
		goBin, err := exec.LookPath("go")
		if err != nil {
			t.Fatalf("the local Go toolchain is required: %v", err)
		}
		r := newDevRig(t, devLoopSteps())
		r.owner = []string{"yes 1"}
		r.f.goPath = goBin
		// The scripted startup and baseline report the live toolchain's
		// version, which the real final validation records (the pinned
		// toolchain binding; CI's Go is the go.mod version, not the
		// script's).
		live, err := exec.Command(goBin, "version").Output()
		if err != nil {
			t.Fatalf("go version: %v", err)
		}
		scripted := r.f.override
		r.f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == goBin && len(s.Args) == 1 && s.Args[0] == "version" {
				return behavior{stdout: string(live)}, true
			}
			if scripted != nil {
				return scripted(s)
			}
			return behavior{}, false
		}
		// The final validation runs for real; everything else stays scripted.
		r.f.launcher.real = func(s ProcSpec) bool {
			return strings.Contains(s.Dir, string(filepath.Separator)+"final"+string(filepath.Separator)) || filepath.Base(s.Path) == "greeting"
		}
		r.expect(0, "")
		m := r.manifest()
		if m.FinalCandidate != r.d.state.Tasks[4].Commit || m.FinalCandidate == r.d.state.Tasks[2].Commit {
			t.Fatalf("the candidate is not the fix loop's: %s", m.FinalCandidate)
		}
		var final TestRun
		if err := json.Unmarshal(r.evidence("validation/final-test.json"), &final); err != nil || final.Toolchain != strings.TrimSpace(string(live)) {
			t.Fatalf("final toolchain %q, live %q (%v)", final.Toolchain, live, err)
		}
		var cov DevCoverageRecord
		if err := json.Unmarshal(r.evidence(devCoverageFile), &cov); err != nil || cov.Coverage == nil || !cov.Coverage.Pass ||
			!strings.HasPrefix(string(r.evidence("validation/coverage.out")), "mode: set\n") {
			t.Fatalf("coverage %+v %v", cov, err)
		}
		if code, _ := r.offline(); code != 0 {
			t.Fatal("offline")
		}
	})
	t.Run("ten_broken_behaviors", func(t *testing.T) {
		t.Parallel()
		// The program misbehaves on every one of the ten cases while every
		// worker claims success and the review approves: each case fails the
		// oracle on its own observed run (its own row), and the approval is
		// a contradiction. (TestDevFullOracle breaks each case alone and
		// proves only its own row fails.)
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		for _, fc := range FeatureCases {
			r.f.cases[fc.ID] = behavior{stdout: "Hello, wrong!\n", exit: 0}
			if fc.Kind == "ok" {
				r.f.cases[fc.ID] = behavior{stdout: "Hello, wrong!\n", stderr: "noise\n", exit: 1}
			}
		}
		r.expect(1, CodeCaseFailed)
		code, rep := r.stored()
		oracle := devCriterion(rep, "feature_oracle").Codes
		if code != 1 || !slices.Contains(oracle, CodeCaseFailed) || !slices.Contains(oracle, CodeReviewContra) {
			t.Fatalf("offline %d %v", code, devCodes(rep))
		}
		if last := r.d.state.Tasks[len(r.d.state.Tasks)-1]; last.Marker != markerReviewApproved {
			t.Fatalf("the review did not approve: %s", last.Marker)
		}
		var cases CasesRecord
		if err := json.Unmarshal(r.evidence("validation/cases.json"), &cases); err != nil || len(cases.Cases) != len(FeatureCases) {
			t.Fatalf("cases %v", err)
		}
		failed := map[string]bool{}
		for _, row := range bundleDoc[DevAudit](t, r.f.bundle(), "audit.json").Rows {
			if strings.HasPrefix(row.ID, "case/") && row.Verdict == VerdictIncorrect && slices.Contains(row.Codes, CodeCaseFailed) {
				failed[strings.TrimPrefix(row.ID, "case/")] = true
			}
		}
		for i, fc := range FeatureCases {
			if cases.Cases[i].Pass || !failed[fc.ID] {
				t.Errorf("case %s was not failed on its own run", fc.ID)
			}
		}
	})
}

func TestDevFullVerifyFP6(t *testing.T) {
	t.Parallel()
	t.Run("interruption", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		ctx, cancel := context.WithCancel(context.Background())
		r.f.ctx = ctx
		r.onTick = func(r *devRig) {
			if len(r.d.state.Tasks) == 2 && r.d.slot != nil && r.d.slot.observed {
				cancel()
			}
		}
		r.expect(1, "interrupted")
		code, rep := r.offline()
		var cost DevCost
		json.Unmarshal(r.evidence(devCostFile), &cost)
		// Every paid call the plane holds is accounted, the interrupted one
		// included, and cleanup ended every task.
		c := r.cleanupRecord()
		if code != 1 || rep.Stop != "interrupted" || !slices.Equal(cost.WorkflowTasks, r.workflowTasks()) || !slices.Equal(cost.PreflightTasks, r.d.s.preflight) ||
			cost.WorkerCalls != len(r.f.plane.ids()) || cost.ProviderUsage != usageUnknown || !c.Complete || len(c.UnresolvedTasks) != 0 {
			t.Fatalf("interrupted accounting %+v %+v %v", cost, c, devCodes(rep))
		}
		for _, id := range r.f.plane.ids() {
			if !contract.TaskTerminal(r.terminalState(id)) {
				t.Fatalf("task %s left %s", id, r.terminalState(id))
			}
		}
	})
	t.Run("extra_admission", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}})
		r.fault = "extra"
		r.expect(1, "extra_admission")
		code, rep := r.offline()
		var cost DevCost
		json.Unmarshal(r.evidence(devCostFile), &cost)
		if code != 1 || len(cost.RejectedTasks) != 1 || !slices.Contains(devCriterion(rep, "admissions").Codes, "extra_admission") || cost.Failed < 1 {
			t.Fatalf("extra admission %+v %v", cost, devCodes(rep))
		}
	})
	t.Run("lingering_process", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.fault = "linger"
		if got := r.run(); got != 1 {
			t.Fatalf("exit %d", got)
		}
		code, rep := r.offline()
		if code != 1 || len(devCriterion(rep, "cleanup").Codes) == 0 || r.cleanupRecord().Complete {
			t.Fatalf("lingering %v", devCodes(rep))
		}
	})
	t.Run("protected_modification", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		claude := filepath.Join(r.f.home, ".claude", "CLAUDE.md")
		r.onTick = func(r *devRig) {
			if r.fault == "" && len(r.d.state.Tasks) == 1 {
				r.fault = "edited"
				os.WriteFile(claude, []byte("# changed by a worker\n"), 0o600)
			}
		}
		if got := r.run(); got != 1 {
			t.Fatalf("exit %d", got)
		}
		code, rep := r.offline()
		if code != 1 || !slices.Contains(devCriterion(rep, "owner_environment").Codes, CodeEnvModified) || len(rep.Protected) == 0 {
			t.Fatalf("protected %v", devCodes(rep))
		}
		// Nothing was restored.
		if b, _ := os.ReadFile(claude); string(b) != "# changed by a worker\n" {
			t.Fatal("the harness restored an owner file")
		}
	})
	// Live probe 20261022T132232Z-3fe07457 (design 12a-dev-flow-verify
	// r0.8, FP-6): Claude Code's synced-skills bookkeeping changed during
	// the attempt. Only the enumerated bookkeeping entries are
	// tool-maintained (report-only, metadata only); a synced skill's
	// content stays protected.
	syncedSkills := func(t *testing.T, r *devRig) string {
		t.Helper()
		synced := filepath.Join(r.f.home, ".claude", "skills", "synced")
		acct := filepath.Join(synced, "acct-1")
		for p, body := range map[string]string{
			filepath.Join(acct, "manifest.json"):              `{"skills":["greet"]}`,
			filepath.Join(acct, "greet", "SKILL.md"):          "# greet\n",
			filepath.Join(acct, ".last-complete-round"):       "1\n",
			filepath.Join(acct, ".staging", "greet", "x.md"):  "staged\n",
			filepath.Join(synced, ".bucket-f21645ff-0001"):    "b\n",
			filepath.Join(synced, ".bucket-f21645ff-gone"):    "g\n",
			filepath.Join(acct, ".staging", "old", "deep.md"): "old\n",
		} {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return synced
	}
	t.Run("synced_skills_bookkeeping", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		synced := syncedSkills(t, r)
		acct := filepath.Join(synced, "acct-1")
		r.onTick = func(r *devRig) {
			if r.fault == "" && len(r.d.state.Tasks) == 1 {
				r.fault = "edited"
				// Added, modified and deleted bookkeeping of every pattern.
				os.WriteFile(filepath.Join(acct, ".last-complete-round"), []byte("2\n"), 0o600)
				os.RemoveAll(filepath.Join(acct, ".staging", "old"))
				os.WriteFile(filepath.Join(acct, ".staging", "greet", "x.md"), []byte("restaged\n"), 0o600)
				os.MkdirAll(filepath.Join(acct, ".staging", "new"), 0o700)
				os.WriteFile(filepath.Join(acct, ".staging", "new", "y.md"), []byte("y\n"), 0o600)
				os.WriteFile(filepath.Join(synced, ".bucket-f21645ff-0001"), []byte("bb\n"), 0o600)
				os.Remove(filepath.Join(synced, ".bucket-f21645ff-gone"))
				os.WriteFile(filepath.Join(synced, ".bucket-f21645ff-0002"), []byte("n\n"), 0o600)
			}
		}
		r.expect(0, "")
		code, rep := r.offline()
		var diff EnvDiff
		json.Unmarshal(r.evidence("environment/diff.json"), &diff)
		if code != 0 || len(rep.Protected) != 0 || diff.ToolDetailCount+diff.ToolOmittedCount < 8 {
			t.Fatalf("synced-skills bookkeeping %v %+v", devCodes(rep), diff.Protected)
		}
		for _, e := range diff.Detail {
			for _, rec := range []*EnvRecord{e.Before, e.After} {
				if rec != nil && strings.Contains(decodeToken(rec.Path), "skills/synced/") && (rec.Class != ClassTool || rec.SHA256 != "") {
					t.Fatalf("bookkeeping record %+v is not metadata-only tool state", *rec)
				}
			}
		}
	})
	t.Run("synced_skill_content", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		synced := syncedSkills(t, r)
		r.onTick = func(r *devRig) {
			if r.fault == "" && len(r.d.state.Tasks) == 1 {
				r.fault = "edited"
				os.WriteFile(filepath.Join(synced, "acct-1", "greet", "SKILL.md"), []byte("# changed\n"), 0o600)
			}
		}
		if got := r.run(); got != 1 {
			t.Fatalf("exit %d", got)
		}
		code, rep := r.offline()
		if code != 1 || !slices.Contains(devCriterion(rep, "owner_environment").Codes, CodeEnvModified) || len(rep.Protected) != 1 {
			t.Fatalf("synced skill content %v %+v", devCodes(rep), rep.Protected)
		}
	})
	t.Run("tool_state_only", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		history := filepath.Join(r.f.home, ".claude", "history.jsonl")
		r.onTick = func(r *devRig) {
			if r.fault == "" && len(r.d.state.Tasks) == 1 {
				r.fault = "edited"
				os.WriteFile(history, []byte(`{"display":"new"}`+"\n"), 0o600)
			}
		}
		r.expect(0, "")
		code, rep := r.offline()
		var diff EnvDiff
		json.Unmarshal(r.evidence("environment/diff.json"), &diff)
		if code != 0 || diff.ToolDetailCount+diff.ToolOmittedCount == 0 || len(rep.Protected) != 0 || !strings.Contains(string(r.evidence("report.txt")), "tool-maintained") {
			t.Fatalf("tool state %v %+v", devCodes(rep), diff)
		}
		if b, _ := os.ReadFile(history); string(b) != `{"display":"new"}`+"\n" {
			t.Fatal("the harness restored tool state")
		}
	})
}

func TestDevFullVerifyFP7(t *testing.T) {
	t.Parallel()
	t.Run("default_and_alias", func(t *testing.T) {
		t.Parallel()
		for _, cmd := range []string{"run", "run-dev-full"} {
			r := newDevRig(t, happy())
			r.owner = []string{"yes 1"}
			r.command = cmd
			r.expect(0, "")
			if r.manifest().Schema != DevBundleSchema {
				t.Fatalf("%s wrote %s", cmd, r.manifest().Schema)
			}
		}
		// A stop is 1; a usage error, a gate refusal and an unknown flag are 2.
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{"no 1"}
		r.command = "run-dev-full"
		r.expect(1, "owner_no")
		for _, args := range [][]string{{"run", "--bogus"}, {"run-dev-full"}, {"run", "--callsheet", "rel"}} {
			var out, errb bytes.Buffer
			h := Host{GOOS: "linux", Args: args, Getenv: func(k string) string { return map[string]string{EnvOptIn: "1"}[k] },
				LookupEnv: func(string) (string, bool) { return "", false }, Stdout: &out, Stderr: &errb,
				NewWorld: func(Host) (*World, func(), error) { t.Error("a usage error built a world"); return nil, nil, nil }}
			if code := MainWith(h); code != 2 {
				t.Fatalf("%v exit %d", args, code)
			}
		}
	})
	t.Run("legacy_flow_refused", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		flow := filepath.Join(t.TempDir(), "flow.json")
		if err := os.WriteFile(flow, mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(legacyExampleDir), FlowFile)), 0o600); err != nil {
			t.Fatal(err)
		}
		r.f.flow = flow
		r.expect(1, "flow_invalid")
		if len(r.f.plane.ids()) != 0 || len(r.f.launcher.specsOf(fakeCallsheet)) != 0 {
			t.Fatal("work started for a legacy four-role flow")
		}
	})
	t.Run("provenance_and_docs", func(t *testing.T) {
		t.Parallel()
		// The saved attempt's captured method sources are the shipped ones.
		root := repoRoot(t)
		sources := bundleDoc[devEnvelope[[]SourceRecord]](t, devPassingBundle(t), "setup/sources.json").Data
		if len(sources) != 15 {
			t.Fatalf("sources %d", len(sources))
		}
		doc := string(mustRead(t, filepath.Join(root, "docs", "real-e2e.md")))
		for _, s := range sources {
			if sha256Hex(mustRead(t, filepath.Join(root, filepath.FromSlash(s.Path)))) != s.SHA256 {
				t.Fatalf("%s is not the shipped method", s.Path)
			}
			if !strings.Contains(doc, "("+strings.TrimPrefix(s.Path, "docs/")+")") {
				t.Fatalf("the runbook does not link %s", s.Path)
			}
		}
		// The legacy four-role files left the shipped examples for the
		// labelled test fixture; the examples directory keeps its README and
		// the checked dev-full method.
		legacy, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(legacyExampleDir)))
		if err != nil || len(legacy) == 0 {
			t.Fatalf("legacy fixture %v", err)
		}
		examples := filepath.Join(root, filepath.FromSlash(ExamplesDir))
		for _, e := range legacy {
			if _, err := os.Lstat(filepath.Join(examples, e.Name())); err == nil {
				t.Fatalf("docs/examples/real-e2e still ships the legacy %s", e.Name())
			}
		}
		for _, n := range []string{"README.md", "dev-full/flow.json"} {
			if st, err := os.Lstat(filepath.Join(examples, filepath.FromSlash(n))); err != nil || !st.Mode().IsRegular() {
				t.Fatalf("docs/examples/real-e2e lacks %s", n)
			}
		}
	})
	t.Run("legacy_compatibility", func(t *testing.T) {
		t.Parallel()
		if code, r, _ := checkBundle(t, passingBundle(t)); code != 0 || r.Result != ResultPass {
			t.Fatalf("legacy check %d", code)
		}
		// The legacy helpers stay callable and keep refusing a development
		// run file; the development helpers refuse a legacy one.
		dev := r2DevRuntime(t)
		var out, errb bytes.Buffer
		if code := MainWith(Host{Args: []string{"observe", "--run", dev, "--task", hexID("t_", 1), "--hop", "designer"}, Stdout: &out, Stderr: &errb}); code != 2 {
			t.Fatalf("legacy observe on a development runtime: %d %s", code, errb.String())
		}
		for _, helper := range []string{"observe", "wait-task", "review-material", "dev-next", "dev-observe", "dev-wait-task", "dev-review-verify"} {
			if !strings.Contains(Usage, "reale2e "+helper+" ") {
				t.Fatalf("Usage does not list %s", helper)
			}
		}
		if !strings.Contains(Usage, "legacy-runtime compatibility helpers") {
			t.Fatal("Usage does not label the legacy helpers")
		}
	})
}

// r2DevRuntime is a development runtime directory with its run file.
func r2DevRuntime(t *testing.T) string {
	t.Helper()
	rt := shortTemp(t)
	run := "20261018T000000Z-01020304"
	b, _ := encodeJSON(DevRunFile{Schema: DevRunSchema, RunID: run, Callsheet: "/c", Plane: "https://127.0.0.1:1", CA: "/rt/ca.crt"})
	os.WriteFile(filepath.Join(rt, RunFileName), b, 0o600)
	os.WriteFile(filepath.Join(rt, ownerMarker), []byte(run+"\n"), 0o600)
	return rt
}

// probeRig is a qualification probe on the offline rig: three wake tasks
// and the reviewer probe.
func probeRig(t *testing.T) *devRig {
	r := newDevRig(t, []devAct{{}, {}, {}, {marker: markerReviewApproved}})
	r.command = "probe-dev-full"
	r.owner = []string{"continue 1", "continue 2"}
	return r
}

// qualification reads the probe's record.
func (r *devRig) qualification() DevQualification {
	r.t.Helper()
	var q DevQualification
	if err := json.Unmarshal(r.evidence(devQualificationFile), &q); err != nil {
		r.t.Fatal(err)
	}
	return q
}

func TestDevFullVerifyFP8(t *testing.T) {
	t.Parallel()
	t.Run("mcp_calls", func(t *testing.T) {
		t.Parallel()
		// The qualification's own namespace: its recorded calls are the
		// valid probe control; a full-flow task's answer substituted into
		// a probe phase resolves in no probe record.
		if codes := qualificationCodes(t, devProbeBundle(t)); len(codes) != 0 {
			t.Fatalf("the probe control: %v", codes)
		}
		full := devPassingBundle(t)
		c := firstCall(t, full, toolTaskShow, nil)
		dir := devProbeBundle(t)
		lines := phaseStream(t, dir, 1)
		insertAt(t, dir, 1, -1, useLine(sessionOf(lines), "toolu_xns", toolTaskShow, c.args), resultLine(sessionOf(lines), "toolu_xns", c.answer, false))
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, devMCPCallInvalid) {
			t.Fatalf("cross-namespace substitution: %v", codes)
		}
	})
	t.Run("qualified", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.expect(0, "")
		q := r.qualification()
		if q.Result != ResultQualified || !qualifiedStages(q.Stages) || q.WorkerCalls != 9 || q.Phases != 3 || len(q.Prompts) != 6 ||
			len(q.Fixture) != 4 || q.Schema != devKind(DevQualificationKind) {
			t.Fatalf("qualification %+v", q)
		}
		if out := r.f.stdout.String(); !strings.Contains(out, "qualification probe: QUALIFIED") || strings.Contains(out, "full development flow: PASS") {
			t.Fatalf("stdout %s", out)
		}
		for _, n := range []string{"probe/wake/01/task.json", "probe/wake/03/wait.json", "probe/reviewer/01/review-evidence.json", "probe/decisions/02.json",
			"setup/probe-system-source.md", "setup/probe-phase-3.md", "setup/probe-fixture/main.go", "workspace/repo.git/refs/heads/review"} {
			r.evidence(n)
		}
		for _, n := range []string{devManifestFile, "tasks", "report.json"} {
			if _, err := os.Stat(filepath.Join(r.f.evidence, n)); err == nil {
				t.Fatalf("a qualification wrote full-flow %s", n)
			}
		}
		// The probe's launches resume one session; check refuses the record.
		specs := r.f.launcher.phaseSpecs()
		if len(specs) != 3 || slices.Contains(specs[0].Args, "--resume") || !slices.Contains(specs[2].Args, testSession) {
			t.Fatalf("phases %d", len(specs))
		}
		if code, _, errOut := devCheck(t, r.f.bundle()); code != 2 || !strings.Contains(errOut, "qualification record") {
			t.Fatalf("check of a qualification %d %s", code, errOut)
		}
	})
	// Design 12a-dev-flow-verify r0.8: the probe qualifies on the system
	// task_notification wake (print_notification included), and the
	// offline qualification check re-derives it.
	t.Run("system_wake", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.wakeForm = "system"
		r.expect(0, "")
		if q := r.qualification(); q.Result != ResultQualified || !qualifiedStages(q.Stages) {
			t.Fatalf("qualification %+v", q.Stages)
		}
		if codes := qualificationCodes(t, r.f.bundle()); len(codes) != 0 {
			t.Fatalf("offline %v", codes)
		}
		b, err := LoadDevQualification(r.f.bundle())
		if err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= 3; n++ {
			recs, _ := probeRecords(b, n)
			v := doc[contract.TaskShowResponse](b, "probe/wake/"+hopDir(n-1)+"/task.json")
			if v == nil {
				t.Fatalf("wake %d task", n)
			}
			w, src, p := deriveDevWait(recs, devWaitCommand(r.d.s.w.Executable, r.d.s.runtime, v.Task.TaskID, n), v.Task.TaskID)
			line := strings.Split(string(r.evidence(src.wake.File)), "\n")[src.wake.FirstLine-1]
			if p != "" || !w.complete() || !strings.Contains(line, `"subtype":"task_notification"`) {
				t.Fatalf("wake %d %+v %s %s", n, w, p, line)
			}
		}
	})
	// r0.9: the probe qualifies with the captured timeout start; a stopped
	// or invalid timeout start blocks it.
	t.Run("timeout_wake", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.wakeForm, r.waitTimeout = "system", "900000"
		r.expect(0, "")
		if q := r.qualification(); q.Result != ResultQualified || !qualifiedStages(q.Stages) {
			t.Fatalf("qualification %+v", q.Stages)
		}
		if codes := qualificationCodes(t, r.f.bundle()); len(codes) != 0 {
			t.Fatalf("offline %v", codes)
		}
	})
	for name, c := range map[string]struct{ timeout, alter, stop string }{
		"timeout_invalid":        {"1.0", "", devWaitMismatch},
		"timeout_stopped_xml":    {"900000", "early-stopped", devWakeUnsupported},
		"timeout_stopped_system": {"900000", "early-stopped", devWakeUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := probeRig(t)
			r.steps[0].alter = c.alter
			r.waitTimeout = c.timeout
			if strings.HasSuffix(name, "_system") {
				r.wakeForm = "system"
			}
			r.expect(1, c.stop)
			if q := r.qualification(); q.Result != ResultBlocked || q.Stages[1].Result == stagePass {
				t.Fatalf("qualified %+v", q.Stages)
			}
		})
	}
	for _, form := range []string{"", "system"} {
		for _, alter := range []string{"bad-wake", "early-fail"} {
			t.Run("refused_"+cmpOr(form, "xml")+"_"+alter, func(t *testing.T) {
				t.Parallel()
				r := probeRig(t)
				r.steps[0].alter = alter
				r.wakeForm = form
				r.expect(1, devWakeUnsupported)
				checkRefusedWake(t, r)
				q := r.qualification()
				if q.Result != ResultBlocked || q.Stages[1].Result == stagePass || q.Stages[2].Result == stagePass {
					t.Fatalf("a refused wake qualified %+v", q.Stages)
				}
				// The offline check refuses to qualify it.
				if codes := qualificationCodes(t, r.f.bundle()); !slices.Contains(codes, "not_qualified") {
					t.Fatalf("the blocked record rechecked as %v", codes)
				}
			})
		}
	}
	t.Run("missing_notification", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{}, {alter: "no-wake"}, {}, {marker: markerReviewApproved}})
		r.command = "probe-dev-full"
		r.owner = []string{"continue 1", "continue 2"}
		r.expect(1, devWaitFailed)
		q := r.qualification()
		if q.Result != ResultBlocked || len(r.workflowTasks()) != 2 || q.Stages[1].Result == stagePass {
			t.Fatalf("blocked %+v tasks %d", q.Stages, len(r.workflowTasks()))
		}
	})
	t.Run("failed_stage_stops", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.f.plane.preflight = func(i int) (string, *string, bool, bool) {
			if i == 2 {
				return contract.TaskFailed, ptr("login required"), false, true
			}
			return contract.TaskSucceeded, ptr(AuthToken + "\n"), false, true
		}
		r.expect(1, "preflight_failed")
		if q := r.qualification(); q.Result != ResultBlocked || len(r.workflowTasks()) != 0 || q.Stages[0].Result == stagePass {
			t.Fatalf("a failed models stage ran later stages: %+v", q.Stages)
		}
	})
	t.Run("owner_stops", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		r.owner = []string{"no 1"}
		r.expect(1, "probe_declined")
		if len(r.f.launcher.phaseSpecs()) != 1 || len(r.workflowTasks()) != 1 {
			t.Fatal("a declined probe continued")
		}
	})
	t.Run("refuses_ci_first", func(t *testing.T) {
		t.Parallel()
		var out, errb bytes.Buffer
		h := Host{GOOS: "linux", Args: []string{"probe-dev-full", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
			Getenv: func(k string) string { return map[string]string{EnvOptIn: "1"}[k] }, LookupEnv: func(k string) (string, bool) { return "", k == EnvCI },
			Stdout: &out, Stderr: &errb, NewWorld: func(Host) (*World, func(), error) { t.Error("the probe built a world in CI"); return nil, nil, nil }}
		if code := MainWith(h); code != 2 || !strings.Contains(errb.String(), ErrCI.Error()) || !strings.Contains(errb.String(), "qualification probe") {
			t.Fatalf("probe in CI %d %s", code, errb.String())
		}
	})
}
