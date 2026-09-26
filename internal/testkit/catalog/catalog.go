// Package catalog validates the checked-in first-wave CLI support catalog
// (tests/testdata/support-catalog.json). It checks structure and
// completeness only; it never claims to establish vendor behaviour.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Status values.
const (
	Verified   = "VERIFIED"
	Unverified = "UNVERIFIED"
)

// RequiredFacts are the fact keys every entry must carry.
var RequiredFacts = []string{
	"headless", "model", "effort", "approval", "sandbox_linux", "sandbox_macos",
	"final_message", "exit_codes", "mcp_config", "mcp_timeout",
	"mcp_timeout_override", "mcp_progress_extension", "runbook",
}

// Vendors maps each first-wave vendor id to its verifying iteration.
var Vendors = map[string]string{"claude": "08", "codex": "08", "grok": "11", "cursor": "11"}

// Fact is one field-level claim.
type Fact struct {
	Status                string   `json:"status"`
	Value                 string   `json:"value"`
	Evidence              []string `json:"evidence"`
	VerificationIteration string   `json:"verification_iteration"`
}

// Entry is one CLI's fact sheet.
type Entry struct {
	ID       string          `json:"id"`
	Version  string          `json:"version"`
	Platform string          `json:"platform"`
	Facts    map[string]Fact `json:"facts"`
}

// Load strictly decodes a catalog manifest (unknown fields are errors).
func Load(p string) ([]Entry, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

// Decode strictly decodes manifest bytes.
func Decode(b []byte) ([]Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var entries []Entry
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("catalog: decode: %w", err)
	}
	return entries, nil
}

// CheckEvidencePath rejects absolute, unclean or escaping evidence paths;
// evidence is repository-root-relative with forward slashes.
func CheckEvidencePath(p string) error {
	switch {
	case p == "":
		return errors.New("empty evidence path")
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || filepath.IsAbs(p) || filepath.VolumeName(p) != "":
		return fmt.Errorf("evidence path %q is absolute", p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("evidence path %q must use forward slashes", p)
	case path.Clean(p) != p:
		return fmt.Errorf("evidence path %q is not clean", p)
	case p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("evidence path %q escapes the repository", p)
	}
	return nil
}

var iterationRef = regexp.MustCompile(`(?i)\biteration (08|11)\b`)

// Validate checks entries against the catalog contract, resolving evidence
// against root (the repository root). It returns every problem found.
func Validate(root string, entries []Entry) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if len(entries) != len(Vendors) {
		add("catalog has %d entries, want %d", len(entries), len(Vendors))
	}
	seen := map[string]bool{}
	for i, e := range entries {
		where := fmt.Sprintf("entry %d (%s)", i, e.ID)
		want, known := Vendors[e.ID]
		if !known {
			add("%s: unknown vendor id", where)
		}
		if seen[e.ID] {
			add("%s: duplicate vendor id", where)
		}
		seen[e.ID] = true
		if strings.TrimSpace(e.Version) == "" {
			add("%s: missing version", where)
		}
		if strings.TrimSpace(e.Platform) == "" {
			add("%s: missing platform", where)
		}
		if len(e.Facts) != len(RequiredFacts) {
			var keys []string
			for k := range e.Facts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			add("%s: facts %v, want exactly %v", where, keys, RequiredFacts)
		}
		for _, key := range RequiredFacts {
			f, ok := e.Facts[key]
			if !ok {
				add("%s: missing fact %s", where, key)
				continue
			}
			validateFact(root, where+"."+key, f, want, known, add)
		}
	}
	for id := range Vendors {
		if !seen[id] {
			add("missing vendor %s", id)
		}
	}
	return errors.Join(errs...)
}

func validateFact(root, where string, f Fact, want string, known bool, add func(string, ...any)) {
	if strings.TrimSpace(f.Value) == "" {
		add("%s: empty value", where)
	}
	if f.VerificationIteration != "08" && f.VerificationIteration != "11" {
		add("%s: verification_iteration %q must be 08 or 11", where, f.VerificationIteration)
	} else if known && f.VerificationIteration != want {
		add("%s: verification_iteration %s, want %s for this vendor", where, f.VerificationIteration, want)
	}
	for _, ev := range f.Evidence {
		if err := CheckEvidencePath(ev); err != nil {
			add("%s: %v", where, err)
			continue
		}
		st, err := os.Stat(filepath.Join(root, filepath.FromSlash(ev)))
		if err != nil || st.IsDir() || st.Size() == 0 {
			add("%s: evidence %s is not a nonempty file", where, ev)
		}
	}
	switch f.Status {
	case Verified:
		if len(f.Evidence) == 0 {
			add("%s: VERIFIED fact without evidence", where)
		}
		if strings.Contains(f.Value, Unverified) || strings.Contains(strings.ToLower(f.Value), "unverified") || strings.Contains(strings.ToLower(f.Value), "unknown") {
			add("%s: VERIFIED fact mixes unverified content; split it or mark it UNVERIFIED", where)
		}
	case Unverified:
		m := iterationRef.FindStringSubmatch(f.Value)
		if m == nil {
			add("%s: UNVERIFIED fact must name its qualification work (iteration 08/11)", where)
		} else if m[1] != f.VerificationIteration {
			add("%s: value names iteration %s but verification_iteration is %s", where, m[1], f.VerificationIteration)
		}
	default:
		add("%s: status %q must be VERIFIED or UNVERIFIED", where, f.Status)
	}
}

var mdLink = regexp.MustCompile(`\]\(([^)#\s]+)\)`)

// CheckDocLinks verifies every relative markdown link in the document at
// docPath resolves to an existing file or directory.
func CheckDocLinks(docPath string) error {
	b, err := os.ReadFile(docPath)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range mdLink.FindAllStringSubmatch(string(b), -1) {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if strings.Contains(target, "design-check/") {
			errs = append(errs, fmt.Errorf("link %s points at gitignored design files", target))
			continue
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(docPath), filepath.FromSlash(target))); err != nil {
			errs = append(errs, fmt.Errorf("broken link %s", target))
		}
	}
	return errors.Join(errs...)
}
