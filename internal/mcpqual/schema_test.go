package mcpqual

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// FP-11 unit tests: strict version-1 plans and reports, bounds,
// duplicates, required and nullable fields, path escapes and size limits.

func TestPlanValidation(t *testing.T) {
	reg := DefaultRegistry()
	if _, err := ParsePlan(planJSON(t, fullPlan()), reg); err != nil {
		t.Fatalf("the full plan is invalid: %v", err)
	}
	cases := map[string]struct {
		f    func(*Plan)
		want string
	}{
		"version":             {func(p *Plan) { p.Version = 2 }, "version 2"},
		"no clients":          {func(p *Plan) { p.Clients = nil }, "0 clients"},
		"five clients":        {func(p *Plan) { p.Clients = append(p.Clients, p.Clients[0], p.Clients[0], p.Clients[0], p.Clients[0]) }, "5 clients"},
		"duplicate client":    {func(p *Plan) { p.Clients = append(p.Clients, p.Clients[0]) }, "duplicate client id"},
		"unknown client":      {func(p *Plan) { p.Clients[0].ID = "aider" }, "unknown id"},
		"sessions zero":       {func(p *Plan) { p.Limits.MaxSessionsPerClient = 0 }, "max_sessions_per_client 0"},
		"sessions over":       {func(p *Plan) { p.Limits.MaxSessionsPerClient = 13 }, "need explicit_increase"},
		"sessions hard":       {func(p *Plan) { p.Limits.MaxSessionsPerClient, p.Limits.ExplicitIncrease = 65, true }, "outside 1-64"},
		"case zero":           {func(p *Plan) { p.Limits.MaxCaseMS = 0 }, "max_case_ms 0"},
		"client below case":   {func(p *Plan) { p.Limits.MaxClientMS = 5 }, "max_client_ms 5"},
		"relative exe":        {func(p *Plan) { p.Clients[0].Executable = "claude" }, "absolute clean path"},
		"unclean exe":         {func(p *Plan) { p.Clients[0].Executable = "/opt/../claude" }, "absolute clean path"},
		"no version":          {func(p *Plan) { p.Clients[0].ExpectedVersion = " " }, "expected_version"},
		"no version argv":     {func(p *Plan) { p.Clients[0].VersionArgv = nil }, "version_argv"},
		"model without":       {func(p *Plan) { p.Clients[0].Driver = DriverModel }, "explicit model"},
		"model not in argv":   {func(p *Plan) { p.Clients[0].Driver, p.Clients[0].Model = DriverModel, "m-1" }, "must appear in session.argv"},
		"direct with model":   {func(p *Plan) { p.Clients[0].Model = "m-1" }, "takes no model"},
		"driver":              {func(p *Plan) { p.Clients[0].Driver = "shell" }, `driver "shell"`},
		"decoder":             {func(p *Plan) { p.Clients[0].Decoder = "codex-jsonl" }, "want claude-json"},
		"fixture":             {func(p *Plan) { p.Clients[0].DecoderFixture = "claude-json/none" }, "not a tested"},
		"argv placeholder":    {func(p *Plan) { p.Clients[0].Session.Argv = append(p.Clients[0].Session.Argv, "{home}") }, "unknown placeholder {home}"},
		"no prompt":           {func(p *Plan) { p.Clients[0].Session.Argv = []string{"--config", "{config}"} }, "{prompt} or {case}"},
		"too many args":       {func(p *Plan) { p.Clients[0].Session.Argv = make([]string, maxArgs+1) }, "arguments"},
		"long arg":            {func(p *Plan) { p.Clients[0].Session.Argv[0] = strings.Repeat("a", maxArgBytes+1) }, "over"},
		"env CI":              {func(p *Plan) { p.Clients[0].Env = map[string]string{"CI": ""} }, "CI can never be set"},
		"env name":            {func(p *Plan) { p.Clients[0].Env = map[string]string{"A-B": "x"} }, "invalid variable"},
		"env template":        {func(p *Plan) { p.Clients[0].Env = map[string]string{"KEY": "<api-key>"} }, "unfilled owner placeholder"},
		"config escape":       {func(p *Plan) { p.Clients[0].Config.Default.Path = "../mcp.json" }, "escapes"},
		"config absolute":     {func(p *Plan) { p.Clients[0].Config.Default.Path = "/etc/mcp.json" }, "relative"},
		"config backslash":    {func(p *Plan) { p.Clients[0].Config.Default.Path = `a\b.json` }, "relative"},
		"config empty":        {func(p *Plan) { p.Clients[0].Config.Default.Path = "" }, "is empty"},
		"content size":        {func(p *Plan) { p.Clients[0].Config.Default.Content = strings.Repeat("x", MaxCaseFileBytes+1) }, "exceeds"},
		"content ph":          {func(p *Plan) { p.Clients[0].Config.Default.Content = "{home}" }, "unknown placeholder {home}"},
		"content template":    {func(p *Plan) { p.Clients[0].Config.Default.Content = "<token>" }, "unfilled owner placeholder"},
		"raised argv ph":      {func(p *Plan) { p.Clients[0].Config.Raised.Argv = []string{"{events}"} }, "unknown placeholder {events}"},
		"raised escape":       {func(p *Plan) { p.Clients[0].Config.Raised.Path = "a/../../b" }, "escapes"},
		"override fields":     {func(p *Plan) { p.Clients[0].Override.Explanation = "" }, "setting, value and explanation"},
		"override value":      {func(p *Plan) { p.Clients[0].Override.Value = "<seconds>" }, "unfilled owner placeholder"},
		"override evidence":   {func(p *Plan) { p.Clients[0].Override.Evidence = []string{"../x"} }, "escapes"},
		"override no default": {func(p *Plan) { p.Clients[0].Phases.Default = nil }, "needs phases.default"},
		"override no raised":  {func(p *Plan) { p.Clients[0].Config.Raised = nil }, "needs config.raised"},
		"override delay":      {func(p *Plan) { p.Clients[0].Phases.Override.DelayMS = 20000 }, "fit max_case_ms"},
		"override bound":      {func(p *Plan) { p.Clients[0].Phases.Override.BoundDelayMS = 500 }, "bound_delay_ms must exceed"},
		"progress alone":      {func(p *Plan) { p.Clients[0].Phases = Phases{Progress: &struct{}{}} }, "phases.progress needs"},
		"absolute alone":      {func(p *Plan) { p.Clients[0].Phases.Progress = nil }, "phases.absolute needs"},
		"absolute bound":      {func(p *Plan) { p.Clients[0].Phases.Absolute.BoundDelayMS = 9000 }, "takes no bound"},
		"absolute delay":      {func(p *Plan) { p.Clients[0].Phases.Absolute.DelayMS = 0 }, "must be positive"},
		"delays order":        {func(p *Plan) { p.Clients[0].Phases.Default.DelaysMS = []int64{300, 100} }, "strictly ascending"},
		"delays duplicate":    {func(p *Plan) { p.Clients[0].Phases.Default.DelaysMS = []int64{300, 300} }, "strictly ascending"},
		"delay max":           {func(p *Plan) { p.Clients[0].Phases.Default.DelaysMS = []int64{MaxDelayMS + 1} }, "at most"},
		"model template":      {func(p *Plan) { p.Clients[0].Driver, p.Clients[0].Model = DriverModel, "<model>" }, "unfilled owner placeholder"},
		"exe template":        {func(p *Plan) { p.Clients[0].Executable = "<absolute-path-to-claude>" }, "unfilled owner placeholder"},
	}
	// The semantic cases validate the decoded struct directly (ParsePlan is
	// strict decoding, covered below, then exactly this validation).
	for name, c := range cases {
		p := fullPlan()
		c.f(p)
		err := p.validate(planCheck{reg: reg})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	p := fullPlan()
	p.Limits.MaxSessionsPerClient, p.Limits.ExplicitIncrease = 20, true
	if _, err := ParsePlan(planJSON(t, p), reg); err != nil {
		t.Fatalf("an explicit increase was refused: %v", err)
	}
	p.Limits = nil
	if l := p.EffectiveLimits(); l.MaxSessionsPerClient != 12 || l.MaxCaseMS != 15*60*1000 || l.MaxClientMS != 90*60*1000 {
		t.Fatalf("defaults %+v", l)
	}
	for name, raw := range map[string]string{
		"unknown top-level": `{"version":1,"clients":[],"shell":"rm -rf"}`,
		"duplicate key":     `{"version":1,"version":1,"clients":[]}`,
		"oversize":          `{"version":1,"x":"` + strings.Repeat("x", MaxPlanBytes) + `"}`,
		"not json":          `version: 1`,
	} {
		if _, err := ParsePlan([]byte(raw), reg); err == nil {
			t.Errorf("%s accepted", name)
		}
		if _, err := ValidateTemplate([]byte(raw), reg); err == nil && name != "oversize" {
			t.Errorf("template %s accepted", name)
		}
	}
}

// The four example plans are valid templates whose owner placeholders
// must be completed before use.
func TestExamplePlans(t *testing.T) {
	reg := DefaultRegistry()
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		b, err := os.ReadFile(filepath.Join("testdata", "plans", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := ValidateTemplate(b, reg)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		c := p.Clients[0]
		if c.ID != id || c.Driver != DriverModel || c.Decoder != clientDecoders[id] || !strings.Contains(c.Model, "<model>") {
			t.Fatalf("%s: %+v", id, c)
		}
		if _, _, err := reg.Select(c.Decoder, c.ExpectedVersion); err != nil {
			t.Fatalf("%s: the captured version has no decoder: %v", id, err)
		}
		if _, err := ParsePlan(b, reg); !errors.Is(err, ErrTemplate) {
			t.Fatalf("%s: an uncompleted example plan was accepted: %v", id, err)
		}
		// Only help-backed noninteractive entry points (design 07b).
		argv := strings.Join(c.Session.Argv, " ")
		want := map[string]string{"claude": "-p {prompt} --output-format json", "codex": "exec --json", "grok": "-p {prompt} --output-format json", "cursor": "-p {prompt} --output-format stream-json"}[id]
		if !strings.HasPrefix(argv, want) || strings.Contains(argv, "approve-mcps") || strings.Contains(argv, "dangerously") || strings.Contains(argv, "--yolo") {
			t.Fatalf("%s session argv %q", id, argv)
		}
	}
}

// mutateReport rewrites a valid report's JSON and returns ParseReport's
// error.
func mutateReport(t *testing.T, raw []byte, f func(map[string]any)) error {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	_, err := ParseReport(mustJSON(t, m))
	return err
}

func TestReportValidation(t *testing.T) {
	out, _ := cachedRun(t, "report-validation", fullModel(), planWith(defaultOnly(900)), false, nil)
	raw, _ := os.ReadFile(filepath.Join(out, "report.json"))
	client := func(m map[string]any) map[string]any { return m["clients"].([]any)[0].(map[string]any) }
	phase0 := func(m map[string]any, i int) map[string]any { return client(m)["phases"].([]any)[i].(map[string]any) }
	case0 := func(m map[string]any) map[string]any { return phase0(m, 1)["cases"].([]any)[0].(map[string]any) }
	// Encoding-level rules (strict members, explicit nulls) need the JSON.
	jsonCases := map[string]struct {
		f    func(map[string]any)
		want string
	}{
		"unknown field":   {func(m map[string]any) { m["extra"] = 1 }, "unknown field"},
		"missing top":     {func(m map[string]any) { delete(m, "plan_sha256") }, "missing field plan_sha256"},
		"missing client":  {func(m map[string]any) { delete(client(m), "reason") }, "missing field reason"},
		"missing info":    {func(m map[string]any) { delete(client(m)["client_info"].(map[string]any), "unqualified_reason") }, "missing field unqualified_reason"},
		"missing phase":   {func(m map[string]any) { delete(phase0(m, 0), "upper_bound_ms") }, "missing field upper_bound_ms"},
		"missing case":    {func(m map[string]any) { delete(case0(m), "elapsed_ms") }, "missing field elapsed_ms"},
		"missing cleanup": {func(m map[string]any) { delete(case0(m)["cleanup"].(map[string]any), "error") }, "missing field error"},
	}
	for name, c := range jsonCases {
		if err := mutateReport(t, raw, c.f); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	// Semantic rules: Validate on the decoded report (ParseReport's last
	// step).
	cases := map[string]struct {
		f    func(*Report)
		want string
	}{
		"schema":            {func(r *Report) { r.Schema = 2 }, "schema 2"},
		"run id":            {func(r *Report) { r.RunID = "a b" }, "run_id"},
		"harness status":    {func(r *Report) { r.HarnessStatus = "vendor verified" }, "harness_status"},
		"vendor behavior":   {func(r *Report) { r.VendorBehavior = "VERIFIED" }, "vendor_behavior"},
		"outcome":           {func(r *Report) { r.Outcome = "maybe" }, "outcome"},
		"no clients":        {func(r *Report) { r.Clients = nil }, "0 clients"},
		"evidence escape":   {func(r *Report) { r.Evidence[0].Path = "../x" }, "escapes"},
		"evidence dup":      {func(r *Report) { r.Evidence = append(r.Evidence, r.Evidence[0]) }, "duplicate evidence"},
		"evidence hash":     {func(r *Report) { r.Evidence[0].SHA256 = "x" }, "bad hash"},
		"evidence unlisted": {func(r *Report) { r.Evidence = nil }, "is not listed"},
		"client dup":        {func(r *Report) { r.Clients = append(r.Clients, r.Clients[0]) }, "unknown or duplicate"},
		"version null":      {func(r *Report) { r.Clients[0].ObservedVersion, r.Clients[0].Reason = nil, nil }, "observed_version null without a reason"},
		"partial reason":    {func(r *Report) { r.Clients[0].Outcome, r.Clients[0].Reason = "partial", nil }, "without a reason"},
		"info null": {func(r *Report) {
			r.Clients[0].ClientInfo.Name, r.Clients[0].ClientInfo.UnqualifiedReason = nil, nil
		}, "unqualified_reason"},
		"phase reason": {func(r *Report) {
			r.Clients[0].Phases[1].Status, r.Clients[0].Phases[1].Reason = StatusInconclusive, nil
		}, "without a reason"},
		"phase result": {func(r *Report) { r.Clients[0].Phases[0].Result = nil }, "conclusive without a result"},
		"case dup": {func(r *Report) {
			p := &r.Clients[0].Phases[1]
			p.Cases = append(p.Cases, p.Cases[0])
		}, "invalid or duplicate"},
		"elapsed null": {func(r *Report) {
			c := &r.Clients[0].Phases[1].Cases[0]
			c.ElapsedMS, c.Reason = nil, nil
		}, "elapsed_ms null without a reason"},
	}
	for name, c := range cases {
		var rep Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		c.f(&rep)
		if err := rep.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	if _, err := ParseReport(append([]byte(" "), make([]byte, MaxEvidenceFileBytes)...)); err == nil {
		t.Fatal("an oversized report was accepted")
	}
	rep, err := ParseReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	tampered := copyRun(t, out)
	os.WriteFile(filepath.Join(tampered, filepath.FromSlash(rep.Evidence[0].Path)), []byte("changed"), 0o644)
	if err := rep.CheckEvidence(tampered); err == nil {
		t.Fatal("changed evidence passed its hash")
	}
	os.Remove(filepath.Join(tampered, filepath.FromSlash(rep.Evidence[0].Path)))
	if err := rep.CheckEvidence(tampered); err == nil {
		t.Fatal("missing evidence passed")
	}
	if !strings.Contains(bracketText(nil, iptr(5)), "<= 5 ms") || bracketText(nil, nil) != "" || reasonSuffix(sptr("")) != "" {
		t.Fatal("report text helpers")
	}
	_ = testkit.MustRepoRoot
}
