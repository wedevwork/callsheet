//go:build gitoracle

package workspacetransfer

// Developer-only generator of the Git configuration oracle table
// (testdata/gitconfig_oracle.tsv). Not part of any CI run: it needs a real
// git executable, which the product never uses. Regenerate with
//
//	go test -tags=gitoracle -run '^TestGenerateGitConfigOracle$' ./internal/workspacetransfer
//
// It enumerates every key `git help --config` lists (placeholders
// expanded) plus the repository extensions Git 2.43 parses but does not
// list, writes each key with a fixed set of malformed values into a
// minimal repository's .git/config (context v0: repository format version
// 0; v1: version 1, for extensions.*), runs `git status --porcelain` with
// an isolated HOME and GIT_CONFIG_NOSYSTEM=1, and records whether Git
// accepts (exit 0) or refuses. Keys Git refuses for any of those values
// are then probed with a wider vocabulary so the table pins each key's
// accepted values, and their standard values are also tried in the other
// places Git reads configuration: a file included with include.path
// (context include), the global ~/.gitconfig (global) and, with
// extensions.worktreeConfig set, .git/config.worktree (worktree).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// oracleNoValue marks a key written without "=" (Git's implicit true).
const oracleNoValue = "<novalue>"

// oracleStandard are the values every key is tried with.
var oracleStandard = []string{"0\v", "0\f", "garbage", "", oracleNoValue, "1K", "-1"}

// oracleVocabulary are the extra values tried for keys Git validates.
var oracleVocabulary = []string{
	"true", "false", "yes", "no", "on", "off", "TRUE", "Off",
	"0", "1", "2", "3", "4", "5", "7", "8", "9", "10", "12", "16", "32", "39", "40", "41", "64", "65", "100", "256", "1000", "-2",
	"2147483647", "2147483648", "4294967296", "0x10", "010", "1k", "1m", "1g", "1t", "1 ", " 1", "1\r",
	"auto", "always", "never", "input", "warn", "keep", "default", "minimal", "link", "rename", "group", "all", "world", "everybody",
	"umask", "0644", "0660", "0600", "0777", "0666", "0750", "none", "normal", "matching", "simple", "current", "upstream", "nothing",
	"tracking", "inherit", "log", "short", "diff", "myers", "patience", "histogram", "zebra", "dimmed-zebra", "dimmed_zebra", "plain",
	"blocks", "ignore-space-at-eol", "ignore-space-change", "ignore-all-space", "allow-indentation-change", "no,ignore-space-change",
	"old", "new", "context", "old,new", "skipping", "noop", "consecutive", "sha1", "SHA1", "sha256", "files", "reftable", "copies", "copy",
	"column", "row", "dense", "nodense", "column,dense", "plain,always", "UTF-8", "latin1", "#", "%", ";", "ab", "dirty", "untracked",
	"trailing-space", "-trailing-space", "space-before-tab,tab-in-indent", "cr-at-eol", "blank-at-eol", "tabwidth=4", "tabwidth=0", "bogus",
	"objects", "derived-metadata", "reference", "committed", "added", "index", "pack", "loose-object", "-loose-object", "fsync",
	"writeout-only", "batch", "lines", "changes", "cumulative", "10", "10%", "files,10", "noncumulative",
	"red", "red bold", "bold red", "#ff0000", "#fff", "255", "256", "red blue", "red blue green", "brightred", "ul", "reverse", "nobold",
	"no-bold", "reset", "italic", "strike", "blink", "dim", "bold ul blink", "red bold #000000", "-1 red",
	// Separators inside and after keyword lists (code review r6).
	"newxold", "new;old", "new\vold", "new,old", "new,", "new,,old", "allX", "old ,new",
}

// oracleExtraKeys are keys Git 2.43 parses that `git help --config` does
// not list.
var oracleExtraKeys = []string{"extensions.preciousObjects", "extensions.partialClone", "extensions.noop", "extensions.refStorage",
	"extensions.unknownCallsheetProbe"}

var oracleKeySyntax = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*(\.[^\s]+)?\.[A-Za-z][A-Za-z0-9-]*$`)

// oracleKeys expands `git help --config`.
func oracleKeys(t *testing.T) []string {
	out, err := exec.Command("git", "help", "--config").Output()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var keys []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "branch.<name>.") {
			l = "branch.main." + strings.TrimPrefix(l, "branch.<name>.")
		}
		l = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(l, "x")
		l = strings.ReplaceAll(l, ".*.", ".x.")
		l = strings.TrimSuffix(l, ".*") + map[bool]string{true: ".x", false: ""}[strings.HasSuffix(l, ".*")]
		if !oracleKeySyntax.MatchString(l) || seen[l] {
			continue
		}
		seen[l] = true
		keys = append(keys, l)
	}
	for _, k := range oracleExtraKeys {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// oracleLine is the configuration text of key = value.
func oracleLine(key, value string) string {
	sec, rest, _ := strings.Cut(key, ".")
	name := rest
	hdr := "[" + sec + "]"
	if i := strings.LastIndexByte(rest, '.'); i >= 0 {
		hdr = "[" + sec + " \"" + rest[:i] + "\"]"
		name = rest[i+1:]
	}
	if value == oracleNoValue {
		return hdr + "\n\t" + name + "\n"
	}
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	return hdr + "\n\t" + name + " = \"" + q + "\"\n"
}

type oracleRepo struct {
	dir, home string
	base      map[string]string
	env       []string
}

func newOracleRepo(t *testing.T) *oracleRepo {
	dir, home := t.TempDir(), t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b", "LC_ALL=C")
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "f"), []byte("f\n"), 0o644)
	run("add", "f")
	run("commit", "-qm", "c")
	cfg, _ := os.ReadFile(filepath.Join(dir, ".git/config"))
	v1 := strings.Replace(string(cfg), "repositoryformatversion = 0", "repositoryformatversion = 1", 1)
	return &oracleRepo{dir: dir, home: home, env: env, base: map[string]string{
		"v0": string(cfg), "v1": v1, "include": string(cfg) + "[include]\n\tpath = included.cfg\n", "global": string(cfg),
		"worktree": v1 + "[extensions]\n\tworktreeConfig = true\n"}}
}

// accepts writes key = value in context ctxName and runs git status.
func (r *oracleRepo) accepts(t *testing.T, ctxName, key, value string) bool {
	line := oracleLine(key, value)
	files := map[string]string{filepath.Join(r.dir, ".git/config"): r.base[ctxName], filepath.Join(r.dir, ".git/included.cfg"): "",
		filepath.Join(r.home, ".gitconfig"): "", filepath.Join(r.dir, ".git/config.worktree"): ""}
	switch ctxName {
	case "v0", "v1":
		files[filepath.Join(r.dir, ".git/config")] += line
	case "include":
		files[filepath.Join(r.dir, ".git/included.cfg")] = line
	case "global":
		files[filepath.Join(r.home, ".gitconfig")] = line
	case "worktree":
		files[filepath.Join(r.dir, ".git/config.worktree")] = line
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "git", "-C", r.dir, "status", "--porcelain")
	c.Env = r.env
	c.Stdout, c.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return c.Run() == nil
}

func TestGenerateGitConfigOracle(t *testing.T) {
	ver, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	keys := oracleKeys(t)
	type row struct {
		key, ctx, value string
		ok              bool
	}
	rows := make([][]row, len(keys))
	const workers = 8
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newOracleRepo(t)
			for i := range next {
				k := keys[i]
				ctxs := []string{"v0"}
				if strings.HasPrefix(strings.ToLower(k), "extensions.") {
					ctxs = []string{"v0", "v1"}
				}
				anyFailing := false
				for _, c := range ctxs {
					failing := false
					var out []row
					for _, val := range oracleStandard {
						ok := r.accepts(t, c, k, val)
						failing = failing || !ok
						out = append(out, row{k, c, val, ok})
					}
					if failing {
						for _, val := range oracleVocabulary {
							out = append(out, row{k, c, val, r.accepts(t, c, k, val)})
						}
					}
					anyFailing = anyFailing || failing
					rows[i] = append(rows[i], out...)
				}
				if anyFailing && !strings.HasPrefix(strings.ToLower(k), "extensions.") {
					for _, c := range []string{"include", "global", "worktree"} {
						for _, val := range oracleStandard {
							rows[i] = append(rows[i], row{k, c, val, r.accepts(t, c, k, val)})
						}
					}
				}
			}
		}()
	}
	for i := range keys {
		next <- i
	}
	close(next)
	wg.Wait()
	var b strings.Builder
	fmt.Fprintf(&b, "# Git configuration oracle (generated; do not edit).\n")
	fmt.Fprintf(&b, "# git: %s (/usr/bin/git; Ubuntu package 1:2.43.0-1ubuntu7.3 on the generating host)\n", strings.TrimSpace(string(ver)))
	fmt.Fprintf(&b, "# command: git -C <repo> status --porcelain, with HOME isolated, GIT_CONFIG_NOSYSTEM=1, LC_ALL=C; the key written last in .git/config\n")
	fmt.Fprintf(&b, "# generator: go test -tags=gitoracle -run '^TestGenerateGitConfigOracle$' ./internal/workspacetransfer\n")
	fmt.Fprintf(&b, "# generated: %s\n", time.Now().UTC().Format("2006-01-02"))
	quote := func(vs []string) string {
		var q []string
		for _, v := range vs {
			if v == oracleNoValue {
				q = append(q, v)
			} else {
				q = append(q, strconv.Quote(v))
			}
		}
		return strings.Join(q, " ")
	}
	fmt.Fprintf(&b, "# standard values: %s\n", quote(oracleStandard))
	fmt.Fprintf(&b, "# vocabulary: %s\n", quote(oracleVocabulary))
	fmt.Fprintf(&b, "# columns: key, context (v0, v1, include, global, worktree), verdicts: one letter per value (A accept, R refuse),\n")
	fmt.Fprintf(&b, "# the standard values, then (for a key Git refuses with a standard value, in contexts v0 and v1) the vocabulary\n")
	for _, rs := range rows {
		verdicts := map[string]*strings.Builder{}
		var order []string
		for _, r := range rs {
			k := r.key + "\t" + r.ctx
			if verdicts[k] == nil {
				verdicts[k] = &strings.Builder{}
				order = append(order, k)
			}
			verdicts[k].WriteString(map[bool]string{true: "A", false: "R"}[r.ok])
		}
		for _, k := range order {
			fmt.Fprintf(&b, "%s\t%s\n", k, verdicts[k].String())
		}
	}
	if err := os.WriteFile(filepath.Join("testdata", "gitconfig_oracle.tsv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// oraclePackedShapes are extra packed-refs records (after a valid record
// for refs/heads/main, which HEAD resolves through) and whole-file
// variants.
var oraclePackedShapes = func() []string {
	h := strings.Repeat("ab", 20)
	var out []string
	for _, n := range []string{"", "refs/tags/a", "refs/tags//a", "refs/tags/./a", "refs/tags/../a", "refs/../x", "/refs/tags/a",
		"refs/tags/a/", "refs/", "refs", "refs/tags/a.lock", "refs/tags/.a", "refs/tags/a..b", "refs/tags/a b", "refs/tags/a\tb",
		"refs/tags/a~1", "refs/tags/a^", "refs/tags/a:b", "refs/tags/a?", "refs/tags/a*", "refs/tags/a[", "refs/tags/a\\b",
		"refs/tags/a@{1}", "refs/tags/@", "@", "HEAD", "FOO_BAR", "FOO-BAR", "foo", "Foo", "refs/tags/a.", "refs/tags/a\r",
		"refs/tags/a\v", "refs/tags/a\x7f", "refs/tags/é", "refs/tags/a/../../b", "refs/tags/...", "refs//", "refs/tags/a/.",
		"refs/tags/a/..", "x/../y", " refs/tags/a", "refs/tags/a ", "refs/./tags", "refs/heads/main", "refs/tags/a/b/c"} {
		out = append(out, h+" "+n+"\n")
	}
	out = append(out,
		h+"\trefs/tags/tab\n", h+"\rrefs/tags/cr\n", h+"  refs/tags/two\n", h+"\vrefs/tags/vt\n", h+"refs/tags/none\n",
		strings.ToUpper(h)+" refs/tags/upper\n", h[:39]+" refs/tags/short\n", h+"a refs/tags/long\n", h+" refs/tags/p\n^"+h+"\n",
		h+" refs/tags/p\n^"+strings.ToUpper(h)+"\n", h+" refs/tags/p\n^"+h+" \n", h+" refs/tags/p\n^"+h+"\n^"+h+"\n", "^"+h+"\n",
		h+" refs/tags/p\n^"+h, "\n", "# comment\n", " \n", h+" refs/tags/unterminated", h+"\n"+"refs/tags/split\n")
	return out
}()

// TestGenerateGitPackedRefsOracle writes testdata/gitpackedrefs_oracle.tsv:
// for each extra record shape, whether git status succeeds and HEAD
// (refs/heads/main, packed) still resolves.
func TestGenerateGitPackedRefsOracle(t *testing.T) {
	ver, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	r := newOracleRepo(t)
	head, err := exec.Command("git", "-C", r.dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	h := strings.TrimSpace(string(head))
	os.Remove(filepath.Join(r.dir, ".git/refs/heads/main"))
	var b strings.Builder
	fmt.Fprintf(&b, "# Git packed-refs oracle (generated; do not edit).\n")
	fmt.Fprintf(&b, "# git: %s (/usr/bin/git; Ubuntu package 1:2.43.0-1ubuntu7.3 on the generating host)\n", strings.TrimSpace(string(ver)))
	fmt.Fprintf(&b, "# command: git -C <repo> status --porcelain and git rev-parse --verify HEAD, HOME isolated, GIT_CONFIG_NOSYSTEM=1, LC_ALL=C\n")
	fmt.Fprintf(&b, "# file: a header, \"<HEAD commit> refs/heads/main\\n\", then the shape (the HEAD commit stands for ab...ab); header\n")
	fmt.Fprintf(&b, "# \"# pack-refs with: peeled fully-peeled \" (unsorted: Git sorts, so it reads every record) and with \"sorted\" (Git binary-searches\n")
	fmt.Fprintf(&b, "# without reading every record)\n")
	fmt.Fprintf(&b, "# generator: go test -tags=gitoracle -run '^TestGenerateGitPackedRefsOracle$' ./internal/workspacetransfer\n")
	fmt.Fprintf(&b, "# generated: %s\n", time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "# columns: shape (Go-quoted), unsorted accept|refuse, sorted accept|refuse\n")
	for _, shape := range oraclePackedShapes {
		body := h + " refs/heads/main\n" + strings.ReplaceAll(shape, strings.Repeat("ab", 20), h)
		body = strings.ReplaceAll(body, strings.ToUpper(strings.Repeat("ab", 20)), strings.ToUpper(h))
		body = strings.ReplaceAll(body, strings.Repeat("ab", 20)[:39], h[:39])
		var res []string
		for _, header := range []string{"# pack-refs with: peeled fully-peeled \n", "# pack-refs with: peeled fully-peeled sorted \n"} {
			if err := os.WriteFile(filepath.Join(r.dir, ".git/packed-refs"), []byte(header+body), 0o644); err != nil {
				t.Fatal(err)
			}
			ok := true
			for _, args := range [][]string{{"status", "--porcelain"}, {"rev-parse", "--verify", "HEAD"}} {
				c := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
				c.Env = r.env
				c.Stdout, c.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
				ok = ok && c.Run() == nil
			}
			res = append(res, map[bool]string{true: "accept", false: "refuse"}[ok])
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", strconv.Quote(shape), res[0], res[1])
	}
	if err := os.WriteFile(filepath.Join("testdata", "gitpackedrefs_oracle.tsv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// oracleMetadataFiles are the files below .git that Git may read when it
// opens a repository or runs status, besides the configuration and
// packed-refs (their own tables).
var oracleMetadataFiles = []string{"HEAD", "refs/heads/main", "index", "shallow", "commondir", "gitdir", "config.worktree",
	"objects/info/alternates", "objects/info/http-alternates", "objects/info/commit-graph", "objects/info/packs",
	"objects/pack/multi-pack-index", "info/grafts", "info/sparse-checkout", "info/exclude", "info/attributes", "MERGE_HEAD",
	"CHERRY_PICK_HEAD", "REVERT_HEAD", "ORIG_HEAD", "FETCH_HEAD", "AUTO_MERGE", "MERGE_MSG", "MERGE_MODE", "BISECT_LOG",
	"BISECT_START", "logs/HEAD", "rebase-merge/head-name", "rebase-apply/head-name", "sequencer/todo", "description"}

// oracleMetadataValues replace the file's content (HEAD and the branch
// keep a valid prefix: the injected bytes follow it).
var oracleMetadataValues = []string{"garbage\n", "0\v", "\f", "\r", "\x00", " ", "\n", ""}

// TestGenerateGitMetadataOracle writes testdata/gitmetadata_oracle.tsv:
// for each metadata file and content, whether git status succeeds with
// HEAD resolving.
func TestGenerateGitMetadataOracle(t *testing.T) {
	ver, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Git metadata-file oracle (generated; do not edit).\n")
	fmt.Fprintf(&b, "# git: %s (/usr/bin/git; Ubuntu package 1:2.43.0-1ubuntu7.3 on the generating host)\n", strings.TrimSpace(string(ver)))
	fmt.Fprintf(&b, "# command: git -C <repo> status --porcelain and git rev-parse --verify HEAD, HOME isolated, GIT_CONFIG_NOSYSTEM=1, LC_ALL=C\n")
	fmt.Fprintf(&b, "# content: the file is created (or replaced) with the value; for HEAD and refs/heads/main the value follows the valid content\n")
	fmt.Fprintf(&b, "# generator: go test -tags=gitoracle -run '^TestGenerateGitMetadataOracle$' ./internal/workspacetransfer\n")
	fmt.Fprintf(&b, "# generated: %s\n", time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "# columns: file, value (Go-quoted), accept|refuse\n")
	for _, f := range oracleMetadataFiles {
		for _, v := range oracleMetadataValues {
			r := newOracleRepo(t)
			p := filepath.Join(r.dir, ".git", filepath.FromSlash(f))
			content := v
			switch f {
			case "HEAD":
				content = "ref: refs/heads/main" + strings.TrimSuffix(v, "\n") + "\n"
			case "refs/heads/main":
				old, _ := os.ReadFile(p)
				content = strings.TrimSpace(string(old)) + strings.TrimSuffix(v, "\n") + "\n"
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			ok := true
			for _, args := range [][]string{{"status", "--porcelain"}, {"rev-parse", "--verify", "HEAD"}} {
				c := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
				c.Env = r.env
				c.Stdout, c.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
				ok = ok && c.Run() == nil
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", f, strconv.Quote(v), map[bool]string{true: "accept", false: "refuse"}[ok])
		}
	}
	if err := os.WriteFile(filepath.Join("testdata", "gitmetadata_oracle.tsv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGenerateGitGrammarOracle writes testdata/gitgrammar_oracle.tsv: for
// each fuzzed key (gitgrammar_test.go), one verdict letter per generated
// value, and reports every disagreement with the key's rule.
func TestGenerateGitGrammarOracle(t *testing.T) {
	ver, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(grammarKeys))
	var wg sync.WaitGroup
	for i, g := range grammarKeys {
		wg.Add(1)
		go func(i int, key string, words []string) {
			defer wg.Done()
			r := newOracleRepo(t)
			var b strings.Builder
			for _, v := range grammarValues(words) {
				ok := r.accepts(t, "v0", key, v)
				b.WriteString(map[bool]string{true: "A", false: "R"}[ok])
				if rule := ruleFor(normalizeKey(key)); rule == nil || rule(v, false) != ok {
					t.Errorf("disagreement: %s = %q: Git %v", key, v, ok)
				}
			}
			out[i] = key + "\t" + b.String()
		}(i, g.key, g.words)
	}
	wg.Wait()
	var b strings.Builder
	fmt.Fprintf(&b, "# Git value-grammar oracle (generated; do not edit).\n")
	fmt.Fprintf(&b, "# git: %s (/usr/bin/git; Ubuntu package 1:2.43.0-1ubuntu7.3 on the generating host)\n", strings.TrimSpace(string(ver)))
	fmt.Fprintf(&b, "# command: git -C <repo> status --porcelain, HOME isolated, GIT_CONFIG_NOSYSTEM=1, LC_ALL=C; key = \"value\" last in .git/config\n")
	fmt.Fprintf(&b, "# values: grammarValues(words) of gitgrammar_test.go, in order; one letter each (A accept, R refuse)\n")
	fmt.Fprintf(&b, "# generator: go test -tags=gitoracle -run '^TestGenerateGitGrammarOracle$' ./internal/workspacetransfer\n")
	fmt.Fprintf(&b, "# generated: %s\n", time.Now().UTC().Format("2006-01-02"))
	for _, l := range out {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(filepath.Join("testdata", "gitgrammar_oracle.tsv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGenerateGitStateOracle writes testdata/gitstate_oracle.tsv: for each
// repository-state scenario (gitstate_test.go), whether git status
// succeeds with HEAD resolving.
func TestGenerateGitStateOracle(t *testing.T) {
	ver, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Git repository-state oracle (generated; do not edit).\n")
	fmt.Fprintf(&b, "# git: %s (/usr/bin/git; Ubuntu package 1:2.43.0-1ubuntu7.3 on the generating host)\n", strings.TrimSpace(string(ver)))
	fmt.Fprintf(&b, "# command: git -C <repo> status --porcelain and git rev-parse --verify HEAD, HOME isolated, GIT_CONFIG_NOSYSTEM=1, LC_ALL=C,\n")
	fmt.Fprintf(&b, "# plus the scenario's environment; repository: files f and d/g committed on main (see stateScenarios)\n")
	fmt.Fprintf(&b, "# generator: go test -tags=gitoracle -run '^TestGenerateGitStateOracle$' ./internal/workspacetransfer\n")
	fmt.Fprintf(&b, "# generated: %s\n", time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "# columns: scenario, accept|refuse\n")
	for _, s := range stateScenarios {
		r := newOracleRepo(t)
		git := func(env []string, args ...string) (string, bool) {
			c := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
			c.Env = env
			var out bytes.Buffer
			c.Stdout, c.Stderr = &out, &bytes.Buffer{}
			err := c.Run()
			return strings.TrimSpace(out.String()), err == nil
		}
		os.MkdirAll(filepath.Join(r.dir, "d"), 0o755)
		os.WriteFile(filepath.Join(r.dir, "d/g"), []byte("g\n"), 0o644)
		git(r.env, "add", "d/g")
		git(r.env, "commit", "-qm", "d")
		head, _ := git(r.env, "rev-parse", "HEAD")
		tree, _ := git(r.env, "rev-parse", "HEAD^{tree}")
		sub, _ := git(r.env, "rev-parse", "HEAD:d")
		blob, _ := git(r.env, "rev-parse", "HEAD:f")
		other, _ := git(r.env, "commit-tree", tree, "-m", "other")
		o := stateObjects{head: head, tree: tree, subtree: sub, blob: blob, other: other, missing: strings.Repeat("ab", 20), missing2: strings.Repeat("cd", 20)}
		s.apply(t, filepath.Join(r.dir, ".git"), o)
		env := append([]string(nil), r.env...)
		for k, v := range s.env {
			env = append(env, k+"="+o.expand(v))
		}
		_, ok1 := git(env, "status", "--porcelain")
		_, ok2 := git(env, "rev-parse", "--verify", "HEAD")
		fmt.Fprintf(&b, "%s\t%s\n", s.name, map[bool]string{true: "accept", false: "refuse"}[ok1 && ok2])
	}
	if err := os.WriteFile(filepath.Join("testdata", "gitstate_oracle.tsv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
