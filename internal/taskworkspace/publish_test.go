package taskworkspace

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// fakeAPI is a scriptable plane publication API: each hook receives its
// call number; nil hooks answer the happy path.
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	begin   func(n int) (contract.PublicationBeginResponse, error)
	finish  func(n int, code string) (contract.PublicationStatus, error)
	observe func(n int, pub string) (contract.PublicationStatus, error)
	pusher  func() (Pusher, error)
	settled contract.PublicationStatus
	n       map[string]int
}

func (a *fakeAPI) count(op string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n == nil {
		a.n = map[string]int{}
	}
	a.calls = append(a.calls, op)
	a.n[op]++
	return a.n[op] - 1
}

func (a *fakeAPI) ops() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.calls, ",")
}

const testPub = "0123456789abcdef0123456789abcdef"

func (a *fakeAPI) Begin(ctx context.Context, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error) {
	n := a.count("begin")
	if a.begin != nil {
		return a.begin(n)
	}
	return contract.PublicationBeginResponse{PublicationID: testPub, State: contract.TaskSucceeded, ExpiresAt: "2026-10-03T10:05:00Z"}, nil
}

func (a *fakeAPI) Finish(ctx context.Context, pubID, code string) (contract.PublicationStatus, error) {
	n := a.count("finish:" + code)
	if a.finish != nil {
		return a.finish(n, code)
	}
	return a.settled, nil
}

func (a *fakeAPI) Observe(ctx context.Context, pubID string) (contract.PublicationStatus, error) {
	n := a.count("observe")
	if a.observe != nil {
		return a.observe(n, pubID)
	}
	return a.settled, nil
}

func (a *fakeAPI) Pusher(pubID string) (Pusher, error) {
	a.count("pusher")
	if a.pusher != nil {
		return a.pusher()
	}
	return &fakePusher{}, nil
}

// fakePusher reads (and decodes) the pushed pack.
type fakePusher struct {
	refs    map[string]plumbing.Hash
	refsErr error
	pushErr error
	mu      sync.Mutex
	got     *memory.Storage
	ref     string
	commit  plumbing.Hash
}

func (p *fakePusher) ReceiveRefs(ctx context.Context) (map[string]plumbing.Hash, error) {
	return p.refs, p.refsErr
}

func (p *fakePusher) PushTask(ctx context.Context, ref string, commit plumbing.Hash, pack io.Reader) error {
	s := memory.NewStorage()
	if err := packfile.UpdateObjectStorage(s, pack); err != nil {
		return err
	}
	p.mu.Lock()
	p.got, p.ref, p.commit = s, ref, commit
	p.mu.Unlock()
	return p.pushErr
}

func (p *fakePusher) Close() {}

// stalledPusher is a receive peer that stops reading the pack after its
// first chunk: the pack writer blocks in its pipe until the push ends.
type stalledPusher struct {
	fakePusher
	parked chan struct{}
}

func (p *stalledPusher) PushTask(ctx context.Context, ref string, commit plumbing.Hash, pack io.Reader) error {
	if _, err := pack.Read(make([]byte, 8)); err != nil {
		return err
	}
	p.parked <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

// pubFixture is a sealed task ready to publish.
type pubFixture struct {
	dir string
	cp  contract.PublicationCheckpoint
	b   contract.WorkspaceBinding
	in  PublishInput
	j   []contract.PublicationCheckpoint
	clk *testkit.FakeClock
	mu  sync.Mutex
	st  []string
	sch chan string
}

// pubTemplate is one sealed task directory (a prepared checkout of base
// "a" with one new file) and its result tree, built once per TestPublish:
// each fixture publishes from its own mode-preserving copy (fixture setup
// only; the state machine under test writes only its copy).
type pubTemplate struct {
	dir  string
	base plumbing.Hash
	tree string
}

func newPubTemplate(t *testing.T) *pubTemplate {
	t.Helper()
	src := newSource(t)
	base := src.commit(spec("a", "a"))
	m, _ := openCache(t, 0)
	dir, p := prepared(t, m, &fetcher{src: src}, binding(base), false)
	writeFile(t, dir+"/"+WorkName+"/new.txt", "n", 0o644)
	tree, _, err := Snapshot(context.Background(), SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: p.Map})
	if err != nil {
		t.Fatal(err)
	}
	return &pubTemplate{dir: dir, base: base, tree: tree.String()}
}

func newPubFixture(t *testing.T, tp *pubTemplate, api *fakeAPI) *pubFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "task")
	if err := testkit.CopyTree(dir, tp.dir); err != nil {
		t.Fatal(err)
	}
	base, tree := tp.base, tp.tree
	zero := 0
	exec := contract.ExecutionToken{Epoch: strings.Repeat("e", 32), Attachment: 1}
	cand := contract.TaskResultBody{TaskID: taskID(1), Execution: exec, Outcome: contract.OutcomeNatural, ExitCode: &zero}.Sealed()
	f := &pubFixture{dir: dir, b: binding(base), clk: testkit.NewFakeClock(time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)), sch: make(chan string, 256)}
	f.cp = contract.PublicationCheckpoint{TaskID: cand.TaskID, Execution: exec, Phase: contract.CheckpointSealed, Candidate: cand, Tree: tree}
	// The plane's canonical settlement of this execution.
	w := contract.NewPublicationFailed(f.b, contract.PubErrStorageFailed)
	res := cand
	res.Workspace = &w
	res = res.Sealed()
	api.settled = contract.PublicationStatus{Phase: contract.PubPhaseSettled, Result: &res, TaskState: contract.TaskSucceeded, Committed: true}
	api.t = t
	f.in = PublishInput{TaskID: cand.TaskID, RoleID: "worker-a", Model: "model", TaskDir: dir, Binding: f.b, Checkpoint: f.cp, API: api, Clock: f.clk,
		Save: func(c contract.PublicationCheckpoint) error {
			if _, err := contract.EncodePublicationCheckpoint(c); err != nil {
				return err
			}
			f.mu.Lock()
			f.j = append(f.j, c)
			f.mu.Unlock()
			return nil
		},
		Hook: func(s string) {
			f.mu.Lock()
			f.st = append(f.st, s)
			f.mu.Unlock()
			select {
			case f.sch <- s:
			default:
			}
		}}
	return f
}

// advanceRetry waits until the retry timer is armed (its stage and the
// clock's waiter), then fires it.
func (f *pubFixture) advanceRetry(t *testing.T) {
	t.Helper()
	awaitStageBy(t, f.sch, "retry-armed", time.After(testWait))
	if err := f.clk.AwaitWaiter(testWait, testkit.HasTimer(time.Second)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(time.Second)
}

func (f *pubFixture) phases() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.j {
		p := c.Phase
		if c.Pushed && p == contract.CheckpointAuthorized {
			p += "+pushed"
		}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

// run publishes in the background and returns its result channel.
func run(in PublishInput, ctx context.Context) chan struct {
	p   Publication
	err error
} {
	ch := make(chan struct {
		p   Publication
		err error
	}, 1)
	go func() {
		p, err := Publish(ctx, in)
		ch <- struct {
			p   Publication
			err error
		}{p, err}
	}()
	return ch
}

// await receives the background publication's end; its timeout rechecks
// the result channel before failing (withinBy).
func await[T any](t *testing.T, ch chan T) T {
	t.Helper()
	return within(t, ch, "the publication's end")
}

func conflict(reason string) error {
	return contract.TaskError(contract.CodeConflict, "", reason, "refused (fixture)")
}

var unavailable = contract.New(contract.CodeUnavailable, "lost (fixture)")

// TestPublish is UT-B6/B8 on the worker side (iteration 10b): the
// checkpointed publication state machine against a scripted plane:
// arbitration refusals, ambiguity resolved only by repetition or
// observation, the finalization bound, local failures after Begin,
// definitive push refusals, resumption and cancellation. Retries wait on
// armed fake-clock timers. Delegated from tests/function
// (TestTaskWorkspacePublication/matrix). Do not rename it.
func TestPublish(t *testing.T) {
	t.Parallel()
	bg := context.Background()
	tp := newPubTemplate(t)
	t.Run("published", func(t *testing.T) {
		pusher := &fakePusher{refs: map[string]plumbing.Hash{}}
		api := &fakeAPI{pusher: func() (Pusher, error) { return pusher, nil }}
		f := newPubFixture(t, tp, api)
		f.in.Retry = 0
		p, err := Publish(bg, f.in)
		if err != nil || p.Result == nil || p.Failed != "" {
			t.Fatalf("publish %+v %v", p, err)
		}
		if f.phases() != "authorized,authorized+pushed,settled" || api.ops() != "begin,pusher,observe" {
			t.Fatalf("phases %s ops %s", f.phases(), api.ops())
		}
		// The pushed pack holds the complete result closure.
		if pusher.ref != contract.TaskRefPrefix+taskID(1) || pusher.got.HasEncodedObject(pusher.commit) != nil {
			t.Fatalf("pushed %s %s", pusher.ref, pusher.commit)
		}
		// Resuming: settled returns at once; authorized+pushed only observes.
		f.in.Checkpoint = f.j[2]
		if p, err := Publish(bg, f.in); err != nil || p.Result == nil {
			t.Fatal(err)
		}
		f.in.Checkpoint = f.j[1]
		api.calls = nil
		if p, err := Publish(bg, f.in); err != nil || p.Result == nil || api.ops() != "observe" {
			t.Fatalf("resume authorized: %v ops %s", err, api.ops())
		}
		// The advertised base is not resent.
		pusher2 := &fakePusher{refs: map[string]plumbing.Hash{"refs/heads/base": plumbing.NewHash(*f.b.BaseCommit)}}
		api2 := &fakeAPI{pusher: func() (Pusher, error) { return pusher2, nil }}
		f2 := newPubFixture(t, tp, api2)
		if _, err := Publish(bg, f2.in); err != nil {
			t.Fatal(err)
		}
		if pusher2.got.HasEncodedObject(plumbing.NewHash(*f2.b.BaseCommit)) == nil {
			t.Fatal("the advertised base commit was pushed again")
		}
		f.in.Checkpoint.Phase = "odd"
		if _, err := Publish(bg, f.in); err == nil {
			t.Fatal("an unknown checkpoint phase published")
		}
	})
	t.Run("recovered", func(t *testing.T) {
		// Review r1 C4: the crash falls between the authorized checkpoint
		// and the push checkpoint. A restarted sidecar's recovery mode only
		// observes that intent until the plane settles it: no fresh push.
		api := &fakeAPI{}
		api.observe = func(n int, _ string) (contract.PublicationStatus, error) {
			if n == 0 {
				return contract.PublicationStatus{Phase: contract.PubPhaseAuthorized, TaskState: contract.TaskRunning}, nil
			}
			return api.settled, nil
		}
		f := newPubFixture(t, tp, api)
		save := f.in.Save
		crash := errors.New("crash before the push checkpoint (fixture)")
		f.in.Save = func(c contract.PublicationCheckpoint) error {
			if c.Pushed {
				return crash
			}
			return save(c)
		}
		if _, err := Publish(bg, f.in); !errors.Is(err, crash) || api.ops() != "begin" || f.phases() != "authorized" || f.j[0].Pushed {
			t.Fatalf("first run: %v ops %s phases %s", err, api.ops(), f.phases())
		}
		api.mu.Lock()
		api.calls = nil
		api.mu.Unlock()
		f.in.Save, f.in.Checkpoint, f.in.Recovered = save, f.j[0], true
		ch := run(f.in, bg)
		f.advanceRetry(t)
		if r := await(t, ch); r.err != nil || r.p.Result == nil || api.ops() != "observe,observe" || f.phases() != "authorized,settled" {
			t.Fatalf("recovered run: %+v %v ops %s phases %s", r.p, r.err, api.ops(), f.phases())
		}
		// A recovered sealed checkpoint's Begin answers an intent that may
		// be the one authorized before the shutdown: observed too.
		api2 := &fakeAPI{}
		f2 := newPubFixture(t, tp, api2)
		f2.in.Recovered = true
		if p, err := Publish(bg, f2.in); err != nil || p.Result == nil || api2.ops() != "begin,observe" || f2.phases() != "authorized,settled" {
			t.Fatalf("recovered Begin: %+v %v ops %s phases %s", p, err, api2.ops(), f2.phases())
		}
	})
	t.Run("begin-refusals", func(t *testing.T) {
		for err, want := range map[error]string{
			conflict(contract.ReasonTaskPublicationClosed):                        contract.PubErrPublicationClosed,
			conflict(contract.ReasonTaskAssignmentMismatch):                       contract.PubErrAssignmentMismatch,
			contract.TaskError(contract.CodeNotFound, "", "", "gone"):             contract.PubErrWorkspaceUnavailable,
			contract.TaskError(contract.CodeConflict, "", "", "x"):                contract.PubErrPublicationClosed,
			contract.TaskError(contract.CodeInvalidArgument, "", "", "malformed"): contract.PubErrPublicationTransportFail,
		} {
			api := &fakeAPI{begin: func(int) (contract.PublicationBeginResponse, error) { return contract.PublicationBeginResponse{}, err }}
			f := newPubFixture(t, tp, api)
			p, perr := Publish(bg, f.in)
			if perr != nil || p.Failed != want || p.Result != nil || len(f.j) != 0 {
				t.Fatalf("%v: %+v %v (want %s)", err, p, perr, want)
			}
		}
	})
	t.Run("begin-ambiguous", func(t *testing.T) {
		api := &fakeAPI{begin: func(n int) (contract.PublicationBeginResponse, error) {
			if n == 0 {
				return contract.PublicationBeginResponse{}, unavailable
			}
			return contract.PublicationBeginResponse{PublicationID: testPub, State: contract.TaskFailed, ExpiresAt: "2026-10-03T10:05:00Z"}, nil
		}}
		f := newPubFixture(t, tp, api)
		ch := run(f.in, bg)
		f.advanceRetry(t)
		r := await(t, ch)
		if r.err != nil || r.p.Result == nil || api.ops() != "begin,begin,pusher,observe" || *f.j[0].State != contract.TaskFailed {
			t.Fatalf("%+v %v ops %s", r.p, r.err, api.ops())
		}
	})
	t.Run("finalization-bound", func(t *testing.T) {
		for name, c := range map[string]struct {
			lookup  contract.PublicationStatus
			want    string
			wantOps string
		}{
			"no-intent":  {contract.PublicationStatus{WithID: true, Phase: contract.PubPhaseNone, TaskState: contract.TaskRunning}, contract.PubErrPublicationTimeout, "begin,observe"},
			"authorized": {contract.PublicationStatus{WithID: true, PublicationID: strPtr(testPub), Phase: contract.PubPhaseAuthorized, TaskState: contract.TaskRunning}, "", "begin,observe,finish:publication_timeout"},
			"settled":    {contract.PublicationStatus{}, "", "begin,observe"},
		} {
			api := &fakeAPI{}
			f := newPubFixture(t, tp, api)
			lookup := c.lookup
			if name == "settled" {
				lookup = api.settled
				lookup.WithID, lookup.PublicationID = true, strPtr(testPub)
			}
			api.begin = func(int) (contract.PublicationBeginResponse, error) {
				return contract.PublicationBeginResponse{}, context.DeadlineExceeded
			}
			api.observe = func(int, string) (contract.PublicationStatus, error) { return lookup, nil }
			bctx, cancel := context.WithCancel(bg)
			cancel()
			f.in.Begin = bctx
			p, err := Publish(bg, f.in)
			if err != nil || p.Failed != c.want || (c.want == "" && p.Result == nil) || api.ops() != c.wantOps {
				t.Fatalf("%s: %+v %v ops %s", name, p, err, api.ops())
			}
		}
		// An unanswered lookup is retried.
		api := &fakeAPI{begin: func(int) (contract.PublicationBeginResponse, error) {
			return contract.PublicationBeginResponse{}, unavailable
		}}
		api.observe = func(n int, _ string) (contract.PublicationStatus, error) {
			if n == 0 {
				return contract.PublicationStatus{}, unavailable
			}
			return contract.PublicationStatus{WithID: true, Phase: contract.PubPhaseNone, TaskState: contract.TaskRunning}, nil
		}
		f := newPubFixture(t, tp, api)
		bctx, cancel := context.WithCancel(bg)
		cancel()
		f.in.Begin = bctx
		ch := run(f.in, bg)
		f.advanceRetry(t)
		if r := await(t, ch); r.err != nil || r.p.Failed != contract.PubErrPublicationTimeout {
			t.Fatalf("retried lookup %+v %v", r.p, r.err)
		}
	})
	t.Run("local-failure-after-begin", func(t *testing.T) {
		api := &fakeAPI{}
		f := newPubFixture(t, tp, api)
		f.in.RoleID = "bad\nrole"
		p, err := Publish(bg, f.in)
		if err != nil || p.Result == nil || api.ops() != "begin,finish:workspace_snapshot_failed" || f.phases() != "settled" {
			t.Fatalf("%+v %v ops %s phases %s", p, err, api.ops(), f.phases())
		}
	})
	t.Run("push-refusals", func(t *testing.T) {
		for err, want := range map[error]string{
			contract.TransferError(contract.CodeConflict, contract.ReasonTaskRefExists, "exists"):             contract.PubErrTaskRefExists,
			contract.TaskError(contract.CodeInvalidArgument, "", contract.PubErrMetadataOverflow, "overflow"): contract.PubErrMetadataOverflow,
			conflict(contract.ReasonTaskPublicationClosed):                                                    contract.PubErrPublicationClosed,
		} {
			pusher := &fakePusher{refs: map[string]plumbing.Hash{}, pushErr: err}
			api := &fakeAPI{pusher: func() (Pusher, error) { return pusher, nil }}
			f := newPubFixture(t, tp, api)
			p, perr := Publish(bg, f.in)
			if perr != nil || p.Result == nil || api.ops() != "begin,pusher,finish:"+want {
				t.Fatalf("%v: %+v %v ops %s", err, p, perr, api.ops())
			}
		}
		// The session cannot be opened: a refusal too.
		api := &fakeAPI{pusher: func() (Pusher, error) { return nil, conflict(contract.ReasonTaskAssignmentMismatch) }}
		f := newPubFixture(t, tp, api)
		if p, err := Publish(bg, f.in); err != nil || p.Result == nil || api.ops() != "begin,pusher,finish:task_assignment_mismatch" {
			t.Fatalf("pusher refused: %v ops %s", err, api.ops())
		}
	})
	t.Run("push-ambiguous", func(t *testing.T) {
		for name, pusher := range map[string]*fakePusher{
			"lost-report":   {refs: map[string]plumbing.Hash{}, pushErr: unavailable},
			"own-ref":       {refs: map[string]plumbing.Hash{contract.TaskRefPrefix + taskID(1): plumbing.NewHash(strings.Repeat("1", 40))}},
			"advertisement": {refsErr: unavailable},
		} {
			api := &fakeAPI{pusher: func() (Pusher, error) { return pusher, nil }}
			api.observe = func(n int, _ string) (contract.PublicationStatus, error) {
				if n == 0 {
					return contract.PublicationStatus{Phase: contract.PubPhaseAuthorized, TaskState: contract.TaskRunning}, nil
				}
				return api.settled, nil
			}
			f := newPubFixture(t, tp, api)
			ch := run(f.in, bg)
			f.advanceRetry(t)
			if r := await(t, ch); r.err != nil || r.p.Result == nil || api.ops() != "begin,pusher,observe,observe" {
				t.Fatalf("%s: %+v %v ops %s", name, r.p, r.err, api.ops())
			}
		}
	})
	t.Run("finish-ambiguous", func(t *testing.T) {
		pusher := &fakePusher{refs: map[string]plumbing.Hash{}, pushErr: conflict(contract.ReasonTaskPublicationClosed)}
		api := &fakeAPI{pusher: func() (Pusher, error) { return pusher, nil }}
		api.finish = func(n int, _ string) (contract.PublicationStatus, error) {
			if n == 0 {
				return contract.PublicationStatus{}, unavailable
			}
			return api.settled, nil
		}
		api.observe = func(int, string) (contract.PublicationStatus, error) {
			return contract.PublicationStatus{}, unavailable
		}
		f := newPubFixture(t, tp, api)
		ch := run(f.in, bg)
		f.advanceRetry(t)
		if r := await(t, ch); r.err != nil || r.p.Result == nil {
			t.Fatalf("%+v %v ops %s", r.p, r.err, api.ops())
		}
	})
	t.Run("checkpoint-failures", func(t *testing.T) {
		api := &fakeAPI{}
		f := newPubFixture(t, tp, api)
		boom := errors.New("journal write failed (fixture)")
		save := f.in.Save
		f.in.Save = func(c contract.PublicationCheckpoint) error {
			if c.Phase == contract.CheckpointAuthorized {
				return boom
			}
			return save(c)
		}
		if _, err := Publish(bg, f.in); !errors.Is(err, boom) || api.ops() != "begin" {
			t.Fatalf("authorized save: %v ops %s", err, api.ops())
		}
		for _, fail := range []string{"pushed", contract.CheckpointSettled} {
			api := &fakeAPI{}
			f := newPubFixture(t, tp, api)
			f.in.Save = func(c contract.PublicationCheckpoint) error {
				if (fail == "pushed" && c.Pushed) || c.Phase == fail {
					return boom
				}
				return save(c)
			}
			if _, err := Publish(bg, f.in); !errors.Is(err, boom) {
				t.Fatalf("%s save: %v", fail, err)
			}
		}
		// A settlement naming another execution is never adopted.
		api = &fakeAPI{}
		f = newPubFixture(t, tp, api)
		other := *api.settled.Result
		other.Execution.Attachment = 9
		api.settled.Result = &other
		if _, err := Publish(bg, f.in); err == nil {
			t.Fatal("another execution's settlement was adopted")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		// A shutdown during a retry wait leaves the outcome unresolved.
		api := &fakeAPI{observe: func(int, string) (contract.PublicationStatus, error) {
			return contract.PublicationStatus{}, unavailable
		}}
		f := newPubFixture(t, tp, api)
		ctx, cancel := context.WithCancel(bg)
		ch := run(f.in, ctx)
		awaitStageBy(t, f.sch, "retry-armed", time.After(testWait))
		cancel()
		if r := await(t, ch); !errors.Is(r.err, context.Canceled) || f.phases() != "authorized,authorized+pushed" {
			t.Fatalf("%v phases %s", r.err, f.phases())
		}
		// UT-B8: a receive peer that stalls mid-pack. Cancellation ends the
		// push; Publish returns only after its pack writer is joined, with
		// the pushed checkpoint kept for observation.
		sp := &stalledPusher{parked: make(chan struct{}, 1)}
		api3 := &fakeAPI{pusher: func() (Pusher, error) { return sp, nil }}
		f3 := newPubFixture(t, tp, api3)
		sctx, scancel := context.WithCancel(bg)
		sch := run(f3.in, sctx)
		within(t, sp.parked, "the push reaching the stalled peer")
		scancel()
		if r := await(t, sch); !errors.Is(r.err, context.Canceled) || f3.phases() != "authorized,authorized+pushed" {
			t.Fatalf("stalled push: %v phases %s", r.err, f3.phases())
		}
		// A cancelled worker never begins.
		api2 := &fakeAPI{begin: func(int) (contract.PublicationBeginResponse, error) {
			return contract.PublicationBeginResponse{}, context.Canceled
		}}
		f2 := newPubFixture(t, tp, api2)
		cctx, ccancel := context.WithCancel(bg)
		ccancel()
		if _, err := Publish(cctx, f2.in); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func strPtr(s string) *string { return &s }
