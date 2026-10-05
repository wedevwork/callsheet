package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Vendor final-message extraction (iteration 08, extended by iteration
// 11). Every extractor bounds what it keeps by MaxVendorFinalBytes, owns
// every retained byte, seals at Finish (idempotent, owned values) and
// ignores Feed afterwards.

// claudeExtractor buffers a stdout JSON result document (at most
// MaxVendorFinalBytes) and at Finish requires exactly one UTF-8 JSON
// object followed only by whitespace: no duplicate top-level key, "type"
// the string "result", "is_error" a boolean and "result" a string,
// returned exactly (an empty string included) even when is_error is true.
// Unknown metadata fields (subtype, usage included) are ignored and never
// decide the task's status. Overflow discards the input and reports
// FinalTooLarge; anything else malformed (empty stdout included)
// FinalInvalid. It is the common result schema of Claude's stdout and
// (iteration 11) Cursor's captured json output: Cursor's extractor is this
// one, with no Grok shape, plain text or stream event accepted.
type claudeExtractor struct {
	buf    []byte
	over   bool
	sealed bool
	out    FinalMessage
}

func (c *claudeExtractor) Feed(b []byte) {
	if c.sealed || c.over || len(b) == 0 {
		return
	}
	if len(b) > MaxVendorFinalBytes-len(c.buf) {
		c.over, c.buf = true, nil
		return
	}
	c.buf = append(c.buf, b...)
}

func (c *claudeExtractor) Finish() FinalMessage {
	if !c.sealed {
		c.sealed = true
		switch {
		case c.over:
			c.out = FinalMessage{Error: FinalTooLarge}
		default:
			if s, ok := parseClaudeResult(c.buf); ok {
				c.out = truncatedMessage(s)
			} else {
				c.out = FinalMessage{Error: FinalInvalid}
			}
		}
		c.buf = nil
	}
	return c.out.clone()
}

// clone returns an owned copy of m.
func (m FinalMessage) clone() FinalMessage {
	out := m
	if m.Message != nil {
		s := *m.Message
		out.Message = &s
	}
	return out
}

// truncatedMessage is s as a final message: its first
// contract.MaxFinalMessageBytes bytes ending at a UTF-8 boundary, marked
// truncated when shortened.
func truncatedMessage(s string) FinalMessage {
	if len(s) <= contract.MaxFinalMessageBytes {
		return FinalMessage{Message: &s}
	}
	cut := contract.MaxFinalMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	t := s[:cut]
	return FinalMessage{Message: &t, Truncated: true}
}

// parseClaudeResult decodes Claude's result object strictly.
func parseClaudeResult(b []byte) (string, bool) {
	if !utf8.Valid(b) || !pairedEscapes(b) {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	seen := map[string]bool{}
	var typ, result *string
	var isErr *bool
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok || seen[key] {
			return "", false
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", false
		}
		switch key {
		case "type", "result":
			s, ok := contract.JSONString(raw)
			if !ok {
				return "", false
			}
			if key == "type" {
				typ = &s
			} else {
				result = &s
			}
		case "is_error":
			var v bool
			if t := bytes.TrimSpace(raw); !bytes.Equal(t, []byte("true")) && !bytes.Equal(t, []byte("false")) || json.Unmarshal(raw, &v) != nil {
				return "", false
			}
			isErr = &v
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", false
	}
	// Only whitespace may follow the object.
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	if typ == nil || *typ != "result" || isErr == nil || result == nil {
		return "", false
	}
	return *result, true
}

// grokExtractor (iteration 11) buffers Grok's stdout (at most
// MaxVendorFinalBytes) and at Finish requires exactly one UTF-8 JSON object
// followed only by whitespace, with no duplicate top-level key, in one of
// two mutually exclusive shapes: normal, a string "text" and a string
// "stopReason" (any value: it has no lifecycle meaning) and neither "type"
// nor "message", whose answer is the decoded text; or error, "type" the
// string "error" and a string "message" and neither "text" nor
// "stopReason", whose answer is the decoded message. Either may be empty.
// Other metadata (thought, usage, session and request IDs, modelUsage) is
// ignored and never extracted; it stays in the bounded stdout log only.
// Overflow reports FinalTooLarge; anything else malformed, a missing, null
// or wrong-typed required field and an ambiguous mixture FinalInvalid. A
// stopReason of cancelled is not a Callsheet cancellation.
type grokExtractor struct {
	buf    []byte
	over   bool
	sealed bool
	out    FinalMessage
}

func (g *grokExtractor) Feed(b []byte) {
	if g.sealed || g.over || len(b) == 0 {
		return
	}
	if len(b) > MaxVendorFinalBytes-len(g.buf) {
		g.over, g.buf = true, nil
		return
	}
	g.buf = append(g.buf, b...)
}

func (g *grokExtractor) Finish() FinalMessage {
	if !g.sealed {
		g.sealed = true
		switch {
		case g.over:
			g.out = FinalMessage{Error: FinalTooLarge}
		default:
			if s, ok := parseGrokResult(g.buf); ok {
				g.out = truncatedMessage(s)
			} else {
				g.out = FinalMessage{Error: FinalInvalid}
			}
		}
		g.buf = nil
	}
	return g.out.clone()
}

// grokField is one of Grok's four shape fields: present, and its decoded
// string when it is one.
type grokField struct {
	present, str bool
	s            string
}

// parseGrokResult decodes Grok's one JSON object strictly.
func parseGrokResult(b []byte) (string, bool) {
	if !utf8.Valid(b) || !pairedEscapes(b) {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	seen := map[string]bool{}
	fields := map[string]*grokField{"text": {}, "stopReason": {}, "type": {}, "message": {}}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok || seen[key] {
			return "", false
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", false
		}
		if f, ok := fields[key]; ok {
			f.present = true
			f.s, f.str = contract.JSONString(raw)
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", false
	}
	// Only whitespace may follow the object.
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	text, stop, typ, msg := fields["text"], fields["stopReason"], fields["type"], fields["message"]
	switch {
	case text.present || stop.present:
		if text.str && stop.str && !typ.present && !msg.present {
			return text.s, true
		}
	case typ.present || msg.present:
		if typ.str && typ.s == "error" && msg.str {
			return msg.s, true
		}
	}
	return "", false
}

// pairedEscapes reports whether every \u surrogate escape in the JSON text
// b is a high surrogate immediately followed by a low one (encoding/json
// would silently substitute U+FFFD otherwise). Backslashes occur only
// inside strings of valid JSON; invalid JSON is rejected by the decoder.
func pairedEscapes(b []byte) bool {
	hex4 := func(p []byte) (rune, bool) {
		if len(p) < 4 {
			return 0, false
		}
		var r rune
		for _, c := range p[:4] {
			r <<= 4
			switch {
			case c >= '0' && c <= '9':
				r |= rune(c - '0')
			case c >= 'a' && c <= 'f':
				r |= rune(c-'a') + 10
			case c >= 'A' && c <= 'F':
				r |= rune(c-'A') + 10
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := bytes.IndexByte(b, '\\'); i >= 0 && i < len(b); {
		if i+1 >= len(b) {
			return false
		}
		if b[i+1] != 'u' {
			i += 2
		} else {
			r, ok := hex4(b[i+2:])
			switch {
			case !ok, r >= 0xDC00 && r <= 0xDFFF:
				return false
			case r >= 0xD800 && r <= 0xDBFF:
				j := i + 6
				if j+1 >= len(b) || b[j] != '\\' || b[j+1] != 'u' {
					return false
				}
				if lo, ok := hex4(b[j+2:]); !ok || lo < 0xDC00 || lo > 0xDFFF {
					return false
				}
				i = j + 6
			default:
				i += 6
			}
		}
		if i >= len(b) {
			break
		}
		next := bytes.IndexByte(b[i:], '\\')
		if next < 0 {
			break
		}
		i += next
	}
	return true
}

// rawExtractor is Codex's final-file extractor: the file's raw bytes are
// the answer, validated as UTF-8 across chunk boundaries (no trimming,
// newline or Unicode normalization, no JSON). It retains at most the first
// contract.MaxFinalMessageBytes bytes ending at a rune boundary plus an
// incomplete rune's state, and checks the remainder up to
// MaxVendorFinalBytes without keeping it. An empty input is the valid
// empty answer; beyond the bound it reports FinalTooLarge, invalid UTF-8
// (a rune cut off at the end included) FinalInvalid.
type rawExtractor struct {
	kept      []byte
	full      bool // the retained prefix is complete: later bytes are only checked
	truncated bool
	pend      [utf8.UTFMax]byte
	npend     int
	total     int
	over, bad bool
	sealed    bool
	out       FinalMessage
}

func (x *rawExtractor) Feed(b []byte) {
	if x.sealed || x.over || x.bad || len(b) == 0 {
		return
	}
	if len(b) > MaxVendorFinalBytes-x.total {
		x.over, x.kept = true, nil
		return
	}
	x.total += len(b)
	// Complete a rune split by the previous chunk.
	for x.npend > 0 && len(b) > 0 {
		x.pend[x.npend] = b[0]
		x.npend++
		b = b[1:]
		if !utf8.FullRune(x.pend[:x.npend]) {
			continue
		}
		r, n := utf8.DecodeRune(x.pend[:x.npend])
		if (r == utf8.RuneError && n == 1) || n != x.npend {
			x.bad, x.kept = true, nil
			return
		}
		x.keep(x.pend[:n])
		x.npend = 0
	}
	if len(b) == 0 {
		return
	}
	// Hold back an incomplete rune at the chunk's end.
	end := len(b)
	i := end - 1
	for i > 0 && i > end-utf8.UTFMax && !utf8.RuneStart(b[i]) {
		i--
	}
	if utf8.RuneStart(b[i]) && !utf8.FullRune(b[i:end]) {
		end = i
	}
	body := b[:end]
	if !utf8.Valid(body) {
		x.bad, x.kept = true, nil
		return
	}
	x.keep(body)
	x.npend = copy(x.pend[:], b[end:])
}

// keep appends valid UTF-8 text to the retained prefix while it fits,
// cutting at a rune boundary and marking the answer truncated once it
// does not.
func (x *rawExtractor) keep(p []byte) {
	if x.full {
		return
	}
	room := contract.MaxFinalMessageBytes - len(x.kept)
	if len(p) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(p[cut]) {
			cut--
		}
		p = p[:cut]
		x.full, x.truncated = true, true
	}
	if need := len(x.kept) + len(p); need > cap(x.kept) {
		grown := make([]byte, len(x.kept), min(max(2*cap(x.kept), need, 4096), contract.MaxFinalMessageBytes))
		copy(grown, x.kept)
		x.kept = grown
	}
	x.kept = append(x.kept, p...)
}

func (x *rawExtractor) Finish() FinalMessage {
	if !x.sealed {
		x.sealed = true
		switch {
		case x.over:
			x.out = FinalMessage{Error: FinalTooLarge}
		case x.bad || x.npend > 0:
			x.out = FinalMessage{Error: FinalInvalid}
		default:
			s := string(x.kept)
			x.out = FinalMessage{Message: &s, Truncated: x.truncated}
		}
		x.kept = nil
	}
	return x.out.clone()
}
