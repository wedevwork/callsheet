package workspacetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Effective git configuration of a git source or destination: the status
// and ignore settings only (never credentials). Precedence follows Git:
// the system file (unless GIT_CONFIG_NOSYSTEM; GIT_CONFIG_SYSTEM overrides
// its path), the XDG user file then ~/.gitconfig (GIT_CONFIG_GLOBAL
// replaces both), the repository's .git/config, then GIT_CONFIG_COUNT
// key/value pairs; within and across files the last value wins.
// include.path is resolved recursively (relative to the including file,
// "~/" from HOME) with cycle detection; includeIf supports gitdir:,
// gitdir/i: and onbranch:. Any other condition, and GIT_CONFIG_PARAMETERS
// (a non-file source), is refused as unsupported: nothing is executed.

// Env is the injectable environment and system-file provider of one
// transfer: tests never read the developer's home or /etc.
type Env struct {
	// Lookup reads an environment variable.
	Lookup func(key string) (string, bool)
	// SystemConfig and SystemAttributes are the system-wide git files
	// (default /etc/gitconfig and /etc/gitattributes).
	SystemConfig, SystemAttributes string
}

// ProcessEnv is the environment of this process with the default system
// file locations.
func ProcessEnv(environ []string) Env {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return Env{Lookup: func(k string) (string, bool) { v, ok := m[k]; return v, ok },
		SystemConfig: "/etc/gitconfig", SystemAttributes: "/etc/gitattributes"}
}

func (e Env) get(k string) string {
	if e.Lookup == nil {
		return ""
	}
	v, _ := e.Lookup(k)
	return v
}

func (e Env) has(k string) bool {
	if e.Lookup == nil {
		return false
	}
	_, ok := e.Lookup(k)
	return ok
}

// home is $HOME, or "" when unset or relative.
func (e Env) home() string {
	h := e.get("HOME")
	if !filepath.IsAbs(h) {
		return ""
	}
	return h
}

// xdgFile is $XDG_CONFIG_HOME/git/<name> for an absolute nonempty
// XDG_CONFIG_HOME, else $HOME/.config/git/<name> ("" without either).
func (e Env) xdgFile(name string) string {
	if x := e.get("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "git", name)
	}
	if h := e.home(); h != "" {
		return filepath.Join(h, ".config", "git", name)
	}
	return ""
}

// configEntry is one key assignment in precedence order.
type configEntry struct {
	key   string // section[.subsection].name, section and name lowercased
	value string
	blank bool // "key" without "=" (boolean true)
	// repo marks an entry read directly from the repository's own
	// .git/config (not through an include): the only place Git reads the
	// repository format (core.repositoryFormatVersion, extensions.*,
	// core.bare and core.worktree) from.
	repo bool
}

// gitConfig is the ordered effective configuration.
type gitConfig struct {
	entries []configEntry
	// files are the configuration files consulted, read or absent, with
	// the digest of what was read (zero when absent): the source-change
	// baselines.
	files []string
	sums  map[string][32]byte
}

// record notes a consulted configuration file and the digest of what
// was read (the first reading wins).
func (c *gitConfig) record(p string, sum [32]byte) {
	if c.sums == nil {
		c.sums = map[string][32]byte{}
	}
	if _, ok := c.sums[p]; ok {
		return
	}
	c.sums[p] = sum
	c.files = append(c.files, p)
}

func configKey(section, sub, name string) string {
	if sub == "" {
		return strings.ToLower(section) + "." + strings.ToLower(name)
	}
	return strings.ToLower(section) + "." + sub + "." + strings.ToLower(name)
}

// normalizeKey lowercases a dotted key's section and name.
func normalizeKey(k string) string {
	first, last := strings.IndexByte(k, '.'), strings.LastIndexByte(k, '.')
	if first < 0 {
		return strings.ToLower(k)
	}
	if first == last {
		return strings.ToLower(k)
	}
	return strings.ToLower(k[:first]) + k[first:last] + strings.ToLower(k[last:])
}

// get returns the last value of key.
func (c *gitConfig) get(key string) (configEntry, bool) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if c.entries[i].key == key {
			return c.entries[i], true
		}
	}
	return configEntry{}, false
}

// str returns key's last value ("" if unset).
func (c *gitConfig) str(key string) (string, bool) {
	e, ok := c.get(key)
	return e.value, ok
}

// boolean returns key's effective Git boolean: blank is true; true, yes,
// on and nonzero integers are true; false, no, off, 0 and "" are false.
func (c *gitConfig) boolean(key string, def bool) (bool, error) {
	e, ok := c.get(key)
	if !ok {
		return def, nil
	}
	if e.blank {
		return true, nil
	}
	return parseGitBool(e.value)
}

// parseGitBool is Git's git_parse_maybe_bool: true, yes or on and false,
// no, off or "" in any ASCII case, else the whole value as an int
// (git_parse_int); invalid trailing bytes fail, nothing is trimmed.
func parseGitBool(v string) (bool, error) {
	switch asciiFold(v) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off", "":
		return false, nil
	}
	if n, ok := parseGitInt(v, math.MaxInt32); ok {
		return n != 0, nil
	}
	return false, errBadConfig
}

// parseGitInt is Git's git_parse_signed with the bound max: the complete
// value is strtoimax in base 0 (C isspace skipped first, an optional
// sign, 0x/0X hexadecimal or 0 octal), then an optional k, m or g unit
// (any case) and nothing else; a magnitude beyond max (after the unit)
// fails. Nothing is trimmed: invalid trailing bytes fail.
func parseGitInt(v string, max int64) (int64, bool) {
	i := 0
	for i < len(v) && strings.IndexByte(" \t\n\v\f\r", v[i]) >= 0 {
		i++
	}
	neg := false
	if i < len(v) && (v[i] == '+' || v[i] == '-') {
		neg = v[i] == '-'
		i++
	}
	base := uint64(10)
	switch {
	case i+2 < len(v) && v[i] == '0' && (v[i+1] == 'x' || v[i+1] == 'X') && hexDigit(v[i+2]) >= 0:
		base = 16
		i += 2
	case i < len(v) && v[i] == '0':
		base = 8
	}
	start := i
	var u uint64
	overflow := false
	for ; i < len(v); i++ {
		d := hexDigit(v[i])
		if d < 0 || uint64(d) >= base {
			break
		}
		if u > (math.MaxUint64-uint64(d))/base {
			overflow = true
		} else {
			u = u*base + uint64(d)
		}
	}
	if i == start || overflow || (!neg && u > math.MaxInt64) || (neg && u > math.MaxInt64+1) {
		return 0, false
	}
	factor := uint64(1)
	switch unit := v[i:]; {
	case unit == "":
	case unit == "k" || unit == "K":
		factor = 1 << 10
	case unit == "m" || unit == "M":
		factor = 1 << 20
	case unit == "g" || unit == "G":
		factor = 1 << 30
	default:
		return 0, false
	}
	if u > uint64(max)/factor {
		return 0, false
	}
	n := int64(u * factor)
	if neg {
		n = -n
	}
	return n, true
}

// hexDigit is a hexadecimal digit's value, or -1.
func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// has reports whether any key with the prefix is set.
func (c *gitConfig) hasPrefix(prefix string) bool {
	for _, e := range c.entries {
		if strings.HasPrefix(e.key, prefix) {
			return true
		}
	}
	return false
}

var errBadConfig = errors.New("invalid git configuration")

// unsupportedConfig refuses a configuration this layer cannot evaluate.
func unsupportedConfig(what string) error {
	return contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository,
		"unsupported git configuration: "+what+"; nothing was read further or executed")
}

// configLoader reads configuration files for one repository.
type configLoader struct {
	ctx    context.Context
	env    Env
	gitDir string // canonical .git directory
	branch string // current branch short name ("" when detached/unborn)
	cfg    *gitConfig
	stack  []string
	repo   bool // the file being parsed is .git/config itself
}

const maxIncludeDepth = 10

// loadConfig resolves the effective configuration of the repository whose
// canonical .git directory is gitDir (on branch, "" if none).
func loadConfig(ctx context.Context, env Env, gitDir, branch string) (*gitConfig, error) {
	l := &configLoader{ctx: ctx, env: env, gitDir: gitDir, branch: branch, cfg: &gitConfig{}}
	if v := env.get("GIT_CONFIG_PARAMETERS"); v != "" {
		return nil, unsupportedConfig("GIT_CONFIG_PARAMETERS (command-line configuration)")
	}
	noSystem, err := envBool(env, "GIT_CONFIG_NOSYSTEM")
	if err != nil {
		return nil, err
	}
	if !noSystem {
		p := env.SystemConfig
		if env.has("GIT_CONFIG_SYSTEM") {
			p = env.get("GIT_CONFIG_SYSTEM")
		}
		if err := l.file(p, false); err != nil {
			return nil, err
		}
	}
	if env.has("GIT_CONFIG_GLOBAL") {
		if err := l.file(env.get("GIT_CONFIG_GLOBAL"), false); err != nil {
			return nil, err
		}
	} else {
		if err := l.file(env.xdgFile("config"), false); err != nil {
			return nil, err
		}
		if h := env.home(); h != "" {
			if err := l.file(filepath.Join(h, ".gitconfig"), false); err != nil {
				return nil, err
			}
		}
	}
	if err := l.file(filepath.Join(gitDir, "config"), true); err != nil {
		return nil, err
	}
	if err := l.envPairs(); err != nil {
		return nil, err
	}
	return l.cfg, nil
}

// envBool reads a Git boolean environment variable (unset or empty is
// false).
func envBool(env Env, k string) (bool, error) {
	v := env.get(k)
	if v == "" {
		return false, nil
	}
	b, err := parseGitBool(v)
	if err != nil {
		return false, unsupportedConfig(k + " is not a boolean")
	}
	return b, nil
}

// envPairs applies GIT_CONFIG_COUNT's GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n.
func (l *configLoader) envPairs() error {
	cs := l.env.get("GIT_CONFIG_COUNT")
	if cs == "" {
		return nil
	}
	// Git reads the count with strtoul and refuses trailing bytes; this
	// stricter reading (no leading whitespace, at most 65536) only ever
	// refuses more.
	n, err := strconv.Atoi(cs)
	if err != nil || n < 0 || n > 1<<16 {
		return unsupportedConfig("GIT_CONFIG_COUNT is not a valid count")
	}
	for i := 0; i < n; i++ {
		k, kok := l.env.Lookup("GIT_CONFIG_KEY_" + strconv.Itoa(i))
		v, vok := l.env.Lookup("GIT_CONFIG_VALUE_" + strconv.Itoa(i))
		if !kok || !vok || k == "" || !strings.Contains(k, ".") {
			return unsupportedConfig("GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n pairs are incomplete")
		}
		key := normalizeKey(k)
		if strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.") {
			return unsupportedConfig("includes from environment configuration")
		}
		l.cfg.entries = append(l.cfg.entries, configEntry{key: key, value: v})
	}
	return nil
}

// expandPath expands "~/" from HOME; a relative path is resolved from
// base (the including file's directory).
func (l *configLoader) expandPath(p, base string) (string, error) {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		h := l.env.home()
		if h == "" {
			return "", unsupportedConfig("a ~/ path without HOME")
		}
		return filepath.Join(h, strings.TrimPrefix(p, "~")), nil
	case strings.HasPrefix(p, "~"):
		return "", unsupportedConfig("~user paths")
	case filepath.IsAbs(p):
		return filepath.Clean(p), nil
	}
	return filepath.Join(base, p), nil
}

// file reads one configuration file (absent: nothing). Unreadable files
// and parse errors fail.
func (l *configLoader) file(p string, repo bool) error {
	if p == "" {
		return nil
	}
	if err := l.ctx.Err(); err != nil {
		return err
	}
	for _, s := range l.stack {
		if s == p {
			return unsupportedConfig("an include cycle")
		}
	}
	if len(l.stack) >= maxIncludeDepth {
		return unsupportedConfig("includes nested too deeply")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			l.cfg.record(p, [32]byte{})
			return nil
		}
		return contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository,
			"a git configuration file cannot be read")
	}
	l.cfg.record(p, sha256.Sum256(data))
	l.stack = append(l.stack, p)
	outer := l.repo
	l.repo = repo
	defer func() { l.stack, l.repo = l.stack[:len(l.stack)-1], outer }()
	return l.parse(data, filepath.Dir(p))
}

// parse applies one file's assignments in order, resolving includes in
// place (values after an include override the included ones).
func (l *configLoader) parse(data []byte, dir string) error {
	return parseGitConfig(data, func(sec, sub, name, value string, blank bool) error {
		switch {
		case strings.EqualFold(sec, "include") && sub == "" && strings.EqualFold(name, "path"):
			return l.include(value, blank, dir)
		case strings.EqualFold(sec, "includeIf") && strings.EqualFold(name, "path"):
			ok, err := l.condition(sub, dir)
			if err != nil || !ok {
				return err
			}
			return l.include(value, blank, dir)
		}
		l.cfg.entries = append(l.cfg.entries, configEntry{key: configKey(sec, sub, name), value: value, blank: blank, repo: l.repo})
		return nil
	})
}

// errConfigSyntax is a configuration file Git would refuse to parse.
func errConfigSyntax() error {
	return contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository,
		"a git configuration file cannot be parsed")
}

// parseGitConfig is Git's configuration file syntax (config.c): "[section]",
// "[section \"subsection\"]" (subsection escapes \\ and \"), the legacy
// "[section.subsection]", "name = value" and a bare "name" (boolean true);
// ';' and '#' comments; values with quotes, the escapes \n \t \b \\ \"
// and backslash line continuation, inner whitespace kept and trailing
// unquoted whitespace dropped; CRLF line ends. fn receives every
// assignment in file order.
func parseGitConfig(data []byte, fn func(sec, sub, name, value string, blank bool) error) error {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	p := &cfgParser{b: data}
	sec, sub := "", ""
	for {
		c := p.next()
		switch {
		case c == eofChar:
			return nil
		case c == '\n' || c == ' ' || c == '\t' || c == '\r':
			// Git's isspace (space, tab, LF and CR; not \v or \f).
			continue
		case c == '#' || c == ';':
			p.skipLine()
			continue
		case c == '[':
			var ok bool
			if sec, sub, ok = p.header(); !ok {
				return errConfigSyntax()
			}
			continue
		case isAlpha(byte(c)):
			if sec == "" {
				return errConfigSyntax()
			}
			name, value, blank, ok := p.assignment(byte(c))
			if !ok {
				return errConfigSyntax()
			}
			if err := fn(sec, sub, name, value, blank); err != nil {
				return err
			}
		default:
			return errConfigSyntax()
		}
	}
}

const eofChar = -1

type cfgParser struct {
	b   []byte
	i   int
	eof bool
}

// next returns the next character with CRLF folded to LF; the end of
// input is an LF once, then eofChar.
func (p *cfgParser) next() int {
	if p.i >= len(p.b) {
		if !p.eof {
			p.eof = true
			return '\n'
		}
		return eofChar
	}
	c := p.b[p.i]
	p.i++
	if c == '\r' && p.i < len(p.b) && p.b[p.i] == '\n' {
		p.i++
		return '\n'
	}
	return int(c)
}

func (p *cfgParser) skipLine() {
	for {
		if c := p.next(); c == '\n' || c == eofChar {
			return
		}
	}
}

func isKeyChar(c byte) bool { return isAlpha(c) || isDigit(c) || c == '-' }

// header parses a section header after '['.
func (p *cfgParser) header() (string, string, bool) {
	var name []byte
	for {
		c := p.next()
		switch {
		case c == ']':
			if len(name) == 0 {
				return "", "", false
			}
			n := string(name)
			if i := strings.IndexByte(n, '.'); i >= 0 {
				// The deprecated [section.subsection] form lowercases the
				// subsection.
				return n[:i], strings.ToLower(n[i+1:]), n[:i] != ""
			}
			return n, "", true
		case c == ' ' || c == '\t':
			if len(name) == 0 || strings.IndexByte(string(name), '.') >= 0 {
				return "", "", false
			}
			sub, ok := p.subsection()
			return string(name), sub, ok
		case c >= 0 && (isKeyChar(byte(c)) || c == '.'):
			name = append(name, byte(c))
		default:
			return "", "", false
		}
	}
}

// subsection parses ' "subsection"]'.
func (p *cfgParser) subsection() (string, bool) {
	c := p.next()
	for c == ' ' || c == '\t' {
		c = p.next()
	}
	if c != '"' {
		return "", false
	}
	var sub []byte
	for {
		c = p.next()
		switch c {
		case '\n', eofChar:
			return "", false
		case '"':
			if p.next() != ']' {
				return "", false
			}
			return string(sub), true
		case '\\':
			c = p.next()
			if c == '\n' || c == eofChar {
				return "", false
			}
		}
		sub = append(sub, byte(c))
	}
}

// assignment parses "name[ = value]" whose first character is first.
func (p *cfgParser) assignment(first byte) (string, string, bool, bool) {
	name := []byte{first}
	c := p.next()
	for c >= 0 && isKeyChar(byte(c)) {
		name = append(name, byte(c))
		c = p.next()
	}
	for c == ' ' || c == '\t' {
		c = p.next()
	}
	switch c {
	case '\n', eofChar:
		return string(name), "", true, true
	case '#', ';':
		p.skipLine()
		return string(name), "", true, true
	case '=':
	default:
		return "", "", false, false
	}
	v, ok := p.value()
	return string(name), v, false, ok
}

// value is Git's parse_value.
func (p *cfgParser) value() (string, bool) {
	var out []byte
	quote, comment := false, false
	space := 0
	for {
		c := p.next()
		if c == '\n' || c == eofChar {
			if quote {
				return "", false
			}
			return string(out), true
		}
		if comment {
			continue
		}
		if (c == ' ' || c == '\t' || c == '\r') && !quote {
			// Git's isspace: a lone CR is whitespace too (\v and \f
			// are value bytes).
			if len(out) > 0 {
				space++
			}
			continue
		}
		if !quote && (c == ';' || c == '#') {
			comment = true
			continue
		}
		for ; space > 0; space-- {
			out = append(out, ' ')
		}
		switch c {
		case '\\':
			switch e := p.next(); e {
			case '\n':
				continue
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'n':
				out = append(out, '\n')
			case '\\', '"':
				out = append(out, byte(e))
			default:
				return "", false
			}
		case '"':
			quote = !quote
		default:
			out = append(out, byte(c))
		}
	}
}

func (l *configLoader) include(value string, blank bool, dir string) error {
	if blank || value == "" {
		return unsupportedConfig("an include without a path")
	}
	p, err := l.expandPath(value, dir)
	if err != nil {
		return err
	}
	return l.file(p, false)
}

// condition evaluates an includeIf condition.
func (l *configLoader) condition(cond, dir string) (bool, error) {
	kind, pat, ok := strings.Cut(cond, ":")
	if !ok {
		return false, unsupportedConfig(fmt.Sprintf("includeIf condition %q", contract.SafeText(cond, 64)))
	}
	switch kind {
	case "gitdir", "gitdir/i":
		return l.gitdirMatch(pat, dir, kind == "gitdir/i")
	case "onbranch":
		if l.branch == "" || pat == "" {
			return false, nil
		}
		if strings.HasSuffix(pat, "/") {
			pat += "**"
		}
		return wildmatch(pat, l.branch, wmPathname), nil
	}
	return false, unsupportedConfig("includeIf condition " + contract.SafeText(kind, 32))
}

// gitdirMatch is Git's include_by_gitdir: "./" is relative to the
// including file, "~/" to HOME, a pattern that is not absolute gets a
// "**/" prefix and a trailing "/" gets "**".
func (l *configLoader) gitdirMatch(pat, dir string, fold bool) (bool, error) {
	switch {
	case strings.HasPrefix(pat, "./"):
		trail := strings.HasSuffix(pat, "/")
		pat = filepath.Join(dir, pat[2:])
		if trail {
			pat += "/"
		}
	case pat == "~" || strings.HasPrefix(pat, "~/"):
		h := l.env.home()
		if h == "" {
			return false, nil
		}
		trail := strings.HasSuffix(pat, "/")
		pat = filepath.Join(h, strings.TrimPrefix(pat, "~"))
		if trail {
			pat += "/"
		}
	case !filepath.IsAbs(pat):
		pat = "**/" + pat
	}
	if strings.HasSuffix(pat, "/") {
		pat += "**"
	}
	flags := wmPathname
	if fold {
		flags |= wmCaseFold
	}
	text := filepath.ToSlash(l.gitDir)
	return wildmatch(pat, text, flags), nil
}
