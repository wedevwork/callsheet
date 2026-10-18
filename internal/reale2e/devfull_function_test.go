package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The eight development function tests (design 12a-dev-flow-harness,
// Function tests), one per FP. They drive MainWith through the offline rig
// and inspect the dispatched requests and the execution evidence. None is
// a live pass.

func happy() []devAct {
	return []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved}}
}

func ownerNoSteps() []devAct {
	return []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}}
}

// published returns a rig-published file of the task at ordinal n.
func (r *devRig) publishedFile(n int, path string) []byte {
	r.t.Helper()
	for _, t := range r.d.state.Tasks {
		if t.Ordinal == n {
			return r.published[t.Commit][path]
		}
	}
	r.t.Fatalf("no task %d", n)
	return nil
}

func TestDevFullFP1(t *testing.T) {
	r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignChanges}, {marker: markerDesignDraft}, {marker: markerDesignApproved}})
	r.owner = []string{"no 2"}
	r.expect(1, "owner_no")
	if len(r.goals) != 4 {
		t.Fatalf("goals %d", len(r.goals))
	}
	first, second := r.goals[1], r.goals[3]
	if !strings.Contains(first, "audit_round: first audit; use the complete checklist") || strings.Contains(first, "prior_audit:") {
		t.Fatalf("first audit envelope:\n%s", first)
	}
	audit := r.d.state.Tasks[1]
	priorReport := sha256Hex(r.publishedFile(2, "design-review.md"))
	priorDesign := sha256Hex(r.publishedFile(1, "design.md"))
	newDesign := sha256Hex(r.publishedFile(3, "design.md"))
	oldHistory, newHistory := sha256Hex(r.publishedFile(1, "revision-history.md")), sha256Hex(r.publishedFile(3, "revision-history.md"))
	for _, want := range []string{
		"audit_round: delta audit after audit task " + audit.TaskID + " (round 1); a new worker session does not make this round one",
		"prior_audit: task " + audit.TaskID + " commit " + audit.Commit + " design-review.md sha256 " + priorReport + " verdict " + markerDesignChanges,
		"prior_design: design.md sha256 " + priorDesign + " at commit " + r.d.state.Tasks[0].Commit,
		"dispositions: the designer's dispositions are in revision-history.md at " + r.d.state.Tasks[2].Commit + " (sha256 " + newHistory + ")",
		"- design.md " + priorDesign + " -> " + newDesign + " (changed)",
		"- revision-history.md " + oldHistory + " -> " + newHistory + " (changed)",
		"design_sha256: " + newDesign,
		"tags: each active finding is persisting, regression or new-on-unchanged",
		"- prior-audit: task " + audit.TaskID + " ordinal 2 commit " + audit.Commit + " design-review.md sha256 " + priorReport,
		"round: 1",
	} {
		if !strings.Contains(second, want) {
			t.Fatalf("delta envelope lacks %q:\n%s", want, second)
		}
	}
	// The designer's revision named the prior audit too.
	if !strings.Contains(r.goals[2], "prior_audit: task "+audit.TaskID) || !strings.Contains(r.goals[2], "round: 1") {
		t.Fatalf("revision envelope:\n%s", r.goals[2])
	}
}

func TestDevFullFP8(t *testing.T) {
	r := newDevRig(t, ownerNoSteps())
	r.owner = []string{"no 1"}
	before := map[string]string{}
	for _, name := range DevMethodNames() {
		rel := filepath.ToSlash(filepath.Join(DevFlowDir, name))
		if name == FlowFile || name == DevPromptFile || name == DevPhaseFile || name == DevScenario {
			rel = filepath.ToSlash(filepath.Join(DevFullDir, name))
		}
		before[rel] = sha256Hex(mustRead(t, filepath.Join(r.f.checkout, filepath.FromSlash(rel))))
	}
	play := filepath.Join(r.f.checkout, filepath.FromSlash(DevFlowDir), DevPlaybook)
	coderRunbook := filepath.Join(r.f.checkout, filepath.FromSlash(DevFlowDir), manualName("coder", "runbook"))
	var registered map[string][]byte
	r.onTick = func(r *devRig) {
		if registered != nil {
			return
		}
		// The checkout changes once the method was captured.
		os.WriteFile(play, []byte("changed after load\n"), 0o644)
		os.WriteFile(coderRunbook, []byte("changed after load\n"), 0o644)
		registered = map[string][]byte{}
		for _, role := range r.f.plane.roles {
			registered[role.ID+"/i"] = mustRead(t, role.Instruction)
			registered[role.ID+"/r"] = mustRead(t, role.Runbook)
		}
	}
	r.expect(1, "owner_no")
	var sources []SourceRecord
	if err := json.Unmarshal(r.evidence("setup/sources.json"), &sources); err != nil || len(sources) != len(before) {
		t.Fatalf("sources %v %d", err, len(sources))
	}
	for _, s := range sources {
		if before[s.Path] != s.SHA256 {
			t.Fatalf("source %s", s.Path)
		}
		// The captured bytes themselves are retained, not only their hash
		// (the runtime copies are removed by cleanup).
		kept := r.evidence("setup/sources/" + s.Path)
		if sha256Hex(kept) != s.SHA256 || len(kept) != s.Bytes {
			t.Fatalf("retained source %s does not match its record", s.Path)
		}
	}
	for _, role := range DevFullRoleIDs {
		ins, rb, _ := r.d.loaded.ManualPair(role)
		for kind, b := range map[string][]byte{"instruction": ins, "runbook": rb} {
			if !bytes.Equal(r.evidence("setup/sources/"+DevFlowDir+"/"+manualName(role, kind)), b) {
				t.Fatalf("the captured %s %s is not retained", role, kind)
			}
		}
	}
	var rendered []RenderedRecord
	if err := json.Unmarshal(r.evidence("setup/rendered.json"), &rendered); err != nil {
		t.Fatal(err)
	}
	system := r.evidence("setup/coordinator-system.md")
	var sys *RenderedRecord
	for i := range rendered {
		if rendered[i].Name == DevSystemName {
			sys = &rendered[i]
		}
	}
	if sys == nil || sys.SHA256 != sha256Hex(system) || !slices.Equal(sys.Sources, []string{DevFlowDir + "/" + DevPlaybook, DevFullDir + "/" + DevPromptFile}) {
		t.Fatalf("system record %+v", sys)
	}
	if !bytes.HasPrefix(system, r.d.loaded.Files[DevPlaybook]) || sha256Hex(r.d.loaded.Files[DevPlaybook]) != before[DevFlowDir+"/"+DevPlaybook] {
		t.Fatal("the coordinator system input is not the literal captured playbook")
	}
	// Registration names the private copies: their bytes are the capture,
	// not the changed checkout, and the phase launch carried them.
	for _, role := range DevFullRoleIDs {
		ins, rb, _ := r.d.loaded.ManualPair(role)
		if !bytes.Equal(registered[role+"/i"], ins) || !bytes.Equal(registered[role+"/r"], rb) {
			t.Fatalf("registered manuals of %s are not the capture", role)
		}
		if sha256Hex(rb) != before[DevFlowDir+"/"+manualName(role, "runbook")] {
			t.Fatalf("the captured %s runbook followed the checkout", role)
		}
	}
	var launch LaunchRecord
	if err := json.Unmarshal(r.evidence("setup/launch-phase-1.json"), &launch); err != nil || launch.SystemSHA256 != sys.SHA256 {
		t.Fatalf("launch %v %+v", err, launch)
	}
	// The generated FEATURE.md: only the size sentence differs.
	feature := string(r.d.s.seed.Files["FEATURE.md"])
	want, _ := devFullFeatureRequest()
	if feature != want || strings.Contains(feature, legacySizeSentence) {
		t.Fatal("the seed's FEATURE.md is not the derived request")
	}
	var fr map[string]string
	if err := json.Unmarshal(r.evidence("setup/feature-request.json"), &fr); err != nil || fr["original_sha256"] != sha256Hex([]byte(FeatureRequest)) ||
		fr["generated_sha256"] != sha256Hex([]byte(feature)) {
		t.Fatalf("feature provenance %v %v", err, fr)
	}
	// The scenario is in every envelope; no role method is copied into it.
	scenario := string(r.d.loaded.Rendered[DevScenario])
	for _, g := range r.goals {
		if !strings.Contains(g, scenario) {
			t.Fatal("an envelope lacks the scenario")
		}
		ins, rb, _ := r.d.loaded.ManualPair("designer")
		if strings.Contains(g, string(rb)) || strings.Contains(g, string(ins)) {
			t.Fatal("an envelope copies a role method")
		}
	}
}

func TestDevFullFP2(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{"no 1"}
		r.expect(1, "owner_no")
		if len(r.d.s.preflight) != 5 {
			t.Fatalf("preflights %d", len(r.d.s.preflight))
		}
		var label map[string]string
		if err := json.Unmarshal(r.evidence("setup/flow-label.json"), &label); err != nil || label["label"] != "default-flow" {
			t.Fatalf("label %s", r.evidence("setup/flow-label.json"))
		}
		roles, _ := r.f.plane.ListRoles(r.f.ctx)
		want := DefaultDevFullFlow()
		if len(roles) != 5 {
			t.Fatal(len(roles))
		}
		for i, role := range roles {
			if role.ID != want.Roles[i].ID || role.Model != want.Roles[i].Model || role.Effort != want.Roles[i].Effort || role.Adapter != want.Roles[i].Adapter ||
				role.Concurrency != 1 || role.Timeout != FeatureTaskBound {
				t.Fatalf("role %d %+v", i, role.RoleConfig)
			}
		}
		// All five probes precede the first workflow admission.
		ids := r.f.plane.ids()
		if !slices.Equal(ids[:5], r.d.s.preflight) {
			t.Fatalf("the probes do not precede the workflow: %v", ids)
		}
		for i := range 5 {
			var obs DevPreflightRecord
			if err := json.Unmarshal(r.evidence("preflight/"+hopDir(i)+"/observation.json"), &obs); err != nil || obs.Role != want.Roles[i].ID ||
				obs.Model != want.Roles[i].Model || !obs.Answered {
				t.Fatalf("preflight %d %+v %v", i, obs, err)
			}
		}
	})
	for n := range 5 {
		t.Run("probe_"+itoa(n+1), func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, ownerNoSteps())
			r.f.plane.preflight = func(i int) (string, *string, bool, bool) {
				if i == n {
					return contract.TaskFailed, ptr("login required"), false, true
				}
				return contract.TaskSucceeded, ptr(AuthToken + "\n"), false, true
			}
			r.expect(1, "preflight_failed")
			if len(r.f.plane.ids()) != n+1 || len(r.workflowTasks()) != 0 {
				t.Fatalf("workflow admitted after a failed probe: %v", r.f.plane.ids())
			}
		})
	}
	t.Run("unready", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.f.plane.notReadyCalls = 1 << 20
		r.expect(1, "roles_not_ready")
		if len(r.f.plane.ids()) != 0 {
			t.Fatal("a task ran with unready roles")
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.f.plane.effectiveModel = "gpt-6.1-sol"
		r.expect(1, "preflight_pair_mismatch")
		if len(r.workflowTasks()) != 0 {
			t.Fatal("workflow after a mismatched pair")
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{"no 1"}
		flow := DefaultDevFullFlow()
		flow.Roles[0].Effort = "medium"
		body, _ := json.Marshal(flow)
		path := filepath.Join(t.TempDir(), "flow.json")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		r.f.flow = path
		r.expect(1, "owner_no")
		var label map[string]string
		if err := json.Unmarshal(r.evidence("setup/flow-label.json"), &label); err != nil || label["label"] != "override-flow" ||
			!strings.Contains(label["selection"], `"effort":"medium"`) || label["flow_sha256"] != sha256Hex(body) {
			t.Fatalf("label %s", r.evidence("setup/flow-label.json"))
		}
		if r.d.flow.Roles[0].Effort != "medium" || r.execution().FlowLabel != "override-flow" {
			t.Fatalf("%+v", r.d.flow.Roles[0])
		}
	})
}

func TestDevFullFP3(t *testing.T) {
	t.Run("recurring", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignChanges}, {marker: markerDesignDraft}, {marker: markerDesignApproved}})
		r.owner = []string{"no 2"}
		r.expect(1, "owner_no")
		tasks := r.d.state.Tasks
		if len(tasks) != 4 || tasks[0].Role != "designer" || tasks[2].Role != "designer" || tasks[0].TaskID == tasks[2].TaskID ||
			tasks[1].Role != "design-reviewer" || tasks[3].Role != "design-reviewer" {
			t.Fatalf("%+v", tasks)
		}
		for i, task := range tasks {
			var w DevWaitRecord
			if err := json.Unmarshal(r.evidence("tasks/"+hopDir(i)+"/wait.json"), &w); err != nil || w.Ordinal != i+1 || w.Task != task.TaskID ||
				!w.Wait.complete() || w.ConsumedSeq <= w.StartSeq || *w.ExitCode != 0 {
				t.Fatalf("wait %d %+v %v", i+1, w, err)
			}
			if task.Ordinal != i+1 || task.Selection.ID != task.Role || task.Instance != r.d.s.workspace.Instance || task.Base == "" {
				t.Fatalf("record %+v", task)
			}
		}
		if slices.Compare(r.workflowTasks(), []string{tasks[0].TaskID, tasks[1].TaskID, tasks[2].TaskID, tasks[3].TaskID}) != 0 {
			t.Fatal("a hidden extra task")
		}
	})
	t.Run("lost_reply", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.fault = "lost"
		r.owner = []string{"no 1"}
		r.expect(1, "owner_no")
		if r.taskCount() != 2 || len(r.workflowTasks()) != 2 {
			t.Fatalf("lost reply recovery admitted %d tasks", r.taskCount())
		}
	})
	// The coordinator asks for the successor as soon as it consumed the
	// wake (or even before): dev-next never names the occupied slot again,
	// so following the dispatch instructions never redispatches it.
	for _, eager := range []string{"after-show", "before-show"} {
		t.Run("successor_"+strings.ReplaceAll(eager, "-", "_"), func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, happy())
			r.owner = []string{"yes 1"}
			r.eager = eager
			r.expect(3, "")
			if len(r.workflowTasks()) != 4 || r.taskCount() != 4 || len(r.docs) != 4 {
				t.Fatalf("tasks %d admitted %d documents %d", len(r.workflowTasks()), r.taskCount(), len(r.docs))
			}
			for i, doc := range r.docs {
				if doc.Ordinal != i+1 {
					t.Fatalf("document %d names ordinal %d", i+1, doc.Ordinal)
				}
			}
			counts := map[string]int{}
			for _, c := range r.codes {
				counts[c]++
			}
			// Each of the four early requests answers pending: before the
			// wake's consumption, or after it while the publication's owned
			// inspection still runs (its admission never runs inside a
			// dev-next reply).
			if counts[CodePrepared] != 4 || counts[CodePaused] != 1 || counts[CodeComplete] != 1 || counts[CodePending] != 4 {
				t.Fatalf("codes %v", r.codes)
			}
		})
	}
	for fault, stop := range map[string]string{"duplicate": "duplicate_admission", "extra": "extra_admission", "unobserved": "observation_missing"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, []devAct{{marker: markerDesignDraft}})
			r.fault = fault
			r.expect(1, stop)
			if r.taskCount() != 0 {
				t.Fatalf("admitted %d", r.taskCount())
			}
			c := r.cleanupRecord()
			for _, id := range r.workflowTasks() {
				if !contract.TaskTerminal(r.terminalState(id)) {
					t.Fatalf("task %s left %s", id, r.terminalState(id))
				}
			}
			if !c.Complete || len(c.UnresolvedTasks) != 0 || r.execution().Outcome != outcomeStopped {
				t.Fatalf("cleanup %+v", c)
			}
			if fault != "unobserved" && len(c.CancelledTasks) == 0 {
				t.Fatal("the rejected admission was not cancelled")
			}
		})
	}
}

func TestDevFullFP4(t *testing.T) {
	t.Run("both_loops", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignChanges}, {marker: markerDesignDraft}, {marker: markerDesignApproved},
			{marker: markerImplComplete}, {marker: markerReviewChanges}, {marker: markerFixesComplete}, {marker: markerReviewApproved},
		})
		r.owner = []string{"yes 2"}
		r.expect(3, "")
		st := r.d.state
		rounds := []int{0, 0, 1, 1, 0, 0, 1, 1}
		for i, task := range st.Tasks {
			if task.Round != rounds[i] {
				t.Fatalf("task %d round %d want %d", i+1, task.Round, rounds[i])
			}
		}
		for i, doc := range r.docs {
			if !strings.Contains(doc.Goal, "\nround: "+itoa(rounds[i])+"\n") {
				t.Fatalf("plan %d header round", i+1)
			}
		}
		ex := r.execution()
		if ex.FinalCandidate != st.Tasks[6].Commit || ex.FinalReview != st.Tasks[7].Commit || ex.FinalCandidate == ex.FinalReview ||
			ex.CandidateTask != st.Tasks[6].TaskID || ex.Outcome != outcomeUnverified || st.DesignCycles != 1 || st.CodeCycles != 1 {
			t.Fatalf("execution %+v", ex)
		}
		if out := r.f.stdout.String(); !strings.Contains(out, devUnverifiedLine) || strings.Contains(out, "PASS") && !strings.Contains(out, "never reports PASS") {
			t.Fatalf("stdout %s", out)
		}
		if len(r.workflowTasks()) != 8 {
			t.Fatal("a hidden retry")
		}
	})
	t.Run("design_cap", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignChanges}, {marker: markerDesignDraft}, {marker: markerDesignChanges},
			{marker: markerDesignDraft}, {marker: markerDesignChanges},
		})
		r.expect(1, stopCap)
		last := r.d.state.Transitions[len(r.d.state.Transitions)-1]
		if r.d.state.DesignCycles != 2 || last.Reason != "designer/revise changes_requested" || len(r.workflowTasks()) != 6 {
			t.Fatalf("design cap %+v", last)
		}
	})
	t.Run("code_cap", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerFixesComplete}, {marker: markerReviewChanges}, {marker: markerFixesComplete}, {marker: markerReviewChanges},
		})
		r.owner = []string{"yes 1"}
		r.expect(1, stopCap)
		if r.d.state.CodeCycles != 2 || len(r.workflowTasks()) != 8 {
			t.Fatal(r.d.state.CodeCycles)
		}
	})
	t.Run("recovery_cap", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerNeedsTriage},
			{marker: markerTriageConf, designRequired: designRequiredYes},
		})
		r.owner = []string{"yes 1", "yes 2"}
		r.expect(1, stopCap)
		if r.d.state.Recovery != 2 || r.taskCount() != 9 {
			t.Fatalf("recovery %d tasks %d", r.d.state.Recovery, r.taskCount())
		}
	})
	t.Run("questions", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved},
		})
		r.owner = []string{"yes 1", "yes 2"}
		r.expect(3, "")
		if !strings.Contains(r.goals[3], "design_questions: resolve every question") || !strings.Contains(r.goals[3], "partial_work: commit "+r.d.state.Tasks[2].Commit) {
			t.Fatalf("design repair envelope:\n%s", r.goals[3])
		}
	})
	t.Run("triage_fix", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerNeedsTriage}, {marker: markerTriageProb, designRequired: designRequiredNo}, {marker: markerFixesComplete},
			{marker: markerReviewApproved},
		})
		r.owner = []string{"yes 1"}
		r.expect(3, "")
		tasks := r.d.state.Tasks
		if tasks[5].Role != "triage" || tasks[6].Mode != modeFix || r.d.state.Recovery != 1 {
			t.Fatalf("%+v", tasks)
		}
		if !strings.Contains(r.goals[5], "Finding C1 needs investigation.") || !strings.Contains(r.goals[6], "brief: the current qualified fix.md of triage task "+tasks[5].TaskID) {
			t.Fatal("the triage and fix envelopes do not carry their inputs")
		}
	})
	t.Run("brief_confidence", func(t *testing.T) {
		t.Parallel()
		// A TRIAGE_CONFIRMED publication whose current brief states another
		// confidence is not a qualified brief.
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerNeedsTriage}, {marker: markerTriageConf, designRequired: designRequiredNo, alter: "wrong-confidence"},
		})
		r.owner = []string{"yes 1"}
		r.expect(1, "publication_rejected")
		if !strings.Contains(r.d.stopReason, "Confidence") || r.taskCount() != 5 || len(r.workflowTasks()) != 6 {
			t.Fatalf("stop %q tasks %d", r.d.stopReason, r.taskCount())
		}
	})
	t.Run("implement_needs_triage", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerNeedsTriage}})
		r.owner = []string{"yes 1"}
		r.expect(1, "invalid_marker")
		if r.taskCount() != 2 {
			t.Fatal("implement-mode NEEDS_TRIAGE was admitted")
		}
	})
	t.Run("unsupported_cause", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerNeedsTriage}, {marker: markerTriageUnconf},
		})
		r.owner = []string{"yes 1"}
		r.expect(1, "cause_unconfirmed")
		if r.taskCount() != 6 || len(r.workflowTasks()) != 6 {
			t.Fatal("work continued after an unsupported cause")
		}
	})
	t.Run("escalation", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerNeedsDesign},
			{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved},
		})
		r.owner = []string{"yes 1", "yes 2"}
		r.expect(3, "")
		st := r.d.state
		if st.Tasks[2].Mode != modeImplement || st.Tasks[5].Mode != modeImplement || st.Tasks[3].Reason != "needs_design" || len(st.Receipts) != 2 ||
			st.Receipts[0].Valid || !st.Receipts[1].Valid || st.Recovery != 1 || st.CodeCycles != 1 || st.DesignCycles != 1 {
			t.Fatalf("escalation %+v", st)
		}
		if !strings.Contains(r.goals[3], "The design cannot be satisfied") {
			t.Fatal("the designer did not receive the NEEDS_DESIGN reason")
		}
	})
	t.Run("stale_receipt", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions}, {marker: markerDesignDraft}})
		r.owner = []string{"yes 1"}
		var coder devDispatchDoc
		r.onTick = func(r *devRig) {
			for _, doc := range r.docs {
				if doc.Role == "coder" {
					coder = doc
				}
			}
			// With the designer's re-entry plan pending, replay the old
			// coder plan's exact request.
			if r.d.slot != nil && r.d.slot.plan.Ordinal == 4 && coder.Goal != "" && r.fault == "" {
				r.fault = "replayed"
				ws, inst, base := coder.Workspace, coder.Instance, coder.Base
				r.mustDispatch(contract.DispatchRequest{Target: coder.Target, Goal: coder.Goal, Acceptance: coder.Acceptance, Payload: []string{},
					RequestedBy: requester, Workspace: &ws, Base: &base, WorkspaceInstance: &inst})
			}
		}
		r.expect(1, "coding_before_approval")
		if r.d.state.Receipt == nil || r.d.state.Receipt.Valid || len(r.d.rejected) != 1 || r.terminalState(r.d.rejected[0]) != contract.TaskCancelled {
			t.Fatalf("stale receipt %+v rejected %v", r.d.state.Receipt, r.d.rejected)
		}
	})
}

func TestDevFullFP5(t *testing.T) {
	renewal := []devAct{
		{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
		{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved},
	}
	t.Run("renewed", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, renewal)
		r.owner = []string{"yes 1", "yes 2"}
		r.expect(3, "")
		st := r.d.state
		if len(st.Receipts) != 2 || st.Receipts[0].Valid || !st.Receipts[1].Valid || st.Receipts[0].Design == st.Receipts[1].Design {
			t.Fatalf("receipts %+v", st.Receipts)
		}
		for gen, rec := range map[int]OwnerReceipt{1: st.Receipts[0], 2: st.Receipts[1]} {
			var got OwnerReceipt
			if err := json.Unmarshal(r.evidence("receipts/"+hopDir(gen-1)+".json"), &got); err != nil || got.Generation != gen || !got.Valid ||
				got.AuditTask != rec.AuditTask {
				t.Fatalf("receipt %d %+v", gen, got)
			}
			if !(got.ApprovalSeq < got.ConsumedSeq && got.ConsumedSeq < got.PhaseExitSeq && got.PhaseExitSeq < got.PromptSeq &&
				got.PromptSeq < got.DecisionSeq && got.DecisionSeq < got.ReceiptSeq) {
				t.Fatalf("receipt %d order %+v", gen, got)
			}
			for _, name := range presentedFiles {
				if !bytes.Contains([]byte(r.f.stdout.String()), []byte(name+" (")) {
					t.Fatalf("%s was not presented", name)
				}
			}
		}
		coder := r.d.state.Tasks[5]
		if coder.AdmittedSeq <= st.Receipts[1].ReceiptSeq {
			t.Fatal("the coder was admitted before the renewed receipt")
		}
		if !strings.Contains(r.goals[5], "owner_receipt: receipts/02.json generation 2") {
			t.Fatalf("coder envelope:\n%s", r.goals[5])
		}
		// Every move has a record bound to its actual source: a task, or
		// the owner decision that chose it. Pauses and completion included.
		type move struct {
			ordinal  int
			decision int
			to       string
		}
		want := []move{{0, 0, "designer/draft"}, {1, 0, "design-reviewer/audit"}, {2, 0, "paused/owner"}, {0, 1, "coder/implement"},
			{3, 0, "designer/revise"}, {4, 0, "design-reviewer/audit"}, {5, 0, "paused/owner"}, {0, 2, "coder/implement"},
			{6, 0, "code-reviewer/full"}, {7, 0, "complete"}}
		if len(st.Transitions) != len(want) {
			t.Fatalf("transitions %+v", st.Transitions)
		}
		for i, tr := range st.Transitions {
			w := want[i]
			task := ""
			if w.ordinal > 0 {
				task = st.Tasks[w.ordinal-1].TaskID
			}
			if tr.FromOrdinal != w.ordinal || tr.FromTask != task || tr.FromDecision != w.decision || tr.ToState != w.to {
				t.Fatalf("transition %d %+v want %+v", i+1, tr, w)
			}
			var got TransitionRecord
			if err := json.Unmarshal(r.evidence("transitions/"+hopDir(i)+".json"), &got); err != nil || got != tr {
				t.Fatalf("transition %d evidence %+v %v", i+1, got, err)
			}
		}
		if _, err := os.Stat(filepath.Join(r.f.evidence, "transitions", hopDir(len(want))+".json")); err == nil {
			t.Fatal("an extra transition record")
		}
	})
	t.Run("pre_window_yes", func(t *testing.T) {
		t.Parallel()
		// "yes 1" typed before the attempt started (buffered on the
		// terminal) is outside every window: it approves nothing.
		r := newDevRig(t, happy())
		r.term.typeLine("yes 1")
		r.expect(1, "owner_expired")
		if r.d.ignoredLines != 1 || len(r.d.state.Receipts) != 0 || len(r.workflowTasks()) != 2 || r.d.decisions != 0 {
			t.Fatalf("pre-window input: ignored %d receipts %d tasks %d", r.d.ignoredLines, len(r.d.state.Receipts), len(r.workflowTasks()))
		}
	})
	t.Run("pre_window_reader", func(t *testing.T) {
		t.Parallel()
		// The production line reader consumed "yes 1" before the window
		// opened and is held before the line's LF until the window is open
		// (the line is then still in flight, never waiting on the channel
		// when the window opened): a line whose first byte was read before
		// its window decides nothing. The terminal then ends.
		r := newDevRig(t, happy())
		in := newHeldReader("yes 1\n")
		r.f.liveTerm = &readerTerminal{r: in}
		consumed, released := false, false
		r.onTick = func(r *devRig) {
			switch {
			case !consumed && r.d.window == nil:
				// The reader goroutine has read every byte before the LF.
				<-in.consumed
				consumed = true
			case consumed && !released && r.d.window != nil:
				released = true
				close(in.release)
				<-in.delivered
			}
		}
		r.expect(1, "owner_eof")
		if !released || r.d.ignoredLines != 1 || r.d.decisions != 0 || len(r.d.state.Receipts) != 0 || len(r.workflowTasks()) != 2 {
			t.Fatalf("in-flight pre-window input: released %t ignored %d decisions %d receipts %d tasks %d", released, r.d.ignoredLines,
				r.d.decisions, len(r.d.state.Receipts), len(r.workflowTasks()))
		}
	})
	t.Run("stale_receipt", func(t *testing.T) {
		t.Parallel()
		// An old-generation yes in the renewed window approves nothing and
		// does not extend it; the terminal then closes.
		r := newDevRig(t, renewal[:5])
		r.owner = []string{"yes 1", "yes 1"}
		r.onTick = func(r *devRig) {
			if r.d.window != nil && r.d.window.gen == 2 && r.answered[r.d.window] && len(r.d.state.Receipts) == 1 && r.fault == "" {
				r.fault = "closed"
				r.term.close()
			}
		}
		r.expect(1, "owner_eof")
		if r.taskCount() != 5 || len(r.d.state.Receipts) != 1 || r.d.state.Receipts[0].Valid {
			t.Fatalf("stale yes authorized coding: %+v", r.d.state.Receipts)
		}
	})
	t.Run("buffered_yes", func(t *testing.T) {
		t.Parallel()
		// "yes 2" typed after generation 1's decision, outside any window,
		// is discarded rather than approving the later design.
		r := newDevRig(t, renewal[:5])
		r.owner = []string{"yes 1", ""}
		r.early = []string{"yes 2"}
		r.expect(1, "owner_eof")
		if r.d.ignoredLines != 1 || len(r.d.state.Receipts) != 1 || r.taskCount() != 5 {
			t.Fatalf("buffered yes: ignored %d receipts %d", r.d.ignoredLines, len(r.d.state.Receipts))
		}
	})
	t.Run("no", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{"no 1"}
		r.expect(1, "owner_no")
		if len(r.workflowTasks()) != 2 || len(r.d.state.Receipts) != 0 {
			t.Fatal("a no dispatched further work")
		}
		var dec DevDecision
		if err := json.Unmarshal(r.evidence("decisions/01.json"), &dec); err != nil || dec.Kind != "no" || dec.Generation != 1 {
			t.Fatalf("decision %+v", dec)
		}
	})
	t.Run("eof", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{""}
		r.expect(1, "owner_eof")
		if len(r.workflowTasks()) != 2 {
			t.Fatal("EOF dispatched further work")
		}
	})
	t.Run("expired", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.onTick = func(r *devRig) {
			if r.d.window != nil && !r.answered[r.d.window] {
				r.answered[r.d.window] = true
				// The window's deadline passes before the answer arrives:
				// equality is already expired.
				r.f.clock.advance(r.d.window.deadline.Sub(r.f.clock.Now()))
				r.term.typeLine("yes 1")
			}
		}
		r.expect(1, "owner_expired")
		if len(r.workflowTasks()) != 2 || len(r.d.state.Receipts) != 0 {
			t.Fatal("an expired window approved")
		}
	})
	t.Run("clarification", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerNeedsInput}, {marker: markerDesignDraft}, {marker: markerDesignApproved}})
		r.owner = []string{"answer 0 call the flag --name", "no 1"}
		r.expect(1, "owner_no")
		if !strings.Contains(r.goals[1], "owner_answer (bounded owner data for generation 0):\n~~~\ncall the flag --name\n~~~") ||
			!strings.Contains(r.goals[1], "Which name should the flag use?") || r.d.state.Recovery != 1 || r.d.state.DesignCycles != 1 {
			t.Fatalf("clarified designer envelope:\n%s", r.goals[1])
		}
		var dec DevDecision
		if err := json.Unmarshal(r.evidence("decisions/01.json"), &dec); err != nil || dec.Kind != "answer" || dec.Answer != "call the flag --name" {
			t.Fatalf("answer record %+v", dec)
		}
	})
}

func TestDevFullFP6(t *testing.T) {
	ok := func(alter string) []devAct {
		return []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved, alter: alter}}
	}
	for _, alter := range []string{"", "before-export"} {
		t.Run("report_only"+map[string]string{"": "_after", "before-export": "_before_after"}[alter], func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, ok(alter))
			r.owner = []string{"yes 1"}
			r.expect(3, "")
			run := r.reviews[0]
			want := []string{"after"}
			if alter != "" {
				want = []string{"before", "after"}
			}
			if !slices.Equal(run.exports, want) || !run.scratchOK || run.exits["after/test"] != 0 || run.exits["after/cover"] != 0 {
				t.Fatalf("review run %+v", run)
			}
			review := r.d.state.Tasks[3]
			if len(review.Artifacts) != 8 || review.Disposition != dispositionReport {
				t.Fatalf("review record %+v", review)
			}
			for p, sum := range r.d.state.Tasks[2].Artifacts {
				if review.Artifacts[p] != sum {
					t.Fatalf("the review changed %s", p)
				}
			}
		})
	}
	for alter, why := range map[string]string{"edit-main": "not an allowed change", "scratch": "scratch remnant", "bad-report": "final marker",
		"no-commit": "published no result", "missing-field": "source hash", "blocker-mismatch": "review blocker"} {
		t.Run(alter, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, ok(alter))
			r.owner = []string{"yes 1"}
			r.expect(1, "publication_rejected")
			if !strings.Contains(r.d.stopReason, why) || r.taskCount() != 3 {
				t.Fatalf("stop reason %q", r.d.stopReason)
			}
		})
	}
	t.Run("audit_digest", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved, alter: "no-digest"}})
		r.expect(1, "publication_rejected")
	})
	t.Run("designer_questions", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
			{marker: markerDesignDraft, alter: "replace-questions"}})
		r.owner = []string{"yes 1"}
		r.expect(1, "publication_rejected")
		if !strings.Contains(r.d.stopReason, "may only empty") {
			t.Fatal(r.d.stopReason)
		}
	})
}

func TestDevFullFP7(t *testing.T) {
	t.Run("exit3", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.expect(3, "")
		out := r.f.stdout.String()
		if !strings.Contains(out, devUnverifiedLine) || !strings.Contains(out, "five paid authentication preflight") ||
			!strings.Contains(out, "at most 19 paid worker") || !strings.Contains(out, "up to\n    four phases") || strings.Contains(out, "no automatic retries or fix loops") {
			t.Fatalf("stdout %s", out)
		}
		if !strings.Contains(Usage, "exit 3") || !strings.Contains(Usage, "run-dev-full") {
			t.Fatal("usage")
		}
		ex := r.execution()
		if ex.Schema != DevExecutionSchema || ex.Outcome != outcomeUnverified || ex.Cleanup != "complete" || ex.Phases != 2 || ex.Tasks != 4 {
			t.Fatalf("%+v", ex)
		}
		for _, name := range []string{"tasks/01/plan.json", "tasks/01/dispatch.json", "tasks/01/task.json", "tasks/01/observation.json", "tasks/01/wait.json",
			"tasks/01/wait-result.json", "tasks/01/admission.json", "tasks/04/record.json", "transitions/01.json", "session/phase-1.stdout.jsonl",
			"session/phase-2.stdout.jsonl", "setup/launch-phase-2.json", "session/coordinator-exit.json", "workspace/tree-manifest.json",
			"workspace/repo.git/refs/heads/task-04", "workspace/repo.git/refs/heads/seed", "cleanup.json", "ledger.json", "receipts/01.json",
			"decisions/01.json", "review/01/design.md", "setup/seed-push.json", "environment/diff.json"} {
			r.evidence(name)
		}
		// The checker rejects the development evidence before any task.
		var o, e bytes.Buffer
		if code := CheckEvidence(r.f.bundle(), &o, &e); code != 2 || !strings.Contains(e.String(), "unsupported schema") {
			t.Fatalf("check %d %s", code, e.String())
		}
	})
	t.Run("legacy", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		if code := f.attempt(); code != 0 {
			t.Fatalf("legacy exit %d\n%s", code, f.stderr.String())
		}
	})
	t.Run("task_timeout", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft, alter: "timeout"}})
		r.expect(1, "task_timeout")
		if id := r.workflowTasks()[0]; r.terminalState(id) != contract.TaskCancelled || !slices.Contains(r.cleanupRecord().CancelledTasks, id) {
			t.Fatal("the overdue task was not cancelled")
		}
	})
	t.Run("hang", func(t *testing.T) {
		t.Parallel()
		// A running task reaches its deadline on the injected clock; there
		// is no poll ceiling.
		r := newDevRig(t, []devAct{{marker: markerDesignDraft, alter: "hang"}})
		r.expect(1, "task_timeout")
	})
	t.Run("slow_coordinator", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.delay = 450
		r.expect(3, "")
	})
	t.Run("attempt_timeout", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}})
		r.fault = "attempt"
		r.expect(1, "attempt_timeout")
		if r.taskCount() != 0 {
			t.Fatal(r.taskCount())
		}
	})
	t.Run("owner_timeout", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, ownerNoSteps())
		r.onTick = func(r *devRig) {
			if r.d.window != nil {
				r.answered[r.d.window] = true
			}
		}
		r.expect(1, "owner_expired")
	})
	for alter, stop := range map[string]string{"no-wake": devWaitFailed, "foreground": devWaitMismatch, "double-wait": devDuplicateWait,
		"deny": devToolDenied, "wait-exit": devWaitFailed} {
		t.Run("wait_"+alter, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, []devAct{{marker: markerDesignDraft, alter: alter}})
			r.expect(1, stop)
			if r.taskCount() != 0 {
				t.Fatal("a task was admitted without its qualified wait")
			}
		})
	}
	t.Run("late_wake", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft, alter: "late-wake"}, {marker: markerDesignApproved}})
		r.owner = []string{"no 1"}
		r.expect(1, "owner_no")
	})
	t.Run("no_seed_push", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.fault = "no-push"
		r.expect(1, "coordinator_exited")
		if len(r.codes) != 1 || r.codes[0] != CodeSeedUnpublished || len(r.workflowTasks()) != 0 {
			t.Fatalf("codes %v", r.codes)
		}
		if _, err := os.Stat(filepath.Join(r.f.evidence, "setup", "seed-push.json")); err == nil {
			t.Fatal("an unpushed seed was recorded as pushed")
		}
	})
	t.Run("evidence_failure", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		var planted bool
		r.onTick = func(r *devRig) {
			if !planted && r.d.state.Pending != nil && r.d.state.Pending.Ordinal == 1 && r.d.slot != nil {
				planted = true
				// The next plan's evidence path is taken: its write fails.
				os.MkdirAll(filepath.Join(r.f.evidence, "tasks", "02"), 0o700)
				os.WriteFile(filepath.Join(r.f.evidence, "tasks", "02", "plan.json"), []byte("{}"), 0o600)
			}
		}
		r.expect(1, "evidence_write_failed")
		if r.taskCount() != 1 || len(r.workflowTasks()) != 1 || r.execution().Outcome != outcomeFailed {
			t.Fatalf("dispatch continued after an evidence failure: %d", len(r.workflowTasks()))
		}
	})
	t.Run("publication_deadline", func(t *testing.T) {
		t.Parallel()
		// The task's 10m bound passes while its publication is pulled: the
		// result is neither admitted nor followed by a successor.
		r := newDevRig(t, ownerNoSteps())
		r.owner = []string{"no 1"}
		pulled := false
		r.pullHook = func(r *devRig) {
			if !pulled {
				pulled = true
				r.f.clock.advance(FeatureTaskBound)
			}
		}
		r.expect(1, "task_timeout")
		if r.taskCount() != 0 || len(r.workflowTasks()) != 1 || len(r.docs) != 1 {
			t.Fatalf("an overdue publication was admitted: tasks %d", r.taskCount())
		}
	})
	t.Run("event_failure", func(t *testing.T) {
		t.Parallel()
		// An event write fails (the events file is closed under the
		// writer's lock) while later file writes still succeed: dispatch
		// closes at once.
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		closed := false
		r.onTick = func(r *devRig) {
			if !closed && r.d.slot != nil && r.d.slot.observed {
				closed = true
				ev := r.d.s.ev
				ev.mu.Lock()
				ev.events.Close()
				ev.mu.Unlock()
			}
		}
		r.expect(1, "evidence_write_failed")
		if r.taskCount() != 0 || len(r.workflowTasks()) != 1 || len(r.docs) != 1 || r.execution().Outcome != outcomeFailed {
			t.Fatalf("dispatch continued after an event failure: tasks %d workflow %d documents %d", r.taskCount(), len(r.workflowTasks()), len(r.docs))
		}
	})
	t.Run("exposure_event_failure", func(t *testing.T) {
		t.Parallel()
		// The second plan's plan_exposed event is the write that fails: the
		// events file is closed (under the writer's lock) just before the
		// dev-next that would expose it, with no failure sticky yet. The
		// plan is never exposed and dev-next answers closing.
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		closed := false
		r.onTick = func(r *devRig) {
			if sl := r.d.slot; !closed && r.pending == nil && sl != nil && sl.plan.Ordinal == 2 && !sl.exposed {
				closed = true
				if err := r.d.s.ev.failure(); err != nil {
					t.Errorf("a failure is already sticky: %v", err)
				}
				ev := r.d.s.ev
				ev.mu.Lock()
				ev.events.Close()
				ev.mu.Unlock()
			}
		}
		r.expect(1, "evidence_write_failed")
		if !closed || r.d.slot == nil || r.d.slot.exposed || len(r.docs) != 1 || len(r.workflowTasks()) != 1 || r.taskCount() != 1 ||
			len(r.codes) == 0 || r.codes[len(r.codes)-1] != CodeClosing {
			t.Fatalf("a plan was exposed despite its failed exposure event: closed %t documents %d workflow %d codes %v", closed, len(r.docs),
				len(r.workflowTasks()), r.codes)
		}
	})
	t.Run("slow_pull", func(t *testing.T) {
		t.Parallel()
		// The first publication's pull is held, with the fake time moved
		// past the 4s control hand-off, until a dev-next exchange over the
		// real socket is answered: the loop keeps serving control while
		// its owned inspection runs, answers pending (never the observed
		// plan again), and the attempt then completes.
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		var answers []string
		r.pullHook = func(r *devRig) {
			if len(answers) > 0 {
				return
			}
			r.f.clock.advance(4500 * time.Millisecond)
			resp, err := devControlCall(r.d.s.runtime, ControlRequest{Op: opDevNext})
			answers = append(answers, fmt.Sprintf("%s %v", resp.Code, err))
		}
		r.expect(3, "")
		if len(answers) != 1 || answers[0] != CodePending+" <nil>" || len(r.docs) != 4 || len(r.workflowTasks()) != 4 {
			t.Fatalf("control during a slow pull: answers %v documents %d workflow %d", answers, len(r.docs), len(r.workflowTasks()))
		}
	})
	t.Run("cancelled_pull", func(t *testing.T) {
		t.Parallel()
		// The attempt stops while the first publication's pull is still
		// held (a conflicting observation over the real socket, served
		// meanwhile): cleanup cancels the pull's context and joins it
		// before settling the owned tasks, and nothing is admitted.
		r := newDevRig(t, happy())
		var answer string
		var pullErr error
		r.pullCtx = func(ctx context.Context, r *devRig) error {
			resp, err := devControlCall(r.d.s.runtime, ControlRequest{Op: opDevObserve, Ordinal: "1", Task: hexID("t_", 77)})
			answer = fmt.Sprintf("%s %v", resp.Code, err)
			<-ctx.Done()
			pullErr = ctx.Err()
			return pullErr
		}
		r.expect(1, "observation_conflict")
		in := r.d.inspection
		if in == nil {
			t.Fatal("no inspection was running when the attempt stopped")
		}
		select {
		case <-in.done:
		default:
			t.Fatal("cleanup did not join the inspection")
		}
		c := r.cleanupRecord()
		if answer != CodeObservationClash+" <nil>" || !errors.Is(pullErr, context.Canceled) || r.taskCount() != 0 || len(r.d.commits) != 0 || !c.Complete {
			t.Fatalf("a stop during a pull: answer %q pull %v tasks %d cleanup %+v", answer, pullErr, r.taskCount(), c)
		}
	})
	t.Run("log_failure", func(t *testing.T) {
		t.Parallel()
		// The terminal task's log evidence cannot be written: the failure
		// propagates before its publication is even pulled.
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		planted, pulls := false, 0
		r.pullHook = func(*devRig) { pulls++ }
		r.onTick = func(r *devRig) {
			if !planted && r.d.slot != nil && r.d.slot.observed {
				planted = true
				os.MkdirAll(filepath.Join(r.f.evidence, "tasks", "01"), 0o700)
				os.WriteFile(filepath.Join(r.f.evidence, "tasks", "01", "logs.txt"), []byte("x"), 0o600)
			}
		}
		r.expect(1, "evidence_write_failed")
		if pulls != 0 || r.taskCount() != 0 || len(r.workflowTasks()) != 1 || r.execution().Outcome != outcomeFailed {
			t.Fatalf("work continued after a log failure: pulls %d tasks %d", pulls, r.taskCount())
		}
	})
	t.Run("fragmented_stream", func(t *testing.T) {
		t.Parallel()
		// A valid record split across two pipe reads, one tick apart, is a
		// normal partial read, not a malformed stream.
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.fragment = true
		r.expect(3, "")
		if !r.tailWritten {
			t.Fatal("the second fragment was never written")
		}
	})
	t.Run("truncated_stream", func(t *testing.T) {
		t.Parallel()
		// A phase that exits with an unterminated last record is still
		// malformed once the whole stream is validated.
		r := newDevRig(t, ownerNoSteps())
		r.fault = "truncated-stream"
		r.expect(1, devSessionMalformed)
	})
	t.Run("final_scan_error", func(t *testing.T) {
		t.Parallel()
		// An unobserved task appears and the final listing fails: cleanup
		// cannot claim every owned task settled.
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		var extra string
		r.onFinal = func(r *devRig) {
			extra = r.mustDispatch(contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "triage"}, Goal: "late",
				Acceptance: "x", Payload: []string{}, RequestedBy: requester}).TaskID
			r.f.plane.listErr = errors.New("page lost")
		}
		got := r.run()
		c := r.cleanupRecord()
		if got != 1 || c.Complete || c.ScanError == "" || r.d.cleanupOK || r.execution().Outcome == outcomeUnverified || extra == "" {
			t.Fatalf("exit %d cleanup %+v", got, c)
		}
		if strings.Contains(r.f.stdout.String(), devUnverifiedLine) {
			t.Fatal("an incomplete final scan reported execution")
		}
	})
	t.Run("late_admission", func(t *testing.T) {
		t.Parallel()
		// A task admitted after the loop ended is found by the final scan:
		// it is cancelled, recorded as rejected and fails the attempt.
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		var extra string
		r.onFinal = func(r *devRig) {
			extra = r.mustDispatch(contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "triage"}, Goal: "late",
				Acceptance: "x", Payload: []string{}, RequestedBy: requester}).TaskID
		}
		r.expect(1, "extra_admission")
		ex, c := r.execution(), r.cleanupRecord()
		if ex.Rejected != 1 || !slices.Equal(r.d.rejected, []string{extra}) || r.terminalState(extra) != contract.TaskCancelled ||
			!slices.Contains(c.CancelledTasks, extra) || !c.Complete || ex.Outcome != outcomeStopped {
			t.Fatalf("late admission: %+v %+v", ex, c)
		}
	})
	t.Run("linger", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.fault = "linger"
		got := r.run()
		c := r.cleanupRecord()
		if got != 1 || r.d.cleanupOK || c.Complete || strings.Contains(r.f.stdout.String(), devUnverifiedLine) || r.execution().Cleanup != "incomplete" {
			t.Fatalf("cleanup claimed complete: exit %d %+v", got, c)
		}
	})
	t.Run("unsafe_paths", func(t *testing.T) {
		t.Parallel()
		// A helper path or runtime that a simple coordinator Bash line could
		// not hold is refused before any paid work starts.
		r := newDevRig(t, happy())
		r.f.executable = "/fake/bin/re e2e"
		r.expect(1, "permission_rule_invalid")
		if len(r.f.plane.ids()) != 0 || len(r.f.launcher.specsOf(fakeCallsheet)) != 0 {
			t.Fatal("work started with an unsafe helper path")
		}
		r = newDevRig(t, happy())
		root, err := os.MkdirTemp(shortRoot, "re x")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
		r.f.tempRoot = root
		r.expect(1, "permission_rule_invalid")
		if len(r.f.plane.ids()) != 0 || slices.ContainsFunc(r.f.launcher.argv(), func(a string) bool { return strings.Contains(a, " plane run") }) {
			t.Fatal("the plane started with an unsafe runtime")
		}
	})
	t.Run("premature_coder", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}})
		r.fault = "premature-coder"
		r.expect(1, "coding_before_approval")
		if r.terminalState(r.d.rejected[0]) != contract.TaskCancelled {
			t.Fatal("the premature coder was not cancelled")
		}
	})
}
