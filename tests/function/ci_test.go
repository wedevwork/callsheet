package function

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wedevwork/callsheet/internal/cicheck"
	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 01b function tests: one top-level TestCI* per FP. They read the
// committed workflow and docs/ci.md only, mutate parsed copies in memory,
// and never contact GitHub or run devcheck recursively.

const (
	checkoutPin = "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	setupGoPin  = "actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417"
)

func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ciWorkflow(t *testing.T) []byte { return repoFile(t, ".github/workflows/ci.yml") }

// node returns the value at a path of mapping keys and sequence indexes.
func node(t *testing.T, n *yaml.Node, path ...any) *yaml.Node {
	t.Helper()
	if n.Kind == yaml.DocumentNode {
		n = n.Content[0]
	}
	for _, p := range path {
		switch k := p.(type) {
		case string:
			var next *yaml.Node
			for i := 0; n.Kind == yaml.MappingNode && i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == k {
					next = n.Content[i+1]
				}
			}
			if next == nil {
				t.Fatalf("no key %q in path %v", k, path)
			}
			n = next
		case int:
			if n.Kind != yaml.SequenceNode || k >= len(n.Content) {
				t.Fatalf("no index %d in path %v", k, path)
			}
			n = n.Content[k]
		}
	}
	return n
}

// deleteKey removes key from mapping m.
func deleteKey(t *testing.T, m *yaml.Node, key string) {
	t.Helper()
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
	t.Fatalf("no key %q", key)
}

func setKey(m *yaml.Node, key, value string) {
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

// mutated parses the checked-in workflow, applies f in memory and returns
// the re-encoded YAML; the file on disk is never rewritten.
func mutated(t *testing.T, f func(root *yaml.Node)) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(ciWorkflow(t), &doc); err != nil {
		t.Fatal(err)
	}
	f(&doc)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustReject(t *testing.T, name string, data []byte, want string) {
	t.Helper()
	err := cicheck.ValidateWorkflow(data)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: got %v, want an error containing %q", name, err, want)
	}
}

// --- devcheck driver helpers (injected runners; no real child tools) ---

type ciRunner struct {
	calls      [][]string
	envs       [][]string
	failOn     string
	coverTotal string
	stdout     string
}

func (r *ciRunner) run(_ context.Context, argv, env []string, _ string, stdout, stderr io.Writer) error {
	r.calls = append(r.calls, argv)
	r.envs = append(r.envs, env)
	joined := strings.Join(argv, " ")
	if r.failOn != "" && strings.Contains(joined, r.failOn) {
		io.WriteString(stderr, "injected child failure\n")
		return errors.New("exit status 1")
	}
	for _, a := range argv {
		if p, ok := strings.CutPrefix(a, "-coverprofile="); ok {
			os.WriteFile(p, []byte("mode: atomic\n"+
				"github.com/wedevwork/callsheet/cmd/callsheet/main.go:1.1,2.2 1 1\n"+
				"github.com/wedevwork/callsheet/cmd/devcheck/main.go:1.1,2.2 1 1\n"+
				"github.com/wedevwork/callsheet/cmd/fake-adapter/main.go:1.1,2.2 1 1\n"), 0o600)
		}
	}
	switch {
	case strings.HasPrefix(joined, "go tool cover"):
		fmt.Fprintf(stdout, "total:\t\t\t(statements)\t%s\n", r.coverTotal)
	case strings.HasPrefix(joined, "go list"):
		io.WriteString(stdout, "github.com/wedevwork/callsheet/cmd/callsheet|1\ngithub.com/wedevwork/callsheet/cmd/devcheck|1\ngithub.com/wedevwork/callsheet/cmd/fake-adapter|1\n")
	case strings.HasPrefix(joined, "go test -json"):
		io.WriteString(stdout, r.stdout)
	}
	return nil
}

// devcheckRun calls devcheck.Run with r and removes any retained scratch.
func devcheckRun(t *testing.T, r *ciRunner, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := devcheck.Run(context.Background(), args, &out, &errOut, r.run)
	for _, line := range strings.Split(out.String(), "\n") {
		if p, ok := strings.CutPrefix(line, "devcheck: scratch "); ok {
			os.RemoveAll(p)
		}
	}
	return code, out.String(), errOut.String()
}

// --- synthetic go test -json events (clearly marked, never evidence) ---

func synth(action, pkg, test string) map[string]any {
	e := map[string]any{"Action": action, "Package": pkg, "Synthetic": "01b function-test event"}
	if test != "" {
		e["Test"] = test
	}
	return e
}

func events(evs ...map[string]any) string {
	var b strings.Builder
	for _, e := range evs {
		j, _ := json.Marshal(e)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

// qualifyingEvents is a complete synthetic FP-6 and plane trust stream;
// drop names a scenario whose events are deleted and skip one that is
// skipped instead.
func qualifyingEvents(drop, skip string) string {
	pkg := devcheck.NativePackage
	evs := []map[string]any{synth("start", pkg, "")}
	for _, name := range devcheck.NativeRequiredTests() {
		if strings.HasPrefix(name, "TestPlane") {
			evs = append(evs, synth("run", pkg, name), synth("pass", pkg, name))
		}
	}
	evs = append(evs, synth("run", pkg, "TestFP6ProcessGroups"))
	for _, s := range []string{"cooperative", "resistant", "leader-exits-first"} {
		name := "TestFP6ProcessGroups/" + s
		switch s {
		case drop:
		case skip:
			evs = append(evs, synth("run", pkg, name), synth("skip", pkg, name))
		default:
			evs = append(evs, synth("run", pkg, name), synth("pass", pkg, name))
		}
	}
	return events(append(evs, synth("pass", pkg, "TestFP6ProcessGroups"), synth("pass", pkg, ""))...)
}

// --- docs/ci.md helpers ---

func docSection(t *testing.T, heading string) string {
	t.Helper()
	doc := string(repoFile(t, "docs/ci.md"))
	start := strings.Index(doc, "\n## "+heading+"\n")
	if start < 0 {
		t.Fatalf("docs/ci.md has no section %q", heading)
	}
	body := doc[start+len("\n## "+heading+"\n"):]
	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}
	return body
}

var stepRE = regexp.MustCompile(`(?m)^(\d+)\. (.*)$`)

func numberedSteps(t *testing.T, section string) []string {
	t.Helper()
	var steps []string
	for i, m := range stepRE.FindAllStringSubmatch(section, -1) {
		if m[1] != fmt.Sprint(i+1) {
			t.Fatalf("procedure steps not numbered in order: %v", m[0])
		}
		steps = append(steps, m[2])
	}
	return steps
}

// stepIndex returns the index of the first step containing every term.
func stepIndex(t *testing.T, steps []string, terms ...string) int {
	t.Helper()
	for i, s := range steps {
		ok := true
		for _, term := range terms {
			ok = ok && strings.Contains(s, term)
		}
		if ok {
			return i
		}
	}
	t.Fatalf("no procedure step contains %q in %q", terms, steps)
	return -1
}

// requireTerms checks each term appears in text, comparing with all runs of
// whitespace collapsed so Markdown line wrapping does not matter.
func requireTerms(t *testing.T, where, text string, terms ...string) {
	t.Helper()
	flat := strings.Join(strings.Fields(text), " ")
	for _, term := range terms {
		if !strings.Contains(flat, strings.Join(strings.Fields(term), " ")) {
			t.Fatalf("%s lacks %q", where, term)
		}
	}
}

// FP-1: triggers, two stable checks, pinned setup, budget and permissions.
func TestCIWorkflowContract(t *testing.T) {
	data := ciWorkflow(t)
	if err := cicheck.ValidateWorkflow(data); err != nil {
		t.Fatalf("checked-in workflow: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"push", "pull_request"} {
		if b := node(t, &doc, "on", ev, "branches"); len(b.Content) != 1 || b.Content[0].Value != "main" {
			t.Fatalf("on.%s.branches = %v", ev, b.Content)
		}
	}
	if node(t, &doc, "permissions", "contents").Value != "read" {
		t.Fatal("permissions")
	}
	var names []string
	for _, j := range cicheck.Jobs() {
		names = append(names, node(t, &doc, "jobs", j.ID, "name").Value)
		if node(t, &doc, "jobs", j.ID, "timeout-minutes").Value != fmt.Sprint(j.TimeoutMinutes) {
			t.Fatalf("%s timeout", j.ID)
		}
		co, sg := node(t, &doc, "jobs", j.ID, "steps", 0, "uses"), node(t, &doc, "jobs", j.ID, "steps", 1, "uses")
		if co.Value != checkoutPin || co.LineComment != "# v6.0.2" || sg.Value != setupGoPin || sg.LineComment != "# v6.3.0" {
			t.Fatalf("%s pins = %q %q / %q %q", j.ID, co.Value, co.LineComment, sg.Value, sg.LineComment)
		}
		if node(t, &doc, "jobs", j.ID, "steps", 1, "with", "go-version-file").Value != "go.mod" || node(t, &doc, "jobs", j.ID, "steps", 1, "with", "cache").Value != "true" {
			t.Fatalf("%s setup-go inputs", j.ID)
		}
		if node(t, &doc, "jobs", j.ID, "steps", 2, "run").Value != "go mod download" {
			t.Fatalf("%s download step", j.ID)
		}
	}
	if strings.Join(names, ",") != "ci-linux,ci-macos" || strings.Join(cicheck.RequiredChecks(), ",") != "ci-linux,ci-macos" {
		t.Fatalf("check names = %v", names)
	}
	if strings.Contains(string(data), "pull_request_target") {
		t.Fatal("pull_request_target present")
	}
	mustReject(t, "trigger removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "on"), "push") }), "on.push: missing required field")
	mustReject(t, "pin removed", mutated(t, func(r *yaml.Node) {
		node(t, r, "jobs", "macos", "steps", 1, "uses").Value = "actions/setup-go@v6"
	}), `jobs.macos.steps[1].uses: must pin a full 40-hex commit SHA, got "v6"`)
	mustReject(t, "permission elevated", mutated(t, func(r *yaml.Node) { node(t, r, "permissions", "contents").Value = "write" }), "permissions.contents")
	mustReject(t, "timeout removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs", "linux"), "timeout-minutes") }), "jobs.linux.timeout-minutes: missing required field")
}

// FP-2: the Linux job runs devcheck test (then race), coverage, bench, cross
// and (01c) stress.
func TestCILinuxBar(t *testing.T) {
	stages, err := cicheck.ExtractStages(ciWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stages["linux"], " "); got != "test coverage bench cross stress" || got != strings.Join(cicheck.Jobs()[0].Stages, " ") {
		t.Fatalf("linux stages = %q", got)
	}
	plan := devcheck.TestSteps("linux")
	if len(plan) != 2 || strings.Join(plan[0].Argv, " ") != "go test -count=1 -timeout=180s ./..." ||
		strings.Join(plan[1].Argv, " ") != "go test -race -count=1 -timeout=180s ./..." || strings.Join(plan[1].Env, " ") != "CGO_ENABLED=1" {
		t.Fatalf("linux test plan = %+v", plan)
	}
	r := &ciRunner{}
	if code, _, errOut := devcheckRun(t, r, "test"); code != 0 {
		t.Fatalf("test = %d %s", code, errOut)
	}
	host := devcheck.TestSteps(runtime.GOOS)
	if len(r.calls) != len(host) {
		t.Fatalf("calls = %v", r.calls)
	}
	for i, s := range host {
		if strings.Join(r.calls[i], " ") != strings.Join(s.Argv, " ") {
			t.Fatalf("call %d = %v", i, r.calls[i])
		}
	}
	if runtime.GOOS == "linux" && (len(r.calls) != 2 || r.calls[1][2] != "-race") {
		t.Fatalf("linux must run test then race: %v", r.calls)
	}
	// Coverage: exactly 80.0% fails, anything above passes.
	code, _, errOut := devcheckRun(t, &ciRunner{coverTotal: "80.0%"}, "coverage")
	if code != 1 || !strings.Contains(errOut, "coverage 80.0% is not greater than 80.0%") {
		t.Fatalf("80.0%% = %d %s", code, errOut)
	}
	if code, _, errOut := devcheckRun(t, &ciRunner{coverTotal: "80.1%"}, "coverage"); code != 0 {
		t.Fatalf("80.1%% = %d %s", code, errOut)
	}
	// Bench and cross child errors propagate as failed stages.
	code, _, errOut = devcheckRun(t, &ciRunner{failOn: "-bench"}, "bench")
	if code != 1 || !strings.Contains(errOut, "stage bench FAILED") || !strings.Contains(errOut, "injected child failure") {
		t.Fatalf("bench = %d %s", code, errOut)
	}
	r = &ciRunner{failOn: "darwin-arm64"}
	code, _, errOut = devcheckRun(t, r, "cross")
	if code != 1 || !strings.Contains(errOut, "stage cross FAILED") || !strings.Contains(errOut, "darwin-arm64") {
		t.Fatalf("cross = %d %s", code, errOut)
	}
}

// FP-3: the macOS job runs native, which requires all three scenarios, and
// (01c) then stress.
func TestCIDarwinQualification(t *testing.T) {
	data := ciWorkflow(t)
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if node(t, &doc, "jobs", "macos", "runs-on").Value != "macos-15" || node(t, &doc, "jobs", "macos", "name").Value != "ci-macos" {
		t.Fatal("macos job identity")
	}
	stages, err := cicheck.ExtractStages(data)
	if err != nil || strings.Join(stages["macos"], " ") != "native stress" || strings.Join(cicheck.Jobs()[1].Stages, " ") != "native stress" {
		t.Fatalf("macos stages = %v %v", stages, err)
	}
	steps, err := devcheck.NativeSteps("darwin")
	if err != nil || len(steps) != 1 || strings.Join(steps[0].Argv, " ") != "go test -json -count=1 -timeout=180s ./..." {
		t.Fatalf("native plan = %+v %v", steps, err)
	}
	if got := strings.Join(devcheck.NativeRequiredTests()[:4], ","); got != "TestFP6ProcessGroups,TestFP6ProcessGroups/cooperative,TestFP6ProcessGroups/resistant,TestFP6ProcessGroups/leader-exits-first" {
		t.Fatalf("required = %s", got)
	}
	if err := devcheck.CheckNativeResults("darwin", strings.NewReader(qualifyingEvents("", ""))); err != nil {
		t.Fatalf("three-case stream: %v", err)
	}
	for _, s := range []string{"cooperative", "resistant", "leader-exits-first"} {
		for name, stream := range map[string]string{"deleted": qualifyingEvents(s, ""), "skipped": qualifyingEvents("", s)} {
			err := devcheck.CheckNativeResults("darwin", strings.NewReader(stream))
			if err == nil || !strings.Contains(err.Error(), "native qualification unobserved") || !strings.Contains(err.Error(), "TestFP6ProcessGroups/"+s) {
				t.Fatalf("%s %s: %v", name, s, err)
			}
		}
	}
}

// FP-4: Linux/macOS only: four targets, full trees, unsupported-OS rejection.
func TestCIPlatformScope(t *testing.T) {
	var got []string
	for _, tg := range devcheck.Matrix {
		got = append(got, tg.String())
	}
	if strings.Join(got, " ") != "linux/amd64 linux/arm64 darwin/amd64 darwin/arm64" {
		t.Fatalf("matrix = %v", got)
	}
	plan, err := devcheck.CrossPlan("/out", devcheck.Matrix)
	if err != nil || len(plan) != 12 {
		t.Fatalf("cross plan = %d %v", len(plan), err)
	}
	for _, s := range plan {
		if strings.Contains(strings.Join(s.Argv, " "), "windows") || strings.Contains(strings.Join(s.Argv, " "), ".exe") {
			t.Fatalf("windows artifact planned: %v", s.Argv)
		}
	}
	for _, goarch := range []string{"amd64", "arm64"} {
		r := &ciRunner{}
		err := devcheck.Cross(context.Background(), r.run, t.TempDir(), []devcheck.Target{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "windows", GOARCH: goarch}})
		if err == nil || !strings.Contains(err.Error(), "unsupported target windows/"+goarch) || len(r.calls) != 0 {
			t.Fatalf("windows/%s: %v after %d runner calls", goarch, err, len(r.calls))
		}
	}
	leafPaths := func(goos string) []string {
		var out []string
		for _, l := range cli.NewTree(goos).Leaves() {
			out = append(out, l.Path())
		}
		return out
	}
	linux, darwin := leafPaths("linux"), leafPaths("darwin")
	if len(linux) != 32 || strings.Join(linux, "|") != strings.Join(darwin, "|") {
		t.Fatalf("trees: linux %d darwin %d", len(linux), len(darwin))
	}
	for _, goos := range []string{"windows", "freebsd", ""} {
		if cli.NewTree(goos) != nil {
			t.Fatalf("NewTree(%q) is not nil", goos)
		}
	}
	bin := testkit.BuildBinary(t, "./cmd/callsheet", "callsheet")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	for _, g := range []string{"plane", "sidecar"} {
		r := runBin(t, bin, t.TempDir(), env, g)
		if r.code != 0 || r.stderr != "" || !strings.HasPrefix(r.stdout, "Usage: callsheet "+g+" <command>\n") {
			t.Fatalf("host %s %s = %+v", runtime.GOOS, g, r)
		}
	}
	doc := string(repoFile(t, "docs/ci.md"))
	requireTerms(t, "docs/ci.md", doc, "all v1 components are Linux/macOS only", "Windows coordinator support is backlog E9, lowest priority")
}

// FP-5: the native driver stage accepts only complete, passing evidence.
func TestCINativeEvidence(t *testing.T) {
	fixture := string(repoFile(t, "internal/devcheck/testdata/cli-go-test.jsonl"))
	valid := fixture + qualifyingEvents("", "")
	bad := map[string]string{
		"missing":   fixture,
		"skip":      fixture + qualifyingEvents("", "resistant"),
		"malformed": valid[:len(valid)-7],
		"fail":      valid + events(synth("fail", "example.com/late", "")),
	}
	if err := devcheck.CheckNativeResults("darwin", strings.NewReader(valid)); err != nil {
		t.Fatalf("valid evidence: %v", err)
	}
	for name, s := range bad {
		if err := devcheck.CheckNativeResults("darwin", strings.NewReader(s)); err == nil {
			t.Fatalf("%s evidence accepted", name)
		}
	}
	if err := devcheck.CheckNativeResults("linux", strings.NewReader(valid)); err == nil || !strings.Contains(err.Error(), "darwin only") {
		t.Fatalf("linux parser: %v", err)
	}
	if _, err := devcheck.NativeSteps("linux"); err == nil {
		t.Fatal("linux native plan accepted")
	}
	if runtime.GOOS != "darwin" {
		// The host cannot qualify: the stage is rejected before any child.
		r := &ciRunner{stdout: valid}
		code, _, errOut := devcheckRun(t, r, "native")
		if code != 1 || len(r.calls) != 0 || !strings.Contains(errOut, "native stage is unsupported on "+fmt.Sprintf("%q", runtime.GOOS)) {
			t.Fatalf("%s native = %d calls=%d %s", runtime.GOOS, code, len(r.calls), errOut)
		}
		return
	}
	r := &ciRunner{stdout: valid}
	code, out, errOut := devcheckRun(t, r, "native")
	if code != 0 || len(r.calls) != 1 || !strings.Contains(out, "native qualification passed on darwin/"+runtime.GOARCH) {
		t.Fatalf("darwin native = %d %s", code, errOut)
	}
	if code, _, errOut := devcheckRun(t, &ciRunner{failOn: "-json"}, "native"); code != 1 || !strings.Contains(errOut, "injected child failure") {
		t.Fatalf("child error = %d %s", code, errOut)
	}
	for name, s := range bad {
		if code, _, _ := devcheckRun(t, &ciRunner{stdout: s}, "native"); code != 1 {
			t.Fatalf("%s evidence passed the native stage", name)
		}
	}
}

// FP-6: offline structural and stage-drift validation of the real workflow.
func TestCIWorkflowDrift(t *testing.T) {
	data := ciWorkflow(t)
	if err := cicheck.ValidateWorkflow(data); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(testkit.MustRepoRoot(t), ".github", "workflows")
	all := map[string][]byte{}
	for _, pat := range []string{"*.yml", "*.yaml"} {
		files, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, f := range files {
			all[filepath.Base(f)] = repoFile(t, ".github/workflows/"+filepath.Base(f))
		}
	}
	if len(all) == 0 {
		t.Fatal("no workflows found")
	}
	if err := cicheck.CheckJobNames(all); err != nil {
		t.Fatalf("job names: %v", err)
	}
	dup := map[string][]byte{"ci.yml": data, "extra.yml": []byte("jobs:\n  other:\n    name: ci-macos\n")}
	if err := cicheck.CheckJobNames(dup); err == nil || !strings.Contains(err.Error(), `"ci-macos" is used by both`) {
		t.Fatalf("duplicate check name: %v", err)
	}
	// Every extracted stage belongs to the actual dispatch.
	stages, err := cicheck.ExtractStages(data)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := strings.Join(devcheck.Stages(), " ")
	for job, ss := range stages {
		for _, s := range ss {
			if !strings.Contains(" "+dispatch+" ", " "+s+" ") {
				t.Fatalf("%s uses stage %q outside dispatch %q", job, s, dispatch)
			}
			code, _, errOut := devcheckRun(t, &ciRunner{failOn: "go"}, s)
			if code == 2 || strings.Contains(errOut, "unknown subcommand") {
				t.Fatalf("stage %q not recognized by dispatch: %s", s, errOut)
			}
		}
	}
	mustReject(t, "nonexistent stage", mutated(t, func(r *yaml.Node) {
		node(t, r, "jobs", "macos", "steps", 3, "run").Value = "go run ./cmd/devcheck race"
	}), `jobs.macos.steps[3].run: unknown devcheck stage "race"`)
	mustReject(t, "omitted coverage gate", mutated(t, func(r *yaml.Node) {
		s := node(t, r, "jobs", "linux", "steps")
		s.Content = append(s.Content[:4], s.Content[5:]...)
	}), `jobs.linux.steps: missing check step for devcheck stage "coverage"`)
	mustReject(t, "permissive step condition", mutated(t, func(r *yaml.Node) {
		setKey(node(t, r, "jobs", "linux", "steps", 4), "if", "always()")
	}), "jobs.linux.steps[4].if: unknown field")
	mustReject(t, "permissive job", mutated(t, func(r *yaml.Node) {
		setKey(node(t, r, "jobs", "macos"), "continue-on-error", "true")
	}), "jobs.macos.continue-on-error: unknown field")
}

// FP-7: the owner-applied branch protection handoff.
func TestCIProtectionHandoff(t *testing.T) {
	checks := docSection(t, "Checks")
	contexts := regexp.MustCompile("`(ci-[a-z0-9-]+)`").FindAllStringSubmatch(checks, -1)
	var listed []string
	seen := map[string]bool{}
	for _, m := range contexts {
		if !seen[m[1]] {
			seen[m[1]] = true
			listed = append(listed, m[1])
		}
	}
	if strings.Join(listed, ",") != strings.Join(cicheck.RequiredChecks(), ",") {
		t.Fatalf("Checks lists %v, want exactly %v", listed, cicheck.RequiredChecks())
	}
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp,
		"wedevwork/callsheet", "branch name pattern `main`",
		"Require a pull request before merging",
		"Require status checks to pass before merging: `ci-linux`, `ci-macos`",
		"Require branches to be up to date before merging",
		"Do not allow bypassing the above settings", "include administrators",
		"no force pushes", "no branch deletion",
		"gh api repos/wedevwork/callsheet/branches/main/protection",
		"required_status_checks.strict", "enforce_admins", "rulesets")
	doc := string(repoFile(t, "docs/ci.md"))
	for _, forbidden := range []string{"ci-windows", "-X PUT", "--method PUT", "-X POST", "--method POST"} {
		if strings.Contains(doc, forbidden) {
			t.Fatalf("docs/ci.md contains %q", forbidden)
		}
	}
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "protection", "api.github.com", "secrets.", "administration"} {
		if strings.Contains(wf, forbidden) {
			t.Fatalf("workflow contains %q", forbidden)
		}
	}
}

// FP-8: PR procedure, bootstrap and first-run evidence. The bootstrap is the
// settled sequence: iterations 01, 01b and 01c join pull request #1 and merge
// together once both checks succeed on its current merge revision.
func TestCIPRProcedure(t *testing.T) {
	pr := docSection(t, "PR flow")
	requireTerms(t, "PR flow", pr, "`iter-NN-<slug>`", "`iter-02-plane-trust`", "Bootstrap exception",
		"Every iteration lands on `main` through a pull request",
		"iterations 01, 01b and 01c join pull request #1",
		"merge together after both checks, `ci-linux` and `ci-macos`, succeed on its current merge revision",
		"enables branch protection after both contexts are available and successful",
		"Do not begin merging iteration 02 before that handoff is complete",
		"No fake first-run evidence", "merge queue")
	for _, stale := range []string{"land directly on `main`", "before CI exists", "From iteration 02 on"} {
		if strings.Contains(strings.Join(strings.Fields(pr), " "), stale) {
			t.Fatalf("PR flow keeps the superseded bootstrap narrative %q", stale)
		}
	}
	steps := numberedSteps(t, pr)
	if len(steps) < 5 {
		t.Fatalf("steps = %q", steps)
	}
	review := stepIndex(t, steps, "code review", "REVIEW_APPROVED")
	commit := stepIndex(t, steps, "Commit the reviewed code")
	open := stepIndex(t, steps, "open a pull request targeting `main`")
	green := stepIndex(t, steps, "both checks", "succeed on the current PR merge revision")
	merge := stepIndex(t, steps, "Merge only after both checks are green")
	if !(review < commit && commit < open && open < green && green < merge) {
		t.Fatalf("procedure order review=%d commit=%d open=%d green=%d merge=%d", review, commit, open, green, merge)
	}
	requireTerms(t, "green step", steps[green], "skipped, canceled, pending or unobserved check is not acceptable")
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "pending until observed", "run URL", "commit",
		"conclusions of both `ci-linux` and `ci-macos`", "native evidence", "native qualification passed on darwin",
		"branch protection verification",
		"git ls-remote https://github.com/actions/checkout.git 'refs/tags/v6.0.2' 'refs/tags/v6.0.2^{}'",
		"git ls-remote https://github.com/actions/setup-go.git 'refs/tags/v6.3.0' 'refs/tags/v6.3.0^{}'",
		"peeled commit", "handoff blocker")
	requireTerms(t, "First remote run", first, "stress evidence from both logs", "`devcheck: stage stress ok`", "elapsed time of each stress step")
	local := docSection(t, "Local verification")
	requireTerms(t, "Local verification", local, "go run ./cmd/devcheck all", "go run ./cmd/devcheck stress", "actionlint", "not a required dependency")
}
