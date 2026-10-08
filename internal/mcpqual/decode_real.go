package mcpqual

// Real decoders (design decoder-enrollment B2, FP-17): the inspected
// linux/amd64 success formats of Codex 0.160.0, Grok 1.0.46 and Claude
// 2.1.292, selected only through Registry.Select's exact-version overrides.
// The synthetic family parsers in decode.go are unchanged and keep their
// negative coverage; nothing here is a union of formats.
//
// Every recognized record is first checked by checkJSON (UTF-8, paired
// surrogate escapes, depth, duplicate members at any level, trailing data),
// and so is each embedded tool-result JSON string. Only the listed
// non-evidence records are ignored; an unknown record, an unknown or
// failed terminal, a duplicate, orphan or cross-case probe result, a
// repeated probe call or a terminal before the result is inconclusive
// (ReasonUnrecognized with a fixed safe suffix, never vendor text). A wrong
// JSON type or a malformed payload is ReasonMalformed; a truncated
// transcript is ReasonTruncated; an empty one ReasonNoTranscript. No real
// branch emits a typed error or timeout: unseen failure formats stay
// inconclusive. A vendor call ID is an opaque string, never equated with
// the probe's JSON-RPC request ID; prose and echoed nonces never count.

import (
	"bytes"
	"encoding/json"
)

// The enrolled real identities (design decoder-enrollment B2, Evidence and
// precedence): exact version strings and their one platform.
const (
	CodexRealVersion     = "codex-cli 0.160.0"
	GrokRealVersion      = "grok 1.0.46 (2765805b9442) [stable]"
	ClaudeRealVersion    = "2.1.292 (Claude Code)"
	EnrolledRealPlatform = "linux/amd64"
	// ClaudeProbeTool is Claude's full name of the probe's slow tool.
	ClaudeProbeTool = "mcp__probe__slow"
)

// jobj is one decoded JSON object's members. checkJSON has already refused
// duplicate members at every level, so the map loses nothing.
type jobj map[string]json.RawMessage

// jsonKind is the first byte of a JSON value: '"', '{', '[', 'n', 't', 'f'
// or a number's first character (0 for an empty value).
func jsonKind(raw json.RawMessage) byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	return raw[0]
}

// asObj decodes raw as a JSON object.
func asObj(raw json.RawMessage) (jobj, bool) {
	if jsonKind(raw) != '{' {
		return nil, false
	}
	var o jobj
	if json.Unmarshal(raw, &o) != nil {
		return nil, false
	}
	return o, true
}

// isNull reports member k present as an explicit null.
func (o jobj) isNull(k string) bool {
	raw, ok := o[k]
	return ok && jsonKind(raw) == 'n'
}

// realState is one real decode: the probe call (one at most), its result,
// the unrelated tool calls, the terminals seen and the first failure.
type realState struct {
	out       Decoded
	probe     map[string]string // probe call ID -> its case ID
	results   map[string]bool   // probe call ID -> its result seen
	other     map[string]bool   // unrelated tool call IDs
	terminals int
	failed    string
	stop      bool
}

func newRealState() *realState {
	return &realState{probe: map[string]string{}, results: map[string]bool{}, other: map[string]bool{}}
}

// malformed records a malformed record or payload (it wins over an
// unrecognized form) and stops the decode.
func (s *realState) malformed() {
	s.failed, s.stop = ReasonMalformed, true
}

// unrecognized records the first inconclusive form with its fixed safe
// suffix; decoding continues so a later malformed record still wins.
func (s *realState) unrecognized(what string) {
	if s.failed == "" {
		s.failed = ReasonUnrecognized + ": " + what
	}
}

// record checks one transcript line (checkJSON) and decodes it as an
// object.
func (s *realState) record(data []byte) (jobj, bool) {
	if checkJSON(data) != nil {
		s.malformed()
		return nil, false
	}
	o, ok := asObj(data)
	if !ok {
		s.malformed()
	}
	return o, ok
}

// str is member k as a string: absent is an unrecognized shape (what lacks
// k), any other JSON type is malformed.
func (s *realState) str(o jobj, k, what string) (string, bool) {
	raw, ok := o[k]
	if !ok {
		s.unrecognized(what + " lacks " + k)
		return "", false
	}
	var v string
	if jsonKind(raw) != '"' || json.Unmarshal(raw, &v) != nil {
		s.malformed()
		return "", false
	}
	return v, true
}

// obj is member k as an object, under str's rules.
func (s *realState) obj(o jobj, k, what string) (jobj, bool) {
	raw, ok := o[k]
	if !ok {
		s.unrecognized(what + " lacks " + k)
		return nil, false
	}
	v, ok := asObj(raw)
	if !ok {
		s.malformed()
	}
	return v, ok
}

// arr is member k as an array, under str's rules.
func (s *realState) arr(o jobj, k, what string) ([]json.RawMessage, bool) {
	raw, ok := o[k]
	if !ok {
		s.unrecognized(what + " lacks " + k)
		return nil, false
	}
	var v []json.RawMessage
	if jsonKind(raw) != '[' || json.Unmarshal(raw, &v) != nil {
		s.malformed()
		return nil, false
	}
	return v, true
}

// boolean is member k as a JSON boolean (present tells whether it exists;
// a present non-boolean, null included, is malformed).
func (s *realState) boolean(o jobj, k string) (v, present, ok bool) {
	raw, present := o[k]
	if !present {
		return false, false, true
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true, true
	case "false":
		return false, true, true
	}
	s.malformed()
	return false, true, false
}

// call records the one probe call: a nonempty call ID and case ID, never a
// second probe call, a reused unrelated ID or a call after the terminal.
func (s *realState) call(off int64, id, caseID string) {
	switch {
	case id == "" || caseID == "":
		s.unrecognized("a probe call without its call or case ID")
	case len(s.probe) > 0:
		s.unrecognized("a repeated probe call")
	case s.other[id]:
		s.unrecognized("a probe call reusing another tool's call ID")
	case s.terminals > 0:
		s.unrecognized("a probe call after the terminal")
	default:
		s.probe[id] = caseID
		s.out.Events = append(s.out.Events, Event{CaseID: caseID, Kind: KindToolCall, OffsetNS: off, RequestID: id})
	}
}

// unrelated tracks another tool's call ID, so its updates and results can
// never become the probe's.
func (s *realState) unrelated(id string) {
	switch _, probe := s.probe[id]; {
	case id == "":
		s.unrecognized("an unrelated tool call without an ID")
	case probe || s.other[id]:
		s.unrecognized("a repeated tool call ID")
	default:
		s.other[id] = true
	}
}

// result records the probe call id's one structured result: text is the
// embedded tool-result JSON (checked by checkJSON) holding a nonempty
// case_id and nonce, the case equal to the call's. A duplicate result, one
// after the terminal or one for another case is inconclusive.
func (s *realState) result(off int64, id, text string) {
	caseID := s.probe[id]
	if s.results[id] {
		s.unrecognized("a duplicate probe result")
		return
	}
	if s.terminals > 0 {
		s.unrecognized("a probe result after the terminal")
		return
	}
	if checkJSON([]byte(text)) != nil {
		s.malformed()
		return
	}
	p, ok := asObj(json.RawMessage(text))
	if !ok {
		s.malformed()
		return
	}
	pc, ok1 := s.str(p, "case_id", "the probe result payload")
	nonce, ok2 := s.str(p, "nonce", "the probe result payload")
	switch {
	case !ok1 || !ok2:
		return
	case pc == "" || nonce == "":
		s.unrecognized("the probe result payload has an empty case or nonce")
		return
	case pc != caseID:
		s.unrecognized("a probe result for another case")
		return
	}
	s.results[id] = true
	s.out.Events = append(s.out.Events, Event{CaseID: caseID, Kind: KindToolResult, OffsetNS: off, RequestID: id, Nonce: nonce})
}

// terminal records the successful terminal: exactly one, after the probe
// result.
func (s *realState) terminal() {
	s.terminals++
	s.out.Terminal = true
	switch {
	case s.terminals > 1:
		s.unrecognized("a second terminal")
	case len(s.results) == 0:
		s.unrecognized("a terminal before the probe result")
	}
}

// otherTerminal records a seen terminal whose status is not the supported
// success: Terminal is set, and the decode is inconclusive (never a typed
// error).
func (s *realState) otherTerminal(what string) {
	s.terminals++
	s.out.Terminal = true
	s.unrecognized(what)
}

// done applies the precedence: truncation, an empty transcript, the first
// failure, then the completeness rules (a successful terminal, the probe
// call and its result).
func (s *realState) done(t Transcript) Decoded {
	switch {
	case t.Truncated:
		s.out.Inconclusive = ReasonTruncated
	case len(t.Lines) == 0:
		s.out.Inconclusive = ReasonNoTranscript
	case s.failed != "":
		s.out.Inconclusive = s.failed
	case s.terminals == 0:
		s.out.Inconclusive = ReasonUnrecognized + ": no successful terminal"
	case len(s.probe) == 0:
		s.out.Inconclusive = ReasonUnrecognized + ": no probe call"
	case len(s.results) < len(s.probe):
		s.out.Inconclusive = ReasonUnrecognized + ": a probe call without its result"
	}
	return s.out
}

// decodeCodexReal reads codex-cli 0.160.0's `exec --json` NDJSON: the probe
// is the mcp_tool_call item of server probe and tool slow, started
// in_progress with null result and error, completed with one text content
// block holding the probe's result JSON; turn.completed is the terminal.
// Agent messages and error items (hook diagnostics) are ignored, never
// parsed.
func decodeCodexReal(t Transcript) Decoded {
	s := newRealState()
	for _, l := range t.Lines {
		if s.stop {
			break
		}
		e, ok := s.record(l.Data)
		if !ok {
			break
		}
		typ, ok := s.str(e, "type", "a codex event")
		if !ok {
			continue
		}
		switch typ {
		case "thread.started", "turn.started":
		case "item.started":
			s.codexStarted(l.OffsetNS, e)
		case "item.completed":
			s.codexCompleted(l.OffsetNS, e)
		case "turn.completed":
			s.terminal()
		case "turn.failed", "error":
			s.otherTerminal("a codex failed turn or error")
		default:
			s.unrecognized("an unknown codex event type")
		}
	}
	return s.done(t)
}

// codexItem is an item event's item with its type, ID, server and tool.
func (s *realState) codexItem(e jobj) (it jobj, typ string, ok bool) {
	if it, ok = s.obj(e, "item", "a codex item event"); !ok {
		return nil, "", false
	}
	typ, ok = s.str(it, "type", "a codex item")
	return it, typ, ok
}

func (s *realState) codexStarted(off int64, e jobj) {
	it, typ, ok := s.codexItem(e)
	if !ok {
		return
	}
	if typ != "mcp_tool_call" {
		s.unrecognized("a codex item.started of another item type")
		return
	}
	id, ok1 := s.str(it, "id", "a codex mcp_tool_call")
	server, ok2 := s.str(it, "server", "a codex mcp_tool_call")
	tool, ok3 := s.str(it, "tool", "a codex mcp_tool_call")
	if !ok1 || !ok2 || !ok3 {
		return
	}
	if server != "probe" || tool != "slow" {
		s.unrelated(id)
		return
	}
	status, ok := s.str(it, "status", "the codex probe start")
	if !ok {
		return
	}
	if status != "in_progress" || !it.isNull("result") || !it.isNull("error") {
		s.unrecognized("an unsupported codex probe start shape")
		return
	}
	args, ok := s.obj(it, "arguments", "the codex probe start")
	if !ok {
		return
	}
	if caseID, ok := s.str(args, "case_id", "the codex probe arguments"); ok {
		s.call(off, id, caseID)
	}
}

func (s *realState) codexCompleted(off int64, e jobj) {
	it, typ, ok := s.codexItem(e)
	if !ok {
		return
	}
	switch typ {
	case "agent_message", "error":
		// Prose and nonterminal diagnostics: never a nonce, a result or a
		// terminal.
		return
	case "mcp_tool_call":
	default:
		s.unrecognized("a codex item.completed of another item type")
		return
	}
	id, ok1 := s.str(it, "id", "a codex mcp_tool_call")
	server, ok2 := s.str(it, "server", "a codex mcp_tool_call")
	tool, ok3 := s.str(it, "tool", "a codex mcp_tool_call")
	if !ok1 || !ok2 || !ok3 {
		return
	}
	caseID, probe := s.probe[id]
	if server != "probe" || tool != "slow" {
		switch {
		case probe:
			s.unrecognized("another tool's result for the probe call ID")
		case !s.other[id]:
			s.unrecognized("an unmatched codex tool result")
		}
		return
	}
	if !probe {
		s.unrecognized("a codex probe item completed without its start")
		return
	}
	status, ok := s.str(it, "status", "the codex probe completion")
	if !ok {
		return
	}
	if status != "completed" || !it.isNull("error") {
		s.unrecognized("an unsupported codex probe completion status")
		return
	}
	args, ok := s.obj(it, "arguments", "the codex probe completion")
	if !ok {
		return
	}
	if c, ok := s.str(args, "case_id", "the codex probe arguments"); !ok || c != caseID {
		if ok {
			s.unrecognized("the codex probe completion names another case")
		}
		return
	}
	res, ok := s.obj(it, "result", "the codex probe completion")
	if !ok {
		return
	}
	if _, present := res["structured_content"]; present && !res.isNull("structured_content") {
		s.unrecognized("an unsupported codex structured_content result")
		return
	}
	content, ok := s.arr(res, "content", "the codex probe result")
	if !ok {
		return
	}
	if text, ok := s.oneText(content, "the codex probe result"); ok {
		s.result(off, id, text)
	}
}

// oneText is the text of exactly one content block of type text.
func (s *realState) oneText(content []json.RawMessage, what string) (string, bool) {
	if len(content) != 1 {
		s.unrecognized(what + " is not exactly one content block")
		return "", false
	}
	b, ok := asObj(content[0])
	if !ok {
		s.malformed()
		return "", false
	}
	typ, ok := s.str(b, "type", what+" block")
	if !ok {
		return "", false
	}
	if typ != "text" {
		s.unrecognized(what + " block is not text")
		return "", false
	}
	return s.str(b, "text", what+" block")
}

// decodeGrokReal reads grok 1.0.46's streaming-json records: the probe is a
// pending use_tool tool_call whose rawInput names probe__slow, an interim
// update with null status, output and empty content, and a completed
// update whose MCP rawOutput from server probe and tool slow holds only
// OkayOutput, the probe's result JSON; end with stopReason end_turn is the
// terminal. Command lists, thoughts, text and usage are ignored.
func decodeGrokReal(t Transcript) Decoded {
	s := newRealState()
	for _, l := range t.Lines {
		if s.stop {
			break
		}
		e, ok := s.record(l.Data)
		if !ok {
			break
		}
		typ, ok := s.str(e, "type", "a grok record")
		if !ok {
			continue
		}
		switch typ {
		case "available_commands", "thought", "text", "usage":
		case "tool_call":
			s.grokCall(l.OffsetNS, e)
		case "tool_call_update":
			s.grokUpdate(l.OffsetNS, e)
		case "end":
			if stop, ok := s.str(e, "stopReason", "the grok end record"); ok {
				if stop == "end_turn" {
					s.terminal()
				} else {
					s.otherTerminal("an unsupported grok end reason")
				}
			}
		default:
			s.unrecognized("an unknown grok record type")
		}
	}
	return s.done(t)
}

func (s *realState) grokCall(off int64, e jobj) {
	id, ok1 := s.str(e, "toolCallId", "a grok tool_call")
	name, ok2 := s.str(e, "toolName", "a grok tool_call")
	if !ok1 || !ok2 {
		return
	}
	// A call is the probe's when it names probe__slow, as its toolName or as
	// rawInput.tool_name, whatever its kind: such a call must have exactly
	// the reviewed use_tool shape, otherwise the decode is inconclusive (an
	// unsupported or repeated probe call never hides as an unrelated tool;
	// code review B2 round 1, C1). Only a call naming another tool is
	// unrelated.
	rawTool := ""
	raw, hasRaw := e["rawInput"]
	rawObj, isObj := asObj(raw)
	if isObj {
		if v, present := rawObj["tool_name"]; present {
			if jsonKind(v) != '"' || json.Unmarshal(v, &rawTool) != nil {
				s.malformed()
				return
			}
		}
	}
	if name != GrokProbeTool && rawTool != GrokProbeTool {
		s.unrelated(id)
		return
	}
	kind, ok := s.str(e, "kind", "the grok probe call")
	if !ok {
		return
	}
	switch {
	case name == GrokProbeTool:
		// A direct probe__slow call format has no reviewed evidence.
		s.unrecognized("an unreviewed direct grok probe call")
		return
	case name != "use_tool" || kind != "use_tool" || !hasRaw || !isObj:
		s.unrecognized("an unsupported grok probe call shape")
		return
	}
	status, ok := s.str(e, "status", "the grok probe call")
	if !ok {
		return
	}
	if status != "pending" {
		s.unrecognized("an unsupported grok probe call status")
		return
	}
	input, ok := s.obj(rawObj, "tool_input", "the grok probe call")
	if !ok {
		return
	}
	if caseID, ok := s.str(input, "case_id", "the grok probe input"); ok {
		s.call(off, id, caseID)
	}
}

func (s *realState) grokUpdate(off int64, e jobj) {
	id, ok := s.str(e, "toolCallId", "a grok tool_call_update")
	if !ok {
		return
	}
	if s.other[id] {
		// An unrelated call's output that claims the probe's server and tool
		// would be a hidden probe invocation: inconclusive, never ignored.
		if ro, ok := asObj(e["rawOutput"]); ok {
			var server, tool string
			json.Unmarshal(ro["server_name"], &server)
			json.Unmarshal(ro["tool_name"], &tool)
			if server == "probe" && tool == "slow" {
				s.unrecognized("an unrelated grok call's output from the probe")
			}
		}
		return
	}
	if _, probe := s.probe[id]; !probe {
		s.unrecognized("an update of an unknown grok tool call")
		return
	}
	if e.isNull("status") {
		// The interim update: null status and output, empty content.
		content, ok := s.arr(e, "content", "the grok interim update")
		if ok && (!e.isNull("rawOutput") || len(content) != 0) {
			s.unrecognized("an unsupported grok interim update")
		}
		return
	}
	status, ok := s.str(e, "status", "the grok probe update")
	if !ok {
		return
	}
	if status != "completed" {
		s.unrecognized("an unsupported grok probe update status")
		return
	}
	ro, ok := s.obj(e, "rawOutput", "the grok probe completion")
	if !ok {
		return
	}
	typ, ok1 := s.str(ro, "type", "the grok probe output")
	server, ok2 := s.str(ro, "server_name", "the grok probe output")
	tool, ok3 := s.str(ro, "tool_name", "the grok probe output")
	if !ok1 || !ok2 || !ok3 {
		return
	}
	if typ != "MCP" || server != "probe" || tool != "slow" {
		s.unrecognized("a grok probe output from another server or tool")
		return
	}
	out, ok := s.obj(ro, "output", "the grok probe output")
	if !ok {
		return
	}
	if _, okay := out["OkayOutput"]; !okay || len(out) != 1 {
		s.unrecognized("an unsupported grok output variant")
		return
	}
	if text, ok := s.str(out, "OkayOutput", "the grok probe output"); ok {
		s.result(off, id, text)
	}
}

// decodeClaudeReal reads Claude Code 2.1.292's `-p --output-format json
// --verbose` output: one JSON array in one record. The probe call is an
// assistant tool_use block named mcp__probe__slow, its result the user
// tool_result block of the same ID (is_error absent or false) with one text
// block holding the probe's result JSON, and the terminal the result
// message of subtype success, is_error false and terminal_reason
// completed. Every emitted event carries the last record's offset; vendor
// timestamps never manufacture sub-event timings. System init, rate-limit
// events, assistant text and the ToolSearch call and its tool_reference
// result (tracked as unrelated) emit nothing; auxiliary copies
// (tool_use_result, wire_tool_inputs, tool_use_meta) are never read.
func decodeClaudeReal(t Transcript) Decoded {
	s := newRealState()
	for _, l := range t.Lines {
		if checkJSON(l.Data) != nil {
			s.malformed()
			return s.done(t)
		}
	}
	if t.Truncated || len(t.Lines) == 0 {
		return s.done(t)
	}
	doc, off := joined(t)
	var msgs []json.RawMessage
	if jsonKind(doc) != '[' || json.Unmarshal(doc, &msgs) != nil {
		// A split or incomplete array across records is not this format.
		s.malformed()
		return s.done(t)
	}
	for _, raw := range msgs {
		if s.stop {
			break
		}
		m, ok := asObj(raw)
		if !ok {
			s.malformed()
			break
		}
		typ, ok := s.str(m, "type", "a claude message")
		if !ok {
			continue
		}
		switch typ {
		case "system":
			if sub, ok := s.str(m, "subtype", "a claude system message"); ok && sub != "init" {
				s.unrecognized("an unknown claude system message")
			}
		case "rate_limit_event":
		case "assistant":
			s.claudeBlocks(off, m, true)
		case "user":
			s.claudeBlocks(off, m, false)
		case "result":
			sub, ok1 := s.str(m, "subtype", "the claude result")
			isErr, present, ok2 := s.boolean(m, "is_error")
			reason, ok3 := s.str(m, "terminal_reason", "the claude result")
			switch {
			case !ok1 || !ok2 || !ok3:
				if !s.stop {
					s.otherTerminal("an unsupported claude result")
				}
			case sub == "success" && present && !isErr && reason == "completed":
				s.terminal()
			default:
				s.otherTerminal("an unsupported claude result")
			}
		default:
			s.unrecognized("an unknown claude message type")
		}
	}
	return s.done(t)
}

// claudeBlocks walks one message's content blocks in order: an assistant's
// text and tool_use blocks, or a user's tool_result blocks.
func (s *realState) claudeBlocks(off int64, m jobj, assistant bool) {
	msg, ok := s.obj(m, "message", "a claude message")
	if !ok {
		return
	}
	if raw, ok := msg["content"]; ok && jsonKind(raw) == '"' {
		s.unrecognized("a claude message with text content")
		return
	}
	blocks, ok := s.arr(msg, "content", "a claude message")
	if !ok {
		return
	}
	for _, raw := range blocks {
		if s.stop {
			return
		}
		b, ok := asObj(raw)
		if !ok {
			s.malformed()
			return
		}
		typ, ok := s.str(b, "type", "a claude content block")
		if !ok {
			continue
		}
		switch {
		case assistant && typ == "text":
		case assistant && typ == "tool_use":
			id, ok1 := s.str(b, "id", "a claude tool_use")
			name, ok2 := s.str(b, "name", "a claude tool_use")
			if !ok1 || !ok2 {
				continue
			}
			if name != ClaudeProbeTool {
				s.unrelated(id)
				continue
			}
			in, ok := s.obj(b, "input", "the claude probe call")
			if !ok {
				continue
			}
			if caseID, ok := s.str(in, "case_id", "the claude probe input"); ok {
				s.call(off, id, caseID)
			}
		case !assistant && typ == "tool_result":
			s.claudeResult(off, b)
		default:
			s.unrecognized("an unknown claude content block")
		}
	}
}

func (s *realState) claudeResult(off int64, b jobj) {
	id, ok := s.str(b, "tool_use_id", "a claude tool_result")
	if !ok || s.other[id] {
		return
	}
	if _, probe := s.probe[id]; !probe {
		s.unrecognized("an unmatched claude tool_result")
		return
	}
	isErr, _, ok := s.boolean(b, "is_error")
	if !ok {
		return
	}
	if isErr {
		s.unrecognized("a claude probe tool_result marked is_error")
		return
	}
	if raw, ok := b["content"]; ok && jsonKind(raw) == '"' {
		s.unrecognized("the claude probe result is not a content array")
		return
	}
	content, ok := s.arr(b, "content", "the claude probe result")
	if !ok {
		return
	}
	if text, ok := s.oneText(content, "the claude probe result"); ok {
		s.result(off, id, text)
	}
}

// realVersionClient maps the three enrolled exact versions to their
// clients.
var realVersionClient = map[string]string{CodexRealVersion: "codex", GrokRealVersion: "grok", ClaudeRealVersion: "claude"}

// isRealVersion reports one of the three enrolled exact identities.
func isRealVersion(client, version string) bool {
	c, ok := realVersionClient[version]
	return ok && c == client
}
