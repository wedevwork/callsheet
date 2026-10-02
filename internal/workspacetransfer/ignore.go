package workspacetransfer

import (
	"bytes"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// The ignore compatibility layer (FP-4). go-git v5.16.3's gitignore
// package is not a complete policy implementation (its reader drops
// errors and truncates long lines, its matcher uses filepath.Match
// without Git's POSIX classes and cannot backtrack every "**"), so ignore
// files are parsed here with Git's rules (dir.c) and matched with a port
// of Git's wildmatch. The compiled patterns implement go-git's
// gitignore.Pattern interface.
//
// Two source policies use it:
//   - plain folders: only the root and nested .gitignore files, byte-exact;
//   - git sources: global excludes < .git/info/exclude < root .gitignore <
//     deeper .gitignore, for untracked paths only, with core.ignoreCase
//     and the darwin precompose comparison keys applied to candidates.

// wildmatch flags and results (wildmatch.c).
const (
	wmCaseFold = 1
	wmPathname = 2

	wmMatch        = 0
	wmNoMatch      = 1
	wmAbortAll     = -1
	wmAbortToStars = -2
)

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return isUpper(c) || isLower(c) }
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isPrint(c byte) bool { return c >= 0x20 && c <= 0x7e }
func isCntrl(c byte) bool { return c < 0x20 || c == 0x7f }
func isPunct(c byte) bool { return isPrint(c) && !isAlpha(c) && !isDigit(c) && c != ' ' }
func isXDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func toLower(c byte) byte {
	if isUpper(c) {
		return c + 'a' - 'A'
	}
	return c
}
func toUpper(c byte) byte {
	if isLower(c) {
		return c - ('a' - 'A')
	}
	return c
}

func isGlobSpecial(c byte) bool { return c == '*' || c == '?' || c == '[' || c == '\\' }

// at returns s[i], or NUL past the end (the C string convention).
func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// wildmatch is Git's wildmatch(): it matches text against pattern with
// wmPathname ('*' and '?' do not cross '/', "**" does in its special
// positions) and wmCaseFold (ASCII only). Malformed bracket expressions
// match nothing.
func wildmatch(pattern, text string, flags int) bool {
	return dowild(pattern, 0, text, 0, flags) == wmMatch
}

func dowild(pat string, p int, text string, t int, flags int) int {
	for ; p < len(pat); t, p = t+1, p+1 {
		pCh := pat[p]
		tCh := at(text, t)
		if tCh == 0 && t >= len(text) && pCh != '*' {
			return wmAbortAll
		}
		if flags&wmCaseFold != 0 {
			tCh, pCh = toLower(tCh), toLower(pCh)
		}
		switch pCh {
		case '\\':
			// The escaped character is compared as written (Git does
			// not fold it).
			p++
			pCh = at(pat, p)
			if tCh != pCh || p >= len(pat) {
				return wmNoMatch
			}
			continue
		default:
			if tCh != pCh {
				return wmNoMatch
			}
			continue
		case '?':
			if flags&wmPathname != 0 && tCh == '/' {
				return wmNoMatch
			}
			continue
		case '*':
			var matchSlash bool
			p++
			if at(pat, p) == '*' {
				prev := p - 2
				for p++; at(pat, p) == '*'; p++ {
				}
				if (prev < 0 || pat[prev] == '/') &&
					(p >= len(pat) || pat[p] == '/' || (at(pat, p) == '\\' && at(pat, p+1) == '/')) {
					if at(pat, p) == '/' && p < len(pat) && dowild(pat, p+1, text, t, flags) == wmMatch {
						return wmMatch
					}
					matchSlash = true
				} else {
					matchSlash = false
				}
			} else {
				matchSlash = flags&wmPathname == 0
			}
			if p >= len(pat) {
				if !matchSlash && strings.IndexByte(text[min(t, len(text)):], '/') >= 0 {
					return wmNoMatch
				}
				return wmMatch
			} else if !matchSlash && pat[p] == '/' {
				i := strings.IndexByte(text[min(t, len(text)):], '/')
				if i < 0 {
					return wmNoMatch
				}
				t += i
				// The slash is consumed by the loop.
				break
			}
			for {
				if t >= len(text) {
					break
				}
				if !isGlobSpecial(pat[p]) {
					pc := pat[p]
					if flags&wmCaseFold != 0 {
						pc = toLower(pc)
					}
					for t < len(text) && (matchSlash || text[t] != '/') {
						tc := text[t]
						if flags&wmCaseFold != 0 {
							tc = toLower(tc)
						}
						if tc == pc {
							break
						}
						t++
					}
					tc := at(text, t)
					if flags&wmCaseFold != 0 {
						tc = toLower(tc)
					}
					if t >= len(text) || tc != pc {
						if matchSlash {
							return wmAbortAll
						}
						return wmAbortToStars
					}
				}
				matched := dowild(pat, p, text, t, flags)
				if matched != wmNoMatch {
					if !matchSlash || matched != wmAbortToStars {
						return matched
					}
				} else if !matchSlash && text[t] == '/' {
					return wmAbortToStars
				}
				t++
			}
			return wmAbortAll
		case '[':
			p++
			pCh = at(pat, p)
			if pCh == '^' {
				pCh = '!'
			}
			negated := pCh == '!'
			if negated {
				p++
				pCh = at(pat, p)
			}
			var prevCh byte
			matched := false
			for {
				if p >= len(pat) {
					return wmAbortAll
				}
				switch {
				case pCh == '\\':
					p++
					if p >= len(pat) {
						return wmAbortAll
					}
					pCh = pat[p]
					if tCh == pCh {
						matched = true
					}
				case pCh == '-' && prevCh != 0 && p+1 < len(pat) && pat[p+1] != ']':
					p++
					pCh = pat[p]
					if pCh == '\\' {
						p++
						if p >= len(pat) {
							return wmAbortAll
						}
						pCh = pat[p]
					}
					if tCh <= pCh && tCh >= prevCh {
						matched = true
					} else if flags&wmCaseFold != 0 && isLower(tCh) {
						u := toUpper(tCh)
						if u <= pCh && u >= prevCh {
							matched = true
						}
					}
					pCh = 0
				case pCh == '[' && at(pat, p+1) == ':':
					s := p + 2
					q := s
					for q < len(pat) && pat[q] != ']' {
						q++
					}
					if q >= len(pat) {
						return wmAbortAll
					}
					i := q - s - 1
					if i < 0 || pat[q-1] != ':' {
						// No ":]": a literal '['.
						p = s - 2
						pCh = '['
						if tCh == pCh {
							matched = true
						}
						break
					}
					p = q
					switch pat[s : s+i] {
					case "alnum":
						matched = matched || isAlpha(tCh) || isDigit(tCh)
					case "alpha":
						matched = matched || isAlpha(tCh)
					case "blank":
						matched = matched || tCh == ' ' || tCh == '\t'
					case "cntrl":
						matched = matched || isCntrl(tCh)
					case "digit":
						matched = matched || isDigit(tCh)
					case "graph":
						matched = matched || (isPrint(tCh) && tCh != ' ')
					case "lower":
						matched = matched || isLower(tCh)
					case "print":
						matched = matched || isPrint(tCh)
					case "punct":
						matched = matched || isPunct(tCh)
					case "space":
						matched = matched || isSpace(tCh)
					case "upper":
						matched = matched || isUpper(tCh) || (flags&wmCaseFold != 0 && isLower(tCh))
					case "xdigit":
						matched = matched || isXDigit(tCh)
					default:
						return wmAbortAll
					}
					pCh = 0
				default:
					if tCh == pCh {
						matched = true
					}
				}
				prevCh = pCh
				p++
				if p >= len(pat) {
					return wmAbortAll
				}
				pCh = pat[p]
				if pCh == ']' {
					break
				}
			}
			if matched == negated || (flags&wmPathname != 0 && tCh == '/') {
				return wmNoMatch
			}
			continue
		}
	}
	if t < len(text) {
		return wmNoMatch
	}
	return wmMatch
}

// Pattern flags (dir.c).
const (
	patNegative = 1 << iota
	patMustBeDir
	patNoDir
	patEndsWith
)

// ignorePattern is one compiled line of an ignore file.
type ignorePattern struct {
	text          string
	flags         int
	nowildcardlen int
	// base is the containing directory's key with a trailing slash ("" at
	// the root).
	base     string
	caseFold bool
}

var _ gitignore.Pattern = (*ignorePattern)(nil)

// simpleLength is the length of the leading run without glob specials.
func simpleLength(s string) int {
	for i := 0; i < len(s); i++ {
		if isGlobSpecial(s[i]) {
			return i
		}
	}
	return len(s)
}

func noWildcard(s string) bool { return simpleLength(s) == len(s) }

// trimTrailingSpaces drops unescaped trailing spaces (not tabs).
func trimTrailingSpaces(s string) string {
	last := -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			if last < 0 {
				last = i
			}
		case '\\':
			i++
			if i >= len(s) {
				return s
			}
			last = -1
		default:
			last = -1
		}
	}
	if last >= 0 {
		return s[:last]
	}
	return s
}

// parseIgnore compiles an ignore file's content: a UTF-8 BOM is skipped,
// lines end at LF with one CR before it removed, '#' starts a comment,
// empty lines are skipped, unescaped trailing spaces are trimmed, and a
// leading '!' negates. Nothing is truncated.
func parseIgnore(content []byte, base string, caseFold bool) []ignorePattern {
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	var out []ignorePattern
	for len(content) > 0 {
		line := content
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		s := trimTrailingSpaces(string(line))
		if p, ok := compilePattern(s, base, caseFold); ok {
			out = append(out, p)
		}
	}
	return out
}

func compilePattern(s, base string, caseFold bool) (ignorePattern, bool) {
	p := ignorePattern{base: base, caseFold: caseFold}
	if strings.HasPrefix(s, "!") {
		p.flags |= patNegative
		s = s[1:]
	}
	if strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
		p.flags |= patMustBeDir
	}
	if s == "" {
		return p, false
	}
	if strings.IndexByte(s, '/') < 0 {
		p.flags |= patNoDir
	}
	p.nowildcardlen = min(simpleLength(s), len(s))
	if s[0] == '*' && noWildcard(s[1:]) {
		p.flags |= patEndsWith
	}
	p.text = s
	return p, true
}

func pathEq(a, b string, fold bool) bool {
	if fold {
		return asciiFold(a) == asciiFold(b)
	}
	return a == b
}

// asciiFold lowercases ASCII letters only (Git's case folding).
func asciiFold(s string) string {
	for i := 0; i < len(s); i++ {
		if isUpper(s[i]) {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				b[j] = toLower(b[j])
			}
			return string(b)
		}
	}
	return s
}

// matches reports whether the pattern (ignoring its negation) matches the
// slash-separated key pathname.
func (p *ignorePattern) matches(pathname string, isDir bool) bool {
	if p.flags&patMustBeDir != 0 && !isDir {
		return false
	}
	flags := 0
	if p.caseFold {
		flags = wmCaseFold
	}
	if p.flags&patNoDir != 0 {
		basename := pathname[strings.LastIndexByte(pathname, '/')+1:]
		return matchBasename(basename, p.text, p.nowildcardlen, p.flags, flags)
	}
	return matchPathname(pathname, strings.TrimSuffix(p.base, "/"), p.text, p.nowildcardlen, flags)
}

func matchBasename(basename, pat string, prefix, pflags, flags int) bool {
	fold := flags&wmCaseFold != 0
	switch {
	case prefix == len(pat):
		return pathEq(pat, basename, fold)
	case pflags&patEndsWith != 0:
		return len(pat)-1 <= len(basename) && pathEq(pat[1:], basename[len(basename)-(len(pat)-1):], fold)
	}
	return wildmatch(pat, basename, flags)
}

func matchPathname(pathname, base, pat string, prefix, flags int) bool {
	fold := flags&wmCaseFold != 0
	if strings.HasPrefix(pat, "/") {
		pat = pat[1:]
		prefix--
	}
	if len(pathname) < len(base)+1 || (base != "" && pathname[len(base)] != '/') || !pathEq(pathname[:len(base)], base, fold) {
		return false
	}
	name := pathname
	if base != "" {
		name = pathname[len(base)+1:]
	}
	if prefix > 0 {
		if prefix > len(name) || !pathEq(pat[:prefix], name[:prefix], fold) {
			return false
		}
		pat, name = pat[prefix:], name[prefix:]
		if pat == "" && name == "" {
			return true
		}
	}
	return wildmatch(pat, name, flags|wmPathname)
}

// Match implements go-git's gitignore.Pattern over path components
// relative to the repository root.
func (p *ignorePattern) Match(path []string, isDir bool) gitignore.MatchResult {
	if !p.matches(strings.Join(path, "/"), isDir) {
		return gitignore.NoMatch
	}
	if p.flags&patNegative != 0 {
		return gitignore.Include
	}
	return gitignore.Exclude
}

// ignoreList is one source of patterns (one file).
type ignoreList struct {
	patterns []ignorePattern
}

// last returns the last pattern of l matching pathname, or nil.
func (l *ignoreList) last(pathname string, isDir bool) *ignorePattern {
	for i := len(l.patterns) - 1; i >= 0; i-- {
		if l.patterns[i].matches(pathname, isDir) {
			return &l.patterns[i]
		}
	}
	return nil
}

// ignoreStack evaluates patterns in Git's precedence: the per-directory
// lists of the path's directory chain from the deepest to the root, then
// the fixed lists (info/exclude before the global file). The first list
// with a matching pattern decides; a negative match includes.
type ignoreStack struct {
	// fixed is ordered from the highest to the lowest priority.
	fixed []*ignoreList
	// dirs holds the per-directory lists from the root down; nil entries
	// are directories without a .gitignore.
	dirs []*ignoreList
}

func (s *ignoreStack) push(l *ignoreList) { s.dirs = append(s.dirs, l) }
func (s *ignoreStack) pop()               { s.dirs = s.dirs[:len(s.dirs)-1] }

// excluded reports whether the key pathname (inside the directory whose
// lists are on the stack) is ignored.
func (s *ignoreStack) excluded(pathname string, isDir bool) bool {
	for i := len(s.dirs) - 1; i >= 0; i-- {
		if s.dirs[i] == nil {
			continue
		}
		if p := s.dirs[i].last(pathname, isDir); p != nil {
			return p.flags&patNegative == 0
		}
	}
	for _, l := range s.fixed {
		if p := l.last(pathname, isDir); p != nil {
			return p.flags&patNegative == 0
		}
	}
	return false
}
