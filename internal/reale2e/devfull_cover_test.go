package reale2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestDevFullConcurrentControl sends control requests over the real
// socket from many goroutines at once while the supervisor runs: the loop
// serves them one at a time (go test -race observes no shared access),
// exactly one wait is admitted and every other answer is a fixed code.
func TestDevFullConcurrentControl(t *testing.T) {
	t.Run("idempotent", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.fault = "concurrent-idempotent"
		r.expect(0, "")
		counts := map[string]int{}
		for _, c := range r.codes {
			counts[c]++
		}
		// Every dev-next made once the slot was observed answers pending:
		// the occupied plan is never named again (one prepared per task).
		if counts[CodeAccepted] < 1 || counts[CodeAlreadyObserved] != 8 || counts[CodePending] != 8 || counts[CodePrepared] != 4 {
			t.Fatalf("codes %v", counts)
		}
	})
	t.Run("duplicate_waits", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}})
		r.fault = "concurrent-wait"
		r.expect(1, devDuplicateWait)
		// The 24 concurrent answers: eight waits, observes and dev-nexts.
		if len(r.codes) != 26 || r.codes[0] != CodePrepared || r.codes[1] != CodeAccepted {
			t.Fatalf("codes %v", r.codes)
		}
		counts := map[string]int{}
		for _, c := range r.codes[2:] {
			counts[c]++
		}
		if counts[CodeAccepted] != 1 || counts[CodeDuplicateWait] != 1 || counts[CodeAccepted]+counts[CodeDuplicateWait]+counts[CodeClosing]+
			counts[CodeAlreadyObserved]+counts[CodePending]+counts["transport"] != 24 || counts[CodePrepared] != 0 {
			t.Fatalf("codes %v", counts)
		}
	})
}

// TestDevFullProductionPuller pulls a published result with the product's
// own transfer code over the fake transfer plane: an ordinary non-bare
// repository is accepted; the bare repository the previous inspection
// used is refused at the production boundary.
func TestDevFullProductionPuller(t *testing.T) {
	ctx := context.Background()
	p := newFakePlane(t, newAutoClock())
	if _, err := p.CreateWorkspace(ctx, "e2e-ws"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddRole(ctx, contract.RoleConfig{ID: "designer", Name: "designer", Node: hexID("n_", 1), Adapter: "codex", Model: "gpt-6-astra", Effort: "low",
		Concurrency: 1, Timeout: FeatureTaskBound, HasTimeout: true}); err != nil {
		t.Fatal(err)
	}
	seed, _ := writeCommit(t, p.hub, map[string][]byte{"main.go": []byte("package main\n")})
	p.pushSeed(seed)
	ws, inst, base := "e2e-ws", p.ws.Instance, seed.String()
	v, err := p.Dispatch(ctx, contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: "designer"}, Goal: "g", Acceptance: "a",
		RequestedBy: requester, Workspace: &ws, Base: &base, WorkspaceInstance: &inst})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := writeCommit(t, p.hub, map[string][]byte{"main.go": []byte("package main\n"), "design.md": []byte("# d\n")}, seed)
	final := "STATUS: DESIGN_DRAFT\n"
	p.finish(v.TaskID, contract.TaskSucceeded, &final, false, &result)
	puller := productionPuller(p)
	bare := filepath.Join(t.TempDir(), "bare")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	if err := puller.PullResult(ctx, v.TaskID, bare); err == nil || !strings.Contains(err.Error(), "a bare repository") {
		t.Fatalf("a bare inspection repository was not refused: %v", err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if _, err := git.PlainInit(plain, false); err != nil {
		t.Fatal(err)
	}
	if err := puller.PullResult(ctx, v.TaskID, plain); err != nil {
		t.Fatalf("an ordinary inspection repository: %v", err)
	}
	repo, err := git.PlainOpen(plain)
	if err != nil {
		t.Fatal(err)
	}
	files, parent, _, err := readCommitTree(repo, result.String())
	if err != nil || parent != seed.String() || len(files) != 2 {
		t.Fatalf("pulled %v %s %d", err, parent, len(files))
	}
	if err := puller.PullResult(ctx, hexID("t_", 99), filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("an unknown task was pulled")
	}
}

func TestDevFullControlSocket(t *testing.T) {
	rt := shortTemp(t)
	body := fmt.Sprintf("{\"schema\":%q,\"run_id\":%q,\"callsheet\":\"/bin/callsheet-missing\",\"plane\":\"https://127.0.0.1:9\",\"ca\":\"/tmp/ca.pem\"}\n", DevRunSchema, helperRunID)
	write(t, filepath.Join(rt, RunFileName), body)
	write(t, filepath.Join(rt, ownerMarker), helperRunID+"\n")
	if err := os.MkdirAll(waitDir(rt, 0), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(rt, "plan.json")
	write(t, doc, "{}\n")
	sum := sha256Hex([]byte("{}\n"))
	id := "t_" + strings.Repeat("cd", 16)
	next := []controlResponse{{OK: true, Code: CodePrepared, Path: doc, Digest: sum, Ordinal: 1, Role: "designer", Mode: modeDraft},
		{OK: true, Code: CodePaused, Summary: "gate=owner generation=1"}, devCode(CodeComplete), devCode(CodePending), devCode(CodeSeedUnpublished)}
	srv, err := listenDevControl(rt, helperRunID, func(req ControlRequest) controlResponse {
		switch req.Op {
		case opDevNext:
			resp := next[0]
			next = next[1:]
			return resp
		case opDevObserve, opDevWait:
			if req.Task != id || req.Ordinal != "1" {
				return devCode(CodeTaskMismatch)
			}
			return devCode(CodeAccepted)
		}
		return devCode(CodeInvalidRequest)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.close)
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := MainWith(Host{Args: args, Stdout: &out, Stderr: &errb})
		return code, out.String(), errb.String()
	}
	for _, want := range []struct {
		code int
		out  string
	}{{0, "prepared " + doc + " " + sum + "\n"}, {0, "paused gate=owner generation=1\n"}, {0, "complete\n"}, {0, "pending\n"}, {1, "seed_unpublished\n"}} {
		if code, out, errOut := run("dev-next", "--run", rt); code != want.code || out != want.out {
			t.Fatalf("dev-next %d %q %s", code, out, errOut)
		}
	}
	if code, out, _ := run("dev-observe", "--run", rt, "--ordinal", "1", "--task", id); code != 0 || out != "accepted\n" {
		t.Fatalf("dev-observe %d %q", code, out)
	}
	if code, _, errOut := run("dev-observe", "--run", rt, "--ordinal", "1", "--task", hexID("t_", 3)); code != 1 || !strings.Contains(errOut, "task_mismatch") {
		t.Fatalf("refused observe %d %s", code, errOut)
	}
	// The accepted wait runs the CLI wait; a missing executable is an
	// I/O error, never a success.
	if code, _, _ := run("dev-wait-task", "--run", rt, "--ordinal", "1", "--task", id); code != 2 {
		t.Fatalf("wait without a watcher returned %d", code)
	}
	// Old and new helpers refuse each other's run files.
	if code, _, _ := run("observe", "--run", rt, "--task", id, "--hop", "designer"); code != 2 {
		t.Fatalf("the legacy helper accepted a development run file: %d", code)
	}
	legacy := shortTemp(t)
	write(t, filepath.Join(legacy, RunFileName), runFileJSON(helperRunID, "/bin/callsheet"))
	write(t, filepath.Join(legacy, ownerMarker), helperRunID+"\n")
	if code, _, _ := run("dev-next", "--run", legacy); code != 2 {
		t.Fatalf("the development helper accepted a legacy run file: %d", code)
	}
	if _, err := liveDevRuntime("relative"); err == nil {
		t.Fatal("relative runtime")
	}
	if _, err := liveDevRuntime(filepath.Join(rt, RunFileName)); err == nil {
		t.Fatal("runtime file")
	}
	write(t, filepath.Join(rt, ownerMarker), "other\n")
	if _, err := liveDevRuntime(rt); err == nil {
		t.Fatal("marker")
	}
	lonely := shortTemp(t)
	write(t, filepath.Join(lonely, RunFileName), body)
	if _, err := devControlCall(lonely, ControlRequest{Op: opDevNext}); err == nil {
		t.Fatal("missing socket")
	}
	if _, err := devControlCall(t.TempDir(), ControlRequest{Op: opDevNext}); err == nil {
		t.Fatal("missing run file")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	write(t, blocked, "x")
	if _, _, err := exposeDispatch(filepath.Join(blocked, "child"), 1, "goal", []byte("{}")); err == nil {
		t.Fatal("dispatch directory")
	}
}

func TestDevFullTreeEdges(t *testing.T) {
	st := memory.NewStorage()
	repo, err := git.Init(st, memfs.New())
	if err != nil {
		t.Fatal(err)
	}
	root, _ := writeCommit(t, st, map[string][]byte{"main.go": []byte("package main\n")})
	if _, _, _, err := readCommitTree(repo, root.String()); err == nil {
		t.Fatal("a root commit")
	}
	if _, err := loadBaseTree(repo, strings.Repeat("ab", 20)); err == nil {
		t.Fatal("missing base")
	}
	if _, _, _, err := readCommitTree(repo, strings.Repeat("ab", 20)); err == nil {
		t.Fatal("missing commit")
	}
	big, _ := writeCommit(t, st, map[string][]byte{"design.md": bytes.Repeat([]byte("a"), int(maxArtifactBytes)+1)}, root)
	if _, _, _, err := readCommitTree(repo, big.String()); err == nil {
		t.Fatal("big blob")
	}
	agg := map[string][]byte{}
	for _, name := range devArtifacts[:5] {
		agg[name] = bytes.Repeat([]byte("a"), maxArtifactBytes)
	}
	over, _ := writeCommit(t, st, agg, root)
	if _, _, _, err := readCommitTree(repo, over.String()); err == nil || !strings.Contains(err.Error(), "artifacts") {
		t.Fatalf("aggregate from sizes: %v", err)
	}
	src, _ := writeCommit(t, st, map[string][]byte{"FEATURE.md": bytes.Repeat([]byte("a"), maxSourceFixtureBytes)}, root)
	if _, _, _, err := readCommitTree(repo, src.String()); err == nil {
		t.Fatal("source budget")
	}
	exec, _ := writeCommitMode(t, st, map[string][]byte{"main.go": []byte("package main\n")}, map[string]filemode.FileMode{"main.go": filemode.Executable}, root)
	if _, _, _, err := readCommitTree(repo, exec.String()); err == nil {
		t.Fatal("executable")
	}
	if _, err := loadBaseTree(repo, exec.String()); err == nil {
		t.Fatal("base executable")
	}
	scratch := writeNestedCommit(t, st, map[string][]byte{"review.md": []byte("r\n"), reviewScratch + "/t/after/main.go": []byte("x")}, root)
	if _, _, _, err := readCommitTree(repo, scratch.String()); err == nil || !strings.Contains(err.Error(), "scratch remnant") {
		t.Fatalf("scratch %v", err)
	}
	nested := writeNestedCommit(t, st, map[string][]byte{"dir/main.go": []byte("x")}, root)
	if _, _, _, err := readCommitTree(repo, nested.String()); err == nil {
		t.Fatal("nested directory")
	}
	missingParent, _ := writeCommit(t, st, map[string][]byte{"main.go": []byte("package main\n")}, plumbing.NewHash(strings.Repeat("cd", 20)))
	if _, _, _, err := readCommitTree(repo, missingParent.String()); err == nil {
		t.Fatal("missing parent")
	}
	good, goodTree := writeCommit(t, st, map[string][]byte{"main.go": []byte("package main\n")}, root)
	files, parent, tree, err := readCommitTree(repo, good.String())
	if err != nil || parent != root.String() || tree != goodTree.String() || len(files) != 1 {
		t.Fatalf("good %v", err)
	}
	// The evidence repository holds the seed and task refs within bound.
	dir := filepath.Join(t.TempDir(), "repo.git")
	if err := writeDevEvidenceRepo(dir, st, root, []devCommit{{Ordinal: 1, Commit: good.String()}}); err != nil {
		t.Fatal(err)
	}
	ev, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]plumbing.Hash{"refs/heads/seed": root, "refs/heads/task-01": good} {
		r, err := ev.Reference(plumbing.ReferenceName(ref), true)
		if err != nil || r.Hash() != want {
			t.Fatalf("%s %v", ref, err)
		}
	}
	if err := writeDevEvidenceRepo(filepath.Join(t.TempDir(), "x.git"), st, root, []devCommit{{Ordinal: 1, Commit: strings.Repeat("ef", 20)}}); err == nil {
		t.Fatal("a missing commit was written")
	}
}
