package mcpqual

// Vendor transcript decoders (FP-12). Each decoder maps its vendor's
// noninteractive output to typed, case-correlated events. Classification
// uses structured fields only (event types, codes, tool result payloads);
// model prose (a final answer or an assistant message) is never read, so a
// narrative mentioning "timeout" can never become a tool timeout.
//
// The event paths below are the synthetic fixture contracts: they test
// parser behaviour only. A decoder version becomes qualified for VERIFIED
// classification only when an owner's redacted actual transcript fixture
// for exactly that version is registered (Qualified): the three enrolled
// real versions of design decoder-enrollment B2 and Cursor's of B3 decode
// with their own exact-version parsers (decode_real.go). Selection is by exact vendor
// version string and never falls back to another version.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Normalized event kinds.
const (
	KindToolCall         = "tool_call"
	KindToolResult       = "tool_result"
	KindToolError        = "tool_error"
	KindMCPTimeout       = "mcp_timeout"
	KindAuthError        = "auth_error"
	KindPermissionDenied = "permission_denied"
	KindModelRefusal     = "model_refusal"
	KindSessionError     = "session_error"
)

// Event is one normalized decoder output.
type Event struct {
	CaseID     string `json:"case_id"`
	Kind       string `json:"event_kind"`
	OffsetNS   int64  `json:"monotonic_offset"`
	RequestID  string `json:"request_id,omitempty"`
	Nonce      string `json:"nonce,omitempty"`
	SafeReason string `json:"safe_reason,omitempty"`
}

// Line is one transcript line with the harness's monotonic offset (from
// case start) at which it was read.
type Line struct {
	OffsetNS int64
	Data     []byte
}

// Transcript is a vendor's stdout, bounded; Truncated means the bound was
// reached and the experiment is invalid.
type Transcript struct {
	Lines     []Line
	Truncated bool
}

// Size is the transcript's byte count including line terminators.
func (t Transcript) Size() int {
	n := 0
	for _, l := range t.Lines {
		n += len(l.Data) + 1
	}
	return n
}

// Decoded is a decoder's result: its events, whether a terminal event was
// seen, and why classification is inconclusive (empty when it is not).
type Decoded struct {
	Events       []Event
	Terminal     bool
	Inconclusive string
}

// DecoderVersion is one supported vendor version of a decoder.
type DecoderVersion struct {
	Version string `json:"version"`
	Fixture string `json:"fixture"`
	// Qualified is true only when a redacted actual owner transcript for
	// this exact version backs the event paths; only then may a
	// measurement become a VERIFIED catalog fact.
	Qualified bool `json:"qualified"`
	// Evidence (design decoder-enrollment, Enrollment contract) lists the
	// enrolled real fixtures behind a qualified version, per platform, and
	// the event capabilities each demonstrated. A qualified version without
	// evidence (a historical record) is unverified: no qualified behavior.
	Evidence []DecoderEvidence `json:"evidence,omitempty"`
}

// DecoderEvidence is one enrolled fixture of a qualified decoder version:
// its platform ("<os>/<arch>"), its stable fixture ID and the event
// capabilities its reviewed transcript demonstrated.
type DecoderEvidence struct {
	Platform string   `json:"platform"`
	Fixture  string   `json:"fixture"`
	Kinds    []string `json:"kinds"`
}

// Evidence capabilities (design decoder-enrollment, Enrollment contract).
const (
	CapToolCall         = "tool_call"
	CapToolResult       = "tool_result"
	CapTerminalSuccess  = "terminal_success"
	CapMCPTimeout       = "mcp_timeout"
	CapToolError        = "tool_error"
	CapAuthError        = "auth_error"
	CapPermissionDenied = "permission_denied"
	CapModelRefusal     = "model_refusal"
	CapSessionError     = "session_error"
)

// Capabilities are the allowed evidence capability names, in order.
var Capabilities = []string{CapToolCall, CapToolResult, CapTerminalSuccess, CapMCPTimeout, CapToolError, CapAuthError, CapPermissionDenied, CapModelRefusal, CapSessionError}

// ReasonUnverifiedEvent is a qualified decoder's outcome whose kind its
// evidence does not demonstrate for the run's platform: never a
// conclusive measurement.
const ReasonUnverifiedEvent = "unverified_decoder_event"

// RequiredCapabilities lists the evidence an outcome needs from a
// qualified decoder: a success needs the call, its result and a successful
// terminal; each error needs its own capability. Inconclusive outcomes
// need none.
func RequiredCapabilities(outcome string) []string {
	switch outcome {
	case KindToolResult:
		return []string{CapToolCall, CapToolResult, CapTerminalSuccess}
	case KindMCPTimeout:
		return []string{CapToolCall, CapMCPTimeout}
	case KindToolError:
		return []string{CapToolCall, CapToolError}
	case KindAuthError:
		return []string{CapAuthError}
	case KindPermissionDenied:
		return []string{CapPermissionDenied}
	case KindModelRefusal:
		return []string{CapModelRefusal}
	case KindSessionError:
		return []string{CapSessionError}
	}
	return nil
}

// MissingCapabilities returns the capabilities of want that v's evidence
// for platform ("<os>/<arch>") does not demonstrate. Every capability is
// missing for an unqualified version or one without evidence.
func (v DecoderVersion) MissingCapabilities(platform string, want ...string) []string {
	have := map[string]bool{}
	if v.Qualified {
		for _, ev := range v.Evidence {
			if ev.Platform == platform {
				for _, k := range ev.Kinds {
					have[k] = true
				}
			}
		}
	}
	var missing []string
	for _, k := range want {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	return missing
}

// validateEvidence checks a version's evidence entries: only a qualified
// version carries evidence, each with a platform, a fixture and known,
// unique capabilities.
func (v DecoderVersion) validateEvidence() error {
	if len(v.Evidence) > 0 && !v.Qualified {
		return fmt.Errorf("decoder version %q is not qualified but carries evidence", v.Version)
	}
	seen := map[string]bool{}
	for _, ev := range v.Evidence {
		if !platformRe.MatchString(ev.Platform) || strings.TrimSpace(ev.Fixture) == "" || len(ev.Kinds) == 0 {
			return fmt.Errorf("decoder version %q: evidence needs a platform os/arch, a fixture and capabilities", v.Version)
		}
		if seen[ev.Platform+"\x00"+ev.Fixture] {
			return fmt.Errorf("decoder version %q: duplicate evidence %s %s", v.Version, ev.Platform, ev.Fixture)
		}
		seen[ev.Platform+"\x00"+ev.Fixture] = true
		kinds := map[string]bool{}
		for _, k := range ev.Kinds {
			if !contains(Capabilities, k) || kinds[k] {
				return fmt.Errorf("decoder version %q: unknown or duplicate capability %q", v.Version, k)
			}
			kinds[k] = true
		}
	}
	return nil
}

// platformRe is an evidence platform: a supported OS and an architecture.
var platformRe = regexp.MustCompile(`^(linux|darwin)/[a-z0-9]+$`)

// decoderSpec is one decoder family: its supported versions, the family
// (synthetic) parser, and exact-version overrides (design
// decoder-enrollment B2, FP-17): a version listed in exact decodes with its
// own real parser, never the family's, so conflicting synthetic and real
// formats coexist without format guessing.
type decoderSpec struct {
	versions []DecoderVersion
	decode   func(Transcript) Decoded
	exact    map[string]func(Transcript) Decoded
}

// Registry maps decoder names to their supported versions.
type Registry map[string]decoderSpec

// DefaultRegistry is the production registry: the four captured CLI
// versions (tests/testdata/cli-help), each backed by a synthetic fixture
// only and so unqualified, and the three enrolled real versions of design
// decoder-enrollment B2 (FP-19): exactly these linux/amd64 identities, each
// Qualified with its one indexed real fixture and only the success
// capabilities, decoded by its exact-version real parser (FP-17); and the
// one Cursor Agent 2026.10.01-e373342 linux/amd64 version of design
// decoder-enrollment B3 (FP-22/FP-23), enrolled the same way with the same
// success-only capabilities (its version is admitted to the index through
// realVersionClient alongside this addition).
func DefaultRegistry() Registry {
	r := SyntheticRegistry()
	add := func(name, v string, decode func(Transcript) Decoded) {
		f := EnrolledFixtureID(name, v, EnrolledRealPlatform)
		s := r[name]
		s.versions = append(s.versions, DecoderVersion{Version: v, Fixture: f, Qualified: true,
			Evidence: []DecoderEvidence{{Platform: EnrolledRealPlatform, Fixture: f, Kinds: []string{CapToolCall, CapToolResult, CapTerminalSuccess}}}})
		s.exact = map[string]func(Transcript) Decoded{v: decode}
		r[name] = s
	}
	add("claude-json", ClaudeRealVersion, decodeClaudeReal)
	add("codex-jsonl", CodexRealVersion, decodeCodexReal)
	add("grok-json", GrokRealVersion, decodeGrokReal)
	add("cursor-jsonl", CursorRealVersion, decodeCursorReal)
	return r
}

// SyntheticRegistry is the four captured CLI versions backed by synthetic
// fixtures only, none qualified and without any enrolled real version: the
// legacy registry that tests extend with injected fake evidence (a fake
// fixture never enters the production index, which DefaultRegistry must
// match exactly).
func SyntheticRegistry() Registry {
	synth := func(name, v string) []DecoderVersion {
		return []DecoderVersion{{Version: v, Fixture: name + "/synthetic"}}
	}
	return Registry{
		"claude-json":  {versions: synth("claude-json", "2.1.282 (Claude Code)"), decode: decodeClaude},
		"codex-jsonl":  {versions: synth("codex-jsonl", "codex-cli 0.156.1"), decode: decodeCodex},
		"grok-json":    {versions: synth("grok-json", "grok 1.0.41 (4220f3b224a6) [stable]"), decode: decodeGrok},
		"cursor-jsonl": {versions: synth("cursor-jsonl", "2026.09.23-86fc751"), decode: decodeCursor},
	}
}

// WithVersion returns a copy of r where decoder name also supports v
// (tests register qualified versions this way; production never does).
// The exact-version overrides are copied too, so a test registry keeps the
// real parsers (design decoder-enrollment B2, FP-17).
func (r Registry) WithVersion(name string, v DecoderVersion) Registry {
	out := Registry{}
	for k, s := range r {
		cp := decoderSpec{versions: append([]DecoderVersion(nil), s.versions...), decode: s.decode}
		if s.exact != nil {
			cp.exact = make(map[string]func(Transcript) Decoded, len(s.exact))
			for ver, f := range s.exact {
				cp.exact[ver] = f
			}
		}
		out[k] = cp
	}
	s := out[name]
	s.versions = append(s.versions, v)
	out[name] = s
	return out
}

// Versions returns a copy of decoder name's registered versions.
func (r Registry) Versions(name string) []DecoderVersion {
	return append([]DecoderVersion(nil), r[name].versions...)
}

// Decoder returns decoder name's family (synthetic) transcript decoder
// (nil when unknown): the legacy accessor. Production execution and
// Replay use Select, which honours the exact-version overrides.
func (r Registry) Decoder(name string) func(Transcript) Decoded {
	return r[name].decode
}

func (r Registry) hasFixture(name, fixture string) bool {
	for _, v := range r[name].versions {
		if v.Fixture == fixture {
			return true
		}
	}
	return false
}

// Select returns the decoder for an exact vendor version; there is no
// fallback to another version. A version with an exact-version override
// decodes with that override (design decoder-enrollment B2, FP-17); every
// other supported version uses the family parser. Selection is by the
// exact string only, never by transcript shape, a shortened version, the
// host platform or the model.
func (r Registry) Select(name, version string) (func(Transcript) Decoded, DecoderVersion, error) {
	s, ok := r[name]
	if !ok {
		return nil, DecoderVersion{}, fmt.Errorf("unknown decoder %q", name)
	}
	for _, v := range s.versions {
		if v.Version == version {
			if f := s.exact[version]; f != nil {
				return f, v, nil
			}
			return s.decode, v, nil
		}
	}
	known := make([]string, 0, len(s.versions))
	for _, v := range s.versions {
		known = append(known, v.Version)
	}
	sort.Strings(known)
	return nil, DecoderVersion{}, fmt.Errorf("%s has no decoder for version %q (supported: %s)", name, version, strings.Join(known, ", "))
}

// Decoder-side safe reasons.
const (
	ReasonTruncated    = "transcript_truncated"
	ReasonMalformed    = "transcript_malformed"
	ReasonUnrecognized = "unrecognized_terminal_or_error_event"
	ReasonNoTranscript = "empty_transcript"
	// SafeModelUnavailable marks a session error caused by an unavailable
	// or unknown model.
	SafeModelUnavailable = "model_unavailable"
	mcpTimeoutErrorCode  = -32001
)

var mcpErrorText = regexp.MustCompile(`^MCP error (-?[0-9]+):`)

// correlator maps vendor tool-call IDs to case IDs.
type correlator struct {
	out    Decoded
	cases  map[string]string
	failed string
}

func newCorrelator() *correlator { return &correlator{cases: map[string]string{}} }

func (c *correlator) call(off int64, id, caseID string) {
	c.cases[id] = caseID
	c.out.Events = append(c.out.Events, Event{CaseID: caseID, Kind: KindToolCall, OffsetNS: off, RequestID: id})
}

func (c *correlator) result(off int64, id, text string) {
	var r struct {
		Nonce  string `json:"nonce"`
		CaseID string `json:"case_id"`
	}
	json.Unmarshal([]byte(text), &r)
	caseID := c.cases[id]
	if r.CaseID != "" && caseID == "" {
		caseID = r.CaseID
	}
	ev := Event{CaseID: caseID, Kind: KindToolResult, OffsetNS: off, RequestID: id, Nonce: r.Nonce}
	if r.CaseID != "" && r.CaseID != caseID {
		ev.SafeReason = "result_case_mismatch"
	}
	c.out.Events = append(c.out.Events, ev)
}

func (c *correlator) toolEvent(off int64, id, kind, reason string) {
	c.out.Events = append(c.out.Events, Event{CaseID: c.cases[id], Kind: kind, OffsetNS: off, RequestID: id, SafeReason: reason})
}

func (c *correlator) terminal(off int64, kind, reason string) {
	c.out.Terminal = true
	if kind != "" {
		c.out.Events = append(c.out.Events, Event{Kind: kind, OffsetNS: off, SafeReason: reason})
	}
}

func (c *correlator) unrecognized(what string) {
	if c.failed == "" {
		c.failed = ReasonUnrecognized + ": " + what
	}
}

func (c *correlator) done(t Transcript) Decoded {
	switch {
	case t.Truncated:
		c.out.Inconclusive = ReasonTruncated
	case c.failed != "":
		c.out.Inconclusive = c.failed
	case len(t.Lines) == 0:
		c.out.Inconclusive = ReasonNoTranscript
	}
	return c.out
}

// errorCode classifies an MCP client error text by its structured code:
// -32001 (the MCP request timeout) is an MCP timeout; another code is a
// tool error; anything else is unrecognized.
func errorCode(text string) (kind string, ok bool) {
	m := mcpErrorText.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	if m[1] == fmt.Sprint(mcpTimeoutErrorCode) {
		return KindMCPTimeout, true
	}
	return KindToolError, true
}

// joined is a single-document transcript (a JSON value across lines).
func joined(t Transcript) ([]byte, int64) {
	var buf bytes.Buffer
	var off int64
	for _, l := range t.Lines {
		buf.Write(l.Data)
		buf.WriteByte('\n')
		off = l.OffsetNS
	}
	return buf.Bytes(), off
}

func textOf(items []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var sb strings.Builder
	for _, it := range items {
		if it.Type == "text" {
			sb.WriteString(it.Text)
		}
	}
	return sb.String()
}

// decodeClaude reads claude-json: `claude -p ... --output-format json`
// with verbose message output, one JSON array of messages. Tool calls are
// assistant tool_use blocks whose name ends in "__slow"; their outcomes
// are user tool_result blocks (is_error with "MCP error <code>:" text);
// the final result message's subtype is the terminal status.
func decodeClaude(t Transcript) Decoded {
	c := newCorrelator()
	doc, off := joined(t)
	var msgs []struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if len(t.Lines) > 0 && !t.Truncated && json.Unmarshal(doc, &msgs) != nil {
		c.failed = ReasonMalformed
		return c.done(t)
	}
	for _, m := range msgs {
		switch m.Type {
		case "assistant":
			for _, b := range m.Message.Content {
				if b.Type == "tool_use" && strings.HasSuffix(b.Name, "__slow") {
					c.call(off, b.ID, caseArg(b.Input))
				}
			}
		case "user":
			for _, b := range m.Message.Content {
				if b.Type != "tool_result" {
					continue
				}
				text := textOf(b.Content)
				if !b.IsError {
					c.result(off, b.ToolUseID, text)
				} else if kind, ok := errorCode(text); ok {
					c.toolEvent(off, b.ToolUseID, kind, kind)
				} else {
					c.unrecognized("claude tool_result error")
				}
			}
		case "result":
			switch m.Subtype {
			case "success":
				c.terminal(off, "", "")
			case "error_authentication":
				c.terminal(off, KindAuthError, m.Subtype)
			case "error_permission_denied":
				c.terminal(off, KindPermissionDenied, m.Subtype)
			case "error_refusal":
				c.terminal(off, KindModelRefusal, m.Subtype)
			case "error_during_execution", "error_max_turns":
				c.terminal(off, KindSessionError, m.Subtype)
			case "error_model_unavailable":
				c.terminal(off, KindSessionError, SafeModelUnavailable)
			default:
				c.terminal(off, "", "")
				c.unrecognized("claude result subtype")
			}
		}
	}
	return c.done(t)
}

func caseArg(raw json.RawMessage) string {
	var a struct {
		CaseID string `json:"case_id"`
	}
	json.Unmarshal(raw, &a)
	return a.CaseID
}

// decodeCodex reads codex-jsonl: `codex exec ... --json` events, one JSON
// object per line. mcp_tool_call items start (item.started) and complete
// (item.completed, status completed or failed with a structured error
// code); turn.completed, turn.failed and error are terminal.
func decodeCodex(t Transcript) Decoded {
	c := newCorrelator()
	for _, l := range t.Lines {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				ID        string          `json:"id"`
				Type      string          `json:"type"`
				Tool      string          `json:"tool"`
				Arguments json.RawMessage `json:"arguments"`
				Status    string          `json:"status"`
				Result    struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"item"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(l.Data, &ev) != nil {
			c.failed = ReasonMalformed
			break
		}
		it := ev.Item
		switch ev.Type {
		case "item.started":
			if it.Type == "mcp_tool_call" && it.Tool == "slow" {
				c.call(l.OffsetNS, it.ID, caseArg(it.Arguments))
			}
		case "item.completed":
			if it.Type != "mcp_tool_call" || it.Tool != "slow" {
				continue
			}
			switch {
			case it.Status == "completed":
				c.result(l.OffsetNS, it.ID, textOf(it.Result.Content))
			case it.Status == "failed" && it.Error.Code == "timeout":
				c.toolEvent(l.OffsetNS, it.ID, KindMCPTimeout, "timeout")
			case it.Status == "failed" && it.Error.Code == "approval_denied":
				c.toolEvent(l.OffsetNS, it.ID, KindPermissionDenied, "approval_denied")
			case it.Status == "failed" && it.Error.Code == "tool_error":
				c.toolEvent(l.OffsetNS, it.ID, KindToolError, "tool_error")
			default:
				c.unrecognized("codex mcp_tool_call status")
			}
		case "turn.completed":
			c.terminal(l.OffsetNS, "", "")
		case "turn.failed", "error":
			switch ev.Error.Code {
			case "unauthorized":
				c.terminal(l.OffsetNS, KindAuthError, "unauthorized")
			case "refusal":
				c.terminal(l.OffsetNS, KindModelRefusal, "refusal")
			case "stream_error", "context_limit":
				c.terminal(l.OffsetNS, KindSessionError, ev.Error.Code)
			case "model_not_found":
				c.terminal(l.OffsetNS, KindSessionError, SafeModelUnavailable)
			default:
				c.terminal(l.OffsetNS, "", "")
				c.unrecognized("codex error code")
			}
		}
	}
	return c.done(t)
}

// decodeGrok reads grok-json: `grok -p ... --output-format json`, one JSON
// object with an events array (tool_call, tool_result with a structured
// error kind) and a terminal status.
func decodeGrok(t Transcript) Decoded {
	c := newCorrelator()
	doc, off := joined(t)
	var d struct {
		Status string `json:"status"`
		Error  struct {
			Kind string `json:"kind"`
		} `json:"error"`
		Events []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Content   string          `json:"content"`
			Error     *struct {
				Kind string `json:"kind"`
			} `json:"error"`
		} `json:"events"`
	}
	if len(t.Lines) > 0 && !t.Truncated && json.Unmarshal(doc, &d) != nil {
		c.failed = ReasonMalformed
		return c.done(t)
	}
	for _, e := range d.Events {
		switch e.Type {
		case "tool_call":
			if e.Name == "slow" || strings.HasSuffix(e.Name, "__slow") {
				c.call(off, e.ID, caseArg(e.Arguments))
			}
		case "tool_result":
			switch {
			case e.Error == nil:
				c.result(off, e.ID, e.Content)
			case e.Error.Kind == "mcp_timeout":
				c.toolEvent(off, e.ID, KindMCPTimeout, "mcp_timeout")
			case e.Error.Kind == "permission_denied":
				c.toolEvent(off, e.ID, KindPermissionDenied, "permission_denied")
			case e.Error.Kind == "tool_error":
				c.toolEvent(off, e.ID, KindToolError, "tool_error")
			default:
				c.unrecognized("grok tool_result error kind")
			}
		}
	}
	if len(t.Lines) > 0 && !t.Truncated {
		switch {
		case d.Status == "success":
			c.terminal(off, "", "")
		case d.Status == "error" && d.Error.Kind == "auth":
			c.terminal(off, KindAuthError, "auth")
		case d.Status == "error" && d.Error.Kind == "refusal":
			c.terminal(off, KindModelRefusal, "refusal")
		case d.Status == "error" && d.Error.Kind == "session":
			c.terminal(off, KindSessionError, "session")
		case d.Status == "error" && d.Error.Kind == "model":
			c.terminal(off, KindSessionError, SafeModelUnavailable)
		default:
			c.terminal(off, "", "")
			c.unrecognized("grok terminal status")
		}
	}
	return c.done(t)
}

// decodeCursor reads cursor-jsonl: `cursor-agent -p ... --output-format
// stream-json`, one JSON object per line. tool_call started/completed
// events carry an mcpToolCall with success or a structured error code; the
// result event is terminal.
func decodeCursor(t Transcript) Decoded {
	c := newCorrelator()
	for _, l := range t.Lines {
		var ev struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			CallID    string `json:"call_id"`
			ErrorKind string `json:"error_kind"`
			ToolCall  struct {
				MCP *struct {
					Args struct {
						ToolName string          `json:"toolName"`
						Args     json.RawMessage `json:"args"`
					} `json:"args"`
					Result *struct {
						Success *struct {
							Content []struct {
								Type string `json:"type"`
								Text string `json:"text"`
							} `json:"content"`
						} `json:"success"`
						Error *struct {
							Code string `json:"code"`
						} `json:"error"`
					} `json:"result"`
				} `json:"mcpToolCall"`
			} `json:"tool_call"`
		}
		if json.Unmarshal(l.Data, &ev) != nil {
			c.failed = ReasonMalformed
			break
		}
		switch ev.Type {
		case "tool_call":
			m := ev.ToolCall.MCP
			if m == nil {
				continue
			}
			switch ev.Subtype {
			case "started":
				if m.Args.ToolName == "slow" {
					c.call(l.OffsetNS, ev.CallID, caseArg(m.Args.Args))
				}
			case "completed":
				if _, ok := c.cases[ev.CallID]; !ok {
					continue
				}
				switch r := m.Result; {
				case r != nil && r.Success != nil:
					c.result(l.OffsetNS, ev.CallID, textOf(r.Success.Content))
				case r != nil && r.Error != nil && r.Error.Code == "timeout":
					c.toolEvent(l.OffsetNS, ev.CallID, KindMCPTimeout, "timeout")
				case r != nil && r.Error != nil && r.Error.Code == "rejected":
					c.toolEvent(l.OffsetNS, ev.CallID, KindPermissionDenied, "rejected")
				case r != nil && r.Error != nil && r.Error.Code == "tool_error":
					c.toolEvent(l.OffsetNS, ev.CallID, KindToolError, "tool_error")
				default:
					c.unrecognized("cursor tool_call result")
				}
			}
		case "result":
			switch {
			case ev.Subtype == "success":
				c.terminal(l.OffsetNS, "", "")
			case ev.Subtype == "error" && ev.ErrorKind == "authentication":
				c.terminal(l.OffsetNS, KindAuthError, "authentication")
			case ev.Subtype == "error" && ev.ErrorKind == "refusal":
				c.terminal(l.OffsetNS, KindModelRefusal, "refusal")
			case ev.Subtype == "error" && ev.ErrorKind == "session":
				c.terminal(l.OffsetNS, KindSessionError, "session")
			case ev.Subtype == "error" && ev.ErrorKind == "model":
				c.terminal(l.OffsetNS, KindSessionError, SafeModelUnavailable)
			default:
				c.terminal(l.OffsetNS, "", "")
				c.unrecognized("cursor result")
			}
		}
	}
	return c.done(t)
}
