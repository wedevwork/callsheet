package workspace

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// fakeGuard is a scriptable TaskGuard.
type fakeGuard struct {
	mu        sync.Mutex
	acc       TaskAccess
	upErr     error
	rvErr     error
	pub       string
	commit    plumbing.Hash
	published func(md TaskMetadata) error
	done      int
	md        *TaskMetadata
}

func (g *fakeGuard) Upload(ctx context.Context, id string, a contract.NodeAssignment) (TaskAccess, error) {
	if g.upErr != nil {
		return TaskAccess{}, g.upErr
	}
	return g.acc, nil
}

func (g *fakeGuard) Receive(ctx context.Context, id string, a contract.NodeAssignment, pub string) (*TaskReceive, error) {
	if g.rvErr != nil {
		return nil, g.rvErr
	}
	if pub != g.pub {
		return nil, TaskConflict(contract.ReasonTaskPublicationClosed)
	}
	return &TaskReceive{Access: g.acc, Commit: g.commit, PubID: pub, Deadline: time.Now().Add(time.Minute),
		Published: func(ctx context.Context, at time.Time, md TaskMetadata) error {
			g.mu.Lock()
			g.md = &md
			f := g.published
			g.mu.Unlock()
			if f != nil {
				return f(md)
			}
			return nil
		},
		Done: func() { g.mu.Lock(); g.done++; g.mu.Unlock() }}, nil
}

const routeTask = "t_0123456789abcdef0123456789abcdef"

func taskAssignment(inst string) http.Header {
	h := http.Header{}
	a := contract.NodeAssignment{NodeID: "n_" + strings.Repeat("1", 32), Execution: contract.ExecutionToken{Epoch: strings.Repeat("e", 32), Attachment: 1},
		StartDigest: strings.Repeat("d", 64), Instance: inst}
	for k, v := range a.Headers() {
		h.Set(k, v)
	}
	return h
}

// serveTask calls ServeTaskGit in process.
func serveTask(m *Manager, g TaskGuard, method, target string, h http.Header, body []byte) rpc {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	m.ServeTaskGit(rec, req, g)
	res := rpc{status: rec.Code, body: rec.Body.String()}
	if rec.Code == http.StatusOK && strings.HasSuffix(target, routeReceive) {
		rs := packp.NewReportStatus()
		if rs.Decode(bytes.NewReader(rec.Body.Bytes())) == nil {
			res.report = rs
		}
	}
	return res
}

func taskPath(route string) string { return contract.PathNodeWorkspaces + routeTask + ".git/" + route }

// uploadBody is a raw upload request for wants and haves.
func uploadBody(t *testing.T, wants, haves []plumbing.Hash, caps ...capability.Capability) []byte {
	t.Helper()
	req := packp.NewUploadPackRequest()
	req.Wants = wants
	for _, c := range caps {
		req.Capabilities.Set(c)
	}
	var buf bytes.Buffer
	if err := req.UploadRequest.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	enc := pktline.NewEncoder(&buf)
	for _, h := range haves {
		enc.Encodef("have %s\n", h)
	}
	enc.Encodef("done\n")
	return buf.Bytes()
}

// TestTaskRoutes is UT-B2 on the hub (iteration 10b): the node transfer
// routes' framing and header checks before any lookup, the guard's
// refusal mapping, the bound base only, the create-once receive of the
// own task ref with provenance and recomputed metadata, base resolution,
// provenance reads and the publication fences. Delegated from
// tests/function (TestTaskWorkspaceAccess/routes). Do not rename it.
func TestTaskRoutes(t *testing.T) {
	f := newTransportFixture(t)
	acc := TaskAccess{Name: "alpha", Instance: f.inst, Base: f.c0, TaskRef: contract.TaskRefPrefix + routeTask}
	hdr := taskAssignment(f.inst)
	t.Run("framing", func(t *testing.T) {
		g := &fakeGuard{acc: acc}
		for name, c := range map[string]struct {
			method, target string
			h              http.Header
			status         int
		}{
			"bad-task":       {http.MethodGet, contract.PathNodeWorkspaces + "t_x.git/info/refs?service=git-upload-pack", hdr, 400},
			"no-headers":     {http.MethodGet, taskPath("info/refs?service=git-upload-pack"), http.Header{}, 400},
			"unknown-route":  {http.MethodGet, taskPath("objects/x"), hdr, 404},
			"post-info":      {http.MethodPost, taskPath("info/refs?service=git-upload-pack"), hdr, 405},
			"get-upload":     {http.MethodGet, taskPath("git-upload-pack"), hdr, 405},
			"bad-service":    {http.MethodGet, taskPath("info/refs?service=other"), hdr, 400},
			"query-on-post":  {http.MethodPost, taskPath("git-upload-pack?x=1"), hdr, 400},
			"encoded":        {http.MethodGet, contract.PathNodeWorkspaces + "t_0123456789abcdef0123456789abcde%66.git/info/refs?service=git-upload-pack", hdr, 400},
			"receive-no-pub": {http.MethodPost, taskPath("git-receive-pack"), hdr, 400},
		} {
			if r := serveTask(f.m, g, c.method, c.target, c.h, nil); r.status != c.status {
				t.Fatalf("%s: %s, want %d", name, r, c.status)
			}
		}
		req := httptest.NewRequest(http.MethodGet, taskPath("info/refs?service=git-upload-pack"), nil)
		req.ProtoMajor = 2
		rec := httptest.NewRecorder()
		f.m.ServeTaskGit(rec, req, g)
		if rec.Code != http.StatusHTTPVersionNotSupported {
			t.Fatalf("HTTP/2: %d", rec.Code)
		}
	})
	t.Run("guard-mapping", func(t *testing.T) {
		for err, want := range map[error]string{
			contract.TaskError(contract.CodeNotFound, "", "", "x"):        "404 not_found",
			TaskConflict(contract.ReasonTaskAssignmentMismatch):           "409 task_assignment_mismatch",
			TaskConflict(contract.ReasonTaskPublicationClosed):            "409 task_publication_closed",
			contract.TaskError(contract.CodeConflict, "", "other", "x"):   "409 workspace_instance_mismatch",
			contract.TaskError(contract.CodeInvalidArgument, "", "", "x"): "400 invalid_argument",
			contract.TaskError(contract.CodeUnavailable, "", "", "x"):     "503 unavailable",
			context.Canceled:   "503 unavailable",
			errors.New("boom"): "500 internal",
		} {
			g := &fakeGuard{upErr: err}
			r := serveTask(f.m, g, http.MethodGet, taskPath("info/refs?service=git-upload-pack"), hdr, nil)
			if got := strings.TrimSpace(strings.Fields(r.String())[1] + " " + strings.Trim(strings.TrimSpace(r.body), `"`)); !strings.HasPrefix(got, strings.Fields(want)[0]) || !strings.Contains(r.body, strings.Fields(want)[1]) {
				t.Fatalf("%v: %s, want %s", err, r, want)
			}
		}
	})
	t.Run("upload", func(t *testing.T) {
		g := &fakeGuard{acc: acc}
		r := serveTask(f.m, g, http.MethodGet, taskPath("info/refs?service=git-upload-pack"), hdr, nil)
		if r.status != 200 || !strings.Contains(r.body, f.c0.String()+" "+TaskBaseRef) || strings.Contains(r.body, "refs/heads/main") {
			t.Fatalf("advertisement %s", r)
		}
		// The bound base only, its full closure.
		if r := serveTask(f.m, g, http.MethodPost, taskPath("git-upload-pack"), hdr, uploadBody(t, []plumbing.Hash{f.c0}, nil, capability.OFSDelta)); r.status != 200 || !strings.Contains(r.body, "PACK") {
			t.Fatalf("base upload %s", r)
		}
		// A have of the base itself: the round trip still happens (an
		// acknowledgement and an empty pack).
		if r := serveTask(f.m, g, http.MethodPost, taskPath("git-upload-pack"), hdr, uploadBody(t, []plumbing.Hash{f.c0}, []plumbing.Hash{f.c0})); r.status != 200 || !strings.Contains(r.body, "ACK "+f.c0.String()) {
			t.Fatalf("cached base %s", r)
		}
		for name, body := range map[string][]byte{
			"other-want":  uploadBody(t, []plumbing.Hash{f.c1}, nil),
			"two-wants":   uploadBody(t, []plumbing.Hash{f.c0, f.c1}, nil),
			"bad-cap":     uploadBody(t, []plumbing.Hash{f.c0}, nil, capability.ThinPack),
			"garbage":     []byte("0010not a request"),
			"malformed-h": append(uploadBody(t, []plumbing.Hash{f.c0}, nil)[:0:0], []byte("0032want "+f.c0.String()+"\n00000009have x\n0009done\n")...),
		} {
			if r := serveTask(f.m, g, http.MethodPost, taskPath("git-upload-pack"), hdr, body); r.status != 400 {
				t.Fatalf("%s: %s", name, r)
			}
		}
		// The empty base advertises nothing and serves no upload.
		empty := &fakeGuard{acc: TaskAccess{Name: "alpha", Instance: f.inst, TaskRef: acc.TaskRef}}
		if r := serveTask(f.m, empty, http.MethodGet, taskPath("info/refs?service=git-upload-pack"), hdr, nil); r.status != 200 || strings.Contains(r.body, TaskBaseRef) {
			t.Fatalf("empty advertisement %s", r)
		}
		if r := serveTask(f.m, empty, http.MethodPost, taskPath("git-upload-pack"), hdr, uploadBody(t, []plumbing.Hash{f.c0}, nil)); r.status != 400 {
			t.Fatalf("empty upload %s", r)
		}
		// A base no longer reachable from a current ref, a changed
		// instance and a missing workspace refuse.
		gone := &fakeGuard{acc: TaskAccess{Name: "alpha", Instance: f.inst, Base: f.c1, TaskRef: acc.TaskRef}}
		if r := serveTask(f.m, gone, http.MethodGet, taskPath("info/refs?service=git-upload-pack"), hdr, nil); r.status != 409 || !strings.Contains(r.body, contract.ReasonWorkspaceBaseUnavailable) {
			t.Fatalf("unreachable base %s", r)
		}
		other := &fakeGuard{acc: TaskAccess{Name: "alpha", Instance: strings.Repeat("9", 32), Base: f.c0, TaskRef: acc.TaskRef}}
		if r := serveTask(f.m, other, http.MethodPost, taskPath("git-upload-pack"), hdr, uploadBody(t, []plumbing.Hash{f.c0}, nil)); r.status != 409 {
			t.Fatalf("other instance %s", r)
		}
		missing := &fakeGuard{acc: TaskAccess{Name: "nosuch", Instance: f.inst, Base: f.c0, TaskRef: acc.TaskRef}}
		if r := serveTask(f.m, missing, http.MethodGet, taskPath("info/refs?service=git-upload-pack"), hdr, nil); r.status/100 != 4 {
			t.Fatalf("missing workspace %s", r)
		}
	})
	t.Run("receive", func(t *testing.T) {
		files := seedFiles()
		files["result.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("result\n")}
		result := commit(t, f.local, files, "result", f.c0)
		pub := strings.Repeat("a", 32)
		g := &fakeGuard{acc: acc, pub: pub, commit: result}
		ph := hdr.Clone()
		ph.Set(contract.PublicationIDHeader, pub)
		post := func(g *fakeGuard, cmds []*packp.Command, pack []byte, caps ...capability.Capability) rpc {
			if caps == nil {
				caps = []capability.Capability{capability.ReportStatus}
			}
			return serveTask(f.m, g, http.MethodPost, taskPath("git-receive-pack"), ph, encodeReceive(t, cmds, caps, pack))
		}
		own := contract.TaskRefPrefix + routeTask
		pack := f.pack(t, result, f.c0)
		// The receive advertisement: the base only (no own ref yet).
		if r := serveTask(f.m, g, http.MethodGet, taskPath("info/refs?service=git-receive-pack"), hdr, nil); r.status != 200 || !strings.Contains(r.body, TaskBaseRef) || strings.Contains(r.body, own) {
			t.Fatalf("receive advertisement %s", r)
		}
		for name, c := range map[string]struct {
			cmds []*packp.Command
			caps []capability.Capability
		}{
			"two-commands": {append(cmd(own, plumbing.ZeroHash, result), cmd("refs/heads/x", plumbing.ZeroHash, result)...), nil},
			"update":       {cmd(own, f.c0, result), nil},
			"delete":       {cmd(own, f.c0, plumbing.ZeroHash), nil},
			"branch":       {cmd("refs/heads/main", plumbing.ZeroHash, result), nil},
			"other-task":   {cmd(contract.TaskRefPrefix+"t_"+strings.Repeat("f", 32), plumbing.ZeroHash, result), nil},
			"no-report":    {cmd(own, plumbing.ZeroHash, result), []capability.Capability{capability.OFSDelta}},
			"bad-cap":      {cmd(own, plumbing.ZeroHash, result), []capability.Capability{capability.ReportStatus, capability.DeleteRefs}},
		} {
			if r := post(g, c.cmds, pack, c.caps...); r.status != 400 {
				t.Fatalf("%s: %s", name, r)
			}
		}
		if r := post(&fakeGuard{acc: acc, pub: "x", commit: result}, cmd(own, plumbing.ZeroHash, result), pack); r.status != 409 || !strings.Contains(r.body, contract.ReasonTaskPublicationClosed) {
			t.Fatalf("closed intent %s", r)
		}
		// A commit other than the intent's: a non-ok report, nothing staged.
		wrong := commit(t, f.local, seedFiles(), "wrong", f.c0)
		if r := post(g, cmd(own, plumbing.ZeroHash, wrong), f.pack(t, wrong, f.c0)); r.status != 200 || r.ok() {
			t.Fatalf("wrong commit %s", r)
		}
		// An incomplete pack fails its closure.
		if r := post(g, cmd(own, plumbing.ZeroHash, result), packOf(t, f.local, result)); r.ok() {
			t.Fatalf("incomplete pack accepted: %s", r)
		}
		// The terminal record fails after the commit point: no success
		// report and the fence stays; then the ref exists exactly once.
		g.published = func(TaskMetadata) error { return errors.New("terminal record write failed") }
		if r := post(g, cmd(own, plumbing.ZeroHash, result), pack); r.ok() || !f.m.Fenced("alpha") {
			t.Fatalf("unconfirmed terminal: %s fenced %v", r, f.m.Fenced("alpha"))
		}
		f.m.ReleasePublication("alpha", routeTask, pub)
		st, err := f.m.TaskRef(context.Background(), "alpha", f.inst, routeTask)
		if err != nil || !st.Exists || st.Commit != result.String() || st.PublicationID != pub || st.PublishedAt.IsZero() {
			t.Fatalf("provenance %+v %v", st, err)
		}
		if g.md == nil || g.md.Diffstat.Added != 1 || len(g.md.Rows) != 1 || string(g.md.Rows[0].Path) != "result.txt" {
			t.Fatalf("recomputed metadata %+v", g.md)
		}
		// Create-once: a retransmission conflicts and the ref is never
		// updated; the advertisement now names it.
		g.published = nil
		if r := post(g, cmd(own, plumbing.ZeroHash, result), pack); r.status != 409 || !strings.Contains(r.body, contract.ReasonTaskRefExists) {
			t.Fatalf("second create %s", r)
		}
		if r := serveTask(f.m, g, http.MethodGet, taskPath("info/refs?service=git-receive-pack"), hdr, nil); !strings.Contains(r.body, result.String()+" "+own) {
			t.Fatalf("advertisement after create %s", r)
		}
		if g.done == 0 {
			t.Fatal("the guard's operation was never released")
		}
		// Metadata from the hub (recovery) agrees.
		md, err := f.m.TaskDiff(context.Background(), "alpha", f.inst, f.c0, result)
		if err != nil || md.Diffstat != g.md.Diffstat {
			t.Fatalf("TaskDiff %+v %v", md, err)
		}
		if _, err := f.m.TaskDiff(context.Background(), "alpha", f.inst, f.c0, f.c1); err == nil {
			t.Fatal("TaskDiff of an absent commit succeeded")
		}
		if st, _ := f.m.TaskRef(context.Background(), "alpha", f.inst, "t_"+strings.Repeat("0", 32)); st.Exists {
			t.Fatal("an absent task ref exists")
		}
		if st, err := f.m.TaskRef(context.Background(), "alpha", strings.Repeat("9", 32), routeTask); err != nil || st.Exists {
			t.Fatalf("another instance's ref %+v %v", st, err)
		}
	})
	t.Run("resolve", func(t *testing.T) {
		m, _ := newManager(t)
		v := mustCreate(t, m, "beta")
		local := testkit.NewMemoryStore()
		c0 := commit(t, local, seedFiles(), "c0")
		g := serveGit(t, m)
		ctx := context.Background()
		sel := func(s *string) contract.Selector {
			x, err := contract.ParseTaskBase(s)
			if err != nil {
				t.Fatal(err)
			}
			return x
		}
		// An unborn main is not found, never an implicit empty base.
		if _, err := m.ResolveTaskBase(ctx, "beta", "", sel(nil)); codeOf(err) != contract.CodeNotFound {
			t.Fatalf("unborn main: %v", err)
		}
		push(t, g.remote("beta", v.Instance), local, "refs/heads/main", plumbing.ZeroHash, c0)
		b, err := m.ResolveTaskBase(ctx, "beta", v.Instance, sel(nil))
		if err != nil || b.BaseSelector != contract.DefaultBranchRef || *b.BaseCommit != c0.String() || b.Instance != v.Instance || b.Validate() != nil {
			t.Fatalf("main %+v %v", b, err)
		}
		e, err := m.ResolveTaskBase(ctx, "beta", "", sel(strp("empty")))
		if err != nil || e.BaseCommit != nil || e.BaseSelector != contract.SelectorEmpty {
			t.Fatalf("empty %+v %v", e, err)
		}
		h, err := m.ResolveTaskBase(ctx, "beta", "", sel(strp(c0.String())))
		if err != nil || *h.BaseCommit != c0.String() {
			t.Fatalf("hash %+v %v", h, err)
		}
		for name, c := range map[string]struct {
			name, inst string
			s          contract.Selector
			code       contract.Code
		}{
			"missing-branch": {"beta", "", sel(strp("nope")), contract.CodeNotFound},
			"absent-commit":  {"beta", "", sel(strp(strings.Repeat("a", 40))), contract.CodeNotFound},
			"absent-task":    {"beta", "", sel(strp("t_" + strings.Repeat("b", 32))), contract.CodeNotFound},
			"instance":       {"beta", strings.Repeat("9", 32), sel(nil), contract.CodeConflict},
			"workspace":      {"gamma", "", sel(nil), contract.CodeNotFound},
			"bad-name":       {"Bad", "", sel(nil), contract.CodeInvalidArgument},
		} {
			if _, err := m.ResolveTaskBase(ctx, c.name, c.inst, c.s); codeOf(err) != c.code {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("fences", func(t *testing.T) {
		m, _ := newManager(t)
		v := mustCreate(t, m, "delta")
		release := m.FencePublication("delta", routeTask, strings.Repeat("b", 32))
		if !m.Fenced("delta") || m.Fenced("other") {
			t.Fatal("fence state")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if _, err := m.Remove(ctx, "delta", v.Instance); err == nil {
			t.Fatal("rm passed a fence")
		}
		done := make(chan error, 1)
		go func() {
			_, err := m.Prune(context.Background(), "delta", v.Instance, time.Now())
			done <- err
		}()
		testkit.AbsentFor(t, done, time.After(20*time.Millisecond), "prune passed a fence")
		release()
		release()
		if err := awaitResult(t, done, "prune after the fence"); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Remove(context.Background(), "delta", v.Instance); err != nil {
			t.Fatal(err)
		}
	})
}

// awaitResult receives a queued operation's result within testWait. The
// timeout branch rechecks the result channel before failing: a result and
// the timeout both ready never fail the test.
func awaitResult(t *testing.T, c <-chan error, what string) error {
	t.Helper()
	return testkit.WithinBy(t, c, time.After(testWait), what)
}

// awaitTrue polls a synchronized predicate within testWait, rechecking it
// before failing.
func awaitTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	testkit.AwaitCondBy(t, cond, tick.C, time.After(testWait), what)
}

// Review r1 C2 (iteration 10b): a remove or prune that passed its pre-lock
// fence check and is queued on the workspace lock while a publication
// registers its fence (and commits) rechecks the fence once it holds its
// locks: it releases them and waits again, cancellably, instead of erasing
// the publication's recovery evidence; it proceeds once the fence is
// released. The other side of the commit point (a fence registered first)
// is TestTaskRoutes/fences.
func TestTaskFenceQueued(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"remove", "prune"} {
		t.Run(op, func(t *testing.T) {
			m, _ := newManager(t)
			v := mustCreate(t, m, "delta")
			refenced := make(chan struct{}, 1)
			m.d.hook = func(stage string, _ context.Context) {
				if stage == op+"-refenced" {
					select {
					case refenced <- struct{}{}:
					default:
					}
				}
			}
			// A receive holds the workspace lock (the publication's commit).
			h, err := m.lookup("delta")
			if err != nil {
				t.Fatal(err)
			}
			if err := h.lock.Lock(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var err error
				if op == "remove" {
					_, err = m.Remove(ctx, "delta", v.Instance)
				} else {
					_, err = m.Prune(ctx, "delta", v.Instance, time.Now())
				}
				done <- err
			}()
			// The mutation passed its fence check and is queued on the lock.
			awaitTrue(t, op+" queued on the workspace lock", func() bool {
				h.lock.mu.Lock()
				defer h.lock.mu.Unlock()
				return h.lock.waiting == 1
			})
			release := m.FencePublication("delta", routeTask, strings.Repeat("c", 32))
			h.lock.Unlock()
			select {
			case <-refenced:
			case err := <-done:
				t.Fatalf("%s completed while a publication fence was active: %v", op, err)
			case <-time.After(testWait):
				select {
				case <-refenced:
				default:
					t.Fatalf("%s never rechecked the fence", op)
				}
			}
			// It holds no lock while waiting again, and changed nothing.
			if _, err := m.lookup("delta"); err != nil {
				t.Fatalf("%s removed the workspace behind a fence: %v", op, err)
			}
			if _, err := m.Show(ctx, "delta"); err != nil {
				t.Fatalf("a read behind the waiting %s: %v", op, err)
			}
			select {
			case err := <-done:
				t.Fatalf("%s completed while a publication fence was active: %v", op, err)
			default:
			}
			release()
			if err := awaitResult(t, done, op); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("cancelled-requeue", func(t *testing.T) {
		// A queued prune cancelled while it waits again returns.
		m, _ := newManager(t)
		v := mustCreate(t, m, "delta")
		cctx, cancel := context.WithCancel(ctx)
		m.d.hook = func(stage string, _ context.Context) {
			if stage == "prune-refenced" {
				cancel()
			}
		}
		h, _ := m.lookup("delta")
		if err := h.lock.Lock(ctx); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := m.Prune(cctx, "delta", v.Instance, time.Now())
			done <- err
		}()
		awaitTrue(t, "prune queued", func() bool {
			h.lock.mu.Lock()
			defer h.lock.mu.Unlock()
			return h.lock.waiting == 1
		})
		release := m.FencePublication("delta", routeTask, strings.Repeat("c", 32))
		defer release()
		h.lock.Unlock()
		if err := awaitResult(t, done, "cancelled prune"); err == nil {
			t.Fatal("a cancelled queued prune succeeded")
		}
	})
}

// Review r1 C1 (iteration 10b): a task receive whose CURRENT rename
// succeeded but whose workspace directory sync failed has an unconfirmed
// storage outcome: no success report, no terminal settlement (Published is
// never called), the publication fence stays, and the provenance read is
// refused (neither presence nor absence can settle the publication) until
// a restart's CURRENT validation confirms the committed ref.
func TestTaskReceiveUnconfirmed(t *testing.T) {
	ctx := context.Background()
	f := newTransportFixture(t)
	acc := TaskAccess{Name: "alpha", Instance: f.inst, Base: f.c0, TaskRef: contract.TaskRefPrefix + routeTask}
	files := seedFiles()
	files["result.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("result\n")}
	result := commit(t, f.local, files, "result", f.c0)
	pub := strings.Repeat("a", 32)
	var published sync.Mutex
	settled := false
	g := &fakeGuard{acc: acc, pub: pub, commit: result, published: func(TaskMetadata) error {
		published.Lock()
		settled = true
		published.Unlock()
		return nil
	}}
	wsDir := filepath.Join(f.m.dir, "alpha")
	var mu sync.Mutex
	failed := false
	f.m.d.fail = func(op, p string) error {
		mu.Lock()
		defer mu.Unlock()
		if op == "dirsync" && p == wsDir && !failed {
			failed = true
			return errors.New("injected CURRENT directory sync failure")
		}
		return nil
	}
	ph := taskAssignment(f.inst)
	ph.Set(contract.PublicationIDHeader, pub)
	own := contract.TaskRefPrefix + routeTask
	r := serveTask(f.m, g, http.MethodPost, taskPath("git-receive-pack"), ph,
		encodeReceive(t, cmd(own, plumbing.ZeroHash, result), []capability.Capability{capability.ReportStatus}, f.pack(t, result, f.c0)))
	mu.Lock()
	injected := failed
	mu.Unlock()
	published.Lock()
	wasSettled := settled
	published.Unlock()
	if !injected || r.ok() || wasSettled {
		t.Fatalf("unconfirmed CURRENT: injected %v report %s settled %v", injected, r, wasSettled)
	}
	if !f.m.Fenced("alpha") {
		t.Fatal("the publication fence was released while CURRENT durability is unconfirmed")
	}
	if st, err := f.m.TaskRef(ctx, "alpha", f.inst, routeTask); err == nil || contract.CodeOf(err) != contract.CodeInternal {
		t.Fatalf("unconfirmed CURRENT was read as provenance: %+v %v", st, err)
	}
	// The workspace itself is fenced (mutations refused until restart); a
	// prune waits on the kept publication fence until it is cancelled.
	if h, err := f.m.lookup("alpha"); err != nil || !h.fenced.Load() {
		t.Fatalf("the ambiguous commit left the workspace unfenced: %v", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, perr := f.m.Prune(pctx, "alpha", f.inst, time.Now())
	cancel()
	if perr == nil {
		t.Fatal("prune passed an unconfirmed publication")
	}
	// A fenced manager (an ambiguous namespace operation) refuses too, for a
	// live and for an absent workspace.
	m2, _ := newManager(t)
	v2 := mustCreate(t, m2, "beta")
	m2.fence()
	for _, name := range []string{"beta", "gone"} {
		if _, err := m2.TaskRef(ctx, name, v2.Instance, routeTask); err == nil {
			t.Fatalf("a fenced manager read the provenance of %s", name)
		}
	}
	// The restart validates CURRENT: the committed ref is confirmed.
	f.m.d.fail = nil
	f.m.Close()
	m3 := openAt(t, fastDeps(), f.m.root)
	st, err := m3.TaskRef(ctx, "alpha", f.inst, routeTask)
	if err != nil || !st.Exists || st.Commit != result.String() || st.PublicationID != pub {
		t.Fatalf("recovered provenance %+v %v", st, err)
	}
}
