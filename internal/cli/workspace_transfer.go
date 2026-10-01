package cli

import (
	"context"
	"flag"
	"io"
	"os"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The local transfer leaves (iteration 09b): ws push and ws pull. They
// run on this machine: push reads the local PATH (a clean git repository
// or a plain folder) and pull writes it (objects and one Callsheet ref in
// an existing repository, or a new or empty folder); only paths, selectors
// and result metadata cross to the plane's verified TLS endpoint.
const (
	wsPushUsage = "--instance TOKEN [--branch BRANCH] " + trustUsage + " [--json] NAME [PATH]"
	wsPullUsage = trustUsage + " [--json] NAME REF [PATH]"

	wsLocalNote = "PATH is on this machine (default: the current directory; relative paths resolve\n" +
		"against it; no ~ or variable expansion). Flags come before the operands. Transfers\n" +
		"stream over the plane's verified TLS endpoint with a 30 s no-progress bound and no\n" +
		"total timeout; large histories take time. go-git is embedded: no git executable,\n" +
		"hook, filter, credential helper or remote is used. See docs/workspaces.md.\n"

	wsPushDetails = "Flags:\n" +
		"  --instance TOKEN   the workspace's instance (from callsheet ws show)\n" +
		"  --branch BRANCH    the target branch, short or refs/heads/... (default main)\n" + trustHelp + wsJSONHelp + "\n" +
		"A git repository (PATH must be its root) pushes its committed HEAD and complete\n" +
		"history with their exact commit IDs, tracked files now ignored included. It must be\n" +
		"clean: no staged, unstaged or untracked non-ignored changes (with .gitignore,\n" +
		".git/info/exclude and the global excludes file; files are compared in Git's\n" +
		"normalized check-in form, so a line-ending-only difference is clean): otherwise\n" +
		"commit first. A plain folder is snapshotted instead, honoring its nested .gitignore files\n" +
		"only, as one deterministic commit (author Callsheet, time 0) whose parent is the\n" +
		"branch's current commit; an unchanged tree creates no commit. Worktrees, submodules,\n" +
		"shallow, partial, sparse and LFS repositories and subdirectories of a repository\n" +
		"are refused.\n\n" +
		"The push is fast-forward only, with compare-and-swap against the branch tip it\n" +
		"observed: a push that would replace history is refused (push to a new branch, then\n" +
		"move the target explicitly with callsheet ws ref set), and a branch moved by someone\n" +
		"else conflicts. Nothing is retried. Prints name, instance, branch, old_commit,\n" +
		"commit, source_kind and changed. If the answer is lost the push may have succeeded:\n" +
		"inspect with callsheet ws status before repeating it.\n\n" +
		"Example (seed a workspace from a folder):\n" +
		"  callsheet ws push --instance <token from ws show> --plane https://plane.example:8443 \\\n" +
		"    --ca plane-ca.crt myproject ./myproject\n" + wsLocalNote
	wsPullDetails = "Flags:\n" + trustHelp + wsJSONHelp + "\n" +
		"REF is a branch (short or refs/heads/...), a full 40-hex commit hash reachable from a\n" +
		"workspace ref, or a complete refs/callsheet/tasks/t_<32 hex> ref (t_... alone is a\n" +
		"branch name). The commit is resolved once and verified completely before anything\n" +
		"local is written.\n\n" +
		"Into an existing git repository (PATH must be its root; local changes are fine) it\n" +
		"installs the objects and sets exactly one Callsheet ref: refs/callsheet/NAME/heads/\n" +
		"BRANCH, refs/callsheet/NAME/tasks/TASK_ID or refs/callsheet/NAME/commits/HASH, with a\n" +
		"local compare-and-swap (it may move backwards). Checkout, index, HEAD, branches,\n" +
		"config, remotes and FETCH_HEAD are never touched. To deliver it, push it to your own\n" +
		"remote yourself, e.g. git push <your-remote> <commit>:refs/heads/<delivery-branch>;\n" +
		"Callsheet never runs that command or stores remote credentials.\n\n" +
		"Into any other PATH it exports the commit's files to a new directory, or to an\n" +
		"existing empty one, in a private sibling staging directory published by one atomic\n" +
		"rename: a nonempty destination is refused and never overwritten. Trees with names\n" +
		"this filesystem cannot keep apart, .git names or symlinks that leave the export\n" +
		"are refused (pull into a repository instead).\n\n" +
		"Prints name, instance, selector, commit, destination_kind, local_ref, old_commit and\n" +
		"changed. After a lost answer or a failed final sync, inspect the destination or the\n" +
		"Callsheet ref before repeating it.\n" + wsLocalNote
)

// Seams of the transfer leaves (tests observe the requests without a
// plane or local repository).
var (
	transferPush = workspacetransfer.Push
	transferPull = workspacetransfer.Pull
	processCwd   = os.Getwd
	processEnv   = os.Environ
)

// transferOptions builds a transfer's options from the verified client,
// the process working directory and environment.
func transferOptions(goos string, f *remoteFlags, ctx context.Context, errOut io.Writer) (workspacetransfer.Options, func(), int, bool) {
	cwd, err := processCwd()
	if err != nil {
		cwd = ""
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return workspacetransfer.Options{}, nil, code, false
	}
	return workspacetransfer.Options{GOOS: goos, Env: workspacetransfer.ProcessEnv(processEnv()), Cwd: cwd, Plane: workspacetransfer.ClientPlane(cl)},
		cl.Close, 0, true
}

// optPath returns the optional PATH operand at index i.
func optPath(ops []string, i int) (string, bool) {
	if len(ops) > i {
		return ops[i], true
	}
	return "", false
}

func noneOr(p *string) string {
	if p == nil {
		return "none"
	}
	return *p
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// RenderPush renders a push result as key: value lines in field order.
func RenderPush(r contract.WorkspacePushResult) string {
	return "name: " + r.Name + "\ninstance: " + r.Instance + "\nbranch: " + r.Branch + "\nold_commit: " + noneOr(r.OldCommit) +
		"\ncommit: " + r.Commit + "\nsource_kind: " + r.SourceKind + "\nchanged: " + boolText(r.Changed) + "\n"
}

// RenderPull renders a pull result as key: value lines in field order.
func RenderPull(r contract.WorkspacePullResult) string {
	return "name: " + r.Name + "\ninstance: " + r.Instance + "\nselector: " + r.Selector + "\ncommit: " + r.Commit +
		"\ndestination_kind: " + r.DestinationKind + "\nlocal_ref: " + noneOr(r.LocalRef) + "\nold_commit: " + noneOr(r.OldCommit) +
		"\nchanged: " + boolText(r.Changed) + "\n"
}

func wsPush(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	var inst, branch single
	f, ops, code, ok := parseRemote(c, args, true, false, true, 2, errOut, instanceFlag(&inst), func(fs *flag.FlagSet) { fs.Var(&branch, "branch", "") })
	if !ok {
		return code
	}
	if code, ok := wsOperands(c, ops, 1, errOut); !ok {
		return code
	}
	if code, ok := needInstance(c, inst, errOut); !ok {
		return code
	}
	if branch.set && branch.val == "" {
		return usageError(errOut, c, "--branch must not be empty")
	}
	if _, err := contract.ValidatePush(ops[0], inst.val, branch.val); err != nil {
		return planeFail(errOut, err)
	}
	path, pathSet := optPath(ops, 1)
	if pathSet {
		if err := contract.ValidateLocalPath(goos, path); err != nil {
			return planeFail(errOut, err)
		}
	}
	o, closeFn, code, ok := transferOptions(goos, f, ctx, errOut)
	if !ok {
		return code
	}
	defer closeFn()
	r, err := transferPush(ctx, o, workspacetransfer.PushRequest{Name: ops[0], Instance: inst.val, Branch: branch.val, Path: path, PathSet: pathSet})
	if err != nil {
		return planeFail(errOut, err)
	}
	return wsOut(out, errOut, f.json.val, r, RenderPush(r))
}

func wsPull(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, ops, code, ok := parseRemote(c, args, true, false, true, 3, errOut)
	if !ok {
		return code
	}
	if len(ops) < 2 {
		return usageError(errOut, c, c.Path()+" needs NAME and REF")
	}
	if _, err := contract.ValidatePull(ops[0], ops[1]); err != nil {
		return planeFail(errOut, err)
	}
	path, pathSet := optPath(ops, 2)
	if pathSet {
		if err := contract.ValidateLocalPath(goos, path); err != nil {
			return planeFail(errOut, err)
		}
	}
	o, closeFn, code, ok := transferOptions(goos, f, ctx, errOut)
	if !ok {
		return code
	}
	defer closeFn()
	r, err := transferPull(ctx, o, workspacetransfer.PullRequest{Name: ops[0], Ref: ops[1], Path: path, PathSet: pathSet})
	if err != nil {
		return planeFail(errOut, err)
	}
	return wsOut(out, errOut, f.json.val, r, RenderPull(r))
}
