package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// frozenWorkerCatalogSHA256 freezes the publication-invariant worker
// projection (design catalog-version, Frozen evidence projection): the
// SHA-256 of sumJSON(workerCatalogProjection(entries)), computed once by
// applying that projection to tests/testdata/support-catalog.json of the
// immutable pre-change commit 1038b09 (read-only git inspection, never the
// modified working catalog). For claude, codex, grok and cursor in that
// order it covers the worker identity (id, version, platform) and nine
// complete facts: the eight worker facts and the retained historical
// runbook fact (included because publication never updates it, not
// because it is worker qualification). It omits coordinator_version and
// excludes exactly the four publisher-managed facts mcp_timeout,
// mcp_timeout_override, mcp_progress_extension and mcp_config, whose
// ownership is unchanged (07b; mcp_config 08/11) and whose coordinator
// claims the per-key ownership checks below, CheckShortPollCatalog and the
// publication receipts validate instead. It replaces the three earlier
// broad freezes (the twelve timeout facts, Grok's and Cursor's mcp_config
// and runbook, the complete Claude and Codex entries). Nothing here reads
// git or a design file at test time, and a later evidence change never
// recomputes it.
const (
	frozenWorkerCatalogSHA256  = "6e78394370c59439c384671bfffa46769c5c5ad2d134b004de0f67616efc6c66"
	wave2Erratum               = "The copied NOTES shorthand `unknown model id` is not the exact message; `runs/grok-stdin-fail/stdout.bin` is authoritative for the exact message."
	wave2SourceRoot            = "design/iterations/11-real-adapters/design-check"
	wave2ProductionPlaceholder = "<composed-prompt>"
)

// workerCatalogVendors and workerCatalogKeys are the projection's fixed
// entry order and its nine projected fact keys.
var (
	workerCatalogVendors = []string{"claude", "codex", "grok", "cursor"}
	workerCatalogKeys    = []string{"headless", "model", "effort", "approval", "sandbox_linux", "sandbox_macos", "final_message", "exit_codes", "runbook"}
)

// workerCatalogEntry is one projected entry, encoded by sumJSON with its
// members in the order id, version, platform, facts (encoding/json sorts
// the facts' keys).
type workerCatalogEntry struct {
	ID       string          `json:"id"`
	Version  string          `json:"version"`
	Platform string          `json:"platform"`
	Facts    map[string]Fact `json:"facts"`
}

// workerCatalogProjection is the publication-invariant worker projection
// of entries: exactly the four vendors in fixed order, each with its nine
// projected facts entire (status, value, evidence list and order, and
// verification iteration). A missing, duplicate or unknown entry or a
// missing projected key is an error, never a zero-value substitution.
func workerCatalogProjection(entries []Entry) ([]workerCatalogEntry, error) {
	byID := map[string]Entry{}
	for _, e := range entries {
		if !slices.Contains(workerCatalogVendors, e.ID) {
			return nil, fmt.Errorf("projection: unknown entry %q", e.ID)
		}
		if _, dup := byID[e.ID]; dup {
			return nil, fmt.Errorf("projection: duplicate entry %q", e.ID)
		}
		byID[e.ID] = e
	}
	out := make([]workerCatalogEntry, 0, len(workerCatalogVendors))
	for _, id := range workerCatalogVendors {
		e, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("projection: missing entry %q", id)
		}
		p := workerCatalogEntry{ID: e.ID, Version: e.Version, Platform: e.Platform, Facts: map[string]Fact{}}
		for _, k := range workerCatalogKeys {
			f, ok := e.Facts[k]
			if !ok {
				return nil, fmt.Errorf("projection: %s lacks fact %s", id, k)
			}
			p.Facts[k] = f
		}
		out = append(out, p)
	}
	return out, nil
}

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
	// The twelve publisher-managed timeout facts keep their per-key 07b
	// owner, whatever a publication wrote into them.
	for _, id := range workerCatalogVendors {
		for _, k := range []string{"mcp_timeout", "mcp_timeout_override", "mcp_progress_extension"} {
			if f := byID[id].Facts[k]; f.VerificationIteration != TimeoutQualification {
				t.Fatalf("%s.%s owner %s", id, k, f.VerificationIteration)
			}
		}
	}
	// The frozen publication-invariant worker projection (design
	// catalog-version): worker identities and the nine non-publisher facts
	// of all four vendors, unchanged since 1038b09.
	projected, err := workerCatalogProjection(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got := sumJSON(projected); got != frozenWorkerCatalogSHA256 {
		t.Fatalf("the frozen worker catalog projection changed: %s", got)
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

// TestWorkerCatalogProjection is UT-1's projection sensitivity (design
// catalog-version, Frozen evidence projection): the checked-in catalog
// projects to the frozen digest; any change to a projected worker identity,
// platform or any part of the nine projected facts changes the digest; a
// change only to coordinator_version or to one of the four
// publisher-managed facts leaves it unchanged; and missing, duplicate or
// unknown input is an error, never a substituted zero value.
func TestWorkerCatalogProjection(t *testing.T) {
	_, good := realCatalog(t)
	digest := func(t *testing.T, es []Entry) string {
		t.Helper()
		p, err := workerCatalogProjection(es)
		if err != nil {
			t.Fatal(err)
		}
		return sumJSON(p)
	}
	if got := digest(t, good); got != frozenWorkerCatalogSHA256 {
		t.Fatalf("checked-in projection %s", got)
	}
	// The projection's own shape: four entries in fixed order, nine facts
	// each, members in the order id, version, platform, facts.
	p, _ := workerCatalogProjection(good)
	b, _ := json.Marshal(p[0])
	if len(p) != 4 || p[0].ID != "claude" || p[3].ID != "cursor" || len(p[2].Facts) != 9 || !strings.HasPrefix(string(b), `{"id":"claude","version":"2.1.285 (Claude Code)","platform":"linux/amd64","facts":{`) ||
		strings.Contains(string(b), "coordinator_version") || strings.Contains(string(b), `"mcp_`) {
		t.Fatalf("projection shape %d %s", len(p), b)
	}
	at := func(es []Entry, id string) *Entry {
		for i := range es {
			if es[i].ID == id {
				return &es[i]
			}
		}
		t.Fatalf("no %s", id)
		return nil
	}
	setFact := func(e *Entry, key string, fn func(*Fact)) {
		f := e.Facts[key]
		fn(&f)
		e.Facts[key] = f
	}
	changes := map[string]func(*Entry){
		"version":  func(e *Entry) { e.Version += "x" },
		"platform": func(e *Entry) { e.Platform = "darwin/arm64" },
	}
	for _, k := range workerCatalogKeys {
		changes[k+".value"] = func(e *Entry) { setFact(e, k, func(f *Fact) { f.Value += " Changed." }) }
		changes[k+".status"] = func(e *Entry) {
			setFact(e, k, func(f *Fact) {
				f.Status = map[string]string{Verified: Unverified, Unverified: Verified}[f.Status]
			})
		}
		changes[k+".evidence"] = func(e *Entry) {
			setFact(e, k, func(f *Fact) { f.Evidence = append(f.Evidence, "tests/testdata/extra.txt") })
		}
		changes[k+".iteration"] = func(e *Entry) { setFact(e, k, func(f *Fact) { f.VerificationIteration = "07b" }) }
	}
	// Evidence order is part of a fact.
	changes["headless.evidence-order"] = func(e *Entry) { setFact(e, "headless", func(f *Fact) { slices.Reverse(f.Evidence) }) }
	invariant := map[string]func(*Entry){
		"coordinator_version": func(e *Entry) { e.CoordinatorVersion = "other 9.9.9" },
	}
	for _, k := range []string{"mcp_timeout", "mcp_timeout_override", "mcp_progress_extension", "mcp_config"} {
		invariant[k] = func(e *Entry) {
			setFact(e, k, func(f *Fact) {
				f.Status, f.Value, f.Evidence = Verified, "Measured by qualification run r on linux/amd64.", append(f.Evidence, "tests/testdata/mcp-qualification/r/report.json")
			})
		}
	}
	for _, id := range workerCatalogVendors {
		for name, fn := range changes {
			es := clone(t, good)
			fn(at(es, id))
			if digest(t, es) == frozenWorkerCatalogSHA256 {
				t.Errorf("%s %s does not change the projection", id, name)
			}
		}
		for name, fn := range invariant {
			es := clone(t, good)
			fn(at(es, id))
			if got := digest(t, es); got != frozenWorkerCatalogSHA256 {
				t.Errorf("%s %s changes the projection: %s", id, name, got)
			}
		}
	}
	// Missing required projection input is a failure.
	failures := map[string]func([]Entry) []Entry{
		"missing entry":   func(es []Entry) []Entry { return es[1:] },
		"duplicate entry": func(es []Entry) []Entry { return append(es, es[0]) },
		"unknown entry":   func(es []Entry) []Entry { es[3].ID = "other"; return es },
		"no entries":      func([]Entry) []Entry { return nil },
	}
	for _, k := range workerCatalogKeys {
		failures["missing "+k] = func(es []Entry) []Entry { delete(at(es, "grok").Facts, k); return es }
	}
	for name, fn := range failures {
		if p, err := workerCatalogProjection(fn(clone(t, good))); err == nil || p != nil {
			t.Errorf("%s: projection %v, err %v", name, p, err)
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
