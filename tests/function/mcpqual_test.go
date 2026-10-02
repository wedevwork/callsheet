package function

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/mcp"
	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Iteration 07b function tests: one top-level test per FP (FP-9..FP-16),
// in FP order. They run the real developer executable cmd/mcpqual with
// fake vendor processes (testdata/fakevendor, by absolute path, with an
// isolated HOME and a PATH of launch-recording traps), and the
// setup and cleanup-failure tests also start the real callsheet mcp
// against a real test plane through pipes. No test invokes an installed
// vendor CLI or a model, and nothing a fake produces becomes VERIFIED.

var productionTools = []string{"node_ls", "node_show", "role_add", "role_set", "role_ls", "role_show", "role_rm", "dispatch", "task_ls", "task_show", "task_logs", "task_cancel", "task_wait",
	"ws_create", "ws_ls", "ws_show", "ws_rm", "ws_prune", "ws_ref_set", "ws_status", "ws_diff", "ws_push", "ws_pull"}

// FP-9: the documented setup of each client drives the real Callsheet.
func TestMCPSetup(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	docB, err := os.ReadFile(filepath.Join(root, mcpqual.SetupDocPath))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(docB)
	secs, err := mcpqual.ParseSetupDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	entries := repoFacts(t, root)
	p := startMCPPlane(t, 0)
	bin := nodeBinary(t)
	handshake := func(t *testing.T, argv []string) {
		t.Helper()
		if !slices.Equal(argv, []string{bin, "mcp", "--plane", p.url, "--ca", p.ca}) {
			t.Fatalf("documented Callsheet argv %q", argv)
		}
		m := startMCPProc(t, argv[2:]...)
		m.dispatch()
		m.initialize("setup-test", "1.0")
		r := m.request("tools/list", map[string]any{})
		var list struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(r.Result, &list); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range list.Tools {
			names = append(names, tl.Name)
			var schema map[string]any
			if json.Unmarshal(tl.InputSchema, &schema) != nil || schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Fatalf("%s schema %s", tl.Name, tl.InputSchema)
			}
		}
		if !slices.Equal(names, productionTools) {
			t.Fatalf("tools %v", names)
		}
		m.stdin.Close()
		m.requireExit(0)
	}
	factRows := func(t *testing.T, id string) {
		t.Helper()
		e := entries[id]
		for _, key := range []string{"mcp_config", "mcp_timeout", "mcp_timeout_override", "mcp_progress_extension", "runbook"} {
			row, jf := secs[id].Facts[key], e.Facts[key]
			if row.Status != jf.Status || len(row.Evidence) == 0 {
				t.Fatalf("%s.%s row %+v, JSON %s", id, key, row, jf.Status)
			}
			for _, ev := range row.Evidence {
				st, err := os.Stat(filepath.Join(root, ev))
				if !slices.Contains(jf.Evidence, ev) || err != nil || st.Size() == 0 {
					t.Fatalf("%s.%s evidence %s (JSON %v, %v)", id, key, ev, jf.Evidence, err)
				}
			}
		}
		// The vendor registration is not promoted by this Callsheet-side test.
		if e.Facts["mcp_config"].Status != catalog.Unverified {
			t.Fatalf("%s mcp_config promoted", id)
		}
	}
	for _, id := range []string{"claude", "codex", "grok"} {
		t.Run(id, func(t *testing.T) {
			s := secs[id]
			argv, err := mcpqual.CalleeArgv(s.Register, bin, p.url, p.ca)
			if err != nil || s.Register[0] != id || !strings.Contains(s.Text, "Registration command syntax is VERIFIED") {
				t.Fatalf("%s registration %q: %v", id, s.Register, err)
			}
			handshake(t, argv)
			factRows(t, id)
		})
	}
	t.Run("cursor", func(t *testing.T) {
		s := secs["cursor"]
		var c struct {
			MCPServers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(s.Candidate), &c); err != nil {
			t.Fatal(err)
		}
		entry := c.MCPServers["callsheet"]
		argv, err := mcpqual.CalleeArgv(append([]string{"--", entry.Command}, entry.Args...), bin, p.url, p.ca)
		if err != nil {
			t.Fatal(err)
		}
		handshake(t, argv)
		if !strings.Contains(s.Text, "exact entry shape is UNVERIFIED") || !strings.Contains(s.Text, "(UNVERIFIED until an actual registration plus tools/list evidence)") {
			t.Fatal("the cursor entry-shape candidate is not kept UNVERIFIED")
		}
		factRows(t, "cursor")
	})
	t.Run("runbook-ownership", func(t *testing.T) {
		vendorExe := map[string]string{"claude": "claude", "codex": "codex", "grok": "grok", "cursor": "cursor-agent"}
		for id, s := range secs {
			if s.Runbook[0] != vendorExe[id] || s.Runbook[len(s.Runbook)-1] != "$(cat -- '<runbook>')" {
				t.Fatalf("%s launcher %q", id, s.Runbook)
			}
			callee := strings.Join(s.Register, " ") + s.Candidate
			if strings.Contains(callee, "runbook") || strings.Contains(callee, "--rules") || strings.Contains(callee, "prompt") {
				t.Fatalf("%s: a runbook reaches the Callsheet argv: %q", id, callee)
			}
		}
		for _, s := range []string{mcp.BudgetDeferralNote, mcp.UnverifiedNotice, "never Callsheet reading it",
			"Initial-prompt loading is the established fallback for Codex and Cursor", "not a claim about automatic `AGENTS.md` precedence",
			"Shell substitution strips trailing newlines and is subject to argument-length limits", "no runbook bytes are sent to Callsheet"} {
			if strings.Count(doc, s) < 1 {
				t.Fatalf("the setup guide lacks %q", s)
			}
		}
	})
	t.Run("client-info", func(t *testing.T) {
		host, _ := os.Hostname()
		for _, tc := range []struct{ name, version string }{{"claude-code", "2.1.282"}, {"Claude Code", "2.1.282 (Claude Code)"}} {
			q := newQualEnv(t)
			repo := qualRepo(t)
			out := filepath.Join(q.dir, "out")
			r := q.qualify(qualPlan(q.fakeClient("claude", map[string]string{"CLIENT_NAME": tc.name, "CLIENT_VERSION": tc.version}, nil)), out, "", nil, "--publish-catalog", repo)
			if r.code != 0 {
				t.Fatalf("%q: %+v", tc.name, r)
			}
			ci := reportClient(t, readReport(t, out), "claude").ClientInfo
			want := contract.RequestedBy{Name: tc.name, Version: tc.version, Hostname: host}.Validate() == nil
			if deref(ci.Name) != tc.name || deref(ci.Version) != tc.version || ci.RequesterCompatible == nil || *ci.RequesterCompatible != want {
				t.Fatalf("%q: client info %+v", tc.name, ci)
			}
			f := repoFacts(t, repo)["claude"].Facts["mcp_config"]
			nameJSON, _ := json.Marshal(tc.name)
			if f.Status != catalog.Unverified || f.VerificationIteration != "08" || !strings.Contains(f.Value, "clientInfo name "+string(nameJSON)) ||
				!strings.Contains(f.Value, "(requested_by compatible: "+map[bool]string{true: "true", false: "false"}[want]) ||
				!slices.Contains(f.Evidence, "tests/testdata/mcp-qualification/"+readReport(t, out).RunID+"/report.json") {
				t.Fatalf("%q: mcp_config %+v", tc.name, f)
			}
			q.groupsGone()
		}
	})
}

// FP-10: the real probe executable over pipes.
func TestMCPQualificationProbe(t *testing.T) {
	cf := mcpqual.CaseFile{Version: 1, RunID: "fn", Nonce: "nonce-fn", Cases: []mcpqual.ProbeCase{
		// prog completes only after three progress notifications (its
		// min_progress gate), and hold cannot complete during the test, so
		// neither subtest depends on a scheduling window.
		{CaseID: "imm"}, {CaseID: "slow", DelayMS: 100}, {CaseID: "prog", DelayMS: 50, ProgressIntervalMS: 10, MinProgress: 3},
		{CaseID: "hold", DelayMS: mcpqual.MaxDelayMS}}}
	call := func(id, caseID, meta string) string {
		if meta != "" {
			meta = `,"_meta":` + meta
		}
		return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"slow","arguments":{"case_id":"` + caseID + `"}` + meta + `}}`
	}
	eventFor := func(evs []mcpqual.ProbeEvent, kind, caseID string) *mcpqual.ProbeEvent {
		for i := range evs {
			if evs[i].Kind == kind && evs[i].CaseID == caseID {
				return &evs[i]
			}
		}
		return nil
	}
	t.Run("immediate", func(t *testing.T) {
		p := startProbeProc(t, cf)
		p.ready()
		p.send(call(`"i-1"`, "imm", ""))
		if l := p.next(); l != `{"jsonrpc":"2.0","id":"i-1","result":{"content":[{"text":"{\"case_id\":\"imm\",\"nonce\":\"nonce-fn\"}","type":"text"}],"isError":false}}`+"\n" {
			t.Fatalf("result %s", l)
		}
		_, evs := p.finish()
		if eventFor(evs, mcpqual.EvCompleted, "imm") == nil || *evs[1].ClientName != "probe-test" {
			t.Fatalf("events %+v", evs)
		}
	})
	t.Run("slow", func(t *testing.T) {
		p := startProbeProc(t, cf)
		p.ready()
		p.send(call(`7`, "slow", ""))
		if l := p.next(); !strings.HasPrefix(l, `{"jsonrpc":"2.0","id":7,"result"`) {
			t.Fatalf("result %s", l)
		}
		_, evs := p.finish()
		rc, done := eventFor(evs, mcpqual.EvReceipt, "slow"), eventFor(evs, mcpqual.EvCompleted, "slow")
		if rc == nil || done == nil || done.OffsetNS-rc.OffsetNS < 100_000_000 || *rc.TokenPresent {
			t.Fatalf("events %+v", evs)
		}
	})
	t.Run("progress", func(t *testing.T) {
		p := startProbeProc(t, cf)
		p.ready()
		p.send(call(`8`, "prog", `{"progressToken":"tok-8"}`))
		n := 0
		for {
			l := p.next()
			if strings.HasPrefix(l, `{"jsonrpc":"2.0","id":8,"result"`) {
				break
			}
			n++
			var note struct {
				Method string `json:"method"`
				Params struct {
					Token    json.RawMessage `json:"progressToken"`
					Progress int             `json:"progress"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(l), &note) != nil || note.Method != "notifications/progress" || string(note.Params.Token) != `"tok-8"` || note.Params.Progress != n {
				t.Fatalf("progress %d: %s", n, l)
			}
		}
		rest, evs := p.finish()
		if n < 3 || len(rest) != 0 || eventFor(evs, mcpqual.EvCompleted, "prog").Progress != n {
			t.Fatalf("%d progress notifications, then %q", n, rest)
		}
	})
	t.Run("no-token", func(t *testing.T) {
		p := startProbeProc(t, cf)
		p.ready()
		p.send(call(`9`, "prog", ""))
		if l := p.next(); !strings.HasPrefix(l, `{"jsonrpc":"2.0","id":9,"result"`) {
			t.Fatalf("progress without a token: %s", l)
		}
		_, evs := p.finish()
		if rc := eventFor(evs, mcpqual.EvReceipt, "prog"); rc == nil || *rc.TokenPresent || eventFor(evs, mcpqual.EvProgress, "prog") != nil {
			t.Fatalf("events %+v", evs)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		p := startProbeProc(t, cf)
		p.ready()
		p.send(call(`10`, "hold", `{"progressToken":1}`))
		p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":10,"reason":"test"}}`)
		p.send(call(`11`, "imm", ""))
		if l := p.next(); !strings.HasPrefix(l, `{"jsonrpc":"2.0","id":11,"result"`) {
			t.Fatalf("after cancellation: %s", l)
		}
		rest, evs := p.finish()
		if len(rest) != 0 || eventFor(evs, mcpqual.EvCancelled, "hold") == nil || eventFor(evs, mcpqual.EvCompleted, "hold") != nil {
			t.Fatalf("late output %q, events %+v", rest, evs)
		}
	})
}

// FP-11: plans and reports are checked strictly before launching or
// publishing.
func TestMCPQualificationSchema(t *testing.T) {
	q := newQualEnv(t)
	refused := func(t *testing.T, plan any, want string, args ...string) {
		t.Helper()
		r := q.qualify(plan, filepath.Join(t.TempDir(), "out"), "", nil, args...)
		if r.code != 2 || !strings.Contains(r.stderr, want) {
			t.Fatalf("want exit 2 with %q, got %+v", want, r)
		}
		if _, err := os.Stat(q.launches); err == nil {
			t.Fatal("an invalid plan launched a process")
		}
	}
	t.Run("plan", func(t *testing.T) {
		example, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "internal", "mcpqual", "testdata", "plans", "claude.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ex any
		json.Unmarshal(example, &ex)
		refused(t, ex, "unfilled owner placeholder")
		plan := qualPlan(q.fakeClient("claude", nil, nil))
		plan["shell"] = "rm -rf /"
		refused(t, plan, "unknown field")
		refused(t, qualPlan(q.fakeClient("claude", nil, nil), q.fakeClient("claude", nil, nil)), "duplicate client id")
		refused(t, map[string]any{"version": 2, "clients": []any{}}, "version 2")
	})
	t.Run("report", func(t *testing.T) {
		repo := qualRepo(t)
		out := filepath.Join(t.TempDir(), "out")
		if r := q.qualify(qualPlan(q.fakeClient("claude", nil, nil)), out, "", nil, "--publish-catalog", repo); r.code != 0 {
			t.Fatalf("qualify = %+v", r)
		}
		rep := readReport(t, out)
		var man mcpqual.Manifest
		mb, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
		if err := json.Unmarshal(mb, &man); err != nil || man.RunID != rep.RunID || len(man.Files) != len(rep.Evidence)+2 {
			t.Fatalf("manifest %+v %v", man, err)
		}
		rb, _ := os.ReadFile(filepath.Join(out, "report.json"))
		sum := sha256.Sum256(rb)
		if man.Files[0].Path != "report.json" || man.Files[0].SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("manifest does not hash report.json: %+v", man.Files[0])
		}
		// A report no longer matching its patch is never published again.
		os.WriteFile(filepath.Join(out, "report.json"), bytes.Replace(rb, []byte(`"outcome": "conclusive"`), []byte(`"outcome": "partial"`), 1), 0o644)
		r := runBin(t, qualBinary(t), q.dir, q.env(""), "publish", "--out", out, "--repo", qualRepo(t))
		if r.code != 4 {
			t.Fatalf("tampered report publish = %+v", r)
		}
		os.Remove(q.launches)
	})
	t.Run("limits", func(t *testing.T) {
		plan := qualPlan(q.fakeClient("claude", nil, nil))
		plan["limits"] = map[string]any{"max_sessions_per_client": 13, "max_case_ms": 1000, "max_client_ms": 1000}
		refused(t, plan, "need explicit_increase")
		plan["limits"] = map[string]any{"max_sessions_per_client": 1, "max_case_ms": 0, "max_client_ms": 1000}
		refused(t, plan, "max_case_ms 0")
		big := qualPlan(q.fakeClient("claude", nil, nil))
		big["clients"].([]map[string]any)[0]["expected_version"] = strings.Repeat("v", mcpqual.MaxPlanBytes)
		refused(t, big, "exceeds")
		// Output: four cases whose 1,500,000-character tool-call IDs keep
		// every evidence file within its limit but sum past report.json's.
		// The run still writes a bounded report ParseReport accepts, with
		// the reduced cases inconclusive and their transcripts referenced.
		out := filepath.Join(t.TempDir(), "out")
		ql := newQualEnv(t) // its own launch log: the invalid plans here launch nothing
		r := ql.qualify(qualPlan(ql.fakeClient("claude", map[string]string{"TOOL_ID_REPEAT": "1500000"},
			map[string]any{"default": map[string]any{"delays_ms": []int{10, 200}}})), out, "", nil)
		st, err := os.Stat(filepath.Join(out, "report.json"))
		if err != nil {
			t.Fatal(err)
		}
		if r.code != 5 || st.Size() > mcpqual.MaxEvidenceFileBytes {
			t.Fatalf("oversize summaries: exit %d, report.json %d bytes (limit %d): %s", r.code, st.Size(), mcpqual.MaxEvidenceFileBytes, r.stdout)
		}
		rep := readReport(t, out)
		for _, ev := range rep.Evidence {
			if ev.Bytes > mcpqual.MaxEvidenceFileBytes {
				t.Fatalf("%s is %d bytes", ev.Path, ev.Bytes)
			}
		}
		bounded := 0
		for _, ph := range reportClient(t, rep, "claude").Phases {
			for _, cs := range ph.Cases {
				if strings.HasPrefix(deref(cs.Reason), mcpqual.ReasonReportBound) && cs.Outcome == mcpqual.OutcomeInconclusive && cs.VendorEvents != nil && ph.Status != mcpqual.StatusConclusive {
					bounded++
				}
			}
		}
		if bounded != 4 || rep.Outcome != "partial" {
			t.Fatalf("%d of 4 cases bounded; report %s", bounded, rep.Outcome)
		}
	})
	t.Run("paths", func(t *testing.T) {
		c := q.fakeClient("claude", nil, nil)
		c["config"].(map[string]any)["default"].(map[string]any)["path"] = "../escape.json"
		refused(t, qualPlan(c), "escapes")
		c = q.fakeClient("claude", nil, map[string]any{"default": map[string]any{}, "override": map[string]any{"delay_ms": 500}})
		c["override"].(map[string]any)["evidence"] = []string{"/etc/passwd"}
		refused(t, qualPlan(c), "must be relative")
		refused(t, qualPlan(q.fakeClient("claude", nil, nil)), "absolute clean path", "--publish-catalog", "relative/repo")
		r := runBin(t, qualBinary(t), q.dir, q.env(""), "qualify", "--plan", q.writePlan(qualPlan(q.fakeClient("claude", nil, nil))), "--out", "relative/out")
		if r.code != 2 || !strings.Contains(r.stderr, "must be an absolute clean path") {
			t.Fatalf("relative out = %+v", r)
		}
	})
}

// FP-12: each vendor format decodes to typed, case-correlated outcomes
// through the real executable; versions select decoders exactly.
func TestMCPQualificationDecoders(t *testing.T) {
	q := newQualEnv(t)
	out := filepath.Join(q.dir, "out")
	var clients []map[string]any
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		clients = append(clients, q.fakeClient(id, map[string]string{"SECRET": "sk-fake-" + id + "-0123456789abcdef"}, map[string]any{"default": map[string]any{"delays_ms": []int{200}}}))
	}
	r := q.qualify(qualPlan(clients...), out, "", nil)
	if r.code != 0 {
		t.Fatalf("qualify = %+v", r)
	}
	rep := readReport(t, out)
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		t.Run(id, func(t *testing.T) {
			c := reportClient(t, rep, id)
			setup, def := reportPhase(t, c, mcpqual.PhaseSetup), reportPhase(t, c, mcpqual.PhaseDefault)
			if c.Decoder != fakeDecoders[id] || c.DecoderVersion == nil || c.DecoderVersion.Qualified || c.DecoderVersion.Version != fakeVersions[id] ||
				setup.Status != mcpqual.StatusConclusive || def.Status != mcpqual.StatusConclusive || deref(def.Result) != mcpqual.ResultTimeoutObserved {
				t.Fatalf("%s: %+v / %+v / %+v", id, c.DecoderVersion, setup, def)
			}
			kinds := func(cs mcpqual.CaseReport) string {
				var ks []string
				for _, e := range cs.Events {
					if e.CaseID != cs.CaseID {
						t.Fatalf("event %+v not correlated with %s", e, cs.CaseID)
					}
					ks = append(ks, e.Kind)
				}
				return strings.Join(ks, ",")
			}
			// The success transcript's prose mentions a timeout; only the
			// typed timeout event of the timed-out case counts.
			if k := kinds(setup.Cases[0]); k != "tool_call,tool_result" {
				t.Fatalf("setup events %s", k)
			}
			if k := kinds(def.Cases[0]); k != "tool_call,mcp_timeout" || def.Cases[0].Outcome != mcpqual.KindMCPTimeout || ms(def.Cases[0].ElapsedMS) < 50 {
				t.Fatalf("default events %s %+v", k, def.Cases[0])
			}
		})
	}
	q.groupsGone()
	t.Run("unknown-version", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(q.fakeClient("claude", map[string]string{"VERSION": "9.9.9 (Claude Code)"}, nil)), out, "", nil)
		c := reportClient(t, readReport(t, out), "claude")
		if r.code != 5 || !strings.HasPrefix(deref(c.Reason), mcpqual.ReasonDecoderVersion) || len(q.launchLog()["session"]) != 0 || len(q.launchLog()["version"]) != 1 {
			t.Fatalf("unknown version = %+v, reason %s, launches %v", r, deref(c.Reason), q.launchLog())
		}
	})
	t.Run("non-tool-error", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(q.fakeClient("codex", map[string]string{"SCENARIO": "auth"}, map[string]any{"default": map[string]any{}})), out, "", nil)
		c := reportClient(t, readReport(t, out), "codex")
		setup := reportPhase(t, c, mcpqual.PhaseSetup)
		if r.code != 5 || setup.Cases[0].Outcome != mcpqual.KindAuthError || deref(setup.Cases[0].Reason) != mcpqual.ReasonAuth ||
			reportPhase(t, c, mcpqual.PhaseDefault).Status != mcpqual.StatusNotRun || len(q.launchLog()["session"]) != 1 {
			t.Fatalf("auth = %+v %+v", r, setup)
		}
		for _, e := range setup.Cases[0].Events {
			if e.Kind == mcpqual.KindMCPTimeout {
				t.Fatal("an authentication failure became a timeout")
			}
		}
		q.groupsGone()
	})
}

// FP-13: the phases, bounds, budgets and partial outcomes.
func TestMCPQualificationMeasurements(t *testing.T) {
	q := newQualEnv(t)
	out := filepath.Join(q.dir, "out")
	full := q.fakeClient("claude", map[string]string{"RESETS": "1", "CAP_MS": "260"}, map[string]any{
		// The raised call (300 ms) must outlast the measured default
		// (about 100 ms) by a scheduling margin of about 200 ms.
		"default": map[string]any{"delays_ms": []int{10, 200}}, "override": map[string]any{"delay_ms": 300, "bound_delay_ms": 500},
		"progress": map[string]any{}, "absolute": map[string]any{"delay_ms": 400}})
	r := q.qualify(qualPlan(full), out, "", nil)
	if r.code != 0 {
		t.Fatalf("qualify = %+v", r)
	}
	c := reportClient(t, readReport(t, out), "claude")
	q.groupsGone()
	t.Run("default", func(t *testing.T) {
		ph := reportPhase(t, c, mcpqual.PhaseDefault)
		if deref(ph.Result) != mcpqual.ResultTimeoutObserved || ms(ph.LowerBoundMS) != 10 || ms(ph.UpperBoundMS) < 50 || ph.Observations != 2 || len(ph.Cases) != 3 ||
			ph.Cases[2].CaseID != "claude-default-repeat" {
			t.Fatalf("default %+v", ph)
		}
	})
	t.Run("override", func(t *testing.T) {
		ph := reportPhase(t, c, mcpqual.PhaseOverride)
		if deref(ph.Result) != mcpqual.ResultOverrideBound || ms(ph.LowerBoundMS) != 300 || ms(ph.UpperBoundMS) < 200 || ph.Cases[0].Setting != "raised" {
			t.Fatalf("override %+v", ph)
		}
	})
	t.Run("progress", func(t *testing.T) {
		ph := reportPhase(t, c, mcpqual.PhaseProgress)
		cs := ph.Cases[0]
		if deref(ph.Result) != mcpqual.ResultExtends || cs.ProgressIntervalMS != 2 || cs.ProgressTokenPresent == nil || !*cs.ProgressTokenPresent || cs.ProgressSent == 0 {
			t.Fatalf("progress %+v %+v", ph, cs)
		}
	})
	t.Run("absolute", func(t *testing.T) {
		ph := reportPhase(t, c, mcpqual.PhaseAbsolute)
		if deref(ph.Result) != mcpqual.ResultCapObserved || ms(ph.UpperBoundMS) < 130 || ms(ph.LowerBoundMS) != 200 {
			t.Fatalf("absolute %+v", ph)
		}
	})
	t.Run("lower-bound", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(q.fakeClient("claude", map[string]string{"TIMEOUT_MS": "0"}, map[string]any{"default": map[string]any{"delays_ms": []int{10, 30}}})), out, "", nil)
		ph := reportPhase(t, reportClient(t, readReport(t, out), "claude"), mcpqual.PhaseDefault)
		if r.code != 0 || deref(ph.Result) != mcpqual.ResultLowerBound || ms(ph.LowerBoundMS) != 30 || ph.UpperBoundMS != nil || ph.Observations != 2 {
			t.Fatalf("lower bound = %+v %+v", r, ph)
		}
	})
	t.Run("partial", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		absent := q.fakeClient("codex", nil, nil)
		absent["executable"] = filepath.Join(q.dir, "no-such-codex")
		r := q.qualify(qualPlan(q.fakeClient("claude", nil, nil), absent), out, "", nil)
		rep := readReport(t, out)
		if r.code != 5 || rep.Outcome != "partial" || deref(reportClient(t, rep, "codex").Reason) != mcpqual.ReasonAbsentBinary ||
			reportClient(t, rep, "claude").Outcome != mcpqual.StatusConclusive {
			t.Fatalf("partial = %+v", r)
		}
	})
	t.Run("budget", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		plan := qualPlan(q.fakeClient("claude", nil, map[string]any{"default": map[string]any{"delays_ms": []int{10, 20, 30}}}))
		plan["limits"] = map[string]any{"max_sessions_per_client": 2, "max_case_ms": 10000, "max_client_ms": 60000}
		r := q.qualify(plan, out, "", nil)
		rep := readReport(t, out)
		ph := reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseDefault)
		if r.code != 5 || ph.Status != mcpqual.StatusInconclusive || !strings.Contains(deref(ph.Reason), mcpqual.ReasonBudget) || rep.PlannedUpperBound.Sessions != 2 ||
			len(q.launchLog()["session"]) != 2 {
			t.Fatalf("budget = %+v %+v", r, ph)
		}
	})
}

// FP-14: publication of redacted evidence and matching facts.
func TestMCPQualificationPublish(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	base := repoFacts(t, root)
	q := newQualEnv(t)
	repo := qualRepo(t)
	out := filepath.Join(q.dir, "out")
	const secret = "sk-fake-secret-0123456789abcdef"
	r := q.qualify(qualPlan(q.fakeClient("claude", map[string]string{"SECRET": secret}, map[string]any{"default": map[string]any{"delays_ms": []int{10, 200}}})), out, "", nil, "--publish-catalog", repo)
	if r.code != 0 || !strings.Contains(r.stdout, "published run") {
		t.Fatalf("qualify = %+v", r)
	}
	rep := readReport(t, out)
	after := repoFacts(t, repo)
	evDir := filepath.Join(repo, mcpqual.EvidenceRoot, rep.RunID)
	t.Run("verified", func(t *testing.T) {
		// Complete fake measurements publish harness evidence only: the
		// decoder version is synthetic, so no fact becomes VERIFIED.
		if rep.Outcome != mcpqual.StatusConclusive || rep.HarnessStatus != mcpqual.HarnessVerified || rep.VendorBehavior != mcpqual.VendorMeasured {
			t.Fatalf("report %+v", rep)
		}
		count := func(es map[string]catalog.Entry) (n int) {
			for _, e := range es {
				for _, f := range e.Facts {
					if f.Status == catalog.Verified {
						n++
					}
				}
			}
			return
		}
		if count(after) != count(base) {
			t.Fatal("fake evidence was published as VERIFIED")
		}
		for _, key := range mcpqual.TimeoutKeys {
			f := after["claude"].Facts[key]
			if f.Status != catalog.Unverified || f.VerificationIteration != "07b" || !strings.Contains(f.Value, mcpqual.HarnessVerified) ||
				!strings.Contains(f.Value, "synthetic fixture") || f.Evidence[len(f.Evidence)-1] != mcpqual.EvidenceRoot+"/"+rep.RunID+"/report.json" {
				t.Fatalf("%s = %+v", key, f)
			}
		}
		md, _ := os.ReadFile(filepath.Join(repo, mcpqual.CatalogMDPath))
		if err := catalog.CheckDocLinks(filepath.Join(repo, mcpqual.CatalogMDPath)); err != nil || strings.Count(string(md), mcpqual.InterimSentence) != 1 {
			t.Fatalf("markdown: %v", err)
		}
	})
	t.Run("partial", func(t *testing.T) {
		q := newQualEnv(t)
		repo := qualRepo(t)
		out := filepath.Join(q.dir, "out")
		absent := q.fakeClient("grok", nil, nil)
		absent["executable"] = filepath.Join(q.dir, "missing-grok")
		r := q.qualify(qualPlan(q.fakeClient("claude", nil, nil), absent), out, "", nil, "--publish-catalog", repo)
		rep := readReport(t, out)
		facts := repoFacts(t, repo)
		if r.code != 5 || rep.Outcome != "partial" || facts["claude"].Facts["mcp_timeout"].Status != catalog.Unverified ||
			!strings.Contains(facts["claude"].Facts["mcp_timeout"].Value, "Established subset") {
			t.Fatalf("partial = %+v", r)
		}
		for key, f := range facts["grok"].Facts {
			if b, _ := json.Marshal(f); string(b) != string(mustMarshal(t, base["grok"].Facts[key])) {
				t.Fatalf("absent grok's %s changed", key)
			}
		}
	})
	t.Run("redaction", func(t *testing.T) {
		for _, dir := range []string{out, evDir} {
			found := false
			filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				b, _ := os.ReadFile(p)
				if bytes.Contains(b, []byte(secret)) || bytes.Contains(b, []byte(q.home)) {
					t.Errorf("%s keeps a secret or the home path", p)
				}
				found = found || bytes.Contains(b, []byte(mcpqual.Redacted))
				return nil
			})
			if !found {
				t.Errorf("nothing redacted in %s", dir)
			}
		}
	})
	t.Run("hashes", func(t *testing.T) {
		var man mcpqual.Manifest
		mb, _ := os.ReadFile(filepath.Join(evDir, "manifest.json"))
		json.Unmarshal(mb, &man)
		for _, f := range man.Files {
			b, err := os.ReadFile(filepath.Join(evDir, filepath.FromSlash(f.Path)))
			sum := sha256.Sum256(b)
			if err != nil || hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(b)) != f.Bytes {
				t.Fatalf("%s: installed bytes differ from their hash", f.Path)
			}
		}
		// A changed evidence file is never installed into another repo.
		copyOut := filepath.Join(t.TempDir(), "out")
		copyTree(t, out, copyOut)
		ev := filepath.Join(copyOut, filepath.FromSlash(rep.Evidence[0].Path))
		os.WriteFile(ev, []byte("forged\n"), 0o644)
		fresh := qualRepo(t)
		if r := runBin(t, qualBinary(t), q.dir, q.env(""), "publish", "--out", copyOut, "--repo", fresh); r.code != 4 {
			t.Fatalf("forged evidence publish = %+v", r)
		}
		if _, err := os.Stat(filepath.Join(fresh, mcpqual.EvidenceRoot)); err == nil {
			t.Fatal("forged evidence installed")
		}
	})
	t.Run("refuse-conflict", func(t *testing.T) {
		other := qualRepo(t)
		mdPath := filepath.Join(other, mcpqual.CatalogMDPath)
		md, _ := os.ReadFile(mdPath)
		edited := append(md, []byte("\nAn unrelated owner edit.\n")...)
		os.WriteFile(mdPath, edited, 0o644)
		jsonBefore, _ := os.ReadFile(filepath.Join(other, mcpqual.CatalogJSONPath))
		r := runBin(t, qualBinary(t), q.dir, q.env(""), "publish", "--out", out, "--repo", other)
		if r.code != 4 || !strings.Contains(r.stderr, "conflict") {
			t.Fatalf("conflict = %+v", r)
		}
		mdAfter, _ := os.ReadFile(mdPath)
		jsonAfter, _ := os.ReadFile(filepath.Join(other, mcpqual.CatalogJSONPath))
		if !bytes.Equal(mdAfter, edited) || !bytes.Equal(jsonAfter, jsonBefore) {
			t.Fatal("unrelated content was overwritten")
		}
		if _, err := os.Stat(filepath.Join(other, mcpqual.EvidenceRoot)); err == nil {
			t.Fatal("evidence installed despite the conflict")
		}
	})
	t.Run("worker-facts", func(t *testing.T) {
		for id, e := range base {
			for key, f := range e.Facts {
				if id == "claude" && (slices.Contains(mcpqual.TimeoutKeys, key) || key == "mcp_config") {
					continue
				}
				if string(mustMarshal(t, f)) != string(mustMarshal(t, after[id].Facts[key])) {
					t.Fatalf("%s.%s changed", id, key)
				}
			}
		}
		if after["claude"].Facts["mcp_config"].VerificationIteration != "08" {
			t.Fatal("clientInfo evidence moved mcp_config's owner")
		}
	})
	q.groupsGone()
}

func mustMarshal(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, _ := os.ReadFile(p)
		return os.WriteFile(dst, b, 0o644)
	})
}

// FP-15: every launched group is reaped and proven gone, natively.
func TestMCPQualificationReaping(t *testing.T) {
	caseCleanup := func(t *testing.T, out string) mcpqual.CaseCleanup {
		t.Helper()
		c := reportClient(t, readReport(t, out), "claude")
		cs := reportPhase(t, c, mcpqual.PhaseSetup).Cases
		if len(cs) != 1 {
			t.Fatalf("setup cases %+v", cs)
		}
		return cs[0].Cleanup
	}
	t.Run("cooperative", func(t *testing.T) {
		// The session hangs; the case watchdog ends it with TERM, which the
		// fake honours: no KILL.
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		plan := qualPlan(q.fakeClient("claude", map[string]string{"MODE": "hang"}, nil))
		plan["limits"] = map[string]any{"max_sessions_per_client": 12, "max_case_ms": 600, "max_client_ms": 60000}
		r := q.qualify(plan, out, "", nil)
		cc := caseCleanup(t, out)
		if r.code != 5 || !cc.TermSent || cc.KillSent || !cc.GroupGone || cc.Error != nil {
			t.Fatalf("cooperative = %+v %+v", r, cc)
		}
		q.groupsGone()
	})
	t.Run("resistant", func(t *testing.T) {
		// The leader ignores TERM (set before it reports ready); an
		// interrupt starts the escalation, KILL ends it after the grace.
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		fifo, ready := q.readyFIFO()
		cmd, stdout, _ := q.start(qualPlan(q.fakeClient("claude", map[string]string{"MODE": "resistant", "READY": fifo}, nil)), out)
		ready()
		cmd.Process.Signal(syscall.SIGINT)
		if err := cmd.Wait(); cmd.ProcessState.ExitCode() != 130 {
			t.Fatalf("exit %v %s", err, stdout.String())
		}
		cc := caseCleanup(t, out)
		if !cc.TermSent || !cc.KillSent || !cc.GroupGone || cc.Error != nil {
			t.Fatalf("resistant cleanup %+v", cc)
		}
		q.groupsGone()
	})
	t.Run("parent-exits-first", func(t *testing.T) {
		// The session completes and its leader exits, leaving a descendant
		// that ignores TERM in the group: the leader's Wait is not
		// completion; KILL at the grace deadline and ESRCH are.
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(q.fakeClient("claude", map[string]string{"MODE": "parent-exits-first"}, nil)), out, "", nil)
		cc := caseCleanup(t, out)
		if r.code != 0 || !cc.LeaderExitedFirst || !cc.TermSent || !cc.KillSent || !cc.GroupGone || cc.Error != nil || len(q.launchLog()["descendant"]) != 1 {
			t.Fatalf("parent-exits-first = %+v %+v", r, cc)
		}
		q.groupsGone()
	})
	t.Run("interrupt", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		fifo, ready := q.readyFIFO()
		cmd, stdout, _ := q.start(qualPlan(q.fakeClient("claude", map[string]string{"MODE": "hang", "READY": fifo}, map[string]any{"default": map[string]any{}})), out)
		ready()
		cmd.Process.Signal(syscall.SIGINT)
		cmd.Wait()
		rep := readReport(t, out)
		c := reportClient(t, rep, "claude")
		cc := caseCleanup(t, out)
		if cmd.ProcessState.ExitCode() != 130 || !rep.Interrupted || !cc.TermSent || cc.KillSent || !cc.GroupGone ||
			reportPhase(t, c, mcpqual.PhaseDefault).Status != mcpqual.StatusNotRun || !strings.Contains(stdout.String(), "partial") {
			t.Fatalf("interrupt = %d %+v %s", cmd.ProcessState.ExitCode(), cc, stdout.String())
		}
		q.groupsGone()
	})
	t.Run("cleanup-failure", func(t *testing.T) {
		// The developer fault makes every group-existence probe fail: the
		// run stops with exit 5, refuses publication, and its children are
		// independently proven gone with an unfaulted signaler.
		q := newQualEnv(t)
		repo := qualRepo(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(q.fakeClient("claude", nil, nil)), out, "", []string{mcpqual.FaultEnv + "=" + mcpqual.FaultValue}, "--publish-catalog", repo)
		rep := readReport(t, out)
		if r.code != 5 || rep.Cleanup.OK || !strings.Contains(strings.Join(rep.Cleanup.Failures, " "), "injected") ||
			!strings.Contains(r.stderr, "publication refused: cleanup failed") || len(q.launchLog()["session"]) != 0 {
			t.Fatalf("cleanup failure = %+v %+v", r, rep.Cleanup)
		}
		if _, err := os.Stat(filepath.Join(repo, mcpqual.EvidenceRoot)); err == nil {
			t.Fatal("published despite a cleanup failure")
		}
		q.groupsGone()
		// An unsupported fault value is refused before anything runs.
		if r := q.qualify(qualPlan(q.fakeClient("claude", nil, nil)), filepath.Join(q.dir, "out2"), "", []string{mcpqual.FaultEnv + "=other"}); r.code != 2 {
			t.Fatalf("unsupported fault = %+v", r)
		}
		// callsheet never consumes the seam: with the variable set it
		// serves a normal session and ends normally at EOF.
		p := startMCPPlane(t, 0)
		m := startMCPProcEnv(t, []string{mcpqual.FaultEnv + "=" + mcpqual.FaultValue}, p.trust()...)
		m.dispatch()
		m.initialize("fault-test", "1.0")
		if r := m.request("tools/list", map[string]any{}); r.Error != nil || !strings.Contains(string(r.Result), `"task_wait"`) {
			t.Fatalf("tools/list %s", r.raw)
		}
		m.stdin.Close()
		m.requireExit(0)
		for _, dir := range []string{"cmd/callsheet", "internal/cli", "internal/mcp", "internal/plane", "internal/sidecar", "internal/client"} {
			filepath.Walk(filepath.Join(testkit.MustRepoRoot(t), dir), func(p string, info os.FileInfo, err error) error {
				if err == nil && strings.HasSuffix(p, ".go") {
					if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(mcpqual.FaultEnv)) || bytes.Contains(b, []byte("internal/mcpqual")) {
						t.Errorf("%s reads the developer fault seam", p)
					}
				}
				return nil
			})
		}
	})
}

// FP-16: model sessions only with --allow-model-calls, never with CI set.
func TestMCPQualificationInvocation(t *testing.T) {
	modelClient := func(q *qualEnv) map[string]any {
		c := q.fakeClient("claude", nil, nil)
		c["driver"], c["model"] = "model", "fake-model-1"
		c["session"] = map[string]any{"argv": []string{"--config", "{config}", "--case", "{case}", "-p", "{prompt}", "--model", "fake-model-1"}}
		return c
	}
	t.Run("denied", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(modelClient(q)), out, "", nil)
		c := reportClient(t, readReport(t, out), "claude")
		if r.code != 5 || deref(c.Reason) != mcpqual.ReasonModelCallsDenied || len(q.launchLog()["session"]) != 0 || len(q.launchLog()["version"]) != 1 {
			t.Fatalf("denied = %+v, launches %v", r, q.launchLog())
		}
		q.groupsGone()
	})
	t.Run("allowed", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(modelClient(q)), out, "", nil, "--allow-model-calls")
		rep := readReport(t, out)
		if r.code != 0 || !rep.ModelCallsAllowed || rep.PlannedUpperBound.Sessions != 1 || len(q.launchLog()["session"]) != 1 {
			t.Fatalf("allowed = %+v, launches %v", r, q.launchLog())
		}
		q.groupsGone()
	})
	t.Run("model-free", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(q.fakeClient("claude", nil, nil)), out, "", nil)
		if r.code != 0 || readReport(t, out).ModelCallsAllowed || len(q.launchLog()["session"]) != 1 {
			t.Fatalf("model-free = %+v, launches %v", r, q.launchLog())
		}
		q.groupsGone()
	})
	t.Run("ci-denied", func(t *testing.T) {
		q := newQualEnv(t)
		out := filepath.Join(q.dir, "out")
		r := q.qualify(qualPlan(modelClient(q)), out, "true", nil, "--allow-model-calls")
		if r.code != 2 || !strings.Contains(r.stderr, "CI is set") {
			t.Fatalf("ci-denied = %+v", r)
		}
		if _, err := os.Stat(q.launches); err == nil {
			t.Fatal("a vendor process launched with CI set")
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("an evidence directory was created with CI set")
		}
	})
}
