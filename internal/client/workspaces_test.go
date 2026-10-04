package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// UT-7: the verified workspace client: inputs are validated before any
// request (nothing is sent for an invalid name, instance, cutoff or
// selector), requests carry the documented method, path, query and body,
// answers are decoded strictly and must name what was asked, and errors,
// cancellation and disconnection surface as contract errors (never a
// stale or partial answer).
func TestWorkspaceClient(t *testing.T) {
	ca := newTestCA(t)
	inst, gen := strings.Repeat("a", 32), strings.Repeat("b", 32)
	h := strings.Repeat("c", 40)
	view := func(name string) string {
		return `{"name":"` + name + `","instance":"` + inst + `","created_at":"2026-09-30T12:00:00Z","generation":"` + gen + `","default_branch":"refs/heads/main","size_bytes":10,"retention":{"automatic_prune":false,"quota_bytes":null}}`
	}
	type sent struct{ method, uri, body string }
	// mu guards the routes and the record: a handler of an abandoned
	// (cancelled or dropped) request may still run while the test sets the
	// next route.
	var mu sync.Mutex
	var last sent
	var hits int
	routes := map[string]http.HandlerFunc{}
	s := startServer(t, ca.leaf(t, false), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		last, hits = sent{r.Method, r.URL.RequestURI(), string(b)}, hits+1
		snapshot := make(map[string]http.HandlerFunc, len(routes))
		for k, v := range routes {
			snapshot[k] = v
		}
		mu.Unlock()
		planeHandler(ca.pem, snapshot).ServeHTTP(w, r)
	}))
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	set := func(path string, h http.HandlerFunc) {
		mu.Lock()
		routes[path] = h
		mu.Unlock()
	}
	got := func() sent {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
	ctx := context.Background()
	set(contract.PathWorkspaces, jsonRoute("6", 201, view("alpha")))
	if v, err := c.CreateWorkspace(ctx, "alpha"); err != nil || v.Name != "alpha" || got().body != `{"name":"alpha"}` {
		t.Fatalf("create %+v %v %+v", v, err, got())
	}
	set(contract.PathWorkspaces, jsonRoute("6", 201, view("beta")))
	if _, err := c.CreateWorkspace(ctx, "alpha"); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("answer naming another workspace: %v", err)
	}
	set(contract.PathWorkspaces, jsonRoute("6", 200, view("alpha")))
	if _, err := c.CreateWorkspace(ctx, "alpha"); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("unexpected status: %v", err)
	}
	set(contract.PathWorkspaces, jsonRoute("6", 409, `{"error":{"code":"conflict","message":"exists"}}`))
	if _, err := c.CreateWorkspace(ctx, "alpha"); codeOf(err) != contract.CodeConflict {
		t.Fatalf("conflict: %v", err)
	}
	// Invalid inputs never reach the plane.
	before := count()
	target := "main"
	for _, err := range []error{
		errOf(c.CreateWorkspace(ctx, "Alpha")),
		errOf(c.ShowWorkspace(ctx, "")),
		errOf(c.ListWorkspaces(ctx, "", 0)),
		errOf(c.RemoveWorkspace(ctx, "alpha", "x")),
		errOf(c.PruneWorkspace(ctx, "alpha", inst, "2026-09-30")),
		errOf(c.SetWorkspaceRef(ctx, "alpha", contract.WorkspaceRefSetRequest{Instance: inst, Branch: "Main", Expected: "absent", Target: &target})),
		errOf(c.WorkspaceStatus(ctx, "alpha", StatusPage{After: "refs/heads/main", Limit: 1})),
		errOf(c.WorkspaceStatus(ctx, "alpha", StatusPage{After: "main", Instance: inst, Generation: gen, Limit: 1})),
		errOf(c.WorkspaceDiff(ctx, "alpha", DiffPage{Base: "empty", Target: "empty2~1", Limit: 1})),
		errOf(c.WorkspaceDiff(ctx, "alpha", DiffPage{Base: "main", Target: h, After: "YQ==", Instance: inst, Generation: gen, Limit: 1})),
	} {
		if codeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("invalid input: %v", err)
		}
	}
	if count() != before {
		t.Fatal("an invalid input reached the plane")
	}
	// Queries are percent-encoded; answers must match the continuation.
	set(contract.PathWorkspaces+"/alpha/status", jsonRoute("6", 200, `{"name":"alpha","instance":"`+inst+`","generation":"`+gen+`","default_branch":"refs/heads/main","refs":[{"name":"refs/heads/x","commit":"`+h+`","published_at":null}],"next_after":null}`))
	if _, err := c.WorkspaceStatus(ctx, "alpha", StatusPage{After: "refs/heads/main", Instance: inst, Generation: gen, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if u := got().uri; !strings.Contains(u, "after=refs%2Fheads%2Fmain") || !strings.Contains(u, "limit=5") {
		t.Fatalf("status query %q", u)
	}
	if _, err := c.WorkspaceStatus(ctx, "alpha", StatusPage{After: "refs/heads/main", Instance: inst, Generation: strings.Repeat("d", 32), Limit: 5}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("mismatched generation: %v", err)
	}
	set(contract.PathWorkspaces+"/alpha/diff", jsonRoute("6", 200, `{"name":"alpha","instance":"`+inst+`","generation":"`+gen+`","base_commit":null,"target_commit":"`+h+`","changes":[],"next_after":null}`))
	if _, err := c.WorkspaceDiff(ctx, "alpha", DiffPage{Base: "empty", Target: h, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WorkspaceDiff(ctx, "alpha", DiffPage{Base: h, Target: h, Limit: 5}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("base mismatch: %v", err)
	}
	if _, err := c.WorkspaceDiff(ctx, "alpha", DiffPage{Base: "empty", Target: strings.Repeat("e", 40), Limit: 5}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("target mismatch: %v", err)
	}
	// Remove sends DELETE with a JSON body; prune and ref set POST.
	set(contract.PathWorkspaces+"/alpha", jsonRoute("6", 200, `{"name":"alpha","instance":"`+inst+`","removed":true}`))
	if _, err := c.RemoveWorkspace(ctx, "alpha", inst); err != nil || got().method != http.MethodDelete || got().body != `{"instance":"`+inst+`"}` {
		t.Fatalf("remove %v %+v", err, got())
	}
	set(contract.PathWorkspaces+"/alpha/prune", jsonRoute("6", 200, `{"name":"alpha","instance":"`+inst+`","generation":"`+gen+`","removed_task_refs":1,"reclaimed_bytes":5,"size_bytes":10}`))
	if r, err := c.PruneWorkspace(ctx, "alpha", inst, "2026-09-30T12:00:00Z"); err != nil || r.RemovedTaskRefs != 1 {
		t.Fatalf("prune %+v %v", r, err)
	}
	set(contract.PathWorkspaces+"/alpha/refs/set", jsonRoute("6", 200, `{"name":"alpha","instance":"`+inst+`","generation":"`+gen+`","ref":"refs/heads/x","old_commit":null,"new_commit":"`+h+`"}`))
	f := false
	if _, err := c.SetWorkspaceRef(ctx, "alpha", contract.WorkspaceRefSetRequest{Instance: inst, Branch: "x", Expected: "absent", Target: &target, Delete: &f}); err != nil || strings.Contains(got().body, "delete") {
		t.Fatalf("ref set %v %+v", err, got())
	}
	if _, err := c.SetWorkspaceRef(ctx, "alpha", contract.WorkspaceRefSetRequest{Instance: inst, Branch: "y", Expected: "absent", Target: &target}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("answer for another ref: %v", err)
	}
	set(contract.PathWorkspaces, jsonRoute("6", 200, `{"workspaces":[],"next_after":null}`))
	if r, err := c.ListWorkspaces(ctx, "a b", 3); err != nil || len(r.Workspaces) != 0 || !strings.Contains(got().uri, "after=a+b") {
		t.Fatalf("list %+v %v %q", r, err, got().uri)
	}
	// An oversized answer is refused, never truncated.
	set(contract.PathWorkspaces+"/alpha", jsonRoute("6", 200, view("alpha")+strings.Repeat(" ", contract.MaxWorkspaceResponse)))
	if _, err := c.ShowWorkspace(ctx, "alpha"); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("oversized: %v", err)
	}
	// Cancellation (once the plane has the request) returns the caller's
	// error; a dropped connection is unavailable; neither yields an answer.
	entered, block := make(chan struct{}), make(chan struct{})
	var once sync.Once
	set(contract.PathWorkspaces+"/alpha", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		<-entered
		cancel()
	}()
	if _, err := c.ShowWorkspace(cctx, "alpha"); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	close(block)
	set(contract.PathWorkspaces+"/alpha", func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	})
	if _, err := c.ShowWorkspace(ctx, "alpha"); codeOf(err) != contract.CodeUnavailable {
		t.Fatalf("disconnect: %v", err)
	}
}

func errOf[T any](_ T, err error) error { return err }

func codeOf(err error) contract.Code { return contract.CodeOf(err) }
