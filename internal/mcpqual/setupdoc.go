package mcpqual

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// SetupDocPath is the operator setup guide (FP-9).
const SetupDocPath = "docs/coordinator.md"

// SetupSection is one client section of the setup guide: its registration
// example (claude, codex, grok) or candidate JSON entry (cursor), its
// runbook launcher example and its fact rows.
type SetupSection struct {
	Client    string
	Register  []string
	Candidate string
	Runbook   []string
	Facts     map[string]SetupFact
	Text      string
}

// SetupFact is a documented fact row: its status and repository-relative
// evidence links.
type SetupFact struct {
	Status   string
	Evidence []string
}

var (
	fenceRe   = regexp.MustCompile("(?s)```(sh|json)\n(.*?)\n```")
	factRowRe = regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| (VERIFIED|UNVERIFIED|" + CatalogReferenceStatus + ") \\| (.*) \\|$")
	linkRe    = regexp.MustCompile(`\]\(\.\./([^)#\s]+)\)`)
)

// CatalogReferenceStatus is the setup guide's reference cell for a
// publisher-managed fact row (design catalog-version, amendment A1): a
// navigation instruction to the vendor's current fact in the catalog
// JSON, never a catalog status (catalog facts stay VERIFIED or
// UNVERIFIED).
const CatalogReferenceStatus = "SEE CATALOG"

// TimeoutStatusSentence is each vendor section's common timeout and
// configuration status sentence (amendment A1): it holds before and after
// any valid publication.
const TimeoutStatusSentence = "Timeout and configuration status: see the catalog for this vendor's current facts and evidence. A larger call budget requires local qualification with response margin."

// managedSetupRows are the guide rows that refer to the catalog: exactly
// the facts the publisher manages.
var managedSetupRows = []string{"mcp_config", "mcp_timeout", "mcp_timeout_override", "mcp_progress_extension"}

// CheckSetupFactRows checks a vendor section's five fact rows against the
// vendor's catalog entry (amendment A1): each publisher-managed row is
// exactly the CatalogReferenceStatus cell with the single catalog JSON
// link, and the runbook row carries the fact's current status and only
// links among its evidence. It checks the guide's references only, not
// the catalog facts' integrity; callers also check that every linked file
// exists.
func CheckSetupFactRows(section SetupSection, entry catalog.Entry) error {
	if section.Client != entry.ID {
		return fmt.Errorf("setup guide: %s section checked against the %s entry", section.Client, entry.ID)
	}
	for _, key := range append(append([]string(nil), managedSetupRows...), "runbook") {
		row, ok := section.Facts[key]
		switch {
		case !ok:
			return fmt.Errorf("setup guide: %s has no %s row", entry.ID, key)
		case key != "runbook" && (row.Status != CatalogReferenceStatus || len(row.Evidence) != 1 || row.Evidence[0] != CatalogJSONPath):
			return fmt.Errorf("setup guide: %s.%s row must be %q linking only %s, not %s %v", entry.ID, key, CatalogReferenceStatus, CatalogJSONPath, row.Status, row.Evidence)
		case key == "runbook" && (row.Status != entry.Facts[key].Status || len(row.Evidence) == 0):
			return fmt.Errorf("setup guide: %s.runbook row %s %v, catalog %s", entry.ID, row.Status, row.Evidence, entry.Facts[key].Status)
		}
		if key == "runbook" {
			for _, ev := range row.Evidence {
				if !slices.Contains(entry.Facts[key].Evidence, ev) {
					return fmt.Errorf("setup guide: %s.runbook links %s, not the fact's evidence %v", entry.ID, ev, entry.Facts[key].Evidence)
				}
			}
		}
	}
	return nil
}

// ParseSetupDoc splits the guide into the four client sections.
func ParseSetupDoc(doc string) (map[string]SetupSection, error) {
	out := map[string]SetupSection{}
	for client, head := range vendorSections {
		i := strings.Index(doc, "\n"+head+"\n")
		if i < 0 {
			return nil, fmt.Errorf("setup guide: no %q section", head)
		}
		body := doc[i+len(head)+2:]
		if j := strings.Index(body, "\n## "); j >= 0 {
			body = body[:j]
		}
		s := SetupSection{Client: client, Text: body, Facts: map[string]SetupFact{}}
		var sh []string
		for _, m := range fenceRe.FindAllStringSubmatch(body, -1) {
			if m[1] == "json" {
				s.Candidate = m[2]
			} else {
				sh = append(sh, m[2])
			}
		}
		wantSh := 2
		if client == "cursor" {
			wantSh = 1
		}
		if len(sh) != wantSh || (client == "cursor") != (s.Candidate != "") {
			return nil, fmt.Errorf("setup guide: %s section has %d shell examples", client, len(sh))
		}
		var err error
		if client != "cursor" {
			if s.Register, err = ShellWords(sh[0]); err != nil {
				return nil, err
			}
		}
		if s.Runbook, err = ShellWords(sh[len(sh)-1]); err != nil {
			return nil, err
		}
		for _, m := range factRowRe.FindAllStringSubmatch(body, -1) {
			f := SetupFact{Status: m[2]}
			for _, l := range linkRe.FindAllStringSubmatch(m[3], -1) {
				f.Evidence = append(f.Evidence, l[1])
			}
			s.Facts[m[1]] = f
		}
		out[client] = s
	}
	return out, nil
}

// ShellWords splits a POSIX-shell-like command line into words, honouring
// single and double quotes literally. It never expands or executes
// anything: "$(cat -- '<runbook>')" stays one literal word.
func ShellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	in := false
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in " + s)
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}

// CalleeArgv returns the Callsheet command after the registration's "--"
// with only the binary, plane and CA placeholders substituted.
func CalleeArgv(register []string, binary, plane, ca string) ([]string, error) {
	i := -1
	for j, w := range register {
		if w == "--" {
			i = j
			break
		}
	}
	if i < 0 || i+1 >= len(register) {
		return nil, errors.New("registration example has no \"-- <callsheet-binary> ...\" command")
	}
	r := strings.NewReplacer("<callsheet-binary>", binary, "<plane-url>", plane, "<ca-path>", ca)
	var out []string
	for _, w := range register[i+1:] {
		out = append(out, r.Replace(w))
	}
	return out, nil
}
