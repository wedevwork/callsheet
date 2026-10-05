package devcheck

// WorkspaceCoverageManifest is the committed coverage manifest of
// iterations 09a, 09b and 10a. 09a: every new production file (all
// blocks) and the changed executable line ranges (new side of the diff
// against the iteration's base, d56ae28) of existing production files.
// 09b (base 631cbc0): every new or changed production file with an empty
// Ranges list (its whole file), the transfer package in GroupTransfer and
// its build-selected publication files native-only. 10a (base 48078df):
// its two new build-selected sidecar files, whole and each on its own OS,
// and its four changed sidecar files, whole and common.
// Every applicable listed file must also pass on its own. The reviewer
// compares it with the implementation diff; CI consumes it without any
// git base or network.
var WorkspaceCoverageManifest = []CoverageEntry{
	// New: the workspace hub package.
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/billy.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/diff.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/fs.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/lock.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/manager.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/prune.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/refs.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/repo.go"},
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/transport.go"},
	// New: contracts, client, CLI, plane API and devcheck policy files.
	{Group: GroupChanged, File: modulePath + "/internal/contract/workspace.go"},
	{Group: GroupChanged, File: modulePath + "/internal/client/workspaces.go"},
	{Group: GroupChanged, File: modulePath + "/internal/cli/workspace.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/workspaces.go"},
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/coverage_manifest.go"},
	// Changed executable ranges of existing production files (hunks that
	// only touch comments, imports, constants, variables or struct fields
	// hold no executable block and are not listed).
	{Group: GroupChanged, File: modulePath + "/internal/cli/cli.go", Ranges: [][2]int{{151, 151}}},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/server.go", Ranges: [][2]int{{491, 491}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/service.go", Ranges: [][2]int{{58, 62}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/state.go", Ranges: [][2]int{{162, 162}, {208, 210}, {238, 238}, {246, 248}, {272, 272}, {278, 278}, {280, 281}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/status.go", Ranges: [][2]int{{92, 100}}},

	// Iteration 09b, new: the local transfer package (whole files; the
	// publication primitives only on their own OS).
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/attributes.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/config.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/convert.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/errors.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/export.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/fetch.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/fsutil.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/gitconfig_rules.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/ignore.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/indexfile.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/manifest.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/objects.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/publish_darwin.go", OS: "darwin"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/publish_linux.go", OS: "linux"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/refs.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/replace.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/snapshot.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/source.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/status.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/transfer.go"},
	// Iteration 09b, new: contracts, the git session, the CLI leaves and
	// the MCP tools.
	{Group: GroupChanged, File: modulePath + "/internal/contract/workspace_transfer.go"},
	{Group: GroupChanged, File: modulePath + "/internal/client/workspace_git.go"},
	{Group: GroupChanged, File: modulePath + "/internal/cli/workspace_transfer.go"},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/transfer.go"},
	// Iteration 09b, changed executable code of existing files (whole
	// files; duplicates of 09a entries count each block once).
	{Group: GroupChanged, File: modulePath + "/internal/cli/mcp.go"},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/schemas.go"},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/tools.go"},
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/devcheck.go"},
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/native.go"},

	// Iteration 10a (base 48078df), new: the build-selected native
	// group-completion primitives of the task guardian, whole files, each
	// evaluated on its own OS.
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/group_alone_darwin.go", OS: "darwin"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/group_alone_linux.go", OS: "linux"},
	// Iteration 10a, changed executable code of existing files (whole
	// files, evaluated on both systems: session.go and tasks.go build
	// everywhere, guardian.go and task_process_unix.go on linux || darwin).
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/guardian.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/session.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/task_process_unix.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/tasks.go"},

	// Iteration 10b (base 7f2e534): every new or changed production file
	// as a whole file (no build constraint: evaluated on both systems).
	// The hub's task routes and the transfer package's task primitives
	// join their package groups; everything else is GroupChanged. The
	// 09a partial plane/server.go entry is superseded by its whole file, and
	// the 09a partial devcheck.go and native.go entries are removed (their
	// 09b whole-file entries remain).
	{Group: GroupWorkspace, File: modulePath + "/internal/workspace/tasks.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/task.go"},
	{Group: GroupTransfer, File: modulePath + "/internal/workspacetransfer/task_snapshot.go"},
	{Group: GroupChanged, File: modulePath + "/internal/taskworkspace/cache.go"},
	{Group: GroupChanged, File: modulePath + "/internal/taskworkspace/prepare.go"},
	{Group: GroupChanged, File: modulePath + "/internal/taskworkspace/publish.go"},
	{Group: GroupChanged, File: modulePath + "/internal/taskworkspace/result.go"},
	{Group: GroupChanged, File: modulePath + "/internal/taskpublication/service.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/contract.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/control.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/frame.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/journal.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/journal_workspace.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/node.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/task.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/task_record.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/task_view.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/task_workspace.go"},
	{Group: GroupChanged, File: modulePath + "/internal/adapter/task.go"},
	{Group: GroupChanged, File: modulePath + "/internal/adapter/vendor.go"},
	{Group: GroupChanged, File: modulePath + "/internal/client/client.go"},
	{Group: GroupChanged, File: modulePath + "/internal/client/node_workspace.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/node_stream.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/nodes.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/server.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/task_api.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/task_controls.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/task_recon.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/task_workspace.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/task_writer.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/tasks.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/run.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/session_tasks.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/sidecar.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/state.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/task_journal.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/task_platform.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/task_recovery.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/task_workspace.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/task_storage.go"},
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/stress.go"},

	// Iteration 10c (base 362405f): the two production files it changes
	// that no earlier entry lists whole, as whole files (no build
	// constraint: evaluated on both systems). Its other changed files are
	// already listed whole: contract/task_workspace.go, cli/workspace.go,
	// cli/workspace_transfer.go, mcp/schemas.go, mcp/tools.go,
	// mcp/transfer.go, plane/task_api.go, plane/task_workspace.go,
	// workspacetransfer/transfer.go (GroupTransfer) and devcheck/native.go;
	// the 09a partial mcp/schemas.go and mcp/tools.go entries are removed
	// (their 09b whole-file entries remain).
	{Group: GroupChanged, File: modulePath + "/internal/client/tasks.go"},
	{Group: GroupChanged, File: modulePath + "/internal/cli/task.go"},

	// Iteration 11 (base 1d9d663): the production files it changes that no
	// earlier entry lists whole, as whole files (no build constraint:
	// evaluated on both systems); the smoke harness in internal/testkit is
	// test support, not production. Its other changed files are already
	// listed whole: adapter/vendor.go,
	// contract/task.go, contract/journal.go, mcp/schemas.go, mcp/tools.go,
	// sidecar/run.go, sidecar/sidecar.go, sidecar/tasks.go and
	// devcheck/native.go.
	{Group: GroupChanged, File: modulePath + "/internal/adapter/adapter.go"},
	{Group: GroupChanged, File: modulePath + "/internal/adapter/final.go"},
	{Group: GroupChanged, File: modulePath + "/internal/sidecar/roles.go"},
	{Group: GroupChanged, File: modulePath + "/internal/cli/sidecar.go"},
	{Group: GroupChanged, File: modulePath + "/internal/cli/role.go"},
	{Group: GroupChanged, File: modulePath + "/internal/contract/role.go"},
	{Group: GroupChanged, File: modulePath + "/internal/plane/roles.go"},
}

// modulePath is the profile's import-path prefix.
const modulePath = "github.com/wedevwork/callsheet"
