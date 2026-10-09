package function

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/mcpqual/procexec"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Design decoder-enrollment B3 function tests (FP-22..FP-26): one parent
// per FP with its literal local case inventory. They use the checked-in
// sanitised fixtures, the real probe and mcpqual, the real export command
// and its test helper, and the fake vendor only (its reviewed Cursor
// shapes and its test-owned worker socket residue under the test's fixture
// HOME): no installed vendor CLI, network, authentication, raw capture
// bundle, private review package or design/ path. The Linux adapter is
// exercised on every host through the runner's injected GOOS and GOARCH,
// never a production override. Process scenarios stay outside the stress
// selectors and prove every launched group gone.

// cursorFixtureID is the enrolled Cursor fixture ID.
var cursorFixtureID = mcpqual.EnrolledFixtureID("cursor-jsonl", mcpqual.CursorRealVersion, "linux/amd64")

// b3Root is a fresh directory directly under the resolved /tmp: a short,
// link-free output root whose <out>/.work slug stays within the residue
// parent-slug bound (54 characters).
func b3Root(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp(base, "cq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// cursorQualifyPlan is the shipped short Cursor template for the pinned
// version and its enrolled fixture, run by the fake vendor in the reviewed
// real format with the B1 recipe checks, the required permission file and
// the per-project approval, with default delays (a short silent call keeps
// the real-process cases quick; FP-26 drives the 15 s schedule on an
// injected clock).
func cursorQualifyPlan(q *qualEnv, settings map[string]string, delays []int) map[string]any {
	s := map[string]string{"FORMAT": "cursor-real", "RECIPE": "cursor", "PERMISSION": "require", "ENABLE": "permission-check,project", "EXPECT_HOME": q.home}
	for k, v := range settings {
		s[k] = v
	}
	p := q.capturePlan(map[string]map[string]string{"cursor": s}, "cursor")
	c := p["clients"].([]any)[0].(map[string]any)
	c["decoder_fixture"] = cursorFixtureID
	c["env"].(map[string]string)["FAKE_VENDOR_CLIENT_NAME"] = "Cursor"
	c["phases"] = map[string]any{"default": map[string]any{"delays_ms": delays}}
	return p
}

// qualifyCursor runs plan in process (the production runner with the real
// fake-vendor and probe processes, linux/amd64 injected) into out under the
// production registry, adjusted by mutate, and proves every group gone.
func qualifyCursor(t *testing.T, q *qualEnv, plan map[string]any, out string, mutate func(r *mcpqual.Runner)) *mcpqual.Report {
	t.Helper()
	b, _ := json.Marshal(plan)
	p, err := mcpqual.ParsePlan(b, mcpqual.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	r := &mcpqual.Runner{Plan: p, PlanSHA256: sha(b), OutDir: out, AllowModelCalls: true, GOOS: "linux", GOARCH: "amd64", Hostname: "function-host",
		ServerPath: qualBinary(t), BaseEnv: q.env(""), Launcher: procexec.Launcher{},
		Reaper: mcpqual.GroupReaper{Sig: mcpqual.SysSignaler{}, Clock: mcpqual.RealClock,
			Policy: mcpqual.CleanupPolicy{Grace: mcpqual.CleanupGrace, Limit: mcpqual.CleanupLimit, Poll: mcpqual.CleanupPoll}},
		Clock: mcpqual.RealClock, Registry: mcpqual.DefaultRegistry(), Log: io.Discard, RunID: "run-cursor-b3", Nonce: "noncecursorb3", CaptureDate: "2026-10-09",
		HarnessVersion: mcpqual.HarnessVersion, Home: q.home}
	if mutate != nil {
		mutate(r)
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q.groupsGone()
	return rep
}

// allCases are a report's cases in run order.
func allCases(rep *mcpqual.Report) []mcpqual.CaseReport {
	var out []mcpqual.CaseReport
	for _, c := range rep.Clients {
		for _, ph := range c.Phases {
			out = append(out, ph.Cases...)
		}
	}
	return out
}

// launchOrder is the fake vendor's launch kinds in order.
func (q *qualEnv) launchOrder() []string {
	b, _ := os.ReadFile(q.launches)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			out = append(out, f[0])
		}
	}
	return out
}

// ownerCursorProjects is the owner's .cursor with its existing projects
// directory (an earlier project's approval), as a real Cursor home has it.
func ownerCursorProjects(t *testing.T, q *qualEnv) {
	ownerCursor(t, q)
	mkdir(t, filepath.Join(q.home, ".cursor", "projects", "home-owner-earlier-project"))
	os.WriteFile(filepath.Join(q.home, ".cursor", "projects", "home-owner-earlier-project", "mcp-approvals.json"), []byte(`["probe-aaaaaaaaaaaaaaaa"]`), 0o600)
}

// caseReasons renders each case's outcome and reasons for a failure.
func caseReasons(cases []mcpqual.CaseReport) string {
	var b strings.Builder
	for _, cs := range cases {
		b.WriteString(cs.CaseID + " " + cs.Outcome + " " + deref(cs.Reason))
		if p := cs.CursorPreparation; p != nil {
			b.WriteString(" [preparation " + p.State + " " + deref(p.Reason) + "]")
		}
		b.WriteString("; ")
	}
	return b.String()
}

// verified requires case cs's preparation to be verified with scope.
func verified(t *testing.T, cs mcpqual.CaseReport, scope string) {
	t.Helper()
	p := cs.CursorPreparation
	if cs.Outcome != mcpqual.KindToolResult || p == nil || p.State != mcpqual.PreparationVerified || p.Approval.Scope != scope || p.ToolPermission == nil ||
		p.ToolPermission.State != mcpqual.PermissionVerified || p.PermissionChecks != (mcpqual.PermissionChecks{AfterEnable: true, BeforeSession: true, AfterCleanup: true}) ||
		p.Residue.State != mcpqual.ResidueVerified {
		t.Fatalf("case %s: %s %q %+v", cs.CaseID, cs.Outcome, deref(cs.Reason), p)
	}
}

// unverified requires case cs's preparation to be unverified with a reason
// containing want and an inconclusive outcome.
func unverified(t *testing.T, cs mcpqual.CaseReport, want string) {
	t.Helper()
	p := cs.CursorPreparation
	if cs.Outcome != mcpqual.OutcomeInconclusive || p == nil || p.State != mcpqual.PreparationUnverified || !strings.Contains(deref(p.Reason), want) {
		t.Fatalf("case %s: %s %q %+v", cs.CaseID, cs.Outcome, deref(cs.Reason), p)
	}
}

// FP-22: the exact-version Cursor decoder replays the reviewed golden
// bytes through Select and Replay to the raw-authored oracle; negative
// mutations of those bytes (correlation, the rejection, prose and malformed
// records) never succeed.
func TestMCPCursorRealDecoder(t *testing.T) {
	t.Parallel()
	runInventory(t, 5, []string{"golden", "exact-dispatch", "correlation", "rejection", "prose-and-malformed"}, []captureCase{
		{"golden", func(t *testing.T) {
			e, b, o, _ := enrolledBundle(t, "cursor")
			if err := mcpqual.Replay(mcpqual.DefaultRegistry(), e, b, o); err != nil {
				t.Fatal(err)
			}
			dec, v, err := mcpqual.DefaultRegistry().Select("cursor-jsonl", mcpqual.CursorRealVersion)
			if err != nil || !v.Qualified {
				t.Fatal(err)
			}
			tr, _ := mcpqual.ReplayTranscript(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileVendorEvents)])
			d := dec(tr)
			id := "call-19919b82-c87a-49fe-9fda-1f3ffd89e029-0" + string(rune(10)) + "fc_83db03ab-9fa9-92c5-b433-8875d2d21ea7_0"
			want := []mcpqual.Event{{CaseID: "cursor-capture-setup", Kind: mcpqual.KindToolCall, OffsetNS: 7474606467, RequestID: id},
				{CaseID: "cursor-capture-setup", Kind: mcpqual.KindToolResult, OffsetNS: 7560444405, RequestID: id, Nonce: "e6085f2e06acab78e174b529053161d7"}}
			if !d.Terminal || d.Inconclusive != "" || len(d.Events) != 2 || d.Events[0] != want[0] || d.Events[1] != want[1] || len(tr.Lines) != 17 {
				t.Fatalf("golden %+v", d)
			}
			if o.ClientInfo.Name != "Cursor" || o.ClientInfo.Version != "1.0.0" || !o.RequesterCompatible || deref(o.SanitizationPolicy) != mcpqual.SanitizationPolicyB3Cursor {
				t.Fatalf("oracle %+v", o)
			}
		}},
		{"exact-dispatch", func(t *testing.T) {
			reg := mcpqual.DefaultRegistry()
			_, b, _, _ := enrolledBundle(t, "cursor")
			tr, _ := mcpqual.ReplayTranscript(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileVendorEvents)])
			// The synthetic version keeps the family parser, which does not
			// read the real shape; near versions select nothing.
			syn, v, err := reg.Select("cursor-jsonl", "2026.09.23-86fc751")
			if err != nil || v.Qualified {
				t.Fatal(err)
			}
			if d := syn(tr); d.Inconclusive == "" && len(d.Events) == 2 && d.Events[1].Nonce != "" {
				t.Fatalf("the synthetic family parser decoded the real shape: %+v", d)
			}
			for _, near := range []string{"2026.10.01", "2026.10.01-e373342 ", "v2026.10.01-e373342"} {
				if _, _, err := reg.Select("cursor-jsonl", near); err == nil {
					t.Fatalf("%q selected", near)
				}
			}
			if _, _, err := mcpqual.SyntheticRegistry().Select("cursor-jsonl", mcpqual.CursorRealVersion); err == nil {
				t.Fatal("the synthetic registry selects the real version")
			}
			w := reg.WithVersion("cursor-jsonl", mcpqual.DecoderVersion{Version: "test-only", Fixture: "cursor-jsonl/synthetic"})
			if d, _, err := w.Select("cursor-jsonl", mcpqual.CursorRealVersion); err != nil || d(tr).Inconclusive != "" {
				t.Fatalf("WithVersion lost the real override: %v", err)
			}
		}},
		{"correlation", func(t *testing.T) {
			e, b, o, _ := enrolledBundle(t, "cursor")
			raw := b.Files[mcpqual.ClientFile("cursor", mcpqual.FileVendorEvents)]
			lf := `-0\nfc_83db03ab`
			for name, edit := range map[string]func(i int, d string) string{
				"orphan-completion": func(i int, d string) string { return map[bool]string{true: "", false: d}[i == 11] },
				"no-completion":     func(i int, d string) string { return map[bool]string{true: "", false: d}[i == 12] },
				"repeated-start": func(i int, d string) string {
					if i == 12 {
						return strings.Replace(d, `"subtype":"completed"`, `"subtype":"started"`, 1)
					}
					return d
				},
				"lf-stripped": func(i int, d string) string {
					if i == 12 {
						return strings.ReplaceAll(d, lf, `-0fc_83db03ab`)
					}
					return d
				},
				"other-case": func(i int, d string) string {
					if i == 12 {
						return strings.ReplaceAll(d, "cursor-capture-setup", "cursor-other")
					}
					return d
				},
				"other-server": func(i int, d string) string {
					if i == 11 || i == 12 {
						return strings.ReplaceAll(d, `"serverIdentifier":"probe"`, `"serverIdentifier":"files"`)
					}
					return d
				},
				"terminal-first": func(i int, d string) string {
					if i == 11 {
						return `{"type":"result","subtype":"success","is_error":false}` + "\n"
					}
					return d
				},
			} {
				forged := withTranscript(b, "cursor", records(t, raw, func(i int, d string) string { return strings.TrimSuffix(edit(i, d), "\n") }))
				if err := mcpqual.Replay(mcpqual.DefaultRegistry(), e, forged, o); err == nil {
					t.Errorf("%s replayed", name)
				}
			}
		}},
		{"rejection", func(t *testing.T) {
			e, b, o, _ := enrolledBundle(t, "cursor")
			raw := b.Files[mcpqual.ClientFile("cursor", mcpqual.FileVendorEvents)]
			for _, name := range []string{"rejected", "rejected-and-success"} {
				// The completion's result as a JSON object edit: the observed
				// rejection alone, or added beside the success alternative.
				forged := records(t, raw, func(i int, d string) string {
					if i != 12 {
						return d
					}
					var m map[string]any
					if err := json.Unmarshal([]byte(d), &m); err != nil {
						t.Fatal(err)
					}
					mcp := m["tool_call"].(map[string]any)["mcpToolCall"].(map[string]any)
					res := mcp["result"].(map[string]any)
					if name == "rejected" {
						res = map[string]any{}
					}
					res["rejected"] = map[string]any{"reason": "User rejected MCP: probe-slow"}
					mcp["result"] = res
					b, _ := json.Marshal(m)
					return string(b)
				})
				dec, _, _ := mcpqual.DefaultRegistry().Select("cursor-jsonl", mcpqual.CursorRealVersion)
				tr, err := mcpqual.ReplayTranscript(forged)
				if err != nil {
					t.Fatal(err)
				}
				d := dec(tr)
				if !d.Terminal || !strings.Contains(d.Inconclusive, "rejection") || strings.Contains(d.Inconclusive, "User rejected") {
					t.Errorf("%s: %+v", name, d)
				}
				for _, ev := range d.Events {
					if ev.Kind != mcpqual.KindToolCall {
						t.Errorf("%s: a %s event", name, ev.Kind)
					}
				}
				if err := mcpqual.Replay(mcpqual.DefaultRegistry(), e, withTranscript(b, "cursor", forged), o); err == nil {
					t.Errorf("%s replayed", name)
				}
			}
		}},
		{"prose-and-malformed", func(t *testing.T) {
			e, b, o, _ := enrolledBundle(t, "cursor")
			raw := b.Files[mcpqual.ClientFile("cursor", mcpqual.FileVendorEvents)]
			dec, _, _ := mcpqual.DefaultRegistry().Select("cursor-jsonl", mcpqual.CursorRealVersion)
			prose := records(t, raw, func(i int, d string) string { return map[bool]string{true: "", false: d}[i == 11 || i == 12] })
			malformed := records(t, raw, func(i int, d string) string { return map[bool]string{true: `{"type":`, false: d}[i == 3] })
			for name, tc := range map[string]struct {
				raw       []byte
				truncated bool
				want      string
			}{
				"prose-only": {prose, false, mcpqual.ReasonUnrecognized},
				"malformed":  {malformed, false, mcpqual.ReasonMalformed},
				"truncated":  {raw, true, mcpqual.ReasonTruncated},
			} {
				tr, err := mcpqual.ReplayTranscript(tc.raw)
				if err != nil {
					t.Fatal(err)
				}
				tr.Truncated = tc.truncated
				if d := dec(tr); !strings.HasPrefix(d.Inconclusive, tc.want) {
					t.Errorf("%s: %+v", name, d)
				}
				if !tc.truncated {
					if err := mcpqual.Replay(mcpqual.DefaultRegistry(), e, withTranscript(b, "cursor", tc.raw), o); err == nil {
						t.Errorf("%s replayed", name)
					}
				}
			}
		}},
	})
}

// fabricatedCursorCapture is a real complete capture of the fake Cursor
// vendor in the reviewed real format (the pinned adapter, its permission
// file and project approval, the real probe), and the helper environment
// accepting it as a source.
func fabricatedCursorCapture(t *testing.T) (string, []string) {
	t.Helper()
	q := newQualEnv(t)
	ownerCursorProjects(t, q)
	out := filepath.Join(realTemp(t), "capture")
	m := inProcessRun(t, q, cursorPermissionPlan(q, map[string]string{"FORMAT": "cursor-real", "ENABLE": "permission-check,project", "CLIENT_NAME": "Cursor"}), out, nil)
	if m.State != mcpqual.CaptureComplete {
		t.Fatalf("fabricated Cursor capture %s %q", m.State, deref(m.Reason))
	}
	b := bundle(t, out)
	roots := filepath.Dir(out) + string(os.PathListSeparator) + testkit.MustRepoRoot(t)
	if d, err := filepath.EvalSymlinks(filepath.Dir(t.TempDir())); err == nil {
		roots = d + string(os.PathListSeparator) + roots
	}
	env := []string{"FIXTURE_EXPORT_CLIENT=cursor", "FIXTURE_EXPORT_VERSION=" + mcpqual.CursorRealVersion, "FIXTURE_EXPORT_PLATFORM=" + b.Manifest.OS + "/" + b.Manifest.Arch,
		"FIXTURE_EXPORT_RUN=" + b.Manifest.RunID, "FIXTURE_EXPORT_SHA256=" + sha(b.ManifestBytes), "FIXTURE_EXPORT_ROOTS=" + roots}
	return out, env
}

// FP-23: the Cursor export policy is deterministic and keeps every
// protected evidence byte; production validation accepts exactly the four
// enrolled fixtures, the three B2 entries byte-identical; corrupted
// provenance and empty or placeholder attestations are refused; the
// documented gate order holds.
func TestMCPCursorEnrollment(t *testing.T) {
	t.Parallel()
	runInventory(t, 5, []string{"deterministic-export", "protected-evidence", "production-inventory", "provenance-corruption", "owner-gate"}, []captureCase{
		{"deterministic-export", func(t *testing.T) {
			src, env := fabricatedCursorCapture(t)
			root := realTemp(t)
			for _, name := range []string{"a", "b"} {
				r := runBin(t, exportHelperBinary(t), root, env, "--source", src, "--out", filepath.Join(root, name))
				if r.code != 0 || !strings.Contains(r.stdout, mcpqual.SanitizationPolicyB3Cursor) {
					t.Fatalf("export %s = %+v", name, r)
				}
			}
			a, b := treeBytes(t, filepath.Join(root, "a")), treeBytes(t, filepath.Join(root, "b"))
			if len(a) != len(b) || len(a) == 0 {
				t.Fatal("two exports differ")
			}
			for p := range a {
				if !bytes.Equal(a[p], b[p]) {
					t.Fatalf("%s differs between two exports", p)
				}
			}
			san, err := mcpqual.ParseFixtureSanitization(a[mcpqual.SanitizationName])
			if err != nil || san.Policy != mcpqual.SanitizationPolicyB3Cursor || len(san.Replacements) == 0 {
				t.Fatalf("receipt %v %+v", err, san)
			}
			// The production command accepts only the compiled pins.
			if r := runBin(t, exportBinary(t), root, nil, "--source", src, "--out", filepath.Join(root, "c")); r.code != 1 || !strings.Contains(r.stderr, "not an accepted source identity") {
				t.Fatalf("production export of a fabricated source = %+v", r)
			}
		}},
		{"protected-evidence", func(t *testing.T) {
			_, b, _, sb := enrolledBundle(t, "cursor")
			san, err := mcpqual.ParseFixtureSanitization(sb)
			if err != nil || san.Policy != mcpqual.SanitizationPolicyB3Cursor || len(san.Replacements) != 49 ||
				san.SourceManifestSHA256 != "b3e57897b6242029f873a738569f6ad124e3ffe6f0c3406feeb8c716bd6a7a2f" {
				t.Fatalf("receipt %v %+v", err, san)
			}
			for _, r := range san.Replacements {
				if r.Pointer == "/apiKeySource" || r.Pointer == "/call_id" || strings.Contains(r.Pointer, "mcpToolCall") || strings.Contains(r.Pointer, "toolCallId") {
					t.Fatalf("a protected value was replaced: %+v", r)
				}
			}
			tr, _ := mcpqual.ReplayTranscript(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileVendorEvents)])
			lf := "-0" + string(rune(10)) + "fc_"
			for i, l := range tr.Lines {
				var m map[string]any
				json.Unmarshal(l.Data, &m)
				switch i {
				case 0:
					if m["apiKeySource"] != "[REDACTED]" || m["cwd"] != mcpqual.FixtureMetadataValue {
						t.Fatalf("init %v", m)
					}
				case 11, 12:
					tc := m["tool_call"].(map[string]any)
					args := tc["mcpToolCall"].(map[string]any)["args"].(map[string]any)
					for _, id := range []any{m["call_id"], tc["toolCallId"], args["toolCallId"]} {
						if s, _ := id.(string); !strings.Contains(s, lf) {
							t.Fatalf("record %d call ID %q", i, id)
						}
					}
				}
			}
			// The structured nonce is the one copy left; narrative copies are
			// replaced; the probe events are byte-identical to the receipt's
			// source hash.
			vendor := string(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileVendorEvents)])
			if strings.Count(vendor, "e6085f2e06acab78e174b529053161d7") != 1 {
				t.Fatal("a narrative nonce copy survived or the structured one is gone")
			}
			for _, f := range san.Files {
				if f.Path == mcpqual.ClientFile("cursor", mcpqual.FileServerEvents) && f.SourceSHA256 != f.ExportedSHA256 {
					t.Fatal("the probe events changed")
				}
			}
		}},
		{"production-inventory", func(t *testing.T) {
			idx, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{Root: testkit.MustRepoRoot(t), Registry: mcpqual.DefaultRegistry()})
			if err != nil || len(idx.Entries) != 4 {
				t.Fatalf("production enrollment: %v", err)
			}
			// The three B2 entries are byte-identical to their approved state.
			b2 := map[string][3]string{
				"claude": {"1b315aabf249095bff003191701581f00b29b43b6cc54c09262b1ada41574263", "c025d2e266ecbf877f73c58be232ed79d67a646f84dfa6b242ec43530722d803",
					"e21ba896b5ed35e86528b7962626bfbb92e922ace7d5aa7a3a18397e5042888a"},
				"codex": {"1dc53f26f8e4b1a87260feec8e9e147c5cc111a511e757be7f98544e6458f8ae", "2c32bd42aa287ea6ad23656f386593d82b3bd61467ab31cfd7ded88207c2cf65",
					"4d973d0a1f0c2898a691fddb97f339d885be3459b31f72ace65e98905344d52c"},
				"grok": {"6136a43e48f03b539de75fd5c8e7da97e482352cef19bf31f2a7339b49ac20b0", "15c0311ac812835ca5b1e1c3587c10f7b1fbe6ba8f94c1ed9b93fe662ff2b732",
					"6f7cb670d687aa359bb218d170119abb853b0b91437154f30c74aea20f19c58a"},
			}
			var clients []string
			for _, e := range idx.Entries {
				clients = append(clients, e.Client)
				if w, ok := b2[e.Client]; ok && (e.ManifestSHA256 != w[0] || e.ExpectedSHA256 != w[1] || deref(e.SanitizationSHA256) != w[2]) {
					t.Fatalf("B2 entry %s changed: %+v", e.Client, e)
				}
			}
			if !slices.Equal(clients, []string{"claude", "codex", "cursor", "grok"}) {
				t.Fatalf("entries %v", clients)
			}
			e := idx.Entries[2]
			if e.Fixture != cursorFixtureID || e.Bundle != mcpqual.EnrolledBundlePath("cursor", mcpqual.CursorRealVersion, "linux/amd64", "20261008T212659Z-a5af9a") {
				t.Fatalf("cursor entry %+v", e)
			}
			v, _, _ := mcpqual.DefaultRegistry().Select("cursor-jsonl", mcpqual.CursorRealVersion)
			_, dv, _ := mcpqual.DefaultRegistry().Select("cursor-jsonl", mcpqual.CursorRealVersion)
			if v == nil || len(dv.Evidence) != 1 || !slices.Equal(dv.Evidence[0].Kinds, []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}) ||
				len(dv.MissingCapabilities("darwin/arm64", mcpqual.CapToolCall)) == 0 || len(dv.MissingCapabilities("linux/amd64", mcpqual.CapPermissionDenied)) == 0 {
				t.Fatalf("evidence %+v", dv)
			}
		}},
		{"provenance-corruption", func(t *testing.T) {
			validate := func(repo string) error {
				_, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: mcpqual.DefaultRegistry()})
				return err
			}
			for name, tc := range map[string]struct {
				mutate func(repo string)
				want   string
			}{
				"receipt-b2-policy": {func(repo string) {
					reindex(t, repo, "cursor", mcpqual.SanitizationName, func(m map[string]any) { m["policy"] = mcpqual.SanitizationPolicyB2 })
				}, "policy"},
				"oracle-b2-policy": {func(repo string) {
					reindex(t, repo, "cursor", mcpqual.ExpectedName, func(m map[string]any) { m["sanitization_policy"] = mcpqual.SanitizationPolicyB2 })
				}, "sanitization_policy"},
				"oracle-lf-removed": {func(repo string) {
					reindex(t, repo, "cursor", mcpqual.ExpectedName, func(m map[string]any) {
						for _, ev := range m["events"].([]any) {
							ev.(map[string]any)["request_id"] = strings.ReplaceAll(ev.(map[string]any)["request_id"].(string), string(rune(10)), "")
						}
					})
				}, "replay"},
				"oracle-source-hash": {func(repo string) {
					reindex(t, repo, "cursor", mcpqual.ExpectedName, func(m map[string]any) { m["source_capture_sha256"] = strings.Repeat("a", 64) })
				}, "source manifest hash"},
				"metadata-leak": {func(repo string) {
					e := productionEntry(t, "cursor")
					rewrite(t, filepath.Join(repo, filepath.FromSlash(e.Bundle), "clients", "cursor", mcpqual.FileVendorEvents), `\"cwd\":\"[fixture metadata]\"`, `\"cwd\":\"/home/owner\"`)
				}, "differ from the manifest"},
			} {
				repo := fixtureRepo(t)
				tc.mutate(repo)
				if err := validate(repo); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: %v", name, err)
				}
			}
		}},
		{"owner-gate", func(t *testing.T) {
			for name, a := range map[string][2]string{"pending": {"", ""}, "reviewer-only": {"", mcpqual.B2ReviewerHandle}, "owner-only": {mcpqual.B2OwnerHandle, ""},
				"placeholder": {"<owner>", "<reviewer>"}, "todo": {"TODO", "TBD"}} {
				repo := fixtureRepo(t)
				reindex(t, repo, "cursor", mcpqual.ExpectedName, func(m map[string]any) {
					m["attestation"].(map[string]any)["owner"], m["attestation"].(map[string]any)["reviewer"] = a[0], a[1]
				})
				if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: mcpqual.DefaultRegistry()}); err == nil || !strings.Contains(err.Error(), "attestation") {
					t.Errorf("%s: a pending or placeholder Cursor oracle validated: %v", name, err)
				}
			}
			guide := string(repoFile(t, mcpqual.SetupDocPath))
			sec := guide[strings.Index(guide, "#### Cursor enrollment, per-case preparation and worker residue (B3)"):]
			for _, w := range []string{"the same seven-item mid-implementation handback, empty handles, literal `STATUS: BLOCKED` reason, independent coordinator verification",
				"explicit owner acceptance", "`decoder-enrollment-b3-cursor-metadata-v2`",
				"the init record's `apiKeySource` keeps the capture policy's `[REDACTED]`", "after the approved fixture, final offline acceptance and code review"} {
				if !strings.Contains(sec, w) {
					t.Fatalf("the B3 section lacks %q", w)
				}
			}
		}},
	})
}

// failingEnableReaper reaps with its inner reaper and reports a cleanup
// failure for the enable command's group (an injected fault: the group is
// still reaped).
type failingEnableReaper struct {
	mcpqual.Reaper
	mu     sync.Mutex
	enable map[int]bool
}

// enableLauncher records the enable command's process groups and runs
// before (when set) as each enable starts.
type enableLauncher struct {
	mcpqual.Launcher
	r      *failingEnableReaper
	before func(n int)
	mu     sync.Mutex
	n      int
}

func (l *enableLauncher) Start(spec mcpqual.ProcSpec) (mcpqual.Proc, error) {
	enable := slices.Equal(spec.Args, mcpqual.CursorApprovalArgv())
	if enable && l.before != nil {
		l.mu.Lock()
		l.n++
		n := l.n
		l.mu.Unlock()
		l.before(n)
	}
	p, err := l.Launcher.Start(spec)
	if err == nil && enable && l.r != nil {
		l.r.mu.Lock()
		l.r.enable[p.PGID()] = true
		l.r.mu.Unlock()
	}
	return p, err
}

func (r *failingEnableReaper) Reap(p mcpqual.Proc) mcpqual.CaseCleanup {
	cc := r.Reaper.Reap(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.enable[p.PGID()] {
		cc.Error = sptrF("cleanup: injected")
	}
	return cc
}

// jumpClock is the real clock whose Now jumps forward by skip once armed.
type jumpClock struct {
	mcpqual.Clock
	mu   sync.Mutex
	skip time.Duration
}

func (c *jumpClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Clock.Now().Add(c.skip)
}

// FP-24: each qualify case of the pinned adapter gets its own preparation
// in a fresh workspace (the exact permission file, one enable with the
// shared inventories and the three reads) before its model process; every
// failure stops before the model or makes the case inconclusive with its
// evidence; the report schema is strict.
func TestMCPCursorQualifyPreparation(t *testing.T) {
	t.Parallel()
	var goodOut string
	runInventory(t, 6, []string{"fresh-workspaces", "exact-permission", "approval-scope", "three-read-checks", "failure-budget-cleanup", "report-validation"}, []captureCase{
		{"fresh-workspaces", func(t *testing.T) {
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			goodOut = filepath.Join(b3Root(t), "out")
			rep := qualifyCursor(t, q, cursorQualifyPlan(q, nil, []int{1}), goodOut, nil)
			cases := allCases(rep)
			if rep.Outcome != mcpqual.StatusConclusive || len(cases) != 3 {
				t.Fatalf("%s %d cases: %s", rep.Outcome, len(cases), caseReasons(cases))
			}
			for _, cs := range cases {
				verified(t, cs, mcpqual.ScopeProjectScoped)
			}
			if got := q.launchOrder(); !slices.Equal(got, []string{"version", "help", "enable", "session", "enable", "session", "enable", "session"}) {
				t.Fatalf("launch order %v", got)
			}
			recs := q.argvLog()
			if len(recs) != 3 {
				t.Fatalf("%d sessions", len(recs))
			}
			for i, want := range []string{"cursor-setup", "cursor-default-1", "cursor-default-repeat"} {
				if filepath.Base(recs[i].Cwd) != want {
					t.Fatalf("session %d ran in %s", i, recs[i].Cwd)
				}
			}
		}},
		{"exact-permission", func(t *testing.T) {
			// Every session saw exactly the A2 bytes, 0600, in its own
			// workspace (the fake's enable checks the file before writing its
			// approval); nothing under the owner's home.
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			rep := qualifyCursor(t, q, cursorQualifyPlan(q, nil, []int{1}), filepath.Join(b3Root(t), "out"), nil)
			for _, r := range q.argvLog() {
				if r.Permission != "600 "+mcpqual.CursorToolPermissionContent {
					t.Fatalf("a session saw %q", r.Permission)
				}
			}
			for _, cs := range allCases(rep) {
				tp := cs.CursorPreparation.ToolPermission
				if tp.Content != mcpqual.CursorToolPermissionContent || tp.Adapter != mcpqual.CursorToolPermissionAdapter || tp.SHA256 != sha([]byte(tp.Content)) {
					t.Fatalf("record %+v", tp)
				}
			}
			filepath.WalkDir(q.home, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.Name() == "cli.json" {
					t.Fatalf("a home permission file %s", p)
				}
				return nil
			})
			// The pre-A2 allow-only bytes in place of the harness file just
			// before the enable: the vendor's schema check fails it.
			q = newQualEnv(t)
			ownerCursorProjects(t, q)
			rep = qualifyCursor(t, q, cursorQualifyPlan(q, nil, []int{1}), filepath.Join(b3Root(t), "out"), func(r *mcpqual.Runner) {
				r.Launcher = legacyPermissionLauncher{Launcher: r.Launcher, kind: "enable"}
			})
			cases := allCases(rep)
			unverified(t, cases[0], mcpqual.ReasonCursorApprovalFailed)
			if len(cases) != 1 || len(q.launchLog()["session"]) != 0 {
				t.Fatalf("legacy content: %d cases, %v", len(cases), q.launchLog())
			}
		}},
		{"approval-scope", func(t *testing.T) {
			for name, tc := range map[string]struct {
				enable, want string
			}{
				"workspace":  {"permission-check,workspace", ""},
				"owner-home": {"permission-check,owner", mcpqual.ReasonCursorOutside},
				"extra-file": {"permission-check,project-extra", mcpqual.ReasonCursorOutside},
				"no-change":  {"permission-check", "no change gives positive evidence"},
				"other-slug": {"permission-check,project-other", mcpqual.ReasonCursorOutside},
			} {
				q := newQualEnv(t)
				ownerCursorProjects(t, q)
				rep := qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"ENABLE": tc.enable}, []int{1}), filepath.Join(b3Root(t), "out"), nil)
				cases := allCases(rep)
				if tc.want == "" {
					for _, cs := range cases {
						verified(t, cs, mcpqual.ScopeWorkspaceOnly)
					}
					continue
				}
				unverified(t, cases[0], tc.want)
				if len(cases) != 1 || len(q.launchLog()["session"]) != 0 || len(q.launchLog()["enable"]) != 1 {
					t.Fatalf("%s: %d cases, %v", name, len(cases), q.launchLog())
				}
			}
		}},
		{"three-read-checks", func(t *testing.T) {
			// A change during the enable stops before the model; a removal
			// during the session leaves the case inconclusive after its
			// cleanup read, with its evidence, and stops the client.
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			rep := qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"ENABLE": "permission-check,project,permission-modify"}, []int{1}), filepath.Join(b3Root(t), "out"), nil)
			cases := allCases(rep)
			unverified(t, cases[0], mcpqual.ReasonCursorScopeUnverified)
			if len(cases) != 1 || len(q.launchLog()["session"]) != 0 || cases[0].CursorPreparation.PermissionChecks.AfterEnable {
				t.Fatalf("after enable: %+v %v", cases, q.launchLog())
			}
			q = newQualEnv(t)
			ownerCursorProjects(t, q)
			rep = qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"SESSION_PERMISSION": "remove"}, []int{1}), filepath.Join(b3Root(t), "out"), nil)
			cases = allCases(rep)
			// (The fake's setting value "remove" is a credential-named plan
			// variable's value, so the report redacts it as a literal.)
			unverified(t, cases[0], "the harness-written <workspace>/.cursor/cli.json was")
			p := cases[0].CursorPreparation
			if len(cases) != 1 || len(q.launchLog()["session"]) != 1 || p.PermissionChecks != (mcpqual.PermissionChecks{AfterEnable: true, BeforeSession: true}) ||
				p.ToolPermission.State != mcpqual.PermissionWritten || cases[0].ServerEvents == nil || cases[0].VendorEvents == nil {
				t.Fatalf("during session: %+v %v", p, q.launchLog())
			}
		}},
		{"failure-budget-cleanup", func(t *testing.T) {
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			rep := qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"ENABLE": "fail"}, []int{1}), filepath.Join(b3Root(t), "out"), nil)
			unverified(t, allCases(rep)[0], mcpqual.ReasonCursorApprovalFailed)
			if len(q.launchLog()["session"]) != 0 {
				t.Fatal("a session after a failed enable")
			}
			// The case budget includes the preparation: a clock that passes
			// the case's 120 s during the enable launches no model.
			q = newQualEnv(t)
			ownerCursorProjects(t, q)
			rep = qualifyCursor(t, q, cursorQualifyPlan(q, nil, []int{1}), filepath.Join(b3Root(t), "out"), func(r *mcpqual.Runner) {
				c := &jumpClock{Clock: r.Clock}
				r.Clock = c
				r.Launcher = &enableLauncher{Launcher: r.Launcher, before: func(int) { c.mu.Lock(); c.skip = 121 * time.Second; c.mu.Unlock() }}
			})
			unverified(t, allCases(rep)[0], mcpqual.ReasonBudget)
			if len(q.launchLog()["session"]) != 0 {
				t.Fatal("a model launch after the case budget")
			}
			// A cleanup failure of the enable command stops the case and the
			// run's cleanup report records it.
			q = newQualEnv(t)
			ownerCursorProjects(t, q)
			rep = qualifyCursor(t, q, cursorQualifyPlan(q, nil, []int{1}), filepath.Join(b3Root(t), "out"), func(r *mcpqual.Runner) {
				fr := &failingEnableReaper{Reaper: r.Reaper, enable: map[int]bool{}}
				r.Reaper = fr
				r.Launcher = &enableLauncher{Launcher: r.Launcher, r: fr}
			})
			unverified(t, allCases(rep)[0], mcpqual.ReasonCleanupFailed)
			if rep.Cleanup.OK || len(q.launchLog()["session"]) != 0 {
				t.Fatalf("cleanup %+v", rep.Cleanup)
			}
		}},
		{"report-validation", func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(goodOut, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			rep, err := mcpqual.ParseReport(b)
			if err != nil || rep.CheckEvidence(goodOut) != nil {
				t.Fatalf("report: %v", err)
			}
			edit := func(f func(cs map[string]any)) []byte {
				var m map[string]any
				json.Unmarshal(b, &m)
				f(m["clients"].([]any)[0].(map[string]any)["phases"].([]any)[0].(map[string]any)["cases"].([]any)[0].(map[string]any))
				nb, _ := json.Marshal(m)
				return nb
			}
			for name, nb := range map[string][]byte{
				"absent": edit(func(cs map[string]any) { delete(cs, "cursor_preparation") }),
				"null":   edit(func(cs map[string]any) { cs["cursor_preparation"] = nil }),
				"forged-output": edit(func(cs map[string]any) {
					cs["cursor_preparation"].(map[string]any)["approval_stdout"] = "cases/cursor-default-1/approval-stdout.txt"
				}),
				"unknown": edit(func(cs map[string]any) { cs["cursor_preparation"].(map[string]any)["approved"] = true }),
				"downgraded": edit(func(cs map[string]any) {
					cs["cursor_preparation"].(map[string]any)["state"], cs["cursor_preparation"].(map[string]any)["reason"] = "unverified", "x"
				}),
			} {
				if _, err := mcpqual.ParseReport(nb); err == nil {
					t.Errorf("%s: a forged preparation parsed", name)
				}
			}
			// The retained enable output is hash-checked evidence.
			cp := filepath.Join(realTemp(t), "copy")
			copyTree(t, goodOut, cp)
			appendFile(t, filepath.Join(cp, "cases", "cursor-setup", mcpqual.FileApprovalStdout), "forged\n")
			if err := rep.CheckEvidence(cp); err == nil {
				t.Fatal("altered approval output passed")
			}
		}},
	})
}

// residueSockets are the residue sockets under home's Cursor projects.
func residueSockets(t *testing.T, home string) []string {
	t.Helper()
	var out []string
	es, _ := os.ReadDir(filepath.Join(home, ".cursor", "projects"))
	for _, e := range es {
		p := filepath.Join(home, ".cursor", "projects", e.Name(), "worker.sock")
		if info, err := os.Lstat(p); err == nil && info.Mode().Type() == fs.ModeSocket {
			out = append(out, p)
		}
	}
	return out
}

// FP-25: the fake vendor leaves a real test-owned worker socket in each
// session; the run admits it into its ledger, the next case passes over it,
// a new invocation refuses the leftovers until the owner removes them, and
// every unsafe shape, replacement or bound fails closed; capture records its
// own residue and the runbook describes owner cleanup only.
func TestMCPCursorWorkerResidue(t *testing.T) {
	t.Parallel()
	runInventory(t, 6, []string{"attributed", "next-case", "pre-existing", "unsafe-shapes", "replacement-and-bounds", "capture-and-runbook"}, []captureCase{
		{"attributed", func(t *testing.T) {
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			rep := qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"RESIDUE": "attributed"}, []int{1}), filepath.Join(b3Root(t), "out"), nil)
			cases := allCases(rep)
			if rep.Outcome != mcpqual.StatusConclusive || len(cases) != 3 {
				t.Fatalf("%s %d: %s", rep.Outcome, len(cases), caseReasons(cases))
			}
			for i, cs := range cases {
				verified(t, cs, mcpqual.ScopeProjectScoped)
				es := cs.CursorPreparation.Residue.Entries
				if len(es) != i+1 || es[i].CaseID != cs.CaseID || es[i].Path != "<home>/.cursor/projects/<case-residue-"+string(rune('1'+i))+">/worker.sock" {
					t.Fatalf("case %d residue %+v", i, es)
				}
			}
			// The harness neither removed nor replaced the sockets.
			if got := residueSockets(t, q.home); len(got) != 3 {
				t.Fatalf("sockets %v", got)
			}
		}},
		{"next-case", func(t *testing.T) {
			// The other observed truncation (the case basename cut after its
			// hyphen): the second and third cases' inventories pass over the
			// earlier sockets.
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			rep := qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"RESIDUE": "attributed", "RESIDUE_K": "7"}, []int{1}), filepath.Join(b3Root(t), "out"), nil)
			for _, cs := range allCases(rep)[1:] {
				verified(t, cs, mcpqual.ScopeProjectScoped)
				if !cs.CursorPreparation.Approval.InventoryComplete {
					t.Fatalf("case %s inventory", cs.CaseID)
				}
			}
		}},
		{"pre-existing", func(t *testing.T) {
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			plan := cursorQualifyPlan(q, map[string]string{"RESIDUE": "attributed"}, []int{1})
			qualifyCursor(t, q, plan, filepath.Join(b3Root(t), "out"), nil)
			left := residueSockets(t, q.home)
			// A fresh invocation fails closed before any enable.
			enables := len(q.launchLog()["enable"])
			rep := qualifyCursor(t, q, plan, filepath.Join(b3Root(t), "out"), nil)
			unverified(t, allCases(rep)[0], "special file")
			if len(q.launchLog()["enable"]) != enables {
				t.Fatal("an enable after the refused pre-scan")
			}
			// The owner removes exactly the known residue (Cursor stopped);
			// the next invocation passes.
			for _, s := range left {
				os.RemoveAll(filepath.Dir(s))
			}
			rep = qualifyCursor(t, q, cursorQualifyPlan(q, nil, []int{1}), filepath.Join(b3Root(t), "out"), nil)
			if rep.Outcome != mcpqual.StatusConclusive {
				t.Fatalf("after owner cleanup: %s", rep.Outcome)
			}
		}},
		{"unsafe-shapes", func(t *testing.T) {
			for _, mode := range []string{"full", "regular", "extra", "two"} {
				q := newQualEnv(t)
				ownerCursorProjects(t, q)
				rep := qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"RESIDUE": mode}, []int{1}), filepath.Join(b3Root(t), "out"), nil)
				cases := allCases(rep)
				unverified(t, cases[0], "worker residue")
				if len(cases) != 1 || len(q.launchLog()["session"]) != 1 || cases[0].CursorPreparation.Residue.State != mcpqual.ResidueUnverified {
					t.Fatalf("%s: %+v", mode, cases)
				}
			}
		}},
		{"replacement-and-bounds", func(t *testing.T) {
			// The first case's admitted socket replaced by a regular file
			// before the second case's enable: the ledger revalidation fails
			// closed.
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			rep := qualifyCursor(t, q, cursorQualifyPlan(q, map[string]string{"RESIDUE": "attributed"}, []int{1}), filepath.Join(b3Root(t), "out"), func(r *mcpqual.Runner) {
				r.Launcher = &enableLauncher{Launcher: r.Launcher, before: func(n int) {
					if n == 2 {
						for _, s := range residueSockets(t, q.home) {
							os.Remove(s)
							os.WriteFile(s, nil, 0o600)
						}
					}
				}}
			})
			cases := allCases(rep)
			if len(cases) != 2 || cases[0].CursorPreparation.State != mcpqual.PreparationVerified || cases[1].CursorPreparation.State == mcpqual.PreparationVerified ||
				cases[1].Outcome == mcpqual.KindToolResult {
				t.Fatalf("replacement: %+v", cases)
			}
			// The bounded inventories: a cap reached fails closed before any
			// enable (never sampled or enlarged).
			q = newQualEnv(t)
			ownerCursorProjects(t, q)
			rep = qualifyCursor(t, q, cursorQualifyPlan(q, nil, []int{1}), filepath.Join(b3Root(t), "out"), func(r *mcpqual.Runner) {
				r.ApprovalLimits = mcpqual.ApprovalScanLimits{Entries: 3}
			})
			unverified(t, allCases(rep)[0], "bound")
			if len(q.launchLog()["enable"]) != 0 {
				t.Fatal("an enable after an exhausted bound")
			}
		}},
		{"capture-and-runbook", func(t *testing.T) {
			q := newQualEnv(t)
			ownerCursorProjects(t, q)
			m := inProcessRun(t, q, cursorPermissionPlan(q, map[string]string{"FORMAT": "cursor-real", "ENABLE": "permission-check,project", "RESIDUE": "attributed"}),
				filepath.Join(realTemp(t), "out"), nil)
			c := captureClient(t, m, "cursor")
			w := c.WorkerResidue
			if m.State != mcpqual.CaptureComplete || w == nil || w.State != mcpqual.ResidueVerified || len(w.Entries) != 1 || w.Entries[0].CaseID != "cursor-capture-setup" {
				t.Fatalf("capture residue %s %+v", m.State, w)
			}
			// A later independent capture refuses the leftover before any
			// enable.
			m = inProcessRun(t, q, cursorPermissionPlan(q, map[string]string{"FORMAT": "cursor-real", "ENABLE": "permission-check,project"}), filepath.Join(realTemp(t), "out"), nil)
			if c := captureClient(t, m, "cursor"); m.State == mcpqual.CaptureComplete || !strings.Contains(deref(c.Reason), "special file") {
				t.Fatalf("leftover: %s %q", m.State, deref(c.Reason))
			}
			q = newQualEnv(t)
			ownerCursorProjects(t, q)
			m = inProcessRun(t, q, cursorPermissionPlan(q, map[string]string{"FORMAT": "cursor-real", "ENABLE": "permission-check,project", "RESIDUE": "regular"}),
				filepath.Join(realTemp(t), "out"), nil)
			if c := captureClient(t, m, "cursor"); m.State == mcpqual.CaptureComplete || c.WorkerResidue == nil || c.WorkerResidue.State != mcpqual.ResidueUnverified {
				t.Fatalf("unsafe capture residue: %s %+v", m.State, c.WorkerResidue)
			}
			guide := string(repoFile(t, mcpqual.SetupDocPath))
			for _, w := range []string{"in-memory ledger", "never opened, connected to, followed or deleted", "with Cursor stopped, inspect it and remove only the known residue directories yourself",
				"there is no automated or wildcard cleanup command", "The initial pre-scan is strict; only the same run's ledger entries are ever exempt"} {
				if !strings.Contains(guide, w) {
					t.Fatalf("the guide lacks %q", w)
				}
			}
		}},
	})
}

// realQualifyCursor is one in-process qualification of the clocked fake
// Cursor vendor (the reviewed real format, an in-process enable writing the
// workspace approval) with the canonical trusted recipe of the pinned
// version and its enrolled fixture, model grok-4.7-low, the short schedule
// (setup, one silent 15000 ms call and its repeat; 3 sessions, 120 s per
// case, 360 s per client) on an injected clock, from a short output path
// under the production registry on goos/amd64.
func realQualifyCursor(t *testing.T, settings map[string]string, goos string, mutate func(c map[string]any)) (*mcpqual.Report, string, error) {
	t.Helper()
	q := newQualEnv(t)
	const exe = "/fake/cursor"
	env := map[string]string{"FAKE_VENDOR_FORMAT": "cursor-real", "FAKE_VENDOR_VERSION": mcpqual.CursorRealVersion, "FAKE_VENDOR_CLIENT_NAME": "Cursor", "FAKE_VENDOR_CLIENT_VERSION": "1.0.0"}
	for k, v := range settings {
		env["FAKE_VENDOR_"+k] = v
	}
	c := map[string]any{"id": "cursor", "executable": exe, "expected_version": mcpqual.CursorRealVersion, "version_argv": []string{"--version"}, "driver": "model",
		"model": "grok-4.7-low", "decoder": "cursor-jsonl", "decoder_fixture": cursorFixtureID, "session": map[string]any{"argv": mcpqual.CursorTrustedArgv("grok-4.7-low")},
		"config": map[string]any{"default": map[string]any{"path": mcpqual.CursorTrustedPath, "content": probeConfig}}, "env": env,
		"phases": map[string]any{"default": map[string]any{"delays_ms": []int{15000}}}}
	if mutate != nil {
		mutate(c)
	}
	plan := map[string]any{"version": 1, "clients": []any{c}, "limits": map[string]any{"max_sessions_per_client": 3, "max_case_ms": 120000, "max_client_ms": 360000}}
	b, _ := json.Marshal(plan)
	p, err := mcpqual.ParsePlan(b, mcpqual.DefaultRegistry())
	if err != nil {
		return nil, "", err
	}
	clock := testkit.NewFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	const server = "/fake/mcpqual"
	out := filepath.Join(b3Root(t), "out")
	r := &mcpqual.Runner{Plan: p, PlanSHA256: sha(b), OutDir: out, AllowModelCalls: true, GOOS: goos, GOARCH: "amd64", Hostname: "function-host", ServerPath: server,
		BaseEnv: q.env(""), Launcher: &clockedVendor{t: t, clock: clock, server: server}, Reaper: clockedReaper{}, Clock: clock, Registry: mcpqual.DefaultRegistry(),
		Log: io.Discard, RunID: "run-real-cursor", Nonce: "noncerealcursor", CaptureDate: "2026-10-09", HarnessVersion: mcpqual.HarnessVersion, Home: q.home,
		HashFile: func(p string) (string, error) {
			if p != exe {
				return "", os.ErrNotExist
			}
			return "2ccc9a8e167797641448b5e5c936f006ba137a2555f117f38c5eb76a5238a233", nil
		}}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep, out, nil
}

// FP-26: the owner's 15 s Cursor confirmation path on the fake vendor's
// reviewed shape and the real probe, driven by the injected clock without
// sleeps: setup and the repeated 15000 ms bound with per-case preparation;
// rejection, unseen timeout, failed preparation and another platform stay
// UNVERIFIED; exact identity and opt-in publication; the documented gate
// order and prerequisites.
func TestMCPCursorConfirmation(t *testing.T) {
	t.Parallel()
	var goodRep *mcpqual.Report
	var goodOut string
	runInventory(t, 4, []string{"repeated-bound", "unverified-outcomes", "identity-publication", "delivery-runbook"}, []captureCase{
		{"repeated-bound", func(t *testing.T) {
			rep, out, err := realQualifyCursor(t, nil, "linux", nil)
			if err != nil {
				t.Fatal(err)
			}
			cr := reportClient(t, rep, "cursor")
			def := reportPhase(t, cr, mcpqual.PhaseDefault)
			if rep.Outcome != mcpqual.StatusConclusive || reportPhase(t, cr, mcpqual.PhaseSetup).Status != mcpqual.StatusConclusive || ms(def.LowerBoundMS) != 15000 ||
				def.UpperBoundMS != nil || def.Observations != 2 || len(allCases(rep)) != 3 || *cr.ExecutableSHA256 != "2ccc9a8e167797641448b5e5c936f006ba137a2555f117f38c5eb76a5238a233" {
				t.Fatalf("%s %+v", rep.Outcome, def)
			}
			for _, cs := range allCases(rep) {
				verified(t, cs, mcpqual.ScopeWorkspaceOnly)
			}
			decision := mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, cr)
			md := string(repoFileAt(t, out, "report.md"))
			if !strings.Contains(decision, "compatible for this measured tuple: L = 15s is the longest silent call that completed (a lower bound, not the timeout): 10s + max(2s, 1.5s) = 12s < 15s.") ||
				strings.Contains(decision, "UNVERIFIED") || !strings.Contains(md, "- "+decision+"\n") {
				t.Fatalf("decision %q", decision)
			}
			goodRep, goodOut = rep, out
		}},
		{"unverified-outcomes", func(t *testing.T) {
			for name, tc := range map[string]struct {
				settings map[string]string
				goos     string
				want     string
			}{
				"rejection":   {map[string]string{"SCENARIO": "rejected"}, "linux", "rejection"},
				"timeout":     {map[string]string{"TIMEOUT_MS": "10000"}, "linux", ""},
				"preparation": {map[string]string{"ENABLE": "none"}, "linux", "no change gives positive evidence"},
				"darwin":      {nil, "darwin", mcpqual.ReasonUnverifiedEvent},
			} {
				rep, _, err := realQualifyCursor(t, tc.settings, tc.goos, nil)
				if err != nil {
					t.Fatal(err)
				}
				cr := reportClient(t, rep, "cursor")
				if !strings.Contains(mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, cr), "UNVERIFIED") || rep.Outcome == mcpqual.StatusConclusive {
					t.Errorf("%s: %s", name, mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, cr))
				}
				for _, cs := range allCases(rep) {
					if cs.Outcome == mcpqual.KindToolResult && cs.DelayMS == 15000 || cs.Outcome == mcpqual.KindMCPTimeout {
						t.Errorf("%s: a conclusive 15 s outcome %+v", name, cs)
					}
					if tc.goos == "darwin" && cs.CursorPreparation != nil {
						t.Errorf("darwin: a preparation record")
					}
				}
				if tc.want != "" && !strings.Contains(deref(allCases(rep)[0].Reason), tc.want) {
					t.Errorf("%s: %q", name, deref(allCases(rep)[0].Reason))
				}
			}
		}},
		{"identity-publication", func(t *testing.T) {
			rep, _, err := realQualifyCursor(t, nil, "linux", func(c map[string]any) { c["decoder_fixture"] = "cursor-jsonl/synthetic" })
			if err != nil || rep.Outcome == mcpqual.StatusConclusive || !strings.Contains(deref(reportClient(t, rep, "cursor").Reason), mcpqual.ReasonDecoderFixture) {
				t.Fatalf("a synthetic fixture for the enrolled version: %v", err)
			}
			rep, _, err = realQualifyCursor(t, map[string]string{"VERSION": "2026.10.02-0000000"}, "linux", nil)
			if err != nil || rep.Outcome == mcpqual.StatusConclusive || !strings.Contains(deref(reportClient(t, rep, "cursor").Reason), mcpqual.ReasonVersionMismatch) {
				t.Fatalf("version mismatch: %v", err)
			}
			// Publication is opt-in and exact: another catalog version
			// conflicts; the matching one publishes only a lower bound.
			if goodRep == nil {
				t.Fatal("no confirmation report")
			}
			repo, base := entryRepo(t, "cursor", "2026.10.02-0000000", "linux/amd64")
			cp := filepath.Join(realTemp(t), "copy")
			copyTree(t, goodOut, cp)
			if _, err := mcpqual.ProposePatch(goodRep, base, cp); err == nil {
				t.Fatal("a version conflict proposed a patch")
			}
			repo, base = entryRepo(t, "cursor", mcpqual.CursorRealVersion, "linux/amd64")
			if _, err := mcpqual.ProposePatch(goodRep, base, goodOut); err != nil {
				t.Fatal(err)
			}
			if err := mcpqual.Publish(goodOut, repo); err != nil {
				t.Fatal(err)
			}
			if f := repoFacts(t, repo)["cursor"].Facts["mcp_timeout"]; f.Status != catalog.Verified || !strings.Contains(f.Value, "this is a lower bound, not the default") {
				t.Fatalf("published %+v", f)
			}
		}},
		{"delivery-runbook", func(t *testing.T) {
			guide := string(repoFile(t, mcpqual.SetupDocPath))
			idx, err := mcpqual.ParseEnrollmentIndex(repoFile(t, mcpqual.EnrollmentIndexPath))
			if err != nil || len(idx.Entries) != 4 {
				t.Fatalf("index %v", err)
			}
			for _, e := range idx.Entries {
				if !strings.Contains(guide, "](../"+e.Bundle+")") || !strings.Contains(guide, "`"+e.Fixture+"`") {
					t.Fatalf("enrolled %s is not linked with its fixture ID", e.Fixture)
				}
			}
			sec := guide[strings.Index(guide, "#### Cursor enrollment, per-case preparation and worker residue (B3)"):]
			for _, w := range []string{"must be at most 54 ASCII characters", "`cursor_approval_scope_unverified: cursor residue parent slug exceeds 54 characters`",
				"`2ccc9a8e167797641448b5e5c936f006ba137a2555f117f38c5eb76a5238a233`", "model `grok-4.7-low`", "the old capture's plan is never rewritten",
				"Record the actual version and hash immediately before the run (a mismatch blocks)", "only the owner's own `--publish-catalog` at the start of that invocation publishes anything",
				"macOS remains UNVERIFIED", "every `qualify` case (setup, the silent 15s call and its repeat) gets its own fresh workspace"} {
				if !strings.Contains(sec, w) {
					t.Fatalf("the B3 section lacks %q", w)
				}
			}
			order := []string{"Mid-implementation handback", "Independent verification and explicit owner acceptance", "Final offline acceptance", "Cursor follow-up (code gate B3)",
				"Owner short-confirmation gate", "Optional publication gate"}
			at := -1
			for _, w := range order {
				i := strings.Index(guide, w)
				if i <= at {
					t.Fatalf("%q is missing or out of order", w)
				}
				at = i
			}
			catalogMD := string(repoFile(t, mcpqual.CatalogMDPath))
			for _, w := range []string{"Decoder enrollment B2 and B3 enroll exactly four linux/amd64 identities", "Cursor Agent `2026.10.01-e373342` (B3, closing the B2 Cursor blocker)",
				"at most 54 characters"} {
				if !strings.Contains(catalogMD, w) {
					t.Fatalf("support-catalog.md lacks %q", w)
				}
			}
		}},
	})
}
