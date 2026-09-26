package testkit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ModulePath is the repository's Go module path.
const ModulePath = "github.com/wedevwork/callsheet"

// RepoRoot locates the repository root from this source file's location and
// its go.mod, independent of the process working directory.
func RepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("testkit: cannot determine source location")
	}
	return findModuleRoot(filepath.Dir(file))
}

func findModuleRoot(dir string) (string, error) {
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(b), "module "+ModulePath+"\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("testkit: repository root (go.mod) not found")
		}
		dir = parent
	}
}

// MustRepoRoot is RepoRoot failing t on error.
func MustRepoRoot(t testing.TB) string {
	t.Helper()
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// GoTool returns the go command of the running toolchain.
func GoTool() string {
	if p := filepath.Join(runtime.GOROOT(), "bin", "go"); fileExists(p) {
		return p
	}
	return "go"
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// BuildBinary builds the repository package pkg (e.g. "./cmd/callsheet")
// for the host into a test-owned temp dir and returns its absolute path.
func BuildBinary(t testing.TB, pkg, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	runGo(t, "build", "-o", out, pkg)
	return out
}

// BuildTestBinary compiles pkg's tests with "go test -c" for the host.
func BuildTestBinary(t testing.TB, pkg, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name+".test")
	runGo(t, "test", "-c", "-o", out, pkg)
	return out
}

func runGo(t testing.TB, args ...string) {
	t.Helper()
	if err := goCommand(args...); err != nil {
		t.Fatal(err)
	}
}

func goCommand(args ...string) error {
	root, err := RepoRoot()
	if err != nil {
		return err
	}
	cmd := exec.Command(GoTool(), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// BuildBinaryAt builds pkg into dir/name without a testing.TB, for
// process-lifetime fixtures that a TestMain creates once and removes at
// teardown (iteration 03), so repeated tests never rebuild it.
func BuildBinaryAt(dir, pkg, name string) (string, error) {
	out := filepath.Join(dir, name)
	return out, goCommand("build", "-o", out, pkg)
}

// BuildTestBinaryAt compiles pkg's tests into dir/name.test, like
// BuildBinaryAt.
func BuildTestBinaryAt(dir, pkg, name string) (string, error) {
	out := filepath.Join(dir, name+".test")
	return out, goCommand("test", "-c", "-o", out, pkg)
}

// EnvWithout returns environ minus the named variables (case-sensitive),
// followed by extra.
func EnvWithout(environ []string, names []string, extra ...string) []string {
	var out []string
	for _, kv := range environ {
		drop := false
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// ProxyVars are the environment proxy variables tests remove from helper
// subprocesses.
var ProxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"}

// EmptyDir creates and returns a new empty directory owned by t.
func EmptyDir(t testing.TB) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "empty-path")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(fmt.Errorf("empty dir: %w", err))
	}
	return d
}
