package client

import (
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// BenchmarkWaitUntilDone (design nonblocking-coordinator-waits) runs the
// renewable wait's loop on the fake single-attempt connection and the
// event-armed fake clock: no real hour, process, network or timer. For 1
// and 16 IDs it measures 120 full 30 s slices (one synthetic hour), 600
// capped 100 ms slices, ten immediate snapshots (start pacing), ten
// failures through the capped backoff and an immediate winner, and asserts
// each run's exact request and timer schedule, the bounded final answer
// (terminal, no rows) and that nothing is retained: no armed timer, the
// connection closed once and no goroutine left. Timings are reported,
// never gated.
func BenchmarkWaitUntilDone(b *testing.B) {
	ms := time.Millisecond
	cases := []struct {
		name   string
		script func(ids []string) []udStep
		timers []time.Duration
	}{
		{"hour", func(ids []string) []udStep {
			return append(steps(120, udStep{resp: udStill(ids, 30000), took: 30 * time.Second}), udStep{resp: udTerminal(ids[len(ids)-1], 10)})
		}, nil},
		{"capped-100ms", func(ids []string) []udStep {
			return append(steps(600, udStep{resp: udStill(ids, 100), took: 100 * ms}), udStep{resp: udTerminal(ids[0], 100)})
		}, nil},
		{"snapshots", func(ids []string) []udStep {
			return append(steps(10, udStep{resp: udStill(ids, 0)}), udStep{resp: udTerminal(ids[0], 0)})
		}, slices.Repeat([]time.Duration{100 * ms}, 10)},
		{"failures", func(ids []string) []udStep {
			return append(steps(10, udStep{err: udUnavailable()}), udStep{resp: udTerminal(ids[0], 0)})
		}, []time.Duration{250 * ms, 500 * ms, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second}},
		{"winner", func(ids []string) []udStep { return []udStep{{resp: udTerminal(ids[0], 0)}} }, nil},
	}
	for _, n := range []int{1, 16} {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("t_%032x", i+1)
		}
		for _, c := range cases {
			script := c.script(ids)
			b.Run(fmt.Sprintf("ids=%d/%s", n, c.name), func(b *testing.B) {
				goroutines := runtime.NumGoroutine()
				requests, timers := 0, 0
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					h := newUDHarness(script...)
					r, err := h.u.run(bg, udOptions, ids)
					armed, live := h.clock.timers()
					switch {
					case err != nil || r.Status != contract.WaitTerminal || r.Task == nil || r.Tasks != nil:
						b.Fatalf("answer %+v %v", r, err)
					case len(h.conn.starts) != len(script) || !slices.Equal(armed, c.timers):
						b.Fatalf("%d requests, timers %v", len(h.conn.starts), armed)
					case live != 0 || h.conn.closed != 1 || h.resolves != 1:
						b.Fatalf("retained: %d timers armed, connection closed %d times", live, h.conn.closed)
					}
					requests += len(h.conn.starts)
					timers += len(armed)
				}
				b.ReportMetric(float64(requests)/float64(b.N), "requests/op")
				b.ReportMetric(float64(timers)/float64(b.N), "timers/op")
				if g := runtime.NumGoroutine(); g > goroutines {
					b.Fatalf("%d goroutines left (from %d)", g, goroutines)
				}
			})
		}
	}
}
