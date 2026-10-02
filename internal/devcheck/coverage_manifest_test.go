package devcheck

import (
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/token"
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
	trFile  = "github.com/wedevwork/callsheet/internal/workspacetransfer/c.go"
	linFile = "github.com/wedevwork/callsheet/internal/workspacetransfer/publish_linux.go"
	darFile = "github.com/wedevwork/callsheet/internal/workspacetransfer/publish_darwin.go"
)

// block renders one profile line.
func block(file string, start, end, stmts, count int) string {
	return fmt.Sprintf("%s:%d.1,%d.9 %d %d\n", file, start, end, stmts, count)
}

// baseManifest has one entry per group plus a native-only file per OS.
func baseManifest() []CoverageEntry {
	return []CoverageEntry{
		{Group: GroupWorkspace, File: wsFile},
		{Group: GroupChanged, File: chgFile, Ranges: [][2]int{{10, 20}}},
		{Group: GroupTransfer, File: trFile},
		{Group: GroupTransfer, File: linFile, OS: "linux"},
		{Group: GroupTransfer, File: darFile, OS: "darwin"},
	}
}

// Iteration 09a coverage policy with iteration 09b's per-file checks,
// OS applicability and the transfer group: whole-file and legacy range
// selection, one count per duplicated block, statement weighting, strict
// thresholds per group and per file.
func TestCoverageManifest(t *testing.T) {
	m := baseManifest()
	// Workspace 9/10; changed range 10-20 selects 9/10 of its blocks while
	// the whole file is 90/100... (the 30-40 block counts for the file).
	profile := "mode: atomic\n" +
		block(wsFile, 1, 3, 9, 0) + block(wsFile, 1, 3, 9, 4) + // duplicate: counted once, covered
		block(wsFile, 5, 6, 1, 0) +
		block(chgFile, 8, 12, 5, 1) + block(chgFile, 18, 25, 4, 1) + block(chgFile, 15, 16, 1, 0) + block(chgFile, 30, 40, 90, 1) +
		block(trFile, 1, 2, 9, 1) + block(trFile, 3, 3, 1, 0) +
		block(linFile, 1, 1, 5, 1) + block(darFile, 1, 1, 5, 1)
	for _, goos := range []string{"linux", "darwin"} {
		got, err := CheckCoverageManifest(goos, profile, m)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if len(got) != 3 || got[0].Group != GroupWorkspace || got[0].Statements != 10 || got[0].Hit != 9 || got[0].Blocks != 2 ||
			got[1].Group != GroupChanged || got[1].Statements != 10 || got[1].Hit != 9 || got[1].Blocks != 3 ||
			got[2].Group != GroupTransfer || got[2].Statements != 15 || got[2].Hit != 14 || got[2].Files != 2 {
			t.Fatalf("%s groups %+v", goos, got)
		}
	}
	// A native-only file absent from the other host's profile is skipped
	// there, never counted; on its own host it is required.
	noDarwin := strings.Replace(profile, block(darFile, 1, 1, 5, 1), "", 1)
	if _, err := CheckCoverageManifest("linux", noDarwin, m); err != nil {
		t.Fatalf("linux without the darwin file: %v", err)
	}
	if _, err := CheckCoverageManifest("darwin", noDarwin, m); err == nil || !strings.Contains(err.Error(), "publish_darwin.go is absent") {
		t.Fatalf("darwin without its file: %v", err)
	}
	// A native-only file below the bar fails only on its host.
	lowLinux := strings.Replace(profile, block(linFile, 1, 1, 5, 1), block(linFile, 1, 1, 5, 0)+block(linFile, 2, 2, 50, 1), 1)
	if _, err := CheckCoverageManifest("darwin", lowLinux, m); err != nil {
		t.Fatalf("darwin with a low linux file: %v", err)
	}
	if _, err := CheckCoverageManifest("linux", lowLinux, m); err != nil {
		t.Fatalf("linux low file at 50/55: %v", err)
	}
	zeroLinux := strings.Replace(profile, block(linFile, 1, 1, 5, 1), block(linFile, 1, 1, 5, 0), 1)
	if _, err := CheckCoverageManifest("linux", zeroLinux, m); err == nil || !strings.Contains(err.Error(), "file "+linFile+" 0.0%") {
		t.Fatalf("uncovered linux file: %v", err)
	}
	// Per-file checks use every block of a listed file whatever its legacy
	// ranges select: an uncovered block outside the range fails the file.
	outside := strings.Replace(profile, block(chgFile, 30, 40, 90, 1), block(chgFile, 30, 40, 90, 0), 1)
	if _, err := CheckCoverageManifest("linux", outside, m); err == nil || !strings.Contains(err.Error(), "file "+chgFile+" 9.0%") ||
		strings.Contains(err.Error(), "group changed") {
		t.Fatalf("file below the bar outside the range: %v", err)
	}
	// Exactly 80% fails for a file and for a group; just above passes.
	eighty := "mode: atomic\n" + block(wsFile, 1, 1, 8, 1) + block(wsFile, 2, 2, 2, 0) + block(chgFile, 10, 10, 1, 1) +
		block(trFile, 1, 1, 1, 1) + block(linFile, 1, 1, 1, 1) + block(darFile, 1, 1, 1, 1)
	if _, err := CheckCoverageManifest("linux", eighty, m); err == nil || !strings.Contains(err.Error(), "group workspace 80.0%") ||
		!strings.Contains(err.Error(), "file "+wsFile+" 80.0%") {
		t.Fatalf("exactly 80%%: %v", err)
	}
	above := strings.Replace(eighty, block(wsFile, 1, 1, 8, 1)+block(wsFile, 2, 2, 2, 0), block(wsFile, 1, 1, 801, 1)+block(wsFile, 2, 2, 199, 0), 1)
	if _, err := CheckCoverageManifest("linux", above, m); err != nil {
		t.Fatalf("80.1%%: %v", err)
	}
	// Duplicate entries add nothing.
	dup := append(baseManifest(), CoverageEntry{Group: GroupWorkspace, File: wsFile}, CoverageEntry{Group: GroupChanged, File: chgFile})
	got, err := CheckCoverageManifest("linux", profile, dup)
	if err != nil || got[0].Statements != 10 || got[0].Hit != 9 || got[1].Statements != 100 {
		t.Fatalf("duplicates %+v %v", got, err)
	}
	for name, c := range map[string]struct {
		goos     string
		profile  string
		manifest []CoverageEntry
		want     string
	}{
		"missing file":     {"linux", "mode: atomic\n" + block(wsFile, 1, 1, 1, 1), m, "absent from the coverage profile"},
		"empty range":      {"linux", strings.Replace(profile, block(chgFile, 8, 12, 5, 1)+block(chgFile, 18, 25, 4, 1)+block(chgFile, 15, 16, 1, 0), "", 1), m, "selects no profile block"},
		"no transfer":      {"linux", profile, m[:2], "group workspacetransfer selects no statements"},
		"no mode":          {"linux", block(wsFile, 1, 1, 1, 1), m, "no mode line"},
		"malformed":        {"linux", "mode: atomic\n" + wsFile + ":x 1 1\n", m, "malformed"},
		"unknown group":    {"linux", profile, append(baseManifest(), CoverageEntry{Group: "other", File: wsFile}), "unknown group"},
		"unknown OS":       {"linux", profile, append(baseManifest(), CoverageEntry{Group: GroupTransfer, File: trFile, OS: "windows"}), "unknown OS"},
		"unknown OS other": {"darwin", profile, append(baseManifest(), CoverageEntry{Group: GroupTransfer, File: linFile, OS: "Linux"}), "unknown OS"},
		"conflicting OS":   {"linux", profile, append(baseManifest(), CoverageEntry{Group: GroupTransfer, File: linFile}), "conflicting OS or group"},
		"conflicting grp":  {"linux", profile, append(baseManifest(), CoverageEntry{Group: GroupChanged, File: trFile}), "conflicting OS or group"},
		"test file":        {"linux", profile, append(baseManifest(), CoverageEntry{Group: GroupChanged, File: modulePath + "/x_test.go"}), "not a production Go file"},
		"foreign path":     {"linux", profile, append(baseManifest(), CoverageEntry{Group: GroupChanged, File: "example.com/x.go"}), "not a production Go file"},
		"inverted range":   {"linux", profile, append(baseManifest(), CoverageEntry{Group: GroupChanged, File: chgFile, Ranges: [][2]int{{20, 10}}}), "is invalid"},
		"zero stmts":       {"linux", strings.Replace(profile, block(trFile, 3, 3, 1, 0), "", 1) + block(modulePath+"/z.go", 1, 1, 0, 1), append(baseManifest(), CoverageEntry{Group: GroupTransfer, File: modulePath + "/z.go"}), "has no statements"},
		"unsupported goos": {"windows", profile, m, "evaluated on linux or darwin"},
	} {
		if _, err := CheckCoverageManifest(c.goos, c.profile, c.manifest); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Diagnostics are sorted.
	bad := strings.Replace(strings.Replace(profile, block(trFile, 1, 2, 9, 1), block(trFile, 1, 2, 9, 0), 1), block(wsFile, 1, 3, 9, 4), "", 1)
	_, err = CheckCoverageManifest("linux", bad, m)
	msg := fmt.Sprint(err)
	i, j := strings.Index(msg, "file "+trFile), strings.Index(msg, "group workspace")
	if err == nil || i < 0 || j < 0 || i > j {
		t.Fatalf("sorted diagnostics: %v", err)
	}
}

// The coverage stage fails when the manifest's coverage is not above the
// bar, even with a passing project-wide total, and reports every group.
func TestCoverageStageManifest(t *testing.T) {
	f := &fakeRunner{coverTotal: "90.0%", cmdList: cmdList, profile: strings.Replace(goodProfile, " 1 1\n", " 1 0\n", -1)}
	code, out, errOut := runDriver(t, "linux", f, "coverage")
	if code != 1 || !strings.Contains(errOut, "new/changed coverage is not greater than 80.0%") {
		t.Fatalf("uncovered manifest = %d %s", code, errOut)
	}
	os.RemoveAll(scratchFrom(out))
	for _, goos := range []string{"linux", "darwin"} {
		f = &fakeRunner{coverTotal: "90.0%", cmdList: cmdList, profile: goodProfile}
		code, out, errOut = runDriver(t, goos, f, "coverage")
		if code != 0 || !strings.Contains(out, "devcheck: new/changed coverage workspace 100.0%") || !strings.Contains(out, "devcheck: new/changed coverage changed 100.0%") ||
			!strings.Contains(out, "devcheck: new/changed coverage workspacetransfer 100.0%") {
			t.Fatalf("%s covered manifest = %d %s %s", goos, code, out, errOut)
		}
		os.RemoveAll(scratchFrom(out))
	}
}

// buildOS returns the OS a production file is restricted to by its
// //go:build expression ("" when it builds on both linux and darwin).
func buildOS(t *testing.T, path string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	var expr constraint.Expr
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				expr, _ = constraint.Parse(c.Text)
			}
		}
	}
	if expr == nil {
		return ""
	}
	linux := expr.Eval(func(tag string) bool { return tag == "linux" || tag == "unix" })
	darwin := expr.Eval(func(tag string) bool { return tag == "darwin" || tag == "unix" })
	switch {
	case linux && darwin:
		return ""
	case linux:
		return "linux"
	case darwin:
		return "darwin"
	}
	t.Fatalf("%s builds on neither linux nor darwin", path)
	return ""
}

// The committed manifest lists every production file of the workspace
// and transfer packages (all blocks), only real repository production
// files, and each file's OS matches its build constraint.
func TestCoverageManifestFiles(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	listed := map[string]CoverageEntry{}
	for _, e := range WorkspaceCoverageManifest {
		rel := strings.TrimPrefix(e.File, modulePath+"/")
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("manifest file %s: %v", e.File, err)
		}
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/testkit/") {
			t.Fatalf("manifest lists non-production %s", rel)
		}
		if os := buildOS(t, filepath.Join(root, filepath.FromSlash(rel))); os != e.OS {
			t.Fatalf("%s: manifest OS %q, build constraint %q", rel, e.OS, os)
		}
		listed[rel] = e
	}
	if err := validateManifest(WorkspaceCoverageManifest); err != nil {
		t.Fatal(err)
	}
	for dir, group := range map[string]string{"workspace": GroupWorkspace, "workspacetransfer": GroupTransfer} {
		files, _ := filepath.Glob(filepath.Join(root, "internal", dir, "*.go"))
		n := 0
		for _, f := range files {
			rel := "internal/" + dir + "/" + filepath.Base(f)
			if strings.HasSuffix(rel, "_test.go") {
				continue
			}
			n++
			e, ok := listed[rel]
			if !ok || e.Group != group || len(e.Ranges) != 0 {
				t.Fatalf("production file %s is not in the coverage manifest's %s group as a whole file", rel, group)
			}
		}
		if n == 0 {
			t.Fatalf("no production files in internal/%s", dir)
		}
	}
	for _, rel := range []string{"internal/contract/workspace_transfer.go", "internal/client/workspace_git.go", "internal/cli/workspace_transfer.go", "internal/mcp/transfer.go"} {
		if e, ok := listed[rel]; !ok || e.Group != GroupChanged || len(e.Ranges) != 0 {
			t.Fatalf("09b file %s missing from the changed group", rel)
		}
	}
	if !slices.ContainsFunc(WorkspaceCoverageManifest, func(e CoverageEntry) bool { return e.OS == "linux" }) ||
		!slices.ContainsFunc(WorkspaceCoverageManifest, func(e CoverageEntry) bool { return e.OS == "darwin" }) {
		t.Fatal("native-only publication files missing")
	}
}
