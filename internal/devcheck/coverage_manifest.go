package devcheck

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Iteration 09a coverage policy: besides the project-wide gate, the
// statement-weighted coverage of the new and changed production code of
// the workspace hub must be strictly above CoverageThreshold, computed
// independently for two groups from the same generated profile: the new
// internal/workspace package, and the union of every other new or changed
// production block. The manifest (coverage_manifest_data.go) is committed:
// CI reads no git base and needs no network. A new file lists all of its
// blocks; a changed existing file lists its changed executable line
// ranges (the new side of the diff). Reviewers compare it with the
// implementation diff.

// Coverage manifest groups.
const (
	GroupWorkspace = "workspace"
	GroupChanged   = "changed"
)

// CoverageEntry selects profile blocks of one file: all of them when
// Ranges is empty, else every block overlapping one of the inclusive line
// ranges.
type CoverageEntry struct {
	Group  string
	File   string
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

// CheckCoverageManifest selects the manifest's blocks from profile and
// requires each group's statement coverage to be strictly above
// CoverageThreshold. A file absent from the profile, a range selecting no
// block, an empty group and exactly the threshold all fail.
func CheckCoverageManifest(profile string, manifest []CoverageEntry) ([]GroupCoverage, error) {
	files, err := parseProfile(profile)
	if err != nil {
		return nil, err
	}
	selected := map[string]map[*profileBlock]bool{}
	entries := map[string]int{}
	fileSets := map[string]map[string]bool{}
	for _, en := range manifest {
		if en.Group != GroupWorkspace && en.Group != GroupChanged {
			return nil, fmt.Errorf("devcheck: coverage manifest entry %s has unknown group %q", en.File, en.Group)
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
		if len(en.Ranges) == 0 {
			for _, b := range blocks {
				selected[en.Group][b] = true
			}
			continue
		}
		for _, r := range en.Ranges {
			if r[0] < 1 || r[1] < r[0] {
				return nil, fmt.Errorf("devcheck: coverage manifest range %d-%d of %s is invalid", r[0], r[1], en.File)
			}
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
	var failed []string
	for _, g := range []string{GroupWorkspace, GroupChanged} {
		gc := GroupCoverage{Group: g, Files: len(fileSets[g]), SelectedEntryCount: entries[g]}
		for b := range selected[g] {
			gc.Blocks++
			gc.Statements += b.stmts
			if b.hit {
				gc.Hit += b.stmts
			}
		}
		if gc.Statements == 0 {
			return out, fmt.Errorf("devcheck: coverage manifest group %s selects no statements", g)
		}
		gc.Percent = 100 * float64(gc.Hit) / float64(gc.Statements)
		out = append(out, gc)
		if !(gc.Percent > CoverageThreshold) {
			failed = append(failed, fmt.Sprintf("%s %.1f%% (%d/%d statements)", g, gc.Percent, gc.Hit, gc.Statements))
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return out, fmt.Errorf("devcheck: new/changed coverage is not greater than %.1f%%: %s", CoverageThreshold, strings.Join(failed, "; "))
	}
	return out, nil
}
