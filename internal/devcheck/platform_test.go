package devcheck

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestPlatformSourceGuard scans the actual repository on every host: every
// non-test runtime.GOOS read must be one of the approved wrappers.
//
// Guard policy (the single source is platformGuardPolicy in platform.go):
//
//	file                                          wrapper    required body
//	internal/cli/cli.go                           Run        return runFor(ctx, runtime.GOOS, args, in, out, errOut)
//	internal/devcheck/devcheck.go                 Run        return runFor(ctx, runtime.GOOS, args, out, errOut, run)
//	internal/spikes/processgroup/experiment.go    evaluate   evaluateFor(r, runtime.GOOS)
//	internal/spikes/processgroup/experiment.go    RunHelper  return runHelperFor(getenv, runtime.GOOS, runtime.GOARCH)
//	internal/testkit/fakeadapter/fakeadapter.go   Parse      return parseFor(args, runtime.GOOS, signalsSupported)
//
//	exempt native file                              //go:build
//	internal/spikes/processgroup/sys_linux.go       linux
//	internal/spikes/processgroup/sys_darwin.go      darwin
//	internal/testkit/fakeadapter/signals_unix.go    linux || darwin
//	internal/testkit/fakeadapter/signals_other.go   !linux && !darwin
func TestPlatformSourceGuard(t *testing.T) {
	if err := CheckPlatformSources(testkit.MustRepoRoot(t)); err != nil {
		t.Fatal(err)
	}
}

// validPolicyTree is a complete miniature source tree that satisfies the
// production policy.
func validPolicyTree() map[string]string {
	return map[string]string{
		"internal/cli/cli.go": `package cli

import (
	"context"
	"io"
	"runtime"
)

// Run is the approved wrapper; runtime.GOOS in a comment is not a read.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return runFor(ctx, runtime.GOOS, args, in, out, errOut)
}

func runFor(ctx context.Context, goos string, args []string, in io.Reader, out, errOut io.Writer) int {
	_ = "runtime.GOOS"
	return len(goos)
}
`,
		"internal/devcheck/devcheck.go": `package devcheck

import (
	"context"
	"io"
	"runtime"
)

type Runner func()

func Run(ctx context.Context, args []string, out, errOut io.Writer, run Runner) int {
	return runFor(ctx, runtime.GOOS, args, out, errOut, run)
}

func runFor(ctx context.Context, goos string, args []string, out, errOut io.Writer, run Runner) int { return 0 }

func label() string { return runtime.GOARCH }
`,
		"internal/spikes/processgroup/experiment.go": `//go:build linux || darwin

package processgroup

import "runtime"

type CaseResult struct{}

func evaluate(r *CaseResult) { evaluateFor(r, runtime.GOOS) }

func evaluateFor(r *CaseResult, goos string) {}

func RunHelper(getenv func(string) string) int {
	return runHelperFor(getenv, runtime.GOOS, runtime.GOARCH)
}

func runHelperFor(getenv func(string) string, goos, goarch string) int { return 0 }
`,
		"internal/testkit/fakeadapter/fakeadapter.go": `package fakeadapter

import "runtime"

type Options struct{}

func Parse(args []string) (Options, error) {
	return parseFor(args, runtime.GOOS, signalsSupported)
}

func parseFor(args []string, goos string, supported bool) (Options, error) {
	runtime.KeepAlive(args)
	return Options{}, nil
}
`,
		"internal/spikes/processgroup/sys_linux.go":     "//go:build linux\n\npackage processgroup\n",
		"internal/spikes/processgroup/sys_darwin.go":    "//go:build darwin\n\npackage processgroup\n",
		"internal/testkit/fakeadapter/signals_unix.go":  "//go:build linux || darwin\n\npackage fakeadapter\n\nconst signalsSupported = true\n",
		"internal/testkit/fakeadapter/signals_other.go": "//go:build !linux && !darwin\n\npackage fakeadapter\n\nconst signalsSupported = false\n",
		"root.go":            "package root\n",
		"cmd/tool/main.go":   "package main\n\nimport \"go/build\"\n\nvar runtime = build.Default\n\nvar _ = runtime.GOOS // another package's GOOS field, no runtime import\n",
		"cmd/tool/blank.go":  "package main\n\nimport _ \"runtime\"\n",
		"cmd/tool/x_test.go": "package main\n\nimport \"runtime\"\n\nvar _ = runtime.GOOS\n",
	}
}

const hostRead = "package x\n\nimport \"runtime\"\n\nfunc host() string { return runtime.GOOS }\n"

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func policyTree(t *testing.T, edit func(files map[string]string)) string {
	t.Helper()
	files := validPolicyTree()
	if edit != nil {
		edit(files)
	}
	root := t.TempDir()
	writeTree(t, root, files)
	return root
}

func guardViolations(t *testing.T, root string) []string {
	t.Helper()
	err := CheckPlatformSources(root)
	if err == nil {
		return nil
	}
	var ge *PlatformGuardError
	if !errors.As(err, &ge) {
		t.Fatalf("error type %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "docs/ci.md, Platform code") || !strings.Contains(err.Error(), "explicit goos parameter") {
		t.Fatalf("diagnostic lacks the convention: %v", err)
	}
	return ge.Violations
}

// requireViolations checks that the violations are exactly want, in order,
// each given as a prefix.
func requireViolations(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d violations %q, want %q", name, len(got), got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("%s: violation %d = %q, want prefix %q\nall: %q", name, i, got[i], want[i], got)
		}
	}
}

func TestPlatformGuardValidTree(t *testing.T) {
	if v := guardViolations(t, policyTree(t, nil)); v != nil {
		t.Fatalf("valid policy tree: %q", v)
	}
	// Skipped trees, test files and formatting-only build expression
	// changes of exempt files are accepted; exempt files may read GOOS.
	root := policyTree(t, func(f map[string]string) {
		for _, dir := range []string{".git", ".agents", ".codex", ".claude", ".github", ".idea", "design", "vendor", "testdata", "internal/x/testdata", "internal/.cache"} {
			f[dir+"/read.go"] = hostRead
		}
		f["internal/x/read_test.go"] = hostRead
		f["internal/testkit/fakeadapter/signals_unix.go"] = "//go:build linux||darwin\n\npackage fakeadapter\n\nimport \"runtime\"\n\nconst signalsSupported = true\n\nvar native = runtime.GOOS\n"
		f["internal/spikes/processgroup/sys_linux.go"] = "// Copyright notice.\n\n//go:build (linux)\n\npackage processgroup\n"
		f["internal/testkit/fakeadapter/signals_other.go"] = "//go:build !linux&&!darwin\n\npackage fakeadapter\n\nconst signalsSupported = false\n"
	})
	if v := guardViolations(t, root); v != nil {
		t.Fatalf("skipped/exempt content flagged: %q", v)
	}
}

func TestPlatformGuardRejectsReads(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(map[string]string)
		want []string
	}{
		{"new default read", func(f map[string]string) { f["internal/x/x.go"] = hostRead },
			[]string{"internal/x/x.go:5:29: unapproved host OS read runtime.GOOS in func host"}},
		{"aliased import", func(f map[string]string) {
			f["internal/x/x.go"] = "package x\n\nimport rt \"runtime\"\n\nfunc linux() bool { return rt.GOOS == \"linux\" }\n"
		}, []string{"internal/x/x.go:5:28: unapproved host OS read rt.GOOS in func linux"}},
		{"package-level alias", func(f map[string]string) {
			f["internal/x/x.go"] = "package x\n\nimport \"runtime\"\n\nvar host = runtime.GOOS\n"
		}, []string{"internal/x/x.go:5:12: unapproved host OS read runtime.GOOS in a package-level declaration"}},
		{"closure", func(f map[string]string) {
			f["internal/x/x.go"] = "package x\n\nimport \"runtime\"\n\nfunc f() func() string {\n\treturn func() string { return runtime.GOOS }\n}\n"
		}, []string{"internal/x/x.go:6:32: unapproved host OS read runtime.GOOS in func f"}},
		{"same-named method", func(f map[string]string) {
			f["internal/cli/cli.go"] += "\ntype T struct{}\n\nfunc (*T) Run() string { return runtime.GOOS }\n"
		}, []string{"internal/cli/cli.go:21:33: unapproved host OS read runtime.GOOS in method T.Run"}},
		{"shadowed import name", func(f map[string]string) {
			f["internal/x/x.go"] = "package x\n\nimport \"runtime\"\n\nvar _ = runtime.NumCPU\n\nfunc f() string {\n\truntime := struct{ GOOS string }{}\n\treturn runtime.GOOS\n}\n"
		}, []string{"internal/x/x.go:9:9: unapproved host OS read runtime.GOOS in func f"}},
		{"dot import", func(f map[string]string) {
			f["internal/x/x.go"] = "package x\n\nimport . \"runtime\"\n\nfunc f() string { return GOOS }\n"
		}, []string{"internal/x/x.go:3:8: dot import of runtime is not allowed"}},
		{"dot import without GOOS", func(f map[string]string) {
			f["internal/x/x.go"] = "package x\n\nimport . \"runtime\"\n\nvar _ = NumCPU\n"
		}, []string{"internal/x/x.go:3:8: dot import of runtime is not allowed"}},
		{"unsupported build tag", func(f map[string]string) {
			f["internal/x/x_freebsd.go"] = "//go:build freebsd\n\n" + hostRead
		}, []string{"internal/x/x_freebsd.go:7:29: unapproved host OS read"}},
		{"darwin-only and linux-only files", func(f map[string]string) {
			f["internal/x/x_darwin.go"] = "//go:build darwin\n\n" + hostRead
			f["internal/x/x_linux.go"] = hostRead
		}, []string{"internal/x/x_darwin.go:7:29: unapproved host OS read", "internal/x/x_linux.go:5:29: unapproved host OS read"}},
		{"generated source", func(f map[string]string) {
			f["internal/x/zz_generated.go"] = "// Code generated by x. DO NOT EDIT.\n\n" + hostRead
		}, []string{"internal/x/zz_generated.go:7:29: unapproved host OS read"}},
		{"untagged file in exempt package", func(f map[string]string) {
			f["internal/spikes/processgroup/sys_extra.go"] = "//go:build linux\n\npackage processgroup\n\nimport \"runtime\"\n\nvar _ = runtime.GOOS\n"
		}, []string{"internal/spikes/processgroup/sys_extra.go:7:9: unapproved host OS read"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireViolations(t, c.name, guardViolations(t, policyTree(t, c.edit)), c.want...)
		})
	}
}

func TestPlatformGuardWrapperShapes(t *testing.T) {
	const cliShape = "internal/cli/cli.go:10:1: approved wrapper Run must be exactly \"return runFor(ctx, runtime.GOOS, args, in, out, errOut)\""
	replaceCLI := func(body string) func(map[string]string) {
		return func(f map[string]string) {
			f["internal/cli/cli.go"] = strings.Replace(f["internal/cli/cli.go"], "\treturn runFor(ctx, runtime.GOOS, args, in, out, errOut)\n", body, 1)
		}
	}
	for _, c := range []struct {
		name string
		edit func(map[string]string)
		want []string
	}{
		{"branch in wrapper", replaceCLI("\tif runtime.GOOS == \"plan9\" {\n\t\treturn 1\n\t}\n\treturn runFor(ctx, runtime.GOOS, args, in, out, errOut)\n"),
			[]string{cliShape + " (no branches", "internal/cli/cli.go:11:5: unapproved host OS read runtime.GOOS in func Run", "internal/cli/cli.go:14:21: unapproved host OS read"}},
		{"host alias in wrapper", replaceCLI("\tgoos := runtime.GOOS\n\treturn runFor(ctx, goos, args, in, out, errOut)\n"),
			[]string{cliShape + " (no branches, extra statements, closures or host aliases): the body must be a single statement", "internal/cli/cli.go:11:10: unapproved host OS read"}},
		{"extra read in wrapper call", replaceCLI("\treturn runFor(ctx, runtime.GOOS, args, in, out, errOut) + len(runtime.GOOS)\n"),
			[]string{cliShape + " (no branches, extra statements, closures or host aliases): the statement must be a direct call", "internal/cli/cli.go:11:21: unapproved host OS read", "internal/cli/cli.go:11:64: unapproved host OS read"}},
		{"reordered arguments", replaceCLI("\treturn runFor(ctx, runtime.GOOS, args, in, errOut, out)\n"),
			[]string{cliShape + " (no branches, extra statements, closures or host aliases): argument 5 must forward out", "internal/cli/cli.go:11:21: unapproved host OS read"}},
		{"goos moved", replaceCLI("\treturn runFor(runtime.GOOS, ctx, args, in, out, errOut)\n"),
			[]string{cliShape + " (no branches, extra statements, closures or host aliases): argument 1 must forward ctx", "internal/cli/cli.go:11:16: unapproved host OS read"}},
		{"other callee", replaceCLI("\treturn run(ctx, runtime.GOOS, args, in, out, errOut)\n"),
			[]string{cliShape + " (no branches, extra statements, closures or host aliases): it must call runFor directly", "internal/cli/cli.go:11:18: unapproved host OS read"}},
		{"closure in wrapper", replaceCLI("\treturn func() int { return runFor(ctx, runtime.GOOS, args, in, out, errOut) }()\n"),
			[]string{cliShape + " (no branches, extra statements, closures or host aliases): it must call runFor directly", "internal/cli/cli.go:11:41: unapproved host OS read"}},
		{"not a return", replaceCLI("\trunFor(ctx, runtime.GOOS, args, in, out, errOut)\n\treturn 0\n"),
			[]string{cliShape + " (no branches, extra statements, closures or host aliases): the body must be a single statement", "internal/cli/cli.go:11:14: unapproved host OS read"}},
		{"wrong argument count", func(f map[string]string) {
			f["internal/devcheck/devcheck.go"] = strings.Replace(f["internal/devcheck/devcheck.go"], "errOut, run)\n}", "errOut, run, nil)\n}", 1)
		}, []string{"internal/devcheck/devcheck.go:11:1: approved wrapper Run must be exactly \"return runFor(ctx, runtime.GOOS, args, out, errOut, run)\" (no branches, extra statements, closures or host aliases): it must pass 6 arguments, got 7", "internal/devcheck/devcheck.go:12:21: unapproved"}},
		{"evaluate returns", func(f map[string]string) {
			f["internal/spikes/processgroup/experiment.go"] = strings.Replace(f["internal/spikes/processgroup/experiment.go"],
				"func evaluate(r *CaseResult) { evaluateFor(r, runtime.GOOS) }", "func evaluate(r *CaseResult) { return; evaluateFor(r, runtime.GOOS) }", 1)
		}, []string{"internal/spikes/processgroup/experiment.go:9:1: approved wrapper evaluate must be exactly \"evaluateFor(r, runtime.GOOS)\"", "internal/spikes/processgroup/experiment.go:9:55: unapproved"}},
		{"GOARCH replaced", func(f map[string]string) {
			f["internal/spikes/processgroup/experiment.go"] = strings.Replace(f["internal/spikes/processgroup/experiment.go"], "runtime.GOOS, runtime.GOARCH)", "runtime.GOOS, runtime.GOOS)", 1)
		}, []string{"internal/spikes/processgroup/experiment.go:13:1: approved wrapper RunHelper must be exactly \"return runHelperFor(getenv, runtime.GOOS, runtime.GOARCH)\" (no branches, extra statements, closures or host aliases): argument 3 must be runtime.GOARCH",
			"internal/spikes/processgroup/experiment.go:14:30: unapproved", "internal/spikes/processgroup/experiment.go:14:44: unapproved"}},
		{"support constant replaced", func(f map[string]string) {
			f["internal/testkit/fakeadapter/fakeadapter.go"] = strings.Replace(f["internal/testkit/fakeadapter/fakeadapter.go"], "runtime.GOOS, signalsSupported)", "runtime.GOOS, true)", 1)
		}, []string{"internal/testkit/fakeadapter/fakeadapter.go:7:1: approved wrapper Parse must be exactly \"return parseFor(args, runtime.GOOS, signalsSupported)\" (no branches, extra statements, closures or host aliases): argument 3 must forward signalsSupported", "internal/testkit/fakeadapter/fakeadapter.go:8:24: unapproved"}},
		{"forwarded non-parameter", func(f map[string]string) {
			f["internal/testkit/fakeadapter/fakeadapter.go"] = strings.Replace(f["internal/testkit/fakeadapter/fakeadapter.go"], "func Parse(args []string)", "func Parse(argv []string)", 1)
		}, []string{"internal/testkit/fakeadapter/fakeadapter.go:7:1: approved wrapper Parse must be exactly", "internal/testkit/fakeadapter/fakeadapter.go:8:24: unapproved"}},
		{"runtime not imported", func(f map[string]string) {
			f["internal/testkit/fakeadapter/fakeadapter.go"] = strings.Replace(f["internal/testkit/fakeadapter/fakeadapter.go"], "import \"runtime\"", "import runtime \"go/build\"", 1)
		}, []string{"internal/testkit/fakeadapter/fakeadapter.go:7:1: approved wrapper Parse must be exactly \"return parseFor(args, runtime.GOOS, signalsSupported)\" (no branches, extra statements, closures or host aliases): argument 2 must be runtime.GOOS"}},
		{"missing wrapper", func(f map[string]string) {
			f["internal/testkit/fakeadapter/fakeadapter.go"] = strings.Replace(f["internal/testkit/fakeadapter/fakeadapter.go"], "func Parse(", "func ParseArgs(", 1)
		}, []string{"internal/testkit/fakeadapter/fakeadapter.go: approved wrapper Parse is missing", "internal/testkit/fakeadapter/fakeadapter.go:8:24: unapproved host OS read runtime.GOOS in func ParseArgs"}},
		{"moved wrapper file", func(f map[string]string) {
			f["internal/cli/run.go"] = f["internal/cli/cli.go"]
			delete(f, "internal/cli/cli.go")
		}, []string{"internal/cli/cli.go: approved wrapper Run is missing", "internal/cli/run.go:11:21: unapproved"}},
		{"duplicate wrapper", func(f map[string]string) {
			f["internal/cli/cli.go"] += "\nfunc Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {\n\treturn runFor(ctx, runtime.GOOS, args, in, out, errOut)\n}\n"
		}, []string{"internal/cli/cli.go: approved wrapper Run is declared 2 times"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireViolations(t, c.name, guardViolations(t, policyTree(t, c.edit)), c.want...)
		})
	}
}

func TestPlatformGuardExemptions(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(map[string]string)
		want []string
	}{
		{"retagged exemption", func(f map[string]string) {
			f["internal/spikes/processgroup/sys_linux.go"] = "//go:build linux || darwin\n\npackage processgroup\n"
		}, []string{"internal/spikes/processgroup/sys_linux.go: exempt native file must keep //go:build linux (found linux || darwin)"}},
		{"untagged exemption", func(f map[string]string) {
			f["internal/testkit/fakeadapter/signals_other.go"] = "package fakeadapter\n\nconst signalsSupported = false\n"
		}, []string{"internal/testkit/fakeadapter/signals_other.go: exempt native file must keep //go:build !linux && !darwin (found none)"}},
		{"build line after package", func(f map[string]string) {
			f["internal/spikes/processgroup/sys_darwin.go"] = "package processgroup\n\n//go:build darwin\n"
		}, []string{"internal/spikes/processgroup/sys_darwin.go: exempt native file must keep //go:build darwin (found none)"}},
		{"missing exemption", func(f map[string]string) { delete(f, "internal/testkit/fakeadapter/signals_unix.go") },
			[]string{"internal/testkit/fakeadapter/signals_unix.go: exempt native file is missing (policy requires it with //go:build linux || darwin)"}},
		{"dot import in exemption", func(f map[string]string) {
			f["internal/spikes/processgroup/sys_darwin.go"] = "//go:build darwin\n\npackage processgroup\n\nimport . \"runtime\"\n\nvar _ = GOOS\n"
		}, []string{"internal/spikes/processgroup/sys_darwin.go:5:8: dot import of runtime is not allowed"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireViolations(t, c.name, guardViolations(t, policyTree(t, c.edit)), c.want...)
		})
	}
}

func TestPlatformGuardSourceErrors(t *testing.T) {
	// Malformed source fails with its position.
	root := policyTree(t, func(f map[string]string) { f["internal/x/bad.go"] = "package x\n\nfunc (\n" })
	requireViolations(t, "malformed", guardViolations(t, root), "internal/x/bad.go:3:8: cannot parse: expected ')', found 'EOF'")
	// Unreadable source fails (injected; permission bits differ for
	// privileged users).
	root = policyTree(t, nil)
	sentinel := errors.New("injected read failure")
	readFile := func(p string) ([]byte, error) {
		if strings.HasSuffix(filepath.ToSlash(p), "internal/devcheck/devcheck.go") {
			return nil, sentinel
		}
		return os.ReadFile(p)
	}
	err := checkPlatformSources(root, platformGuardPolicy, readFile)
	var ge *PlatformGuardError
	if !errors.As(err, &ge) {
		t.Fatalf("unreadable = %v", err)
	}
	requireViolations(t, "unreadable", ge.Violations,
		"internal/devcheck/devcheck.go: approved wrapper Run is missing",
		"internal/devcheck/devcheck.go: cannot read: injected read failure")
	// A missing root or a file root is an error, not an empty pass.
	if err := CheckPlatformSources(filepath.Join(root, "missing")); err == nil || !os.IsNotExist(errors.Unwrap(err)) {
		t.Fatalf("missing root = %v", err)
	}
	if err := CheckPlatformSources(filepath.Join(root, "root.go")); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file root = %v", err)
	}
	// Symlinks are never followed: a linked Go source or source directory
	// is rejected; links to other files, linked tests and links inside
	// skipped names are not.
	root = policyTree(t, nil)
	links := map[string]string{
		"internal/x/link.go":       "../cli/cli.go",
		"internal/linked":          "cli",
		"internal/x/notes.txt":     "../cli/cli.go",
		"internal/x/link_test.go":  "../cli/cli.go",
		"internal/.hidden":         "cli",
		"internal/x/dangling":      "nowhere",
		"internal/x/dangling.go":   "nowhere.go",
		"internal/x/dirlink.go":    "../cli",
		"internal/x/testdata-link": "../../design",
	}
	os.MkdirAll(filepath.Join(root, "internal", "x"), 0o755)
	for rel, target := range links {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	requireViolations(t, "symlinks", guardViolations(t, root),
		"internal/linked: symlinked source directory is not allowed",
		"internal/x/dangling.go: symlinked Go source is not allowed",
		"internal/x/dirlink.go: symlinked Go source is not allowed",
		"internal/x/link.go: symlinked Go source is not allowed")
}

func TestPlatformGuardDeterministic(t *testing.T) {
	root := policyTree(t, func(f map[string]string) {
		f["z/last.go"] = hostRead
		f["a/first.go"] = "package a\n\nimport \"runtime\"\n\nvar b = runtime.GOOS\nvar a = runtime.GOOS\n"
		f["m/dot.go"] = "package m\n\nimport . \"runtime\"\n"
		delete(f, "internal/spikes/processgroup/sys_darwin.go")
	})
	want := []string{
		"a/first.go:5:9: unapproved",
		"a/first.go:6:9: unapproved",
		"internal/spikes/processgroup/sys_darwin.go: exempt native file is missing",
		"m/dot.go:3:8: dot import",
		"z/last.go:5:29: unapproved",
	}
	first := guardViolations(t, root)
	requireViolations(t, "sorted", first, want...)
	for i := 0; i < 5; i++ {
		if again := guardViolations(t, root); strings.Join(again, "\n") != strings.Join(first, "\n") {
			t.Fatalf("run %d differs:\n%q\n%q", i, again, first)
		}
	}
	err := CheckPlatformSources(root)
	if !strings.HasPrefix(err.Error(), "devcheck: platform guard: 5 violation(s) of the platform seam convention") {
		t.Fatalf("summary = %v", err)
	}
}

// A policy different from production (a fixture allowlist) is honored by
// the private scanner, so production policy is the only fixed input.
func TestPlatformGuardFixturePolicy(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"p/p.go": "package p\n\nimport \"runtime\"\n\nfunc Host() string { return hostFor(runtime.GOOS) }\n\nfunc hostFor(goos string) string { return goos }\n",
	})
	pol := platformPolicy{wrappers: []platformWrapper{{file: "p/p.go", fn: "Host", ret: true, callee: "hostFor", args: []wrapperArg{hostArg("GOOS")}}}}
	if err := checkPlatformSources(root, pol, os.ReadFile); err != nil {
		t.Fatalf("fixture policy: %v", err)
	}
	if err := checkPlatformSources(root, platformPolicy{}, os.ReadFile); err == nil || !strings.Contains(err.Error(), "p/p.go:5:37: unapproved host OS read runtime.GOOS in func Host") {
		t.Fatalf("empty policy: %v", err)
	}
	if v := (wrapperArg{name: "x"}).String(); v != "x" {
		t.Fatal(v)
	}
}
