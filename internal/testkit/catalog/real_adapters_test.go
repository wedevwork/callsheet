package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
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
	wave2Evidence(t, root, entries)
}

// wave2Captures is iteration 11's checked-in evidence root.
const wave2Captures = "tests/testdata/real-adapters/linux-2026-10-04"

// Frozen against the base fixtures (1d9d663's support-catalog.json, never
// regenerated from the modified file): the twelve 07b timeout facts of all
// four vendors, Grok's and Cursor's mcp_config and runbook facts, and the
// complete Claude and Codex entries, each as the SHA-256 of its JSON.
const (
	frozenTimeoutsSHA256       = "1a9150027290e4e4bbd90dbae839308b501a73768016774a3d040fe54099936b"
	frozenWave2CoordSHA256     = "ec7e65c3abafe812ae450bfce0d00ab704b842fd49737035ce6aebbc2c5b0ea5"
	frozenClaudeCodexSHA256    = "2aa8a97db2a950558526baff196dbd5518d3b18cda2b2a9f2b0dd945d1c3f8a0"
	wave2Erratum               = "The copied NOTES shorthand `unknown model id` is not the exact message; `runs/grok-stdin-fail/stdout.bin` is authoritative for the exact message."
	wave2SourceRoot            = "design/iterations/11-real-adapters/design-check"
	wave2ProductionPlaceholder = "<composed-prompt>"
)

// shortPollPolicy and retiredInterim are the catalog's short-poll policy
// and the retired 07a interim sentence it replaced (design
// nonblocking-coordinator-waits; mcpqual's ShortPollPolicy and
// LegacyInterimSentence, which this package cannot import).
const (
	shortPollPolicy = "The default 10s MCP call budget is a deliberately short poll. Long waits use a harness-managed background CLI command. " +
		"Vendor timeout compatibility is claimed only by named local evidence; an unmeasured client remains UNVERIFIED. " +
		"Increasing the budget requires local timeout qualification with response margin."
	retiredInterim = "Owner-authorized interim exception: iteration 07a ships task_wait and dispatch-with-wait with an UNVERIFIED 10s outer call budget, " +
		"shorter plane waits reserve transport/admission/response time, and any increase requires local timeout qualification with an explicit response margin."
)

// wave2Runs is the complete iteration 11 run inventory.
var wave2Runs = []string{"grok-stdin-missing", "grok-stdin-success", "grok-stdin-fail", "grok-shell-permitted", "grok-shell-home", "grok-shell-tmp",
	"grok-edit-permitted", "grok-edit-home", "grok-baseline-permitted", "grok-baseline-home", "cursor-stdin-success", "cursor-stdin-fail",
	"cursor-shell-permitted", "cursor-shell-home", "cursor-shell-tmp", "cursor-edit-permitted", "cursor-edit-home", "cursor-baseline-permitted",
	"cursor-baseline-home", "cursor-sandbox-permitted", "cursor-sandbox-home"}

func sumJSON(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// wave2Evidence is UT FP-7 for iteration 11: the byte-identical copies
// against their manifest (absent sentinels, the erratum), the Grok and
// Cursor facts' versions, owners and wording, the frozen subtrees, and the
// production Grok recipe versus Cursor's rejected candidate.
func wave2Evidence(t *testing.T, root string, entries []Entry) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(wave2Captures), "manifest.json"))
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
		Errata []string `json:"errata"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.SourceRoot != wave2SourceRoot || !strings.Contains(m.Provenance, "not a new run") || !slices.Equal(m.Errata, []string{wave2Erratum}) {
		t.Fatalf("wave-2 manifest provenance %q, source %q, errata %q", m.Provenance, m.SourceRoot, m.Errata)
	}
	copies := map[string]bool{}
	help := 0
	for _, f := range m.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Destination)))
		sum := sha256.Sum256(data)
		rel, ok := strings.CutPrefix(f.Destination, wave2Captures+"/")
		want := "linux-2026-10-04/" + rel
		if name, isHelp := strings.CutPrefix(rel, "help/"); isHelp {
			want = "local-help-2026-10-04/" + name
			help++
		}
		if err != nil || !ok || len(data) != f.Bytes || hex.EncodeToString(sum[:]) != f.SHA256 || f.Source != want || CheckEvidencePath(f.Destination) != nil {
			t.Fatalf("wave-2 manifest entry %+v: %v", f, err)
		}
		copies[f.Destination] = true
	}
	// Every file under the root is listed (no credential-bearing extra),
	// every run has its seven named artifacts, sentinels absent or copied.
	filepath.WalkDir(filepath.Join(root, filepath.FromSlash(wave2Captures)), func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, p)
		if err == nil && !d.IsDir() && filepath.Base(p) != "manifest.json" && !copies[filepath.ToSlash(rel)] {
			t.Errorf("unlisted wave-2 evidence file %s", rel)
		}
		return nil
	})
	if help != 12 || len(m.Files) != 4+12+len(wave2Runs)*7+6 || len(m.Absent) != len(wave2Runs)*3-6 {
		t.Fatalf("wave-2 manifest: %d files (%d help), %d absent", len(m.Files), help, len(m.Absent))
	}
	for _, run := range wave2Runs {
		for _, n := range []string{"argv.txt", "stdin.txt", "pwd.txt", "stdout.bin", "stderr.bin", "exit.txt", "scratch.txt"} {
			if !copies[wave2Captures+"/runs/"+run+"/"+n] {
				t.Fatalf("%s/%s is not copied", run, n)
			}
		}
		for _, n := range []string{"canary-ok.txt", "escape-home.txt", "escape-tmp.txt"} {
			rel := "runs/" + run + "/" + n
			if copies[wave2Captures+"/"+rel] == slices.Contains(m.Absent, rel) {
				t.Fatalf("%s is neither copied nor absent (or both)", rel)
			}
		}
	}
	for _, a := range m.Absent {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(wave2Captures), filepath.FromSlash(a))); !os.IsNotExist(err) {
			t.Fatalf("absent %s exists: %v", a, err)
		}
	}
	notes, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(wave2Captures), "NOTES.md"))
	host, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(wave2Captures), "host.txt"))
	if !strings.Contains(string(notes), "Erratum: "+wave2Erratum) {
		t.Fatal("the copied NOTES lacks the erratum")
	}
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	// The frozen subtrees. The non-blocking coordinator waits design's one
	// change to them, every mcp_timeout value's retired interim suffix
	// replaced by the short-poll policy, is undone first, so the digests
	// still prove each subtree equal to the base fixtures apart from it.
	for id, e := range byID {
		f := e.Facts["mcp_timeout"]
		if !strings.HasSuffix(f.Value, " "+shortPollPolicy) || strings.Contains(f.Value, retiredInterim) {
			t.Fatalf("%s.mcp_timeout does not carry the short-poll policy: %s", id, f.Value)
		}
		f.Value = strings.TrimSuffix(f.Value, shortPollPolicy) + retiredInterim
		facts := maps.Clone(e.Facts)
		facts["mcp_timeout"] = f
		e.Facts = facts
		byID[id] = e
	}
	var timeouts, coord []Fact
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		for _, k := range []string{"mcp_timeout", "mcp_timeout_override", "mcp_progress_extension"} {
			if f := byID[id].Facts[k]; f.VerificationIteration != TimeoutQualification {
				t.Fatalf("%s.%s owner %s", id, k, f.VerificationIteration)
			}
			timeouts = append(timeouts, byID[id].Facts[k])
		}
	}
	for _, id := range []string{"grok", "cursor"} {
		for _, k := range []string{"mcp_config", "runbook"} {
			coord = append(coord, byID[id].Facts[k])
		}
	}
	if sumJSON(timeouts) != frozenTimeoutsSHA256 || sumJSON(coord) != frozenWave2CoordSHA256 || sumJSON([]Entry{byID["claude"], byID["codex"]}) != frozenClaudeCodexSHA256 {
		t.Fatalf("a frozen catalog subtree changed: %s %s %s", sumJSON(timeouts), sumJSON(coord), sumJSON([]Entry{byID["claude"], byID["codex"]}))
	}
	for _, id := range []string{"grok", "cursor"} {
		e := byID[id]
		q := qualificationOf(id)
		if e.Version != q.Version || !strings.Contains(string(host), "\n"+id+" "+e.Version+"\n") || e.Platform != "linux/amd64" {
			t.Fatalf("%s version %q (qualified %q)", id, e.Version, q.Version)
		}
		for key, f := range e.Facts {
			if f.VerificationIteration != Owner(id, key) {
				t.Fatalf("%s.%s owner %s", id, key, f.VerificationIteration)
			}
			lower := strings.ToLower(f.Value)
			switch {
			case f.Status == Verified && (strings.Contains(lower, "unknown") || strings.Contains(lower, "unverified")):
				t.Fatalf("%s.%s VERIFIED value names an unknown: %s", id, key, f.Value)
			case f.Status == Unverified && f.VerificationIteration == "11" && !strings.Contains(f.Value, "iteration 11"):
				t.Fatalf("%s.%s UNVERIFIED value does not name iteration 11: %s", id, key, f.Value)
			}
			if key == "mcp_config" || key == "runbook" || f.VerificationIteration == TimeoutQualification {
				continue
			}
			for _, ev := range f.Evidence {
				if !copies[ev] {
					t.Fatalf("%s.%s evidence %s is not a wave-2 manifest copy", id, key, ev)
				}
			}
		}
		for _, key := range []string{"sandbox_linux", "sandbox_macos", "exit_codes"} {
			if e.Facts[key].Status != Unverified {
				t.Fatalf("%s.%s promoted", id, key)
			}
		}
		for _, key := range []string{"headless", "model", "effort", "final_message"} {
			if e.Facts[key].Status != Verified {
				t.Fatalf("%s.%s is %s", id, key, e.Facts[key].Status)
			}
		}
	}
	if byID["grok"].Facts["approval"].Status != Verified || byID["cursor"].Facts["approval"].Status != Unverified {
		t.Fatal("approval statuses")
	}
	// Production Grok: the backticked recipe, tokenized, is the executable
	// and the adapter's argv with one composed-prompt placeholder, and the
	// capture's argv (one line per argument) with its prompt substituted.
	recipe := backticked(t, byID["grok"].Facts["headless"].Value, "grok ")
	grok, _ := adapter.Builtin("").Lookup("grok")
	inv, err := grok.Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("a", 32), Model: "grok-4.7", Effort: "low", Prompt: []byte("P")})
	if err != nil || inv.Stdin != nil {
		t.Fatalf("grok invocation %v", err)
	}
	want := append([]string{"grok"}, inv.Argv...)
	want[len(want)-1] = wave2ProductionPlaceholder
	if !slices.Equal(recipe, want) {
		t.Fatalf("grok catalog recipe %q, want %q", recipe, want)
	}
	captured := argvLines(t, root, "runs/grok-stdin-success/argv.txt")
	captured[0] = "grok"
	captured[len(captured)-1] = wave2ProductionPlaceholder
	if !slices.Equal(captured, want) {
		t.Fatalf("grok captured argv %q, want %q", captured, want)
	}
	// Cursor: Invocation is always a zero refusal; the catalog's candidate
	// is the capture's argv, labelled rejected, never production argv.
	cursor, _ := adapter.Builtin("").Lookup("cursor")
	if inv, err := cursor.Invocation(adapter.TaskInput{TaskID: "t_" + strings.Repeat("a", 32), Model: "grok-4.7", Effort: "low", Prompt: []byte("P")}); err == nil || inv.Argv != nil {
		t.Fatalf("cursor invocation %+v %v", inv, err)
	}
	value := byID["cursor"].Facts["headless"].Value
	candidate := backticked(t, value, "cursor-agent ")
	captured = argvLines(t, root, "runs/cursor-stdin-success/argv.txt")
	captured[0] = "cursor-agent"
	if !slices.Equal(candidate, captured) || !slices.Contains(candidate, "--force") || !slices.Contains(candidate, "--trust") ||
		!strings.Contains(value, "rejected candidate, not production argv") {
		t.Fatalf("cursor candidate %q (capture %q)", candidate, captured)
	}
}

// backticked returns the first backticked text of value starting with
// prefix, split on spaces (never executed).
func backticked(t *testing.T, value, prefix string) []string {
	t.Helper()
	start := strings.Index(value, "`"+prefix)
	if start < 0 {
		t.Fatalf("no backticked %q in %q", prefix, value)
	}
	end := strings.Index(value[start+1:], "`")
	return strings.Fields(value[start+1 : start+1+end])
}

// argvLines is a capture's argv.txt: one argument per line (the
// executable first), never parsed as shell.
func argvLines(t *testing.T, root, rel string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(wave2Captures), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func qualificationOf(id string) adapter.Qualification {
	for _, q := range adapter.Qualifications() {
		if q.ID == id {
			return q
		}
	}
	return adapter.Qualification{}
}
