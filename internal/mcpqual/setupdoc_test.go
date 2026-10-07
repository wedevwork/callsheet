package mcpqual

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/mcp"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// FP-9 unit tests: the setup guide's registration and runbook examples,
// its four fact rows per client against the JSON catalog (statuses and
// evidence), the clientInfo compatibility text, the example-plan, README
// and support-catalog links, and the per-key ownership rule.

var designLink = regexp.MustCompile(`\]\([^)]*design/`)

func readDoc(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetupGuide(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	doc := readDoc(t, root, SetupDocPath)
	secs, err := ParseSetupDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := catalog.Load(filepath.Join(root, filepath.FromSlash(CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalog.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	vendorExe := map[string]string{"claude": "claude", "codex": "codex", "grok": "grok", "cursor": "cursor-agent"}
	for id, s := range secs {
		e := byID[id]
		if !strings.Contains(s.Text, "`"+e.Version+"`") {
			t.Errorf("%s: the captured version %q is not named", id, e.Version)
		}
		if id != "cursor" {
			if s.Register[0] != id {
				t.Errorf("%s registration starts with %q", id, s.Register[0])
			}
			callee, err := CalleeArgv(s.Register, "/opt/callsheet", "https://plane.test:7443", "/etc/ca.pem")
			if err != nil || !slices.Equal(callee, []string{"/opt/callsheet", "mcp", "--plane", "https://plane.test:7443", "--ca", "/etc/ca.pem"}) {
				t.Errorf("%s callee argv %q (%v)", id, callee, err)
			}
			if !strings.Contains(s.Text, "Registration command syntax is VERIFIED") {
				t.Errorf("%s: registration status not stated", id)
			}
		} else {
			var c struct {
				MCPServers map[string]struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(s.Candidate), &c); err != nil {
				t.Fatal(err)
			}
			cs := c.MCPServers["callsheet"]
			if cs.Command != "<callsheet-binary>" || !slices.Equal(cs.Args, []string{"mcp", "--plane", "<plane-url>", "--ca", "<ca-path>"}) ||
				!strings.Contains(s.Text, "exact entry shape is UNVERIFIED") || !strings.Contains(s.Text, "There is no `cursor-agent mcp add`") {
				t.Errorf("cursor candidate %+v", c)
			}
		}
		// The runbook goes to the vendor's argv, never to Callsheet's.
		if s.Runbook[0] != vendorExe[id] || s.Runbook[len(s.Runbook)-1] != "$(cat -- '<runbook>')" || strings.Contains(strings.Join(s.Register, " "), "runbook") {
			t.Errorf("%s runbook example %q", id, s.Runbook)
		}
		if !strings.Contains(s.Text, "no supported override is established—run local qualification") {
			t.Errorf("%s: timeout rows lack the qualification wording", id)
		}
		for _, key := range []string{"mcp_config", "mcp_timeout", "mcp_timeout_override", "mcp_progress_extension", "runbook"} {
			row, ok := s.Facts[key]
			jf := e.Facts[key]
			if !ok || row.Status != jf.Status || len(row.Evidence) == 0 {
				t.Errorf("%s.%s row %+v, JSON %s", id, key, row, jf.Status)
				continue
			}
			for _, ev := range row.Evidence {
				if !slices.Contains(jf.Evidence, ev) {
					t.Errorf("%s.%s links %s, not the fact's evidence %v", id, key, ev, jf.Evidence)
				}
				if st, err := os.Stat(filepath.Join(root, ev)); err != nil || st.Size() == 0 {
					t.Errorf("%s.%s evidence %s missing", id, key, ev)
				}
			}
		}
	}
	for _, s := range []string{mcp.BudgetDeferralNote, mcp.ShortPollNotice, "<callsheet-binary>", "<plane-url>", "<ca-path>", "<runbook>",
		"`--ca-fingerprint <sha256>` can replace `--ca <ca-path>`; never pass both", "wrong URL or certificate name (SAN), the CA file and the clock",
		"Initial-prompt loading is the established fallback for Codex and Cursor", "strips trailing newlines", "no runbook bytes are sent to Callsheet",
		"a space-containing name is a dispatch compatibility failure", "never normalized", "B + max(2s, 10% of T) < T", "keep the shared B at 10s",
		"Never raise B on progress evidence alone", "Linux observations never certify macOS", "(support-catalog.md#interim-mcp-wait-exception)",
		"refuses to start when `CI` is set", "--allow-model-calls"} {
		if !strings.Contains(doc, s) {
			t.Errorf("the setup guide lacks %q", s)
		}
	}
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		if !strings.Contains(doc, "](../internal/mcpqual/testdata/plans/"+id+".json)") {
			t.Errorf("the %s example plan is not linked", id)
		}
	}
	for _, rel := range []string{SetupDocPath, "README.md", CatalogMDPath} {
		text := readDoc(t, root, rel)
		if err := catalog.CheckDocLinks(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
		if designLink.MatchString(text) {
			t.Errorf("%s links into the design tree", rel)
		}
		if rel != SetupDocPath && !strings.Contains(text, "coordinator.md)") {
			t.Errorf("%s does not link the setup guide", rel)
		}
	}
	// Rendered-catalog/JSON consistency: one per-key owner rule.
	for _, e := range entries {
		for key, f := range e.Facts {
			if f.VerificationIteration != catalog.Owner(e.ID, key) {
				t.Errorf("%s.%s owner %s", e.ID, key, f.VerificationIteration)
			}
		}
	}
	if !strings.Contains(readDoc(t, root, CatalogMDPath), "`07b` for the three coordinator timeout facts `mcp_timeout`, `mcp_timeout_override` and `mcp_progress_extension` of every vendor") {
		t.Error("support-catalog.md does not state the per-key rule")
	}
}

func TestSetupDocParsing(t *testing.T) {
	if _, err := ParseSetupDoc("# nothing\n"); err == nil {
		t.Fatal("an empty guide parsed")
	}
	root := testkit.MustRepoRoot(t)
	doc := readDoc(t, root, SetupDocPath)
	if _, err := ParseSetupDoc(strings.Replace(doc, "```sh\nclaude mcp add", "```text\nclaude mcp add", 1)); err == nil {
		t.Fatal("a missing registration example parsed")
	}
	if _, err := ParseSetupDoc(strings.Replace(doc, "'<runbook>')\"\n", "'<runbook>')\n", 1)); err == nil {
		t.Fatal("an unterminated quote parsed")
	}
	w, err := ShellWords(`a 'b c' "d 'e'"  f`)
	if err != nil || !slices.Equal(w, []string{"a", "b c", "d 'e'", "f"}) {
		t.Fatalf("%q %v", w, err)
	}
	if _, err := CalleeArgv([]string{"claude", "mcp", "add"}, "b", "p", "c"); err == nil {
		t.Fatal("a registration without -- parsed")
	}
}
