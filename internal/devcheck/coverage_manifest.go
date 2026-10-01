package devcheck

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Coverage policy of new and changed code (iteration 09a, extended by
// iteration 09b): besides the project-wide gate, the statement-weighted
// coverage of the new and changed production code must be strictly above
// CoverageThreshold for each group, computed from the same generated
// profile: the workspace hub package (GroupWorkspace), the local transfer
// package (GroupTransfer, 09b) and the union of every other new or
// changed production block (GroupChanged). Since 09b every applicable
// listed file must also pass on its own, over all of its profile blocks
// (whatever legacy Ranges select for its group). The manifest
// (coverage_manifest_data.go) is committed: CI reads no git base and needs
// no network. Reviewers compare it with the implementation diff.
//
// An entry's OS selects where it is evaluated: "" is common (every host),
// "linux" or "darwin" native-only (only that host's coverage job observes
// it; another host skips it, which is never a passing observation).
// Hosts are never aggregated: each host's job must pass on its own.

// Coverage manifest groups.
const (
	GroupWorkspace = "workspace"
	GroupChanged   = "changed"
	GroupTransfer  = "workspacetransfer"
)

// coverageGroups are the groups in report order.
var coverageGroups = []string{GroupWorkspace, GroupChanged, GroupTransfer}

// coverageOS are the valid entry OS values.
var coverageOS = map[string]bool{"": true, "linux": true, "darwin": true}

// CoverageEntry selects profile blocks of one file for its group: all of
// them when Ranges is empty, else every block overlapping one of the
// inclusive line ranges. OS is "" (common) or the only host ("linux",
// "darwin") that evaluates it.
type CoverageEntry struct {
	Group  string
	File   string
	OS     string
	Ranges [][2]int
}

// GroupCoverage is one group's result.
type GroupCoverage struct {
	Group              string
	Statements, Hit    int
	Blocks             int
	Percent            float64
	Files              int
	SelectedEntryCount int
}

// profileBlock is one deduplicated profile block.
type profileBlock struct {
	file               string
	startLine, endLine int
	stmts              int
	hit                bool
}

// parseProfile deduplicates a coverage profile's blocks (the same block
// written by several test binaries counts once, covered if any count is
// positive).
func parseProfile(profile string) (map[string]map[string]*profileBlock, error) {
	lines := strings.Split(strings.TrimSpace(profile), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "mode: ") {
		return nil, errors.New("devcheck: coverage profile has no mode line")
	}
	files := map[string]map[string]*profileBlock{}
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		// file:start.col,end.col stmts count
		colon := strings.LastIndex(l, ":")
		f := strings.Fields(l[colon+1:])
		if colon < 0 || len(f) != 3 {
			return nil, fmt.Errorf("devcheck: malformed coverage profile line %q", l)
		}
		file, pos := l[:colon], f[0]
		start, end, ok := strings.Cut(pos, ",")
		sl, _, ok1 := strings.Cut(start, ".")
		el, _, ok2 := strings.Cut(end, ".")
		s, err1 := strconv.Atoi(sl)
		e, err2 := strconv.Atoi(el)
		stmts, err3 := strconv.Atoi(f[1])
		count, err4 := strconv.Atoi(f[2])
		if !ok || !ok1 || !ok2 || errors.Join(err1, err2, err3, err4) != nil {
			return nil, fmt.Errorf("devcheck: malformed coverage profile line %q", l)
		}
		blocks := files[file]
		if blocks == nil {
			blocks = map[string]*profileBlock{}
			files[file] = blocks
		}
		b := blocks[pos]
		if b == nil {
			b = &profileBlock{file: file, startLine: s, endLine: e, stmts: stmts}
			blocks[pos] = b
		}
		b.hit = b.hit || count > 0
	}
	return files, nil
}

// validateManifest checks every entry's group, OS, path and range syntax,
// and that a file's entries agree on OS and group, before any host
// filtering: an unknown OS or group fails on every host.
func validateManifest(manifest []CoverageEntry) error {
	seen := map[string]CoverageEntry{}
	for _, en := range manifest {
		switch {
		case en.Group != GroupWorkspace && en.Group != GroupChanged && en.Group != GroupTransfer:
			return fmt.Errorf("devcheck: coverage manifest entry %s has unknown group %q", en.File, en.Group)
		case !coverageOS[en.OS]:
			return fmt.Errorf("devcheck: coverage manifest entry %s has unknown OS %q (want \"\", linux or darwin)", en.File, en.OS)
		case !strings.HasPrefix(en.File, modulePath+"/") || !strings.HasSuffix(en.File, ".go") || strings.HasSuffix(en.File, "_test.go") ||
			strings.Contains(en.File, "//") || strings.Contains(en.File, ".."):
			return fmt.Errorf("devcheck: coverage manifest entry %q is not a production Go file of %s", en.File, modulePath)
		}
		for _, r := range en.Ranges {
			if r[0] < 1 || r[1] < r[0] {
				return fmt.Errorf("devcheck: coverage manifest range %d-%d of %s is invalid", r[0], r[1], en.File)
			}
		}
		if prev, ok := seen[en.File]; ok && (prev.OS != en.OS || prev.Group != en.Group) {
			return fmt.Errorf("devcheck: coverage manifest lists %s with conflicting OS or group", en.File)
		}
		seen[en.File] = en
	}
	return nil
}

func percent(hit, stmts int) float64 { return 100 * float64(hit) / float64(stmts) }

// CheckCoverageManifest selects the manifest's applicable entries for goos
// (common entries and goos's own) from profile and requires each group's
// statement coverage, and each listed file's complete coverage, to be
// strictly above CoverageThreshold. Every entry is validated first; an
// applicable file absent from the profile or without statements, a range
// selecting no block, a group without applicable statements and exactly
// the threshold all fail. Duplicate entries count each block once.
func CheckCoverageManifest(goos, profile string, manifest []CoverageEntry) ([]GroupCoverage, error) {
	if goos != "linux" && goos != "darwin" {
		return nil, fmt.Errorf("devcheck: coverage manifest is evaluated on linux or darwin, not %q", goos)
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	files, err := parseProfile(profile)
	if err != nil {
		return nil, err
	}
	selected := map[string]map[*profileBlock]bool{}
	entries := map[string]int{}
	fileSets := map[string]map[string]bool{}
	checkedFiles := map[string]bool{}
	var failed []string
	for _, en := range manifest {
		if en.OS != "" && en.OS != goos {
			continue
		}
		blocks := files[en.File]
		if len(blocks) == 0 {
			return nil, fmt.Errorf("devcheck: coverage manifest file %s is absent from the coverage profile", en.File)
		}
		if selected[en.Group] == nil {
			selected[en.Group] = map[*profileBlock]bool{}
			fileSets[en.Group] = map[string]bool{}
		}
		entries[en.Group]++
		fileSets[en.Group][en.File] = true
		if !checkedFiles[en.File] {
			checkedFiles[en.File] = true
			stmts, hit := 0, 0
			for _, b := range blocks {
				stmts += b.stmts
				if b.hit {
					hit += b.stmts
				}
			}
			switch {
			case stmts == 0:
				failed = append(failed, fmt.Sprintf("file %s has no statements", en.File))
			case !(percent(hit, stmts) > CoverageThreshold):
				failed = append(failed, fmt.Sprintf("file %s %.1f%% (%d/%d statements)", en.File, percent(hit, stmts), hit, stmts))
			}
		}
		if len(en.Ranges) == 0 {
			for _, b := range blocks {
				selected[en.Group][b] = true
			}
			continue
		}
		for _, r := range en.Ranges {
			n := 0
			for _, b := range blocks {
				if b.startLine <= r[1] && b.endLine >= r[0] {
					selected[en.Group][b] = true
					n++
				}
			}
			if n == 0 {
				return nil, fmt.Errorf("devcheck: coverage manifest range %d-%d of %s selects no profile block", r[0], r[1], en.File)
			}
		}
	}
	var out []GroupCoverage
	for _, g := range coverageGroups {
		gc := GroupCoverage{Group: g, Files: len(fileSets[g]), SelectedEntryCount: entries[g]}
		for b := range selected[g] {
			gc.Blocks++
			gc.Statements += b.stmts
			if b.hit {
				gc.Hit += b.stmts
			}
		}
		if gc.Statements == 0 {
			return out, fmt.Errorf("devcheck: coverage manifest group %s selects no statements on %s", g, goos)
		}
		gc.Percent = percent(gc.Hit, gc.Statements)
		out = append(out, gc)
		if !(gc.Percent > CoverageThreshold) {
			failed = append(failed, fmt.Sprintf("group %s %.1f%% (%d/%d statements)", g, gc.Percent, gc.Hit, gc.Statements))
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return out, fmt.Errorf("devcheck: new/changed coverage is not greater than %.1f%%: %s", CoverageThreshold, strings.Join(failed, "; "))
	}
	return out, nil
}
