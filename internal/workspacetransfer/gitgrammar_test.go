package workspacetransfer

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Code review r6: the hand-written value parsers of gitconfig_rules.go
// are checked against Git 2.43.0 with a generated grammar fuzz: for each
// parser (one representative key), its keywords alone and in case
// variants, joined by every ASCII byte, prefixed and suffixed by every
// ASCII byte, truncated and extended. testdata/gitgrammar_oracle.tsv holds
// Git's verdict for every generated value (gitoracle_gen_test.go
// regenerates it).

// grammarKeys are the fuzzed keys and their keywords.
var grammarKeys = []struct {
	key   string
	words []string
}{
	{"color.diff.new", []string{"red", "bold", "#ff0000", "255", "reset", "no-bold", "brightred", "normal", "default", "ul", "-1"}},
	{"color.ui", []string{"never", "always", "auto", "true"}},
	{"diff.colorMoved", []string{"plain", "blocks", "zebra", "dimmed-zebra", "default", "no", "true"}},
	{"diff.colorMovedWS", []string{"ignore-space-change", "allow-indentation-change", "no", "ignore-all-space", "ignore-space-at-eol"}},
	{"diff.wsErrorHighlight", []string{"new", "old", "context", "all", "none", "default"}},
	{"column.ui", []string{"always", "never", "auto", "plain", "column", "row", "dense", "nodense"}},
	{"core.abbrev", []string{"auto", "no", "7", "40", "false"}},
	{"core.sharedRepository", []string{"group", "umask", "all", "world", "everybody", "0664", "true", "1"}},
	{"core.commentChar", []string{"auto", "#", ";"}},
	{"diff.x.binary", []string{"auto", "true", "0"}},
	{"core.autocrlf", []string{"input", "true"}},
	{"core.safecrlf", []string{"warn", "true"}},
	{"core.logAllRefUpdates", []string{"always", "true"}},
	{"diff.renames", []string{"copies", "copy", "true"}},
	{"branch.autoSetupMerge", []string{"always", "inherit", "simple", "true"}},
	{"branch.autoSetupRebase", []string{"never", "local", "remote", "always"}},
	{"core.createObject", []string{"rename", "link"}},
	{"diff.algorithm", []string{"myers", "minimal", "patience", "histogram", "default"}},
	{"diff.ignoreSubmodules", []string{"none", "untracked", "dirty", "all"}},
	{"fetch.negotiationAlgorithm", []string{"consecutive", "skipping", "noop", "default"}},
	{"push.default", []string{"nothing", "current", "upstream", "tracking", "simple", "matching"}},
	{"status.showUntrackedFiles", []string{"no", "normal", "all"}},
	{"core.fileMode", []string{"true", "false", "1", "0x1", "1k"}},
	{"diff.context", []string{"3", "0x10", "1k", "010"}},
	{"core.bigFileThreshold", []string{"512m", "1", "0x10"}},
	{"core.compression", []string{"9", "-1", "0"}},
}

// grammarValues generates the fuzz values of a keyword list.
func grammarValues(words []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if !seen[v] && !strings.ContainsAny(v, "\n\x00") {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, w := range words {
		add(w)
		add(strings.ToUpper(w))
		if w != "" {
			add(strings.ToUpper(w[:1]) + w[1:])
			add(w[:len(w)-1])
		}
		add(w + "x")
		add("x" + w)
	}
	second := words[0]
	if len(words) > 1 {
		second = words[1]
	}
	for b := byte(1); b < 0x7f; b++ {
		s := string([]byte{b})
		add(words[0] + s + second)
		add(words[0] + s)
		add(s + words[0])
		add(words[0] + s + s + second)
	}
	return out
}

// TestGitGrammarOracle checks every fuzz verdict against the key's rule
// and pushes a rotating sample (all refused values of the reviewer's
// cases included) through the production entry point.
func TestGitGrammarOracle(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "gitgrammar_oracle.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	verdicts := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if l := sc.Text(); !strings.HasPrefix(l, "#") {
			k, v, _ := strings.Cut(l, "\t")
			verdicts[k] = v
		}
	}
	type pcase struct {
		key, value string
		accept     bool
	}
	var pushes []pcase
	n := 0
	for _, g := range grammarKeys {
		values := grammarValues(g.words)
		v := verdicts[g.key]
		if len(v) != len(values) {
			t.Fatalf("%s: %d verdicts for %d values (regenerate the table)", g.key, len(v), len(values))
		}
		rule := ruleFor(normalizeKey(g.key))
		if rule == nil {
			t.Fatalf("%s has no rule", g.key)
		}
		for i, val := range values {
			accept := v[i] == 'A'
			n++
			if rule(val, false) != accept {
				t.Errorf("%s = %q: Git %v, rule %v", g.key, val, accept, !accept)
			}
			pushes = append(pushes, pcase{g.key, val, accept})
		}
	}
	if n < 5000 {
		t.Fatalf("grammar oracle truncated: %d values", n)
	}
	// The reviewer's cases, with Git 2.43.0's verdicts.
	for v, ok := range map[string]bool{"newxold": false, "new\vold": false, "new;old": false, "allX": false, "new,old": true, "new,": true, ",new": false} {
		pushes = append(pushes, pcase{"diff.wsErrorHighlight", v, ok})
	}
	r := newRepo(t, map[string]fspec{"f": reg("f\n")})
	env := emptyEnv(t)
	run := sampler(t)
	for i, c := range pushes {
		if i < len(pushes)-7 && i%23 != 0 || !run(i) {
			continue
		}
		mustWrite(t, filepath.Join(r.root, ".git/config"), baseFor("v0")+oracleRow{key: c.key, value: c.value}.line())
		_, err := fastDeps().push(context.Background(), Options{GOOS: "linux", Env: env, Cwd: "/", Plane: &localPlane{t: t}},
			PushRequest{Name: "ws", Instance: tInst, Path: r.root, PathSet: true})
		switch {
		case c.accept && err != nil:
			t.Errorf("%s = %q: Git accepts it, Push refused: %v", c.key, c.value, err)
		case !c.accept && contract.TransferReason(err) != contract.ReasonUnsupportedRepository:
			t.Errorf("%s = %q: Git refuses it, Push: %v", c.key, c.value, err)
		}
	}
}
