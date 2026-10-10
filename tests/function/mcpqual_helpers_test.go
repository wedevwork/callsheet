package function

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Iteration 07b function-test helpers: the real developer executable and
// the fake vendor CLI (each built once per process), fake vendor plans that
// name the fake by absolute path, an isolated HOME and a PATH holding only launch-recording
// traps named like the vendor CLIs, and independent verification that every
// launched process group is gone.

// qualBinary is the shared cmd/mcpqual binary.
func qualBinary(t *testing.T) string {
	return fixture(t, "mcpqual", func(dir string) (string, error) { return testkit.BuildBinaryAt(dir, "./cmd/mcpqual", "mcpqual") })
}

// fakeVendorBinary is the shared fake vendor CLI (testdata/fakevendor),
// built without instrumentation so each launch starts quickly even when
// this package runs under the race detector.
func fakeVendorBinary(t *testing.T) string {
	return fixture(t, "fakevendor", func(dir string) (string, error) {
		return testkit.BuildBinaryAt(dir, "./tests/function/testdata/fakevendor", "fakevendor")
	})
}

var fakeVersions = map[string]string{"claude": "2.1.282 (Claude Code)", "codex": "codex-cli 0.156.1", "grok": "grok 1.0.41 (4220f3b224a6) [stable]", "cursor": "2026.09.23-86fc751"}

var fakeDecoders = map[string]string{"claude": "claude-json", "codex": "codex-jsonl", "grok": "grok-json", "cursor": "cursor-jsonl"}

const probeConfig = `{"mcpServers":{"probe":{"command":"{server}","args":["serve","--case-file","{case_file}","--events","{events}"]}}}`

// qualEnv is one test's isolated environment.
type qualEnv struct {
	t        *testing.T
	dir      string
	home     string
	trapDir  string
	trapLog  string
	launches string
}

func newQualEnv(t *testing.T) *qualEnv {
	t.Helper()
	dir := t.TempDir()
	q := &qualEnv{t: t, dir: dir, home: mkdir(t, filepath.Join(dir, "home")), trapDir: mkdir(t, filepath.Join(dir, "trap")),
		trapLog: filepath.Join(dir, "trap.log"), launches: filepath.Join(dir, "launches.log")}
	for _, name := range []string{"claude", "codex", "grok", "cursor-agent", "agent"} {
		script := "#!/bin/sh\necho \"$0 $*\" >> '" + q.trapLog + "'\nexit 97\n"
		if err := writeExecutable(filepath.Join(q.trapDir, name), []byte(script)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(q.noInstalledVendor)
	return q
}

// noInstalledVendor asserts that no vendor CLI was looked up on PATH.
func (q *qualEnv) noInstalledVendor() {
	if b, err := os.ReadFile(q.trapLog); err == nil {
		q.t.Errorf("an installed vendor executable was launched: %s", b)
	}
}

// fakeClient is a plan client for the fake vendor id with fake settings
// (FAKE_VENDOR_*) and phases.
func (q *qualEnv) fakeClient(id string, settings map[string]string, phases map[string]any) map[string]any {
	env := map[string]string{"FAKE_VENDOR_FORMAT": fakeDecoders[id], "FAKE_VENDOR_VERSION": fakeVersions[id],
		"FAKE_VENDOR_CLIENT_NAME": id + "-cli", "FAKE_VENDOR_CLIENT_VERSION": "1.0.0", "FAKE_VENDOR_TIMEOUT_MS": "100", "FAKE_VENDOR_TOKEN": "1",
		"FAKE_VENDOR_LOG": q.launches}
	for k, v := range settings {
		env["FAKE_VENDOR_"+k] = v
	}
	version := fakeVersions[id]
	if v, ok := settings["VERSION"]; ok {
		version = v
	}
	if phases == nil {
		phases = map[string]any{}
	}
	return map[string]any{"id": id, "executable": fakeVendorBinary(q.t), "expected_version": version, "version_argv": []string{"--version"},
		"driver": "direct", "decoder": fakeDecoders[id], "decoder_fixture": fakeDecoders[id] + "/synthetic",
		"session": map[string]any{"argv": []string{"--config", "{config}", "--case", "{case}", "-p", "{prompt}"}},
		"config": map[string]any{"default": map[string]any{"path": "probe-mcp.json", "content": probeConfig},
			"raised": map[string]any{"path": "probe-mcp.json", "content": strings.Replace(probeConfig, `{"mcpServers"`, `{"timeout_ms":400,"mcpServers"`, 1)}},
		"override": map[string]any{"setting": "timeout_ms", "value": "400", "explanation": "the fake vendor's per-call timeout key",
			"evidence": []string{"tests/testdata/cli-help/claude-help.txt"}},
		"env": env, "phases": phases}
}

func qualPlan(clients ...map[string]any) map[string]any {
	return map[string]any{"version": 1, "limits": map[string]any{"max_sessions_per_client": 12, "max_case_ms": 10000, "max_client_ms": 60000}, "clients": clients}
}

func (q *qualEnv) writePlan(plan any) string {
	q.t.Helper()
	b, err := json.Marshal(plan)
	if err != nil {
		q.t.Fatal(err)
	}
	p := filepath.Join(q.dir, "plan-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		q.t.Fatal(err)
	}
	return p
}

// env is the launched mcpqual's environment: isolated HOME, the trap PATH
// and no CI variable at all (design decoder-enrollment: any CI presence,
// even an empty CI=, refuses qualify and capture). ci-denied passes a
// nonempty value; an explicit empty presence is extra "CI=".
func (q *qualEnv) env(ci string, extra ...string) []string {
	env := []string{"PATH=" + q.trapDir, "HOME=" + q.home}
	if ci != "" {
		env = append(env, "CI="+ci)
	}
	return append(env, extra...)
}

// qualify runs "mcpqual qualify" to completion.
func (q *qualEnv) qualify(plan any, out string, ci string, extraEnv []string, args ...string) result {
	q.t.Helper()
	argv := append([]string{"qualify", "--plan", q.writePlan(plan), "--out", out}, args...)
	return runBin(q.t, qualBinary(q.t), q.dir, q.env(ci, extraEnv...), argv...)
}

// start starts "mcpqual qualify" without waiting.
func (q *qualEnv) start(plan any, out string, args ...string) (*exec.Cmd, *safeBuffer, *safeBuffer) {
	q.t.Helper()
	cmd := exec.Command(qualBinary(q.t), append([]string{"qualify", "--plan", q.writePlan(plan), "--out", out}, args...)...)
	cmd.Env, cmd.Dir = q.env(""), q.dir
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		q.t.Fatal(err)
	}
	q.t.Cleanup(func() { cmd.Process.Kill() })
	return cmd, stdout, stderr
}

// launchLog parses the fake vendor's launch log: kind (version, session,
// descendant) to PIDs, in order.
func (q *qualEnv) launchLog() map[string][]int {
	out := map[string][]int{}
	b, _ := os.ReadFile(q.launches)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			pid, _ := strconv.Atoi(f[1])
			out[f[0]] = append(out[f[0]], pid)
		}
	}
	return out
}

// groupsGone independently proves (with an unfaulted signaler) that every
// fake vendor group, and every recorded descendant, no longer exists.
func (q *qualEnv) groupsGone() {
	q.t.Helper()
	log := q.launchLog()
	var targets []int
	for _, pid := range append(append(append(append([]int(nil), log["version"]...), log["help"]...), log["session"]...), log["enable"]...) {
		targets = append(targets, -pid)
	}
	targets = append(targets, log["descendant"]...)
	if len(targets) == 0 {
		return
	}
	if err := processgroup.WaitGone(processgroup.SysSignaler{}, processgroup.RealClock{}, 5*time.Second, 10*time.Millisecond, targets...); err != nil {
		q.t.Fatalf("a launched process survived: %v", err)
	}
}

// readyFIFO makes a FIFO the fake vendor writes once its signal
// dispositions are set; wait blocks (bounded) for that line.
func (q *qualEnv) readyFIFO() (string, func() string) {
	q.t.Helper()
	p := filepath.Join(q.dir, "ready.fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		q.t.Fatal(err)
	}
	ch := make(chan string, 1)
	go func() {
		f, err := os.OpenFile(p, os.O_RDONLY, 0)
		if err != nil {
			ch <- ""
			return
		}
		line, _ := bufio.NewReader(f).ReadString('\n')
		f.Close()
		ch <- line
	}()
	q.t.Cleanup(func() {
		// Unblock the reader if the fake never came.
		if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	return p, func() string {
		q.t.Helper()
		select {
		case l := <-ch:
			return l
		case <-time.After(mcpWait):
			q.t.Fatal("the fake vendor never became ready")
			return ""
		}
	}
}

func readReport(t *testing.T, out string) *mcpqual.Report {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := mcpqual.ParseReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.CheckEvidence(out); err != nil {
		t.Fatal(err)
	}
	return rep
}

func reportClient(t *testing.T, rep *mcpqual.Report, id string) mcpqual.ClientReport {
	t.Helper()
	for _, c := range rep.Clients {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no client %s", id)
	return mcpqual.ClientReport{}
}

func reportPhase(t *testing.T, c mcpqual.ClientReport, name string) mcpqual.PhaseReport {
	t.Helper()
	for _, ph := range c.Phases {
		if ph.Name == name {
			return ph
		}
	}
	t.Fatalf("%s has no %s phase", c.ID, name)
	return mcpqual.PhaseReport{}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func ms(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// qualRepo copies the catalog, its Markdown, the setup guide and the
// captured CLI evidence into a scratch repository.
func qualRepo(t *testing.T) string {
	t.Helper()
	root := testkit.MustRepoRoot(t)
	repo := t.TempDir()
	copyFile := func(rel string) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(repo, rel)
		mkdir(t, filepath.Dir(dst))
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// docs/real-adapters.md (iteration 08) is linked from the catalog's
	// Markdown, whose links the publication tests resolve.
	for _, rel := range []string{mcpqual.CatalogJSONPath, mcpqual.CatalogMDPath, mcpqual.SetupDocPath, "docs/real-adapters.md"} {
		copyFile(rel)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "tests", "testdata", "cli-help"))
	for _, e := range entries {
		copyFile(filepath.Join("tests", "testdata", "cli-help", e.Name()))
	}
	// The iteration 08 worker evidence is linked read-only; the fake
	// vendors report their fixture versions, which the scratch catalog
	// records as each entry's coordinator (MCP publication) version:
	// publication compares a run's observed version with it (design
	// catalog-version). The worker version stays the checked-in worker
	// qualification identity, which the fakes never report.
	if err := os.Symlink(filepath.Join(root, "tests", "testdata", "real-adapters"), filepath.Join(repo, "tests", "testdata", "real-adapters")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(repo, mcpqual.CatalogJSONPath)
	es, err := catalog.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// The four publisher-managed facts start from the frozen
	// pre-publication baseline (design catalog-version, amendment A1), so
	// no scenario depends on whether the checkout has received published
	// evidence.
	baseline := catalog.PublicationBaselineFacts()
	for i := range es {
		for key, f := range baseline[es[i].ID] {
			es[i].Facts[key] = f
		}
		es[i].CoordinatorVersion = fakeVersions[es[i].ID]
	}
	b, err := mcpqual.RenderCatalog(es)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	// So do the Markdown's three timeout bullets per vendor.
	mp := filepath.Join(repo, mcpqual.CatalogMDPath)
	md, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := catalog.WithPublicationBaselineBullets(string(md))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp, []byte(seeded), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func repoFacts(t *testing.T, repo string) map[string]catalog.Entry {
	t.Helper()
	es, err := catalog.Load(filepath.Join(repo, mcpqual.CatalogJSONPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(repo, es); err != nil {
		t.Fatal(err)
	}
	out := map[string]catalog.Entry{}
	for _, e := range es {
		out[e.ID] = e
	}
	return out
}

// probeProc is a real "mcpqual serve" over pipes.
type probeProc struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin interface {
		Write([]byte) (int, error)
		Close() error
	}
	lines  chan string
	events string
}

func startProbeProc(t *testing.T, cf mcpqual.CaseFile) *probeProc {
	t.Helper()
	dir := t.TempDir()
	caseFile, events := filepath.Join(dir, "case.json"), filepath.Join(dir, "events.jsonl")
	b, _ := json.Marshal(cf)
	os.WriteFile(caseFile, b, 0o600)
	cmd := exec.Command(qualBinary(t), "serve", "--case-file", caseFile, "--events", events)
	cmd.Env = []string{"PATH=" + t.TempDir(), "HOME=" + dir}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &probeProc{t: t, cmd: cmd, stdin: in, lines: make(chan string, 256), events: events}
	go func() {
		br := bufio.NewReader(out)
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				p.lines <- l
			}
			if err != nil {
				close(p.lines)
				return
			}
		}
	}()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return p
}

func (p *probeProc) send(s string) {
	p.t.Helper()
	if _, err := p.stdin.Write([]byte(s + "\n")); err != nil {
		p.t.Fatal(err)
	}
}

// next returns the next output line (bounded wait).
func (p *probeProc) next() string {
	p.t.Helper()
	select {
	case l, ok := <-p.lines:
		if !ok {
			p.t.Fatal("probe stdout ended")
		}
		return l
	case <-time.After(mcpWait):
		p.t.Fatal("no probe output")
		return ""
	}
}

// finish closes stdin, requires exit 0 and returns the remaining output
// and the parsed events.
func (p *probeProc) finish() ([]string, []mcpqual.ProbeEvent) {
	p.t.Helper()
	p.stdin.Close()
	var rest []string
	for l := range p.lines {
		rest = append(rest, l)
	}
	if err := p.cmd.Wait(); err != nil {
		p.t.Fatalf("probe exit: %v", err)
	}
	b, _ := os.ReadFile(p.events)
	evs, intact, err := mcpqual.ParseProbeEvents(b)
	if err != nil || !intact {
		p.t.Fatalf("events intact=%v: %v", intact, err)
	}
	return rest, evs
}

func (p *probeProc) ready() {
	p.t.Helper()
	p.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe-test","version":"1"}}}`)
	if l := p.next(); !strings.Contains(l, `"serverInfo":{"name":"mcpqual"`) {
		p.t.Fatalf("initialize: %s", l)
	}
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}
