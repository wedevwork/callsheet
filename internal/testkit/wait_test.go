package testkit

import (
	"sync/atomic"
	"testing"
	"time"
)

// simultaneous is how often each both-ready case repeats: select chooses
// uniformly among ready cases, so a wait deciding by that choice fails
// about half of the repetitions (2^-200 to escape).
const simultaneous = 200

// TestWaitRecheck (iteration 10b, review r1 C5): with the success and the
// timeout both ready, no wait fails; at a timeout with no success every
// wait fails; a negative wait fails when its event and its window's end
// are both ready, and passes when only the window ended.
func TestWaitRecheck(t *testing.T) {
	for i := 0; i < simultaneous; i++ {
		c := make(chan int, 1)
		c <- i
		if Fails(func(tb testing.TB) {
			if got := WithinBy(tb, c, Fired(), "ready value"); got != i {
				tb.Fatalf("got %d", got)
			}
		}) {
			t.Fatal("WithinBy failed with its value and its timeout both ready")
		}
		// The predicate turns true after its first check: the wake and the
		// timeout are then both ready.
		var calls atomic.Int32
		wake := make(chan struct{}, 1)
		wake <- struct{}{}
		if Fails(func(tb testing.TB) {
			AwaitCondBy(tb, func() bool { return calls.Add(1) > 1 }, wake, Fired(), "predicate")
		}) {
			t.Fatal("AwaitCondBy failed with its predicate and its timeout both ready")
		}
		s := make(chan string, 2)
		s <- "other"
		s <- "want"
		if Fails(func(tb testing.TB) {
			if AwaitValueBy(tb, s, func(v string) bool { return v == "want" }, Fired(), "stage") != "want" {
				tb.Fatal("wrong value")
			}
		}) {
			t.Fatal("AwaitValueBy failed with its value and its timeout both ready")
		}
		ev := make(chan struct{}, 1)
		ev <- struct{}{}
		if !Fails(func(tb testing.TB) { AbsentFor(tb, ev, Fired(), "event") }) {
			t.Fatal("AbsentFor passed with its event and its window's end both ready")
		}
	}
	// A real timeout with no success fails; a window with no event passes.
	never := make(chan int)
	if !Fails(func(tb testing.TB) { WithinBy(tb, never, Fired(), "nothing") }) {
		t.Fatal("WithinBy passed without a value")
	}
	if !Fails(func(tb testing.TB) {
		AwaitCondBy[struct{}](tb, func() bool { return false }, nil, Fired(), "never")
	}) {
		t.Fatal("AwaitCondBy passed with a false predicate")
	}
	if !Fails(func(tb testing.TB) {
		AwaitValueBy(tb, make(chan string, 1), func(string) bool { return true }, Fired(), "nothing")
	}) {
		t.Fatal("AwaitValueBy passed without a value")
	}
	if Fails(func(tb testing.TB) { AbsentFor(tb, make(chan struct{}), time.After(time.Millisecond), "event") }) {
		t.Fatal("AbsentFor failed without an event")
	}
}
