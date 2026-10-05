package containeracceptance

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// claimDocs are the bundled milestone documents (/fixtures/docs).
var claimDocs = []string{"real-adapters.md", "workspaces.md", "ci.md"}

// m4Mapping is the required M4 check mapping: each check's row, its gate
// and its container case ("none" for the optional external delivery).
var m4Mapping = []struct{ check, gate, caseID string }{
	{"M4-1 Laptop push", "FP-3", CasePublication},
	{"M4-2 Remote cwd is the base", "FP-3", CasePublication},
	{"M4-3 A metadata and diff", "FP-3", CasePublication},
	{"M4-4 Second hop", "FP-4", CaseContinuation},
	{"M4-5 Parallel sibling", "FP-4", CaseContinuation},
	{"M4-6 Partial results", "FP-5", CasePartialResults},
	{"M4-7 Lost task", "FP-6", CaseLost},
	{"M4-8 Restart around publication", "FP-7", CasePublicationRestart},
	{"M4-9 Laptop pull", "FP-8", CaseDirtyPull},
	{"M4-10 External delivery (optional)", "optional, not gated", ""},
}

// Bounded claim statements every gate section makes.
var scopeClaims = []string{
	"demonstrates M3/M4 under Linux loopback, fake workers and a deterministic CLI coordinator",
	"does not demonstrate two machines, two operating systems, a real vendor process, vendor authentication or paid model calls",
	"No macOS container proof is required",
	"optional, non-gating",
	"never a second milestone requirement",
}

// Superseded claims that made the physical-machine session mandatory or
// claimed M3 had happened.
var supersededClaims = map[string][]string{
	"workspaces.md": {
		"These are manual checks for the two-machine M3+M4 acceptance session",
		"does not perform or qualify them",
	},
	"real-adapters.md": {
		"It is demonstrated only when the following manual procedure passes on real machines",
		"M3 was demonstrated", "M3 is demonstrated", "M3 has been demonstrated", "M3 passed",
	},
	"ci.md": {
		"remain unobserved until performed separately",
		"names the manual M4 checks, never that the two-machine session ran",
	},
}

// flat collapses every run of whitespace so Markdown wrapping never
// matters.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// section returns the text of "## heading" up to the next "## " heading.
func section(doc, heading string) (string, bool) {
	_, rest, ok := strings.Cut("\n"+doc, "\n## "+heading+"\n")
	if !ok {
		return "", false
	}
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	return rest, true
}

// CheckClaims checks the bundled milestone documents: the container
// command is the blocking M3 and M4 demonstration, every M4 check maps to
// its FP and container case (M4-10 optional), the actual 14-case manifest
// is named, the bounded scope statements and the M4-8 boundary are
// present, the CI publication is explained, and no superseded mandatory
// physical-machine or demonstrated-M3 claim remains.
func CheckClaims(docs map[string][]byte) error {
	var errs []error
	bad := func(doc, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", doc, fmt.Sprintf(format, args...)))
	}
	for _, name := range claimDocs {
		if _, ok := docs[name]; !ok {
			bad(name, "missing from the bundle")
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	require := func(doc, text string, terms ...string) {
		f := flat(text)
		for _, t := range terms {
			if !strings.Contains(f, flat(t)) {
				bad(doc, "lacks %q", t)
			}
		}
	}
	ws, ok := section(string(docs["workspaces.md"]), "Manual M4 checks")
	if !ok {
		bad("workspaces.md", "has no Manual M4 checks section")
	} else {
		require("workspaces.md", ws, "`go run ./cmd/devcheck container-e2e`", "after acknowledged publication, before coordinator delivery",
			"this slice does not restart the plane", "/tmp/callsheet-container-e2e-evidence/report.txt")
		require("workspaces.md", ws, scopeClaims...)
		for _, id := range RequiredCaseIDs() {
			require("workspaces.md", ws, "`"+id+"`")
		}
		rows := map[string]string{}
		for _, line := range strings.Split(ws, "\n") {
			if rest, ok := strings.CutPrefix(line, "| **"); ok {
				check, _, _ := strings.Cut(rest, ":**")
				if _, dup := rows[check]; dup {
					bad("workspaces.md", "maps %s twice", check)
				}
				rows[check] = line
			}
		}
		if len(rows) != len(m4Mapping) {
			bad("workspaces.md", "maps %d M4 checks, want %d", len(rows), len(m4Mapping))
		}
		for _, m := range m4Mapping {
			row, ok := rows[m.check]
			cell := "| `" + m.caseID + "` |"
			if m.caseID == "" {
				cell = "| none |"
			}
			if !ok || !strings.Contains(row, "| "+m.gate+" |") || !strings.HasSuffix(strings.TrimSpace(row), cell) {
				bad("workspaces.md", "does not map %s to %s and %s: %q", m.check, m.gate, cell, row)
			}
		}
	}
	ra, ok := section(string(docs["real-adapters.md"]), "Remote acceptance (M3)")
	if !ok {
		bad("real-adapters.md", "has no Remote acceptance (M3) section")
	} else {
		require("real-adapters.md", ra, "go run ./cmd/devcheck container-e2e", "FP-2", "`TestContainerCoordinator`", "`prepare`", "`goal_answer`",
			"`TestContainerSampleFlow`", "workspaces.md#manual-m4-checks", "M3 has not been demonstrated")
		require("real-adapters.md", ra, scopeClaims...)
	}
	require("ci.md", string(docs["ci.md"]), "Publish container E2E evidence", "`cat /tmp/callsheet-container-e2e-evidence/report.txt`",
		"go run ./cmd/devcheck container-e2e --count=20", "not as a downloadable artifact")
	for doc, claims := range supersededClaims {
		f := flat(string(docs[doc]))
		for _, c := range claims {
			if strings.Contains(f, flat(c)) {
				bad(doc, "keeps the superseded claim %q", c)
			}
		}
	}
	return errors.Join(errs...)
}

// b64 is a path's standard base64 (the diff rows' path encoding).
func b64(p string) string { return base64.StdEncoding.EncodeToString([]byte(p)) }
