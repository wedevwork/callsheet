package taskhub

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/plane"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// The production plane rig (iteration 10b): a real in-process plane
// (plane.Run: its per-task writer, workspace hub, publication service and
// receipts) and a simulated protocol-6 node speaking the stream, shared by
// the plane wiring tests (taskpublication) and the publication benchmark
// (taskworkspace). Every wait rechecks its success before failing.

// Wait bounds every rig wait (a failure bound, never a verdict).
const Wait = 30 * time.Second

// listenLog captures the plane's listening address.
type listenLog struct{ addr chan string }

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

// Plane is a running production plane.
type Plane struct {
	Root, URL string
	PEM       []byte
	Client    *client.Client
	cancel    context.CancelFunc
	done      chan error
}

// RunPlane runs plane.Run on state root root until t's cleanup (or Stop).
func RunPlane(t testing.TB, root string) *Plane {
	t.Helper()
	ll := &listenLog{addr: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Plane{Root: root, cancel: cancel, done: make(chan error, 1)}
	go func() {
		p.done <- plane.Run(ctx, plane.RunOptions{StateDir: root, Bind: "127.0.0.1:0", BindSet: true, SANs: []string{"127.0.0.1"}, SANsSet: true, Logger: slog.New(ll)})
	}()
	type started struct {
		addr string
		err  error
	}
	ready := make(chan started, 2)
	go func() {
		select {
		case a := <-ll.addr:
			ready <- started{addr: a}
		case err := <-p.done:
			p.done <- err
			ready <- started{err: err}
		case <-ctx.Done():
		}
	}()
	s := testkit.WithinBy(t, ready, time.After(Wait), "the plane listening")
	if s.err != nil {
		cancel()
		t.Fatalf("plane: %v", s.err)
	}
	p.URL = "https://" + s.addr
	var err error
	if p.PEM, err = os.ReadFile(filepath.Join(root, "pki", "ca.crt")); err != nil {
		t.Fatal(err)
	}
	if p.Client, err = client.New(p.URL, client.Trust{CAPEM: p.PEM}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p
}

// Stop stops the plane and joins it (idempotent).
func (p *Plane) Stop() {
	if p.cancel == nil {
		return
	}
	p.cancel()
	<-p.done
	p.Client.Close()
	p.cancel = nil
}

// Node is a simulated protocol-6 worker: one goroutine reads the stream;
// sidecar requests go one at a time on the b sequence.
type Node struct {
	ID      string
	ws      *websocket.Conn
	mu      sync.Mutex
	b       int
	queue   []func(rid string) (string, any)
	busy    bool
	rev     int
	roles   []contract.RoleRecord
	Starts  chan contract.TaskStartBody
	acks    chan contract.TaskPreparedAckBody
	results chan contract.TaskResultAckBody
	Cancels chan contract.TaskCancelBody
	Ready   chan struct{}
	done    chan struct{}
	errs    chan string
	// hold and held: start replies withheld until ReleaseStartReply.
	hold bool
	held []func()
}

// HoldStartReplies makes the node hand each later start to Starts before
// answering it; the answer is written only by ReleaseStartReply. A test
// thereby lets a node transfer overtake the start reply, as the sidecar's
// fetch over HTTPS can overtake its preparing reply on the stream.
func (n *Node) HoldStartReplies(on bool) {
	n.mu.Lock()
	n.hold = on
	n.mu.Unlock()
}

// ReleaseStartReply writes the oldest withheld start reply (none: nothing).
func (n *Node) ReleaseStartReply() {
	n.mu.Lock()
	if len(n.held) == 0 {
		n.mu.Unlock()
		return
	}
	reply := n.held[0]
	n.held = n.held[1:]
	n.mu.Unlock()
	reply()
}

// StartNode enrolls node id (an existing enrollment is reused) and opens
// its stream until t's cleanup (or Close).
func StartNode(t testing.TB, p *Plane, id string) *Node {
	t.Helper()
	if _, err := p.Client.EnrollNode(context.Background(), id, "sim-6"); err != nil && contract.CodeOf(err) != contract.CodeConflict {
		t.Fatal(err)
	}
	ws, err := p.Client.DialNodeStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(contract.MaxFrameBytes)
	n := &Node{ID: id, ws: ws, b: 1, Starts: make(chan contract.TaskStartBody, 8), acks: make(chan contract.TaskPreparedAckBody, 8),
		results: make(chan contract.TaskResultAckBody, 8), Cancels: make(chan contract.TaskCancelBody, 8), Ready: make(chan struct{}, 8),
		done: make(chan struct{}), errs: make(chan string, 8)}
	n.write(contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "sim-6"})
	go n.loop()
	t.Cleanup(n.Close)
	return n
}

// Close closes the stream and joins the reader (idempotent).
func (n *Node) Close() {
	n.ws.CloseNow()
	<-n.done
}

func (n *Node) write(typ, rid string, body any) {
	b, err := contract.EncodeFrame(contract.ProtocolVersion, typ, rid, body)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), Wait)
	defer cancel()
	n.ws.Write(ctx, websocket.MessageText, b)
}

// request queues a sidecar request (sent when the b slot is free).
func (n *Node) request(f func(rid string) (string, any)) {
	n.mu.Lock()
	n.queue = append(n.queue, f)
	n.mu.Unlock()
	n.pump()
}

func (n *Node) pump() {
	n.mu.Lock()
	if n.busy || len(n.queue) == 0 {
		n.mu.Unlock()
		return
	}
	f := n.queue[0]
	n.queue = n.queue[1:]
	rid := "b" + strconv.Itoa(n.b)
	n.b++
	n.busy = true
	n.mu.Unlock()
	typ, body := f(rid)
	n.write(typ, rid, body)
}

func (n *Node) free() {
	n.mu.Lock()
	n.busy = false
	n.mu.Unlock()
	n.pump()
}

func (n *Node) heartbeat() {
	n.request(func(string) (string, any) {
		n.mu.Lock()
		hb := contract.HeartbeatBody{RolesRevision: n.rev, Roles: []contract.RoleStatus{}}
		for _, r := range n.roles {
			hb.Roles = append(hb.Roles, contract.RoleStatus{RoleID: r.ID, Concurrency: r.Concurrency, CanAccept: true})
		}
		n.mu.Unlock()
		return contract.FrameHeartbeat, hb
	})
}

func (n *Node) loop() {
	defer close(n.done)
	for {
		_, b, err := n.ws.Read(context.Background())
		if err != nil {
			return
		}
		f, err := contract.DecodeFrame(b, contract.FromPlane)
		if err != nil {
			n.errs <- "protocol error: " + err.Error()
			return
		}
		switch f.Type {
		case contract.FrameHelloOK:
			n.write(contract.FrameTaskInventory, "i1", contract.TaskInventoryBody{RunID: strings.Repeat("6", 32), Final: true})
		case contract.FrameTaskInventoryAck:
		case contract.FrameTaskReconcile:
			n.write(contract.FrameTaskReconcileAck, f.RequestID, contract.TaskReconcileAckBody{Received: true})
		case contract.FrameRoleValidate:
			n.write(contract.FrameRoleValidateResult, f.RequestID, contract.RoleValidateResult{})
		case contract.FrameRolesReplace:
			body, err := contract.DecodeRolesReplace(f.Body, nil)
			if err != nil {
				n.errs <- "bad snapshot"
				return
			}
			n.mu.Lock()
			n.rev, n.roles = body.Revision, body.Roles
			n.mu.Unlock()
			n.write(contract.FrameRolesReplaceAck, f.RequestID, contract.RolesReplaceAckBody{Revision: body.Revision})
			n.heartbeat()
		case contract.FrameHeartbeatAck:
			n.free()
			select {
			case n.Ready <- struct{}{}:
			default:
			}
		case contract.FrameTaskStart:
			st, err := contract.DecodeTaskStart(f.Body, nil)
			if err != nil {
				n.errs <- "bad start: " + err.Error()
				return
			}
			reply := func() {
				n.write(contract.FrameTaskStartResult, f.RequestID, contract.TaskStartResult{TaskID: st.TaskID, Preparing: st.Workspace != nil})
			}
			n.mu.Lock()
			hold := n.hold
			if hold {
				n.held = append(n.held, reply)
			}
			n.mu.Unlock()
			if !hold {
				reply()
			}
			n.Starts <- st
		case contract.FrameTaskPreparedAck:
			a, err := contract.DecodeTaskPreparedAck(f.Body)
			if err != nil {
				n.errs <- "bad prepared ack"
				return
			}
			n.free()
			n.acks <- a
		case contract.FrameTaskResultAck:
			a, err := contract.DecodeTaskResultAck(f.Body)
			if err != nil {
				n.errs <- "bad result ack"
				return
			}
			n.free()
			n.results <- a
		case contract.FrameTaskLogAck:
			n.free()
		case contract.FrameTaskCancel:
			c, err := contract.DecodeTaskCancel(f.Body)
			if err != nil {
				n.errs <- "bad cancel"
				return
			}
			n.write(contract.FrameTaskCancelAck, f.RequestID, contract.TaskCancelAckBody{TaskID: c.TaskID, Execution: c.Execution, StopID: c.StopID, Received: true})
			n.Cancels <- c
		default:
			n.errs <- "unexpected " + f.Type
			return
		}
	}
}

// Recv receives the next value of ch within Wait; a simulated node error
// fails t. The timeout rechecks ch before failing (a value and the timeout
// both ready never fail the test).
func Recv[T any](t testing.TB, n *Node, ch chan T, what string) T {
	t.Helper()
	return RecvBy(t, n, ch, time.After(Wait), what)
}

// RecvBy is Recv with an explicit timeout channel.
func RecvBy[T any](t testing.TB, n *Node, ch chan T, timeout <-chan time.Time, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case e := <-n.errs:
		t.Fatalf("simulated node: %s", e)
	case <-timeout:
		select {
		case v := <-ch:
			return v
		default:
		}
		t.Fatalf("no %s", what)
	}
	var zero T
	return zero
}

// ClientAPI is a task's production publication client: the plane's
// verified HTTPS publication endpoints and the node receive session, as
// the sidecar wires them.
func ClientAPI(c *client.Client, taskID string, a contract.NodeAssignment) (taskworkspace.PlaneAPI, error) {
	tp, err := c.TaskPublications(taskID, a)
	if err != nil {
		return nil, err
	}
	return clientAPI{c: c, tp: tp, id: taskID, a: a}, nil
}

type clientAPI struct {
	c  *client.Client
	tp *client.TaskPublications
	id string
	a  contract.NodeAssignment
}

func (p clientAPI) Begin(ctx context.Context, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error) {
	return p.tp.Begin(ctx, req)
}

func (p clientAPI) Finish(ctx context.Context, pub, code string) (contract.PublicationStatus, error) {
	return p.tp.Finish(ctx, pub, code)
}

func (p clientAPI) Observe(ctx context.Context, pub string) (contract.PublicationStatus, error) {
	return p.tp.Observe(ctx, pub)
}

func (p clientAPI) Pusher(pub string) (taskworkspace.Pusher, error) {
	g, err := p.c.NodeTaskPusher(p.id, p.a, pub)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// Assignment is the node's assignment of start st.
func (n *Node) Assignment(st contract.TaskStartBody) contract.NodeAssignment {
	return contract.NodeAssignment{NodeID: n.ID, Execution: st.Execution, StartDigest: st.StartDigestHex(), Instance: st.Workspace.Instance}
}

// Prepared reports st's preparation and returns the plane's decision.
func (n *Node) Prepared(t testing.TB, st contract.TaskStartBody, perr *contract.Error) contract.TaskPreparedAckBody {
	t.Helper()
	n.request(func(string) (string, any) {
		return contract.FrameTaskPrepared, contract.TaskPreparedBody{TaskID: st.TaskID, Execution: st.Execution, StartDigest: st.StartDigestHex(), OK: perr == nil, Error: perr}
	})
	return Recv(t, n, n.acks, "task_prepared_ack")
}

// Result reports a sealed result and waits for its committed
// acknowledgement (the plane's durable receipt), resending it like the
// sidecar's outbox while the plane answers uncommitted.
func (n *Node) Result(t testing.TB, r contract.TaskResultBody) contract.TaskResultAckBody {
	t.Helper()
	deadline := time.Now().Add(Wait)
	for {
		n.request(func(string) (string, any) { return contract.FrameTaskResult, r })
		a := Recv(t, n, n.results, "task_result_ack")
		if a.Committed || time.Now().After(deadline) {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
}
