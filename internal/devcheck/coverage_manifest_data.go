package devcheck

// WorkspaceCoverageManifest is the committed coverage manifest of
// iterations 09a and 09b. 09a: every new production file (all blocks) and
// the changed executable line ranges (new side of the diff against the
// iteration's base, d56ae28) of existing production files. 09b (base
// 631cbc0): every new or changed production file with an empty Ranges
// list (its whole file), the transfer package in GroupTransfer and its
// build-selected publication files native-only. Every applicable listed
// file must also pass on its own. The reviewer compares it with the
// implementation diff; CI consumes it without any git base or network.
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
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/devcheck.go", Ranges: [][2]int{{265, 267}, {278, 283}, {372, 380}}},
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/native.go", Ranges: [][2]int{{739, 746}}},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/schemas.go", Ranges: [][2]int{{153, 245}}},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/server.go", Ranges: [][2]int{{491, 491}}},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/tools.go", Ranges: [][2]int{{142, 142}, {170, 488}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/server.go", Ranges: [][2]int{{107, 117}, {121, 121}, {151, 151}, {161, 161}, {221, 225}, {234, 236}, {279, 282}}},
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
}

// modulePath is the profile's import-path prefix.
const modulePath = "github.com/wedevwork/callsheet"
