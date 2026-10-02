package function

import (
	"context"
	"debug/elf"
	"debug/macho"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP8BuildMatrix invokes devcheck.Cross with ExecRunner and the
// four-target Linux/macOS matrix from the repository root, then inspects
// (never executes) every artifact's target metadata. Its twelve cross
// builds are the package's largest single cost, so they start as soon as
// the test is reached and overlap the serial tests that follow it (those
// mostly wait on subprocesses); the test then waits for them as a
// parallel test. This file sorts first in the package, so the builds
// start with the package's first test. The builds read no live process
// state: isolatedCrossRunner fixes the go executable, the environment and
// the working directory before they start (t.Chdir is not allowed in a
// parallel test, and serial tests change the environment meanwhile).
func TestFP8BuildMatrix(t *testing.T) {
	// The supported set is asserted independently of the planner.
	supported := []devcheck.Target{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "linux", GOARCH: "arm64"}, {GOOS: "darwin", GOARCH: "amd64"}, {GOOS: "darwin", GOARCH: "arm64"}}
	if len(devcheck.Matrix) != len(supported) {
		t.Fatalf("matrix = %v, want %v", devcheck.Matrix, supported)
	}
	for i, tg := range supported {
		if devcheck.Matrix[i] != tg {
			t.Fatalf("matrix = %v, want %v", devcheck.Matrix, supported)
		}
	}
	// The builds overlap serial tests that change the process environment
	// (t.Setenv, for example an empty PATH): the runner reads none of it.
	run := isolatedCrossRunner(t, testkit.MustRepoRoot(t), devcheck.Matrix)
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	built := make(chan error, 1)
	go func() { built <- devcheck.Cross(ctx, run, out, devcheck.Matrix) }()
	t.Parallel()
	if err := <-built; err != nil {
		t.Fatalf("cross: %v", err)
	}
	var want []string
	for _, tg := range supported {
		want = append(want, devcheck.CallsheetArtifact(tg), devcheck.FakeArtifact(tg), devcheck.ProcessTestArtifact(tg))
	}
	if len(want) != 12 {
		t.Fatalf("expected 4+4+4 artifacts, planned %d", len(want))
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != len(want) {
		t.Fatalf("artifacts = %d, want %d", len(entries), len(want))
	}
	for _, tg := range supported {
		for _, n := range []string{devcheck.CallsheetArtifact(tg), devcheck.FakeArtifact(tg), devcheck.ProcessTestArtifact(tg)} {
			p := filepath.Join(out, n)
			st, err := os.Stat(p)
			if err != nil || st.Size() == 0 {
				t.Fatalf("%s missing or empty: %v", n, err)
			}
			goos, goarch := inspect(t, p)
			if goos != tg.GOOS || goarch != tg.GOARCH {
				t.Fatalf("%s metadata = %s/%s, want %s", n, goos, goarch, tg)
			}
		}
	}
}

// isolatedCrossRunner returns a devcheck.Runner bound to state captured
// now: the absolute go executable, an immutable copy of the process
// environment and the repository root as the working directory. From a
// call it takes only the argument vector (its "go" replaced by the
// captured executable) and the step's own overrides, the variables
// CrossPlan sets for targets (CGO_ENABLED, GOOS, GOARCH); GOFLAGS gains
// -p=1, so one package compiles at a time and the builds leave the other
// CPUs to the tests they overlap. Nothing it does reads the live process
// environment, PATH or working directory, which serial tests may change
// while the builds run.
func isolatedCrossRunner(t *testing.T, root string, targets []devcheck.Target) devcheck.Runner {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err == nil {
		goBin, err = filepath.Abs(goBin)
	}
	if err != nil {
		t.Fatalf("the go executable: %v", err)
	}
	base := slices.Clone(os.Environ())
	plan, err := devcheck.CrossPlan(root, targets)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, st := range plan {
		for _, kv := range st.Env {
			k, _, _ := strings.Cut(kv, "=")
			keys[k] = true
		}
	}
	return func(ctx context.Context, argv, env []string, _ string, stdout, stderr io.Writer) error {
		if len(argv) == 0 || argv[0] != "go" {
			return fmt.Errorf("cross step %v does not run go", argv)
		}
		var overrides []string
		for _, kv := range env {
			if k, _, _ := strings.Cut(kv, "="); keys[k] {
				overrides = append(overrides, kv)
			}
		}
		args := append([]string{goBin}, argv[1:]...)
		return devcheck.ExecRunner(ctx, args, withGoFlag(devcheck.MergeEnv(base, overrides), "-p=1"), root, stdout, stderr)
	}
}

// TestCrossBuildEnvIsolation proves the cross-build runner is independent
// of the live process state (code review of the headroom fix, C1): with
// PATH emptied and GOFLAGS, GOOS and the working directory's meaning
// changed after it was created, a go command it runs still finds go,
// sees the step's own GOOS and GOARCH, the captured GOFLAGS plus -p=1,
// and the repository root. As a serial test that changes the
// environment, it also overlaps TestFP8BuildMatrix's running builds in
// every run of the package.
func TestCrossBuildEnvIsolation(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	run := isolatedCrossRunner(t, root, devcheck.Matrix)
	t.Setenv("PATH", "")
	t.Setenv("GOFLAGS", "-mod=callsheet-bogus")
	t.Setenv("GOOS", "plan9")
	var out, errOut strings.Builder
	env := devcheck.MergeEnv(os.Environ(), []string{"CGO_ENABLED=0", "GOOS=darwin", "GOARCH=arm64"})
	if err := run(context.Background(), []string{"go", "env", "GOOS", "GOARCH", "GOFLAGS", "GOMOD"}, env, "", &out, &errOut); err != nil {
		t.Fatalf("go env under a changed environment: %v %s", err, errOut.String())
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(got) != 4 || got[0] != "darwin" || got[1] != "arm64" || strings.Contains(got[2], "bogus") || !strings.HasSuffix(got[2], "-p=1") ||
		got[3] != filepath.Join(root, "go.mod") {
		t.Fatalf("go env = %q", got)
	}
}

// withGoFlag returns env with flag added to GOFLAGS.
func withGoFlag(env []string, flag string) []string {
	out := make([]string, 0, len(env)+1)
	value := flag
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GOFLAGS="); ok {
			if v != "" {
				value = v + " " + flag
			}
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOFLAGS="+value)
}

// inspect reads executable headers to determine the target without running it.
func inspect(t *testing.T, p string) (string, string) {
	t.Helper()
	if f, err := elf.Open(p); err == nil {
		defer f.Close()
		arch := map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64"}[f.Machine]
		if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
			t.Fatalf("%s is not an executable ELF", p)
		}
		return "linux", arch
	}
	if f, err := macho.Open(p); err == nil {
		defer f.Close()
		arch := map[macho.Cpu]string{macho.CpuAmd64: "amd64", macho.CpuArm64: "arm64"}[f.Cpu]
		if f.Type != macho.TypeExec {
			t.Fatalf("%s is not an executable Mach-O", p)
		}
		return "darwin", arch
	}
	t.Fatalf("%s: unrecognised executable format", p)
	return "", ""
}

// TestFunctionParallelSafety guards the package's CI headroom (the
// macOS native stage runs this package under a per-package timeout):
// TestFP8BuildMatrix runs in parallel; no parallel test changes the
// process's working directory or environment, directly or through the
// package's helpers (whatever its *testing.T parameter is called; Go
// also refuses t.Chdir and t.Setenv there at run time); and no test
// starts work before t.Parallel() that would overlap the serial tests
// changing the environment, unless it first fixes its state with
// isolatedCrossRunner. Whether a goroutine reads live process state
// through other packages (devcheck.Cross reads os.Environ for every step)
// cannot be decided from this package's syntax, so that one allowance is
// proved dynamically instead: TestCrossBuildEnvIsolation runs the runner
// under a changed environment and, as a serial t.Setenv test, overlaps
// TestFP8BuildMatrix's builds in every run. The checker is first shown to
// catch each forbidden shape.
func TestFunctionParallelSafety(t *testing.T) {
	t.Parallel()
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "x_test.go"), []byte(`package function

import (
	"os"
	"testing"
)

func TestFP8BuildMatrix(t *testing.T) { t.Chdir("/") }

func TestOther(t *testing.T) { t.Parallel(); t.Setenv("A", "b") }

func changeDir(t *testing.T) { t.Chdir("/") }

func viaHelper(t *testing.T) { changeDir(t) }

func TestHelper(t *testing.T) { t.Parallel(); viaHelper(t) }

func TestRenamed(tb *testing.T) { tb.Parallel(); tb.Setenv("A", "b") }

func TestOSEnv(t *testing.T) { t.Parallel(); os.Setenv("A", "b") }

func TestEarlyWork(t *testing.T) {
	done := make(chan bool)
	go func() { done <- true }()
	t.Parallel()
	<-done
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := parallelSafetyProblems(t, bad)
	for _, want := range []string{"TestFP8BuildMatrix must run in parallel", "TestOther is parallel", "TestHelper is parallel",
		"TestRenamed is parallel", "TestOSEnv is parallel", "TestEarlyWork starts a goroutine before"} {
		if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, want) }) {
			t.Errorf("the checker missed %q: %v", want, got)
		}
	}
	if len(got) != 6 {
		t.Errorf("checker problems %v", got)
	}
	for _, p := range parallelSafetyProblems(t, ".") {
		t.Error(p)
	}
}

// parallelSafetyProblems lists the violations of the package's
// parallel-safety rules in the test files of dir.
func parallelSafetyProblems(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool { return strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	// testingParams are a function's parameters of type *testing.T,
	// *testing.B or testing.TB.
	testingParams := func(fn *ast.FuncDecl) map[string]bool {
		out := map[string]bool{}
		for _, f := range fn.Type.Params.List {
			typ := f.Type
			if st, ok := typ.(*ast.StarExpr); ok {
				typ = st.X
			}
			if sel, ok := typ.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "testing" && (sel.Sel.Name == "T" || sel.Sel.Name == "B" || sel.Sel.Name == "TB") {
					for _, n := range f.Names {
						out[n.Name] = true
					}
				}
			}
		}
		return out
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
					funcs[fn.Name.Name] = fn // methods by name too: a conservative match
				}
			}
		}
	}
	// direct reports whether node calls a process-state mutation itself,
	// and collects the package functions it calls.
	direct := func(node ast.Node, params map[string]bool, callees map[string]bool) bool {
		mutates := false
		ast.Inspect(node, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := c.Fun.(type) {
			case *ast.Ident:
				if _, ok := funcs[fun.Name]; ok {
					callees[fun.Name] = true
				}
			case *ast.SelectorExpr:
				if id, ok := fun.X.(*ast.Ident); ok {
					switch {
					case params[id.Name] && (fun.Sel.Name == "Chdir" || fun.Sel.Name == "Setenv"):
						mutates = true
					case id.Name == "os" && (fun.Sel.Name == "Chdir" || fun.Sel.Name == "Setenv" || fun.Sel.Name == "Unsetenv" || fun.Sel.Name == "Clearenv"):
						mutates = true
					}
				}
				if _, ok := funcs[fun.Sel.Name]; ok {
					callees[fun.Sel.Name] = true
				}
			}
			return true
		})
		return mutates
	}
	// mutating is the transitive closure over the package's functions.
	memo := map[string]int{} // 0 unknown, 1 visiting/no, 2 yes
	var mutating func(name string) bool
	mutating = func(name string) bool {
		if v, ok := memo[name]; ok {
			return v == 2
		}
		memo[name] = 1
		fn := funcs[name]
		callees := map[string]bool{}
		yes := direct(fn.Body, testingParams(fn), callees)
		for c := range callees {
			if !yes && mutating(c) {
				yes = true
			}
		}
		if yes {
			memo[name] = 2
		}
		return yes
	}
	var problems []string
	parallel := map[string]bool{}
	var names []string
	for n := range funcs {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, name := range names {
		fn := funcs[name]
		params := testingParams(fn)
		if !strings.HasPrefix(name, "Test") || fn.Recv != nil || len(params) != 1 {
			continue
		}
		var tp string
		for p := range params {
			tp = p
		}
		// The statements before the one that calls tp.Parallel().
		parIndex := -1
		for i, st := range fn.Body.List {
			found := false
			ast.Inspect(st, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == tp && sel.Sel.Name == "Parallel" {
							found = true
						}
					}
				}
				return !found
			})
			if found {
				parIndex = i
				break
			}
		}
		if parIndex < 0 {
			continue
		}
		parallel[name] = true
		callees := map[string]bool{}
		yes := direct(fn.Body, params, callees)
		for c := range callees {
			if !yes && mutating(c) {
				yes = true
			}
		}
		if yes {
			problems = append(problems, name+" is parallel and changes the process's working directory or environment")
		}
		isolated := false
		for _, st := range fn.Body.List[:parIndex] {
			ast.Inspect(st, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "isolatedCrossRunner" {
						isolated = true
					}
				}
				if _, ok := n.(*ast.GoStmt); ok && !isolated {
					problems = append(problems, name+" starts a goroutine before t.Parallel() without isolatedCrossRunner (it would overlap serial tests that change the environment)")
					return false
				}
				return true
			})
		}
	}
	if !parallel["TestFP8BuildMatrix"] {
		problems = append(problems, "TestFP8BuildMatrix must run in parallel (its cross builds are the package's largest cost)")
	}
	return problems
}
