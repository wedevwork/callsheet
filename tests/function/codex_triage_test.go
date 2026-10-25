// Codex triage documentation contracts (design 12a-codex-triage, FP-2c,
// FP-4, FP-5a, FP-5b and FP-6). The checks read shipped documentation and
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
		"e2e":      string(repoFile(t, "docs/real-e2e.md")),
		"readme":   string(repoFile(t, "docs/examples/dev-flow/README.md")),
		"playbook": string(repoFile(t, "docs/examples/dev-flow/coordinator-playbook.md")),
		"flow":     string(repoFile(t, "docs/examples/real-e2e/dev-full/flow.json")),
	}
}

// historicalRecordClauses are FP-2c's required clauses.
var historicalRecordClauses = []string{
	"Bundles and qualification records produced with the Grok triage selection are kept as they are, with no migration; the harness never rewrites them.",
	"Check each with the revision that produced it: this revision adds no cross-revision decoder and does not accept their old selection.",
	"A historical `callsheet-dev-full-qualification/v1` record keeps its full-only meaning.",
	"No historical qualification, including an old PASS or QUALIFIED record, qualifies the Codex triage selection.",
	"A fresh owner probe 10 is an operator prerequisite you record yourself; the harness does not look up a qualification file at startup.",
}

// checkHistoricalRecords validates FP-2c in docs/real-e2e.md.
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
	return nil
}

func TestCodexTriageFP2c(t *testing.T) {
	docs := codexDocs(t)
	mutateExpect(t, docs, checkHistoricalRecords, "e2e", append(deletions(historicalRecordClauses...),
		[2]string{"with no migration", "migrated to the new selection"},
		[2]string{"Check each with the revision that produced it", "Check each with this revision"},
		[2]string{"No historical qualification, including an old PASS or QUALIFIED record, qualifies", "An old PASS or QUALIFIED record still qualifies"},
		[2]string{"the harness does not look up a qualification file at startup", "the harness looks up the latest qualification file at startup"},
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

// retainedGrokClauses are docs/real-e2e.md's harness prerequisite and
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
}

// qualifiedRE finds a qualification claim.
var qualifiedRE = regexp.MustCompile(`(?i)\bqualif(?:ied|ies|y)\b|\bpass(?:ed)?\b`)

// checkFailedProbes validates FP-5a in docs/real-e2e.md.
func checkFailedProbes(docs map[string]string) error {
	e2e := docs["e2e"]
	sec := mdSubsection(e2e, "#### Failed probe history")
	if sec == "" || !strings.Contains(mdSubsection(e2e, "### Codex triage qualification"), sec) {
		return fmt.Errorf("no Failed probe history section under Codex triage qualification")
	}
	if err := requirePhrases(sec, failedProbeClauses); err != nil {
		return err
	}
	// Neither bundle is relabeled: every sentence claiming a pass or
	// qualification is negated or is the reviewer stage's own pass.
	for _, s := range regexp.MustCompile(`\.\s+`).Split(sec, -1) {
		if !qualifiedRE.MatchString(s) {
			continue
		}
		if !strings.Contains(s, "neither") && !strings.Contains(s, " not ") && !strings.Contains(s, "reviewer stage passed") {
			return fmt.Errorf("a failed probe is relabeled: %q", s)
		}
	}
	// The pending probe 9 gate is gone.
	for _, gone := range []string{"owner probe 9:", "A probe 9 failure", "The gate is owner probe 9"} {
		if strings.Contains(e2e, gone) {
			return fmt.Errorf("the runbook still carries %q", gone)
		}
	}
	return nil
}

func TestCodexTriageFP5a(t *testing.T) {
	docs := codexDocs(t)
	mutateExpect(t, docs, checkFailedProbes, "e2e", append(deletions(failedProbeClauses...),
		[2]string{"neither bundle is relabeled as qualified", "both bundles are relabeled as qualified"},
		[2]string{"it does not show that Codex triage succeeds.", "it does not show that Codex triage succeeds. Probe 9b is QUALIFIED."},
		[2]string{"`94ee4fc6209b21ec998617af4d2a5aeb3c243018`", "`25a770797f69612df1c21e325afa527445677609`"},
		[2]string{"(`light_reviewer_evidence/not_reached`)", "(`light_reviewer_evidence/pass`)"},
		[2]string{"#### Failed probe history", "#### Probe history"},
		[2]string{"The gate is owner probe 10:", "The gate is owner probe 9: `probe-dev-full --scenario both`. The gate is owner probe 10:"},
	))
}

// probe10Command is the documented owner probe 10 command.
const probe10Command = "CALLSHEET_REAL_E2E=1 /abs/reale2e probe-dev-full --scenario both --callsheet /abs/callsheet --claude /abs/claude --codex /abs/codex --grok /abs/grok --protect-root /abs/projects --evidence /abs/callsheet/design/real-e2e-runs/20261025-probe10"

// probe10Clauses are FP-5b's required clauses.
var probe10Clauses = []string{
	"Run owner probe 10 only after the pre-probe smoke passed, on the implementation revision with the actual built binaries:",
	"Use a fresh evidence path if that one exists.",
	"The operator prerequisites stay: Linux, the opt-in, no CI, a quiescent protected environment, supported authenticated Claude and Codex selections, the required eligible Grok executable, and Go and Git, at the current minimum versions; the harness performs no login and no vendor-home change.",
	"Record in the owner evidence the exact command and SHA, the versions, the new selection digest and registered roles, the actual Codex argv and effective effort, the reproduction's tool execution, the scratch paths and the literal cleanup, the valid triage marker, the admitted report-only tree, the light reviewer's execution evidence and every stage verdict.",
	"Claim qualification only when all six stages (`models`, `resume_wake`, `print_notification`, `reviewer_evidence`, `triage_evidence` and `light_reviewer_evidence`) pass and the probe's offline qualification check of its written record agrees.",
	"Inspect the logs for the reproduction and its cleanup, not just the final marker: a passing marker alone does not show that the scratch instruction was followed.",
	"On a failure, keep the evidence and return to investigation; no retry around a refusal, timeout increase or permission substitution.",
	"**ASSUMPTION (unverified):** this installed Codex recipe reproduces and cleans up under the triage envelope.",
	"Probe 10 settles that deployment prerequisite.",
	"A pass proves only that this selection and these fixtures completed on that installation with the recorded policy; it does not prove that all prompts are approved, general confinement, future vendor behavior, recovery paths it did not execute, or a complete owner-authorized `run-dev-exercise`.",
	"Codex can differ from Grok in prompt interpretation and permission behavior.",
	"The later combined real attempt remains separately required.",
	"Offline tests cannot qualify a paid vendor.",
}

// checkProbe10 validates FP-5b in docs/real-e2e.md.
func checkProbe10(docs map[string]string) error {
	e2e := docs["e2e"]
	sec := mdSubsection(e2e, "#### Owner probe 10")
	if sec == "" || !strings.Contains(mdSubsection(e2e, "### Codex triage qualification"), sec) {
		return fmt.Errorf("no Owner probe 10 section under Codex triage qualification")
	}
	if !strings.Contains(sec, "```sh\n"+probe10Command+"\n```\n") {
		return fmt.Errorf("the both-scope probe 10 command is missing or changed")
	}
	if err := requirePhrases(sec, probe10Clauses); err != nil {
		return err
	}
	// The pending gate in the scratch section points at probe 10.
	return requirePhrases(mdSubsection(e2e, "### Light triage scratch"), []string{
		"Requalification is pending.",
		"The gate is owner probe 10: `probe-dev-full --scenario both` on the Codex triage revision, after the [pre-probe smoke](#pre-probe-smoke), without configuration changes or fallback",
		"Offline success does not qualify the live recipe.",
	})
}

func TestCodexTriageFP5b(t *testing.T) {
	docs := codexDocs(t)
	mutateExpect(t, docs, checkProbe10, "e2e", append(deletions(append(slices.Clone(probe10Clauses), "Requalification is pending.", "Offline success does not qualify the live recipe.")...),
		[2]string{probe10Command, strings.Replace(probe10Command, "--scenario both", "--scenario light", 1)},
		[2]string{probe10Command, strings.Replace(probe10Command, "--scenario both ", "", 1)},
		[2]string{"all six stages", "the triage stage"},
		[2]string{"**ASSUMPTION (unverified):**", "**Proven:**"},
		[2]string{"The gate is owner probe 10:", "The gate is owner probe 9:"},
		[2]string{"#### Owner probe 10", "#### Probe"},
	))
}

// The FP-6 smoke's pinned parts (design 12a-codex-triage, Pre-probe
// smoke, with the post-final DS1 clarification: the older-toolchain
// exception names the go.mod directive, never a version number).
const (
	smokeRepoAssignment = "(set -eu; smoke_repo=${CALLSHEET_REPO:-/opt/gitspace/callsheet}; "
	smokeMktemp         = `smoke_dir=$(mktemp -d /tmp/callsheet-triage-smoke.XXXXXXXX); printf "SMOKE_DIR=%s\n" "$smoke_dir"; mkdir "$smoke_dir/work"; `
	smokeSeedCopy       = `cp "$smoke_repo/internal/reale2e/testdata/devlight-seed/FEATURE.md" "$smoke_repo/internal/reale2e/testdata/devlight-seed/main.go" "$smoke_repo/internal/reale2e/testdata/devlight-seed/main_test.go" "$smoke_dir/work/"; `
	smokeAwk            = `awk '$1 == "go" && NF == 2 { print "module example.com/greeting\n\ngo " $2; found=1; exit } END { if (!found) exit 1 }' "$smoke_repo/go.mod" > "$smoke_dir/work/go.mod"; `
	smokeCwd            = `cd "$smoke_dir/work"; `
	smokeGoEnv          = `GOCACHE="$PWD/.callsheet-triage-scratch/gocache" GOMODCACHE="$PWD/.callsheet-triage-scratch/gomodcache" TMPDIR="$PWD/.callsheet-triage-scratch/tmp" GOTMPDIR="$PWD/.callsheet-triage-scratch/tmp" GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -count=1 ./...`
	smokeCodex          = `"$HOME/.local/bin/codex" `
	smokeRedirects      = ` > "$smoke_dir/codex.jsonl" 2> "$smoke_dir/codex.stderr"; `
	smokeTail           = `test "$(cat "$smoke_dir/callsheet-final.txt")" = SMOKE_OK; test ! -e .callsheet-triage-scratch; test ! -L .callsheet-triage-scratch; printf "SMOKE_PASS\n")`
	smokeFinal          = `$smoke_dir/callsheet-final.txt`
)

// smokeClauses are the smoke's required prose.
var smokeClauses = []string{
	"Before probe 10, run this owner-run paid smoke once. It is non-qualifying, and neither CI, the implementation tests nor review automation runs it.",
	"Run it from any directory on the owner installation, with Codex installed at `$HOME/.local/bin/codex`.",
	"Set `CALLSHEET_REPO` to the absolute repository root if needed; it defaults to `/opt/gitspace/callsheet`.",
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
	"Stop before probe 10 and return to triage, without flag or configuration changes, alternative commands or retry workarounds.",
	"a failure of the form `go: ... requires go >= ...` under `GOTOOLCHAIN=local` means the installed Go is older than the repository's go.mod `go` directive",
	"That is an environment prerequisite failure to fix on your machine, not a Codex finding, and not a return to triage.",
	"Even a successful smoke is non-qualifying: no qualification record may cite it as qualification or as a substitute for any probe stage.",
	"probe 10 still settles the complete triage-envelope assumption.",
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
	if strings.Index(e2e, "#### Pre-probe smoke") > strings.Index(e2e, "#### Owner probe 10") {
		return fmt.Errorf("the smoke is not documented before probe 10")
	}
	if err := requirePhrases(sec, smokeClauses); err != nil {
		return err
	}
	line, err := smokeLine(sec)
	if err != nil {
		return err
	}
	// The fixed prefix: the repository variable and its default, the
	// external mktemp, the literal seed copy, the two-field go directive
	// and the copied seed as CWD.
	prefix := smokeRepoAssignment + smokeMktemp + smokeSeedCopy + smokeAwk + smokeCwd
	if !strings.HasPrefix(line, prefix) {
		return fmt.Errorf("the smoke's setup prefix changed")
	}
	if strings.Count(line, "/opt/gitspace/callsheet") != 1 || strings.Count(line, "smoke_repo=") != 1 || strings.Count(line, "CALLSHEET_REPO") != 1 {
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
		mutateExpect(t, docs, checkSmoke, "e2e", append(deletions(smokeClauses...),
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
			// Hardcoded or wrong repository variable or default.
			[2]string{"smoke_repo=${CALLSHEET_REPO:-/opt/gitspace/callsheet};", "smoke_repo=/opt/gitspace/callsheet;"},
			[2]string{"smoke_repo=${CALLSHEET_REPO:-/opt/gitspace/callsheet};", "smoke_repo=${REPO:-/opt/gitspace/callsheet};"},
			[2]string{"smoke_repo=${CALLSHEET_REPO:-/opt/gitspace/callsheet};", "smoke_repo=${CALLSHEET_REPO:-/opt/callsheet};"},
			[2]string{`cp "$smoke_repo/internal/reale2e/testdata/devlight-seed/FEATURE.md"`, `cp "/opt/gitspace/callsheet/internal/reale2e/testdata/devlight-seed/FEATURE.md"`},
			// Missing two-field awk guard.
			[2]string{`awk '$1 == "go" && NF == 2 {`, `awk '$1 == "go" {`},
			// Missing final checks.
			[2]string{`test ! -L .callsheet-triage-scratch; printf "SMOKE_PASS\n")`, `printf "SMOKE_PASS\n")`},
			[2]string{"final answer exactly SMOKE_OK", "final answer SMOKE_OK when done"},
			// Weakened toolchain exception, stop rule or non-qualification.
			[2]string{"not a Codex finding, and not a return to triage", "a Codex finding to triage"},
			[2]string{"Stop before probe 10 and return to triage", "Continue to probe 10"},
			[2]string{"Even a successful smoke is non-qualifying", "A successful smoke qualifies"},
		))
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
