//go:build linux || darwin

package function

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/plane"
	"github.com/wedevwork/callsheet/internal/sidecar"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

// TestControlNativeGroups is iteration 06a's direct real-binary
// qualification of guardian-owned process groups (not an FP; required
// native evidence on Linux and macOS). Each scenario starts exactly one
// sidecar fixture (the sidecar package's test binary running a production
// Run whose only change is a probe-free fake adapter, so it starts no
// readiness child), which starts one guardian (the callsheet binary),
// which starts one fake CLI leader (the fake adapter's group mode), which
// starts one fake descendant: four children per scenario, sixteen per
// invocation, asserted. The plane is in process; the orphan-restart
// replacement sidecar is this test process (sidecar.Run in process, fake
// adapter disabled: no child). Every child is gone when a scenario ends.
func TestControlNativeGroups(t *testing.T) {
	ledger := &nativeLedger{}
	t.Cleanup(func() {
		if n := ledger.total(); !t.Failed() && n != 16 {
			t.Errorf("native ledger: %d children, want 16", n)
		}
	})
	t.Run("cooperative", func(t *testing.T) {
		t.Parallel()
		// Run shutdown asks the guardian to clean its group: TERM reaches
		// the leader (its wait status) and the descendant, which exits; the
		// whole group completes and is proved gone.
		r := startNativeRig(t, ledger, fakeadapter.TermExit)
		start := time.Now()
		r.fixture.Process.Signal(syscall.SIGTERM)
		if code := r.fixtureExit(t); code != 130 {
			t.Fatalf("fixture exit %d:\n%s", code, r.logs.String())
		}
		res := r.journalResult(t)
		if res.Outcome != contract.OutcomeLost || res.Signal == nil || *res.Signal != "SIGTERM" {
			t.Fatalf("interrupted result %+v", res)
		}
		if el := time.Since(start); el < time.Second {
			t.Fatalf("the group was reported gone after %v, before the 1 s grace", el)
		}
		r.allGone(t)
	})
	t.Run("resistant", func(t *testing.T) {
		t.Parallel()
		// The leader exits on TERM; its descendant ignores TERM and is
		// killed with the group at the end of the grace (KILL to the
		// guardian's own group, the guardian included).
		r := startNativeRig(t, ledger, fakeadapter.TermIgnore)
		start := time.Now()
		r.fixture.Process.Signal(syscall.SIGTERM)
		if code := r.fixtureExit(t); code != 130 {
			t.Fatalf("fixture exit %d:\n%s", code, r.logs.String())
		}
		if el := time.Since(start); el < time.Second {
			t.Fatalf("a TERM-resistant descendant was gone after %v", el)
		}
		if res := r.journalResult(t); res.Signal == nil || *res.Signal != "SIGTERM" {
			t.Fatalf("leader status %+v", res)
		}
		r.allGone(t)
	})
	t.Run("orphan-restart", func(t *testing.T) {
		t.Parallel()
		// The sidecar fixture is SIGKILLed: its guardian survives,
		// reparented, still leading its group (same PID, PGID = PID), and
		// starts its own cleanup (its parent-lifetime pipe ended). This
		// test process becomes the replacement sidecar: it recovers the
		// journal, asks the authentic guardian to stop through its FIFO
		// (or finds the group gone), reports the execution lost and the
		// group ends in ESRCH.
		r := startNativeRig(t, ledger, fakeadapter.TermIgnore)
		// The plane received output before the crash: the recovered lost
		// outcome reports less than that (its journal predates it) and
		// must still resolve the execution, keeping that output.
		started := "native started pid=" + strconv.Itoa(r.info.LeaderPID) + "\n"
		r.awaitTask(t, func(v contract.TaskView) bool { return strings.Contains(v.LogTail, started) })
		r.fixture.Process.Signal(syscall.SIGKILL)
		r.fixtureExit(t)
		if err := syscall.Kill(r.guardian, 0); err == nil {
			if pp := parentPID(r.guardian); pp == r.fixture.Process.Pid || pp <= 0 {
				t.Fatalf("guardian %d not reparented (ppid %d)", r.guardian, pp)
			}
			if pg, _ := unix.Getpgid(r.guardian); pg != r.guardian {
				t.Fatalf("surviving guardian %d leads group %d", r.guardian, pg)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- sidecar.Run(ctx, sidecar.RunOptions{StateDir: r.state, SoftwareVersion: "native-replacement",
				Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), GOOS: runtime.GOOS})
		}()
		defer func() {
			cancel()
			select {
			case <-done:
			case <-time.After(nodeWait):
				t.Error("the replacement sidecar did not stop")
			}
		}()
		v := r.awaitTask(t, func(v contract.TaskView) bool { return v.State == contract.TaskLost })
		if v.Reason == nil || v.Reason.Code != contract.ReasonWorkerLost || !strings.Contains(v.LogTail, started) || !v.Log.Incomplete {
			t.Fatalf("recovered task %+v", v)
		}
		r.allGone(t)
	})
	t.Run("plane-restart", func(t *testing.T) {
		t.Parallel()
		// Only the plane closes and reopens: the exact same leader PID
		// keeps running, then produces output and exits; its result is the
		// task's (no lost decision, no replay, no cleanup signal before).
		r := startNativeRig(t, ledger, fakeadapter.TermExit)
		r.plane.stop(t)
		r.plane.start(t)
		r.awaitOnline(t)
		if err := syscall.Kill(r.info.LeaderPID, 0); err != nil {
			t.Fatalf("the leader did not survive the plane restart: %v", err)
		}
		os.WriteFile(filepath.Join(r.groupDir, fakeadapter.GroupTrigger), nil, 0o644)
		v := r.awaitTask(t, func(v contract.TaskView) bool { return contract.TaskTerminal(v.State) })
		if v.State != contract.TaskSucceeded || !strings.Contains(v.LogTail, "native output pid="+strconv.Itoa(r.info.LeaderPID)+"\n") {
			t.Fatalf("after the plane restart %+v", v)
		}
		r.fixture.Process.Signal(syscall.SIGTERM)
		if code := r.fixtureExit(t); code != 130 {
			t.Fatalf("fixture exit %d", code)
		}
		r.allGone(t)
	})
}

// sidecarFixtureEnv selects the sidecar package test binary's fixture
// mode (internal/sidecar's FixtureEnv, fixture_unix_test.go).
const sidecarFixtureEnv = "CALLSHEET_SIDECAR_FIXTURE"

// nativeLedger counts the children of every scenario.
type nativeLedger struct {
	mu sync.Mutex
	n  int
}

func (l *nativeLedger) add(k int) {
	l.mu.Lock()
	l.n += k
	l.mu.Unlock()
}

func (l *nativeLedger) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// fixedPlane is an in-process plane on a fixed loopback port, so a
// restart listens where the sidecar enrolled.
type fixedPlane struct {
	root, bind, url, ca string
	cancel              context.CancelFunc
	done                chan error
}

func (p *fixedPlane) start(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(nodeWait)
	for {
		ll := &listenLog{addr: make(chan string, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- plane.Run(ctx, plane.RunOptions{StateDir: p.root, Bind: p.bind, BindSet: true, SANs: []string{"127.0.0.1"}, SANsSet: true, Logger: slog.New(ll)})
		}()
		select {
		case <-ll.addr:
			p.cancel, p.done = cancel, done
			return
		case err := <-done:
			cancel()
			if time.Now().After(deadline) {
				t.Fatalf("plane on %s: %v", p.bind, err)
			}
			time.Sleep(50 * time.Millisecond) // the released port is briefly unavailable
		case <-time.After(nodeWait):
			cancel()
			t.Fatal("the plane did not listen")
		}
	}
}

func (p *fixedPlane) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(nodeWait):
		t.Fatal("the plane did not stop")
	}
}

func startFixedPlane(t *testing.T) *fixedPlane {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bind := ln.Addr().String()
	ln.Close()
	p := &fixedPlane{root: filepath.Join(t.TempDir(), "plane"), bind: bind, url: "https://" + bind}
	p.ca = filepath.Join(p.root, "pki", "ca.crt")
	p.start(t)
	t.Cleanup(func() {
		p.cancel()
		<-p.done
	})
	return p
}

// safeBuffer is a concurrency-safe log sink.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// nativeRig is one scenario: the plane, the enrolled fixture state, the
// running fixture and its task's group.
type nativeRig struct {
	plane    *fixedPlane
	cl       *client.Client
	state    string
	node     string
	groupDir string
	fixture  *exec.Cmd
	exited   chan int
	logs     *safeBuffer
	taskID   string
	info     fakeadapter.GroupInfo
	guardian int
	pids     []int
}

// startNativeRig serves a plane, enrolls and starts the sidecar fixture,
// installs a fake role, dispatches one group-mode task and waits until
// its guardian released the leader and the descendant is ready. It
// asserts the group's identity and the four-child ledger.
func startNativeRig(t *testing.T, ledger *nativeLedger, descendantTerm string) *nativeRig {
	t.Helper()
	bg := context.Background()
	r := &nativeRig{plane: startFixedPlane(t), state: filepath.Join(t.TempDir(), "sidecar"), groupDir: t.TempDir(), logs: &safeBuffer{}, exited: make(chan int, 1)}
	e, err := sidecar.Enroll(bg, sidecar.EnrollOptions{StateDir: r.state, PlaneURL: r.plane.url, CAFile: r.plane.ca, SoftwareVersion: "native"})
	if err != nil {
		t.Fatal(err)
	}
	r.node = e.NodeID
	pem, err := os.ReadFile(r.plane.ca)
	if err != nil {
		t.Fatal(err)
	}
	if r.cl, err = client.New(r.plane.url, client.Trust{CAPEM: pem}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.cl.Close)
	cfg, _ := json.Marshal(struct {
		StateDir    string `json:"state_dir"`
		FakeAdapter string `json:"fake_adapter"`
		Guardian    string `json:"guardian"`
	}{r.state, fakeAdapterBinary(t), nodeBinary(t)})
	r.fixture = exec.Command(contractBinary(t, "./internal/sidecar"))
	r.fixture.Env = append(os.Environ(), sidecarFixtureEnv+"="+string(cfg), fakeadapter.EnvTaskMode+"=group",
		fakeadapter.EnvTaskGroupDir+"="+r.groupDir, fakeadapter.EnvTaskDescendantTerm+"="+descendantTerm)
	r.fixture.Stdout, r.fixture.Stderr = r.logs, r.logs
	if err := r.fixture.Start(); err != nil {
		t.Fatal(err)
	}
	ledger.add(1)
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
		// Never leave a child behind: the fixture, then anything of the
		// group still alive (a failed scenario).
		r.fixture.Process.Kill()
		select {
		case c := <-r.exited:
			r.exited <- c
		case <-time.After(nodeWait):
			t.Error("the sidecar fixture was not reaped")
		}
		for _, pid := range r.pids {
			if syscall.Kill(pid, 0) == nil {
				t.Errorf("process %d outlived its scenario", pid)
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	r.awaitOnline(t)
	dir := t.TempDir()
	ins, run := filepath.Join(dir, "instruction.md"), filepath.Join(dir, "runbook.md")
	os.WriteFile(ins, []byte("native instruction\n"), 0o644)
	os.WriteFile(run, []byte("native runbook\n"), 0o644)
	if _, err := r.cl.AddRole(bg, contract.RoleConfig{ID: "native", Name: "native", Node: r.node, Adapter: "fake", Instruction: ins, Runbook: run,
		Model: "example model", Effort: "medium", Concurrency: 1}); err != nil {
		t.Fatalf("role add: %v\n%s", err, r.logs.String())
	}
	poll(t, "role ready", func() bool {
		v, err := r.cl.ShowRole(bg, "native")
		return err == nil && v.CanAccept
	})
	dispatched := time.Now()
	v, err := r.cl.Dispatch(bg, contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "native"}, Goal: "native group",
		Payload: []string{}, Acceptance: "group", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "test", Hostname: "native"}})
	if err != nil {
		t.Fatal(err)
	}
	r.taskID = v.TaskID
	r.awaitTask(t, func(v contract.TaskView) bool { return v.State == contract.TaskRunning })
	prep := time.Since(dispatched)
	t.Logf("native launch (dispatch to running, the durable launch barriers included): %v", prep)
	if prep >= 10*time.Second {
		t.Fatalf("a healthy native launch took %v, not inside the 10 s preparation budget", prep)
	}
	poll(t, "group.json", func() bool {
		b, err := os.ReadFile(filepath.Join(r.groupDir, fakeadapter.GroupFile))
		return err == nil && json.Unmarshal(b, &r.info) == nil
	})
	var owner contract.OwnerRecord
	poll(t, "owner released", func() bool {
		b, err := os.ReadFile(filepath.Join(r.state, "tasks", r.taskID, "owner.json"))
		if err != nil {
			return false
		}
		o, err := contract.ParseOwner(b)
		owner = o
		return err == nil && o.Phase == contract.OwnerReleased
	})
	r.guardian = owner.GuardianPID
	fixture := r.fixture.Process.Pid
	r.pids = []int{r.guardian, r.info.LeaderPID, r.info.DescendantPID}
	// Identity: the guardian leads a new group (PGID = PID, not the
	// fixture's), the leader and its descendant are members, nobody
	// started a session, and the leader is the guardian's child.
	fpg, _ := unix.Getpgid(fixture)
	fsid, _ := unix.Getsid(fixture)
	for name, c := range map[string]struct{ pid, pgid int }{
		"guardian":   {r.guardian, r.guardian},
		"leader":     {r.info.LeaderPID, r.guardian},
		"descendant": {r.info.DescendantPID, r.guardian},
	} {
		pg, err := unix.Getpgid(c.pid)
		sid, serr := unix.Getsid(c.pid)
		if err != nil || serr != nil || pg != c.pgid || pg == fpg || sid != fsid {
			t.Fatalf("%s %d: pgid %d (want %d, fixture %d), sid %d (fixture %d): %v %v", name, c.pid, pg, c.pgid, fpg, sid, fsid, err, serr)
		}
	}
	if pp := parentPID(r.info.LeaderPID); pp != r.guardian {
		t.Fatalf("leader's parent %d, want the guardian %d", pp, r.guardian)
	}
	if pp := parentPID(r.guardian); pp != fixture {
		t.Fatalf("guardian's parent %d, want the fixture %d", pp, fixture)
	}
	// One guardian (one owned task directory), one leader, one descendant.
	entries, _ := os.ReadDir(filepath.Join(r.state, "tasks"))
	if len(entries) != 1 || r.info.DescendantPID <= 1 || r.info.LeaderPID == r.info.DescendantPID {
		t.Fatalf("ledger: %d task directories, group %+v", len(entries), r.info)
	}
	ledger.add(3)
	return r
}

// awaitOnline waits until the node is online (a fresh attachment's first
// heartbeat).
func (r *nativeRig) awaitOnline(t *testing.T) {
	t.Helper()
	poll(t, "node online", func() bool {
		n, err := r.cl.ShowNode(context.Background(), r.node)
		return err == nil && n.Liveness == contract.LivenessOnline
	})
}

// awaitTask polls the task until ok.
func (r *nativeRig) awaitTask(t *testing.T, ok func(contract.TaskView) bool) contract.TaskView {
	t.Helper()
	var v contract.TaskView
	poll(t, "task state", func() bool {
		var err error
		v, err = r.cl.ShowTask(context.Background(), r.taskID, contract.DefaultTailLines)
		return err == nil && ok(v)
	})
	return v
}

// fixtureExit waits for the fixture's exit code.
func (r *nativeRig) fixtureExit(t *testing.T) int {
	t.Helper()
	select {
	case c := <-r.exited:
		r.exited <- c
		return c
	case <-time.After(nodeWait):
		t.Fatalf("the sidecar fixture did not exit:\n%s", r.logs.String())
		return -1
	}
}

// journalResult is the task's journaled outcome.
func (r *nativeRig) journalResult(t *testing.T) contract.TaskResultBody {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.state, "tasks", r.taskID, "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	var j struct {
		Phase  string                   `json:"phase"`
		Result *contract.TaskResultBody `json:"result"`
	}
	if err := json.Unmarshal(b, &j); err != nil || j.Result == nil {
		t.Fatalf("journal %s: %v", b, err)
	}
	return *j.Result
}

// allGone requires the guardian, the leader and the descendant gone and
// the group itself absent (ESRCH).
func (r *nativeRig) allGone(t *testing.T) {
	t.Helper()
	poll(t, "the group gone", func() bool {
		return errors.Is(syscall.Kill(-r.guardian, 0), syscall.ESRCH)
	})
	for _, pid := range r.pids {
		poll(t, "process "+strconv.Itoa(pid)+" gone", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	}
}

// poll waits (bounded, real time) until cond holds.
func poll(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(nodeWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not observed within %v", what, nodeWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
