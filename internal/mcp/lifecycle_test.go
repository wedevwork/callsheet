package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Regressions of code review round 1 (07a): call contexts released on
// completion (C1), the delivery deadline judged by recorded instants (C2)
// and readiness only after the initialize answer was emitted (W1).

// holds blocks chosen hook events until released; every event is still
// recorded by the harness first, so await observes the blocked one.
type holds struct {
	mu sync.Mutex
	m  map[hookEvent]chan struct{}
}

func (hs *holds) block(stage, id string) {
	hs.mu.Lock()
	c := hs.m[hookEvent{stage, id}]
	hs.mu.Unlock()
	if c != nil {
		<-c
	}
}

// hold makes the next occurrences of stage/id wait for the returned
// release.
func (hs *holds) hold(stage, id string) (release func()) {
	c := make(chan struct{})
	hs.mu.Lock()
	if hs.m == nil {
		hs.m = map[hookEvent]chan struct{}{}
	}
	hs.m[hookEvent{stage, id}] = c
	hs.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(c) }) }
}

// liveChildren is the number of child contexts registered with the
// session's cancel context (a *context.cancelCtx: the probe reads its
// children map under its own mutex and fails loudly if the standard
// library's layout changes).
func liveChildren(t *testing.T, ctx context.Context) int {
	t.Helper()
	v := reflect.ValueOf(ctx)
	if v.Kind() != reflect.Pointer {
		t.Fatalf("session context %T is not a pointer", ctx)
	}
	e := v.Elem()
	mu, children := e.FieldByName("mu"), e.FieldByName("children")
	if !mu.IsValid() || !children.IsValid() || children.Kind() != reflect.Map || mu.Type() != reflect.TypeFor[sync.Mutex]() {
		t.Fatalf("context internals changed (%T): update the probe", ctx)
	}
	m := (*sync.Mutex)(unsafe.Pointer(mu.UnsafeAddr()))
	m.Lock()
	defer m.Unlock()
	return children.Len()
}

// C1: every completed call releases its contexts, on success, on a tool
// error before the client exists, on a client error, on a trust failure
// and for a waiting call: nothing stays registered with the session's
// context, and every context a call handed out is cancelled.
func TestCallContextsReleased(t *testing.T) {
	var mu sync.Mutex
	var ctxs []context.Context
	fail := false
	fake := &fakeClient{}
	happy(fake)
	fake.showNode = func(context.Context, string) (contract.Node, error) {
		return contract.Node{}, contract.New(contract.CodeNotFound, "no such node")
	}
	h := start(t, withFactory(func(ctx context.Context) (Client, error) {
		mu.Lock()
		defer mu.Unlock()
		ctxs = append(ctxs, ctx)
		if fail {
			return nil, contract.New(contract.CodeTrustFailed, "connection not trusted")
		}
		return fake, nil
	}))
	h.ready()
	const rounds = 20
	for i := range rounds {
		for _, c := range []struct{ tool, args string }{
			{toolNodeLs, ""},
			{toolNodeShow, `{"id":"bad"}`},
			{toolNodeShow, `{"id":"` + nodeA + `"}`},
			{toolTaskWait, `{"task_ids":["` + taskA + `"],"wait":"0"}`},
		} {
			h.ask(c.tool, c.args)
			h.await(StageReleased, "n"+itoa(h.nextID))
		}
		mu.Lock()
		fail = i%2 == 0
		mu.Unlock()
		h.ask(toolNodeLs, "")
		h.await(StageReleased, "n"+itoa(h.nextID))
		mu.Lock()
		fail = false
		mu.Unlock()
	}
	if n := liveChildren(t, h.s.ctx); n != 0 {
		t.Fatalf("%d completed calls are still registered with the session context", n)
	}
	mu.Lock()
	seen := append([]context.Context(nil), ctxs...)
	mu.Unlock()
	if len(seen) != rounds*4 {
		t.Fatalf("%d factory contexts", len(seen))
	}
	for i, ctx := range seen {
		if ctx.Err() == nil || !errors.Is(context.Cause(ctx), errCallDone) {
			t.Fatalf("call context %d not released: %v %v", i, ctx.Err(), context.Cause(ctx))
		}
	}
	// A cancelled call and the session's end keep their own causes.
	g := newGate()
	var held context.Context
	fake.listRoles = func(ctx context.Context) ([]contract.RoleView, error) {
		held = ctx
		return nil, g.wait(ctx)
	}
	h.nextID++
	h.call(h.nextID, toolRoleLs, "")
	g.awaitEntered(t)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + itoa(h.nextID) + `}}`)
	h.await(StageReleased, "n"+itoa(h.nextID))
	if !errors.Is(context.Cause(held), errCancelled) || liveChildren(t, h.s.ctx) != 0 {
		t.Fatalf("cancelled call: %v, %d live", context.Cause(held), liveChildren(t, h.s.ctx))
	}
}

// deliveryRig is a session whose single task_wait answer (larger than a
// 64 KiB pipe) is being written when the test takes over the clock: the
// call's timers are armed at the epoch (D = epoch+10s) and the writer is
// blocked in the middle of the frame.
type deliveryRig struct {
	h  *harness
	w  *ctlWriter
	hs *holds
	d  time.Time
}

func newDeliveryRig(t *testing.T) *deliveryRig {
	t.Helper()
	r := &deliveryRig{w: newCtlWriter(64 << 10), hs: &holds{}}
	r.h = start(t, withOutput(r.w, r.w.Close), withHook(r.hs.block))
	happy(r.h.fake)
	v := taskView(taskA)
	v.State = contract.TaskSucceeded
	// ~100 KB of bytes that need no escaping: more than the 64 KiB pipe
	// plus the rig's drains, cheap to encode under -race and coverage.
	v.LogTail = strings.Repeat("x", 96<<10)
	r.h.fake.waitTasks = func(context.Context, []string, time.Duration) (contract.WaitResponse, error) {
		return contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 1, Winner: taskA, Task: &v}, nil
	}
	readyOn(t, r.h, r.w)
	b := NewBudget(DefaultBudget)
	r.d = epoch.Add(b.B)
	r.h.call(1, toolTaskWait, `{"task_ids":["`+taskA+`"]}`)
	awaitArmed(t, r.h, b, epoch)
	r.w.awaitFull(t)
	// D-R passes first (the answer is already queued: nothing changes),
	// then the clock stops just before D. Set fires nothing, so before
	// each move the writer's progress timer is re-armed from the new
	// instant by one positive write.
	r.setAndProgress(t, r.d.Add(-b.R-time.Nanosecond))
	r.h.clock.Advance(time.Nanosecond)
	r.h.await(StageExpired, "n1")
	r.setAndProgress(t, r.d.Add(-time.Nanosecond))
	return r
}

// setAndProgress sets the clock to at (firing nothing), lets the writer
// make progress (re-arming its timer from at) and waits until it blocks
// again on the full pipe.
func (r *deliveryRig) setAndProgress(t *testing.T, at time.Time) {
	t.Helper()
	r.h.clock.Set(at)
	r.w.drain(4 << 10)
	if err := r.h.clock.AwaitWaiter(testWait, armedAt(at.Add(WriteIdle), WriteIdle)); err != nil {
		t.Fatal(err)
	}
	r.w.awaitFull(t)
}

// complete drains until the frame's last byte was written.
func (r *deliveryRig) complete(t *testing.T) {
	t.Helper()
	if l := r.w.line(t); !bytes.Contains(l, []byte(`"id":1,`)) {
		t.Fatalf("delivered %.80s", l)
	}
}

// running requires the session to be alive: a ping is answered.
func (r *deliveryRig) running(t *testing.T) {
	t.Helper()
	select {
	case code := <-r.h.code:
		t.Fatalf("the session ended (%d): %q", code, r.h.stderr.String())
	default:
	}
	r.h.send(`{"jsonrpc":"2.0","id":"alive","method":"ping"}`)
	if l := r.w.line(t); !bytes.Contains(l, []byte(`"id":"alive"`)) {
		t.Fatalf("after delivery %s", l)
	}
}

func (r *deliveryRig) late(t *testing.T) {
	t.Helper()
	if code := r.h.exit(); code != ExitOutput || !strings.Contains(r.h.stderr.String(), "not delivered within its 10s call budget") ||
		!strings.Contains(r.h.stderr.String(), LateWriteNote) {
		t.Fatalf("exit %d: %q", code, r.h.stderr.String())
	}
}

// C2: a completion after D is late even when D's timer goroutine runs only
// after the writer finished (and after the answer was marked answered).
func TestDeliveryLateWhileTimerDelayed(t *testing.T) {
	for _, c := range []struct {
		name  string
		after time.Duration
	}{{"after-d", 5 * time.Millisecond}, {"at-d", 0}} {
		t.Run(c.name, func(t *testing.T) {
			r := newDeliveryRig(t)
			release := r.hs.hold(StageDeadline, "n1")
			defer release()
			// D fires; its goroutine is held before it judges. The frame
			// completes at D (both events ready at the same instant) or
			// after it.
			r.h.clock.Advance(time.Nanosecond + c.after)
			r.h.await(StageDeadline, "n1")
			r.complete(t)
			// The writer's own outcome, before D's goroutine may judge: it
			// judged the completion late.
			r.h.await(StageLate, "n1")
			release()
			r.late(t)
		})
	}
}

// C2 (review round 2): the reviewer's probe. The final Write returns at
// D-1ns; the call's mutex is held by someone else while the clock reaches
// D and D's timer fires; only then is the mutex released. The completion
// instant is the one sampled when the Write returned: on time.
func TestDeliveryOnTimeMutexContended(t *testing.T) {
	r := newDeliveryRig(t)
	r.h.s.mu.Lock()
	c := r.h.s.calls["n1"]
	r.h.s.mu.Unlock()
	if c == nil {
		t.Fatal("the call is not active")
	}
	c.mu.Lock()
	r.complete(t)
	r.h.await(StageSampled, "n1") // the final Write returned at D-1ns
	r.h.clock.Advance(time.Nanosecond)
	r.h.await(StageDeadline, "n1")
	c.mu.Unlock()
	r.h.await(StageTimed, "n1")
	r.running(t)
}

// C2 (review round 2): D's timer fires while the writer is between its
// final Write's return (with the sampled instant D-1ns) and publishing the
// record: D defers to the writer, which judges on time.
func TestDeliveryOnTimeDeadlineDuringRecord(t *testing.T) {
	r := newDeliveryRig(t)
	release := r.hs.hold(StageSampled, "n1")
	defer release()
	r.complete(t)
	r.h.await(StageSampled, "n1")
	r.h.clock.Advance(time.Nanosecond) // D fires and is judged now
	r.h.await(StageTimed, "n1")
	release()
	r.h.await(StageWritten, "n1")
	r.running(t)
}

// C2 (review round 2): D fires while a Write that can finish the answer
// is in progress. D defers to that Write: if it then completes (at or
// after D) the writer judges it late; if it never returns, the writer's
// one-second progress timer ends the session. Either way exit 5, and D
// never judges a Write it cannot see the end of.
func TestDeliveryFinalWriteInProgressAtD(t *testing.T) {
	toFinal := func(t *testing.T, r *deliveryRig) {
		t.Helper()
		// At D-1ns, drain until the writer blocks inside a Write of the
		// answer's last chunk (every positive write re-arms its progress
		// timer from D-1ns).
		for !r.h.ev.peek(hookEvent{StageFinal, "n1"}) {
			r.w.drain(4 << 10)
			if r.w.awaitFullOrLine(t) {
				t.Fatal("the answer completed before its last chunk blocked")
			}
		}
		r.w.awaitFull(t)
	}
	t.Run("completes-after-d", func(t *testing.T) {
		r := newDeliveryRig(t)
		toFinal(t, r)
		r.h.clock.Advance(time.Nanosecond) // D: a final Write is in progress
		r.h.await(StageTimed, "n1")
		select {
		case code := <-r.h.code:
			t.Fatalf("D ended the session (%d) while the final Write was in progress", code)
		default:
		}
		r.complete(t)
		r.h.await(StageLate, "n1")
		r.late(t)
	})
	t.Run("short-return-after-d", func(t *testing.T) {
		r := newDeliveryRig(t)
		toFinal(t, r)
		r.h.clock.Advance(time.Nanosecond + 5*time.Millisecond) // D fires and defers to the blocked final Write
		r.h.await(StageTimed, "n1")
		r.w.mu.Lock()
		calls := r.w.calls
		r.w.mu.Unlock()
		// One byte of room: the blocked Write returns n < len(chunk) with
		// no error, after D.
		r.w.drain(1)
		if code := r.h.exit(); code != ExitOutput || !strings.Contains(r.h.stderr.String(), LateShortNote) ||
			strings.Contains(r.h.stderr.String(), "made no progress") {
			t.Fatalf("exit %d: %q", code, r.h.stderr.String())
		}
		r.w.mu.Lock()
		defer r.w.mu.Unlock()
		if r.w.calls != calls {
			t.Fatalf("%d further Write calls after the late short return", r.w.calls-calls)
		}
		if bytes.HasSuffix(r.w.buf, []byte("\n")) {
			t.Fatal("the frame completed: the return was not short")
		}
	})
	t.Run("never-returns", func(t *testing.T) {
		r := newDeliveryRig(t)
		toFinal(t, r)
		r.h.clock.Advance(time.Nanosecond)
		r.h.await(StageTimed, "n1")
		r.h.clock.Advance(WriteIdle - time.Nanosecond) // one second after the last progress
		if code := r.h.exit(); code != ExitOutput || !strings.Contains(r.h.stderr.String(), "made no progress for 1s") {
			t.Fatalf("exit %d: %q", code, r.h.stderr.String())
		}
	})
}

// C2: a completion before D stays on time even when its bookkeeping is
// delayed past D and D's goroutine judges first.
func TestDeliveryOnTimeBookkeepingDelayed(t *testing.T) {
	r := newDeliveryRig(t)
	release := r.hs.hold(StageFlushed, "n1")
	defer release()
	r.complete(t) // the final write returns at D-1ns
	r.h.await(StageFlushed, "n1")
	r.h.clock.Advance(time.Nanosecond) // D: judged while the writer is held
	r.h.await(StageTimed, "n1")
	release()
	r.h.await(StageWritten, "n1")
	r.running(t)
}

// C2: completion before D and D's firing ready together (the answer
// already marked answered when D's goroutine runs): never late.
func TestDeliveryOnTimeBothReady(t *testing.T) {
	r := newDeliveryRig(t)
	release := r.hs.hold(StageDeadline, "n1")
	defer release()
	r.complete(t)
	r.h.await(StageWritten, "n1")
	r.h.clock.Advance(time.Nanosecond)
	r.h.await(StageTimed, "n1") // either select path: responded or the held D
	release()
	r.running(t)
}

// W1: an early notifications/initialized does not make the session ready
// while the initialize answer is still blocked in the writer; once the
// answer is emitted the session becomes ready and serves the tools.
func TestReadyAfterInitializeEmitted(t *testing.T) {
	w := newCtlWriter(16)
	h := start(t, withOutput(w, w.Close))
	h.send(initLine)
	w.awaitFull(t)
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	ready := func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return h.s.state == stateReady
	}
	// Either the reader waits for the emission (the hook) or it made the
	// session ready at once (the defect).
	deadline := time.Now().Add(testWait)
	for !h.ev.peek(hookEvent{StageInitWait, ""}) {
		if ready() {
			t.Fatal("ready while the initialize answer is blocked in the writer")
		}
		if time.Now().After(deadline) {
			t.Fatal("the notification was never handled")
		}
		time.Sleep(time.Millisecond)
	}
	if ready() {
		t.Fatal("ready while the initialize answer is blocked in the writer")
	}
	// The reader waits too (an in-memory pipe has no buffer): send async.
	go io.WriteString(h.inW, `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`+"\n")
	if l := w.line(t); !bytes.Contains(l, []byte(`"id":"init"`)) {
		t.Fatalf("first line %.80s", l)
	}
	w.mu.Lock()
	w.cap = 1 << 20 // the large discovery answer need not crawl through 16 bytes
	w.cond.Broadcast()
	w.mu.Unlock()
	if l := w.line(t); !bytes.Contains(l, []byte(`"id":"list","result":{"tools":[`)) {
		t.Fatalf("tools/list after emission %.120s", l)
	}
	if !ready() {
		t.Fatal("not ready after the answer was emitted")
	}
}
