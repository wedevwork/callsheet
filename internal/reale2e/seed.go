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

Cases (each is one subtest of the table test TestRun, named exactly by its
case ID, calling run):

| Case ID | Arguments | Result |
|---|---|---|
| no_args | (none) | exit 0, stdout "Hello, world!\n", empty stderr |
| name | --name Ada | exit 0, stdout "Hello, Ada!\n", empty stderr |
| spaces | --name "Ada Lovelace" | exit 0, stdout "Hello, Ada Lovelace!\n", empty stderr |
| unicode | --name 世界 | exit 0, stdout "Hello, 世界!\n", empty stderr |
| whitespace | --name " " | exit 0, stdout "Hello,  !\n", empty stderr |
| empty_name | --name "" | exit 2, empty stdout, nonempty stderr |
| unknown_flag | --unknown | exit 2, empty stdout, nonempty stderr |
| missing_value | --name | exit 2, empty stdout, nonempty stderr |
| positional | Ada | exit 2, empty stdout, nonempty stderr |
| help | --help | exit 0, usage mentioning name |
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

// legacySizeSentence is the single FeatureRequest sentence the development
// request replaces (design 12a-dev-flow-harness, Shared method loading).
const legacySizeSentence = "Keep the whole project under\n  16 KiB."

// devFullSizeSentence is the owner-approved replacement.
const devFullSizeSentence = "Keep FEATURE.md, go.mod, main.go and main_test.go together strictly below 16 KiB. Each planning/report artifact is at most 32 KiB; all planning/report artifacts together are at most 128 KiB."

// devFullFeatureRequest derives the development full scenario's FEATURE.md
// from FeatureRequest: exactly one occurrence of the legacy size sentence
// is required and only it is replaced. The legacy request is unchanged.
func devFullFeatureRequest() (string, error) {
	if n := strings.Count(FeatureRequest, legacySizeSentence); n != 1 {
		return "", fmt.Errorf("feature request: the size sentence occurs %d times", n)
	}
	return strings.Replace(FeatureRequest, legacySizeSentence, devFullSizeSentence, 1), nil
}

// devSeedFiles is the development seed: the legacy seed files with the
// derived FEATURE.md.
func devSeedFiles(goVersion string) (map[string][]byte, error) {
	feature, err := devFullFeatureRequest()
	if err != nil {
		return nil, err
	}
	files := seedFiles(goVersion)
	files["FEATURE.md"] = []byte(feature)
	return files, nil
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

// exportProject exports files into the new directory dir/src (only
// project files, regular, private) with its private caches, returning the
// source directory and the isolated environment.
func exportProject(dir string, files map[string][]byte, pathEnv string) (string, []string, error) {
	for _, n := range sortedKeys(files) {
		if !projectFiles[n] {
			return "", nil, fmt.Errorf("the tree holds the unexpected file %s; nothing was executed", n)
		}
	}
	src := filepath.Join(dir, "src")
	for _, d := range []string{src, filepath.Join(dir, "gocache"), filepath.Join(dir, "gomodcache"), filepath.Join(dir, "gopath"), filepath.Join(dir, "home"),
		filepath.Join(dir, "tmp"), filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", nil, err
		}
	}
	for _, n := range sortedKeys(files) {
		if err := os.WriteFile(filepath.Join(src, n), files[n], 0o600); err != nil {
			return "", nil, err
		}
	}
	return src, testEnv(dir, pathEnv), nil
}

// runGoTests exports files into the new directory dir/src and runs "go
// test -count=1 ./..." there with an isolated environment, bounded by
// FinalTestBound (the seed's baseline). The record names the commit and
// tree the files came from.
func runGoTests(run commandRunner, goBin, pathEnv, dir string, files map[string][]byte, commit, tree string) (TestRun, []byte, []byte, error) {
	rec := TestRun{Schema: TestRunSchema, Argv: append([]string(nil), testArgv...), Commit: commit, Tree: tree, Files: sortedKeys(files)}
	src, env, err := exportProject(dir, files, pathEnv)
	if err != nil {
		return rec, nil, nil, err
	}
	rec.Env = offlineEnv(env)
	ver, err := run(ProcSpec{Path: goBin, Args: []string{"version"}, Env: env, Dir: src}, 30*time.Second, 4096)
	if err != nil {
		return rec, nil, nil, err
	}
	rec.Toolchain = strings.TrimSpace(string(ver.Stdout))
	res, err := run(ProcSpec{Path: goBin, Args: testArgv[1:], Env: env, Dir: src}, FinalTestBound, 1<<20)
	rec.ExitCode, rec.Signal, rec.TimedOut, rec.DurationMS = res.ExitCode, res.Signal, res.TimedOut, res.Duration.Milliseconds()
	return rec, res.Stdout, res.Stderr, err
}
