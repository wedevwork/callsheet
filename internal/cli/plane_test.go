package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/plane"
)

var planeLeaves = [][]string{{"plane", "init"}, {"plane", "run"}, {"plane", "status"}, {"plane", "cert", "reissue"}}

// isolate points HOME and XDG_STATE_HOME at a fresh directory.
func isolate(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	return home
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != dir {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestPlaneLeafHelp(t *testing.T) {
	home := isolate(t)
	for _, goos := range []string{"linux", "darwin"} {
		for _, leaf := range planeLeaves {
			path := "callsheet " + strings.Join(leaf, " ")
			code, out, errOut := exec(t, goos, append(append([]string{}, leaf...), "--help")...)
			if code != 0 || errOut != "" || !strings.HasPrefix(out, "Usage: "+path+" [--state-dir PATH]") ||
				!strings.Contains(out, "\nStatus: implemented.\n") || strings.Contains(out, "future stub") || !strings.Contains(out, "--state-dir PATH") {
				t.Fatalf("%s %v --help = %d %q %q", goos, leaf, code, out, errOut)
			}
			// Help precedence: -h, "help <path>" and --help among other
			// flags print the same help and never touch state.
			for _, args := range [][]string{
				append(append([]string{}, leaf...), "-h"),
				append([]string{"help"}, leaf...),
				append(append([]string{}, leaf...), "--san", "x", "--state-dir", home+"/s", "--help"),
				append(append([]string{}, leaf...), "--bogus", "-h"),
			} {
				if c, o, e := exec(t, goos, args...); c != 0 || o != out || e != "" {
					t.Fatalf("%v = %d %q %q", args, c, o, e)
				}
			}
		}
		code, out, _ := exec(t, goos, "plane")
		for _, n := range []string{"init", "run", "status"} {
			if code != 0 || !regexpLine(out, n, "[implemented]") {
				t.Fatalf("plane group help: %q", out)
			}
		}
		if !regexpLine(out, "cert", "[group]") {
			t.Fatalf("plane group help: %q", out)
		}
		if _, out, _ := exec(t, goos, "plane", "cert"); !regexpLine(out, "reissue", "[implemented]") {
			t.Fatalf("cert group help: %q", out)
		}
		_, root, _ := exec(t, goos)
		if strings.Contains(root, "All commands except version are future stubs") ||
			!strings.Contains(root, "Commands marked [implemented] work in this build; [future stub] commands are reserved and exit 8.") {
			t.Fatalf("root help: %q", root)
		}
	}
	if f := files(t, home); len(f) != 0 {
		t.Fatalf("help created %v", f)
	}
}

func regexpLine(help, name, status string) bool {
	for _, line := range strings.Split(help, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == name && f[len(f)-1] == status {
			return true
		}
	}
	return false
}

func TestPlaneFlagErrors(t *testing.T) {
	home := isolate(t)
	d := home + "/s"
	for _, c := range []struct {
		args []string
		msg  string
		help bool
	}{
		{[]string{"plane", "init", "--bogus"}, "callsheet plane init: flag provided but not defined: -bogus", true},
		{[]string{"plane", "init", "-help"}, "callsheet plane init: flag: help requested", true},
		{[]string{"plane", "init", "--state-dir", d, "--state-dir", d, "--san", "x"}, "may be given only once", true},
		{[]string{"plane", "run", "--bind=127.0.0.1:1", "--bind", "127.0.0.1:2", "--san", "x"}, "may be given only once", true},
		{[]string{"plane", "init", "--san"}, "flag needs an argument: -san", true},
		{[]string{"plane", "init", "--san", "x", "extra"}, `unexpected argument "extra" for "callsheet plane init"`, true},
		{[]string{"plane", "init", "--san", "x", "--", "--san", "y"}, `unexpected argument "--san"`, true},
		{[]string{"plane", "init", "--state-dir=", "--san", "x"}, "--state-dir must not be empty", true},
		{[]string{"plane", "status", "--san", "x"}, "flag provided but not defined: -san", true},
		{[]string{"plane", "status", "--bind", "127.0.0.1:1"}, "flag provided but not defined: -bind", true},
		{[]string{"plane", "cert", "reissue", "--bind", "127.0.0.1:1", "--san", "x"}, "flag provided but not defined: -bind", true},
		{[]string{"plane", "cert", "reissue", "--state-dir", d}, "requires the complete replacement list", true},
		{[]string{"plane", "--state-dir", d, "init"}, `unknown flag "--state-dir" for "callsheet plane"`, true},
		{[]string{"plane", "init", "--san", "bad name"}, `invalid --san "bad name"`, false},
		{[]string{"plane", "init", "--bind", "0.0.0.0:8443", "--san", "x"}, "wildcard", false},
		{[]string{"plane", "run", "--bind", "localhost:8443", "--san", "x"}, "not a literal IP", false},
		{[]string{"plane", "init"}, "new state requires at least one --san", false},
		{[]string{"plane", "run"}, "new state requires at least one --san", false},
	} {
		for _, goos := range []string{"linux", "darwin"} {
			code, out, errOut := exec(t, goos, c.args...)
			if code != 2 || out != "" || !strings.HasPrefix(errOut, "callsheet: invalid_argument: ") || !strings.Contains(errOut, c.msg) ||
				strings.Contains(errOut, "Usage: ") != c.help {
				t.Fatalf("%s %v = %d %q %q", goos, c.args, code, out, errOut)
			}
		}
	}
	if f := files(t, home); len(f) != 0 {
		t.Fatalf("invalid commands created %v", f)
	}
}

// expectedReport builds the 13-line report independently of the plane
// package, from the files on disk.
func expectedReport(t *testing.T, root string) string {
	t.Helper()
	var cfg map[string]any
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatalf("config: %v", err)
	}
	parse := func(name string) (*x509.Certificate, []byte) {
		b, err := os.ReadFile(filepath.Join(root, "pki", name))
		if err != nil {
			t.Fatal(err)
		}
		blk, _ := pem.Decode(b)
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c, blk.Bytes
	}
	ca, caDER := parse("ca.crt")
	srv, _ := parse("server.crt")
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
	return "state_dir: " + root + "\n" +
		"bind: " + cfg["bind"].(string) + "\n" +
		"ca_cert_path: " + root + "/pki/ca.crt\n" +
		"ca_key_path: " + root + "/pki/ca.key\n" +
		"server_cert_path: " + root + "/pki/server.crt\n" +
		"server_key_path: " + root + "/pki/server.key\n" +
		"dns_names: " + strings.Join(dns, ",") + "\n" +
		"ip_addresses: " + strings.Join(ips, ",") + "\n" +
		"ca_fingerprint: sha256:" + hex.EncodeToString(sum[:]) + "\n" +
		"ca_not_before: " + ts(ca.NotBefore) + "\n" +
		"ca_not_after: " + ts(ca.NotAfter) + "\n" +
		"server_not_before: " + ts(srv.NotBefore) + "\n" +
		"server_not_after: " + ts(srv.NotAfter) + "\n"
}

func TestPlaneCommandsLifecycle(t *testing.T) {
	isolate(t)
	for _, goos := range []string{"linux", "darwin"} {
		root := filepath.Join(t.TempDir(), "state")
		code, out, errOut := exec(t, goos, "plane", "init", "--state-dir="+root, "--bind", "127.0.0.1:0", "--san", "B.example.", "--san", "10.0.0.1", "--san", "a.example")
		want := expectedReport(t, root)
		if code != 0 || errOut != "" || out != want || strings.Count(out, "\n") != 13 {
			t.Fatalf("%s init = %d %q %q\nwant %q", goos, code, out, errOut, want)
		}
		for _, args := range [][]string{
			{"plane", "status", "--state-dir", root},
			{"plane", "init", "--state-dir", root},
			{"plane", "init", "--state-dir", root, "--san", "a.example", "--san", "b.example", "--san", "10.0.0.1", "--bind", "127.0.0.1:0"},
		} {
			if c, o, e := exec(t, goos, args...); c != 0 || o != want || e != "" {
				t.Fatalf("%s %v = %d %q %q", goos, args, c, o, e)
			}
		}
		for _, c := range []struct {
			args []string
			code int
			msg  string
		}{
			{[]string{"plane", "init", "--state-dir", root, "--san", "a.example"}, 4, "callsheet: conflict: --san list"},
			{[]string{"plane", "init", "--state-dir", root, "--bind", "127.0.0.1:8443"}, 4, "callsheet: conflict: --bind"},
			{[]string{"plane", "status", "--state-dir", root + "/missing"}, 3, "callsheet: not_found: "},
			{[]string{"plane", "cert", "reissue", "--state-dir", root + "/missing", "--san", "x"}, 3, "callsheet: not_found: "},
		} {
			code, out, errOut := exec(t, goos, c.args...)
			if code != c.code || out != "" || !strings.HasPrefix(errOut, c.msg) || strings.Count(errOut, "\n") != 1 {
				t.Fatalf("%s %v = %d %q %q", goos, c.args, code, out, errOut)
			}
		}
		code, out, errOut = exec(t, goos, "plane", "cert", "reissue", "--state-dir", root, "--san", "c.example")
		if code != 0 || errOut != "" || out != expectedReport(t, root) || !strings.Contains(out, "dns_names: c.example\nip_addresses: \n") {
			t.Fatalf("%s reissue = %d %q %q", goos, code, out, errOut)
		}
		os.Chmod(filepath.Join(root, "pki", "ca.key"), 0o644)
		code, out, errOut = exec(t, goos, "plane", "status", "--state-dir", root)
		if code != 6 || out != "" || !strings.HasPrefix(errOut, "callsheet: trust_failed: private key ") {
			t.Fatalf("%s unsafe key = %d %q %q", goos, code, out, errOut)
		}
		os.Chmod(filepath.Join(root, "pki", "ca.key"), 0o600)
		os.Remove(filepath.Join(root, "config.json"))
		code, _, errOut = exec(t, goos, "plane", "status", "--state-dir", root)
		if code != 4 || !strings.HasPrefix(errOut, "callsheet: conflict: plane state in ") {
			t.Fatalf("%s partial = %d %q", goos, code, errOut)
		}
	}
}

func TestPlaneDefaultStateDir(t *testing.T) {
	for goos, rel := range map[string]string{"linux": ".local/state/callsheet/plane", "darwin": "Library/Application Support/callsheet/plane"} {
		home := isolate(t)
		t.Setenv("XDG_STATE_HOME", "relative")
		code, out, errOut := exec(t, goos, "plane", "init", "--san", "localhost")
		if goos == "linux" {
			if code != 2 || !strings.Contains(errOut, "XDG_STATE_HOME") {
				t.Fatalf("relative XDG = %d %q", code, errOut)
			}
			t.Setenv("XDG_STATE_HOME", "")
			code, out, errOut = exec(t, goos, "plane", "init", "--san", "localhost")
		}
		root := filepath.Join(home, rel)
		if code != 0 || errOut != "" || !strings.HasPrefix(out, "state_dir: "+root+"\nbind: 127.0.0.1:8443\n") {
			t.Fatalf("%s default = %d %q %q", goos, code, out, errOut)
		}
		if b, _ := os.ReadFile(filepath.Join(root, "config.json")); string(b) != "{\n  \"schema_version\": 1,\n  \"bind\": \"127.0.0.1:8443\"\n}\n" {
			t.Fatalf("config %q", b)
		}
		if goos == "linux" {
			xdg := t.TempDir()
			t.Setenv("XDG_STATE_HOME", xdg)
			if c, o, _ := exec(t, goos, "plane", "status"); c != 3 || o != "" {
				t.Fatalf("XDG status = %d", c)
			}
			if c, _, _ := exec(t, goos, "plane", "init", "--san", "x"); c != 0 {
				t.Fatal("XDG init")
			}
			if _, err := os.Stat(filepath.Join(xdg, "callsheet", "plane", "config.json")); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("HOME", "")
		t.Setenv("XDG_STATE_HOME", "")
		if c, _, e := exec(t, goos, "plane", "status"); c != 2 || !strings.Contains(e, "HOME") {
			t.Fatalf("%s no HOME = %d %q", goos, c, e)
		}
	}
	// A relative --state-dir resolves against the working directory.
	isolate(t)
	t.Chdir(t.TempDir())
	cwd, _ := os.Getwd()
	if c, o, _ := exec(t, "linux", "plane", "init", "--state-dir", "rel/s", "--san", "x"); c != 0 || !strings.HasPrefix(o, "state_dir: "+filepath.Join(cwd, "rel", "s")+"\n") {
		t.Fatalf("relative = %d %q", c, o)
	}
}

// syncBuffer is a concurrency-safe writer that signals when want appears.
type syncBuffer struct {
	mu   sync.Mutex
	b    bytes.Buffer
	want string
	seen chan struct{}
	once sync.Once
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.b.Write(p)
	if s.want != "" && strings.Contains(s.b.String(), s.want) {
		s.once.Do(func() { close(s.seen) })
	}
	return n, err
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPlaneRunCommand(t *testing.T) {
	isolate(t)
	for _, goos := range []string{"linux", "darwin"} {
		root := filepath.Join(t.TempDir(), "state")
		ctx, cancel := context.WithCancel(context.Background())
		var out bytes.Buffer
		errOut := &syncBuffer{want: `"msg":"listening"`, seen: make(chan struct{})}
		done := make(chan int, 1)
		go func() {
			done <- run(ctx, NewTree(goos), goos, []string{"plane", "run", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "localhost"}, &out, errOut)
		}()
		select {
		case <-errOut.seen:
		case code := <-done:
			cancel()
			t.Fatalf("run exited %d: %s", code, errOut.String())
		case <-time.After(20 * time.Second):
			cancel()
			t.Fatal("no listening record")
		}
		cancel()
		var code int
		select {
		case code = <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("run did not stop")
		}
		logs := errOut.String()
		if code != 130 || out.Len() != 0 || !strings.HasSuffix(logs, "callsheet: interrupted\n") {
			t.Fatalf("%s run = %d out=%q err=%q", goos, code, out.String(), logs)
		}
		var rec map[string]any
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, `"msg":"listening"`) {
				json.Unmarshal([]byte(line), &rec)
			}
		}
		st, err := plane.Inspect(context.Background(), root)
		if err != nil || rec["component"] != "plane" || rec["level"] != "INFO" || rec["ca_fingerprint"] != st.CAFingerprint ||
			!strings.HasPrefix(rec["bind"].(string), "127.0.0.1:") || !strings.Contains(logs, `"msg":"initialized"`) {
			t.Fatalf("records %q (%v)", logs, err)
		}
		// Invalid state: diagnostics only, stdout stays empty.
		os.Chmod(filepath.Join(root, "pki", "server.key"), 0o644)
		var o2, e2 bytes.Buffer
		if code := run(context.Background(), NewTree(goos), goos, []string{"plane", "run", "--state-dir", root}, &o2, &e2); code != 6 || o2.Len() != 0 ||
			!strings.HasPrefix(e2.String(), "callsheet: trust_failed: ") {
			t.Fatalf("unsafe run = %d %q %q", code, o2.String(), e2.String())
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

func TestPlaneOutputAndCancellation(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "state")
	var errOut bytes.Buffer
	c := NewTree("linux").Child("plane").Child("init")
	code := planeInit(context.Background(), "linux", c, []string{"--state-dir", root, "--san", "x"}, failWriter{}, &errOut)
	if code != 1 || !strings.HasPrefix(errOut.String(), "callsheet: internal: cannot write the report to stdout: stdout closed; the operation itself completed") {
		t.Fatalf("writer failure = %d %q", code, errOut.String())
	}
	if _, err := plane.Inspect(context.Background(), root); err != nil {
		t.Fatalf("completed init was rolled back: %v", err)
	}
	// Cancellation during an operation exits 130, for every leaf.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, leaf := range []struct {
		fn   leafFunc
		args []string
	}{
		{planeInit, []string{"--state-dir", root}},
		{planeRun, []string{"--state-dir", root}},
		{planeStatus, []string{"--state-dir", root}},
		{planeReissue, []string{"--state-dir", root, "--san", "y"}},
	} {
		var out, e bytes.Buffer
		if code := leaf.fn(ctx, "linux", c, leaf.args, &out, &e); code != 130 || out.Len() != 0 || e.String() != "callsheet: interrupted\n" {
			t.Fatalf("canceled = %d %q %q", code, out.String(), e.String())
		}
	}
	// Cancellation keeps precedence over parsing in the dispatcher.
	var out, e bytes.Buffer
	if code := run(ctx, NewTree("linux"), "linux", []string{"plane", "init", "--bogus"}, &out, &e); code != 130 {
		t.Fatalf("dispatcher cancel = %d", code)
	}
	// Unsupported systems never reach a plane leaf.
	if code := runFor(context.Background(), "windows", []string{"plane", "init", "--san", "x"}, nil, &out, &e); code != 2 {
		t.Fatalf("windows = %d", code)
	}
	var e2 bytes.Buffer
	if code := planeFail(&e2, errors.New("plain")); code != 1 || e2.String() != "callsheet: internal: plain\n" {
		t.Fatalf("non-contract error = %d %q", code, e2.String())
	}
	var e3 bytes.Buffer
	if code := planeStatus(context.Background(), "freebsd", c, nil, io.Discard, &e3); code != 2 || !strings.Contains(e3.String(), "unsupported operating system") {
		t.Fatalf("status on freebsd = %d %q", code, e3.String())
	}
}

func TestRenderStatusAndWarnings(t *testing.T) {
	ts := time.Date(2026, 9, 26, 12, 0, 0, 0, time.FixedZone("x", 3600))
	st := plane.Status{
		StateDir: "/a\\b\nc\td\re", Bind: "[fd00::1]:8443",
		CACertPath: "/p/ca.crt", CAKeyPath: "/p/ca.key", ServerCertPath: "/p/server.crt", ServerKeyPath: "/p/server.key",
		DNSNames: []string{}, IPAddresses: []string{"10.0.0.1", "fd00::1"}, CAFingerprint: "sha256:00",
		CANotBefore: ts, CANotAfter: ts, ServerNotBefore: ts, ServerNotAfter: ts,
		Warnings: []plane.Warning{{Certificate: "ca", Condition: plane.ConditionExpiresSoon, At: ts}},
	}
	want := "state_dir: /a\\\\b\\nc\\td\\re\nbind: [fd00::1]:8443\nca_cert_path: /p/ca.crt\nca_key_path: /p/ca.key\n" +
		"server_cert_path: /p/server.crt\nserver_key_path: /p/server.key\ndns_names: \nip_addresses: 10.0.0.1,fd00::1\n" +
		"ca_fingerprint: sha256:00\nca_not_before: 2026-09-26T11:00:00Z\nca_not_after: 2026-09-26T11:00:00Z\n" +
		"server_not_before: 2026-09-26T11:00:00Z\nserver_not_after: 2026-09-26T11:00:00Z\n"
	if got := RenderStatus(st); got != want {
		t.Fatalf("report\n%q\nwant\n%q", got, want)
	}
	ws := []plane.Warning{
		{Certificate: "ca", Condition: plane.ConditionNotYetValid, At: ts},
		{Certificate: "server", Condition: plane.ConditionExpired, At: ts.Add(time.Hour)},
	}
	if got := RenderWarnings(ws); got != "callsheet: warning: ca not yet valid: 2026-09-26T11:00:00Z\ncallsheet: warning: server expired: 2026-09-26T12:00:00Z\n" {
		t.Fatalf("warnings %q", got)
	}
	if RenderWarnings(nil) != "" {
		t.Fatal("empty warnings")
	}
}
