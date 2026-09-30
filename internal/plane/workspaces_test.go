package plane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	pclient "github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// wsPlane is an in-process plane run with a verified client.
type wsPlane struct {
	root string
	url  string
	cl   *pclient.Client
	hc   *http.Client
	s    *served
}

func startWsPlane(t *testing.T) *wsPlane {
	t.Helper()
	root := newRoot(t)
	d := testDeps(t)
	mustInit(t, d, root, "127.0.0.1")
	s := serveBG(t, d, RunOptions{StateDir: root})
	caPEM := certPEM(readCA(t, root).Raw)
	url := "https://" + s.addr.String()
	cl, err := pclient.New(url, pclient.Trust{CAPEM: caPEM})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return &wsPlane{root: root, url: url, cl: cl, hc: client(t, readCA(t, root), "127.0.0.1", 0), s: s}
}

// raw sends one request with the protocol header.
func (p *wsPlane) raw(t *testing.T, method, path, ctype string, body []byte, hdr ...bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, p.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(hdr) == 0 || hdr[0] {
		req.Header.Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// UT-7: the control API's routes, strict bodies and queries, status codes
// and the verified client's parity, over the real plane.
func TestWorkspaceControlAPI(t *testing.T) {
	ctx := context.Background()
	p := startWsPlane(t)
	v, err := p.cl.CreateWorkspace(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.cl.CreateWorkspace(ctx, "alpha"); codeOf(err) != contract.CodeConflict {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := p.cl.ShowWorkspace(ctx, "alpha")
	if err != nil || got.Instance != v.Instance {
		t.Fatalf("show %+v %v", got, err)
	}
	for _, c := range []struct {
		method, path, ctype, body string
		status                    int
	}{
		{"POST", "/api/v1/workspaces", "application/json", `{"name":"Bad"}`, 400},
		{"POST", "/api/v1/workspaces", "application/json", `{"name":"b","x":1}`, 400},
		{"POST", "/api/v1/workspaces", "text/plain", `{"name":"b"}`, 400},
		{"POST", "/api/v1/workspaces", "application/json", `{"name":"` + strings.Repeat("a", 70<<10) + `"}`, 400},
		{"POST", "/api/v1/workspaces?x=1", "application/json", `{"name":"b"}`, 400},
		{"PUT", "/api/v1/workspaces", "", "", 400},
		{"GET", "/api/v1/workspaces?limit=0", "", "", 400},
		{"GET", "/api/v1/workspaces?limit=101", "", "", 400},
		{"GET", "/api/v1/workspaces?limit=1&limit=2", "", "", 400},
		{"GET", "/api/v1/workspaces?bogus=1", "", "", 400},
		{"GET", "/api/v1/workspaces/Alpha", "", "", 400},
		{"GET", "/api/v1/workspaces/al%2Fpha", "", "", 400},
		{"GET", "/api/v1/workspaces/nope", "", "", 404},
		{"GET", "/api/v1/workspaces/alpha/bogus", "", "", 404},
		{"POST", "/api/v1/workspaces/alpha", "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/prune", "", "", 400},
		{"DELETE", "/api/v1/workspaces/alpha", "application/json", `{"instance":"` + strings.Repeat("0", 32) + `"}`, 409},
		{"DELETE", "/api/v1/workspaces/alpha", "application/json", `{}`, 400},
		{"POST", "/api/v1/workspaces/alpha/prune", "application/json", `{"instance":"` + v.Instance + `","before":"2026-09-30T12:00:00"}`, 400},
		{"POST", "/api/v1/workspaces/alpha/refs/set", "application/json", `{"instance":"` + v.Instance + `","branch":"Main","expected":"absent","target":"x"}`, 400},
		{"POST", "/api/v1/workspaces/alpha/refs/set", "application/json", `{"instance":"` + v.Instance + `","branch":"main","expected":"absent","target":"nope"}`, 404},
		{"POST", "/api/v1/workspaces/alpha/refs/set", "application/json", "{\"instance\":\"" + v.Instance + "\",\"branch\":\"ma\xffin\",\"expected\":\"absent\",\"target\":\"x\"}", 400},
		{"GET", "/api/v1/workspaces/alpha/status?after=refs/heads/main", "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/status?after=refs%2Fheads%2FMain&instance=" + v.Instance + "&generation=" + v.Generation, "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/status?after=refs%2Fheads%2Fma%FFin&instance=" + v.Instance + "&generation=" + v.Generation, "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/status?after=%ZZ&instance=" + v.Instance + "&generation=" + v.Generation, "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/diff?base=empty", "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/diff?base=empty&target=main&after=YQ%3D%3D&instance=" + v.Instance + "&generation=" + v.Generation, "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/diff?base=empty&target=" + strings.Repeat("1", 40) + "&after=YQ&instance=" + v.Instance + "&generation=" + v.Generation, "", "", 400},
		{"GET", "/api/v1/workspaces/alpha/diff?base=empty&target=main", "", "", 404},
	} {
		status, body := p.raw(t, c.method, c.path, c.ctype, []byte(c.body))
		if status != c.status || !strings.Contains(body, `"error"`) {
			t.Fatalf("%s %s %s = %d %s", c.method, c.path, c.body, status, body)
		}
	}
	if status, _ := p.raw(t, "GET", "/api/v1/workspaces", "", nil, false); status != 400 {
		t.Fatalf("missing protocol header = %d", status)
	}
	if status, body := p.raw(t, "GET", "/api/v1/workspaces/alpha", "", nil); status != 200 || strings.Contains(body, `"version"`) {
		t.Fatalf("show wire %d %s", status, body)
	}
	// Git transport over the plane's real TLS server with a scoped client.
	caPEM := certPEM(readCA(t, p.root).Raw)
	hc, err := testkit.GitHTTPClient(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	remote := testkit.GitRemote{URL: p.url + "/ws/alpha.git", Instance: v.Instance, HTTP: hc}
	s := testkit.NewMemoryStore()
	c0, err := testkit.CommitFiles(s, map[string]testkit.FileSpec{"a": {Mode: 0o100644, Content: []byte("a")}}, nil, "c0")
	if err != nil {
		t.Fatal(err)
	}
	if res := remote.Push(ctx, s, "refs/heads/main", plumbing.ZeroHash, c0); !res.OK() {
		t.Fatalf("push %+v", res)
	}
	st, err := p.cl.WorkspaceStatus(ctx, "alpha", pclient.StatusPage{Limit: 10})
	if err != nil || len(st.Refs) != 1 || st.Refs[0].Commit != c0.String() {
		t.Fatalf("status %+v %v", st, err)
	}
	r, err := p.cl.SetWorkspaceRef(ctx, "alpha", contract.WorkspaceRefSetRequest{Instance: v.Instance, Branch: "refs/heads/topic/x", Expected: contract.ExpectedAbsent, Target: strp("main")})
	if err != nil || r.Ref != "refs/heads/topic/x" {
		t.Fatalf("ref set %+v %v", r, err)
	}
	st, err = p.cl.WorkspaceStatus(ctx, "alpha", pclient.StatusPage{Limit: 1})
	if err != nil || st.NextAfter == nil {
		t.Fatalf("page 1 %+v %v", st, err)
	}
	st2, err := p.cl.WorkspaceStatus(ctx, "alpha", pclient.StatusPage{After: *st.NextAfter, Instance: st.Instance, Generation: st.Generation, Limit: 1})
	if err != nil || len(st2.Refs) != 1 || st2.Refs[0].Name != "refs/heads/topic/x" {
		t.Fatalf("page 2 %+v %v", st2, err)
	}
	d, err := p.cl.WorkspaceDiff(ctx, "alpha", pclient.DiffPage{Base: "empty", Target: "topic/x", Limit: 10})
	if err != nil || len(d.Changes) != 1 || d.Changes[0].Kind != contract.ChangeAdded {
		t.Fatalf("diff %+v %v", d, err)
	}
	pr, err := p.cl.PruneWorkspace(ctx, "alpha", v.Instance, "2026-09-30T14:00:00+02:00")
	if err != nil || pr.Name != "alpha" {
		t.Fatalf("prune %+v %v", pr, err)
	}
	l, err := p.cl.ListWorkspaces(ctx, "", 10)
	if err != nil || len(l.Workspaces) != 1 {
		t.Fatalf("list %+v %v", l, err)
	}
	if _, err := p.cl.RemoveWorkspace(ctx, "alpha", v.Instance); err != nil {
		t.Fatal(err)
	}
	if status, _ := p.raw(t, "GET", "/ws/alpha.git/info/refs?service=git-upload-pack", "", nil); status != 404 {
		t.Fatalf("removed transport URL = %d", status)
	}
	// The plane's shutdown cancels and joins workspace handlers.
	if err := p.s.stop(t); err != context.Canceled {
		t.Fatalf("stop: %v", err)
	}
}

func strp(s string) *string { return &s }

// UT-2: the cleaned and canonical state roots are bounded at 256 bytes
// before initialization and run write anything, for explicit and
// symlink-expanded roots alike; a short alias to an overlong canonical root
// does not bypass the bound.
func TestStateRootBudget(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(n int) string {
		root := base
		for len(root) < n {
			root = filepath.Join(root, strings.Repeat("r", min(n-len(root)-1, 200)))
		}
		return root
	}
	ok := mk(256)
	if len(ok) != 256 {
		t.Fatalf("root %d", len(ok))
	}
	d := testDeps(t)
	st, err := d.init(bg, initOpts(ok, "127.0.0.1"))
	if err != nil || st.StateDir != ok {
		t.Fatalf("256-byte root: %v", err)
	}
	long := mk(257)
	if _, err := d.init(bg, initOpts(long, "127.0.0.1")); codeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), "plane state root exceeds 256 bytes; choose a shorter --state-dir") {
		t.Fatalf("257-byte root: %v", err)
	}
	if _, err := os.Lstat(long); err == nil {
		t.Fatal("a refused root was created")
	}
	if err := d.run(bg, RunOptions{StateDir: long}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("run 257: %v", err)
	}
	// A short alias whose canonical form is 257 bytes.
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(long, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := d.init(bg, initOpts(filepath.Join(alias, "state"), "127.0.0.1")); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("alias to an overlong root: %v", err)
	}
	if es, _ := os.ReadDir(long); len(es) != 0 {
		t.Fatalf("alias init wrote %v", es)
	}
	if c, err := canonicalRoot(filepath.Join(alias, "x", "y")); err != nil || c != filepath.Join(long, "x", "y") {
		t.Fatalf("canonical %q %v", c, err)
	}
}

// Workspace state admission: an existing installation without workspaces/
// stays valid; workspaces/ alone is partial state; corrupt workspace state
// fails startup and plane status instead of reading as an empty registry.
func TestWorkspaceStateAdmission(t *testing.T) {
	root := newRoot(t)
	d := testDeps(t)
	mustInit(t, d, root, "127.0.0.1")
	if _, err := d.inspect(bg, root); err != nil {
		t.Fatalf("existing installation: %v", err)
	}
	bare := newRoot(t)
	os.MkdirAll(filepath.Join(bare, workspacesName), 0o700)
	if _, err := d.inspect(bg, bare); codeOf(err) != contract.CodeConflict || !strings.Contains(err.Error(), "workspace store") {
		t.Fatalf("workspaces without trust: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, workspacesName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.inspect(bg, root); codeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("group-readable workspaces/: %v", err)
	}
	os.Chmod(filepath.Join(root, workspacesName), 0o700)
	os.WriteFile(filepath.Join(root, workspacesName, "Junk"), nil, 0o600)
	if _, err := d.inspect(bg, root); codeOf(err) != contract.CodeConflict {
		t.Fatalf("corrupt workspace state in status: %v", err)
	}
	if err := d.run(bg, RunOptions{StateDir: root}); codeOf(err) != contract.CodeConflict {
		t.Fatalf("corrupt workspace state at startup: %v", err)
	}
}
