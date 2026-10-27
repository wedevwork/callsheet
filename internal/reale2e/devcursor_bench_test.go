package reale2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkDevFullCursorActivity (design 12a-3-cursor-worker, FP-3) is
// one bounded shallow observation phase of three synthetic roots of more
// than a million generated entries each (every sampled directory again a
// million-entry generator): ns/op, B/op and allocs/op, with the consumed
// entries, metadata operations and serialized bytes per phase reported
// and their caps verified on every iteration, independent of the
// generated cardinality. No host scan and no timing threshold. It is
// selected by the existing ^BenchmarkDevFull bench.
func BenchmarkDevFullCursorActivity(b *testing.B) {
	f := &synthFS{t: b, roots: map[string]*synthNode{"/c": bigDir(1_100_000, 1_100_000), "/t": bigDir(1_100_000, 1_100_000),
		"/v": bigDir(1_100_000, 1_100_000)}}
	specs := []activityRootSpec{spec(activityScopeCache, "/c"), spec(activityScopeTmp, "/t"), spec(activityScopeVarTmp, "/v")}
	clock := newTickClock(0)
	const maxConsumed = 3 * (activityMaxEntries + 1 + activityMaxEntries*(activityMaxEntries+1))
	const maxStats = 3 * (2 + activityMaxEntries + activityMaxEntries*(1+activityMaxEntries))
	var bytes int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		before, stats := f.consumed.Load(), f.stats.Load()
		agg := (activityObserver{fs: f, clock: clock, uid: 1000}).observe("run", "before", specs, clock.Now().Add(activityScanBudget))
		data, err := json.Marshal(agg)
		if err != nil {
			b.Fatal(err)
		}
		bytes = len(data)
		if c, s := f.consumed.Load()-before, f.stats.Load()-stats; c > maxConsumed || s > maxStats || bytes > activityMaxOutput {
			b.Fatalf("consumed %d (cap %d), metadata operations %d (cap %d), output %d bytes", c, maxConsumed, s, maxStats, bytes)
		}
	}
	b.StopTimer()
	n := float64(b.N)
	b.ReportMetric(float64(f.consumed.Load())/n, "entries/op")
	b.ReportMetric(float64(f.stats.Load())/n, "stats/op")
	b.ReportMetric(float64(bytes), "record-bytes")
}

// Slice 3b's offline-check benchmarks (design 12a-3-cursor-worker, 3b.md
// Benchmark tests): load plus independent check of a live-collected
// Cursor qualification (FP-1), full and light attempt (FP-2) and combined
// exercise root (FP-3), each prepared outside the timed loop with a
// maximum legal pair of shallow samples. Every iteration asserts the
// result and identical canonical derivations; the evidence stays
// byte-identical and its consumed bytes stay within the shared budgets. A
// malformed record padded near its bound exercises bounded rejection. No
// paid benchmark, execution or latency threshold.

// maxLegalSamples replaces dir's two samples with the largest legal ones
// the observer writes (each root at its entry and output bounds) and
// returns their bytes.
func maxLegalSamples(tb testing.TB, dir, run string) int {
	tb.Helper()
	n := 0
	for i, phase := range []string{"before", "after"} {
		f := &synthFS{t: tb, roots: map[string]*synthNode{"/c": bigDir(1_100_000, 1_100_000), "/t": bigDir(1_100_000, 1_100_000),
			"/v": bigDir(1_100_000, 1_100_000)}}
		clock := newTickClock(0)
		o := activityObserver{fs: f, clock: clock, uid: 1000}
		agg := o.observe(run, phase, []activityRootSpec{{Scope: activityScopeCache, Path: "/c", Token: "$HOME/.cache"},
			{Scope: activityScopeTmp, Path: "/t", Token: "/tmp"}, {Scope: activityScopeVarTmp, Path: "/v", Token: "/var/tmp"}}, clock.Now().Add(activityScanBudget))
		data, err := json.Marshal(agg)
		if err != nil {
			tb.Fatal(err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(cursorActivityFiles[i])), data, 0o600); err != nil {
			tb.Fatal(err)
		}
		n += len(data)
	}
	return n
}

// malformedNearLimit replaces dir's before sample with an unterminated
// record just below its 256 KiB bound.
func malformedNearLimit(tb testing.TB, dir string) {
	tb.Helper()
	head := `{"schema":"` + ActivitySchema + `","run_id":"`
	data := []byte(head + strings.Repeat("a", activityMaxOutput-len(head)-1))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(cursorActivityFiles[0])), data, 0o600); err != nil {
		tb.Fatal(err)
	}
}

// evidenceBytes is the total regular-file bytes below dir.
func evidenceBytes(tb testing.TB, dir string) int64 {
	tb.Helper()
	var n int64
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if st, err := d.Info(); err == nil {
				n += st.Size()
			}
		}
		return nil
	})
	return n
}

func BenchmarkDevFullCursorQualification(b *testing.B) {
	dir := cursorQualifiedBundle(b, probeScopeBoth)
	q := bundleDoc[DevQualification](b, dir, devQualificationFile)
	samples := maxLegalSamples(b, dir, q.RunID)
	requalify(b, dir)
	bad := filepath.Join(b.TempDir(), "bad")
	if err := copyTree(dir, bad); err != nil {
		b.Fatal(err)
	}
	malformedNearLimit(b, bad)
	b.Run("qualified", func(b *testing.B) {
		before, size := treeFingerprint(b, dir), evidenceBytes(b, dir)
		if size > maxTextTotal+maxRepoFileBytes {
			b.Fatalf("the evidence is %d bytes, beyond the shared budgets", size)
		}
		var first []byte
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rep := cursorQualificationReportAt(dir)
			got, _ := encodeJSON(rep)
			if rep.Result != ResultQualified || first != nil && !bytes.Equal(got, first) {
				b.Fatalf("result %s codes %v", rep.Result, rep.Codes)
			}
			first = got
		}
		b.StopTimer()
		if treeFingerprint(b, dir) != before {
			b.Fatal("the check changed the evidence")
		}
		b.ReportMetric(float64(size), "evidence-bytes")
		b.ReportMetric(float64(samples), "sample-bytes")
	})
	b.Run("malformed_near_limit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := LoadDevQualification(bad); err == nil {
				b.Fatal("a malformed near-limit sample loaded")
			}
		}
	})
}

func BenchmarkDevFullCursorAttempt(b *testing.B) {
	for _, scenario := range []string{scenarioFull, scenarioLight} {
		dir := cursorAttemptBundle(b, scenario)
		m := bundleDoc[DevManifest](b, dir, devManifestFile)
		samples := maxLegalSamples(b, dir, m.RunID)
		reforge(b, dir)
		bad := filepath.Join(b.TempDir(), "bad")
		if err := copyTree(dir, bad); err != nil {
			b.Fatal(err)
		}
		malformedNearLimit(b, bad)
		b.Run(scenario, func(b *testing.B) {
			before, size := treeFingerprint(b, dir), evidenceBytes(b, dir)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bb, err := LoadDevBundle(dir)
				if err != nil {
					b.Fatal(err)
				}
				res := checkDevBundle(bb, false)
				if res.report.Result != ResultPass || !bytes.Equal(res.reportJSON, bb.Files["report.json"]) || res.report.Activity == nil {
					b.Fatalf("%s: %v", scenario, devCodes(res.report))
				}
			}
			b.StopTimer()
			if treeFingerprint(b, dir) != before {
				b.Fatal("the check changed the evidence")
			}
			b.ReportMetric(float64(size), "evidence-bytes")
			b.ReportMetric(float64(samples), "sample-bytes")
		})
		b.Run(scenario+"/malformed_near_limit", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := LoadDevBundle(bad); err == nil {
					b.Fatal("a malformed near-limit sample loaded")
				}
			}
		})
	}
}

// maxSampleRows asserts that dir's two samples are maximum legal ones:
// every root at its 64-row entry bound.
func maxSampleRows(tb testing.TB, dir string) {
	tb.Helper()
	for _, rel := range cursorActivityFiles {
		var a ActivityAggregate
		if err := decodeStrict(mustRead(tb, filepath.Join(dir, filepath.FromSlash(rel))), &a); err != nil {
			tb.Fatal(err)
		}
		for _, r := range a.Roots {
			if len(r.Rows) != activityMaxEntries || !r.Flags.EntryLimited {
				tb.Fatalf("%s %s: %d rows, not a maximum legal sample", dir, rel, len(r.Rows))
			}
		}
	}
}

func BenchmarkDevFullCursorExercise(b *testing.B) {
	root := cursorExerciseRoot(b)
	// Both children carry maximum legal samples: each child's samples are
	// replaced and its bound documents (inventory, report, audit) and the
	// root's child digests and derived files regenerated, all outside the
	// timed loop.
	samples := 0
	for i, rel := range []string{exerciseFullDir, exerciseLightDir} {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		m := bundleDoc[DevManifest](b, dir, devManifestFile)
		samples += maxLegalSamples(b, dir, m.RunID)
		reforge(b, dir)
		sum := sha256Hex(mustRead(b, filepath.Join(dir, devManifestFile)))
		editJSON(b, root, devExerciseFile, func(ex map[string]any) {
			ex["children"].([]any)[i].(map[string]any)["manifest_sha256"] = sum
		})
		maxSampleRows(b, dir)
	}
	rewriteExerciseDerived(b, root)
	before := treeFingerprint(b, root)
	var first []byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := checkExerciseRoot(root, false)
		if err != nil || res.report.Result != ResultPass || res.report.Schema != devCursorExerciseReportSchema || first != nil && !bytes.Equal(res.reportJSON, first) {
			b.Fatalf("%v %+v", err, res.report.Codes)
		}
		first = res.reportJSON
	}
	b.StopTimer()
	if treeFingerprint(b, root) != before {
		b.Fatal("the check changed the evidence")
	}
	b.ReportMetric(float64(evidenceBytes(b, root)), "evidence-bytes")
	b.ReportMetric(float64(samples), "sample-bytes")
}
