package function

import (
	"context"
	"debug/elf"
	"debug/macho"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP8BuildMatrix is FP-8's function test: the repeatable cross-build
// entry point produces the declared artifacts without executing foreign
// binaries. The cross stage (devcheck cross, which ci-linux runs) builds
// all twelve artifacts of the four targets, and Cross itself verifies them:
// each exists, is nonempty and has an ELF or Mach-O header naming its
// GOOS/GOARCH. This test proves the same entry point cheaply on every host
// that runs the package, without rebuilding the matrix:
//   - the declared Matrix, CrossPlan and CrossArtifacts against the
//     supported set asserted here independently: four targets and twelve
//     artifact names, each a CGO-disabled go build or go test -c for its
//     own GOOS/GOARCH;
//   - Cross with ExecRunner for the host's own target, from the repository
//     root: three real builds, whose packages the suite's CGO-disabled
//     host helper builds have already compiled, which Cross verifies; the
//     test re-inspects them independently and never executes them;
//   - the verifier on those real artifacts planned under every other
//     target's names: another OS or another architecture fails, naming
//     each artifact.
func TestFP8BuildMatrix(t *testing.T) {
	t.Parallel() // a few links on a warm cache; reads no state the serial tests change
	supported := []devcheck.Target{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "linux", GOARCH: "arm64"}, {GOOS: "darwin", GOARCH: "amd64"}, {GOOS: "darwin", GOARCH: "arm64"}}
	if !slices.Equal(devcheck.Matrix, supported) {
		t.Fatalf("matrix = %v, want %v", devcheck.Matrix, supported)
	}
	plan, err := devcheck.CrossPlan("/out", devcheck.Matrix)
	if err != nil {
		t.Fatal(err)
	}
	arts, err := devcheck.CrossArtifacts("/out", devcheck.Matrix)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 12 || len(arts) != 12 {
		t.Fatalf("expected 4+4+4 artifacts, planned %d steps and %d artifacts", len(plan), len(arts))
	}
	for i, tg := range supported {
		sfx := tg.GOOS + "-" + tg.GOARCH
		want := [][2]string{
			{"callsheet-" + sfx, "go build -o /out/callsheet-" + sfx + " ./cmd/callsheet"},
			{"fake-adapter-" + sfx, "go build -o /out/fake-adapter-" + sfx + " ./cmd/fake-adapter"},
			{"processgroup-" + sfx + ".test", "go test -c -o /out/processgroup-" + sfx + ".test ./internal/spikes/processgroup"},
		}
		for j, w := range want {
			s, a := plan[3*i+j], arts[3*i+j]
			if got := strings.Join(s.Argv, " "); got != filepath.FromSlash(w[1]) {
				t.Fatalf("%s step = %s, want %s", tg, got, w[1])
			}
			if got := strings.Join(s.Env, " "); got != "CGO_ENABLED=0 GOOS="+tg.GOOS+" GOARCH="+tg.GOARCH {
				t.Fatalf("%s step env = %s", tg, got)
			}
			if a.Path != filepath.Join("/out", w[0]) || a.Target != tg {
				t.Fatalf("artifact %+v, want %s for %s", a, w[0], tg)
			}
		}
	}

	// The real entry point for this host's target.
	host := devcheck.Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	if err := devcheck.ValidateTarget(host); err != nil {
		t.Fatalf("this host: %v", err)
	}
	root := testkit.MustRepoRoot(t)
	inRoot := func(ctx context.Context, argv, env []string, _ string, stdout, stderr io.Writer) error {
		return devcheck.ExecRunner(ctx, argv, env, root, stdout, stderr)
	}
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := devcheck.Cross(ctx, inRoot, out, []devcheck.Target{host}); err != nil {
		t.Fatalf("cross %s: %v", host, err)
	}
	built := map[string][]byte{}
	entries, _ := os.ReadDir(out)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(out, e.Name()))
		if err != nil || len(b) == 0 {
			t.Fatalf("%s missing or empty: %v", e.Name(), err)
		}
		if goos, goarch := inspect(t, filepath.Join(out, e.Name())); goos != host.GOOS || goarch != host.GOARCH {
			t.Fatalf("%s metadata = %s/%s, want %s", e.Name(), goos, goarch, host)
		}
		built[e.Name()] = b
	}
	sfx := host.GOOS + "-" + host.GOARCH
	for _, n := range []string{"callsheet-" + sfx, "fake-adapter-" + sfx, "processgroup-" + sfx + ".test"} {
		if built[n] == nil {
			t.Fatalf("artifacts = %v, missing %s", slices.Sorted(maps.Keys(built)), n)
		}
	}
	if len(built) != 3 {
		t.Fatalf("artifacts = %v", slices.Sorted(maps.Keys(built)))
	}

	// The verifier refuses the real host artifacts under foreign names.
	for _, tg := range supported {
		if tg == host {
			continue
		}
		dir := t.TempDir()
		fsfx := tg.GOOS + "-" + tg.GOARCH
		for _, kind := range [][2]string{{"callsheet-", ""}, {"fake-adapter-", ""}, {"processgroup-", ".test"}} {
			if err := os.WriteFile(filepath.Join(dir, kind[0]+fsfx+kind[1]), built[kind[0]+sfx+kind[1]], 0o755); err != nil {
				t.Fatal(err)
			}
		}
		err := devcheck.VerifyArtifacts(dir, []devcheck.Target{tg})
		for _, n := range []string{"callsheet-" + fsfx, "fake-adapter-" + fsfx, "processgroup-" + fsfx + ".test"} {
			if err == nil || !strings.Contains(err.Error(), "cross artifact "+n+": built for "+host.String()+", want "+tg.String()) {
				t.Fatalf("host artifacts planned as %s: %v", tg, err)
			}
		}
	}
}

// inspect reads executable headers to determine the target without running
// it, independently of devcheck's verifier.
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
// macOS native stage runs this package under a per-package timeout, and
// its independent tests run in parallel): no parallel test changes the
// process's working directory or environment, directly or through the
// package's helpers (whatever its *testing.T parameter is called; Go also
// refuses t.Chdir and t.Setenv there at run time), and no test starts a
// goroutine before t.Parallel(), which would overlap the serial tests that
// change the environment. The checker is first shown to catch each
// forbidden shape.
func TestFunctionParallelSafety(t *testing.T) {
	t.Parallel()
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "x_test.go"), []byte(`package function

import (
	"os"
	"testing"
)

func TestSerial(t *testing.T) { t.Chdir("/"); t.Setenv("A", "b") }

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

func TestEarlyIsolated(t *testing.T) {
	run := isolatedCrossRunner(t)
	go run()
	t.Parallel()
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := parallelSafetyProblems(t, bad)
	for _, want := range []string{"TestOther is parallel", "TestHelper is parallel",
		"TestRenamed is parallel", "TestOSEnv is parallel", "TestEarlyWork starts a goroutine before", "TestEarlyIsolated starts a goroutine before"} {
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
		for _, st := range fn.Body.List[:parIndex] {
			ast.Inspect(st, func(n ast.Node) bool {
				if _, ok := n.(*ast.GoStmt); ok {
					problems = append(problems, name+" starts a goroutine before t.Parallel() (it would overlap serial tests that change the environment)")
					return false
				}
				return true
			})
		}
	}
	return problems
}
