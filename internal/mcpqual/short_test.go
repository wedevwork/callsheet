package mcpqual

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/mcp"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Design nonblocking-coordinator-waits, UT-7 (FP-7: the short-poll
// catalog policy check) and UT-8 (FP-8: the four short plan templates,
// their scheduling and the short-poll decision's wording).

func readPlanFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "plans", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestShortPlanTemplates: each {vendor}-short.json validates as a template
// (and is refused unfilled), copies its full template's isolated default
// configuration and argv shape, leaves the exact CLI version as an owner
// input (<exact-cli-version>: the version the owner's CLI reports, never
// the catalog's, review W1) that an otherwise filled plan still refuses,
// requests only setup and one explicit 15s silent call, has no raised
// configuration, override or long phase, and is bounded to three sessions,
// 120s per case and 6 minutes per client.
func TestShortPlanTemplates(t *testing.T) {
	entries, err := catalog.Load(filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, e := range entries {
		versions[e.ID] = e.Version
	}
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		raw := readPlanFile(t, id+"-short.json")
		p, err := ValidateTemplate(raw, DefaultRegistry())
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if _, err := ParsePlan(raw, DefaultRegistry()); !errors.Is(err, ErrTemplate) {
			t.Fatalf("%s: an unfilled short plan parsed: %v", id, err)
		}
		full, err := ValidateTemplate(readPlanFile(t, id+".json"), DefaultRegistry())
		if err != nil {
			t.Fatal(err)
		}
		c, f := p.Clients[0], full.Clients[0]
		// Design decoder-enrollment B1 amends the Codex, Grok and Cursor
		// short recipes only (their full templates stay unchanged): the
		// canonical argv, Codex's approval snapshot and every prerequisite.
		switch b1 := b1ShortRecipes()[id]; {
		case b1 == nil:
		case !slices.Equal(c.Session.Argv, b1.Session.Argv) || !reflect.DeepEqual(c.Config.Default, b1.Config.Default) || c.Config.Default.Path != f.Config.Default.Path:
			t.Fatalf("%s: the B1 recipe %q %+v", id, c.Session.Argv, c.Config.Default)
		default:
			f.Session, f.Config.Default = b1.Session, b1.Config.Default
		}
		switch {
		case len(p.Clients) != 1 || c.ID != id || p.Limits == nil || *p.Limits != (Limits{MaxSessionsPerClient: 3, MaxCaseMS: 120000, MaxClientMS: 360000}):
			t.Fatalf("%s: limits %+v", id, p.Limits)
		case c.Phases.Default == nil || !slices.Equal(c.Phases.Default.DelaysMS, []int64{15000}) || c.Phases.Override != nil || c.Phases.Progress != nil || c.Phases.Absolute != nil:
			t.Fatalf("%s: phases %+v", id, c.Phases)
		case c.Config.Raised != nil || c.Override != nil:
			t.Fatalf("%s: a raised setting or override", id)
		case c.ExpectedVersion != "<exact-cli-version>" || strings.Contains(string(raw), versions[id]):
			t.Fatalf("%s: version %q is not the owner input (catalog %q)", id, c.ExpectedVersion, versions[id])
		case c.Executable != f.Executable || c.Model != f.Model || c.Driver != DriverModel || c.Decoder != f.Decoder || c.DecoderFixture != f.DecoderFixture ||
			!slices.Equal(c.VersionArgv, f.VersionArgv) || !slices.Equal(c.HelpArgv, f.HelpArgv) || !slices.Equal(c.Session.Argv, f.Session.Argv) ||
			!reflect.DeepEqual(c.Config.Default, f.Config.Default) || !reflect.DeepEqual(c.Env, f.Env):
			t.Fatalf("%s: the default recipe differs from the full template", id)
		case !strings.Contains(string(raw), `"delays_ms": [`):
			t.Fatalf("%s: the 15s delay is not explicit", id)
		}
		// Filled but for the version: still an unfilled owner input.
		filled := strings.ReplaceAll(strings.ReplaceAll(string(raw), "<model>", "m1"), f.Executable, "/opt/vendor/"+id)
		if _, err := ParsePlan([]byte(filled), DefaultRegistry()); !errors.Is(err, ErrTemplate) || !strings.Contains(err.Error(), "expected_version") {
			t.Fatalf("%s: a plan without its exact version parsed: %v", id, err)
		}
		if _, err := ParsePlan([]byte(strings.ReplaceAll(filled, "<exact-cli-version>", "9.9.9")), DefaultRegistry()); err != nil {
			t.Fatalf("%s: a filled short plan: %v", id, err)
		}
		r := &Runner{Plan: p, AllowModelCalls: true}
		if b := r.plannedBound(); b != (PlannedBound{Sessions: 3, WallMS: 360000}) {
			t.Fatalf("%s: planned %+v", id, b)
		}
	}
	if ShortPollBudget != mcp.DefaultBudget {
		t.Fatal("the short confirmation checks another budget than the shipping B")
	}
}

// b1ShortRecipes are the B1 short recipes, literally (design
// decoder-enrollment B1, slice-b1.md Codex, Grok and Cursor recipes), with
// the template's model placeholder.
func b1ShortRecipes() map[string]*PlanClient {
	const m = "<model>"
	toml := "[mcp_servers.probe]\ncommand = \"{server}\"\nargs = [\"serve\", \"--case-file\", \"{case_file}\", \"--events\", \"{events}\"]\n"
	return map[string]*PlanClient{
		"codex": {Session: Recipe{Argv: []string{"exec", "--json", "--skip-git-repo-check", "-C", "{workspace}", "-m", m,
			"-c", `mcp_servers.probe.command="{server}"`,
			"-c", `mcp_servers.probe.args=["serve","--case-file","{workspace}/case.json","--events","{workspace}/server-events.jsonl"]`,
			"-c", `mcp_servers.probe.enabled_tools=["slow"]`,
			"-c", `mcp_servers.probe.tools.slow.approval_mode="approve"`,
			"{prompt}"}},
			Config: ConfigRecipes{Default: ConfigRecipe{Path: "probe-config.toml",
				Content:      toml + "enabled_tools = [\"slow\"]\n[mcp_servers.probe.tools.slow]\napproval_mode = \"approve\"\n",
				Prerequisite: "Invocation-only approval for probe.slow; no server-wide approval or timeout override."}}},
		"grok": {Session: Recipe{Argv: []string{"--trust", "-p", "{prompt}", "--output-format", "streaming-json", "--cwd", "{workspace}", "-m", m}},
			Config: ConfigRecipes{Default: ConfigRecipe{Path: ".grok/config.toml", Content: toml,
				Prerequisite: "Trust only the generated workspace; use normal Grok login; project probe config, probe__slow and streaming-json; no timeout override."}}},
		"cursor": {Session: Recipe{Argv: []string{"-p", "{prompt}", "--output-format", "stream-json", "--model", m, "--workspace", "{workspace}", "--trust"}},
			Config: ConfigRecipes{Default: ConfigRecipe{Path: ".cursor/mcp.json",
				Content:      `{"mcpServers":{"probe":{"command":"{server}","args":["serve","--case-file","{case_file}","--events","{events}"]}}}`,
				Prerequisite: "Trust only the generated workspace; capture runs mcp enable probe there and verifies workspace-only file changes before the model session; never --approve-mcps."}}},
	}
}

// shortFake is a short plan run on the fake world: the template filled
// with the fake executable and the fake vendor's version, decoded by the
// synthetic fixture or (qualified) by a decoder qualified from an actual
// transcript.
func shortFake(t *testing.T, m vendorModel, qualified bool) (*Report, *fakeLauncher, string) {
	t.Helper()
	p, err := ValidateTemplate(readPlanFile(t, "claude-short.json"), DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	f := fullPlan().Clients[0]
	c := &p.Clients[0]
	c.Executable, c.ExpectedVersion, c.Driver, c.Model, c.Session, c.Config.Default = f.Executable, f.ExpectedVersion, DriverDirect, "", f.Session, f.Config.Default
	w := newWorld(t, m)
	r := newRunner(t, w, p)
	if qualified {
		c.DecoderFixture = "claude-json/actual-test"
		r.Registry = qualifiedRegistry()
	}
	rep := runPlan(t, r, context.Background())
	md, _ := os.ReadFile(filepath.Join(r.OutDir, "report.md"))
	return rep, w, string(md)
}

// TestShortPlanScheduling: setup, one silent 15s call and a repeat of
// that successful bound (three sessions, a lower bound); a first call
// that times out schedules no repeat (two sessions). The report states a
// vendor compatibility verdict only from qualified evidence (review C2):
// with the synthetic fixture both runs are UNVERIFIED and show their
// numbers as measurement only.
func TestShortPlanScheduling(t *testing.T) {
	const lower = "L = 15s is the longest silent call that completed (a lower bound, not the timeout): 10s + max(2s, 1.5s) = 12s < 15s."
	const synthetic = "vendor compatibility UNVERIFIED (decoder claude-json/synthetic is a synthetic fixture, not an actual-transcript qualification): measurement only, "
	for _, qualified := range []bool{false, true} {
		rep, w, md := shortFake(t, vendorModel{timeoutMS: 20000}, qualified)
		ph := phaseOf(rep.Clients[0], PhaseDefault)
		if len(w.sessions()) != 3 || ph == nil || *ph.Result != ResultLowerBound || ms(ph.LowerBoundMS) != 15000 || ph.Observations != 2 ||
			ph.Cases[1].CaseID != "claude-default-repeat" || ph.Cases[1].DelayMS != 15000 || len(rep.Clients[0].Phases) != 2 {
			t.Fatalf("lower bound: %d sessions %+v", len(w.sessions()), ph)
		}
		want := synthetic + lower
		if qualified {
			want = "compatible for this measured tuple: " + lower
		}
		if !strings.Contains(md, want) || (!qualified && strings.Contains(md, "compatible for this measured tuple")) {
			t.Fatalf("qualified %v report.md:\n%s", qualified, md)
		}
		rep, w, md = shortFake(t, vendorModel{timeoutMS: 9000}, qualified)
		ph = phaseOf(rep.Clients[0], PhaseDefault)
		if len(w.sessions()) != 2 || *ph.Result != ResultTimeoutObserved || ph.LowerBoundMS != nil || ph.Observations != 1 {
			t.Fatalf("timeout: %d sessions %+v", len(w.sessions()), ph)
		}
		want = synthetic + "the client timed out at or below"
		if qualified {
			want = "not compatible: the client timed out at or below"
		}
		if !strings.Contains(md, want) || strings.Contains(md, "compatible for this measured tuple") || (!qualified && strings.Contains(md, "not compatible")) {
			t.Fatalf("qualified %v report.md:\n%s", qualified, md)
		}
	}
}

func i64(v int64) *int64 { return &v }

// decisionOf renders the decision for default phase ph of a qualified,
// clean client, after mutate.
func decisionOf(ph *PhaseReport, mutate func(*Report, *ClientReport)) string {
	ok := StatusConclusive
	healthy := ResultHealthy
	c := ClientReport{ID: "claude", DecoderVersion: &DecoderVersion{Version: "v", Fixture: "claude-json/actual-test", Qualified: true,
		Evidence: []DecoderEvidence{testEvidence("linux/amd64")}},
		Phases: []PhaseReport{{Name: PhaseSetup, Status: ok, Result: &healthy, Observations: 1}}}
	if ph != nil {
		p := *ph
		p.Name = PhaseDefault
		c.Phases = append(c.Phases, p)
	}
	rep := &Report{OS: "linux", Arch: "amd64", Cleanup: CleanupReport{OK: true}}
	if mutate != nil {
		mutate(rep, &c)
	}
	return ShortPollDecision(ShortPollBudget, rep, c)
}

// TestShortPollDecision: the conservative inequality at and around 12s
// and 15s, lower-bound versus observed-timeout wording and the
// unestablished cases on qualified evidence; and (review C2) every gap in
// the evidence (no or a synthetic decoder, no conclusive setup, no repeat,
// an interrupted or uncleaned run) leaves vendor compatibility UNVERIFIED
// with the numbers shown as measurement only.
func TestShortPollDecision(t *testing.T) {
	b := ShortPollBudget
	for _, c := range []struct {
		l    time.Duration
		want bool
	}{{15 * time.Second, true}, {12 * time.Second, false}, {12*time.Second + time.Millisecond, true}, {11 * time.Second, false}, {20 * time.Second, true}, {30 * time.Second, true}} {
		if ShortPollCompatible(b, c.l) != c.want {
			t.Fatalf("L=%v: %v", c.l, !c.want)
		}
	}
	if ShortPollMargin(15*time.Second) != 2*time.Second || ShortPollMargin(40*time.Second) != 4*time.Second {
		t.Fatal("margin")
	}
	lb, to, ok := ResultLowerBound, ResultTimeoutObserved, StatusConclusive
	for name, c := range map[string]struct {
		ph   *PhaseReport
		want []string
	}{
		"lower-bound": {&PhaseReport{Status: ok, Result: &lb, LowerBoundMS: i64(15000), Observations: 2},
			[]string{"compatible for this measured tuple", "(a lower bound, not the timeout): 10s + max(2s, 1.5s) = 12s < 15s."}},
		"lower-bound-and-timeout": {&PhaseReport{Status: ok, Result: &to, LowerBoundMS: i64(15000), UpperBoundMS: i64(60010), Observations: 2},
			[]string{"compatible for this measured tuple", "A typed timeout was observed at or below 1m0.01s; it is reported, never used as the safe value."}},
		"short-lower-bound": {&PhaseReport{Status: ok, Result: &lb, LowerBoundMS: i64(12000), Observations: 2},
			[]string{"not established: the lower bound L = 12s is too short: 10s + max(2s, 1.2s) = 12s >= 12s"}},
		"timeout-at-12s": {&PhaseReport{Status: ok, Result: &to, UpperBoundMS: i64(12000), Observations: 1},
			[]string{"not compatible: the client timed out at or below 12s, and even that bound fails: 10s + max(2s, 1.2s) = 12s >= 12s."}},
		"timeout-under-12s-after-5s": {&PhaseReport{Status: ok, Result: &to, LowerBoundMS: i64(5000), UpperBoundMS: i64(11000), Observations: 2},
			[]string{"not compatible: the client timed out at or below 11s"}},
		"spanning": {&PhaseReport{Status: ok, Result: &to, LowerBoundMS: i64(5000), UpperBoundMS: i64(15000), Observations: 2},
			[]string{"not established (failed or inconclusive): a typed timeout at or below 15s", "an upper bound never proves safety"}},
		"inconclusive": {&PhaseReport{Status: StatusInconclusive, Reason: func() *string { s := "repeat observation missing: budget"; return &s }()},
			[]string{"compatibility UNVERIFIED: the default phase is inconclusive (repeat observation missing: budget); a failed, interrupted or unmeasured run is never a pass."}},
		"no-bound":  {&PhaseReport{Status: ok, Result: &lb}, []string{"compatibility UNVERIFIED: the default phase measured no bound."}},
		"not-asked": {nil, []string{"compatibility UNVERIFIED: the default phase was not requested."}},
	} {
		got := decisionOf(c.ph, nil)
		if !strings.HasPrefix(got, "Short-poll decision (B=10s, B + max(2s, 0.1L) < L): ") {
			t.Fatalf("%s: %q", name, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: %q lacks %q", name, got, w)
			}
		}
	}
	compatible := &PhaseReport{Status: ok, Result: &lb, LowerBoundMS: i64(15000), Observations: 2}
	incompatible := &PhaseReport{Status: ok, Result: &to, UpperBoundMS: i64(9000), Observations: 1}
	for name, c := range map[string]struct {
		ph     *PhaseReport
		mutate func(*Report, *ClientReport)
		why    string
	}{
		"synthetic": {compatible, func(_ *Report, c *ClientReport) {
			c.DecoderVersion.Qualified, c.DecoderVersion.Fixture = false, "claude-json/synthetic"
		},
			"decoder claude-json/synthetic is a synthetic fixture, not an actual-transcript qualification"},
		"synthetic-timeout": {incompatible, func(_ *Report, c *ClientReport) {
			c.DecoderVersion.Qualified, c.DecoderVersion.Fixture = false, "claude-json/synthetic"
		},
			"decoder claude-json/synthetic is a synthetic fixture"},
		"no-decoder": {compatible, func(_ *Report, c *ClientReport) { c.DecoderVersion = nil }, "no decoder version was selected"},
		// Design decoder-enrollment: a historical qualified record without
		// evidence and evidence for another platform both stay unverified.
		"historical": {compatible, func(_ *Report, c *ClientReport) { c.DecoderVersion.Evidence = nil },
			"decoder claude-json/actual-test is marked qualified without enrolled real-transcript evidence"},
		"platform": {compatible, func(r *Report, _ *ClientReport) { r.OS, r.Arch = "darwin", "arm64" },
			"decoder claude-json/actual-test has no enrolled success-path evidence for darwin/arm64"},
		"setup":       {compatible, func(_ *Report, c *ClientReport) { c.Phases[0].Status = StatusInconclusive }, "the setup phase is not conclusive"},
		"no-setup":    {compatible, func(_ *Report, c *ClientReport) { c.Phases = c.Phases[1:] }, "the setup phase is not conclusive"},
		"no-repeat":   {&PhaseReport{Status: ok, Result: &lb, LowerBoundMS: i64(15000), Observations: 1}, nil, "the successful bound was not repeated"},
		"interrupted": {compatible, func(r *Report, _ *ClientReport) { r.Interrupted = true }, "the run was interrupted"},
		"cleanup":     {compatible, func(r *Report, _ *ClientReport) { r.Cleanup.OK = false }, "the run's cleanup failed"},
	} {
		got := decisionOf(c.ph, c.mutate)
		if !strings.Contains(got, "vendor compatibility UNVERIFIED ("+c.why) || !strings.Contains(got, "): measurement only, ") ||
			strings.Contains(got, "compatible for this measured tuple") || strings.Contains(got, "not compatible") || !strings.Contains(got, " = 12s ") {
			t.Fatalf("%s: %q", name, got)
		}
	}
}

// TestShortPollCatalogCheck (UT-7): the shipped catalog keeps the
// retired anchor's policy and no interim sentence; properly evidenced
// VERIFIED lower bounds and measured timeouts and published UNVERIFIED
// subsets pass; every other shape is refused.
func TestShortPollCatalogCheck(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	md, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(CatalogMDPath)))
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := catalog.Load(filepath.Join(root, filepath.FromSlash(CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	// The mutations below assume pre-publication managed facts: start from
	// the frozen baseline (design catalog-version, amendment A1), never
	// from whatever the checkout has published.
	baseline := catalog.PublicationBaselineFacts()
	for i := range shipped {
		for key, f := range baseline[shipped[i].ID] {
			shipped[i].Facts[key] = f
		}
	}
	// load is a fresh deep copy of the shipped entries (read once).
	load := func() []catalog.Entry { return cloneEntries(shipped) }
	doc := string(md)
	if err := CheckShortPollCatalog(doc, load()); err != nil {
		t.Fatal(err)
	}
	report := EvidenceRoot + "/run-9/report.json"
	set := func(key string, f func(*catalog.Fact)) []catalog.Entry {
		es := load()
		fact := es[0].Facts[key]
		f(&fact)
		es[0].Facts[key] = fact
		return es
	}
	verified := func(v string, ev ...string) func(*catalog.Fact) {
		return func(f *catalog.Fact) { f.Status, f.Value, f.Evidence = catalog.Verified, v, append(f.Evidence, ev...) }
	}
	for name, es := range map[string][]catalog.Entry{
		"lower-bound": set("mcp_timeout", verified("Measured by qualification run run-9 on linux/amd64: silent tool calls of up to 15000 ms completed (2 observations); this is a lower bound, not the default.", report)),
		"timeout":     set("mcp_timeout", verified("Measured by qualification run run-9 on linux/amd64: a silent tool call ended with the client's typed MCP timeout event after 60010 ms.", report)),
		"subset": set("mcp_timeout", func(f *catalog.Fact) {
			f.Value = "Established subset from qualification run run-9 on linux/amd64: the default phase observed lower_bound. Missing: a qualified decoder."
		}),
		"override": set("mcp_timeout_override", verified("Measured by qualification run run-9: setting x let a 900 ms silent call complete.", report)),
	} {
		if err := CheckShortPollCatalog(doc, es); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct {
		md string
		es []catalog.Entry
	}{
		"legacy prose":       {strings.Replace(doc, ShortPollPolicy, ShortPollPolicy+" "+LegacyInterimSentence, 1), load()},
		"no policy":          {strings.Replace(doc, ShortPollPolicy, "Short polls.", 1), load()},
		"not labelled":       {strings.Replace(doc, "**Retired: short-poll policy.** ", "", 1), load()},
		"policy twice":       {doc + "\n" + ShortPollAnchor + ShortPollPolicy + "\n", load()},
		"legacy fact":        {doc, set("runbook", func(f *catalog.Fact) { f.Value += " " + LegacyInterimSentence })},
		"no report":          {doc, set("mcp_timeout", verified("Measured by qualification run run-9: a lower bound.", "tests/testdata/cli-help/claude-help.txt"))},
		"not measured":       {doc, set("mcp_timeout", verified("The timeout is 15s.", report))},
		"bare unverified":    {doc, set("mcp_timeout", func(f *catalog.Fact) { f.Value = "MCP tool-call timeout is unknown." })},
		"owner":              {doc, set("mcp_timeout", func(f *catalog.Fact) { f.VerificationIteration = "08" })},
		"evidence":           {doc, set("mcp_progress_extension", func(f *catalog.Fact) { f.Evidence = nil })},
		"status":             {doc, set("mcp_timeout_override", func(f *catalog.Fact) { f.Status = "MAYBE" })},
		"nested report path": {doc, set("mcp_timeout", verified("Measured by qualification run run-9: x.", EvidenceRoot+"/a/b/report.json"))},
	} {
		if err := CheckShortPollCatalog(c.md, c.es); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	es := load()
	delete(es[1].Facts, "mcp_timeout")
	if err := CheckShortPollCatalog(doc, es); err == nil {
		t.Fatal("a missing timeout fact accepted")
	}
}
