package function

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Design decoder-enrollment B2 function tests (FP-17..FP-21): one parent
// per FP with its literal local case inventory. They use the checked-in
// sanitised fixtures, the real probe, the real mcpqual and export
// executables and the fake vendor only: no installed vendor CLI, network,
// authentication, raw capture bundle or design/ path. Process scenarios
// stay outside the stress selectors.

// b2Clients are the enrolled clients with their exact versions and run
// IDs (design decoder-enrollment B2, Evidence and precedence).
var b2Clients = []struct {
	id, decoder, version, run string
}{
	{"codex", "codex-jsonl", mcpqual.CodexRealVersion, "20261008T110604Z-13cb4a"},
	{"grok", "grok-json", mcpqual.GrokRealVersion, "20261008T110619Z-1663d7"},
	{"claude", "claude-json", mcpqual.ClaudeRealVersion, "20261008T122513Z-66db37"},
}

// productionEntry is the production index entry of client.
func productionEntry(t *testing.T, client string) mcpqual.EnrollmentEntry {
	t.Helper()
	b := repoFile(t, mcpqual.EnrollmentIndexPath)
	idx, err := mcpqual.ParseEnrollmentIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		if e.Client == client {
			return e
		}
	}
	t.Fatalf("no production enrollment of %s", client)
	return mcpqual.EnrollmentEntry{}
}

// enrolledBundle is client's checked-in bundle, validated with its two
// extra files, its oracle and its receipt bytes.
func enrolledBundle(t *testing.T, client string) (mcpqual.EnrollmentEntry, *mcpqual.CaptureBundle, *mcpqual.ExpectedOracle, []byte) {
	t.Helper()
	e := productionEntry(t, client)
	dir := filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(e.Bundle))
	b, err := mcpqual.ValidateCaptureBundle(os.DirFS(dir), ".", mcpqual.ExpectedName, mcpqual.SanitizationName)
	if err != nil {
		t.Fatal(err)
	}
	ob, _ := os.ReadFile(filepath.Join(dir, mcpqual.ExpectedName))
	o, err := mcpqual.ParseExpected(ob)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := os.ReadFile(filepath.Join(dir, mcpqual.SanitizationName))
	return e, b, o, sb
}

// withTranscript is b with client's transcript replaced by raw.
func withTranscript(b *mcpqual.CaptureBundle, client string, raw []byte) *mcpqual.CaptureBundle {
	files := map[string][]byte{}
	for k, v := range b.Files {
		files[k] = v
	}
	files[mcpqual.ClientFile(client, mcpqual.FileVendorEvents)] = raw
	cp := *b
	cp.Files = files
	return &cp
}

// records rewrites the decoded data of a wrapper transcript record by
// record (edit returns the new data, or "" to drop the record).
func records(t *testing.T, raw []byte, edit func(i int, data string) string) []byte {
	t.Helper()
	tr, err := mcpqual.ReplayTranscript(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for i, l := range tr.Lines {
		d := edit(i, string(l.Data))
		if d == "" {
			continue
		}
		w, _ := json.Marshal(map[string]any{"offset_ns": l.OffsetNS, "data": d})
		out.Write(append(w, '\n'))
	}
	return out.Bytes()
}

// FP-17: the three exact-version real decoders replay the reviewed golden
// bytes through Select and Replay to their oracles; negative mutations of
// those bytes, prose echoes and Claude's ToolSearch never contribute; and
// dispatch is by the exact version only.
func TestMCPRealDecoderMappings(t *testing.T) {
	t.Parallel()
	reg := mcpqual.DefaultRegistry()
	golden := func(t *testing.T, client string, mutations map[string]func(i int, data string) string) {
		e, b, o, _ := enrolledBundle(t, client)
		if err := mcpqual.Replay(reg, e, b, o); err != nil {
			t.Fatalf("%s golden replay: %v", client, err)
		}
		dec, _, err := reg.Select(e.Decoder, e.Version)
		if err != nil {
			t.Fatal(err)
		}
		tr, _ := mcpqual.ReplayTranscript(b.Files[mcpqual.ClientFile(client, mcpqual.FileVendorEvents)])
		if d := dec(tr); !d.Terminal || d.Inconclusive != "" || len(d.Events) != 2 || d.Events[1].Nonce != o.Nonce || d.Events[0].RequestID != d.Events[1].RequestID {
			t.Fatalf("%s decoded %+v", client, d)
		}
		for name, edit := range mutations {
			forged := withTranscript(b, client, records(t, b.Files[mcpqual.ClientFile(client, mcpqual.FileVendorEvents)], edit))
			if err := mcpqual.Replay(reg, e, forged, o); err == nil {
				t.Fatalf("%s %s: a mutated transcript replayed", client, name)
			}
		}
	}
	nonce := func(o string) func(i int, d string) string {
		return func(_ int, d string) string { return strings.ReplaceAll(d, o, "0000feedfacecafe0000feedfacecafe") }
	}
	runInventory(t, 5, []string{"codex", "grok", "claude", "structured-only", "exact-dispatch"}, []captureCase{
		{"codex", func(t *testing.T) {
			golden(t, "codex", map[string]func(int, string) string{
				"nonce": nonce("ba01c3dad606eec0062d873d448a31f2"),
				"no-result": func(_ int, d string) string {
					if strings.Contains(d, `"item.completed"`) && strings.Contains(d, `"mcp_tool_call"`) {
						return ""
					}
					return d
				},
				"failed": func(_ int, d string) string {
					return strings.Replace(d, `"status":"completed"`, `"status":"failed"`, 1)
				},
				"other-server": func(_ int, d string) string { return strings.ReplaceAll(d, `"server":"probe"`, `"server":"other"`) },
				"no-terminal": func(_ int, d string) string {
					if strings.Contains(d, `"turn.completed"`) {
						return ""
					}
					return d
				},
			})
		}},
		{"grok", func(t *testing.T) {
			golden(t, "grok", map[string]func(int, string) string{
				"nonce":       nonce("246477f501e17c8f2315a6963bf8d8c2"),
				"err-variant": func(_ int, d string) string { return strings.Replace(d, `"OkayOutput"`, `"ErrOutput"`, 1) },
				"other-tool":  func(_ int, d string) string { return strings.Replace(d, `"tool_name":"slow"`, `"tool_name":"fast"`, 1) },
				"stop-reason": func(_ int, d string) string {
					return strings.Replace(d, `"stopReason":"end_turn"`, `"stopReason":"max_tokens"`, 1)
				},
				"duplicate-id": func(_ int, d string) string {
					return strings.Replace(d, `"toolName":"use_tool"`, `"toolName":"use_tool","toolName":"use_tool"`, 1)
				},
			})
		}},
		{"claude", func(t *testing.T) {
			golden(t, "claude", map[string]func(int, string) string{
				"nonce": nonce("387ab817f352f4247d157e6bd4242a17"),
				"is-error": func(_ int, d string) string {
					return strings.Replace(d, `"tool_use_id":"toolu_01KK6p41QYGfUCGd1PL3s29b",`, `"tool_use_id":"toolu_01KK6p41QYGfUCGd1PL3s29b","is_error":true,`, 1)
				},
				"suffix-name": func(_ int, d string) string {
					return strings.Replace(d, `"name":"mcp__probe__slow"`, `"name":"mcp__other__slow"`, 1)
				},
				"error-result": func(_ int, d string) string {
					return strings.Replace(d, `"terminal_reason":"completed"`, `"terminal_reason":"max_turns"`, 1)
				},
			})
		}},
		{"structured-only", func(t *testing.T) {
			// Only the structured result counts: with it removed, the prose
			// echoes of the nonce (Codex's agent message, Grok's text) and
			// Claude's ToolSearch tool_reference never stand in for it.
			for _, c := range b2Clients {
				e, b, o, _ := enrolledBundle(t, c.id)
				raw := b.Files[mcpqual.ClientFile(c.id, mcpqual.FileVendorEvents)]
				var drop func(i int, d string) string
				switch c.id {
				case "codex":
					drop = func(_ int, d string) string {
						if strings.Contains(d, `"item.completed"`) && strings.Contains(d, `"mcp_tool_call"`) {
							return `{"type":"item.completed","item":{"id":"item_9","type":"agent_message","text":"{\"case_id\":\"codex-capture-setup\",\"nonce\":\"` + o.Nonce + `\"}"}}`
						}
						return d
					}
				case "grok":
					drop = func(_ int, d string) string {
						if strings.Contains(d, `"OkayOutput"`) {
							return `{"type":"text","data":"{\"case_id\":\"grok-capture-setup\",\"nonce\":\"` + o.Nonce + `\"}"}`
						}
						return d
					}
				case "claude":
					drop = func(_ int, d string) string {
						// The probe call and its result become ToolSearch-like
						// unrelated traffic naming the probe tool.
						d = strings.Replace(d, `"name":"mcp__probe__slow","input":{"case_id":"claude-capture-setup"}`, `"name":"ToolSearch","input":{"query":"select:mcp__probe__slow"}`, 1)
						return d
					}
				}
				dec, _, _ := mcpqual.DefaultRegistry().Select(e.Decoder, e.Version)
				tr, _ := mcpqual.ReplayTranscript(records(t, raw, drop))
				if d := dec(tr); d.Inconclusive == "" || slices.ContainsFunc(d.Events, func(ev mcpqual.Event) bool { return ev.Kind == mcpqual.KindToolResult }) {
					t.Fatalf("%s: prose or an unrelated tool produced %+v", c.id, d)
				}
				if err := mcpqual.Replay(mcpqual.DefaultRegistry(), e, withTranscript(b, c.id, records(t, raw, drop)), o); err == nil {
					t.Fatalf("%s: replayed without a structured result", c.id)
				}
			}
		}},
		{"exact-dispatch", func(t *testing.T) {
			// Real-format mutations only the exact version's parser refuses (a
			// slow tool of another server, a suffix-matched tool name, the
			// synthetic document shape): the selected decoder is the real
			// one, never the family parser.
			distinguish := map[string]func(int, string) string{
				"codex": func(_ int, d string) string { return strings.ReplaceAll(d, `"server":"probe"`, `"server":"other"`) },
				"grok":  func(_ int, d string) string { return d },
				"claude": func(_ int, d string) string {
					return strings.ReplaceAll(d, `"name":"mcp__probe__slow"`, `"name":"mcp__other__slow"`)
				},
			}
			for _, c := range b2Clients {
				_, b, _, _ := enrolledBundle(t, c.id)
				raw := b.Files[mcpqual.ClientFile(c.id, mcpqual.FileVendorEvents)]
				tr, _ := mcpqual.ReplayTranscript(raw)
				dec, v, err := reg.Select(c.decoder, c.version)
				real := dec(tr)
				if err != nil || !v.Qualified || real.Inconclusive != "" {
					t.Fatalf("%s: dispatch %v %+v", c.id, err, real)
				}
				mt, _ := mcpqual.ReplayTranscript(records(t, raw, distinguish[c.id]))
				fam, sel := reg.Decoder(c.decoder)(mt), dec(mt)
				if fam.Inconclusive == "" && c.id == "grok" || sel.Inconclusive == "" && c.id != "grok" || fam.Inconclusive == sel.Inconclusive && slices.Equal(fam.Events, sel.Events) {
					t.Fatalf("%s: the selected decoder %+v is not distinct from the family parser %+v", c.id, sel, fam)
				}
				// A test registry keeps the override.
				w := reg.WithVersion(c.decoder, mcpqual.DecoderVersion{Version: "test-only", Fixture: c.decoder + "/synthetic"})
				if wd, _, err := w.Select(c.decoder, c.version); err != nil || !slices.Equal(wd(tr).Events, real.Events) {
					t.Fatalf("%s: WithVersion lost the override", c.id)
				}
				for _, near := range []string{strings.Fields(c.version)[0], c.version + " ", strings.ToUpper(c.version)} {
					if _, _, err := reg.Select(c.decoder, near); err == nil {
						t.Fatalf("%s: near version %q selected", c.id, near)
					}
				}
			}
			if _, _, err := reg.Select("cursor-jsonl", "2026.10.01-e373342"); err == nil {
				t.Fatal("a Cursor real version is selectable")
			}
		}},
	})
}

// exportHelperBinary is the test helper process calling the exported
// command runner with an injected test policy (testdata/fixtureexport).
func exportHelperBinary(t *testing.T) string {
	return fixture(t, "fixtureexport", func(dir string) (string, error) {
		return testkit.BuildBinaryAt(dir, "./tests/function/testdata/fixtureexport", "fixtureexport")
	})
}

// exportBinary is the production maintainer command.
func exportBinary(t *testing.T) string {
	return fixture(t, "mcpfixture-export", func(dir string) (string, error) {
		return testkit.BuildBinaryAt(dir, "./cmd/mcpfixture-export", "mcpfixture-export")
	})
}

// fabricatedCapture is a real complete capture of the fake Claude vendor
// emitting the real Claude format with metadata (the fake vendor, the real
// probe), and the helper environment accepting it as a source.
func fabricatedCapture(t *testing.T, settings map[string]string) (string, []string) {
	t.Helper()
	q := newQualEnv(t)
	out := filepath.Join(realTemp(t), "capture")
	s := map[string]string{"FORMAT": "claude-real"}
	for k, v := range settings {
		s[k] = v
	}
	r := q.capture(q.capturePlan(map[string]map[string]string{"claude": s}, "claude"), out, nil, "--allow-model-calls")
	if r.code != 0 {
		t.Fatalf("fabricated capture = %+v", r)
	}
	q.groupsGone()
	b := bundle(t, out)
	// The helper's placement check observes the test's temporary root and
	// the repository (the DW10 boundary): a host marker above them (a
	// sandbox's /tmp/.git) never decides these cases, while every marker
	// they plant, and the repository's own .git, is real.
	roots := filepath.Dir(out) + string(os.PathListSeparator) + testkit.MustRepoRoot(t)
	if d, err := filepath.EvalSymlinks(filepath.Dir(t.TempDir())); err == nil {
		roots = d + string(os.PathListSeparator) + roots
	}
	env := []string{"FIXTURE_EXPORT_CLIENT=claude", "FIXTURE_EXPORT_VERSION=" + captureVersions["claude"], "FIXTURE_EXPORT_PLATFORM=" + b.Manifest.OS + "/" + b.Manifest.Arch,
		"FIXTURE_EXPORT_RUN=" + b.Manifest.RunID, "FIXTURE_EXPORT_SHA256=" + sha(b.ManifestBytes), "FIXTURE_EXPORT_ROOTS=" + roots}
	return out, env
}

// treeBytes reads every regular file below dir by slash path.
func treeBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	return out
}

// fixtureRepo is a temporary repository holding a copy of the production
// enrollment tree (index and bundles).
func fixtureRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(realTemp(t), "repo")
	copyTree(t, filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(mcpqual.EnrollmentRoot)), filepath.Join(repo, filepath.FromSlash(mcpqual.EnrollmentRoot)))
	return repo
}

// reindex rewrites entry client's expected.json or receipt in repo with
// edit and makes the index consistent with the new bytes.
func reindex(t *testing.T, repo, client, name string, edit func(m map[string]any)) {
	t.Helper()
	ib, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(mcpqual.EnrollmentIndexPath)))
	idx, err := mcpqual.ParseEnrollmentIndex(ib)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range idx.Entries {
		if e.Client != client {
			continue
		}
		p := filepath.Join(repo, filepath.FromSlash(e.Bundle), name)
		var m map[string]any
		b, _ := os.ReadFile(p)
		json.Unmarshal(b, &m)
		edit(m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		os.WriteFile(p, nb, 0o600)
		h := sha(nb)
		if name == mcpqual.ExpectedName {
			idx.Entries[i].ExpectedSHA256 = h
		} else {
			idx.Entries[i].SanitizationSHA256 = &h
		}
	}
	nb, _ := json.MarshalIndent(idx, "", "  ")
	os.WriteFile(filepath.Join(repo, filepath.FromSlash(mcpqual.EnrollmentIndexPath)), nb, 0o600)
}

// FP-18: deterministic export through the command runner (a test helper
// process with an injected policy on a fabricated bundle; the production
// command refuses that bundle), protected bytes and receipts, the public
// provenance chain of the checked-in fixtures, unsafe inputs and outputs,
// and the owner gate: no oracle or attestation from the exporter, and
// pending or placeholder attestations refused by the production validator.
func TestMCPFixtureSanitization(t *testing.T) {
	t.Parallel()
	validate := func(repo string) error {
		_, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: mcpqual.DefaultRegistry()})
		return err
	}
	runInventory(t, 5, []string{"deterministic-export", "protected-bytes", "provenance-chain", "unsafe-input", "owner-gate"}, []captureCase{
		{"deterministic-export", func(t *testing.T) {
			src, env := fabricatedCapture(t, nil)
			dir := realTemp(t)
			for _, name := range []string{"a", "b"} {
				r := runBin(t, exportHelperBinary(t), dir, env, "--source", src, "--out", filepath.Join(dir, name))
				if r.code != 0 || !strings.Contains(r.stdout, "candidate export complete") {
					t.Fatalf("export %s = %+v", name, r)
				}
			}
			a, b := treeBytes(t, filepath.Join(dir, "a")), treeBytes(t, filepath.Join(dir, "b"))
			if len(a) != 11 || len(a) != len(b) {
				t.Fatalf("exports hold %d and %d files", len(a), len(b))
			}
			for p, d := range a {
				if !bytes.Equal(d, b[p]) {
					t.Fatalf("%s differs between two exports", p)
				}
			}
			// The production command accepts only the compiled pins.
			r := runBin(t, exportBinary(t), dir, nil, "--source", src, "--out", filepath.Join(dir, "prod"))
			if r.code != 1 || !strings.Contains(r.stderr, "not an accepted source identity") {
				t.Fatalf("production command on a fabricated bundle = %+v", r)
			}
			if _, err := os.Stat(filepath.Join(dir, "prod", mcpqual.CaptureManifestName)); err == nil {
				t.Fatal("a refused export published a manifest")
			}
			if r := runBin(t, exportBinary(t), dir, nil, "--source", src); r.code != 2 {
				t.Fatalf("missing --out = %+v", r)
			}
		}},
		{"protected-bytes", func(t *testing.T) {
			src, env := fabricatedCapture(t, nil)
			out := filepath.Join(realTemp(t), "out")
			if r := runBin(t, exportHelperBinary(t), filepath.Dir(out), env, "--source="+src, "--out="+out); r.code != 0 {
				t.Fatalf("export = %+v", r)
			}
			sb := bundle(t, src)
			eb := bundle(t, out, mcpqual.SanitizationName)
			vendor := mcpqual.ClientFile("claude", mcpqual.FileVendorEvents)
			for p, d := range sb.Files {
				if p != vendor && !bytes.Equal(d, eb.Files[p]) {
					t.Fatalf("%s changed", p)
				}
			}
			st, _ := mcpqual.ReplayTranscript(sb.Files[vendor])
			et, _ := mcpqual.ReplayTranscript(eb.Files[vendor])
			if len(st.Lines) != 1 || len(et.Lines) != 1 || st.Lines[0].OffsetNS != et.Lines[0].OffsetNS {
				t.Fatal("record count or offset changed")
			}
			// The session_id and cwd metadata are replaced; the tool_use, the
			// tool result and the terminal discriminants are kept.
			e := string(et.Lines[0].Data)
			for _, keep := range []string{`"name":"mcp__probe__slow"`, `"tool_use_id":"toolu_F"`, `"terminal_reason":"completed"`, `"subtype":"success"`} {
				if !strings.Contains(e, keep) {
					t.Fatalf("protected %s lost", keep)
				}
			}
			if strings.Contains(e, `"session_id":"s"`) || strings.Contains(e, `"cwd":"w"`) || !strings.Contains(e, `"session_id":"[fixture metadata]"`) {
				t.Fatalf("metadata not replaced: %s", e)
			}
			// Amendment A1 (policy v2): the rate-limit map becomes {} and only
			// the cost numbers become the metadata string; the event type,
			// token counts and cost basis stay.
			for _, w := range []string{`"type":"rate_limit_event","rate_limit_info":{}`, `"total_cost_usd":"[fixture metadata]"`, `"costUSD":"[fixture metadata]"`,
				`"inputTokens":6`, `"costBasis":"list"`} {
				if !strings.Contains(e, w) {
					t.Fatalf("policy v2 export lacks %s: %s", w, e)
				}
			}
			if strings.Contains(e, "utilization") || strings.Contains(e, "0.1227972") || !strings.Contains(string(repoFileAt(t, out, mcpqual.SanitizationName)), `"policy": "decoder-enrollment-b2-metadata-v2"`) {
				t.Fatalf("a declined value survived or the policy is not v2: %s", e)
			}
			// The receipt verifies from the exported bytes alone; so do the
			// checked-in fixtures' receipts under the production policy.
			pol := mcpqual.FixturePolicyForTests(mcpqual.FixtureSource{Client: "claude", Version: captureVersions["claude"], Platform: sb.Manifest.OS + "/" + sb.Manifest.Arch,
				RunID: sb.Manifest.RunID, ManifestSHA256: sha(sb.ManifestBytes)})
			if _, err := mcpqual.CheckFixtureSanitization(pol, eb, repoFileAt(t, out, mcpqual.SanitizationName)); err != nil {
				t.Fatal(err)
			}
			for _, c := range b2Clients {
				_, b, _, rb := enrolledBundle(t, c.id)
				san, err := mcpqual.CheckFixtureSanitization(mcpqual.ProductionFixturePolicy(), b, rb)
				tr, _ := mcpqual.ReplayTranscript(b.Files[mcpqual.ClientFile(c.id, mcpqual.FileVendorEvents)])
				var all strings.Builder
				for _, l := range tr.Lines {
					all.Write(l.Data)
				}
				// The checked-in fixtures carry policy v2's rows: no rate-limit
				// state or per-run cost value remains.
				if s := all.String(); strings.Contains(s, "utilization") || strings.Contains(s, "0.1227972") || strings.Contains(s, "0.03165128") ||
					c.id != "codex" && !strings.Contains(s, `"total_cost_usd":"[fixture metadata]"`) || san == nil || san.Policy != "decoder-enrollment-b2-metadata-v2" {
					t.Fatalf("%s fixture is not a policy v2 export", c.id)
				}
				if err != nil || len(san.Replacements) == 0 {
					t.Fatalf("%s receipt: %v", c.id, err)
				}
				// A protected evidence change the receipt cannot explain.
				forged := withTranscript(b, c.id, bytes.Replace(b.Files[mcpqual.ClientFile(c.id, mcpqual.FileVendorEvents)], []byte(`-capture-setup`), []byte(`-capture-setuq`), 1))
				if _, err := mcpqual.CheckFixtureSanitization(mcpqual.ProductionFixturePolicy(), forged, rb); err == nil {
					t.Fatalf("%s: a changed transcript kept its receipt", c.id)
				}
			}
		}},
		{"provenance-chain", func(t *testing.T) {
			repo := fixtureRepo(t)
			if err := validate(repo); err != nil {
				t.Fatalf("production fixtures: %v", err)
			}
			for name, tc := range map[string]struct {
				client, file string
				edit         func(m map[string]any)
				want         string
			}{
				"source-hash":     {"codex", mcpqual.SanitizationName, func(m map[string]any) { m["source_manifest_sha256"] = strings.Repeat("0", 64) }, "compiled source pin"},
				"replacement":     {"grok", mcpqual.SanitizationName, func(m map[string]any) { m["replacements"] = m["replacements"].([]any)[1:] }, "applicable replacements"},
				"protected":       {"claude", mcpqual.SanitizationName, func(m map[string]any) { m["protected_sha256"] = strings.Repeat("1", 64) }, "protected_sha256"},
				"oracle-policy":   {"codex", mcpqual.ExpectedName, func(m map[string]any) { delete(m, "sanitization_policy") }, "sanitization_policy"},
				"oracle-source":   {"grok", mcpqual.ExpectedName, func(m map[string]any) { m["source_capture_sha256"] = strings.Repeat("2", 64) }, "source manifest hash"},
				"exported-hash":   {"claude", mcpqual.SanitizationName, func(m map[string]any) { m["exported_manifest_sha256"] = strings.Repeat("3", 64) }, "exported_manifest_sha256"},
				"unknown-member":  {"codex", mcpqual.SanitizationName, func(m map[string]any) { m["note"] = "x" }, "unknown field"},
				"oracle-event":    {"claude", mcpqual.ExpectedName, func(m map[string]any) { m["events"].([]any)[0].(map[string]any)["monotonic_offset"] = 1 }, "replay"},
				"oracle-unpolicy": {"grok", mcpqual.ExpectedName, func(m map[string]any) { m["sanitization_policy"] = nil }, "nonempty string"},
				// Amendment A1: the superseded policy v1 is refused in the
				// receipt and in the oracle.
				"receipt-v1": {"claude", mcpqual.SanitizationName, func(m map[string]any) { m["policy"] = "decoder-enrollment-b2-metadata-v1" }, "policy"},
				"oracle-v1":  {"codex", mcpqual.ExpectedName, func(m map[string]any) { m["sanitization_policy"] = "decoder-enrollment-b2-metadata-v1" }, "sanitization_policy"},
			} {
				repo := fixtureRepo(t)
				reindex(t, repo, tc.client, tc.file, tc.edit)
				if err := validate(repo); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s: %v", name, err)
				}
			}
			// The index's receipt hash and an unlisted receipt.
			repo = fixtureRepo(t)
			ib := filepath.Join(repo, filepath.FromSlash(mcpqual.EnrollmentIndexPath))
			b, _ := os.ReadFile(ib)
			os.WriteFile(ib, bytes.Replace(b, []byte(`"sanitization_sha256": "`), []byte(`"sanitization_sha256": "f`), 1), 0o600)
			if err := validate(repo); err == nil {
				t.Fatal("a wrong receipt hash validated")
			}
		}},
		{"unsafe-input", func(t *testing.T) {
			src, env := fabricatedCapture(t, nil)
			dir := realTemp(t)
			run := func(args ...string) result { return runBin(t, exportHelperBinary(t), dir, env, args...) }
			os.Mkdir(filepath.Join(dir, "exists"), 0o700)
			os.Symlink(dir, filepath.Join(dir, "link"))
			for name, tc := range map[string]struct {
				args []string
				code int
				want string
			}{
				"existing-out":  {[]string{"--source", src, "--out", filepath.Join(dir, "exists")}, 1, "must not exist"},
				"inside-source": {[]string{"--source", src, "--out", filepath.Join(src, "x")}, 1, "inside the source tree"},
				"symlink-out":   {[]string{"--source", src, "--out", filepath.Join(dir, "link", "x")}, 1, "symbolic link"},
				"work-tree":     {[]string{"--source", src, "--out", filepath.Join(testkit.MustRepoRoot(t), "tests", "x-export")}, 1, "Git work tree"},
				"relative":      {[]string{"--source", "capture", "--out", filepath.Join(dir, "r")}, 2, "absolute"},
				"repeated":      {[]string{"--source", src, "--source", src, "--out", filepath.Join(dir, "r")}, 2, "repeated"},
				"unknown-flag":  {[]string{"--source", src, "--out", filepath.Join(dir, "r"), "--policy", "x"}, 2, "unknown argument"},
			} {
				if r := run(tc.args...); r.code != tc.code || !strings.Contains(r.stderr, tc.want) {
					t.Fatalf("%s = %+v", name, r)
				}
			}
			// A source with an extra or a linked payload.
			bad := filepath.Join(dir, "bad")
			copyTree(t, src, bad)
			os.WriteFile(filepath.Join(bad, "clients", "claude", "extra.txt"), nil, 0o600)
			if r := run("--source", bad, "--out", filepath.Join(dir, "bad-out")); r.code != 1 || !strings.Contains(r.stderr, "not a valid capture bundle") {
				t.Fatalf("extra payload = %+v", r)
			}
			linked := filepath.Join(dir, "linked")
			copyTree(t, src, linked)
			p := filepath.Join(linked, "clients", "claude", mcpqual.FileVersionStderr)
			os.Remove(p)
			os.Symlink("/etc/hostname", p)
			if r := run("--source", linked, "--out", filepath.Join(dir, "linked-out")); r.code != 1 {
				t.Fatalf("linked payload = %+v", r)
			}
			// Amendment A1: a cost member of another type than the observed
			// number is refused (never coerced), with no manifest written.
			wsrc, wenv := fabricatedCapture(t, map[string]string{"COST": "string"})
			wout := filepath.Join(dir, "wrong-type-out")
			if r := runBin(t, exportHelperBinary(t), dir, wenv, "--source", wsrc, "--out", wout); r.code != 1 || !strings.Contains(r.stderr, "/total_cost_usd has an unexpected type") {
				t.Fatalf("a string cost member = %+v", r)
			}
			if _, err := os.Stat(filepath.Join(wout, mcpqual.CaptureManifestName)); err == nil {
				t.Fatal("a refused export published a manifest")
			}
			if _, err := os.Stat(filepath.Join(testkit.MustRepoRoot(t), "tests", "x-export")); err == nil {
				t.Fatal("an export was written inside the work tree")
			}
		}},
		{"owner-gate", func(t *testing.T) {
			// The exporter writes no oracle, attestation or index.
			src, env := fabricatedCapture(t, nil)
			out := filepath.Join(realTemp(t), "out")
			if r := runBin(t, exportHelperBinary(t), filepath.Dir(out), env, "--source", src, "--out", out); r.code != 0 {
				t.Fatalf("export = %+v", r)
			}
			for p := range treeBytes(t, out) {
				if path.Base(p) == mcpqual.ExpectedName || path.Base(p) == "index.json" {
					t.Fatalf("the exporter wrote %s", p)
				}
			}
			// A coder-authored candidate with empty handles, or any
			// placeholder, is refused by the production validator; only the
			// authorized exact handles satisfy the structural rule. These
			// strings are fake evidence for the rule, never authorization.
			for name, a := range map[string][2]string{"pending": {"", ""}, "reviewer-only": {"", mcpqual.B2ReviewerHandle}, "whitespace": {" ", " "},
				"todo": {"TODO", "TODO"}, "tbd": {"TBD", "TBD"}, "angle": {"<owner>", "<reviewer>"}, "example": {"example", "example"}, "test": {"test-owner", "test-reviewer"}} {
				repo := fixtureRepo(t)
				reindex(t, repo, "grok", mcpqual.ExpectedName, func(m map[string]any) {
					at := m["attestation"].(map[string]any)
					at["owner"], at["reviewer"] = a[0], a[1]
				})
				if err := validate(repo); err == nil || !strings.Contains(err.Error(), "attestation") {
					t.Fatalf("%s: %v", name, err)
				}
			}
			if err := mcpqual.CheckB2Attestation(mcpqual.Attestation{Owner: mcpqual.B2OwnerHandle, Reviewer: mcpqual.B2ReviewerHandle, Policy: mcpqual.CaptureRedactionPolicy}); err != nil {
				t.Fatal(err)
			}
			// The runbook's handback and resume protocol.
			guide := string(repoFile(t, mcpqual.SetupDocPath))
			for _, w := range []string{"`STATUS: BLOCKED` with the literal reason `awaiting independent fixture verification and explicit owner approval`",
				"authors `expected.json` from the raw captured bytes with a derivation trace and both attestation handles empty (never from decoder output)",
				"`verification.md`", "`owner-authorization.md`", "`finalization.md`", "`/tmp/mcpqual-b2-review/<handoff-id>/<client>/`",
				"not personal execution of the CLI or manual validation of every byte", "Empty, whitespace-only and placeholder attestations never validate",
				"go run ./cmd/mcpfixture-export --source ABSOLUTE_BUNDLE --out ABSOLUTE_NEW_STAGING_DIRECTORY"} {
				if !strings.Contains(guide, w) {
					t.Fatalf("the guide lacks %q", w)
				}
			}
		}},
	})
}

// repoFileAt reads dir/name.
func repoFileAt(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FP-19: the production index enrolls exactly the three linux/amd64
// identities and validates offline (Cursor absent); bounded corruptions
// are refused; the evidence qualifies only linux/amd64 and the success
// capabilities; the legacy synthetic versions and injected legacy
// registries are unchanged.
func TestMCPRealEnrollment(t *testing.T) {
	t.Parallel()
	runInventory(t, 4, []string{"production-inventory", "corruption", "platform-capabilities", "legacy-synthetic"}, []captureCase{
		{"production-inventory", func(t *testing.T) {
			idx, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{Root: testkit.MustRepoRoot(t), Registry: mcpqual.DefaultRegistry()})
			if err != nil || len(idx.Entries) != 3 {
				t.Fatalf("production enrollment: %v", err)
			}
			for i, c := range []int{2, 0, 1} { // sorted by fixture: claude-json, codex-jsonl, grok-json
				w := b2Clients[c]
				e := idx.Entries[i]
				if e.Client != w.id || e.Version != w.version || e.Platform != "linux/amd64" || path.Base(e.Bundle) != w.run ||
					e.Fixture != mcpqual.EnrolledFixtureID(w.decoder, w.version, "linux/amd64") || e.SanitizationSHA256 == nil {
					t.Fatalf("entry %d %+v", i, e)
				}
			}
			if q := mcpqual.DefaultRegistry().QualifiedVersions(); len(q) != 3 || slices.ContainsFunc(q, func(s string) bool { return strings.HasPrefix(s, "cursor") }) {
				t.Fatalf("qualified %v", q)
			}
			for _, e := range idx.Entries {
				if e.Client == "cursor" {
					t.Fatal("a Cursor entry is enrolled")
				}
			}
		}},
		{"corruption", func(t *testing.T) {
			validate := func(repo string) error {
				_, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: mcpqual.DefaultRegistry()})
				return err
			}
			e := productionEntry(t, "codex")
			for name, mutate := range map[string]func(repo, dir string){
				"manifest": func(_, dir string) {
					rewrite(t, filepath.Join(dir, mcpqual.CaptureManifestName), `"harness_version": "`, `"harness_version": "x`)
				},
				"payload": func(_, dir string) {
					appendFile(t, filepath.Join(dir, "clients", "codex", mcpqual.FileServerEvents), "\n")
				},
				"extra":   func(_, dir string) { os.WriteFile(filepath.Join(dir, "notes.md"), nil, 0o600) },
				"missing": func(_, dir string) { os.Remove(filepath.Join(dir, mcpqual.SanitizationName)) },
				"orphan": func(repo, _ string) {
					ib := filepath.Join(repo, filepath.FromSlash(mcpqual.EnrollmentIndexPath))
					b, _ := os.ReadFile(ib)
					var idx map[string]any
					json.Unmarshal(b, &idx)
					es := idx["entries"].([]any)
					idx["entries"] = es[:len(es)-1]
					nb, _ := json.MarshalIndent(idx, "", "  ")
					os.WriteFile(ib, nb, 0o600)
				},
				"no-receipt": func(repo, _ string) {
					reindex(t, repo, "codex", mcpqual.ExpectedName, func(m map[string]any) {})
					ib := filepath.Join(repo, filepath.FromSlash(mcpqual.EnrollmentIndexPath))
					b, _ := os.ReadFile(ib)
					var idx mcpqual.EnrollmentIndex
					json.Unmarshal(b, &idx)
					for i := range idx.Entries {
						if idx.Entries[i].Client == "codex" {
							idx.Entries[i].SanitizationSHA256 = nil
						}
					}
					nb, _ := json.MarshalIndent(idx, "", "  ")
					os.WriteFile(ib, nb, 0o600)
				},
			} {
				repo := fixtureRepo(t)
				mutate(repo, filepath.Join(repo, filepath.FromSlash(e.Bundle)))
				if err := validate(repo); err == nil {
					t.Fatalf("%s: a corrupted enrollment validated", name)
				}
			}
		}},
		{"platform-capabilities", func(t *testing.T) {
			success := []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}
			for _, c := range b2Clients {
				_, v, err := mcpqual.DefaultRegistry().Select(c.decoder, c.version)
				if err != nil || len(v.MissingCapabilities("linux/amd64", success...)) != 0 {
					t.Fatalf("%s: %v %+v", c.id, err, v)
				}
				for _, p := range []string{"darwin/amd64", "darwin/arm64", "linux/arm64"} {
					if len(v.MissingCapabilities(p, success...)) != 3 {
						t.Fatalf("%s qualified on %s", c.id, p)
					}
				}
				for _, k := range []string{mcpqual.CapMCPTimeout, mcpqual.CapToolError, mcpqual.CapPermissionDenied, mcpqual.CapAuthError} {
					if len(v.MissingCapabilities("linux/amd64", k)) != 1 {
						t.Fatalf("%s qualified %s", c.id, k)
					}
				}
				// The Linux bytes replay on this host too: a parser check only.
				e, b, o, _ := enrolledBundle(t, c.id)
				if err := mcpqual.Replay(mcpqual.DefaultRegistry(), e, b, o); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"legacy-synthetic", func(t *testing.T) {
			reg := mcpqual.DefaultRegistry()
			for name, version := range map[string]string{"claude-json": "2.1.282 (Claude Code)", "codex-jsonl": "codex-cli 0.156.1", "grok-json": "grok 1.0.41 (4220f3b224a6) [stable]",
				"cursor-jsonl": "2026.09.23-86fc751"} {
				if _, v, err := reg.Select(name, version); err != nil || v.Qualified || v.Fixture != name+"/synthetic" || len(v.Evidence) != 0 {
					t.Fatalf("%s %+v", name, v)
				}
			}
			// The legacy fake enrollment validates against its injected
			// registry, and an empty index against the synthetic one.
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(sharedEnrollment(t).repo), Registry: sharedEnrollment(t).reg}); err != nil {
				t.Fatal(err)
			}
			empty := filepath.Join(realTemp(t), "empty")
			mkdir(t, filepath.Join(empty, filepath.FromSlash(mcpqual.EnrollmentRoot)))
			os.WriteFile(filepath.Join(empty, filepath.FromSlash(mcpqual.EnrollmentIndexPath)), []byte(`{"schema":"mcpqual-enrollment-v1","entries":[]}`), 0o600)
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(empty), Registry: mcpqual.SyntheticRegistry()}); err != nil {
				t.Fatal(err)
			}
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(empty), Registry: reg}); err == nil {
				t.Fatal("an empty index backed the production registry")
			}
		}},
	})
}

// cursorPermissionPlan is the trusted Cursor capture plan of the pinned
// adapter with fake settings.
func cursorPermissionPlan(q *qualEnv, settings map[string]string) map[string]any {
	s := map[string]string{"ENABLE": "permission-check,workspace", "PERMISSION": "require", "EXPECT_HOME": q.home}
	for k, v := range settings {
		s[k] = v
	}
	return cursorPlan(q, s)
}

// FP-20: the scoped Cursor permission file of the pinned adapter: the fake
// CLI sees exactly the file before enable and at its session, the capture
// completes with a verified record and no broader permission; a rejected
// tool with a final success envelope stays partial; mutations after the
// enable or during the session fail closed; other versions and platforms
// get no file; an unsafe pre-existing path launches nothing.
func TestMCPCaptureCursorToolPermission(t *testing.T) {
	t.Parallel()
	// pinned runs the production capture runner in process with the real
	// fake-vendor and probe processes, the adapter's platform injected
	// through the runner's GOOS/GOARCH (linux/amd64 on every host, never a
	// production override), and validates the bundle from disk.
	pinned := func(t *testing.T, q *qualEnv, settings map[string]string) (result, *mcpqual.CaptureBundle, mcpqual.CaptureClient) {
		out := filepath.Join(realTemp(t), "out")
		m := inProcessRun(t, q, cursorPermissionPlan(q, settings), out, nil)
		b := bundle(t, out)
		return result{code: m.ExitCode()}, b, captureClient(t, b.Manifest, "cursor")
	}
	runInventory(t, 5, []string{"scoped-file", "rejected-tool", "mutation", "unsupported-adapter", "unsafe-path"}, []captureCase{
		{"scoped-file", func(t *testing.T) {
			q := newQualEnv(t)
			ownerCursor(t, q)
			r, b, c := pinned(t, q, nil)
			tp := c.ToolPermission
			if r.code != 0 || c.State != mcpqual.CaptureComplete || tp == nil || tp.State != mcpqual.PermissionVerified || tp.Adapter != mcpqual.CursorToolPermissionAdapter ||
				tp.Content != `{"permissions":{"allow":["Mcp(probe:slow)"]}}` || tp.Path != "<workspace>/.cursor/cli.json" || tp.SHA256 != sha([]byte(tp.Content)) {
				t.Fatalf("capture = %+v %+v %+v", r, tp, c.Reason)
			}
			recs := q.argvLog()
			if len(recs) != 1 || recs[0].Permission != "600 "+mcpqual.CursorToolPermissionContent || filepath.Base(recs[0].Cwd) != "cursor-capture-setup" ||
				b.Manifest.OS != "linux" || b.Manifest.Arch != "amd64" {
				t.Fatalf("session saw %+v", recs)
			}
			noBlanket(t, recs[0].Argv)
			// Baseline, not evidence: the approval changes never list it, and
			// nothing was written under the owner's home.
			for _, ch := range c.Approval.Changes {
				if strings.HasSuffix(ch.Path, "cli.json") {
					t.Fatalf("the harness file is approval evidence: %+v", c.Approval.Changes)
				}
			}
			filepath.Walk(q.home, func(p string, info os.FileInfo, err error) error {
				if err == nil && (info.Name() == "cli.json" || info.Name() == "permissions.json") {
					t.Fatalf("a home permission file %s", p)
				}
				return nil
			})
			q.groupsGone()
		}},
		{"rejected-tool", func(t *testing.T) {
			q := newQualEnv(t)
			ownerCursor(t, q)
			r, _, c := pinned(t, q, map[string]string{"SCENARIO": "rejected"})
			if r.code != 5 || c.State != mcpqual.CapturePartial || deref(c.Reason) != mcpqual.ReasonProbeNotObserved || c.ToolPermission.State != mcpqual.PermissionVerified {
				t.Fatalf("rejected tool = %+v %q %+v", r, deref(c.Reason), c.ToolPermission)
			}
			q.groupsGone()
		}},
		{"mutation", func(t *testing.T) {
			for name, tc := range map[string]struct {
				settings map[string]string
				sessions int
			}{
				"after-enable":   {map[string]string{"ENABLE": "permission-check,workspace,permission-modify"}, 0},
				"during-session": {map[string]string{"SESSION_PERMISSION": "remove"}, 1},
			} {
				q := newQualEnv(t)
				ownerCursor(t, q)
				r, _, c := pinned(t, q, tc.settings)
				if r.code != 5 || !strings.HasPrefix(deref(c.Reason), mcpqual.ReasonCursorScopeUnverified) || len(q.launchLog()["session"]) != tc.sessions ||
					c.ToolPermission == nil || c.ToolPermission.State != mcpqual.PermissionWritten {
					t.Fatalf("%s = %+v %q %+v", name, r, deref(c.Reason), c.ToolPermission)
				}
				q.groupsGone()
			}
		}},
		{"unsupported-adapter", func(t *testing.T) {
			// Another platform (injected through the runner's GOOS/GOARCH,
			// never a production override) and another version: no file, no
			// record, the session sees none.
			for name, tc := range map[string]struct {
				version string
				mutate  func(c *mcpqual.CaptureRunner)
			}{
				"darwin":  {captureVersions["cursor"], func(c *mcpqual.CaptureRunner) { c.GOOS = "darwin" }},
				"arm64":   {captureVersions["cursor"], func(c *mcpqual.CaptureRunner) { c.GOARCH = "arm64" }},
				"version": {"2026.10.02-0000000", nil},
			} {
				q := newQualEnv(t)
				ownerCursor(t, q)
				plan := cursorPermissionPlan(q, map[string]string{"PERMISSION": "absent", "ENABLE": "workspace", "VERSION": tc.version})
				plan["clients"].([]any)[0].(map[string]any)["expected_version"] = tc.version
				m := inProcessRun(t, q, plan, filepath.Join(realTemp(t), "out"), tc.mutate)
				c := captureClient(t, m, "cursor")
				recs := q.argvLog()
				if c.ToolPermission != nil || len(recs) != 1 || recs[0].Permission != "<absent>" || c.State != mcpqual.CaptureComplete {
					t.Fatalf("%s: %+v %q %+v", name, c.ToolPermission, deref(c.Reason), recs)
				}
			}
		}},
		{"unsafe-path", func(t *testing.T) {
			for name, want := range map[string]string{"cli-json": "a cli.json already exists", "cursor-link": "not an ordinary directory"} {
				q := newQualEnv(t)
				ownerCursor(t, q)
				r, _, c := pinned(t, q, map[string]string{"PLANT": name})
				launched := q.launchLog()
				if r.code != 5 || !strings.Contains(deref(c.Reason), want) || len(launched["enable"]) != 0 || len(launched["session"]) != 0 || c.ToolPermission != nil {
					t.Fatalf("%s = %+v %q %v", name, r, deref(c.Reason), launched)
				}
				q.groupsGone()
			}
		}},
	})
}

// realQualify is one in-process qualification of the clocked fake vendor
// of client in its enrolled real format under the production registry, the
// exact enrolled version and fixture, on goos/amd64 (injected runner
// labels), with timeoutMS the vendor's silent limit: the specified short
// confirmation schedule (setup, one silent 15000 ms call and its repeat, at
// most three sessions, 120 s per case, 360 s per client), driven on an
// injected fake clock without sleeps (design decoder-enrollment B2, FP-21).
// It returns the report and the evidence directory holding report.md.
func realQualify(t *testing.T, client, timeoutMS, goos string, mutate func(c map[string]any)) (*mcpqual.Report, string, error) {
	t.Helper()
	var w struct{ decoder, version string }
	for _, c := range b2Clients {
		if c.id == client {
			w.decoder, w.version = c.decoder, c.version
		}
	}
	q := newQualEnv(t)
	exe := "/fake/" + client
	c := map[string]any{"id": client, "executable": exe, "expected_version": w.version, "version_argv": []string{"--version"}, "driver": "direct",
		"decoder": w.decoder, "decoder_fixture": mcpqual.EnrolledFixtureID(w.decoder, w.version, "linux/amd64"),
		"session": map[string]any{"argv": []string{"--config", "{config}", "--case", "{case}", "-p", "{prompt}"}},
		"config":  map[string]any{"default": map[string]any{"path": "probe-mcp.json", "content": probeConfig}},
		"env": map[string]string{"FAKE_VENDOR_FORMAT": client + "-real", "FAKE_VENDOR_VERSION": w.version, "FAKE_VENDOR_TIMEOUT_MS": timeoutMS,
			"FAKE_VENDOR_CLIENT_NAME": client + "-cli", "FAKE_VENDOR_CLIENT_VERSION": "1.0.0", "FAKE_VENDOR_TOKEN": "1"},
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
	clock := testkit.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	const server = "/fake/mcpqual"
	out := filepath.Join(realTemp(t), "out")
	r := &mcpqual.Runner{Plan: p, PlanSHA256: sha(b), OutDir: out, GOOS: goos, GOARCH: "amd64", Hostname: "function-host", ServerPath: server,
		BaseEnv: q.env(""), Launcher: &clockedVendor{t: t, clock: clock, server: server}, Reaper: clockedReaper{}, Clock: clock,
		Registry: mcpqual.DefaultRegistry(), Log: io.Discard, RunID: "run-real-" + client, Nonce: "noncereal" + client, CaptureDate: "2026-10-08",
		HarnessVersion: mcpqual.HarnessVersion, Home: q.home,
		HashFile: func(p string) (string, error) {
			if p != exe {
				return "", os.ErrNotExist
			}
			return sha([]byte(exe)), nil
		}}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep, out, nil
}

// clockedGuard bounds, in real time, each wait of the clocked fake vendor
// for the probe (a reply, or the probe arming its delay timer). It bounds
// the test's waits only; no measured time passes through it.
const clockedGuard = 30 * time.Second

// clockedVendor is the FP-21 fake vendor launcher: each launch is an
// in-process session reading its FAKE_VENDOR_* settings from the launch
// environment. A version launch prints FAKE_VENDOR_VERSION. A session reads
// the plan-rendered probe configuration, runs the real probe command
// (mcpqual serve, in process, on the run's fake clock) over pipes, speaks
// MCP to it (initialize, initialized, tools/list, one tools/call of its own
// case), and prints the client's reviewed real format for its own case and
// the probe's result. Time moves only here: after observing the probe's
// armed delay timer, the session advances the clock by the case's delay,
// or by its silent limit when the delay reaches that limit (it then cancels
// the call and prints an unseen timeout shape).
type clockedVendor struct {
	t      *testing.T
	clock  *testkit.FakeClock
	server string
	mu     sync.Mutex
	n      int
}

// clockedProc is one in-process launch.
type clockedProc struct {
	pgid   int
	out    *io.PipeReader
	exited chan struct{}
	exit   int
}

func (p *clockedProc) PGID() int               { return p.pgid }
func (p *clockedProc) Stdout() io.Reader       { return p.out }
func (p *clockedProc) Exited() <-chan struct{} { return p.exited }
func (p *clockedProc) Status() (*int, *string) { e := p.exit; return &e, nil }
func (p *clockedProc) CloseStdout()            { p.out.Close() }

func (v *clockedVendor) Start(spec mcpqual.ProcSpec) (mcpqual.Proc, error) {
	v.mu.Lock()
	v.n++
	p := &clockedProc{pgid: 1_000_000 + v.n, exited: make(chan struct{})}
	v.mu.Unlock()
	settings := map[string]string{}
	for _, kv := range spec.Env {
		if k, val, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "FAKE_VENDOR_") {
			settings[strings.TrimPrefix(k, "FAKE_VENDOR_")] = val
		}
	}
	r, w := io.Pipe()
	p.out = r
	go func() {
		var transcript string
		var err error
		if slices.Contains(spec.Args, "--version") {
			transcript = settings["VERSION"] + "\n"
		} else {
			transcript, err = v.session(spec, settings)
		}
		if err != nil {
			p.exit = 1
			if spec.StderrPath != "" {
				os.WriteFile(spec.StderrPath, []byte("fake vendor: "+err.Error()+"\n"), 0o600)
			}
		}
		io.WriteString(w, transcript)
		w.Close()
		close(p.exited)
	}()
	return p, nil
}

// session runs one MCP session against the in-process real probe.
func (v *clockedVendor) session(spec mcpqual.ProcSpec, settings map[string]string) (string, error) {
	arg := func(flag string, args []string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	caseID := arg("--case", spec.Args)
	raw, err := os.ReadFile(arg("--config", spec.Args))
	if err != nil {
		return "", err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", err
	}
	srv := cfg.MCPServers["probe"]
	if srv.Command != v.server || len(srv.Args) == 0 || srv.Args[0] != "serve" {
		return "", errors.New("the configuration does not name the probe server")
	}
	cfRaw, err := os.ReadFile(arg("--case-file", srv.Args))
	if err != nil {
		return "", err
	}
	cf, err := mcpqual.ParseCaseFile(cfRaw)
	if err != nil {
		return "", err
	}
	var delay int64
	for _, pc := range cf.Cases {
		if pc.CaseID == caseID {
			delay = pc.DelayMS
		}
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	served := make(chan int, 1)
	go func() {
		code := mcpqual.Main(context.Background(), mcpqual.Env{Args: srv.Args, Stdin: inR, Stdout: outW, Stderr: &stderr, Clock: v.clock})
		outW.Close()
		served <- code
	}()
	lines := make(chan map[string]json.RawMessage, 64)
	go func() {
		defer close(lines)
		br := bufio.NewReader(outR)
		for {
			l, err := br.ReadBytes('\n')
			var m map[string]json.RawMessage
			if len(l) > 0 && json.Unmarshal(l, &m) == nil {
				lines <- m
			}
			if err != nil {
				return
			}
		}
	}()
	// end closes the probe's input and waits for the serve command (its
	// output drained) before the session reports.
	end := func(sessionErr error) error {
		inW.Close()
		for range lines {
		}
		if code := <-served; code != 0 && sessionErr == nil {
			sessionErr = fmt.Errorf("the probe exited %d: %s", code, stderr.String())
		}
		return sessionErr
	}
	send := func(m any) {
		b, _ := json.Marshal(m)
		inW.Write(append(b, '\n'))
	}
	guard := time.NewTimer(clockedGuard)
	defer guard.Stop()
	await := func(id string) (map[string]json.RawMessage, error) {
		for {
			select {
			case m, ok := <-lines:
				if !ok {
					return nil, io.ErrUnexpectedEOF
				}
				if string(m["id"]) == id {
					return m, nil
				}
			case <-guard.C:
				return nil, errors.New("no probe reply " + id + " in real time")
			}
		}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": mcpqual.ProtocolVersion,
		"capabilities": map[string]any{}, "clientInfo": map[string]any{"name": settings["CLIENT_NAME"], "version": settings["CLIENT_VERSION"]}}})
	if _, err := await("1"); err != nil {
		return "", end(err)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if _, err := await("2"); err != nil {
		return "", end(err)
	}
	params := map[string]any{"name": "slow", "arguments": map[string]any{"case_id": caseID}}
	if settings["TOKEN"] == "1" {
		params["_meta"] = map[string]any{"progressToken": "tok-" + caseID}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": params})
	if delay > 0 {
		// The probe has received the call once its delay timer is armed.
		if err := v.clock.AwaitWaiter(clockedGuard, testkit.HasTimer(time.Duration(delay)*time.Millisecond)); err != nil {
			return "", end(err)
		}
		if limit, _ := strconv.ParseInt(settings["TIMEOUT_MS"], 10, 64); limit > 0 && delay >= limit {
			v.clock.Advance(time.Duration(limit) * time.Millisecond)
			send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 3, "reason": "timeout"}})
			return clockedTranscript(settings["FORMAT"], "timeout", caseID, ""), end(nil)
		}
		v.clock.Advance(time.Duration(delay) * time.Millisecond)
	}
	res, err := await("3")
	if err != nil {
		return "", end(err)
	}
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(res["result"], &result)
	if len(result.Content) != 1 {
		return "", end(errors.New("the probe result has no single text block"))
	}
	return clockedTranscript(settings["FORMAT"], "success", caseID, result.Content[0].Text), end(nil)
}

// clockedTranscript renders the reviewed real format of an enrolled
// version (the fake vendor binary's codex-real, grok-real and claude-real
// shapes) for this session's own case and result: success carries the
// structured result plus prose echoes of it; a timeout is an unseen,
// unenrolled shape (a failed tool item, an ErrOutput, an is_error result).
func clockedTranscript(format, outcome, caseID, text string) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	prose := "The tool call may have timed out; MCP error -32001 is a timeout."
	switch format {
	case "codex-real":
		args := `"arguments":{"case_id":` + q(caseID) + `}`
		lines := []string{`{"type":"thread.started","thread_id":"t-fake"}`, `{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":` + q(prose) + `}}`,
			`{"type":"item.started","item":{"id":"item_1","type":"mcp_tool_call","server":"probe","tool":"slow",` + args + `,"result":null,"error":null,"status":"in_progress"}}`}
		if outcome == "timeout" {
			lines = append(lines, `{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"probe","tool":"slow",`+args+
				`,"result":null,"error":{"message":"timed out"},"status":"failed"}}`)
		} else {
			lines = append(lines, `{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"probe","tool":"slow",`+args+
				`,"result":{"content":[{"type":"text","text":`+q(text)+`}],"structured_content":null},"error":null,"status":"completed"}}`,
				`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":`+q(text)+`}}`)
		}
		return strings.Join(append(lines, `{"type":"turn.completed","usage":{"input_tokens":1}}`), "\n") + "\n"
	case "grok-real":
		lines := []string{`{"type":"available_commands","tools":["use_tool","probe__slow"],"commands":["compact"]}`, `{"type":"thought","data":` + q(prose) + `}`,
			`{"type":"tool_call","toolCallId":"call-fake-0","title":"use_tool","kind":"use_tool","status":"pending","toolName":"use_tool","rawInput":{"tool_name":"probe__slow","tool_input":{"case_id":` +
				q(caseID) + `}},"content":[],"locations":[]}`,
			`{"type":"tool_call_update","toolCallId":"call-fake-0","status":null,"content":[],"rawOutput":null,"locations":[]}`}
		if outcome == "timeout" {
			lines = append(lines, `{"type":"tool_call_update","toolCallId":"call-fake-0","status":"failed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"ErrOutput":"timed out"}},"locations":[]}`,
				`{"type":"end","stopReason":"end_turn","sessionId":"s"}`)
		} else {
			lines = append(lines, `{"type":"tool_call_update","toolCallId":"call-fake-0","status":"completed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"OkayOutput":`+
				q(text)+`}},"locations":[]}`, `{"type":"text","data":`+q(text)+`}`, `{"type":"usage","usage":{"input_tokens":1},"signature":"sig"}`,
				`{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r","total_cost_usd":0.03165128,"total_cost_usd_ticks":316512800,`+
					`"modelUsage":{"grok-4.7-build":{"modelCalls":2,"costUSD":0.03165128}}}`)
		}
		return strings.Join(lines, "\n") + "\n"
	}
	call := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":` + q(prose) + `},{"type":"tool_use","id":"toolu_F","name":"mcp__probe__slow","input":{"case_id":` + q(caseID) + `}}]}}`
	end := `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":` + q(text) + `,"total_cost_usd":0.1227972` +
		`,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":6,"costUSD":0.1227972,"costBasis":"list"}}}`
	msgs := []string{`{"type":"system","subtype":"init","cwd":"w","session_id":"s","tools":["mcp__probe__slow"]}`, call}
	if outcome == "timeout" {
		msgs = append(msgs, `{"type":"user","message":{"content":[{"tool_use_id":"toolu_F","type":"tool_result","is_error":true,"content":[{"type":"text","text":"MCP error -32001: Request timed out"}]}]}}`, end)
	} else {
		msgs = append(msgs, `{"type":"user","message":{"content":[{"tool_use_id":"toolu_F","type":"tool_result","content":[{"type":"text","text":`+q(text)+`}]}]}}`,
			`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","utilization":0.25,"resetsAt":1791470400},"uuid":"u"}`, end)
	}
	return "[" + strings.Join(msgs, ",") + "]\n"
}

// clockedReaper reaps an in-process launch: its session has ended (and its
// in-process probe with it) once Exited is closed, so nothing remains.
type clockedReaper struct{}

func (clockedReaper) Reap(p mcpqual.Proc) mcpqual.CaseCleanup {
	guard := time.NewTimer(clockedGuard)
	defer guard.Stop()
	select {
	case <-p.Exited():
		return mcpqual.CaseCleanup{GroupGone: true}
	case <-guard.C:
		e := "cleanup: the in-process session did not end"
		return mcpqual.CaseCleanup{Error: &e}
	}
}

// entryRepo is a scratch repository whose catalog entry id has version
// and platform.
func entryRepo(t *testing.T, id, version, platform string) (string, *mcpqual.CatalogBase) {
	t.Helper()
	repo := qualRepo(t)
	p := filepath.Join(repo, mcpqual.CatalogJSONPath)
	es, err := catalog.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range es {
		if es[i].ID == id {
			es[i].Version, es[i].Platform = version, platform
		}
	}
	b, _ := mcpqual.RenderCatalog(es)
	os.WriteFile(p, b, 0o644)
	base, err := mcpqual.ReadCatalogBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, base
}

// FP-21: the 15 s confirmation path for the enrolled clients with the
// fake vendor emitting the three reviewed formats (its own case and
// nonce) and the real probe, on the specified schedule driven by the
// injected clock without sleeps (realQualify): the repeated successful
// 15000 ms bound under the production registry and its compatibility
// decision; unseen outcomes and unbacked platforms stay UNVERIFIED; exact
// identity and opt-in publication; the documented delivery order and links.
func TestMCPRealEnrollmentConfirmation(t *testing.T) {
	t.Parallel()
	var codexRep *mcpqual.Report
	var codexOut string
	runInventory(t, 4, []string{"repeated-bound", "unverified-outcome", "identity-and-publication", "delivery-runbook"}, []captureCase{
		{"repeated-bound", func(t *testing.T) {
			for _, c := range b2Clients {
				rep, out, err := realQualify(t, c.id, "0", "linux", nil)
				if err != nil {
					t.Fatal(err)
				}
				cr := reportClient(t, rep, c.id)
				def := reportPhase(t, cr, mcpqual.PhaseDefault)
				if setup := reportPhase(t, cr, mcpqual.PhaseSetup); rep.Outcome != mcpqual.StatusConclusive || setup.Status != mcpqual.StatusConclusive ||
					ms(def.LowerBoundMS) != 15000 || def.UpperBoundMS != nil || def.Observations != 2 || len(def.Cases) != 2 {
					t.Fatalf("%s: %s %+v", c.id, rep.Outcome, def)
				}
				for _, cs := range def.Cases {
					if cs.Outcome != mcpqual.KindToolResult || cs.DelayMS != 15000 {
						t.Fatalf("%s case %+v", c.id, cs)
					}
				}
				// The resulting compatibility decision, end to end: the
				// repeated 15 s bound on a qualified linux/amd64 decoder is
				// compatible with the shipping budget (10s + max(2s, 1.5s) =
				// 12s < 15s), stated as a lower bound in the written report.
				decision := mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, cr)
				md := string(repoFileAt(t, out, "report.md"))
				if !strings.Contains(decision, "compatible for this measured tuple: L = 15s is the longest silent call that completed (a lower bound, not the timeout): 10s + max(2s, 1.5s) = 12s < 15s.") ||
					strings.Contains(decision, "UNVERIFIED") || !strings.Contains(md, "- "+decision+"\n") || !strings.Contains(md, "(> 15000 ms tested)") {
					t.Fatalf("%s decision %q in\n%s", c.id, decision, md)
				}
				if c.id == "codex" {
					codexRep, codexOut = rep, out
				}
			}
		}},
		{"unverified-outcome", func(t *testing.T) {
			// The vendor's timeout is an unseen, unenrolled format: never a
			// typed timeout, never incompatible, never VERIFIED.
			rep, out, _ := realQualify(t, "grok", "10000", "linux", nil)
			cr := reportClient(t, rep, "grok")
			def := reportPhase(t, cr, mcpqual.PhaseDefault)
			if def.Status != mcpqual.StatusInconclusive || def.LowerBoundMS != nil || def.UpperBoundMS != nil ||
				!strings.Contains(mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, cr), "compatibility UNVERIFIED") {
				t.Fatalf("unseen timeout: %+v", def)
			}
			_, base := entryRepo(t, "grok", mcpqual.GrokRealVersion, "linux/amd64")
			if p, err := mcpqual.ProposePatch(rep, base, out); err == nil {
				for _, fc := range p.Facts {
					if fc.Final.Status == catalog.Verified {
						t.Fatalf("%s VERIFIED from an unseen outcome", fc.Key)
					}
				}
			}
			// Linux evidence never qualifies darwin: the setup is already an
			// unverified decoder event, so no bound is measured.
			rep, _, _ = realQualify(t, "claude", "0", "darwin", nil)
			setup := reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseSetup)
			def = reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseDefault)
			if rep.Outcome == mcpqual.StatusConclusive || setup.Status != mcpqual.StatusInconclusive || !strings.Contains(deref(setup.Reason), mcpqual.ReasonUnverifiedEvent) ||
				def.LowerBoundMS != nil {
				t.Fatalf("darwin: %+v %+v", setup, def)
			}
		}},
		{"identity-and-publication", func(t *testing.T) {
			// The plan names the enrolled fixture ID; a synthetic label for an
			// enrolled version is refused before any model session.
			rep, _, err := realQualify(t, "codex", "0", "linux", func(c map[string]any) { c["decoder_fixture"] = "codex-jsonl/synthetic" })
			if err != nil || rep.Outcome == mcpqual.StatusConclusive || !strings.Contains(deref(reportClient(t, rep, "codex").Reason), mcpqual.ReasonDecoderFixture) {
				t.Fatalf("a synthetic fixture for an enrolled version: %v %+v", err, rep)
			}
			// An observed version other than the enrolled one never runs a
			// session.
			rep, _, err = realQualify(t, "claude", "0", "linux", func(c map[string]any) { c["env"].(map[string]string)["FAKE_VENDOR_VERSION"] = "2.1.293 (Claude Code)" })
			if err != nil || rep.Outcome == mcpqual.StatusConclusive || !strings.Contains(deref(reportClient(t, rep, "claude").Reason), mcpqual.ReasonVersionMismatch) {
				t.Fatalf("version mismatch: %v %+v", err, rep)
			}
			// Publication is opt-in and exact: a catalog at another version
			// conflicts (exit 4); the matching one publishes only a lower bound.
			repo, base := entryRepo(t, "codex", "codex-cli 0.156.1", "linux/amd64")
			cp := filepath.Join(realTemp(t), "copy")
			copyTree(t, codexOut, cp)
			if _, err := mcpqual.ProposePatch(codexRep, base, cp); err == nil {
				t.Fatal("a version conflict proposed a patch")
			}
			repo, base = entryRepo(t, "codex", mcpqual.CodexRealVersion, "linux/amd64")
			if _, err := mcpqual.ProposePatch(codexRep, base, codexOut); err != nil {
				t.Fatal(err)
			}
			if err := mcpqual.Publish(codexOut, repo); err != nil {
				t.Fatal(err)
			}
			if f := repoFacts(t, repo)["codex"].Facts["mcp_timeout"]; f.Status != catalog.Verified || !strings.Contains(f.Value, "this is a lower bound, not the default") {
				t.Fatalf("published %+v", f)
			}
		}},
		{"delivery-runbook", func(t *testing.T) {
			guide := string(repoFile(t, mcpqual.SetupDocPath))
			if strings.Contains(guide, "mcpqual ships synthetic decoder fixtures only") {
				t.Fatal("the synthetic-only sentence remains")
			}
			idx, err := mcpqual.ParseEnrollmentIndex(repoFile(t, mcpqual.EnrollmentIndexPath))
			if err != nil || len(idx.Entries) != 3 {
				t.Fatalf("index %v", err)
			}
			for _, e := range idx.Entries {
				if !strings.Contains(guide, "](../"+e.Bundle+")") || !strings.Contains(guide, "`"+e.Fixture+"`") {
					t.Fatalf("enrolled %s is not linked with its fixture ID", e.Fixture)
				}
			}
			// The ordered delivery gates.
			order := []string{"Mid-implementation handback", "Independent verification and explicit owner acceptance", "Final offline acceptance",
				"then the explicitly selected codex-reviewer reviews the code", "Owner Cursor scoped-permission recapture", "Cursor follow-up", "Owner short-confirmation gate", "Optional publication gate"}
			at := -1
			for _, w := range order {
				i := strings.Index(guide, w)
				if i <= at {
					t.Fatalf("%q is missing or out of order", w)
				}
				at = i
			}
			for _, w := range []string{"Cursor blocker", "never holds the other clients' enrollment hostage", "approval does not transfer from the capture workspace",
				"`qualify` writes no permission file and reports no `tool_permission` field", "`12eb3e81114588aca3b7998f4f19e8997b056aca08e57a7ca7c8a3ec8c652aad`",
				"`41626a53292324140b92556b9d42ff5542e3dcd04aff85eafb8689dd4adb44fc`", "`a967e7b1d8b4e47ee421d5433027880347952b0c0857abf880e2c942a4ec93b3`",
				"never an edited `expected_version`", "run no concurrent Claude Code session", "10s + max(2s, 1.5s) = 12s < 15s"} {
				if !strings.Contains(guide, w) {
					t.Fatalf("the guide lacks %q", w)
				}
			}
			catalogMD := string(repoFile(t, mcpqual.CatalogMDPath))
			for _, w := range []string{"Decoder enrollment B2 enrolls exactly three linux/amd64 identities", "Cursor Agent stays UNVERIFIED", "`codex-cli 0.160.0`"} {
				if !strings.Contains(catalogMD, w) {
					t.Fatalf("support-catalog.md lacks %q", w)
				}
			}
		}},
	})
}
