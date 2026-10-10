//go:build realadaptercheck

package mcp

import (
	"strings"
	"testing"
)

// TestSelectionSchemas is UT-6 (FP-6, design 12a-worker-selection; build
// tag realadaptercheck, never a stress shard): role_add, role_set and
// dispatch keep the model a bounded string (never an enum) and the effort
// a slug (never a universal enum, its set depending on the role's
// adapter), and describe free model text, vendor-owned compatibility,
// every adapter's effort set and the minimum versions while keeping the
// adapter posture notes.
func TestSelectionSchemas(t *testing.T) {
	props := func(s schema) schema { return s["properties"].(schema) }
	override := props(schemaFor(toolDispatch))["override"].(schema)
	fields := map[string]schema{
		"role_add": props(schemaFor(toolRoleAdd)),
		"role_set": props(schemaFor(toolRoleSet)),
		"dispatch": props(override),
	}
	sets := "claude low, medium, high, xhigh, max; codex low, medium, high, xhigh, max, ultra; grok low, medium, high, xhigh; cursor none, minimal, low, medium, high, xhigh, max; fake low, medium, high."
	for name, p := range fields {
		model, effort := p["model"].(schema), p["effort"].(schema)
		if model["type"] != "string" || model["minLength"] != 1 || model["maxLength"] != 1024 || model["enum"] != nil || model["pattern"] != nil {
			t.Fatalf("%s model schema %v", name, model)
		}
		if effort["type"] != "string" || effort["enum"] != nil || effort["pattern"] != slugPattern {
			t.Fatalf("%s effort schema %v", name, effort)
		}
		md, ed := model["description"].(string), effort["description"].(string)
		for _, w := range []string{"free text passed unchanged to the adapter", "never inferred or defaulted", "no model list is kept",
			"the vendor decides whether it runs the model with the effort", "a vendor refusal is the task's result"} {
			if !strings.Contains(md, w) {
				t.Fatalf("%s model description lacks %q: %s", name, w, md)
			}
		}
		if !strings.HasSuffix(ed, sets) || strings.Contains(md+ed, "qualified pair") || strings.Contains(md+ed, "only sonnet") {
			t.Fatalf("%s effort description %q", name, ed)
		}
	}
	add := props(schemaFor(toolRoleAdd))
	if d := add["model"].(schema)["description"].(string); !strings.HasPrefix(d, "Required model name, ") ||
		!strings.HasSuffix(d, "Worker executables are accepted at or above their adapter's minimum version.") {
		t.Fatalf("role_add model description %q", d)
	}
	if d := add["adapter"].(schema)["description"].(string); !strings.Contains(d, "grok (Grok Build; Linux workers only)") ||
		!strings.Contains(d, "cursor (Cursor Agent; registered but its roles are refused on every OS)") {
		t.Fatalf("role_add adapter description %q", d)
	}
	for _, d := range []string{override["properties"].(schema)["model"].(schema)["description"].(string), override["properties"].(schema)["effort"].(schema)["description"].(string)} {
		if !strings.Contains(d, "for this task only (omitted: the role's ") {
			t.Fatalf("override description %q", d)
		}
	}
}
