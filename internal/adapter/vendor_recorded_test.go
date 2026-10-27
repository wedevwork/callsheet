package adapter

import "testing"

// TestCheckRecordedVersion is design 12a-3-cursor-worker slice 3b's
// offline application of the minimum-version policy to recorded lines.
func TestCheckRecordedVersion(t *testing.T) {
	for _, c := range []struct {
		id, line string
		ok       bool
	}{
		{CursorID, "2026.10.14-0f9edde", true},
		{CursorID, "2026.10.01-e373342", true},
		{CursorID, "2026.10.01-aaaaaaa", true},
		{CursorID, "2020.01.01-aaaa", false},
		{CursorID, "2026.09.30-e373342", false},
		{CursorID, "grok 1.0.52 (0920c8169e27) [stable]", false},
		{ClaudeID, "2.1.299 (Claude Code)", true},
		{ClaudeID, "2.1.290 (Claude Code)", false},
		{CodexID, "codex-cli 0.162.1", true},
		{GrokID, "grok 1.0.52 (0920c8169e27) [stable]", true},
		{GrokID, "grok 1.0.47 (abcdef0) [stable]", false},
		{"fake", "1.0.0", false},
		{CursorID, "", false},
	} {
		if err := CheckRecordedVersion(c.id, c.line); (err == nil) != c.ok {
			t.Errorf("%s %q: %v", c.id, c.line, err)
		}
	}
}
