package workspacetransfer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 10c (UT-C2): a task pull's paired expected instance and
// commit guard the transfer itself: validated before any plane call,
// compared with the initial instance and the resolved canonical task ref
// before any fetch, and the final instance check runs even when the
// closure is already local. A missing or recreated workspace is
// conflict/workspace_instance_mismatch at every check; explicit pulls keep
// their landed errors. Everything runs on the in-memory plane.

const pullTask = "t_ffffffffffffffffffffffffffffffff"

// taskSeed is a memSeed whose workspace holds pullTask's ref at second.
func taskSeed(t *testing.T) (*memSeed, string) {
	t.Helper()
	s := newMemSeed(t)
	s.p.setRef("ws", contract.TaskRefPrefix+pullTask, s.second)
	return s, s.p.ws["ws"].instance
}

func taskPull(t *testing.T, d *deps, p Plane, env Env, inst, commit, path string) (contract.WorkspacePullResult, error) {
	t.Helper()
	return d.pull(context.Background(), Options{GOOS: hostGOOS(), Env: env, Cwd: "/", Plane: p},
		PullRequest{Name: "ws", Ref: contract.TaskRefPrefix + pullTask, Path: path, PathSet: true, ExpectedInstance: inst, ExpectedCommit: commit})
}

func wantMismatch(t *testing.T, what string, err error) {
	t.Helper()
	ce, _ := err.(*contract.Error)
	if contract.CodeOf(err) != contract.CodeConflict || ce == nil || ce.Details["reason"] != contract.ReasonWorkspaceInstanceMismatch {
		t.Fatalf("%s: %v, want conflict/workspace_instance_mismatch", what, err)
	}
}

func TestPullTaskPrecondition(t *testing.T) {
	s, inst := taskSeed(t)
	local := "refs/callsheet/ws/tasks/" + pullTask
	// Into a dirty existing repository: objects and exactly the task
	// observation ref (canonical selector), nothing else.
	dst := newRepo(t, map[string]fspec{"mine": reg("local")})
	dst.writeFile("mine", reg("uncommitted"))
	before := repoFingerprint(t, dst.root)
	res, err := taskPull(t, fastDeps(), s.p, s.env, inst, s.second.String(), dst.root)
	if err != nil || res.Selector != contract.TaskRefPrefix+pullTask || res.Commit != s.second.String() || res.Instance != inst || *res.LocalRef != local {
		t.Fatalf("task pull %+v %v", res, err)
	}
	if readRef(t, dst.root, local) != s.second.String() || repoFingerprint(t, dst.root) != before {
		t.Fatal("the task pull changed more than objects and its ref")
	}
	// Into a new folder.
	out := filepath.Join(tempDir(t), "out")
	if res, err := taskPull(t, fastDeps(), s.p, s.env, inst, s.second.String(), out); err != nil || res.DestinationKind != contract.KindFolder {
		t.Fatalf("folder %+v %v", res, err)
	}
	// An invalid pair is refused before any plane call.
	fetches := s.p.fetches
	for _, req := range []PullRequest{
		{Name: "ws", Ref: contract.TaskRefPrefix + pullTask, ExpectedInstance: inst},
		{Name: "ws", Ref: contract.TaskRefPrefix + pullTask, ExpectedCommit: s.second.String()},
		{Name: "ws", Ref: contract.TaskRefPrefix + pullTask, ExpectedInstance: "x", ExpectedCommit: s.second.String()},
		{Name: "ws", Ref: contract.TaskRefPrefix + pullTask, ExpectedInstance: inst, ExpectedCommit: "abc"},
		{Name: "ws", Ref: "refs/heads/main", ExpectedInstance: inst, ExpectedCommit: s.second.String()},
		{Name: "ws", Ref: s.second.String(), ExpectedInstance: inst, ExpectedCommit: s.second.String()},
	} {
		req.Path, req.PathSet = dst.root, true
		if _, err := fastDeps().pull(context.Background(), Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}, req); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	// Before any fetch: another instance, another hash, a pruned ref.
	other := strings.Repeat("9", 32)
	wantMismatchPull := func(what string, d *deps, p Plane, inst, commit string) {
		t.Helper()
		fresh := newRepo(t, map[string]fspec{"x": reg("x")})
		_, err := taskPull(t, d, p, s.env, inst, commit, fresh.root)
		wantMismatch(t, what, err)
		if readRef(t, fresh.root, local) != "" {
			t.Fatalf("%s: a ref was written", what)
		}
	}
	wantMismatchPull("initial instance", fastDeps(), s.p, other, s.second.String())
	_, err = taskPull(t, fastDeps(), s.p, s.env, inst, s.first.String(), newRepo(t, nil).root)
	if ce, _ := err.(*contract.Error); contract.CodeOf(err) != contract.CodeConflict || ce.Details["reason"] != contract.ReasonTaskResultUnavailable {
		t.Fatalf("other hash: %v", err)
	}
	s.p.setRef("ws", contract.TaskRefPrefix+pullTask, plumbing.ZeroHash)
	if _, err := taskPull(t, fastDeps(), s.p, s.env, inst, s.second.String(), newRepo(t, nil).root); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("pruned: %v", err)
	}
	if s.p.fetches != fetches {
		t.Fatalf("a refused task pull fetched (%d)", s.p.fetches-fetches)
	}
	// A removed workspace, and recreations after resolution and after the
	// fetch.
	s, inst = taskSeed(t)
	s.p.mu.Lock()
	delete(s.p.ws, "ws")
	s.p.mu.Unlock()
	wantMismatchPull("removed", fastDeps(), s.p, inst, s.second.String())
	for _, stage := range []string{"pull-resolved", "pull-fetched"} {
		s, inst = taskSeed(t)
		d := fastDeps()
		d.hook = func(st string) {
			if st == stage {
				s.p.create("ws")
			}
		}
		wantMismatchPull("recreated at "+stage, d, s.p, inst, s.second.String())
	}
	// A fetch failure on an unchanged instance is never reported as a
	// mismatch: the commit became unreachable after resolution.
	s, inst = taskSeed(t)
	d0 := fastDeps()
	d0.hook = func(st string) {
		if st == "pull-resolved" {
			s.p.setRef("ws", "refs/heads/main", plumbing.ZeroHash)
			s.p.setRef("ws", contract.TaskRefPrefix+pullTask, s.first)
		}
	}
	_, err = taskPull(t, d0, s.p, s.env, inst, s.second.String(), newRepo(t, nil).root)
	if contract.CodeOf(err) != contract.CodeConflict || contract.TransferReason(err) == contract.ReasonWorkspaceInstanceMismatch {
		t.Fatalf("unreachable after resolution: %v", err)
	}
	// The final check runs even when the closure is already local: no
	// fetch happens, the recreation is still refused.
	s, inst = taskSeed(t)
	have := newRepo(t, map[string]fspec{"x": reg("x")})
	if _, err := s.pull(t, fastDeps(), "main", have.root); err != nil {
		t.Fatal(err)
	}
	fetches = s.p.fetches
	d := fastDeps()
	d.hook = func(st string) {
		if st == "pull-resolved" {
			s.p.create("ws")
		}
	}
	_, err = taskPull(t, d, s.p, s.env, inst, s.second.String(), have.root)
	wantMismatch(t, "local closure", err)
	if s.p.fetches != fetches || readRef(t, have.root, local) != "" {
		t.Fatalf("local closure: %d fetches, ref %q", s.p.fetches-fetches, readRef(t, have.root, local))
	}
	// An explicit pull keeps its plain conflict (no task reason).
	s, _ = taskSeed(t)
	d = fastDeps()
	d.hook = func(st string) {
		if st == "pull-fetched" {
			s.p.create("ws")
		}
	}
	_, err = s.pull(t, d, contract.TaskRefPrefix+pullTask, newRepo(t, nil).root)
	if ce, _ := err.(*contract.Error); contract.CodeOf(err) != contract.CodeConflict || ce.Details["reason"] != nil {
		t.Fatalf("explicit recreation: %v", err)
	}
}

// scanPlane changes the workspace after the first status page.
type scanPlane struct {
	*memPlane
	pages  int
	change func()
}

func (p *scanPlane) WorkspaceStatus(ctx context.Context, name string, pg client.StatusPage) (contract.WorkspaceStatusResponse, error) {
	if p.pages == 1 {
		p.change()
	}
	p.pages++
	return p.memPlane.WorkspaceStatus(ctx, name, pg)
}

// Instance changes observed while the selector is resolved across status
// pages: a recreation (or removal) is a mismatch, a generation change
// alone keeps the plane's conflict; a recreation before the first page is
// a mismatch too.
func TestPullTaskSelectorScan(t *testing.T) {
	seed := func() (*memSeed, string) {
		s, inst := taskSeed(t)
		// 110 earlier task refs put pullTask on the second page.
		for i := range 110 {
			s.p.setRef("ws", contract.TaskRefPrefix+fmt.Sprintf("t_%032x", i+1), s.first)
		}
		return s, inst
	}
	s, inst := seed()
	sp := &scanPlane{memPlane: s.p, change: func() {}}
	if res, err := taskPull(t, fastDeps(), sp, s.env, inst, s.second.String(), newRepo(t, nil).root); err != nil || sp.pages != 2 || res.Commit != s.second.String() {
		t.Fatalf("second page %+v %v (%d pages)", res, err, sp.pages)
	}
	for name, change := range map[string]func(p *memPlane){
		"recreated": func(p *memPlane) { p.create("ws") },
		"removed": func(p *memPlane) {
			p.mu.Lock()
			delete(p.ws, "ws")
			p.mu.Unlock()
		},
	} {
		s, inst := seed()
		sp := &scanPlane{memPlane: s.p, change: func() { change(s.p) }}
		_, err := taskPull(t, fastDeps(), sp, s.env, inst, s.second.String(), newRepo(t, nil).root)
		wantMismatch(t, name, err)
	}
	s, inst = seed()
	sp = &scanPlane{memPlane: s.p, change: func() { s.p.setRef("ws", "refs/heads/other", s.first) }}
	_, err := taskPull(t, fastDeps(), sp, s.env, inst, s.second.String(), newRepo(t, nil).root)
	if ce, _ := err.(*contract.Error); contract.CodeOf(err) != contract.CodeConflict || ce.Details["reason"] != nil {
		t.Fatalf("generation change: %v", err)
	}
	// Recreated between the initial ShowWorkspace and the first page.
	s, inst = seed()
	sp = &scanPlane{memPlane: s.p, change: func() {}, pages: 1}
	sp.change = func() { s.p.create("ws") }
	_, err = taskPull(t, fastDeps(), sp, s.env, inst, s.second.String(), newRepo(t, nil).root)
	wantMismatch(t, "recreated before the scan", err)
}
