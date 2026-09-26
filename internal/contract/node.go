package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"
)

// Node wire constants (iteration 03). The HTTP path prefix /api/v1 is an
// API namespace only; the integer ProtocolVersion is the compatibility
// contract and must be equal on both ends.
const (
	// ProtocolHeader carries the integer protocol version on every node
	// HTTP request and response (not on CA bootstrap or health).
	ProtocolHeader = "X-Callsheet-Protocol"

	PathCA         = "/api/v1/ca"
	PathNodes      = "/api/v1/nodes"
	PathEnroll     = "/api/v1/nodes/enroll"
	PathNodeStream = "/api/v1/node-stream"

	// MaxFrameBytes bounds one decoded node-stream message; MaxBodyBytes
	// bounds its body. Both ends set the WebSocket read limit to
	// MaxFrameBytes and enforce both limits on outbound messages.
	MaxFrameBytes = 16 << 10
	MaxBodyBytes  = 8 << 10
	// MaxEnrollRequestBytes bounds POST /api/v1/nodes/enroll.
	MaxEnrollRequestBytes = 4 << 10

	// HeartbeatIntervalMS and LeaseMS are the fixed protocol-1 timing
	// values carried by hello_ok; they are validated, never negotiated.
	HeartbeatIntervalMS = 5000
	LeaseMS             = 15000

	LivenessOnline  = "online"
	LivenessOffline = "offline"
)

// Frame types.
const (
	FrameHello        = "hello"
	FrameHelloOK      = "hello_ok"
	FrameHeartbeat    = "heartbeat"
	FrameHeartbeatAck = "heartbeat_ack"
	FrameError        = "error"
)

// Direction names the sender of a frame.
type Direction int

const (
	FromSidecar Direction = iota
	FromPlane
)

var (
	nodeIDRE    = regexp.MustCompile(`^n_[0-9a-f]{32}$`)
	requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	integerRE   = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})$`)
)

// ValidNodeID reports whether id is a canonical node ID, n_ plus 32
// lowercase hex digits.
func ValidNodeID(id string) bool { return nodeIDRE.MatchString(id) }

// ValidRequestID reports whether id is 1-64 ASCII letters, digits,
// underscores or hyphens.
func ValidRequestID(id string) bool { return requestIDRE.MatchString(id) }

// ValidSoftwareVersion reports whether v is 1-128 bytes in 0x21-0x7e: no
// controls and no whitespace, so it can never inject a row or a field.
func ValidSoftwareVersion(v string) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// ParseInteger parses a strict decimal integer (no sign plus, no leading
// zeros, at most 18 digits).
func ParseInteger(s string) (int, bool) {
	if !integerRE.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return int(n), err == nil
}

// VersionMismatch is the protocol_mismatch error naming both integers:
// local is the reporting side's version, remote its peer's.
func VersionMismatch(local, remote int) *Error {
	return &Error{
		Code:    CodeProtocolMismatch,
		Message: fmt.Sprintf("protocol version mismatch: local=%d remote=%d", local, remote),
		Details: map[string]any{"local_version": local, "remote_version": remote},
	}
}

// RoleStatus is the reserved per-role heartbeat shape. In protocol 1 every
// roles array must be empty; iteration 04 defines its validation.
type RoleStatus struct {
	RoleID      string `json:"role_id"`
	Inflight    int    `json:"inflight"`
	Concurrency int    `json:"concurrency"`
	CanAccept   bool   `json:"can_accept"`
}

// Node is the roster object: exactly these six fields, in this order.
// LastSeen, ProtocolVersion and SoftwareVersion are null until the first
// valid heartbeat of the current plane run.
type Node struct {
	ID              string       `json:"id"`
	Liveness        string       `json:"liveness"`
	LastSeen        *time.Time   `json:"last_seen"`
	ProtocolVersion *int         `json:"protocol_version"`
	SoftwareVersion *string      `json:"software_version"`
	Roles           []RoleStatus `json:"roles"`
}

// MarshalJSON renders the node with UTC RFC3339Nano last_seen and an empty
// (never null) roles array.
func (n Node) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID              string       `json:"id"`
		Liveness        string       `json:"liveness"`
		LastSeen        *string      `json:"last_seen"`
		ProtocolVersion *int         `json:"protocol_version"`
		SoftwareVersion *string      `json:"software_version"`
		Roles           []RoleStatus `json:"roles"`
	}
	w := wire{ID: n.ID, Liveness: n.Liveness, ProtocolVersion: n.ProtocolVersion, SoftwareVersion: n.SoftwareVersion, Roles: n.Roles}
	if n.LastSeen != nil {
		s := n.LastSeen.UTC().Format(time.RFC3339Nano)
		w.LastSeen = &s
	}
	if w.Roles == nil {
		w.Roles = []RoleStatus{}
	}
	return compact(w)
}

// NodeResponse is {"version":1,"node":<Node>}.
type NodeResponse struct {
	Version int  `json:"version"`
	Node    Node `json:"node"`
}

// NodeListResponse is {"version":1,"nodes":[<Node>,...]}.
type NodeListResponse struct {
	Version int    `json:"version"`
	Nodes   []Node `json:"nodes"`
}

// MarshalJSON renders an empty roster as [] rather than null.
func (r NodeListResponse) MarshalJSON() ([]byte, error) {
	type wire struct {
		Version int    `json:"version"`
		Nodes   []Node `json:"nodes"`
	}
	w := wire(r)
	if w.Nodes == nil {
		w.Nodes = []Node{}
	}
	return compact(w)
}

// EnrollRequest is the POST /api/v1/nodes/enroll body.
type EnrollRequest struct {
	NodeID          string `json:"node_id"`
	SoftwareVersion string `json:"software_version"`
}

// Encode renders v as compact JSON without HTML escaping and without a
// trailing newline.
func Encode(v any) ([]byte, error) { return compact(v) }

func compact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// errInvalid returns an invalid_argument error.
func errInvalid(format string, args ...any) *Error {
	return New(CodeInvalidArgument, fmt.Sprintf(format, args...))
}

// object is one strictly decoded JSON object: its raw member values by key
// in document order. Duplicate keys, non-objects and trailing data are
// rejected.
type object struct {
	keys []string
	raw  map[string]json.RawMessage
}

func decodeObject(data []byte, what string) (object, error) {
	o := object{raw: map[string]json.RawMessage{}}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return o, errInvalid("%s is not a JSON object", what)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return o, errInvalid("%s is malformed JSON", what)
		}
		key, ok := tok.(string)
		if !ok {
			return o, errInvalid("%s is malformed JSON", what)
		}
		if _, dup := o.raw[key]; dup {
			return o, errInvalid("%s has a duplicate field %q", what, safeKey(key))
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return o, errInvalid("%s is malformed JSON", what)
		}
		o.raw[key] = v
		o.keys = append(o.keys, key)
	}
	if _, err := dec.Token(); err != nil {
		return o, errInvalid("%s is malformed JSON", what)
	}
	if _, err := dec.Token(); err != io.EOF {
		return o, errInvalid("%s has trailing data after the JSON object", what)
	}
	return o, nil
}

// safeKey bounds and sanitizes a peer-supplied key for a diagnostic.
func safeKey(k string) string {
	if len(k) > 32 {
		k = k[:32]
	}
	b := []byte(k)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// only rejects keys outside allowed and missing or null required keys.
func (o object) only(what string, allowed []string, optional ...string) error {
	known := map[string]bool{}
	for _, k := range allowed {
		known[k] = true
	}
	for _, k := range optional {
		known[k] = true
	}
	for _, k := range o.keys {
		if !known[k] {
			return errInvalid("%s has an unknown field %q", what, safeKey(k))
		}
	}
	for _, k := range allowed {
		v, ok := o.raw[k]
		if !ok {
			return errInvalid("%s lacks the required field %q", what, k)
		}
		if isNull(v) {
			return errInvalid("%s field %q must not be null", what, k)
		}
	}
	return nil
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

func (o object) str(what, key string) (string, error) {
	var s string
	if err := json.Unmarshal(o.raw[key], &s); err != nil {
		return "", errInvalid("%s field %q must be a string", what, key)
	}
	return s, nil
}

func (o object) integer(what, key string) (int, error) {
	n, ok := ParseInteger(string(bytes.TrimSpace(o.raw[key])))
	if !ok {
		return 0, errInvalid("%s field %q must be an integer", what, key)
	}
	return n, nil
}

func (o object) boolean(what, key string) (bool, error) {
	var b bool
	if err := json.Unmarshal(o.raw[key], &b); err != nil {
		return false, errInvalid("%s field %q must be a boolean", what, key)
	}
	return b, nil
}

// array splits a JSON array value into its raw elements.
func array(v json.RawMessage, what string) ([]json.RawMessage, error) {
	if isNull(v) || len(bytes.TrimSpace(v)) == 0 || bytes.TrimSpace(v)[0] != '[' {
		return nil, errInvalid("%s must be an array", what)
	}
	var out []json.RawMessage
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, errInvalid("%s must be an array", what)
	}
	return out, nil
}

// ParseRoles validates a protocol-1 roles array: present, an array, and
// empty. Nonempty arrays are rejected, never silently accepted; element
// shapes are still checked so the error names the actual problem.
func ParseRoles(v json.RawMessage) ([]RoleStatus, error) {
	elems, err := array(v, "roles")
	if err != nil {
		return nil, err
	}
	for _, e := range elems {
		if _, err := parseRole(e); err != nil {
			return nil, err
		}
	}
	if len(elems) != 0 {
		return nil, errInvalid("roles must be empty in protocol %d", ProtocolVersion)
	}
	return []RoleStatus{}, nil
}

func parseRole(v json.RawMessage) (RoleStatus, error) {
	const what = "role status"
	o, err := decodeObject(v, what)
	if err != nil {
		return RoleStatus{}, err
	}
	if err := o.only(what, []string{"role_id", "inflight", "concurrency", "can_accept"}); err != nil {
		return RoleStatus{}, err
	}
	var r RoleStatus
	if r.RoleID, err = o.str(what, "role_id"); err != nil {
		return r, err
	}
	if r.Inflight, err = o.integer(what, "inflight"); err != nil {
		return r, err
	}
	if r.Concurrency, err = o.integer(what, "concurrency"); err != nil {
		return r, err
	}
	if r.CanAccept, err = o.boolean(what, "can_accept"); err != nil {
		return r, err
	}
	return r, nil
}

// ParseTime parses a canonical UTC RFC3339Nano timestamp.
func ParseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Location() != time.UTC || t.Format(time.RFC3339Nano) != s {
		return time.Time{}, false
	}
	return t, true
}

// ParseNode strictly decodes one Node object.
func ParseNode(v json.RawMessage) (Node, error) {
	const what = "node"
	o, err := decodeObject(v, what)
	if err != nil {
		return Node{}, err
	}
	if err := o.only(what, []string{"id", "liveness", "roles"}, "last_seen", "protocol_version", "software_version"); err != nil {
		return Node{}, err
	}
	for _, k := range []string{"last_seen", "protocol_version", "software_version"} {
		if _, ok := o.raw[k]; !ok {
			return Node{}, errInvalid("node lacks the required field %q", k)
		}
	}
	var n Node
	if n.ID, err = o.str(what, "id"); err != nil {
		return n, err
	}
	if !ValidNodeID(n.ID) {
		return n, errInvalid("node id is not a valid node ID")
	}
	if n.Liveness, err = o.str(what, "liveness"); err != nil {
		return n, err
	}
	if n.Liveness != LivenessOnline && n.Liveness != LivenessOffline {
		return n, errInvalid("node liveness must be %q or %q", LivenessOnline, LivenessOffline)
	}
	if !isNull(o.raw["last_seen"]) {
		s, err := o.str(what, "last_seen")
		if err != nil {
			return n, err
		}
		t, ok := ParseTime(s)
		if !ok {
			return n, errInvalid("node last_seen must be a UTC RFC3339 timestamp")
		}
		n.LastSeen = &t
	}
	if !isNull(o.raw["protocol_version"]) {
		pv, err := o.integer(what, "protocol_version")
		if err != nil {
			return n, err
		}
		n.ProtocolVersion = &pv
	}
	if !isNull(o.raw["software_version"]) {
		sv, err := o.str(what, "software_version")
		if err != nil {
			return n, err
		}
		if !ValidSoftwareVersion(sv) {
			return n, errInvalid("node software_version must be 1-128 printable ASCII bytes without spaces")
		}
		n.SoftwareVersion = &sv
	}
	if n.Roles, err = ParseRoles(o.raw["roles"]); err != nil {
		return n, err
	}
	return n, nil
}

// envelopeVersion checks a response envelope's version field.
func envelopeVersion(o object, what string) error {
	v, err := o.integer(what, "version")
	if err != nil {
		return err
	}
	if v != ProtocolVersion {
		return VersionMismatch(ProtocolVersion, v)
	}
	return nil
}

// ParseNodeResponse strictly decodes {"version":1,"node":{...}}.
func ParseNodeResponse(data []byte) (Node, error) {
	const what = "node response"
	o, err := decodeObject(data, what)
	if err != nil {
		return Node{}, err
	}
	if err := o.only(what, []string{"version", "node"}); err != nil {
		return Node{}, err
	}
	if err := envelopeVersion(o, what); err != nil {
		return Node{}, err
	}
	return ParseNode(o.raw["node"])
}

// ParseNodeListResponse strictly decodes {"version":1,"nodes":[...]} and
// requires IDs in strictly ascending order (sorted and unique).
func ParseNodeListResponse(data []byte) ([]Node, error) {
	const what = "node list response"
	o, err := decodeObject(data, what)
	if err != nil {
		return nil, err
	}
	if err := o.only(what, []string{"version", "nodes"}); err != nil {
		return nil, err
	}
	if err := envelopeVersion(o, what); err != nil {
		return nil, err
	}
	elems, err := array(o.raw["nodes"], "nodes")
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(elems))
	for i, e := range elems {
		n, err := ParseNode(e)
		if err != nil {
			return nil, err
		}
		if i > 0 && out[i-1].ID >= n.ID {
			return nil, errInvalid("nodes are not sorted by unique ID")
		}
		out = append(out, n)
	}
	return out, nil
}

// ParseEnrollRequest strictly decodes and validates an enrollment request.
func ParseEnrollRequest(data []byte) (EnrollRequest, error) {
	const what = "enrollment request"
	if len(data) > MaxEnrollRequestBytes {
		return EnrollRequest{}, errInvalid("%s is larger than %d bytes", what, MaxEnrollRequestBytes)
	}
	o, err := decodeObject(data, what)
	if err != nil {
		return EnrollRequest{}, err
	}
	if err := o.only(what, []string{"node_id", "software_version"}); err != nil {
		return EnrollRequest{}, err
	}
	var r EnrollRequest
	if r.NodeID, err = o.str(what, "node_id"); err != nil {
		return r, err
	}
	if r.SoftwareVersion, err = o.str(what, "software_version"); err != nil {
		return r, err
	}
	if !ValidNodeID(r.NodeID) {
		return r, errInvalid("node_id must match n_ followed by 32 lowercase hex digits")
	}
	if !ValidSoftwareVersion(r.SoftwareVersion) {
		return r, errInvalid("software_version must be 1-128 printable ASCII bytes without spaces")
	}
	return r, nil
}

// ParseErrorBody strictly decodes the contract error wire shape
// {"error":{"code":"...","message":"...","details":{...}}}. The code must be
// a known code; the message is bounded and sanitized for display.
func ParseErrorBody(data []byte) (*Error, error) {
	const what = "error body"
	o, err := decodeObject(data, what)
	if err != nil {
		return nil, err
	}
	if err := o.only(what, []string{"error"}); err != nil {
		return nil, err
	}
	in, err := decodeObject(o.raw["error"], what)
	if err != nil {
		return nil, err
	}
	if err := in.only(what, []string{"code", "message"}, "details"); err != nil {
		return nil, err
	}
	code, err := in.str(what, "code")
	if err != nil {
		return nil, err
	}
	if !knownCode(Code(code)) {
		return nil, errInvalid("%s has an unknown error code", what)
	}
	msg, err := in.str(what, "message")
	if err != nil {
		return nil, err
	}
	e := &Error{Code: Code(code), Message: SafeText(msg, 512)}
	if d, ok := in.raw["details"]; ok && !isNull(d) {
		dec := json.NewDecoder(bytes.NewReader(d))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil || m == nil {
			return nil, errInvalid("%s details must be an object", what)
		}
		e.Details = m
	}
	return e, nil
}

func knownCode(c Code) bool {
	switch c {
	case CodeInvalidArgument, CodeNotFound, CodeConflict, CodeUnavailable, CodeTrustFailed,
		CodeProtocolMismatch, CodeNotImplemented, CodeInternal:
		return true
	}
	return false
}

// SafeText bounds s to max bytes and replaces control and non-ASCII bytes,
// so peer-supplied text can be shown on one diagnostic line.
func SafeText(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// DetailInt reads an integer detail (a json.Number, float64 or int).
func (e *Error) DetailInt(key string) (int, bool) {
	switch v := e.Details[key].(type) {
	case int:
		return v, true
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case json.Number:
		return ParseInteger(v.String())
	}
	return 0, false
}

// IsCode reports whether err's first contract error has code c.
func IsCode(err error, c Code) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Code == c
}
