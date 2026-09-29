package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	root, good := realCatalog(t)
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

	root, good := realCatalog(t)
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
