package reale2e

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
)

// The light scenario's transition policy (design 12a-dev-flow-light, Light
// transitions; FP-3). NextLight is pure like Next: it never dispatches and
// never mutates the state it is given. The light attempt runs one initial
// triage, the frozen coder in defect mode and the executing light review,
// at most two additional fix/review pairs and at most two recovery
// dispatches (a re-triage or the interrupted coder's extra re-entry), so at
// most nine workflow tasks. OriginalMode is defect throughout and the
// initial commit is always the buggy seed. Escalations stop the attempt
// with a next-step handoff: there is no designer in a light attempt.

const (
	// modeDefect is the light coder's defect mode; modeReviewLight is the
	// light code review.
	modeDefect      = "defect"
	modeReviewLight = "light"
	// maxLightTasks is the light attempt's workflow task cap (the common
	// structural ordinal ceiling stays maxWorkflowTasks).
	maxLightTasks = 9
	// stopNeedsDesign is a light escalation's stop: a qualified brief or a
	// coder or review asking for design.
	stopNeedsDesign = "needs_design"
)

// InitialPlanFor is scenario's first plan on base: the full flow's
// initial designer draft, or the light attempt's initial triage
// investigation of the seed (OriginalMode defect). It reserves nothing.
func InitialPlanFor(scenario, base string) (DevFullState, StepResult) {
	if scenario != scenarioLight {
		return InitialPlan(base)
	}
	st := DevFullState{Scenario: scenarioLight, InitialCommit: base, ChainCommit: base}
	return scheduleLight(st, TaskPlan{Role: "triage", Mode: modeInvestigate, Reason: "initial"}, false, false, "initial")
}

// NextLight selects the light transition for an admitted outcome. The
// caller has already checked publication, binding and the marker parse.
func NextLight(st DevFullState, out Admitted) (DevFullState, StepResult) {
	if st.Scenario != scenarioLight || st.Pending == nil || out.Ordinal != st.Pending.Ordinal || out.Role != st.Pending.Role ||
		out.Mode != st.Pending.Mode {
		return stopWith(st, "invalid_transition", "")
	}
	if !lightMarkerAllowed(out.Role, out.Mode, out.Marker) {
		return stopWith(st, "invalid_marker", "")
	}
	st = recordTask(st, out)
	o := &Outcome{Ordinal: out.Ordinal, TaskID: out.TaskID, Role: out.Role, Mode: out.Mode, Marker: out.Marker, Commit: out.Commit,
		Final: out.Final, FinalSHA: sha256Hex([]byte(out.Final))}
	switch out.Role + "/" + out.Mode + "/" + out.Marker {
	case "triage/" + modeInvestigate + "/" + markerTriageConf, "triage/" + modeInvestigate + "/" + markerTriageProb:
		if !out.BriefCurrent {
			return stopWith(st, "stale_brief", "")
		}
		st.Triage = o
		switch out.DesignRequired {
		case designRequiredYes:
			st.Escalation = o
			return stopWith(st, stopNeedsDesign, "design_required")
		case designRequiredNo:
		default:
			return stopWith(st, "invalid_field", "")
		}
		if st.CoderCount == 0 {
			// The initial qualified brief: the frozen coder in defect mode
			// (the ordinary pair, nothing reserved).
			return scheduleLight(st, TaskPlan{Role: "coder", Mode: modeDefect, Reason: "triage_defect"}, false, false, "triage_defect")
		}
		// A recovery brief resumes the interrupted coder: defect when no
		// successful candidate exists yet, otherwise fix with the original
		// review context. The re-entry is one recovery dispatch.
		mode := modeFix
		if st.CandidateCommit == "" {
			mode = modeDefect
		}
		return scheduleLight(st, TaskPlan{Role: "coder", Mode: mode, Reason: "triage_resume"}, false, true, "triage_resume")
	case "triage/" + modeInvestigate + "/" + markerTriageUnconf:
		return stopWith(st, "cause_unconfirmed", "")
	case "triage/" + modeInvestigate + "/" + markerTriageBlocked:
		return stopWith(st, "obstacle", "")
	case "coder/" + modeDefect + "/" + markerImplComplete, "coder/" + modeFix + "/" + markerFixesComplete:
		st.CandidateTask, st.CandidateCommit = out.TaskID, out.Commit
		st.Diagnostic, st.NeedsTriage = nil, nil
		return scheduleLight(st, TaskPlan{Role: "code-reviewer", Mode: modeReviewLight, Reason: "review"}, false, false, "review")
	case "coder/" + modeDefect + "/" + markerQuestions, "coder/" + modeFix + "/" + markerQuestions:
		// An unsupported cause: re-triage it, keeping the coder's partial
		// work and its questions.
		st.Diagnostic, st.NeedsTriage = o, o
		return scheduleLight(st, TaskPlan{Role: "triage", Mode: modeInvestigate, Reason: "questions"}, false, true, "questions")
	case "coder/" + modeFix + "/" + markerNeedsTriage:
		st.Diagnostic, st.NeedsTriage = o, o
		return scheduleLight(st, TaskPlan{Role: "triage", Mode: modeInvestigate, Reason: "needs_triage"}, false, true, "needs_triage")
	case "coder/" + modeDefect + "/" + markerNeedsDesign, "coder/" + modeFix + "/" + markerNeedsDesign,
		"code-reviewer/" + modeReviewLight + "/" + markerNeedsDesign:
		st.Escalation = o
		if out.Role == "coder" {
			st.Diagnostic = o
		}
		return stopWith(st, stopNeedsDesign, "needs_design")
	case "coder/" + modeDefect + "/" + markerBlocked, "coder/" + modeFix + "/" + markerBlocked:
		return stopWith(st, "obstacle", "")
	case "code-reviewer/" + modeReviewLight + "/" + markerReviewApproved:
		st.LastReview = o
		st.ReviewTask, st.ReviewCommit = out.TaskID, out.Commit
		st.Complete = true
		st.Pending = nil
		st = moved(st, "complete", "review_approved")
		return st, StepResult{Action: actionComplete, Reason: "review_approved"}
	case "code-reviewer/" + modeReviewLight + "/" + markerReviewChanges:
		st.LastReview = o
		switch out.Blocker {
		case blockerEnvironment:
			return stopWith(st, "environment_blocker", "")
		case blockerNone:
		default:
			return stopWith(st, "invalid_field", "")
		}
		return scheduleLight(st, TaskPlan{Role: "coder", Mode: modeFix, Reason: "changes_requested"}, true, false, "changes_requested")
	}
	return stopWith(st, "invalid_marker", "")
}

// lightMarkerAllowed reports whether marker is legal for a light role and
// mode. NEEDS_TRIAGE is invalid in defect mode.
func lightMarkerAllowed(role, mode, marker string) bool {
	switch role + "/" + mode {
	case "coder/" + modeDefect:
		return marker == markerImplComplete || marker == markerQuestions || marker == markerNeedsDesign || marker == markerBlocked
	case "coder/" + modeFix:
		return marker == markerFixesComplete || marker == markerQuestions || marker == markerNeedsDesign || marker == markerNeedsTriage || marker == markerBlocked
	case "code-reviewer/" + modeReviewLight:
		return marker == markerReviewApproved || marker == markerReviewChanges || marker == markerNeedsDesign
	case "triage/" + modeInvestigate:
		return marker == markerTriageConf || marker == markerTriageProb || marker == markerTriageUnconf || marker == markerTriageBlocked
	}
	return false
}

// scheduleLight reserves before the dispatch (code: one additional
// fix/review pair; recovery: one recovery dispatch), never refunding, and
// schedules plan within the light task cap. An exhausted cap stops with
// cap_exhausted naming the next task.
func scheduleLight(st DevFullState, plan TaskPlan, code, recovery bool, reason string) (DevFullState, StepResult) {
	if code && st.CodeCycles+1 > maxCodeCycles || recovery && st.Recovery+1 > maxRecoveryTasks || len(st.Tasks) >= maxLightTasks {
		return stopWith(st, stopCap, proposedWork(plan))
	}
	beforeC, beforeR := st.CodeCycles, st.Recovery
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
	st.Transitions[n].CodeBefore, st.Transitions[n].RecoveryBefore = beforeC, beforeR
	return st, res
}

// lightAllowedOutputs is the light producer allowlist.
func lightAllowedOutputs(role string) []string {
	switch role {
	case "coder":
		return []string{"main.go", "main_test.go", "design-questions.md (only when blocked; a successful publication may only clear it)"}
	case "code-reviewer":
		return []string{"review.md"}
	case "triage":
		return []string{"rca.md", "fix.md (TRIAGE_CONFIRMED or TRIAGE_PROBABLE only)"}
	}
	return nil
}

// lightPlanInputs names a light plan's immutable inputs: the current
// qualified brief and rca with their originating triage task and commit,
// the last review and its findings, the diagnostic partial work and the
// candidate.
func lightPlanInputs(st DevFullState, plan TaskPlan) []PlanInput {
	var in []PlanInput
	add := func(kind string, o *Outcome, path string) {
		if o != nil {
			in = append(in, PlanInput{Kind: kind, Ordinal: o.Ordinal, Task: o.TaskID, Commit: o.Commit, Path: path})
		}
	}
	switch plan.Role {
	case "coder":
		add("brief", st.Triage, "fix.md")
		add("rca", st.Triage, "rca.md")
		if plan.Mode == modeFix {
			add("review", st.LastReview, "review.md")
			add("findings", st.LastReview, "")
		}
		add("partial-work", st.Diagnostic, "")
		if st.CandidateCommit != "" {
			in = append(in, PlanInput{Kind: "last-candidate", Task: st.CandidateTask, Commit: st.CandidateCommit})
		}
	case "code-reviewer":
		in = append(in, PlanInput{Kind: "candidate", Task: st.CandidateTask, Commit: st.CandidateCommit})
		add("brief", st.Triage, "fix.md")
		add("rca", st.Triage, "rca.md")
		add("prior-review", st.LastReview, "review.md")
	case "triage":
		add("findings", st.NeedsTriage, "")
		if st.NeedsTriage != nil && st.NeedsTriage.Marker == markerQuestions {
			add("questions", st.NeedsTriage, "design-questions.md")
		}
		add("review", st.LastReview, "review.md")
		add("prior-brief", st.Triage, "fix.md")
		add("prior-rca", st.Triage, "rca.md")
	}
	return in
}

// rootCauseRE is a light brief's root-cause reference line (design
// 12a-dev-flow-light, Triage admission): exact case and spacing, no
// indentation or trailing text.
var rootCauseRE = regexp.MustCompile(`^Root cause: (main\.go|main_test\.go):([1-9][0-9]*)$`)

// errRootCause is a brief without exactly one resolving root-cause
// reference.
var errRootCause = errNamed("root_cause_invalid")

// parseRootCause returns the single root-cause reference of a qualified
// light fix.md: exactly one line outside code fences (the shipped field
// parser's fence handling) matching rootCauseRE, whose positive decimal
// line number (parsed without overflow) names an existing 1-based line of
// that file in the immutable initial seed tree (a trailing LF does not
// make an extra line). Every other line, a nonmatching "Root cause:" line
// included, is prose and ignored, never malformed: it neither counts nor
// rejects.
func parseRootCause(fix string, seed map[string]TreeFile) (string, int, error) {
	path, line, found := "", 0, 0
	for _, l := range scanLines(fix) {
		if l.fenced {
			continue
		}
		m := rootCauseRE.FindStringSubmatch(l.text)
		if m == nil {
			continue
		}
		found++
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0, fmt.Errorf("%w: the root-cause line number overflows", errRootCause)
		}
		path, line = m[1], n
	}
	switch {
	case found == 0:
		return "", 0, fmt.Errorf("%w: no root-cause reference line", errRootCause)
	case found > 1:
		return "", 0, fmt.Errorf("%w: %d root-cause reference lines", errRootCause, found)
	}
	f, ok := seed[path]
	if !ok || line > fileLineCount(f.Content) {
		return "", 0, fmt.Errorf("%w: %s:%d is not a line of the seed", errRootCause, path, line)
	}
	return path, line, nil
}

// fileLineCount is a file's 1-based line count: a final LF ends the last
// line without adding one; an unterminated last line counts.
func fileLineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}
