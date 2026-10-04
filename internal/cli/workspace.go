package cli

import (
	"context"
	"encoding/base64"
	"flag"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The workspace leaves (iteration 09a): create, ls, show, rm, prune,
// status, diff and ref set over the verified client. Every leaf names its
// workspace explicitly (no default, no local configuration or cache) and
// operates on the plane: nothing reads or writes a local repository or
// working tree. Iteration 09b adds the two local transfers, push and
// pull (see workspace_transfer.go), the only leaves that touch local
// files.
const (
	wsCreateUsage = trustUsage + " [--json] NAME"
	wsLsUsage     = "[--after NAME] [--limit N] " + trustUsage + " [--json]"
	wsShowUsage   = trustUsage + " [--json] NAME"
	wsRmUsage     = "--instance TOKEN " + trustUsage + " [--json] NAME"
	wsPruneUsage  = "--instance TOKEN --before RFC3339 " + trustUsage + " [--json] NAME"
	wsStatusUsage = "[--after REF --generation TOKEN --instance TOKEN] [--limit N] " + trustUsage + " [--json] (NAME | TASK_ID)"
	wsDiffUsage   = "[--base SELECTOR] [--after CURSOR --instance TOKEN --generation TOKEN] [--limit N] " + trustUsage + " [--json] (NAME TARGET | TASK_ID)"
	wsRefSetUsage = "--instance TOKEN --expected HASH|absent [--delete] " + trustUsage + " [--json] NAME BRANCH [TARGET]"

	wsJSONHelp  = "  --json             print the plane's exact API success object as one JSON value\n"
	wsPlaneNote = "It operates on the plane over verified TLS; nothing local is read or written,\n" +
		"and nothing is cached. Flags come before the operands.\n"
	wsSelectorHelp = "A selector is a full 40-hex commit hash reachable from a current ref, a branch\n" +
		"(short, e.g. main, or refs/heads/...) or a complete refs/callsheet/tasks/ ref. A\n" +
		"40-hex string always means a hash; no short hashes or revision expressions.\n"
	wsBranchHelp = "Branch names are byte-exact lowercase ASCII: slash-separated components of 1-200\n" +
		"bytes of a-z, 0-9, '.', '_' and '-' (starting with a letter or digit), at most 512\n" +
		"bytes as refs/heads/...; Main, non-ASCII and invalid UTF-8 are rejected, never\n" +
		"repaired.\n"

	wsCreateDetails = "Flags:\n" + trustHelp + wsJSONHelp + "\n" +
		"Creates an empty, durable workspace (a bare git repository on the plane, with no\n" +
		"commit and an unborn main) and prints its identity. The instance token is assigned\n" +
		"once and never reused; rm, prune and ref set require it. Names are unique slugs:\n" +
		"repeating a create conflicts (read callsheet ws show). " + wsPlaneNote
	wsLsDetails = "Flags:\n" +
		"  --after NAME       start after this name (the previous page's next_after)\n" +
		"  --limit N          at most N rows (1-100, default 100)\n" + trustHelp + wsJSONHelp + "\n" +
		"Lists one page of workspaces as tab-separated NAME, INSTANCE and CREATED_AT, sorted by\n" +
		"name, then next_after when more remain. Each page is a fresh observation: pages are\n" +
		"not a snapshot across concurrent create and rm. " + wsPlaneNote
	wsShowDetails = "Flags:\n" + trustHelp + wsJSONHelp + "\n" +
		"Shows a workspace's identity, current generation, default branch, retention and\n" +
		"size_bytes: the logical size of every regular file the plane owns for it (retained\n" +
		"old generations included), not allocated blocks or transfer size. There is no quota\n" +
		"and no automatic expiry. " + wsPlaneNote
	wsRmDetails = "Flags:\n" +
		"  --instance TOKEN   the workspace's instance (from callsheet ws show)\n" + trustHelp + wsJSONHelp + "\n" +
		"Removes the whole workspace after its current readers and writer finish; a stale\n" +
		"instance never removes a recreated name. A lost reply may still mean the name\n" +
		"disappeared: inspect with callsheet ws ls before acting again. " + wsPlaneNote
	wsPruneDetails = "Flags:\n" +
		"  --instance TOKEN   the workspace's instance (from callsheet ws show)\n" +
		"  --before RFC3339   prune task refs published strictly before this time; a zone is\n" +
		"                     required, e.g. 2026-09-30T12:00:00Z\n" + trustHelp + wsJSONHelp + "\n" +
		"Deletes task refs whose plane publication time is strictly before the cutoff (commit\n" +
		"dates are never age) and collects every object no surviving ref reaches. Branches\n" +
		"and their history are always kept. Prints removed_task_refs, reclaimed_bytes and the\n" +
		"new size_bytes. " + wsPlaneNote
	wsStatusDetails = "Flags:\n" +
		"  --after REF --generation TOKEN --instance TOKEN\n" +
		"                     continue from the previous page (all three, from its output)\n" +
		"  --limit N          at most N rows (1-100, default 100)\n" + trustHelp + wsJSONHelp + "\n" +
		"Prints the plane's stored refs of a workspace sorted by name: tab-separated REF,\n" +
		"COMMIT and PUBLISHED_AT (task refs only; - for branches). An unborn main has no row.\n" +
		"A changed generation between pages conflicts: restart from the first page. It never\n" +
		"inspects a local working tree or reports ahead/behind.\n\n" +
		"With a TASK_ID (no paging flags) it prints that task's workspace status instead:\n" +
		"state, workspace_phase (preparing, executing or publishing; - when terminal), the\n" +
		"immutable binding, the terminal result's publication metadata (- while running) and\n" +
		"available: whether the current instance still holds the exact result ref and hash\n" +
		"(false after a prune, removal or recreation; the historical result stays). A task\n" +
		"without a workspace is invalid (no_task_workspace).\n" + wsPlaneNote
	wsDiffDetails = "Flags:\n" +
		"  --base SELECTOR    NAME TARGET form (required there): the base snapshot, or empty for\n" +
		"                     the empty tree\n" +
		"  --after CURSOR --instance TOKEN --generation TOKEN\n" +
		"                     continue from the previous page (all three, from its output; in\n" +
		"                     the NAME TARGET form base and target then as the printed full\n" +
		"                     hashes)\n" +
		"  --limit N          at most N rows (1-100, default 100)\n" + trustHelp + wsJSONHelp + "\n" +
		"Compares two stored snapshots and prints changed-path metadata only: tab-separated\n" +
		"KIND (added, deleted, modified), OLD_MODE, NEW_MODE, OLD_BYTES, NEW_BYTES and PATH (a\n" +
		"quoted UTF-8 path, or base64: and the raw path bytes). No file content, patch or\n" +
		"commit message is ever returned; renames are a delete and an add.\n" + wsSelectorHelp +
		"In the NAME TARGET form a bare task ID (as --base or TARGET) means its complete task\n" +
		"ref; a branch named like a task ID is refs/heads/t_....\n\n" +
		"With a TASK_ID (no --base, no TARGET) it compares the task's admitted base (or the\n" +
		"empty tree) with its published result commit, both fixed by the task's record. Every\n" +
		"page rechecks the bound instance and the exact result ref: a removed or recreated\n" +
		"workspace conflicts (workspace_instance_mismatch, even with identical hashes), a\n" +
		"pruned result is not_found, a changed generation conflicts; nothing restarts or\n" +
		"mixes pages. The simplest way to read every page is to run ws diff TASK_ID\n" +
		"again from the first page. To compare against another base use the NAME TARGET form.\n" + wsPlaneNote
	wsRefSetDetails = "Flags:\n" +
		"  --instance TOKEN   the workspace's instance (from callsheet ws show)\n" +
		"  --expected HASH|absent\n" +
		"                     the branch's current full hash, or absent to create it\n" +
		"  --delete           delete the branch instead of setting it (no TARGET)\n" + trustHelp + wsJSONHelp + "\n" +
		"Creates, moves or deletes one branch with compare-and-swap: a stale --expected\n" +
		"conflicts and nothing changes. Moves need no fast-forward and never merge; no other\n" +
		"branch moves. Deleting main leaves HEAD unborn. TARGET is a selector.\n" + wsSelectorHelp + wsBranchHelp + wsPlaneNote
)

func wsTree() *Command {
	return node("ws", "Workspace commands (on the plane)",
		planeLeaf("create", "Create a workspace", wsCreateUsage, wsCreateDetails, wsCreate),
		planeLeaf("ls", "List workspaces", wsLsUsage, wsLsDetails, wsLs),
		planeLeaf("show", "Show a workspace", wsShowUsage, wsShowDetails, wsShow),
		planeLeaf("rm", "Remove a workspace", wsRmUsage, wsRmDetails, wsRm),
		planeLeaf("prune", "Prune old task refs and unreachable objects", wsPruneUsage, wsPruneDetails, wsPrune),
		planeLeaf("push", "Push a clean repository's HEAD or a folder snapshot to a workspace branch", wsPushUsage, wsPushDetails, wsPush),
		planeLeaf("pull", "Pull a workspace commit into a repository or a new folder", wsPullUsage, wsPullDetails, wsPull),
		planeLeaf("status", "Show a workspace's refs on the plane", wsStatusUsage, wsStatusDetails, wsStatus),
		planeLeaf("diff", "Compare two workspace snapshots (metadata only)", wsDiffUsage, wsDiffDetails, wsDiff),
		node("ref", "Workspace ref commands",
			planeLeaf("set", "Create, move or delete a workspace branch", wsRefSetUsage, wsRefSetDetails, wsRefSet),
		),
	)
}

// wsName checks the workspace name operand.
func wsName(errOut io.Writer, name string) (int, bool) {
	if !contract.ValidWorkspaceName(name) {
		return planeFail(errOut, contract.InvalidWorkspaceName()), false
	}
	return 0, true
}

// wsOperands requires between lo and hi operands.
func wsOperands(c *Command, ops []string, lo int, errOut io.Writer) (int, bool) {
	if len(ops) < lo {
		return usageError(errOut, c, c.Path()+" needs "+map[int]string{1: "a workspace NAME", 2: "NAME and TARGET"}[lo]), false
	}
	return wsName(errOut, ops[0])
}

// wsLimit parses --limit (default 100).
func wsLimit(c *Command, l single, errOut io.Writer) (int, int, bool) {
	if !l.set {
		return contract.DefaultWorkspaceLimit, 0, true
	}
	n, ok := contract.ParseInteger(l.val)
	if !ok || n < 1 || n > contract.MaxWorkspaceLimit {
		return 0, usageError(errOut, c, "--limit must be an integer from 1 to 100"), false
	}
	return n, 0, true
}

func wsClient(ctx context.Context, f *remoteFlags, errOut io.Writer) (*client.Client, int, bool) {
	return roleClient(ctx, f, errOut)
}

// wsOut prints v as JSON or as text.
func wsOut(out, errOut io.Writer, asJSON bool, v any, text string) int {
	if asJSON {
		return writeJSON(out, errOut, v)
	}
	return writeOut(out, errOut, text)
}

func orDash(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

// RenderWorkspace renders a view as label: value lines.
func RenderWorkspace(v contract.WorkspaceView) string {
	return "name: " + v.Name + "\ninstance: " + v.Instance + "\ncreated_at: " + v.CreatedAt + "\ngeneration: " + v.Generation +
		"\ndefault_branch: " + v.DefaultBranch + "\nsize_bytes: " + strconv.FormatInt(v.SizeBytes, 10) +
		"\nretention: automatic_prune=false quota_bytes=null\n"
}

func wsCreate(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut)
	if !ok {
		return code
	}
	if code, ok := wsOperands(c, ops, 1, errOut); !ok {
		return code
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	v, err := cl.CreateWorkspace(ctx, ops[0])
	if err != nil {
		return planeFail(errOut, err)
	}
	return wsOut(out, errOut, f.json.val, v, RenderWorkspace(v))
}

func wsShow(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut)
	if !ok {
		return code
	}
	if code, ok := wsOperands(c, ops, 1, errOut); !ok {
		return code
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	v, err := cl.ShowWorkspace(ctx, ops[0])
	if err != nil {
		return planeFail(errOut, err)
	}
	return wsOut(out, errOut, f.json.val, v, RenderWorkspace(v))
}

func wsLs(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	var after, limit single
	f, _, code, ok := parseRemote(c, args, true, false, true, 0, errOut, func(fs *flag.FlagSet) {
		fs.Var(&after, "after", "")
		fs.Var(&limit, "limit", "")
	})
	if !ok {
		return code
	}
	n, code, ok := wsLimit(c, limit, errOut)
	if !ok {
		return code
	}
	if after.set && after.val == "" {
		return usageError(errOut, c, "--after must not be empty")
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := cl.ListWorkspaces(ctx, after.val, n)
	if err != nil {
		return planeFail(errOut, err)
	}
	var b strings.Builder
	b.WriteString(joinFields([]string{"NAME", "INSTANCE", "CREATED_AT"}))
	for _, s := range r.Workspaces {
		b.WriteString(joinFields([]string{s.Name, s.Instance, s.CreatedAt}))
	}
	if r.NextAfter != nil {
		b.WriteString("next_after: " + *r.NextAfter + "\n")
	}
	return wsOut(out, errOut, f.json.val, r, b.String())
}

// instanceFlag registers the required --instance.
func instanceFlag(v *single) func(*flag.FlagSet) {
	return func(fs *flag.FlagSet) { fs.Var(v, "instance", "") }
}

func needInstance(c *Command, v single, errOut io.Writer) (int, bool) {
	if !v.set {
		return usageError(errOut, c, "--instance is required: give the workspace's instance from callsheet ws show"), false
	}
	if err := contract.ValidateInstance(v.val); err != nil {
		return planeFail(errOut, err), false
	}
	return 0, true
}

func wsRm(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	var inst single
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, instanceFlag(&inst))
	if !ok {
		return code
	}
	if code, ok := wsOperands(c, ops, 1, errOut); !ok {
		return code
	}
	if code, ok := needInstance(c, inst, errOut); !ok {
		return code
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := cl.RemoveWorkspace(ctx, ops[0], inst.val)
	if err != nil {
		return planeFail(errOut, err)
	}
	return wsOut(out, errOut, f.json.val, r, "removed: "+r.Name+" instance: "+r.Instance+"\n")
}

func wsPrune(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	var inst, before single
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, instanceFlag(&inst), func(fs *flag.FlagSet) { fs.Var(&before, "before", "") })
	if !ok {
		return code
	}
	if code, ok := wsOperands(c, ops, 1, errOut); !ok {
		return code
	}
	if code, ok := needInstance(c, inst, errOut); !ok {
		return code
	}
	if !before.set {
		return usageError(errOut, c, "--before is required: an RFC3339 cutoff with a zone")
	}
	if _, err := contract.ParseCutoff(before.val); err != nil {
		return planeFail(errOut, err)
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := cl.PruneWorkspace(ctx, ops[0], inst.val, before.val)
	if err != nil {
		return planeFail(errOut, err)
	}
	text := "name: " + r.Name + "\ninstance: " + r.Instance + "\ngeneration: " + r.Generation +
		"\nremoved_task_refs: " + strconv.FormatInt(r.RemovedTaskRefs, 10) + "\nreclaimed_bytes: " + strconv.FormatInt(r.ReclaimedBytes, 10) +
		"\nsize_bytes: " + strconv.FormatInt(r.SizeBytes, 10) + "\n"
	return wsOut(out, errOut, f.json.val, r, text)
}

// continuationFlags are --after, --instance and --generation.
type continuationFlags struct{ after, instance, generation, limit single }

func (cf *continuationFlags) register(fs *flag.FlagSet) {
	fs.Var(&cf.after, "after", "")
	fs.Var(&cf.instance, "instance", "")
	fs.Var(&cf.generation, "generation", "")
	fs.Var(&cf.limit, "limit", "")
}

func (cf *continuationFlags) check(c *Command, errOut io.Writer) (int, bool) {
	if cf.after.set != cf.instance.set || cf.after.set != cf.generation.set {
		return usageError(errOut, c, "--after, --instance and --generation continue a previous page and go together"), false
	}
	for _, v := range []single{cf.after, cf.instance, cf.generation} {
		if v.set && v.val == "" {
			return usageError(errOut, c, "continuation flags must not be empty"), false
		}
	}
	return 0, true
}

func wsStatus(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	var cf continuationFlags
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, cf.register)
	if !ok {
		return code
	}
	if len(ops) == 1 {
		task, err := taskForm(ops[0])
		if err != nil {
			return planeFail(errOut, err)
		}
		if task {
			return wsTaskStatus(ctx, c, f, &cf, ops[0], out, errOut)
		}
	}
	if code, ok := wsOperands(c, ops, 1, errOut); !ok {
		return code
	}
	if code, ok := cf.check(c, errOut); !ok {
		return code
	}
	n, code, ok := wsLimit(c, cf.limit, errOut)
	if !ok {
		return code
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := cl.WorkspaceStatus(ctx, ops[0], client.StatusPage{After: cf.after.val, Instance: cf.instance.val, Generation: cf.generation.val, Limit: n})
	if err != nil {
		return planeFail(errOut, err)
	}
	var b strings.Builder
	b.WriteString("name: " + r.Name + "\ninstance: " + r.Instance + "\ngeneration: " + r.Generation + "\ndefault_branch: " + r.DefaultBranch + "\n")
	b.WriteString(joinFields([]string{"REF", "COMMIT", "PUBLISHED_AT"}))
	for _, ref := range r.Refs {
		b.WriteString(joinFields([]string{ref.Name, ref.Commit, orDash(ref.PublishedAt)}))
	}
	if r.NextAfter != nil {
		b.WriteString("next_after: " + *r.NextAfter + "\n")
	}
	return wsOut(out, errOut, f.json.val, r, b.String())
}

// wsTaskStatus is ws status TASK_ID (iteration 10c): no paging flags; the
// exact versioned envelope as JSON, or its fields as text.
func wsTaskStatus(ctx context.Context, c *Command, f *remoteFlags, cf *continuationFlags, id string, out, errOut io.Writer) int {
	if cf.after.set || cf.instance.set || cf.generation.set || cf.limit.set {
		return usageError(errOut, c, c.Path()+" TASK_ID takes no --after, --instance, --generation or --limit")
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := taskWorkspaceStatus(cl, ctx, id)
	if err != nil {
		return planeFail(errOut, err)
	}
	return wsOut(out, errOut, f.json.val, r, RenderTaskWorkspaceStatus(r.Workspace))
}

// RenderTaskWorkspaceStatus renders a task's workspace status as label:
// value lines (- for null), its result's change rows as the diff table;
// metadata only, never contents.
func RenderTaskWorkspaceStatus(s contract.TaskWorkspaceStatus) string {
	var b strings.Builder
	line := func(k, v string) { b.WriteString(k + ": " + v + "\n") }
	line("task_id", s.TaskID)
	line("state", s.State)
	line("workspace_phase", orDash(s.WorkspacePhase))
	line("workspace", s.Binding.Name)
	line("workspace_instance", s.Binding.Instance)
	line("base_selector", s.Binding.BaseSelector)
	line("base_commit", orDash(s.Binding.BaseCommit))
	line("available", boolText(s.Available))
	r := s.Result
	if r == nil {
		line("publication", "-")
		return b.String()
	}
	line("publication", r.Publication)
	line("result_commit", orDash(r.Commit))
	line("result_ref", orDash(r.Ref))
	line("publication_error", orDash(r.Error))
	ds := "-"
	if d := r.Diffstat; d != nil {
		ds = "added=" + strconv.FormatInt(d.Added, 10) + " modified=" + strconv.FormatInt(d.Modified, 10) + " deleted=" + strconv.FormatInt(d.Deleted, 10) +
			" old_bytes=" + strconv.FormatInt(d.OldBytes, 10) + " new_bytes=" + strconv.FormatInt(d.NewBytes, 10)
	}
	line("diffstat", ds)
	if len(r.Changes) > 0 {
		writeChanges(&b, r.Changes)
	}
	if r.ChangesTruncated {
		line("changes_truncated", "true")
		line("next_after", orDash(r.NextAfter))
	}
	return b.String()
}

// writeChanges writes the diff table header and one row per change.
func writeChanges(b *strings.Builder, changes []contract.WorkspaceChange) {
	b.WriteString(joinFields([]string{"KIND", "OLD_MODE", "NEW_MODE", "OLD_BYTES", "NEW_BYTES", "PATH"}))
	for _, ch := range changes {
		b.WriteString(joinFields([]string{ch.Kind, orDash(ch.OldMode), orDash(ch.NewMode), sizeOrDash(ch.OldBytes), sizeOrDash(ch.NewBytes), DisplayPath(ch.PathBase64)}))
	}
}

// renderDiff renders one diff page as text.
func renderDiff(r contract.WorkspaceDiffResponse) string {
	var b strings.Builder
	baseCommit := "empty"
	if r.BaseCommit != nil {
		baseCommit = *r.BaseCommit
	}
	b.WriteString("name: " + r.Name + "\ninstance: " + r.Instance + "\ngeneration: " + r.Generation + "\nbase_commit: " + baseCommit + "\ntarget_commit: " + r.TargetCommit + "\n")
	writeChanges(&b, r.Changes)
	if r.NextAfter != nil {
		b.WriteString("next_after: " + *r.NextAfter + "\n")
	}
	return b.String()
}

// DisplayPath renders a raw diff path for a terminal: a valid UTF-8 path
// as a quoted string with every control character escaped, any other as
// base64: followed by its standard base64.
func DisplayPath(b64 string) string {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !utf8.Valid(raw) {
		return "base64:" + b64
	}
	return strconv.Quote(string(raw))
}

func sizeOrDash(n *int64) string {
	if n == nil {
		return "-"
	}
	return strconv.FormatInt(*n, 10)
}

func wsDiff(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	var cf continuationFlags
	var base single
	f, ops, code, ok := parseRemote(c, args, true, false, true, 2, errOut, cf.register, func(fs *flag.FlagSet) { fs.Var(&base, "base", "") })
	if !ok {
		return code
	}
	task := false
	if len(ops) > 0 {
		var err error
		if task, err = taskForm(ops[0]); err != nil {
			return planeFail(errOut, err)
		}
	}
	if task {
		switch {
		case len(ops) != 1:
			return usageError(errOut, c, c.Path()+" TASK_ID takes no TARGET: it compares the task's base with its result")
		case base.set:
			return usageError(errOut, c, c.Path()+" TASK_ID takes no --base (its base is the task's admitted base); compare against another base with --base BASE NAME TARGET")
		}
	} else {
		if code, ok := wsOperands(c, ops, 2, errOut); !ok {
			return code
		}
		if !base.set {
			return usageError(errOut, c, "--base is required: a selector or empty")
		}
	}
	if code, ok := cf.check(c, errOut); !ok {
		return code
	}
	n, code, ok := wsLimit(c, cf.limit, errOut)
	if !ok {
		return code
	}
	var page client.DiffPage
	if !task {
		// A bare task ID as base or target means its full task ref.
		b, err := contract.ParseTaskSelector(base.val, "base", true)
		if err != nil {
			return planeFail(errOut, err)
		}
		t, err := contract.ParseTaskSelector(ops[1], "target", false)
		if err != nil {
			return planeFail(errOut, err)
		}
		page = client.DiffPage{Base: contract.SelectorString(b), Target: contract.SelectorString(t), After: cf.after.val, Instance: cf.instance.val,
			Generation: cf.generation.val, Limit: n}
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	var r contract.WorkspaceDiffResponse
	var err error
	if task {
		r, err = taskWorkspaceDiff(cl, ctx, ops[0], client.TaskDiffPage{After: cf.after.val, Instance: cf.instance.val, Generation: cf.generation.val, Limit: n})
	} else {
		r, err = cl.WorkspaceDiff(ctx, ops[0], page)
	}
	if err != nil {
		return planeFail(errOut, err)
	}
	return wsOut(out, errOut, f.json.val, r, renderDiff(r))
}

func wsRefSet(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	var inst, expected single
	var del boolFlag
	f, ops, code, ok := parseRemote(c, args, true, false, true, 3, errOut, instanceFlag(&inst), func(fs *flag.FlagSet) {
		fs.Var(&expected, "expected", "")
		fs.Var(&del, "delete", "")
	})
	if !ok {
		return code
	}
	if len(ops) < 2 {
		return usageError(errOut, c, c.Path()+" needs NAME and BRANCH (and TARGET unless --delete)")
	}
	if code, ok := wsName(errOut, ops[0]); !ok {
		return code
	}
	if code, ok := needInstance(c, inst, errOut); !ok {
		return code
	}
	if !expected.set {
		return usageError(errOut, c, "--expected is required: the branch's current full hash, or absent")
	}
	req := contract.WorkspaceRefSetRequest{Instance: inst.val, Branch: ops[1], Expected: expected.val}
	if del.val {
		t := true
		req.Delete = &t
	}
	if len(ops) == 3 {
		req.Target = &ops[2]
	}
	if _, err := contract.ValidateRefSet(req); err != nil {
		return planeFail(errOut, err)
	}
	cl, code, ok := wsClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := cl.SetWorkspaceRef(ctx, ops[0], req)
	if err != nil {
		return planeFail(errOut, err)
	}
	text := "name: " + r.Name + "\ninstance: " + r.Instance + "\ngeneration: " + r.Generation + "\nref: " + r.Ref +
		"\nold_commit: " + orDash(r.OldCommit) + "\nnew_commit: " + orDash(r.NewCommit) + "\n"
	return wsOut(out, errOut, f.json.val, r, text)
}
