package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Development full-flow selection (design 12a-dev-flow-harness, Selection
// manifest). Dispatch order is independent of this canonical row order.
// An explicit flow may change only model and effort.

const DevFullFlowSchema = "callsheet-dev-full-flow/v1"

// DevFullRoleIDs are the five roles in canonical serialization order.
var DevFullRoleIDs = []string{HopDesigner, HopDesignReviewer, HopCoder, HopCodeReviewer, "triage"}

// devFullAdapters are each role's fixed adapter (design 12a-codex-triage:
// triage runs on Codex, so an old Grok-triage flow is flow_invalid).
var devFullAdapters = []string{adapter.CodexID, adapter.ClaudeID, adapter.ClaudeID, adapter.CodexID, adapter.CodexID}

// DevFullFlow is the development selection manifest.
type DevFullFlow struct {
	Schema string     `json:"schema"`
	Roles  []FlowRole `json:"roles"`
}

// DefaultDevFullFlow is the owner's default five-role table.
func DefaultDevFullFlow() DevFullFlow {
	return DevFullFlow{Schema: DevFullFlowSchema, Roles: []FlowRole{
		{ID: HopDesigner, Adapter: adapter.CodexID, Model: "gpt-6-astra", Effort: "low"},
		{ID: HopDesignReviewer, Adapter: adapter.ClaudeID, Model: "claude-fable-5-1", Effort: "low"},
		{ID: HopCoder, Adapter: adapter.ClaudeID, Model: "claude-opus-5-5", Effort: "high"},
		{ID: HopCodeReviewer, Adapter: adapter.CodexID, Model: "gpt-6.1-sol", Effort: "high"},
		{ID: "triage", Adapter: adapter.CodexID, Model: "gpt-6.1-sol", Effort: "high"},
	}}
}

// ParseDevFullFlow strictly decodes a development flow. Unknown keys,
// duplicate keys, the wrong schema, and a selection Validate rejects all
// fail. Adapters and role order are fixed; model and effort go through
// adapter.ValidateSelection.
func ParseDevFullFlow(data []byte) (DevFullFlow, error) {
	if len(data) > maxFlowBytes {
		return DevFullFlow{}, errors.New("flow: the file exceeds 16 KiB")
	}
	var f DevFullFlow
	if err := decodeStrict(data, &f); err != nil {
		return DevFullFlow{}, fmt.Errorf("flow: %w", err)
	}
	if err := f.Validate(); err != nil {
		return DevFullFlow{}, err
	}
	return f, nil
}

// Validate checks the schema, the canonical role order, the fixed
// adapters, and each model/effort pair.
func (f DevFullFlow) Validate() error {
	if f.Schema != DevFullFlowSchema {
		return fmt.Errorf("flow: unknown schema %q", contract.SafeText(f.Schema, 64))
	}
	if len(f.Roles) != len(DevFullRoleIDs) {
		return fmt.Errorf("flow: want exactly %d roles, got %d", len(DevFullRoleIDs), len(f.Roles))
	}
	seen := map[string]bool{}
	for i, r := range f.Roles {
		switch {
		case r.ID != DevFullRoleIDs[i]:
			return fmt.Errorf("flow: role %d must be %s (roles and order are fixed)", i+1, DevFullRoleIDs[i])
		case seen[r.ID]:
			return fmt.Errorf("flow: duplicate role %s", r.ID)
		case r.Adapter != devFullAdapters[i]:
			return fmt.Errorf("flow: role %s must use adapter %s", r.ID, devFullAdapters[i])
		}
		seen[r.ID] = true
		if err := adapter.ValidateSelection(r.Adapter, r.Model, r.Effort); err != nil {
			return fmt.Errorf("flow: role %s: %v", r.ID, err)
		}
	}
	return nil
}

// Kind is FlowDefault when f equals the owner's table, else FlowOverride.
func (f DevFullFlow) Kind() string {
	d := DefaultDevFullFlow()
	if f.Schema != d.Schema || len(f.Roles) != len(d.Roles) {
		return FlowOverride
	}
	for i := range d.Roles {
		if f.Roles[i] != d.Roles[i] {
			return FlowOverride
		}
	}
	return FlowDefault
}

// Role returns the row named id.
func (f DevFullFlow) Role(id string) (FlowRole, bool) {
	for _, r := range f.Roles {
		if r.ID == id {
			return r, true
		}
	}
	return FlowRole{}, false
}

// SelectionCanonical is the canonical JSON of the validated selection,
// independent of insignificant source whitespace.
func (f DevFullFlow) SelectionCanonical() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// CheckDevPosture applies adapter.ValidateWorkerPosture to every selected
// role. The shipped selection (Codex and Claude only) is eligible on Linux
// and Darwin; that never bypasses the live Linux-only Gate, which the
// harness's required vendor installations and sidecars keep.
func (f DevFullFlow) CheckDevPosture(goos string) error {
	if err := f.Validate(); err != nil {
		return err
	}
	for _, r := range f.Roles {
		if err := adapter.ValidateWorkerPosture(r.Adapter, goos); err != nil {
			return fmt.Errorf("flow: role %s: %w", r.ID, err)
		}
	}
	return nil
}
