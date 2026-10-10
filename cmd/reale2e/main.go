//go:build reale2e

// Command reale2e is the development-only real local end-to-end harness
// (design 12a-real-e2e). It is compiled only with the reale2e build tag,
// never shipped and never run in CI with real vendors: "run" refuses to
// start without CALLSHEET_REAL_E2E=1, with any CI variable or outside
// Linux. This entrypoint only forwards host inputs; all logic lives in
// internal/reale2e. See docs/real-e2e.md.
package main

import (
	"io"
	"os"
	"runtime"

	"github.com/wedevwork/callsheet/internal/reale2e"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// run is the approved host wrapper (docs/ci.md, Platform code): it only
// forwards runtime.GOOS and runtime.GOARCH.
func run(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runFor(runtime.GOOS, runtime.GOARCH, args, getenv, stdin, stdout, stderr)
}

// runFor forwards to reale2e.Main with the presence lookup behind the CI
// refusal.
func runFor(goos, goarch string, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	return reale2e.Main(goos, goarch, args, getenv, os.LookupEnv, stdin, stdout, stderr)
}
