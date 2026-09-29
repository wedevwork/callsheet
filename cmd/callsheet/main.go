// Command callsheet is the single Callsheet executable.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/sidecar"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run connects stdio and a context canceled by SIGINT or SIGTERM to
// cli.Run. The internal task guardian (iteration 06a) is dispatched first,
// before any signal handling or CLI parsing, so the public SIGTERM
// cancellation never terminates a task's group anchor. main passes the
// process's own stdin and stdout files: the mcp leaf (iteration 07a) owns
// them as closable handles, and only it ever closes them; no global
// SIGPIPE disposition is installed here (the mcp leaf registers its own).
func run(args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) > 0 && args[0] == sidecar.GuardianToken {
		return sidecar.RunTaskGuardian(args, errOut)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.Run(ctx, args, in, out, errOut)
}
