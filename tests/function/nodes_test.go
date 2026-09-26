package function

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 03 function tests: exactly one top-level TestNode* per FP, in
// FP order. They drive the built callsheet binary (built once per test
// process) against real planes and sidecars on 127.0.0.1, speak raw wire
// messages through the exported client where the protocol itself is the
// subject, and delegate clock-dependent and failure-injection mechanisms
// to named package-private contracts run by name in compiled package test
// binaries (also built once per process), requiring run/pass evidence.

// nodeCLIEnv hands the once-built CLI to delegated sidecar contracts; the
// sidecar package reads it only in its test fixture.
const nodeCLIEnv = "CALLSHEET_TEST_CLI_BINARY"

const nodeWait = 30 * time.Second

// Process-lifetime fixtures: every binary is built at most once per test
// process into fixtureRoot and removed by TestMain at teardown, never in a
// single test's TempDir, so repeated runs (-count) reuse them.
var (
	fixtureMu   sync.Mutex
	fixtureRoot string
	fixtureBins = map[string]string{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureRoot != "" {
		os.RemoveAll(fixtureRoot)
	}
	os.Exit(code)
}

func fixture(t *testing.T, key string, build func(dir string) (string, error)) string {
	t.Helper()
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if p, ok := fixtureBins[key]; ok {
		return p
	}
	if fixtureRoot == "" {
		d, err := os.MkdirTemp("", "callsheet-function-fixtures-")
		if err != nil {
			t.Fatal(err)
		}
		fixtureRoot = d
	}
	p, err := build(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixtureBins[key] = p
	return p
}

// nodeBinary is the shared callsheet binary.
func nodeBinary(t *testing.T) string {
	return fixture(t, "cli", func(dir string) (string, error) { return testkit.BuildBinaryAt(dir, "./cmd/callsheet", "callsheet") })
}

// contractBinary is pkg's compiled test binary, shared by all wrappers.
func contractBinary(t *testing.T, pkg string) string {
	return fixture(t, pkg, func(dir string) (string, error) {
		return testkit.BuildTestBinaryAt(dir, pkg, strings.ReplaceAll(strings.TrimPrefix(pkg, "./"), "/", "-"))
	})
}

// newNodeCLI is a CLI environment (isolated HOME and cwd) on the shared
// binary.
func newNodeCLI(t *testing.T) *planeCLI {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	return &planeCLI{bin: nodeBinary(t), home: home, cwd: cwd, env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}}
}

// nodePlane is a running plane with its URL, CA file and fingerprint.
type nodePlane struct {
	root, url, ca, fp string
	proc              *planeProc
}

// startNodePlane initializes and runs a plane on 127.0.0.1:0 with the
// given SANs (default 127.0.0.1).
func startNodePlane(t *testing.T, p *planeCLI, sans ...string) *nodePlane {
	t.Helper()
	if len(sans) == 0 {
		sans = []string{"127.0.0.1"}
	}
	np := &nodePlane{root: filepath.Join(t.TempDir(), "plane")}
	args := []string{"plane", "init", "--state-dir", np.root, "--bind", "127.0.0.1:0"}
	for _, s := range sans {
		args = append(args, "--san", s)
	}
	r := p.run(t, args...)
	if r.code != 0 {
		t.Fatalf("plane init = %+v", r)
	}
	for _, line := range strings.Split(r.stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "ca_fingerprint: "); ok {
			np.fp = v
		}
	}
	np.ca = filepath.Join(np.root, "pki", "ca.crt")
	np.proc = p.start(t, "--state-dir", np.root)
	np.url = "https://" + np.proc.addr
	return np
}

func (np *nodePlane) client(t *testing.T) *client.Client {
	t.Helper()
	ca, err := os.ReadFile(np.ca)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(np.url, client.Trust{CAPEM: ca})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// enroll runs sidecar enroll with --ca into state and returns the node ID.
func (np *nodePlane) enroll(t *testing.T, p *planeCLI, state string) string {
	t.Helper()
	r := p.run(t, "sidecar", "enroll", "--plane", np.url, "--ca", np.ca, "--state-dir", state)
	id, ok := strings.CutPrefix(strings.Split(r.stdout, "\n")[0], "node_id: ")
	if r.code != 0 || !ok || !contract.ValidNodeID(id) || r.stdout != "node_id: "+id+"\nplane_url: "+np.url+"\nca_fingerprint: "+np.fp+"\n" || r.stderr != "" {
		t.Fatalf("enroll = %+v", r)
	}
	return id
}

// nodeLogs collects a child's stderr and signals JSON records.
type nodeLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
	sig chan struct{}
}

func (l *nodeLogs) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.buf.Write(b)
	select {
	case l.sig <- struct{}{}:
	default:
	}
	return n, err
}

func (l *nodeLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// has reports a JSON record with msg (and the given attributes).
func (l *nodeLogs) has(msg string, attrs map[string]any) bool {
	return hasRecord(l.String(), msg, attrs)
}

// hasRecord reports whether logs hold a JSON record with msg whose
// attributes include attrs.
func hasRecord(logs, msg string, attrs map[string]any) bool {
	for _, line := range strings.Split(logs, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != msg {
			continue
		}
		ok := true
		for k, v := range attrs {
			if fmt.Sprint(rec[k]) != fmt.Sprint(v) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// sidecarProc is a running "callsheet sidecar run".
type sidecarProc struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	logs   *nodeLogs
	done   chan struct{}
	err    error
}

// startSidecar runs "sidecar run" on state and waits (bounded) for the
// record msg: the explicit readiness event.
func startSidecar(t *testing.T, p *planeCLI, state, msg string) *sidecarProc {
	t.Helper()
	s := &sidecarProc{logs: &nodeLogs{sig: make(chan struct{}, 1)}, done: make(chan struct{})}
	s.cmd = exec.Command(p.bin, "sidecar", "run", "--state-dir", state)
	s.cmd.Dir, s.cmd.Env = p.cwd, p.env
	s.cmd.Stdout, s.cmd.Stderr = &s.stdout, s.logs
	s.cmd.WaitDelay = 5 * time.Second
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { s.err = s.cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		s.cmd.Process.Kill()
		select {
		case <-s.done:
		case <-time.After(nodeWait):
			t.Errorf("sidecar %d not reaped", s.cmd.Process.Pid)
		}
	})
	s.await(t, msg)
	return s
}

func (s *sidecarProc) await(t *testing.T, msg string) {
	t.Helper()
	deadline := time.After(nodeWait)
	for !s.logs.has(msg, nil) {
		select {
		case <-s.logs.sig:
		case <-s.done:
			if !s.logs.has(msg, nil) {
				t.Fatalf("sidecar exited (%v) before %q:\n%s", s.err, msg, s.logs.String())
			}
		case <-deadline:
			// The record and the deadline may be ready together: recheck
			// before failing.
			if s.logs.has(msg, nil) {
				return
			}
			t.Fatalf("no %q from the sidecar:\n%s", msg, s.logs.String())
		}
	}
}

// stop signals the sidecar and returns its exit code (bounded).
func (s *sidecarProc) stop(t *testing.T, sig os.Signal) int {
	t.Helper()
	s.cmd.Process.Signal(sig)
	select {
	case <-s.done:
	case <-time.After(nodeWait):
		t.Fatalf("sidecar ignored %v", sig)
	}
	var ee *exec.ExitError
	if errors.As(s.err, &ee) {
		return ee.ExitCode()
	}
	return 0
}

// wait returns the exit code of a sidecar that exits by itself.
func (s *sidecarProc) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(nodeWait):
		t.Fatal("sidecar did not exit")
	}
	var ee *exec.ExitError
	if errors.As(s.err, &ee) {
		return ee.ExitCode()
	}
	return 0
}

// nodeContract runs one delegated contract (and optionally one subtest) in
// the package's shared test binary with exact run/pass evidence.
func nodeContract(t *testing.T, pkg, contractName, sub string, env []string, subs ...string) {
	t.Helper()
	run := "^" + contractName + "$"
	pass := []string{contractName}
	if sub != "" {
		run += "/^" + sub + "$"
		pass = append(pass, contractName+"/"+sub)
	}
	for _, s := range subs {
		pass = append(pass, contractName+"/"+s)
	}
	out := contractRun(t, contractBinary(t, pkg), pkg, run, env, pass...)
	for _, n := range pass {
		if !strings.Contains(out, "=== RUN   "+n+"\n") {
			t.Fatalf("%s: no RUN evidence for %s", pkg, n)
		}
	}
}

func nodeFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FP-1: explicit CA trust and the pinned bootstrap, and every rejection
// before any enrollment or configuration.
func TestNodeTrust(t *testing.T) {
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	var ids []string
	t.Run("ca", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "s")
		ids = append(ids, np.enroll(t, p, state))
		var e map[string]any
		b := nodeFile(t, filepath.Join(state, "enrollment.json"))
		if json.Unmarshal(b, &e) != nil || e["plane_url"] != np.url || e["ca_pem"] != string(nodeFile(t, np.ca)) || e["ca_fingerprint"] != np.fp {
			t.Fatalf("enrollment %s", b)
		}
		r := p.run(t, "node", "ls", "--plane", np.url, "--ca", np.ca)
		if r.code != 0 || !strings.Contains(r.stdout, "\n"+ids[0]+"\toffline\t") {
			t.Fatalf("ls = %+v", r)
		}
	})
	t.Run("pin", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "s")
		r := p.run(t, "sidecar", "enroll", "--plane", np.url, "--ca-fingerprint", np.fp, "--state-dir", state)
		id, _ := strings.CutPrefix(strings.Split(r.stdout, "\n")[0], "node_id: ")
		if r.code != 0 || r.stdout != "node_id: "+id+"\nplane_url: "+np.url+"\nca_fingerprint: "+np.fp+"\n" {
			t.Fatalf("pin enroll = %+v", r)
		}
		ids = append(ids, id)
		// The fetched CA is exactly the plane's CA, stored once.
		var e map[string]any
		json.Unmarshal(nodeFile(t, filepath.Join(state, "enrollment.json")), &e)
		if e["ca_pem"] != string(nodeFile(t, np.ca)) || e["ca_fingerprint"] != np.fp {
			t.Fatalf("stored CA differs: %v", e)
		}
		r = p.run(t, "node", "ls", "--plane", np.url, "--ca-fingerprint", np.fp)
		if r.code != 0 || !strings.Contains(r.stdout, id) {
			t.Fatalf("pin ls = %+v", r)
		}
	})
	t.Run("rejections", func(t *testing.T) {
		other := startNodePlane(t, p)
		sanPlane := startNodePlane(t, p, "localhost")
		for _, c := range []struct {
			name string
			args []string
			code int
			msg  string
		}{
			{"no trust", []string{"--plane", np.url}, 6, "callsheet: trust_failed: connection not trusted: supply --ca or --ca-fingerprint"},
			{"wrong ca", []string{"--plane", np.url, "--ca", other.ca}, 6, "callsheet: trust_failed: connection not trusted: the plane's certificate is not signed by the supplied CA"},
			{"wrong pin", []string{"--plane", np.url, "--ca-fingerprint", other.fp}, 6, "callsheet: trust_failed: connection not trusted: the plane's CA does not match --ca-fingerprint"},
			{"missing ca", []string{"--plane", np.url, "--ca", filepath.Join(t.TempDir(), "none.crt")}, 6, "callsheet: trust_failed: connection not trusted: the --ca file"},
			{"san", []string{"--plane", sanPlane.url, "--ca", sanPlane.ca}, 6, "callsheet: trust_failed: connection not trusted: the plane's certificate is not valid for 127.0.0.1"},
			{"san pin", []string{"--plane", sanPlane.url, "--ca-fingerprint", sanPlane.fp}, 6, "callsheet: trust_failed: connection not trusted: the plane's certificate is not valid for 127.0.0.1"},
			{"plaintext url", []string{"--plane", "http://" + np.proc.addr, "--ca", np.ca}, 2, "callsheet: invalid_argument: invalid --plane URL"},
		} {
			state := filepath.Join(t.TempDir(), "s")
			r := p.run(t, append([]string{"sidecar", "enroll", "--state-dir", state}, c.args...)...)
			mustCode(t, r, c.code, c.msg)
			if _, err := os.Lstat(state); !os.IsNotExist(err) {
				t.Fatalf("%s: created %v", c.name, listTree(t, state))
			}
			if c.code == 6 {
				mustCode(t, p.run(t, append([]string{"node", "ls"}, c.args...)...), 6, "callsheet: trust_failed: connection not trusted")
			}
		}
		// A plane speaking another protocol version: the response header is
		// rejected with both versions, before any body is used.
		ca, err := testkit.NewFixtureCA()
		if err != nil {
			t.Fatal(err)
		}
		caFile := filepath.Join(t.TempDir(), "fixture.crt")
		os.WriteFile(caFile, ca.CertPEM, 0o644)
		v2 := serveFixture(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(contract.ProtocolHeader, "2")
			io.WriteString(w, `{"version":2,"nodes":[]}`)
		}))
		mustCode(t, p.run(t, "node", "ls", "--plane", v2, "--ca", caFile), 7, "callsheet: protocol_mismatch: protocol version mismatch: local=1 remote=2")
		// Nothing was enrolled by any rejection.
		nodes, err := np.client(t).ListNodes(context.Background())
		if err != nil || len(nodes) != len(ids) {
			t.Fatalf("roster %v %v, want only %v", nodes, err, ids)
		}
	})
}

// serveFixture serves h over TLS with the fixture CA's 127.0.0.1 leaf and
// returns its URL.
func serveFixture(t *testing.T, ca *testkit.FixtureCA, h http.Handler) string {
	t.Helper()
	cert, err := ca.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	go func() {
		srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
		close(done)
	}()
	t.Cleanup(func() { srv.Close(); <-done })
	return "https://" + ln.Addr().String()
}

// FP-2: stable enrollment: identity persistence across restarts and
// re-enrollment, the local failure/retry contract, and real competing
// processes.
func TestNodeEnrollment(t *testing.T) {
	p := newNodeCLI(t)
	t.Run("identity", func(t *testing.T) {
		a := startNodePlane(t, p)
		state := filepath.Join(t.TempDir(), "s")
		id := a.enroll(t, p, state)
		identity := nodeFile(t, filepath.Join(state, "identity.json"))
		if string(identity) != "{\n  \"schema_version\": 1,\n  \"node_id\": \""+id+"\"\n}\n" {
			t.Fatalf("identity %q", identity)
		}
		info, _ := os.Stat(filepath.Join(state, "enrollment.json"))
		if again := a.enroll(t, p, state); again != id {
			t.Fatalf("second enrollment %s, want %s", again, id)
		}
		if info2, _ := os.Stat(filepath.Join(state, "enrollment.json")); !os.SameFile(info, info2) || !info2.ModTime().Equal(info.ModTime()) {
			t.Fatal("identical configuration rewritten")
		}
		// Restarting the sidecar keeps the identity.
		for range 2 {
			s := startSidecar(t, p, state, "heartbeat acknowledged")
			if n, err := a.client(t).ShowNode(context.Background(), id); err != nil || n.Liveness != contract.LivenessOnline {
				t.Fatalf("node = %+v %v", n, err)
			}
			if code := s.stop(t, syscall.SIGTERM); code != 130 {
				t.Fatalf("sidecar exit %d", code)
			}
		}
		// A new target keeps the identity; the old plane still knows it.
		b := startNodePlane(t, p)
		if moved := b.enroll(t, p, state); moved != id {
			t.Fatalf("re-enrolled as %s, want %s", moved, id)
		}
		if !bytes.Equal(identity, nodeFile(t, filepath.Join(state, "identity.json"))) || !strings.Contains(string(nodeFile(t, filepath.Join(state, "enrollment.json"))), b.url) {
			t.Fatal("re-enrollment changed the identity or kept the old target")
		}
		if n, err := a.client(t).ShowNode(context.Background(), id); err != nil || n.ID != id {
			t.Fatalf("old plane forgot the node: %v", err)
		}
	})
	t.Run("recovery", func(t *testing.T) {
		nodeContract(t, "./internal/sidecar", "TestEnrollmentRecoveryContract", "", os.Environ(), "faults", "pending", "ambiguous", "reenroll", "entropy")
		// On the process boundary: a pending identity (registration never
		// completed) makes run refuse with the next step, and enroll
		// completes it with the same identity.
		a := startNodePlane(t, p)
		state := filepath.Join(t.TempDir(), "s")
		os.MkdirAll(state, 0o700)
		id := "n_" + strings.Repeat("5a", 16)
		os.WriteFile(filepath.Join(state, "identity.json"), []byte("{\n  \"schema_version\": 1,\n  \"node_id\": \""+id+"\"\n}\n"), 0o600)
		mustCode(t, p.run(t, "sidecar", "run", "--state-dir", state), 3, "callsheet: not_found: node "+id)
		if got := a.enroll(t, p, state); got != id {
			t.Fatalf("pending identity replaced: %s", got)
		}
	})
	t.Run("locking", func(t *testing.T) {
		a := startNodePlane(t, p)
		state := filepath.Join(t.TempDir(), "s")
		a.enroll(t, p, state)
		s := startSidecar(t, p, state, "sidecar starting")
		mustCode(t, p.run(t, "sidecar", "enroll", "--plane", a.url, "--ca", a.ca, "--state-dir", state), 4, "callsheet: conflict: sidecar state "+state+" is in use")
		mustCode(t, p.run(t, "sidecar", "run", "--state-dir", state), 4, "callsheet: conflict: sidecar state "+state+" is in use")
		if code := s.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("sidecar exit %d", code)
		}
		a.enroll(t, p, state) // the lock was released
	})
}

// streamPeer is a raw node stream through the exported client.
type streamPeer struct {
	t *testing.T
	c *websocket.Conn
}

func dialPeer(t *testing.T, np *nodePlane) *streamPeer {
	t.Helper()
	c, err := np.client(t).DialNodeStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &streamPeer{t: t, c: c}
}

func (s *streamPeer) sendRaw(typ websocket.MessageType, b []byte) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nodeWait)
	defer cancel()
	if err := s.c.Write(ctx, typ, b); err != nil {
		s.t.Fatal(err)
	}
}

func (s *streamPeer) send(version int, typ, rid string, body any) {
	s.t.Helper()
	b, err := contract.EncodeFrame(version, typ, rid, body)
	if err != nil {
		s.t.Fatal(err)
	}
	s.sendRaw(websocket.MessageText, b)
}

// recv returns the next message's envelope and, for an error, its body.
func (s *streamPeer) recv() (contract.NodeFrame, *contract.Error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nodeWait)
	defer cancel()
	_, b, err := s.c.Read(ctx)
	if err != nil {
		s.t.Fatalf("read: %v", err)
	}
	f, err := contract.DecodeFrame(b, contract.FromPlane)
	if err != nil {
		s.t.Fatalf("%s: %v", b, err)
	}
	if f.Type == contract.FrameError {
		e, err := contract.ParseErrorBody(f.Body)
		if err != nil {
			s.t.Fatal(err)
		}
		return f, e
	}
	return f, nil
}

// closeStatus reads to the end and returns the close code.
func (s *streamPeer) closeStatus() websocket.StatusCode {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nodeWait)
	defer cancel()
	for {
		if _, _, err := s.c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				s.t.Fatal("stream not closed")
			}
			return websocket.CloseStatus(err)
		}
	}
}

// FP-3: protocol-v1 messages over real WSS: hello and version rules on both
// ends, sequence errors, no liveness before a heartbeat, and bounded
// message and blocked-peer handling.
func TestNodeProtocol(t *testing.T) {
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	id := np.enroll(t, p, filepath.Join(t.TempDir(), "s"))
	t.Run("hello", func(t *testing.T) {
		for _, c := range []struct {
			name string
			send func(s *streamPeer)
			rid  string
			code contract.Code
		}{
			{"first-not-hello", func(s *streamPeer) { s.send(1, contract.FrameHeartbeat, "b1", contract.HeartbeatBody{}) }, "b1", contract.CodeInvalidArgument},
			{"version", func(s *streamPeer) {
				s.send(2, contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "v2"})
			}, "h1", contract.CodeProtocolMismatch},
			{"unknown", func(s *streamPeer) {
				s.send(1, contract.FrameHello, "h1", contract.HelloBody{NodeID: "n_" + strings.Repeat("0", 32), SoftwareVersion: "dev"})
			}, "h1", contract.CodeNotFound},
		} {
			s := dialPeer(t, np)
			c.send(s)
			f, e := s.recv()
			if e == nil || e.Code != c.code || f.RequestID != c.rid || f.Version != 1 {
				t.Fatalf("%s: %+v %v", c.name, f, e)
			}
			if c.code == contract.CodeProtocolMismatch {
				l, _ := e.DetailInt("local_version")
				r, _ := e.DetailInt("remote_version")
				if e.Message != "protocol version mismatch: local=1 remote=2" || l != 1 || r != 2 {
					t.Fatalf("mismatch body %+v", e)
				}
			}
			if st := s.closeStatus(); st != websocket.StatusPolicyViolation {
				t.Fatalf("%s: close %v", c.name, st)
			}
		}
		if !hasRecord(np.proc.logs.String(), "protocol version mismatch", map[string]any{"local_version": 1, "remote_version": 2}) {
			t.Fatalf("plane did not log both versions:\n%s", np.proc.logs.String())
		}
		// hello_ok carries the fixed values; hello alone is not liveness; a
		// skipped counter is refused.
		s := dialPeer(t, np)
		s.send(1, contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "dev"})
		f, _ := s.recv()
		if b, err := contract.DecodeHelloOK(f.Body); err != nil || f.Type != contract.FrameHelloOK || f.RequestID != "h1" || b.LeaseMS != 15000 {
			t.Fatalf("hello_ok %+v %v", f, err)
		}
		if n, err := np.client(t).ShowNode(context.Background(), id); err != nil || n.Liveness != contract.LivenessOffline || n.LastSeen != nil {
			t.Fatalf("liveness before a heartbeat: %+v %v", n, err)
		}
		s.send(1, contract.FrameHeartbeat, "b2", contract.HeartbeatBody{})
		if _, e := s.recv(); e == nil || e.Code != contract.CodeInvalidArgument {
			t.Fatalf("skipped counter accepted: %v", e)
		}
		s.closeStatus()
		s = dialPeer(t, np)
		s.send(1, contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "dev"})
		s.recv()
		s.send(1, contract.FrameHeartbeat, "b1", contract.HeartbeatBody{})
		if f, e := s.recv(); e != nil || f.Type != contract.FrameHeartbeatAck || f.RequestID != "b1" {
			t.Fatalf("ack %+v %v", f, e)
		}
		if n, _ := np.client(t).ShowNode(context.Background(), id); n.Liveness != contract.LivenessOnline || *n.ProtocolVersion != 1 {
			t.Fatalf("after heartbeat %+v", n)
		}
		s.c.Close(websocket.StatusNormalClosure, "")
		// The sidecar end: a plane speaking version 2 is a terminal
		// mismatch naming both versions.
		ca, err := testkit.NewFixtureCA()
		if err != nil {
			t.Fatal(err)
		}
		v2 := serveFixture(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), nodeWait)
			defer cancel()
			ws.Read(ctx)
			b, _ := contract.EncodeFrame(2, contract.FrameError, "h1", contract.VersionMismatch(2, 1))
			ws.Write(ctx, websocket.MessageText, b)
			ws.Read(ctx)
		}))
		state := filepath.Join(t.TempDir(), "v2")
		writeSidecarState(t, state, "n_"+strings.Repeat("3c", 16), v2, ca.CertPEM)
		sc := startSidecar(t, p, state, "sidecar starting")
		if code := sc.wait(t); code != 7 || !strings.Contains(sc.logs.String(), "callsheet: protocol_mismatch: protocol version mismatch: local=1 remote=2\n") ||
			!sc.logs.has("connection failed permanently", map[string]any{"local_version": 1, "remote_version": 2}) {
			t.Fatalf("sidecar against v2 = %d\n%s", code, sc.logs.String())
		}
	})
	t.Run("limits", func(t *testing.T) {
		s := dialPeer(t, np)
		s.sendRaw(websocket.MessageText, bytes.Repeat([]byte(" "), contract.MaxFrameBytes+1))
		if st := s.closeStatus(); st != websocket.StatusMessageTooBig {
			t.Fatalf("oversized close %v", st)
		}
		s = dialPeer(t, np)
		s.sendRaw(websocket.MessageBinary, []byte{1})
		if st := s.closeStatus(); st != websocket.StatusUnsupportedData {
			t.Fatalf("binary close %v", st)
		}
		// A peer that never reads is closed within the shutdown bound.
		blocked := dialPeer(t, np)
		blocked.send(1, contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "dev"})
		start := time.Now()
		if code := np.proc.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("plane exit %d", code)
		}
		if el := time.Since(start); el > 10*time.Second {
			t.Fatalf("shutdown with a blocked peer took %v", el)
		}
	})
}

// writeSidecarState writes a sidecar root for id enrolled at url with
// caPEM, as enroll would.
func writeSidecarState(t *testing.T, root, id, url string, caPEM []byte) {
	t.Helper()
	os.MkdirAll(root, 0o700)
	c, err := client.New(url, client.Trust{CAPEM: caPEM})
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := json.MarshalIndent(map[string]any{"schema_version": 1, "plane_url": url, "ca_pem": string(caPEM), "ca_fingerprint": c.Trust().Fingerprint}, "", "  ")
	// Field order matters only for byte comparisons; the reader is strict
	// about keys, not order.
	os.WriteFile(filepath.Join(root, "identity.json"), []byte("{\n  \"schema_version\": 1,\n  \"node_id\": \""+id+"\"\n}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "enrollment.json"), append(enc, '\n'), 0o600)
}

// listeningSockets reports whether pid has any listening TCP socket
// (Linux: /proc; macOS: lsof). Test code may branch on the host.
func listeningSockets(t *testing.T, pid int) []string {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		inodes := map[string]bool{}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range fds {
			if l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); err == nil {
				if in, ok := strings.CutPrefix(l, "socket:["); ok {
					inodes[strings.TrimSuffix(in, "]")] = true
				}
			}
		}
		var out []string
		for _, table := range []string{"tcp", "tcp6"} {
			f, err := os.Open(fmt.Sprintf("/proc/%d/net/%s", pid, table))
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				fields := strings.Fields(sc.Text())
				if len(fields) > 9 && fields[3] == "0A" && inodes[fields[9]] {
					out = append(out, table+" "+fields[1])
				}
			}
			f.Close()
		}
		return out
	case "darwin":
		out, err := exec.Command("lsof", "-a", "-n", "-P", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN").Output()
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatalf("lsof: %v", err)
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			return strings.Split(s, "\n")
		}
		return nil
	}
	t.Fatalf("unsupported host %s", runtime.GOOS)
	return nil
}

// FP-4: the outbound-only supervisor connection: restart and disconnect
// through the delegated sidecar contracts (injected retry clock, real
// plane processes) and a real CLI sidecar's SIGTERM shutdown.
func TestNodeReconnect(t *testing.T) {
	p := newNodeCLI(t)
	env := append(testkit.EnvWithout(os.Environ(), testkit.ProxyVars), nodeCLIEnv+"="+p.bin)
	t.Run("restart", func(t *testing.T) {
		nodeContract(t, "./internal/sidecar", "TestNodeReconnectContract", "restart", env)
	})
	t.Run("disconnect", func(t *testing.T) {
		nodeContract(t, "./internal/sidecar", "TestNodeReconnectContract", "disconnect", env)
	})
	t.Run("shutdown", func(t *testing.T) {
		np := startNodePlane(t, p)
		state := filepath.Join(t.TempDir(), "s")
		id := np.enroll(t, p, state)
		s := startSidecar(t, p, state, "heartbeat acknowledged")
		if ls := listeningSockets(t, s.cmd.Process.Pid); len(ls) != 0 {
			t.Fatalf("the sidecar listens: %v", ls)
		}
		start := time.Now()
		if code := s.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("sidecar exit %d:\n%s", code, s.logs.String())
		}
		if el := time.Since(start); el > 10*time.Second {
			t.Fatalf("sidecar shutdown took %v", el)
		}
		if s.stdout.Len() != 0 || !strings.HasSuffix(s.logs.String(), "callsheet: interrupted\n") || !s.logs.has("sidecar stopped", map[string]any{"node_id": id}) {
			t.Fatalf("sidecar output %q / %s", s.stdout.String(), s.logs.String())
		}
		// The lock is released and the plane saw the stream close.
		np.enroll(t, p, state)
		deadline := time.After(nodeWait)
		for !hasRecord(np.proc.logs.String(), "node disconnected", map[string]any{"node_id": id}) {
			select {
			case <-time.After(10 * time.Millisecond):
			case <-deadline:
				t.Fatalf("plane never saw the disconnect:\n%s", np.proc.logs.String())
			}
		}
	})
}

// FP-5: the monotonic lease through the delegated plane contract (injected
// node clock, production handlers and client).
func TestNodeLease(t *testing.T) {
	env := testkit.EnvWithout(os.Environ(), testkit.ProxyVars)
	t.Run("expiry", func(t *testing.T) {
		nodeContract(t, "./internal/plane", "TestNodeLeaseContract", "expiry", env)
	})
	t.Run("return", func(t *testing.T) {
		nodeContract(t, "./internal/plane", "TestNodeLeaseContract", "return", env)
	})
}

// FP-6: the durable roster: a stopped backup restores known nodes offline
// with null observations and identical PKI; invalid node state stops every
// command before listening, and is never deleted.
func TestNodeRegistry(t *testing.T) {
	p := newNodeCLI(t)
	t.Run("restore", func(t *testing.T) {
		np := startNodePlane(t, p)
		s1 := filepath.Join(t.TempDir(), "s1")
		ids := []string{np.enroll(t, p, s1), np.enroll(t, p, filepath.Join(t.TempDir(), "s2"))}
		slices.Sort(ids)
		sc := startSidecar(t, p, s1, "heartbeat acknowledged")
		sc.stop(t, syscall.SIGTERM)
		if code := np.proc.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("plane exit %d", code)
		}
		backup := filepath.Join(t.TempDir(), "backup")
		copyDir(t, np.root, backup)
		if got := listTree(t, filepath.Join(backup, "nodes")); len(got) != 2 {
			t.Fatalf("backup nodes %v", got)
		}
		sameFiles(t, stateFiles(t, np.root), stateFiles(t, backup))
		pp := p.start(t, "--state-dir", backup)
		r := p.run(t, "node", "ls", "--plane", "https://"+pp.addr, "--ca", filepath.Join(backup, "pki", "ca.crt"))
		want := "ID\tLIVENESS\tLAST_SEEN\tPROTOCOL_VERSION\tSOFTWARE_VERSION\tROLES\n" + ids[0] + "\toffline\t-\t-\t-\t[]\n" + ids[1] + "\toffline\t-\t-\t-\t[]\n"
		if r.code != 0 || r.stdout != want {
			t.Fatalf("restored roster = %+v, want %q", r, want)
		}
		if listeningRecords(pp.logs.String())[0]["ca_fingerprint"] != np.fp {
			t.Fatal("restored plane has another CA")
		}
		pp.stop(t, syscall.SIGTERM)
	})
	t.Run("validation", func(t *testing.T) {
		np := startNodePlane(t, p)
		id := np.enroll(t, p, filepath.Join(t.TempDir(), "s"))
		np.proc.stop(t, syscall.SIGTERM)
		rec := filepath.Join(np.root, "nodes", id+".json")
		good := nodeFile(t, rec)
		for _, c := range []struct {
			name   string
			mutate func()
			code   int
			msg    string
		}{
			{"corrupt", func() { os.WriteFile(rec, []byte(`{"schema_version":1,"id":"`+id+`"`), 0o600) }, 4, "callsheet: conflict: invalid node record " + rec},
			{"unknown key", func() {
				os.WriteFile(rec, bytes.Replace(good, []byte(`"enrolled_at"`), []byte(`"liveness": "online",`+"\n  "+`"enrolled_at"`), 1), 0o600)
			}, 4, `unknown field "liveness"`},
			{"unknown file", func() { os.WriteFile(filepath.Join(np.root, "nodes", "notes.txt"), nil, 0o600) }, 4, "holds unexpected " + filepath.Join(np.root, "nodes", "notes.txt")},
			{"symlink", func() {
				os.Remove(rec)
				target := filepath.Join(t.TempDir(), "rec.json")
				os.WriteFile(target, good, 0o600)
				os.Symlink(target, rec)
			}, 6, "callsheet: trust_failed: " + rec + " is a symbolic link"},
		} {
			c.mutate()
			for _, args := range [][]string{{"plane", "run", "--state-dir", np.root}, {"plane", "status", "--state-dir", np.root}, {"plane", "init", "--state-dir", np.root}} {
				r := p.run(t, args...)
				if r.code != c.code || r.stdout != "" || !strings.Contains(r.stderr, c.msg) || strings.Contains(r.stderr, "listening") {
					t.Fatalf("%s: %v = %+v", c.name, args, r)
				}
			}
			// Nothing was deleted; restore the record for the next case.
			if _, err := os.Lstat(rec); err != nil {
				t.Fatalf("%s: the record was deleted", c.name)
			}
			os.Remove(filepath.Join(np.root, "nodes", "notes.txt"))
			os.Remove(rec)
			os.WriteFile(rec, good, 0o600)
		}
		if r := p.run(t, "plane", "status", "--state-dir", np.root); r.code != 0 {
			t.Fatalf("restored state = %+v", r)
		}
	})
}

// FP-7: verified remote discovery from an independent machine root: exact
// text and JSON, sorting, empty rosters and error codes.
func TestNodeDiscovery(t *testing.T) {
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	empty := startNodePlane(t, p)
	s1 := filepath.Join(t.TempDir(), "s1")
	ids := []string{np.enroll(t, p, s1), np.enroll(t, p, filepath.Join(t.TempDir(), "s2"))}
	online := ids[0]
	slices.Sort(ids)
	sc := startSidecar(t, p, s1, "heartbeat acknowledged")
	defer sc.stop(t, syscall.SIGTERM)
	// The operator machine: a different HOME and cwd, no local state.
	op := newNodeCLI(t)
	t.Run("text", func(t *testing.T) {
		r := op.run(t, "node", "ls", "--plane", empty.url, "--ca", empty.ca)
		if r.code != 0 || r.stderr != "" || r.stdout != "ID\tLIVENESS\tLAST_SEEN\tPROTOCOL_VERSION\tSOFTWARE_VERSION\tROLES\n" {
			t.Fatalf("empty = %+v", r)
		}
		r = op.run(t, "node", "ls", "--plane", np.url, "--ca", np.ca)
		lines := strings.Split(r.stdout, "\n")
		if r.code != 0 || len(lines) != 4 || lines[3] != "" || lines[0] != "ID\tLIVENESS\tLAST_SEEN\tPROTOCOL_VERSION\tSOFTWARE_VERSION\tROLES" {
			t.Fatalf("ls = %+v", r)
		}
		for i, id := range ids {
			f := strings.Split(lines[i+1], "\t")
			if len(f) != 6 || f[0] != id || f[5] != "[]" {
				t.Fatalf("row %q", lines[i+1])
			}
			if id == online {
				if _, ok := contract.ParseTime(f[2]); !ok || f[1] != "online" || f[3] != "1" || f[4] != "dev" {
					t.Fatalf("online row %q", lines[i+1])
				}
			} else if f[1] != "offline" || f[2] != "-" || f[3] != "-" || f[4] != "-" {
				t.Fatalf("offline row %q", lines[i+1])
			}
		}
		r = op.run(t, "node", "show", online, "--plane", np.url, "--ca-fingerprint", np.fp)
		show := strings.Split(r.stdout, "\n")
		if r.code != 0 || len(show) != 7 || show[0] != "id: "+online || show[1] != "liveness: online" || !strings.HasPrefix(show[2], "last_seen: ") ||
			show[3] != "protocol_version: 1" || show[4] != "software_version: dev" || show[5] != "roles: []" {
			t.Fatalf("show = %+v", r)
		}
		if f := listTree(t, op.home); len(f) != 0 {
			t.Fatalf("discovery wrote local state: %v", f)
		}
	})
	t.Run("json", func(t *testing.T) {
		r := op.run(t, "node", "ls", "--json", "--plane", empty.url, "--ca", empty.ca)
		if r.code != 0 || r.stdout != `{"version":1,"nodes":[]}`+"\n" {
			t.Fatalf("empty json = %+v", r)
		}
		r = op.run(t, "node", "ls", "--plane", np.url, "--ca", np.ca, "--json")
		nodes, err := contract.ParseNodeListResponse([]byte(strings.TrimSuffix(r.stdout, "\n")))
		if r.code != 0 || err != nil || strings.Count(r.stdout, "\n") != 1 || len(nodes) != 2 || nodes[0].ID != ids[0] || nodes[1].ID != ids[1] {
			t.Fatalf("json = %+v %v", r, err)
		}
		again, _ := contract.Encode(contract.NodeListResponse{Version: 1, Nodes: nodes})
		if string(again)+"\n" != r.stdout {
			t.Fatalf("not the compact ordered envelope: %q", r.stdout)
		}
		r = op.run(t, "node", "show", "--json", ids[1], "--plane", np.url, "--ca", np.ca)
		n, err := contract.ParseNodeResponse([]byte(strings.TrimSuffix(r.stdout, "\n")))
		if r.code != 0 || err != nil || n.ID != ids[1] {
			t.Fatalf("show json = %+v %v", r, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		gone := "https://" + ln.Addr().String()
		ln.Close()
		for _, c := range []struct {
			args []string
			code int
			msg  string
		}{
			{[]string{"node", "ls", "--plane", np.url, "--ca", empty.ca}, 6, "callsheet: trust_failed: connection not trusted"},
			{[]string{"node", "show", ids[0], "--plane", np.url, "--ca-fingerprint", empty.fp}, 6, "callsheet: trust_failed: connection not trusted"},
			{[]string{"node", "ls", "--plane", gone, "--ca", np.ca}, 5, "callsheet: unavailable: cannot reach the plane"},
			{[]string{"node", "show", "n_" + strings.Repeat("f", 32), "--plane", np.url, "--ca", np.ca}, 3, "callsheet: not_found: "},
			{[]string{"node", "show", "not-a-node", "--plane", np.url, "--ca", np.ca}, 2, "callsheet: invalid_argument: invalid node ID"},
			{[]string{"node", "ls", "--ca", np.ca}, 2, "callsheet: invalid_argument: --plane is required"},
		} {
			mustCode(t, op.run(t, c.args...), c.code, c.msg)
		}
	})
}

// FP-8: the verification contract: OS path seams, native state, the exact
// driver plans and guards, and the strengthened S1' assertions, each
// through its delegated contract with run/pass evidence.
func TestNodePlatform(t *testing.T) {
	t.Run("paths", func(t *testing.T) {
		nodeContract(t, "./internal/sidecar", "TestSidecarPlatformContract", "", os.Environ(), "linux", "darwin")
		// The host's default root on the process boundary.
		p := newNodeCLI(t)
		np := startNodePlane(t, p)
		r := p.run(t, "sidecar", "enroll", "--plane", np.url, "--ca", np.ca)
		want := map[string]string{"linux": ".local/state/callsheet/sidecar", "darwin": "Library/Application Support/callsheet/sidecar"}[runtime.GOOS]
		if _, err := os.Stat(filepath.Join(p.home, want, "identity.json")); r.code != 0 || err != nil {
			t.Fatalf("default root %s: %+v %v", want, r, err)
		}
	})
	t.Run("native-state", func(t *testing.T) {
		nodeContract(t, "./internal/sidecar", "TestSidecarNativeStateContract", "", os.Environ(), "modes", "flock", "atomic")
	})
	t.Run("policy", func(t *testing.T) {
		const nodeFn = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestNodeEnrollment|TestNodeReconnect)$/^(locking|shutdown)$ ./tests/function"
		for _, goos := range []string{"linux", "darwin"} {
			shards, err := devcheck.StressShards(goos)
			if err != nil || len(shards[2].Steps) != 3 || strings.Join(shards[2].Steps[2].Argv, " ") != nodeFn ||
				!slices.Contains(shards[0].Steps[0].Argv, "./internal/sidecar") || !slices.Contains(shards[0].Steps[0].Argv, "./internal/client") || !slices.Contains(shards[0].Steps[0].Argv, "./internal/contract") {
				t.Fatalf("%s stress plan = %+v %v", goos, shards, err)
			}
		}
		if b := devcheck.BenchSteps(); len(b) != 3 || strings.Join(b[2].Argv, " ") != "go test ./internal/contract -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s" {
			t.Fatalf("bench plan = %+v", b)
		}
		// Every required node name is defined here, as a top-level test or
		// a mandatory subtest.
		src := string(repoFile(t, "tests/function/nodes_test.go"))
		var required []string
		for _, name := range devcheck.NativeRequiredTests() {
			if strings.HasPrefix(name, "TestNode") {
				required = append(required, name)
			}
		}
		if len(required) != 30 {
			t.Fatalf("required node names %v", required)
		}
		for _, name := range required {
			parent, sub, isSub := strings.Cut(name, "/")
			if !strings.Contains(src, "\nfunc "+parent+"(t *testing.T) {") || (isSub && !strings.Contains(src, `t.Run("`+sub+`", func(t *testing.T) {`)) {
				t.Fatalf("%s is not defined", name)
			}
		}
		if err := devcheck.CheckPlatformSources(testkit.MustRepoRoot(t)); err != nil {
			t.Fatal(err)
		}
		requireTerms(t, "Platform code", docSection(t, "Platform code"), "`internal/sidecar/lock_unix.go`", "`internal/sidecar/lock_other.go`", "`linux || darwin`", "`!linux && !darwin`")
		nodeContract(t, "./internal/devcheck", "TestNodeVerificationPolicyContract", "", os.Environ(), "linux", "darwin", "missing", "skipped", "failed", "source-guard", "test-only-env")
	})
	t.Run("sticky-write", func(t *testing.T) {
		nodeContract(t, "./internal/devcheck", "TestStressConcurrencyContract", "", os.Environ(), "overlap", "failure", "watchdog", "logs")
	})
}
