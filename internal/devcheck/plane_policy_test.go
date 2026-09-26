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
	wantNative     = "go test -json -count=1 -timeout=180s ./..."
	wantBenchGit   = "go test ./internal/spikes/gittransport -run ^$ -bench . -benchmem -benchtime=3x -count=1 -timeout=180s"
	wantBenchPlane = "go test ./internal/plane -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
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
		if got := strings.Join(argvOf(TestSteps("linux")), "|"); got != wantTestNative+"|"+wantTestRace {
			t.Fatalf("test plan = %s", got)
		}
		if got := strings.Join(argvOf(BenchSteps()), "|"); got != wantBenchGit+"|"+wantBenchPlane {
			t.Fatalf("bench plan = %s", got)
		}
		stress, err := StressSteps("linux")
		if err != nil || strings.Join(argvOf(stress), "|") != wantStressPlan {
			t.Fatalf("stress plan = %v %v", argvOf(stress), err)
		}
		if _, err := NativeSteps("linux"); err == nil {
			t.Fatal("linux native plan accepted")
		}
		for stage, want := range map[string]string{
			"bench":  wantBenchGit + "|" + wantBenchPlane,
			"stress": wantStressPlan,
			"test":   wantTestNative + "|" + wantTestRace,
		} {
			f := &fakeRunner{}
			code, out, errOut := runDriver(t, "linux", f, stage)
			if code != 0 || strings.Join(f.argvs(), "|") != want {
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
		if got := strings.Join(argvOf(TestSteps("darwin")), "|"); got != wantTestNative {
			t.Fatalf("darwin test plan = %s", got)
		}
		req := NativeRequiredTests()
		want := append([]string{fp6, fp6 + "/cooperative", fp6 + "/resistant", fp6 + "/leader-exits-first"}, planeNames()...)
		if strings.Join(req, ",") != strings.Join(want, ",") || len(req) != 28 {
			t.Fatalf("required = %v", req)
		}
		if err := check(stream(qualification()...)); err != nil {
			t.Fatalf("complete evidence: %v", err)
		}
		f := &fakeRunner{native: stream(qualification()...)}
		code, out, errOut := runDriver(t, "darwin", f, "native")
		if code != 0 || strings.Join(f.argvs(), "|") != wantNative || !strings.Contains(out, "TestPlaneStatus/expiry-warnings, TestPlanePlatform") {
			t.Fatalf("darwin native = %d %s", code, errOut)
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
