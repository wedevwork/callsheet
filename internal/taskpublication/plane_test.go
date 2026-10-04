package taskpublication_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/taskhub"
)

// The plane's workspace wiring (iteration 10b), qualified from outside
// the plane package (no plane stress growth): a real in-process plane, a
// simulated protocol-6 node speaking the stream, and the real worker
// library over the verified HTTPS routes (node fetch, publication
// endpoints, guarded receive).

// wiring is one plane with a workspace "proj" seeded on main, a node with
// role "ws-a" and a worker cache.
type wiring struct {
	p     *taskhub.Plane
	n     *taskhub.Node
	inst  string
	base  plumbing.Hash
	cache *taskworkspace.Manager
	root  string
}

func newWiring(t *testing.T, root string) *wiring {
	t.Helper()
	w := &wiring{p: taskhub.RunPlane(t, root), root: t.TempDir()}
	w.n = taskhub.StartNode(t, w.p, "n_"+strings.Repeat("6", 32))
	taskhub.Recv(t, w.n, w.n.Ready, "first heartbeat")
	dir := t.TempDir()
	ins, run := filepath.Join(dir, "i.md"), filepath.Join(dir, "r.md")
	os.WriteFile(ins, []byte("i"), 0o644)
	os.WriteFile(run, []byte("r"), 0o644)
	if _, err := w.p.Client.AddRole(context.Background(), contract.RoleConfig{ID: "ws-a", Name: "ws-a", Node: w.n.ID, Adapter: "fake", Instruction: ins, Runbook: run,
		Model: "example-model", Effort: "medium", Concurrency: 4}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "role ready", func() bool {
		v, err := w.p.Client.ShowRole(context.Background(), "ws-a")
		return err == nil && v.CanAccept
	})
	v, err := w.p.Client.CreateWorkspace(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	w.inst = v.Instance
	hc, err := testkit.GitHTTPClient(w.p.PEM)
	if err != nil {
		t.Fatal(err)
	}
	s := testkit.NewMemoryStore()
	w.base, _ = testkit.CommitFiles(s, files("a.txt", "a\n"), nil, "base")
	g := testkit.GitRemote{HTTP: hc, URL: w.p.URL + contract.WorkspaceGitPrefix + "proj.git", Instance: w.inst}
	if res := g.Push(context.Background(), s, "refs/heads/main", plumbing.ZeroHash, w.base); !res.OK() {
		t.Fatalf("seed push: %v", res.Err)
	}
	if w.cache, err = taskworkspace.OpenCache(context.Background(), taskworkspace.CacheOptions{Root: w.root, Origin: w.p.URL}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *wiring) dispatch(t *testing.T, ws, base *string) contract.TaskView {
	t.Helper()
	v, err := w.p.Client.Dispatch(context.Background(), contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "ws-a"}, Goal: "g",
		Payload: []string{}, Acceptance: "a", RequestedBy: contract.RequestedBy{Name: "t", Version: "1", Hostname: "h"}, Workspace: ws, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (w *wiring) show(t *testing.T, id string) contract.TaskView {
	t.Helper()
	v, err := w.p.Client.ShowTask(context.Background(), id, contract.DefaultTailLines)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// prepare runs the worker's real preparation of st through the node route.
func (w *wiring) prepare(t *testing.T, st contract.TaskStartBody) (string, *taskworkspace.Prepared) {
	t.Helper()
	dir := filepath.Join(w.root, "tasks", st.TaskID)
	p, err := w.tryPrepare(context.Background(), st, dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, p
}

// tryPrepare runs the worker's real preparation of st into dir under ctx.
func (w *wiring) tryPrepare(ctx context.Context, st contract.TaskStartBody, dir string) (*taskworkspace.Prepared, error) {
	os.MkdirAll(filepath.Join(dir, taskworkspace.WorkName), 0o700)
	g, err := w.p.Client.NodeTaskGit(st.TaskID, w.n.Assignment(st), "")
	if err != nil {
		return nil, err
	}
	defer g.Close()
	return taskworkspace.Prepare(ctx, taskworkspace.PrepareInput{GOOS: runtime.GOOS, TaskDir: dir, Binding: *st.Workspace, Remote: g, Cache: w.cache})
}

// publish seals and publishes a released execution with exit code.
func (w *wiring) publish(t *testing.T, st contract.TaskStartBody, dir string, p *taskworkspace.Prepared, exit int) taskworkspace.Publication {
	t.Helper()
	tree, _, err := taskworkspace.Snapshot(context.Background(), taskworkspace.SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: p.Map})
	if err != nil {
		t.Fatal(err)
	}
	cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit}.Sealed()
	cp := contract.PublicationCheckpoint{TaskID: st.TaskID, Execution: st.Execution, Phase: contract.CheckpointSealed, Candidate: cand, Tree: tree.String()}
	a := w.n.Assignment(st)
	api, err := taskhub.ClientAPI(w.p.Client, st.TaskID, a)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := taskworkspace.Publish(context.Background(), taskworkspace.PublishInput{TaskID: st.TaskID, RoleID: st.Role.ID, Model: st.Effective.Model, TaskDir: dir,
		Binding: *st.Workspace, Checkpoint: cp, API: api, Clock: taskhub.RealClock{}, Retry: 10 * time.Millisecond,
		Save: func(contract.PublicationCheckpoint) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// TestPlaneWiring qualifies the plane's workspace wiring end to end: the
// admission binding, the preparing start and the prepared exchange's
// release decision (duplicates replayed), the publication endpoints over
// HTTPS with their refusals, the guarded receive and the settled view, a
// refused preparation, a cancellation before release, a failed
// publication on the ordinary result path, and the startup replay of an
// authorized intent. Its preparation subtest is delegated from
// tests/function (TestTaskWorkspaceAdmission/slow-prepare). Do not rename.
func TestPlaneWiring(t *testing.T) {
	t.Parallel()
	w := newWiring(t, filepath.Join(t.TempDir(), "plane"))
	ctx := context.Background()
	name := "proj"
	t.Run("admission", func(t *testing.T) {
		for _, c := range []struct {
			ws, base, inst *string
			code           contract.Code
		}{
			{&name, strp("nope"), nil, contract.CodeNotFound},
			{&name, nil, strp(strings.Repeat("9", 32)), contract.CodeConflict},
			{strp("nosuch"), nil, nil, contract.CodeNotFound},
		} {
			_, err := w.p.Client.Dispatch(ctx, contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "ws-a"}, Goal: "g", Payload: []string{},
				Acceptance: "a", RequestedBy: contract.RequestedBy{Name: "t", Version: "1", Hostname: "h"}, Workspace: c.ws, Base: c.base, WorkspaceInstance: c.inst})
			if contract.CodeOf(err) != c.code {
				t.Fatalf("%+v: %v", c, err)
			}
		}
	})
	t.Run("preparation", func(t *testing.T) {
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		if st.Workspace == nil || *st.Workspace.BaseCommit != w.base.String() {
			t.Fatalf("start binding %+v", st.Workspace)
		}
		eventually(t, "preparing phase", func() bool {
			v := w.show(t, v.TaskID)
			return v.WorkspacePhase != nil && *v.WorkspacePhase == contract.WorkspacePhasePreparing && v.State == contract.TaskPending
		})
		dir, p := w.prepare(t, st)
		if ack := w.n.Prepared(t, st, nil); !ack.Release {
			t.Fatal("a prepared start was not released")
		}
		// A duplicate replays the same decision.
		if ack := w.n.Prepared(t, st, nil); !ack.Release {
			t.Fatal("a duplicate task_prepared changed the decision")
		}
		if v := w.show(t, v.TaskID); v.State != contract.TaskRunning || v.WorkspacePhase == nil || *v.WorkspacePhase != contract.WorkspacePhaseExecuting {
			t.Fatalf("after release %s %v", v.State, v.WorkspacePhase)
		}
		os.WriteFile(filepath.Join(dir, taskworkspace.WorkName, "new.txt"), []byte("n"), 0o644)
		pub := w.publish(t, st, dir, p, 0)
		if pub.Result == nil || pub.Result.Workspace.Publication != contract.PublicationPublished {
			t.Fatalf("publication %+v", pub)
		}
		if ack := w.n.Result(t, *pub.Result); !ack.Committed {
			t.Fatal("the settled result was not committed")
		}
		done := w.show(t, v.TaskID)
		if done.State != contract.TaskSucceeded || done.Result.Workspace == nil || done.Result.ResultCommit == nil || *done.Result.ResultCommit != *pub.Result.Workspace.Commit ||
			done.WorkspacePhase != nil || done.Result.Diffstat.Added != 1 {
			t.Fatalf("settled view %+v", done.Result)
		}
	})
	t.Run("endpoint-refusals", func(t *testing.T) {
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		hc, _ := testkit.GitHTTPClient(w.p.PEM)
		do := func(method, path string, h map[string]string, body string) int {
			req, _ := http.NewRequest(method, w.p.URL+path, strings.NewReader(body))
			req.Header.Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
			if body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			for k, v := range h {
				req.Header.Set(k, v)
			}
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return resp.StatusCode
		}
		a := w.n.Assignment(st)
		base := contract.PublicationPath(st.TaskID, "", false)
		wrong := a
		wrong.NodeID = "n_" + strings.Repeat("7", 32)
		otherInst := a
		otherInst.Instance = strings.Repeat("9", 32)
		for name, c := range map[string]struct {
			method, path string
			h            map[string]string
			body         string
			status       int
		}{
			"no-headers":   {http.MethodGet, base, nil, "", 400},
			"wrong-node":   {http.MethodGet, base, wrong.Headers(), "", 409},
			"wrong-inst":   {http.MethodGet, base, otherInst.Headers(), "", 409},
			"unknown-task": {http.MethodGet, contract.PublicationPath("t_"+strings.Repeat("0", 32), "", false), a.Headers(), "", 404},
			"get-body":     {http.MethodGet, base, a.Headers(), "x", 400},
			// The plane's API answers a wrong method invalid_argument with
			// its Allow header.
			"method":      {http.MethodPut, base, a.Headers(), "", 400},
			"bad-json":    {http.MethodPost, base, a.Headers(), "{", 400},
			"finish-get":  {http.MethodGet, contract.PublicationPath(st.TaskID, strings.Repeat("a", 32), true), a.Headers(), "", 400},
			"finish-body": {http.MethodPost, contract.PublicationPath(st.TaskID, strings.Repeat("a", 32), true), a.Headers(), `{"error":"x"}`, 400},
			"no-intent":   {http.MethodPost, contract.PublicationPath(st.TaskID, strings.Repeat("a", 32), true), a.Headers(), `{"error":"workspace_storage_failed"}`, 404},
		} {
			if got := do(c.method, c.path, c.h, c.body); got != c.status {
				t.Fatalf("%s: HTTP %d, want %d", name, got, c.status)
			}
		}
		// Begin before the release has no publishable outcome.
		tp, _ := w.p.Client.TaskPublications(st.TaskID, a)
		cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int)}.Sealed()
		if _, err := tp.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("4b825dc642cb6eb9a060e54bf8d69288fbee4904", 1)}); err == nil {
			t.Fatal("Begin before the release was authorized")
		}
		if st2, err := tp.Observe(ctx, ""); err != nil || st2.Phase != contract.PubPhaseNone {
			t.Fatalf("lookup %+v %v", st2, err)
		}
		// A refused preparation is rejected with its reason.
		ack := w.n.Prepared(t, st, contract.TaskError(contract.CodeUnavailable, "", contract.ReasonWorkspaceCheckoutFailed, "checkout failed"))
		if ack.Release {
			t.Fatal("a failed preparation was released")
		}
		eventually(t, "rejected", func() bool { return contract.TaskTerminal(w.show(t, v.TaskID).State) })
		done := w.show(t, v.TaskID)
		if done.State != contract.TaskRejected || done.Reason == nil || done.Reason.Code != contract.ReasonWorkspaceCheckoutFailed ||
			done.Result.Workspace == nil || done.Result.Workspace.Publication != contract.PublicationNotApplicable {
			t.Fatalf("refused preparation %s %+v", done.State, done.Reason)
		}
	})
	t.Run("cancel-before-release", func(t *testing.T) {
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		if _, err := w.p.Client.CancelTask(ctx, v.TaskID); err != nil {
			t.Fatal(err)
		}
		c := taskhub.Recv(t, w.n, w.n.Cancels, "task_cancel")
		// A preparation reported after the stop is never released.
		if ack := w.n.Prepared(t, st, nil); ack.Release {
			t.Fatal("a cancelled preparation was released")
		}
		ns := contract.NewNotStarted(*st.Workspace)
		stop := c.StopID
		r := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeCancelled, StopID: &stop, Workspace: &ns}.Sealed()
		w.n.Result(t, r)
		eventually(t, "cancelled", func() bool { return w.show(t, v.TaskID).State == contract.TaskCancelled })
		if done := w.show(t, v.TaskID); done.Result.Workspace.Publication != contract.PublicationNotStarted || done.StartedAt != nil {
			t.Fatalf("cancelled before release %+v", done.Result.Workspace)
		}
	})
	t.Run("failed-publication-result", func(t *testing.T) {
		// A worker whose Begin never produced an intent reports a failed
		// publication through the ordinary result path.
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		w.prepare(t, st)
		w.n.Prepared(t, st, nil)
		zero := 0
		f := contract.NewPublicationFailed(*st.Workspace, contract.PubErrPublicationTransportFail)
		r := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &zero, Workspace: &f}.Sealed()
		if ack := w.n.Result(t, r); !ack.Committed {
			t.Fatal("result not committed")
		}
		if done := w.show(t, v.TaskID); done.State != contract.TaskSucceeded || done.Result.Workspace.Publication != contract.PublicationFailed {
			t.Fatalf("failed publication %s %+v", done.State, done.Result.Workspace)
		}
		// The decided task (no intent was ever authorized) refuses a Begin.
		tp, _ := w.p.Client.TaskPublications(st.TaskID, w.n.Assignment(st))
		cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int)}.Sealed()
		if _, err := tp.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("3", 40)}); contract.CodeOf(err) != contract.CodeConflict {
			t.Fatalf("Begin on a decided task: %v", err)
		}
		// A wrong node's Begin is refused by the arbitration's own check.
		wa := w.n.Assignment(st)
		wa.NodeID = "n_" + strings.Repeat("7", 32)
		tpw, _ := w.p.Client.TaskPublications(st.TaskID, wa)
		if _, err := tpw.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("3", 40)}); contract.CodeOf(err) != contract.CodeConflict {
			t.Fatalf("Begin by another node: %v", err)
		}
	})
	t.Run("no-workspace-task", func(t *testing.T) {
		// A task without a workspace has no publication endpoints.
		v := w.dispatch(t, nil, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		a := contract.NodeAssignment{NodeID: w.n.ID, Execution: st.Execution, StartDigest: st.StartDigestHex(), Instance: w.inst}
		tp, _ := w.p.Client.TaskPublications(st.TaskID, a)
		cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int)}.Sealed()
		if _, err := tp.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("3", 40)}); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("Begin on a task without a workspace: %v", err)
		}
		if ack := w.n.Result(t, cand); !ack.Committed {
			t.Fatal("result not committed")
		}
		if done := w.show(t, v.TaskID); done.State != contract.TaskSucceeded || done.Result.Workspace != nil || done.WorkspaceBinding != nil {
			t.Fatalf("no-workspace task %s %+v", done.State, done.Result)
		}
	})
	t.Run("arbitration", func(t *testing.T) {
		// A stop requested after the release makes the natural candidate's
		// canonical state cancelled (its commit says so, the settlement
		// carries the stop); a duplicate Begin returns the original intent;
		// a decided task refuses a Begin.
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		dir, p := w.prepare(t, st)
		w.n.Prepared(t, st, nil)
		if _, err := w.p.Client.CancelTask(ctx, v.TaskID); err != nil {
			t.Fatal(err)
		}
		c := taskhub.Recv(t, w.n, w.n.Cancels, "task_cancel")
		pub := w.publish(t, st, dir, p, 0)
		if pub.Result == nil || pub.Result.Outcome != contract.OutcomeCancelled || pub.Result.StopID == nil || *pub.Result.StopID != c.StopID {
			t.Fatalf("cancelled settlement %+v", pub.Result)
		}
		w.n.Result(t, *pub.Result)
		if done := w.show(t, v.TaskID); done.State != contract.TaskCancelled || done.Result.Workspace.Publication != contract.PublicationPublished {
			t.Fatalf("cancelled view %s %+v", done.State, done.Result.Workspace)
		}
		tp, _ := w.p.Client.TaskPublications(st.TaskID, w.n.Assignment(st))
		cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int)}.Sealed()
		if _, err := tp.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("1", 40)}); err == nil {
			t.Fatal("a decided task began another publication")
		}
		// A timed-out execution publishes with its state; a duplicate Begin
		// answers the same intent; a lost candidate is refused.
		v2 := w.dispatch(t, &name, nil)
		st2 := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		dir2, p2 := w.prepare(t, st2)
		w.n.Prepared(t, st2, nil)
		tree, _, err := taskworkspace.Snapshot(ctx, taskworkspace.SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir2, Map: p2.Map})
		if err != nil {
			t.Fatal(err)
		}
		tp2, _ := w.p.Client.TaskPublications(st2.TaskID, w.n.Assignment(st2))
		lost := contract.TaskResultBody{TaskID: st2.TaskID, Execution: st2.Execution, Outcome: contract.OutcomeLost}.Sealed()
		if _, err := tp2.Begin(ctx, contract.PublicationBeginRequest{Candidate: lost, Tree: tree.String()}); err == nil {
			t.Fatal("a lost candidate began a publication")
		}
		sig := "SIGKILL"
		timed := contract.TaskResultBody{TaskID: st2.TaskID, Execution: st2.Execution, Outcome: contract.OutcomeTimedOut, Signal: &sig}.Sealed()
		r1, err := tp2.Begin(ctx, contract.PublicationBeginRequest{Candidate: timed, Tree: tree.String()})
		if err != nil || r1.State != contract.TaskTimedOut {
			t.Fatalf("timed-out begin %+v %v", r1, err)
		}
		if r2, err := tp2.Begin(ctx, contract.PublicationBeginRequest{Candidate: timed, Tree: tree.String()}); err != nil || r2 != r1 {
			t.Fatalf("duplicate begin %+v %v", r2, err)
		}
		if _, err := tp2.Begin(ctx, contract.PublicationBeginRequest{Candidate: timed, Tree: strings.Repeat("2", 40)}); err == nil {
			t.Fatal("a different tree replaced the intent")
		}
		// The definitive refusal path: Finish settles failed.
		fin, err := tp2.Finish(ctx, r1.PublicationID, contract.PubErrPublicationTransportFail)
		if err != nil || fin.Phase != contract.PubPhaseSettled || fin.Result.Outcome != contract.OutcomeTimedOut || fin.Result.Workspace.Publication != contract.PublicationFailed {
			t.Fatalf("finish %+v %v", fin, err)
		}
		w.n.Result(t, *fin.Result)
		if done := w.show(t, v2.TaskID); done.State != contract.TaskTimedOut || done.Result.Workspace.Error == nil {
			t.Fatalf("timed-out view %s %+v", done.State, done.Result.Workspace)
		}
	})
	t.Run("reply-overtaken", func(t *testing.T) {
		// The sidecar answers a workspace start preparing on its stream and
		// fetches over HTTPS at once, so the fetch can reach the plane
		// before the reply is applied. Forced here: the node withholds the
		// reply. The fetch must wait for it, never be refused, and complete
		// once the reply is applied.
		w.n.HoldStartReplies(true)
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		w.n.HoldStartReplies(false)
		// A failure below still answers the start (later starts queue
		// behind it).
		defer w.n.ReleaseStartReply()
		type prepared struct {
			p   *taskworkspace.Prepared
			err error
		}
		got := make(chan prepared, 1)
		go func() {
			ctx, cancel := context.WithTimeout(ctx, taskhub.Wait)
			defer cancel()
			p, err := w.tryPrepare(ctx, st, filepath.Join(w.root, "tasks", st.TaskID))
			got <- prepared{p, err}
		}()
		// The barrier, never an elapsed time: the reply is released only
		// once the fetch reached the plane's guard and the guard registered
		// its wait. Any answer before that is a fetch judged while the reply
		// was in flight (a refusal).
		awaitGuardWait(t, func() (string, bool) {
			select {
			case r := <-got:
				return fmt.Sprintf("%+v %v", r.p, r.err), true
			default:
				return "", false
			}
		})
		w.n.ReleaseStartReply()
		r := testkit.WithinBy(t, got, time.After(taskhub.Wait), "the preparation")
		if r.err != nil || r.p == nil || r.p.Files != 1 {
			t.Fatalf("the overtaking fetch after the reply: %+v %v", r.p, r.err)
		}
		// Settle the task: its preparation is reported refused.
		if ack := w.n.Prepared(t, st, contract.TaskError(contract.CodeUnavailable, "", contract.ReasonWorkspaceCheckoutFailed, "settled by the test")); ack.Release {
			t.Fatal("a refused preparation was released")
		}
		eventually(t, "rejected", func() bool { return contract.TaskTerminal(w.show(t, v.TaskID).State) })
	})
	t.Run("replay", func(t *testing.T) {
		// An authorized intent whose ref was never pushed settles failed
		// (publication_interrupted) at the next plane start.
		v := w.dispatch(t, &name, nil)
		st := taskhub.Recv(t, w.n, w.n.Starts, "task_start")
		dir, p := w.prepare(t, st)
		w.n.Prepared(t, st, nil)
		tree, _, err := taskworkspace.Snapshot(ctx, taskworkspace.SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: p.Map})
		if err != nil {
			t.Fatal(err)
		}
		tp, _ := w.p.Client.TaskPublications(st.TaskID, w.n.Assignment(st))
		cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int)}.Sealed()
		if _, err := tp.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: tree.String()}); err != nil {
			t.Fatal(err)
		}
		if v := w.show(t, v.TaskID); !v.CompletionPending || v.WorkspacePhase == nil || *v.WorkspacePhase != contract.WorkspacePhasePublishing {
			t.Fatalf("publishing view %+v", v)
		}
		w.n.Close()
		w.p.Stop()
		p2 := taskhub.RunPlane(t, w.p.Root)
		v2, err := p2.Client.ShowTask(ctx, v.TaskID, 0)
		if err != nil || v2.State != contract.TaskSucceeded || v2.Result.Workspace.Error == nil || *v2.Result.Workspace.Error != contract.PubErrPublicationInterrupted {
			t.Fatalf("replayed %+v %v", v2.Result, err)
		}
	})
}

// The plane's start-reply guard: AwaitStart takes the task lock, finds the
// reply in flight and, in waitLocked, registers its waiter before it
// releases the lock and parks in a select. Applying the reply needs that
// lock, so a goroutine parked there was registered before any later reply.
const (
	guardWait   = "/internal/plane.(*taskService).waitLocked("
	guardCaller = "/internal/plane.(*taskService).AwaitStart("
)

// guardWaiting reports whether a goroutine of this binary is parked (in a
// select) in the guard's wait called from AwaitStart.
func guardWaiting() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if !strings.Contains(header, "[select") {
			continue
		}
		if i := strings.Index(g, guardWait); i >= 0 && strings.Contains(g[i:], guardCaller) {
			return true
		}
	}
	return false
}

// awaitGuardWait returns once the guard registered a fetch's wait. answered
// reports (and consumes) the fetch's answer; an answer before the barrier
// fails the test, checked before every observation of the guard so a
// simultaneous answer is never taken for the barrier.
func awaitGuardWait(t *testing.T, answered func() (string, bool)) {
	t.Helper()
	deadline := time.Now().Add(taskhub.Wait)
	for {
		if r, ok := answered(); ok {
			t.Fatalf("the fetch was answered while the start reply was in flight: %s", r)
		}
		if guardWaiting() {
			return
		}
		if time.Now().After(deadline) {
			if r, ok := answered(); ok {
				t.Fatalf("the fetch was answered while the start reply was in flight: %s", r)
			}
			t.Fatalf("the fetch never reached the plane's start-reply guard within %v", taskhub.Wait)
		}
		time.Sleep(time.Millisecond)
	}
}

func strp(s string) *string { return &s }

var _ = bytes.NewReader
