package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The background wait helper (design 12a-real-e2e, amendment A2):
// reale2e wait --run ABS --task ID --hop NAME relays a hop's completion
// from the live supervisor over the runtime's Unix socket, so a sandboxed
// coordinator Bash command never needs the plane's loopback address. It
// is one background process looping over short status requests (never a
// long-held connection and never model polling): after each pending
// answer it waits 250ms on its injectable timer and opens a new
// connection. Only pending continues the loop; there is no retry or
// reconnect after any other answer or error. A terminal answer prints
// exactly what callsheet task wait --until-done --json prints on success
// (one encoded WaitResponse line) and exits 0, whatever the worker's own
// exit code; every error exits 1 with a fixed diagnostic and no stdout,
// usage errors 2. A 90m ceiling from invocation is the last-resort bound.

// Wait helper bounds.
const (
	waitPoll    = 250 * time.Millisecond
	waitCeiling = 90 * time.Minute
	// waitRequested is the wait duration the nested response is validated
	// against (as the collector does).
	waitRequested = 40 * time.Second
)

// WaitWorld is the wait helper's capabilities: its cancellation, its
// clock (the poll timer and the ceiling) and one exchange's budget.
type WaitWorld struct {
	Context  context.Context
	Clock    Clock
	Exchange time.Duration
}

// LiveWaitWorld is the live wait world: SIGINT/SIGTERM cancel it, the
// wall clock (monotonic) and the socket's 5s exchange.
func LiveWaitWorld() (WaitWorld, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return WaitWorld{Context: ctx, Clock: realClock{}, Exchange: controlExchange}, stop
}

// waitError is a helper failure with its fixed diagnostic.
type waitError string

func (e waitError) Error() string { return string(e) }

// Fixed wait diagnostics.
const (
	errWaitRunFile    = waitError("cannot read the run file")
	errWaitSocket     = waitError("the control socket path exceeds 90 bytes")
	errWaitReach      = waitError("cannot reach the supervisor")
	errWaitIncomplete = waitError("no complete response from the supervisor")
	errWaitMalformed  = waitError("malformed response from the supervisor")
	errWaitOversized  = waitError("oversized response from the supervisor")
	errWaitTimeout    = waitError("the exchange with the supervisor timed out")
	errWaitResult     = waitError("invalid wait result from the supervisor")
	errWaitCancelled  = waitError("cancelled")
	errWaitCeiling    = waitError("the 90m wait ceiling passed")
	errWaitWrite      = waitError("cannot write the result")
)

// waitHelper runs the wait loop and prints the terminal result.
func waitHelper(ww WaitWorld, stdout, stderr io.Writer, runtime, task, hop string) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "reale2e wait: %v\n", err)
		return 1
	}
	out, err := waitLoop(ww, runtime, task, hop)
	if err != nil {
		return fail(err)
	}
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		return fail(errWaitWrite)
	}
	return 0
}

// waitLoop polls the supervisor until the task is terminal, returning the
// encoded WaitResponse.
func waitLoop(ww WaitWorld, runtime, task, hop string) ([]byte, error) {
	start := ww.Clock.Now()
	runID, err := readRunFile(runtime)
	if err != nil {
		return nil, errWaitRunFile
	}
	sock, err := socketPath(runtime)
	if err != nil {
		return nil, errWaitSocket
	}
	for {
		if ww.Context.Err() != nil {
			return nil, errWaitCancelled
		}
		left := waitCeiling - ww.Clock.Now().Sub(start)
		if left <= 0 {
			return nil, errWaitCeiling
		}
		code, out, err := waitExchange(ww.Context, sock, runID, task, hop, min(ww.Exchange, left))
		switch {
		case err != nil:
			return nil, err
		case code == CodeTerminal:
			return out, nil
		case code != CodePending:
			return nil, waitError("the supervisor refused: " + code)
		}
		t, stop := ww.Clock.NewTimer(waitPoll)
		select {
		case <-t:
		case <-ww.Context.Done():
			stop()
			return nil, errWaitCancelled
		}
	}
}

// waitExchange performs one wait request on a new connection within
// budget: the request, a half-close, one response line and EOF. It
// returns the answer's code and, for terminal, the re-encoded result.
func waitExchange(ctx context.Context, sock, runID, task, hop string, budget time.Duration) (string, []byte, error) {
	deadline := time.Now().Add(budget)
	d := net.Dialer{Deadline: deadline}
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, errWaitCancelled
		}
		return "", nil, errWaitReach
	}
	conn := c.(*net.UnixConn)
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(deadline)
	req, _ := json.Marshal(map[string]string{"run": runID, "op": opWait, "task": task, "hop": hop})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return "", nil, transportError(ctx, err)
	}
	if err := conn.CloseWrite(); err != nil {
		return "", nil, transportError(ctx, err)
	}
	// The whole answer up to EOF, bounded before any growth beyond it.
	data, err := io.ReadAll(io.LimitReader(conn, maxWaitLine+1))
	switch {
	case err != nil:
		return "", nil, transportError(ctx, err)
	case len(data) > maxWaitLine:
		return "", nil, errWaitOversized
	}
	i := bytes.IndexByte(data, '\n')
	switch {
	case i < 0:
		return "", nil, errWaitIncomplete
	case i != len(data)-1:
		return "", nil, errWaitMalformed
	}
	return parseWaitLine(data[:i], task)
}

// transportError classifies a failed exchange.
func transportError(ctx context.Context, err error) error {
	var ne net.Error
	switch {
	case ctx.Err() != nil:
		return errWaitCancelled
	case errors.As(err, &ne) && ne.Timeout():
		return errWaitTimeout
	}
	return errWaitIncomplete
}

// parseWaitLine strictly decodes one wait response line (without its
// newline): exactly pending, terminal with its result, or a refusal.
func parseWaitLine(line []byte, task string) (string, []byte, error) {
	if !utf8.Valid(line) || checkDuplicateKeys(line) != nil {
		return "", nil, errWaitMalformed
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
		return "", nil, errWaitMalformed
	}
	var ok bool
	var code string
	if json.Unmarshal(raw["ok"], &ok) != nil || !bytes.Equal(bytes.TrimSpace(raw["ok"]), []byte(fmt.Sprint(ok))) ||
		json.Unmarshal(raw["code"], &code) != nil || bytes.HasPrefix(bytes.TrimSpace(raw["code"]), []byte("null")) {
		return "", nil, errWaitMalformed
	}
	result, hasResult := raw["result"]
	keys := 2
	if hasResult {
		keys = 3
	}
	switch {
	case len(raw) != keys:
		return "", nil, errWaitMalformed
	case ok && code == CodeTerminal && hasResult:
	case ok && code == CodePending && !hasResult, !ok && !hasResult && slices.Contains(errorCodes, code):
		if len(line)+1 > maxControlLine {
			return "", nil, errWaitOversized
		}
		return code, nil, nil
	default:
		return "", nil, errWaitMalformed
	}
	if len(result)+1 > contract.MaxWaitResponseBytes {
		return "", nil, errWaitOversized
	}
	wr, err := contract.ParseWaitResponse(result, []string{task}, waitRequested)
	if err != nil || wr.Status != contract.WaitTerminal || wr.Task == nil {
		return "", nil, errWaitResult
	}
	out, err := contract.EncodeWaitResponse(wr)
	if err != nil {
		return "", nil, errWaitResult
	}
	return CodeTerminal, out, nil
}
