// Command devcheck is the development-only check driver (test, coverage,
// bench, cross, all). It is not distributed. Run it from the repository root:
//
//	go run ./cmd/devcheck all
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
