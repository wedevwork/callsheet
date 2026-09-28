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
