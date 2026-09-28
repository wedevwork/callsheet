package function

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/plane"
)

// Iteration 05 function tests: exactly one top-level TestTask* per FP, in
// FP order. Every subtest but TestTaskCommands' delegates to the package
// contracts named in devcheck's task delegation table and requires each
// package's own run/pass evidence; TestTaskCommands drives the in-process
// CLI against an in-process TLS plane and a simulated worker. No task or
// probe child is started here (the real process qualification is the
// sidecar package's, delegated by TestTaskExecution/process).

// taskDelegate runs every delegation row of wrapper in its package's
// compiled test binary and requires the wrapper's complete table.
func taskDelegate(t *testing.T, wrapper string) {
	t.Helper()
	env := append(os.Environ(), nodeCLIEnv+"="+nodeBinary(t))
	outputs := map[string]string{}
	rows := devcheck.TaskDelegationsFor(wrapper)
	if len(rows) == 0 {
		t.Fatalf("%s has no delegation rows", wrapper)
	}
	for _, d := range rows {
		out := contractRun(t, contractBinary(t, d.Package), d.Package, d.Selector, env, d.Required...)
		if err := devcheck.CheckDelegationEvidence(d, out); err != nil {
			t.Fatal(err)
		}
		outputs[d.Package] = out
	}
	if err := devcheck.CheckTaskWrapperEvidence(wrapper, outputs); err != nil {
		t.Fatal(err)
	}
}

// FP-1: the strict envelope and the persisted state/result matrix.
func TestTaskModel(t *testing.T) {
	t.Run("envelope", func(t *testing.T) { taskDelegate(t, "TestTaskModel/envelope") })
	t.Run("states", func(t *testing.T) { taskDelegate(t, "TestTaskModel/states") })
}

// FP-2: the stable picker and both deterministic last-slot orderings.
func TestTaskDispatch(t *testing.T) {
	t.Run("selection", func(t *testing.T) { taskDelegate(t, "TestTaskDispatch/selection") })
	t.Run("gate-race", func(t *testing.T) { taskDelegate(t, "TestTaskDispatch/gate-race") })
	t.Run("reserved-slot", func(t *testing.T) { taskDelegate(t, "TestTaskDispatch/reserved-slot") })
}

// FP-3: wire, deduplication and fencing; receipt independent of disk;
// genuine missing-ack behavior (plane and sidecar each).
func TestTaskProtocol(t *testing.T) {
	t.Run("duplex", func(t *testing.T) { taskDelegate(t, "TestTaskProtocol/duplex") })
	t.Run("bounds", func(t *testing.T) { taskDelegate(t, "TestTaskProtocol/bounds") })
	t.Run("result-receipt", func(t *testing.T) { taskDelegate(t, "TestTaskProtocol/result-receipt") })
	t.Run("result-ack-loss", func(t *testing.T) { taskDelegate(t, "TestTaskProtocol/result-ack-loss") })
}

// FP-4: local prompt, explicit argv/extraction/signals, state
// translation, the sole real process fixture, OS seams and the literal
// evidence policy.
func TestTaskExecution(t *testing.T) {
	t.Run("compose", func(t *testing.T) { taskDelegate(t, "TestTaskExecution/compose") })
	t.Run("invoke", func(t *testing.T) { taskDelegate(t, "TestTaskExecution/invoke") })
	t.Run("exit", func(t *testing.T) { taskDelegate(t, "TestTaskExecution/exit") })
	t.Run("process", func(t *testing.T) { taskDelegate(t, "TestTaskExecution/process") })
	t.Run("platform", func(t *testing.T) { taskDelegate(t, "TestTaskExecution/platform") })
	t.Run("policy", func(t *testing.T) { taskDelegate(t, "TestTaskExecution/policy") })
}

// FP-5: cap and gaps, bounded drainage, final output and durable tail.
func TestTaskLogs(t *testing.T) {
	t.Run("retention", func(t *testing.T) { taskDelegate(t, "TestTaskLogs/retention") })
	t.Run("backpressure", func(t *testing.T) { taskDelegate(t, "TestTaskLogs/backpressure") })
	t.Run("final", func(t *testing.T) { taskDelegate(t, "TestTaskLogs/final") })
}

// FP-6: fault boundaries and reload, removed-role history included.
func TestTaskPersistence(t *testing.T) {
	t.Run("durability", func(t *testing.T) { taskDelegate(t, "TestTaskPersistence/durability") })
	t.Run("restore", func(t *testing.T) { taskDelegate(t, "TestTaskPersistence/restore") })
}

// FP-8: counts, set/rm interlocks, remaining capacity and recovery-only
// removal (plane and sidecar each).
func TestTaskRoles(t *testing.T) {
	t.Run("counts", func(t *testing.T) { taskDelegate(t, "TestTaskRoles/counts") })
	t.Run("mutation", func(t *testing.T) { taskDelegate(t, "TestTaskRoles/mutation") })
	t.Run("remaining-capacity", func(t *testing.T) { taskDelegate(t, "TestTaskRoles/remaining-capacity") })
	t.Run("recovery-remove", func(t *testing.T) { taskDelegate(t, "TestTaskRoles/recovery-remove") })
}

// FP-9: no replay, usable unreserved slots, removal and re-add keeping
// nonterminal history (plane and sidecar each).
func TestTaskRecoveryBoundary(t *testing.T) {
	t.Run("disconnect", func(t *testing.T) { taskDelegate(t, "TestTaskRecoveryBoundary/disconnect") })
	t.Run("remaining-capacity", func(t *testing.T) { taskDelegate(t, "TestTaskRecoveryBoundary/remaining-capacity") })
	t.Run("recovery-remove", func(t *testing.T) { taskDelegate(t, "TestTaskRecoveryBoundary/recovery-remove") })
}

// ---- In-process plane, simulated worker and CLI (TestTaskCommands) ----

// listenLog captures the plane's listening record.
type listenLog struct {
	addr chan string
}

func (l *listenLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *listenLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "listening" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "bind" {
				select {
				case l.addr <- a.Value.String():
				default:
				}
			}
			return true
		})
	}
	return nil
}
func (l *listenLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *listenLog) WithGroup(string) slog.Handler      { return l }

// inProcessPlane runs plane.Run in this process on 127.0.0.1.
type inProcessPlane struct {
	url, ca, fp string
	cancel      context.CancelFunc
	done        chan error
}

func startInProcessPlane(t *testing.T) *inProcessPlane {
	t.Helper()
	root := filepath.Join(t.TempDir(), "plane")
	ll := &listenLog{addr: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	p := &inProcessPlane{cancel: cancel, done: make(chan error, 1), ca: filepath.Join(root, "pki", "ca.crt")}
	go func() {
		p.done <- plane.Run(ctx, plane.RunOptions{StateDir: root, Bind: "127.0.0.1:0", BindSet: true, SANs: []string{"127.0.0.1"}, SANsSet: true, Logger: slog.New(ll)})
	}()
	select {
	case a := <-ll.addr:
		p.url = "https://" + a
	case err := <-p.done:
		cancel()
		t.Fatalf("plane: %v", err)
	case <-time.After(nodeWait):
		cancel()
		t.Fatal("the plane did not listen")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-p.done:
		case <-time.After(nodeWait):
			t.Error("the plane did not stop")
		}
	})
	st, err := plane.Inspect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	p.fp = st.CAFingerprint
	return p
}

func (p *inProcessPlane) trust() []string { return []string{"--plane", p.url, "--ca", p.ca} }

func (p *inProcessPlane) client(t *testing.T) *client.Client {
	t.Helper()
	pem, err := os.ReadFile(p.ca)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(p.url, client.Trust{CAPEM: pem})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// simWorker is a simulated sidecar speaking protocol 4 over verified
// WSS: it reports an empty task inventory and acknowledges the plane's
// empty reconciliation, validates every role, acknowledges snapshots with
// a ready heartbeat, accepts every task start and reports, one request at
// a time on the shared b sequence, the output and sealed exit its script
// chooses, resending a result until the plane acknowledges it committed.
type simWorker struct {
	t      *testing.T
	ws     *websocket.Conn
	id     string
	script func(contract.TaskStartBody) ([]byte, int)
	b      int
	queue  []func(rid string) (string, any)
	busy   bool
	mu     sync.Mutex
	rev    int
	roles  []contract.RoleRecord
	// results are the sealed results reported, for resending.
	results []contract.TaskResultBody
	events  chan string
	done    chan struct{}
}

func startSimWorker(t *testing.T, p *inProcessPlane, script func(contract.TaskStartBody) ([]byte, int)) *simWorker {
	t.Helper()
	c := p.client(t)
	id := "n_" + strings.Repeat("5", 32)
	if _, err := c.EnrollNode(context.Background(), id, "sim-1"); err != nil {
		t.Fatal(err)
	}
	ws, err := c.DialNodeStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := &simWorker{t: t, ws: ws, id: id, script: script, b: 1, events: make(chan string, 256), done: make(chan struct{})}
	w.write(contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "sim-1"})
	go w.loop()
	t.Cleanup(func() {
		ws.CloseNow()
		select {
		case <-w.done:
		case <-time.After(nodeWait):
			t.Error("the simulated worker did not stop")
		}
	})
	w.await(t, "heartbeat-acked")
	return w
}

func (w *simWorker) write(typ, rid string, body any) {
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, rid, body)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nodeWait)
	defer cancel()
	w.ws.Write(ctx, websocket.MessageText, b)
}

// request queues a sidecar request; it is sent when the slot is free.
func (w *simWorker) request(f func(rid string) (string, any)) {
	w.queue = append(w.queue, f)
	w.pump()
}

func (w *simWorker) pump() {
	if w.busy || len(w.queue) == 0 {
		return
	}
	f := w.queue[0]
	w.queue = w.queue[1:]
	rid := "b" + strconv.Itoa(w.b)
	w.b++
	typ, body := f(rid)
	w.busy = true
	w.write(typ, rid, body)
}

func (w *simWorker) heartbeat() {
	w.request(func(string) (string, any) {
		hb := contract.HeartbeatBody{RolesRevision: w.rev, Roles: []contract.RoleStatus{}}
		for _, r := range w.roles {
			hb.Roles = append(hb.Roles, contract.RoleStatus{RoleID: r.ID, Concurrency: r.Concurrency, CanAccept: true})
		}
		return contract.FrameHeartbeat, hb
	})
}

func (w *simWorker) loop() {
	defer close(w.done)
	for {
		_, b, err := w.ws.Read(context.Background())
		if err != nil {
			return
		}
		f, err := contract.DecodeFrame(b, contract.FromPlane)
		if err != nil {
			w.events <- "protocol error: " + err.Error()
			return
		}
		switch f.Type {
		case contract.FrameHelloOK:
			w.write(contract.FrameTaskInventory, "i1", contract.TaskInventoryBody{RunID: strings.Repeat("5", 32), Final: true})
		case contract.FrameTaskInventoryAck:
		case contract.FrameTaskReconcile:
			if b, err := contract.DecodeTaskReconcile(f.Body); err != nil || !b.Final || len(b.Entries) != 0 {
				w.events <- "bad reconcile"
				return
			}
			w.write(contract.FrameTaskReconcileAck, f.RequestID, contract.TaskReconcileAckBody{Received: true})
		case contract.FrameRoleValidate:
			w.write(contract.FrameRoleValidateResult, f.RequestID, contract.RoleValidateResult{})
		case contract.FrameRolesReplace:
			body, err := contract.DecodeRolesReplace(f.Body, nil)
			if err != nil {
				w.events <- "bad snapshot"
				return
			}
			w.rev, w.roles = body.Revision, body.Roles
			w.write(contract.FrameRolesReplaceAck, f.RequestID, contract.RolesReplaceAckBody{Revision: body.Revision})
			w.heartbeat()
		case contract.FrameTaskStart:
			st, err := contract.DecodeTaskStart(f.Body, nil)
			if err != nil {
				w.events <- "bad start"
				return
			}
			w.write(contract.FrameTaskStartResult, f.RequestID, contract.TaskStartResult{TaskID: st.TaskID})
			out, exit := w.script(st)
			for off := 0; off < len(out); off += contract.MaxLogChunkBytes {
				chunk := out[off:min(len(out), off+contract.MaxLogChunkBytes)]
				o := off
				w.request(func(string) (string, any) {
					return contract.FrameTaskLog, contract.TaskLogBody{TaskID: st.TaskID, Execution: st.Execution, Offset: o, Data: chunk}
				})
			}
			msg := "fake task completed"
			res := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit, FinalMessage: &msg,
				OutputBytes: len(out)}.Sealed()
			w.results = append(w.results, res)
			w.request(func(string) (string, any) { return contract.FrameTaskResult, res })
		case contract.FrameTaskResultAck:
			ack, err := contract.DecodeTaskResultAck(f.Body)
			if err != nil {
				w.events <- "bad result ack"
				return
			}
			w.busy = false
			if !ack.Committed {
				// Received but not yet durable: the outbox keeps it and
				// resends it (a real sidecar waits 1 s).
				for _, res := range w.results {
					if res.TaskID == ack.TaskID {
						time.Sleep(20 * time.Millisecond)
						w.request(func(string) (string, any) { return contract.FrameTaskResult, res })
					}
				}
			} else {
				w.events <- "result-acked"
			}
			w.pump()
		case contract.FrameHeartbeatAck, contract.FrameTaskLogAck:
			w.busy = false
			w.events <- map[string]string{contract.FrameHeartbeatAck: "heartbeat-acked", contract.FrameTaskLogAck: "log-acked"}[f.Type]
			w.pump()
		default:
			w.events <- "unexpected " + f.Type
			return
		}
	}
}

func (w *simWorker) await(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(nodeWait)
	for {
		select {
		case e := <-w.events:
			if e == want {
				return
			}
			if strings.HasPrefix(e, "protocol error") || strings.HasPrefix(e, "unexpected") || strings.HasPrefix(e, "bad") {
				t.Fatalf("simulated worker: %s", e)
			}
		case <-deadline:
			t.Fatalf("simulated worker never saw %s", want)
		}
	}
}

// runCLI runs the CLI in process.
func runCLI(ctx context.Context, args ...string) result {
	var out, errOut bytes.Buffer
	code := cli.Run(ctx, args, nil, &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

// awaitTask waits (bounded) until task id reaches a terminal state.
func awaitTask(t *testing.T, c *client.Client, id string) contract.TaskView {
	t.Helper()
	deadline := time.After(nodeWait)
	for {
		v, err := c.ShowTask(context.Background(), id, contract.DefaultTailLines)
		if err != nil {
			t.Fatal(err)
		}
		if contract.TaskTerminal(v.State) {
			return v
		}
		select {
		case <-deadline:
			if v, _ := c.ShowTask(context.Background(), id, 0); contract.TaskTerminal(v.State) {
				return v
			}
			t.Fatalf("task %s stayed %s", id, v.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// FP-7: dispatch and task ls/show/logs through the in-process CLI over
// verified TLS: text and JSON per operation, attribution on dispatch,
// pagination on ls, tails on show, byte-exact JSON on logs, and CA, pin,
// deadline and errors on trust. These are direct cases, not delegations.
func TestTaskCommands(t *testing.T) {
	bg := context.Background()
	p := startInProcessPlane(t)
	w := startSimWorker(t, p, func(st contract.TaskStartBody) ([]byte, int) {
		if strings.Contains(st.Request.Goal, "fail") {
			return []byte("failure line\n"), 7
		}
		return []byte("line one\nline two\n\x1b[31mred\x00\xff\nlast\n"), 0
	})
	c := p.client(t)
	add := append([]string{"role", "add", "worker-a", "--name", "implementer", "--node", w.id, "--adapter", "fake", "--instruction", "/srv/i.md",
		"--runbook", "/srv/r.md", "--model", "example model", "--effort", "medium", "--concurrency", "2"}, p.trust()...)
	if r := runCLI(bg, add...); r.code != 0 {
		t.Fatalf("role add = %+v", r)
	}
	w.await(t, "heartbeat-acked")
	var ids []string
	t.Run("dispatch", func(t *testing.T) {
		host, _ := os.Hostname()
		r := runCLI(bg, append([]string{"dispatch", "--role-name", "implementer", "--goal", "first goal", "--acceptance", "tests pass", "--payload", "repo://x", "--json"}, p.trust()...)...)
		if r.code != 0 || r.stderr != "" {
			t.Fatalf("dispatch = %+v", r)
		}
		v, err := contract.ParseDispatchResponse([]byte(strings.TrimSuffix(r.stdout, "\n")))
		if err != nil || v.Role.ID != "worker-a" || v.Request.RequestedBy != (contract.RequestedBy{Name: "callsheet", Version: cli.Version, Hostname: host}) ||
			strings.Count(r.stdout, "\n") != 1 {
			t.Fatalf("dispatch json %q %v", r.stdout, err)
		}
		ids = append(ids, v.TaskID)
		r = runCLI(bg, append([]string{"dispatch", "--role-id", "worker-a", "--goal", "please fail", "--acceptance", "a", "--timeout", "90m"}, p.trust()...)...)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "task_id: t_") || !strings.Contains(r.stdout, "\ntimeout: 1h30m0s\ntimeout_policy: enforced\n") || strings.Contains(r.stdout, contract.TimeoutNotice) {
			t.Fatalf("dispatch text %+v", r)
		}
		ids = append(ids, strings.TrimPrefix(strings.SplitN(r.stdout, "\n", 2)[0], "task_id: "))
		if v := awaitTask(t, c, ids[0]); v.State != contract.TaskSucceeded {
			t.Fatalf("first task %+v", v)
		}
		if v := awaitTask(t, c, ids[1]); v.State != contract.TaskFailed || *v.Result.ExitCode != 7 {
			t.Fatalf("second task %+v", v)
		}
		// Workspaces are refused (iteration 10), nothing is dispatched.
		_, err = c.Dispatch(bg, contract.DispatchRequest{})
		if err == nil {
			t.Fatal("an empty request was sent")
		}
	})
	t.Run("ls", func(t *testing.T) {
		r := runCLI(bg, append([]string{"task", "ls", "--limit", "1"}, p.trust()...)...)
		lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
		if r.code != 0 || len(lines) != 3 || lines[0] != "TASK_ID\tSTATE\tROLE_ID\tNODE\tELAPSED_MS\tRECONCILING" || !strings.HasPrefix(lines[2], "next_after: t_") {
			t.Fatalf("ls page 1 %+v", r)
		}
		cursor := strings.TrimPrefix(lines[2], "next_after: ")
		r = runCLI(bg, append([]string{"task", "ls", "--after", cursor, "--json"}, p.trust()...)...)
		tasks, next, err := contract.ParseTaskListResponse([]byte(strings.TrimSuffix(r.stdout, "\n")))
		if r.code != 0 || err != nil || len(tasks) != 1 || next != nil || tasks[0].TaskID <= cursor {
			t.Fatalf("ls page 2 %+v %v", r, err)
		}
		all := map[string]bool{strings.Split(lines[1], "\t")[0]: true, tasks[0].TaskID: true}
		if !all[ids[0]] || !all[ids[1]] {
			t.Fatalf("pages %v miss %v", all, ids)
		}
	})
	t.Run("show", func(t *testing.T) {
		r := runCLI(bg, append([]string{"task", "show", ids[0], "--lines", "2"}, p.trust()...)...)
		if r.code != 0 || !strings.Contains(r.stdout, "state: succeeded\n") || !strings.Contains(r.stdout, "final_message: \"fake task completed\"\n") ||
			!strings.HasSuffix(r.stdout, "log_tail:\n\\x1b[31mred\\x00�\nlast\n") || strings.Contains(r.stdout, "\x1b") {
			t.Fatalf("show text %+v", r)
		}
		r = runCLI(bg, append([]string{"task", "show", ids[1], "--json"}, p.trust()...)...)
		v, err := contract.ParseTaskShowResponse([]byte(strings.TrimSuffix(r.stdout, "\n")))
		if r.code != 0 || err != nil || v.State != contract.TaskFailed || v.LogTail != "failure line\n" {
			t.Fatalf("show json %+v %v", r, err)
		}
	})
	t.Run("logs", func(t *testing.T) {
		r := runCLI(bg, append([]string{"task", "logs", ids[0], "--json"}, p.trust()...)...)
		lr, err := contract.ParseTaskLogsResponse([]byte(strings.TrimSuffix(r.stdout, "\n")))
		if r.code != 0 || r.stderr != "" || err != nil || string(lr.Data) != "line one\nline two\n\x1b[31mred\x00\xff\nlast\n" || lr.Truncated {
			t.Fatalf("logs json %+v %v", r, err)
		}
		r = runCLI(bg, append([]string{"task", "logs", ids[0]}, p.trust()...)...)
		if r.code != 0 || r.stdout != "line one\nline two\n\\x1b[31mred\\x00�\nlast\n" {
			t.Fatalf("logs text %+v", r)
		}
	})
	t.Run("trust", func(t *testing.T) {
		pin := []string{"--plane", p.url, "--ca-fingerprint", p.fp}
		if r := runCLI(bg, append([]string{"task", "show", ids[0]}, pin...)...); r.code != 0 {
			t.Fatalf("pinned show %+v", r)
		}
		wrong := []string{"--plane", p.url, "--ca-fingerprint", "sha256:" + strings.Repeat("0", 64)}
		for _, args := range [][]string{append([]string{"task", "ls"}, wrong...), {"task", "logs", ids[0], "--plane", p.url}} {
			if r := runCLI(bg, args...); r.code != 6 || !strings.Contains(r.stderr, "connection not trusted") {
				t.Fatalf("%v = %+v", args, r)
			}
		}
		if r := runCLI(bg, append([]string{"task", "show", "t_" + strings.Repeat("f", 32)}, p.trust()...)...); r.code != 3 || !strings.Contains(r.stderr, "does not exist") {
			t.Fatalf("unknown task %+v", r)
		}
		if r := runCLI(bg, append([]string{"dispatch", "--role-id", "nobody", "--goal", "g", "--acceptance", "a"}, p.trust()...)...); r.code != 3 {
			t.Fatalf("unknown role %+v", r)
		}
		// The caller's deadline wins over a plane that never answers.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
		defer cancel()
		if r := runCLI(ctx, "task", "ls", "--plane", "https://"+ln.Addr().String(), "--ca", p.ca); r.code != 130 {
			t.Fatalf("deadline %+v", r)
		}
	})
}
