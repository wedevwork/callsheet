package plane

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// lockHelperEnv makes the test binary act as a lock-holder helper process
// (TestNativeStateContract/flock): it takes the state lock on the named
// root, prints "locked" and holds the lock until stdin reaches EOF.
const lockHelperEnv = "CALLSHEET_PLANE_LOCK_HELPER"

func TestMain(m *testing.M) {
	if root := os.Getenv(lockHelperEnv); root != "" {
		os.Exit(lockHelper(root))
	}
	os.Exit(m.Run())
}

func lockHelper(root string) int {
	lk, err := layout{root: root}.acquire()
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return 3
	}
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
	lk.release()
	return 0
}

// lockHolder is a running helper process holding the state lock.
type lockHolder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan error
}

// startLockHolder starts the helper on root and waits (bounded) until it
// reports that it holds the lock. Cleanup kills and reaps it.
func startLockHolder(t *testing.T, root string) *lockHolder {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &lockHolder{cmd: cmd, stdin: stdin, done: make(chan error, 1)}
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- s
		io.Copy(io.Discard, stdout)
		h.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		select {
		case <-h.done:
		case <-time.After(20 * time.Second):
			t.Error("lock helper was not reaped")
		}
	})
	select {
	case s := <-line:
		if s != "locked\n" {
			t.Fatalf("lock helper: %q", s)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("lock helper did not report the lock")
	}
	return h
}

// release closes stdin and waits for a clean exit.
func (h *lockHolder) release(t *testing.T) {
	t.Helper()
	h.stdin.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("lock helper exit: %v", err)
		}
		h.done <- nil // let Cleanup observe the reaped process
	case <-time.After(20 * time.Second):
		t.Fatal("lock helper did not exit")
	}
}

// kill ends the helper with SIGKILL, leaving the kernel to drop its lock.
func (h *lockHolder) kill(t *testing.T) {
	t.Helper()
	h.cmd.Process.Kill()
	select {
	case <-h.done:
		h.done <- nil
	case <-time.After(20 * time.Second):
		t.Fatal("lock helper was not reaped after kill")
	}
}

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

// testDeps are production deps with a real clock and an interface
// enumeration that fails the test if consulted.
func testDeps(t testing.TB) *deps {
	d := defaultDeps()
	d.addrs = func() ([]netip.Addr, error) {
		t.Error("unexpected interface enumeration")
		return nil, errors.New("unexpected")
	}
	return d
}

func newRoot(t testing.TB) string { return filepath.Join(t.TempDir(), "state") }

var bg = context.Background()

func codeOf(err error) contract.Code { return contract.CodeOf(err) }

func wantCode(t *testing.T, err error, code contract.Code, substr ...string) {
	t.Helper()
	if codeOf(err) != code {
		t.Fatalf("err = %v (code %q), want %q", err, codeOf(err), code)
	}
	for _, s := range substr {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("err = %v, want it to contain %q", err, s)
		}
	}
}

func initOpts(root string, sans ...string) InitOptions {
	return InitOptions{StateDir: root, Bind: "127.0.0.1:0", BindSet: true, SANs: sans, SANsSet: true}
}

func mustInit(t testing.TB, d *deps, root string, sans ...string) Status {
	t.Helper()
	st, err := d.init(bg, initOpts(root, sans...))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return st
}

// snapshot reads the five durable files (nil for a missing file).
func snapshot(t testing.TB, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, rel := range durable {
		b, err := os.ReadFile(layout{root: root}.path(rel))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[rel] = b
	}
	return out
}

func sameSnapshot(t *testing.T, a, b map[string][]byte, except ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, e := range except {
		skip[e] = true
	}
	for _, rel := range durable {
		if !skip[rel] && !bytes.Equal(a[rel], b[rel]) {
			t.Fatalf("%s changed", rel)
		}
	}
}

// tempLeftovers lists unpublished temporaries under root.
func tempLeftovers(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(e.Name(), tempPrefix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// failAt injects err at exactly one (op, name) boundary.
func failAt(op, name string) func(string, string) error {
	return func(o, n string) error {
		if o == op && n == name {
			return fmt.Errorf("injected %s failure at %s", o, n)
		}
		return nil
	}
}

// logBuffer is a concurrency-safe JSON log capture.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, nil)).With("component", "plane")
}

// served is a plane run in the background.
type served struct {
	addr   net.Addr
	cancel context.CancelFunc
	done   chan error
}

// serveBG runs d.run with ready reporting and returns once listening.
func serveBG(t *testing.T, d *deps, o RunOptions) *served {
	t.Helper()
	ready := make(chan net.Addr, 1)
	d.ready = func(a net.Addr) { ready <- a }
	ctx, cancel := context.WithCancel(bg)
	s := &served{cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- d.run(ctx, o) }()
	select {
	case s.addr = <-ready:
	case err := <-s.done:
		cancel()
		t.Fatalf("run ended before listening: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("run did not become ready")
	}
	t.Cleanup(func() { s.stop(t) })
	return s
}

// stop cancels the run and returns its result (once).
func (s *served) stop(t *testing.T) error {
	t.Helper()
	s.cancel()
	select {
	case err, ok := <-s.done:
		if ok {
			close(s.done)
		}
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("run did not stop")
		return nil
	}
}

// client trusts only caPEM's certificate, with bounded timeouts.
func client(t *testing.T, ca *x509.Certificate, serverName string, maxVersion uint16) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if ca != nil {
		pool.AddCert(ca)
	}
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS10, MaxVersion: maxVersion},
		Proxy:             nil,
		DisableKeepAlives: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func readCA(t testing.TB, root string) *x509.Certificate {
	t.Helper()
	c, err := loadCert(layout{root: root}.path(caCertName))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readServer(t testing.TB, root string) *x509.Certificate {
	t.Helper()
	c, err := loadCert(layout{root: root}.path(serverCertName))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
