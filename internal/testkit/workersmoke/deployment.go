package workersmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Logs is a child's stderr: concurrency-safe, with a wake-up per write.
type Logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
	sig chan struct{}
}

func newLogs() *Logs { return &Logs{sig: make(chan struct{}, 1)} }

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	n, err := l.buf.Write(p)
	l.mu.Unlock()
	select {
	case l.sig <- struct{}{}:
	default:
	}
	return n, err
}

func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Record returns the first JSON log record whose msg is msg.
func (l *Logs) Record(msg string) (map[string]any, bool) {
	for _, line := range strings.Split(l.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			return rec, true
		}
	}
	return nil, false
}

// Proc is a long-lived callsheet child (plane run or sidecar run).
type Proc struct {
	cmd  *exec.Cmd
	Logs *Logs
	done chan struct{}
	err  error
}

// PID is the child's process ID (diagnostics).
func (p *Proc) PID() int { return p.cmd.Process.Pid }

// await waits until the child logs msg, exits, or ctx ends.
func (p *Proc) await(ctx context.Context, msg string) (map[string]any, error) {
	for {
		if rec, ok := p.Logs.Record(msg); ok {
			return rec, nil
		}
		select {
		case <-p.Logs.sig:
		case <-p.done:
			if rec, ok := p.Logs.Record(msg); ok {
				return rec, nil
			}
			return nil, fmt.Errorf("%s exited (%v) before %q:\n%s", p.cmd.Args[1], p.err, msg, p.Logs.String())
		case <-ctx.Done():
			return nil, fmt.Errorf("no %q from %s: %w\n%s", msg, p.cmd.Args[1], ctx.Err(), p.Logs.String())
		}
	}
}

// stop sends SIGTERM and waits (bounded), killing a child that ignores it.
func (p *Proc) stop() error {
	if p == nil {
		return nil
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("%s ignored SIGTERM", p.cmd.Args[1])
	}
	return nil
}

// Sidecar is an enrolled, running sidecar.
type Sidecar struct {
	*Proc
	NodeID string
	State  string
}

// Stop stops the sidecar (SIGTERM; its tasks' groups are cleaned up) and
// waits for it; stopping a stopped sidecar is a no-op.
func (s *Sidecar) Stop() error { return s.stop() }

// Deployment is a real local Callsheet deployment on 127.0.0.1: a plane
// and the sidecars started for it, all from Bin, with a verified client.
type Deployment struct {
	Bin    string
	Dir    string
	Env    []string
	URL    string
	CA     string
	Client *client.Client

	plane *Proc
	// mu guards sidecars: parallel tests start sidecars on one deployment.
	mu       sync.Mutex
	sidecars []*Sidecar
}

// Start initializes and runs a plane under dir with the environment env
// (the CLI's complete environment: nothing else is inherited).
func Start(ctx context.Context, bin, dir string, env []string) (*Deployment, error) {
	d := &Deployment{Bin: bin, Dir: dir, Env: env}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "plane")
	if out, err := d.run(ctx, env, "plane", "init", "--state-dir", root, "--bind", "127.0.0.1:0", "--san", "127.0.0.1"); err != nil {
		return nil, fmt.Errorf("plane init: %v\n%s", err, out)
	}
	d.CA = filepath.Join(root, "pki", "ca.crt")
	p, err := d.start(env, "plane", "run", "--state-dir", root)
	if err != nil {
		return nil, err
	}
	d.plane = p
	rec, err := p.await(ctx, "listening")
	if err != nil {
		d.Close()
		return nil, err
	}
	bind, _ := rec["bind"].(string)
	d.URL = "https://" + bind
	pem, err := os.ReadFile(d.CA)
	if err != nil {
		d.Close()
		return nil, err
	}
	if d.Client, err = client.New(d.URL, client.Trust{CAPEM: pem}); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// run runs one short CLI command and returns its combined output.
func (d *Deployment) run(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Dir, cmd.Env = d.Dir, env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// start starts a long-lived child.
func (d *Deployment) start(env []string, args ...string) (*Proc, error) {
	p := &Proc{Logs: newLogs(), done: make(chan struct{})}
	p.cmd = exec.Command(d.Bin, args...)
	p.cmd.Dir, p.cmd.Env = d.Dir, env
	p.cmd.Stdout, p.cmd.Stderr = p.Logs, p.Logs
	p.cmd.WaitDelay = 5 * time.Second
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	return p, nil
}

// StartSidecar enrolls a node in dir/name and runs "sidecar run" with the
// extra flags args (the explicit adapter paths) under env, returning once
// the plane acknowledged its first heartbeat.
func (d *Deployment) StartSidecar(ctx context.Context, name string, env []string, args ...string) (*Sidecar, error) {
	state := filepath.Join(d.Dir, name)
	out, err := d.run(ctx, d.Env, "sidecar", "enroll", "--plane", d.URL, "--ca", d.CA, "--state-dir", state)
	id, ok := strings.CutPrefix(strings.SplitN(out, "\n", 2)[0], "node_id: ")
	if err != nil || !ok || !contract.ValidNodeID(id) {
		return nil, fmt.Errorf("sidecar enroll: %v\n%s", err, out)
	}
	p, err := d.start(env, append([]string{"sidecar", "run", "--state-dir", state}, args...)...)
	if err != nil {
		return nil, err
	}
	s := &Sidecar{Proc: p, NodeID: id, State: state}
	d.mu.Lock()
	d.sidecars = append(d.sidecars, s)
	d.mu.Unlock()
	if _, err := p.await(ctx, "heartbeat acknowledged"); err != nil {
		return nil, err
	}
	return s, nil
}

// Restart stops s (SIGTERM) and runs it again on its state with the same
// arguments and environment, returning once the plane acknowledged its
// first heartbeat: its new attachment receives the node's complete current
// roles snapshot.
func (d *Deployment) Restart(ctx context.Context, s *Sidecar) error {
	if err := s.stop(); err != nil {
		return err
	}
	p, err := d.start(s.cmd.Env, s.cmd.Args[1:]...)
	if err != nil {
		return err
	}
	s.Proc = p
	_, err = p.await(ctx, "heartbeat acknowledged")
	return err
}

// Close stops every sidecar (their tasks' groups cleaned up by SIGTERM),
// then the plane, and closes the client.
func (d *Deployment) Close() error {
	var errs []error
	d.mu.Lock()
	sidecars := slices.Clone(d.sidecars)
	d.mu.Unlock()
	for _, s := range sidecars {
		errs = append(errs, s.stop())
	}
	errs = append(errs, d.plane.stop())
	if d.Client != nil {
		d.Client.Close()
	}
	return errors.Join(errs...)
}

// Manuals writes a worker's instruction and runbook under dir/name.
func (d *Deployment) Manuals(name, instruction, runbook string) (string, string, error) {
	dir := filepath.Join(d.Dir, "manuals", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	ins, run := filepath.Join(dir, "instruction.md"), filepath.Join(dir, "runbook.md")
	if err := os.WriteFile(ins, []byte(instruction), 0o644); err != nil {
		return "", "", err
	}
	return ins, run, os.WriteFile(run, []byte(runbook), 0o644)
}

// AddVendorRole registers role id for vendor on node with the vendor's
// qualified model/effort pair (never an operator default) and waits until
// it can accept work.
func (d *Deployment) AddVendorRole(ctx context.Context, id, vendor, node, ins, run string, concurrency int) error {
	var q adapter.Qualification
	for _, c := range adapter.Qualifications() {
		if c.ID == vendor {
			q = c
		}
	}
	if q.ID == "" {
		return fmt.Errorf("%s is not a qualified vendor", vendor)
	}
	if _, err := d.Client.AddRole(ctx, contract.RoleConfig{ID: id, Name: id, Node: node, Adapter: vendor, Instruction: ins, Runbook: run,
		Model: q.Model, Effort: q.Effort, Concurrency: concurrency}); err != nil {
		return fmt.Errorf("role add %s: %w", id, err)
	}
	return poll(ctx, "role "+id+" ready", func() (bool, error) {
		v, err := d.Client.ShowRole(ctx, id)
		return err == nil && v.CanAccept, nil
	})
}

// poll checks cond every 20 ms until it holds or ctx ends.
func poll(ctx context.Context, what string, cond func() (bool, error)) error {
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", what, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Result is a finished smoke task: its view and complete log bytes.
type Result struct {
	View contract.TaskView
	Logs []byte
}

// ErrOuterBound is a task that did not finish within OuterBound (it was
// cancelled and its cleanup awaited).
var ErrOuterBound = errors.New("the smoke task did not finish within its outer bound")

// Dispatch sends goal to role roleID and waits for the task's terminal
// state within bound. When the bound expires the task is cancelled through
// the plane and the harness waits (bounded) until it is terminal, which a
// worker reports only after its process group's cleanup was verified.
func (d *Deployment) Dispatch(ctx context.Context, roleID, goal string, bound time.Duration) (Result, error) {
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: roleID}, Goal: goal, Payload: []string{},
		Acceptance: "the final answer is the requested text", RequestedBy: contract.RequestedBy{Name: "callsheet-smoke", Version: "1", Hostname: "smoke"}}
	v, err := d.Client.Dispatch(ctx, req)
	if err != nil {
		return Result{}, fmt.Errorf("dispatch: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var view contract.TaskView
	terminal := func(c context.Context) error {
		return poll(c, "task "+v.TaskID, func() (bool, error) {
			var err error
			view, err = d.Client.ShowTask(ctx, v.TaskID, contract.DefaultTailLines)
			return err == nil && contract.TaskTerminal(view.State), nil
		})
	}
	if err := terminal(wctx); err != nil {
		if ctx.Err() != nil {
			return Result{View: view}, err
		}
		d.Client.CancelTask(ctx, v.TaskID)
		cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
		defer ccancel()
		if cerr := terminal(cctx); cerr != nil {
			return Result{View: view}, fmt.Errorf("%w; after the cancel: %v", ErrOuterBound, cerr)
		}
		return Result{View: view}, ErrOuterBound
	}
	logs, err := d.Client.TaskLogs(ctx, v.TaskID)
	if err != nil {
		return Result{View: view}, fmt.Errorf("task logs: %w", err)
	}
	return Result{View: view, Logs: logs.Data}, nil
}

// SmokeInstruction and SmokeRunbook are the smoke worker's manuals.
const (
	SmokeInstruction = "You are a Callsheet smoke-test worker. Follow the task goal exactly and do not call any tools.\n"
	SmokeRunbook     = "Answer with exactly the text the goal asks for, nothing else.\n"
)

// Run is one vendor's complete smoke: a fresh deployment under dir from
// bin whose sidecar enables vendor with the explicit executable exe
// (sidecarEnv is that sidecar's complete environment), one qualified role,
// and one dispatch of goal within OuterBound. The deployment is always
// stopped before Run returns.
func Run(ctx context.Context, bin, dir string, env, sidecarEnv []string, vendor, exe, goal string) (res Result, err error) {
	d, err := Start(ctx, bin, dir, env)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if cerr := d.Close(); err == nil {
			err = cerr
		}
	}()
	s, err := d.StartSidecar(ctx, "sidecar-"+vendor, sidecarEnv, "--"+vendor+"-adapter", exe)
	if err != nil {
		return Result{}, err
	}
	ins, run, err := d.Manuals(vendor, SmokeInstruction, SmokeRunbook)
	if err != nil {
		return Result{}, err
	}
	if err := d.AddVendorRole(ctx, "smoke-"+vendor, vendor, s.NodeID, ins, run, 1); err != nil {
		return Result{}, fmt.Errorf("%w\n%s", err, s.Logs.String())
	}
	return d.Dispatch(ctx, "smoke-"+vendor, goal, OuterBound)
}
