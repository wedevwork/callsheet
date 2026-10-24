package reale2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Benchmarks of design 12a-dev-flow-light: the offline check of the
// maximum nine-task light history (valid and rejected), the coverage
// profile validation and integer comparison, the inert capture map and
// path rejection, and the combined exercise root over two maximum child
// histories. Their DevFull prefix enrolls them in the existing benchmark
// selection; each iteration checks its verdict and starts no process.

// lightMaxSteps is the maximum light history: nine workflow tasks (two
// fix/review pairs, a NEEDS_TRIAGE re-triage and the resumed fix).
func lightMaxSteps() []devAct {
	conf := devAct{marker: markerTriageConf, designRequired: designRequiredNo}
	return []devAct{conf, {marker: markerImplComplete}, {marker: markerReviewChanges}, {marker: markerFixesComplete}, {marker: markerReviewChanges},
		{marker: markerNeedsTriage}, conf, {marker: markerFixesComplete}, {marker: markerReviewApproved}}
}

// lightMaxHistory and exerciseMaxHistory are the cached maximum histories.
var (
	lightMaxHistory struct {
		sync.Once
		dir string
		err error
	}
	exerciseMaxHistory struct {
		sync.Once
		dir string
		err error
	}
)

// lightMaxBundle is a fresh copy of the nine-task light bundle.
func lightMaxBundle(tb testing.TB) string {
	tb.Helper()
	lightMaxHistory.Do(func() {
		r := newLightRig(tb, lightMaxSteps())
		if code := r.run(); code != 0 || len(r.d.state.Tasks) != maxLightTasks {
			lightMaxHistory.err = fmt.Errorf("the nine-task light history failed (%d, %d tasks): %s", code, len(r.d.state.Tasks), r.d.stopCode)
			return
		}
		lightMaxHistory.dir = filepath.Join(bundleCacheRoot, "light-max")
		lightMaxHistory.err = copyTree(r.f.bundle(), lightMaxHistory.dir)
	})
	if lightMaxHistory.err != nil {
		tb.Fatal(lightMaxHistory.err)
	}
	dst := filepath.Join(tb.TempDir(), "light-max")
	if err := copyTree(lightMaxHistory.dir, dst); err != nil {
		tb.Fatal(err)
	}
	return dst
}

// exerciseMaxRoot is a fresh copy of an exercise root whose children are
// the twelve-task full history and the nine-task light history.
func exerciseMaxRoot(tb testing.TB) string {
	tb.Helper()
	exerciseMaxHistory.Do(func() {
		x := newExerciseRig(tb, maxTasksHistory.steps, lightMaxSteps())
		x.full.owner = maxTasksHistory.owner
		if code := x.run(); code != 0 {
			exerciseMaxHistory.err = fmt.Errorf("the maximum exercise failed (%d): %s", code, x.full.f.stderr.String())
			return
		}
		exerciseMaxHistory.dir = filepath.Join(bundleCacheRoot, "exercise-max")
		exerciseMaxHistory.err = copyTree(x.root(), exerciseMaxHistory.dir)
	})
	if exerciseMaxHistory.err != nil {
		tb.Fatal(exerciseMaxHistory.err)
	}
	dst := filepath.Join(tb.TempDir(), "exercise-max")
	if err := copyTree(exerciseMaxHistory.dir, dst); err != nil {
		tb.Fatal(err)
	}
	return dst
}

func BenchmarkDevFullLightCheck(b *testing.B) {
	valid, rejected := lightMaxBundle(b), lightMaxBundle(b)
	editJSON(b, rejected, lightReceiptFile, func(m map[string]any) { m["data"].(map[string]any)["request_sha256"] = strings.Repeat("0", 64) })
	reforge(b, rejected)
	vb, err := LoadDevBundle(valid)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(2 * bundleBytes(vb))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, c := range []struct {
			dir  string
			pass bool
		}{{valid, true}, {rejected, false}} {
			bundle, err := LoadDevBundle(c.dir)
			if err != nil {
				b.Fatal(err)
			}
			res := checkDevBundle(bundle, false)
			if (res.report.Result == ResultPass) != c.pass || res.report.Tasks != maxLightTasks {
				b.Fatalf("%s: %s %d %v", c.dir, res.report.Result, res.report.Tasks, devCodes(res.report))
			}
			if !c.pass && devCriterion(res.report, "light_intake").Result != ResultCriterionFail {
				b.Fatal("the counterfeit intake was not rejected")
			}
		}
	}
}

func BenchmarkDevFullLightCoverage(b *testing.B) {
	main := lightFixture(b)["main.go"]
	profile := synthProfile("example.com/greeting", main)
	b.SetBytes(int64(len(profile) + len(main)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base, err := deriveWholeCoverage(main, profile, "example.com/greeting")
		if err != nil || base.Total == 0 {
			b.Fatalf("%+v %v", base, err)
		}
		cand := base
		cand.Covered--
		if cmp := compareLightCoverage(base, base); !cmp.Pass {
			b.Fatal("equal fractions failed")
		}
		if cmp := compareLightCoverage(base, cand); cmp.Pass || cmp.Code != CodeCoverageDecreased {
			b.Fatal("a decrease passed")
		}
	}
}

func BenchmarkDevFullCaptureMap(b *testing.B) {
	logical := append(devCaptureLogical(scenarioLight), probeCaptureLogical(probeScopeBoth)...)
	layout := devLayoutFor(devBundleV2, scenarioLight)
	rejected := []string{"setup/sources/" + DevFlowDir + "/" + DevPlaybook, "_captured/setup/light-fixture/MAIN.go.bin", "_captured/x.go", "setup/x.go",
		"_captured/setup/light-fixture/main.go.txt"}
	var n int64
	for _, l := range logical {
		n += int64(len(l))
	}
	b.SetBytes(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var physical []string
		for _, l := range logical {
			physical = append(physical, capturedPath(l))
		}
		seen := map[string]bool{}
		var unique []string
		for _, p := range physical {
			if !seen[p] {
				seen[p] = true
				unique = append(unique, p)
			}
		}
		if err := checkCaptureFold(unique); err != nil {
			b.Fatal(err)
		}
		for _, p := range unique[:len(devCaptureLogical(scenarioLight))] {
			if !layout.file(p) {
				b.Fatalf("%s refused", p)
			}
		}
		for _, p := range rejected {
			if layout.file(p) {
				b.Fatalf("%s accepted", p)
			}
		}
		if checkCaptureFold([]string{"_captured/A.bin", "_captured/a.bin"}) == nil {
			b.Fatal("a fold collision passed")
		}
	}
}

func BenchmarkDevFullExerciseCheck(b *testing.B) {
	root := exerciseMaxRoot(b)
	var n int64
	for _, dir := range []string{exerciseFullDir, exerciseLightDir} {
		bundle, err := LoadDevBundle(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			b.Fatal(err)
		}
		n += bundleBytes(bundle)
	}
	b.SetBytes(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := checkExerciseRoot(root, false)
		if err != nil || res.report.Result != ResultPass || res.report.Accounting.WorkerCalls > exerciseMaxCalls {
			b.Fatalf("%v %+v", err, res.report)
		}
	}
	if _, err := os.Stat(filepath.Join(root, devExerciseFile)); err != nil {
		b.Fatal(err)
	}
}
