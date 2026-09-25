package cli

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
)

func exec(t *testing.T, goos string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), NewTree(goos), goos, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func names(cs []*Command) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func TestNewTreeSelection(t *testing.T) {
	want := "version plane sidecar role node dispatch task ws mcp"
	for _, goos := range []string{"linux", "darwin"} {
		if got := strings.Join(names(NewTree(goos).Children), " "); got != want {
			t.Fatalf("%s root = %q", goos, got)
		}
	}
	win := strings.Join(names(NewTree("windows").Children), " ")
	if win != "version role node dispatch task ws mcp" {
		t.Fatalf("windows root = %q", win)
	}
	var paths []string
	for _, l := range NewTree("linux").Leaves() {
		paths = append(paths, l.Path())
	}
	if len(paths) != 32 {
		t.Fatalf("leaf count = %d: %v", len(paths), paths)
	}
	for _, p := range []string{"callsheet plane cert reissue", "callsheet ws ref set", "callsheet dispatch", "callsheet mcp"} {
		found := false
		for _, q := range paths {
			found = found || q == p
		}
		if !found {
			t.Fatalf("missing leaf %s", p)
		}
	}
	if len(NewTree("linux").Groups()) != 9 {
		t.Fatalf("groups = %v", names(NewTree("linux").Groups()))
	}
	if NewTree("linux").Child("nope") != nil {
		t.Fatal("unexpected child")
	}
}

func TestRootHelpForms(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		code, out, errOut := exec(t, "linux", args...)
		if code != 0 || errOut != "" {
			t.Fatalf("%v: code=%d err=%q", args, code, errOut)
		}
		if !strings.HasPrefix(out, "Usage: callsheet <command>") || !strings.Contains(out, "future stub") {
			t.Fatalf("%v: out=%q", args, out)
		}
		for _, n := range []string{"version", "plane", "sidecar", "role", "node", "dispatch", "task", "ws", "mcp"} {
			if !strings.Contains(out, "\n  "+n+" ") {
				t.Fatalf("%v: missing child %s in %q", args, n, out)
			}
		}
	}
	// Deterministic output.
	_, a, _ := exec(t, "linux")
	_, b, _ := exec(t, "linux", "help")
	if a != b {
		t.Fatal("help output not deterministic")
	}
}

func TestGroupAndLeafHelp(t *testing.T) {
	cases := [][]string{{"task"}, {"help", "task"}, {"task", "--help"}, {"task", "-h"}}
	for _, args := range cases {
		code, out, errOut := exec(t, "linux", args...)
		if code != 0 || errOut != "" || !strings.HasPrefix(out, "Usage: callsheet task <command>") {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
		for _, n := range []string{"ls", "show", "logs", "cancel", "wait", "prune"} {
			if !strings.Contains(out, "\n  "+n+" ") {
				t.Fatalf("missing %s", n)
			}
		}
	}
	code, out, _ := exec(t, "linux", "help", "ws", "ref", "set")
	if code != 0 || !strings.HasPrefix(out, "Usage: callsheet ws ref set\n") || !strings.Contains(out, "not implemented yet") {
		t.Fatalf("leaf help: %d %q", code, out)
	}
	code, out, _ = exec(t, "linux", "task", "ls", "--help")
	if code != 0 || !strings.HasPrefix(out, "Usage: callsheet task ls\n") {
		t.Fatalf("leaf --help: %d %q", code, out)
	}
	code, out, _ = exec(t, "linux", "mcp", "-h")
	if code != 0 || !strings.HasPrefix(out, "Usage: callsheet mcp\n") {
		t.Fatalf("mcp -h: %d %q", code, out)
	}
	code, out, _ = exec(t, "linux", "help", "version")
	if code != 0 || !strings.Contains(out, "Status: implemented.") {
		t.Fatalf("version help: %d %q", code, out)
	}
	code, out, _ = exec(t, "linux", "plane", "cert")
	if code != 0 || !strings.Contains(out, "reissue") {
		t.Fatalf("nested group: %d %q", code, out)
	}
}

func TestStubLeaves(t *testing.T) {
	for _, leaf := range NewTree("linux").Leaves() {
		if leaf.Name == "version" {
			continue
		}
		args := strings.Fields(strings.TrimPrefix(leaf.Path(), "callsheet "))
		code, out, errOut := exec(t, "linux", append(args, "opaque", "--future-flag", "x")...)
		if code != 8 || out != "" {
			t.Fatalf("%v: code=%d out=%q", args, code, out)
		}
		want := "callsheet: not_implemented: \"" + leaf.Path() + "\" is not implemented yet\n"
		if errOut != want {
			t.Fatalf("%v: stderr=%q want %q", args, errOut, want)
		}
	}
	// Help after "--" is opaque and not honoured.
	code, _, _ := exec(t, "linux", "task", "ls", "--", "--help")
	if code != 8 {
		t.Fatalf("after -- code=%d", code)
	}
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, out, errOut := exec(t, "linux", args...)
		if code != 0 || out != "callsheet dev protocol=1\n" || errOut != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	old := Version
	Version = "1.2.3"
	defer func() { Version = old }()
	_, out, _ := exec(t, "linux", "version")
	if out != "callsheet 1.2.3 protocol=1\n" {
		t.Fatalf("ldflags version: %q", out)
	}
	code, _, errOut := exec(t, "linux", "version", "extra")
	if code != 2 || !strings.Contains(errOut, "invalid_argument") {
		t.Fatalf("version extra: %d %q", code, errOut)
	}
	code, out, _ = exec(t, "linux", "--version", "--help")
	if code != 0 || !strings.HasPrefix(out, "Usage: callsheet version") {
		t.Fatalf("--version --help: %d %q", code, out)
	}
}

func TestInvalidCommands(t *testing.T) {
	cases := []struct {
		args []string
		msg  string
	}{
		{[]string{"bogus"}, `unknown command "bogus" for "callsheet"`},
		{[]string{"task", "bogus"}, `unknown command "bogus" for "callsheet task"`},
		{[]string{"--bogus"}, `unknown flag "--bogus" for "callsheet"`},
		{[]string{"task", "--state-dir", "x", "ls"}, `unknown flag "--state-dir" for "callsheet task"`},
		{[]string{"-v"}, `unknown flag "-v"`},
		{[]string{"help", "bogus"}, `unknown command "bogus"`},
		{[]string{"help", "--x"}, `unknown flag "--x" for help`},
		{[]string{"task", "--version"}, `unknown flag "--version"`},
	}
	for _, c := range cases {
		code, out, errOut := exec(t, "linux", c.args...)
		if code != 2 || out != "" {
			t.Fatalf("%v: code=%d out=%q", c.args, code, out)
		}
		if !strings.HasPrefix(errOut, "callsheet: invalid_argument: ") || !strings.Contains(errOut, c.msg) || !strings.Contains(errOut, "Usage: ") {
			t.Fatalf("%v: stderr=%q", c.args, errOut)
		}
	}
}

func TestWindowsRejectsUnixOnly(t *testing.T) {
	for _, g := range []string{"plane", "sidecar"} {
		for _, args := range [][]string{{g}, {g, "run"}, {"help", g}} {
			code, out, errOut := exec(t, "windows", args...)
			if code != 2 || out != "" || !strings.Contains(errOut, "Linux and macOS only") {
				t.Fatalf("%v: %d %q %q", args, code, out, errOut)
			}
		}
	}
	code, out, _ := exec(t, "windows")
	if code != 0 || strings.Contains(out, "\n  plane") || !strings.Contains(out, "Linux and macOS only") {
		t.Fatalf("windows help: %q", out)
	}
	code, _, _ = exec(t, "windows", "dispatch")
	if code != 8 {
		t.Fatalf("windows dispatch = %d", code)
	}
	// On Linux, sidecar is a group, not an error.
	code, _, _ = exec(t, "linux", "sidecar")
	if code != 0 {
		t.Fatalf("linux sidecar = %d", code)
	}
}

func TestInterruptedAndPublicRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if code := Run(ctx, []string{"version"}, nil, &out, &errOut); code != 130 {
		t.Fatalf("cancelled ctx = %d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := Run(context.Background(), []string{"version"}, strings.NewReader(""), &out, &errOut); code != 0 || out.String() != "callsheet dev protocol=1\n" {
		t.Fatalf("Run = %d %q", code, out.String())
	}
	// Run uses the host tree.
	code := Run(context.Background(), []string{"plane"}, nil, &out, &errOut)
	if runtime.GOOS == "windows" && code != 2 || runtime.GOOS != "windows" && code != 0 {
		t.Fatalf("host plane = %d", code)
	}
}

func TestMcpStubStdoutClean(t *testing.T) {
	code, out, errOut := exec(t, "linux", "mcp", "--plane", "https://x", "--ca", "y")
	if code != 8 || out != "" || !strings.Contains(errOut, "not implemented yet") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}
