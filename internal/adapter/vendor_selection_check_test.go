//go:build realadaptercheck

package adapter

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Design 12a-worker-selection's ordinary-only adapter matrices (build tag
// realadaptercheck: the tagged coverage and native streams and the
// function suite's prebuilt tagged adapter binary run them; they never
// enter a stress shard). Every expectation is an independent literal: the
// effort unions (vendor_test.go's claudeUnion and siblings), argv arrays
// and version lines are written out here, never derived from the
// production tables.

// unionOf is each real vendor's literal effort union.
var unionOf = map[string][]string{ClaudeID: claudeUnion, CodexID: codexUnion, GrokID: grokUnion, CursorID: cursorUnion}

// realIDs are the real vendors in registry order.
var realIDs = []string{ClaudeID, CodexID, CursorID, GrokID}

// wantSelectionErr requires a *SelectionError of field with exactly msg.
func wantSelectionErr(t *testing.T, err error, field, msg string) {
	t.Helper()
	var se *SelectionError
	if !errors.As(err, &se) || se.Field != field || se.Error() != msg {
		t.Fatalf("selection error %v (field %v), want %s %q", err, se, field, msg)
	}
	if strings.Contains(se.Error(), "SECRET") {
		t.Fatalf("the diagnostic reflects input: %q", se.Error())
	}
}

// TestWorkerSelectionPolicy is UT-1 (FP-1): free model text, every
// adapter's effort union, the check order, defensive descriptor copies and
// the observation table as evidence that restricts nothing.
func TestWorkerSelectionPolicy(t *testing.T) {
	t.Run("free-model", func(t *testing.T) {
		// Future, unknown, punctuated, spaced, hyphen-led and maximum-length
		// model text passes for every adapter, byte for byte.
		valid := []string{"claude-opus-6-0", "gpt-7-nova", "grok-5-preview", "grok-4.7-build", "grok-4.7-build-fast", "grok-4.7-low", "m", "opus[1m]", "model with spaces", "-leading-hyphen", "--model",
			"\"quoted\" 'single' $(no) `tick`", "unicode é 😀", " leading space", "trailing space ", "SECRET-but-valid", strings.Repeat("m", 1024),
			strings.Repeat("é", 512)}
		for _, id := range append(slices.Clone(realIDs), FakeID) {
			effort := "low"
			for _, m := range valid {
				if err := ValidateSelection(id, m, effort); err != nil {
					t.Fatalf("%s %q: %v", id, m, err)
				}
			}
			// Invalid grammar: empty, blank, control (C0, DEL, C1), invalid
			// UTF-8 and over 1024 bytes, each with a valid effort.
			for _, m := range []string{"", " ", "   ", "\t", "\n", "SECRET\n", "a\x00b", "a\x7fb", "a\u0085b", "\xff", "SECRET\xc3", strings.Repeat("m", 1025),
				strings.Repeat("é", 512) + "m"} {
				wantSelectionErr(t, ValidateSelection(id, m, effort), "model", "adapter: invalid model")
			}
		}
	})
	t.Run("effort-union", func(t *testing.T) {
		for _, id := range realIDs {
			allowed := "adapter: effort is not allowed for adapter " + id + "; allowed: " + strings.Join(unionOf[id], ", ")
			for _, e := range unionOf[id] {
				for _, m := range []string{"any-future-model", "grok-4.7-build-fast", "-x"} {
					if err := ValidateSelection(id, m, e); err != nil {
						t.Fatalf("%s %q/%q: %v", id, m, e, err)
					}
				}
			}
			// Empty, case-mismatched, padded, other vendors' and invented
			// efforts are refused with the static list only.
			for _, e := range []string{"", " ", "LOW", "Low", "High", "XHIGH", " low", "low ", "low\n", "fast", "thinking", "extra-high", "default", "auto",
				"SECRET-EFFORT", "medium,high"} {
				wantSelectionErr(t, ValidateSelection(id, "m", e), "effort", allowed)
			}
			for _, other := range []string{"none", "minimal", "max", "ultra", "xhigh"} {
				err := ValidateSelection(id, "m", other)
				if slices.Contains(unionOf[id], other) != (err == nil) {
					t.Fatalf("%s %q: %v", id, other, err)
				}
				if err != nil {
					wantSelectionErr(t, err, "effort", allowed)
				}
			}
		}
		// The fake keeps its own three efforts.
		wantSelectionErr(t, ValidateSelection(FakeID, "m", "max"), "effort", "adapter: effort is not allowed for adapter fake; allowed: low, medium, high")
	})
	t.Run("precedence", func(t *testing.T) {
		// The adapter is resolved first, then the model, then the effort;
		// the unknown-adapter text names only the registered IDs.
		for _, id := range []string{"", "Claude", "CLAUDE", "claude ", "unknown-vendor", "agent", "cursor-agent", "SECRET-ADAPTER"} {
			wantSelectionErr(t, ValidateSelection(id, "\n", "SECRET"), "adapter", "adapter: unknown adapter; registered adapters: claude, codex, cursor, fake, grok")
		}
		for _, id := range realIDs {
			wantSelectionErr(t, ValidateSelection(id, "\n", "SECRET"), "model", "adapter: invalid model")
			wantSelectionErr(t, ValidateSelection(id, "SECRET", "SECRET"), "effort",
				"adapter: effort is not allowed for adapter "+id+"; allowed: "+strings.Join(unionOf[id], ", "))
		}
	})
	t.Run("descriptor-copies", func(t *testing.T) {
		r := Builtin("")
		for _, id := range realIDs {
			a, _ := r.Lookup(id)
			d := a.Descriptor()
			if d.ID != id || d.TestOnly || !slices.Equal(d.Efforts, unionOf[id]) {
				t.Fatalf("%s descriptor %+v", id, d)
			}
			// Every call is a fresh slice: mutating one changes neither the
			// adapter, the registry, the contract lookup nor the policy.
			d.Efforts[0], d.Efforts[len(d.Efforts)-1] = "mutated", "mutated"
			d.Efforts = append(d.Efforts, "turbo")
			again := a.Descriptor()
			if &again.Efforts[0] == &d.Efforts[0] || !slices.Equal(again.Efforts, unionOf[id]) {
				t.Fatalf("%s descriptor aliases: %+v", id, again)
			}
			info, ok := ContractLookup(r)(id)
			if !ok || info.TestOnly || !slices.Equal(info.Efforts, unionOf[id]) {
				t.Fatalf("%s lookup %+v", id, info)
			}
			info.Efforts[0] = "mutated"
			if info2, _ := Lookup()(id); !slices.Equal(info2.Efforts, unionOf[id]) {
				t.Fatalf("%s lookup aliases %+v", id, info2)
			}
			if err := ValidateSelection(id, "m", "mutated"); err == nil {
				t.Fatalf("%s: a mutated descriptor widened the policy", id)
			}
			if err := ValidateSelection(id, "m", unionOf[id][0]); err != nil {
				t.Fatalf("%s: a mutated descriptor narrowed the policy: %v", id, err)
			}
		}
		ds := r.Descriptors()
		ds[0].Efforts[0] = "mutated"
		if again := r.Descriptors(); again[0].Efforts[0] != "low" {
			t.Fatalf("registry aliases %+v", again[0])
		}
		// The registry accepts every union (slug efforts, no duplicates).
		if _, err := NewRegistry(NewClaude(""), NewCodex(""), NewCursor(""), NewGrok(""), NewFake("")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("qualification-evidence", func(t *testing.T) {
		want := []Qualification{
			{ID: "claude", Version: "2.1.285 (Claude Code)", Model: "sonnet", Effort: "low"},
			{ID: "codex", Version: "codex-cli 0.159.0", Model: "gpt-6.1-sol", Effort: "low"},
			{ID: "cursor", Version: "2026.10.01-e373342", Model: "grok-4.7", Effort: "low"},
			{ID: "grok", Version: "grok 1.0.46 (2765805b9442) [stable]", Model: "grok-4.7", Effort: "low"},
		}
		qs := Qualifications()
		if !slices.Equal(qs, want) {
			t.Fatalf("observations %+v", qs)
		}
		qs[0].Model, qs[1].Effort, qs[3].Version = "mutated", "mutated", "mutated"
		if again := Qualifications(); !slices.Equal(again, want) || &again[0] == &qs[0] {
			t.Fatalf("observations aliased %+v", again)
		}
		for _, q := range want {
			if q.Pair() != "model "+q.Model+", effort "+q.Effort {
				t.Fatalf("%s pair %q", q.ID, q.Pair())
			}
			// The observed pair is one valid selection among many: another
			// model with every other union effort passes too.
			if err := ValidateSelection(q.ID, q.Model, q.Effort); err != nil {
				t.Fatalf("%s observed pair: %v", q.ID, err)
			}
			for _, e := range unionOf[q.ID] {
				if err := ValidateSelection(q.ID, q.Model+"-other", e); err != nil {
					t.Fatalf("%s %s: %v", q.ID, e, err)
				}
			}
			// The observed version is the minimum: itself is eligible.
			if r := checkWorkerVersion(q.ID, q.Version, q.Version+"\n"); r != "" {
				t.Fatalf("%s baseline %q: %s", q.ID, q.Version, r)
			}
		}
	})
}

// TestWorkerSelectionInvocation is UT-2 (FP-2): the exact argv of every
// callable adapter for every union effort with alternate model text
// (spaces, punctuation and leading hyphens one argv element), Codex's
// literal quotes, each prompt transport, Grok's fast model, and Cursor's
// zero invocation for every valid selection.
func TestWorkerSelectionInvocation(t *testing.T) {
	models := []string{"claude-opus-5-5", "gpt-6-astra", "grok-4.7-build-fast", "model with spaces", "-leading", "--effort max", "\"q\" é $(no)"}
	prompt := []byte("{\"format\":\"callsheet-task-v1\"} -- --model other\n")
	scratch := "/state/tasks/" + taskID + "/work"
	t.Run("claude", func(t *testing.T) {
		a := NewClaude("")
		for _, e := range claudeUnion {
			for _, m := range models {
				inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: m, Effort: e, Prompt: prompt, ScratchDir: "ignored"})
				want := []string{"-p", "--model", m, "--effort", e, "--permission-prompts", "none", "--output-format", "json"}
				if err != nil || !slices.Equal(inv.Argv, want) || !bytes.Equal(inv.Stdin, prompt) || &inv.Stdin[0] == &prompt[0] || inv.FinalFile != "" {
					t.Fatalf("claude %q/%q: %q %v", m, e, inv.Argv, err)
				}
			}
		}
	})
	t.Run("codex", func(t *testing.T) {
		a := NewCodex("")
		for _, e := range codexUnion {
			for _, m := range models {
				inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: m, Effort: e, Prompt: prompt, ScratchDir: scratch})
				final := scratch + "/callsheet-final.txt"
				want := []string{"-a", "never", "exec", "--model", m, "-c", "model_reasoning_effort=\x22" + e + "\x22", "--json", "--skip-git-repo-check",
					"--output-last-message", final, "-"}
				if err != nil || !slices.Equal(inv.Argv, want) || !bytes.Equal(inv.Stdin, prompt) || inv.FinalFile != final {
					t.Fatalf("codex %q/%q: %q %v", m, e, inv.Argv, err)
				}
			}
		}
	})
	t.Run("grok", func(t *testing.T) {
		a := NewGrok("")
		for _, e := range grokUnion {
			for _, m := range models {
				inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: m, Effort: e, Prompt: prompt, ScratchDir: scratch, FinalDir: scratch + "/f"})
				want := []string{"--output-format", "json", "--model", m, "--reasoning-effort", e, "--permission-mode", "dontAsk", "-p", string(prompt)}
				if err != nil || !slices.Equal(inv.Argv, want) || inv.Stdin != nil || inv.FinalFile != "" {
					t.Fatalf("grok %q/%q: %q %v", m, e, inv.Argv, err)
				}
			}
		}
		// Grok's fast variant is a model with a separate effort; the build
		// model with high effort (formerly a refused slot) is valid too.
		inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7-build-fast", Effort: "low", Prompt: []byte("p")})
		if err != nil || !slices.Equal(inv.Argv, []string{"--output-format", "json", "--model", "grok-4.7-build-fast", "--reasoning-effort", "low",
			"--permission-mode", "dontAsk", "-p", "p"}) {
			t.Fatalf("grok fast %q %v", inv.Argv, err)
		}
		inv, err = a.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7-build", Effort: "high", Prompt: []byte("p")})
		if err != nil || !slices.Equal(inv.Argv, []string{"--output-format", "json", "--model", "grok-4.7-build", "--reasoning-effort", "high",
			"--permission-mode", "dontAsk", "-p", "p"}) || inv.Stdin != nil {
			t.Fatalf("grok build high %q %v", inv.Argv, err)
		}
	})
	t.Run("cursor", func(t *testing.T) {
		// Every valid selection, the observed vendor names and newly
		// admitted efforts included, is refused before any argv: there is
		// no executable Cursor mapping.
		a := NewCursor("")
		for _, e := range cursorUnion {
			for _, m := range append(slices.Clone(models), "grok-4.7", "grok-4.7-low", "grok-4.7-xhigh", "gpt-5.6-sol-none", "auto") {
				inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: m, Effort: e, Prompt: prompt, ScratchDir: scratch})
				if err == nil || err.Error() != cursorPostureText || inv.Argv != nil || inv.Stdin != nil || inv.FinalFile != "" {
					t.Fatalf("cursor %q/%q: %+v %v", m, e, inv, err)
				}
			}
		}
	})
	t.Run("refused", func(t *testing.T) {
		// Out-of-union efforts and invalid grammar never build an argv, on
		// Cursor too (input validation precedes its posture refusal).
		for _, id := range realIDs {
			a, _ := Builtin("").Lookup(id)
			for _, in := range []TaskInput{{TaskID: taskID, Model: "m", Effort: "turbo", Prompt: prompt, ScratchDir: scratch},
				{TaskID: taskID, Model: "grok-4.7", Effort: "fast", Prompt: prompt, ScratchDir: scratch},
				{TaskID: taskID, Model: "\x00", Effort: "low", Prompt: prompt, ScratchDir: scratch},
				{TaskID: taskID, Model: "m", Effort: "", Prompt: prompt, ScratchDir: scratch}} {
				inv, err := a.Invocation(in)
				if err == nil || err.Error() == cursorPostureText || inv.Argv != nil || inv.Stdin != nil {
					t.Fatalf("%s %+v: %+v %v", id, in, inv, err)
				}
			}
		}
	})
}

// version line builders for the 256/257-byte boundary: recognizable
// version lines of exactly n bytes, padded with dot-separated 32-byte
// prerelease identifiers (each a valid identifier).
func paddedVersion(prefix, suffix string, n int) string {
	core := prefix + "9.0.0-"
	pad := n - len(core) - len(suffix)
	var ids []string
	for pad > 0 {
		k := min(32, pad)
		if pad-k == 1 {
			k-- // never leave a lone byte for the next dot and identifier
		}
		ids = append(ids, strings.Repeat("a", k))
		pad -= k + 1
	}
	line := core + strings.Join(ids, ".") + suffix
	return line[:min(len(line), n)]
}

// TestWorkerVersionPolicy is UT-3 (FP-3): the bounded parser and
// comparator for all four vendors.
func TestWorkerVersionPolicy(t *testing.T) {
	baseline := map[string]string{ClaudeID: "2.1.285 (Claude Code)", CodexID: "codex-cli 0.159.0", GrokID: "grok 1.0.46 (2765805b9442) [stable]",
		CursorID: "2026.10.01-e373342"}
	check := func(id, out string) string { return checkWorkerVersion(id, baseline[id], out) }
	older := func(id string) string { return "has an older " + id + " version; minimum " + baseline[id] }
	invalid := func(id string) string {
		return "has an invalid " + id + " version; expected an orderable version at or above " + baseline[id]
	}
	wrong := func(id string) string { return "is not the " + id + " CLI (unexpected version output)" }
	t.Run("minimum-and-newer", func(t *testing.T) {
		for id, outs := range map[string][]string{
			ClaudeID: {"2.1.285 (Claude Code)", "2.1.292 (Claude Code)", "2.1.286 (Claude Code)", "2.2.0 (Claude Code)", "3.0.0 (Claude Code)",
				"2.1.1000 (Claude Code)", "2.10.0 (Claude Code)", "2.1.285+build.7 (Claude Code)", "2.1.285+other (Claude Code)", "2.1.286-rc.1 (Claude Code)",
				"4294967295.4294967295.4294967295 (Claude Code)"},
			CodexID: {"codex-cli 0.159.0", "codex-cli 0.160.0", "codex-cli 0.159.1", "codex-cli 0.160.0-rc.1", "codex-cli 1.0.0", "codex-cli 0.159.10",
				"codex-cli 0.160.0-alpha.beta+exp.sha.5114f85"},
			GrokID: {"grok 1.0.46 (2765805b9442) [stable]", "grok 1.0.46 (0000000000ff) [beta]", "grok 1.0.46 (a) [nightly-2]", "grok 1.0.47 (2765805b9442) [stable]",
				"grok 2.0.0 (" + strings.Repeat("f", 64) + ") [" + strings.Repeat("c", 32) + "]", "grok 1.0.46+meta (2765805b9442) [stable]"},
			// Same date with another hash is equal and accepted; later dates
			// and a valid leap day pass.
			CursorID: {"2026.10.01-e373342", "2026.10.01-abcdef0", "2026.10.01-0", "2026.10.02-abcdef0", "2026.11.01-e373342", "2027.01.01-1",
				"2028.02.29-e373342", "2400.02.29-e373342", "9999.12.31-" + strings.Repeat("0", 64)},
		} {
			for _, out := range outs {
				for _, nl := range []string{"", "\n", "\r\n"} {
					if r := check(id, out+nl); r != "" {
						t.Fatalf("%s %q: %s", id, out+nl, r)
					}
				}
			}
		}
		// The bounded line at exactly 256 bytes is eligible.
		for id, line := range map[string]string{CodexID: paddedVersion("codex-cli ", "", 256), ClaudeID: paddedVersion("", " (Claude Code)", 256)} {
			if len(line) != 256 || check(id, line+"\n") != "" {
				t.Fatalf("%s 256-byte line (%d): %s", id, len(line), check(id, line))
			}
		}
	})
	t.Run("older", func(t *testing.T) {
		for id, outs := range map[string][]string{
			ClaudeID: {"2.1.284 (Claude Code)", "2.0.999 (Claude Code)", "1.99.999 (Claude Code)", "0.0.0 (Claude Code)", "2.1.285-rc.1 (Claude Code)",
				"2.1.285-0 (Claude Code)", "2.1.284+2.1.292 (Claude Code)"},
			CodexID:  {"codex-cli 0.158.99", "codex-cli 0.159.0-rc.1", "codex-cli 0.9.0", "codex-cli 0.159.0-alpha"},
			GrokID:   {"grok 1.0.45 (1111111aaaaa) [stable]", "grok 1.0.46-rc.1 (2765805b9442) [stable]", "grok 0.99.99 (2765805b9442) [stable]"},
			CursorID: {"2026.09.30-abcdef0", "2026.09.30-e373342", "2025.12.31-e373342", "2026.01.31-ffffff", "0001.01.01-0"},
		} {
			for _, out := range outs {
				if r := check(id, out+"\n"); r != older(id) {
					t.Fatalf("%s %q: %q", id, out, r)
				}
			}
		}
	})
	t.Run("malformed", func(t *testing.T) {
		// Recognizable output of the vendor that is not a complete orderable
		// line: the invalid-version diagnostic, never echoing the output.
		long := strings.Repeat("a", 65)
		for id, outs := range map[string][]string{
			ClaudeID: {"v2.1.292 (Claude Code)", "2.1 (Claude Code)", "2.1.292.1 (Claude Code)", "02.1.292 (Claude Code)", "2.01.292 (Claude Code)",
				"2.1.4294967296 (Claude Code)", "99999999999.1.1 (Claude Code)", " 2.1.292 (Claude Code)", "2.1.292  (Claude Code)", "2.1.292- (Claude Code)",
				"2.1.292-rc..1 (Claude Code)", "2.1.292-01 (Claude Code)", "2.1.292-rc.01 (Claude Code)", "2.1.292+ (Claude Code)", "2.1.292+a..b (Claude Code)",
				"2.1.292-" + long + " (Claude Code)", "2.1.292+" + long + " (Claude Code)", "2.1.292-é (Claude Code)", "2.1.292-a_b (Claude Code)",
				" (Claude Code)", "2.1.292 (Claude Code)\n\n", "2.1.292 (Claude Code)\r\n\r\n", "2.1.292 (Claude Code)\r",
				"2.1.292 (Claude Code)\nupdate available", "\x002.1.292 (Claude Code)", "2.1.292 (Claude Code)\r\r\n", "-2.1.292 (Claude Code)", "2.1.-1 (Claude Code)"},
			CodexID: {"codex-cli ", "codex-cli 0.160", "codex-cli 0.160.0 ", "codex-cli  0.160.0", "codex-cli v0.160.0", "codex-cli 0.160.0\nupdate available",
				"codex-cli 0.160.0\n\n", "codex-cli 0.160.0\x00", "codex-cli 0.160.0\x1b[0m", "codex-cli 0.160.0 (abc)", "codex-cli 0.160.0.0", "codex-cli 0.160.x",
				"codex-cli " + strings.Repeat("9", 300), "codex-cli 0.160.0-é"},
			GrokID: {"grok ", "grok 1.0.47", "grok 1.0.47 (ABC) [stable]", "grok 1.0.47 () [stable]", "grok 1.0.47 (abc) []", "grok 1.0.47 (abc) [st able]",
				"grok 1.0.47 (abc) [stable] extra", "grok 1.0.47 (abc)[stable]", "grok 1.0.47 (" + strings.Repeat("a", 65) + ") [stable]",
				"grok 1.0.47 (abc) [" + strings.Repeat("c", 33) + "]", "grok 1.0.47 (abc) [stable]\n\n", "grok 1.0.47 (abc) [st_able]", "grok 1.0.47 (xyz) [stable]",
				"grok 1.0 (abc) [stable]", "grok  1.0.47 (abc) [stable]", "grok 1.0.47 abc [stable]"},
			CursorID: {"2026.02.29-abc", "2100.02.29-abc", "0000.01.01-abc", "2026.00.10-abc", "2026.10.00-abc", "2026.04.31-abc", "2026.13.01-abc",
				"2026.10.01-", "2026.10.01-g", "2026.10.01-E373342", "2026.10.01-" + strings.Repeat("a", 65), "2026.10.01-e373342 ", "2026.10.01-e373342-x",
				"2026.10.01-e373342\n2026.10.01-e373342", "2026.10.32-abc"},
		} {
			for _, out := range outs {
				r := check(id, out)
				if r != invalid(id) {
					t.Fatalf("%s %q: %q", id, out, r)
				}
			}
		}
		// One byte over the bound: a prefix-marked line is an invalid version.
		if line := paddedVersion("codex-cli ", "", 257); len(line) != 257 || check(CodexID, line) != invalid(CodexID) {
			t.Fatalf("257-byte codex line (%d): %q", len(line), check(CodexID, line))
		}
		if line := paddedVersion("grok ", " (abc) [stable]", 257); len(line) != 257 || check(GrokID, line) != invalid(GrokID) {
			t.Fatalf("257-byte grok line (%d): %q", len(line), check(GrokID, line))
		}
	})
	t.Run("wrong-vendor", func(t *testing.T) {
		claudeV, codexV, grokV, cursorV := baseline[ClaudeID], baseline[CodexID], baseline[GrokID], baseline[CursorID]
		for id, outs := range map[string][]string{
			ClaudeID: {"", "\n", "\r\n", codexV, grokV, cursorV, "Welcome!\n" + claudeV, "claude 2.1.292", "Claude Code 2.1.292", "2.1.292 (claude code)",
				"2.1.292 (Claude Code) ", "2.1.292 (Claude Code)x", "\n" + claudeV, claudeV + "\x00", "2.1.292\t(Claude Code)", "(Claude Code)",
				// A first line over the bound shows no marker within it.
				paddedVersion("", " (Claude Code)", 257), strings.Repeat("x", 300) + " (Claude Code)"},
			CodexID: {"", claudeV, grokV, cursorV, "codex 0.160.0", "codex-cli0.160.0", "Codex-cli 0.160.0", "codex-cli\t0.160.0", " codex-cli 0.160.0",
				"update available\n" + codexV, strings.Repeat("x", 4096)},
			GrokID: {"", "grok", "grok\n", "Grok 1.0.46 (2765805b9442) [stable]", "grok-cli 1.0.46", claudeV, codexV, cursorV, "\tgrok 1.0.47 (a) [b]"},
			CursorID: {"", "cursor-agent " + cursorV, "26.10.01-e373342", "2026.1.01-e373342", "2026/10/01-e373342", "2026.10.01", "2026.10.01\r-e373342",
				"a026.10.01-e373342", claudeV, codexV, grokV},
		} {
			for _, out := range outs {
				r := check(id, out)
				if r != wrong(id) {
					t.Fatalf("%s %q: %q", id, out, r)
				}
			}
		}
	})
	t.Run("baseline", func(t *testing.T) {
		// Every configured baseline parses; an invalid or unknown baseline
		// refuses every output with the fixed configuration text.
		for _, q := range Qualifications() {
			if _, ok := parseWorkerVersion(q.ID, q.Version); !ok || baseline[q.ID] != q.Version {
				t.Fatalf("%s baseline %q does not parse", q.ID, q.Version)
			}
		}
		for id, bad := range map[string]string{ClaudeID: "2.1.285", CodexID: "0.159.0", GrokID: "grok 1.0.46", CursorID: "2026.02.30-e373342"} {
			for _, out := range []string{baseline[id], "2.1.292 (Claude Code)", "garbage", ""} {
				if r := checkWorkerVersion(id, bad, out); r != "has no valid configured "+id+" minimum version" {
					t.Fatalf("%s bad baseline %q with %q: %q", id, bad, out, r)
				}
			}
		}
		if r := checkWorkerVersion(FakeID, "1.0.0", "1.0.0"); r != "has no valid configured fake minimum version" {
			t.Fatalf("unknown vendor baseline: %q", r)
		}
	})
	t.Run("ordering", func(t *testing.T) {
		// SemVer 2.0.0's precedence example, then numeric identifiers beyond
		// any integer width (length, then digits), numeric below
		// alphanumeric, core components numerically, build ignored.
		chain := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
			"1.0.1-0", "1.0.1-9", "1.0.1-10", "1.0.1-99999999999999999999", "1.0.1-100000000000000000000", "1.0.1-A", "1.0.1-a", "1.0.1-a.0", "1.0.1",
			"1.9.0", "1.10.0", "2.0.0", "4294967295.0.0"}
		parsed := make([]workerVersion, len(chain))
		for i, s := range chain {
			v, ok := parseSemver(s)
			if !ok {
				t.Fatalf("%q does not parse", s)
			}
			parsed[i] = v
		}
		for i := range parsed {
			for j := range parsed {
				want := map[bool]int{true: -1, false: 1}[i < j]
				if i == j {
					want = 0
				}
				if got := compareWorkerVersions(parsed[i], parsed[j]); got != want {
					t.Fatalf("compare(%q, %q) = %d, want %d", chain[i], chain[j], got, want)
				}
			}
		}
		a, _ := parseSemver("1.2.3+build.1")
		b, _ := parseSemver("1.2.3+other")
		if compareWorkerVersions(a, b) != 0 {
			t.Fatal("build metadata ordered")
		}
		d1, _ := parseWorkerVersion(CursorID, "2026.10.01-aaaa")
		d2, _ := parseWorkerVersion(CursorID, "2026.10.01-ffff")
		d3, _ := parseWorkerVersion(CursorID, "2026.09.30-ffff")
		if compareWorkerVersions(d1, d2) != 0 || compareWorkerVersions(d3, d1) != -1 || compareWorkerVersions(d1, d3) != 1 {
			t.Fatal("cursor date ordering")
		}
	})
}
