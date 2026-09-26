package sidecar

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// planeProc is a real "callsheet plane run" subprocess.
type planeProc struct {
	cmd  *exec.Cmd
	logs *syncLog
	done chan struct{}
	err  error
	addr string
}

// planeCLI runs one bounded CLI command with an isolated HOME.
func planeCLI(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command(cliBinary(t), args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("callsheet %v: %v\n%s", args, err, out)
	}
}

// runPlane starts "plane run" on root and waits for its listening record
// (the explicit readiness event). It returns ok=false if the process
// exited first, e.g. because its fixed port is taken.
func runPlane(t *testing.T, root string) (*planeProc, bool) {
	t.Helper()
	p := &planeProc{logs: newSyncLog(), done: make(chan struct{})}
	p.cmd = exec.Command(cliBinary(t), "plane", "run", "--state-dir", root)
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	p.cmd.Stderr = p.logs
	p.cmd.WaitDelay = 5 * time.Second
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(testWait):
			t.Errorf("plane %d not reaped", p.cmd.Process.Pid)
		}
	})
	deadline := time.After(testWait)
	for {
		if p.addr = listenAddr(p.logs.String()); p.addr != "" {
			return p, true
		}
		select {
		case <-p.logs.sig:
		case <-p.done:
			if p.addr = listenAddr(p.logs.String()); p.addr != "" {
				return p, true
			}
			return p, false
		case <-deadline:
			// Readiness and the deadline may be ready together: recheck
			// before failing.
			if p.addr = listenAddr(p.logs.String()); p.addr != "" {
				return p, true
			}
			t.Fatalf("plane not ready:\n%s", p.logs.String())
		}
	}
}

// stop sends SIGTERM and requires the interrupted exit.
func (p *planeProc) stop(t *testing.T) {
	t.Helper()
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(testWait):
		t.Fatal("plane ignored SIGTERM")
	}
	var ee *exec.ExitError
	if !errors.As(p.err, &ee) || ee.ExitCode() != 130 {
		t.Fatalf("plane exit %v:\n%s", p.err, p.logs.String())
	}
}

// reservedPlane initializes a plane bound to a fixed loopback port, so a
// restart listens at the address the sidecar enrolled. The port is chosen
// by binding :0 and releasing it; a start that loses the port to another
// process is retried with a fresh state and port, and a restart retries
// rebinding the same released port, both within a bounded deadline.
func reservedPlane(t *testing.T) (root string, p *planeProc) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		root = filepath.Join(t.TempDir(), "plane"+strconv.Itoa(attempt))
		planeCLI(t, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:"+strconv.Itoa(port), "--san", "127.0.0.1")
		if p, ok := runPlane(t, root); ok {
			return root, p
		}
	}
	t.Fatal("could not reserve a loopback port for the plane")
	return "", nil
}

// restart starts the plane again on its configured port, retrying while
// the released port is briefly unavailable.
func restart(t *testing.T, root string) *planeProc {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if p, ok := runPlane(t, root); ok {
			return p
		}
	}
	t.Fatal("the plane could not rebind its port")
	return nil
}

// planeClient trusts the plane at root.
func planeClientFor(t *testing.T, root, url string) *client.Client {
	t.Helper()
	ca, err := os.ReadFile(filepath.Join(root, "pki", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(url, client.Trust{CAPEM: ca})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// untilConnected advances the retry clock after each observed failed
// attempt until session > after connects and is acknowledged.
func untilConnected(t *testing.T, clk *testkit.FakeClock, ev *events, after int) int {
	t.Helper()
	deadline := time.After(testWait)
	connected := 0
	// handle processes one event and reports whether the reconnected
	// session was acknowledged.
	handle := func(e event) bool {
		switch e.kind {
		case evEnded:
			if terminal(e.err) {
				t.Fatalf("terminal error while reconnecting: %v", e.err)
			}
		case evBackoff:
			if err := clk.AwaitWaiter(testWait, testkit.HasTimer(e.delay)); err != nil {
				t.Fatal(err)
			}
			clk.Advance(e.delay)
		case evConnected:
			if e.session > after {
				connected = e.session
			}
		case evAck:
			return connected != 0 && e.session == connected
		}
		return false
	}
	for {
		select {
		case e := <-ev.ch:
			if handle(e) {
				return connected
			}
		case <-deadline:
			// Events and the deadline may be ready together: process the
			// events already delivered before failing.
			for {
				select {
				case e := <-ev.ch:
					if handle(e) {
						return connected
					}
					continue
				default:
				}
				t.Fatal("the sidecar did not reconnect")
			}
		}
	}
}

// TestNodeReconnectContract is delegated from tests/function
// (TestNodeReconnect/restart and /disconnect). Inside package sidecar it
// runs deps.run, the implementation behind Run, with an injected retry
// clock and jitter against a real plane subprocess and the production
// dial and session. Retry time advances only after the failed attempt or
// the restarted plane's readiness was observed. Do not rename or skip its
// subtests.
func TestNodeReconnectContract(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		root, p := reservedPlane(t)
		url := "https://" + p.addr
		clk := testkit.NewFakeClock(time.Now())
		d := testDeps(clk)
		ev := observe(d)
		state := newRoot(t)
		e, err := d.enroll(bg, EnrollOptions{StateDir: state, PlaneURL: url, CAFile: filepath.Join(root, "pki", "ca.crt"), SoftwareVersion: "test-1"})
		if err != nil {
			t.Fatal(err)
		}
		identity := fileBytes(t, filepath.Join(state, identityName))
		enrollment := fileBytes(t, filepath.Join(state, enrollmentName))
		r := startRun(t, d, state, discard())
		if s := untilConnected(t, clk, ev, 0); s != 1 {
			t.Fatalf("first session %d", s)
		}
		online := func() {
			t.Helper()
			n, err := planeClientFor(t, root, url).ShowNode(bg, e.NodeID)
			if err != nil || n.Liveness != contract.LivenessOnline || *n.SoftwareVersion != "test-1" {
				t.Fatalf("node = %+v %v", n, err)
			}
		}
		online()
		p.stop(t)
		wantCode(t, ev.await(t, evEnded).err, contract.CodeUnavailable)
		// The retry waits on the injected clock while the plane is down.
		b := ev.await(t, evBackoff)
		if err := clk.AwaitWaiter(testWait, testkit.HasTimer(b.delay)); err != nil {
			t.Fatal(err)
		}
		p = restart(t, root)
		if p.addr != strings.TrimPrefix(url, "https://") {
			t.Fatalf("restarted on %s", p.addr)
		}
		clk.Advance(b.delay)
		if s := untilConnected(t, clk, ev, 1); s < 2 {
			t.Fatalf("reconnected session %d", s)
		}
		online()
		if !equalBytes(identity, fileBytes(t, filepath.Join(state, identityName))) || !equalBytes(enrollment, fileBytes(t, filepath.Join(state, enrollmentName))) {
			t.Fatal("reconnecting changed local state")
		}
		r.cancel()
		if err := r.result(t); !errors.Is(err, context.Canceled) {
			t.Fatalf("run = %v", err)
		}
	})
	t.Run("disconnect", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "plane")
		planeCLI(t, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "127.0.0.1")
		p, ok := runPlane(t, root)
		if !ok {
			t.Fatalf("plane did not start:\n%s", p.logs.String())
		}
		px := startProxy(t, p.addr)
		url := "https://" + px.addr()
		clk := testkit.NewFakeClock(time.Now())
		d := testDeps(clk)
		ev := observe(d)
		state := newRoot(t)
		e, err := d.enroll(bg, EnrollOptions{StateDir: state, PlaneURL: url, CAFile: filepath.Join(root, "pki", "ca.crt"), SoftwareVersion: "test-1"})
		if err != nil {
			t.Fatal(err)
		}
		r := startRun(t, d, state, discard())
		untilConnected(t, clk, ev, 0)
		px.cut()
		if s := untilConnected(t, clk, ev, 1); s < 2 {
			t.Fatalf("session after the cut %d", s)
		}
		// The plane saw a second attachment (a new generation) for the same
		// identity, and the node is online.
		p.logs.await(t, "second attachment", func(s string) bool {
			n := 0
			for _, line := range strings.Split(s, "\n") {
				if strings.Contains(line, `"msg":"node connected"`) && strings.Contains(line, `"node_id":"`+e.NodeID+`"`) {
					n++
				}
			}
			return n >= 2
		})
		n, err := planeClientFor(t, root, "https://"+p.addr).ShowNode(bg, e.NodeID)
		if err != nil || n.Liveness != contract.LivenessOnline {
			t.Fatalf("node = %+v %v", n, err)
		}
		r.cancel()
		if err := r.result(t); !errors.Is(err, context.Canceled) {
			t.Fatalf("run = %v", err)
		}
		p.stop(t)
	})
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

// proxy forwards TCP connections to target and can cut them all.
type proxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	wg     sync.WaitGroup
}

func startProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	px := &proxy{ln: ln, target: target}
	px.wg.Add(1)
	go func() {
		defer px.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			px.mu.Lock()
			px.conns = append(px.conns, c, u)
			px.mu.Unlock()
			px.wg.Add(2)
			go func() { defer px.wg.Done(); io.Copy(u, c); u.Close() }()
			go func() { defer px.wg.Done(); io.Copy(c, u); c.Close() }()
		}
	}()
	t.Cleanup(func() { ln.Close(); px.cut(); px.wg.Wait() })
	return px
}

func (px *proxy) addr() string { return px.ln.Addr().String() }

// cut closes every forwarded connection in both directions.
func (px *proxy) cut() {
	px.mu.Lock()
	defer px.mu.Unlock()
	for _, c := range px.conns {
		c.Close()
	}
	px.conns = nil
}
