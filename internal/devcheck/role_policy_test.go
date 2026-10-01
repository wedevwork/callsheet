package devcheck

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Literal oracles for iteration 04's verification policy (design 04, CI
// plan and Function tests), compared against the plans, never derived from
// them.
const (
	// Placement since iteration 05b: ./internal/plane (whose role contracts
	// run in its own package) is in the plane shard, and since its sidecar
	// follow-up ./internal/sidecar is in the sidecar shard, not this
	// invocation.
	wantRoleStressPackages = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/gittransport ./internal/client ./internal/contract ./internal/adapter ./internal/mcp ./internal/mcpqual ./internal/workspace ./internal/workspacetransfer"
	wantRoleBenchPlan      = "go test ./internal/spikes/gittransport -run ^$ -bench . -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/plane -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/contract -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/sidecar -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/adapter -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/mcp -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/mcpqual -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/workspace -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/workspacetransfer -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s|" +
		"go test ./internal/sidecar -tags=realadaptercheck -run=^$ -bench=^BenchmarkRealAdapterFile$ -benchmem -benchtime=3x -count=1 -timeout=180s"
)

// wantRoleDelegations is the design's four-column table, literally.
var wantRoleDelegations = []string{
	"TestRoleProtocol/duplex|./internal/plane|^TestRoleStreamContract$/^duplex$|TestRoleStreamContract,TestRoleStreamContract/duplex",
	"TestRoleProtocol/duplex|./internal/sidecar|^TestRoleStreamContract$/^duplex$|TestRoleStreamContract,TestRoleStreamContract/duplex",
	"TestRoleProtocol/bounds|./internal/plane|^TestRoleStreamContract$/^bounds$|TestRoleStreamContract,TestRoleStreamContract/bounds",
	"TestRoleProtocol/bounds|./internal/sidecar|^TestRoleStreamContract$/^bounds$|TestRoleStreamContract,TestRoleStreamContract/bounds",
	"TestRoleReadiness/changes|./internal/sidecar|^TestRoleReadinessContract$/^changes$|TestRoleReadinessContract,TestRoleReadinessContract/changes",
	"TestRoleReadiness/reconnect|./internal/plane|^TestRoleDistributionContract$/^reconnect$|TestRoleDistributionContract,TestRoleDistributionContract/reconnect",
	"TestRolePersistence/failures|./internal/plane|^TestRoleRegistryContract$/^failures$|TestRoleRegistryContract,TestRoleRegistryContract/failures",
	"TestRoleMutation/races|./internal/plane|^TestRoleMutationContract$/^races$|TestRoleMutationContract,TestRoleMutationContract/races,TestRoleMutationContract/races/snapshot-in-flight",
	"TestRolePlatform/manuals|./internal/sidecar|^TestRolePlatformContract$/^manuals$|TestRolePlatformContract,TestRolePlatformContract/manuals",
	"TestRolePlatform/executable|./internal/adapter|^TestRolePlatformContract$/^executable$|TestRolePlatformContract,TestRolePlatformContract/executable",
	"TestRolePlatform/policy|./internal/devcheck|^TestRolePolicy$/^policy$|TestRolePolicy,TestRolePolicy/policy",
}

// passing is verbose output with run and pass evidence for names.
func passing(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString("=== RUN   " + n + "\n")
	}
	for i := len(names) - 1; i >= 0; i-- {
		b.WriteString("--- PASS: " + names[i] + " (0.01s)\n")
	}
	return b.String() + "PASS\n"
}

// importsOf returns the import paths of the non-test Go files in dir.
func importsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && skippedDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !isSource(d.Name()) {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			out[relPath(dir, p)] = append(out[relPath(dir, p)], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRolePolicy is delegated from tests/function (TestRolePlatform/policy).
// It checks iteration 04's exact plans and native evidence on both
// systems, the delegation table and its evidence rule (missing either
// package's evidence fails qualification), and the source guards. Do not
// rename or skip its subtests.
func TestRolePolicy(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		for _, goos := range []string{"linux", "darwin"} {
			shards, err := StressShards(goos)
			if err != nil || len(shards) != 7 || strings.Join(shards[0].Steps[0].Argv, " ") != wantRoleStressPackages ||
				joinedArgv(shards, 1, 2) != wantStressPlane1+"|"+wantStressPlane2+"|"+wantStressPlane4 || !shards[1].Parallel || !shards[2].Parallel ||
				joinedArgv(shards, 1) != wantStressPlane1 || joinedArgv(shards, 2) != wantStressPlane2+"|"+wantStressPlane4 ||
				joinedArgv(shards, 3, 4) != wantStressSidecar1+"|"+wantStressSidecar2+"|"+wantStressSidecar4 || !shards[3].Parallel || !shards[4].Parallel ||
				joinedArgv(shards, 3) != wantStressSidecar1 || joinedArgv(shards, 4) != wantStressSidecar2+"|"+wantStressSidecar4 ||
				joinedArgv(shards, 5) != wantStressPG1+"|"+wantStressPG2+"|"+wantStressPG4 || !shards[5].Parallel ||
				joinedArgv(shards, 6) != wantStressFunction+"|"+wantStressPlaneFunction+"|"+wantNodeStressFunction {
				t.Fatalf("%s stress plan = %+v %v", goos, shards, err)
			}
		}
		if got := strings.Join(argvOf(BenchSteps()), "|"); got != wantRoleBenchPlan {
			t.Fatalf("bench plan = %s", got)
		}
		// Native: the 58 earlier names first and unchanged, then the nine
		// role tests and their 20 subtests (iteration 05's 41 task names
		// and iteration 06a's 13 control names follow them).
		req := NativeRequiredTests()
		if len(req) != 145+len(mcpNames())+len(qualNames())+len(realNames())+len(wsNames())+len(trNames()) || strings.Join(req[58:87], ",") != strings.Join(roleNames(), ",") || len(roleNames()) != 29 {
			t.Fatalf("native required = %v", req)
		}
		f := &fakeRunner{native: stream(qualification()...)}
		code, out, errOut := runDriver(t, "darwin", f, "native")
		if code != 0 || !strings.Contains(out, "TestRolePlatform/policy") {
			t.Fatalf("darwin native = %d %s", code, errOut)
		}
		for _, name := range roleNames() {
			mustFail(t, "missing "+name, stream(without(without(qualification(), "run", name), "pass", name)...), name+" has no run event", unobserved)
			mustFail(t, "skipped "+name, stream(replacing(qualification(), "pass", name, ev("skip", NativePackage, name))...), "skipped: "+unobserved)
			mustFail(t, "failed "+name, stream(replacing(qualification(), "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
		}
		// The delegation table, literally; selectors are single argv values.
		rows := RoleDelegations()
		if len(rows) != len(wantRoleDelegations) {
			t.Fatalf("delegations = %+v", rows)
		}
		for i, d := range rows {
			if got := d.Wrapper + "|" + d.Package + "|" + d.Selector + "|" + strings.Join(d.Required, ","); got != wantRoleDelegations[i] || strings.ContainsAny(d.Selector, " \t") {
				t.Fatalf("row %d = %s", i, got)
			}
		}
		rows[0].Required[0] = "mutated"
		if RoleDelegations()[0].Required[0] != "TestRoleStreamContract" {
			t.Fatal("the table is mutable through a copy")
		}
		// Evidence: both protocol wrappers need both packages' own output.
		for _, wrapper := range []string{"TestRoleProtocol/duplex", "TestRoleProtocol/bounds"} {
			sub := strings.TrimPrefix(wrapper, "TestRoleProtocol/")
			good := passing("TestRoleStreamContract", "TestRoleStreamContract/"+sub)
			if err := CheckRoleWrapperEvidence(wrapper, map[string]string{"./internal/plane": good, "./internal/sidecar": good}); err != nil {
				t.Fatalf("%s complete: %v", wrapper, err)
			}
			for _, only := range []string{"./internal/plane", "./internal/sidecar"} {
				err := CheckRoleWrapperEvidence(wrapper, map[string]string{only: good})
				if err == nil || !strings.Contains(err.Error(), "has no invocation of") || !strings.Contains(err.Error(), unobserved) {
					t.Fatalf("%s with only %s = %v", wrapper, only, err)
				}
			}
			for name, bad := range map[string]string{
				"empty":    "",
				"no pass":  "=== RUN   TestRoleStreamContract\n=== RUN   TestRoleStreamContract/" + sub + "\n",
				"no run":   "--- PASS: TestRoleStreamContract (0.01s)\n--- PASS: TestRoleStreamContract/" + sub + " (0.00s)\n",
				"skipped":  good + "    --- SKIP: TestRoleStreamContract/" + sub + "/x (0.00s)\n",
				"failed":   good + "--- FAIL: TestRoleStreamContract/x (0.00s)\n",
				"no tests": "testing: warning: no tests to run\nPASS\n",
				"other":    passing("TestRoleStreamContract", "TestRoleStreamContract/other"),
			} {
				if err := CheckRoleWrapperEvidence(wrapper, map[string]string{"./internal/plane": good, "./internal/sidecar": bad}); err == nil {
					t.Fatalf("%s with %s sidecar evidence passed", wrapper, name)
				}
			}
			if err := CheckRoleWrapperEvidence(wrapper, map[string]string{"./internal/plane": good, "./internal/sidecar": good, "./internal/client": good}); err == nil {
				t.Fatalf("%s accepted an extra package", wrapper)
			}
		}
		races := RoleDelegationsFor("TestRoleMutation/races")
		if len(races) != 1 || CheckDelegationEvidence(races[0], passing("TestRoleMutationContract", "TestRoleMutationContract/races")) == nil ||
			CheckDelegationEvidence(races[0], passing(races[0].Required...)) != nil {
			t.Fatal("races evidence must include the snapshot-in-flight case")
		}
		if err := CheckRoleWrapperEvidence("TestRoleNothing", nil); err == nil {
			t.Fatal("an unknown wrapper passed")
		}
		// Source guards: no new host OS read (the guard's lists are
		// unchanged), the adapter package imports no plane, sidecar,
		// devcheck or testkit, and no production code imports the
		// fixture package.
		root := repoRoot(t)
		if err := CheckPlatformSources(root); err != nil {
			t.Fatalf("platform guard: %v", err)
		}
		if len(platformGuardPolicy.wrappers) != 6 || len(platformGuardPolicy.exemptions) != 6 {
			t.Fatal("the platform guard's exception list grew")
		}
		const mod = "github.com/wedevwork/callsheet/"
		for file, imps := range importsOf(t, filepath.Join(root, "internal", "adapter")) {
			for _, imp := range imps {
				for _, banned := range []string{"internal/plane", "internal/sidecar", "internal/devcheck", "internal/testkit"} {
					if strings.HasPrefix(imp, mod+banned) {
						t.Fatalf("internal/adapter/%s imports %s", file, imp)
					}
				}
			}
		}
		for file, imps := range importsOf(t, root) {
			if strings.HasPrefix(file, "internal/testkit/") || strings.HasPrefix(file, "cmd/fake-adapter/") {
				continue
			}
			for _, imp := range imps {
				if imp == mod+"internal/testkit/fakeadapter" {
					t.Fatalf("production file %s imports the fake fixture", file)
				}
			}
		}
		src, err := os.ReadFile(filepath.Join(root, "internal", "adapter", "fake.go"))
		if err != nil || !strings.Contains(string(src), `"CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD"`) || !strings.Contains(string(src), `"CALLSHEET_FAKE_READY_FD"`) {
			t.Fatalf("the adapter's environment filter lost its literal keys: %v", err)
		}
	})
}
