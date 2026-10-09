package devcheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// testOnlyEnv names the environment variable through which the node
// function tests hand their once-built CLI to delegated package contracts
// (design 03, Function tests). It must be read only by test code.
const testOnlyEnv = "CALLSHEET_TEST_CLI_BINARY"

// testOnlySources returns the repository-relative Go files under root that
// mention name, split into test and non-test files. It walks like the
// platform guard (skippedDir) and reads raw bytes, grep-style, so any read
// or construction from a literal is caught.
func testOnlySources(root, name string) (tests, others []string, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skippedDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), name) {
			return nil
		}
		rel := relPath(root, p)
		if strings.HasSuffix(p, "_test.go") {
			tests = append(tests, rel)
		} else {
			others = append(others, rel)
		}
		return nil
	})
	sort.Strings(tests)
	sort.Strings(others)
	return tests, others, err
}

// Literal oracles for iteration 03's verification policy (design 03, CI
// plan), compared against the plans, never derived from them.
const (
	// Iteration 04 appended ./internal/adapter to the same invocation;
	// iteration 05b moved ./internal/plane (the plane's lease, registry and
	// stream contracts) to the plane shard, one invocation per CPU setting,
	// and its sidecar follow-up moved ./internal/sidecar (the reconnect
	// contracts) to the sidecar shard likewise; the contract headroom fix
	// moved ./internal/contract, and the workspace and mcpqual headroom fix
	// ./internal/mcpqual and ./internal/workspace, to the packages shard's
	// per-CPU groups.
	wantNodeStressPackages = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/adapter ./internal/mcp ./internal/workspacetransfer ./internal/taskworkspace ./internal/taskpublication"
	wantNodeStressFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"
)

// TestNodeVerificationPolicyContract is delegated from tests/function
// (TestNodePlatform/policy). It checks the exact node stress, bench and
// native plans on both systems, the native checker's handling of every
// node name, the source guard, and the DS4 guard that the test-only CLI
// variable is read only in _test.go files. Do not rename or skip its
// subtests.
func TestNodeVerificationPolicyContract(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			shards, err := StressShards(goos)
			if err != nil || len(shards) != 8 || strings.Join(shards[0].Steps[0].Argv, " ") != wantNodeStressPackages || shards[0].CPUGroups != nil ||
				shards[1].Name != "packages-cpu" || shards[1].Steps != nil ||
				strings.Join(argvOf(slices.Concat(shards[1].CPUGroups...)), "|") != strings.Join(slices.Concat(groupContract, groupMcpqual, groupWorkspace), "|") ||
				shards[2].Name != "plane-cpu1" || shards[3].Name != "plane" || joinedArgv(shards, 2, 3) != wantStressPlane1+"|"+wantStressPlane2+"|"+wantStressPlane4 ||
				joinedArgv(shards, 2) != wantStressPlane1 || joinedArgv(shards, 3) != wantStressPlane2+"|"+wantStressPlane4 ||
				shards[4].Name != "sidecar-cpu1" || shards[5].Name != "sidecar" || joinedArgv(shards, 4, 5) != wantStressSidecar1+"|"+wantStressSidecar2+"|"+wantStressSidecar4 ||
				joinedArgv(shards, 4) != wantStressSidecar1 || joinedArgv(shards, 5) != wantStressSidecar2+"|"+wantStressSidecar4 {
				t.Fatalf("packages, packages-cpu, plane and sidecar shards = %+v %v", shards, err)
			}
			fn := shards[7].Steps
			if len(fn) != 3 || fn[2].Name != "stress node function" || strings.Join(fn[2].Argv, " ") != wantNodeStressFunction ||
				strings.Join(fn[0].Argv, " ") != wantStressFunction || strings.Join(fn[1].Argv, " ") != wantStressPlaneFunction {
				t.Fatalf("functions shard = %+v", fn)
			}
			if got := strings.Join(argvOf(BenchSteps()), "|"); got != wantBenchPlan {
				t.Fatalf("bench plan = %s", got)
			}
			f := &fakeRunner{}
			code, out, errOut := runDriver(t, goos, f, "stress-functions")
			if code != 0 || !callsMatch(f.argvs(), [][]string{{wantStressFunction}, {wantStressPlaneFunction}, {wantNodeStressFunction}}) {
				t.Fatalf("%s stress-functions = %d %v %s", goos, code, f.argvs(), errOut)
			}
			os.RemoveAll(scratchFrom(out))
			req := NativeRequiredTests()
			if len(req) != 145+len(mcpNames())+len(qualNames())+len(realNames())+len(wsNames())+len(trNames())+len(latNames())+len(wsTaskNames())+len(wsDoorNames())+len(wave2Names())+len(nbwNames())+len(dceNames())+len(b1Names())+len(b15Names())+len(b2Names())+len(b3Names()) || strings.Join(req[28:58], ",") != strings.Join(nodeNames(), ",") {
				t.Fatalf("native required = %v", req)
			}
			if goos == "darwin" {
				f := &fakeRunner{native: stream(qualification()...)}
				code, out, errOut := runDriver(t, goos, f, "native")
				if code != 0 || !strings.Contains(out, "TestNodePlatform/sticky-write") {
					t.Fatalf("darwin native = %d %s", code, errOut)
				}
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		for _, name := range nodeNames() {
			evs := without(without(qualification(), "run", name), "pass", name)
			mustFail(t, "missing "+name, stream(evs...), name+" has no run event", unobserved)
			mustFail(t, "no pass "+name, stream(without(qualification(), "pass", name)...), name+" has no pass event", unobserved)
		}
	})
	t.Run("skipped", func(t *testing.T) {
		for _, name := range nodeNames() {
			evs := replacing(qualification(), "pass", name, ev("skip", NativePackage, name))
			mustFail(t, "skipped "+name, stream(evs...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		}
	})
	t.Run("failed", func(t *testing.T) {
		for _, name := range nodeNames() {
			evs := replacing(qualification(), "pass", name, ev("fail", NativePackage, name))
			mustFail(t, "failed "+name, stream(evs...), "test "+name+" in "+NativePackage+" failed")
		}
	})
	t.Run("source-guard", func(t *testing.T) {
		root := repoRoot(t)
		if err := CheckPlatformSources(root); err != nil {
			t.Fatalf("repository: %v", err)
		}
		// The sidecar's build-selected lock files need no exemption.
		for _, e := range platformGuardPolicy.exemptions {
			if strings.HasPrefix(e.file, "internal/sidecar/") || strings.HasPrefix(e.file, "internal/client/") {
				t.Fatalf("unexpected exemption %s", e.file)
			}
		}
		if len(platformGuardPolicy.exemptions) != 6 || len(platformGuardPolicy.wrappers) != 6 {
			t.Fatal("the guard's exception list grew")
		}
	})
	t.Run("test-only-env", func(t *testing.T) {
		root := repoRoot(t)
		tests, others, err := testOnlySources(root, testOnlyEnv)
		if err != nil {
			t.Fatal(err)
		}
		if len(others) != 0 {
			t.Fatalf("%s is read outside _test.go files: %v", testOnlyEnv, others)
		}
		// The guard sees the real reader (the sidecar test fixture) and
		// the function-test wrapper that sets it.
		want := map[string]bool{"internal/sidecar/helpers_test.go": false, "tests/function/nodes_test.go": false}
		for _, f := range tests {
			if _, ok := want[f]; ok {
				want[f] = true
			}
		}
		for f, seen := range want {
			if !seen {
				t.Fatalf("%s does not mention %s; test files: %v", f, testOnlyEnv, tests)
			}
		}
		// A production file that reads it is caught, in any directory the
		// guard scans; skipped directories and test files are not.
		fixture := t.TempDir()
		write := func(rel, content string) {
			p := filepath.Join(fixture, filepath.FromSlash(rel))
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(content), 0o644)
		}
		write("internal/x/x.go", "package x\n\nimport \"os\"\n\nvar bin = os.Getenv(\""+testOnlyEnv+"\")\n")
		write("internal/x/x_test.go", "package x\n\nconst env = \""+testOnlyEnv+"\"\n")
		write("design/notes.go", "package design // "+testOnlyEnv+"\n")
		write("internal/x/testdata/t.go", "package t // "+testOnlyEnv+"\n")
		tests, others, err = testOnlySources(fixture, testOnlyEnv)
		if err != nil || strings.Join(others, ",") != "internal/x/x.go" || strings.Join(tests, ",") != "internal/x/x_test.go" {
			t.Fatalf("fixture = %v %v %v", tests, others, err)
		}
		if _, _, err := testOnlySources(filepath.Join(fixture, "missing"), testOnlyEnv); err == nil {
			t.Fatal("missing root accepted")
		}
	})
}

// repoRoot finds the module root from the working directory (the package
// directory under go test).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.HasPrefix(string(b), "module github.com/wedevwork/callsheet\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}
