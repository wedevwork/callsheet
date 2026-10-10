package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

func realCatalog(t *testing.T) (string, []Entry) {
	t.Helper()
	root := testkit.MustRepoRoot(t)
	entries, err := Load(filepath.Join(root, "tests", "testdata", "support-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	return root, entries
}

// baselineCatalog is the checked-in catalog with every vendor's four
// publisher-managed facts at the frozen pre-publication baseline (design
// catalog-version, amendment A1): the starting state of the mutation
// vectors that assume pre-publication facts, whatever the checkout has
// published.
func baselineCatalog(t *testing.T) (string, []Entry) {
	t.Helper()
	root, entries := realCatalog(t)
	baseline := PublicationBaselineFacts()
	for i := range entries {
		for key, f := range baseline[entries[i].ID] {
			entries[i].Facts[key] = f
		}
	}
	return root, entries
}

func TestCheckedInCatalogIsValid(t *testing.T) {
	root, entries := realCatalog(t)
	if err := Validate(root, entries); err != nil {
		t.Fatal(err)
	}
	if err := CheckDocLinks(filepath.Join(root, "docs", "support-catalog.md")); err != nil {
		t.Fatal(err)
	}
}

func clone(t *testing.T, entries []Entry) []Entry {
	b, _ := json.Marshal(entries)
	out, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestValidationRejections(t *testing.T) {
	// The vectors assume pre-publication managed facts (A1).
	root, good := baselineCatalog(t)
	mutate := func(name, wantErr string, f func(es []Entry) []Entry) {
		t.Run(name, func(t *testing.T) {
			err := Validate(root, f(clone(t, good)))
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("want %q, got %v", wantErr, err)
			}
		})
	}
	setFact := func(es []Entry, id, key string, fn func(*Fact)) {
		for i := range es {
			if es[i].ID == id {
				f := es[i].Facts[key]
				fn(&f)
				es[i].Facts[key] = f
			}
		}
	}
	mutate("three entries", "want 4", func(es []Entry) []Entry { return es[:3] })
	mutate("duplicate vendor", "duplicate vendor", func(es []Entry) []Entry { es[1].ID = "claude"; return es })
	mutate("unknown vendor", "unknown vendor id", func(es []Entry) []Entry { es[3].ID = "other"; return es })
	mutate("missing version", "missing version", func(es []Entry) []Entry { es[0].Version = " "; return es })
	mutate("missing platform", "missing platform", func(es []Entry) []Entry { es[0].Platform = ""; return es })
	mutate("missing fact", "missing fact runbook", func(es []Entry) []Entry { delete(es[0].Facts, "runbook"); return es })
	mutate("extra fact", "want exactly", func(es []Entry) []Entry { es[0].Facts["extra"] = es[0].Facts["model"]; return es })
	mutate("bad status", "must be VERIFIED or UNVERIFIED", func(es []Entry) []Entry {
		setFact(es, "codex", "model", func(f *Fact) { f.Status = "MAYBE" })
		return es
	})
	mutate("empty value", "empty value", func(es []Entry) []Entry {
		setFact(es, "grok", "model", func(f *Fact) { f.Value = "" })
		return es
	})
	mutate("bad iteration", "must be 07b, 08 or 11", func(es []Entry) []Entry {
		setFact(es, "grok", "model", func(f *Fact) { f.VerificationIteration = "09" })
		return es
	})
	mutate("wrong owner", "want 11 for this vendor", func(es []Entry) []Entry {
		setFact(es, "cursor", "model", func(f *Fact) { f.VerificationIteration = "08" })
		return es
	})
	mutate("verified without evidence", "VERIFIED fact without evidence", func(es []Entry) []Entry {
		setFact(es, "claude", "headless", func(f *Fact) { f.Evidence = nil })
		return es
	})
	mutate("mixed verified", "mixes unverified", func(es []Entry) []Entry {
		setFact(es, "claude", "headless", func(f *Fact) { f.Value += " Exit codes are unknown." })
		return es
	})
	mutate("unqualified unverified", "must name its qualification work", func(es []Entry) []Entry {
		setFact(es, "claude", "mcp_timeout", func(f *Fact) { f.Value = "Unknown." })
		return es
	})
	mutate("iteration mismatch in value", "value names iteration 11", func(es []Entry) []Entry {
		setFact(es, "claude", "mcp_timeout", func(f *Fact) { f.Value = "Unknown; iteration 11 measures it." })
		return es
	})
	for name, p := range map[string]string{
		"absolute":  "/etc/passwd",
		"escaping":  "../outside.txt",
		"unclean":   "tests/./testdata/cli-help/claude-help.txt",
		"backslash": `tests\testdata\cli-help\claude-help.txt`,
		"missing":   "tests/testdata/cli-help/nope.txt",
		"directory": "tests/testdata/cli-help",
		"empty":     "",
	} {
		p := p
		want := map[string]string{"missing": "not a nonempty file", "directory": "not a nonempty file", "empty": "empty evidence path", "backslash": "forward slashes"}[name]
		if want == "" {
			want = p
		}
		mutate("evidence "+name, want, func(es []Entry) []Entry {
			setFact(es, "codex", "headless", func(f *Fact) { f.Evidence = []string{p} })
			return es
		})
	}
}

// TestCatalogVersionModel is UT-1 (design catalog-version, Identity model):
// the four exact worker/coordinator identity pairs, the checked-in member
// order, the mandatory coordinator_version with its exact diagnostic for
// the missing, empty and whitespace-only forms (old JSON still decodes but
// never validates, with no fallback to version), strict decoding of an
// unknown member, and the worker evidence and per-key ownership preserved.
func TestCatalogVersionModel(t *testing.T) {
	root, good := realCatalog(t)
	want := []struct{ id, worker, coordinator string }{
		{"claude", "2.1.285 (Claude Code)", "2.1.292 (Claude Code)"},
		{"codex", "codex-cli 0.159.0", "codex-cli 0.160.0"},
		{"grok", "grok 1.0.46 (2765805b9442) [stable]", "grok 1.0.46 (2765805b9442) [stable]"},
		{"cursor", "2026.10.01-e373342", "2026.10.01-e373342"},
	}
	if len(good) != len(want) {
		t.Fatalf("%d entries", len(good))
	}
	for i, w := range want {
		e := good[i]
		if e.ID != w.id || e.Version != w.worker || e.CoordinatorVersion != w.coordinator || e.Platform != "linux/amd64" || len(e.Facts) != 13 {
			t.Fatalf("entry %d: %q %q %q %q", i, e.ID, e.Version, e.CoordinatorVersion, e.Platform)
		}
		for _, key := range RequiredFacts {
			if e.Facts[key].VerificationIteration != Owner(e.ID, key) {
				t.Fatalf("%s.%s owner %s", e.ID, key, e.Facts[key].VerificationIteration)
			}
		}
	}
	// Member order of every checked-in entry and of the struct encoding.
	b, err := os.ReadFile(filepath.Join(root, "tests", "testdata", "support-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"id", "version", "coordinator_version", "platform", "facts"}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) != 4 {
		t.Fatalf("raw entries %v", err)
	}
	for i, r := range raw {
		if got := memberOrder(t, r); strings.Join(got, ",") != strings.Join(order, ",") {
			t.Fatalf("entry %d members %v, want %v", i, got, order)
		}
	}
	enc, _ := json.Marshal(good[0])
	if got := memberOrder(t, enc); strings.Join(got, ",") != strings.Join(order, ",") {
		t.Fatalf("Entry encodes as %v", got)
	}
	// Missing, empty and whitespace-only: the exact diagnostic, with the
	// entry's zero-based index and id, and no fallback to version.
	for name, mutate := range map[string]func([]byte) []byte{
		"missing": func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"coordinator_version": "codex-cli 0.160.0",`+"\n    ", "", 1))
		},
		"empty":      func(b []byte) []byte { return []byte(strings.Replace(string(b), `"codex-cli 0.160.0"`, `""`, 1)) },
		"whitespace": func(b []byte) []byte { return []byte(strings.Replace(string(b), `"codex-cli 0.160.0"`, `" \t "`, 1)) },
	} {
		mb := mutate(b)
		if bytes.Equal(mb, b) {
			t.Fatalf("%s: no mutation", name)
		}
		es, err := Decode(mb)
		if err != nil {
			t.Fatalf("%s: old or blank JSON must still decode structurally: %v", name, err)
		}
		if es[1].Version != "codex-cli 0.159.0" || strings.TrimSpace(es[1].CoordinatorVersion) != "" {
			t.Fatalf("%s: decoded %+v", name, es[1].CoordinatorVersion)
		}
		err = Validate(root, es)
		if err == nil || !slices.Contains(strings.Split(err.Error(), "\n"), "entry 1 (codex): missing coordinator_version") || strings.Count(err.Error(), "\n") != 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A pre-change catalog (no member anywhere) fails for every entry.
	old := regexp.MustCompile(`\n    "coordinator_version": "[^"]*",`).ReplaceAll(b, nil)
	es, err := Decode(old)
	if err != nil {
		t.Fatal(err)
	}
	err = Validate(root, es)
	for i, w := range want {
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("entry %d (%s): missing coordinator_version", i, w.id)) {
			t.Fatalf("old catalog: %v", err)
		}
	}
	// Unknown members stay strict errors, including a near-miss name.
	for _, j := range []string{`[{"id":"claude","coordinator":"x"}]`, `[{"id":"claude","coordinator_versions":"x"}]`, `[{"id":"claude","mcp_version":"x"}]`} {
		if _, err := Decode([]byte(j)); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s: %v", j, err)
		}
	}
	// Equal identities are valid, and a differing coordinator identity
	// never touches the worker identity or facts.
	es = clone(t, good)
	es[0].CoordinatorVersion = es[0].Version
	if err := Validate(root, es); err != nil {
		t.Fatal(err)
	}
}

// TestPublicationBaselineFacts (design catalog-version, amendment A1): the
// frozen baseline holds exactly the sixteen publisher-managed facts with
// their per-key owners, valid in the catalog it was taken from, and every
// call returns an independent deep copy.
func TestPublicationBaselineFacts(t *testing.T) {
	managed := []string{"mcp_config", "mcp_timeout", "mcp_timeout_override", "mcp_progress_extension"}
	b := PublicationBaselineFacts()
	if len(b) != len(Vendors) {
		t.Fatalf("%d vendors", len(b))
	}
	root, good := realCatalog(t)
	es := clone(t, good)
	for i := range es {
		facts := b[es[i].ID]
		if len(facts) != len(managed) {
			t.Fatalf("%s: %d facts", es[i].ID, len(facts))
		}
		for _, key := range managed {
			f, ok := facts[key]
			if !ok || f.Status != Unverified || f.VerificationIteration != Owner(es[i].ID, key) || len(f.Evidence) == 0 {
				t.Fatalf("%s.%s baseline %+v", es[i].ID, key, f)
			}
			es[i].Facts[key] = f
		}
	}
	// The baseline is a valid pre-publication state of the catalog.
	if err := Validate(root, es); err != nil {
		t.Fatal(err)
	}
	// Mutating a returned copy never reaches the next caller.
	f := b["codex"]["mcp_config"]
	f.Evidence[0] = "changed"
	b["claude"]["mcp_timeout"] = Fact{}
	delete(b, "grok")
	again := PublicationBaselineFacts()
	if len(again) != 4 || again["codex"]["mcp_config"].Evidence[0] == "changed" || again["claude"]["mcp_timeout"].Value == "" || len(again["grok"]) != 4 {
		t.Fatal("the baseline is shared between callers")
	}
}

// TestPublicationBaselineBullets (amendment A1): the frozen Markdown half
// of the baseline holds three anchored bullets per vendor, copies are
// independent, and seeding a support catalog replaces exactly those lines
// (idempotently), refusing a missing section, a missing or a duplicate
// bullet.
func TestPublicationBaselineBullets(t *testing.T) {
	anchors := []string{"- **MCP call timeout:", "- **How to raise it:", "- **Progress extends calls:"}
	b := PublicationBaselineBullets()
	if len(b) != len(Vendors) {
		t.Fatalf("%d vendors", len(b))
	}
	for id := range Vendors {
		if len(b[id]) != 3 {
			t.Fatalf("%s: %d bullets", id, len(b[id]))
		}
		for i, l := range b[id] {
			if !strings.HasPrefix(l, anchors[i]+" UNVERIFIED") || strings.Contains(l, "\n") {
				t.Fatalf("%s bullet %d %q", id, i, l)
			}
		}
	}
	b["claude"][0] = "changed"
	if PublicationBaselineBullets()["claude"][0] == "changed" {
		t.Fatal("the baseline bullets are shared between callers")
	}
	root := testkit.MustRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "support-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	md := string(raw)
	seeded, err := WithPublicationBaselineBullets(md)
	if err != nil {
		t.Fatal(err)
	}
	again, err := WithPublicationBaselineBullets(seeded)
	if err != nil || again != seeded {
		t.Fatalf("seeding is not idempotent: %v", err)
	}
	before, after := strings.Split(md, "\n"), strings.Split(seeded, "\n")
	if len(before) != len(after) {
		t.Fatal("seeding changed the line count")
	}
	for i := range before {
		if before[i] != after[i] && !slices.ContainsFunc(anchors, func(a string) bool { return strings.HasPrefix(before[i], a) && strings.HasPrefix(after[i], a) }) {
			t.Fatalf("seeding changed a non-bullet line %d: %q", i, before[i])
		}
	}
	for _, lines := range PublicationBaselineBullets() {
		for _, l := range lines {
			if !strings.Contains(seeded, "\n"+l+"\n") {
				t.Fatalf("seeded catalog lacks %q", l)
			}
		}
	}
	for name, bad := range map[string]string{
		"missing section":   strings.Replace(seeded, "\n## Grok Build\n", "\n## Grok\n", 1),
		"missing bullet":    strings.Replace(seeded, "\n- **How to raise it:", "\n- How to raise it:", 1),
		"duplicate bullet":  strings.Replace(seeded, "\n- **MCP call timeout:", "\n- **MCP call timeout: x\n- **MCP call timeout:", 1),
		"duplicate section": seeded + "\n## Cursor Agent\n",
	} {
		if _, err := WithPublicationBaselineBullets(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// memberOrder is the member names of one JSON object, in order.
func memberOrder(t *testing.T, obj []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(obj))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %v", err)
	}
	var names []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return names
}

func TestDecodeStrictAndLoadErrors(t *testing.T) {
	if _, err := Decode([]byte(`[{"id":"claude","bogus":1}]`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file")
	}
	if err := CheckEvidencePath(".."); err == nil {
		t.Fatal("bare .. accepted")
	}
}

func TestCheckDocLinks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o644)
	doc := filepath.Join(dir, "doc.md")
	os.WriteFile(doc, []byte("[a](ok.txt) [web](https://example.invalid/x) [m](mailto:x@y) [frag](#top)"), 0o644)
	if err := CheckDocLinks(doc); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(doc, []byte("[a](missing.txt) [b](design-check/claude-help.txt)"), 0o644)
	err := CheckDocLinks(doc)
	if err == nil || !strings.Contains(err.Error(), "broken link missing.txt") || !strings.Contains(err.Error(), "gitignored") {
		t.Fatalf("err = %v", err)
	}
	if err := CheckDocLinks(filepath.Join(dir, "none.md")); err == nil {
		t.Fatal("missing doc")
	}
}

// TestCatalogQualificationIteration pins the per-key ownership rule
// (iteration 07b, FP-14): Owner against a literal table (never derived from
// Owner itself), including its fallbacks, and the validator's use of it.
func TestCatalogQualificationIteration(t *testing.T) {
	timeout := map[string]bool{"mcp_timeout": true, "mcp_timeout_override": true, "mcp_progress_extension": true}
	vendorOwner := map[string]string{"claude": "08", "codex": "08", "grok": "11", "cursor": "11"}
	keys := []string{"headless", "model", "effort", "approval", "sandbox_linux", "sandbox_macos", "final_message", "exit_codes",
		"mcp_config", "mcp_timeout", "mcp_timeout_override", "mcp_progress_extension", "runbook"}
	if strings.Join(keys, ",") != strings.Join(RequiredFacts, ",") {
		t.Fatal("RequiredFacts changed")
	}
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		for _, key := range keys {
			want := vendorOwner[id]
			if timeout[key] {
				want = "07b"
			}
			if got := Owner(id, key); got != want {
				t.Errorf("Owner(%s, %s) = %q, want %q", id, key, got, want)
			}
		}
	}
	// Fallbacks: the timeout keys are 07b even for an unknown vendor; any
	// other key of an unknown vendor has no owner.
	for key, want := range map[string]string{"mcp_timeout": "07b", "mcp_timeout_override": "07b", "mcp_progress_extension": "07b", "model": "", "mcp_config": ""} {
		if got := Owner("unknown", key); got != want {
			t.Errorf("Owner(unknown, %s) = %q, want %q", key, got, want)
		}
	}
	if Owner("claude", "no_such_key") != "08" {
		t.Error("an unknown key does not fall back to the vendor's owner")
	}

	// The vectors below assume pre-publication managed facts (A1).
	root, good := baselineCatalog(t)
	if err := Validate(root, good); err != nil {
		t.Fatalf("the exact mapping is rejected: %v", err)
	}
	set := func(id, key string, fn func(*Fact)) []Entry {
		es := clone(t, good)
		for i := range es {
			if es[i].ID == id {
				f := es[i].Facts[key]
				fn(&f)
				es[i].Facts[key] = f
			}
		}
		return es
	}
	expectErr := func(what string, es []Entry, want string) {
		t.Helper()
		if err := Validate(root, es); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", what, want, err)
		}
	}
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		for _, key := range keys {
			var wrong []string
			if timeout[key] {
				wrong = []string{"08", "11"}
			} else {
				wrong = []string{"07b", map[string]string{"08": "11", "11": "08"}[vendorOwner[id]]}
			}
			for _, w := range wrong {
				expectErr(id+"."+key+"="+w, set(id, key, func(f *Fact) { f.VerificationIteration = w }), "verification_iteration "+w+", want "+Owner(id, key)+" for this vendor")
			}
		}
	}
	// UNVERIFIED work references: missing, mismatched, and matching 07b.
	expectErr("missing reference", set("grok", "mcp_timeout", func(f *Fact) { f.Value = "Unknown." }), "must name its qualification work (iteration 07b/08/11)")
	expectErr("mismatched reference", set("grok", "mcp_timeout_override", func(f *Fact) { f.Value = "Unknown; iteration 11 must find it." }),
		"value names iteration 11 but verification_iteration is 07b")
	expectErr("07b text on an 08 fact", set("codex", "mcp_config", func(f *Fact) { f.Value = "Candidate only; iteration 07b validates it." }),
		"value names iteration 07b but verification_iteration is 08")
	if err := Validate(root, set("cursor", "mcp_progress_extension", func(f *Fact) { f.Value = "Unknown; ITERATION 07b local qualification must test it." })); err != nil {
		t.Errorf("matching iteration 07b text rejected: %v", err)
	}
	// VERIFIED keeps its mixed-content and evidence rules.
	expectErr("mixed verified", set("claude", "mcp_timeout", func(f *Fact) { f.Status, f.Value = Verified, "Measured: an unknown maximum." }), "mixes unverified")
	expectErr("verified without evidence", set("claude", "mcp_timeout_override", func(f *Fact) { f.Status, f.Value, f.Evidence = Verified, "Measured.", nil }),
		"VERIFIED fact without evidence")
	if err := Validate(root, set("claude", "mcp_timeout", func(f *Fact) {
		f.Status, f.Value = Verified, "Measured on linux/amd64: a 5000 ms silent call completed."
	})); err != nil {
		t.Errorf("a clean VERIFIED timeout fact rejected: %v", err)
	}
}
