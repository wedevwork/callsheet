//go:build linux || darwin

package testkit

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"
)

// refuseBound is the longest a refused connection attempt may take. A
// refusal is a local reset, answered in well under a millisecond; an
// attempt that reaches this bound is waiting for a timeout (the macOS
// failure of the bound-but-not-listening socket: its SYN was dropped).
const refuseBound = time.Second

// TestRefusingAddr: the unreachable-plane fixture fails every connection
// at once, before a byte is exchanged, for the test's whole lifetime, and
// no other listener (a parallel test's TLS server, say) can take its
// address meanwhile.
func TestRefusingAddr(t *testing.T) {
	addr := RefusingAddr(t)
	// net.Listen sets SO_REUSEADDR, the most permissive bind a listener
	// makes: even it cannot take the address.
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		t.Fatalf("another listener took the reserved address %s", addr)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("listen on %s: %v, want address in use", addr, err)
	}
	for i := 0; i < 3; i++ {
		start := time.Now()
		err := connectOnce(addr)
		if took := time.Since(start); took >= refuseBound {
			t.Fatalf("attempt %d on %s took %v (%v): a timeout, not a refusal", i, addr, took, err)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("attempt %d on %s: %v, want the connection refused or reset", i, addr, err)
		}
	}
	// Still held after it answered.
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		t.Fatalf("another listener took the reserved address %s after use", addr)
	}
}

// connectOnce is a client's first exchange with addr: dial, then read the
// server's first byte, each bounded by refuseBound (a fixture that does
// not refuse fails the test in a second, not at the package timeout). It
// returns the first failure; a byte that arrives is a failure of the
// fixture, reported as such.
func connectOnce(addr string) error {
	c, err := net.DialTimeout("tcp", addr, refuseBound)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(refuseBound)); err != nil {
		return err
	}
	n, err := c.Read(make([]byte, 1))
	if n > 0 {
		return fmt.Errorf("the fixture sent %d byte(s)", n)
	}
	return err
}
