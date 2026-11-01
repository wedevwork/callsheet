package function

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 12-1 function tests (design 12-1-readme): one top-level
// TestReadme* per FP. Each validates the shipped README (and, for FP-4, the
// built binary's root help) with a test-only validator, then proves that the
// validator rejects deliberate mutations of in-memory copies. Nothing here
// contacts a network, launches a vendor CLI or modifies the checkout. The
// validators detect accidental documentation regressions; they are not a
// Markdown parser and not a secret scanner.

const (
	readmeSentence   = "Callsheet is a distributed AI agent dispatcher."
	readmeOldTagline = "Callsheet coordinates AI coding agents across machines"
	readmeExclusion  = "It is not an agent, agent orchestration, a deployment tool, SaaS, or a workflow engine; pipelines live in coordinator runbooks."
	readmeSecurity   = "Use a trusted network and a dedicated OS user: TLS protects transport but provides no client identity or authorization, " +
		"any reachable peer holding the CA can dispatch work and read or write every workspace, and worker containment depends on the " +
		"vendor configuration because Callsheet adds no sandbox of its own."
	readmeWordCap  = 750
	readmeTopology = "Coordinator CLI -> local callsheet mcp -> Plane\n" +
		"                                       (roles, tasks, workspace hub)\n" +
		"Worker node A: sidecar -> Plane (outbound TLS connection)\n" +
		"              roles -> worker CLI -> task checkout -> result commit\n" +
		"Worker node B: sidecar -> Plane (outbound TLS connection)\n" +
		"              roles -> worker CLI -> task checkout -> result commit"
	readmeComponentOS = "Callsheet's plane, sidecar and coordinator commands support Linux and macOS"
	readmeNoWindows   = "Windows is not supported"
	readmeHarnessNote = "The real-e2e harness is for maintainers and is not a prerequisite for ordinary use."
)

// readmeH2 is the complete, ordered list of second-level headings.
var readmeH2 = []string{"How it works", "Quickstart summary", "Supported CLIs and OSes", "Security", "Documentation", "For maintainers"}

// readmeIntro pins the introductory paragraph's relationship clauses.
var readmeIntro = []string{"shared roster, dispatch path, task log and versioned workspace across machines", "one CLI as your coordinator", "other CLI processes do worker tasks"}

// readmeHow pins the relationship prose beside the topology diagram.
var readmeHow = []string{
	"The plane holds the shared roster and tasks.",
	"sidecar connects out to the plane and supervises worker CLI processes",
	"A role selects the manuals, model, effort and concurrency",
	"chooses tasks and every follow-up",
	"Callsheet hosts no coordinator, runs no pipeline, merges no results and starts a fresh worker process for each task",
	"Workspaces are optional",
	"a workspace task starts from a base commit in a fresh checkout and publishes a result commit",
	"a task without one runs in a scratch directory",
	"the plane, a sidecar and the coordinator can share one machine",
}

// readmeSteps pins each quickstart step's command pointers, links and
// qualifiers, in order.
var readmeSteps = [][]string{
	{"Install Callsheet using the [quickstart](docs/quickstart.md#install-callsheet)", "select a published release and verify its checksum", "If no release is published yet, use its source-build fallback"},
	{"`plane init`", "`plane run`", "CA certificate", "`--help`"},
	{"`sidecar enroll`", "`sidecar run`", "explicitly enabled, already authenticated vendor CLI", "`role add`", "](docs/real-adapters.md)"},
	{"`callsheet mcp`", "runbook", "](docs/coordinator.md)", "Direct CLI dispatch remains available"},
	{"`dispatch`", "`task show`", "`task wait`", "](docs/workspaces.md)", "](docs/examples/dev-flow/README.md)"},
}

// readmeSupportRows are the exact worker rows, in order.
var readmeSupportRows = [][2]string{
	{"Claude Code (`claude`)", "Linux; macOS can be enabled explicitly, vendor sandbox and exit behavior unverified"},
	{"Codex (`codex`)", "Linux; macOS can be enabled explicitly, vendor sandbox and exit behavior unverified"},
	{"Grok Build (`grok`)", "Linux only; macOS worker execution refused"},
	{"Cursor Agent (`cursor`)", "Linux only; non-Linux worker execution refused"},
}

var (
	readmeUserMap     = []string{"docs/quickstart.md", "docs/coordinator.md", "docs/coordinator.md#coordinator-runbook-example", "docs/real-adapters.md", "docs/workspaces.md", "docs/support-catalog.md", "docs/examples/dev-flow/README.md"}
	readmeMaintainers = []string{"docs/maintainers/ci.md", "docs/maintainers/real-e2e.md", "docs/examples/real-e2e/README.md"}
	readmePlanned     = []string{"docs/guide/index.md", "docs/deploy.md"}
	// readmeExisting are the nine existing documentation entry paths
	// with maintainer documents under docs/maintainers/.
	readmeExisting = []string{"docs/quickstart.md", "docs/coordinator.md", "docs/real-adapters.md", "docs/workspaces.md", "docs/support-catalog.md",
		"docs/examples/dev-flow/README.md", "docs/maintainers/ci.md", "docs/maintainers/real-e2e.md", "docs/examples/real-e2e/README.md"}
	readmePrivate = []string{"/home/", "/Users/", "/opt/gitspace/", "file:"}
)

var (
	readmeInlineLink = regexp.MustCompile(`\[([^\]]*)\]\([^)\s]*\)`)
	readmeTextFence  = regexp.MustCompile("(?s)```text\n(.*?)\n```")
	readmeStep       = regexp.MustCompile(`^(\d+)\. `)
	readmeURIScheme  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	readmeHeading    = regexp.MustCompile(`^#{1,6} +(.+?) *$`)
	readmeQualified  = regexp.MustCompile(`(?i)\b(qualified|certified)\b`)
)

// repoLookup reads a repository-relative regular file; the second result
// is false when it does not exist (or is a directory).
type repoLookup func(rel string) ([]byte, bool)

func checkoutLookup(root string) repoLookup {
	return func(rel string) ([]byte, bool) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return nil, false
		}
		b, err := os.ReadFile(p)
		return b, err == nil
	}
}

func mapLookup(files map[string]string) repoLookup {
	return func(rel string) ([]byte, bool) {
		s, ok := files[rel]
		return []byte(s), ok
	}
}

type readmeSection struct{ name, body string }

type readmeDoc struct {
	title, preamble string
	sections        []readmeSection
}

func fenceLine(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "```") }

// parseReadme splits md into its single H1 title, the preamble before the
// first H2 and the H2 sections, ignoring heading-like lines inside fences.
func parseReadme(md string) (readmeDoc, error) {
	var d readmeDoc
	var pre []string
	var bodies [][]string
	inFence, titled := false, false
	for _, line := range strings.Split(md, "\n") {
		if !inFence && strings.HasPrefix(line, "# ") {
			if titled {
				return d, fmt.Errorf("structure: a second H1 %q", line)
			}
			titled, d.title = true, strings.TrimSpace(line[2:])
			continue
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			if !titled {
				return d, fmt.Errorf("structure: H2 %q before the title", line)
			}
			d.sections = append(d.sections, readmeSection{name: strings.TrimSpace(line[3:])})
			bodies = append(bodies, nil)
			continue
		}
		if fenceLine(line) {
			inFence = !inFence
		}
		switch {
		case !titled:
			if strings.TrimSpace(line) != "" {
				return d, fmt.Errorf("structure: text before the title: %q", line)
			}
		case len(bodies) == 0:
			pre = append(pre, line)
		default:
			bodies[len(bodies)-1] = append(bodies[len(bodies)-1], line)
		}
	}
	if !titled {
		return d, errors.New("structure: no H1 title")
	}
	d.preamble = strings.Join(pre, "\n")
	for i := range d.sections {
		d.sections[i].body = strings.Join(bodies[i], "\n")
	}
	return d, nil
}

func (d readmeDoc) section(name string) (string, error) {
	for _, s := range d.sections {
		if s.name == name {
			return s.body, nil
		}
	}
	return "", fmt.Errorf("structure: no %q section", name)
}

func normSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func stripInlineLinks(s string) string { return readmeInlineLink.ReplaceAllString(s, "$1") }

// paragraphs returns the blank-line-separated paragraphs of s, whitespace
// normalized.
func paragraphs(s string) []string {
	var out []string
	for _, p := range regexp.MustCompile(`\n\s*\n`).Split(s, -1) {
		if p = normSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sectionOf(md, name string) (string, error) {
	d, err := parseReadme(md)
	if err != nil {
		return "", err
	}
	return d.section(name)
}

// checkPositioning: the title, the canonical first sentence, the
// introductory clauses and the exclusion sentence (FP-1).
func checkPositioning(md string) error {
	d, err := parseReadme(md)
	if err != nil {
		return err
	}
	if d.title != "Callsheet" {
		return fmt.Errorf("positioning: title %q, want %q", d.title, "Callsheet")
	}
	paras := paragraphs(d.preamble)
	if len(paras) == 0 || paras[0] != readmeSentence {
		return fmt.Errorf("positioning: first sentence %q, want %q", strings.Join(paras[:min(1, len(paras))], ""), readmeSentence)
	}
	pre := normSpace(d.preamble)
	if !strings.Contains(pre, readmeExclusion) {
		return fmt.Errorf("positioning: no exclusion sentence %q", readmeExclusion)
	}
	for _, s := range readmeIntro {
		if !strings.Contains(pre, s) {
			return fmt.Errorf("positioning: introduction lacks %q", s)
		}
	}
	// "orchestration" appears only inside the exclusion.
	if strings.Contains(strings.ToLower(strings.ReplaceAll(normSpace(md), readmeExclusion, "")), "orchestrat") {
		return errors.New("positioning: orchestration named outside the exclusion sentence")
	}
	return nil
}

// checkTopology: the exact text diagram and the relationship prose (FP-1).
func checkTopology(md string) error {
	body, err := sectionOf(md, "How it works")
	if err != nil {
		return err
	}
	blocks := readmeTextFence.FindAllStringSubmatch(body, -1)
	if len(blocks) != 1 || blocks[0][1] != readmeTopology {
		return fmt.Errorf("topology: diagram is not the single exact text block (%d text blocks)", len(blocks))
	}
	if strings.Contains(md, "```mermaid") || strings.Contains(md, "![") {
		return errors.New("topology: a Mermaid or image diagram")
	}
	prose := normSpace(readmeTextFence.ReplaceAllString(body, ""))
	for _, s := range readmeHow {
		if !strings.Contains(prose, s) {
			return fmt.Errorf("topology: How it works lacks %q", s)
		}
	}
	return nil
}

type mdStep struct {
	n    string
	text string
}

// orderedSteps returns the top-level ordered list items of body; indented
// lines continue the current item.
func orderedSteps(body string) []mdStep {
	var out []mdStep
	open := false
	for _, line := range strings.Split(body, "\n") {
		if m := readmeStep.FindStringSubmatch(line); m != nil {
			out, open = append(out, mdStep{n: m[1], text: line[len(m[0]):]}), true
			continue
		}
		if open && strings.TrimSpace(line) != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			out[len(out)-1].text += " " + line
			continue
		}
		open = false
	}
	return out
}

// checkQuickstart: the five ordered orientation steps (FP-1).
func checkQuickstart(md string) error {
	body, err := sectionOf(md, "Quickstart summary")
	if err != nil {
		return err
	}
	if !strings.Contains(normSpace(body), "not a complete installation recipe") {
		return errors.New("quickstart: not labeled an orientation rather than a complete installation recipe")
	}
	steps := orderedSteps(body)
	if len(steps) != len(readmeSteps) {
		return fmt.Errorf("quickstart: %d steps, want %d", len(steps), len(readmeSteps))
	}
	for i, st := range steps {
		if st.n != fmt.Sprint(i+1) {
			return fmt.Errorf("quickstart: step %d is numbered %s", i+1, st.n)
		}
		text := normSpace(st.text)
		for _, s := range readmeSteps[i] {
			if !strings.Contains(text, s) {
				return fmt.Errorf("quickstart: step %d lacks %q", i+1, s)
			}
		}
	}
	lower := strings.ToLower(body)
	for _, s := range []string{"ca key", "private key", "://", "curl ", "sudo "} {
		if strings.Contains(lower, s) {
			return fmt.Errorf("quickstart: forbidden %q", s)
		}
	}
	return nil
}

// checkStructureAndBudget: the title, the exact ordered H2 list and the
// strings.Fields word cap over the whole Markdown source (FP-1).
func checkStructureAndBudget(md string) error {
	d, err := parseReadme(md)
	if err != nil {
		return err
	}
	var names []string
	for _, s := range d.sections {
		names = append(names, s.name)
	}
	if !slices.Equal(names, readmeH2) {
		return fmt.Errorf("structure: H2 headings %q, want %q", names, readmeH2)
	}
	if n := len(strings.Fields(md)); n > readmeWordCap {
		return fmt.Errorf("budget: %d words, cap %d", n, readmeWordCap)
	}
	return nil
}

func checkEntryPoint(md string) error {
	return errors.Join(checkPositioning(md), checkTopology(md), checkQuickstart(md), checkStructureAndBudget(md))
}

func tableCells(line string) []string {
	cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	for i, c := range cells {
		cells[i] = normSpace(c)
	}
	return cells
}

func sectionLinks(body string) []string { return markdownLinks(body) }

// checkSupport: component OS support, the four exact worker rows, the
// coordinator/qualification pointer, and no affirmative macOS, Windows or
// qualification claim anywhere outside the sanctioned text (FP-2).
func checkSupport(md string) error {
	body, err := sectionOf(md, "Supported CLIs and OSes")
	if err != nil {
		return err
	}
	text := normSpace(body)
	for _, s := range []string{readmeComponentOS, readmeNoWindows, "not blanket vendor qualification"} {
		if !strings.Contains(text, s) {
			return fmt.Errorf("support: lacks %q", s)
		}
	}
	var table []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			table = append(table, line)
		}
	}
	if len(table) < 2 || !slices.Equal(tableCells(table[0]), []string{"CLI", "Worker availability"}) || strings.Trim(table[1], "|-: ") != "" {
		return errors.New("support: no `CLI | Worker availability` table")
	}
	rows := table[2:]
	if len(rows) != len(readmeSupportRows) {
		return fmt.Errorf("support: %d worker rows, want %d", len(rows), len(readmeSupportRows))
	}
	for i, r := range rows {
		want := []string{readmeSupportRows[i][0], readmeSupportRows[i][1]}
		if got := tableCells(r); !slices.Equal(got, want) {
			return fmt.Errorf("support: row %d = %q, want %q", i+1, got, want)
		}
	}
	links := sectionLinks(body)
	for _, l := range []string{"docs/coordinator.md", "docs/support-catalog.md"} {
		if !slices.Contains(links, l) {
			return fmt.Errorf("support: missing link %s", l)
		}
	}
	rest := normSpace(md)
	for _, s := range []string{readmeComponentOS, readmeNoWindows, readmeSupportRows[0][1], readmeSupportRows[2][1], readmeSupportRows[3][1]} {
		rest = strings.ReplaceAll(rest, s, "")
	}
	lower := strings.ToLower(rest)
	if strings.Contains(lower, "macos") {
		return errors.New("support: macOS claim outside the component statement and worker rows")
	}
	if strings.Contains(lower, "windows") {
		return errors.New("support: Windows claim beyond \"Windows is not supported\"")
	}
	if m := readmeQualified.FindString(rest); m != "" {
		return fmt.Errorf("support: affirmative qualification claim %q", m)
	}
	return nil
}

// checkSecurity: the section is exactly the one security sentence, with the
// two required links (FP-2).
func checkSecurity(md string) error {
	body, err := sectionOf(md, "Security")
	if err != nil {
		return err
	}
	if got := normSpace(stripInlineLinks(body)); got != readmeSecurity {
		return fmt.Errorf("security: sentence %q, want %q", got, readmeSecurity)
	}
	for _, l := range []string{"[CA](docs/coordinator.md#what-every-configuration-shares)", "[vendor configuration](docs/real-adapters.md#enabling-a-worker)"} {
		if !strings.Contains(body, l) {
			return fmt.Errorf("security: missing link %s", l)
		}
	}
	return nil
}

func checkSupportAndSecurity(md string) error {
	return errors.Join(checkSupport(md), checkSecurity(md))
}

// headingSlugs returns the fragments of doc's headings outside fences: the
// heading text lowercased with spaces replaced by hyphens.
func headingSlugs(doc string) map[string]bool {
	out := map[string]bool{}
	inFence := false
	for _, line := range strings.Split(doc, "\n") {
		if fenceLine(line) {
			inFence = !inFence
			continue
		}
		if m := readmeHeading.FindStringSubmatch(line); m != nil && !inFence {
			out[strings.ReplaceAll(strings.ToLower(m[1]), " ", "-")] = true
		}
	}
	return out
}

// readmeLinkFile returns raw's cleaned repository-relative file part ("" for
// a pure fragment) or why it is not an allowed destination.
func readmeLinkFile(raw string) (string, error) {
	file, _, _ := strings.Cut(raw, "#")
	switch {
	case readmeURIScheme.MatchString(raw):
		return "", fmt.Errorf("link %s: URI scheme", raw)
	case strings.HasPrefix(file, "/") || strings.HasPrefix(file, `\`) || filepath.IsAbs(file):
		return "", fmt.Errorf("link %s: absolute path", raw)
	case file == "":
		return "", nil
	}
	clean := path.Clean(file)
	switch {
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("link %s: escapes the repository", raw)
	case clean == "design" || strings.HasPrefix(clean, "design/"):
		return "", fmt.Errorf("link %s: points into the ignored design tree", raw)
	}
	return clean, nil
}

// checkReadmeLinks resolves every simple inline link of md through look:
// files must exist and fragments must match a heading outside fences, of
// the target or (for a pure fragment) of md itself.
func checkReadmeLinks(md string, look repoLookup) error {
	var errs []error
	for _, raw := range markdownLinks(md) {
		file, err := readmeLinkFile(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		doc := md
		if file != "" {
			b, ok := look(file)
			if !ok {
				errs = append(errs, fmt.Errorf("link %s: destination %s does not exist", raw, file))
				continue
			}
			doc = string(b)
		}
		if _, frag, ok := strings.Cut(raw, "#"); ok && (frag == "" || !headingSlugs(doc)[frag]) {
			errs = append(errs, fmt.Errorf("link %s: fragment #%s matches no heading", raw, frag))
		}
	}
	return errors.Join(errs...)
}

// checkPrivateText rejects the private-path sentinels in shipped text.
func checkPrivateText(where, text string) error {
	for _, s := range readmePrivate {
		if strings.Contains(text, s) {
			return fmt.Errorf("%s: private path sentinel %q", where, s)
		}
	}
	return nil
}

func linkFiles(body string) []string {
	var out []string
	for _, raw := range markdownLinks(body) {
		if f, err := readmeLinkFile(raw); err == nil && f != "" {
			out = append(out, f)
		}
	}
	return out
}

// checkDocMap: the user map, the separate maintainer map, the unlinked
// coming-in-v1 entries, the existing entry paths, every link and anchor,
// and the private-path sentinels (FP-3).
func checkDocMap(md string, look repoLookup) error {
	d, err := parseReadme(md)
	if err != nil {
		return err
	}
	docs, err := d.section("Documentation")
	if err != nil {
		return err
	}
	maint, err := d.section("For maintainers")
	if err != nil {
		return err
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	for _, l := range readmeUserMap {
		if !slices.Contains(markdownLinks(docs), l) {
			add("documentation: missing link %s", l)
		}
	}
	for _, l := range readmeMaintainers {
		if !slices.Contains(markdownLinks(maint), l) {
			add("for maintainers: missing link %s", l)
		}
	}
	if !strings.Contains(normSpace(maint), readmeHarnessNote) {
		add("for maintainers: lacks %q", readmeHarnessNote)
	}
	parts := append([]readmeSection{{name: "preamble", body: d.preamble}}, d.sections...)
	for _, s := range parts {
		if s.name == "For maintainers" {
			continue
		}
		for _, f := range linkFiles(s.body) {
			if slices.Contains(readmeMaintainers, f) {
				add("%s: maintainer link %s outside For maintainers", s.name, f)
			}
		}
	}
	all := linkFiles(md)
	for _, p := range readmePlanned {
		if slices.Contains(all, p) {
			add("documentation: planned %s is a link", p)
		}
		if _, ok := look(p); ok {
			add("documentation: planned %s exists as a placeholder", p)
		}
		listed := false
		for _, line := range strings.Split(docs, "\n") {
			if strings.Contains(line, "`"+p+"`") {
				listed = true
				if !strings.Contains(line, "coming in v1") {
					add("documentation: planned %s is not labeled coming in v1", p)
				}
			}
		}
		if !listed {
			add("documentation: planned %s is not listed", p)
		}
	}
	for _, p := range readmeExisting {
		if _, ok := look(p); !ok {
			add("documentation: existing entry %s is missing", p)
		}
	}
	errs = append(errs, checkReadmeLinks(md, look), checkPrivateText("README", md))
	return errors.Join(errs...)
}

// rootHelp is one root help form's outcome.
type rootHelp struct {
	args []string
	res  result
}

// checkCLIPositioning: every root help form succeeds silently with the same
// output, whose summary is the canonical sentence and the README's opening
// (FP-4).
func checkCLIPositioning(readme string, forms []rootHelp) error {
	if len(forms) != 4 {
		return fmt.Errorf("help: %d root forms, want 4", len(forms))
	}
	var errs []error
	for _, f := range forms {
		if f.res.code != 0 || f.res.stderr != "" {
			errs = append(errs, fmt.Errorf("help %q: exit %d, stderr %q", f.args, f.res.code, f.res.stderr))
		}
		if f.res.stdout != forms[0].res.stdout {
			errs = append(errs, fmt.Errorf("help %q diverges from %q", f.args, forms[0].args))
		}
	}
	out := forms[0].res.stdout
	lines := strings.Split(out, "\n")
	summary := ""
	if len(lines) < 4 || lines[0] != "Usage: callsheet <command>" || lines[1] != "" || lines[3] != "" {
		errs = append(errs, fmt.Errorf("help: layout changed: %q", out))
	} else if summary = lines[2]; summary != readmeSentence {
		errs = append(errs, fmt.Errorf("help: summary %q, want %q", summary, readmeSentence))
	}
	if strings.Contains(out, readmeOldTagline) {
		errs = append(errs, errors.New("help: carries the old tagline"))
	}
	if err := checkPrivateText("help", out); err != nil {
		errs = append(errs, err)
	}
	d, err := parseReadme(readme)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if paras := paragraphs(d.preamble); len(paras) == 0 || paras[0] != summary {
		errs = append(errs, fmt.Errorf("README opening %q differs from the help summary %q", strings.Join(paras[:min(1, len(paras))], ""), summary))
	}
	return errors.Join(errs...)
}

// readmeMutation is one deliberate in-memory change and the error text its
// validator must report.
type readmeMutation struct {
	name  string
	apply func(t *testing.T, md string) string
	want  string
}

func replaceOnce(old, repl string) func(*testing.T, string) string {
	return func(t *testing.T, md string) string {
		t.Helper()
		if !strings.Contains(md, old) {
			t.Fatalf("mutation target %q not found", old)
		}
		return strings.Replace(md, old, repl, 1)
	}
}

// sectionSpan returns the byte range of section name's body in md.
func sectionSpan(t *testing.T, md, name string) (int, int) {
	t.Helper()
	h := "\n## " + name + "\n"
	i := strings.Index(md, h)
	if i < 0 {
		t.Fatalf("no section %q", name)
	}
	start := i + len(h)
	end := len(md)
	if j := strings.Index(md[start:], "\n## "); j >= 0 {
		end = start + j + 1
	}
	return start, end
}

// inSection replaces the first old inside section name only.
func inSection(name, old, repl string) func(*testing.T, string) string {
	return func(t *testing.T, md string) string {
		t.Helper()
		s, e := sectionSpan(t, md, name)
		return md[:s] + replaceOnce(old, repl)(t, md[s:e]) + md[e:]
	}
}

// inSectionRe replaces the first match of re inside section name only.
func inSectionRe(name, re, repl string) func(*testing.T, string) string {
	return func(t *testing.T, md string) string {
		t.Helper()
		s, e := sectionSpan(t, md, name)
		body := md[s:e]
		loc := regexp.MustCompile(re).FindStringSubmatchIndex(body)
		if loc == nil {
			t.Fatalf("mutation pattern %q not found in %q", re, name)
		}
		var out []byte
		out = regexp.MustCompile(re).ExpandString(out, repl, body, loc)
		return md[:s] + body[:loc[0]] + string(out) + body[loc[1]:] + md[e:]
	}
}

// appendToSection adds a paragraph at the end of section name.
func appendToSection(name, text string) func(*testing.T, string) string {
	return func(t *testing.T, md string) string {
		t.Helper()
		_, e := sectionSpan(t, md, name)
		return md[:e] + "\n" + text + "\n" + md[e:]
	}
}

// padWords returns md grown to exactly n strings.Fields words.
func padWords(t *testing.T, md string, n int) string {
	t.Helper()
	have := len(strings.Fields(md))
	if have > n {
		t.Fatalf("README already has %d words, more than %d", have, n)
	}
	return md + strings.Repeat(" pad", n-have) + "\n"
}

func runMutations(t *testing.T, md string, check func(string) error, ms []readmeMutation) {
	t.Helper()
	for _, m := range ms {
		err := check(m.apply(t, md))
		if err == nil || !strings.Contains(err.Error(), m.want) {
			t.Errorf("mutation %s: err = %v, want one containing %q", m.name, err, m.want)
		}
	}
}

// TestReadmeEntryPoint (FP-1): positioning, topology, quickstart and
// structure/budget of the shipped README, each rejecting its mutations.
func TestReadmeEntryPoint(t *testing.T) {
	md := string(repoFile(t, "README.md"))
	sub := func(name string, check func(string) error, ms []readmeMutation) {
		t.Run(name, func(t *testing.T) {
			if err := check(md); err != nil {
				t.Fatalf("shipped README: %v", err)
			}
			runMutations(t, md, check, ms)
		})
	}
	sub("positioning", checkPositioning, []readmeMutation{
		{"dispatcher-to-orchestration", replaceOnce("AI agent dispatcher.", "AI agent orchestration."), "positioning: first sentence"},
		{"remove-exclusion", replaceOnce(readmeExclusion, ""), "positioning: no exclusion sentence"},
		{"retitle", replaceOnce("# Callsheet\n", "# callsheet\n"), "positioning: title"},
		{"sentence-not-first", replaceOnce(readmeSentence+"\n", "Welcome.\n\n"+readmeSentence+"\n"), "positioning: first sentence"},
		{"orchestration-claim", appendToSection("How it works", "Callsheet provides agent orchestration."), "positioning: orchestration named outside"},
	})
	sub("topology", checkTopology, []readmeMutation{
		{"reverse-sidecar-arrow", replaceOnce("Worker node A: sidecar -> Plane", "Worker node A: sidecar <- Plane"), "topology: diagram"},
		{"inbound-connection", replaceOnce("(outbound TLS connection)", "(inbound TLS connection)"), "topology: diagram"},
		{"omit-scratch", replaceOnce(", and a task without one runs in a scratch directory", ""), "topology: How it works lacks \"a task without one runs in a scratch directory\""},
		{"omit-optional", replaceOnce("Workspaces are optional", "Workspaces are required"), "topology: How it works lacks \"Workspaces are optional\""},
		{"imply-automation", replaceOnce("hosts no coordinator, runs no pipeline, merges no results", "hosts the coordinator, runs pipelines, merges results"), "topology: How it works lacks"},
		{"mermaid", replaceOnce("```text\n", "```mermaid\n"), "topology:"},
	})
	sub("quickstart", checkQuickstart, []readmeMutation{
		{"drop-step", inSectionRe("Quickstart summary", `(?m)^5\. .*\n`, ""), "quickstart: 4 steps, want 5"},
		{"drop-install-link", inSection("Quickstart summary", "[quickstart](docs/quickstart.md#install-callsheet)", "quickstart"), "quickstart: step 1 lacks \"Install Callsheet using the [quickstart](docs/quickstart.md#install-callsheet)\""},
		{"remove-source-fallback", inSection("Quickstart summary", "If no release is published yet, use its source-build fallback.", "Binaries are available."), "quickstart: step 1 lacks \"If no release is published yet, use its source-build fallback\""},
		{"swap-steps", inSectionRe("Quickstart summary", `(?m)^2\. (.*)\n3\. (.*)\n`, "2. $2\n3. $1\n"), "quickstart: step 2 lacks"},
		{"drop-role-add", inSection("Quickstart summary", "(`role add`)", ""), "quickstart: step 3 lacks \"`role add`\""},
		{"ca-key", inSection("Quickstart summary", "give its CA certificate", "give its CA certificate and CA key"), "quickstart: forbidden \"ca key\""},
		{"download-url", appendToSection("Quickstart summary", "Download from https://example.invalid/callsheet."), "quickstart: forbidden \"://\""},
	})
	sub("structure-and-budget", checkStructureAndBudget, []readmeMutation{
		{"exceed-word-cap", func(t *testing.T, md string) string { return padWords(t, md, readmeWordCap+1) }, "budget: 751 words, cap 750"},
		{"rename-section", replaceOnce("\n## Security\n", "\n## Safety\n"), "structure: H2 headings"},
		{"drop-section", replaceOnce("\n## For maintainers\n", "\n"), "structure: H2 headings"},
		{"reorder-sections", func(t *testing.T, md string) string {
			md = replaceOnce("\n## Security\n", "\n## TMP\n")(t, md)
			md = replaceOnce("\n## Documentation\n", "\n## Security\n")(t, md)
			return replaceOnce("\n## TMP\n", "\n## Documentation\n")(t, md)
		}, "structure: H2 headings"},
		{"second-title", appendToSection("For maintainers", "# Callsheet again"), "structure: a second H1"},
	})
}

// TestReadmeSupportAndSecurity (FP-2): the four exact worker rows, the
// component OS statement and the one-sentence security posture.
func TestReadmeSupportAndSecurity(t *testing.T) {
	md := string(repoFile(t, "README.md"))
	if err := checkSupportAndSecurity(md); err != nil {
		t.Fatalf("shipped README: %v", err)
	}
	const sec = "Supported CLIs and OSes"
	claude, codex := readmeSupportRows[0], readmeSupportRows[1]
	runMutations(t, md, checkSupportAndSecurity, []readmeMutation{
		{"grok-macos-supported", inSection(sec, "| Linux only; macOS worker execution refused |", "| Linux and macOS |"), "support: row 3"},
		{"cursor-macos-supported", inSection(sec, "| Linux only; non-Linux worker execution refused |", "| Linux and macOS |"), "support: row 4"},
		{"claude-unverified-removed", inSection(sec, "| "+claude[0]+" | "+claude[1]+" |", "| "+claude[0]+" | Linux; macOS can be enabled explicitly |"), "support: row 1"},
		{"codex-unverified-removed", inSection(sec, "| "+codex[0]+" | "+codex[1]+" |", "| "+codex[0]+" | Linux; macOS can be enabled explicitly |"), "support: row 2"},
		{"windows-supported", replaceOnce(readmeNoWindows, "Windows is supported"), "support: lacks \"Windows is not supported\""},
		{"windows-extra-claim", appendToSection(sec, "Windows workers are available too."), "support: Windows claim"},
		{"windows-row", inSection(sec, "| Cursor Agent (`cursor`) | Linux only; non-Linux worker execution refused |\n",
			"| Cursor Agent (`cursor`) | Linux only; non-Linux worker execution refused |\n| Codex (`codex`) | Windows |\n"), "support: 5 worker rows, want 4"},
		{"contradictory-extra-row", inSection(sec, "| Cursor Agent (`cursor`) | Linux only; non-Linux worker execution refused |\n",
			"| Cursor Agent (`cursor`) | Linux only; non-Linux worker execution refused |\n| Grok Build (`grok`) | Linux and macOS |\n"), "support: 5 worker rows, want 4"},
		{"macos-claim-elsewhere", appendToSection("How it works", "Grok Build workers run on macOS too."), "support: macOS claim"},
		{"qualified-claim", appendToSection(sec, "Every CLI is fully qualified."), "support: affirmative qualification claim"},
		{"blanket-qualification", inSection(sec, "not blanket vendor qualification", "blanket vendor qualification"), "support: lacks \"not blanket vendor qualification\""},
		{"no-catalog-link", inSection(sec, "](docs/support-catalog.md)", "](docs/support-catalog.md#catalog-contract)"), "support: missing link docs/support-catalog.md"},
		{"identity-to-authentication", replaceOnce("provides no client identity or authorization", "authenticates every client"), "security: sentence"},
		{"remove-no-own-sandbox", replaceOnce(" because Callsheet adds no sandbox of its own", ""), "security: sentence"},
		{"second-sentence", appendToSection("Security", "Callsheet authenticates every node."), "security: sentence"},
		{"unlinked-ca", replaceOnce("[CA](docs/coordinator.md#what-every-configuration-shares)", "CA"), "security: missing link [CA]"},
		{"unlinked-vendor-configuration", replaceOnce("[vendor configuration](docs/real-adapters.md#enabling-a-worker)", "vendor configuration"), "security: missing link [vendor configuration]"},
	})
}

// TestReadmeDocumentationMap (FP-3): user and maintainer maps, unlinked
// coming-in-v1 entries, existing paths, every link and anchor offline, and
// no private paths.
func TestReadmeDocumentationMap(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	md := string(repoFile(t, "README.md"))
	look := checkoutLookup(root)
	if err := checkDocMap(md, look); err != nil {
		t.Fatalf("shipped README: %v", err)
	}
	// The shared resolver agrees on file existence, and the example's own
	// index still links all of its manuals (TestDevFlowPackage's check).
	if err := resolveDocLinks(filepath.Join(root, "README.md"), md); err != nil {
		t.Fatal(err)
	}
	if err := readmeLinksManuals(string(repoFile(t, "docs/examples/dev-flow/README.md"))); err != nil {
		t.Fatal(err)
	}
	check := func(md string) error { return checkDocMap(md, look) }
	var ms []readmeMutation
	for _, l := range readmeUserMap {
		ms = append(ms, readmeMutation{"delete-" + l, inSectionRe("Documentation", `\[([^\]]*)\]\(`+regexp.QuoteMeta(l)+`\)`, "$1"), "documentation: missing link " + l})
	}
	for _, l := range readmeMaintainers {
		ms = append(ms, readmeMutation{"delete-" + l, inSectionRe("For maintainers", `\[([^\]]*)\]\(`+regexp.QuoteMeta(l)+`\)`, "$1"), "for maintainers: missing link " + l})
	}
	ms = append(ms,
		readmeMutation{"maintainer-in-user-map", appendToSection("Documentation", "- [CI](docs/maintainers/ci.md): checks."), "Documentation: maintainer link docs/maintainers/ci.md outside For maintainers"},
		readmeMutation{"maintainer-in-quickstart", appendToSection("Quickstart summary", "See the [exercise](docs/maintainers/real-e2e.md#run)."), "Quickstart summary: maintainer link docs/maintainers/real-e2e.md"},
		// Design 12-1b-maintainer-docs: a link restored to a removed
		// pre-relocation path no longer resolves.
		readmeMutation{"old-ci-path", inSection("For maintainers", "](docs/maintainers/ci.md)", "](docs/ci.md)"), "destination docs/ci.md does not exist"},
		readmeMutation{"old-real-e2e-path", inSection("For maintainers", "](docs/maintainers/real-e2e.md)", "](docs/real-e2e.md)"), "destination docs/real-e2e.md does not exist"},
		readmeMutation{"no-harness-note", replaceOnce(readmeHarnessNote, ""), "for maintainers: lacks"},
		readmeMutation{"missing-shipped-quickstart", inSection("Documentation", "- [Quickstart](docs/quickstart.md): install, first dispatch on one machine, then two.\n", ""), "documentation: missing link docs/quickstart.md"},
		readmeMutation{"planned-guide-link", appendToSection("For maintainers", "[guide](docs/guide/index.md)"), "documentation: planned docs/guide/index.md is a link"},
		readmeMutation{"planned-deploy-link", appendToSection("Documentation", "- [deploy](docs/deploy.md)"), "documentation: planned docs/deploy.md is a link"},
		readmeMutation{"planned-unlisted", inSection("Documentation", "`docs/deploy.md`", "deployment"), "documentation: planned docs/deploy.md is not listed"},
		readmeMutation{"remove-coming-label", func(t *testing.T, md string) string {
			s, e := sectionSpan(t, md, "Documentation")
			lines := strings.Split(md[s:e], "\n")
			for i, line := range lines {
				if strings.Contains(line, "`docs/guide/index.md`") {
					lines[i] = replaceOnce("coming in v1", "available")(t, line)
				}
			}
			return md[:s] + strings.Join(lines, "\n") + md[e:]
		}, "documentation: planned docs/guide/index.md is not labeled coming in v1"},
		readmeMutation{"typo-destination", inSection("Documentation", "](docs/coordinator.md)", "](docs/coordinator-missing.md)"), "destination docs/coordinator-missing.md does not exist"},
		readmeMutation{"typo-fragment", replaceOnce("#coordinator-runbook-example)", "#coordinator-runbook-exampel)"), "fragment #coordinator-runbook-exampel matches no heading"},
		readmeMutation{"typo-security-fragment", replaceOnce("#enabling-a-worker)", "#enabling-workers)"), "fragment #enabling-workers matches no heading"},
		readmeMutation{"absolute-link", appendToSection("Documentation", "- [notes](/srv/notes.md)"), "link /srv/notes.md: absolute path"},
		readmeMutation{"private-link", appendToSection("Documentation", "- [notes](/home/someone/notes.md)"), "link /home/someone/notes.md: absolute path"},
		readmeMutation{"file-url", appendToSection("Documentation", "- [notes](file:///etc/hosts)"), "URI scheme"},
		readmeMutation{"escaping-link", appendToSection("Documentation", "- [notes](docs/../../outside.md)"), "escapes the repository"},
		readmeMutation{"design-link", appendToSection("Documentation", "- [design](design/roadmap.md)"), "ignored design tree"},
		readmeMutation{"private-text", appendToSection("Quickstart summary", "Build in /opt/gitspace/callsheet first."), "README: private path sentinel \"/opt/gitspace/\""},
		readmeMutation{"users-text", appendToSection("For maintainers", "Logs live in /Users/me/callsheet."), "README: private path sentinel \"/Users/\""},
	)
	runMutations(t, md, check, ms)

	// A placeholder file for a planned document is rejected; an existing
	// entry path that disappears is rejected.
	withDeploy := func(rel string) ([]byte, bool) {
		if rel == "docs/deploy.md" {
			return []byte("# Deploy\n"), true
		}
		return look(rel)
	}
	if err := checkDocMap(md, withDeploy); err == nil || !strings.Contains(err.Error(), "planned docs/deploy.md exists as a placeholder") {
		t.Errorf("placeholder: err = %v", err)
	}
	withoutCI := func(rel string) ([]byte, bool) {
		if rel == "docs/maintainers/ci.md" {
			return nil, false
		}
		return look(rel)
	}
	if err := checkDocMap(md, withoutCI); err == nil || !strings.Contains(err.Error(), "existing entry docs/maintainers/ci.md is missing") {
		t.Errorf("missing entry: err = %v", err)
	}
}

// TestReadmeCLIPositioning (FP-4): every root help form of the built binary
// carries the README's opening sentence, succeeds silently and identically,
// and creates no state.
func TestReadmeCLIPositioning(t *testing.T) {
	md := string(repoFile(t, "README.md"))
	bin := nodeBinary(t)
	home, cwd := t.TempDir(), t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home + "/.config",
		"XDG_STATE_HOME=" + home + "/.local/state", "XDG_CACHE_HOME=" + home + "/.cache"}
	var forms []rootHelp
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		forms = append(forms, rootHelp{args, runBin(t, bin, cwd, env, args...)})
	}
	if err := checkCLIPositioning(md, forms); err != nil {
		t.Fatalf("shipped help: %v", err)
	}
	if got := append(listTree(t, home), listTree(t, cwd)...); len(got) != 0 {
		t.Fatalf("root help created state: %v", got)
	}
	mutated := func(edit func(fs []rootHelp)) []rootHelp {
		fs := slices.Clone(forms)
		edit(fs)
		return fs
	}
	for _, c := range []struct {
		name   string
		readme string
		forms  []rootHelp
		want   string
	}{
		{"old-tagline", md, mutated(func(fs []rootHelp) {
			for i := range fs {
				fs[i].res.stdout = strings.Replace(fs[i].res.stdout, readmeSentence, readmeOldTagline, 1)
			}
		}), "help: summary \"" + readmeOldTagline + "\""},
		{"readme-opening-only", strings.Replace(md, readmeSentence, "Callsheet is a distributed AI agent dispatcher for teams.", 1), forms, "README opening"},
		{"nonzero-exit", md, mutated(func(fs []rootHelp) { fs[2].res.code = 2 }), "exit 2"},
		{"stderr", md, mutated(func(fs []rootHelp) { fs[3].res.stderr = "warning\n" }), "stderr \"warning\\n\""},
		{"divergent-form", md, mutated(func(fs []rootHelp) { fs[1].res.stdout += "extra\n" }), "help [\"help\"] diverges"},
		{"layout", md, mutated(func(fs []rootHelp) {
			for i := range fs {
				fs[i].res.stdout = strings.Replace(fs[i].res.stdout, "\n\n", "\n", 1)
			}
		}), "help: layout changed"},
		{"private-path", md, mutated(func(fs []rootHelp) {
			for i := range fs {
				fs[i].res.stdout += "see /home/someone\n"
			}
		}), "help: private path sentinel"},
		{"three-forms", md, forms[:3], "help: 3 root forms"},
	} {
		if err := checkCLIPositioning(c.readme, c.forms); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("mutation %s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
}

// TestReadmeValidators is the validators' table test (FP-1..3): missing
// sections, the word cap boundary, path escapes, absent destinations and
// fragments against a controlled lookup, and acceptance of a valid file,
// fragment and the canonical content.
func TestReadmeValidators(t *testing.T) {
	md := string(repoFile(t, "README.md"))
	look := mapLookup(map[string]string{
		"docs/a.md": "# A\n\n## Some heading\n\n```text\n## Fenced heading\n```\n",
		"go.mod":    "module x\n",
	})
	drop := func(name string) string {
		s, e := sectionSpan(t, md, name)
		return md[:s-len("## "+name+"\n")] + md[e:]
	}
	for _, c := range []struct {
		name string
		err  error
		want string // "" accepts
	}{
		{"canonical-entry-point", checkEntryPoint(md), ""},
		{"canonical-support-security", checkSupportAndSecurity(md), ""},
		{"canonical-doc-map", checkDocMap(md, checkoutLookup(testkit.MustRepoRoot(t))), ""},
		{"exactly-750-words", checkStructureAndBudget(padWords(t, md, readmeWordCap)), ""},
		{"751-words", checkStructureAndBudget(padWords(t, md, readmeWordCap+1)), "budget: 751 words"},
		{"missing-security", checkSecurity(drop("Security")), "structure: no \"Security\" section"},
		{"missing-support", checkSupport(drop("Supported CLIs and OSes")), "structure: no \"Supported CLIs and OSes\" section"},
		{"missing-how", checkTopology(drop("How it works")), "structure: no \"How it works\" section"},
		{"missing-quickstart", checkQuickstart(drop("Quickstart summary")), "structure: no \"Quickstart summary\" section"},
		{"missing-documentation", checkDocMap(drop("Documentation"), look), "structure: no \"Documentation\" section"},
		{"missing-maintainers", checkDocMap(drop("For maintainers"), look), "structure: no \"For maintainers\" section"},
		{"missing-title", checkPositioning(strings.TrimPrefix(md, "# Callsheet\n")), "structure:"},
		{"valid-file-and-fragment", checkReadmeLinks("[a](docs/a.md) [b](docs/a.md#some-heading) [c](./docs/a.md#a) [d](#local)\n\n## Local\n", look), ""},
		{"absent-destination", checkReadmeLinks("[a](docs/b.md)", look), "link docs/b.md: destination docs/b.md does not exist"},
		{"directory-destination", checkReadmeLinks("[a](docs)", look), "destination docs does not exist"},
		{"absent-fragment", checkReadmeLinks("[a](docs/a.md#missing)", look), "fragment #missing matches no heading"},
		{"fenced-heading-fragment", checkReadmeLinks("[a](docs/a.md#fenced-heading)", look), "fragment #fenced-heading matches no heading"},
		{"empty-fragment", checkReadmeLinks("[a](docs/a.md#)", look), "fragment # matches no heading"},
		{"absent-local-fragment", checkReadmeLinks("[a](#nowhere)\n\n## Somewhere\n", look), "fragment #nowhere matches no heading"},
		{"path-escape", checkReadmeLinks("[a](../a.md)", look), "escapes the repository"},
		{"nested-escape", checkReadmeLinks("[a](docs/../../a.md)", look), "escapes the repository"},
		{"absolute", checkReadmeLinks("[a](/docs/a.md)", look), "absolute path"},
		{"scheme", checkReadmeLinks("[a](https://example.invalid/a.md)", look), "URI scheme"},
		{"design-tree", checkReadmeLinks("[a](design/x.md)", look), "ignored design tree"},
	} {
		switch {
		case c.want == "" && c.err != nil:
			t.Errorf("%s: rejected: %v", c.name, c.err)
		case c.want != "" && (c.err == nil || !strings.Contains(c.err.Error(), c.want)):
			t.Errorf("%s: err = %v, want one containing %q", c.name, c.err, c.want)
		}
	}
}
