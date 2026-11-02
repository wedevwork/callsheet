package function

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Slice 12-4 function tests (design 12-4-user-guide): one top-level TestGuide*
// per FP over the shipped user guide, the example and the finalized catalog,
// plus TestGuideCommandContract across their commands and tool arguments. Each
// validates the shipped text with a pure validator, then proves that validator
// rejects deliberate in-memory mutations. Markdown is treated as data: nothing
// here runs a fence through a shell, contacts a network or starts a vendor CLI.
// These are offline contracts; they declare no vendor qualification and no M5.

const (
	guideDir        = "docs/guide/"
	guideGoal       = "Reply with one sentence describing your role."
	guideAcceptance = "The final message is one sentence."
	guideExampleDoc = "docs/examples/dev-flow/README.md"
	guidePlaybook   = "docs/examples/dev-flow/coordinator-playbook.md"
	guideCatalogMD  = "docs/support-catalog.md"
	guideAdapters   = "docs/real-adapters.md"
	guideEnvTest    = "internal/reale2e/environment_test.go"
	// guideGrokAssumption is DW2's exact attributed sentence.
	guideGrokAssumption = "ASSUMPTION (unverified): The owner reported during 12a that Grok headless could not run compound shell commands; applicability to other versions and configurations is unverified."
	guideGrokProbe      = "tests simple versus compound commands with the actual authenticated version and records the argv, output and actions"
	guideRedaction      = "the recorded checkout parent"
)

var (
	guideMarker       = regexp.MustCompile(`^<!-- guide: ([a-z0-9-]+) -->$`)
	guideJSONMarker   = regexp.MustCompile(`^<!-- guide-json: ([a-z0-9-]+) -->$`)
	devFlowMarker     = regexp.MustCompile(`^<!-- dev-flow: ([a-z0-9-]+) -->$`)
	devFlowJSONMarker = regexp.MustCompile(`^<!-- dev-flow-json: ([a-z0-9-]+) -->$`)
	guideHelpPointer  = regexp.MustCompile("`(callsheet [a-z ]+ --help)`")
	guideAnchorTag    = regexp.MustCompile(`<a id="([^"]+)"></a>`)
)

// guideChapter is one chapter's exact frame: its H2 list and next reading.
type guideChapter struct {
	file string
	h2   []string
	next string
}

// guideChapters are the eight chapters in reading order.
var guideChapters = []guideChapter{
	{"concepts.md", []string{"Components", "Roles and tasks", "Files and results", "Trust and limits"}, "roles-and-manuals.md"},
	{"roles-and-manuals.md", []string{"Prepare manuals", "Register a role", "Check readiness", "Change a role"}, "dispatch-and-wait.md"},
	{"dispatch-and-wait.md", []string{"Dispatch from the CLI", "Dispatch through MCP", "Wait and inspect", "Recover safely"}, "workspaces.md"},
	{"workspaces.md", []string{"Seed a folder", "Chain results", "Inspect and deliver", "Conflicts and limits"}, "coordinator-setup.md"},
	{"coordinator-setup.md", []string{"Before configuring", "Claude Code", "OpenAI Codex", "Grok Build", "Cursor Agent", "Verify the connection"}, "worker-setup.md"},
	{"worker-setup.md", []string{"Before enabling a worker", "Claude Code", "OpenAI Codex", "Grok Build", "Cursor Agent", "Verify readiness", "Vendor changes"}, "example.md"},
	{"example.md", []string{"Follow the example", "What the evidence shows", "M5 walkthrough checklist"}, "troubleshooting.md"},
	{"troubleshooting.md", []string{"Connection and readiness", "Authentication and permission failures", "Task and wait results", "Workspace delivery", "Vendor changes"}, "../deploy.md"},
}

func guideChapterOf(file string) guideChapter {
	return guideChapters[slices.IndexFunc(guideChapters, func(c guideChapter) bool { return c.file == file })]
}

// guideVariables are each fence-bearing page's stated operator variables.
var guideVariables = map[string][]string{
	"roles-and-manuals.md": {"ROLE_ID", "ROLE_NAME", "NODE", "INSTRUCTION", "RUNBOOK", "MODEL", "EFFORT", "PLANE", "CA"},
	"dispatch-and-wait.md": {"ROLE_ID", "TASK", "PLANE", "CA"},
}

// guideRecipes pin the expected argv of every guide fence. A page not named
// here holds no shell fence at all.
var guideRecipes = map[string]map[string][]string{
	"roles-and-manuals.md": {
		"nodes": {"callsheet", "node", "ls", "--plane", "$PLANE", "--ca", "$CA"},
		"register": {"callsheet", "role", "add", "$ROLE_ID", "--name", "$ROLE_NAME", "--node", "$NODE", "--adapter", "claude", "--instruction", "$INSTRUCTION",
			"--runbook", "$RUNBOOK", "--model", "$MODEL", "--effort", "$EFFORT", "--concurrency", "1", "--timeout", "10m", "--plane", "$PLANE", "--ca", "$CA"},
		"roles": {"callsheet", "role", "ls", "--plane", "$PLANE", "--ca", "$CA"},
		"role":  {"callsheet", "role", "show", "$ROLE_ID", "--plane", "$PLANE", "--ca", "$CA"},
	},
	"dispatch-and-wait.md": {
		"dispatch": {"callsheet", "dispatch", "--role-id", "$ROLE_ID", "--goal", guideGoal, "--acceptance", guideAcceptance, "--plane", "$PLANE", "--ca", "$CA", "--json"},
		"wait":     {"callsheet", "task", "wait", "$TASK", "--until-done", "--plane", "$PLANE", "--ca", "$CA", "--json"},
		"show":     {"callsheet", "task", "show", "$TASK", "--plane", "$PLANE", "--ca", "$CA", "--json"},
		"logs":     {"callsheet", "task", "logs", "$TASK", "--plane", "$PLANE", "--ca", "$CA", "--json"},
		"list":     {"callsheet", "task", "ls", "--plane", "$PLANE", "--ca", "$CA", "--json"},
		"cancel":   {"callsheet", "task", "cancel", "$TASK", "--plane", "$PLANE", "--ca", "$CA", "--json"},
	},
}

// guideInventories pin the same fences' leaves for the shared contract core.
var guideInventories = map[string][]docFenceSpec{
	"roles-and-manuals.md": {{"nodes", "sh", []string{"node ls"}}, {"register", "sh", []string{"role add"}}, {"roles", "sh", []string{"role ls"}}, {"role", "sh", []string{"role show"}}},
	"dispatch-and-wait.md": {{"dispatch", "sh", []string{"dispatch"}}, {"wait", "sh", []string{"task wait"}}, {"show", "sh", []string{"task show"}},
		{"logs", "sh", []string{"task logs"}}, {"list", "sh", []string{"task ls"}}, {"cancel", "sh", []string{"task cancel"}}},
}

// The example's fence inventory, its variables and its fixed node mapping.
var (
	devFlowInventory = []docFenceSpec{
		{"ready-nodes", "sh", []string{"node ls"}},
		{"ready-roles", "sh", []string{"role ls"}},
		{"register-roles", "sh", []string{"role add", "role add", "role add", "role add", "role add"}},
	}
	devFlowVariables    = []string{"MANUALS", "PLANE", "CA", "CODEX_NODE", "CLAUDE_NODE"}
	devFlowNodeVariable = map[string]string{"designer": "CODEX_NODE", "code-reviewer": "CODEX_NODE", "triage": "CODEX_NODE", "design-reviewer": "CLAUDE_NODE", "coder": "CLAUDE_NODE"}
)

// guideTools maps a guide JSON marker to its MCP tool and exact argument keys.
var guideTools = map[string]struct {
	tool string
	keys []string
}{
	"dispatch": {"dispatch", []string{"acceptance", "goal", "target"}},
	"wait":     {"task_wait", []string{"task_ids"}},
	"show":     {"task_show", []string{"id"}},
	"logs":     {"task_logs", []string{"id"}},
	"list":     {"task_ls", nil},
	"cancel":   {"task_cancel", []string{"id"}},
}

// The declared illustrative placeholders, substituted only as whole string
// values and never as keys.
var (
	guidePlaceholders   = map[string]string{"<role-id>": "example-role"}
	devFlowPlaceholders = map[string]string{
		"<workspace>":           "example-workspace",
		"<instance>":            "0123456789abcdef0123456789abcdef",
		"<40-hex-commit>":       "0123456789abcdef0123456789abcdef01234567",
		"<40-hex-buggy-commit>": "0123456789abcdef0123456789abcdef01234567",
	}
)

func guideDoc(t *testing.T, file string) string {
	t.Helper()
	return string(repoFile(t, guideDir+file))
}

// runGuideMutations adds the no-op guard to runFPMutations: each mutation must
// change the document before its validator is asked to reject it.
func runGuideMutations(t *testing.T, md string, check func(string) error, ms []readmeMutation) {
	t.Helper()
	wrapped := make([]readmeMutation, len(ms))
	for i, m := range ms {
		wrapped[i] = m
		wrapped[i].apply = func(t *testing.T, md string) string {
			t.Helper()
			out := m.apply(t, md)
			if out == md {
				t.Fatalf("mutation %s changed nothing", m.name)
			}
			return out
		}
	}
	runFPMutations(t, md, check, wrapped)
}

// parseCallsheetOnly is the guide's fence parser: simple Callsheet commands
// only, so a substitution, pipeline, redirection or setup step is refused.
func parseCallsheetOnly(f qsFence) (qsParsed, error) {
	var p qsParsed
	cmds, err := tokenizeShell(f.id, f.body, f.line+1, false)
	if err != nil {
		return p, err
	}
	for _, c := range cmds {
		if first, ok := c.words[0].literal(); !ok || first != "callsheet" {
			return p, fmt.Errorf("fence %s line %d: only Callsheet commands are allowed", f.id, c.line)
		}
		p.callsheet = append(p.callsheet, c)
	}
	return p, nil
}

// checkArgv requires one parsed command to be exactly want, with every
// variable quoted, and names the first kind of difference it finds.
func checkArgv(where string, c qsCommand, want []string) error {
	for _, w := range c.words {
		for _, p := range w.parts {
			if p.variable != "" && !w.quoted {
				return fmt.Errorf("%s: unquoted variable $%s", where, p.variable)
			}
		}
	}
	got := renderWords(c.words)
	if slices.Equal(got, want) {
		return nil
	}
	isFlag := func(s string) bool { return strings.HasPrefix(s, "--") }
	for _, w := range want {
		if isFlag(w) && !slices.Contains(got, w) {
			return fmt.Errorf("%s: missing required flag %s", where, w)
		}
	}
	for _, g := range got {
		if isFlag(g) && !slices.Contains(want, g) {
			return fmt.Errorf("%s: unexpected flag %s", where, g)
		}
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Errorf("%s: word %d is %q, want %q", where, i, got[i], want[i])
		}
	}
	return fmt.Errorf("%s: %d words, want %d", where, len(got), len(want))
}

// checkGuideRecipes validates a page's pinned argv without any process.
func checkGuideRecipes(file, md string) error {
	fences, err := parseMarkedFences(md, guideMarker)
	if err != nil {
		return err
	}
	var errs []error
	recipes := guideRecipes[file]
	for _, id := range slices.Sorted(maps.Keys(recipes)) {
		f, ok := fenceByID(fences, id)
		if !ok {
			errs = append(errs, fmt.Errorf("inventory: required fence %s is missing", id))
			continue
		}
		p, err := parseCallsheetOnly(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(p.callsheet) != 1 {
			errs = append(errs, fmt.Errorf("fence %s holds %d commands, want 1", id, len(p.callsheet)))
			continue
		}
		errs = append(errs, checkArgv("fence "+id, p.callsheet[0], recipes[id]))
	}
	return errors.Join(errs...)
}

func guideContract(file string) docFenceContract {
	return docFenceContract{marker: guideMarker, inventory: guideInventories[file], parse: parseCallsheetOnly,
		variables: func(string) []string { return guideVariables[file] }}
}

func devFlowContract() docFenceContract {
	return docFenceContract{marker: devFlowMarker, inventory: devFlowInventory, parse: parseCallsheetOnly,
		variables: func(string) []string { return devFlowVariables }}
}

// checkGuideFrame is the chapter frame: one title, exact H2s, a first link
// back to the index, a last line naming the next reading and no private path.
func checkGuideFrame(c guideChapter, md string) error {
	d, err := parseReadme(md)
	if err != nil {
		return err
	}
	var names []string
	for _, s := range d.sections {
		names = append(names, s.name)
	}
	if !slices.Equal(names, c.h2) {
		return fmt.Errorf("%s: H2 headings %q, want %q", c.file, names, c.h2)
	}
	if paras := paragraphs(d.preamble); len(paras) == 0 || paras[0] != "[User guide](index.md)" {
		return fmt.Errorf("%s: does not start with a link to the index", c.file)
	}
	lines := strings.Split(strings.TrimSpace(md), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "Next: [") || !slices.Contains(markdownLinks(last), c.next) {
		return fmt.Errorf("%s: last line %q does not name the next reading %s", c.file, last, c.next)
	}
	return checkPrivateText(c.file, md)
}

// wantLinks requires each exact destination among md's links.
func wantLinks(where, md string, links ...string) error {
	have := markdownLinks(md)
	var errs []error
	for _, l := range links {
		if !slices.Contains(have, l) {
			errs = append(errs, fmt.Errorf("%s: missing link %s", where, l))
		}
	}
	return errors.Join(errs...)
}

// checkNoGuideFences refuses any shell fence, and any hidden Callsheet command
// in another fence, on a page that owns no executable recipe.
func checkNoGuideFences(file, md string) error {
	fences, err := parseMarkedFences(md, guideMarker)
	if err != nil {
		return err
	}
	for _, f := range fences {
		if f.id != "" {
			return fmt.Errorf("inventory: fence %s is not in the pinned inventory of %s", f.id, file)
		}
	}
	return nil
}

// ---- FP-1: guide entry and concepts.

func checkGuideIndex(md string) error {
	d, err := parseReadme(md)
	if err != nil {
		return err
	}
	var names []string
	for _, s := range d.sections {
		names = append(names, s.name)
	}
	if want := []string{"Start here", "Learn Callsheet", "References", "Example"}; !slices.Equal(names, want) {
		return fmt.Errorf("index: H2 headings %q, want %q", names, want)
	}
	var errs []error
	start, _ := d.section("Start here")
	if l := markdownLinks(start); len(l) == 0 || l[0] != "../quickstart.md" {
		errs = append(errs, errors.New("index: Start here does not link the quickstart first"))
	}
	learn, _ := d.section("Learn Callsheet")
	want := make([]string, 0, len(guideChapters)+1)
	for _, c := range guideChapters {
		want = append(want, c.file)
	}
	if got := markdownLinks(learn); !slices.Equal(got, append(want, "../deploy.md")) {
		errs = append(errs, fmt.Errorf("index: Learn Callsheet links %q, want the eight chapters in order, then deployment", got))
	}
	refs, _ := d.section("References")
	errs = append(errs, wantLinks("index", refs, "../coordinator.md", "../workspaces.md", "../real-adapters.md", "../support-catalog.md"))
	example, _ := d.section("Example")
	errs = append(errs, wantLinks("index", example, "../examples/dev-flow/README.md", "example.md", "../maintainers/real-e2e.md"))
	if !strings.Contains(example, "Maintainer evidence, not a prerequisite") {
		errs = append(errs, errors.New("index: the maintainer link is not labeled as evidence"))
	}
	errs = append(errs, checkPrivateText("index", md))
	return errors.Join(errs...)
}

func checkGuideConcepts(md string) error {
	prose := stripInlineLinks(md)
	return errors.Join(checkGuideFrame(guideChapterOf("concepts.md"), md), checkNoGuideFences("concepts.md", md),
		needAll("concepts", prose,
			"**Plane.**", "**Sidecar and node.**", "**Coordinator.** Your own client", "Callsheet hosts no coordinator.",
			"The **role ID** is unique", "The **role name** is a logical label", "The **instruction** is standing guidance and the **runbook** is the procedure.",
			"A **task ID** identifies one admitted task.", "**workspace name**", "**workspace instance**", "The **base** is the commit", "The **result commit**",
			"Dispatch is asynchronous admission", "There is no queue and no reroute.", "Each task starts a fresh worker process",
			"Continuity between tasks is carried by files, not by conversation reuse.", "A task without a workspace gets a scratch directory, not a delivered project.",
			"A published result does not advance a branch", "Acceptance text is passed to the worker, not evaluated by Callsheet.",
			"Callsheet needs no Callsheet SaaS account, no workflow object, no vendor CLI patches and no bundled VPN.", "Vendor accounts and authentication may still be needed.",
			"only trusted peers may reach it, there is no per-client authorization, Callsheet adds no sandbox of its own, and sidecars belong under a dedicated worker account"),
		wantLinks("concepts", md, "../deploy.md#security-posture", "../deploy.md#give-clients-the-ca"),
		forbidAll("concepts", md, "tasks are queued", "queued until", "rerouted", "callsheet evaluates acceptance", "resumes the conversation", "resumes a conversation", "saas account is required", "workflow object is required"))
}

// TestGuideConcepts (FP-1): the index routes to every chapter and reference, and
// the concepts chapter defines every identity and the AC-NF-1 boundaries.
func TestGuideConcepts(t *testing.T) {
	index, concepts := guideDoc(t, "index.md"), guideDoc(t, "concepts.md")
	if err := checkGuideIndex(index); err != nil {
		t.Fatalf("shipped index: %v", err)
	}
	if err := checkGuideConcepts(concepts); err != nil {
		t.Fatalf("shipped concepts: %v", err)
	}
	t.Run("index", func(t *testing.T) {
		runGuideMutations(t, index, checkGuideIndex, []readmeMutation{
			{"remove-chapter", replaceOnce("6. [Worker setup](worker-setup.md): the ordered enabling procedure for each of the four worker CLIs.\n", ""), "Learn Callsheet links"},
			{"reorder-chapters", replaceOnce("1. [Concepts](concepts.md)", "1. [Roles and manuals](roles-and-manuals.md) 0. [Concepts](concepts.md)"), "Learn Callsheet links"},
			{"remove-reference", replaceOnce("[CLI support catalog](../support-catalog.md)", "CLI support catalog"), "missing link ../support-catalog.md"},
			{"quickstart-not-first", replaceOnce("Complete the [quickstart](../quickstart.md) first.", "Read [concepts](concepts.md), then the [quickstart](../quickstart.md)."), "does not link the quickstart first"},
			{"unlabeled-maintainer-link", replaceOnce("Maintainer evidence, not a prerequisite of this path:", "Required next:"), "not labeled as evidence"},
			{"renamed-section", replaceOnce("## References", "## Reference"), "H2 headings"},
			{"private-path", replaceOnce("## Example\n", "## Example\n\nSee /home/someone.\n"), "private path sentinel"},
		})
	})
	t.Run("concepts", func(t *testing.T) {
		runGuideMutations(t, concepts, checkGuideConcepts, []readmeMutation{
			{"imply-queued", replaceOnce("There is no queue and no reroute.", "Tasks are queued and rerouted to a free node."), "lacks \"There is no queue"},
			{"acceptance-evaluator", replaceOnce("Acceptance text is passed to the worker, not evaluated by Callsheet.", "Callsheet evaluates acceptance before publishing."), "lacks \"Acceptance text"},
			{"resumed-conversation", replaceOnce("Continuity between tasks is carried by files, not by conversation reuse.", "Each task resumes the conversation of the last one."), "lacks \"Continuity"},
			{"required-saas", replaceOnce("no Callsheet SaaS account, no workflow object", "a Callsheet SaaS account and a workflow object"), "lacks \"Callsheet needs no Callsheet SaaS"},
			{"branch-advances", replaceOnce("A published result does not advance a branch", "A published result advances main"), "lacks \"A published result does not"},
			{"identity-removed", replaceOnce("The **role name** is a logical label", "A name is a label"), "lacks \"The **role name**"},
			{"no-index-link", replaceOnce("[User guide](index.md)\n", "User guide\n"), "does not start with a link to the index"},
			{"no-next", replaceOnce("Next: [Roles and manuals](roles-and-manuals.md).", "The end."), "does not name the next reading"},
			{"added-claim", appendToSection("Files and results", "Tasks are queued until a slot frees."), "forbidden \"tasks are queued\""},
			{"hidden-command", appendToSection("Files and results", "```text\ncallsheet task ls\n```"), "holds a Callsheet command"},
		})
	})
}

// ---- FP-2: roles and manuals.

// checkGuideHelpPointers resolves each inline `callsheet ... --help` pointer
// through the real tree and leaf help and requires the two lifecycle pointers.
func checkGuideHelpPointers(t *testing.T, md string) error {
	t.Helper()
	root := cli.NewTree(runtime.GOOS)
	var seen []string
	var errs []error
	for _, m := range guideHelpPointer.FindAllStringSubmatch(md, -1) {
		seen = append(seen, m[1])
		cmds, err := tokenizeShell("help-pointer", m[1], 1, false)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		leaf, _, err := leafOf(root, cmds[0])
		if err != nil {
			errs = append(errs, fmt.Errorf("pointer %s: %w", m[1], err))
			continue
		}
		if _, err := leafHelp(t, leaf); err != nil {
			errs = append(errs, err)
		}
	}
	for _, p := range []string{"callsheet role set --help", "callsheet role rm --help"} {
		if !slices.Contains(seen, p) {
			errs = append(errs, fmt.Errorf("roles: missing lifecycle pointer %s", p))
		}
	}
	return errors.Join(errs...)
}

func checkGuideRoles(t *testing.T, md string) error {
	t.Helper()
	const file = "roles-and-manuals.md"
	prose := stripInlineLinks(md)
	return errors.Join(checkGuideFrame(guideChapterOf(file), md), checkGuideRecipes(file, md), checkGuideHelpPointers(t, md),
		needAll("roles", prose,
			"Both are UTF-8 files read on the selected sidecar when a task runs.", "Copying them only to the coordinator is insufficient",
			"Callsheet does not distribute manuals", "These worker manuals are different from the coordinator's user-loaded playbook", "Callsheet never reads that playbook.",
			"They are your own selections, not built-in environment configuration", "`node <id> is offline; start its sidecar and retry`",
			"A node identity can exist before its first online heartbeat.", "you retry only after the node is online",
			"Selection stays explicit", "Vendor acceptance is distinct from Callsheet validation", "Inspect the roster after all registry changes",
			"`can_accept`", "A successful version probe is not authentication", "`concurrency` sets the role's slots.", "`--role-id` pins an instance",
			"Do not change a model or permissions automatically after a refusal."),
		inOrder("roles", md, "<!-- guide: nodes -->", "<!-- guide: register -->", "<!-- guide: roles -->", "<!-- guide: role -->"),
		wantLinks("roles", md, "../quickstart.md", "../real-adapters.md#selection-and-versions"),
		forbidAll("roles", md, "role update", "callsheet copies the manuals", "callsheet distributes", "--force"))
}

// TestGuideRolesAndManuals (FP-2).
func TestGuideRolesAndManuals(t *testing.T) {
	md := guideDoc(t, "roles-and-manuals.md")
	check := func(md string) error { return checkGuideRoles(t, md) }
	if err := check(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	register := "callsheet role add \"$ROLE_ID\" --name \"$ROLE_NAME\" --node \"$NODE\" --adapter claude --instruction \"$INSTRUCTION\" --runbook \"$RUNBOOK\" --model \"$MODEL\" --effort \"$EFFORT\" --concurrency 1 --timeout 10m --plane \"$PLANE\" --ca \"$CA\""
	runGuideMutations(t, md, check, []readmeMutation{
		{"online-check-after-add", func(t *testing.T, md string) string {
			md = replaceOnce("<!-- guide: nodes -->", "<!-- guide: tmp -->")(t, md)
			md = replaceOnce("<!-- guide: register -->", "<!-- guide: nodes -->")(t, md)
			return replaceOnce("<!-- guide: tmp -->", "<!-- guide: register -->")(t, md)
		}, "fence"},
		{"online-prose-removed", replaceOnce("you retry only after the node is online", "you may retry at once"), "lacks \"you retry only after"},
		{"coordinator-local-manuals", replaceOnce("Both are UTF-8 files read on the selected sidecar when a task runs. Copying them only to the coordinator is insufficient", "Both are UTF-8 files read on the coordinator. Copying them to the coordinator is sufficient"), "lacks \"Both are UTF-8"},
		{"omit-model", replaceOnce(register, strings.Replace(register, ` --model "$MODEL"`, "", 1)), "missing required flag --model"},
		{"omit-effort", replaceOnce(register, strings.Replace(register, ` --effort "$EFFORT"`, "", 1)), "missing required flag --effort"},
		{"remove-can-accept", replaceOnce("`can_accept` true (`CAN_ACCEPT` in the text listing)", "listing correct"), "lacks \"`can_accept`\""},
		{"role-name-as-node", replaceOnce(`--node "$NODE"`, `--node "$ROLE_NAME"`), "word 7 is \"$ROLE_NAME\", want \"$NODE\""},
		{"role-update-pointer", replaceOnce("`callsheet role set --help`", "`callsheet role update --help`"), "unknown command \"update\""},
		{"role-update-prose", appendToSection("Change a role", "Use role update for this."), "forbidden \"role update\""},
		{"auto-fallback", replaceOnce("Do not change a model or permissions automatically after a refusal.", "Change the model automatically after a refusal."), "lacks \"Do not change a model"},
		{"probe-is-authentication", replaceOnce("A successful version probe is not authentication", "A successful version probe proves authentication"), "lacks \"A successful version probe"},
		{"callsheet-distributes", replaceOnce("Callsheet does not distribute manuals", "Callsheet distributes manuals"), "lacks \"Callsheet does not distribute"},
	})
}

// ---- FP-3: dispatch and wait.

// markedJSONFences returns md's JSON fences by marker: each JSON fence must
// follow a unique marker that is immediately before it.
func markedJSONFences(md string, marker *regexp.Regexp) (map[string]string, error) {
	out := map[string]string{}
	lines := strings.Split(md, "\n")
	pending := ""
	inOther := false
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if inOther {
			inOther = trimmed != "```"
			continue
		}
		if m := marker.FindStringSubmatch(trimmed); m != nil {
			if _, dup := out[m[1]]; dup || pending != "" {
				return nil, fmt.Errorf("json: duplicate or misplaced marker %s at line %d", m[1], i+1)
			}
			pending = m[1]
			continue
		}
		switch {
		case trimmed == "```json":
			if pending == "" {
				return nil, fmt.Errorf("json: unmarked JSON fence at line %d", i+1)
			}
			var body []string
			closed := false
			for i++; i < len(lines); i++ {
				if strings.TrimSpace(lines[i]) == "```" {
					closed = true
					break
				}
				body = append(body, lines[i])
			}
			if !closed {
				return nil, fmt.Errorf("json: fence %s is not closed", pending)
			}
			out[pending] = strings.Join(body, "\n")
			pending = ""
		case pending != "":
			return nil, fmt.Errorf("json: marker %s is not immediately before a JSON fence", pending)
		case strings.HasPrefix(trimmed, "```"):
			inOther = true
		}
	}
	if pending != "" {
		return nil, fmt.Errorf("json: marker %s is not immediately before a JSON fence", pending)
	}
	return out, nil
}

// substitutePlaceholders replaces whole declared string values only.
func substitutePlaceholders(v any, declared map[string]string) any {
	switch x := v.(type) {
	case string:
		if r, ok := declared[x]; ok {
			return r
		}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = substitutePlaceholders(e, declared)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = substitutePlaceholders(e, declared)
		}
		return out
	}
	return v
}

// guideToolArguments decodes the page's six tool examples and checks their
// exact shape and cross-field rules, with placeholders substituted.
func guideToolArguments(md string) (map[string]any, error) {
	fences, err := markedJSONFences(md, guideJSONMarker)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(fences)) {
		if _, ok := guideTools[id]; !ok {
			errs = append(errs, fmt.Errorf("json: fence %s is not in the pinned inventory", id))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(guideTools)) {
		spec := guideTools[id]
		body, ok := fences[id]
		if !ok {
			errs = append(errs, fmt.Errorf("json: required fence %s is missing", id))
			continue
		}
		if !strings.Contains(md, "Tool `"+spec.tool+"`:\n\n<!-- guide-json: "+id+" -->") {
			errs = append(errs, fmt.Errorf("json: fence %s is not labeled as tool %s", id, spec.tool))
		}
		v, err := decodeStrictJSON(body)
		if err != nil {
			errs = append(errs, fmt.Errorf("json: fence %s: %w", id, err))
			continue
		}
		args, ok := v.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Errorf("json: fence %s is not an arguments object", id))
			continue
		}
		if got := slices.Sorted(maps.Keys(args)); !slices.Equal(got, spec.keys) {
			errs = append(errs, fmt.Errorf("json: fence %s has keys %q, want %q", id, got, spec.keys))
		}
		out[spec.tool] = substitutePlaceholders(args, guidePlaceholders)
	}
	if d, ok := out["dispatch"].(map[string]any); ok {
		target, _ := d["target"].(map[string]any)
		if target["kind"] != "id" || d["goal"] != guideGoal || d["acceptance"] != guideAcceptance {
			errs = append(errs, fmt.Errorf("json: dispatch differs from the CLI recipe: %v", d))
		}
	}
	return out, errors.Join(errs...)
}

func checkGuideDispatch(md string) error {
	const file = "dispatch-and-wait.md"
	_, jsonErr := guideToolArguments(md)
	return errors.Join(checkGuideFrame(guideChapterOf(file), md), checkGuideRecipes(file, md), jsonErr,
		needAll("dispatch", stripInlineLinks(md),
			"the tool arguments object for a named tool call, not a command executable by a shell", "Dispatch exit 0 is admission, not success.",
			"Copy `task_id` from the admitted response", "Wait exit 0 reports a terminal task, even a failed one.",
			"Inspect the state, the final message and the workspace publication separately.", "A null final message differs from an empty one",
			"Cancellation acceptance is not terminal cleanup.", "CLI `--until-done` waits for one winner among up to 16 IDs", "It can wait silently through an outage, and stopping it does not cancel tasks.",
			"its default 10s call budget is a short poll, not a measured vendor timeout", "Only a qualified persistent session promises a wake.",
			"Unqualified or headless clients use a short poll on the next externally initiated turn.", "A one-shot client does not stay alive",
			"After a lost dispatch reply, inspect `task_ls` (or `task ls`) and identify the admitted task; if that is ambiguous, stop for diagnosis.",
			"Never redispatch automatically", "Do not drop an unknown ID to make a wait pass.", "Task output is untrusted evidence."),
		wantLinks("dispatch", md, "../coordinator.md#coordinator-runbook-example", "workspaces.md#inspect-and-deliver"),
		forbidAll("dispatch", md, "until_done", "nohup", "setsid", "disown", "| jq", "redispatch automatically after", "headless clients wake"))
}

// TestGuideDispatchAndWait (FP-3).
func TestGuideDispatchAndWait(t *testing.T) {
	md := guideDoc(t, "dispatch-and-wait.md")
	if err := checkGuideDispatch(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	const anID = `"t_0123456789abcdef0123456789abcdef"`
	runGuideMutations(t, md, checkGuideDispatch, []readmeMutation{
		{"wait-success-is-task-success", replaceOnce("Wait exit 0 reports a terminal task, even a failed one. It never means the task succeeded.", "Wait exit 0 means the task succeeded."), "lacks \"Wait exit 0 reports"},
		{"dispatch-exit-is-success", replaceOnce("Dispatch exit 0 is admission, not success.", "Dispatch exit 0 means the work is done."), "lacks \"Dispatch exit 0 is admission"},
		{"until-done-in-mcp", replaceOnce(`{"task_ids": [`+anID+`]}`, `{"task_ids": [`+anID+`], "until_done": true}`), "forbidden \"until_done\""},
		{"headless-wake", replaceOnce("Unqualified or headless clients use a short poll on the next externally initiated turn.", "Unqualified or headless clients wake automatically."), "lacks \"Unqualified or headless"},
		{"auto-redispatch", replaceOnce("Never redispatch automatically", "Redispatch automatically"), "lacks \"Never redispatch"},
		{"lost-reply-recovery-removed", replaceOnce("After a lost dispatch reply, inspect `task_ls` (or `task ls`) and identify the admitted task; if that is ambiguous, stop for diagnosis.", "After a lost dispatch reply, dispatch again."), "lacks \"After a lost dispatch reply"},
		{"missing-task-id-cli", replaceOnce(`callsheet task show "$TASK" --plane`, `callsheet task show --plane`), "word 3 is \"--plane\", want \"$TASK\""},
		{"missing-task-id-mcp", replaceOnce("Tool `task_show`:\n\n<!-- guide-json: show -->\n```json\n{\"id\": "+anID+"}", "Tool `task_show`:\n\n<!-- guide-json: show -->\n```json\n{}"), "fence show has keys"},
		{"no-final-state-inspection", replaceOnce("Inspect the state, the final message and the workspace publication separately.", "The exit is enough."), "lacks \"Inspect the state"},
		{"null-is-empty", replaceOnce("A null final message differs from an empty one", "A null final message is an empty one"), "lacks \"A null final message differs"},
		{"cancel-is-cleanup", replaceOnce("Cancellation acceptance is not terminal cleanup.", "Cancellation acceptance ends the task."), "lacks \"Cancellation acceptance"},
		{"detached-loop", appendToSection("Wait and inspect", "Run it with nohup."), "forbidden \"nohup\""},
		{"parity-goal-differs", func(t *testing.T, md string) string {
			return replaceOnce(`"goal": "`+guideGoal+`"`, `"goal": "Another goal."`)(t, md)
		}, "dispatch differs from the CLI recipe"},
		{"unlabeled-tool", replaceOnce("Tool `task_logs`:", "Logs:"), "fence logs is not labeled as tool task_logs"},
		{"not-labeled-arguments", replaceOnce("not a command executable by a shell", "run it in your shell"), "lacks \"the tool arguments"},
	})
}

// ---- FP-4: workspaces.

func checkGuideWorkspaces(md string) error {
	const file = "workspaces.md"
	return errors.Join(checkGuideFrame(guideChapterOf(file), md), checkNoGuideFences(file, md),
		inOrder("workspaces", md, "1. Create the workspace.", "2. Push the source folder.", "3. Obtain the workspace instance and the seed commit",
			"4. Dispatch the first task at that immutable base, with the instance.", "5. Inspect the task's publication and read all diff pages.",
			"6. Chain the next role using the result commit and the bound instance, never a floating branch.", "7. Pull the accepted task into a new destination."),
		needAll("workspaces", stripInlineLinks(md),
			"Start from an ordinary non-git source folder and a fresh destination.", "Git is not required", "Workspace refs live on the plane.",
			"Your local source is not automatically synchronized", "The selected base and instance are fixed at admission", "No branch moves on task completion.",
			"A succeeded process can have failed publication, and a failed publication is not delivery.", "Unavailable results cannot be delivered.",
			"A diff is paged, so read every page", "Publication and semantic approval are separate questions", "refuses overwrite",
			"A stale instance or generation is a conflict requiring fresh inspection, not blind overwrite."),
		wantLinks("workspaces", md, "../workspaces.md#pushing-a-plain-folder", "../workspaces.md#chaining-tasks", "../workspaces.md#task-results-by-task-id",
			"../workspaces.md#pulling-into-a-folder", "../workspaces.md#ignore-rules", "../workspaces.md#errors", "example.md"),
		forbidAll("workspaces", md, "--force", "overwrite the destination", "chain from main", "ws push --instance"))
}

// TestGuideWorkspaces (FP-4).
func TestGuideWorkspaces(t *testing.T) {
	md := guideDoc(t, "workspaces.md")
	if err := checkGuideWorkspaces(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	runGuideMutations(t, md, checkGuideWorkspaces, []readmeMutation{
		{"floating-branch-base", replaceOnce("6. Chain the next role using the result commit and the bound instance, never a floating branch.", "6. Chain the next role from main."), "lacks step \"6. Chain"},
		{"omitted-instance", replaceOnce("4. Dispatch the first task at that immutable base, with the instance.", "4. Dispatch the first task at that immutable base."), "lacks step \"4. Dispatch"},
		{"out-of-order", func(t *testing.T, md string) string {
			md = replaceOnce("5. Inspect the task's publication and read all diff pages.", "@@")(t, md)
			md = replaceOnce("7. Pull the accepted task into a new destination.", "5. Inspect the task's publication and read all diff pages.")(t, md)
			return replaceOnce("@@", "7. Pull the accepted task into a new destination.")(t, md)
		}, "is out of order"},
		{"failed-publication-is-delivery", replaceOnce("A succeeded process can have failed publication, and a failed publication is not delivery.", "A succeeded process has delivered its result."), "lacks \"A succeeded process can"},
		{"ignored-diff-continuation", replaceOnce("A diff is paged, so read every page", "Read the first diff page"), "lacks \"A diff is paged"},
		{"overwrite-advice", replaceOnce("refuses overwrite", "will overwrite the destination for you"), "lacks \"refuses overwrite\""},
		{"blind-overwrite", replaceOnce("A stale instance or generation is a conflict requiring fresh inspection, not blind overwrite.", "Retry a stale instance until it passes."), "lacks \"A stale instance"},
		{"reference-link-removed", replaceOnce("[errors](../workspaces.md#errors)", "errors"), "missing link ../workspaces.md#errors"},
		{"new-shell-fence", appendToSection("Chain results", "```sh\ncallsheet ws create x\n```"), "unmarked sh fence"},
		{"marked-shell-fence", appendToSection("Chain results", "<!-- guide: extra -->\n```sh\ncallsheet ws create x\n```"), "fence extra is not in the pinned inventory"},
	})
}

// ---- FP-5: coordinator setup.

func checkGuideCoordinator(md string) error {
	const file = "coordinator-setup.md"
	errs := []error{checkGuideFrame(guideChapterOf(file), md), checkNoGuideFences(file, md),
		inOrder("coordinator", md, "1. Authenticate the chosen coordinator", "2. Install Callsheet", "3. Obtain the plane URL and its public CA", "4. Apply that vendor's user-owned MCP recipe",
			"5. Authorize Callsheet MCP access in that session", "6. Load your user playbook", "7. Discover nodes and roles", "8. Dispatch only when ready"),
		needAll("coordinator", stripInlineLinks(md), "Callsheet never edits configuration", "Coordinator eligibility is not worker qualification",
			"not a live qualification of a wake", "Any client without a verified wake polls with a short `task_wait` only when it next has a turn.",
			"Advanced `mcpqual` is an optional maintainer qualification tool, not required to use the example"),
		wantLinks("coordinator", md, "../coordinator.md#coordinator-runbook-example", "../coordinator.md#local-timeout-qualification", "../deploy.md#connection-not-trusted"),
		forbidAll("coordinator", md, "mcp add", "mcpservers", "mcp_servers", "wakes every", "headless clients wake", "mcpqual is required")}
	for vendor, anchor := range map[string]string{"Claude Code": "claude-code", "OpenAI Codex": "openai-codex", "Grok Build": "grok-build", "Cursor Agent": "cursor-agent"} {
		body, err := sectionOf(md, vendor)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, wantLinks("coordinator "+vendor, body, "../coordinator.md#"+anchor))
		if !strings.Contains(body, "Evidence caveat:") || !strings.Contains(body, "for loading the playbook") {
			errs = append(errs, fmt.Errorf("coordinator %s: lacks its playbook route or evidence caveat", vendor))
		}
	}
	return errors.Join(errs...)
}

// TestGuideCoordinatorSetup (FP-5).
func TestGuideCoordinatorSetup(t *testing.T) {
	md := guideDoc(t, "coordinator-setup.md")
	if err := checkGuideCoordinator(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	runGuideMutations(t, md, checkGuideCoordinator, []readmeMutation{
		{"remove-vendor-link", replaceOnce("[Grok Build](../coordinator.md#grok-build)", "Grok Build"), "missing link ../coordinator.md#grok-build"},
		{"wrong-vendor-link", replaceOnce("[Cursor Agent](../coordinator.md#cursor-agent)", "[Cursor Agent](../coordinator.md#claude-code)"), "missing link ../coordinator.md#cursor-agent"},
		{"remove-authentication", replaceOnce("1. Authenticate the chosen coordinator under the actual user who will run it.\n", ""), "lacks step \"1. Authenticate"},
		{"remove-playbook-step", replaceOnce("6. Load your user playbook.\n", ""), "lacks step \"6. Load"},
		{"remove-caveat", replaceOnce("Evidence caveat: its wake is UNVERIFIED in the table, so use a short `task_wait` on the next externally started turn.", "It wakes."), "lacks its playbook route or evidence caveat"},
		{"promise-headless-wake", replaceOnce("Any client without a verified wake polls with a short `task_wait` only when it next has a turn.", "Any client wakes automatically."), "lacks \"Any client without"},
		{"mcpqual-mandatory", replaceOnce("Advanced `mcpqual` is an optional maintainer qualification tool, not required to use the example", "Advanced `mcpqual` is required to use the example"), "lacks \"Advanced `mcpqual`"},
		{"copied-registration", appendToSection("Claude Code", "Run claude mcp add first."), "forbidden \"mcp add\""},
		{"coordinator-is-qualification", replaceOnce("Coordinator eligibility is not worker qualification", "Coordinator eligibility is worker qualification"), "lacks \"Coordinator eligibility"},
		{"callsheet-edits-config", replaceOnce("Callsheet never edits configuration", "Callsheet edits your configuration"), "lacks \"Callsheet never edits"},
	})
}

// ---- FP-6: the example.

var guideExampleStale = []string{
	"live qualification stays pending", "used by the four-hop exercise", "current four-hop harness constants", "M5 passed", "M5 is complete", "M5 is passed", "docs-only walkthrough has passed",
}

func checkGuideExampleReadme(readme, playbook string) error {
	rows, tableErr := parseRoleTable(readme)
	errs := []error{tableErr, checkRegistration(readme, canonicalRoles), readmeLinksManuals(readme), checkHandoffs(playbook), checkPrivateText("example README", readme),
		needAll("example README", stripInlineLinks(readme),
			"The default vendors are Claude and Codex: five roles, two installed tools.", "The coordinator is the owner's client using the playbook.",
			"No reale2e binary, development checkout, extra orchestrator or Callsheet workflow object is required.", "An ordinary folder is a valid workspace source.",
			"**Callsheet itself and the coordinator require no git CLI.**", "**The node hosting code-reviewer needs git on PATH**", "`git archive --format=tar`",
			"**Nodes hosting coder, code-reviewer and triage need the project verification tools**", "including Go for the section 8 greeting-CLI scenario",
			"A plain-folder transfer does not remove these worker-side tool requirements.", "requires an additional eligible Linux installation",
			"Configure the owner's coordinator using coordinator setup and load coordinator-playbook.md.", "retain the instance and seed commit",
			"Give the coordinator the request and the initial base commit.", "Inspect the exact artifacts and digests before answering.", "Wait for an explicit yes.",
			"The coder and reviewer consume pinned accepted commits.", "pull the accepted task into a new destination",
			"Keep the original source base for the red-before check", "A `NEEDS_DESIGN` marker routes to full approval.",
			"default probe 11, the combined full and light exercise `20261026T102944Z-d3827a6b` and the owner's external judgment audits establish the recorded 12a method run",
			"the optional Cursor evidence is the later qual2 and exercise2 record, never the superseded first exercise",
			"They are not proof that a docs-only walkthrough already happened", "not a hidden workflow API",
			"Each file in this directory is at most 32 KiB.", "The figures above are adoption defaults for this example; they do not change the verification harness's own limits.",
			"The four-hop sample in which Cursor reviews code is a separate scenario, not exercised by this selection."),
		inOrder("example README", readme, "## Start here", "## Run with Callsheet", "## Installation", "## Role selection", "## Full walkthrough", "## Light walkthrough"),
		wantLinks("example README", readme, "coordinator-playbook.md#routing-table", "coordinator-playbook.md#admission", "coordinator-playbook.md#caps", "../../maintainers/real-e2e.md", "../../guide/example.md#m5-walkthrough-checklist"),
		forbidAll("example README", readme, append(slices.Clone(guideExampleStale), "reale2e is required", "`<node-id>`")...),
	}
	if tableErr == nil {
		errs = append(errs, validateSelections(rows))
	}
	for _, name := range []string{"Full walkthrough", "Light walkthrough"} {
		body, err := sectionOf(readme, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, needAll("example README "+name, body, "pull the accepted task into a new destination"))
	}
	if playRows, err := parseRoleTable(playbook); err != nil || !sameRoles(rows, playRows) {
		errs = append(errs, fmt.Errorf("example README: the README and playbook role tables disagree (%v)", err))
	}
	return errors.Join(errs...)
}

func checkGuideExampleChapter(md string) error {
	const file = "example.md"
	errs := []error{checkGuideFrame(guideChapterOf(file), md), checkNoGuideFences(file, md),
		needAll("example", stripInlineLinks(md), "its README is the canonical set of instructions", "with the owner's explicit yes before coding",
			"**Mechanics.**", "**Recorded live evidence.**", "**M5 usability.**", "That harness is not a user dependency.",
			"Neither the offline tests nor the 12a exercise is proof that a docs-only walkthrough already happened.",
			"The owner records the actual pass or blockers here; this document does not prefill them.", "Whether a release is available is conditional."),
		wantLinks("example", md, "../examples/dev-flow/README.md", "../examples/dev-flow/README.md#start-here", "../examples/dev-flow/README.md#run-with-callsheet",
			"../examples/dev-flow/README.md#full-walkthrough", "../examples/dev-flow/README.md#light-walkthrough", "../examples/dev-flow/coordinator-playbook.md", "../maintainers/real-e2e.md#combined-exercise"),
		forbidAll("example", md, append(slices.Clone(guideExampleStale), "reale2e is required", "minutes to first dispatch")...),
	}
	rows, err := parseMarkdownTable(md, "Evidence", "Recorded value")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	want := []string{"OS and architecture", "Callsheet and vendor versions", "Selected roles", "Source, instance, commits and task IDs", "Time to first dispatch", "Full or light route taken", "Approval and delivery", "Every snag and its disposition"}
	var names []string
	for _, r := range rows {
		names = append(names, r[0])
		if strings.TrimSpace(r[1]) != "" {
			errs = append(errs, fmt.Errorf("example: the M5 checklist row %q is prefilled", r[0]))
		}
	}
	if !slices.Equal(names, want) {
		errs = append(errs, fmt.Errorf("example: M5 checklist rows %q, want %q", names, want))
	}
	return errors.Join(errs...)
}

// TestGuideExample (FP-6): the runnable README and the short chapter. The method
// mechanics stay with the existing TestDevFlow* parents; this is the path into it.
func TestGuideExample(t *testing.T) {
	readme, playbook, chapter := string(repoFile(t, guideExampleDoc)), string(repoFile(t, guidePlaybook)), guideDoc(t, "example.md")
	checkReadme := func(md string) error { return checkGuideExampleReadme(md, playbook) }
	if err := checkReadme(readme); err != nil {
		t.Fatalf("shipped README: %v", err)
	}
	if err := checkGuideExampleChapter(chapter); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	t.Run("readme", func(t *testing.T) {
		runGuideMutations(t, readme, checkReadme, []readmeMutation{
			{"callsheet-needs-git", replaceOnce("**Callsheet itself and the coordinator require no git CLI.**", "**Callsheet and the coordinator need the git CLI.**"), "lacks \"**Callsheet itself"},
			{"omit-reviewer-git", replaceOnce("**The node hosting code-reviewer needs git on PATH**", "**The node hosting code-reviewer needs nothing more**"), "lacks \"**The node hosting"},
			{"omit-project-tools", replaceOnce("**Nodes hosting coder, code-reviewer and triage need the project verification tools**", "**Nodes need nothing else**"), "lacks \"**Nodes hosting"},
			{"omit-go", replaceOnce("including Go for the section 8 greeting-CLI scenario", "for the scenario"), "lacks \"including Go"},
			{"require-reale2e", replaceOnce("No reale2e binary, development checkout, extra orchestrator or Callsheet workflow object is required.", "The reale2e binary is required."), "lacks \"No reale2e binary"},
			{"require-reale2e-added", appendToSection("Start here", "Note that reale2e is required."), "forbidden \"reale2e is required\""},
			{"change-role-mapping", replaceOnce("callsheet role add coder --name coder --node \"$CLAUDE_NODE\"", "callsheet role add coder --name coder --node \"$CODEX_NODE\""), "word 7 is \"$CODEX_NODE\", want \"$CLAUDE_NODE\""},
			{"change-role-model", replaceOnce("| coder | claude | claude-opus-5-5 | high | 1 | 10m |", "| coder | claude | claude-opus-5-5 | low | 1 | 10m |"), "role table"},
			{"omit-owner-yes", replaceOnce("Wait for an explicit yes.", "Continue."), "lacks \"Wait for an explicit yes.\""},
			{"omit-playbook-loading", replaceOnce("and load [coordinator-playbook.md](coordinator-playbook.md).", "and start."), "lacks \"Configure the owner's coordinator"},
			{"omit-delivery", replaceOnce("then pull the accepted task into a new destination.", "then finish."), "example README Full walkthrough: lacks \"pull the accepted task"},
			{"omit-pinned-handoff", replaceOnce("The coder and reviewer consume pinned accepted commits.", "The coder uses main."), "lacks \"The coder and reviewer"},
			{"restore-live-pending", replaceOnce("is not a judgment of the method's outputs.", "is not a judgment of the method's outputs, and its live qualification stays pending until the owner-run attempt is recorded."), "forbidden \"live qualification stays pending\""},
			{"restore-four-hop-exercise", replaceOnce("Each file in this directory is at most 32 KiB.", "Each file in this directory is at most 32 KiB, the existing template bound used by the four-hop exercise."), "forbidden \"used by the four-hop exercise\""},
			{"restore-four-hop-constants", replaceOnce("The figures above are adoption defaults for this example; they do not change the verification harness's own limits.", "The figures above are adoption defaults for this example. They do not change the current four-hop harness constants."), "lacks \"The figures above"},
			{"claim-m5", appendToSection("Start here", "M5 passed on this example."), "forbidden \"M5 passed\""},
			{"superseded-exercise", replaceOnce("never the superseded first exercise", "including the first exercise"), "lacks \"the optional Cursor evidence"},
			{"remove-separate-scenario", replaceOnce("The four-hop sample in which Cursor reviews code is a separate scenario, not exercised by this selection.", ""), "lacks \"The four-hop sample"},
			{"no-folder-source", replaceOnce("An ordinary folder is a valid workspace source.", "A git repository is required."), "lacks \"An ordinary folder"},
		})
		// A deleted variable definition or a mutated argv fails, even though the
		// name is still mentioned elsewhere in the document.
		for _, c := range []struct{ name, old, repl, want string }{
			{"deleted-variable", "| `MANUALS` | The absolute directory holding this copy on the sidecar |\n", "", "registration variable MANUALS is not defined"},
			{"missing-trust", `--model gpt-6-astra --effort low --concurrency 1 --timeout 10m --plane "$PLANE" --ca "$CA"`, `--model gpt-6-astra --effort low --concurrency 1 --timeout 10m --plane "$PLANE"`, "missing required flag --ca"},
			{"wrong-node-variable", `add triage --name triage --node "$CODEX_NODE"`, `add triage --name triage --node "$CLAUDE_NODE"`, "word 7 is \"$CLAUDE_NODE\", want \"$CODEX_NODE\""},
			{"wrong-manual-suffix", `"$MANUALS/triage-runbook.md"`, `"$MANUALS/triage-instruction.md"`, "word 13 is"},
			{"unquoted-variable", `--plane "$PLANE" --ca "$CA"` + "\ncallsheet role add design-reviewer", `--plane $PLANE --ca "$CA"` + "\ncallsheet role add design-reviewer", "unquoted variable $PLANE"},
			{"default-note-removed", "`default 2h`", "a default", "registration default timeout note"},
		} {
			t.Run(c.name, func(t *testing.T) {
				mutated := replaceOnce(c.old, c.repl)(t, readme)
				if err := checkRegistration(mutated, canonicalRoles); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want one containing %q", err, c.want)
				}
			})
		}
	})
	t.Run("chapter", func(t *testing.T) {
		runGuideMutations(t, chapter, checkGuideExampleChapter, []readmeMutation{
			{"claim-m5-complete", appendToSection("What the evidence shows", "M5 is complete."), "forbidden \"M5 is complete\""},
			{"harness-is-dependency", replaceOnce("That harness is not a user dependency.", "That harness is a user dependency."), "lacks \"That harness"},
			{"docs-only-happened", replaceOnce("Neither the offline tests nor the 12a exercise is proof that a docs-only walkthrough already happened.", "The 12a exercise is the docs-only walkthrough."), "lacks \"Neither the offline"},
			{"prefilled-checklist", replaceOnce("| Selected roles |  |", "| Selected roles | the five defaults |"), "row \"Selected roles\" is prefilled"},
			{"checklist-row-removed", replaceOnce("| Every snag and its disposition |  |\n", ""), "M5 checklist rows"},
			{"owner-yes-removed", replaceOnce("with the owner's explicit yes before coding", "and code at once"), "lacks \"with the owner's explicit yes"},
			{"canonical-removed", replaceOnce("its README is the canonical set of instructions", "this chapter is the canonical set"), "lacks \"its README"},
			{"section-link-removed", replaceOnce("(../examples/dev-flow/README.md#light-walkthrough)", "(../examples/dev-flow/README.md)"), "missing link ../examples/dev-flow/README.md#light-walkthrough"},
		})
	})
}

// ---- FP-7: the finalized catalog.

func checkGuideCatalogProse(md, adapters string) error {
	d, err := parseReadme(md)
	if err != nil {
		return err
	}
	if d.title != "CLI support catalog" {
		return fmt.Errorf("catalog: title %q", d.title)
	}
	var names []string
	for _, s := range d.sections {
		names = append(names, s.name)
	}
	var errs []error
	if want := []string{"Catalog contract", "Claude Code", "OpenAI Codex", "Grok Build", "Cursor Agent", "Worker selection policy", "Qualification handoff"}; !slices.Equal(names, want) {
		errs = append(errs, fmt.Errorf("catalog: H2 headings %q, want %q", names, want))
	}
	errs = append(errs, needAll("catalog", md,
		"Finalized for v1: internally consistent current policy, explicit dated evidence and known limits, complete links and tested machine data. It does not mean that every fact is VERIFIED.",
		"Four version concepts stay distinct: the historical catalog `version`, the runtime `adapter.MinimumVersion`, the version actually installed and observed, and the exact `coordinator_version`.",
		"It is not the runtime `adapter.MinimumVersion` that decides eligibility",
		"Worker versions are eligible at or above the runtime `adapter.MinimumVersion`, not the historical catalog `version`",
		// D3's retained complete sentences and clause.
		"It is not a worker qualification: the runtime minimum version is 2.1.299 (Claude Code) (the historical observation 2.1.285 is not that minimum; a newer worker is eligible by the runtime minimum, not by this identity).",
		"It is not a worker qualification: the runtime minimum version is codex-cli 0.162.1 (the historical observation 0.159.0 is not that minimum; a newer worker is eligible by the runtime minimum, not by this identity).",
		"a same-date version with another hash being equal and accepted",
		"Historical captured recipe (iteration 08, not current argv): `claude -p", "Historical captured recipe (iteration 08, not current argv): `codex -a never", "Historical captured recipe (iteration 11, not current argv): `grok",
		"owner-recorded, not a fresh upstream verification",
		"`docs/maintainers/real-e2e.md#cursor-coder-qualification` is complete for the recorded binary identity and selection",
		"For Grok, current Callsheet already requests auto, and useful tool execution under it remains a separate unverified claim",
		"Non-Linux execution is not qualified"))
	errs = append(errs, forbidAll("catalog", md, "first-wave cli support catalog", "reserved for iteration 07", "not functional in the skeleton", "the worker's minimum version baseline",
		"at or above the catalog `version`, the minimum", "before changing `dontAsk`", "grok runs the measured", "encoding is unverified until", "all operating systems are qualified", "every os is qualified", "worker execution is universally refused"))
	errs = append(errs, needAll("real adapters", adapters, "complete for the recorded binary identity and selection", "is below the current", "macOS is **not** qualified."))
	errs = append(errs, forbidAll("real adapters", adapters, "are above their minimums", "unverified against a live CLI until", "all operating systems are qualified"))
	return errors.Join(errs...)
}

// checkGuideCatalogEntries pins the machine facts that must not be promoted.
func checkGuideCatalogEntries(root string, entries []catalog.Entry) error {
	if err := catalog.Validate(root, entries); err != nil {
		return err
	}
	for _, e := range entries {
		for _, key := range []string{"sandbox_macos", "exit_codes"} {
			if e.Facts[key].Status != catalog.Unverified {
				return fmt.Errorf("catalog: %s.%s was promoted to %s", e.ID, key, e.Facts[key].Status)
			}
		}
		if want, _ := adapter.MinimumVersion(e.ID); want == "" {
			return fmt.Errorf("catalog: %s has no runtime minimum", e.ID)
		}
		q := slices.IndexFunc(adapter.Qualifications(), func(q adapter.Qualification) bool { return q.ID == e.ID })
		if q < 0 || adapter.Qualifications()[q].Version != e.Version {
			return fmt.Errorf("catalog: %s version %q is not its historical observation", e.ID, e.Version)
		}
	}
	return nil
}

// TestGuideCatalog (FP-7).
func TestGuideCatalog(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	md, adapters := string(repoFile(t, guideCatalogMD)), string(repoFile(t, guideAdapters))
	check := func(md string) error { return checkGuideCatalogProse(md, adapters) }
	if err := check(md); err != nil {
		t.Fatalf("shipped catalog: %v", err)
	}
	entries, err := catalog.Load(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogJSONPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGuideCatalogEntries(root, entries); err != nil {
		t.Fatalf("shipped machine catalog: %v", err)
	}
	if err := catalog.CheckDocLinks(filepath.Join(root, filepath.FromSlash(guideCatalogMD))); err != nil {
		t.Fatal(err)
	}
	runGuideMutations(t, md, check, []readmeMutation{
		{"conflate-minimum", replaceOnce("It is not the runtime `adapter.MinimumVersion` that decides eligibility (Cursor's two values happen to be equal), and it is neither", "It is the worker's minimum version baseline, and it is neither"), "lacks \"It is not the runtime"},
		{"restore-policy-equation", replaceOnce("at or above the runtime `adapter.MinimumVersion`, not the historical catalog `version`", "at or above the catalog `version`, the minimum"), "lacks \"Worker versions are eligible"},
		{"remove-claude-minimum", replaceOnce("the runtime minimum version is 2.1.299 (Claude Code) ", "the minimum is unstated "), "lacks \"It is not a worker qualification: the runtime minimum version is 2.1.299"},
		{"remove-codex-minimum", replaceOnce("the runtime minimum version is codex-cli 0.162.1 ", "the minimum is unstated "), "lacks \"It is not a worker qualification: the runtime minimum version is codex-cli 0.162.1"},
		{"remove-same-date-clause", replaceOnce(", a same-date version with another hash being equal and accepted", ""), "lacks \"a same-date version"},
		{"promote-historical", replaceOnce("Historical captured recipe (iteration 08, not current argv): `claude -p", "Recommended recipe: `claude -p"), "lacks \"Historical captured recipe (iteration 08, not current argv): `claude -p\""},
		{"elevate-owner-recorded", replaceOnce("(owner-recorded, not a fresh upstream verification)", "(verified upstream)"), "lacks \"owner-recorded"},
		{"all-oses-qualified", appendToSection("Qualification handoff", "All operating systems are qualified."), "forbidden \"all operating systems are qualified\""},
		{"non-linux-qualified", replaceOnce("Non-Linux execution is not qualified", "Non-Linux execution is qualified"), "lacks \"Non-Linux execution"},
		{"retitled", replaceOnce("# CLI support catalog\n", "# First-wave CLI support catalog\n"), "catalog: title"},
		{"finalized-means-verified", replaceOnce("It does not mean that every fact is VERIFIED.", "Every fact is VERIFIED."), "lacks \"Finalized for v1"},
		{"restore-reserved-flags", appendToSection("Claude Code", "The Callsheet flags are reserved for iteration 07, not functional in the skeleton."), "forbidden \"reserved for iteration 07\""},
		{"restore-dontask-handoff", replaceOnce("For Grok, current Callsheet already requests auto, and useful tool execution under it remains a separate unverified claim that needs its own owner probe.", "For Grok, qualify useful tool execution separately before changing `dontAsk`."), "lacks \"For Grok, current"},
		{"stale-cursor-evidence", replaceOnce("is complete for the recorded binary identity and selection, on Cursor", "awaits the slice 3a diagnostic, on Cursor"), "lacks \"`docs/maintainers/real-e2e.md#cursor-coder-qualification`"},
		{"section-renamed", replaceOnce("## Worker selection policy\n", "## Selection\n"), "H2 headings"},
	})
	if err := checkGuideCatalogProse(md, strings.Replace(adapters, "is below\nthe current", "are above their minimums, below\nthe current", 1)); err == nil || !strings.Contains(err.Error(), "are above their minimums") {
		t.Errorf("a restored below-minimum smoke claim: err = %v", err)
	}
	for name, edit := range map[string]func([]catalog.Entry){
		"rewrite-status": func(es []catalog.Entry) {
			f := es[0].Facts["sandbox_macos"]
			f.Status = catalog.Verified
			es[0].Facts["sandbox_macos"] = f
		},
		"historical-to-min": func(es []catalog.Entry) { min, _ := adapter.MinimumVersion(es[0].ID); es[0].Version = min },
	} {
		t.Run(name, func(t *testing.T) {
			es, err := catalog.Load(filepath.Join(root, filepath.FromSlash(mcpqual.CatalogJSONPath)))
			if err != nil {
				t.Fatal(err)
			}
			edit(es)
			if err := checkGuideCatalogEntries(root, es); err == nil {
				t.Fatal("the machine catalog mutation was accepted")
			}
		})
	}
}

// ---- FP-8: navigation.

// guideGitHubSlug is a heading's rendered fragment, which the simple resolver's
// headingSlugs does not model for punctuation.
func guideGitHubSlug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// guideAnchored appends synthetic headings for the rendered slugs and explicit
// anchors of md, so that the shared resolver can check them.
func guideAnchored(md string) string {
	var extra []string
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		if fenceLine(line) {
			inFence = !inFence
			continue
		}
		if m := readmeHeading.FindStringSubmatch(line); m != nil && !inFence {
			extra = append(extra, "## "+guideGitHubSlug(m[1]))
		}
	}
	for _, m := range guideAnchorTag.FindAllStringSubmatch(md, -1) {
		extra = append(extra, "## "+m[1])
	}
	return md + "\n" + strings.Join(extra, "\n") + "\n"
}

// guideUserDocs are the documents whose every link and fragment FP-8 resolves.
func guideUserDocs() []string {
	docs := []string{"README.md", quickstartDoc, deployDoc, "docs/coordinator.md", "docs/workspaces.md", guideAdapters, guideCatalogMD, guideExampleDoc}
	docs = append(docs, guideDir+"index.md")
	for _, c := range guideChapters {
		docs = append(docs, guideDir+c.file)
	}
	return docs
}

// checkGuideNavigation is FP-8's validator over docs (path to text) and scan
// (every user Markdown document, for placeholders).
func checkGuideNavigation(docs, scan map[string]string, look repoLookup) error {
	anchored := func(rel string) ([]byte, bool) {
		if md, ok := docs[rel]; ok {
			return []byte(guideAnchored(md)), true
		}
		b, ok := look(rel)
		if !ok {
			return nil, false
		}
		return []byte(guideAnchored(string(b))), true
	}
	var errs []error
	for _, src := range slices.Sorted(maps.Keys(docs)) {
		errs = append(errs, checkSourceLinks(src, guideAnchored(docs[src]), anchored))
		if strings.HasPrefix(src, guideDir) || src == guideExampleDoc || src == "README.md" {
			errs = append(errs, checkPrivateText(src, docs[src]))
		}
	}
	for _, src := range slices.Sorted(maps.Keys(scan)) {
		if strings.Contains(strings.ToLower(scan[src]), "coming in v1") {
			errs = append(errs, fmt.Errorf("%s: a coming-in-v1 placeholder remains", src))
		}
	}
	readmeDocs, err := sectionOf(docs["README.md"], "Documentation")
	if err != nil {
		return err
	}
	if !strings.Contains(readmeDocs, readmeGuideRow) {
		errs = append(errs, errors.New("README: no live user guide row"))
	}
	next, err := sectionOf(docs[quickstartDoc], "Next steps")
	if err != nil {
		return err
	}
	errs = append(errs, wantLinks("quickstart next steps", next, "guide/index.md", "guide/example.md"))
	verify, err := sectionOf(docs[deployDoc], "Verify the deployment")
	if err != nil {
		return err
	}
	errs = append(errs, wantLinks("deploy next reading", verify, "guide/index.md", "guide/example.md"))
	errs = append(errs, wantLinks("coordinator backlink", docs["docs/coordinator.md"], "guide/index.md", "guide/coordinator-setup.md"))
	errs = append(errs, wantLinks("workspaces backlink", docs["docs/workspaces.md"], "guide/index.md", "guide/workspaces.md"))
	errs = append(errs, wantLinks("real adapters backlink", docs[guideAdapters], "guide/index.md", "guide/worker-setup.md"))
	return errors.Join(errs...)
}

// guideScanDocs reads README and the user Markdown under docs, excluding the
// maintainer documents and the maintainers' exercise assets.
func guideScanDocs(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{"README.md": string(repoFile(t, "README.md"))}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && (rel == "docs/maintainers" || rel == "docs/examples/real-e2e") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(rel, ".md") {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestGuideNavigation (FP-8): every link and fragment of the new and edited user
// documents resolves, the entry and next-step navigation is live, no placeholder
// remains, and the README stays inside its cap.
func TestGuideNavigation(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	look := func(rel string) ([]byte, bool) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		st, err := os.Stat(p)
		switch {
		case err != nil:
			return nil, false
		case st.IsDir():
			// A directory destination is a valid evidence link with no fragment.
			return nil, true
		}
		b, err := os.ReadFile(p)
		return b, err == nil
	}
	docs := map[string]string{}
	for _, rel := range guideUserDocs() {
		docs[rel] = string(repoFile(t, rel))
	}
	scan := guideScanDocs(t, root)
	if err := checkGuideNavigation(docs, scan, look); err != nil {
		t.Fatalf("shipped documents: %v", err)
	}
	readme := docs["README.md"]
	n := len(strings.Fields(readme))
	if n > readmeWordCap {
		t.Fatalf("README: %d words, cap %d", n, readmeWordCap)
	}
	t.Logf("README: %d words (strings.Fields), margin %d to %d", n, readmeWordCap-n, readmeWordCap)
	if err := checkStructureAndBudget(padWords(t, readme, readmeWordCap+1)); err == nil || !strings.Contains(err.Error(), "budget: 751 words") {
		t.Errorf("751 words: err = %v", err)
	}
	for _, c := range []struct{ name, rel, old, repl, want string }{
		{"restore-guide-placeholder", "README.md", readmeGuideRow, "- `docs/guide/index.md`: the user guide; coming in v1.", "no live user guide row"},
		{"break-anchor", guideDir + "workspaces.md", "../workspaces.md#ignore-rules", "../workspaces.md#ignoring", "fragment #ignoring matches no heading"},
		{"break-local-anchor", guideDir + "worker-setup.md", "roles-and-manuals.md#check-readiness", "roles-and-manuals.md#readiness", "fragment #readiness matches no heading"},
		{"break-file", guideDir + "index.md", "(concepts.md)", "(concept.md)", "docs/guide/concept.md does not exist"},
		{"point-into-design", guideDir + "index.md", "(../quickstart.md)", "(../../design/roadmap.md)", "ignored design tree"},
		{"quickstart-next-removed", quickstartDoc, "(guide/index.md)", "(deploy.md)", "quickstart next steps: missing link guide/index.md"},
		{"quickstart-example-removed", quickstartDoc, "(guide/example.md)", "(deploy.md)", "quickstart next steps: missing link guide/example.md"},
		{"deploy-next-removed", deployDoc, "(guide/example.md)", "(quickstart.md)", "deploy next reading: missing link guide/example.md"},
		{"coordinator-backlink-removed", "docs/coordinator.md", "(guide/coordinator-setup.md)", "(deploy.md)", "coordinator backlink: missing link guide/coordinator-setup.md"},
		{"workspaces-backlink-removed", "docs/workspaces.md", "(guide/workspaces.md)", "(deploy.md)", "workspaces backlink: missing link guide/workspaces.md"},
		{"private-path", guideDir + "concepts.md", "## Components\n", "## Components\n\nSee /Users/someone.\n", "private path sentinel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mutated := maps.Clone(docs)
			mutated[c.rel] = replaceOnce(c.old, c.repl)(t, docs[c.rel])
			if mutated[c.rel] == docs[c.rel] {
				t.Fatal("the mutation changed nothing")
			}
			err := checkGuideNavigation(mutated, scan, look)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
	t.Run("guide-destination-missing", func(t *testing.T) {
		without := maps.Clone(docs)
		delete(without, guideDir+"concepts.md")
		err := checkGuideNavigation(without, scan, func(rel string) ([]byte, bool) {
			if rel == guideDir+"concepts.md" {
				return nil, false
			}
			return look(rel)
		})
		if err == nil || !strings.Contains(err.Error(), "docs/guide/concepts.md does not exist") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("placeholder-in-scanned-document", func(t *testing.T) {
		mutated := maps.Clone(scan)
		mutated["docs/examples/dev-flow/coder-runbook.md"] += "\nThe harness notes are coming in v1.\n"
		if err := checkGuideNavigation(docs, mutated, look); err == nil || !strings.Contains(err.Error(), "a coming-in-v1 placeholder remains") {
			t.Fatalf("err = %v", err)
		}
	})
}

// ---- FP-9: worker setup.

func checkGuideWorkerSetup(md, adapters, catalogMD string) error {
	const file = "worker-setup.md"
	errs := []error{checkGuideFrame(guideChapterOf(file), md), checkNoGuideFences(file, md),
		needAll("worker", stripInlineLinks(md),
			"Login is an operator prerequisite, not a Callsheet operation.", "Current auto policies are not OS confinement.",
			"Grok and Cursor prompts are passed in argv and are visible to process inspection, with the product's 32 KiB prompt bound.", "Never include credentials in goals.",
			"An authentication or permission refusal stops the operation for owner diagnosis, without fallback, settings edits or broad permission bypasses.",
			"Claude and Codex are eligible with an unverified macOS runtime warning; Grok and Cursor execution is refused.", "A flag being accepted is not execution eligibility, and Linux evidence does not certify macOS.",
			"owner-recorded, not a fresh upstream verification", "workspace-write is the selected vendor policy", "The older `-a never` argv is historical.",
			"that proves the old observation only", "Useful tool execution under current auto remains unverified.",
			guideGrokAssumption, guideGrokProbe, "Prefer separate simple tool commands, not shell bypass flags.", "The default Claude and Codex example does not depend on this assumption.",
			"The historical `--force --trust` candidate is not a recipe to recommend.", "is complete for the recorded binary identity and selection",
			"A2 remains unobserved: no approval-needing action occurred under `--auto-review`.", "no general approval-behavior or confinement qualification",
			"Readiness checks invocability and version, and does not prove login or useful work.", "The first authorized small task must inspect both the requested result and the exit.",
			"Cursor self-updated to 2026.10.22-27dcf5f during the full child's coder run", "Record before and after versions and any changed identity, and stop or requalify the affected work.",
			"Callsheet does not claim to detect every mid-task replacement"),
		wantLinks("worker", md, "../real-adapters.md#selection-and-versions", "../real-adapters.md#enabling-a-worker", "../real-adapters.md#wave-2-grok-and-cursor",
			"../maintainers/real-e2e.md#cursor-coder-qualification", "../maintainers/real-e2e.md#superseded-first-cursor-qualification-and-exercise",
			"../deploy.md#authenticate-the-worker", "../deploy.md#security-posture", "roles-and-manuals.md#check-readiness"),
		forbidAll("worker", md, "grok cannot run compound shell commands", "grok headless cannot", "compound shell commands are qualified", "confines the worker", "provides confinement", "proves login", "proves authentication",
			"dangerously", "--always-approve", "--yolo", "credentials are stored in", "api_key", "cursor is qualified"),
	}
	steps := []string{"1. Install it using the vendor's instructions.", "2. Authenticate as the actual dedicated sidecar user.", "3. Ensure its credentials and configuration are available in that account's service environment.",
		"4. Check its version against the linked", "5. Inspect its effective permissions.", "6. Pass an explicit absolute executable path on every sidecar start", "7. Confirm the node is online and the role is ready."}
	for _, v := range []struct{ section, id, flag, exe, anchor, enabling string }{
		{"Claude Code", adapter.ClaudeID, "--claude-adapter", "`claude`", "claude-code", "../real-adapters.md#enabling-a-worker"},
		{"OpenAI Codex", adapter.CodexID, "--codex-adapter", "`codex`", "openai-codex", "../real-adapters.md#enabling-a-worker"},
		{"Grok Build", adapter.GrokID, "--grok-adapter", "`grok`", "grok-build", "../real-adapters.md#wave-2-grok-and-cursor"},
		{"Cursor Agent", adapter.CursorID, "--cursor-adapter", "`cursor-agent`", "cursor-agent", "../real-adapters.md#wave-2-grok-and-cursor"},
	} {
		body, err := sectionOf(md, v.section)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, inOrder("worker "+v.section, stripInlineLinks(body), steps...))
		errs = append(errs, needAll("worker "+v.section, body, "Adapter flag `"+v.flag+"`, executable "+v.exe+"."))
		errs = append(errs, wantLinks("worker "+v.section, body, "../real-adapters.md#selection-and-versions", v.enabling, "../support-catalog.md#"+v.anchor))
		// The reference tables stay authoritative, and they agree with the code.
		minVersion, ok := adapter.MinimumVersion(v.id)
		if !ok || !strings.Contains(adapters, "`"+minVersion+"`") {
			errs = append(errs, fmt.Errorf("real adapters: lacks the runtime minimum of %s", v.id))
		}
		// The guide links current minima rather than copying them.
		if strings.Contains(md, minVersion) {
			errs = append(errs, fmt.Errorf("worker: copies the runtime minimum %s", minVersion))
		}
		darwin, linux := adapter.ValidateWorkerPosture(v.id, "darwin"), adapter.ValidateWorkerPosture(v.id, "linux")
		if linux != nil {
			errs = append(errs, fmt.Errorf("worker: %s is not eligible on Linux: %v", v.id, linux))
		}
		linuxOnly := strings.Contains(body, "Linux only.")
		if linuxOnly != (darwin != nil) {
			errs = append(errs, fmt.Errorf("worker %s: Linux-only is %v, posture on darwin %v", v.section, linuxOnly, darwin))
		}
	}
	for _, q := range adapter.Qualifications() {
		if strings.Contains(md, q.Version) {
			errs = append(errs, fmt.Errorf("worker: names the historical version %s", q.Version))
		}
	}
	errs = append(errs, needAll("real adapters", adapters, "--permission-mode auto --permission-prompts none", "exec --approve-for-me", "-p --trust --auto-review --output-format json --model MODEL-EFFORT <composed-prompt>"))
	errs = append(errs, needAll("catalog", catalogMD, "owner-recorded, not a fresh upstream verification", "Useful headless tool execution under auto is UNVERIFIED"))
	return errors.Join(errs...)
}

// TestGuideWorkerSetup (FP-9).
func TestGuideWorkerSetup(t *testing.T) {
	md, adapters, catalogMD := guideDoc(t, "worker-setup.md"), string(repoFile(t, guideAdapters)), string(repoFile(t, guideCatalogMD))
	check := func(md string) error { return checkGuideWorkerSetup(md, adapters, catalogMD) }
	if err := check(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	claudeMin, _ := adapter.MinimumVersion(adapter.ClaudeID)
	runGuideMutations(t, md, check, []readmeMutation{
		{"remove-vendor", replaceOnce("## Grok Build\n", "## Grok\n"), "H2 headings"},
		{"remove-authentication", inSection("OpenAI Codex", "2. Authenticate as the actual dedicated sidecar user.\n", ""), "lacks step \"2. Authenticate"},
		{"remove-service-user", inSection("Cursor Agent", "3. Ensure its credentials and configuration are available in that account's service environment.\n", ""), "lacks step \"3. Ensure"},
		{"remove-executable", inSection("Cursor Agent", "executable `cursor-agent`", "executable `cursor`"), "lacks \"Adapter flag `--cursor-adapter`"},
		{"remove-flag", inSection("Claude Code", "Adapter flag `--claude-adapter`", "Adapter flag `--claude`"), "lacks \"Adapter flag `--claude-adapter`"},
		{"historical-minimum", appendToSection("Claude Code", "The minimum is 2.1.285 (Claude Code)."), "names the historical version 2.1.285 (Claude Code)"},
		{"copied-minimum", appendToSection("Claude Code", "The minimum is "+claudeMin+"."), "copies the runtime minimum"},
		{"probe-proves-login", replaceOnce("and does not prove login or useful work", "which proves login"), "lacks \"Readiness checks"},
		{"grok-macos", replaceOnce("Grok and Cursor execution is refused.", "Grok and Cursor are eligible too."), "lacks \"Claude and Codex are eligible"},
		{"grok-not-linux-only", inSection("Grok Build", "Linux only.", "Linux and macOS."), "Linux-only is false"},
		{"claude-linux-only", inSection("Claude Code", "executable `claude`.", "executable `claude`. Linux only."), "Linux-only is true"},
		{"cursor-a2-observed", replaceOnce("A2 remains unobserved: no approval-needing action occurred under `--auto-review`.", "A2 was observed and passed."), "lacks \"A2 remains unobserved"},
		{"invent-confinement", appendToSection("Before enabling a worker", "The auto policy confines the worker."), "forbidden \"confines the worker\""},
		{"auto-is-confinement", replaceOnce("Current auto policies are not OS confinement.", "Current auto policies confine the OS."), "lacks \"Current auto policies"},
		{"remove-update-caveat", replaceOnce("Cursor self-updated to 2026.10.22-27dcf5f during the full child's coder run", "Cursor updates are rare"), "lacks \"Cursor self-updated"},
		{"remove-assumption", replaceOnce(guideGrokAssumption, ""), "lacks \"ASSUMPTION (unverified)"},
		{"drop-assumption-label", replaceOnce("ASSUMPTION (unverified): The owner", "The owner"), "lacks \"ASSUMPTION (unverified)"},
		{"drop-attribution", replaceOnce("The owner reported during 12a that Grok headless could not run", "Grok headless could not run"), "lacks \"ASSUMPTION (unverified)"},
		{"assumption-to-rule", replaceOnce(guideGrokAssumption, "Grok cannot run compound shell commands."), "forbidden \"grok cannot run compound shell commands\""},
		{"assumption-qualified", replaceOnce(guideGrokAssumption, "Compound shell commands are qualified on Grok."), "forbidden \"compound shell commands are qualified\""},
		{"remove-probe", replaceOnce(guideGrokProbe, "tries it"), "lacks \"tests simple versus compound"},
		{"bypass-flag", appendToSection("Grok Build", "Pass --always-approve when it refuses."), "forbidden \"--always-approve\""},
		{"permission-fallback", replaceOnce("without fallback, settings edits or broad permission bypasses", "with a broader permission fallback"), "lacks \"An authentication or permission refusal"},
		{"promoted-owner-recorded", replaceOnce("owner-recorded, not a fresh upstream verification", "verified upstream"), "lacks \"owner-recorded"},
		{"credentials-in-goals", replaceOnce("Never include credentials in goals.", "Goals may carry credentials."), "lacks \"Never include credentials"},
		{"link-removed", inSection("Cursor Agent", "[Wave 2](../real-adapters.md#wave-2-grok-and-cursor)", "Wave 2"), "worker Cursor Agent: missing link ../real-adapters.md#wave-2-grok-and-cursor"},
	})
	// A posture or reference value drifting from the code fails.
	if min, ok := adapter.MinimumVersion(adapter.GrokID); !ok {
		t.Fatal("no Grok minimum")
	} else if err := checkGuideWorkerSetup(md, strings.ReplaceAll(adapters, min, "grok 0.0.1 (0) [stable]"), catalogMD); err == nil || !strings.Contains(err.Error(), "lacks the runtime minimum of grok") {
		t.Errorf("drifted minimum: err = %v", err)
	}
}

// ---- FP-10: private-path correction.

// guideClaudeSandbox is the corrected value, whole: any edit to it fails here,
// and the full catalog delta proof is TestWorkerCatalogPathRedaction's.
const guideClaudeSandbox = "On the recorded Linux host with 2.1.285 and the recorded settings (sandbox enabled, strictMode and failIfUnavailable true, allowUnsandboxedCommands false, a filesystem write allowlist including /tmp and the recorded checkout parent), `--permission-prompts none` did not widen the observed write outcomes versus the baseline without it: the in-directory `canary-ok.txt` write succeeded (bytes `permitted`) and the home sentinel stayed absent in both. `claude sandbox status` reported available=false and installed=false with a Windows-specific reason; the canary outcomes, not that status, are the observation."

// guideLegacyPath rebuilds the removed path from components, so this test does
// not reintroduce the literal it forbids.
func guideLegacyPath() string { return "/" + strings.Join([]string{"opt", "gitspace"}, "/") }

func checkGuidePrivatePath(md, jsonText, envTest string) error {
	var errs []error
	for name, text := range map[string]string{"catalog": md, "machine catalog": jsonText, "environment test": envTest} {
		if strings.Contains(text, guideLegacyPath()) {
			errs = append(errs, fmt.Errorf("%s: holds the private path", name))
		}
	}
	errs = append(errs, needAll("catalog", md, "a filesystem write allowlist including `/tmp` and "+guideRedaction+"; the home sentinel directory was outside it"))
	errs = append(errs, needAll("environment test", envTest, "// The owner's shape: the checkout lies under a supplied protected root; so may the home.",
		"frame, so the checkout's repository is inside its root"))
	entries, err := catalog.Decode([]byte(jsonText))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	at := slices.IndexFunc(entries, func(e catalog.Entry) bool { return e.ID == adapter.ClaudeID })
	if at < 0 {
		return errors.Join(append(errs, errors.New("machine catalog: no claude entry"))...)
	}
	e := entries[at]
	f := e.Facts["sandbox_linux"]
	if strings.Count(f.Value, guideRedaction) != 1 {
		errs = append(errs, fmt.Errorf("machine catalog: %d descriptive replacements, want 1", strings.Count(f.Value, guideRedaction)))
	}
	if f.Value != guideClaudeSandbox {
		errs = append(errs, errors.New("machine catalog: claude.sandbox_linux value changed"))
	}
	if f.Status != catalog.Verified || f.VerificationIteration != catalog.Owner(e.ID, "sandbox_linux") || len(f.Evidence) == 0 || e.Version != "2.1.285 (Claude Code)" {
		errs = append(errs, fmt.Errorf("machine catalog: claude status %q, owner %q, version %q, %d evidence paths", f.Status, f.VerificationIteration, e.Version, len(f.Evidence)))
	}
	return errors.Join(errs...)
}

// checkGuideEvidenceRetained verifies the immutable captures against their own
// manifests: their bytes, which may hold recorded source paths, are never edited.
func checkGuideEvidenceRetained(root string) error {
	for _, dir := range []string{"tests/testdata/real-adapters/linux-2026-09-30", "tests/testdata/real-adapters/linux-2026-10-04"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "manifest.json"))
		if err != nil {
			return err
		}
		var m struct {
			Files []struct {
				Destination, SHA256 string
				Bytes               int
			} `json:"files"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if len(m.Files) == 0 {
			return fmt.Errorf("%s: an empty manifest", dir)
		}
		for _, f := range m.Files {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Destination)))
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			if len(data) != f.Bytes || hex.EncodeToString(sum[:]) != f.SHA256 {
				return fmt.Errorf("%s: captured bytes changed", f.Destination)
			}
		}
	}
	return nil
}

// TestGuidePrivatePathCorrection (FP-10).
func TestGuidePrivatePathCorrection(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	md, jsonText, envTest := string(repoFile(t, guideCatalogMD)), string(repoFile(t, mcpqual.CatalogJSONPath)), string(repoFile(t, guideEnvTest))
	if err := checkGuidePrivatePath(md, jsonText, envTest); err != nil {
		t.Fatalf("shipped text: %v", err)
	}
	if err := checkGuideEvidenceRetained(root); err != nil {
		t.Fatal(err)
	}
	// A status, evidence, version or value edit is applied to the decoded JSON text.
	const sandboxKey = `"sandbox_linux": {
        "status": "VERIFIED",
        "value": "On the recorded Linux host with 2.1.285`
	for _, c := range []struct{ name, doc, old, repl, want string }{
		{"literal-in-catalog", "md", "and " + guideRedaction + ";", "and `" + guideLegacyPath() + "`;", "catalog: holds the private path"},
		{"literal-in-json", "json", "/tmp and " + guideRedaction, "/tmp and " + guideLegacyPath(), "machine catalog: holds the private path"},
		{"literal-in-comment", "env", "the checkout lies under a supplied protected root", "the checkout (" + guideLegacyPath() + "/callsheet) lies under a supplied protected root", "environment test: holds the private path"},
		{"missed-replacement-catalog", "md", "and " + guideRedaction + ";", "and a parent;", "catalog: lacks"},
		{"missed-replacement-json", "json", "/tmp and " + guideRedaction, "/tmp and a parent", "0 descriptive replacements"},
		{"second-replacement", "json", "the home sentinel stayed absent in both. `claude sandbox status`", "the home sentinel stayed absent in both, " + guideRedaction + ". `claude sandbox status`", "2 descriptive replacements"},
		{"value-edit", "json", "with a Windows-specific reason; the canary outcomes, not that status, are the observation.", "with a Windows-specific reason.", "claude.sandbox_linux value changed"},
		{"status-change", "json", sandboxKey, strings.Replace(sandboxKey, "VERIFIED", "UNVERIFIED", 1), "claude status \"UNVERIFIED\""},
		{"version-change", "json", `"version": "2.1.285 (Claude Code)"`, `"version": "2.1.299 (Claude Code)"`, "version \"2.1.299 (Claude Code)\""},
		{"comment-frame-removed", "env", "frame, so the checkout's repository is inside its root", "frame", "environment test: lacks"},
	} {
		t.Run(c.name, func(t *testing.T) {
			docs := map[string]string{"md": md, "json": jsonText, "env": envTest}
			docs[c.doc] = replaceOnce(c.old, c.repl)(t, docs[c.doc])
			err := checkGuidePrivatePath(docs["md"], docs["json"], docs["env"])
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
	t.Run("evidence-removed", func(t *testing.T) {
		evidence, err := catalog.Decode([]byte(jsonText))
		if err != nil {
			t.Fatal(err)
		}
		f := evidence[0].Facts["sandbox_linux"]
		f.Evidence = nil
		evidence[0].Facts["sandbox_linux"] = f
		b, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkGuidePrivatePath(md, string(b), envTest); err == nil || !strings.Contains(err.Error(), "0 evidence paths") {
			t.Fatalf("err = %v", err)
		}
	})
}

// ---- FP-11: troubleshooting.

func checkGuideTroubleshooting(md string) error {
	const file = "troubleshooting.md"
	var errs []error
	errs = append(errs, checkGuideFrame(guideChapterOf(file), md), checkNoGuideFences(file, md),
		needAll("troubleshooting", md, guideGrokAssumption, guideGrokProbe),
		forbidAll("troubleshooting", md, "grok cannot run compound shell commands", "chmod", "--insecure", "skip verification", "disable verification", "dispatch it again", "redispatch", "--yolo", "--force", "login command"))
	for _, c := range []struct {
		section, symptom string
		wants            []string
		link             string
	}{
		{"Connection and readiness", "Trust error", []string{"The URL against the server SANs, a readable CA and the clock", "Verification stays on"}, "../deploy.md#connection-not-trusted"},
		{"Connection and readiness", "Offline node", []string{"Start the sidecar, wait for its heartbeat, then retry `role add`"}, "roles-and-manuals.md#check-readiness"},
		{"Connection and readiness", "Unready role", []string{"The adapter path", "both manuals are readable"}, "../real-adapters.md#selection-and-versions"},
		{"Authentication and permission failures", "Login or permission failure", []string{"The actual service user and the vendor policy", "with no permission bypass"}, "worker-setup.md#before-enabling-a-worker"},
		{"Authentication and permission failures", "Grok compound command", []string{"separate simple tool commands"}, "worker-setup.md#grok-build"},
		{"Task and wait results", "Successful exit without the work", []string{"The final output plus the artifacts"}, ""},
		{"Task and wait results", "Null final message", []string{"The extraction diagnostic", "Null differs from an empty answer"}, "../real-adapters.md#how-a-task-runs"},
		{"Task and wait results", "Lost dispatch response", []string{"`task_ls`", "Never dispatch a duplicate"}, ""},
		{"Task and wait results", "Silent long wait", []string{"The saved task status and any plane or node outage", "Do not dispatch a duplicate"}, "dispatch-and-wait.md#recover-safely"},
		{"Workspace delivery", "Failed publication", []string{"A failed publication is not delivery"}, "../workspaces.md#task-results-by-task-id"},
		{"Workspace delivery", "Stale instance", []string{"do not overwrite blindly"}, "../workspaces.md#errors"},
		{"Workspace delivery", "Paginated diff", []string{"Read every page"}, "../workspaces.md#task-results-by-task-id"},
		{"Workspace delivery", "Pull conflict", []string{"Pull into a new destination"}, "../workspaces.md#pulling-into-a-folder"},
		{"Vendor changes", "A vendor CLI changed version", []string{"The recorded Cursor update", "stop or requalify"}, "worker-setup.md#vendor-changes"},
	} {
		body, err := sectionOf(md, c.section)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var row string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "| "+c.symptom) {
				row = line
			}
		}
		if row == "" {
			errs = append(errs, fmt.Errorf("troubleshooting: no %q row in %s", c.symptom, c.section))
			continue
		}
		if cells := tableCells(row); len(cells) != 3 || cells[1] == "" || cells[2] == "" {
			errs = append(errs, fmt.Errorf("troubleshooting: %q lacks a check or a next action", c.symptom))
		}
		errs = append(errs, needAll("troubleshooting "+c.symptom, stripInlineLinks(row), c.wants...))
		if c.link != "" {
			errs = append(errs, wantLinks("troubleshooting "+c.symptom, row, c.link))
		}
	}
	return errors.Join(errs...)
}

// TestGuideTroubleshooting (FP-11).
func TestGuideTroubleshooting(t *testing.T) {
	md := guideDoc(t, "troubleshooting.md")
	if err := checkGuideTroubleshooting(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	runGuideMutations(t, md, checkGuideTroubleshooting, []readmeMutation{
		{"remove-category", replaceOnce("## Workspace delivery\n", "## Delivery\n"), "H2 headings"},
		{"remove-symptom", func(t *testing.T, md string) string {
			return regexp.MustCompile(`(?m)^\| Null final message .*\n`).ReplaceAllString(md, "")
		}, "no \"Null final message\" row"},
		{"remove-next-action", func(t *testing.T, md string) string {
			return replaceOnce("Pull into a new destination |", "|")(t, md)
		}, "\"Pull conflict\" lacks a check or a next action"},
		{"verification-bypass", replaceOnce("Correct the URL or CA copy. Verification stays on", "Skip verification for now"), "forbidden \"skip verification\""},
		{"redispatch", replaceOnce("Never dispatch a duplicate", "Redispatch the task"), "forbidden \"redispatch\""},
		{"silent-wait-redispatch", replaceOnce("Do not dispatch a duplicate", "Dispatch it again"), "forbidden \"dispatch it again\""},
		{"failed-publication-delivery", replaceOnce("A failed publication is not delivery", "A failed publication still counts as delivery"), "lacks \"A failed publication is not delivery\""},
		{"remove-assumption", replaceOnce(guideGrokAssumption+"\n", ""), "lacks \"ASSUMPTION (unverified)"},
		{"assumption-to-rule", replaceOnce(guideGrokAssumption, "Grok cannot run compound shell commands."), "forbidden \"grok cannot run compound shell commands\""},
		{"remove-attribution", replaceOnce("The owner reported during 12a that Grok headless could not run", "Grok headless could not run"), "lacks \"ASSUMPTION (unverified)"},
		{"remove-probe", replaceOnce(guideGrokProbe, "tries it"), "lacks \"tests simple versus compound"},
		{"recipe-from-memory", appendToSection("Authentication and permission failures", "Run chmod 777 on the state directory."), "forbidden \"chmod\""},
		{"category-link-removed", replaceOnce("[recover safely](dispatch-and-wait.md#recover-safely)", "recover safely"), "missing link dispatch-and-wait.md#recover-safely"},
	})
}

// ---- FP-2, FP-3, FP-6: the shared command and tool-argument contract.

// checkGuideJSONAgainstSchemas validates every guide tool example against the
// real tools/list schemas.
func checkGuideJSONAgainstSchemas(md string, schemas map[string]map[string]any) error {
	args, err := guideToolArguments(md)
	if err != nil {
		return err
	}
	var errs []error
	for _, tool := range slices.Sorted(maps.Keys(args)) {
		s, ok := schemas[tool]
		if !ok {
			errs = append(errs, fmt.Errorf("json: tool %s is not in tools/list", tool))
			continue
		}
		if err := validateAgainstSchema(s, args[tool]); err != nil {
			errs = append(errs, fmt.Errorf("json: tool %s: %w", tool, err))
		}
	}
	return errors.Join(errs...)
}

// checkDevFlowEnvelopes validates the example's two dispatch envelopes against
// the real dispatch schema, plus the cross-field rules a schema cannot express.
func checkDevFlowEnvelopes(readme string, schemas map[string]map[string]any) error {
	fences, err := markedJSONFences(readme, devFlowJSONMarker)
	if err != nil {
		return err
	}
	if got := slices.Sorted(maps.Keys(fences)); !slices.Equal(got, []string{"defect", "full"}) {
		return fmt.Errorf("json: envelope inventory %q", got)
	}
	var errs []error
	for id, mode := range map[string]string{"full": "flow and mode: full / implement", "defect": "flow and mode: light / defect"} {
		v, err := decodeStrictJSON(fences[id])
		if err != nil {
			errs = append(errs, fmt.Errorf("json: envelope %s: %w", id, err))
			continue
		}
		args := substitutePlaceholders(v, devFlowPlaceholders)
		if err := validateAgainstSchema(schemas["dispatch"], args); err != nil {
			errs = append(errs, fmt.Errorf("json: envelope %s: %w", id, err))
			continue
		}
		m := args.(map[string]any)
		if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, []string{"acceptance", "base", "goal", "target", "workspace", "workspace_instance"}) {
			errs = append(errs, fmt.Errorf("json: envelope %s has keys %q", id, got))
			continue
		}
		base, _ := m["base"].(string)
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(base) {
			errs = append(errs, fmt.Errorf("json: envelope %s base %q is not a full immutable commit", id, base))
		}
		target, _ := m["target"].(map[string]any)
		goal, _ := m["goal"].(string)
		acceptance, _ := m["acceptance"].(string)
		if target["kind"] != "name" || target["value"] != "coder" || !strings.Contains(goal, mode) || !strings.HasPrefix(goal, "ENVELOPE\n") {
			errs = append(errs, fmt.Errorf("json: envelope %s target or mode is wrong", id))
		}
		if err := taskTextBudget([]byte(goal), []byte(acceptance)); err != nil {
			errs = append(errs, fmt.Errorf("json: envelope %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// guideToolSchemas reads the real tools/list from one short-lived loopback plane
// and MCP pair, with no sidecar and no task. Both are stopped by test cleanup.
func guideToolSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	p := startMCPPlane(t, 0)
	m := startMCP(t, p.trust()...)
	r := m.request("tools/list", map[string]any{})
	if r.Error != nil {
		t.Fatalf("tools/list %.300s", r.raw)
	}
	v, err := decodeStrictJSON(string(r.Result))
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := v.(map[string]any)["tools"].([]any)
	out := map[string]map[string]any{}
	for _, e := range tools {
		tool, _ := e.(map[string]any)
		name, _ := tool["name"].(string)
		s, ok := tool["inputSchema"].(map[string]any)
		if name == "" || !ok {
			t.Fatalf("tools/list entry %v", e)
		}
		out[name] = s
	}
	return out
}

// TestGuideCommandContract: every pinned shell inventory through the real tree
// and leaf help, and every tool example through the real tools/list schemas.
func TestGuideCommandContract(t *testing.T) {
	roles, dispatch := guideDoc(t, "roles-and-manuals.md"), guideDoc(t, "dispatch-and-wait.md")
	readme := string(repoFile(t, guideExampleDoc))
	checkPage := func(file string) func(string) error {
		return func(md string) error {
			return errors.Join(checkFenceContract(t, md, guideContract(file)), checkGuideRecipes(file, md))
		}
	}
	t.Run("guide-inventories", func(t *testing.T) {
		for file := range guideInventories {
			if err := checkPage(file)(guideDoc(t, file)); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		}
		for _, c := range guideChapters {
			if _, fenced := guideInventories[c.file]; !fenced {
				if err := checkNoGuideFences(c.file, guideDoc(t, c.file)); err != nil {
					t.Errorf("%s: %v", c.file, err)
				}
			}
		}
		if err := checkNoGuideFences("index.md", guideDoc(t, "index.md")); err != nil {
			t.Error(err)
		}
		runGuideMutations(t, roles, checkPage("roles-and-manuals.md"), []readmeMutation{
			{"unknown-leaf", replaceOnce("callsheet role ls --plane", "callsheet role list --plane"), "unknown command \"list\""},
			{"unknown-flag", replaceOnce(`callsheet role ls --plane "$PLANE"`, `callsheet role ls --bogus --plane "$PLANE"`), "unknown flag --bogus"},
			{"missing-required-flag", replaceOnce(`callsheet role show "$ROLE_ID" --plane "$PLANE" --ca "$CA"`, `callsheet role show "$ROLE_ID" --plane "$PLANE"`), "missing required flag --ca"},
			{"wrong-operand-order", replaceOnce(`callsheet role show "$ROLE_ID" --plane "$PLANE" --ca "$CA"`, `callsheet role show --plane "$PLANE" --ca "$CA" "$ROLE_ID"`), "word 3 is \"--plane\""},
			{"bad-variable", replaceOnce(`callsheet role show "$ROLE_ID"`, `callsheet role show "$OTHER"`), "unknown variable $OTHER"},
			{"unquoted-variable", replaceOnce(`callsheet role show "$ROLE_ID"`, `callsheet role show $ROLE_ID`), "unquoted variable $ROLE_ID"},
			{"unknown-fence", appendToSection("Change a role", "<!-- guide: extra -->\n```sh\ncallsheet role ls --plane \"$PLANE\" --ca \"$CA\"\n```"), "fence extra is not in the pinned inventory"},
			{"missing-fence", replaceOnce("<!-- guide: role -->\n```sh\ncallsheet role show \"$ROLE_ID\" --plane \"$PLANE\" --ca \"$CA\"\n```\n", ""), "required fence role is missing"},
			{"duplicate-fence", appendToSection("Change a role", "<!-- guide: role -->\n```sh\ncallsheet role show \"$ROLE_ID\" --plane \"$PLANE\" --ca \"$CA\"\n```"), "duplicate fence ID role"},
			{"unmarked-fence", appendToSection("Change a role", "```sh\ncallsheet role ls\n```"), "unmarked sh fence"},
			{"hidden-command", appendToSection("Change a role", "```text\ncallsheet role rm x\n```"), "holds a Callsheet command"},
			{"marker-not-before-fence", replaceOnce("<!-- guide: role -->\n", "<!-- guide: role -->\n\n"), "is not immediately before a fence"},
			{"substitution", replaceOnce(`callsheet role show "$ROLE_ID"`, `callsheet role show "$(cat x)"`), "command substitution"},
			{"pipeline", replaceOnce(`callsheet node ls --plane "$PLANE" --ca "$CA"`, `callsheet node ls --plane "$PLANE" --ca "$CA" | cat`), "unsupported shell syntax pipe"},
			{"redirect", replaceOnce(`callsheet node ls --plane "$PLANE" --ca "$CA"`, `callsheet node ls --plane "$PLANE" --ca "$CA" > out`), "unsupported shell syntax redirect"},
			{"setup-step", replaceOnce("<!-- guide: role -->\n```sh\n", "<!-- guide: role -->\n```sh\nread -r ROLE_ID\n"), "only Callsheet commands are allowed"},
		})
		runGuideMutations(t, dispatch, checkPage("dispatch-and-wait.md"), []readmeMutation{
			{"no-goal", replaceOnce(`--goal "`+guideGoal+`" `, ""), "missing required flag --goal"},
			{"no-until-done", replaceOnce(" --until-done", ""), "missing required flag --until-done"},
			{"wrong-goal", replaceOnce(`--goal "`+guideGoal+`"`, `--goal "other"`), "word 5 is \"other\""},
			{"role-name-selection", replaceOnce(`--role-id "$ROLE_ID"`, `--role-name "$ROLE_ID"`), "missing required flag --role-id"},
			{"unknown-variable", replaceOnce(`callsheet task cancel "$TASK"`, `callsheet task cancel "$ROLE_NAME"`), "unknown variable $ROLE_NAME"},
		})
	})
	t.Run("example-inventory", func(t *testing.T) {
		check := func(md string) error {
			return errors.Join(checkFenceContract(t, md, devFlowContract()), checkRegistration(md, canonicalRoles))
		}
		if err := check(readme); err != nil {
			t.Fatalf("shipped README: %v", err)
		}
		runGuideMutations(t, readme, check, []readmeMutation{
			{"missing-fence", replaceOnce("<!-- dev-flow: ready-roles -->\n```sh\ncallsheet role ls --plane \"$PLANE\" --ca \"$CA\"\n```\n", ""), "required fence ready-roles is missing"},
			{"unknown-fence", appendToSection("Waits", "<!-- dev-flow: extra -->\n```sh\ncallsheet task ls\n```"), "fence extra is not in the pinned inventory"},
			{"unmarked-fence", appendToSection("Waits", "```sh\ncallsheet task ls\n```"), "unmarked sh fence"},
			{"hidden-command", appendToSection("Waits", "```\ncallsheet task ls\n```"), "holds a Callsheet command"},
			{"bad-variable", replaceOnce(`--node "$CODEX_NODE" --adapter codex --instruction "$MANUALS/triage-instruction.md"`, `--node "$NODE" --adapter codex --instruction "$MANUALS/triage-instruction.md"`), "unknown variable $NODE"},
			{"wrong-leaf-sequence", replaceOnce("callsheet role add triage --name triage", "callsheet role set triage --name triage"), "fence register-roles runs"},
			{"dropped-role", func(t *testing.T, md string) string {
				return regexp.MustCompile(`(?m)^callsheet role add triage .*\n`).ReplaceAllString(md, "")
			}, "register-roles holds 4 commands, want 5"},
			{"reordered-roles", func(t *testing.T, md string) string {
				first := regexp.MustCompile(`(?m)^callsheet role add designer .*\n`).FindString(md)
				second := regexp.MustCompile(`(?m)^callsheet role add design-reviewer .*\n`).FindString(md)
				return replaceOnce(first+second, second+first)(t, md)
			}, "word 3 is \"design-reviewer\", want \"designer\""},
		})
	})
	t.Run("tool-arguments", func(t *testing.T) {
		schemas := guideToolSchemas(t)
		if len(schemas) != 23 {
			t.Fatalf("tools/list has %d tools", len(schemas))
		}
		// An unsupported keyword anywhere in a real schema fails visibly.
		for _, name := range slices.Sorted(maps.Keys(schemas)) {
			if err := checkSchemaDeclaration("$", schemas[name]); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		check := func(md string) error { return checkGuideJSONAgainstSchemas(md, schemas) }
		if err := check(dispatch); err != nil {
			t.Fatalf("shipped chapter: %v", err)
		}
		if err := checkDevFlowEnvelopes(readme, schemas); err != nil {
			t.Fatalf("shipped envelopes: %v", err)
		}
		const anID = `"t_0123456789abcdef0123456789abcdef"`
		show := "<!-- guide-json: show -->\n```json\n{\"id\": " + anID + "}"
		runGuideMutations(t, dispatch, check, []readmeMutation{
			{"unknown-tool-key", replaceOnce(`"goal": "`+guideGoal+`",`, `"goal": "`+guideGoal+`", "priority": 1,`), "has keys"},
			{"duplicate-key", replaceOnce(`"goal": "`+guideGoal+`",`, `"goal": "`+guideGoal+`", "goal": "x",`), "duplicate key \"goal\""},
			{"trailing-data", replaceOnce(show, show+" {}"), "trailing data"},
			{"wrong-type", replaceOnce(show, "<!-- guide-json: show -->\n```json\n{\"id\": 1}"), "$.id: value is not of type string"},
			{"malformed-task-id", replaceOnce(show, "<!-- guide-json: show -->\n```json\n{\"id\": \"task-1\"}"), "$.id: value does not match the pattern"},
			{"undeclared-placeholder", replaceOnce(`"value": "<role-id>"`, `"value": "<role>"`), "$.target.value: value does not match the pattern"},
			{"bad-target-kind", replaceOnce(`"kind": "id"`, `"kind": "role"`), "dispatch differs from the CLI recipe"},
			{"missing-required", replaceOnce("{\"task_ids\": ["+anID+"]}", "{}"), "fence wait has keys"},
			{"empty-ids", replaceOnce("{\"task_ids\": ["+anID+"]}", "{\"task_ids\": []}"), "$.task_ids: 0 items is out of bounds"},
			{"duplicate-ids", replaceOnce("{\"task_ids\": ["+anID+"]}", "{\"task_ids\": ["+anID+", "+anID+"]}"), "$.task_ids: items 0 and 1 are duplicates"},
			{"unmarked-json", appendToSection("Recover safely", "```json\n{}\n```"), "unmarked JSON fence"},
			{"unknown-json-fence", appendToSection("Recover safely", "<!-- guide-json: extra -->\n```json\n{}\n```"), "fence extra is not in the pinned inventory"},
			{"missing-json-fence", replaceOnce("Tool `task_ls`:\n\n<!-- guide-json: list -->\n```json\n{}\n```\n", ""), "required fence list is missing"},
			{"duplicate-json-fence", appendToSection("Recover safely", "<!-- guide-json: list -->\n```json\n{}\n```"), "duplicate or misplaced marker list"},
			{"misplaced-json-marker", replaceOnce("<!-- guide-json: list -->\n", "<!-- guide-json: list -->\n\n"), "is not immediately before a JSON fence"},
		})
		envelopes := func(md string) error { return checkDevFlowEnvelopes(md, schemas) }
		runGuideMutations(t, readme, envelopes, []readmeMutation{
			{"floating-base", replaceOnce(`"base": "<40-hex-commit>"`, `"base": "main"`), "base \"main\" is not a full immutable commit"},
			{"unknown-envelope-key", replaceOnce(`"base": "<40-hex-commit>"`, `"model": "x", "base": "<40-hex-commit>"`), "$: unknown property model"},
			{"missing-instance", replaceOnce("  \"workspace_instance\": \"<instance>\",\n  \"base\": \"<40-hex-commit>\"", "  \"base\": \"<40-hex-commit>\""), "has keys"},
			{"duplicate-envelope-key", replaceOnce(`"base": "<40-hex-commit>"`, `"base": "<40-hex-commit>", "base": "main"`), "duplicate key \"base\""},
			{"wrong-target", replaceOnce("\"target\": {\"kind\": \"name\", \"value\": \"coder\"},\n  \"goal\": \"ENVELOPE\\nattempt ID: <attempt>\\nflow and mode: light", "\"target\": {\"kind\": \"name\", \"value\": \"designer\"},\n  \"goal\": \"ENVELOPE\\nattempt ID: <attempt>\\nflow and mode: light"), "envelope defect target or mode is wrong"},
			{"missing-marker", replaceOnce("<!-- dev-flow-json: defect -->\n", ""), "unmarked JSON fence"},
		})
		// A keyword the dialect does not enforce makes the real schema fail.
		mutated := maps.Clone(schemas)
		withKeyword := maps.Clone(schemas["task_show"])
		withKeyword["oneOf"] = []any{}
		mutated["task_show"] = withKeyword
		if err := checkGuideJSONAgainstSchemas(dispatch, mutated); err == nil || !strings.Contains(err.Error(), "unsupported schema keyword oneOf") {
			t.Errorf("a new schema keyword: err = %v", err)
		}
		delete(mutated, "task_show")
		if err := checkGuideJSONAgainstSchemas(dispatch, mutated); err == nil || !strings.Contains(err.Error(), "tool task_show is not in tools/list") {
			t.Errorf("a removed tool: err = %v", err)
		}
	})
}
