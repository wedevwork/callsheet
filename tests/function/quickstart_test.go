package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

// Iteration 12-2 function tests (design 12-2-release-quickstart): one
// top-level TestQuickstart* per FP over the shipped docs/quickstart.md. A
// small test-only parser reads its marked shell fences; the lifecycle runs the
// documented argv with fixed fixture substitutions against the fake adapter,
// and the install recipe runs its own fence bodies under isolated bash. Nothing
// here contacts a network, launches a vendor CLI or modifies the checkout, and
// no loopback fixture is a two-machine or vendor qualification.

const (
	quickstartDoc = "docs/quickstart.md"
	// qsWait bounds one lifecycle command or readiness poll; it is the
	// function harness's deadline, never the human time target.
	qsWait        = 90 * time.Second
	qsDefaultBind = "127.0.0.1:8443"
	qsDefaultURL  = "https://" + qsDefaultBind
	qsReleases    = "https://github.com/wedevwork/callsheet/releases"
	qsGoal        = "Create hello.txt containing exactly 'hello from Callsheet' followed by a newline. Preserve README.md."
	qsAcceptance  = "hello.txt has the requested single line and README.md is unchanged"
	qsInstruction = "Work only in the task checkout. Follow the goal, preserve existing files, and report what you changed.\n"
	qsRunbook     = "Read README.md. Create the requested file, then check its exact contents. Do not install dependencies or access external services.\n"
	qsSeedReadme  = "Callsheet quickstart scratch project.\n"
	qsHello       = "hello from Callsheet\n"
	qsFakeModel   = "example-model"
	qsCoordinator = "Use the Callsheet MCP server to discover nodes and roles. Do not dispatch until I ask. Record every admitted task ID; never redispatch after a lost answer. " +
		"For this quickstart I will run the first dispatch and wait in my operator terminal so I can see the exact commands. Inspect its task ID when I give it to you. " +
		"Later tasks may use MCP dispatch and short task_wait polls; follow the coordinator guide for harness-managed background waits. " +
		"Check state, final message and workspace publication before declaring completion."
)

// qsHeadings are the stable link targets, in order.
var qsHeadings = []string{"Install Callsheet", "Prerequisites", "One machine", "Add a second machine", "If something goes wrong", "Next steps"}

// docFenceSpec pins one marked fence of a document and the Callsheet leaves it
// may run, in order; a fence with no leaf holds no Callsheet command. The
// quickstart and the deployment chapter share it.
type docFenceSpec struct {
	id     string
	lang   string
	leaves []string
}

// qsInventory pins every marked fence of the quickstart; fences with no leaf
// are setup or install fences.
var qsInventory = []docFenceSpec{
	{"install-shell", "sh", nil},
	{"download", "bash", nil},
	{"checksum-linux", "bash", nil},
	{"checksum-macos", "bash", nil},
	{"install", "bash", []string{"version"}},
	{"source-build", "bash", []string{"version"}},
	{"env-plane", "sh", nil},
	{"plane-init", "sh", []string{"plane init"}},
	{"plane-run", "sh", []string{"plane run"}},
	{"env-sidecar", "sh", nil},
	{"sidecar-enroll", "sh", []string{"sidecar enroll"}},
	{"worker-path", "sh", nil},
	{"sidecar-run", "sh", []string{"sidecar run"}},
	{"env-operator", "sh", nil},
	{"manuals-and-seed", "sh", nil},
	{"node-and-model", "sh", nil},
	{"node-list", "sh", []string{"node ls"}},
	{"role-add", "sh", []string{"role add"}},
	{"role-list", "sh", []string{"role ls"}},
	{"coordinator-runbook", "sh", nil},
	{"workspace-create", "sh", []string{"ws create"}},
	{"read-instance", "sh", nil},
	{"workspace-push", "sh", []string{"ws push"}},
	{"dispatch", "sh", []string{"dispatch"}},
	{"read-task", "sh", nil},
	{"wait", "sh", []string{"task wait"}},
	{"task-show", "sh", []string{"task show"}},
	{"workspace-status", "sh", []string{"ws status"}},
	{"pull", "sh", []string{"ws pull"}},
	{"inspect-result", "sh", nil},
	{"remote-address", "sh", nil},
	{"remote-cert", "sh", []string{"plane cert reissue"}},
	{"remote-plane-run", "sh", []string{"plane run"}},
	{"remote-url", "sh", nil},
	{"local-reenroll", "sh", []string{"sidecar enroll"}},
	{"local-restart", "sh", []string{"sidecar run"}},
	{"remote-env", "sh", nil},
	{"remote-manuals", "sh", nil},
	{"remote-enroll", "sh", []string{"sidecar enroll"}},
	{"remote-worker-path", "sh", nil},
	{"remote-sidecar-run", "sh", []string{"sidecar run"}},
	{"remote-node-and-model", "sh", nil},
	{"remote-role-add", "sh", []string{"role add"}},
	{"remote-roster", "sh", []string{"node ls", "role ls"}},
	{"remote-dispatch", "sh", []string{"dispatch"}},
	{"remote-read-task", "sh", nil},
	{"remote-wait", "sh", []string{"task wait"}},
	{"remote-show", "sh", []string{"task show"}},
	{"remote-status", "sh", []string{"ws status"}},
	{"remote-pull", "sh", []string{"ws pull"}},
	{"diagnostics", "sh", []string{"task logs"}},
}

// qsVariables is the fixed substitution allowlist of Callsheet commands.
var qsVariables = []string{"QS", "PLANE", "CA", "NODE", "WORKER", "MODEL", "INSTANCE", "TASK", "PLANE_IP", "REMOTE_NODE", "REMOTE_TASK"}

// qsFence is one fence of the quickstart.
type qsFence struct {
	id, lang, body string
	line           int
}

var (
	qsMarker = regexp.MustCompile(`^<!-- quickstart: ([a-z0-9-]+) -->$`)
	qsName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
)

// parseQuickstartFences returns every fence in order. Each shell fence must
// follow a unique marker; a non-shell fence may not hold a Callsheet command,
// so removing a label cannot hide one.
func parseQuickstartFences(md string) ([]qsFence, error) {
	return parseMarkedFences(md, qsMarker)
}

// parseMarkedFences is the parser core: marker selects the document's fence
// marker. Its diagnostics are the quickstart's, for every document.
func parseMarkedFences(md string, marker *regexp.Regexp) ([]qsFence, error) {
	var out []qsFence
	lines := strings.Split(md, "\n")
	seen := map[string]bool{}
	pending, pendingLine := "", 0
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if m := marker.FindStringSubmatch(trimmed); m != nil {
			if pending != "" {
				return nil, fmt.Errorf("fences: marker %s at line %d is not immediately before a fence", pending, pendingLine)
			}
			if seen[m[1]] {
				return nil, fmt.Errorf("fences: duplicate fence ID %s at line %d", m[1], i+1)
			}
			seen[m[1]] = true
			pending, pendingLine = m[1], i+1
			continue
		}
		if !strings.HasPrefix(trimmed, "```") {
			if pending != "" {
				return nil, fmt.Errorf("fences: marker %s at line %d is not immediately before a fence", pending, pendingLine)
			}
			continue
		}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
		f := qsFence{id: pending, lang: strings.TrimPrefix(trimmed, "```"), line: i + 1}
		pending = ""
		var body []string
		closed := false
		for i++; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "```" {
				closed = true
				break
			}
			body = append(body, strings.TrimPrefix(lines[i], indent))
		}
		if !closed {
			return nil, fmt.Errorf("fences: fence at line %d is not closed", f.line)
		}
		f.body = strings.Join(body, "\n")
		shell := f.lang == "sh" || f.lang == "bash"
		switch {
		case shell && f.id == "":
			return nil, fmt.Errorf("fences: unmarked %s fence at line %d", f.lang, f.line)
		case !shell && f.id != "":
			return nil, fmt.Errorf("fences: fence %s at line %d is not a shell fence", f.id, f.line)
		case !shell && hasCallsheetLine(f.body):
			return nil, fmt.Errorf("fences: unlabelled fence at line %d holds a Callsheet command", f.line)
		}
		out = append(out, f)
	}
	if pending != "" {
		return nil, fmt.Errorf("fences: marker %s at line %d is not immediately before a fence", pending, pendingLine)
	}
	return out, nil
}

func hasCallsheetLine(body string) bool {
	for _, l := range strings.Split(body, "\n") {
		if isCallsheetLine(l) {
			return true
		}
	}
	return false
}

func isCallsheetLine(line string) bool {
	l := strings.TrimSpace(line)
	return l == "callsheet" || strings.HasPrefix(l, "callsheet ") || strings.HasPrefix(l, "callsheet\t")
}

func fenceByID(fences []qsFence, id string) (qsFence, bool) {
	for _, f := range fences {
		if f.id == id {
			return f, true
		}
	}
	return qsFence{}, false
}

// qsPart is one fragment of a word: literal text or a variable.
type qsPart struct {
	text, variable string
}

// qsWord is one argv value: adjacent fragments, and whether any part was
// quoted (a quoted flag-looking value is a value, not a flag).
type qsWord struct {
	parts  []qsPart
	quoted bool
}

func (w qsWord) literal() (string, bool) {
	var b strings.Builder
	for _, p := range w.parts {
		if p.variable != "" {
			return "", false
		}
		b.WriteString(p.text)
	}
	return b.String(), true
}

func (w qsWord) expand(vars map[string]string) (string, error) {
	var b strings.Builder
	for _, p := range w.parts {
		if p.variable == "" {
			b.WriteString(p.text)
			continue
		}
		v, ok := vars[p.variable]
		if !ok {
			return "", fmt.Errorf("unknown variable $%s", p.variable)
		}
		b.WriteString(v)
	}
	return b.String(), nil
}

// qsCommand is one logical line; redirect is a printf's redirection target.
type qsCommand struct {
	words    []qsWord
	redirect *qsWord
	line     int
}

// tokenizeShell parses the deliberately small subset: whitespace, single and
// double quotes, backslash-newline, literals and $NAME/${NAME}. With
// allowRedirect a single "> word" may end a command. Everything else fails
// with the fence ID and line; nothing is ever evaluated by a shell.
func tokenizeShell(id, body string, firstLine int, allowRedirect bool) ([]qsCommand, error) {
	var cmds []qsCommand
	var cur qsCommand
	var word *qsWord
	line := firstLine
	cur.line = line
	fail := func(what string) ([]qsCommand, error) {
		return nil, fmt.Errorf("fence %s line %d: unsupported shell syntax %s", id, line, what)
	}
	lit := func(s string) {
		if word == nil {
			word = &qsWord{}
		}
		if n := len(word.parts); n > 0 && word.parts[n-1].variable == "" {
			word.parts[n-1].text += s
			return
		}
		word.parts = append(word.parts, qsPart{text: s})
	}
	inRedirect := false
	endWord := func() {
		if word == nil {
			return
		}
		if inRedirect {
			cur.redirect = word
			inRedirect = false
		} else {
			cur.words = append(cur.words, *word)
		}
		word = nil
	}
	variable := func(i int) (int, error) {
		rest := body[i+1:]
		if strings.HasPrefix(rest, "{") {
			end := strings.Index(rest, "}")
			if end < 0 || !qsName.MatchString(rest[1:end]) || qsName.FindString(rest[1:end]) != rest[1:end] {
				return 0, fmt.Errorf("fence %s line %d: unsupported shell syntax ${", id, line)
			}
			if word == nil {
				word = &qsWord{}
			}
			word.parts = append(word.parts, qsPart{variable: rest[1:end]})
			return i + 1 + end, nil
		}
		name := qsName.FindString(rest)
		if name == "" {
			return 0, fmt.Errorf("fence %s line %d: unsupported shell syntax $", id, line)
		}
		if word == nil {
			word = &qsWord{}
		}
		word.parts = append(word.parts, qsPart{variable: name})
		return i + len(name), nil
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case ' ', '\t':
			endWord()
		case '\n':
			endWord()
			if inRedirect {
				return fail("> without a target")
			}
			if cur.redirect != nil && len(cur.words) == 0 {
				return fail("redirect without a command")
			}
			if len(cur.words) > 0 {
				cmds = append(cmds, cur)
			}
			line++
			cur = qsCommand{line: line}
		case '\\':
			if i+1 < len(body) && body[i+1] == '\n' {
				endWord()
				i++
				line++
				continue
			}
			return fail(`\`)
		case '\'':
			end := strings.IndexByte(body[i+1:], '\'')
			if end < 0 {
				return fail("unterminated '")
			}
			lit(body[i+1 : i+1+end])
			word.quoted = true
			line += strings.Count(body[i+1:i+1+end], "\n")
			i += 1 + end
		case '"':
			if word == nil {
				word = &qsWord{}
			}
			word.quoted = true
			closed := false
			for i++; i < len(body); i++ {
				d := body[i]
				if d == '"' {
					closed = true
					break
				}
				switch d {
				case '`':
					return fail("command substitution")
				case '\\':
					if i+1 >= len(body) || !strings.ContainsRune(`"$\`, rune(body[i+1])) {
						return fail(`\ in double quotes`)
					}
					i++
					lit(string(body[i]))
				case '$':
					if i+1 < len(body) && body[i+1] == '(' {
						return fail("command substitution")
					}
					next, err := variable(i)
					if err != nil {
						return nil, err
					}
					i = next
				default:
					if d == '\n' {
						line++
					}
					lit(string(d))
				}
			}
			if !closed {
				return fail(`unterminated "`)
			}
		case '$':
			if i+1 < len(body) && body[i+1] == '(' {
				return fail("command substitution")
			}
			next, err := variable(i)
			if err != nil {
				return nil, err
			}
			i = next
		case '>':
			if !allowRedirect || inRedirect || cur.redirect != nil {
				return fail("redirect")
			}
			endWord()
			inRedirect = true
		case '|':
			return fail("pipe")
		case ';':
			return fail("semicolon chain")
		case '`':
			return fail("command substitution")
		case '#':
			return fail("comment")
		case '&', '<', '(', ')', '*', '?', '[', ']', '{', '}', '~', '!':
			return fail(string(c))
		default:
			lit(string(c))
		}
	}
	endWord()
	if inRedirect {
		return fail("> without a target")
	}
	if cur.redirect != nil && len(cur.words) == 0 {
		return fail("redirect without a command")
	}
	if len(cur.words) > 0 {
		cmds = append(cmds, cur)
	}
	return cmds, nil
}

// qsParsed is a fence's tokenized commands: Callsheet commands in order and,
// for sh fences, its setup operations.
type qsParsed struct {
	callsheet []qsCommand
	setup     []qsCommand
}

// parseFenceCommands tokenizes a fence's strict scope: every line of an sh
// fence, and only the Callsheet lines of a bash fence (the rest is validated
// and executed by the install contract).
func parseFenceCommands(f qsFence) (qsParsed, error) {
	var p qsParsed
	if f.lang == "bash" {
		for i, l := range strings.Split(f.body, "\n") {
			if !isCallsheetLine(l) {
				continue
			}
			cmds, err := tokenizeShell(f.id, strings.TrimSpace(l), f.line+1+i, false)
			if err != nil {
				return p, err
			}
			p.callsheet = append(p.callsheet, cmds...)
		}
		return p, nil
	}
	cmds, err := tokenizeShell(f.id, f.body, f.line+1, true)
	if err != nil {
		return p, err
	}
	for _, c := range cmds {
		if first, ok := c.words[0].literal(); ok && first == "callsheet" {
			if c.redirect != nil {
				return p, fmt.Errorf("fence %s line %d: unsupported shell syntax redirect", f.id, c.line)
			}
			p.callsheet = append(p.callsheet, c)
			continue
		}
		if err := checkSetupOperation(f.id, c); err != nil {
			return p, err
		}
		p.setup = append(p.setup, c)
	}
	return p, nil
}

var qsAssignment = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*=`)

// checkSetupOperation accepts only the fixture-construction operations the
// lifecycle knows how to apply.
func checkSetupOperation(id string, c qsCommand) error {
	first, ok := c.words[0].literal()
	bad := func(what string) error {
		return fmt.Errorf("fence %s line %d: unrecognized setup operation: %s", id, c.line, what)
	}
	if !ok {
		// Only the documented worker version check starts with a variable.
		if len(c.words) == 2 && len(c.words[0].parts) == 1 && c.words[0].parts[0].variable == "WORKER" && c.redirect == nil {
			if v, _ := c.words[1].literal(); v == "--version" {
				return nil
			}
		}
		if len(c.words) == 1 && len(c.words[0].parts) > 0 && c.words[0].parts[0].variable == "" {
			if qsAssignment.MatchString(c.words[0].parts[0].text) {
				return nil
			}
		}
		return bad("a variable command")
	}
	if c.redirect != nil && first != "printf" {
		return bad("a redirect on " + first)
	}
	switch {
	case first == "printf":
		if c.redirect == nil || len(c.words) < 2 || len(c.words) > 3 {
			return bad("printf without a single redirected file")
		}
	case first == "mkdir":
		if len(c.words) != 2 {
			return bad("mkdir takes one directory")
		}
	case first == "read":
		if len(c.words) != 3 {
			return bad("read takes -r NAME")
		}
		if v, _ := c.words[1].literal(); v != "-r" {
			return bad("read takes -r NAME")
		}
	case first == "export":
		if len(c.words) != 2 {
			return bad("export must prepend the install PATH")
		}
		if v, _ := c.words[1].expand(map[string]string{"HOME": "H", "PATH": "P"}); v != "PATH=H/.local/bin:P" {
			return bad("export must prepend the install PATH")
		}
	case first == "cat" || first == "cmp":
		// Result inspection, checked by the lifecycle's delivery assertion.
	case first == "bash" && id == "install-shell":
		// The install validator pins this launch line exactly.
	case len(c.words) == 1 && qsAssignment.MatchString(first):
	default:
		return bad(first)
	}
	return nil
}

// leafOf resolves a Callsheet command against the real tree and returns the
// leaf path without "callsheet " and the remaining words.
func leafOf(root *cli.Command, c qsCommand) (string, []qsWord, error) {
	cur := root
	i := 1
	for ; i < len(c.words) && !cur.IsLeaf(); i++ {
		name, ok := c.words[i].literal()
		if !ok {
			return "", nil, fmt.Errorf("line %d: a command path holds a variable", c.line)
		}
		next := cur.Child(name)
		if next == nil {
			return "", nil, fmt.Errorf("line %d: unknown command %q under %q", c.line, name, cur.Path())
		}
		cur = next
	}
	if !cur.IsLeaf() {
		return "", nil, fmt.Errorf("line %d: %q is not a leaf command", c.line, cur.Path())
	}
	return strings.TrimPrefix(cur.Path(), root.Name+" "), c.words[i:], nil
}

// flagTokens are a command's flag vocabulary, from its real help output.
func flagTokens(help string) map[string]bool {
	out := map[string]bool{}
	for _, f := range regexp.MustCompile(`--[a-z][a-z-]*`).FindAllString(help, -1) {
		out[f] = true
	}
	return out
}

// leafHelp caches each leaf's real --help once per test process.
var (
	leafHelpMu    sync.Mutex
	leafHelpCache = map[string]result{}
)

func leafHelp(t *testing.T, leaf string) (result, error) {
	t.Helper()
	leafHelpMu.Lock()
	defer leafHelpMu.Unlock()
	if r, ok := leafHelpCache[leaf]; ok {
		return r, nil
	}
	home := t.TempDir()
	r := runBin(t, nodeBinary(t), home, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}, append(strings.Fields(leaf), "--help")...)
	if r.code != 0 {
		return r, fmt.Errorf("%s --help exited %d: %s", leaf, r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "Status: implemented.") {
		return r, fmt.Errorf("%s is not implemented", leaf)
	}
	leafHelpCache[leaf] = r
	return r, nil
}

// checkCommandContract validates the whole fence inventory: required IDs,
// exact leaf sequences through the real tree, flags through real help, and
// known variables only.
func checkCommandContract(t *testing.T, md string) error {
	t.Helper()
	return checkFenceContract(t, md, docFenceContract{marker: qsMarker, inventory: qsInventory, variables: func(string) []string { return qsVariables }, parse: parseFenceCommands})
}

// docFenceContract selects one document's marker, pinned inventory, allowed
// Callsheet-command variables per fence and fence parser.
type docFenceContract struct {
	marker    *regexp.Regexp
	inventory []docFenceSpec
	variables func(id string) []string
	parse     func(qsFence) (qsParsed, error)
}

// checkCallsheetCommand validates one parsed Callsheet command against the
// real tree: it returns its leaf and any flag or variable findings.
func checkCallsheetCommand(t *testing.T, root *cli.Command, id string, c qsCommand, allowed []string) (string, []error) {
	t.Helper()
	leaf, rest, err := leafOf(root, c)
	if err != nil {
		return "", []error{fmt.Errorf("fence %s %w", id, err)}
	}
	help, err := leafHelp(t, leaf)
	if err != nil {
		return leaf, []error{fmt.Errorf("fence %s: %w", id, err)}
	}
	var errs []error
	vocabulary := flagTokens(help.stdout)
	for _, w := range rest {
		if v, ok := w.literal(); ok && !w.quoted && strings.HasPrefix(v, "--") {
			name, _, _ := strings.Cut(v, "=")
			if !vocabulary[name] {
				errs = append(errs, fmt.Errorf("fence %s line %d: unknown flag %s of %s", id, c.line, name, leaf))
			}
		}
		for _, p := range w.parts {
			if p.variable != "" && !slices.Contains(allowed, p.variable) {
				errs = append(errs, fmt.Errorf("fence %s line %d: unknown variable $%s", id, c.line, p.variable))
			}
		}
	}
	return leaf, errs
}

// checkFenceContract is the contract core over one document's inventory.
func checkFenceContract(t *testing.T, md string, c docFenceContract) error {
	t.Helper()
	fences, err := parseMarkedFences(md, c.marker)
	if err != nil {
		return err
	}
	root := cli.NewTree(runtime.GOOS)
	known := map[string]bool{}
	for _, inv := range c.inventory {
		known[inv.id] = true
		if _, ok := fenceByID(fences, inv.id); !ok {
			return fmt.Errorf("inventory: required fence %s is missing", inv.id)
		}
	}
	var errs []error
	for _, f := range fences {
		if f.lang != "sh" && f.lang != "bash" {
			continue
		}
		if !known[f.id] {
			errs = append(errs, fmt.Errorf("inventory: fence %s is not in the pinned inventory", f.id))
			continue
		}
		inv := c.inventory[slices.IndexFunc(c.inventory, func(e docFenceSpec) bool { return e.id == f.id })]
		if inv.lang != f.lang {
			errs = append(errs, fmt.Errorf("inventory: fence %s is %s, want %s", f.id, f.lang, inv.lang))
		}
		parsed, err := c.parse(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var leaves []string
		for _, cmd := range parsed.callsheet {
			leaf, findings := checkCallsheetCommand(t, root, f.id, cmd, c.variables(f.id))
			errs = append(errs, findings...)
			if leaf != "" {
				leaves = append(leaves, leaf)
			}
		}
		if !slices.Equal(leaves, inv.leaves) {
			errs = append(errs, fmt.Errorf("inventory: fence %s runs %q, want %q", f.id, leaves, inv.leaves))
		}
	}
	return errors.Join(errs...)
}

// qsDocSection returns the text of H2 section name in the quickstart.
func qsDocSection(md, name string) (string, error) { return sectionOf(md, name) }

// runFPMutations runs each mutation as its own subtest against the validator
// that owns it, requiring its specific error and logging the rejection.
func runFPMutations(t *testing.T, md string, check func(string) error, ms []readmeMutation) {
	t.Helper()
	for _, m := range ms {
		t.Run(m.name, func(t *testing.T) {
			err := check(m.apply(t, md))
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("err = %v, want one containing %q", err, m.want)
			}
			t.Logf("rejected: %v", firstLine(err))
		})
	}
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// needAll reports the first of wants absent from whitespace-normalized text.
func needAll(where, text string, wants ...string) error {
	norm := normSpace(text)
	for _, w := range wants {
		if !strings.Contains(norm, normSpace(w)) {
			return fmt.Errorf("%s: lacks %q", where, w)
		}
	}
	return nil
}

// fenceText returns fence id's body, or an error naming it.
func fenceText(md, id string) (string, error) {
	fences, err := parseQuickstartFences(md)
	if err != nil {
		return "", err
	}
	f, ok := fenceByID(fences, id)
	if !ok {
		return "", fmt.Errorf("fence %s is missing", id)
	}
	return f.body, nil
}

// fenceArgs returns fence id's single Callsheet command as literal argv
// (variables render as $NAME) after its leaf, with quoted text intact.
func fenceArgs(md, id string) ([]string, error) {
	fences, err := parseQuickstartFences(md)
	if err != nil {
		return nil, err
	}
	f, ok := fenceByID(fences, id)
	if !ok {
		return nil, fmt.Errorf("fence %s is missing", id)
	}
	parsed, err := parseFenceCommands(f)
	if err != nil {
		return nil, err
	}
	if len(parsed.callsheet) != 1 {
		return nil, fmt.Errorf("fence %s holds %d Callsheet commands, want 1", id, len(parsed.callsheet))
	}
	return renderWords(parsed.callsheet[0].words), nil
}

// renderWords returns words as literal text, variables as $NAME.
func renderWords(words []qsWord) []string {
	var out []string
	for _, w := range words {
		var b strings.Builder
		for _, p := range w.parts {
			if p.variable != "" {
				b.WriteString("$" + p.variable)
			} else {
				b.WriteString(p.text)
			}
		}
		out = append(out, b.String())
	}
	return out
}

// flagValue returns the value after flag in args.
func flagValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// wantFlags requires exact flag values in fence id's command.
func wantFlags(md, id string, want map[string]string) error {
	args, err := fenceArgs(md, id)
	if err != nil {
		return err
	}
	return checkFlagValues(id, args, want)
}

// checkFlagValues requires exact flag values in a fence's argv.
func checkFlagValues(id string, args []string, want map[string]string) error {
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if got, ok := flagValue(args, k); !ok || got != want[k] {
			return fmt.Errorf("fence %s: %s = %q, want %q", id, k, got, want[k])
		}
	}
	return nil
}

// ---- FP-3: installation.

const (
	qsVersionLine = "VERSION='REPLACE_WITH_PUBLISHED_TAG'"
	qsAssetLine   = "ASSET='REPLACE_WITH_ASSET_NAME'"
	qsRegexLine   = `[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$ ]] || { printf 'Replace VERSION with a published release tag\n' >&2; exit 1; }`
	qsCurlAsset   = `curl --fail --location --output "$ASSET" "https://github.com/wedevwork/callsheet/releases/download/$VERSION/$ASSET"`
	qsCurlSums    = `curl --fail --location --output SHA256SUMS "https://github.com/wedevwork/callsheet/releases/download/$VERSION/SHA256SUMS"`
	qsCopyAsset   = `cp "$FIXTURE_DIR/$ASSET" "$ASSET"`
	qsCopySums    = `cp "$FIXTURE_DIR/SHA256SUMS" SHA256SUMS`
	qsAwkLine     = `awk -v asset="$ASSET" '$2 == asset { print; found++ } END { if (found != 1) exit 1 }' SHA256SUMS > selected.sha256`
	qsMissingTag  = "Replace VERSION with a published release tag"
	qsInstallLine = `cp "$ASSET" "$HOME/.local/bin/callsheet"`
)

// qsInstallRows are the four table rows, from the release plan.
func qsInstallRows() map[string][2]string {
	return map[string][2]string{
		devcheck.CallsheetArtifact(devcheck.Target{GOOS: "linux", GOARCH: "amd64"}):  {"Linux", "x86_64"},
		devcheck.CallsheetArtifact(devcheck.Target{GOOS: "linux", GOARCH: "arm64"}):  {"Linux", "aarch64 or arm64"},
		devcheck.CallsheetArtifact(devcheck.Target{GOOS: "darwin", GOARCH: "amd64"}): {"Darwin", "x86_64"},
		devcheck.CallsheetArtifact(devcheck.Target{GOOS: "darwin", GOARCH: "arm64"}): {"Darwin", "arm64"},
	}
}

// checkInstallDoc is FP-3's document validator.
func checkInstallDoc(md string) error {
	body, err := qsDocSection(md, "Install Callsheet")
	if err != nil {
		return err
	}
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	prose := normSpace(md)
	add(needAll("install", prose,
		"Target: a first completed dispatch within 15 minutes after Callsheet and an authenticated vendor CLI are installed and the prerequisites below are satisfied.",
		"This is a target, not a measured result; record installation and prerequisite preparation time separately.",
		"Windows is unsupported"))
	add(needAll("install", stripInlineLinks(body),
		"Open the releases page, select a published version (an RC is marked prerelease), and copy its tag below.",
		"If no release is published yet, use the source-build fallback.",
		"A workflow in the repository does not mean a binary is already available.",
		"Do not use exec.", "`mv callsheet-download callsheet-download-old`", "Do not continue later fences after a failed step.",
		"Only after that reports OK", "never proceed after a checksum mismatch",
		"Source builds print `dev`, not a released version.",
		"The release path needs no Go toolchain."))
	if !slices.Contains(markdownLinks(body), qsReleases) {
		add(errors.New("install: no link to the releases page"))
	}
	if !slices.Contains(markdownLinks(body), "#source-build-fallback") {
		add(errors.New("install: no link to the source-build fallback"))
	}
	if !slices.Contains(markdownLinks(body), "../go.mod") {
		add(errors.New("install: the source fallback does not link go.mod"))
	}
	for _, claim := range []string{"releases/latest", "/latest/", "v1.0.0", "binaries are available", "v1 is available", "is already published", "download the latest"} {
		if strings.Contains(strings.ToLower(md), strings.ToLower(claim)) {
			add(fmt.Errorf("install: false release-availability claim %q", claim))
		}
	}
	for asset, row := range qsInstallRows() {
		if !strings.Contains(body, "| "+row[0]+" | "+row[1]+" | `"+asset+"` |") {
			add(fmt.Errorf("install: no table row for %s", asset))
		}
	}
	if n := strings.Count(body, "| `callsheet-"); n != 4 {
		add(fmt.Errorf("install: %d asset rows, want 4", n))
	}
	fences, err := parseQuickstartFences(md)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	order := []string{"install-shell", "download", "checksum-linux", "checksum-macos", "install", "source-build"}
	last := -1
	for _, id := range order {
		i := slices.IndexFunc(fences, func(f qsFence) bool { return f.id == id })
		if i < 0 {
			add(fmt.Errorf("install: fence %s is missing", id))
			continue
		}
		if i < last {
			add(fmt.Errorf("install: fence %s is out of order", id))
		}
		last = i
		if strings.Contains(fences[i].body, "exec ") {
			add(fmt.Errorf("install: fence %s uses exec", id))
		}
		for _, l := range strings.Split(fences[i].body, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == "read" {
				add(fmt.Errorf("install: fence %s reads input", id))
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	text := func(id string) string {
		f, _ := fenceByID(fences, id)
		return f.body
	}
	if text("install-shell") != "bash --noprofile --norc" {
		add(fmt.Errorf("install: install-shell is %q", text("install-shell")))
	}
	dl := strings.Split(text("download"), "\n")
	wantOrder := []string{"{", "set -euo pipefail", qsVersionLine, qsAssetLine, qsRegexLine, `case "$ASSET" in`,
		"  callsheet-linux-amd64|callsheet-linux-arm64|callsheet-darwin-amd64|callsheet-darwin-arm64) ;;", `  *) printf 'Choose an asset from the table\n' >&2; exit 1 ;;`, "esac",
		"mkdir callsheet-download", "cd callsheet-download", qsCurlAsset, qsCurlSums, qsAwkLine, "}"}
	if !slices.Equal(dl, wantOrder) {
		add(fmt.Errorf("install: the download fence differs from the contract: %q", dl))
	}
	if got := text("checksum-linux"); got != "sha256sum --check selected.sha256" {
		add(fmt.Errorf("install: checksum-linux is %q", got))
	}
	if got := text("checksum-macos"); got != "shasum -a 256 --check selected.sha256" {
		add(fmt.Errorf("install: checksum-macos is %q", got))
	}
	in := strings.Split(text("install"), "\n")
	if !slices.Equal(in, []string{"{", `mkdir -p "$HOME/.local/bin"`, `chmod 755 "$ASSET"`, qsInstallLine, `export PATH="$HOME/.local/bin:$PATH"`, "callsheet version", "}"}) {
		add(fmt.Errorf("install: the install fence differs from the contract: %q", in))
	}
	src := strings.Split(text("source-build"), "\n")
	if !slices.Equal(src, []string{`mkdir -p "$HOME/.local/bin"`, `go build -o "$HOME/.local/bin/callsheet" ./cmd/callsheet`, `export PATH="$HOME/.local/bin:$PATH"`, "callsheet version"}) {
		add(fmt.Errorf("install: the source-build fence differs from the contract: %q", src))
	}
	return errors.Join(errs...)
}

// checkPrerequisitesDoc is FP-3's prerequisites validator.
func checkPrerequisitesDoc(md string) error {
	body, err := qsDocSection(md, "Prerequisites")
	if err != nil {
		return err
	}
	links := markdownLinks(body)
	for _, l := range []string{"real-adapters.md#selection-and-versions", "real-adapters.md#current-worker-recipes", "real-adapters.md#enabling-a-worker", "support-catalog.md", "coordinator.md", "../README.md#security", "deploy.md#security-posture"} {
		if !slices.Contains(links, l) {
			return fmt.Errorf("prerequisites: missing link %s", l)
		}
	}
	return needAll("prerequisites", stripInlineLinks(body),
		"same OS user running the sidecar", "not login, entitlement, model availability or useful headless tool execution",
		"no model default is promised", "Claude `--permission-mode auto`", "Codex `--approve-for-me`", "Grok `--permission-mode auto`", "Cursor `--trust --auto-review`",
		"not commands you append to `sidecar run`", "an absolute executable path on every sidecar start", "Grok and Cursor workers are Linux-only",
		"Installing a macOS binary does not qualify a vendor worker.", "No live qualification harness is a prerequisite.",
		"trusted network and a dedicated OS user", "fresh nonsensitive plain folder")
}

// installCase is one execution of the document's install fences.
type installCase struct {
	name          string
	version       string // the assignment value, "" keeps the placeholder
	asset         string
	mutateFixture func(t *testing.T, fixtures, asset string, manifest *string)
	prepareWork   func(t *testing.T, work string)
	wantErr       string // stderr must contain this
	wantDownload  bool   // a download directory is expected
}

// shellQuote renders a value as one single-quoted shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// installScript builds the script from the shipped fence bodies with only the
// marked replacements, and returns the diagnostics of what it replaced.
func installScript(t *testing.T, md, version, asset string) (string, []string) {
	t.Helper()
	body := func(id string) string {
		s, err := fenceText(md, id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	checksum := "checksum-linux"
	if runtime.GOOS == "darwin" {
		checksum = "checksum-macos"
	}
	var marks []string
	dl := body("download")
	replace := func(old, repl, mark string) {
		if strings.Count(dl, old) != 1 {
			t.Fatalf("install script: %q does not occur exactly once", old)
		}
		dl = strings.Replace(dl, old, repl, 1)
		marks = append(marks, mark+": "+repl)
	}
	if version != "" {
		replace(qsVersionLine, "VERSION="+shellQuote(version), "assignment")
		replace(qsAssetLine, "ASSET="+shellQuote(asset), "assignment")
		replace(qsCurlAsset, qsCopyAsset, "download")
		replace(qsCurlSums, qsCopySums, "download")
	}
	return strings.Join([]string{dl, body(checksum), body("install")}, "\n") + "\n", marks
}

// selectBash is the recipe's interpreter: /bin/bash on darwin, never a
// replacement from PATH; on Linux the one PATH resolves before it is replaced.
func selectBash(t *testing.T) (string, string) {
	t.Helper()
	path := "/bin/bash"
	if runtime.GOOS != "darwin" {
		p, err := exec.LookPath("bash")
		if err != nil {
			t.Fatalf("bash: %v", err)
		}
		if path, err = filepath.Abs(p); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v: %s", path, err, out)
	}
	return path, string(out)
}

// installTools populates the tools directory with the native utilities only,
// and a curl stub that fails loudly.
func installTools(t *testing.T, dir string) {
	t.Helper()
	names := []string{"awk", "mkdir", "chmod", "cp", "sha256sum"}
	if runtime.GOOS == "darwin" {
		names[4] = "shasum"
	}
	for _, n := range names {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if p, err = filepath.Abs(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\nprintf 'curl stub: curl must never run in this test\\n' >&2\nexit 99\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fixtureAssets writes the four script assets and their manifest.
func fixtureAssets(t *testing.T, dir string) (string, string) {
	t.Helper()
	asset := "#!/bin/sh\nprintf 'callsheet v0.0.0-rc.1 protocol=" + strconv.Itoa(contract.ProtocolVersion) + "\\n'\n"
	names := make([]string, 0, 4)
	for name := range qsInstallRows() {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	sum := sha256.Sum256([]byte(asset))
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(asset), 0o644); err != nil {
			t.Fatal(err)
		}
		b.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	return asset, b.String()
}

// runInstallChild launches argv with stdin from a dedicated prefilled pipe and
// proves afterwards, byte for byte, that no child consumed it.
func runInstallChild(t *testing.T, input string, env []string, dir string, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), qsWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdin = env, dir, r
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("run %v: %v", argv, runErr)
	}
	rest := make([]byte, len(input)+16)
	n, _ := r.Read(rest)
	if string(rest[:n]) != input {
		t.Fatalf("stdin was consumed: %q remains of %q", rest[:n], input)
	}
	return out.String(), errOut.String(), code
}

// runInstallCase runs one fresh case and asserts its outcome.
func runInstallCase(t *testing.T, md, bash string, c installCase) {
	t.Helper()
	root := t.TempDir()
	home, work, fixtures, tools := mkdir(t, filepath.Join(root, "home")), mkdir(t, filepath.Join(root, "work")), mkdir(t, filepath.Join(root, "fixtures")), mkdir(t, filepath.Join(root, "tools"))
	installTools(t, tools)
	assetBytes, manifest := fixtureAssets(t, fixtures)
	if c.mutateFixture != nil {
		c.mutateFixture(t, fixtures, c.asset, &manifest)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "SHA256SUMS"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if c.prepareWork != nil {
		c.prepareWork(t, work)
	}
	script, marks := installScript(t, md, c.version, c.asset)
	scriptPath := filepath.Join(root, "script.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("substitutions: %q", marks)
	input := "v0.0.0-rc.1\n" + c.asset + "\n"
	env := []string{"HOME=" + home, "PATH=" + tools, "FIXTURE_DIR=" + fixtures}
	stdout, stderr, code := runInstallChild(t, input, env, work, bash, "--noprofile", "--norc", scriptPath)
	installed := filepath.Join(home, ".local", "bin", "callsheet")
	if strings.Contains(stderr, "curl stub") {
		t.Fatalf("curl was invoked: %s", stderr)
	}
	want := "callsheet v0.0.0-rc.1 protocol=" + strconv.Itoa(contract.ProtocolVersion) + "\n"
	if c.wantErr == "" && c.mutateFixture == nil && c.prepareWork == nil && c.version != "" {
		if code != 0 {
			t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, c.asset+": OK\n") || !strings.HasSuffix(stdout, want) {
			t.Fatalf("stdout = %q", stdout)
		}
		b, err := os.ReadFile(installed)
		if err != nil || string(b) != assetBytes {
			t.Fatalf("installed bytes = %q, %v", b, err)
		}
		st, err := os.Stat(installed)
		if err != nil || st.Mode().Perm() != 0o755 {
			t.Fatalf("installed mode: %v, %v", st, err)
		}
		if got := listTree(t, home); !slices.Equal(got, []string{".local", ".local/bin", ".local/bin/callsheet"}) {
			t.Fatalf("home = %q", got)
		}
		if got := listTree(t, work); !slices.Equal(got, []string{"callsheet-download", "callsheet-download/" + c.asset, "callsheet-download/SHA256SUMS", "callsheet-download/selected.sha256"}) &&
			!slices.Equal(got, []string{"callsheet-download", "callsheet-download/SHA256SUMS", "callsheet-download/" + c.asset, "callsheet-download/selected.sha256"}) {
			t.Fatalf("work = %q", got)
		}
		return
	}
	if code == 0 {
		t.Fatalf("exit 0\nstdout %s\nstderr %s", stdout, stderr)
	}
	if c.wantErr != "" && !strings.Contains(stderr+stdout, c.wantErr) {
		t.Fatalf("output lacks %q\nstdout %s\nstderr %s", c.wantErr, stdout, stderr)
	}
	if _, err := os.Lstat(installed); err == nil {
		t.Fatal("an executable was installed")
	}
	if strings.Contains(stdout, "protocol=") {
		t.Fatalf("version output on a failure: %q", stdout)
	}
	if _, err := os.Lstat(filepath.Join(work, "callsheet-download")); err == nil && !c.wantDownload {
		t.Fatal("a download directory was created")
	}
}

// TestQuickstartInstall (FP-3): the document's install contract, its
// structural mutations, and the shipped fence bodies executed under isolated
// bash with no curl or network, on this native OS.
func TestQuickstartInstall(t *testing.T) {
	md := string(repoFile(t, quickstartDoc))
	check := func(md string) error { return errors.Join(checkInstallDoc(md), checkPrerequisitesDoc(md)) }
	t.Run("document", func(t *testing.T) {
		if err := check(md); err != nil {
			t.Fatalf("shipped quickstart: %v", err)
		}
		runFPMutations(t, md, check, []readmeMutation{
			{"wrong-artifact-row", replaceOnce("| Darwin | arm64 | `callsheet-darwin-arm64` |", "| Darwin | arm64 | `callsheet-macos-arm64` |"), "no table row for callsheet-darwin-arm64"},
			{"extra-asset-row", replaceOnce("| Darwin | arm64 | `callsheet-darwin-arm64` |", "| Darwin | arm64 | `callsheet-darwin-arm64` |\n| Windows | amd64 | `callsheet-windows-amd64` |"), "5 asset rows, want 4"},
			{"wrong-url", replaceOnce("github.com/wedevwork/callsheet/releases/download/$VERSION/$ASSET", "example.invalid/releases/download/$VERSION/$ASSET"), "the download fence differs"},
			{"latest-url", replaceOnce("releases/download/$VERSION/SHA256SUMS", "releases/latest/download/SHA256SUMS"), "false release-availability claim"},
			{"omit-checksum-linux", replaceOnce("sha256sum --check selected.sha256", "true"), "checksum-linux is"},
			{"omit-checksum-macos", replaceOnce("shasum -a 256 --check selected.sha256", "true"), "checksum-macos is"},
			{"chmod-before-checksum", func(t *testing.T, md string) string {
				t.Helper()
				return replaceOnce("mkdir callsheet-download\n", "chmod 755 \"$ASSET\"\nmkdir callsheet-download\n")(t, md)
			}, "the download fence differs"},
			{"install-before-checksum", func(t *testing.T, md string) string {
				t.Helper()
				a, _ := fenceBlock(t, md, "checksum-linux")
				b, _ := fenceBlock(t, md, "install")
				md = strings.Replace(md, a, "@@A@@", 1)
				md = strings.Replace(md, b, a, 1)
				return strings.Replace(md, "@@A@@", b, 1)
			}, "out of order"},
			{"false-availability", appendToSection("Install Callsheet", "Prebuilt binaries are available for every platform."), "false release-availability claim"},
			{"hard-coded-release", appendToSection("Install Callsheet", "Use v1.0.0."), "false release-availability claim"},
			{"remove-conditional", replaceOnce("A workflow in the repository does not mean a binary is already available.", ""), "lacks \"A workflow in the repository"},
			{"remove-source-fallback", func(t *testing.T, md string) string {
				t.Helper()
				b, _ := fenceBlock(t, md, "source-build")
				return strings.Replace(md, b, "", 1)
			}, "fence source-build is missing"},
			{"source-no-go-mod", replaceOnce("[go.mod](../go.mod)", "go.mod"), "does not link go.mod"},
			{"placeholder-replaced-by-fake-release", replaceOnce(qsVersionLine, "VERSION='v1.2.3'"), "the download fence differs"},
			{"read-inserted", replaceOnce(qsVersionLine, "read -r VERSION\n"+qsVersionLine), "fence download reads input"},
			{"read-in-source-build", replaceOnce("go build -o \"$HOME/.local/bin/callsheet\" ./cmd/callsheet", "read -r X\ngo build -o \"$HOME/.local/bin/callsheet\" ./cmd/callsheet"), "fence source-build reads input"},
			{"download-group-removed", func(t *testing.T, md string) string {
				t.Helper()
				return replaceOnce("{\nset -euo pipefail\n", "set -euo pipefail\n")(t, md)
			}, "the download fence differs"},
			{"install-group-removed", replaceOnce("{\nmkdir -p \"$HOME/.local/bin\"\nchmod", "mkdir -p \"$HOME/.local/bin\"\nchmod"), "the install fence differs"},
			{"regex-after-mkdir", func(t *testing.T, md string) string {
				t.Helper()
				md = replaceOnce(qsRegexLine+"\n", "")(t, md)
				return replaceOnce("mkdir callsheet-download\n", "mkdir callsheet-download\n"+qsRegexLine+"\n")(t, md)
			}, "the download fence differs"},
			{"exec-shell", replaceOnce("bash --noprofile --norc\n```", "exec bash --noprofile --norc\n```"), "fence install-shell uses exec"},
			{"no-target", replaceOnce("This is a target, not a measured result", "This is a measured result"), "lacks"},
			{"no-prereq-link", replaceOnce("(real-adapters.md#enabling-a-worker)", "(real-adapters.md)"), "prerequisites: missing link"},
			{"no-login-caveat", replaceOnce("**not login, entitlement, model availability or useful headless tool execution**", "login"), "prerequisites: lacks"},
		})
		if err := checkSourceLinks(quickstartDoc, md, checkoutLookup(testkit.MustRepoRoot(t))); err != nil {
			t.Error(err)
		}
	})

	bash, version := selectBash(t)
	t.Logf("selected %s\n%s", bash, version)
	names := make([]string, 0, 4)
	for name := range qsInstallRows() {
		names = append(names, name)
	}
	slices.Sort(names)
	// The table's rows are exactly the release plan's products.
	plan := devcheck.ReleaseAssets(devcheck.Matrix)
	slices.Sort(plan)
	if !slices.Equal(append(slices.Clone(names), devcheck.ReleaseManifest), append(plan[1:], plan[0])) {
		t.Fatalf("table assets %q, release plan %q", names, plan)
	}
	for _, asset := range names {
		t.Run("install-"+asset, func(t *testing.T) {
			runInstallCase(t, md, bash, installCase{name: asset, version: "v0.0.0-rc.1", asset: asset})
		})
	}
	host := devcheck.CallsheetArtifact(devcheck.Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})
	corrupt := func(t *testing.T, fixtures, asset string, _ *string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fixtures, asset), []byte("#!/bin/sh\necho corrupted\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []installCase{
		{name: "invalid-tag", version: "v01.0.0", asset: host, wantErr: qsMissingTag},
		{name: "tag-with-metadata", version: "v1.0.0+meta", asset: host, wantErr: qsMissingTag},
		{name: "injected-tag", version: "v1.0.0; touch pwned", asset: host, wantErr: qsMissingTag},
		{name: "invalid-asset", version: "v0.0.0-rc.1", asset: "callsheet-windows-amd64", wantErr: "Choose an asset from the table"},
		{name: "unedited-paste", asset: host, wantErr: qsMissingTag},
		{name: "missing-product", version: "v0.0.0-rc.1", asset: host, wantDownload: true, mutateFixture: func(t *testing.T, fixtures, asset string, _ *string) {
			t.Helper()
			if err := os.Remove(filepath.Join(fixtures, asset)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corrupted-product", version: "v0.0.0-rc.1", asset: host, wantDownload: true, mutateFixture: corrupt},
		{name: "missing-manifest-entry", version: "v0.0.0-rc.1", asset: host, wantDownload: true, mutateFixture: func(_ *testing.T, _, asset string, m *string) {
			var kept []string
			for _, l := range strings.Split(strings.TrimSuffix(*m, "\n"), "\n") {
				if !strings.HasSuffix(l, "  "+asset) {
					kept = append(kept, l)
				}
			}
			*m = strings.Join(kept, "\n") + "\n"
		}},
		{name: "duplicate-manifest-entry", version: "v0.0.0-rc.1", asset: host, wantDownload: true, mutateFixture: func(_ *testing.T, _, asset string, m *string) {
			for _, l := range strings.Split(*m, "\n") {
				if strings.HasSuffix(l, "  "+asset) {
					*m += l + "\n"
					return
				}
			}
		}},
		{name: "failed-checksum", version: "v0.0.0-rc.1", asset: host, wantDownload: true, mutateFixture: func(_ *testing.T, _, asset string, m *string) {
			lines := strings.Split(*m, "\n")
			for i, l := range lines {
				if strings.HasSuffix(l, "  "+asset) {
					lines[i] = strings.Repeat("0", 64) + "  " + asset
				}
			}
			*m = strings.Join(lines, "\n")
		}},
		{name: "existing-download-directory", version: "v0.0.0-rc.1", asset: host, wantDownload: true, prepareWork: func(t *testing.T, work string) {
			t.Helper()
			mkdir(t, filepath.Join(work, "callsheet-download"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) { runInstallCase(t, md, bash, c) })
	}

	// The failing child returns to its parent, which stays alive: a
	// non-interactive launch boundary, never a paste or zsh syntax test.
	boundary := func(t *testing.T, parent string) {
		t.Helper()
		root := t.TempDir()
		home, work, tools := mkdir(t, filepath.Join(root, "home")), mkdir(t, filepath.Join(root, "work")), mkdir(t, filepath.Join(root, "tools"))
		installTools(t, tools)
		script, _ := installScript(t, md, "", host)
		p := filepath.Join(root, "script.sh")
		if err := os.WriteFile(p, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		inner := shellQuote(bash) + " --noprofile --norc " + shellQuote(p) + "; printf 'sentinel child=%s\\n' \"$?\""
		out, errOut, code := runInstallChild(t, "v0.0.0-rc.1\n"+host+"\n", []string{"HOME=" + home, "PATH=" + tools}, work, parent, "-c", inner)
		if code != 0 || !strings.Contains(out, "sentinel child=1\n") || !strings.Contains(errOut, qsMissingTag) {
			t.Fatalf("parent = %d, stdout %q, stderr %q", code, out, errOut)
		}
	}
	t.Run("parent-survives-sh", func(t *testing.T) { boundary(t, "/bin/sh") })
	if zsh, err := exec.LookPath("zsh"); err == nil {
		t.Run("parent-survives-zsh", func(t *testing.T) { boundary(t, zsh) })
	}
}

// fenceBlock returns fence id's complete Markdown block, marker included.
func fenceBlock(t *testing.T, md, id string) (string, int) {
	t.Helper()
	return markedFenceBlock(t, md, "<!-- quickstart: "+id+" -->")
}

// markedFenceBlock returns the complete block that starts at marker.
func markedFenceBlock(t *testing.T, md, marker string) (string, int) {
	t.Helper()
	i := strings.Index(md, marker)
	if i < 0 {
		t.Fatalf("no marker %s", marker)
	}
	open := strings.Index(md[i:], "```")
	if open < 0 {
		t.Fatalf("no fence after %s", marker)
	}
	end := strings.Index(md[i+open+3:], "```")
	if end < 0 {
		t.Fatalf("unclosed fence %s", marker)
	}
	stop := i + open + 3 + end + 3
	return md[i:stop], i
}

// ---- FP-4 and FP-9: the document-derived lifecycle.

// qsRig is one independent one-machine deployment driven by documented argv.
type qsRig struct {
	t       *testing.T
	md      string
	fences  []qsFence
	root    *cli.Command
	p       *planeCLI
	vars    map[string]string
	home    string
	fake    string
	plane   *planeProc
	sidecar *sidecarProc
}

// qsPrefix keeps every task checkout path short enough for native PATH_MAX.
const qsPrefix = "csqs-"

func newQuickstartRig(t *testing.T) *qsRig {
	t.Helper()
	root, err := os.MkdirTemp("", qsPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	home, cwd := mkdir(t, filepath.Join(root, "home")), mkdir(t, filepath.Join(root, "cwd"))
	md := string(repoFile(t, quickstartDoc))
	fences, err := parseQuickstartFences(md)
	if err != nil {
		t.Fatal(err)
	}
	return &qsRig{t: t, md: md, fences: fences, root: cli.NewTree(runtime.GOOS), home: home, fake: fakeAdapterBinary(t), vars: map[string]string{"HOME": home, "PATH": os.Getenv("PATH")},
		p: &planeCLI{bin: nodeBinary(t), home: home, cwd: cwd, env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}}}
}

func (r *qsRig) fence(id string) qsParsed {
	r.t.Helper()
	f, ok := fenceByID(r.fences, id)
	if !ok {
		r.t.Fatalf("no fence %s", id)
	}
	parsed, err := parseFenceCommands(f)
	if err != nil {
		r.t.Fatal(err)
	}
	return parsed
}

// argv returns fence id's single command with the variable allowlist and the
// fixture translations applied: the fake adapter and fixture model only.
func (r *qsRig) argv(id string) []string {
	r.t.Helper()
	parsed := r.fence(id)
	if len(parsed.callsheet) != 1 {
		r.t.Fatalf("fence %s: %d commands", id, len(parsed.callsheet))
	}
	var out []string
	for _, w := range parsed.callsheet[0].words[1:] {
		v, err := w.expand(r.vars)
		if err != nil {
			r.t.Fatalf("fence %s: %v", id, err)
		}
		out = append(out, v)
	}
	if err := translateFixtureArgs(out, r.fake); err != nil {
		r.t.Fatalf("fence %s: %v", id, err)
	}
	return out
}

// translateFixtureArgs applies the fixture substitutions to args in place.
func translateFixtureArgs(args []string, fake string) error {
	for i, a := range args {
		if slices.Contains([]string{"--claude-adapter", "--adapter", "--bind", "--goal"}, a) && i+1 == len(args) {
			return fmt.Errorf("%s has no value", a)
		}
		switch {
		case a == "--claude-adapter":
			args[i] = "--fake-adapter"
			args[i+1] = fake
		case a == "--adapter" && args[i+1] == "claude":
			args[i+1] = "fake"
		case a == "--bind" && args[i+1] == qsDefaultBind:
			args[i+1] = "127.0.0.1:0"
		case a == "--goal":
			if args[i+1] != qsGoal {
				return fmt.Errorf("documented goal %q", args[i+1])
			}
			args[i+1] = fakeadapter.WorkspaceGoal(fakeadapter.WorkspaceScript{Ops: []fakeadapter.WorkspaceOp{{Write: "hello.txt", Data: qsHello}}})
		}
	}
	return nil
}

// argsAfterLeaf returns args without their leading command path.
func argsAfterLeaf(args []string, leaf ...string) ([]string, error) {
	if len(args) < len(leaf) || !slices.Equal(args[:len(leaf)], leaf) {
		return nil, fmt.Errorf("args %q: want the command %q", args, strings.Join(leaf, " "))
	}
	return args[len(leaf):], nil
}

// withJSON inserts the only allowed output-format substitution after the leaf.
func (r *qsRig) withJSON(id string, args []string) []string {
	r.t.Helper()
	parsed := r.fence(id)
	if len(parsed.callsheet) != 1 {
		r.t.Fatalf("fence %s: %d commands", id, len(parsed.callsheet))
	}
	leaf, _, err := leafOf(r.root, parsed.callsheet[0])
	if err != nil {
		r.t.Fatal(err)
	}
	n := len(strings.Fields(leaf))
	if n > len(args) {
		r.t.Fatalf("fence %s: args %q are shorter than the command %q", id, args, leaf)
	}
	return slices.Concat(args[:n], []string{"--json"}, args[n:])
}

// setup applies an sh fence's setup operations; read prompts take their value
// from the fixture variables already set.
func (r *qsRig) setup(id string) {
	r.t.Helper()
	for _, c := range r.fence(id).setup {
		first, ok := c.words[0].literal()
		if !ok {
			if len(c.words) == 2 {
				continue // the worker version check: a vendor step, not a fixture.
			}
			first = ""
		}
		switch {
		case first == "mkdir":
			if err := os.Mkdir(r.expand(c.words[1]), 0o755); err != nil {
				r.t.Fatal(err)
			}
		case first == "printf":
			r.writePrintf(c)
		case first == "read":
			name, _ := c.words[2].literal()
			if _, ok := r.vars[name]; !ok {
				r.t.Fatalf("fence %s: no fixture value for prompted %s", id, name)
			}
		case first == "export", first == "cat", first == "cmp":
		default:
			name, val, _ := strings.Cut(c.words[0].parts[0].text, "=")
			rest := qsWord{parts: slices.Clone(c.words[0].parts)}
			rest.parts[0].text = val
			r.vars[name] = r.expand(rest)
		}
	}
}

func (r *qsRig) expand(w qsWord) string {
	r.t.Helper()
	v, err := w.expand(r.vars)
	if err != nil {
		r.t.Fatal(err)
	}
	return v
}

// printfOutput renders the documented printf: a literal with only \n, or %s\n
// with one argument. It is never handed to a shell.
func printfOutput(c qsCommand) (string, error) {
	if len(c.words) < 2 || len(c.words) > 3 {
		return "", fmt.Errorf("line %d: printf takes a format and at most one argument", c.line)
	}
	format, _ := c.words[1].literal()
	if len(c.words) == 3 {
		arg, _ := c.words[2].literal()
		if format != `%s\n` {
			return "", fmt.Errorf("line %d: unsupported printf format %q", c.line, format)
		}
		return arg + "\n", nil
	}
	body := strings.ReplaceAll(format, `\n`, "\n")
	if strings.ContainsAny(body, `\%`) {
		return "", fmt.Errorf("line %d: unsupported printf format %q", c.line, format)
	}
	return body, nil
}

func (r *qsRig) writePrintf(c qsCommand) {
	r.t.Helper()
	body, err := printfOutput(c)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.expand(*c.redirect), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *qsRig) ok(what string, args ...string) result {
	r.t.Helper()
	res := r.p.run(r.t, args...)
	if res.code != 0 {
		r.t.Fatalf("%s: %+v", what, res)
	}
	return res
}

// await polls cond until it holds, bounded by the harness deadline.
func (r *qsRig) await(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.After(qsWait)
	for !cond() {
		select {
		case <-deadline:
			if cond() {
				return
			}
			r.t.Fatalf("%s: not reached within %v", what, qsWait)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// startSidecar runs the documented sidecar run argv under the fixture's
// workspace mode, owned and reaped by test cleanup.
func (r *qsRig) startSidecar(id string) {
	r.t.Helper()
	r.startSidecarArgs(r.argv(id))
}

// startSidecarArgs runs a sidecar from already translated documented argv.
func (r *qsRig) startSidecarArgs(args []string) {
	r.t.Helper()
	s := &sidecarProc{logs: &nodeLogs{sig: make(chan struct{}, 1)}, done: make(chan struct{})}
	s.cmd = exec.Command(r.p.bin, args...)
	s.cmd.Dir = r.p.cwd
	s.cmd.Env = append(slices.Clone(r.p.env), fakeadapter.EnvTaskMode+"="+fakeadapter.WorkspaceMode)
	s.cmd.Stdout, s.cmd.Stderr = &s.stdout, s.logs
	s.cmd.WaitDelay = 5 * time.Second
	if err := s.cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	go func() { s.err = s.cmd.Wait(); close(s.done) }()
	r.t.Cleanup(func() {
		s.cmd.Process.Signal(os.Interrupt)
		select {
		case <-s.done:
			return
		case <-time.After(nodeWait):
		}
		s.cmd.Process.Kill()
		select {
		case <-s.done:
		case <-time.After(nodeWait):
			r.t.Errorf("sidecar %d not reaped", s.cmd.Process.Pid)
		}
	})
	r.sidecar = s
}

// setupQuickstart is the shared helper: the documented plane, sidecar and role
// setup through a ready roster. Each caller gets its own processes.
func setupQuickstart(t *testing.T) *qsRig {
	t.Helper()
	r := newQuickstartRig(t)
	r.setup("env-plane")
	r.ok("plane init", r.argv("plane-init")...)
	planeArgs, err := argsAfterLeaf(r.argv("plane-run"), "plane", "run")
	if err != nil {
		t.Fatalf("fence plane-run: %v", err)
	}
	r.plane = r.p.start(t, planeArgs...)

	r.setup("env-sidecar")
	if r.vars["PLANE"] != qsDefaultURL {
		t.Fatalf("documented PLANE = %q", r.vars["PLANE"])
	}
	url := "https://" + r.plane.addr
	r.vars["PLANE"] = url
	out := r.ok("sidecar enroll", r.argv("sidecar-enroll")...)
	id, ok := strings.CutPrefix(strings.Split(out.stdout, "\n")[0], "node_id: ")
	if !ok || !contract.ValidNodeID(id) {
		t.Fatalf("enroll = %+v", out)
	}
	r.vars["WORKER"] = r.fake
	r.setup("worker-path")
	r.startSidecar("sidecar-run")

	r.setup("env-operator")
	if r.vars["PLANE"] != qsDefaultURL {
		t.Fatalf("documented operator PLANE = %q", r.vars["PLANE"])
	}
	r.vars["PLANE"] = url
	r.setup("manuals-and-seed")
	r.vars["NODE"], r.vars["MODEL"] = id, qsFakeModel
	r.setup("node-and-model")
	r.await("node online", func() bool {
		res := r.p.run(t, r.withJSON("node-list", r.argv("node-list"))...)
		if res.code != 0 {
			return false
		}
		nodes, err := contract.ParseNodeListResponse([]byte(strings.TrimSuffix(res.stdout, "\n")))
		return err == nil && slices.ContainsFunc(nodes, func(n contract.Node) bool { return n.ID == id && n.Liveness == contract.LivenessOnline })
	})
	r.ok("role add", r.argv("role-add")...)
	r.await("role can_accept", func() bool {
		res := r.p.run(t, r.withJSON("role-list", r.argv("role-list"))...)
		if res.code != 0 {
			return false
		}
		roles, err := contract.ParseRoleListResponse([]byte(strings.TrimSuffix(res.stdout, "\n")), nil)
		return err == nil && slices.ContainsFunc(roles, func(v contract.RoleView) bool {
			return v.ID == "quickstart-local" && v.Node == id && v.CanAccept && v.Effort == "low" && v.Model == qsFakeModel
		})
	})
	return r
}

// checkSetupDoc is FP-4's document validator.
func checkSetupDoc(md string) error {
	body, err := qsDocSection(md, "One machine")
	if err != nil {
		return err
	}
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(needAll("setup", stripInlineLinks(body),
		"**A** runs the plane, **B** the sidecar, **C** is your operator", "Stay outside any existing git repository",
		"Never share `pki/ca.key` or `server.key`", "A shell alias is not a worker executable", "they live outside the seed",
		"Expect the node online and the role's `CAN_ACCEPT` true", "Never retry `role add` blindly",
		"Supply the absolute installed `callsheet` path, `PLANE` and `CA`", "ask it to list nodes and roles with Callsheet MCP", "It must see `quickstart-local`",
		"Do not run a second dispatch from the coordinator", "do not disable sandboxing"))
	if !slices.Contains(markdownLinks(body), "coordinator.md#claude-code") {
		add(errors.New("setup: no link to the Claude Code coordinator recipe"))
	}
	if !strings.Contains(md, "```text\n"+qsCoordinator+"\n```") {
		add(errors.New("setup: no literal coordinator runbook"))
	}
	if txt, err := fenceText(md, "coordinator-runbook"); err != nil || txt != "printf '%s\\n' '"+qsCoordinator+"' > \"$QS/coordinator.md\"" {
		add(fmt.Errorf("setup: coordinator-runbook does not write the literal runbook (%v)", err))
	}
	add(wantFlags(md, "plane-init", map[string]string{"--state-dir": "$QS/plane", "--bind": qsDefaultBind, "--san": "127.0.0.1"}))
	add(wantFlags(md, "plane-run", map[string]string{"--state-dir": "$QS/plane"}))
	add(wantFlags(md, "sidecar-enroll", map[string]string{"--state-dir": "$QS/sidecar", "--plane": "$PLANE", "--ca": "$CA"}))
	add(wantFlags(md, "sidecar-run", map[string]string{"--state-dir": "$QS/sidecar", "--claude-adapter": "$WORKER"}))
	add(wantFlags(md, "role-add", map[string]string{"--name": "quickstart", "--node": "$NODE", "--adapter": "claude", "--instruction": "$QS/manuals/instruction.md",
		"--runbook": "$QS/manuals/runbook.md", "--model": "$MODEL", "--effort": "low", "--concurrency": "1", "--timeout": "5m", "--plane": "$PLANE", "--ca": "$CA"}))
	if args, err := fenceArgs(md, "role-add"); err == nil && (len(args) < 4 || args[3] != "quickstart-local") {
		add(fmt.Errorf("setup: role-add ID = %q", args))
	}
	fences, err := parseQuickstartFences(md)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	f, ok := fenceByID(fences, "manuals-and-seed")
	if !ok {
		return errors.Join(append(errs, errors.New("setup: fence manuals-and-seed is missing"))...)
	}
	parsed, err := parseFenceCommands(f)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	want := map[string]string{"$QS/manuals/instruction.md": qsInstruction, "$QS/manuals/runbook.md": qsRunbook, "$QS/seed/README.md": qsSeedReadme}
	for _, c := range parsed.setup {
		if c.redirect == nil {
			continue
		}
		var target strings.Builder
		for _, p := range c.redirect.parts {
			if p.variable != "" {
				target.WriteString("$" + p.variable)
			} else {
				target.WriteString(p.text)
			}
		}
		got, err := printfOutput(c)
		if err != nil || want[target.String()] != got {
			add(fmt.Errorf("setup: %s holds %q (%v)", target.String(), got, err))
		}
		delete(want, target.String())
	}
	for k := range want {
		add(fmt.Errorf("setup: %s is not written", k))
	}
	return errors.Join(errs...)
}

// TestQuickstartSetup (FP-4): document-derived setup reaches a ready roster on
// the fake adapter, and its document validator rejects each mutation. It ends
// at the ready roster, before workspace creation.
func TestQuickstartSetup(t *testing.T) {
	md := string(repoFile(t, quickstartDoc))
	t.Run("document", func(t *testing.T) {
		if err := checkSetupDoc(md); err != nil {
			t.Fatalf("shipped quickstart: %v", err)
		}
		runFPMutations(t, md, checkSetupDoc, []readmeMutation{
			{"missing-instruction-flag", replaceOnce("  --instruction \"$QS/manuals/instruction.md\" --runbook", "  --runbook"), "--instruction"},
			{"missing-runbook-flag", replaceOnce("--runbook \"$QS/manuals/runbook.md\" \\\n  --model \"$MODEL\" --effort low --concurrency 1 --timeout 5m --plane \"$PLANE\" --ca \"$CA\"\n```\n\nConfirm it can", "\\\n  --model \"$MODEL\" --effort low --concurrency 1 --timeout 5m --plane \"$PLANE\" --ca \"$CA\"\n```\n\nConfirm it can"), "--runbook"},
			{"wrong-node-binding", replaceOnce("--name quickstart --node \"$NODE\"", "--name quickstart --node \"$OTHER\""), "--node"},
			{"role-wrong-id", replaceOnce("role add quickstart-local", "role add quickstart-other"), "role-add ID"},
			{"removed-coordinator-instruction", replaceOnce("ask it to list nodes and roles with Callsheet MCP", "start it"), "lacks \"ask it to list nodes and roles"},
			{"removed-coordinator-pointer", replaceOnce("(coordinator.md#claude-code)", "(coordinator.md)"), "no link to the Claude Code coordinator recipe"},
			{"removed-absolute-path", replaceOnce("Supply the absolute installed `callsheet` path, `PLANE` and `CA`", "Supply the path"), "lacks"},
			{"changed-runbook", replaceOnce("```text\nUse the Callsheet MCP server", "```text\nUse Callsheet"), "no literal coordinator runbook"},
			{"runbook-not-written", replaceOnce("> \"$QS/coordinator.md\"", "> \"$QS/other.md\""), "coordinator-runbook does not write"},
			{"changed-manual", replaceOnce("printf 'Work only in the task checkout.", "printf 'Work anywhere."), "instruction.md holds"},
			{"seed-not-written", replaceOnce("printf 'Callsheet quickstart scratch project.\\n' > \"$QS/seed/README.md\"\n", ""), "seed/README.md is not written"},
			{"no-can-accept-check", replaceOnce("Expect the node online and the role's `CAN_ACCEPT` true", "Continue"), "lacks"},
			{"wrong-bind", replaceOnce("--bind 127.0.0.1:8443", "--bind 0.0.0.0:8443"), "--bind"},
		})
	})
	t.Run("ready-roster", func(t *testing.T) {
		r := setupQuickstart(t)
		for path, want := range map[string]string{"manuals/instruction.md": qsInstruction, "manuals/runbook.md": qsRunbook, "seed/README.md": qsSeedReadme} {
			if b, err := os.ReadFile(filepath.Join(r.vars["QS"], filepath.FromSlash(path))); err != nil || string(b) != want {
				t.Errorf("%s = %q, %v", path, b, err)
			}
		}
		if got := r.ok("role ls", r.argv("role-list")...); !strings.Contains(got.stdout, "\tquickstart-local\t") {
			t.Errorf("role ls = %q", got.stdout)
		}
		if got := r.ok("node ls", r.argv("node-list")...); !strings.Contains(got.stdout, r.vars["NODE"]+"\tonline\t") {
			t.Errorf("node ls = %q", got.stdout)
		}
	})
}

// checkFirstTaskDoc is FP-9's document validator.
func checkFirstTaskDoc(md string) error {
	body, err := qsDocSection(md, "One machine")
	if err != nil {
		return err
	}
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(needAll("first task", stripInlineLinks(body),
		"Do not infer an ID or dispatch again if the response is lost", "Ctrl-C stops the watcher, not the task", "Its exit 0 means a terminal answer, not task success",
		"Require state `succeeded`, the expected final message, workspace publication `published` and `available: true`",
		"The acceptance string is an instruction only", "The delivery folder must be absent or empty", "does not overwrite a dirty git checkout or push to GitHub",
		"wait for each foreground command to return", "there is no destructive cleanup step"))
	add(wantFlags(md, "workspace-push", map[string]string{"--instance": "$INSTANCE", "--plane": "$PLANE", "--ca": "$CA"}))
	add(wantFlags(md, "dispatch", map[string]string{"--role-id": "quickstart-local", "--goal": qsGoal, "--acceptance": qsAcceptance, "--workspace": "quickstart", "--base": "main",
		"--workspace-instance": "$INSTANCE", "--plane": "$PLANE", "--ca": "$CA"}))
	add(wantFlags(md, "task-show", map[string]string{"--lines": "0"}))
	if txt, err := fenceText(md, "inspect-result"); err != nil || !strings.Contains(txt, `cat "$QS/delivery/hello.txt"`) || !strings.Contains(txt, `cmp "$QS/seed/README.md" "$QS/delivery/README.md"`) {
		add(fmt.Errorf("first task: inspect-result does not check the delivery (%v)", err))
	}
	links := markdownLinks(md)
	for _, l := range []string{"coordinator.md", "workspaces.md", "examples/dev-flow/README.md", "deploy.md"} {
		if !slices.Contains(links, l) {
			add(fmt.Errorf("next steps: missing link %s", l))
		}
	}
	if slices.Contains(links, "guide/index.md") {
		add(errors.New("next steps: links future document guide/index.md"))
	}
	return errors.Join(errs...)
}

// checkTroubleshootingDoc is the failure guidance validator, timed_out
// included.
func checkTroubleshootingDoc(md string) error {
	body, err := qsDocSection(md, "If something goes wrong")
	if err != nil {
		return err
	}
	if err := needAll("troubleshooting", stripInlineLinks(body),
		"Download 404", "Checksum mismatch", "TLS or trust failure", "Never use an insecure flag or copy a key", "Offline or no capacity", "Unready worker", "Readiness does not prove login",
		"Failed or refused task", "Pull unavailable", "Silent wait", "Never duplicate work on a missing reply"); err != nil {
		return err
	}
	return checkTimedOutGuidance(body)
}

func checkTimedOutGuidance(body string) error {
	return needAll("timed_out", body, "`timed_out`", "5m execution deadline", "without a worker malfunction", "larger explicit dispatch `--timeout`", "Do not relabel this state failed or retry automatically",
		"check the prior task before starting more work")
}

// checkDelivery requires exactly the seed README and hello.txt with the exact
// bytes, and the seed unchanged.
func checkDelivery(delivery, seed map[string]string) error {
	want := map[string]string{"README.md": qsSeedReadme, "hello.txt": qsHello}
	if len(delivery) != len(want) {
		return fmt.Errorf("delivery holds %d files, want %d", len(delivery), len(want))
	}
	for name, body := range want {
		if got, ok := delivery[name]; !ok || got != body {
			return fmt.Errorf("delivery %s = %q, want %q", name, got, body)
		}
	}
	if len(seed) != 1 || seed["README.md"] != qsSeedReadme {
		return fmt.Errorf("seed changed: %q", seed)
	}
	return nil
}

func readFolder(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rel := range listTree(t, dir) {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(rel)] = string(b)
	}
	return out
}

// TestQuickstartOneMachine (FP-9): after its own document-derived setup, the
// documented workspace seed, dispatch, wait, show, status and pull with the
// fake worker; the fake goal substitution is explicit test-only behavior.
func TestQuickstartOneMachine(t *testing.T) {
	md := string(repoFile(t, quickstartDoc))
	t.Run("document", func(t *testing.T) {
		check := func(md string) error { return errors.Join(checkFirstTaskDoc(md), checkTroubleshootingDoc(md)) }
		if err := check(md); err != nil {
			t.Fatalf("shipped quickstart: %v", err)
		}
		runFPMutations(t, md, check, []readmeMutation{
			{"missing-instance-binding", replaceOnce("--workspace quickstart --base main --workspace-instance \"$INSTANCE\" --plane \"$PLANE\" --ca \"$CA\"\n```\n\nRecord", "--workspace quickstart --base main --plane \"$PLANE\" --ca \"$CA\"\n```\n\nRecord"), "--workspace-instance"},
			{"missing-workspace-binding", replaceOnce("--workspace quickstart --base main --workspace-instance \"$INSTANCE\" --plane \"$PLANE\" --ca \"$CA\"\n```\n\nRecord", "--plane \"$PLANE\" --ca \"$CA\"\n```\n\nRecord"), "--base"},
			{"changed-goal", replaceOnce("--goal \"Create hello.txt containing", "--goal \"Make hello.txt containing"), "--goal"},
			{"missing-publication-check", replaceOnce("workspace publication `published` and `available: true`", "a zero exit"), "lacks \"Require state"},
			{"exit-means-success", replaceOnce("Its exit 0 means a terminal answer, not task success", "Its exit 0 means success"), "lacks"},
			{"redispatch", replaceOnce("Do not infer an ID or dispatch again if the response is lost", "Dispatch again if the response is lost"), "lacks"},
			{"no-inspection", replaceOnce("cmp \"$QS/seed/README.md\" \"$QS/delivery/README.md\"\n", ""), "inspect-result does not check"},
			{"future-link", appendToSection("Next steps", "- [Guide](guide/index.md)."), "links future document guide/index.md"},
			{"no-deploy-link", replaceOnce("- [Deployment](deploy.md): sustained operation, services, address changes and recovery.\n", ""), "missing link deploy.md"},
			{"no-next-step", replaceOnce("- [Development-flow example](examples/dev-flow/README.md): a reusable starting point.\n", ""), "missing link examples/dev-flow/README.md"},
			{"no-symptom", replaceOnce("| Silent wait |", "| Waiting |"), "lacks \"Silent wait\""},
			{"insecure-advice", replaceOnce("Never use an insecure flag or copy a key", "Use an insecure flag"), "lacks"},
		})
		// The timed_out guidance is validated separately.
		body, err := qsDocSection(md, "If something goes wrong")
		if err != nil {
			t.Fatal(err)
		}
		if err := checkTimedOutGuidance(stripInlineLinks(body)); err != nil {
			t.Fatal(err)
		}
		runFPMutations(t, stripInlineLinks(body), checkTimedOutGuidance, []readmeMutation{
			{"relabel", replaceOnce("Do not relabel this state failed or retry automatically", "Treat it as failed and retry"), "lacks"},
			{"no-deadline", replaceOnce("5m execution deadline", "deadline"), "lacks"},
			{"no-larger-timeout", replaceOnce("larger explicit dispatch `--timeout`", "retry"), "lacks"},
		})
		good := map[string]string{"README.md": qsSeedReadme, "hello.txt": qsHello}
		seed := map[string]string{"README.md": qsSeedReadme}
		if err := checkDelivery(good, seed); err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			delivery, seed map[string]string
			want           string
		}{
			"wrong-bytes":   {map[string]string{"README.md": qsSeedReadme, "hello.txt": "hello\n"}, seed, "delivery hello.txt"},
			"no-newline":    {map[string]string{"README.md": qsSeedReadme, "hello.txt": strings.TrimSuffix(qsHello, "\n")}, seed, "delivery hello.txt"},
			"readme-edited": {map[string]string{"README.md": "x\n", "hello.txt": qsHello}, seed, "delivery README.md"},
			"extra-file":    {map[string]string{"README.md": qsSeedReadme, "hello.txt": qsHello, "x": "y"}, seed, "holds 3 files"},
			"missing-file":  {map[string]string{"README.md": qsSeedReadme}, seed, "holds 1 files"},
			"seed-changed":  {good, map[string]string{"README.md": qsSeedReadme, "hello.txt": qsHello}, "seed changed"},
		} {
			t.Run(name, func(t *testing.T) {
				err := checkDelivery(c.delivery, c.seed)
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want %q", err, c.want)
				}
				t.Logf("rejected: %v", err)
			})
		}
	})
	t.Run("lifecycle", func(t *testing.T) {
		r := setupQuickstart(t)
		created := r.ok("ws create", r.withJSON("workspace-create", r.argv("workspace-create"))...)
		view, err := contract.ParseWorkspaceView([]byte(strings.TrimSuffix(created.stdout, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		r.vars["INSTANCE"] = view.Instance
		r.setup("read-instance")
		r.ok("ws push", r.argv("workspace-push")...)

		admitted := r.ok("dispatch", r.withJSON("dispatch", r.argv("dispatch"))...)
		task, err := contract.ParseDispatchResponse([]byte(strings.TrimSuffix(admitted.stdout, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		r.vars["TASK"] = task.TaskID
		r.setup("read-task")
		// task wait owns no process beyond this bounded child.
		ctx, cancel := context.WithTimeout(context.Background(), qsWait)
		defer cancel()
		wait := exec.CommandContext(ctx, r.p.bin, r.argv("wait")...)
		wait.Dir, wait.Env = r.p.cwd, r.p.env
		wait.WaitDelay = 5 * time.Second
		if out, err := wait.CombinedOutput(); err != nil {
			t.Fatalf("task wait: %v: %s", err, out)
		}

		show := r.ok("task show", r.withJSON("task-show", r.argv("task-show"))...)
		done, err := contract.ParseTaskShowResponse([]byte(strings.TrimSuffix(show.stdout, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		if done.State != contract.TaskSucceeded || done.Result == nil {
			t.Fatalf("task = %+v", done)
		}
		status := r.ok("ws status", r.withJSON("workspace-status", r.argv("workspace-status"))...)
		st, err := contract.ParseTaskWorkspaceStatusResponse([]byte(strings.TrimSuffix(status.stdout, "\n")))
		if err != nil {
			t.Fatal(err)
		}
		if st.Workspace.State != contract.TaskSucceeded || st.Workspace.Result == nil || st.Workspace.Result.Publication != contract.PublicationPublished || !st.Workspace.Available {
			t.Fatalf("status = %+v", st.Workspace)
		}
		// The documented plain forms show what the text tells the reader to require.
		for _, c := range []struct {
			id    string
			lines []string
		}{
			{"task-show", []string{"state: succeeded", "publication: published"}},
			{"workspace-status", []string{"state: succeeded", "publication: published", "available: true"}},
		} {
			got := r.ok(c.id, r.argv(c.id)...)
			for _, line := range c.lines {
				if !hasLine(got.stdout, line) {
					t.Errorf("%s lacks %q:\n%s", c.id, line, got.stdout)
				}
			}
		}
		r.ok("ws pull", r.argv("pull")...)
		r.setup("inspect-result")
		qs := r.vars["QS"]
		if err := checkDelivery(readFolder(t, filepath.Join(qs, "delivery")), readFolder(t, filepath.Join(qs, "seed"))); err != nil {
			t.Fatal(err)
		}
		r.ok("task logs", r.argv("diagnostics")...)
	})
}

// ---- FP-5: the second machine.

// checkSecondMachineDoc is FP-5's static validator.
func checkSecondMachineDoc(md string) error {
	body, err := qsDocSection(md, "Add a second machine")
	if err != nil {
		return err
	}
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(needAll("second machine", stripInlineLinks(body),
		"**machine A** and **machine B** are distinct computers", "no tasks in flight", "**same Callsheet version**", "never 127.0.0.1 or 0.0.0.0",
		"`plane run --bind NEW` is not a configuration update", "stop the local sidecar and plane foreground commands", "Do not delete or reinitialize state",
		"edit **only** `bind` in `$QS/plane/config.json`", "the CA is unchanged", "Copy only `$QS/plane/pki/ca.crt`",
		"Never copy plane state, `ca.key`, `server.key`, sidecar state, node identity or vendor credentials", "Its node ID remains unchanged",
		"machine B's own, distinct node ID", "Never copy machine A's", "`--role-name` could select the original local node and is forbidden",
		"no manual transfer of seed, results or vendor authentication"))
	if !strings.Contains(body, `"bind": "127.0.0.1:8443"`) || !strings.Contains(body, `"bind": "192.168.1.10:8443"`) || !strings.Contains(body, "illustrative") {
		add(errors.New("second machine: no illustrative before and after configuration"))
	}
	if !slices.Contains(markdownLinks(body), "deploy.md#change-the-plane-address") {
		add(errors.New("second machine: no link to the canonical address-change procedure"))
	}
	for _, forbidden := range []string{"cp -r", "rm -rf", "--insecure", "0.0.0.0:8443", "ca.key\"", "cp \"$QS/plane/pki/ca.key"} {
		if strings.Contains(body, forbidden) {
			add(fmt.Errorf("second machine: forbidden %q", forbidden))
		}
	}
	// The ordered stop, edit, reissue, restart and re-enroll sequence.
	order := []string{"stop the local sidecar", "edit **only** `bind`", "quickstart: remote-cert", "quickstart: remote-plane-run", "quickstart: local-reenroll", "quickstart: local-restart",
		"Copy only `$QS/plane/pki/ca.crt`", "quickstart: remote-env", "quickstart: remote-manuals", "quickstart: remote-enroll", "quickstart: remote-sidecar-run", "quickstart: remote-role-add",
		"quickstart: remote-roster", "quickstart: remote-dispatch", "quickstart: remote-wait", "quickstart: remote-show", "quickstart: remote-status", "quickstart: remote-pull"}
	at := -1
	for _, s := range order {
		i := strings.Index(body, s)
		if i < 0 {
			add(fmt.Errorf("second machine: lacks step %q", s))
			continue
		}
		if i < at {
			add(fmt.Errorf("second machine: step %q is out of order", s))
		}
		at = i
	}
	cert, err := fenceArgs(md, "remote-cert")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if want := []string{"callsheet", "plane", "cert", "reissue", "--state-dir", "$QS/plane", "--san", "127.0.0.1", "--san", "$PLANE_IP"}; !slices.Equal(cert, want) {
		add(fmt.Errorf("second machine: remote-cert = %q", cert))
	}
	if run, err := fenceArgs(md, "remote-plane-run"); err != nil || slices.Contains(run, "--bind") {
		add(fmt.Errorf("second machine: remote-plane-run = %q (%v)", run, err))
	}
	if txt, err := fenceText(md, "remote-url"); err != nil || !strings.Contains(txt, `PLANE="https://$PLANE_IP:8443"`) {
		add(fmt.Errorf("second machine: remote-url does not use the private address (%v)", err))
	}
	txt, err := fenceText(md, "remote-env")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, s := range []string{`QS="$HOME/callsheet-quickstart-remote"`, `mkdir "$QS"`, `PLANE="https://$PLANE_IP:8443"`, `CA="$QS/plane-ca.crt"`} {
		if !strings.Contains(txt, s) {
			add(fmt.Errorf("second machine: remote-env lacks %s", s))
		}
	}
	add(wantFlags(md, "local-reenroll", map[string]string{"--state-dir": "$QS/sidecar", "--plane": "$PLANE", "--ca": "$CA"}))
	add(wantFlags(md, "local-restart", map[string]string{"--state-dir": "$QS/sidecar", "--claude-adapter": "$WORKER"}))
	add(wantFlags(md, "remote-enroll", map[string]string{"--state-dir": "$QS/sidecar", "--plane": "$PLANE", "--ca": "$CA"}))
	add(wantFlags(md, "remote-sidecar-run", map[string]string{"--state-dir": "$QS/sidecar", "--claude-adapter": "$WORKER"}))
	add(wantFlags(md, "remote-role-add", map[string]string{"--name": "quickstart", "--node": "$REMOTE_NODE", "--adapter": "claude", "--instruction": "$QS/manuals/instruction.md",
		"--runbook": "$QS/manuals/runbook.md", "--model": "$MODEL", "--effort": "low", "--concurrency": "1", "--timeout": "5m", "--plane": "$PLANE", "--ca": "$CA"}))
	if args, err := fenceArgs(md, "remote-role-add"); err != nil || len(args) < 4 || args[3] != "quickstart-remote" {
		add(fmt.Errorf("second machine: remote-role-add ID = %q (%v)", args, err))
	}
	add(wantFlags(md, "remote-dispatch", map[string]string{"--role-id": "quickstart-remote", "--goal": qsGoal, "--acceptance": qsAcceptance, "--workspace": "quickstart", "--base": "main",
		"--workspace-instance": "$INSTANCE", "--plane": "$PLANE", "--ca": "$CA"}))
	if args, err := fenceArgs(md, "remote-dispatch"); err != nil || slices.Contains(args, "--role-name") {
		add(fmt.Errorf("second machine: remote-dispatch = %q (%v)", args, err))
	}
	if args, err := fenceArgs(md, "remote-pull"); err != nil || !slices.Contains(args, "$QS/delivery-remote") || !slices.Contains(args, "$REMOTE_TASK") {
		add(fmt.Errorf("second machine: remote-pull = %q (%v)", args, err))
	}
	if man, err := fenceText(md, "remote-manuals"); err != nil || !strings.Contains(man, strings.TrimSuffix(qsInstruction, "\n")) || !strings.Contains(man, strings.TrimSuffix(qsRunbook, "\n")) {
		add(fmt.Errorf("second machine: remote-manuals do not repeat the first-path contents (%v)", err))
	}
	return errors.Join(errs...)
}

// tryStartPlane is planeCLI.start for a port another process may have taken:
// a child that exits before listening is an error the caller may retry.
func tryStartPlane(t *testing.T, p *planeCLI, args ...string) (*planeProc, error) {
	t.Helper()
	pp := &planeProc{logs: &logLines{ready: make(chan string, 1)}, done: make(chan struct{})}
	pp.cmd = exec.Command(p.bin, append([]string{"plane", "run"}, args...)...)
	pp.cmd.Dir, pp.cmd.Env = p.cwd, p.env
	pp.cmd.Stdout, pp.cmd.Stderr = &pp.stdout, pp.logs
	pp.cmd.WaitDelay = 5 * time.Second
	if err := pp.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { pp.err = pp.cmd.Wait(); close(pp.done) }()
	t.Cleanup(func() {
		select {
		case <-pp.done:
			return
		default:
		}
		pp.cmd.Process.Kill()
		select {
		case <-pp.done:
		case <-time.After(planeChildTimeout):
			t.Errorf("plane run %d was not reaped", pp.cmd.Process.Pid)
		}
	})
	select {
	case pp.addr = <-pp.logs.ready:
		return pp, nil
	case <-pp.done:
		return nil, fmt.Errorf("plane run exited before listening: %v\n%s", pp.err, pp.logs.String())
	case <-time.After(planeReadyTimeout):
		t.Fatalf("plane run not ready after %v:\n%s", planeReadyTimeout, pp.logs.String())
	}
	return nil, nil
}

func certSANs(t *testing.T, path string) (dns, ips []string) {
	t.Helper()
	block, _ := pem.Decode(nodeFile(t, path))
	if block == nil {
		t.Fatalf("%s holds no PEM block", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	return cert.DNSNames, ips
}

// freeLoopbackPort asks the kernel for a port other than not.
func freeLoopbackPort(t *testing.T, not string) string {
	t.Helper()
	for range 20 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		if addr != not {
			return addr
		}
	}
	t.Fatal("no free loopback port")
	return ""
}

// TestQuickstartSecondMachine (FP-5): the ordered topology, every Callsheet
// leaf and flag against real help, and the stopped-plane bind change. The
// fixture stays on 127.0.0.1: it is no evidence of LAN or VPN routing.
func TestQuickstartSecondMachine(t *testing.T) {
	md := string(repoFile(t, quickstartDoc))
	t.Run("document", func(t *testing.T) {
		if err := checkSecondMachineDoc(md); err != nil {
			t.Fatalf("shipped quickstart: %v", err)
		}
		remoteEnvPlane := "PLANE=\"https://$PLANE_IP:8443\"\n   CA=\"$QS/plane-ca.crt\""
		runFPMutations(t, md, checkSecondMachineDoc, []readmeMutation{
			{"remote-url-loopback", replaceOnce(remoteEnvPlane, "PLANE=\"https://127.0.0.1:8443\"\n   CA=\"$QS/plane-ca.crt\""), "remote-env lacks"},
			{"local-url-loopback", replaceOnce("read -r PLANE_IP\n   PLANE=\"https://$PLANE_IP:8443\"\n   ```\n\n   Re-enroll", "read -r PLANE_IP\n   PLANE=\"https://127.0.0.1:8443\"\n   ```\n\n   Re-enroll"), "remote-url does not use the private address"},
			{"copy-ca-key", replaceOnce("Copy only `$QS/plane/pki/ca.crt`", "Copy `$QS/plane/pki/ca.key`"), "lacks"},
			{"omit-original-san", replaceOnce("--san 127.0.0.1 --san \"$PLANE_IP\"", "--san \"$PLANE_IP\""), "remote-cert ="},
			{"reuse-local-node-state", replaceOnce("QS=\"$HOME/callsheet-quickstart-remote\"", "QS=\"$HOME/callsheet-quickstart\""), "remote-env lacks"},
			{"reuse-local-node-id", replaceOnce("--node \"$REMOTE_NODE\"", "--node \"$NODE\""), "--node"},
			{"dispatch-local-role", replaceOnce("--role-id quickstart-remote", "--role-id quickstart-local"), "--role-id"},
			{"dispatch-by-name", replaceOnce("--role-id quickstart-remote", "--role-name quickstart"), "--role-id"},
			{"explicit-bind-update", replaceOnce("callsheet plane run --state-dir \"$QS/plane\"\n   ```\n\n5.", "callsheet plane run --state-dir \"$QS/plane\" --bind \"$PLANE_IP:8443\"\n   ```\n\n5."), "remote-plane-run ="},
			{"wrong-order", func(t *testing.T, md string) string {
				t.Helper()
				a, _ := fenceBlock(t, md, "remote-cert")
				b, _ := fenceBlock(t, md, "remote-plane-run")
				md = strings.Replace(md, a, "@@A@@", 1)
				md = strings.Replace(md, b, a, 1)
				return strings.Replace(md, "@@A@@", b, 1)
			}, "out of order"},
			{"local-manual-paths", replaceOnce("--node \"$REMOTE_NODE\" --adapter claude \\\n     --instruction \"$QS/manuals/instruction.md\"", "--node \"$REMOTE_NODE\" --adapter claude \\\n     --instruction \"/srv/manuals/instruction.md\""), "--instruction"},
			{"no-reenroll", func(t *testing.T, md string) string {
				t.Helper()
				b, _ := fenceBlock(t, md, "local-reenroll")
				return strings.Replace(md, b, "", 1)
			}, "lacks step \"quickstart: local-reenroll\""},
			{"delete-state", replaceOnce("Do not delete or reinitialize state", "Delete the state"), "lacks"},
			{"same-delivery", replaceOnce("\"$REMOTE_TASK\" \"$QS/delivery-remote\"", "\"$REMOTE_TASK\" \"$QS/delivery\""), "remote-pull ="},
			{"changed-remote-manual", replaceOnce("   printf 'Work only in the task checkout.", "   printf 'Work anywhere."), "remote-manuals do not repeat"},
			{"no-config-example", replaceOnce("\"bind\": \"192.168.1.10:8443\"", "\"bind\": \"10.0.0.1:8443\""), "no illustrative before and after"},
			{"no-canonical-link", replaceOnce("(deploy.md#change-the-plane-address)", "(deploy.md)"), "no link to the canonical address-change procedure"},
		})
	})
	t.Run("leaves-and-flags", func(t *testing.T) {
		if err := checkCommandContract(t, md); err != nil {
			t.Fatal(err)
		}
		// Every remote fence reaches a real leaf through the same checker.
		bad := replaceOnce("callsheet ws pull --plane \"$PLANE\" --ca \"$CA\" \"$REMOTE_TASK\"", "callsheet ws pull --remote --plane \"$PLANE\" --ca \"$CA\" \"$REMOTE_TASK\"")(t, md)
		if err := checkCommandContract(t, bad); err == nil || !strings.Contains(err.Error(), "fence remote-pull line") || !strings.Contains(err.Error(), "unknown flag --remote") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bind-change", func(t *testing.T) {
		p := newNodeCLI(t)
		root := filepath.Join(t.TempDir(), "plane")
		const oldBind = "127.0.0.1:0"
		if r := p.run(t, "plane", "init", "--state-dir", root, "--bind", oldBind, "--san", "127.0.0.1"); r.code != 0 {
			t.Fatalf("plane init = %+v", r)
		}
		ca := filepath.Join(root, "pki", "ca.crt")
		caBefore := nodeFile(t, ca)
		first := p.start(t, "--state-dir", root)
		if code := first.stop(t, os.Interrupt); code != 130 {
			t.Fatalf("stop = %d", code)
		}
		config := filepath.Join(root, "config.json")
		var doc map[string]any
		before := nodeFile(t, config)
		if err := json.Unmarshal(before, &doc); err != nil || doc["bind"] != oldBind || doc["schema_version"] != float64(1) || len(doc) != 2 {
			t.Fatalf("config = %s, %v", before, err)
		}
		st, err := os.Stat(config)
		if err != nil {
			t.Fatal(err)
		}
		mode := st.Mode()

		// Edit only the bind, on a different port.
		var newBind string
		editBind := func() {
			t.Helper()
			newBind = freeLoopbackPort(t, first.addr)
			edited := bytes.Replace(before, []byte(`"`+oldBind+`"`), []byte(`"`+newBind+`"`), 1)
			if bytes.Equal(edited, before) {
				t.Fatalf("config holds no bind to edit: %s", before)
			}
			if err := os.WriteFile(config, edited, mode.Perm()); err != nil {
				t.Fatal(err)
			}
		}
		editBind()
		r := p.run(t, "plane", "cert", "reissue", "--state-dir", root, "--san", "127.0.0.1", "--san", "localhost")
		if r.code != 0 {
			t.Fatalf("reissue = %+v", r)
		}
		if !bytes.Equal(nodeFile(t, ca), caBefore) {
			t.Fatal("reissue changed the CA")
		}
		dns, ips := certSANs(t, filepath.Join(root, "pki", "server.crt"))
		if !slices.Contains(dns, "localhost") || !slices.Contains(ips, "127.0.0.1") {
			t.Fatalf("SANs = %q, %q", dns, ips)
		}

		// A port taken in the meantime picks another one.
		var again *planeProc
		for attempt := 0; again == nil; attempt++ {
			if attempt > 0 {
				editBind()
			}
			// The old explicit bind is refused while the state holds the new one.
			refused := p.run(t, "plane", "run", "--state-dir", root, "--bind", oldBind)
			if refused.code == 0 || !strings.Contains(refused.stderr, "stop the plane and edit") {
				t.Fatalf("old --bind = %+v", refused)
			}
			pp, err := tryStartPlane(t, p, "--state-dir", root)
			switch {
			case err == nil:
				again = pp
			case attempt < 5 && strings.Contains(err.Error(), "address already in use"):
				t.Logf("retrying: %v", err)
			default:
				t.Fatal(err)
			}
		}
		if again.addr != newBind {
			t.Fatalf("listening on %s, want %s", again.addr, newBind)
		}
		if got := nodeFile(t, ca); !bytes.Equal(got, caBefore) {
			t.Fatal("restart changed the CA")
		}
		if r := p.run(t, "node", "ls", "--plane", "https://"+newBind, "--ca", ca); r.code != 0 {
			t.Fatalf("new port with the original CA = %+v", r)
		}
		if r := p.run(t, "node", "ls", "--plane", "https://"+first.addr, "--ca", ca); r.code == 0 {
			t.Fatalf("the old address still answers: %+v", r)
		}
		if code := again.stop(t, os.Interrupt); code != 130 {
			t.Fatalf("final stop = %d", code)
		}
	})
}

// ---- FP-6: navigation.

// checkQuickstartNavigation is FP-6's validator over the three documents.
func checkQuickstartNavigation(readme, coordinator, quickstart string, look repoLookup) error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	summary, err := sectionOf(readme, "Quickstart summary")
	if err != nil {
		return err
	}
	docs, err := sectionOf(readme, "Documentation")
	if err != nil {
		return err
	}
	if !slices.Contains(markdownLinks(summary), "docs/quickstart.md#install-callsheet") {
		add(errors.New("README: step 1 does not link the install section"))
	}
	if !slices.Contains(markdownLinks(summary), "docs/quickstart.md") {
		add(errors.New("README: the summary does not link the complete recipe"))
	}
	if !slices.Contains(markdownLinks(docs), "docs/quickstart.md") {
		add(errors.New("README: the documentation map does not link the shipped quickstart"))
	}
	if strings.Contains(docs, "`docs/quickstart.md`") {
		add(errors.New("README: quickstart is still a coming-in-v1 entry"))
	}
	for _, line := range strings.Split(docs, "\n") {
		if strings.Contains(line, "quickstart") && strings.Contains(line, "coming in v1") {
			add(errors.New("README: quickstart is still a coming-in-v1 entry"))
		}
	}
	add(needAll("README", stripInlineLinks(summary),
		"Install Callsheet using the quickstart: select a published release and verify its checksum. If no release is published yet, use its source-build fallback."))
	const clause = "[quickstart](quickstart.md#install-callsheet): select a published release and verify its checksum, or, if no release is published yet, use its source-build fallback."
	if !strings.Contains(coordinator, clause) {
		add(errors.New("coordinator: no conditional quickstart install clause"))
	}
	for _, s := range []string{"Make it executable and use its absolute path.", "Install the vendor CLI with its vendor's own instructions for Linux or macOS"} {
		if !strings.Contains(coordinator, s) {
			add(fmt.Errorf("coordinator: lacks %q", s))
		}
	}
	if strings.Contains(coordinator, "prebuilt release binaries are coming in v1") {
		add(errors.New("coordinator: restored the interim coming-in-v1 clause"))
	}
	lowerRead := strings.ToLower(readme + coordinator)
	for _, claim := range []string{"binaries are available", "download the latest", "releases/latest", "v1 is released", "prebuilt release binaries exist"} {
		if strings.Contains(lowerRead, claim) {
			add(fmt.Errorf("navigation: unconditional binaries-exist claim %q", claim))
		}
	}
	add(checkReadmeLinks(readme, look))
	add(checkSourceLinks(quickstartDoc, quickstart, look))
	if !slices.ContainsFunc(markdownLinks(coordinator), func(l string) bool { return l == "quickstart.md#install-callsheet" }) {
		add(errors.New("coordinator: no link to the install section"))
	}
	add(checkSourceLinks("docs/coordinator.md", "[install](quickstart.md#install-callsheet)", look))
	for _, l := range markdownLinks(quickstart) {
		if strings.HasPrefix(l, "http") && !strings.HasPrefix(l, "https://github.com/wedevwork/callsheet") {
			add(fmt.Errorf("quickstart: unexpected external link %s", l))
		}
		if strings.Contains(l, "design/") {
			add(fmt.Errorf("quickstart: link into the design tree %s", l))
		}
	}
	add(checkPrivateText("README", readme))
	add(checkPrivateText("quickstart", quickstart))
	return errors.Join(errs...)
}

// TestQuickstartNavigation (FP-6): README and coordinator installation
// guidance agree with the shipped quickstart and stay true before a release.
func TestQuickstartNavigation(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	readme, coordinator, quickstart := string(repoFile(t, "README.md")), string(repoFile(t, "docs/coordinator.md")), string(repoFile(t, quickstartDoc))
	look := checkoutLookup(root)
	check := func(readme, coordinator, quickstart string) error {
		return checkQuickstartNavigation(readme, coordinator, quickstart, func(rel string) ([]byte, bool) {
			if rel == quickstartDoc {
				return []byte(quickstart), true
			}
			return look(rel)
		})
	}
	if err := check(readme, coordinator, quickstart); err != nil {
		t.Fatalf("shipped documents: %v", err)
	}
	if err := errors.Join(checkEntryPoint(readme), checkSupportAndSecurity(readme), checkDocMap(readme, look)); err != nil {
		t.Fatalf("README contracts: %v", err)
	}
	if n := len(strings.Fields(readme)); n > readmeWordCap {
		t.Fatalf("README: %d words, cap %d", n, readmeWordCap)
	}
	for _, c := range []struct {
		name  string
		apply func() error
		want  string
	}{
		{"missing-shipped-map-link", func() error {
			return check(replaceOnce("- [Quickstart](docs/quickstart.md): install, first dispatch on one machine, then two.\n", "")(t, readme), coordinator, quickstart)
		}, "does not link the shipped quickstart"},
		{"restored-coming-in-v1", func() error {
			return check(replaceOnce("- [Quickstart](docs/quickstart.md): install, first dispatch on one machine, then two.", "- `docs/quickstart.md`: first dispatch on one machine, then two; coming in v1.")(t, readme), coordinator, quickstart)
		}, "quickstart is still a coming-in-v1 entry"},
		{"step-without-install-link", func() error {
			return check(replaceOnce("(docs/quickstart.md#install-callsheet)", "(docs/quickstart.md)")(t, readme), coordinator, quickstart)
		}, "step 1 does not link the install section"},
		{"summary-without-recipe", func() error {
			return check(replaceOnce("the [quickstart](docs/quickstart.md) is the complete recipe and ", "")(t, readme), coordinator, quickstart)
		}, "the summary does not link the complete recipe"},
		{"unconditional-binaries", func() error {
			return check(replaceOnce("If no release is published yet, use its source-build fallback.", "Prebuilt binaries are available.")(t, readme), coordinator, quickstart)
		}, "unconditional binaries-exist claim"},
		{"coordinator-unconditional", func() error {
			return check(readme, replaceOnce(", or, if no release is published yet, use its source-build fallback.", ".")(t, coordinator), quickstart)
		}, "no conditional quickstart install clause"},
		{"coordinator-interim-restored", func() error {
			return check(readme, coordinator+"\nprebuilt release binaries are coming in v1\n", quickstart)
		}, "restored the interim coming-in-v1 clause"},
		{"coordinator-lost-absolute-path", func() error {
			return check(readme, replaceOnce("Make it executable and use its absolute path.", "Run it.")(t, coordinator), quickstart)
		}, "lacks \"Make it executable"},
		{"broken-fragment", func() error {
			return check(readme, coordinator, replaceOnce("## Install Callsheet", "## Installing")(t, quickstart))
		}, "fragment #install-callsheet"},
		{"broken-quickstart-fragment", func() error {
			return check(readme, coordinator, replaceOnce("(real-adapters.md#enabling-a-worker)", "(real-adapters.md#enabling-workers)")(t, quickstart))
		}, "fragment #enabling-workers"},
		{"broken-quickstart-file", func() error {
			return check(readme, coordinator, replaceOnce("(examples/dev-flow/README.md)", "(examples/dev-flow/README-missing.md)")(t, quickstart))
		}, "does not exist"},
		{"private-path-readme", func() error {
			return check(replaceOnce("## Security\n", "## Security\n\nSee /home/someone.\n")(t, readme), coordinator, quickstart)
		}, "README: private path sentinel"},
		{"private-path-quickstart", func() error {
			return check(readme, coordinator, replaceOnce("## Next steps\n", "## Next steps\n\nSee /Users/someone.\n")(t, quickstart))
		}, "quickstart: private path sentinel"},
		{"design-link", func() error {
			return check(readme, coordinator, replaceOnce("## Next steps\n", "## Next steps\n\n[d](../design/x.md)\n")(t, quickstart))
		}, "design"},
		{"foreign-link", func() error {
			return check(readme, coordinator, replaceOnce("## Next steps\n", "## Next steps\n\n[d](https://example.invalid/x)\n")(t, quickstart))
		}, "unexpected external link"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.apply()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			t.Logf("rejected: %v", firstLine(err))
		})
	}
	// The headings are the stable link targets.
	for _, h := range qsHeadings {
		if !strings.Contains(quickstart, "\n## "+h+"\n") {
			t.Errorf("quickstart lacks heading %q", h)
		}
	}
}

// ---- FP-7: the command contract.

// TestQuickstartCommandContract (FP-7): the tokenizer and fence parser's unit
// cases, the shipped fence inventory through the real tree and leaf help, and
// each drift mutation rejected.
func TestQuickstartCommandContract(t *testing.T) {
	md := string(repoFile(t, quickstartDoc))
	t.Run("tokenizer", func(t *testing.T) {
		vars := map[string]string{"QS": "/tmp/q s", "X": "x"}
		for _, c := range []struct {
			name, body string
			want       [][]string
		}{
			{"plain", "callsheet plane run", [][]string{{"callsheet", "plane", "run"}}},
			{"quoted-suffix", `a "$QS/plane" ${QS}/b`, [][]string{{"a", "/tmp/q s/plane", "/tmp/q s/b"}}},
			{"adjacent", `a"b"'c'$X`, [][]string{{"abcx"}}},
			{"continuation", "a b \\\n  c", [][]string{{"a", "b", "c"}}},
			{"two-lines", "a\nb", [][]string{{"a"}, {"b"}}},
			{"multiline-quote", "a \"b\nc\" 'd\ne'", [][]string{{"a", "b\nc", "d\ne"}}},
			{"escaped-quote", `a "b\"c\$d"`, [][]string{{"a", `b"c$d`}}},
			{"quoted-operators", `a "|;&<>()*?" '$(x)'`, [][]string{{"a", "|;&<>()*?", "$(x)"}}},
		} {
			cmds, err := tokenizeShell("t", c.body, 1, false)
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
				continue
			}
			var got [][]string
			for _, cmd := range cmds {
				var line []string
				for _, w := range cmd.words {
					v, err := w.expand(vars)
					if err != nil {
						t.Errorf("%s: %v", c.name, err)
					}
					line = append(line, v)
				}
				got = append(got, line)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("%s = %q, want %q", c.name, got, c.want)
			}
		}
		for _, c := range []struct{ name, body, want string }{
			{"pipe", "a | b", "pipe"},
			{"semicolon", "a; b", "semicolon chain"},
			{"redirect", "a > b", "redirect"},
			{"input", "a < b", "<"},
			{"substitution", "a $(b)", "command substitution"},
			{"quoted-substitution", `a "$(b)"`, "command substitution"},
			{"backtick", "a `b`", "command substitution"},
			{"quoted-backtick", "a \"`b`\"", "command substitution"},
			{"glob", "a *", "*"},
			{"background", "a &", "&"},
			{"subshell", "(a)", "("},
			{"function", "f() { a; }", "("},
			{"comment", "a # b", "comment"},
			{"unterminated-single", "a 'b", "unterminated '"},
			{"unterminated-double", `a "b`, `unterminated "`},
			{"backslash", `a \b`, `\`},
			{"quoted-backslash", `a "\b"`, `\ in double quotes`},
			{"special-variable", "a $1", "$"},
			{"bad-brace", "a ${X:-y}", "${"},
		} {
			_, err := tokenizeShell("fence-id", "ok\n"+c.body, 7, false)
			if err == nil || !strings.Contains(err.Error(), "fence fence-id line 8") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want fence and line with %q", c.name, err, c.want)
			}
		}
		// A flag-looking token in quotes is a value; an unknown variable fails.
		cmds, err := tokenizeShell("t", `callsheet x --flag "--not-a-flag" $NOPE`, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		if !cmds[0].words[3].quoted || cmds[0].words[2].quoted {
			t.Error("quoted flag-looking value not marked")
		}
		if _, err := cmds[0].words[4].expand(vars); err == nil || !strings.Contains(err.Error(), "unknown variable $NOPE") {
			t.Errorf("unknown variable: %v", err)
		}
		// One redirect ends a printf; a second one or none is refused.
		if cmds, err := tokenizeShell("t", `printf 'x\n' > "$QS/f"`, 1, true); err != nil || cmds[0].redirect == nil {
			t.Errorf("redirect: %v, %v", cmds, err)
		}
		for _, body := range []string{"a > b > c", "a >", "a >\nb"} {
			if _, err := tokenizeShell("t", body, 1, true); err == nil {
				t.Errorf("%q accepted", body)
			}
		}
	})
	t.Run("fences", func(t *testing.T) {
		for _, c := range []struct{ name, md, want string }{
			{"unmarked-shell", "```sh\ntrue\n```\n", "unmarked sh fence at line 1"},
			{"unmarked-bash", "```bash\ntrue\n```\n", "unmarked bash fence"},
			{"unlabelled-callsheet", "```\ncallsheet version\n```\n", "unlabelled fence at line 1 holds a Callsheet command"},
			{"text-callsheet", "```text\ncallsheet version\n```\n", "holds a Callsheet command"},
			{"marked-non-shell", "<!-- quickstart: a -->\n```text\nx\n```\n", "fence a at line 2 is not a shell fence"},
			{"duplicate-id", "<!-- quickstart: a -->\n```sh\nx\n```\n<!-- quickstart: a -->\n```sh\nx\n```\n", "duplicate fence ID a"},
			{"marker-no-fence", "<!-- quickstart: a -->\ntext\n```sh\nx\n```\n", "marker a at line 1 is not immediately before a fence"},
			{"marker-eof", "<!-- quickstart: a -->\n", "marker a at line 1"},
			{"marker-marker", "<!-- quickstart: a -->\n<!-- quickstart: b -->\n```sh\nx\n```\n", "marker a at line 1"},
			{"unclosed", "<!-- quickstart: a -->\n```sh\nx\n", "not closed"},
		} {
			if _, err := parseQuickstartFences(c.md); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
			}
		}
		fences, err := parseQuickstartFences("1. item\n\n   <!-- quickstart: a -->\n   ```sh\n   callsheet version\n   ```\n\n```json\n{}\n```\n")
		if err != nil || len(fences) != 2 || fences[0].body != "callsheet version" || fences[0].id != "a" {
			t.Errorf("indented fence = %+v, %v", fences, err)
		}
	})
	t.Run("setup-operations", func(t *testing.T) {
		for _, body := range []string{`rm -rf "$QS"`, `curl https://x`, `export OTHER=1`, `mkdir a b`, `read X`, `cat a > b`, `$OTHER --version`, `printf 'x'`, "PATH=a b"} {
			if _, err := parseFenceCommands(qsFence{id: "f", lang: "sh", body: body, line: 1}); err == nil || !strings.Contains(err.Error(), "fence f line") {
				t.Errorf("%q: err = %v", body, err)
			}
		}
		for _, body := range []string{`mkdir "$QS"`, `QS="$HOME/x"`, `PLANE=https://h`, `read -r X`, `"$WORKER" --version`, `export PATH="$HOME/.local/bin:$PATH"`, `printf 'x\n' > "$QS/f"`, `cat "$QS/f"`} {
			if _, err := parseFenceCommands(qsFence{id: "f", lang: "sh", body: body, line: 1}); err != nil {
				t.Errorf("%q: %v", body, err)
			}
		}
		// Bash install logic is opaque except its Callsheet lines.
		p, err := parseFenceCommands(qsFence{id: "f", lang: "bash", body: "case x in\n  *) exit 1 ;;\nesac\ncallsheet version", line: 1})
		if err != nil || len(p.callsheet) != 1 {
			t.Errorf("bash fence = %+v, %v", p, err)
		}
		if _, err := parseFenceCommands(qsFence{id: "f", lang: "bash", body: "callsheet version | cat", line: 1}); err == nil {
			t.Error("a pipe on a Callsheet line accepted in a bash fence")
		}
		if _, err := parseFenceCommands(qsFence{id: "f", lang: "sh", body: "callsheet version > out", line: 1}); err == nil {
			t.Error("a redirect on a Callsheet line accepted")
		}
		for _, c := range [][]string{{`printf '%d\n' 'x' > "f"`}, {`printf 'a%b' > "f"`}} {
			cmds, err := tokenizeShell("f", c[0], 1, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := printfOutput(cmds[0]); err == nil {
				t.Errorf("%q rendered", c[0])
			}
		}
	})
	t.Run("malformed-arguments", func(t *testing.T) {
		// Each returns a diagnostic; none may panic on a missing argument.
		for _, c := range []struct{ name, body, want string }{
			{"bare-read", "read", "read takes -r NAME"},
			{"read-flag-only", "read -r", "read takes -r NAME"},
			{"read-no-flag", "read NAME", "read takes -r NAME"},
			{"bare-export", "export", "export must prepend the install PATH"},
			{"bare-mkdir", "mkdir", "mkdir takes one directory"},
			{"bare-printf", "printf", "printf without a single redirected file"},
			{"printf-redirect-only", `printf > "$QS/f"`, "printf without a single redirected file"},
			{"bare-cat", "cat", ""},
			{"redirect-only", `> "$X"`, "redirect without a command"},
			{"redirect-only-before-command", "> \"$X\"\nmkdir \"$QS\"", "redirect without a command"},
			{"bare-unknown", "rm", "unrecognized setup operation: rm"},
			{"empty-word", `""`, "unrecognized setup operation"},
			{"worker-alone", `"$WORKER"`, "a variable command"},
		} {
			t.Run(c.name, func(t *testing.T) {
				_, err := parseFenceCommands(qsFence{id: "f", lang: "sh", body: c.body, line: 1})
				if c.want == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "fence f line 2") || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want fence, line and %q", err, c.want)
				}
			})
		}
		printfCases := []struct{ name, body string }{{"printf-no-format", "printf"}, {"printf-too-many", "printf a b c"}}
		for _, c := range printfCases {
			t.Run(c.name, func(t *testing.T) {
				cmds, err := tokenizeShell("f", c.body, 1, true)
				if err != nil {
					t.Fatal(err)
				}
				if got, err := printfOutput(cmds[0]); err == nil || !strings.Contains(err.Error(), "printf takes") {
					t.Fatalf("printfOutput = %q, %v", got, err)
				}
			})
		}
		for _, flag := range []string{"--claude-adapter", "--adapter", "--bind", "--goal"} {
			t.Run("trailing"+flag, func(t *testing.T) {
				if err := translateFixtureArgs([]string{"sidecar", "run", flag}, "/fake"); err == nil || !strings.Contains(err.Error(), flag+" has no value") {
					t.Fatalf("err = %v", err)
				}
			})
		}
		t.Run("short-command-path", func(t *testing.T) {
			if _, err := argsAfterLeaf([]string{"plane"}, "plane", "run"); err == nil || !strings.Contains(err.Error(), `want the command "plane run"`) {
				t.Fatalf("err = %v", err)
			}
			if _, err := argsAfterLeaf([]string{"sidecar", "run", "--x"}, "plane", "run"); err == nil {
				t.Fatal("another command accepted")
			}
			if got, err := argsAfterLeaf([]string{"plane", "run", "--x"}, "plane", "run"); err != nil || !slices.Equal(got, []string{"--x"}) {
				t.Fatalf("args = %q, %v", got, err)
			}
		})
	})
	t.Run("leaves", func(t *testing.T) {
		root := cli.NewTree(runtime.GOOS)
		parse := func(s string) qsCommand {
			cmds, err := tokenizeShell("t", s, 1, false)
			if err != nil {
				t.Fatal(err)
			}
			return cmds[0]
		}
		if leaf, rest, err := leafOf(root, parse(`callsheet plane cert reissue --san "$X"`)); err != nil || leaf != "plane cert reissue" || len(rest) != 2 {
			t.Errorf("leaf = %q, %v, %v", leaf, rest, err)
		}
		for _, s := range []string{"callsheet nope", "callsheet plane", "callsheet plane nope", `callsheet "$X"`} {
			if _, _, err := leafOf(root, parse(s)); err == nil {
				t.Errorf("%q resolved", s)
			}
		}
	})
	t.Run("shipped", func(t *testing.T) {
		if err := checkCommandContract(t, md); err != nil {
			t.Fatal(err)
		}
	})
	check := func(md string) error { return checkCommandContract(t, md) }
	t.Run("mutations", func(t *testing.T) {
		runFPMutations(t, md, check, []readmeMutation{
			{"unknown-leaf", replaceOnce("callsheet node ls --plane \"$PLANE\" --ca \"$CA\"\n```\n\nRegister", "callsheet node list --plane \"$PLANE\" --ca \"$CA\"\n```\n\nRegister"), "unknown command \"list\""},
			{"unknown-flag", replaceOnce("callsheet task wait \"$TASK\" --until-done", "callsheet task wait \"$TASK\" --until-finished"), "unknown flag --until-finished"},
			{"flag-value-is-not-flag", replaceOnce("--acceptance \"hello.txt has the requested", "--bogus \"hello.txt has the requested"), "unknown flag --bogus"},
			{"unmarked-extra-command", appendToSection("Next steps", "```sh\ncallsheet task ls\n```"), "unmarked sh fence"},
			{"unlabelled-extra-command", appendToSection("Next steps", "```\ncallsheet task ls\n```"), "unlabelled fence"},
			{"removed-label", replaceOnce("<!-- quickstart: diagnostics -->\n", ""), "unmarked sh fence"},
			{"marked-extra-command", appendToSection("Next steps", "<!-- quickstart: extra -->\n```sh\ncallsheet task ls\n```"), "fence extra is not in the pinned inventory"},
			{"extra-command-in-fence", replaceOnce("callsheet task logs \"$TASK\" --plane \"$PLANE\" --ca \"$CA\"\n", "callsheet task logs \"$TASK\" --plane \"$PLANE\" --ca \"$CA\"\ncallsheet task ls --plane \"$PLANE\" --ca \"$CA\"\n"), "inventory: fence diagnostics runs"},
			{"missing-required-command", func(t *testing.T, md string) string {
				t.Helper()
				b, _ := fenceBlock(t, md, "pull")
				return strings.Replace(md, b, "", 1)
			}, "required fence pull is missing"},
			{"swapped-leaf", replaceOnce("callsheet ws status --plane \"$PLANE\" --ca \"$CA\" \"$TASK\"", "callsheet ws show --plane \"$PLANE\" --ca \"$CA\" \"$TASK\""), "inventory: fence workspace-status runs"},
			{"unknown-variable", replaceOnce("callsheet task show \"$TASK\" --lines 0", "callsheet task show \"$OTHER\" --lines 0"), "unknown variable $OTHER"},
			{"pipe", replaceOnce("callsheet role ls --plane \"$PLANE\" --ca \"$CA\"\n```\n\nExpect", "callsheet role ls --plane \"$PLANE\" --ca \"$CA\" | cat\n```\n\nExpect"), "pipe"},
			{"substitution", replaceOnce("--node \"$NODE\" --adapter claude", "--node \"$(cat node)\" --adapter claude"), "command substitution"},
			{"redirect", replaceOnce("callsheet node ls --plane \"$PLANE\" --ca \"$CA\"\n```\n\nRegister", "callsheet node ls --plane \"$PLANE\" --ca \"$CA\" > out\n```\n\nRegister"), "redirect"},
			{"chain", replaceOnce("callsheet plane run --state-dir \"$QS/plane\"\n```\n\nIn terminal B", "callsheet plane run --state-dir \"$QS/plane\"; true\n```\n\nIn terminal B"), "semicolon chain"},
			{"eval", replaceOnce("read -r INSTANCE\n", "eval \"$X\"\n"), "unrecognized setup operation"},
			{"duplicate-id", replaceOnce("<!-- quickstart: read-task -->", "<!-- quickstart: read-instance -->"), "duplicate fence ID read-instance"},
			{"wrong-language", replaceOnce("<!-- quickstart: diagnostics -->\n```sh", "<!-- quickstart: diagnostics -->\n```bash"), "fence diagnostics is bash, want sh"},
		})
	})
}
