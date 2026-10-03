package devcheck

import (
	"os"
	"strings"
	"testing"
)

// Literal oracles for the iteration 02 verification policy (design 02,
// CI plan). They are compared against the plan functions, never derived
// from them.
const (
	wantTestNative = "go test -count=1 -timeout=180s ./..."
	wantTestRace   = "go test -race -count=1 -timeout=180s ./..."
	wantNative     = "go test -json -tags=realadaptercheck -count=1 -timeout=300s ./..."
	wantBenchGit   = "go test ./internal/spikes/gittransport -run ^$ -bench . -benchmem -benchtime=3x -count=1 -timeout=180s"
	wantBenchPlane = "go test ./internal/plane -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantBenchContract is iteration 03's frame benchmark command.
	wantBenchContract = "go test ./internal/contract -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantBenchSidecar and wantBenchAdapter are iteration 04's appended
	// bench commands.
	wantBenchSidecar = "go test ./internal/sidecar -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	wantBenchAdapter = "go test ./internal/adapter -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantBenchMCP is iteration 07a's appended MCP benchmark command.
	wantBenchMCP = "go test ./internal/mcp -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantBenchMCPQual is iteration 07b's appended qualification-harness
	// benchmark command.
	wantBenchMCPQual = "go test ./internal/mcpqual -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantBenchRealAdapter is iteration 08's appended tagged final-file
	// benchmark command.
	wantBenchRealAdapter = "go test ./internal/sidecar -tags=realadaptercheck -run=^$ -bench=^BenchmarkRealAdapterFile$ -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantBenchPlan is the complete bench plan in order.
	wantBenchPlan = wantBenchGit + "|" + wantBenchPlane + "|" + wantBenchContract + "|" + wantBenchSidecar + "|" + wantBenchAdapter + "|" + wantBenchMCP + "|" +
		wantBenchMCPQual + "|" + wantBenchWorkspace + "|" + wantBenchTransfer + "|" + wantBenchRealAdapter
	// wantBenchWorkspace is iteration 09a's appended workspace benchmark
	// command.
	wantBenchWorkspace = "go test ./internal/workspace -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantBenchTransfer is iteration 09b's local transfer benchmark
	// command, immediately after the workspace one.
	wantBenchTransfer = "go test ./internal/workspacetransfer -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	// wantTestTagged and wantTestTaggedRace are iteration 08's tagged
	// ordinary sidecar contract steps (race on Linux only, like the suite).
	wantTestTagged     = "go test -tags=realadaptercheck ./internal/sidecar -run=^TestRealAdapterLocal$ -count=1"
	wantTestTaggedRace = "go test -race -tags=realadaptercheck ./internal/sidecar -run=^TestRealAdapterLocal$ -count=1"
	// wantTestPlanLinux is the complete Linux test plan in order.
	wantTestPlanLinux = wantTestNative + "|" + wantTestRace + "|" + wantTestTagged + "|" + wantTestTaggedRace
)

func argvOf(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, strings.Join(s.Argv, " "))
	}
	return out
}

// TestPlaneVerificationPolicyContract is delegated from tests/function
// (TestPlanePlatform). It checks the exact Linux and Darwin plans and
// drives the native parser and stage with simulated streams and a
// simulated Runner; it never runs an actual suite. Do not rename or skip
// its subtests.
func TestPlaneVerificationPolicyContract(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		if got := strings.Join(argvOf(TestSteps("linux")), "|"); got != wantTestPlanLinux {
			t.Fatalf("test plan = %s", got)
		}
		if got := strings.Join(argvOf(BenchSteps()), "|"); got != wantBenchPlan {
			t.Fatalf("bench plan = %s", got)
		}
		stress, err := StressSteps("linux")
		if err != nil || strings.Join(argvOf(stress), "|") != wantStressPlan {
			t.Fatalf("stress plan = %v %v", argvOf(stress), err)
		}
		if _, err := NativeSteps("linux"); err == nil {
			t.Fatal("linux native plan accepted")
		}
		for stage, want := range map[string][][]string{
			"bench":               {{wantBenchGit}, {wantBenchPlane}, {wantBenchContract}, {wantBenchSidecar}, {wantBenchAdapter}, {wantBenchMCP}, {wantBenchMCPQual}, {wantBenchWorkspace}, {wantBenchTransfer}, {wantBenchRealAdapter}},
			"stress":              wantStageGroups["stress"],
			"stress-packages":     wantStageGroups["stress-packages"],
			"stress-plane-cpu1":   wantStageGroups["stress-plane-cpu1"],
			"stress-plane":        wantStageGroups["stress-plane"],
			"stress-sidecar-cpu1": wantStageGroups["stress-sidecar-cpu1"],
			"stress-sidecar":      wantStageGroups["stress-sidecar"],
			"stress-processgroup": wantStageGroups["stress-processgroup"],
			"stress-functions":    wantStageGroups["stress-functions"],
			"test":                {{wantTestNative}, {wantTestRace}, {wantTestTagged}, {wantTestTaggedRace}},
		} {
			f := &fakeRunner{}
			code, out, errOut := runDriver(t, "linux", f, stage)
			if code != 0 || !callsMatch(f.argvs(), want) {
				t.Fatalf("linux %s = %d %v %s", stage, code, f.argvs(), errOut)
			}
			os.RemoveAll(scratchFrom(out))
		}
	})
	t.Run("darwin", func(t *testing.T) {
		native, err := NativeSteps("darwin")
		if err != nil || strings.Join(argvOf(native), "|") != wantNative {
			t.Fatalf("native plan = %v %v", argvOf(native), err)
		}
		stress, err := StressSteps("darwin")
		if err != nil || strings.Join(argvOf(stress), "|") != wantStressPlan {
			t.Fatalf("stress plan = %v %v", argvOf(stress), err)
		}
		if got := strings.Join(argvOf(TestSteps("darwin")), "|"); got != wantTestNative+"|"+wantTestTagged {
			t.Fatalf("darwin test plan = %s", got)
		}
		req := NativeRequiredTests()
		want := append([]string{fp6, fp6 + "/cooperative", fp6 + "/resistant", fp6 + "/leader-exits-first"}, planeNames()...)
		// The 28 iteration-02 names are preserved first; iteration 03 appends
		// the node names, iteration 04 the role names, iteration 05 the
		// task names, iteration 06a the control names.
		if strings.Join(req[:28], ",") != strings.Join(want, ",") || len(req) != 28+len(nodeNames())+len(roleNames())+len(taskNames())+len(controlNames())+len(mcpNames())+len(qualNames())+len(realNames())+len(wsNames())+len(trNames())+len(latNames()) || strings.Join(req[28:58], ",") != strings.Join(nodeNames(), ",") {
			t.Fatalf("required = %v", req)
		}
		if err := check(stream(qualification()...)); err != nil {
			t.Fatalf("complete evidence: %v", err)
		}
		f := &fakeRunner{native: stream(qualification()...)}
		code, out, errOut := runDriver(t, "darwin", f, "native")
		// Iteration 09a: after the qualification, the coverage stage and
		// the workspace benchmarks run outside the parsed event stream;
		// iteration 09b: then the transfer benchmarks (six invocations).
		a := f.argvs()
		if code != 0 || len(a) != 6 || a[0] != wantNative || !strings.Contains(a[1], "-coverprofile=") || !strings.HasPrefix(a[2], "go tool cover") ||
			!strings.HasPrefix(a[3], "go list") || a[4] != wantBenchWorkspace || a[5] != wantBenchTransfer ||
			!strings.Contains(out, "TestPlaneStatus/expiry-warnings, TestPlanePlatform") {
			t.Fatalf("darwin native = %d %v %s", code, a, errOut)
		}
	})
	t.Run("missing", func(t *testing.T) {
		for _, name := range planeNames() {
			evs := without(without(qualification(), "run", name), "pass", name)
			mustFail(t, "missing "+name, stream(evs...), name+" has no run event", unobserved)
			mustFail(t, "no pass "+name, stream(without(qualification(), "pass", name)...), name+" has no pass event", unobserved)
			f := &fakeRunner{native: stream(evs...)}
			code, out, errOut := runDriver(t, "darwin", f, "native")
			if code != 1 || !strings.Contains(errOut, name+" has no run event") || strings.Contains(out, "stage native ok") {
				t.Fatalf("native stage with %s missing = %d %s", name, code, errOut)
			}
			os.RemoveAll(scratchFrom(out))
		}
	})
	t.Run("skipped", func(t *testing.T) {
		for _, name := range planeNames() {
			evs := replacing(qualification(), "pass", name, ev("skip", NativePackage, name))
			mustFail(t, "skipped "+name, stream(evs...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		}
	})
	t.Run("failed", func(t *testing.T) {
		for _, name := range planeNames() {
			evs := replacing(qualification(), "pass", name, ev("fail", NativePackage, name))
			mustFail(t, "failed "+name, stream(evs...), "test "+name+" in "+NativePackage+" failed")
			f := &fakeRunner{native: stream(evs...)}
			code, out, _ := runDriver(t, "darwin", f, "native")
			if code != 1 {
				t.Fatalf("native stage with %s failed = %d", name, code)
			}
			os.RemoveAll(scratchFrom(out))
		}
	})
}
