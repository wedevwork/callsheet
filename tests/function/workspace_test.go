//go:build linux || darwin

package function

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 09a function tests: one top-level test per FP (FP-1..FP-10),
// in FP order, through the real CLI binary and the real stdio MCP server
// against a real plane subprocess, with go-git seeding and fetching over
// the plane's production TLS endpoint. Package-local contracts (failure
// injection, the raw receive-pack scenarios and captured storage paths)
// are delegated to the compiled internal/workspace test binary.

const wsPkg = "./internal/workspace"

// wsEnv is a plane subprocess with a CLI environment, an MCP session and a
// verified git client.
type wsEnv struct {
	t     *testing.T
	p     *mcpPlane
	cli   *planeCLI
	mcp   *mcpProc
	git   testkitHTTP
	extra []string
}

type testkitHTTP = testkit.GitRemote

// startWs starts a plane (state root root, or a temp one) whose CLI, MCP
// and plane processes all get env (PATH and HOME included).
func startWs(t *testing.T, root string, env []string) *wsEnv {
	t.Helper()
	bin := nodeBinary(t)
	home, cwd := t.TempDir(), t.TempDir()
	if env == nil {
		env = []string{"PATH=" + os.Getenv("PATH")}
	}
	cli := &planeCLI{bin: bin, home: home, cwd: cwd, env: append(append([]string{}, env...), "HOME="+home)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bind := ln.Addr().String()
	ln.Close()
	if root == "" {
		root = filepath.Join(t.TempDir(), "plane")
	}
	p := &mcpPlane{root: root, bind: bind, url: "https://" + bind, cli: cli}
	r := cli.run(t, "plane", "init", "--state-dir", root, "--bind", bind, "--san", "127.0.0.1")
	if r.code != 0 {
		t.Fatalf("plane init = %+v", r)
	}
	p.ca = filepath.Join(root, "pki", "ca.crt")
	t.Cleanup(func() {
		p.mu.Lock()
		cmd, done := p.proc, p.done
		p.mu.Unlock()
		if cmd == nil {
			return
		}
		cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(mcpWait):
			t.Errorf("plane run %d was not reaped", cmd.Process.Pid)
		}
	})
	p.start(t)
	pem, err := os.ReadFile(p.ca)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := testkit.GitHTTPClient(pem)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	e := &wsEnv{t: t, p: p, cli: cli, extra: env}
	e.git = testkit.GitRemote{HTTP: hc}
	m := startMCPProcEnv(t, env, p.trust()...)
	m.dispatch()
	m.initialize("workspace-test", "9.0.0")
	e.mcp = m
	return e
}

// run runs a CLI workspace command with trust flags.
func (e *wsEnv) run(args ...string) result {
	e.t.Helper()
	return e.cli.run(e.t, append(args, e.p.trust()...)...)
}

// ok runs a CLI command that must succeed.
func (e *wsEnv) ok(args ...string) string {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("%v = %+v", args, r)
	}
	return r.stdout
}

// jsonCLI runs a CLI command with --json and decodes its object.
func (e *wsEnv) jsonCLI(v any, args ...string) {
	e.t.Helper()
	leaf := 2
	if args[1] == "ref" {
		leaf = 3
	}
	out := e.ok(append(append(append([]string{}, args[:leaf]...), "--json"), args[leaf:]...)...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		e.t.Fatalf("%v: %v %q", args, err, out)
	}
}

// mcpJSON calls an MCP tool that must succeed and decodes its object.
func (e *wsEnv) mcpJSON(v any, tool string, args map[string]any) string {
	e.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	text := e.mcp.ok(tool, args)
	if err := json.Unmarshal([]byte(text), v); err != nil {
		e.t.Fatalf("%s: %v %q", tool, err, text)
	}
	return text
}

func (e *wsEnv) remote(name, instance string) testkit.GitRemote {
	r := e.git
	r.URL = e.p.url + "/ws/" + name + ".git"
	r.Instance = instance
	return r
}

// restart stops the plane (SIGTERM) and starts it again.
func (e *wsEnv) restart() {
	e.t.Helper()
	e.p.stop(e.t)
	e.p.start(e.t)
}

// wsFiles lists every path below the state root's workspaces/.
func wsFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.Walk(filepath.Join(root, "workspaces"), func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// ownedSize sums the regular files of a workspace directory.
func ownedSize(t *testing.T, root, name string) int64 {
	t.Helper()
	var n int64
	filepath.Walk(filepath.Join(root, "workspaces", name), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func seedStore(t *testing.T) (*memory.Storage, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	s := testkit.NewMemoryStore()
	bin := make([]byte, 1024)
	for i := range bin {
		bin[i] = byte(i*31 + 7)
	}
	files := map[string]testkit.FileSpec{
		"README.md":  {Mode: filemode.Regular, Content: []byte("# ws\n")},
		"bin/run.sh": {Mode: filemode.Executable, Content: []byte("#!/bin/sh\necho hi\n")},
		"data.bin":   {Mode: filemode.Regular, Content: bin},
		"link":       {Mode: filemode.Symlink, Content: []byte("README.md")},
	}
	c0, err := testkit.CommitFiles(s, files, nil, "c0")
	if err != nil {
		t.Fatal(err)
	}
	files["next.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("next\n")}
	c1, err := testkit.CommitFiles(s, files, []plumbing.Hash{c0}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	return s, c0, c1
}

func pushOK(t *testing.T, r testkit.GitRemote, s *memory.Storage, ref string, old, new plumbing.Hash) {
	t.Helper()
	res := r.Push(context.Background(), s, ref, old, new)
	if !res.OK() {
		t.Fatalf("push %s: err=%v report=%+v", ref, res.Err, res.Report)
	}
}

// statusRefs reads all status rows through the CLI (JSON).
func (e *wsEnv) statusRefs(name string) map[string]string {
	e.t.Helper()
	var st contract.WorkspaceStatusResponse
	e.jsonCLI(&st, "ws", "status", name)
	out := map[string]string{}
	for _, r := range st.Refs {
		out[r.Name] = r.Commit
	}
	return out
}

func (e *wsEnv) generation(name string) string {
	e.t.Helper()
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "show", name)
	return v.Generation
}

// FP-1: create through the CLI and MCP; duplicates conflict on both;
// identity survives a restart exactly; invalid names create no files.
func TestWorkspaceCreate(t *testing.T) {
	e := startWs(t, "", nil)
	var a contract.WorkspaceView
	e.jsonCLI(&a, "ws", "create", "alpha")
	if a.Name != "alpha" || a.DefaultBranch != "refs/heads/main" || !contract.ValidWorkspaceToken(a.Instance) || a.Retention.AutomaticPrune || a.Retention.QuotaBytes != nil {
		t.Fatalf("create %+v", a)
	}
	e.mcp.fails("ws_create", map[string]any{"name": "alpha"}, contract.CodeConflict)
	if r := e.run("ws", "create", "alpha"); r.code != 4 {
		t.Fatalf("CLI duplicate %+v", r)
	}
	var b contract.WorkspaceView
	e.mcpJSON(&b, "ws_create", map[string]any{"name": "beta"})
	if b.Instance == a.Instance {
		t.Fatal("instance reused")
	}
	before := wsFiles(t, e.p.root)
	for _, bad := range []string{"Alpha", "-a", "a_b", strings.Repeat("a", 64)} {
		if r := e.run("ws", "create", bad); r.code != 2 {
			t.Fatalf("CLI %q = %+v", bad, r)
		}
		e.mcp.fails("ws_create", map[string]any{"name": bad}, contract.CodeInvalidArgument)
	}
	if after := wsFiles(t, e.p.root); !slices.Equal(before, after) {
		t.Fatalf("invalid names wrote files")
	}
	e.restart()
	var a2, b2 contract.WorkspaceView
	e.jsonCLI(&a2, "ws", "show", "alpha")
	e.mcpJSON(&b2, "ws_show", map[string]any{"name": "beta"})
	a.SizeBytes, a2.SizeBytes, b.SizeBytes, b2.SizeBytes = 0, 0, 0, 0
	if a2 != a || b2 != b {
		t.Fatalf("identity after restart %+v %+v", a2, b2)
	}
}

// FP-2: both frontends page a sorted list without contents, from an
// empty plane; an unavailable plane fails both.
func TestWorkspaceList(t *testing.T) {
	e := startWs(t, "", nil)
	var l contract.WorkspaceListResponse
	e.jsonCLI(&l, "ws", "ls")
	if l.Workspaces == nil || len(l.Workspaces) != 0 || l.NextAfter != nil {
		t.Fatalf("empty %+v", l)
	}
	if text := e.mcp.ok("ws_ls", nil); text != `{"workspaces":[],"next_after":null}` {
		t.Fatalf("empty MCP %s", text)
	}
	for _, n := range []string{"delta", "alpha", "charlie", "bravo", "echo"} {
		e.ok("ws", "create", n)
	}
	page := func(mcp bool) []string {
		var names []string
		after := ""
		for {
			var p contract.WorkspaceListResponse
			if mcp {
				args := map[string]any{"limit": 2}
				if after != "" {
					args["after"] = after
				}
				text := e.mcpJSON(&p, "ws_ls", args)
				if strings.Contains(text, "size_bytes") || strings.Contains(text, "generation") {
					t.Fatalf("list rows carry more than identity: %s", text)
				}
			} else {
				args := []string{"ws", "ls", "--limit", "2"}
				if after != "" {
					args = append(args, "--after", after)
				}
				e.jsonCLI(&p, args...)
			}
			for _, s := range p.Workspaces {
				names = append(names, s.Name)
			}
			if p.NextAfter == nil {
				return names
			}
			after = *p.NextAfter
		}
	}
	want := "alpha,bravo,charlie,delta,echo"
	if got := strings.Join(page(false), ","); got != want {
		t.Fatalf("CLI pages %s", got)
	}
	if got := strings.Join(page(true), ","); got != want {
		t.Fatalf("MCP pages %s", got)
	}
	e.p.stop(t)
	if r := e.run("ws", "ls"); r.code != 5 {
		t.Fatalf("unavailable CLI %+v", r)
	}
	e.mcp.fails("ws_ls", nil, contract.CodeUnavailable)
}

// FP-3: show reports the owned on-disk size exactly after seeding binary,
// text and history over TLS, with fixed retention; identity and size
// survive a restart.
func TestWorkspaceShow(t *testing.T) {
	e := startWs(t, "", nil)
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "alpha")
	s, c0, c1 := seedStore(t)
	pushOK(t, e.remote("alpha", v.Instance), s, "refs/heads/main", plumbing.ZeroHash, c0)
	pushOK(t, e.remote("alpha", v.Instance), s, "refs/heads/main", c0, c1)
	var got contract.WorkspaceView
	e.jsonCLI(&got, "ws", "show", "alpha")
	if want := ownedSize(t, e.p.root, "alpha"); got.SizeBytes != want || want <= v.SizeBytes {
		t.Fatalf("size %d, owned files %d (empty %d)", got.SizeBytes, want, v.SizeBytes)
	}
	if got.Retention.AutomaticPrune || got.Retention.QuotaBytes != nil {
		t.Fatalf("retention %+v", got.Retention)
	}
	text := e.ok("ws", "show", "alpha")
	if !strings.Contains(text, fmt.Sprintf("size_bytes: %d\n", got.SizeBytes)) {
		t.Fatalf("text %q", text)
	}
	e.restart()
	var after contract.WorkspaceView
	e.mcpJSON(&after, "ws_show", map[string]any{"name": "alpha"})
	if after != got {
		t.Fatalf("after restart %+v, before %+v", after, got)
	}
}

// FP-4: both frontends remove a whole workspace; the old transport URL
// disappears; an old instance never removes a recreated name; an injected
// failure before the commit point preserves the workspace (delegated).
func TestWorkspaceRemove(t *testing.T) {
	e := startWs(t, "", nil)
	var a, b contract.WorkspaceView
	e.jsonCLI(&a, "ws", "create", "alpha")
	e.mcpJSON(&b, "ws_create", map[string]any{"name": "beta"})
	s, c0, _ := seedStore(t)
	pushOK(t, e.remote("alpha", a.Instance), s, "refs/heads/main", plumbing.ZeroHash, c0)
	if r := e.run("ws", "rm", "--instance", strings.Repeat("0", 32), "alpha"); r.code != 4 {
		t.Fatalf("stale CLI rm %+v", r)
	}
	if out := e.ok("ws", "rm", "--instance", a.Instance, "alpha"); out != "removed: alpha instance: "+a.Instance+"\n" {
		t.Fatalf("CLI rm %q", out)
	}
	var rm contract.WorkspaceRemoveResponse
	e.mcpJSON(&rm, "ws_rm", map[string]any{"name": "beta", "instance": b.Instance})
	if !rm.Removed || rm.Name != "beta" {
		t.Fatalf("MCP rm %+v", rm)
	}
	if _, err := e.remote("alpha", "").Refs(context.Background()); err == nil {
		t.Fatal("the removed workspace's transport still answers")
	}
	if len(wsFiles(t, e.p.root)) != 1 {
		t.Fatalf("leftovers %v", wsFiles(t, e.p.root))
	}
	var a2 contract.WorkspaceView
	e.jsonCLI(&a2, "ws", "create", "alpha")
	e.mcp.fails("ws_rm", map[string]any{"name": "alpha", "instance": a.Instance}, contract.CodeConflict)
	if r := e.run("ws", "rm", "--instance", a.Instance, "alpha"); r.code != 4 {
		t.Fatalf("old instance CLI rm %+v", r)
	}
	if got := e.statusRefs("alpha"); len(got) != 0 {
		t.Fatalf("recreated workspace refs %v", got)
	}
	planeContract(t, contractBinary(t, wsPkg), wsPkg, "TestDurabilityBoundaries", "remove")
}

// plantTaskRefs writes task refs, their objects and publication metadata
// into a stopped plane's workspace (fixture only: 09a has no public task
// ref creation).
func plantTaskRefs(t *testing.T, root, name string, src *memory.Storage, tasks map[string]plumbing.Hash, published map[string]time.Time) {
	t.Helper()
	ws := filepath.Join(root, "workspaces", name)
	cur, err := os.ReadFile(filepath.Join(ws, "CURRENT"))
	if err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(ws, "generations", strings.TrimSpace(string(cur)))
	repo := filepath.Join(gen, "repo.git")
	st := filesystem.NewStorage(osfs.New(repo, osfs.WithBoundOS()), cache.NewObjectLRUDefault())
	iter, _ := src.IterEncodedObjects(plumbing.AnyObject)
	if err := iter.ForEach(func(o plumbing.EncodedObject) error { _, err := st.SetEncodedObject(o); return err }); err != nil {
		t.Fatal(err)
	}
	st.Close()
	type meta struct {
		Commit      string `json:"commit"`
		PublishedAt string `json:"published_at"`
	}
	var doc struct {
		Schema   int             `json:"schema"`
		TaskRefs map[string]meta `json:"task_refs"`
	}
	b, _ := os.ReadFile(filepath.Join(gen, "refs.json"))
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for id, c := range tasks {
		ref := "refs/callsheet/tasks/" + id
		p := filepath.Join(repo, filepath.FromSlash(ref))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(c.String()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc.TaskRefs[ref] = meta{Commit: c.String(), PublishedAt: contract.FormatTime(published[id])}
	}
	b, _ = json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(filepath.Join(gen, "refs.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	// Managed storage is private.
	filepath.Walk(ws, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			mode := os.FileMode(0o600)
			if fi.IsDir() {
				mode = 0o700
			}
			os.Chmod(p, mode)
		}
		return nil
	})
}

// hasObjects reports which hashes a stopped plane's workspace stores.
func hasObjects(t *testing.T, root, name string, hs ...plumbing.Hash) []bool {
	t.Helper()
	ws := filepath.Join(root, "workspaces", name)
	cur, _ := os.ReadFile(filepath.Join(ws, "CURRENT"))
	st := filesystem.NewStorage(osfs.New(filepath.Join(ws, "generations", strings.TrimSpace(string(cur)), "repo.git")), cache.NewObjectLRUDefault())
	defer st.Close()
	var out []bool
	for _, h := range hs {
		_, err := st.EncodedObject(plumbing.AnyObject, h)
		out = append(out, err == nil)
	}
	return out
}

// FP-5: task refs planted with publication times before, at and after the
// cutoff in a stopped plane; prune through the CLI removes exactly the
// older one and its exclusive objects while the branch and the newer task
// ancestry remain; the size falls; MCP prune and a restart confirm.
func TestWorkspacePrune(t *testing.T) {
	e := startWs(t, "", nil)
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "alpha")
	s, c0, c1 := seedStore(t)
	pushOK(t, e.remote("alpha", v.Instance), s, "refs/heads/main", plumbing.ZeroHash, c1)
	mk := func(content string, parents ...plumbing.Hash) plumbing.Hash {
		h, err := testkit.CommitFiles(s, map[string]testkit.FileSpec{"t": {Mode: filemode.Regular, Content: bytes.Repeat([]byte(content), 4096)}}, parents, content)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	oldTask := mk("old-exclusive")
	equalTask := mk("equal", c0)
	newParent := mk("new-parent")
	newTask := mk("new", newParent)
	cutoff := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ids := map[string]plumbing.Hash{"t_" + strings.Repeat("1", 32): oldTask, "t_" + strings.Repeat("2", 32): equalTask, "t_" + strings.Repeat("3", 32): newTask}
	pub := map[string]time.Time{"t_" + strings.Repeat("1", 32): cutoff.Add(-time.Second), "t_" + strings.Repeat("2", 32): cutoff, "t_" + strings.Repeat("3", 32): cutoff.Add(time.Hour)}
	e.p.stop(t)
	plantTaskRefs(t, e.p.root, "alpha", s, ids, pub)
	e.p.start(t)
	var before contract.WorkspaceView
	e.jsonCLI(&before, "ws", "show", "alpha")
	var pr contract.WorkspacePruneResponse
	e.jsonCLI(&pr, "ws", "prune", "--instance", v.Instance, "--before", "2026-09-30T14:00:00+02:00", "alpha")
	if pr.RemovedTaskRefs != 1 || pr.ReclaimedBytes <= 0 || pr.SizeBytes >= before.SizeBytes {
		t.Fatalf("prune %+v before %d", pr, before.SizeBytes)
	}
	refs := e.statusRefs("alpha")
	if _, ok := refs["refs/callsheet/tasks/t_"+strings.Repeat("1", 32)]; ok || len(refs) != 3 || refs["refs/heads/main"] != c1.String() {
		t.Fatalf("refs after prune %v", refs)
	}
	// The newer task's ancestry is still fetchable.
	fetched := testkit.NewMemoryStore()
	if err := e.remote("alpha", "").Fetch(context.Background(), fetched, []plumbing.Hash{newTask, c1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := object.GetCommit(fetched, newParent); err != nil {
		t.Fatalf("newer task ancestry: %v", err)
	}
	// MCP prune at a later cutoff removes the equal one too.
	var pr2 contract.WorkspacePruneResponse
	e.mcpJSON(&pr2, "ws_prune", map[string]any{"name": "alpha", "instance": v.Instance, "before": "2026-09-30T12:00:00.000000001Z"})
	if pr2.RemovedTaskRefs != 1 {
		t.Fatalf("MCP prune %+v", pr2)
	}
	e.restart()
	if refs := e.statusRefs("alpha"); len(refs) != 2 {
		t.Fatalf("refs after restart %v", refs)
	}
	e.p.stop(t)
	if got := hasObjects(t, e.p.root, "alpha", oldTask, equalTask, c0, c1, newTask, newParent); !slices.Equal(got, []bool{false, false, true, true, true, true}) {
		t.Fatalf("objects after prune %v", got)
	}
}

// FP-6: real TLS push and fetch into independent stores keep ancestry,
// modes and binary data; a wrong CA or name never reaches the handler;
// concurrent writers get exactly one CAS winner; every UT-4 scenario runs
// on the production handler (delegated).
func TestWorkspaceTransport(t *testing.T) {
	e := startWs(t, "", nil)
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "alpha")
	s, c0, c1 := seedStore(t)
	r := e.remote("alpha", v.Instance)
	pushOK(t, r, s, "refs/heads/main", plumbing.ZeroHash, c0)
	pushOK(t, r, s, "refs/heads/main", c0, c1)
	other := testkit.NewMemoryStore()
	if err := r.Fetch(context.Background(), other, []plumbing.Hash{c1}, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []plumbing.Hash{c0, c1} {
		want, _ := testkit.TreeFiles(s, c)
		got, err := testkit.TreeFiles(other, c)
		if err != nil {
			t.Fatal(err)
		}
		if err := testkit.EqualFiles(got, want); err != nil {
			t.Fatal(err)
		}
	}
	if cm, _ := object.GetCommit(other, c1); cm.ParentHashes[0] != c0 {
		t.Fatal("ancestry lost")
	}
	// Wrong trust never reaches the plane: the TLS handshake fails.
	gen := e.generation("alpha")
	wrongCA, _ := testkit.NewFixtureCA()
	hc, _ := testkit.GitHTTPClient(wrongCA.CertPEM)
	if res := (testkit.GitRemote{URL: r.URL, Instance: v.Instance, HTTP: hc}).Push(context.Background(), s, "refs/heads/x", plumbing.ZeroHash, c1); res.Err == nil || !strings.Contains(res.Err.Error(), "certificate") {
		t.Fatalf("wrong CA: %+v", res.Err)
	}
	named := r
	named.URL = strings.Replace(r.URL, "127.0.0.1", "localhost", 1)
	if res := named.Push(context.Background(), s, "refs/heads/x", plumbing.ZeroHash, c1); res.Err == nil || !strings.Contains(res.Err.Error(), "certificate") {
		t.Fatalf("wrong name: %+v", res.Err)
	}
	if e.generation("alpha") != gen {
		t.Fatal("an untrusted request changed the workspace")
	}
	// Concurrent writers: exactly one CAS winner.
	var heads []plumbing.Hash
	for i := range 5 {
		h, _ := testkit.CommitFiles(s, map[string]testkit.FileSpec{"w": {Mode: filemode.Regular, Content: []byte(fmt.Sprint(i))}}, []plumbing.Hash{c1}, fmt.Sprint(i))
		heads = append(heads, h)
	}
	var wg sync.WaitGroup
	wins := make([]bool, len(heads))
	start := make(chan struct{})
	for i, h := range heads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			wins[i] = r.Push(context.Background(), s, "refs/heads/main", c1, h).OK()
		}()
	}
	close(start)
	wg.Wait()
	n := 0
	for i, w := range wins {
		if w {
			n++
			if e.statusRefs("alpha")["refs/heads/main"] != heads[i].String() {
				t.Fatal("the winner is not main")
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d CAS winners", n)
	}
	bin := contractBinary(t, wsPkg)
	for _, name := range []string{"TestReceiveRefNames", "TestReceivePolicy", "TestReceiveCAS", "TestReceiveIntegrity", "TestUploadPolicy",
		"TestGitRouting", "TestReceiveConcurrentCAS", "TestReceiveCancellation", "TestGitIOHaltJoins", "TestIdleDeadline"} {
		planeContract(t, bin, wsPkg, name)
	}
}

// maxRef is a valid 512-byte full branch ref.
func maxRef() string {
	return "refs/heads/" + strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 99)
}

// exactRoot returns a canonical, not yet existing state root of n bytes.
func exactRoot(t *testing.T, n int) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// rem bytes of "/name" components, each name 1-200 bytes; the last one
	// does not exist yet.
	rem := n - len(base)
	k := (rem + 200) / 201
	names := rem - k
	root := base
	for i := range k {
		l := names / k
		if i < names%k {
			l++
		}
		if i == k-1 {
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		root = filepath.Join(root, strings.Repeat(string(rune('p'+i%8)), l))
	}
	if len(root) != n {
		t.Fatalf("root %d bytes", len(root))
	}
	return root
}

// FP-7: create, move and delete branches through both frontends with CAS,
// explicit non-fast-forward moves included; main changes only when named;
// stale updates fail across a restart; nonportable names are refused by
// both frontends before any write; the D1 maximum-path subcase.
func TestWorkspaceRefSet(t *testing.T) {
	e := startWs(t, "", nil)
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "alpha")
	s, c0, c1 := seedStore(t)
	pushOK(t, e.remote("alpha", v.Instance), s, "refs/heads/main", plumbing.ZeroHash, c1)
	inst := "--instance=" + v.Instance
	var r contract.WorkspaceRefSetResponse
	e.jsonCLI(&r, "ws", "ref", "set", inst, "--expected", "absent", "alpha", "feature", c0.String())
	if r.OldCommit != nil || *r.NewCommit != c0.String() || r.Ref != "refs/heads/feature" {
		t.Fatalf("CLI create %+v", r)
	}
	e.mcpJSON(&r, "ws_ref_set", map[string]any{"name": "alpha", "instance": v.Instance, "branch": "feature", "expected": c0.String(), "target": "main"})
	if *r.OldCommit != c0.String() || *r.NewCommit != c1.String() {
		t.Fatalf("MCP move %+v", r)
	}
	// An explicit non-fast-forward move back.
	e.jsonCLI(&r, "ws", "ref", "set", inst, "--expected", c1.String(), "alpha", "refs/heads/feature", c0.String())
	if refs := e.statusRefs("alpha"); refs["refs/heads/main"] != c1.String() || refs["refs/heads/feature"] != c0.String() {
		t.Fatalf("main moved %v", refs)
	}
	e.mcp.fails("ws_ref_set", map[string]any{"name": "alpha", "instance": v.Instance, "branch": "feature", "expected": c1.String(), "target": "main"}, contract.CodeConflict)
	e.restart()
	if rr := e.run("ws", "ref", "set", inst, "--expected", c1.String(), "alpha", "feature", "main"); rr.code != 4 {
		t.Fatalf("stale after restart %+v", rr)
	}
	e.mcpJSON(&r, "ws_ref_set", map[string]any{"name": "alpha", "instance": v.Instance, "branch": "feature", "expected": c0.String(), "delete": true})
	if r.NewCommit != nil {
		t.Fatalf("MCP delete %+v", r)
	}
	// Nonportable names: invalid_argument before any write, both frontends.
	gen, before := e.generation("alpha"), e.statusRefs("alpha")
	for _, bad := range []string{"Main", "café", "café", "ma�in", strings.Repeat("a", 201), strings.Repeat("a", 256), maxRef() + "c"} {
		if rr := e.run("ws", "ref", "set", inst, "--expected", "absent", "alpha", bad, "main"); rr.code != 2 {
			t.Fatalf("CLI %q = %+v", bad, rr)
		}
		e.mcp.fails("ws_ref_set", map[string]any{"name": "alpha", "instance": v.Instance, "branch": bad, "expected": "absent", "target": "main"}, contract.CodeInvalidArgument)
	}
	if rr := e.run("ws", "ref", "set", inst, "--expected", "absent", "alpha", "ma\xffin", "main"); rr.code != 2 {
		t.Fatalf("CLI raw invalid UTF-8 = %+v", rr)
	}
	// Invalid UTF-8 cannot be JSON: the MCP frame is refused at parsing
	// (-32700) and no tool runs.
	if _, err := e.mcp.stdin.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":9999,\"method\":\"tools/call\",\"params\":{\"name\":\"ws_ref_set\",\"arguments\":{\"name\":\"alpha\",\"instance\":\"" + v.Instance + "\",\"branch\":\"ma\xffin\",\"expected\":\"absent\",\"target\":\"main\"}}}\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(mcpWait)
	for {
		e.mcp.mu.Lock()
		n := len(e.mcp.unexpected)
		parsed := n > 0 && strings.Contains(e.mcp.unexpected[n-1], "-32700")
		e.mcp.mu.Unlock()
		if parsed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no parse error for raw invalid UTF-8")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.mcp.mu.Lock()
	e.mcp.unexpected = nil
	e.mcp.mu.Unlock()
	if gen2, after := e.generation("alpha"), e.statusRefs("alpha"); gen2 != gen || fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a rejected name changed state: %v", after)
	}
	a200 := strings.Repeat("z", 200)
	e.ok("ws", "ref", "set", inst, "--expected", "absent", "alpha", a200, "main")
	e.mcp.ok("ws_ref_set", map[string]any{"name": "alpha", "instance": v.Instance, "branch": maxRef(), "expected": "absent", "target": "main"})
	e.ok("ws", "ref", "set", inst, "--expected", "absent", "alpha", "main2", "main")
	refs := e.statusRefs("alpha")
	if refs["refs/heads/"+a200] != c1.String() || refs[maxRef()] != c1.String() || refs["refs/heads/main2"] != c1.String() || refs["refs/heads/main"] != c1.String() {
		t.Fatalf("accepted names %v", refs)
	}
	t.Run("max-path", func(t *testing.T) { refSetMaxPath(t) })
}

// refSetMaxPath is the D1 subcase: a 256-byte canonical state root, a
// 63-byte workspace name and a 512-byte ref through CLI and MCP (create,
// move, reopen, prune, delete, prune), the captured-path budget delegated
// to the package, and the 257-byte root and 513-byte ref refusals.
func refSetMaxPath(t *testing.T) {
	root := exactRoot(t, 256)
	e := startWs(t, root, nil)
	name := strings.Repeat("w", 63)
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", name)
	s, c0, c1 := seedStore(t)
	pushOK(t, e.remote(name, v.Instance), s, "refs/heads/main", plumbing.ZeroHash, c0)
	pushOK(t, e.remote(name, v.Instance), s, "refs/heads/keep", plumbing.ZeroHash, c1)
	ref := maxRef()
	var r contract.WorkspaceRefSetResponse
	e.jsonCLI(&r, "ws", "ref", "set", "--instance", v.Instance, "--expected", "absent", name, ref, "main")
	if *r.NewCommit != c0.String() || e.statusRefs(name)[ref] != c0.String() {
		t.Fatalf("create %+v", r)
	}
	prev := r.Generation
	e.mcpJSON(&r, "ws_ref_set", map[string]any{"name": name, "instance": v.Instance, "branch": ref, "expected": c0.String(), "target": c1.String()})
	if *r.NewCommit != c1.String() || r.Generation == prev || !contract.ValidWorkspaceToken(r.Generation) {
		t.Fatalf("move %+v", r)
	}
	published := filepath.Join(root, "workspaces", name, "generations", r.Generation, "repo.git", filepath.FromSlash(ref))
	if len(published) != 898 {
		t.Fatalf("published ref path %d bytes", len(published))
	}
	e.restart()
	if got := e.statusRefs(name)[ref]; got != c1.String() {
		t.Fatalf("after reopen %s", got)
	}
	e.ok("ws", "ref", "set", "--instance", v.Instance, "--expected", c1.String(), "--delete", name, "keep")
	e.ok("ws", "prune", "--instance", v.Instance, "--before", "2026-09-30T00:00:00Z", name)
	e.mcp.ok("ws_ref_set", map[string]any{"name": name, "instance": v.Instance, "branch": ref, "expected": c1.String(), "delete": true})
	e.mcp.ok("ws_prune", map[string]any{"name": name, "instance": v.Instance, "before": "2026-09-30T00:00:00Z"})
	e.p.stop(t)
	if got := hasObjects(t, root, name, c0, c1); !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("objects after delete and prune %v", got)
	}
	planeContract(t, contractBinary(t, wsPkg), wsPkg, "TestMaxPathLayout")
	// A 257-byte root: invalid_argument before any state is written.
	long := exactRoot(t, 257)
	if rr := e.cli.run(t, "plane", "init", "--state-dir", long, "--bind", "127.0.0.1:0", "--san", "127.0.0.1"); rr.code != 2 || !strings.Contains(rr.stderr, "plane state root exceeds 256 bytes; choose a shorter --state-dir") {
		t.Fatalf("257-byte root %+v", rr)
	}
	if _, err := os.Lstat(long); err == nil {
		t.Fatal("a refused root was created")
	}
	if _, err := contract.NormalizeBranch(ref+"c", "branch"); err == nil {
		t.Fatal("513-byte ref accepted")
	}
}

// FP-8: status is the plane's stored ref inventory on both frontends:
// parallel branches are separate rows, a moved ref shows its new value,
// case and normalization variants never create or change rows, invalid
// cursors are invalid_argument, and no local working tree is consulted.
func TestWorkspaceStatus(t *testing.T) {
	e := startWs(t, "", nil)
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "alpha")
	s, c0, c1 := seedStore(t)
	r := e.remote("alpha", v.Instance)
	pushOK(t, r, s, "refs/heads/main", plumbing.ZeroHash, c1)
	pushOK(t, r, s, "refs/heads/feature-a", plumbing.ZeroHash, c0)
	pushOK(t, r, s, "refs/heads/feature-b", plumbing.ZeroHash, c1)
	// A coordinator-local repository with a different main is never read.
	local := filepath.Join(e.cli.cwd, ".git")
	os.MkdirAll(filepath.Join(local, "refs", "heads"), 0o755)
	os.WriteFile(filepath.Join(local, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(local, "refs", "heads", "main"), []byte(strings.Repeat("f", 40)+"\n"), 0o644)
	var cliSt, mcpSt contract.WorkspaceStatusResponse
	e.jsonCLI(&cliSt, "ws", "status", "alpha")
	e.mcpJSON(&mcpSt, "ws_status", map[string]any{"name": "alpha"})
	if fmt.Sprint(cliSt) != fmt.Sprint(mcpSt) || len(cliSt.Refs) != 3 || cliSt.Refs[2].Name != "refs/heads/main" || cliSt.Refs[2].Commit != c1.String() {
		t.Fatalf("status CLI %+v MCP %+v", cliSt, mcpSt)
	}
	// Pages through both frontends.
	var p1 contract.WorkspaceStatusResponse
	e.mcpJSON(&p1, "ws_status", map[string]any{"name": "alpha", "limit": 2})
	var p2 contract.WorkspaceStatusResponse
	e.jsonCLI(&p2, "ws", "status", "--after", *p1.NextAfter, "--instance", p1.Instance, "--generation", p1.Generation, "--limit", "2", "alpha")
	if len(p1.Refs) != 2 || len(p2.Refs) != 1 || p2.NextAfter != nil {
		t.Fatalf("pages %+v %+v", p1, p2)
	}
	// A move shows the new state; a stale page conflicts.
	e.ok("ws", "ref", "set", "--instance", v.Instance, "--expected", c0.String(), "alpha", "feature-a", "main")
	if e.statusRefs("alpha")["refs/heads/feature-a"] != c1.String() {
		t.Fatal("moved ref not shown")
	}
	e.mcp.fails("ws_status", map[string]any{"name": "alpha", "after": *p1.NextAfter, "instance": p1.Instance, "generation": p1.Generation}, contract.CodeConflict)
	// Variants never create or change rows.
	before := e.statusRefs("alpha")
	for _, bad := range []string{"refs/heads/Main", "refs/heads/FEATURE-A", "refs/heads/café", "refs/heads/café"} {
		res := r.Push(context.Background(), s, bad, plumbing.ZeroHash, c0)
		if res.OK() {
			t.Fatalf("pushed %q", bad)
		}
	}
	if fmt.Sprint(e.statusRefs("alpha")) != fmt.Sprint(before) {
		t.Fatal("a rejected variant changed rows")
	}
	gen := e.generation("alpha")
	for _, bad := range []string{"refs/heads/Main", "main", "refs/heads/café", "refs/tags/v1"} {
		if rr := e.run("ws", "status", "--after", bad, "--instance", v.Instance, "--generation", gen, "alpha"); rr.code != 2 {
			t.Fatalf("CLI cursor %q = %+v", bad, rr)
		}
		e.mcp.fails("ws_status", map[string]any{"name": "alpha", "after": bad, "instance": v.Instance, "generation": gen}, contract.CodeInvalidArgument)
	}
}

const wsSentinel = "WORKSPACE-CONTENT-SENTINEL-9e21"

// FP-9: explicit base/target comparisons return exact metadata rows before
// and after a ref set on both frontends; a content sentinel in file data,
// a symlink target and commit messages never appears in any MCP traffic;
// pages return every changed path exactly once.
func TestWorkspaceDiff(t *testing.T) {
	e := startWs(t, "", nil)
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "alpha")
	s := testkit.NewMemoryStore()
	base := map[string]testkit.FileSpec{
		"keep.txt": {Mode: filemode.Regular, Content: []byte("keep " + wsSentinel)},
		"edit.txt": {Mode: filemode.Regular, Content: []byte("v1 " + wsSentinel)},
		"gone.txt": {Mode: filemode.Regular, Content: []byte("gone")},
	}
	b, _ := testkit.CommitFiles(s, base, nil, wsSentinel+" base")
	target := map[string]testkit.FileSpec{
		"keep.txt":  {Mode: filemode.Regular, Content: []byte("keep " + wsSentinel)},
		"edit.txt":  {Mode: filemode.Executable, Content: []byte("v2 longer " + wsSentinel)},
		"link":      {Mode: filemode.Symlink, Content: []byte(wsSentinel)},
		"new/a.bin": {Mode: filemode.Regular, Content: []byte{0, 1, 2}},
	}
	c, _ := testkit.CommitFiles(s, target, []plumbing.Hash{b}, wsSentinel+" target")
	r := e.remote("alpha", v.Instance)
	pushOK(t, r, s, "refs/heads/main", plumbing.ZeroHash, b)
	pushOK(t, r, s, "refs/heads/next", plumbing.ZeroHash, c)
	want := []string{
		"edit.txt modified 100644 100755",
		"gone.txt deleted 100644 -",
		"link added - 120000",
		"new/a.bin added - 100644",
	}
	rowsOf := func(d contract.WorkspaceDiffResponse) []string {
		var out []string
		for _, ch := range d.Changes {
			p, _ := base64.StdEncoding.DecodeString(ch.PathBase64)
			om, nm := "-", "-"
			if ch.OldMode != nil {
				om = *ch.OldMode
			}
			if ch.NewMode != nil {
				nm = *ch.NewMode
			}
			out = append(out, string(p)+" "+ch.Kind+" "+om+" "+nm)
		}
		return out
	}
	var cd, md contract.WorkspaceDiffResponse
	e.jsonCLI(&cd, "ws", "diff", "--base", "main", "alpha", "next")
	e.mcpJSON(&md, "ws_diff", map[string]any{"name": "alpha", "base": b.String(), "target": "next"})
	if !slices.Equal(rowsOf(cd), want) || !slices.Equal(rowsOf(md), want) || *cd.BaseCommit != b.String() || md.TargetCommit != c.String() {
		t.Fatalf("CLI %v MCP %v", rowsOf(cd), rowsOf(md))
	}
	// After a ref set the same ref-based comparison resolves the new
	// commit; the returned hashes keep the old comparison.
	e.ok("ws", "ref", "set", "--instance", v.Instance, "--expected", b.String(), "alpha", "main", "next")
	e.mcpJSON(&md, "ws_diff", map[string]any{"name": "alpha", "base": "main", "target": "next"})
	if len(md.Changes) != 0 {
		t.Fatalf("after ref set %v", rowsOf(md))
	}
	e.jsonCLI(&cd, "ws", "diff", "--base", b.String(), "alpha", c.String())
	if !slices.Equal(rowsOf(cd), want) {
		t.Fatalf("by hashes %v", rowsOf(cd))
	}
	// Pages of one row each, through both frontends alternately.
	var seen []string
	args := map[string]any{"name": "alpha", "base": "empty", "target": c.String(), "limit": 1}
	for i := 0; ; i++ {
		var p contract.WorkspaceDiffResponse
		if i%2 == 0 {
			e.mcpJSON(&p, "ws_diff", args)
		} else {
			cli := []string{"ws", "diff", "--base", "empty", "--limit", "1", "--after", args["after"].(string), "--instance", args["instance"].(string), "--generation", args["generation"].(string), "alpha", c.String()}
			e.jsonCLI(&p, cli...)
		}
		for _, r := range rowsOf(p) {
			seen = append(seen, strings.Fields(r)[0])
		}
		if p.NextAfter == nil {
			break
		}
		args["after"], args["instance"], args["generation"] = *p.NextAfter, p.Instance, p.Generation
	}
	names := []string{}
	for n := range target {
		names = append(names, n)
	}
	sort.Strings(names)
	if !slices.Equal(seen, names) {
		t.Fatalf("pages %v want %v", seen, names)
	}
	e.mcp.fails("ws_diff", map[string]any{"name": "alpha", "base": "empty", "target": strings.Repeat("0", 40)}, contract.CodeNotFound)
	e.mcp.mu.Lock()
	all := strings.Join(e.mcp.all, "")
	e.mcp.mu.Unlock()
	if strings.Contains(all, wsSentinel) {
		t.Fatal("content reached MCP traffic")
	}
}

// FP-10: with git absent from PATH (an empty directory) for the test
// process, the plane, the CLI and the MCP server, every 09a operation
// contract works: create, list, show, remove, prune, transport (go-git
// seeding and fetching in this environment), ref set, status and diff.
func TestWorkspaceNoGit(t *testing.T) {
	nodeBinary(t) // built before PATH is emptied
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if p, err := exec.LookPath("git"); err == nil || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("git found at %q", p)
	}
	e := startWs(t, "", []string{"PATH=" + empty})
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "alpha")
	var b contract.WorkspaceView
	e.mcpJSON(&b, "ws_create", map[string]any{"name": "beta"})
	var l contract.WorkspaceListResponse
	e.mcpJSON(&l, "ws_ls", nil)
	if len(l.Workspaces) != 2 {
		t.Fatalf("list %+v", l)
	}
	s, c0, c1 := seedStore(t)
	r := e.remote("alpha", v.Instance)
	pushOK(t, r, s, "refs/heads/main", plumbing.ZeroHash, c0)
	pushOK(t, r, s, "refs/heads/main", c0, c1)
	fetched := testkit.NewMemoryStore()
	if err := r.Fetch(context.Background(), fetched, []plumbing.Hash{c1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := object.GetCommit(fetched, c0); err != nil {
		t.Fatal(err)
	}
	var shown contract.WorkspaceView
	e.jsonCLI(&shown, "ws", "show", "alpha")
	if shown.SizeBytes != ownedSize(t, e.p.root, "alpha") {
		t.Fatal("size")
	}
	e.ok("ws", "ref", "set", "--instance", v.Instance, "--expected", "absent", "alpha", "old", c0.String())
	var st contract.WorkspaceStatusResponse
	e.mcpJSON(&st, "ws_status", map[string]any{"name": "alpha"})
	if len(st.Refs) != 2 {
		t.Fatalf("status %+v", st)
	}
	var d contract.WorkspaceDiffResponse
	e.jsonCLI(&d, "ws", "diff", "--base", "old", "alpha", "main")
	if len(d.Changes) != 1 {
		t.Fatalf("diff %+v", d)
	}
	e.ok("ws", "prune", "--instance", v.Instance, "--before", "2026-09-30T00:00:00Z", "alpha")
	e.mcp.ok("ws_rm", map[string]any{"name": "beta", "instance": b.Instance})
	e.ok("ws", "rm", "--instance", v.Instance, "alpha")
	if out := e.ok("ws", "ls"); out != "NAME\tINSTANCE\tCREATED_AT\n" {
		t.Fatalf("final list %q", out)
	}
}
