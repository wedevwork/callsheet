package reale2e

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Light oracle tests (design 12a-dev-flow-light, FP-4 and FP-10): small
// real-toolchain fixtures prove the exact seed failure and the healthy
// candidate; forged event streams are judged without rebuilding.

// realGo is the local Go toolchain (required: the oracle runs it).
func realGo(t testing.TB) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the local Go toolchain is required: %v", err)
	}
	return goBin
}

// lightFixture reads the committed light seed fixture with go.mod.
func lightFixture(t testing.TB) map[string][]byte {
	t.Helper()
	root := repoRoot(t)
	files := map[string][]byte{"go.mod": []byte("module example.com/greeting\n\ngo 1.26.0\n")}
	for _, n := range []string{"FEATURE.md", "main.go", "main_test.go"} {
		files[n] = mustRead(t, filepath.Join(root, filepath.FromSlash(devLightFixtureDir), n))
	}
	return files
}

// lightGoodCandidate is the seed with the known-good probe fixture's
// main.go and main_test.go (the restoration and the added regression).
func lightGoodCandidate(t testing.TB, seed map[string][]byte) map[string][]byte {
	t.Helper()
	root := repoRoot(t)
	cand := map[string][]byte{}
	for k, v := range seed {
		cand[k] = v
	}
	for _, n := range []string{"main.go", "main_test.go"} {
		cand[n] = mustRead(t, filepath.Join(root, filepath.FromSlash(devProbeFixtureDir), n))
	}
	return cand
}

// lightWarm is the real-toolchain oracle's shared warm build cache and
// the seed baseline that warmed it (the validator tests' pattern: one cold
// run compiles the standard library once; every later export starts from
// a copy of the cache it left, each command still executing). The
// baseline of the committed seed is an immutable fixture result, shared by
// the tests that judge it.
var lightWarm struct {
	sync.Once
	cache string
	base  lightBaselineRun
	err   error
}

// lightWarmed returns the warm cache directory and the seed baseline.
func lightWarmed(t testing.TB) (string, lightBaselineRun) {
	t.Helper()
	lightWarm.Do(func() {
		dir := filepath.Join(bundleCacheRoot, "light-warm")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			lightWarm.err = err
			return
		}
		lightWarm.base = runLightBaseline(coldLightOptions(t, dir), lightFixture(t))
		lightWarm.cache = filepath.Join(dir, "gocache")
		if !lightWarm.base.rec.Valid {
			lightWarm.err = fmt.Errorf("the warming seed baseline is invalid: %+v", lightWarm.base.rec)
		}
	})
	if lightWarm.err != nil {
		t.Fatal(lightWarm.err)
	}
	return lightWarm.cache, lightWarm.base
}

// lightSeedBaseline is the shared real baseline of the committed seed.
func lightSeedBaseline(t testing.TB) lightBaselineRun {
	t.Helper()
	_, b := lightWarmed(t)
	return b
}

// coldLightOptions runs the oracle with the real toolchain under dir on
// cold private caches.
func coldLightOptions(t testing.TB, dir string) lightRunOptions {
	t.Helper()
	goBin := realGo(t)
	clock := realClock{}
	run := func(spec ProcSpec, timeout time.Duration, limit int) (CommandResult, error) {
		return runCommand(ExecLauncher{}, clock, spec, timeout, limit)
	}
	return lightRunOptions{run: run, clock: clock, goBin: goBin, pathEnv: os.Getenv("PATH"), dir: dir, commit: strings.Repeat("a", 40),
		tree: strings.Repeat("b", 40), deadline: time.Now().Add(2 * time.Minute), mark: func(string, string) int { return 0 }}
}

// lightExportCaches are the private build caches every light oracle
// export under dir uses: the baseline's, the regression's before and
// after (directly and inside a final validation) and the final candidate's.
var lightExportCaches = []string{"gocache", "before/gocache", "after/gocache", "regression/before/gocache", "regression/after/gocache",
	"candidate/gocache"}

// realLightOptions runs the oracle with the real toolchain under dir,
// every export's private cache starting as a copy of the warm one (hard
// links of the immutable cache entries).
func realLightOptions(t testing.TB, dir string) lightRunOptions {
	t.Helper()
	cache, _ := lightWarmed(t)
	for _, rel := range lightExportCaches {
		if err := linkTree(cache, filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	return coldLightOptions(t, dir)
}

// linkTree recreates src's directories under dst and links (or, across
// file systems, copies) its regular files.
func linkTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := os.Link(p, target); err == nil {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
}

func TestDevLightOracleReal(t *testing.T) {
	t.Parallel()
	seed := lightFixture(t)
	base := lightSeedBaseline(t)
	if !base.rec.Valid || !base.rec.Defect.Exact || base.rec.Coverage == nil {
		t.Fatalf("baseline %+v", base.rec)
	}
	cand := lightGoodCandidate(t, seed)
	reg := runLightRegression(realLightOptions(t, t.TempDir()), strings.Repeat("c", 40), strings.Repeat("d", 40), seed, cand, base.rec.Defect.Exact)
	if !reg.rec.Pass {
		t.Fatalf("regression %s %+v %+v %+v", reg.rec.Code, reg.rec.RedEvents, reg.rec.GreenEvents, reg.rec.Structure)
	}
}
