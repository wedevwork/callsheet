// Light development-flow delivery contracts (slice 2d.4, design
// 12a-dev-flow-light, Delivery): the runbook's scope, commands, costs,
// helper clause, current selection and observed live status (design
// 12a-probe11-closeout, FP-1c), the examples'
// links and the light scenario's prompt size. The checks read shipped
// documentation only; they do not read design/, touch a vendor home or run
// a vendor CLI.
package function

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// headingAnchor is a markdown heading's GitHub anchor.
func headingAnchor(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// localAnchors checks every same-document anchor link of md names a
// heading of md.
func localAnchors(md string) error {
	heads := map[string]bool{}
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "#") {
			heads[headingAnchor(strings.TrimLeft(l, "#"))] = true
		}
	}
	for _, m := range regexp.MustCompile(`]\(#([^)\s]+)\)`).FindAllStringSubmatch(md, -1) {
		if !heads[m[1]] {
			return fmt.Errorf("the anchor #%s names no heading", m[1])
		}
	}
	return nil
}

func TestDevFlowLightDelivery(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	e2e := string(repoFile(t, "docs/real-e2e.md"))
	t.Run("scope_and_commands", func(t *testing.T) {
		if err := requirePhrases(e2e, []string{
			"`reale2e run-dev-light`", "`reale2e run-dev-exercise`", "/abs/path/reale2e run-dev-light", "/abs/path/reale2e run-dev-exercise",
			"`probe-dev-full --scenario full|light|both`", "`whitespace-restoration/v1`", "`light <attempt-id>` or `no <attempt-id>`",
			"`exercise <exercise-id>`", "`callsheet-dev-evidence/v2`", "`callsheet-dev-full-evidence/v1`", "`callsheet-dev-exercise/v1`",
			"`callsheet-dev-qualification/v2`", "`callsheet-dev-full-qualification/v1`", "`_captured/L.bin`", "`needs_design`",
			"This intake is not a design sign-off", "The nine-case seed suite and the ten-case final suite differ by design",
			"red means exit 1 with exactly `TestRun/whitespace` failing", "Assertion messages are never matched",
			"exits 0 for a complete mechanical PASS, 1 for a semantic failure and 2 for unsafe, malformed or unsupported evidence",
			"never claims a light or combined pass", "move an old bundle outside the module yourself",
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("costs", func(t *testing.T) {
		if err := requirePhrases(e2e, []string{
			"at most nine workflow tasks and 14 paid worker calls", "the happy path is 8 calls", "at most 33 paid worker calls (full 19 plus light 14)",
			"17 on the happy path (full 9 plus light 8)", "At most eleven worker calls", "90 + 2 + 90 + 2 = 184 minutes (204 with that 20-minute window)",
			"Monetary cost is unknown", "not counted in the 33 calls", "up to 28 worker calls",
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("helper_clause", func(t *testing.T) {
		runbook := string(repoFile(t, "docs/examples/dev-flow/code-reviewer-runbook.md"))
		if err := requirePhrases(runbook, []string{
			"This clause is conditional on such an envelope.", "For a light review the envelope's helper command carries `--scenario light`",
			"exports the initial source base into `baseline/`", "the initial source base with the candidate's complete `main_test.go` into `before/`",
			"and the candidate into `after/`", "exact covered-over-total fraction, never a rounded percentage", "the root-cause, assertion and restoration judgments remain yours",
		}); err != nil {
			t.Fatal(err)
		}
		if err := requirePhrases(e2e, []string{
			"`reale2e dev-review-verify --task ID --candidate COMMIT --initial-source COMMIT --go ABS --deadline YYYY-MM-DDTHH:MM:SSZ --scenario light`",
			"`callsheet-dev-light-review-run/v1`", "the red line states the expected exit 1 and the expected assertion failure of `TestRun/whitespace`",
			"A full receipt never stands in for a light review",
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("selection", func(t *testing.T) {
		var flow struct {
			Roles []struct {
				ID      string `json:"id"`
				Adapter string `json:"adapter"`
				Model   string `json:"model"`
				Effort  string `json:"effort"`
			} `json:"roles"`
		}
		if err := json.Unmarshal(repoFile(t, "docs/examples/real-e2e/dev-full/flow.json"), &flow); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{}
		for _, r := range canonicalRoles {
			want[r.Name] = r.Adapter + "/" + r.Model + "/" + r.Effort
		}
		for _, r := range flow.Roles {
			if want[r.ID] != r.Adapter+"/"+r.Model+"/"+r.Effort {
				t.Errorf("flow.json %s is %s/%s/%s", r.ID, r.Adapter, r.Model, r.Effort)
			}
			if !strings.Contains(e2e, fmt.Sprintf("| %s | %s | %s | %s |", r.ID, r.Adapter, r.Model, r.Effort)) {
				t.Errorf("the runbook's role table lacks %s", r.ID)
			}
		}
		// Design 12a-codex-triage: light triage is Codex sol/high.
		if want["triage"] != "codex/gpt-6.1-sol/high" || want["coder"] != "claude/claude-opus-5-5/high" || want["code-reviewer"] != "codex/gpt-6.1-sol/high" {
			t.Fatalf("the light roles' current selection %v", want)
		}
	})
	t.Run("live_observed", func(t *testing.T) {
		const report = "`design/real-e2e-runs/20261026-exercise/report.txt`"
		const fullAudit = "`design/real-e2e-runs/20261026-exercise/attempts/full/audit-review.md`"
		mutateExpect(t, map[string]string{"e2e": e2e}, checkLiveCloseout, "e2e", append(deletions(liveCloseoutClauses...),
			// The live paragraph: status, MET, qualification, exercise, audits.
			[2]string{"Live status: observed;", "Live status: pending;"},
			[2]string{"roadmap 12a's live condition is MET", "roadmap 12a's live condition is pending"},
			[2]string{"Probe 11 is QUALIFIED with `--scenario both` at main `387759a`.", "Probe 11 is QUALIFIED with `--scenario full` at main `387759a`."},
			[2]string{"Probe 11 is QUALIFIED with `--scenario both` at main `387759a`.", "Probe 11 is BLOCKED with `--scenario both` at main `387759a`."},
			[2]string{"Combined exercise `20261026T102944Z-d3827a6b` is PASS", "Combined exercise `20261026T102944Z-d3827a6b` is FAIL"},
			[2]string{"with both full and light children mechanical PASS", "with the full child mechanical PASS"},
			[2]string{"judgment audits are complete: all applicable child judgment rows are correct", "judgment audits are pending"},
			[2]string{"in the owner evidence under `design/`, not here", "here"},
			// Obsolete pending claims appended beside the correct paragraph.
			[2]string{liveStatusParagraph, liveStatusParagraph + " Live status: pending."},
			[2]string{"\n\n## Prerequisites\n", "\n\nNo owner-run qualification probe or full live attempt of this method has been recorded yet.\n\n## Prerequisites\n"},
			[2]string{"\n\n## Prerequisites\n", "\n\nThe light attempt and the combined exercise have no recorded live run either.\n\n## Prerequisites\n"},
			[2]string{"\n\n## Prerequisites\n", "\n\nThis runbook documents a checked, offline-tested harness, not a demonstrated live PASS.\n\n## Prerequisites\n"},
			// The exercise and audit citations.
			[2]string{report, "`design/real-e2e-runs/20261026-exercise/report.json`"},
			[2]string{"full judgment audit " + fullAudit + " and ", "full judgment audit and "},
			[2]string{" and light judgment audit `design/real-e2e-runs/20261026-exercise/attempts/light/audit-review.md`", ""},
			[2]string{"are complete and record all four judgment rows in each child as correct", "are pending review"},
			[2]string{"the mechanical report alone does not approve model judgment", "the mechanical report approves model judgment"},
			[2]string{"passed on main `387759a`", "passed on main `f2ee9d2`"},
			[2]string{observationParagraph + "\n\n" + exerciseParagraph, exerciseParagraph + "\n\n" + observationParagraph},
			// Citations converted to links, or links appended beside them.
			[2]string{report, "[report.txt](../design/real-e2e-runs/20261026-exercise/report.txt)"},
			[2]string{fullAudit, fullAudit + " ([audit](../design/real-e2e-runs/20261026-exercise/attempts/full/audit-review.md))"},
			[2]string{"\n\n## Prerequisites\n", "\n\nSee [the exercise report](../design/real-e2e-runs/20261026-exercise/report.txt).\n\n## Prerequisites\n"},
		))
	})
	t.Run("links", func(t *testing.T) {
		if err := localAnchors(e2e); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e2e, "](#light-attempt)") || !strings.Contains(e2e, "](#combined-exercise)") {
			t.Fatal("the runbook does not link its light and exercise sections")
		}
		readme := string(repoFile(t, "docs/examples/real-e2e/README.md"))
		if !strings.Contains(readme, "](dev-full/scenario-light.md)") {
			t.Fatal("the exercise README does not link the light scenario")
		}
		if err := resolveDocLinks(filepath.Join(root, "docs", "examples", "real-e2e", "README.md"), readme); err != nil {
			t.Fatal(err)
		}
		devREADME := string(repoFile(t, "docs/examples/dev-flow/README.md"))
		if err := requirePhrases(devREADME, []string{"`reale2e run-dev-light` runs the light defect flow", "`reale2e run-dev-exercise` runs both as independent attempts"}); err != nil {
			t.Fatal(err)
		}
		coord := string(repoFile(t, "docs/examples/real-e2e/dev-full/coordinator-prompt.md"))
		if strings.Contains(coord, "full-flow attempt") || !strings.Contains(coord, "of the full or the light scenario") {
			t.Fatal("the coordinator prompt is not scenario-neutral")
		}
	})
	t.Run("light_scenario", func(t *testing.T) {
		scenario := repoFile(t, "docs/examples/real-e2e/dev-full/scenario-light.md")
		if err := requirePhrases(string(scenario), []string{
			"Exercise scenario (light flow).", "--scenario light", "TestRun/whitespace", "the one root-cause reference line the envelope specifies",
			"FEATURE.md and go.mod never change", "a design need stops the attempt",
		}); err != nil {
			t.Fatal(err)
		}
		// The scenario text is part of every light envelope: it leaves most
		// of the 8192-byte goal-plus-acceptance ceiling to the envelope, raw
		// and JSON-encoded.
		enc, err := encodedContentLen(scenario)
		if err != nil {
			t.Fatal(err)
		}
		if len(scenario) > triageTaskCeiling/2 || enc > triageTaskCeiling/2 {
			t.Fatalf("scenario-light.md is %d raw and %d encoded bytes", len(scenario), enc)
		}
		for _, bad := range forbiddenFlowPaths {
			if strings.Contains(string(scenario), bad) {
				t.Fatalf("the light scenario names %q", bad)
			}
		}
	})
}

// liveStatusParagraph is the runbook's opening live status (design
// 12a-probe11-closeout, FP-1c: the owner's 2026-10-26 MET decision).
const liveStatusParagraph = "Live status: observed; roadmap 12a's live condition is MET. Probe 11 is QUALIFIED with `--scenario both` at main `387759a`. Combined exercise `20261026T102944Z-d3827a6b` is PASS, with both full and light children mechanical PASS. The owner's external coordinator judgment audits are complete: all applicable child judgment rows are correct. A full-only historical pass does not complete roadmap 12a: that needs a current `--scenario both` qualification, then one owner-run combined exercise whose full and light children both pass mechanically, and the owner's external judgment audit of every applicable child row. The owner records the live command, the merged revision, the qualification scope, the exercise and child paths and every paid call in the owner evidence under `design/`, not here. See [Codex triage qualification](#codex-triage-qualification) for the observed qualification and exercise records; historical evidence paths are not reusable output paths."

// exerciseParagraph cites the combined exercise and its two completed
// judgment audits, immediately after the observed qualification (FP-1c).
const exerciseParagraph = "Combined exercise `20261026T102944Z-d3827a6b` passed on main `387759a`: its report `design/real-e2e-runs/20261026-exercise/report.txt` records PASS with both children mechanical PASS. The owner's external coordinator full judgment audit `design/real-e2e-runs/20261026-exercise/attempts/full/audit-review.md` and light judgment audit `design/real-e2e-runs/20261026-exercise/attempts/light/audit-review.md` are complete and record all four judgment rows in each child as correct. These owner records stay under `design/`; the mechanical report alone does not approve model judgment."

// liveCloseoutClauses are the live paragraph's and the exercise
// paragraph's sentences, each deleted by a mutation.
var liveCloseoutClauses = []string{
	"Live status: observed; roadmap 12a's live condition is MET.",
	"Probe 11 is QUALIFIED with `--scenario both` at main `387759a`.",
	"Combined exercise `20261026T102944Z-d3827a6b` is PASS, with both full and light children mechanical PASS.",
	"The owner's external coordinator judgment audits are complete: all applicable child judgment rows are correct.",
	"A full-only historical pass does not complete roadmap 12a: that needs a current `--scenario both` qualification, then one owner-run combined exercise whose full and light children both pass mechanically, and the owner's external judgment audit of every applicable child row.",
	"The owner records the live command, the merged revision, the qualification scope, the exercise and child paths and every paid call in the owner evidence under `design/`, not here.",
	"See [Codex triage qualification](#codex-triage-qualification) for the observed qualification and exercise records; historical evidence paths are not reusable output paths.",
	"Combined exercise `20261026T102944Z-d3827a6b` passed on main `387759a`: its report `design/real-e2e-runs/20261026-exercise/report.txt` records PASS with both children mechanical PASS.",
	"The owner's external coordinator full judgment audit `design/real-e2e-runs/20261026-exercise/attempts/full/audit-review.md` and light judgment audit `design/real-e2e-runs/20261026-exercise/attempts/light/audit-review.md` are complete and record all four judgment rows in each child as correct.",
	"These owner records stay under `design/`; the mechanical report alone does not approve model judgment.",
}

// staleLiveStatus are the pre-closeout live-pending claims, refused anywhere
// in the runbook even beside the correct paragraph.
var staleLiveStatus = []string{
	"Live status: pending", "No owner-run qualification probe or full live attempt of this method has been recorded yet",
	"The light attempt and the combined exercise have no recorded live run either.", "not a demonstrated live PASS",
}

// checkLiveCloseout validates FP-1c in docs/real-e2e.md: the exact opening
// live status, the exercise and audit citation paragraph right after the
// observed qualification, and no obsolete pending claim or evidence link.
func checkLiveCloseout(docs map[string]string) error {
	e2e := docs["e2e"]
	intro, _, _ := strings.Cut(e2e, "\n## ")
	if strings.TrimSuffix(paragraphWith(intro, "Live status:"), "\n") != liveStatusParagraph || strings.Count(e2e, "Live status:") != 1 {
		return fmt.Errorf("the opening live status is missing or changed")
	}
	sec := mdSubsection(e2e, "#### Owner requalification")
	if !strings.Contains(sec, "\n\n"+observationParagraph+"\n\n"+exerciseParagraph+"\n") || strings.Count(e2e, exerciseParagraph) != 1 {
		return fmt.Errorf("the exercise paragraph does not follow the observed qualification")
	}
	// The two exact paragraphs carry every liveCloseoutClauses entry.
	for _, s := range staleLiveStatus {
		if strings.Contains(e2e, s) {
			return fmt.Errorf("the runbook still carries %q", s)
		}
	}
	return checkNoEvidenceLink(e2e)
}
