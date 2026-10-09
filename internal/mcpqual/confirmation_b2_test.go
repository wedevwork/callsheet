package mcpqual

import (
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// Design decoder-enrollment B2, UT-21 (FP-21): the confirmation plan's
// fixture identity for the three enrolled versions, the unchanged short
// schedule and margin boundary, no typed outcome or retroactive promotion
// from the real branches, and the documented delivery order: the
// mid-implementation BLOCKED handback, independent verification, explicit
// owner authorization, final acceptance then code review, the version and
// executable-hash gate, the Cursor follow-up prerequisite and opt-in
// publication. Offline only; no live execution.

func TestRealConfirmationPlanIdentity(t *testing.T) {
	reg := DefaultRegistry()
	for _, tc := range []struct{ id, decoder, version string }{
		{"codex", "codex-jsonl", CodexRealVersion}, {"grok", "grok-json", GrokRealVersion}, {"claude", "claude-json", ClaudeRealVersion},
	} {
		raw := string(readPlanFile(t, tc.id+"-short.json"))
		filled := strings.NewReplacer("<exact-cli-version>", tc.version, "<model>", "owner-model", "<absolute-path-to-"+tc.id+">", "/opt/vendor/"+tc.id).Replace(raw)
		fixture := EnrolledFixtureID(tc.decoder, tc.version, "linux/amd64")
		filled = strings.Replace(filled, `"`+tc.decoder+`/synthetic"`, `"`+fixture+`"`, 1)
		p, err := ParsePlan([]byte(filled), reg)
		if err != nil {
			t.Fatalf("%s: the completed short template with the enrolled fixture: %v", tc.id, err)
		}
		pc := p.Clients[0]
		_, dv, err := reg.Select(pc.Decoder, pc.ExpectedVersion)
		if err != nil || dv.Fixture != pc.DecoderFixture || !dv.Qualified {
			t.Fatalf("%s: the plan's fixture %q is not the enrolled %q (%v)", tc.id, pc.DecoderFixture, dv.Fixture, err)
		}
		// The schedule is unchanged: setup, one silent 15 s call and its
		// repeat, at most three sessions, 120 s per case and 360 s per client.
		lim := p.EffectiveLimits()
		if pc.Phases.Default == nil || len(pc.Phases.Default.DelaysMS) != 1 || pc.Phases.Default.DelaysMS[0] != 15000 || lim.MaxSessionsPerClient != 3 ||
			lim.MaxCaseMS != 120000 || lim.MaxClientMS != 360000 {
			t.Fatalf("%s schedule %+v %+v", tc.id, pc.Phases, lim)
		}
	}
	// The margin boundary at the short plan's L = 15 s with B = 10 s.
	if !ShortPollCompatible(10*time.Second, 15*time.Second) || ShortPollCompatible(10*time.Second, 12*time.Second) ||
		ShortPollMargin(15*time.Second) != 2*time.Second || ShortPollCompatible(10*time.Second, 11999*time.Millisecond) {
		t.Fatal("the unchanged bar 10s + max(2s, 0.1L) < L")
	}
}

// The real branches never produce a typed timeout or error outcome, so an
// unseen failure can never become a qualified fact, and a version's
// evidence names only the success capabilities: nothing retroactively
// upgrades an earlier report.
func TestRealConfirmationConservative(t *testing.T) {
	for _, tc := range []struct {
		dec   func(Transcript) Decoded
		lines []string
	}{
		{decodeCodexReal, swap(cxSuccess(), 5, strings.Replace(cxDone, `"status":"completed"`, `"status":"failed"`, 1))},
		{decodeGrokReal, swap(gkSuccess(), 4, strings.Replace(gkResult, `"OkayOutput"`, `"TimeoutOutput"`, 1))},
		{decodeClaudeReal, clDoc(swap(clSuccess(), 5, strings.Replace(clResult, `"type":"tool_result",`, `"type":"tool_result","is_error":true,`, 1)))},
	} {
		d := tc.dec(lines(1, tc.lines...))
		for _, ev := range d.Events {
			if ev.Kind != KindToolCall && ev.Kind != KindToolResult {
				t.Fatalf("a typed %s outcome", ev.Kind)
			}
		}
		if d.Inconclusive == "" {
			t.Fatalf("an unseen failure was conclusive: %+v", d)
		}
	}
	for _, name := range []string{"claude-json", "codex-jsonl", "grok-json"} {
		for _, v := range DefaultRegistry().Versions(name) {
			if v.Qualified && len(v.MissingCapabilities("linux/amd64", RequiredCapabilities(KindMCPTimeout)...)) == 0 {
				t.Fatalf("%s %s qualifies a timeout", name, v.Version)
			}
		}
	}
}

// The guide's delivery order and gates (FP-21), as the operator reads them.
func TestRealConfirmationRunbook(t *testing.T) {
	doc := readDoc(t, testkit.MustRepoRoot(t), SetupDocPath)
	order := []string{"`STATUS: BLOCKED` with the literal reason `awaiting independent fixture verification and explicit owner approval`",
		"only a discrepancy-free report lets it insert the Reviewer handle `decoder-enrollment-coordinator`",
		"only that explicit authorization, recorded in `owner-authorization.md`, lets it insert the Owner handle `callsheet-owner`",
		"runs the full offline acceptance pass on the final bytes and reports `IMPL_COMPLETE`", "then the explicitly selected codex-reviewer reviews the code",
		"Owner Cursor scoped-permission recapture", "Cursor follow-up", "Owner short-confirmation gate", "Optional publication gate"}
	at := -1
	for _, w := range order {
		i := strings.Index(doc, w)
		if i <= at {
			t.Fatalf("%q is missing or out of order", w)
		}
		at = i
	}
	for _, w := range []string{"stop on any mismatch (fresh reviewed evidence, never an edited `expected_version`)", "approval does not transfer from the capture workspace",
		"Silence, a pending answer or the earlier design approval is never authorization", "A review fix that changes approved evidence re-enters gate 2 first",
		"four under B3: the three B2 clients and Cursor", "initial invocation", "exits 4", "Cursor blocker"} {
		if !strings.Contains(doc, w) {
			t.Fatalf("the guide lacks %q", w)
		}
	}
}
