package mcpqual

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// testWait bounds every event-armed wait in real time; it never times the
// code under test.
const testWait = 10 * time.Second

var epoch = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

// lineSink collects whole-line writes and lets tests await the n-th.
type lineSink struct {
	mu      sync.Mutex
	lines   []string
	changed chan struct{}
	failAt  int // fail the n-th write (1-based) when > 0
}

func newSink() *lineSink { return &lineSink{changed: make(chan struct{})} }

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAt > 0 && len(s.lines)+1 == s.failAt {
		return 0, errors.New("sink: write failed")
	}
	for _, l := range strings.SplitAfter(string(p), "\n") {
		if l != "" {
			s.lines = append(s.lines, strings.TrimSuffix(l, "\n"))
		}
	}
	close(s.changed)
	s.changed = make(chan struct{})
	return len(p), nil
}

func (s *lineSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// await blocks until cond holds for the collected lines.
func (s *lineSink) await(t *testing.T, what string, cond func([]string) bool) []string {
	t.Helper()
	deadline := time.NewTimer(testWait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		lines := append([]string(nil), s.lines...)
		ch := s.changed
		s.mu.Unlock()
		if cond(lines) {
			return lines
		}
		select {
		case <-ch:
		case <-deadline.C:
			t.Fatalf("%s not observed; lines %q", what, lines)
		}
	}
}

func hasLine(sub string) func([]string) bool {
	return func(ls []string) bool {
		for _, l := range ls {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}
}

func countLines(sub string, n int) func([]string) bool {
	return func(ls []string) bool {
		c := 0
		for _, l := range ls {
			if strings.Contains(l, sub) {
				c++
			}
		}
		return c >= n
	}
}

// probeHarness runs Serve over a pipe with a fake clock.
type probeHarness struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *lineSink
	events *lineSink
	clock  *testkit.FakeClock
	done   chan error
}

func testCases() CaseFile {
	return CaseFile{Version: 1, RunID: "run1", Nonce: "nonce1", Cases: []ProbeCase{
		{CaseID: "slow", DelayMS: 4000, ProgressIntervalMS: 1000},
		{CaseID: "silent", DelayMS: 3000},
		{CaseID: "zero"},
		{CaseID: "gated", DelayMS: 3000, ProgressIntervalMS: 1000, MinProgress: 1},
		{CaseID: "gated2", DelayMS: 1000, ProgressIntervalMS: 1000, MinProgress: 2},
	}}
}

func startProbe(t *testing.T, cf CaseFile, mutate ...func(*ProbeConfig)) *probeHarness {
	t.Helper()
	pr, pw := io.Pipe()
	h := &probeHarness{t: t, in: pw, out: newSink(), events: newSink(), clock: testkit.NewFakeClock(epoch), done: make(chan error, 1)}
	cfg := ProbeConfig{Cases: cf, In: pr, Out: h.out, Events: h.events, Clock: h.clock, Version: "test"}
	for _, m := range mutate {
		m(&cfg)
	}
	go func() { h.done <- Serve(cfg) }()
	t.Cleanup(func() {
		pw.Close()
		pr.Close()
		select {
		case <-h.done:
		case <-time.After(testWait):
			t.Error("probe did not end")
		}
	})
	return h
}

func (h *probeHarness) send(lines ...string) {
	h.t.Helper()
	for _, l := range lines {
		if _, err := io.WriteString(h.in, l+"\n"); err != nil {
			h.t.Fatalf("send: %v", err)
		}
	}
}

// ready completes initialize and notifications/initialized.
func (h *probeHarness) ready() {
	h.t.Helper()
	h.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-client","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.out.await(h.t, "initialize answer", hasLine(`"id":0,"result"`))
	h.send(`{"jsonrpc":"2.0","id":"ping-ready","method":"ping"}`)
	h.out.await(h.t, "ping", hasLine(`"id":"ping-ready","result":{}`))
}

func (h *probeHarness) call(id, caseID, meta string) {
	h.t.Helper()
	m := ""
	if meta != "" {
		m = `,"_meta":` + meta
	}
	h.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"slow","arguments":{"case_id":"` + caseID + `"}` + m + `}}`)
}

// end closes stdin and returns Serve's result.
func (h *probeHarness) end() error {
	h.t.Helper()
	h.in.Close()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(testWait):
		h.t.Fatal("probe did not end")
		return nil
	}
}

func (h *probeHarness) awaitTimer(d time.Duration) {
	h.t.Helper()
	if err := h.clock.AwaitWaiter(testWait, testkit.HasTimer(d)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *probeHarness) awaitTicker(d time.Duration) {
	h.t.Helper()
	if err := h.clock.AwaitWaiter(testWait, testkit.HasTicker(d)); err != nil {
		h.t.Fatal(err)
	}
}

func parseEvents(t *testing.T, lines []string) []ProbeEvent {
	t.Helper()
	evs, _, err := ParseProbeEvents([]byte(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func kinds(evs []ProbeEvent) string {
	var ks []string
	for _, e := range evs {
		ks = append(ks, e.Kind)
	}
	return strings.Join(ks, ",")
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contains2(b []byte, sub string) bool { return bytes.Contains(b, []byte(sub)) }

func planJSON(t *testing.T, p *Plan) []byte { return mustJSON(t, p) }
