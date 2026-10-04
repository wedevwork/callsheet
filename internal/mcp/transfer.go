package mcp

import (
	"context"
	"encoding/json"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The local transfer tools (iteration 09b): ws_push and ws_pull run on
// the machine running callsheet mcp. Only paths, selectors and the result
// metadata cross MCP: never file contents, file lists, commit messages,
// absolute local paths, credentials or pack diagnostics. They are
// ordinary calls (no waiting-call budget): cancellation, EOF and signals
// cancel them, and a call's slot is released only after its scanning,
// packing and temporary cleanup have ended.

// Transfers is one call's local transfer service.
type Transfers interface {
	Push(ctx context.Context, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error)
	Pull(ctx context.Context, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error)
	// ResolveTaskResult (iteration 10c) resolves a task's published result
	// through the same call's typed client for ws_pull's task form.
	ResolveTaskResult(ctx context.Context, id string) (client.TaskResultSelection, error)
	Close()
}

// TransferFactory returns a fresh trusted client and transfer service for
// one call; cwd is the server's working directory captured at admission.
type TransferFactory func(ctx context.Context, cwd string) (Transfers, error)

// planeTransfers is the production transfer service of one call.
type planeTransfers struct {
	cl *client.Client
	o  workspacetransfer.Options
}

func (p *planeTransfers) Push(ctx context.Context, req workspacetransfer.PushRequest) (contract.WorkspacePushResult, error) {
	return workspacetransfer.Push(ctx, p.o, req)
}

func (p *planeTransfers) Pull(ctx context.Context, req workspacetransfer.PullRequest) (contract.WorkspacePullResult, error) {
	return workspacetransfer.Pull(ctx, p.o, req)
}

func (p *planeTransfers) ResolveTaskResult(ctx context.Context, id string) (client.TaskResultSelection, error) {
	return p.cl.ResolveTaskResult(ctx, id)
}

func (p *planeTransfers) Close() { p.cl.Close() }

// PlaneTransferFactory is the production transfer factory: per call it
// resolves trust afresh and builds a verified client, with the explicit
// goos and the server's environment.
func PlaneTransferFactory(planeURL, caFile, fingerprint, goos string, environ func() []string) TransferFactory {
	return func(ctx context.Context, cwd string) (Transfers, error) {
		trust, err := client.ResolveTrust(ctx, client.TrustOptions{PlaneURL: planeURL, CAFile: caFile, CAFingerprint: fingerprint})
		if err != nil {
			return nil, err
		}
		c, err := client.New(planeURL, trust)
		if err != nil {
			return nil, err
		}
		return &planeTransfers{cl: c, o: workspacetransfer.Options{GOOS: goos, Env: workspacetransfer.ProcessEnv(environ()), Cwd: cwd,
			Plane: workspacetransfer.ClientPlane(c)}}, nil
	}
}

// transferTools are ws_push and ws_pull.
func transferTools() []*tool {
	const local = " PATH is on the machine running callsheet mcp (default: that server's working directory; a relative path resolves against it; no ~ or variable expansion); nothing about the files is returned."
	const long = " Transfers can take a long time for large histories (streams have a 30-second no-progress bound, no total timeout)."
	return []*tool{
		{name: toolWsPush, annotations: mutation(false, false), run: runWsPush,
			description: "Push local files to a workspace branch (default main): this tool READS local files. A clean git repository (path must be its root) pushes its committed HEAD and complete history with exact commit IDs; it must be clean (no staged, unstaged or untracked non-ignored changes, with .gitignore, info/exclude and the global excludes file; files are compared in Git's normalized check-in form, so a line-ending-only difference is clean), else commit first. A plain folder is snapshotted honoring its nested .gitignore files as one deterministic commit parented on the branch's current commit (an unchanged tree adds none). Fast-forward only with compare-and-swap on the observed tip: a push that would replace history is refused (push to a new branch, then ws_ref_set); a moved branch conflicts. Worktrees, submodules, shallow, partial, sparse and LFS repositories and repository subdirectories are refused. Returns name, instance, branch, old_commit, commit, source_kind and changed. A lost answer may hide a completed push: inspect ws_status before repeating it (never retried)." +
				local + long + mutationNote + freshNote},
		{name: toolWsPull, annotations: mutation(false, false), run: runWsPull,
			description: "Pull a workspace commit to local files: this tool WRITES local files. Two forms (exactly one). {task_id, path?}: that task's published result from its bound workspace instance (its record selects the instance and immutable result commit; a running task conflicts with task_result_pending, an unpublished result with task_result_unavailable, a scratch task is invalid_argument no_task_workspace, a pruned result not_found and a removed or recreated workspace conflicts with workspace_instance_mismatch: never a fallback to a new workspace of the same name). {name, ref, path?}: ref is a branch, a full reachable 40-hex commit hash, a complete refs/callsheet/tasks/ ref or a bare task ID meaning that task ref (refs/heads/t_... is a branch named like a task ID). The commit is verified completely before anything local is written. Into an existing git repository (path must be its root; local changes are fine) it installs objects and sets only refs/callsheet/NAME/heads|tasks|commits/... with a local compare-and-swap; checkout, index, HEAD, branches, config and remotes are never touched, and delivering it to your own remote is your own git push. Into any other path it exports the files to a new or empty directory with one atomic rename (a nonempty directory is refused, never overwritten; unsafe trees are refused). Returns name, instance, selector, commit, destination_kind, local_ref, old_commit and changed. After a lost answer inspect the destination before repeating it." +
				local + long + mutationNote + freshNote},
	}
}

// transfers returns a fresh transfer service for this call, with the
// server's working directory captured now (at admission).
func (c *call) transfers() (Transfers, error) {
	c.s.hook(StageTrust, c.id.key)
	cwd := ""
	if c.s.cfg.Cwd != nil {
		if d, err := c.s.cfg.Cwd(); err == nil {
			cwd = d
		}
	}
	if c.s.cfg.Transfer == nil {
		return nil, contract.New(contract.CodeInternal, "local transfers are not configured in this MCP server")
	}
	return c.s.cfg.Transfer(c.opCtx, cwd)
}

// optionalPath validates an optional path argument (omitted: the working
// directory; present: nonempty, valid UTF-8, no NUL, native length).
func (c *call) optionalPath(p *string) (string, bool, error) {
	if p == nil {
		return "", false, nil
	}
	if err := contract.ValidateLocalPath(c.s.cfg.GOOS, *p); err != nil {
		return "", false, err
	}
	return *p, true, nil
}

func runWsPush(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		Name     string  `json:"name"`
		Instance string  `json:"instance"`
		Branch   *string `json:"branch,omitempty"`
		Path     *string `json:"path,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	branch := ""
	if a.Branch != nil {
		if *a.Branch == "" {
			return toolResult{}, invalid("branch", "branch must not be empty; omit it for main")
		}
		branch = *a.Branch
	}
	if _, err := contract.ValidatePush(a.Name, a.Instance, branch); err != nil {
		return toolResult{}, err
	}
	path, set, err := c.optionalPath(a.Path)
	if err != nil {
		return toolResult{}, err
	}
	tr, err := c.transfers()
	if err != nil {
		return toolResult{}, err
	}
	defer tr.Close()
	r, err := tr.Push(c.opCtx, workspacetransfer.PushRequest{Name: a.Name, Instance: a.Instance, Branch: branch, Path: path, PathSet: set})
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

// pullArgs are ws_pull's flat arguments (iteration 10c): the explicit
// form {name, ref, path?} or the task form {task_id, path?}; exactly one
// form and no mixed key are runtime rules.
type pullArgs struct {
	TaskID *string `json:"task_id,omitempty"`
	Name   *string `json:"name,omitempty"`
	Ref    *string `json:"ref,omitempty"`
	Path   *string `json:"path,omitempty"`
}

// request validates the form and returns the pull request (without the
// task form's resolved selection) and whether it is the task form.
func (a pullArgs) request() (workspacetransfer.PullRequest, bool, error) {
	var req workspacetransfer.PullRequest
	switch {
	case a.TaskID != nil && (a.Name != nil || a.Ref != nil):
		return req, false, invalid("task_id", "ws_pull takes either task_id or name and ref, never both")
	case a.TaskID != nil && !contract.ValidTaskID(*a.TaskID):
		return req, false, invalidTaskID("task_id")
	case a.TaskID != nil:
		return req, true, nil
	case a.Name == nil || a.Ref == nil:
		return req, false, invalid("name", "ws_pull needs task_id, or name and ref together")
	}
	if !contract.ValidWorkspaceName(*a.Name) {
		return req, false, invalidWsName()
	}
	// A bare task ID means its full task ref (refs/heads/t_... escapes to
	// a branch); the unchanged ValidatePull then checks the result.
	sel, err := contract.ParseTaskSelector(*a.Ref, "ref", false)
	if err != nil {
		return req, false, err
	}
	req.Name, req.Ref = *a.Name, contract.SelectorString(sel)
	if _, err := contract.ValidatePull(req.Name, req.Ref); err != nil {
		return req, false, err
	}
	return req, false, nil
}

func runWsPull(c *call, raw json.RawMessage) (toolResult, error) {
	var a pullArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	req, task, err := a.request()
	if err != nil {
		return toolResult{}, err
	}
	if req.Path, req.PathSet, err = c.optionalPath(a.Path); err != nil {
		return toolResult{}, err
	}
	tr, err := c.transfers()
	if err != nil {
		return toolResult{}, err
	}
	defer tr.Close()
	if task {
		// The task's record selects its bound instance and immutable
		// result; the pair guards the transfer itself. The call's context
		// cancels the lookup and the transfer alike.
		sel, err := tr.ResolveTaskResult(c.opCtx, *a.TaskID)
		if err != nil {
			return toolResult{}, err
		}
		req.Name, req.Ref, req.ExpectedInstance, req.ExpectedCommit = sel.Name, sel.Ref, sel.Instance, sel.Commit
	}
	r, err := tr.Pull(c.opCtx, req)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}
