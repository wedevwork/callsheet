package taskworkspace_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/sidecar"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/taskhub"
)

// The publication benchmark's production stack (iteration 10b, review r1
// C7): a real in-process plane (plane.Run: its per-task writer persists
// the intent, the terminal record and the result receipt; its workspace hub
// runs the guarded receive's generation transaction), a simulated node on
// the stream, and the node's production persistence (sidecar.TaskStorage:
// the task directory, publication checkpoints, the outbox journal and the
// removal of the whole task directory).

// benchNodeID is the simulated worker of the benchmark planes.
var benchNodeID = "n_" + strings.Repeat("8", 32)

// benchPlane is one production plane with workspace "bench" seeded with a
// base history, the node's shared cache and storage, and a template task
// directory prepared once over the node route.
type benchPlane struct {
	p        *taskhub.Plane
	n        *taskhub.Node
	inst     string
	cache    *taskworkspace.Manager
	store    *sidecar.TaskStorage
	template string
	pmap     *taskworkspace.Prepared
}

// benchPlanes are the per-process planes, by history length (fixture
// setup shared by the N=1 probe and the timed run).
var benchPlanes = map[int]*benchPlane{}

func benchPlaneOf(b *testing.B, depth int) *benchPlane {
	b.Helper()
	if f := benchPlanes[depth]; f != nil {
		return f
	}
	tb := benchProcess{b}
	ctx := context.Background()
	f := &benchPlane{}
	f.p = taskhub.RunPlane(tb, filepath.Join(tb.TempDir(), "plane"))
	f.n = taskhub.StartNode(tb, f.p, benchNodeID)
	taskhub.Recv(tb, f.n, f.n.Ready, "the first heartbeat")
	dir := tb.TempDir()
	ins, run := filepath.Join(dir, "i.md"), filepath.Join(dir, "r.md")
	put(tb, ins, []byte("i"), 0o644)
	put(tb, run, []byte("r"), 0o644)
	if _, err := f.p.Client.AddRole(ctx, contract.RoleConfig{ID: "bench-a", Name: "bench-a", Node: benchNodeID, Adapter: "fake", Instruction: ins, Runbook: run,
		Model: "example-model", Effort: "medium", Concurrency: 4}); err != nil {
		b.Fatal(err)
	}
	ready := func() bool {
		v, err := f.p.Client.ShowRole(ctx, "bench-a")
		return err == nil && v.CanAccept
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	testkit.AwaitCondBy(tb, ready, tick.C, time.After(taskhub.Wait), "the benchmark role ready")
	v, err := f.p.Client.CreateWorkspace(ctx, "bench")
	if err != nil {
		b.Fatal(err)
	}
	f.inst = v.Instance
	hc, err := testkit.GitHTTPClient(f.p.PEM)
	if err != nil {
		b.Fatal(err)
	}
	g := testkit.GitRemote{HTTP: hc, URL: f.p.URL + contract.WorkspaceGitPrefix + "bench.git", Instance: f.inst}
	s := testkit.NewMemoryStore()
	old := plumbing.ZeroHash
	for ver := 0; ver < depth; ver++ {
		var parents []plumbing.Hash
		if ver > 0 {
			parents = []plumbing.Hash{old}
		}
		c, err := testkit.CommitFiles(s, benchFilesV(ver, ver), parents, "seed")
		if err != nil {
			b.Fatal(err)
		}
		if res := g.Push(ctx, s, "refs/heads/main", old, c, testkit.PackWindow(0)); !res.OK() {
			b.Fatalf("seed push: %v", res.Err)
		}
		old = c
	}
	if f.cache, err = taskworkspace.OpenCache(ctx, taskworkspace.CacheOptions{Root: tb.TempDir(), Origin: f.p.URL}); err != nil {
		b.Fatal(err)
	}
	state := tb.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		b.Fatal(err)
	}
	f.store = sidecar.NewTaskStorage(state)
	// The template: one real preparation over the node route (its task is
	// then refused, freeing the plane's capacity).
	st := f.dispatch(b)
	f.template = filepath.Join(tb.TempDir(), "template")
	if err := os.MkdirAll(filepath.Join(f.template, taskworkspace.WorkName), 0o700); err != nil {
		b.Fatal(err)
	}
	rg, err := f.p.Client.NodeTaskGit(st.TaskID, f.n.Assignment(st), "")
	if err != nil {
		b.Fatal(err)
	}
	f.pmap, err = taskworkspace.Prepare(ctx, taskworkspace.PrepareInput{GOOS: runtime.GOOS, TaskDir: f.template, Binding: *st.Workspace, Remote: rg, Cache: f.cache})
	rg.Close()
	if err != nil {
		b.Fatal(err)
	}
	if ack := f.n.Prepared(tb, st, contract.TaskError(contract.CodeUnavailable, "", contract.ReasonWorkspaceCheckoutFailed, "template (fixture)")); ack.Release {
		b.Fatal("the template task was released")
	}
	benchPlanes[depth] = f
	return f
}

// dispatch dispatches a workspace task on main and returns its start.
func (f *benchPlane) dispatch(b *testing.B) contract.TaskStartBody {
	b.Helper()
	name := "bench"
	if _, err := f.p.Client.Dispatch(context.Background(), contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "bench-a"}, Goal: "g",
		Payload: []string{}, Acceptance: "a", RequestedBy: contract.RequestedBy{Name: "bench", Version: "1", Hostname: "h"}, Workspace: &name}); err != nil {
		b.Fatal(err)
	}
	return taskhub.Recv(b, f.n, f.n.Starts, "task_start")
}

// released is a task released for execution whose child changed one file
// and whose result tree is sealed: its journal (running), its private
// objects and checkout (a copy of the prepared template: the same base
// closure and checkout bytes), and the sealed checkpoint to persist.
func (f *benchPlane) released(b *testing.B, i int) (contract.TaskStartBody, contract.ExecutionJournal, contract.PublicationCheckpoint) {
	b.Helper()
	ctx := context.Background()
	st := f.dispatch(b)
	j := contract.ExecutionJournal{TaskID: st.TaskID, Execution: st.Execution, StartDigest: st.StartDigestHex(), Role: st.Role, Effective: st.Effective,
		TimeoutPolicy: contract.TimeoutPolicyEnforced, Phase: contract.JournalPrepared, Work: true, Workspace: st.Workspace}
	if err := f.store.Create(j); err != nil {
		b.Fatal(err)
	}
	dir := f.store.Dir(st.TaskID)
	for _, d := range []string{taskworkspace.WorkName, taskworkspace.ObjectsName} {
		if err := copyTree(filepath.Join(dir, d), filepath.Join(f.template, d)); err != nil {
			b.Fatal(err)
		}
	}
	if ack := f.n.Prepared(b, st, nil); !ack.Release {
		b.Fatal("a prepared task was not released")
	}
	nonce, at := strings.Repeat("0f", 32), time.Now().UTC()
	j.Phase, j.OwnerNonce, j.StartedAt = contract.JournalRunning, &nonce, &at
	if err := f.store.Write(j); err != nil {
		b.Fatal(err)
	}
	put(b, filepath.Join(dir, taskworkspace.WorkName, "d00/f0000.bin"), benchBlob(0, 77+i), 0o644)
	tree, _, err := taskworkspace.Snapshot(ctx, taskworkspace.SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: f.pmap.Map})
	if err != nil {
		b.Fatal(err)
	}
	zero := 0
	cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &zero}.Sealed()
	return st, j, contract.PublicationCheckpoint{TaskID: st.TaskID, Execution: st.Execution, Phase: contract.CheckpointSealed, Candidate: cand, Tree: tree.String()}
}

// generationBytes is the regular-file bytes of the plane's current
// generation of workspace "bench": the copy its last guarded transaction
// made, with the received objects.
func (f *benchPlane) generationBytes(b *testing.B) int64 {
	b.Helper()
	ws := filepath.Join(f.p.Root, "workspaces", "bench")
	cur, err := os.ReadFile(filepath.Join(ws, "CURRENT"))
	if err != nil {
		b.Fatal(err)
	}
	n, err := dirSize(filepath.Join(ws, "generations", strings.TrimSpace(string(cur))))
	if err != nil || n <= 0 {
		b.Fatalf("generation bytes %d: %v", n, err)
	}
	return n
}

// BenchmarkTaskWorkspacePublish measures one publication of a one-file
// change through production persistence on both sides, base history 1 and
// 8: the durable sealed checkpoint, Begin (the plane's per-task writer
// persists the intent), the deterministic commit, the authorized and push
// checkpoints, the guarded create-once receive with its hub generation
// transaction and the durable terminal record, the settlement's
// observation and checkpoint, the outbox journal, the task_result exchange
// up to the plane's committed receipt, and the removal of the whole task
// directory. It reports the pushed pack payload (under the 256 KiB
// incremental limit), the bytes of the hub generation the transaction
// copied and allocations; each settled result is asserted published with
// exactly one modified path.
func BenchmarkTaskWorkspacePublish(b *testing.B) {
	for _, depth := range []int{1, 8} {
		b.Run(fmt.Sprintf("history=%d", depth), func(b *testing.B) {
			f := benchPlaneOf(b, depth)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			var payload, copied atomic.Int64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				st, j, cp := f.released(b, i)
				var sent atomic.Int64
				api, err := taskhub.ClientAPI(f.p.Client, st.TaskID, f.n.Assignment(st))
				if err != nil {
					b.Fatal(err)
				}
				in := taskworkspace.PublishInput{TaskID: st.TaskID, RoleID: st.Role.ID, Model: st.Effective.Model, TaskDir: f.store.Dir(st.TaskID), Binding: *st.Workspace,
					Checkpoint: cp, API: countingAPI{api, &sent}, Save: f.store.SaveCheckpoint, Clock: taskhub.RealClock{}, Retry: 10 * time.Millisecond}
				b.StartTimer()
				if err := f.store.SaveCheckpoint(cp); err != nil {
					b.Fatal(err)
				}
				pub, err := taskworkspace.Publish(ctx, in)
				if err != nil || pub.Result == nil || pub.Result.Workspace == nil || pub.Result.Workspace.Publication != contract.PublicationPublished ||
					pub.Result.Workspace.Diffstat == nil || pub.Result.Workspace.Diffstat.Modified != 1 {
					b.Fatalf("publish %+v %v", pub, err)
				}
				j.Phase, j.OwnerNonce, j.Result = contract.JournalCompleted, nil, pub.Result
				if err := f.store.Write(j); err != nil {
					b.Fatal(err)
				}
				if ack := f.n.Result(b, *pub.Result); !ack.Committed {
					b.Fatal("the plane never committed the result receipt")
				}
				if err := f.store.Remove(st.TaskID); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if n := sent.Load(); n <= 0 || n >= incrementMax {
					b.Fatalf("one-change publication sent %d pack bytes (limit %d)", n, incrementMax)
				}
				if _, err := os.Lstat(f.store.Dir(st.TaskID)); !os.IsNotExist(err) {
					b.Fatalf("task directory after cleanup: %v", err)
				}
				v, err := f.p.Client.ShowTask(ctx, st.TaskID, 0)
				if err != nil || v.State != contract.TaskSucceeded || v.Result == nil || v.Result.ResultCommit == nil || *v.Result.ResultCommit != *pub.Result.Workspace.Commit {
					b.Fatalf("settled view %+v %v", v.Result, err)
				}
				payload.Add(sent.Load())
				copied.Add(f.generationBytes(b))
				b.StartTimer()
			}
			b.ReportMetric(float64(payload.Load())/float64(b.N), "payload-B/op")
			b.ReportMetric(float64(copied.Load())/float64(b.N), "copied-B/op")
		})
	}
}
