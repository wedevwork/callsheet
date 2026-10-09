package mcpqual

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// Design decoder-enrollment UT-5 (index, manifest and oracle integrity,
// registry agreement) and UT-6 (offline replay) with fake fixtures only:
// one enrolled claude bundle built once per test process from a fake
// capture, a hand-written oracle and a test-only qualified registry. No
// fake fixture ever enters the production index (TestProductionEnrollment).

// enrollTranscript is a claude-json setup transcript (the synthetic
// format's paths) carrying the call, the nonce result and success.
const enrollTranscript = `[{"type":"system","subtype":"init"},{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"mcp__probe__slow","input":{"case_id":"claude-capture-setup"}}]}},` +
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[{"type":"text","text":"{\"nonce\":\"noncecap1\",\"case_id\":\"claude-capture-setup\"}"}]}]}},` +
	`{"type":"result","subtype":"success","result":"done"}]`

// enrolled is the shared fake enrollment: a repository view, its entry,
// oracle and registry.
type enrolled struct {
	files  map[string][]byte // repository-relative path -> bytes
	entry  EnrollmentEntry
	oracle *ExpectedOracle
	bundle *CaptureBundle
	reg    Registry
}

var (
	enrolledOnce sync.Once
	enrolledFix  *enrolled
)

// expectedOracle is the hand-authored oracle of enrollTranscript.
func expectedOracle(sourceHash string) *ExpectedOracle {
	return &ExpectedOracle{CaseID: "claude-capture-setup", Nonce: "noncecap1",
		Events: []Event{{CaseID: "claude-capture-setup", Kind: KindToolCall, RequestID: "toolu_1"},
			{CaseID: "claude-capture-setup", Kind: KindToolResult, RequestID: "toolu_1", Nonce: "noncecap1"}},
		Terminal: true, ClientInfo: ExpectedClientInfo{Name: "fake-cli", Version: "1.0.0"}, RequesterCompatible: true, ErrorKinds: []string{},
		Capabilities: []string{CapToolCall, CapToolResult, CapTerminalSuccess}, CaptureState: CaptureComplete, SourceCaptureSHA256: sourceHash,
		Attestation: Attestation{Owner: "owner", Reviewer: "reviewer", Policy: CaptureRedactionPolicy}}
}

func enrolledFixture(t *testing.T) *enrolled {
	t.Helper()
	enrolledOnce.Do(func() {
		sc := cachedCapture(t, "enroll", func(w *capWorld) *CaptureRunner {
			w.script = func(spec ProcSpec) capBehavior {
				b := w.defaults(spec)
				if launchKind(spec) == "session" {
					b.stdout = enrollTranscript + "\n"
				}
				return b
			}
			return newCapRunner(t, w, capPlan(t, "claude"))
		})
		version := capVersions["claude"]
		fixture := EnrolledFixtureID("claude-json", version, "linux/amd64")
		dir := EnrolledBundlePath("claude", version, "linux/amd64", sc.man.RunID)
		o := expectedOracle(sha256Hex(sc.bundle.ManifestBytes))
		ob, _ := encodeIndent(o)
		e := EnrollmentEntry{Client: "claude", Decoder: "claude-json", Version: version, Platform: "linux/amd64", Fixture: fixture, Bundle: dir,
			ManifestSHA256: sha256Hex(sc.bundle.ManifestBytes), ExpectedSHA256: sha256Hex(ob)}
		idx, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{e}})
		files := map[string][]byte{EnrollmentIndexPath: idx, path.Join(dir, CaptureManifestName): sc.bundle.ManifestBytes, path.Join(dir, ExpectedName): ob}
		for p, data := range sc.bundle.Files {
			files[path.Join(dir, p)] = data
		}
		// An injected legacy registry: the synthetic versions and this one
		// fake qualified version (the production registry's real versions
		// are backed only by the production index).
		reg := SyntheticRegistry().WithVersion("claude-json", DecoderVersion{Version: version, Fixture: fixture, Qualified: true,
			Evidence: []DecoderEvidence{{Platform: "linux/amd64", Fixture: fixture, Kinds: []string{CapToolCall, CapToolResult, CapTerminalSuccess}}}})
		enrolledFix = &enrolled{files: files, entry: e, oracle: o, bundle: sc.bundle, reg: reg}
	})
	if enrolledFix == nil {
		t.Fatal("the enrollment fixture was not built")
	}
	return enrolledFix
}

func (e *enrolled) fs(mutate func(map[string][]byte)) fstest.MapFS {
	files := map[string][]byte{}
	for k, v := range e.files {
		files[k] = v
	}
	if mutate != nil {
		mutate(files)
	}
	out := fstest.MapFS{}
	for k, v := range files {
		out[k] = &fstest.MapFile{Data: v, Mode: 0o600}
	}
	return out
}

func TestEnrollmentContract(t *testing.T) {
	e := enrolledFixture(t)
	idx, err := ValidateEnrollment(EnrollmentOptions{FS: e.fs(nil), Registry: e.reg})
	if err != nil || len(idx.Entries) != 1 {
		t.Fatalf("valid enrollment: %v", err)
	}
	if got := e.reg.QualifiedVersions(); len(got) != 1 || got[0] != "claude-json "+capVersions["claude"] {
		t.Fatalf("qualified %v", got)
	}
	dir := e.entry.Bundle
	// Integration negatives through the whole validator.
	for name, tc := range map[string]struct {
		mutate func(map[string][]byte)
		opts   func(*EnrollmentOptions)
		want   string
	}{
		"forged-manifest": {func(f map[string][]byte) {
			f[path.Join(dir, CaptureManifestName)] = bytes.Replace(f[path.Join(dir, CaptureManifestName)], []byte(`"harness_version": "test"`), []byte(`"harness_version": "tset"`), 1)
		}, nil, "forged or altered provenance"},
		"altered-oracle": {func(f map[string][]byte) {
			f[path.Join(dir, ExpectedName)] = append(f[path.Join(dir, ExpectedName)], ' ')
		}, nil, "expected.json's hash differs"},
		"no-index":      {func(f map[string][]byte) { delete(f, EnrollmentIndexPath) }, nil, "enrollment index"},
		"owner-literal": {nil, func(o *EnrollmentOptions) { o.Literals = []string{"fake-cli"} }, "not a fixed point"},
		"platform": {func(f map[string][]byte) {
			moved := EnrolledBundlePath("claude", e.entry.Version, "linux/arm64", path.Base(dir))
			for k, v := range e.files {
				if strings.HasPrefix(k, dir+"/") {
					delete(f, k)
					f[moved+strings.TrimPrefix(k, dir)] = v
				}
			}
			en := e.entry
			en.Platform, en.Fixture, en.Bundle = "linux/arm64", EnrolledFixtureID("claude-json", en.Version, "linux/arm64"), moved
			f[EnrollmentIndexPath], _ = encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{en}})
		}, nil, "indexed as linux/arm64"},
		"version": {func(f map[string][]byte) {
			other := "2.1.292 (Claude Code)"
			moved := EnrolledBundlePath("claude", other, "linux/amd64", path.Base(dir))
			for k, v := range e.files {
				if strings.HasPrefix(k, dir+"/") {
					delete(f, k)
					f[moved+strings.TrimPrefix(k, dir)] = v
				}
			}
			en := e.entry
			en.Version, en.Fixture, en.Bundle = other, EnrolledFixtureID("claude-json", other, "linux/amd64"), moved
			f[EnrollmentIndexPath], _ = encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{en}})
		}, nil, "differs from the index's exact"},
	} {
		opts := EnrollmentOptions{FS: e.fs(tc.mutate), Registry: e.reg}
		if tc.opts != nil {
			tc.opts(&opts)
		}
		if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The index's own integrity: schema, sorting, duplicates, the exact
// fixture and bundle forms (so no synthetic, fake or aliased fixture can
// be indexed), hashes and identities.
func TestEnrollmentIndex(t *testing.T) {
	e := enrolledFixture(t)
	parse := func(es ...EnrollmentEntry) error {
		b, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: es})
		_, err := ParseEnrollmentIndex(b)
		return err
	}
	if err := parse(e.entry); err != nil {
		t.Fatal(err)
	}
	second := e.entry
	second.Platform = "darwin/arm64"
	second.Fixture = EnrolledFixtureID(second.Decoder, second.Version, second.Platform)
	second.Bundle = EnrolledBundlePath(second.Client, second.Version, second.Platform, "run-2")
	if err := parse(second, e.entry); err != nil {
		t.Fatal(err)
	}
	set := func(f func(*EnrollmentEntry)) EnrollmentEntry { x := e.entry; f(&x); return x }
	for name, entries := range map[string][]EnrollmentEntry{
		"unsorted":       {e.entry, second},
		"dup-fixture":    {e.entry, e.entry},
		"dup-bundle":     {e.entry, set(func(x *EnrollmentEntry) { x.Fixture += "z" })},
		"fake-fixture":   {set(func(x *EnrollmentEntry) { x.Fixture = "claude-json/actual-test" })},
		"synthetic":      {set(func(x *EnrollmentEntry) { x.Fixture = "claude-json/synthetic" })},
		"aliased":        {set(func(x *EnrollmentEntry) { x.Version = "2.1.291" })},
		"outside-root":   {set(func(x *EnrollmentEntry) { x.Bundle = "tests/testdata/other/" + path.Base(x.Bundle) })},
		"bad-run":        {set(func(x *EnrollmentEntry) { x.Bundle = path.Dir(x.Bundle) + "/bad run" })},
		"bad-hash":       {set(func(x *EnrollmentEntry) { x.ManifestSHA256 = "x" })},
		"unknown-client": {set(func(x *EnrollmentEntry) { x.Client = "zed" })},
		"decoder":        {set(func(x *EnrollmentEntry) { x.Decoder = "codex-jsonl" })},
		"multiline":      {set(func(x *EnrollmentEntry) { x.Version += "\nx" })},
		"platform":       {set(func(x *EnrollmentEntry) { x.Platform = "windows/amd64" })},
	} {
		if err := parse(entries...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, raw := range map[string]string{
		"schema":        `{"schema":"mcpqual-enrollment-v2","entries":[]}`,
		"null-entries":  `{"schema":"mcpqual-enrollment-v1","entries":null}`,
		"missing":       `{"schema":"mcpqual-enrollment-v1"}`,
		"unknown-field": `{"schema":"mcpqual-enrollment-v1","entries":[],"x":1}`,
		"not-json":      `{`,
	} {
		if _, err := ParseEnrollmentIndex([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseEnrollmentIndex(bytes.Repeat([]byte(" "), maxIndexBytes+1)); err == nil {
		t.Fatal("an oversized index was accepted")
	}
	if VersionKey("a b/c") != sha256Hex([]byte("a b/c")) || !strings.HasPrefix(e.entry.Fixture, "claude-json/real/") || !strings.HasSuffix(e.entry.Fixture, "/linux-amd64") {
		t.Fatalf("fixture %s", e.entry.Fixture)
	}
}

// The oracle is a reviewed, nonempty, correlated success with a clean
// attestation, whose capabilities it demonstrates itself.
func TestEnrollmentOracle(t *testing.T) {
	e := enrolledFixture(t)
	c := &e.bundle.Manifest.Clients[0]
	if err := e.oracle.validate(c); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(o *ExpectedOracle){
		"empty":        func(o *ExpectedOracle) { o.Events = nil },
		"no-result":    func(o *ExpectedOracle) { o.Events = o.Events[:1] },
		"no-call":      func(o *ExpectedOracle) { o.Events = o.Events[1:] },
		"error-event":  func(o *ExpectedOracle) { o.Events = append(o.Events, Event{CaseID: o.CaseID, Kind: KindMCPTimeout}) },
		"other-case":   func(o *ExpectedOracle) { o.Events[0].CaseID = "x" },
		"nonce":        func(o *ExpectedOracle) { o.Nonce = "other" },
		"result-nonce": func(o *ExpectedOracle) { o.Events[1].Nonce = "other" },
		"case":         func(o *ExpectedOracle) { o.CaseID, o.Events[0].CaseID, o.Events[1].CaseID = "x", "x", "x" },
		"no-terminal":  func(o *ExpectedOracle) { o.Terminal = false },
		"inconclusive": func(o *ExpectedOracle) { o.Inconclusive = ReasonUnrecognized },
		"error-kinds":  func(o *ExpectedOracle) { o.ErrorKinds = []string{CapAuthError} },
		"state":        func(o *ExpectedOracle) { o.CaptureState = CapturePartial },
		"client-info":  func(o *ExpectedOracle) { o.ClientInfo.Name = "other-cli" },
		"no-info":      func(o *ExpectedOracle) { o.ClientInfo = ExpectedClientInfo{} },
		"grammar":      func(o *ExpectedOracle) { o.RequesterCompatible = false },
		"source":       func(o *ExpectedOracle) { o.SourceCaptureSHA256 = "x" },
		"no-reviewer":  func(o *ExpectedOracle) { o.Attestation.Reviewer = "" },
		"omissions":    func(o *ExpectedOracle) { o.Attestation.Omissions = 1 },
		"policy":       func(o *ExpectedOracle) { o.Attestation.Policy = "other" },
		"undemonstrated": func(o *ExpectedOracle) {
			o.Capabilities = append(o.Capabilities, CapMCPTimeout)
		},
		"unknown-cap":   func(o *ExpectedOracle) { o.Capabilities = append(o.Capabilities, "everything") },
		"duplicate-cap": func(o *ExpectedOracle) { o.Capabilities = append(o.Capabilities, CapToolCall) },
		"no-cap":        func(o *ExpectedOracle) { o.Capabilities = nil },
	} {
		o := *e.oracle
		o.Events = append([]Event(nil), e.oracle.Events...)
		o.Capabilities = append([]string(nil), e.oracle.Capabilities...)
		mutate(&o)
		if err := o.validate(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	partial := *c
	partial.State = CapturePartial
	if err := e.oracle.validate(&partial); err == nil {
		t.Fatal("a partial capture enrolled")
	}
	ob, _ := encodeIndent(e.oracle)
	if _, err := ParseExpected(bytes.Replace(ob, []byte(`"terminal": true,`), []byte(""), 1)); err == nil {
		t.Fatal("an oracle without terminal parsed")
	}
	if _, err := ParseExpected(append(bytes.Repeat([]byte(" "), maxIndexBytes), '{')); err == nil {
		t.Fatal("an oversized oracle parsed")
	}
}

// Registry and index agree exactly: evidence per qualified version, every
// fixture indexed for its version, platform and capability set, no orphan
// and no evidence on an unqualified version.
func TestEnrollmentRegistry(t *testing.T) {
	e := enrolledFixture(t)
	idx := &EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{e.entry}}
	oracles := map[string]*ExpectedOracle{e.entry.Fixture: e.oracle}
	if err := CheckRegistryEvidence(e.reg, idx, oracles); err != nil {
		t.Fatal(err)
	}
	version := e.entry.Version
	ev := func(platform, fixture string, kinds ...string) DecoderEvidence {
		return DecoderEvidence{Platform: platform, Fixture: fixture, Kinds: kinds}
	}
	ok := []string{CapToolCall, CapToolResult, CapTerminalSuccess}
	for name, tc := range map[string]struct {
		v    DecoderVersion
		want string
	}{
		"no-evidence":    {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Qualified: true}, "qualified without enrolled evidence"},
		"not-indexed":    {DecoderVersion{Version: version, Fixture: "claude-json/x", Qualified: true, Evidence: []DecoderEvidence{ev("linux/amd64", "claude-json/x", ok...)}}, "not in the enrollment index"},
		"platform":       {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Qualified: true, Evidence: []DecoderEvidence{ev("darwin/arm64", e.entry.Fixture, ok...)}}, "is indexed for"},
		"version":        {DecoderVersion{Version: "other", Fixture: e.entry.Fixture, Qualified: true, Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture, ok...)}}, "is indexed for"},
		"capabilities":   {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Qualified: true, Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture, append(ok, CapMCPTimeout)...)}}, "differ from the oracle"},
		"unknown-cap":    {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Qualified: true, Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture, "everything")}}, "unknown or duplicate capability"},
		"canonical":      {DecoderVersion{Version: version, Fixture: "claude-json/other", Qualified: true, Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture, ok...)}}, "is not its first evidence fixture"},
		"unqualified":    {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture, ok...)}}, "not qualified but carries evidence"},
		"bad-platform":   {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Qualified: true, Evidence: []DecoderEvidence{ev("linux", e.entry.Fixture, ok...)}}, "needs a platform"},
		"twice-claimed":  {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Qualified: true, Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture, ok...), ev("linux/amd64", e.entry.Fixture, ok...)}}, "duplicate evidence"},
		"no-kinds":       {DecoderVersion{Version: version, Fixture: e.entry.Fixture, Qualified: true, Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture)}}, "needs a platform"},
		"orphan-version": {DecoderVersion{Version: version, Fixture: e.entry.Fixture}, "is an orphan"},
	} {
		reg := SyntheticRegistry().WithVersion("claude-json", tc.v)
		if err := CheckRegistryEvidence(reg, idx, oracles); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A second qualified version claiming the same fixture.
	reg := e.reg.WithVersion("claude-json", DecoderVersion{Version: version + "x", Fixture: e.entry.Fixture, Qualified: true,
		Evidence: []DecoderEvidence{ev("linux/amd64", e.entry.Fixture, ok...)}})
	if err := CheckRegistryEvidence(reg, idx, oracles); err == nil {
		t.Fatal("a fixture claimed by two versions")
	}
	// Old reports stay readable; a historical qualified record without
	// evidence is unverified and can never be promoted.
	dv := DecoderVersion{Version: "v", Fixture: "claude-json/actual-test", Qualified: true}
	b, _ := json.Marshal(dv)
	if bytes.Contains(b, []byte("evidence")) {
		t.Fatalf("an empty evidence list is serialized: %s", b)
	}
	if missing := dv.MissingCapabilities("linux/amd64", CapToolCall); len(missing) != 1 {
		t.Fatal("a version without evidence demonstrated a capability")
	}
	if missing := (DecoderVersion{Evidence: []DecoderEvidence{ev("linux/amd64", "f", ok...)}}).MissingCapabilities("linux/amd64", ok...); len(missing) != 3 {
		t.Fatal("an unqualified version demonstrated a capability")
	}
	if RequiredCapabilities(OutcomeInconclusive) != nil || len(RequiredCapabilities(KindAuthError)) != 1 || len(RequiredCapabilities(KindToolError)) != 2 {
		t.Fatal("required capabilities")
	}
}

// UT-6: offline replay against the oracle; the synthetic negatives.
func TestEnrollmentReplay(t *testing.T) {
	e := enrolledFixture(t)
	if err := Replay(e.reg, e.entry, e.bundle, e.oracle); err != nil {
		t.Fatal(err)
	}
	withTranscript := func(lines ...string) *CaptureBundle {
		var buf bytes.Buffer
		for _, l := range lines {
			buf.Write(wrapLine(0, []byte(l)))
			buf.WriteByte('\n')
		}
		b := *e.bundle
		b.Files = map[string][]byte{}
		for k, v := range e.bundle.Files {
			b.Files[k] = v
		}
		b.Files[ClientFile("claude", FileVendorEvents)] = buf.Bytes()
		return &b
	}
	prose := `[{"type":"assistant","message":{"content":[{"type":"text","text":"I called slow: {\"nonce\":\"noncecap1\",\"case_id\":\"claude-capture-setup\"} then it timed out"}]}},{"type":"result","subtype":"success"}]`
	for name, tc := range map[string]struct {
		b      *CaptureBundle
		oracle func(o *ExpectedOracle)
	}{
		// Prose with the nonce is never a structured result.
		"synthetic-prose": {withTranscript(prose), nil},
		"unrelated-tool":  {withTranscript(strings.ReplaceAll(enrollTranscript, "mcp__probe__slow", "mcp__other__slow2")), nil},
		"no-success":      {withTranscript(strings.Replace(enrollTranscript, `"subtype":"success"`, `"subtype":"error_during_execution"`, 1)), nil},
		"malformed":       {withTranscript(`{"truncated`), nil},
		"offset":          {nil, func(o *ExpectedOracle) { o.Events[0].OffsetNS = 5 }},
		"terminal":        {nil, func(o *ExpectedOracle) { o.Terminal = false }},
		"client-info":     {nil, func(o *ExpectedOracle) { o.ClientInfo.Version = "2" }},
	} {
		b := tc.b
		if b == nil {
			b = e.bundle
		}
		o := *e.oracle
		o.Events = append([]Event(nil), e.oracle.Events...)
		if tc.oracle != nil {
			tc.oracle(&o)
		}
		if err := Replay(e.reg, e.entry, b, &o); err == nil {
			t.Errorf("%s: replayed", name)
		}
	}
	// A marker line, an extra probe receipt and an unknown version.
	b := *e.bundle
	b.Files = map[string][]byte{}
	for k, v := range e.bundle.Files {
		b.Files[k] = v
	}
	b.Files[ClientFile("claude", FileVendorEvents)] = append(append([]byte(nil), e.bundle.Files[ClientFile("claude", FileVendorEvents)]...), []byte(`{"omitted_lines":1}`+"\n")...)
	if err := Replay(e.reg, e.entry, &b, e.oracle); err == nil {
		t.Fatal("a marker line replayed")
	}
	b.Files[ClientFile("claude", FileVendorEvents)] = e.bundle.Files[ClientFile("claude", FileVendorEvents)]
	b.Files[ClientFile("claude", FileServerEvents)] = []byte(probeWith(ev(EvReceipt, ""), ev(EvReceipt, ""), ev(EvCompleted, ""), exitEv())(CaseFile{RunID: "run-cap-1"}, "claude-capture-setup"))
	if err := Replay(e.reg, e.entry, &b, e.oracle); err == nil || !strings.Contains(err.Error(), "probe replay") {
		t.Fatalf("an extra receipt replayed: %v", err)
	}
	en := e.entry
	en.Version = "9"
	if err := Replay(e.reg, en, e.bundle, e.oracle); err == nil {
		t.Fatal("an unknown version replayed")
	}
	if tr, err := ReplayTranscript(nil); err != nil || len(tr.Lines) != 0 {
		t.Fatal("an empty transcript")
	}
	// The requester grammar is recorded, never normalized: a name with a
	// space replays when the oracle records it incompatible.
	if ok, _ := RequesterCompatible("fake cli", "1.0.0", replayHostname); ok {
		t.Fatal("a space-containing name passed the grammar")
	}
}

// The production state of slices B2 and B3 (UT-19/UT-23, FP-5/FP-19/FP-23):
// the checked-in index holds exactly the four enrolled linux/amd64
// identities, sorted, each sanitized; the production registry qualifies
// exactly those four (Cursor's added by B3, amending B2's "no real Cursor
// version") with the success capabilities and keeps the four synthetic
// versions labeled and unqualified; and the offline
// self-check passes on the real repository. The legacy empty index stays
// valid against an injected synthetic-only registry, never against the
// production one.
func TestProductionEnrollment(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(EnrollmentIndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := ParseEnrollmentIndex(b)
	if err != nil || idx.Schema != EnrollmentSchema || len(idx.Entries) != 4 {
		t.Fatalf("production index: %v %+v", err, idx)
	}
	want := []struct{ client, decoder, version, run string }{
		{"claude", "claude-json", ClaudeRealVersion, "20261008T122513Z-66db37"},
		{"codex", "codex-jsonl", CodexRealVersion, "20261008T110604Z-13cb4a"},
		{"cursor", "cursor-jsonl", CursorRealVersion, "20261008T212659Z-a5af9a"},
		{"grok", "grok-json", GrokRealVersion, "20261008T110619Z-1663d7"},
	}
	for i, w := range want {
		e := idx.Entries[i]
		if e.Client != w.client || e.Decoder != w.decoder || e.Version != w.version || e.Platform != "linux/amd64" ||
			e.Fixture != EnrolledFixtureID(w.decoder, w.version, "linux/amd64") || e.Bundle != EnrolledBundlePath(w.client, w.version, "linux/amd64", w.run) ||
			e.SanitizationSHA256 == nil {
			t.Fatalf("entry %d %+v", i, e)
		}
	}
	reg := DefaultRegistry()
	if q := reg.QualifiedVersions(); !slices.Equal(q, []string{"claude-json " + ClaudeRealVersion, "codex-jsonl " + CodexRealVersion, "cursor-jsonl " + CursorRealVersion, "grok-json " + GrokRealVersion}) {
		t.Fatalf("qualified production versions %v", q)
	}
	for _, name := range decoderNames {
		for _, v := range reg.Versions(name) {
			if !v.Qualified && (len(v.Evidence) != 0 || v.Fixture != name+"/synthetic") {
				t.Fatalf("%s %+v", name, v)
			}
			if v.Qualified && (len(v.Evidence) != 1 || v.Evidence[0].Platform != "linux/amd64" || v.Evidence[0].Fixture != v.Fixture ||
				!slices.Equal(v.Evidence[0].Kinds, []string{CapToolCall, CapToolResult, CapTerminalSuccess})) {
				t.Fatalf("%s %+v", name, v)
			}
		}
		if reg.Decoder(name) == nil {
			t.Fatalf("no %s decoder", name)
		}
	}
	if vs := reg.Versions("cursor-jsonl"); len(vs) != 2 || vs[0].Qualified || !vs[1].Qualified || vs[1].Version != CursorRealVersion {
		t.Fatalf("cursor versions %+v", vs)
	}
	// Confined opening of the repository (code review C5, round 2 C2): a
	// held os.Root, every component opened relative to its parent.
	got, err := ValidateEnrollment(EnrollmentOptions{Root: root, Registry: reg})
	if err != nil || len(got.Entries) != 4 {
		t.Fatalf("production self-check: %v", err)
	}
	// The legacy empty index: valid against an injected synthetic-only
	// registry, refused against the production registry.
	empty := fstest.MapFS{EnrollmentIndexPath: &fstest.MapFile{Data: []byte(`{"schema":"mcpqual-enrollment-v1","entries":[]}`), Mode: 0o600}}
	if got, err := ValidateEnrollment(EnrollmentOptions{FS: empty, Registry: SyntheticRegistry()}); err != nil || len(got.Entries) != 0 {
		t.Fatalf("legacy empty index: %v", err)
	}
	if _, err := ValidateEnrollment(EnrollmentOptions{FS: empty, Registry: reg}); err == nil || !strings.Contains(err.Error(), "is not in the enrollment index") {
		t.Fatalf("an empty index backed the production registry: %v", err)
	}
}
