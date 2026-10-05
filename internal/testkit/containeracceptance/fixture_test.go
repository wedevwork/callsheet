package containeracceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

// fakePlane is the host unit tests' injected command and process fixture:
// an in-process stand-in for the plane, the two sidecars, the scripted fake
// worker and the coordinator CLI. It never starts a process, builds a
// binary or opens a socket: the suite's seams (deployment, command run,
// process table) reach it instead. Its answers are the CLI's --json
// documents built from the contract types, so the suite's strict parsers
// decode them exactly as they decode the real CLI's; its workspaces are
// real go-git objects (a pull writes them into the destination
// repository), and the scripted worker writes its real report and barrier
// files. It is a fixture, not a model of Callsheet: the real behaviour is
// proven only by the tagged container package.
//
// Fault injection: startErr, sidecarErr and restartErr fail the process
// operations; fail returns a nonzero exit for the n-th CLI call; mutate
// rewrites any answer; survivors are reported by the process table.
type fakePlane struct {
	cs   string // the configured callsheet executable
	dir  string
	url  string
	ca   string
	when string

	mu       sync.Mutex
	nodes    map[string]*fakeNode // by alias
	roles    map[string]*fakeRole
	order    int
	revision int // the registry revision (bumped by every registration)
	ws       map[string]*fakeWS
	tasks    map[string]*fakeTask
	seq      int
	calls    []string
	closed   bool

	startErr   error
	sidecarErr map[string]error
	restartErr error
	closeErr   error
	keepWork   bool
	survivors  []string
	// fail is the 1-based CLI call that exits 1 (0: none).
	fail   int
	mutate func(args []string, r *fakeAnswer)
}

// fakeAnswer is one CLI call's exit code and output.
type fakeAnswer struct {
	code           int
	stdout, stderr string
}

type fakeNode struct {
	alias, name string
	n           *node
	pid         int
	synced      int // the registry revision the sidecar last attached with
}

type fakeRole struct {
	rec  contract.RoleRecord
	node *fakeNode
}

type fakeWS struct {
	name, instance, generation string
	store                      *memory.Storage
	main                       plumbing.Hash
	refs                       map[string]plumbing.Hash // task ID -> result
}

type fakeTask struct {
	id      string
	role    *fakeRole
	req     contract.DispatchRequest
	eff     contract.TaskEffective
	ws      *fakeWS
	base    plumbing.Hash
	script  fakeadapter.WorkspaceScript
	files   map[string]string
	added   []string
	pc      int
	state   string
	exit    *int
	final   *string
	timeout bool
	commit  plumbing.Hash
	pub     string
	work    string
}

// newFakePlane is a fixture answering for the callsheet executable cs.
func newFakePlane(cs string) *fakePlane {
	return &fakePlane{cs: cs, when: "2026-10-05T12:00:00Z", nodes: map[string]*fakeNode{}, roles: map[string]*fakeRole{}, ws: map[string]*fakeWS{},
		tasks: map[string]*fakeTask{}, sidecarErr: map[string]error{}}
}

// seams returns the suite seams that reach this fixture.
func (f *fakePlane) seams() seams {
	return seams{start: f.start, run: f.run, live: f.live}
}

func (f *fakePlane) start(_ context.Context, bin, dir string, _ []string) (deployment, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	if bin != f.cs {
		return nil, fmt.Errorf("plane init: %s is not the callsheet executable", bin)
	}
	f.dir = dir
	f.ca = filepath.Join(dir, "plane", "pki", "ca.crt")
	if err := os.MkdirAll(filepath.Dir(f.ca), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(f.ca, []byte("synthetic CA\n"), 0o600); err != nil {
		return nil, err
	}
	f.url = "https://127.0.0.1:47011"
	return f, nil
}

func (f *fakePlane) origin() (string, string) { return f.url, f.ca }

func (f *fakePlane) startSidecar(_ context.Context, name string, _ []string, args ...string) (*node, error) {
	if err := f.sidecarErr[name]; err != nil {
		return nil, err
	}
	if len(args) != 2 || args[0] != "--fake-adapter" {
		return nil, fmt.Errorf("sidecar %s: unexpected flags %v", name, args)
	}
	if st, err := os.Stat(args[1]); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("sidecar %s refuses the fake adapter %s", name, args[1])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Join(state, "tasks"), 0o700); err != nil {
		return nil, err
	}
	fn := &fakeNode{alias: strings.TrimPrefix(name, "node-"), name: name, pid: 4100 + 10*len(f.nodes)}
	fn.n = &node{NodeID: fmt.Sprintf("n_%032x", 0xa0+len(f.nodes)), State: state, pid: func() int { return fn.pid }}
	f.nodes[fn.alias] = fn
	return fn.n, nil
}

// restart restarts a sidecar: its running tasks are lost (their work
// removed) and it attaches with the current registry revision.
func (f *fakePlane) restart(_ context.Context, n *node) error {
	if f.restartErr != nil {
		return f.restartErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fn := range f.nodes {
		if fn.n != n {
			continue
		}
		fn.pid++
		fn.synced = f.revision
		for _, t := range f.tasks {
			if t.role.node == fn && t.state == contract.TaskRunning {
				t.state, t.pub = contract.TaskLost, contract.PublicationNotApplicable
				os.RemoveAll(filepath.Dir(t.work))
			}
		}
		return nil
	}
	return errors.New("restart of an unknown sidecar")
}

// close stops the sidecars (their running tasks' work removed unless
// keepWork) and the plane.
func (f *fakePlane) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if !f.keepWork {
		for _, t := range f.tasks {
			if t.work != "" {
				os.RemoveAll(filepath.Dir(t.work))
			}
		}
	}
	return f.closeErr
}

func (f *fakePlane) live(map[string]bool, []int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.survivors, nil
}

// run answers one coordinator CLI command.
func (f *fakePlane) run(ctx context.Context, c command) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	if c.bin != f.cs {
		// A broken coordinator CLI: not the callsheet executable.
		io.WriteString(c.stderr, "usage: fake-adapter\n")
		return 2, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.dir == "" || len(c.env) == 0 {
		return -1, errors.New("the coordinator command has no directory or environment")
	}
	f.calls = append(f.calls, strings.Join(c.args[:min(2, len(c.args))], " "))
	var a fakeAnswer
	if f.fail == len(f.calls) {
		a = fakeAnswer{code: 1, stderr: "callsheet: synthetic failure\n"}
	} else {
		a = f.answer(c.args)
	}
	if f.mutate != nil {
		f.mutate(c.args, &a)
	}
	io.WriteString(c.stdout, a.stdout)
	io.WriteString(c.stderr, a.stderr)
	return a.code, nil
}

// cliArgs is a parsed command line: positionals, flag values and --json.
type cliArgs struct {
	pos   []string
	flags map[string]string
	json  bool
}

func parseCLI(args []string) cliArgs {
	a := cliArgs{flags: map[string]string{}}
	for i := 0; i < len(args); i++ {
		switch x := args[i]; {
		case x == "--json":
			a.json = true
		case strings.HasPrefix(x, "--") && i+1 < len(args):
			a.flags[x[2:]] = args[i+1]
			i++
		default:
			a.pos = append(a.pos, x)
		}
	}
	return a
}

func (a cliArgs) arg(i int) string {
	if i < len(a.pos) {
		return a.pos[i]
	}
	return ""
}

// doc renders a --json answer.
func doc(v any) fakeAnswer {
	b, err := json.Marshal(v)
	if err != nil {
		return fakeAnswer{code: 1, stderr: err.Error()}
	}
	return fakeAnswer{stdout: string(b) + "\n"}
}

func refused(code int, format string, args ...any) fakeAnswer {
	return fakeAnswer{code: code, stderr: "callsheet: " + fmt.Sprintf(format, args...) + "\n"}
}

func (f *fakePlane) answer(args []string) fakeAnswer {
	a := parseCLI(args)
	if a.flags["plane"] != f.url {
		return refused(5, "the plane is unreachable")
	}
	if a.flags["ca"] != f.ca {
		return refused(6, "TLS verification of %s failed", f.url)
	}
	f.advance()
	switch verb := a.arg(0) + " " + a.arg(1); {
	case verb == "node ls":
		return f.nodeList()
	case verb == "role add":
		return f.roleAdd(a)
	case verb == "role show":
		r := f.roles[a.arg(2)]
		if r == nil {
			return refused(4, "role %s not found", a.arg(2))
		}
		return doc(contract.RoleResponse{Version: contract.ProtocolVersion, Role: f.roleView(r)})
	case a.arg(0) == "dispatch":
		return f.dispatch(a)
	case verb == "task show", verb == "task wait", verb == "task logs", verb == "task cancel":
		t := f.tasks[a.arg(2)]
		if t == nil {
			return refused(4, "task %s not found", a.arg(2))
		}
		return f.taskVerb(a.arg(1), t)
	case verb == "ws create":
		return f.wsCreate(a.arg(2))
	case verb == "ws push":
		return f.wsPush(a)
	case verb == "ws status", verb == "ws diff":
		if w := f.ws[a.arg(2)]; w != nil && verb == "ws status" {
			return doc(f.wsStatus(w))
		}
		t := f.tasks[a.arg(2)]
		if t == nil || t.ws == nil {
			return refused(4, "%s not found", a.arg(2))
		}
		if verb == "ws status" {
			return doc(contract.TaskWorkspaceStatusResponse{Version: contract.ProtocolVersion, Workspace: f.taskStatus(t)})
		}
		return doc(contract.WorkspaceDiffResponse{Name: t.ws.name, Instance: t.ws.instance, Generation: t.ws.generation, BaseCommit: hashp(t.base),
			TargetCommit: t.commit.String(), Changes: changes(t)})
	case verb == "ws pull":
		return f.wsPull(a)
	}
	return refused(2, "unknown command %v", a.pos)
}

func hashp(h plumbing.Hash) *string {
	if h.IsZero() {
		return nil
	}
	s := h.String()
	return &s
}

func (f *fakePlane) nodeList() fakeAnswer {
	var nodes []contract.Node
	pv, sv := contract.ProtocolVersion, "dev"
	now, _ := time.Parse(time.RFC3339, f.when)
	for _, fn := range f.nodes {
		nodes = append(nodes, contract.Node{ID: fn.n.NodeID, Liveness: contract.LivenessOnline, LastSeen: &now, ProtocolVersion: &pv, SoftwareVersion: &sv})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return doc(contract.NodeListResponse{Version: contract.ProtocolVersion, Nodes: nodes})
}

func (f *fakePlane) roleAdd(a cliArgs) fakeAnswer {
	var fn *fakeNode
	for _, n := range f.nodes {
		if n.n.NodeID == a.flags["node"] {
			fn = n
		}
	}
	if fn == nil || f.roles[a.arg(2)] != nil {
		return refused(4, "role %s cannot be added", a.arg(2))
	}
	var conc int
	fmt.Sscan(a.flags["concurrency"], &conc)
	f.order++
	f.revision++
	r := &fakeRole{node: fn, rec: contract.RoleRecord{RegistrationOrder: f.order, RoleConfig: contract.RoleConfig{ID: a.arg(2), Name: a.flags["name"],
		Node: fn.n.NodeID, Adapter: a.flags["adapter"], Instruction: a.flags["instruction"], Runbook: a.flags["runbook"], Model: a.flags["model"],
		Effort: a.flags["effort"], Concurrency: conc}}}
	f.roles[r.rec.ID] = r
	return doc(contract.RoleResponse{Version: contract.ProtocolVersion, Role: f.roleView(r)})
}

// roleView: a role accepts once node a attached with the current registry
// revision (the documented registry-revision workaround).
func (f *fakePlane) roleView(r *fakeRole) contract.RoleView {
	return contract.RoleView{RoleRecord: r.rec, CanAccept: f.nodes["a"].synced >= f.revision, NodeLiveness: contract.LivenessOnline}
}

func (f *fakePlane) dispatch(a cliArgs) fakeAnswer {
	r := f.roles[a.flags["role-id"]]
	if r == nil {
		return refused(4, "role %s not found", a.flags["role-id"])
	}
	f.seq++
	t := &fakeTask{id: fmt.Sprintf("t_%032x", f.seq), role: r, state: contract.TaskPending, files: map[string]string{},
		eff: contract.TaskEffective{Model: r.rec.Model, Effort: r.rec.Effort, Timeout: 2 * time.Hour}}
	t.req = contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: r.rec.ID}, Goal: a.flags["goal"], Payload: []string{},
		Acceptance: a.flags["acceptance"], RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "dev", Hostname: "container"}}
	if d, ok := a.flags["timeout"]; ok {
		to, err := time.ParseDuration(d)
		if err != nil {
			return refused(2, "invalid timeout")
		}
		t.timeout, t.eff.Timeout = true, to
		t.req.Override = &contract.TaskOverride{Timeout: &to}
	}
	if name, ok := a.flags["workspace"]; ok {
		w := f.ws[name]
		if w == nil || a.flags["workspace-instance"] != w.instance {
			return refused(4, "workspace %s instance mismatch", name)
		}
		base, inst := a.flags["base"], w.instance
		t.ws, t.base = w, plumbing.NewHash(base)
		t.req.Workspace, t.req.Base, t.req.WorkspaceInstance = &name, &base, &inst
		files, err := testkit.TreeFiles(w.store, t.base)
		if err != nil {
			return refused(4, "base %s: %v", base, err)
		}
		for p, spec := range files {
			t.files[p] = string(spec.Content)
		}
		t.work = filepath.Join(r.node.n.State, "tasks", t.id, "work")
		os.MkdirAll(t.work, 0o700)
	}
	if body, ok := strings.CutPrefix(t.req.Goal, fakeadapter.WorkspaceGoalPrefix); ok {
		if err := json.Unmarshal([]byte(body), &t.script); err != nil {
			return refused(2, "invalid script")
		}
	}
	f.tasks[t.id] = t
	admitted := f.view(t)
	t.state = contract.TaskRunning
	f.step(t)
	return doc(contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: t.id, Task: &admitted})
}

// advance continues every running task whose barrier appeared and times
// out a held task with a timeout.
func (f *fakePlane) advance() {
	for _, t := range f.tasks {
		if t.state != contract.TaskRunning {
			continue
		}
		if t.timeout {
			f.finish(t, contract.TaskTimedOut, nil, nil)
			continue
		}
		f.step(t)
	}
}

// step runs t's script from its current op until a missing barrier holds
// it or it ends.
func (f *fakePlane) step(t *fakeTask) {
	sc := t.script
	if t.pc == 0 && sc.Report != "" {
		rep := fakeadapter.WorkspaceReport{CWD: t.work, CWDMode: "0700", HasGit: true, Files: map[string]string{}}
		for p, c := range t.files {
			rep.Files[p] = "f 0644 " + sha(c)
			for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
				rep.Files[d] = "d 0755 " + sha("")
			}
		}
		b, _ := json.Marshal(rep)
		os.WriteFile(sc.Report, b, 0o600)
	}
	for ; t.pc < len(sc.Ops); t.pc++ {
		op := sc.Ops[t.pc]
		switch {
		case op.Write != "":
			if _, ok := t.files[op.Write]; !ok {
				t.added = append(t.added, op.Write)
			}
			t.files[op.Write] = op.Data
		case op.Touch != "":
			os.WriteFile(op.Touch, nil, 0o600)
		case op.WaitFor != "":
			if _, err := os.Stat(op.WaitFor); err != nil {
				return
			}
		}
	}
	if sc.Exit != 0 {
		f.finish(t, contract.TaskFailed, &sc.Exit, nil)
		return
	}
	zero, msg := 0, FinalMessage
	f.finish(t, contract.TaskSucceeded, &zero, &msg)
}

// finish ends t and publishes its files (when it has a workspace) as the
// sidecar's result commit on its base.
func (f *fakePlane) finish(t *fakeTask, state string, exit *int, final *string) {
	t.state, t.exit, t.final = state, exit, final
	if t.ws == nil {
		return
	}
	specs := map[string]testkit.FileSpec{}
	for p, c := range t.files {
		specs[p] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte(c)}
	}
	tree, err := testkit.WriteTree(t.ws.store, specs)
	if err == nil {
		sig := object.Signature{Name: "Callsheet", Email: "workspace@callsheet.invalid", When: time.Unix(0, 0).UTC()}
		msg := "Callsheet task result\ntask: " + t.id + "\nrole: " + t.role.rec.ID + "\n"
		t.commit, err = testkit.WriteObject(t.ws.store, &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree, ParentHashes: []plumbing.Hash{t.base}})
	}
	if err != nil {
		panic(err)
	}
	t.ws.refs[t.id] = t.commit
	t.pub = contract.PublicationPublished
	os.RemoveAll(filepath.Dir(t.work))
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// changes is t's diff: its added paths in raw byte order.
func changes(t *fakeTask) []contract.WorkspaceChange {
	paths := append([]string(nil), t.added...)
	sort.Strings(paths)
	out := []contract.WorkspaceChange{}
	mode := "100644"
	for _, p := range paths {
		n := int64(len(t.files[p]))
		out = append(out, contract.WorkspaceChange{PathBase64: contract.EncodePath([]byte(p)), Kind: contract.ChangeAdded, NewMode: &mode, NewBytes: &n})
	}
	return out
}

// fakeWSResult is t's terminal workspace result.
func fakeWSResult(t *fakeTask) *contract.TaskWorkspaceResult {
	w := &contract.TaskWorkspaceResult{Name: t.ws.name, Instance: t.ws.instance, BaseCommit: hashp(t.base), Publication: t.pub, Changes: []contract.WorkspaceChange{}}
	if t.pub == contract.PublicationPublished {
		ref := contract.TaskRefPrefix + t.id
		w.Commit, w.Ref, w.Changes = hashp(t.commit), &ref, changes(t)
		ds := &contract.TaskDiffstat{}
		for _, c := range w.Changes {
			ds.Added++
			ds.NewBytes += *c.NewBytes
		}
		w.Diffstat = ds
	}
	return w
}

func (f *fakePlane) binding(t *fakeTask) *contract.WorkspaceBinding {
	if t.ws == nil {
		return nil
	}
	return &contract.WorkspaceBinding{Name: t.ws.name, Instance: t.ws.instance, BaseSelector: t.base.String(), BaseCommit: hashp(t.base)}
}

// view is t's task view.
func (f *fakePlane) view(t *fakeTask) contract.TaskView {
	now := f.when
	v := contract.TaskView{TaskID: t.id, Request: t.req, Role: contract.PublicRole(t.role.rec), Effective: t.eff, TimeoutPolicy: contract.TimeoutPolicyEnforced,
		State: t.state, CreatedAt: now, DurabilityConfirmed: true, WorkspaceBinding: f.binding(t)}
	if t.state != contract.TaskPending {
		v.StartedAt = &now
	}
	if !contract.TaskTerminal(t.state) {
		v.WorkspacePhase = contract.WorkspacePhaseOf(v.WorkspaceBinding, t.state, false)
		return v
	}
	v.FinishedAt = &now
	if t.state == contract.TaskLost {
		v.Reason = &contract.TaskReason{Code: contract.ReasonLeaseExpired, Message: "the sidecar restarted"}
	}
	r := contract.TaskResult{State: t.state, ExitCode: t.exit, FinalMessage: t.final}
	if t.ws != nil {
		r = r.WithWorkspace(fakeWSResult(t))
	}
	v.Result = &r
	return v
}

func (f *fakePlane) taskVerb(verb string, t *fakeTask) fakeAnswer {
	switch verb {
	case "show":
		return doc(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: f.view(t)})
	case "wait":
		if contract.TaskTerminal(t.state) {
			v := f.view(t)
			return doc(contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, Winner: t.id, Task: &v})
		}
		return doc(contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitStillRunning,
			Tasks: []contract.WaitRow{{TaskID: t.id, State: t.state}}})
	case "logs":
		data := []byte(fakeadapter.FinalMarker)
		return doc(contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: t.id, Data: data, RetainedBytes: len(data), SourceBytes: len(data)})
	}
	if t.state != contract.TaskRunning {
		return refused(4, "task %s is %s", t.id, t.state)
	}
	f.finish(t, contract.TaskCancelled, nil, nil)
	return doc(contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: t.id, Accepted: true, Task: f.view(t)})
}

func (f *fakePlane) wsCreate(name string) fakeAnswer {
	if !contract.ValidWorkspaceName(name) {
		return refused(2, "invalid workspace name")
	}
	if f.ws[name] != nil {
		return refused(4, "workspace %s exists", name)
	}
	f.seq++
	w := &fakeWS{name: name, instance: fmt.Sprintf("%032x", 0x1000+f.seq), generation: fmt.Sprintf("%032x", 0x2000+f.seq), store: memory.NewStorage(),
		refs: map[string]plumbing.Hash{}}
	f.ws[name] = w
	return doc(contract.WorkspaceView{Name: name, Instance: w.instance, CreatedAt: f.when, Generation: w.generation, DefaultBranch: contract.DefaultBranchRef,
		Retention: contract.NewRetention()})
}

func (f *fakePlane) wsPush(a cliArgs) fakeAnswer {
	w := f.ws[a.arg(2)]
	if w == nil || a.flags["instance"] != w.instance {
		return refused(4, "workspace %s instance mismatch", a.arg(2))
	}
	src := a.arg(3)
	specs := map[string]testkit.FileSpec{}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(src, p)
		specs[filepath.ToSlash(rel)] = testkit.FileSpec{Mode: filemode.Regular, Content: b}
		return err
	})
	if err != nil {
		return refused(4, "push: %v", err)
	}
	w.main, err = testkit.CommitFiles(w.store, specs, nil, "coordinator push")
	if err != nil {
		return refused(4, "push: %v", err)
	}
	return doc(contract.WorkspacePushResult{Name: w.name, Instance: w.instance, Branch: contract.DefaultBranchRef, Commit: w.main.String(),
		SourceKind: contract.KindFolder, Changed: true})
}

func (f *fakePlane) wsStatus(w *fakeWS) contract.WorkspaceStatusResponse {
	refs := []contract.WorkspaceRef{{Name: contract.DefaultBranchRef, Commit: w.main.String()}}
	for id, h := range w.refs {
		at := f.when
		refs = append(refs, contract.WorkspaceRef{Name: contract.TaskRefPrefix + id, Commit: h.String(), PublishedAt: &at})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return contract.WorkspaceStatusResponse{Name: w.name, Instance: w.instance, Generation: w.generation, DefaultBranch: contract.DefaultBranchRef, Refs: refs}
}

func (f *fakePlane) taskStatus(t *fakeTask) contract.TaskWorkspaceStatus {
	st := contract.TaskWorkspaceStatus{TaskID: t.id, State: t.state, Binding: *f.binding(t), Available: t.pub == contract.PublicationPublished}
	if contract.TaskTerminal(t.state) {
		st.Result = fakeWSResult(t)
	} else {
		st.WorkspacePhase = contract.WorkspacePhaseOf(f.binding(t), t.state, false)
	}
	return st
}

// importReachable copies the objects reachable from commit (its ancestry,
// trees and blobs) from src into repo.
func importReachable(src *memory.Storage, repo *git.Repository, commit plumbing.Hash) error {
	seen := map[plumbing.Hash]bool{}
	var copyObj func(h plumbing.Hash) error
	copyObj = func(h plumbing.Hash) error {
		if seen[h] {
			return nil
		}
		seen[h] = true
		o, err := src.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return err
		}
		if _, err := repo.Storer.SetEncodedObject(o); err != nil {
			return err
		}
		switch o.Type() {
		case plumbing.CommitObject:
			c, err := object.DecodeCommit(src, o)
			if err != nil {
				return err
			}
			for _, p := range append([]plumbing.Hash{c.TreeHash}, c.ParentHashes...) {
				if err := copyObj(p); err != nil {
					return err
				}
			}
		case plumbing.TreeObject:
			tr, err := object.DecodeTree(src, o)
			if err != nil {
				return err
			}
			for _, e := range tr.Entries {
				if err := copyObj(e.Hash); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return copyObj(commit)
}

// wsPull delivers a published task result into an existing repository
// (objects and refs/callsheet/<ws>/tasks/<id> only) or a new folder.
func (f *fakePlane) wsPull(a cliArgs) fakeAnswer {
	t := f.tasks[a.arg(2)]
	if t == nil || t.ws == nil || t.pub != contract.PublicationPublished {
		return refused(4, "%s: %s", contract.ReasonTaskResultUnavailable, a.arg(2))
	}
	dest := a.arg(3)
	res := contract.WorkspacePullResult{Name: t.ws.name, Instance: t.ws.instance, Selector: contract.TaskRefPrefix + t.id, Commit: t.commit.String(), Changed: true}
	if repo, err := git.PlainOpen(dest); err == nil {
		if err := importReachable(t.ws.store, repo, t.commit); err != nil {
			return refused(1, "pull: %v", err)
		}
		local := "refs/callsheet/" + t.ws.name + "/tasks/" + t.id
		if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(local), t.commit)); err != nil {
			return refused(1, "pull: %v", err)
		}
		res.DestinationKind, res.LocalRef = contract.KindGit, &local
	} else {
		if _, err := os.Lstat(dest); err == nil {
			return refused(4, "destination %s exists", dest)
		}
		for p, c := range t.files {
			full := filepath.Join(dest, filepath.FromSlash(p))
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
				return refused(1, "pull: %v", err)
			}
		}
		res.DestinationKind = contract.KindFolder
	}
	if !a.json {
		return fakeAnswer{stdout: "pulled " + t.id + " into " + dest + "\n"}
	}
	return doc(res)
}
