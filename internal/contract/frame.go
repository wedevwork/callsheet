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

// HelloOKBody carries the fixed protocol-1 timing values.
type HelloOKBody struct {
	HeartbeatIntervalMS int `json:"heartbeat_interval_ms"`
	LeaseMS             int `json:"lease_ms"`
}

// HeartbeatBody carries the (in protocol 1 always empty) role statuses.
type HeartbeatBody struct {
	Roles []RoleStatus `json:"roles"`
}

// EncodeFrame renders a frame with body b (any JSON-encodable value, or
// a *Error for the error type) and enforces the outbound body and message
// limits. Version is normally ProtocolVersion.
func EncodeFrame(version int, typ, requestID string, b any) ([]byte, error) {
	var body []byte
	var err error
	switch v := b.(type) {
	case *Error:
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
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("outbound %s body of %d bytes exceeds %d", typ, len(body), MaxBodyBytes)
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
	FromSidecar: {FrameHello: true, FrameHeartbeat: true, FrameError: true},
	FromPlane:   {FrameHelloOK: true, FrameHeartbeatAck: true, FrameError: true},
}

// DecodeFrame decodes one message sent by from. It reads the bounded
// envelope first: a valid integer version that differs from
// ProtocolVersion is rejected as protocol_mismatch (local=ProtocolVersion,
// remote=the frame's version) before anything else is validated or the
// body is decoded; the returned frame then carries that version and, if
// valid, the request ID. Every other defect is invalid_argument. The body
// is returned raw (at most MaxBodyBytes) for the type's own decoder.
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
	if len(body) > MaxBodyBytes {
		return f, errInvalid("message body of %d bytes exceeds %d", len(body), MaxBodyBytes)
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

// DecodeHelloOK decodes a hello_ok body and requires exactly the protocol-1
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

// DecodeHeartbeat decodes a heartbeat body; roles must be an empty array.
func DecodeHeartbeat(body json.RawMessage) (HeartbeatBody, error) {
	const what = "heartbeat body"
	o, err := decodeObject(body, what)
	if err != nil {
		return HeartbeatBody{}, err
	}
	if err := o.only(what, []string{"roles"}); err != nil {
		return HeartbeatBody{}, err
	}
	roles, err := ParseRoles(o.raw["roles"])
	if err != nil {
		return HeartbeatBody{}, err
	}
	return HeartbeatBody{Roles: roles}, nil
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
