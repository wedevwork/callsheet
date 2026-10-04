package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// NodeFrame is one node-stream text message: exactly one JSON object with
// the required fields version, type, request_id and body, in that order.
type NodeFrame struct {
	Version   int             `json:"version"`
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Body      json.RawMessage `json:"body"`
}

// HelloBody is the sidecar's first message body.
type HelloBody struct {
	NodeID          string `json:"node_id"`
	SoftwareVersion string `json:"software_version"`
}

// HelloOKBody carries the fixed timing values.
type HelloOKBody struct {
	HeartbeatIntervalMS int `json:"heartbeat_interval_ms"`
	LeaseMS             int `json:"lease_ms"`
}

// HeartbeatBody carries the installed role snapshot's revision and one
// status per installed role, in registration order (protocol 2).
type HeartbeatBody struct {
	RolesRevision int          `json:"roles_revision"`
	Roles         []RoleStatus `json:"roles"`
}

// RoleValidateBody is the plane's role_validate request body.
type RoleValidateBody struct {
	Role RoleConfig `json:"role"`
}

// RoleValidateResult is the sidecar's role_validate_result body: exactly
// {"ok":true}, or {"ok":false,"error":{...}} with a safe contract error
// whose code is invalid_argument, unavailable or internal.
type RoleValidateResult struct {
	Err *Error
}

// MarshalJSON renders the exact success or failure shape.
func (r RoleValidateResult) MarshalJSON() ([]byte, error) {
	if r.Err == nil {
		return []byte(`{"ok":true}`), nil
	}
	e, err := r.Err.MarshalJSON()
	if err != nil {
		return nil, err
	}
	// e is {"error":{...}}: splice ok in front.
	return append([]byte(`{"ok":false,`), e[1:]...), nil
}

// RolesReplaceBody is the plane's full per-node role snapshot.
type RolesReplaceBody struct {
	Revision int          `json:"revision"`
	Roles    []RoleRecord `json:"roles"`
}

// MarshalJSON renders an empty snapshot as [].
func (b RolesReplaceBody) MarshalJSON() ([]byte, error) {
	type wire RolesReplaceBody
	w := wire(b)
	if w.Roles == nil {
		w.Roles = []RoleRecord{}
	}
	return compact(w)
}

// RolesReplaceAckBody acknowledges installation of a snapshot revision.
type RolesReplaceAckBody struct {
	Revision int `json:"revision"`
}

// EncodeFrame renders a frame with body b (any JSON-encodable value, or
// a *Error for the error type) and enforces the type's body limit and the
// message limit. Version is normally ProtocolVersion.
func EncodeFrame(version int, typ, requestID string, b any) ([]byte, error) {
	var body []byte
	var err error
	switch v := b.(type) {
	case *Error:
		body, err = v.MarshalJSON()
	case TaskStartResult:
		body, err = v.MarshalJSON()
	case HeartbeatBody:
		if v.Roles == nil {
			v.Roles = []RoleStatus{}
		}
		body, err = compact(v)
	case nil:
		body = []byte("{}")
	default:
		body, err = compact(v)
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s body: %w", typ, err)
	}
	if limit := min(BodyLimit(typ), MaxBodyBytes); len(body) > limit {
		return nil, fmt.Errorf("outbound %s body of %d bytes exceeds %d", typ, len(body), limit)
	}
	out, err := compact(NodeFrame{Version: version, Type: typ, RequestID: requestID, Body: body})
	if err != nil {
		return nil, fmt.Errorf("encode %s frame: %w", typ, err)
	}
	if len(out) > MaxFrameBytes {
		return nil, fmt.Errorf("outbound %s message of %d bytes exceeds %d", typ, len(out), MaxFrameBytes)
	}
	return out, nil
}

var frameTypes = map[Direction]map[string]bool{
	FromSidecar: {FrameHello: true, FrameHeartbeat: true, FrameError: true, FrameRoleValidateResult: true, FrameRolesReplaceAck: true,
		FrameTaskStartResult: true, FrameTaskLog: true, FrameTaskResult: true, FrameTaskInventory: true, FrameTaskReconcileAck: true,
		FrameTaskCancelAck: true, FrameTaskPrepared: true},
	FromPlane: {FrameHelloOK: true, FrameHeartbeatAck: true, FrameError: true, FrameRoleValidate: true, FrameRolesReplace: true,
		FrameTaskStart: true, FrameTaskLogAck: true, FrameTaskResultAck: true, FrameTaskInventoryAck: true, FrameTaskReconcile: true,
		FrameTaskCancel: true, FrameTaskPreparedAck: true},
}

// DecodeFrame decodes one message sent by from. It reads the bounded
// envelope first: a valid integer version that differs from
// ProtocolVersion is rejected as protocol_mismatch (local=ProtocolVersion,
// remote=the frame's version) before anything else is validated or the
// body is decoded; the returned frame then carries that version and, if
// valid, the request ID. Every other defect is invalid_argument. The body
// is returned raw (at most the type's BodyLimit) for the type's decoder.
func DecodeFrame(data []byte, from Direction) (NodeFrame, error) {
	const what = "message"
	var f NodeFrame
	if len(data) > MaxFrameBytes {
		return f, errInvalid("message of %d bytes exceeds %d", len(data), MaxFrameBytes)
	}
	o, err := decodeObject(data, what)
	if err != nil {
		return f, err
	}
	v, ok := o.raw["version"]
	if !ok || isNull(v) {
		return f, errInvalid("message lacks the required field \"version\"")
	}
	if f.Version, err = o.integer(what, "version"); err != nil {
		return f, err
	}
	if id, ok := o.raw["request_id"]; ok {
		var s string
		if json.Unmarshal(id, &s) == nil && ValidRequestID(s) {
			f.RequestID = s
		}
	}
	if f.Version != ProtocolVersion {
		return f, VersionMismatch(ProtocolVersion, f.Version)
	}
	if err := o.only(what, []string{"version", "type", "request_id", "body"}); err != nil {
		return f, err
	}
	if f.Type, err = o.str(what, "type"); err != nil {
		return f, err
	}
	rid, err := o.str(what, "request_id")
	if err != nil {
		return f, err
	}
	if !ValidRequestID(rid) {
		return f, errInvalid("message request_id must be 1-64 ASCII letters, digits, underscores or hyphens")
	}
	if !frameTypes[from][f.Type] {
		if frameTypes[FromSidecar][f.Type] || frameTypes[FromPlane][f.Type] {
			return f, errInvalid("message type %q is not valid in this direction", f.Type)
		}
		return f, errInvalid("unexpected message type %q", safeKey(f.Type))
	}
	body := bytes.TrimSpace(o.raw["body"])
	if limit := min(BodyLimit(f.Type), MaxBodyBytes); len(body) > limit {
		return f, errInvalid("%s message body of %d bytes exceeds %d", f.Type, len(body), limit)
	}
	if len(body) == 0 || body[0] != '{' {
		return f, errInvalid("message body must be a JSON object")
	}
	f.Body = body
	return f, nil
}

// DecodeHello decodes a hello body.
func DecodeHello(body json.RawMessage) (HelloBody, error) {
	const what = "hello body"
	o, err := decodeObject(body, what)
	if err != nil {
		return HelloBody{}, err
	}
	if err := o.only(what, []string{"node_id", "software_version"}); err != nil {
		return HelloBody{}, err
	}
	var h HelloBody
	if h.NodeID, err = o.str(what, "node_id"); err != nil {
		return h, err
	}
	if h.SoftwareVersion, err = o.str(what, "software_version"); err != nil {
		return h, err
	}
	if !ValidNodeID(h.NodeID) {
		return h, errInvalid("hello node_id must match n_ followed by 32 lowercase hex digits")
	}
	if !ValidSoftwareVersion(h.SoftwareVersion) {
		return h, errInvalid("hello software_version must be 1-128 printable ASCII bytes without spaces")
	}
	return h, nil
}

// DecodeHelloOK decodes a hello_ok body and requires exactly the fixed
// values heartbeat_interval_ms=5000 and lease_ms=15000.
func DecodeHelloOK(body json.RawMessage) (HelloOKBody, error) {
	const what = "hello_ok body"
	o, err := decodeObject(body, what)
	if err != nil {
		return HelloOKBody{}, err
	}
	if err := o.only(what, []string{"heartbeat_interval_ms", "lease_ms"}); err != nil {
		return HelloOKBody{}, err
	}
	var h HelloOKBody
	if h.HeartbeatIntervalMS, err = o.integer(what, "heartbeat_interval_ms"); err != nil {
		return h, err
	}
	if h.LeaseMS, err = o.integer(what, "lease_ms"); err != nil {
		return h, err
	}
	if h.HeartbeatIntervalMS != HeartbeatIntervalMS || h.LeaseMS != LeaseMS {
		return h, errInvalid("hello_ok must carry heartbeat_interval_ms=%d and lease_ms=%d in protocol %d", HeartbeatIntervalMS, LeaseMS, ProtocolVersion)
	}
	return h, nil
}

// revision reads a snapshot revision: an exact integer 0..MaxSafeInteger.
func (o object) revision(what, key string) (int, error) {
	n, err := o.integer(what, key)
	if err != nil || n < 0 || n > MaxSafeInteger {
		return 0, errInvalid("%s field %q must be an integer from 0 to %d", what, key, MaxSafeInteger)
	}
	return n, nil
}

// DecodeHeartbeat decodes a heartbeat body: the installed snapshot's
// revision and a valid status array (ParseRoles).
func DecodeHeartbeat(body json.RawMessage) (HeartbeatBody, error) {
	const what = "heartbeat body"
	o, err := decodeObject(body, what)
	if err != nil {
		return HeartbeatBody{}, err
	}
	if err := o.only(what, []string{"roles_revision", "roles"}); err != nil {
		return HeartbeatBody{}, err
	}
	var hb HeartbeatBody
	if hb.RolesRevision, err = o.revision(what, "roles_revision"); err != nil {
		return hb, err
	}
	if hb.Roles, err = ParseRoles(o.raw["roles"]); err != nil {
		return hb, err
	}
	return hb, nil
}

// DecodeAck decodes a heartbeat_ack body, which must be {}.
func DecodeAck(body json.RawMessage) error {
	const what = "heartbeat_ack body"
	o, err := decodeObject(body, what)
	if err != nil {
		return err
	}
	return o.only(what, nil)
}

// DecodeRoleValidate decodes the role_validate wrapper {"role":{...}} and
// returns the role object raw, for ParseResolvedRoleConfig: a malformed
// wrapper is a protocol error, an invalid configuration a validation
// result.
func DecodeRoleValidate(body json.RawMessage) (json.RawMessage, error) {
	const what = "role_validate body"
	o, err := decodeObject(body, what)
	if err != nil {
		return nil, err
	}
	if err := o.only(what, []string{"role"}); err != nil {
		return nil, err
	}
	r := bytes.TrimSpace(o.raw["role"])
	if len(r) == 0 || r[0] != '{' {
		return nil, errInvalid("%s role must be an object", what)
	}
	return r, nil
}

// validationResultCode reports whether c may appear in a validation result.
func validationResultCode(c Code) bool {
	return c == CodeInvalidArgument || c == CodeUnavailable || c == CodeInternal
}

// DecodeRoleValidateResult decodes a role_validate_result body: exactly
// {"ok":true} (nil, nil) or {"ok":false,"error":{...}} (the error, nil)
// whose code is invalid_argument, unavailable or internal.
func DecodeRoleValidateResult(body json.RawMessage) (*Error, error) {
	const what = "role_validate_result body"
	o, err := decodeObject(body, what)
	if err != nil {
		return nil, err
	}
	raw, ok := o.raw["ok"]
	if !ok || isNull(raw) {
		return nil, errInvalid("%s lacks the required field %q", what, "ok")
	}
	okv, err := o.boolean(what, "ok")
	if err != nil {
		return nil, err
	}
	if okv {
		if err := o.only(what, []string{"ok"}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := o.only(what, []string{"ok", "error"}); err != nil {
		return nil, err
	}
	e, err := ParseErrorBody(append(append([]byte(`{"error":`), o.raw["error"]...), '}'))
	if err != nil {
		return nil, err
	}
	if !validationResultCode(e.Code) {
		return nil, errInvalid("%s error code %s is not allowed", what, e.Code)
	}
	return e, nil
}

// DecodeRolesReplace decodes a roles_replace body: a revision 0..MaxSafe
// and at most MaxRoles valid resolved records (ParseRoleRecord against
// lookup) with unique IDs and strictly increasing registration orders.
func DecodeRolesReplace(body json.RawMessage, lookup AdapterLookup) (RolesReplaceBody, error) {
	const what = "roles_replace body"
	o, err := decodeObject(body, what)
	if err != nil {
		return RolesReplaceBody{}, err
	}
	if err := o.only(what, []string{"revision", "roles"}); err != nil {
		return RolesReplaceBody{}, err
	}
	var b RolesReplaceBody
	if b.Revision, err = o.revision(what, "revision"); err != nil {
		return b, err
	}
	elems, err := array(o.raw["roles"], "roles")
	if err != nil {
		return b, err
	}
	if len(elems) > MaxRoles {
		return b, errInvalid("%s has %d roles; at most %d are allowed", what, len(elems), MaxRoles)
	}
	b.Roles = make([]RoleRecord, 0, len(elems))
	ids := map[string]bool{}
	for i, e := range elems {
		r, err := ParseRoleRecord(e, lookup)
		if err != nil {
			return b, err
		}
		if ids[r.ID] {
			return b, errInvalid("%s repeats role %s", what, r.ID)
		}
		ids[r.ID] = true
		if i > 0 && b.Roles[i-1].RegistrationOrder >= r.RegistrationOrder {
			return b, errInvalid("%s roles are not in strictly increasing registration order", what)
		}
		b.Roles = append(b.Roles, r)
	}
	return b, nil
}

// DecodeRolesReplaceAck decodes a roles_replace_ack body {"revision":N}.
func DecodeRolesReplaceAck(body json.RawMessage) (int, error) {
	const what = "roles_replace_ack body"
	o, err := decodeObject(body, what)
	if err != nil {
		return 0, err
	}
	if err := o.only(what, []string{"revision"}); err != nil {
		return 0, err
	}
	return o.revision(what, "revision")
}
