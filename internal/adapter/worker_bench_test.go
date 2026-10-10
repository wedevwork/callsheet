package adapter

import (
	"slices"
	"strings"
	"testing"
)

// BenchmarkWorkerSelection measures validating an alternate selection
// (ValidateSelection) plus building its invocation (design
// 12a-worker-selection) for each callable vendor within the existing
// recipe bounds, checking the literal argv and prompt transport every
// iteration. No process is started.
func BenchmarkWorkerSelection(b *testing.B) {
	scratch := "/state/tasks/" + taskID + "/work"
	prompt := []byte("{\"format\":\"callsheet-task-v1\"} reply pong")
	cases := []struct {
		id, model, effort string
		want              []string
		stdin             bool
	}{
		{ClaudeID, "claude-opus-5-5", "high", []string{"-p", "--model", "claude-opus-5-5", "--effort", "high", "--permission-prompts", "none", "--output-format", "json"}, true},
		{CodexID, "gpt-6-astra", "ultra", []string{"-a", "never", "exec", "--model", "gpt-6-astra", "-c", `model_reasoning_effort="ultra"`, "--json",
			"--skip-git-repo-check", "--output-last-message", scratch + "/" + CodexFinalName, "-"}, true},
		{GrokID, "grok-4.7-build-fast", "xhigh", []string{"--output-format", "json", "--model", "grok-4.7-build-fast", "--reasoning-effort", "xhigh",
			"--permission-mode", "dontAsk", "-p", string(prompt)}, false},
	}
	for _, c := range cases {
		a, _ := Builtin("").Lookup(c.id)
		b.Run(c.id, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := ValidateSelection(c.id, c.model, c.effort); err != nil {
					b.Fatal(err)
				}
				inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: c.model, Effort: c.effort, Prompt: prompt, ScratchDir: scratch})
				if err != nil || !slices.Equal(inv.Argv, c.want) || (len(inv.Stdin) == len(prompt)) != c.stdin {
					b.Fatalf("%s invocation %q %v", c.id, inv.Argv, err)
				}
			}
		})
	}
}

// BenchmarkWorkerVersion measures the pure version predicate on the
// minimum, a newer version, a near-limit (256-byte) recognizable but
// malformed line and an oversized line of another program, checking each
// decision every iteration. The input is bounded to 256 bytes and scanned
// linearly; larger output is refused, never parsed.
func BenchmarkWorkerVersion(b *testing.B) {
	malformed := "codex-cli 0.160.0-" + strings.Repeat("a", 64) + "." + strings.Repeat("b", 64) + "." + strings.Repeat("c", 64) + "."
	malformed += strings.Repeat("d", 256-len(malformed)-1) + "_"
	cases := []struct {
		name, id, baseline, out, want string
	}{
		{"minimum", ClaudeID, "2.1.285 (Claude Code)", "2.1.285 (Claude Code)\n", ""},
		{"newer", CodexID, "codex-cli 0.159.0", "codex-cli 0.160.0\n", ""},
		{"grok-newer", GrokID, "grok 1.0.46 (2765805b9442) [stable]", "grok 1.0.47 (0123456789ab) [beta]\n", ""},
		{"cursor-same-date", CursorID, "2026.10.01-e373342", "2026.10.01-abcdef0\n", ""},
		{"near-limit-malformed", CodexID, "codex-cli 0.159.0", malformed, "has an invalid codex version; expected an orderable version at or above codex-cli 0.159.0"},
		{"oversized-wrong-vendor", ClaudeID, "2.1.285 (Claude Code)", strings.Repeat("x", 4096), "is not the claude CLI (unexpected version output)"},
	}
	if len(malformed) != 256 {
		b.Fatalf("near-limit line is %d bytes", len(malformed))
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.out)))
			b.ReportAllocs()
			for b.Loop() {
				if got := checkWorkerVersion(c.id, c.baseline, c.out); got != c.want {
					b.Fatalf("%s: %q", c.name, got)
				}
			}
		})
	}
}
