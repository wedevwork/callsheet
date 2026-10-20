package reale2e

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Offline replay (design 12a-dev-flow-verify, Replay and audit,
// Publication verification, Reviewer evidence). The checker replays the
// pure policy from the seed (InitialPlan, Next, the owner decisions and
// receipts) on outcomes it reconstructs from the authoritative task
// views, the validated evidence repository, the captured decisions and the
// event order, never from the stored ledger or its counts. Every plan and
// envelope is rebuilt from the frozen sources and the reconstructed state
// and compared byte for byte with the stored plan, the dispatch document
// and the dispatched request; every publication is re-admitted on trees
// loaded independently from the repository; every code review's execution
// evidence is re-derived from its captured log. The stored transitions,
// records and ledger must then equal the replay.

// devReplay is the replay's outcome.
type devReplay struct {
	state    DevFullState
	complete bool
	// stopCode is a policy stop (Next's); halted is a replay that ended
	// at a step whose evidence is absent (a supervisor stop).
	stopCode string
	halted   bool
	pausedAt *devGate
	reviews  map[int]DevReviewEvidence
}

// replay drives the policy over the captured evidence.
func (c *devChecker) replay() {
	m := c.m
	rp := &devReplay{reviews: map[int]DevReviewEvidence{}}
	c.rp = rp
	if !c.flowOK {
		c.absent("transitions", "flow_mismatch", c.reached(typeIs(EvVersionsChecked)))
		rp.halted = true
		return
	}
	seed := m.Workspace.SeedCommit
	if _, ok := c.trees[seed]; !ok {
		c.absent("publications", "seed_missing", c.seqOf(func(e Event) bool { return e.Type == EvSeedCreated }) > 0)
		rp.halted = true
		return
	}
	c.checkSeed(seed)
	st, res := InitialPlan(seed)
	st.RunID = c.runID
	if res.Invalidate {
		st = InvalidateReceipt(st)
	}
	decision := 0
	for range 2*maxWorkflowTasks + 8 {
		switch res.Action {
		case actionComplete:
			rp.complete = true
		case actionStop:
			rp.stopCode = res.Stop
		case actionDispatch, actionPause:
			var ok bool
			if res.Action == actionDispatch {
				st, res, ok = c.replayTask(st)
			} else {
				decision++
				st, res, ok = c.replayDecision(st, res, decision)
			}
			if ok {
				continue
			}
			rp.halted = true
		}
		break
	}
	rp.state = st
	c.compareTransitions(st)
	c.compareOutcome()
	c.checkTreeManifest(st)
}

// taskFacts is the re-derived session facts of ordinal n (nil: none).
func (c *devChecker) taskFacts(n int) *DevTaskFacts {
	for i := range c.eventMap.Tasks {
		if c.eventMap.Tasks[i].Ordinal == n {
			return &c.eventMap.Tasks[i]
		}
	}
	return nil
}

// checkSeed requires the seed to be exactly the generated development
// FEATURE.md and the seed main, test and module for its Go directive.
func (c *devChecker) checkSeed(seed string) {
	rb := c.newRow("setup/seed", "publications",
		"The seed is exactly the generated development FEATURE.md and the seed main.go, main_test.go and go.mod for the recorded Go directive", groupSetup)
	defer rb.done()
	tree := c.trees[seed]
	ver, err := goDirective(fileContent(tree, "go.mod"))
	want, werr := devSeedFiles(ver)
	if err != nil || werr != nil || len(tree) != len(want) {
		rb.bad("seed_mismatch")
		return
	}
	for name, b := range want {
		if !bytes.Equal(fileContent(tree, name), b) || tree[name].Mode != filemode.Regular {
			rb.bad("seed_mismatch")
		}
	}
	if c.treeHash[seed] != c.m.Workspace.SeedTree || c.b.Repo.refs["refs/heads/seed"].String() != seed {
		rb.bad("seed_mismatch")
	}
	rb.see("seed %s tree %s (go %s)", seed, c.treeHash[seed], auditText(ver, 16))
}

// checkTreeManifest requires the stored tree manifest to be exactly the
// seed and every replayed publication, and no extra task ref.
func (c *devChecker) checkTreeManifest(st DevFullState) {
	want := DevTreeManifest{SeedCommit: c.m.Workspace.SeedCommit, SeedTree: c.treeHash[c.m.Workspace.SeedCommit], Tasks: []devCommit{}}
	for _, t := range st.Tasks {
		want.Tasks = append(want.Tasks, devCommit{Ordinal: t.Ordinal, Task: t.TaskID, Commit: t.Commit, Parent: t.Base, Tree: t.Tree})
	}
	got := doc[devEnvelope[DevTreeManifest]](c.b, devTreesFile)
	c.source("publications", devTreesFile)
	if got == nil {
		c.absent("publications", "tree_manifest_missing", c.reached(typeIs(EvCollectionStarted)))
	} else if !bytes.Equal(mustJSON(got.Data), mustJSON(want)) {
		c.fail("publications", "tree_manifest_mismatch")
	}
	if c.b.Repo != nil && len(c.b.Repo.refs) != len(st.Tasks)+1 {
		c.fail("publications", "unreplayed_ref")
	}
}

// compareOutcome binds the replay's end to the manifest's stop and final
// identities.
func (c *devChecker) compareOutcome() {
	rp, m := c.rp, c.m
	rb := c.newRow("setup/outcome", "transitions", "The replayed policy's end (completion, a policy stop or a verified supervisor stop) agrees with the manifest's stop and final identities", groupSetup)
	rb.ref(devManifestFile, nil, "", "")
	switch {
	case rp.complete:
		st := rp.state
		if m.FinalCandidate != st.CandidateCommit || m.FinalCandidateTask != st.CandidateTask || m.FinalReview != st.ReviewCommit || m.FinalReviewTask != st.ReviewTask {
			rb.bad("final_identity_mismatch")
		}
		rb.see("complete: candidate %s (task %s), approving review %s", st.CandidateCommit, auditText(st.CandidateTask, 64), st.ReviewCommit)
	case rp.stopCode != "":
		if c.stop == nil || c.stop.code != rp.stopCode {
			rb.bad("stop_mismatch")
		}
		rb.see("policy stop %s", rp.stopCode)
	case rp.halted:
		if c.stop == nil {
			rb.bad("stop_unverified")
		} else {
			rb.see("stopped by %s at event %d", c.stop.code, c.stop.seq)
		}
	}
	if !rp.complete && (m.FinalCandidate != "" || m.FinalReview != "") {
		rb.bad("final_identity_mismatch")
	}
	rb.done()
}

// taskEvents are one task's supervisor event sequences.
type taskEvents struct {
	exposed, admitted, observed, waitStart, terminal, consumed int
}

// eventsOfTask finds ordinal n's events for task.
func (c *devChecker) eventsOfTask(n int, task string) taskEvents {
	hop := workflowLabel(n)
	var t taskEvents
	for _, e := range c.b.Events {
		if e.Hop != hop {
			continue
		}
		switch {
		case e.Type == EvCoordinatorReady && e.Code == "plan_exposed" && t.exposed == 0:
			t.exposed = e.Seq
		case e.Task != task:
		case e.Type == EvTaskAdmitted && e.Code == "" && t.admitted == 0:
			t.admitted = e.Seq
		case e.Type == EvObserved && t.observed == 0:
			t.observed = e.Seq
		case e.Type == EvTaskAdmitted && e.Code == "wait_started" && t.waitStart == 0:
			t.waitStart = e.Seq
		case e.Type == EvTaskTerminal && e.Code == "" && t.terminal == 0:
			t.terminal = e.Seq
		case e.Type == EvTaskTerminal && e.Code == "wait_consumed" && t.consumed == 0:
			t.consumed = e.Seq
		}
	}
	return t
}

// replayTask replays the pending plan's task. ok is false when the
// replay must end here (its evidence is absent or contradicts it).
func (c *devChecker) replayTask(st DevFullState) (DevFullState, StepResult, bool) {
	m := c.m
	p := st.Pending
	n := p.Ordinal
	dir := taskDir(n)
	view := c.taskView(n)
	taskID := ""
	if view != nil {
		taskID = view.TaskID
		c.views[n] = view
	}
	ev := c.eventsOfTask(n, taskID)
	rb := c.newRow(fmt.Sprintf("task/%02d/dispatch", n), "transitions",
		fmt.Sprintf("Plan %d (%s/%s, round %d, base %s) is rebuilt exactly from the frozen sources and the replayed state; the coordinator dispatched exactly that request once through MCP on the pinned workspace instance with the role's frozen selection and the supervisor admitted and observed it", n, p.Role, p.Mode, p.Round, p.ExpectedBase),
		groupEvents).task(n, taskID, p.Role, p.Mode, p.Round, p.ExpectedBase, "").at(cmpOrInt(ev.exposed, ev.admitted))
	defer rb.done()
	ec := envelopeContext{runID: c.runID, state: st, trees: c.trees, finals: c.finals, scenario: c.b.Files["setup/scenario.md"], helper: m.Helper}
	goal, acc, inputs, err := buildEnvelope(ec, p)
	stored := doc[devEnvelope[TaskPlan]](c.b, dir+"/plan.json")
	if err != nil {
		// The supervisor refuses such a plan before exposing it.
		if c.stop == nil || c.stop.code != codeOf(err) || stored != nil {
			rb.bad("plan_unreproducible")
		}
		rb.see("the plan cannot be rebuilt (%s)", codeOf(err))
		return st, StepResult{}, false
	}
	p.Goal, p.Acceptance, p.Inputs = goal, acc, inputs
	if p.Role == "coder" {
		if why := c.receiptCovers(st, p.ExpectedBase); why != "" {
			if c.stop == nil || c.stop.code != "stale_receipt" || stored != nil {
				rb.bad("coding_before_approval")
			}
			rb.see("%s", why)
			return st, StepResult{}, false
		}
	}
	if stored == nil {
		rb.miss("plan_missing", false)
		return st, StepResult{}, false
	}
	rb.ref(dir+"/plan.json", nil, taskID, "")
	if !bytes.Equal(mustJSON(stored.Data), mustJSON(*p)) {
		rb.bad("plan_mismatch")
	}
	want, _ := marshalDispatch(devDispatchDoc{Schema: devDispatchSchema, Ordinal: n, Role: p.Role, Mode: p.Mode,
		Target: contract.TaskTarget{Kind: contract.TargetID, Value: p.Role}, Goal: goal, Acceptance: acc, Payload: []string{},
		Workspace: m.Workspace.Name, Instance: m.Workspace.Instance, Base: p.ExpectedBase})
	if got, ok := c.b.Files[dir+"/dispatch.json"]; !ok {
		rb.miss("dispatch_missing", false)
	} else if !bytes.Equal(got, want) {
		rb.bad("dispatch_mismatch")
	}
	rb.ref(dir+"/dispatch.json", nil, taskID, "")
	if view == nil {
		// Exposed but never admitted, observed and settled into evidence:
		// the next blocked step, not an invented task.
		rb.miss("task_missing", ev.admitted > 0 || ev.observed > 0)
		return st, StepResult{}, false
	}
	rb.ref(dir+"/task.json", nil, taskID, "")
	if code := c.bindingOf(*view, *p); code != "" {
		rb.bad(code)
	}
	// The coordinator's MCP dispatch of exactly this plan, reconciled with
	// the plane's admission (a lost reply through one task_ls).
	if facts := c.taskFacts(n); facts == nil {
		rb.miss(devDispatchUnobserved, ev.observed > 0)
	} else {
		switch {
		case facts.Task != taskID:
			rb.bad(devDispatchMismatch)
		case facts.Problem == devDispatchUnobserved || facts.Problem == devDispatchMismatch || facts.Problem == devDispatchOrder:
			rb.bad(facts.Problem)
		}
		rb.refs(facts.Dispatch, taskID)
		if facts.Recovered {
			rb.see("the dispatch reply was lost and recovered by task_ls")
		}
	}
	if ev.admitted == 0 {
		rb.bad("admission_unrecorded")
	}
	if ev.exposed == 0 || ev.admitted < ev.exposed {
		rb.bad("premature_admission")
	}
	obs := doc[DevObservation](c.b, dir+"/observation.json")
	switch {
	case obs == nil:
		rb.miss("observation_missing", ev.observed > 0)
	case obs.Ordinal != n || obs.Task != taskID || obs.AdmittedSeq != ev.admitted || obs.ObservedSeq != ev.observed || obs.CreatedAt != view.CreatedAt ||
		obs.Phase < 1 || obs.Phase > maxPhases:
		rb.bad("observation_mismatch")
	}
	rb.ref(dir+"/observation.json", nil, taskID, "")
	rb.see("task %s admitted at seq %d, observed at seq %d", auditText(taskID, 64), ev.admitted, ev.observed)
	if !contract.TaskTerminal(view.State) || ev.consumed == 0 {
		// The task was admitted but its result never became admissible
		// before the attempt stopped.
		c.markerRow(n, *p, view, "", ev, false)
		return st, StepResult{}, false
	}
	out, ok := c.reconstructOutcome(st, *p, view, ev)
	if !ok {
		return st, StepResult{}, false
	}
	next, step := Next(st, out)
	if step.Invalidate {
		next = InvalidateReceipt(next)
	}
	c.compareRecord(n, next)
	c.finals[taskID] = out.Final
	return next, step, true
}

// cmpOrInt is a, or b when a is zero.
func cmpOrInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// taskView is ordinal n's stored authoritative task view.
func (c *devChecker) taskView(n int) *contract.TaskView {
	if v := doc[contract.TaskShowResponse](c.b, taskDir(n)+"/task.json"); v != nil {
		t := v.Task
		return &t
	}
	return nil
}

// bindingOf compares an authoritative view with its plan: the exact goal
// and acceptance, empty payload, no override, the frozen selection, and
// the workspace name, instance and planned base.
func (c *devChecker) bindingOf(v contract.TaskView, p TaskPlan) string {
	role, ok := c.flow.Role(p.Role)
	b := v.WorkspaceBinding
	switch {
	case !ok || v.Role.ID != p.Role || v.Role.Adapter != role.Adapter:
		return "role_mismatch"
	case v.Request.Goal != p.Goal || v.Request.Acceptance != p.Acceptance || len(v.Request.Payload) != 0:
		return "request_mismatch"
	case v.Request.Override != nil || v.Effective.Model != role.Model || v.Effective.Effort != role.Effort:
		return "selection_mismatch"
	case b == nil || b.Name != c.m.Workspace.Name || b.Instance != c.m.Workspace.Instance:
		return "workspace_instance_mismatch"
	case b.BaseCommit == nil || *b.BaseCommit != p.ExpectedBase:
		return "base_binding_mismatch"
	}
	return ""
}

// receiptCovers checks the active receipt for a coder plan on base: valid,
// of the current generation and latest approving audit, and the approved
// specification bytes unchanged in base ("" when it covers).
func (c *devChecker) receiptCovers(st DevFullState, base string) string {
	r, a := st.Receipt, st.Audit
	switch {
	case r == nil || !r.Valid || r.ReceiptSeq == 0:
		return "no valid owner receipt"
	case r.Generation != st.Generation || a == nil || r.AuditTask != a.TaskID || r.AuditCommit != a.Commit || r.RunID != c.runID:
		return "the receipt is not bound to the latest approving audit"
	}
	tree, ok := c.trees[base]
	if !ok || fileSHA(tree, "design.md") != r.Design || fileSHA(tree, "revision-history.md") != r.History {
		return "the approved specification changed"
	}
	return ""
}

// markerRow is task n's final-marker row.
func (c *devChecker) markerRow(n int, p TaskPlan, v *contract.TaskView, marker string, ev taskEvents, ended bool) {
	dir := taskDir(n)
	rb := c.newRow(fmt.Sprintf("task/%02d/marker", n), "transitions",
		fmt.Sprintf("Task %d ended its complete, untruncated final answer with exactly one standalone STATUS line valid for %s/%s, equal to final.txt", n, p.Role, p.Mode),
		groupEvents).task(n, v.TaskID, p.Role, p.Mode, p.Round, p.ExpectedBase, "").at(cmpOrInt(ev.terminal, ev.admitted))
	defer rb.done()
	if !ended {
		rb.miss("task_not_ended", ev.terminal > 0 && ev.consumed > 0)
		return
	}
	rb.ref(dir+"/final.txt", nil, v.TaskID, "")
	rb.see("marker %s", marker)
}

// reconstructOutcome rebuilds an admitted outcome from the view, the
// repository and the events, re-admitting its publication.
func (c *devChecker) reconstructOutcome(st DevFullState, p TaskPlan, v *contract.TaskView, ev taskEvents) (Admitted, bool) {
	n := p.Ordinal
	dir := taskDir(n)
	r := v.Result
	final := ""
	mrb := c.newRow(fmt.Sprintf("task/%02d/marker", n), "transitions",
		fmt.Sprintf("Task %d succeeded with exit 0 and ended its complete, untruncated final answer (equal to final.txt) with exactly one standalone STATUS line valid for %s/%s", n, p.Role, p.Mode),
		groupEvents).task(n, v.TaskID, p.Role, p.Mode, p.Round, p.ExpectedBase, "").at(ev.terminal)
	mrb.ref(dir+"/final.txt", nil, v.TaskID, "")
	ok := true
	switch {
	case v.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0:
		mrb.bad("task_failed")
		ok = false
	case r.FinalMessage == nil || r.FinalMessageTruncated:
		mrb.bad("final_message_missing")
		ok = false
	default:
		final = *r.FinalMessage
		if got, has := c.b.Files[dir+"/final.txt"]; !has || string(got) != final {
			mrb.bad("final_text_mismatch")
		}
	}
	marker, err := parseTerminalMarker(final)
	if ok && (err != nil || !markerAllowed(p.Role, p.Mode, marker)) {
		mrb.bad("invalid_marker")
		ok = false
	}
	if ok {
		mrb.see("marker %s", marker)
	}
	mrb.done()
	if !ok {
		return Admitted{}, false
	}
	prb := c.newRow(fmt.Sprintf("task/%02d/publication", n), "publications",
		fmt.Sprintf("Task %d published one commit whose single parent is its planned base; its complete tree, re-read from the evidence repository, changes only %s's allowed paths, keeps every input byte and mode, fits the source and artifact budgets and meets the producer's artifact contract", n, p.Role),
		groupEvents).task(n, v.TaskID, p.Role, p.Mode, p.Round, p.ExpectedBase, "").at(ev.consumed)
	defer prb.done()
	if r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || r.Workspace.Commit == nil {
		prb.bad("publication_missing")
		return Admitted{}, false
	}
	commit := *r.Workspace.Commit
	prb.r.ResultCommit = commit
	ref := fmt.Sprintf("refs/heads/task-%02d", n)
	if c.b.Repo == nil || c.b.Repo.refs[ref].String() != commit {
		prb.bad("result_ref_mismatch")
		return Admitted{}, false
	}
	if parent, has := c.commitParent(commit); !has || parent != p.ExpectedBase {
		prb.bad("parent_mismatch")
		return Admitted{}, false
	}
	base, okB := c.trees[p.ExpectedBase]
	result, okR := c.trees[commit]
	if !okB || !okR {
		prb.bad("tree_unreadable")
		return Admitted{}, false
	}
	in := admitInput{Role: p.Role, Mode: p.Mode, Marker: marker, Base: treeList(base), Result: treeList(result), Final: final, TaskID: v.TaskID}
	if p.Role == "design-reviewer" {
		in.DesignDigest = fileSHA(base, "design.md")
	}
	if p.Role == "code-reviewer" {
		if p.ExpectedBase != st.CandidateCommit {
			prb.bad("review_input_not_candidate")
			return Admitted{}, false
		}
		in.Candidate = p.ExpectedBase
		rev := c.reviewerExecution(n, p, v, marker, base, fileText(result, "review.md"), ev)
		if _, stops := rev.blocks(); stops {
			return Admitted{}, false
		}
		in.ReviewRun = rev.runDigest()
	}
	res, err := AdmitPublication(in)
	if err != nil {
		prb.bad("publication_rejected")
		prb.see("%s", auditText(err.Error(), 300))
		return Admitted{}, false
	}
	if p.Role == "coder" {
		if why := c.receiptCovers(st, p.ExpectedBase); why != "" || fileSHA(res.Files, "design.md") != st.Receipt.Design ||
			fileSHA(res.Files, "revision-history.md") != st.Receipt.History {
			prb.bad("stale_receipt")
			return Admitted{}, false
		}
	}
	c.checkFixedInputs(prb, result)
	tree := c.treeHash[commit]
	sel, _ := c.flow.Role(p.Role)
	out := Admitted{Ordinal: n, TaskID: v.TaskID, Role: p.Role, Mode: p.Mode, Marker: marker, Commit: commit, Tree: tree, Base: p.ExpectedBase,
		Workspace: c.m.Workspace.Name, Instance: c.m.Workspace.Instance, Selection: sel, Final: final, Report: res.Report,
		DesignSHA: fileSHA(res.Files, "design.md"), HistorySHA: fileSHA(res.Files, "revision-history.md"), ReviewSHA: fileSHA(res.Files, "design-review.md"),
		Artifacts: treeDigests(res.Files), Blocker: res.Blocker, DesignRequired: res.DesignRequired, BriefCurrent: res.BriefCurrent,
		CreatedAt: v.CreatedAt, AdmittedSeq: ev.admitted, ObservedSeq: ev.observed, ConsumedSeq: ev.consumed}
	if v.FinishedAt != nil {
		out.FinishedAt = *v.FinishedAt
	}
	adm := doc[DevAdmission](c.b, dir+"/admission.json")
	wantAdm := DevAdmission{Schema: devKind("admission"), RunID: c.runID, Task: v.TaskID, Marker: marker, Commit: commit, Parent: p.ExpectedBase,
		Tree: tree, Artifacts: out.Artifacts, Disposition: dispositionOf(p.Role, marker), Blocker: res.Blocker, DesignRequired: res.DesignRequired,
		BriefCurrent: res.BriefCurrent}
	switch {
	case adm == nil:
		prb.miss("admission_missing", true)
	case !bytes.Equal(mustJSON(*adm), mustJSON(wantAdm)):
		prb.bad("admission_record_mismatch")
	}
	prb.ref(dir+"/admission.json", nil, v.TaskID, commit)
	prb.see("commit %s parent %s changed [%s]", commit, p.ExpectedBase, auditText(strings.Join(changedPaths2(base, result), ","), 300))
	return out, true
}

// checkFixedInputs requires FEATURE.md and go.mod to stay the seed's.
func (c *devChecker) checkFixedInputs(rb *rowBuilder, result map[string]TreeFile) {
	seed := c.trees[c.m.Workspace.SeedCommit]
	for _, name := range []string{"FEATURE.md", "go.mod"} {
		if fileSHA(result, name) != fileSHA(seed, name) {
			rb.bad("fixed_input_changed")
		}
	}
}

// treeList is a tree's files in path order.
func treeList(t map[string]TreeFile) []TreeFile {
	out := make([]TreeFile, 0, len(t))
	for _, name := range sortedTree(t) {
		out = append(out, t[name])
	}
	return out
}

// compareRecord compares ordinal n's stored task record with the replay's.
func (c *devChecker) compareRecord(n int, st DevFullState) {
	if len(st.Tasks) == 0 || st.Tasks[len(st.Tasks)-1].Ordinal != n {
		return
	}
	want := st.Tasks[len(st.Tasks)-1]
	got := doc[devEnvelope[TaskRecord]](c.b, taskDir(n)+"/record.json")
	if got == nil || !bytes.Equal(mustJSON(got.Data), mustJSON(want)) {
		c.fail("transitions", "record_mismatch")
	}
	c.source("transitions", taskDir(n)+"/record.json")
}

// replayDecision replays the owner's decision for the pause res.
func (c *devChecker) replayDecision(st DevFullState, res StepResult, k int) (DevFullState, StepResult, bool) {
	rel := fmt.Sprintf("decisions/%02d.json", k)
	dec := doc[devEnvelope[DevDecision]](c.b, rel)
	prompt := c.seqOf(func(e Event) bool {
		return e.Type == EvAwaitingOwner && e.Hop == fmt.Sprintf("generation-%d", res.Generation) && e.Code == res.Pause
	})
	rb := c.newRow(fmt.Sprintf("decision/%02d/binding", k), "owner_receipts",
		fmt.Sprintf("After the %s gate of generation %d, the phase exited with its gate line and its group gone, the prompt opened a window of at most 20m within the attempt, and an explicit, generation-qualified owner line inside that window decided it", res.Pause, res.Generation),
		groupEvents).at(prompt)
	defer rb.done()
	c.rp.pausedAt = &devGate{kind: res.Pause, gen: res.Generation}
	if dec == nil {
		rb.miss("decision_missing", false)
		return st, StepResult{}, false
	}
	d := dec.Data
	rb.ref(rel, nil, "", "")
	ev := c.eventAt(d.EventSeq)
	kinds := map[string][]string{pauseOwner: {"yes", "no"}, pauseAnswer: {"answer"}}
	switch {
	case d.Sequence != k || d.Window != res.Pause || d.Generation != res.Generation || !slices.Contains(kinds[res.Pause], d.Kind):
		rb.bad("decision_binding_mismatch")
	case ev == nil || ev.Type != EvOwnerDecision || ev.Hop != fmt.Sprintf("generation-%d", d.Generation) || ev.Code != d.Kind || ev.Time != d.Time:
		rb.bad("decision_event_mismatch")
	case prompt == 0 || prompt >= d.EventSeq:
		rb.bad("decision_out_of_order")
	}
	if prompt > 0 && ev != nil {
		p := c.eventAt(prompt)
		pt, _ := parseTime(p.Time)
		dt, _ := parseTime(ev.Time)
		deadline := minTime(pt.Add(OwnerDecisionBound), c.attemptDeadline())
		if !dt.Before(deadline) {
			rb.bad("decision_deadline_exceeded")
		}
	}
	if d.Kind == "answer" && (d.Answer == "" || len(d.Answer) > triageTaskCeiling) {
		rb.bad("decision_binding_mismatch")
	}
	rb.see("%s %d decided %q at seq %d", d.Window, d.Generation, d.Kind, d.EventSeq)
	switch d.Kind {
	case "no":
		next, step := OwnerDecision(st, d.Generation, false, OwnerReceipt{}, k)
		return next, step, true
	case "answer":
		next, step := OwnerAnswer(st, d.Generation, d.Answer, k)
		if step.Invalidate {
			next = InvalidateReceipt(next)
		}
		return next, step, true
	}
	rec, ok := c.replayReceipt(st, d, prompt)
	if !ok {
		return st, StepResult{}, false
	}
	next, step := OwnerDecision(st, d.Generation, true, rec, k)
	if step.Invalidate {
		next = InvalidateReceipt(next)
	}
	return next, step, true
}

// replayReceipt verifies generation's receipt: bound to the latest
// approving audit, its approved bytes, and strictly ordered audit
// admission, wait consumption, phase exit (group gone), prompt, decision
// and receipt events; no plain, buffered or stale yes backfills it.
func (c *devChecker) replayReceipt(st DevFullState, d DevDecision, prompt int) (OwnerReceipt, bool) {
	rel := fmt.Sprintf("receipts/%02d.json", d.Generation)
	rb := c.newRow(fmt.Sprintf("receipt/%02d/authorization", d.Generation), "owner_receipts",
		fmt.Sprintf("Generation %d's receipt binds the latest approving audit's task, commit and design, history and review SHA-256 values, and its events are strictly ordered: audit admission, wait consumption, phase exit, prompt, decision, receipt, then the coder's admission", d.Generation),
		groupEvents).at(d.EventSeq)
	defer rb.done()
	got := doc[devEnvelope[OwnerReceipt]](c.b, rel)
	want, err := BuildReceipt(st, d.Generation)
	if got == nil || err != nil {
		if got == nil {
			rb.miss("receipt_missing", c.seqOf(func(e Event) bool {
				return e.Type == EvReceiptCreated && e.Hop == fmt.Sprintf("generation-%d", d.Generation)
			}) > 0)
		} else {
			rb.bad("stale_receipt")
		}
		return OwnerReceipt{}, false
	}
	rb.ref(rel, nil, got.Data.AuditTask, got.Data.AuditCommit)
	r := got.Data
	rcpt := c.seqOf(func(e Event) bool {
		return e.Type == EvReceiptCreated && e.Hop == fmt.Sprintf("generation-%d", d.Generation)
	})
	exit := c.phaseExitBefore(prompt)
	want.PhaseExitSeq, want.PromptSeq, want.DecisionSeq, want.DecisionTime, want.ReceiptSeq = exit, prompt, d.EventSeq, d.Time, rcpt
	if !bytes.Equal(mustJSON(r), mustJSON(want)) {
		rb.bad("receipt_binding_mismatch")
	}
	seqs := []int{r.ApprovalSeq, r.ConsumedSeq, r.PhaseExitSeq, r.PromptSeq, r.DecisionSeq, r.ReceiptSeq}
	for i := 1; i < len(seqs); i++ {
		if seqs[i-1] <= 0 || seqs[i] <= seqs[i-1] {
			rb.bad("receipt_order")
			break
		}
	}
	if audit, ok := c.trees[r.AuditCommit]; !ok || fileSHA(audit, "design.md") != r.Design || fileSHA(audit, "revision-history.md") != r.History ||
		fileSHA(audit, "design-review.md") != r.Review {
		rb.bad("receipt_digest_mismatch")
	}
	rb.see("generation %d receipt for audit task %s commit %s at seq %d", r.Generation, auditText(r.AuditTask, 64), r.AuditCommit, r.ReceiptSeq)
	return r, true
}

// phaseExitBefore is the last phase exit event before seq.
func (c *devChecker) phaseExitBefore(seq int) int {
	exit := 0
	for _, e := range c.b.Events {
		if e.Seq >= seq {
			break
		}
		if e.Type == EvPhaseExited {
			exit = e.Seq
		}
	}
	return exit
}

// attemptDeadline is the manifest's attempt deadline.
func (c *devChecker) attemptDeadline() time.Time {
	t, _ := parseTime(c.m.AttemptDeadline)
	return t
}

// compareTransitions requires exactly the replay's transition records.
func (c *devChecker) compareTransitions(st DevFullState) {
	stored := c.b.ordinalsOf("transitions")
	for i, tr := range st.Transitions {
		n := i + 1
		rel := fmt.Sprintf("transitions/%02d.json", n)
		rb := c.newRow(fmt.Sprintf("transition/%02d/route", n), "transitions",
			fmt.Sprintf("Transition %d (%s -> %s, %s) is the policy's own move from its source, with the counters reserved before dispatch and never reset", n, tr.FromState, tr.ToState, tr.Reason),
			groupEvents).task(tr.ToOrdinal, tr.FromTask, "", "", 0, "", "").at(c.transitionSeq(tr))
		got := doc[devEnvelope[TransitionRecord]](c.b, rel)
		switch {
		case got == nil:
			rb.miss("transition_missing", true)
		case got.Data != tr:
			rb.bad("transition_mismatch")
		}
		rb.ref(rel, nil, tr.FromTask, "")
		rb.see("design %d->%d, code %d->%d, recovery %d->%d", tr.DesignBefore, tr.DesignAfter, tr.CodeBefore, tr.CodeAfter, tr.RecoveryBefore, tr.RecoveryAfter)
		rb.done()
	}
	if len(stored) != len(st.Transitions) {
		c.fail("transitions", "transition_count_mismatch")
	}
	for i, n := range stored {
		if n != i+1 {
			c.fail("transitions", "transition_ordinal_gap")
		}
	}
	ledger := doc[devEnvelope[DevFullState]](c.b, devLedgerFile)
	c.source("transitions", devLedgerFile)
	if ledger == nil {
		c.absent("transitions", "ledger_missing", true)
	} else if !bytes.Equal(mustJSON(ledger.Data), mustJSON(normalizeState(st))) {
		c.fail("transitions", "ledger_mismatch")
	}
	tasks := c.b.ordinalsOf("tasks")
	for i, n := range tasks {
		if n != i+1 {
			c.fail("transitions", "ordinal_gap")
		}
	}
	if len(tasks) > len(st.Tasks)+1 {
		c.fail("transitions", "unreplayed_task")
	}
}

// transitionSeq orders a transition after its source's consumption (or
// its decision).
func (c *devChecker) transitionSeq(tr TransitionRecord) int {
	if tr.FromDecision > 0 {
		if d := doc[devEnvelope[DevDecision]](c.b, fmt.Sprintf("decisions/%02d.json", tr.FromDecision)); d != nil {
			return d.Data.EventSeq
		}
	}
	if tr.FromOrdinal > 0 {
		return c.eventsOfTask(tr.FromOrdinal, tr.FromTask).consumed
	}
	return 0
}

// bundleReviewInput is a code review's evidence derivation input read
// from a bundle: the stored log and its TaskLogs metadata (a log whose
// bytes do not hash as recorded is not complete), the authoritative task
// view's times and the given trusted plan, trees and report.
func bundleReviewInput(b *DevBundle, dir string, n int, v *contract.TaskView, helper reviewHelperSpec, candidate, initial, candTree string,
	cand map[string]TreeFile, seedMain []byte, marker, report string) reviewEvidenceInput {
	logs, hasLog := b.Files[dir+"/logs.txt"]
	lr := doc[DevLogRecord](b, dir+"/logs.json")
	in := reviewEvidenceInput{Ordinal: n, Log: logs, LogFile: dir + "/logs.txt", Task: v.TaskID, Helper: helper, Candidate: candidate,
		Initial: initial, CandTree: candTree, CandFiles: cand, SeedMain: seedMain, Marker: marker, Report: report}
	switch {
	case lr == nil:
		in.Meta = DevLogMeta{Error: "logs.json missing"}
	case !hasLog:
		in.Meta = DevLogMeta{Error: "logs.txt missing"}
	default:
		in.Meta = lr.Meta
		if lr.Meta.RawSHA256 != sha256Hex(logs) && lr.Meta.complete() {
			in.Meta.StoredChanged = true
		}
	}
	if at, ok := contract.ParseTime(v.CreatedAt); ok {
		in.AdmittedAt = at
	}
	if v.FinishedAt != nil {
		if at, ok := contract.ParseTime(*v.FinishedAt); ok {
			in.FinishedAt = at
		}
	}
	return in
}

// reviewerExecution re-derives a code review's execution evidence and
// compares it with the stored record.
func (c *devChecker) reviewerExecution(n int, p TaskPlan, v *contract.TaskView, marker string, cand map[string]TreeFile, report string, ev taskEvents) DevReviewEvidence {
	dir := taskDir(n)
	rb := c.newRow(fmt.Sprintf("task/%02d/reviewer-execution", n), "reviewer_execution",
		"The reviewer ran the fixed verification helper exactly once, corroborated by one top-level command record in its complete captured log; its receipt is bound to this task, candidate, initial source, Go executable and deadline, its coverage and test claims recompute, review.md names its digest and an approval has its complete successful run",
		groupEvents).task(n, v.TaskID, p.Role, p.Mode, p.Round, p.ExpectedBase, "").at(ev.consumed)
	defer rb.done()
	in := bundleReviewInput(c.b, dir, n, v, c.m.Helper, p.ExpectedBase, c.m.Workspace.SeedCommit, c.treeHash[p.ExpectedBase], cand,
		fileContent(c.trees[c.m.Workspace.SeedCommit], "main.go"), marker, report)
	derived := DeriveDevReviewEvidence(in)
	c.rp.reviews[n] = derived
	stored := doc[DevReviewRecord](c.b, dir+"/review-evidence.json")
	switch {
	case stored == nil:
		rb.miss("review_evidence_missing", true)
	case !bytes.Equal(mustJSON(stored.Evidence), mustJSON(derived)):
		rb.bad(CodeReviewMismatch)
	}
	if code, stops := derived.blocks(); stops {
		rb.bad(code)
	}
	if derived.Approving && derived.Status != evidenceVerified {
		rb.bad(CodeReviewUnobserved)
	}
	rb.ref(dir+"/logs.txt", nil, v.TaskID, "")
	rb.ref(dir+"/review-evidence.json", nil, v.TaskID, "")
	if derived.Started != nil {
		rb.refs([]SourceRef{{File: derived.Started.File, SHA256: derived.Started.SHA256, FirstLine: derived.Started.FirstLine, LastLine: derived.Completed.LastLine}}, v.TaskID)
	}
	rb.see("evidence %s (%s), run %s", derived.Status, cmpOr(derived.Code, "ok"), cmpOr(derived.RunStatus, "none"))
	if derived.Coverage != nil {
		rb.see("reviewer changed-code coverage %d/%d (%s%%)", derived.Coverage.Covered, derived.Coverage.Total, derived.Coverage.Percent)
	}
	return derived
}
