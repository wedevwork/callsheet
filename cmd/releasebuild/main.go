// Command releasebuild builds and verifies the four stamped Callsheet release
// products and their SHA256SUMS manifest in a fresh directory. It takes the
// version and the output directory explicitly, runs no git command, looks up
// no tag and publishes nothing. Run it from the repository root:
//
//	go run ./cmd/releasebuild --version v1.0.0-rc.1 --out /path/to/new-directory
package main

import (
	"context"
	"io"
	"os"
	"os/signal"

	"github.com/wedevwork/callsheet/internal/devcheck"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, devcheck.DefaultReleaseDeps()))
}

func run(args []string, out, errOut io.Writer, deps devcheck.ReleaseDeps) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return devcheck.ReleaseMain(ctx, args, out, errOut, deps)
}
