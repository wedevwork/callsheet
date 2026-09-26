package devcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// stressPackages are the timing- and concurrency-sensitive packages of
	// the packages shard, run completely (tests only, no benchmarks) with
	// the CPU list in one invocation.
	stressPackages = []string{
		"./internal/testkit",
		"./internal/testkit/fakeadapter",
		"./internal/spikes/gittransport",
		"./internal/plane",
	}
	// stressProcessGroupPackage is the processgroup shard's package
	// (iteration 02c). Its stress time is dominated by the 1 s TERM grace
	// repeated sequentially, not by CPU, so its three CPU settings run as
	// three concurrent invocations (runConcurrentCPU), each StressCount
	// times at one setting: the same 60 repetitions per test as one
	// sequential -cpu=1,2,4 invocation.
	stressProcessGroupPackage = "./internal/spikes/processgroup"
	// stressFunctionPackage and stressFunctionTests select only the FP-4/5
	// function tests, deliberately excluding unrelated ones such as the
	// twelve-artifact cross-build test. TestFP6ProcessGroups is not
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
	// excluded.
	stressPlaneSubtests = []string{"paths", "persistence", "locking", "validation", "https-only", "prelisten-validation", "bounded-shutdown", "process"}
)

const (
	// stressTestTimeout bounds each test binary (go test -timeout).
	stressTestTimeout = "6m"
	// stressWatchdog bounds one stress command, compilation included: each
	// shard stage has its own, and "stress" shares one across all shards.
	stressWatchdog = 15 * time.Minute
	// maxConcurrentCPU bounds the concurrent invocations of a Parallel
	// shard: at most three top-level go test commands run at once.
	maxConcurrentCPU = 3
	// stressStagePrefix prefixes a shard's name to form its devcheck stage.
	stressStagePrefix = "stress-"
)

// StressShard is one independently executable part of the stress plan
// (iteration 02c). Steps of a sequential shard run one after another and
// stop at the first failure; the steps of a Parallel shard run concurrently
// (runConcurrentCPU). Its devcheck stage is "stress-" + Name.
type StressShard struct {
	Name     string
	Parallel bool
	Steps    []Step
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

// stressFlags returns the fixed go test prefix for one CPU list.
func stressFlags(cpus string) []string {
	return []string{"go", "test", "-race", "-count=" + strconv.Itoa(StressCount), "-cpu=" + cpus, "-timeout=" + stressTestTimeout}
}

func stressSupported(goos string) error {
	if goos != "linux" && goos != "darwin" {
		return fmt.Errorf("devcheck: stress stage is unsupported on %q: supported: linux, darwin", goos)
	}
	return nil
}

// StressShards returns the stress plan for goos as its three fixed shards,
// in order: packages (sequential), processgroup (Parallel: one invocation
// per CPU setting) and functions (sequential). Their disjoint union is
// exactly the iteration 02b selection: every selected test runs StressCount
// times at each CPU setting under the race detector. Only linux and darwin
// are supported; every other goos is rejected before any child runs. Each
// call returns fresh slices, nested ones included.
func StressShards(goos string) ([]StressShard, error) {
	if err := stressSupported(goos); err != nil {
		return nil, err
	}
	env := func() []string { return []string{"CGO_ENABLED=1"} }
	processgroup := make([]Step, 0, len(stressCPUs))
	for _, c := range stressCPUs {
		processgroup = append(processgroup, Step{Name: "stress processgroup cpu" + strconv.Itoa(c), Env: env(),
			Argv: append(stressFlags(strconv.Itoa(c)), stressProcessGroupPackage)})
	}
	return []StressShard{
		{Name: "packages", Steps: []Step{
			{Name: "stress packages", Env: env(), Argv: append(stressFlags(stressCPUList()), stressPackages...)},
		}},
		{Name: "processgroup", Parallel: true, Steps: processgroup},
		{Name: "functions", Steps: []Step{
			{Name: "stress function", Env: env(),
				Argv: append(stressFlags(stressCPUList()), "-run="+stressSelector(stressFunctionTests), stressFunctionPackage)},
			{Name: "stress plane function", Env: env(),
				Argv: append(stressFlags(stressCPUList()), "-run="+stressPlaneSelector(), stressFunctionPackage)},
		}},
	}, nil
}

// StressSteps returns the stress plan for goos flattened in shard and CPU
// order: six race-built go test commands. It is an inspection view only;
// execution uses StressShards and never infers concurrency from this list.
// Unsupported goos values are rejected like StressShards. Each call returns
// fresh slices.
func StressSteps(goos string) ([]Step, error) {
	shards, err := StressShards(goos)
	if err != nil {
		return nil, err
	}
	var steps []Step
	for _, sh := range shards {
		steps = append(steps, sh.Steps...)
	}
	return steps, nil
}

// stressPlan returns the shards a stress stage runs: all three for
// "stress", else the single shard named by the stage.
func stressPlan(goos, stage string) ([]StressShard, error) {
	shards, err := StressShards(goos)
	if err != nil {
		return nil, err
	}
	if stage == "stress" {
		return shards, nil
	}
	for _, sh := range shards {
		if stressStagePrefix+sh.Name == stage {
			return []StressShard{sh}, nil
		}
	}
	return nil, fmt.Errorf("devcheck: no stress shard for stage %q", stage)
}

func isStressStage(stage string) bool {
	return stage == "stress" || stage == stressStagePrefix+"packages" ||
		stage == stressStagePrefix+"processgroup" || stage == stressStagePrefix+"functions"
}

// stress runs shards in order under one watchdog: a context that ends at
// the earlier of stressWatchdog from now and the caller's deadline, and
// that is always canceled on return. A shard stage passes one shard and so
// gets its own watchdog; "stress" passes all three, which share one. A
// failure or an expired watchdog fails the stage and never starts the next
// step or shard.
func (d *driver) stress(shards []StressShard) error {
	ctx, cancel := context.WithTimeout(d.ctx, stressWatchdog)
	defer cancel()
	sd := *d
	sd.ctx = ctx
	for _, sh := range shards {
		if !sh.Parallel {
			if err := sd.stressSequential(sh.Steps); err != nil {
				return err
			}
			continue
		}
		label := "stress " + sh.Name
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("devcheck: stress watchdog (%v) ended before %s: %w", stressWatchdog, label, err)
		}
		if err := runConcurrentCPU(ctx, d.run, sh.Steps, dirLogs(d.scratch), d.out); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("devcheck: stress watchdog (%v) ended during %s: %w", stressWatchdog, label, err)
			}
			return err
		}
	}
	return nil
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
// (iteration 02c): it runs steps, at most maxConcurrentCPU of them,
// concurrently under ctx and joins every started Runner call before it
// returns.
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
