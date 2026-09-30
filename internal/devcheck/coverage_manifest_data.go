package devcheck

// WorkspaceCoverageManifest is iteration 09a's committed coverage manifest:
// every new production file (all blocks) and the changed executable line
// ranges (new side of the diff against the iteration's base, d56ae28) of
// existing production files. The reviewer compares it with the
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
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/devcheck.go", Ranges: [][2]int{{264, 266}, {274, 279}, {362, 369}}},
	{Group: GroupChanged, File: modulePath + "/internal/devcheck/native.go", Ranges: [][2]int{{724, 730}}},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/schemas.go", Ranges: [][2]int{{153, 232}}},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/server.go", Ranges: [][2]int{{483, 483}}},
	{Group: GroupChanged, File: modulePath + "/internal/mcp/tools.go", Ranges: [][2]int{{138, 138}, {166, 484}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/server.go", Ranges: [][2]int{{107, 117}, {121, 121}, {151, 151}, {161, 161}, {221, 225}, {234, 236}, {279, 282}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/service.go", Ranges: [][2]int{{58, 62}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/state.go", Ranges: [][2]int{{162, 162}, {208, 210}, {238, 238}, {246, 248}, {272, 272}, {278, 278}, {280, 281}}},
	{Group: GroupChanged, File: modulePath + "/internal/plane/status.go", Ranges: [][2]int{{92, 100}}},
}

// modulePath is the profile's import-path prefix.
const modulePath = "github.com/wedevwork/callsheet"
