package reale2e

import (
	"testing"
)

// TestOfflineAttemptPasses runs one complete offline attempt (scripted
// coordinator, fake plane, scripted children) and requires a PASS report.
func TestOfflineAttemptPasses(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	code := f.attempt()
	if code != 0 {
		t.Fatalf("attempt = %d\nstdout:\n%s\nstderr:\n%s\ncodes %v", code, f.stdout.String(), f.stderr.String(), failedCodes(f.report()))
	}
	r := f.report()
	if r.Result != ResultPass || r.Flow != FlowDefault {
		t.Fatalf("report %+v", r)
	}
}
