package workspace

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// goList returns the dependency closure of pkg.
func goList(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	cmd := exec.Command(testkit.GoTool(), "list", "-deps", pkg)
	cmd.Dir = testkit.MustRepoRoot(t)
	cmd.Env = testkit.EnvWithout(os.Environ(), testkit.ProxyVars)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	deps := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		deps[l] = true
	}
	return deps
}

// UT-8: the workspace hub's production dependency graph excludes the
// spikes (the iteration 01 git transport spike stays test-only, also for
// the callsheet binary; the sidecar's pre-existing process-group use is
// outside this slice); the workspace package reaches neither os/exec nor
// go-git's client, file or SSH transports (so no library path can fall
// through to a git process, remote or helper); and its production files
// import no process API. Runtime proof with git absent is
// TestWorkspaceNoGit.
func TestDependencyContract(t *testing.T) {
	bin := goList(t, testkit.ModulePath+"/cmd/callsheet")
	for d := range bin {
		if strings.HasPrefix(d, testkit.ModulePath+"/internal/spikes/gittransport") || strings.HasPrefix(d, testkit.ModulePath+"/internal/testkit") {
			t.Fatalf("callsheet depends on %s", d)
		}
	}
	if !bin[testkit.ModulePath+"/internal/workspace"] || !bin["github.com/go-git/go-git/v5/plumbing/transport/server"] {
		t.Fatal("callsheet does not link the workspace hub")
	}
	ws := goList(t, testkit.ModulePath+"/internal/workspace")
	for d := range ws {
		if strings.HasPrefix(d, testkit.ModulePath+"/internal/") && d != testkit.ModulePath+"/internal/workspace" && d != testkit.ModulePath+"/internal/contract" {
			t.Fatalf("internal/workspace depends on %s", d)
		}
	}
	for _, banned := range []string{
		"os/exec",
		"github.com/go-git/go-git/v5",
		"github.com/go-git/go-git/v5/plumbing/transport/client",
		"github.com/go-git/go-git/v5/plumbing/transport/file",
		"github.com/go-git/go-git/v5/plumbing/transport/ssh",
		"github.com/go-git/go-git/v5/plumbing/transport/git",
		"github.com/go-git/go-git/v5/plumbing/transport/http",
	} {
		if ws[banned] {
			t.Fatalf("internal/workspace depends on %s", banned)
		}
	}
	files, err := filepath.Glob(filepath.Join(testkit.MustRepoRoot(t), "internal", "workspace", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("sources %v %v", files, err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range src.Imports {
			switch strings.Trim(imp.Path.Value, `"`) {
			case "os/exec", "syscall/exec", "github.com/go-git/go-git/v5":
				t.Fatalf("%s imports %s", filepath.Base(f), imp.Path.Value)
			}
		}
	}
}
