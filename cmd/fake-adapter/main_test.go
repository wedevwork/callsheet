package main

import (
	"bytes"
	"testing"
)

func TestRunWrapper(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--stdout=hi", "--exit-code=3"}, &out, &errOut); code != 3 {
		t.Fatalf("code = %d", code)
	}
	if out.String() != "hi\n" {
		t.Fatalf("out = %q", out.String())
	}
	if code := run([]string{"--bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("bad flag code = %d", code)
	}
}
