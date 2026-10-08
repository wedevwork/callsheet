// Command fixtureexport is the function tests' helper process for the
// decoder-enrollment maintainer command (design decoder-enrollment B2,
// FP-18): its main calls the same exported command runner as
// cmd/mcpfixture-export, with the B2 metadata rules accepting one
// fabricated source named by FIXTURE_EXPORT_CLIENT, _VERSION, _PLATFORM,
// _RUN and _SHA256, and its output placement check observing the test's
// fixture roots (FIXTURE_EXPORT_ROOTS, a path list; the design
// decoder-enrollment B1 DW10 boundary: paths at or below a root are the real
// filesystem, its ancestors clean directories, anything else absent). The
// production command accepts only the compiled pins, observes the
// operating system and reads no variable. tests/function builds this by
// explicit path; testdata keeps it out of ./... and of coverage.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
)

func main() {
	get := func(k string) string { return os.Getenv("FIXTURE_EXPORT_" + k) }
	pol := mcpqual.FixturePolicyForTests(mcpqual.FixtureSource{Client: get("CLIENT"), Version: get("VERSION"), Platform: get("PLATFORM"), RunID: get("RUN"),
		ManifestSHA256: get("SHA256")})
	if roots := get("ROOTS"); roots != "" {
		pol = pol.WithPlacementView(rootsView(filepath.SplitList(roots)))
	}
	os.Exit(mcpqual.RunFixtureExport(os.Args[1:], os.Stdout, os.Stderr, pol))
}

// rootsView is the fixture-root view of the placement check.
type rootsView []string

func (v rootsView) inside(p string) bool {
	for _, r := range v {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

func (v rootsView) above(p string) bool {
	for _, r := range v {
		if p != r && (p == "/" || strings.HasPrefix(r, p+"/")) {
			return true
		}
	}
	return false
}

type viewDir string

func (d viewDir) Name() string       { return string(d) }
func (d viewDir) Size() int64        { return 0 }
func (d viewDir) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (d viewDir) ModTime() time.Time { return time.Time{} }
func (d viewDir) IsDir() bool        { return true }
func (d viewDir) Sys() any           { return nil }

func (v rootsView) Lstat(p string) (os.FileInfo, error) {
	switch p = filepath.Clean(p); {
	case v.inside(p):
		return os.Lstat(p)
	case v.above(p):
		return viewDir(filepath.Base(p)), nil
	}
	return nil, &os.PathError{Op: "lstat", Path: p, Err: os.ErrNotExist}
}

func (v rootsView) EvalSymlinks(p string) (string, error) {
	switch p = filepath.Clean(p); {
	case v.inside(p):
		return filepath.EvalSymlinks(p)
	case v.above(p):
		return p, nil
	}
	return "", &os.PathError{Op: "evalsymlinks", Path: p, Err: os.ErrNotExist}
}
