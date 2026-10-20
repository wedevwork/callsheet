package devcheck

import (
	"context"
	"io"
)

// runFor runs args for goos in local mode (the controlled default of the
// package's driver tests; CI mode is runMode with ci true).
func runFor(ctx context.Context, goos string, args []string, out, errOut io.Writer, run Runner, opts RunOptions) int {
	return runMode(ctx, goos, false, args, out, errOut, run, opts)
}
