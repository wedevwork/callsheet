package client

import (
	"context"
	"net/http"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The renewable any-of wait (design nonblocking-coordinator-waits, FP-2):
// callsheet task wait --until-done. It has no overall deadline: one
// bounded plane wait at a time (a 30 s slice, a 40 s attempt deadline),
// renewed after every still_running answer and retried after every
// unavailable failure with a capped backoff, until a task of ids ends
// durably, a permanent error arrives or the caller cancels. A plane gone
// for good is indistinguishable from an outage: the call then waits,
// silently, until it is cancelled. Its wider retry class (every
// unavailable answer, trust resolution included) is confined to this file;
// the bounded WaitTasks keeps its transport-only retry rule.

// Renewable wait bounds.
const (
	// UntilDoneSlice is the wait each request asks the plane for (the plane
	// still applies its own --max-task-wait cap).
	UntilDoneSlice = 30 * time.Second
	// UntilDoneAttempt is one attempt's deadline: the slice plus the
	// ordinary transport margin.
	UntilDoneAttempt = UntilDoneSlice + waitMargin
	// UntilDonePace is the least time between two attempt starts.
	UntilDonePace = 100 * time.Millisecond
)

// UntilDoneBackoff returns the delay after the n-th (from 0) consecutive
// unavailable failure, measured after the failed attempt: 250 ms, 500 ms,
// 1 s, 2 s, 4 s, then 5 s repeatedly.
func UntilDoneBackoff(n int) time.Duration {
	schedule := [...]time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second}
	return schedule[min(max(n, 0), len(schedule)-1)]
}

// waitConn is one resolved plane connection: it performs single bounded
// wait attempts and owns its transports.
type waitConn interface {
	waitOnce(ctx context.Context, ids []string, body []byte) (contract.WaitResponse, error)
	close()
}

// untilDone is one renewable wait call with its injected operations.
type untilDone struct {
	// clock is the call's own time source (the package Clock; tests
	// inject a fake one per call).
	clock Clock
	// resolve proves trust (ResolveTrust); open builds the verified
	// connection for that trust.
	resolve func(context.Context, TrustOptions) (Trust, error)
	open    func(planeURL string, t Trust) (waitConn, error)
	// slice is the requested plane wait, attempt one attempt's deadline
	// and pace the least time between two attempt starts.
	slice, attempt, pace time.Duration
}

// newUntilDone is a call on the real clock, trust resolution and client.
func newUntilDone() *untilDone {
	u := &untilDone{clock: realClock{}, resolve: ResolveTrust, slice: UntilDoneSlice, attempt: UntilDoneAttempt, pace: UntilDonePace}
	u.open = u.openClient
	return u
}

// WaitTasksUntilDone waits, with no overall deadline, until the first of
// ids (1 to 16 unique task IDs, in caller order) ends durably, and returns
// that terminal answer; it never returns still_running. It owns trust
// resolution (resolved once, then retained with one verified client until
// it returns), retries every unavailable failure silently and stops at the
// first permanent error or the caller's cancellation (whose error it
// returns unchanged).
func WaitTasksUntilDone(ctx context.Context, options TrustOptions, ids []string) (contract.WaitResponse, error) {
	return newUntilDone().run(ctx, options, ids)
}

// checkTrustOptions is the trust inputs' syntax, checked before any
// network access (a missing trust input is left to ResolveTrust).
func checkTrustOptions(o TrustOptions) error {
	if _, err := parseEndpoint(o.PlaneURL); err != nil {
		return err
	}
	if o.CAFile != "" && o.CAFingerprint != "" {
		return contract.New(contract.CodeInvalidArgument, "give only one of --ca and --ca-fingerprint")
	}
	if o.CAFingerprint != "" && !ValidPin(o.CAFingerprint) {
		return contract.New(contract.CodeInvalidArgument, "invalid --ca-fingerprint: want sha256: followed by 64 lowercase hex digits, as printed by callsheet plane init")
	}
	return nil
}

// sleep waits d on the call's clock. Cancellation interrupts it, and
// after any wake the caller's state decides: a cancelled context wins even
// when the timer fired too.
func (u *untilDone) sleep(ctx context.Context, d time.Duration) error {
	if d > 0 {
		c, stop := u.clock.NewTimer(d)
		select {
		case <-c:
		case <-ctx.Done():
		}
		stop()
	}
	return ctx.Err()
}

// run is WaitTasksUntilDone's loop: one request at a time; nothing of an
// answer is retained after the next request starts.
func (u *untilDone) run(ctx context.Context, o TrustOptions, ids []string) (contract.WaitResponse, error) {
	if err := ctx.Err(); err != nil {
		return contract.WaitResponse{}, err
	}
	if err := contract.ValidateWaitIDs(ids); err != nil {
		return contract.WaitResponse{}, err
	}
	if err := checkTrustOptions(o); err != nil {
		return contract.WaitResponse{}, err
	}
	body, err := encodeBody(contract.TaskWaitRequest{TaskIDs: ids, Wait: u.slice})
	if err != nil {
		return contract.WaitResponse{}, err
	}
	var conn waitConn
	defer func() {
		if conn != nil {
			conn.close()
		}
	}()
	fails := 0
	var lastStart time.Time
	started := false
	// retry classifies a failure: cancellation first (never retried), then
	// only unavailable is retried after the backoff.
	retry := func(err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if contract.CodeOf(err) != contract.CodeUnavailable {
			return err
		}
		d := UntilDoneBackoff(fails)
		fails++
		return u.sleep(ctx, d)
	}
	for {
		if err := ctx.Err(); err != nil {
			return contract.WaitResponse{}, err
		}
		if conn == nil {
			trust, err := u.resolve(ctx, o)
			if err == nil {
				conn, err = u.open(o.PlaneURL, trust)
			}
			if err != nil {
				conn = nil
				if err := retry(err); err != nil {
					return contract.WaitResponse{}, err
				}
				continue
			}
		}
		if started {
			if err := u.sleep(ctx, lastStart.Add(u.pace).Sub(u.clock.Now())); err != nil {
				return contract.WaitResponse{}, err
			}
		}
		lastStart, started = u.clock.Now(), true
		r, err := conn.waitOnce(ctx, ids, body)
		if cerr := ctx.Err(); cerr != nil {
			return contract.WaitResponse{}, cerr
		}
		if err != nil {
			if err := retry(err); err != nil {
				return contract.WaitResponse{}, err
			}
			continue
		}
		if r.Status == contract.WaitTerminal {
			return r, nil
		}
		// A valid still_running answer: renew, retaining none of it.
		fails = 0
	}
}

// openClient is the real connection: one verified client for the
// resolved trust.
func (u *untilDone) openClient(planeURL string, t Trust) (waitConn, error) {
	c, err := New(planeURL, t)
	if err != nil {
		return nil, err
	}
	return &clientWaitConn{c: c, slice: u.slice, attempt: u.attempt, now: u.clock.Now}, nil
}

// clientWaitConn performs single attempts over fresh wait transports of
// one verified client (never the bounded WaitTasks retry loop).
type clientWaitConn struct {
	c              *Client
	slice, attempt time.Duration
	now            func() time.Time
}

// waitOnce is one attempt: a request for the slice bounded by the attempt
// deadline (and the caller's context), the status and the complete answer
// validated against ids. An answer decoded at or after the attempt
// deadline is discarded as unavailable, never published.
func (w *clientWaitConn) waitOnce(ctx context.Context, ids []string, body []byte) (contract.WaitResponse, error) {
	deadline := w.now().Add(w.attempt)
	// The attempt's own deadline is a context of its own, so its expiry
	// is observable whatever stage the request reached (dial, headers or
	// a stalled body, an error body included).
	actx, cancel := context.WithTimeout(ctx, w.attempt)
	defer cancel()
	hc, done := w.c.waitClient(w.attempt)
	status, b, err, _ := w.c.requestVia(actx, hc, w.attempt, http.MethodPost, contract.PathTaskWait, body, contract.MaxWaitResponseBytes)
	done()
	// expired is the attempt's deadline, either way it is observed: the
	// deadline context (its expiry is delivered by a timer, so it may lag)
	// or the attempt's clock past the recorded deadline.
	expired := func() bool { return actx.Err() != nil || !w.now().Before(deadline) }
	if err != nil {
		// The caller's cancellation first, then the attempt's expiry: an
		// error completed at or after the attempt deadline (such as a
		// non-2xx error body cut off by it, or a malformed one that arrived
		// late) is retryable unavailability; one completed before the
		// deadline keeps its own classification.
		if cerr := ctx.Err(); cerr != nil {
			return contract.WaitResponse{}, cerr
		}
		if expired() {
			return contract.WaitResponse{}, ownDeadline("the wait attempt")
		}
		return contract.WaitResponse{}, err
	}
	if expired() {
		return contract.WaitResponse{}, ownDeadline("the wait attempt")
	}
	if status != http.StatusOK {
		return contract.WaitResponse{}, invalidResponse("unexpected status for a wait")
	}
	r, err := contract.ParseWaitResponse(b, ids, w.slice)
	if expired() {
		return contract.WaitResponse{}, ownDeadline("the wait attempt")
	}
	return r, err
}

func (w *clientWaitConn) close() { w.c.Close() }
