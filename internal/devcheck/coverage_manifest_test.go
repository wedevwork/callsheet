package devcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

const (
	wsFile  = "github.com/wedevwork/callsheet/internal/workspace/a.go"
	chgFile = "github.com/wedevwork/callsheet/internal/plane/b.go"
)

// block renders one profile line.
func block(file string, start, end, stmts, count int) string {
	return fmt.Sprintf("%s:%d.1,%d.9 %d %d\n", file, start, end, stmts, count)
}

// Iteration 09a coverage policy: whole-file and range selection, one count
// per duplicated block (covered if any binary covered it), statement
// weighting, a strict threshold per group, and failures for missing files,
// empty ranges, empty groups and malformed profiles.
func TestCoverageManifest(t *testing.T) {
	manifest := []CoverageEntry{
		{Group: GroupWorkspace, File: wsFile},
		{Group: GroupChanged, File: chgFile, Ranges: [][2]int{{10, 20}}},
	}
	// 9 of 10 workspace statements; the changed range selects the blocks
	// overlapping 10-20 only (the 30-40 block is ignored).
	profile := "mode: atomic\n" +
		block(wsFile, 1, 3, 9, 0) + block(wsFile, 1, 3, 9, 4) + // duplicate: counted once, covered
		block(wsFile, 5, 6, 1, 0) +
		block(chgFile, 8, 12, 5, 1) + block(chgFile, 18, 25, 4, 1) + block(chgFile, 15, 16, 1, 0) + block(chgFile, 30, 40, 100, 0)
	got, err := CheckCoverageManifest(profile, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Statements != 10 || got[0].Hit != 9 || got[0].Blocks != 2 || got[1].Statements != 10 || got[1].Hit != 9 || got[1].Blocks != 3 {
		t.Fatalf("groups %+v", got)
	}
	// Exactly 80% fails; just above passes.
	eighty := "mode: atomic\n" + block(wsFile, 1, 1, 8, 1) + block(wsFile, 2, 2, 2, 0) + block(chgFile, 10, 10, 1, 1)
	if _, err := CheckCoverageManifest(eighty, manifest); err == nil || !strings.Contains(err.Error(), "workspace 80.0%") {
		t.Fatalf("exactly 80%%: %v", err)
	}
	above := "mode: atomic\n" + block(wsFile, 1, 1, 801, 1) + block(wsFile, 2, 2, 199, 0) + block(chgFile, 10, 10, 1, 1)
	if _, err := CheckCoverageManifest(above, manifest); err != nil {
		t.Fatalf("80.1%%: %v", err)
	}
	for name, c := range map[string]struct {
		profile  string
		manifest []CoverageEntry
		want     string
	}{
		"missing file":   {"mode: atomic\n" + block(wsFile, 1, 1, 1, 1), manifest, "absent from the coverage profile"},
		"empty range":    {"mode: atomic\n" + block(wsFile, 1, 1, 1, 1) + block(chgFile, 50, 60, 1, 1), manifest, "selects no profile block"},
		"empty group":    {"mode: atomic\n" + block(wsFile, 1, 1, 1, 1), manifest[:1], "group changed selects no statements"},
		"no mode":        {block(wsFile, 1, 1, 1, 1), manifest, "no mode line"},
		"malformed":      {"mode: atomic\n" + wsFile + ":x 1 1\n", manifest, "malformed"},
		"unknown group":  {profile, []CoverageEntry{{Group: "other", File: wsFile}}, "unknown group"},
		"inverted range": {profile, []CoverageEntry{{Group: GroupChanged, File: chgFile, Ranges: [][2]int{{20, 10}}}}, "is invalid"},
		"zero stmts":     {"mode: atomic\n" + block(wsFile, 1, 1, 0, 1) + block(chgFile, 10, 10, 1, 1), manifest, "group workspace selects no statements"},
	} {
		if _, err := CheckCoverageManifest(c.profile, c.manifest); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The coverage stage fails when the manifest's coverage is not above the
// bar, even with a passing project-wide total, and reports both groups.
func TestCoverageStageManifest(t *testing.T) {
	f := &fakeRunner{coverTotal: "90.0%", cmdList: cmdList, profile: strings.Replace(goodProfile, " 1 1\n", " 1 0\n", -1)}
	code, out, errOut := runDriver(t, "linux", f, "coverage")
	if code != 1 || !strings.Contains(errOut, "new/changed coverage is not greater than 80.0%") {
		t.Fatalf("uncovered manifest = %d %s", code, errOut)
	}
	os.RemoveAll(scratchFrom(out))
	f = &fakeRunner{coverTotal: "90.0%", cmdList: cmdList, profile: goodProfile}
	code, out, errOut = runDriver(t, "linux", f, "coverage")
	if code != 0 || !strings.Contains(out, "devcheck: new/changed coverage workspace 100.0%") || !strings.Contains(out, "devcheck: new/changed coverage changed 100.0%") {
		t.Fatalf("covered manifest = %d %s %s", code, out, errOut)
	}
}

// The committed manifest lists every production file of the new
// workspace package (all blocks) and only real repository files.
func TestCoverageManifestFiles(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	var listed []string
	for _, e := range WorkspaceCoverageManifest {
		rel := strings.TrimPrefix(e.File, modulePath+"/")
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("manifest file %s: %v", e.File, err)
		}
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/testkit/") {
			t.Fatalf("manifest lists non-production %s", rel)
		}
		listed = append(listed, rel)
	}
	files, _ := filepath.Glob(filepath.Join(root, "internal", "workspace", "*.go"))
	for _, f := range files {
		rel := "internal/workspace/" + filepath.Base(f)
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		if !slices.Contains(listed, rel) {
			t.Fatalf("new production file %s is not in the coverage manifest", rel)
		}
	}
}
