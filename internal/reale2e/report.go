package reale2e

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// The shareable report (design 12a-real-e2e, Evidence and checker): only
// allowlisted structured fields (run ID, flow kind, revision, platform,
// tool versions, fixed criterion IDs and codes, and relative artifact
// names with their hashes), never raw logs, prompts, transcript or final
// prose. Version strings pass through the redactor: known home, runtime
// and checkout prefixes become $HOME, $RUN and $CHECKOUT, URL credentials
// and credential-shaped fields are replaced. Output is deterministic.

// Report is report.json and the check command's output.
type Report struct {
	Schema    string          `json:"schema"`
	RunID     string          `json:"run_id"`
	Result    string          `json:"result"`
	Flow      string          `json:"flow"`
	Claim     string          `json:"claim"`
	Revision  string          `json:"revision"`
	Platform  string          `json:"platform"`
	Versions  []VersionRecord `json:"versions"`
	Criteria  []Criterion     `json:"criteria"`
	Artifacts []FileHash      `json:"artifacts"`
	Error     string          `json:"error,omitempty"`
}

// Report results.
const (
	ResultPass = "PASS"
	ResultFail = "FAIL"
)

// Redactor replaces known local prefixes and credential shapes.
type Redactor struct {
	Home, Run, Checkout string
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
	pres := []pre{{r.Checkout, "$CHECKOUT"}, {r.Run, "$RUN"}, {r.Home, "$HOME"}}
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
		return "the default flow passed on this installation"
	}
	return "an override flow passed; the owner's default pairs are not demonstrated"
}

// BuildReport assembles the report of a loaded bundle's criteria.
func BuildReport(b *Bundle, cs []Criterion, red Redactor) Report {
	r := Report{Schema: ReportSchema, Result: ResultFail, Criteria: cs, Versions: []VersionRecord{}, Artifacts: []FileHash{}}
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
	}
	r.Claim = claimFor(r.Result, r.Flow)
	for name, data := range b.Files {
		if name == "report.json" || name == "report.txt" {
			continue
		}
		r.Artifacts = append(r.Artifacts, FileHash{Name: name, SHA256: sha256Hex(data)})
	}
	sort.Slice(r.Artifacts, func(i, j int) bool { return r.Artifacts[i].Name < r.Artifacts[j].Name })
	return r
}

// schemaReport is the report of a bundle that could not be validated.
func schemaReport() Report {
	return Report{Schema: ReportSchema, Result: ResultFail, Claim: claimFor(ResultFail, ""), Versions: []VersionRecord{},
		Criteria: []Criterion{}, Artifacts: []FileHash{}, Error: "evidence_schema_error"}
}

// RenderText is the human-readable summary of r.
func RenderText(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "result: %s\nrun: %s\nflow: %s\nclaim: %s\nrevision: %s\nplatform: %s\n", r.Result, r.RunID, r.Flow, r.Claim, r.Revision, r.Platform)
	if r.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", r.Error)
	}
	for _, v := range r.Versions {
		fmt.Fprintf(&b, "version %s: %s\n", v.Tool, v.Version)
	}
	for _, c := range r.Criteria {
		if len(c.Codes) == 0 {
			fmt.Fprintf(&b, "criterion %s: %s\n", c.ID, c.Result)
			continue
		}
		fmt.Fprintf(&b, "criterion %s: %s (%s)\n", c.ID, c.Result, strings.Join(c.Codes, ", "))
	}
	fmt.Fprintf(&b, "artifacts: %d files (names and SHA-256 in report.json)\n", len(r.Artifacts))
	return b.String()
}

// CheckEvidence runs the offline check of root and writes the JSON report
// to out: 0 for PASS, 1 for failed or incomplete evidence, 2 for schema
// errors. It writes nothing to the bundle.
func CheckEvidence(root string, red Redactor, out, errOut io.Writer) int {
	b, err := LoadBundle(root)
	if err != nil {
		// Every load failure is a schema error: malformed or unsafe
		// evidence is never partially judged.
		data, _ := encodeJSON(schemaReport())
		out.Write(data)
		fmt.Fprintf(errOut, "reale2e check: %v\n", err)
		return 2
	}
	r := BuildReport(b, Check(b), red)
	data, _ := encodeJSON(r)
	out.Write(data)
	if r.Result == ResultPass {
		return 0
	}
	return 1
}
