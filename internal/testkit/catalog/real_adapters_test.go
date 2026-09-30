package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// realCaptures is iteration 08's checked-in evidence root.
const realCaptures = "tests/testdata/real-adapters/linux-2026-09-30"

// TestRealAdapterEvidence is UT FP-7 for the catalog (iteration 08): the
// byte-identical evidence copies against their provenance manifest, the
// Claude/Codex facts' owners and wording rules, and the catalog recipes'
// agreement with the adapters' invocations.
func TestRealAdapterEvidence(t *testing.T) {
	root, entries := realCatalog(t)
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(realCaptures), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Provenance string `json:"provenance"`
		SourceRoot string `json:"source_root"`
		Files      []struct {
			Destination, Source, SHA256 string
			Bytes                       int
		} `json:"files"`
		Absent []string `json:"absent"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	copies := map[string]bool{}
	for _, f := range m.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Destination)))
		sum := sha256.Sum256(data)
		if err != nil || len(data) != f.Bytes || hex.EncodeToString(sum[:]) != f.SHA256 || f.Destination != realCaptures+"/"+f.Source || CheckEvidencePath(f.Destination) != nil {
			t.Fatalf("manifest entry %+v: %v", f, err)
		}
		copies[f.Destination] = true
	}
	for _, a := range m.Absent {
		if copies[realCaptures+"/"+a] {
			t.Fatalf("%s is both copied and absent", a)
		}
	}
	for _, need := range []string{"NOTES.md", "host.txt", "claude-sandbox-status.json", "runs-scratch/claude-stdin-success/stdout.bin",
		"runs-scratch/claude-stdin-fail/stdout.bin", "runs-scratch/codex-skip-success/final.saved.txt", "runs-scratch/codex-skip-fail/stdout.bin"} {
		if !copies[realCaptures+"/"+need] {
			t.Fatalf("%s is not copied", need)
		}
	}
	if !slices.Contains(m.Absent, "runs-scratch/codex-skip-fail/final.saved.txt") || !strings.Contains(m.Provenance, "not a new run") {
		t.Fatalf("manifest provenance/absence: %q %v", m.Provenance, m.Absent)
	}
	host, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(realCaptures), "host.txt"))
	for _, e := range entries {
		if e.ID != "claude" && e.ID != "codex" {
			continue
		}
		q := qualificationOf(e.ID)
		if e.Version != q.Version || !strings.Contains(string(host), e.ID+": "+e.Version+"\n") {
			t.Fatalf("%s version %q (qualified %q)", e.ID, e.Version, q.Version)
		}
		for key, f := range e.Facts {
			if f.VerificationIteration != Owner(e.ID, key) {
				t.Fatalf("%s.%s owner %s", e.ID, key, f.VerificationIteration)
			}
			for _, ev := range f.Evidence {
				if strings.HasPrefix(ev, realCaptures+"/") && !copies[ev] {
					t.Fatalf("%s.%s evidence %s is not a manifest copy", e.ID, key, ev)
				}
			}
			lower := strings.ToLower(f.Value)
			switch {
			case f.Status == Verified && (strings.Contains(lower, "unknown") || strings.Contains(lower, "unverified")):
				t.Fatalf("%s.%s VERIFIED value names an unknown: %s", e.ID, key, f.Value)
			case f.Status == Unverified && f.VerificationIteration == "08" && (!strings.Contains(f.Value, "iteration 08") || !strings.Contains(f.Value, "did not measure")):
				t.Fatalf("%s.%s UNVERIFIED value does not say iteration 08 did not measure it: %s", e.ID, key, f.Value)
			}
		}
		// The recipe in the headless value is the adapter's argv (the final
		// path the only variable).
		a, _ := adapter.Builtin("").Lookup(e.ID)
		inv, err := a.Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("a", 32), Model: q.Model, Effort: q.Effort, ScratchDir: "/s"})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Join(append([]string{e.ID}, inv.Argv...), " ")
		want = strings.Replace(want, "/s/"+adapter.CodexFinalName, "<final-file>", 1)
		if !strings.Contains(e.Facts["headless"].Value, "`"+want+"`") {
			t.Fatalf("%s headless recipe does not match the invocation %q", e.ID, want)
		}
		for _, key := range []string{"sandbox_macos", "exit_codes"} {
			if e.Facts[key].Status != Unverified {
				t.Fatalf("%s.%s promoted", e.ID, key)
			}
		}
	}
}

func qualificationOf(id string) adapter.Qualification {
	for _, q := range adapter.Qualifications() {
		if q.ID == id {
			return q
		}
	}
	return adapter.Qualification{}
}
