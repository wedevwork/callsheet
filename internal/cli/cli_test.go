package cli

import (
	"bytes"
	"context"
	"io"
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
	// Linux and macOS build the identical full tree.
	var darwin []string
	for _, l := range NewTree("darwin").Leaves() {
		darwin = append(darwin, l.Path())
	}
	if strings.Join(darwin, "|") != strings.Join(paths, "|") {
		t.Fatalf("darwin leaves differ from linux: %v", darwin)
	}
	for _, g := range []string{"plane", "sidecar"} {
		for _, goos := range []string{"linux", "darwin"} {
			if code, out, errOut := exec(t, goos, g); code != 0 || errOut != "" || !strings.HasPrefix(out, "Usage: callsheet "+g+" <command>") {
				t.Fatalf("%s %s = %d %q %q", goos, g, code, out, errOut)
			}
		}
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
	stubs := 0
	for _, leaf := range NewTree("linux").Leaves() {
		if leaf.implemented() {
			continue
		}
		stubs++
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
	// version and the four plane leaves are implemented; 27 stubs remain.
	if stubs != 27 {
		t.Fatalf("stubs = %d", stubs)
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

func TestUnsupportedOS(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	argSets := [][]string{nil, {"help"}, {"--help"}, {"-h"}, {"version"}, {"--version"}, {"plane"}, {"sidecar", "run"}, {"help", "plane"}, {"dispatch"}, {"bogus"}}
	for _, goos := range []string{"windows", "freebsd", ""} {
		if tree := NewTree(goos); tree != nil {
			t.Fatalf("NewTree(%q) = %v, want nil", goos, tree.Children)
		}
		want := "callsheet: invalid_argument: unsupported operating system \"" + goos + "\"; supported: linux, darwin\n"
		for _, args := range argSets {
			code, out, errOut := exec(t, goos, args...)
			if code != 2 || out != "" || errOut != want {
				t.Fatalf("%q %v = %d %q %q", goos, args, code, out, errOut)
			}
		}
		// A supported tree does not rescue an unsupported goos.
		var out, errOut bytes.Buffer
		if code := run(context.Background(), NewTree("linux"), goos, []string{"version"}, &out, &errOut); code != 2 || out.Len() != 0 || errOut.String() != want {
			t.Fatalf("%q with linux tree = %d %q %q", goos, code, out.String(), errOut.String())
		}
		// Cancellation keeps precedence over the platform rejection.
		out.Reset()
		errOut.Reset()
		if code := run(canceled, NewTree(goos), goos, []string{"version"}, &out, &errOut); code != 130 || out.Len() != 0 || errOut.String() != "callsheet: interrupted\n" {
			t.Fatalf("%q canceled = %d %q %q", goos, code, out.String(), errOut.String())
		}
	}
	// A nil root is rejected before it is dereferenced.
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, "linux", []string{"plane"}, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "unsupported operating system") {
		t.Fatalf("nil root = %d %q %q", code, out.String(), errOut.String())
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
	// Run uses the host tree; the supported test hosts have the plane group.
	out.Reset()
	errOut.Reset()
	code := Run(context.Background(), []string{"plane"}, nil, &out, &errOut)
	if code != 0 || errOut.Len() != 0 || !strings.HasPrefix(out.String(), "Usage: callsheet plane <command>") {
		t.Fatalf("host %s plane = %d %q %q", runtime.GOOS, code, out.String(), errOut.String())
	}
}

func TestMcpStubStdoutClean(t *testing.T) {
	code, out, errOut := exec(t, "linux", "mcp", "--plane", "https://x", "--ca", "y")
	if code != 8 || out != "" || !strings.Contains(errOut, "not implemented yet") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

// TestPlatformSeamContract is the FP-2 contract test for the CLI, executed by
// name from tests/function (TestHardeningPlatformSeams). Its linux and
// darwin subtests drive runFor, the seam behind Run, on any host. Do not
// rename or skip.
func TestPlatformSeamContract(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			for _, c := range []struct {
				name     string
				ctx      context.Context
				args     []string
				code     int
				out, err string
			}{
				{"help", context.Background(), nil, 0, "Usage: callsheet <command>\n", ""},
				{"version", context.Background(), []string{"version"}, 0, "callsheet dev protocol=1\n", ""},
				{"stub", context.Background(), []string{"task", "ls"}, 8, "", "callsheet: not_implemented: \"callsheet task ls\" is not implemented yet\n"},
				{"usage", context.Background(), []string{"bogus"}, 2, "", "callsheet: invalid_argument: unknown command \"bogus\" for \"callsheet\"\n"},
				{"cancel", canceled, []string{"version"}, 130, "", "callsheet: interrupted\n"},
			} {
				in := strings.NewReader("unread input")
				var out, errOut bytes.Buffer
				code := runFor(c.ctx, goos, c.args, in, &out, &errOut)
				if code != c.code || !strings.HasPrefix(out.String(), c.out) || (c.out == "" && out.Len() != 0) || !strings.HasPrefix(errOut.String(), c.err) || (c.err == "" && errOut.Len() != 0) {
					t.Fatalf("%s %s = %d %q %q", goos, c.name, code, out.String(), errOut.String())
				}
				if in.Len() != len("unread input") {
					t.Fatalf("%s %s consumed input", goos, c.name)
				}
			}
			// The full tree is selected from goos alone.
			var out bytes.Buffer
			if code := runFor(context.Background(), goos, []string{"sidecar"}, nil, &out, io.Discard); code != 0 || !strings.HasPrefix(out.String(), "Usage: callsheet sidecar <command>") {
				t.Fatalf("%s sidecar = %d %q", goos, code, out.String())
			}
		})
	}
	for _, goos := range []string{"windows", "freebsd", ""} {
		var out, errOut bytes.Buffer
		if code := runFor(context.Background(), goos, []string{"version"}, nil, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "unsupported operating system") {
			t.Fatalf("%q = %d %q", goos, code, errOut.String())
		}
		if code := runFor(canceled, goos, nil, nil, &out, &errOut); code != 130 {
			t.Fatalf("%q canceled = %d", goos, code)
		}
	}
}
