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
	// Per-key ownership (iteration 07b): 07b for mcp_timeout,
	// mcp_timeout_override and mcp_progress_extension, the vendor's 08/11
	// for the other ten; catalog.Owner is the one rule the validator shares.
	owners := map[string]string{"claude": "08", "codex": "08", "grok": "11", "cursor": "11"}
	timeoutKeys := map[string]bool{"mcp_timeout": true, "mcp_timeout_override": true, "mcp_progress_extension": true}
	verified, unverified := 0, 0
	for _, e := range entries {
		// The recorded version is backed by the captured --version output:
		// iteration 08's worker qualification host capture for Claude and
		// Codex, iteration 11's for Grok and Cursor (the 2026-09-25 help
		// captures stay evidence of the older versions only).
		evidence := filepath.Join(root, "tests", "testdata", "real-adapters", "linux-2026-10-04", "host.txt")
		want := "\n" + e.ID + " " + e.Version + "\n"
		if e.ID == "claude" || e.ID == "codex" {
			evidence, want = filepath.Join(root, "tests", "testdata", "real-adapters", "linux-2026-09-30", "host.txt"), e.ID+": "+e.Version+"\n"
		}
		vb, err := os.ReadFile(evidence)
		if err != nil || !strings.Contains(string(vb), want) {
			t.Fatalf("%s version %q not in captured evidence (%v)", e.ID, e.Version, err)
		}
		if e.Platform != "linux/amd64" {
			t.Fatalf("%s platform = %q", e.ID, e.Platform)
		}
		for _, key := range catalog.RequiredFacts {
			f := e.Facts[key]
			want := owners[e.ID]
			if timeoutKeys[key] {
				want = "07b"
			}
			if f.VerificationIteration != catalog.Owner(e.ID, key) || f.VerificationIteration != want {
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
	// Design catalog-version FP-1: the separate worker and coordinator
	// identities, the frozen worker projection and committed receipts.
	t.Run("catalog-version-model", func(t *testing.T) { catalogVersionModel(t, root, entries) })
}
