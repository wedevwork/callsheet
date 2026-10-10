package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Local deployment (design 12a-real-e2e, Deployment and roles): a plane on
// 127.0.0.1 with an ephemeral port and its workspace hub, one enrolled
// sidecar per adapter with only its explicit adapter path, all four roles
// (concurrency 1, timeout 10m) registered before any task, then every
// sidecar reconnected (a deliberate superset of the documented minimum,
// avoiding the cross-node role_changed issue) and all roles awaited ready
// at the final registry state. Children are started from argv slices and
// tracked by their captured handles.

// sidecarAdapters are the three sidecars in start order.
var sidecarAdapters = []string{"codex", "claude", "grok"}

// lineLog is a child's bounded combined output with a wake-up per write.
type lineLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
	sig chan struct{}
}

// maxChildLog bounds a child's retained output.
const maxChildLog = 1 << 20

func newLineLog() *lineLog { return &lineLog{sig: make(chan struct{}, 1)} }

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	if room := maxChildLog - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	l.mu.Unlock()
	select {
	case l.sig <- struct{}{}:
	default:
	}
	return len(p), nil
}

// record returns the first JSON log record whose msg is msg.
func (l *lineLog) record(msg string) (map[string]any, bool) {
	l.mu.Lock()
	data := l.buf.String()
	l.mu.Unlock()
	for _, line := range strings.Split(data, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			return rec, true
		}
	}
	return nil, false
}

// child is one long-lived owned process.
type child struct {
	name string
	spec ProcSpec
	proc Proc
	logs *lineLog
}

// deployment is the run's plane and sidecars.
type deployment struct {
	s        *supervisor
	plane    *child
	sidecars map[string]*child
	nodes    map[string]string
	url      string
	caPath   string
	caPEM    []byte
	manuals  map[string][2]string
}

// start starts a long-lived child and records it.
func (d *deployment) start(name string, args ...string) (*child, error) {
	c := &child{name: name, logs: newLineLog()}
	c.spec = ProcSpec{Path: d.s.args.callsheet, Args: args, Env: d.s.w.Environ, Dir: d.s.runtime, Stdout: c.logs, Stderr: c.logs}
	p, err := d.s.w.Launcher.Start(c.spec)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	c.proc = p
	d.s.track(c)
	d.s.ev.event(Event{Type: EvProcessStarted, Handle: name})
	return c, nil
}

// await waits until c logs msg, exits or the bound passes.
func (d *deployment) await(c *child, msg string, bound time.Duration) (map[string]any, error) {
	t, stop := d.s.w.Clock.NewTimer(bound)
	defer stop()
	for {
		if rec, ok := c.logs.record(msg); ok {
			return rec, nil
		}
		select {
		case <-c.logs.sig:
		case <-c.proc.Done():
			if rec, ok := c.logs.record(msg); ok {
				return rec, nil
			}
			return nil, fmt.Errorf("%s exited before %q", c.name, msg)
		case <-t:
			return nil, fmt.Errorf("%s did not log %q within %s", c.name, msg, bound)
		}
	}
}

// short runs a short callsheet command and returns its stdout.
func (d *deployment) short(what string, args ...string) (string, error) {
	res, err := d.s.command(ProcSpec{Path: d.s.args.callsheet, Args: args, Env: d.s.w.Environ, Dir: d.s.runtime}, 30*time.Second, 64<<10)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if res.TimedOut || res.ExitCode != 0 {
		return "", fmt.Errorf("%s failed (exit %d)", what, res.ExitCode)
	}
	return string(res.Stdout), nil
}

// startPlane initializes and runs the plane and connects to it.
func (d *deployment) startPlane() error {
	state := filepath.Join(d.s.runtime, "p")
	if _, err := d.short("plane init", "plane", "init", "--state-dir", state, "--bind", "127.0.0.1:0", "--san", "127.0.0.1"); err != nil {
		return err
	}
	c, err := d.start("plane", "plane", "run", "--state-dir", state)
	if err != nil {
		return err
	}
	d.plane = c
	rec, err := d.await(c, "listening", time.Minute)
	if err != nil {
		return err
	}
	bind, _ := rec["bind"].(string)
	if !strings.HasPrefix(bind, "127.0.0.1:") {
		return errors.New("the plane did not bind 127.0.0.1")
	}
	d.url = "https://" + bind
	d.caPath = filepath.Join(state, "pki", "ca.crt")
	if d.caPEM, err = readBounded(d.caPath, 64<<10); err != nil {
		return fmt.Errorf("plane CA: %w", err)
	}
	return nil
}

// startSidecars enrolls and runs one sidecar per adapter with its explicit
// executable path.
func (d *deployment) startSidecars() error {
	d.sidecars, d.nodes = map[string]*child{}, map[string]string{}
	for _, a := range sidecarAdapters {
		state := filepath.Join(d.s.runtime, "n-"+a)
		out, err := d.short("sidecar enroll "+a, "sidecar", "enroll", "--plane", d.url, "--ca", d.caPath, "--state-dir", state)
		if err != nil {
			return err
		}
		id, ok := strings.CutPrefix(strings.SplitN(out, "\n", 2)[0], "node_id: ")
		if !ok || !contract.ValidNodeID(id) {
			return fmt.Errorf("sidecar enroll %s printed no node ID", a)
		}
		d.nodes[a] = id
		c, err := d.start("sidecar-"+a, "sidecar", "run", "--state-dir", state, "--"+a+"-adapter", d.s.args.vendor(a))
		if err != nil {
			return err
		}
		d.sidecars[a] = c
		if _, err := d.await(c, "heartbeat acknowledged", time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// restart stops a sidecar by its handle and runs it again with the same
// argv, awaiting its first heartbeat.
func (d *deployment) restart(a string) error {
	old := d.sidecars[a]
	rec := stopProc(old.name, old.proc, d.s.w.Clock, 15*time.Second)
	d.s.stopped(old, rec)
	if !rec.ExitVerified {
		return fmt.Errorf("sidecar %s did not stop", a)
	}
	c, err := d.start(old.name, old.spec.Args...)
	if err != nil {
		return err
	}
	d.sidecars[a] = c
	_, err = d.await(c, "heartbeat acknowledged", time.Minute)
	return err
}

// writeManuals writes the rendered manuals into the runtime and returns
// each role's instruction and runbook paths.
func (d *deployment) writeManuals(rendered map[string][]byte) error {
	d.manuals = map[string][2]string{}
	dir := filepath.Join(d.s.runtime, "m")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, h := range Hops {
		var paths [2]string
		for i, kind := range []string{"instruction", "runbook"} {
			p := filepath.Join(dir, manualName(h, kind))
			if err := os.WriteFile(p, rendered[manualName(h, kind)], 0o600); err != nil {
				return err
			}
			paths[i] = p
		}
		d.manuals[h] = paths
	}
	return nil
}

// registerRoles adds the four roles; only the plane's documented transient
// refusal (unavailable, nothing changed, "retry") is repeated, boundedly.
func (d *deployment) registerRoles(ctx context.Context) error {
	for _, r := range d.s.flow.Roles {
		rc := contract.RoleConfig{ID: r.ID, Name: r.ID, Node: d.nodes[r.Adapter], Adapter: r.Adapter, Instruction: d.manuals[r.ID][0],
			Runbook: d.manuals[r.ID][1], Model: r.Model, Effort: r.Effort, Concurrency: RoleConcurrency, Timeout: RoleTimeout, HasTimeout: true}
		_, err := d.addRole(ctx, rc)
		for n := 0; err != nil && contract.CodeOf(err) == contract.CodeUnavailable && strings.Contains(err.Error(), "retry") && n < 50; n++ {
			d.s.sleep(100 * time.Millisecond)
			_, err = d.addRole(ctx, rc)
		}
		if err != nil {
			return fmt.Errorf("role add %s: %w", r.ID, err)
		}
	}
	return nil
}

// addRole is one bounded registration request.
func (d *deployment) addRole(ctx context.Context, rc contract.RoleConfig) (contract.RoleView, error) {
	ctx, cancel := context.WithTimeout(ctx, planeCallBound)
	defer cancel()
	return d.s.plane.AddRole(ctx, rc)
}

// awaitReady polls until every role can accept work, then snapshots the
// registry.
func (d *deployment) awaitReady(ctx context.Context, bound time.Duration) (RolesRecord, error) {
	deadline := d.s.w.Clock.Now().Add(bound)
	for {
		ready := true
		for _, r := range d.s.flow.Roles {
			rctx, cancel := context.WithTimeout(ctx, planeCallBound)
			v, err := d.s.plane.ShowRole(rctx, r.ID)
			cancel()
			ready = ready && err == nil && v.CanAccept
		}
		if ready {
			break
		}
		if !d.s.w.Clock.Now().Before(deadline) {
			return RolesRecord{}, errors.New("the roles did not become ready")
		}
		d.s.sleep(time.Second)
	}
	lctx, cancel := context.WithTimeout(ctx, planeCallBound)
	defer cancel()
	views, err := d.s.plane.ListRoles(lctx)
	if err != nil {
		return RolesRecord{}, fmt.Errorf("role list: %w", err)
	}
	rec := RolesRecord{Schema: RolesSchema, ReadyAt: formatTime(d.s.w.Clock.Now())}
	for _, r := range d.s.flow.Roles {
		for _, v := range views {
			if v.ID == r.ID {
				rec.Roles = append(rec.Roles, RoleSnapshot{ID: v.ID, Node: v.Node, Adapter: v.Adapter, Model: v.Model, Effort: v.Effort,
					Concurrency: v.Concurrency, Timeout: v.Resolved().Timeout.String(), CanAccept: v.CanAccept})
			}
		}
	}
	if len(views) != len(d.s.flow.Roles) || len(rec.Roles) != len(d.s.flow.Roles) {
		return rec, errors.New("the registry does not hold exactly the four roles")
	}
	return rec, nil
}
