package workspacetransfer

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Code review r4: every metadata file and configuration value the
// transfer reads is parsed with Git's exact rule. The expected outcomes
// were taken from Git 2.43.0 (Ubuntu 24.04 package 1:2.43.0-1ubuntu7.3,
// /usr/bin/git) on identical fixtures: "accept" when git status succeeds,
// HEAD resolves to the commit and the repository is neither shallow nor
// using an alternate store; "refuse" otherwise. The few deliberate
// divergences are refusals (the safe direction) and are marked.

// injected are the bytes injected into every value: vertical tab, form
// feed, a lone CR, NUL, a trailing space and an extra LF.
var injected = []struct{ name, b string }{
	{"vt", "\v"}, {"ff", "\f"}, {"cr", "\r"}, {"nul", "\x00"}, {"sp", " "}, {"lf", "\n"},
}

type metadataCase struct {
	what   string
	setup  func(t *testing.T, r *fixtureRepo)
	accept bool
	note   string // a deliberate divergence from Git (always a refusal)
}

// metadataCases builds the matrix: each target with each injected byte.
func metadataCases() []metadataCase {
	var cases []metadataCase
	write := func(rel, s string) func(*testing.T, *fixtureRepo) {
		return func(t *testing.T, r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, rel), s) }
	}
	cfg := func(s string) func(*testing.T, *fixtureRepo) {
		return func(_ *testing.T, r *fixtureRepo) { r.config(s) }
	}
	for _, x := range injected {
		in := func(what string) string { return what + "+" + x.name }
		// shallow: Git refuses every nonempty shallow file ("bad shallow
		// line"); an empty one makes the repository shallow.
		cases = append(cases, metadataCase{what: in("shallow"), setup: write(".git/shallow", x.b)})
		// Alternates: an entry is a line that is neither empty nor a
		// comment; Git then uses (or complains about) an alternate store,
		// which Callsheet refuses. A lone LF has no entry.
		for _, f := range []string{"alternates", "http-alternates"} {
			c := metadataCase{what: in(f), setup: write(".git/objects/info/"+f, x.b), accept: x.name == "lf"}
			if !c.accept {
				c.note = "Git continues past a broken alternate entry; any alternate entry is refused"
			}
			cases = append(cases, c)
		}
		// HEAD and a loose branch: Git trims space, tab, CR and LF only;
		// \v and \f make them corrupt. Git truncates at NUL (a C string);
		// Callsheet refuses NUL.
		for _, t := range []struct{ what, rel, content string }{
			{"HEAD", ".git/HEAD", "ref: refs/heads/main"},
			{"loose ref", ".git/refs/heads/main", ""},
		} {
			c := metadataCase{what: in(t.what), accept: x.name == "cr" || x.name == "sp" || x.name == "lf"}
			rel, content, b := t.rel, t.content, x.b
			c.setup = func(tt *testing.T, r *fixtureRepo) {
				v := content
				if v == "" {
					v = r.head.String()
				}
				mustWrite(tt, filepath.Join(r.root, rel), v+b+"\n")
			}
			if x.name == "nul" {
				c.note = "Git truncates at NUL; a NUL in ref metadata is refused"
			}
			cases = append(cases, c)
		}
		// packed-refs: every byte breaks the only ref (an invalid name is
		// ignored, an empty line is fatal), so HEAD does not resolve.
		b := x.b
		cases = append(cases, metadataCase{what: in("packed-refs"), setup: func(tt *testing.T, r *fixtureRepo) {
			os.Remove(filepath.Join(r.root, ".git/refs/heads/main"))
			mustWrite(tt, filepath.Join(r.root, ".git/packed-refs"), "# pack-refs with: peeled fully-peeled sorted \n"+r.head.String()+" refs/heads/main"+b+"\n")
		}})
		// Configuration values Git parses strictly (booleans and the
		// format version): \v and \f are invalid trailing bytes, a lone
		// CR, a trailing space and an LF are whitespace Git drops. Git
		// truncates at NUL; Callsheet refuses it.
		for _, kv := range []string{"filemode = 0", "filemode = true", "bare = false", "repositoryformatversion = 0", "symlinks = true",
			"ignorecase = false", "precomposeunicode = false", "sparsecheckout = false", "autocrlf = false", "autocrlf = input"} {
			c := metadataCase{what: in("core." + kv), setup: cfg("[core]\n\t" + kv + x.b + "\n"), accept: x.name == "cr" || x.name == "sp" || x.name == "lf"}
			if x.name == "nul" {
				c.note = "Git truncates a value at NUL; a NUL in a parsed value is refused"
			}
			cases = append(cases, c)
		}
		// core.eol: Git treats any value it does not know as unset.
		cases = append(cases, metadataCase{what: in("core.eol"), setup: cfg("[core]\n\teol = lf" + x.b + "\n"), accept: true})
		c := metadataCase{what: in("extensions.worktreeConfig"), setup: cfg("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tworktreeConfig = false" + x.b + "\n"),
			accept: x.name == "cr" || x.name == "sp" || x.name == "lf"}
		if x.name == "nul" {
			c.note = "Git truncates a value at NUL; a NUL in a parsed value is refused"
		}
		cases = append(cases, c)
	}
	// Integer spellings (git_parse_int: strtoimax base 0 after C isspace,
	// an optional k/m/g unit, the int range) as a boolean and as the
	// format version (0 or 1 only).
	for _, n := range []struct {
		v             string
		asBool, asVer bool
	}{
		{"0x0", true, true}, {"00", true, true}, {"+0", true, true}, {"0k", true, true}, {"1K", true, false}, {" 0", true, true},
		{"\v0", true, true}, {"\f1", true, true}, {"\t0", true, true}, {"\r0", true, true}, {"1g", true, false}, {"1m", true, false},
		{"0X10", true, false}, {"010", true, false}, {"0 ", false, false}, {"08", false, false}, {"2147483648", false, false},
		{"9223372036854775808", false, false}, {"0x", false, false}, {"1kb", false, false}, {"0 #c", false, false},
		{"TrUe", true, false}, {"Off", true, false}, {"yes", true, false}, {"nope", false, false}, {"-1", true, false},
	} {
		cases = append(cases, metadataCase{what: "core.filemode=" + n.v, setup: cfg("[core]\n\tfilemode = \"" + n.v + "\"\n"), accept: n.asBool})
		c := metadataCase{what: "core.repositoryformatversion=" + n.v, setup: cfg("[core]\n\trepositoryformatversion = \"" + n.v + "\"\n"), accept: n.asVer}
		if n.v == "-1" {
			c.note = "Git accepts a negative format version; only 0 and 1 are supported"
		}
		cases = append(cases, c)
	}
	cases = append(cases,
		metadataCase{what: "empty shallow", setup: write(".git/shallow", "")},
		metadataCase{what: "empty alternates", setup: write(".git/objects/info/alternates", ""), accept: true},
		metadataCase{what: "comment-only alternates", setup: write(".git/objects/info/alternates", "#comment\n\n"), accept: true},
		metadataCase{what: "repositoryformatversion without a value", setup: cfg("[core]\n\trepositoryformatversion\n")},
		metadataCase{what: "empty repositoryformatversion", setup: cfg("[core]\n\trepositoryformatversion =\n")},
		metadataCase{what: "objectformat SHA1", setup: cfg("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = SHA1\n")},
		metadataCase{what: "refstorage Files", setup: cfg("[core]\n\trepositoryformatversion = 1\n[extensions]\n\trefstorage = Files\n")},
		metadataCase{what: "objectformat sha1", setup: cfg("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha1\n"), accept: true},
		metadataCase{what: "core.eol unknown", setup: cfg("[core]\n\teol = bogus\n"), accept: true},
		metadataCase{what: "a malformed boolean overridden later", setup: cfg("[core]\n\tfilemode = 0\v\n[core]\n\tfilemode = true\n")},
		metadataCase{what: "packed-refs blank line", setup: func(t *testing.T, r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), r.head.String()+" refs/tags/t\n\n")
		}},
		metadataCase{what: "packed-refs unterminated", setup: func(t *testing.T, r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), r.head.String()+" refs/tags/t")
		}},
		metadataCase{what: "packed-refs comment after the header", setup: func(t *testing.T, r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), "# pack-refs with: peeled\n# x\n")
		}},
		metadataCase{what: "packed-refs stray peeled line", setup: func(t *testing.T, r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), "^"+r.head.String()+"\n")
		}},
		metadataCase{what: "packed-refs valid peeled line", setup: func(t *testing.T, r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), "# pack-refs with: peeled\n"+r.head.String()+" refs/tags/t\n^"+r.head.String()+"\n")
		}, accept: true},
		metadataCase{what: "packed-refs broken name ignored", setup: func(t *testing.T, r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), r.head.String()+" refs/tags/bad\v\n")
		}, accept: true},
		metadataCase{what: "empty packed-refs", setup: write(".git/packed-refs", ""), accept: true},
	)
	return cases
}

// TestMetadataMatchesGit pushes a clean repository after injecting each
// byte into each metadata file and configuration value, expecting Git's
// outcome (or a marked refusal).
func TestMetadataMatchesGit(t *testing.T) {
	env := emptyEnv(t)
	run := sampler(t)
	for i, c := range metadataCases() {
		if !run(i) {
			continue
		}
		r := newRepo(t, map[string]fspec{"f": reg("f\n")})
		c.setup(t, r)
		err := pushLocal(t, r.root, env)
		switch {
		case c.accept && err != nil:
			t.Errorf("%q: Git accepts it, Push refused: %v", c.what, err)
		case !c.accept && err == nil:
			t.Errorf("%q: Git refuses it (or a marked divergence: %s), Push accepted", c.what, c.note)
		case !c.accept && contract.CodeOf(err) != contract.CodeInvalidArgument && contract.CodeOf(err) != contract.CodeConflict:
			t.Errorf("%q: refused with an unexpected error: %v", c.what, err)
		}
	}
}

// TestParseGitInt covers git_parse_int directly.
func TestParseGitInt(t *testing.T) {
	for _, c := range []struct {
		v  string
		n  int64
		ok bool
	}{
		{"0", 0, true}, {"-0", 0, true}, {"+7", 7, true}, {"0x1f", 31, true}, {"0X1F", 31, true}, {"017", 15, true}, {"1k", 1024, true},
		{"2M", 2 << 20, true}, {"1G", 1 << 30, true}, {" \t\n\v\f\r5", 5, true}, {"-2147483647", -2147483647, true},
		{"2147483647", 2147483647, true}, {"2147483648", 0, false}, {"-2147483648", 0, false}, {"2g", 0, false}, {"", 0, false},
		{"+", 0, false}, {"-", 0, false}, {"0x", 0, false}, {"0xg", 0, false}, {"08", 0, false}, {"1 ", 0, false}, {"1\v", 0, false},
		{"1kk", 0, false}, {"1t", 0, false}, {"99999999999999999999", 0, false}, {"-9223372036854775809", 0, false}, {"1\x00", 0, false},
	} {
		n, ok := parseGitInt(c.v, math.MaxInt32)
		if n != c.n || ok != c.ok {
			t.Errorf("%q: %d %v, want %d %v", c.v, n, ok, c.n, c.ok)
		}
	}
	if n, ok := parseGitInt("9223372036854775807", math.MaxInt64); !ok || n != math.MaxInt64 {
		t.Errorf("max int64: %d %v", n, ok)
	}
	if !strings.Contains(errBadConfig.Error(), "invalid") {
		t.Fatal(errBadConfig)
	}
}
