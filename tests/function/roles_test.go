package function

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/sidecar"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 04 function tests: exactly one top-level TestRole* per FP, in
// FP order. They drive the built callsheet binary (a plane, real sidecars
// running the once-built fake adapter, and operator CLIs in separate
// roots) on 127.0.0.1, and delegate the timing-dependent mechanisms to the
// package contracts named in devcheck's delegation table, requiring each
// package's own run/pass evidence.

// roleSentinel is manual content that must never leave the worker.
const roleSentinel = "ROLE-SENTINEL-MANUAL-BODY-3f9a"

// fakeAdapterBinary is the once-built fake adapter fixture.
func fakeAdapterBinary(t *testing.T) string {
	return fixture(t, "fake-adapter", func(dir string) (string, error) {
		return testkit.BuildBinaryAt(dir, "./cmd/fake-adapter", "fake-adapter")
	})
}

// roleDelegate runs every delegation row of wrapper in its package's
// compiled test binary, validates each invocation's own evidence, then
// requires the wrapper's complete table (both packages for duplex and
// bounds).
func roleDelegate(t *testing.T, wrapper string) {
	t.Helper()
	env := append(os.Environ(), nodeCLIEnv+"="+nodeBinary(t))
	outputs := map[string]string{}
	rows := devcheck.RoleDelegationsFor(wrapper)
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
	if err := devcheck.CheckRoleWrapperEvidence(wrapper, outputs); err != nil {
		t.Fatal(err)
	}
}

// roleWorker is an enrolled node with its running sidecar and a manuals
// directory on the worker.
type roleWorker struct {
	id, state, dir string
	proc           *sidecarProc
}

// startRoleWorker enrolls a node and runs "sidecar run", with the fake
// adapter enabled when fake is non-empty, under env (nil: p's). It returns
// once the node heartbeats (online).
func startRoleWorker(t *testing.T, p *planeCLI, np *nodePlane, fake string, env []string) *roleWorker {
	t.Helper()
	w := &roleWorker{state: filepath.Join(t.TempDir(), "sidecar"), dir: filepath.Join(t.TempDir(), "worker manuals")}
	os.MkdirAll(w.dir, 0o755)
	w.id = np.enroll(t, p, w.state)
	args := []string{"sidecar", "run", "--state-dir", w.state}
	if fake != "" {
		args = append(args, "--fake-adapter", fake)
	}
	if env == nil {
		env = p.env
	}
	w.proc = startProc(t, p.bin, p.cwd, env, args, "heartbeat acknowledged")
	return w
}

// startProc runs a long-lived CLI child and waits for its JSON record msg.
func startProc(t *testing.T, bin, cwd string, env, args []string, msg string) *sidecarProc {
	t.Helper()
	s := &sidecarProc{logs: &nodeLogs{sig: make(chan struct{}, 1)}, done: make(chan struct{})}
	s.cmd = exec.Command(bin, args...)
	s.cmd.Dir, s.cmd.Env = cwd, env
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
			t.Errorf("process %d not reaped", s.cmd.Process.Pid)
		}
	})
	s.await(t, msg)
	return s
}

// manual writes a manual on the worker and returns its absolute path.
func (w *roleWorker) manual(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(w.dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (np *nodePlane) trust() []string { return []string{"--plane", np.url, "--ca", np.ca} }

// roleAdd builds "role add" for id on node with the given manuals; extra
// flag/value pairs replace a default value or are appended.
func roleAdd(np *nodePlane, id, name, node, ins, run string, extra ...string) []string {
	args := []string{"role", "add", id, "--name", name, "--node", node, "--adapter", "fake", "--instruction", ins, "--runbook", run,
		"--model", "example model", "--effort", "medium", "--concurrency", "2"}
	for i := 0; i < len(extra); i++ {
		if j := slices.Index(args, extra[i]); j > 0 && i+1 < len(extra) && strings.HasPrefix(extra[i], "--") {
			args[j+1] = extra[i+1]
			i++
			continue
		}
		args = append(args, extra[i])
	}
	return append(args, np.trust()...)
}

// rmNotice is the fixed no-cancellation notice every successful text
// role rm prints since iteration 05 (tasks.md, Recovery-only removal).
const rmNotice = "notice: removal cancels no task and signals no worker; adding the role again is not proof that old execution stopped\n"

func mustOK(t *testing.T, r result) result {
	t.Helper()
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("command failed: %+v", r)
	}
	return r
}

// roles lists the plane's roles over the client.
func (np *nodePlane) roles(t *testing.T) []contract.RoleView {
	t.Helper()
	list, err := np.client(t).ListRoles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// awaitReady waits (bounded) until every role in ids can accept work: a
// test's observation of readiness, which follows the node's next checked
// heartbeat after publication.
func (np *nodePlane) awaitReady(t *testing.T, ids ...string) {
	t.Helper()
	cl := np.client(t)
	ready := func() bool {
		list, err := cl.ListRoles(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, v := range list {
			if slices.Contains(ids, v.ID) && v.CanAccept {
				n++
			}
		}
		return n == len(ids)
	}
	deadline := time.After(nodeWait)
	for !ready() {
		select {
		case <-deadline:
			if ready() {
				return
			}
			t.Fatalf("roles %v never became ready: %+v", ids, np.roles(t))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// FP-1: required fields and constraints, free model text, repeated names,
// sticky configuration and monotonic registration order.
func TestRoleConfiguration(t *testing.T) {
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	w := startRoleWorker(t, p, np, fakeAdapterBinary(t), nil)
	op := newNodeCLI(t)
	ins, run := w.manual(t, "instruction.md", "i"), w.manual(t, "runbook.md", "r")
	t.Run("fields", func(t *testing.T) {
		full := roleAdd(np, "a", "coder", w.id, ins, run)
		drop := func(flag string) []string {
			var out []string
			for i := 0; i < len(full); i++ {
				if full[i] == flag {
					i++
					continue
				}
				out = append(out, full[i])
			}
			return out
		}
		set := func(flag, value string) []string {
			out := slices.Clone(full)
			out[slices.Index(out, flag)+1] = value
			return out
		}
		for _, c := range []struct {
			args []string
			msg  string
		}{
			{drop("--model"), "--model is required"},
			{drop("--effort"), "--effort is required"},
			{drop("--instruction"), "--instruction is required"},
			{drop("--runbook"), "--runbook is required"},
			{drop("--name"), "--name is required"},
			{drop("--node"), "--node is required"},
			{drop("--adapter"), "--adapter is required"},
			{drop("--concurrency"), "--concurrency is required"},
			{set("--model", "   "), "model must be nonblank"},
			{set("--effort", ""), "effort must not be empty"},
			{set("--effort", "extreme"), "effort is not allowed for adapter fake"},
			{set("--concurrency", "0"), "concurrency must be an integer from 1"},
			{set("--instruction", filepath.Join(w.dir, "absent.md")), "the instruction manual is not readable on this node"},
			{set("--runbook", filepath.Join(w.dir, "absent.md")), "the runbook manual is not readable on this node"},
		} {
			r := op.run(t, c.args...)
			if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, c.msg) {
				t.Fatalf("%v = %+v, want exit 2 with %q", c.args, r, c.msg)
			}
		}
		if roles := np.roles(t); len(roles) != 0 {
			t.Fatalf("a rejected registration published: %+v", roles)
		}
		// Free model text: spaces, quotes, a leading dash, no allowlist.
		model := `gpt-6 sol (preview) -x "q"`
		mustOK(t, op.run(t, set("--model", model)...))
		r := mustOK(t, op.run(t, append([]string{"role", "show", "a"}, np.trust()...)...))
		if !strings.Contains(r.stdout, "model: "+`"gpt-6 sol (preview) -x \"q\""`+"\n") || !strings.Contains(r.stdout, "registration_order: 1\n") {
			t.Fatalf("show = %q", r.stdout)
		}
	})
	t.Run("order", func(t *testing.T) {
		mustOK(t, op.run(t, roleAdd(np, "b", "coder", w.id, ins, run)...))
		mustOK(t, op.run(t, roleAdd(np, "c", "reviewer", w.id, ins, run, "--timeout", "0")...))
		mustOK(t, op.run(t, roleAdd(np, "d", "coder", w.id, ins, ins)...))
		got := func() []string {
			var out []string
			for _, v := range np.roles(t) {
				out = append(out, v.Name+":"+v.ID+":"+itoa(v.RegistrationOrder))
			}
			return out
		}
		if g := got(); strings.Join(g, ",") != "coder:a:1,coder:b:2,coder:d:4,reviewer:c:3" {
			t.Fatalf("grouped order = %v", g)
		}
		// Sticky: set changes only the given field, keeps the order.
		mustOK(t, op.run(t, append([]string{"role", "set", "b", "--model", "other model"}, np.trust()...)...))
		v, err := np.client(t).ShowRole(t.Context(), "b")
		if err != nil || v.Model != "other model" || v.Name != "coder" || v.Effort != "medium" || v.Concurrency != 2 || v.RegistrationOrder != 2 || v.Timeout != 2*time.Hour {
			t.Fatalf("after set = %+v %v", v, err)
		}
		// rm and re-add: a new, higher order; never reused.
		mustOK(t, op.run(t, append([]string{"role", "rm", "a"}, np.trust()...)...))
		mustOK(t, op.run(t, roleAdd(np, "a", "coder", w.id, ins, run)...))
		if g := got(); strings.Join(g, ",") != "coder:b:2,coder:d:4,coder:a:5,reviewer:c:3" {
			t.Fatalf("after re-add = %v", g)
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

// FP-2: the fake adapter is refused unless the worker enables it with an
// explicit path; enabled, the real built fixture is probed with no vendor
// CLI and no PATH.
func TestRoleAdapter(t *testing.T) {
	// Parallel: it waits for a real sidecar's next 5 s heartbeat to
	// report readiness; nothing here is shared with other tests.
	t.Parallel()
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	op := newNodeCLI(t)
	t.Run("disabled", func(t *testing.T) {
		w := startRoleWorker(t, p, np, "", nil)
		if w.proc.logs.has(sidecar.FakeAdapterWarning, nil) {
			t.Fatal("warning without the flag")
		}
		ins := w.manual(t, "i.md", "x")
		r := op.run(t, roleAdd(np, "a", "coder", w.id, ins, ins)...)
		if r.code != 2 || !strings.Contains(r.stderr, "fake adapter is disabled on node; start sidecar with --fake-adapter ABSOLUTE_PATH") {
			t.Fatalf("disabled add = %+v", r)
		}
		if roles := np.roles(t); len(roles) != 0 {
			t.Fatalf("published %+v", roles)
		}
	})
	t.Run("probe", func(t *testing.T) {
		env := slices.Clone(p.env)
		for i, kv := range env {
			if strings.HasPrefix(kv, "PATH=") {
				env[i] = "PATH="
			}
		}
		w := startRoleWorker(t, p, np, fakeAdapterBinary(t), env)
		if !w.proc.logs.has(sidecar.FakeAdapterWarning, nil) {
			t.Fatalf("no fake adapter warning:\n%s", w.proc.logs.String())
		}
		ins := w.manual(t, "i.md", "x")
		mustOK(t, op.run(t, roleAdd(np, "a", "coder", w.id, ins, ins)...))
		v := np.roles(t)[0]
		if !v.AdapterTestOnly || v.Adapter != "fake" {
			t.Fatalf("view = %+v", v)
		}
		np.awaitReady(t, "a")
	})
}

// FP-3: a coordinator in its own root registers worker-local manuals that
// only the worker checks; every failed check publishes nothing and no
// manual body appears on the coordinator, the plane or in any output.
func TestRoleValidation(t *testing.T) {
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	w := startRoleWorker(t, p, np, fakeAdapterBinary(t), nil)
	op := newNodeCLI(t)
	var outputs bytes.Buffer
	t.Run("remote", func(t *testing.T) {
		ins, run := w.manual(t, "manuals/instruction.md", roleSentinel), w.manual(t, "manuals/runbook.md", roleSentinel)
		r := mustOK(t, op.run(t, append(roleAdd(np, "worker-a", "implementer", w.id, ins, run), "--json")...))
		outputs.WriteString(r.stdout + r.stderr)
		v, err := contract.ParseRoleResponse(bytes.TrimSpace([]byte(r.stdout)), nil)
		if err != nil || v.Instruction != ins || v.Node != w.id {
			t.Fatalf("add = %+v %v", v, err)
		}
		if f := listTree(t, op.home); len(f) != 0 {
			t.Fatalf("the coordinator wrote local state: %v", f)
		}
	})
	t.Run("rejections", func(t *testing.T) {
		good := w.manual(t, "good.md", roleSentinel)
		locked := w.manual(t, "locked.md", roleSentinel)
		os.Chmod(locked, 0o000)
		fifo := filepath.Join(w.dir, "fifo.md")
		syscall.Mkfifo(fifo, 0o644)
		lockedReadable := false
		if f, err := os.Open(locked); err == nil {
			f.Close()
			lockedReadable = true // e.g. root: the actual open decides
		}
		wrong := filepath.Join(t.TempDir(), "wrong-adapter")
		os.WriteFile(wrong, []byte("#!/bin/sh\necho "+roleSentinel+"; echo "+roleSentinel+" >&2\n"), 0o755)
		bad := startRoleWorker(t, p, np, wrong, nil)
		badManual := bad.manual(t, "m.md", roleSentinel)
		offline := np.enroll(t, p, filepath.Join(t.TempDir(), "offline"))
		for _, c := range []struct {
			name string
			args []string
			code int
			msg  string
		}{
			{"missing", roleAdd(np, "r1", "coder", w.id, filepath.Join(w.dir, "none.md"), good), 2, "the instruction manual is not readable on this node"},
			{"directory", roleAdd(np, "r2", "coder", w.id, good, w.dir), 2, "the runbook manual is not a regular file on this node"},
			{"fifo", roleAdd(np, "r3", "coder", w.id, fifo, good), 2, "the instruction manual is not a regular file on this node"},
			{"probe", roleAdd(np, "r4", "coder", bad.id, badManual, badManual), 2, "the fake adapter cannot be invoked on this node"},
			{"offline", roleAdd(np, "r5", "coder", offline, good, good), 5, "is offline"},
			{"unknown-node", roleAdd(np, "r6", "coder", "n_"+strings.Repeat("e", 32), good, good), 3, "is not enrolled"},
		} {
			r := op.run(t, c.args...)
			outputs.WriteString(r.stdout + r.stderr)
			if r.code != c.code || r.stdout != "" || !strings.Contains(r.stderr, c.msg) {
				t.Fatalf("%s = %+v", c.name, r)
			}
		}
		r := op.run(t, roleAdd(np, "r7", "coder", w.id, locked, good)...)
		outputs.WriteString(r.stdout + r.stderr)
		if lockedReadable != (r.code == 0) || (!lockedReadable && !strings.Contains(r.stderr, "permission denied")) {
			t.Fatalf("mode-000 manual (readable here: %v) = %+v", lockedReadable, r)
		}
		var ids []string
		for _, v := range np.roles(t) {
			ids = append(ids, v.ID)
		}
		want := "worker-a"
		if lockedReadable {
			want = "r7,worker-a"
		}
		if strings.Join(ids, ",") != want {
			t.Fatalf("published after rejections: %v", ids)
		}
		for what, text := range map[string]string{"cli": outputs.String(), "plane": np.proc.logs.String(), "worker": w.proc.logs.String(), "bad worker": bad.proc.logs.String()} {
			if strings.Contains(text, roleSentinel) {
				t.Fatalf("a manual body or child output reached the %s:\n%s", what, text)
			}
		}
	})
}

// FP-4: the duplex, bounded protocol-2 stream, delegated to the plane and
// sidecar contracts with real WSS; a protocol-1 peer is refused with both
// versions named.
func TestRoleProtocol(t *testing.T) {
	t.Run("duplex", func(t *testing.T) {
		roleDelegate(t, "TestRoleProtocol/duplex")
		p := newNodeCLI(t)
		np := startNodePlane(t, p)
		id := np.enroll(t, p, filepath.Join(t.TempDir(), "s"))
		s := dialPeer(t, np)
		s.send(1, contract.FrameHello, "h1", contract.HelloBody{NodeID: id, SoftwareVersion: "iteration-03"})
		f, e := s.recv()
		if e == nil || e.Code != contract.CodeProtocolMismatch || e.Message != "protocol version mismatch: local=4 remote=1" || f.Version != 4 {
			t.Fatalf("protocol-1 hello = %+v %v", f, e)
		}
	})
	t.Run("bounds", func(t *testing.T) {
		roleDelegate(t, "TestRoleProtocol/bounds")
	})
}

// FP-5: distribution and readiness, delegated: the worker's fake-clock
// cycles toggle readiness on deletion and recovery, set/rm replacements
// are exact, and a reconnect gets a full replacement.
func TestRoleReadiness(t *testing.T) {
	t.Run("changes", func(t *testing.T) {
		roleDelegate(t, "TestRoleReadiness/changes")
	})
	t.Run("reconnect", func(t *testing.T) {
		roleDelegate(t, "TestRoleReadiness/reconnect")
	})
}

// FP-6: a stopped-root backup restores configuration, order, CA and node
// identity, with readiness false until the node reports again; publication
// failures are delegated.
func TestRolePersistence(t *testing.T) {
	// Parallel: it waits for a real sidecar's next 5 s heartbeat to
	// report readiness; nothing here is shared with other tests.
	t.Parallel()
	t.Run("restore", func(t *testing.T) {
		p := newNodeCLI(t)
		np := startNodePlane(t, p)
		w := startRoleWorker(t, p, np, fakeAdapterBinary(t), nil)
		ins := w.manual(t, "i.md", "x")
		for _, id := range []string{"b", "a", "c"} {
			mustOK(t, p.run(t, roleAdd(np, id, "coder", w.id, ins, ins)...))
		}
		mustOK(t, p.run(t, append([]string{"role", "rm", "a"}, np.trust()...)...))
		np.awaitReady(t, "b", "c")
		before := np.roles(t)
		w.proc.stop(t, syscall.SIGTERM)
		if code := np.proc.stop(t, syscall.SIGTERM); code != 130 {
			t.Fatalf("plane exit %d", code)
		}
		backup := filepath.Join(t.TempDir(), "backup")
		copyDir(t, np.root, backup)
		sameFiles(t, stateFiles(t, np.root), stateFiles(t, backup))
		pp := p.start(t, "--state-dir", backup)
		if listeningRecords(pp.logs.String())[0]["ca_fingerprint"] != np.fp {
			t.Fatal("restored plane has another CA")
		}
		restored := &nodePlane{root: backup, url: "https://" + pp.addr, ca: filepath.Join(backup, "pki", "ca.crt"), fp: np.fp, proc: pp}
		after := restored.roles(t)
		if len(after) != 2 || len(before) != 2 {
			t.Fatalf("restored %+v from %+v", after, before)
		}
		for i := range after {
			if !contract.SameRoleConfig(after[i].RoleConfig, before[i].RoleConfig) || after[i].RegistrationOrder != before[i].RegistrationOrder || after[i].CanAccept || after[i].NodeLiveness != contract.LivenessOffline {
				t.Fatalf("restored %+v, was %+v", after[i], before[i])
			}
		}
		if n, err := restored.client(t).ShowNode(t.Context(), w.id); err != nil || len(n.Roles) != 2 || n.Roles[0].RoleID != "b" || n.Roles[0].CanAccept {
			t.Fatalf("restored node = %+v %v", n, err)
		}
		// Counters survive: the next add gets order 4 (a's 2 is never reused).
		ww := startRoleWorkerAt(t, p, restored, w)
		mustOK(t, p.run(t, roleAdd(restored, "a", "coder", w.id, ins, ins)...))
		if v, _ := restored.client(t).ShowRole(t.Context(), "a"); v.RegistrationOrder != 4 {
			t.Fatalf("re-added order %d", v.RegistrationOrder)
		}
		ww.proc.stop(t, syscall.SIGTERM)
	})
	t.Run("failures", func(t *testing.T) {
		roleDelegate(t, "TestRolePersistence/failures")
	})
}

// startRoleWorkerAt re-enrolls w's sidecar state with np and runs it.
func startRoleWorkerAt(t *testing.T, p *planeCLI, np *nodePlane, w *roleWorker) *roleWorker {
	t.Helper()
	if id := np.enroll(t, p, w.state); id != w.id {
		t.Fatalf("identity changed: %s", id)
	}
	proc := startProc(t, p.bin, p.cwd, p.env, []string{"sidecar", "run", "--state-dir", w.state, "--fake-adapter", fakeAdapterBinary(t)}, "heartbeat acknowledged")
	return &roleWorker{id: w.id, state: w.state, dir: w.dir, proc: proc}
}

// FP-7: the five operations through the real CLI from two independent
// operator roots; the grouped roster and node discovery agree; text and
// JSON are exact; CA and pin trust work and wrong trust fails with exit 6.
func TestRoleCommands(t *testing.T) {
	// Parallel: it waits for a real sidecar's next 5 s heartbeat to
	// report readiness; nothing here is shared with other tests.
	t.Parallel()
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	w := startRoleWorker(t, p, np, fakeAdapterBinary(t), nil)
	a, b := newNodeCLI(t), newNodeCLI(t) // two coordinators, no node state
	ins, run := w.manual(t, "instruction.md", "i"), w.manual(t, "runbook.md", "r")
	t.Run("text", func(t *testing.T) {
		mustOK(t, a.run(t, roleAdd(np, "r1", "coder", w.id, ins, run)...))
		mustOK(t, a.run(t, roleAdd(np, "r2", "reviewer", w.id, ins, run, "--model", "review model")...))
		r := mustOK(t, a.run(t, roleAdd(np, "r3", "coder", w.id, ins, run, "--timeout", "90m")...))
		want := "id: r3\nname: coder\nnode: " + w.id + "\nadapter: fake\ninstruction: " + mustJSONString(t, ins) + "\nrunbook: " + mustJSONString(t, run) +
			"\nmodel: \"example model\"\neffort: medium\nconcurrency: 2\ntimeout: 1h30m0s\nregistration_order: 3\ninflight: 0\ncan_accept: false\nnode_liveness: online\nadapter_test_only: true\n"
		if r.stdout != want {
			t.Fatalf("add text:\n%s\nwant:\n%s", r.stdout, want)
		}
		np.awaitReady(t, "r1", "r2", "r3")
		r = mustOK(t, b.run(t, append([]string{"role", "ls"}, np.trust()...)...))
		row := func(name, id, model string) string {
			return name + "\t" + id + "\t" + w.id + "\tonline\tfake\t" + model + "\tmedium\t0\t2\ttrue\n"
		}
		wantLs := "NAME\tID\tNODE\tNODE_LIVENESS\tADAPTER\tMODEL\tEFFORT\tINFLIGHT\tCONCURRENCY\tCAN_ACCEPT\n" +
			row("coder", "r1", `"example model"`) + row("coder", "r3", `"example model"`) + row("reviewer", "r2", `"review model"`)
		if r.stdout != wantLs {
			t.Fatalf("ls from the other root:\n%s\nwant:\n%s", r.stdout, wantLs)
		}
		r = mustOK(t, b.run(t, append([]string{"node", "show", w.id}, np.trust()...)...))
		if !strings.Contains(r.stdout, `roles: [{"role_id":"r1","inflight":0,"concurrency":2,"can_accept":true},{"role_id":"r2","inflight":0,"concurrency":2,"can_accept":true},{"role_id":"r3","inflight":0,"concurrency":2,"can_accept":true}]`+"\n") {
			t.Fatalf("node roles:\n%s", r.stdout)
		}
		r = mustOK(t, b.run(t, append([]string{"role", "set", "r1", "--concurrency", "3"}, np.trust()...)...))
		if !strings.Contains(r.stdout, "concurrency: 3\n") || !strings.Contains(r.stdout, "registration_order: 1\n") {
			t.Fatalf("set text:\n%s", r.stdout)
		}
		if r := mustOK(t, a.run(t, append([]string{"role", "show", "r1"}, np.trust()...)...)); !strings.Contains(r.stdout, "concurrency: 3\n") {
			t.Fatalf("show from the first root:\n%s", r.stdout)
		}
		if r := mustOK(t, b.run(t, append([]string{"role", "rm", "r3"}, np.trust()...)...)); r.stdout != "removed: r3\n"+rmNotice {
			t.Fatalf("rm = %q", r.stdout)
		}
	})
	t.Run("json", func(t *testing.T) {
		r := mustOK(t, a.run(t, append(roleAdd(np, "r4", "coder", w.id, ins, run), "--json")...))
		v, err := contract.ParseRoleResponse(bytes.TrimSuffix([]byte(r.stdout), []byte("\n")), nil)
		if err != nil || strings.Count(r.stdout, "\n") != 1 || v.ID != "r4" || v.RegistrationOrder != 4 {
			t.Fatalf("add json = %q %v", r.stdout, err)
		}
		if again, _ := contract.Encode(contract.RoleResponse{Version: 4, Role: v}); string(again)+"\n" != r.stdout {
			t.Fatalf("not the compact envelope: %q", r.stdout)
		}
		r = mustOK(t, b.run(t, append([]string{"role", "ls", "--json"}, np.trust()...)...))
		list, err := contract.ParseRoleListResponse(bytes.TrimSuffix([]byte(r.stdout), []byte("\n")), nil)
		if err != nil || len(list) != 3 || list[0].ID != "r1" || list[1].ID != "r4" || list[2].ID != "r2" {
			t.Fatalf("ls json = %q %v", r.stdout, err)
		}
		if again, _ := contract.Encode(contract.RoleListResponse{Version: 4, Roles: list}); string(again)+"\n" != r.stdout {
			t.Fatalf("not the compact list envelope: %q", r.stdout)
		}
		if r := mustOK(t, b.run(t, append([]string{"role", "show", "r2", "--json"}, np.trust()...)...)); !strings.HasPrefix(r.stdout, `{"version":4,"role":{"id":"r2",`) {
			t.Fatalf("show json = %q", r.stdout)
		}
		if r := mustOK(t, a.run(t, append([]string{"role", "rm", "--json", "r4", "--force"}, np.trust()...)...)); r.stdout != `{"version":4,"removed":"r4"}`+"\n" {
			t.Fatalf("rm json = %q", r.stdout)
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(mustOK(t, b.run(t, append([]string{"node", "ls", "--json"}, np.trust()...)...)).stdout), &decoded) != nil {
			t.Fatal("node ls json")
		}
	})
	t.Run("trust", func(t *testing.T) {
		pin := []string{"--plane", np.url, "--ca-fingerprint", np.fp}
		if r := mustOK(t, b.run(t, append([]string{"role", "ls"}, pin...)...)); !strings.Contains(r.stdout, "\tr1\t") {
			t.Fatalf("ls with pin = %q", r.stdout)
		}
		wrong := "sha256:" + strings.Repeat("0", 64)
		for _, args := range [][]string{
			{"role", "ls", "--plane", np.url, "--ca-fingerprint", wrong},
			{"role", "show", "r1", "--plane", np.url},
			{"role", "rm", "r1", "--plane", np.url},
		} {
			if r := b.run(t, args...); r.code != 6 || r.stdout != "" || !strings.Contains(r.stderr, "connection not trusted") {
				t.Fatalf("%v = %+v", args, r)
			}
		}
		// The observers needed no node state and wrote nothing locally.
		for _, c := range []*planeCLI{a, b} {
			if f := listTree(t, c.home); len(f) != 0 {
				t.Fatalf("a coordinator wrote %v", f)
			}
		}
	})
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := contract.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// FP-8: serialized mutations (delegated) and removal online and offline,
// with and without --force, identically and without any cancellation or
// task artifact.
func TestRoleMutation(t *testing.T) {
	t.Run("races", func(t *testing.T) {
		roleDelegate(t, "TestRoleMutation/races")
	})
	t.Run("remove", func(t *testing.T) {
		p := newNodeCLI(t)
		np := startNodePlane(t, p)
		w := startRoleWorker(t, p, np, fakeAdapterBinary(t), nil)
		ins := w.manual(t, "i.md", "x")
		for _, id := range []string{"r1", "r2", "r3", "r4"} {
			mustOK(t, p.run(t, roleAdd(np, id, "coder", w.id, ins, ins)...))
		}
		sidecarFiles := listTree(t, w.state)
		for i, force := range []bool{false, true} {
			id := []string{"r1", "r2"}[i]
			args := append([]string{"role", "rm", id}, np.trust()...)
			if force {
				args = append(args, "--force")
			}
			if r := mustOK(t, p.run(t, args...)); r.stdout != "removed: "+id+"\n"+rmNotice {
				t.Fatalf("online rm = %+v", r)
			}
		}
		// The worker stays connected and heartbeating: no cancellation.
		if w.proc.logs.has("disconnected; retrying", nil) {
			t.Fatalf("worker disconnected:\n%s", w.proc.logs.String())
		}
		w.proc.stop(t, syscall.SIGTERM)
		for i, force := range []bool{false, true} {
			id := []string{"r3", "r4"}[i]
			args := append([]string{"role", "rm", id}, np.trust()...)
			if force {
				args = append(args, "--force")
			}
			if r := mustOK(t, p.run(t, args...)); r.stdout != "removed: "+id+"\n"+rmNotice {
				t.Fatalf("offline rm = %+v", r)
			}
			if r := p.run(t, args...); r.code != 3 || !strings.Contains(r.stderr, "does not exist") {
				t.Fatalf("second rm = %+v", r)
			}
		}
		if roles := np.roles(t); len(roles) != 0 {
			t.Fatalf("left %+v", roles)
		}
		// No task, cancellation or other artifact on either side.
		if got := listTree(t, w.state); !slices.Equal(got, sidecarFiles) {
			t.Fatalf("sidecar state changed: %v -> %v", sidecarFiles, got)
		}
		for _, f := range listTree(t, np.root) {
			if top := strings.Split(f, string(filepath.Separator))[0]; !slices.Contains([]string{"config.json", "pki", "tmp", "nodes", "roles", ".lock"}, top) {
				t.Fatalf("unexpected plane artifact %s", f)
			}
		}
	})
}

// FP-9: native manual and executable semantics on this host and the
// devcheck plans, evidence and source guards, delegated.
func TestRolePlatform(t *testing.T) {
	t.Run("manuals", func(t *testing.T) {
		roleDelegate(t, "TestRolePlatform/manuals")
	})
	t.Run("executable", func(t *testing.T) {
		roleDelegate(t, "TestRolePlatform/executable")
	})
	t.Run("policy", func(t *testing.T) {
		roleDelegate(t, "TestRolePlatform/policy")
	})
}
