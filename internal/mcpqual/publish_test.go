package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// FP-14 unit tests: canonical catalog rendering, VERIFIED only from a
// qualified decoder on the entry's platform, partial and mixed facts
// UNVERIFIED, worker facts untouched, redacted evidence hashes, the base
// hash and prior-value conflicts, the three Markdown anchors and
// retry-safe installation.

// tempRepo copies the catalog, its Markdown and the captured CLI evidence
// into a scratch repository.
func tempRepo(t *testing.T) string {
	t.Helper()
	root := testkit.MustRepoRoot(t)
	repo := t.TempDir()
	copyFile := func(rel string) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(repo, rel)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The two catalog files are writable copies; the captured evidence and
	// the setup guide are read-only links to the real repository.
	copyFile(CatalogJSONPath)
	copyFile(CatalogMDPath)
	for _, rel := range []string{SetupDocPath, filepath.Join("tests", "testdata", "cli-help")} {
		if err := os.Symlink(filepath.Join(root, rel), filepath.Join(repo, rel)); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func qualifiedRegistry() Registry {
	return Registry{"claude-json": {versions: []DecoderVersion{{Version: "2.1.282 (Claude Code)", Fixture: "claude-json/actual-test", Qualified: true}}, decode: decodeClaude}}
}

// Shared fixture runs: a completed run used only as publication input is
// built once per test process (not once per repetition) in a package
// directory removed by TestMain. Callers never modify it in place; tests
// that tamper with evidence copy it first.
var (
	sharedMu   sync.Mutex
	sharedDir  string
	sharedRuns = map[string]string{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDir != "" {
		os.RemoveAll(sharedDir)
	}
	os.Exit(code)
}

// cachedRun returns the evidence directory of a completed run of plan p
// against model m (with the qualified decoder when q) and a fresh copy of
// its report. key must identify m, p, q and mutate.
func cachedRun(t *testing.T, key string, m vendorModel, p *Plan, q bool, mutate func(*Runner)) (string, *Report) {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	out, ok := sharedRuns[key]
	if !ok {
		if sharedDir == "" {
			d, err := os.MkdirTemp("", "mcpqual-shared-runs-")
			if err != nil {
				t.Fatal(err)
			}
			sharedDir = d
		}
		if q {
			p.Clients[0].DecoderFixture = "claude-json/actual-test"
		}
		r := newRunner(t, newWorld(t, m), p)
		r.OutDir = filepath.Join(sharedDir, strconv.Itoa(len(sharedRuns)))
		if q {
			r.Registry = qualifiedRegistry()
		}
		if mutate != nil {
			mutate(r)
		}
		runPlan(t, r, context.Background())
		out = r.OutDir
		sharedRuns[key] = out
	}
	b, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	return out, &rep
}

// copyRun copies a shared run's evidence directory for a test that
// modifies it.
func copyRun(t *testing.T, from string) string {
	t.Helper()
	to := filepath.Join(t.TempDir(), "out")
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
	return to
}

// publishRun proposes the patch of a shared run of (m, p, q) against repo.
func publishRun(t *testing.T, repo string, m vendorModel, p *Plan, q bool) (*Runner, *Report, *Patch, error) {
	t.Helper()
	base, err := ReadCatalogBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("%+v|%s|%v", m, planJSON(t, p), q)
	out, rep := cachedRun(t, key, m, p, q, nil)
	patch, err := ProposePatch(rep, base, out)
	return &Runner{OutDir: out}, rep, patch, err
}

func loadFacts(t *testing.T, repo string) map[string]catalog.Entry {
	t.Helper()
	es, err := catalog.Load(filepath.Join(repo, CatalogJSONPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(repo, es); err != nil {
		t.Fatal(err)
	}
	out := map[string]catalog.Entry{}
	for _, e := range es {
		out[e.ID] = e
	}
	return out
}

func TestRenderCatalogCanonical(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	b, _ := os.ReadFile(filepath.Join(root, CatalogJSONPath))
	es, err := catalog.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := RenderCatalog(es); err != nil || !bytes.Equal(r, b) {
		t.Fatal("the checked-in catalog is not in canonical form")
	}
	es[0].Facts["zz_extra"] = es[0].Facts["model"]
	if r, _ := RenderCatalog(es); !bytes.Contains(r, []byte(`"zz_extra"`)) {
		t.Fatal("an extra fact was dropped")
	}
	repo := tempRepo(t)
	if _, err := ReadCatalogBase(repo); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(){
		"relative": func() {},
		"non-canonical": func() {
			os.WriteFile(filepath.Join(repo, CatalogJSONPath), bytes.ReplaceAll(b, []byte("  "), []byte("\t")), 0o644)
		},
		"invalid":     func() { os.WriteFile(filepath.Join(repo, CatalogJSONPath), []byte(`{}`), 0o644) },
		"no-markdown": func() { os.Remove(filepath.Join(repo, CatalogMDPath)) },
		"no-json":     func() { os.Remove(filepath.Join(repo, CatalogJSONPath)) },
	} {
		mutate()
		dir := repo
		if name == "relative" {
			dir = "relative/repo"
		}
		if _, err := ReadCatalogBase(dir); contract.ExitCode(err) != 2 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// withoutInterim removes the 07a interim sentence from the repo's
// mcp_timeout values (the state after a future design decision ends the
// exception), keeping the catalog canonical and valid.
func withoutInterim(t *testing.T, repo string) {
	t.Helper()
	p := filepath.Join(repo, CatalogJSONPath)
	es, err := catalog.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range es {
		f := es[i].Facts["mcp_timeout"]
		f.Value = strings.TrimSuffix(f.Value, " "+InterimSentence)
		es[i].Facts["mcp_timeout"] = f
	}
	b, _ := RenderCatalog(es)
	os.WriteFile(p, b, 0o644)
}

// W2: while the 07a interim exception is in the catalog contract, a
// VERIFIED mcp_timeout is refused (exit 4) and nothing is installed.
func TestPublishRefusesVerifiedTimeoutUnderInterim(t *testing.T) {
	repo := tempRepo(t)
	jsonBefore, _ := os.ReadFile(filepath.Join(repo, CatalogJSONPath))
	_, _, _, err := publishRun(t, repo, fullModel(), planWith(defaultOnly(900)), true)
	if contract.ExitCode(err) != 4 || !strings.Contains(err.Error(), "interim exception") {
		t.Fatalf("a VERIFIED mcp_timeout under the interim contract: %v", err)
	}
	jsonAfter, _ := os.ReadFile(filepath.Join(repo, CatalogJSONPath))
	if !bytes.Equal(jsonBefore, jsonAfter) {
		t.Fatal("the refused proposal changed the catalog")
	}
	if _, err := os.Stat(filepath.Join(repo, EvidenceRoot)); err == nil {
		t.Fatal("evidence installed by a refused proposal")
	}
}

// VERIFIED rendering and installation, once the interim exception has been
// ended by a design decision (simulated by removing its sentence).
func TestPublishVerified(t *testing.T) {
	repo := tempRepo(t)
	withoutInterim(t, repo)
	before := loadFacts(t, repo)
	baseMD, _ := os.ReadFile(filepath.Join(repo, CatalogMDPath))
	r, rep, patch, err := publishRun(t, repo, fullModel(), fullPlan(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(r.OutDir, repo); err != nil {
		t.Fatal(err)
	}
	after := loadFacts(t, repo)
	c := after["claude"]
	for _, key := range TimeoutKeys {
		f := c.Facts[key]
		low := strings.ToLower(f.Value)
		if f.Status != catalog.Verified || f.VerificationIteration != "07b" || strings.Contains(low, "unknown") || strings.Contains(low, "unverified") ||
			!strings.Contains(f.Value, "linux/amd64") || !strings.Contains(f.Value, "2.1.282 (Claude Code)") || f.Evidence[len(f.Evidence)-1] != path.Join(EvidenceRoot, "run-1", "report.json") {
			t.Fatalf("%s = %+v", key, f)
		}
	}
	if v := c.Facts["mcp_timeout"].Value; !strings.Contains(v, "after 500 ms") || !strings.Contains(v, "300 ms (2 observations") {
		t.Fatal(v)
	}
	if v := c.Facts["mcp_timeout_override"].Value; !strings.Contains(v, "tool_timeout_ms = 1000") || !strings.Contains(v, "timed out after 1000 ms") {
		t.Fatal(v)
	}
	if v := c.Facts["mcp_progress_extension"].Value; !strings.Contains(v, "after 1200 ms (the observed absolute cap)") {
		t.Fatal(v)
	}
	// mcp_config records the exact clientInfo, stays UNVERIFIED and 08.
	if f := c.Facts["mcp_config"]; f.Status != catalog.Unverified || f.VerificationIteration != "08" || !strings.Contains(f.Value, `clientInfo name "claude-code" and version "2.1.282" (requested_by compatible: true)`) {
		t.Fatalf("mcp_config %+v", f)
	}
	// Worker facts and every other vendor are untouched.
	for id, e := range before {
		for key, f := range e.Facts {
			if (id == "claude" && (contains(TimeoutKeys, key) || key == "mcp_config")) || sameFact(f, after[id].Facts[key]) {
				continue
			}
			t.Fatalf("%s.%s changed", id, key)
		}
	}
	md, _ := os.ReadFile(filepath.Join(repo, CatalogMDPath))
	if strings.Count(string(md), InterimSentence) != 1 || !strings.Contains(string(md), "- **MCP call timeout: VERIFIED.** Measured by qualification run run-1") {
		t.Fatal("markdown bullets not rewritten or the interim sentence duplicated")
	}
	if err := catalog.CheckDocLinks(filepath.Join(repo, CatalogMDPath)); err != nil {
		t.Fatal(err)
	}
	// Only the three claude timeout bullets differ in the Markdown.
	changed := 0
	bl, al := strings.Split(string(baseMD), "\n"), strings.Split(string(md), "\n")
	for i := range bl {
		if bl[i] != al[i] {
			changed++
			if !strings.HasPrefix(bl[i], "- **") || i < 25 || i > 40 {
				t.Fatalf("line %d changed: %q", i, bl[i])
			}
		}
	}
	if changed != 3 || len(bl) != len(al) {
		t.Fatalf("%d Markdown lines changed", changed)
	}
	// Installed evidence is byte-identical to the report's hashes.
	for _, f := range patch.Files {
		b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(f.Dest)))
		if err != nil || sha256Hex(b) != f.SHA256 {
			t.Fatalf("%s: %v", f.Dest, err)
		}
	}
	if rep.Clients[0].DecoderVersion.Fixture != "claude-json/actual-test" {
		t.Fatal("qualified registry not used")
	}
	// Publishing again is a no-op (every file already holds its bytes).
	if err := Publish(r.OutDir, repo); err != nil {
		t.Fatal(err)
	}
}

func TestPublishSyntheticStaysUnverified(t *testing.T) {
	repo := tempRepo(t)
	r, _, _, err := publishRun(t, repo, fullModel(), planWith(defaultOnly(900)), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(r.OutDir, repo); err != nil {
		t.Fatal(err)
	}
	c := loadFacts(t, repo)["claude"]
	for _, key := range TimeoutKeys {
		f := c.Facts[key]
		if f.Status != catalog.Unverified || !strings.Contains(f.Value, "synthetic fixture") || !strings.Contains(f.Value, HarnessVerified) || !strings.Contains(f.Value, "iteration 07b") {
			t.Fatalf("fake evidence published as %s: %s", f.Status, f.Value)
		}
	}
	if !strings.HasSuffix(c.Facts["mcp_timeout"].Value, " "+InterimSentence) {
		t.Fatal("the interim sentence was lost")
	}
}

func TestPublishPartialAndMixed(t *testing.T) {
	shared := tempRepo(t) // for subtests that only propose a patch
	withoutInterim(t, shared)
	t.Run("lower-bound-only", func(t *testing.T) {
		_, rep, patch, err := publishRun(t, shared, vendorModel{}, planWith(Phases{Default: &DefaultPhase{DelaysMS: []int64{100}}, Override: &DelayPhase{DelayMS: 600}, Progress: &struct{}{}}), true)
		if err != nil || rep.ExitCode() != 5 {
			t.Fatalf("exit %d: %v", rep.ExitCode(), err)
		}
		final := map[string]catalog.Fact{}
		for _, fc := range patch.Facts {
			final[fc.Key] = fc.Final
		}
		if f := final["mcp_timeout"]; f.Status != catalog.Verified || !strings.Contains(f.Value, "lower bound, not the default") {
			t.Fatalf("mcp_timeout %+v", f)
		}
		for _, key := range []string{"mcp_timeout_override", "mcp_progress_extension"} {
			if f := final[key]; f.Status != catalog.Unverified || !strings.Contains(f.Value, "not_run") {
				t.Fatalf("%s %+v", key, f)
			}
		}
	})
	t.Run("no-cap-observed", func(t *testing.T) {
		repo := shared
		_, rep, patch, err := publishRun(t, repo, vendorModel{timeoutMS: 500, raisedMS: 1000, resets: true}, planWith(Phases{Default: &DefaultPhase{DelaysMS: []int64{900}}, Progress: &struct{}{}, Absolute: &DelayPhase{DelayMS: 1500}}), true)
		if err != nil {
			t.Fatal(err)
		}
		var f catalog.Fact
		for _, fc := range patch.Facts {
			if fc.Key == "mcp_progress_extension" {
				f = fc.Final
			}
		}
		if rep.Outcome != StatusConclusive || f.Status != catalog.Unverified || !strings.Contains(f.Value, "no absolute cap observed up to 1500 ms, so the maximum remains UNVERIFIED") {
			t.Fatalf("progress fact %+v", f)
		}
	})
	t.Run("does-not-extend", func(t *testing.T) {
		repo := shared
		_, _, patch, err := publishRun(t, repo, vendorModel{timeoutMS: 500, raisedMS: 1000}, planWith(Phases{Default: &DefaultPhase{DelaysMS: []int64{900}}, Progress: &struct{}{}, Absolute: &DelayPhase{DelayMS: 1500}}), true)
		if err != nil {
			t.Fatal(err)
		}
		for _, fc := range patch.Facts {
			if fc.Key == "mcp_progress_extension" && (fc.Final.Status != catalog.Verified || !strings.Contains(fc.Final.Value, "did not extend")) {
				t.Fatalf("%+v", fc.Final)
			}
		}
	})
	t.Run("other-platform", func(t *testing.T) {
		repo := shared
		base, _ := ReadCatalogBase(repo)
		p := planWith(defaultOnly(900))
		p.Clients[0].DecoderFixture = "claude-json/actual-test"
		r := newRunner(t, newWorld(t, fullModel()), p)
		r.Registry, r.GOOS, r.GOARCH = qualifiedRegistry(), "darwin", "arm64"
		rep := runPlan(t, r, context.Background())
		patch, err := ProposePatch(rep, base, r.OutDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, fc := range patch.Facts {
			if contains(TimeoutKeys, fc.Key) && (fc.Final.Status != catalog.Unverified || !strings.Contains(fc.Final.Value, "observed on darwin/arm64, which does not certify linux/amd64")) {
				t.Fatalf("macOS evidence certified linux: %+v", fc.Final)
			}
		}
	})
	t.Run("client-that-never-ran", func(t *testing.T) {
		repo := shared
		p := planWith(defaultOnly(900))
		p.Clients[0].Executable = "/missing/claude"
		_, _, patch, err := publishRun(t, repo, fullModel(), p, false)
		if err != nil || len(patch.Facts) != 0 {
			t.Fatalf("an absent client changed facts: %+v %v", patch, err)
		}
	})
	t.Run("incompatible-client-info", func(t *testing.T) {
		repo := shared
		m := fullModel()
		m.clientName = "Claude Code"
		_, _, patch, err := publishRun(t, repo, m, planWith(Phases{}), true)
		if err != nil {
			t.Fatal(err)
		}
		for _, fc := range patch.Facts {
			if fc.Key == "mcp_config" && (fc.Final.Status != catalog.Unverified || !strings.Contains(fc.Final.Value, `"Claude Code"`) || !strings.Contains(fc.Final.Value, "dispatch attribution fails")) {
				t.Fatalf("%+v", fc.Final)
			}
		}
	})
}

func TestPublishRefusals(t *testing.T) {
	t.Run("cleanup-failed", func(t *testing.T) {
		repo := tempRepo(t)
		base, _ := ReadCatalogBase(repo)
		r := newRunner(t, newWorld(t, fullModel()), planWith(Phases{}))
		r.Reaper = &fakeReaper{fail: func(int) bool { return true }}
		rep := runPlan(t, r, context.Background())
		if _, err := ProposePatch(rep, base, r.OutDir); contract.ExitCode(err) != 5 {
			t.Fatalf("published after a cleanup failure: %v", err)
		}
	})
	t.Run("version-mismatch", func(t *testing.T) {
		repo := tempRepo(t)
		base, _ := ReadCatalogBase(repo)
		base.Entries[0].Version = "3.0.0 (Claude Code)"
		r := newRunner(t, newWorld(t, fullModel()), planWith(Phases{}))
		rep := runPlan(t, r, context.Background())
		if _, err := ProposePatch(rep, base, r.OutDir); contract.ExitCode(err) != 4 || !strings.Contains(err.Error(), "version") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("markdown-anchors", func(t *testing.T) {
		for name, edit := range map[string]func(string) string{
			"missing-bullet": func(s string) string {
				return strings.Replace(s, "- **How to raise it: UNVERIFIED.** `MCP_TOOL_TIMEOUT`", "- How to raise it: `MCP_TOOL_TIMEOUT`", 1)
			},
			"duplicate-bullet": func(s string) string {
				return strings.Replace(s, "- **MCP call timeout: UNVERIFIED.** Do not", "- **MCP call timeout: x\n- **MCP call timeout: UNVERIFIED.** Do not", 1)
			},
			"missing-section": func(s string) string { return strings.Replace(s, "## Claude Code\n", "## Claude\n", 1) },
		} {
			repo := tempRepo(t)
			md := filepath.Join(repo, CatalogMDPath)
			b, _ := os.ReadFile(md)
			os.WriteFile(md, []byte(edit(string(b))), 0o644)
			if _, _, _, err := publishRun(t, repo, fullModel(), planWith(Phases{}), false); contract.ExitCode(err) != 4 {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
	t.Run("tampered-evidence", func(t *testing.T) {
		repo := tempRepo(t)
		base, _ := ReadCatalogBase(repo)
		r := newRunner(t, newWorld(t, fullModel()), planWith(Phases{}))
		rep := runPlan(t, r, context.Background())
		os.WriteFile(filepath.Join(r.OutDir, "cases", "claude-setup", "vendor-events.jsonl"), []byte("forged\n"), 0o644)
		if _, err := ProposePatch(rep, base, r.OutDir); contract.ExitCode(err) != 4 {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPublishConflictsAndRetry(t *testing.T) {
	// One proposed patch serves every case; the repository is reset to its
	// base bytes (and no installed evidence) between cases.
	repo := tempRepo(t)
	r, _, patch, err := publishRun(t, repo, fullModel(), planWith(Phases{}), false)
	if err != nil {
		t.Fatal(err)
	}
	baseJSON, _ := os.ReadFile(filepath.Join(repo, CatalogJSONPath))
	baseMD, _ := os.ReadFile(filepath.Join(repo, CatalogMDPath))
	patchBytes, _ := os.ReadFile(filepath.Join(r.OutDir, PatchFileName))
	reset := func() {
		os.WriteFile(filepath.Join(repo, CatalogJSONPath), baseJSON, 0o644)
		os.WriteFile(filepath.Join(repo, CatalogMDPath), baseMD, 0o644)
		os.WriteFile(filepath.Join(r.OutDir, PatchFileName), patchBytes, 0o644)
		os.RemoveAll(filepath.Join(repo, EvidenceRoot))
	}
	refused := func(t *testing.T, want string, wantJSON, wantMD []byte) {
		t.Helper()
		if err := Publish(r.OutDir, repo); contract.ExitCode(err) != 4 || !strings.Contains(err.Error(), want) {
			t.Fatalf("want a conflict with %q, got %v", want, err)
		}
		j, _ := os.ReadFile(filepath.Join(repo, CatalogJSONPath))
		md, _ := os.ReadFile(filepath.Join(repo, CatalogMDPath))
		if !bytes.Equal(j, wantJSON) || !bytes.Equal(md, wantMD) {
			t.Fatal("a refused publication modified the catalog")
		}
	}
	noEvidence := func(t *testing.T) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(repo, EvidenceRoot)); err == nil {
			t.Fatal("evidence was installed by a refused publication")
		}
	}
	t.Run("unrelated-json-edit", func(t *testing.T) {
		defer reset()
		edited := bytes.Replace(baseJSON, []byte("Candidate worker use"), []byte("Candidate worker usage"), 1)
		os.WriteFile(filepath.Join(repo, CatalogJSONPath), edited, 0o644)
		refused(t, "changed since the run started", edited, baseMD)
		noEvidence(t)
	})
	t.Run("unrelated-markdown-edit", func(t *testing.T) {
		defer reset()
		edited := append(append([]byte(nil), baseMD...), []byte("\nAn owner's note.\n")...)
		os.WriteFile(filepath.Join(repo, CatalogMDPath), edited, 0o644)
		refused(t, "changed since the run started", baseJSON, edited)
		noEvidence(t)
	})
	t.Run("evidence-destination-differs", func(t *testing.T) {
		defer reset()
		dst := filepath.Join(repo, filepath.FromSlash(patch.Files[0].Dest))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.WriteFile(dst, []byte("someone else's"), 0o644)
		refused(t, "different bytes", baseJSON, baseMD)
		if b, _ := os.ReadFile(dst); string(b) != "someone else's" {
			t.Fatal("conflicting bytes were overwritten")
		}
	})
	t.Run("prior-value-differs", func(t *testing.T) {
		defer reset()
		p := *patch
		p.Facts = append([]FactChange(nil), patch.Facts...)
		p.Facts[0].Prior.Value = "tampered"
		pb, _ := encodeIndent(&p)
		os.WriteFile(filepath.Join(r.OutDir, PatchFileName), pb, 0o644)
		refused(t, "expected prior value", baseJSON, baseMD)
	})
	t.Run("proposed-bytes-differ", func(t *testing.T) {
		md := filepath.Join(r.OutDir, proposedDir, "support-catalog.md")
		orig, _ := os.ReadFile(md)
		defer os.WriteFile(md, orig, 0o644)
		defer reset()
		os.WriteFile(md, []byte("x"), 0o644)
		refused(t, "proposed catalog files", baseJSON, baseMD)
	})
	t.Run("evidence-source-differs", func(t *testing.T) {
		src := filepath.Join(r.OutDir, "report.md")
		orig, _ := os.ReadFile(src)
		defer os.WriteFile(src, orig, 0o644)
		defer reset()
		os.WriteFile(src, []byte("x"), 0o644)
		refused(t, "report.md is missing or differs", baseJSON, baseMD)
		noEvidence(t)
	})
	t.Run("bad-inputs", func(t *testing.T) {
		defer reset()
		if err := Publish(r.OutDir, "relative"); contract.ExitCode(err) != 2 {
			t.Fatal(err)
		}
		if err := Publish(t.TempDir(), repo); contract.ExitCode(err) != 2 || !strings.Contains(err.Error(), "publish only retries a run qualified with --publish-catalog") {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(r.OutDir, PatchFileName), []byte(`{"schema":1,"x":1}`), 0o644)
		if err := Publish(r.OutDir, repo); contract.ExitCode(err) != 2 {
			t.Fatal(err)
		}
	})
	// Interruption after each install step: a retry completes the same
	// intended bytes without overwriting anything else.
	finalJSON, _ := os.ReadFile(filepath.Join(r.OutDir, proposedDir, "support-catalog.json"))
	finalMD, _ := os.ReadFile(filepath.Join(r.OutDir, proposedDir, "support-catalog.md"))
	install := func(src, rel string) {
		b, _ := os.ReadFile(src)
		dst := filepath.Join(repo, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.WriteFile(dst, b, 0o644)
	}
	for _, stop := range []string{"first-evidence", "all-evidence", "json", "all"} {
		t.Run("retry-after-"+stop, func(t *testing.T) {
			defer reset()
			n := len(patch.Files)
			if stop == "first-evidence" {
				n = 1
			}
			for _, f := range patch.Files[:n] {
				install(filepath.Join(r.OutDir, filepath.FromSlash(f.Source)), f.Dest)
			}
			if stop == "json" || stop == "all" {
				install(filepath.Join(r.OutDir, proposedDir, "support-catalog.json"), CatalogJSONPath)
			}
			if stop == "all" {
				install(filepath.Join(r.OutDir, proposedDir, "support-catalog.md"), CatalogMDPath)
			}
			if err := Publish(r.OutDir, repo); err != nil {
				t.Fatal(err)
			}
			j, _ := os.ReadFile(filepath.Join(repo, CatalogJSONPath))
			md, _ := os.ReadFile(filepath.Join(repo, CatalogMDPath))
			if !bytes.Equal(j, finalJSON) || !bytes.Equal(md, finalMD) {
				t.Fatal("the retry did not reach the intended bytes")
			}
			for _, f := range patch.Files {
				if b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(f.Dest))); err != nil || sha256Hex(b) != f.SHA256 {
					t.Fatalf("%s not installed", f.Dest)
				}
			}
		})
	}
	t.Run("recheck-before-rename", func(t *testing.T) {
		dir := t.TempDir()
		err := installFile(dir, "a/b.txt", []byte("x"), func() error { return conflict("moved underneath") })
		if contract.ExitCode(err) != 4 {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(filepath.Join(dir, "a")); len(entries) != 0 {
			t.Fatalf("temporary files left: %v", entries)
		}
	})
}
