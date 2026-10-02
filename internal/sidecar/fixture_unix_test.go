//go:build linux || darwin

package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// The sidecar fixture of tests/function's native group qualification
// (iteration 06a, TestControlNativeGroups): this package's compiled test
// binary, run with FixtureEnv set to its JSON configuration, is a sidecar
// Run with the production task path (the real guardian re-executed from
// the callsheet binary, real process groups, the control FIFO) whose only
// difference is the fake adapter's readiness probe, which starts no
// executable: every child it starts is a counted task child. SIGTERM is a
// graceful Run shutdown (exit 130).
const FixtureEnv = "CALLSHEET_SIDECAR_FIXTURE"

// FixtureConfig is the fixture's configuration.
type FixtureConfig struct {
	StateDir    string `json:"state_dir"`
	FakeAdapter string `json:"fake_adapter"`
	Guardian    string `json:"guardian"`
}

// probeFree is the fake adapter without its probe child.
type probeFree struct{ adapter.Adapter }

func (probeFree) Probe(context.Context, string) error { return nil }

// runFixture is the fixture's main.
func runFixture(raw string) int {
	var c FixtureConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		fmt.Fprintf(os.Stderr, "fixture config: %v\n", err)
		return 2
	}
	d := defaultDeps()
	d.guardianExe = func() (string, error) { return c.Guardian, nil }
	d.adapters = func(string) adapter.Registry {
		r, err := adapter.NewRegistry(probeFree{adapter.NewFake("")})
		if err != nil {
			panic(err)
		}
		return r
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	err := d.run(ctx, RunOptions{StateDir: c.StateDir, SoftwareVersion: "fixture", Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		FakeAdapterPath: c.FakeAdapter, GOOS: runtime.GOOS})
	if ctx.Err() != nil {
		return 130
	}
	fmt.Fprintf(os.Stderr, "fixture run: %v\n", err)
	return 1
}

// Iteration 10a: two more modes of this package's test binary.
//
// As the guardian (argv[1] is GuardianToken, as the sidecar re-executes
// it): tests/function's TestTaskFastGroupCleanup points a fixture's
// guardian at this binary for its injected-unknown scenario, the
// production guardian whose group observation is installed but never
// proves anything (unknown), so its grace must run in full.
//
// As the native probe calibration (probeCalibrationEnv set, started in a
// process group of its own): BenchmarkGuardianCompletion's one
// measurement of the native primitive outside its repeated loop.

// probeCalibrationEnv selects the probe calibration mode.
const probeCalibrationEnv = "CALLSHEET_SIDECAR_PROBE_CALIBRATION"

// probeCalibrations is the calibration's probe count.
const probeCalibrations = 1000

// runInjectedGuardian is the injected-unknown guardian.
func runInjectedGuardian(args []string) int {
	env := defaultGuardianEnv()
	env.groupAlone = func(int) (groupState, error) { return groupUnknown, nil }
	return runGuardian(args, os.Stderr, env)
}

// probeCalibration installs the subreaper (where one exists), observes
// its own childless group probeCalibrations times and prints the last
// state and the mean nanoseconds per probe.
func probeCalibration() int {
	if err := enableGuardianSubreaper(); err != nil {
		fmt.Printf("subreap: %v\n", err)
		return 1
	}
	var st groupState
	var err error
	start := time.Now()
	for range probeCalibrations {
		if st, err = probeGuardianGroup(os.Getpid()); err != nil {
			fmt.Printf("probe: %v\n", err)
			return 1
		}
	}
	fmt.Printf("%s %d\n", st, time.Since(start).Nanoseconds()/probeCalibrations)
	return 0
}
