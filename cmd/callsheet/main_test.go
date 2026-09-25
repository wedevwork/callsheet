package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunWrapper(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if out.String() != "callsheet dev protocol=1\n" || errOut.Len() != 0 {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"task", "ls"}, nil, &out, &errOut); code != 8 || out.Len() != 0 {
		t.Fatalf("stub code = %d out=%q", code, out.String())
	}
}
