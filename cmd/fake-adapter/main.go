// Command fake-adapter is the deterministic fake worker used by tests. It is a
// fixture, not a supported adapter, and is not distributed.
package main

import (
	"context"
	"io"
	"os"

	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return fakeadapter.Run(context.Background(), fakeadapter.Env{Args: args, Stdout: stdout, Stderr: stderr})
}
