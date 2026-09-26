package gittransport

import (
	"fmt"
	"strings"
	"testing"
)

// fatalRecorder is a fatalHelper double that records Fatalf calls instead of
// stopping the test.
type fatalRecorder struct {
	helpers int
	fatals  []string
}

func (r *fatalRecorder) Helper() { r.helpers++ }
func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

// TestSANMismatchHostOracle is the FP-4 contract test, executed by name from
// tests/function (TestHardeningSANOracle). The oracle accepts only a
// listener on the literal 127.0.0.1, independently of sanMismatchHost, and
// rejects other loopback addresses and malformed input on any host. Do not
// rename or skip.
func TestSANMismatchHostOracle(t *testing.T) {
	for _, c := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:1234", true},
		{"127.0.0.1:0", true},
		{"127.0.0.2:1234", false},
		{"127.1.2.3:1234", false},
		{"[::1]:1234", false},
		{"localhost:1234", false},
		{"0.0.0.0:1234", false},
		{"127.0.0.1", false},
		{"not an address", false},
		{"", false},
	} {
		t.Run(c.addr, func(t *testing.T) {
			r := &fatalRecorder{}
			requireSANMismatchHost(r, c.addr)
			if r.helpers != 1 {
				t.Fatalf("Helper calls = %d", r.helpers)
			}
			if c.ok {
				if len(r.fatals) != 0 {
					t.Fatalf("%q rejected: %v", c.addr, r.fatals)
				}
				return
			}
			if len(r.fatals) != 1 || !strings.Contains(r.fatals[0], fmt.Sprintf("%q is not on 127.0.0.1", c.addr)) {
				t.Fatalf("%q: fatals = %q", c.addr, r.fatals)
			}
		})
	}
}
