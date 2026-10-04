package taskpublication_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskpublication"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/taskhub"
)

// UT-B2/B6 (iteration 10b): the publication state machine on the real
// workspace hub (both route families over verified TLS) and a durable
// file-backed task authority, driven by the worker's real preparation,
// snapshot and publication client. Lifecycle matrices live here, not in
// the plane. Delegated from tests/function (TestTaskWorkspacePublication
// /matrix and TestTaskWorkspaceRecovery/crash-boundaries). Do not rename.

const testWait = 30 * time.Second

func files(kv ...string) map[string]testkit.FileSpec {
	out := map[string]testkit.FileSpec{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte(kv[i+1])}
	}
	return out
}

// fixture is one hub with a seeded workspace and a worker.
type fixture struct {
	h    *taskhub.Hub
	w    *taskhub.Worker
	name string
	inst string
	base plumbing.Hash
	n    int
}

func newFixture(t *testing.T, o taskhub.Options) *fixture {
	t.Helper()
	h := taskhub.Start(t, o)
	f := &fixture{h: h, name: "proj"}
	f.inst = h.Workspace(t, f.name)
	f.base = h.Seed(t, f.name, f.inst, "main", plumbing.ZeroHash, files("a.txt", "a\n", "keep.txt", "k\n"))
	f.w = taskhub.NewWorker(t, h, 0, runtime.GOOS)
	return f
}

// task registers a live task on main.
func (f *fixture) task(t *testing.T) (taskpublication.Task, contract.NodeAssignment) {
	t.Helper()
	f.n++
	return f.h.NewTask(t, f.n, f.h.Binding(t, f.name, f.inst, nil))
}

// sealed prepares tk, applies edit in work, and seals with exit code.
func (f *fixture) sealed(t *testing.T, tk taskpublication.Task, exit int, edit func(work string)) (string, contract.PublicationCheckpoint) {
	t.Helper()
	dir, p, err := f.w.Prepare(context.Background(), t, f.h, tk)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(filepath.Join(dir, taskworkspace.WorkName))
	}
	return dir, f.w.Seal(context.Background(), t, dir, p, taskhub.Candidate(tk, exit))
}

func write(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitMessage reads a published commit's message from the hub.
func (f *fixture) commit(t *testing.T, h plumbing.Hash) *object.Commit {
	t.Helper()
	s := testkit.NewMemoryStore()
	if err := f.h.Remote(f.name, f.inst).Fetch(context.Background(), s, []plumbing.Hash{h}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := object.GetCommit(s, h)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func codeOf(err error) (contract.Code, string) {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		return "", ""
	}
	r, _ := ce.Details["reason"].(string)
	return ce.Code, r
}

// stages records the service's hook stages.
type stages struct {
	mu  sync.Mutex
	got []string
	ch  chan string
}

func newStages() *stages { return &stages{ch: make(chan string, 256)} }

func (s *stages) hook(stage, id string) {
	s.mu.Lock()
	s.got = append(s.got, stage+" "+id)
	s.mu.Unlock()
	select {
	case s.ch <- stage + " " + id:
	default:
	}
}

// await waits for stage on task id (within testWait).
func (s *stages) await(t *testing.T, stage, id string) {
	t.Helper()
	s.awaitBy(t, stage, id, time.After(testWait))
}

// reached reports whether stage on task id was recorded.
func (s *stages) reached(stage, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.got {
		if g == stage+" "+id {
			return true
		}
	}
	return false
}

// awaitBy waits for stage on task id unless timeout fires; the timeout
// rechecks the synchronized record before failing (a stage and the
// timeout both ready never fail the test).
func (s *stages) awaitBy(t testing.TB, stage, id string, timeout <-chan time.Time) {
	t.Helper()
	testkit.AwaitCondBy(t, func() bool { return s.reached(stage, id) }, s.ch, timeout, "stage "+stage+" of "+id)
}

// TestStagesAwait (review r1 C5): the stage wait, with its stage and its
// timeout both ready, never fails; with neither stage it fails.
func TestStagesAwait(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := newStages()
		s.ch <- "x"
		// The stage is recorded after the first check: the wake and the
		// timeout are then both ready.
		checks := 0
		if testkit.Fails(func(tb testing.TB) {
			testkit.AwaitCondBy(tb, func() bool {
				if checks++; checks > 1 {
					s.hook("armed", "t")
				}
				return s.reached("armed", "t")
			}, s.ch, testkit.Fired(), "stage")
		}) {
			t.Fatal("a stage wait failed with its stage and its timeout both ready")
		}
		if testkit.Fails(func(tb testing.TB) { s.awaitBy(tb, "armed", "t", testkit.Fired()) }) {
			t.Fatal("a recorded stage failed its wait")
		}
	}
	if !testkit.Fails(func(tb testing.TB) { newStages().awaitBy(tb, "armed", "t", testkit.Fired()) }) {
		t.Fatal("an absent stage passed its wait")
	}
}

func TestPublication(t *testing.T) {
	t.Parallel()
	t.Run("published", func(t *testing.T) {
		f := newFixture(t, taskhub.Options{})
		tk, a := f.task(t)
		dir, cp := f.sealed(t, tk, 0, func(work string) {
			write(t, filepath.Join(work, "a.txt"), "a2\n")
			write(t, filepath.Join(work, "new.txt"), "n\n")
			os.Remove(filepath.Join(work, "keep.txt"))
		})
		var j taskhub.Checkpoints
		pub, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, dir, cp, f.h.API(tk.TaskID, a), &j))
		if err != nil || pub.Result == nil {
			t.Fatalf("publish: %+v %v", pub, err)
		}
		w := pub.Result.Workspace
		if w == nil || w.Publication != contract.PublicationPublished || w.Diffstat.Added != 1 || w.Diffstat.Modified != 1 || w.Diffstat.Deleted != 1 || len(w.Changes) != 3 {
			t.Fatalf("result %+v", w)
		}
		st, err := f.h.M.TaskRef(context.Background(), f.name, f.inst, tk.TaskID)
		got, _ := f.h.Tasks.Get(tk.TaskID)
		if err != nil || !st.Exists || st.Commit != *w.Commit || st.PublicationID != got.Publication.ID || !got.Committed || got.State != contract.TaskSucceeded {
			t.Fatalf("ref %+v task %+v %v", st, got, err)
		}
		c := f.commit(t, plumbing.NewHash(*w.Commit))
		if c.Message != contract.TaskResultMessage(tk.TaskID, tk.RoleID, tk.Model, contract.TaskSucceeded) || len(c.ParentHashes) != 1 || c.ParentHashes[0] != f.base {
			t.Fatalf("commit %+v", c)
		}
		// The checkpoint journal: sealed -> authorized -> pushed -> settled.
		var phases []string
		for _, c := range j.List {
			p := c.Phase
			if c.Pushed && c.Phase == contract.CheckpointAuthorized {
				p += "+pushed"
			}
			phases = append(phases, p)
		}
		if strings.Join(phases, ",") != "authorized,authorized+pushed,settled" {
			t.Fatalf("checkpoints %v", phases)
		}
		// Resuming a settled checkpoint returns it; an authorized one only
		// observes (no second push).
		if again, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, dir, j.Last(), f.h.API(tk.TaskID, a), &j)); err != nil || again.Result.Digest != pub.Result.Digest {
			t.Fatalf("resume settled: %v", err)
		}
		auth := j.List[1]
		if again, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, dir, auth, f.h.API(tk.TaskID, a), &j)); err != nil || again.Result.Digest != pub.Result.Digest {
			t.Fatalf("resume authorized: %v", err)
		}
		// The settled publication's receive and the task's upload are closed.
		p, _ := f.h.Client.NodeTaskPusher(tk.TaskID, a, got.Publication.ID)
		defer p.Close()
		if err := p.PushTask(context.Background(), contract.TaskRefPrefix+tk.TaskID, f.base, bytes.NewReader(nil)); err == nil {
			t.Fatal("a settled publication accepted another receive")
		}
		g := f.h.NodeRemote(t, tk.TaskID, a)
		defer g.Close()
		if _, err := g.UploadRefs(context.Background()); err == nil {
			t.Fatal("a terminal task's upload was served")
		}
	})
	t.Run("begin", func(t *testing.T) {
		f := newFixture(t, taskhub.Options{})
		tk, a := f.task(t)
		_, cp := f.sealed(t, tk, 3, nil)
		req := contract.PublicationBeginRequest{Candidate: cp.Candidate, Tree: cp.Tree}
		r1, err := f.h.Svc.Begin(context.Background(), tk.TaskID, a, req)
		if err != nil || r1.State != contract.TaskFailed {
			t.Fatalf("begin %+v %v", r1, err)
		}
		// A byte-identical duplicate returns the original intent (never a
		// later expiry); a different candidate or tree conflicts.
		r2, err := f.h.Svc.Begin(context.Background(), tk.TaskID, a, req)
		if err != nil || r2 != r1 {
			t.Fatalf("duplicate begin %+v %v", r2, err)
		}
		other := req
		other.Tree = strings.Repeat("1", 40)
		if _, err := f.h.Svc.Begin(context.Background(), tk.TaskID, a, other); err == nil {
			t.Fatal("a different tree was accepted")
		}
		wrong := a
		wrong.NodeID = "n_" + strings.Repeat("1", 32)
		if _, err := f.h.Svc.Begin(context.Background(), tk.TaskID, wrong, req); err == nil {
			t.Fatal("a wrong node began a publication")
		}
		foreign := req
		foreign.Candidate.TaskID = taskhub.TaskID(99)
		if code, _ := codeOf(func() error { _, err := f.h.Svc.Begin(context.Background(), tk.TaskID, a, foreign); return err }()); code != contract.CodeInvalidArgument {
			t.Fatal("a candidate of another task was accepted")
		}
		// Refusals without intent: lost, decided, stale attachment.
		tk2, a2 := f.task(t)
		lost := contract.TaskResultBody{TaskID: tk2.TaskID, Execution: tk2.Execution, Outcome: contract.OutcomeLost}.Sealed()
		if _, err := f.h.Svc.Begin(context.Background(), tk2.TaskID, a2, contract.PublicationBeginRequest{Candidate: lost, Tree: cp.Tree}); err == nil {
			t.Fatal("a lost outcome began a publication")
		}
		f.h.Tasks.Update(tk2.TaskID, func(t *taskpublication.Task, _ *bool) { t.Live = false })
		if _, err := f.h.Svc.Begin(context.Background(), tk2.TaskID, a2, contract.PublicationBeginRequest{Candidate: taskhub.Candidate(tk2, 0), Tree: cp.Tree}); err == nil {
			t.Fatal("a stale attachment began a publication")
		}
		f.h.Tasks.Update(tk2.TaskID, func(t *taskpublication.Task, _ *bool) { t.State = contract.TaskLost })
		if _, err := f.h.Svc.Begin(context.Background(), tk2.TaskID, a2, contract.PublicationBeginRequest{Candidate: taskhub.Candidate(tk2, 0), Tree: cp.Tree}); err == nil {
			t.Fatal("a decided task began a publication")
		}
		if got, _ := f.h.Tasks.Get(tk2.TaskID); got.Publication != nil {
			t.Fatal("a refused Begin left an intent")
		}
		// A durable stop request makes the canonical state cancelled.
		tk3, a3 := f.task(t)
		f.h.Tasks.Update(tk3.TaskID, func(_ *taskpublication.Task, stop *bool) { *stop = true })
		dir3, cp3 := f.sealed(t, tk3, 0, func(work string) { write(t, filepath.Join(work, "part.txt"), "p") })
		var j taskhub.Checkpoints
		pub, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk3, dir3, cp3, f.h.API(tk3.TaskID, a3), &j))
		if err != nil || pub.Result == nil || pub.Result.Outcome != contract.OutcomeCancelled {
			t.Fatalf("cancelled publish %+v %v", pub, err)
		}
		c := f.commit(t, plumbing.NewHash(*pub.Result.Workspace.Commit))
		if !strings.HasSuffix(c.Message, "state: cancelled\n") {
			t.Fatalf("message %q", c.Message)
		}
	})
	t.Run("finish", func(t *testing.T) {
		f := newFixture(t, taskhub.Options{})
		tk, a := f.task(t)
		_, cp := f.sealed(t, tk, 0, nil)
		r, err := f.h.Svc.Begin(context.Background(), tk.TaskID, a, contract.PublicationBeginRequest{Candidate: cp.Candidate, Tree: cp.Tree})
		if err != nil {
			t.Fatal(err)
		}
		st, err := f.h.Svc.Observe(tk.TaskID, a, "")
		if err != nil || st.Phase != contract.PubPhaseAuthorized || st.PublicationID == nil || *st.PublicationID != r.PublicationID {
			t.Fatalf("lookup %+v %v", st, err)
		}
		st, err = f.h.Svc.Finish(context.Background(), tk.TaskID, a, r.PublicationID, contract.PubErrPublicationTransportFail)
		if err != nil || st.Phase != contract.PubPhaseSettled || st.Result.Workspace.Publication != contract.PublicationFailed || !st.Committed {
			t.Fatalf("finish %+v %v", st, err)
		}
		if _, err := f.h.Svc.Observe(tk.TaskID, a, strings.Repeat("0", 32)); err == nil {
			t.Fatal("an unknown publication was observed")
		}
		if _, err := f.h.Svc.Finish(context.Background(), tk.TaskID, a, strings.Repeat("0", 32), contract.PubErrStorageFailed); err == nil {
			t.Fatal("an unknown publication was finished")
		}
		// A repeated Finish answers the settlement.
		if st, err := f.h.Svc.Finish(context.Background(), tk.TaskID, a, r.PublicationID, contract.PubErrStorageFailed); err != nil || st.Result.Workspace.Error == nil ||
			*st.Result.Workspace.Error != contract.PubErrPublicationTransportFail {
			t.Fatalf("repeated finish %+v %v", st, err)
		}
		f.noRef(t, tk.TaskID)
		// No intent: the lookup is phase none.
		tk2, a2 := f.task(t)
		if st, err := f.h.Svc.Observe(tk2.TaskID, a2, ""); err != nil || st.Phase != contract.PubPhaseNone || st.PublicationID != nil {
			t.Fatalf("lookup none %+v %v", st, err)
		}
	})
	t.Run("finish-after-commit", func(t *testing.T) {
		// The push committed but its answer was lost and the worker
		// believed it failed: Finish inspects the provenance first and
		// settles published.
		f := newFixture(t, taskhub.Options{})
		tk, a := f.task(t)
		dir, cp := f.sealed(t, tk, 0, func(work string) { write(t, filepath.Join(work, "x"), "x") })
		api := f.h.API(tk.TaskID, a)
		lost := 0
		api.Lose = func(op string) bool {
			if op == "observe" && lost == 0 {
				lost++
				return true
			}
			return false
		}
		var j taskhub.Checkpoints
		pub, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, dir, cp, api, &j))
		if err != nil || pub.Result == nil || pub.Result.Workspace.Publication != contract.PublicationPublished {
			t.Fatalf("publish %+v %v", pub, err)
		}
		got, _ := f.h.Tasks.Get(tk.TaskID)
		st, err := f.h.Svc.Finish(context.Background(), tk.TaskID, a, got.Publication.ID, contract.PubErrPublicationTransportFail)
		if err != nil || st.Result.Workspace.Publication != contract.PublicationPublished {
			t.Fatalf("finish after commit %+v %v", st, err)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		// The hub bounds a receive by the intent's deadline on the wall
		// clock: the fake clock starts now.
		clk := testkit.NewFakeClock(time.Now().UTC())
		ev := newStages()
		f := newFixture(t, taskhub.Options{Clock: clk, Hook: ev.hook})
		tk, a := f.task(t)
		_, cp := f.sealed(t, tk, 0, nil)
		r, err := f.h.Svc.Begin(context.Background(), tk.TaskID, a, contract.PublicationBeginRequest{Candidate: cp.Candidate, Tree: cp.Tree})
		if err != nil {
			t.Fatal(err)
		}
		// Arm before advancing: the intent's own expiry timer.
		ev.await(t, "expiry-armed", tk.TaskID)
		if err := clk.AwaitWaiter(testWait, testkit.HasTimer(contract.PublicationExpiry)); err != nil {
			t.Fatal(err)
		}
		clk.Advance(contract.PublicationExpiry)
		if err := f.h.Tasks.AwaitDurable(ctxT(t), tk.TaskID); err != nil {
			t.Fatal(err)
		}
		st, err := f.h.Svc.Observe(tk.TaskID, a, r.PublicationID)
		if err != nil || st.Result.Workspace.Error == nil || *st.Result.Workspace.Error != contract.PubErrPublicationTimeout {
			t.Fatalf("expired %+v %v", st, err)
		}
		// A late receive is closed before any pack byte.
		p, _ := f.h.Client.NodeTaskPusher(tk.TaskID, a, r.PublicationID)
		defer p.Close()
		err = p.PushTask(context.Background(), contract.TaskRefPrefix+tk.TaskID, f.base, bytes.NewReader(nil))
		if code, reason := codeOf(err); code != contract.CodeConflict || reason != contract.ReasonTaskPublicationClosed {
			t.Fatalf("late receive %v", err)
		}
		// An expiry after the commit point settles published.
		tk2, a2 := f.task(t)
		dir2, cp2 := f.sealed(t, tk2, 0, func(work string) { write(t, filepath.Join(work, "y"), "y") })
		failed := false
		f.h.Tasks.FailWrite = func(op, id string) error {
			if op == "settle" && id == tk2.TaskID && !failed {
				failed = true
				return errors.New("terminal record write failed (fixture)")
			}
			return nil
		}
		api := f.h.API(tk2.TaskID, a2)
		api.Fail = func(op string) error {
			if op == "observe" || op == "finish" {
				return contract.New(contract.CodeUnavailable, "plane unreachable (fixture)")
			}
			return nil
		}
		var j taskhub.Checkpoints
		if pub, err := untilRetry(taskhub.Input(tk2, dir2, cp2, api, &j)); err == nil {
			t.Fatalf("an unobservable publication resolved: %+v (%d checkpoints)", pub, len(j.List))
		} else {
			t.Logf("unresolved: %v; %d checkpoints", err, len(j.List))
		}
		if st, _ := f.h.M.TaskRef(context.Background(), f.name, f.inst, tk2.TaskID); !st.Exists || !f.h.M.Fenced(f.name) {
			t.Fatalf("the committed ref %+v must stay fenced until its terminal record", st)
		}
		ev.await(t, "expiry-armed", tk2.TaskID)
		clk.Advance(contract.PublicationExpiry)
		if err := f.h.Tasks.AwaitDurable(ctxT(t), tk2.TaskID); err != nil {
			t.Fatal(err)
		}
		got, _ := f.h.Tasks.Get(tk2.TaskID)
		if got.Result.Workspace.Publication != contract.PublicationPublished {
			t.Fatalf("expiry after commit %+v", got.Result.Workspace)
		}
		eventually(t, "fence released", func() bool { return !f.h.M.Fenced(f.name) })
	})
	t.Run("fences", func(t *testing.T) {
		f := newFixture(t, taskhub.Options{})
		release := f.h.M.FencePublication(f.name, taskhub.TaskID(1), strings.Repeat("a", 32))
		done := make(chan error, 1)
		go func() {
			_, err := f.h.M.Remove(context.Background(), f.name, f.inst)
			done <- err
		}()
		testkit.AbsentFor(t, done, time.After(50*time.Millisecond), "rm passed a publication fence")
		ctx, cancel := context.WithCancel(context.Background())
		pdone := make(chan error, 1)
		go func() {
			_, err := f.h.M.Prune(ctx, f.name, f.inst, time.Now())
			pdone <- err
		}()
		cancel()
		if err := testkit.WithinBy(t, pdone, time.After(testWait), "the cancelled prune's return"); err == nil {
			t.Fatal("a cancelled prune behind a fence succeeded")
		}
		release()
		if err := testkit.WithinBy(t, done, time.After(testWait), "rm resuming after the fence"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("expected-commit", func(t *testing.T) {
		b := contract.WorkspaceBinding{Name: "p", Instance: strings.Repeat("a", 32), BaseSelector: contract.SelectorEmpty}
		h1, err := taskpublication.ExpectedCommit(taskhub.TaskID(1), "r", "m", contract.TaskSucceeded, b, strings.Repeat("b", 40))
		if err != nil {
			t.Fatal(err)
		}
		h2, _ := taskpublication.ExpectedCommit(taskhub.TaskID(2), "r", "m", contract.TaskSucceeded, b, strings.Repeat("b", 40))
		if h1 == h2 {
			t.Fatal("two tasks share a result commit")
		}
		if _, err := taskpublication.ExpectedCommit(taskhub.TaskID(1), "r\n", "m", contract.TaskSucceeded, b, strings.Repeat("b", 40)); err == nil {
			t.Fatal("a control character entered a commit message")
		}
	})
}

// TestPublicationCrash restarts the plane side (every store reopened from
// disk, the startup replay before serving) at each boundary between the
// task and workspace stores.
func TestPublicationCrash(t *testing.T) {
	t.Parallel()
	t.Run("intent-without-ref", func(t *testing.T) {
		dir := t.TempDir()
		f := newFixture(t, taskhub.Options{Dir: dir})
		tk, a := f.task(t)
		_, cp := f.sealed(t, tk, 0, nil)
		r, err := f.h.Svc.Begin(context.Background(), tk.TaskID, a, contract.PublicationBeginRequest{Candidate: cp.Candidate, Tree: cp.Tree})
		if err != nil {
			t.Fatal(err)
		}
		h, err := f.h.Restart(t)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := h.Tasks.Get(tk.TaskID)
		if got.Publication.ID != r.PublicationID || got.Result.Workspace.Error == nil || *got.Result.Workspace.Error != contract.PubErrPublicationInterrupted {
			t.Fatalf("replayed %+v", got.Result)
		}
	})
	t.Run("ref-without-terminal", func(t *testing.T) {
		f := newFixture(t, taskhub.Options{Dir: t.TempDir()})
		tk, a := f.task(t)
		tdir, j := f.committedUnsettled(t, tk, a)
		st, _ := f.h.M.TaskRef(context.Background(), f.name, f.inst, tk.TaskID)
		h, err := f.h.Restart(t)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := h.Tasks.Get(tk.TaskID)
		if !got.Committed || got.Result.Workspace.Publication != contract.PublicationPublished || *got.Result.Workspace.Commit != st.Commit || h.M.Fenced(f.name) {
			t.Fatalf("replayed %+v", got.Result)
		}
		// The worker's restarted publication (authorized, pushed) observes
		// the replayed settlement and never pushes again.
		pub, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, tdir, j.Last(), h.API(tk.TaskID, a), j))
		if err != nil || pub.Result.Digest != got.Result.Digest {
			t.Fatalf("worker resume %+v %v", pub, err)
		}
	})
	t.Run("unconfirmed-current", func(t *testing.T) {
		// Review r1 C1: the receive's CURRENT rename succeeds but the
		// workspace directory's sync fails, an ambiguous storage outcome.
		// Nothing settles from the in-memory provenance: the receive reports
		// no success, Finish and the intent's expiry leave the task
		// completion-pending and the publication fenced. The restart's
		// CURRENT validation recovers the generation and the replay settles
		// it published; the worker's resumed publication observes that.
		dir := t.TempDir()
		clk := testkit.NewFakeClock(time.Now().UTC())
		ev := newStages()
		wsDir := filepath.Join(dir, "plane", "workspaces", "proj")
		var armed, hit atomic.Bool
		f := newFixture(t, taskhub.Options{Dir: dir, Clock: clk, Hook: ev.hook, DirSyncFault: func(d string) error {
			if d == wsDir && armed.CompareAndSwap(true, false) {
				hit.Store(true)
				return errors.New("injected workspace directory sync failure (fixture)")
			}
			return nil
		}})
		tk, a := f.task(t)
		tdir, cp := f.sealed(t, tk, 0, func(work string) { write(t, filepath.Join(work, "u"), "u") })
		j := &taskhub.Checkpoints{}
		armed.Store(true)
		if pub, err := untilRetry(taskhub.Input(tk, tdir, cp, f.h.API(tk.TaskID, a), j)); err == nil {
			t.Fatalf("an unconfirmed publication resolved: %+v", pub)
		}
		if !hit.Load() {
			t.Fatal("the CURRENT directory sync was never reached")
		}
		pending := func(what string) {
			t.Helper()
			got, _ := f.h.Tasks.Get(tk.TaskID)
			if got.Committed || got.Result != nil || got.Publication == nil || got.Publication.Phase != contract.PubPhaseAuthorized || !f.h.M.Fenced(f.name) {
				t.Fatalf("%s: settled from unconfirmed CURRENT: %+v fenced %v", what, got.Publication, f.h.M.Fenced(f.name))
			}
		}
		pending("after the receive")
		if _, err := f.h.M.TaskRef(context.Background(), f.name, f.inst, tk.TaskID); err == nil {
			t.Fatal("unconfirmed CURRENT was read as provenance")
		}
		got, _ := f.h.Tasks.Get(tk.TaskID)
		if _, err := f.h.Svc.Finish(context.Background(), tk.TaskID, a, got.Publication.ID, contract.PubErrPublicationTransportFail); err == nil {
			t.Fatal("Finish settled an unconfirmed publication")
		}
		pending("after Finish")
		// Arm before advancing: the intent's own expiry timer.
		ev.await(t, "expiry-armed", tk.TaskID)
		if err := clk.AwaitWaiter(testWait, testkit.HasTimer(contract.PublicationExpiry)); err != nil {
			t.Fatal(err)
		}
		clk.Advance(contract.PublicationExpiry)
		ev.await(t, "expiry-unsettled", tk.TaskID)
		pending("after the expiry")
		h, err := f.h.Restart(t)
		if err != nil {
			t.Fatal(err)
		}
		got, _ = h.Tasks.Get(tk.TaskID)
		st, serr := h.M.TaskRef(context.Background(), f.name, f.inst, tk.TaskID)
		if serr != nil || !st.Exists || !got.Committed || got.Result.Workspace.Publication != contract.PublicationPublished ||
			*got.Result.Workspace.Commit != st.Commit || h.M.Fenced(f.name) {
			t.Fatalf("replayed %+v ref %+v %v", got.Result, st, serr)
		}
		pub, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, tdir, j.Last(), h.API(tk.TaskID, a), j))
		if err != nil || pub.Result.Digest != got.Result.Digest {
			t.Fatalf("worker resume %+v %v", pub, err)
		}
	})
	t.Run("authorized-before-push", func(t *testing.T) {
		// Review r1 C4: the worker crashes between its authorized checkpoint
		// and its push checkpoint. Its restarted (recovered) publication
		// only observes the intent: the plane's expiry settles it failed
		// (publication_timeout) and no task ref is ever created.
		clk := testkit.NewFakeClock(time.Now().UTC())
		ev := newStages()
		f := newFixture(t, taskhub.Options{Dir: t.TempDir(), Clock: clk, Hook: ev.hook})
		tk, a := f.task(t)
		tdir, cp := f.sealed(t, tk, 0, func(work string) { write(t, filepath.Join(work, "v"), "v") })
		crash := errors.New("crash before the push checkpoint (fixture)")
		j := &taskhub.Checkpoints{Fail: func(c contract.PublicationCheckpoint) error {
			if c.Pushed {
				return crash
			}
			return nil
		}}
		if _, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, tdir, cp, f.h.API(tk.TaskID, a), j)); !errors.Is(err, crash) {
			t.Fatalf("first run: %v", err)
		}
		j.Fail = nil
		api := f.h.API(tk.TaskID, a)
		var mu sync.Mutex
		var ops []string
		api.Fail = func(op string) error {
			mu.Lock()
			ops = append(ops, op)
			mu.Unlock()
			return nil
		}
		in := taskhub.Input(tk, tdir, j.Last(), api, j)
		in.Recovered = true
		observing := make(chan struct{}, 1)
		in.Hook = func(s string) {
			if s == "retry-armed" {
				select {
				case observing <- struct{}{}:
				default:
				}
			}
		}
		done := make(chan error, 1)
		go func() {
			pub, err := taskworkspace.Publish(context.Background(), in)
			if err == nil && (pub.Result == nil || pub.Result.Workspace.Error == nil || *pub.Result.Workspace.Error != contract.PubErrPublicationTimeout) {
				err = errors.New("the recovered publication did not adopt the expiry's settlement")
			}
			done <- err
		}()
		testkit.WithinBy(t, observing, time.After(testWait), "the recovered publication observing")
		ev.await(t, "expiry-armed", tk.TaskID)
		if err := clk.AwaitWaiter(testWait, testkit.HasTimer(contract.PublicationExpiry)); err != nil {
			t.Fatal(err)
		}
		clk.Advance(contract.PublicationExpiry)
		if err := testkit.WithinBy(t, done, time.After(testWait), "the recovered publication's end"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, op := range ops {
			if op != "observe" {
				t.Fatalf("the recovered publication called %s (ops %v)", op, ops)
			}
		}
		f.noRef(t, tk.TaskID)
	})
	t.Run("ref-without-provenance", func(t *testing.T) {
		// The task's ref carries another intent's provenance: corruption.
		// Replay fails for that record (never guessed) and keeps its fence.
		f := newFixture(t, taskhub.Options{Dir: t.TempDir()})
		tk, a := f.task(t)
		f.committedUnsettled(t, tk, a)
		f.h.Tasks.FailWrite = nil
		f.h.Tasks.Update(tk.TaskID, func(t *taskpublication.Task, _ *bool) {
			p := *t.Publication
			p.ID = strings.Repeat("c", 32)
			t.Publication = &p
		})
		h, err := f.h.Restart(t)
		if err == nil {
			t.Fatal("replay adopted a ref without matching provenance")
		}
		if got, _ := h.Tasks.Get(tk.TaskID); got.Committed || !h.M.Fenced(f.name) {
			t.Fatalf("a corrupt record was settled or unfenced: %+v", got)
		}
	})
}

// noRef requires that the workspace has no ref for task id.
func (f *fixture) noRef(t *testing.T, id string) {
	t.Helper()
	if st, err := f.h.M.TaskRef(context.Background(), f.name, f.inst, id); err != nil || st.Exists {
		t.Fatalf("task %s ref %+v %v", id, st, err)
	}
}

// committedUnsettled publishes tk's result while every terminal record
// write fails and the plane is unobservable: the ref is committed and its
// intent stays authorized (the crash between the stores).
func (f *fixture) committedUnsettled(t *testing.T, tk taskpublication.Task, a contract.NodeAssignment) (string, *taskhub.Checkpoints) {
	t.Helper()
	tdir, cp := f.sealed(t, tk, 0, func(work string) { write(t, filepath.Join(work, "z"), "z") })
	f.h.Tasks.FailWrite = func(op, id string) error {
		if op == "settle" {
			return errors.New("crash before the terminal record (fixture)")
		}
		return nil
	}
	api := f.h.API(tk.TaskID, a)
	api.Fail = func(op string) error {
		if op == "observe" || op == "finish" {
			return contract.New(contract.CodeUnavailable, "plane down (fixture)")
		}
		return nil
	}
	j := &taskhub.Checkpoints{}
	if _, err := untilRetry(taskhub.Input(tk, tdir, cp, api, j)); err == nil {
		t.Fatal("publication resolved without its terminal record")
	}
	if st, _ := f.h.M.TaskRef(context.Background(), f.name, f.inst, tk.TaskID); !st.Exists {
		t.Fatal("the ref was not committed")
	}
	return tdir, j
}

// untilRetry runs a publication whose plane answers are unavailable until,
// after its push, it arms its first retry wait (it can then never resolve
// on its own), and cancels it there: an event-armed stop, not a wall-clock
// wait. The hook runs on the publishing goroutine.
func untilRetry(in taskworkspace.PublishInput) (taskworkspace.Publication, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pushed := false
	in.Hook = func(s string) {
		switch {
		case s == "push":
			pushed = true
		case s == "retry-armed" && pushed:
			cancel()
		}
	}
	return taskworkspace.Publish(ctx, in)
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	t.Cleanup(cancel)
	return ctx
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			if cond() {
				return
			}
			t.Fatalf("%s never held", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
