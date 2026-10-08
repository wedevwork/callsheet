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
		// m3-m4-container-e2e: the container acceptance helper package is the
		// one test-support package whose executable logic the design
		// requires in the manifest; every other testkit path stays out.
		if strings.HasSuffix(rel, "_test.go") || (strings.HasPrefix(rel, "internal/testkit/") && !strings.HasPrefix(rel, "internal/testkit/containeracceptance/")) {
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
	for rel, goos := range map[string]string{"internal/sidecar/group_alone_linux.go": "linux", "internal/sidecar/group_alone_darwin.go": "darwin",
		"internal/sidecar/guardian.go": "", "internal/sidecar/session.go": "", "internal/sidecar/task_process_unix.go": "", "internal/sidecar/tasks.go": ""} {
		if e, ok := listed[rel]; !ok || e.Group != GroupChanged || len(e.Ranges) != 0 || e.OS != goos {
			t.Fatalf("10a file %s missing from the changed group as a whole file (OS %q)", rel, goos)
		}
	}
	// Iteration 10b (UT-B9): both new packages' production files whole in
	// the changed group, and every other new or changed production file
	// whole in its group with its build OS; no 10b file keeps an older
	// partial-range entry.
	tenB := map[string]string{
		"internal/workspace/tasks.go": GroupWorkspace, "internal/workspacetransfer/task.go": GroupTransfer,
		"internal/workspacetransfer/task_snapshot.go": GroupTransfer,
	}
	for _, dir := range []string{"taskworkspace", "taskpublication"} {
		files, _ := filepath.Glob(filepath.Join(root, "internal", dir, "*.go"))
		n := 0
		for _, f := range files {
			if !strings.HasSuffix(f, "_test.go") {
				tenB["internal/"+dir+"/"+filepath.Base(f)] = GroupChanged
				n++
			}
		}
		if n == 0 {
			t.Fatalf("no production files in internal/%s", dir)
		}
	}
	for _, rel := range []string{"internal/contract/contract.go", "internal/contract/control.go", "internal/contract/frame.go",
		"internal/contract/journal.go", "internal/contract/journal_workspace.go", "internal/contract/node.go", "internal/contract/task.go",
		"internal/contract/task_record.go", "internal/contract/task_view.go", "internal/contract/task_workspace.go",
		"internal/adapter/task.go", "internal/adapter/vendor.go", "internal/client/client.go", "internal/client/node_workspace.go",
		"internal/client/workspace_git.go", "internal/plane/node_stream.go", "internal/plane/nodes.go", "internal/plane/server.go",
		"internal/plane/task_api.go", "internal/plane/task_controls.go", "internal/plane/task_recon.go", "internal/plane/task_workspace.go",
		"internal/plane/task_writer.go", "internal/plane/tasks.go", "internal/sidecar/run.go", "internal/sidecar/session.go",
		"internal/sidecar/session_tasks.go", "internal/sidecar/sidecar.go", "internal/sidecar/state.go", "internal/sidecar/task_journal.go",
		"internal/sidecar/task_platform.go", "internal/sidecar/task_recovery.go", "internal/sidecar/task_workspace.go", "internal/sidecar/task_storage.go",
		"internal/sidecar/tasks.go", "internal/devcheck/devcheck.go", "internal/devcheck/native.go", "internal/devcheck/stress.go"} {
		tenB[rel] = GroupChanged
	}
	for rel, group := range map[string]string{"internal/workspace/manager.go": GroupWorkspace, "internal/workspace/prune.go": GroupWorkspace,
		"internal/workspace/repo.go": GroupWorkspace, "internal/workspacetransfer/export.go": GroupTransfer,
		"internal/workspacetransfer/fsutil.go": GroupTransfer, "internal/workspacetransfer/objects.go": GroupTransfer,
		"internal/workspacetransfer/publish_darwin.go": GroupTransfer, "internal/workspacetransfer/publish_linux.go": GroupTransfer} {
		tenB[rel] = group
	}
	for rel, group := range tenB {
		e, ok := listed[rel]
		if !ok || e.Group != group || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("10b file %s missing from the %s group with its build OS", rel, group)
		}
		for _, x := range WorkspaceCoverageManifest {
			if x.File == modulePath+"/"+rel && len(x.Ranges) != 0 {
				t.Fatalf("10b file %s keeps a partial-range entry %v", rel, x.Ranges)
			}
		}
	}
	// Iteration 10c (UT-C6): every production file it changes, whole in its
	// group with its build OS, and none keeps an older partial-range entry.
	tenC := map[string]string{"internal/workspacetransfer/transfer.go": GroupTransfer}
	for _, rel := range []string{"internal/contract/task_workspace.go", "internal/client/tasks.go", "internal/cli/task.go", "internal/cli/workspace.go",
		"internal/cli/workspace_transfer.go", "internal/mcp/schemas.go", "internal/mcp/tools.go", "internal/mcp/transfer.go", "internal/plane/task_api.go",
		"internal/plane/task_workspace.go", "internal/devcheck/native.go"} {
		tenC[rel] = GroupChanged
	}
	for rel, group := range tenC {
		e, ok := listed[rel]
		if !ok || e.Group != group || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("10c file %s missing from the %s group with its build OS", rel, group)
		}
		for _, x := range WorkspaceCoverageManifest {
			if x.File == modulePath+"/"+rel && len(x.Ranges) != 0 {
				t.Fatalf("10c file %s keeps a partial-range entry %v", rel, x.Ranges)
			}
		}
	}
	if !slices.ContainsFunc(WorkspaceCoverageManifest, func(e CoverageEntry) bool { return e.OS == "linux" }) ||
		!slices.ContainsFunc(WorkspaceCoverageManifest, func(e CoverageEntry) bool { return e.OS == "darwin" }) {
		t.Fatal("native-only publication files missing")
	}
	// m3-m4-container-e2e: every non-test file of the acceptance helper
	// package, the new driver and evidence files and the changed workflow
	// validator, whole in the changed group with its build OS.
	m3m4 := []string{"internal/devcheck/container_e2e.go", "internal/devcheck/container_expected.go", "internal/devcheck/container_report.go",
		"internal/cicheck/workflow.go"}
	helpers, _ := filepath.Glob(filepath.Join(root, "internal", "testkit", "containeracceptance", "*.go"))
	n := 0
	for _, f := range helpers {
		if !strings.HasSuffix(f, "_test.go") {
			m3m4 = append(m3m4, "internal/testkit/containeracceptance/"+filepath.Base(f))
			n++
		}
	}
	if n == 0 {
		t.Fatal("no production files in internal/testkit/containeracceptance")
	}
	for _, rel := range m3m4 {
		e, ok := listed[rel]
		if !ok || e.Group != GroupChanged || len(e.Ranges) != 0 || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("m3-m4 file %s missing from the changed group as a whole file", rel)
		}
	}
	// Non-blocking coordinator waits: every new or changed production file,
	// whole in the changed group with its build OS, none keeping an older
	// partial-range entry.
	for _, rel := range []string{"internal/client/until_done.go", "internal/cli/task.go", "internal/cli/mcp.go", "internal/mcp/wait.go", "internal/mcp/tools.go",
		"internal/mcpqual/facts.go", "internal/mcpqual/plan.go", "internal/mcpqual/publish.go", "internal/mcpqual/report.go", "internal/devcheck/devcheck.go", "internal/devcheck/native.go",
		"internal/devcheck/container_expected.go", "internal/testkit/containeracceptance/record.go", "internal/testkit/containeracceptance/scenarios.go",
		"internal/testkit/containeracceptance/suite.go", "internal/testkit/containeracceptance/synthetic.go"} {
		e, ok := listed[rel]
		if !ok || e.Group != GroupChanged || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("non-blocking waits file %s missing from the changed group with its build OS", rel)
		}
		for _, x := range WorkspaceCoverageManifest {
			if x.File == modulePath+"/"+rel && len(x.Ranges) != 0 {
				t.Fatalf("non-blocking waits file %s keeps a partial-range entry %v", rel, x.Ranges)
			}
		}
	}
}

// decoderEnrollmentFiles are the production files decoder enrollment's
// slice A adds or changes (design decoder-enrollment, CI plan), each
// required whole in the changed group.
var decoderEnrollmentFiles = []string{"internal/mcpqual/cli.go", "internal/mcpqual/plan.go", "internal/mcpqual/session.go", "internal/mcpqual/runner.go",
	"internal/mcpqual/proc.go", "internal/mcpqual/report.go", "internal/mcpqual/facts.go", "internal/mcpqual/decode.go", "internal/mcpqual/capture.go",
	"internal/mcpqual/capture_manifest.go", "internal/mcpqual/enrollment.go", "internal/mcpqual/measure.go", "internal/mcpqual/redact.go",
	"internal/mcpqual/procexec/exec_unix.go", "cmd/mcpqual/main.go", "internal/devcheck/native.go", "internal/devcheck/devcheck.go"}

// TestDecoderEnrollmentCoverageManifest (design decoder-enrollment, UT-9):
// every new or changed production file is a whole-file changed-group entry
// with its build OS, and none keeps a partial-range entry.
func TestDecoderEnrollmentCoverageManifest(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	listed := map[string]CoverageEntry{}
	for _, e := range WorkspaceCoverageManifest {
		listed[strings.TrimPrefix(e.File, modulePath+"/")] = e
	}
	for _, rel := range decoderEnrollmentFiles {
		e, ok := listed[rel]
		if !ok || e.Group != GroupChanged || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("decoder-enrollment file %s missing from the changed group with its build OS", rel)
		}
		for _, x := range WorkspaceCoverageManifest {
			if x.File == modulePath+"/"+rel && len(x.Ranges) != 0 {
				t.Fatalf("decoder-enrollment file %s keeps a partial-range entry %v", rel, x.Ranges)
			}
		}
	}
}

// decoderEnrollmentB1Files are the production files decoder enrollment's
// slice B1 adds or changes (design decoder-enrollment B1, CI plan, against
// the actual diff), each required whole in the changed group.
var decoderEnrollmentB1Files = []string{"internal/mcpqual/probe.go", "internal/mcpqual/events.go", "internal/mcpqual/plan.go", "internal/mcpqual/session.go",
	"internal/mcpqual/capture.go", "internal/mcpqual/capture_approval.go", "internal/mcpqual/capture_approval_unix.go", "internal/mcpqual/capture_manifest.go",
	"internal/mcpqual/cli.go", "internal/devcheck/native.go"}

// TestDecoderEnrollmentB1CoverageManifest (design decoder-enrollment B1,
// CI plan): every B1 production file is a whole-file changed-group entry
// with its build OS, and none keeps a partial-range entry.
func TestDecoderEnrollmentB1CoverageManifest(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	listed := map[string]CoverageEntry{}
	for _, e := range WorkspaceCoverageManifest {
		listed[strings.TrimPrefix(e.File, modulePath+"/")] = e
	}
	for _, rel := range decoderEnrollmentB1Files {
		e, ok := listed[rel]
		if !ok || e.Group != GroupChanged || len(e.Ranges) != 0 || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("decoder-enrollment B1 file %s missing from the changed group with its build OS", rel)
		}
	}
}

// decoderEnrollmentB15Files are the production files decoder enrollment's
// slice B1.5 adds or changes (design decoder-enrollment B1.5, CI plan,
// against the actual diff), each required whole in the changed group.
var decoderEnrollmentB15Files = []string{"internal/mcpqual/probe_observation.go", "internal/mcpqual/capture.go", "internal/mcpqual/session.go",
	"internal/mcpqual/capture_manifest.go", "internal/mcpqual/capture_approval.go", "internal/mcpqual/enrollment.go", "internal/mcpqual/report.go",
	"internal/mcpqual/runner.go", "internal/devcheck/native.go"}

// TestDecoderEnrollmentB15CoverageManifest (design decoder-enrollment
// B1.5, CI plan): every B1.5 production file is a whole-file changed-group
// entry with its build OS, and none keeps a partial-range entry.
func TestDecoderEnrollmentB15CoverageManifest(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	listed := map[string]CoverageEntry{}
	for _, e := range WorkspaceCoverageManifest {
		listed[strings.TrimPrefix(e.File, modulePath+"/")] = e
	}
	for _, rel := range decoderEnrollmentB15Files {
		e, ok := listed[rel]
		if !ok || e.Group != GroupChanged || len(e.Ranges) != 0 || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("decoder-enrollment B1.5 file %s missing from the changed group with its build OS", rel)
		}
	}
}

// taskWorkspaceBudget is design 10b r0.2's Budgets entry, verbatim.
const taskWorkspaceBudget = "10b: place all new lifecycle matrices outside plane; add no plane stress cases or repeated selectors and no child-heavy sidecar matrix. " +
	"Use post-10a CPU1 CI ranges from runs 37095473701, 37090552825 and 37101472176: sidecar 155.0–173.2 s Linux / 189.0–252.5 s macOS and plane 144.2–213.9 s Linux / 229.9–257.0 s macOS. " +
	"Allocate sidecar binary growth of 20 s Linux / 30 s macOS for 10b, giving CPU1 planning targets of 193.2 s / 282.5 s from the observed maxima; reserve a further 30 s diagnostic variance envelope, giving 223.2 s / 312.5 s and leaving 136.8 s / 47.5 s below the unchanged 360 s binary timeout. " +
	"Plane has zero planned growth, with observed maxima 213.9 s / 257.0 s and a 30 s diagnostic envelope of 243.9 s / 287.0 s. " +
	"Other sidecar CPUs retain the 20 s / 30 s incremental allowance against matched post-10a runs; CPU1 measurements do not establish their baselines. " +
	"These are planning and investigation thresholds, not comparative wall-clock acceptance gates; one noisy run above a target is an allocation miss to investigate, not proof of a regression or permission to ignore a failure. " +
	"Record binary and command times separately; use matched alternating base/change observations to distinguish persistent growth from runner variance, retaining all results. " +
	"The 360 s timeout and all test assertions remain gates. Do not spend an estimated saving twice or weaken tests to meet an allocation. " +
	"Packages stage growth allowance is 20 s Linux / 30 s macOS; each new combined package binary is allocated 45 s Linux / 60 s macOS across all 60 repetitions. " +
	"Workspace per-CPU growth allowance is 5 s each; transfer combined growth allowance is 10 s. " +
	"Function binary growth allowance is 12 s Linux / 18 s macOS per normal/race/native run against 10a. New benchmark step allowance is 10 s Linux / 15 s macOS. " +
	"Unit coverage execution growth allowance is 20 s Linux / 30 s macOS per coverage command, reported separately from stress. " +
	"Preserve 18 jobs, four required checks and all existing timeouts. An allocation miss requires investigation and fixture reduction or design revision, never weaker tests."

// UT-B9 (iteration 10b): docs/ci.md carries the exact 10b budgets policy
// once, as a planning allocation in Budgets (before its measurements).
func TestTaskWorkspaceBudgets(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	at := strings.Index(s, taskWorkspaceBudget)
	budgets, measured := strings.Index(s, "\nBudgets:\n"), strings.Index(s, "\nMeasurements, newest first.")
	switch {
	case at < 0 || strings.Count(s, taskWorkspaceBudget) != 1:
		t.Fatal("docs/ci.md does not carry the 10b budgets entry exactly once")
	case budgets < 0 || measured < 0 || at < budgets || at > measured:
		t.Fatal("the 10b budgets entry is not a Budgets allocation before the measurements")
	case !strings.Contains(s[:at], "Iteration 10b allocation (design 10b r0.2 Budgets; planning allowances"):
		t.Fatal("the 10b budgets entry is not labelled as planning allowances")
	}
}

// taskDeliveryBudget is the parent design's 10c Budgets entry, verbatim.
const taskDeliveryBudget = "10c: plane/sidecar stress workload unchanged; packages growth ≤5 s Linux / 8 s macOS; function binary growth ≤8 s Linux / 12 s macOS. " +
	"No added benchmark step. Other shards: no new execution, ≤5 s shared compile overhead across the whole iteration; summaries unchanged. Preserve all timeouts. " +
	"Record binary and command times separately and compare matched runner/cache conditions. " +
	"An allocation miss requires investigation and fixture reduction or design revision, never weaker tests. " +
	"The owner specifically retains 18 jobs despite the historical 06b split trigger: no automatic expansion to 22 jobs in this iteration."

// UT-C6 (iteration 10c): docs/ci.md carries the exact 10c budgets policy
// once, as a planning allocation in Budgets (before its measurements), and
// Checks names the six 10c function tests and the 353-name inventory.
func TestTaskDeliveryBudgets(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	at := strings.Index(s, taskDeliveryBudget)
	budgets, measured := strings.Index(s, "\nBudgets:\n"), strings.Index(s, "\nMeasurements, newest first.")
	switch {
	case at < 0 || strings.Count(s, taskDeliveryBudget) != 1:
		t.Fatal("docs/ci.md does not carry the 10c budgets entry exactly once")
	case budgets < 0 || measured < 0 || at < budgets || at > measured:
		t.Fatal("the 10c budgets entry is not a Budgets allocation before the measurements")
	case !strings.Contains(s[:at], "Iteration 10c allocation (design 10c r0.4 CI plan and the parent design's\n  10c Budgets entry; planning allowances"):
		t.Fatal("the 10c budgets entry is not labelled as planning allowances")
	case !strings.Contains(s, "6 more names, 353 in all"):
		t.Fatal("docs/ci.md Checks does not count 353 native names")
	}
	for _, n := range wsDoorNames() {
		if !strings.Contains(s, "`"+n+"`") {
			t.Fatalf("docs/ci.md does not name %s", n)
		}
	}
}

// TestWave2NativeDocs (iteration 11): docs/ci.md's Checks names every
// wave-2 function name and tagged sidecar subtest with the new counts, and
// keeps every earlier count.
func TestWave2NativeDocs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	for _, w := range []string{"42 more names, 395 in all", "with the 353 earlier names unchanged and first", "(12\nnames in all)",
		"6 more names, 353 in all", "39 more names, 312 in all", "CI stays 18 jobs"} {
		if !strings.Contains(s, w) {
			t.Fatalf("docs/ci.md lacks %q", w)
		}
	}
	for _, n := range append(wave2Names(), realLocal[len(realLocal)-4:]...) {
		if !strings.Contains(s, "`"+n+"`") {
			t.Fatalf("docs/ci.md does not name %s", n)
		}
	}
	if len(NativeRequiredTests()) != 424 || len(NativeTaskProcessTests()) != 12 {
		t.Fatalf("%d native names, %d sidecar names", len(NativeRequiredTests()), len(NativeTaskProcessTests()))
	}
}

// nonblockingWaitBudget is design nonblocking-coordinator-waits r0.2's
// Budgets entry, verbatim.
const nonblockingWaitBudget = "Nonblocking waits: preserve 18 jobs, four required checks and every timeout. Against green main run 37415353383 attempt 2 (a09b711), main-job growth allowance is 20s Linux / 20s macOS (764→784s / 464→484s); " +
	"packages-job growth is 10s each (711→721s / 731→741s), combined packages command 433.9→443.9s / 455.3→465.3s. Client binary growth is 8s each (127.5→135.5s / 106.9→114.9s); " +
	"each mcpqual CPU binary and the MCP binary gets 2s. Workspacetransfer gets zero new stress work (308.9s Linux / 280.1s macOS against 360s limit). " +
	"Normal/race function binary growth is 5s each (Linux 110.9→115.9s / 126.6→131.6s); new benchmark execution allocation is 2s per platform. " +
	"The container-e2e iteration inside ci-linux test gets 2s growth (9.8→11.8s); its static binary build allocation is unchanged at 24.2s. " +
	"Separately, the internal/testkit/containeracceptance test binary in ci-linux gets 3s growth (94.7→97.7s). " +
	"Stress-function execution is unchanged (Linux function/plane/node 24.8/19.6/7.9s, macOS 30.6/46.7/17.2s). " +
	"Other shards get no execution growth and at most 5s shared compilation growth. Reserve a separate ±30s runner-variance envelope; do not spend it as test workload. " +
	"Compare binary, command and job times separately, retaining all first-run evidence. " +
	"An allocation miss requires investigation or design revision, never weakened assertions, skips, changed repetition counts or timeout increases."

// TestNonblockingWaitDocs (design nonblocking-coordinator-waits):
// docs/ci.md carries the exact Budgets entry once, as a labelled planning
// allocation before the measurements, and Checks names the eight function
// parents with the 403-name inventory, the client wait benchmark step and
// the container's background_wait subtest.
func TestNonblockingWaitDocs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	at := strings.Index(s, nonblockingWaitBudget)
	budgets, measured := strings.Index(s, "\nBudgets:\n"), strings.Index(s, "\nMeasurements, newest first.")
	switch {
	case at < 0 || strings.Count(s, nonblockingWaitBudget) != 1:
		t.Fatal("docs/ci.md does not carry the non-blocking waits budgets entry exactly once")
	case budgets < 0 || measured < 0 || at < budgets || at > measured:
		t.Fatal("the budgets entry is not a Budgets allocation before the measurements")
	case !strings.Contains(s[:at], "Non-blocking coordinator waits allocation (design\n  nonblocking-coordinator-waits r0.2 Budgets; planning allocations, not\n  measured deltas"):
		t.Fatal("the budgets entry is not labelled as planning allocations")
	}
	for _, w := range []string{"8 more names,\n403 in all", "with the 395 earlier names unchanged and first", "`bench client wait`", "(13 steps; Linux `all`\nmakes 32 ordinary calls)",
		"(8 ordinary calls)", "`background_wait` subtest", "CI stays 18 jobs", "the client wait benchmark step (non-blocking coordinator waits)", "`BenchmarkWaitUntilDone`"} {
		if !strings.Contains(s, w) {
			t.Fatalf("docs/ci.md lacks %q", w)
		}
	}
	for _, n := range nbwNames() {
		if !strings.Contains(s, "`"+n+"`") {
			t.Fatalf("docs/ci.md does not name %s", n)
		}
	}
}

// decoderEnrollmentBudget is design decoder-enrollment r0.2's Budgets
// entry, verbatim.
const decoderEnrollmentBudget = "Decoder enrollment: baseline is main run 37523901881 (ff8058f), as named by the coordinator. Supplied mcpqual per-CPU stress duration is approximately 60–85s per invocation; " +
	"allocate at most 5s additional test execution per invocation (planning envelope 65–90s, not a measured result), with unchanged 360s binary timeout. " +
	"Allocate 10s additional packages-job wall time per host, 15s main-job growth Linux and 20s macOS, 5s function-binary growth per native/race invocation and 5s new benchmark execution per host (macOS adds the mcpqual benchmark command). " +
	"Other shard execution and container workloads get zero growth; shared compilation allowance is 5s/job. Reserve a separate ±30s runner-variance envelope, not spendable test workload. " +
	"Keep all eighteen jobs, four required checks and existing watchdogs. Compare binary, command and job times separately against run 37523901881, retain failed first-run evidence, " +
	"and investigate an allocation miss without reducing counts, skipping cases, weakening assertions or increasing timeouts."

// TestDecoderEnrollmentDocs (design decoder-enrollment, UT-9): docs/ci.md
// carries the exact Budgets entry once, as a labelled planning allocation
// before the measurements, and Checks names the nine function parents with
// the 412-name inventory and the native mcpqual benchmark step.
func TestDecoderEnrollmentDocs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	at := strings.Index(s, decoderEnrollmentBudget)
	budgets, measured := strings.Index(s, "\nBudgets:\n"), strings.Index(s, "\nMeasurements, newest first.")
	switch {
	case at < 0 || strings.Count(s, decoderEnrollmentBudget) != 1:
		t.Fatal("docs/ci.md does not carry the decoder enrollment budgets entry exactly once")
	case budgets < 0 || measured < 0 || at < budgets || at > measured:
		t.Fatal("the budgets entry is not a Budgets allocation before the measurements")
	case !strings.Contains(s[:at], "Decoder enrollment allocation (design decoder-enrollment r0.2 Budgets;\n  planning allowances, not measured deltas or pass/fail timing gates"):
		t.Fatal("the budgets entry is not labelled as planning allowances")
	}
	for _, w := range []string{"9 more names, 412 in all", "with the 403 earlier names\nunchanged and first", "(9 ordinary calls)", "13 steps (Linux `all` still makes 32 ordinary calls)",
		"`BenchmarkMCPCaptureEvidence`", "CI stays 18 jobs"} {
		if !strings.Contains(s, w) {
			t.Fatalf("docs/ci.md lacks %q", w)
		}
	}
	for _, n := range dceNames() {
		if !strings.Contains(s, "`"+n+"`") {
			t.Fatalf("docs/ci.md does not name %s", n)
		}
	}
}

// TestDecoderEnrollmentB1Docs (design decoder-enrollment B1, CI plan):
// docs/ci.md's Checks names the four B1 function parents with the
// 416-name inventory, the unchanged native call, benchmark and twenty-job
// counts, and no new budget allocation.
func TestDecoderEnrollmentB1Docs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	for _, w := range []string{"4 more names, 416 in all", "with the 412 earlier names unchanged and first", "positions 403 to 412",
		"`devcheck native` still makes 9 ordinary calls", "`devcheck bench` keeps 13 steps", "The workflow keeps its twenty jobs", "no new budget allocation",
		"`stress-packages-cpu`"} {
		if !strings.Contains(strings.Join(strings.Fields(s), " "), w) {
			t.Fatalf("docs/ci.md lacks %q", w)
		}
	}
	for _, n := range b1Names() {
		if !strings.Contains(s, "`"+n+"`") {
			t.Fatalf("docs/ci.md does not name %s", n)
		}
	}
}

// TestDecoderEnrollmentB15Docs (design decoder-enrollment B1.5, CI plan):
// docs/ci.md's Checks names the three B1.5 function parents with the
// 419-name inventory and its ranges, the unchanged native call, benchmark,
// cross and twenty-job counts, the new whole-file entry and no new budget
// allocation or stress workload.
func TestDecoderEnrollmentB15Docs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(strings.Fields(string(doc)), " ")
	for _, w := range []string{"3 more names, 419 in all", "with the 416 earlier names unchanged and first", "the B1 names keep positions 412 to 416",
		"positions 416 to 419", "`devcheck native` still makes 9 ordinary calls", "`devcheck bench` keeps 13 steps", "cross keeps 12 artifacts",
		"Linux `all` still makes 32 ordinary calls", "`internal/mcpqual/probe_observation.go`", "The workflow keeps its twenty jobs", "B1.5 makes no new budget allocation",
		"no new stress selector or workload"} {
		if !strings.Contains(s, w) {
			t.Fatalf("docs/ci.md lacks %q", w)
		}
	}
	for _, n := range b15Names() {
		if !strings.Contains(s, "`"+n+"`") {
			t.Fatalf("docs/ci.md does not name %s", n)
		}
	}
}

// decoderEnrollmentB2Files are the production files decoder enrollment's
// slice B2 adds or changes (design decoder-enrollment B2, CI plan, against
// the actual diff), each required whole in the changed group.
var decoderEnrollmentB2Files = []string{"internal/mcpqual/decode.go", "internal/mcpqual/decode_real.go", "internal/mcpqual/enrollment.go",
	"internal/mcpqual/fixture_export.go", "internal/mcpqual/capture.go", "internal/mcpqual/capture_approval.go", "internal/mcpqual/capture_approval_unix.go",
	"internal/mcpqual/capture_manifest.go", "cmd/mcpfixture-export/main.go", "internal/devcheck/native.go"}

// TestDecoderEnrollmentB2CoverageManifest (design decoder-enrollment B2,
// CI plan): every B2 production file is a whole-file changed-group entry
// with its build OS, and none keeps a partial-range entry.
func TestDecoderEnrollmentB2CoverageManifest(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	listed := map[string]CoverageEntry{}
	for _, e := range WorkspaceCoverageManifest {
		listed[strings.TrimPrefix(e.File, modulePath+"/")] = e
	}
	for _, rel := range decoderEnrollmentB2Files {
		e, ok := listed[rel]
		if !ok || e.Group != GroupChanged || len(e.Ranges) != 0 || e.OS != buildOS(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("decoder-enrollment B2 file %s missing from the changed group with its build OS", rel)
		}
	}
}

// TestDecoderEnrollmentB2Docs (design decoder-enrollment B2, CI plan):
// docs/ci.md's Checks names the five B2 function parents with the 424-name
// inventory and its ranges, the new offline cases and benchmark subcases,
// the unchanged native call, benchmark, cross and twenty-job counts, the
// new whole-file entries and no new budget allocation or stress workload.
func TestDecoderEnrollmentB2Docs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(strings.Fields(string(doc)), " ")
	for _, w := range []string{"5 more names, 424 in all", "with the 419 earlier names unchanged and first", "the B1.5 names 416 to 419",
		"positions 419 to 424", "`devcheck native` still makes 9 ordinary calls", "`devcheck bench` keeps 13 steps", "cross keeps 12 artifacts",
		"Linux `all` still makes 32 ordinary calls", "`internal/mcpqual/decode_real.go`", "`internal/mcpqual/fixture_export.go`", "`cmd/mcpfixture-export/main.go`",
		"separating decode CPU from replay and `ValidateEnrollment` I/O", "The workflow keeps its twenty jobs", "B2 makes no new budget allocation",
		"B2 adds no new stress selector or workload", "not a hosted step or cross artifact"} {
		if !strings.Contains(s, w) {
			t.Fatalf("docs/ci.md lacks %q", w)
		}
	}
	for _, n := range b2Names() {
		if !strings.Contains(s, "`"+n+"`") {
			t.Fatalf("docs/ci.md does not name %s", n)
		}
	}
}
