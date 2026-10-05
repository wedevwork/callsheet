package containeracceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

// FinalMessage is the fake worker's fixed decoded final answer.
const FinalMessage = "fake task completed"

// Boundary labels of the named observation boundaries (the host compares
// them with its own catalog).
const (
	BoundaryRegistryRestart    = "registry_revision_restart_node_a_before_dispatch"
	BoundarySiblingWhileHeld   = "sibling_while_continuation_held"
	BoundaryWrittenBeforeStop  = "written_barrier_before_stop"
	BoundaryLostBeforeReturn   = "sidecar_restart_after_written_barrier_before_worker_return"
	BoundaryPublicationRestart = "after_acknowledged_publication_before_coordinator_delivery"
)

var boundaries = map[string]string{
	CaseCoordinator:        BoundaryRegistryRestart,
	PhasePrepare:           BoundaryRegistryRestart,
	CaseContinuation:       BoundarySiblingWhileHeld,
	PhaseSibling:           BoundarySiblingWhileHeld,
	CasePartialCancelled:   BoundaryWrittenBeforeStop,
	CasePartialTimedOut:    BoundaryWrittenBeforeStop,
	CaseLost:               BoundaryLostBeforeReturn,
	CasePublicationRestart: BoundaryPublicationRestart,
}

// roleRow is one sample registration row, in registration order.
type roleRow struct {
	id, label, alias string
}

// sampleRoles are the four scenario roles on the two sidecars.
var sampleRoles = []roleRow{
	{"proof-a", "design", "a"},
	{"proof-code", "code", "a"},
	{"proof-b", "design-review", "b"},
	{"proof-code-review", "code-review", "b"},
}

// preparation is the prepared session: the frozen a/b node bindings.
type preparation struct {
	bindings map[string]string
}

// observed is one checked task observation with its evidence label.
type observed struct {
	label string
	view  contract.TaskView
}

// task renders the evidence task object from the actual decoded view:
// workspace_instance and base_commit from the immutable admission binding,
// the result and publication from result.workspace; only the fixed answer
// may enter final_message.
func (o observed) task() Task {
	v := o.view
	t := Task{Label: o.label, RoleID: v.Role.ID, NodeID: v.Role.Node, TaskID: v.TaskID, TerminalState: v.State}
	cp := func(p *string) *string {
		if p == nil {
			return nil
		}
		s := *p
		return &s
	}
	if b := v.WorkspaceBinding; b != nil {
		inst := b.Instance
		t.WorkspaceInstance, t.BaseCommit = &inst, cp(b.BaseCommit)
	}
	if r := v.Result; r != nil {
		if r.ExitCode != nil {
			n := *r.ExitCode
			t.ExitCode = &n
		}
		if r.FinalMessage != nil {
			msg := *r.FinalMessage
			if msg != FinalMessage {
				msg = "<unexpected final message>"
			}
			t.FinalMessage = &msg
		}
		if w := r.Workspace; w != nil {
			pub := w.Publication
			t.PublicationStatus, t.ResultCommit, t.ResultRef = &pub, cp(w.Commit), cp(w.Ref)
		}
	}
	return t
}

// workspaceFixture is a pushed workspace: its name, instance, pushed base
// and the base's files.
type workspaceFixture struct {
	name, instance, base string
	files                map[string]string
}

// scenarios maps every case and phase ID to its implementation.
func (s *Suite) scenarios() map[string]func(context.Context, *CaseRecord) error {
	return map[string]func(context.Context, *CaseRecord) error{
		CaseRuntime:            s.caseRuntime,
		PhasePrepare:           func(ctx context.Context, _ *CaseRecord) error { return s.ensurePrepared(ctx) },
		PhaseGoalAnswer:        func(ctx context.Context, _ *CaseRecord) error { return s.ensureAnswers(ctx) },
		CaseCoordinator:        s.caseCoordinator,
		CaseSampleFlow:         s.caseSampleFlow,
		CasePublication:        s.casePublication,
		PhaseSibling:           func(ctx context.Context, _ *CaseRecord) error { return s.ensureSibling(ctx) },
		PhaseContinuation:      func(ctx context.Context, _ *CaseRecord) error { return s.ensureContinuation(ctx) },
		CaseContinuation:       s.caseContinuation,
		CasePartialFailed:      s.partialCase("failed"),
		CasePartialCancelled:   s.partialCase("cancelled"),
		CasePartialTimedOut:    s.partialCase("timed_out"),
		CasePartialResults:     s.casePartialResults,
		CaseLost:               s.caseLost,
		CasePublicationRestart: s.casePublicationRestart,
		CaseDirtyPull:          s.caseDirtyPull,
		CaseClaims:             s.caseClaims,
		CaseEvidence:           func(context.Context, *CaseRecord) error { return nil },
	}
}

// ---- FP-1: runtime ----

// caseRuntime checks the binaries' hashes, the real running plane and
// sidecars, the isolated per-process state, verified loopback TLS (the
// real CA accepted, a foreign CA refused) and the enrolled nodes, then
// freezes the a/b node bindings after preparation.
func (s *Suite) caseRuntime(ctx context.Context, rec *CaseRecord) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	for key, p := range map[string]string{HashCallsheet: s.cfg.CallsheetPath, HashFakeAdapter: s.cfg.FakeAdapterPath, HashAcceptanceTest: self} {
		got, err := fileSHA256(p)
		if err != nil {
			return err
		}
		if got != s.cfg.ExpectedHashes[key] {
			return fmt.Errorf("binary %s hash %s, want %s", key, got, s.cfg.ExpectedHashes[key])
		}
	}
	if !strings.HasPrefix(s.url, "https://127.0.0.1:") {
		return fmt.Errorf("plane origin %s is not loopback https", s.url)
	}
	if !strings.HasPrefix(s.root, s.cfg.WorkDir+string(filepath.Separator)) {
		return fmt.Errorf("runtime state %s is outside the work root", s.root)
	}
	states := map[string]bool{}
	for alias, sc := range s.nodes {
		if sc.PID() <= 0 || !strings.HasPrefix(sc.State, s.root) || states[sc.State] {
			return fmt.Errorf("node %s has no isolated running sidecar (%d, %s)", alias, sc.PID(), sc.State)
		}
		states[sc.State] = true
	}
	b, err := s.cliOK(ctx, "node", "ls", "--json")
	if err != nil {
		return err
	}
	nodes, err := contract.ParseNodeListResponse(b)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, n := range nodes {
		ids[n.ID] = true
	}
	if len(nodes) != 2 || !ids[s.nodes["a"].NodeID] || !ids[s.nodes["b"].NodeID] {
		return fmt.Errorf("node ls lists %v, want the two enrolled nodes", ids)
	}
	foreign, err := testkit.NewFixtureCA()
	if err != nil {
		return err
	}
	bad := filepath.Join(s.root, "foreign-ca.crt")
	if err := os.WriteFile(bad, foreign.CertPEM, 0o600); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	r, err := s.cliRaw(cctx, "node", "ls", "--plane", s.url, "--ca", bad)
	if err != nil {
		return err
	}
	if r.code != 6 {
		return fmt.Errorf("a foreign CA was not refused: exit %d", r.code)
	}
	if err := s.ensurePrepared(ctx); err != nil {
		return err
	}
	rec.NodeBindings = map[string]string{"a": s.prep.bindings["a"], "b": s.prep.bindings["b"]}
	return nil
}

// ---- FP-2: preparation and goal-and-answer ----

// manual is a role's fixture manual path.
func (s *Suite) manual(role, kind string) string {
	return filepath.Join(s.cfg.FixtureDir, "manuals", role, kind+".md")
}

// ensurePrepared registers the four roles in order through the CLI,
// applies the documented registry-revision workaround (restart node a,
// whose final registration precedes node b's), and requires every role's
// can_accept and registered fields through role show before any dispatch.
func (s *Suite) ensurePrepared(ctx context.Context) error {
	return s.ensure("prepare", func() error {
		for _, r := range sampleRoles {
			for _, kind := range []string{"instruction", "runbook"} {
				if st, err := os.Stat(s.manual(r.id, kind)); err != nil || !st.Mode().IsRegular() {
					return fmt.Errorf("manual %s is missing", s.manual(r.id, kind))
				}
			}
		}
		check := func(v contract.RoleView, r roleRow) error {
			node := s.nodes[r.alias].NodeID
			if v.ID != r.id || v.Name != r.label || v.Node != node || v.Adapter != "fake" || v.Instruction != s.manual(r.id, "instruction") ||
				v.Runbook != s.manual(r.id, "runbook") || v.Model != "example-model" || v.Effort != "medium" || v.Concurrency != 2 {
				return fmt.Errorf("role %s registered as %+v", r.id, v.RoleConfig)
			}
			return nil
		}
		for _, r := range sampleRoles {
			b, err := s.cliOK(ctx, "role", "add", r.id, "--name", r.label, "--node", s.nodes[r.alias].NodeID, "--adapter", "fake",
				"--instruction", s.manual(r.id, "instruction"), "--runbook", s.manual(r.id, "runbook"), "--model", "example-model",
				"--effort", "medium", "--concurrency", "2", "--json")
			if err != nil {
				return err
			}
			v, err := contract.ParseRoleResponse(b, nil)
			if err != nil {
				return err
			}
			if err := check(v, r); err != nil {
				return err
			}
		}
		if err := s.dep.restart(ctx, s.nodes["a"]); err != nil {
			return fmt.Errorf("registry-revision restart of node a: %w", err)
		}
		for _, r := range sampleRoles {
			if err := s.awaitAccept(ctx, r.id); err != nil {
				return err
			}
			v, err := s.roleView(ctx, r.id)
			if err != nil {
				return err
			}
			if err := check(v, r); err != nil {
				return err
			}
		}
		s.prep.bindings = map[string]string{"a": s.nodes["a"].NodeID, "b": s.nodes["b"].NodeID}
		return nil
	})
}

// assignment checks a task's role and node against the frozen bindings.
func (s *Suite) assignment(v contract.TaskView, role, alias string) error {
	if v.Role.ID != role || v.Role.Node != s.prep.bindings[alias] {
		return fmt.Errorf("task %s ran as %s on %s, want %s on node %s", v.TaskID, v.Role.ID, v.Role.Node, role, alias)
	}
	return nil
}

// ensureAnswers dispatches the two no-workspace goals (proof-a on node a,
// proof-b on node b), requires success, exit 0, the exact fixed answer,
// the marker in the logs and no workspace binding or result, and checks
// the consolidated two-entry ledger.
func (s *Suite) ensureAnswers(ctx context.Context) error {
	return s.ensure("goal_answer", func() error {
		if err := s.ensurePrepared(ctx); err != nil {
			return err
		}
		ledger := map[string]string{}
		for _, a := range []struct{ label, role, alias string }{{"answer-a", "proof-a", "a"}, {"answer-b", "proof-b", "b"}} {
			v, err := s.dispatch(ctx, a.role, "M3 goal-and-answer check "+a.label+": reply with your fixed answer")
			if err != nil {
				return err
			}
			if v.WorkspaceBinding != nil || v.Request.Workspace != nil {
				return fmt.Errorf("%s was admitted with a workspace", a.label)
			}
			done, err := s.await(ctx, v.TaskID)
			if err != nil {
				return err
			}
			r := done.Result
			switch {
			case done.State != contract.TaskSucceeded || r.ExitCode == nil || *r.ExitCode != 0:
				return fmt.Errorf("%s ended %s (exit %v)", a.label, done.State, r.ExitCode)
			case r.FinalMessage == nil || *r.FinalMessage != FinalMessage:
				return fmt.Errorf("%s answered %v, want %q", a.label, r.FinalMessage, FinalMessage)
			case done.WorkspaceBinding != nil || r.Workspace != nil || r.ResultCommit != nil:
				return fmt.Errorf("%s has a workspace result", a.label)
			}
			if err := s.assignment(done, a.role, a.alias); err != nil {
				return err
			}
			b, err := s.cliOK(ctx, "task", "logs", "--json", v.TaskID)
			if err != nil {
				return err
			}
			logs, err := contract.ParseTaskLogsResponse(b)
			if err != nil {
				return err
			}
			if !strings.Contains(string(logs.Data), strings.TrimSuffix(fakeadapter.FinalMarker, "\n")) {
				return fmt.Errorf("%s logs lack the final marker", a.label)
			}
			ledger[v.TaskID] = a.role + "@" + done.Role.Node
			s.answers = append(s.answers, observed{a.label, done})
		}
		ids := make([]string, 0, len(ledger))
		for id := range ledger {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		want := map[string]bool{"proof-a@" + s.prep.bindings["a"]: true, "proof-b@" + s.prep.bindings["b"]: true}
		if len(ids) != 2 || ledger[ids[0]] == ledger[ids[1]] || !want[ledger[ids[0]]] || !want[ledger[ids[1]]] {
			return fmt.Errorf("goal-and-answer ledger %v", ledger)
		}
		return nil
	})
}

// caseCoordinator is FP-2's parent record: both phases and the two
// no-workspace tasks.
func (s *Suite) caseCoordinator(ctx context.Context, rec *CaseRecord) error {
	errP := s.ensurePrepared(ctx)
	errA := s.ensureAnswers(ctx)
	rec.Subcases = map[string]string{"prepare": outcome(errP), "goal_answer": outcome(errA)}
	for _, o := range s.answers {
		rec.Tasks = append(rec.Tasks, o.task())
	}
	return errors.Join(errP, errA)
}

func outcome(err error) string {
	if err != nil {
		return OutcomeFail
	}
	return OutcomePass
}

// ---- workspaces ----

// sum is a byte string's SHA-256.
func sum(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

// reportOf is the fake worker's expected pre-edit report of files: every
// regular file 0644 and every directory 0755.
func reportOf(files map[string]string) map[string]string {
	out := map[string]string{}
	for p, c := range files {
		out[p] = "f 0644 " + sum(c)
		for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
			out[filepath.ToSlash(d)] = "d 0755 " + sum("")
		}
	}
	return out
}

// with returns files plus extra.
func with(files map[string]string, extra ...string) map[string]string {
	out := map[string]string{}
	for k, v := range files {
		out[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i]] = extra[i+1]
	}
	return out
}

// pushWorkspace creates workspace name and pushes a plain folder of files
// through the CLI, returning the fixture.
func (s *Suite) pushWorkspace(ctx context.Context, name string, files map[string]string) (workspaceFixture, error) {
	b, err := s.cliOK(ctx, "ws", "create", "--json", name)
	if err != nil {
		return workspaceFixture{}, err
	}
	view, err := contract.ParseWorkspaceView(b)
	if err != nil {
		return workspaceFixture{}, err
	}
	dir := filepath.Join(s.root, "projects", name)
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return workspaceFixture{}, err
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			return workspaceFixture{}, err
		}
	}
	b, err = s.cliOK(ctx, "ws", "push", "--json", "--instance", view.Instance, name, dir)
	if err != nil {
		return workspaceFixture{}, err
	}
	push, err := contract.ParseWorkspacePushResult(b)
	if err != nil {
		return workspaceFixture{}, err
	}
	if push.Instance != view.Instance || push.Branch != contract.DefaultBranchRef || !push.Changed {
		return workspaceFixture{}, fmt.Errorf("push %+v", push)
	}
	return workspaceFixture{name: name, instance: view.Instance, base: push.Commit, files: files}, nil
}

// ensureM4 pushes the M4 project once.
func (s *Suite) ensureM4(ctx context.Context) error {
	return s.ensure("m4-workspace", func() error {
		if err := s.ensurePrepared(ctx); err != nil {
			return err
		}
		var err error
		s.m4, err = s.pushWorkspace(ctx, "m4-proof", map[string]string{"README.md": "# M4 proof project\n", "src/app.txt": "app v1\n"})
		return err
	})
}

// wsScript is a fake workspace script: a pre-edit report, then ops.
func (s *Suite) wsScript(report string, ops ...fakeadapter.WorkspaceOp) string {
	sc := fakeadapter.WorkspaceScript{Ops: ops}
	if report != "" {
		sc.Report = filepath.Join(s.barriers, report)
	}
	return fakeadapter.WorkspaceGoal(sc)
}

// wsDispatch dispatches goal to role on ws at base with the instance guard.
func (s *Suite) wsDispatch(ctx context.Context, role string, ws workspaceFixture, base, goal string, extra ...string) (contract.TaskView, error) {
	v, err := s.dispatch(ctx, role, goal, append([]string{"--workspace", ws.name, "--base", base, "--workspace-instance", ws.instance}, extra...)...)
	if err != nil {
		return v, err
	}
	if b := v.WorkspaceBinding; b == nil || b.Name != ws.name || b.Instance != ws.instance || b.BaseCommit == nil || *b.BaseCommit != base {
		return v, fmt.Errorf("task %s binding %+v, want %s@%s on %s", v.TaskID, v.WorkspaceBinding, ws.name, ws.instance, base)
	}
	return v, nil
}

// published requires a succeeded (or the given) state and a published
// result whose ref is the task's, whose result_commit mirrors it, and
// whose binding is still the admission's.
func published(v contract.TaskView, state string, ws workspaceFixture, base string) (string, error) {
	r := v.Result
	switch {
	case v.State != state:
		return "", fmt.Errorf("task %s ended %s, want %s", v.TaskID, v.State, state)
	case r == nil || r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || r.Workspace.Commit == nil || r.Workspace.Ref == nil:
		return "", fmt.Errorf("task %s has no published result: %+v", v.TaskID, r)
	case *r.Workspace.Ref != contract.TaskRefPrefix+v.TaskID || r.ResultCommit == nil || *r.ResultCommit != *r.Workspace.Commit:
		return "", fmt.Errorf("task %s result ref/commit %v %v %v", v.TaskID, *r.Workspace.Ref, r.ResultCommit, *r.Workspace.Commit)
	case v.WorkspaceBinding == nil || v.WorkspaceBinding.Instance != ws.instance || v.WorkspaceBinding.BaseCommit == nil || *v.WorkspaceBinding.BaseCommit != base:
		return "", fmt.Errorf("task %s binding %+v", v.TaskID, v.WorkspaceBinding)
	}
	return *r.Workspace.Commit, nil
}

// answered requires the exact fixed answer and exit 0.
func answered(v contract.TaskView) error {
	r := v.Result
	if r == nil || r.ExitCode == nil || *r.ExitCode != 0 || r.FinalMessage == nil || *r.FinalMessage != FinalMessage {
		return fmt.Errorf("task %s did not answer %q with exit 0: %+v", v.TaskID, FinalMessage, r)
	}
	return nil
}

// readReport reads a worker's pre-edit report and requires its files to
// equal want exactly.
func (s *Suite) checkReport(name string, want map[string]string) error {
	b, err := os.ReadFile(filepath.Join(s.barriers, name))
	if err != nil {
		return fmt.Errorf("report %s: %w", name, err)
	}
	var rep fakeadapter.WorkspaceReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return fmt.Errorf("report %s: %w", name, err)
	}
	if !rep.HasGit || !reflect.DeepEqual(rep.Files, reportOf(want)) {
		return fmt.Errorf("report %s files %v, want %v", name, rep.Files, reportOf(want))
	}
	return nil
}

// fixtureRepo makes a coordinator repository with one committed file
// (go-git; no shell git).
func fixtureRepo(dir string) (*git.Repository, error) {
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("coordinator\n"), 0o644); err != nil {
		return nil, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	if _, err := wt.Add("local.txt"); err != nil {
		return nil, err
	}
	sig := &object.Signature{Name: "Coordinator", Email: "coordinator@example.invalid", When: time.Unix(1700000000, 0).UTC()}
	if _, err := wt.Commit("coordinator fixture", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		return nil, err
	}
	return repo, nil
}

// pullCommit pulls task id into a fresh coordinator repository through the
// CLI and returns the installed commit and its tree, read with go-git.
func (s *Suite) pullCommit(ctx context.Context, ws workspaceFixture, id, commit string) (*object.Commit, map[string]testkit.FileSpec, error) {
	dir, err := os.MkdirTemp(s.root, "pull-repo-")
	if err != nil {
		return nil, nil, err
	}
	repo, err := fixtureRepo(dir)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.cliOK(ctx, "ws", "pull", "--json", id, dir)
	if err != nil {
		return nil, nil, err
	}
	res, err := contract.ParseWorkspacePullResult(b)
	if err != nil {
		return nil, nil, err
	}
	local := "refs/callsheet/" + ws.name + "/tasks/" + id
	if res.Commit != commit || res.Instance != ws.instance || res.DestinationKind != contract.KindGit || res.LocalRef == nil || *res.LocalRef != local ||
		res.Selector != contract.TaskRefPrefix+id {
		return nil, nil, fmt.Errorf("pull of %s: %+v", id, res)
	}
	ref, err := repo.Reference(plumbing.ReferenceName(local), false)
	if err != nil || ref.Hash().String() != commit {
		return nil, nil, fmt.Errorf("pulled ref %s = %v %v, want %s", local, ref, err, commit)
	}
	c, err := repo.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return nil, nil, err
	}
	files, err := testkit.TreeFiles(repo.Storer, c.Hash)
	return c, files, err
}

// sameTree requires a tree of regular files equal to want.
func sameTree(got map[string]testkit.FileSpec, want map[string]string) error {
	w := map[string]testkit.FileSpec{}
	for p, c := range want {
		w[p] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte(c)}
	}
	return testkit.EqualFiles(got, w)
}

// sidecarCommit requires the fixed Callsheet result identity and message
// and exactly one parent, base: the sidecar, not the fixture, made it.
func sidecarCommit(c *object.Commit, v contract.TaskView, base string) error {
	prefix := "Callsheet task result\ntask: " + v.TaskID + "\nrole: " + v.Role.ID + "\n"
	if c.Author.Name != "Callsheet" || c.Author.Email != "workspace@callsheet.invalid" || c.Author.When.Unix() != 0 ||
		c.Committer.Email != "workspace@callsheet.invalid" || !strings.HasPrefix(c.Message, prefix) {
		return fmt.Errorf("result commit %s is not the sidecar's (%s <%s>, %q)", c.Hash, c.Author.Name, c.Author.Email, c.Message)
	}
	if len(c.ParentHashes) != 1 || c.ParentHashes[0].String() != base {
		return fmt.Errorf("result commit %s parents %v, want %s", c.Hash, c.ParentHashes, base)
	}
	return nil
}

// refsOf is ws status NAME's refs.
func (s *Suite) refsOf(ctx context.Context, ws workspaceFixture) (map[string]string, map[string]int, error) {
	b, err := s.cliOK(ctx, "ws", "status", "--json", "--limit", fmt.Sprint(contract.MaxWorkspaceLimit), ws.name)
	if err != nil {
		return nil, nil, err
	}
	st, err := contract.ParseWorkspaceStatus(b, "", contract.MaxWorkspaceLimit)
	if err != nil {
		return nil, nil, err
	}
	if st.Instance != ws.instance || st.NextAfter != nil {
		return nil, nil, fmt.Errorf("workspace %s status instance %s, more pages %v", ws.name, st.Instance, st.NextAfter != nil)
	}
	refs, count := map[string]string{}, map[string]int{}
	for _, r := range st.Refs {
		refs[r.Name] = r.Commit
		count[r.Name]++
	}
	return refs, count, nil
}

// mainUnmoved is the shared workspace postcondition: main is still the
// originally pushed base.
func (s *Suite) mainUnmoved(ctx context.Context, ws workspaceFixture) error {
	refs, _, err := s.refsOf(ctx, ws)
	if err != nil {
		return err
	}
	if refs[contract.DefaultBranchRef] != ws.base {
		return fmt.Errorf("workspace %s main moved to %s, want %s", ws.name, refs[contract.DefaultBranchRef], ws.base)
	}
	return nil
}

// withMain runs a workspace case and always checks the main postcondition
// in that case, including after a failure with observable state.
func (s *Suite) withMain(ctx context.Context, ws func() workspaceFixture, run func() error) error {
	err := run()
	if f := ws(); f.name != "" {
		err = errors.Join(err, s.mainUnmoved(ctx, f))
	}
	return err
}

// inspect checks a published task through task show (the view), ws
// status TASK_ID and ws diff TASK_ID: one available exact result and the
// diff from its admitted base.
func (s *Suite) inspect(ctx context.Context, v contract.TaskView, base, commit string, added ...string) error {
	b, err := s.cliOK(ctx, "ws", "status", "--json", v.TaskID)
	if err != nil {
		return err
	}
	st, err := contract.ParseTaskWorkspaceStatusResponse(b)
	if err != nil {
		return err
	}
	w := st.Workspace
	if !w.Available || w.Result == nil || w.Result.Commit == nil || *w.Result.Commit != commit || w.Binding.BaseCommit == nil || *w.Binding.BaseCommit != base {
		return fmt.Errorf("ws status %s: %+v", v.TaskID, w)
	}
	b, err = s.cliOK(ctx, "ws", "diff", "--json", v.TaskID)
	if err != nil {
		return err
	}
	d, err := contract.ParseWorkspaceDiff(b, nil, contract.MaxWorkspaceLimit)
	if err != nil {
		return err
	}
	if d.BaseCommit == nil || *d.BaseCommit != base || d.TargetCommit != commit || len(d.Changes) != len(added) {
		return fmt.Errorf("ws diff %s: %+v", v.TaskID, d)
	}
	for i, ch := range d.Changes {
		if ch.Kind != "added" || ch.PathBase64 != b64(added[i]) {
			return fmt.Errorf("ws diff %s change %d: %+v, want added %s", v.TaskID, i, ch, added[i])
		}
	}
	return nil
}

// ---- FP-3: initial publication ----

// ensurePublication runs task A once: proof-a on the pushed base reports
// its pre-edit tree (exactly the base) and writes a.txt; its published
// result is inspected through task show, ws status, ws diff and a real
// pull read with go-git.
func (s *Suite) ensurePublication(ctx context.Context) error {
	return s.ensure("publication", func() error {
		if err := s.ensureM4(ctx); err != nil {
			return err
		}
		ws := s.m4
		v, err := s.wsDispatch(ctx, "proof-a", ws, ws.base, s.wsScript("a-report.json", fakeadapter.WorkspaceOp{Write: "a.txt", Data: "A\n"}))
		if err != nil {
			return err
		}
		done, err := s.await(ctx, v.TaskID)
		if err != nil {
			return err
		}
		s.pubA = observed{"A", done}
		return s.checkPublished(ctx, ws, done, ws.base, "a-report.json", ws.files, with(ws.files, "a.txt", "A\n"), "proof-a", "a", "a.txt")
	})
}

// checkPublished is the shared published-task check: the fixed answer,
// role and node, published ref and commit, the pre-edit report, status,
// diff, and a pulled sidecar commit with exactly want.
func (s *Suite) checkPublished(ctx context.Context, ws workspaceFixture, v contract.TaskView, base, report string, before, want map[string]string, role, alias string, added ...string) error {
	if err := answered(v); err != nil {
		return err
	}
	if err := s.assignment(v, role, alias); err != nil {
		return err
	}
	commit, err := published(v, contract.TaskSucceeded, ws, base)
	if err != nil {
		return err
	}
	if report != "" {
		if err := s.checkReport(report, before); err != nil {
			return err
		}
	}
	if err := s.inspect(ctx, v, base, commit, added...); err != nil {
		return err
	}
	c, files, err := s.pullCommit(ctx, ws, v.TaskID, commit)
	if err != nil {
		return err
	}
	if err := sidecarCommit(c, v, base); err != nil {
		return err
	}
	return sameTree(files, want)
}

func (s *Suite) casePublication(ctx context.Context, rec *CaseRecord) error {
	return s.withMain(ctx, func() workspaceFixture { return s.m4 }, func() error {
		err := s.ensurePublication(ctx)
		if s.pubA.view.TaskID != "" {
			rec.Tasks = []Task{s.pubA.task()}
		}
		return err
	})
}

// ---- FP-4: chaining and isolation ----

// ensureSibling starts B (proof-b) on A's exact result and instance, holds
// it at its barrier after its edit, and while it is held runs the sibling
// (proof-a) on the original base: the sibling's pre-edit tree is exactly
// the base, its only parent the base, and B is still running when the
// sibling has published. A failure releases B.
func (s *Suite) ensureSibling(ctx context.Context) error {
	return s.ensure("sibling", func() (err error) {
		if err := s.ensurePublication(ctx); err != nil {
			return err
		}
		ws, a := s.m4, *s.pubA.view.Result.Workspace.Commit
		in, release := s.hold("b-in"), s.hold("b-go")
		defer func() {
			if err != nil {
				os.WriteFile(release, nil, 0o600)
			}
		}()
		vb, err := s.wsDispatch(ctx, "proof-b", ws, a, s.wsScript("b-report.json", fakeadapter.WorkspaceOp{Write: "b.txt", Data: "B\n"},
			fakeadapter.WorkspaceOp{Touch: in}, fakeadapter.WorkspaceOp{WaitFor: release}))
		if err != nil {
			return err
		}
		s.contB = observed{"B", vb}
		if err := s.awaitFile(ctx, in, vb.TaskID); err != nil {
			return err
		}
		vs, err := s.wsDispatch(ctx, "proof-a", ws, ws.base, s.wsScript("sibling-report.json", fakeadapter.WorkspaceOp{Write: "sibling.txt", Data: "S\n"}))
		if err != nil {
			return err
		}
		done, err := s.await(ctx, vs.TaskID)
		if err != nil {
			return err
		}
		s.sibling = observed{"sibling", done}
		if err := s.checkPublished(ctx, ws, done, ws.base, "sibling-report.json", ws.files, with(ws.files, "sibling.txt", "S\n"), "proof-a", "a", "sibling.txt"); err != nil {
			return err
		}
		held, err := s.show(ctx, vb.TaskID)
		if err != nil {
			return err
		}
		if contract.TaskTerminal(held.State) {
			return fmt.Errorf("continuation %s ended %s before the sibling was inspected", vb.TaskID, held.State)
		}
		return nil
	})
}

// ensureContinuation releases B and inspects it: its pre-edit tree is the
// base with A's change, its only parent is A, and its result adds b.txt;
// every task ref is distinct.
func (s *Suite) ensureContinuation(ctx context.Context) error {
	return s.ensure("continuation", func() error {
		serr := s.ensureSibling(ctx)
		release := filepath.Join(s.barriers, "b-go")
		if s.contB.view.TaskID == "" {
			return serr
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			return err
		}
		if serr != nil {
			return serr
		}
		ws, a := s.m4, *s.pubA.view.Result.Workspace.Commit
		done, err := s.await(ctx, s.contB.view.TaskID)
		if err != nil {
			return err
		}
		s.contB = observed{"B", done}
		afterA := with(ws.files, "a.txt", "A\n")
		if err := s.checkPublished(ctx, ws, done, a, "b-report.json", afterA, with(afterA, "b.txt", "B\n"), "proof-b", "b", "b.txt"); err != nil {
			return err
		}
		refs, count, err := s.refsOf(ctx, ws)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, o := range []observed{s.pubA, s.contB, s.sibling} {
			ref := contract.TaskRefPrefix + o.view.TaskID
			if count[ref] != 1 || refs[ref] != *o.view.Result.Workspace.Commit || seen[refs[ref]] {
				return fmt.Errorf("task ref %s: %d refs at %s", ref, count[ref], refs[ref])
			}
			seen[refs[ref]] = true
		}
		return nil
	})
}

func (s *Suite) caseContinuation(ctx context.Context, rec *CaseRecord) error {
	return s.withMain(ctx, func() workspaceFixture { return s.m4 }, func() error {
		errS := s.ensureSibling(ctx)
		errC := s.ensureContinuation(ctx)
		rec.Subcases = map[string]string{"sibling": outcome(errS), "continuation": outcome(errC)}
		for _, o := range []observed{s.pubA, s.contB, s.sibling} {
			if o.view.TaskID != "" {
				rec.Tasks = append(rec.Tasks, o.task())
			}
		}
		return errors.Join(errS, errC)
	})
}

// ---- FP-5: partial results ----

// partialCase returns one partial-result subcase: failed (exit 3), a real
// CLI cancel after the written barrier, or a 5 s CLI timeout after it.
// Each starts at the original base, writes partial.txt, and must publish
// its partial bytes with the matching terminal state.
func (s *Suite) partialCase(kind string) func(context.Context, *CaseRecord) error {
	return func(ctx context.Context, rec *CaseRecord) error {
		return s.withMain(ctx, func() workspaceFixture { return s.m4 }, func() error {
			err := s.ensure("partial-"+kind, func() error { return s.runPartial(ctx, kind) })
			if o, ok := s.partial[kind]; ok {
				rec.Tasks = []Task{o.task()}
			}
			return err
		})
	}
}

func (s *Suite) runPartial(ctx context.Context, kind string) error {
	if err := s.ensureM4(ctx); err != nil {
		return err
	}
	ws := s.m4
	data := "partial " + kind + "\n"
	var goal string
	var extra []string
	var written string
	state := map[string]string{"failed": contract.TaskFailed, "cancelled": contract.TaskCancelled, "timed_out": contract.TaskTimedOut}[kind]
	switch kind {
	case "failed":
		goal = fakeadapter.WorkspaceGoal(fakeadapter.WorkspaceScript{Ops: []fakeadapter.WorkspaceOp{{Write: "partial.txt", Data: data}}, Exit: 3})
	default:
		written = s.hold(kind + "-written")
		goal = s.wsScript("", fakeadapter.WorkspaceOp{Write: "partial.txt", Data: data}, fakeadapter.WorkspaceOp{Touch: written},
			fakeadapter.WorkspaceOp{WaitFor: s.hold(kind + "-never")})
		if kind == "timed_out" {
			extra = []string{"--timeout", "5s"}
		}
	}
	v, err := s.wsDispatch(ctx, "proof-a", ws, ws.base, goal, extra...)
	if err != nil {
		return err
	}
	s.partial[kind] = observed{kind, v}
	if written != "" {
		if err := s.awaitFile(ctx, written, v.TaskID); err != nil {
			return err
		}
	}
	if kind == "cancelled" {
		b, err := s.cliOK(ctx, "task", "cancel", "--json", v.TaskID)
		if err != nil {
			return err
		}
		if r, err := contract.ParseCancelResponse(b, 202, v.TaskID); err != nil || !r.Accepted {
			return fmt.Errorf("cancel of %s not accepted: %v", v.TaskID, err)
		}
	}
	done, err := s.await(ctx, v.TaskID)
	if err != nil {
		return err
	}
	s.partial[kind] = observed{kind, done}
	if err := s.assignment(done, "proof-a", "a"); err != nil {
		return err
	}
	commit, err := published(done, state, ws, ws.base)
	if err != nil {
		return err
	}
	if kind == "failed" && (done.Result.ExitCode == nil || *done.Result.ExitCode != 3) {
		return fmt.Errorf("failed task %s exit %v, want 3", v.TaskID, done.Result.ExitCode)
	}
	c, files, err := s.pullCommit(ctx, ws, v.TaskID, commit)
	if err != nil {
		return err
	}
	if len(c.ParentHashes) != 1 || c.ParentHashes[0].String() != ws.base {
		return fmt.Errorf("partial result parents %v, want %s", c.ParentHashes, ws.base)
	}
	return sameTree(files, with(ws.files, "partial.txt", data))
}

// casePartialResults is FP-5's parent: the three subcases' outcomes.
func (s *Suite) casePartialResults(ctx context.Context, rec *CaseRecord) error {
	var errs []error
	for _, kind := range []string{"failed", "cancelled", "timed_out"} {
		err := s.ensure("partial-"+kind, func() error { return s.runPartial(ctx, kind) })
		rec.Subcases[kind] = outcome(err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ---- FP-6: lost worker ----

// caseLost holds a file-writing task after its written barrier, restarts
// node a on the same state, and requires lost, not_applicable, no result
// commit, no task ref, a refused task-ID pull that creates nothing, a
// removed work directory, and a fresh task on the restarted node.
func (s *Suite) caseLost(ctx context.Context, rec *CaseRecord) error {
	var lost, recovered observed
	err := s.withMain(ctx, func() workspaceFixture { return s.m4 }, func() error {
		if err := s.ensureM4(ctx); err != nil {
			return err
		}
		ws := s.m4
		written := s.hold("lost-written")
		v, err := s.wsDispatch(ctx, "proof-a", ws, ws.base, s.wsScript("", fakeadapter.WorkspaceOp{Write: "lost.txt", Data: "never published\n"},
			fakeadapter.WorkspaceOp{Touch: written}, fakeadapter.WorkspaceOp{WaitFor: s.hold("lost-never")}))
		if err != nil {
			return err
		}
		lost = observed{"lost", v}
		if err := s.awaitFile(ctx, written, v.TaskID); err != nil {
			return err
		}
		node := s.nodes["a"]
		if err := s.dep.restart(ctx, node); err != nil {
			return err
		}
		done, err := s.await(ctx, v.TaskID)
		if err != nil {
			return err
		}
		lost = observed{"lost", done}
		r := done.Result
		if done.State != contract.TaskLost || r.Workspace == nil || r.Workspace.Publication != contract.PublicationNotApplicable || r.ResultCommit != nil ||
			r.Workspace.Commit != nil || r.Workspace.Ref != nil {
			return fmt.Errorf("lost task %s: %s %+v", v.TaskID, done.State, r.Workspace)
		}
		if err := s.assignment(done, "proof-a", "a"); err != nil {
			return err
		}
		if _, count, err := s.refsOf(ctx, ws); err != nil || count[contract.TaskRefPrefix+v.TaskID] != 0 {
			return fmt.Errorf("lost task %s has a ref (%v)", v.TaskID, err)
		}
		dest := filepath.Join(s.root, "lost-pull")
		pr, err := s.cli(ctx, "ws", "pull", v.TaskID, dest)
		if err != nil {
			return err
		}
		if pr.code != 4 || !strings.Contains(pr.stderr, contract.ReasonTaskResultUnavailable) {
			return fmt.Errorf("pull of the lost task: exit %d %q", pr.code, pr.stderr)
		}
		if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the refused pull created %s", dest)
		}
		if err := poll(ctx, "lost work removed", readyBound, func() (bool, error) {
			_, err := os.Stat(filepath.Join(node.State, "tasks", v.TaskID))
			return errors.Is(err, fs.ErrNotExist), nil
		}); err != nil {
			return err
		}
		vr, err := s.wsDispatch(ctx, "proof-a", ws, ws.base, s.wsScript("", fakeadapter.WorkspaceOp{Write: "recovered.txt", Data: "R\n"}))
		if err != nil {
			return err
		}
		done, err = s.await(ctx, vr.TaskID)
		if err != nil {
			return err
		}
		recovered = observed{"recovered", done}
		return s.checkPublished(ctx, ws, done, ws.base, "", nil, with(ws.files, "recovered.txt", "R\n"), "proof-a", "a", "recovered.txt")
	})
	for _, o := range []observed{lost, recovered} {
		if o.view.TaskID != "" {
			rec.Tasks = append(rec.Tasks, o.task())
		}
	}
	return err
}

// ---- FP-7: publication restart ----

// caseRestart dispatches a short task, polls task show until its published
// receipt is visible, saves its complete workspace result and binding,
// restarts node a at once (after acknowledged publication, before
// coordinator delivery), then requires repeated task show and ws status to
// return the same receipt, a pull of the same tree, exactly one task ref,
// and a fresh successful task.
func (s *Suite) casePublicationRestart(ctx context.Context, rec *CaseRecord) error {
	var first, fresh observed
	err := s.withMain(ctx, func() workspaceFixture { return s.m4 }, func() error {
		if err := s.ensureM4(ctx); err != nil {
			return err
		}
		ws := s.m4
		v, err := s.wsDispatch(ctx, "proof-a", ws, ws.base, s.wsScript("", fakeadapter.WorkspaceOp{Write: "restart.txt", Data: "restart\n"}))
		if err != nil {
			return err
		}
		first = observed{"restart", v}
		var seen contract.TaskView
		if err := poll(ctx, "published receipt of "+v.TaskID, taskBound, func() (bool, error) {
			var err error
			seen, err = s.show(ctx, v.TaskID)
			return err == nil && resolved(seen) && seen.Result.Workspace.Publication == contract.PublicationPublished, err
		}); err != nil {
			return err
		}
		first = observed{"restart", seen}
		saved, _ := json.Marshal([]any{seen.Result.Workspace, seen.WorkspaceBinding, seen.Result.ResultCommit, seen.State})
		if err := s.dep.restart(ctx, s.nodes["a"]); err != nil {
			return err
		}
		commit, err := published(seen, contract.TaskSucceeded, ws, ws.base)
		if err != nil {
			return err
		}
		for i := 0; i < 3; i++ {
			again, err := s.show(ctx, v.TaskID)
			if err != nil {
				return err
			}
			got, _ := json.Marshal([]any{again.Result.Workspace, again.WorkspaceBinding, again.Result.ResultCommit, again.State})
			if string(got) != string(saved) {
				return fmt.Errorf("receipt changed after the restart:\n%s\n%s", got, saved)
			}
			b, err := s.cliOK(ctx, "ws", "status", "--json", v.TaskID)
			if err != nil {
				return err
			}
			st, err := contract.ParseTaskWorkspaceStatusResponse(b)
			if err != nil {
				return err
			}
			if w := st.Workspace; !w.Available || w.Result == nil || w.Result.Commit == nil || *w.Result.Commit != commit || w.Result.Ref == nil ||
				*w.Result.Ref != contract.TaskRefPrefix+v.TaskID || w.Binding.Instance != ws.instance || *w.Binding.BaseCommit != ws.base {
				return fmt.Errorf("ws status after the restart: %+v", w)
			}
		}
		c, files, err := s.pullCommit(ctx, ws, v.TaskID, commit)
		if err != nil {
			return err
		}
		if err := sidecarCommit(c, seen, ws.base); err != nil {
			return err
		}
		if err := sameTree(files, with(ws.files, "restart.txt", "restart\n")); err != nil {
			return err
		}
		if _, count, err := s.refsOf(ctx, ws); err != nil || count[contract.TaskRefPrefix+v.TaskID] != 1 {
			return fmt.Errorf("task %s has %d refs (%v)", v.TaskID, count[contract.TaskRefPrefix+v.TaskID], err)
		}
		vf, err := s.wsDispatch(ctx, "proof-a", ws, ws.base, s.wsScript("", fakeadapter.WorkspaceOp{Write: "fresh.txt", Data: "F\n"}))
		if err != nil {
			return err
		}
		done, err := s.await(ctx, vf.TaskID)
		if err != nil {
			return err
		}
		fresh = observed{"fresh", done}
		return s.checkPublished(ctx, ws, done, ws.base, "", nil, with(ws.files, "fresh.txt", "F\n"), "proof-a", "a", "fresh.txt")
	})
	for _, o := range []observed{first, fresh} {
		if o.view.TaskID != "" {
			rec.Tasks = append(rec.Tasks, o.task())
		}
	}
	return err
}

// ---- FP-8: pull preservation ----

// snapshot records every worktree file (bytes and mode), every file of
// .git except imported objects and refs/callsheet, and the branch refs.
type snapshot struct {
	files    map[string]string
	branches map[string]string
}

func takeSnapshot(dir string, repo *git.Repository) (snapshot, error) {
	snap := snapshot{files: map[string]string{}, branches: map[string]string{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == ".git/objects" || rel == ".git/refs/callsheet" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		snap.files[rel] = fmt.Sprintf("%v %s", info.Mode(), sum(string(b)))
		return nil
	})
	if err != nil {
		return snap, err
	}
	iter, err := repo.Branches()
	if err != nil {
		return snap, err
	}
	err = iter.ForEach(func(r *plumbing.Reference) error {
		snap.branches[r.Name().String()] = r.Hash().String()
		return nil
	})
	return snap, err
}

// caseDirtyPull pulls B by task ID into a dirty coordinator checkout
// (committed, modified tracked, staged and untracked content) and requires
// the worktree, index, HEAD and branch refs unchanged, only objects and
// refs/callsheet added, the imported ref at B with the expected tree, and
// an independent fresh-folder pull of the same files.
func (s *Suite) caseDirtyPull(ctx context.Context, rec *CaseRecord) error {
	err := s.withMain(ctx, func() workspaceFixture { return s.m4 }, func() error {
		if err := s.ensureContinuation(ctx); err != nil {
			return err
		}
		ws, b := s.m4, s.contB.view
		commit := *b.Result.Workspace.Commit
		dir := filepath.Join(s.root, "coordinator-checkout")
		repo, err := fixtureRepo(dir)
		if err != nil {
			return err
		}
		wt, err := repo.Worktree()
		if err != nil {
			return err
		}
		for p, c := range map[string]string{"staged.txt": "staged\n"} {
			if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
				return err
			}
			if _, err := wt.Add(p); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("modified, not staged\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
			return err
		}
		before, err := takeSnapshot(dir, repo)
		if err != nil {
			return err
		}
		if before.files[".git/index"] == "" || before.files[".git/HEAD"] == "" || len(before.branches) != 1 {
			return fmt.Errorf("the dirty checkout fixture is incomplete: %v", before)
		}
		out, err := s.cliOK(ctx, "ws", "pull", "--json", b.TaskID, dir)
		if err != nil {
			return err
		}
		res, err := contract.ParseWorkspacePullResult(out)
		if err != nil {
			return err
		}
		local := "refs/callsheet/" + ws.name + "/tasks/" + b.TaskID
		if res.Commit != commit || res.LocalRef == nil || *res.LocalRef != local || res.DestinationKind != contract.KindGit {
			return fmt.Errorf("dirty pull %+v", res)
		}
		repo, err = git.PlainOpen(dir)
		if err != nil {
			return err
		}
		after, err := takeSnapshot(dir, repo)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("the pull changed the checkout:\nbefore %v\nafter  %v", before, after)
		}
		ref, err := repo.Reference(plumbing.ReferenceName(local), false)
		if err != nil || ref.Hash().String() != commit {
			return fmt.Errorf("imported ref %s = %v %v", local, ref, err)
		}
		files, err := testkit.TreeFiles(repo.Storer, ref.Hash())
		if err != nil {
			return err
		}
		want := with(ws.files, "a.txt", "A\n", "b.txt", "B\n")
		if err := sameTree(files, want); err != nil {
			return err
		}
		folder := filepath.Join(s.root, "fresh-delivery")
		if _, err := s.cliOK(ctx, "ws", "pull", b.TaskID, folder); err != nil {
			return err
		}
		got := map[string]string{}
		err = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			c, err := os.ReadFile(p)
			rel, _ := filepath.Rel(folder, p)
			got[filepath.ToSlash(rel)] = string(c)
			return err
		})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("fresh-folder pull %v, want %v", got, want)
		}
		return nil
	})
	if s.contB.view.TaskID != "" {
		rec.Tasks = []Task{s.contB.task()}
	}
	return err
}

// ---- FP-11: sample development flow ----

// sampleSteps is the ordered stub flow: label, role, node and file.
var sampleSteps = []struct{ label, role, alias, file, data string }{
	{"design", "proof-a", "a", "design.txt", "stub design\n"},
	{"design-review", "proof-b", "b", "design-review.txt", "stub design approved\n"},
	{"code", "proof-code", "a", "implementation.txt", "stub implementation\n"},
	{"code-review", "proof-code-review", "b", "code-review.txt", "stub code approved\n"},
}

// caseSampleFlow runs, after the no-workspace pair, the four stub tasks in
// a separate sample-flow workspace, each on the previous result hash: the
// pre-edit report is the preceding tree, the only parent the base, a CLI
// pull the cumulative tree; then four distinct task IDs on their roles.
func (s *Suite) caseSampleFlow(ctx context.Context, rec *CaseRecord) error {
	var ws workspaceFixture
	return s.withMain(ctx, func() workspaceFixture { return ws }, func() error {
		if err := s.ensureAnswers(ctx); err != nil {
			return err
		}
		var err error
		if ws, err = s.pushWorkspace(ctx, "sample-flow", map[string]string{"seed.txt": "sample seed\n"}); err != nil {
			return err
		}
		base, tree := ws.base, ws.files
		ids := map[string]bool{}
		for i, st := range sampleSteps {
			report := fmt.Sprintf("sample-%d.json", i)
			v, err := s.wsDispatch(ctx, st.role, ws, base, s.wsScript(report, fakeadapter.WorkspaceOp{Write: st.file, Data: st.data}))
			if err != nil {
				return err
			}
			done, err := s.await(ctx, v.TaskID)
			if err != nil {
				return err
			}
			rec.Tasks = append(rec.Tasks, observed{st.label, done}.task())
			next := with(tree, st.file, st.data)
			if err := s.checkPublished(ctx, ws, done, base, report, tree, next, st.role, st.alias, st.file); err != nil {
				return fmt.Errorf("%s: %w", st.label, err)
			}
			if ids[v.TaskID] {
				return fmt.Errorf("%s reused task %s", st.label, v.TaskID)
			}
			ids[v.TaskID] = true
			base, tree = *done.Result.Workspace.Commit, next
		}
		return nil
	})
}

// ---- FP-9: claims ----

// caseClaims checks the bundled milestone documents.
func (s *Suite) caseClaims(_ context.Context, _ *CaseRecord) error {
	docs := map[string][]byte{}
	for _, name := range claimDocs {
		b, err := os.ReadFile(filepath.Join(s.cfg.FixtureDir, "docs", name))
		if err != nil {
			return err
		}
		docs[name] = b
	}
	return CheckClaims(docs)
}
