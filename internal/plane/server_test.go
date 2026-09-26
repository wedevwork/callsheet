package plane

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

func get(t *testing.T, c *http.Client, url string) (int, string, string, error) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b), nil
}

// TestTLSService exercises the real verified listener: right, wrong and
// missing CA, hostname verification, TLS 1.1 refusal and plaintext.
func TestTLSService(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	mustInit(t, d, root, "localhost", "127.0.0.1")
	ca := readCA(t, root)
	var hits atomic.Int64
	d.wrap = func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); h.ServeHTTP(w, r) })
	}
	logs := &logBuffer{}
	s := serveBG(t, d, RunOptions{StateDir: root, Logger: logs.logger()})
	addr := s.addr.String()
	if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":0") {
		t.Fatalf("listening on %s", addr)
	}
	base := "https://" + addr
	code, ctype, body, err := get(t, client(t, ca, "", tls.VersionTLS13), base+HealthPath)
	if err != nil || code != 200 || ctype != "application/json" || body != "{\"status\":\"ok\",\"version\":1}\n" {
		t.Fatalf("health = %d %q %q %v", code, ctype, body, err)
	}
	// DNS SAN, with TLS 1.2 as the minimum.
	_, port, _ := net.SplitHostPort(addr)
	c := client(t, ca, "localhost", tls.VersionTLS12)
	if code, _, _, err := get(t, c, "https://127.0.0.1:"+port+HealthPath); err != nil || code != 200 {
		t.Fatalf("localhost SAN over TLS 1.2 = %d %v", code, err)
	}
	for name, c := range map[string]*http.Client{
		"wrong CA": client(t, readCA(t, func() string { r := newRoot(t); mustInit(t, testDeps(t), r, "localhost"); return r }()), "", tls.VersionTLS13),
		"no CA":    client(t, nil, "", tls.VersionTLS13),
		"SAN miss": client(t, ca, "other.example", tls.VersionTLS13),
	} {
		if _, _, _, err := get(t, c, base+HealthPath); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The server refuses protocol versions below TLS 1.2.
	for _, v := range []uint16{tls.VersionTLS10, tls.VersionTLS11} {
		pool := x509.NewCertPool()
		pool.AddCert(ca)
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{RootCAs: pool, MinVersion: v, MaxVersion: v})
		if err == nil {
			conn.Close()
			t.Fatalf("TLS version %x accepted", v)
		}
		if !strings.Contains(err.Error(), "protocol version") {
			t.Fatalf("TLS version %x: %v", v, err)
		}
	}
	// Plaintext HTTP to the TLS socket never reaches a handler.
	before := hits.Load()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "GET /api/v1/health HTTP/1.1\r\nHost: x\r\n\r\n")
	reply, _ := io.ReadAll(bufio.NewReader(conn))
	conn.Close()
	if strings.Contains(string(reply), `"status":"ok"`) || (len(reply) > 0 && !strings.HasPrefix(string(reply), "HTTP/1.0 400 Bad Request")) {
		t.Fatalf("plaintext reply %q", reply)
	}
	if hits.Load() != before {
		t.Fatal("a plaintext request reached a handler")
	}
	// Other routes and methods return contract JSON errors.
	cl := client(t, ca, "", tls.VersionTLS13)
	for _, c := range []struct {
		method, path string
		status       int
		code         contract.Code
	}{
		{"GET", "/", 404, contract.CodeNotFound},
		{"GET", "/api/v1/health/", 404, contract.CodeNotFound},
		{"GET", "/api/v1/ca.key", 404, contract.CodeNotFound},
		{"POST", HealthPath, 400, contract.CodeInvalidArgument},
		{"HEAD", HealthPath, 400, ""},
		{"DELETE", HealthPath, 400, contract.CodeInvalidArgument},
	} {
		req, _ := http.NewRequest(c.method, base+c.path, strings.NewReader("SECRET-BODY"))
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || strings.Contains(string(b), "SECRET-BODY") {
			t.Fatalf("%s %s = %d %q", c.method, c.path, resp.StatusCode, b)
		}
		if c.code != "" {
			var e struct{ Error struct{ Code contract.Code } }
			if json.Unmarshal(b, &e) != nil || e.Error.Code != c.code || resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("%s %s body %q", c.method, c.path, b)
			}
		}
	}
	if err := s.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("run returned %v", err)
	}
	// The listening record names the actual address and fingerprint.
	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		json.Unmarshal([]byte(line), &rec)
		if rec["msg"] == "listening" {
			break
		}
	}
	if rec["msg"] != "listening" || rec["bind"] != addr || rec["ca_fingerprint"] != Fingerprint(ca.Raw) || rec["component"] != "plane" || rec["level"] != "INFO" {
		t.Fatalf("listening record %v", rec)
	}
	if strings.Contains(logs.String(), "SECRET-BODY") || strings.Contains(logs.String(), "PRIVATE KEY") {
		t.Fatal("logs leak request or key data")
	}
	// The port and the lock are released.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	ln.Close()
	lk, err := layout{root: root}.acquire()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	lk.release()
}

func TestServerLimits(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	mustInit(t, d, root, "127.0.0.1")
	s := serveBG(t, d, RunOptions{StateDir: root})
	// Oversized headers are rejected before any handler.
	c := client(t, readCA(t, root), "", tls.VersionTLS13)
	req, _ := http.NewRequest("GET", "https://"+s.addr.String()+HealthPath, nil)
	req.Header.Set("X-Big", strings.Repeat("a", 24<<10))
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("oversized header = %d", resp.StatusCode)
		}
	}
	if readHeaderTimeout != 5*time.Second || readTimeout != 10*time.Second || writeTimeout != 10*time.Second || idleTimeout != 30*time.Second || maxHeaderBytes != 16<<10 {
		t.Fatal("server bounds changed")
	}
}

// TestRunBootstrap: first run on empty state initializes exactly as init,
// logs the fingerprint, keeps quiet otherwise, and a second run loads the
// identical files.
func TestRunBootstrap(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	err := d.run(bg, RunOptions{StateDir: root})
	wantCode(t, err, contract.CodeInvalidArgument, "at least one --san")
	logs := &logBuffer{}
	s := serveBG(t, d, RunOptions{StateDir: root, Bind: "127.0.0.1:0", BindSet: true, SANs: []string{"localhost"}, SANsSet: true, Logger: logs.logger()})
	s.stop(t)
	snap := snapshot(t, root)
	fp := Fingerprint(readCA(t, root).Raw)
	if !strings.Contains(logs.String(), `"msg":"initialized"`) || !strings.Contains(logs.String(), fp) {
		t.Fatalf("bootstrap log %s", logs.String())
	}
	logs2 := &logBuffer{}
	s = serveBG(t, testDeps(t), RunOptions{StateDir: root, Logger: logs2.logger()})
	s.stop(t)
	sameSnapshot(t, snap, snapshot(t, root))
	if strings.Contains(logs2.String(), `"msg":"initialized"`) {
		t.Fatal("second run reinitialized")
	}
	// Bootstrap inputs on initialized state must match.
	err = testDeps(t).run(bg, RunOptions{StateDir: root, SANs: []string{"other"}, SANsSet: true})
	wantCode(t, err, contract.CodeConflict, "reissue")
	// A canceled context stops before any listener.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	d = testDeps(t)
	d.listen = func(string, string) (net.Listener, error) {
		t.Error("listened after cancel")
		return nil, errors.New("x")
	}
	if err := d.run(ctx, RunOptions{StateDir: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run = %v", err)
	}
	if _, err := d.init(ctx, InitOptions{StateDir: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled init = %v", err)
	}
	if _, err := d.inspect(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled status = %v", err)
	}
	if _, err := d.reissue(ctx, ReissueOptions{StateDir: root, SANs: []string{"x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reissue = %v", err)
	}
	sameSnapshot(t, snap, snapshot(t, root))
	// Run refuses unsafe key modes before listening.
	os.Chmod(layout{root: root}.path(serverKeyName), 0o640)
	err = d.run(bg, RunOptions{StateDir: root})
	wantCode(t, err, contract.CodeTrustFailed, "0640")
}

// failingListener accepts nothing: Accept fails permanently.
type failingListener struct {
	net.Listener
	closed atomic.Bool
}

func (f *failingListener) Accept() (net.Conn, error) {
	return nil, errors.New("injected accept failure")
}
func (f *failingListener) Close() error { f.closed.Store(true); return f.Listener.Close() }

// TestServerFailureContract is delegated from tests/function
// (TestPlaneTLS/bounded-shutdown). Do not rename or skip its subtests.
func TestServerFailureContract(t *testing.T) {
	released := func(t *testing.T, root string) {
		t.Helper()
		lk, err := layout{root: root}.acquire()
		if err != nil {
			t.Fatalf("ownership not released: %v", err)
		}
		lk.release()
	}
	t.Run("listen", func(t *testing.T) {
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		d.listen = func(string, string) (net.Listener, error) { return nil, errors.New("injected listen failure") }
		d.ready = func(net.Addr) { t.Error("ready after a listen failure") }
		logs := &logBuffer{}
		err := d.run(bg, RunOptions{StateDir: root, Logger: logs.logger()})
		wantCode(t, err, contract.CodeUnavailable, "cannot listen on 127.0.0.1:0", "injected listen failure")
		if strings.Contains(logs.String(), "listening") {
			t.Fatal("listening record after a listen failure")
		}
		released(t, root)
	})
	t.Run("serve", func(t *testing.T) {
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		var fl *failingListener
		d.listen = func(n, a string) (net.Listener, error) {
			ln, err := net.Listen(n, a)
			fl = &failingListener{Listener: ln}
			return fl, err
		}
		err := d.run(bg, RunOptions{StateDir: root})
		wantCode(t, err, contract.CodeUnavailable, "stopped unexpectedly", "injected accept failure")
		if !fl.closed.Load() {
			t.Fatal("listener not closed")
		}
		released(t, root)
	})
	t.Run("shutdown-deadline", func(t *testing.T) {
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "127.0.0.1")
		d.shutdownTimeout = 50 * time.Millisecond
		entered, exited := make(chan struct{}), make(chan struct{})
		d.wrap = func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/block" {
					h.ServeHTTP(w, r)
					return
				}
				defer close(exited)
				close(entered)
				<-r.Context().Done() // released only by the forced Close
			})
		}
		s := serveBG(t, d, RunOptions{StateDir: root})
		reqDone := make(chan error, 1)
		go func() {
			_, _, _, err := get(t, client(t, readCA(t, root), "", tls.VersionTLS13), "https://"+s.addr.String()+"/block")
			reqDone <- err
		}()
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("blocking request never started")
		}
		start := time.Now()
		err := s.stop(t)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run = %v", err)
		}
		// The in-flight handler was joined before run returned.
		select {
		case <-exited:
		default:
			t.Fatal("run returned before its handler finished")
		}
		if el := time.Since(start); el > 4*time.Second {
			t.Fatalf("forced shutdown took %v", el)
		}
		select {
		case err := <-reqDone:
			if err == nil {
				t.Fatal("blocked request completed after a forced close")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("client never observed the forced close")
		}
		ln, err := net.Listen("tcp", s.addr.String())
		if err != nil {
			t.Fatalf("port not released: %v", err)
		}
		ln.Close()
		released(t, root)
	})
}

func TestTrackerRefusesAfterClose(t *testing.T) {
	tr := newTracker()
	tr.closeAndWait()
	rec := &respRecorder{h: http.Header{}}
	tr.wrap(serviceHandler()).ServeHTTP(rec, &http.Request{Method: "GET"})
	if rec.code != 503 {
		t.Fatalf("after close = %d", rec.code)
	}
}

type respRecorder struct {
	h    http.Header
	code int
}

func (r *respRecorder) Header() http.Header         { return r.h }
func (r *respRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *respRecorder) WriteHeader(c int)           { r.code = c }
