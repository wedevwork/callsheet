package reale2e

import (
	"regexp"
	"strings"
	"testing"
)

// The probe's seed push order (owner probe 20261020T155113Z-505281a4,
// BLOCKED coordinator_tool_failed): dev-next answers seed_unpublished with
// exit 1 until the seed push is seen, and an errored vendor tool result is
// fatal, so the probe instructions must order the push before the first
// dev-next, as the full-flow coordinator prompt does.

// seedPromptValues are the placeholder values both seed-order tests render
// with.
func seedPromptValues() map[string]string {
	return map[string]string{"RUN_ID": "run", "RUNTIME": "/rt", "PLANE_URL": "https://127.0.0.1:1", "CA_PATH": "/ca.pem", "REALE2E": "/bin/reale2e",
		"WORKSPACE": "ws", "INSTANCE": strings.Repeat("a", 32), "SEED_PATH": "/seed", "SEED_COMMIT": strings.Repeat("b", 40), "COORD_DIR": "/coord"}
}

// sentences splits a rendered prompt into sentences and clauses.
func sentences(text []byte) []string { return regexp.MustCompile(`[.;]\s+`).Split(string(text), -1) }

// conditionalSeedPush matches a sentence that makes a push depend on a
// seed_unpublished answer.
var conditionalSeedPush = regexp.MustCompile(`(?i)\bif\b.*seed_unpublished.*push`)

// probeSentences renders the probe system prompt and splits it into
// sentences.
func probeSentences(t *testing.T) []string {
	t.Helper()
	out, err := renderProbePrompt("devProbeSystemPrompt", devProbeSystemPrompt, seedPromptValues())
	if err != nil {
		t.Fatal(err)
	}
	return sentences(out)
}

// TestDevPhaseSeedPushOrder is the same order for the full flow's phase
// prompt (docs/examples/real-e2e/dev-full/phase.md), which every
// coordinator phase receives: it must order the seed push before the first
// dev-next of the first phase, as coordinator-prompt.md does, and never
// make the push conditional on a seed_unpublished answer.
func TestDevPhaseSeedPushOrder(t *testing.T) {
	loaded, err := LoadDevMethod(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if loaded, err = RenderDevPrompts(loaded, seedPromptValues()); err != nil {
		t.Fatal(err)
	}
	var push []string
	for _, s := range sentences(loaded.Rendered[DevPhaseFile]) {
		if strings.Contains(strings.ToLower(s), "push the seed") {
			push = append(push, s)
		}
		if conditionalSeedPush.MatchString(s) {
			t.Errorf("the seed push is conditional on a seed_unpublished answer: %q", s)
		}
	}
	if len(push) != 1 || !strings.Contains(push[0], "before the first dev-next") || !strings.Contains(push[0], "first phase") {
		t.Fatalf("the seed push is not ordered before the first dev-next of the first phase: %q", push)
	}
}

func TestDevProbeSeedPushOrder(t *testing.T) {
	var push []string
	for _, s := range probeSentences(t) {
		if strings.Contains(s, "call ws_push") {
			push = append(push, s)
		}
		if conditionalSeedPush.MatchString(s) {
			t.Errorf("the seed push is conditional on a seed_unpublished answer: %q", s)
		}
	}
	if len(push) != 1 || !strings.Contains(push[0], "before the first dev-next") {
		t.Fatalf("the seed push is not ordered before the first dev-next: %q", push)
	}
}

// TestDevProbeSeedUnpublishedFatal pins the verifier rule the order relies
// on: a dev-next answered seed_unpublished (exit 1) is an errored vendor
// tool result, which fails the phase.
func TestDevProbeSeedUnpublishedFatal(t *testing.T) {
	stream := `{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_seed","name":"Bash","input":{"command":"/bin/reale2e dev-next --run /rt","description":"Ask probe harness for next task"}}]}}` + "\n" +
		`{"type":"user","session_id":"s","message":{"role":"user","content":[{"type":"tool_result","content":"Exit code 1\nseed_unpublished","is_error":true,"tool_use_id":"toolu_seed"}]}}` + "\n"
	if s := decodeDevStream([]byte(stream), ""); s.problem != devToolFailed {
		t.Fatalf("an errored seed_unpublished dev-next: %q, want %s", s.problem, devToolFailed)
	}
}

// TestDevPromptsWaitStartKeys pins design 12a-dev-flow-verify r0.9's
// prompt rule: both the full-flow coordinator prompt and the rendered
// probe system prompt ask for the background dev-wait-task Bash call with
// exactly command, description and run_in_background: true, and no
// timeout (the verifier still tolerates a valid optional timeout).
func TestDevPromptsWaitStartKeys(t *testing.T) {
	loaded, err := LoadDevMethod(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if loaded, err = RenderDevPrompts(loaded, seedPromptValues()); err != nil {
		t.Fatal(err)
	}
	probe, err := renderProbePrompt("devProbeSystemPrompt", devProbeSystemPrompt, seedPromptValues())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		text string
		want string
	}{
		"coordinator-prompt.md": {string(loaded.Rendered[DevPromptFile]),
			"Start it with one Bash call passing exactly `command` (that command), `description` (a short string) and `run_in_background: true`, with no `timeout` and no other key."},
		"devProbeSystemPrompt": {string(probe),
			"Start each wait with one Bash call passing exactly command (that command), description (a short string) and run_in_background: true, with no timeout and no other key."},
	} {
		if strings.Count(c.text, c.want) != 1 {
			t.Errorf("%s does not ask for exactly the three-key wait start", name)
		}
		if strings.Count(c.text, "timeout") != 1 {
			t.Errorf("%s mentions a timeout other than to exclude it", name)
		}
	}
}
