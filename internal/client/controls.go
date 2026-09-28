package client

import (
	"context"
	"net/http"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The iteration 06b task controls: cancel and bounded waits, typed so the
// CLI and a future MCP server mirror them one to one. A wait uses its own
// transport (never mutating the shared one ordinary operations use),
// bounded by the requested wait plus the ordinary 10 s scale (plus the 6 s
// admission budget for a dispatch). Only a task wait retries, and only an
// interrupted transport, within one absolute deadline; a dispatch is never
// retried.

// waitMargin is the transport margin beyond a requested wait;
// dispatchMargin adds the admission budget for a dispatch with a wait.
const (
	waitMargin     = OperationTimeout
	dispatchMargin = OperationTimeout + 6*time.Second
	// waitBackoffFirst and waitBackoffMax bound the reconnect backoff of
	// an interrupted task wait.
	waitBackoffFirst = 100 * time.Millisecond
	waitBackoffMax   = time.Second
)

// Seams of the bounded wait's retry schedule (tests shorten them).
var (
	waitNow   = time.Now
	waitSleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// waitMarginFor lets tests shorten the transport margin.
	waitMarginFor = func(dispatch bool) time.Duration {
		if dispatch {
			return dispatchMargin
		}
		return waitMargin
	}
)

// waitClient is a fresh verified client whose response header bound is
// header: the shared transport's configuration, never the shared transport.
func (c *Client) waitClient(header time.Duration) (*http.Client, func()) {
	tr := newTransportHeader(c.tr.TLSClientConfig.Clone(), header)
	return newHTTPClient(tr), tr.CloseIdleConnections
}

// checkWait validates a requested wait: 0 through 5 minutes.
func checkWait(d time.Duration) error {
	if d < 0 || d > contract.MaxWait {
		return contract.TaskError(contract.CodeInvalidArgument, "wait", "", "wait must be from 0 to 5m")
	}
	return nil
}

// ownDeadline is the unavailable answer of a call whose own bound passed.
func ownDeadline(what string) error {
	return contract.New(contract.CodeUnavailable, what+" did not complete within its own deadline; the plane may be unreachable (inspect callsheet task show and retry)")
}

// CancelTask requests cancellation of task id: accepted=true (202) once a
// stop intent is durable, false (200) for an already durably terminal
// task. Acceptance is not the outcome: read task wait or task show. A lost
// response may hide a durable intent; a retry is safe (idempotent).
func (c *Client) CancelTask(ctx context.Context, id string) (contract.CancelResponse, error) {
	if !contract.ValidTaskID(id) {
		return contract.CancelResponse{}, invalidTaskID()
	}
	status, b, err := c.request(ctx, http.MethodPost, contract.PathTasks+"/"+id+"/cancel", []byte("{}"), contract.MaxWaitResponseBytes)
	if err != nil {
		return contract.CancelResponse{}, err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return contract.CancelResponse{}, invalidResponse("unexpected status for a cancel")
	}
	return contract.ParseCancelResponse(b, status, id)
}

// WaitTasks waits at most wait (0: an immediate snapshot) for the first
// of ids (1 to 16 unique task IDs) to end durably. It answers terminal
// with the winner's view, or still_running with one compact row per id in
// caller order. An interrupted transport (a plane restart) is retried
// with the same ids and trust within the original deadline, each retry
// asking only for the wait that remains; once the wait is spent a
// reachable plane gets one immediate snapshot and an unreachable one is
// unavailable. Validation, trust, not_found and the caller's cancellation
// are never retried, and a response decoded after the caller's own
// deadline is not published.
func (c *Client) WaitTasks(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error) {
	if err := contract.ValidateWaitIDs(ids); err != nil {
		return contract.WaitResponse{}, err
	}
	if err := checkWait(wait); err != nil {
		return contract.WaitResponse{}, err
	}
	margin := waitMarginFor(false)
	start := waitNow()
	waitEnd := start.Add(wait)
	outer := waitEnd.Add(margin)
	backoff := waitBackoffFirst
	snapshot := false
	attempt := 0
	for {
		now := waitNow()
		remaining := max(0, waitEnd.Sub(now))
		if remaining == 0 {
			snapshot = true
		}
		budget := min(outer.Sub(now), remaining+margin)
		if budget <= 0 {
			return contract.WaitResponse{}, ownDeadline("the wait")
		}
		// The first request asks for the whole wait; a retry only for what
		// remains of it (a plane restart never renews it).
		ask := wait
		if attempt > 0 {
			ask = remaining.Truncate(time.Millisecond)
		}
		attempt++
		body, err := encodeBody(contract.TaskWaitRequest{TaskIDs: ids, Wait: ask})
		if err != nil {
			return contract.WaitResponse{}, err
		}
		hc, done := c.waitClient(budget)
		status, b, err, transport := c.requestVia(ctx, hc, budget, http.MethodPost, contract.PathTaskWait, body, contract.MaxWaitResponseBytes)
		done()
		if cerr := ctx.Err(); cerr != nil {
			return contract.WaitResponse{}, cerr
		}
		if err == nil {
			if !waitNow().Before(outer) {
				return contract.WaitResponse{}, ownDeadline("the wait")
			}
			if status != http.StatusOK {
				return contract.WaitResponse{}, invalidResponse("unexpected status for a wait")
			}
			return contract.ParseWaitResponse(b, ids, wait)
		}
		if !transport || snapshot {
			return contract.WaitResponse{}, err
		}
		sleep := min(backoff, max(0, outer.Sub(waitNow())))
		if serr := waitSleep(ctx, sleep); serr != nil {
			return contract.WaitResponse{}, serr
		}
		backoff = min(2*backoff, waitBackoffMax)
		if !waitNow().Before(outer) {
			return contract.WaitResponse{}, err
		}
	}
}

// DispatchWithWait submits req with a transport wait (0 through 5 min):
// admission happens once and is never retried. It returns the admitted
// task's ID and the wait union (terminal with the view, or still_running
// with its one compact row). A lost response may hide an admitted task:
// the error says to inspect task ls; a known task ID can be waited on
// separately with WaitTasks.
func (c *Client) DispatchWithWait(ctx context.Context, req contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error) {
	if err := req.Validate(); err != nil {
		return contract.DispatchResponse{}, err
	}
	if err := checkWait(wait); err != nil {
		return contract.DispatchResponse{}, err
	}
	b, err := contract.Encode(req)
	if err != nil {
		return contract.DispatchResponse{}, contract.Wrap(contract.CodeInternal, "cannot encode the request", err)
	}
	// The transport envelope: the request's members plus wait.
	ws, _ := contract.Encode(wait.String())
	body := append(append(b[:len(b)-1:len(b)-1], []byte(`,"wait":`)...), ws...)
	body = append(body, '}')
	budget := wait + waitMarginFor(true)
	outer := waitNow().Add(budget)
	hc, done := c.waitClient(budget)
	defer done()
	status, rb, err, transport := c.requestVia(ctx, hc, budget, http.MethodPost, contract.PathTasks, body, contract.MaxWaitResponseBytes)
	if cerr := ctx.Err(); cerr != nil {
		return contract.DispatchResponse{}, cerr
	}
	if err != nil {
		if transport {
			return contract.DispatchResponse{}, contract.Wrap(contract.CodeUnavailable, "the dispatch's response was lost; a task may have been admitted: inspect callsheet task ls before dispatching again", err)
		}
		return contract.DispatchResponse{}, err
	}
	if !waitNow().Before(outer) {
		return contract.DispatchResponse{}, ownDeadline("the dispatch's wait")
	}
	if status != http.StatusAccepted {
		return contract.DispatchResponse{}, invalidResponse("unexpected status for a dispatch")
	}
	id, wr, err := contract.ParseDispatchWaitResponse(rb, wait)
	if err != nil {
		return contract.DispatchResponse{}, err
	}
	if wr.Task != nil && (wr.Task.Request.Goal != req.Goal || wr.Task.Request.Target != req.Target) {
		return contract.DispatchResponse{}, invalidResponse("it describes another request")
	}
	return contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: id, WaitResult: &wr}, nil
}
