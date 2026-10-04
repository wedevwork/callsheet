package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
	"github.com/wedevwork/callsheet/internal/testkit/workersmoke"
)

// wsDelegate runs a package contract in its compiled test binary and
// requires each named subtest's own PASS line.
func wsDelegate(t *testing.T, pkg, selector string, pass ...string) {
	t.Helper()
	contractRun(t, contractBinary(t, pkg), pkg, selector, os.Environ(), pass...)
}

// codeOf is err's contract code and fixed reason.
func codeOf(err error) (contract.Code, string) {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		return "", ""
	}
	r, _ := ce.Details["reason"].(string)
	return ce.Code, r
}

// taskCount is the number of durable task records.
func (r *wsRig) taskCount(t *testing.T) int {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(r.root, "plane", "tasks"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(es)
}

// logRecord returns sidecar s's JSON record msg for task id.
func logRecord(s *workersmoke.Sidecar, msg, id string) (map[string]any, bool) {
	for _, line := range strings.Split(s.Logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg && rec["task_id"] == id {
			return rec, true
		}
	}
	return nil, false
}

// wsPushTree pushes an arbitrary tree as branch through the coordinator
// receive-pack (for bases a plain folder cannot hold natively).
func (r *wsRig) wsPushTree(t *testing.T, name, inst, branch string, files map[string]testkit.FileSpec) plumbing.Hash {
	t.Helper()
	s := testkit.NewMemoryStore()
	c, err := testkit.CommitFiles(s, files, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	pushOK(t, r.remote(name, inst), s, "refs/heads/"+branch, plumbing.ZeroHash, c)
	return c
}

// FP-1: admission, the immutable binding and the preparation protocol.
func TestTaskWorkspaceAdmission(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("one\n")})
	t.Run("defaults", func(t *testing.T) {
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{})
		b := v.WorkspaceBinding
		if v.State != contract.TaskSucceeded || b == nil || b.Name != name || b.Instance != inst || b.BaseSelector != "refs/heads/main" || b.BaseCommit == nil || *b.BaseCommit != base {
			t.Fatalf("defaults: %s %+v", v.State, b)
		}
		if v.WorkspacePhase != nil || v.Request.Workspace == nil || *v.Request.Workspace != name || v.Request.Base != nil {
			t.Fatalf("view %+v", v)
		}
		publishedWs(t, v)
	})
	t.Run("empty", func(t *testing.T) {
		v := r.run(t, "ws-a", sp(name), sp("empty"), fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "n.txt", Data: "n\n"})})
		if v.WorkspaceBinding == nil || v.WorkspaceBinding.BaseSelector != "empty" || v.WorkspaceBinding.BaseCommit != nil {
			t.Fatalf("empty binding %+v", v.WorkspaceBinding)
		}
		w := publishedWs(t, v)
		got, c := r.files(t, name, inst, *w.Commit)
		wantResultCommit(t, c, v, contract.TaskSucceeded)
		if err := testkit.EqualFiles(got, specOf(map[string]tfile{"n.txt": tr("n\n")})); err != nil {
			t.Fatal(err)
		}
	})
	var first contract.TaskView
	t.Run("hash-and-task-ref", func(t *testing.T) {
		first = r.run(t, "ws-a", sp(name), sp(base), fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "f.txt", Data: "two\n"})})
		if first.WorkspaceBinding.BaseSelector != base || *first.WorkspaceBinding.BaseCommit != base {
			t.Fatalf("hash binding %+v", first.WorkspaceBinding)
		}
		if first.State != contract.TaskSucceeded {
			b, _ := json.Marshal(first)
			t.Fatalf("hash base: %s\n%s", b, r.a.Logs.String())
		}
		w := publishedWs(t, first)
		// A bare task ID selects that task's ref, normalized.
		v := r.run(t, "ws-a", sp(name), sp(first.TaskID), fakeadapter.WorkspaceScript{})
		if v.WorkspaceBinding.BaseSelector != contract.TaskRefPrefix+first.TaskID || *v.WorkspaceBinding.BaseCommit != *w.Commit {
			t.Fatalf("task-ref binding %+v", v.WorkspaceBinding)
		}
		w2 := publishedWs(t, v)
		_, c := r.files(t, name, inst, *w2.Commit)
		wantResultCommit(t, c, v, contract.TaskSucceeded, plumbing.NewHash(*w.Commit))
	})
	t.Run("invalid", func(t *testing.T) {
		unborn, uinst := r.workspace(t)
		other := strings.Repeat("0", 32)
		before := r.taskCount(t)
		for _, c := range []struct {
			name           string
			ws, base, inst *string
			code           contract.Code
		}{
			{"empty-name", sp(""), nil, nil, contract.CodeInvalidArgument},
			{"bad-name", sp("Bad Name"), nil, nil, contract.CodeInvalidArgument},
			{"base-without-workspace", nil, sp("main"), nil, contract.CodeInvalidArgument},
			{"instance-without-workspace", nil, nil, sp(inst), contract.CodeInvalidArgument},
			// A short hash is never resolved as a commit (as a branch
			// spelling it names no branch); no case repair.
			{"short-hash", sp(name), sp(base[:12]), nil, contract.CodeNotFound},
			{"upper-hash", sp(name), sp(strings.ToUpper(base)), nil, contract.CodeInvalidArgument},
			{"spaced", sp(name), sp(" main"), nil, contract.CodeInvalidArgument},
			{"bad-instance", sp(name), nil, sp("nope"), contract.CodeInvalidArgument},
			{"instance-mismatch", sp(name), nil, sp(other), contract.CodeConflict},
			{"missing-workspace", sp("nosuch"), nil, nil, contract.CodeNotFound},
			{"unborn-main", sp(unborn), nil, sp(uinst), contract.CodeNotFound},
			{"missing-branch", sp(name), sp("nobranch"), nil, contract.CodeNotFound},
			{"missing-commit", sp(name), sp(strings.Repeat("a", 40)), nil, contract.CodeNotFound},
		} {
			_, err := r.wsDispatch(t, "ws-a", c.ws, c.base, c.inst, fakeadapter.WorkspaceScript{})
			if code, _ := codeOf(err); code != c.code {
				t.Fatalf("%s: %v (want %s)", c.name, err, c.code)
			}
		}
		if after := r.taskCount(t); after != before {
			t.Fatalf("refused admissions left %d task records", after-before)
		}
		// The matching instance is accepted.
		v, err := r.wsDispatch(t, "ws-a", sp(name), nil, sp(inst), fakeadapter.WorkspaceScript{})
		if err != nil {
			t.Fatal(err)
		}
		publishedWs(t, r.await(t, v.TaskID))
	})
	t.Run("immutable-binding", func(t *testing.T) {
		b := newBarrier(t)
		v := r.running(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("in")},
			fakeadapter.WorkspaceOp{WaitFor: b.path("go")}, fakeadapter.WorkspaceOp{Write: "late.txt", Data: "l\n"})}, b, "in")
		tip := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("moved\n")})
		if tip == *v.WorkspaceBinding.BaseCommit {
			t.Fatal("main did not move")
		}
		b.release(t, "go")
		done := r.await(t, v.TaskID)
		w := publishedWs(t, done)
		got, c := r.files(t, name, inst, *w.Commit)
		wantResultCommit(t, c, done, contract.TaskSucceeded, plumbing.NewHash(*v.WorkspaceBinding.BaseCommit))
		if string(got["f.txt"].Content) != "one\n" {
			t.Fatalf("the result follows the moved branch: %q", got["f.txt"].Content)
		}
	})
	// The preparing acknowledgement against the old 15 s start timer, on
	// an armed fake clock, with heartbeats and controls responsive and no
	// duplicate child.
	t.Run("slow-prepare", func(t *testing.T) {
		wsDelegate(t, "./internal/sidecar", "^TestTaskWorkspaceSession$/^slow-prepare$", "TestTaskWorkspaceSession/slow-prepare")
		wsDelegate(t, "./internal/taskpublication", "^TestPlaneWiring$/^preparation$", "TestPlaneWiring/preparation")
	})
}

// FP-2: the node transfer routes are restricted to the assignment.
func TestTaskWorkspaceAccess(t *testing.T) {
	r := sharedWsRig(t)
	ctx := context.Background()
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("one\n")})
	other := r.seed(t, name, inst, "side", map[string]tfile{"g.txt": tr("side\n")})
	b := newBarrier(t)
	v := r.running(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("in")},
		fakeadapter.WorkspaceOp{WaitFor: b.path("go")}, fakeadapter.WorkspaceOp{Write: "x.txt", Data: "x\n"})}, b, "in")
	asg := r.assignment(t, v.TaskID)
	c := r.dep.Client
	t.Run("bound-base-only", func(t *testing.T) {
		g, err := c.NodeTaskGit(v.TaskID, asg, "")
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		refs, err := g.UploadRefs(ctx)
		if err != nil || len(refs) != 1 || refs["refs/heads/base"].String() != base {
			t.Fatalf("advertisement %v %v", refs, err)
		}
		if err := g.Fetch(ctx, []plumbing.Hash{plumbing.NewHash(other)}, nil, func(io.Reader) error { return nil }); err == nil {
			t.Fatal("a commit other than the bound base was served")
		}
	})
	t.Run("wrong-assignment", func(t *testing.T) {
		for _, c2 := range []struct {
			name   string
			mut    func(a *contract.NodeAssignment) string
			code   contract.Code
			reason string
		}{
			{"node", func(a *contract.NodeAssignment) string { a.NodeID = r.z.NodeID; return v.TaskID }, contract.CodeConflict, contract.ReasonTaskAssignmentMismatch},
			{"execution", func(a *contract.NodeAssignment) string { a.Execution.Attachment++; return v.TaskID }, contract.CodeConflict, contract.ReasonTaskAssignmentMismatch},
			{"digest", func(a *contract.NodeAssignment) string { a.StartDigest = strings.Repeat("0", 64); return v.TaskID }, contract.CodeConflict, contract.ReasonTaskAssignmentMismatch},
			{"instance", func(a *contract.NodeAssignment) string { a.Instance = strings.Repeat("0", 32); return v.TaskID }, contract.CodeConflict, contract.ReasonWorkspaceInstanceMismatch},
			{"task", func(a *contract.NodeAssignment) string { return "t_" + strings.Repeat("0", 32) }, contract.CodeNotFound, ""},
		} {
			a := asg
			id := c2.mut(&a)
			g, err := c.NodeTaskGit(id, a, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = g.UploadRefs(ctx)
			g.Close()
			if code, reason := codeOf(err); code != c2.code || reason != c2.reason {
				t.Fatalf("%s: %v (%s %s)", c2.name, err, code, reason)
			}
		}
	})
	t.Run("receive-needs-intent", func(t *testing.T) {
		p, err := c.NodeTaskPusher(v.TaskID, asg, strings.Repeat("1", 32))
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		// The advertisement is read-only (the bound base only); the
		// receive itself needs the open durable intent, checked before any
		// pack byte.
		refs, err := p.ReceiveRefs(ctx)
		if err != nil || len(refs) != 1 || refs["refs/heads/base"].String() != base {
			t.Fatalf("receive advertisement %v %v", refs, err)
		}
		err = p.PushTask(ctx, contract.TaskRefPrefix+v.TaskID, plumbing.NewHash(base), bytes.NewReader(nil))
		if code, reason := codeOf(err); code != contract.CodeConflict || reason != contract.ReasonTaskPublicationClosed {
			t.Fatalf("receive without an open intent: %v", err)
		}
	})
	b.release(t, "go")
	done := r.await(t, v.TaskID)
	w := publishedWs(t, done)
	t.Run("own-ref-once", func(t *testing.T) {
		rec := r.record(t, v.TaskID)
		if rec.Publication == nil || rec.Publication.PublicationID == "" {
			t.Fatalf("record publication %+v", rec.Publication)
		}
		// The settled publication's receive is closed: a second creation
		// (or retransmission) is refused before any pack.
		p, err := c.NodeTaskPusher(v.TaskID, asg, rec.Publication.PublicationID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.ReceiveRefs(ctx)
		if code, _ := codeOf(err); code != contract.CodeConflict {
			t.Fatalf("receive after settlement: %v", err)
		}
		err = p.PushTask(ctx, contract.TaskRefPrefix+v.TaskID, plumbing.NewHash(base), bytes.NewReader(nil))
		p.Close()
		if code, _ := codeOf(err); code != contract.CodeConflict {
			t.Fatalf("second creation: %v", err)
		}
		// Upload is refused once the task is terminal.
		g, _ := c.NodeTaskGit(v.TaskID, asg, "")
		_, err = g.UploadRefs(ctx)
		g.Close()
		if code, _ := codeOf(err); code != contract.CodeConflict {
			t.Fatalf("upload after terminal: %v", err)
		}
	})
	t.Run("coordinator-cannot-write", func(t *testing.T) {
		s := testkit.NewMemoryStore()
		h, err := testkit.CommitFiles(s, map[string]testkit.FileSpec{"z": {Mode: filemode.Regular, Content: []byte("z")}}, nil, "z")
		if err != nil {
			t.Fatal(err)
		}
		ref := contract.TaskRefPrefix + v.TaskID
		if res := r.remote(name, inst).Push(ctx, s, ref, plumbing.NewHash(*w.Commit), h); res.OK() {
			t.Fatal("the coordinator updated a task ref")
		}
		if res := r.remote(name, inst).Push(ctx, s, ref, plumbing.NewHash(*w.Commit), plumbing.ZeroHash); res.OK() {
			t.Fatal("the coordinator deleted a task ref")
		}
		refs := r.refs(t, name, inst)
		if refs[ref].String() != *w.Commit || refs["refs/heads/main"].String() != base || refs["refs/heads/side"].String() != other {
			t.Fatalf("refs %v", refs)
		}
	})
	t.Run("routes", func(t *testing.T) {
		wsDelegate(t, "./internal/workspace", "^TestTaskRoutes$", "TestTaskRoutes")
	})
}

// FP-3: the shared per-node cache.
func TestTaskWorkspaceCache(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	files := map[string]tfile{}
	for i := 0; i < 64; i++ {
		files[fmt.Sprintf("d/f%02d.txt", i)] = tr(strings.Repeat(fmt.Sprintf("line %d\n", i), 64))
	}
	r.seed(t, name, inst, "", files)
	prepared := func(t *testing.T, s *workersmoke.Sidecar, id string) map[string]any {
		t.Helper()
		var rec map[string]any
		eventually(t, "prepared record", func() bool {
			var ok bool
			rec, ok = logRecord(s, "task workspace prepared", id)
			return ok
		})
		return rec
	}
	t.Run("sequential", func(t *testing.T) {
		v1 := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{})
		publishedWs(t, v1)
		p1 := prepared(t, r.a, v1.TaskID)
		v2 := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{})
		publishedWs(t, v2)
		p2 := prepared(t, r.a, v2.TaskID)
		if p1["cache_hit"] != false || p2["cache_hit"] != true || p2["saved_bytes"].(float64) <= 0 || p2["fetched_bytes"].(float64) >= p1["fetched_bytes"].(float64) {
			t.Fatalf("cache stats %v then %v", p1, p2)
		}
	})
	t.Run("overlapping", func(t *testing.T) {
		b := newBarrier(t)
		script := func(me, peer string) fakeadapter.WorkspaceScript {
			return fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: me + ".txt", Data: me}, fakeadapter.WorkspaceOp{Touch: b.path(me)},
				fakeadapter.WorkspaceOp{WaitFor: b.path(peer)})}
		}
		va, err := r.wsDispatch(t, "ws-a", sp(name), nil, nil, script("x", "y"))
		if err != nil {
			t.Fatal(err)
		}
		vb, err := r.wsDispatch(t, "ws-a", sp(name), nil, nil, script("y", "x"))
		if err != nil {
			var ce *contract.Error
			errors.As(err, &ce)
			t.Fatalf("%v %+v", err, ce.Details)
		}
		for _, v := range []contract.TaskView{r.await(t, va.TaskID), r.await(t, vb.TaskID)} {
			publishedWs(t, v)
		}
	})
	t.Run("corruption-refetch", func(t *testing.T) {
		cur := filepath.Join(r.a.State, "workspace-cache", inst, "current")
		n := 0
		filepath.Walk(cur, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() && strings.Contains(p, string(filepath.Separator)+"objects"+string(filepath.Separator)) {
				os.Chmod(p, 0o600)
				os.WriteFile(p, []byte("corrupt"), 0o600)
				n++
			}
			return nil
		})
		if n == 0 {
			t.Fatalf("no cached objects under %s", cur)
		}
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{})
		publishedWs(t, v)
		// The corrupt disposable entry is discarded and one fresh
		// authorized fetch (no haves) serves the task.
		if p := prepared(t, r.a, v.TaskID); p["cache_hit"] != false || (p["cache_purged"] != true && p["refetched"] != true) || p["fetched_bytes"].(float64) <= 0 {
			t.Fatalf("corrupt cache not refetched: %v", p)
		}
	})
	t.Run("eviction-during-execution", func(t *testing.T) {
		b := newBarrier(t)
		v := r.running(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("in")},
			fakeadapter.WorkspaceOp{WaitFor: b.path("go")}, fakeadapter.WorkspaceOp{Write: "after.txt", Data: "a"})}, b, "in")
		// The disposable entry disappears while the child runs: its own
		// copy and checkout are independent.
		if err := os.RemoveAll(filepath.Join(r.a.State, "workspace-cache", inst)); err != nil {
			t.Fatal(err)
		}
		b.release(t, "go")
		w := publishedWs(t, r.await(t, v.TaskID))
		got, _ := r.files(t, name, inst, *w.Commit)
		if len(got) != len(files)+1 || string(got["after.txt"].Content) != "a" {
			t.Fatalf("result after eviction has %d files", len(got))
		}
	})
	t.Run("removed-workspace", func(t *testing.T) {
		gone, ginst := r.workspace(t)
		r.seed(t, gone, ginst, "", map[string]tfile{"f": tr("f")})
		if _, err := r.dep.Client.RemoveWorkspace(context.Background(), gone, ginst); err != nil {
			t.Fatal(err)
		}
		if _, err := r.wsDispatch(t, "ws-a", sp(gone), nil, nil, fakeadapter.WorkspaceScript{}); err == nil {
			t.Fatal("a removed workspace was admitted")
		}
	})
	t.Run("matrix", func(t *testing.T) {
		wsDelegate(t, "./internal/taskworkspace", "^TestCache$", "TestCache")
	})
}

// FP-4: the private checkout or empty scratch directory.
func TestTaskWorkspaceCheckout(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"README.md": tr("# r\n"), "bin/run.sh": tx("#!/bin/sh\n"), "link": tlnk("README.md"),
		"callsheet-final.txt": tr("a project file\n")})
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	check := func(t *testing.T, rep fakeadapter.WorkspaceReport, s *workersmoke.Sidecar, id string) {
		t.Helper()
		if want := filepath.Join(taskDir(s, id), "work"); rep.CWD != want || rep.PWD != want || rep.CWDMode != "0700" {
			t.Fatalf("cwd %s pwd %s mode %s, want %s", rep.CWD, rep.PWD, rep.CWDMode, want)
		}
	}
	t.Run("base", func(t *testing.T) {
		b := newBarrier(t)
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Report: b.path("rep"), Ops: ops(fakeadapter.WorkspaceOp{Write: "w.txt", Data: "w"})})
		rep := readWsReport(t, b.path("rep"))
		check(t, rep, r.a, v.TaskID)
		if !rep.HasGit || rep.HEAD != base+"\n" {
			t.Fatalf("HEAD %q git %v", rep.HEAD, rep.HasGit)
		}
		want := map[string]string{"README.md": "f 0644 " + sum("# r\n"), "bin": "d 0755 " + sum(""), "bin/run.sh": "f 0755 " + sum("#!/bin/sh\n"),
			"callsheet-final.txt": "f 0644 " + sum("a project file\n")}
		for p, w := range want {
			if rep.Files[p] != w {
				t.Fatalf("%s = %q, want %q (all %v)", p, rep.Files[p], w, rep.Files)
			}
		}
		if l := rep.Files["link"]; !strings.HasPrefix(l, "l ") || !strings.HasSuffix(l, sum("README.md")) || len(rep.Files) != 5 {
			t.Fatalf("files %v", rep.Files)
		}
		w := publishedWs(t, v)
		got, _ := r.files(t, name, inst, *w.Commit)
		// The project's own callsheet-final.txt is ordinary data.
		if string(got["callsheet-final.txt"].Content) != "a project file\n" || string(got["w.txt"].Content) != "w" {
			t.Fatalf("result %v", got)
		}
		eventually(t, "work removed", func() bool { _, err := os.Stat(taskDir(r.a, v.TaskID)); return os.IsNotExist(err) })
	})
	t.Run("explicit-empty", func(t *testing.T) {
		b := newBarrier(t)
		v := r.run(t, "ws-a", sp(name), sp("empty"), fakeadapter.WorkspaceScript{Report: b.path("rep")})
		rep := readWsReport(t, b.path("rep"))
		check(t, rep, r.a, v.TaskID)
		if !rep.HasGit || rep.HEAD != "ref: refs/heads/main\n" || len(rep.Files) != 0 {
			t.Fatalf("empty checkout %+v", rep)
		}
		publishedWs(t, v)
	})
	t.Run("no-workspace", func(t *testing.T) {
		b := newBarrier(t)
		v := r.run(t, "ws-a", nil, nil, fakeadapter.WorkspaceScript{Report: b.path("rep")})
		rep := readWsReport(t, b.path("rep"))
		check(t, rep, r.a, v.TaskID)
		if rep.HasGit || len(rep.Files) != 0 || v.State != contract.TaskSucceeded || v.Result.Workspace != nil || v.WorkspaceBinding != nil {
			t.Fatalf("scratch %+v view %+v", rep, v.Result)
		}
		eventually(t, "scratch removed", func() bool { _, err := os.Stat(taskDir(r.a, v.TaskID)); return os.IsNotExist(err) })
	})
	t.Run("native-paths", func(t *testing.T) {
		// Case aliases: distinct entries on Linux, a refused collision on a
		// case-insensitive APFS (never an overwrite).
		alias := r.wsPushTree(t, name, inst, "alias", map[string]testkit.FileSpec{"A.txt": {Mode: filemode.Regular, Content: []byte("A")},
			"a.txt": {Mode: filemode.Regular, Content: []byte("a")}})
		v := r.run(t, "ws-a", sp(name), sp(alias.String()), fakeadapter.WorkspaceScript{})
		if runtime.GOOS == "darwin" {
			if v.State != contract.TaskRejected || v.Reason == nil || v.Reason.Code != contract.ReasonWorkspaceCheckoutFailed {
				t.Fatalf("darwin alias: %s %+v", v.State, v.Reason)
			}
		} else {
			w := publishedWs(t, v)
			got, _ := r.files(t, name, inst, *w.Commit)
			if string(got["A.txt"].Content) != "A" || string(got["a.txt"].Content) != "a" {
				t.Fatalf("linux alias result %v", got)
			}
		}
		// An escaping link refuses the checkout on every platform.
		esc := r.wsPushTree(t, name, inst, "escape", map[string]testkit.FileSpec{"out": {Mode: filemode.Symlink, Content: []byte("../../../etc")}})
		v = r.run(t, "ws-a", sp(name), sp(esc.String()), fakeadapter.WorkspaceScript{})
		if v.State != contract.TaskRejected || v.Reason == nil || v.Reason.Code != contract.ReasonWorkspaceCheckoutFailed ||
			v.Result.Workspace == nil || v.Result.Workspace.Publication != contract.PublicationNotApplicable {
			t.Fatalf("escaping link: %s %+v", v.State, v.Reason)
		}
		r.noRef(t, name, inst, v.TaskID)
		eventually(t, "refused work removed", func() bool { _, err := os.Stat(taskDir(r.a, v.TaskID)); return os.IsNotExist(err) })
	})
	t.Run("matrix", func(t *testing.T) {
		wsDelegate(t, "./internal/taskworkspace", "^TestPrepare$", "TestPrepare")
	})
}

// FP-5: the result commit (AC-WS-1).
func TestTaskWorkspaceCommit(t *testing.T) {
	r := sharedWsRig(t)
	t.Run("AC-WS-1", func(t *testing.T) {
		name, inst := r.workspace(t)
		base := r.seed(t, name, inst, "", map[string]tfile{"README.md": tr("# base\n"), "src/a.txt": tr("a\n"), "run.sh": tx("#!/bin/sh\n"), "gone.txt": tr("bye\n")})
		before := r.refs(t, name, inst)
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(
			fakeadapter.WorkspaceOp{Write: "src/a.txt", Data: "a2\n"}, fakeadapter.WorkspaceOp{Write: "new/b.txt", Data: "b\n"}, fakeadapter.WorkspaceOp{Remove: "gone.txt"})})
		if v.State != contract.TaskSucceeded {
			b, _ := json.Marshal(v)
			t.Fatalf("state %s: %s\n%s", v.State, b, r.a.Logs.String())
		}
		w := publishedWs(t, v)
		if v.WorkspaceBinding == nil || v.WorkspaceBinding.BaseSelector != "refs/heads/main" || v.WorkspaceBinding.BaseCommit == nil || *v.WorkspaceBinding.BaseCommit != base {
			t.Fatalf("binding %+v", v.WorkspaceBinding)
		}
		got, c := r.files(t, name, inst, *w.Commit)
		wantResultCommit(t, c, v, contract.TaskSucceeded, plumbing.NewHash(base))
		want := specOf(map[string]tfile{"README.md": tr("# base\n"), "src/a.txt": tr("a2\n"), "run.sh": tx("#!/bin/sh\n"), "new/b.txt": tr("b\n")})
		if err := testkit.EqualFiles(got, want); err != nil {
			t.Fatal(err)
		}
		after := r.refs(t, name, inst)
		if after["refs/heads/main"] != before["refs/heads/main"] || after[contract.TaskRefPrefix+v.TaskID].String() != *w.Commit {
			t.Fatalf("refs before %v after %v", before, after)
		}
		if w.Diffstat == nil || w.Diffstat.Added != 1 || w.Diffstat.Modified != 1 || w.Diffstat.Deleted != 1 || len(w.Changes) != 3 {
			t.Fatalf("diffstat %+v changes %+v", w.Diffstat, w.Changes)
		}
		var paths []string
		for _, ch := range w.Changes {
			b, _ := base64.StdEncoding.DecodeString(ch.PathBase64)
			paths = append(paths, string(b)+":"+ch.Kind)
		}
		if strings.Join(paths, ",") != "gone.txt:deleted,new/b.txt:added,src/a.txt:modified" {
			t.Fatalf("changes %v", paths)
		}
	})
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"keep.txt": tr("k\n"), "old.log": tr("log\n"), "crlf.txt": tr("x\n")})
	t.Run("child-git-and-ignores", func(t *testing.T) {
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(
			fakeadapter.WorkspaceOp{Write: ".gitignore", Data: "build/\nold.log\n"}, fakeadapter.WorkspaceOp{Write: "build/out.bin", Data: "obj"},
			fakeadapter.WorkspaceOp{Write: "crlf.txt", Data: "a\r\nb\r\n"},
			// The child's own commits, branch and HEAD have no effect.
			fakeadapter.WorkspaceOp{GitCommit: "child commit", GitBranch: "child"}, fakeadapter.WorkspaceOp{Write: "post.txt", Data: "p"})})
		w := publishedWs(t, v)
		got, c := r.files(t, name, inst, *w.Commit)
		wantResultCommit(t, c, v, contract.TaskSucceeded, plumbing.NewHash(base))
		want := specOf(map[string]tfile{".gitignore": tr("build/\nold.log\n"), "keep.txt": tr("k\n"), "crlf.txt": tr("a\r\nb\r\n"), "post.txt": tr("p")})
		if err := testkit.EqualFiles(got, want); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unchanged-and-removed-git", func(t *testing.T) {
		v1 := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{})
		w1 := publishedWs(t, v1)
		_, c1 := r.files(t, name, inst, *w1.Commit)
		_, cb := r.fetch(t, name, inst, base)
		if c1.TreeHash != cb.TreeHash || w1.Diffstat == nil || *w1.Diffstat != (contract.TaskDiffstat{}) || len(w1.Changes) != 0 {
			t.Fatalf("unchanged result %+v", w1)
		}
		v2 := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{RemoveGit: true})})
		w2 := publishedWs(t, v2)
		_, c2 := r.files(t, name, inst, *w2.Commit)
		if c2.TreeHash != cb.TreeHash || *w2.Commit == *w1.Commit {
			t.Fatalf("removed .git result %s (tree %s)", *w2.Commit, c2.TreeHash)
		}
	})
	t.Run("nested-repository", func(t *testing.T) {
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "sub/.git/HEAD", Data: "ref: refs/heads/main\n"})})
		if v.State != contract.TaskSucceeded || v.Result.Workspace == nil || v.Result.Workspace.Publication != contract.PublicationFailed ||
			v.Result.Workspace.Error == nil || *v.Result.Workspace.Error != contract.PubErrUnsupportedRepository {
			t.Fatalf("nested repository: %s %+v", v.State, v.Result.Workspace)
		}
		r.noRef(t, name, inst, v.TaskID)
	})
	t.Run("matrix", func(t *testing.T) {
		wsDelegate(t, "./internal/taskworkspace", "^TestSnapshot$", "TestSnapshot")
	})
}

// awaitResolved polls task id to terminal, requiring on every observation
// that a terminal view carries its resolved publication and a
// nonterminal one is pending/running.
func (r *wsRig) awaitResolved(t *testing.T, id string) contract.TaskView {
	t.Helper()
	var v contract.TaskView
	eventually(t, "task "+id+" terminal", func() bool {
		var err error
		v, err = r.dep.Client.ShowTask(context.Background(), id, contract.DefaultTailLines)
		if err != nil {
			t.Fatal(err)
		}
		if contract.TaskTerminal(v.State) {
			if v.Result == nil || v.Result.Workspace == nil || v.Result.Workspace.Publication == "" || v.WorkspacePhase != nil {
				t.Fatalf("terminal view without its resolved publication: %+v", v)
			}
			return true
		}
		if v.WorkspacePhase != nil && *v.WorkspacePhase == contract.WorkspacePhasePublishing && (!v.CompletionPending || v.Result != nil) {
			t.Fatalf("publishing view %+v", v)
		}
		return false
	})
	return v
}

// FP-6: durable publication of partial results (AC-WS-5).
func TestTaskWorkspacePublication(t *testing.T) {
	r := sharedWsRig(t)
	ctx := context.Background()
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("f\n")})
	partial := func(t *testing.T, v contract.TaskView, state string) {
		t.Helper()
		if v.State != state {
			t.Fatalf("state %s, want %s: %+v", v.State, state, v.Result)
		}
		w := publishedWs(t, v)
		got, c := r.files(t, name, inst, *w.Commit)
		wantResultCommit(t, c, v, state, plumbing.NewHash(base))
		if string(got["partial.txt"].Content) != "written" || string(got["f.txt"].Content) != "f\n" {
			t.Fatalf("%s partial tree %v", state, got)
		}
	}
	held := func(b barrier) fakeadapter.WorkspaceScript {
		return fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "partial.txt", Data: "written"}, fakeadapter.WorkspaceOp{Touch: b.path("w")},
			fakeadapter.WorkspaceOp{WaitFor: b.path("never")})}
	}
	t.Run("AC-WS-5", func(t *testing.T) {
		t.Run("succeeded", func(t *testing.T) {
			v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "partial.txt", Data: "written"})})
			partial(t, r.awaitResolved(t, v.TaskID), contract.TaskSucceeded)
		})
		t.Run("failed", func(t *testing.T) {
			v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "partial.txt", Data: "written"}), Exit: 3})
			partial(t, v, contract.TaskFailed)
			if v.Result.ExitCode == nil || *v.Result.ExitCode != 3 {
				t.Fatalf("exit %v", v.Result.ExitCode)
			}
		})
		t.Run("cancelled", func(t *testing.T) {
			b := newBarrier(t)
			v := r.running(t, "ws-a", sp(name), nil, held(b), b, "w")
			if n := r.inflight(t, "ws-a"); n < 1 {
				t.Fatalf("inflight %d while running", n)
			}
			if _, err := r.dep.Client.CancelTask(ctx, v.TaskID); err != nil {
				t.Fatal(err)
			}
			partial(t, r.awaitResolved(t, v.TaskID), contract.TaskCancelled)
		})
		t.Run("timed_out", func(t *testing.T) {
			b := newBarrier(t)
			d := 1500 * time.Millisecond
			req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "ws-a"}, Goal: fakeadapter.WorkspaceGoal(held(b)), Payload: []string{},
				Acceptance: "fixture", RequestedBy: contract.RequestedBy{Name: "ws-test", Version: "1", Hostname: "fixture"}, Workspace: sp(name),
				Override: &contract.TaskOverride{Timeout: &d}}
			v, err := r.dep.Client.Dispatch(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			b.await(t, "w")
			partial(t, r.awaitResolved(t, v.TaskID), contract.TaskTimedOut)
		})
	})
	t.Run("rejected-no-ref", func(t *testing.T) {
		esc := r.wsPushTree(t, name, inst, "escape", map[string]testkit.FileSpec{"out": {Mode: filemode.Symlink, Content: []byte("/etc/passwd")}})
		v := r.run(t, "ws-a", sp(name), sp(esc.String()), fakeadapter.WorkspaceScript{})
		if v.State != contract.TaskRejected || v.Result.Workspace.Publication != contract.PublicationNotApplicable || v.StartedAt != nil {
			t.Fatalf("rejected %s %+v", v.State, v.Result.Workspace)
		}
		r.noRef(t, name, inst, v.TaskID)
	})
	t.Run("lost-no-ref", func(t *testing.T) {
		b := newBarrier(t)
		v := r.running(t, "ws-a", sp(name), nil, held(b), b, "w")
		// Shutdown before any intent abandons the publication.
		if err := r.dep.Restart(ctx, r.a); err != nil {
			t.Fatal(err)
		}
		done := r.awaitResolved(t, v.TaskID)
		if done.State != contract.TaskLost || done.Result.Workspace.Publication != contract.PublicationNotApplicable || done.Result.ResultCommit != nil {
			t.Fatalf("lost %s %+v", done.State, done.Result.Workspace)
		}
		r.noRef(t, name, inst, v.TaskID)
		eventually(t, "lost work removed", func() bool { _, err := os.Stat(taskDir(r.a, v.TaskID)); return os.IsNotExist(err) })
	})
	t.Run("refused-push-keeps-outcome", func(t *testing.T) {
		gone, ginst := r.workspace(t)
		r.seed(t, gone, ginst, "", map[string]tfile{"f": tr("f")})
		b := newBarrier(t)
		v := r.running(t, "ws-a", sp(gone), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("in")},
			fakeadapter.WorkspaceOp{WaitFor: b.path("go")}, fakeadapter.WorkspaceOp{Write: "x", Data: "x"})}, b, "in")
		if _, err := r.dep.Client.RemoveWorkspace(ctx, gone, ginst); err != nil {
			t.Fatal(err)
		}
		b.release(t, "go")
		done := r.awaitResolved(t, v.TaskID)
		w := done.Result.Workspace
		if done.State != contract.TaskSucceeded || w.Publication != contract.PublicationFailed || w.Commit != nil || w.Ref != nil || w.Error == nil ||
			!contract.ValidPublicationError(*w.Error) || w.Diffstat != nil || len(w.Changes) != 0 || done.Result.ResultCommit != nil {
			t.Fatalf("refused publication %s %+v", done.State, w)
		}
	})
	eventually(t, "capacity released", func() bool { return r.inflight(t, "ws-a") == 0 })
	t.Run("matrix", func(t *testing.T) {
		wsDelegate(t, "./internal/taskpublication", "^TestPublication$", "TestPublication")
		wsDelegate(t, "./internal/taskworkspace", "^TestPublish$", "TestPublish")
	})
}

// wsKindsRow is one TestTaskWorkspaceMetadata/kinds change row.
type wsKindsRow struct {
	kind, oldMode, newMode string
	oldBytes, newBytes     int64
}

// wsKindsRawOp is the kinds script's index of its last op, the create of
// the non-UTF-8 name "\xff\xfe.raw".
const wsKindsRawOp = 6

// wsKindsWant selects TestTaskWorkspaceMetadata/kinds' expected rows and
// Added count by that create's outcome as the child reported it (state,
// logTail): the task succeeded with no op error (the name was created;
// Linux), every row and Added 5; the task failed and its whole log is that
// op's create refused for its encoding (APFS: EILSEQ), the same rows
// without that empty file (NewBytes unchanged) and Added 4. Any other
// outcome fails.
func wsKindsWant(state, logTail, sentinel string) (map[string]wsKindsRow, int64, error) {
	full := map[string]wsKindsRow{
		"mod": {"modified", "100644", "100644", 3, int64(len(sentinel))}, "del": {"deleted", "100644", "-", 5, -1},
		"bin.dat": {"added", "-", "100644", -1, 6}, "ln": {"added", "-", "120000", -1, 4}, "x.sh": {"added", "-", "100755", -1, 2},
		"ctl\x01name": {"added", "-", "100644", -1, 0}, "\xff\xfe.raw": {"added", "-", "100644", -1, 0},
	}
	line, rest, _ := strings.Cut(logTail, "\n")
	switch {
	case state == contract.TaskSucceeded && !strings.Contains(logTail, "fake-adapter:"):
		return full, 5, nil
	case state == contract.TaskFailed && rest == "" && strings.HasSuffix(logTail, "\n") &&
		strings.HasPrefix(line, fmt.Sprintf("fake-adapter: op %d: open ", wsKindsRawOp)) && strings.HasSuffix(line, ": "+syscall.EILSEQ.Error()):
		delete(full, "\xff\xfe.raw")
		return full, 4, nil
	}
	return nil, 0, fmt.Errorf("create of the non-UTF-8 name: task %s, log %q", state, logTail)
}

// TestTaskWorkspaceMetadataKindsSelector pins wsKindsWant: a created name
// keeps every row; a create refused for its encoding (APFS: EILSEQ) omits
// only that empty file; any other outcome fails.
func TestTaskWorkspaceMetadataKindsSelector(t *testing.T) {
	t.Parallel()
	const sentinel = "S"
	opErr := func(op int, errno syscall.Errno) string {
		return fmt.Sprintf("fake-adapter: op %d: %v\n", op, &os.PathError{Op: "open", Path: "/w/work/\xff\xfe.raw", Err: errno})
	}
	full, added, err := wsKindsWant(contract.TaskSucceeded, "", sentinel)
	if err != nil || added != 5 || len(full) != 7 || full["\xff\xfe.raw"] != (wsKindsRow{"added", "-", "100644", -1, 0}) ||
		full["mod"] != (wsKindsRow{"modified", "100644", "100644", 3, int64(len(sentinel))}) {
		t.Fatalf("created: %v %d %v", full, added, err)
	}
	refused, added, err := wsKindsWant(contract.TaskFailed, opErr(wsKindsRawOp, syscall.EILSEQ), sentinel)
	delete(full, "\xff\xfe.raw")
	if err != nil || added != 4 || fmt.Sprint(refused) != fmt.Sprint(full) {
		t.Fatalf("encoding refused: %v %d %v", refused, added, err)
	}
	for _, c := range []struct{ state, log string }{
		{contract.TaskFailed, opErr(wsKindsRawOp, syscall.EIO)},
		{contract.TaskFailed, opErr(wsKindsRawOp-1, syscall.EILSEQ)},
		{contract.TaskFailed, "x\n" + opErr(wsKindsRawOp, syscall.EILSEQ)},
		{contract.TaskSucceeded, opErr(wsKindsRawOp, syscall.EILSEQ)},
		{contract.TaskFailed, ""},
		{contract.TaskLost, ""},
	} {
		if m, n, err := wsKindsWant(c.state, c.log, sentinel); err == nil {
			t.Errorf("%s %q selected %v %d", c.state, c.log, m, n)
		}
	}
}

// FP-7: bounded metadata without file contents.
func TestTaskWorkspaceMetadata(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	// The sentinel lives only in file bytes (the seeded base and the
	// child's copy), never in the goal or any output.
	const sentinel = "WS-FILE-SENTINEL-10b-7c1e"
	r.seed(t, name, inst, "", map[string]tfile{"keep": tr("k"), "mod": tr("old"), "del": tr("gone!"), "secret": tr(sentinel)})
	t.Run("kinds", func(t *testing.T) {
		bin := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff, 0xfe, 'x'})
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(
			fakeadapter.WorkspaceOp{Copy: "secret", Target: "mod"}, fakeadapter.WorkspaceOp{Remove: "del"},
			fakeadapter.WorkspaceOp{Write: "bin.dat", DataB64: bin}, fakeadapter.WorkspaceOp{Symlink: "ln", Target: "keep"},
			fakeadapter.WorkspaceOp{Write: "x.sh", Data: "#!", Mode: 0o755},
			fakeadapter.WorkspaceOp{WriteB64: base64.StdEncoding.EncodeToString([]byte("ctl\x01name"))},
			fakeadapter.WorkspaceOp{WriteB64: base64.StdEncoding.EncodeToString([]byte("\xff\xfe.raw"))})})
		w := publishedWs(t, v)
		// APFS refuses a non-UTF-8 name; wsKindsWant selects the rows by
		// the child's real create outcome.
		want, added, err := wsKindsWant(v.State, v.Result.LogTail, sentinel)
		if err != nil {
			t.Fatal(err)
		}
		type row = wsKindsRow
		mode := func(p *string) string {
			if p == nil {
				return "-"
			}
			return *p
		}
		n := func(p *int64) int64 {
			if p == nil {
				return -1
			}
			return *p
		}
		got := map[string]row{}
		var order []string
		for _, ch := range w.Changes {
			p, err := base64.StdEncoding.DecodeString(ch.PathBase64)
			if err != nil {
				t.Fatal(err)
			}
			order = append(order, string(p))
			got[string(p)] = row{ch.Kind, mode(ch.OldMode), mode(ch.NewMode), n(ch.OldBytes), n(ch.NewBytes)}
		}
		if !sort.StringsAreSorted(order) {
			t.Fatalf("changes not in raw byte order: %q", order)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("changes %v\nwant %v", got, want)
		}
		d := w.Diffstat
		if d.Added != added || d.Modified != 1 || d.Deleted != 1 || d.OldBytes != 8 || d.NewBytes != int64(len(sentinel))+6+4+2 || w.ChangesTruncated || w.NextAfter != nil {
			t.Fatalf("diffstat %+v truncated %v", d, w.ChangesTruncated)
		}
		// Mirrors, and no file content anywhere in the views or logs.
		if len(v.Result.ChangedPaths) != len(w.Changes) || v.Result.Diffstat == nil || *v.Result.Diffstat != *d {
			t.Fatalf("mirrors %+v", v.Result)
		}
		view, _ := json.Marshal(v)
		logs, err := r.dep.Client.TaskLogs(context.Background(), v.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(view, []byte(sentinel)) || bytes.Contains(logs.Data, []byte(sentinel)) || strings.Contains(r.a.Logs.String(), sentinel) {
			t.Fatal("a file sentinel reached a view or log")
		}
	})
	t.Run("bounded", func(t *testing.T) {
		v := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Many: "many/" + strings.Repeat("p", 40), Count: 300})})
		w := publishedWs(t, v)
		b, _ := json.Marshal(w)
		if !w.ChangesTruncated || len(w.Changes) == 0 || len(w.Changes) > contract.MaxWorkspaceChanges || len(b) > contract.MaxWorkspaceDTOBytes ||
			w.Diffstat.Added != 300 || w.NextAfter == nil || *w.NextAfter != w.Changes[len(w.Changes)-1].PathBase64 {
			t.Fatalf("bounded: %d rows %d bytes truncated %v next %v diffstat %+v", len(w.Changes), len(b), w.ChangesTruncated, w.NextAfter, w.Diffstat)
		}
		if !v.Result.ChangedPathsTruncated || v.Result.ChangedPathsNextAfter == nil || *v.Result.ChangedPathsNextAfter != *w.NextAfter {
			t.Fatalf("mirrored truncation %+v", v.Result)
		}
	})
	t.Run("matrix", func(t *testing.T) {
		wsDelegate(t, "./internal/taskworkspace", "^TestMetadata$", "TestMetadata")
		wsDelegate(t, "./internal/contract", "^TestTaskWorkspaceContract$", "TestTaskWorkspaceContract")
	})
}

// FP-8: cancellation and recovery.
func TestTaskWorkspaceRecovery(t *testing.T) {
	r := sharedWsRig(t)
	ctx := context.Background()
	name, inst := r.workspace(t)
	r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("f\n")})
	t.Run("kill-before-push", func(t *testing.T) {
		b := newBarrier(t)
		v := r.running(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "p", Data: "p"},
			fakeadapter.WorkspaceOp{Touch: b.path("w")}, fakeadapter.WorkspaceOp{WaitFor: b.path("never")})}, b, "w")
		syscall.Kill(r.a.PID(), syscall.SIGKILL)
		if err := r.dep.Restart(ctx, r.a); err != nil {
			t.Fatal(err)
		}
		done := r.awaitResolved(t, v.TaskID)
		if done.State != contract.TaskLost || done.Result.Workspace.Publication != contract.PublicationNotApplicable {
			t.Fatalf("killed: %s %+v", done.State, done.Result.Workspace)
		}
		r.noRef(t, name, inst, v.TaskID)
		eventually(t, "work and slot released", func() bool {
			_, err := os.Stat(taskDir(r.a, v.TaskID))
			return os.IsNotExist(err) && r.inflight(t, "ws-a") == 0
		})
		// The node keeps working: a fresh task publishes.
		publishedWs(t, r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{}))
	})
	t.Run("cancel-while-held", func(t *testing.T) {
		b := newBarrier(t)
		v := r.running(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("w")},
			fakeadapter.WorkspaceOp{WaitFor: b.path("never")})}, b, "w")
		r.dep.Client.CancelTask(ctx, v.TaskID)
		done := r.awaitResolved(t, v.TaskID)
		if done.State != contract.TaskCancelled {
			t.Fatalf("cancel %s", done.State)
		}
		eventually(t, "cancelled work removed", func() bool { _, err := os.Stat(taskDir(r.a, v.TaskID)); return os.IsNotExist(err) })
	})
	// Restart after a durable push with its reply withheld, and the plane
	// restart between its two stores, through the deterministic crash
	// hooks of the owning packages (real persistence, no production
	// switch).
	t.Run("crash-boundaries", func(t *testing.T) {
		wsDelegate(t, "./internal/taskpublication", "^TestPublicationCrash$", "TestPublicationCrash")
		wsDelegate(t, "./internal/sidecar", "^TestTaskWorkspaceSession$/^restart$", "TestTaskWorkspaceSession/restart")
	})
}

// FP-9: same-base parallel isolation (AC-WS-2), with an empty PATH too.
func TestTaskWorkspaceIsolation(t *testing.T) {
	r := sharedWsRig(t)
	isolation := func(t *testing.T, role string, s *workersmoke.Sidecar) {
		name, inst := r.workspace(t)
		base := r.seed(t, name, inst, "", map[string]tfile{"shared.txt": tr("base\n"), "keep.txt": tr("k\n")})
		r.seed(t, name, inst, "other", map[string]tfile{"o": tr("o")})
		before := r.refs(t, name, inst)
		b := newBarrier(t)
		script := func(me string, extra fakeadapter.WorkspaceOp) fakeadapter.WorkspaceScript {
			return fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "shared.txt", Data: me}, fakeadapter.WorkspaceOp{Write: me + "-only.txt", Data: me},
				fakeadapter.WorkspaceOp{Touch: b.path(me)}, fakeadapter.WorkspaceOp{WaitFor: b.path("go")}, extra)}
		}
		va, err := r.wsDispatch(t, role, sp(name), nil, nil, script("a", fakeadapter.WorkspaceOp{GitCommit: "a rewrites its repo", GitBranch: "mine"}))
		if err != nil {
			t.Fatal(err)
		}
		vb, err := r.wsDispatch(t, role, sp(name), nil, nil, script("b", fakeadapter.WorkspaceOp{RemoveGit: true}))
		if err != nil {
			t.Fatal(err)
		}
		b.await(t, "a")
		b.await(t, "b")
		// Both children run at once; the disposable cache entry is evicted
		// under them.
		os.RemoveAll(filepath.Join(s.State, "workspace-cache", inst))
		b.release(t, "go")
		results := map[string]string{}
		for me, v := range map[string]contract.TaskView{"a": r.await(t, va.TaskID), "b": r.await(t, vb.TaskID)} {
			w := publishedWs(t, v)
			got, c := r.files(t, name, inst, *w.Commit)
			wantResultCommit(t, c, v, contract.TaskSucceeded, plumbing.NewHash(base))
			want := specOf(map[string]tfile{"shared.txt": tr(me), me + "-only.txt": tr(me), "keep.txt": tr("k\n")})
			if err := testkit.EqualFiles(got, want); err != nil {
				t.Fatalf("%s: %v", me, err)
			}
			results[me] = *w.Commit
		}
		if results["a"] == results["b"] {
			t.Fatal("the two tasks share a result")
		}
		after := r.refs(t, name, inst)
		for ref, h := range before {
			if after[ref] != h {
				t.Fatalf("%s moved: %s -> %s", ref, h, after[ref])
			}
		}
		if len(after) != len(before)+2 {
			t.Fatalf("refs %v (before %v)", after, before)
		}
	}
	t.Run("AC-WS-2", func(t *testing.T) { isolation(t, "ws-a", r.a) })
	t.Run("empty-PATH", func(t *testing.T) { isolation(t, "ws-z", r.z) })
	t.Run("no-git-subprocess", func(t *testing.T) {
		wsDelegate(t, "./internal/taskworkspace", "^TestIsolation$", "TestIsolation")
	})
}
