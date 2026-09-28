package testkit

import (
	"errors"
	"net"
	"testing"
)

// RefusingAddr returns a loopback address at which every connection fails
// at once, before a byte is exchanged, for the rest of the test, and which
// no other listener (a parallel test's server, say) can take meanwhile.
//
// The address is a listener's, held until cleanup, that aborts every
// connection it accepts: SO_LINGER with a zero timeout makes the close
// abortive, so the kernel answers with a reset (RST) at once and the
// client's first read or write fails with "connection reset by peer". The
// same construction behaves the same on Linux and macOS:
//   - Held: a listening socket bound to the address excludes every other
//     bind of it. Linux documents this for SO_REUSEADDR binds, the most
//     permissive a listener makes (socket(7): such a socket may bind
//     "except when there is an active listening socket bound to the
//     address"); XNU refuses a duplicate binding unless both sockets set
//     SO_REUSEPORT, which net.Listen never sets.
//   - Immediate: a zero linger turns close into an abort that sends a
//     reset, in Linux (tcp_close disconnects with an active reset) and in
//     XNU (tcp_disconnect calls tcp_drop, which sends RST, for SO_LINGER
//     with a zero linger).
//
// It replaces a socket that was bound but never listened: Linux resets a
// connection to it, but XNU finds its protocol control block in state
// CLOSED and drops the SYN without a reset (tcp_input), so on macOS every
// dial waited for its timeout. The released-ephemeral-port pattern before
// that let another listener take the port; neither is used.
func RefusingAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("refusing address: listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				// Closed at cleanup. Any other failure would leave later
				// connections queued unanswered, so it fails the test.
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("refusing address: accept: %v", err)
				}
				return
			}
			if err := c.(*net.TCPConn).SetLinger(0); err != nil {
				t.Errorf("refusing address: linger: %v", err)
			}
			c.Close()
		}
	}()
	// The listener is closed and its goroutine gone before the test ends.
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	return ln.Addr().String()
}
