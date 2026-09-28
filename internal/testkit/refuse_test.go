//go:build linux || darwin

package testkit

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// TestRefusingAddr: the unreachable-plane fixture refuses every
// connection for the test's whole lifetime, and no other listener (a
// parallel test's TLS server, say) can take its address meanwhile.
func TestRefusingAddr(t *testing.T) {
	addr := RefusingAddr(t)
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		t.Fatalf("another listener took the reserved address %s", addr)
	}
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			c.Close()
			t.Fatalf("a connection to %s was accepted", addr)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("dial %s: %v, want connection refused", addr, err)
		}
	}
}
