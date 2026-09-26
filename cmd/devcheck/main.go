// Command devcheck is the development-only check driver (test, coverage,
// bench, cross, all, native, stress, stress-packages, stress-processgroup,
// stress-functions). It is not distributed. Run it from the repository root:
//
//	go run ./cmd/devcheck all
//	go run ./cmd/devcheck stress
//
// native runs the full suite with go test -json on macOS and requires passing
// evidence for the process-group qualification tests; other hosts reject it.
// stress repeats the timing- and concurrency-sensitive tests under the race
// detector (StressCount times at each of -cpu=1,2,4) on Linux and macOS; it is
// deliberately not part of all. It runs three shards in order, each also a
// stage of its own (one CI worker job each): stress-packages,
// stress-processgroup (its three CPU settings as concurrent invocations) and
// stress-functions. None takes arguments.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"

	"github.com/wedevwork/callsheet/internal/devcheck"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, devcheck.ExecRunner))
}

func run(args []string, out, errOut io.Writer, runner devcheck.Runner) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return devcheck.Run(ctx, args, out, errOut, runner)
}
