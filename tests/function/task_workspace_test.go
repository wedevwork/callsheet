package function

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
	"github.com/wedevwork/callsheet/internal/testkit/workersmoke"
)

// Iteration 10b function tests: exactly one top-level TestTaskWorkspace*
// per FP, in FP order, on one shared real deployment (a plane and fake
// adapter sidecars from the built binaries, on 127.0.0.1): HTTP dispatch
// with a workspace binding, the real node git routes, the private
// checkout, the result commit and its crash-safe publication, inspected
// with go-git through the coordinator's own workspace endpoint. The fake
// adapter's workspace mode executes the goal's script in its cwd and
// mutates its private repository with go-git (never shell git). Lifecycle
// combinatorics live in the taskworkspace and taskpublication packages.

// wsTaskWait bounds one workspace task's terminal wait.
const wsTaskWait = 90 * time.Second

// wsRig is the shared deployment: worker a (the host PATH) and worker z
// (an empty PATH: git is never required), each with a fake role of
// concurrency 2.
type wsRig struct {
	root string
	dep  *workersmoke.Deployment
	a, z *workersmoke.Sidecar
	pem  []byte
	git  testkit.GitRemote
	env  []string
	// seq names fresh workspaces.
	mu  sync.Mutex
	seq int
}

var (
	wsRigOnce sync.Once
	theWsRig  *wsRig
	wsRigErr  error
)

// sharedWsRig starts the shared deployment once per test binary.
func sharedWsRig(t *testing.T) *wsRig {
	t.Helper()
	bin, fake := nodeBinary(t), fakeAdapterBinary(t)
	wsRigOnce.Do(func() { theWsRig, wsRigErr = startWsRig(bin, fake) })
	if wsRigErr != nil {
		t.Fatal(wsRigErr)
	}
	return theWsRig
}

func startWsRig(bin, fake string) (*wsRig, error) {
	// A short root: every checkout path must fit the native PATH_MAX.
	root, err := os.MkdirTemp("", "cs10b-")
	if err != nil {
		return nil, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, err
	}
	r := &wsRig{root: root}
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o700)
	r.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	fixtureMu.Lock()
	afterSuite = append(afterSuite, func() {
		if r.dep != nil {
			r.dep.Close()
		}
		os.RemoveAll(root)
	})
	fixtureMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if r.dep, err = workersmoke.Start(ctx, bin, root, r.env); err != nil {
		return nil, err
	}
	mode := fakeadapter.EnvTaskMode + "=" + fakeadapter.WorkspaceMode
	if r.a, err = r.dep.StartSidecar(ctx, "a", []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, mode}, "--fake-adapter", fake); err != nil {
		return nil, err
	}
	// Worker z has an empty PATH and the absolute fake executable.
	if r.z, err = r.dep.StartSidecar(ctx, "z", []string{"PATH=", "HOME=" + home, mode}, "--fake-adapter", fake); err != nil {
		return nil, err
	}
	ins, run, err := r.dep.Manuals("ws", "Workspace fixture instruction.\n", "Workspace fixture runbook.\n")
	if err != nil {
		return nil, err
	}
	roles := []string{"ws-a", "ws-z"}
	for i, node := range []string{r.a.NodeID, r.z.NodeID} {
		if _, err := r.dep.Client.AddRole(ctx, contract.RoleConfig{ID: roles[i], Name: roles[i], Node: node, Adapter: "fake", Instruction: ins, Runbook: run,
			Model: "example-model", Effort: "medium", Concurrency: 2}); err != nil {
			return nil, err
		}
	}
	// A role added on z raises the registry revision but is distributed
	// only to z (landed behavior since iteration 04), while every start
	// carries the registry revision: worker a reattaches to install it.
	if err := r.dep.Restart(ctx, r.a); err != nil {
		return nil, err
	}
	for _, id := range roles {
		ready := func() bool {
			v, err := r.dep.Client.ShowRole(context.Background(), id)
			return err == nil && v.CanAccept
		}
		for !ready() {
			select {
			case <-ctx.Done():
				// Recheck once before the verdict: readiness and the bound
				// both ready never fail the rig.
				if ready() {
					continue
				}
				return nil, fmt.Errorf("role %s never became ready", id)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	if r.pem, err = os.ReadFile(r.dep.CA); err != nil {
		return nil, err
	}
	hc, err := testkit.GitHTTPClient(r.pem)
	if err != nil {
		return nil, err
	}
	r.git = testkit.GitRemote{HTTP: hc}
	return r, nil
}

// cli runs the callsheet CLI with the deployment's trust.
func (r *wsRig) cli(t *testing.T, args ...string) result {
	t.Helper()
	return runBin(t, r.dep.Bin, r.root, r.env, append(args, "--plane", r.dep.URL, "--ca", r.dep.CA)...)
}

// workspace creates a fresh workspace and returns its name and instance.
func (r *wsRig) workspace(t *testing.T) (string, string) {
	t.Helper()
	r.mu.Lock()
	r.seq++
	name := fmt.Sprintf("p%d", r.seq)
	r.mu.Unlock()
	v, err := r.dep.Client.CreateWorkspace(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return name, v.Instance
}

// remote is the coordinator git endpoint of workspace name.
func (r *wsRig) remote(name, instance string) testkit.GitRemote {
	g := r.git
	g.URL = r.dep.URL + "/ws/" + name + ".git"
	g.Instance = instance
	return g
}

// seed pushes a plain folder with files as branch (default main) and
// returns the commit.
func (r *wsRig) seed(t *testing.T, name, inst, branch string, files map[string]tfile) string {
	t.Helper()
	dir := mkFolder(t, files)
	args := []string{"ws", "push", "--json", "--instance", inst}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	res := r.cli(t, append(args, name, dir)...)
	if res.code != 0 {
		t.Fatalf("ws push: %+v", res)
	}
	p, err := contract.ParseWorkspacePushResult([]byte(strings.TrimSuffix(res.stdout, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return p.Commit
}

// refs reads the workspace's advertised refs.
func (r *wsRig) refs(t *testing.T, name, inst string) map[string]plumbing.Hash {
	t.Helper()
	refs, err := r.remote(name, inst).Refs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

// fetch fetches commit's closure from the hub.
func (r *wsRig) fetch(t *testing.T, name, inst, commit string) (*memory.Storage, *object.Commit) {
	t.Helper()
	s := testkit.NewMemoryStore()
	h := plumbing.NewHash(commit)
	if err := r.remote(name, inst).Fetch(context.Background(), s, []plumbing.Hash{h}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := object.GetCommit(s, h)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

// wsDispatch dispatches script to role with a workspace selection.
func (r *wsRig) wsDispatch(t *testing.T, role string, ws, base, inst *string, s fakeadapter.WorkspaceScript) (contract.TaskView, error) {
	t.Helper()
	r.awaitAccept(role)
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: role}, Goal: fakeadapter.WorkspaceGoal(s), Payload: []string{},
		Acceptance: "fixture", RequestedBy: contract.RequestedBy{Name: "ws-test", Version: "1", Hostname: "fixture"}, Workspace: ws, Base: base, WorkspaceInstance: inst}
	return r.dep.Client.Dispatch(context.Background(), req)
}

// awaitAccept waits, bounded, until role reports it can accept a start
// (the plane's dispatch precondition: a ready-check cycle may lapse under
// load). Past the bound the dispatch still runs and its result is
// asserted.
func (r *wsRig) awaitAccept(role string) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, err := r.dep.Client.ShowRole(context.Background(), role)
		if (err == nil && v.CanAccept) || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// await polls task id until terminal and returns its view, after the
// worker let go of the task (awaitReleased).
func (r *wsRig) await(t *testing.T, id string) contract.TaskView {
	t.Helper()
	deadline := time.Now().Add(wsTaskWait)
	for {
		v, err := r.dep.Client.ShowTask(context.Background(), id, contract.DefaultTailLines)
		if err == nil && contract.TaskTerminal(v.State) {
			r.awaitReleased(id)
			return v
		}
		if time.Now().After(deadline) {
			// Recheck once before the verdict.
			if v, err := r.dep.Client.ShowTask(context.Background(), id, contract.DefaultTailLines); err == nil && contract.TaskTerminal(v.State) {
				r.awaitReleased(id)
				return v
			}
			t.Fatalf("task %s not terminal: %+v %v", id, v, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitReleased waits, bounded, until neither worker holds task id's
// directory. A workspace task's terminal record precedes its worker's slot
// release (the plane settles the publication first; the worker frees the
// slot only after its outbox is durable and its work directory removed:
// design 10b, Publication transaction step 5), and the janitor deletes the
// task directory only after that committed outcome, so its absence proves
// the slot is free before the next dispatch. Assertions about cleanup stay
// with the tests that make them.
func (r *wsRig) awaitReleased(id string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, ea := os.Lstat(taskDir(r.a, id))
		_, ez := os.Lstat(taskDir(r.z, id))
		if os.IsNotExist(ea) && os.IsNotExist(ez) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// run dispatches and awaits one task.
func (r *wsRig) run(t *testing.T, role string, ws, base *string, s fakeadapter.WorkspaceScript) contract.TaskView {
	t.Helper()
	v, err := r.wsDispatch(t, role, ws, base, nil, s)
	if err != nil {
		t.Fatal(err)
	}
	return r.await(t, v.TaskID)
}

// report reads a child's workspace report.
func readWsReport(t *testing.T, p string) fakeadapter.WorkspaceReport {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rep fakeadapter.WorkspaceReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func sp(s string) *string { return &s }

// published requires a published workspace result and returns it.
func publishedWs(t *testing.T, v contract.TaskView) contract.TaskWorkspaceResult {
	t.Helper()
	if v.Result == nil || v.Result.Workspace == nil {
		t.Fatalf("task %s has no workspace result: %+v", v.TaskID, v)
	}
	w := *v.Result.Workspace
	if w.Publication != contract.PublicationPublished || w.Commit == nil || w.Ref == nil || *w.Ref != contract.TaskRefPrefix+v.TaskID ||
		v.Result.ResultCommit == nil || *v.Result.ResultCommit != *w.Commit {
		t.Fatalf("task %s workspace %+v (state %s, log %q)", v.TaskID, w, v.State, v.Result.LogTail)
	}
	return w
}

// wantResultCommit checks the fixed identity, message and parent list.
func wantResultCommit(t *testing.T, c *object.Commit, v contract.TaskView, state string, parents ...plumbing.Hash) {
	t.Helper()
	msg := "Callsheet task result\ntask: " + v.TaskID + "\nrole: " + v.Role.ID + "\nmodel: " + v.Effective.Model + "\nstate: " + state + "\n"
	if c.Message != msg || c.Author.Name != "Callsheet" || c.Author.Email != "workspace@callsheet.invalid" || c.Author.When.Unix() != 0 ||
		c.Committer.Name != "Callsheet" || c.Committer.Email != "workspace@callsheet.invalid" || c.Committer.When.Unix() != 0 || c.PGPSignature != "" {
		t.Fatalf("result commit identity/message %+v", c)
	}
	if fmt.Sprint(c.ParentHashes) != fmt.Sprint(parents) {
		t.Fatalf("result parents %v, want %v", c.ParentHashes, parents)
	}
}

// barrier is a directory of the test's barrier files.
type barrier string

func newBarrier(t *testing.T) barrier {
	t.Helper()
	d, err := os.MkdirTemp(theWsRig.root, "b")
	if err != nil {
		t.Fatal(err)
	}
	return barrier(d)
}

func (b barrier) path(name string) string { return filepath.Join(string(b), name) }

// release creates the barrier file name.
func (b barrier) release(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(b.path(name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// await waits (bounded) for the barrier file name, rechecking it before
// the verdict.
func (b barrier) await(t *testing.T, name string) {
	t.Helper()
	eventually(t, "barrier "+name, func() bool {
		_, err := os.Stat(b.path(name))
		return err == nil
	})
}

// eventually polls cond (bounded), rechecking it before the verdict.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wsTaskWait)
	for !cond() {
		if time.Now().After(deadline) {
			if cond() {
				return
			}
			t.Fatalf("%s never held", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wsRecord is the plane's durable task record fields a node presents.
type wsRecord struct {
	Execution   contract.ExecutionToken `json:"execution"`
	StartDigest *string                 `json:"start_digest"`
	Role        struct {
		Node string `json:"node"`
	} `json:"role"`
	Workspace   *contract.WorkspaceBinding `json:"workspace_binding"`
	Publication *struct {
		PublicationID string `json:"publication_id"`
	} `json:"publication"`
}

func (r *wsRig) record(t *testing.T, id string) wsRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.root, "plane", "tasks", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec wsRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// assignment is task id's node assignment as its worker presents it.
func (r *wsRig) assignment(t *testing.T, id string) contract.NodeAssignment {
	t.Helper()
	rec := r.record(t, id)
	if rec.StartDigest == nil || rec.Workspace == nil {
		b, _ := os.ReadFile(filepath.Join(r.root, "plane", "tasks", id+".json"))
		t.Fatalf("task %s record has no start digest or binding: %s", id, b)
	}
	return contract.NodeAssignment{NodeID: rec.Role.Node, Execution: rec.Execution, StartDigest: *rec.StartDigest, Instance: rec.Workspace.Instance}
}

// running dispatches s and waits until its child reached barrier name.
func (r *wsRig) running(t *testing.T, role string, ws, base *string, s fakeadapter.WorkspaceScript, b barrier, name string) contract.TaskView {
	t.Helper()
	v, err := r.wsDispatch(t, role, ws, base, nil, s)
	if err != nil {
		t.Fatal(err)
	}
	b.await(t, name)
	return v
}

// taskDir is task id's journal directory on sidecar s.
func taskDir(s *workersmoke.Sidecar, id string) string { return filepath.Join(s.State, "tasks", id) }

// ops builds a script's op list.
func ops(o ...fakeadapter.WorkspaceOp) []fakeadapter.WorkspaceOp { return o }

// noRef requires that workspace name has no task ref for id.
func (r *wsRig) noRef(t *testing.T, name, inst, id string) {
	t.Helper()
	if h, ok := r.refs(t, name, inst)[contract.TaskRefPrefix+id]; ok {
		t.Fatalf("task %s has ref %s", id, h)
	}
}

// files reads a result commit's tree.
func (r *wsRig) files(t *testing.T, name, inst, commit string) (map[string]testkit.FileSpec, *object.Commit) {
	t.Helper()
	s, c := r.fetch(t, name, inst, commit)
	got, err := testkit.TreeFiles(s, c.Hash)
	if err != nil {
		t.Fatal(err)
	}
	return got, c
}

// specOf flattens tfiles into FileSpecs.
func specOf(files map[string]tfile) map[string]testkit.FileSpec {
	out := map[string]testkit.FileSpec{}
	for p, f := range files {
		out[p] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
	}
	return out
}

// inflight is role id's plane-held task count.
func (r *wsRig) inflight(t *testing.T, id string) int {
	t.Helper()
	v, err := r.dep.Client.ShowRole(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v.Inflight
}
