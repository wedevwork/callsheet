package workspacetransfer

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// UT-4: Git's wildmatch (a corpus from Git's own wildmatch tests), the
// ignore-file syntax and the precedence stack.

func TestWildmatchCorpus(t *testing.T) {
	// pattern, text, pathname-mode result.
	cases := []struct {
		p, s string
		want bool
	}{
		{"foo", "foo", true}, {"foo", "bar", false}, {"", "", true}, {"???", "foo", true}, {"??", "foo", false},
		{"*", "foo", true}, {"f*", "foo", true}, {"*f", "foo", false}, {"*foo*", "foo", true}, {"*ob*a*r*", "foobar", true},
		{"*ab", "aaaaaaabababab", true}, {`foo\*`, "foo*", true}, {`foo\*bar`, "foobar", false}, {`f\\oo`, `f\oo`, true},
		{"*[al]?", "ball", true}, {"[ten]", "ten", false}, {"**[!te]", "ten", true}, {"**[!ten]", "ten", false},
		{"t[a-g]n", "ten", true}, {"t[!a-g]n", "ten", false}, {"t[!a-g]n", "ton", true}, {"t[^a-g]n", "ton", true},
		{"a[]]b", "a]b", true}, {"a[]-]b", "a-b", true}, {"a[]-]b", "a]b", true}, {"a[]-]b", "aab", false}, {"a[]a-]b", "aab", true},
		{"]", "]", true}, {"foo*bar", "foo/baz/bar", false}, {"foo**bar", "foo/baz/bar", false}, {"foo**bar", "foobazbar", true},
		{"foo/**/bar", "foo/baz/bar", true}, {"foo/**/**/bar", "foo/baz/bar", true}, {"foo/**/bar", "foo/b/a/z/bar", true},
		{"foo/**/**/bar", "foo/b/a/z/bar", true}, {"foo/**/bar", "foo/bar", true}, {"foo/**/**/bar", "foo/bar", true},
		{"foo?bar", "foo/bar", false}, {"foo[/]bar", "foo/bar", false}, {"foo[^a-z]bar", "foo/bar", false},
		{"f[^eiu][^eiu][^eiu][^eiu][^eiu]r", "foo/bar", false}, {"f[^eiu][^eiu][^eiu][^eiu][^eiu]r", "foo-bar", true},
		{"**/foo", "foo", true}, {"**/foo", "XXX/foo", true}, {"**/foo", "bar/baz/foo", true}, {"*/foo", "bar/baz/foo", false},
		{"**/bar*", "foo/bar/baz", false}, {"**/bar/*", "deep/foo/bar/baz", true}, {"**/bar/*", "deep/foo/bar/baz/", false},
		{"**/bar/**", "deep/foo/bar/baz/", true}, {"**/bar/*", "deep/foo/bar", false}, {"**/bar/**", "deep/foo/bar/", true},
		{"**/bar**", "foo/bar/baz", false}, {"*/bar/**", "foo/bar/baz/x", true}, {"*/bar/**", "deep/foo/bar/baz/x", false},
		{"**/bar/*/*", "deep/foo/bar/baz/x", true}, {"a[c-c]st", "acrt", false}, {"a[c-c]rt", "acrt", true},
		{"[!]-]", "]", false}, {"[!]-]", "a", true}, {`\`, "", false}, {`\`, `\`, false}, {`*/\`, `/\`, false}, {`*/\\`, `/\`, true},
		{"@foo", "@foo", true}, {"@foo", "foo", false}, {`\[ab]`, "[ab]", true}, {"[[]ab]", "[ab]", true}, {"[[:]ab]", "[ab]", true},
		{"[[::]ab]", "[ab]", false}, {"[[:digit]ab]", "[ab]", true}, {`[\[:]ab]`, "[ab]", true}, {`\??\?b`, "?a?b", true},
		{`\a\b\c`, "abc", true}, {"", "foo", false}, {"**/t[o]", "foo/bar/baz/to", true},
		{"[[:alpha:]][[:digit:]][[:upper:]]", "a1B", true}, {"[[:digit:][:upper:][:space:]]", "a", false},
		{"[[:digit:][:upper:][:space:]]", "A", true}, {"[[:digit:][:upper:][:space:]]", "1", true},
		{"[[:digit:][:upper:][:spaci:]]", "1", false}, {"[[:digit:][:upper:][:space:]]", " ", true},
		{"[[:digit:][:upper:][:space:]]", ".", false}, {"[[:digit:][:punct:][:space:]]", ".", true},
		{"[[:xdigit:]]", "5", true}, {"[[:xdigit:]]", "f", true}, {"[[:xdigit:]]", "D", true},
		{"[[:alnum:][:alpha:][:blank:][:cntrl:][:digit:][:graph:][:lower:][:print:][:punct:][:space:][:upper:][:xdigit:]]", "_", true},
		{"[^[:alnum:][:alpha:][:blank:][:cntrl:][:digit:][:lower:][:space:][:upper:][:xdigit:]]", ".", true},
		{"[a-c[:digit:]x-z]", "5", true}, {"[a-c[:digit:]x-z]", "b", true}, {"[a-c[:digit:]x-z]", "y", true}, {"[a-c[:digit:]x-z]", "q", false},
		{`[\\-^]`, "]", true}, {`[\\-^]`, "[", false}, {`[\-_]`, "-", true}, {`[\]]`, "]", true}, {`[\]]`, `\]`, false}, {`[\]]`, `\`, false},
		{"a[]b", "ab", false}, {"a[]b", "a[]b", false}, {"ab[", "ab[", false}, {"[!", "ab", false}, {"[-", "ab", false}, {"[-]", "-", true},
		{"[a-", "-", false}, {"[!a-", "-", false}, {"[--A]", "-", true}, {"[--A]", "5", true}, {"[ --]", " ", true}, {"[ --]", "$", true},
		{"[ --]", "-", true}, {"[ --]", "0", false}, {"[---]", "-", true}, {"[------]", "-", true}, {"[a-e-n]", "j", false}, {"[a-e-n]", "-", true},
		{"[!------]", "a", true}, {"[]-a]", "[", false}, {"[]-a]", "^", true}, {"[!]-a]", "^", false}, {"[!]-a]", "[", true},
		{"[a^bc]", "^", true}, {"[a-]b]", "-b]", true}, {`[\]`, `\`, false}, {`[\\]`, `\`, true}, {`[!\\]`, `\`, false}, {`[A-\\]`, "G", true},
		{"b*a", "aaabbb", false}, {"*ba*", "aabcaa", false}, {"[,]", ",", true}, {`[\\,]`, ",", true}, {`[\\,]`, `\`, true},
		{"[,-.]", "-", true}, {"[,-.]", "+", false}, {`[\1-\3]`, "2", true}, {`[\1-\3]`, "3", true}, {`[\1-\3]`, "4", false},
		{`[[-\]]`, `\`, true}, {`[[-\]]`, "[", true}, {`[[-\]]`, "]", true}, {`[[-\]]`, "-", false},
		{"-*-*-*-*-*-*-12-*-*-*-m-*-*-*", "-adobe-courier-bold-o-normal--12-120-75-75-m-70-iso8859-1", true},
		{"-*-*-*-*-*-*-12-*-*-*-m-*-*-*", "-adobe-courier-bold-o-normal--12-120-75-75-X-70-iso8859-1", false},
		{"**/*a*b*g*n*t", "abcd/abcdefg/abcdefghijk/abcdefghijklmnop.txt", true},
		{"**/*a*b*g*n*t", "abcd/abcdefg/abcdefghijk/abcdefghijklmnop.txtz", false},
		{"*/*/*", "foo", false}, {"*/*/*", "foo/bar", false}, {"*/*/*", "foo/bba/arr", true}, {"*/*/*", "foo/bb/aa/rr", false},
		{"**/**/**", "foo/bb/aa/rr", true}, {"*X*i", "abcXdefXghi", true}, {"*/*X*/*/*i", "ab/cXd/efXg/hi", true},
		{"**/*X*/**/*i", "ab/cXd/efXg/hi", true},
	}
	for _, c := range cases {
		if got := wildmatch(c.p, c.s, wmPathname); got != c.want {
			t.Errorf("wildmatch(%q, %q) = %v, want %v", c.p, c.s, got, c.want)
		}
	}
	// Without WM_PATHNAME (basename matching) '*' crosses '/'.
	if !wildmatch("foo*bar", "foo/baz/bar", 0) || !wildmatch("foo?bar", "foo/bar", 0) || !wildmatch("*/foo", "bar/baz/foo", 0) {
		t.Error("non-pathname matching")
	}
	// ASCII case folding.
	for _, c := range []struct{ p, s string }{{"[A-Z]", "a"}, {"[a-z]", "A"}, {"[[:upper:]]", "a"}, {"FOO", "foo"}, {"f*O", "FOO"}} {
		if !wildmatch(c.p, c.s, wmPathname|wmCaseFold) || wildmatch(c.p, strings.Repeat("é", 1), wmPathname|wmCaseFold) {
			t.Errorf("casefold %q %q", c.p, c.s)
		}
	}
	if wildmatch("É", "é", wmCaseFold) {
		t.Error("non-ASCII folded")
	}
}

func TestParseIgnore(t *testing.T) {
	content := "\xef\xbb\xbf# comment\n\n\\#hash\n\\!bang\n!neg\ntrail   \nesc\\ \\ \nkeep\\\\ \r\ncrlf\r\ndir/\n/anchored\na/b\n*.o\n   \n" + strings.Repeat("x", 70000) + "\nlast"
	pats := parseIgnore([]byte(content), "", false)
	var texts []string
	for _, p := range pats {
		texts = append(texts, p.text)
	}
	want := []string{`\#hash`, `\!bang`, "neg", "trail", `esc\ \ `, `keep\\`, "crlf", "dir", "/anchored", "a/b", "*.o", strings.Repeat("x", 70000), "last"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("patterns %q", texts)
	}
	if pats[2].flags&patNegative == 0 || pats[1].flags&patNegative != 0 || pats[7].flags&patMustBeDir == 0 || pats[9].flags&patNoDir != 0 ||
		pats[10].flags&patEndsWith == 0 || pats[0].flags&patNoDir == 0 {
		t.Fatalf("flags %+v", pats)
	}
	if !pats[0].matches("#hash", false) || !pats[1].matches("!bang", false) || !pats[4].matches("esc  ", false) || !pats[5].matches(`keep\`, false) {
		t.Fatal("escaped patterns")
	}
	// Whitespace-only lines are patterns of spaces trimmed to nothing.
	if len(parseIgnore([]byte("   \n\t\n"), "", false)) != 1 {
		t.Fatal("a tab line is a pattern")
	}
}

// matcher builds a stack with per-directory files (dir key -> content)
// and fixed lists (highest first).
type layered struct {
	dirs  map[string]string
	fixed []string
	fold  bool
}

func (l layered) excluded(path string, isDir bool) bool {
	var s ignoreStack
	for _, f := range l.fixed {
		s.fixed = append(s.fixed, &ignoreList{patterns: parseIgnore([]byte(f), "", l.fold)})
	}
	parts := strings.Split(path, "/")
	prefix := ""
	for i := 0; i < len(parts); i++ {
		if c, ok := l.dirs[prefix]; ok {
			s.push(&ignoreList{patterns: parseIgnore([]byte(c), prefix, l.fold)})
		} else {
			s.push(nil)
		}
		// A directory on the way that is itself excluded prunes the path.
		if i < len(parts)-1 {
			d := strings.Join(parts[:i+1], "/")
			if s.excluded(d, true) {
				return true
			}
			prefix = d + "/"
		}
	}
	return s.excluded(path, isDir)
}

func TestIgnorePrecedence(t *testing.T) {
	l := layered{
		dirs: map[string]string{
			"":       "*.log\n!keep.log\nbuild/\ndir/*\n!dir/keep\nex/\n!ex/keep\n/rootonly\n**/deep/x\nsub/nested.txt\n",
			"sub/":   "!*.log\nlocal\n/anchored-here\n",
			"sub/b/": "*.log\n",
		},
		fixed: []string{"info-ignored\n!global-ignored-but-info-keeps\n", "global-ignored\nglobal-ignored-but-info-keeps\n*.tmp\n"},
	}
	for _, c := range []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"a.log", false, true}, {"keep.log", false, false}, {"x/a.log", false, true},
		{"build", true, true}, {"build", false, false}, {"build/x", false, true},
		{"dir/other", false, true}, {"dir/keep", false, false}, // dir/* keeps dir traversable
		{"ex/keep", false, true}, // ex/ excludes the parent: no resurrection
		{"rootonly", false, true}, {"sub/rootonly", false, false},
		{"a/deep/x", false, true}, {"deep/x", false, true},
		{"sub/a.log", false, false}, {"sub/b/a.log", false, true}, // deeper wins, both ways
		{"sub/local", false, true}, {"local", false, false},
		{"sub/anchored-here", false, true}, {"sub/x/anchored-here", false, false},
		{"sub/nested.txt", false, true},
		{"info-ignored", false, true}, {"global-ignored", false, true}, {"global-ignored-but-info-keeps", false, false},
		{"x.tmp", false, true}, {"plain", false, false},
	} {
		if got := l.excluded(c.path, c.isDir); got != c.want {
			t.Errorf("excluded(%q, %v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
	fold := layered{dirs: map[string]string{"": "README\n*.TXT\n"}, fold: true}
	if !fold.excluded("readme", false) || !fold.excluded("a.txt", false) || fold.excluded("readm", false) {
		t.Error("ignoreCase matching")
	}
	exact := layered{dirs: map[string]string{"": "README\n"}}
	if exact.excluded("readme", false) {
		t.Error("byte-exact matching folded")
	}
	// Literal NFC and NFD pattern spellings are distinct bytes.
	nfc, nfd := "café", "café"
	lit := layered{dirs: map[string]string{"": nfc + "\n"}}
	if !lit.excluded(nfc, false) || lit.excluded(nfd, false) {
		t.Error("pattern text normalized")
	}
}

// The compiled patterns implement go-git's gitignore.Pattern.
func TestIgnorePatternInterface(t *testing.T) {
	ps := parseIgnore([]byte("*.o\n!keep.o\nbuild/\n"), "", false)
	var gp []gitignore.Pattern
	for i := range ps {
		gp = append(gp, &ps[i])
	}
	m := gitignore.NewMatcher(gp)
	if !m.Match([]string{"a", "x.o"}, false) || m.Match([]string{"keep.o"}, false) || !m.Match([]string{"build"}, true) || m.Match([]string{"build"}, false) {
		t.Fatal("go-git matcher over compiled patterns")
	}
	if ps[1].Match([]string{"keep.o"}, false) != gitignore.Include || ps[0].Match([]string{"x.c"}, false) != gitignore.NoMatch {
		t.Fatal("match results")
	}
}
