package devcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StressCount is the project's declared repeat count: every selected
// top-level test runs this many times at each CPU setting in stressCPUs. It
// is the single executable source of the count; there is no override, no
// lighter per-platform count and no environment-based bypass.
const StressCount = 20

// The stress plan is declared only here: the CPU list, the shards and their
// package groups, the function-test selectors and the time budgets.
var (
	// stressCPUs are the GOMAXPROCS settings of the top-level test binaries.
	stressCPUs = []int{1, 2, 4}
	// stressWaves is the static execution schedule of Parallel shards
	// (iteration 05b, DW4), keyed by shard name: each wave lists the CPU
	// settings (values of stressCPUs) whose invocations run concurrently,
	// and each wave starts only after the previous one has been joined and
	// succeeded. A Parallel shard without an entry runs all of its
	// invocations as one concurrent wave. It is empty: the plane CPU1
	// singleton and the sidecar CPU1, CPU2 and CPU4 singletons run their
	// one invocation, the plane pair its CPU 2 and 4 together, and
	// processgroup its three CPU settings together. Waves apply only to a shard of one step per
	// CPU setting (stressGroups); design 05b's plane fallback
	// "plane": {{4}, {1, 2}} is superseded by design 06a-perf, and the
	// two-step plane shard rejects it before anything starts. TestStressPlan
	// pins the empty schedule; a future wave policy needs a design revision.
	stressWaves = map[string][][]int{}
	// stressPackages are the timing- and concurrency-sensitive packages of
	// the packages shard, run completely (tests only, no benchmarks) with
	// the CPU list in one invocation. ./internal/plane left this list for
	// its own shard in iteration 05b (stressPlanePackage), and
	// ./internal/sidecar in its sidecar follow-up (stressSidecarPackage).
	stressPackages = []string{
		"./internal/testkit",
		"./internal/testkit/fakeadapter",
		"./internal/spikes/gittransport",
		// Iteration 03: the node client and wire contract, complete (the
		// node sidecar is in the sidecar shard; the wire contract runs per
		// CPU setting since the contract headroom fix, stressSplitPackages).
		"./internal/client",
		// Iteration 04: the adapter package (probe deadlines, cancellation
		// and child waits on an injected clock), complete. The role
		// contracts of plane, sidecar, client and contract already run in
		// their packages (plane and sidecar in their own shards).
		"./internal/adapter",
		// Iteration 07a: the MCP server's lifecycle, deadline and
		// concurrency unit tests (event-armed fake clocks), complete.
		"./internal/mcp",
		// Iteration 07b's qualification harness and iteration 09a's
		// workspace hub run per CPU setting since the workspace and
		// mcpqual headroom fix (stressSplitPackages).
		// Iteration 09b: the local transfers, complete, for the
		// event-armed no-progress watchdog, the ref-lock, CAS and
		// concurrent-writer tests and the cancellation joins (in-memory
		// plane and tiny fixtures; one shared production plane per
		// process; no real subprocess).
		"./internal/workspacetransfer",
		// Iteration 10b: the task workspace's cache, preparation, snapshot
		// and publication state machine, then the plane-owned publication
		// transaction with its external plane-wiring harness, complete
		// (fake clocks, tiny fixtures, no repeated real child).
		"./internal/taskworkspace",
		"./internal/taskpublication",
	}
	// stressSplitPackages are the packages-cpu shard's per-CPU packages, in
	// execution order. Each left the combined -cpu=1,2,4 invocation for
	// three single-setting invocations, each StressCount times at one
	// setting: the same 60 repetitions per test, each binary with its own
	// limit (stressPackageTimeout: 6 minutes, 8 minutes for mcpqual since
	// design 12a-mcpqual-stress-budget). Since the headroom fixes they ran in the packages
	// shard after the combined invocation; the stress worker rebalance
	// (2026-10-07) moved them unchanged to the packages-cpu shard, its own
	// stage and CI worker per platform, so the macOS packages worker no
	// longer runs them after the combined invocation. They run one
	// package's three invocations at a time as one concurrent group
	// (runConcurrentCPU, as the processgroup shard), so at most three run
	// at once.
	//   - ./internal/contract (the contract headroom fix, 2026-10-02): its
	//     race time is CPU-bound on maximum-size JSON, and inside the
	//     combined invocation it came within 11 s of its limit on a slow
	//     hosted runner.
	//   - ./internal/mcpqual (iteration 07b's qualification harness: its
	//     event-armed cancellation/completion, progress/completion and
	//     deadline/result races on injected runners and fake clocks) and
	//     ./internal/workspace (iteration 09a's hub: lock cancellation,
	//     reader/writer, CAS winner, registry race and
	//     cancellation-before-publication tests on deterministic barriers
	//     and hooks), the workspace and mcpqual headroom fix (2026-10-02):
	//     236.6 s and 273.3 s inside the combined invocation on a normal
	//     hosted Linux runner, about 355 s and 410 s at the observed 1.5x
	//     slow-runner factor.
	// ./internal/workspacetransfer stays in the combined invocation, after
	// mcp and before taskworkspace and taskpublication: iteration 10b's
	// r0.5 schedule ran it as a fourth group, and its hosted remeasure
	// returned it here (r0.6), since the later per-CPU waves were already
	// full and the overlap inside the combined invocation was the saving.
	// Historical: its 198.4 s (about 300 s at 1.5x) was measured then; for
	// the current budget rationale see stressPackagesTestTimeout (design
	// 12a-stress-packages-budget).
	stressSplitPackages = []string{"./internal/contract", "./internal/mcpqual", "./internal/workspace"}
	// stressPlanePackage is the internal/plane package of the plane shards,
	// distinct from the plane function-test selector in the functions shard
	// (stressPlaneFunctionTests). Its stress time is CPU-bound under the
	// race detector, and one -cpu=1,2,4 binary outgrew its 6-minute limit
	// on the hosted runners (iteration 05b), so its three CPU settings run
	// as single-setting invocations, each StressCount times at one setting:
	// the same 60 repetitions per test. Since design 06a-perf the slowest,
	// CPU 1, runs alone in the plane-cpu1 shard (its own CI worker, away
	// from competing race binaries), and CPU 2 and 4 run concurrently
	// (runConcurrentCPU) in the plane shard.
	stressPlanePackage = "./internal/plane"
	// stressSidecarPackage is the sidecar shards' package (design 05b's
	// pre-decided sidecar follow-up, triggered when the first hosted run
	// after the plane split measured the sidecar binary over 300.0 s in
	// the packages shard). Its reconnect contracts run real plane
	// subprocesses and TestTaskExecutionContract/process launches the
	// probe and the guardian-backed task children (5 per repetition since
	// iteration 06a: one probe and two guardian/adapter pairs, 300 across
	// its CPU settings), so its three CPU settings run as single-setting
	// invocations, each StressCount times at one setting: the same 60
	// repetitions per test and the same children. Since design 06a-perf
	// CPU 1 runs alone in the sidecar-cpu1 shard. Since design
	// 12a-stress-sidecar-margin CPU 2 runs alone in the sidecar shard and
	// CPU 4 runs alone in the sidecar-cpu4 shard (runConcurrentCPU
	// singletons, as CPU 1 does), so the two binaries no longer share a
	// worker. The plane pair stays concurrent.
	stressSidecarPackage = "./internal/sidecar"
	// stressProcessGroupPackage is the processgroup shard's package
	// (iteration 02c). Its stress time is dominated by the 1 s TERM grace
	// repeated sequentially, not by CPU, so its three CPU settings run as
	// three concurrent invocations (runConcurrentCPU), each StressCount
	// times at one setting: the same 60 repetitions per test as one
	// sequential -cpu=1,2,4 invocation.
	stressProcessGroupPackage = "./internal/spikes/processgroup"
	// stressFunctionPackage and stressFunctionTests select only the FP-4/5
	// function tests, deliberately excluding unrelated ones such as FP-8's
	// cross-build entry-point test. TestFP6ProcessGroups is not
	// repeated here (iteration 02b): its RunExperiment is the experiment
	// ./internal/spikes/processgroup's TestExperiment already repeats in
	// the processgroup shard; it still runs once in the ordinary suites.
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
	// internal/plane contracts already repeated in the packages shard, is
	// excluded; those contracts repeat in the plane shard.
	stressPlaneSubtests = []string{"paths", "persistence", "locking", "validation", "https-only", "prelisten-validation", "bounded-shutdown", "process"}
	// stressNodeFunctionTests and stressNodeSubtests select the two
	// process-boundary node scenarios (iteration 03): real competing
	// sidecar processes and a real sidecar CLI's SIGTERM shutdown. The
	// delegated clock contracts behind TestNodeReconnect's restart and
	// disconnect and TestNodeLease are repeated directly in the sidecar
	// and plane shards, so their wrappers are not repeated here.
	stressNodeFunctionTests = []string{"TestNodeEnrollment", "TestNodeReconnect"}
	stressNodeSubtests      = []string{"locking", "shutdown"}
)

const (
	// stressTestTimeout bounds each test binary (go test -timeout) except
	// the combined packages command's binaries (stressPackagesTestTimeout),
	// the mcpqual per-CPU binaries (stressMcpqualTestTimeout) and the
	// sidecar CPU1 binary (stressSidecarCPU1TestTimeout). Sidecar CPU2 and
	// CPU4 keep it.
	stressTestTimeout = "6m"
	// stressPackagesTestTimeout bounds each test binary of the combined
	// "stress packages" command, all nine of its packages (design
	// 12a-stress-packages-budget): its macOS workspacetransfer binary hit
	// the 6-minute limit at 360.050 s (main run 40224728895, attempt 1)
	// with completed hosted samples up to 292.704 s. The exception belongs
	// to the combined command, not to a package: stressPackageTimeout
	// keeps 6 minutes for every single-package step but mcpqual's. It is a
	// static plan decision: no option or environment overrides it.
	stressPackagesTestTimeout = "8m"
	// stressMcpqualTestTimeout bounds each ./internal/mcpqual per-CPU stress
	// binary (design 12a-mcpqual-stress-budget): its macOS CPU1 binary hit
	// the 6-minute limit at 360.2 s with CPU2 and CPU4 at 346.0 s and
	// 342.0 s (main run 39546490440), without a package cost regression.
	// It is a static plan decision: no option or environment overrides it.
	stressMcpqualTestTimeout = "8m"
	// stressSidecarCPU1TestTimeout bounds the one sidecar CPU1 stress
	// binary, "stress sidecar cpu1" of the sidecar-cpu1 shard (design
	// 12a-stress-sidecar-cpu1-budget, owner decision 2026-10-27): its macOS
	// binary hit the 6-minute limit at 360.214 s (main run 40412993068,
	// attempt 1) with archived completed package samples up to 280.364 s and
	// no sidecar change. The exception belongs to that command, not to the
	// package: stressPackageTimeout keeps 6 minutes for ./internal/sidecar,
	// and sidecar CPU2 and CPU4 keep stressTestTimeout. It is a static plan
	// decision: no option or environment overrides it.
	stressSidecarCPU1TestTimeout = "8m"
	// stressWatchdog bounds one stress command, compilation included: each
	// shard stage has its own, and "stress" shares one across all shards.
	stressWatchdog = 15 * time.Minute
	// maxConcurrentCPU bounds the concurrent invocations of a Parallel
	// shard: at most three top-level go test commands run at once, within
	// one shard only (shards never overlap).
	maxConcurrentCPU = 3
	// stressStagePrefix prefixes a shard's name to form its devcheck stage.
	stressStagePrefix = "stress-"
)

// StressShard is one independently executable part of the stress plan
// (iteration 02c). Steps of a sequential shard run one after another and
// stop at the first failure; the steps of a Parallel shard run concurrently
// (runConcurrentCPU), one step per CPU setting in ascending CPU order, in
// the waves stressWaves declares (by default one wave of all steps). A
// Parallel shard holds one to three steps: a CPU1, CPU2 or CPU4 singleton
// (plane and sidecar CPU1 since design 06a-perf; sidecar CPU2 and CPU4
// since design 12a-stress-sidecar-margin), a CPU2/CPU4 pair (plane) or all
// three CPU settings (processgroup); a singleton still runs through
// runConcurrentCPU, for its lifecycle and log format. A sequential
// shard may also hold CPUGroups: for each split package, one step per CPU
// setting. The groups run after its Steps, one after another, each as one
// concurrent group through runConcurrentCPU (the contract, mcpqual and
// workspace invocations since the headroom fixes, in the group-only
// packages-cpu shard since the stress worker rebalance); stressWaves never
// applies to them. Its devcheck stage is "stress-" + Name.
type StressShard struct {
	Name      string
	Parallel  bool
	Steps     []Step
	CPUGroups [][]Step
}

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

// stressNodeSelector is the two-level -run expression of the node function
// step, built like stressPlaneSelector.
func stressNodeSelector() string {
	return stressSelector(stressNodeFunctionTests) + "/" + stressSelector(stressNodeSubtests)
}

// stressFlags returns the fixed go test prefix for one CPU list and one
// test-binary timeout.
func stressFlags(cpus, timeout string) []string {
	return []string{"go", "test", "-race", "-count=" + strconv.Itoa(StressCount), "-cpu=" + cpus, "-timeout=" + timeout}
}

// stressPackageTimeout is the package-only test-binary timeout of a
// single-package stress step built by perCPU: stressMcpqualTestTimeout for
// exactly ./internal/mcpqual, stressTestTimeout for every other package,
// ./internal/sidecar included. It is not the sidecar CPU1 command's policy:
// that shard builds its one step with stressSidecarCPU1TestTimeout directly
// (design 12a-stress-sidecar-cpu1-budget), so CPU2 and CPU4 keep 6 minutes.
func stressPackageTimeout(pkg string) string {
	if pkg == "./internal/mcpqual" {
		return stressMcpqualTestTimeout
	}
	return stressTestTimeout
}

func stressSupported(goos string) error {
	if goos != "linux" && goos != "darwin" {
		return fmt.Errorf("devcheck: stress stage is unsupported on %q: supported: linux, darwin", goos)
	}
	return nil
}

// StressShards returns the stress plan for goos as its nine fixed shards,
// in order: packages (sequential: the combined invocation alone),
// packages-cpu (sequential and group-only, the stress worker rebalance:
// no Steps, and the three CPU settings of each of stressSplitPackages as
// one concurrent group per package), plane-cpu1 (Parallel: plane's CPU 1
// invocation alone, design 06a-perf), plane (Parallel: CPU 2 and 4,
// iteration 05b), sidecar-cpu1 (Parallel: sidecar's CPU 1 alone, design
// 06a-perf, with its own 8-minute binary timeout since design
// 12a-stress-sidecar-cpu1-budget), sidecar (Parallel: sidecar's CPU 2 alone, design
// 12a-stress-sidecar-margin), sidecar-cpu4 (Parallel: sidecar's CPU 4
// alone, the same design), processgroup (Parallel: CPU 1, 2 and 4,
// iteration 02c) and functions (sequential). Their disjoint union is exactly the iteration 02b
// selection plus iteration 03's node packages and node function selector
// and iteration 04's adapter package: every selected test runs StressCount
// times at each CPU setting under the race detector. Only linux and darwin
// are supported; every other goos is rejected before any child runs. Each
// call returns fresh slices, nested ones included. packages-cpu contains
// all three per-CPU groups at CPU 1, 2 and 4; unlike plane-cpu1 and
// sidecar-cpu1, it is not a CPU1-only shard, and there is no packages-cpu1
// stage.
func StressShards(goos string) ([]StressShard, error) {
	if err := stressSupported(goos); err != nil {
		return nil, err
	}
	env := func() []string { return []string{"CGO_ENABLED=1"} }
	// perCPU returns one single-setting step of pkg for each CPU setting in
	// cpus, in the given (ascending) order.
	perCPU := func(name, pkg string, cpus []int) []Step {
		steps := make([]Step, 0, len(cpus))
		for _, c := range cpus {
			steps = append(steps, Step{Name: "stress " + name + " cpu" + strconv.Itoa(c), Env: env(),
				Argv: append(stressFlags(strconv.Itoa(c), stressPackageTimeout(pkg)), pkg)})
		}
		return steps
	}
	var groups [][]Step
	for _, pkg := range stressSplitPackages {
		groups = append(groups, perCPU(path.Base(pkg), pkg, stressCPUs))
	}
	return []StressShard{
		{Name: "packages", Steps: []Step{
			{Name: "stress packages", Env: env(), Argv: append(stressFlags(stressCPUList(), stressPackagesTestTimeout), stressPackages...)},
		}},
		{Name: "packages-cpu", CPUGroups: groups},
		{Name: "plane-cpu1", Parallel: true, Steps: perCPU("plane", stressPlanePackage, []int{1})},
		{Name: "plane", Parallel: true, Steps: perCPU("plane", stressPlanePackage, []int{2, 4})},
		// Design 12a-stress-sidecar-cpu1-budget: the sidecar CPU1 singleton
		// is built explicitly with its own 8-minute binary timeout; perCPU
		// and stressPackageTimeout stay package-only.
		{Name: "sidecar-cpu1", Parallel: true, Steps: []Step{
			{Name: "stress sidecar cpu1", Env: env(), Argv: append(stressFlags("1", stressSidecarCPU1TestTimeout), stressSidecarPackage)},
		}},
		{Name: "sidecar", Parallel: true, Steps: perCPU("sidecar", stressSidecarPackage, []int{2})},
		{Name: "sidecar-cpu4", Parallel: true, Steps: perCPU("sidecar", stressSidecarPackage, []int{4})},
		{Name: "processgroup", Parallel: true, Steps: perCPU("processgroup", stressProcessGroupPackage, stressCPUs)},
		{Name: "functions", Steps: []Step{
			{Name: "stress function", Env: env(),
				Argv: append(stressFlags(stressCPUList(), stressTestTimeout), "-run="+stressSelector(stressFunctionTests), stressFunctionPackage)},
			{Name: "stress plane function", Env: env(),
				Argv: append(stressFlags(stressCPUList(), stressTestTimeout), "-run="+stressPlaneSelector(), stressFunctionPackage)},
			{Name: "stress node function", Env: env(),
				Argv: append(stressFlags(stressCPUList(), stressTestTimeout), "-run="+stressNodeSelector(), stressFunctionPackage)},
		}},
	}, nil
}

// StressSteps returns the stress plan for goos flattened in shard and CPU
// order: twenty-two race-built go test commands (a shard's Steps, then its
// CPUGroups in order). It is an inspection view only;
// execution uses StressShards (and stressWaves for the order of a Parallel
// shard's invocations) and never infers concurrency or execution order
// from this list. Unsupported goos values are rejected like StressShards.
// Each call returns fresh slices.
func StressSteps(goos string) ([]Step, error) {
	shards, err := StressShards(goos)
	if err != nil {
		return nil, err
	}
	var steps []Step
	for _, sh := range shards {
		steps = append(steps, sh.Steps...)
		for _, g := range sh.CPUGroups {
			steps = append(steps, g...)
		}
	}
	return steps, nil
}

// stressPlan returns the shards a stress stage runs: all nine for
// "stress", else the single shard named by the stage. An invalid wave
// schedule (stressWaves) for any returned shard is rejected here, before a
// scratch directory or child exists.
func stressPlan(goos, stage string) ([]StressShard, error) {
	shards, err := StressShards(goos)
	if err != nil {
		return nil, err
	}
	if stage != "stress" {
		var one []StressShard
		for _, sh := range shards {
			if stressStagePrefix+sh.Name == stage {
				one = []StressShard{sh}
			}
		}
		if one == nil {
			return nil, fmt.Errorf("devcheck: no stress shard for stage %q", stage)
		}
		shards = one
	}
	for _, sh := range shards {
		if !sh.Parallel {
			continue
		}
		if _, err := stressGroups(sh, stressWaves[sh.Name]); err != nil {
			return nil, err
		}
	}
	return shards, nil
}

// isStressStage reports whether stage is "stress" or one of the nine
// shard stages (the plane and sidecar CPU1 stages since design 06a-perf,
// packages-cpu since the stress worker rebalance, sidecar CPU4 since
// design 12a-stress-sidecar-margin).
func isStressStage(stage string) bool {
	switch stage {
	case "stress", stressStagePrefix + "packages", stressStagePrefix + "packages-cpu", stressStagePrefix + "plane-cpu1", stressStagePrefix + "plane", stressStagePrefix + "sidecar-cpu1",
		stressStagePrefix + "sidecar", stressStagePrefix + "sidecar-cpu4", stressStagePrefix + "processgroup", stressStagePrefix + "functions":
		return true
	}
	return false
}

// stress runs shards in order under one watchdog: a context that ends at
// the earlier of stressWatchdog from now and the caller's deadline, and
// that is always canceled on return. A shard stage passes one shard and so
// gets its own watchdog; "stress" passes all nine, which share one. A
// failure or an expired watchdog fails the stage and never starts the next
// step, wave, group or shard. A sequential shard's CPUGroups run after
// its Steps (none in the group-only packages-cpu shard), only if they
// succeeded, one group after another, each as one concurrent group
// (runConcurrentCPU). Every Parallel shard's waves (stressGroups) are resolved
// before anything starts, so an invalid schedule starts nothing
// (stressPlan has already rejected it for a dispatched stage).
func (d *driver) stress(shards []StressShard) error {
	groups := make([][][]Step, len(shards))
	for i, sh := range shards {
		if !sh.Parallel {
			continue
		}
		g, err := stressGroups(sh, stressWaves[sh.Name])
		if err != nil {
			return err
		}
		groups[i] = g
	}
	ctx, cancel := context.WithTimeout(d.ctx, stressWatchdog)
	defer cancel()
	sd := *d
	sd.ctx = ctx
	for i, sh := range shards {
		if !sh.Parallel {
			if err := sd.stressSequential(sh.Steps); err != nil {
				return err
			}
			// stressSequential has checked the watchdog after the last
			// step (if any), and runConcurrentCPU starts nothing once it
			// is done, so an expired watchdog fails a group-only shard
			// before its first group.
			for g, group := range sh.CPUGroups {
				if err := runConcurrentCPU(ctx, d.run, group, dirLogs(d.scratch), d.out); err != nil {
					if ctx.Err() != nil {
						return fmt.Errorf("devcheck: stress watchdog (%v) ended during stress %s per-CPU group %d: %w", stressWatchdog, sh.Name, g+1, err)
					}
					return err
				}
			}
			continue
		}
		for w, group := range groups[i] {
			label := "stress " + sh.Name
			if len(groups[i]) > 1 {
				label += " wave " + strconv.Itoa(w+1)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("devcheck: stress watchdog (%v) ended before %s: %w", stressWatchdog, label, err)
			}
			if err := runConcurrentCPU(ctx, d.run, group, dirLogs(d.scratch), d.out); err != nil {
				if ctx.Err() != nil {
					return fmt.Errorf("devcheck: stress watchdog (%v) ended during %s: %w", stressWatchdog, label, err)
				}
				return err
			}
		}
	}
	return nil
}

// stressGroups returns the concurrent groups of Parallel shard sh in
// execution order (iteration 05b, DW4). With no waves the shard is one
// group of all its steps. Otherwise each wave names CPU settings, and the
// step of CPU setting stressCPUs[i] is sh.Steps[i]: every step must be
// covered exactly once, by nonempty waves of at most maxConcurrentCPU
// steps, or the schedule is rejected before anything runs. Waved groups
// are fresh slices; neither kind of group changes the shard's step order,
// its flattened inspection view.
func stressGroups(sh StressShard, waves [][]int) ([][]Step, error) {
	if len(waves) == 0 {
		return [][]Step{sh.Steps}, nil
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("devcheck: invalid stress waves %v for shard %s: "+format, append([]any{waves, sh.Name}, args...)...)
	}
	if !sh.Parallel || len(sh.Steps) != len(stressCPUs) {
		return nil, bad("not a Parallel shard of one step per CPU setting")
	}
	used := make([]bool, len(stressCPUs))
	groups := make([][]Step, 0, len(waves))
	for _, wave := range waves {
		if len(wave) == 0 || len(wave) > maxConcurrentCPU {
			return nil, bad("a wave runs 1 to %d invocations, got %d", maxConcurrentCPU, len(wave))
		}
		group := make([]Step, 0, len(wave))
		for _, c := range wave {
			i := slices.Index(stressCPUs, c)
			if i < 0 {
				return nil, bad("CPU setting %d is not in %v", c, stressCPUs)
			}
			if used[i] {
				return nil, bad("CPU setting %d appears twice", c)
			}
			used[i] = true
			group = append(group, sh.Steps[i])
		}
		groups = append(groups, group)
	}
	for i, u := range used {
		if !u {
			return nil, bad("CPU setting %d is not scheduled", stressCPUs[i])
		}
	}
	return groups, nil
}

// stressSequential runs steps one after another under d.ctx (the watchdog
// context), stopping at the first failure.
func (d *driver) stressSequential(steps []Step) error {
	for _, s := range steps {
		if err := d.ctx.Err(); err != nil {
			return fmt.Errorf("devcheck: stress watchdog (%v) ended before %s: %w", stressWatchdog, s.Name, err)
		}
		if err := d.stressStep(s); err != nil {
			return err
		}
		if err := d.ctx.Err(); err != nil {
			return fmt.Errorf("devcheck: stress watchdog (%v) ended during %s: %w", stressWatchdog, s.Name, err)
		}
	}
	return nil
}

// errNoTests fails a selector step whose go test reported that it ran no
// tests (iteration 02c, DS1): an empty selection is never a pass.
var errNoTests = errors.New("go test reported no tests to run for the selector")

// stressStep runs one sequential stress step, logged like every other step,
// reports its elapsed time and, for a step with a -run selector, fails if
// go test reported no tests to run.
func (d *driver) stressStep(s Step) error {
	fmt.Fprintf(d.out, "devcheck: %s: %s\n", s.Name, strings.Join(s.Argv, " "))
	guard := &noTestsGuard{}
	var capture io.Writer
	if hasSelector(s) {
		capture = io.MultiWriter(d.out, guard)
	}
	start := time.Now()
	err := d.logged(s, capture)
	if err == nil && guard.seen() {
		err = &StepError{Step: s, Err: errNoTests}
	}
	reportOutcome(d.out, s, time.Since(start), err)
	return err
}

func hasSelector(s Step) bool {
	for _, a := range s.Argv {
		if strings.HasPrefix(a, "-run=") {
			return true
		}
	}
	return false
}

func reportOutcome(out io.Writer, s Step, elapsed time.Duration, err error) {
	if err != nil {
		fmt.Fprintf(out, "devcheck: %s: FAILED after %.1fs\n", s.Name, elapsed.Seconds())
		return
	}
	fmt.Fprintf(out, "devcheck: %s: ok in %.1fs\n", s.Name, elapsed.Seconds())
}

// noTestsMarker is common to go test's "[no tests to run]" package summary
// and the test binary's "testing: warning: no tests to run".
const noTestsMarker = "no tests to run"

// noTestsGuard detects noTestsMarker in a stream, across write boundaries.
// It is safe for the concurrent stdout and stderr copies of one child.
type noTestsGuard struct {
	mu    sync.Mutex
	carry []byte
	found bool
}

func (g *noTestsGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.found {
		buf := append(append([]byte(nil), g.carry...), p...)
		if bytes.Contains(buf, []byte(noTestsMarker)) {
			g.found = true
		}
		if keep := len(noTestsMarker) - 1; len(buf) > keep {
			buf = buf[len(buf)-keep:]
		}
		g.carry = buf
	}
	return len(p), nil
}

func (g *noTestsGuard) seen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.found
}

// stressLogs creates the separate log of each concurrent invocation and
// reopens it for replay.
type stressLogs interface {
	Create(stepName string) (w io.WriteCloser, path string, err error)
	Open(path string) (io.ReadCloser, error)
}

// dirLogs keeps the logs in a scratch directory, named like every other
// step log.
type dirLogs string

func (d dirLogs) Create(stepName string) (io.WriteCloser, string, error) {
	p := filepath.Join(string(d), stepLogName(stepName))
	f, err := os.Create(p)
	if err != nil {
		return nil, p, err
	}
	return f, p, nil
}

func (d dirLogs) Open(path string) (io.ReadCloser, error) { return os.Open(path) }

// syncLog is one invocation's only output: its stdout and stderr, combined
// under a mutex into its own log. It keeps a bounded tail for the failure
// message and the first write error, after which it rejects further output.
type syncLog struct {
	mu   sync.Mutex
	w    io.Writer
	tail tailBuffer
	err  error
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tail.Write(p)
	if l.err != nil {
		return 0, l.err
	}
	n, err := l.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		l.err = err
	}
	return n, err
}

func (l *syncLog) state() (tail string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.tail.b), l.err
}

// cpuRun is one concurrent invocation's bookkeeping. The coordinator owns
// every field except err and elapsed. Preparation writes err (a log that
// could not be created) before any worker starts; after launch only the
// invocation's worker goroutine writes err and elapsed, before the
// coordinator's join.
type cpuRun struct {
	step    Step
	path    string
	w       io.WriteCloser
	log     *syncLog
	started bool
	err     error
	elapsed time.Duration
}

// runConcurrentCPU is the concurrent coordinator of a Parallel stress shard
// (iteration 02c; one call per wave, iteration 05b): it runs steps, one to
// maxConcurrentCPU of them (a CPU1, CPU2 or CPU4 singleton, the plane
// CPU2/CPU4 pair, or three, design 06a-perf and design
// 12a-stress-sidecar-margin), concurrently under ctx and joins every started Runner
// call before it returns.
//
//   - A context already done starts nothing. All logs are created before
//     any launch; if one cannot be, nothing starts and the prepared logs are
//     closed. The context is checked before each launch: once it is done no
//     further invocation starts, and those not started fail the shard.
//   - Each invocation writes only to its own log through a synchronized
//     writer; out receives the announcements before launch and, after the
//     join, each log replayed from disk under its step name in plan order,
//     with the invocation's elapsed time and outcome.
//   - A failed invocation does not cancel its siblings: every result is
//     collected, then the shard fails. Failures are aggregated in plan
//     order with the command's identity: Runner errors and log create,
//     write, close, read and replay errors, which fail even an invocation
//     whose child exited zero.
//   - A context done after launch (the watchdog or the caller) fails the
//     shard even if every Runner returned nil. Cancellation reaches every
//     outstanding call through ctx; the coordinator still waits for them.
//     ExecRunner then ends each direct go test child, but up to three test
//     binaries and their descendants can outlive a forced kill as orphans
//     until the runner ends (the 01c limit): the shard fails regardless,
//     and there is no process-tree supervisor.
//
// There are no retries and no background work survives the call.
func runConcurrentCPU(ctx context.Context, run Runner, steps []Step, logs stressLogs, out io.Writer) error {
	if len(steps) == 0 || len(steps) > maxConcurrentCPU {
		return fmt.Errorf("devcheck: a concurrent shard runs 1 to %d invocations, got %d", maxConcurrentCPU, len(steps))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("devcheck: context ended before %s: %w", steps[0].Name, err)
	}
	runs := make([]*cpuRun, len(steps))
	prepFailed := false
	for i, s := range steps {
		w, p, err := logs.Create(s.Name)
		runs[i] = &cpuRun{step: s, path: p}
		if err != nil {
			runs[i].err = fmt.Errorf("create log %s: %w", p, err)
			prepFailed = true
			continue
		}
		runs[i].w = w
		runs[i].log = &syncLog{w: w, tail: tailBuffer{max: 2000}}
	}
	if prepFailed {
		// Close every prepared log; report create and close failures
		// together in plan order.
		var prepErrs []error
		for _, r := range runs {
			if r.err != nil {
				prepErrs = append(prepErrs, &StepError{Step: r.step, Err: r.err})
				continue
			}
			if err := r.w.Close(); err != nil {
				prepErrs = append(prepErrs, &StepError{Step: r.step, Err: fmt.Errorf("close log %s: %w", r.path, err)})
			}
		}
		return errors.Join(prepErrs...)
	}
	for _, r := range runs {
		fmt.Fprintf(out, "devcheck: %s: %s (concurrent, log %s)\n", r.step.Name, strings.Join(r.step.Argv, " "), r.path)
	}
	var wg sync.WaitGroup
	for _, r := range runs {
		if ctx.Err() != nil {
			break
		}
		r.started = true
		wg.Add(1)
		go func(r *cpuRun) {
			defer wg.Done()
			start := time.Now()
			err := run(ctx, r.step.Argv, MergeEnv(os.Environ(), r.step.Env), "", r.log, r.log)
			r.elapsed = time.Since(start)
			r.err = err
		}(r)
	}
	wg.Wait()
	ctxErr := ctx.Err()
	var errs []error
	for _, r := range runs {
		errs = append(errs, finishCPURun(r, logs, out, ctxErr)...)
	}
	if ctxErr != nil {
		errs = append(errs, fmt.Errorf("devcheck: context ended while the concurrent invocations ran: %w", ctxErr))
	}
	return errors.Join(errs...)
}

// finishCPURun closes, replays and reports one joined (or never started)
// invocation and returns its failures.
func finishCPURun(r *cpuRun, logs stressLogs, out io.Writer, ctxErr error) []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, &StepError{Step: r.step, Err: fmt.Errorf(format, args...)})
	}
	if err := r.w.Close(); err != nil {
		fail("close log %s: %w", r.path, err)
	}
	if !r.started {
		fail("not started: %w", ctxErr)
		fmt.Fprintf(out, "devcheck: %s: not started\n", r.step.Name)
		return errs
	}
	tail, writeErr := r.log.state()
	if r.err != nil {
		errs = append(errs, &StepError{Step: r.step, Err: r.err, Stderr: tail})
	}
	if writeErr != nil {
		fail("write log %s: %w", r.path, writeErr)
	}
	if _, err := fmt.Fprintf(out, "devcheck: ---- %s log (%s) ----\n", r.step.Name, r.path); err != nil {
		fail("replay log %s: %w", r.path, err)
	} else if f, err := logs.Open(r.path); err != nil {
		fail("read log %s: %w", r.path, err)
	} else {
		if _, err := io.Copy(out, f); err != nil {
			fail("replay log %s: %w", r.path, err)
		}
		if err := f.Close(); err != nil {
			fail("close log %s after replay: %w", r.path, err)
		}
	}
	var outcome error
	if len(errs) > 0 {
		outcome = errors.Join(errs...)
	}
	reportOutcome(out, r.step, r.elapsed, outcome)
	return errs
}
