package devcheck

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// StressCount is the project's declared repeat count: every selected
// top-level test runs this many times at each CPU setting in stressCPUs. It
// is the single executable source of the count; there is no override, no
// lighter per-platform count and no environment-based bypass.
const StressCount = 20

// The stress plan is declared only here: the CPU list, the package groups,
// the function-test selector and the time budgets.
var (
	// stressCPUs are the GOMAXPROCS settings of the top-level test binaries.
	stressCPUs = []int{1, 2, 4}
	// stressPackages are the timing- and concurrency-sensitive packages run
	// completely (tests only, no benchmarks).
	stressPackages = []string{
		"./internal/testkit",
		"./internal/testkit/fakeadapter",
		"./internal/spikes/processgroup",
		"./internal/spikes/gittransport",
		"./internal/plane",
	}
	// stressFunctionPackage and stressFunctionTests select only the FP-4/5
	// function tests, deliberately excluding unrelated ones such as the
	// twelve-artifact cross-build test. TestFP6ProcessGroups is not
	// repeated here (iteration 02b): its RunExperiment is the experiment
	// ./internal/spikes/processgroup's TestExperiment already repeats in
	// the packages step; it still runs once in the ordinary suites.
	stressFunctionPackage = "./tests/function"
	stressFunctionTests   = []string{"TestFP4TransportHarness", "TestFP5GitRoundTrip"}
	// stressPlaneFunctionTests select the listener- and lock-bearing plane
	// trust function tests (iteration 02). They run in their own step: the
	// combined function binary measured 322.7 s on Linux (2026-09-26),
	// reaching design 02's 5-minute split trigger, so the pre-authorized
	// static split applies on both platforms. The union with
	// stressFunctionTests is disjoint and every selected test still runs
	// StressCount times per CPU setting.
	stressPlaneFunctionTests = []string{"TestPlaneState", "TestPlaneTLS", "TestPlaneReissue"}
	// stressPlaneSubtests are the second-level names selected under
	// stressPlaneFunctionTests (iteration 02b): the process-boundary CLI
	// scenarios only. Each parent's "contracts" subtest, which delegates to
	// internal/plane contracts already repeated in the packages step, is
	// excluded.
	stressPlaneSubtests = []string{"paths", "persistence", "locking", "validation", "https-only", "prelisten-validation", "bounded-shutdown", "process"}
)

const (
	// stressTestTimeout bounds each test binary (go test -timeout).
	stressTestTimeout = "6m"
	// stressWatchdog bounds the whole stage, compilation included.
	stressWatchdog = 15 * time.Minute
)

func stressCPUList() string {
	parts := make([]string, len(stressCPUs))
	for i, c := range stressCPUs {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ",")
}

// stressSelector is the anchored -run expression selecting tests.
func stressSelector(tests []string) string {
	return "^(" + strings.Join(tests, "|") + ")$"
}

// stressPlaneSelector is the two-level -run expression of the plane
// function step: two independently anchored alternations joined by "/".
// go test splits a -run pattern into per-level patterns only at a "/"
// outside parentheses and brackets, so this selects the parents at the top
// level and only the named subtests below them; it is not a flat
// alternation.
func stressPlaneSelector() string {
	return stressSelector(stressPlaneFunctionTests) + "/" + stressSelector(stressPlaneSubtests)
}

func stressFlags() []string {
	return []string{"go", "test", "-race", "-count=" + strconv.Itoa(StressCount), "-cpu=" + stressCPUList(), "-timeout=" + stressTestTimeout}
}

// StressSteps returns the stress plan for goos: three sequential race-built
// go test commands, the complete timing-sensitive packages, the selected
// iteration 01 function tests and the selected process-boundary subtests of
// the plane trust function tests, each repeated StressCount times at every
// CPU setting. Only linux and darwin are supported; every other goos is
// rejected before any child runs. Each call returns fresh slices.
func StressSteps(goos string) ([]Step, error) {
	if goos != "linux" && goos != "darwin" {
		return nil, fmt.Errorf("devcheck: stress stage is unsupported on %q: supported: linux, darwin", goos)
	}
	return []Step{
		{Name: "stress packages", Env: []string{"CGO_ENABLED=1"},
			Argv: append(stressFlags(), stressPackages...)},
		{Name: "stress function", Env: []string{"CGO_ENABLED=1"},
			Argv: append(stressFlags(), "-run="+stressSelector(stressFunctionTests), stressFunctionPackage)},
		{Name: "stress plane function", Env: []string{"CGO_ENABLED=1"},
			Argv: append(stressFlags(), "-run="+stressPlaneSelector(), stressFunctionPackage)},
	}, nil
}

// stress runs the plan under a watchdog: a context that ends at the earlier
// of stressWatchdog from now and the caller's deadline, and that is always
// canceled on return. A Runner error or an expired watchdog fails the stage
// and never starts the next step.
func (d *driver) stress(steps []Step) error {
	ctx, cancel := context.WithTimeout(d.ctx, stressWatchdog)
	defer cancel()
	sd := *d
	sd.ctx = ctx
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("devcheck: stress watchdog (%v) ended before %s: %w", stressWatchdog, s.Name, err)
		}
		if err := sd.steps([]Step{s}); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("devcheck: stress watchdog (%v) ended during %s: %w", stressWatchdog, s.Name, err)
		}
	}
	return nil
}
