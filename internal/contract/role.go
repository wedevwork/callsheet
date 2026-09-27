package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Role wire and configuration constants (iteration 04).
const (
	// PathRoles is the role collection; one role is PathRoles + "/" + ID.
	PathRoles = "/api/v1/roles"

	// MaxRoles is the registry capacity of this iteration.
	MaxRoles = 100
	// MaxRoleConfigBytes bounds one compact encoded resolved RoleConfig.
	MaxRoleConfigBytes = 8 << 10
	// MaxRoleRequestBytes bounds a role mutation request body.
	MaxRoleRequestBytes = 16 << 10
	// MaxRoleResponseBytes bounds a single-role (or removal) response.
	MaxRoleResponseBytes = 32 << 10
	// MaxRoleListBytes bounds a role list response.
	MaxRoleListBytes = 8 << 20

	// MaxSafeInteger bounds revisions and registration orders (2^53-1).
	MaxSafeInteger = 1<<53 - 1
	// MaxConcurrency bounds a role's concurrency.
	MaxConcurrency = 2147483647
	// DefaultRoleTimeout is the timeout an add request that omits it gets.
	DefaultRoleTimeout = 2 * time.Hour

	// maxRoleText bounds instruction, runbook and model.
	maxRoleText = 1024
)

// Safe reasons carried in role error details.
const (
	ReasonNodeOffline       = "node_offline"
	ReasonNodeDisconnected  = "node_disconnected"
	ReasonBusy              = "busy"
	ReasonValidationTimeout = "validation_timeout"
	ReasonManualUnreadable  = "manual_unreadable"
	ReasonManualNotRegular  = "manual_not_regular"
	ReasonAdapterDisabled   = "adapter_disabled"
	ReasonProbeFailed       = "probe_failed"
)

// ValidSlug reports whether s is a 1-63 byte lowercase ASCII slug, the
// grammar ^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$: letters, digits and
// internal hyphens (role IDs and names, adapter IDs, efforts).
func ValidSlug(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// AdapterInfo is the adapter metadata a role configuration is checked
// against: the case-sensitive allowed efforts and the test-only mark.
type AdapterInfo struct {
	Efforts  []string
	TestOnly bool
}

// AdapterLookup returns the metadata of a registered adapter.
type AdapterLookup func(id string) (AdapterInfo, bool)

// RoleConfig is one role's configuration. Timeout 0 means unlimited;
// HasTimeout is false only for an add request that omitted it. Resolved
// (persisted and distributed) configurations always carry a timeout.
type RoleConfig struct {
	ID          string
	Name        string
	Node        string
	Adapter     string
	Instruction string
	Runbook     string
	Model       string
	Effort      string
	Concurrency int
	Timeout     time.Duration
	HasTimeout  bool
}

// Resolved returns c with the default timeout applied when it was omitted.
func (c RoleConfig) Resolved() RoleConfig {
	if !c.HasTimeout {
		c.Timeout, c.HasTimeout = DefaultRoleTimeout, true
	}
	return c
}

type roleConfigWire struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Node        string  `json:"node"`
	Adapter     string  `json:"adapter"`
	Instruction string  `json:"instruction"`
	Runbook     string  `json:"runbook"`
	Model       string  `json:"model"`
	Effort      string  `json:"effort"`
	Concurrency int     `json:"concurrency"`
	Timeout     *string `json:"timeout,omitempty"`
}

func (c RoleConfig) wire() roleConfigWire {
	w := roleConfigWire{ID: c.ID, Name: c.Name, Node: c.Node, Adapter: c.Adapter, Instruction: c.Instruction,
		Runbook: c.Runbook, Model: c.Model, Effort: c.Effort, Concurrency: c.Concurrency}
	if c.HasTimeout {
		s := c.Timeout.String()
		w.Timeout = &s
	}
	return w
}

// MarshalJSON renders the fields in table order; timeout is omitted only
// when it was not given.
func (c RoleConfig) MarshalJSON() ([]byte, error) { return compact(c.wire()) }

// RoleRecord is a stored and distributed role: the resolved configuration
// plus its immutable registration order.
type RoleRecord struct {
	RoleConfig
	RegistrationOrder int
}

type roleRecordWire struct {
	roleConfigWire
	RegistrationOrder int `json:"registration_order"`
}

// MarshalJSON renders the resolved configuration, then registration_order.
func (r RoleRecord) MarshalJSON() ([]byte, error) {
	return compact(roleRecordWire{roleConfigWire: r.Resolved().wire(), RegistrationOrder: r.RegistrationOrder})
}

// RoleView is the operator view of a role: the record followed by the
// derived observation fields.
type RoleView struct {
	RoleRecord
	Inflight        int
	CanAccept       bool
	NodeLiveness    string
	AdapterTestOnly bool
}

// MarshalJSON renders the flat record fields, then inflight, can_accept,
// node_liveness and adapter_test_only.
func (v RoleView) MarshalJSON() ([]byte, error) {
	type wire struct {
		roleRecordWire
		Inflight        int    `json:"inflight"`
		CanAccept       bool   `json:"can_accept"`
		NodeLiveness    string `json:"node_liveness"`
		AdapterTestOnly bool   `json:"adapter_test_only"`
	}
	return compact(wire{roleRecordWire: roleRecordWire{roleConfigWire: v.Resolved().wire(), RegistrationOrder: v.RegistrationOrder},
		Inflight: v.Inflight, CanAccept: v.CanAccept, NodeLiveness: v.NodeLiveness, AdapterTestOnly: v.AdapterTestOnly})
}

// RoleResponse is {"version":3,"role":RoleView}.
type RoleResponse struct {
	Version int      `json:"version"`
	Role    RoleView `json:"role"`
}

// RoleListResponse is {"version":3,"roles":[RoleView,...]}.
type RoleListResponse struct {
	Version int        `json:"version"`
	Roles   []RoleView `json:"roles"`
}

// MarshalJSON renders an empty list as [].
func (r RoleListResponse) MarshalJSON() ([]byte, error) {
	type wire RoleListResponse
	w := wire(r)
	if w.Roles == nil {
		w.Roles = []RoleView{}
	}
	return compact(w)
}

// RoleRemoveResponse is {"version":3,"removed":ID}.
type RoleRemoveResponse struct {
	Version int    `json:"version"`
	Removed string `json:"removed"`
}

// RoleRemoveRequest is the DELETE body {"force":BOOL}.
type RoleRemoveRequest struct {
	Force bool `json:"force"`
}

// RolePatch holds the mutable fields a set changes; nil fields are
// omitted and keep their values.
type RolePatch struct {
	Name        *string
	Adapter     *string
	Instruction *string
	Runbook     *string
	Model       *string
	Effort      *string
	Concurrency *int
	Timeout     *time.Duration
}

// Empty reports whether p changes nothing.
func (p RolePatch) Empty() bool {
	return p.Name == nil && p.Adapter == nil && p.Instruction == nil && p.Runbook == nil &&
		p.Model == nil && p.Effort == nil && p.Concurrency == nil && p.Timeout == nil
}

// MarshalJSON renders the present fields in table order.
func (p RolePatch) MarshalJSON() ([]byte, error) {
	type wire struct {
		Name        *string `json:"name,omitempty"`
		Adapter     *string `json:"adapter,omitempty"`
		Instruction *string `json:"instruction,omitempty"`
		Runbook     *string `json:"runbook,omitempty"`
		Model       *string `json:"model,omitempty"`
		Effort      *string `json:"effort,omitempty"`
		Concurrency *int    `json:"concurrency,omitempty"`
		Timeout     *string `json:"timeout,omitempty"`
	}
	w := wire{Name: p.Name, Adapter: p.Adapter, Instruction: p.Instruction, Runbook: p.Runbook, Model: p.Model, Effort: p.Effort, Concurrency: p.Concurrency}
	if p.Timeout != nil {
		s := p.Timeout.String()
		w.Timeout = &s
	}
	return compact(w)
}

// Apply returns c with p's fields replaced.
func (p RolePatch) Apply(c RoleConfig) RoleConfig {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&c.Name, p.Name)
	set(&c.Adapter, p.Adapter)
	set(&c.Instruction, p.Instruction)
	set(&c.Runbook, p.Runbook)
	set(&c.Model, p.Model)
	set(&c.Effort, p.Effort)
	if p.Concurrency != nil {
		c.Concurrency = *p.Concurrency
	}
	if p.Timeout != nil {
		c.Timeout, c.HasTimeout = *p.Timeout, true
	}
	return c
}

// roleFields is the table order of RoleConfig's fields.
var roleFields = []string{"id", "name", "node", "adapter", "instruction", "runbook", "model", "effort", "concurrency", "timeout"}

// mutableFields are the fields a set may change, in table order.
var mutableFields = []string{"name", "adapter", "instruction", "runbook", "model", "effort", "concurrency", "timeout"}

// fieldErr is invalid_argument naming the field.
func fieldErr(field, format string, args ...any) *Error {
	e := errInvalid(format, args...)
	e.Details = map[string]any{"field": field}
	return e
}

// validText reports whether s is valid UTF-8 of 1..max bytes without any
// Unicode control character (CR, LF, TAB and DEL included).
func validText(s string, max int) bool {
	if len(s) == 0 || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidRolePath reports whether p is a valid worker-local manual path: an
// absolute POSIX path of valid UTF-8, 1-1024 bytes, without controls.
func ValidRolePath(p string) bool { return validText(p, maxRoleText) && path.IsAbs(p) }

// ValidModel reports whether m is valid free model text: valid UTF-8, 1-1024
// bytes, nonblank, without controls. Bytes are preserved as given.
func ValidModel(m string) bool { return validText(m, maxRoleText) && strings.TrimSpace(m) != "" }

// ParseRoleTimeout parses a role timeout: a Go duration string; zero (for
// example "0") means unlimited; negative, overflowing or blank values are
// invalid.
func ParseRoleTimeout(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 0, fieldErr("timeout", "timeout must be a Go duration such as 2h, 90m or 0 (unlimited)")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fieldErr("timeout", "timeout must be a Go duration such as 2h, 90m or 0 (unlimited)")
	}
	if d < 0 {
		return 0, fieldErr("timeout", "timeout must not be negative")
	}
	return d, nil
}

// checkField validates one configuration field's value.
func checkField(field string, c *RoleConfig) error {
	switch field {
	case "id":
		if !ValidSlug(c.ID) {
			return fieldErr(field, "id must be a 1-63 character slug of lowercase letters, digits and internal hyphens")
		}
	case "name":
		if !ValidSlug(c.Name) {
			return fieldErr(field, "name must be a 1-63 character slug of lowercase letters, digits and internal hyphens")
		}
	case "node":
		if !ValidNodeID(c.Node) {
			return fieldErr(field, "node must be a node ID: n_ followed by 32 lowercase hex digits")
		}
	case "adapter":
		if c.Adapter == "" {
			return fieldErr(field, "adapter must not be empty")
		}
	case "instruction", "runbook":
		p := c.Instruction
		if field == "runbook" {
			p = c.Runbook
		}
		if !ValidRolePath(p) {
			return fieldErr(field, "%s must be an absolute worker-local path of 1-1024 bytes of valid UTF-8 without control characters", field)
		}
	case "model":
		if !ValidModel(c.Model) {
			return fieldErr(field, "model must be nonblank text of 1-1024 bytes of valid UTF-8 without control characters")
		}
	case "effort":
		if c.Effort == "" {
			return fieldErr(field, "effort must not be empty")
		}
	case "concurrency":
		if c.Concurrency < 1 || c.Concurrency > MaxConcurrency {
			return fieldErr(field, "concurrency must be an integer from 1 to %d", MaxConcurrency)
		}
	}
	return nil
}

// checkAdapter validates adapter identity, then effort membership.
func checkAdapter(c RoleConfig, lookup AdapterLookup) error {
	// Submitted values are never echoed: the messages name the field and
	// the allowed values.
	info, ok := lookup(c.Adapter)
	if !ok {
		return fieldErr("adapter", "unknown adapter; registered adapters: fake")
	}
	for _, e := range info.Efforts {
		if e == c.Effort {
			return nil
		}
	}
	return fieldErr("effort", "effort is not allowed for adapter %s; allowed: %s", c.Adapter, strings.Join(info.Efforts, ", "))
}

// ValidateRoleConfig checks c completely in the documented precedence:
// every field's constraint in table order, adapter identity then allowed
// effort (when lookup is non-nil), then the encoded size bound of the
// resolved configuration.
func ValidateRoleConfig(c RoleConfig, lookup AdapterLookup) error {
	for _, f := range roleFields[:9] {
		if err := checkField(f, &c); err != nil {
			return err
		}
	}
	if c.HasTimeout && c.Timeout < 0 {
		return fieldErr("timeout", "timeout must not be negative")
	}
	if lookup != nil {
		if err := checkAdapter(c, lookup); err != nil {
			return err
		}
	}
	b, err := compact(c.Resolved().wire())
	if err != nil || len(b) > MaxRoleConfigBytes {
		return errInvalid("the encoded role configuration exceeds %d bytes", MaxRoleConfigBytes)
	}
	return nil
}

// decodeRoleFields reads the configuration fields of o in table order,
// checking presence, type and constraint of each before the next.
// requireTimeout makes timeout required and canonical (resolved form).
func decodeRoleFields(o object, what string, requireTimeout bool) (RoleConfig, error) {
	var c RoleConfig
	for _, f := range roleFields {
		v, ok := o.raw[f]
		if !ok {
			if f == "timeout" && !requireTimeout {
				continue
			}
			return c, fieldErr(f, "%s lacks the required field %q", what, f)
		}
		if isNull(v) {
			return c, fieldErr(f, "%s field %q must not be null", what, f)
		}
		switch f {
		case "concurrency":
			n, err := o.integer(what, f)
			if err != nil {
				return c, fieldErr(f, "%s field %q must be an integer", what, f)
			}
			c.Concurrency = n
		default:
			s, err := roleString(o, what, f)
			if err != nil {
				return c, err
			}
			switch f {
			case "id":
				c.ID = s
			case "name":
				c.Name = s
			case "node":
				c.Node = s
			case "adapter":
				c.Adapter = s
			case "instruction":
				c.Instruction = s
			case "runbook":
				c.Runbook = s
			case "model":
				c.Model = s
			case "effort":
				c.Effort = s
			case "timeout":
				d, err := ParseRoleTimeout(s)
				if err != nil {
					return c, err
				}
				if requireTimeout && d.String() != s {
					return c, fieldErr(f, "%s timeout %q is not in canonical form (want %s)", what, SafeText(s, 64), d.String())
				}
				c.Timeout, c.HasTimeout = d, true
				continue
			}
		}
		if err := checkField(f, &c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// roleString decodes a string field after checking its raw JSON text is
// valid UTF-8: encoding/json would silently replace invalid bytes.
func roleString(o object, what, field string) (string, error) {
	if !utf8.Valid(o.raw[field]) {
		return "", fieldErr(field, "%s field %q is not valid UTF-8", what, field)
	}
	s, err := o.str(what, field)
	if err != nil {
		return "", fieldErr(field, "%s field %q must be a string", what, field)
	}
	return s, nil
}

// rejectUnknown fails on the first key of o outside allowed; a key in
// readOnly is named as read-only.
func rejectUnknown(o object, what string, allowed []string, readOnly ...string) error {
	known := map[string]bool{}
	for _, k := range allowed {
		known[k] = true
	}
	for _, k := range o.keys {
		if known[k] {
			continue
		}
		for _, r := range readOnly {
			if k == r {
				return fieldErr(k, "%s field %q is read-only and cannot be set", what, k)
			}
		}
		return errInvalid("%s has an unknown field %q", what, safeKey(k))
	}
	return nil
}

// ParseRoleConfig strictly decodes an add request body: one object with
// every field except the optional timeout, no unknown, duplicate or null
// fields, exact types. It validates fields in table order, then adapter
// and effort against lookup (when non-nil), then the encoded size.
func ParseRoleConfig(data []byte, lookup AdapterLookup) (RoleConfig, error) {
	const what = "role"
	o, err := decodeObject(data, what)
	if err != nil {
		return RoleConfig{}, err
	}
	if err := rejectUnknown(o, what, roleFields); err != nil {
		return RoleConfig{}, err
	}
	c, err := decodeRoleFields(o, what, false)
	if err != nil {
		return c, err
	}
	return c, ValidateRoleConfig(c, lookup)
}

// ParseResolvedRoleConfig is ParseRoleConfig for a resolved configuration
// (the role_validate body's role): timeout required and canonical.
func ParseResolvedRoleConfig(data []byte, lookup AdapterLookup) (RoleConfig, error) {
	const what = "role"
	o, err := decodeObject(data, what)
	if err != nil {
		return RoleConfig{}, err
	}
	if err := rejectUnknown(o, what, roleFields); err != nil {
		return RoleConfig{}, err
	}
	c, err := decodeRoleFields(o, what, true)
	if err != nil {
		return c, err
	}
	return c, ValidateRoleConfig(c, lookup)
}

// ParseRolePatch strictly decodes a set request body: a nonempty object of
// mutable fields only. Read-only fields (id, node, registration_order)
// are invalid even with their current values.
func ParseRolePatch(data []byte) (RolePatch, error) {
	const what = "role change"
	var p RolePatch
	o, err := decodeObject(data, what)
	if err != nil {
		return p, err
	}
	if err := rejectUnknown(o, what, mutableFields, "id", "node", "registration_order"); err != nil {
		return p, err
	}
	if len(o.keys) == 0 {
		return p, errInvalid("a role change must set at least one field")
	}
	var c RoleConfig
	for _, f := range mutableFields {
		v, ok := o.raw[f]
		if !ok {
			continue
		}
		if isNull(v) {
			return p, fieldErr(f, "%s field %q must not be null", what, f)
		}
		if f == "concurrency" {
			n, err := o.integer(what, f)
			if err != nil {
				return p, fieldErr(f, "%s field %q must be an integer", what, f)
			}
			c.Concurrency = n
			if err := checkField(f, &c); err != nil {
				return p, err
			}
			p.Concurrency = &n
			continue
		}
		s, err := roleString(o, what, f)
		if err != nil {
			return p, err
		}
		if f == "timeout" {
			d, err := ParseRoleTimeout(s)
			if err != nil {
				return p, err
			}
			p.Timeout = &d
			continue
		}
		v2 := s
		switch f {
		case "name":
			c.Name, p.Name = s, &v2
		case "adapter":
			c.Adapter, p.Adapter = s, &v2
		case "instruction":
			c.Instruction, p.Instruction = s, &v2
		case "runbook":
			c.Runbook, p.Runbook = s, &v2
		case "model":
			c.Model, p.Model = s, &v2
		case "effort":
			c.Effort, p.Effort = s, &v2
		}
		if err := checkField(f, &c); err != nil {
			return p, err
		}
	}
	return p, nil
}

// ValidatePatch checks the constraints of p's present fields.
func ValidatePatch(p RolePatch) error {
	if p.Empty() {
		return errInvalid("a role change must set at least one field")
	}
	c := p.Apply(RoleConfig{})
	present := map[string]bool{"name": p.Name != nil, "adapter": p.Adapter != nil, "instruction": p.Instruction != nil,
		"runbook": p.Runbook != nil, "model": p.Model != nil, "effort": p.Effort != nil, "concurrency": p.Concurrency != nil}
	for _, f := range mutableFields[:7] {
		if present[f] {
			if err := checkField(f, &c); err != nil {
				return err
			}
		}
	}
	if p.Timeout != nil && *p.Timeout < 0 {
		return fieldErr("timeout", "timeout must not be negative")
	}
	return nil
}

// parseRoleRecordObject decodes a stored/distributed record: every
// resolved field plus registration_order 1..MaxSafeInteger.
func parseRoleRecordObject(o object, what string, lookup AdapterLookup, extra ...string) (RoleRecord, error) {
	allowed := append(append([]string{}, roleFields...), "registration_order")
	if err := rejectUnknown(o, what, append(allowed, extra...)); err != nil {
		return RoleRecord{}, err
	}
	c, err := decodeRoleFields(o, what, true)
	if err != nil {
		return RoleRecord{}, err
	}
	if err := ValidateRoleConfig(c, lookup); err != nil {
		return RoleRecord{}, err
	}
	v, ok := o.raw["registration_order"]
	if !ok || isNull(v) {
		return RoleRecord{}, fieldErr("registration_order", "%s lacks the required field %q", what, "registration_order")
	}
	n, err := o.integer(what, "registration_order")
	if err != nil || n < 1 || n > MaxSafeInteger {
		return RoleRecord{}, fieldErr("registration_order", "%s registration_order must be an integer from 1 to %d", what, MaxSafeInteger)
	}
	return RoleRecord{RoleConfig: c, RegistrationOrder: n}, nil
}

// ParseRoleRecord strictly decodes one stored or distributed role record.
func ParseRoleRecord(v json.RawMessage, lookup AdapterLookup) (RoleRecord, error) {
	const what = "role record"
	o, err := decodeObject(v, what)
	if err != nil {
		return RoleRecord{}, err
	}
	return parseRoleRecordObject(o, what, lookup)
}

// ParseRoleView strictly decodes one RoleView: the record, then inflight
// (the plane's held reservations, 0..MaxSafeInteger, which may exceed a
// lowered concurrency; iteration 05), can_accept (false while inflight
// reaches the concurrency), node_liveness and adapter_test_only, which
// must match the adapter's metadata.
func ParseRoleView(v json.RawMessage, lookup AdapterLookup) (RoleView, error) {
	const what = "role view"
	o, err := decodeObject(v, what)
	if err != nil {
		return RoleView{}, err
	}
	obs := []string{"inflight", "can_accept", "node_liveness", "adapter_test_only"}
	rec, err := parseRoleRecordObject(o, what, lookup, obs...)
	if err != nil {
		return RoleView{}, err
	}
	for _, k := range obs {
		if raw, ok := o.raw[k]; !ok || isNull(raw) {
			return RoleView{}, errInvalid("%s lacks the required field %q", what, k)
		}
	}
	rv := RoleView{RoleRecord: rec}
	if rv.Inflight, err = o.integer(what, "inflight"); err != nil {
		return rv, err
	}
	if rv.Inflight < 0 || rv.Inflight > MaxSafeInteger {
		return rv, errInvalid("%s inflight must be an integer from 0 to %d", what, MaxSafeInteger)
	}
	if rv.CanAccept, err = o.boolean(what, "can_accept"); err != nil {
		return rv, err
	}
	if rv.CanAccept && rv.Inflight >= rv.Concurrency {
		return rv, errInvalid("%s can_accept must be false while inflight reaches the concurrency", what)
	}
	if rv.NodeLiveness, err = o.str(what, "node_liveness"); err != nil {
		return rv, err
	}
	if rv.NodeLiveness != LivenessOnline && rv.NodeLiveness != LivenessOffline {
		return rv, errInvalid("%s node_liveness must be %q or %q", what, LivenessOnline, LivenessOffline)
	}
	if rv.AdapterTestOnly, err = o.boolean(what, "adapter_test_only"); err != nil {
		return rv, err
	}
	if lookup != nil {
		if info, _ := lookup(rv.Adapter); info.TestOnly != rv.AdapterTestOnly {
			return rv, errInvalid("%s adapter_test_only does not match adapter %s", what, rv.Adapter)
		}
	}
	return rv, nil
}

// ParseRoleResponse strictly decodes {"version":3,"role":RoleView}.
func ParseRoleResponse(data []byte, lookup AdapterLookup) (RoleView, error) {
	const what = "role response"
	o, err := decodeObject(data, what)
	if err != nil {
		return RoleView{}, err
	}
	if err := o.only(what, []string{"version", "role"}); err != nil {
		return RoleView{}, err
	}
	if err := envelopeVersion(o, what); err != nil {
		return RoleView{}, err
	}
	return ParseRoleView(o.raw["role"], lookup)
}

// RoleListLess is the list order: bytewise name, then registration
// order, then ID as a defensive tie-break.
func RoleListLess(a, b RoleRecord) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.RegistrationOrder != b.RegistrationOrder {
		return a.RegistrationOrder < b.RegistrationOrder
	}
	return a.ID < b.ID
}

// ParseRoleListResponse strictly decodes {"version":3,"roles":[...]} and
// requires at most MaxRoles views in list order with unique IDs and
// registration orders.
func ParseRoleListResponse(data []byte, lookup AdapterLookup) ([]RoleView, error) {
	const what = "role list response"
	o, err := decodeObject(data, what)
	if err != nil {
		return nil, err
	}
	if err := o.only(what, []string{"version", "roles"}); err != nil {
		return nil, err
	}
	if err := envelopeVersion(o, what); err != nil {
		return nil, err
	}
	elems, err := array(o.raw["roles"], "roles")
	if err != nil {
		return nil, err
	}
	if len(elems) > MaxRoles {
		return nil, errInvalid("the role list has %d roles; at most %d are allowed", len(elems), MaxRoles)
	}
	out := make([]RoleView, 0, len(elems))
	ids, orders := map[string]bool{}, map[int]bool{}
	for i, e := range elems {
		v, err := ParseRoleView(e, lookup)
		if err != nil {
			return nil, err
		}
		if ids[v.ID] || orders[v.RegistrationOrder] {
			return nil, errInvalid("the role list repeats an ID or registration order")
		}
		ids[v.ID], orders[v.RegistrationOrder] = true, true
		if i > 0 && !RoleListLess(out[i-1].RoleRecord, v.RoleRecord) {
			return nil, errInvalid("the role list is not sorted by name, then registration order")
		}
		out = append(out, v)
	}
	return out, nil
}

// ParseRoleRemoveResponse strictly decodes {"version":3,"removed":ID}.
func ParseRoleRemoveResponse(data []byte) (string, error) {
	const what = "role removal response"
	o, err := decodeObject(data, what)
	if err != nil {
		return "", err
	}
	if err := o.only(what, []string{"version", "removed"}); err != nil {
		return "", err
	}
	if err := envelopeVersion(o, what); err != nil {
		return "", err
	}
	id, err := o.str(what, "removed")
	if err != nil {
		return "", err
	}
	if !ValidSlug(id) {
		return "", errInvalid("%s removed is not a valid role ID", what)
	}
	return id, nil
}

// ParseRoleRemoveRequest strictly decodes the DELETE body {"force":BOOL}.
func ParseRoleRemoveRequest(data []byte) (bool, error) {
	const what = "role removal request"
	o, err := decodeObject(data, what)
	if err != nil {
		return false, err
	}
	if err := o.only(what, []string{"force"}); err != nil {
		return false, err
	}
	return o.boolean(what, "force")
}

// ParseRoleStatus strictly decodes one heartbeat/node role status: a slug
// role ID, inflight 0..MaxConcurrency (iteration 05: not bounded by the
// concurrency, which a set may lower below it), concurrency
// 1..MaxConcurrency and a boolean readiness that is false while inflight
// reaches the concurrency.
func ParseRoleStatus(v json.RawMessage) (RoleStatus, error) {
	r, err := parseRole(v)
	if err != nil {
		return r, err
	}
	switch {
	case !ValidSlug(r.RoleID):
		return r, errInvalid("role status role_id must be a role ID slug")
	case r.Inflight < 0 || r.Inflight > MaxConcurrency:
		return r, errInvalid("role status inflight must be 0 to %d", MaxConcurrency)
	case r.Concurrency < 1 || r.Concurrency > MaxConcurrency:
		return r, errInvalid("role status concurrency must be 1 to %d", MaxConcurrency)
	case r.CanAccept && r.Inflight >= r.Concurrency:
		return r, errInvalid("role status can_accept must be false while inflight reaches the concurrency")
	}
	return r, nil
}

// EncodeRoleConfigSize is the compact encoded size of c's resolved form.
func EncodeRoleConfigSize(c RoleConfig) int {
	b, err := compact(c.Resolved().wire())
	if err != nil {
		return MaxRoleConfigBytes + 1
	}
	return len(b)
}

// SameRoleConfig reports whether a and b resolve to the same configuration.
func SameRoleConfig(a, b RoleConfig) bool {
	x, err1 := compact(a.Resolved().wire())
	y, err2 := compact(b.Resolved().wire())
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// roleErrorf is a role error with safe details.
func roleErrorf(code Code, details map[string]any, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Details: details}
}

// RoleError returns a contract error of code with the given safe details
// (role_id, node_id, field, reason; empty values are omitted).
func RoleError(code Code, roleID, nodeID, field, reason, format string, args ...any) *Error {
	d := map[string]any{}
	for k, v := range map[string]string{"role_id": roleID, "node_id": nodeID, "field": field, "reason": reason} {
		if v != "" {
			d[k] = v
		}
	}
	if len(d) == 0 {
		d = nil
	}
	return roleErrorf(code, d, format, args...)
}
