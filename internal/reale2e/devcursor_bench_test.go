package reale2e

import (
	"encoding/json"
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
