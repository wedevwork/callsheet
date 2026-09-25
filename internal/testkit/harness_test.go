package testkit

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

type fatalSentinel struct{ msg string }

// fakeTB captures Fatalf/Cleanup so setup failure paths can be asserted.
type fakeTB struct {
	testing.TB
	cleanups []func()
	errs     []string
}

func (f *fakeTB) Helper()                        {}
func (f *fakeTB) Cleanup(fn func())              { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Errorf(format string, a ...any) { f.errs = append(f.errs, fmt.Sprintf(format, a...)) }
func (f *fakeTB) Fatalf(format string, a ...any) {
	panic(fatalSentinel{fmt.Sprintf(format, a...)})
}
func (f *fakeTB) runCleanups() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func expectFatal(t *testing.T, f func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		s, ok := r.(fatalSentinel)
		if !ok {
			t.Fatalf("expected fatal, got %v", r)
		}
		msg = s.msg
	}()
	f()
	return ""
}

func TestFixtureTrustGeneration(t *testing.T) {
	before := time.Now()
	ca, err := NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || ca.Cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatalf("CA = %+v", ca.Cert)
	}
	if pub := ca.Cert.PublicKey.(*ecdsa.PublicKey); pub.Curve != elliptic.P256() {
		t.Fatal("CA curve must be P-256")
	}
	if d := before.Add(-time.Hour).Sub(ca.Cert.NotBefore); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("NotBefore = %v", ca.Cert.NotBefore)
	}
	if d := before.Add(24 * time.Hour).Sub(ca.Cert.NotAfter); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("NotAfter = %v", ca.Cert.NotAfter)
	}
	leaf, err := ca.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	opts := x509.VerifyOptions{Roots: ca.Pool()}
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		opts.DNSName = host
		if _, err := leaf.Leaf.Verify(opts); err != nil {
			t.Fatalf("leaf does not cover %s: %v", host, err)
		}
	}
	opts.DNSName = "127.0.0.2"
	if _, err := leaf.Leaf.Verify(opts); err == nil {
		t.Fatal("leaf must not cover 127.0.0.2")
	}
	other, _ := NewFixtureCA()
	opts.DNSName = "localhost"
	opts.Roots = other.Pool()
	if _, err := leaf.Leaf.Verify(opts); err == nil {
		t.Fatal("unrelated CA must not verify leaf")
	}
	tr := ca.Client().Transport.(*http.Transport)
	if tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.Proxy != nil {
		t.Fatal("client TLS config")
	}
}

func TestHealthAndGitMount(t *testing.T) {
	seen := make(chan string, 4)
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
		io.WriteString(w, "spy")
	})
	h := NewHarness(t, WithGitHandler(spy))
	resp, err := h.Client.Get(h.URL + "/test/health")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != `{"ok":true}` {
		t.Fatalf("health = %d %s", resp.StatusCode, b)
	}
	resp, err = h.Client.Get(h.URL + "/test/git/repo.git/info/refs?service=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-seen; got != "/test/git/repo.git/info/refs" {
		t.Fatalf("git handler saw %q (must be full path)", got)
	}
	// Without a git handler /test/git/ is 404.
	h2 := NewHarness(t)
	resp, err = h2.Client.Get(h2.URL + "/test/git/repo.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("absent git handler = %d", resp.StatusCode)
	}
	if st, err := os.Stat(h.Root); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("root perms: %v %v", st, err)
	}
	if h.CA() == nil || !strings.HasPrefix(h.URL, "https://127.0.0.1:") {
		t.Fatalf("url = %s", h.URL)
	}
}

func TestInvalidOptionsFailSetupWithCleanup(t *testing.T) {
	for name, opts := range map[string][]Option{
		"nil":       {WithGitHandler(nil)},
		"duplicate": {WithGitHandler(http.NotFoundHandler()), WithGitHandler(http.NotFoundHandler())},
	} {
		t.Run(name, func(t *testing.T) {
			ftb := &fakeTB{TB: t}
			msg := expectFatal(t, func() { NewHarness(ftb, opts...) })
			if !strings.Contains(msg, "git handler") && !strings.Contains(msg, "WithGitHandler(nil)") {
				t.Fatalf("fatal = %q", msg)
			}
			ftb.runCleanups()
			if len(ftb.errs) != 0 {
				t.Fatalf("cleanup errors: %v", ftb.errs)
			}
		})
	}
}

func TestHelloEchoAndCorrelation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	sc, err := h.DialSidecar(ctx, contract.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"text":"你好","n":1}`)
	got, err := sc.Echo(ctx, "r1", body)
	if err != nil || string(got) != string(body) {
		t.Fatalf("echo = %s %v", got, err)
	}
	// A stray frame with another id is a correlation error.
	sc.frames <- Frame{Version: 1, Type: "echo_ok", RequestID: "zzz"}
	if _, err := sc.Echo(ctx, "r2", body); err == nil || !strings.Contains(err.Error(), "correlation") {
		t.Fatalf("correlation err = %v", err)
	}
	// Drain r2's genuine reply, then check a wrong type is rejected.
	<-sc.frames
	sc.frames <- Frame{Version: 1, Type: "bogus", RequestID: "r3"}
	if _, err := sc.Echo(ctx, "r3", body); err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Fatalf("type err = %v", err)
	}
	<-sc.frames
	// Unsupported frame types get an error frame from the server.
	if err := writeFrame(ctx, sc.conn, Frame{Version: 1, Type: "task", RequestID: "x"}); err != nil {
		t.Fatal(err)
	}
	f := <-sc.frames
	if f.Type != "error" || contract.CodeOf(decodeWireError(f.Body)) != contract.CodeInvalidArgument {
		t.Fatalf("unsupported frame reply = %+v", f)
	}
	sc.frames <- Frame{Version: 1, Type: "error", RequestID: "r4", Body: json.RawMessage(`{"error":{"code":"conflict","message":"m"}}`)}
	if _, err := sc.Echo(ctx, "r4", body); contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("error frame = %v", err)
	}
	<-sc.frames
	// Oversized frames are refused locally.
	if _, err := sc.Echo(ctx, "big", json.RawMessage(`"`+strings.Repeat("a", MaxFrameBytes)+`"`)); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal("second close")
	}
	if _, err := sc.Echo(ctx, "after", body); err == nil {
		t.Fatal("echo after close succeeded")
	}
}

func TestVersionMismatch(t *testing.T) {
	h := NewHarness(t)
	_, err := h.DialSidecar(context.Background(), 2)
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Code != contract.CodeProtocolMismatch {
		t.Fatalf("err = %v", err)
	}
	if ce.Details["local_version"].(float64) != 1 || ce.Details["remote_version"].(float64) != 2 {
		t.Fatalf("details = %v", ce.Details)
	}
	// Raw peer: mismatch closes with policy violation before app traffic.
	c, _, err := websocket.Dial(context.Background(), "wss://"+h.Addr()+NodeStreamPath, &websocket.DialOptions{HTTPClient: h.Client})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	writeFrame(context.Background(), c, Frame{Version: 9, Type: "hello", RequestID: "h1"})
	f, err := readFrame(context.Background(), c)
	if err != nil || f.Type != "error" {
		t.Fatalf("frame = %+v %v", f, err)
	}
	_, err = readFrame(context.Background(), c)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v (%v)", websocket.CloseStatus(err), err)
	}
	// First frame that is not hello is also rejected.
	c2, _, _ := websocket.Dial(context.Background(), "wss://"+h.Addr()+NodeStreamPath, &websocket.DialOptions{HTTPClient: h.Client})
	defer c2.CloseNow()
	writeFrame(context.Background(), c2, Frame{Version: 1, Type: "echo", RequestID: "e"})
	f, _ = readFrame(context.Background(), c2)
	if f.Type != "error" {
		t.Fatalf("non-hello first frame = %+v", f)
	}
	// Binary and malformed frames are errors, not panics.
	c3, _, _ := websocket.Dial(context.Background(), "wss://"+h.Addr()+NodeStreamPath, &websocket.DialOptions{HTTPClient: h.Client})
	defer c3.CloseNow()
	c3.Write(context.Background(), websocket.MessageBinary, []byte{1})
	if _, err := readFrame(context.Background(), c3); err == nil {
		t.Fatal("binary hello accepted")
	}
	c4, _, _ := websocket.Dial(context.Background(), "wss://"+h.Addr()+NodeStreamPath, &websocket.DialOptions{HTTPClient: h.Client})
	defer c4.CloseNow()
	c4.Write(context.Background(), websocket.MessageText, []byte("{"))
	if _, err := readFrame(context.Background(), c4); err == nil {
		t.Fatal("malformed hello accepted")
	}
	if decodeWireError(json.RawMessage(`nope`)).(*contract.Error).Code != contract.CodeInternal {
		t.Fatal("malformed error frame")
	}
}

func TestWrongCAAndPlainHTTP(t *testing.T) {
	h := NewHarness(t)
	other, err := NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Client().Get(h.URL + "/test/health")
	var uerr x509.UnknownAuthorityError
	if !errors.As(err, &uerr) {
		t.Fatalf("wrong CA err = %v", err)
	}
	_, err = h.dialSidecar(context.Background(), other.Client(), 1)
	if contract.CodeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("sidecar wrong CA = %v", err)
	}
	if classifyDialErr(errors.New("x")).(*contract.Error).Code != contract.CodeUnavailable {
		t.Fatal("classify")
	}
	resp, err := http.Get("http://" + h.Addr() + "/test/health")
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 || strings.Contains(string(b), `"ok":true`) {
			t.Fatalf("plain HTTP reached a route: %d %s", resp.StatusCode, b)
		}
	}
	if !strings.Contains(h.ServerLog(), "TLS handshake error") {
		t.Logf("server log: %q", h.ServerLog())
	}
}

func TestCloseJoinsAndPermitsRootCleanup(t *testing.T) {
	h := NewHarness(t)
	sc, err := h.DialSidecar(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(h.Root+"/f", []byte("x"), 0o600)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done:
	default:
		t.Fatal("Done not closed after Close")
	}
	for err := range h.Errs {
		t.Fatalf("unexpected harness error: %v", err)
	}
	if err := os.RemoveAll(h.Root); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal("Close must be idempotent")
	}
	if _, err := sc.Echo(context.Background(), "x", json.RawMessage(`{}`)); err == nil {
		t.Fatal("peer still usable after harness close")
	}
	if _, err := h.DialSidecar(context.Background(), 1); err == nil {
		t.Fatal("dial after close succeeded")
	}
	if h.goTracked(func() {}) {
		t.Fatal("goroutine started after close")
	}
}

func TestEchoContextCancellation(t *testing.T) {
	h := NewHarness(t)
	sc, err := h.DialSidecar(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sc.Echo(ctx, "c", json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled echo = %v", err)
	}
	dctx, dcancel := context.WithCancel(context.Background())
	dcancel()
	if _, err := h.DialSidecar(dctx, 1); err == nil {
		t.Fatal("cancelled dial succeeded")
	}
}

// TestEchoPreCancelledNeverReturnsReply is the regression test for the Echo
// cancellation race: a pre-cancelled context must yield context.Canceled
// every time, never a reply that raced ahead of ctx.Done in the select.
func TestEchoPreCancelledNeverReturnsReply(t *testing.T) {
	h := NewHarness(t)
	sc, err := h.DialSidecar(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 500; i++ {
		if _, err := sc.Echo(ctx, fmt.Sprintf("c%d", i), json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: cancelled echo = %v", i, err)
		}
	}
	// The peer stays usable: a cancelled Echo must not have sent anything
	// whose reply would be read by a later call.
	if b, err := sc.Echo(context.Background(), "live", json.RawMessage(`{"k":1}`)); err != nil || string(b) != `{"k":1}` {
		t.Fatalf("echo after cancelled calls = %s, %v", b, err)
	}
}

// TestEchoContextDoneWhileWaiting covers a context that ends after the write,
// while Echo waits: the reply goes to the real peer's reader, never to this
// view's frames channel, so only ctx.Done can end the wait.
func TestEchoContextDoneWhileWaiting(t *testing.T) {
	h := NewHarness(t)
	sc, err := h.DialSidecar(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	view := &SidecarConn{h: h, conn: sc.conn, frames: make(chan Frame), readEr: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := view.Echo(ctx, "w", json.RawMessage(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("echo past deadline = %v", err)
	}
}

func TestJoinTimeoutReported(t *testing.T) {
	ftb := &fakeTB{TB: t}
	h, err := newHarness(ftb)
	if err != nil {
		t.Fatal(err)
	}
	h.joinLimit = 50 * time.Millisecond
	release := make(chan struct{})
	h.goTracked(func() { <-release })
	err = h.Close()
	close(release)
	if err == nil || !strings.Contains(err.Error(), "did not join") {
		t.Fatalf("join timeout err = %v", err)
	}
	ftb.runCleanups()
	if len(ftb.errs) != 1 {
		t.Fatalf("cleanup must report the join failure: %v", ftb.errs)
	}
	h.wg.Wait()
}

func TestRequestAfterCloseRefused(t *testing.T) {
	h := NewHarness(t)
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	rec := &recorder{header: http.Header{}}
	h.gate(http.NotFoundHandler()).ServeHTTP(rec, &http.Request{})
	if rec.code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.code)
	}
	h.mu.Lock()
	h.closed = false
	h.mu.Unlock()
	if h.track(nil); len(h.conns) != 1 {
		t.Fatal("track")
	}
	h.untrack(nil)
	h.reportErr(errors.New("e"))
	if err := <-h.Errs; err.Error() != "e" {
		t.Fatal("reportErr")
	}
}

type recorder struct {
	header http.Header
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(c int)           { r.code = c }

func TestRepoHelpers(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root + "/go.mod"); err != nil {
		t.Fatal(err)
	}
	if _, err := findModuleRoot(t.TempDir()); err == nil {
		t.Fatal("root found in temp dir")
	}
	env := EnvWithout([]string{"A=1", "HTTP_PROXY=x", "B=2"}, ProxyVars, "C=3")
	if strings.Join(env, ",") != "A=1,B=2,C=3" {
		t.Fatalf("env = %v", env)
	}
	if d := EmptyDir(t); d == "" {
		t.Fatal("empty dir")
	}
	if GoTool() == "" {
		t.Fatal("go tool")
	}
	_ = net.IPv4len
}

// UT-5: host helper paths carry no platform suffix on the supported hosts.
func TestHostHelperPathsSuffixFree(t *testing.T) {
	if g := GoTool(); g != "go" && !strings.HasSuffix(g, string(filepath.Separator)+filepath.Join("bin", "go")) {
		t.Fatalf("GoTool = %q", g)
	}
	bin := BuildBinary(t, "./cmd/fake-adapter", "fake-adapter")
	if filepath.Base(bin) != "fake-adapter" {
		t.Fatalf("BuildBinary = %q", bin)
	}
	test := BuildTestBinary(t, "./internal/contract", "contract")
	if filepath.Base(test) != "contract.test" {
		t.Fatalf("BuildTestBinary = %q", test)
	}
	for _, p := range []string{bin, test} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s not an executable file: %v", p, err)
		}
	}
}
