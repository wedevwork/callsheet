package mcp

import (
	"github.com/wedevwork/callsheet/internal/contract"
)

// Input metadata (FP-1, tools.md "Shared schema conventions"). Every
// schema is fully materialized JSON Schema: an object with
// additionalProperties false on the arguments and every nested argument
// object, the required members, exact integer/boolean/string/array types
// and local constraints only (no $ref, no remote reference). The schemas
// guide the coordinator; the contract validators remain authoritative
// (byte limits are UTF-8 bytes even where maxLength counts characters).

// Patterns of the identifier grammars.
const (
	slugPattern      = `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`
	nodeIDPattern    = `^n_[0-9a-f]{32}$`
	taskIDPattern    = `^t_[0-9a-f]{32}$`
	operationPattern = `^[0-9a-f]{32}$`
)

type schema = map[string]any

func object(props schema, required ...string) schema {
	s := schema{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) schema { return schema{"type": "string", "description": desc} }

func pattern(desc, p string) schema {
	return schema{"type": "string", "pattern": p, "description": desc}
}

func text(desc string, maxLen int) schema {
	return schema{"type": "string", "minLength": 1, "maxLength": maxLen, "description": desc}
}

func integer(desc string, lo, hi, def int, withDefault bool) schema {
	s := schema{"type": "integer", "minimum": lo, "maximum": hi, "description": desc}
	if withDefault {
		s["default"] = def
	}
	return s
}

func boolean(desc string) schema {
	return schema{"type": "boolean", "default": false, "description": desc}
}

func slug(desc string) schema   { return pattern(desc, slugPattern) }
func nodeID(desc string) schema { return pattern(desc, nodeIDPattern) }
func taskID(desc string) schema { return pattern(desc, taskIDPattern) }
func duration(desc string) schema {
	return str(desc + " A Go duration string such as 90s, 5m or 2h (not a JSON number).")
}

// roleText is a role field of printable UTF-8 text.
func roleText(desc string) schema {
	return schema{"type": "string", "minLength": 1, "maxLength": 1024, "description": desc}
}

// schemaFor returns the input schema of tool name.
func schemaFor(name string) schema {
	switch name {
	case toolNodeLs, toolRoleLs:
		return object(schema{})
	case toolNodeShow:
		return object(schema{"id": nodeID("The node ID (n_ followed by 32 lowercase hex digits).")}, "id")
	case toolRoleAdd:
		return object(schema{
			"id":          slug("The new role's unique ID (a slug of lowercase letters, digits and internal hyphens, 1-63 bytes)."),
			"name":        slug("The logical role name; several roles may share it."),
			"node":        nodeID("The worker node ID (see node_ls)."),
			"adapter":     slug("A registered adapter ID: claude (Claude Code), codex (Codex CLI) or fake (a test/demo adapter that never calls a model)."),
			"instruction": roleText("Absolute path of the instruction manual on the worker node (never opened by the coordinator)."),
			"runbook":     roleText("Absolute path of the runbook manual on the worker node (never opened by the coordinator)."),
			"model":       roleText("Model name passed to the adapter (never inferred or defaulted); claude accepts only sonnet and codex only gpt-6.1-sol (their qualified pairs, checked by the worker node); fake: free text."),
			"effort":      slug("Effort, one of the adapter's efforts (claude, codex: low; fake: low, medium, high)."),
			"concurrency": integer("Concurrent tasks for this role, shared by all coordinators.", 1, contract.MaxConcurrency, 0, false),
			"timeout":     duration("Task execution timeout; 0 is unlimited; omitted keeps the role default (2h)."),
		}, "id", "name", "node", "adapter", "instruction", "runbook", "model", "effort", "concurrency")
	case toolRoleSet:
		s := object(schema{
			"id":          slug("The role ID to change (it, the node and the registration order never change)."),
			"name":        slug("New logical role name."),
			"adapter":     slug("New registered adapter ID: claude, codex or fake."),
			"instruction": roleText("New absolute instruction manual path on the worker node."),
			"runbook":     roleText("New absolute runbook manual path on the worker node."),
			"model":       roleText("New model name."),
			"effort":      slug("New effort, one of the adapter's efforts."),
			"concurrency": integer("New concurrency.", 1, contract.MaxConcurrency, 0, false),
			"timeout":     duration("New task execution timeout; 0 is unlimited."),
		}, "id")
		s["minProperties"] = 2
		return s
	case toolRoleShow:
		return object(schema{"id": slug("The role ID.")}, "id")
	case toolRoleRm:
		return object(schema{
			"id":        slug("The role ID to remove."),
			"force":     boolean("Cancel the role's tasks first; the plane fences the instance and removes it once every task ended durably."),
			"operation": pattern("With force only: the removal operation token of a pending forced removal (role_show's removal), to join exactly that operation.", operationPattern),
		}, "id")
	case toolDispatch:
		override := object(schema{
			"model":   roleText("Override the role's model for this task only."),
			"effort":  slug("Override the role's effort for this task only."),
			"timeout": duration("This task's execution timeout; 0 is unlimited."),
		})
		override["minProperties"] = 1
		return object(schema{
			"target": object(schema{
				"kind":  schema{"type": "string", "enum": []string{contract.TargetID, contract.TargetName}, "description": "id: exactly this role instance; name: the first role of this name, in registration order, that can accept now."},
				"value": slug("The role ID or role name."),
			}, "kind", "value"),
			"goal":       text("What the task must achieve (passed verbatim; at most 16 KiB of UTF-8).", contract.MaxGoalBytes),
			"acceptance": text("How the result will be judged (passed through, never evaluated; at most 16 KiB).", contract.MaxAcceptanceBytes),
			"payload": schema{"type": "array", "maxItems": contract.MaxPayloadPointers, "default": []string{},
				"items":       schema{"type": "string", "minLength": 1, "maxLength": contract.MaxPointerBytes},
				"description": "Opaque pointers for the worker (never fetched): at most 64, each at most 2 KiB, 64 KiB in total."},
			"override": override,
			"wait":     duration("After admission, wait at most this long (0 to 5m) for the task to end, capped by the call budget; omitted: no wait."),
		}, "target", "goal", "acceptance")
	case toolTaskLs:
		return object(schema{
			"after": taskID("Start after this task ID (the previous page's next_after)."),
			"limit": integer("At most this many tasks.", 1, contract.MaxTaskListLimit, contract.DefaultTaskListLimit, true),
		})
	case toolTaskShow:
		return object(schema{
			"id":    taskID("The task ID."),
			"lines": integer("The last lines of retained output to include.", 0, contract.MaxTailLines, contract.DefaultTailLines, true),
		}, "id")
	case toolTaskLogs:
		return object(schema{
			"id":   taskID("The task ID."),
			"late": boolean("Return the late result's separate output instead (a task without one is not_found)."),
		}, "id")
	case toolTaskCancel:
		return object(schema{"id": taskID("The task ID.")}, "id")
	case toolTaskWait:
		return object(schema{
			"task_ids": schema{"type": "array", "minItems": 1, "maxItems": contract.MaxWaitIDs, "uniqueItems": true,
				"items": schema{"type": "string", "pattern": taskIDPattern}, "description": "1 to 16 unique task IDs; the first to end durably wins."},
			"wait": duration("Wait at most this long (0 to 5m; 0 is an immediate snapshot), capped by the call budget; omitted: the whole available budget."),
		}, "task_ids")
	}
	return workspaceSchema(name)
}

// Workspace grammars (iteration 09a). A branch is byte-exact lowercase
// ASCII: slash-separated components of 1-200 bytes, at most 512 bytes as
// refs/heads/... (the validators also refuse "..", ".lock" suffixes and a
// trailing "."). Selectors add full hashes, complete task refs and, for a
// diff base, empty.
const (
	tokenPattern    = `^[0-9a-f]{32}$`
	branchPattern   = `^(refs/heads/)?[a-z0-9][a-z0-9._-]{0,199}(/[a-z0-9][a-z0-9._-]{0,199})*$`
	selectorPattern = `^([0-9a-f]{40}|refs/callsheet/tasks/t_[0-9a-f]{32}|(refs/heads/)?[a-z0-9][a-z0-9._-]{0,199}(/[a-z0-9][a-z0-9._-]{0,199})*)$`
	basePattern     = `^(empty|[0-9a-f]{40}|refs/callsheet/tasks/t_[0-9a-f]{32}|(refs/heads/)?[a-z0-9][a-z0-9._-]{0,199}(/[a-z0-9][a-z0-9._-]{0,199})*)$`
	cursorPattern   = `^(refs/heads/[a-z0-9][a-z0-9._-]{0,199}(/[a-z0-9][a-z0-9._-]{0,199})*|refs/callsheet/tasks/t_[0-9a-f]{32})$`
)

func wsName() schema {
	return slug("The workspace name (a slug of lowercase letters, digits and internal hyphens, 1-63 bytes), named explicitly on every call.")
}

func wsInstance() schema {
	return pattern("The workspace's instance token from ws_show (32 lowercase hex digits); it prevents acting on a recreated name.", tokenPattern)
}

func wsLimit() schema {
	return integer("At most this many rows.", 1, contract.MaxWorkspaceLimit, contract.DefaultWorkspaceLimit, true)
}

func bounded(desc, p string, maxLen int) schema {
	s := pattern(desc, p)
	s["maxLength"] = maxLen
	return s
}

func workspaceSchema(name string) schema {
	cont := func() schema {
		return schema{
			"instance":   pattern("Continuation only (with after and generation): the previous page's instance.", tokenPattern),
			"generation": pattern("Continuation only (with after and instance): the previous page's generation; a changed generation conflicts.", tokenPattern),
			"limit":      wsLimit(),
		}
	}
	switch name {
	case toolWsCreate, toolWsShow:
		return object(schema{"name": wsName()}, "name")
	case toolWsLs:
		return object(schema{
			"after": text("Start after this name (the previous page's next_after).", 63),
			"limit": wsLimit(),
		})
	case toolWsRm:
		return object(schema{"name": wsName(), "instance": wsInstance()}, "name", "instance")
	case toolWsPrune:
		return object(schema{
			"name":     wsName(),
			"instance": wsInstance(),
			"before":   str("Prune task refs published strictly before this RFC3339 time; a zone is required, e.g. 2026-09-30T12:00:00Z."),
		}, "name", "instance", "before")
	case toolWsRefSet:
		return object(schema{
			"name":     wsName(),
			"instance": wsInstance(),
			"branch":   bounded("The branch: a short name (main) or refs/heads/...; byte-exact lowercase ASCII, never repaired.", branchPattern, contract.MaxFullRef),
			"expected": schema{"type": "string", "pattern": `^([0-9a-f]{40}|absent)$`, "description": "The branch's current full commit hash, or absent to create it (compare-and-swap)."},
			"target":   bounded("The new value (omit with delete): a full commit hash reachable from a current ref, a branch or a complete refs/callsheet/tasks/ ref.", selectorPattern, contract.MaxFullRef),
			"delete":   boolean("Delete the branch instead (expected must be its current hash)."),
		}, "name", "instance", "branch", "expected")
	case toolWsStatus:
		props := cont()
		props["after"] = bounded("Continuation only (with instance and generation): the previous page's next_after, a complete ref.", cursorPattern, contract.MaxFullRef)
		props["name"] = wsName()
		return object(props, "name")
	case toolWsDiff:
		props := cont()
		props["after"] = pattern("Continuation only (with instance and generation): the previous page's next_after, base64 of a raw path.", `^[A-Za-z0-9+/]+={0,2}$`)
		props["name"] = wsName()
		props["base"] = bounded("The base snapshot: a selector, or empty for the empty tree (a continuation uses the returned base_commit or empty).", basePattern, contract.MaxFullRef)
		props["target"] = bounded("The target snapshot: a full reachable commit hash, a branch or a complete refs/callsheet/tasks/ ref (a continuation uses the returned target_commit).", selectorPattern, contract.MaxFullRef)
		return object(props, "name", "base", "target")
	case toolWsPush:
		return object(schema{
			"name":     wsName(),
			"instance": wsInstance(),
			"branch":   bounded("The target branch, a short name (default main) or refs/heads/...: slash-separated components of 1-200 bytes of lowercase ASCII letters, digits, '.', '_' and '-' (starting with a letter or digit), at most 512 bytes as a full ref; byte-exact, never repaired.", branchPattern, contract.MaxFullRef),
			"path":     localPath("The local source directory (a git repository root or a plain folder) on the machine running callsheet mcp; omitted: the server's working directory."),
		}, "name", "instance")
	case toolWsPull:
		return object(schema{
			"name": wsName(),
			"ref":  bounded("The commit to pull: a branch (short or refs/heads/...), a full 40-hex commit hash reachable from a workspace ref or a complete refs/callsheet/tasks/t_<32 hex> ref (t_... alone is a branch name); no short hashes or revision expressions.", selectorPattern, contract.MaxFullRef),
			"path": localPath("The local destination on the machine running callsheet mcp: an existing git repository root, or a new or empty directory; omitted: the server's working directory."),
		}, "name", "ref")
	}
	return nil
}

// localPath is a local path argument: nonempty, at most the Linux native
// limit (4095 bytes; the validators apply each OS's native byte limit and
// reject invalid UTF-8 and NUL).
func localPath(desc string) schema {
	return schema{"type": "string", "minLength": 1, "maxLength": contract.MaxPathArgument, "description": desc}
}
