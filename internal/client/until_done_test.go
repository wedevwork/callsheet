package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Design nonblocking-coordinator-waits, the renewable wait's client
// contracts: UT-2 (FP-2: bounds, renewal, pacing, backoff, trust and
// retention), UT-3 (FP-3: the retry class and cancellation at every await)
// and UT-4's client half (FP-4: no still_running answer is ever returned).
// The long-duration matrix runs on an injected, event-armed fake clock and
// a scripted single-attempt connection (no real time passes and no timer
// is fired before it is armed); TestWaitUntilDoneTransport adds the few
// real TLS attempts that tie the loop to the verified transport.

// udFakeClock is the event-armed fake clock of one call: arming a timer
// is the event, and only then does the clock move to the timer's deadline
// and fire it (the loop is synchronous: no timer is ever advanced before
// it exists). onArm, when set, sees the n-th armed delay (from 1) first
// and decides whether the timer fires.
type udFakeClock struct {
	mu    sync.Mutex
	now   time.Time
	armed []time.Duration
	live  int
	onArm func(n int, d time.Duration) (fire bool)
}

func newUDClock() *udFakeClock {
	return &udFakeClock{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *udFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *udFakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *udFakeClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	c.armed = append(c.armed, d)
	c.live++
	n, hook := len(c.armed), c.onArm
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	fire := hook == nil || hook(n, d)
	if fire {
		c.add(d)
		ch <- c.Now()
	}
	var once sync.Once
	return ch, func() bool {
		once.Do(func() {
			c.mu.Lock()
			c.live--
			c.mu.Unlock()
		})
		return !fire
	}
}

func (c *udFakeClock) timers() ([]time.Duration, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.armed...), c.live
}

// udStep is one scripted attempt: its answer or error, the time it takes
// on the fake clock and an optional action at its start.
type udStep struct {
	resp contract.WaitResponse
	err  error
	took time.Duration
	at   func()
}

// udFakeConn is a scripted single-attempt connection.
type udFakeConn struct {
	clock  *udFakeClock
	steps  []udStep
	starts []time.Time
	bodies []string
	closed int
}

func (f *udFakeConn) waitOnce(ctx context.Context, ids []string, body []byte) (contract.WaitResponse, error) {
	f.starts = append(f.starts, f.clock.Now())
	f.bodies = append(f.bodies, string(body))
	if len(f.starts) > len(f.steps) {
		return contract.WaitResponse{}, errors.New("the attempt script is exhausted")
	}
	s := f.steps[len(f.starts)-1]
	if s.at != nil {
		s.at()
	}
	f.clock.add(s.took)
	return s.resp, s.err
}

func (f *udFakeConn) close() { f.closed++ }

// udHarness is one call on the fake clock and connection; resolveErrs are
// the first resolutions' errors (then success) and openErr the open
// error.
type udHarness struct {
	u           *untilDone
	clock       *udFakeClock
	conn        *udFakeConn
	resolveErrs []error
	resolves    int
	opens       int
	openErr     error
}

func newUDHarness(steps ...udStep) *udHarness {
	h := &udHarness{clock: newUDClock()}
	h.conn = &udFakeConn{clock: h.clock, steps: steps}
	h.u = &untilDone{clock: h.clock, slice: UntilDoneSlice, attempt: UntilDoneAttempt, pace: UntilDonePace,
		resolve: func(ctx context.Context, _ TrustOptions) (Trust, error) {
			h.resolves++
			if h.resolves <= len(h.resolveErrs) {
				return Trust{}, h.resolveErrs[h.resolves-1]
			}
			return Trust{}, nil
		},
		open: func(string, Trust) (waitConn, error) {
			h.opens++
			if h.openErr != nil {
				return nil, h.openErr
			}
			return h.conn, nil
		}}
	return h
}

// udOptions are syntactically valid trust options.
var udOptions = TrustOptions{PlaneURL: "https://127.0.0.1:7443", CAFile: "/ca.crt"}

func (h *udHarness) run(ctx context.Context, ids ...string) (contract.WaitResponse, error) {
	return h.u.run(ctx, udOptions, ids)
}

func udStill(ids []string, eff int) contract.WaitResponse {
	rows := make([]contract.WaitRow, len(ids))
	for i, id := range ids {
		rows[i] = contract.WaitRow{TaskID: id, State: contract.TaskRunning, ElapsedMS: 5, DurabilityConfirmed: true}
	}
	return contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitStillRunning, EffectiveWaitMS: eff, Tasks: rows}
}

func udTerminal(id string, eff int) contract.WaitResponse {
	v := taskView(id, taskRequest("done"), contract.TaskSucceeded)
	return contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: eff, Winner: id, Task: &v}
}

func udUnavailable() error {
	return contract.New(contract.CodeUnavailable, "cannot reach the plane: the connection was closed")
}

// steps repeats s n times.
func steps(n int, s udStep) []udStep {
	out := make([]udStep, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// done checks the common end state: the connection closed exactly once
// (when it was opened) and no timer left armed.
func (h *udHarness) done(t *testing.T) []time.Duration {
	t.Helper()
	armed, live := h.clock.timers()
	if live != 0 || (h.opens > 0 && h.openErr == nil && h.conn.closed != 1) {
		t.Fatalf("%d timers still armed, connection closed %d times", live, h.conn.closed)
	}
	return armed
}

func wantDurations(t *testing.T, what string, got []time.Duration, want ...time.Duration) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// TestWaitUntilDoneContract is the renewable wait's injected contract,
// delegated from tests/function (TestWaitUntilDoneRenewal,
// TestWaitUntilDoneFailure, TestWaitUntilDoneOutput). Do not rename or
// skip it.
func TestWaitUntilDoneContract(t *testing.T) {
	ids := []string{taskA, taskB}
	t.Run("bounds", func(t *testing.T) {
		u := newUntilDone()
		if u.slice != 30*time.Second || u.attempt != 40*time.Second || u.pace != 100*time.Millisecond || UntilDoneAttempt != UntilDoneSlice+OperationTimeout {
			t.Fatalf("bounds %v %v %v", u.slice, u.attempt, u.pace)
		}
		var got []time.Duration
		for n := 0; n < 9; n++ {
			got = append(got, UntilDoneBackoff(n))
		}
		wantDurations(t, "backoff", got, 250*time.Millisecond, 500*time.Millisecond, time.Second, 2*time.Second, 4*time.Second, 5*time.Second,
			5*time.Second, 5*time.Second, 5*time.Second)
		if UntilDoneBackoff(-1) != 250*time.Millisecond {
			t.Fatal("negative backoff index")
		}
		// Every request asks for exactly the slice, in caller order.
		h := newUDHarness(udStep{resp: udTerminal(taskB, 0)})
		if _, err := h.run(bg, taskB, taskA); err != nil {
			t.Fatal(err)
		}
		if h.conn.bodies[0] != `{"task_ids":["`+taskB+`","`+taskA+`"],"wait":"30s"}` {
			t.Fatalf("request %s", h.conn.bodies[0])
		}
	})
	t.Run("renewal", func(t *testing.T) {
		// 120 full slices (one synthetic hour) then the winner: one request
		// at a time, the same body each time, no pacing timer (each slice
		// outlasts the pace) and only the final answer returned.
		h := newUDHarness(append(steps(120, udStep{resp: udStill(ids, 30000), took: 30 * time.Second}), udStep{resp: udTerminal(taskB, 1200), took: 1200 * time.Millisecond})...)
		r, err := h.run(bg, ids...)
		if err != nil || r.Status != contract.WaitTerminal || r.Winner != taskB || r.EffectiveWaitMS != 1200 || r.Tasks != nil {
			t.Fatalf("renewal %+v %v", r, err)
		}
		if len(h.conn.starts) != 121 || h.resolves != 1 || h.opens != 1 {
			t.Fatalf("%d attempts, %d resolutions, %d opens", len(h.conn.starts), h.resolves, h.opens)
		}
		for _, b := range h.conn.bodies {
			if b != h.conn.bodies[0] {
				t.Fatalf("a renewal changed the request: %s", b)
			}
		}
		if armed := h.done(t); len(armed) != 0 {
			t.Fatalf("pacing timers %v", armed)
		}
		if got := h.clock.Now().Sub(h.conn.starts[0]); got != time.Hour+1200*time.Millisecond {
			t.Fatalf("synthetic hour %v", got)
		}
	})
	t.Run("pacing", func(t *testing.T) {
		// Immediate snapshots: at least 100 ms between attempt starts; a
		// 100 ms capped slice needs no extra delay; the winner is returned
		// with no pacing after it.
		h := newUDHarness(udStep{resp: udStill(ids, 0)}, udStep{resp: udStill(ids, 0), took: 30 * time.Millisecond}, udStep{resp: udStill(ids, 100), took: 100 * time.Millisecond},
			udStep{resp: udStill(ids, 100), took: 100 * time.Millisecond}, udStep{resp: udTerminal(taskA, 0)})
		if _, err := h.run(bg, ids...); err != nil {
			t.Fatal(err)
		}
		wantDurations(t, "pacing", h.done(t), 100*time.Millisecond, 70*time.Millisecond)
		for i := 1; i < len(h.conn.starts); i++ {
			if gap := h.conn.starts[i].Sub(h.conn.starts[i-1]); gap < 100*time.Millisecond {
				t.Fatalf("attempts %d and %d started %v apart", i-1, i, gap)
			}
		}
	})
	t.Run("backoff", func(t *testing.T) {
		// Seven failures ramp to the 5 s cap, measured after each failed
		// attempt; a valid still_running resets the ramp; failures that take
		// time are not shortened by it.
		var s []udStep
		s = append(s, steps(7, udStep{err: udUnavailable(), took: 3 * time.Second})...)
		s = append(s, udStep{resp: udStill(ids, 30000), took: 30 * time.Second})
		s = append(s, steps(2, udStep{err: udUnavailable()})...)
		s = append(s, udStep{resp: udTerminal(taskA, 10)})
		h := newUDHarness(s...)
		if _, err := h.run(bg, ids...); err != nil {
			t.Fatal(err)
		}
		ms := time.Millisecond
		wantDurations(t, "schedule", h.done(t), 250*ms, 500*ms, time.Second, 2*time.Second, 4*time.Second, 5*time.Second, 5*time.Second, 250*ms, 500*ms)
		if h.resolves != 1 || h.opens != 1 {
			t.Fatalf("trust was not retained: %d resolutions, %d opens", h.resolves, h.opens)
		}
		// The delay follows the failed attempt's end.
		if gap := h.conn.starts[1].Sub(h.conn.starts[0]); gap != 3*time.Second+250*ms {
			t.Fatalf("gap %v", gap)
		}
	})
	t.Run("trust", func(t *testing.T) {
		// The first resolution's outage is retried silently on the same
		// schedule; once resolved, trust and the client are retained across
		// later outages (a restarted plane with the same CA).
		h := newUDHarness(udStep{err: udUnavailable()}, udStep{resp: udStill(ids, 30000), took: 30 * time.Second}, udStep{resp: udTerminal(taskA, 5)})
		h.resolveErrs = []error{udUnavailable(), udUnavailable()}
		if _, err := h.run(bg, ids...); err != nil {
			t.Fatal(err)
		}
		ms := time.Millisecond
		wantDurations(t, "schedule", h.done(t), 250*ms, 500*ms, time.Second)
		if h.resolves != 3 || h.opens != 1 {
			t.Fatalf("%d resolutions, %d opens", h.resolves, h.opens)
		}
		// A trust failure (resolution or the client's own check) is final:
		// never a silent fetch of a replacement CA.
		for name, mut := range map[string]func(*udHarness){
			"resolve": func(h *udHarness) {
				h.resolveErrs = []error{udUnavailable(), untrustedErr("the plane's CA does not match --ca-fingerprint")}
			},
			"open": func(h *udHarness) {
				h.openErr = contract.New(contract.CodeTrustFailed, untrusted+": the configured CA does not match")
			},
		} {
			h := newUDHarness()
			mut(h)
			if _, err := h.run(bg, ids...); contract.CodeOf(err) != contract.CodeTrustFailed || len(h.conn.starts) != 0 {
				t.Fatalf("%s: %v after %d attempts", name, err, len(h.conn.starts))
			}
			h.done(t)
		}
	})
	t.Run("retry-class", func(t *testing.T) {
		// All and only unavailable is retried; every other code ends the
		// call at once with that error.
		for _, code := range []contract.Code{contract.CodeInvalidArgument, contract.CodeNotFound, contract.CodeConflict, contract.CodeTrustFailed,
			contract.CodeProtocolMismatch, contract.CodeNotImplemented, contract.CodeInternal} {
			h := newUDHarness(udStep{err: udUnavailable()}, udStep{err: contract.New(code, "permanent")}, udStep{resp: udTerminal(taskA, 0)})
			_, err := h.run(bg, ids...)
			if contract.CodeOf(err) != code || len(h.conn.starts) != 2 {
				t.Fatalf("%s: %v after %d attempts", code, err, len(h.conn.starts))
			}
			wantDurations(t, string(code), h.done(t), 250*time.Millisecond)
		}
		// An untyped error (not a contract error) is permanent too.
		h := newUDHarness(udStep{err: errors.New("boom")})
		if _, err := h.run(bg, ids...); err == nil || contract.CodeOf(err) == contract.CodeUnavailable {
			t.Fatalf("untyped %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		// The caller's cancellation at every await ends the call with its
		// own error, and wins over a ready timer or a ready answer.
		type cancelCase struct {
			name  string
			setup func(h *udHarness, cancel context.CancelFunc)
		}
		for _, c := range []cancelCase{
			{"before-start", func(h *udHarness, cancel context.CancelFunc) { cancel() }},
			{"during-resolve", func(h *udHarness, cancel context.CancelFunc) {
				h.u.resolve = func(ctx context.Context, _ TrustOptions) (Trust, error) {
					h.resolves++
					cancel()
					return Trust{}, udUnavailable()
				}
			}},
			{"during-resolve-backoff", func(h *udHarness, cancel context.CancelFunc) {
				h.resolveErrs = []error{udUnavailable()}
				h.clock.onArm = func(int, time.Duration) bool { cancel(); return false }
			}},
			{"during-attempt", func(h *udHarness, cancel context.CancelFunc) {
				h.conn.steps = []udStep{{err: udUnavailable(), at: cancel}}
			}},
			{"answer-and-cancel", func(h *udHarness, cancel context.CancelFunc) {
				h.conn.steps = []udStep{{resp: udTerminal(taskA, 0), at: cancel}}
			}},
			{"during-backoff", func(h *udHarness, cancel context.CancelFunc) {
				h.conn.steps = []udStep{{err: udUnavailable()}, {resp: udTerminal(taskA, 0)}}
				h.clock.onArm = func(int, time.Duration) bool { cancel(); return false }
			}},
			{"timer-and-cancel", func(h *udHarness, cancel context.CancelFunc) {
				h.conn.steps = []udStep{{err: udUnavailable()}, {resp: udTerminal(taskA, 0)}}
				h.clock.onArm = func(int, time.Duration) bool { cancel(); return true }
			}},
			{"during-pacing", func(h *udHarness, cancel context.CancelFunc) {
				h.conn.steps = []udStep{{resp: udStill(ids, 0)}, {resp: udTerminal(taskA, 0)}}
				h.clock.onArm = func(int, time.Duration) bool { cancel(); return false }
			}},
			{"permanent-outage", func(h *udHarness, cancel context.CancelFunc) {
				// A plane gone for good: failures at the capped backoff until
				// the caller stops the call.
				h.conn.steps = steps(50, udStep{err: udUnavailable()})
				h.clock.onArm = func(n int, _ time.Duration) bool {
					if n == 40 {
						cancel()
						return false
					}
					return true
				}
			}},
		} {
			t.Run(c.name, func(t *testing.T) {
				h := newUDHarness(udStep{resp: udTerminal(taskA, 0)})
				ctx, cancel := context.WithCancel(bg)
				defer cancel()
				c.setup(h, cancel)
				r, err := h.run(ctx, ids...)
				if !errors.Is(err, context.Canceled) || r.Status != "" {
					t.Fatalf("%+v %v", r, err)
				}
				h.done(t)
				if c.name == "permanent-outage" && (len(h.conn.starts) != 40 || h.resolves != 1) {
					t.Fatalf("outage: %d attempts, %d resolutions", len(h.conn.starts), h.resolves)
				}
			})
		}
	})
	t.Run("validation", func(t *testing.T) {
		// Invalid IDs and trust syntax fail before any resolution.
		many := make([]string, contract.MaxWaitIDs+1)
		for i := range many {
			many[i] = fmt.Sprintf("t_%032x", i+1)
		}
		for name, c := range map[string]struct {
			ids []string
			o   TrustOptions
		}{
			"none":      {nil, udOptions},
			"seventeen": {many, udOptions},
			"duplicate": {[]string{taskA, taskA}, udOptions},
			"malformed": {[]string{"t_X"}, udOptions},
			"url":       {ids, TrustOptions{PlaneURL: "http://plane", CAFile: "/ca"}},
			"both":      {ids, TrustOptions{PlaneURL: udOptions.PlaneURL, CAFile: "/ca", CAFingerprint: "sha256:" + strings.Repeat("a", 64)}},
			"pin":       {ids, TrustOptions{PlaneURL: udOptions.PlaneURL, CAFingerprint: "sha256:abc"}},
		} {
			h := newUDHarness()
			if _, err := h.u.run(bg, c.o, c.ids); contract.CodeOf(err) != contract.CodeInvalidArgument || h.resolves != 0 {
				t.Fatalf("%s: %v (%d resolutions)", name, err, h.resolves)
			}
		}
		if err := checkTrustOptions(TrustOptions{PlaneURL: udOptions.PlaneURL, CAFingerprint: "sha256:" + strings.Repeat("a", 64)}); err != nil {
			t.Fatal(err)
		}
	})
}

// udServer is a verified test plane for the real-transport cases: its
// handler is set per case, and its CA is on disk for real trust
// resolution.
type udServer struct {
	url, caFile string
	ca          []byte
	mu          sync.Mutex
	h           http.HandlerFunc
	bodies      []string
}

func startUDServer(t *testing.T) *udServer {
	t.Helper()
	ca := newTestCA(t)
	s := &udServer{}
	srv := startServer(t, ca.leaf(t, true), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == contract.PathCA {
			w.Write(ca.pem)
			return
		}
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		h := s.h
		s.mu.Unlock()
		h(w, r)
	}))
	s.url, s.ca = srv.url, ca.pem
	s.caFile = filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(s.caFile, ca.pem, 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

// script answers the n-th request (from 0) with hs[n].
func (s *udServer) script(hs ...http.HandlerFunc) {
	var n atomic.Int64
	s.mu.Lock()
	s.bodies = nil
	s.h = func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(hs) {
			i = len(hs) - 1
		}
		hs[i](w, r)
	}
	s.mu.Unlock()
}

func (s *udServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func (s *udServer) options() TrustOptions { return TrustOptions{PlaneURL: s.url, CAFile: s.caFile} }

func encodedWait(t testing.TB, r contract.WaitResponse) string {
	t.Helper()
	b, err := contract.EncodeWaitResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// realUD is a call over the real client for trust (resolved once by the
// caller) on the fake clock: its backoff and pacing never sleep in real
// time, and every attempt is one real TLS request.
func realUD(trust Trust) (*untilDone, *udFakeClock) {
	u := newUntilDone()
	c := newUDClock()
	u.clock = c
	u.resolve = func(context.Context, TrustOptions) (Trust, error) { return trust, nil }
	return u, c
}

// TestWaitUntilDoneTransport is the renewable wait over a real verified
// TLS server: one retained client across a
// dropped connection, typed and unstructured 503 and capacity answers
// (retried) and an unstructured 500 (final); a durably unconfirmed winner
// is an invalid answer; an answer decoded after the attempt deadline is
// discarded; the attempt's own deadline is retryable; the caller's
// cancellation of an outstanding request is its own error. Real attempts
// are few (one TLS handshake each); the schedule matrix is the contract's.
func TestWaitUntilDoneTransport(t *testing.T) {
	s := startUDServer(t)
	ids := []string{taskA, taskB}
	term := encodedWait(t, udTerminal(taskB, 2))
	// The CLI and function tests drive the exported entry point with real
	// trust resolution; here the trust is the server's CA, so each case
	// costs only its attempts' handshakes.
	trust := Trust{CAPEM: s.ca}
	t.Run("classes", func(t *testing.T) {
		u, c := realUD(trust)
		var conn *clientWaitConn
		u.open = func(url string, tr Trust) (waitConn, error) {
			w, err := u.openClient(url, tr)
			conn, _ = w.(*clientWaitConn)
			return w, err
		}
		late := func(w http.ResponseWriter, r *http.Request) {
			c.add(time.Minute)
			jsonRoute("6", 200, encodedWait(t, udStill(ids, 30000)))(w, r)
		}
		s.script(hangUp, jsonRoute("6", 503, `{"error":{"code":"unavailable","message":"full","details":{"reason":"wait_capacity"}}}`),
			jsonRoute("6", 503, "busy"), late, jsonRoute("6", 200, encodedWait(t, udStill(ids, 30000))), jsonRoute("6", 500, "oops"))
		_, err := u.run(bg, s.options(), ids)
		if contract.CodeOf(err) != contract.CodeInternal || conn == nil || conn.slice != 30*time.Second || conn.attempt != 40*time.Second {
			t.Fatalf("final 500: %v (connection %+v)", err, conn)
		}
		reqs := s.requests()
		if len(reqs) != 6 || reqs[0] != `{"task_ids":["`+taskA+`","`+taskB+`"],"wait":"30s"}` || slices.ContainsFunc(reqs, func(b string) bool { return b != reqs[0] }) {
			t.Fatalf("requests %v", reqs)
		}
		armed, live := c.timers()
		wantDurations(t, "schedule", armed, 250*time.Millisecond, 500*time.Millisecond, time.Second, 2*time.Second, 100*time.Millisecond)
		if live != 0 {
			t.Fatal("a timer is still armed")
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		// A terminal answer without confirmed durability is an invalid
		// answer (exit 2), never a winner, and never retried.
		u, _ := realUD(trust)
		s.script(jsonRoute("6", 200, strings.Replace(encodedWait(t, udTerminal(taskA, 0)), `"durability_confirmed":true`, `"durability_confirmed":false`, 1)))
		if _, err := u.run(bg, s.options(), ids); contract.CodeOf(err) != contract.CodeInvalidArgument || len(s.requests()) != 1 {
			t.Fatalf("unconfirmed winner: %v", err)
		}
	})
	t.Run("attempt-deadline", func(t *testing.T) {
		// An attempt that outlives its own deadline (shortened here) is
		// retryable unavailability, also when the plane already sent a
		// non-2xx status and its error body stalls (review C1); the next
		// attempt (its deadline restored once the stall was observed)
		// returns the winner. A malformed answer received before the
		// deadline stays permanent ("classes": the final 500).
		u, c := realUD(trust)
		u.attempt = 25 * time.Millisecond
		var conn *clientWaitConn
		u.open = func(url string, tr Trust) (waitConn, error) {
			w, err := u.openClient(url, tr)
			conn, _ = w.(*clientWaitConn)
			return w, err
		}
		var stalled atomic.Bool
		c.onArm = func(int, time.Duration) bool {
			if stalled.Load() {
				conn.attempt = UntilDoneAttempt
			}
			return true
		}
		s.script(func(w http.ResponseWriter, r *http.Request) {
			if stalled.Load() {
				jsonRoute("6", 200, term)(w, r)
				return
			}
			w.Header().Set(contract.ProtocolHeader, "6")
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"code":`)
			http.NewResponseController(w).Flush()
			stalled.Store(true)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		r, err := u.run(bg, s.options(), ids)
		armed, live := c.timers()
		if err != nil || r.Winner != taskB || !stalled.Load() || live != 0 || len(armed) == 0 || armed[0] != 250*time.Millisecond {
			t.Fatalf("stalled error body: %+v %v (stalled %v, timers %v)", r.Winner, err, stalled.Load(), armed)
		}
	})
	t.Run("late-error-response", func(t *testing.T) {
		// A malformed error answer (HTTP 500 without a valid body) that
		// completed after the attempt deadline on the attempt's clock is
		// retryable unavailability, like a late success, even before the
		// deadline context's own expiry is delivered (code review r2); one
		// completed before the deadline stays permanent ("classes").
		u, c := realUD(trust)
		s.script(func(w http.ResponseWriter, r *http.Request) {
			c.add(time.Minute)
			jsonRoute("6", 500, "oops")(w, r)
		}, jsonRoute("6", 200, term))
		r, err := u.run(bg, s.options(), ids)
		armed, _ := c.timers()
		if err != nil || r.Winner != taskB || len(s.requests()) != 2 || len(armed) != 1 || armed[0] != 250*time.Millisecond {
			t.Fatalf("late error response: %+v %v (requests %d, timers %v)", r.Winner, err, len(s.requests()), armed)
		}
	})
	t.Run("cancelled-request", func(t *testing.T) {
		// The caller's cancellation of an outstanding request is its own
		// error, even when the plane answers afterwards.
		u, _ := realUD(trust)
		ctx, cancel := context.WithCancel(bg)
		arrived := make(chan struct{})
		s.script(func(w http.ResponseWriter, r *http.Request) {
			close(arrived)
			<-r.Context().Done()
			jsonRoute("6", 200, term)(w, r)
		})
		go func() { <-arrived; cancel() }()
		if _, err := u.run(ctx, s.options(), ids); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled %v", err)
		}
	})
}
