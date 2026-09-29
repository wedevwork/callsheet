package mcpqual

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Redacted replaces every removed secret or identifying value.
const Redacted = "[REDACTED]"

// secretPatterns are credential shapes removed from every publishable
// file: bearer tokens, common API-key prefixes, and the values of
// credential-named JSON members (also when a cut line ends the value),
// TOML/env assignments and URL userinfo.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\b(sk|pk|xai|key|rk)-[A-Za-z0-9_-]{12,}`),
	regexp.MustCompile(`(?i)("[A-Za-z0-9_-]*(api[_-]?key|token|secret|passw(?:or)?d|authorization|credential|cookie)[A-Za-z0-9_-]*"\s*:\s*")[^"]*("|$)`),
	regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(API_KEY|TOKEN|SECRET|PASSWORD)[A-Za-z0-9_]*\s*=\s*)("[^"\n]*"|[^\s"]+)`),
	regexp.MustCompile(`(https?://)[^/\s:@]+:[^/\s@]+@`),
}

// patternTriggers are lower-case substrings without which a pattern
// cannot match; the regular expression runs only when one is present.
var patternTriggers = [][]string{
	{"bearer"},
	{"sk-", "pk-", "xai-", "key-", "rk-"},
	{"key", "token", "secret", "passw", "authorization", "credential", "cookie"},
	{"api_key", "token", "secret", "password"},
	{"@"},
}

// Redactor removes known literal values (plan-supplied environment values,
// the home directory, the user name) and credential shapes. It runs before
// any publishable file is written, so evidence hashes cover sanitized bytes
// only.
type Redactor struct {
	literals []literal
}

type literal struct{ value, repl string }

// NewRedactor builds a redactor; secrets are replaced with Redacted and
// each path in paths with its label.
func NewRedactor(secrets []string, paths map[string]string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(strings.TrimSpace(s)) >= 4 {
			r.literals = append(r.literals, literal{s, Redacted})
		}
	}
	for p, label := range paths {
		if len(p) > 1 {
			r.literals = append(r.literals, literal{p, label})
		}
	}
	sort.SliceStable(r.literals, func(i, j int) bool { return len(r.literals[i].value) > len(r.literals[j].value) })
	return r
}

// Bytes returns b with every literal and credential shape removed.
func (r *Redactor) Bytes(b []byte) []byte {
	s := string(b)
	for _, l := range r.literals {
		s = strings.ReplaceAll(s, l.value, l.repl)
	}
	lower := strings.ToLower(s)
	for i, re := range secretPatterns {
		hit := false
		for _, tr := range patternTriggers[i] {
			if strings.Contains(lower, tr) {
				hit = true
				break
			}
		}
		if !hit || i == 2 && !strings.Contains(s, ":") || i == 3 && !strings.Contains(s, "=") {
			continue
		}
		switch i {
		case 0, 3, 4:
			s = re.ReplaceAllString(s, "${1}"+Redacted)
		case 2:
			s = re.ReplaceAllString(s, "${1}"+Redacted+"${3}")
		default:
			s = re.ReplaceAllString(s, Redacted)
		}
	}
	return []byte(s)
}

// String redacts s by its unescaped content, as Line does; the common
// string holding no escape, no literal and no trigger is returned as is.
func (r *Redactor) String(s string) string {
	return r.text(s, 0)
}

// mayRedact reports whether s holds a literal or any trigger of a pattern
// or of a credential-named member.
func (r *Redactor) mayRedact(s string) bool {
	for _, l := range r.literals {
		if strings.Contains(s, l.value) {
			return true
		}
	}
	lower := strings.ToLower(s)
	for _, tr := range jsonTriggers {
		if strings.Contains(lower, tr) {
			return true
		}
	}
	return false
}

// Struct redacts, in place, every string, *string and string slice element
// reachable from v (a settable struct, pointer, slice or string value).
func (r *Redactor) Struct(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			r.Struct(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				r.Struct(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return // raw bytes (json.RawMessage) are not report text
		}
		for i := 0; i < v.Len(); i++ {
			r.Struct(v.Index(i))
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(r.String(v.String()))
		}
	}
}

// secretMember names JSON members whose string values are credentials.
var secretMember = regexp.MustCompile(`(?i)(api[_-]?key|apikey|token|secret|password|passwd|authorization|credential|cookie|private[_-]?key)`)

// jsonTriggers are lower-case substrings without which a JSON line holds
// neither a credential-named member nor a credential shape.
var jsonTriggers = []string{"key", "token", "secret", "passw", "auth", "credential", "cookie", "bearer", "sk-", "pk-", "xai-", "rk-", "@"}

// maxRedactDepth bounds how many JSON-in-string or escape layers are
// decoded before a redaction decision.
const maxRedactDepth = 8

// Line sanitizes one evidence line. Every decision is made on unescaped
// content: a JSON value (after any leading whitespace) is walked token by
// token, preserving member order, with keys and strings redacted by their
// decoded content (recursing into strings that are JSON themselves) and
// credential-named members' strings replaced; any other line is redacted
// as text and, when it holds escapes (a cut JSON line), by its decoded
// view as well. Only a line with no escape, no literal and no trigger is
// kept without a walk, since then its bytes are its content.
func (r *Redactor) Line(b []byte) []byte {
	if bytes.IndexByte(b, '\\') < 0 && !r.mayRedact(string(b)) {
		return b
	}
	return []byte(r.text(string(b), 0))
}

// TranscriptLine sanitizes one vendor transcript line for publication,
// reporting false when the line must be omitted whole. A line whose
// original bytes are one complete JSON value takes Line's decoded-content
// path. An invalid line (including one that is not UTF-8, which json.Valid
// accepts inside strings) that may hold a credential (any backslash, a
// known literal or a credential trigger) is never unescaped or partly
// redacted: no part of it is published. Other invalid lines are redacted
// as text.
func (r *Redactor) TranscriptLine(b []byte) ([]byte, bool) {
	if (!utf8.Valid(b) || !json.Valid(b)) && (bytes.IndexByte(b, '\\') >= 0 || r.mayRedact(string(b))) {
		return nil, false
	}
	return r.Line(b), true
}

// text redacts s by its unescaped content; see Line.
func (r *Redactor) text(s string, depth int) string {
	esc := strings.IndexByte(s, '\\') >= 0
	if !esc && !r.mayRedact(s) {
		return s
	}
	t := strings.TrimLeft(s, " \t\r\n")
	if depth < maxRedactDepth && t != "" && strings.IndexByte(`{["`, t[0]) >= 0 && json.Valid([]byte(t)) {
		if out, changed := r.jsonText([]byte(t), depth); changed {
			return string(r.Bytes([]byte(s[:len(s)-len(t)] + string(out))))
		}
	} else if esc {
		if v := unescapeView(s); v != s {
			if red := string(r.Bytes([]byte(v))); red != v {
				return red
			}
		}
	}
	return string(r.Bytes([]byte(s)))
}

// jsonText re-emits one valid JSON value compactly in order, redacting
// keys and strings by content; it reports whether anything was redacted
// (so an unchanged value keeps its original bytes). Strings anywhere
// under a credential-named member are replaced.
func (r *Redactor) jsonText(b []byte, depth int) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	type frame struct {
		obj    bool
		n      int
		secret bool // the current member is credential-named
		all    bool // the container is itself under a credential-named member
	}
	var st []frame
	var out bytes.Buffer
	changed := false
	for {
		tok, err := dec.Token()
		if err != nil { // io.EOF after the one value; b is valid JSON
			break
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			out.WriteByte(byte(d))
			st = st[:len(st)-1]
			continue
		}
		secret := false
		if len(st) > 0 {
			top := &st[len(st)-1]
			switch {
			case top.obj && top.n%2 == 1:
				out.WriteByte(':')
				secret = top.secret
			case top.n > 0:
				out.WriteByte(',')
			}
			secret = secret || top.all
			if top.obj && top.n%2 == 0 {
				key, _ := tok.(string)
				top.secret = secretMember.MatchString(key) || secretMember.MatchString(unescapeView(key))
				top.n++
				rk := r.text(key, depth+1)
				changed = changed || rk != key
				out.WriteString(quoteJSON(rk))
				continue
			}
			top.n++
		}
		switch v := tok.(type) {
		case json.Delim:
			out.WriteByte(byte(v))
			st = append(st, frame{obj: v == '{', all: secret})
		case string:
			rv := Redacted
			if !secret {
				rv = r.text(v, depth+1)
			}
			changed = changed || rv != v
			out.WriteString(quoteJSON(rv))
		case json.Number:
			out.WriteString(string(v))
		case bool:
			out.WriteString(strconv.FormatBool(v))
		case nil:
			out.WriteString("null")
		}
	}
	return out.Bytes(), changed
}

// unescapeView decodes JSON string escapes wherever they occur in s,
// repeatedly (up to maxRedactDepth layers), leaving malformed escapes as
// they are. It is only a view for redaction decisions.
func unescapeView(s string) string {
	for i := 0; i < maxRedactDepth && strings.IndexByte(s, '\\') >= 0; i++ {
		u := unescapeOnce(s)
		if u == s {
			break
		}
		s = u
	}
	return s
}

func unescapeOnce(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch e := s[i+1]; e {
		case '"', '\\', '/':
			b.WriteByte(e)
			i++
		case 'b', 'f', 'n', 'r', 't':
			b.WriteByte("\b\f\n\r\t"[strings.IndexByte("bfnrt", e)])
			i++
		case 'u':
			c, n := unicodeEscape(s[i:])
			if n == 0 {
				b.WriteByte(s[i])
				continue
			}
			b.WriteRune(c)
			i += n - 1
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// unicodeEscape decodes the \uXXXX (or surrogate pair) at the start of s,
// returning the rune and the bytes consumed, or 0 bytes when malformed.
func unicodeEscape(s string) (rune, int) {
	hex := func(s string) (rune, bool) {
		if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
			return 0, false
		}
		n, err := strconv.ParseUint(s[2:6], 16, 16)
		return rune(n), err == nil
	}
	c, ok := hex(s)
	if !ok {
		return 0, 0
	}
	if utf16.IsSurrogate(c) {
		if lo, ok := hex(s[6:]); ok {
			if p := utf16.DecodeRune(c, lo); p != utf8.RuneError {
				return p, 12
			}
		}
	}
	return c, 6
}
