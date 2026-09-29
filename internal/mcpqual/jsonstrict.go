package mcpqual

// Strict JSON scanning, duplicated from the internal/mcp codec subset the
// probe and the schemas need (design 07b: accepted duplication). checkJSON
// validates one JSON text beyond encoding/json: nesting depth at most
// maxDepth, no duplicate object keys at any level (compared after
// unescaping), every \u surrogate escape paired, and no trailing data.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// maxDepth bounds JSON nesting in frames, plans, case files and reports.
const maxDepth = 32

var errSyntax = errors.New("malformed JSON")

func checkJSON(b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("not valid UTF-8")
	}
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

// decodeStrict checks b strictly and decodes it into v, rejecting unknown
// fields at every level.
func decodeStrict(b []byte, v any) error {
	if err := checkJSON(b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

type jsonScanner struct {
	b []byte
	i int
}

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
	if depth > maxDepth {
		return fmt.Errorf("nesting deeper than %d levels", maxDepth)
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
			return errors.New("duplicate object key " + strconv.Quote(key))
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
	if depth > maxDepth {
		return fmt.Errorf("nesting deeper than %d levels", maxDepth)
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
			var s string
			if json.Unmarshal(tok, &s) != nil {
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

// encodeJSON renders v compactly without HTML escaping.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// encodeIndent renders v with two-space indentation, without HTML
// escaping, ending in a newline.
func encodeIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// quoteJSON is s as a JSON string literal (no HTML escaping).
func quoteJSON(s string) string {
	b, _ := encodeJSON(s)
	return string(b)
}
