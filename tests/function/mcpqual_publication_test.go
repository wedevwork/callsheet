package function

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Design catalog-version function tests (FP-1..FP-9): one logical subtest
// per FP under an existing parent, outside the parents' fixed case
// inventories (the parents call these bodies unconditionally). Everything
// is offline: the publisher and report APIs on temporary catalog and
// output trees, the fake vendor and the clocked real-format fakes the
// parents already use, and the unchanged checked-in fixtures. No test
// reads a design path, a real HOME or a vendor executable, and every
// publication receipt a test writes is an explicitly synthetic temporary
// one.

// publicationReceiptName is the coordinator-authored delivery receipt of a
// committed qualification run directory.
const publicationReceiptName = "publication-review.json"

// Exact attestation handles of a final receipt.
const (
	receiptReviewer = "decoder-enrollment-coordinator"
	receiptOwner    = "callsheet-owner"
)

// Frozen at the immutable pre-change commit 1038b09 (read-only git
// inspection, never regenerated from a modified tree): the enrollment
// index's bytes, the 429 native parents in their order, the stress plan's
// argv on both hosts and the CI workflow's bytes.
const (
	enrollmentIndexSHA256 = "c0f28d31dc24401977037c9e0b27a44810ea19984518a8bc46ab62c8b6ea7e61"
	nativeInventorySHA256 = "f1ff8d0ac67dee91651b844f2ed26f17304dbc1e305817bbff0c299d1a77468d"
	stressArgvSHA256      = "04ed36ef45fddaa0c036be114d8a5ce89e8a86dc0675997341dd89e89805e61a"
	ciWorkflowSHA256      = "a4cbc826d9e75459d4e72b075167cb8513b602fbe6f74e3c11aa901afc9ab452"
)

// catalogIdentities are the migrated catalog's exact worker and
// coordinator (MCP publication) identities.
var catalogIdentities = []struct{ id, worker, coordinator string }{
	{"claude", "2.1.285 (Claude Code)", mcpqual.ClaudeRealVersion},
	{"codex", "codex-cli 0.159.0", mcpqual.CodexRealVersion},
	{"grok", "grok 1.0.46 (2765805b9442) [stable]", mcpqual.GrokRealVersion},
	{"cursor", "2026.10.01-e373342", mcpqual.CursorRealVersion},
}

// publicationReceipt is publication-review.json.
type publicationReceipt struct {
	Schema                int           `json:"schema"`
	RunID                 string        `json:"run_id"`
	Files                 []receiptFile `json:"files"`
	Reviewer              string        `json:"reviewer"`
	ReviewedAt            string        `json:"reviewed_at"`
	Owner                 string        `json:"owner"`
	ApprovedAt            string        `json:"approved_at"`
	IndependentlyVerified bool          `json:"independently_verified"`
	PrivacyApproved       bool          `json:"privacy_approved"`
	OwnerAccepted         bool          `json:"owner_accepted"`
}

// receiptFile is one inventory entry of a receipt.
type receiptFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

var receiptHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxReceiptBytes bounds a receipt and every inventoried file read here.
const maxReceiptBytes = 8 << 20

// validatePublicationReceipt validates the committed qualification run
// directory runDir and its receipt: the strict receipt schema, the exact
// handles, flags and ordered UTC timestamps, and an inventory of every
// other file in the directory (sorted, clean, unique, regular files with
// exact sizes and lowercase SHA-256, no link, extra, absent file or
// self-reference) that equals the report and manifest's own file set as
// the existing report parser, CheckEvidence and the recomputed manifest
// hashes establish it. It returns nil only for a valid package. It checks
// structure; it cannot prove the human review and approval happened.
func validatePublicationReceipt(runDir string) error {
	info, err := os.Lstat(runDir)
	if err != nil {
		return fmt.Errorf("run directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("run directory %s is not a directory", runDir)
	}
	rp := filepath.Join(runDir, publicationReceiptName)
	ri, err := os.Lstat(rp)
	if err != nil {
		return fmt.Errorf("missing receipt %s: %w", publicationReceiptName, err)
	}
	if !ri.Mode().IsRegular() || ri.Size() > maxReceiptBytes {
		return fmt.Errorf("receipt %s is not a bounded regular file", publicationReceiptName)
	}
	b, err := os.ReadFile(rp)
	if err != nil {
		return fmt.Errorf("receipt: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r publicationReceipt
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("receipt: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("receipt: trailing data")
	}
	switch {
	case r.Schema != 1:
		return fmt.Errorf("receipt schema %d, want 1", r.Schema)
	case r.Reviewer != receiptReviewer:
		return fmt.Errorf("receipt reviewer %q is not %q", r.Reviewer, receiptReviewer)
	case r.Owner != receiptOwner:
		return fmt.Errorf("receipt owner %q is not %q", r.Owner, receiptOwner)
	case !r.IndependentlyVerified || !r.PrivacyApproved || !r.OwnerAccepted:
		return errors.New("receipt has a false flag: independently_verified, privacy_approved and owner_accepted must be true")
	}
	reviewed, err := receiptTime("reviewed_at", r.ReviewedAt)
	if err != nil {
		return err
	}
	approved, err := receiptTime("approved_at", r.ApprovedAt)
	if err != nil {
		return err
	}
	if approved.Before(reviewed) {
		return errors.New("receipt approved_at is before reviewed_at")
	}
	listed := map[string]receiptFile{}
	for i, f := range r.Files {
		if err := receiptPath(f.Path); err != nil {
			return err
		}
		if f.Path == publicationReceiptName {
			return errors.New("receipt inventory lists the receipt itself")
		}
		if i > 0 && f.Path <= r.Files[i-1].Path {
			return fmt.Errorf("receipt inventory is not sorted or repeats %s", f.Path)
		}
		if !receiptHex.MatchString(f.SHA256) || f.Bytes < 0 {
			return fmt.Errorf("receipt entry %s has an invalid hash or size", f.Path)
		}
		fi, err := os.Lstat(filepath.Join(runDir, filepath.FromSlash(f.Path)))
		if err != nil {
			return fmt.Errorf("listed file %s is absent: %w", f.Path, err)
		}
		if !fi.Mode().IsRegular() || fi.Size() > maxReceiptBytes {
			return fmt.Errorf("listed file %s is not a bounded regular file (link or special)", f.Path)
		}
		data, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(f.Path)))
		if err != nil {
			return fmt.Errorf("listed file %s: %w", f.Path, err)
		}
		if int64(len(data)) != f.Bytes || sha(data) != f.SHA256 {
			return fmt.Errorf("listed file %s differs from its size or hash", f.Path)
		}
		listed[f.Path] = f
	}
	// Every other file is listed; no link or special file anywhere.
	err = filepath.WalkDir(runDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == runDir {
			return nil
		}
		rel, _ := filepath.Rel(runDir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("run directory holds a link %s", rel)
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("run directory holds a special file %s", rel)
		case rel == publicationReceiptName:
			return nil
		}
		if _, ok := listed[rel]; !ok {
			return fmt.Errorf("unlisted file %s", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The report and manifest bind the same inventory (existing APIs).
	rb, err := os.ReadFile(filepath.Join(runDir, "report.json"))
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	rep, err := mcpqual.ParseReport(rb)
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if rep.RunID != r.RunID || filepath.Base(runDir) != r.RunID {
		return fmt.Errorf("receipt run_id %q, report %q, directory %q", r.RunID, rep.RunID, filepath.Base(runDir))
	}
	if err := rep.CheckEvidence(runDir); err != nil {
		return fmt.Errorf("report evidence: %w", err)
	}
	mb, err := os.ReadFile(filepath.Join(runDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	mdec := json.NewDecoder(bytes.NewReader(mb))
	mdec.DisallowUnknownFields()
	var man mcpqual.Manifest
	if err := mdec.Decode(&man); err != nil || man.Schema != mcpqual.ReportSchema || man.RunID != r.RunID {
		return fmt.Errorf("manifest schema or run differs: %v", err)
	}
	want := map[string]bool{"manifest.json": true}
	fromReport := map[string]mcpqual.EvidenceRef{"report.json": {}, "report.md": {}}
	for _, ev := range rep.Evidence {
		fromReport[ev.Path] = ev
	}
	for _, f := range man.Files {
		l, ok := listed[f.Path]
		ev, inReport := fromReport[f.Path]
		switch {
		case want[f.Path]:
			return fmt.Errorf("manifest repeats %s", f.Path)
		case !ok || l.SHA256 != f.SHA256 || l.Bytes != f.Bytes:
			return fmt.Errorf("manifest file %s differs from the receipt inventory", f.Path)
		case !inReport || ev.Path != "" && (ev.SHA256 != f.SHA256 || ev.Bytes != f.Bytes):
			return fmt.Errorf("manifest file %s differs from the report", f.Path)
		}
		want[f.Path] = true
	}
	if len(want) != len(fromReport)+1 {
		return errors.New("manifest does not list exactly the report, its Markdown and every evidence file")
	}
	for p := range listed {
		if !want[p] {
			return fmt.Errorf("receipt lists %s outside the report and manifest", p)
		}
	}
	if len(listed) != len(want) {
		return errors.New("receipt inventory lacks a report or manifest file")
	}
	return nil
}

// receiptTime parses a nonempty RFC3339 UTC timestamp that is not a
// placeholder (zero or the Unix epoch).
func receiptTime(name, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("receipt %s is empty", name)
	}
	ts, err := time.Parse(time.RFC3339, s)
	switch {
	case err != nil:
		return time.Time{}, fmt.Errorf("receipt %s %q is not RFC3339: %v", name, s, err)
	case !strings.HasSuffix(s, "Z"):
		return time.Time{}, fmt.Errorf("receipt %s %q is not UTC", name, s)
	case ts.IsZero() || ts.Equal(time.Unix(0, 0)):
		return time.Time{}, fmt.Errorf("receipt %s %q is a placeholder date", name, s)
	}
	return ts, nil
}

// receiptPath rejects an absolute, unclean, escaping or backslash path.
func receiptPath(p string) error {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("receipt path %q is not a clean run-relative path", p)
	}
	return nil
}

// writeSyntheticReceipt writes an explicitly synthetic, test-only receipt
// for runDir: its inventory computed from the directory, then mutate.
func writeSyntheticReceipt(t *testing.T, runDir string, mutate func(*publicationReceipt)) {
	t.Helper()
	r := publicationReceipt{Schema: 1, RunID: filepath.Base(runDir), Files: []receiptFile{}, Reviewer: receiptReviewer, ReviewedAt: "2026-10-11T09:00:00Z",
		Owner: receiptOwner, ApprovedAt: "2026-10-11T10:00:00Z", IndependentlyVerified: true, PrivacyApproved: true, OwnerAccepted: true}
	filepath.WalkDir(runDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(runDir, p)
		if rel = filepath.ToSlash(rel); rel == publicationReceiptName {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		r.Files = append(r.Files, receiptFile{Path: rel, Bytes: int64(len(b)), SHA256: sha(b)})
		return nil
	})
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
	if mutate != nil {
		mutate(&r)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(runDir, publicationReceiptName), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// publicationRunDirs is every committed qualification run directory under
// root: those the catalog's evidence references and those present.
func publicationRunDirs(t *testing.T, root string, entries []catalog.Entry) []string {
	t.Helper()
	runs := map[string]bool{}
	for _, e := range entries {
		for _, f := range e.Facts {
			for _, ev := range f.Evidence {
				if rel, ok := strings.CutPrefix(ev, mcpqual.EvidenceRoot+"/"); ok {
					runs[strings.SplitN(rel, "/", 2)[0]] = true
				}
			}
		}
	}
	des, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(mcpqual.EvidenceRoot)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, d := range des {
		runs[d.Name()] = true
	}
	var out []string
	for run := range runs {
		out = append(out, filepath.Join(root, filepath.FromSlash(mcpqual.EvidenceRoot), run))
	}
	sort.Strings(out)
	return out
}

// catalogPublicationSection is the guide's unique "### Catalog
// publication" subsection, to the next level-2 or level-3 heading or EOF.
func catalogPublicationSection(t *testing.T, guide string) string {
	t.Helper()
	const head = "\n### Catalog publication\n"
	if strings.Count(guide, head) != 1 {
		t.Fatalf("the guide has %d %q headings", strings.Count(guide, head), strings.TrimSpace(head))
	}
	sec := guide[strings.Index(guide, head)+1:]
	body := sec[len(head)-1:]
	end := len(body)
	for _, h := range []string{"\n## ", "\n### "} {
		if i := strings.Index(body, h); i >= 0 && i < end {
			end = i
		}
	}
	return sec[:len(head)-1+end]
}

// readCatalogFiles returns repo's two catalog files.
func readCatalogFiles(t *testing.T, repo string) (string, string) {
	t.Helper()
	j, err1 := os.ReadFile(filepath.Join(repo, mcpqual.CatalogJSONPath))
	m, err2 := os.ReadFile(filepath.Join(repo, mcpqual.CatalogMDPath))
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	return string(j), string(m)
}

// editCatalog rewrites repo's JSON catalog canonically after edit.
func editCatalog(t *testing.T, repo string, edit func([]catalog.Entry)) {
	t.Helper()
	p := filepath.Join(repo, mcpqual.CatalogJSONPath)
	es, err := catalog.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	edit(es)
	b, err := mcpqual.RenderCatalog(es)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// entryNamed returns the entry id of es.
func entryNamed(t *testing.T, es []catalog.Entry, id string) *catalog.Entry {
	t.Helper()
	for i := range es {
		if es[i].ID == id {
			return &es[i]
		}
	}
	t.Fatalf("no %s entry", id)
	return nil
}

// freshOut is a copy of a run's output without its proposed patch.
func freshOut(t *testing.T, out string) string {
	t.Helper()
	cp := filepath.Join(realTemp(t), "copy")
	copyTree(t, out, cp)
	os.RemoveAll(filepath.Join(cp, "proposed"))
	os.Remove(filepath.Join(cp, mcpqual.PatchFileName))
	return cp
}

func pubPtr[T any](v T) *T { return &v }

// managedKeys are the four publisher-managed fact keys (design
// catalog-version, amendment A1).
var managedKeys = []string{"mcp_timeout", "mcp_timeout_override", "mcp_progress_extension", "mcp_config"}

// vendorHeadings are the support catalog's vendor section headings.
var vendorHeadings = map[string]string{"claude": "## Claude Code", "codex": "## OpenAI Codex", "grok": "## Grok Build", "cursor": "## Cursor Agent"}

// timeoutBulletAnchors are the three publisher-rewritten bullets, in order.
var timeoutBulletAnchors = []string{"- **MCP call timeout:", "- **How to raise it:", "- **Progress extends calls:"}

// sameFacts reports whether two facts are byte-identical as JSON (status,
// value, ordered evidence and owner).
func sameFacts(a, b catalog.Fact) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// checkPublicationBaseline (amendment A1) proves the frozen publication
// baseline still matches the earlier historical freezes: the six
// Claude/Codex and twelve timeout facts (with their short-poll suffix
// restored to the retired interim sentence, 2 and 4 of them) and Grok's
// and Cursor's baseline mcp_config with the current runbook facts of
// entries. Managed facts always come from the immutable baseline, never
// from entries.
func checkPublicationBaseline(entries []catalog.Entry) error {
	b := catalog.PublicationBaselineFacts()
	byID := map[string]catalog.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	timeouts := func(ids ...string) []catalog.Fact {
		var out []catalog.Fact
		for _, id := range ids {
			for _, k := range mcpqual.TimeoutKeys {
				out = append(out, b[id][k])
			}
		}
		return out
	}
	digest := func(v any) string { j, _ := json.Marshal(v); return sha(j) }
	cc, n := preShortPoll(timeouts("claude", "codex"))
	if n != 2 || digest(cc) != timeoutFactsSHA256 {
		return fmt.Errorf("baseline Claude/Codex timeout facts: %d restored, %s", n, digest(cc))
	}
	all, n := preShortPoll(timeouts("claude", "codex", "grok", "cursor"))
	if n != 4 || digest(all) != wave2TimeoutsSHA256 {
		return fmt.Errorf("baseline timeout facts: %d restored, %s", n, digest(all))
	}
	var coord []catalog.Fact
	for _, id := range []string{"grok", "cursor"} {
		e, ok := byID[id]
		if !ok {
			return fmt.Errorf("no %s entry", id)
		}
		coord = append(coord, b[id]["mcp_config"], e.Facts["runbook"])
	}
	if digest(coord) != wave2CoordSHA256 {
		return fmt.Errorf("baseline Grok/Cursor configuration with the current runbook facts: %s", digest(coord))
	}
	return nil
}

// validatePublishedCatalog (design catalog-version, amendment A1) accepts
// entries, the catalog of root, only when every vendor's four managed
// facts either equal the frozen baseline exactly or equal, with the
// vendor's three support-catalog bullets, the exact regeneration by
// ProposePatch from the baseline through the vendor's ordered history of
// validated, receipted, single-client runs. t supplies scratch directories
// only; root is never modified.
func validatePublishedCatalog(t *testing.T, root string, entries []catalog.Entry) error {
	t.Helper()
	if err := catalog.Validate(root, entries); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	md, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogMDPath)))
	if err != nil {
		return err
	}
	if err := mcpqual.CheckShortPollCatalog(string(md), entries); err != nil {
		return fmt.Errorf("short-poll policy: %w", err)
	}
	baseline := catalog.PublicationBaselineFacts()
	for _, e := range entries {
		unchanged := len(baseline[e.ID]) == len(managedKeys)
		for _, k := range managedKeys {
			unchanged = unchanged && sameFacts(e.Facts[k], baseline[e.ID][k])
		}
		if unchanged {
			// An unpublished vendor also keeps its baseline bullets.
			got, err := vendorBullets(string(md), e.ID)
			if err != nil {
				return err
			}
			if got != strings.Join(catalog.PublicationBaselineBullets()[e.ID], "\n") {
				return fmt.Errorf("%s: the support catalog's timeout bullets differ from the frozen baseline", e.ID)
			}
			continue
		}
		if err := replayVendor(t, root, entries, e, string(md)); err != nil {
			return fmt.Errorf("%s: %w", e.ID, err)
		}
	}
	return nil
}

// replayVendor regenerates vendor e's managed facts and bullets from its
// publication history and compares them with the current ones.
func replayVendor(t *testing.T, root string, entries []catalog.Entry, e catalog.Entry, md string) error {
	t.Helper()
	pred := catalog.PublicationBaselineFacts()[e.ID]
	cur, prefix := e.Facts["mcp_timeout"].Evidence, pred["mcp_timeout"].Evidence
	if len(cur) < len(prefix) || !slices.Equal(cur[:len(prefix)], prefix) {
		return errors.New("mcp_timeout no longer starts with its baseline evidence")
	}
	var runs []string
	seen := map[string]bool{}
	for _, ev := range cur[len(prefix):] {
		rel, ok := strings.CutPrefix(ev, mcpqual.EvidenceRoot+"/")
		run, file, ok2 := strings.Cut(rel, "/")
		if !ok || !ok2 || file != "report.json" || run == "" || catalog.CheckEvidencePath(ev) != nil {
			return fmt.Errorf("history entry %q is not a qualification report", ev)
		}
		if seen[run] {
			return fmt.Errorf("history repeats run %s", run)
		}
		seen[run] = true
		runs = append(runs, run)
	}
	if len(runs) == 0 {
		return errors.New("managed facts differ from the baseline with no publication history")
	}
	for _, k := range managedKeys {
		for _, ev := range e.Facts[k].Evidence {
			if rel, ok := strings.CutPrefix(ev, mcpqual.EvidenceRoot+"/"); ok {
				if run, file, _ := strings.Cut(rel, "/"); !seen[run] || file != "report.json" {
					return fmt.Errorf("%s cites %s outside its publication history", k, ev)
				}
			}
		}
	}
	for _, run := range runs {
		dir := filepath.Join(root, filepath.FromSlash(mcpqual.EvidenceRoot), run)
		if err := validatePublicationReceipt(dir); err != nil {
			return fmt.Errorf("run %s: %w", run, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "report.json"))
		if err != nil {
			return err
		}
		rep, err := mcpqual.ParseReport(b)
		switch {
		case err != nil:
			return fmt.Errorf("run %s: %w", run, err)
		case len(rep.Clients) != 1 || rep.Clients[0].ID != e.ID:
			return fmt.Errorf("run %s is not a single-client %s run", run, e.ID)
		case rep.Clients[0].ObservedVersion == nil || *rep.Clients[0].ObservedVersion != e.CoordinatorVersion:
			return fmt.Errorf("run %s did not observe the coordinator_version %q", run, e.CoordinatorVersion)
		case !rep.Cleanup.OK || rep.Interrupted:
			return fmt.Errorf("run %s did not end cleanly", run)
		}
	}
	finalMD := md
	for _, run := range runs {
		var err error
		if pred, finalMD, err = replayRun(t, root, entries, e.ID, pred, run, md); err != nil {
			return fmt.Errorf("run %s: %w", run, err)
		}
	}
	for _, k := range managedKeys {
		if !sameFacts(e.Facts[k], pred[k]) {
			return fmt.Errorf("%s differs from its regeneration from the publication history", k)
		}
	}
	want, err := vendorBullets(finalMD, e.ID)
	if err != nil {
		return err
	}
	got, err := vendorBullets(md, e.ID)
	if err != nil {
		return err
	}
	if got != want {
		return errors.New("the support catalog's timeout bullets differ from their regeneration")
	}
	return nil
}

// replayRun proposes run's committed package (without its receipt) against
// a scratch predecessor catalog: the current entries with every vendor's
// managed facts at the baseline, except id's at pred, and the current
// Markdown, with every referenced evidence file copied. It returns id's
// regenerated managed facts and the proposal's Markdown.
func replayRun(t *testing.T, root string, entries []catalog.Entry, id string, pred map[string]catalog.Fact, run, md string) (map[string]catalog.Fact, string, error) {
	t.Helper()
	jb, err := mcpqual.RenderCatalog(entries)
	if err != nil {
		return nil, "", err
	}
	es, err := catalog.Decode(jb)
	if err != nil {
		return nil, "", err
	}
	baseline := catalog.PublicationBaselineFacts()
	for i := range es {
		facts := baseline[es[i].ID]
		if es[i].ID == id {
			facts = pred
		}
		for _, k := range managedKeys {
			es[i].Facts[k] = facts[k]
		}
	}
	repo, out := realTemp(t), realTemp(t)
	if jb, err = mcpqual.RenderCatalog(es); err != nil {
		return nil, "", err
	}
	write := func(rel string, b []byte) error {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, b, 0o644)
	}
	if err := errors.Join(write(mcpqual.CatalogJSONPath, jb), write(mcpqual.CatalogMDPath, []byte(md))); err != nil {
		return nil, "", err
	}
	for _, e := range es {
		for _, f := range e.Facts {
			for _, ev := range f.Evidence {
				b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ev)))
				if err != nil {
					return nil, "", fmt.Errorf("evidence %s: %w", ev, err)
				}
				if err := write(ev, b); err != nil {
					return nil, "", err
				}
			}
		}
	}
	pkg := filepath.Join(root, filepath.FromSlash(mcpqual.EvidenceRoot), run)
	err = filepath.WalkDir(pkg, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(pkg, p)
		if rel == publicationReceiptName {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return nil, "", err
	}
	base, err := mcpqual.ReadCatalogBase(repo)
	if err != nil {
		return nil, "", err
	}
	rb, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		return nil, "", err
	}
	rep, err := mcpqual.ParseReport(rb)
	if err != nil {
		return nil, "", err
	}
	if _, err := mcpqual.ProposePatch(rep, base, out); err != nil {
		return nil, "", fmt.Errorf("proposal: %w", err)
	}
	final, err := catalog.Load(filepath.Join(out, "proposed", "support-catalog.json"))
	if err != nil {
		return nil, "", err
	}
	pmd, err := os.ReadFile(filepath.Join(out, "proposed", "support-catalog.md"))
	if err != nil {
		return nil, "", err
	}
	next := map[string]catalog.Fact{}
	for _, f := range final {
		if f.ID == id {
			for _, k := range managedKeys {
				next[k] = f.Facts[k]
			}
		}
	}
	return next, string(pmd), nil
}

// vendorBullets is vendor id's three timeout bullets of the support
// catalog Markdown md, each anchored exactly once in its section.
func vendorBullets(md, id string) (string, error) {
	head := "\n" + vendorHeadings[id] + "\n"
	if strings.Count(md, head) != 1 {
		return "", fmt.Errorf("the support catalog has no single %s section", id)
	}
	sec := md[strings.Index(md, head)+1:]
	if i := strings.Index(sec[1:], "\n## "); i >= 0 {
		sec = sec[:i+1]
	}
	var out []string
	for _, anchor := range timeoutBulletAnchors {
		var found []string
		for _, l := range strings.Split(sec, "\n") {
			if strings.HasPrefix(l, anchor) {
				found = append(found, l)
			}
		}
		if len(found) != 1 {
			return "", fmt.Errorf("%s has %d %q bullets", id, len(found), anchor)
		}
		out = append(out, found[0])
	}
	return strings.Join(out, "\n"), nil
}

// workerView is the scratch proof's local nine-fact and identity
// projection (the frozen catalog-package projection's field set: id,
// version, platform and the nine non-publisher facts, plus the coordinator
// identity, which publication never changes either).
func workerView(t *testing.T, entries []catalog.Entry) string {
	t.Helper()
	type view struct {
		ID, Version, CoordinatorVersion, Platform string
		Facts                                     map[string]catalog.Fact
	}
	var out []view
	for _, e := range entries {
		v := view{e.ID, e.Version, e.CoordinatorVersion, e.Platform, map[string]catalog.Fact{}}
		for k, f := range e.Facts {
			if !slices.Contains(managedKeys, k) {
				v.Facts[k] = f
			}
		}
		if len(v.Facts) != 9 {
			t.Fatalf("%s has %d non-publisher facts", e.ID, len(v.Facts))
		}
		out = append(out, v)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// copyRepoTree copies a scratch repository, recreating its symbolic links
// (the read-only worker evidence link) instead of following them.
func copyRepoTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, dst)
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// a1Repo is a scratch repository at the frozen pre-publication baseline
// with the checked-in worker and coordinator identities.
func a1Repo(t *testing.T) string {
	t.Helper()
	root := testkit.MustRepoRoot(t)
	real, err := catalog.Load(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	repo := qualRepo(t)
	editCatalog(t, repo, func(es []catalog.Entry) {
		for i := range es {
			es[i].CoordinatorVersion = entryNamed(t, real, es[i].ID).CoordinatorVersion
		}
	})
	return repo
}

// checkGuideRows runs the shared setup-guide row validation over the
// guide of root against entries, with every linked file present.
func checkGuideRows(t *testing.T, root, guide string, entries []catalog.Entry) error {
	t.Helper()
	secs, err := mcpqual.ParseSetupDoc(guide)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := secs[e.ID]
		if !strings.Contains(s.Text, mcpqual.TimeoutStatusSentence) {
			return fmt.Errorf("%s section lacks the status sentence", e.ID)
		}
		if err := mcpqual.CheckSetupFactRows(s, e); err != nil {
			return err
		}
		for _, f := range s.Facts {
			for _, ev := range f.Evidence {
				if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(ev))); err != nil || st.Size() == 0 {
					return fmt.Errorf("%s links missing %s", e.ID, ev)
				}
			}
		}
	}
	return nil
}

// installedPackage runs one fake qualification with --publish-catalog and
// returns its installed run directory (a temporary repository).
func installedPackage(t *testing.T) string {
	t.Helper()
	q := newQualEnv(t)
	repo := qualRepo(t)
	out := filepath.Join(q.dir, "out")
	r := q.qualify(qualPlan(q.fakeClient("claude", nil, map[string]any{"default": map[string]any{"delays_ms": []int{10}}})), out, "", nil, "--publish-catalog", repo)
	if r.code != 0 || !strings.Contains(r.stdout, "published run") {
		t.Fatalf("qualify = %+v", r)
	}
	q.groupsGone()
	return filepath.Join(repo, filepath.FromSlash(mcpqual.EvidenceRoot), readReport(t, out).RunID)
}

// FP-1 (TestFP7CatalogContract/catalog-version-model): the schema's four
// exact identity migrations, the coordinator identities' enrolled
// exact-version provenance, the frozen worker projection and worker
// capture provenance (the catalog package's contracts), the thirteen facts
// and owners, the documented labels and historical limits, and every
// committed qualification run's receipt and inventory.
func catalogVersionModel(t *testing.T, root string, entries []catalog.Entry) {
	if len(entries) != len(catalogIdentities) {
		t.Fatalf("%d entries", len(entries))
	}
	idx, err := mcpqual.ParseEnrollmentIndex(repoFile(t, mcpqual.EnrollmentIndexPath))
	if err != nil || len(idx.Entries) != 4 {
		t.Fatalf("index %v", err)
	}
	for i, w := range catalogIdentities {
		e := entries[i]
		if e.ID != w.id || e.Version != w.worker || e.CoordinatorVersion != w.coordinator || e.Platform != "linux/amd64" || len(e.Facts) != len(catalog.RequiredFacts) {
			t.Fatalf("entry %d: %q %q %q %q", i, e.ID, e.Version, e.CoordinatorVersion, e.Platform)
		}
		for _, key := range catalog.RequiredFacts {
			if e.Facts[key].VerificationIteration != catalog.Owner(e.ID, key) {
				t.Fatalf("%s.%s owner %s", e.ID, key, e.Facts[key].VerificationIteration)
			}
		}
		// The publication target is an enrolled exact-version identity; a
		// worker version that differs from it is not.
		enrolled := func(version string) bool {
			return slices.ContainsFunc(idx.Entries, func(x mcpqual.EnrollmentEntry) bool {
				return x.Client == e.ID && x.Version == version && x.Platform == "linux/amd64" && x.Fixture == mcpqual.EnrolledFixtureID(x.Decoder, version, "linux/amd64")
			})
		}
		if !enrolled(e.CoordinatorVersion) || e.Version != e.CoordinatorVersion && enrolled(e.Version) {
			t.Fatalf("%s: coordinator %q / worker %q enrollment", e.ID, e.CoordinatorVersion, e.Version)
		}
	}
	// The frozen worker projection, worker capture provenance and schema
	// unit contracts, run from the catalog package itself.
	contractRun(t, contractBinary(t, "./internal/testkit/catalog"), "./internal/testkit/catalog", "^(TestRealAdapterEvidence|TestWorkerCatalogProjection|TestCatalogVersionModel)$",
		os.Environ(), "TestRealAdapterEvidence", "TestWorkerCatalogProjection", "TestCatalogVersionModel")
	// Documentation labels both identities and their limits.
	md := string(repoFile(t, mcpqual.CatalogMDPath))
	heads := map[string]string{"claude": "## Claude Code", "codex": "## OpenAI Codex", "grok": "## Grok Build", "cursor": "## Cursor Agent"}
	for _, w := range catalogIdentities {
		sec := md[strings.Index(md, "\n"+heads[w.id]+"\n"):]
		sec = sec[:strings.Index(sec[1:], "\n## ")+1]
		wi, ci := strings.Index(sec, "**Worker version:** `"+w.worker+"`"), strings.Index(sec, "**MCP publication version:** `"+w.coordinator+"` (`coordinator_version`")
		if wi < 0 || ci < wi || !strings.Contains(sec, "exact-version decoder evidence, not timeout evidence") {
			t.Fatalf("support-catalog.md %s labels: worker %d, publication %d", w.id, wi, ci)
		}
	}
	for _, s := range []string{"four entries (`id`, `version`, `coordinator_version`, `platform`, `facts`, in that canonical order)", "**Worker version** (`version`)",
		"**MCP publication version** (`coordinator_version`)", "`entry <i> (<id>): missing coordinator_version`", "There is no fallback to `version` and no runtime migration",
		"It is not a worker qualification", "`runbook` and the historical part of `mcp_config` are not re-certified by `coordinator_version`",
		"Publication rewrites exactly the three timeout bullets", "worker execution is refused", "A published coordinator fact never lifts the worker refusal"} {
		if !strings.Contains(md, s) {
			t.Fatalf("support-catalog.md lacks %q", s)
		}
	}
	secs, err := mcpqual.ParseSetupDoc(string(repoFile(t, mcpqual.SetupDocPath)))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range catalogIdentities {
		s := secs[w.id].Text
		if !strings.Contains(s, "worker `version` is ") || !strings.Contains(s, "`"+w.worker+"`") || !strings.Contains(s, "`coordinator_version`, is ") ||
			!strings.Contains(s, "`"+w.coordinator+"`, the enrolled exact version") || !strings.Contains(s, "this coordinator help was not recaptured from it") {
			t.Fatalf("coordinator.md %s section identities", w.id)
		}
	}
	// Every committed qualification run carries a valid receipt (none
	// before the evidence pull request).
	for _, dir := range publicationRunDirs(t, root, entries) {
		if err := validatePublicationReceipt(dir); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
	}
	// Amendment A1: the historical freezes still hold for the frozen
	// baseline, and every real committed managed fact is the baseline or
	// its exact regeneration.
	if err := checkPublicationBaseline(entries); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedCatalog(t, root, entries); err != nil {
		t.Fatal(err)
	}
}

// FP-2 (TestMCPQualificationPublish/coordinator-version): publication
// admits a run only on the entry's explicit coordinator identity. repo is
// the parent's published scratch repository and out its run.
func publishCoordinatorVersion(t *testing.T, repo, out string) {
	root := testkit.MustRepoRoot(t)
	observed := fakeVersions["claude"]
	real, err := catalog.Load(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	// The successful explicit-target install: worker versions retained,
	// coordinator versions the fakes' observations, canonical order.
	pub, err := catalog.Load(filepath.Join(repo, filepath.FromSlash(mcpqual.CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	for i := range real {
		if pub[i].ID != real[i].ID || pub[i].Version != real[i].Version || pub[i].CoordinatorVersion != fakeVersions[real[i].ID] || pub[i].Platform != real[i].Platform {
			t.Fatalf("installed %s identities %q %q", pub[i].ID, pub[i].Version, pub[i].CoordinatorVersion)
		}
	}
	if c := entryNamed(t, pub, "claude"); c.Version == c.CoordinatorVersion || c.CoordinatorVersion != observed {
		t.Fatalf("claude %q %q: the published run did not exercise differing identities", c.Version, c.CoordinatorVersion)
	}
	jb, _ := readCatalogFiles(t, repo)
	if r, err := mcpqual.RenderCatalog(pub); err != nil || string(r) != jb ||
		!strings.Contains(jb, "\"version\": \"2.1.285 (Claude Code)\",\n    \"coordinator_version\": \""+observed+"\",\n    \"platform\": \"linux/amd64\",\n    \"facts\": {") {
		t.Fatalf("installed catalog is not canonical with id, version, coordinator_version, platform, facts: %v", err)
	}
	// propose proposes the parent's run against a fresh scratch catalog,
	// requiring that no repository file is written either way.
	propose := func(t *testing.T, editRepo func([]catalog.Entry), editBase func(*mcpqual.CatalogBase), editRep func(*mcpqual.Report)) (*mcpqual.CatalogBase, *mcpqual.Patch, string, error) {
		t.Helper()
		r := qualRepo(t)
		if editRepo != nil {
			editCatalog(t, r, editRepo)
		}
		base, err := mcpqual.ReadCatalogBase(r)
		if err != nil {
			t.Fatal(err)
		}
		if editBase != nil {
			editBase(base)
		}
		cp := freshOut(t, out)
		rep := readReport(t, cp)
		if editRep != nil {
			editRep(rep)
		}
		j0, m0 := readCatalogFiles(t, r)
		p, err := mcpqual.ProposePatch(rep, base, cp)
		if j1, m1 := readCatalogFiles(t, r); j1 != j0 || m1 != m0 {
			t.Fatal("a proposal wrote the repository's catalog")
		}
		if _, serr := os.Stat(filepath.Join(r, filepath.FromSlash(mcpqual.EvidenceRoot))); serr == nil {
			t.Fatal("a proposal installed evidence")
		}
		return base, p, cp, err
	}
	t.Run("differing-identities", func(t *testing.T) {
		base, p, cp, err := propose(t, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		fresh, _ := catalog.Decode(base.JSON)
		final, err := catalog.Load(filepath.Join(cp, "proposed", "support-catalog.json"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range fresh {
			b, f := base.Entries[i], final[i]
			if b.Version != fresh[i].Version || b.CoordinatorVersion != fresh[i].CoordinatorVersion || f.Version != b.Version || f.CoordinatorVersion != b.CoordinatorVersion {
				t.Fatalf("%s identities changed by the proposal", b.ID)
			}
			for key, fact := range fresh[i].Facts {
				if string(mustMarshal(t, base.Entries[i].Facts[key])) != string(mustMarshal(t, fact)) {
					t.Fatalf("the base's %s.%s changed", b.ID, key)
				}
			}
		}
		for _, fc := range p.Facts {
			if fc.Client != "claude" || !slices.Contains(append(slices.Clone(mcpqual.TimeoutKeys), "mcp_config"), fc.Key) {
				t.Fatalf("patch touches %s.%s", fc.Client, fc.Key)
			}
		}
	})
	t.Run("equal-identities", func(t *testing.T) {
		if _, _, _, err := propose(t, func(es []catalog.Entry) { entryNamed(t, es, "claude").Version = observed }, nil, nil); err != nil {
			t.Fatal(err)
		}
	})
	// Refusals: the existing conflict (exit 4) naming the vendor and the
	// expected coordinator identity.
	for name, target := range map[string]string{"newer-target": "2.1.283 (Claude Code)", "older-target": "2.1.281 (Claude Code)", "case-differs": "2.1.282 (claude code)",
		"trailing-space": observed + " "} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := propose(t, func(es []catalog.Entry) { entryNamed(t, es, "claude").CoordinatorVersion = target }, nil, nil)
			if contract.ExitCode(err) != 4 || !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), fmt.Sprintf("%q", target)) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("worker-only-match", func(t *testing.T) {
		_, _, _, err := propose(t, func(es []catalog.Entry) {
			c := entryNamed(t, es, "claude")
			c.Version, c.CoordinatorVersion = observed, mcpqual.ClaudeRealVersion
		}, nil, nil)
		if contract.ExitCode(err) != 4 || !strings.Contains(err.Error(), "coordinator_version \""+mcpqual.ClaudeRealVersion+"\"") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing-observed", func(t *testing.T) {
		_, _, _, err := propose(t, nil, nil, func(rep *mcpqual.Report) {
			for i := range rep.Clients {
				rep.Clients[i].ObservedVersion = nil
			}
		})
		if contract.ExitCode(err) != 4 || !strings.Contains(err.Error(), "observed version") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("blank-target", func(t *testing.T) {
		for _, blank := range []string{"", " \t"} {
			_, _, _, err := propose(t, nil, func(b *mcpqual.CatalogBase) { entryNamed(t, b.Entries, "claude").CoordinatorVersion = blank }, nil)
			if contract.ExitCode(err) != 4 || !strings.Contains(err.Error(), "no coordinator_version") {
				t.Fatalf("%q: err = %v", blank, err)
			}
		}
	})
	t.Run("invalid-catalog", func(t *testing.T) {
		// ReadCatalogBase refuses a missing, empty or whitespace-only
		// identity as invalid input.
		for name, edit := range map[string]func(string) string{
			"missing": func(s string) string {
				return strings.Replace(s, "\n    \"coordinator_version\": \""+observed+"\",", "", 1)
			},
			"empty": func(s string) string {
				return strings.Replace(s, "\"coordinator_version\": \""+observed+"\"", "\"coordinator_version\": \"\"", 1)
			},
			"whitespace": func(s string) string {
				return strings.Replace(s, "\"coordinator_version\": \""+observed+"\"", "\"coordinator_version\": \"  \"", 1)
			},
		} {
			r := qualRepo(t)
			j, _ := readCatalogFiles(t, r)
			if edited := edit(j); edited == j {
				t.Fatalf("%s: no edit", name)
			} else if err := os.WriteFile(filepath.Join(r, filepath.FromSlash(mcpqual.CatalogJSONPath)), []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := mcpqual.ReadCatalogBase(r); contract.ExitCode(err) != 2 || !strings.Contains(err.Error(), "entry 0 (claude): missing coordinator_version") {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
		// The CLI refuses it before any fake session (or version) launch
		// and before creating the output directory.
		q2 := newQualEnv(t)
		r := qualRepo(t)
		editCatalog(t, r, func(es []catalog.Entry) { entryNamed(t, es, "claude").CoordinatorVersion = "" })
		j0, m0 := readCatalogFiles(t, r)
		out2 := filepath.Join(q2.dir, "out")
		res := q2.qualify(qualPlan(q2.fakeClient("claude", nil, nil)), out2, "", nil, "--publish-catalog", r)
		if res.code != 2 || !strings.Contains(res.stderr, "missing coordinator_version") || len(q2.launchLog()) != 0 {
			t.Fatalf("invalid catalog qualify = %+v, launches %v", res, q2.launchLog())
		}
		if _, err := os.Stat(out2); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the output directory exists: %v", err)
		}
		if j1, m1 := readCatalogFiles(t, r); j1 != j0 || m1 != m0 {
			t.Fatal("the refused catalog changed")
		}
		q2.groupsGone()
	})
}

// FP-4 (TestMCPQualificationPublish/evidence-package): the parent's
// installed run directory holds exactly the explicit file set with the
// output's bytes and hashes, no private, raw or workspace file, no secret
// or home path, and every committed link resolves.
func publishEvidencePackage(t *testing.T, q *qualEnv, repo, out, secret string) {
	rep := readReport(t, out)
	evRoot := filepath.Join(repo, filepath.FromSlash(mcpqual.EvidenceRoot))
	evDir := filepath.Join(evRoot, rep.RunID)
	want := map[string]bool{"report.json": true, "report.md": true, "manifest.json": true}
	for _, ev := range rep.Evidence {
		want[ev.Path] = true
	}
	if runs, err := os.ReadDir(evRoot); err != nil || len(runs) != 1 || runs[0].Name() != rep.RunID {
		t.Fatalf("evidence root holds %v (%v)", runs, err)
	}
	got := map[string]bool{}
	filepath.WalkDir(evDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(evDir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".work" || rel == "proposed" || strings.HasPrefix(rel, ".") && rel != "." {
				t.Errorf("installed directory %s", rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			t.Errorf("installed %s is not a regular file", rel)
			return nil
		}
		b, _ := os.ReadFile(p)
		src, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(b, src) {
			t.Errorf("installed %s differs from the sanitized output: %v", rel, err)
		}
		if bytes.Contains(b, []byte(secret)) || bytes.Contains(b, []byte(q.home)) {
			t.Errorf("installed %s keeps a secret or the home path", rel)
		}
		got[rel] = true
		return nil
	})
	if len(got) != len(want) {
		t.Fatalf("installed %v, want exactly %v", got, want)
	}
	for p := range want {
		if !got[p] {
			t.Fatalf("%s was not installed", p)
		}
	}
	// What the output holds besides the package (its proposed catalogs and
	// patch; the run already removed its .work tree) never spills.
	for _, private := range []string{"proposed", mcpqual.PatchFileName} {
		if _, err := os.Stat(filepath.Join(out, private)); err != nil {
			t.Fatalf("the output lacks its private %s, so the spill check proves nothing: %v", private, err)
		}
	}
	redacted := false
	for p := range got {
		b, _ := os.ReadFile(filepath.Join(evDir, filepath.FromSlash(p)))
		redacted = redacted || bytes.Contains(b, []byte(mcpqual.Redacted))
	}
	if !redacted {
		t.Fatal("no sanitized sentinel in the installed package")
	}
	// The manifest binds every file's size and hash, and a synthetic
	// receipt over a copy of exactly this package validates.
	var man mcpqual.Manifest
	if err := json.Unmarshal(repoFileAt(t, evDir, "manifest.json"), &man); err != nil || len(man.Files) != len(want)-1 {
		t.Fatalf("manifest %v %d", err, len(man.Files))
	}
	for _, f := range man.Files {
		b := repoFileAt(t, evDir, f.Path)
		if sha(b) != f.SHA256 || int64(len(b)) != f.Bytes {
			t.Fatalf("%s differs from the manifest", f.Path)
		}
	}
	cp := filepath.Join(realTemp(t), rep.RunID)
	copyTree(t, evDir, cp)
	writeSyntheticReceipt(t, cp, nil)
	if err := validatePublicationReceipt(cp); err != nil {
		t.Fatalf("a synthetic receipt over the installed package: %v", err)
	}
	// Every committed link resolves.
	md := filepath.Join(repo, filepath.FromSlash(mcpqual.CatalogMDPath))
	if err := catalog.CheckDocLinks(md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(repoFileAt(t, filepath.Dir(md), filepath.Base(md))), "](../"+mcpqual.EvidenceRoot+"/"+rep.RunID+"/report.json)") {
		t.Fatal("the Markdown does not link the installed report")
	}
	repoFacts(t, repo)
}

// FP-7 (TestMCPQualificationPublish/publication-recovery): publish only
// retries the run's own patch; base and final mixed states recover;
// an edited identity or a different evidence destination is refused
// without clobbering; no patch, no publication; sequential runs chain
// their bases.
func publishRecovery(t *testing.T, q *qualEnv, repo, out string) {
	first, err := mcpqual.LoadPatch(out)
	if err != nil {
		t.Fatal(err)
	}
	finalJSON := string(repoFileAt(t, filepath.Join(out, "proposed"), "support-catalog.json"))
	finalMD := string(repoFileAt(t, filepath.Join(out, "proposed"), "support-catalog.md"))
	publish := func(o, r string) result {
		return runBin(t, qualBinary(t), q.dir, q.env(""), "publish", "--out", o, "--repo", r)
	}
	atBase := func(t *testing.T, r string) {
		t.Helper()
		j, m := readCatalogFiles(t, r)
		if sha([]byte(j)) != first.BaseJSONSHA256 || sha([]byte(m)) != first.BaseMarkdownSHA256 {
			t.Fatal("a fresh scratch catalog is not the patch's base")
		}
	}
	evDest := func(r string) string { return filepath.Join(r, filepath.FromSlash(mcpqual.EvidenceRoot), first.RunID) }
	t.Run("idempotent-retry", func(t *testing.T) {
		j0, m0 := readCatalogFiles(t, repo)
		if res := publish(out, repo); res.code != 0 {
			t.Fatalf("retry = %+v", res)
		}
		if j1, m1 := readCatalogFiles(t, repo); j1 != j0 || m1 != m0 || j1 != finalJSON || m1 != finalMD {
			t.Fatal("a retry of an installed patch changed the catalog")
		}
	})
	for name, write := range map[string]func(r string){
		"json-final-markdown-base": func(r string) {
			os.WriteFile(filepath.Join(r, filepath.FromSlash(mcpqual.CatalogJSONPath)), []byte(finalJSON), 0o644)
		},
		"markdown-final-json-base": func(r string) {
			os.WriteFile(filepath.Join(r, filepath.FromSlash(mcpqual.CatalogMDPath)), []byte(finalMD), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := qualRepo(t)
			atBase(t, r)
			write(r)
			if res := publish(out, r); res.code != 0 {
				t.Fatalf("mixed-state retry = %+v", res)
			}
			if j, m := readCatalogFiles(t, r); j != finalJSON || m != finalMD {
				t.Fatal("the mixed state did not reach the intended final bytes")
			}
			if !bytes.Equal(repoFileAt(t, evDest(r), "report.json"), repoFileAt(t, out, "report.json")) {
				t.Fatal("the evidence was not installed")
			}
		})
	}
	t.Run("identity-edited-after-base", func(t *testing.T) {
		for _, edit := range []func(*catalog.Entry){
			func(e *catalog.Entry) { e.CoordinatorVersion += " edited" },
			func(e *catalog.Entry) { e.Version = e.CoordinatorVersion },
		} {
			r := qualRepo(t)
			atBase(t, r)
			editCatalog(t, r, func(es []catalog.Entry) { edit(entryNamed(t, es, "claude")) })
			j0, m0 := readCatalogFiles(t, r)
			res := publish(out, r)
			if res.code != 4 || !strings.Contains(res.stderr, "changed since the run started") {
				t.Fatalf("edited identity publish = %+v", res)
			}
			if j1, m1 := readCatalogFiles(t, r); j1 != j0 || m1 != m0 {
				t.Fatal("an edited catalog was overwritten")
			}
			if _, err := os.Stat(filepath.Join(r, filepath.FromSlash(mcpqual.EvidenceRoot))); err == nil {
				t.Fatal("evidence installed despite the conflict")
			}
		}
	})
	t.Run("evidence-destination-mismatch", func(t *testing.T) {
		r := qualRepo(t)
		atBase(t, r)
		dst := filepath.Join(mkdir(t, evDest(r)), "report.md")
		os.WriteFile(dst, []byte("an owner's unrelated bytes\n"), 0o644)
		res := publish(out, r)
		if res.code != 4 || !strings.Contains(res.stderr, "already exists with different bytes") {
			t.Fatalf("mismatched destination publish = %+v", res)
		}
		if string(repoFileAt(t, filepath.Dir(dst), "report.md")) != "an owner's unrelated bytes\n" {
			t.Fatal("the destination was clobbered")
		}
		atBase(t, r)
		if _, err := os.Stat(filepath.Join(evDest(r), "report.json")); err == nil {
			t.Fatal("other evidence installed despite the conflict")
		}
	})
	t.Run("no-retroactive-publication", func(t *testing.T) {
		// A run output without its original-invocation patch (as every run
		// qualified without --publish-catalog) cannot be published later.
		cp := freshOut(t, out)
		r := qualRepo(t)
		res := publish(cp, r)
		if res.code != 2 || !strings.Contains(res.stderr, "no proposed catalog patch") {
			t.Fatalf("patchless publish = %+v", res)
		}
		atBase(t, r)
		if _, err := os.Stat(filepath.Join(r, filepath.FromSlash(mcpqual.EvidenceRoot))); err == nil {
			t.Fatal("a patchless run installed evidence")
		}
	})
	t.Run("sequential-bases", func(t *testing.T) {
		r := qualRepo(t)
		if res := publish(out, r); res.code != 0 {
			t.Fatalf("install = %+v", res)
		}
		q2 := newQualEnv(t)
		out2 := filepath.Join(q2.dir, "out")
		res := q2.qualify(qualPlan(q2.fakeClient("codex", nil, map[string]any{"default": map[string]any{"delays_ms": []int{10}}})), out2, "", nil, "--publish-catalog", r)
		q2.groupsGone()
		if res.code != 0 || !strings.Contains(res.stdout, "published run") {
			t.Fatalf("second qualify = %+v", res)
		}
		second, err := mcpqual.LoadPatch(out2)
		if err != nil || second.BaseJSONSHA256 != first.FinalJSONSHA256 || second.BaseMarkdownSHA256 != first.FinalMarkdownSHA256 || second.RunID == first.RunID {
			t.Fatalf("second patch %+v (%v)", second, err)
		}
		firstFinal, _ := catalog.Decode([]byte(finalJSON))
		now := repoFacts(t, r)
		for _, key := range append(slices.Clone(mcpqual.TimeoutKeys), "mcp_config") {
			if string(mustMarshal(t, now["claude"].Facts[key])) != string(mustMarshal(t, entryNamed(t, firstFinal, "claude").Facts[key])) {
				t.Fatalf("the second run changed claude.%s", key)
			}
			if string(mustMarshal(t, now["codex"].Facts[key])) == string(mustMarshal(t, entryNamed(t, firstFinal, "codex").Facts[key])) {
				t.Fatalf("the second run did not publish codex.%s", key)
			}
		}
		for _, w := range []struct{ id, worker string }{{"claude", "2.1.285 (Claude Code)"}, {"codex", "codex-cli 0.159.0"}} {
			if now[w.id].Version != w.worker || now[w.id].CoordinatorVersion != fakeVersions[w.id] {
				t.Fatalf("%s identities changed", w.id)
			}
		}
		for _, run := range []string{first.RunID, second.RunID} {
			if _, err := os.Stat(filepath.Join(evDest(r), "..", run, "report.json")); err != nil {
				t.Fatalf("run %s evidence: %v", run, err)
			}
		}
	})
	// Design catalog-version, amendment A1: the stage-2 offline proof that
	// published catalogs need no later re-freeze or guide rewrite.
	publicationA1(t)
}

// publicationA1 is amendment A1's offline proof (under FP-7's
// publication-recovery): the frozen baseline, a generated four-vendor
// publication with ordered history, and the hand edits each shared
// validator rejects.
func publicationA1(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	realEntries, err := catalog.Load(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(repoFile(t, mcpqual.SetupDocPath))
	load := func(t *testing.T, repo string) []catalog.Entry {
		t.Helper()
		es, err := catalog.Load(filepath.Join(repo, filepath.FromSlash(mcpqual.CatalogJSONPath)))
		if err != nil {
			t.Fatal(err)
		}
		return es
	}
	t.Run("a1-baseline", func(t *testing.T) {
		repo := a1Repo(t)
		es := load(t, repo)
		baseline := catalog.PublicationBaselineFacts()
		for _, e := range es {
			for _, k := range managedKeys {
				if !sameFacts(e.Facts[k], baseline[e.ID][k]) {
					t.Fatalf("%s.%s is not the frozen baseline", e.ID, k)
				}
			}
		}
		if err := checkPublicationBaseline(es); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(mcpqual.EvidenceRoot))); err == nil {
			t.Fatal("the baseline scratch repository holds qualification runs")
		}
		if err := validatePublishedCatalog(t, repo, es); err != nil {
			t.Fatalf("the baseline needs no publication evidence: %v", err)
		}
		if workerView(t, es) != workerView(t, realEntries) {
			t.Fatal("the scratch baseline changed a worker fact or identity")
		}
		if err := checkGuideRows(t, repo, guide, es); err != nil {
			t.Fatal(err)
		}
		// Every caller gets its own copy of the baseline.
		mut := catalog.PublicationBaselineFacts()
		f := mut["grok"]["mcp_config"]
		f.Evidence[0] = "changed"
		delete(mut, "claude")
		if again := catalog.PublicationBaselineFacts(); again["grok"]["mcp_config"].Evidence[0] == "changed" || len(again["claude"]) != 4 || checkPublicationBaseline(es) != nil {
			t.Fatal("a caller changed the shared baseline")
		}
	})
	var genRepo string
	var genEntries []catalog.Entry
	runs := map[string]string{"claude": "run-a1-claude", "codex": "run-a1-codex", "grok-1": "run-a1-grok-1", "grok-2": "run-a1-grok-2", "cursor": "run-real-cursor"}
	// The generated repository outlives its subtest: the hand edits copy it.
	repo := a1Repo(t)
	t.Run("a1-generated-publication", func(t *testing.T) {
		view0 := workerView(t, load(t, repo))
		publish := func(t *testing.T, rep *mcpqual.Report, out string, err error) {
			t.Helper()
			if err != nil || rep == nil || len(rep.Clients) != 1 {
				t.Fatalf("report %v", err)
			}
			base, err := mcpqual.ReadCatalogBase(repo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mcpqual.ProposePatch(rep, base, out); err != nil {
				t.Fatal(err)
			}
			if err := mcpqual.Publish(out, repo); err != nil {
				t.Fatal(err)
			}
			writeSyntheticReceipt(t, filepath.Join(repo, filepath.FromSlash(mcpqual.EvidenceRoot), rep.RunID), nil)
		}
		// Single-client runs, interleaved across vendors: Claude's eligible
		// 15 s bound, Grok's unseen-timeout subset (UNVERIFIED), Codex's
		// bound, Grok's later eligible bound, Cursor's prepared bound.
		rep, out, err := realQualifyAs(t, runs["claude"], "claude", "0", "linux", nil)
		publish(t, rep, out, err)
		rep, out, err = realQualifyAs(t, runs["grok-1"], "grok", "10000", "linux", nil)
		if err == nil && rep.Outcome == mcpqual.StatusConclusive {
			t.Fatal("the unseen-timeout grok run is conclusive")
		}
		publish(t, rep, out, err)
		if f := repoFacts(t, repo)["grok"].Facts["mcp_timeout"]; f.Status != catalog.Unverified || !strings.HasPrefix(f.Value, "Established subset from qualification run "+runs["grok-1"]) {
			t.Fatalf("grok subset %+v", f)
		}
		rep, out, err = realQualifyAs(t, runs["codex"], "codex", "0", "linux", nil)
		publish(t, rep, out, err)
		rep, out, err = realQualifyAs(t, runs["grok-2"], "grok", "0", "linux", nil)
		publish(t, rep, out, err)
		rep, out, err = realQualifyCursor(t, map[string]string{"ENABLE": "project", "RESIDUE": "in_place"}, "linux", nil)
		publish(t, rep, out, err)
		es := load(t, repo)
		byID := map[string]catalog.Entry{}
		for _, e := range es {
			byID[e.ID] = e
			if f := e.Facts["mcp_timeout"]; f.Status != catalog.Verified || !strings.Contains(f.Value, "silent tool calls of up to 15000 ms completed (2 observations)") {
				t.Fatalf("%s mcp_timeout %+v", e.ID, f)
			}
		}
		// Grok's ordered history: both reports appended in order, and two
		// accumulated initialize observations in its configuration fact.
		grok := byID["grok"]
		ev := grok.Facts["mcp_timeout"].Evidence
		if len(ev) < 2 || ev[len(ev)-2] != mcpqual.EvidenceRoot+"/"+runs["grok-1"]+"/report.json" || ev[len(ev)-1] != mcpqual.EvidenceRoot+"/"+runs["grok-2"]+"/report.json" ||
			strings.Count(grok.Facts["mcp_config"].Value, "observed initialize clientInfo") != 2 {
			t.Fatalf("grok history %v / %q", ev, grok.Facts["mcp_config"].Value)
		}
		if err := checkPublicationBaseline(es); err != nil {
			t.Fatal(err)
		}
		if err := validatePublishedCatalog(t, repo, es); err != nil {
			t.Fatalf("a generated publication is refused: %v", err)
		}
		if workerView(t, es) != view0 {
			t.Fatal("publication changed a worker fact or identity")
		}
		if err := checkGuideRows(t, repo, guide, es); err != nil {
			t.Fatal(err)
		}
		if string(repoFileAt(t, filepath.Join(repo, "docs"), "coordinator.md")) != guide {
			t.Fatal("publication changed the setup guide")
		}
		genRepo, genEntries = repo, es
	})
	t.Run("a1-hand-edits", func(t *testing.T) {
		if genRepo == "" {
			t.Fatal("no generated publication")
		}
		edited := func(fn func([]catalog.Entry)) []catalog.Entry {
			jb, _ := mcpqual.RenderCatalog(genEntries)
			es, err := catalog.Decode(jb)
			if err != nil {
				t.Fatal(err)
			}
			fn(es)
			return es
		}
		fact := func(es []catalog.Entry, id, key string, fn func(*catalog.Fact)) {
			e := entryNamed(t, es, id)
			f := e.Facts[key]
			f.Evidence = slices.Clone(f.Evidence)
			fn(&f)
			e.Facts[key] = f
		}
		reject := func(name, repo string, es []catalog.Entry) {
			t.Helper()
			err := validatePublishedCatalog(t, repo, es)
			if err == nil {
				t.Errorf("%s: accepted", name)
				return
			}
			t.Logf("%s: rejected: %v", name, err)
		}
		// Each managed fact: value, status, evidence order and owner.
		for _, k := range managedKeys {
			reject("claude."+k+" value", genRepo, edited(func(es []catalog.Entry) { fact(es, "claude", k, func(f *catalog.Fact) { f.Value += " Hand edited." }) }))
			reject("claude."+k+" status", genRepo, edited(func(es []catalog.Entry) {
				fact(es, "claude", k, func(f *catalog.Fact) {
					f.Status = map[string]string{catalog.Verified: catalog.Unverified, catalog.Unverified: catalog.Verified}[f.Status]
				})
			}))
			reject("claude."+k+" evidence order", genRepo, edited(func(es []catalog.Entry) { fact(es, "claude", k, func(f *catalog.Fact) { slices.Reverse(f.Evidence) }) }))
			reject("claude."+k+" owner", genRepo, edited(func(es []catalog.Entry) {
				fact(es, "claude", k, func(f *catalog.Fact) {
					f.VerificationIteration = map[string]string{"07b": "08", "08": "07b"}[f.VerificationIteration]
				})
			}))
		}
		// A plausible measurement sentence with unchanged, matching evidence.
		reject("hand-edited managed fact", genRepo, edited(func(es []catalog.Entry) {
			fact(es, "codex", "mcp_timeout", func(f *catalog.Fact) { f.Value = strings.Replace(f.Value, "up to 15000 ms", "up to 30000 ms", 1) })
		}))
		reject("unsupported config observation", genRepo, edited(func(es []catalog.Entry) {
			fact(es, "codex", "mcp_config", func(f *catalog.Fact) {
				f.Value += " Qualification run " + runs["codex"] + ` observed initialize clientInfo name "codex-cli" and version "1.0.0" (requested_by compatible: true); registration through the vendor command is not qualified by it.`
			})
		}))
		report := func(key string) string { return mcpqual.EvidenceRoot + "/" + runs[key] + "/report.json" }
		reject("dropped history", genRepo, edited(func(es []catalog.Entry) {
			fact(es, "grok", "mcp_timeout", func(f *catalog.Fact) { f.Evidence = f.Evidence[:len(f.Evidence)-1] })
		}))
		reject("reordered history", genRepo, edited(func(es []catalog.Entry) {
			for _, k := range managedKeys {
				fact(es, "grok", k, func(f *catalog.Fact) {
					for i, ev := range f.Evidence {
						switch ev {
						case report("grok-1"):
							f.Evidence[i] = report("grok-2")
						case report("grok-2"):
							f.Evidence[i] = report("grok-1")
						}
					}
				})
			}
		}))
		reject("repeated run", genRepo, edited(func(es []catalog.Entry) {
			fact(es, "claude", "mcp_timeout", func(f *catalog.Fact) { f.Evidence = append(f.Evidence, report("claude")) })
		}))
		reject("foreign report", genRepo, edited(func(es []catalog.Entry) {
			fact(es, "claude", "mcp_timeout_override", func(f *catalog.Fact) { f.Evidence = append(f.Evidence, report("codex")) })
		}))
		reject("another coordinator version", genRepo, edited(func(es []catalog.Entry) { entryNamed(t, es, "claude").CoordinatorVersion = "2.1.293 (Claude Code)" }))
		// An edit with no publication history at all.
		baseRepo := a1Repo(t)
		baseEntries, _ := catalog.Load(filepath.Join(baseRepo, filepath.FromSlash(mcpqual.CatalogJSONPath)))
		fact(baseEntries, "codex", "mcp_timeout", func(f *catalog.Fact) { f.Value = strings.Replace(f.Value, "unknown", "unknown (edited)", 1) })
		reject("unpublished edit", baseRepo, baseEntries)
		// A hand-edited bullet of an unpublished vendor, its facts untouched.
		bulletCopy := filepath.Join(realTemp(t), "repo")
		copyRepoTree(t, baseRepo, bulletCopy)
		mdPath := filepath.Join(bulletCopy, filepath.FromSlash(mcpqual.CatalogMDPath))
		mdb, _ := os.ReadFile(mdPath)
		grokBullet := catalog.PublicationBaselineBullets()["grok"][0]
		if !bytes.Contains(mdb, []byte("\n"+grokBullet+"\n")) {
			t.Fatal("the baseline scratch Markdown lacks grok's baseline bullet")
		}
		os.WriteFile(mdPath, bytes.Replace(mdb, []byte("\n"+grokBullet+"\n"), []byte("\n"+grokBullet+" Measured: 30000 ms.\n"), 1), 0o644)
		baseCatalog, _ := catalog.Load(filepath.Join(baseRepo, filepath.FromSlash(mcpqual.CatalogJSONPath)))
		reject("hand-edited unpublished bullet", bulletCopy, baseCatalog)
		// Worker and runbook values: the worker view and the baseline's
		// runbook freeze reject them.
		if workerView(t, edited(func(es []catalog.Entry) {
			fact(es, "claude", "headless", func(f *catalog.Fact) { f.Value += " Edited." })
		})) == workerView(t, genEntries) {
			t.Error("a worker edit kept the worker view")
		}
		if workerView(t, edited(func(es []catalog.Entry) {
			fact(es, "codex", "runbook", func(f *catalog.Fact) { f.Value += " Edited." })
		})) == workerView(t, genEntries) {
			t.Error("a runbook edit kept the worker view")
		}
		if err := checkPublicationBaseline(edited(func(es []catalog.Entry) { fact(es, "grok", "runbook", func(f *catalog.Fact) { f.Value += " Edited." }) })); err == nil {
			t.Error("a Grok runbook edit kept the baseline coordinator freeze")
		}
		// Filesystem edits on copies of the published repository.
		onCopy := func(name string, fn func(repo string), es []catalog.Entry) {
			t.Helper()
			c := filepath.Join(realTemp(t), "repo")
			copyRepoTree(t, genRepo, c)
			fn(c)
			if es == nil {
				es = genEntries
			}
			reject(name, c, es)
		}
		runDir := func(repo, key string) string {
			return filepath.Join(repo, filepath.FromSlash(mcpqual.EvidenceRoot), runs[key])
		}
		claudeRep := readReport(t, runDir(genRepo, "claude"))
		evPath := func(repo string) string {
			return filepath.Join(runDir(repo, "claude"), filepath.FromSlash(claudeRep.Evidence[0].Path))
		}
		onCopy("missing report", func(c string) { os.Remove(filepath.Join(runDir(c, "claude"), "report.json")) }, nil)
		onCopy("missing receipt", func(c string) { os.Remove(filepath.Join(runDir(c, "claude"), publicationReceiptName)) }, nil)
		onCopy("evidence bytes", func(c string) {
			b, _ := os.ReadFile(evPath(c))
			os.WriteFile(evPath(c), append(b, '\n'), 0o644)
		}, nil)
		onCopy("evidence bytes with a recomputed receipt", func(c string) {
			b, _ := os.ReadFile(evPath(c))
			os.WriteFile(evPath(c), append(b, '\n'), 0o644)
			writeSyntheticReceipt(t, runDir(c, "claude"), nil)
		}, nil)
		onCopy("hand-edited bullet", func(c string) {
			p := filepath.Join(c, filepath.FromSlash(mcpqual.CatalogMDPath))
			b, _ := os.ReadFile(p)
			head := strings.Index(string(b), "\n## OpenAI Codex\n")
			bullet := strings.Index(string(b)[head:], "- **MCP call timeout: VERIFIED.** Measured by qualification run "+runs["codex"]) + head
			edited := string(b)[:bullet] + strings.Replace(string(b)[bullet:], "up to 15000 ms", "up to 30000 ms", 1)
			if edited == string(b) || head < 0 {
				t.Fatal("no bullet edited")
			}
			os.WriteFile(p, []byte(edited), 0o644)
		}, nil)
		// A freshly recomputed, valid receipt over the unchanged package
		// cannot authorize an edited fact.
		onCopy("edited fact with a recomputed receipt", func(c string) {
			writeSyntheticReceipt(t, runDir(c, "claude"), func(r *publicationReceipt) {
				r.ReviewedAt, r.ApprovedAt = "2026-10-12T09:00:00Z", "2026-10-12T10:00:00Z"
			})
			if err := validatePublicationReceipt(runDir(c, "claude")); err != nil {
				t.Fatalf("the recomputed receipt itself is invalid: %v", err)
			}
		}, edited(func(es []catalog.Entry) {
			fact(es, "claude", "mcp_timeout", func(f *catalog.Fact) { f.Value += " Hand edited." })
		}))
		// Guide rows: a stale managed status, another target, a missing row
		// and a referenced runbook fail the shared row checker.
		ref := " | SEE CATALOG | [Current status and evidence](../tests/testdata/support-catalog.json) |"
		for name, edit := range map[string]func(string) string{
			"stale managed status": func(s string) string {
				return strings.Replace(s, "| `mcp_timeout`"+ref, "| `mcp_timeout` | VERIFIED | [help](../tests/testdata/cli-help/claude-help.txt) |", 1)
			},
			"wrong target": func(s string) string {
				return strings.Replace(s, "| `mcp_config`"+ref, "| `mcp_config` | SEE CATALOG | [Current status and evidence](../tests/testdata/cli-help/claude-help.txt) |", 1)
			},
			"missing row": func(s string) string { return strings.Replace(s, "| `mcp_progress_extension`"+ref+"\n", "", 1) },
			"referenced runbook": func(s string) string {
				return strings.Replace(s, "| `runbook` | UNVERIFIED | [help](../tests/testdata/cli-help/claude-help.txt) |", "| `runbook`"+ref, 1)
			},
		} {
			g := edit(guide)
			if g == guide {
				t.Fatalf("%s: no guide edit", name)
			}
			if err := checkGuideRows(t, genRepo, g, genEntries); err == nil {
				t.Errorf("guide %s: accepted", name)
			}
		}
		// The shared row checker's rejection matrix (moved from the
		// stress-selected TestSetupGuide, review C1): for every vendor
		// section of the checked-in guide, a mirrored managed status,
		// another target, a missing row, a referenced or foreign runbook
		// and another vendor's entry are refused.
		t.Run("guide-row-matrix", func(t *testing.T) { guideRowMatrix(t, guide, realEntries) })
	})
}

// guideRowMatrix runs the setup guide row checker's rejection vectors over
// each vendor section of guide against entries (the checked-in catalog).
// Each unmodified section is accepted first, so every rejection is the
// broken row's.
func guideRowMatrix(t *testing.T, guide string, entries []catalog.Entry) {
	secs, err := mcpqual.ParseSetupDoc(guide)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalog.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		s, e := secs[id], byID[id]
		if err := mcpqual.CheckSetupFactRows(s, e); err != nil {
			t.Fatalf("%s: the unmodified section is refused: %v", id, err)
		}
		copyRows := func() map[string]mcpqual.SetupFact {
			out := map[string]mcpqual.SetupFact{}
			for k, v := range s.Facts {
				out[k] = v
			}
			return out
		}
		row := func(key string, f mcpqual.SetupFact) mcpqual.SetupSection {
			c := s
			c.Facts = copyRows()
			c.Facts[key] = f
			return c
		}
		without := copyRows()
		delete(without, "mcp_timeout")
		other := byID[map[string]string{"claude": "codex", "codex": "grok", "grok": "cursor", "cursor": "claude"}[id]]
		for name, bad := range map[string]struct {
			s mcpqual.SetupSection
			e catalog.Entry
		}{
			"stale status":       {row("mcp_timeout", mcpqual.SetupFact{Status: e.Facts["mcp_timeout"].Status, Evidence: []string{mcpqual.CatalogJSONPath}}), e},
			"wrong target":       {row("mcp_config", mcpqual.SetupFact{Status: mcpqual.CatalogReferenceStatus, Evidence: []string{"tests/testdata/cli-help/claude-help.txt"}}), e},
			"missing row":        {mcpqual.SetupSection{Client: id, Facts: without}, e},
			"referenced runbook": {row("runbook", mcpqual.SetupFact{Status: mcpqual.CatalogReferenceStatus, Evidence: []string{mcpqual.CatalogJSONPath}}), e},
			"foreign runbook":    {row("runbook", mcpqual.SetupFact{Status: e.Facts["runbook"].Status, Evidence: []string{mcpqual.CatalogJSONPath}}), e},
			"other vendor":       {s, other},
		} {
			if err := mcpqual.CheckSetupFactRows(bad.s, bad.e); err == nil {
				t.Errorf("%s: %s accepted", id, name)
			} else {
				t.Logf("%s %s: rejected: %v", id, name, err)
			}
		}
	}
}

// FP-3 (TestMCPRealEnrollmentConfirmation/publication-claims): an eligible
// short report publishes exactly the VERIFIED 15000 ms lower bound with
// its wording, both other timeout facts and mcp_config stay UNVERIFIED,
// the B=10s inequality and its decision hold, and missing evidence, the
// wrong platform or capability and unqualified full phases never become a
// vendor pass.
func publicationClaims(t *testing.T, rep *mcpqual.Report, out string) {
	if rep == nil {
		t.Fatal("no eligible codex report")
	}
	const head = "Short-poll decision (B=10s, B + max(2s, 0.1L) < L): "
	compatible := head + "compatible for this measured tuple: L = 15s is the longest silent call that completed (a lower bound, not the timeout): 10s + max(2s, 1.5s) = 12s < 15s."
	if mcpqual.ShortPollBudget != 10*time.Second || mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, reportClient(t, rep, "codex")) != compatible {
		t.Fatalf("decision %q", mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, reportClient(t, rep, "codex")))
	}
	for l, want := range map[time.Duration]bool{15 * time.Second: true, 12 * time.Second: false, 11 * time.Second: false, 12001 * time.Millisecond: true, 30 * time.Second: true} {
		if mcpqual.ShortPollCompatible(10*time.Second, l) != want {
			t.Fatalf("B=10s, L=%v: want %v", l, want)
		}
	}
	// The eligible run against a catalog whose worker and coordinator
	// identities differ.
	repo, base := entryRepo(t, "codex", mcpqual.CodexRealVersion, "linux/amd64")
	prior := *entryNamed(t, base.Entries, "codex")
	if prior.Version != "codex-cli 0.159.0" {
		t.Fatalf("codex worker version %q", prior.Version)
	}
	cp := freshOut(t, out)
	p, err := mcpqual.ProposePatch(rep, base, cp)
	if err != nil {
		t.Fatal(err)
	}
	report := mcpqual.EvidenceRoot + "/" + rep.RunID + "/report.json"
	prov := fmt.Sprintf("qualification run %s on linux/amd64 with %s and the plan's default configuration", rep.RunID, mcpqual.CodexRealVersion)
	unrequested := catalog.Fact{Status: catalog.Unverified, Value: "Established subset from " + prov +
		": the phase was not requested. Missing: the phase was not requested; iteration 07b local timeout qualification must establish it.", VerificationIteration: "07b"}
	want := map[string]catalog.Fact{
		"mcp_timeout": {Status: catalog.Verified, Value: "Measured by " + prov + ": silent tool calls of up to 15000 ms completed (2 observations); this is a lower bound, not the default.",
			VerificationIteration: "07b"},
		"mcp_timeout_override":   unrequested,
		"mcp_progress_extension": unrequested,
	}
	got := map[string]catalog.Fact{}
	for _, fc := range p.Facts {
		if fc.Client != "codex" {
			t.Fatalf("patch touches %s.%s", fc.Client, fc.Key)
		}
		if string(mustMarshal(t, fc.Prior)) != string(mustMarshal(t, prior.Facts[fc.Key])) {
			t.Fatalf("%s prior differs from the base", fc.Key)
		}
		got[fc.Key] = fc.Final
	}
	if len(got) != 4 {
		t.Fatalf("patch keys %v", got)
	}
	for key, w := range want {
		w.Evidence = append(slices.Clone(prior.Facts[key].Evidence), report)
		if string(mustMarshal(t, got[key])) != string(mustMarshal(t, w)) {
			t.Fatalf("%s = %+v, want %+v", key, got[key], w)
		}
	}
	cfg := got["mcp_config"]
	if cfg.Status != catalog.Unverified || cfg.VerificationIteration != "08" || !strings.HasPrefix(cfg.Value, prior.Facts["mcp_config"].Value+" ") ||
		!strings.HasSuffix(cfg.Value, `Qualification run `+rep.RunID+` observed initialize clientInfo name "codex-cli" and version "1.0.0" (requested_by compatible: true); registration through the vendor command is not qualified by it.`) ||
		!slices.Equal(cfg.Evidence, append(slices.Clone(prior.Facts["mcp_config"].Evidence), report)) {
		t.Fatalf("mcp_config %+v", cfg)
	}
	// Markdown: exactly the three bullets, no clientInfo text.
	md := string(repoFileAt(t, filepath.Join(cp, "proposed"), "support-catalog.md"))
	link := " See the [qualification report](../" + report + ").\n"
	for _, b := range []string{"- **MCP call timeout: VERIFIED.** " + want["mcp_timeout"].Value + link, "- **How to raise it: UNVERIFIED.** " + unrequested.Value + link,
		"- **Progress extends calls: UNVERIFIED.** " + unrequested.Value + link} {
		if strings.Count(md, b) != 1 {
			t.Fatalf("the Markdown lacks %q", b)
		}
	}
	if strings.Contains(md, `clientInfo name "codex-cli"`) {
		t.Fatal("the Markdown repeats the JSON-only clientInfo observation")
	}
	if err := mcpqual.Publish(cp, repo); err != nil {
		t.Fatal(err)
	}
	after := repoFacts(t, repo)
	if after["codex"].Version != prior.Version || after["codex"].CoordinatorVersion != mcpqual.CodexRealVersion {
		t.Fatal("publication changed an identity")
	}
	for key, f := range prior.Facts {
		if _, published := got[key]; !published && string(mustMarshal(t, after["codex"].Facts[key])) != string(mustMarshal(t, f)) {
			t.Fatalf("worker fact codex.%s changed", key)
		}
	}
	// Never a vendor pass without its evidence.
	mutated := func(t *testing.T, fn func(*mcpqual.Report, *mcpqual.ClientReport)) *mcpqual.Report {
		t.Helper()
		r := readReport(t, out)
		for i := range r.Clients {
			if r.Clients[i].ID == "codex" {
				fn(r, &r.Clients[i])
			}
		}
		return r
	}
	phase := func(c *mcpqual.ClientReport, name string) *mcpqual.PhaseReport {
		for i := range c.Phases {
			if c.Phases[i].Name == name {
				return &c.Phases[i]
			}
		}
		t.Fatalf("no %s phase", name)
		return nil
	}
	for name, fn := range map[string]func(*mcpqual.Report, *mcpqual.ClientReport){
		"missing-repeat": func(_ *mcpqual.Report, c *mcpqual.ClientReport) {
			ph := phase(c, mcpqual.PhaseDefault)
			ph.Observations, ph.Cases = 1, ph.Cases[:1]
		},
		"setup": func(_ *mcpqual.Report, c *mcpqual.ClientReport) {
			ph := phase(c, mcpqual.PhaseSetup)
			ph.Status, ph.Reason = mcpqual.StatusInconclusive, pubPtr("setup_failed")
		},
		"cleanup":     func(r *mcpqual.Report, _ *mcpqual.ClientReport) { r.Cleanup.OK = false },
		"interrupted": func(r *mcpqual.Report, _ *mcpqual.ClientReport) { r.Interrupted = true },
		"platform":    func(r *mcpqual.Report, _ *mcpqual.ClientReport) { r.OS, r.Arch = "darwin", "arm64" },
		"synthetic":   func(_ *mcpqual.Report, c *mcpqual.ClientReport) { c.DecoderVersion.Qualified = false },
		"historical":  func(_ *mcpqual.Report, c *mcpqual.ClientReport) { c.DecoderVersion.Evidence = nil },
	} {
		r := mutated(t, fn)
		if d := mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, r, reportClient(t, r, "codex")); !strings.Contains(d, "UNVERIFIED") || strings.HasPrefix(d, head+"compatible") {
			t.Fatalf("%s: decision %q", name, d)
		}
	}
	if _, err := mcpqual.ProposePatch(mutated(t, func(r *mcpqual.Report, _ *mcpqual.ClientReport) { r.Cleanup.OK = false }), base, freshOut(t, out)); contract.ExitCode(err) != 5 {
		t.Fatalf("a cleanup failure proposed: %v", err)
	}
	finalFacts := func(t *testing.T, r *mcpqual.Report, b *mcpqual.CatalogBase) map[string]catalog.Fact {
		t.Helper()
		p, err := mcpqual.ProposePatch(r, b, freshOut(t, out))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]catalog.Fact{}
		for _, fc := range p.Facts {
			m[fc.Key] = fc.Final
		}
		return m
	}
	_, darwin := entryRepo(t, "codex", mcpqual.CodexRealVersion, "darwin/arm64")
	for name, c := range map[string]struct {
		rep  *mcpqual.Report
		base *mcpqual.CatalogBase
		want string
	}{
		"unsupported-platform": {readReport(t, out), darwin, "does not certify darwin/arm64"},
		"synthetic-capability": {mutated(t, func(_ *mcpqual.Report, c *mcpqual.ClientReport) { c.DecoderVersion.Qualified = false }), base, "synthetic fixture"},
		"historical-capability": {mutated(t, func(_ *mcpqual.Report, c *mcpqual.ClientReport) { c.DecoderVersion.Evidence = nil }), base,
			"without enrolled real-transcript evidence"},
	} {
		if f := finalFacts(t, c.rep, c.base)["mcp_timeout"]; f.Status != catalog.Unverified || !strings.Contains(f.Value, c.want) {
			t.Fatalf("%s: %+v", name, f)
		}
	}
	// Full-plan phases keep their existing statuses: an inconclusive
	// override and an extension without an observed cap stay UNVERIFIED.
	full := finalFacts(t, mutated(t, func(_ *mcpqual.Report, c *mcpqual.ClientReport) {
		c.Phases = append(c.Phases, mcpqual.PhaseReport{Name: mcpqual.PhaseOverride, Status: mcpqual.StatusInconclusive, Reason: pubPtr("budget")},
			mcpqual.PhaseReport{Name: mcpqual.PhaseProgress, Status: mcpqual.StatusConclusive, Result: pubPtr(mcpqual.ResultExtends), LowerBoundMS: pubPtr(int64(20000)), Observations: 1})
	}), base)
	if full["mcp_timeout"].Status != catalog.Verified || full["mcp_timeout_override"].Status != catalog.Unverified ||
		!strings.Contains(full["mcp_timeout_override"].Value, "the override phase was inconclusive (budget)") ||
		full["mcp_progress_extension"].Status != catalog.Unverified || !strings.Contains(full["mcp_progress_extension"].Value, "Missing: an observed absolute cap") {
		t.Fatalf("full phases %+v", full)
	}
}

// FP-5 (TestMCPCaptureRunbook/publication-review): the guide's catalog
// publication subsection orders independent verification before owner
// acceptance of an immutable package, forbids raw bundles and placeholder
// attestations; the shared receipt validator accepts a complete synthetic
// package and rejects every invalid receipt or inventory.
func publicationReview(t *testing.T, guide string) {
	sec := catalogPublicationSection(t, guide)
	order := []string{"1. Private handoff", "2. Independent verification", "Only discrepancy-free verification lets it record the Reviewer `decoder-enrollment-coordinator`",
		"3. Owner acceptance", "only the owner's explicit acceptance of that independently verified, immutable package permits the Owner `callsheet-owner`", "4. Commit"}
	at := -1
	for _, w := range order {
		i := strings.Index(sec, w)
		if i <= at {
			t.Fatalf("%q is missing or out of order", w)
		}
		at = i
	}
	for _, w := range []string{"Automatic installation into an uncommitted owner checkout is not acceptance to commit", "permission for local installation only",
		"Any change to the candidate payload invalidates the review and the approval", "never changed to insert attestations", "a raw bundle is never committed",
		"Empty handles, placeholder dates and false flags are never valid final receipts", "a private draft with empty handles never enters the repository",
		"`" + publicationReceiptName + "`", "`independently_verified`, `privacy_approved` and `owner_accepted` all true", "Decoder output alone is not an independent oracle",
		"sanitized does not mean approved for a public commit", "never hand-edit evidence, recompute hashes to hide a change"} {
		if !strings.Contains(sec, w) {
			t.Fatalf("the catalog publication subsection lacks %q", w)
		}
	}
	pkg := installedPackage(t)
	fresh := func(t *testing.T) string {
		t.Helper()
		d := filepath.Join(realTemp(t), filepath.Base(pkg))
		copyTree(t, pkg, d)
		return d
	}
	valid := fresh(t)
	writeSyntheticReceipt(t, valid, nil)
	if err := validatePublicationReceipt(valid); err != nil {
		t.Fatalf("a complete synthetic package: %v", err)
	}
	set := func(fn func(*publicationReceipt)) func(*testing.T, string) {
		return func(t *testing.T, d string) { writeSyntheticReceipt(t, d, fn) }
	}
	after := func(fn func(*testing.T, string)) func(*testing.T, string) {
		return func(t *testing.T, d string) { writeSyntheticReceipt(t, d, nil); fn(t, d) }
	}
	first := func(r *publicationReceipt) *receiptFile { return &r.Files[0] }
	for name, c := range map[string]struct {
		prepare func(*testing.T, string)
		want    string
	}{
		"missing-receipt":       {func(*testing.T, string) {}, "missing receipt"},
		"empty-reviewer":        {set(func(r *publicationReceipt) { r.Reviewer = "" }), "reviewer"},
		"placeholder-reviewer":  {set(func(r *publicationReceipt) { r.Reviewer = "<reviewer>" }), "reviewer"},
		"empty-owner":           {set(func(r *publicationReceipt) { r.Owner = "" }), "owner"},
		"placeholder-owner":     {set(func(r *publicationReceipt) { r.Owner = "TBD" }), "owner"},
		"not-verified":          {set(func(r *publicationReceipt) { r.IndependentlyVerified = false }), "false flag"},
		"privacy-not-approved":  {set(func(r *publicationReceipt) { r.PrivacyApproved = false }), "false flag"},
		"owner-not-accepted":    {set(func(r *publicationReceipt) { r.OwnerAccepted = false }), "false flag"},
		"empty-reviewed-at":     {set(func(r *publicationReceipt) { r.ReviewedAt = "" }), "reviewed_at is empty"},
		"date-only":             {set(func(r *publicationReceipt) { r.ReviewedAt = "2026-10-11" }), "not RFC3339"},
		"placeholder-text-date": {set(func(r *publicationReceipt) { r.ApprovedAt = "YYYY-MM-DDTHH:MM:SSZ" }), "not RFC3339"},
		"zero-date":             {set(func(r *publicationReceipt) { r.ReviewedAt = "0001-01-01T00:00:00Z" }), "placeholder date"},
		"epoch-date":            {set(func(r *publicationReceipt) { r.ApprovedAt = "1970-01-01T00:00:00Z" }), "placeholder date"},
		"not-utc":               {set(func(r *publicationReceipt) { r.ApprovedAt = "2026-10-11T12:00:00+02:00" }), "not UTC"},
		"approved-before":       {set(func(r *publicationReceipt) { r.ApprovedAt = "2026-10-11T08:59:59Z" }), "before reviewed_at"},
		"schema":                {set(func(r *publicationReceipt) { r.Schema = 2 }), "schema"},
		"run-id":                {set(func(r *publicationReceipt) { r.RunID = "other-run" }), "run_id"},
		"duplicate-path":        {set(func(r *publicationReceipt) { r.Files = append(r.Files[:1], r.Files...) }), "not sorted or repeats"},
		"unsorted":              {set(func(r *publicationReceipt) { r.Files[0], r.Files[1] = r.Files[1], r.Files[0] }), "not sorted or repeats"},
		"escaping-path":         {set(func(r *publicationReceipt) { first(r).Path = "../escape.json" }), "not a clean run-relative path"},
		"absolute-path":         {set(func(r *publicationReceipt) { first(r).Path = "/etc/passwd" }), "not a clean run-relative path"},
		"unclean-path":          {set(func(r *publicationReceipt) { first(r).Path = "./" + first(r).Path }), "not a clean run-relative path"},
		"backslash-path":        {set(func(r *publicationReceipt) { first(r).Path = `cases\x.json` }), "not a clean run-relative path"},
		"self-reference": {set(func(r *publicationReceipt) {
			r.Files = append(r.Files, receiptFile{Path: publicationReceiptName, Bytes: 1, SHA256: strings.Repeat("0", 64)})
		}), "lists the receipt itself"},
		"hash-mismatch":  {set(func(r *publicationReceipt) { first(r).SHA256 = strings.Repeat("0", 64) }), "differs from its size or hash"},
		"uppercase-hash": {set(func(r *publicationReceipt) { first(r).SHA256 = strings.ToUpper(first(r).SHA256) }), "invalid hash"},
		"size-mismatch":  {set(func(r *publicationReceipt) { first(r).Bytes++ }), "differs from its size or hash"},
		"absent-file": {set(func(r *publicationReceipt) {
			r.Files = append(r.Files, receiptFile{Path: "zz-absent.txt", SHA256: sha(nil)})
		}), "is absent"},
		"unlisted-file": {after(func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "notes.txt"), []byte("unreviewed\n"), 0o644)
		}), "unlisted file notes.txt"},
		"unlisted-dropped": {set(func(r *publicationReceipt) { r.Files = r.Files[1:] }), "unlisted file"},
		"linked-file": {after(func(t *testing.T, d string) {
			os.Remove(filepath.Join(d, "report.md"))
			os.Symlink(filepath.Join(pkg, "report.md"), filepath.Join(d, "report.md"))
		}), "not a bounded regular file"},
		"modified-payload": {after(func(t *testing.T, d string) {
			b := repoFileAt(t, d, "report.md")
			os.WriteFile(filepath.Join(d, "report.md"), append(b, []byte("approved later\n")...), 0o644)
		}), "differs from its size or hash"},
		"unknown-member": {after(func(t *testing.T, d string) {
			b := repoFileAt(t, d, publicationReceiptName)
			os.WriteFile(filepath.Join(d, publicationReceiptName), bytes.Replace(b, []byte(`"schema": 1,`), []byte(`"schema": 1, "signature": "x",`), 1), 0o644)
		}), "unknown field"},
		"extra-outside-report": {after(func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "extra.json"), []byte("{}\n"), 0o644)
			writeSyntheticReceipt(t, d, nil)
		}), "outside the report and manifest"},
	} {
		d := fresh(t)
		c.prepare(t, d)
		if err := validatePublicationReceipt(d); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}

// FP-6 (TestMCPCursorConfirmation/publication-runbook): the runbook pins
// the four plans, versions, models, executable hashes and enrolled
// fixtures, documents exactly the shipped invocation shapes and four
// individual commands (which reach the templates' owner placeholders
// before any launch), scoped approvals, no concurrent sessions, and the
// A3.1 foreign, own-path and cleanup rules.
func publicationRunbook(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	guide := string(repoFile(t, mcpqual.SetupDocPath))
	sec := catalogPublicationSection(t, guide)
	if s, c := strings.Index(guide, "\n### Short confirmation\n"), strings.Index(guide, "\n### Catalog publication\n"); s < 0 || c < s ||
		strings.Contains(guide[s+1:c], "\n## ") || strings.Count(guide[s+1:c], "\n### ") != 0 {
		t.Fatal("the catalog publication subsection does not immediately follow the short confirmation")
	}
	entries, err := catalog.Load(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := mcpqual.ParseEnrollmentIndex(repoFile(t, mcpqual.EnrollmentIndexPath))
	if err != nil {
		t.Fatal(err)
	}
	pins := []struct{ id, model, hash, decoder string }{
		{"claude", "claude-sonnet-5-5", "a967e7b1d8b4e47ee421d5433027880347952b0c0857abf880e2c942a4ec93b3", "claude-json"},
		{"codex", "gpt-6.1-sol", "12eb3e81114588aca3b7998f4f19e8997b056aca08e57a7ca7c8a3ec8c652aad", "codex-jsonl"},
		{"grok", "grok-4.7", "41626a53292324140b92556b9d42ff5542e3dcd04aff85eafb8689dd4adb44fc", "grok-json"},
		{"cursor", "grok-4.7-low", "2ccc9a8e167797641448b5e5c936f006ba137a2555f117f38c5eb76a5238a233", "cursor-jsonl"},
	}
	plans := filepath.Join(root, "internal", "mcpqual", "testdata", "plans")
	for _, p := range pins {
		version := entryNamed(t, entries, p.id).CoordinatorVersion
		key := sha([]byte(version))
		fixture := mcpqual.EnrolledFixtureID(p.decoder, version, "linux/amd64")
		row := "| " + p.id + " | `" + version + "` | `" + p.model + "` | `" + p.hash + "` | `" + key + "` | `" + fixture + "` |"
		if !strings.Contains(sec, row) || mcpqual.VersionKey(version) != key {
			t.Fatalf("%s pin row %q", p.id, row)
		}
		// The executable hash is the shipped enrollment runbook's.
		if !strings.Contains(guide[:strings.Index(guide, "\n### Catalog publication\n")], "`"+version+"` | `"+p.hash+"` |") {
			t.Fatalf("%s hash is not the shipped runbook's pin", p.id)
		}
		if !slices.ContainsFunc(idx.Entries, func(e mcpqual.EnrollmentEntry) bool {
			return e.Client == p.id && e.Version == version && e.Fixture == fixture && strings.Contains(sec, "](../"+e.Bundle+")")
		}) {
			t.Fatalf("%s fixture %s is not the enrolled, linked bundle", p.id, fixture)
		}
		// The shipped template's invocation shape, budget and phases.
		var plan struct {
			Limits struct {
				Sessions int `json:"max_sessions_per_client"`
				Case     int `json:"max_case_ms"`
				Client   int `json:"max_client_ms"`
			} `json:"limits"`
			Clients []struct {
				Session struct {
					Argv []string `json:"argv"`
				} `json:"session"`
				Phases map[string]struct {
					Delays []int `json:"delays_ms"`
				} `json:"phases"`
			} `json:"clients"`
		}
		if err := json.Unmarshal(repoFileAt(t, plans, p.id+"-short.json"), &plan); err != nil || len(plan.Clients) != 1 {
			t.Fatalf("%s template %v", p.id, err)
		}
		shape := "`" + strings.Join(plan.Clients[0].Session.Argv, " ") + "`"
		if !strings.Contains(sec, shape) || plan.Limits.Sessions != 3 || plan.Limits.Case != 120000 || plan.Limits.Client != 360000 ||
			len(plan.Clients[0].Phases) != 1 || !slices.Equal(plan.Clients[0].Phases["default"].Delays, []int{15000}) {
			t.Fatalf("%s shape %s or template budget", p.id, shape)
		}
	}
	// The four individual commands, in order, through the real mcpqual: the
	// shipped templates stop at their owner placeholders before anything
	// is launched or written.
	m := regexp.MustCompile("(?s)```sh\n(.*?)\n```").FindStringSubmatch(sec)
	if m == nil {
		t.Fatal("no command block")
	}
	lines := strings.Split(m[1], "\n")
	if len(lines) != 4 {
		t.Fatalf("%d commands", len(lines))
	}
	q := newQualEnv(t)
	repo := qualRepo(t)
	j0, m0 := readCatalogFiles(t, repo)
	for i, p := range pins {
		words, err := mcpqual.ShellWords(lines[i])
		want := []string{"$MCPQUAL", "qualify", "--plan", "$PLAN_DIR/" + p.id + "-short.json", "--out", "$RUN_DIR/" + p.id, "--allow-model-calls", "--publish-catalog", "$REPO"}
		if err != nil || !slices.Equal(words, want) {
			t.Fatalf("command %d %q", i, words)
		}
		args := slices.Clone(words[1:])
		args[2], args[4], args[7] = filepath.Join(plans, p.id+"-short.json"), filepath.Join(q.dir, "out-"+p.id), repo
		if r := runBin(t, qualBinary(t), q.dir, q.env(""), args...); r.code != 2 || !strings.Contains(r.stderr, "unfilled owner placeholder") {
			t.Fatalf("%s documented qualify = %+v", p.id, r)
		}
		if _, err := os.Stat(filepath.Join(q.dir, "out-"+p.id)); err == nil {
			t.Fatalf("%s: an output directory was created", p.id)
		}
	}
	if _, err := os.Stat(q.launches); err == nil {
		t.Fatal("a documented command launched a vendor")
	}
	if j1, m1 := readCatalogFiles(t, repo); j1 != j0 || m1 != m0 {
		t.Fatal("a documented command touched the catalog")
	}
	for _, w := range []string{
		// Pins, binary and plans.
		"build one `mcpqual` binary from the reviewed schema and publisher revision, record its revision and full SHA-256",
		"change only the executable, `expected_version` (the exact `coordinator_version` above), both `<model>` placeholders and `decoder_fixture`",
		"keep the delay, limits, session and configuration recipes and default timeout settings intact",
		"compare its version and full executable hash with the pins immediately before each run: a mismatch stops, and needs renewed evidence, never an edited pin",
		"each individually, and inspect each result before the next; never a loop that retries a failure automatically",
		"never run these vendor shapes by hand instead of qualification",
		// Scoped approvals.
		"no `--approve-mcps`, `--force`, `--yolo`, approval-policy widening, timeout override or replacement HOME or authentication store",
		"Claude allows only `mcp__probe__slow`", "Codex pre-approves only `probe.slow`", "`" + mcpqual.CursorToolPermissionContent + "`",
		"one `mcp enable probe` per case", "Grok uses the normal login and workspace-scoped trust", "with `CI`, `GIT_DIR` and `GIT_WORK_TREE` absent",
		// No concurrent sessions.
		"stop every other Claude Code session for the Claude run", "Stop all Cursor sessions, Cursor-backed flow roles and their workers throughout the Cursor run",
		"never connect to or unlink a socket as a liveness test",
		// A3.1 foreign and own-path rules.
		"at most 10,000 entries, 8 MiB per file and 64 MiB in all", "the effective HOME's `chats/` and `ai-tracking/` after their boundary checks",
		"metadata-only baseline of foreign session artifacts", "changed, new, removed or unverifiable foreign state fails closed",
		"Each case's own full-slug project directory and its possible hashed-candidate directory must be absent before its enable, even when the foreign baseline holds them",
		"the parent-slug cap (P at most 54 characters) applies only to an observed hashed candidate, not to every output path", "Do not guess the vendor's path-length switch",
		// Cleanup and cost.
		"removes only exact known prior run directories, with the workers stopped", "no wildcard deletion, no projects-root removal",
		"no requirement to clean unrelated projects, and no automated cleanup or retry", "Every case (setup, the silent 15s call and its repeat) gets its own scoped approval and residue validation",
		"At most three model sessions per client, 120 s per case and 360 s per client"} {
		if !strings.Contains(sec, w) {
			t.Fatalf("the catalog publication subsection lacks %q", w)
		}
	}
	q.groupsGone()
}

// FP-8 (TestMCPEnrollmentCIPolicy/catalog-version-policy): the 429 native
// parents in their prior order, the unchanged stress plan and workflow,
// the pinned enrollment index and the production enrollment validation of
// the fixture bytes it references.
func catalogVersionPolicy(t *testing.T) {
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	if req := devcheck.NativeRequiredTests(); len(req) != 429 || sum(strings.Join(req, "\n")) != nativeInventorySHA256 {
		t.Fatalf("native inventory %d %s", len(req), sum(strings.Join(req, "\n")))
	}
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := devcheck.StressSteps(goos)
		var argv []string
		for _, s := range steps {
			argv = append(argv, strings.Join(s.Argv, " "))
		}
		if err != nil || len(steps) != 22 || sum(strings.Join(argv, "\n")) != stressArgvSHA256 {
			t.Fatalf("%s stress plan %d %s %v", goos, len(steps), sum(strings.Join(argv, "\n")), err)
		}
	}
	if got := sha(repoFile(t, ".github/workflows/ci.yml")); got != ciWorkflowSHA256 {
		t.Fatalf("ci.yml %s", got)
	}
	if got := sha(repoFile(t, mcpqual.EnrollmentIndexPath)); got != enrollmentIndexSHA256 {
		t.Fatalf("enrollment index %s, pinned %s", got, enrollmentIndexSHA256)
	}
	root := testkit.MustRepoRoot(t)
	idx, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{Root: root, Registry: mcpqual.DefaultRegistry()})
	if err != nil || len(idx.Entries) != 4 {
		t.Fatalf("enrollment validation %v", err)
	}
	// Amendment A1, on the real checkout itself (no state shared with
	// another parent): the baseline freezes, the published-catalog
	// validation and the guide's shared row validation all hold, so the
	// evidence pull request needs no re-freeze and no guide rewrite.
	entries, err := catalog.Load(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPublicationBaseline(entries); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedCatalog(t, root, entries); err != nil {
		t.Fatal(err)
	}
	if err := checkGuideRows(t, root, string(repoFile(t, mcpqual.SetupDocPath)), entries); err != nil {
		t.Fatal(err)
	}
}

// FP-9 (TestMCPEnrollmentCIPolicy/catalog-version-ci): both hosts run the
// new offline tests through the existing stages, every changed production
// file is a whole-file changed-group entry under the strict greater-than-80%
// gate, no stage is live, and no catalog-version test depends on a design
// path.
func catalogVersionCI(t *testing.T, live func([]devcheck.Step) string) {
	wf := string(repoFile(t, ".github/workflows/ci.yml"))
	for _, w := range []string{"name: ci-linux\n    runs-on: ubuntu-24.04", "name: ci-macos\n    runs-on: macos-15", "go run ./cmd/devcheck test", "go run ./cmd/devcheck coverage",
		"go run ./cmd/devcheck bench", "go run ./cmd/devcheck cross", "go run ./cmd/devcheck native"} {
		if !strings.Contains(wf, w) {
			t.Fatalf("ci.yml lacks %q", w)
		}
	}
	for _, goos := range []string{"linux", "darwin"} {
		steps := devcheck.TestSteps(goos)
		if !slices.Contains(steps[0].Argv, "./...") || live(steps) != "" {
			t.Fatalf("%s test steps %+v", goos, steps)
		}
	}
	native, err := devcheck.NativeSteps("darwin")
	if err != nil || len(native) != 1 || !slices.Contains(native[0].Argv, "./...") || live(native) != "" {
		t.Fatalf("native steps %+v %v", native, err)
	}
	cover, _, _ := devcheck.CoverageSteps("profile.out")
	if !slices.Contains(cover.Argv, "-coverpkg=./internal/...,./cmd/...") || !slices.Contains(cover.Argv, "./internal/...") {
		t.Fatalf("coverage step %q", cover.Argv)
	}
	whole := map[string]bool{}
	for _, e := range devcheck.WorkspaceCoverageManifest {
		if e.Group == devcheck.GroupChanged && len(e.Ranges) == 0 && e.OS == "" {
			whole[strings.TrimPrefix(e.File, "github.com/wedevwork/callsheet/")] = true
		}
	}
	for _, f := range []string{"internal/testkit/catalog/catalog.go", "internal/mcpqual/publish.go", "internal/testkit/catalog/publication_baseline.go", "internal/mcpqual/setupdoc.go"} {
		if !whole[f] {
			t.Fatalf("%s is not a whole-file changed entry on both systems", f)
		}
	}
	if devcheck.CoverageThreshold != 80 {
		t.Fatalf("coverage threshold %v", devcheck.CoverageThreshold)
	}
	// No catalog-version test reads a design path.
	design := "design" + "/"
	for _, rel := range []string{"tests/function/mcpqual_publication_test.go", "internal/testkit/catalog/catalog_test.go"} {
		if strings.Contains(string(repoFile(t, rel)), design) {
			t.Fatalf("%s names a design path", rel)
		}
	}
	if b := string(repoFile(t, "internal/testkit/catalog/real_adapters_test.go")); strings.Count(b, design) != 1 ||
		!strings.Contains(b, "wave2SourceRoot            = \""+design+"iterations/11-real-adapters/design-check\"") {
		t.Fatal("real_adapters_test.go names a design path beyond the manifest's recorded source root")
	}
	doc := strings.Join(strings.Fields(string(repoFile(t, "docs/ci.md"))), " ")
	for _, w := range []string{"Catalog version (design catalog-version)", "the whole-file coverage manifest adds `internal/testkit/catalog/catalog.go`",
		"catalog-version adds no new stress selector or workload"} {
		if !strings.Contains(doc, w) {
			t.Fatalf("docs/ci.md lacks %q", w)
		}
	}
}
