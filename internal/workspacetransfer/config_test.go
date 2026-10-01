package workspacetransfer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// UT-2/UT-4: the git configuration reader (syntax, precedence, includes
// and conditions, environment sources) behind the status settings and
// core.excludesFile.

func TestParseGitConfig(t *testing.T) {
	src := "\xef\xbb\xbf# comment\n; other\n[core]\n\tfilemode = false ; trailing\n\tbare\n\tname = \"quoted # not comment\" tail \\\n continued\r\n" +
		"[Section \"Sub \\\"q\\\" \\\\x\"] key = v1\n[old.Style]\nK = \"a\\tb\\nc\\\\\\\"\"\n  spaced   =   inner   words   \n[empty]\n"
	var got []string
	err := parseGitConfig([]byte(src), func(sec, sub, name, value string, blank bool) error {
		got = append(got, sec+"|"+sub+"|"+name+"|"+value+"|"+map[bool]string{true: "blank", false: ""}[blank])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"core||filemode|false|", "core||bare||blank", "core||name|quoted # not comment tail  continued|",
		`Section|Sub "q" \x|key|v1|`, "old|style|K|a\tb\nc\\\"|", "old|style|spaced|inner   words|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s", strings.Join(got, "\n"))
	}
	for _, bad := range []string{"key = v\n", "[core\n", "[core] = x\n", "[core]\nk = \"open\n", "[core]\nk = \\q\n", "[ \"x\"]\n",
		"[a \"b\" ]\n", "[core]\n1k = v\n", "[a.b \"c\"]\n", "[a \"b\n"} {
		if err := parseGitConfig([]byte(bad), func(string, string, string, string, bool) error { return nil }); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	c := &gitConfig{}
	parseGitConfig([]byte(src), func(sec, sub, name, value string, blank bool) error {
		c.entries = append(c.entries, configEntry{key: configKey(sec, sub, name), value: value, blank: blank})
		return nil
	})
	if b, _ := c.boolean("core.filemode", true); b {
		t.Fatal("filemode")
	}
	if b, _ := c.boolean("core.bare", false); !b {
		t.Fatal("blank boolean")
	}
	if v, _ := c.str("old.style.k"); v != "a\tb\nc\\\"" {
		t.Fatalf("legacy subsection %q", v)
	}
	if normalizeKey("Core.Sub.Name") != "core.Sub.name" || normalizeKey("Core.X") != "core.x" || normalizeKey("x") != "x" {
		t.Fatal("normalizeKey")
	}
	for v, want := range map[string]bool{"yes": true, "On": true, "1": true, "-2": true, "0x10": true, "no": false, "": false, "0": false} {
		if b, err := parseGitBool(v); err != nil || b != want {
			t.Errorf("parseGitBool(%q) = %v %v", v, b, err)
		}
	}
	if _, err := parseGitBool("maybe"); err == nil {
		t.Error("bad boolean")
	}
}

// configFixture is a repository-like .git directory, a home and optional
// system and XDG files for loadConfig.
type configFixture struct {
	t                    *testing.T
	root, git, home, xdg string
	env                  map[string]string
}

func newConfigFixture(t *testing.T) *configFixture {
	base := tempDir(t)
	f := &configFixture{t: t, root: filepath.Join(base, "repo"), home: filepath.Join(base, "home"), xdg: filepath.Join(base, "xdg")}
	f.git = filepath.Join(f.root, ".git")
	for _, d := range []string{f.git, f.home, filepath.Join(f.xdg, "git")} {
		os.MkdirAll(d, 0o755)
	}
	f.env = map[string]string{"HOME": f.home, "GIT_CONFIG_NOSYSTEM": "1"}
	return f
}

func (f *configFixture) write(p, s string) string {
	mustWrite(f.t, p, s)
	return p
}

func (f *configFixture) load(branch string) (*gitConfig, error) {
	return loadConfig(context.Background(), envOf(f.env), f.git, branch)
}

func (f *configFixture) get(t *testing.T, key, branch string) string {
	t.Helper()
	c, err := f.load(branch)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := c.str(key)
	return v
}

func TestConfigPrecedence(t *testing.T) {
	f := newConfigFixture(t)
	sys := f.write(filepath.Join(f.home, "etc-gitconfig"), "[core]\n\texcludesFile = /sys\n\tignoreCase = true\n")
	f.write(filepath.Join(f.xdg, "git", "config"), "[core]\n\texcludesFile = /xdg\n\tsymlinks = false\n")
	f.write(filepath.Join(f.home, ".gitconfig"), "[core]\n\texcludesFile = /home\n")
	f.write(filepath.Join(f.git, "config"), "[core]\n\tfilemode = false\n")
	f.env["XDG_CONFIG_HOME"] = f.xdg
	if v := f.get(t, "core.excludesfile", ""); v != "/home" {
		t.Fatalf("home over xdg: %q", v)
	}
	f.env["GIT_CONFIG_SYSTEM"] = sys
	if v := f.get(t, "core.ignorecase", ""); v != "" {
		t.Fatalf("GIT_CONFIG_NOSYSTEM honored: %q", v)
	}
	delete(f.env, "GIT_CONFIG_NOSYSTEM")
	if v := f.get(t, "core.ignorecase", ""); v != "true" {
		t.Fatalf("system via GIT_CONFIG_SYSTEM: %q", v)
	}
	f.write(filepath.Join(f.git, "config"), "[core]\n\texcludesFile = /repo\n")
	if v := f.get(t, "core.excludesfile", ""); v != "/repo" {
		t.Fatalf("repository wins: %q", v)
	}
	f.env["GIT_CONFIG_GLOBAL"] = f.write(filepath.Join(f.home, "alt"), "[core]\n\tattributesFile = /alt\n")
	c, _ := f.load("")
	if v, _ := c.str("core.symlinks"); v != "" {
		t.Fatal("GIT_CONFIG_GLOBAL replaces the XDG and home files")
	}
	if v, _ := c.str("core.attributesfile"); v != "/alt" {
		t.Fatal("GIT_CONFIG_GLOBAL read")
	}
	f.env["GIT_CONFIG_COUNT"] = "2"
	f.env["GIT_CONFIG_KEY_0"], f.env["GIT_CONFIG_VALUE_0"] = "Core.ExcludesFile", "/env"
	f.env["GIT_CONFIG_KEY_1"], f.env["GIT_CONFIG_VALUE_1"] = "core.precomposeUnicode", "true"
	if v := f.get(t, "core.excludesfile", ""); v != "/env" {
		t.Fatalf("environment pairs win: %q", v)
	}
	for _, bad := range []map[string]string{
		{"GIT_CONFIG_COUNT": "x"}, {"GIT_CONFIG_COUNT": "1"}, {"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "nodot", "GIT_CONFIG_VALUE_0": "v"},
		{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "include.path", "GIT_CONFIG_VALUE_0": "/x"},
		{"GIT_CONFIG_PARAMETERS": "'core.bare'='true'"}, {"GIT_CONFIG_NOSYSTEM": "maybe"},
	} {
		g := newConfigFixture(t)
		for k, v := range bad {
			g.env[k] = v
		}
		if _, err := g.load(""); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

func TestConfigIncludes(t *testing.T) {
	f := newConfigFixture(t)
	inc := f.write(filepath.Join(f.home, "inc", "a.inc"), "[core]\n\texcludesFile = from-a\n[include]\n\tpath = b.inc\n")
	f.write(filepath.Join(f.home, "inc", "b.inc"), "[core]\n\tattributesFile = from-b\n")
	f.write(filepath.Join(f.home, ".gitconfig"), "[core]\n\texcludesFile = before\n[include]\n\tpath = ~/inc/a.inc\n\tpath = missing.inc\n[core]\n\tignoreCase = after\n")
	c, err := f.load("")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.str("core.excludesfile"); v != "from-a" {
		t.Fatalf("include overrides earlier values: %q", v)
	}
	if v, _ := c.str("core.attributesfile"); v != "from-b" {
		t.Fatalf("relative nested include: %q", v)
	}
	if !strings.Contains(strings.Join(c.files, " "), inc) {
		t.Fatal("included files recorded")
	}
	// Conditional includes: gitdir (with ~/ and trailing /), gitdir/i,
	// onbranch; unsupported conditions are refused.
	rel, _ := filepath.Rel(f.home, f.root)
	f.write(filepath.Join(f.home, "cond.inc"), "[core]\n\tfilemode = false\n")
	for _, c := range []struct {
		cond, branch string
		want         bool
	}{
		{"gitdir:" + f.root + "/", "", true},
		{"gitdir:" + f.root + "/.git", "", true},
		{"gitdir:~/" + rel + "/", "", true},
		{"gitdir:repo/", "", true},
		{"gitdir:other/", "", false},
		{"gitdir/i:" + strings.ToUpper(f.root) + "/", "", true},
		{"gitdir:" + strings.ToUpper(f.root) + "/", "", strings.ToUpper(f.root) == f.root},
		{"onbranch:main", "main", true},
		{"onbranch:feat/", "feat/x", true},
		{"onbranch:main", "", false},
		{"onbranch:main", "dev", false},
	} {
		f.write(filepath.Join(f.home, ".gitconfig"), "[includeIf \""+c.cond+"\"]\n\tpath = cond.inc\n")
		cfg, err := f.load(c.branch)
		if err != nil {
			t.Fatalf("%s: %v", c.cond, err)
		}
		_, got := cfg.get("core.filemode")
		if got != c.want {
			t.Errorf("%s on %q = %v", c.cond, c.branch, got)
		}
	}
	for _, bad := range []string{
		"[includeIf \"hasconfig:remote.*.url:https://x\"]\n\tpath = cond.inc\n",
		"[includeIf \"weird\"]\n\tpath = cond.inc\n",
		"[includeIf \"future:x\"]\n\tpath = cond.inc\n",
		"[include]\n\tpath\n",
		"[include]\n\tpath = ~user/x\n",
		"[include]\n\tpath = cycle.inc\n",
		"[core]\n\tbroken = \"\n",
	} {
		f.write(filepath.Join(f.home, "cycle.inc"), "[include]\n\tpath = cycle.inc\n")
		f.write(filepath.Join(f.home, ".gitconfig"), bad)
		if _, err := f.load("main"); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Deep but acyclic nesting beyond the bound is refused.
	for i := 0; i < 12; i++ {
		f.write(filepath.Join(f.home, "n"+string(rune('a'+i))), "[include]\n\tpath = n"+string(rune('a'+i+1))+"\n")
	}
	f.write(filepath.Join(f.home, ".gitconfig"), "[include]\n\tpath = na\n")
	if _, err := f.load(""); err == nil {
		t.Error("include depth unbounded")
	}
	// An unreadable file fails; a directory named like the file is absent.
	f.write(filepath.Join(f.home, ".gitconfig"), "")
	os.Remove(filepath.Join(f.home, ".gitconfig"))
	os.Mkdir(filepath.Join(f.home, ".gitconfig"), 0o755)
	if _, err := f.load(""); err == nil {
		t.Error("a directory config read")
	}
}

func TestEnvHelpers(t *testing.T) {
	e := ProcessEnv([]string{"HOME=/h", "XDG_CONFIG_HOME=rel", "BROKEN", "A=b=c"})
	if e.get("HOME") != "/h" || e.home() != "/h" || e.get("A") != "b=c" || e.has("BROKEN") || e.SystemConfig != "/etc/gitconfig" {
		t.Fatalf("%+v", e)
	}
	if e.xdgFile("ignore") != "/h/.config/git/ignore" {
		t.Fatalf("relative XDG ignored: %q", e.xdgFile("ignore"))
	}
	if (Env{}).home() != "" || (Env{}).xdgFile("x") != "" || (Env{}).has("HOME") {
		t.Fatal("empty env")
	}
	x := envOf(map[string]string{"HOME": "rel", "XDG_CONFIG_HOME": "/x"})
	if x.home() != "" || x.xdgFile("config") != "/x/git/config" {
		t.Fatal("absolute XDG")
	}
}
