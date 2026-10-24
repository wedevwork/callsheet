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
	"slices"
	"strings"
	"sync"
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

// ciRunner records calls under mu, which it never holds while a call writes
// output, so the concurrent processgroup shard can use it; read calls only
// after devcheck has returned.
type ciRunner struct {
	mu         sync.Mutex
	calls      [][]string
	envs       [][]string
	failOn     string
	coverTotal string
	// stdout is the first native command's stream and reale2eStdout the
	// dedicated internal/reale2e command's (design
	// 12a-native-reale2e-budget): each child writes its own part.
	stdout        string
	reale2eStdout string
}

// manifestBlocks is one covered profile block per coverage manifest
// selection (iterations 09a and 09b; native-only entries of both OS
// values are present, each host evaluating its own): the synthetic
// profile must satisfy that gate too.
func manifestBlocks() string {
	var b strings.Builder
	for _, e := range devcheck.WorkspaceCoverageManifest {
		ranges := e.Ranges
		if len(ranges) == 0 {
			ranges = [][2]int{{1, 1}}
		}
		for _, r := range ranges {
			fmt.Fprintf(&b, "%s:%d.1,%d.2 1 1\n", e.File, r[0], r[1])
		}
	}
	return b.String()
}

func (r *ciRunner) run(_ context.Context, argv, env []string, _ string, stdout, stderr io.Writer) error {
	r.mu.Lock()
	r.calls = append(r.calls, argv)
	r.envs = append(r.envs, env)
	r.mu.Unlock()
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
				"github.com/wedevwork/callsheet/cmd/fake-adapter/main.go:1.1,2.2 1 1\n"+
				"github.com/wedevwork/callsheet/cmd/mcpqual/main.go:1.1,2.2 1 1\n"+manifestBlocks()), 0o600)
		}
	}
	// A cross build writes a synthetic executable for its GOOS/GOARCH, so
	// the cross stage's verification sees a complete, correct build.
	if _, err := testkit.FakeGoBuild(argv, env); err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(joined, "go tool cover"):
		fmt.Fprintf(stdout, "total:\t\t\t(statements)\t%s\n", r.coverTotal)
	case strings.HasPrefix(joined, "go list"):
		io.WriteString(stdout, "github.com/wedevwork/callsheet/cmd/callsheet|1\ngithub.com/wedevwork/callsheet/cmd/devcheck|1\ngithub.com/wedevwork/callsheet/cmd/fake-adapter|1\n"+
			"github.com/wedevwork/callsheet/cmd/mcpqual|1\n")
	case strings.HasPrefix(joined, "go test -json") && slices.Contains(argv, "./internal/reale2e"):
		io.WriteString(stdout, r.reale2eStdout)
	case strings.HasPrefix(joined, "go test -json"):
		io.WriteString(stdout, r.stdout)
	}
	return nil
}

// evidenceOpts returns devcheck options with a fresh temporary evidence
// directory: no test creates, locks or writes the command's fixed path.
func evidenceOpts(t *testing.T) devcheck.RunOptions {
	t.Helper()
	return devcheck.RunOptions{EvidenceDir: filepath.Join(t.TempDir(), "evidence")}
}

// devcheckRun calls devcheck.Run with r and a fresh evidence directory and
// removes any retained scratch.
func devcheckRun(t *testing.T, r *ciRunner, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := devcheck.Run(context.Background(), args, &out, &errOut, r.run, evidenceOpts(t))
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

// reale2ePkg independently pins devcheck.NativeReale2ePackage.
const reale2ePkg = "github.com/wedevwork/callsheet/internal/reale2e"

// reale2eEvents is the dedicated second native command's complete
// synthetic stream: internal/reale2e's sole start, one test and the
// package pass (design 12a-native-reale2e-budget).
func reale2eEvents() string {
	return events(synth("start", reale2ePkg, ""), synth("run", reale2ePkg, "TestRealE2ESynthetic"), synth("pass", reale2ePkg, "TestRealE2ESynthetic"),
		synth("pass", reale2ePkg, ""))
}

// qualifyingEvents is the complete synthetic combined native stream: the
// first command's (sharedQualifyingEvents) followed by the reale2e
// command's.
func qualifyingEvents(drop, skip string) string {
	return sharedQualifyingEvents(drop, skip) + reale2eEvents()
}

// sharedQualifyingEvents is a complete synthetic first-command stream:
// FP-6, plane trust, node and every later required name, then the sidecar
// tuple; drop names a scenario whose events are deleted and skip one that
// is skipped instead.
func sharedQualifyingEvents(drop, skip string) string {
	pkg := devcheck.NativePackage
	evs := []map[string]any{synth("start", pkg, "")}
	for _, name := range devcheck.NativeRequiredTests() {
		if strings.HasPrefix(name, "TestPlane") || strings.HasPrefix(name, "TestNode") || strings.HasPrefix(name, "TestRole") || strings.HasPrefix(name, "TestTask") ||
			strings.HasPrefix(name, "TestControl") || strings.HasPrefix(name, "TestMCP") || strings.HasPrefix(name, "TestRealAdapter") || strings.HasPrefix(name, "TestWorkspace") ||
			strings.HasPrefix(name, "TestWave2") || strings.HasPrefix(name, "TestWaitUntilDone") || name == "TestCoordinatorBackgroundWait" || name == "TestShortPollCatalogPolicy" {
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
	evs = append(evs, synth("pass", pkg, "TestFP6ProcessGroups"), synth("pass", pkg, ""))
	return events(append(evs, taskProcessEvents("")...)...)
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

// FP-1: triggers, the stable required checks (four since iteration 02b,
// two of them summaries since 02c), pinned setup, budget and permissions.
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
		if j.Required {
			names = append(names, node(t, &doc, "jobs", j.ID, "name").Value)
		}
		if node(t, &doc, "jobs", j.ID, "timeout-minutes").Value != fmt.Sprint(j.TimeoutMinutes) {
			t.Fatalf("%s timeout", j.ID)
		}
		if len(j.Needs) > 0 {
			// A summary has no setup: its one step evaluates results.
			if len(node(t, &doc, "jobs", j.ID, "steps").Content) != 1 {
				t.Fatalf("%s is not a one-step summary", j.ID)
			}
			continue
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
	const four = "ci-linux,ci-macos,ci-linux-stress,ci-macos-stress"
	if strings.Join(names, ",") != four || strings.Join(cicheck.RequiredChecks(), ",") != four {
		t.Fatalf("check names = %v", names)
	}
	if strings.Contains(string(data), "pull_request_target") {
		t.Fatal("pull_request_target present")
	}
	// FP-12 (m3-m4-container-e2e): the Linux job's eighth and final step
	// publishes the retained container evidence report to the job log,
	// always, with exactly name, if and run; no other job has it.
	ev := node(t, &doc, "jobs", "linux", "steps", 7)
	if len(node(t, &doc, "jobs", "linux", "steps").Content) != 8 || len(ev.Content) != 6 ||
		node(t, ev, "name").Value != cicheck.ContainerEvidenceName || node(t, ev, "if").Value != "always()" ||
		node(t, ev, "run").Value != "cat /tmp/callsheet-container-e2e-evidence/report.txt" || cicheck.ContainerEvidenceIf != "always()" ||
		cicheck.ContainerEvidenceRun != "cat /tmp/callsheet-container-e2e-evidence/report.txt" || cicheck.ContainerEvidenceName != "Publish container E2E evidence" {
		t.Fatalf("linux evidence step = %+v", ev)
	}
	if got := node(t, &doc, "jobs", "linux", "steps", 6, "run").Value; got != "go run ./cmd/devcheck cross" {
		t.Fatalf("devcheck cross is not index 6: %q", got)
	}
	for _, j := range cicheck.Jobs() {
		if j.ID == "linux" {
			continue
		}
		for i, st := range node(t, &doc, "jobs", j.ID, "steps").Content {
			if strings.Contains(fmt.Sprint(yamlScalars(st)), "callsheet-container-e2e-evidence") {
				t.Fatalf("%s step %d publishes container evidence", j.ID, i)
			}
		}
	}
	mustReject(t, "evidence step omitted", mutated(t, func(r *yaml.Node) {
		s := node(t, r, "jobs", "linux", "steps")
		s.Content = s.Content[:7]
	}), "jobs.linux.steps: must have exactly 8 steps (checkout, setup-go, go mod download, then devcheck test, coverage, bench, cross, then container evidence), got 7")
	mustReject(t, "evidence path", mutated(t, func(r *yaml.Node) {
		node(t, r, "jobs", "linux", "steps", 7, "run").Value = "cat /tmp/other/report.txt"
	}), `jobs.linux.steps[7].run: must be exactly "cat /tmp/callsheet-container-e2e-evidence/report.txt"`)
	mustReject(t, "evidence condition", mutated(t, func(r *yaml.Node) {
		node(t, r, "jobs", "linux", "steps", 7, "if").Value = "${{ always() }}"
	}), `jobs.linux.steps[7].if: must be exactly "always()"`)
	mustReject(t, "evidence extra field", mutated(t, func(r *yaml.Node) {
		setKey(node(t, r, "jobs", "linux", "steps", 7), "continue-on-error", "true")
	}), "jobs.linux.steps[7].continue-on-error: unknown field")
	mustReject(t, "trigger removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "on"), "push") }), "on.push: missing required field")
	mustReject(t, "pin removed", mutated(t, func(r *yaml.Node) {
		node(t, r, "jobs", "macos", "steps", 1, "uses").Value = "actions/setup-go@v6"
	}), `jobs.macos.steps[1].uses: must pin a full 40-hex commit SHA, got "v6"`)
	mustReject(t, "permission elevated", mutated(t, func(r *yaml.Node) { node(t, r, "permissions", "contents").Value = "write" }), "permissions.contents")
	mustReject(t, "timeout removed", mutated(t, func(r *yaml.Node) { deleteKey(t, node(t, r, "jobs", "linux"), "timeout-minutes") }), "jobs.linux.timeout-minutes: missing required field")
}

// FP-2: the Linux job runs devcheck test (then race), coverage, bench and
// cross; its stress moved to ci-linux-stress (iteration 02b).
// Design 12a-ci-race-schedule adds the serial whole-module race subtests;
// since code review round 5 of design 12a-dev-flow-verify (owner
// approved) the whole module's race suite is two steps: every package but
// internal/reale2e serialized under 180 s, then internal/reale2e alone
// under 360 s (internal/devcheck's TestRaceSelectionCoversModule keeps the
// two selections equal to ./...).
func TestCILinuxBar(t *testing.T) {
	const wantRace = "go test -race -p=1 -count=1 -timeout=180s ./cmd/... ./internal/adapter/... ./internal/cicheck/... ./internal/cli/... " +
		"./internal/client/... ./internal/contract/... ./internal/devcheck/... ./internal/logging/... ./internal/mcp/... ./internal/mcpqual/... " +
		"./internal/plane/... ./internal/sidecar/... ./internal/spikes/... ./internal/taskpublication/... ./internal/taskworkspace/... " +
		"./internal/testkit/... ./internal/workspace/... ./internal/workspacetransfer/... ./tests/..."
	const wantRaceReale2e = "go test -race -count=1 -timeout=360s ./internal/reale2e"
	t.Run("race-schedule", func(t *testing.T) {
		stages, err := cicheck.ExtractStages(ciWorkflow(t))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(stages["linux"], " "); got != "test coverage bench cross" || got != strings.Join(cicheck.Jobs()[0].Stages, " ") {
			t.Fatalf("linux stages = %q", got)
		}
		plan := devcheck.TestSteps("linux")
		if len(plan) < 3 || plan[1].Name != "test -race" || strings.Join(plan[1].Argv, " ") != wantRace ||
			strings.Join(plan[1].Env, " ") != "CGO_ENABLED=1" || strings.Contains(wantRace, "reale2e") ||
			plan[2].Name != "test -race reale2e" || strings.Join(plan[2].Argv, " ") != wantRaceReale2e || strings.Join(plan[2].Env, " ") != "CGO_ENABLED=1" {
			t.Fatalf("linux race steps = %+v", plan)
		}
	})
	t.Run("race-stage-compatibility", func(t *testing.T) {
		plan := devcheck.TestSteps("linux")
		if len(plan) != 5 ||
			plan[0].Name != "test" || strings.Join(plan[0].Argv, " ") != "go test -count=1 -timeout=180s ./..." || len(plan[0].Env) != 0 ||
			plan[1].Name != "test -race" || strings.Join(plan[1].Argv, " ") != wantRace || strings.Join(plan[1].Env, " ") != "CGO_ENABLED=1" ||
			plan[2].Name != "test -race reale2e" || strings.Join(plan[2].Argv, " ") != wantRaceReale2e || strings.Join(plan[2].Env, " ") != "CGO_ENABLED=1" ||
			plan[3].Name != "test realadaptercheck" || strings.Join(plan[3].Argv, " ") != "go test -tags=realadaptercheck ./internal/sidecar -run=^TestRealAdapterLocal$ -count=1" || len(plan[3].Env) != 0 ||
			plan[4].Name != "test realadaptercheck -race" || strings.Join(plan[4].Argv, " ") != "go test -race -tags=realadaptercheck ./internal/sidecar -run=^TestRealAdapterLocal$ -count=1" || strings.Join(plan[4].Env, " ") != "CGO_ENABLED=1" {
			t.Fatalf("linux test plan = %+v", plan)
		}
		darwin := devcheck.TestSteps("darwin")
		if len(darwin) != 2 ||
			darwin[0].Name != "test" || strings.Join(darwin[0].Argv, " ") != "go test -count=1 -timeout=180s ./..." || len(darwin[0].Env) != 0 ||
			darwin[1].Name != "test realadaptercheck" || strings.Join(darwin[1].Argv, " ") != "go test -tags=realadaptercheck ./internal/sidecar -run=^TestRealAdapterLocal$ -count=1" || len(darwin[1].Env) != 0 ||
			strings.Contains(strings.Join(darwin[1].Argv, " "), "-race") {
			t.Fatalf("darwin test plan = %+v", darwin)
		}
		// The host's test plan (m3-m4-container-e2e: plan-level; the Linux
		// stage's added container operation is driven by internal/devcheck's
		// container driver contract tests, never from here).
		host := devcheck.TestSteps(runtime.GOOS)
		if runtime.GOOS == "linux" && (len(host) != 5 || host[1].Argv[2] != "-race" || host[1].Argv[3] != "-p=1" || host[2].Argv[2] != "-race" || host[4].Argv[2] != "-race") {
			t.Fatalf("linux must run test then race: %+v", host)
		}
		if runtime.GOOS == "darwin" && (len(host) != 2 || strings.Join(host[1].Argv, " ") != "go test -tags=realadaptercheck ./internal/sidecar -run=^TestRealAdapterLocal$ -count=1") {
			t.Fatalf("darwin test plan = %+v", host)
		}
		for _, s := range host {
			if strings.Contains(strings.Join(s.Argv, " "), "docker") || strings.Contains(strings.Join(s.Argv, " "), "containeracceptance") {
				t.Fatalf("the container operation is a Step of the test plan: %v", s.Argv)
			}
		}
		// FP-3 failure assertions live in this subtest with the FP-2 plan
		// assertions. Linux drives the public entry point; other hosts pass
		// without a skip (a skip would fail Darwin native qualification).
		if runtime.GOOS != "linux" {
			return
		}
		r := &ciRunner{failOn: "-p=1"}
		code, out, errOut := devcheckRun(t, r, "test")
		want := []string{
			"go test -count=1 -timeout=180s ./...",
			wantRace,
		}
		var got []string
		for _, c := range r.calls {
			got = append(got, strings.Join(c, " "))
		}
		if code != 1 || strings.Join(got, "|") != strings.Join(want, "|") ||
			!strings.Contains(out, "devcheck: test -race: "+want[1]) ||
			!strings.Contains(errOut, "injected child failure") {
			t.Fatalf("race failure = %d calls=%v\n%s\n%s", code, got, out, errOut)
		}
	})
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
	r := &ciRunner{failOn: "darwin-arm64"}
	code, _, errOut = devcheckRun(t, r, "cross")
	if code != 1 || !strings.Contains(errOut, "stage cross FAILED") || !strings.Contains(errOut, "darwin-arm64") {
		t.Fatalf("cross = %d %s", code, errOut)
	}
}

// wantNativeShared and wantNativeReale2e are Darwin's two native commands
// in order (design 12a-native-reale2e-budget, FP-1), literal and never
// derived from devcheck: every package but internal/reale2e under the
// 300-second binary cap, then internal/reale2e alone under 600 seconds.
const (
	wantNativeShared = "go test -json -tags=realadaptercheck -count=1 -timeout=300s ./cmd/... ./internal/adapter/... ./internal/cicheck/... ./internal/cli/... " +
		"./internal/client/... ./internal/contract/... ./internal/devcheck/... ./internal/logging/... ./internal/mcp/... ./internal/mcpqual/... " +
		"./internal/plane/... ./internal/sidecar/... ./internal/spikes/... ./internal/taskpublication/... ./internal/taskworkspace/... " +
		"./internal/testkit/... ./internal/workspace/... ./internal/workspacetransfer/... ./tests/..."
	wantNativeReale2e = "go test -json -tags=realadaptercheck -count=1 -timeout=600s ./internal/reale2e"
)

// FP-3: the macOS job runs native, which requires all three scenarios; its
// stress moved to ci-macos-stress (iteration 02b). Design
// 12a-native-reale2e-budget (FP-1): the native plan is two commands with
// disjoint tagged selections whose union is the module, internal/reale2e
// alone in the second under 600 s and never in the 300 s first.
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
	if err != nil || strings.Join(stages["macos"], " ") != "native" || strings.Join(cicheck.Jobs()[1].Stages, " ") != "native" {
		t.Fatalf("macos stages = %v %v", stages, err)
	}
	steps, err := devcheck.NativeSteps("darwin")
	if err != nil || len(steps) != 2 || steps[0].Name != "native" || strings.Join(steps[0].Argv, " ") != wantNativeShared || len(steps[0].Env) != 0 ||
		steps[1].Name != "native reale2e" || strings.Join(steps[1].Argv, " ") != wantNativeReale2e || len(steps[1].Env) != 0 {
		t.Fatalf("native plan = %+v %v", steps, err)
	}
	// The 300 s command never selects internal/reale2e by its own pattern
	// or an enclosing one; the resolved proof is the delegated contract.
	for _, a := range steps[0].Argv {
		if a == "./..." || a == "./internal/..." || strings.Contains(a, "reale2e") || a == "-race" || a == "-p=1" || a == "-timeout=600s" {
			t.Fatalf("the 300s native command selects or inherits %q: %v", a, steps[0].Argv)
		}
	}
	if devcheck.NativeReale2ePackage != reale2ePkg {
		t.Fatalf("NativeReale2ePackage = %q", devcheck.NativeReale2ePackage)
	}
	// The tagged module selection proof, delegated by exact name: every
	// package of go list -tags=realadaptercheck ./... once across the two
	// commands, internal/reale2e alone in the second, the sidecar in the
	// first only, planted duplicate, omission and wrong-300s plans caught.
	const devcheckPkg = "./internal/devcheck"
	contractRun(t, contractBinary(t, devcheckPkg), devcheckPkg, "^TestNativeSelectionCoversModule$", os.Environ(), "TestNativeSelectionCoversModule")
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
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
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
	bin := nodeBinary(t)
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
	// Qualification (design 12a-native-reale2e-budget: two commands, each
	// writing its own part of the stream), then the coverage stage
	// (profile, func report, cmd list), the workspace benchmark, (iteration
	// 09b) the transfer benchmark, (iteration 10b) the task workspace
	// benchmark, (non-blocking coordinator waits) the client wait benchmark
	// and (decoder enrollment) the qualification harness's benchmark, then
	// the development full-flow benchmark: eleven calls. An empty cover
	// total is parsed as the literal "(statements)" and fails the stage.
	first := fixture + sharedQualifyingEvents("", "")
	r := &ciRunner{stdout: first, reale2eStdout: reale2eEvents(), coverTotal: "91.7%"}
	code, out, errOut := devcheckRun(t, r, "native")
	if code != 0 || len(r.calls) != 11 ||
		strings.Join(r.calls[0], " ") != wantNativeShared || strings.Join(r.calls[1], " ") != wantNativeReale2e ||
		strings.Join(r.calls[5], " ") != strings.Join(devcheck.WorkspaceBenchStep().Argv, " ") ||
		strings.Join(r.calls[6], " ") != strings.Join(devcheck.TransferBenchStep().Argv, " ") ||
		strings.Join(r.calls[7], " ") != strings.Join(devcheck.TaskWorkspaceBenchStep().Argv, " ") ||
		strings.Join(r.calls[8], " ") != strings.Join(devcheck.ClientWaitBenchStep().Argv, " ") ||
		strings.Join(r.calls[9], " ") != strings.Join(devcheck.MCPQualBenchStep().Argv, " ") ||
		strings.Join(r.calls[10], " ") != strings.Join(devcheck.RealE2EDevBenchStep().Argv, " ") ||
		!strings.Contains(out, "native qualification passed on darwin/"+runtime.GOARCH) {
		t.Fatalf("darwin native = %d calls=%d %s", code, len(r.calls), errOut)
	}
	if code, _, errOut := devcheckRun(t, &ciRunner{failOn: "-json"}, "native"); code != 1 || !strings.Contains(errOut, "injected child failure") {
		t.Fatalf("child error = %d %s", code, errOut)
	}
	firstBad := map[string]string{
		"missing":   fixture,
		"skip":      fixture + sharedQualifyingEvents("", "resistant"),
		"malformed": first[:len(first)-7],
		"fail":      first + events(synth("fail", "example.com/late", "")),
	}
	for name, s := range firstBad {
		if code, _, _ := devcheckRun(t, &ciRunner{stdout: s, reale2eStdout: reale2eEvents()}, "native"); code != 1 {
			t.Fatalf("%s evidence passed the native stage", name)
		}
	}
	if code, _, _ := devcheckRun(t, &ciRunner{stdout: first}, "native"); code != 1 {
		t.Fatal("a stream without internal/reale2e passed the native stage")
	}
}

// FP-2 (design 12a-native-reale2e-budget): the exported checker qualifies
// the combined stream of the two native commands, a complete first stream
// followed by internal/reale2e's own, and rejects a missing, failed,
// skipped or truncated reale2e stream, a sidecar or reale2e package started
// by both commands, and missing function or sidecar evidence.
func TestCINativeSplitQualification(t *testing.T) {
	if devcheck.NativeReale2ePackage != "github.com/wedevwork/callsheet/internal/reale2e" || reale2ePkg != devcheck.NativeReale2ePackage {
		t.Fatalf("NativeReale2ePackage = %q", devcheck.NativeReale2ePackage)
	}
	r, sc := reale2ePkg, devcheck.NativeTaskProcessPackage
	first, second := sharedQualifyingEvents("", ""), reale2eEvents()
	checkStream := func(s string) error { return devcheck.CheckNativeResults("darwin", strings.NewReader(s)) }
	if err := checkStream(first + second); err != nil {
		t.Fatalf("disjoint complete streams: %v", err)
	}
	combined := first + second
	for name, c := range map[string]struct{ stream, want string }{
		"missing reale2e":           {first, "package " + r + " has no start event"},
		"only reale2e":              {second, "package " + devcheck.NativePackage + " has no start event"},
		"failed reale2e test":       {first + events(synth("start", r, ""), synth("run", r, "TestX"), synth("fail", r, "TestX")), "test TestX in " + r + " failed"},
		"failed reale2e package":    {first + events(synth("start", r, ""), synth("run", r, "TestX"), synth("pass", r, "TestX"), synth("fail", r, "")), "package " + r + " failed"},
		"skipped reale2e test":      {first + events(synth("start", r, ""), synth("run", r, "TestX"), synth("skip", r, "TestX")), "test TestX in " + r + " skipped"},
		"skipped reale2e package":   {first + events(synth("start", r, ""), synth("skip", r, "")), "package " + r + " has no pass event"},
		"unfinished reale2e":        {first + events(synth("start", r, ""), synth("run", r, "TestX")), "package " + r + " did not finish"},
		"truncated reale2e":         {combined[:len(combined)-7], "malformed event stream after"},
		"duplicate sidecar":         {first + second + events(synth("start", sc, ""), synth("pass", sc, "")), "package " + sc + " started twice"},
		"duplicate reale2e":         {first + second + second, "package " + r + " started twice"},
		"reale2e in the 300s child": {sharedQualifyingEvents("", "") + second + second, "package " + r + " started twice"},
		"missing function evidence": {sharedQualifyingEvents("resistant", "") + second, "TestFP6ProcessGroups/resistant has no run event"},
		"missing sidecar evidence":  {firstNativeStream("TestTaskExecutionContract/process") + second, "TestTaskExecutionContract/process in " + sc + " has no run event"},
	} {
		err := checkStream(c.stream)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want %q", name, err, c.want)
		}
		if strings.Contains(name, "missing") || strings.HasPrefix(name, "skipped") {
			if !strings.Contains(err.Error(), "native qualification unobserved") {
				t.Fatalf("%s: %v is not unobserved", name, err)
			}
		}
	}
	if err := devcheck.CheckNativeResults("linux", strings.NewReader(first+second)); err == nil || !strings.Contains(err.Error(), "darwin only") {
		t.Fatalf("linux checker: %v", err)
	}
}

// FP-3 (design 12a-native-reale2e-budget): docs/ci.md carries the split
// policy, the evidence-based provisional cap, the job-budget projection
// with its assumption and the hosted verification procedure, distinguishing
// pending measurements from passed tests; the workflow keeps its 30-minute
// macOS job and adds no step timeout.
func TestCINativeBudgetHandoff(t *testing.T) {
	checks := docSection(t, "Checks")
	const marker = "**Native reale2e budget (design 12a-native-reale2e-budget, 2026-10-24).**"
	start := strings.Index(checks, marker)
	if start < 0 || strings.Count(checks, marker) != 1 {
		t.Fatal("Checks has no single dated native reale2e budget explanation")
	}
	end := strings.Index(checks[start:], "\n\nIteration 11 (real adapters, wave 2")
	if end < 0 || !strings.Contains(strings.Join(strings.Fields(checks[:start]), " "), "That contract is in a `realadaptercheck`-tagged file") {
		t.Fatal("the budget explanation does not follow the tagged-sidecar paragraph")
	}
	budget := checks[start : start+end]
	requireTerms(t, "Checks budget", budget,
		// The two-step, disjoint policy and the caps.
		"two sequential tagged commands with disjoint package selections whose union is the module",
		"every package except `internal/reale2e` under the unchanged `-timeout=300s`", "`internal/reale2e` alone under `-timeout=600s`",
		"`TestNativeSelectionCoversModule`", "`go list -tags=realadaptercheck ./...`", "checked once after both succeed",
		"every package starts once across the two commands", "`github.com/wedevwork/callsheet/internal/reale2e`", "a package skip never satisfies",
		"A failure of either command stops the stage before qualification, coverage and the benchmarks",
		// The evidence and the provisional cap.
		"run [40116764068](https://github.com/wedevwork/callsheet/actions/runs/40116764068) at `803106b`",
		"run [40072740039](https://github.com/wedevwork/callsheet/actions/runs/40072740039) at `7575691`",
		`"Action":"fail", ... "Elapsed":300.187`, `"Action":"pass", ... "Elapsed":251.849`,
		"provisional policy cap, not a measurement", "`(749.4 + 132.4) / 3 = 293.93`", "`251.849 - 660/3 = 31.849`",
		"approximately **326 seconds before 107 unstarted new leaves**", "an estimated floor", "not an upper bound or a completed duration",
		"`600 - 326 = 274`", "about **84% headroom over that estimated floor**", "`600 - 251.849 = 348.151`",
		"“If a test binary runs longer than duration d, panic.”", "bounds the test binary, not its compilation or the whole command",
		// Job feasibility, coverage's implicit bound and the projection.
		"`timeout-minutes: 30`, 1800 seconds", "no step-level timeout",
		"its implicit per-binary bound is go test's default, **10m (600s)**", "“The default is 10 minutes (10m).”",
		"`ok  github.com/wedevwork/callsheet/internal/reale2e  209.197s`", "`600 - 209.197 = 390.803`",
		"36 s setup, 346 s old combined qualification", "266 s coverage, report and listing, 222 s benchmarks and 2 s cleanup, **872 s**",
		`"Elapsed":233.682`, "`300 - 233.682 = 66.318`", "`346 - 233.682 = 112.318`", "not spare time",
		"**120 s planning contingency**", "`872 + 600 + 120 = 1592` seconds", "**208 seconds below the 1800-second job limit**",
		"ASSUMPTION (unverified)", "shares that reserve with all other growth", "a conditional projection, not a worst-case bound",
		"does not fit the job and is not promised to", "is pending until recorded; pending is not passed")
	// The ci-macos row and the tagged-sidecar paragraph state the current
	// two-command policy, never the former single invocation.
	requireTerms(t, "Checks", checks,
		"`devcheck native`: the complete suite as two sequential `go test -json -tags=realadaptercheck` commands with disjoint package selections",
		"then `internal/reale2e` alone under `-timeout=600s`; either command failing stops the stage",
		"must show the package start and package pass of `github.com/wedevwork/callsheet/internal/reale2e`",
		"one package start per package across both commands",
		"the first of the two native commands (the one selecting `internal/sidecar`; design 12a-native-reale2e-budget)")
	flat := strings.Join(strings.Fields(checks), " ")
	for _, stale := range []string{"the complete suite as one `go test -json", "the native stage is one invocation", "the native command, the tagged `test`"} {
		if strings.Contains(flat, stale) {
			t.Fatalf("Checks keeps the single-invocation claim %q", stale)
		}
	}
	// The hosted procedure: what to record, where, and the pending rule.
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first,
		"for 12a-native-reale2e-budget (2026-10-24; owner-closed delivery verification, outside the implementation acceptance bar",
		"records in pull request #67 (or the implementing pull request)", "the run, job and attempt links and the SHA",
		"runner image and architecture, the Go version and the cache context",
		"(`Package` exactly `github.com/wedevwork/callsheet/internal/reale2e`, no `Test` field, `Action` `pass`) and its `Elapsed`",
		"record `600 - Elapsed` as the observed margin",
		"the coverage child's `ok  github.com/wedevwork/callsheet/internal/reale2e  Ns` line", "record `N` and `600 - N` against its implicit 10m (600 s) bound",
		"compare `N` with the 209.197 s main baseline", "Require positive margins for both executions", "a successful whole job within 1800 seconds",
		"command wall is not package `Elapsed`", "check them against the 1592-second projection",
		"A failed, cancelled, missing or skipped result never qualifies; preserve failed attempts",
		"mark hosted verification pending and perform the same check on the first post-merge run, recording its actual SHA",
		"never label another SHA as the pull request sample", "Pending hosted evidence does not block implementation review when its test bar is met",
		"until recorded it is pending, not passed", "never an automatic budget increase or a weakened test", "a successful job above 1592 seconds invalidates the projection",
		"no repeat-until-green sampling", "Hosted verification: pending.")
	// The workflow: the 30-minute macOS job, its four steps, no step
	// timeout, no budget raised.
	data := ciWorkflow(t)
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if node(t, &doc, "jobs", "macos", "timeout-minutes").Value != "30" || cicheck.Jobs()[1].ID != "macos" || cicheck.Jobs()[1].TimeoutMinutes != 30 {
		t.Fatal("the ci-macos job is no longer 30 minutes")
	}
	steps := node(t, &doc, "jobs", "macos", "steps")
	if len(steps.Content) != 4 || node(t, steps, 3, "run").Value != "go run ./cmd/devcheck native" {
		t.Fatalf("ci-macos steps changed: %d", len(steps.Content))
	}
	for i, st := range steps.Content {
		for j := 0; j+1 < len(st.Content); j += 2 {
			if st.Content[j].Value == "timeout-minutes" {
				t.Fatalf("ci-macos step %d has a step timeout", i)
			}
		}
	}
}

// FP-4 (design 12a-native-reale2e-budget): the two native children run
// sequentially, either one's failure stops the stage, and the coverage and
// benchmark tail runs only after both children and the combined
// qualification succeed, with the unchanged success line, in eleven runner
// calls. Proved on either host by delegating devcheck's injected-driver
// contract by exact name (no real native suite runs).
func TestCINativeSplitExecution(t *testing.T) {
	const pkg = "./internal/devcheck"
	contractRun(t, contractBinary(t, pkg), pkg, "^TestNativeSplitDriverContract$", os.Environ(),
		"TestNativeSplitDriverContract", "TestNativeSplitDriverContract/first-child-failure", "TestNativeSplitDriverContract/second-child-failure",
		"TestNativeSplitDriverContract/malformed-second-stream", "TestNativeSplitDriverContract/tail-suppression", "TestNativeSplitDriverContract/success")
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
	// FP-12: the evidence step cannot drift: relocated before cross,
	// duplicated, or copied into the macOS job or a stress worker.
	mustReject(t, "evidence relocated", mutated(t, func(r *yaml.Node) {
		s := node(t, r, "jobs", "linux", "steps")
		s.Content[6], s.Content[7] = s.Content[7], s.Content[6]
	}), `jobs.linux.steps[6].run: must be "go run ./cmd/devcheck cross"`)
	mustReject(t, "evidence duplicated", mutated(t, func(r *yaml.Node) {
		s := node(t, r, "jobs", "linux", "steps")
		s.Content = append(s.Content, s.Content[7])
	}), "jobs.linux.steps[8]: unexpected extra step")
	for _, id := range []string{"macos", "linux-stress-functions", "macos-stress-packages"} {
		mustReject(t, "evidence in "+id, mutated(t, func(r *yaml.Node) {
			ev := node(t, r, "jobs", "linux", "steps", 7)
			s := node(t, r, "jobs", id, "steps")
			s.Content = append(s.Content, ev)
		}), "jobs."+id+".steps[")
	}
	// ExtractStages still yields the four Linux check stages: cat is not a
	// devcheck command.
	if got := strings.Join(stages["linux"], " "); got != "test coverage bench cross" {
		t.Fatalf("linux stages = %q", got)
	}
}

// ownerAddContexts is the only repository mutation docs/ci.md may contain:
// the owner-only additive request adding the two stress contexts
// (iteration 02b). Nothing executes it.
const ownerAddContexts = "gh api --method POST \\\n" +
	"  -H 'Accept: application/vnd.github+json' \\\n" +
	"  repos/wedevwork/callsheet/branches/main/protection/required_status_checks/contexts \\\n" +
	"  -f 'contexts[]=ci-linux-stress' \\\n" +
	"  -f 'contexts[]=ci-macos-stress'\n"

// FP-7: the owner-applied branch protection handoff.
func TestCIProtectionHandoff(t *testing.T) {
	// Checks names the four required contexts first, then only the
	// eighteen stress workers (iteration 02c, plane since 05b, sidecar since
	// its sidecar follow-up, the plane and sidecar CPU1 workers since
	// 06a-perf, the packages-cpu workers since the stress worker
	// rebalance, and the sidecar CPU4 workers since iteration
	// 12a-stress-sidecar-margin): no other check name.
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
	var all []string
	for _, j := range cicheck.Jobs() {
		all = append(all, j.Name)
	}
	sortedListed, sortedAll := slices.Clone(listed), slices.Clone(all)
	slices.Sort(sortedListed)
	slices.Sort(sortedAll)
	if len(listed) < 4 || strings.Join(listed[:4], ",") != strings.Join(cicheck.RequiredChecks(), ",") || !slices.Equal(sortedListed, sortedAll) || len(all) != 22 {
		t.Fatalf("Checks lists %v, want the required %v first and then only the other jobs of %v", listed, cicheck.RequiredChecks(), all)
	}
	bp := docSection(t, "Branch protection")
	requireTerms(t, "Branch protection", bp,
		"wedevwork/callsheet", "branch name pattern `main`",
		"Require a pull request before merging",
		"Require status checks to pass before merging: `ci-linux`, `ci-macos`, `ci-linux-stress`, `ci-macos-stress`",
		"Require branches to be up to date before merging",
		"Do not allow bypassing the above settings", "include administrators",
		"no force pushes", "no branch deletion",
		"gh api repos/wedevwork/callsheet/branches/main/protection",
		"required_status_checks.strict", "enforce_admins", "rulesets")
	// The documented owner handoff is the only mutating request: exactly one
	// POST, inside the exact additive command in Branch protection, and no
	// other method that could replace or remove protection.
	doc := string(repoFile(t, "docs/ci.md"))
	if strings.Count(doc, "--method POST") != 1 || strings.Count(doc, ownerAddContexts) != 1 || !strings.Contains(bp, "\n"+ownerAddContexts) {
		t.Fatalf("docs/ci.md must contain exactly one POST, the owner handoff in Branch protection:\n%s", ownerAddContexts)
	}
	for _, forbidden := range []string{"ci-windows", "-X PUT", "--method PUT", "-X POST", "-X PATCH", "--method PATCH", "-X DELETE", "--method DELETE", "curl "} {
		if strings.Contains(doc, forbidden) {
			t.Fatalf("docs/ci.md contains %q", forbidden)
		}
	}
	wf := string(ciWorkflow(t))
	for _, forbidden := range []string{"gh ", "protection", "api.github.com", "secrets.", "administration", "--method", "POST", "curl"} {
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
		"iterations 02, 02b and 02c join pull request #2", "Adding the stress contexts",
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
	green := stepIndex(t, steps, "all four checks", "`ci-linux-stress`", "`ci-macos-stress`", "succeed on the current PR merge revision")
	merge := stepIndex(t, steps, "Merge only after all four checks are green")
	if !(review < commit && commit < open && open < green && green < merge) {
		t.Fatalf("procedure order review=%d commit=%d open=%d green=%d merge=%d", review, commit, open, green, merge)
	}
	requireTerms(t, "green step", steps[green], "skipped, canceled, pending or unobserved check is not acceptable")
	first := docSection(t, "First remote run")
	requireTerms(t, "First remote run", first, "pending until observed", "run URL", "commit",
		"conclusions of all twenty-two jobs: all four checks, `ci-linux`, `ci-macos`, `ci-linux-stress` and `ci-macos-stress`, and the eighteen stress workers",
		"native evidence", "native qualification passed on darwin",
		"branch protection verification",
		"git ls-remote https://github.com/actions/checkout.git 'refs/tags/v6.0.2' 'refs/tags/v6.0.2^{}'",
		"git ls-remote https://github.com/actions/setup-go.git 'refs/tags/v6.3.0' 'refs/tags/v6.3.0^{}'",
		"peeled commit", "handoff blocker")
	requireTerms(t, "First remote run", first, "stress evidence from the eighteen worker logs", "`devcheck: stage stress-packages ok`",
		"`devcheck: stage stress-packages-cpu ok`", "`devcheck: stage stress-plane-cpu1 ok`", "`devcheck: stage stress-plane ok`", "`devcheck: stage stress-sidecar-cpu1 ok`",
		"`devcheck: stage stress-sidecar ok`", "`devcheck: stage stress-sidecar-cpu4 ok`", "`devcheck: stage stress-functions ok`", "elapsed time of each stress command",
		"the actual job and step times of all twenty-two jobs")
	for _, stale := range []string{"conclusions of all fourteen jobs", "stress evidence from the ten worker logs", "the actual job and step times of all fourteen jobs",
		"conclusions of all eighteen jobs", "stress evidence from the fourteen worker logs", "the actual job and step times of all eighteen jobs"} {
		if strings.Contains(strings.Join(strings.Fields(first), " "), stale) {
			t.Fatalf("First remote run keeps the obsolete %q", stale)
		}
	}
	local := docSection(t, "Local verification")
	requireTerms(t, "Local verification", local, "go run ./cmd/devcheck all", "go run ./cmd/devcheck stress", "actionlint", "not a required dependency")
}
