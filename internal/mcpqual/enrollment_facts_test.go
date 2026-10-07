package mcpqual

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Design decoder-enrollment UT-7: short confirmation with qualified
// evidence is conservative. A qualified decoder's outcome counts only with
// its demonstrated capability on the run's own OS/arch (from the runner,
// never the runtime); a typed timeout without timeout evidence, another
// platform, or a historical qualified record without evidence is
// UNVERIFIED; the report records the evidence it was measured under, so a
// later enrollment change never rewrites it.

// successOnly is fake qualified evidence of the success path only.
func successOnly(platform string) Registry {
	return Registry{"claude-json": {versions: []DecoderVersion{{Version: "2.1.282 (Claude Code)", Fixture: "claude-json/actual-test", Qualified: true,
		Evidence: []DecoderEvidence{{Platform: platform, Fixture: "claude-json/actual-test", Kinds: []string{CapToolCall, CapToolResult, CapTerminalSuccess}}}}}, decode: decodeClaude}}
}

func TestEnrolledShortConfirmation(t *testing.T) {
	short := defaultOnly(150)
	// Success evidence on linux/amd64: the lower bound is repeated and
	// conclusive; the report records the evidence it was measured under.
	_, rep := cachedRun(t, "enrolled-success", vendorModel{}, planWith(short), false, func(r *Runner) {
		r.Plan.Clients[0].DecoderFixture, r.Registry = "claude-json/actual-test", successOnly("linux/amd64")
	})
	def := phase(t, rep, "claude", PhaseDefault)
	c := rep.Clients[0]
	if rep.Outcome != StatusConclusive || def.Status != StatusConclusive || ms(def.LowerBoundMS) != 150 || def.Observations != 2 ||
		len(c.DecoderVersion.Evidence) != 1 || c.DecoderVersion.Evidence[0].Platform != "linux/amd64" {
		t.Fatalf("lower bound: %s %v", def, c.DecoderVersion)
	}
	if d := ShortPollDecision(ShortPollBudget, rep, c); !strings.Contains(d, "not established: the lower bound L = 150ms is too short") {
		t.Fatalf("decision %q", d)
	}
	e := catalog.Entry{ID: "claude", Version: "2.1.282 (Claude Code)", Platform: "linux/amd64", Facts: map[string]catalog.Fact{}}
	facts := proposeFacts(rep, c, e, "tests/testdata/mcp-qualification/run-1/report.json")
	if f := facts["mcp_timeout"]; f.Status != catalog.Verified || !strings.Contains(f.Value, "lower bound") {
		t.Fatalf("eligible lower bound: %+v", f)
	}
	// Unused phases stay UNVERIFIED.
	for _, key := range []string{"mcp_timeout_override", "mcp_progress_extension"} {
		if facts[key].Status != catalog.Unverified {
			t.Fatalf("%s %+v", key, facts[key])
		}
	}
	// The report is self-contained: parsing it again after the registry
	// changed yields the same evidence and facts.
	b, _ := encodeIndent(rep)
	parsed, err := ParseReport(b)
	if err != nil || len(parsed.Clients[0].DecoderVersion.Evidence) != 1 {
		t.Fatalf("report round trip: %v", err)
	}
	again := proposeFacts(parsed, parsed.Clients[0], e, "tests/testdata/mcp-qualification/run-1/report.json")
	x, _ := json.Marshal(facts)
	y, _ := json.Marshal(again)
	if string(x) != string(y) {
		t.Fatal("facts depend on more than the recorded report")
	}
	// A typed timeout without timeout evidence is unverified_decoder_event,
	// never a conclusive measurement (the run once per process).
	_, rep = cachedRun(t, "enrolled-timeout", vendorModel{timeoutMS: 100}, planWith(short), false, func(r *Runner) {
		r.Plan.Clients[0].DecoderFixture = "claude-json/actual-test"
		r.Registry, r.GOOS, r.GOARCH = successOnly("linux/amd64"), "linux", "amd64"
	})
	def = phase(t, rep, "claude", PhaseDefault)
	if def.Status != StatusInconclusive || !strings.Contains(*def.Reason, ReasonUnverifiedEvent+": mcp_timeout without mcp_timeout evidence for linux/amd64") {
		t.Fatalf("unsupported timeout: %s", def)
	}
	// Evidence for another platform: even the setup success is unverified
	// (the platform is the runner's, from Env, never the runtime's).
	m := &measure{r: &Runner{GOOS: "darwin", GOARCH: "arm64"}, dv: successOnly("linux/amd64")["claude-json"].versions[0]}
	cs := CaseReport{Outcome: KindToolResult}
	m.checkCapability(&cs)
	if cs.Outcome != OutcomeInconclusive || !strings.Contains(*cs.Reason, ReasonUnverifiedEvent+": tool_result without tool_call, tool_result, terminal_success evidence for darwin/arm64") {
		t.Fatalf("platform: %+v", cs)
	}
	m.r.GOOS, m.r.GOARCH = "linux", "amd64"
	cs = CaseReport{Outcome: KindToolResult}
	if m.checkCapability(&cs); cs.Outcome != KindToolResult || cs.Reason != nil {
		t.Fatalf("evidenced success: %+v", cs)
	}
	m.dv = DecoderVersion{Version: "v", Fixture: "claude-json/synthetic"}
	cs = CaseReport{Outcome: KindAuthError}
	if m.checkCapability(&cs); cs.Outcome != KindAuthError {
		t.Fatal("synthetic classification changed")
	}
}

// Fact eligibility from a recorded report alone.
func TestEnrolledFactEligibility(t *testing.T) {
	lb, to := ResultLowerBound, ResultTimeoutObserved
	healthy := ResultHealthy
	client := func(dv *DecoderVersion, result *string, upper *int64) ClientReport {
		return ClientReport{ID: "claude", DecoderVersion: dv, Phases: []PhaseReport{{Name: PhaseSetup, Status: StatusConclusive, Result: &healthy, Observations: 1},
			{Name: PhaseDefault, Status: StatusConclusive, Result: result, LowerBoundMS: i64(15000), UpperBoundMS: upper, Observations: 2}}}
	}
	rep := &Report{RunID: "run-1", OS: "linux", Arch: "amd64", Cleanup: CleanupReport{OK: true}}
	e := catalog.Entry{ID: "claude", Version: "v", Platform: "linux/amd64", Facts: map[string]catalog.Fact{}}
	success := &DecoderVersion{Version: "v", Fixture: "f", Qualified: true, Evidence: []DecoderEvidence{{Platform: "linux/amd64", Fixture: "f", Kinds: []string{CapToolCall, CapToolResult, CapTerminalSuccess}}}}
	for name, tc := range map[string]struct {
		c    ClientReport
		rep  *Report
		want string
	}{
		"historical": {client(&DecoderVersion{Version: "v", Fixture: "f", Qualified: true}, &lb, nil), rep, "without enrolled real-transcript evidence (a historical record)"},
		"platform":   {client(success, &lb, nil), &Report{RunID: "run-1", OS: "linux", Arch: "arm64", Cleanup: CleanupReport{OK: true}}, "which does not certify linux/amd64"},
		"evidence-platform": {client(&DecoderVersion{Version: "v", Fixture: "f", Qualified: true, Evidence: []DecoderEvidence{{Platform: "darwin/arm64", Fixture: "f", Kinds: success.Evidence[0].Kinds}}}, &lb, nil),
			rep, "no enrolled tool_call, tool_result, terminal_success evidence for linux/amd64"},
		"timeout-unsupported": {client(success, &to, i64(60000)), rep, "no enrolled mcp_timeout evidence for linux/amd64"},
	} {
		f := proposeFacts(tc.rep, tc.c, e, "r")["mcp_timeout"]
		if f.Status != catalog.Unverified || !strings.Contains(f.Value, tc.want) {
			t.Fatalf("%s: %+v", name, f)
		}
		if d := ShortPollDecision(ShortPollBudget, tc.rep, tc.c); strings.Contains(d, verdictCompatible) {
			t.Fatalf("%s: decision %q", name, d)
		}
	}
	if f := proposeFacts(rep, client(success, &lb, nil), e, "r")["mcp_timeout"]; f.Status != catalog.Verified {
		t.Fatalf("eligible: %+v", f)
	}
	// An old report (no evidence member) parses; invalid evidence does not.
	rep2 := &Report{Schema: ReportSchema, RunID: "run-1", CaptureDate: "2026-10-06", OS: "linux", Arch: "amd64", HarnessVersion: "h", PlanSHA256: strings.Repeat("a", 64),
		HarnessStatus: HarnessVerified, VendorBehavior: VendorNotRun, Outcome: "partial", Clients: []ClientReport{{ID: "claude", Outcome: "unqualified", Reason: sptr("x"),
			ObservedVersion: sptr("v"), DecoderVersion: &DecoderVersion{Version: "v", Fixture: "f", Qualified: true}, Phases: []PhaseReport{},
			ClientInfo: ClientInfo{UnqualifiedReason: sptr("x")}}}, Cleanup: CleanupReport{OK: true, Failures: []string{}}, Evidence: []EvidenceRef{}}
	b, _ := encodeIndent(rep2)
	if strings.Contains(string(b), `"evidence": [
            {`) {
		t.Fatal("unexpected evidence")
	}
	if _, err := ParseReport(b); err != nil {
		t.Fatalf("old report: %v", err)
	}
	rep2.Clients[0].DecoderVersion.Evidence = []DecoderEvidence{{Platform: "linux/amd64", Fixture: "f", Kinds: []string{"everything"}}}
	b, _ = encodeIndent(rep2)
	if _, err := ParseReport(b); err == nil || !strings.Contains(err.Error(), "unknown or duplicate capability") {
		t.Fatalf("invalid evidence: %v", err)
	}
	// The plan's fixture must be the selected version's canonical fixture.
	p := planWith(Phases{})
	p.Clients[0].DecoderFixture = "claude-json/synthetic"
	p.Clients[0].ExpectedVersion = "2.1.282 (Claude Code)x"
	r := newRunner(t, newWorld(t, vendorModel{}), p)
	r.Registry = DefaultRegistry().WithVersion("claude-json", DecoderVersion{Version: "2.1.282 (Claude Code)x", Fixture: "claude-json/other"})
	w := newWorld(t, vendorModel{version: "2.1.282 (Claude Code)x"})
	r.Launcher, r.Clock = w, w.clock
	rep3 := runPlan(t, r, context.Background())
	if c := rep3.Clients[0]; !strings.HasPrefix(*c.Reason, ReasonDecoderFixture) || len(w.sessions()) != 0 {
		t.Fatalf("fixture mismatch: %v", *c.Reason)
	}
}
