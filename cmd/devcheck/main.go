// Command devcheck is the development-only check driver (test, coverage,
// bench, cross, all, native, stress, stress-packages, stress-packages-cpu,
// stress-plane-cpu1, stress-plane, stress-sidecar-cpu1, stress-sidecar,
// stress-processgroup, stress-functions). It is not distributed. Run it
// from the repository
// root:
//
//	go run ./cmd/devcheck all
//	go run ./cmd/devcheck stress
//
// native runs the full suite with go test -json on macOS and requires passing
// evidence for the process-group qualification tests; other hosts reject it.
// stress repeats the timing- and concurrency-sensitive tests under the race
// detector (StressCount times at each of -cpu=1,2,4) on Linux and macOS; it is
// deliberately not part of all. It runs eight shards in order, each also a
// stage of its own (one CI worker job per platform each): stress-packages
// (the combined invocation), stress-packages-cpu (the contract, mcpqual and
// workspace groups at CPU 1, 2 and 4, one concurrent group per package),
// stress-plane-cpu1 (plane at CPU 1 alone), stress-plane (plane at CPU 2
// and 4 as concurrent invocations), stress-sidecar-cpu1 and
// stress-sidecar (likewise for sidecar), stress-processgroup (its three
// CPU settings as concurrent invocations) and stress-functions. None takes
// arguments. container-e2e (Linux only) runs the M3/M4 container
// acceptance in fresh isolated Docker containers (--count=N, 1 to 20; the
// Linux test stage also runs it once) and retains its evidence in
// /tmp/callsheet-container-e2e-evidence.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"

	"github.com/wedevwork/callsheet/internal/devcheck"
)

// defaultEvidenceDir is the container acceptance's retained evidence
// directory (m3-m4-container-e2e): outside the disposable scratch tree,
// published by the CI workflow's final always-run step. Only main selects
// it; tests pass their own temporary directories.
const defaultEvidenceDir = "/tmp/callsheet-container-e2e-evidence"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, devcheck.ExecRunner, devcheck.RunOptions{EvidenceDir: defaultEvidenceDir}))
}

func run(args []string, out, errOut io.Writer, runner devcheck.Runner, opts devcheck.RunOptions) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return devcheck.Run(ctx, args, out, errOut, runner, opts)
}
