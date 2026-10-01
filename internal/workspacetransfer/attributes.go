package workspacetransfer

import (
	"bytes"
	"strconv"
	"strings"
)

// Git attributes (gitattributes(5)) for the cleanliness check: only the
// content conversions matter (text, crlf, eol, ident, filter and
// working-tree-encoding); every other attribute is inert. Files are
// parsed with Git's rules (attr.c): whitespace-separated states, "-attr"
// unset, "!attr" unspecified, "attr=value", C-quoted patterns, no
// negative patterns, "[attr]" macros only at the top level. The built-in
// "binary" macro is -diff -merge -text; a user macro that changes a
// conversion attribute, or a redefinition of binary that changes its
// conversion (Git honors redefinitions), is refused as unsupported rather
// than guessed.

// Attribute states.
const (
	attrUnspecified = iota
	attrSet
	attrUnset
	attrValue
)

type attrState struct {
	state int
	value string
}

type attrAssign struct {
	name string
	attrState
}

// attrLine is one pattern line with its assignments.
type attrLine struct {
	pat     ignorePattern
	assigns []attrAssign
}

// attrFile is one parsed attributes file.
type attrFile struct {
	lines  []attrLine
	macros map[string][]attrAssign
}

// conversionAttrs are the attributes that change content on check-in.
var conversionAttrs = map[string]bool{"text": true, "crlf": true, "eol": true, "ident": true, "filter": true,
	"working-tree-encoding": true, "binary": true}

const maxAttrLine = 2048

const attrBlank = " \t\r\n"

func validAttrName(n string) bool {
	if n == "" || n[0] == '-' {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c == '-' || c == '.' || c == '_' || isDigit(c) || isAlpha(c)) {
			return false
		}
	}
	return true
}

// parseAttrState parses one state token.
func parseAttrState(tok string) (attrAssign, bool) {
	var a attrAssign
	switch {
	case strings.HasPrefix(tok, "-"):
		a.name, a.state = tok[1:], attrUnset
	case strings.HasPrefix(tok, "!"):
		a.name, a.state = tok[1:], attrUnspecified
	default:
		if n, v, ok := strings.Cut(tok, "="); ok {
			a.name, a.state, a.value = n, attrValue, v
		} else {
			a.name, a.state = tok, attrSet
		}
	}
	return a, validAttrName(a.name)
}

// errUnsupportedMacro marks a user macro that changes a conversion.
type errUnsupportedMacro struct{}

func (errUnsupportedMacro) Error() string { return "unsupported attribute macro" }

// parseAttributes parses an attributes file whose patterns are relative
// to base (a directory key with a trailing slash, "" at the root).
// macroOK permits "[attr]" definitions (top-level files only; elsewhere
// Git ignores them).
func parseAttributes(content []byte, base string, macroOK, caseFold bool) (*attrFile, error) {
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	f := &attrFile{macros: map[string][]attrAssign{}}
	for len(content) > 0 {
		line := content
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}
		if len(line) >= maxAttrLine {
			continue
		}
		s := strings.TrimLeft(string(line), attrBlank)
		if s == "" || s[0] == '#' {
			continue
		}
		var name, states string
		if s[0] == '"' {
			n, rest, ok := unquoteC(s)
			if !ok {
				continue
			}
			name, states = n, rest
		} else {
			i := strings.IndexAny(s, attrBlank)
			if i < 0 {
				i = len(s)
			}
			name, states = s[:i], s[i:]
		}
		macro := ""
		if len(name) > len("[attr]") && strings.HasPrefix(name, "[attr]") {
			if !macroOK {
				continue
			}
			// As attr.c: the macro name is the prefix's remainder up to
			// the first blank ("[attr] name" is the pattern "[attr]",
			// since its first token is not longer than the prefix).
			m := strings.TrimLeft(name[len("[attr]"):], attrBlank)
			if i := strings.IndexAny(m, attrBlank); i >= 0 {
				m = m[:i]
			}
			if !validAttrName(m) {
				continue
			}
			macro = m
		}
		var assigns []attrAssign
		ok := true
		for _, tok := range strings.FieldsFunc(states, func(r rune) bool { return strings.ContainsRune(attrBlank, r) }) {
			a, valid := parseAttrState(tok)
			if !valid {
				ok = false
				break
			}
			assigns = append(assigns, a)
		}
		if !ok {
			continue
		}
		if macro != "" {
			if macro == "binary" {
				// Git honors a redefined binary macro (it replaces the
				// built-in -diff -merge -text). One whose conversion is
				// exactly the built-in's (-text, no other conversion
				// attribute) is equivalent here; any other changes the
				// conversion and is refused as unsupported.
				if !binaryEquivalent(assigns) {
					return nil, errUnsupportedMacro{}
				}
				continue
			}
			for _, a := range assigns {
				if conversionAttrs[a.name] {
					return nil, errUnsupportedMacro{}
				}
			}
			f.macros[macro] = assigns
			continue
		}
		p, ok := compilePattern(name, base, caseFold)
		if !ok || p.flags&patNegative != 0 {
			continue
		}
		f.lines = append(f.lines, attrLine{pat: p, assigns: assigns})
	}
	return f, nil
}

// binaryEquivalent reports a binary macro definition whose conversion
// effect is the built-in's: text unset and no other conversion attribute
// (diff and merge are inert here).
func binaryEquivalent(assigns []attrAssign) bool {
	text := false
	for _, a := range assigns {
		switch {
		case a.name == "text" && a.state == attrUnset:
			text = true
		case conversionAttrs[a.name]:
			return false
		}
	}
	return text
}

// unquoteC decodes a leading C-quoted string (Git's unquote_c_style) and
// returns it with the rest of s.
func unquoteC(s string) (string, string, bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			return b.String(), s[i+1:], true
		case '\\':
			i++
			if i >= len(s) {
				return "", "", false
			}
			switch e := s[i]; e {
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'v':
				b.WriteByte('\v')
			case '\\', '"':
				b.WriteByte(e)
			case '0', '1', '2', '3':
				if i+2 >= len(s) {
					return "", "", false
				}
				n, err := strconv.ParseUint(s[i:i+3], 8, 8)
				if err != nil {
					return "", "", false
				}
				b.WriteByte(byte(n))
				i += 2
			default:
				return "", "", false
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", false
}

// attrMap is the resolved state of the conversion attributes of a path.
type attrMap map[string]attrState

// apply applies one assignment, expanding the binary macro and user
// macros (whose assignments are inert by construction).
func (s attrMap) apply(a attrAssign, macros []map[string][]attrAssign) {
	s[a.name] = a.attrState
	if a.state != attrSet {
		return
	}
	if a.name == "binary" {
		s["diff"] = attrState{state: attrUnset}
		s["merge"] = attrState{state: attrUnset}
		s["text"] = attrState{state: attrUnset}
		return
	}
	for i := len(macros) - 1; i >= 0; i-- {
		if m, ok := macros[i][a.name]; ok {
			for _, x := range m {
				s[x.name] = x.attrState
			}
			return
		}
	}
}

// resolveAttrs returns the attributes of the key pathname (never a directory)
// from files in ascending priority.
func resolveAttrs(files []*attrFile, pathname string) attrMap {
	s := attrMap{}
	macros := make([]map[string][]attrAssign, 0, len(files))
	for _, f := range files {
		if f != nil && len(f.macros) > 0 {
			macros = append(macros, f.macros)
		}
	}
	for _, f := range files {
		if f == nil {
			continue
		}
		for i := range f.lines {
			l := &f.lines[i]
			if l.pat.matches(pathname, false) {
				for _, a := range l.assigns {
					s.apply(a, macros)
				}
			}
		}
	}
	return s
}

// fixedAttributeFiles returns the system and global attributes files Git
// reads, lowest priority first: the system file unless GIT_ATTR_NOSYSTEM,
// then core.attributesFile (empty: none) or the XDG default.
func fixedAttributeFiles(env Env, cfg *gitConfig, root string) ([]string, error) {
	var out []string
	noSystem, err := envBool(env, "GIT_ATTR_NOSYSTEM")
	if err != nil {
		return nil, err
	}
	if !noSystem && env.SystemAttributes != "" {
		out = append(out, env.SystemAttributes)
	}
	attrFile, ok := cfg.str("core.attributesfile")
	switch {
	case ok && attrFile == "":
	case ok:
		p, err := expandConfigPath(env, attrFile, root)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	default:
		if p := env.xdgFile("attributes"); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// assignsLFS reports an attributes file that routes any path to the LFS
// filter (filter=lfs, directly or through a macro).
func assignsLFS(b []byte, macroOK bool) (bool, error) {
	f, err := parseAttributes(b, "", macroOK, false)
	if err != nil {
		return false, err
	}
	for _, l := range f.lines {
		for _, a := range l.assigns {
			if a.name == "filter" && a.state == attrValue && a.value == "lfs" {
				return true, nil
			}
		}
	}
	return false, nil
}
