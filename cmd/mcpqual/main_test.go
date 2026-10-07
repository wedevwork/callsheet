package main

import (
	"bytes"
	"strings"
	"testing"
)

// FP-16/FP-15: the thin entrypoint forwards the host OS and maps the
// developer fault variable, the only place it is read.
func TestRunWrapper(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "mcpqual ") {
		t.Fatalf("version = %d %q %q", code, out.String(), errOut.String())
	}
}

func TestFaultVariable(t *testing.T) {
	for _, tc := range []struct {
		fault string
		code  int
		err   string
	}{
		{"", 0, ""},
		{"probe-error", 0, ""},
		{"kill-everything", 2, `unsupported MCPQUAL_TEST_FAULT "kill-everything"`},
	} {
		var out, errOut bytes.Buffer
		getenv := func(k string) string {
			if k == "MCPQUAL_TEST_FAULT" {
				return tc.fault
			}
			return ""
		}
		code := runFor("linux", "amd64", []string{"version"}, getenv, strings.NewReader(""), &out, &errOut)
		if code != tc.code || !strings.Contains(errOut.String(), tc.err) {
			t.Errorf("%q = %d %q", tc.fault, code, errOut.String())
		}
	}
	// An unsupported host reaches the qualify refusal, not a launch.
	var out, errOut bytes.Buffer
	code := runFor("windows", "amd64", []string{"qualify", "--plan", "/p.json", "--out", "/o"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "linux or darwin only") {
		t.Fatalf("windows = %d %q", code, errOut.String())
	}
}
