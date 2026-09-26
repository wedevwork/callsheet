package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestRunWrapper(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut, nil); code != 2 || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("no args = %d %q", code, errOut.String())
	}
	// The wrapper passes the injected runner through; it never recursively
	// invokes devcheck itself.
	var calls [][]string
	fake := func(_ context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
		calls = append(calls, argv)
		return nil
	}
	if code := run([]string{"bench"}, &out, &errOut, fake); code != 0 {
		t.Fatalf("bench = %d %s", code, errOut.String())
	}
	// Two bench commands: git transport, then plane trust (iteration 02).
	if len(calls) != 2 || calls[0][0] != "go" || calls[0][1] != "test" || calls[1][2] != "./internal/plane" {
		t.Fatalf("calls = %v", calls)
	}
	// The stress shard stages (iteration 02c) take no operands or flags:
	// rejected with exit 2 before any child runs.
	calls = nil
	for _, args := range [][]string{{"stress-packages", "x"}, {"stress-processgroup", "-cpu=1"}, {"stress-functions", "-count=1"}, {"stress", "-o", "p"}} {
		errOut.Reset()
		if code := run(args, &out, &errOut, fake); code != 2 || len(calls) != 0 || !strings.Contains(errOut.String(), "stress-processgroup") {
			t.Fatalf("%v = %d, %d calls, %q", args, code, len(calls), errOut.String())
		}
	}
}
