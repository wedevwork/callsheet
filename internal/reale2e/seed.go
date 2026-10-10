package reale2e

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The throwaway Go project (design 12a-real-e2e, Deployment and roles,
// Feature and hop protocol): a normal small repository with a local
// throwaway identity, no remote or hook, a dependency-free module whose
// program prints "Hello, world!\n", baseline tests proving it, and the
// feature request. It is created with go-git only in the new runtime;
// no global Git configuration is read or modified.

// SeedAuthor is the seed commit's local throwaway identity.
var seedAuthor = object.Signature{Name: "Callsheet Real E2E", Email: "real-e2e@callsheet.invalid", When: time.Unix(1767225600, 0).UTC()}

// FeatureRequest is the seed's FEATURE.md.
const FeatureRequest = `# Feature request: --name

Add an optional --name NAME flag to this greeting CLI.

- With no flag, print "Hello, world!" and a newline (exit 0).
- With --name Ada, print "Hello, Ada!" and a newline (exit 0).
- Preserve spaces and Unicode in a nonempty name.
- Reject an empty name, unknown flags, a missing value and positional
  arguments with exit status 2, nothing on stdout and a nonempty diagnostic
  on stderr.
- --help prints usage and exits 0.
- Standard Go flag parsing is acceptable. Implement a testable
  run(args []string, stdout, stderr io.Writer) int with table tests,
  including Unicode, a whitespace name, every error and help.
- No dependencies, network or persistence. Keep the whole project under
  16 KiB.
`

// seedMain is the seed's main.go.
const seedMain = `// Command greeting prints a greeting.
package main

import "fmt"

// greeting is the text the program prints.
func greeting() string { return "Hello, world!\n" }

func main() {
	fmt.Print(greeting())
}
`

// seedTest is the seed's main_test.go.
const seedTest = `package main

import "testing"

func TestGreeting(t *testing.T) {
	if got := greeting(); got != "Hello, world!\n" {
		t.Fatalf("greeting() = %q, want %q", got, "Hello, world!\n")
	}
}
`

// seedFiles returns the seed's files for a module at goVersion.
func seedFiles(goVersion string) map[string][]byte {
	return map[string][]byte{
		"go.mod":       []byte("module example.com/greeting\n\ngo " + goVersion + "\n"),
		"main.go":      []byte(seedMain),
		"main_test.go": []byte(seedTest),
		"FEATURE.md":   []byte(FeatureRequest),
	}
}

// goDirective returns the go directive of a go.mod file.
func goDirective(gomod []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(gomod))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[0] == "go" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("the checkout's go.mod has no go directive")
}

// seedRepo is the created seed.
type seedRepo struct {
	Path   string
	Commit plumbing.Hash
	Tree   plumbing.Hash
	Files  map[string][]byte
	repo   *git.Repository
}

// createSeed initializes dir as a repository and commits files.
func createSeed(dir string, files map[string][]byte) (*seedRepo, error) {
	r, err := git.PlainInit(dir, false)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	wt, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	for _, name := range sortedKeys(files) {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return nil, fmt.Errorf("seed: %w", err)
		}
		if _, err := wt.Add(name); err != nil {
			return nil, fmt.Errorf("seed: %w", err)
		}
	}
	sig := seedAuthor
	h, err := wt.Commit("Seed the throwaway greeting CLI\n", &git.CommitOptions{Author: &sig, Committer: &sig})
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	c, err := r.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	return &seedRepo{Path: dir, Commit: h, Tree: c.TreeHash, Files: files, repo: r}, nil
}

// sortedKeys returns m's keys in order.
func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// projectFiles are the only files a tested tree may contain.
var projectFiles = map[string]bool{"go.mod": true, "main.go": true, "main_test.go": true, "FEATURE.md": true, "design.md": true, "design-review.md": true}

// testEnv returns the isolated environment of a test run under dir.
func testEnv(dir, path string) []string {
	return []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=0",
		"GOCACHE=" + filepath.Join(dir, "gocache"), "GOMODCACHE=" + filepath.Join(dir, "gomodcache"), "GOPATH=" + filepath.Join(dir, "gopath"),
		"HOME=" + filepath.Join(dir, "home"), "TMPDIR=" + filepath.Join(dir, "tmp"), "PATH=" + path}
}

// commandRunner runs one short command (the supervisor's command).
type commandRunner func(spec ProcSpec, timeout time.Duration, limit int) (CommandResult, error)

// runGoTests exports files into the new directory dir/src (only project
// files, regular, private) and runs "go test -count=1 ./..." there with an
// isolated environment, bounded by FinalTestBound. The record names the
// commit and tree the files came from.
func runGoTests(run commandRunner, goBin, pathEnv, dir string, files map[string][]byte, commit, tree string) (TestRun, []byte, []byte, error) {
	rec := TestRun{Schema: TestRunSchema, Argv: append([]string(nil), testArgv...), Commit: commit, Tree: tree, Files: sortedKeys(files)}
	for _, n := range rec.Files {
		if !projectFiles[n] {
			return rec, nil, nil, fmt.Errorf("the tree holds the unexpected file %s; nothing was executed", n)
		}
	}
	src := filepath.Join(dir, "src")
	for _, d := range []string{src, filepath.Join(dir, "gocache"), filepath.Join(dir, "gomodcache"), filepath.Join(dir, "gopath"), filepath.Join(dir, "home"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return rec, nil, nil, err
		}
	}
	for _, n := range rec.Files {
		if err := os.WriteFile(filepath.Join(src, n), files[n], 0o600); err != nil {
			return rec, nil, nil, err
		}
	}
	env := testEnv(dir, pathEnv)
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == "GOTOOLCHAIN" || k == "GOPROXY" || k == "GOSUMDB" || k == "GOWORK" {
			rec.Env = append(rec.Env, e)
		}
	}
	ver, err := run(ProcSpec{Path: goBin, Args: []string{"version"}, Env: env, Dir: src}, 30*time.Second, 4096)
	if err != nil {
		return rec, nil, nil, err
	}
	rec.Toolchain = strings.TrimSpace(string(ver.Stdout))
	res, err := run(ProcSpec{Path: goBin, Args: testArgv[1:], Env: env, Dir: src}, FinalTestBound, 1<<20)
	rec.ExitCode, rec.Signal, rec.TimedOut, rec.DurationMS = res.ExitCode, res.Signal, res.TimedOut, res.Duration.Milliseconds()
	return rec, res.Stdout, res.Stderr, err
}
