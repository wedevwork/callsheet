package reale2e

import (
	"fmt"
	"slices"
	"strings"
)

// Transition policy (design 12a-dev-flow-harness, FP-3 and FP-4). Next is
// pure: it does not dispatch, and it never mutates the state it is given
// (every slice and record it changes is copied). Counters are reserved
// before the dispatch the caller then performs. An incomplete cycle is not
// refunded.

const (
	maxDesignCycles  = 2
	maxCodeCycles    = 2
	maxRecoveryTasks = 2
	maxWorkflowTasks = 14
	maxPhases        = 4
	scenarioFull     = "full"
)

const (
	modeDraft       = "draft"
	modeRevise      = "revise"
	modeAudit       = "audit"
	modeImplement   = "implement"
	modeFix         = "fix"
	modeReviewFull  = "full"
	modeInvestigate = "investigate"
)

const (
	actionDispatch = "dispatch"
	actionPause    = "pause"
	actionStop     = "stop"
	actionComplete = "complete"
)

const (
	pauseOwner  = "owner"
	pauseAnswer = "answer"
	stopCap     = "cap_exhausted"
)

// Publication dispositions.
const (
	dispositionReady      = "implementation-ready"
	dispositionDiagnostic = "diagnostic"
	dispositionReport     = "report"
)

// PlanInput is one immutable input a plan names: a task's published file
// (Path set) or a task's final answer (Path empty).
type PlanInput struct {
	Kind    string `json:"kind"`
	Ordinal int    `json:"ordinal"`
	Task    string `json:"task"`
	Commit  string `json:"commit"`
	Path    string `json:"path,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// TaskPlan is one persisted workflow plan. Ordinals are one-based.
type TaskPlan struct {
	Ordinal       int         `json:"ordinal"`
	Role          string      `json:"role"`
	Mode          string      `json:"mode"`
	OriginalMode  string      `json:"original_mode"`
	Round         int         `json:"round"`
	DesignRound   int         `json:"design_round"`
	CodeRound     int         `json:"code_round"`
	RecoveryRound int         `json:"recovery_round"`
	Recovery      bool        `json:"recovery"`
	Reason        string      `json:"reason"`
	Generation    int         `json:"generation"`
	Inputs        []PlanInput `json:"inputs"`
	ExpectedBase  string      `json:"expected_base"`
	Diagnostic    bool        `json:"diagnostic_base"`
	Allowed       []string    `json:"allowed_outputs"`
	Acceptance    string      `json:"acceptance"`
	Goal          string      `json:"goal"`
}

// TaskRecord is one admitted workflow task.
type TaskRecord struct {
	Ordinal     int               `json:"ordinal"`
	TaskID      string            `json:"task_id"`
	Role        string            `json:"role"`
	Mode        string            `json:"mode"`
	Round       int               `json:"round"`
	Reason      string            `json:"reason"`
	Selection   FlowRole          `json:"selection"`
	Workspace   string            `json:"workspace"`
	Instance    string            `json:"instance"`
	Base        string            `json:"base"`
	Commit      string            `json:"commit"`
	Tree        string            `json:"tree"`
	Marker      string            `json:"marker"`
	Disposition string            `json:"disposition"`
	Artifacts   map[string]string `json:"artifacts"`
	CreatedAt   string            `json:"created_at"`
	FinishedAt  string            `json:"finished_at"`
	AdmittedSeq int               `json:"admitted_seq"`
	ObservedSeq int               `json:"observed_seq"`
	ConsumedSeq int               `json:"consumed_seq"`
}

// TransitionRecord binds a move to its source and the counters around it.
type TransitionRecord struct {
	FromOrdinal    int    `json:"from_ordinal"`
	FromTask       string `json:"from_task"`
	FromDecision   int    `json:"from_decision"`
	FromState      string `json:"from_state"`
	ToState        string `json:"to_state"`
	Reason         string `json:"reason"`
	ToOrdinal      int    `json:"to_ordinal"`
	DesignBefore   int    `json:"design_before"`
	DesignAfter    int    `json:"design_after"`
	CodeBefore     int    `json:"code_before"`
	CodeAfter      int    `json:"code_after"`
	RecoveryBefore int    `json:"recovery_before"`
	RecoveryAfter  int    `json:"recovery_after"`
	Stop           string `json:"stop,omitempty"`
}

// AuditBinding is one design audit: its input design revision and the
// report it published.
type AuditBinding struct {
	Ordinal    int    `json:"ordinal"`
	TaskID     string `json:"task_id"`
	Commit     string `json:"commit"`
	Base       string `json:"base"`
	Generation int    `json:"generation"`
	Marker     string `json:"marker"`
	Design     string `json:"design_sha256"`
	History    string `json:"history_sha256"`
	Review     string `json:"review_sha256"`
	// AdmittedSeq and ConsumedSeq are the audit's admission and consumed
	// wait event sequences (the receipt's ordering starts there).
	AdmittedSeq int `json:"admitted_seq"`
	ConsumedSeq int `json:"consumed_seq"`
}

// OwnerReceipt is one generation's explicit decision. Prior receipts are
// never overwritten or reactivated.
type OwnerReceipt struct {
	RunID        string `json:"run_id"`
	Scenario     string `json:"scenario"`
	Generation   int    `json:"generation"`
	AuditOrdinal int    `json:"audit_ordinal"`
	AuditTask    string `json:"audit_task"`
	AuditCommit  string `json:"audit_commit"`
	Design       string `json:"design_sha256"`
	History      string `json:"history_sha256"`
	Review       string `json:"review_sha256"`
	ApprovalSeq  int    `json:"approval_seq"`
	ConsumedSeq  int    `json:"consumed_seq"`
	PhaseExitSeq int    `json:"phase_exit_seq"`
	PromptSeq    int    `json:"prompt_seq"`
	DecisionSeq  int    `json:"decision_seq"`
	ReceiptSeq   int    `json:"receipt_seq"`
	DecisionTime string `json:"decision_time"`
	Decision     string `json:"decision"`
	Valid        bool   `json:"valid"`
}

// Outcome is one admitted task's identity and final answer.
type Outcome struct {
	Ordinal  int    `json:"ordinal"`
	TaskID   string `json:"task_id"`
	Role     string `json:"role"`
	Mode     string `json:"mode"`
	Marker   string `json:"marker"`
	Commit   string `json:"commit"`
	Final    string `json:"final"`
	FinalSHA string `json:"final_sha256"`
}

// DevFullState is the bounded ledger. Role IDs are lookup keys, not task
// identities.
type DevFullState struct {
	RunID         string `json:"run_id"`
	Scenario      string `json:"scenario"`
	InitialCommit string `json:"initial_commit"`
	ChainCommit   string `json:"chain_commit"`
	// CandidateTask and CandidateCommit are the last complete coder
	// candidate (IMPL_COMPLETE or FIXES_COMPLETE); a diagnostic
	// publication never replaces them.
	CandidateTask   string `json:"candidate_task"`
	CandidateCommit string `json:"candidate_commit"`
	// ReviewTask and ReviewCommit are the approving code review.
	ReviewTask   string         `json:"review_task"`
	ReviewCommit string         `json:"review_commit"`
	Generation   int            `json:"generation"`
	Audit        *AuditBinding  `json:"approving_audit"`
	LastAudit    *AuditBinding  `json:"last_audit"`
	Receipt      *OwnerReceipt  `json:"receipt"`
	Receipts     []OwnerReceipt `json:"receipts"`
	// Diagnostic is the last diagnostic publication (questions, a design
	// or triage escalation); Question the designer's open questions;
	// Clarification the owner's answer for the next designer; LastReview
	// the last code review; Triage the last qualified triage; NeedsTriage
	// the coder outcome that named the findings.
	Diagnostic    *Outcome           `json:"diagnostic"`
	Question      *Outcome           `json:"question"`
	Clarification string             `json:"clarification"`
	LastReview    *Outcome           `json:"last_review"`
	Triage        *Outcome           `json:"triage"`
	NeedsTriage   *Outcome           `json:"needs_triage"`
	Escalation    *Outcome           `json:"escalation"`
	DesignCycles  int                `json:"design_cycles"`
	CodeCycles    int                `json:"code_cycles"`
	Recovery      int                `json:"recovery"`
	DesignerCount int                `json:"designer_count"`
	CoderCount    int                `json:"coder_count"`
	ResumeFix     bool               `json:"resume_fix"`
	Pending       *TaskPlan          `json:"pending"`
	Tasks         []TaskRecord       `json:"tasks"`
	Transitions   []TransitionRecord `json:"transitions"`
	StopCode      string             `json:"stop_code"`
	Complete      bool               `json:"complete"`
}

// Admitted is one fully published, correctly bound outcome. The
// supervisor fills it only after FP-6's admission passed.
type Admitted struct {
	Ordinal        int
	TaskID         string
	Role, Mode     string
	Marker         string
	Commit         string
	Tree           string
	Base           string
	Workspace      string
	Instance       string
	Selection      FlowRole
	Final          string
	Report         string
	DesignSHA      string
	HistorySHA     string
	ReviewSHA      string
	Artifacts      map[string]string
	Blocker        string
	DesignRequired string
	// BriefCurrent is set only when this triage publication itself
	// published fix.md and its confidence matches the marker.
	BriefCurrent bool
	CreatedAt    string
	FinishedAt   string
	AdmittedSeq  int
	ObservedSeq  int
	ConsumedSeq  int
}

// StepResult is Next's decision. Plan is set only for actionDispatch.
type StepResult struct {
	Action     string
	Pause      string
	Stop       string
	Plan       *TaskPlan
	Generation int
	Reason     string
	Invalidate bool
}

// Next selects the semantic transition for an admitted outcome. The
// caller has already checked publication, binding and the marker parse.
func Next(st DevFullState, out Admitted) (DevFullState, StepResult) {
	if st.Pending == nil || out.Ordinal != st.Pending.Ordinal || out.Role != st.Pending.Role || out.Mode != st.Pending.Mode {
		return stopWith(st, "invalid_transition", "")
	}
	if !markerAllowed(out.Role, out.Mode, out.Marker) {
		return stopWith(st, "invalid_marker", "")
	}
	st = recordTask(st, out)
	o := &Outcome{Ordinal: out.Ordinal, TaskID: out.TaskID, Role: out.Role, Mode: out.Mode, Marker: out.Marker, Commit: out.Commit,
		Final: out.Final, FinalSHA: sha256Hex([]byte(out.Final))}
	switch out.Role + "/" + out.Mode + "/" + out.Marker {
	case "designer/" + modeDraft + "/" + markerDesignDraft, "designer/" + modeRevise + "/" + markerDesignDraft:
		st.Generation++
		st.Question, st.Clarification = nil, ""
		return schedule(st, TaskPlan{Role: "design-reviewer", Mode: modeAudit, Reason: "audit"}, "audit")
	case "designer/" + modeDraft + "/" + markerNeedsInput, "designer/" + modeRevise + "/" + markerNeedsInput:
		st.Question = o
		st = moved(st, "paused/"+pauseAnswer, "needs_input")
		return st, StepResult{Action: actionPause, Pause: pauseAnswer, Generation: st.Generation, Reason: "needs_input"}
	case "designer/" + modeDraft + "/" + markerBlocked, "designer/" + modeRevise + "/" + markerBlocked,
		"coder/" + modeImplement + "/" + markerBlocked, "coder/" + modeFix + "/" + markerBlocked,
		"triage/" + modeInvestigate + "/" + markerTriageBlocked:
		return stopWith(st, "obstacle", "")
	case "design-reviewer/" + modeAudit + "/" + markerDesignChanges:
		st.LastAudit = auditBindingOf(st, out)
		return scheduleDesigner(st, false, "changes_requested")
	case "design-reviewer/" + modeAudit + "/" + markerDesignApproved:
		st.LastAudit = auditBindingOf(st, out)
		st.Audit = st.LastAudit
		st = moved(st, "paused/"+pauseOwner, "approved")
		return st, StepResult{Action: actionPause, Pause: pauseOwner, Generation: st.Generation, Reason: "approved"}
	case "coder/" + modeImplement + "/" + markerImplComplete, "coder/" + modeFix + "/" + markerFixesComplete:
		st.CandidateTask, st.CandidateCommit = out.TaskID, out.Commit
		st.Diagnostic = nil
		return schedule(st, TaskPlan{Role: "code-reviewer", Mode: modeReviewFull, Reason: "review"}, "review")
	case "coder/" + modeImplement + "/" + markerQuestions, "coder/" + modeFix + "/" + markerQuestions:
		return designRecovery(st, o, "questions")
	case "coder/" + modeImplement + "/" + markerNeedsDesign, "coder/" + modeFix + "/" + markerNeedsDesign,
		"code-reviewer/" + modeReviewFull + "/" + markerNeedsDesign:
		return designRecovery(st, o, "needs_design")
	case "coder/" + modeFix + "/" + markerNeedsTriage:
		st.ResumeFix = true
		st.Diagnostic, st.NeedsTriage = o, o
		return scheduleRecovery(st, TaskPlan{Role: "triage", Mode: modeInvestigate, Reason: "needs_triage"}, false, false, true, "triage")
	case "code-reviewer/" + modeReviewFull + "/" + markerReviewApproved:
		st.LastReview = o
		st.ReviewTask, st.ReviewCommit = out.TaskID, out.Commit
		st.Complete = true
		st.Pending = nil
		st = moved(st, "complete", "review_approved")
		return st, StepResult{Action: actionComplete, Reason: "review_approved"}
	case "code-reviewer/" + modeReviewFull + "/" + markerReviewChanges:
		st.LastReview = o
		if out.Blocker == blockerEnvironment {
			return stopWith(st, "environment_blocker", "")
		}
		if out.Blocker != blockerNone {
			return stopWith(st, "invalid_field", "")
		}
		st.ResumeFix = true
		return scheduleCoder(st, modeFix, "changes_requested")
	case "triage/" + modeInvestigate + "/" + markerTriageConf, "triage/" + modeInvestigate + "/" + markerTriageProb:
		if !out.BriefCurrent {
			return stopWith(st, "stale_brief", "")
		}
		st.Triage = o
		switch out.DesignRequired {
		case designRequiredYes:
			return designRecovery(st, o, "design_required")
		case designRequiredNo:
			st.ResumeFix = true
			return scheduleCoder(st, modeFix, "triage_fix")
		}
		return stopWith(st, "invalid_field", "")
	case "triage/" + modeInvestigate + "/" + markerTriageUnconf:
		return stopWith(st, "cause_unconfirmed", "")
	}
	return stopWith(st, "invalid_marker", "")
}

// auditBindingOf binds an audit outcome to its input design revision.
func auditBindingOf(st DevFullState, out Admitted) *AuditBinding {
	return &AuditBinding{Ordinal: out.Ordinal, TaskID: out.TaskID, Commit: out.Commit, Base: out.Base, Generation: st.Generation,
		Marker: out.Marker, Design: out.DesignSHA, History: out.HistorySHA, Review: out.ReviewSHA, AdmittedSeq: out.AdmittedSeq, ConsumedSeq: out.ConsumedSeq}
}

// designRecovery routes a design blocker to the designer. A fix-mode coder
// keeps the fix context; implement-mode questions and implement-mode
// NEEDS_DESIGN resume implement after the renewed receipt. A review or
// triage escalation keeps whatever context the coder had. The last
// complete candidate stays as it was.
func designRecovery(st DevFullState, o *Outcome, reason string) (DevFullState, StepResult) {
	switch {
	case o.Role == "coder" && o.Mode == modeFix:
		st.ResumeFix = true
	case o.Role == "coder" && o.Mode == modeImplement:
		st.ResumeFix = false
	}
	if o.Role == "coder" {
		st.Diagnostic = o
	}
	st.Escalation = o
	return scheduleDesigner(st, true, reason)
}

// BuildReceipt is the receipt an explicit yes for generation would record,
// bound to the latest approving audit. The caller adds the event sequences
// and persists it before OwnerDecision consumes it.
func BuildReceipt(st DevFullState, generation int) (OwnerReceipt, error) {
	if st.Audit == nil || st.Audit.Generation != generation || generation != st.Generation || (st.Receipt != nil && st.Receipt.Valid) {
		return OwnerReceipt{}, errNamed("stale_receipt")
	}
	a := st.Audit
	return OwnerReceipt{RunID: st.RunID, Scenario: st.Scenario, Generation: generation, AuditOrdinal: a.Ordinal, AuditTask: a.TaskID,
		AuditCommit: a.Commit, Design: a.Design, History: a.History, Review: a.Review, ApprovalSeq: a.AdmittedSeq, ConsumedSeq: a.ConsumedSeq,
		Decision: "yes", Valid: true}, nil
}

// OwnerDecision applies an explicit yes or no for the open generation. yes
// takes the persisted receipt and resumes the coder: implement, or fix
// when a fix context is current. decision is the decision's sequence
// number (decisions/NN.json): every transition it causes names it as its
// source.
func OwnerDecision(st DevFullState, generation int, yes bool, rec OwnerReceipt, decision int) (DevFullState, StepResult) {
	n := len(st.Transitions)
	kind := map[bool]string{true: "yes", false: "no"}[yes]
	st, res := ownerDecision(st, generation, yes, rec)
	return fromDecision(st, n, decision, kind, generation), res
}

func ownerDecision(st DevFullState, generation int, yes bool, rec OwnerReceipt) (DevFullState, StepResult) {
	if st.Audit == nil || st.Audit.Generation != generation || generation != st.Generation || (st.Receipt != nil && st.Receipt.Valid) {
		return stopWith(st, "stale_receipt", "")
	}
	if !yes {
		return stopWith(st, "owner_no", "")
	}
	want, _ := BuildReceipt(st, generation)
	if rec.Generation != want.Generation || rec.AuditTask != want.AuditTask || rec.AuditCommit != want.AuditCommit || rec.Design != want.Design ||
		rec.History != want.History || rec.Review != want.Review || !rec.Valid || rec.Decision != "yes" {
		return stopWith(st, "stale_receipt", "")
	}
	mode := modeImplement
	if st.ResumeFix {
		mode = modeFix
	}
	st.Receipt = &rec
	st.Receipts = append(slices.Clip(st.Receipts), rec)
	return scheduleCoder(st, mode, "owner_yes")
}

// OwnerAnswer re-enters the designer after DESIGN_NEEDS_INPUT with the
// owner's bounded answer as data. It is not approval and creates no
// receipt. It reserves a design cycle and a recovery dispatch. decision
// is the answer's decision sequence number, the source of its transition.
func OwnerAnswer(st DevFullState, generation int, answer string, decision int) (DevFullState, StepResult) {
	n := len(st.Transitions)
	var res StepResult
	if st.Question == nil || generation != st.Generation {
		st, res = stopWith(st, "stale_generation", "")
	} else {
		st.Clarification = answer
		st, res = scheduleDesigner(st, true, "clarification")
	}
	return fromDecision(st, n, decision, "answer", generation), res
}

// fromDecision binds the transitions from index n on to owner decision
// seq instead of the last task. They were appended to a fresh copy, so
// the caller's records are unchanged.
func fromDecision(st DevFullState, n, seq int, kind string, generation int) DevFullState {
	for i := n; i < len(st.Transitions); i++ {
		tr := &st.Transitions[i]
		tr.FromOrdinal, tr.FromTask, tr.FromDecision = 0, "", seq
		tr.FromState = fmt.Sprintf("owner/%s:generation-%d", kind, generation)
	}
	return st
}

// moved records a transition that dispatches nothing: a pause for an owner
// or clarification gate, or completion.
func moved(st DevFullState, to, reason string) DevFullState {
	tr := TransitionRecord{Reason: reason, ToState: to, DesignBefore: st.DesignCycles, DesignAfter: st.DesignCycles, CodeBefore: st.CodeCycles,
		CodeAfter: st.CodeCycles, RecoveryBefore: st.Recovery, RecoveryAfter: st.Recovery}
	tr.FromOrdinal, tr.FromTask, tr.FromState = lastSource(st)
	st.Transitions = append(slices.Clip(st.Transitions), tr)
	return st
}

func scheduleDesigner(st DevFullState, recovery bool, reason string) (DevFullState, StepResult) {
	// The first designer is the initial draft and reserves nothing. Every
	// later designer reserves a design cycle. Clarification, questions and
	// escalation also reserve a recovery dispatch; ordinary changes
	// requested revisions do not.
	design := st.DesignerCount >= 1
	return scheduleRecovery(st, TaskPlan{Role: "designer", Mode: modeRevise, Reason: reason, Recovery: recovery}, design, false, recovery, reason)
}

func scheduleCoder(st DevFullState, mode, reason string) (DevFullState, StepResult) {
	code := st.CoderCount >= 1
	return scheduleRecovery(st, TaskPlan{Role: "coder", Mode: mode, Reason: reason}, false, code, false, reason)
}

func scheduleRecovery(st DevFullState, plan TaskPlan, design, code, recovery bool, reason string) (DevFullState, StepResult) {
	if design && st.DesignCycles+1 > maxDesignCycles || code && st.CodeCycles+1 > maxCodeCycles || recovery && st.Recovery+1 > maxRecoveryTasks {
		return stopWith(st, stopCap, proposedWork(plan))
	}
	beforeD, beforeC, beforeR := st.DesignCycles, st.CodeCycles, st.Recovery
	if design {
		st.DesignCycles++
	}
	if code {
		st.CodeCycles++
	}
	if recovery {
		st.Recovery++
	}
	plan.Recovery = recovery
	st, res := schedule(st, plan, reason)
	if res.Action != actionDispatch {
		return st, res
	}
	n := len(st.Transitions) - 1
	st.Transitions[n].DesignBefore, st.Transitions[n].CodeBefore, st.Transitions[n].RecoveryBefore = beforeD, beforeC, beforeR
	if plan.Role == "designer" {
		res.Invalidate = true
	}
	return st, res
}

// roundOf is a plan's round from its role's counter: the design cycle for
// the designer and its audit, the code cycle for the coder and its review,
// the recovery count for triage.
func roundOf(st DevFullState, role string) int {
	switch role {
	case "designer", "design-reviewer":
		return st.DesignCycles
	case "coder", "code-reviewer":
		return st.CodeCycles
	}
	return st.Recovery
}

func schedule(st DevFullState, plan TaskPlan, reason string) (DevFullState, StepResult) {
	if len(st.Tasks) >= maxWorkflowTasks {
		return stopWith(st, stopCap, proposedWork(plan))
	}
	plan.Ordinal = len(st.Tasks) + 1
	plan.ExpectedBase = st.ChainCommit
	plan.OriginalMode = modeImplement
	plan.Generation = st.Generation
	plan.DesignRound, plan.CodeRound, plan.RecoveryRound = st.DesignCycles, st.CodeCycles, st.Recovery
	plan.Round = roundOf(st, plan.Role)
	plan.Allowed = allowedOutputs(plan.Role)
	if n := len(st.Tasks); n > 0 {
		plan.Diagnostic = st.Tasks[n-1].Disposition == dispositionDiagnostic
	}
	plan.Inputs = planInputs(st, plan)
	st.Pending = &plan
	tr := TransitionRecord{Reason: reason, ToOrdinal: plan.Ordinal, ToState: plan.Role + "/" + plan.Mode, DesignBefore: st.DesignCycles,
		DesignAfter: st.DesignCycles, CodeBefore: st.CodeCycles, CodeAfter: st.CodeCycles, RecoveryBefore: st.Recovery, RecoveryAfter: st.Recovery}
	tr.FromOrdinal, tr.FromTask, tr.FromState = lastSource(st)
	st.Transitions = append(slices.Clip(st.Transitions), tr)
	return st, StepResult{Action: actionDispatch, Plan: &plan, Reason: reason, Generation: st.Generation}
}

// lastSource is the last admitted task's ordinal, ID and state.
func lastSource(st DevFullState) (int, string, string) {
	if n := len(st.Tasks); n > 0 {
		t := st.Tasks[n-1]
		return t.Ordinal, t.TaskID, t.Role + "/" + t.Mode + ":" + t.Marker
	}
	return 0, "", "start"
}

// allowedOutputs is the fixture path contract's producer allowlist.
func allowedOutputs(role string) []string {
	switch role {
	case "designer":
		return []string{"design.md", "revision-history.md", "design-questions.md (emptying only)"}
	case "design-reviewer":
		return []string{"design-review.md"}
	case "coder":
		return []string{"main.go", "main_test.go", "design-questions.md"}
	case "code-reviewer":
		return []string{"review.md"}
	case "triage":
		return []string{"rca.md", "fix.md (TRIAGE_CONFIRMED or TRIAGE_PROBABLE only)"}
	}
	return nil
}

// planInputs names the immutable inputs a plan refers to; the supervisor
// adds their digests from the admitted trees.
func planInputs(st DevFullState, plan TaskPlan) []PlanInput {
	var in []PlanInput
	add := func(kind string, o *Outcome, path string) {
		if o != nil {
			in = append(in, PlanInput{Kind: kind, Ordinal: o.Ordinal, Task: o.TaskID, Commit: o.Commit, Path: path})
		}
	}
	switch plan.Role {
	case "design-reviewer":
		if a := st.LastAudit; a != nil {
			in = append(in, PlanInput{Kind: "prior-audit", Ordinal: a.Ordinal, Task: a.TaskID, Commit: a.Commit, Path: "design-review.md", SHA256: a.Review})
		}
	case "designer":
		if a := st.LastAudit; a != nil {
			in = append(in, PlanInput{Kind: "prior-audit", Ordinal: a.Ordinal, Task: a.TaskID, Commit: a.Commit, Path: "design-review.md", SHA256: a.Review})
		}
		switch plan.Reason {
		case "clarification":
			add("questions", st.Question, "")
		case "questions":
			add("questions", st.Escalation, "design-questions.md")
			add("escalation", st.Escalation, "")
		case "needs_design":
			add("escalation", st.Escalation, "")
		case "design_required":
			add("brief", st.Triage, "fix.md")
			add("rca", st.Triage, "rca.md")
		}
		if st.Diagnostic != nil && plan.Reason != "changes_requested" && plan.Reason != "clarification" {
			add("partial-work", st.Diagnostic, "")
		}
	case "coder":
		if st.Receipt != nil && st.Receipt.Valid {
			in = append(in, PlanInput{Kind: "approving-audit", Ordinal: st.Receipt.AuditOrdinal, Task: st.Receipt.AuditTask, Commit: st.Receipt.AuditCommit,
				Path: "design-review.md", SHA256: st.Receipt.Review})
		}
		switch plan.Reason {
		case "changes_requested":
			add("review", st.LastReview, "review.md")
		case "triage_fix":
			add("review", st.LastReview, "review.md")
			add("brief", st.Triage, "fix.md")
		case "owner_yes":
			if plan.Mode == modeFix {
				add("review", st.LastReview, "review.md")
			}
		}
		if st.Diagnostic != nil && plan.Reason != "changes_requested" {
			add("partial-work", st.Diagnostic, "")
		}
		if st.CandidateCommit != "" {
			in = append(in, PlanInput{Kind: "last-candidate", Task: st.CandidateTask, Commit: st.CandidateCommit})
		}
	case "code-reviewer":
		in = append(in, PlanInput{Kind: "candidate", Task: st.CandidateTask, Commit: st.CandidateCommit})
		add("prior-review", st.LastReview, "review.md")
	case "triage":
		add("findings", st.NeedsTriage, "")
		add("review", st.LastReview, "review.md")
	}
	return in
}

// recordTask appends the admitted task and advances the chain: every
// admitted publication, report-only or diagnostic, is the next parent.
func recordTask(st DevFullState, out Admitted) DevFullState {
	rec := TaskRecord{Ordinal: out.Ordinal, TaskID: out.TaskID, Role: out.Role, Mode: out.Mode, Commit: out.Commit, Tree: out.Tree,
		Marker: out.Marker, Selection: out.Selection, Workspace: out.Workspace, Instance: out.Instance, Base: out.Base,
		Disposition: dispositionOf(out.Role, out.Marker), Artifacts: out.Artifacts, CreatedAt: out.CreatedAt, FinishedAt: out.FinishedAt,
		AdmittedSeq: out.AdmittedSeq, ObservedSeq: out.ObservedSeq, ConsumedSeq: out.ConsumedSeq}
	if st.Pending != nil {
		rec.Round, rec.Reason = st.Pending.Round, st.Pending.Reason
		if rec.Base == "" {
			rec.Base = st.Pending.ExpectedBase
		}
	}
	st.Tasks = append(slices.Clip(st.Tasks), rec)
	switch out.Role {
	case "designer":
		st.DesignerCount++
	case "coder":
		st.CoderCount++
	}
	if out.Commit != "" {
		st.ChainCommit = out.Commit
	}
	st.Pending = nil
	return st
}

// dispositionOf classifies a publication: a complete coder candidate is
// implementation-ready, any other coder result diagnostic, everything else
// a report or planning artifact.
func dispositionOf(role, marker string) string {
	switch {
	case role == "coder" && (marker == markerImplComplete || marker == markerFixesComplete):
		return dispositionReady
	case role == "coder":
		return dispositionDiagnostic
	}
	return dispositionReport
}

func stopWith(st DevFullState, code, reason string) (DevFullState, StepResult) {
	st.StopCode = code
	st.Pending = nil
	tr := TransitionRecord{Reason: cmpOr(reason, code), Stop: code, DesignBefore: st.DesignCycles, DesignAfter: st.DesignCycles,
		CodeBefore: st.CodeCycles, CodeAfter: st.CodeCycles, RecoveryBefore: st.Recovery, RecoveryAfter: st.Recovery, ToState: "stopped"}
	tr.FromOrdinal, tr.FromTask, tr.FromState = lastSource(st)
	st.Transitions = append(slices.Clip(st.Transitions), tr)
	return st, StepResult{Action: actionStop, Stop: code, Reason: reason}
}

func proposedWork(p TaskPlan) string {
	return fmt.Sprintf("%s/%s %s", p.Role, p.Mode, p.Reason)
}

// InitialPlan is the first designer draft. It reserves no cycle.
func InitialPlan(base string) (DevFullState, StepResult) {
	st := DevFullState{Scenario: scenarioFull, InitialCommit: base, ChainCommit: base}
	return schedule(st, TaskPlan{Role: "designer", Mode: modeDraft, Reason: "initial"}, "initial")
}

// InvalidateReceipt marks the active receipt invalid. A design dispatch
// does this before its result, and the receipt is never reactivated. The
// given state's records are not changed.
func InvalidateReceipt(st DevFullState) DevFullState {
	if st.Receipt != nil {
		r := *st.Receipt
		r.Valid = false
		st.Receipt = &r
	}
	if n := len(st.Receipts); n > 0 {
		st.Receipts = slices.Clone(st.Receipts)
		st.Receipts[n-1].Valid = false
	}
	return st
}

// SameFrozen reports whether the plan's role still matches the frozen row.
func SameFrozen(flow DevFullFlow, role string, got FlowRole) bool {
	want, ok := flow.Role(role)
	return ok && want == got
}

// GoalHeader is the canonical correlation header. Payload stays empty.
func GoalHeader(runID string, plan TaskPlan, inputCommit string) string {
	return strings.Join([]string{
		"run_id: " + runID,
		"scenario: " + scenarioFull,
		"ordinal: " + itoa(plan.Ordinal),
		"role: " + plan.Role,
		"mode: " + plan.Mode,
		"round: " + itoa(plan.Round),
		"input_commit: " + inputCommit,
	}, "\n")
}

// GoalIdentity is a parsed canonical goal header.
type GoalIdentity struct {
	RunID, Scenario, Role, Mode, Input string
	Ordinal, Round                     int
}

// ParseGoalHeader reads the canonical header. A mismatch fails closed.
func ParseGoalHeader(goal string) (GoalIdentity, bool) {
	lines := strings.SplitN(goal, "\n", 8)
	want := []string{"run_id: ", "scenario: ", "ordinal: ", "role: ", "mode: ", "round: ", "input_commit: "}
	if len(lines) < len(want) {
		return GoalIdentity{}, false
	}
	vals := make([]string, len(want))
	for i, p := range want {
		v, ok := strings.CutPrefix(lines[i], p)
		if !ok {
			return GoalIdentity{}, false
		}
		vals[i] = v
	}
	ordinal, ok1 := atoi(vals[2])
	round, ok2 := atoi(vals[5])
	if !ok1 || !ok2 || vals[1] != scenarioFull || itoa(ordinal) != vals[2] || itoa(round) != vals[5] {
		return GoalIdentity{}, false
	}
	return GoalIdentity{RunID: vals[0], Scenario: vals[1], Ordinal: ordinal, Role: vals[3], Mode: vals[4], Round: round, Input: vals[6]}, true
}

func atoi(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
