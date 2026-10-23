package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

func repoRoot(t testing.TB) string {
	t.Helper()
	repoRootOnce.Do(func() { repoRootOnce.root = testkit.MustRepoRoot(t) })
	return repoRootOnce.root
}

// commitOf is a deterministic 40-hex commit for label.
func commitOf(label string) string { return sha256Hex([]byte(label))[:40] }

// admit is the admitted outcome of st's pending plan with marker.
func admit(st DevFullState, marker string) Admitted {
	p := st.Pending
	label := itoa(p.Ordinal) + marker
	out := Admitted{Ordinal: p.Ordinal, TaskID: "t_" + sha256Hex([]byte(label))[:32], Role: p.Role, Mode: p.Mode, Marker: marker,
		Commit: commitOf(label), Base: p.ExpectedBase, Final: "STATUS: " + marker + "\n",
		DesignSHA: sha256Hex([]byte("design" + itoa(st.Generation))), HistorySHA: strings.Repeat("b", 64), ReviewSHA: sha256Hex([]byte(label)),
		AdmittedSeq: 10 * p.Ordinal, ConsumedSeq: 10*p.Ordinal + 1}
	if p.Role == "code-reviewer" {
		out.Blocker = blockerNone
	}
	if marker == markerTriageConf || marker == markerTriageProb {
		out.DesignRequired, out.BriefCurrent = designRequiredNo, true
	}
	return out
}

func drive(t testing.TB, st DevFullState, out Admitted) (DevFullState, StepResult) {
	t.Helper()
	next, res := Next(st, out)
	if res.Invalidate {
		next = InvalidateReceipt(next)
	}
	return next, res
}

// approve gives the owner's yes for st's current generation.
func approve(t testing.TB, st DevFullState) (DevFullState, StepResult) {
	t.Helper()
	rec, err := BuildReceipt(st, st.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return OwnerDecision(st, st.Generation, true, rec, len(st.Receipts)+1)
}

// script drives st through markers (an "owner" step approves).
func script(t testing.TB, markers ...string) (DevFullState, StepResult) {
	t.Helper()
	st, res := InitialPlan(commitOf("seed"))
	for _, m := range markers {
		if m == "owner" {
			st, res = approve(t, st)
			continue
		}
		if st.Pending == nil {
			t.Fatalf("no pending plan for %s (last %+v)", m, res)
		}
		st, res = drive(t, st, admit(st, m))
	}
	return st, res
}

func TestDevFullTransitions(t *testing.T) {
	t.Run("marker_table", func(t *testing.T) {
		all := []string{markerDesignDraft, markerNeedsInput, markerDesignChanges, markerDesignApproved, markerImplComplete, markerFixesComplete,
			markerQuestions, markerNeedsDesign, markerNeedsTriage, markerBlocked, markerReviewApproved, markerReviewChanges, markerTriageConf,
			markerTriageProb, markerTriageUnconf, markerTriageBlocked, "PASS", ""}
		valid := map[string][]string{
			"designer/draft":        {markerDesignDraft, markerNeedsInput, markerBlocked},
			"designer/revise":       {markerDesignDraft, markerNeedsInput, markerBlocked},
			"design-reviewer/audit": {markerDesignChanges, markerDesignApproved},
			"coder/implement":       {markerImplComplete, markerQuestions, markerNeedsDesign, markerBlocked},
			"coder/fix":             {markerFixesComplete, markerQuestions, markerNeedsDesign, markerNeedsTriage, markerBlocked},
			"code-reviewer/full":    {markerReviewApproved, markerReviewChanges, markerNeedsDesign},
			"triage/investigate":    {markerTriageConf, markerTriageProb, markerTriageUnconf, markerTriageBlocked},
			"designer/audit":        {},
			"coder/full":            {},
		}
		for rm, ok := range valid {
			role, mode, _ := strings.Cut(rm, "/")
			for _, m := range all {
				if got := markerAllowed(role, mode, m); got != slices.Contains(ok, m) {
					t.Errorf("markerAllowed(%s, %s) = %t", rm, m, got)
				}
			}
		}
	})
	t.Run("routes", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			markers []string
			action  string
			want    string // next role/mode, pause kind or stop code
		}{
			{"draft", []string{markerDesignDraft}, actionDispatch, "design-reviewer/audit"},
			{"needs_input", []string{markerNeedsInput}, actionPause, pauseAnswer},
			{"designer_blocked", []string{markerBlocked}, actionStop, "obstacle"},
			{"changes", []string{markerDesignDraft, markerDesignChanges}, actionDispatch, "designer/revise"},
			{"approved", []string{markerDesignDraft, markerDesignApproved}, actionPause, pauseOwner},
			{"owner_yes", []string{markerDesignDraft, markerDesignApproved, "owner"}, actionDispatch, "coder/implement"},
			{"impl", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete}, actionDispatch, "code-reviewer/full"},
			{"approved_review", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewApproved}, actionComplete, ""},
			{"review_changes", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges}, actionDispatch, "coder/fix"},
			{"impl_questions", []string{markerDesignDraft, markerDesignApproved, "owner", markerQuestions}, actionDispatch, "designer/revise"},
			{"impl_needs_design", []string{markerDesignDraft, markerDesignApproved, "owner", markerNeedsDesign}, actionDispatch, "designer/revise"},
			{"impl_needs_triage", []string{markerDesignDraft, markerDesignApproved, "owner", markerNeedsTriage}, actionStop, "invalid_marker"},
			{"coder_blocked", []string{markerDesignDraft, markerDesignApproved, "owner", markerBlocked}, actionStop, "obstacle"},
			{"review_needs_design", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerNeedsDesign}, actionDispatch, "designer/revise"},
			{"fix_needs_triage", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges, markerNeedsTriage},
				actionDispatch, "triage/investigate"},
			{"fix_questions", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges, markerQuestions},
				actionDispatch, "designer/revise"},
			{"triage_fix", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges, markerNeedsTriage,
				markerTriageConf}, actionDispatch, "coder/fix"},
			{"triage_unconfirmed", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges, markerNeedsTriage,
				markerTriageUnconf}, actionStop, "cause_unconfirmed"},
			{"triage_blocked", []string{markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges, markerNeedsTriage,
				markerTriageBlocked}, actionStop, "obstacle"},
			{"wrong_marker", []string{markerImplComplete}, actionStop, "invalid_marker"},
		} {
			t.Run(c.name, func(t *testing.T) {
				st, res := script(t, c.markers...)
				got := ""
				switch res.Action {
				case actionDispatch:
					got = st.Pending.Role + "/" + st.Pending.Mode
				case actionPause:
					got = res.Pause
				case actionStop:
					got = res.Stop
				}
				if res.Action != c.action || got != c.want {
					t.Fatalf("%s: %s %q, want %s %q", c.name, res.Action, got, c.action, c.want)
				}
			})
		}
	})
	t.Run("environment_blocker", func(t *testing.T) {
		st, _ := script(t, markerDesignDraft, markerDesignApproved, "owner", markerImplComplete)
		env := admit(st, markerReviewChanges)
		env.Blocker = blockerEnvironment
		if _, res := drive(t, st, env); res.Stop != "environment_blocker" || st.CodeCycles != 0 {
			t.Fatalf("environment %+v", res)
		}
		bad := admit(st, markerReviewChanges)
		bad.Blocker = ""
		if _, res := drive(t, st, bad); res.Stop != "invalid_field" {
			t.Fatalf("missing blocker %+v", res)
		}
	})
	t.Run("caps", func(t *testing.T) {
		// Design: the initial pair plus two additional cycles.
		st, res := script(t, markerDesignDraft, markerDesignChanges, markerDesignDraft, markerDesignChanges, markerDesignDraft)
		if res.Action != actionDispatch || st.DesignCycles != 2 {
			t.Fatalf("second revision %+v", res)
		}
		st, res = drive(t, st, admit(st, markerDesignChanges))
		if res.Stop != stopCap || res.Reason != "designer/revise changes_requested" || st.Transitions[len(st.Transitions)-1].Stop != stopCap {
			t.Fatalf("design cap %+v", res)
		}
		// Code: two additional coder cycles.
		st, _ = script(t, markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges, markerFixesComplete,
			markerReviewChanges, markerFixesComplete)
		if st.CodeCycles != 2 {
			t.Fatal(st.CodeCycles)
		}
		if _, res = drive(t, st, admit(st, markerReviewChanges)); res.Stop != stopCap || !strings.Contains(res.Reason, "coder/fix") {
			t.Fatalf("code cap %+v", res)
		}
		// Recovery: two dispatches; a triage that requests design needs a
		// second recovery slot for its designer.
		st, _ = script(t, markerDesignDraft, markerDesignApproved, "owner", markerQuestions, markerDesignDraft, markerDesignApproved, "owner",
			markerImplComplete, markerReviewChanges, markerNeedsTriage)
		if st.Recovery != 2 || st.Pending.Role != "triage" {
			t.Fatalf("recovery %d", st.Recovery)
		}
		yes := admit(st, markerTriageConf)
		yes.DesignRequired = designRequiredYes
		if _, res = drive(t, st, yes); res.Stop != stopCap || !strings.Contains(res.Reason, "designer/revise design_required") {
			t.Fatalf("recovery cap %+v", res)
		}
		// The workflow ledger never exceeds fourteen slots.
		full := st
		full.Tasks = make([]TaskRecord, maxWorkflowTasks)
		if _, res = schedule(full, TaskPlan{Role: "coder", Mode: modeFix}, "x"); res.Stop != stopCap {
			t.Fatalf("slot cap %+v", res)
		}
	})
	t.Run("counters_and_rounds", func(t *testing.T) {
		st, _ := script(t, markerDesignDraft, markerDesignApproved, "owner", markerNeedsDesign, markerDesignDraft)
		if st.DesignCycles != 1 || st.Recovery != 1 || st.Pending.Round != 1 || st.Pending.Role != "design-reviewer" {
			t.Fatalf("re-entry counters %+v", st.Pending)
		}
		st, _ = drive(t, st, admit(st, markerDesignApproved))
		st, _ = approve(t, st)
		// Implement-mode NEEDS_DESIGN resumes implement with the renewed
		// receipt, charged one code cycle; the counters never reset.
		if st.Pending.Mode != modeImplement || st.CodeCycles != 1 || st.Pending.Round != 1 || st.Generation != 2 || len(st.Receipts) != 2 ||
			st.Receipts[0].Valid || !st.Receipt.Valid {
			t.Fatalf("resume %+v %+v", st.Pending, st.Receipts)
		}
		st, _ = drive(t, st, admit(st, markerImplComplete))
		if st.Pending.Round != 1 || st.Pending.Role != "code-reviewer" {
			t.Fatalf("review round %+v", st.Pending)
		}
		st, _ = drive(t, st, admit(st, markerReviewChanges))
		st, _ = drive(t, st, admit(st, markerNeedsTriage))
		if st.Pending.Role != "triage" || st.Pending.Round != 2 || st.Recovery != 2 {
			t.Fatalf("triage round %+v", st.Pending)
		}
	})
	t.Run("candidate_identity", func(t *testing.T) {
		st, _ := script(t, markerDesignDraft, markerDesignApproved, "owner", markerImplComplete)
		cand := st.CandidateCommit
		st, _ = drive(t, st, admit(st, markerReviewChanges))
		st, _ = drive(t, st, admit(st, markerQuestions))
		if st.CandidateCommit != cand || st.Diagnostic == nil || st.Pending.Role != "designer" {
			t.Fatalf("a diagnostic replaced the candidate: %s", st.CandidateCommit)
		}
		st, _ = drive(t, st, admit(st, markerDesignDraft))
		st, _ = drive(t, st, admit(st, markerDesignApproved))
		st, _ = approve(t, st)
		if st.Pending.Mode != modeFix || st.Pending.Role != "coder" {
			t.Fatalf("fix context %+v", st.Pending)
		}
		st, _ = drive(t, st, admit(st, markerFixesComplete))
		fixed := st.CandidateCommit
		st, res := drive(t, st, admit(st, markerReviewApproved))
		if res.Action != actionComplete || st.CandidateCommit != fixed || st.ReviewCommit == fixed || st.ReviewCommit == "" {
			t.Fatalf("final identities candidate %s review %s", st.CandidateCommit, st.ReviewCommit)
		}
	})
	t.Run("diagnostic_base", func(t *testing.T) {
		st, _ := script(t, markerDesignDraft, markerDesignApproved, "owner", markerQuestions)
		if !st.Pending.Diagnostic || st.Tasks[2].Disposition != dispositionDiagnostic {
			t.Fatalf("diagnostic %+v", st.Pending)
		}
		ready, _ := script(t, markerDesignDraft, markerDesignApproved, "owner", markerImplComplete)
		if ready.Pending.Diagnostic || ready.Tasks[2].Disposition != dispositionReady {
			t.Fatal("a complete candidate is diagnostic")
		}
	})
	t.Run("stale_brief", func(t *testing.T) {
		st, _ := script(t, markerDesignDraft, markerDesignApproved, "owner", markerImplComplete, markerReviewChanges, markerNeedsTriage)
		old := admit(st, markerTriageProb)
		old.BriefCurrent = false
		if _, res := drive(t, st, old); res.Stop != "stale_brief" {
			t.Fatalf("an inherited brief with a valid field resumed the fix: %+v", res)
		}
	})
	t.Run("purity", func(t *testing.T) {
		st, _ := script(t, markerDesignDraft, markerDesignApproved, "owner", markerImplComplete)
		before, _ := json.Marshal(st)
		a, _ := drive(t, st, admit(st, markerReviewChanges))
		b, _ := drive(t, st, admit(st, markerReviewApproved))
		after, _ := json.Marshal(st)
		if !bytes.Equal(before, after) || a.Tasks[3].Marker != markerReviewChanges || b.Tasks[3].Marker != markerReviewApproved {
			t.Fatal("Next changed the state it was given")
		}
		q, _ := drive(t, st, admit(st, markerReviewChanges))
		q, _ = drive(t, q, admit(q, markerQuestions))
		if !st.Receipt.Valid || q.Receipt.Valid {
			t.Fatal("invalidation changed the earlier state's receipt")
		}
	})
}

func TestDevFullFieldGrammar(t *testing.T) {
	ok := []string{
		"Reviewed.\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n",
		"Reviewed.\r\nCALLSHEET_REVIEW_BLOCKER: none\r\nSTATUS: REVIEW_APPROVED\r\n",
		"```\nCALLSHEET_REVIEW_BLOCKER: environment\n```\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n\n",
		"> CALLSHEET_REVIEW_BLOCKER: environment\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED",
	}
	for _, text := range ok {
		if v, err := parseReviewBlocker(text, text, markerReviewApproved); err != nil || v != blockerNone {
			t.Fatalf("rejected %q: %v", text, err)
		}
	}
	env := "CALLSHEET_REVIEW_BLOCKER: environment\nSTATUS: REVIEW_CHANGES_REQUESTED\n"
	if v, err := parseReviewBlocker(env, env, markerReviewChanges); err != nil || v != blockerEnvironment {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"approval_env":  "CALLSHEET_REVIEW_BLOCKER: environment\nSTATUS: REVIEW_APPROVED\n",
		"missing":       "STATUS: REVIEW_APPROVED\n",
		"blank_between": "CALLSHEET_REVIEW_BLOCKER: none\n\nSTATUS: REVIEW_APPROVED\n",
		"space_between": "CALLSHEET_REVIEW_BLOCKER: none\n  \nSTATUS: REVIEW_APPROVED\n",
		"misplaced":     "CALLSHEET_REVIEW_BLOCKER: none\nmore text\nSTATUS: REVIEW_APPROVED\n",
		"wrong_case":    "CALLSHEET_REVIEW_BLOCKER: None\nSTATUS: REVIEW_APPROVED\n",
		"lower_key":     "callsheet_review_blocker: none\nSTATUS: REVIEW_APPROVED\n",
		"two_spaces":    "CALLSHEET_REVIEW_BLOCKER:  none\nSTATUS: REVIEW_APPROVED\n",
		"trailing":      "CALLSHEET_REVIEW_BLOCKER: none \nSTATUS: REVIEW_APPROVED\n",
		"heading":       "# CALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n",
		"table":         "| CALLSHEET_REVIEW_BLOCKER: none |\nSTATUS: REVIEW_APPROVED\n",
		"list":          "- CALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n",
		"quoted":        "> CALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n",
		"indented":      " CALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n",
		"fenced":        "```\nCALLSHEET_REVIEW_BLOCKER: none\n```\nSTATUS: REVIEW_APPROVED\n",
		"tilde_fenced":  "~~~\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n~~~\n",
		"unclosed":      "```\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n",
		"duplicate":     "CALLSHEET_REVIEW_BLOCKER: none\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n",
		"bare_cr":       "CALLSHEET_REVIEW_BLOCKER: none\rSTATUS: REVIEW_APPROVED\n",
	} {
		if _, err := parseReviewBlocker(bad, bad, markerReviewApproved); err == nil {
			t.Errorf("%s accepted %q", name, bad)
		}
	}
	good := "CALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_CHANGES_REQUESTED\n"
	if _, err := parseReviewBlocker(good, env, markerReviewChanges); err == nil {
		t.Fatal("conflicting values accepted")
	}
	for _, text := range []string{"note\nDesign required: yes\n", "Design required: no\r\n", "```\nDesign required: yes\n```\nDesign required: no\n",
		"~~~~\n~~~\nDesign required: yes\n~~~~\nDesign required: no\n"} {
		if _, err := parseDesignRequired(text); err != nil {
			t.Fatalf("rejected %q", text)
		}
	}
	for name, bad := range map[string]string{
		"empty": "", "question": "Design required?: yes\n", "case": "Design required: Yes\n", "key_case": "design required: yes\n",
		"both": "Design required: yes\nDesign required: no\n", "heading": "# Design required: yes\n", "fenced": "```\nDesign required: yes\n```\n",
		"tilde": "~~~\nDesign required: yes\n~~~\n", "unclosed": "~~~\nDesign required: yes\n", "trailing": "Design required: yes \n",
		"spaces": "Design required:  yes\n", "table": "| Design required: yes |\n", "item": "* Design required: yes\n", "quote": "> Design required: yes\n",
		"bare_cr": "Design required: yes\rno\n", "indent": "    Design required: yes\n",
		// Setext headings: the underline makes the paragraph above it a
		// heading, whichever underline character and however long.
		"setext_equals": "Design required: yes\n===\n", "setext_dashes": "Design required: yes\n---\n",
		"setext_one": "Design required: yes\n=\n", "setext_spaced": "Design required: yes\n   ---  \n",
		"setext_crlf": "Design required: yes\r\n===\r\n", "setext_paragraph": "Intro line\nDesign required: yes\n===\n",
		"setext_lazy": "Design required: yes\n    continued\n---\n",
	} {
		if _, err := parseDesignRequired(bad); err == nil {
			t.Errorf("%s accepted %q", name, bad)
		}
	}
	// A thematic break after a blank line, or a list or fence ending
	// before the dashes, does not turn a field line into a heading.
	for _, text := range []string{"Design required: no\n\n---\n", "Design required: no\n- item\n---\n", "Design required: no\n```\nx\n```\n---\n",
		"---\nDesign required: no\n", "Design required: no\n    ===\n\n"} {
		if v, err := parseDesignRequired(text); err != nil || v != designRequiredNo {
			t.Errorf("rejected %q: %v", text, err)
		}
	}
	if vals, bad := fieldValues("Cleanup: done\n---\nCoverage: 90% x\n===\n", "Cleanup:"); len(vals) != 0 || bad {
		t.Fatalf("a setext heading supplied a review field: %v", vals)
	}
	if vals, _ := fieldValues("Cleanup: done\n\n---\n", "Cleanup:"); len(vals) != 1 {
		t.Fatal("a field before a thematic break was lost")
	}
	for name, bad := range map[string]string{
		"setext_status": "STATUS: DESIGN_DRAFT\n===\n", "setext_blocker": "CALLSHEET_REVIEW_BLOCKER: none\nSTATUS: REVIEW_APPROVED\n---\n",
	} {
		if _, err := parseReviewBlocker(bad, bad, markerReviewApproved); err == nil {
			t.Errorf("%s accepted %q", name, bad)
		}
		if _, err := parseTerminalMarker(bad); err == nil {
			t.Errorf("%s supplied a marker", name)
		}
	}
	for _, text := range []string{"noise\nSTATUS: DESIGN_DRAFT\n", "STATUS: DESIGN_DRAFT", "```\nSTATUS: BLOCKED\n```\nSTATUS: DESIGN_DRAFT\n\n"} {
		if m, err := parseTerminalMarker(text); err != nil || m != markerDesignDraft {
			t.Fatalf("%q: %v", text, err)
		}
	}
	for name, bad := range map[string]string{
		"two": "STATUS: DESIGN_DRAFT\nSTATUS: BLOCKED\n", "not_last": "STATUS: DESIGN_DRAFT\nmore\n", "unclosed": "```\nSTATUS: DESIGN_DRAFT\n",
		"tilde_unclosed": "~~~\nSTATUS: DESIGN_DRAFT\n", "space": "STATUS:  DESIGN_DRAFT\n", "lower": "status: DESIGN_DRAFT\n",
		"earlier_lower": "Status: done\nSTATUS: DESIGN_DRAFT\n", "quoted": "> STATUS: DESIGN_DRAFT\n", "empty": "", "token": "STATUS: design draft\n",
		"bare_cr": "STATUS: DESIGN_DRAFT\r", "cr_inside": "STATUS: DESIGN\rDRAFT\n",
	} {
		if _, err := parseTerminalMarker(bad); err == nil {
			t.Errorf("%s accepted %q", name, bad)
		}
	}
	if kind, _, ok := ownerLineKind("yes 2", 2, false); !ok || kind != "yes" {
		t.Fatal("yes")
	}
	for _, l := range []string{"yes 1", "yes", "yes  2", " yes 2", "YES 2", "no 1", "answer 2 x"} {
		if _, _, ok := ownerLineKind(l, 2, false); ok {
			t.Fatalf("decided %q", l)
		}
	}
	if kind, text, ok := ownerLineKind("answer 2 the flag is --name", 2, true); !ok || kind != "answer" || text != "the flag is --name" {
		t.Fatal("answer")
	}
	for _, l := range []string{"answer 1 x", "answer 2 ", "answer 2", "yes 2", "answer 2 " + strings.Repeat("a", triageTaskCeiling+1), "answer 2 \xff"} {
		if _, _, ok := ownerLineKind(l, 2, true); ok {
			t.Fatalf("answered %q", l)
		}
	}
	if gate, gen, err := parsePhaseGate("Paused.\ngate=owner generation=2\n"); err != nil || gate != pauseOwner || gen != 2 {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "gate=owner generation=02", "gate=owner generation=1\ngate=owner generation=1", "gate=other generation=1",
		"> gate=owner generation=1", "gate=owner  generation=1"} {
		if _, _, err := parsePhaseGate(bad); err == nil {
			t.Fatalf("gate %q", bad)
		}
	}
	if itoa(-12) != "-12" || itoa(0) != "0" {
		t.Fatal("itoa")
	}
}

// treeOf builds a regular-file tree.
func treeOf(files map[string]string) []TreeFile {
	var out []TreeFile
	for p, c := range files {
		out = append(out, TreeFile{Path: p, Mode: filemode.Regular, Content: []byte(c)})
	}
	return out
}

func with(base map[string]string, changes map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range changes {
		if v == "\x00delete" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func TestDevFullPublications(t *testing.T) {
	seed := map[string]string{"FEATURE.md": "feature\n", "go.mod": "module m\n\ngo 1.26.0\n", "main.go": "package main\nfunc main() {}\n", "main_test.go": "package main\n"}
	draft := with(seed, map[string]string{"design.md": "# d\n", "revision-history.md": "# h\n"})
	design := sha256Hex([]byte("# d\n"))
	audit := with(draft, map[string]string{"design-review.md": "design " + design + "\nSTATUS: DESIGN_REVIEW_APPROVED\n"})
	coded := with(audit, map[string]string{"main.go": "package main\nfunc main() { println(1) }\n"})
	runDigest := sha256Hex([]byte("the helper's receipt\n"))
	adm := func(role, mode, marker string, base, result map[string]string) (admitResult, error) {
		return AdmitPublication(admitInput{Role: role, Mode: mode, Marker: marker, Base: treeOf(base), Result: treeOf(result), DesignDigest: design,
			Candidate: commitOf("cand"), TaskID: "t_" + strings.Repeat("ab", 16), Final: "CALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + marker + "\n",
			ReviewRun: runDigest})
	}
	if _, err := adm("designer", modeDraft, markerDesignDraft, seed, draft); err != nil {
		t.Fatal(err)
	}
	if _, err := adm("design-reviewer", modeAudit, markerDesignApproved, draft, audit); err != nil {
		t.Fatal(err)
	}
	if _, err := adm("coder", modeImplement, markerImplComplete, audit, coded); err != nil {
		t.Fatal(err)
	}
	cf := map[string]TreeFile{}
	for _, f := range treeOf(coded) {
		cf[f.Path] = f
	}
	report := func(marker string, drop string) string {
		var b strings.Builder
		b.WriteString("# Review\n\nReviewed candidate: " + commitOf("cand") + "\n")
		for _, p := range devSourceFiles {
			if p != drop {
				b.WriteString("Source sha256: " + p + " " + sha256Hex(cf[p].Content) + "\n")
			}
		}
		b.WriteString("Verification exit 0: go test ./...\nVerification exit 0: go test -coverprofile=.callsheet-review-scratch/t_" + strings.Repeat("ab", 16) +
			"/coverage.out ./...\nCoverage: 85.0% of statements\nCleanup: scratch removed\nReviewer run sha256: " + runDigest + "\nCALLSHEET_REVIEW_BLOCKER: none\nSTATUS: " + marker + "\n")
		return b.String()
	}
	reviewed := with(coded, map[string]string{"review.md": report(markerReviewApproved, "")})
	if res, err := adm("code-reviewer", modeReviewFull, markerReviewApproved, coded, reviewed); err != nil || res.Blocker != blockerNone {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		role, marker string
		base, result map[string]string
	}{
		"review_edit":       {"code-reviewer", markerReviewApproved, coded, with(reviewed, map[string]string{"main.go": "package main\n// edit\n"})},
		"review_add":        {"code-reviewer", markerReviewApproved, coded, with(reviewed, map[string]string{"notes.md": "x\n"})},
		"review_delete":     {"code-reviewer", markerReviewApproved, coded, with(reviewed, map[string]string{"design.md": "\x00delete"})},
		"review_missing":    {"code-reviewer", markerReviewApproved, coded, coded},
		"review_marker":     {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": report(markerReviewChanges, "")})},
		"review_source":     {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": report(markerReviewApproved, "main.go")})},
		"review_candidate":  {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": strings.Replace(report(markerReviewApproved, ""), commitOf("cand"), commitOf("other"), 1)})},
		"review_scratch":    {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": strings.Replace(report(markerReviewApproved, ""), "t_"+strings.Repeat("ab", 16)+"/", "other/", 1)})},
		"review_command":    {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": strings.Replace(report(markerReviewApproved, ""), "Verification exit 0: go test ./...\n", "", 1)})},
		"review_coverage":   {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": strings.Replace(report(markerReviewApproved, ""), "85.0% of statements", "good", 1)})},
		"review_cleanup":    {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": strings.Replace(report(markerReviewApproved, ""), "Cleanup: scratch removed\n", "", 1)})},
		"review_dup_field":  {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": strings.Replace(report(markerReviewApproved, ""), "Cleanup:", "Cleanup: a\nCleanup:", 1)})},
		"review_bad_case":   {"code-reviewer", markerReviewApproved, coded, with(coded, map[string]string{"review.md": strings.Replace(report(markerReviewApproved, ""), "Coverage:", "COVERAGE: 1% x\nCoverage:", 1)})},
		"audit_digest":      {"design-reviewer", markerDesignApproved, draft, with(draft, map[string]string{"design-review.md": "no digest\nSTATUS: DESIGN_REVIEW_APPROVED\n"})},
		"audit_marker":      {"design-reviewer", markerDesignApproved, draft, with(draft, map[string]string{"design-review.md": design + "\nSTATUS: DESIGN_CHANGES_REQUESTED\n"})},
		"audit_edits":       {"design-reviewer", markerDesignApproved, draft, with(audit, map[string]string{"design.md": "# e\n"})},
		"coder_design":      {"coder", markerImplComplete, audit, with(coded, map[string]string{"design.md": "# x\n"})},
		"coder_unresolved":  {"coder", markerImplComplete, audit, with(coded, map[string]string{"design-questions.md": "still?\n"})},
		"coder_no_question": {"coder", markerQuestions, audit, coded},
		"coder_delete_test": {"coder", markerImplComplete, audit, with(coded, map[string]string{"main_test.go": "\x00delete"})},
		"designer_source":   {"designer", markerDesignDraft, seed, with(draft, map[string]string{"main.go": "package main\n"})},
		"designer_empty":    {"designer", markerDesignDraft, seed, with(draft, map[string]string{"design.md": " \n"})},
		"designer_new_q":    {"designer", markerDesignDraft, seed, with(draft, map[string]string{"design-questions.md": "q?\n"})},
		"designer_replace":  {"designer", markerDesignDraft, with(audit, map[string]string{"design-questions.md": "q?\n"}), with(audit, map[string]string{"design-questions.md": "other?\n"})},
		"designer_delete_q": {"designer", markerDesignDraft, with(audit, map[string]string{"design-questions.md": "q?\n"}), audit},
		"triage_source":     {"triage", markerTriageConf, coded, with(coded, map[string]string{"main.go": "x\n", "rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Design required: no\n"})},
		"triage_no_brief":   {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n"})},
		"triage_unconf_fix": {"triage", markerTriageUnconf, coded, with(coded, map[string]string{"rca.md": "r\n", "fix.md": "Design required: no\n"})},
		"triage_confidence": {"triage", markerTriageProb, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Design required: no\n"})},
		"triage_field":      {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Design required?: no\n"})},
		"triage_inherited": {"triage", markerTriageConf, with(coded, map[string]string{"fix.md": "old\nConfidence: CONFIRMED\nDesign required: no\n"}),
			with(coded, map[string]string{"fix.md": "old\nConfidence: CONFIRMED\nDesign required: no\n", "rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n"})},
		// The current brief's confidence must match the admitted verdict.
		"brief_no_confidence":    {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Design required: no\n"})},
		"brief_unconfirmed":      {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Confidence: UNCONFIRMED\nDesign required: no\n"})},
		"brief_probable":         {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Confidence: PROBABLE\nDesign required: no\n"})},
		"brief_confirmed":        {"triage", markerTriageProb, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_PROBABLE\n", "fix.md": "Confidence: CONFIRMED\nDesign required: no\n"})},
		"brief_lower":            {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Confidence: confirmed\nDesign required: no\n"})},
		"brief_two":              {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Confidence: CONFIRMED\nConfidence: CONFIRMED\nDesign required: no\n"})},
		"brief_conflict":         {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Confidence: CONFIRMED\nconfidence: probable\nDesign required: no\n"})},
		"brief_heading":          {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "## Confidence: CONFIRMED\nDesign required: no\n"})},
		"brief_fenced":           {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "~~~\nConfidence: CONFIRMED\n~~~\nDesign required: no\n"})},
		"brief_setext":           {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Confidence: CONFIRMED\n---\nDesign required: no\n"})},
		"brief_trailing":         {"triage", markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n", "fix.md": "Confidence: CONFIRMED \nDesign required: no\n"})},
		"brief_missing_probable": {"triage", markerTriageProb, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_PROBABLE\n", "fix.md": "Design required: yes\n"})},
		// Diagnostic publications preserve every required input whatever
		// their marker (round 1's deletion guard).
		"questions_delete_source": {"coder", markerQuestions, audit, with(audit, map[string]string{"design-questions.md": "q?\n", "main.go": "\x00delete", "main_test.go": "\x00delete"})},
		"needs_design_delete":     {"coder", markerNeedsDesign, audit, with(audit, map[string]string{"main_test.go": "\x00delete"})},
		"needs_triage_delete":     {"coder", markerNeedsTriage, audit, with(audit, map[string]string{"main.go": "\x00delete"})},
		"coder_blocked_delete":    {"coder", markerBlocked, audit, with(audit, map[string]string{"main.go": "\x00delete"})},
		"input_delete_design":     {"designer", markerNeedsInput, audit, with(audit, map[string]string{"design.md": "\x00delete", "revision-history.md": "\x00delete"})},
		"input_delete_history":    {"designer", markerNeedsInput, audit, with(audit, map[string]string{"revision-history.md": "\x00delete"})},
		"designer_blocked_delete": {"designer", markerBlocked, audit, with(audit, map[string]string{"design.md": "\x00delete"})},
		"draft_delete_history":    {"designer", markerDesignDraft, audit, with(audit, map[string]string{"design.md": "# d2\n", "revision-history.md": "\x00delete"})},
		"triage_delete_rca":       {"triage", markerTriageUnconf, with(coded, map[string]string{"rca.md": "old\n"}), with(coded, map[string]string{"rca.md": "\x00delete"})},
		"review_delete_report":    {"code-reviewer", markerReviewApproved, reviewed, with(reviewed, map[string]string{"review.md": "\x00delete"})},
	} {
		mode := map[string]string{"designer": modeRevise, "design-reviewer": modeAudit, "coder": modeImplement, "code-reviewer": modeReviewFull, "triage": modeInvestigate}[c.role]
		if _, err := adm(c.role, mode, c.marker, c.base, c.result); err == nil {
			t.Errorf("%s admitted", name)
		}
	}
	// The designer may empty an existing questions file, and a qualified
	// triage publishes a current brief.
	if _, err := adm("designer", modeRevise, markerDesignDraft, with(audit, map[string]string{"design-questions.md": "q?\n"}),
		with(audit, map[string]string{"design-questions.md": "", "design.md": "# d2\n"})); err != nil {
		t.Fatal(err)
	}
	res, err := adm("triage", modeInvestigate, markerTriageProb, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_PROBABLE\n", "fix.md": "Confidence: PROBABLE\nDesign required: yes\n"}))
	if err != nil || !res.BriefCurrent || res.DesignRequired != designRequiredYes {
		t.Fatalf("brief %+v %v", res, err)
	}
	res, err = adm("triage", modeInvestigate, markerTriageConf, coded, with(coded, map[string]string{"rca.md": "r\nSTATUS: TRIAGE_CONFIRMED\n",
		"fix.md": "# Brief\r\n\r\nConfidence: CONFIRMED\r\n```\nConfidence: PROBABLE\n```\nDesign required: no\r\n"}))
	if err != nil || !res.BriefCurrent || res.DesignRequired != designRequiredNo {
		t.Fatalf("confirmed brief %+v %v", res, err)
	}
	// Initial clarification before any draft exists, and diagnostic
	// publications that preserve every input, are admitted.
	for name, c := range map[string]struct {
		role, mode, marker string
		base, result       map[string]string
	}{
		"initial_clarification": {"designer", modeDraft, markerNeedsInput, seed, seed},
		"later_clarification":   {"designer", modeRevise, markerNeedsInput, audit, audit},
		"designer_blocked":      {"designer", modeDraft, markerBlocked, seed, seed},
		"questions":             {"coder", modeImplement, markerQuestions, audit, with(audit, map[string]string{"design-questions.md": "q?\n"})},
		"needs_design_partial":  {"coder", modeImplement, markerNeedsDesign, audit, with(audit, map[string]string{"main.go": "package main\n// partial\n"})},
		"coder_clears_answered": {"coder", modeImplement, markerImplComplete, with(audit, map[string]string{"design-questions.md": ""}), coded},
	} {
		if _, err := adm(c.role, c.mode, c.marker, c.base, c.result); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	// Separate source and artifact budgets, each at its boundary.
	src := func(n int) map[string]string {
		return with(seed, map[string]string{"FEATURE.md": strings.Repeat("f", n-len(seed["go.mod"])-len(seed["main.go"])-len(seed["main_test.go"]))})
	}
	if err := budgets(index(treeOf(src(maxSourceFixtureBytes - 1)))); err != nil {
		t.Fatalf("source at 16 KiB - 1: %v", err)
	}
	if err := budgets(index(treeOf(src(maxSourceFixtureBytes)))); err == nil {
		t.Fatal("source at 16 KiB accepted")
	}
	arts := map[string]string{}
	for _, name := range devArtifacts[:4] {
		arts[name] = strings.Repeat("a", maxArtifactBytes)
	}
	if err := budgets(index(treeOf(with(seed, arts)))); err != nil {
		t.Fatalf("aggregate at 128 KiB: %v", err)
	}
	arts["review.md"] = "x"
	if err := budgets(index(treeOf(with(seed, arts)))); err == nil {
		t.Fatal("aggregate over 128 KiB accepted")
	}
	if err := budgets(index(treeOf(with(seed, map[string]string{"design.md": strings.Repeat("a", maxArtifactBytes+1)})))); err == nil {
		t.Fatal("artifact over 32 KiB accepted")
	}
	// Modes, nested paths, scratch, links and text.
	exec := treeOf(coded)
	for i := range exec {
		if exec[i].Path == "main.go" {
			exec[i].Mode = filemode.Executable
		}
	}
	if err := AdmitTree("coder", modeImplement, markerImplComplete, treeOf(audit), exec); err == nil {
		t.Fatal("an executable mode")
	}
	for _, f := range []TreeFile{{Path: reviewScratch, Mode: filemode.Regular}, {Path: "dir/x.go", Mode: filemode.Regular}, {Path: ".hidden", Mode: filemode.Regular},
		{Path: "link", Mode: filemode.Symlink}, {Path: "nul", Mode: filemode.Regular, Content: []byte("a\x00")}, {Path: "bad", Mode: filemode.Regular, Content: []byte{0xff}}} {
		if err := AdmitTree("coder", modeImplement, markerImplComplete, treeOf(audit), append(treeOf(coded), f)); err == nil {
			t.Errorf("admitted %+v", f.Path)
		}
	}
	if err := AdmitTree("auditor", modeAudit, markerDesignApproved, treeOf(draft), treeOf(draft)); err == nil {
		t.Fatal("an unknown role")
	}
	// The derived fixture keeps every case of the request.
	feature, _ := devFullFeatureRequest()
	for _, c := range FeatureCases {
		if !strings.Contains(feature, c.ID) {
			t.Fatalf("case %s lost", c.ID)
		}
	}
}

func index(files []TreeFile) map[string]TreeFile {
	m, _ := indexTree(files)
	return m
}

func TestDevFullTemplates(t *testing.T) {
	loaded, err := LoadDevMethod(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Sources) != len(DevMethodNames()) || len(DevMethodNames()) != 15 || loaded.FlowSum != sha256Hex(loaded.FlowRaw) {
		t.Fatalf("sources %d", len(loaded.Sources))
	}
	for _, s := range loaded.Sources {
		b := mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(s.Path)))
		if s.SHA256 != sha256Hex(b) || s.Bytes != len(b) {
			t.Fatalf("source %s", s.Path)
		}
	}
	vals := map[string]string{}
	for _, k := range DevPromptKeys {
		vals[k] = "v-" + k
	}
	rendered, err := RenderDevPrompts(loaded, vals)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range devManualNames() {
		if !bytes.Equal(rendered.Files[name], loaded.Files[name]) {
			t.Fatalf("manual %s was rendered", name)
		}
	}
	if !bytes.Contains(rendered.Rendered[DevPromptFile], []byte("v-SEED_PATH")) || !bytes.HasPrefix(rendered.Rendered[DevSystemName], loaded.Files[DevPlaybook]) ||
		len(rendered.Records) != 4 {
		t.Fatal("render")
	}
	delete(vals, "RUN_ID")
	if _, err := RenderDevPrompts(loaded, vals); err == nil {
		t.Fatal("unset key")
	}
	vals["RUN_ID"] = "r"
	bad := loaded
	bad.Files = map[string][]byte{}
	for k, v := range loaded.Files {
		bad.Files[k] = v
	}
	bad.Files[DevPhaseFile] = []byte("{{NOT_A_KEY}}")
	if _, err := RenderDevPrompts(bad, vals); err == nil {
		t.Fatal("unknown placeholder")
	}
	scratch := t.TempDir()
	copyDevDocs(t, scratch)
	frozen, err := LoadDevMethod(scratch)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scratch, filepath.FromSlash(DevFlowDir), DevPlaybook)
	if err := os.WriteFile(path, []byte("mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frozen.Files[DevPlaybook], loaded.Files[DevPlaybook]) {
		t.Fatal("loaded bytes followed the checkout")
	}
	if _, err := LoadDevMethod(t.TempDir()); err == nil {
		t.Fatal("missing method")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "real.md"), []byte("x\n"), 0o644)
	os.Symlink("real.md", filepath.Join(dir, "link.md"))
	os.Mkdir(filepath.Join(dir, "sub.md"), 0o755)
	os.WriteFile(filepath.Join(dir, "over.md"), bytes.Repeat([]byte("a"), int(maxTemplateBytes)+1), 0o644)
	os.WriteFile(filepath.Join(dir, "empty.md"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "bin.md"), []byte{0xff, 0xfe}, 0o644)
	for _, name := range []string{"link.md", "sub.md", "over.md", "empty.md", "bin.md", "missing.md"} {
		if _, err := readLiteral(filepath.Join(dir, name), maxTemplateBytes); err == nil {
			t.Errorf("read %s", name)
		}
	}
	if _, _, err := (DevLoaded{Files: map[string][]byte{}}).ManualPair("triage"); err == nil {
		t.Fatal("missing pair")
	}
}

func TestDevFullFeatureRequest(t *testing.T) {
	if strings.Count(FeatureRequest, legacySizeSentence) != 1 {
		t.Fatal("source sentence")
	}
	got, err := devFullFeatureRequest()
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(FeatureRequest, legacySizeSentence)
	if got[:i] != FeatureRequest[:i] || got[i+len(devFullSizeSentence):] != FeatureRequest[i+len(legacySizeSentence):] ||
		got[i:i+len(devFullSizeSentence)] != devFullSizeSentence {
		t.Fatal("the derived request differs outside the size sentence")
	}
	files, err := devSeedFiles("1.26.0")
	if err != nil || string(files["FEATURE.md"]) != got || string(seedFiles("1.26.0")["FEATURE.md"]) != FeatureRequest {
		t.Fatal("the development seed or the legacy seed changed")
	}
}

func TestDevFullFlow(t *testing.T) {
	raw := mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(DevFullDir), FlowFile))
	flow, err := ParseDevFullFlow(raw)
	if err != nil {
		t.Fatal(err)
	}
	def, _ := DefaultDevFullFlow().SelectionCanonical()
	got, _ := flow.SelectionCanonical()
	if !bytes.Equal(def, got) || flow.Kind() != FlowDefault {
		t.Fatalf("shipped flow is not the default\n%s\n%s", def, got)
	}
	want := [][4]string{{"designer", "codex", "gpt-6-astra", "low"}, {"design-reviewer", "claude", "claude-fable-5-1", "low"},
		{"coder", "claude", "claude-opus-5-5", "high"}, {"code-reviewer", "codex", "gpt-6.1-sol", "high"}, {"triage", "grok", "grok-4.7", "xhigh"}}
	for i, r := range DefaultDevFullFlow().Roles {
		if [4]string{r.ID, r.Adapter, r.Model, r.Effort} != want[i] {
			t.Fatalf("default row %d %+v", i, r)
		}
		if err := adapter.ValidateSelection(r.Adapter, r.Model, r.Effort); err != nil {
			t.Fatal(err)
		}
	}
	if err := flow.CheckDevPosture("linux"); err != nil {
		t.Fatal(err)
	}
	if err := flow.CheckDevPosture("darwin"); err == nil {
		t.Fatal("grok posture accepted off linux")
	}
	mutate := func(f func(m map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal(raw, &m)
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	roles := func(m map[string]any) []any { return m["roles"].([]any) }
	for name, b := range map[string][]byte{
		"unknown_key":  mutate(func(m map[string]any) { m["extra"] = 1 }),
		"role_key":     mutate(func(m map[string]any) { roles(m)[0].(map[string]any)["x"] = 1 }),
		"schema":       mutate(func(m map[string]any) { m["schema"] = "callsheet-dev-full-flow/v2" }),
		"order":        mutate(func(m map[string]any) { r := roles(m); r[0], r[1] = r[1], r[0] }),
		"missing":      mutate(func(m map[string]any) { m["roles"] = roles(m)[:4] }),
		"extra":        mutate(func(m map[string]any) { m["roles"] = append(roles(m), roles(m)[0]) }),
		"duplicate":    mutate(func(m map[string]any) { roles(m)[1] = roles(m)[0] }),
		"adapter":      mutate(func(m map[string]any) { roles(m)[0].(map[string]any)["adapter"] = "claude" }),
		"effort":       mutate(func(m map[string]any) { roles(m)[4].(map[string]any)["effort"] = "max" }),
		"dup_json_key": []byte(`{"schema":"callsheet-dev-full-flow/v1","schema":"callsheet-dev-full-flow/v1","roles":[]}`),
		"oversize":     bytes.Repeat([]byte(" "), int(maxFlowBytes)+1),
	} {
		if _, err := ParseDevFullFlow(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	over, err := ParseDevFullFlow(mutate(func(m map[string]any) { roles(m)[0].(map[string]any)["effort"] = "medium" }))
	if err != nil || over.Kind() != FlowOverride || over.Roles[0].Adapter != "codex" {
		t.Fatalf("override %v", err)
	}
	if _, ok := over.Role("nope"); ok {
		t.Fatal("role lookup")
	}
	if !SameFrozen(DefaultDevFullFlow(), "coder", DefaultDevFullFlow().Roles[2]) || SameFrozen(DefaultDevFullFlow(), "coder", over.Roles[0]) {
		t.Fatal("frozen comparison")
	}
}

func TestDevFullPreflight(t *testing.T) {
	loaded, err := LoadDevMethod(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	in, rb, err := loaded.ManualPair("triage")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTriageManualBudget(in, rb); err != nil {
		t.Fatal(err)
	}
	if err := checkTriageManualBudget(bytes.Repeat([]byte("a"), triageManualCap/2), bytes.Repeat([]byte("a"), triageManualCap/2)); err != nil {
		t.Fatalf("manuals at 20480: %v", err)
	}
	if err := checkTriageManualBudget(bytes.Repeat([]byte("a"), triageManualCap/2+1), bytes.Repeat([]byte("a"), triageManualCap/2)); err == nil {
		t.Fatal("manuals over 20480")
	}
	// Raw manuals fit; JSON escaping overflows the encoded inequality.
	quotes := bytes.Repeat([]byte(`"`), 10000)
	if err := checkTriageManualBudget(quotes, quotes[:2000]); err == nil {
		t.Fatal("escaping-heavy manuals accepted")
	}
	if err := checkTriageTaskBudget(strings.Repeat("q", triageTaskCeiling-2), "ok"); err != nil {
		t.Fatalf("task text at 8192: %v", err)
	}
	if err := checkTriageTaskBudget(strings.Repeat("q", triageTaskCeiling-1), "ok"); err == nil {
		t.Fatal("task text over 8192")
	}
	if err := checkTriageTaskBudget(strings.Repeat("\"", 4100), ""); err == nil {
		t.Fatal("encoded overflow")
	}
	if err := checkTriageTaskBudget("<>&", strings.Repeat("a", triageTaskCeiling-3)); err != nil {
		t.Fatalf("HTML escaping is disabled: %v", err)
	}
	// The representative task encodes within the adapter's prompt bound.
	goal, acc := strings.Repeat("g", 6000), strings.Repeat("a", 2192)
	task, _ := json.Marshal(map[string]any{"goal": goal, "acceptance": acc, "payload": []string{}})
	if len(in)+len(rb)+len(task)+devPromptReserve > adapter.MaxGrokPromptBytes {
		t.Fatalf("representative triage prompt %d", len(in)+len(rb)+len(task))
	}
}

func TestDevFullIdentity(t *testing.T) {
	st, _ := script(t, markerDesignDraft, markerDesignChanges, markerDesignDraft)
	if st.Tasks[0].Ordinal == st.Tasks[2].Ordinal || st.Tasks[2].Role != "designer" || st.Tasks[0].TaskID == st.Tasks[2].TaskID || st.Pending.Ordinal != 4 {
		t.Fatalf("%+v", st.Tasks)
	}
	h := GoalHeader("20261017T000000Z-01020304", *st.Pending, st.Pending.ExpectedBase) + "\n\nbody"
	id, ok := ParseGoalHeader(h)
	if !ok || id.Ordinal != 4 || id.Role != "design-reviewer" || id.Mode != modeAudit || id.Round != 1 || id.Input != st.Pending.ExpectedBase {
		t.Fatalf("%+v", id)
	}
	for _, bad := range []string{"", "run_id: x", strings.Replace(h, "ordinal: 4", "ordinal: 04", 1), strings.Replace(h, "scenario: full", "scenario: light", 1),
		strings.Replace(h, "round: 1", "round: -1", 1), strings.Replace(h, "role: ", "role:", 1)} {
		if _, ok := ParseGoalHeader(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
	if _, ok := atoi("1234567890"); ok {
		t.Fatal("overlong number")
	}
}

func TestDevFullReceipts(t *testing.T) {
	st, res := script(t, markerDesignDraft, markerDesignApproved)
	if res.Pause != pauseOwner || st.Audit == nil || st.Audit.Generation != 1 {
		t.Fatalf("%+v", res)
	}
	if _, err := BuildReceipt(st, 2); err == nil {
		t.Fatal("future generation")
	}
	rec, err := BuildReceipt(st, 1)
	if err != nil || rec.Design != st.Audit.Design || rec.Review != st.Audit.Review || rec.AuditCommit != st.Audit.Commit || rec.ApprovalSeq != 20 ||
		rec.ConsumedSeq != 21 {
		t.Fatalf("receipt %+v", rec)
	}
	forged := rec
	forged.Design = strings.Repeat("0", 64)
	if _, res := OwnerDecision(st, 1, true, forged, 1); res.Stop != "stale_receipt" {
		t.Fatal("a receipt with another design digest authorized coding")
	}
	if _, res := OwnerDecision(st, 0, true, rec, 1); res.Stop != "stale_receipt" {
		t.Fatal("an old generation authorized coding")
	}
	paused := st.Transitions[len(st.Transitions)-1]
	if paused.ToState != "paused/owner" || paused.FromOrdinal != 2 || paused.FromTask != st.Tasks[1].TaskID || paused.FromDecision != 0 {
		t.Fatalf("the approval pause is not recorded from its audit: %+v", paused)
	}
	no, res := OwnerDecision(st, 1, false, OwnerReceipt{}, 1)
	if last := no.Transitions[len(no.Transitions)-1]; res.Stop != "owner_no" || last.FromDecision != 1 || last.FromOrdinal != 0 || last.FromTask != "" ||
		last.FromState != "owner/no:generation-1" || last.Stop != "owner_no" {
		t.Fatalf("no %+v", last)
	}
	before := slices.Clone(st.Transitions)
	st, res = OwnerDecision(st, 1, true, rec, 1)
	if res.Action != actionDispatch || !st.Receipt.Valid {
		t.Fatal(res)
	}
	if last := st.Transitions[len(st.Transitions)-1]; last.FromDecision != 1 || last.FromOrdinal != 0 || last.FromTask != "" ||
		last.FromState != "owner/yes:generation-1" || last.ToState != "coder/implement" || last.Reason != "owner_yes" {
		t.Fatalf("owner_yes %+v", last)
	}
	if !slices.Equal(before, st.Transitions[:len(before)]) || !slices.Equal(before, no.Transitions[:len(before)]) {
		t.Fatal("binding a decision changed an earlier transition record")
	}
	if _, err := BuildReceipt(st, 1); err == nil {
		t.Fatal("a second receipt for an active generation")
	}
	held := st
	st, res = drive(t, st, admit(st, markerQuestions))
	if !res.Invalidate || st.Receipt.Valid || !held.Receipt.Valid || st.Receipts[0].Valid {
		t.Fatal("the designer's dispatch did not invalidate the receipt")
	}
	// A same-byte draft still needs a fresh audit and decision.
	same := admit(st, markerDesignDraft)
	st, _ = drive(t, st, same)
	if st.Generation != 2 || st.Pending.Role != "design-reviewer" {
		t.Fatal("a same-byte draft skipped the audit")
	}
	if _, res := OwnerDecision(st, 1, true, rec, 2); res.Stop != "stale_receipt" {
		t.Fatal("the old receipt was reactivated")
	}
	if _, res := OwnerAnswer(st, st.Generation, "x", 2); res.Stop != "stale_generation" {
		t.Fatal("an answer without open questions")
	}
	q, _ := script(t, markerNeedsInput)
	if _, res := OwnerAnswer(q, 1, "x", 1); res.Stop != "stale_generation" {
		t.Fatal("an answer for another generation")
	}
	q, res = OwnerAnswer(q, 0, "the answer", 1)
	if res.Action != actionDispatch || q.Clarification != "the answer" || q.Pending.Reason != "clarification" || q.Recovery != 1 {
		t.Fatal("clarification")
	}
	tr := q.Transitions
	if n := len(tr); n != 3 || tr[1].ToState != "paused/answer" || tr[1].FromOrdinal != 1 || tr[2].FromDecision != 1 ||
		tr[2].FromState != "owner/answer:generation-0" || tr[2].ToState != "designer/revise" {
		t.Fatalf("clarification transitions %+v", tr)
	}
	// Lines already waiting when a window opens are pre-window input:
	// every one is discarded (EOF included) and none is left to decide.
	lines := make(chan ownerInput, 4)
	lines <- ownerInput{line: "yes 1"}
	lines <- ownerInput{line: "no 1"}
	close(lines)
	d := &devSupervisor{lines: lines}
	d.discardPending()
	if d.ignoredLines != 2 || !d.termEOF || d.lines != nil {
		t.Fatalf("pending lines: ignored %d eof %t", d.ignoredLines, d.termEOF)
	}
	open := make(chan ownerInput, 1)
	open <- ownerInput{line: "yes 2"}
	d = &devSupervisor{lines: open}
	d.discardPending()
	if d.ignoredLines != 1 || d.termEOF || d.lines == nil || len(open) != 0 {
		t.Fatal("an open terminal's waiting line was not discarded")
	}
	stampedReader(t)
	// The development path needs a terminal that stamps its lines with
	// decision windows; none at all is an EOF at the first window.
	d = &devSupervisor{s: &supervisor{w: &World{Terminal: &fakeTerminal{}}}}
	if err := d.activateTerminal(); codeOf(err) != "owner_terminal" || d.activated || d.lines != nil {
		t.Fatalf("a plain terminal was activated: %v", err)
	}
	d = &devSupervisor{s: &supervisor{w: &World{}}}
	if err := d.activateTerminal(); err != nil || d.activated || d.lines != nil {
		t.Fatalf("no terminal: %v", err)
	}
	rt := &readerTerminal{r: strings.NewReader("yes 1\n")}
	d = &devSupervisor{s: &supervisor{w: &World{Terminal: rt}}}
	if err := d.activateTerminal(); err != nil || !d.activated || d.term != rt || d.lines != rt.ActivateWindowed() {
		t.Fatalf("the windowed terminal: %v", err)
	}
	if in, ok := <-d.lines; !ok || in != (ownerInput{line: "yes 1"}) || rt.OpenWindow() != 1 || rt.OpenWindow() != 2 {
		t.Fatalf("windowed line %+v %t", in, ok)
	}
	if _, ok := <-d.lines; ok {
		t.Fatal("the windowed reader did not end at EOF")
	}
}

// stepReader serves one byte per Read, each from bytes (closed: EOF);
// every Read first reports on asked, so a test knows the line reader
// finished with every earlier byte before it sends the next one.
type stepReader struct {
	bytes chan byte
	asked chan struct{}
}

func (s *stepReader) Read(p []byte) (int, error) {
	s.asked <- struct{}{}
	b, ok := <-s.bytes
	if !ok {
		return 0, io.EOF
	}
	p[0] = b
	return 1, nil
}

// stampedReader drives the production windowed line reader through a
// controlled byte order: a line partly read before a window opens, a line
// complete but still in flight (its send blocked) when a window opens,
// and lines read after; then an empty line, an over-long line and a
// partial last line at EOF.
func stampedReader(t *testing.T) {
	t.Helper()
	sr := &stepReader{bytes: make(chan byte), asked: make(chan struct{})}
	var epoch atomic.Uint64
	out := make(chan ownerInput) // unbuffered: a complete line waits in flight
	go readStampedLines(sr, &epoch, out)
	feed := func(s string) {
		for i := 0; i < len(s); i++ {
			<-sr.asked
			sr.bytes <- s[i]
		}
	}
	// "yes" is read before the window; the reader then asks for more.
	feed("yes")
	<-sr.asked
	epoch.Add(1)
	sr.bytes <- ' '
	feed("1\n")
	if in := <-out; in != (ownerInput{line: "yes 1", epoch: 0}) {
		t.Fatalf("a line begun before the window: %+v", in)
	}
	// "no 1" is read whole in window 1 and its send blocks; window 2 then
	// opens before the loop receives it.
	feed("no 1\n")
	epoch.Add(1)
	if in := <-out; in != (ownerInput{line: "no 1", epoch: 1}) {
		t.Fatalf("a line in flight when the window opened: %+v", in)
	}
	feed("yes 2\n")
	if in := <-out; in != (ownerInput{line: "yes 2", epoch: 2}) {
		t.Fatalf("a line read in the window: %+v", in)
	}
	feed("\n")
	if in := <-out; in != (ownerInput{line: "", epoch: 2}) {
		t.Fatalf("an empty line: %+v", in)
	}
	feed(strings.Repeat("a", maxControlLine+3) + "\n")
	if in := <-out; in.line != strings.Repeat("a", maxControlLine) || in.epoch != 2 {
		t.Fatalf("an over-long line is %d bytes", len(in.line))
	}
	feed("yes")
	<-sr.asked
	close(sr.bytes)
	if in, ok := <-out; ok {
		t.Fatalf("a partial last line was delivered: %+v", in)
	}
}

// terminalView is a valid terminal task view from the fake plane.
func terminalView(t *testing.T) contract.TaskView {
	t.Helper()
	p := newFakePlane(t, newAutoClock())
	if _, err := p.AddRole(context.Background(), contract.RoleConfig{ID: "designer", Name: "designer", Node: hexID("n_", 1), Adapter: "codex",
		Model: "gpt-6-astra", Effort: "low", Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	v, err := p.Dispatch(context.Background(), contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "designer"},
		Goal: "g", Acceptance: "a", RequestedBy: requester})
	if err != nil {
		t.Fatal(err)
	}
	final := "STATUS: DESIGN_DRAFT\n"
	p.finish(v.TaskID, contract.TaskSucceeded, &final, false, nil)
	v, _ = p.ShowTask(context.Background(), v.TaskID, 0)
	return v
}

func TestDevFullLifecycle(t *testing.T) {
	view := terminalView(t)
	task := view.TaskID
	cmd := devWaitCommand("/bin/reale2e", "/tmp/rt", task, 1)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// lines counts the stream's records (each test stream restarts it).
	lines := 0
	w := func(v map[string]any) {
		if _, ok := v["session_id"]; !ok {
			v["session_id"] = testSession
		}
		lines++
		enc.Encode(v)
	}
	// raw writes one literal record (no session added).
	raw := func(line string) {
		lines++
		buf.WriteString(line + "\n")
	}
	use := func(id, name string, input map[string]any) {
		w(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
	}
	result := func(id, text string, isErr bool) {
		w(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr}}}})
	}
	note := func(handle, toolUse string) {
		w(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification>\n<task-id>" + handle + "</task-id>\n<tool-use-id>" + toolUse +
			"</tool-use-id>\n<status>completed</status>\n</task-notification>"}})
	}
	show, _ := json.Marshal(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: view})
	// judge decodes a built stream and derives the slot's wait: the stream
	// problem first (a notification's own session and shape), then the
	// wait's, with the line of the record a refusal names.
	judge := func(build func()) (devWait, string, int) {
		buf.Reset()
		lines = 0
		w(map[string]any{"type": "system", "subtype": "init", "permissionMode": "auto"})
		build()
		s := decodeDevStream(buf.Bytes(), "")
		if s.problem != "" {
			return devWait{}, s.problem, s.problemSrc.FirstLine
		}
		got, src, p := deriveDevWait(s.records, cmd, task)
		return got, p, src.refused.FirstLine
	}
	// qualify is the slot's wait derivation alone, after a clean stream.
	qualify := func(build func()) (devWait, string) {
		buf.Reset()
		lines = 0
		w(map[string]any{"type": "system", "subtype": "init", "permissionMode": "auto"})
		build()
		s := decodeDevStream(buf.Bytes(), "")
		if s.problem != "" && s.problem != devToolFailed {
			t.Fatalf("stream problem %s", s.problem)
		}
		return qualifyDevWait(s.records, cmd, task)
	}
	start := func() {
		use("tu", toolBash, map[string]any{"command": cmd, "run_in_background": true, "description": "wait"})
		result("tu", "Command running in background with ID: b1. Output is being written to: /tmp/x", false)
	}
	consume := func(id string) {
		use(id, toolTaskShow, map[string]any{"id": task})
		result(id, string(show), false)
	}
	if got, p := qualify(func() { start(); note("b1", "tu"); consume("ts") }); p != "" || !got.complete() || got.Handle != "b1" {
		t.Fatalf("qualified wait %+v %s", got, p)
	}
	// Live probe 20261022T132232Z-3fe07457 (design 12a-dev-flow-verify
	// r0.8): Claude Code 2.1.306 reports the completed background wait as
	// a top-level system task_notification record (sanitized copy of the
	// captured shape, run_id and uuid included).
	sysFields := func(handle, toolUse string) map[string]any {
		return map[string]any{"type": "system", "subtype": "task_notification", "task_id": handle, "tool_use_id": toolUse, "status": "completed",
			"output_file": "/tmp/claude/tasks/" + handle + ".output", "summary": "Background command \"wait\" completed (exit code 0)",
			"run_id": "11111111-2222-3333-4444-555555555555", "uuid": "66666666-7777-8888-9999-000000000000"}
	}
	sysNote := func(handle, toolUse string) { w(sysFields(handle, toolUse)) }
	if got, p := qualify(func() { start(); sysNote("b1", "tu"); consume("ts") }); p != "" || !got.complete() || got.Handle != "b1" {
		t.Fatalf("system notification wait %+v %s", got, p)
	}
	// Live probe 20261022T181848Z-dbf4f52f (design 12a-dev-flow-verify
	// r0.9): Claude Code 2.1.306 started the wait with a Bash timeout
	// (sanitized copy of the captured start: the exact command, its
	// description, timeout 900000 and run_in_background true, in that key
	// order).
	capturedStart := func() {
		raw(`{"type":"assistant","session_id":"` + testSession + `","message":{"content":[{"type":"tool_use","id":"tu","name":"Bash","input":{"command":` +
			strconv.Quote(cmd) + `,"description":"Wait in background for the probe task to finish","timeout":900000,"run_in_background":true}}]}}`)
		result("tu", "Command running in background with ID: b1. Output is being written to: /tmp/x", false)
	}
	if got, p := qualify(func() { capturedStart(); sysNote("b1", "tu"); consume("ts") }); p != "" || !got.complete() || got.Handle != "b1" {
		t.Fatalf("captured timeout start %+v %s", got, p)
	}
	// The session ID is optional; the record then inherits the phase's.
	if got, p := qualify(func() {
		start()
		b, _ := json.Marshal(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "b1", "tool_use_id": "tu", "status": "completed"})
		raw(string(b))
		consume("ts")
	}); p != "" || !got.complete() {
		t.Fatalf("system notification without a session %+v %s", got, p)
	}
	// A foreground command and a system status event are no wake.
	if got, p := qualify(func() {
		use("fg", toolBash, map[string]any{"command": cmd})
		result("fg", "done", false)
		w(map[string]any{"type": "system", "subtype": "status"})
		consume("ts")
	}); p != devWaitMismatch || got.complete() {
		t.Fatalf("foreground %+v %s", got, p)
	}
	if got, p := qualify(func() {
		start()
		w(map[string]any{"type": "system", "subtype": "status"})
		consume("ts")
	}); p != "" || got.Woke || got.complete() {
		t.Fatalf("status event %+v %s", got, p)
	}
	// An unrelated background Bash loop's completed notification, in
	// either form, is a non-wake and no refusal: the wait still completes
	// by its own notification. Its result text mentioning an envelope and
	// assistant prose quoting one are never notification evidence.
	loop := func() {
		use("loop", toolBash, map[string]any{"command": "while true; do sleep 1; done", "run_in_background": true})
		result("loop", "Command running in background with ID: b7. Output is being written to: /tmp/y", false)
	}
	for form, unrelated := range map[string]func(){"system": func() { sysNote("b7", "loop") }, "xml": func() { note("b7", "loop") }} {
		if got, p := qualify(func() {
			loop()
			start()
			unrelated()
			use("cat", toolBash, map[string]any{"command": "cat notes"})
			result("cat", "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>tu</tool-use-id>\n<status>completed</status>\n</task-notification>", false)
			w(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text",
				"text": "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>tu</tool-use-id>\n<status>completed</status>\n</task-notification>"}}}})
			consume("early")
			sysNote("b1", "tu")
			consume("ts")
		}); p != "" || !got.complete() || got.ConsumeUse != "ts" {
			t.Fatalf("%s unrelated job %+v %s", form, got, p)
		}
	}
	// An earlier slot's notification is that slot's evidence: it cannot
	// wake this slot, which still waits for its own.
	prev := devWaitCommand("/bin/reale2e", "/tmp/rt", "t_"+strings.Repeat("c", 32), 1)
	slot2 := devWaitCommand("/bin/reale2e", "/tmp/rt", task, 2)
	for form, prior := range map[string]func(){"system": func() { sysNote("b0", "tp") }, "xml": func() { note("b0", "tp") }} {
		build := func() {
			use("tp", toolBash, map[string]any{"command": prev, "run_in_background": true})
			result("tp", "Command running in background with ID: b0. Output is being written to: /tmp/z", false)
			use("tu", toolBash, map[string]any{"command": slot2, "run_in_background": true})
			result("tu", "Command running in background with ID: b1. Output is being written to: /tmp/x", false)
			prior()
			consume("ts")
		}
		buf.Reset()
		lines = 0
		w(map[string]any{"type": "system", "subtype": "init", "permissionMode": "auto"})
		build()
		s := decodeDevStream(buf.Bytes(), "")
		if got, _, p := deriveDevWait(s.records, slot2, task); s.problem != "" || p != "" || got.Woke || got.Consumed {
			t.Fatalf("%s prior-slot notification woke this slot: %+v %s %s", form, got, s.problem, p)
		}
	}
	for name, c := range map[string]struct {
		build func()
		want  string
	}{
		"duplicate": {func() { start(); start() }, devDuplicateWait},
		"extra_key": {func() {
			use("tu", toolBash, map[string]any{"command": cmd, "run_in_background": true, "timeout_ms": 1})
		}, devWaitMismatch},
		"other_cmd": {func() {
			use("tu", toolBash, map[string]any{"command": "callsheet task wait " + task, "run_in_background": true})
		}, devWaitMismatch},
		"no_handle": {func() {
			use("tu", toolBash, map[string]any{"command": cmd, "run_in_background": true})
			result("tu", "ran", false)
		}, devWaitMismatch},
		"interrupted": {func() {
			start()
			note("b1", "tu")
			w(map[string]any{"type": "user", "message": map[string]any{"content": "are you done?"}})
			consume("ts")
		}, devWakeNotAutomatic},
		"failed_start": {func() {
			use("tu", toolBash, map[string]any{"command": cmd, "run_in_background": true})
			result("tu", "denied", true)
		}, devWaitMismatch},
	} {
		if _, p := qualify(c.build); p != c.want {
			t.Errorf("%s: problem %q want %q", name, p, c.want)
		}
	}
	// Every refused notification fails at its own record with
	// wake_representation_unsupported, even when a valid notification,
	// the consuming task_show and the captured files follow it (r0.8).
	field := func(mutate func(m map[string]any)) func() {
		return func() {
			m := sysFields("b1", "tu")
			mutate(m)
			w(m)
		}
	}
	rawSys := func(body string) func() {
		return func() { raw(`{"type":"system","subtype":"task_notification",` + body + `}`) }
	}
	refusals := map[string]func(){
		"sys_wrong_handle":   field(func(m map[string]any) { m["task_id"] = "b2" }),
		"sys_missing_handle": field(func(m map[string]any) { delete(m, "task_id") }),
		"sys_empty_handle":   field(func(m map[string]any) { m["task_id"] = "" }),
		"sys_number_handle":  field(func(m map[string]any) { m["task_id"] = 7 }),
		"sys_null_handle":    field(func(m map[string]any) { m["task_id"] = nil }),
		"sys_wrong_use":      field(func(m map[string]any) { m["tool_use_id"] = "zz" }),
		"sys_missing_use":    field(func(m map[string]any) { delete(m, "tool_use_id") }),
		"sys_empty_use":      field(func(m map[string]any) { m["tool_use_id"] = "" }),
		"sys_object_use":     field(func(m map[string]any) { m["tool_use_id"] = map[string]any{} }),
		"sys_failed":         field(func(m map[string]any) { m["status"] = "failed" }),
		"sys_status_case":    field(func(m map[string]any) { m["status"] = "Completed" }),
		"sys_missing_status": field(func(m map[string]any) { delete(m, "status") }),
		"sys_empty_status":   field(func(m map[string]any) { m["status"] = "" }),
		"sys_bool_status":    field(func(m map[string]any) { m["status"] = true }),
		"sys_other_session":  field(func(m map[string]any) { m["session_id"] = "11111111-1111-4111-8111-111111111111" }),
		"sys_empty_session":  field(func(m map[string]any) { m["session_id"] = "" }),
		"sys_number_session": field(func(m map[string]any) { m["session_id"] = 1 }),
		"sys_unknown_field":  field(func(m map[string]any) { m["exit_code"] = 0 }),
		"sys_session_alias":  field(func(m map[string]any) { m["sessionId"] = testSession }),
		"sys_wrong_summary":  field(func(m map[string]any) { m["summary"] = 1 }),
		"sys_null_uuid":      field(func(m map[string]any) { m["uuid"] = nil }),
		"sys_duplicate_key":  rawSys(`"task_id":"b1","task_id":"b1","tool_use_id":"tu","status":"completed"`),
		// Code review W1: a user XML notification the strict decoder
		// refuses (a duplicate key) is still a refused notification.
		"xml_duplicate_key": func() {
			raw(`{"type":"user","type":"user","session_id":"` + testSession + `","message":{"content":"<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>tu</tool-use-id>\n<status>completed</status>\n</task-notification>"}}`)
		},
		"sys_on_user_record": func() {
			w(map[string]any{"type": "user", "subtype": "task_notification", "task_id": "b1", "tool_use_id": "tu", "status": "completed"})
		},
		"sys_on_assistant": func() {
			w(map[string]any{"type": "assistant", "subtype": "task_notification", "task_id": "b1", "tool_use_id": "tu", "status": "completed"})
		},
		"xml_wrong_use":    func() { note("b1", "zz") },
		"xml_wrong_handle": func() { note("b2", "tu") },
		"xml_failed": func() {
			w(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>tu</tool-use-id>\n<status>failed</status>\n</task-notification>"}})
		},
		"xml_other_session": func() { note("b1", "tu") /* replaced below */ },
		"xml_unsupported_shape": func() {
			w(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification>b1</task-notification> and"}})
		},
		"xml_beside_results": func() {
			w(map[string]any{"type": "user", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "q", "content": "x"},
				map[string]any{"type": "text", "text": "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>tu</tool-use-id>\n<status>completed</status>\n</task-notification>"}}}})
		},
	}
	refusals["xml_other_session"] = func() {
		w(map[string]any{"type": "user", "session_id": "11111111-1111-4111-8111-111111111111", "message": map[string]any{"content": "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>tu</tool-use-id>\n<status>completed</status>\n</task-notification>"}})
	}
	for name, bad := range refusals {
		var at int
		if _, p, line := judge(func() {
			start()
			bad()
			at = lines
			sysNote("b1", "tu")
			consume("ts")
		}); p != devWakeUnsupported || line != at {
			t.Errorf("%s: problem %q at line %d, want %s at line %d", name, p, line, devWakeUnsupported, at)
		}
	}
	// A notification's own session mismatch is refused by the stream
	// decoder itself, before the generic session check and before any wait
	// derivation.
	for _, name := range []string{"sys_other_session", "xml_other_session", "sys_empty_session", "sys_unknown_field", "sys_duplicate_key", "xml_duplicate_key"} {
		buf.Reset()
		lines = 0
		w(map[string]any{"type": "system", "subtype": "init", "permissionMode": "auto"})
		start()
		refusals[name]()
		if s := decodeDevStream(buf.Bytes(), ""); s.problem != devWakeUnsupported || s.problemSrc.FirstLine != lines {
			t.Errorf("%s: stream problem %q at %d", name, s.problem, s.problemSrc.FirstLine)
		}
		// The event map names the same record.
		em := deriveDevEventMap(devMapInput{streams: map[int][]byte{1: buf.Bytes()}, phases: 1})
		if f := em.Phases[0]; f.Problem != devWakeUnsupported || f.ProblemSource == nil || f.ProblemSource.FirstLine != lines || f.ProblemSource.File != devStreamFile(1) {
			t.Errorf("%s: event map phase %+v", name, f)
		}
	}
	// Binding: before the start or its handle, unbound, cross-bound with
	// an unrelated start, or a duplicate within or across the two forms.
	for name, c := range map[string]struct {
		build func() int
	}{
		"before_start": {func() int { sysNote("b1", "tu"); at := lines; start(); return at }},
		"before_handle": {func() int {
			use("tu", toolBash, map[string]any{"command": cmd, "run_in_background": true})
			note("b1", "tu")
			return lines
		}},
		"sys_unbound":   {func() int { start(); sysNote("b9", "other"); return lines }},
		"xml_unbound":   {func() int { start(); note("b9", "other"); return lines }},
		"sys_cross":     {func() int { loop(); start(); sysNote("b1", "loop"); return lines }},
		"xml_cross":     {func() int { loop(); start(); note("b1", "loop"); return lines }},
		"sys_cross_use": {func() int { loop(); start(); sysNote("b7", "tu"); return lines }},
		"xml_cross_use": {func() int { loop(); start(); note("b7", "tu"); return lines }},
		"sys_duplicate": {func() int { start(); sysNote("b1", "tu"); sysNote("b1", "tu"); return lines }},
		"xml_duplicate": {func() int { start(); note("b1", "tu"); note("b1", "tu"); return lines }},
		"sys_then_xml":  {func() int { start(); sysNote("b1", "tu"); note("b1", "tu"); return lines }},
		"xml_then_sys":  {func() int { start(); note("b1", "tu"); sysNote("b1", "tu"); return lines }},
		"after_consume": {func() int { start(); sysNote("b1", "tu"); consume("ts"); note("b1", "tu"); return lines }},
	} {
		var at int
		if _, p, line := judge(func() { at = c.build(); consume("ts2") }); p != devWakeUnsupported || line != at {
			t.Errorf("%s: problem %q at line %d, want %s at line %d", name, p, line, devWakeUnsupported, at)
		}
	}
	// Fourth probe (design 12a-dev-flow-verify r0.10): the vendor's
	// persisted-output replacement of the consuming task_show is no
	// evidence, with or without is_error; a later separately paired lines:0
	// read consumes; a Bash read of the saved file never does.
	replaced := "Error: result (67,506 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to /tmp/claude/tool-results/mcp-callsheet-task_show-1.txt."
	for _, isErr := range []bool{false, true} {
		if got, p := qualify(func() {
			start()
			note("b1", "tu")
			use("big", toolTaskShow, map[string]any{"id": task})
			result("big", replaced, isErr)
		}); p != "" || got.Consumed {
			t.Fatalf("the replacement (is_error %t) consumed: %+v %s", isErr, got, p)
		}
		if got, p := qualify(func() {
			start()
			note("b1", "tu")
			use("big", toolTaskShow, map[string]any{"id": task})
			result("big", replaced, isErr)
			use("cat", toolBash, map[string]any{"command": "cat /tmp/claude/tool-results/mcp-callsheet-task_show-1.txt"})
			result("cat", string(show), false)
			use("small", toolTaskShow, map[string]any{"id": task, "lines": 0})
			result("small", string(show), false)
		}); p != "" || !got.complete() || got.ConsumeUse != "small" {
			t.Fatalf("a later lines:0 read (is_error %t): %+v %s", isErr, got, p)
		}
	}
	if got, _ := qualify(func() {
		start()
		note("b1", "tu")
		use("big", toolTaskShow, map[string]any{"id": task})
		result("big", replaced, false)
		use("cat", toolBash, map[string]any{"command": "cat /tmp/claude/tool-results/mcp-callsheet-task_show-1.txt"})
		result("cat", string(show), false)
	}); got.Consumed {
		t.Fatal("a Bash read of the saved file consumed the wait")
	}
	for text, want := range map[string]bool{
		replaced:        true,
		replaced + "\n": true,
		"Error: result (67506 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to /x.":        true,
		"Error: result (1,234,567 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to /x.":    true,
		"Error: result (67,506 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to /x":        false,
		"Error: result (67,506 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to .":         false,
		"Error: result (67,506 characters across 2 lines) exceeds maximum allowed tokens. Output has been saved to /x.":      false,
		"Error: result (67,5060 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to /x.":      false,
		"Error: result (,506 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to /x.":         false,
		"Error: result (67,506 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to /x.\nmore": false,
		"Error: the result exceeds the maximum allowed tokens; it was saved to /x.":                                          false,
		" " + replaced: false,
	} {
		if taskShowReplaced(text) != want {
			t.Errorf("replacement %q: %v", text, !want)
		}
	}
	// r0.10c: the captured multiline replacement (package fixture: the
	// decoded tool result of the fourth probe's line 47 with its saved path
	// neutralized), its accepted variants and every mutation outside the
	// carve-out.
	captured := string(mustRead(t, filepath.Join("testdata", "task-show-replaced.txt")))
	// The capture's saved path is replaced by a neutral absolute path (owner
	// decision); every other byte is the captured payload's.
	const capturedPath = "/home/user/.claude/projects/example/tool-results/mcp-callsheet-task_show-0.txt"
	capLines := strings.Split(strings.TrimSuffix(captured, "\n"), "\n")
	if len(capLines) != 9 || strings.Count(captured, capturedPath) != 2 || !strings.HasSuffix(captured, ".\n") {
		t.Fatalf("fixture %d lines, path %d times", len(capLines), strings.Count(captured, capturedPath))
	}
	join := func(ls []string) string { return strings.Join(ls, "\n") + "\n" }
	edited := func(f func(ls []string) []string) string { return join(f(slices.Clone(capLines))) }
	accepted := map[string]string{
		"captured":           captured,
		"no_trailing_lf":     strings.TrimSuffix(captured, "\n"),
		"ungrouped":          strings.Replace(captured, "(67,506 ", "(67506 ", 1),
		"other_path":         strings.ReplaceAll(captured, capturedPath, "/tmp/other dir/x in y.txt"),
		"short_path":         strings.ReplaceAll(captured, capturedPath, "/x"),
		"grouped_large":      strings.Replace(captured, "(67,506 ", "(1,234,567 ", 1),
		"one_line_crlf":      capLines[0] + "\r\n",
		"one_line_no_ending": capLines[0],
	}
	refused := map[string]string{
		"extra_trailing_lf": captured + "\n",
		"bullet_path":       strings.Replace(captured, "file at "+capturedPath, "file at "+capturedPath+"x", 1),
		"first_path":        strings.Replace(captured, "saved to "+capturedPath, "saved to "+capturedPath+"x", 1),
		"relative_path":     strings.ReplaceAll(captured, capturedPath, "tool-results/x.txt"),
		"empty_path":        strings.ReplaceAll(captured, capturedPath, ""),
		"root_only":         strings.ReplaceAll(captured, capturedPath, "/"),
		"crlf":              strings.ReplaceAll(captured, "\n", "\r\n"),
		"blank_line":        edited(func(ls []string) []string { return slices.Insert(ls, 4, "") }),
		"reordered":         edited(func(ls []string) []string { ls[1], ls[2] = ls[2], ls[1]; return ls }),
		"tail":              captured + "more\n",
		"trimmed":           edited(func(ls []string) []string { ls[3] = ls[3] + " "; return ls }),
		"one_line_cr_only":  capLines[0] + "\r",
		"one_line_relative": strings.Replace(capLines[0], "saved to /", "saved to ", 1),
	}
	for i := 1; i < 9; i++ {
		refused[fmt.Sprintf("changed_%d", i)] = edited(func(ls []string) []string { ls[i] = strings.Replace(ls[i], " ", "  ", 1); return ls })
		refused[fmt.Sprintf("deleted_%d", i)] = edited(func(ls []string) []string { return slices.Delete(ls, i, i+1) })
		refused[fmt.Sprintf("inserted_%d", i)] = edited(func(ls []string) []string { return slices.Insert(ls, i, ls[i]) })
	}
	for name, text := range accepted {
		if !taskShowReplaced(text) {
			t.Errorf("accepted %s refused", name)
		}
	}
	for name, text := range refused {
		if taskShowReplaced(text) {
			t.Errorf("refused %s accepted", name)
		}
	}
	// Through the session: the captured payload is no evidence, then a
	// later lines:0 read consumes.
	if got, p := qualify(func() {
		start()
		sysNote("b1", "tu")
		use("big", toolTaskShow, map[string]any{"id": task})
		result("big", captured, false)
	}); p != "" || got.Consumed {
		t.Fatalf("the captured payload consumed: %+v %s", got, p)
	}
	if got, p := qualify(func() {
		start()
		sysNote("b1", "tu")
		use("big", toolTaskShow, map[string]any{"id": task})
		result("big", captured, false)
		use("small", toolTaskShow, map[string]any{"id": task, "lines": 0})
		result("small", string(show), false)
	}); p != "" || !got.complete() || got.ConsumeUse != "small" {
		t.Fatalf("a later lines:0 read %+v %s", got, p)
	}
	// Consumption before the wake does not count.
	if got, _ := qualify(func() { start(); consume("ts"); note("b1", "tu") }); got.Consumed {
		t.Fatal("a task_show before the wake consumed it")
	}
	// The recognizer (r0.9): devWaitCommand's grammar for the known
	// executable and runtime, any valid task and ordinal, classified three
	// ways before any background filter.
	prefix := devWaitPrefix(cmd, task)
	if prefix != devWaitPrefixOf("/bin/reale2e", "/tmp/rt") || devWaitPrefix("callsheet task wait", task) != "" {
		t.Fatalf("prefix %q", prefix)
	}
	other := "t_" + strings.Repeat("c", 32)
	for c, want := range map[string]bool{
		devWaitCommand("/bin/reale2e", "/tmp/rt", other, 7):                          true,
		devWaitCommand("/bin/reale2e", "/tmp/rt", task, 2):                           true,
		devWaitCommand("/bin/other", "/tmp/rt", task, 2):                             false,
		devWaitCommand("/bin/reale2e", "/tmp/rt2", task, 2):                          false,
		devWaitCommand("/bin/reale2e", "/tmp/rt", "t_x", 2):                          false,
		"/bin/reale2e dev-wait-task --run /tmp/rt --task " + task + " --ordinal 02":  false,
		"/bin/reale2e dev-wait-task --run /tmp/rt --task " + task + " --ordinal 0":   false,
		"/bin/reale2e dev-wait-task --run /tmp/rt --task " + task + " --ordinal 1 &": false,
		"while true; do sleep 1; done":                                               false,
	} {
		if got := devWaitGrammar(c, prefix); got != want || devWaitGrammar(c, "") {
			t.Errorf("grammar %q: %v want %v", c, got, want)
		}
	}
	// classOf classifies a literal input object.
	classOf := func(input string) devStartClass { return devWaitStartClass(json.RawMessage(input), prefix) }
	q := strconv.Quote
	for _, c := range []string{cmd, devWaitCommand("/bin/reale2e", "/tmp/rt", other, 1)} {
		for input, want := range map[string]devStartClass{
			`{"command":` + q(c) + `,"run_in_background":true}`:                                     devStartWait,
			`{"command":` + q(c) + `,"description":"w","run_in_background":true}`:                   devStartWait,
			`{"command":` + q(c) + `,"description":"w","timeout":900000,"run_in_background":true}`:  devStartWait,
			`{"command":` + q(c) + `,"timeout":1,"run_in_background":true}`:                         devStartWait,
			`{"command":` + q(c) + `,"timeout": 99999999999999999999999 ,"run_in_background":true}`: devStartWait,
			`{"command":` + q(c) + `,"timeout":1.0,"run_in_background":true}`:                       devStartInvalid,
			`{"command":` + q(c) + `,"timeout":1e3,"run_in_background":true}`:                       devStartInvalid,
			`{"command":` + q(c) + `,"timeout":0,"run_in_background":true}`:                         devStartInvalid,
			`{"command":` + q(c) + `,"timeout":-1,"run_in_background":true}`:                        devStartInvalid,
			`{"command":` + q(c) + `,"timeout":"900000","run_in_background":true}`:                  devStartInvalid,
			`{"command":` + q(c) + `,"timeout":true,"run_in_background":true}`:                      devStartInvalid,
			`{"command":` + q(c) + `,"timeout":null,"run_in_background":true}`:                      devStartInvalid,
			`{"command":` + q(c) + `,"timeout":[1],"run_in_background":true}`:                       devStartInvalid,
			`{"command":` + q(c) + `,"timeout":1,"timeout":1,"run_in_background":true}`:             devStartInvalid,
			`{"command":` + q(c) + `,"description":"a","description":"a","run_in_background":true}`: devStartInvalid,
			`{"command":` + q(c) + `,"run_in_background":true,"run_in_background":true}`:            devStartInvalid,
			`{"command":` + q(c) + `,"run_in_background":true,"dangerouslyDisableSandbox":true}`:    devStartInvalid,
			`{"command":` + q(c) + `,"run_in_background":true,"timeout_ms":1}`:                      devStartInvalid,
			`{"command":` + q(c) + `}`:                                                                devStartInvalid,
			`{"command":` + q(c) + `,"run_in_background":false}`:                                      devStartInvalid,
			`{"command":` + q(c) + `,"run_in_background":"true"}`:                                     devStartInvalid,
			`{"command":` + q(c) + `,"run_in_background":null}`:                                       devStartInvalid,
			`{"command":"while true; do sleep 1; done","timeout":1.5,"run_in_background":true,"x":1}`: devStartUnrelated,
			`{"command":"while true; do sleep 1; done"}`:                                              devStartUnrelated,
			`{"command":1,"run_in_background":true}`:                                                  devStartUnrelated,
			`[]`:                                                                                      devStartUnrelated,
		} {
			if got := classOf(input); got != want {
				t.Errorf("class %s: %d want %d", input, got, want)
			}
		}
	}
	// Through the slot's derivation, current slot and prior slot alike:
	// a valid timeout start is recognized; every invalid grammar start
	// fails wait_command_mismatch at its start, background or not, and is
	// never an unrelated job.
	startWith := func(id, c, extra string) {
		raw(`{"type":"assistant","session_id":"` + testSession + `","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":` +
			q(c) + extra + `}}]}}`)
	}
	prior := devWaitCommand("/bin/reale2e", "/tmp/rt", other, 1)
	for _, timeout := range []string{"1", "900000", "99999999999999999999999"} {
		if got, p := qualify(func() {
			startWith("tp", prior, `,"timeout":`+timeout+`,"run_in_background":true`)
			result("tp", "Command running in background with ID: b0. Output is being written to: /tmp/z", false)
			sysNote("b0", "tp")
			startWith("tu", cmd, `,"description":"w","timeout":`+timeout+`,"run_in_background":true`)
			result("tu", "Command running in background with ID: b1. Output is being written to: /tmp/x", false)
			note("b1", "tu")
			consume("ts")
		}); p != "" || !got.complete() {
			t.Errorf("timeout %s: %+v %s", timeout, got, p)
		}
	}
	for _, extra := range []string{`,"timeout":1.0,"run_in_background":true`, `,"timeout":0,"run_in_background":true`, `,"timeout":"1","run_in_background":true`,
		`,"timeout":null,"run_in_background":true`, `,"run_in_background":true,"x":1`, `,"run_in_background":false`, ``, `,"run_in_background":1`} {
		for slot, c := range map[string]string{"current": cmd, "prior": prior} {
			var at int
			if _, p, line := judge(func() { startWith("tz", c, extra); at = lines }); p != devWaitMismatch {
				t.Errorf("%s slot %s: %q at %d (start at %d)", slot, extra, p, line, at)
			}
		}
	}
	// A duplicate key makes the record refused by the strict decoder: a
	// dev-wait-task start (with the known prefix) is wait_command_mismatch,
	// at its record; without the prefix it stays session_malformed.
	for slot, c := range map[string]string{"current": cmd, "prior": prior} {
		buf.Reset()
		lines = 0
		w(map[string]any{"type": "system", "subtype": "init", "permissionMode": "auto"})
		startWith("td", c, `,"timeout":1,"timeout":1,"run_in_background":true`)
		if s := decodeDevStreamFor("phase-stream", buf.Bytes(), "", prefix); s.problem != devWaitMismatch || s.problemSrc.FirstLine != lines {
			t.Errorf("%s duplicate timeout: %q at %d", slot, s.problem, s.problemSrc.FirstLine)
		}
		if s := decodeDevStream(buf.Bytes(), ""); s.problem != devSessionMalformed {
			t.Errorf("%s duplicate timeout without the prefix: %q", slot, s.problem)
		}
		em := deriveDevEventMap(devMapInput{streams: map[int][]byte{1: buf.Bytes()}, phases: 1, waitPrefix: prefix})
		if em.Phases[0].Problem != devWaitMismatch {
			t.Errorf("%s duplicate timeout event map %+v", slot, em.Phases[0])
		}
	}
	// With an accepted timeout start, a timeout-stopped wait's notification
	// is refused at once, in either form; no false wake.
	for form, stopped := range map[string]func(){
		"system": func() {
			w(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "b1", "tool_use_id": "tu", "status": "stopped"})
		},
		"xml": func() {
			w(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>tu</tool-use-id>\n<status>stopped</status>\n</task-notification>"}})
		},
	} {
		var at int
		if got, p, line := judge(func() { capturedStart(); stopped(); at = lines }); p != devWakeUnsupported || line != at || got.Woke {
			t.Errorf("%s stopped wait: %q at %d want %d", form, p, line, at)
		}
	}
	// Stream-level problems.
	for name, c := range map[string]struct {
		lines string
		want  string
	}{
		"not_auto":    {`{"type":"system","subtype":"init","permissionMode":"default","session_id":"s"}`, devAutoModeMissing},
		"denied":      {`{"type":"result","subtype":"success","is_error":false,"session_id":"s","permission_denials":[{"tool_name":"Bash","tool_use_id":"x"}]}`, devToolDenied},
		"error":       {`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s"}`, devResultFailed},
		"malformed":   {`{not json`, devSessionMalformed},
		"long_poll":   {`{"type":"assistant","session_id":"s","message":{"content":[{"type":"tool_use","id":"p","name":"` + toolTaskWait + `","input":{}}]}}`, devLongPoll},
		"tool_failed": {`{"type":"user","session_id":"s","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"no","is_error":true}]}}`, devToolFailed},
	} {
		if s := decodeDevStream([]byte(c.lines+"\n"), ""); s.problem != c.want {
			t.Errorf("%s: %q want %q", name, s.problem, c.want)
		}
	}
	if s := decodeDevStream([]byte(`{"type":"system","subtype":"init","permissionMode":"auto","session_id":"a"}`+"\n"), "b"); s.problem != devSessionMismatch {
		t.Fatalf("resumed session %q", s.problem)
	}
	// The captured wait files.
	rt := shortTemp(t)
	if err := os.MkdirAll(waitDir(rt, 0), 0o700); err != nil {
		t.Fatal(err)
	}
	f := readDevWaitFiles(rt, 1)
	var rec DevWaitRecord
	if f.captured(task, &rec) || f.OKX {
		t.Fatal("no files captured")
	}
	wr, _ := json.Marshal(contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 1, Winner: task, Task: &view})
	os.WriteFile(filepath.Join(waitDir(rt, 0), runtimeWaitResult), append(wr, '\n'), 0o600)
	os.WriteFile(filepath.Join(waitDir(rt, 0), runtimeWaitStderr), nil, 0o600)
	os.WriteFile(filepath.Join(waitDir(rt, 0), runtimeWaitExit), []byte("1\n"), 0o600)
	if f := readDevWaitFiles(rt, 1); f.captured(task, &rec) {
		t.Fatal("a nonzero wait exit")
	}
	os.Remove(filepath.Join(waitDir(rt, 0), runtimeWaitExit))
	os.WriteFile(filepath.Join(waitDir(rt, 0), runtimeWaitExit), []byte("0\n"), 0o600)
	if f := readDevWaitFiles(rt, 1); !f.captured(task, &rec) || rec.Winner != task || f.captured("t_cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd", &DevWaitRecord{}) {
		t.Fatalf("captured %+v", rec)
	}
}

func TestDevFullControl(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"goal":"g"}` + "\n")
	p, sum, err := exposeDispatch(dir, 1, "g", body)
	if err != nil || sum != sha256Hex(mustRead(t, p)) {
		t.Fatalf("the advertised digest %s is not the file's %s", sum, sha256Hex(mustRead(t, p)))
	}
	if _, _, err := exposeDispatch(dir, 1, "g", body); err == nil {
		t.Fatal("an existing document was overwritten")
	}
	at := strings.Repeat("a", contract.MaxGoalBytes)
	if _, _, err := exposeDispatch(t.TempDir(), 1, at, body); err != nil {
		t.Fatalf("goal at MaxGoalBytes: %v", err)
	}
	if _, _, err := exposeDispatch(t.TempDir(), 1, at+"a", body); err == nil {
		t.Fatal("goal one byte over")
	}
	multi := strings.Repeat("é", contract.MaxGoalBytes/2)
	if _, _, err := exposeDispatch(t.TempDir(), 1, multi, body); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exposeDispatch(t.TempDir(), 1, multi+"é", body); err == nil {
		t.Fatal("multibyte overflow")
	}
	if _, _, err := exposeDispatch(t.TempDir(), 1, "\xff", body); err == nil {
		t.Fatal("invalid UTF-8 goal")
	}
	if _, _, err := exposeDispatch(t.TempDir(), 1, "g", bytes.Repeat([]byte("b"), maxDispatchDocByte+1)); err == nil {
		t.Fatal("document cap")
	}
	for _, s := range []string{"01", "0", "15", "-1", "1 ", " 1", "1e1", "", "+1", "１"} {
		if _, ok := devOrdinalOK(s); ok {
			t.Fatalf("accepted ordinal %q", s)
		}
	}
	if n, ok := devOrdinalOK("14"); !ok || n != 14 {
		t.Fatal("14")
	}
	for _, args := range [][]string{
		{"dev-next"}, {"dev-next", "--run", "relative"}, {"dev-next", "--run", "/tmp", "--extra", "x"},
		{"dev-observe", "--run", "/tmp", "--ordinal", "01", "--task", hexID("t_", 1)},
		{"dev-observe", "--run", "/tmp", "--ordinal", "1", "--task", "t_bad;rm"},
		{"dev-wait-task", "--run", "/tmp/x y", "--ordinal", "1"},
	} {
		var o, e bytes.Buffer
		if code := MainWith(Host{Args: args, Stdout: &o, Stderr: &e}); code != 2 {
			t.Errorf("%v = %d", args, code)
		}
	}
	sum64 := strings.Repeat("ab", 32)
	for name, c := range map[string]struct {
		op, line string
		ok       bool
	}{
		"prepared":       {opDevNext, `{"ok":true,"code":"prepared","path":"/rt/d.json","digest":"` + sum64 + `","ordinal":1,"role":"designer","mode":"draft"}`, true},
		"paused":         {opDevNext, `{"ok":true,"code":"paused","summary":"gate=owner generation=1"}`, true},
		"complete":       {opDevNext, `{"ok":true,"code":"complete"}`, true},
		"pending":        {opDevNext, `{"ok":true,"code":"pending"}`, true},
		"pending_other":  {opDevObserve, `{"ok":true,"code":"pending"}`, false},
		"pending_fields": {opDevNext, `{"ok":true,"code":"pending","ordinal":1}`, false},
		"refused":        {opDevNext, `{"ok":false,"code":"seed_unpublished"}`, true},
		"accepted":       {opDevWait, `{"ok":true,"code":"accepted"}`, true},
		"relative":       {opDevNext, `{"ok":true,"code":"prepared","path":"d.json","digest":"` + sum64 + `","ordinal":1,"role":"designer","mode":"draft"}`, false},
		"bad_digest":     {opDevNext, `{"ok":true,"code":"prepared","path":"/d.json","digest":"x","ordinal":1,"role":"designer","mode":"draft"}`, false},
		"bad_ordinal":    {opDevNext, `{"ok":true,"code":"prepared","path":"/d.json","digest":"` + sum64 + `","ordinal":15,"role":"designer","mode":"draft"}`, false},
		"bad_gate":       {opDevNext, `{"ok":true,"code":"paused","summary":"gate=yes"}`, false},
		"extra_field":    {opDevObserve, `{"ok":false,"code":"invalid_request","path":"/tmp/x"}`, false},
		"unknown":        {opDevObserve, `{"ok":true,"code":"nope"}`, false},
		"ok_mismatch":    {opDevObserve, `{"ok":false,"code":"accepted"}`, false},
		"prepared_other": {opDevWait, `{"ok":true,"code":"complete"}`, false},
		"array":          {opDevNext, `[]`, false},
		"dup":            {opDevNext, `{"ok":true,"ok":true,"code":"complete"}`, false},
		"legacy_code":    {opDevObserve, `{"ok":false,"code":"invalid_hop"}`, false},
	} {
		if _, err := decodeDevResponse(c.op, []byte(c.line)); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The legacy code lists are unchanged by the development codes.
	if slices.Contains(okCodes, CodePrepared) || slices.Contains(errorCodes, CodeDuplicateWait) || len(okCodes) != 2 || len(errorCodes) != 8 {
		t.Fatal("legacy codes changed")
	}
	if !strings.Contains(Usage, "run-dev-full") || !strings.Contains(Usage, "deprecated alias of run") || !strings.Contains(Usage, "no longer exits 3") {
		t.Fatal("usage does not document the run-dev-full alias")
	}
}

func TestDevFullDeltaBrief(t *testing.T) {
	d := &devSupervisor{s: &supervisor{runID: "20261017T000000Z-01020304"}, trees: map[string]map[string]TreeFile{}, finals: map[string]string{},
		loaded: DevLoaded{Rendered: map[string][]byte{DevScenario: []byte("scenario\n")}}}
	tree := func(files map[string]string) map[string]TreeFile {
		m := map[string]TreeFile{}
		for p, c := range files {
			m[p] = TreeFile{Path: p, Mode: filemode.Regular, Content: []byte(c)}
		}
		return m
	}
	st, _ := InitialPlan(commitOf("seed"))
	d.trees[commitOf("seed")] = tree(map[string]string{"FEATURE.md": "f\n"})
	draft1 := admit(st, markerDesignDraft)
	d.trees[draft1.Commit] = tree(map[string]string{"FEATURE.md": "f\n", "design.md": "d1\n", "revision-history.md": "h1\n"})
	st, _ = drive(t, st, draft1)
	d.state = st
	first, _, _, err := d.envelope(st.Pending)
	if err != nil || !strings.Contains(first, "audit_round: first audit; use the complete checklist") || !strings.Contains(first, "design_sha256: "+sha256Hex([]byte("d1\n"))) {
		t.Fatalf("round one %v\n%s", err, first)
	}
	a1 := admit(st, markerDesignChanges)
	d.trees[a1.Commit] = tree(map[string]string{"FEATURE.md": "f\n", "design.md": "d1\n", "revision-history.md": "h1\n", "design-review.md": "r1\n"})
	a1.ReviewSHA = sha256Hex([]byte("r1\n"))
	a1.DesignSHA = sha256Hex([]byte("d1\n"))
	st, _ = drive(t, st, a1)
	d.state = st
	if g, _, _, err := d.envelope(st.Pending); err != nil || !strings.Contains(g, "prior_audit: task "+a1.TaskID) {
		t.Fatalf("revision %v\n%s", err, g)
	}
	draft2 := admit(st, markerDesignDraft)
	d.trees[draft2.Commit] = tree(map[string]string{"FEATURE.md": "f\n", "design.md": "d2\n", "revision-history.md": "h1\n", "design-review.md": "r1\n"})
	st, _ = drive(t, st, draft2)
	d.state = st
	delta, _, inputs, err := d.envelope(st.Pending)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"prior_audit: task " + a1.TaskID + " commit " + a1.Commit + " design-review.md sha256 " + sha256Hex([]byte("r1\n")),
		"prior_design: design.md sha256 " + sha256Hex([]byte("d1\n")) + " at commit " + draft1.Commit,
		"- design.md " + sha256Hex([]byte("d1\n")) + " -> " + sha256Hex([]byte("d2\n")) + " (changed)",
		"- revision-history.md " + sha256Hex([]byte("h1\n")) + " -> " + sha256Hex([]byte("h1\n")) + " (unchanged)",
		"a new worker session does not make this round one",
	} {
		if !strings.Contains(delta, want) {
			t.Fatalf("delta lacks %q:\n%s", want, delta)
		}
	}
	if len(inputs) != 1 || inputs[0].SHA256 != sha256Hex([]byte("r1\n")) {
		t.Fatalf("inputs %+v", inputs)
	}
	// An unreadable prior report refuses the plan rather than starting an
	// unbound audit.
	delete(d.trees, a1.Commit)
	if _, _, _, err := d.envelope(st.Pending); err == nil || codeOf(err) != "input_unavailable" {
		t.Fatalf("missing prior audit: %v", err)
	}
	// Over-budget envelopes are refused, never truncated.
	d.trees[a1.Commit] = tree(map[string]string{"design-review.md": "r1\n"})
	d.loaded.Rendered[DevScenario] = []byte(strings.Repeat("s", contract.MaxGoalBytes))
	if _, _, _, err := d.envelope(st.Pending); codeOf(err) != CodePromptBudget {
		t.Fatalf("oversized envelope %v", err)
	}
	// The shipped delta policy names every tag in both reviewer manuals.
	for _, name := range []string{"design-reviewer-instruction.md", "design-reviewer-runbook.md", "coordinator-playbook.md"} {
		b := string(mustRead(t, filepath.Join(repoRoot(t), filepath.FromSlash(DevFlowDir), name)))
		for _, tag := range []string{"persisting", "regression", "new-on-unchanged"} {
			if !strings.Contains(b, tag) {
				t.Fatalf("%s lacks %s", name, tag)
			}
		}
	}
}

func BenchmarkDevFullTemplates(b *testing.B) {
	root := repoRoot(b)
	vals := map[string]string{}
	for _, k := range DevPromptKeys {
		vals[k] = "/v/" + k
	}
	loaded, err := LoadDevMethod(root)
	if err != nil {
		b.Fatal(err)
	}
	var total int64
	for _, s := range loaded.Sources {
		total += int64(s.Bytes)
	}
	want := sha256Hex(loaded.Files[DevPlaybook])
	b.SetBytes(total)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l, err := LoadDevMethod(root)
		if err != nil || len(l.Sources) != 15 {
			b.Fatal(err)
		}
		out, err := RenderDevPrompts(l, vals)
		if err != nil || len(out.Rendered) != 4 || sha256Hex(out.Files[DevPlaybook]) != want || !bytes.HasPrefix(out.Rendered[DevSystemName], out.Files[DevPlaybook]) {
			b.Fatal("render")
		}
	}
}

// maxTree is the largest permitted tree: the source fixture one byte below
// 16 KiB and the seven artifacts at exactly 128 KiB together.
func maxTree() (base, result []TreeFile, total int64) {
	src := map[string]int{"FEATURE.md": 8191, "go.mod": 1024, "main.go": 4096, "main_test.go": 3072}
	for _, name := range devSourceFiles {
		base = append(base, TreeFile{Path: name, Mode: filemode.Regular, Content: bytes.Repeat([]byte("s"), src[name])})
	}
	sizes := map[string]int{"design.md": 32 << 10, "revision-history.md": 16 << 10, "design-questions.md": 8 << 10, "design-review.md": 24 << 10,
		"rca.md": 16 << 10, "fix.md": 8 << 10, "review.md": 24 << 10}
	for _, name := range devArtifacts {
		if name != "review.md" {
			base = append(base, TreeFile{Path: name, Mode: filemode.Regular, Content: bytes.Repeat([]byte("a"), sizes[name])})
		}
	}
	result = append(append([]TreeFile{}, base...), TreeFile{Path: "review.md", Mode: filemode.Regular, Content: bytes.Repeat([]byte("r"), sizes["review.md"])})
	for _, f := range result {
		total += int64(len(f.Content))
	}
	return base, result, total
}

func BenchmarkDevFullAdmission(b *testing.B) {
	base, result, total := maxTree()
	if total != maxSourceFixtureBytes-1+maxArtifactAggregate {
		b.Fatalf("tree %d", total)
	}
	over := append(append([]TreeFile{}, result[:len(result)-1]...), TreeFile{Path: "review.md", Mode: filemode.Regular,
		Content: bytes.Repeat([]byte("r"), 24<<10+1)})
	b.SetBytes(total)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := AdmitTree("code-reviewer", modeReviewFull, markerReviewApproved, base, result); err != nil {
			b.Fatal(err)
		}
		bm, rm := index(base), index(result)
		if paths := changedPaths2(bm, rm); len(paths) != 1 || paths[0] != "review.md" {
			b.Fatal(paths)
		}
		if err := AdmitTree("code-reviewer", modeReviewFull, markerReviewApproved, base, over); err == nil {
			b.Fatal("the aggregate bound was not enforced")
		}
	}
}

func BenchmarkDevFullTransitions(b *testing.B) {
	path := []string{markerDesignDraft, markerDesignChanges, markerDesignDraft, markerDesignChanges, markerDesignDraft, markerDesignApproved, "owner",
		markerImplComplete, markerReviewChanges, markerFixesComplete, markerReviewChanges, markerFixesComplete}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		st, res := script(b, path...)
		if res.Action != actionDispatch || len(st.Tasks) != 11 || st.DesignCycles != 2 || st.CodeCycles != 2 || len(st.Tasks) > maxWorkflowTasks {
			b.Fatalf("loops %+v", res)
		}
		final, res := drive(b, st, admit(st, markerReviewChanges))
		if res.Stop != stopCap || len(final.Tasks) != 12 || final.Pending != nil {
			b.Fatalf("cap %+v", res)
		}
		full := st
		full.Tasks = make([]TaskRecord, maxWorkflowTasks)
		if _, res := schedule(full, TaskPlan{Role: "code-reviewer", Mode: modeReviewFull}, "x"); res.Stop != stopCap {
			b.Fatal("the 14-slot ledger accepted a fifteenth plan")
		}
	}
}
