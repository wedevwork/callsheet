package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/sidecar"
)

// TestRealAdapterCLIOptions is UT FP-1 for the CLI (iteration 08): the
// sidecar run vendor flags (each flag, duplicates, empty and relative
// values, omitted means disabled, mixed fake and real enablement), their
// help, and the role adapter diagnostics. No sidecar is started: the Run
// entry point is replaced to observe its options.
func TestRealAdapterCLIOptions(t *testing.T) {
	var got []sidecar.RunOptions
	orig := runSidecar
	runSidecar = func(_ context.Context, o sidecar.RunOptions) error {
		o.Logger = nil
		got = append(got, o)
		return nil
	}
	t.Cleanup(func() { runSidecar = orig })
	state := t.TempDir()
	run := func(args ...string) (int, string) {
		t.Helper()
		got = nil
		code, _, errOut := exec(t, "linux", append([]string{"sidecar", "run", "--state-dir", state}, args...)...)
		return code, errOut
	}
	// Each flag passes its path literally (spaces kept); omitted flags
	// leave that vendor disabled; the fake stays independent. Iteration 11's
	// grok and cursor flags behave alike, mixed with the others.
	for _, c := range []struct {
		args                               []string
		claude, codex, grok, cursor, fakeP string
	}{
		{nil, "", "", "", "", ""},
		{[]string{"--claude-adapter", "/opt/vendor tools/claude"}, "/opt/vendor tools/claude", "", "", "", ""},
		{[]string{"--codex-adapter=/usr/local/bin/codex"}, "", "/usr/local/bin/codex", "", "", ""},
		{[]string{"--claude-adapter", "/c", "--codex-adapter", "/x", "--fake-adapter", "/f"}, "/c", "/x", "", "", "/f"},
		{[]string{"--fake-adapter", "/f"}, "", "", "", "", "/f"},
		{[]string{"--grok-adapter", "/opt/vendor tools/grok"}, "", "", "/opt/vendor tools/grok", "", ""},
		{[]string{"--cursor-adapter=/home/op/.local/bin/cursor-agent"}, "", "", "", "/home/op/.local/bin/cursor-agent", ""},
		{[]string{"--grok-adapter", "/g", "--cursor-adapter", "/u", "--claude-adapter", "/c", "--codex-adapter", "/x", "--fake-adapter", "/f"}, "/c", "/x", "/g", "/u", "/f"},
		{[]string{"--cursor-adapter", "/u", "--fake-adapter", "/f"}, "", "", "", "/u", "/f"},
	} {
		if code, errOut := run(c.args...); code != 0 || len(got) != 1 {
			t.Fatalf("%v = %d %q", c.args, code, errOut)
		}
		o := got[0]
		if o.ClaudeAdapterPath != c.claude || o.CodexAdapterPath != c.codex || o.GrokAdapterPath != c.grok || o.CursorAdapterPath != c.cursor ||
			o.FakeAdapterPath != c.fakeP || o.StateDir != state || o.GOOS != "linux" {
			t.Fatalf("%v options %+v", c.args, o)
		}
	}
	// Duplicate, empty and relative values are usage errors (exit 2) and
	// start nothing.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--claude-adapter", "/a", "--claude-adapter", "/b"}, "may be given only once"},
		{[]string{"--codex-adapter", "/a", "--codex-adapter=/b"}, "may be given only once"},
		{[]string{"--claude-adapter", ""}, "--claude-adapter must be an absolute path to the claude executable"},
		{[]string{"--codex-adapter="}, "--codex-adapter must be an absolute path to the codex executable"},
		{[]string{"--claude-adapter", "claude"}, "--claude-adapter must be an absolute path"},
		{[]string{"--codex-adapter", "./bin/codex"}, "--codex-adapter must be an absolute path"},
		{[]string{"--claude-adapter"}, "flag needs an argument"},
		{[]string{"--grok-adapter", "/a", "--grok-adapter", "/a"}, "may be given only once"},
		{[]string{"--cursor-adapter=/a", "--cursor-adapter", "/b"}, "may be given only once"},
		{[]string{"--grok-adapter", ""}, "--grok-adapter must be an absolute path to the grok executable"},
		{[]string{"--cursor-adapter="}, "--cursor-adapter must be an absolute path to the cursor executable"},
		{[]string{"--grok-adapter", "grok"}, "--grok-adapter must be an absolute path"},
		{[]string{"--cursor-adapter", "cursor-agent"}, "--cursor-adapter must be an absolute path"},
		{[]string{"--cursor-adapter", "agent", "--claude-adapter", "/c"}, "--cursor-adapter must be an absolute path"},
		{[]string{"--grok-adapter", "./bin/grok", "--fake-adapter", "/f"}, "--grok-adapter must be an absolute path"},
		{[]string{"--cursor-adapter"}, "flag needs an argument"},
	} {
		code, errOut := run(c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, "Usage:") || len(got) != 0 {
			t.Fatalf("%v = %d %q (%d runs)", c.args, code, errOut, len(got))
		}
	}
	// Help names every flag, the qualified pairs, the macOS limit and
	// (iteration 11) Grok's argv/32 KiB/dontAsk limits and Cursor's refusal.
	_, help, _ := exec(t, "linux", "sidecar", "run", "--help")
	for _, w := range []string{"[--claude-adapter PATH] [--codex-adapter PATH] [--grok-adapter PATH] [--cursor-adapter PATH] [--fake-adapter PATH]",
		"version 2.1.285 (Claude Code)", "model sonnet, effort low", "codex-cli 0.159.0", "gpt-6.1-sol, effort low", "no PATH lookup", "give it on every start",
		"qualified on Linux only", "test/demo adapter; never calls a model",
		"--grok-adapter PATH", "version grok 1.0.46\n                     (2765805b9442) [stable], model grok-4.7, effort low; Linux only)",
		"(at most 32 KiB) is passed in argv", "visible to process inspection", "dontAsk cancelled every measured", "On macOS\n                     grok roles are refused",
		"--cursor-adapter PATH", "(cursor-agent; known: version 2026.10.01-e373342", "for version probing only", "are refused on every OS",
		"no qualified unattended recipe\n                     preserves the operator posture"} {
		if !strings.Contains(help, w) {
			t.Fatalf("sidecar run help lacks %q:\n%s", w, help)
		}
	}
	for _, leaf := range []string{"add", "set"} {
		_, h, _ := exec(t, "linux", "role", leaf, "--help")
		for _, w := range []string{"claude (Claude Code), codex (Codex CLI), grok (Grok\n                     Build; Linux workers only), cursor (Cursor Agent",
			"or\n                     fake (test/demo adapter; never calls a model)", "its roles are refused on every OS",
			"claude model", "sonnet, effort low; codex model gpt-6.1-sol, effort low", "grok and cursor: model grok-4.7, effort low",
			"(claude, codex, grok, cursor:\n                     low; fake: low, medium, high)"} {
			if !strings.Contains(h, w) {
				t.Fatalf("role %s help lacks %q:\n%s", leaf, w, h)
			}
		}
	}
	// Role diagnostics name every registered adapter and the vendors'
	// single effort, before any trust or network.
	add := func(adapterID, effort string) []string {
		return []string{"role", "add", "w", "--name", "coder", "--node", "n_0123456789abcdef0123456789abcdef", "--adapter", adapterID,
			"--instruction", "/i.md", "--runbook", "/r.md", "--model", "sonnet", "--effort", effort, "--concurrency", "1", "--plane", "https://127.0.0.1:1"}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{add("nosuch-adapter", "low"), "unknown adapter; registered adapters: claude, codex, cursor, fake, grok"},
		{add("claude", "medium"), "effort is not allowed for adapter claude; allowed: low"},
		{add("codex", "high"), "effort is not allowed for adapter codex; allowed: low"},
		{add("grok", "medium"), "effort is not allowed for adapter grok; allowed: low"},
		{add("cursor", "xhigh"), "effort is not allowed for adapter cursor; allowed: low"},
		{[]string{"role", "set", "w", "--adapter", "unknown-vendor", "--plane", "https://127.0.0.1:1"}, "unknown adapter; registered adapters: claude, codex, cursor, fake, grok"},
	} {
		code, _, errOut := exec(t, "linux", c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Fatalf("%v = %d %q", c.args, code, errOut)
		}
	}
}
