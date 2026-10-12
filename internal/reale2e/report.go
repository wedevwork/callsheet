package reale2e

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// The shareable report (design 12a-real-e2e, Evidence and checker, A3):
// only allowlisted structured fields (run ID, flow kind, revision,
// platform, tool versions, fixed criterion IDs and codes, the protected
// roots, the environment's protected changes and tool-maintained summary
// (A3.6: report.txt only), the
// coordinator's tool findings with their private evidence references, and
// relative artifact names with their hashes), never raw logs, prompts,
// transcript or final prose. It is derived from the bundle alone (paths
// were tokenized when written), so the offline check recomputes it byte
// for byte. Mechanical PASS never claims the role results were judged:
// those stay pending owner/coordinator review.

// Report is report.json and the check command's output.
type Report struct {
	Schema         string          `json:"schema"`
	RunID          string          `json:"run_id"`
	Result         string          `json:"result"`
	Flow           string          `json:"flow"`
	Claim          string          `json:"claim"`
	Judgments      string          `json:"judgments"`
	Revision       string          `json:"revision"`
	Platform       string          `json:"platform"`
	Versions       []VersionRecord `json:"versions"`
	ProtectedRoots []string        `json:"protected_roots"`
	ProtectDepth   int             `json:"protect_depth"`
	Criteria       []Criterion     `json:"criteria"`
	Protected      []EnvChange     `json:"protected_changes"`
	// Housekeeping is always empty since A3.6 (tool-maintained changes
	// are summarized in report.txt only); the field set is unchanged.
	Housekeeping []EnvChange `json:"housekeeping_changes"`
	// OversizedConfig names configuration files fingerprinted by metadata
	// only because they exceed 1 MiB (A3.3).
	OversizedConfig []string     `json:"oversized_config"`
	ToolFindings    []MapFinding `json:"tool_findings"`
	Artifacts       []FileHash   `json:"artifacts"`
	Error           string       `json:"error,omitempty"`
	// tool is the diff's tool-maintained summary for report.txt.
	tool toolReport
}

// toolReport is the tool-maintained summary the text report prints: one
// row per vendor top-level directory and the retained and omitted detail
// counts.
type toolReport struct {
	rows            []ToolSummaryRow
	detail, omitted int
	present         bool
}

// ClaudeStateExclusions are the owner-accepted coverage exclusions
// (A3.6), printed adjacently in report.txt and docs/real-e2e.md.
var ClaudeStateExclusions = []string{
	"not covered: ~/.claude.json",
	"not covered: ~/.claude/.config.json (Claude state file; holds per-project tool grants, MCP-server lists and trust flags)",
	"not covered: ~/.claude/backups/.config.json.backup.* (backups of Claude state, including per-project grants)",
}

// NestedRepoLimitation is the report's statement of the A3.1 discovery
// limit.
const NestedRepoLimitation = "a nested repository that its parent repository ignores is not separately protected"

// Report results.
const (
	ResultPass = "PASS"
	ResultFail = "FAIL"
)

// Redactor replaces known local prefixes and credential shapes. HomeAlias
// and CheckoutAlias are a second spelling of the same directory (symlinks
// resolved, or as the owner wrote it), replaced by the same token.
type Redactor struct {
	Home, Run, Checkout      string
	HomeAlias, CheckoutAlias string
}

var (
	urlCredRE   = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s:]+(:[^/@\s]*)?@`)
	credFieldRE = regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password|passwd|authorization|bearer|cookie|session[_-]?key)(\s*[:=]\s*|\s+)("?)[^\s"]+`)
	credShapeRE = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[abpr]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})\b`)
	runtimeRE   = regexp.MustCompile(`/tmp/ce-[A-Za-z0-9]+`)
)

// Redact returns s with local prefixes tokenized and credentials removed.
func (r Redactor) Redact(s string) string {
	// Longest prefix first, so a checkout inside the home directory stays
	// $CHECKOUT.
	type pre struct{ path, token string }
	pres := []pre{{r.Checkout, "$CHECKOUT"}, {r.CheckoutAlias, "$CHECKOUT"}, {r.Run, "$RUN"}, {r.Home, "$HOME"}, {r.HomeAlias, "$HOME"}}
	sort.SliceStable(pres, func(i, j int) bool { return len(pres[i].path) > len(pres[j].path) })
	for _, p := range pres {
		if len(p.path) > 1 {
			s = strings.ReplaceAll(s, p.path, p.token)
		}
	}
	s = runtimeRE.ReplaceAllString(s, "$$RUN")
	s = urlCredRE.ReplaceAllString(s, "${1}REDACTED@")
	s = credFieldRE.ReplaceAllString(s, "${1}${2}${3}REDACTED")
	return credShapeRE.ReplaceAllString(s, "REDACTED")
}

// claimFor states what a result demonstrates.
func claimFor(result, flow string) string {
	switch {
	case result != ResultPass:
		return "not demonstrated"
	case flow == FlowDefault:
		return "the default flow's mechanics and deterministic feature cases passed on this installation"
	}
	return "an override flow's mechanics passed; the owner's default pairs are not demonstrated"
}

// derivedFiles are the report and audit outputs (and the optional
// external review), excluded from every source inventory.
var derivedFiles = map[string]bool{"report.json": true, "report.txt": true, "audit.json": true, "audit.md": true, "audit-review.md": true}

// sanitizeChange keeps a change's structured fields, redacted.
func sanitizeChange(c EnvChange) EnvChange {
	red := Redactor{}
	c.Scope, c.Path = red.Redact(c.Scope), red.Redact(c.Path)
	if c.Fields == nil {
		c.Fields = []string{}
	}
	return c
}

// BuildReport assembles the report of a loaded bundle's criteria.
func BuildReport(b *Bundle, cs []Criterion) Report {
	red := Redactor{}
	r := Report{Schema: ReportSchema, Result: ResultFail, Criteria: cs, Versions: []VersionRecord{}, Artifacts: []FileHash{}, ProtectedRoots: []string{},
		Protected: []EnvChange{}, Housekeeping: []EnvChange{}, OversizedConfig: []string{}, ToolFindings: []MapFinding{}, Judgments: PendingReview}
	if passed(cs) {
		r.Result = ResultPass
	}
	if m := b.Manifest; m != nil {
		if ValidRunID(m.RunID) {
			r.RunID = m.RunID
		}
		r.Flow = m.FlowKind
		if m.FlowKind != FlowDefault && m.FlowKind != FlowOverride {
			r.Flow = ""
		}
		if hex40RE.MatchString(m.Revision) {
			r.Revision = m.Revision
		}
		r.Platform = red.Redact(m.GOOS + "/" + m.GOARCH)
		for _, v := range m.Versions {
			r.Versions = append(r.Versions, VersionRecord{Tool: red.Redact(v.Tool), Path: red.Redact(v.Path), Version: red.Redact(v.Version), Eligible: v.Eligible})
		}
		r.ProtectDepth = m.ProtectDepth
		for _, root := range m.ProtectedRoots {
			r.ProtectedRoots = append(r.ProtectedRoots, red.Redact(root))
		}
	}
	if d := b.Diff; d != nil {
		for _, c := range d.Protected {
			r.Protected = append(r.Protected, sanitizeChange(c))
		}
		r.tool = toolReport{detail: d.ToolDetailCount, omitted: d.ToolOmittedCount, present: true}
		for _, row := range d.ToolSummary {
			row.Scope, row.TopLevel = red.Redact(row.Scope), red.Redact(row.TopLevel)
			r.tool.rows = append(r.tool.rows, row)
		}
		for _, n := range d.OversizedConfig {
			r.OversizedConfig = append(r.OversizedConfig, red.Redact(n))
		}
	}
	if em := b.EventMap; em != nil {
		for _, f := range em.Findings {
			if f.Code == CodeToolDenied || f.Code == CodeToolFailed {
				r.ToolFindings = append(r.ToolFindings, MapFinding{Code: f.Code, ToolUseID: f.ToolUseID, Tool: red.Redact(f.Tool), Sources: f.Sources})
			}
		}
	}
	r.Claim = claimFor(r.Result, r.Flow)
	for name, data := range b.Files {
		if derivedFiles[name] {
			continue
		}
		r.Artifacts = append(r.Artifacts, FileHash{Name: name, SHA256: sha256Hex(data)})
	}
	sort.Slice(r.Artifacts, func(i, j int) bool { return r.Artifacts[i].Name < r.Artifacts[j].Name })
	return r
}

// schemaReport is the report of a bundle that could not be validated.
func schemaReport() Report {
	return Report{Schema: ReportSchema, Result: ResultFail, Claim: claimFor(ResultFail, ""), Judgments: PendingReview, Versions: []VersionRecord{},
		ProtectedRoots: []string{}, Criteria: []Criterion{}, Protected: []EnvChange{}, Housekeeping: []EnvChange{}, OversizedConfig: []string{}, ToolFindings: []MapFinding{},
		Artifacts: []FileHash{}, Error: "evidence_schema_error"}
}

// RenderText is the human-readable summary of r.
func RenderText(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "result: %s (mechanical and deterministic criteria)\n%s\n", r.Result, JudgmentReportLine)
	fmt.Fprintf(&b, "run: %s\nflow: %s\nclaim: %s\nrevision: %s\nplatform: %s\n", r.RunID, r.Flow, r.Claim, r.Revision, r.Platform)
	if r.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", r.Error)
	}
	for _, v := range r.Versions {
		fmt.Fprintf(&b, "version %s: %s\n", v.Tool, v.Version)
	}
	if len(r.ProtectedRoots) == 0 {
		b.WriteString("protected roots: No repository root was protected (only the four vendor homes)\n")
	}
	for _, root := range r.ProtectedRoots {
		fmt.Fprintf(&b, "protected root: %s\n", root)
	}
	fmt.Fprintf(&b, "protected depth: repositories at most %d directory levels below each protected root; deeper repositories are not protected\n", r.ProtectDepth)
	for _, l := range ClaudeStateExclusions {
		b.WriteString(l + "\n")
	}
	b.WriteString("not covered: " + NestedRepoLimitation + "\n")
	for _, c := range r.Criteria {
		if len(c.Codes) == 0 {
			fmt.Fprintf(&b, "criterion %s: %s\n", c.ID, c.Result)
			continue
		}
		fmt.Fprintf(&b, "criterion %s: %s (%s)\n", c.ID, c.Result, strings.Join(c.Codes, ", "))
	}
	for _, c := range r.Protected {
		fmt.Fprintf(&b, "protected change: %s %s %s [%s]\n", c.Scope, c.Path, c.Kind, strings.Join(c.Fields, ","))
	}
	for _, n := range r.OversizedConfig {
		fmt.Fprintf(&b, "configuration file over 1 MiB, fingerprinted by metadata only: %s\n", n)
	}
	if t := r.tool; t.present {
		fmt.Fprintf(&b, "tool-maintained changes (report-only): %d (%d detailed in diff.json, %d omitted)\n", t.detail+t.omitted, t.detail, t.omitted)
		for _, row := range t.rows {
			fmt.Fprintf(&b, "  tool-maintained %s %s: %d added, %d changed, %d deleted, %d total\n", row.Scope, row.TopLevel, row.Added, row.Changed,
				row.Deleted, row.Total)
		}
	}
	for _, f := range r.ToolFindings {
		ref := ""
		if len(f.Sources) > 0 {
			ref = fmt.Sprintf(" (private evidence %s lines %d-%d)", f.Sources[0].File, f.Sources[0].FirstLine, f.Sources[0].LastLine)
		}
		fmt.Fprintf(&b, "coordinator tool %s: %s tool-use %s%s\n", f.Code, f.Tool, f.ToolUseID, ref)
	}
	fmt.Fprintf(&b, "artifacts: %d files (names and SHA-256 in report.json)\n", len(r.Artifacts))
	return b.String()
}

// CheckEvidence runs the offline check of root and writes the JSON report
// to out: 0 for PASS, 1 for failed or incomplete evidence, 2 for schema
// errors (a version 1 bundle included). It writes nothing to the bundle
// and executes nothing.
func CheckEvidence(root string, out, errOut io.Writer) int {
	b, err := LoadBundle(root)
	if err != nil {
		// Every load failure is a schema error: malformed or unsafe
		// evidence is never partially judged.
		data, _ := encodeJSON(schemaReport())
		out.Write(data)
		fmt.Fprintf(errOut, "reale2e check: %v\n", err)
		return 2
	}
	r := BuildReport(b, Check(b))
	data, _ := encodeJSON(r)
	out.Write(data)
	if r.Result == ResultPass {
		return 0
	}
	return 1
}
