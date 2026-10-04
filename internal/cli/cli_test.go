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
	code, out, _ := exec(t, "linux", "help", "task", "prune")
	if code != 0 || !strings.HasPrefix(out, "Usage: callsheet task prune\n") || !strings.Contains(out, "not implemented yet") {
		t.Fatalf("leaf help: %d %q", code, out)
	}
	code, out, _ = exec(t, "linux", "task", "prune", "--help")
	if code != 0 || !strings.HasPrefix(out, "Usage: callsheet task prune\n") || !strings.Contains(out, "not implemented yet") {
		t.Fatalf("leaf --help: %d %q", code, out)
	}
	// Iteration 05: the four task leaves are implemented; iteration 06b
	// implements cancel and wait; prune remains a reserved stub.
	code, out, _ = exec(t, "linux", "task")
	for _, name := range []string{"ls", "show", "logs"} {
		if !strings.Contains(out, "\n  "+name+" ") || code != 0 {
			t.Fatalf("task group lacks %s: %q", name, out)
		}
	}
	for _, name := range []string{"ls", "show", "logs"} {
		if !strings.Contains(out, name+" "+strings.Repeat(" ", 6-len(name))) && !strings.Contains(out, "[implemented]") {
			t.Fatalf("task %s not implemented: %q", name, out)
		}
	}
	for _, leaf := range [][]string{{"dispatch"}, {"task", "ls"}, {"task", "show"}, {"task", "logs"}, {"task", "cancel"}, {"task", "wait"}} {
		code, help, _ := exec(t, "linux", append(leaf, "--help")...)
		if code != 0 || !strings.Contains(help, "Status: implemented.") || strings.Contains(help, "future stub") {
			t.Fatalf("%v help: %d %q", leaf, code, help)
		}
	}
	for _, name := range []string{"prune"} {
		code, help, _ := exec(t, "linux", "task", name, "--help")
		if code != 0 || !strings.Contains(help, "Status: future stub; not implemented yet (exits 8).") {
			t.Fatalf("task %s help: %d %q", name, code, help)
		}
	}
	code, out, _ = exec(t, "linux", "mcp", "-h")
	if code != 0 || !strings.HasPrefix(out, "Usage: callsheet mcp --plane URL (--ca FILE | --ca-fingerprint SHA256) [--wait-call-budget DURATION]\n") ||
		!strings.Contains(out, "Status: implemented.") {
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
	// version, the four plane leaves (iteration 02), sidecar enroll and run
	// and node ls and show (iteration 03), the five role leaves (iteration
	// 04), dispatch and task ls, show and logs (iteration 05) and task
	// cancel and wait (iteration 06b), mcp (iteration 07a), the eight
	// workspace leaves (iteration 09a) and ws push and pull (iteration 09b)
	// are implemented; 1 stub remains (task prune).
	if stubs != 1 {
		t.Fatalf("stubs = %d", stubs)
	}
	// The role group lists its five leaves as implemented, each leaf's help
	// says so, and none is a stub any more.
	code, out, _ := exec(t, "linux", "role")
	if code != 0 || strings.Contains(out, "future stub") {
		t.Fatalf("role group help: %d %q", code, out)
	}
	for _, name := range []string{"add", "set", "ls", "show", "rm"} {
		if !strings.Contains(out, "  "+name+" ") || !strings.Contains(out, "[implemented]") {
			t.Fatalf("role group help lacks %s: %q", name, out)
		}
		code, help, _ := exec(t, "linux", "role", name, "--help")
		if code != 0 || !strings.HasPrefix(help, "Usage: callsheet role "+name+" ") || !strings.Contains(help, "Status: implemented.") || strings.Contains(help, "future stub") {
			t.Fatalf("role %s help: %d %q", name, code, help)
		}
	}
	// Root help still describes the stub convention for the other leaves.
	if _, root, _ := exec(t, "linux", "help"); !strings.Contains(root, "[future stub] commands are reserved and exit 8") || !strings.Contains(root, "  role      Role commands [group]") {
		t.Fatalf("root help: %q", root)
	}
	// Help after "--" is opaque and not honoured.
	code, _, _ = exec(t, "linux", "task", "prune", "--", "--help")
	if code != 8 {
		t.Fatalf("after -- code=%d", code)
	}
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, out, errOut := exec(t, "linux", args...)
		if code != 0 || out != "callsheet dev protocol=6\n" || errOut != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	old := Version
	Version = "1.2.3"
	defer func() { Version = old }()
	_, out, _ := exec(t, "linux", "version")
	if out != "callsheet 1.2.3 protocol=6\n" {
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
	if code := Run(context.Background(), []string{"version"}, strings.NewReader(""), &out, &errOut); code != 0 || out.String() != "callsheet dev protocol=6\n" {
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

// TestMCPLeafStdoutClean: the implemented mcp leaf (iteration 07a) with
// no input ends at once (EOF, exit 0) and writes nothing to stdout or
// stderr; it never contacts the plane.
func TestMCPLeafStdoutClean(t *testing.T) {
	code, out, errOut := exec(t, "linux", "mcp", "--plane", "https://x", "--ca", "y")
	if code != 0 || out != "" || errOut != "" {
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
				{"version", context.Background(), []string{"version"}, 0, "callsheet dev protocol=6\n", ""},
				{"stub", context.Background(), []string{"task", "prune"}, 8, "", "callsheet: not_implemented: \"callsheet task prune\" is not implemented yet\n"},
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
