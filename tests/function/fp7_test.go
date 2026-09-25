package function

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// TestFP7CatalogContract validates the checked-in machine-readable catalog:
// four complete, versioned entries with field-level evidence or explicit
// qualification gaps and their later verification owners. It is structural;
// it does not establish vendor behaviour.
func TestFP7CatalogContract(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	entries, err := catalog.Load(filepath.Join(root, "tests", "testdata", "support-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(root, entries); err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{"claude": "08", "codex": "08", "grok": "11", "cursor": "11"}
	verified, unverified := 0, 0
	for _, e := range entries {
		// The recorded version is backed by the captured --version output.
		vb, err := os.ReadFile(filepath.Join(root, "tests", "testdata", "cli-help", e.ID+"-version.txt"))
		if err != nil || !strings.Contains(string(vb), e.Version) {
			t.Fatalf("%s version %q not in captured evidence (%v)", e.ID, e.Version, err)
		}
		if e.Platform != "linux/amd64" {
			t.Fatalf("%s platform = %q", e.ID, e.Platform)
		}
		for _, key := range catalog.RequiredFacts {
			f := e.Facts[key]
			if f.VerificationIteration != owners[e.ID] {
				t.Fatalf("%s.%s owner = %s", e.ID, key, f.VerificationIteration)
			}
			for _, ev := range f.Evidence {
				if strings.Contains(ev, "design") || filepath.IsAbs(ev) {
					t.Fatalf("%s.%s evidence %s must be checked-in, root-relative testdata", e.ID, key, ev)
				}
			}
			switch f.Status {
			case catalog.Verified:
				verified++
			case catalog.Unverified:
				unverified++
			}
		}
	}
	if verified == 0 || unverified == 0 || verified+unverified != 4*13 {
		t.Fatalf("verified=%d unverified=%d", verified, unverified)
	}
	doc := filepath.Join(root, "docs", "support-catalog.md")
	if err := catalog.CheckDocLinks(doc); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(doc)
	for _, s := range []string{"## Claude Code", "## OpenAI Codex", "## Grok Build", "## Cursor Agent", "## Qualification handoff", "UNVERIFIED", "support-catalog.json"} {
		if !strings.Contains(string(b), s) {
			t.Fatalf("docs/support-catalog.md lacks %q", s)
		}
	}
}
