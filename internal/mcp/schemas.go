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
	return nil
}
