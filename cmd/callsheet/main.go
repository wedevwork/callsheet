// Command callsheet is the single Callsheet executable.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wedevwork/callsheet/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run connects stdio and a context canceled by SIGINT or SIGTERM to
// cli.Run.
func run(args []string, in io.Reader, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.Run(ctx, args, in, out, errOut)
}
