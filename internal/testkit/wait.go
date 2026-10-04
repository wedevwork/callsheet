package testkit

import (
	"runtime"
	"testing"
	"time"
)

// Bounded test waits (iteration 10b, review r1 C5). A test's timeout is a
// failure bound, never a verdict between two ready events: every wait
// below rechecks its success channel or predicate when its timeout fires,
// so a success and the timeout both ready never fail a test, and a
// negative wait rechecks its forbidden channel when its window ends, so an
// event is never hidden by a ready timer. Callers pass the timeout channel
// (time.After(bound) in tests; an already fired one in the helpers' own
// simultaneous-readiness tests).

// WithinBy receives the next value of c unless timeout fires first; at the
// timeout c is rechecked before t fails.
func WithinBy[T any](t testing.TB, c <-chan T, timeout <-chan time.Time, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-timeout:
		select {
		case v := <-c:
			return v
		default:
		}
		t.Fatalf("%s did not happen within the test bound", what)
	}
	var zero T
	return zero
}

// AwaitCondBy waits until cond holds: it is checked first and after each
// wake; at the timeout it is rechecked before t fails. cond must be
// synchronized by the caller.
func AwaitCondBy[W any](t testing.TB, cond func() bool, wake <-chan W, timeout <-chan time.Time, what string) {
	t.Helper()
	for !cond() {
		select {
		case <-wake:
		case <-timeout:
			if cond() {
				return
			}
			t.Fatalf("%s never held within the test bound", what)
		}
	}
}

// AwaitValueBy consumes values of c until match accepts one or timeout
// fires; at the timeout the values already sent are drained (rechecked)
// before t fails.
func AwaitValueBy[T any](t testing.TB, c <-chan T, match func(T) bool, timeout <-chan time.Time, what string) T {
	t.Helper()
	for {
		select {
		case v := <-c:
			if match(v) {
				return v
			}
		case <-timeout:
			for {
				select {
				case v := <-c:
					if match(v) {
						return v
					}
				default:
					t.Fatalf("%s did not happen within the test bound", what)
					var zero T
					return zero
				}
			}
		}
	}
}

// AbsentFor requires that c delivers nothing while window runs; the
// window's end rechecks c, so an event and the window's end both ready
// still fail t.
func AbsentFor[T any](t testing.TB, c <-chan T, window <-chan time.Time, what string) {
	t.Helper()
	select {
	case <-c:
		t.Fatal(what)
	case <-window:
		select {
		case <-c:
			t.Fatal(what)
		default:
		}
	}
}

// Fired returns a timeout channel that is already ready (simultaneous
// readiness tests).
func Fired() <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

// FatalTB is a testing.TB recording whether a helper failed it: Fatal and
// Fatalf record and end the calling goroutine (run the helper through
// Fails). Every other method is the embedded TB's (nil: unused).
type FatalTB struct {
	testing.TB
	failed bool
}

// Helper is a no-op.
func (r *FatalTB) Helper() {}

// Fatal records the failure and ends the goroutine.
func (r *FatalTB) Fatal(...any) { r.failed = true; runtime.Goexit() }

// Fatalf records the failure and ends the goroutine.
func (r *FatalTB) Fatalf(string, ...any) { r.failed = true; runtime.Goexit() }

// Fails runs f with a FatalTB on its own goroutine, joins it and reports
// whether f failed it.
func Fails(f func(t testing.TB)) bool {
	r := &FatalTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(r)
	}()
	<-done
	return r.failed
}
