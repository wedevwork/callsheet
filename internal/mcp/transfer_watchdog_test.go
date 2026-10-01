package mcp

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

type holdWriter struct {
	w    io.Writer
	hold chan struct{}
}

func (h holdWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	if bytes.Contains(p, []byte(`"id":3`)) {
		<-h.hold
	}
	return n, err
}

// UT-8 (code review r1 C8), forced ordering: the third call's answer is
// delivered but its Write has not returned, so the output writer's
// watchdog (WriteIdle) is still armed. The cancelled transfers must have
// armed no timer of their own; once the frame is written the writer's
// watchdog is disarmed too.
func TestTransferCancellationWatchdogOrdering(t *testing.T) {
	pr, pw := io.Pipe()
	hold := make(chan struct{})
	held := false
	defer func() {
		if !held {
			close(hold)
		}
	}()
	h := start(t, withOutput(holdWriter{pw, hold}, pw.Close))
	lines := bufio.NewScanner(pr)
	next := func() string {
		if !lines.Scan() {
			t.Fatal("no line")
		}
		return lines.Text()
	}
	h.send(initLine)
	next()
	h.await(StageWritten, "sinit")
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.fake.pull = func(ctx context.Context, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
		<-ctx.Done()
		return contract.WorkspacePullResult{}, ctx.Err()
	}
	h.call(1, toolWsPull, `{"name":"alpha","ref":"a"}`)
	h.call(2, toolWsPull, `{"name":"alpha","ref":"b"}`)
	h.await(StageAdmitted, "n1")
	h.await(StageAdmitted, "n2")
	h.call(3, toolWsPull, `{"name":"alpha","ref":"c"}`)
	if l := next(); !bytes.Contains([]byte(l), []byte(`"id":3`)) {
		t.Fatalf("third answer %s", l)
	}
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	h.await(StageReleased, "n1")
	h.await(StageReleased, "n2")
	for _, w := range h.clock.Waiters() {
		if w.Ticker || w.Duration != WriteIdle {
			t.Fatalf("a timer other than the output writer's watchdog is armed: %v", h.clock.Waiters())
		}
	}
	held = true
	close(hold)
	h.await(StageWritten, "n3")
	if w := h.clock.Waiters(); len(w) != 0 {
		t.Fatalf("timers armed after the frame was written: %v", w)
	}
	if h.ev.peek(hookEvent{StageArmed, "n1"}) || h.ev.peek(hookEvent{StageArmed, "n2"}) {
		t.Fatal("a transfer armed a waiting-call budget")
	}
}
