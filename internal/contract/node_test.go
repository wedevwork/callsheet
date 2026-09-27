package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testID = "n_0123456789abcdef0123456789abcdef"

func wantErr(t *testing.T, err error, code Code, substr string) {
	t.Helper()
	if CodeOf(err) != code || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want %s containing %q", err, code, substr)
	}
}

func TestValidators(t *testing.T) {
	for id, ok := range map[string]bool{
		testID: true, "n_0123456789ABCDEF0123456789abcdef": false, "n_0123": false, "x_0123456789abcdef0123456789abcdef": false,
		testID + "0": false, "": false, "n_0123456789abcdef0123456789abcdeg": false, " " + testID: false,
	} {
		if ValidNodeID(id) != ok {
			t.Errorf("ValidNodeID(%q) != %v", id, ok)
		}
	}
	for id, ok := range map[string]bool{"h1": true, "b12": true, "A-z_09": true, strings.Repeat("a", 64): true, strings.Repeat("a", 65): false, "": false, "a b": false, "a/b": false, "é": false} {
		if ValidRequestID(id) != ok {
			t.Errorf("ValidRequestID(%q) != %v", id, ok)
		}
	}
	for v, ok := range map[string]bool{"dev": true, "1.2.3-rc.1+meta": true, strings.Repeat("v", 128): true, strings.Repeat("v", 129): false, "": false, "a b": false, "a\tb": false, "a\nb": false, "\x7f": false, "é": false, "!~": true} {
		if ValidSoftwareVersion(v) != ok {
			t.Errorf("ValidSoftwareVersion(%q) != %v", v, ok)
		}
	}
	for s, want := range map[string]int{"0": 0, "1": 1, "-3": -3, "15000": 15000} {
		if n, ok := ParseInteger(s); !ok || n != want {
			t.Errorf("ParseInteger(%q) = %d %v", s, n, ok)
		}
	}
	for _, s := range []string{"", "01", "+1", "1.0", "1e3", " 1", "1234567890123456789", "x"} {
		if _, ok := ParseInteger(s); ok {
			t.Errorf("ParseInteger(%q) accepted", s)
		}
	}
	e := VersionMismatch(1, 2)
	if e.Code != CodeProtocolMismatch || e.Message != "protocol version mismatch: local=1 remote=2" || e.Details["local_version"] != 1 || e.Details["remote_version"] != 2 {
		t.Fatalf("mismatch = %+v", e)
	}
	if ExitCode(e) != 7 || HTTPStatus(e.Code) != 409 || !IsCode(e, CodeProtocolMismatch) || IsCode(errors.New("x"), CodeInternal) {
		t.Fatal("mismatch mappings")
	}
	if got := SafeText("a\x00bé"+strings.Repeat("x", 20), 8); got != "a?b??xxx" {
		t.Fatalf("SafeText = %q", got)
	}
}

func ptr[T any](v T) *T { return &v }

func TestNodeJSON(t *testing.T) {
	ts := time.Date(2026, 9, 26, 12, 0, 0, 5, time.FixedZone("x", 3600))
	n := Node{ID: testID, Liveness: LivenessOnline, LastSeen: &ts, ProtocolVersion: ptr(1), SoftwareVersion: ptr("dev")}
	b, err := json.Marshal(n)
	want := `{"id":"` + testID + `","liveness":"online","last_seen":"2026-09-26T11:00:00.000000005Z","protocol_version":1,"software_version":"dev","roles":[]}`
	if err != nil || string(b) != want {
		t.Fatalf("node = %s %v", b, err)
	}
	empty := Node{ID: testID, Liveness: LivenessOffline}
	b, _ = json.Marshal(empty)
	if string(b) != `{"id":"`+testID+`","liveness":"offline","last_seen":null,"protocol_version":null,"software_version":null,"roles":[]}` {
		t.Fatalf("empty node = %s", b)
	}
	list, _ := Encode(NodeListResponse{Version: 2})
	if string(list) != `{"version":2,"nodes":[]}` {
		t.Fatalf("empty list = %s", list)
	}
	resp, _ := Encode(NodeResponse{Version: 2, Node: n})
	got, err := ParseNodeResponse(resp)
	if err != nil || got.ID != testID || !got.LastSeen.Equal(ts) || *got.ProtocolVersion != 1 || *got.SoftwareVersion != "dev" || len(got.Roles) != 0 || got.Roles == nil {
		t.Fatalf("round trip = %+v %v", got, err)
	}
	two := Node{ID: "n_1123456789abcdef0123456789abcdef", Liveness: LivenessOffline}
	lb, _ := Encode(NodeListResponse{Version: 2, Nodes: []Node{empty, two}})
	nodes, err := ParseNodeListResponse(lb)
	if err != nil || len(nodes) != 2 || nodes[0].LastSeen != nil || nodes[1].ID != two.ID {
		t.Fatalf("list = %+v %v", nodes, err)
	}
	if nodes, err := ParseNodeListResponse([]byte(`{"version":2,"nodes":[]}`)); err != nil || len(nodes) != 0 {
		t.Fatalf("empty list = %v %v", nodes, err)
	}
}

func TestNodeParsingRejects(t *testing.T) {
	good := `{"id":"` + testID + `","liveness":"offline","last_seen":null,"protocol_version":null,"software_version":null,"roles":[]}`
	repl := func(old, new string) string { return strings.Replace(good, old, new, 1) }
	for name, c := range map[string]struct{ in, want string }{
		"not object":    {`[]`, "not a JSON object"},
		"trailing":      {good + `{}`, "trailing data"},
		"duplicate":     {repl(`"roles":[]`, `"roles":[],"roles":[]`), "duplicate field"},
		"unknown":       {repl(`"roles":[]`, `"roles":[],"x":1`), "unknown field"},
		"missing id":    {repl(`"id":"`+testID+`",`, ""), `required field "id"`},
		"missing seen":  {repl(`"last_seen":null,`, ""), `required field "last_seen"`},
		"null roles":    {repl(`"roles":[]`, `"roles":null`), "must not be null"},
		"null id":       {repl(`"`+testID+`"`, "null"), "must not be null"},
		"bad id":        {repl(testID, "n_1"), "valid node ID"},
		"liveness":      {repl(`"offline"`, `"maybe"`), "liveness"},
		"seen format":   {repl(`"last_seen":null`, `"last_seen":"2026-09-26T12:00:00+01:00"`), "UTC RFC3339"},
		"seen type":     {repl(`"last_seen":null`, `"last_seen":5`), "must be a string"},
		"pv type":       {repl(`"protocol_version":null`, `"protocol_version":"1"`), "integer"},
		"pv float":      {repl(`"protocol_version":null`, `"protocol_version":1.5`), "integer"},
		"sv spaces":     {repl(`"software_version":null`, `"software_version":"a b"`), "software_version"},
		"sv type":       {repl(`"software_version":null`, `"software_version":1`), "must be a string"},
		"roles object":  {repl(`"roles":[]`, `"roles":{}`), "must be an array"},
		"role inflight": {repl(`"roles":[]`, `"roles":[{"role_id":"r","inflight":1,"concurrency":1,"can_accept":true}]`), "inflight must be 0"},
		"role zero":     {repl(`"roles":[]`, `"roles":[{"role_id":"r","inflight":0,"concurrency":0,"can_accept":true}]`), "concurrency must be 1"},
		"role big":      {repl(`"roles":[]`, `"roles":[{"role_id":"r","inflight":0,"concurrency":2147483648,"can_accept":true}]`), "concurrency must be 1"},
		"role slug":     {repl(`"roles":[]`, `"roles":[{"role_id":"R","inflight":0,"concurrency":1,"can_accept":true}]`), "role ID slug"},
		"role repeat":   {repl(`"roles":[]`, `"roles":[{"role_id":"r","inflight":0,"concurrency":1,"can_accept":true},{"role_id":"r","inflight":0,"concurrency":2,"can_accept":false}]`), "repeats role r"},
		"role shape":    {repl(`"roles":[]`, `"roles":[{"role_id":"r"}]`), "required field"},
		"role type":     {repl(`"roles":[]`, `"roles":[{"role_id":"r","inflight":"0","concurrency":1,"can_accept":true}]`), "integer"},
		"role bool":     {repl(`"roles":[]`, `"roles":[{"role_id":"r","inflight":0,"concurrency":1,"can_accept":1}]`), "boolean"},
		"role id":       {repl(`"roles":[]`, `"roles":[{"role_id":1,"inflight":0,"concurrency":1,"can_accept":true}]`), "string"},
		"role conc":     {repl(`"roles":[]`, `"roles":[{"role_id":"r","inflight":0,"concurrency":"1","can_accept":true}]`), "integer"},
		"role array":    {repl(`"roles":[]`, `"roles":[1]`), "not a JSON object"},
		"malformed":     {`{"id":`, "malformed"},
		"key garbage":   {`{"id" 1}`, "malformed"},
		"liv type":      {repl(`"offline"`, `1`), "must be a string"},
	} {
		_, err := ParseNode(json.RawMessage(c.in))
		if CodeOf(err) != CodeInvalidArgument || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	for name, c := range map[string]struct{ in, want string }{
		"version":  {`{"version":1,"node":` + good + `}`, "protocol version mismatch: local=2 remote=1"},
		"version2": {`{"version":"2","node":` + good + `}`, "integer"},
		"extra":    {`{"version":2,"node":` + good + `,"x":1}`, "unknown field"},
		"null":     {`{"version":2,"node":null}`, "must not be null"},
		"bad node": {`{"version":2,"node":{}}`, "required field"},
		"array":    {`[]`, "not a JSON object"},
	} {
		_, err := ParseNodeResponse([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("response %s: %v", name, err)
		}
	}
	for name, c := range map[string]struct{ in, want string }{
		"null nodes": {`{"version":2,"nodes":null}`, "must not be null"},
		"not array":  {`{"version":2,"nodes":{}}`, "must be an array"},
		"unsorted":   {`{"version":2,"nodes":[` + strings.Replace(good, "n_0", "n_1", 1) + `,` + good + `]}`, "not sorted"},
		"duplicate":  {`{"version":2,"nodes":[` + good + `,` + good + `]}`, "not sorted"},
		"bad item":   {`{"version":2,"nodes":[{}]}`, "required field"},
		"version":    {`{"version":3,"nodes":[]}`, "remote=3"},
		"missing":    {`{"version":2}`, "required field"},
		"garbage":    {`x`, "not a JSON object"},
		"broken arr": {`{"version":2,"nodes":[1,}`, "malformed"},
	} {
		_, err := ParseNodeListResponse([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("list %s: %v", name, err)
		}
	}
	if _, err := array(json.RawMessage(`[1,`), "x"); err == nil {
		t.Fatal("broken array accepted")
	}
}

func TestEnrollRequestAndErrorBody(t *testing.T) {
	req, err := ParseEnrollRequest([]byte(`{"node_id":"` + testID + `","software_version":"dev"}`))
	if err != nil || req.NodeID != testID || req.SoftwareVersion != "dev" {
		t.Fatalf("enroll = %+v %v", req, err)
	}
	for name, c := range map[string]struct{ in, want string }{
		"big":     {`{"node_id":"` + strings.Repeat(" ", MaxEnrollRequestBytes) + `"}`, "larger than"},
		"id":      {`{"node_id":"n_1","software_version":"dev"}`, "node_id must match"},
		"version": {`{"node_id":"` + testID + `","software_version":"a b"}`, "software_version"},
		"extra":   {`{"node_id":"` + testID + `","software_version":"dev","x":1}`, "unknown field"},
		"type":    {`{"node_id":1,"software_version":"dev"}`, "must be a string"},
		"type2":   {`{"node_id":"` + testID + `","software_version":2}`, "must be a string"},
		"null":    {`{"node_id":"` + testID + `","software_version":null}`, "must not be null"},
		"garbage": {`x`, "not a JSON object"},
	} {
		_, err := ParseEnrollRequest([]byte(c.in))
		wantErr(t, err, CodeInvalidArgument, c.want)
		_ = name
	}
	e, err := ParseErrorBody([]byte(`{"error":{"code":"not_found","message":"no\nsuch","details":{"n":3}}}`))
	if err != nil || e.Code != CodeNotFound || e.Message != "no?such" {
		t.Fatalf("error body = %+v %v", e, err)
	}
	if n, ok := e.DetailInt("n"); !ok || n != 3 {
		t.Fatalf("detail = %d %v", n, ok)
	}
	if _, ok := e.DetailInt("missing"); ok {
		t.Fatal("missing detail")
	}
	e.Details["f"], e.Details["i"], e.Details["frac"] = 2.0, 4, 2.5
	if n, ok := e.DetailInt("f"); !ok || n != 2 {
		t.Fatal("float detail")
	}
	if n, ok := e.DetailInt("i"); !ok || n != 4 {
		t.Fatal("int detail")
	}
	if _, ok := e.DetailInt("frac"); ok {
		t.Fatal("fractional detail")
	}
	for _, bad := range []string{`{"error":{"code":"bogus","message":"m"}}`, `{"error":{"code":"internal"}}`, `{"error":{"code":"internal","message":"m","details":[]}}`,
		`{"error":null}`, `{"error":{"code":1,"message":"m"}}`, `{"error":{"code":"internal","message":2}}`, `{"x":1}`, `{"error":[]}`, `x`} {
		if _, err := ParseErrorBody([]byte(bad)); CodeOf(err) != CodeInvalidArgument {
			t.Errorf("error body %s accepted: %v", bad, err)
		}
	}
	if e, err := ParseErrorBody([]byte(`{"error":{"code":"internal","message":"m","details":null}}`)); err != nil || e.Details != nil {
		t.Fatalf("null details = %v %v", e, err)
	}
	if !knownCode(CodeNotImplemented) || knownCode("x") {
		t.Fatal("knownCode")
	}
}

func TestParseTime(t *testing.T) {
	for s, ok := range map[string]bool{"2026-09-26T12:00:00Z": true, "2026-09-26T12:00:00.5Z": true, "2026-09-26T12:00:00.50Z": false, "2026-09-26T12:00:00+00:00": false, "x": false} {
		if _, got := ParseTime(s); got != ok {
			t.Errorf("ParseTime(%q) = %v", s, got)
		}
	}
}

func frame(version any, typ, rid, body string) []byte {
	v, _ := json.Marshal(version)
	return []byte(`{"version":` + string(v) + `,"type":"` + typ + `","request_id":"` + rid + `","body":` + body + `}`)
}

// TestFrames is UT-Frames: every shape, direction, limit and the
// version-before-body rule.
func TestFrames(t *testing.T) {
	hello, err := EncodeFrame(ProtocolVersion, FrameHello, "h1", HelloBody{NodeID: testID, SoftwareVersion: "dev"})
	want := `{"version":2,"type":"hello","request_id":"h1","body":{"node_id":"` + testID + `","software_version":"dev"}}`
	if err != nil || string(hello) != want {
		t.Fatalf("hello = %s %v", hello, err)
	}
	f, err := DecodeFrame(hello, FromSidecar)
	if err != nil || f.Type != FrameHello || f.RequestID != "h1" {
		t.Fatalf("decode hello = %+v %v", f, err)
	}
	h, err := DecodeHello(f.Body)
	if err != nil || h.NodeID != testID || h.SoftwareVersion != "dev" {
		t.Fatalf("hello body = %+v %v", h, err)
	}
	ok, _ := EncodeFrame(ProtocolVersion, FrameHelloOK, "h1", HelloOKBody{HeartbeatIntervalMS, LeaseMS})
	if string(ok) != `{"version":2,"type":"hello_ok","request_id":"h1","body":{"heartbeat_interval_ms":5000,"lease_ms":15000}}` {
		t.Fatalf("hello_ok = %s", ok)
	}
	f, _ = DecodeFrame(ok, FromPlane)
	if _, err := DecodeHelloOK(f.Body); err != nil {
		t.Fatal(err)
	}
	hb, _ := EncodeFrame(ProtocolVersion, FrameHeartbeat, "b1", HeartbeatBody{})
	if string(hb) != `{"version":2,"type":"heartbeat","request_id":"b1","body":{"roles_revision":0,"roles":[]}}` {
		t.Fatalf("heartbeat = %s", hb)
	}
	f, _ = DecodeFrame(hb, FromSidecar)
	if b, err := DecodeHeartbeat(f.Body); err != nil || b.Roles == nil {
		t.Fatalf("heartbeat body %v %v", b, err)
	}
	ack, _ := EncodeFrame(ProtocolVersion, FrameHeartbeatAck, "b1", nil)
	if string(ack) != `{"version":2,"type":"heartbeat_ack","request_id":"b1","body":{}}` {
		t.Fatalf("ack = %s", ack)
	}
	f, _ = DecodeFrame(ack, FromPlane)
	if err := DecodeAck(f.Body); err != nil {
		t.Fatal(err)
	}
	ef, _ := EncodeFrame(ProtocolVersion, FrameError, "h1", VersionMismatch(1, 2))
	if string(ef) != `{"version":2,"type":"error","request_id":"h1","body":{"error":{"code":"protocol_mismatch","message":"protocol version mismatch: local=1 remote=2","details":{"local_version":1,"remote_version":2}}}}` {
		t.Fatalf("error frame = %s", ef)
	}
	for _, from := range []Direction{FromSidecar, FromPlane} {
		f, err = DecodeFrame(ef, from)
		if err != nil || f.Type != FrameError {
			t.Fatalf("error frame from %d: %v", from, err)
		}
	}
	pe, err := ParseErrorBody(f.Body)
	if err != nil || pe.Code != CodeProtocolMismatch {
		t.Fatalf("error body %v %v", pe, err)
	}
	if l, _ := pe.DetailInt("local_version"); l != 1 {
		t.Fatal("details")
	}

	// Version before body: a mismatched integer version wins over every
	// other defect, including an unknown type, a bad body and extra keys.
	for _, in := range [][]byte{
		frame(1, "hello", "h7", `{"anything":true}`),
		[]byte(`{"version":1,"type":"future","request_id":"h7","body":{"x":1},"extra":1}`),
		[]byte(`{"type":"future","version":1,"request_id":"h7"}`),
	} {
		f, err := DecodeFrame(in, FromSidecar)
		if CodeOf(err) != CodeProtocolMismatch || !strings.Contains(err.Error(), "local=2 remote=1") || f.Version != 1 || f.RequestID != "h7" {
			t.Fatalf("mismatch %s = %+v %v", in, f, err)
		}
	}
	if f, err := DecodeFrame([]byte(`{"version":0,"request_id":"bad id"}`), FromPlane); CodeOf(err) != CodeProtocolMismatch || f.RequestID != "" {
		t.Fatalf("invalid request id kept: %+v %v", f, err)
	}

	for name, c := range map[string]struct {
		in   []byte
		from Direction
		want string
	}{
		"not object":     {[]byte(`[1]`), FromSidecar, "not a JSON object"},
		"no version":     {[]byte(`{"type":"hello","request_id":"h1","body":{}}`), FromSidecar, `"version"`},
		"null version":   {[]byte(`{"version":null,"type":"hello","request_id":"h1","body":{}}`), FromSidecar, `"version"`},
		"string version": {frame("2", "hello", "h1", `{}`), FromSidecar, "must be an integer"},
		"float version":  {frame(1.5, "hello", "h1", `{}`), FromSidecar, "must be an integer"},
		"duplicate":      {[]byte(`{"version":2,"version":2,"type":"hello","request_id":"h1","body":{}}`), FromSidecar, "duplicate field"},
		"unknown":        {[]byte(`{"version":2,"type":"hello","request_id":"h1","body":{},"x":1}`), FromSidecar, "unknown field"},
		"null type":      {[]byte(`{"version":2,"type":null,"request_id":"h1","body":{}}`), FromSidecar, "must not be null"},
		"null body":      {[]byte(`{"version":2,"type":"hello","request_id":"h1","body":null}`), FromSidecar, "must not be null"},
		"no body":        {[]byte(`{"version":2,"type":"hello","request_id":"h1"}`), FromSidecar, "required field"},
		"trailing":       {append(frame(2, "hello", "h1", `{}`), []byte(` {}`)...), FromSidecar, "trailing data"},
		"bad request id": {frame(2, "hello", "a b", `{}`), FromSidecar, "request_id must be"},
		"long rid":       {frame(2, "hello", strings.Repeat("a", 65), `{}`), FromSidecar, "request_id must be"},
		"rid type":       {[]byte(`{"version":2,"type":"hello","request_id":1,"body":{}}`), FromSidecar, "must be a string"},
		"type type":      {[]byte(`{"version":2,"type":1,"request_id":"h1","body":{}}`), FromSidecar, "must be a string"},
		"unknown type":   {frame(2, "task", "h1", `{}`), FromSidecar, `unexpected message type "task"`},
		"wrong dir 1":    {frame(2, "hello_ok", "h1", `{}`), FromSidecar, "not valid in this direction"},
		"wrong dir 2":    {frame(2, "heartbeat", "b1", `{"roles":[]}`), FromPlane, "not valid in this direction"},
		"wrong dir 3":    {frame(2, "hello", "h1", `{}`), FromPlane, "not valid in this direction"},
		"body array":     {frame(2, "hello", "h1", `[]`), FromSidecar, "body must be a JSON object"},
		"body string":    {frame(2, "hello", "h1", `"x"`), FromSidecar, "body must be a JSON object"},
		"malformed":      {[]byte(`{"version":2,`), FromSidecar, "malformed"},
	} {
		_, err := DecodeFrame(c.in, c.from)
		if CodeOf(err) != CodeInvalidArgument || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	for name, c := range map[string]struct {
		dec  func(json.RawMessage) error
		body string
		want string
	}{
		"hello extra":    {func(b json.RawMessage) error { _, e := DecodeHello(b); return e }, `{"node_id":"` + testID + `","software_version":"dev","x":1}`, "unknown field"},
		"hello id":       {func(b json.RawMessage) error { _, e := DecodeHello(b); return e }, `{"node_id":"n_1","software_version":"dev"}`, "node_id"},
		"hello sv":       {func(b json.RawMessage) error { _, e := DecodeHello(b); return e }, `{"node_id":"` + testID + `","software_version":""}`, "software_version"},
		"hello null":     {func(b json.RawMessage) error { _, e := DecodeHello(b); return e }, `{"node_id":null,"software_version":"dev"}`, "must not be null"},
		"hello types":    {func(b json.RawMessage) error { _, e := DecodeHello(b); return e }, `{"node_id":1,"software_version":"dev"}`, "must be a string"},
		"hello types2":   {func(b json.RawMessage) error { _, e := DecodeHello(b); return e }, `{"node_id":"` + testID + `","software_version":1}`, "must be a string"},
		"hello garbage":  {func(b json.RawMessage) error { _, e := DecodeHello(b); return e }, `1`, "not a JSON object"},
		"ok values":      {func(b json.RawMessage) error { _, e := DecodeHelloOK(b); return e }, `{"heartbeat_interval_ms":1000,"lease_ms":15000}`, "must carry heartbeat_interval_ms=5000"},
		"ok lease":       {func(b json.RawMessage) error { _, e := DecodeHelloOK(b); return e }, `{"heartbeat_interval_ms":5000,"lease_ms":3}`, "lease_ms=15000"},
		"ok type":        {func(b json.RawMessage) error { _, e := DecodeHelloOK(b); return e }, `{"heartbeat_interval_ms":"5000","lease_ms":15000}`, "integer"},
		"ok type2":       {func(b json.RawMessage) error { _, e := DecodeHelloOK(b); return e }, `{"heartbeat_interval_ms":5000,"lease_ms":"x"}`, "integer"},
		"ok missing":     {func(b json.RawMessage) error { _, e := DecodeHelloOK(b); return e }, `{"heartbeat_interval_ms":5000}`, "required field"},
		"ok garbage":     {func(b json.RawMessage) error { _, e := DecodeHelloOK(b); return e }, `[]`, "not a JSON object"},
		"hb roles null":  {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles_revision":0,"roles":null}`, "must not be null"},
		"hb roles bad":   {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles_revision":0,"roles":[{"role_id":"a","inflight":2,"concurrency":1,"can_accept":true}]}`, "inflight must be 0"},
		"hb extra":       {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles_revision":0,"roles":[],"x":1}`, "unknown field"},
		"hb missing":     {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles":[]}`, "required field"},
		"hb revision":    {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles_revision":-1,"roles":[]}`, "integer from 0"},
		"hb rev float":   {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles_revision":1.0,"roles":[]}`, "integer from 0"},
		"hb rev big":     {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles_revision":9007199254740992,"roles":[]}`, "integer from 0"},
		"hb garbage":     {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `x`, "not a JSON object"},
		"hb roles type":  {func(b json.RawMessage) error { _, e := DecodeHeartbeat(b); return e }, `{"roles_revision":0,"roles":"x"}`, "must be an array"},
		"ack extra":      {DecodeAck, `{"x":1}`, "unknown field"},
		"ack not object": {DecodeAck, `[]`, "not a JSON object"},
	} {
		if err := c.dec(json.RawMessage(c.body)); CodeOf(err) != CodeInvalidArgument || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// TestFrameLimits checks the exact boundaries (protocol 2): the 2 MiB
// message and 1 MiB absolute body limits and every type-specific body limit
// (roles_replace 1 MiB, heartbeat 32 KiB, role_validate 16 KiB, all others
// 8 KiB) are accepted at the limit and rejected one byte above it, on input
// and on output. Whitespace inside the body counts.
func TestFrameLimits(t *testing.T) {
	pad := func(n int) string { return strings.Repeat("a", n) }
	// A body of exactly n bytes: {"x":"aaa..."}.
	bodyOf := func(n int) string { return `{"x":"` + pad(n-8) + `"}` }
	type big struct {
		X string `json:"x"`
	}
	for _, c := range []struct {
		typ   string
		from  Direction
		limit int
	}{
		{FrameHello, FromSidecar, MaxOtherBody},
		{FrameHelloOK, FromPlane, MaxOtherBody},
		{FrameHeartbeatAck, FromPlane, MaxOtherBody},
		{FrameError, FromSidecar, MaxOtherBody},
		{FrameRoleValidateResult, FromSidecar, MaxOtherBody},
		{FrameRolesReplaceAck, FromSidecar, MaxOtherBody},
		{FrameHeartbeat, FromSidecar, MaxHeartbeatBody},
		{FrameRoleValidate, FromPlane, MaxRoleValidateBody},
		{FrameRolesReplace, FromPlane, MaxRolesReplaceBody},
	} {
		if BodyLimit(c.typ) != c.limit {
			t.Fatalf("%s limit = %d", c.typ, BodyLimit(c.typ))
		}
		at := bodyOf(c.limit)
		if len(at) != c.limit {
			t.Fatal(len(at))
		}
		if f, err := DecodeFrame(frame(2, c.typ, "p1", at), c.from); err != nil || len(f.Body) != c.limit {
			t.Fatalf("%s body at limit: %v", c.typ, err)
		}
		if _, err := DecodeFrame(frame(2, c.typ, "p1", bodyOf(c.limit+1)), c.from); CodeOf(err) != CodeInvalidArgument || !strings.Contains(err.Error(), "exceeds "+strconv.Itoa(c.limit)) {
			t.Fatalf("%s body above limit: %v", c.typ, err)
		}
		// Whitespace counts: a compact body one byte under the limit plus
		// one inner space is exactly at it, two spaces are above it.
		spaced := `{ "x":"` + pad(c.limit-9) + `"}`
		if _, err := DecodeFrame(frame(2, c.typ, "p1", spaced), c.from); err != nil {
			t.Fatalf("%s spaced body at limit: %v", c.typ, err)
		}
		if _, err := DecodeFrame(frame(2, c.typ, "p1", `{  "x":"`+pad(c.limit-9)+`"}`), c.from); CodeOf(err) != CodeInvalidArgument {
			t.Fatalf("%s spaced body above limit: %v", c.typ, err)
		}
		if _, err := EncodeFrame(2, c.typ, "p1", big{pad(c.limit - 8)}); err != nil {
			t.Fatalf("%s outbound body at limit: %v", c.typ, err)
		}
		if _, err := EncodeFrame(2, c.typ, "p1", big{pad(c.limit - 7)}); err == nil || !strings.Contains(err.Error(), "exceeds "+strconv.Itoa(c.limit)) {
			t.Fatalf("%s outbound body above limit: %v", c.typ, err)
		}
	}
	if MaxBodyBytes != 1<<20 || MaxFrameBytes != 2<<20 || MaxRolesReplaceBody != MaxBodyBytes {
		t.Fatal("absolute limits changed")
	}
	// A message of exactly MaxFrameBytes with whitespace padding outside the
	// body, and one byte more.
	base := frame(2, "hello", "h1", `{}`)
	exact := append(bytes.Repeat([]byte(" "), MaxFrameBytes-len(base)), base...)
	if len(exact) != MaxFrameBytes {
		t.Fatal(len(exact))
	}
	if _, err := DecodeFrame(exact, FromSidecar); err != nil {
		t.Fatalf("message at limit: %v", err)
	}
	if _, err := DecodeFrame(append([]byte(" "), exact...), FromSidecar); CodeOf(err) != CodeInvalidArgument || !strings.Contains(err.Error(), "exceeds 2097152") {
		t.Fatalf("message above limit: %v", err)
	}
	if _, err := EncodeFrame(2, FrameHello, pad(MaxFrameBytes), nil); err == nil || !strings.Contains(err.Error(), "exceeds 2097152") {
		t.Fatalf("outbound message above limit: %v", err)
	}
	if _, err := EncodeFrame(2, FrameHello, "h1", make(chan int)); err == nil {
		t.Fatal("unencodable body accepted")
	}
}

// BenchmarkNodeFrame encodes and decodes a heartbeat and a boundary-sized
// message per iteration, checking the round trip every time.
func BenchmarkNodeFrame(b *testing.B) {
	type big struct {
		X string `json:"x"`
	}
	payload := big{strings.Repeat("a", MaxOtherBody-8)}
	b.ReportAllocs()
	for b.Loop() {
		hb, err := EncodeFrame(ProtocolVersion, FrameHeartbeat, "b1", HeartbeatBody{})
		if err != nil {
			b.Fatal(err)
		}
		f, err := DecodeFrame(hb, FromSidecar)
		if err != nil || f.RequestID != "b1" {
			b.Fatalf("heartbeat round trip: %v", err)
		}
		if body, err := DecodeHeartbeat(f.Body); err != nil || body.Roles == nil {
			b.Fatalf("heartbeat body: %v", err)
		}
		m, err := EncodeFrame(ProtocolVersion, FrameHello, "h1", payload)
		if err != nil || len(m) > MaxFrameBytes {
			b.Fatalf("boundary encode: %v", err)
		}
		f, err = DecodeFrame(m, FromSidecar)
		if err != nil || len(f.Body) != MaxOtherBody {
			b.Fatalf("boundary decode: %v %d", err, len(f.Body))
		}
	}
}
