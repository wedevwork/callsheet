package main

import (
	"bytes"
	"os"
	"reflect"
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

// noCI replaces the presence lookup for the duration of t, so a hosted
// runner's own CI variable cannot decide these tests.
func noCI(t *testing.T) {
	saved := lookupEnv
	lookupEnv = func(string) (string, bool) { return "", false }
	t.Cleanup(func() { lookupEnv = saved })
}

// Design decoder-enrollment: the production lookup is the presence lookup
// behind the CI prohibition of qualify and capture.
func TestLookupEnvWiring(t *testing.T) {
	if reflect.ValueOf(lookupEnv).Pointer() != reflect.ValueOf(os.LookupEnv).Pointer() {
		t.Fatal("lookupEnv is not os.LookupEnv")
	}
	saved := lookupEnv
	defer func() { lookupEnv = saved }()
	lookupEnv = func(k string) (string, bool) { return "", k == "CI" }
	for _, cmd := range []string{"qualify", "capture"} {
		var out, errOut bytes.Buffer
		code := runFor("linux", "amd64", []string{cmd, "--plan", "/p.json", "--out", "/o", "--allow-model-calls"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), cmd+" refused: CI is set") {
			t.Fatalf("%s with an empty CI = %d %q", cmd, code, errOut.String())
		}
	}
}

func TestFaultVariable(t *testing.T) {
	noCI(t)
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
