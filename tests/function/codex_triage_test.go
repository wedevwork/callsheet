// Codex triage documentation contracts (design 12a-codex-triage, FP-2c,
// FP-4, FP-5a, FP-5b and FP-6), with design 12a-probe11-closeout's failed
// probe 10 history (FP-1a), probe 11's observed qualification (FP-1b) and
// the generic owner requalification gate (FP-1d). The checks read shipped documentation and
// in-memory mutations of those bytes, plus the adapter's own Codex recipe.
// They do not read design/, touch a vendor home or run a vendor CLI; the
// documented owner smoke is only syntax-checked, never executed.
package function

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// codexNoFallbackSentence is the exact no-fallback sentence (design
// 12a-codex-triage, Selection).
const codexNoFallbackSentence = "Codex triage uses only the shipped `exec --approve-for-me` recipe; do not add or substitute `--sandbox`, `-a` or `--yolo`, or change `~/.codex/config.toml` or any `AGENTS.md` to make a smoke or probe pass."

// codexTriageRow is the shipped triage selection, independently pinned.
var codexTriageRow = roleRow{"triage", "codex", "gpt-6.1-sol", "high", "1", "10m"}

// mdSubsection is a markdown section's text from its heading line to the
// next heading of the same or a higher level ("" when absent).
func mdSubsection(doc, heading string) string {
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	_, rest, ok := strings.Cut(doc, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	lines := strings.Split(rest, "\n")
	fenced := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			fenced = !fenced
		}
		if fenced || !strings.HasPrefix(l, "#") {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, "#")); n <= level && strings.HasPrefix(l[n:], " ") {
			return strings.Join(lines[:i], "\n")
		}
	}
	return rest
}

// mutateExpect applies each [old, new] replacement to docs[kind] (old must
// occur) and requires check to refuse the result.
func mutateExpect(t *testing.T, docs map[string]string, check func(map[string]string) error, kind string, muts [][2]string) {
	t.Helper()
	if err := check(docs); err != nil {
		t.Fatalf("the shipped documents fail: %v", err)
	}
	for _, m := range muts {
		if !strings.Contains(docs[kind], m[0]) {
			t.Fatalf("%s: the mutation target %q is not in the shipped document", kind, m[0])
		}
		changed := map[string]string{}
		for k, v := range docs {
			changed[k] = v
		}
		changed[kind] = strings.Replace(docs[kind], m[0], m[1], 1)
		if err := check(changed); err == nil {
			t.Errorf("%s: the mutation %q -> %q was accepted", kind, m[0], m[1])
		}
	}
}

// deletions are mutations deleting each phrase.
func deletions(phrases ...string) [][2]string {
	out := make([][2]string, len(phrases))
	for i, p := range phrases {
		out[i] = [2]string{p, ""}
	}
	return out
}

// codexDocs are the shipped documents these validators read.
func codexDocs(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"e2e":      string(repoFile(t, "docs/maintainers/real-e2e.md")),
		"readme":   string(repoFile(t, "docs/examples/dev-flow/README.md")),
		"playbook": string(repoFile(t, "docs/examples/dev-flow/coordinator-playbook.md")),
		"flow":     string(repoFile(t, "docs/examples/real-e2e/dev-full/flow.json")),
	}
}

// historicalRecordClauses are FP-2c's required clauses (the last two as
// generalized by design 12a-probe11-closeout, FP-1d replacements 1 and 2:
// the scope is the Grok-selection records, and the operator prerequisite is
// a generic owner requalification).
var historicalRecordClauses = []string{
	"Bundles and qualification records produced with the Grok triage selection are kept as they are, with no migration; the harness never rewrites them.",
	"Check each with the revision that produced it: this revision adds no cross-revision decoder and does not accept their old selection.",
	"A historical `callsheet-dev-full-qualification/v1` record keeps its full-only meaning.",
	"No historical qualification produced with the Grok triage selection, including an old PASS or QUALIFIED record, qualifies the Codex triage selection.",
	"A fresh owner requalification is an operator prerequisite you record yourself when requalification is needed; the harness does not look up a qualification file at startup.",
}

// numberedOwnerProbeRE finds a numbered owner probe in the historical-record
// policy, where only the generic owner requalification is current.
var numberedOwnerProbeRE = regexp.MustCompile(`(?i)\bowner probe \d`)

// checkHistoricalRecords validates FP-2c in docs/maintainers/real-e2e.md.
func checkHistoricalRecords(docs map[string]string) error {
	sec := mdSubsection(docs["e2e"], "#### Historical records")
	if sec == "" {
		return fmt.Errorf("no Historical records section")
	}
	if err := requirePhrases(sec, historicalRecordClauses); err != nil {
		return err
	}
	for _, bad := range []string{"migrated", "is converted", "qualifies the new selection", "still qualifies", "reused as qualification"} {
		if strings.Contains(sec, bad) {
			return fmt.Errorf("the historical-record policy says %q", bad)
		}
	}
	if m := numberedOwnerProbeRE.FindString(sec); m != "" {
		return fmt.Errorf("the historical-record policy names a numbered %q", m)
	}
	return nil
}

func TestCodexTriageFP2c(t *testing.T) {
	docs := codexDocs(t)
	mutateExpect(t, docs, checkHistoricalRecords, "e2e", append(deletions(historicalRecordClauses...),
		[2]string{"with no migration", "migrated to the new selection"},
		[2]string{"Check each with the revision that produced it", "Check each with this revision"},
		[2]string{"No historical qualification produced with the Grok triage selection, including an old PASS or QUALIFIED record, qualifies", "An old PASS or QUALIFIED record still qualifies"},
		// The unscoped pre-closeout clause would contradict probe 11.
		[2]string{"No historical qualification produced with the Grok triage selection, including", "No historical qualification, including"},
		[2]string{"the harness does not look up a qualification file at startup", "the harness looks up the latest qualification file at startup"},
		[2]string{"A fresh owner requalification is an operator prerequisite", "A fresh owner probe 10 is an operator prerequisite"},
		[2]string{"A fresh owner requalification is an operator prerequisite", "A fresh owner probe 12 is an operator prerequisite. A fresh owner requalification is an operator prerequisite"},
		[2]string{" when requalification is needed;", ";"},
		[2]string{"#### Historical records", "#### Old records"},
	))
	// No new runtime qualification lookup: no live command takes a
	// qualification input.
	main := string(repoFile(t, "internal/reale2e/main.go"))
	if strings.Contains(main, `"qualification"`) || strings.Contains(main, "--qualification") {
		t.Fatal("a live command takes a qualification input")
	}
}

// staleTriageCatalog are the obsolete Grok triage values in any current
// catalog, registration, posture or budget text.
var staleTriageCatalog = []string{
	"| triage | grok |", "--adapter grok", "grok-4.7", "xhigh", `"adapter": "grok"`, "adapter.MaxGrokPromptBytes", "`MaxGrokPromptBytes`",
	"Grok configuration change", "the Grok and Cursor worker postures are Linux only", "the grok worker posture",
}

// retainedGrokClauses are docs/maintainers/real-e2e.md's harness prerequisite and
// selection-change clauses.
var retainedGrokClauses = []string{
	"the harness requires Linux-qualified vendor installations and sidecars",
	"the shipped selection is Codex and Claude only, both eligible on Linux and macOS, and that never bypasses the live Linux gate",
	"A required, eligible Grok installation, although no shipped role selects Grok: `--grok` stays mandatory, absolute and version-checked, and its idle sidecar is enrolled, deployed and cleaned up with the other two.",
	"not evidence that a Grok task ran",
	"(`claude, codex, grok, go, git`)",
	"A missing or too-old Grok installation still stops before any paid work",
	"triage and the code reviewer both preflight on the Codex sidecar under their own role IDs",
	"an older local flow whose triage row still names `grok` is refused before any paid work as `flow_invalid`, never treated as an override or silently converted",
	"copy the new shipped `flow.json` and reapply only supported model or effort changes; do not copy the Grok model or effort into the Codex row",
	"every recovery triage of either flow",
}

// crossFamilyClauses are the playbook's pairing policy.
var crossFamilyClauses = []string{
	"triage (GPT) diagnoses for the coder (Opus), and the code reviewer (GPT) checks the Opus implementation",
	"Triage and the code reviewer may both be GPT: they judge different work, and the executing code reviewer, not triage, checks the implementation.",
}

// checkCodexCatalog validates FP-4: the flow, both role tables, the
// registration recipe, the runbook's role table, posture, budget text,
// the retained harness prerequisites and the exact no-fallback sentence.
func checkCodexCatalog(docs map[string]string) error {
	var flow struct {
		Roles []struct{ ID, Adapter, Model, Effort string } `json:"roles"`
	}
	if err := json.Unmarshal([]byte(docs["flow"]), &flow); err != nil {
		return err
	}
	tri := 0
	for _, r := range flow.Roles {
		if r.ID == "triage" {
			tri++
			if r.Adapter != codexTriageRow.Adapter || r.Model != codexTriageRow.Model || r.Effort != codexTriageRow.Effort {
				return fmt.Errorf("flow.json triage is %s/%s/%s", r.Adapter, r.Model, r.Effort)
			}
		}
	}
	if tri != 1 || len(flow.Roles) != 5 {
		return fmt.Errorf("flow.json has %d roles, %d triage", len(flow.Roles), tri)
	}
	for _, name := range []string{"readme", "playbook"} {
		rows, err := parseRoleTable(docs[name])
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !slices.Contains(rows, codexTriageRow) || !sameRoles(rows, canonicalRoles) {
			return fmt.Errorf("%s: the role table is not the Codex triage catalog", name)
		}
	}
	if err := checkRegistration(docs["readme"], canonicalRoles); err != nil {
		return err
	}
	if !strings.Contains(docs["e2e"], "| triage | codex | gpt-6.1-sol | high |") {
		return fmt.Errorf("the runbook's role table lacks the Codex triage row")
	}
	if err := checkTriagePosture(docs["readme"], docs["playbook"]); err != nil {
		return err
	}
	for _, name := range []string{"readme", "playbook"} {
		if !strings.Contains(docs[name], promptBudgetParagraph) {
			return fmt.Errorf("%s: the prompt budget does not name the product cap", name)
		}
	}
	if !strings.Contains(docs["readme"], "`internal/reale2e/devfull_budget.go` defines the product cap `triagePromptCap` as 32768, independent of any vendor adapter's own bound; the selected adapter's final check stays authoritative.") {
		return fmt.Errorf("the README does not define the product cap")
	}
	for name, doc := range docs {
		for _, s := range staleTriageCatalog {
			if strings.Contains(doc, s) && !(name == "e2e" && (s == "xhigh" || s == "grok-4.7")) {
				return fmt.Errorf("%s still carries %q", name, s)
			}
		}
	}
	if err := requirePhrases(docs["e2e"], retainedGrokClauses); err != nil {
		return err
	}
	if err := requirePhrases(docs["playbook"], crossFamilyClauses); err != nil {
		return err
	}
	// The exact no-fallback sentence, once, inside the scratch section's
	// unchanged no-fallback contract.
	if strings.Count(docs["e2e"], codexNoFallbackSentence) != 1 {
		return fmt.Errorf("the exact Codex no-fallback sentence is missing or repeated")
	}
	p := paragraphWith(docs["e2e"], codexNoFallbackSentence)
	if !strings.Contains(p, "There is no permission fallback: no sandbox override, bypass, always-approve or permission rule. "+codexNoFallbackSentence) ||
		!strings.Contains(mdSubsection(docs["e2e"], "### Light triage scratch"), p) {
		return fmt.Errorf("the no-fallback sentence is not in the scratch contract")
	}
	return nil
}

func TestCodexTriageFP4(t *testing.T) {
	docs := codexDocs(t)
	t.Run("e2e", func(t *testing.T) {
		mutateExpect(t, docs, checkCodexCatalog, "e2e", append(deletions(append(slices.Clone(retainedGrokClauses), codexNoFallbackSentence)...),
			[2]string{"| triage | codex | gpt-6.1-sol | high |", "| triage | grok | grok-4.7 | xhigh |"},
			[2]string{"| triage | codex | gpt-6.1-sol | high |", "| triage | codex | gpt-6.1-sol | xhigh |"},
			[2]string{"do not add or substitute `--sandbox`, `-a` or `--yolo`", "do not add or substitute `--sandbox`"},
			[2]string{"or change `~/.codex/config.toml` or any `AGENTS.md`", "or change any `AGENTS.md`"},
			[2]string{"no sandbox override, bypass, always-approve or permission rule. ", "no sandbox override, bypass, always-approve, permission rule or Grok configuration change. "},
			[2]string{"the harness requires Linux-qualified vendor installations and sidecars.", "the Grok and Cursor worker postures are Linux only."},
		))
	})
	t.Run("shared_docs", func(t *testing.T) {
		mutateExpect(t, docs, checkCodexCatalog, "readme", [][2]string{
			{"| triage | codex | gpt-6.1-sol | high | 1 | 10m |", "| triage | grok | grok-4.7 | xhigh | 1 | 10m |"},
			{"--adapter codex --instruction /srv/manuals/dev-flow/triage-instruction.md", "--adapter grok --instruction /srv/manuals/dev-flow/triage-instruction.md"},
			{"--model gpt-6.1-sol --effort high --concurrency 1 --timeout 10m --plane <plane-url> --ca <ca-path>\n```", "--model gpt-6.1-sol --effort medium --concurrency 1 --timeout 10m --plane <plane-url> --ca <ca-path>\n```"},
			{"(the product triage prompt cap `triagePromptCap`)", "(adapter.MaxGrokPromptBytes)"},
			{"defines the product cap `triagePromptCap` as 32768", "defines `MaxGrokPromptBytes` as 32768"},
			{codexTriageClause, "The triage role (grok) executes only on a Linux sidecar."},
			{harnessLinuxClause, "The harness runs anywhere"},
		})
		mutateExpect(t, docs, checkCodexCatalog, "playbook", append(deletions(crossFamilyClauses...),
			[2]string{"| triage | codex | gpt-6.1-sol | high | 1 | 10m |", "| triage | grok | grok-4.7 | xhigh | 1 | 10m |"},
			[2]string{"(the product triage prompt cap `triagePromptCap`)", "(adapter.MaxGrokPromptBytes)"},
			[2]string{"Verify readiness after the final registry change. Do not", "Verify readiness after the final registry change. The triage role runs only on a Linux sidecar. Do not"},
		))
		mutateExpect(t, docs, checkCodexCatalog, "flow", [][2]string{
			{`{"id": "triage", "adapter": "codex", "model": "gpt-6.1-sol", "effort": "high"}`, `{"id": "triage", "adapter": "grok", "model": "grok-4.7", "effort": "xhigh"}`},
			{`{"id": "triage", "adapter": "codex", "model": "gpt-6.1-sol", "effort": "high"}`, `{"id": "triage", "adapter": "codex", "model": "gpt-6.1-sol", "effort": "low"}`},
		})
	})
}

// failedProbeClauses are FP-5a's required clauses.
var failedProbeClauses = []string{
	"Probes 8 and 9b, both with Grok triage, failed and stay failed history; neither bundle is relabeled as qualified.",
	"Probe 8 remains failed evidence: `design/real-e2e-runs/20261024-probe8`, produced by revision `94ee4fc6209b21ec998617af4d2a5aeb3c243018`.",
	"`design/real-e2e-runs/20261025-probe9b`, produced by revision `25a770797f69612df1c21e325afa527445677609`.",
	"Both qualification records are `BLOCKED` with `triage_evidence/invalid_marker`",
	"their light review was not reached (`light_reviewer_evidence/not_reached`)",
	"even though probe 9b's full code reviewer stage passed with `REVIEW_APPROVED`",
	"A path outside the workspace did not by itself cause probe 8",
	"probe 9b failed the same way inside `.callsheet-triage-scratch/`",
	"it does not show that Codex triage succeeds",
	// Probe 10 (design 12a-probe11-closeout, FP-1a): failed history too.
	"Probe 10, with Codex triage, also remains failed history: qualification record `design/real-e2e-runs/20261025-probe10/qualification.json`, produced by revision `f2ee9d21760ca1c78c78b7bbddb663b73ebd8a4c`.",
	"Its result is `BLOCKED`: `triage_evidence` failed with `probe_triage_unqualified`, and `light_reviewer_evidence` was `not_reached`.",
	"The triage final text `design/real-e2e-runs/20261025-probe10/probe/triage/01/final.txt` reports an unnecessary export-mode guard failure and `STATUS: TRIAGE_BLOCKED`.",
	"Design 12a-triage-scratch-guard-wording subsequently corrected that instruction; probe 10 is not qualified.",
}

// qualifiedRE finds a qualification claim.
var qualifiedRE = regexp.MustCompile(`(?i)\bqualif(?:ied|ies|y)\b|\bpass(?:ed)?\b`)

// numberedGateRE finds a numbered current owner gate (design
// 12a-probe11-closeout, FP-1d): the live gate, the smoke's lead-in and stop
// rule, the gate heading and its introduction are generic owner
// requalification. Historical citations of numbered probes stay allowed.
var numberedGateRE = regexp.MustCompile(`The gate is (?:an )?owner probe \d|Before probe \d|[Ss]top before probe \d|#### Owner probe \d|[Rr]un owner probe \d|probe \d+ still settles`)

// checkNoNumberedGate refuses a numbered current owner gate anywhere in the
// runbook.
func checkNoNumberedGate(e2e string) error {
	if m := numberedGateRE.FindString(e2e); m != "" {
		return fmt.Errorf("the runbook carries a numbered current gate %q", m)
	}
	return nil
}

// evidenceLinkRE finds a Markdown link into the ignored evidence tree.
var evidenceLinkRE = regexp.MustCompile(`]\([^)\s]*(?:design/|real-e2e-runs)[^)\s]*\)`)

// checkNoEvidenceLink refuses a Markdown link to an evidence artifact: the
// runbook cites the ignored design/ tree only by backticked
// repository-relative path (design 12a-probe11-closeout, Qualification
// closeout).
func checkNoEvidenceLink(e2e string) error {
	if m := evidenceLinkRE.FindString(e2e); m != "" {
		return fmt.Errorf("the runbook links the ignored evidence tree: %q", m)
	}
	return nil
}

// checkFailedProbes validates FP-5a in docs/maintainers/real-e2e.md.
func checkFailedProbes(docs map[string]string) error {
	e2e := docs["e2e"]
	sec := mdSubsection(e2e, "#### Failed probe history")
	if sec == "" || !strings.Contains(mdSubsection(e2e, "### Codex triage qualification"), sec) {
		return fmt.Errorf("no Failed probe history section under Codex triage qualification")
	}
	if err := requirePhrases(sec, failedProbeClauses); err != nil {
		return err
	}
	// No bundle is relabeled: every sentence claiming a pass or
	// qualification is negated or is the reviewer stage's own pass.
	for _, s := range regexp.MustCompile(`\.\s+`).Split(sec, -1) {
		if !qualifiedRE.MatchString(s) {
			continue
		}
		if !strings.Contains(s, "neither") && !strings.Contains(s, " not ") && !strings.Contains(s, "reviewer stage passed") {
			return fmt.Errorf("a failed probe is relabeled: %q", s)
		}
	}
	// The pending probe 9 gate is gone, as is every numbered gate.
	for _, gone := range []string{"owner probe 9:", "A probe 9 failure", "The gate is owner probe 9"} {
		if strings.Contains(e2e, gone) {
			return fmt.Errorf("the runbook still carries %q", gone)
		}
	}
	if err := checkNoNumberedGate(e2e); err != nil {
		return err
	}
	return checkNoEvidenceLink(e2e)
}

func TestCodexTriageFP5a(t *testing.T) {
	docs := codexDocs(t)
	const probe10Record = "`design/real-e2e-runs/20261025-probe10/qualification.json`"
	mutateExpect(t, docs, checkFailedProbes, "e2e", append(deletions(failedProbeClauses...),
		[2]string{"neither bundle is relabeled as qualified", "both bundles are relabeled as qualified"},
		[2]string{"it does not show that Codex triage succeeds.", "it does not show that Codex triage succeeds. Probe 9b is QUALIFIED."},
		[2]string{"`94ee4fc6209b21ec998617af4d2a5aeb3c243018`", "`25a770797f69612df1c21e325afa527445677609`"},
		[2]string{"(`light_reviewer_evidence/not_reached`)", "(`light_reviewer_evidence/pass`)"},
		[2]string{"#### Failed probe history", "#### Probe history"},
		// Probe 10: false qualification appended or substituted, a wrong
		// producing revision or result, and a linked evidence citation.
		[2]string{"probe 10 is not qualified.", "probe 10 is not qualified. Probe 10 is QUALIFIED."},
		[2]string{"probe 10 is not qualified.", "probe 10 is not qualified. Probe 10 passed."},
		[2]string{"probe 10 is not qualified.", "probe 10 is qualified."},
		[2]string{"Its result is `BLOCKED`", "Its result is `QUALIFIED`"},
		[2]string{"`f2ee9d21760ca1c78c78b7bbddb663b73ebd8a4c`", "`387759a78caa62dc93723d42039dccb508776ca6`"},
		[2]string{"`light_reviewer_evidence` was `not_reached`", "`light_reviewer_evidence` passed"},
		[2]string{probe10Record, "[qualification.json](../design/real-e2e-runs/20261025-probe10/qualification.json)"},
		[2]string{probe10Record, probe10Record + " ([record](../design/real-e2e-runs/20261025-probe10/qualification.json))"},
		[2]string{"probe 10 is not qualified.", "probe 10 is not qualified. See [the record](../design/real-e2e-runs/20261025-probe10/qualification.json)."},
		// Stale numbered gates inserted beside the generic one.
		[2]string{"The gate is an owner requalification:", "The gate is owner probe 9: `probe-dev-full --scenario both`. The gate is an owner requalification:"},
		[2]string{"The gate is an owner requalification:", "The gate is owner probe 10: `probe-dev-full --scenario both`. The gate is an owner requalification:"},
		[2]string{"The gate is an owner requalification:", "The gate is owner probe 11: `probe-dev-full --scenario both`. The gate is an owner requalification:"},
	))
	t.Run("historical_mentions", func(t *testing.T) {
		// Numbered probes and dates in historical citations stay accepted.
		e2e := docs["e2e"]
		if !strings.Contains(mdSubsection(e2e, "#### Failed probe history"), "Probe 10, with Codex triage") || !strings.Contains(e2e, "Probe 11 qualified the Codex triage selection on 2026-10-26") {
			t.Fatal("the runbook does not cite probes 10 and 11 historically")
		}
		changed := map[string]string{"e2e": strings.Replace(e2e, "probe 10 is not qualified.", "probe 10 is not qualified. Probe 10 ran on 2026-10-25 before probe 11.", 1), "flow": docs["flow"]}
		for name, check := range map[string]func(map[string]string) error{"failed": checkFailedProbes, "requalification": checkOwnerRequalification, "smoke": checkSmoke} {
			if err := check(changed); err != nil {
				t.Errorf("%s refuses a historical mention: %v", name, err)
			}
		}
	})
}

// ownerRequalificationCommand is the documented generic owner
// requalification command (design 12a-probe11-closeout, FP-1d): no fixed or
// dated evidence path.
const ownerRequalificationCommand = `CALLSHEET_REAL_E2E=1 /abs/reale2e probe-dev-full --scenario both --callsheet /abs/callsheet --claude /abs/claude --codex /abs/codex --grok /abs/grok --protect-root /abs/projects --evidence "$PROBE_EVIDENCE"`

// probeEvidenceSentences are FP-1d replacement 8: a new evidence directory
// for every invocation, set immediately after the command.
const probeEvidenceSentences = "Before each invocation, set `PROBE_EVIDENCE` to a new absolute, nonexistent evidence directory. There is no default evidence path; never reuse an earlier bundle."

// ownerGateSentence is the Light triage scratch gate (FP-1d replacement 3).
const ownerGateSentence = "The gate is an owner requalification: `probe-dev-full --scenario both` on the revision being qualified, after the [pre-probe smoke](#pre-probe-smoke), without configuration changes or fallback (see [Codex triage qualification](#codex-triage-qualification))."

// ownerRequalificationClauses are FP-5b's required gate clauses.
var ownerRequalificationClauses = []string{
	"Run owner requalification only after the pre-probe smoke passed, on the revision being qualified with the actual built binaries:",
	"Before each invocation, set `PROBE_EVIDENCE` to a new absolute, nonexistent evidence directory.",
	"There is no default evidence path; never reuse an earlier bundle.",
	"The operator prerequisites stay: Linux, the opt-in, no CI, a quiescent protected environment, supported authenticated Claude and Codex selections, the required eligible Grok executable, and Go and Git, at the current minimum versions; the harness performs no login and no vendor-home change.",
	"Record in the owner evidence the exact command and SHA, the versions, the new selection digest and registered roles, the actual Codex argv and effective effort, the reproduction's tool execution, the scratch paths and the literal cleanup, the valid triage marker, the admitted report-only tree, the light reviewer's execution evidence and every stage verdict.",
	"Claim qualification only when all six stages (`models`, `resume_wake`, `print_notification`, `reviewer_evidence`, `triage_evidence` and `light_reviewer_evidence`) pass and the probe's offline qualification check of its written record agrees.",
	"Inspect the logs for the reproduction and its cleanup, not just the final marker: a passing marker alone does not show that the scratch instruction was followed.",
	"On a failure, keep the evidence and return to investigation; no retry around a refusal, timeout increase or permission substitution.",
}

// checkOwnerRequalification validates FP-5b's generic gate in
// docs/maintainers/real-e2e.md (design 12a-probe11-closeout, FP-1d).
func checkOwnerRequalification(docs map[string]string) error {
	e2e := docs["e2e"]
	sec := mdSubsection(e2e, "#### Owner requalification")
	if sec == "" || !strings.Contains(mdSubsection(e2e, "### Codex triage qualification"), sec) {
		return fmt.Errorf("no Owner requalification section under Codex triage qualification")
	}
	if strings.Count(sec, "```sh\n") != 1 || !strings.Contains(sec, "```sh\n"+ownerRequalificationCommand+"\n```\n\n"+probeEvidenceSentences+" The operator prerequisites stay:") {
		return fmt.Errorf("the both-scope requalification command or its fresh-evidence rule is missing or changed")
	}
	if err := requirePhrases(sec, ownerRequalificationClauses); err != nil {
		return err
	}
	if strings.Contains(sec, "Use a fresh evidence path if that one exists") {
		return fmt.Errorf("the requalification keeps a fixed evidence path")
	}
	// The gate in the scratch section is the generic requalification.
	if err := requirePhrases(mdSubsection(e2e, "### Light triage scratch"), []string{ownerGateSentence, "Offline success does not qualify the live recipe."}); err != nil {
		return err
	}
	return checkNoNumberedGate(e2e)
}

// scratchStatusSentence replaces the pending requalification in Light
// triage scratch (design 12a-probe11-closeout, FP-1b).
const scratchStatusSentence = "Probe 11 qualified the Codex triage selection on 2026-10-26 at main `387759a` (qualification record `design/real-e2e-runs/20261026-probe11/qualification.json`)."

// observationParagraph replaces the recipe ASSUMPTION under Owner
// requalification (design 12a-probe11-closeout, FP-1b).
const observationParagraph = "**Observed on 2026-10-26:** this installed Codex recipe reproduced and cleaned up under the triage envelope. Probe 11 is `QUALIFIED` with `--scenario both` at main `387759a78caa62dc93723d42039dccb508776ca6`; all six stages passed in its qualification record `design/real-e2e-runs/20261026-probe11/qualification.json`. Its triage execution log `design/real-e2e-runs/20261026-probe11/probe/triage/01/logs.txt` records reproduction in `item_5`, the literal guarded cleanup completing with exit 0 in `item_9`, and scratch absence and unchanged original input bytes/modes in `item_10`; its final text `design/real-e2e-runs/20261026-probe11/probe/triage/01/final.txt` ends `STATUS: TRIAGE_CONFIRMED`. A pass proves only that this selection and these fixtures completed on that installation with the recorded policy; it does not prove that all prompts are approved, general confinement, future vendor behavior, recovery paths it did not execute, or a complete owner-authorized `run-dev-exercise`. Codex can differ from Grok in prompt interpretation and permission behavior. The combined exercise has separate evidence below. Offline tests cannot qualify a paid vendor."

// observedQualificationClauses are the observation paragraph's sentences,
// each deleted by a mutation (the limit and vendor-difference sentences
// are the earlier exact pins, kept).
var observedQualificationClauses = []string{
	"this installed Codex recipe reproduced and cleaned up under the triage envelope.",
	"Probe 11 is `QUALIFIED` with `--scenario both` at main `387759a78caa62dc93723d42039dccb508776ca6`; all six stages passed in its qualification record `design/real-e2e-runs/20261026-probe11/qualification.json`.",
	"Its triage execution log `design/real-e2e-runs/20261026-probe11/probe/triage/01/logs.txt` records reproduction in `item_5`, the literal guarded cleanup completing with exit 0 in `item_9`, and scratch absence and unchanged original input bytes/modes in `item_10`;",
	"its final text `design/real-e2e-runs/20261026-probe11/probe/triage/01/final.txt` ends `STATUS: TRIAGE_CONFIRMED`.",
	"A pass proves only that this selection and these fixtures completed on that installation with the recorded policy; it does not prove that all prompts are approved, general confinement, future vendor behavior, recovery paths it did not execute, or a complete owner-authorized `run-dev-exercise`.",
	"Codex can differ from Grok in prompt interpretation and permission behavior.",
	"The combined exercise has separate evidence below.",
	"Offline tests cannot qualify a paid vendor.",
}

// oldAssumptionParagraph is the pre-closeout recipe ASSUMPTION, never
// accepted again.
const oldAssumptionParagraph = "**ASSUMPTION (unverified):** this installed Codex recipe reproduces and cleans up under the triage envelope. Probe 10 settles that deployment prerequisite. A pass proves only that this selection and these fixtures completed on that installation with the recorded policy; it does not prove that all prompts are approved, general confinement, future vendor behavior, recovery paths it did not execute, or a complete owner-authorized `run-dev-exercise`. Codex can differ from Grok in prompt interpretation and permission behavior. The later combined real attempt remains separately required. Offline tests cannot qualify a paid vendor."

// staleObservation are pre-closeout pending and assumption claims, refused
// anywhere in the runbook even beside the correct text.
var staleObservation = []string{
	"Requalification is pending", "**ASSUMPTION (unverified):** this installed Codex recipe", "this installed Codex recipe reproduces and cleans up",
	"Probe 10 settles that deployment prerequisite", "The later combined real attempt remains separately required",
}

// checkObservedQualification validates FP-1b: probe 11's observed
// qualification in the scratch status and under Owner requalification.
func checkObservedQualification(docs map[string]string) error {
	e2e := docs["e2e"]
	if !strings.Contains(mdSubsection(e2e, "### Light triage scratch"), "\n\n"+scratchStatusSentence+" The scratch envelope binds new bundles only") {
		return fmt.Errorf("the scratch status does not record probe 11's qualification")
	}
	sec := mdSubsection(e2e, "#### Owner requalification")
	if sec == "" || !strings.Contains(mdSubsection(e2e, "### Codex triage qualification"), sec) {
		return fmt.Errorf("no Owner requalification section under Codex triage qualification")
	}
	// The exact paragraph carries every observedQualificationClauses entry.
	if paragraphWith(sec, "**Observed on 2026-10-26:**") != observationParagraph || strings.Count(e2e, "**Observed on") != 1 {
		return fmt.Errorf("the observed-qualification paragraph is missing or changed")
	}
	for _, s := range staleObservation {
		if strings.Contains(e2e, s) {
			return fmt.Errorf("the runbook still carries %q", s)
		}
	}
	return checkNoEvidenceLink(e2e)
}

func TestCodexTriageFP5b(t *testing.T) {
	docs := codexDocs(t)
	t.Run("requalification", func(t *testing.T) {
		mutateExpect(t, docs, checkOwnerRequalification, "e2e", append(deletions(append(slices.Clone(ownerRequalificationClauses), ownerGateSentence, "Offline success does not qualify the live recipe.")...),
			// A light-only or omitted scenario.
			[2]string{ownerRequalificationCommand, strings.Replace(ownerRequalificationCommand, "--scenario both", "--scenario light", 1)},
			[2]string{ownerRequalificationCommand, strings.Replace(ownerRequalificationCommand, "--scenario both ", "", 1)},
			[2]string{"The gate is an owner requalification: `probe-dev-full --scenario both`", "The gate is an owner requalification: `probe-dev-full --scenario light`"},
			// A fixed, dated or reused evidence path.
			[2]string{`--evidence "$PROBE_EVIDENCE"`, "--evidence /abs/callsheet/design/real-e2e-runs/20261027-probe12"},
			[2]string{`--evidence "$PROBE_EVIDENCE"`, "--evidence /abs/callsheet/design/real-e2e-runs/20261026-probe11"},
			[2]string{ownerRequalificationCommand, "PROBE_EVIDENCE=/abs/callsheet/design/real-e2e-runs/NEW-PROBE " + ownerRequalificationCommand},
			[2]string{probeEvidenceSentences, "Use a fresh evidence path if that one exists."},
			[2]string{probeEvidenceSentences, probeEvidenceSentences + " Use a fresh evidence path if that one exists."},
			[2]string{"\n```\n\n" + probeEvidenceSentences, "\n```\n\nUse an earlier bundle when convenient. " + probeEvidenceSentences},
			[2]string{"and every stage verdict.", "and every stage verdict. Use a fresh evidence path if that one exists."},
			// Weakened six-stage, execution or failure rules.
			[2]string{"all six stages", "the triage stage"},
			[2]string{"not just the final marker", "or just the final marker"},
			// Numbered gates, substituted or inserted beside the generic one.
			[2]string{"The gate is an owner requalification:", "The gate is owner probe 10:"},
			[2]string{"The gate is an owner requalification:", "The gate is owner probe 11:"},
			[2]string{"Run owner requalification only after", "Run owner probe 11 only after"},
			[2]string{"#### Owner requalification", "#### Owner probe 10"},
			[2]string{"#### Owner requalification", "#### Owner probe 10\n\n#### Owner requalification"},
			[2]string{"#### Owner requalification", "#### Probe"},
		))
	})
	t.Run("observed", func(t *testing.T) {
		const probe11Log = "`design/real-e2e-runs/20261026-probe11/probe/triage/01/logs.txt`"
		mutateExpect(t, docs, checkObservedQualification, "e2e", append(deletions(append(slices.Clone(observedQualificationClauses), scratchStatusSentence)...),
			// The old assumption, by prefix, by paragraph and appended.
			[2]string{"**Observed on 2026-10-26:** ", ""},
			[2]string{"**Observed on 2026-10-26:**", "**ASSUMPTION (unverified):**"},
			[2]string{observationParagraph, oldAssumptionParagraph},
			[2]string{"Offline tests cannot qualify a paid vendor.", "Offline tests cannot qualify a paid vendor.\n\n" + oldAssumptionParagraph},
			[2]string{"No paid run is part of implementation or review automation.", "No paid run is part of implementation or review automation. Probe 10 settles that deployment prerequisite."},
			[2]string{"No paid run is part of implementation or review automation.", "No paid run is part of implementation or review automation. The later combined real attempt remains separately required."},
			// The old pending status, substituted or appended.
			[2]string{scratchStatusSentence, "Requalification is pending."},
			[2]string{"No paid run is part of implementation or review automation.", "No paid run is part of implementation or review automation. Requalification is pending."},
			// A wrong qualification result, revision, scope or record.
			[2]string{"Probe 11 is `QUALIFIED`", "Probe 11 is `BLOCKED`"},
			[2]string{"at main `387759a78caa62dc93723d42039dccb508776ca6`", "at main `f2ee9d21760ca1c78c78b7bbddb663b73ebd8a4c`"},
			[2]string{"at main `387759a` (qualification record", "at main `f2ee9d2` (qualification record"},
			[2]string{"Probe 11 is `QUALIFIED` with `--scenario both`", "Probe 11 is `QUALIFIED` with `--scenario full`"},
			[2]string{"`design/real-e2e-runs/20261026-probe11/qualification.json`)", "`design/real-e2e-runs/20261025-probe10/qualification.json`)"},
			[2]string{"in `item_9`", "in `item_8`"},
			[2]string{"ends `STATUS: TRIAGE_CONFIRMED`", "ends `STATUS: TRIAGE_BLOCKED`"},
			// Weakened qualification limits.
			[2]string{"it does not prove that all prompts are approved, ", "it proves "},
			[2]string{", or a complete owner-authorized `run-dev-exercise`", ""},
			// Evidence citations converted to links, or links appended.
			[2]string{probe11Log, "[logs.txt](../design/real-e2e-runs/20261026-probe11/probe/triage/01/logs.txt)"},
			[2]string{"#### Owner requalification", "#### Owner record"},
			[2]string{"No paid run is part of implementation or review automation.", "No paid run is part of implementation or review automation. See [probe 11](../design/real-e2e-runs/20261026-probe11/qualification.json)."},
		))
	})
}

// The FP-6 smoke's pinned parts (design 12a-codex-triage, Pre-probe
// smoke, with the post-final DS1 clarification: the older-toolchain
// exception names the go.mod directive, never a version number).
const (
	smokeRepoAssignment = "(set -eu; smoke_repo=${CALLSHEET_REPO:?set CALLSHEET_REPO to the absolute repository root}; "
	smokeMktemp         = `smoke_dir=$(mktemp -d /tmp/callsheet-triage-smoke.XXXXXXXX); printf "SMOKE_DIR=%s\n" "$smoke_dir"; mkdir "$smoke_dir/work"; `
	smokeSeedCopy       = `cp "$smoke_repo/internal/reale2e/testdata/devlight-seed/FEATURE.md" "$smoke_repo/internal/reale2e/testdata/devlight-seed/main.go" "$smoke_repo/internal/reale2e/testdata/devlight-seed/main_test.go" "$smoke_dir/work/"; `
	smokeAwk            = `awk '$1 == "go" && NF == 2 { print "module example.com/greeting\n\ngo " $2; found=1; exit } END { if (!found) exit 1 }' "$smoke_repo/go.mod" > "$smoke_dir/work/go.mod"; `
	smokeCwd            = `cd "$smoke_dir/work"; `
	smokeGoEnv          = `GOCACHE="$PWD/.callsheet-triage-scratch/gocache" GOMODCACHE="$PWD/.callsheet-triage-scratch/gomodcache" TMPDIR="$PWD/.callsheet-triage-scratch/tmp" GOTMPDIR="$PWD/.callsheet-triage-scratch/tmp" GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -count=1 ./...`
	smokeCodex          = `"$HOME/.local/bin/codex" `
	smokeRedirects      = ` > "$smoke_dir/codex.jsonl" 2> "$smoke_dir/codex.stderr"; `
	smokeTail           = `test "$(cat "$smoke_dir/callsheet-final.txt")" = SMOKE_OK; test ! -e .callsheet-triage-scratch; test ! -L .callsheet-triage-scratch; printf "SMOKE_PASS\n")`
	smokeFinal          = `$smoke_dir/callsheet-final.txt`
	// smokeRepoRequired is the smoke's required-input sentence (design
	// 12a-3-cursor-closeout, FP-8): no private repository default.
	smokeRepoRequired = "Set `CALLSHEET_REPO` to the absolute repository root before running the smoke; it is required and has no default."
)

// smokeClauses are the smoke's required prose.
var smokeClauses = []string{
	"Before owner requalification, run this owner-run paid smoke once. It is non-qualifying, and neither CI, the implementation tests nor review automation runs it.",
	"Run it from any directory on the owner installation, with Codex installed at `$HOME/.local/bin/codex`.",
	smokeRepoRequired,
	"That repository-root variable is the only operator-adjustable part of the command: keep the recipe and every other field unchanged.",
	"creates a fresh private `/tmp/callsheet-triage-smoke.XXXXXXXX` directory outside the repository and every evidence bundle, and runs Codex with the copied seed as its working directory.",
	"Do not redirect it into a bundle.",
	"then the repository's Go directive, selected by `$1 == \"go\" && NF == 2` as the supervisor reads it",
	"No repository or evidence-bundle write is permitted, and no operator policy is edited",
	"`--approve-for-me` is the only sandbox or approval selection.",
	"The smoke's instructions are operator text, never part of the triage manuals or task envelopes.",
	"Expected output: shell exit 0 and exactly `SMOKE_PASS` as the last stdout line, after `SMOKE_DIR=/tmp/callsheet-triage-smoke.<unique suffix>`.",
	"`callsheet-final.txt` holds only `SMOKE_OK` (with an optional trailing newline), and `work/.callsheet-triage-scratch` is absent and not a symlink.",
	"Inspect `codex.jsonl` for completed command execution creating the scratch, the scratch-local cache and temporary environment, a successful `go test -count=1 ./...` (package `example.com/greeting`, exit 0) and the literal cleanup chain completing with exit 0.",
	"A marker or wrapper PASS without that execution evidence is a failed smoke, not a successful check.",
	"Stop rule: any startup failure, refusal, nonzero command, wrong or missing final text, missing execution evidence or residual scratch fails the smoke.",
	"Stop before owner requalification and return to triage, without flag or configuration changes, alternative commands or retry workarounds.",
	"a failure of the form `go: ... requires go >= ...` under `GOTOOLCHAIN=local` means the installed Go is older than the repository's go.mod `go` directive",
	"That is an environment prerequisite failure to fix on your machine, not a Codex finding, and not a return to triage.",
	"Even a successful smoke is non-qualifying: no qualification record may cite it as qualification or as a substitute for any probe stage.",
	"It only reduces uncertainty about these commands on this installation; owner requalification still checks the complete triage envelope for the installation and revision being qualified.",
}

// shellWords splits a simple shell word list: single and double quotes
// group and are removed; there are no escapes.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	quote, in := byte(0), false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// smokeLine is the section's one-line fenced bash command.
func smokeLine(sec string) (string, error) {
	_, rest, ok := strings.Cut(sec, "```bash\n")
	if !ok {
		return "", fmt.Errorf("no bash block")
	}
	body, _, ok := strings.Cut(rest, "\n```\n")
	if !ok || strings.Contains(body, "\n") || strings.Count(sec, "```bash\n") != 1 {
		return "", fmt.Errorf("the smoke is not one single-line bash block")
	}
	return body, nil
}

// checkSmoke validates FP-6: the documented one-line smoke against the
// adapter's Codex recipe and the shipped triage selection, its literal
// cleanup, external mktemp, CWD and final paths, scratch-local Go
// environment, output and the stop and non-qualification rules.
func checkSmoke(docs map[string]string) error {
	e2e := docs["e2e"]
	sec := mdSubsection(e2e, "#### Pre-probe smoke")
	if sec == "" || !strings.Contains(mdSubsection(e2e, "### Codex triage qualification"), sec) || !strings.Contains(e2e, "](#pre-probe-smoke)") {
		return fmt.Errorf("no Pre-probe smoke section under Codex triage qualification")
	}
	if strings.Index(e2e, "#### Pre-probe smoke") > strings.Index(e2e, "#### Owner requalification") {
		return fmt.Errorf("the smoke is not documented before owner requalification")
	}
	if err := checkNoNumberedGate(e2e); err != nil {
		return err
	}
	if err := requirePhrases(sec, smokeClauses); err != nil {
		return err
	}
	line, err := smokeLine(sec)
	if err != nil {
		return err
	}
	// The fixed prefix: the required repository variable (no default), the
	// external mktemp, the literal seed copy, the two-field go directive
	// and the copied seed as CWD.
	prefix := smokeRepoAssignment + smokeMktemp + smokeSeedCopy + smokeAwk + smokeCwd
	if !strings.HasPrefix(line, prefix) {
		return fmt.Errorf("the smoke's setup prefix changed")
	}
	// One assignment and one expansion; the variable's two occurrences are
	// the expansion and its error message.
	if strings.Count(line, "smoke_repo=") != 1 || strings.Count(line, "${CALLSHEET_REPO:") != 1 || strings.Count(line, "CALLSHEET_REPO") != 2 {
		return fmt.Errorf("the repository is hardcoded beside its variable")
	}
	// The prompt, the Codex argv and the checks after it.
	rest := strings.TrimPrefix(line, prefix)
	pr, ok := strings.CutPrefix(rest, `printf "%s\n" '`)
	if !ok {
		return fmt.Errorf("the prompt is not one printf argument")
	}
	prompt, after, ok := strings.Cut(pr, "' | "+smokeCodex)
	if !ok || strings.Contains(prompt, "'") {
		return fmt.Errorf("the prompt does not pipe into the installed Codex")
	}
	argText, tail, ok := strings.Cut(after, smokeRedirects)
	if !ok || tail != smokeTail {
		return fmt.Errorf("the output redirects or the final checks changed")
	}
	// The argv is exactly the adapter's Codex recipe for the shipped
	// triage selection, the final file in the smoke directory.
	var flow struct {
		Roles []struct{ ID, Adapter, Model, Effort string } `json:"roles"`
	}
	if err := json.Unmarshal([]byte(docs["flow"]), &flow); err != nil {
		return err
	}
	var model, effort string
	for _, r := range flow.Roles {
		if r.ID == "triage" && r.Adapter == adapter.CodexID {
			model, effort = r.Model, r.Effort
		}
	}
	inv, err := adapter.NewCodex(".").Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("a", 32), Model: model, Effort: effort, Prompt: []byte(prompt),
		ScratchDir: "/scratch", FinalDir: "/final"})
	if err != nil {
		return fmt.Errorf("the shipped triage selection has no Codex recipe: %v", err)
	}
	want := slices.Clone(inv.Argv)
	for i, a := range want {
		if a == inv.FinalFile && filepath.Base(a) == adapter.CodexFinalName {
			want[i] = smokeFinal
		}
	}
	if got := shellWords(argText); !slices.Equal(got, want) || want[len(want)-1] != "-" {
		return fmt.Errorf("the smoke argv %q is not the adapter recipe %q", got, want)
	}
	for _, bad := range []string{"--sandbox", " -a ", "--yolo", "--dangerously", "--full-auto", "config.toml", "AGENTS.md", "--profile"} {
		if strings.Contains(line, bad) {
			return fmt.Errorf("the smoke adds %q", bad)
		}
	}
	// Writes go only to the smoke directory.
	for _, m := range regexp.MustCompile(`>\s*"([^"]*)"`).FindAllStringSubmatch(line, -1) {
		if !strings.HasPrefix(m[1], "$smoke_dir/") {
			return fmt.Errorf("the smoke writes %s", m[1])
		}
	}
	if strings.Contains(line, "real-e2e-runs") || strings.Contains(line, "$smoke_repo/design") {
		return fmt.Errorf("the smoke names an evidence bundle")
	}
	// The prompt's scratch-local Go environment and the literal cleanup.
	if err := requirePhrases(prompt, []string{
		"Refuse any pre-existing .callsheet-triage-scratch path, including a symlink.",
		"Create .callsheet-triage-scratch and its gocache, gomodcache and tmp children.",
		"From the initial checkout root run this command between BEGIN and END: BEGIN " + smokeGoEnv + " END.",
		"If it succeeds, join all children, return to the initial checkout root, then execute this command between BEGIN and END: BEGIN " + triageScratchRecipe + " END.",
		"Confirm the directory is absent.",
		"Only after every step succeeds, final answer exactly SMOKE_OK.",
		"Otherwise stop and report failure without a workaround.",
		"do not use another permission recipe or retry a refused command",
	}); err != nil {
		return fmt.Errorf("the smoke prompt: %w", err)
	}
	for _, env := range regexp.MustCompile(`\b(GOCACHE|GOMODCACHE|TMPDIR|GOTMPDIR)=("[^"]*"|\S+)`).FindAllStringSubmatch(line, -1) {
		if !strings.HasPrefix(env[2], `"$PWD/.callsheet-triage-scratch/`) {
			return fmt.Errorf("%s is outside the scratch: %s", env[1], env[2])
		}
	}
	if strings.Count(line, "chmod -R u+w .callsheet-triage-scratch") != 1 || strings.Count(line, "rm -r .callsheet-triage-scratch") != 1 || strings.Contains(line, "rm -rf") {
		return fmt.Errorf("the cleanup chain is respelled")
	}
	return nil
}

func TestCodexTriageFP6(t *testing.T) {
	docs := codexDocs(t)
	if err := checkSmoke(docs); err != nil {
		t.Fatal(err)
	}
	line, err := smokeLine(mdSubsection(docs["e2e"], "#### Pre-probe smoke"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("shell_syntax", func(t *testing.T) {
		// Syntax only (bash -n never executes the command).
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skipf("no bash: %v", err)
		}
		if out, err := exec.Command(bash, "-n", "-c", line).CombinedOutput(); err != nil {
			t.Fatalf("bash -n: %v %s", err, out)
		}
	})
	t.Run("mutations", func(t *testing.T) {
		const required = "smoke_repo=${CALLSHEET_REPO:?set CALLSHEET_REPO to the absolute repository root};"
		mutateExpect(t, docs, checkSmoke, "e2e", cursorChanged(t, append(deletions(smokeClauses...),
			// Extra or substituted approval flags.
			[2]string{"exec --approve-for-me --model", "exec --approve-for-me --sandbox workspace-write --model"},
			[2]string{"exec --approve-for-me --model", "exec --yolo --model"},
			[2]string{"exec --approve-for-me --model", "exec -a never --model"},
			// Wrong selection.
			[2]string{`-c 'model_reasoning_effort="high"'`, `-c 'model_reasoning_effort="xhigh"'`},
			[2]string{"--model gpt-6.1-sol -c", "--model gpt-6-astra -c"},
			// Repository or bundle output.
			[2]string{`> "$smoke_dir/codex.jsonl"`, `> "$smoke_repo/design/real-e2e-runs/codex.jsonl"`},
			[2]string{`--output-last-message "$smoke_dir/callsheet-final.txt"`, `--output-last-message "$smoke_repo/callsheet-final.txt"`},
			[2]string{"mktemp -d /tmp/callsheet-triage-smoke.XXXXXXXX", `mktemp -d "$smoke_repo/design/real-e2e-runs/smoke.XXXXXXXX"`},
			[2]string{`cd "$smoke_dir/work"; `, ""},
			// External cache.
			[2]string{`GOCACHE="$PWD/.callsheet-triage-scratch/gocache"`, `GOCACHE="/tmp/gocache"`},
			[2]string{`TMPDIR="$PWD/.callsheet-triage-scratch/tmp" GOTMPDIR`, `TMPDIR=/tmp GOTMPDIR`},
			// Changed removal chain.
			[2]string{"BEGIN test -d .callsheet-triage-scratch && test ! -L .callsheet-triage-scratch && chmod", "BEGIN test -d .callsheet-triage-scratch && chmod"},
			[2]string{"rm -r .callsheet-triage-scratch && test ! -e .callsheet-triage-scratch END", "rm -rf .callsheet-triage-scratch && test ! -e .callsheet-triage-scratch END"},
			// Hardcoded or wrong repository variable or default, and the
			// old private default restored in the command or the prose
			// (design 12a-3-cursor-closeout, FP-8).
			[2]string{required, "smoke_repo=/opt/gitspace/callsheet;"},
			[2]string{required, "smoke_repo=${REPO:-/opt/gitspace/callsheet};"},
			[2]string{required, "smoke_repo=${CALLSHEET_REPO:-/opt/callsheet};"},
			[2]string{`cp "$smoke_repo/internal/reale2e/testdata/devlight-seed/FEATURE.md"`, `cp "/opt/gitspace/callsheet/internal/reale2e/testdata/devlight-seed/FEATURE.md"`},
			[2]string{required, "smoke_repo=${CALLSHEET_REPO:-/opt/gitspace/callsheet};"},
			[2]string{smokeRepoRequired, "Set `CALLSHEET_REPO` to the absolute repository root if needed; it defaults to `/opt/gitspace/callsheet`."},
			// Missing two-field awk guard.
			[2]string{`awk '$1 == "go" && NF == 2 {`, `awk '$1 == "go" {`},
			// Missing final checks.
			[2]string{`test ! -L .callsheet-triage-scratch; printf "SMOKE_PASS\n")`, `printf "SMOKE_PASS\n")`},
			[2]string{"final answer exactly SMOKE_OK", "final answer SMOKE_OK when done"},
			// Weakened toolchain exception, stop rule or non-qualification.
			[2]string{"not a Codex finding, and not a return to triage", "a Codex finding to triage"},
			[2]string{"Stop before owner requalification and return to triage", "Continue to owner requalification"},
			// Numbered gates (design 12a-probe11-closeout, FP-1d), substituted
			// or inserted beside the generic wording, and the smoke's order.
			[2]string{"Before owner requalification, run", "Before probe 10, run"},
			[2]string{"Stop before owner requalification and return to triage", "Stop before probe 10 and return to triage. Stop before owner requalification and return to triage"},
			[2]string{"owner requalification still checks the complete triage envelope for the installation and revision being qualified.", "probe 10 still settles the complete triage-envelope assumption."},
			[2]string{"owner requalification still checks the complete triage envelope for the installation and revision being qualified.", "owner requalification still checks the complete triage envelope for the installation and revision being qualified; probe 11 still settles it."},
			[2]string{"#### Pre-probe smoke\n", "#### Owner requalification\n\n#### Pre-probe smoke\n"},
			[2]string{"Even a successful smoke is non-qualifying", "A successful smoke qualifies"},
		)))
	})
	t.Run("version_not_pinned", func(t *testing.T) {
		// The older-toolchain exception pins its wording, never the go.mod
		// directive's current number (post-final DS1).
		e2e := docs["e2e"]
		if !strings.Contains(e2e, "`go` directive (currently ") {
			t.Fatal("the exception does not name the go.mod directive")
		}
		bumped := regexp.MustCompile("`go` directive \\(currently [0-9.]+\\)").ReplaceAllString(e2e, "`go` directive (currently 9.99.0)")
		changed := map[string]string{"e2e": bumped, "flow": docs["flow"]}
		if bumped == e2e {
			t.Fatal("no version to bump")
		}
		if err := checkSmoke(changed); err != nil {
			t.Fatalf("the validator pins the version number: %v", err)
		}
	})
}

// The neutral operator paths of design 12a-3-cursor-closeout, FP-8 (review
// W1): the Run example's placeholder roots and their explanation.
const (
	runProtectRootsLine = "  --protect-root /abs/projects --protect-root /abs/other-projects \\"
	runProtectRootsNote = "The two `--protect-root` values are placeholders; substitute your own roots or omit both flags for vendor-home-only protection."
)

// checkCursorCloseoutPaths validates FP-8 in docs/maintainers/real-e2e.md: the smoke's
// required repository assignment and required-input prose, the Run
// example's placeholder roots line and explanation once, and no
// /opt/gitspace or /opt/gospace anywhere (a bounded W1 regression guard,
// not a general private-path classifier). It calls neither checkSmoke nor
// the closeout aggregate.
func checkCursorCloseoutPaths(doc string) error {
	sec := mdSubsection(doc, "#### Pre-probe smoke")
	line, err := smokeLine(sec)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, smokeRepoAssignment) {
		return fmt.Errorf("the smoke does not require CALLSHEET_REPO without a default")
	}
	if strings.Count(flatText(sec), flatText(smokeRepoRequired)) != 1 {
		return fmt.Errorf("the smoke lacks its required-input sentence")
	}
	run := mdSubsection(doc, "## Run")
	roots := 0
	for _, l := range strings.Split(run, "\n") {
		if l == runProtectRootsLine {
			roots++
		}
	}
	if roots != 1 {
		return fmt.Errorf("the Run example's placeholder roots line is not there once")
	}
	if strings.Count(flatText(run), flatText(runProtectRootsNote)) != 1 {
		return fmt.Errorf("the Run example's placeholder explanation is not there once")
	}
	for _, private := range []string{"/opt/gitspace", "/opt/gospace"} {
		if strings.Contains(doc, private) {
			return fmt.Errorf("the runbook names the private path %s", private)
		}
	}
	return nil
}

func TestCursorCloseoutFP8(t *testing.T) {
	docs := codexDocs(t)
	e2e := docs["e2e"]
	// Positive control through both entry points: the generic /tmp
	// template and the $HOME-relative Codex stay.
	line, err := smokeLine(mdSubsection(e2e, "#### Pre-probe smoke"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "mktemp -d /tmp/callsheet-triage-smoke.XXXXXXXX") || !strings.Contains(line, `"$HOME/.local/bin/codex"`) {
		t.Fatal("the smoke lost its /tmp template or $HOME-relative Codex")
	}
	combined := func(docs map[string]string) error {
		if err := checkSmoke(docs); err != nil {
			return err
		}
		return checkCursorCloseoutPaths(docs["e2e"])
	}
	for name, check := range map[string]func(map[string]string) error{"helper": onCursorRunbook(checkCursorCloseoutPaths), "combined": combined} {
		if err := check(docs); err != nil {
			t.Fatalf("%s: the shipped runbook fails: %v", name, err)
		}
	}
	const (
		required     = "smoke_repo=${CALLSHEET_REPO:?set CALLSHEET_REPO to the absolute repository root};"
		oldRoots     = "  --protect-root /opt/gitspace --protect-root /opt/gospace \\"
		oldNote      = "The two `--protect-root` values are an example (the owner's); substitute your own roots or omit both flags for vendor-home-only protection."
		oldProse     = "Set `CALLSHEET_REPO` to the absolute repository root if needed; it defaults to `/opt/gitspace/callsheet`."
		closingFence = "printf \"SMOKE_PASS\\n\")\n```\n"
	)
	if n := strings.Count(e2e, closingFence); n != 1 {
		t.Fatalf("the smoke's anchored closing fence occurs %d times, want 1", n)
	}
	muts := [][2]string{
		// smokeLine errors: a sh fence, a two-line command, no closing fence.
		{"```bash\n(set -eu; smoke_repo=", "```sh\n(set -eu; smoke_repo="},
		{"; smoke_dir=$(mktemp -d", ";\nsmoke_dir=$(mktemp -d"},
		{"printf \"SMOKE_PASS\\n\")\n```\n", "printf \"SMOKE_PASS\\n\")\n"},
		// The required assignment prefix.
		{required, "smoke_repo=/abs/projects;"},
		{"${CALLSHEET_REPO:?set", "${OTHER_REPO:?set"},
		{"${CALLSHEET_REPO:?set", "${CALLSHEET_REPO:-set"},
		{required + " ", ""},
		// The required-input prose.
		{smokeRepoRequired, ""},
		{smokeRepoRequired, "Set `CALLSHEET_REPO` if needed."},
		// The Run roots line, once.
		{runProtectRootsLine, "  --protect-root /abs/other-projects \\"},
		{runProtectRootsLine, "  --protect-root /abs/projects \\"},
		{runProtectRootsLine, "  --protect-root /abs/different-projects --protect-root /abs/other-projects \\"},
		{runProtectRootsLine + "\n", runProtectRootsLine + "\n" + runProtectRootsLine + "\n"},
		// The Run explanation, once.
		{runProtectRootsNote, ""},
		{runProtectRootsNote, oldNote},
		{runProtectRootsNote + "\n", runProtectRootsNote + "\n\n" + runProtectRootsNote + "\n"},
		// Each private prefix alone, outside the smoke and Run sections.
		{"\n## Sign-off\n", "\nSee /opt/gitspace for an example.\n\n## Sign-off\n"},
		{"\n## Sign-off\n", "\nSee /opt/gospace for an example.\n\n## Sign-off\n"},
		// Overlapping restorations of the old private defaults.
		{runProtectRootsLine, "  --protect-root /opt/gitspace --protect-root /abs/other-projects \\"},
		{runProtectRootsLine, "  --protect-root /abs/projects --protect-root /opt/gospace \\"},
		{runProtectRootsLine, oldRoots},
		{required, "smoke_repo=${CALLSHEET_REPO:-/opt/gitspace/callsheet};"},
		{smokeRepoRequired, oldProse},
		{smokeRepoRequired, smokeRepoRequired + " " + oldProse},
		{runProtectRootsLine + "\n", runProtectRootsLine + "\n" + oldRoots + "\n"},
	}
	t.Run("helper", func(t *testing.T) {
		mutateExpect(t, codexDocs(t), onCursorRunbook(checkCursorCloseoutPaths), "e2e", cursorChanged(t, muts))
	})
	t.Run("combined", func(t *testing.T) {
		mutateExpect(t, codexDocs(t), combined, "e2e", cursorChanged(t, muts))
	})
}
