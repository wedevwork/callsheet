package mcpqual

// Catalog publication (FP-14): measurement never edits the repository; an
// explicit --publish-catalog REPO records the catalog's base hashes at run
// start, then proposes a patch (catalog-patch.json with the intended final
// bytes under proposed/) and installs it. Installation refuses (conflict)
// when either catalog differs from its base, a target fact differs from its
// expected prior value or an evidence destination holds different bytes;
// a file already equal to this patch's intended bytes is accepted, so an
// interrupted install can be retried. Each file is written to a temporary
// name and renamed; there is no cross-file atomicity.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Repository locations.
const (
	CatalogJSONPath = "tests/testdata/support-catalog.json"
	CatalogMDPath   = "docs/support-catalog.md"
	EvidenceRoot    = "tests/testdata/mcp-qualification"
	PatchFileName   = "catalog-patch.json"
	proposedDir     = "proposed"
)

// Markdown anchors of the three timeout bullets in each vendor section.
var (
	bulletAnchors  = map[string]string{"mcp_timeout": "- **MCP call timeout:", "mcp_timeout_override": "- **How to raise it:", "mcp_progress_extension": "- **Progress extends calls:"}
	vendorSections = map[string]string{"claude": "## Claude Code", "codex": "## OpenAI Codex", "grok": "## Grok Build", "cursor": "## Cursor Agent"}
)

// CatalogBase is the repository catalog as read at run start.
type CatalogBase struct {
	Repo     string
	JSON     []byte
	Markdown []byte
	Entries  []catalog.Entry
}

func conflict(format string, a ...any) error {
	return contract.New(contract.CodeConflict, "publication refused: "+fmt.Sprintf(format, a...))
}

// ReadCatalogBase reads both catalog representations of repo. The JSON
// must be valid and in canonical form (so a minimal patch leaves every
// other byte unchanged).
func ReadCatalogBase(repo string) (*CatalogBase, error) {
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return nil, contract.New(contract.CodeInvalidArgument, fmt.Sprintf("--publish-catalog %q must be an absolute clean path", repo))
	}
	j, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(CatalogJSONPath)))
	if err != nil {
		return nil, contract.Wrap(contract.CodeInvalidArgument, "--publish-catalog: cannot read "+CatalogJSONPath, err)
	}
	md, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(CatalogMDPath)))
	if err != nil {
		return nil, contract.Wrap(contract.CodeInvalidArgument, "--publish-catalog: cannot read "+CatalogMDPath, err)
	}
	entries, err := catalog.Decode(j)
	if err != nil {
		return nil, contract.Wrap(contract.CodeInvalidArgument, "--publish-catalog: invalid catalog", err)
	}
	if r, err := RenderCatalog(entries); err != nil || !bytes.Equal(r, j) {
		return nil, contract.New(contract.CodeInvalidArgument, "--publish-catalog: "+CatalogJSONPath+" is not in canonical form")
	}
	return &CatalogBase{Repo: repo, JSON: j, Markdown: md, Entries: entries}, nil
}

// orderedFacts marshals facts in catalog.RequiredFacts order.
type orderedFacts map[string]catalog.Fact

func (f orderedFacts) MarshalJSON() ([]byte, error) {
	keys := append([]string(nil), catalog.RequiredFacts...)
	var extra []string
	for k := range f {
		if !contains(keys, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	var buf bytes.Buffer
	buf.WriteByte('{')
	n := 0
	for _, k := range append(keys, extra...) {
		v, ok := f[k]
		if !ok {
			continue
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		n++
		kb, _ := encodeJSON(k)
		vb, err := encodeJSON(v)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// RenderCatalog is the canonical catalog encoding: two-space indentation,
// no HTML escaping, facts in RequiredFacts order, a final newline.
func RenderCatalog(entries []catalog.Entry) ([]byte, error) {
	type ordered struct {
		ID       string       `json:"id"`
		Version  string       `json:"version"`
		Platform string       `json:"platform"`
		Facts    orderedFacts `json:"facts"`
	}
	out := make([]ordered, len(entries))
	for i, e := range entries {
		out[i] = ordered{e.ID, e.Version, e.Platform, orderedFacts(e.Facts)}
	}
	return encodeIndent(out)
}

// FactChange is one catalog fact replacement.
type FactChange struct {
	Client string       `json:"client"`
	Key    string       `json:"key"`
	Prior  catalog.Fact `json:"prior"`
	Final  catalog.Fact `json:"final"`
}

// PatchFile is one evidence file to install.
type PatchFile struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
	SHA256 string `json:"sha256"`
}

// Patch is the proposed catalog update.
type Patch struct {
	Schema              int          `json:"schema"`
	RunID               string       `json:"run_id"`
	BaseJSONSHA256      string       `json:"base_json_sha256"`
	BaseMarkdownSHA256  string       `json:"base_markdown_sha256"`
	FinalJSONSHA256     string       `json:"final_json_sha256"`
	FinalMarkdownSHA256 string       `json:"final_markdown_sha256"`
	Facts               []FactChange `json:"facts"`
	Files               []PatchFile  `json:"files"`
}

// ProposePatch builds the patch for rep from base and writes it (with the
// intended final bytes) into outDir. Only clients whose sessions ran are
// touched; worker facts never change.
func ProposePatch(rep *Report, base *CatalogBase, outDir string) (*Patch, error) {
	if !rep.Cleanup.OK || rep.Interrupted {
		return nil, contract.New(contract.CodeUnavailable, "publication refused: the run's cleanup failed or it was interrupted")
	}
	if err := rep.CheckEvidence(outDir); err != nil {
		return nil, conflict("%v", err)
	}
	evDir := path.Join(EvidenceRoot, rep.RunID)
	reportPath := path.Join(evDir, "report.json")
	// An old-policy catalog (the retired 07a interim exception) is refused
	// before anything is proposed: never a mixed old paragraph and new
	// facts. A short-poll catalog accepts properly evidenced VERIFIED
	// lower bounds and measured timeouts.
	if err := CheckShortPollCatalog(string(base.Markdown), base.Entries); err != nil {
		return nil, conflict("%v; update catalog policy first", err)
	}
	p := &Patch{Schema: 1, RunID: rep.RunID, BaseJSONSHA256: sha256Hex(base.JSON), BaseMarkdownSHA256: sha256Hex(base.Markdown), Facts: []FactChange{}}
	for _, name := range []string{"report.json", "report.md", "manifest.json"} {
		b, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			return nil, conflict("missing %s: %v", name, err)
		}
		p.Files = append(p.Files, PatchFile{Source: name, Dest: path.Join(evDir, name), SHA256: sha256Hex(b)})
	}
	for _, ev := range rep.Evidence {
		p.Files = append(p.Files, PatchFile{Source: ev.Path, Dest: path.Join(evDir, ev.Path), SHA256: ev.SHA256})
	}
	final := cloneEntries(base.Entries)
	md := string(base.Markdown)
	for _, c := range rep.Clients {
		ran := false
		for _, ph := range c.Phases {
			ran = ran || len(ph.Cases) > 0
		}
		if !ran {
			continue
		}
		idx := -1
		for i, e := range final {
			if e.ID == c.ID {
				idx = i
			}
		}
		if idx < 0 {
			return nil, conflict("the catalog has no %s entry", c.ID)
		}
		e := final[idx]
		if c.ObservedVersion == nil || *c.ObservedVersion != e.Version {
			return nil, conflict("%s: the run's observed version does not equal the catalog's %q", c.ID, e.Version)
		}
		facts := proposeFacts(rep, c, e, reportPath)
		for _, key := range append(append([]string(nil), TimeoutKeys...), "mcp_config") {
			f, ok := facts[key]
			if !ok {
				continue
			}
			p.Facts = append(p.Facts, FactChange{Client: c.ID, Key: key, Prior: e.Facts[key], Final: f})
			e.Facts[key] = f
		}
		var err error
		if md, err = patchMarkdown(md, c.ID, facts, reportPath); err != nil {
			return nil, err
		}
	}
	jb, err := RenderCatalog(final)
	if err != nil {
		return nil, err
	}
	if err := validateStaged(base, outDir, p, final); err != nil {
		return nil, conflict("the proposed catalog is invalid: %v", err)
	}
	if err := CheckShortPollCatalog(md, final); err != nil {
		return nil, conflict("the proposed catalog is invalid: %v", err)
	}
	p.FinalJSONSHA256, p.FinalMarkdownSHA256 = sha256Hex(jb), sha256Hex([]byte(md))
	pb, _ := encodeIndent(p)
	os.MkdirAll(filepath.Join(outDir, proposedDir), 0o755)
	for name, data := range map[string][]byte{filepath.Join(proposedDir, "support-catalog.json"): jb, filepath.Join(proposedDir, "support-catalog.md"): []byte(md), PatchFileName: pb} {
		if err := os.WriteFile(filepath.Join(outDir, name), data, 0o644); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func cloneEntries(es []catalog.Entry) []catalog.Entry {
	out := make([]catalog.Entry, len(es))
	for i, e := range es {
		out[i] = e
		out[i].Facts = map[string]catalog.Fact{}
		for k, f := range e.Facts {
			f.Evidence = append([]string(nil), f.Evidence...)
			out[i].Facts[k] = f
		}
	}
	return out
}

// validateStaged runs the catalog validator on the proposed entries with
// the new evidence staged (as symbolic links) beside the repository's
// existing evidence.
func validateStaged(base *CatalogBase, outDir string, p *Patch, entries []catalog.Entry) error {
	stage, err := os.MkdirTemp(outDir, ".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	staged := map[string]string{}
	for _, f := range p.Files {
		staged[f.Dest] = filepath.Join(outDir, filepath.FromSlash(f.Source))
	}
	linked := map[string]bool{}
	for _, e := range entries {
		for _, f := range e.Facts {
			for _, ev := range f.Evidence {
				if linked[ev] || catalog.CheckEvidencePath(ev) != nil {
					continue
				}
				linked[ev] = true
				src, ok := staged[ev]
				if !ok {
					src = filepath.Join(base.Repo, filepath.FromSlash(ev))
				}
				abs, err := filepath.Abs(src)
				if err != nil {
					continue
				}
				dst := filepath.Join(stage, filepath.FromSlash(ev))
				os.MkdirAll(filepath.Dir(dst), 0o755)
				os.Symlink(abs, dst)
			}
		}
	}
	return catalog.Validate(stage, entries)
}

// patchMarkdown rewrites exactly the three timeout bullets of vendor's
// section from facts; a missing or duplicated anchor is a conflict.
func patchMarkdown(md, vendor string, facts map[string]catalog.Fact, reportPath string) (string, error) {
	head := vendorSections[vendor]
	start := strings.Index(md, "\n"+head+"\n")
	if start < 0 || strings.Count(md, "\n"+head+"\n") != 1 {
		return "", conflict("docs/support-catalog.md has no single %q section", head)
	}
	start++
	end := len(md)
	if i := strings.Index(md[start+len(head):], "\n## "); i >= 0 {
		end = start + len(head) + i + 1
	}
	section := md[start:end]
	for _, key := range TimeoutKeys {
		anchor := bulletAnchors[key]
		lines := strings.SplitAfter(section, "\n")
		found := -1
		for i, l := range lines {
			if strings.HasPrefix(l, anchor) {
				if found >= 0 {
					return "", conflict("%s section has a duplicate %q bullet", head, anchor)
				}
				found = i
			}
		}
		if found < 0 {
			return "", conflict("%s section has no %q bullet", head, anchor)
		}
		f := facts[key]
		lines[found] = fmt.Sprintf("%s %s.** %s See the [qualification report](../%s).\n", anchor, f.Status, f.Value, reportPath)
		section = strings.Join(lines, "")
	}
	return md[:start] + section + md[end:], nil
}

// LoadPatch reads a proposed patch strictly.
func LoadPatch(outDir string) (*Patch, error) {
	b, err := os.ReadFile(filepath.Join(outDir, PatchFileName))
	if err != nil {
		return nil, contract.Wrap(contract.CodeInvalidArgument, "no proposed catalog patch in "+outDir+
			": publish only retries a run qualified with --publish-catalog (the base hashes are recorded when that run starts)", err)
	}
	var p Patch
	if err := decodeStrict(b, &p); err != nil {
		return nil, contract.Wrap(contract.CodeInvalidArgument, "invalid catalog patch", err)
	}
	return &p, nil
}

// Publish installs the proposed patch in outDir into repo (see the package
// comment above for the conflict and retry contract).
func Publish(outDir, repo string) error {
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return contract.New(contract.CodeInvalidArgument, fmt.Sprintf("repository %q must be an absolute clean path", repo))
	}
	p, err := LoadPatch(outDir)
	if err != nil {
		return err
	}
	rb, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	if err != nil {
		return conflict("no report: %v", err)
	}
	rep, err := ParseReport(rb)
	if err != nil {
		return conflict("%v", err)
	}
	if rep.RunID != p.RunID || !rep.Cleanup.OK || rep.Interrupted {
		return conflict("the report does not match the patch or its run did not end cleanly")
	}
	if err := rep.CheckEvidence(outDir); err != nil {
		return conflict("%v", err)
	}
	finalJSON, err1 := os.ReadFile(filepath.Join(outDir, proposedDir, "support-catalog.json"))
	finalMD, err2 := os.ReadFile(filepath.Join(outDir, proposedDir, "support-catalog.md"))
	if err := errors.Join(err1, err2); err != nil || sha256Hex(finalJSON) != p.FinalJSONSHA256 || sha256Hex(finalMD) != p.FinalMarkdownSHA256 {
		return conflict("the proposed catalog files are missing or differ from the patch")
	}
	type install struct {
		dest string
		data []byte
	}
	var files []install
	for _, f := range p.Files {
		if checkRelPath("patch source", f.Source) != nil || catalog.CheckEvidencePath(f.Dest) != nil || !strings.HasPrefix(f.Dest, EvidenceRoot+"/") {
			return conflict("patch file %s -> %s escapes its directory", f.Source, f.Dest)
		}
		b, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(f.Source)))
		if err != nil || sha256Hex(b) != f.SHA256 {
			return conflict("evidence %s is missing or differs from the patch", f.Source)
		}
		files = append(files, install{f.Dest, b})
	}
	// Preflight everything before the first write.
	for _, f := range files {
		if err := destState(repo, f.dest, f.data); err != nil {
			return err
		}
	}
	jsonDone, err := catalogState(repo, CatalogJSONPath, p.BaseJSONSHA256, p.FinalJSONSHA256)
	if err != nil {
		return err
	}
	mdDone, err := catalogState(repo, CatalogMDPath, p.BaseMarkdownSHA256, p.FinalMarkdownSHA256)
	if err != nil {
		return err
	}
	if !jsonDone {
		cur, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(CatalogJSONPath)))
		entries, err := catalog.Decode(cur)
		if err != nil {
			return conflict("%v", err)
		}
		for _, fc := range p.Facts {
			if !sameFact(entryFact(entries, fc.Client, fc.Key), fc.Prior) {
				return conflict("%s.%s differs from its expected prior value", fc.Client, fc.Key)
			}
		}
	}
	for _, f := range files {
		if err := installFile(repo, f.dest, f.data, func() error { return destState(repo, f.dest, f.data) }); err != nil {
			return err
		}
	}
	if !jsonDone {
		if err := installFile(repo, CatalogJSONPath, finalJSON, func() error {
			_, err := catalogState(repo, CatalogJSONPath, p.BaseJSONSHA256, p.FinalJSONSHA256)
			return err
		}); err != nil {
			return err
		}
	}
	if !mdDone {
		if err := installFile(repo, CatalogMDPath, finalMD, func() error {
			_, err := catalogState(repo, CatalogMDPath, p.BaseMarkdownSHA256, p.FinalMarkdownSHA256)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func entryFact(es []catalog.Entry, id, key string) catalog.Fact {
	for _, e := range es {
		if e.ID == id {
			return e.Facts[key]
		}
	}
	return catalog.Fact{}
}

func sameFact(a, b catalog.Fact) bool {
	x, _ := encodeJSON(a)
	y, _ := encodeJSON(b)
	return bytes.Equal(x, y)
}

// destState accepts an absent destination or one already holding exactly
// data; anything else is a conflict.
func destState(repo, rel string, data []byte) error {
	b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return conflict("%s: %v", rel, err)
	case !bytes.Equal(b, data):
		return conflict("%s already exists with different bytes", rel)
	}
	return nil
}

// catalogState reports whether rel already holds the final bytes; it is a
// conflict unless it holds the base or the final bytes.
func catalogState(repo, rel, base, final string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
	if err != nil {
		return false, conflict("%s: %v", rel, err)
	}
	switch sha256Hex(b) {
	case final:
		return true, nil
	case base:
		return false, nil
	}
	return false, conflict("%s changed since the run started (unrelated edits are never overwritten)", rel)
}

// installFile writes data to rel through a temporary file and a rename,
// rechecking the destination just before the rename.
func installFile(repo, rel string, data []byte, recheck func() error) error {
	dst := filepath.Join(repo, filepath.FromSlash(rel))
	if b, err := os.ReadFile(dst); err == nil && bytes.Equal(b, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	if err := recheck(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// CheckShortPollCatalog is the support catalog's short-poll policy
// consistency (design nonblocking-coordinator-waits): the Markdown keeps
// the interim anchor once, labelled retired, with the exact policy; the
// retired interim sentence appears nowhere (prose or JSON); and each
// vendor's three timeout facts keep owner 07b and their evidence. A
// VERIFIED timeout fact must be a qualification measurement backed by a
// published report under EvidenceRoot; an UNVERIFIED mcp_timeout is the
// shipped policy value (ending with the policy) or a published
// established-subset fact. Nothing here promotes or requires a fact.
func CheckShortPollCatalog(md string, entries []catalog.Entry) error {
	switch {
	case strings.Contains(md, LegacyInterimSentence):
		return fmt.Errorf("%s still carries the retired 07a interim exception", CatalogMDPath)
	case strings.Count(md, ShortPollAnchor+ShortPollPolicy) != 1 || strings.Count(md, `<a id="interim-mcp-wait-exception"></a>`) != 1:
		return fmt.Errorf("%s lacks the retired interim anchor's short-poll policy", CatalogMDPath)
	}
	for _, e := range entries {
		for key, f := range e.Facts {
			if strings.Contains(f.Value, LegacyInterimSentence) {
				return fmt.Errorf("%s.%s still carries the retired 07a interim exception", e.ID, key)
			}
		}
		for _, key := range TimeoutKeys {
			f, ok := e.Facts[key]
			switch {
			case !ok || len(f.Evidence) == 0 || f.VerificationIteration != catalog.Owner(e.ID, key):
				return fmt.Errorf("%s.%s lacks its evidence or owner", e.ID, key)
			case f.Status == catalog.Verified && (!strings.HasPrefix(f.Value, "Measured by qualification run ") || !publishedReport(f.Evidence)):
				return fmt.Errorf("%s.%s is VERIFIED without a published qualification report", e.ID, key)
			case f.Status == catalog.Unverified && key == "mcp_timeout" && !strings.HasSuffix(f.Value, " "+ShortPollPolicy) &&
				!strings.HasPrefix(f.Value, "Established subset from qualification run "):
				return fmt.Errorf("%s.mcp_timeout is neither the short-poll policy value nor a published measurement", e.ID)
			case f.Status != catalog.Verified && f.Status != catalog.Unverified:
				return fmt.Errorf("%s.%s has status %q", e.ID, key, f.Status)
			}
		}
	}
	return nil
}

// publishedReport reports whether evidence names a published
// qualification report (EvidenceRoot/<run>/report.json).
func publishedReport(evidence []string) bool {
	for _, ev := range evidence {
		if rel, ok := strings.CutPrefix(ev, EvidenceRoot+"/"); ok && strings.Count(rel, "/") == 1 && strings.HasSuffix(rel, "/report.json") {
			return true
		}
	}
	return false
}
