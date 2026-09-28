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
	if out.String() != "callsheet dev protocol=5\n" || errOut.Len() != 0 {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"task", "prune"}, nil, &out, &errOut); code != 8 || out.Len() != 0 {
		t.Fatalf("stub code = %d out=%q", code, out.String())
	}
	// The internal guardian token is dispatched before the CLI: an extra
	// argument is an invalid invocation (exit 2) with a bounded diagnostic,
	// never a CLI usage error.
	out.Reset()
	errOut.Reset()
	if code := run([]string{"__callsheet_task_guardian_v1", "extra"}, nil, &out, &errOut); code != 2 || out.Len() != 0 ||
		!strings.HasPrefix(errOut.String(), "callsheet task guardian: invalid invocation") {
		t.Fatalf("guardian token = %d out=%q err=%q", code, out.String(), errOut.String())
	}
}
