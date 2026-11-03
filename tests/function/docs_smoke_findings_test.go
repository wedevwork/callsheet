// Documentation contracts for the dev-flow smoke findings and the example's
// own role selection. The checks read shipped documentation and in-memory
// mutations of those bytes; they run no vendor CLI and read no design/ file.
package function

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/reale2e"
)

// Item 1: hosting Callsheet from a sandboxed Claude Code Bash command.
const (
	sandboxHostingNamespace = "as observed with sandboxed Claude Code Bash on Linux: each command has its own network namespace, so a plane or sidecar started from one command is unreachable from the next, and a background command stops with its session."
	sandboxHostingRoute     = "Run the plane and sidecars in ordinary terminals, as the [quickstart](quickstart.md#one-machine) does, or as services ([deployment](deploy.md#run-as-services))."
	sandboxHostingScope     = "This observation covers only sandboxed Claude Code Bash, not every sandbox; keep your sandbox as configured."
)

// Item 2: a transient readiness answer, with no timing figure.
const (
	readinessTransient       = "Readiness is reported after the sidecar's probe, so `false` right after `role add` can be transient"
	readinessQuickstartNext  = ": run `role ls` again first. If it stays false, follow [troubleshooting](guide/troubleshooting.md#connection-and-readiness) and resolve it before dispatching."
	readinessRolesNext       = ": run `role ls` again first, and go to [troubleshooting](troubleshooting.md#connection-and-readiness) if it stays false."
	readinessNeverRetryBlind = "Never retry `role add` blindly on a conflict"
)

// mcpToolsObservation is item 3's dated observation.
const mcpToolsObservation = "Observed on 2026-11-02, with Claude Code 2.1.316, the version installed on the observing host when this was written: `claude mcp get` showed the server Connected, but the running session did not see its tools until it was restarted or resumed."

// Item 4: enabling Codex before the example needs it.
const (
	enableCodexStep    = "2. Enable Codex, which the quickstart's Claude-only sidecar leaves disabled; it serves designer and code-reviewer here. Follow [Enable Codex](#enable-codex)."
	enableCodexReason  = "Codex serves designer and code-reviewer in this selection, so choose one route ([enabling a worker](../../real-adapters.md#enabling-a-worker))."
	enableCodexRestart = "**Restart the sidecar** with `--codex-adapter` added, on the same state directory, once it is idle. The node keeps its ID and its registered roles, so `CODEX_NODE` and `CLAUDE_NODE` are the same node."
	enableCodexSecond  = "**Or enroll a second sidecar** for Codex and leave the first running. It is a new node with its own ID: use that ID as `CODEX_NODE`."
)

// enableCodexFences pin the two routes' exact argv.
var enableCodexFences = map[string][][]string{
	"enable-codex-restart": {{"callsheet", "sidecar", "run", "--state-dir", "$SIDECAR_STATE", "--claude-adapter", "$CLAUDE_BIN", "--codex-adapter", "$CODEX_BIN"}},
	"enable-codex-second-sidecar": {
		{"callsheet", "sidecar", "enroll", "--state-dir", "$CODEX_SIDECAR_STATE", "--plane", "$PLANE", "--ca", "$CA"},
		{"callsheet", "sidecar", "run", "--state-dir", "$CODEX_SIDECAR_STATE", "--codex-adapter", "$CODEX_BIN"},
	},
}

// Item 5: the triage dispatch example.
const (
	triageEnvelopeProse = "`scratch_policy` names `.callsheet-triage-scratch/` with its literal removal recipe, the only place triage may reproduce. With no scratch path in the envelope, a reproduction that needs scratch is `TRIAGE_BLOCKED` ([triage runbook](triage-runbook.md#reproductions))."
)

// triageEnvelopeFields are the triage goal's playbook and runbook fields.
var triageEnvelopeFields = []string{
	"attempt ID:", "flow and mode: light / triage", "request/brief path:", "design path:", "FP subset:", "input task/commit/digests:", "initial source base:",
	"initial_source_commit:", "allowed output paths: rca.md; fix.md only at CONFIRMED or PROBABLE", "preserved_inputs: unchanged; add no other path",
	"scratch_policy: " + triageScratchRecipe, "verification commands:", "timeout/repetition policy:", "evidence location: none; see triage-runbook.md",
	"owner approval reference:", "round/finding IDs:",
}

// Review round 1, C1: the runbook's four-file reproduction export, stated where
// the example offers triage. The manual's method is unchanged.
const (
	triageRunbookExport     = "Export the four seed files (`FEATURE.md`, `go.mod`, `main.go`, `main_test.go`) of the explicitly named `initial_source_commit` into `copy/`"
	triageRestrictionExport = "### Seed-project restriction\n\nThe triage runbook's reproduction exports exactly four root files of `initial_source_commit` into `copy/`: `FEATURE.md`, `go.mod`, `main.go` and `main_test.go`."
	triageRestrictionFit    = "It fits only a project those four files fully reproduce, although any ordinary folder is a valid workspace source."
	triageRestrictionOther  = "On any other layout the export is missing or incomplete, so triage reports `TRIAGE_BLOCKED` when it needs a reproduction; an investigation that needs no reproduction can still finish from direct evidence."
	triageRestrictionScope  = "The restriction is on triage's reproduction only: the coder and the code reviewer work from the whole commit."
	lightWalkthroughScope   = "Step 1 is scoped by the [seed-project restriction](#seed-project-restriction): triage can reproduce only in a project that `FEATURE.md`, `go.mod`, `main.go` and `main_test.go` fully reproduce."
	sourceFolderScope       = "The light walkthrough's triage reproduction has a [seed-project restriction](#seed-project-restriction)."
	chapterLightScope       = "Its triage can reproduce only in a project that `FEATURE.md`, `go.mod`, `main.go` and `main_test.go` fully reproduce; see the [seed-project restriction](../examples/dev-flow/README.md#seed-project-restriction)."
)

// Review round 1, W1: the ASSUMPTION's probe sentence stays verbatim, and the
// shipped harness is stated not to be that probe for this selection.
const (
	assumptionProbeSentence = "Qualification probe: the later live harness must probe every distinct pair through its adapter with authentication and effective-selection evidence before dispatching workflow tasks."
	harnessNotQualifying    = "The shipped harness is not that probe for this selection: it keeps its earlier selection and does not qualify this one, and `internal/reale2e/devfull_flow.go` refuses Claude as triage."
	ownerQualificationNeed  = "Separate owner qualification of these pairs remains necessary."
)

// grokObservation is item 6's dated line.
const grokObservation = "Observed on 2026-11-02: grok 1.0.67 (2fe064390d07) with `grok-4.7-build-fast` at `low` created one file in one simple task under `auto`; compound shell commands were not tested."

// reviewerTaskIDClause is item 7's clause.
const reviewerTaskIDClause = "Use the Callsheet task ID, the `task_id` Callsheet supplies in the prompt, not one the coordinator provides and not a free-form goal string."

// Item 9: denied or skipped verification.
const (
	deniedVerificationRule = "Count a verification that a worker reports as denied or skipped as not done, and do not accept it as evidence."
	workerRefusalSentence  = "An authentication or permission refusal that ends a task stops the operation for owner diagnosis, without fallback, settings edits or broad permission bypasses."
	workerDeniedCommand    = "A denial of one command inside a running task does not end the task: Callsheet does not observe individual commands, and a worker can continue after one, so read its final message and artifacts, not the task state alone."
)

// sidecarUserConfig is item 10's sentence.
const sidecarUserConfig = "The vendor CLI runs as the sidecar's OS user, with the sidecar's environment and home directory, so that user's vendor configuration applies to workers, including any sandbox setting; see [accounts and paths](deploy.md#accounts-and-paths)."

// deliveredDesignDocs is item 11's sentence.
const deliveredDesignDocs = "The delivered tree contains the flow's documents under `design/`, so decide what to keep before copying it into a project."

// The example's own evidence statement and pairing rule.
const (
	exampleEvidenceEarlier   = "establish the recorded 12a method run on an earlier selection (Codex triage, low designer and design-reviewer effort)"
	exampleEvidenceProof     = "Those records prove that earlier selection's method and observed results using a verification harness"
	exampleEvidenceNone      = "The role selection below has no recorded live evidence, and an accepted effort value is not evidence that it ran."
	exampleHarnessKeepsOwn   = "The harness keeps its own qualified selection in [flow.json](../real-e2e/dev-full/flow.json)."
	exampleChapterEarlier    = "The 12a exercises prove an earlier selection's method and observed results using a verification harness"
	exampleChapterNone       = "The example's current selection has no recorded live evidence."
	playbookHarnessSelection = "A verification harness that loads this playbook keeps its own frozen selection in its flow file; in that run, the flow file is the expected selection."
)

// exampleAssumption stays as shipped and covers the new pairs.
const exampleAssumption = "**ASSUMPTION (unverified):** all five requested pairs execute successfully on the owner's installation."

// staleExampleClaims are claims of the earlier selection, or of its evidence,
// that neither the example README nor its playbook may make.
var staleExampleClaims = []string{
	"triage (GPT)", "Triage and the code reviewer may both be GPT", "The triage role (codex)", "the code reviewer and triage stay on Codex",
	"the GPT code reviewer and triage judge", "Those records prove that selection's", "CODEX_NODE` | The node ID for designer, code-reviewer and triage",
}

// sandboxDocs are the reader-facing pages under the no-disable rule.
var sandboxDocs = []string{"coordinator", "quickstart", "roles", "readme", "playbook", "worker", "example"}

var (
	sandboxOffRE   = regexp.MustCompile(`(?i)\b(disable|disabling|turn off|turning off|switch off)\b[^.;]*\bsandbox`)
	sandboxOffNo   = regexp.MustCompile(`(?i)\b(do not|don't|never|no)\b`)
	sandboxEnabled = `"enabled":false`
	timingFigureRE = regexp.MustCompile(`\b\d+\s*(ms|s|sec|secs|seconds?|minutes?)\b`)
	taskIDRE       = regexp.MustCompile(`\bt_[0-9a-f]{32}\b`)
)

// smokeFindingDocs are the shipped documents these validators read.
func smokeFindingDocs(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"coordinator": string(repoFile(t, "docs/coordinator.md")),
		"quickstart":  string(repoFile(t, "docs/quickstart.md")),
		"roles":       string(repoFile(t, "docs/guide/roles-and-manuals.md")),
		"readme":      string(repoFile(t, "docs/examples/dev-flow/README.md")),
		"playbook":    string(repoFile(t, "docs/examples/dev-flow/coordinator-playbook.md")),
		"worker":      string(repoFile(t, "docs/guide/worker-setup.md")),
		"example":     string(repoFile(t, "docs/guide/example.md")),
		"reviewer":    string(repoFile(t, "docs/examples/dev-flow/code-reviewer-runbook.md")),
		"triage":      string(repoFile(t, "docs/examples/dev-flow/triage-runbook.md")),
		"flow":        string(repoFile(t, "docs/examples/real-e2e/dev-full/flow.json")),
		"cursorflow":  string(repoFile(t, "docs/examples/real-e2e/dev-full/flow-cursor-coder.json")),
		"e2e":         string(repoFile(t, "docs/maintainers/real-e2e.md")),
	}
}

// checkSandboxHosting validates item 1, inside one paragraph of the
// coordinator guide.
func checkSandboxHosting(docs map[string]string) error {
	p := paragraphWith(docs["coordinator"], sandboxHostingNamespace)
	if p == "" {
		return fmt.Errorf("coordinator: no paragraph states the sandboxed hosting observation")
	}
	return requirePhrases(p, []string{sandboxHostingRoute, sandboxHostingScope})
}

// checkNoSandboxDisable is the rule that nothing tells a reader to disable a
// sandbox: a sentence that mentions it must refuse it, and the one
// session-only setting stays the coordinator guide's two exercise notes.
func checkNoSandboxDisable(docs map[string]string) error {
	for _, name := range sandboxDocs {
		for _, s := range regexp.MustCompile(`[.;:]\s+|\n+`).Split(docs[name], -1) {
			if sandboxOffRE.MatchString(s) && !sandboxOffNo.MatchString(s) {
				return fmt.Errorf("%s tells readers to disable a sandbox: %q", name, s)
			}
		}
		want := 0
		if name == "coordinator" {
			want = 2
		}
		if n := strings.Count(docs[name], sandboxEnabled); n != want {
			return fmt.Errorf("%s carries the sandbox-off setting %d times, want %d", name, n, want)
		}
	}
	if !strings.Contains(docs["coordinator"], "do not turn on broad approval or disable sandboxing") || !strings.Contains(docs["quickstart"], "do not disable sandboxing") {
		return fmt.Errorf("the explicit do-not-disable sentences are gone")
	}
	return nil
}

// checkTransientReadiness validates item 2 on both pages.
func checkTransientReadiness(docs map[string]string) error {
	for name, next := range map[string]string{"quickstart": readinessQuickstartNext, "roles": readinessRolesNext} {
		want := readinessTransient + next
		if !strings.Contains(docs[name], want) {
			return fmt.Errorf("%s lacks %q", name, want)
		}
		if m := timingFigureRE.FindString(paragraphWith(docs[name], readinessTransient)); m != "" {
			return fmt.Errorf("%s gives the readiness note a timing figure %q", name, m)
		}
	}
	if !strings.Contains(docs["quickstart"], readinessNeverRetryBlind) {
		return fmt.Errorf("quickstart lost %q", readinessNeverRetryBlind)
	}
	return nil
}

// checkMCPToolsObservation validates item 3 in the Claude Code section.
func checkMCPToolsObservation(docs map[string]string) error {
	sec := mdSection(docs["coordinator"], "## Claude Code")
	if !strings.Contains(sec, mcpToolsObservation) {
		return fmt.Errorf("the Claude Code section lacks the dated tools observation")
	}
	return nil
}

// checkEnableCodex validates item 4: the step's position, the two routes and
// their exact fences.
func checkEnableCodex(docs map[string]string) error {
	readme := docs["readme"]
	sec := mdSection(readme, "## Run with Callsheet")
	if !strings.Contains(sec, "1. Complete the [quickstart](../../quickstart.md).\n"+enableCodexStep+"\n3. Authenticate both workers") {
		return fmt.Errorf("the Codex step is not between the quickstart and authentication")
	}
	sub := mdSubsection(sec, "### Enable Codex")
	if sub == "" {
		return fmt.Errorf("no Enable Codex subsection")
	}
	if err := requirePhrases(sub, []string{enableCodexReason, enableCodexRestart, enableCodexSecond}); err != nil {
		return err
	}
	if err := localAnchors(readme); err != nil {
		return err
	}
	fences, err := parseMarkedFences(readme, devFlowMarker)
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(enableCodexFences)) {
		f, ok := fenceByID(fences, id)
		if !ok {
			return fmt.Errorf("no %s fence", id)
		}
		p, err := parseCallsheetOnly(f)
		if err != nil {
			return err
		}
		want := enableCodexFences[id]
		if len(p.callsheet) != len(want) {
			return fmt.Errorf("fence %s holds %d commands, want %d", id, len(p.callsheet), len(want))
		}
		for i, argv := range want {
			if err := checkArgv("fence "+id, p.callsheet[i], argv); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTriageEnvelope validates item 5: the fields of the triage example and
// its scratch explanation. The dispatch schema is checked with the other two
// envelopes by checkDevFlowEnvelopes.
func checkTriageEnvelope(docs map[string]string) error {
	fences, err := markedJSONFences(docs["readme"], devFlowJSONMarker)
	if err != nil {
		return err
	}
	raw, ok := fences["triage"]
	if !ok {
		return fmt.Errorf("no triage envelope")
	}
	var env struct {
		Goal       string `json:"goal"`
		Acceptance string `json:"acceptance"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return err
	}
	if err := requirePhrases(env.Goal, triageEnvelopeFields); err != nil {
		return fmt.Errorf("triage goal: %w", err)
	}
	if err := requirePhrases(env.Acceptance, []string{"TRIAGE_CONFIRMED", "TRIAGE_PROBABLE", "TRIAGE_UNCONFIRMED", "TRIAGE_BLOCKED", "fix.md only at CONFIRMED or PROBABLE"}); err != nil {
		return fmt.Errorf("triage acceptance: %w", err)
	}
	if err := taskTextBudget([]byte(env.Goal), []byte(env.Acceptance)); err != nil {
		return err
	}
	return requirePhrases(mdSection(docs["readme"], "## Dispatch example: triage"), []string{triageEnvelopeProse})
}

// checkTriageRestriction validates C1: the runbook still exports the four
// files, and the restriction stands beside the triage envelope, in the light
// walkthrough, at the source-folder step and in the guide chapter.
func checkTriageRestriction(docs map[string]string) error {
	if err := requirePhrases(mdSection(docs["triage"], "## Reproductions"), []string{triageRunbookExport}); err != nil {
		return fmt.Errorf("the restriction no longer matches the runbook: %w", err)
	}
	if err := requirePhrases(mdSection(docs["readme"], "## Dispatch example: triage"), []string{
		triageRestrictionExport, triageRestrictionFit, triageRestrictionOther, triageRestrictionScope}); err != nil {
		return fmt.Errorf("triage example: %w", err)
	}
	if err := requirePhrases(mdSection(docs["readme"], "## Light walkthrough"), []string{lightWalkthroughScope}); err != nil {
		return fmt.Errorf("light walkthrough: %w", err)
	}
	if err := requirePhrases(mdSection(docs["readme"], "## Run with Callsheet"), []string{sourceFolderScope}); err != nil {
		return fmt.Errorf("source-folder step: %w", err)
	}
	if err := requirePhrases(mdSection(docs["example"], "## Follow the example"), []string{chapterLightScope}); err != nil {
		return fmt.Errorf("example chapter: %w", err)
	}
	return localAnchors(docs["readme"])
}

// checkHarnessNotQualifying validates W1: the ASSUMPTION paragraph's sentences
// verbatim, followed directly by the harness statement.
func checkHarnessNotQualifying(docs map[string]string) error {
	p := exampleAssumption + " " + assumptionProbeSentence + " A rejection stops the attempt. There is no model fallback.\n\n" + harnessNotQualifying + " " + ownerQualificationNeed
	if !strings.Contains(mdSection(docs["readme"], "## Role selection"), p) {
		return fmt.Errorf("the Role selection section does not hold the ASSUMPTION followed by the harness statement")
	}
	return nil
}

// checkGrokEvidence validates item 6: the dated line, in the Grok section,
// with no Cursor claim or task ID, beside the kept limit and assumption.
func checkGrokEvidence(docs map[string]string) error {
	sec := mdSection(docs["worker"], "## Grok Build")
	if err := requirePhrases(sec, []string{grokObservation, "Useful tool execution under current auto remains unverified.", guideGrokAssumption}); err != nil {
		return err
	}
	for _, line := range strings.Split(sec, "\n") {
		if !strings.Contains(line, "2026-11-02") {
			continue
		}
		if line != grokObservation {
			return fmt.Errorf("the dated Grok line is not the exact line: %q", line)
		}
	}
	if m := taskIDRE.FindString(sec); m != "" {
		return fmt.Errorf("the Grok section names task %s", m)
	}
	return nil
}

// checkReviewerTaskID validates item 7, with the scratch paths unchanged.
func checkReviewerTaskID(docs map[string]string) error {
	return requirePhrases(mdSection(docs["reviewer"], "## Scratch"), []string{
		reviewerTaskIDClause, ".callsheet-review-scratch/<task-id>/before/", ".callsheet-review-scratch/<task-id>/after/"})
}

// checkDeniedVerification validates item 9 on both pages.
func checkDeniedVerification(docs map[string]string) error {
	if err := requirePhrases(mdSection(docs["playbook"], "## Admission"), []string{deniedVerificationRule}); err != nil {
		return fmt.Errorf("playbook admission: %w", err)
	}
	if !strings.Contains(mdSection(docs["worker"], "## Before enabling a worker"), workerRefusalSentence+" "+workerDeniedCommand) {
		return fmt.Errorf("worker setup does not say what a refusal covers")
	}
	if strings.Contains(docs["worker"], "An authentication or permission refusal stops the operation") {
		return fmt.Errorf("worker setup keeps the unscoped refusal sentence")
	}
	return nil
}

// checkSidecarUserConfig validates item 10 in the quickstart's prerequisites.
func checkSidecarUserConfig(docs map[string]string) error {
	if !strings.Contains(mdSection(docs["quickstart"], "## Prerequisites"), sidecarUserConfig) {
		return fmt.Errorf("the prerequisites lack the sidecar user's configuration note")
	}
	return nil
}

// checkDeliveredDesignDocs validates item 11: both walkthroughs and the chapter.
func checkDeliveredDesignDocs(docs map[string]string) error {
	for _, name := range []string{"## Full walkthrough", "## Light walkthrough"} {
		if n := strings.Count(mdSection(docs["readme"], name), deliveredDesignDocs); n != 1 {
			return fmt.Errorf("%s carries the delivered-tree sentence %d times", name, n)
		}
	}
	if n := strings.Count(mdSection(docs["example"], "## Follow the example"), deliveredDesignDocs); n != 1 {
		return fmt.Errorf("the example chapter carries the delivered-tree sentence %d times", n)
	}
	return nil
}

// checkFlowFile requires a flow document to hold exactly rows.
func checkFlowFile(name, doc string, rows []roleRow) error {
	var flow struct {
		Roles []struct{ ID, Adapter, Model, Effort string } `json:"roles"`
	}
	if err := json.Unmarshal([]byte(doc), &flow); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if len(flow.Roles) != len(rows) {
		return fmt.Errorf("%s holds %d roles, want %d", name, len(flow.Roles), len(rows))
	}
	for i, r := range rows {
		got := flow.Roles[i]
		if got.ID != r.Name || got.Adapter != r.Adapter || got.Model != r.Model || got.Effort != r.Effort {
			return fmt.Errorf("%s row %d is %s/%s/%s/%s, want %s/%s/%s/%s", name, i, got.ID, got.Adapter, got.Model, got.Effort, r.Name, r.Adapter, r.Model, r.Effort)
		}
	}
	return nil
}

// cursorHarnessRoles is flow-cursor-coder.json: the harness selection with the
// coder alone on Cursor.
func cursorHarnessRoles() []roleRow {
	out := slices.Clone(harnessRoles)
	for i := range out {
		if out[i].Name == "coder" {
			out[i].Adapter, out[i].Model = "cursor", "grok-4.7"
		}
	}
	return out
}

// checkSelectionSplit validates both halves: the example's tables and
// registration hold canonicalRoles, the harness's flow files and runbook hold
// harnessRoles, and neither claims the other's evidence.
func checkSelectionSplit(docs map[string]string) error {
	for _, name := range []string{"readme", "playbook"} {
		rows, err := parseRoleTable(docs[name])
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !sameRoles(rows, canonicalRoles) {
			return fmt.Errorf("%s: the role table is not the example's selection", name)
		}
	}
	if err := checkRegistration(docs["readme"], canonicalRoles); err != nil {
		return err
	}
	if err := checkFlowFile("flow.json", docs["flow"], harnessRoles); err != nil {
		return err
	}
	if err := checkFlowFile("flow-cursor-coder.json", docs["cursorflow"], cursorHarnessRoles()); err != nil {
		return err
	}
	for _, r := range harnessRoles {
		if !strings.Contains(docs["e2e"], fmt.Sprintf("| %s | %s | %s | %s |", r.Name, r.Adapter, r.Model, r.Effort)) {
			return fmt.Errorf("the runbook's role table lacks the harness row %s", r.Name)
		}
	}
	var errs []error
	for _, name := range []string{"readme", "playbook"} {
		for _, s := range staleExampleClaims {
			if strings.Contains(docs[name], s) {
				errs = append(errs, fmt.Errorf("%s still claims %q", name, s))
			}
		}
	}
	return errors.Join(errs...)
}

// checkExampleEvidence validates the README's evidence statement and the
// chapter's, the kept assumption, the pairing rule and the Cursor paragraph.
func checkExampleEvidence(docs map[string]string) error {
	if err := requirePhrases(docs["readme"], []string{
		exampleEvidenceEarlier, exampleEvidenceProof, exampleEvidenceNone, exampleHarnessKeepsOwn, exampleAssumption,
		"`CLAUDE_NODE` | The node ID for design-reviewer, coder and triage",
		"`CODEX_NODE` | The node ID for designer and code-reviewer",
	}); err != nil {
		return fmt.Errorf("README: %w", err)
	}
	if err := requirePhrases(docs["example"], []string{exampleChapterEarlier, exampleChapterNone}); err != nil {
		return fmt.Errorf("example chapter: %w", err)
	}
	return requirePhrases(docs["playbook"], []string{playbookHarnessSelection})
}

// TestDocsSmokeFindingsPartA covers the smoke run's documentation findings.
func TestDocsSmokeFindingsPartA(t *testing.T) {
	cases := []struct {
		name  string
		check func(map[string]string) error
		kind  string
		muts  [][2]string
	}{
		{"item1_sandbox_hosting", checkSandboxHosting, "coordinator", append(deletions(sandboxHostingNamespace, sandboxHostingRoute, sandboxHostingScope),
			[2]string{sandboxHostingScope, "This holds for every sandbox."},
			[2]string{"(quickstart.md#one-machine)", "(quickstart.md)"},
			[2]string{"(deploy.md#run-as-services)", "(deploy.md)"})},
		{"item2_readiness_quickstart", checkTransientReadiness, "quickstart", append(deletions(readinessTransient, readinessNeverRetryBlind),
			[2]string{readinessQuickstartNext, ": wait 5 seconds, then dispatch."},
			[2]string{readinessTransient, readinessTransient + " for 30 seconds"})},
		{"item2_readiness_roles", checkTransientReadiness, "roles", append(deletions(readinessTransient),
			[2]string{readinessRolesNext, ": retry `role add`."},
			[2]string{readinessTransient, readinessTransient + " for 30 seconds"})},
		{"item3_mcp_tools", checkMCPToolsObservation, "coordinator", append(deletions(mcpToolsObservation),
			[2]string{"Observed on 2026-11-02, with Claude Code 2.1.316", "Observed on 2026-11-02"},
			[2]string{"until it was restarted or resumed", "at once"})},
		{"item4_enable_codex", checkEnableCodex, "readme", append(deletions(enableCodexStep+"\n", enableCodexReason, enableCodexRestart, enableCodexSecond),
			[2]string{"### Enable Codex", "### Codex"},
			[2]string{"<!-- dev-flow: enable-codex-restart -->\n", ""},
			[2]string{"<!-- dev-flow: enable-codex-second-sidecar -->\n", ""},
			[2]string{`--claude-adapter "$CLAUDE_BIN" --codex-adapter "$CODEX_BIN"`, `--codex-adapter "$CODEX_BIN"`},
			[2]string{`--claude-adapter "$CLAUDE_BIN" --codex-adapter "$CODEX_BIN"`, `--claude-adapter "$CLAUDE_BIN"`},
			[2]string{`callsheet sidecar enroll --state-dir "$CODEX_SIDECAR_STATE" --plane "$PLANE" --ca "$CA"` + "\n", ""},
			[2]string{"The node keeps its ID and its registered roles", "The node gets a new ID"},
			[2]string{"it serves designer and code-reviewer here", "it serves designer, code-reviewer and triage here"})},
		{"item5_triage_envelope", checkTriageEnvelope, "readme", append(deletions(triageEnvelopeProse),
			[2]string{"scratch_policy: " + triageScratchRecipe, "scratch_policy: rm -rf .callsheet-triage-scratch"},
			[2]string{`\nscratch_policy: ` + triageScratchRecipe, ""},
			[2]string{`\nallowed output paths: rca.md; fix.md only at CONFIRMED or PROBABLE`, `\nallowed output paths: rca.md and fix.md`},
			[2]string{`evidence location: none; see triage-runbook.md`, `evidence location: /tmp/triage`},
			[2]string{`\ninitial_source_commit: <40-hex-buggy-commit>`, ""},
			[2]string{"<!-- dev-flow-json: triage -->\n", ""},
			[2]string{"## Dispatch example: triage", "## Dispatch example: investigation"})},
		{"item6_grok", checkGrokEvidence, "worker", append(deletions(grokObservation, "Useful tool execution under current auto remains unverified.", guideGrokAssumption),
			[2]string{"compound shell commands were not tested", "compound shell commands work"},
			[2]string{"in one simple task", "in every task"},
			[2]string{grokObservation, grokObservation + " Cursor did the same."},
			[2]string{grokObservation, grokObservation + "\n\nTask t_0123456789abcdef0123456789abcdef."})},
		{"item7_reviewer_task_id", checkReviewerTaskID, "reviewer", append(deletions(reviewerTaskIDClause),
			[2]string{"not one the coordinator provides", "from the coordinator"},
			[2]string{".callsheet-review-scratch/<task-id>/before/", ".callsheet-review-scratch/before/"})},
		{"item9_playbook", checkDeniedVerification, "playbook", append(deletions(deniedVerificationRule),
			[2]string{"as not done", "as done"})},
		{"item9_worker", checkDeniedVerification, "worker", append(deletions(workerRefusalSentence, workerDeniedCommand),
			[2]string{"An authentication or permission refusal that ends a task stops", "An authentication or permission refusal stops"},
			[2]string{"a worker can continue after one", "the task stops at once"})},
		{"item10_sidecar_user", checkSidecarUserConfig, "quickstart", append(deletions(sidecarUserConfig),
			[2]string{"including any sandbox setting", "except any sandbox setting"},
			[2]string{"(deploy.md#accounts-and-paths)", "(deploy.md)"})},
		{"item11_readme", checkDeliveredDesignDocs, "readme", [][2]string{
			{deliveredDesignDocs, ""},
			{"pull the accepted task into a new destination. " + deliveredDesignDocs, "pull the accepted task into a new destination."},
		}},
		{"item11_chapter", checkDeliveredDesignDocs, "example", deletions(deliveredDesignDocs)},
		{"c1_restriction_readme", checkTriageRestriction, "readme", append(deletions(triageRestrictionExport, triageRestrictionFit, triageRestrictionOther, triageRestrictionScope, lightWalkthroughScope, sourceFolderScope),
			[2]string{"exports exactly four root files", "exports the project's files"},
			[2]string{"It fits only a project those four files fully reproduce", "It fits any project"},
			[2]string{"triage reports `TRIAGE_BLOCKED` when it needs a reproduction", "triage reproduces from what it finds"},
			[2]string{"## Dispatch example: triage", "## Dispatch example: investigation"})},
		{"c1_restriction_chapter", checkTriageRestriction, "example", append(deletions(chapterLightScope),
			[2]string{"(../examples/dev-flow/README.md#seed-project-restriction)", "(../examples/dev-flow/README.md)"})},
		{"c1_restriction_tracks_runbook", checkTriageRestriction, "triage", deletions(triageRunbookExport)},
		{"w1_harness_statement", checkHarnessNotQualifying, "readme", append(deletions(harnessNotQualifying, ownerQualificationNeed, assumptionProbeSentence, exampleAssumption),
			[2]string{"does not qualify this one", "qualifies this one"},
			[2]string{"it keeps its earlier selection", "it follows this table"},
			[2]string{"refuses Claude as triage", "accepts Claude as triage"},
			[2]string{"remains necessary", "is optional"},
			[2]string{"the later live harness must probe", "the shipped harness probes"})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutateExpect(t, smokeFindingDocs(t), c.check, c.kind, c.muts)
		})
	}
	t.Run("no_sandbox_disable", func(t *testing.T) {
		docs := smokeFindingDocs(t)
		for _, name := range sandboxDocs {
			head, _, _ := strings.Cut(docs[name], "\n")
			mutateExpect(t, docs, checkNoSandboxDisable, name, [][2]string{
				{head, head + "\n\nDisable the sandbox to reach the plane."},
				{head, head + "\n\nTurn off your sandbox if a call fails."},
				{head, head + "\n\nSet `{\"sandbox\":{\"enabled\":false}}` in your settings."},
			})
		}
		mutateExpect(t, docs, checkNoSandboxDisable, "coordinator", deletions("do not turn on broad approval or disable sandboxing"))
		mutateExpect(t, docs, checkNoSandboxDisable, "quickstart", deletions("do not disable sandboxing"))
	})
}

// TestDocsExampleSelectionSplit covers the example's selection and its
// separation from the harness's qualified one, with mutations crossing the
// two in both directions.
func TestDocsExampleSelectionSplit(t *testing.T) {
	t.Run("two_named_selections", func(t *testing.T) {
		if sameRoles(canonicalRoles, harnessRoles) {
			t.Fatal("the example's selection and the harness's are the same")
		}
		var differ []string
		for i := range canonicalRoles {
			if canonicalRoles[i] != harnessRoles[i] {
				differ = append(differ, canonicalRoles[i].Name)
			}
		}
		if !slices.Equal(differ, []string{"designer", "design-reviewer", "triage"}) {
			t.Fatalf("the selections differ in %q", differ)
		}
		for _, rows := range [][]roleRow{canonicalRoles, harnessRoles} {
			for _, r := range rows {
				if err := adapter.ValidateSelection(r.Adapter, r.Model, r.Effort); err != nil {
					t.Errorf("%s: %v", r.Name, err)
				}
			}
		}
	})
	t.Run("harness_go_default", func(t *testing.T) {
		// The harness's compiled selection is the named harness half.
		def := reale2e.DefaultDevFullFlow()
		if len(def.Roles) != len(harnessRoles) {
			t.Fatalf("the harness default holds %d roles", len(def.Roles))
		}
		for i, r := range harnessRoles {
			got := def.Roles[i]
			if got.ID != r.Name || got.Adapter != r.Adapter || got.Model != r.Model || got.Effort != r.Effort {
				t.Errorf("harness default row %d is %+v, want %+v", i, got, r)
			}
		}
	})
	t.Run("harness_refuses_claude_triage", func(t *testing.T) {
		// W1's claim about the harness is its parser's behavior, not prose.
		flow := repoFile(t, "docs/examples/real-e2e/dev-full/flow.json")
		if _, err := reale2e.ParseDevFullFlow(flow); err != nil {
			t.Fatalf("the shipped flow is refused: %v", err)
		}
		const old = `{"id": "triage", "adapter": "codex", "model": "gpt-6.1-sol", "effort": "high"}`
		if !strings.Contains(string(flow), old) {
			t.Fatalf("flow.json lacks %q", old)
		}
		claude := strings.Replace(string(flow), old, `{"id": "triage", "adapter": "claude", "model": "claude-opus-5-5", "effort": "high"}`, 1)
		if _, err := reale2e.ParseDevFullFlow([]byte(claude)); err == nil || !strings.Contains(err.Error(), "role triage must use adapter codex") {
			t.Fatalf("a Claude triage flow: err = %v", err)
		}
	})
	docs := smokeFindingDocs(t)
	row := func(r roleRow) string {
		return fmt.Sprintf("| %s | %s | %s | %s | %s | %s |", r.Name, r.Adapter, r.Model, r.Effort, r.Concurrency, r.Timeout)
	}
	// Example documents crossed onto the harness's rows.
	var tables [][2]string
	for i := range canonicalRoles {
		if canonicalRoles[i] != harnessRoles[i] {
			tables = append(tables, [2]string{row(canonicalRoles[i]), row(harnessRoles[i])})
		}
	}
	t.Run("readme_crossed_to_harness", func(t *testing.T) {
		mutateExpect(t, docs, checkSelectionSplit, "readme", append(slices.Clone(tables),
			[2]string{`--model gpt-6-astra --effort medium`, `--model gpt-6-astra --effort low`},
			[2]string{`--model claude-fable-5-1 --effort medium`, `--model claude-fable-5-1 --effort low`},
			[2]string{`add triage --name triage --node "$CLAUDE_NODE" --adapter claude`, `add triage --name triage --node "$CODEX_NODE" --adapter codex`},
			[2]string{`"$MANUALS/triage-runbook.md" --model claude-opus-5-5`, `"$MANUALS/triage-runbook.md" --model gpt-6.1-sol`},
			[2]string{`add triage --name triage --node "$CLAUDE_NODE"`, `add triage --name triage --node "$CODEX_NODE"`},
			[2]string{"the default table above is unchanged", "the default table above is unchanged and the code reviewer and triage stay on Codex"},
		))
	})
	t.Run("playbook_crossed_to_harness", func(t *testing.T) {
		mutateExpect(t, docs, checkSelectionSplit, "playbook", append(slices.Clone(tables),
			[2]string{"triage (Opus) and the coder (Opus) share a family", "triage (GPT) diagnoses for the coder (Opus)"},
		))
	})
	// Harness documents crossed onto the example's rows.
	t.Run("flow_crossed_to_example", func(t *testing.T) {
		for _, kind := range []string{"flow", "cursorflow"} {
			mutateExpect(t, docs, checkSelectionSplit, kind, [][2]string{
				{`"model": "gpt-6-astra", "effort": "low"`, `"model": "gpt-6-astra", "effort": "medium"`},
				{`"model": "claude-fable-5-1", "effort": "low"`, `"model": "claude-fable-5-1", "effort": "medium"`},
				{`{"id": "triage", "adapter": "codex", "model": "gpt-6.1-sol", "effort": "high"}`, `{"id": "triage", "adapter": "claude", "model": "claude-opus-5-5", "effort": "high"}`},
			})
		}
	})
	t.Run("runbook_crossed_to_example", func(t *testing.T) {
		mutateExpect(t, docs, checkSelectionSplit, "e2e", [][2]string{
			{"| designer | codex | gpt-6-astra | low |", "| designer | codex | gpt-6-astra | medium |"},
			{"| design-reviewer | claude | claude-fable-5-1 | low |", "| design-reviewer | claude | claude-fable-5-1 | medium |"},
			{"| triage | codex | gpt-6.1-sol | high |", "| triage | claude | claude-opus-5-5 | high |"},
		})
	})
	t.Run("evidence", func(t *testing.T) {
		mutateExpect(t, docs, checkExampleEvidence, "readme", append(deletions(exampleEvidenceEarlier, exampleEvidenceProof, exampleEvidenceNone, exampleHarnessKeepsOwn, exampleAssumption),
			[2]string{exampleEvidenceNone, "The role selection below ran in the recorded exercise."},
			[2]string{"Those records prove that earlier selection's", "Those records prove this selection's"},
			[2]string{"`CODEX_NODE` | The node ID for designer and code-reviewer", "`CODEX_NODE` | The node ID for designer, code-reviewer and triage"},
			[2]string{"`CLAUDE_NODE` | The node ID for design-reviewer, coder and triage", "`CLAUDE_NODE` | The node ID for design-reviewer and coder"},
		))
		mutateExpect(t, docs, checkExampleEvidence, "example", append(deletions(exampleChapterEarlier, exampleChapterNone),
			[2]string{"prove an earlier selection's", "prove that selection's"},
		))
		mutateExpect(t, docs, checkExampleEvidence, "playbook", deletions(playbookHarnessSelection))
	})
	t.Run("harness_clause_scope", func(t *testing.T) {
		// The real-e2e runbook's qualification text stays on the harness.
		if !strings.Contains(docs["e2e"], "Triage runs on Codex `gpt-6.1-sol` at `high`") {
			t.Fatal("the runbook's selection text changed")
		}
	})
}
