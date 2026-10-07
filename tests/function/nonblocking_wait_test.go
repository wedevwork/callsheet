//go:build linux || darwin

package function

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/mcp"
	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Design nonblocking-coordinator-waits: exactly one function parent per
// FP (FP-1..FP-8), each with independent fixtures (t.Parallel, like the
// package's other parents). They drive the built callsheet and mcpqual
// executables against real TLS planes and the scripted worker, and
// delegate the exhaustive injected clock and error matrices to the package
// contracts (TestWaitUntilDoneContract in internal/client,
// TestUntilDoneCommand in internal/cli) through their compiled test
// binaries. No vendor executable or model is ever launched.
//
// Platform semantics relied on (Linux and macOS alike): the CLI's
// signal.NotifyContext turns SIGINT and SIGTERM into context cancellation
// (exit 130); a killed child's descriptors close at exit, so its plane
// connection ends; os/exec reaps the child in Wait, after which signal 0
// to its PID fails. Temporary paths are compared through
// filepath.EvalSymlinks (macOS /var is /private/var).

// nbwRig is one parent's plane (with its wait cap), scripted worker and
// role, a TCP relay in front of the plane (end-to-end TLS with the plane's
// own CA; one accepted connection per attempt is the event that arms a
// step), and an HTTP proxy for injected answers.
type nbwRig struct {
	p     *mcpPlane
	w     *mcpWorker
	relay *tcpRelay
	role  string
}

func startNBW(t *testing.T, maxWait time.Duration, role string) *nbwRig {
	t.Helper()
	p := startMCPPlane(t, maxWait)
	w := startMCPWorker(t, p, nodeIDN('e'))
	p.addRole(t, role, role, w.id, 8)
	r := &nbwRig{p: p, w: w, relay: startTCPRelay(t, strings.TrimPrefix(p.url, "https://")), role: role}
	poll(t, role+" can accept", func() bool {
		v, err := p.cl.ShowRole(context.Background(), role)
		return err == nil && v.CanAccept
	})
	return r
}

// trust is the relayed plane URL with the plane's own CA (its certificate
// names 127.0.0.1, whatever the port).
func (r *nbwRig) trust() []string {
	return []string{"--plane", "https://" + r.relay.addr, "--ca", r.p.ca}
}

// dispatch admits goal (a worker script: "quick ..." ends at once, "hold
// ..." runs until released).
func (r *nbwRig) dispatch(t *testing.T, goal string) string {
	t.Helper()
	v, err := r.p.cl.Dispatch(context.Background(), contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: r.role},
		Goal: goal, Payload: []string{}, Acceptance: "a", RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "test", Hostname: "nbw"}})
	if err != nil {
		t.Fatal(err)
	}
	return v.TaskID
}

// held admits n held tasks and awaits their start on the worker.
func (r *nbwRig) held(t *testing.T, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := r.dispatch(t, "hold nbw")
		r.w.await(t, "started "+id)
		ids = append(ids, id)
	}
	return ids
}

func (r *nbwRig) durable(t *testing.T, id string) contract.TaskView {
	t.Helper()
	return r.p.awaitTask(t, id, func(v contract.TaskView) bool { return contract.TaskTerminal(v.State) && v.DurabilityConfirmed })
}

// cli runs the built callsheet to its exit.
func (r *nbwRig) cli(t *testing.T, args ...string) result {
	t.Helper()
	return runBin(t, r.p.cli.bin, r.p.cli.cwd, r.p.cli.env, args...)
}

// tcpRelay forwards TCP connections to target, recording "accepted",
// "connected" (the target answered) and "refused" (it did not) per
// connection. It never terminates TLS.
type tcpRelay struct {
	addr   string
	target string
	events *evset
	ln     net.Listener
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  []net.Conn
}

func startTCPRelay(t *testing.T, target string) *tcpRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &tcpRelay{addr: ln.Addr().String(), target: target, events: newEvset(), ln: ln}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.events.add("accepted")
			up, err := net.Dial("tcp", r.target)
			if err != nil {
				r.events.add("refused")
				c.Close()
				continue
			}
			r.events.add("connected")
			r.mu.Lock()
			r.conns = append(r.conns, c, up)
			r.mu.Unlock()
			r.wg.Add(2)
			go func() { defer r.wg.Done(); io.Copy(up, c); up.(*net.TCPConn).CloseWrite() }()
			go func() { defer r.wg.Done(); io.Copy(c, up); c.(*net.TCPConn).CloseWrite() }()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		r.mu.Lock()
		for _, c := range r.conns {
			c.Close()
		}
		r.mu.Unlock()
		r.wg.Wait()
	})
	return r
}

func (r *tcpRelay) await(t *testing.T, ev string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if !r.events.take(ev, mcpWait) {
			t.Fatalf("relay never saw %s %d (events %v)", ev, i+1, r.events.snapshot())
		}
	}
}

// waitChild is one background CLI child: its outputs, its exit
// (recorded when its Wait returned) and its PID.
type waitChild struct {
	cmd         *exec.Cmd
	out, errOut *safeBuffer
	done        chan struct{}
	exitAt      time.Time
	code        int
}

func startChild(t *testing.T, bin string, dir string, env []string, args ...string) *waitChild {
	t.Helper()
	c := &waitChild{cmd: exec.Command(bin, args...), out: &safeBuffer{}, errOut: &safeBuffer{}, done: make(chan struct{})}
	c.cmd.Dir, c.cmd.Env, c.cmd.Stdout, c.cmd.Stderr = dir, env, c.out, c.errOut
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		c.cmd.Wait()
		c.exitAt = time.Now()
		c.code = c.cmd.ProcessState.ExitCode()
		close(c.done)
	}()
	t.Cleanup(func() {
		c.cmd.Process.Kill()
		select {
		case <-c.done:
		case <-time.After(mcpWait):
			t.Errorf("child %d was not reaped", c.cmd.Process.Pid)
		}
	})
	return c
}

func (r *nbwRig) child(t *testing.T, args ...string) *waitChild {
	t.Helper()
	return startChild(t, r.p.cli.bin, r.p.cli.cwd, r.p.cli.env, args...)
}

func (c *waitChild) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// wait awaits the child's exit (bounded) and returns its code.
func (c *waitChild) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(mcpWait):
	}
	if !c.exited() {
		t.Fatalf("the wait child did not exit (stdout %q, stderr %q)", c.out.String(), c.errOut.String())
	}
	return c.code
}

// silent requires a running child that printed nothing.
func (c *waitChild) silent(t *testing.T, when string) {
	t.Helper()
	if c.exited() || c.out.String() != "" || c.errOut.String() != "" {
		t.Fatalf("%s: the wait child ended or printed (exit %v, stdout %q, stderr %q)", when, c.exited(), c.out.String(), c.errOut.String())
	}
}

var winnerLine = regexp.MustCompile(`^winner: t_[0-9a-f]{32}\n`)

// FP-1: the opt-in unbounded any-of wait. An already terminal task wins at
// once; of two held tasks the released second one wins; a mixed list with
// an unknown ID fails as a whole (also when the known one is terminal);
// conflicting flags are refused before any request; the bounded mode is
// unchanged. The parsing and help matrix is the CLI contract's.
func TestWaitUntilDoneCLI(t *testing.T) {
	t.Parallel()
	r := startNBW(t, 0, "nbw-cli")
	quick := r.dispatch(t, "quick nbw")
	r.durable(t, quick)
	start := time.Now()
	res := r.cli(t, append([]string{"task", "wait", quick, "--until-done"}, r.trust()...)...)
	if res.code != 0 || !strings.HasPrefix(res.stdout, "winner: "+quick+"\n") || res.stderr != "" || time.Since(start) > 10*time.Second {
		t.Fatalf("terminal: %+v", res)
	}
	ids := r.held(t, 2)
	r.relay.events.clear()
	c := r.child(t, append([]string{"task", "wait", ids[0], ids[1], "--until-done"}, r.trust()...)...)
	r.relay.await(t, "connected", 2) // trust, then the first wait attempt
	c.silent(t, "both held")
	r.w.release(ids[1])
	if code := c.wait(t); code != 0 || !strings.HasPrefix(c.out.String(), "winner: "+ids[1]+"\n") || c.errOut.String() != "" {
		t.Fatalf("second of two: %d %q %q", code, c.out.String(), c.errOut.String())
	}
	unknown := "t_" + strings.Repeat("f", 32)
	for _, list := range [][]string{{ids[0], unknown}, {quick, unknown}} {
		res := r.cli(t, append(append([]string{"task", "wait"}, list...), append([]string{"--until-done"}, r.trust()...)...)...)
		if res.code != 3 || res.stdout != "" || !strings.HasPrefix(res.stderr, "callsheet: not_found: ") {
			t.Fatalf("mixed %v: %+v", list, res)
		}
	}
	if res := r.cli(t, append([]string{"task", "wait", ids[0], "--until-done", "--wait", "0"}, r.trust()...)...); res.code != 2 || res.stdout != "" {
		t.Fatalf("conflict %+v", res)
	}
	res = r.cli(t, append([]string{"task", "wait", ids[0], "--wait", "0"}, r.trust()...)...)
	if res.code != 0 || !strings.HasPrefix(res.stdout, "TASK_ID\tSTATE\tELAPSED_MS\tLAST_LOG_LINE\tLOG_TRUNCATED\tDURABILITY_CONFIRMED\n"+ids[0]+"\trunning\t") {
		t.Fatalf("bounded mode %+v", res)
	}
	r.w.release(ids[0])
	out := contractRun(t, contractBinary(t, "./internal/cli"), "./internal/cli", "^TestUntilDoneCommand$", os.Environ(),
		"TestUntilDoneCommand", "TestUntilDoneCommand/help", "TestUntilDoneCommand/parsing", "TestUntilDoneCommand/bounded-unchanged")
	if !strings.Contains(out, "--- PASS: TestUntilDoneCommand (") {
		t.Fatal(out)
	}
}

// FP-2: renewed bounded requests across a real plane restart. With a
// 100 ms plane cap the child renews its wait again and again; the plane
// stops (the child retries silently while it is down) and restarts on the
// same port with the same CA and the durable task; the child, on its
// retained trust, renews against it and returns the task once it ends.
// The injected schedule (slices, pacing, the backoff ramp and reset,
// retained trust, no retention) is the client contract's.
func TestWaitUntilDoneRenewal(t *testing.T) {
	t.Parallel()
	r := startNBW(t, contract.MinMaxTaskWait, "nbw-renew")
	ids := r.held(t, 1)
	c := r.child(t, append([]string{"task", "wait", ids[0], "--until-done", "--json"}, "--plane", "https://"+r.relay.addr, "--ca-fingerprint", r.p.fp)...)
	r.relay.await(t, "connected", 5) // the pinned bootstrap, then four capped wait attempts
	c.silent(t, "renewing")
	entry := r.w.runningEntry(ids[0])
	r.w.detach()
	r.p.stop(t)
	r.relay.await(t, "refused", 2)
	c.silent(t, "plane down")
	r.p.start(t)
	r.w.attach(t, []contract.TaskInventoryEntry{entry})
	r.relay.events.clear()
	r.relay.await(t, "connected", 2) // renewed against the restarted plane
	c.silent(t, "after the restart")
	if v, err := r.p.cl.ShowTask(context.Background(), ids[0], 0); err != nil || v.State != contract.TaskRunning || !v.DurabilityConfirmed {
		t.Fatalf("the task did not survive the restart durably: %+v %v", v.State, err)
	}
	r.w.release(ids[0])
	if code := c.wait(t); code != 0 || c.errOut.String() != "" {
		t.Fatalf("renewal exit %d %q", code, c.errOut.String())
	}
	w, err := contract.ParseWaitResponse([]byte(strings.TrimSuffix(c.out.String(), "\n")), ids, 30*time.Second)
	if err != nil || w.Status != contract.WaitTerminal || w.Winner != ids[0] || w.EffectiveWaitMS > 100 {
		t.Fatalf("renewal answer %q %v", c.out.String(), err)
	}
	contractRun(t, contractBinary(t, "./internal/client"), "./internal/client", "^TestWaitUntilDoneContract$", os.Environ(),
		"TestWaitUntilDoneContract", "TestWaitUntilDoneContract/bounds", "TestWaitUntilDoneContract/renewal", "TestWaitUntilDoneContract/pacing",
		"TestWaitUntilDoneContract/backoff", "TestWaitUntilDoneContract/trust")
}

// FP-3: permanent errors end the wait with their exact code, one
// diagnostic and no winner; SIGINT and SIGTERM end a waiting child (and
// one retrying an unreachable plane) with 130 within 1 s, without
// cancelling the task, and leave no process behind. The retry class and
// cancellation at every await are the client contract's.
func TestWaitUntilDoneFailure(t *testing.T) {
	t.Parallel()
	r := startNBW(t, 0, "nbw-fail")
	ids := r.held(t, 1)
	x := startPlaneProxy(t, r.p)
	foreign := filepath.Join(t.TempDir(), "foreign-ca.crt")
	if b, err := os.ReadFile(x.ca); err != nil || os.WriteFile(foreign, b, 0o644) != nil {
		t.Fatal(err)
	}
	// A terminal task makes the plane answer at once (the protocol case
	// needs a plane answer to carry the foreign version).
	quick := r.dispatch(t, "quick nbw")
	r.durable(t, quick)
	permanent := func(name string, want int, args ...string) {
		t.Helper()
		res := r.cli(t, append([]string{"task", "wait", quick, ids[0], "--until-done"}, args...)...)
		if res.code != want || res.stdout != "" || strings.Count(res.stderr, "\n") != 1 || !strings.HasPrefix(res.stderr, "callsheet: ") {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	permanent("trust", 6, "--plane", "https://"+r.relay.addr, "--ca", foreign)
	for _, c := range []struct {
		name string
		e    *contract.Error
		code int
	}{{"conflict", contract.New(contract.CodeConflict, "no"), 4}, {"not-implemented", contract.New(contract.CodeNotImplemented, "no"), 8},
		{"internal", contract.New(contract.CodeInternal, "no"), 1}} {
		x.mu.Lock()
		x.inject["POST /api/v1/tasks/wait"] = c.e
		x.mu.Unlock()
		permanent(c.name, c.code, x.trust()...)
	}
	x.mu.Lock()
	delete(x.inject, "POST /api/v1/tasks/wait")
	x.version = "5"
	x.mu.Unlock()
	permanent("protocol", 7, x.trust()...)
	x.mu.Lock()
	x.version = ""
	x.mu.Unlock()
	// Signals: a child mid-wait, and one retrying a plane that refuses
	// every connection (its backoff timer is the await).
	dead := startTCPRelay(t, closedAddr(t))
	for _, c := range []struct {
		name  string
		sig   syscall.Signal
		relay *tcpRelay
		ev    string
	}{{"sigint-waiting", syscall.SIGINT, r.relay, "connected"}, {"sigterm-waiting", syscall.SIGTERM, r.relay, "connected"}, {"sigterm-retrying", syscall.SIGTERM, dead, "refused"}} {
		c.relay.events.clear()
		ch := r.child(t, "task", "wait", ids[0], "--until-done", "--plane", "https://"+c.relay.addr, "--ca", r.p.ca)
		c.relay.await(t, c.ev, 2)
		ch.silent(t, c.name)
		sent := time.Now()
		if err := ch.cmd.Process.Signal(c.sig); err != nil {
			t.Fatal(err)
		}
		code := ch.wait(t)
		if took := ch.exitAt.Sub(sent); code != 130 || took > time.Second || ch.out.String() != "" || ch.errOut.String() != "callsheet: interrupted\n" {
			t.Fatalf("%s: exit %d after %v, %q %q", c.name, code, took, ch.out.String(), ch.errOut.String())
		}
		if err := syscall.Kill(ch.cmd.Process.Pid, 0); err == nil {
			t.Fatalf("%s: the observer survives", c.name)
		}
	}
	if v, err := r.p.cl.ShowTask(context.Background(), ids[0], 0); err != nil || contract.TaskTerminal(v.State) || v.StopRequested {
		t.Fatalf("an interrupted wait touched the task: %+v %v", v.State, err)
	}
	r.w.release(ids[0])
	contractRun(t, contractBinary(t, "./internal/client"), "./internal/client", "^TestWaitUntilDoneContract$/^(retry-class|cancellation|validation)$", os.Environ(),
		"TestWaitUntilDoneContract/retry-class", "TestWaitUntilDoneContract/cancellation", "TestWaitUntilDoneContract/cancellation/permanent-outage",
		"TestWaitUntilDoneContract/validation")
	contractRun(t, contractBinary(t, "./internal/cli"), "./internal/cli", "^TestUntilDoneCommand$/^(errors|cancellation)$", os.Environ(),
		"TestUntilDoneCommand/errors", "TestUntilDoneCommand/cancellation")
}

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

// FP-4: nothing on stdout or stderr until the barrier-released winner,
// across several renewed slices; then exactly the winner line and the task
// as task show renders it, or exactly one terminal JSON envelope; never a
// still_running row. Retry silence is the CLI and client contracts'.
func TestWaitUntilDoneOutput(t *testing.T) {
	t.Parallel()
	r := startNBW(t, contract.MinMaxTaskWait, "nbw-out")
	ids := r.held(t, 2)
	r.relay.events.clear()
	text := r.child(t, append([]string{"task", "wait", ids[0], "--until-done"}, r.trust()...)...)
	js := r.child(t, append([]string{"task", "wait", ids[1], "--until-done", "--json"}, r.trust()...)...)
	r.relay.await(t, "connected", 8) // both trusts and several capped slices each
	text.silent(t, "text, held")
	js.silent(t, "json, held")
	r.w.release(ids[0])
	r.w.release(ids[1])
	if text.wait(t) != 0 || js.wait(t) != 0 {
		t.Fatalf("exits %d %d: %q %q", text.code, js.code, text.errOut.String(), js.errOut.String())
	}
	show := r.cli(t, append([]string{"task", "show", ids[0]}, r.trust()...)...)
	if show.code != 0 || text.out.String() != "winner: "+ids[0]+"\n"+show.stdout || !winnerLine.MatchString(text.out.String()) || text.errOut.String() != "" {
		t.Fatalf("text answer %q, task show %q", text.out.String(), show.stdout)
	}
	out := js.out.String()
	w, err := contract.ParseWaitResponse([]byte(strings.TrimSuffix(out, "\n")), ids[1:], 30*time.Second)
	if err != nil || strings.Count(out, "\n") != 1 || w.Status != contract.WaitTerminal || w.Winner != ids[1] || js.errOut.String() != "" {
		t.Fatalf("json answer %q %v", out, err)
	}
	for _, s := range []string{text.out.String(), out} {
		if strings.Contains(s, "TASK_ID\t") || strings.Contains(s, contract.WaitStillRunning) {
			t.Fatalf("an intermediate row: %q", s)
		}
	}
	// A winner whose stdout is a pipe without a reader (fd 1 of a real
	// pipe, its read end closed): exit 1 with the diagnostic on stderr,
	// never death by SIGPIPE (review C3).
	quick := r.dispatch(t, "quick nbw")
	r.durable(t, quick)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pr.Close()
	broken := exec.Command(r.p.cli.bin, append([]string{"task", "wait", quick, "--until-done"}, r.trust()...)...)
	broken.Dir, broken.Env, broken.Stdout = r.p.cli.cwd, r.p.cli.env, pw
	var berr safeBuffer
	broken.Stderr = &berr
	runErr := broken.Run()
	pw.Close()
	if broken.ProcessState == nil || broken.ProcessState.ExitCode() != 1 || !strings.Contains(berr.String(), "callsheet: internal: cannot write to stdout") {
		t.Fatalf("closed stdout pipe: %v, stderr %q", runErr, berr.String())
	}
	contractRun(t, contractBinary(t, "./internal/cli"), "./internal/cli", "^TestUntilDoneCommand$/^(text|json|silent-retry|write-failure|sigpipe)$", os.Environ(),
		"TestUntilDoneCommand/text", "TestUntilDoneCommand/json", "TestUntilDoneCommand/silent-retry", "TestUntilDoneCommand/write-failure",
		"TestUntilDoneCommand/sigpipe")
}

// guideSection returns the "## title" section of docs/coordinator.md.
func guideSection(t *testing.T, doc, title string) string {
	t.Helper()
	i := strings.Index(doc, "\n## "+title+"\n")
	if i < 0 {
		t.Fatalf("docs/coordinator.md has no %q section", title)
	}
	s := doc[i+1:]
	if j := strings.Index(s[3:], "\n## "); j >= 0 {
		s = s[:j+4]
	}
	return s
}

// The per-client rows carry, inline, the evidence summary of the design's
// checked-in capability check (design/iterations/nonblocking-coordinator-
// waits/design-check/coordinator-background-wait-2026-10-06.md: bundled
// skills, help text and binary strings, no model calls), which shipped
// docs never link.
//
// FP-5: the guide's runbook example (the background wait, its exit codes
// and the gone-for-good contract, the steps, the single-line wrapper and
// per-client support with its evidence and fallback, linked from the
// README and the catalog), then a local process harness running it: the
// guide's own wrapper as the background command with the real CLI on
// PATH, independent coordinator work while the wait stays silent, the
// release, one handled wake (line and exit together), and a re-arm for
// the remaining task as an exit-notified direct command.
func TestCoordinatorBackgroundWait(t *testing.T) {
	t.Parallel()
	root := testkit.MustRepoRoot(t)
	docb, err := os.ReadFile(filepath.Join(root, "docs", "coordinator.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(docb)
	sec := guideSection(t, doc, "Coordinator runbook example")
	for _, s := range []string{
		"callsheet task wait ID [ID ...] --until-done --json --plane <plane-url> --ca <ca-path>",
		"**A plane gone for good looks like a long outage: the command then waits, silently, until it is stopped.**",
		"SIGINT or SIGTERM exits 130 with `callsheet: interrupted`, on Linux and macOS alike; no task is stopped",
		"`setsid`, `nohup`, `&` or `disown`", "stable batches of at most 16", "never re-arm an empty list",
		"handle them idempotently by wait handle and winner task ID", "A nonzero exit is an error path, never a task success",
		"`^winner: t_[0-9a-f]{32}$`", "Match the notification with `^callsheet-wait-ended: `",
		"| Claude Code 2.1.291 |", "`run_in_background: true`", "| Grok 1.0.46 |", "at most 10 hours", "| Cursor Agent 2026.10.01 (local) |", "`notify_on_output`",
		"| Codex 0.160.0 | **UNVERIFIED wake.**", "static evidence, not a live qualification", "without another user message or a model poll",
		"Headless one-shot runs (`claude -p`, `codex exec`, `grok -p` and their Cursor equivalent) end with their turn",
		"Linux evidence does not certify macOS", "Callsheet never reads, stores or registers a runbook",
	} {
		if !strings.Contains(sec, s) {
			t.Fatalf("the runbook example lacks %q", s)
		}
	}
	if strings.Contains(sec, "design-check") || strings.Contains(sec, "](../design") {
		t.Fatal("the guide links a design artifact")
	}
	readme, _ := os.ReadFile(filepath.Join(root, "README.md"))
	cat, _ := os.ReadFile(filepath.Join(root, "docs", "support-catalog.md"))
	if !strings.Contains(string(readme), "](docs/coordinator.md#coordinator-runbook-example)") || !strings.Contains(string(cat), "](coordinator.md#coordinator-runbook-example)") {
		t.Fatal("the runbook example is not linked from the README and the catalog")
	}
	fence := regexp.MustCompile("(?s)```sh\n(callsheet task wait .*?)\n```").FindStringSubmatch(sec)
	if fence == nil {
		t.Fatal("no wrapper script in the runbook example")
	}
	wrapper := fence[1]

	r := startNBW(t, 0, "nbw-coord")
	ids := r.held(t, 2)
	bin := mkdir(t, filepath.Join(t.TempDir(), "bin"))
	if err := os.Symlink(r.p.cli.bin, filepath.Join(bin, "callsheet")); err != nil {
		t.Fatal(err)
	}
	wrapperEnv := func(t *testing.T, handle string, ids []string) ([]string, string) {
		t.Helper()
		waitdir, err := filepath.EvalSymlinks(mkdir(t, filepath.Join(t.TempDir(), "wait-"+handle)))
		if err != nil {
			t.Fatal(err)
		}
		return append(append([]string(nil), r.p.cli.env...), "PATH="+bin+":/usr/bin:/bin", "id1="+ids[0], "id2="+ids[1], "plane=https://"+r.relay.addr,
			"ca="+r.p.ca, "handle="+handle, "waitdir="+waitdir), waitdir
	}
	// A forced early exit (review C4): a scenario that ends before
	// releasing anything, as a failed assertion does. Its cleanup stops
	// the wrapper's whole process group (the shell and its foreground
	// callsheet child), joins the harness goroutine and proves the group
	// gone; the wrapper never published a status (an interrupted watcher)
	// and the tasks are untouched.
	early := r.held(t, 2)
	var g *managedWrapper
	var earlyDir string
	t.Run("early-exit-cleanup", func(t *testing.T) {
		r.relay.events.clear()
		var env []string
		env, earlyDir = wrapperEnv(t, "early", early)
		g = startManagedWrapper(t, wrapper, r.p.cli.cwd, env)
		r.relay.await(t, "connected", 2) // the wait is armed; then the early exit
	})
	select {
	case <-g.joined:
	default:
		t.Fatal("the harness goroutine was not joined by the cleanup")
	}
	if err := syscall.Kill(-g.pgid, 0); err != syscall.ESRCH {
		t.Fatalf("the wrapper's process group survives its cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(earlyDir, "exit-code")); err == nil {
		t.Fatal("a stopped wrapper published a status")
	}
	for _, id := range early {
		if v, err := r.p.cl.ShowTask(context.Background(), id, 0); err != nil || contract.TaskTerminal(v.State) || v.StopRequested {
			t.Fatalf("stopping the watcher touched task %s: %+v %v", id, v.State, err)
		}
		r.w.release(id)
	}
	env, waitdir := wrapperEnv(t, "h1", ids)
	var events []string
	// The harness: the wrapper as a background command whose stdout lines
	// notify (matched by the documented regex) and whose exit notifies.
	r.relay.events.clear()
	g = startManagedWrapper(t, wrapper, r.p.cli.cwd, env)
	lines, exited := g.lines, g.exited
	events = append(events, "started")
	r.relay.await(t, "connected", 2) // the wait is armed
	if res := r.cli(t, append([]string{"node", "ls", "--json"}, r.trust()...)...); res.code != 0 || !strings.Contains(res.stdout, r.w.id) {
		t.Fatalf("independent work %+v", res)
	}
	events = append(events, "independent-work")
	select {
	case l := <-lines:
		t.Fatalf("the wrapper notified during independent work: %q", l)
	default:
	}
	if _, err := os.Stat(filepath.Join(waitdir, "exit-code")); err == nil {
		t.Fatal("the wrapper published a status before the release")
	}
	r.w.release(ids[1])
	var line string
	select {
	case line = <-lines:
	case <-time.After(mcpWait):
		t.Fatal("no wake line")
	}
	if !regexp.MustCompile(`^callsheet-wait-ended: `).MatchString(line) || line != "callsheet-wait-ended: h1 exit=0\n" {
		t.Fatalf("wake line %q", line)
	}
	events = append(events, "notified")
	handled := map[string]bool{}
	handle := func(h string, out []byte, ids []string) string {
		t.Helper()
		w, err := contract.ParseWaitResponse(out, ids, 30*time.Second)
		if err != nil || w.Status != contract.WaitTerminal {
			t.Fatalf("%s: %q %v", h, out, err)
		}
		if handled[h+"/"+w.Winner] {
			return ""
		}
		handled[h+"/"+w.Winner] = true
		return w.Winner
	}
	res, _ := os.ReadFile(filepath.Join(waitdir, "result.json"))
	code, _ := os.ReadFile(filepath.Join(waitdir, "exit-code"))
	errText, _ := os.ReadFile(filepath.Join(waitdir, "error.txt"))
	if string(code) != "0\n" || len(errText) != 0 {
		t.Fatalf("status %q %q", code, errText)
	}
	if w := handle("h1", res, ids); w != ids[1] {
		t.Fatalf("winner %s, want the released second task", w)
	}
	events = append(events, "handled")
	select {
	case c := <-exited:
		if c != 0 {
			t.Fatalf("wrapper exit %d", c)
		}
	case <-time.After(mcpWait):
		t.Fatal("no exit notification")
	}
	if w := handle("h1", res, ids); w != "" {
		t.Fatal("the exit notification was handled a second time")
	}
	outstanding := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == ids[1] })
	r.relay.events.clear()
	re := r.child(t, append(append([]string{"task", "wait"}, outstanding...), append([]string{"--until-done", "--json"}, r.trust()...)...)...)
	events = append(events, "re-armed")
	r.relay.await(t, "connected", 2)
	re.silent(t, "re-armed")
	r.w.release(ids[0])
	if re.wait(t) != 0 {
		t.Fatalf("re-armed exit %d %q", re.code, re.errOut.String())
	}
	events = append(events, "notified")
	if w := handle("h2", []byte(strings.TrimSuffix(re.out.String(), "\n")), outstanding); w != ids[0] {
		t.Fatalf("re-armed winner %s", w)
	}
	events = append(events, "handled")
	if got := strings.Join(events, ","); got != "started,independent-work,notified,handled,re-armed,notified,handled" {
		t.Fatalf("harness events %s", got)
	}
}

// managedWrapper is the harness's background command: the wrapper shell
// in a process group of its own (with its foreground callsheet child), its
// stdout lines as wake events and its exit as a second notification, read
// and reaped by one goroutine that cleanup joins.
type managedWrapper struct {
	cmd    *exec.Cmd
	pgid   int
	lines  chan string
	exited chan int
	joined chan struct{}
}

func startManagedWrapper(t *testing.T, script, dir string, env []string) *managedWrapper {
	t.Helper()
	m := &managedWrapper{cmd: exec.Command("/bin/sh", "-c", script), lines: make(chan string, 16), exited: make(chan int, 1), joined: make(chan struct{})}
	m.cmd.Dir, m.cmd.Env = dir, env
	m.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := m.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	m.pgid = m.cmd.Process.Pid
	go func() {
		defer close(m.joined)
		// Each stdout line is a wake as it arrives (a monitor harness);
		// the exit is a second notification of the same wait.
		br := bufio.NewReader(stdout)
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				m.lines <- l
			}
			if err != nil {
				break
			}
		}
		m.cmd.Wait()
		m.exited <- m.cmd.ProcessState.ExitCode()
	}()
	t.Cleanup(func() { m.stop(t) })
	return m
}

// stop kills the wrapper's whole process group (its foreground child
// included), joins the harness goroutine and waits (bounded) until no
// process of the group remains.
func (m *managedWrapper) stop(t *testing.T) {
	syscall.Kill(-m.pgid, syscall.SIGKILL)
	select {
	case <-m.joined:
	case <-time.After(mcpWait):
		t.Errorf("the wrapper %d was not reaped", m.pgid)
	}
	if err := processgroup.WaitGone(processgroup.SysSignaler{}, processgroup.RealClock{}, 5*time.Second, 10*time.Millisecond, -m.pgid); err != nil {
		t.Errorf("the wrapper's process group %d survives: %v", m.pgid, err)
	}
}

// FP-6: the real server's tools/list keeps the 23 tools in order with flat
// schemas and no until_done argument, and describes the two waiting tools
// as short polls carrying the exact notice once (as do mcp --help and the
// guide); both short paths stay bounded: dispatch's wait is the fast path
// for a quick task and task_wait answers still_running within its budget.
func TestMCPShortPollGuidance(t *testing.T) {
	t.Parallel()
	r := startNBW(t, 0, "nbw-mcp")
	m := startMCP(t, append(r.p.trust(), "--wait-call-budget", "1s")...)
	d10 := startMCP(t, r.p.trust()...)
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(m.request("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		waiting := tl.Name == "task_wait" || tl.Name == "dispatch"
		if strings.Contains(string(tl.InputSchema), "until") || !strings.Contains(string(tl.InputSchema), `"additionalProperties":false`) ||
			strings.Count(tl.Description, mcp.ShortPollNotice) != map[bool]int{true: 1, false: 0}[waiting] ||
			waiting && !strings.HasSuffix(tl.Description, "B=1s. "+mcp.BudgetDeferralNote+" "+mcp.ShortPollNotice) {
			t.Fatalf("%s: %q %s", tl.Name, tl.Description, tl.InputSchema)
		}
	}
	if strings.Join(names, ",") != strings.Join(mcp.ToolNames, ",") || len(names) != 23 {
		t.Fatalf("tools %v", names)
	}
	help := r.cli(t, "mcp", "--help")
	guide, _ := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "docs", "coordinator.md"))
	if help.code != 0 || !strings.Contains(help.stdout, mcp.ShortPollNotice) || !strings.Contains(string(guide), "> "+mcp.ShortPollNotice+"\n") {
		t.Fatal("the notice is not in mcp --help and the guide")
	}
	// The default 10s budget: dispatch's wait is the fast path of a quick
	// task; task_wait answers at its own requested short wait.
	quick := d10.ok("dispatch", dispatchArgs("id", r.role, "quick fast", map[string]any{"wait": "5m"}))
	var d struct {
		TaskID     string          `json:"task_id"`
		WaitResult json.RawMessage `json:"wait_result"`
	}
	if err := json.Unmarshal([]byte(quick), &d); err != nil || waitOf(t, string(d.WaitResult), []string{d.TaskID}).Status != contract.WaitTerminal {
		t.Fatalf("fast path %s %v", quick, err)
	}
	ids := r.held(t, 1)
	if w := waitOf(t, d10.ok("task_wait", map[string]any{"task_ids": ids, "wait": "200ms"}), ids); w.Status != contract.WaitStillRunning || w.EffectiveWaitMS != 200 {
		t.Fatalf("short poll %+v", w)
	}
	// A 1s budget caps a 5m request at B minus both reserves.
	start := time.Now()
	w := waitOf(t, m.ok("task_wait", map[string]any{"task_ids": ids, "wait": "5m"}), ids)
	if took := time.Since(start); w.Status != contract.WaitStillRunning || w.EffectiveWaitMS > 500 || took > 5*time.Second {
		t.Fatalf("short poll %+v after %v", w, took)
	}
	r.w.release(ids[0])
}

// shortPollPolicy is the catalog's policy paragraph after its retired
// anchor, verbatim from the design.
const shortPollPolicy = `<a id="interim-mcp-wait-exception"></a>**Retired: short-poll policy.** The default 10s MCP call budget is a deliberately short poll. ` +
	"Long waits use a harness-managed background CLI command. Vendor timeout compatibility is claimed only by named local evidence; an unmeasured client remains UNVERIFIED. " +
	"Increasing the budget requires local timeout qualification with response margin."

// preShortPoll returns facts as they were before the short-poll policy:
// each value ending with the policy gets the retired interim sentence back
// (the design's only change to the catalog subtrees earlier iterations
// froze), so digests frozen against earlier fixtures still prove every
// other byte unchanged. It also returns how many values it restored.
func preShortPoll(facts []catalog.Fact) ([]catalog.Fact, int) {
	out := slices.Clone(facts)
	n := 0
	for i, f := range out {
		if strings.HasSuffix(f.Value, " "+mcpqual.ShortPollPolicy) {
			out[i].Value = strings.TrimSuffix(f.Value, mcpqual.ShortPollPolicy) + mcpqual.LegacyInterimSentence
			n++
		}
	}
	return out, n
}

// legacyCatalog turns repo's catalog back into the retired interim policy.
func legacyCatalog(t *testing.T, repo string) {
	t.Helper()
	for _, rel := range []string{mcpqual.CatalogMDPath, mcpqual.CatalogJSONPath} {
		p := filepath.Join(repo, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(b), "**Retired: short-poll policy.** ", "")
		s = strings.ReplaceAll(s, mcpqual.ShortPollPolicy, mcpqual.LegacyInterimSentence)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// FP-7: the catalog twins validate under the short-poll policy; a real
// fake-vendor qualification publishes its lower bound into a disposable
// new-policy catalog (UNVERIFIED: the decoder fixture is synthetic) and,
// with an eligible (qualified-decoder) report, a VERIFIED lower bound;
// an old-policy catalog and invalid evidence are refused (exit 4).
func TestShortPollCatalogPolicy(t *testing.T) {
	t.Parallel()
	root := testkit.MustRepoRoot(t)
	md, _ := os.ReadFile(filepath.Join(root, mcpqual.CatalogMDPath))
	raw, _ := os.ReadFile(filepath.Join(root, mcpqual.CatalogJSONPath))
	entries, err := catalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkShortPollCatalog(string(md), raw); err != nil || catalog.Validate(root, entries) != nil || catalog.CheckDocLinks(filepath.Join(root, mcpqual.CatalogMDPath)) != nil {
		t.Fatalf("the shipped catalog: %v", err)
	}
	q := newQualEnv(t)
	lower := qualPlan(q.fakeClient("claude", map[string]string{"TIMEOUT_MS": "0"}, map[string]any{"default": map[string]any{"delays_ms": []int{20}}}))
	repo := qualRepo(t)
	out := filepath.Join(q.dir, "out")
	if r := q.qualify(lower, out, "", nil, "--publish-catalog", repo); r.code != 0 || !strings.Contains(r.stdout, "published run") {
		t.Fatalf("publish %+v", r)
	}
	rep := readReport(t, out)
	f := repoFacts(t, repo)["claude"].Facts["mcp_timeout"]
	pmd, _ := os.ReadFile(filepath.Join(repo, mcpqual.CatalogMDPath))
	pjson, _ := os.ReadFile(filepath.Join(repo, mcpqual.CatalogJSONPath))
	if f.Status != catalog.Unverified || !strings.Contains(f.Value, "lower_bound") || strings.Contains(f.Value, mcpqual.ShortPollPolicy) ||
		checkShortPollCatalog(string(pmd), pjson) != nil {
		t.Fatalf("published fact %+v", f)
	}
	// The same run, eligible (a qualified decoder, the catalog's platform):
	// its lower bound is published VERIFIED through the production path.
	eligible := filepath.Join(t.TempDir(), "eligible")
	copyTree(t, out, eligible)
	// Design decoder-enrollment: a qualified record without enrolled
	// evidence is a historical record, never quietly upgraded; explicit
	// fake evidence for the run's platform makes the lower bound eligible.
	rep.Clients[0].DecoderVersion.Qualified = true
	rep.OS, rep.Arch = "linux", "amd64"
	legacyOut := filepath.Join(t.TempDir(), "legacy")
	copyTree(t, out, legacyOut)
	if _, err := mcpqual.ProposePatch(rep, mustBase(t, qualRepo(t)), legacyOut); err != nil {
		t.Fatal(err)
	}
	if legacy, _ := os.ReadFile(filepath.Join(legacyOut, mcpqual.PatchFileName)); !strings.Contains(string(legacy), "without enrolled real-transcript evidence (a historical record)") {
		t.Fatal("a qualified record without evidence was upgraded")
	}
	rep.Clients[0].DecoderVersion.Evidence = []mcpqual.DecoderEvidence{{Platform: "linux/amd64", Fixture: rep.Clients[0].DecoderVersion.Fixture,
		Kinds: []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}}}
	rb, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(eligible, "report.json"), rb, 0o644)
	fresh := qualRepo(t)
	base, err := mcpqual.ReadCatalogBase(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mcpqual.ProposePatch(rep, base, eligible); err != nil {
		t.Fatal(err)
	}
	if err := mcpqual.Publish(eligible, fresh); err != nil {
		t.Fatal(err)
	}
	v := repoFacts(t, fresh)["claude"].Facts["mcp_timeout"]
	vmd, _ := os.ReadFile(filepath.Join(fresh, mcpqual.CatalogMDPath))
	vjson, _ := os.ReadFile(filepath.Join(fresh, mcpqual.CatalogJSONPath))
	if v.Status != catalog.Verified || !strings.Contains(v.Value, "this is a lower bound, not the default") || checkShortPollCatalog(string(vmd), vjson) != nil ||
		!strings.Contains(string(vmd), "- **MCP call timeout: VERIFIED.**") {
		t.Fatalf("verified lower bound %+v", v)
	}
	// An old-policy catalog is refused before anything is installed.
	old := qualRepo(t)
	legacyCatalog(t, old)
	before, _ := os.ReadFile(filepath.Join(old, mcpqual.CatalogJSONPath))
	if r := q.qualify(lower, filepath.Join(q.dir, "out-old"), "", nil, "--publish-catalog", old); r.code != 4 || !strings.Contains(r.stderr, "update catalog policy first") {
		t.Fatalf("old policy %+v", r)
	}
	after, _ := os.ReadFile(filepath.Join(old, mcpqual.CatalogJSONPath))
	if string(before) != string(after) {
		t.Fatal("an old-policy catalog was changed")
	}
	if _, err := os.Stat(filepath.Join(old, mcpqual.EvidenceRoot)); err == nil {
		t.Fatal("evidence installed into an old-policy catalog")
	}
	// Invalid evidence never publishes.
	forged := filepath.Join(t.TempDir(), "forged")
	copyTree(t, out, forged)
	os.WriteFile(filepath.Join(forged, filepath.FromSlash(rep.Evidence[0].Path)), []byte("forged\n"), 0o644)
	if r := runBin(t, qualBinary(t), q.dir, q.env(""), "publish", "--out", forged, "--repo", qualRepo(t)); r.code != 4 {
		t.Fatalf("forged evidence %+v", r)
	}
	q.groupsGone()
}

// FP-8: the four short plan templates validate as templates (the real
// mcpqual refuses them only for their owner placeholders); filled with the
// fake vendor (its silent delay scaled from 15 s to 150 ms, the limits
// alike), the real mcpqual runs setup, one silent call and the repeat of
// that successful bound (three sessions) and reports a lower bound with
// its decision; a first call that times out schedules no repeat and an
// authentication failure measures nothing, each reported as such; the
// guide renders the decision and its example as mcpqual does; the full
// templates still validate.
func TestMCPShortConfirmation(t *testing.T) {
	t.Parallel()
	root := testkit.MustRepoRoot(t)
	guide, _ := os.ReadFile(filepath.Join(root, "docs", "coordinator.md"))
	l := int64(15000)
	// The guide's example is what mcpqual renders for qualified evidence (a
	// decoder qualified by an actual transcript, setup and the repeated
	// bound, a clean run).
	decision := mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, &mcpqual.Report{OS: "linux", Arch: "amd64", Cleanup: mcpqual.CleanupReport{OK: true}}, mcpqual.ClientReport{
		DecoderVersion: &mcpqual.DecoderVersion{Version: "v", Fixture: "claude-json/actual", Qualified: true, Evidence: []mcpqual.DecoderEvidence{{Platform: "linux/amd64",
			Fixture: "claude-json/actual", Kinds: []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}}}},
		Phases: []mcpqual.PhaseReport{{Name: mcpqual.PhaseSetup, Status: mcpqual.StatusConclusive},
			{Name: mcpqual.PhaseDefault, Status: mcpqual.StatusConclusive, LowerBoundMS: &l, Observations: 2}}})
	for _, s := range []string{"B + max(2s, 0.1L) < L", "For the short plan's L = 15s: 10s + max(2s, 1.5s) = 12s < 15s", "### Short confirmation",
		"--plan /absolute/vendor-short.json --out /absolute/evidence-dir --allow-model-calls --publish-catalog /absolute/repo",
		"never use a timeout's upper bound or an interval midpoint as the safe value", "even when mcpqual exits 0 for a conclusive timeout observation"} {
		if !strings.Contains(string(guide), s) {
			t.Fatalf("the guide lacks %q", s)
		}
	}
	if !strings.Contains(decision, "compatible for this measured tuple: L = 15s") || !strings.Contains(decision, "10s + max(2s, 1.5s) = 12s < 15s") {
		t.Fatalf("mcpqual renders %q", decision)
	}
	q := newQualEnv(t)
	plans := filepath.Join(root, "internal", "mcpqual", "testdata", "plans")
	for _, id := range []string{"claude", "codex", "grok", "cursor"} {
		if !strings.Contains(string(guide), "](../internal/mcpqual/testdata/plans/"+id+"-short.json)") {
			t.Fatalf("the %s short plan is not linked", id)
		}
		for _, name := range []string{id + "-short.json", id + ".json"} {
			b, err := os.ReadFile(filepath.Join(plans, name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mcpqual.ValidateTemplate(b, mcpqual.DefaultRegistry()); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		r := runBin(t, qualBinary(t), q.dir, q.env(""), "qualify", "--plan", filepath.Join(plans, id+"-short.json"), "--out", filepath.Join(q.dir, "tmpl-"+id))
		if r.code != 2 || !strings.Contains(r.stderr, "unfilled owner placeholder") {
			t.Fatalf("%s short template %+v", id, r)
		}
	}
	// The claude short plan, filled with the fake vendor and scaled 1:100.
	var short map[string]any
	b, _ := os.ReadFile(filepath.Join(plans, "claude-short.json"))
	if err := json.Unmarshal(b, &short); err != nil {
		t.Fatal(err)
	}
	run := func(name string, settings map[string]string) (*mcpqual.Report, string, int) {
		t.Helper()
		q := newQualEnv(t)
		c := q.fakeClient("claude", settings, map[string]any{"default": map[string]any{"delays_ms": []int{150}}})
		delete(c["config"].(map[string]any), "raised")
		delete(c, "override")
		plan := map[string]any{"version": short["version"], "clients": []any{c},
			"limits": map[string]any{"max_sessions_per_client": 3, "max_case_ms": 1200, "max_client_ms": 3600}}
		out := filepath.Join(q.dir, "out-"+name)
		r := q.qualify(plan, out, "", nil)
		rep := readReport(t, out)
		md, _ := os.ReadFile(filepath.Join(out, "report.md"))
		q.groupsGone()
		return rep, string(md), r.code
	}
	if lim := short["limits"].(map[string]any); lim["max_sessions_per_client"] != float64(3) || lim["max_case_ms"] != float64(120000) || lim["max_client_ms"] != float64(360000) {
		t.Fatalf("short limits %v", lim)
	}
	rep, md, code := run("lower", map[string]string{"TIMEOUT_MS": "0"})
	c := reportClient(t, rep, "claude")
	def := reportPhase(t, c, mcpqual.PhaseDefault)
	if code != 0 || len(c.Phases) != 2 || deref(def.Result) != mcpqual.ResultLowerBound || ms(def.LowerBoundMS) != 150 || def.Observations != 2 || len(def.Cases) != 2 ||
		def.Cases[1].CaseID != "claude-default-repeat" || rep.PlannedUpperBound.Sessions != 3 || !strings.Contains(md, "- Phase default: conclusive lower_bound (> 150 ms tested)") ||
		!strings.Contains(md, "vendor compatibility UNVERIFIED (decoder claude-json/synthetic is a synthetic fixture, not an actual-transcript qualification): measurement only, the lower bound L = 150ms is too short: 10s + max(2s, 15ms) = 12s >= 150ms") {
		t.Fatalf("lower bound: %d %+v\n%s", code, def, md)
	}
	rep, md, code = run("timeout", map[string]string{"TIMEOUT_MS": "50"})
	def = reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseDefault)
	if code != 0 || deref(def.Result) != mcpqual.ResultTimeoutObserved || def.LowerBoundMS != nil || len(def.Cases) != 1 || !strings.Contains(md, "vendor compatibility UNVERIFIED (decoder claude-json/synthetic is a synthetic fixture, not an actual-transcript qualification): measurement only, the client timed out at or below") || strings.Contains(md, "not compatible") {
		t.Fatalf("timeout: %d %+v\n%s", code, def, md)
	}
	rep, md, code = run("auth", map[string]string{"SCENARIO": "auth"})
	def = reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseDefault)
	if code != 5 || def.Status != mcpqual.StatusNotRun || !strings.Contains(md, "compatibility UNVERIFIED: the default phase is not_run") {
		t.Fatalf("auth: %d %+v\n%s", code, def, md)
	}
}

// mustBase reads repo's catalog base.
func mustBase(t *testing.T, repo string) *mcpqual.CatalogBase {
	t.Helper()
	base, err := mcpqual.ReadCatalogBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	return base
}
