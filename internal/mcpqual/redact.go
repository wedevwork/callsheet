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
	// capture selects the mcpqual-capture-v1 policy (NewCaptureRedactor):
	// values are redacted in place and only unsafe non-JSON lines are
	// omitted.
	capture bool
}

type literal struct{ value, repl string }

// NewRedactor builds a redactor; secrets are replaced with Redacted and
// each path in paths with its label. Secrets shorter than four characters
// are kept (qualification policy).
func NewRedactor(secrets []string, paths map[string]string) *Redactor {
	return newRedactor(secrets, paths, 4)
}

// NewCaptureRedactor is the capture redaction policy mcpqual-capture-v1
// (design decoder-enrollment r0.3, Capture evidence). Credential words in
// ordinary prose are not secrets: values are redacted in place with
// Redacted, every nonempty known secret literal whatever its length
// (longest first), Bearer credentials, sk-/pk-/xai-/key-/rk- prefixed keys,
// plain credential assignments (captureAssignments), HTTP(S) URL userinfo
// and credential-named JSON members (by decoded content); paths keep their
// labels. A non-JSON line is omitted whole only when it is unsafe
// (unsafeLine). A short literal may corrupt evidence incidentally; that
// makes enrollment fail rather than permitting leakage.
func NewCaptureRedactor(secrets []string, paths map[string]string) *Redactor {
	r := newRedactor(secrets, paths, 1)
	r.capture = true
	return r
}

func newRedactor(secrets []string, paths map[string]string, minSecret int) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(strings.TrimSpace(s)) >= minSecret {
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

// CredentialValues returns the values of credential-named variables
// (case-insensitive key, token, secret, password, passwd, auth,
// credential, cookie or session in the name) of the inherited environment
// environ's KEY=VALUE entries, for removal as literals; the environment
// itself is never serialized. The one exception (design decoder-enrollment
// r0.3) is the OS session number: exactly XDG_SESSION_ID with a decimal
// value is metadata, not a credential, so it never becomes a literal (a
// session "2" would otherwise replace every 2 in versions, numbers and
// nonces). A value another credential variable also holds stays a secret.
func CredentialValues(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" && secretName.MatchString(k) && !(k == "XDG_SESSION_ID" && decimalRe.MatchString(v)) {
			out = append(out, v)
		}
	}
	return out
}

var decimalRe = regexp.MustCompile(`^[0-9]+$`)

// jsonPass is the shape pass over re-encoded JSON text. Under the capture
// policy it skips the plain-assignment rule: every decoded string already
// had it, and in encoded text a quoted value is escaped (\"), which the
// rule would misread.
func (r *Redactor) jsonPass(s string) string {
	if r.capture {
		return r.captureShapes(s, false)
	}
	return string(r.Bytes([]byte(s)))
}

// captureBytes is the capture policy's in-place value redaction of text.
func (r *Redactor) captureBytes(s string) string { return r.captureShapes(s, true) }

func (r *Redactor) captureShapes(s string, assignments bool) string {
	for _, l := range r.literals {
		s = strings.ReplaceAll(s, l.value, l.repl)
	}
	lower := foldLower(s)
	has := func(i int) bool {
		for _, tr := range patternTriggers[i] {
			if strings.Contains(lower, tr) {
				return true
			}
		}
		return false
	}
	if has(0) {
		s = secretPatterns[0].ReplaceAllString(s, "${1}"+Redacted)
	}
	if has(1) {
		s = secretPatterns[1].ReplaceAllString(s, Redacted)
	}
	if has(2) && strings.Contains(s, ":") {
		s = secretPatterns[2].ReplaceAllString(s, "${1}"+Redacted+"${3}")
	}
	if assignments && strings.Contains(s, "=") {
		s, _ = captureAssignments(s)
	}
	if strings.Contains(s, "@") {
		s = secretPatterns[4].ReplaceAllString(s, "${1}"+Redacted)
	}
	return s
}

// assignmentRe finds an identifier and its assignment separator: an ASCII
// identifier, optional horizontal whitespace, =, optional horizontal
// whitespace.
var assignmentRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)[ \t]*=[ \t]*`)

// captureAssignments redacts the value of every plain credential
// assignment (an identifier matching secretName): an unquoted value
// through the next whitespace, or the contents of a single- or
// double-quoted value closed on the same line. Identifier, separator and
// quotes are kept; an already-redacted value stays byte-identical. An
// unterminated quoted value is redacted to the end of its line here and
// reported, so a line-level caller omits the line instead.
func captureAssignments(s string) (string, bool) {
	var out strings.Builder
	cursor, unterminated := 0, false
	for _, m := range assignmentRe.FindAllStringSubmatchIndex(s, -1) {
		if m[0] < cursor || !secretName.MatchString(s[m[2]:m[3]]) {
			continue
		}
		v := m[1]
		end := v
		switch {
		case v < len(s) && (s[v] == '"' || s[v] == '\''):
			eol := strings.IndexByte(s[v+1:], '\n')
			if eol < 0 {
				eol = len(s) - v - 1
			}
			if c := strings.IndexByte(s[v+1:v+1+eol], s[v]); c >= 0 {
				v, end = v+1, v+1+c
			} else {
				v, end, unterminated = v+1, v+1+eol, true
			}
		default:
			for end < len(s) && !strings.ContainsRune(" \t\n\r\v\f", rune(s[end])) {
				end++
			}
			if end == v {
				continue
			}
		}
		out.WriteString(s[cursor:v])
		out.WriteString(Redacted)
		cursor = end
	}
	if cursor == 0 && out.Len() == 0 {
		return s, unterminated
	}
	out.WriteString(s[cursor:])
	return out.String(), unterminated
}

// quotedMemberRe is an apparent quoted JSON credential member (the
// credential-member name shape of secretPatterns[2], then a colon).
var quotedMemberRe = regexp.MustCompile(`(?i)"[A-Za-z0-9_-]*(api[_-]?key|token|secret|passw(?:or)?d|authorization|credential|cookie)[A-Za-z0-9_-]*"\s*:`)

// unsafeLine reports a non-JSON line the capture policy omits whole:
// invalid UTF-8, any backslash (an ambiguous encoded or cut value), an
// apparent quoted JSON credential member, or an unterminated quoted
// credential assignment.
func unsafeLine(b []byte) bool {
	if !utf8.Valid(b) || bytes.IndexByte(b, '\\') >= 0 || quotedMemberRe.Match(b) {
		return true
	}
	if bytes.IndexByte(b, '=') >= 0 {
		_, unterminated := captureAssignments(string(b))
		return unterminated
	}
	return false
}

// foldLower is s lower-cased for the trigger prefilters, with the two
// non-ASCII letters Unicode case folding equates with ASCII letters mapped
// to them (ſ, long s, to s; the Kelvin sign is lower-cased to k by
// ToLower), so a prefilter is never narrower than the case-insensitive
// expressions it guards (code review C1).
func foldLower(s string) string {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "ſ") {
		lower = strings.ReplaceAll(lower, "ſ", "s")
	}
	return lower
}

// Bytes returns b with every literal and credential shape removed.
func (r *Redactor) Bytes(b []byte) []byte {
	if r.capture {
		return []byte(r.captureBytes(string(b)))
	}
	s := string(b)
	for _, l := range r.literals {
		s = strings.ReplaceAll(s, l.value, l.repl)
	}
	lower := foldLower(s)
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
	lower := foldLower(s)
	for _, tr := range jsonTriggers {
		if strings.Contains(lower, tr) {
			return true
		}
	}
	// The capture assignment rule also names session variables.
	return r.capture && strings.Contains(lower, "session")
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

// memberTriggers are lower-case substrings without which secretMember
// cannot match (a cheap necessary condition checked first).
var memberTriggers = []string{"key", "token", "secret", "passw", "authorization", "credential", "cookie"}

// isSecretMember reports whether a member name is credential-named. The
// trigger prefilter applies to ASCII names only: a non-ASCII name (which
// case folding could match, such as a Kelvin sign for k) always takes the
// regular expression.
func isSecretMember(key string) bool {
	for i := 0; i < len(key); i++ {
		if key[i] >= utf8.RuneSelf {
			return secretMember.MatchString(key)
		}
	}
	lower := strings.ToLower(key)
	for _, tr := range memberTriggers {
		if strings.Contains(lower, tr) {
			return secretMember.MatchString(key)
		}
	}
	return false
}

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
//
// Under the capture policy a complete JSON value takes the decoded-content
// path (valid escaped JSON included), any other line is omitted only when
// unsafeLine, and otherwise its values are redacted in place: a credential
// word in prose is kept.
func (r *Redactor) TranscriptLine(b []byte) ([]byte, bool) {
	if r.capture {
		switch {
		case utf8.Valid(b) && json.Valid(b):
			return r.Line(b), true
		case unsafeLine(b):
			return nil, false
		}
		return []byte(r.text(string(b), 0)), true
	}
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
	if esc && backslashRun(s) >= maxEncodedRun {
		// k JSON-string layers leave a run of 2^k-1 backslashes: this many
		// can never be resolved within the two budgets (maxRedactDepth JSON
		// walks, then maxRedactDepth unescapes), so it is withheld whole at
		// once rather than walked (code review C2).
		return Redacted
	}
	t := strings.TrimLeft(s, " \t\r\n")
	jsonLike := t != "" && strings.IndexByte(`{["`, t[0]) >= 0 && json.Valid([]byte(t))
	if depth >= maxRedactDepth && (esc || jsonLike) {
		// The decode budget is exhausted with a value still encoded: it is
		// never inspected further, so it is withheld whole (code review C2).
		return Redacted
	}
	if jsonLike {
		if out, changed := r.jsonText([]byte(t), depth); changed {
			return r.jsonPass(s[:len(s)-len(t)] + string(out))
		}
		return r.jsonPass(s)
	} else if esc {
		v, resolved := unescapeResolved(s)
		if !resolved {
			return Redacted
		}
		if v != s {
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
				// A name still encoded when its decode budget runs out was
				// never classified: its whole value subtree is withheld
				// (code review round 2, C1).
				view, resolved := unescapeResolved(key)
				top.secret = isSecretMember(key) || !resolved || isSecretMember(view)
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
// they are. It drops whether the decoding resolved, so no redaction
// decision uses it: every decision site calls unescapeResolved and treats
// an unresolved view as a credential (code review round 2, C1).
func unescapeView(s string) string {
	v, _ := unescapeResolved(s)
	return v
}

// maxEncodedRun is the shortest backslash run that needs more decoding
// layers than both budgets together allow.
const maxEncodedRun = 1 << (2 * maxRedactDepth)

// backslashRun is the length of the longest run of backslashes in s.
func backslashRun(s string) int {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// unescapeResolved is unescapeView also reporting whether the decoding
// reached its fixed point within the budget; an unresolved view still
// holds an encoded layer that no redaction decision saw.
func unescapeResolved(s string) (string, bool) {
	for i := 0; i < maxRedactDepth; i++ {
		if strings.IndexByte(s, '\\') < 0 {
			return s, true
		}
		u := unescapeOnce(s)
		if u == s {
			return s, true
		}
		s = u
	}
	return s, strings.IndexByte(s, '\\') < 0 || unescapeOnce(s) == s
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
