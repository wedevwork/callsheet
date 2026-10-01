package mcp

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// heldWriter copies every frame to an OS pipe and, on the first frame of
// the tool answer with id 1, holds Write after the copy: the client can
// already read that answer while the session's writer is still inside
// Out.Write (the window a slow or descheduled writer leaves open).
type heldWriter struct {
	w    *os.File
	held chan struct{}
	cont chan struct{}
	once sync.Once
}

func (p *heldWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if err == nil && strings.Contains(string(b), `"id":1,`) {
		p.once.Do(func() {
			close(p.held)
			<-p.cont
		})
	}
	return n, err
}

// TestAnswerReadableWhileSlotHeldRefusesThirdCall (code review of PR #13
// run 36875521594, fix.md): a finished call's admission slot is free once
// the writer has taken its answer, before any byte is written. While that
// answer is readable but its Write has not returned, the call no longer
// counts as active, and one in-flight call plus a new one are both
// admitted (two concurrent calls, the 07a cap).
func TestAnswerReadableWhileSlotHeldRefusesThirdCall(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	w := &heldWriter{w: pw, held: make(chan struct{}), cont: make(chan struct{})}
	var release sync.Once
	finish := func() { release.Do(func() { close(w.cont) }) }
	admitted3 := make(chan struct{})
	var once3 sync.Once
	h := start(t, withOutput(w, pw.Close), withHook(func(stage, id string) {
		if stage == StageAdmitted && id == "n3" {
			once3.Do(func() { close(admitted3) })
		}
	}))
	t.Cleanup(finish)
	entered := make(chan struct{}, 2)
	h.fake.listNodes = func(ctx context.Context) ([]contract.Node, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	br := bufio.NewReader(pr)
	readLine := func() string {
		t.Helper()
		pr.SetReadDeadline(time.Now().Add(testWait))
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stdout: %v", err)
		}
		return line
	}
	h.send(initLine)
	if l := readLine(); !strings.Contains(l, `"id":"init"`) {
		t.Fatalf("initialize %s", l)
	}
	h.await(StageWritten, "sinit")
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// A fast tool error; its answer is readable while its Write is held.
	h.call(1, toolRoleAdd, `{}`)
	select {
	case <-w.held:
	case <-time.After(testWait):
		t.Fatal("the writer did not reach the held Write")
	}
	if l := readLine(); !strings.Contains(l, `"id":1,`) || !strings.Contains(l, `"isError":true`) {
		t.Fatalf("answer %s", l)
	}
	h.s.mu.Lock()
	active := h.s.active
	h.s.mu.Unlock()
	if active != 0 {
		t.Errorf("active = %d while the readable answer's Write is still in progress, want 0", active)
	}
	// One in-flight call and one more: both admitted.
	h.call(2, toolNodeLs, "")
	select {
	case <-entered:
	case <-time.After(testWait):
		t.Fatal("the second call was not admitted")
	}
	h.call(3, toolNodeLs, "")
	select {
	case <-admitted3:
	case <-time.After(testWait):
		h.s.wmu.Lock()
		var queued []string
		for _, f := range h.s.queue {
			queued = append(queued, string(f.data))
		}
		h.s.wmu.Unlock()
		h.s.mu.Lock()
		active = h.s.active
		h.s.mu.Unlock()
		t.Fatalf("the third call was not admitted (active %d); queued %q", active, queued)
	}
	h.s.mu.Lock()
	active = h.s.active
	h.s.mu.Unlock()
	if active != 2 {
		t.Fatalf("active = %d, want 2", active)
	}
	finish()
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`)
	h.await(StageReleased, "n2")
	h.await(StageReleased, "n3")
}
