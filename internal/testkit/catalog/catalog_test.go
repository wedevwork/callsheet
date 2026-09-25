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
	mutate("bad iteration", "must be 08 or 11", func(es []Entry) []Entry {
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
