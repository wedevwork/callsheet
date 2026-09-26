package function

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 02 function tests: exactly one top-level TestPlane* per FP.
// They drive the built callsheet binary against real state in temporary
// directories, delegate clock and failure injection to the named
// package-private contracts (run by name in a compiled test binary), and
// listen only on 127.0.0.1:0.

const (
	planeChildTimeout    = 60 * time.Second
	planeContractTimeout = 90 * time.Second
	planeReadyTimeout    = 30 * time.Second
)

// planeCLI is the built binary with an isolated HOME and working directory.
type planeCLI struct {
	bin, home, cwd string
	env            []string
}

func newPlaneCLI(t *testing.T) *planeCLI {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	return &planeCLI{
		bin:  testkit.BuildBinary(t, "./cmd/callsheet", "callsheet"),
		home: home,
		cwd:  cwd,
		env:  []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home},
	}
}

// run executes one bounded, non-interactive command and waits for it.
func (p *planeCLI) run(t *testing.T, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), planeChildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.bin, args...)
	cmd.Dir, cmd.Env = p.cwd, p.env
	cmd.WaitDelay = 5 * time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		t.Fatalf("%v: timed out after %v", args, planeChildTimeout)
	case errors.As(err, &ee):
		return result{ee.ExitCode(), out.String(), errOut.String()}
	case err != nil:
		t.Fatalf("%v: %v", args, err)
	}
	return result{0, out.String(), errOut.String()}
}

// logLines collects a child's stderr and signals its listening record.
type logLines struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	partial []byte
	ready   chan string
	once    sync.Once
}

func (l *logLines) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(b)
	l.partial = append(l.partial, b...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		var rec map[string]any
		if json.Unmarshal(l.partial[:i], &rec) == nil && rec["msg"] == "listening" {
			if bind, ok := rec["bind"].(string); ok {
				l.once.Do(func() { l.ready <- bind })
			}
		}
		l.partial = l.partial[i+1:]
	}
	return len(b), nil
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// planeProc is a running "callsheet plane run".
type planeProc struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	logs   *logLines
	addr   string
	done   chan struct{}
	err    error
}

// start runs "plane run args..." and waits (bounded) for its listening
// record. Cleanup kills the child if it is still running and reaps it.
func (p *planeCLI) start(t *testing.T, args ...string) *planeProc {
	t.Helper()
	pp := &planeProc{logs: &logLines{ready: make(chan string, 1)}, done: make(chan struct{})}
	pp.cmd = exec.Command(p.bin, append([]string{"plane", "run"}, args...)...)
	pp.cmd.Dir, pp.cmd.Env = p.cwd, p.env
	pp.cmd.Stdout, pp.cmd.Stderr = &pp.stdout, pp.logs
	pp.cmd.WaitDelay = 5 * time.Second
	if err := pp.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { pp.err = pp.cmd.Wait(); close(pp.done) }()
	t.Cleanup(func() {
		select {
		case <-pp.done:
			return
		default:
		}
		pp.cmd.Process.Kill()
		select {
		case <-pp.done:
		case <-time.After(planeChildTimeout):
			t.Errorf("plane run %d was not reaped", pp.cmd.Process.Pid)
		}
	})
	select {
	case pp.addr = <-pp.logs.ready:
	case <-pp.done:
		t.Fatalf("plane run exited before listening: %v\n%s", pp.err, pp.logs.String())
	case <-time.After(planeReadyTimeout):
		t.Fatalf("plane run not ready after %v:\n%s", planeReadyTimeout, pp.logs.String())
	}
	return pp
}

// stop signals the child and waits (bounded) for its exit code.
func (pp *planeProc) stop(t *testing.T, sig os.Signal) int {
	t.Helper()
	if err := pp.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-pp.done:
	case <-time.After(planeChildTimeout):
		t.Fatalf("plane run ignored %v for %v", sig, planeChildTimeout)
	}
	var ee *exec.ExitError
	if errors.As(pp.err, &ee) {
		return ee.ExitCode()
	}
	if pp.err != nil {
		t.Fatalf("wait: %v", pp.err)
	}
	return 0
}

// planeContract runs one delegated contract by name in bin (the compiled
// package test binary) and requires exact RUN and PASS evidence for it
// and each mandatory subtest, rejecting any skip or failure.
func planeContract(t *testing.T, bin, pkg, name string, subtests ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), planeContractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^"+name+"$", "-test.v", "-test.timeout=30s")
	cmd.Dir = filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
	cmd.Env = testkit.EnvWithout(os.Environ(), testkit.ProxyVars)
	cmd.WaitDelay = 5 * time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\nstdout:\n%s\nstderr:\n%s", pkg, name, err, out.String(), errOut.String())
	}
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		l = strings.TrimLeft(l, " \t")
		if strings.HasPrefix(l, "--- SKIP:") || strings.HasPrefix(l, "--- FAIL:") {
			t.Fatalf("%s %s: %q\n%s", pkg, name, l, out.String())
		}
		lines = append(lines, l)
	}
	for _, n := range append([]string{name}, prefixed(name, subtests)...) {
		if !slices.Contains(lines, "=== RUN   "+n) || !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "--- PASS: "+n+" (") }) {
			t.Fatalf("%s: no RUN/PASS evidence for %s:\n%s", pkg, n, out.String())
		}
	}
}

func prefixed(parent string, subs []string) []string {
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = parent + "/" + s
	}
	return out
}

var planeFiles = []string{"config.json", "pki/ca.crt", "pki/ca.key", "pki/server.crt", "pki/server.key"}

func stateFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, rel := range planeFiles {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[rel] = b
	}
	return out
}

func sameFiles(t *testing.T, a, b map[string][]byte, except ...string) {
	t.Helper()
	for _, rel := range planeFiles {
		if !slices.Contains(except, rel) && !bytes.Equal(a[rel], b[rel]) {
			t.Fatalf("%s changed", rel)
		}
	}
}

func pemCert(t *testing.T, path string) (*x509.Certificate, []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("%s is not one PEM certificate", path)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c, blk.Bytes
}

// escapePath is the report's path escaping, written independently.
func escapePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wantReport constructs the 13-line report from the files on disk.
func wantReport(t *testing.T, root string) string {
	t.Helper()
	var cfg struct {
		SchemaVersion int    `json:"schema_version"`
		Bind          string `json:"bind"`
	}
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatalf("config.json: %v %q", err, b)
	}
	ca, caDER := pemCert(t, filepath.Join(root, "pki", "ca.crt"))
	srv, _ := pemCert(t, filepath.Join(root, "pki", "server.crt"))
	dns := slices.Clone(srv.DNSNames)
	slices.Sort(dns)
	var ips []string
	for _, ip := range srv.IPAddresses {
		a, _ := netip.AddrFromSlice(ip)
		ips = append(ips, a.Unmap().String())
	}
	slices.Sort(ips)
	sum := sha256.Sum256(caDER)
	ts := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
	lines := []string{
		"state_dir: " + escapePath(root),
		"bind: " + cfg.Bind,
		"ca_cert_path: " + escapePath(filepath.Join(root, "pki", "ca.crt")),
		"ca_key_path: " + escapePath(filepath.Join(root, "pki", "ca.key")),
		"server_cert_path: " + escapePath(filepath.Join(root, "pki", "server.crt")),
		"server_key_path: " + escapePath(filepath.Join(root, "pki", "server.key")),
		"dns_names: " + strings.Join(dns, ","),
		"ip_addresses: " + strings.Join(ips, ","),
		"ca_fingerprint: sha256:" + hex.EncodeToString(sum[:]),
		"ca_not_before: " + ts(ca.NotBefore),
		"ca_not_after: " + ts(ca.NotAfter),
		"server_not_before: " + ts(srv.NotBefore),
		"server_not_after: " + ts(srv.NotAfter),
	}
	return strings.Join(lines, "\n") + "\n"
}

// trustClient trusts exactly the plane's parsed CA (never a replacement
// fixture CA), with a bounded timeout and idle-connection cleanup.
func trustClient(t *testing.T, ca *x509.Certificate) *http.Client {
	t.Helper()
	c := (&testkit.FixtureCA{Cert: ca}).Client()
	c.Timeout = 10 * time.Second
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// namedClient verifies against ca for serverName (instead of the URL host).
func namedClient(t *testing.T, ca *x509.Certificate, serverName string) *http.Client {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: (&testkit.FixtureCA{Cert: ca}).Pool(), ServerName: serverName, MinVersion: tls.VersionTLS12}, Proxy: nil}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func health(c *http.Client, addr string) error {
	resp, err := c.Get("https://" + addr + "/api/v1/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "{\"status\":\"ok\",\"version\":1}\n" || resp.Header.Get("Content-Type") != "application/json" {
		return fmt.Errorf("health = %d %q", resp.StatusCode, b)
	}
	return nil
}

func mustCode(t *testing.T, r result, code int, stderrPrefix string) {
	t.Helper()
	if r.code != code || r.stdout != "" || !strings.HasPrefix(r.stderr, stderrPrefix) {
		t.Fatalf("got %+v, want exit %d with stderr %q and empty stdout", r, code, stderrPrefix)
	}
}

// FP-1: the four implemented plane leaves through the built binary.
func TestPlaneCommands(t *testing.T) {
	p := newPlaneCLI(t)
	leaves := [][]string{{"plane", "init"}, {"plane", "run"}, {"plane", "status"}, {"plane", "cert", "reissue"}}
	for _, leaf := range leaves {
		path := "callsheet " + strings.Join(leaf, " ")
		h := p.run(t, append(slices.Clone(leaf), "--help")...)
		if h.code != 0 || h.stderr != "" || !strings.HasPrefix(h.stdout, "Usage: "+path+" [--state-dir PATH]") || !strings.Contains(h.stdout, "\nStatus: implemented.\n") {
			t.Fatalf("%v --help = %+v", leaf, h)
		}
		for _, args := range [][]string{append(slices.Clone(leaf), "-h"), append([]string{"help"}, leaf...), append(slices.Clone(leaf), "--san", "x", "--help")} {
			if r := p.run(t, args...); r != h {
				t.Fatalf("%v = %+v", args, r)
			}
		}
	}
	// Remaining stubs keep their contract.
	for _, stub := range [][]string{{"task", "ls"}, {"sidecar", "enroll"}, {"mcp"}} {
		want := "callsheet: not_implemented: \"callsheet " + strings.Join(stub, " ") + "\" is not implemented yet\n"
		if r := p.run(t, stub...); r.code != 8 || r.stdout != "" || r.stderr != want {
			t.Fatalf("%v = %+v", stub, r)
		}
	}
	if f := listTree(t, p.home); len(f) != 0 {
		t.Fatalf("help and stubs created %v", f)
	}
	// Usage errors: exit 2, empty stdout, contract diagnostic.
	for _, args := range [][]string{
		{"plane", "init", "--bogus"}, {"plane", "init", "--state-dir", "a", "--state-dir", "b", "--san", "x"},
		{"plane", "init", "--san", "x", "operand"}, {"plane", "init", "--state-dir=", "--san", "x"}, {"plane", "--san", "x", "init"},
		{"plane", "status", "--san", "x"}, {"plane", "cert", "reissue"}, {"plane", "init", "--san", "bad name"},
		{"plane", "init", "--san"}, {"plane", "run", "--bind", "0.0.0.0:0", "--san", "x"},
	} {
		mustCode(t, p.run(t, args...), 2, "callsheet: invalid_argument: ")
	}
	if f := listTree(t, p.home); len(f) != 0 {
		t.Fatalf("invalid commands created %v", f)
	}
	// Exact report bytes for init, status and reissue, including path
	// escaping and an empty list, with --flag=value forms.
	root := filepath.Join(t.TempDir(), "a\\b\tc\nd", "state")
	r := p.run(t, "plane", "init", "--state-dir="+root, "--bind=127.0.0.1:0", "--san=127.0.0.1", "--san", "::1")
	if want := wantReport(t, root); r.code != 0 || r.stderr != "" || r.stdout != want || !strings.Contains(r.stdout, "\ndns_names: \n") ||
		!strings.Contains(r.stdout, `\\b\tc\nd`) || strings.Count(r.stdout, "\n") != 13 {
		t.Fatalf("init = %+v\nwant %q", r, want)
	}
	if s := p.run(t, "plane", "status", "--state-dir", root); s.code != 0 || s.stderr != "" || s.stdout != r.stdout {
		t.Fatalf("status = %+v", s)
	}
	if s := p.run(t, "plane", "init", "--state-dir", root); s.code != 0 || s.stdout != r.stdout {
		t.Fatalf("repeat init = %+v", s)
	}
	re := p.run(t, "plane", "cert", "reissue", "--state-dir", root, "--san", "Plane.Example.")
	if re.code != 0 || re.stderr != "" || re.stdout != wantReport(t, root) || !strings.Contains(re.stdout, "\ndns_names: plane.example\nip_addresses: \n") {
		t.Fatalf("reissue = %+v", re)
	}
	// Contract codes on the process boundary.
	mustCode(t, p.run(t, "plane", "status", "--state-dir", filepath.Join(p.home, "none")), 3, "callsheet: not_found: ")
	mustCode(t, p.run(t, "plane", "init", "--state-dir", root, "--san", "other"), 4, "callsheet: conflict: ")
	os.Chmod(filepath.Join(root, "pki", "server.key"), 0o644)
	mustCode(t, p.run(t, "plane", "status", "--state-dir", root), 6, "callsheet: trust_failed: ")
	mustCode(t, p.run(t, "plane", "run", "--state-dir", root), 6, "callsheet: trust_failed: ")
}

// FP-2: private persistent state, locking and validation. The direct CLI
// subtests are the process-boundary scenarios devcheck stress repeats; the
// delegated package contracts run in "contracts" only (iteration 02b), since
// the stress packages step already repeats them in internal/plane.
func TestPlaneState(t *testing.T) {
	p := newPlaneCLI(t)
	t.Run("paths", func(t *testing.T) {
		want := map[string]string{"linux": ".local/state/callsheet/plane", "darwin": "Library/Application Support/callsheet/plane"}[runtime.GOOS]
		if want == "" {
			t.Fatalf("unsupported host %s", runtime.GOOS)
		}
		r := p.run(t, "plane", "init", "--san", "localhost")
		root := filepath.Join(p.home, want)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "state_dir: "+root+"\n") {
			t.Fatalf("default root = %+v", r)
		}
		if s := p.run(t, "plane", "status"); s.code != 0 || s.stdout != r.stdout {
			t.Fatalf("default status = %+v", s)
		}
		explicit := filepath.Join(t.TempDir(), "explicit")
		if r := p.run(t, "plane", "init", "--state-dir", explicit, "--san", "localhost"); r.code != 0 || !strings.HasPrefix(r.stdout, "state_dir: "+explicit+"\n") {
			t.Fatalf("explicit root = %+v", r)
		}
		// The child resolves its working directory without $PWD, so compare
		// with the real path (macOS temporary directories are symlinked).
		realCwd, err := filepath.EvalSymlinks(p.cwd)
		if err != nil {
			t.Fatal(err)
		}
		if r := p.run(t, "plane", "init", "--state-dir", "rel/state", "--san", "localhost"); r.code != 0 || !strings.HasPrefix(r.stdout, "state_dir: "+filepath.Join(realCwd, "rel", "state")+"\n") {
			t.Fatalf("relative root = %+v", r)
		}
		if runtime.GOOS == "linux" {
			xdg := t.TempDir()
			saved := p.env
			p.env = append(slices.Clone(saved), "XDG_STATE_HOME="+xdg)
			if r := p.run(t, "plane", "init", "--san", "localhost"); r.code != 0 || !strings.HasPrefix(r.stdout, "state_dir: "+filepath.Join(xdg, "callsheet", "plane")+"\n") {
				t.Fatalf("XDG root = %+v", r)
			}
			p.env = append(slices.Clone(saved), "XDG_STATE_HOME=relative")
			mustCode(t, p.run(t, "plane", "status"), 2, "callsheet: invalid_argument: XDG_STATE_HOME")
			p.env = saved
		}
	})
	t.Run("persistence", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "p", "state")
		r := p.run(t, "plane", "init", "--state-dir", root, "--san", "localhost")
		if r.code != 0 {
			t.Fatalf("init = %+v", r)
		}
		if b, _ := os.ReadFile(filepath.Join(root, "config.json")); string(b) != "{\n  \"schema_version\": 1,\n  \"bind\": \"127.0.0.1:8443\"\n}\n" {
			t.Fatalf("config bytes %q", b)
		}
		for rel, mode := range map[string]fs.FileMode{".": fs.ModeDir | 0o700, "pki": fs.ModeDir | 0o700, "tmp": fs.ModeDir | 0o700, ".lock": 0o600,
			"config.json": 0o600, "pki/ca.crt": 0o600, "pki/ca.key": 0o600, "pki/server.crt": 0o600, "pki/server.key": 0o600} {
			if fi, err := os.Lstat(filepath.Join(root, rel)); err != nil || fi.Mode() != mode {
				t.Fatalf("%s mode %v (%v), want %v", rel, fi.Mode(), err, mode)
			}
		}
		// A copy of the stopped root is a complete backup.
		backup := filepath.Join(t.TempDir(), "backup")
		copyDir(t, root, backup)
		b := p.run(t, "plane", "init", "--state-dir", backup)
		if b.code != 0 || b.stdout != strings.ReplaceAll(r.stdout, root, backup) {
			t.Fatalf("restored backup = %+v", b)
		}
		sameFiles(t, stateFiles(t, root), stateFiles(t, backup))
	})
	t.Run("locking", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if r := p.run(t, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "127.0.0.1"); r.code != 0 {
			t.Fatalf("init = %+v", r)
		}
		before := stateFiles(t, root)
		pp := p.start(t, "--state-dir", root)
		mustCode(t, p.run(t, "plane", "init", "--state-dir", root), 4, "callsheet: conflict: plane state "+root+" is in use")
		mustCode(t, p.run(t, "plane", "cert", "reissue", "--state-dir", root, "--san", "x"), 4, "callsheet: conflict: ")
		mustCode(t, p.run(t, "plane", "run", "--state-dir", root), 4, "callsheet: conflict: ")
		if s := p.run(t, "plane", "status", "--state-dir", root); s.code != 0 {
			t.Fatalf("status while running = %+v", s)
		}
		sameFiles(t, before, stateFiles(t, root))
		if code := pp.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("run exit %d", code)
		}
		if r := p.run(t, "plane", "cert", "reissue", "--state-dir", root, "--san", "127.0.0.1"); r.code != 0 {
			t.Fatalf("reissue after release = %+v", r)
		}
	})
	t.Run("validation", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if r := p.run(t, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "localhost"); r.code != 0 {
			t.Fatalf("init = %+v", r)
		}
		full := stateFiles(t, root)
		key := filepath.Join(root, "pki", "server.key")
		aside := filepath.Join(t.TempDir(), "server.key") // parked outside the root
		os.Rename(key, aside)
		for _, args := range [][]string{
			{"plane", "init", "--state-dir", root}, {"plane", "run", "--state-dir", root},
			{"plane", "status", "--state-dir", root}, {"plane", "cert", "reissue", "--state-dir", root, "--san", "x"},
		} {
			r := p.run(t, args...)
			mustCode(t, r, 4, "callsheet: conflict: plane state in "+root+" is incomplete (missing "+key+")")
		}
		sameFiles(t, full, stateFiles(t, root), "pki/server.key")
		os.Rename(aside, key)
		// Corruption is refused and never regenerated.
		os.WriteFile(filepath.Join(root, "pki", "ca.crt"), []byte("corrupt"), 0o600)
		mustCode(t, p.run(t, "plane", "init", "--state-dir", root, "--san", "localhost"), 6, "callsheet: trust_failed: invalid certificate ")
		mustCode(t, p.run(t, "plane", "run", "--state-dir", root), 6, "callsheet: trust_failed: ")
		if b, _ := os.ReadFile(filepath.Join(root, "pki", "ca.crt")); string(b) != "corrupt" {
			t.Fatal("corrupt CA was replaced")
		}
		// A root that already holds an unrelated entry is not empty state:
		// init refuses and publishes nothing beside it.
		occupied := filepath.Join(t.TempDir(), "occupied")
		os.Mkdir(occupied, 0o700)
		os.WriteFile(filepath.Join(occupied, "unrelated.txt"), []byte("x"), 0o600)
		mustCode(t, p.run(t, "plane", "init", "--state-dir", occupied, "--san", "localhost"), 4, "callsheet: conflict: state directory "+occupied+" holds unexpected "+filepath.Join(occupied, "unrelated.txt"))
		if got := listTree(t, occupied); !slices.Equal(got, []string{"unrelated.txt"}) {
			t.Fatalf("occupied root now holds %v", got)
		}
	})
	t.Run("contracts", func(t *testing.T) {
		contracts := testkit.BuildTestBinary(t, "./internal/plane", "plane-contract")
		planeContract(t, contracts, "./internal/plane", "TestNativeStateContract", "modes", "no-replace", "rename", "flock")
		planeContract(t, contracts, "./internal/plane", "TestStateFailureContract", "create", "write", "sync", "close", "publish", "partial", "corrupt")
	})
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		info, err := e.Info()
		if err != nil {
			return err
		}
		if e.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), info.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// absentPrivateAddr returns a private IPv4 address not assigned locally.
func absentPrivateAddr(t *testing.T) string {
	t.Helper()
	as, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("host enumeration: %v", err)
	}
	for _, cand := range []string{"10.255.254.253", "172.31.254.253", "192.168.254.253", "100.127.254.253"} {
		present := false
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(net.ParseIP(cand)) {
				present = true
			}
		}
		if !present {
			return cand
		}
	}
	t.Fatal("every candidate private address is assigned on this host")
	return ""
}

// FP-3: loopback default and the explicit private address policy.
func TestPlaneBind(t *testing.T) {
	p := newPlaneCLI(t)
	// The default bind is recorded without opening any listener (init
	// never listens), so no test touches the fixed default port.
	root := filepath.Join(t.TempDir(), "default")
	if r := p.run(t, "plane", "init", "--state-dir", root, "--san", "localhost"); r.code != 0 || !strings.Contains(r.stdout, "\nbind: 127.0.0.1:8443\n") {
		t.Fatalf("default init = %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "config.json")); !strings.Contains(string(b), `"bind": "127.0.0.1:8443"`) {
		t.Fatalf("config %q", b)
	}
	// Serving uses an explicit OS-assigned loopback port in a separate root.
	served := filepath.Join(t.TempDir(), "served")
	pp := p.start(t, "--state-dir", served, "--bind", "127.0.0.1:0", "--san", "127.0.0.1")
	if host, port, _ := net.SplitHostPort(pp.addr); host != "127.0.0.1" || port == "0" {
		t.Fatalf("listening on %s", pp.addr)
	}
	ca, _ := pemCert(t, filepath.Join(served, "pki", "ca.crt"))
	if err := health(trustClient(t, ca), pp.addr); err != nil {
		t.Fatal(err)
	}
	if s := p.run(t, "plane", "status", "--state-dir", served); !strings.Contains(s.stdout, "\nbind: 127.0.0.1:0\n") {
		t.Fatalf("status = %+v", s)
	}
	if code := pp.stop(t, syscall.SIGTERM); code != 130 {
		t.Fatalf("exit %d", code)
	}
	// Public, wildcard, other-loopback, link-local and DNS binds are
	// rejected before state or listener creation; an absent private
	// address is unavailable.
	for _, c := range []struct {
		bind string
		code int
	}{
		{"0.0.0.0:0", 2}, {"[::]:0", 2}, {"8.8.8.8:0", 2}, {"127.0.0.2:0", 2}, {"169.254.1.1:0", 2}, {"localhost:0", 2},
		{"[2001:db8::1]:0", 2}, {absentPrivateAddr(t) + ":0", 5},
	} {
		for _, leaf := range []string{"init", "run"} {
			fresh := filepath.Join(t.TempDir(), "state")
			r := p.run(t, "plane", leaf, "--state-dir", fresh, "--bind", c.bind, "--san", "localhost")
			if r.code != c.code || r.stdout != "" || strings.Contains(r.stderr, "listening") {
				t.Fatalf("%s --bind %s = %+v", leaf, c.bind, r)
			}
			if _, err := os.Lstat(fresh); !os.IsNotExist(err) {
				t.Fatalf("%s --bind %s created state", leaf, c.bind)
			}
		}
	}
	bin := testkit.BuildTestBinary(t, "./internal/plane", "plane-contract")
	planeContract(t, bin, "./internal/plane", "TestInterfacePolicyContract", "linux", "darwin", "native-adapter")
}

// listeningRecords returns the listening log records.
func listeningRecords(logs string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(logs, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "listening" {
			out = append(out, rec)
		}
	}
	return out
}

// FP-4: first-start trust, the CA fingerprint and restart invariance.
func TestPlaneInit(t *testing.T) {
	p := newPlaneCLI(t)
	root := filepath.Join(t.TempDir(), "state")
	var firstFP string
	t.Run("issuance", func(t *testing.T) {
		pp := p.start(t, "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "localhost", "--san", "127.0.0.1")
		ca, caDER := pemCert(t, filepath.Join(root, "pki", "ca.crt"))
		srv, _ := pemCert(t, filepath.Join(root, "pki", "server.crt"))
		if !ca.IsCA || !ca.BasicConstraintsValid || ca.MaxPathLen != 0 || ca.Subject.CommonName != "Callsheet internal CA" ||
			srv.IsCA || srv.Subject.CommonName != "Callsheet plane" || !slices.Equal(srv.DNSNames, []string{"localhost"}) {
			t.Fatalf("credentials: CA %+v server %+v", ca.Subject, srv.Subject)
		}
		if _, err := srv.Verify(x509.VerifyOptions{Roots: (&testkit.FixtureCA{Cert: ca}).Pool(), DNSName: "localhost", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Fatalf("server does not verify: %v", err)
		}
		if err := health(trustClient(t, ca), pp.addr); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(caDER)
		firstFP = "sha256:" + hex.EncodeToString(sum[:])
		if code := pp.stop(t, os.Interrupt); code != 130 || pp.stdout.Len() != 0 {
			t.Fatalf("exit %d stdout %q", code, pp.stdout.String())
		}
		if logs := pp.logs.String(); !strings.Contains(logs, `"msg":"initialized"`) || !strings.Contains(logs, firstFP) || strings.Contains(logs, "PRIVATE KEY") {
			t.Fatalf("bootstrap logs %s", logs)
		}
		bin := testkit.BuildTestBinary(t, "./internal/plane", "plane-contract")
		planeContract(t, bin, "./internal/plane", "TestIssuanceContract", "calendar", "entropy", "san-validation")
	})
	t.Run("fingerprint", func(t *testing.T) {
		r := p.run(t, "plane", "init", "--state-dir", root)
		if r.code != 0 || r.stderr != "" || r.stdout != wantReport(t, root) || !strings.Contains(r.stdout, "\nca_fingerprint: "+firstFP+"\n") {
			t.Fatalf("report = %+v, want fingerprint %s", r, firstFP)
		}
		fresh := filepath.Join(t.TempDir(), "fresh")
		r = p.run(t, "plane", "init", "--state-dir", fresh, "--san", "plane.example")
		if r.code != 0 || r.stdout != wantReport(t, fresh) || strings.Contains(r.stdout, firstFP) {
			t.Fatalf("fresh init = %+v", r)
		}
	})
	t.Run("restart-invariance", func(t *testing.T) {
		snap := stateFiles(t, root)
		for _, b := range snap {
			if b == nil {
				t.Fatal("first start left no complete state")
			}
		}
		pp := p.start(t, "--state-dir", root)
		recs := listeningRecords(pp.logs.String())
		if len(recs) != 1 || recs[0]["ca_fingerprint"] != firstFP || strings.Contains(pp.logs.String(), `"msg":"initialized"`) {
			t.Fatalf("second start logs %s", pp.logs.String())
		}
		if code := pp.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("exit %d", code)
		}
		sameFiles(t, snap, stateFiles(t, root)) // AC-TLS-1
		if r := p.run(t, "plane", "init", "--state-dir", root, "--san", "127.0.0.1", "--san", "LOCALHOST"); r.code != 0 || !strings.Contains(r.stdout, firstFP) {
			t.Fatalf("repeat init = %+v", r)
		}
		mustCode(t, p.run(t, "plane", "init", "--state-dir", root, "--san", "localhost"), 4, "callsheet: conflict: ")
		mustCode(t, p.run(t, "plane", "run", "--state-dir", root, "--san", "other"), 4, "callsheet: conflict: ")
		sameFiles(t, snap, stateFiles(t, root))
	})
}

// FP-5: HTTPS only, pre-listen validation and bounded shutdown. The
// delegated failure contract runs in "contracts" only (iteration 02b).
func TestPlaneTLS(t *testing.T) {
	p := newPlaneCLI(t)
	root := filepath.Join(t.TempDir(), "state")
	if r := p.run(t, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "localhost", "--san", "127.0.0.1"); r.code != 0 {
		t.Fatalf("init = %+v", r)
	}
	ca, _ := pemCert(t, filepath.Join(root, "pki", "ca.crt"))
	t.Run("https-only", func(t *testing.T) {
		pp := p.start(t, "--state-dir", root)
		if err := health(trustClient(t, ca), pp.addr); err != nil {
			t.Fatal(err)
		}
		if err := health(namedClient(t, ca, "localhost"), pp.addr); err != nil {
			t.Fatal(err)
		}
		wrong, err := testkit.NewFixtureCA()
		if err != nil {
			t.Fatal(err)
		}
		noCA := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, Proxy: nil}, Timeout: 10 * time.Second}
		for name, c := range map[string]*http.Client{"wrong CA": trustClient(t, wrong.Cert), "no CA": noCA, "SAN mismatch": namedClient(t, ca, "other.example")} {
			if err := health(c, pp.addr); err == nil || !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("%s: %v", name, err)
			}
		}
		noCA.CloseIdleConnections()
		// Plaintext never reaches a handler: at most net/http's standard
		// plaintext rejection, never the health body (AC-TLS-2).
		conn, err := net.DialTimeout("tcp", pp.addr, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(conn, "GET /api/v1/health HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
		reply, _ := io.ReadAll(bufio.NewReader(conn))
		conn.Close()
		if strings.Contains(string(reply), `"status":`) || (len(reply) > 0 && !strings.HasPrefix(string(reply), "HTTP/1.0 400 Bad Request")) {
			t.Fatalf("plaintext reply %q", reply)
		}
		plain := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
		if resp, err := plain.Get("http://" + pp.addr + "/api/v1/health"); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 400 || strings.Contains(string(b), `"status":`) {
				t.Fatalf("plaintext client = %d %q", resp.StatusCode, b)
			}
		}
		plain.CloseIdleConnections()
		if code := pp.stop(t, syscall.SIGTERM); code != 130 || len(listeningRecords(pp.logs.String())) != 1 || pp.stdout.Len() != 0 {
			t.Fatalf("exit %d, logs %s", code, pp.logs.String())
		}
	})
	t.Run("prelisten-validation", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "other")
		if r := p.run(t, "plane", "init", "--state-dir", other, "--san", "localhost"); r.code != 0 {
			t.Fatal(r)
		}
		key, srvCrt := filepath.Join(root, "pki", "server.key"), filepath.Join(root, "pki", "server.crt")
		good := stateFiles(t, root)
		for _, c := range []struct {
			name   string
			mutate func()
			msg    string
		}{
			{"key mode", func() { os.Chmod(key, 0o640) }, "has mode 0640"},
			{"dir mode", func() { os.Chmod(filepath.Join(root, "pki"), 0o755) }, "has mode 0755"},
			{"foreign key", func() { b, _ := os.ReadFile(filepath.Join(other, "pki", "server.key")); os.WriteFile(key, b, 0o600) }, "server.key does not match"},
			{"foreign certificate", func() { b, _ := os.ReadFile(filepath.Join(other, "pki", "server.crt")); os.WriteFile(srvCrt, b, 0o600) }, "not issued by ca.crt"},
			{"symlinked certificate", func() { os.Remove(srvCrt); os.Symlink(filepath.Join(other, "pki", "server.crt"), srvCrt) }, "symbolic link"},
		} {
			c.mutate()
			r := p.run(t, "plane", "run", "--state-dir", root)
			if r.code != 6 || r.stdout != "" || !strings.Contains(r.stderr, "callsheet: trust_failed: ") || !strings.Contains(r.stderr, c.msg) || strings.Contains(r.stderr, "listening") {
				t.Fatalf("%s: %+v", c.name, r)
			}
			os.Chmod(filepath.Join(root, "pki"), 0o700)
			os.Remove(srvCrt)
			for rel, b := range good {
				os.WriteFile(filepath.Join(root, rel), b, 0o600)
			}
			os.Chmod(key, 0o600)
		}
		sameFiles(t, good, stateFiles(t, root))
	})
	t.Run("bounded-shutdown", func(t *testing.T) {
		for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
			pp := p.start(t, "--state-dir", root)
			start := time.Now()
			if code := pp.stop(t, sig); code != 130 {
				t.Fatalf("%v: exit %d", sig, code)
			}
			if el := time.Since(start); el > 10*time.Second {
				t.Fatalf("%v: shutdown took %v", sig, el)
			}
			if logs := pp.logs.String(); !strings.HasSuffix(logs, "callsheet: interrupted\n") {
				t.Fatalf("%v: logs %s", sig, logs)
			}
			// The released assigned port can be bound again, and the state
			// lock is free.
			ln, err := net.Listen("tcp", pp.addr)
			if err != nil {
				t.Fatalf("%v: port %s not released: %v", sig, pp.addr, err)
			}
			ln.Close()
			if r := p.run(t, "plane", "init", "--state-dir", root); r.code != 0 {
				t.Fatalf("%v: lock not released: %+v", sig, r)
			}
		}
	})
	t.Run("contracts", func(t *testing.T) {
		bin := testkit.BuildTestBinary(t, "./internal/plane", "plane-contract")
		planeContract(t, bin, "./internal/plane", "TestServerFailureContract", "listen", "serve", "shutdown-deadline")
	})
}

// FP-6: same-CA reissue keeps clients that retain the original CA working.
// "process" is the real CLI scenario devcheck stress repeats; the delegated
// failure contract runs in "contracts" only (iteration 02b). The parent
// builds the CLI for "process"; "contracts" needs only the contract binary.
func TestPlaneReissue(t *testing.T) {
	p := newPlaneCLI(t)
	t.Run("process", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if r := p.run(t, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "localhost", "--san", "127.0.0.1"); r.code != 0 {
			t.Fatalf("init = %+v", r)
		}
		ca, _ := pemCert(t, filepath.Join(root, "pki", "ca.crt")) // retained by the client throughout
		client := trustClient(t, ca)
		pp := p.start(t, "--state-dir", root)
		if err := health(client, pp.addr); err != nil {
			t.Fatal(err)
		}
		before := stateFiles(t, root)
		mustCode(t, p.run(t, "plane", "cert", "reissue", "--state-dir", root, "--san", "plane.example", "--san", "127.0.0.1"), 4, "callsheet: conflict: ")
		sameFiles(t, before, stateFiles(t, root))
		if code := pp.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("exit %d", code)
		}
		r := p.run(t, "plane", "cert", "reissue", "--state-dir", root, "--san", "plane.example", "--san", "127.0.0.1")
		if r.code != 0 || r.stderr != "" || r.stdout != wantReport(t, root) || !strings.Contains(r.stdout, "\ndns_names: plane.example\nip_addresses: 127.0.0.1\n") {
			t.Fatalf("reissue = %+v", r)
		}
		after := stateFiles(t, root)
		sameFiles(t, before, after, "pki/server.crt")
		if bytes.Equal(before["pki/server.crt"], after["pki/server.crt"]) {
			t.Fatal("server.crt unchanged")
		}
		srv, _ := pemCert(t, filepath.Join(root, "pki", "server.crt"))
		if _, err := srv.Verify(x509.VerifyOptions{Roots: (&testkit.FixtureCA{Cert: ca}).Pool(), DNSName: "plane.example"}); err != nil {
			t.Fatalf("new certificate does not verify against the unchanged CA: %v", err)
		}
		// Restart; rediscover the new ephemeral address. The retained CA still
		// works for the retained and new names; the removed name fails.
		pp = p.start(t, "--state-dir", root)
		if err := health(client, pp.addr); err != nil {
			t.Fatalf("retained IP SAN: %v", err)
		}
		if err := health(namedClient(t, ca, "plane.example"), pp.addr); err != nil {
			t.Fatalf("new SAN: %v", err)
		}
		if err := health(namedClient(t, ca, "localhost"), pp.addr); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("removed SAN: %v", err)
		}
		if code := pp.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("contracts", func(t *testing.T) {
		bin := testkit.BuildTestBinary(t, "./internal/plane", "plane-contract")
		planeContract(t, bin, "./internal/plane", "TestReissueFailureContract", "expired-leaf", "ca-horizon", "before-rename", "after-rename")
	})
}

// FP-7: offline inspection and expiry warnings.
func TestPlaneStatus(t *testing.T) {
	p := newPlaneCLI(t)
	t.Run("inspection", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if r := p.run(t, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "localhost", "--san", "fd00::1"); r.code != 0 {
			t.Fatalf("init = %+v", r)
		}
		// An operator-edited private bind that is not assigned locally:
		// status stays offline (no enumeration) while run is unavailable.
		cfg := "{\n  \"schema_version\": 1,\n  \"bind\": \"" + absentPrivateAddr(t) + ":8443\"\n}\n"
		os.WriteFile(filepath.Join(root, "config.json"), []byte(cfg), 0o600)
		os.Remove(filepath.Join(root, ".lock"))
		type entry struct {
			mode fs.FileMode
			mod  time.Time
		}
		listing := func() map[string]entry {
			out := map[string]entry{}
			filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
				if err == nil {
					fi, _ := e.Info()
					out[p] = entry{fi.Mode(), fi.ModTime()}
				}
				return nil
			})
			return out
		}
		before, files := listing(), stateFiles(t, root)
		r := p.run(t, "plane", "status", "--state-dir", root)
		if r.code != 0 || r.stderr != "" || r.stdout != wantReport(t, root) {
			t.Fatalf("status = %+v", r)
		}
		if fmt.Sprint(listing()) != fmt.Sprint(before) {
			t.Fatal("status wrote to the state directory")
		}
		sameFiles(t, files, stateFiles(t, root))
		for _, rel := range []string{"pki/ca.key", "pki/server.key"} {
			body := strings.Split(string(files[rel]), "\n")[1]
			if strings.Contains(r.stdout+r.stderr, body) || strings.Contains(r.stdout, "PRIVATE KEY") {
				t.Fatal("status output contains key material")
			}
		}
		mustCode(t, p.run(t, "plane", "run", "--state-dir", root), 5, "callsheet: unavailable: bind address ")
	})
	t.Run("expiry-warnings", func(t *testing.T) {
		bin := testkit.BuildTestBinary(t, "./internal/plane", "plane-contract")
		planeContract(t, bin, "./internal/plane", "TestClockContract", "ca", "server")
	})
}

// FP-8: the devcheck plans, native required names, fixtures and docs name
// the plane package and tests for both platforms. Iteration 02b narrowed
// the plane stress step to the process-boundary subtests and added the
// "process" and "contracts" boundaries to the native required names.
func TestPlanePlatform(t *testing.T) {
	const stressFn = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip)$ ./tests/function"
	const stressPlaneFn = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestPlaneState|TestPlaneTLS|TestPlaneReissue)$/^(paths|persistence|locking|validation|https-only|prelisten-validation|bounded-shutdown|process)$ ./tests/function"
	const stressPkgs = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/processgroup ./internal/spikes/gittransport ./internal/plane"
	const benchPlane = "go test ./internal/plane -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := devcheck.StressSteps(goos)
		if err != nil || len(steps) != 3 || strings.Join(steps[0].Argv, " ") != stressPkgs || strings.Join(steps[1].Argv, " ") != stressFn ||
			steps[2].Name != "stress plane function" || strings.Join(steps[2].Argv, " ") != stressPlaneFn {
			t.Fatalf("%s stress plan = %+v %v", goos, steps, err)
		}
	}
	bench := devcheck.BenchSteps()
	if len(bench) != 2 || strings.Join(bench[1].Argv, " ") != benchPlane {
		t.Fatalf("bench plan = %+v", bench)
	}
	if _, err := devcheck.NativeSteps("linux"); err == nil {
		t.Fatal("linux native plan accepted")
	}
	required := []string{"TestFP6ProcessGroups", "TestFP6ProcessGroups/cooperative", "TestFP6ProcessGroups/resistant", "TestFP6ProcessGroups/leader-exits-first",
		"TestPlaneCommands", "TestPlaneState", "TestPlaneState/paths", "TestPlaneState/persistence", "TestPlaneState/locking", "TestPlaneState/validation", "TestPlaneState/contracts",
		"TestPlaneBind", "TestPlaneInit", "TestPlaneInit/issuance", "TestPlaneInit/fingerprint", "TestPlaneInit/restart-invariance",
		"TestPlaneTLS", "TestPlaneTLS/https-only", "TestPlaneTLS/prelisten-validation", "TestPlaneTLS/bounded-shutdown", "TestPlaneTLS/contracts",
		"TestPlaneReissue", "TestPlaneReissue/process", "TestPlaneReissue/contracts", "TestPlaneStatus", "TestPlaneStatus/inspection", "TestPlaneStatus/expiry-warnings", "TestPlanePlatform"}
	if got := devcheck.NativeRequiredTests(); len(got) != 28 || !slices.Equal(got, required) {
		t.Fatalf("native required = %v", got)
	}
	// Every required plane name exists as a top-level test or mandatory
	// subtest in this package's source.
	src := string(repoFile(t, "tests/function/plane_trust_test.go"))
	for _, name := range required[4:] {
		parent, sub, isSub := strings.Cut(name, "/")
		if !strings.Contains(src, "\nfunc "+parent+"(t *testing.T) {") || (isSub && !strings.Contains(src, `t.Run("`+sub+`", func(t *testing.T) {`)) {
			t.Fatalf("%s is not defined", name)
		}
	}
	// The recorded parser fixture alone cannot qualify Darwin.
	fixture := string(repoFile(t, "internal/devcheck/testdata/cli-go-test.jsonl"))
	err := devcheck.CheckNativeResults("darwin", strings.NewReader(fixture))
	if err == nil || !strings.Contains(err.Error(), "TestPlaneStatus/expiry-warnings has no run event") {
		t.Fatalf("fixture-only evidence: %v", err)
	}
	// docs/ci.md states the executed policy.
	stress := docSection(t, "Stress checks")
	for _, cmd := range []string{stressPkgs, stressFn, stressPlaneFn} {
		if !strings.Contains(stress, "\n"+cmd+"\n") {
			t.Fatalf("Stress checks lacks %q", cmd)
		}
	}
	requireTerms(t, "Stress checks", stress, "`internal/plane`", "`TestPlaneState`", "`TestPlaneTLS`", "`TestPlaneReissue`", "`contracts`", "`process`")
	checks := docSection(t, "Checks")
	for _, name := range required {
		requireTerms(t, "Checks", checks, "`"+strings.TrimPrefix(name[strings.LastIndex(name, "/")+1:], "")+"`")
	}
	requireTerms(t, "Checks", checks, "plane trust benchmarks")
	requireTerms(t, "Platform code", docSection(t, "Platform code"), "`internal/plane/lock_unix.go`", "need no exemption")
	// Leaf commands on the host are the implemented plane commands.
	for _, leaf := range cli.NewTree(runtime.GOOS).Leaves() {
		if strings.HasPrefix(leaf.Path(), "callsheet plane ") {
			if leaf.Path() != "callsheet plane init" && leaf.Path() != "callsheet plane run" && leaf.Path() != "callsheet plane status" && leaf.Path() != "callsheet plane cert reissue" {
				t.Fatalf("unexpected plane leaf %s", leaf.Path())
			}
		}
	}
	bin := testkit.BuildTestBinary(t, "./internal/devcheck", "devcheck-contract")
	planeContract(t, bin, "./internal/devcheck", "TestPlaneVerificationPolicyContract", "linux", "darwin", "missing", "skipped", "failed")
}
