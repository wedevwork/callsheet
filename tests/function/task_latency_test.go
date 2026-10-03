//go:build linux || darwin

package function

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/sidecar"
)

// Iteration 10a function tests, one per FP (both native-required parents):
// TestTaskPromptReadiness (FP-1) and TestTaskFastGroupCleanup (FP-2). Each
// runs its scenarios in order, without t.Parallel, on its own otherwise
// idle deployment: the production plane over TLS in this process, and the
// production sidecar Run (the callsheet binary, or the sidecar package's
// fixture with the real guardian) in a child process whose JSON stderr is
// collected before it starts. Latencies are recorded in the test log;
// none is retried.

// promptBound and cleanupBound are the FP regression bounds.
const (
	promptBound  = 500 * time.Millisecond
	cleanupBound = 750 * time.Millisecond
	// cleanupGrace is the guardian's TERM grace (sidecar groupGrace).
	cleanupGrace = time.Second
)

// logStream collects a child's JSON log records as they are written and
// wakes waiters (event-driven: nothing polls the stream).
type logStream struct {
	mu   sync.Mutex
	raw  bytes.Buffer
	part []byte
	recs []map[string]any
	sig  chan struct{}
}

func newLogStream() *logStream { return &logStream{sig: make(chan struct{}, 1)} }

func (l *logStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.raw.Write(p)
	l.part = append(l.part, p...)
	for {
		i := bytes.IndexByte(l.part, '\n')
		if i < 0 {
			break
		}
		var rec map[string]any
		if json.Unmarshal(l.part[:i], &rec) == nil {
			l.recs = append(l.recs, rec)
		}
		l.part = l.part[i+1:]
	}
	l.mu.Unlock()
	select {
	case l.sig <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (l *logStream) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.raw.String()
}

// find returns the first record satisfying match.
func (l *logStream) find(match func(map[string]any) bool) (map[string]any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if match(r) {
			return r, true
		}
	}
	return nil, false
}

// await waits (bounded) for the first record satisfying match.
func (l *logStream) await(t *testing.T, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(nodeWait)
	for {
		if r, ok := l.find(match); ok {
			return r
		}
		select {
		case <-l.sig:
		case <-deadline:
			t.Fatalf("no %s within %v:\n%s", what, nodeWait, l.String())
			return nil
		}
	}
}

// msgFor matches a record of msg whose key equals value.
func msgFor(msg, key, value string) func(map[string]any) bool {
	return func(r map[string]any) bool { return r["msg"] == msg && r[key] == value }
}

// recordTime is a record's source time (slog JSON: RFC 3339 with at least
// millisecond precision).
func recordTime(t *testing.T, r map[string]any) time.Time {
	t.Helper()
	s, _ := r["time"].(string)
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("record time %q: %v", s, err)
	}
	return ts
}

// ---- The node-stream relay (FP-1) ----

// relayBeat is one sidecar heartbeat the relay forwarded.
type relayBeat struct {
	id   string
	body contract.HeartbeatBody
	at   time.Time
}

// relay is a TLS endpoint, trusted through the plane's own CA, that the
// sidecar enrolls with and streams through: it forwards every node frame
// unchanged in both directions (ordinary HTTPS requests by reverse proxy),
// records the sidecar's heartbeats and the plane's acknowledgements (the
// plane-side observable of an accepted heartbeat), and can withhold one
// acknowledgement (the plane frames after it pass on).
type relay struct {
	url    string
	plane  string
	client *http.Client
	mu     sync.Mutex
	sig    chan struct{}
	beats  []relayBeat
	acked  map[string]time.Time
	// holdFor selects the heartbeat whose acknowledgement is withheld;
	// held is its request ID once seen, release ends the hold.
	holdFor func(contract.HeartbeatBody) bool
	held    string
	release chan struct{}
}

// relayCert issues the relay's 127.0.0.1 server certificate from the
// plane's CA (its state directory's pki/ca.crt and pki/ca.key).
func relayCert(t *testing.T, planeRoot string) tls.Certificate {
	t.Helper()
	caPEM, err1 := os.ReadFile(filepath.Join(planeRoot, "pki", "ca.crt"))
	keyPEM, err2 := os.ReadFile(filepath.Join(planeRoot, "pki", "ca.key"))
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	ca, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "callsheet test relay"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startRelay(t *testing.T, p *fixedPlane) *relay {
	t.Helper()
	pem, err := os.ReadFile(p.ca)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	rl := &relay{plane: "wss://" + p.bind + contract.PathNodeStream, client: &http.Client{Transport: tr}, sig: make(chan struct{}, 1),
		acked: map[string]time.Time{}}
	target, _ := url.Parse(p.url)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = tr
	ctx, cancel := context.WithCancel(context.Background())
	var streams sync.WaitGroup
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != contract.PathNodeStream {
			proxy.ServeHTTP(w, r)
			return
		}
		streams.Add(1)
		defer streams.Done()
		rl.stream(ctx, w, r)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rl.url = "https://" + ln.Addr().String()
	done := make(chan struct{})
	go func() {
		srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{relayCert(t, p.root)}, MinVersion: tls.VersionTLS12}))
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		rl.releaseHeld()
		srv.Close()
		<-done
		streams.Wait()
		tr.CloseIdleConnections()
	})
	return rl
}

func (rl *relay) notify() {
	select {
	case rl.sig <- struct{}{}:
	default:
	}
}

// stream relays one node stream until either side ends.
func (rl *relay) stream(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	side, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer side.CloseNow()
	side.SetReadLimit(contract.MaxFrameBytes)
	up, _, err := websocket.Dial(ctx, rl.plane, &websocket.DialOptions{HTTPClient: rl.client, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer up.CloseNow()
	up.SetReadLimit(contract.MaxFrameBytes)
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var fwd sync.WaitGroup
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		defer stop()
		for {
			typ, b, err := side.Read(sctx)
			if err != nil {
				return
			}
			if f, err := contract.DecodeFrame(b, contract.FromSidecar); err == nil && f.Type == contract.FrameHeartbeat {
				if hb, err := contract.DecodeHeartbeat(f.Body); err == nil {
					rl.mu.Lock()
					rl.beats = append(rl.beats, relayBeat{id: f.RequestID, body: hb, at: time.Now()})
					if rl.holdFor != nil && rl.held == "" && rl.holdFor(hb) {
						rl.held = f.RequestID
					}
					rl.mu.Unlock()
					rl.notify()
				}
			}
			if up.Write(sctx, typ, b) != nil {
				return
			}
		}
	}()
	for {
		typ, b, err := up.Read(sctx)
		if err != nil {
			break
		}
		if f, err := contract.DecodeFrame(b, contract.FromPlane); err == nil && f.Type == contract.FrameHeartbeatAck {
			rl.mu.Lock()
			wait := rl.release
			if f.RequestID != rl.held || rl.held == "" {
				wait = nil
			}
			rl.mu.Unlock()
			if wait != nil {
				// Withheld: forwarded on release, while later frames pass.
				fwd.Add(1)
				go func() {
					defer fwd.Done()
					select {
					case <-wait:
					case <-sctx.Done():
						return
					}
					rl.mu.Lock()
					rl.acked[f.RequestID] = time.Now()
					rl.mu.Unlock()
					rl.notify()
					side.Write(sctx, typ, b)
				}()
				continue
			}
			rl.mu.Lock()
			rl.acked[f.RequestID] = time.Now()
			rl.mu.Unlock()
			rl.notify()
		}
		if side.Write(sctx, typ, b) != nil {
			break
		}
	}
	stop()
	<-upDone
	fwd.Wait()
}

// await waits (bounded) until cond holds over the relay's records (under
// its lock).
func (rl *relay) await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(nodeWait)
	for {
		rl.mu.Lock()
		ok := cond()
		rl.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-rl.sig:
		case <-deadline:
			t.Fatalf("relay: no %s within %v", what, nodeWait)
		}
	}
}

// hold withholds the plane's acknowledgement of the first later heartbeat
// that pred selects.
func (rl *relay) hold(pred func(contract.HeartbeatBody) bool) {
	rl.mu.Lock()
	rl.holdFor, rl.held, rl.release = pred, "", make(chan struct{})
	rl.mu.Unlock()
}

// releaseHeld forwards a withheld acknowledgement.
func (rl *relay) releaseHeld() {
	rl.mu.Lock()
	if rl.release != nil {
		close(rl.release)
		rl.release, rl.holdFor = nil, nil
	}
	rl.mu.Unlock()
}

// beatIndex returns the index of heartbeat id (under the lock), or -1.
func (rl *relay) beatIndex(id string) int {
	for i, b := range rl.beats {
		if b.id == id {
			return i
		}
	}
	return -1
}

// roleStatus returns role id's status in a heartbeat.
func roleStatus(hb contract.HeartbeatBody, id string) (contract.RoleStatus, bool) {
	for _, r := range hb.Roles {
		if r.RoleID == id {
			return r, true
		}
	}
	return contract.RoleStatus{}, false
}

// probeScript is the barrier-controlled readiness probe of the fake
// adapter: one probe passes at once when a pass-next file exists (it
// consumes it: a role validation); otherwise it passes once the barrier
// file exists, and waits until then (the sidecar's one-second probe bound
// ends it).
func probeScript(barrier, passNext string) string {
	return "#!/bin/sh\n[ \"$1\" = --callsheet-probe ] || exit 3\nif ! rm '" + passNext + "' 2>/dev/null; then\n\twhile [ ! -e '" + barrier +
		"' ]; do\n\t\tsleep 0.01\n\tdone\nfi\nprintf 'callsheet-fake-probe-v1\\n'\n"
}

// TestTaskPromptReadiness is iteration 10a's FP-1 function test: through
// the production node stream (TLS, the real plane, the callsheet sidecar
// binary and its fake adapter with a barrier-controlled probe), a passed
// readiness cycle is visible in ShowRole less than 500 ms after the
// sidecar logs it; the periodic heartbeat keeps the lease with an
// identical snapshot; changes while its acknowledgement is withheld
// coalesce into one report of the latest snapshot.
func TestTaskPromptReadiness(t *testing.T) {
	bg := context.Background()
	pl := startFixedPlane(t)
	rl := startRelay(t, pl)
	dir := t.TempDir()
	barrier, passNext, probe := filepath.Join(dir, "probe-ready"), filepath.Join(dir, "probe-pass-next"), filepath.Join(dir, "probe")
	if err := writeExecutable(probe, []byte(probeScript(barrier, passNext))); err != nil {
		t.Fatal(err)
	}
	ins, run := filepath.Join(dir, "instruction.md"), filepath.Join(dir, "runbook.md")
	os.WriteFile(ins, []byte("readiness instruction\n"), 0o644)
	os.WriteFile(run, []byte("readiness runbook\n"), 0o644)
	state := filepath.Join(t.TempDir(), "sidecar")
	e, err := sidecar.Enroll(bg, sidecar.EnrollOptions{StateDir: state, PlaneURL: rl.url, CAFile: pl.ca, SoftwareVersion: "readiness"})
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(pl.ca)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(pl.url, client.Trust{CAPEM: pem})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	// The collector is installed before the sidecar starts.
	logs := newLogStream()
	cmd := exec.Command(nodeBinary(t), "sidecar", "run", "--state-dir", state, "--fake-adapter", probe)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(nodeWait):
			cmd.Process.Kill()
			<-exited
			t.Error("the sidecar ignored SIGTERM")
		}
	})
	logs.await(t, "first heartbeat", func(r map[string]any) bool { return r["msg"] == "heartbeat acknowledged" })
	addRole := func(id string) {
		t.Helper()
		if _, err := cl.AddRole(bg, contract.RoleConfig{ID: id, Name: id, Node: e.NodeID, Adapter: "fake", Instruction: ins, Runbook: run,
			Model: "example model", Effort: "medium", Concurrency: 1}); err != nil {
			t.Fatalf("role add %s: %v\n%s", id, err, logs.String())
		}
	}
	roleA, roleB := "ready-a", "ready-b"

	// (1) Ready visibility. The role's validation probe passes; its first
	// readiness cycle's probe waits for the barrier. Pre-arm (DS4): the
	// plane's acknowledgement of a heartbeat reporting the new role
	// unready, observed at the relay, and ShowRole false; only then the
	// held probe may pass.
	if err := os.WriteFile(passNext, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	addRole(roleA)
	var unready relayBeat
	rl.await(t, "acknowledged unready heartbeat for "+roleA, func() bool {
		for _, b := range rl.beats {
			if st, ok := roleStatus(b.body, roleA); ok && !st.CanAccept {
				if _, acked := rl.acked[b.id]; acked {
					unready = b
					return true
				}
			}
		}
		return false
	})
	if v, err := cl.ShowRole(bg, roleA); err != nil || v.CanAccept {
		t.Fatalf("before the barrier: %+v %v", v, err)
	}
	if err := os.WriteFile(barrier, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ready := logs.await(t, "role ready for "+roleA, msgFor("role ready", "role_id", roleA))
	t0 := recordTime(t, ready) // armed: the cycle's publication, at its source
	var t1 time.Time
	for polls := 0; ; polls++ {
		v, err := cl.ShowRole(bg, roleA)
		if err == nil && v.CanAccept {
			t1 = time.Now()
			t.Logf("FP-1 ready visibility: %v after the sidecar's role ready record (%d ShowRole calls; unready heartbeat %s at %s)",
				t1.Sub(t0), polls+1, unready.id, unready.at.Format(time.RFC3339Nano))
			break
		}
		if time.Since(t0) > nodeWait {
			t.Fatalf("%s never became ready: %+v %v\n%s", roleA, v, err, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d := t1.Sub(t0); d < 0 || d >= promptBound {
		t.Fatalf("ready visibility %v (t0 %s, t1 %s), want 0 <= d < %v", d, t0.Format(time.RFC3339Nano), t1.Format(time.RFC3339Nano), promptBound)
	}

	// (2) A withheld ordinary acknowledgement: the next heartbeat's
	// acknowledgement is held at the relay (later plane frames still
	// pass); a second role's snapshot is installed and its first cycle
	// passes while that heartbeat is outstanding: nothing more is written
	// until the acknowledgement arrives, then one report carries the
	// latest snapshot.
	rl.hold(func(hb contract.HeartbeatBody) bool { _, ok := roleStatus(hb, roleB); return !ok })
	var held relayBeat
	rl.await(t, "a withheld heartbeat", func() bool {
		if i := rl.beatIndex(rl.held); i >= 0 {
			held = rl.beats[i]
			return true
		}
		return false
	})
	addRole(roleB)
	logs.await(t, "role ready for "+roleB, msgFor("role ready", "role_id", roleB))
	rl.mu.Lock()
	extra := len(rl.beats) - 1 - rl.beatIndex(held.id)
	rl.mu.Unlock()
	if extra != 0 {
		t.Fatalf("%d heartbeats written while %s was outstanding", extra, held.id)
	}
	released := time.Now()
	rl.releaseHeld()
	var coalesced relayBeat
	rl.await(t, "the coalesced report", func() bool {
		if i := rl.beatIndex(held.id); i >= 0 && i+1 < len(rl.beats) {
			coalesced = rl.beats[i+1]
			return true
		}
		return false
	})
	a, _ := roleStatus(coalesced.body, roleA)
	b, okB := roleStatus(coalesced.body, roleB)
	if coalesced.body.RolesRevision <= held.body.RolesRevision || !okB || !a.CanAccept || !b.CanAccept || coalesced.at.Sub(released) >= time.Second {
		t.Fatalf("after the acknowledgement: %+v %v later (withheld %+v), want one report of the latest snapshot", coalesced.body, coalesced.at.Sub(released), held.body)
	}
	t.Logf("FP-1 coalesced report: %v after the withheld acknowledgement (%s withheld %v)", coalesced.at.Sub(released), held.id, released.Sub(held.at))

	// (3) The periodic heartbeat backstop: the withheld heartbeat was the
	// periodic one, heartbeatInterval after the ready report, with the
	// identical snapshot; acknowledged by the plane, it kept the lease.
	var readyBeat relayBeat
	rl.mu.Lock()
	for i := rl.beatIndex(held.id) - 1; i >= 0; i-- {
		if st, ok := roleStatus(rl.beats[i].body, roleA); ok && st.CanAccept {
			readyBeat = rl.beats[i]
		} else {
			break
		}
	}
	_, ackedHeld := rl.acked[held.id]
	rl.mu.Unlock()
	pj, _ := json.Marshal(held.body)
	rj, _ := json.Marshal(readyBeat.body)
	if gap := held.at.Sub(readyBeat.at); readyBeat.id == "" || !bytes.Equal(pj, rj) || gap < 4500*time.Millisecond || !ackedHeld {
		t.Fatalf("periodic %s %s after %v (acknowledged %v), want the identical snapshot %s %s about 5 s later", held.id, pj, gap, ackedHeld, readyBeat.id, rj)
	}
	if n, err := cl.ShowNode(bg, e.NodeID); err != nil || n.Liveness != contract.LivenessOnline {
		t.Fatalf("node %+v %v", n, err)
	}
}

// ---- Group cleanup (FP-2) ----

// cleanupDirEnv names the cleanup fixture adapter's PID directory.
const cleanupDirEnv = "CALLSHEET_FN_CLEANUP_DIR"

// cleanupAdapter is the fixture adapter of TestTaskFastGroupCleanup: the
// composed prompt names its scenario; it records its descendant's PID,
// leaves it running (exits first, before it; a TERM-ignoring descendant
// only once it ignores TERM) and writes the final marker. Every
// descendant's stdio is detached from the task's pipes.
const cleanupAdapter = `#!/bin/sh
p=$(cat)
d="$` + cleanupDirEnv + `"
ready() { while [ ! -s "$1" ]; do sleep 0.01; done; }
case "$p" in
*cleanup-cooperative*)
	sleep 30 </dev/null >/dev/null 2>&1 &
	echo $! >"$d/cooperative.pid" ;;
*cleanup-resistant*)
	sh -c 'trap "" TERM; echo $$ >"$1"; exec sleep 30' sh "$d/resistant.pid" </dev/null >/dev/null 2>&1 &
	ready "$d/resistant.pid" ;;
*cleanup-churn*)
	sh -c 'trap "" TERM; echo $$ >"$1"; i=0; while [ $i -lt 20000 ]; do /bin/true; i=$((i+1)); done' sh "$d/churn.pid" </dev/null >/dev/null 2>&1 &
	ready "$d/churn.pid" ;;
*cleanup-cancel*)
	sleep 30 </dev/null >/dev/null 2>&1 &
	echo $! >"$d/cancel.pid"
	echo "cancel ready"
	wait
	exit 0 ;;
esac
printf '%s\n' '{"type":"callsheet_final","message":"cleanup fixture done"}'
`

// cleanupRig is one deployment of TestTaskFastGroupCleanup: the plane in
// this process, the sidecar package's fixture (production Run, real
// guardian re-executed from guardian, the cleanup adapter) and one role.
type cleanupRig struct {
	plane   *fixedPlane
	cl      *client.Client
	dir     string
	fixture *exec.Cmd
	exited  chan int
	logs    *logStream
	pids    []int
	n       int
}

func startCleanupRig(t *testing.T, guardian string) *cleanupRig {
	t.Helper()
	bg := context.Background()
	r := &cleanupRig{plane: startFixedPlane(t), dir: t.TempDir(), logs: newLogStream(), exited: make(chan int, 1)}
	state := filepath.Join(t.TempDir(), "sidecar")
	e, err := sidecar.Enroll(bg, sidecar.EnrollOptions{StateDir: state, PlaneURL: r.plane.url, CAFile: r.plane.ca, SoftwareVersion: "cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(r.plane.ca)
	if err != nil {
		t.Fatal(err)
	}
	if r.cl, err = client.New(r.plane.url, client.Trust{CAPEM: pem}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.cl.Close)
	adapterPath := filepath.Join(t.TempDir(), "cleanup-adapter")
	if err := writeExecutable(adapterPath, []byte(cleanupAdapter)); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(struct {
		StateDir    string `json:"state_dir"`
		FakeAdapter string `json:"fake_adapter"`
		Guardian    string `json:"guardian"`
	}{state, adapterPath, guardian})
	r.fixture = exec.Command(contractBinary(t, "./internal/sidecar"))
	r.fixture.Env = append(os.Environ(), sidecarFixtureEnv+"="+string(cfg), cleanupDirEnv+"="+r.dir)
	r.fixture.Stdout, r.fixture.Stderr = r.logs, r.logs
	if err := r.fixture.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := r.fixture.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		r.exited <- code
	}()
	t.Cleanup(func() {
		r.fixture.Process.Signal(syscall.SIGTERM)
		select {
		case <-r.exited:
		case <-time.After(nodeWait):
			r.fixture.Process.Kill()
			<-r.exited
			t.Error("the sidecar fixture ignored SIGTERM")
		}
		for _, pid := range r.pids {
			if syscall.Kill(pid, 0) == nil {
				t.Errorf("process %d outlived its scenario", pid)
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	poll(t, "node online", func() bool {
		n, err := r.cl.ShowNode(bg, e.NodeID)
		return err == nil && n.Liveness == contract.LivenessOnline
	})
	ins, run := filepath.Join(r.dir, "instruction.md"), filepath.Join(r.dir, "runbook.md")
	os.WriteFile(ins, []byte("cleanup instruction\n"), 0o644)
	os.WriteFile(run, []byte("cleanup runbook\n"), 0o644)
	if _, err := r.cl.AddRole(bg, contract.RoleConfig{ID: "cleanup", Name: "cleanup", Node: e.NodeID, Adapter: "fake", Instruction: ins, Runbook: run,
		Model: "example model", Effort: "medium", Concurrency: 1}); err != nil {
		t.Fatalf("role add: %v\n%s", err, r.logs.String())
	}
	return r
}

// cleanupRun is one finished scenario task: its terminal view, the pair of
// diagnostics and the cleanup's elapsed time.
type cleanupRun struct {
	view    contract.TaskView
	pgid    int
	exitRec map[string]any
	elapsed time.Duration
}

// dispatch runs one scenario task to its terminal state. during, when set,
// runs once the task is running.
func (r *cleanupRig) dispatch(t *testing.T, scenario string, during func(id string)) cleanupRun {
	t.Helper()
	bg := context.Background()
	r.n++
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "cleanup"}, Goal: "cleanup-" + scenario + " #" + strconv.Itoa(r.n),
		Payload: []string{}, Acceptance: "cleanup", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "test", Hostname: "cleanup"}}
	// The role's first readiness report, and each earlier task's freed
	// slot, may still be on their way: only an unavailable refusal is
	// retried.
	var v contract.TaskView
	deadline := time.Now().Add(nodeWait)
	for {
		var err error
		if v, err = r.cl.Dispatch(bg, req); err == nil {
			break
		}
		if contract.CodeOf(err) != contract.CodeUnavailable || time.Now().After(deadline) {
			t.Fatalf("dispatch %s: %v\n%s", scenario, err, r.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	id := v.TaskID
	if during != nil {
		poll(t, "task running", func() bool {
			v, err := r.cl.ShowTask(bg, id, contract.DefaultTailLines)
			return err == nil && v.State == contract.TaskRunning
		})
		during(id)
	}
	poll(t, "task terminal", func() bool {
		var err error
		v, err = r.cl.ShowTask(bg, id, contract.DefaultTailLines)
		return err == nil && contract.TaskTerminal(v.State)
	})
	exit := r.logs.await(t, "adapter exit diagnostic", msgFor("task adapter exit observed", "task_id", id))
	gone := r.logs.await(t, "group gone diagnostic", msgFor("task group gone", "task_id", id))
	pgid, _ := exit["pgid"].(float64)
	ns, ok := gone["cleanup_elapsed_ns"].(float64)
	if !ok || pgid <= 1 || gone["pgid"] != exit["pgid"] || ns < 0 {
		t.Fatalf("%s: unpaired diagnostics %v / %v", scenario, exit, gone)
	}
	if err := syscall.Kill(-int(pgid), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("%s: group %d still present: %v", scenario, int(pgid), err)
	}
	run := cleanupRun{view: v, pgid: int(pgid), exitRec: exit, elapsed: time.Duration(ns)}
	t.Logf("FP-2 %s: adapter exit to proven group absence %v (task %s, group %d, %s)", scenario, run.elapsed, id, run.pgid, v.State)
	return run
}

// descendant returns the PID a scenario recorded (and tracks it).
func (r *cleanupRig) descendant(t *testing.T, scenario string) int {
	t.Helper()
	var pid int
	poll(t, scenario+" descendant", func() bool {
		b, err := os.ReadFile(filepath.Join(r.dir, scenario+".pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 1
	})
	r.pids = append(r.pids, pid)
	return pid
}

// TestTaskFastGroupCleanup is iteration 10a's FP-2 function test: real
// guardians (the callsheet binary) and their adapters' groups on this
// host, with Linux's subreaper ECHILD proof or macOS's process-group list.
// An adapter that exits at once is proved gone less than 750 ms after the
// sidecar received its exit status; a cooperative descendant (TERM ends
// it), left behind by its exiting parent, completes before the grace, and
// so does a cancelled task's group; a TERM-resistant descendant, or a
// group whose member keeps forking and reaping children, keeps the full
// grace and its KILL; an injected unknown observation (the guardian built
// from the sidecar package's test binary) keeps the grace for a
// cooperative group. ESRCH remains the final absence.
func TestTaskFastGroupCleanup(t *testing.T) {
	r := startCleanupRig(t, nodeBinary(t))
	immediate := r.dispatch(t, "immediate", nil)
	if immediate.view.State != contract.TaskSucceeded || immediate.elapsed >= cleanupBound {
		t.Fatalf("immediate exit: %s after %v, want succeeded within %v", immediate.view.State, immediate.elapsed, cleanupBound)
	}
	coop := r.dispatch(t, "cooperative", nil)
	pid := r.descendant(t, "cooperative")
	if coop.view.State != contract.TaskSucceeded || coop.elapsed >= cleanupGrace {
		t.Fatalf("cooperative descendant: %s after %v, want succeeded before the %v grace", coop.view.State, coop.elapsed, cleanupGrace)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("cooperative descendant %d: %v", pid, err)
	}
	cancelled := r.dispatch(t, "cancel", func(id string) {
		r.descendant(t, "cancel")
		poll(t, "cancel output", func() bool {
			v, err := r.cl.ShowTask(context.Background(), id, contract.DefaultTailLines)
			return err == nil && strings.Contains(v.LogTail, "cancel ready")
		})
		if resp, err := r.cl.CancelTask(context.Background(), id); err != nil || !resp.Accepted {
			t.Fatalf("cancel %+v %v", resp, err)
		}
	})
	if cancelled.view.State != contract.TaskCancelled || cancelled.elapsed >= cleanupGrace {
		t.Fatalf("cancelled: %s after %v, want cancelled before the %v grace", cancelled.view.State, cancelled.elapsed, cleanupGrace)
	}
	// The resistant descendant outlives its exiting parent in the
	// guardian's group (Linux: adopted by the guardian's subreaper) until
	// the grace's KILL; observed while it still runs.
	resistant := r.dispatch(t, "resistant", func(id string) {
		pid := r.descendant(t, "resistant")
		exit := r.logs.await(t, "adapter exit diagnostic", msgFor("task adapter exit observed", "task_id", id))
		pgid, _ := exit["pgid"].(float64)
		wantAdoption(t, pid, int(pgid))
	})
	if resistant.view.State != contract.TaskSucceeded || resistant.elapsed < cleanupGrace-100*time.Millisecond {
		t.Fatalf("resistant descendant: %s after %v, want the full %v grace", resistant.view.State, resistant.elapsed, cleanupGrace)
	}
	churn := r.dispatch(t, "churn", nil)
	r.descendant(t, "churn")
	if churn.view.State != contract.TaskSucceeded || churn.elapsed < cleanupGrace-100*time.Millisecond {
		t.Fatalf("fork/exit churn: %s after %v, want the full %v grace (never proved alone while a member lives)", churn.view.State, churn.elapsed, cleanupGrace)
	}
	// An injected unknown observation (this guardian is the sidecar
	// package's test binary, whose group observation always answers
	// unknown) keeps the grace for the same cooperative group.
	u := startCleanupRig(t, contractBinary(t, "./internal/sidecar"))
	unknown := u.dispatch(t, "cooperative", nil)
	u.descendant(t, "cooperative")
	if unknown.view.State != contract.TaskSucceeded || unknown.elapsed < cleanupGrace-100*time.Millisecond {
		t.Fatalf("injected unknown: %s after %v, want the full %v grace", unknown.view.State, unknown.elapsed, cleanupGrace)
	}
}
