package main

import (
	"bytes"
	"testing"
)

// TestFakeProbe checks the command's exclusive probe mode: exact stdout,
// empty stderr, exit 0; any other operand or flag is a usage error.
func TestFakeProbe(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--callsheet-probe"}, &out, &errOut); code != 0 || out.String() != "callsheet-fake-probe-v1\n" || errOut.Len() != 0 {
		t.Fatalf("probe = %d %q %q", code, out.String(), errOut.String())
	}
	for _, args := range [][]string{{"--callsheet-probe", "x"}, {"--stdout=hi", "--callsheet-probe"}} {
		out.Reset()
		if code := run(args, &out, &errOut); code != 2 || out.Len() != 0 {
			t.Fatalf("%v = %d %q", args, code, out.String())
		}
	}
}
