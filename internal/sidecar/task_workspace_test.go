package sidecar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// fakeWS is an in-memory workspace plane for the session tests: a base
// served through the node fetch, and a publication API that settles what
// was pushed. Gates and failures are scriptable.
type fakeWS struct {
	mu        sync.Mutex
	src       *memory.Storage
	base      plumbing.Hash
	gate      chan struct{}
	entered   chan struct{}
	fetchErr  error
	beginErr  error
	observeOK bool
	// observeCode is the settlement's failure code ("" settles published).
	observeCode string
	begins      int
	pushes      int
	observes    int
	cand        *contract.TaskResultBody
	state       string
	pushed      plumbing.Hash
	binding     contract.WorkspaceBinding
}

const wsPubID = "0123456789abcdef0123456789abcdef"

func newFakeWS(t *testing.T) *fakeWS {
	t.Helper()
	s := testkit.NewMemoryStore()
	c, err := testkit.CommitFiles(s, map[string]testkit.FileSpec{"README.md": {Mode: filemode.Regular, Content: []byte("# base\n")}}, nil, "base")
	if err != nil {
		t.Fatal(err)
	}
	b := c.String()
	return &fakeWS{src: s, base: c, observeOK: true, entered: make(chan struct{}, 8),
		binding: contract.WorkspaceBinding{Name: "proj", Instance: strings.Repeat("a", 32), BaseSelector: contract.DefaultBranchRef, BaseCommit: &b}}
}

func (f *fakeWS) Remote(string, contract.NodeAssignment) (taskworkspace.Remote, func(), error) {
	return wsRemote{f}, func() {}, nil
}

func (f *fakeWS) API(id string, a contract.NodeAssignment) (taskworkspace.PlaneAPI, error) {
	return wsAPI{f}, nil
}

type wsRemote struct{ f *fakeWS }

func (r wsRemote) UploadRefs(context.Context) (map[string]plumbing.Hash, error) {
	return map[string]plumbing.Hash{"refs/heads/base": r.f.base}, nil
}

func (r wsRemote) Fetch(ctx context.Context, wants, haves []plumbing.Hash, sink func(io.Reader) error) error {
	r.f.mu.Lock()
	gate, err := r.f.gate, r.f.fetchErr
	r.f.mu.Unlock()
	r.f.entered <- struct{}{}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	objs, err := revlist.Objects(r.f.src, wants, nil)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, r.f.src, false).Encode(objs, 0); err != nil {
		return err
	}
	return sink(&buf)
}

type wsAPI struct{ f *fakeWS }

func (a wsAPI) Begin(ctx context.Context, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if a.f.beginErr != nil {
		return contract.PublicationBeginResponse{}, a.f.beginErr
	}
	a.f.begins++
	c := req.Candidate
	a.f.cand = &c
	a.f.state = contract.TaskSucceeded
	if c.ExitCode == nil || *c.ExitCode != 0 {
		a.f.state = contract.TaskFailed
	}
	return contract.PublicationBeginResponse{PublicationID: wsPubID, State: a.f.state, ExpiresAt: "2026-09-26T12:05:00Z"}, nil
}

func (a wsAPI) settled(code string) contract.PublicationStatus {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	r := *a.f.cand
	w := contract.NewPublicationFailed(a.f.binding, code)
	if code == "" {
		c, ref := a.f.pushed.String(), contract.TaskRefPrefix+r.TaskID
		w = contract.TaskWorkspaceResult{Name: a.f.binding.Name, Instance: a.f.binding.Instance, BaseCommit: a.f.binding.BaseCommit,
			Publication: contract.PublicationPublished, Commit: &c, Ref: &ref, Diffstat: &contract.TaskDiffstat{}}
	}
	r.Workspace = &w
	r = r.Sealed()
	return contract.PublicationStatus{Phase: contract.PubPhaseSettled, Result: &r, TaskState: a.f.state, Committed: true}
}

func (a wsAPI) Finish(ctx context.Context, pubID, code string) (contract.PublicationStatus, error) {
	return a.settled(code), nil
}

func (a wsAPI) Observe(ctx context.Context, pubID string) (contract.PublicationStatus, error) {
	a.f.mu.Lock()
	a.f.observes++
	ok, code := a.f.observeOK, a.f.observeCode
	a.f.mu.Unlock()
	if !ok {
		return contract.PublicationStatus{}, contract.New(contract.CodeUnavailable, "plane unreachable (fixture)")
	}
	return a.settled(code), nil
}

func (a wsAPI) Pusher(pubID string) (taskworkspace.Pusher, error) { return wsPusher{a.f}, nil }

type wsPusher struct{ f *fakeWS }

func (p wsPusher) ReceiveRefs(context.Context) (map[string]plumbing.Hash, error) {
	return map[string]plumbing.Hash{"refs/heads/base": p.f.base}, nil
}

func (p wsPusher) PushTask(ctx context.Context, ref string, commit plumbing.Hash, pack io.Reader) error {
	s := memory.NewStorage()
	if err := packfile.UpdateObjectStorage(s, pack); err != nil {
		return err
	}
	p.f.mu.Lock()
	p.f.pushes++
	p.f.pushed = commit
	p.f.mu.Unlock()
	return nil
}

func (p wsPusher) Close() {}

// wsStart is a workspace task_start for task n on role cfg.
func wsStart(n int, cfg contract.RoleConfig, gen int, ws *fakeWS) contract.TaskStartBody {
	b := startBody(n, cfg, 1, gen, "workspace goal")
	name := ws.binding.Name
	b.Request.Workspace = &name
	bb := ws.binding
	b.Workspace = &bb
	return b
}

// prepared reads the next task_prepared request.
func (s *taskSession) prepared(t *testing.T) (string, contract.TaskPreparedBody) {
	t.Helper()
	f := s.request(t)
	if f.Type != contract.FrameTaskPrepared {
		t.Fatalf("got %s %s (%s), want a task_prepared", f.Type, f.RequestID, f.Body)
	}
	b, err := contract.DecodeTaskPrepared(f.Body)
	if err != nil {
		t.Fatal(err)
	}
	return f.RequestID, b
}

// ackPrepared answers a task_prepared with the release decision.
func (s *taskSession) ackPrepared(t *testing.T, rid string, b contract.TaskPreparedBody, release bool) {
	t.Helper()
	s.c.send(contract.ProtocolVersion, contract.FrameTaskPreparedAck, rid, contract.TaskPreparedAckBody{TaskID: b.TaskID, Execution: b.Execution, Release: release})
	s.tr.ev.awaitMatch(t, evPreparedAcked, func(ev event) bool { return ev.id == b.TaskID })
}

// wsRun starts a task run with ws as its workspace plane (and the extra
// deps adjustments).
func wsRun(t *testing.T, fp *fakePlane, ws *fakeWS, root string, extra ...func(d *deps)) *taskRun {
	t.Helper()
	return startTaskRun(t, fp, taskOpts{root: root, adjust: func(d *deps) {
		d.taskWorkspacePlane = ws
		for _, f := range extra {
			f(d)
		}
	}})
}

// wsSession is a connected run with role a and a workspace start sent and
// answered preparing.
func wsSession(t *testing.T, ws *fakeWS, extra ...func(d *deps)) (*taskRun, *taskSession, contract.TaskStartBody) {
	t.Helper()
	fp := startFakePlane(t)
	tr := wsRun(t, fp, ws, "", extra...)
	ins, run := manuals(t, tr.dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	s := tr.connect(t, 1, 1, cfg)
	st := wsStart(1, cfg, 1, ws)
	rid := s.nextP()
	s.c.sendStart(rid, st)
	if r := s.c.startResult(rid); r.Err != nil || !r.Preparing {
		t.Fatalf("start result %+v", r)
	}
	return tr, s, st
}

// awaitReportedBy is slowPrepare's preparation gate: it waits until
// reported is closed unless timeout fires first, and reports the failure
// as an error (it runs on the worker's goroutine, not the test's). At the
// timeout reported is rechecked before the failure is returned.
func awaitReportedBy(reported <-chan struct{}, timeout <-chan time.Time) error {
	select {
	case <-reported:
	case <-timeout:
		select {
		case <-reported:
		default:
			return errors.New("the reserved slot's report was not acknowledged")
		}
	}
	return nil
}

// TestPreparationGateAtBound is the C14 regression for slowPrepare's
// preparation gate (iteration 10b review r6): the report acknowledged and
// the gate's bound both ready must never record a gate failure, whichever
// the select takes; each attempt leaves both ready. With the report not
// acknowledged, the expired bound still records it.
func TestPreparationGateAtBound(t *testing.T) {
	t.Parallel()
	t.Run("both-ready", func(t *testing.T) {
		t.Parallel()
		for i := range 100 {
			reported := make(chan struct{})
			close(reported)
			if err := awaitReportedBy(reported, testkit.Fired()); err != nil {
				t.Fatalf("attempt %d: %v although the report was acknowledged", i+1, err)
			}
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		if awaitReportedBy(make(chan struct{}), testkit.Fired()) == nil {
			t.Fatal("the gate passed with the report not acknowledged")
		}
	})
}

// slowPrepare is TestTaskWorkspaceSession/slow-prepare: the base fetch
// outlasts the old 15 s start exchange; the start was answered preparing
// at once, heartbeats keep flowing on the armed clock, and exactly one
// child starts after the release. The clock moves only after the
// preparing reply's write returned: that write is bounded on the same
// clock, and its bytes reach the plane before it returns.
//
// With late set it is the C12 regression (iteration 10b review r5): the
// worker's preparation waits until the reserved slot's report was
// acknowledged and the periodic timer armed after it, so that arming
// precedes the preparing reply; and the reply's write is held after its
// bytes were delivered until the completion barrier. A clock move before
// the barrier would expire that write's bound and close the connection.
func slowPrepare(t *testing.T, late bool) {
	fp := startFakePlane(t)
	ws := newFakeWS(t)
	ws.gate = make(chan struct{})
	var hold *writeHold
	var extra []func(d *deps)
	reported := make(chan struct{})
	workRel := filepath.Join(taskDirRel(taskID(1)), workName)
	var gateMu sync.Mutex
	var gateErr error
	if late {
		hold = newWriteHold(func(f contract.NodeFrame) bool { return f.Type == contract.FrameTaskStartResult })
		extra = append(extra, hold.install(fp), func(d *deps) {
			d.fail = func(op, name string) error {
				if op != "mkdir" || name != workRel {
					return nil
				}
				if err := awaitReportedBy(reported, time.After(testWait)); err != nil {
					gateMu.Lock()
					gateErr = err
					gateMu.Unlock()
				}
				return nil
			}
		})
		t.Cleanup(hold.let)
	}
	tr := wsRun(t, fp, ws, "", extra...)
	ins, run := manuals(t, tr.dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	s := tr.connect(t, 1, 1, cfg)
	// The revision's first cycle completes before the start, so the
	// reserved slot's report is the last status change while the clock
	// stands still.
	start := tr.clk.Now()
	cyc := tr.ev.awaitCycleSince(t, 1, start)
	st := wsStart(1, cfg, 1, ws)
	if st.TaskID != taskID(1) {
		t.Fatalf("task %s, want %s", st.TaskID, taskID(1))
	}
	reserved := func(r contract.RoleStatus) bool { return r.Inflight == 1 && r.CanAccept == cyc.passed[0] }
	rid := s.nextP()
	s.c.sendStart(rid, st)
	if late {
		// The reserved slot's report, acknowledged, and the periodic timer
		// armed after it, all before the session can answer the start.
		s.report(t, 1, reserved)
		tr.ev.awaitHeartbeatArmed(t, s.c.minSession, s.b-1, start.Add(heartbeatInterval))
		close(reported)
	}
	if r := s.c.startResult(rid); !r.Preparing {
		t.Fatalf("start result %+v", r)
	}
	<-ws.entered
	// Arm before advancing: the preparation's own five-minute bound.
	tr.ev.awaitMatch(t, evPrepArmed, func(ev event) bool { return ev.id == st.TaskID })
	if !late {
		// The reserved slot's readiness report, acknowledged (late: awaited
		// above, before the reply).
		s.report(t, 1, reserved)
	} else {
		hold.wait(t)
		if !tr.clk.Now().Equal(start) {
			t.Fatalf("the clock moved before the barrier (%v)", tr.clk.Now())
		}
		hold.let()
	}
	// The completion barrier: the preparing reply's write returned. Then
	// each move waits for the periodic timer armed after the last
	// exchange.
	tr.ev.awaitStartReplied(t, st.TaskID)
	for i := 0; i < 4; i++ {
		s.periodicArmed(t, 1)
	}
	if tr.clk.Now().Sub(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)) <= prepBudget {
		t.Fatalf("the clock did not pass the old start bound (%v)", prepBudget)
	}
	tr.noChild(t)
	close(ws.gate)
	prid, pb := s.prepared(t)
	if !pb.OK {
		t.Fatalf("prepared %+v", pb)
	}
	s.ackPrepared(t, prid, pb, true)
	ch := tr.child(t)
	ch.exitCode(0)
	s.drain(t, st)
	tr.noChild(t)
	if late {
		hold.check(t)
		gateMu.Lock()
		defer gateMu.Unlock()
		if gateErr != nil {
			t.Fatal(gateErr)
		}
	}
}

// TestTaskWorkspaceSession is UT-B8 on the sidecar (iteration 10b): a
// workspace start answered preparing at once, its preparation (a slow
// fetch past the old start bound with heartbeats flowing), the prepared
// exchange and the release barrier, the result snapshot and publication,
// refusals, and the restart of a publication whose push was durable with
// its answer withheld. Its slow-prepare and restart subtests are
// delegated from tests/function (TestTaskWorkspaceAdmission/slow-prepare,
// TestTaskWorkspaceRecovery/crash-boundaries). Do not rename them.
func TestTaskWorkspaceSession(t *testing.T) {
	t.Run("publish", func(t *testing.T) {
		fp := startFakePlane(t)
		ws := newFakeWS(t)
		tr := wsRun(t, fp, ws, "")
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st := wsStart(1, cfg, 1, ws)
		rid := s.nextP()
		s.c.sendStart(rid, st)
		if r := s.c.startResult(rid); r.Err != nil || !r.Preparing {
			t.Fatalf("start result %+v", r)
		}
		prid, pb := s.prepared(t)
		if !pb.OK || pb.Error != nil || pb.StartDigest != st.StartDigestHex() {
			t.Fatalf("prepared %+v", pb)
		}
		tr.noChild(t)
		s.ackPrepared(t, prid, pb, true)
		ch := tr.child(t)
		// The child's cwd is the private checkout of the base.
		work := workDir(t, runtime.GOOS, tr.root, st.TaskID)
		if ch.spec.dir != work {
			t.Fatalf("cwd %s, want %s", ch.spec.dir, work)
		}
		if b, err := os.ReadFile(filepath.Join(work, "README.md")); err != nil || string(b) != "# base\n" {
			t.Fatalf("checkout %q %v", b, err)
		}
		os.WriteFile(filepath.Join(work, "new.txt"), []byte("n"), 0o644)
		ch.exitCode(0)
		r, _ := s.drain(t, st)
		w := r.Workspace
		if w == nil || w.Publication != contract.PublicationPublished || w.Commit == nil || *w.Commit != ws.pushed.String() || ws.begins != 1 || ws.pushes != 1 {
			t.Fatalf("result %+v (begins %d pushes %d)", w, ws.begins, ws.pushes)
		}
		tr.ev.awaitCollected(t, st.TaskID)
		eventually(t, "task directory removed", func() bool {
			_, err := os.Stat(filepath.Join(tr.root, journalDir, st.TaskID))
			return os.IsNotExist(err)
		})
	})
	t.Run("refused-preparation", func(t *testing.T) {
		fp := startFakePlane(t)
		ws := newFakeWS(t)
		ws.fetchErr = contract.TaskError(contract.CodeConflict, "", contract.ReasonWorkspaceBaseUnavailable, "pruned")
		tr := wsRun(t, fp, ws, "")
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st := wsStart(1, cfg, 1, ws)
		rid := s.nextP()
		s.c.sendStart(rid, st)
		if r := s.c.startResult(rid); !r.Preparing {
			t.Fatalf("start result %+v", r)
		}
		prid, pb := s.prepared(t)
		if pb.OK || pb.Error == nil || pb.Error.Details["reason"] != contract.ReasonWorkspaceBaseUnavailable {
			t.Fatalf("prepared %+v", pb)
		}
		s.ackPrepared(t, prid, pb, false)
		tr.noChild(t)
		tr.ev.awaitCollected(t, st.TaskID)
		eventually(t, "refused task directory removed", func() bool {
			_, err := os.Stat(filepath.Join(tr.root, journalDir, st.TaskID))
			return os.IsNotExist(err)
		})
	})
	t.Run("release-refused", func(t *testing.T) {
		fp := startFakePlane(t)
		ws := newFakeWS(t)
		tr := wsRun(t, fp, ws, "")
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st := wsStart(1, cfg, 1, ws)
		rid := s.nextP()
		s.c.sendStart(rid, st)
		s.c.startResult(rid)
		prid, pb := s.prepared(t)
		// The plane refuses (cancelled or expired): no adapter, cleanup.
		s.ackPrepared(t, prid, pb, false)
		tr.noChild(t)
		tr.ev.awaitCollected(t, st.TaskID)
		if tr.ledger.starts() != 0 {
			t.Fatal("a refused release started an adapter")
		}
	})
	t.Run("snapshot-failure", func(t *testing.T) {
		fp := startFakePlane(t)
		ws := newFakeWS(t)
		tr := wsRun(t, fp, ws, "")
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st := wsStart(1, cfg, 1, ws)
		rid := s.nextP()
		s.c.sendStart(rid, st)
		s.c.startResult(rid)
		prid, pb := s.prepared(t)
		s.ackPrepared(t, prid, pb, true)
		ch := tr.child(t)
		os.MkdirAll(filepath.Join(ch.spec.dir, "sub", ".git"), 0o755)
		ch.exitCode(3)
		r, _ := s.drain(t, st)
		if w := r.Workspace; w == nil || w.Publication != contract.PublicationFailed || w.Error == nil || *w.Error != contract.PubErrUnsupportedRepository || ws.begins != 0 {
			t.Fatalf("result %+v begins %d", r.Workspace, ws.begins)
		}
	})
	t.Run("slow-prepare", func(t *testing.T) { slowPrepare(t, false) })
	t.Run("slow-prepare-late-reply", func(t *testing.T) { slowPrepare(t, true) })
	t.Run("restart", func(t *testing.T) {
		// The push was durable but its settlement unobservable (the reply
		// withheld): Run stops with the authorized, pushed checkpoint. The
		// next Run only observes the settlement: no second Begin, push or
		// child, then the result and the cleanup.
		fp := startFakePlane(t)
		ws := newFakeWS(t)
		ws.observeOK = false
		tr := wsRun(t, fp, ws, "")
		root := tr.root
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st := wsStart(1, cfg, 1, ws)
		rid := s.nextP()
		s.c.sendStart(rid, st)
		s.c.startResult(rid)
		prid, pb := s.prepared(t)
		s.ackPrepared(t, prid, pb, true)
		ch := tr.child(t)
		ch.exitCode(0)
		tr.ev.awaitMatch(t, evPublication, func(ev event) bool { return ev.id == st.TaskID && ev.action == "observe" })
		cp, err := (layout{root: root}).readCheckpoint(st.TaskID)
		if err != nil || cp == nil || cp.Phase != contract.CheckpointAuthorized || !cp.Pushed {
			t.Fatalf("checkpoint %+v %v", cp, err)
		}
		tr.stopped(t)
		ws.mu.Lock()
		ws.observeOK = true
		ws.mu.Unlock()
		tr2 := wsRun(t, fp, ws, root)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("second run logs:\n%s", tr2.logs.String())
			}
		})
		s2, entries := tr2.reconnect(t, 2, 1, map[string]string{st.TaskID: contract.ActionContinue}, cfg)
		if len(entries) != 1 || entries[0].TaskID != st.TaskID {
			t.Fatalf("inventory %+v", entries)
		}
		r, _ := s2.drain(t, st)
		if r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || ws.begins != 1 || ws.pushes != 1 {
			t.Fatalf("restarted result %+v (begins %d pushes %d)", r.Workspace, ws.begins, ws.pushes)
		}
		tr2.noChild(t)
	})
	t.Run("restart-authorized", func(t *testing.T) {
		// Review r1 C4: Run stops between the authorized checkpoint and the
		// push checkpoint (that write keeps failing until the shutdown). The
		// next Run's recovered publication only observes the intent until
		// the plane settles it (here at its expiry, publication_timeout): no
		// second Begin, no push, no child.
		fp := startFakePlane(t)
		ws := newFakeWS(t)
		ws.observeCode = contract.PubErrPublicationTimeout
		var mu sync.Mutex
		writes := 0
		var rel string
		tr := wsRun(t, fp, ws, "", func(d *deps) {
			d.fail = func(op, name string) error {
				mu.Lock()
				defer mu.Unlock()
				if op != "rename" || rel == "" || name != rel {
					return nil
				}
				// sealed, authorized, then the push checkpoint fails.
				if writes++; writes >= 3 {
					return errors.New("injected push checkpoint failure")
				}
				return nil
			}
		})
		root := tr.root
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st := wsStart(1, cfg, 1, ws)
		mu.Lock()
		rel = filepath.Join(taskDirRel(st.TaskID), publicationName)
		mu.Unlock()
		rid := s.nextP()
		s.c.sendStart(rid, st)
		s.c.startResult(rid)
		prid, pb := s.prepared(t)
		s.ackPrepared(t, prid, pb, true)
		tr.child(t).exitCode(0)
		tr.ev.awaitMatch(t, evPublication, func(ev event) bool { return ev.id == st.TaskID && ev.action == "authorized" })
		eventually(t, "the failing push checkpoint", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return writes >= 3
		})
		tr.stopped(t)
		cp, err := (layout{root: root}).readCheckpoint(st.TaskID)
		if err != nil || cp == nil || cp.Phase != contract.CheckpointAuthorized || cp.Pushed || ws.pushes != 0 {
			t.Fatalf("checkpoint %+v %v (pushes %d)", cp, err, ws.pushes)
		}
		tr2 := wsRun(t, fp, ws, root)
		s2, entries := tr2.reconnect(t, 2, 1, map[string]string{st.TaskID: contract.ActionContinue}, cfg)
		if len(entries) != 1 || entries[0].TaskID != st.TaskID {
			t.Fatalf("inventory %+v", entries)
		}
		r, _ := s2.drain(t, st)
		ws.mu.Lock()
		begins, pushes, observes := ws.begins, ws.pushes, ws.observes
		ws.mu.Unlock()
		if r.Workspace == nil || r.Workspace.Publication != contract.PublicationFailed || r.Workspace.Error == nil ||
			*r.Workspace.Error != contract.PubErrPublicationTimeout || begins != 1 || pushes != 0 || observes == 0 {
			t.Fatalf("recovered result %+v (begins %d pushes %d observes %d)", r.Workspace, begins, pushes, observes)
		}
		tr2.noChild(t)
	})
}

// TestTaskWorkspaceLifecycle covers the remaining workspace execution
// paths on the injected harness (iteration 10b): an adapter that fails to
// launch after the release (lost: the start was durably authorized), a
// cancellation before the release (cancelled, not_started), a Begin
// refused by the plane (the ordinary result with a failed publication), a
// failing checkpoint write retried on its armed timer, and the production
// client adapters.
func TestTaskWorkspaceLifecycle(t *testing.T) {
	t.Run("lost-before-launch", func(t *testing.T) {
		ws := newFakeWS(t)
		tr, s, st := wsSession(t, ws)
		tr.ledger.mu.Lock()
		tr.ledger.adapterErr = errUnsupported
		tr.ledger.mu.Unlock()
		prid, pb := s.prepared(t)
		s.ackPrepared(t, prid, pb, true)
		r, _ := s.drain(t, st)
		if r.Outcome != contract.OutcomeLost || r.Workspace == nil || r.Workspace.Publication != contract.PublicationNotApplicable {
			t.Fatalf("result %+v", r)
		}
	})
	t.Run("cancel-before-release", func(t *testing.T) {
		ws := newFakeWS(t)
		ws.gate = make(chan struct{})
		defer close(ws.gate)
		tr, s, st := wsSession(t, ws)
		<-ws.entered
		in := stopOf("c")
		s.sendCancel(t, st, in)
		r, _ := s.drain(t, st)
		if r.Outcome != contract.OutcomeCancelled || r.StopID == nil || *r.StopID != in.ID || r.Workspace == nil ||
			r.Workspace.Publication != contract.PublicationNotStarted {
			t.Fatalf("result %+v", r)
		}
		tr.noChild(t)
	})
	t.Run("begin-refused", func(t *testing.T) {
		ws := newFakeWS(t)
		ws.beginErr = contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskPublicationClosed, "decided")
		tr, s, st := wsSession(t, ws)
		prid, pb := s.prepared(t)
		s.ackPrepared(t, prid, pb, true)
		tr.child(t).exitCode(0)
		r, _ := s.drain(t, st)
		if r.Workspace == nil || r.Workspace.Publication != contract.PublicationFailed || r.Workspace.Error == nil ||
			*r.Workspace.Error != contract.PubErrPublicationClosed || ws.pushes != 0 {
			t.Fatalf("result %+v", r.Workspace)
		}
	})
	t.Run("work-removal-held", func(t *testing.T) {
		// A failed work directory removal after a confirmed cleanup keeps
		// the slot held (cleanup pending) until the janitor's journal
		// removal deletes the whole task directory.
		ws := newFakeWS(t)
		fc := &failCounter{}
		tr, s, st := wsSession(t, ws, func(d *deps) { d.fail = fc.fail })
		sup := tr.super(t)
		fc.set("remove", filepath.Join(taskDirRel(st.TaskID), workName), 1)
		prid, pb := s.prepared(t)
		s.ackPrepared(t, prid, pb, true)
		tr.child(t).exitCode(0)
		tr.ev.awaitMatch(t, evWorkPending, func(ev event) bool { return ev.id == st.TaskID })
		if n, _ := sup.local(keyOf(st.Role)); n != 1 {
			t.Fatalf("%d slots occupied after the failed work removal, want 1 (held)", n)
		}
		if _, err := os.Stat(sup.jr.l.path(filepath.Join(taskDirRel(st.TaskID), workName))); err != nil {
			t.Fatalf("work directory: %v", err)
		}
		r, _ := s.drain(t, st)
		if r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished {
			t.Fatalf("result %+v", r.Workspace)
		}
		tr.ev.awaitMatch(t, evWorkReleased, func(ev event) bool { return ev.id == st.TaskID })
		if n, _ := sup.local(keyOf(st.Role)); n != 0 {
			t.Fatalf("%d slots occupied after the janitor removed the task directory", n)
		}
		if _, err := os.Lstat(sup.jr.l.path(taskDirRel(st.TaskID))); !os.IsNotExist(err) {
			t.Fatalf("task directory after the janitor: %v", err)
		}
	})
	t.Run("recovered-work-held", func(t *testing.T) {
		// Review r1 C3: a previous Run's ownerless journal (a preparation
		// interrupted by shutdown) still owns its work directory. The next
		// Run holds the slot for it, deletes the work only after the lost
		// outcome is durable, and a failed deletion keeps the slot held
		// until the janitor removes the whole task directory.
		ws := newFakeWS(t)
		ws.gate = make(chan struct{})
		tr, _, st := wsSession(t, ws)
		<-ws.entered
		tr.stopped(t)
		close(ws.gate)
		work := (layout{root: tr.root}).path(filepath.Join(taskDirRel(st.TaskID), workName))
		if _, err := os.Stat(work); err != nil {
			t.Fatalf("the interrupted preparation's work directory: %v", err)
		}
		fc := &failCounter{}
		fc.set("remove", filepath.Join(taskDirRel(st.TaskID), workName), 1)
		tr2 := wsRun(t, tr.fp, ws, tr.root, func(d *deps) { d.fail = fc.fail })
		sup := tr2.super(t)
		tr2.ev.awaitMatch(t, evWorkPending, func(ev event) bool { return ev.id == st.TaskID })
		if n, _ := sup.local(keyOf(st.Role)); n != 1 {
			t.Fatalf("%d slots occupied while the recovered work exists, want 1 (held)", n)
		}
		if _, err := os.Stat(work); err != nil {
			t.Fatalf("work directory after the failed deletion: %v", err)
		}
		ins, run := manuals(t, tr.dir, "a", "m")
		s2, entries := tr2.reconnect(t, 2, 1, map[string]string{st.TaskID: contract.ActionSendResult}, roleConfig("a", ins, run))
		if len(entries) != 1 || entries[0].TaskID != st.TaskID {
			t.Fatalf("inventory %+v", entries)
		}
		if r, _ := s2.drain(t, st); r.Outcome != contract.OutcomeLost || r.Workspace == nil || r.Workspace.Publication != contract.PublicationNotApplicable {
			t.Fatalf("recovered result %+v", r)
		}
		tr2.ev.awaitMatch(t, evWorkReleased, func(ev event) bool { return ev.id == st.TaskID })
		if n, _ := sup.local(keyOf(st.Role)); n != 0 {
			t.Fatalf("%d slots occupied after the janitor removed the task directory", n)
		}
		if _, err := os.Lstat(sup.jr.l.path(taskDirRel(st.TaskID))); !os.IsNotExist(err) {
			t.Fatalf("task directory after the janitor: %v", err)
		}
	})
	t.Run("recovered-work-released", func(t *testing.T) {
		// The same recovery without a fault: the work is deleted before the
		// slot is released (both before any acknowledgement).
		ws := newFakeWS(t)
		ws.gate = make(chan struct{})
		tr, _, st := wsSession(t, ws)
		<-ws.entered
		tr.stopped(t)
		close(ws.gate)
		work := (layout{root: tr.root}).path(filepath.Join(taskDirRel(st.TaskID), workName))
		tr2 := wsRun(t, tr.fp, ws, tr.root)
		sup := tr2.super(t)
		tr2.ev.awaitMatch(t, evWorkReleased, func(ev event) bool { return ev.id == st.TaskID })
		if _, err := os.Lstat(work); !os.IsNotExist(err) {
			t.Fatalf("the slot was released before the work was deleted: %v", err)
		}
		if n, _ := sup.local(keyOf(st.Role)); n != 0 {
			t.Fatalf("%d slots occupied after the recovered work was deleted", n)
		}
		if _, err := os.Stat(sup.jr.l.path(filepath.Join(taskDirRel(st.TaskID), executionName))); err != nil {
			t.Fatalf("the outbox journal must stay until its acknowledgement: %v", err)
		}
	})
	t.Run("checkpoint-retry", func(t *testing.T) {
		ws := newFakeWS(t)
		fc := &failCounter{}
		tr, s, st := wsSession(t, ws, func(d *deps) { d.fail = fc.fail })
		fc.set("rename", filepath.Join(taskDirRel(st.TaskID), publicationName), 1)
		prid, pb := s.prepared(t)
		s.ackPrepared(t, prid, pb, true)
		tr.child(t).exitCode(0)
		// The sealed checkpoint's first write fails: the worker blocks the
		// node and retries on its armed timer (armed after the failure).
		eventually(t, "the injected failure", func() bool {
			fc.mu.Lock()
			defer fc.mu.Unlock()
			return fc.hits == 1
		})
		eventually(t, "the node blocked", func() bool { return tr.super(t).nodeBlocked() })
		if err := tr.clk.AwaitWaiter(testWait, testkit.HasTimer(journalRetry)); err != nil {
			t.Fatal(err)
		}
		tr.clk.Advance(journalRetry)
		r, _ := s.drain(t, st)
		if r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || fc.hits != 1 {
			t.Fatalf("result %+v (%d injected failures)", r.Workspace, fc.hits)
		}
	})
	t.Run("client-adapters", func(t *testing.T) {
		fp := startFakePlane(t)
		c, err := client.New(fp.url, client.Trust{CAPEM: fp.caPEM})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		p := clientPlane{c}
		a := contract.NodeAssignment{NodeID: testID, Execution: contract.ExecutionToken{Epoch: taskEpoch, Attachment: 1}, StartDigest: strings.Repeat("d", 64),
			Instance: strings.Repeat("a", 32)}
		r, closeR, err := p.Remote(taskID(1), a)
		if err != nil || r == nil {
			t.Fatal(err)
		}
		closeR()
		if _, _, err := p.Remote("t_x", a); err == nil {
			t.Fatal("an invalid task opened a session")
		}
		api, err := p.API(taskID(1), a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.API("t_x", a); err == nil {
			t.Fatal("an invalid task opened the publication endpoints")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		zero := 0
		cand := contract.TaskResultBody{TaskID: taskID(1), Execution: a.Execution, Outcome: contract.OutcomeNatural, ExitCode: &zero}.Sealed()
		// The fake plane serves no publication endpoints: every call fails
		// cleanly (the adapters only forward).
		if _, err := api.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("1", 40)}); err == nil {
			t.Fatal("Begin succeeded against a plane without the endpoint")
		}
		if _, err := api.Finish(ctx, wsPubID, contract.PubErrStorageFailed); err == nil {
			t.Fatal("Finish succeeded")
		}
		if _, err := api.Observe(ctx, wsPubID); err == nil {
			t.Fatal("Observe succeeded")
		}
		pu, err := api.Pusher(wsPubID)
		if err != nil {
			t.Fatal(err)
		}
		pu.Close()
		if _, err := api.Pusher("x"); err == nil {
			t.Fatal("an invalid publication opened a receive session")
		}
	})
}

// TestTaskStorage covers the exported production task storage (iteration
// 10b, review r1 C7: the publication benchmark's node persistence): the
// published task directory, its checkpoint and frozen outbox journal read
// back by the supervisor's own strict loaders, and the removal of the
// whole directory, work and trusted objects included.
func TestTaskStorage(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ins, run := manuals(t, t.TempDir(), "a", "m")
	st := startBody(1, roleConfig("a", ins, run), 1, 1, "goal")
	lookup := adapter.ContractLookup(newScript().registry())
	ts := NewTaskStorage(root)
	j := journalOf(st, contract.JournalPrepared)
	j.Work = true
	if err := ts.Create(j); err != nil {
		t.Fatal(err)
	}
	dir := ts.Dir(st.TaskID)
	if dir != (layout{root: root}).path(taskDirRel(st.TaskID)) {
		t.Fatalf("task directory %s", dir)
	}
	for _, d := range []string{workName, objectsName} {
		if err := os.MkdirAll(filepath.Join(dir, d, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, d, "sub", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	zero := 0
	cand := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &zero}.Sealed()
	cp := contract.PublicationCheckpoint{TaskID: st.TaskID, Execution: st.Execution, Phase: contract.CheckpointSealed, Candidate: cand, Tree: strings.Repeat("1", 40)}
	if err := ts.SaveCheckpoint(cp); err != nil {
		t.Fatal(err)
	}
	if got, err := (layout{root: root}).readCheckpoint(st.TaskID); err != nil || got == nil || got.Phase != contract.CheckpointSealed || got.Tree != cp.Tree {
		t.Fatalf("checkpoint %+v %v", got, err)
	}
	done := journalOf(st, contract.JournalCompleted)
	done.Work, done.Result = true, &cand
	if err := ts.Write(done); err != nil {
		t.Fatal(err)
	}
	lj, err := (layout{root: root}).loadJournal(st.TaskID, lookup)
	if err != nil || lj.j.Phase != contract.JournalCompleted || lj.j.Result == nil || lj.j.Result.Digest != cand.Digest {
		t.Fatalf("outbox journal %+v %v", lj.j, err)
	}
	if ids, err := (layout{root: root}).scanJournals(); err != nil || len(ids) != 1 || ids[0] != st.TaskID {
		t.Fatalf("scan %v %v", ids, err)
	}
	if err := ts.Remove(st.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("task directory after removal: %v", err)
	}
	// A failed write leaves nothing published (an invalid document).
	if err := ts.Write(contract.ExecutionJournal{TaskID: st.TaskID}); err == nil {
		t.Fatal("an invalid journal was written")
	}
	if err := ts.SaveCheckpoint(contract.PublicationCheckpoint{TaskID: st.TaskID}); err == nil {
		t.Fatal("an invalid checkpoint was written")
	}
}

// eventually polls cond (bounded), rechecking it before the verdict.
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
