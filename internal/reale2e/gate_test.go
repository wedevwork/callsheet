package reale2e

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestManualGate (FP-1): Main refuses live startup whenever CI is present
// (even empty), the opt-in is absent or not exactly "1", or the supplied
// host OS is not linux, with exit 2, the fixed refusal and the bounds, and
// without building the world: no filesystem, executable or network
// activity happens.
func TestManualGate(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	for _, tc := range []struct {
		name string
		goos string
		env  map[string]string
		ci   bool
		want error
	}{
		{"no opt-in", "linux", nil, false, ErrOptIn},
		{"opt-in not 1", "linux", map[string]string{EnvOptIn: "yes"}, false, ErrOptIn},
		{"present-empty CI", "linux", map[string]string{EnvOptIn: "1"}, true, ErrCI},
		{"CI without opt-in", "linux", nil, true, ErrOptIn},
		{"darwin", "darwin", map[string]string{EnvOptIn: "1"}, false, ErrPlatform},
		{"windows", "windows", map[string]string{EnvOptIn: "1"}, false, ErrPlatform},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			built := false
			h := Host{GOOS: tc.goos, GOARCH: "amd64",
				Args:      []string{"run", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", cwd},
				Getenv:    func(k string) string { return tc.env[k] },
				LookupEnv: func(k string) (string, bool) { return "", k == EnvCI && tc.ci },
				Stdout:    &out, Stderr: &errOut,
				NewWorld: func(Host) (*World, func(), error) { built = true; return nil, nil, nil }}
			if code := MainWith(h); code != 2 || built || !strings.Contains(errOut.String(), tc.want.Error()) || !strings.Contains(errOut.String(), "Cost and time bounds") || out.Len() != 0 {
				t.Fatalf("code %d built %v out %q err %q", code, built, out.String(), errOut.String())
			}
			if es, _ := os.ReadDir(cwd); len(es) != 0 {
				t.Fatalf("the refused startup wrote %v", es)
			}
		})
	}
	// Main itself wires the injected lookup: a present-empty CI refuses.
	var out, errOut bytes.Buffer
	code := Main("linux", "amd64", []string{"run"}, func(k string) string { return map[string]string{EnvOptIn: "1"}[k] },
		func(k string) (string, bool) { return "", k == EnvCI }, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), ErrCI.Error()) {
		t.Fatalf("Main = %d %q", code, errOut.String())
	}
	// Past the gate, missing or relative paths are usage errors, still
	// before any world exists.
	for _, args := range [][]string{{"run"}, {"run", "--callsheet", "rel", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
		{"run", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e", "--flow", "rel"},
		{"run", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e", "positional"}} {
		var out, errOut bytes.Buffer
		h := Host{GOOS: "linux", Args: args, Getenv: func(k string) string { return map[string]string{EnvOptIn: "1"}[k] },
			LookupEnv: func(string) (string, bool) { return "", false }, Stdout: &out, Stderr: &errOut,
			NewWorld: func(Host) (*World, func(), error) { t.Fatal("world built"); return nil, nil, nil }}
		if code := MainWith(h); code != 2 || !strings.Contains(errOut.String(), "usage:") {
			t.Errorf("%v = %d %q", args, code, errOut.String())
		}
	}
}

// TestGateUnits (UT-1): the gate's order (opt-in, CI presence, then the
// injected OS) and the command's usage paths, none with side effects.
func TestGateUnits(t *testing.T) {
	t.Parallel()
	on := func(k string) string { return map[string]string{EnvOptIn: "1"}[k] }
	none := func(string) (string, bool) { return "", false }
	if err := Gate("linux", on, none); err != nil {
		t.Fatal(err)
	}
	if err := Gate("plan9", func(string) string { return "" }, func(string) (string, bool) { return "", true }); err != ErrOptIn {
		t.Fatalf("order: %v", err)
	}
	if err := Gate("plan9", on, func(string) (string, bool) { return "", true }); err != ErrCI {
		t.Fatalf("order: %v", err)
	}
	for _, args := range [][]string{nil, {"bogus"}, {"observe"}, {"observe", "--run", "/r", "--task", "bad", "--hop", HopDesigner},
		{"observe", "--run", "/r", "--task", hexID("t_", 1), "--hop", "planner"}, {"finish"}, {"finish", "--run", "rel"}} {
		var out, errOut bytes.Buffer
		h := Host{GOOS: "linux", Args: args, Getenv: on, LookupEnv: none, Stdout: &out, Stderr: &errOut,
			NewWorld: func(Host) (*World, func(), error) { t.Fatal("world built"); return nil, nil, nil }}
		if code := MainWith(h); code != 2 || !strings.Contains(errOut.String(), "usage:") {
			t.Errorf("%v = %d %q", args, code, errOut.String())
		}
	}
	// A world that cannot be built fails the run (exit 1).
	var out, errOut bytes.Buffer
	h := Host{GOOS: "linux", Args: []string{"run", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
		Getenv: on, LookupEnv: none, Stdout: &out, Stderr: &errOut,
		NewWorld: func(Host) (*World, func(), error) { return nil, nil, os.ErrPermission }}
	if code := MainWith(h); code != 1 || !strings.Contains(errOut.String(), "permission") {
		t.Fatalf("world failure = %d %q", code, errOut.String())
	}
}
