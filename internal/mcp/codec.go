package mcp

// The wire codec (FP-1): message classification and request IDs, the
// frames the writer sends (compact JSON-RPC lines, tool results wrapped in
// one escaped text item, size-checked whole before they are built) and
// the strict scanner that validates every inbound line before decoding.
// Nothing here holds state or locks.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// ---- Messages ----

const (
	kindRequest = iota
	kindNotification
	kindResponse
)

// requestID is a validated request ID: its canonical JSON (type and
// value preserved) and a type-tagged key.
type requestID struct {
	raw []byte
	key string
}

type message struct {
	kind   int
	id     *requestID
	method string
	params json.RawMessage
}

// protoErr is a protocol-level answer to an invalid message; silent for a
// notification, which never gets a reply.
type protoErr struct {
	code   int
	msg    string
	id     *requestID
	silent bool
}

var envelopeKeys = map[string]bool{"jsonrpc": true, "id": true, "method": true, "params": true, "result": true, "error": true}

// parseMessage validates one line: UTF-8, JSON syntax, nesting depth,
// duplicate keys and escapes recursively, then the JSON-RPC 2.0
// envelope. It classifies requests, notifications and responses.
func parseMessage(line []byte) (message, *protoErr) {
	if !utf8.Valid(line) {
		return message{}, &protoErr{code: codeParse, msg: "parse error: the line is not valid UTF-8"}
	}
	if err := checkJSON(line); err != nil {
		if json.Valid(line) && looksLikeNotification(line) {
			return message{}, &protoErr{silent: true}
		}
		return message{}, &protoErr{code: codeParse, msg: "parse error: " + err.Error()}
	}
	trimmed := bytes.TrimLeft(line, " \t\r")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return message{}, &protoErr{code: codeInvalidRequest, msg: "invalid request: a message is one JSON object (batches are not supported)"}
	}
	var o map[string]json.RawMessage
	if err := json.Unmarshal(line, &o); err != nil {
		return message{}, &protoErr{code: codeParse, msg: "parse error: malformed JSON"}
	}
	idRaw, hasID := o["id"]
	methodRaw, hasMethod := o["method"]
	_, hasResult := o["result"]
	_, hasError := o["error"]
	var id *requestID
	if hasID {
		if v, ok := parseID(idRaw); ok {
			id = &v
		}
	}
	if !hasMethod {
		if hasID && (hasResult || hasError) {
			return message{kind: kindResponse}, nil
		}
		return message{}, &protoErr{code: codeInvalidRequest, msg: "invalid request: no method", id: id}
	}
	notification := !hasID
	if hasID && id == nil {
		return message{}, &protoErr{code: codeInvalidRequest, msg: "invalid request: the id must be a string of at most 128 bytes or an integer within ±(2^53-1)"}
	}
	bad := func(code int, msg string) (message, *protoErr) {
		return message{}, &protoErr{code: code, msg: msg, id: id, silent: notification}
	}
	for k := range o {
		if !envelopeKeys[k] {
			return bad(codeInvalidRequest, "invalid request: unknown member "+quoteName(k))
		}
	}
	if v, ok := contract.JSONString(bytes.TrimSpace(o["jsonrpc"])); !ok || v != "2.0" {
		return bad(codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
	}
	method, ok := contract.JSONString(bytes.TrimSpace(methodRaw))
	if !ok {
		return bad(codeInvalidRequest, "invalid request: method must be a string")
	}
	if hasResult || hasError {
		return bad(codeInvalidRequest, "invalid request: a request carries no result or error")
	}
	params, hasParams := o["params"]
	if hasParams && !isObject(params) {
		return bad(codeInvalidParams, "invalid params: params must be an object")
	}
	kind := kindRequest
	if notification {
		kind = kindNotification
	}
	return message{kind: kind, id: id, method: method, params: params}, nil
}

// looksLikeNotification reports whether valid JSON is an object with a
// method and without an id (it gets no reply even when malformed).
func looksLikeNotification(line []byte) bool {
	var o map[string]json.RawMessage
	if json.Unmarshal(line, &o) != nil {
		return false
	}
	_, hasID := o["id"]
	_, hasMethod := o["method"]
	return hasMethod && !hasID
}

// parseID validates a request ID: a string of at most MaxIDBytes UTF-8
// bytes or an exact integer within ±(2^53-1); null, fractions, exponents
// and other types are invalid.
func parseID(raw json.RawMessage) (requestID, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return requestID{}, false
	}
	if raw[0] == '"' {
		s, ok := contract.JSONString(raw)
		if !ok || len(s) > MaxIDBytes {
			return requestID{}, false
		}
		enc, err := contract.Encode(s)
		if err != nil {
			return requestID{}, false
		}
		return requestID{raw: enc, key: "s" + s}, true
	}
	n, ok := contract.ParseInteger(string(raw))
	if !ok || n > maxSafeInteger || n < -maxSafeInteger {
		return requestID{}, false
	}
	v := strconv.Itoa(n)
	return requestID{raw: []byte(v), key: "n" + v}, true
}

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// members splits a validated JSON object (duplicates were rejected by
// checkJSON).
func members(raw json.RawMessage) map[string]json.RawMessage {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil || o == nil {
		return map[string]json.RawMessage{}
	}
	return o
}

// ---- Encoding ----

const (
	framePrefix   = `{"jsonrpc":"2.0","id":`
	resultOpen    = `,"result":{"content":[{"type":"text","text":"`
	resultCloseOK = `"}],"isError":false}}` + "\n"
	resultCloseEr = `"}],"isError":true}}` + "\n"
)

// resultFrame is a successful protocol answer carrying raw JSON.
func resultFrame(id requestID, result []byte) []byte {
	b := make([]byte, 0, len(framePrefix)+len(id.raw)+len(result)+16)
	b = append(b, framePrefix...)
	b = append(b, id.raw...)
	b = append(b, `,"result":`...)
	b = append(b, result...)
	return append(b, "}\n"...)
}

// protocolError is a JSON-RPC error answer (a null id when none can be
// recovered).
func protocolError(id *requestID, code int, msg string) []byte {
	idRaw := []byte("null")
	if id != nil {
		idRaw = id.raw
	}
	m, _ := contract.Encode(msg)
	b := make([]byte, 0, 64+len(idRaw)+len(m))
	b = append(b, framePrefix...)
	b = append(b, idRaw...)
	b = append(b, `,"error":{"code":`...)
	b = strconv.AppendInt(b, int64(code), 10)
	b = append(b, `,"message":`...)
	b = append(b, m...)
	return append(b, "}}\n"...)
}

// textFrameSize is the exact size of a tool answer frame around text.
func textFrameSize(id requestID, text []byte, isError bool) int {
	closer := len(resultCloseOK)
	if isError {
		closer = len(resultCloseEr)
	}
	return len(framePrefix) + len(id.raw) + len(resultOpen) + escapedLen(text) + closer
}

// textFrame wraps text as the single text content item of a tool answer.
func textFrame(id requestID, text []byte, isError bool) []byte {
	b := make([]byte, 0, textFrameSize(id, text, isError))
	b = append(b, framePrefix...)
	b = append(b, id.raw...)
	b = append(b, resultOpen...)
	b = appendEscaped(b, text)
	if isError {
		return append(b, resultCloseEr...)
	}
	return append(b, resultCloseOK...)
}

// successFrame is a tool's successful answer, size-checked whole before
// it is built: over its bound it becomes a small internal tool error,
// never a truncated object.
func successFrame(id requestID, res toolResult) []byte {
	limit := MaxFrameBytes
	if res.wireLimit > 0 {
		limit = res.wireLimit
	}
	if n := textFrameSize(id, res.text, false); n > limit {
		return errorResultFrame(id, contract.New(contract.CodeInternal,
			"the answer of "+strconv.Itoa(n)+" bytes exceeds its "+strconv.Itoa(limit)+"-byte bound; nothing was truncated"))
	}
	return textFrame(id, res.text, false)
}

// errorResultFrame is a tool error answer: the safe serialized contract
// error, or the fixed internal error when that exceeds its bound.
func errorResultFrame(id requestID, e *contract.Error) []byte {
	text := encodeToolError(e)
	if textFrameSize(id, text, true) > MaxErrorFrameBytes {
		text = encodeToolError(internalError())
	}
	return textFrame(id, text, true)
}

const hexDigits = "0123456789abcdef"

// Escaping. A JSON string body escapes '"', '\\', the C0 controls and
// DEL; C1 controls (U+0080-U+009F) and U+2028/U+2029 are escaped too, so
// no raw terminal control or line separator reaches stdout. Runs of plain
// bytes are found eight at a time.

const (
	swarOnes  = 0x0101010101010101
	swarHighs = 0x8080808080808080
)

func swarHasZero(v uint64) bool { return (v-swarOnes)&^v&swarHighs != 0 }

// swarSpecial reports whether any byte of w may need escaping (a control,
// '"', '\\', DEL or the lead byte 0xc2 or 0xe2 of a sequence that might).
func swarSpecial(w uint64) bool {
	return (w-swarOnes*0x20)&^w&swarHighs != 0 || swarHasZero(w^(swarOnes*'"')) || swarHasZero(w^(swarOnes*'\\')) ||
		swarHasZero(w^(swarOnes*0x7f)) || swarHasZero(w^(swarOnes*0xc2)) || swarHasZero(w^(swarOnes*0xe2))
}

func plainByte(c byte) bool {
	return c >= 0x20 && c != '"' && c != '\\' && c != 0x7f && c != 0xc2 && c != 0xe2
}

// plainRun is the length of text's leading run of bytes that are copied
// unchanged.
func plainRun(text []byte) int {
	i := 0
	for i+8 <= len(text) && !swarSpecial(binary.LittleEndian.Uint64(text[i:])) {
		i += 8
	}
	for i < len(text) && plainByte(text[i]) {
		i++
	}
	return i
}

// escapeAt describes the sequence starting at text[i]: the length of its
// escape (0: the byte is copied as is) and of the sequence itself.
func escapeAt(text []byte, i int) (escLen, seqLen int) {
	switch c := text[i]; {
	case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t':
		return 2, 1
	case c < 0x20 || c == 0x7f:
		return 6, 1
	case c == 0xc2 && i+1 < len(text) && text[i+1] >= 0x80 && text[i+1] <= 0x9f:
		return 6, 2
	case c == 0xe2 && i+2 < len(text) && text[i+1] == 0x80 && (text[i+2] == 0xa8 || text[i+2] == 0xa9):
		return 6, 3
	}
	return 0, 1
}

// shortEscape is the letter of a two-byte escape.
func shortEscape(c byte) byte {
	switch c {
	case '\n':
		return 'n'
	case '\r':
		return 'r'
	case '\t':
		return 't'
	}
	return c
}

// escapedLen is the length of text escaped as a JSON string body.
func escapedLen(text []byte) int {
	n := 0
	for i := 0; i < len(text); {
		r := plainRun(text[i:])
		n += r
		if i += r; i >= len(text) {
			break
		}
		e, k := escapeAt(text, i)
		if e == 0 {
			e = 1
		}
		n += e
		i += k
	}
	return n
}

// appendEscaped appends text as a JSON string body. The text is valid
// UTF-8 JSON produced by the contract encoder.
func appendEscaped(b, text []byte) []byte {
	for i := 0; i < len(text); {
		r := plainRun(text[i:])
		b = append(b, text[i:i+r]...)
		if i += r; i >= len(text) {
			break
		}
		e, k := escapeAt(text, i)
		c := text[i]
		switch {
		case e == 0:
			b = append(b, c)
		case e == 2:
			b = append(b, '\\', shortEscape(c))
		case k == 1:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		case k == 2:
			d := text[i+1]
			b = append(b, '\\', 'u', '0', '0', hexDigits[d>>4], hexDigits[d&0xf])
		default:
			b = append(b, '\\', 'u', '2', '0', '2', hexDigits[text[i+2]-0xa0])
		}
		i += k
	}
	return b
}

// ---- Strict JSON scanning ----

// checkJSON validates one JSON text beyond encoding/json: nesting depth
// at most MaxDepth, no duplicate object keys at any level (compared after
// unescaping), every \u surrogate escape paired, and no trailing data.
// The input is valid UTF-8.
func checkJSON(b []byte) error {
	sc := &jsonScanner{b: b}
	sc.ws()
	if err := sc.value(0); err != nil {
		return err
	}
	sc.ws()
	if sc.i != len(sc.b) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

type jsonScanner struct {
	b []byte
	i int
}

var errSyntax = errors.New("malformed JSON")

func (sc *jsonScanner) ws() {
	for sc.i < len(sc.b) {
		switch sc.b[sc.i] {
		case ' ', '\t', '\r', '\n':
			sc.i++
		default:
			return
		}
	}
}

func (sc *jsonScanner) value(depth int) error {
	if sc.i >= len(sc.b) {
		return errSyntax
	}
	switch c := sc.b[sc.i]; {
	case c == '{':
		return sc.object(depth + 1)
	case c == '[':
		return sc.array(depth + 1)
	case c == '"':
		_, err := sc.str(false)
		return err
	case c == 't':
		return sc.literal("true")
	case c == 'f':
		return sc.literal("false")
	case c == 'n':
		return sc.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return sc.number()
	}
	return errSyntax
}

func (sc *jsonScanner) literal(lit string) error {
	if !bytes.HasPrefix(sc.b[sc.i:], []byte(lit)) {
		return errSyntax
	}
	sc.i += len(lit)
	return nil
}

func (sc *jsonScanner) object(depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("nesting deeper than %d levels", MaxDepth)
	}
	sc.i++
	sc.ws()
	if sc.i < len(sc.b) && sc.b[sc.i] == '}' {
		sc.i++
		return nil
	}
	seen := map[string]bool{}
	for {
		sc.ws()
		if sc.i >= len(sc.b) || sc.b[sc.i] != '"' {
			return errSyntax
		}
		key, err := sc.str(true)
		if err != nil {
			return err
		}
		if seen[key] {
			return errors.New("duplicate object key " + quoteName(key))
		}
		seen[key] = true
		sc.ws()
		if sc.i >= len(sc.b) || sc.b[sc.i] != ':' {
			return errSyntax
		}
		sc.i++
		sc.ws()
		if err := sc.value(depth); err != nil {
			return err
		}
		sc.ws()
		if sc.i >= len(sc.b) {
			return errSyntax
		}
		switch sc.b[sc.i] {
		case ',':
			sc.i++
		case '}':
			sc.i++
			return nil
		default:
			return errSyntax
		}
	}
}

func (sc *jsonScanner) array(depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("nesting deeper than %d levels", MaxDepth)
	}
	sc.i++
	sc.ws()
	if sc.i < len(sc.b) && sc.b[sc.i] == ']' {
		sc.i++
		return nil
	}
	for {
		sc.ws()
		if err := sc.value(depth); err != nil {
			return err
		}
		sc.ws()
		if sc.i >= len(sc.b) {
			return errSyntax
		}
		switch sc.b[sc.i] {
		case ',':
			sc.i++
		case ']':
			sc.i++
			return nil
		default:
			return errSyntax
		}
	}
}

// str scans a string token; with decode it returns the unescaped value.
func (sc *jsonScanner) str(decode bool) (string, error) {
	start := sc.i
	sc.i++
	escaped := false
	for sc.i < len(sc.b) {
		c := sc.b[sc.i]
		switch {
		case c == '"':
			sc.i++
			tok := sc.b[start:sc.i]
			if !decode {
				return "", nil
			}
			if !escaped {
				return string(tok[1 : len(tok)-1]), nil
			}
			s, ok := contract.JSONString(tok)
			if !ok {
				return "", errSyntax
			}
			return s, nil
		case c < 0x20:
			return "", errors.New("a control character inside a string")
		case c == '\\':
			escaped = true
			if sc.i+1 >= len(sc.b) {
				return "", errSyntax
			}
			switch sc.b[sc.i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				sc.i += 2
			case 'u':
				r, ok := hex4(sc.b[sc.i+2:])
				if !ok {
					return "", errSyntax
				}
				sc.i += 6
				switch {
				case r >= 0xdc00 && r <= 0xdfff:
					return "", errors.New("an unpaired surrogate escape")
				case r >= 0xd800 && r <= 0xdbff:
					if sc.i+1 >= len(sc.b) || sc.b[sc.i] != '\\' || sc.b[sc.i+1] != 'u' {
						return "", errors.New("an unpaired surrogate escape")
					}
					lo, ok := hex4(sc.b[sc.i+2:])
					if !ok || lo < 0xdc00 || lo > 0xdfff {
						return "", errors.New("an unpaired surrogate escape")
					}
					sc.i += 6
				}
			default:
				return "", errSyntax
			}
		default:
			sc.i++
		}
	}
	return "", errSyntax
}

func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
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

func (sc *jsonScanner) number() error {
	digits := func() int {
		n := 0
		for sc.i < len(sc.b) && sc.b[sc.i] >= '0' && sc.b[sc.i] <= '9' {
			sc.i++
			n++
		}
		return n
	}
	if sc.b[sc.i] == '-' {
		sc.i++
	}
	if sc.i < len(sc.b) && sc.b[sc.i] == '0' {
		sc.i++
	} else if digits() == 0 {
		return errSyntax
	}
	if sc.i < len(sc.b) && sc.b[sc.i] == '.' {
		sc.i++
		if digits() == 0 {
			return errSyntax
		}
	}
	if sc.i < len(sc.b) && (sc.b[sc.i] == 'e' || sc.b[sc.i] == 'E') {
		sc.i++
		if sc.i < len(sc.b) && (sc.b[sc.i] == '+' || sc.b[sc.i] == '-') {
			sc.i++
		}
		if digits() == 0 {
			return errSyntax
		}
	}
	return nil
}

// containsNull reports whether a validated JSON value holds a null
// anywhere (outside strings).
func containsNull(raw []byte) bool {
	in := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case in && c == '\\':
			i++
		case c == '"':
			in = !in
		case !in && c == 'n' && bytes.HasPrefix(raw[i:], []byte("null")):
			return true
		}
	}
	return false
}
