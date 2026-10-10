package reale2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The four feature hops, in order. Each hop's role ID is its hop name.
const (
	HopDesigner       = "designer"
	HopDesignReviewer = "design-reviewer"
	HopCoder          = "coder"
	HopCodeReviewer   = "code-reviewer"
)

// Hops are the hop names in chain order.
var Hops = []string{HopDesigner, HopDesignReviewer, HopCoder, HopCodeReviewer}

// hopIndex returns the 0-based position of hop, or -1.
func hopIndex(hop string) int {
	for i, h := range Hops {
		if h == hop {
			return i
		}
	}
	return -1
}

// hopDir is the evidence directory number of the 0-based hop i ("01".."04").
func hopDir(i int) string { return fmt.Sprintf("%02d", i+1) }

// Fixed role policy (design 12a-real-e2e, Deployment and roles): every
// role has concurrency 1 and a 10m timeout; adapters and order are fixed
// in this slice.
const (
	RoleConcurrency = 1
	RoleTimeout     = 10 * time.Minute
)

// fixedAdapters are each hop's adapter, never changed by a flow.
var fixedAdapters = []string{adapter.CodexID, adapter.ClaudeID, adapter.ClaudeID, adapter.GrokID}

// FlowSchema is the flow configuration's schema identifier.
const FlowSchema = "callsheet-real-e2e-flow/v1"

// Flow kinds as the report labels them.
const (
	FlowDefault  = "default-flow"
	FlowOverride = "override-flow"
)

// FlowRole is one role of the flow: its ID (the hop), adapter and frozen
// model/effort selection.
type FlowRole struct {
	ID      string `json:"id"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
}

// Flow is the shipped JSON configuration (docs/examples/real-e2e/flow.json)
// or an explicit local copy of it.
type Flow struct {
	Schema string     `json:"schema"`
	Roles  []FlowRole `json:"roles"`
}

// DefaultFlow returns the owner's default role table.
func DefaultFlow() Flow {
	return Flow{Schema: FlowSchema, Roles: []FlowRole{
		{ID: HopDesigner, Adapter: adapter.CodexID, Model: "gpt-6-astra", Effort: "low"},
		{ID: HopDesignReviewer, Adapter: adapter.ClaudeID, Model: "claude-fable-5-1", Effort: "low"},
		{ID: HopCoder, Adapter: adapter.ClaudeID, Model: "claude-opus-5-5", Effort: "high"},
		{ID: HopCodeReviewer, Adapter: adapter.GrokID, Model: "grok-4.7", Effort: "high"},
	}}
}

// maxFlowBytes bounds a flow file.
const maxFlowBytes = 16 << 10

// ParseFlow strictly decodes and validates a flow document: the schema,
// exactly the four roles in hop order with their fixed adapters, and each
// model/effort pair through the existing contract and adapter validators
// (never a second allowlist).
func ParseFlow(data []byte) (Flow, error) {
	if len(data) > maxFlowBytes {
		return Flow{}, errors.New("flow: the file exceeds 16 KiB")
	}
	var f Flow
	if err := decodeStrict(data, &f); err != nil {
		return Flow{}, fmt.Errorf("flow: %w", err)
	}
	return f, f.Validate()
}

// Validate checks f's schema, role order, fixed adapters and selections.
func (f Flow) Validate() error {
	if f.Schema != FlowSchema {
		return fmt.Errorf("flow: unknown schema %q", contract.SafeText(f.Schema, 64))
	}
	if len(f.Roles) != len(Hops) {
		return fmt.Errorf("flow: want exactly %d roles, got %d", len(Hops), len(f.Roles))
	}
	for i, r := range f.Roles {
		switch {
		case r.ID != Hops[i]:
			return fmt.Errorf("flow: role %d must be %s (roles and order are fixed)", i+1, Hops[i])
		case r.Adapter != fixedAdapters[i]:
			return fmt.Errorf("flow: role %s must use adapter %s (adapters are fixed in this slice)", r.ID, fixedAdapters[i])
		}
		if err := adapter.ValidateSelection(r.Adapter, r.Model, r.Effort); err != nil {
			return fmt.Errorf("flow: role %s: %v", r.ID, err)
		}
	}
	return nil
}

// Kind is FlowDefault when f equals the owner's default table, else
// FlowOverride.
func (f Flow) Kind() string {
	d := DefaultFlow()
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

// Role returns the flow role of hop.
func (f Flow) Role(hop string) (FlowRole, bool) {
	for _, r := range f.Roles {
		if r.ID == hop {
			return r, true
		}
	}
	return FlowRole{}, false
}

// sha256Hex is the lowercase hex SHA-256 of b.
func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
