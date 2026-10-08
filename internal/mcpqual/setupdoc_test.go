package mcpqual

import (
	"context"
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

// UT-8 (design decoder-enrollment): the guide's capture section documents
// exactly the executable interface: its example command parses with the
// capture flags (reaching the plan's owner placeholders, before any
// launch), it links the four short templates, and it states the owner
// gates, the cost, the CI refusal and the approval limits.
func TestCaptureRunbookDoc(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", SetupDocPath))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	start, end := strings.Index(s, "### Decoder enrollment: setup capture\n"), strings.Index(s, "### Short confirmation\n")
	if start < 0 || end < start {
		t.Fatal("no capture section before the short confirmation")
	}
	sec := s[start:end]
	m := regexp.MustCompile("(?s)```text\n(.*?)\n```").FindStringSubmatch(sec)
	if m == nil {
		t.Fatal("no capture example")
	}
	words, err := ShellWords(m[1])
	if err != nil || len(words) != 7 || words[0] != "<absolute-mcpqual>" || words[1] != "capture" {
		t.Fatalf("example %q", words)
	}
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		if !strings.Contains(sec, "](../internal/mcpqual/testdata/plans/"+id+"-short.json)") {
			t.Fatalf("%s template not linked", id)
		}
		args := append([]string(nil), words[1:]...)
		args[2] = filepath.Join(mustAbs(t, "testdata/plans"), id+"-short.json")
		args[4] = filepath.Join(t.TempDir(), "capture-dir")
		w := newWorld(t, fullModel())
		env, _, errOut := testEnv(t, w, args...)
		if code := Main(context.Background(), env); code != 2 || !strings.Contains(errOut.String(), "unfilled owner placeholder") || len(w.launches) != 0 {
			t.Fatalf("%s example = %d %s", id, code, errOut)
		}
	}
	for _, want := range []string{"Real execution is never part of CI", "even empty", "`! <absolute-mcpqual> capture ...`", "`!` is the UI escape, not a shell negation",
		"Claude must run in the owner's unsandboxed shell", "One capture plus at most three confirmation sessions per client", "Never add `--approve-mcps`",
		"Owner capture gate", "Owner short-confirmation gate", "Optional publication gate", "never retried until green", "macOS timeout compatibility remains UNVERIFIED",
		"`vendor_behavior` `not_evaluated`", "720 s", "Credential words in prose", "`XDG_SESSION_CLASS` and `XDG_SESSION_TYPE`", "an inherited `XDG_SESSION_ID` whose value is a decimal session number",
		// Design decoder-enrollment B1: the recipes, their prerequisites and
		// the thirteen-group cleanup allowance.
		"up to thirteen process groups", "protocol version `2025-06-18`", "`-c mcp_servers.probe.tools.slow.approval_mode=\"approve\"` for this invocation only",
		"an inference from its name", "global `--trust`", "`streaming-json`", "never kills the leader", "exactly one `mcp enable probe` in that workspace",
		"`grok_workspace_placement_unverified`", "`cursor_approval_outside_workspace`", "never restored, deleted or retried", "only the generated workspace is ever trusted",
		// Design decoder-enrollment B1.5: every owner rule of the runbook
		// amendments.
		"Code gate B1.5", "pin the executable and its version from capture through confirmation", "with no other Claude Code session active",
		"`JSON Parse error: Unexpected EOF`", "`" + InventoryPolicyV2 + "`", "`~/.cursor/chats` and `~/.cursor/ai-tracking`", "`" + ScopeProjectScoped + "`",
		"`" + cursorProjectVersion + "` on linux/amd64 only", "\"" + TerminalObservedLabel + "\"", "is absent", "never deleted or overwritten by mcpqual",
		"passing offline macOS tests is not vendor qualification"} {
		if !strings.Contains(sec, want) {
			t.Fatalf("the capture section lacks %q", want)
		}
	}
	for _, want := range []string{"Publication preflight", "initial invocation", "exits 4", "only retries an existing patch"} {
		if !strings.Contains(s, want) {
			t.Fatalf("the guide lacks %q", want)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
