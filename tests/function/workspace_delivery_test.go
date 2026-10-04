package function

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/mcp"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

// Iteration 10c function tests: exactly one top-level TestWorkspace* per
// FP (FP-1..FP-6, design 10c), in FP order, on the shared 10b deployment
// (sharedWsRig: a plane and two fake-adapter workers from the built
// binaries). They drive the coordinator doors: the CLI binary and the
// stdio MCP server against the production TLS endpoint.

// ---- helpers ----

// cliEnv runs the CLI in dir with env and the deployment's trust.
func (r *wsRig) cliEnv(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	return runBin(t, r.dep.Bin, dir, env, append(args, "--plane", r.dep.URL, "--ca", r.dep.CA)...)
}

// okOut requires exit 0 and returns stdout without its final newline.
func okOut(t *testing.T, what string, res result) []byte {
	t.Helper()
	if res.code != 0 {
		t.Fatalf("%s: %+v", what, res)
	}
	return []byte(strings.TrimSuffix(res.stdout, "\n"))
}

// cliDispatch dispatches script to role with the CLI (JSON) and extra
// flags.
func (r *wsRig) cliDispatch(t *testing.T, role string, s fakeadapter.WorkspaceScript, extra ...string) contract.TaskView {
	t.Helper()
	r.awaitAccept(role)
	v, err := contract.ParseDispatchResponse(okOut(t, "dispatch", r.cli(t, append([]string{"dispatch", "--json", "--role-id", role, "--goal", fakeadapter.WorkspaceGoal(s),
		"--acceptance", "fixture"}, extra...)...)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// taskStatus is ws status TASK_ID's exact envelope payload.
func (r *wsRig) taskStatus(t *testing.T, id string) contract.TaskWorkspaceStatus {
	t.Helper()
	resp, err := contract.ParseTaskWorkspaceStatusResponse(okOut(t, "ws status", r.cli(t, "ws", "status", "--json", id)))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Workspace
}

// pullJSON runs ws pull --json args.
func (r *wsRig) pullJSON(t *testing.T, args ...string) contract.WorkspacePullResult {
	t.Helper()
	p, err := contract.ParseWorkspacePullResult(okOut(t, "ws pull", r.cli(t, append([]string{"ws", "pull", "--json"}, args...)...)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// diffJSON runs ws diff --json args and decodes the page (after is the
// cursor the page continues).
func (r *wsRig) diffJSON(t *testing.T, after string, limit int, args ...string) contract.WorkspaceDiffResponse {
	t.Helper()
	var cur []byte
	if after != "" {
		cur, _ = base64.StdEncoding.DecodeString(after)
	}
	d, err := contract.ParseWorkspaceDiff(okOut(t, "ws diff", r.cli(t, append([]string{"ws", "diff", "--json"}, args...)...)), cur, limit)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// freshName is a new workspace name (not created).
func (r *wsRig) freshName(prefix string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return fmt.Sprintf("%s%d", prefix, r.seq)
}

func future() string { return time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }

// recreate removes and recreates workspace name.
func (r *wsRig) recreate(t *testing.T, name, inst string) string {
	t.Helper()
	okOut(t, "ws rm", r.cli(t, "ws", "rm", "--instance", inst, name))
	v, err := r.dep.Client.CreateWorkspace(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return v.Instance
}

// hasLine reports whether text has the exact line.
func hasLine(text, line string) bool { return strings.Contains("\n"+text, "\n"+line+"\n") }

// localCommit reads a commit from a fixture repository's object store.
func localCommit(t *testing.T, repo *fixRepo, h string) *object.Commit {
	t.Helper()
	c, err := object.GetCommit(repo.store, plumbing.NewHash(h))
	if err != nil {
		t.Fatalf("commit %s not installed: %v", h, err)
	}
	return c
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- FP-1 ----

// FP-1: CLI and MCP dispatch with workspace, base and workspace_instance
// reach the real plane and node with the shared contract; invalid
// combinations never dispatch; scratch dispatch is unchanged.
func TestWorkspaceDispatchDoors(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	stale := strings.Repeat("9", 32)
	write := fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "doors.txt", Data: "d\n"})}
	goal := fakeadapter.WorkspaceGoal(write)
	before := r.taskCount(t)
	for _, c := range []struct {
		args []string
		code int
		frag string
	}{
		{[]string{"--workspace", ""}, 2, "workspace name"},
		{[]string{"--base", "main"}, 2, "base requires workspace"},
		{[]string{"--workspace-instance", inst}, 2, "workspace_instance requires workspace"},
		{[]string{"--workspace", name, "--base", "HEAD~1"}, 2, "invalid base"},
		{[]string{"--workspace", name, "--base", ""}, 2, "invalid base"},
		{[]string{"--workspace", name, "--workspace", name}, 2, "only once"},
		{[]string{"--workspace", name, "--workspace-instance", "x"}, 2, "32-hex"},
		// An unborn main is never an implicit empty base; a stale instance
		// and an unknown workspace refuse before admission.
		{[]string{"--workspace", name}, 3, "nothing was dispatched"},
		{[]string{"--workspace", name, "--base", "empty", "--workspace-instance", stale}, 4, "nothing was dispatched"},
		{[]string{"--workspace", "nosuch", "--base", "empty"}, 3, "nothing was dispatched"},
	} {
		wantExit(t, r.cli(t, append([]string{"dispatch", "--role-id", "ws-a", "--goal", goal, "--acceptance", "fixture"}, c.args...)...), c.code, c.frag)
	}
	if n := r.taskCount(t); n != before {
		t.Fatalf("%d invalid CLI dispatches were admitted", n-before)
	}
	// CLI: an empty workspace needs base empty; the guard is forwarded.
	v := r.cliDispatch(t, "ws-a", write, "--workspace", name, "--base", "empty", "--workspace-instance", inst)
	if b := v.WorkspaceBinding; b == nil || b.Name != name || b.Instance != inst || b.BaseSelector != contract.SelectorEmpty || b.BaseCommit != nil ||
		v.Request.WorkspaceInstance == nil || *v.Request.WorkspaceInstance != inst || v.WorkspacePhase == nil {
		t.Fatalf("CLI binding %+v", v)
	}
	done := r.await(t, v.TaskID)
	w := publishedWs(t, done)
	files, c := r.files(t, name, inst, *w.Commit)
	if len(c.ParentHashes) != 0 || string(files["doors.txt"].Content) != "d\n" {
		t.Fatalf("empty-base result %v %v", c.ParentHashes, files)
	}
	show := r.cli(t, "task", "show", "--lines", "0", v.TaskID).stdout
	for _, line := range []string{"workspace: " + name, "workspace_instance: " + inst, "base_commit: -", "workspace_phase: -", "publication: published",
		"result_commit: " + *w.Commit, "result_ref: " + contract.TaskRefPrefix + v.TaskID, "publication_error: -", `changed_path: "doors.txt"`} {
		if !hasLine(show, line) {
			t.Fatalf("task show lacks %q:\n%s", line, show)
		}
	}
	// MCP: the default base (main) with the guard, and a task-ID base.
	main := r.seed(t, name, inst, "", map[string]tfile{"m.txt": tr("m\n")})
	m := startMCP(t, "--plane", r.dep.URL, "--ca", r.dep.CA)
	r.awaitAccept("ws-z")
	mv, err := contract.ParseDispatchResponse([]byte(m.ok("dispatch", dispatchArgs("id", "ws-z", goal, map[string]any{"workspace": name, "workspace_instance": inst}))))
	if err != nil || mv.WorkspaceBinding == nil || *mv.WorkspaceBinding.BaseCommit != main || mv.WorkspaceBinding.BaseSelector != contract.DefaultBranchRef {
		t.Fatalf("MCP main binding %+v %v", mv.WorkspaceBinding, err)
	}
	r.await(t, mv.TaskID)
	r.awaitAccept("ws-z")
	tv, err := contract.ParseDispatchResponse([]byte(m.ok("dispatch", dispatchArgs("id", "ws-z", goal, map[string]any{"workspace": name, "base": v.TaskID}))))
	if err != nil || tv.WorkspaceBinding == nil || tv.WorkspaceBinding.BaseSelector != contract.TaskRefPrefix+v.TaskID || *tv.WorkspaceBinding.BaseCommit != *w.Commit {
		t.Fatalf("MCP task-ID base %+v %v", tv.WorkspaceBinding, err)
	}
	r.await(t, tv.TaskID)
	before = r.taskCount(t)
	for _, c := range []struct {
		extra map[string]any
		code  contract.Code
	}{
		{map[string]any{"workspace": ""}, contract.CodeInvalidArgument},
		{map[string]any{"workspace": nil}, contract.CodeInvalidArgument},
		{map[string]any{"base": "main"}, contract.CodeInvalidArgument},
		{map[string]any{"workspace_instance": inst}, contract.CodeInvalidArgument},
		{map[string]any{"workspace": name, "base": "HEAD~1"}, contract.CodeInvalidArgument},
		{map[string]any{"workspace": name, "workspaceInstance": inst}, contract.CodeInvalidArgument},
		{map[string]any{"workspace": name, "workspace_instance": stale}, contract.CodeConflict},
	} {
		m.fails("dispatch", dispatchArgs("id", "ws-z", goal, c.extra), c.code)
	}
	if n := r.taskCount(t); n != before {
		t.Fatalf("%d invalid MCP dispatches were admitted", n-before)
	}
	// Scratch dispatch is unchanged: no binding, no workspace lines, no
	// result commit.
	sv := r.cliDispatch(t, "ws-a", fakeadapter.WorkspaceScript{})
	if sv.WorkspaceBinding != nil || sv.WorkspacePhase != nil || sv.Request.Workspace != nil {
		t.Fatalf("scratch %+v", sv)
	}
	sd := r.await(t, sv.TaskID)
	if sd.Result.Workspace != nil || sd.Result.ResultCommit != nil || strings.Contains(r.cli(t, "task", "show", sv.TaskID).stdout, "\nworkspace") {
		t.Fatalf("scratch result %+v", sd.Result)
	}
}

// ---- FP-2 ----

// FP-2: task-only and explicit pulls into a dirty repository and a new
// folder: exact objects and local ref, an untouched checkout, the
// task-shaped branch escape, lookup refusals, and pruned and recreated
// workspaces refused without fallback.
func TestWorkspaceTaskPull(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("f\n")})
	a := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "a.txt", Data: "A\n"})})
	wa := publishedWs(t, a)
	// A running task's result is pending; a scratch task has none.
	b := newBarrier(t)
	p := r.running(t, "ws-z", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("in")}, fakeadapter.WorkspaceOp{WaitFor: b.path("go")})}, b, "in")
	wantExit(t, r.cli(t, "ws", "pull", p.TaskID, filepath.Join(canonTemp(t), "x")), 4, "task_result_pending")
	b.release(t, "go")
	r.await(t, p.TaskID)
	s := r.run(t, "ws-a", nil, nil, fakeadapter.WorkspaceScript{})
	wantExit(t, r.cli(t, "ws", "pull", s.TaskID, filepath.Join(canonTemp(t), "x")), 2, "no_task_workspace")
	wantExit(t, r.cli(t, "ws", "pull", "t_"+strings.Repeat("0", 32)), 3, "")
	wantExit(t, r.cli(t, "ws", "pull", "t_bad"), 2, "invalid task ID")
	// Task-only pull into a dirty existing repository.
	repo := mkRepo(t, map[string]tfile{"mine": tr("local\n")})
	repo.write("mine", tr("uncommitted\n"))
	repo.write("untracked", tr("u\n"))
	before := treeDigest(t, repo.root, ".git/objects", ".git/refs/callsheet")
	local := "refs/callsheet/" + name + "/tasks/" + a.TaskID
	res := r.pullJSON(t, a.TaskID, repo.root)
	if res.Selector != contract.TaskRefPrefix+a.TaskID || res.Commit != *wa.Commit || res.Instance != inst || res.LocalRef == nil || *res.LocalRef != local ||
		res.DestinationKind != contract.KindGit || !res.Changed {
		t.Fatalf("task pull %+v", res)
	}
	if readFile(t, filepath.Join(repo.root, ".git", filepath.FromSlash(local))) != *wa.Commit+"\n" || treeDigest(t, repo.root, ".git/objects", ".git/refs/callsheet") != before {
		t.Fatal("the task pull changed the checkout or wrote another ref")
	}
	c := localCommit(t, repo, *wa.Commit)
	if f, err := c.File("a.txt"); err != nil || len(c.ParentHashes) != 1 || c.ParentHashes[0].String() != base {
		t.Fatalf("installed result %v %v", c.ParentHashes, err)
	} else if txt, _ := f.Contents(); txt != "A\n" {
		t.Fatal("installed content")
	}
	// Repeating is a same-value observation.
	if again := r.pullJSON(t, a.TaskID, repo.root); again.Changed || again.OldCommit == nil || *again.OldCommit != *wa.Commit {
		t.Fatalf("repeat %+v", again)
	}
	// Task-only pull into a new folder.
	out := filepath.Join(canonTemp(t), "out")
	if res := r.pullJSON(t, a.TaskID, out); res.DestinationKind != contract.KindFolder || readFile(t, filepath.Join(out, "a.txt")) != "A\n" ||
		readFile(t, filepath.Join(out, "f.txt")) != "f\n" {
		t.Fatalf("folder pull %+v", res)
	}
	// Explicit full-ref and bare task-ID refs select the task ref; a branch
	// named like the task ID needs refs/heads/.
	okOut(t, "ref set", r.cli(t, "ws", "ref", "set", "--instance", inst, "--expected", "absent", name, a.TaskID, base))
	repo2 := mkRepo(t, map[string]tfile{"x": tr("x")})
	for _, c := range []struct{ ref, selector, commit string }{
		{contract.TaskRefPrefix + a.TaskID, contract.TaskRefPrefix + a.TaskID, *wa.Commit},
		{a.TaskID, contract.TaskRefPrefix + a.TaskID, *wa.Commit},
		{"refs/heads/" + a.TaskID, "refs/heads/" + a.TaskID, base},
	} {
		if res := r.pullJSON(t, name, c.ref, repo2.root); res.Selector != c.selector || res.Commit != c.commit {
			t.Fatalf("%s: %+v", c.ref, res)
		}
	}
	// A pruned result is not_found; a recreated workspace is a mismatch,
	// never a fallback to the new workspace of the same name.
	cz := r.run(t, "ws-z", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "c.txt", Data: "C\n"})})
	publishedWs(t, cz)
	st, err := r.dep.Client.WorkspaceStatus(context.Background(), name, statusPage())
	if err != nil {
		t.Fatal(err)
	}
	cutoff := ""
	for _, ref := range st.Refs {
		if ref.Name == contract.TaskRefPrefix+cz.TaskID {
			cutoff = *ref.PublishedAt
		}
	}
	okOut(t, "prune", r.cli(t, "ws", "prune", "--instance", inst, "--before", cutoff, name))
	wantExit(t, r.cli(t, "ws", "pull", a.TaskID, filepath.Join(canonTemp(t), "x")), 3, "pruned")
	if res := r.pullJSON(t, cz.TaskID, repo2.root); res.Commit != *cz.Result.Workspace.Commit {
		t.Fatalf("surviving result %+v", res)
	}
	newInst := r.recreate(t, name, inst)
	r.seed(t, name, newInst, "", map[string]tfile{"f.txt": tr("f\n")})
	gone := filepath.Join(canonTemp(t), "gone")
	wantExit(t, r.cli(t, "ws", "pull", cz.TaskID, gone), 4, "workspace_instance_mismatch")
	if _, err := os.Lstat(gone); !os.IsNotExist(err) {
		t.Fatal("a refused pull wrote its destination")
	}
}

// statusPage is the first full status page.
func statusPage() client.StatusPage { return client.StatusPage{Limit: contract.MaxWorkspaceLimit} }

// ---- FP-3 ----

// FP-3: task workspace status through pending, published, failed
// publication and pruned results; task diffs against the admitted base
// with exact-ref rechecks and continuation generation rejection.
func TestWorkspaceTaskInspect(t *testing.T) {
	r := sharedWsRig(t)
	t.Run("status", func(t *testing.T) {
		name, inst := r.workspace(t)
		base := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("f\n")})
		b := newBarrier(t)
		p := r.running(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("in")}, fakeadapter.WorkspaceOp{WaitFor: b.path("go")},
			fakeadapter.WorkspaceOp{Write: "s.txt", Data: "s\n"})}, b, "in")
		pending := r.taskStatus(t, p.TaskID)
		if contract.TaskTerminal(pending.State) || pending.WorkspacePhase == nil || pending.Result != nil || pending.Available || *pending.Binding.BaseCommit != base {
			t.Fatalf("pending %+v", pending)
		}
		text := r.cli(t, "ws", "status", p.TaskID).stdout
		if !hasLine(text, "task_id: "+p.TaskID) || !hasLine(text, "available: false") || !hasLine(text, "publication: -") || !hasLine(text, "workspace_phase: "+*pending.WorkspacePhase) {
			t.Fatalf("pending text %q", text)
		}
		b.release(t, "go")
		done := r.await(t, p.TaskID)
		w := publishedWs(t, done)
		pub := r.taskStatus(t, p.TaskID)
		if !pub.Available || pub.WorkspacePhase != nil || pub.Result == nil || *pub.Result.Commit != *w.Commit || pub.State != contract.TaskSucceeded {
			t.Fatalf("published %+v", pub)
		}
		if text := r.cli(t, "ws", "status", p.TaskID).stdout; !hasLine(text, "available: true") || !hasLine(text, "result_commit: "+*w.Commit) {
			t.Fatalf("published text %q", text)
		}
		// A failed publication is never available.
		f := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "sub/.git/HEAD", Data: "ref: refs/heads/main\n"})})
		if fs := r.taskStatus(t, f.TaskID); fs.Available || fs.Result.Publication != contract.PublicationFailed || *fs.Result.Error != contract.PubErrUnsupportedRepository {
			t.Fatalf("failed publication %+v", fs.Result)
		}
		// Pruned: unavailable, the historical result intact.
		okOut(t, "prune", r.cli(t, "ws", "prune", "--instance", inst, "--before", future(), name))
		if gone := r.taskStatus(t, p.TaskID); gone.Available || gone.Result == nil || *gone.Result.Commit != *w.Commit {
			t.Fatalf("pruned %+v", gone)
		}
		wantExit(t, r.cli(t, "ws", "status", "--limit", "1", p.TaskID), 2, "no --after")
		wantExit(t, r.cli(t, "ws", "status", r.run(t, "ws-z", nil, nil, fakeadapter.WorkspaceScript{}).TaskID), 2, "no_task_workspace")
		wantExit(t, r.cli(t, "ws", "status", "t_"+strings.Repeat("0", 32)), 3, "")
	})
	t.Run("diff", func(t *testing.T) {
		name, inst := r.workspace(t)
		base := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("f\n"), "g.txt": tr("g\n")})
		x := r.run(t, "ws-z", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "f.txt", Data: "F!\n"},
			fakeadapter.WorkspaceOp{Write: "h.txt", Data: "h\n"}, fakeadapter.WorkspaceOp{Remove: "g.txt"})})
		w := publishedWs(t, x)
		d := r.diffJSON(t, "", 100, x.TaskID)
		if d.BaseCommit == nil || *d.BaseCommit != base || d.TargetCommit != *w.Commit || d.Instance != inst || len(d.Changes) != 3 || d.NextAfter != nil {
			t.Fatalf("task diff %+v", d)
		}
		// Base/result tree metadata equality: the explicit diff of the same
		// hashes and the result DTO's rows.
		e := r.diffJSON(t, "", 100, "--base", base, name, *w.Commit)
		got, _ := json.Marshal(d.Changes)
		want, _ := json.Marshal(e.Changes)
		dto, _ := json.Marshal(w.Changes)
		if string(got) != string(want) || string(got) != string(dto) {
			t.Fatalf("metadata differs:\n%s\n%s\n%s", got, want, dto)
		}
		// Explicit form with a bare task ID target.
		if e2 := r.diffJSON(t, "", 100, "--base", base, name, x.TaskID); e2.TargetCommit != *w.Commit {
			t.Fatalf("bare task ID target %+v", e2)
		}
		// Pages and a continuation; a changed generation is refused.
		p1 := r.diffJSON(t, "", 1, "--limit", "1", x.TaskID)
		if len(p1.Changes) != 1 || p1.NextAfter == nil {
			t.Fatalf("page 1 %+v", p1)
		}
		p2 := r.diffJSON(t, *p1.NextAfter, 1, "--limit", "1", "--after", *p1.NextAfter, "--instance", p1.Instance, "--generation", p1.Generation, x.TaskID)
		if len(p2.Changes) != 1 || p2.Changes[0].PathBase64 == p1.Changes[0].PathBase64 || p2.Generation != p1.Generation {
			t.Fatalf("page 2 %+v", p2)
		}
		if text := r.cli(t, "ws", "diff", x.TaskID).stdout; !hasLine(text, "base_commit: "+base) || !hasLine(text, "target_commit: "+*w.Commit) {
			t.Fatalf("diff text %q", text)
		}
		r.seed(t, name, inst, "other", map[string]tfile{"o.txt": tr("o\n")})
		wantExit(t, r.cli(t, "ws", "diff", "--limit", "1", "--after", *p1.NextAfter, "--instance", p1.Instance, "--generation", p1.Generation, x.TaskID), 4, "restart")
		wantExit(t, r.cli(t, "ws", "diff", "--base", base, x.TaskID), 2, "no --base")
		// Exact-ref rechecks: pruned is not_found, recreated a mismatch.
		okOut(t, "prune", r.cli(t, "ws", "prune", "--instance", inst, "--before", future(), name))
		wantExit(t, r.cli(t, "ws", "diff", x.TaskID), 3, "pruned")
		r.recreate(t, name, inst)
		wantExit(t, r.cli(t, "ws", "diff", x.TaskID), 4, "workspace_instance_mismatch")
	})
}

// ---- FP-4 ----

// formTools are the three two-form tools and their exact properties.
var formTools = map[string][]string{
	"ws_pull":   {"name", "path", "ref", "task_id"},
	"ws_status": {"after", "generation", "instance", "limit", "name", "task_id"},
	"ws_diff":   {"after", "base", "generation", "instance", "limit", "name", "target", "task_id"},
}

// FP-4: the MCP door over stdio against the production TLS deployment:
// flat discovery schemas and both forms of each affected tool, bounded
// metadata-only results, and call lifetimes (cancellation, EOF,
// saturation).
func TestWorkspaceTaskMCP(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"f.txt": tr("f\n")})
	task := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "t.txt", Data: "t\n"})})
	w := publishedWs(t, task)
	t.Run("schemas", func(t *testing.T) {
		m := startMCP(t, "--plane", r.dep.URL, "--ca", r.dep.CA)
		list := m.request("tools/list", nil)
		var tl struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(list.Result, &tl); err != nil || len(tl.Tools) != 23 {
			t.Fatalf("tools/list %v %d", err, len(tl.Tools))
		}
		for i, tool := range tl.Tools {
			if tool.Name != mcp.ToolNames[i] {
				t.Fatalf("tool %d = %s", i, tool.Name)
			}
			want, ok := formTools[tool.Name]
			if !ok && tool.Name != "dispatch" {
				continue
			}
			var s struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
				Additional *bool                      `json:"additionalProperties"`
			}
			json.Unmarshal(tool.InputSchema, &s)
			if tool.Name == "dispatch" {
				if s.Properties["workspace"] == nil || s.Properties["base"] == nil || s.Properties["workspace_instance"] == nil || len(s.Required) != 3 {
					t.Fatalf("dispatch schema %s", tool.InputSchema)
				}
				continue
			}
			var keys []string
			for k := range s.Properties {
				keys = append(keys, k)
			}
			if fmt.Sprint(sortedCopy(keys)) != fmt.Sprint(want) || s.Required != nil || s.Additional == nil || *s.Additional || s.Type != "object" {
				t.Fatalf("%s schema %s", tool.Name, tool.InputSchema)
			}
			for _, comb := range []string{`"oneOf"`, `"anyOf"`, `"allOf"`} {
				if strings.Contains(string(tool.InputSchema), comb) {
					t.Fatalf("%s uses %s", tool.Name, comb)
				}
			}
		}
		// Both accepted forms of each tool.
		var pr contract.WorkspacePullResult
		json.Unmarshal([]byte(m.ok("ws_pull", map[string]any{"task_id": task.TaskID, "path": filepath.Join(canonTemp(t), "a")})), &pr)
		if pr.Selector != contract.TaskRefPrefix+task.TaskID || pr.Commit != *w.Commit {
			t.Fatalf("task-form pull %+v", pr)
		}
		json.Unmarshal([]byte(m.ok("ws_pull", map[string]any{"name": name, "ref": task.TaskID, "path": filepath.Join(canonTemp(t), "b")})), &pr)
		if pr.Selector != contract.TaskRefPrefix+task.TaskID {
			t.Fatalf("explicit-form pull %+v", pr)
		}
		st, err := contract.ParseTaskWorkspaceStatusResponse([]byte(m.ok("ws_status", map[string]any{"task_id": task.TaskID})))
		if err != nil || !st.Workspace.Available {
			t.Fatalf("task-form status %+v %v", st, err)
		}
		if text := m.ok("ws_status", map[string]any{"name": name}); !strings.Contains(text, contract.TaskRefPrefix+task.TaskID) {
			t.Fatalf("name-form status %s", text)
		}
		td, err := contract.ParseWorkspaceDiff([]byte(m.ok("ws_diff", map[string]any{"task_id": task.TaskID})), nil, 100)
		if err != nil || td.TargetCommit != *w.Commit || *td.BaseCommit != base {
			t.Fatalf("task-form diff %+v %v", td, err)
		}
		if _, err := contract.ParseWorkspaceDiff([]byte(m.ok("ws_diff", map[string]any{"name": name, "base": base, "target": task.TaskID})), nil, 100); err != nil {
			t.Fatal(err)
		}
		// Invalid mixes and incomplete forms never reach the plane.
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"ws_pull", map[string]any{"task_id": task.TaskID, "name": name}},
			{"ws_pull", map[string]any{"name": name}},
			{"ws_pull", map[string]any{"task_id": "t_x"}},
			{"ws_status", map[string]any{"task_id": task.TaskID, "limit": 1}},
			{"ws_status", map[string]any{"task_id": task.TaskID, "name": name}},
			{"ws_diff", map[string]any{"task_id": task.TaskID, "base": "empty"}},
			{"ws_diff", map[string]any{"name": name, "base": "empty"}},
			{"ws_diff", map[string]any{"task_id": task.TaskID, "after": "YQ=="}},
			{"ws_diff", map[string]any{}},
		} {
			m.fails(c.tool, c.args, contract.CodeInvalidArgument)
		}
		e := m.fails("ws_status", map[string]any{"task_id": r.run(t, "ws-z", nil, nil, fakeadapter.WorkspaceScript{}).TaskID}, contract.CodeInvalidArgument)
		if e.Details["reason"] != contract.ReasonNoTaskWorkspace {
			t.Fatalf("scratch status %+v", e)
		}
	})
	t.Run("content", func(t *testing.T) {
		// The sentinel exists only in workspace files: seeded from a local
		// folder (never through MCP) and copied by the child, so neither the
		// goal nor any output channel carries it.
		const sentinel = "SENTINEL-10C-WORKSPACE-ONLY"
		r.seed(t, name, inst, "secret", map[string]tfile{"secret.txt": tr(sentinel + "\n")})
		m := startMCP(t, "--plane", r.dep.URL, "--ca", r.dep.CA)
		r.awaitAccept("ws-z")
		goal := fakeadapter.WorkspaceGoal(fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Copy: "secret.txt", Target: "copy.txt"},
			fakeadapter.WorkspaceOp{Symlink: "link", Target: "copy.txt"})})
		v, err := contract.ParseDispatchResponse([]byte(m.ok("dispatch", dispatchArgs("id", "ws-z", goal, map[string]any{"workspace": name, "base": "secret",
			"workspace_instance": inst}))))
		if err != nil {
			t.Fatal(err)
		}
		r.await(t, v.TaskID)
		m.ok("task_wait", map[string]any{"task_ids": []string{v.TaskID}, "wait": "0"})
		status := m.ok("ws_status", map[string]any{"task_id": v.TaskID})
		m.ok("ws_diff", map[string]any{"task_id": v.TaskID})
		out := filepath.Join(canonTemp(t), "out")
		m.ok("ws_pull", map[string]any{"task_id": v.TaskID, "path": out})
		m.ok("task_show", map[string]any{"id": v.TaskID})
		if readFile(t, filepath.Join(out, "copy.txt")) != sentinel+"\n" {
			t.Fatal("the content was not delivered locally")
		}
		st, err := contract.ParseTaskWorkspaceStatusResponse([]byte(status))
		if err != nil {
			t.Fatal(err)
		}
		dto, _ := json.Marshal(st.Workspace.Result)
		if len(status) > contract.MaxWorkspaceResponse || len(dto) > contract.MaxWorkspaceDTOBytes {
			t.Fatalf("unbounded status %d / %d bytes", len(status), len(dto))
		}
		m.mu.Lock()
		frames := strings.Join(m.all, "")
		m.mu.Unlock()
		if strings.Contains(frames, sentinel) || strings.Contains(frames, out) {
			t.Fatal("workspace content or a local path reached an MCP frame")
		}
	})
	t.Run("lifetime", func(t *testing.T) {
		x := startPlaneProxy(t, &mcpPlane{url: r.dep.URL, ca: r.dep.CA})
		m := startMCP(t, x.trust()...)
		b := newBarrier(t)
		run := r.running(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Touch: b.path("in")}, fakeadapter.WorkspaceOp{WaitFor: b.path("go")})}, b, "in")
		release := x.hold("GET " + contract.PathTasks + "/")
		out1 := filepath.Join(canonTemp(t), "one")
		_, c1 := m.callAsync("ws_pull", map[string]any{"task_id": task.TaskID, "path": out1})
		x.await(t, "response GET "+contract.PathTasks+"/"+task.TaskID)
		id2, _ := m.callAsync("ws_status", map[string]any{"task_id": run.TaskID})
		x.await(t, "response GET "+contract.TaskWorkspacePath(run.TaskID))
		// Two calls in flight saturate the session.
		if e := m.fails("ws_status", map[string]any{"task_id": task.TaskID}, contract.CodeUnavailable); e.Details["reason"] != mcp.ReasonCapacity {
			t.Fatalf("third call %+v", e)
		}
		// Cancellation cancels the lookup, never the task.
		m.notify("notifications/cancelled", map[string]any{"requestId": json.Number(id2)})
		x.await(t, "canceled GET "+contract.TaskWorkspacePath(run.TaskID))
		if v, err := r.dep.Client.ShowTask(context.Background(), run.TaskID, 0); err != nil || contract.TaskTerminal(v.State) {
			t.Fatalf("the cancelled lookup changed the task: %+v %v", v.State, err)
		}
		release()
		if res := decodeTool(t, m.wait(c1)); res.isError || readFile(t, filepath.Join(out1, "t.txt")) != "t\n" {
			t.Fatalf("held pull %+v", res)
		}
		// The slots are released once the owned work joined.
		eventually(t, "a free slot", func() bool {
			res := m.call("ws_status", map[string]any{"task_id": task.TaskID})
			return !res.isError
		})
		// EOF cancels and joins a lookup in flight; nothing is published.
		release = x.hold("GET " + contract.PathTasks + "/")
		parent := canonTemp(t)
		m.callAsync("ws_pull", map[string]any{"task_id": task.TaskID, "path": filepath.Join(parent, "eof")})
		x.await(t, "response GET "+contract.PathTasks+"/"+task.TaskID)
		m.stdin.Close()
		select {
		case <-m.exited:
		case <-time.After(mcpWait):
			t.Fatal("the MCP server did not exit on EOF")
		}
		release()
		if !m.state.Success() {
			t.Fatalf("EOF exit %v", m.state)
		}
		if es, _ := os.ReadDir(parent); len(es) != 0 {
			t.Fatalf("EOF left %v", es)
		}
		b.release(t, "go")
		r.await(t, run.TaskID)
	})
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ---- FP-5 ----

// FP-5: task B on A's returned hash and instance sees A's edits with A's
// commit as its only parent; a sibling on the original base sees
// neither; an explicit continuation from a failed task's partial result;
// a stale instance never dispatches; no branch moves.
func TestWorkspaceMultiHop(t *testing.T) {
	r := sharedWsRig(t)
	name, inst := r.workspace(t)
	base := r.seed(t, name, inst, "", map[string]tfile{"base.txt": tr("base\n")})
	rep := canonTemp(t)
	a := r.cliDispatch(t, "ws-a", fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "a.txt", Data: "A\n"})}, "--workspace", name, "--workspace-instance", inst)
	ad := r.await(t, a.TaskID)
	wa := publishedWs(t, ad)
	// The recipe: published; result.workspace.commit and the binding.
	if ad.Result.Workspace.Publication != contract.PublicationPublished || *ad.Result.ResultCommit != *ad.Result.Workspace.Commit {
		t.Fatalf("A %+v", ad.Result)
	}
	b := r.cliDispatch(t, "ws-z", fakeadapter.WorkspaceScript{Report: filepath.Join(rep, "b.json"), Ops: ops(fakeadapter.WorkspaceOp{Write: "b.txt", Data: "B\n"})},
		"--workspace", ad.WorkspaceBinding.Name, "--base", *ad.Result.Workspace.Commit, "--workspace-instance", ad.WorkspaceBinding.Instance)
	if bb := b.WorkspaceBinding; bb.Instance != inst || *bb.BaseCommit != *wa.Commit || bb.BaseSelector != *wa.Commit {
		t.Fatalf("B binding %+v", bb)
	}
	bd := r.await(t, b.TaskID)
	wb := publishedWs(t, bd)
	if f := readWsReport(t, filepath.Join(rep, "b.json")).Files; f["a.txt"] == "" || f["base.txt"] == "" {
		t.Fatalf("B saw %v", f)
	}
	files, cb := r.files(t, name, inst, *wb.Commit)
	wantResultCommit(t, cb, bd, contract.TaskSucceeded, plumbing.NewHash(*wa.Commit))
	if string(files["a.txt"].Content) != "A\n" || string(files["b.txt"].Content) != "B\n" {
		t.Fatalf("B tree %v", files)
	}
	// A sibling on the original base sees neither A nor B.
	c := r.cliDispatch(t, "ws-a", fakeadapter.WorkspaceScript{Report: filepath.Join(rep, "c.json")}, "--workspace", name, "--workspace-instance", inst)
	cd := r.await(t, c.TaskID)
	if f := readWsReport(t, filepath.Join(rep, "c.json")).Files; f["a.txt"] != "" || f["b.txt"] != "" || f["base.txt"] == "" || *cd.WorkspaceBinding.BaseCommit != base {
		t.Fatalf("sibling saw %v", f)
	}
	// A failed task's partial result, continued explicitly.
	a2 := r.cliDispatch(t, "ws-z", fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "partial.txt", Data: "p\n"}), Exit: 1}, "--workspace", name)
	a2d := r.await(t, a2.TaskID)
	wa2 := publishedWs(t, a2d)
	if a2d.State != contract.TaskFailed {
		t.Fatalf("A2 %s", a2d.State)
	}
	d := r.cliDispatch(t, "ws-a", fakeadapter.WorkspaceScript{Report: filepath.Join(rep, "d.json")}, "--workspace", name, "--base", *wa2.Commit, "--workspace-instance", inst)
	dd := r.await(t, d.TaskID)
	_, cdd := r.files(t, name, inst, *publishedWs(t, dd).Commit)
	if f := readWsReport(t, filepath.Join(rep, "d.json")).Files; f["partial.txt"] == "" || len(cdd.ParentHashes) != 1 || cdd.ParentHashes[0].String() != *wa2.Commit {
		t.Fatalf("partial continuation %v %v", f, cdd.ParentHashes)
	}
	// The guard: another instance never dispatches.
	before := r.taskCount(t)
	wantExit(t, r.cli(t, "dispatch", "--role-id", "ws-a", "--goal", "g", "--acceptance", "a", "--workspace", name, "--base", *wa.Commit,
		"--workspace-instance", strings.Repeat("9", 32)), 4, "nothing was dispatched")
	if r.taskCount(t) != before {
		t.Fatal("a stale instance dispatched")
	}
	// No branch moved; every result ref stays.
	refs := r.refs(t, name, inst)
	if refs["refs/heads/main"].String() != base {
		t.Fatalf("main moved to %s", refs["refs/heads/main"])
	}
	for _, id := range []string{a.TaskID, b.TaskID, c.TaskID, a2.TaskID, d.TaskID} {
		if _, ok := refs[contract.TaskRefPrefix+id]; !ok {
			t.Fatalf("result ref of %s missing", id)
		}
	}
	// An unpublished result is no base: a failed publication has no task
	// ref. A prune before admission makes A's hash unavailable: the
	// dispatch fails normally, nothing reruns A or picks another base.
	f := r.run(t, "ws-a", sp(name), nil, fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "x/.git/HEAD", Data: "ref: refs/heads/main\n"})})
	if f.Result.Workspace.Publication != contract.PublicationFailed {
		t.Fatalf("unpublished %+v", f.Result.Workspace)
	}
	before = r.taskCount(t)
	wantExit(t, r.cli(t, "dispatch", "--role-id", "ws-a", "--goal", "g", "--acceptance", "a", "--workspace", name, "--base", f.TaskID), 3, "nothing was dispatched")
	okOut(t, "prune", r.cli(t, "ws", "prune", "--instance", inst, "--before", future(), name))
	wantExit(t, r.cli(t, "dispatch", "--role-id", "ws-a", "--goal", "g", "--acceptance", "a", "--workspace", name, "--base", *wa.Commit,
		"--workspace-instance", inst), 3, "nothing was dispatched")
	if r.taskCount(t) != before || r.refs(t, name, inst)["refs/heads/main"].String() != base {
		t.Fatal("a refused continuation dispatched or moved main")
	}
}

// ---- FP-6 ----

// workflowBlock extracts the guide's executable workflow lines.
func workflowBlock(t *testing.T, doc string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, "<!-- workflow:begin -->")
	block, _, ok2 := strings.Cut(rest, "<!-- workflow:end -->")
	if !ok || !ok2 {
		t.Fatal("docs/workspaces.md has no workflow block")
	}
	var lines []string
	for _, l := range strings.Split(block, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "```") {
			lines = append(lines, l)
		}
	}
	return lines
}

// splitArgs splits a command line on spaces, keeping double-quoted words.
func splitArgs(line string) []string {
	var out []string
	var cur strings.Builder
	quoted, have := false, false
	for _, r := range line {
		switch {
		case r == '"':
			quoted, have = !quoted, true
		case r == ' ' && !quoted:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

var (
	assignLine = regexp.MustCompile(`^([A-Z_]+)=(.*)$`)
	noteRule   = regexp.MustCompile(`note the printed ([a-z_]+) as ([A-Z_]+)`)
	varRef     = regexp.MustCompile(`\$([A-Z_]+)`)
)

// valueOf returns a key: value line's value.
func valueOf(out, key string) (string, bool) {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, key+": "); ok {
			return v, true
		}
	}
	return "", false
}

// M4 checks and evidence fields the guide must name.
var (
	m4Checks = []string{"M4-1 Laptop push", "M4-2 Remote cwd is the base", "M4-3 A metadata and diff", "M4-4 Second hop", "M4-5 Parallel sibling",
		"M4-6 Partial results", "M4-7 Lost task", "M4-8 Restart around publication", "M4-9 Laptop pull", "M4-10 External delivery (optional)"}
	m4Phrases = []string{"laptop", "files equal the selected base commit", "`ws diff TASK_ID`", "A's exact result hash and instance", "neither A's nor B's changes",
		"a failed, a cancelled and a timed-out task", "has no ref", "one stable receipt", "dirty checkout, and `main` never moves", "their own `git push`"}
	m4Evidence = []string{"the OS of both machines", "the Callsheet and adapter versions", "the task IDs", "the workspace instance", "the base and result hashes",
		"the terminal state and publication status", "Never record file contents or credentials"}
)

// FP-6: the operator guide's workflow runs as written; the local
// sequence runs with git absent from the CLI's PATH and records its
// metadata; the guide names every manual M4 check and evidence field
// (checklist presence only: no remote session is performed here).
func TestWorkspaceOperatorWorkflow(t *testing.T) {
	r := sharedWsRig(t)
	doc := string(repoFile(t, "docs/workspaces.md"))
	t.Run("documentation", func(t *testing.T) {
		name := r.freshName("doc")
		dir := canonTemp(t)
		os.MkdirAll(filepath.Join(dir, name), 0o755)
		os.WriteFile(filepath.Join(dir, name, "readme.txt"), []byte("docs\n"), 0o644)
		repo := mkRepo(t, map[string]tfile{"mine": tr("local\n")})
		repo.write("mine", tr("uncommitted\n"))
		before := treeDigest(t, repo.root, ".git/objects", ".git/refs/callsheet")
		vars := map[string]string{"TRUST": "--plane " + r.dep.URL + " --ca " + r.dep.CA, "ROLE": "ws-a"}
		preset := map[string]bool{"TRUST": true, "ROLE": true}
		var pending [][2]string
		ran, skipped := 0, 0
		for _, line := range workflowBlock(t, doc) {
			if m := assignLine.FindStringSubmatch(line); m != nil {
				if !preset[m[1]] {
					vars[m[1]] = strings.Trim(m[2], `"`)
				}
				delete(preset, m[1])
				continue
			}
			if strings.HasPrefix(line, "#") {
				for _, n := range noteRule.FindAllStringSubmatch(line, -1) {
					pending = append(pending, [2]string{n[1], n[2]})
				}
				continue
			}
			if strings.HasPrefix(line, "git ") {
				skipped++ // the user's own optional delivery step
				continue
			}
			if !strings.HasPrefix(line, "callsheet ") {
				t.Fatalf("unexpected workflow line %q", line)
			}
			line = strings.ReplaceAll(line, "~/src/myproject", repo.root)
			line = strings.ReplaceAll(line, "myproject", name)
			line = varRef.ReplaceAllStringFunc(line, func(v string) string {
				val, ok := vars[v[1:]]
				if !ok {
					t.Fatalf("workflow uses %s before it is noted", v)
				}
				return val
			})
			args := splitArgs(line)[1:]
			res := runBin(t, r.dep.Bin, dir, r.env, args...)
			if res.code != 0 {
				t.Fatalf("%s: %+v", line, res)
			}
			ran++
			var left [][2]string
			for _, p := range pending {
				if v, ok := valueOf(res.stdout, p[0]); ok {
					vars[p[1]] = v
				} else {
					left = append(left, p)
				}
			}
			pending = left
		}
		if len(pending) != 0 || len(preset) != 0 || ran != 12 || skipped != 1 {
			t.Fatalf("workflow incomplete: pending %v, unset %v, %d run, %d skipped", pending, preset, ran, skipped)
		}
		// The documented chain: B's only parent is A's result commit; the
		// dirty checkout is untouched; the folder delivery has the files.
		bv, err := r.dep.Client.ShowTask(context.Background(), vars["TASK_B"], 0)
		if err != nil {
			t.Fatal(err)
		}
		wb := publishedWs(t, bv)
		_, cb := r.files(t, name, vars["INSTANCE"], *wb.Commit)
		if len(cb.ParentHashes) != 1 || cb.ParentHashes[0].String() != vars["COMMIT_A"] || *bv.WorkspaceBinding.BaseCommit != vars["COMMIT_A"] {
			t.Fatalf("B parents %v, A %s", cb.ParentHashes, vars["COMMIT_A"])
		}
		local := filepath.Join(repo.root, ".git", "refs", "callsheet", name, "tasks", vars["TASK_B"])
		if readFile(t, local) != *wb.Commit+"\n" || treeDigest(t, repo.root, ".git/objects", ".git/refs/callsheet") != before {
			t.Fatal("repository delivery")
		}
		if readFile(t, filepath.Join(dir, "delivery", "readme.txt")) != "docs\n" {
			t.Fatal("folder delivery")
		}
	})
	t.Run("local", func(t *testing.T) {
		empty := testkit.EmptyDir(t)
		env := []string{"PATH=" + empty, "HOME=" + canonTemp(t)}
		dir := canonTemp(t)
		name := r.freshName("local")
		folder := mkFolder(t, map[string]tfile{"app.txt": tr("app\n")})
		var v contract.WorkspaceView
		json.Unmarshal(okOut(t, "ws create", r.cliEnv(t, dir, env, "ws", "create", "--json", name)), &v)
		push, err := contract.ParseWorkspacePushResult(okOut(t, "ws push", r.cliEnv(t, dir, env, "ws", "push", "--json", "--instance", v.Instance, name, folder)))
		if err != nil {
			t.Fatal(err)
		}
		rep := canonTemp(t)
		dispatch := func(s fakeadapter.WorkspaceScript, extra ...string) contract.TaskView {
			t.Helper()
			r.awaitAccept("ws-z")
			tv, err := contract.ParseDispatchResponse(okOut(t, "dispatch", r.cliEnv(t, dir, env, append([]string{"dispatch", "--json", "--role-id", "ws-z",
				"--goal", fakeadapter.WorkspaceGoal(s), "--acceptance", "fixture", "--workspace", name, "--workspace-instance", v.Instance}, extra...)...)))
			if err != nil {
				t.Fatal(err)
			}
			okOut(t, "task wait", r.cliEnv(t, dir, env, "task", "wait", "--wait", "2m", tv.TaskID))
			shown, err := contract.ParseTaskShowResponse(okOut(t, "task show", r.cliEnv(t, dir, env, "task", "show", "--json", "--lines", "0", tv.TaskID)))
			if err != nil {
				t.Fatal(err)
			}
			return shown
		}
		a := dispatch(fakeadapter.WorkspaceScript{Ops: ops(fakeadapter.WorkspaceOp{Write: "feature.txt", Data: "feature\n"})})
		wa := publishedWs(t, a)
		okOut(t, "ws status", r.cliEnv(t, dir, env, "ws", "status", a.TaskID))
		okOut(t, "ws diff", r.cliEnv(t, dir, env, "ws", "diff", a.TaskID))
		b := dispatch(fakeadapter.WorkspaceScript{Report: filepath.Join(rep, "b.json")}, "--base", *wa.Commit)
		wb := publishedWs(t, b)
		if f := readWsReport(t, filepath.Join(rep, "b.json")).Files; f["feature.txt"] == "" {
			t.Fatalf("B did not see A's change: %v", f)
		}
		repo := mkRepo(t, map[string]tfile{"mine": tr("local\n")})
		repo.write("mine", tr("dirty\n"))
		before := treeDigest(t, repo.root, ".git/objects", ".git/refs/callsheet")
		pr, err := contract.ParseWorkspacePullResult(okOut(t, "pull repo", r.cliEnv(t, dir, env, "ws", "pull", "--json", b.TaskID, repo.root)))
		if err != nil || pr.Commit != *wb.Commit || treeDigest(t, repo.root, ".git/objects", ".git/refs/callsheet") != before {
			t.Fatalf("repository delivery %+v %v", pr, err)
		}
		out := filepath.Join(dir, "delivery")
		okOut(t, "pull folder", r.cliEnv(t, dir, env, "ws", "pull", b.TaskID, out))
		if readFile(t, filepath.Join(out, "feature.txt")) != "feature\n" || readFile(t, filepath.Join(out, "app.txt")) != "app\n" {
			t.Fatal("folder delivery")
		}
		// The evidence record the manual session keeps: metadata only.
		version := strings.TrimSpace(string(okOut(t, "version", runBin(t, r.dep.Bin, dir, env, "version"))))
		evidence := map[string]any{"os": runtime.GOOS + "/" + runtime.GOARCH, "callsheet": version, "workspace": name, "instance": v.Instance,
			"seed": push.Commit, "tasks": []map[string]any{
				{"id": a.TaskID, "base": *a.WorkspaceBinding.BaseCommit, "result": *wa.Commit, "state": a.State, "publication": wa.Publication},
				{"id": b.TaskID, "base": *b.WorkspaceBinding.BaseCommit, "result": *wb.Commit, "state": b.State, "publication": wb.Publication}}}
		rec, _ := json.Marshal(evidence)
		if strings.Contains(string(rec), "feature\n") || version == "" || a.State != contract.TaskSucceeded || *b.WorkspaceBinding.BaseCommit != *wa.Commit {
			t.Fatalf("evidence %s", rec)
		}
		t.Logf("local workflow evidence: %s", rec)
	})
	t.Run("manual_checklist", func(t *testing.T) {
		_, section, ok := strings.Cut(doc, "## Manual M4 checks")
		section, _, _ = strings.Cut(section, "\n## ")
		if !ok || !strings.Contains(section, "does not perform or qualify them") {
			t.Fatal("the guide has no manual M4 section stating it is not automated")
		}
		for i, c := range m4Checks {
			if !strings.Contains(section, "**"+c+":**") || !strings.Contains(section, m4Phrases[i]) {
				t.Fatalf("the guide lacks %q (%q)", c, m4Phrases[i])
			}
		}
		for _, e := range m4Evidence {
			if !strings.Contains(section, e) {
				t.Fatalf("the guide lacks the evidence field %q", e)
			}
		}
	})
}
