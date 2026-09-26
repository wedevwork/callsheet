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
	if len(calls) != 1 || calls[0][0] != "go" || calls[0][1] != "test" {
		t.Fatalf("calls = %v", calls)
	}
}
