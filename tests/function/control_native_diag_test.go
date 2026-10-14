//go:build linux || darwin

package function

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// onlineFailFacts is what awaitOnline prints when the node does not come
// online: the fixture's reconnect lines, the last ShowNode result, whether
// plane.done has fired, and whether the fixture and leader pids are alive.
type onlineFailFacts struct {
	logs       string
	showErr    error
	liveness   string
	lastSeen   *time.Time
	planeDone  bool
	planeErr   error
	fixturePID int
	fixture    string
	leaderPID  int
	leader     string
}

func formatOnlineFailure(f onlineFailFacts) string {
	var b strings.Builder
	lines := reconnectLines(f.logs)
	if len(lines) == 0 {
		b.WriteString("fixture stderr: no disconnected; retrying, connected, or connection failed permanently line\n")
	} else {
		b.WriteString("fixture stderr:\n")
		for _, line := range lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if f.showErr != nil {
		fmt.Fprintf(&b, "ShowNode: %s\n", f.showErr)
	} else {
		fmt.Fprintf(&b, "ShowNode: Liveness=%s LastSeen=%s\n", f.liveness, formatLastSeen(f.lastSeen))
	}
	if f.planeDone {
		fmt.Fprintf(&b, "plane.done: fired (%v)\n", f.planeErr)
	} else {
		b.WriteString("plane.done: not fired\n")
	}
	fmt.Fprintf(&b, "fixture pid %d: %s\n", f.fixturePID, f.fixture)
	fmt.Fprintf(&b, "leader pid %d: %s\n", f.leaderPID, f.leader)
	return b.String()
}

func formatLastSeen(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// reconnectLines keeps fixture stderr lines for a disconnect, a connect,
// or a permanent failure, in the order they were written. The raw line
// carries error and retry_in_ms.
func reconnectLines(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"msg":"disconnected; retrying"`) ||
			strings.Contains(line, `"msg":"connected"`) ||
			strings.Contains(line, `"msg":"connection failed permanently"`) {
			out = append(out, line)
		}
	}
	return out
}

// chanErrorFired reports whether done already holds a value, without
// consuming it.
func chanErrorFired(done chan error) (bool, error) {
	if done == nil {
		return false, nil
	}
	select {
	case err := <-done:
		done <- err
		return true, err
	default:
		return false, nil
	}
}

// pidAlive reports whether pid is running. A non-positive pid is absent
// and is not signaled.
func pidAlive(pid int) string {
	if pid <= 0 {
		return "absent"
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return "dead: " + err.Error()
	}
	return "alive"
}

func TestOnlineFailureReportsRestartFacts(t *testing.T) {
	seen := time.Date(2026, 10, 14, 12, 46, 8, 0, time.UTC)
	logs := strings.Join([]string{
		`{"level":"INFO","msg":"connected","session":1}`,
		`{"level":"INFO","msg":"heartbeat acknowledged"}`,
		`{"level":"WARN","msg":"disconnected; retrying","error":"dial tcp: connection refused","retry_in_ms":800}`,
		`{"level":"ERROR","msg":"connection failed permanently","error":"protocol mismatch","local_version":6,"remote_version":1}`,
	}, "\n")
	msg := formatOnlineFailure(onlineFailFacts{
		logs: logs, showErr: errors.New("Get \"https://127.0.0.1/api/v1/nodes/n\": connection refused"),
		planeDone: true, planeErr: errors.New("plane exited"),
		fixturePID: 4242, fixture: "alive",
		leaderPID: 4343, leader: "dead: no such process",
	})
	connected := strings.Index(msg, `"msg":"connected"`)
	retry := strings.Index(msg, `"msg":"disconnected; retrying"`)
	permanent := strings.Index(msg, `"msg":"connection failed permanently"`)
	if connected < 0 || retry < connected || permanent < retry {
		t.Fatalf("reconnect lines out of order:\n%s", msg)
	}
	for _, want := range []string{
		`"error":"dial tcp: connection refused"`,
		`"retry_in_ms":800`,
		"ShowNode: Get \"https://127.0.0.1/api/v1/nodes/n\": connection refused",
		"plane.done: fired (plane exited)",
		"fixture pid 4242: alive",
		"leader pid 4343: dead: no such process",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure text lacks %q\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "heartbeat acknowledged") {
		t.Errorf("unrelated stderr line was printed:\n%s", msg)
	}
	ok := formatOnlineFailure(onlineFailFacts{
		liveness: contract.LivenessOnline, lastSeen: &seen,
		fixturePID: 7, fixture: "alive", leaderPID: 0, leader: "absent",
	})
	for _, want := range []string{
		"fixture stderr: no disconnected; retrying, connected, or connection failed permanently line",
		"ShowNode: Liveness=online LastSeen=2026-10-14T12:46:08Z",
		"plane.done: not fired",
		"leader pid 0: absent",
	} {
		if !strings.Contains(ok, want) {
			t.Errorf("success-ShowNode text lacks %q\n%s", want, ok)
		}
	}
	ch := make(chan error, 1)
	if fired, err := chanErrorFired(ch); fired || err != nil {
		t.Fatalf("empty done fired: %v %v", fired, err)
	}
	ch <- errors.New("plane exited")
	fired, err := chanErrorFired(ch)
	if !fired || err == nil || err.Error() != "plane exited" {
		t.Fatalf("done peek: fired %v err %v", fired, err)
	}
	fired, err = chanErrorFired(ch)
	if !fired || err == nil || err.Error() != "plane exited" {
		t.Fatalf("done peek consumed the error: fired %v err %v", fired, err)
	}
	if pidAlive(0) != "absent" || pidAlive(-1) != "absent" {
		t.Fatal("non-positive pid was not absent")
	}
	if pidAlive(os.Getpid()) != "alive" {
		t.Fatal("this process was not alive")
	}
	// Above the kernel pid limit, so the check cannot observe a recycled pid.
	if got := pidAlive(1 << 30); !strings.HasPrefix(got, "dead:") {
		t.Fatalf("impossible pid: %s", got)
	}
}
