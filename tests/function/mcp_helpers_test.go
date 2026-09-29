//go:build linux || darwin

package function

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
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

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 07a function-test fixtures: a restartable in-process TLS
// plane, a scripted worker speaking protocol 5 over verified WSS, a
// controllable TLS proxy in front of the plane, and a Go MCP client that
// drives the real "callsheet mcp" binary over subprocess pipes with a
// single response dispatcher. No vendor executable is ever launched.

// mcpWait bounds every wait on an event in these tests (a failure bound,
// the project's helper join; never a scheduling assumption).
const mcpWait = 20 * time.Second

// ---- Plane ----

// mcpPlane is a real "callsheet plane run" subprocess (the built binary,
// never race-instrumented) on a fixed loopback port, restartable in place,
// with its CA file, fingerprint and a verified client.
type mcpPlane struct {
	root, bind, url, ca, fp string
	maxWait                 time.Duration
	cli                     *planeCLI
	cl                      *client.Client

	mu   sync.Mutex
	proc *exec.Cmd
	done chan struct{}
	logs *logLines
}

func startMCPPlane(t *testing.T, maxWait time.Duration) *mcpPlane {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bind := ln.Addr().String()
	ln.Close()
	p := &mcpPlane{root: filepath.Join(t.TempDir(), "plane"), bind: bind, url: "https://" + bind, maxWait: maxWait, cli: newNodeCLI(t)}
	r := p.cli.run(t, "plane", "init", "--state-dir", p.root, "--bind", bind, "--san", "127.0.0.1")
	if r.code != 0 {
		t.Fatalf("plane init = %+v", r)
	}
	for _, line := range strings.Split(r.stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "ca_fingerprint: "); ok {
			p.fp = v
		}
	}
	p.ca = filepath.Join(p.root, "pki", "ca.crt")
	t.Cleanup(func() {
		// Never leave the plane behind, whichever subtest last started it.
		p.mu.Lock()
		cmd, done := p.proc, p.done
		p.mu.Unlock()
		if cmd == nil {
			return
		}
		cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(mcpWait):
			t.Errorf("plane run %d was not reaped", cmd.Process.Pid)
		}
	})
	p.start(t)
	pem, err := os.ReadFile(p.ca)
	if err != nil {
		t.Fatal(err)
	}
	if p.cl, err = client.New(p.url, client.Trust{CAPEM: pem}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.cl.Close)
	return p
}

// start runs the plane on its port and waits for its listening record,
// retrying while a just-released port is briefly unavailable.
func (p *mcpPlane) start(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(mcpWait)
	for {
		args := []string{"plane", "run", "--state-dir", p.root}
		if p.maxWait > 0 {
			args = append(args, "--max-task-wait", p.maxWait.String())
		}
		cmd := exec.Command(p.cli.bin, args...)
		cmd.Dir, cmd.Env = p.cli.cwd, p.cli.env
		logs := &logLines{ready: make(chan string, 1)}
		cmd.Stdout, cmd.Stderr = io.Discard, logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		p.mu.Lock()
		p.proc, p.done, p.logs = cmd, done, logs
		p.mu.Unlock()
		select {
		case <-logs.ready:
			if p.cl != nil {
				p.cl.Close() // no idle connection to the previous run
			}
			return
		case <-done:
			if time.Now().After(deadline) {
				t.Fatalf("plane run exited before listening:\n%s", logs.String())
			}
			time.Sleep(20 * time.Millisecond)
		case <-time.After(mcpWait):
			t.Fatalf("plane run not ready:\n%s", logs.String())
		}
	}
}

// stop terminates the plane (SIGTERM, exit 130) and waits for it.
func (p *mcpPlane) stop(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	cmd, done := p.proc, p.done
	p.proc, p.done = nil, nil
	p.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(mcpWait):
		t.Fatal("the plane did not stop")
	}
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("plane run exited %d", code)
	}
}

// trust is the --ca flag set for this plane.
func (p *mcpPlane) trust() []string { return []string{"--plane", p.url, "--ca", p.ca} }

// awaitTask polls the plane until task id satisfies ok.
func (p *mcpPlane) awaitTask(t *testing.T, id string, ok func(contract.TaskView) bool) contract.TaskView {
	t.Helper()
	var v contract.TaskView
	deadline := time.Now().Add(mcpWait)
	for {
		var err error
		v, err = p.cl.ShowTask(context.Background(), id, contract.DefaultTailLines)
		if err == nil && ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: %+v %v", id, v.State, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// addRole registers a fake role on node with concurrency n.
func (p *mcpPlane) addRole(t *testing.T, id, name, node string, n int) {
	t.Helper()
	if _, err := p.cl.AddRole(context.Background(), contract.RoleConfig{ID: id, Name: name, Node: node, Adapter: "fake", Instruction: "/srv/i.md",
		Runbook: "/srv/r.md", Model: "example model", Effort: "medium", Concurrency: n}); err != nil {
		t.Fatalf("role add %s: %v", id, err)
	}
}

// ---- Scripted worker ----

// Worker scripts, chosen by the task goal's first word.
const (
	scriptQuick    = "quick"    // succeeds at once with a short output
	scriptFail     = "fail"     // exits 7
	scriptHold     = "hold"     // runs until released; a cancel ends it at once (cancelled)
	scriptStubborn = "stubborn" // runs until released, even after a cancel
	scriptLogs     = "logs"     // "logs N": N bytes of binary output, then success
	scriptBig      = "big"      // a 64 KiB final message and a long tail of quotes
)

type simTask struct {
	start    contract.TaskStartBody
	script   string
	released bool
	stopID   *string
	out      []byte
	result   *contract.TaskResultBody
}

// mcpWorker is a simulated sidecar: it enrolls, attaches over verified
// WSS, validates every role, heartbeats every second, and runs tasks by
// script, one sidecar request at a time.
type mcpWorker struct {
	t      *testing.T
	pl     *mcpPlane
	id     string
	runID  string
	events *evset

	wmu sync.Mutex // serializes frame writes

	mu    sync.Mutex
	ws    *websocket.Conn
	done  chan struct{}
	stopH chan struct{}
	b     int
	queue []func(rid string) (string, any)
	busy  bool
	rev   int
	roles []contract.RoleRecord
	tasks map[string]*simTask
}

func startMCPWorker(t *testing.T, p *mcpPlane, id string) *mcpWorker {
	t.Helper()
	w := &mcpWorker{t: t, pl: p, id: id, runID: strings.Repeat("6", 32), events: newEvset(), tasks: map[string]*simTask{}}
	if _, err := p.cl.EnrollNode(context.Background(), id, "sim-07a"); err != nil {
		t.Fatal(err)
	}
	w.attach(t, nil)
	t.Cleanup(w.detach)
	return w
}

// attach opens a new attachment reporting inv as its complete inventory.
func (w *mcpWorker) attach(t *testing.T, inv []contract.TaskInventoryEntry) {
	t.Helper()
	ws, err := w.pl.cl.DialNodeStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	// Sidecar request IDs restart at b1 and the acknowledged roles
	// revision at 0 on every attachment.
	w.ws, w.done, w.stopH, w.busy, w.queue, w.b = ws, make(chan struct{}), make(chan struct{}), false, nil, 0
	w.rev, w.roles = 0, nil
	done, stopH := w.done, w.stopH
	w.mu.Unlock()
	w.write(contract.FrameHello, "h1", contract.HelloBody{NodeID: w.id, SoftwareVersion: "sim-07a"})
	go w.loop(ws, done, inv)
	go func() {
		// Heartbeats keep the lease across long scenarios.
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				w.mu.Lock()
				w.heartbeatLocked()
				w.mu.Unlock()
			case <-stopH:
				return
			case <-done:
				return
			}
		}
	}()
	w.await(t, "reconciled")
	// The first heartbeat of the attachment makes the node online.
	w.mu.Lock()
	w.heartbeatLocked()
	w.mu.Unlock()
	w.await(t, "heartbeat-acked")
}

// detach closes the current attachment and waits for its loop.
func (w *mcpWorker) detach() {
	w.mu.Lock()
	ws, done, stopH := w.ws, w.done, w.stopH
	w.mu.Unlock()
	if ws == nil {
		return
	}
	close(stopH)
	ws.CloseNow()
	select {
	case <-done:
	case <-time.After(mcpWait):
		w.t.Error("the simulated worker did not stop")
	}
	w.mu.Lock()
	if w.ws == ws {
		w.ws = nil
	}
	w.mu.Unlock()
}

func (w *mcpWorker) write(typ, rid string, body any) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.writeLocked(typ, rid, body)
}

// writeLocked writes one frame; the caller holds wmu (never mu).
func (w *mcpWorker) writeLocked(typ, rid string, body any) {
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, rid, body)
	if err != nil {
		panic(err)
	}
	w.mu.Lock()
	ws := w.ws
	w.mu.Unlock()
	if ws == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpWait)
	defer cancel()
	ws.Write(ctx, websocket.MessageText, b)
}

func (w *mcpWorker) event(e string) { w.events.add(e) }

// requestLocked queues a sidecar request; it is sent when the slot is free.
func (w *mcpWorker) requestLocked(f func(rid string) (string, any)) {
	w.queue = append(w.queue, f)
	w.pumpLocked()
}

func (w *mcpWorker) pumpLocked() {
	if w.busy || len(w.queue) == 0 || w.ws == nil {
		return
	}
	f := w.queue[0]
	w.queue = w.queue[1:]
	w.b++
	rid := "b" + strconv.Itoa(w.b)
	w.busy = true
	go func() {
		// The body is built and written under the write lock, so a
		// heartbeat's revision is the one last acknowledged on the wire.
		w.wmu.Lock()
		defer w.wmu.Unlock()
		w.mu.Lock()
		typ, body := f(rid)
		w.mu.Unlock()
		w.writeLocked(typ, rid, body)
	}()
}

func (w *mcpWorker) heartbeatLocked() {
	if w.ws == nil {
		return
	}
	w.requestLocked(func(string) (string, any) { return contract.FrameHeartbeat, w.heartbeatBodyLocked() })
}

// heartbeatBodyLocked reports every role ready with its own concurrency;
// the body is built when sent (the caller holds mu through pumpLocked).
func (w *mcpWorker) heartbeatBodyLocked() contract.HeartbeatBody {
	hb := contract.HeartbeatBody{RolesRevision: w.rev, Roles: []contract.RoleStatus{}}
	for _, r := range w.roles {
		hb.Roles = append(hb.Roles, contract.RoleStatus{RoleID: r.ID, Concurrency: r.Concurrency, CanAccept: true})
	}
	return hb
}

func (w *mcpWorker) loop(ws *websocket.Conn, done chan struct{}, inv []contract.TaskInventoryEntry) {
	defer close(done)
	for {
		_, b, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		f, err := contract.DecodeFrame(b, contract.FromPlane)
		if err != nil {
			w.event("protocol error: " + err.Error())
			return
		}
		switch f.Type {
		case contract.FrameHelloOK:
			if inv == nil {
				inv = []contract.TaskInventoryEntry{}
			}
			w.write(contract.FrameTaskInventory, "i1", contract.TaskInventoryBody{RunID: w.runID, Final: true, Entries: inv})
		case contract.FrameTaskInventoryAck:
		case contract.FrameTaskReconcile:
			rb, err := contract.DecodeTaskReconcile(f.Body)
			if err != nil {
				w.event("bad reconcile: " + err.Error())
				return
			}
			w.write(contract.FrameTaskReconcileAck, f.RequestID, contract.TaskReconcileAckBody{Received: true})
			w.mu.Lock()
			for _, e := range rb.Entries {
				if st := w.tasks[e.TaskID]; st != nil && e.Action == contract.ActionSendResult {
					w.sendResultLocked(st, true)
				}
			}
			w.mu.Unlock()
			if rb.Final {
				w.event("reconciled")
			}
		case contract.FrameRoleValidate:
			w.write(contract.FrameRoleValidateResult, f.RequestID, contract.RoleValidateResult{})
		case contract.FrameRolesReplace:
			body, err := contract.DecodeRolesReplace(f.Body, nil)
			if err != nil {
				w.event("bad snapshot")
				return
			}
			w.wmu.Lock()
			w.writeLocked(contract.FrameRolesReplaceAck, f.RequestID, contract.RolesReplaceAckBody{Revision: body.Revision})
			w.mu.Lock()
			w.rev, w.roles = body.Revision, body.Roles
			w.heartbeatLocked()
			w.mu.Unlock()
			w.wmu.Unlock()
		case contract.FrameTaskStart:
			st, err := contract.DecodeTaskStart(f.Body, nil)
			if err != nil {
				w.event("bad start")
				return
			}
			w.write(contract.FrameTaskStartResult, f.RequestID, contract.TaskStartResult{TaskID: st.TaskID})
			w.mu.Lock()
			w.startLocked(st)
			w.mu.Unlock()
			w.event("started " + st.TaskID)
		case contract.FrameTaskCancel:
			cb, err := contract.DecodeTaskCancel(f.Body)
			if err != nil {
				w.event("bad cancel")
				return
			}
			w.write(contract.FrameTaskCancelAck, f.RequestID, contract.TaskCancelAckBody{TaskID: cb.TaskID, Execution: cb.Execution, StopID: cb.StopID, Received: true})
			w.mu.Lock()
			if st := w.tasks[cb.TaskID]; st != nil && st.result == nil {
				id := cb.StopID
				st.stopID = &id
				if st.script == scriptHold {
					w.finishLocked(st)
				}
			}
			w.mu.Unlock()
			w.event("cancel " + cb.TaskID)
		case contract.FrameTaskResultAck:
			ack, err := contract.DecodeTaskResultAck(f.Body)
			if err != nil {
				w.event("bad result ack")
				return
			}
			w.mu.Lock()
			w.busy = false
			if st := w.tasks[ack.TaskID]; st != nil && !ack.Committed {
				// Received but not yet durable: resend (a real sidecar waits).
				w.sendResultLocked(st, false)
			}
			w.pumpLocked()
			w.mu.Unlock()
			if ack.Committed {
				w.event("result-acked " + ack.TaskID)
			}
		case contract.FrameHeartbeatAck, contract.FrameTaskLogAck:
			w.mu.Lock()
			w.busy = false
			w.pumpLocked()
			w.mu.Unlock()
			if f.Type == contract.FrameHeartbeatAck {
				w.event("heartbeat-acked")
			}
		case contract.FrameError:
			w.event("plane error " + string(f.Body))
			return
		default:
			w.event("unexpected " + f.Type)
			return
		}
	}
}

// startLocked runs a started task by its goal's script.
func (w *mcpWorker) startLocked(st contract.TaskStartBody) {
	script, arg, _ := strings.Cut(strings.TrimSpace(st.Request.Goal), " ")
	s := &simTask{start: st, script: script}
	w.tasks[st.TaskID] = s
	switch script {
	case scriptHold, scriptStubborn:
		s.out = []byte("working on " + st.TaskID + "\n")
		w.logLocked(s, s.out, nil)
		return
	case scriptLogs:
		n, _ := strconv.Atoi(strings.Fields(arg + " 0")[0])
		s.out = binaryOutput(n)
	case scriptFail:
		s.out = []byte("failure line\n")
	case scriptBig:
		line := strings.Repeat(`"`, 1000) + "\n"
		s.out = []byte(strings.Repeat(line, 64))
	default:
		s.out = []byte("line one\nline two\n")
	}
	w.logLocked(s, s.out, nil)
	w.finishLocked(s)
}

// binaryOutput is n bytes of output holding invalid UTF-8, NUL, escape
// sequences and newlines.
func binaryOutput(n int) []byte {
	pattern := []byte("out\x00\xff\xfe\x1b[31m\"\\\n")
	out := make([]byte, n)
	for i := range out {
		out[i] = pattern[i%len(pattern)]
	}
	return out
}

// logLocked queues output chunks (a late digest tags a replayed tail).
func (w *mcpWorker) logLocked(s *simTask, out []byte, late *string) {
	for off := 0; off < len(out); off += contract.MaxLogChunkBytes {
		chunk := out[off:min(len(out), off+contract.MaxLogChunkBytes)]
		o := off
		w.requestLocked(func(string) (string, any) {
			return contract.FrameTaskLog, contract.TaskLogBody{TaskID: s.start.TaskID, Execution: s.start.Execution, Offset: o, Data: chunk, LateDigest: late}
		})
	}
}

// finishLocked seals the task's result (cancelled under a received stop)
// and sends it.
func (w *mcpWorker) finishLocked(s *simTask) {
	if s.result != nil {
		return
	}
	exit := 0
	if s.script == scriptFail {
		exit = 7
	}
	msg := "fake task completed"
	if s.script == scriptBig {
		msg = strings.Repeat(`"`, contract.MaxFinalMessageBytes)
	}
	res := contract.TaskResultBody{TaskID: s.start.TaskID, Execution: s.start.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit,
		FinalMessage: &msg, OutputBytes: len(s.out)}
	if s.stopID != nil {
		sig := "SIGTERM"
		res.Outcome, res.ExitCode, res.Signal, res.StopID = contract.OutcomeCancelled, nil, &sig, s.stopID
	}
	sealed := res.Sealed()
	s.result = &sealed
	w.sendResultLocked(s, false)
}

func (w *mcpWorker) sendResultLocked(s *simTask, replayLogs bool) {
	if s.result == nil {
		return
	}
	res := *s.result
	if replayLogs {
		d := res.Digest
		w.logLocked(s, s.out, &d)
	}
	w.requestLocked(func(string) (string, any) { return contract.FrameTaskResult, res })
}

// release ends a held task (a stubborn one reports cancelled if a stop
// arrived).
func (w *mcpWorker) release(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.tasks[id]; s != nil {
		s.released = true
		w.finishLocked(s)
	}
}

// inventoryEntry is the inventory line of a task this worker ran to its
// result (phase result).
func (w *mcpWorker) inventoryEntry(id string) contract.TaskInventoryEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.tasks[id]
	started := contract.FormatTime(time.Now().Add(-time.Second))
	d := s.result.Digest
	return contract.TaskInventoryEntry{TaskID: id, Execution: s.start.Execution, StartDigest: s.start.StartDigestHex(), Phase: contract.PhaseResult,
		StartedAt: &started, ResultDigest: &d}
}

// runningEntry is the inventory line of a task this worker still runs.
func (w *mcpWorker) runningEntry(id string) contract.TaskInventoryEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.tasks[id]
	started := contract.FormatTime(time.Now().Add(-time.Second))
	return contract.TaskInventoryEntry{TaskID: id, Execution: s.start.Execution, StartDigest: s.start.StartDigestHex(), Phase: contract.PhaseRunning, StartedAt: &started}
}

// sealLocal seals a held task's result without sending it.
func (w *mcpWorker) sealLocal(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.tasks[id]
	exit := 0
	msg := "late completion"
	s.out = append(s.out, []byte("late output \xff\n")...)
	res := contract.TaskResultBody{TaskID: id, Execution: s.start.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit, FinalMessage: &msg,
		OutputBytes: len(s.out)}.Sealed()
	s.result = &res
}

// await consumes one occurrence of worker event want.
func (w *mcpWorker) await(t *testing.T, want string) {
	t.Helper()
	if !w.events.take(want, mcpWait) {
		t.Fatalf("simulated worker never saw %s (events %v)", want, w.events.snapshot())
	}
}

// evset counts named events; each recorded occurrence satisfies one take.
type evset struct {
	mu     sync.Mutex
	cond   *sync.Cond
	counts map[string]int
	log    []string
}

func newEvset() *evset {
	e := &evset{counts: map[string]int{}}
	e.cond = sync.NewCond(&e.mu)
	return e
}

func (e *evset) add(ev string) {
	e.mu.Lock()
	e.counts[ev]++
	if len(e.log) < 256 {
		e.log = append(e.log, ev)
	}
	e.mu.Unlock()
	e.cond.Broadcast()
}

// clear forgets every recorded event.
func (e *evset) clear() {
	e.mu.Lock()
	e.counts = map[string]int{}
	e.log = nil
	e.mu.Unlock()
}

func (e *evset) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

func (e *evset) take(ev string, timeout time.Duration) bool {
	expired := false
	tm := time.AfterFunc(timeout, func() {
		e.mu.Lock()
		expired = true
		e.mu.Unlock()
		e.cond.Broadcast()
	})
	defer tm.Stop()
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.counts[ev] == 0 {
		if expired {
			return false
		}
		e.cond.Wait()
	}
	e.counts[ev]--
	return true
}

// ---- Proxy ----

// planeProxy is a TLS proxy in front of the plane with its own fixture
// CA: it forwards every request and can hold, drop, rewrite or replace a
// response, reporting request receipt, response and cancellation events.
type planeProxy struct {
	url, ca string
	target  string
	up      *http.Client
	srv     *http.Server
	events  *evset

	mu      sync.Mutex
	holds   map[string]chan struct{} // "METHOD path-prefix" -> release
	drops   map[string]bool
	inject  map[string]*contract.Error
	version string
	counts  map[string]int
}

func startPlaneProxy(t *testing.T, p *mcpPlane) *planeProxy {
	t.Helper()
	ca, err := testkit.NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(p.ca)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("plane CA does not parse")
	}
	x := &planeProxy{target: p.url, events: newEvset(), holds: map[string]chan struct{}{}, drops: map[string]bool{},
		inject: map[string]*contract.Error{}, counts: map[string]int{},
		up: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: false}}}
	x.ca = filepath.Join(t.TempDir(), "proxy-ca.crt")
	if err := os.WriteFile(x.ca, ca.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	x.url = "https://" + ln.Addr().String()
	x.srv = &http.Server{Handler: x, ErrorLog: nil, ReadHeaderTimeout: mcpWait,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}}
	x.srv.ErrorLog = log.New(io.Discard, "", 0)
	go x.srv.ServeTLS(ln, "", "")
	t.Cleanup(func() {
		x.mu.Lock()
		for k, c := range x.holds {
			close(c)
			delete(x.holds, k)
		}
		x.mu.Unlock()
		x.srv.Close()
		x.up.CloseIdleConnections()
	})
	return x
}

func (x *planeProxy) trust() []string { return []string{"--plane", x.url, "--ca", x.ca} }

// key is a request's "METHOD path" (query excluded).
func proxyKey(r *http.Request) string { return r.Method + " " + r.URL.Path }

func (x *planeProxy) match(m map[string]bool, key string) bool {
	for k := range m {
		if strings.HasPrefix(key, k) {
			return true
		}
	}
	return false
}

// hold makes responses to requests whose key starts with prefix wait for
// the returned release.
func (x *planeProxy) hold(prefix string) (release func()) {
	c := make(chan struct{})
	x.mu.Lock()
	x.holds[prefix] = c
	x.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			x.mu.Lock()
			if x.holds[prefix] == c {
				delete(x.holds, prefix)
			}
			x.mu.Unlock()
			close(c)
		})
	}
}

func (x *planeProxy) event(e string) { x.events.add(e) }

// await consumes one occurrence of proxy event want.
func (x *planeProxy) await(t *testing.T, want string) {
	t.Helper()
	if !x.events.take(want, mcpWait) {
		t.Fatalf("proxy never saw %s (events %v)", want, x.events.snapshot())
	}
}

func (x *planeProxy) count(key string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.counts[key]
}

func (x *planeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := proxyKey(r)
	x.mu.Lock()
	x.counts[key]++
	var inj *contract.Error
	for k, e := range x.inject {
		if strings.HasPrefix(key, k) {
			inj = e
		}
	}
	version := x.version
	var hold chan struct{}
	for k, c := range x.holds {
		if strings.HasPrefix(key, k) {
			hold = c
		}
	}
	x.mu.Unlock()
	x.event("request " + key)
	if inj != nil {
		b, _ := inj.MarshalJSON()
		w.Header().Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(contract.HTTPStatus(inj.Code))
		w.Write(b)
		return
	}
	body, _ := io.ReadAll(r.Body)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, x.target+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	for k, v := range r.Header {
		req.Header[k] = v
	}
	resp, err := x.up.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			x.event("canceled " + key) // the MCP client gave the request up
		} else {
			x.event("upstream-error " + key)
		}
		panic(http.ErrAbortHandler) // the client sees an interrupted transport
	}
	rb, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	x.event("response " + key)
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			x.event("canceled " + key)
			<-hold
		}
	}
	x.mu.Lock()
	drop := x.match(x.drops, key) // decided once the plane answered
	x.mu.Unlock()
	if drop {
		x.event("dropped " + key)
		panic(http.ErrAbortHandler)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	if version != "" {
		w.Header().Set(contract.ProtocolHeader, version)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(rb)
	x.event("answered " + key)
}

// ---- MCP client ----

// mcpProc is one "callsheet mcp" subprocess in its own process group with
// a single response dispatcher on stdout.
type mcpProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *safeBuffer
	exited chan struct{}
	state  *os.ProcessState

	mu         sync.Mutex
	waiters    map[string]chan mcpResponse
	unexpected []string
	all        []string
	nextID     int
	dispatched chan struct{}
}

type mcpResponse struct {
	raw    string
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// startMCPProc starts "callsheet mcp args..." without a dispatcher (raw).
func startMCPProc(t *testing.T, args ...string) *mcpProc {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	m := &mcpProc{t: t, stdin: inW, stdout: outR, stderr: &safeBuffer{}, exited: make(chan struct{}), waiters: map[string]chan mcpResponse{}}
	m.cmd = exec.Command(nodeBinary(t), append([]string{"mcp"}, args...)...)
	home := t.TempDir()
	m.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	m.cmd.Dir = home
	m.cmd.Stdin, m.cmd.Stdout, m.cmd.Stderr = inR, outW, m.stderr
	m.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := m.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inR.Close()
	outW.Close()
	go func() {
		m.cmd.Wait()
		m.state = m.cmd.ProcessState
		close(m.exited)
	}()
	t.Cleanup(func() {
		m.cmd.Process.Kill()
		select {
		case <-m.exited:
		case <-time.After(mcpWait):
			t.Errorf("mcp process %d not reaped", m.cmd.Process.Pid)
		}
		m.stdin.Close()
		m.stdout.Close()
	})
	return m
}

// startMCP starts the process with the dispatcher and initializes it.
func startMCP(t *testing.T, args ...string) *mcpProc {
	t.Helper()
	m := startMCPProc(t, args...)
	m.dispatch()
	m.initialize("coordinator-test", "7.0.1")
	return m
}

// dispatch starts the single response dispatcher: every stdout line must
// be one JSON-RPC response to a request this client sent.
func (m *mcpProc) dispatch() {
	m.dispatched = make(chan struct{})
	go func() {
		defer close(m.dispatched)
		br := bufio.NewReaderSize(m.stdout, 1<<20)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				m.route(line)
			}
			if err != nil {
				return
			}
		}
	}()
}

// largeFrame is the size above which the dispatcher routes a line by its
// prefix alone (a maximum log frame is ~14 MB; decoding it repeatedly
// would dominate the suite under the race detector). Large answers are
// compared byte for byte by their tests.
const largeFrame = 1 << 20

var framePrefix = `{"jsonrpc":"2.0","id":`

func (m *mcpProc) route(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(line) < largeFrame {
		m.all = append(m.all, line)
	}
	id, ok := "", strings.HasSuffix(line, "}\n") && strings.HasPrefix(line, framePrefix)
	if ok {
		rest := line[len(framePrefix):]
		if i := strings.IndexByte(rest, ','); i > 0 {
			id = rest[:i]
		} else {
			ok = false
		}
	}
	if ok && len(line) < largeFrame {
		var env struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
		}
		ok = json.Unmarshal([]byte(line), &env) == nil && env.JSONRPC == "2.0" && string(env.ID) == id
	}
	c := m.waiters[id]
	if !ok || c == nil {
		m.unexpected = append(m.unexpected, line[:min(len(line), 4096)])
		return
	}
	delete(m.waiters, id)
	r := mcpResponse{raw: line}
	if len(line) < largeFrame {
		json.Unmarshal([]byte(line), &r)
	}
	c <- r
}

func (m *mcpProc) send(v any) {
	m.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		m.t.Fatal(err)
	}
	if _, err := m.stdin.Write(append(b, '\n')); err != nil {
		m.t.Fatalf("stdin: %v", err)
	}
}

// requestAsync sends a request and returns its ID and answer channel.
func (m *mcpProc) requestAsync(method string, params any) (string, <-chan mcpResponse) {
	m.mu.Lock()
	m.nextID++
	id := strconv.Itoa(m.nextID)
	c := make(chan mcpResponse, 1)
	m.waiters[id] = c
	m.mu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": m.nextID, "method": method}
	if params != nil {
		msg["params"] = params
	}
	m.send(msg)
	return id, c
}

func (m *mcpProc) wait(c <-chan mcpResponse) mcpResponse {
	m.t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(mcpWait):
		m.t.Fatalf("no answer; stderr %q", m.stderr.String())
		return mcpResponse{}
	}
}

// drained waits until the dispatcher read stdout to its end.
func (m *mcpProc) drained() {
	m.t.Helper()
	select {
	case <-m.dispatched:
	case <-time.After(mcpWait):
		m.t.Fatal("stdout did not end")
	}
}

func (m *mcpProc) request(method string, params any) mcpResponse {
	m.t.Helper()
	_, c := m.requestAsync(method, params)
	return m.wait(c)
}

func (m *mcpProc) notify(method string, params any) {
	m.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	m.send(msg)
}

func (m *mcpProc) initialize(name, version string) {
	m.t.Helper()
	r := m.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": name, "version": version}})
	if r.Error != nil || !strings.Contains(string(r.Result), `"protocolVersion":"2025-06-18"`) {
		m.t.Fatalf("initialize %s", r.raw)
	}
	m.notify("notifications/initialized", nil)
}

// toolResult is a decoded tools/call answer.
type toolResult struct {
	text    string
	isError bool
	raw     string
}

func decodeTool(t *testing.T, r mcpResponse) toolResult {
	t.Helper()
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if r.Error != nil || json.Unmarshal(r.Result, &res) != nil || len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("not a tool result: %.300s", r.raw)
	}
	return toolResult{text: res.Content[0].Text, isError: res.IsError, raw: r.raw}
}

func (m *mcpProc) callAsync(tool string, args any) (string, <-chan mcpResponse) {
	if args == nil {
		args = map[string]any{}
	}
	return m.requestAsync("tools/call", map[string]any{"name": tool, "arguments": args})
}

func (m *mcpProc) call(tool string, args any) toolResult {
	m.t.Helper()
	_, c := m.callAsync(tool, args)
	return decodeTool(m.t, m.wait(c))
}

// ok calls tool and requires success; it returns the text.
func (m *mcpProc) ok(tool string, args any) string {
	m.t.Helper()
	r := m.call(tool, args)
	if r.isError {
		m.t.Fatalf("%s %v: %s", tool, args, r.text)
	}
	return r.text
}

// fails calls tool and requires a tool error of code.
func (m *mcpProc) fails(tool string, args any, code contract.Code) *contract.Error {
	m.t.Helper()
	r := m.call(tool, args)
	if !r.isError {
		m.t.Fatalf("%s %v succeeded: %.300s", tool, args, r.text)
	}
	e, err := contract.ParseErrorBody([]byte(r.text))
	if err != nil || e.Code != code {
		m.t.Fatalf("%s %v: want %s, got %s (%v)", tool, args, code, r.text, err)
	}
	return e
}

// exit waits for the process and returns its wait status.
func (m *mcpProc) exit() syscall.WaitStatus {
	m.t.Helper()
	select {
	case <-m.exited:
	case <-time.After(mcpWait):
		m.t.Fatalf("mcp did not exit; stderr %q", m.stderr.String())
	}
	return m.state.Sys().(syscall.WaitStatus)
}

// requireExit waits for a normal exit with code.
func (m *mcpProc) requireExit(code int) {
	m.t.Helper()
	ws := m.exit()
	if !ws.Exited() || ws.Signaled() || ws.ExitStatus() != code {
		m.t.Fatalf("mcp wait status %v (exited %t, code %d, signaled %t), want exit %d; stderr %q", ws, ws.Exited(), ws.ExitStatus(), ws.Signaled(), code, m.stderr.String())
	}
}

// protocolOnly requires every stdout line to have been an expected
// response.
func (m *mcpProc) protocolOnly() {
	m.t.Helper()
	if m.dispatched != nil {
		select {
		case <-m.dispatched:
		case <-time.After(mcpWait):
			m.t.Fatal("stdout did not end")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.unexpected) > 0 {
		m.t.Fatalf("stdout carried non-protocol or unexpected lines: %q", m.unexpected)
	}
}

// reaped requires the process and its whole process group gone.
func (m *mcpProc) reaped() {
	m.t.Helper()
	m.exit()
	pid := m.cmd.Process.Pid
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		m.t.Fatalf("mcp process %d still exists: %v", pid, err)
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		m.t.Fatalf("mcp process group %d still has members: %v", pid, err)
	}
}

// taskArgs is a dispatch argument object.
func dispatchArgs(kind, value, goal string, extra map[string]any) map[string]any {
	a := map[string]any{"target": map[string]any{"kind": kind, "value": value}, "goal": goal, "acceptance": "tests pass"}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

// taskIDOf returns the task_id of a dispatch answer.
func taskIDOf(t *testing.T, text string) string {
	t.Helper()
	var r struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil || !contract.ValidTaskID(r.TaskID) {
		t.Fatalf("dispatch answer %.300s", text)
	}
	return r.TaskID
}

// sameJSON requires two JSON texts to decode equal.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var a, b any
	if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		t.Fatalf("not JSON: %.200s / %.200s", got, want)
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if !bytes.Equal(ga, gb) {
		t.Fatalf("JSON differs:\n got %.600s\nwant %.600s", got, want)
	}
}

// roleOf decodes a role_add/set/show answer.
func roleOf(t *testing.T, text string) contract.RoleView {
	t.Helper()
	v, err := contract.ParseRoleResponse([]byte(text), adapter.Lookup())
	if err != nil {
		t.Fatalf("role answer %.300s: %v", text, err)
	}
	return v
}
