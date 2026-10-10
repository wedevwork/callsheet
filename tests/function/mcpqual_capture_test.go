package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/mcpqual/procexec"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Decoder enrollment, slice A (design decoder-enrollment): one function
// parent per FP (FP-1..FP-9). Each parent asserts its literal case
// inventory (count and labels) before running the cases in order, and
// that every case completed. Capture runs the real mcpqual executable
// with the fake vendor (testdata/fakevendor, by absolute path, with an
// isolated HOME and a PATH of launch-recording traps) and the real probe;
// no test runs an installed vendor CLI or a model. Every temporary
// directory whose path the evidence must not contain is resolved with
// filepath.EvalSymlinks (macOS /var is /private/var). The enrollment and
// CI-policy parents run the production validators over temporary copies.

// captureCase is one labeled case of a parent's literal inventory.
type captureCase struct {
	label string
	run   func(t *testing.T)
}

// runInventory requires exactly the literal labels, in order, before any
// case runs, then runs each case and requires every one to complete.
func runInventory(t *testing.T, count int, labels []string, cases []captureCase) {
	t.Helper()
	var got []string
	for _, c := range cases {
		got = append(got, c.label)
	}
	if len(cases) != count || !slices.Equal(got, labels) {
		t.Fatalf("case inventory %q, want %d cases %q", got, count, labels)
	}
	completed := 0
	for _, c := range cases {
		c.run(t)
		if t.Failed() {
			t.Fatalf("case %s failed", c.label)
		}
		completed++
	}
	if completed != count {
		t.Fatalf("%d of %d cases completed", completed, count)
	}
}

// captureVersions are the coordinator inventory's exact identities, none
// of which the production registry knows.
var captureVersions = map[string]string{"claude": "2.1.291 (Claude Code)", "codex": "codex-cli 0.160.0", "grok": "grok 1.0.46 (2765805b9442) [stable]", "cursor": "2026.10.01-e373342"}

// realTemp is a new temporary directory under its symlink-free path.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// capturePlan fills the shipped short template of each client with the
// fake vendor (FAKE_VENDOR_* settings per client) and returns the plan.
func (q *qualEnv) capturePlan(settings map[string]map[string]string, ids ...string) map[string]any {
	q.t.Helper()
	root := testkit.MustRepoRoot(q.t)
	var clients []any
	var limits any
	for _, id := range ids {
		b, err := os.ReadFile(filepath.Join(root, "internal", "mcpqual", "testdata", "plans", id+"-short.json"))
		if err != nil {
			q.t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(b, &p); err != nil {
			q.t.Fatal(err)
		}
		limits = p["limits"]
		c := p["clients"].([]any)[0].(map[string]any)
		c["executable"], c["expected_version"], c["model"] = fakeVendorBinary(q.t), captureVersions[id], "fake-model-1"
		argv := c["session"].(map[string]any)["argv"].([]any)
		for i, a := range argv {
			if a == "<model>" {
				argv[i] = "fake-model-1"
			}
		}
		env := map[string]string{"FAKE_VENDOR_FORMAT": fakeDecoders[id], "FAKE_VENDOR_VERSION": captureVersions[id], "FAKE_VENDOR_CLIENT_NAME": id + "-cli",
			"FAKE_VENDOR_CLIENT_VERSION": "1.0.0", "FAKE_VENDOR_LOG": q.launches, "FAKE_VENDOR_ARGV_LOG": filepath.Join(q.dir, "argv.log")}
		for k, v := range settings[id] {
			env["FAKE_VENDOR_"+k] = v
		}
		c["env"] = env
		clients = append(clients, c)
	}
	return map[string]any{"version": 1, "limits": limits, "clients": clients}
}

// capture runs "mcpqual capture" to completion.
func (q *qualEnv) capture(plan any, out string, extraEnv []string, args ...string) result {
	q.t.Helper()
	argv := append([]string{"capture", "--plan", q.writePlan(plan), "--out", out}, args...)
	return runBin(q.t, qualBinary(q.t), q.dir, q.env("", extraEnv...), argv...)
}

// startCapture starts "mcpqual capture --allow-model-calls" without
// waiting.
func (q *qualEnv) startCapture(plan any, out string) (*exec.Cmd, *safeBuffer) {
	q.t.Helper()
	cmd := exec.Command(qualBinary(q.t), "capture", "--plan", q.writePlan(plan), "--out", out, "--allow-model-calls")
	cmd.Env, cmd.Dir = q.env(""), q.dir
	stdout := &safeBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stdout
	if err := cmd.Start(); err != nil {
		q.t.Fatal(err)
	}
	q.t.Cleanup(func() { cmd.Process.Kill() })
	return cmd, stdout
}

// argvLog is the fake vendor's session records: argv, cwd and config.
type argvRecord struct {
	Argv       []string `json:"argv"`
	Cwd        string   `json:"cwd"`
	Config     string   `json:"config"`
	ConfigPath string   `json:"config_path"`
	CaseFile   string   `json:"case_file"`
	// Permission is what the session saw at .cursor/cli.json (design
	// decoder-enrollment B2): "<absent>" or "<mode> <content>".
	Permission string `json:"permission"`
}

// setupOnly is the structured setup-only check (code review B1.5 round 3,
// C1; never a byte search, since a timestamp such as offset_ns 15000 is a
// valid record): the case file the session's probe served holds exactly
// the zero-delay setup case of caseID and its marker, with no progress,
// and the parsed server events hold exactly one receipt and one
// completion, both of caseID, and no record of any other case.
func setupOnly(caseFile string, server []byte, caseID string) error {
	cf, err := mcpqual.ParseCaseFile([]byte(caseFile))
	if err != nil {
		return fmt.Errorf("case file: %v", err)
	}
	if len(cf.Cases) != 2 || cf.Cases[0].CaseID != caseID || cf.Cases[1].CaseID != caseID+"-marker" {
		return fmt.Errorf("case file cases %+v, want only %s and its marker", cf.Cases, caseID)
	}
	for _, c := range cf.Cases {
		if c.DelayMS != 0 || c.ProgressIntervalMS != 0 {
			return fmt.Errorf("case %s has delay %d ms and progress %d ms, want a zero-delay setup", c.CaseID, c.DelayMS, c.ProgressIntervalMS)
		}
	}
	evs, _, err := mcpqual.ParseProbeEvents(server)
	if err != nil {
		return fmt.Errorf("server events: %v", err)
	}
	receipts, completions := 0, 0
	for _, ev := range evs {
		switch {
		case ev.CaseID != "" && ev.CaseID != caseID:
			return fmt.Errorf("a %s record of case %s", ev.Kind, ev.CaseID)
		case ev.Kind == mcpqual.EvReceipt:
			receipts++
		case ev.Kind == mcpqual.EvCompleted:
			completions++
		}
	}
	if receipts != 1 || completions != 1 {
		return fmt.Errorf("%d receipts and %d completions, want one setup call", receipts, completions)
	}
	return nil
}

func (q *qualEnv) argvLog() []argvRecord {
	q.t.Helper()
	b, _ := os.ReadFile(filepath.Join(q.dir, "argv.log"))
	var out []argvRecord
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
		if len(l) == 0 {
			continue
		}
		var r argvRecord
		if err := json.Unmarshal(l, &r); err != nil {
			q.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// bundle validates the capture bundle in out.
func bundle(t *testing.T, out string, extra ...string) *mcpqual.CaptureBundle {
	t.Helper()
	b, err := mcpqual.ValidateCaptureBundle(os.DirFS(out), ".", extra...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func captureClient(t *testing.T, m *mcpqual.CaptureManifest, id string) mcpqual.CaptureClient {
	t.Helper()
	for _, c := range m.Clients {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no client %s", id)
	return mcpqual.CaptureClient{}
}

// FP-1: decoder-free capture of an unknown exact version with an explicit
// opt-in; any CI presence and a missing opt-in launch nothing; qualify
// still refuses the unknown version.
func TestMCPCaptureInvocation(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, extra []string, args []string, want string) {
		t.Helper()
		q := newQualEnv(t)
		out := filepath.Join(realTemp(t), "out")
		r := q.capture(q.capturePlan(nil, "claude"), out, extra, args...)
		if r.code != 2 || !strings.Contains(r.stderr, want) {
			t.Fatalf("refusal = %+v", r)
		}
		if _, err := os.Stat(q.launches); err == nil {
			t.Fatal("a process launched")
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("an output directory was created")
		}
	}
	runInventory(t, 5, []string{"unknown-version-capture", "ci-set", "ci-empty", "opt-in-denied", "qualify-unknown-version"}, []captureCase{
		{"unknown-version-capture", func(t *testing.T) {
			q := newQualEnv(t)
			out := filepath.Join(realTemp(t), "out")
			r := q.capture(q.capturePlan(nil, "claude"), out, nil, "--allow-model-calls")
			if r.code != 0 || !strings.Contains(r.stdout, "capture complete; vendor behavior not evaluated") || !strings.Contains(r.stderr, "setup only; default phase not executed") {
				t.Fatalf("capture = %+v", r)
			}
			m := bundle(t, out).Manifest
			c := captureClient(t, m, "claude")
			log := q.launchLog()
			if m.State != mcpqual.CaptureComplete || m.VendorBehavior != mcpqual.VendorNotEvaluated || *c.ObservedVersion != captureVersions["claude"] || c.DecoderValidated ||
				len(log["version"]) != 1 || len(log["help"]) != 1 || len(log["session"]) != 1 {
				t.Fatalf("manifest %+v launches %v", c, log)
			}
			if _, err := os.Stat(filepath.Join(out, "report.json")); err == nil {
				t.Fatal("capture wrote a qualification report")
			}
			q.groupsGone()
		}},
		{"ci-set", func(t *testing.T) {
			refused(t, []string{"CI=true"}, []string{"--allow-model-calls"}, "capture refused: CI is set")
		}},
		{"ci-empty", func(t *testing.T) {
			refused(t, []string{"CI="}, []string{"--allow-model-calls"}, "capture refused: CI is set")
		}},
		{"opt-in-denied", func(t *testing.T) { refused(t, nil, nil, "capture needs --allow-model-calls") }},
		{"qualify-unknown-version", func(t *testing.T) {
			q := newQualEnv(t)
			out := filepath.Join(realTemp(t), "out")
			r := q.qualify(q.capturePlan(nil, "claude"), out, "", nil, "--allow-model-calls")
			c := reportClient(t, readReport(t, out), "claude")
			if r.code != 5 || !strings.HasPrefix(deref(c.Reason), mcpqual.ReasonDecoderVersion) || len(q.launchLog()["session"]) != 0 {
				t.Fatalf("qualify of an unknown version = %+v %s", r, deref(c.Reason))
			}
			// qualify's CI refusal is the same presence rule.
			if r := q.qualify(q.capturePlan(nil, "claude"), filepath.Join(q.dir, "out-ci"), "", []string{"CI="}, "--allow-model-calls"); r.code != 2 ||
				!strings.Contains(r.stderr, "qualify refused: CI is set") {
				t.Fatalf("qualify with CI= = %+v", r)
			}
			q.groupsGone()
		}},
	})
}

// FP-2: the four exact recipes from the shipped templates: version, help,
// one zero-delay setup session with the exact argv and configuration, and
// no 15 s session.
func TestMCPCaptureRecipes(t *testing.T) {
	t.Parallel()
	const m = "fake-model-1"
	type recipe struct {
		help    []string
		argv    func(ws, srv string) []string
		cfgRel  string
		cfgBody func(ws, srv string) string
	}
	jsonCfg := func(ws, srv string) string {
		return `{"mcpServers":{"probe":{"command":"` + srv + `","args":["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]}}}`
	}
	tomlCfg := func(ws, srv string) string {
		return "[mcp_servers.probe]\ncommand = \"" + srv + "\"\nargs = [\"serve\", \"--case-file\", \"" + ws + "/case.json\", \"--events\", \"" + ws + "/server-events.jsonl\"]\n"
	}
	recipes := map[string]recipe{
		"claude": {[]string{"--help"}, func(ws, srv string) []string {
			return []string{"-p", mcpqual.Prompt("claude-capture-setup"), "--output-format", "json", "--verbose", "--model", m, "--mcp-config", ws + "/probe-mcp.json",
				"--strict-mcp-config", "--allowed-tools", "mcp__probe__slow"}
		}, "probe-mcp.json", jsonCfg},
		// Design decoder-enrollment B1: Codex's invocation-only probe.slow
		// approval, Grok's trusted project recipe and Cursor's trusted
		// workspace.
		"codex": {[]string{"exec", "--help"}, func(ws, srv string) []string {
			return []string{"exec", "--json", "--skip-git-repo-check", "-C", ws, "-m", m, "-c", `mcp_servers.probe.command="` + srv + `"`, "-c",
				`mcp_servers.probe.args=["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]`,
				"-c", `mcp_servers.probe.enabled_tools=["slow"]`, "-c", `mcp_servers.probe.tools.slow.approval_mode="approve"`, mcpqual.Prompt("codex-capture-setup")}
		}, "probe-config.toml", func(ws, srv string) string {
			return tomlCfg(ws, srv) + "enabled_tools = [\"slow\"]\n[mcp_servers.probe.tools.slow]\napproval_mode = \"approve\"\n"
		}},
		"grok": {[]string{"--help"}, func(ws, srv string) []string {
			return []string{"--trust", "-p", mcpqual.PromptForClient("grok", "grok-capture-setup"), "--output-format", "streaming-json", "--cwd", ws, "-m", m}
		}, ".grok/config.toml", tomlCfg},
		"cursor": {[]string{"--help"}, func(ws, srv string) []string {
			return []string{"-p", mcpqual.Prompt("cursor-capture-setup"), "--output-format", "stream-json", "--model", m, "--workspace", ws, "--trust"}
		}, ".cursor/mcp.json", jsonCfg},
	}
	serverRe := regexp.MustCompile(`"command":"([^"]+)"|command = "([^"]+)"`)
	one := func(id string) func(t *testing.T) {
		return func(t *testing.T) {
			rc := recipes[id]
			q := newQualEnv(t)
			root := realTemp(t)
			out := filepath.Join(root, "out")
			var r result
			if id == "grok" {
				// The trusted Grok recipe's placement gate observes the
				// filesystem through the existing GrokPlacementFS seam with the
				// fixture root as its boundary (DW10, as in FP-12): a marker on
				// the host above the test's temporary root (a developer's .git)
				// is not part of this recipe case, while the gate still walks
				// every ancestor and applies every rule. The production runner
				// runs in process with the real fake-vendor and probe processes.
				log := &safeBuffer{}
				man := inProcessRun(t, q, q.capturePlan(nil, id), out, func(c *mcpqual.CaptureRunner) { c.GrokPlacementFS, c.Log = fixtureView{root}, log })
				r = result{code: man.ExitCode(), stderr: log.String()}
			} else {
				r = q.capture(q.capturePlan(nil, id), out, nil, "--allow-model-calls")
			}
			if r.code != 0 {
				t.Fatalf("%s capture = %+v", id, r)
			}
			recs := q.argvLog()
			if len(recs) != 1 {
				t.Fatalf("%s: %d sessions recorded", id, len(recs))
			}
			rec := recs[0]
			ws := filepath.Join(out, ".work", id+"-capture-setup")
			sm := serverRe.FindStringSubmatch(rec.Config)
			if sm == nil {
				t.Fatalf("%s: no probe command in %q", id, rec.Config)
			}
			srv := sm[1] + sm[2]
			if a, b := mustReal(t, srv), mustReal(t, qualBinary(t)); a != b {
				t.Fatalf("%s: probe %s is not the mcpqual executable %s", id, a, b)
			}
			if !slices.Equal(rec.Argv, rc.argv(ws, srv)) || rec.Config != rc.cfgBody(ws, srv) || filepath.Clean(rec.Cwd) != ws ||
				!strings.HasSuffix(rec.ConfigPath, "/"+rc.cfgRel) && id != "codex" {
				t.Fatalf("%s: argv %q cwd %s config %s %q", id, rec.Argv, rec.Cwd, rec.ConfigPath, rec.Config)
			}
			b := bundle(t, out)
			c := captureClient(t, b.Manifest, id)
			log := q.launchLog()
			cfg := string(b.Files[mcpqual.ClientFile(id, mcpqual.FileConfig)])
			// Cursor alone runs its one approval preparation, before the
			// session; Cursor's config.txt is then the configuration re-read
			// (unchanged by this enable).
			if enables := len(log["enable"]); (id == "cursor") != (enables == 1) || id == "cursor" && (c.Approval == nil || c.Approval.Scope != mcpqual.ScopeWorkspaceOnly) {
				t.Fatalf("%s: %d enables, approval %+v", id, enables, c.Approval)
			}
			// Design decoder-enrollment B2 (FP-20): the scoped Cursor file is
			// written for the pinned adapter only (the manifest's recorded
			// platform), verified, and seen by the session; no other client
			// or platform gets one.
			pinned := id == "cursor" && b.Manifest.OS+"/"+b.Manifest.Arch == "linux/amd64"
			if pinned != (c.ToolPermission != nil) || pinned && (c.ToolPermission.State != mcpqual.PermissionVerified || rec.Permission != "600 "+mcpqual.CursorToolPermissionContent) ||
				!pinned && rec.Permission != "<absent>" {
				t.Fatalf("%s: permission %+v seen %q", id, c.ToolPermission, rec.Permission)
			}
			if c.State != mcpqual.CaptureComplete || len(log["version"]) != 1 || len(log["help"]) != 1 || len(log["session"]) != 1 || c.Config.Path != rc.cfgRel ||
				!strings.Contains(cfg, "<workspace>/case.json") || strings.Contains(cfg, ws) || c.Probe.Receipts != 1 || !c.Probe.Completed || b.Manifest.Limits.SessionsPerClient != 1 {
				t.Fatalf("%s: %+v launches %v config %q", id, c, log, cfg)
			}
			// The help command, with the exact argv after the executable.
			if !strings.Contains(string(b.Files[mcpqual.ClientFile(id, mcpqual.FileHelpStdout)]), "usage: fake") || !slices.Equal(c.Argv[:1], rc.argv("<workspace>", "<server>")[:1]) {
				t.Fatalf("%s: help or argv %q", id, c.Argv)
			}
			// Setup only: the one session's case file named a zero-delay case
			// and its marker, the probe saw one setup call and nothing else,
			// and the default phase was not executed (structured, never a byte
			// search of the events).
			if err := setupOnly(rec.CaseFile, b.Files[mcpqual.ClientFile(id, mcpqual.FileServerEvents)], id+"-capture-setup"); err != nil ||
				!strings.Contains(r.stderr, "setup only; default phase not executed") || len(recs) != 1 {
				t.Fatalf("%s: scheduling %v: %s", id, err, r.stderr)
			}
			q.groupsGone()
		}
	}
	// The setup-only check's own regression (code review B1.5 round 3, C1):
	// a valid zero-delay setup whose start record's offset_ns is 15000
	// passes, while a default-phase delay or call is caught by structure.
	const caseID = "claude-capture-setup"
	cfJSON := func(delay int64, extra ...mcpqual.ProbeCase) string {
		cf := mcpqual.CaseFile{Version: 1, RunID: "run-1", Nonce: "nonce1", Cases: append([]mcpqual.ProbeCase{{CaseID: caseID, DelayMS: delay}, {CaseID: caseID + "-marker"}}, extra...)}
		b, _ := json.Marshal(cf)
		return string(b)
	}
	collide := `{"seq":1,"kind":"start","offset_ns":15000,"run_id":"run-1"}` + "\n" +
		`{"seq":2,"kind":"initialize","offset_ns":150000,"run_id":"run-1","client_name":"claude-cli","client_version":"1.0.0"}` + "\n" +
		`{"seq":3,"kind":"receipt","offset_ns":215000,"run_id":"run-1","case_id":"claude-capture-setup","request_id":3}` + "\n" +
		`{"seq":4,"kind":"scheduled","offset_ns":315000,"run_id":"run-1","case_id":"claude-capture-setup","request_id":3}` + "\n" +
		`{"seq":5,"kind":"completed","offset_ns":415000,"run_id":"run-1","case_id":"claude-capture-setup","request_id":3}` + "\n" +
		`{"seq":6,"kind":"eof","offset_ns":515000,"run_id":"run-1"}` + "\n" + `{"seq":7,"kind":"exit","offset_ns":615000,"run_id":"run-1","reason":"eof"}` + "\n"
	if _, intact, err := mcpqual.ParseProbeEvents([]byte(collide)); err != nil || !intact || !strings.Contains(collide, "15000") {
		t.Fatalf("collision vector %v %v", intact, err)
	}
	if err := setupOnly(cfJSON(0), []byte(collide), caseID); err != nil {
		t.Fatalf("a valid timestamp collision was refused: %v", err)
	}
	other := strings.Replace(collide, `{"seq":6,"kind":"eof"`, `{"seq":6,"kind":"receipt","offset_ns":515000,"run_id":"run-1","case_id":"claude-default-1","request_id":4}`+"\n"+`{"seq":7,"kind":"eof"`, 1)
	other = strings.Replace(other, `{"seq":7,"kind":"exit"`, `{"seq":8,"kind":"exit"`, 1)
	for name, tc := range map[string][2]string{
		"delay":      {cfJSON(15000), collide},
		"extra-case": {cfJSON(0, mcpqual.ProbeCase{CaseID: "claude-default-1", DelayMS: 15000}), collide},
		"other-call": {cfJSON(0), other},
		"no-call":    {cfJSON(0), strings.Join(strings.Split(collide, "\n")[:2], "\n") + "\n"},
		"bad-events": {cfJSON(0), "x\n"},
		"bad-case":   {"{", collide},
	} {
		if err := setupOnly(tc[0], []byte(tc[1]), caseID); err == nil {
			t.Fatalf("%s: a non-setup session passed", name)
		}
	}
	runInventory(t, 4, []string{"claude", "codex", "grok", "cursor"}, []captureCase{{"claude", one("claude")}, {"codex", one("codex")}, {"grok", one("grok")}, {"cursor", one("cursor")}})
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// captureKit is one real fake-vendor capture of claude carrying secret
// canaries, built once per process and used read-only (copied before any
// change): the evidence and enrollment parents' input.
type captureKit struct {
	out, home string
	bundle    *mcpqual.CaptureBundle
	secret    string
}

var (
	captureKitOnce sync.Once
	captureKitVal  *captureKit
)

// The kit's planted values (design decoder-enrollment r0.3): its help and
// stderr carry credential-word prose, kept, around values, redacted.
const (
	kitSecret = "sk-fake-secret-0123456789abcdef"
	kitHelp   = "usage: fake [options]\n  --api-key <KEY>  API key for auth, e.g. API_KEY=planted-help-value-1"
	kitStderr = "note: authentication uses a token here; token=planted-stderr-value-1\n"
)

func sharedCaptureKit(t *testing.T) *captureKit {
	t.Helper()
	captureKitOnce.Do(func() {
		q := newQualEnv(t)
		dir, err := os.MkdirTemp("", "mcpqual-capture-kit-")
		if err != nil {
			t.Fatal(err)
		}
		dir, _ = filepath.EvalSymlinks(dir)
		out := filepath.Join(dir, "out")
		r := q.capture(q.capturePlan(map[string]map[string]string{"claude": {"SECRET": kitSecret, "STDERR": kitStderr, "HELP": kitHelp}}, "claude"), out, nil, "--allow-model-calls")
		if r.code != 0 {
			t.Fatalf("kit capture = %+v", r)
		}
		q.groupsGone()
		captureKitVal = &captureKit{out: out, home: q.home, bundle: bundle(t, out), secret: kitSecret}
	})
	if captureKitVal == nil {
		t.Fatal("the capture kit was not built")
	}
	return captureKitVal
}

// inProcessCapture runs the production CaptureRunner with the real
// launcher, the fake vendor and the real probe, with injected small byte
// limits (design decoder-enrollment: function-test flood fixtures use
// small limits). GOOS and GOARCH are manifest labels supplied as test
// data, never read from the runtime.
func inProcessCapture(t *testing.T, q *qualEnv, plan map[string]any, lim mcpqual.CaptureLimits) (*mcpqual.CaptureManifest, string) {
	t.Helper()
	out := filepath.Join(realTemp(t), "out")
	return inProcessRun(t, q, plan, out, func(c *mcpqual.CaptureRunner) { c.Limits = lim }), out
}

// inProcessRun is inProcessCapture into out with the runner adjusted by
// mutate (test inputs only: injected limits, a reaper wrapper, the
// placement view of the FP-12 parent).
func inProcessRun(t *testing.T, q *qualEnv, plan map[string]any, out string, mutate func(c *mcpqual.CaptureRunner)) *mcpqual.CaptureManifest {
	t.Helper()
	b, _ := json.Marshal(plan)
	p, err := mcpqual.ParseCapturePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	c := &mcpqual.CaptureRunner{Plan: p, OutDir: out, GOOS: "linux", GOARCH: "amd64", ServerPath: qualBinary(t), BaseEnv: q.env(""), Launcher: procexec.Launcher{},
		Reaper: mcpqual.GroupReaper{Sig: mcpqual.SysSignaler{}, Clock: mcpqual.RealClock,
			Policy: mcpqual.CleanupPolicy{Grace: mcpqual.CleanupGrace, Limit: mcpqual.CleanupLimit, Poll: mcpqual.CleanupPoll}},
		Clock: mcpqual.RealClock, Log: io.Discard, RunID: "run-flood", Nonce: "nonceflood", CapturedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		HarnessVersion: mcpqual.HarnessVersion, Home: q.home}
	if mutate != nil {
		mutate(c)
	}
	m, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q.groupsGone()
	return m
}

func streamOf(t *testing.T, c mcpqual.CaptureClient, name string) mcpqual.CaptureStream {
	t.Helper()
	for _, s := range c.Streams {
		if s.Path == mcpqual.ClientFile(c.ID, name) {
			return s
		}
	}
	t.Fatalf("no stream %s", name)
	return mcpqual.CaptureStream{}
}

// FP-3: a valid, redacted, hash-addressed bundle; secret canaries absent
// even after decoding; the RedactionFixedPoint guard through
// ValidateEnrollment; the nonce kept; both streams flooded past injected
// bounds; corrupt evidence rejected.
func TestMCPCaptureEvidence(t *testing.T) {
	t.Parallel()
	kit := sharedCaptureKit(t)
	runInventory(t, 6, []string{"valid-bundle", "redaction-fixed-point", "nonce", "stdout-bound", "stderr-bound", "corrupt-evidence"}, []captureCase{
		{"valid-bundle", func(t *testing.T) {
			m := kit.bundle.Manifest
			if len(m.Files) != 9 || m.Schema != mcpqual.CaptureSchema || m.RedactionPolicy != mcpqual.CaptureRedactionPolicy || m.PlanSHA256 != kit.bundle.Manifest.Files[len(m.Files)-1].SHA256 {
				t.Fatalf("manifest %+v", m)
			}
			for _, f := range m.Files {
				info, err := os.Lstat(filepath.Join(kit.out, filepath.FromSlash(f.Path)))
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
					t.Fatalf("%s: %v", f.Path, err)
				}
			}
			if info, _ := os.Stat(kit.out); info.Mode().Perm() != 0o700 {
				t.Fatal("the capture directory is not 0700")
			}
			if _, err := os.Stat(filepath.Join(kit.out, ".work")); err == nil {
				t.Fatal("the workspace survived")
			}
		}},
		{"redaction-fixed-point", func(t *testing.T) {
			var view strings.Builder
			view.Write(kit.bundle.ManifestBytes)
			for _, data := range kit.bundle.Files {
				view.Write(data)
			}
			tr, err := mcpqual.ReplayTranscript(kit.bundle.Files[mcpqual.ClientFile("claude", mcpqual.FileVendorEvents)])
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range tr.Lines {
				var v any
				json.Unmarshal(l.Data, &v)
				b, _ := json.Marshal(v)
				view.Write(l.Data)
				view.Write(b)
			}
			for _, s := range []string{kit.secret, kit.home, kit.out, "planted-help-value-1", "planted-stderr-value-1"} {
				if strings.Contains(view.String(), s) {
					t.Fatalf("%q survived (decoded)", s)
				}
			}
			if !strings.Contains(view.String(), mcpqual.Redacted) {
				t.Fatal("nothing was redacted")
			}
			// Credential-word prose is kept, values are redacted in place, and
			// nothing is omitted (r0.3).
			help, stderr := string(kit.bundle.Files[mcpqual.ClientFile("claude", mcpqual.FileHelpStdout)]), string(kit.bundle.Files[mcpqual.ClientFile("claude", mcpqual.FileVendorStderr)])
			if help != strings.Replace(kitHelp, "planted-help-value-1", mcpqual.Redacted, 1)+"\n" || stderr != strings.Replace(kitStderr, "planted-stderr-value-1", mcpqual.Redacted, 1) {
				t.Fatalf("help %q stderr %q", help, stderr)
			}
			for _, st := range kit.bundle.Manifest.Clients[0].Streams {
				if !st.Clean() {
					t.Fatalf("stream %+v", st)
				}
			}
			// Design decoder-enrollment B1.5: the new probe observation is part
			// of the same redacted manifest (its members are fixed enums).
			if o := kit.bundle.Manifest.Clients[0].Probe.Observation; o == nil || o.EndState != mcpqual.EndIntact || !o.CleanSession ||
				!strings.Contains(string(kit.bundle.ManifestBytes), `"observation": {`) {
				t.Fatalf("kit observation %+v", o)
			}
			// Design decoder-enrollment B2 (FP-20): the pinned Cursor
			// adapter's permission record is labeled, owner-free evidence and
			// a fixed point of the capture policy with the owner's literals.
			q, pc, pb, pout := projectRun(t, false, map[string]string{"ENABLE": "project"}, nil, nil)
			red := mcpqual.NewCaptureRedactor([]string{"cursor-owner-secret-1"}, map[string]string{q.home: "<home>", pout: "<out>"})
			if pc.ToolPermission == nil || pc.ToolPermission.Path != "<workspace>/.cursor/cli.json" || bytes.Contains(pb.ManifestBytes, []byte(q.home)) ||
				bytes.Contains(pb.ManifestBytes, []byte(pout)) || mcpqual.RedactionFixedPoint(mcpqual.CaptureManifestName, pb.ManifestBytes, red) != nil {
				t.Fatalf("permission evidence %+v", pc.ToolPermission)
			}
			e := sharedEnrollment(t)
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(e.repo), Registry: e.reg}); err != nil {
				t.Fatal(err)
			}
			// A residual planted value (with every hash made consistent) is
			// not a fixed point: enrollment refuses it.
			repo, _ := enrolledCopy(t)
			leakHelp(t, repo, e, "token=leaked-value-1\n")
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: e.reg}); err == nil || !strings.Contains(err.Error(), "redaction fixed point") {
				t.Fatalf("residual secret: %v", err)
			}
			// The owner's local literal (here the observed clientInfo) is not
			// a fixed point: enrollment refuses rather than edit evidence.
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(e.repo), Registry: e.reg, Literals: []string{"claude-cli"}}); err == nil ||
				!strings.Contains(err.Error(), "redaction fixed point") {
				t.Fatalf("fixed point: %v", err)
			}
		}},
		{"nonce", func(t *testing.T) {
			c := kit.bundle.Manifest.Clients[0]
			tr, err := mcpqual.ReplayTranscript(kit.bundle.Files[mcpqual.ClientFile("claude", mcpqual.FileVendorEvents)])
			if err != nil || len(tr.Lines) != 1 {
				t.Fatalf("transcript: %v", err)
			}
			if len(c.Nonce) != 32 || !strings.Contains(string(tr.Lines[0].Data), `\"nonce\":\"`+c.Nonce+`\"`) || !strings.Contains(string(kit.bundle.ManifestBytes), `"nonce": "`+c.Nonce+`"`) {
				t.Fatalf("nonce %q not kept exactly in %s", c.Nonce, tr.Lines[0].Data)
			}
		}},
		{"stdout-bound", func(t *testing.T) {
			q := newQualEnv(t)
			m, out := inProcessCapture(t, q, q.capturePlan(map[string]map[string]string{"claude": {"FLOOD_STDOUT": strconv.Itoa(1 << 20)}}, "claude"), mcpqual.CaptureLimits{Stream: 4096})
			c := captureClient(t, m, "claude")
			s := streamOf(t, c, mcpqual.FileVendorEvents)
			if deref(c.Reason) != mcpqual.ReasonEvidenceTruncated || !s.InputTruncated || *c.Session.Exit != 0 || c.Session.Watchdog || c.Session.StdoutHeld {
				t.Fatalf("stdout flood: %s %+v %+v", deref(c.Reason), s, c.Session)
			}
			if info, _ := os.Stat(filepath.Join(out, "clients", "claude", mcpqual.FileVendorEvents)); info.Size() > 2*4096 {
				t.Fatalf("stored %d bytes", info.Size())
			}
		}},
		{"stderr-bound", func(t *testing.T) {
			q := newQualEnv(t)
			m, out := inProcessCapture(t, q, q.capturePlan(map[string]map[string]string{"claude": {"FLOOD_STDERR": strconv.Itoa(1 << 20)}}, "claude"), mcpqual.CaptureLimits{Stream: 4096})
			c := captureClient(t, m, "claude")
			s := streamOf(t, c, mcpqual.FileVendorStderr)
			if deref(c.Reason) != mcpqual.ReasonEvidenceTruncated || !s.InputTruncated || *c.Session.Exit != 0 || c.Session.StderrHeld {
				t.Fatalf("stderr flood: %s %+v %+v", deref(c.Reason), s, c.Session)
			}
			if info, _ := os.Stat(filepath.Join(out, "clients", "claude", mcpqual.FileVendorStderr)); info.Size() > 4096 {
				t.Fatalf("stored %d bytes", info.Size())
			}
		}},
		{"corrupt-evidence", func(t *testing.T) {
			vendor := mcpqual.ClientFile("claude", mcpqual.FileVendorEvents)
			for name, mutate := range map[string]func(dir string){
				"changed-byte": func(d string) { appendFile(t, filepath.Join(d, filepath.FromSlash(vendor)), "\n") },
				"extra-file":   func(d string) { os.WriteFile(filepath.Join(d, "clients", "claude", "notes.txt"), []byte("x"), 0o600) },
				"missing-file": func(d string) { os.Remove(filepath.Join(d, filepath.FromSlash(vendor))) },
				"symlink": func(d string) {
					p := filepath.Join(d, filepath.FromSlash(vendor))
					os.Remove(p)
					os.Symlink(filepath.Join(d, "plan.json"), p)
				},
				"unknown-field": func(d string) { rewrite(t, filepath.Join(d, "manifest.json"), `"schema"`, `"forged": true, "schema"`) },
			} {
				d := filepath.Join(realTemp(t), "copy")
				copyTree(t, kit.out, d)
				mutate(d)
				if _, err := mcpqual.ValidateCaptureBundle(os.DirFS(d), "."); err == nil {
					t.Fatalf("%s: corrupt evidence accepted", name)
				}
			}
		}},
	})
}

// leakHelp appends text to the enrolled help-stdout.txt of repo and makes
// the manifest inventory and the index consistent with it.
func leakHelp(t *testing.T, repo string, e *enrollmentKit, text string) {
	t.Helper()
	dir := filepath.Join(repo, filepath.FromSlash(e.entry.Bundle))
	rel := mcpqual.ClientFile("claude", mcpqual.FileHelpStdout)
	appendFile(t, filepath.Join(dir, filepath.FromSlash(rel)), text)
	data, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	var m mcpqual.CaptureManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	for i := range m.Files {
		if m.Files[i].Path == rel {
			m.Files[i].SHA256, m.Files[i].Bytes = sha(data), int64(len(data))
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(&m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), buf.Bytes(), 0o600)
	en := e.entry
	en.ManifestSHA256 = sha(buf.Bytes())
	writeEnrollment(t, repo, &en, e.oracle)
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Close()
}

func rewrite(t *testing.T, p, old, new string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil || !bytes.Contains(b, []byte(old)) {
		t.Fatalf("%s lacks %q", p, old)
	}
	os.WriteFile(p, bytes.Replace(b, []byte(old), []byte(new), 1), 0o600)
}

// FP-4: process-group cleanup reused with proven absence; interrupt,
// blocked or no-tool sessions and a held pipe end bounded; a cleanup
// failure cannot complete and starts no further client.
func TestMCPCaptureLifecycle(t *testing.T) {
	t.Parallel()
	small := func(plan map[string]any, caseMS int) map[string]any {
		plan["limits"] = map[string]any{"max_sessions_per_client": 1, "max_case_ms": caseMS, "max_client_ms": 60000}
		return plan
	}
	session := func(t *testing.T, plan map[string]any, wantCode int, extra ...string) (*qualEnv, mcpqual.CaptureClient, result) {
		t.Helper()
		q := plan["q"].(*qualEnv)
		delete(plan, "q")
		out := filepath.Join(realTemp(t), "out")
		r := q.capture(plan, out, extra, "--allow-model-calls")
		if r.code != wantCode {
			t.Fatalf("capture = %+v", r)
		}
		return q, captureClient(t, bundle(t, out).Manifest, "claude"), r
	}
	planFor := func(t *testing.T, settings map[string]string) map[string]any {
		q := newQualEnv(t)
		p := q.capturePlan(map[string]map[string]string{"claude": settings}, "claude")
		p["q"] = q
		return p
	}
	noTyped := func(t *testing.T, c mcpqual.CaptureClient) {
		b, _ := json.Marshal(c)
		for _, kind := range []string{mcpqual.KindAuthError, mcpqual.SafeModelUnavailable, mcpqual.KindMCPTimeout} {
			if strings.Contains(string(b), kind) {
				t.Fatalf("a typed %s was inferred: %s", kind, b)
			}
		}
	}
	runInventory(t, 9, []string{"resistant", "leader-first", "interrupt", "auth-failure", "model-failure", "approval-blocked", "no-tool", "held-pipe", "cleanup-failure"}, []captureCase{
		{"resistant", func(t *testing.T) {
			// The session ignores TERM; the session watchdog (lowered by the
			// plan) starts the escalation and KILL ends it after the grace.
			q, c, _ := session(t, small(planFor(t, map[string]string{"MODE": "resistant"}), 800), 5)
			cc := c.Session.Cleanup
			if deref(c.Reason) != mcpqual.ReasonSessionWatchdog || !c.Session.Watchdog || !cc.TermSent || !cc.KillSent || !cc.GroupGone || cc.Error != nil {
				t.Fatalf("resistant: %s %+v", deref(c.Reason), cc)
			}
			q.groupsGone()
		}},
		{"leader-first", func(t *testing.T) {
			q, c, _ := session(t, planFor(t, map[string]string{"MODE": "parent-exits-first"}), 0)
			cc := c.Session.Cleanup
			if c.State != mcpqual.CaptureComplete || !cc.LeaderExitedFirst || !cc.TermSent || !cc.KillSent || !cc.GroupGone || len(q.launchLog()["descendant"]) != 1 {
				t.Fatalf("leader first: %+v", cc)
			}
			q.groupsGone()
		}},
		{"interrupt", func(t *testing.T) {
			q := newQualEnv(t)
			fifo, ready := q.readyFIFO()
			out := filepath.Join(realTemp(t), "out")
			cmd, stdout := q.startCapture(q.capturePlan(map[string]map[string]string{"claude": {"MODE": "hang", "READY": fifo}, "codex": nil}, "claude", "codex"), out)
			ready()
			cmd.Process.Signal(syscall.SIGINT)
			cmd.Wait()
			m := bundle(t, out).Manifest
			c := captureClient(t, m, "claude")
			if cmd.ProcessState.ExitCode() != 130 || m.State != mcpqual.CaptureInterrupted || !c.Session.Interrupted || !c.Session.Cleanup.TermSent ||
				!c.Session.Cleanup.GroupGone || captureClient(t, m, "codex").State != mcpqual.CaptureNotRun {
				t.Fatalf("interrupt = %d %s %+v", cmd.ProcessState.ExitCode(), stdout.String(), c.Session)
			}
			q.groupsGone()
		}},
		{"auth-failure", func(t *testing.T) {
			q, c, _ := session(t, planFor(t, map[string]string{"SCENARIO": "noprobe", "EXIT": "1", "STDERR": "Error: authentication required (run login)\n"}), 5)
			if deref(c.Reason) != mcpqual.ReasonProbeNotObserved || *c.Session.Exit != 1 || len(q.launchLog()["session"]) != 1 {
				t.Fatalf("auth: %s %v", deref(c.Reason), q.launchLog())
			}
			noTyped(t, c)
			q.groupsGone()
		}},
		{"model-failure", func(t *testing.T) {
			q, c, _ := session(t, planFor(t, map[string]string{"SCENARIO": "noprobe", "EXIT": "1", "STDERR": "error: unknown model fake-model-1\n"}), 5)
			if deref(c.Reason) != mcpqual.ReasonProbeNotObserved || len(q.launchLog()["session"]) != 1 {
				t.Fatalf("model: %s", deref(c.Reason))
			}
			noTyped(t, c)
			q.groupsGone()
		}},
		{"approval-blocked", func(t *testing.T) {
			// A session waiting on an approval that never comes: the watchdog
			// ends it; no approval flag is ever added.
			q, c, _ := session(t, small(planFor(t, map[string]string{"MODE": "hang"}), 500), 5)
			if deref(c.Reason) != mcpqual.ReasonSessionWatchdog || slices.Contains(c.Argv, "--approve-mcps") || c.Probe.Receipts != 0 {
				t.Fatalf("blocked: %s %q", deref(c.Reason), c.Argv)
			}
			noTyped(t, c)
			q.groupsGone()
		}},
		{"no-tool", func(t *testing.T) {
			q, c, _ := session(t, planFor(t, map[string]string{"SCENARIO": "no-tool"}), 5)
			if deref(c.Reason) != mcpqual.ReasonProbeNotObserved || *c.Session.Exit != 0 || !c.Probe.Initialized {
				t.Fatalf("no tool: %s %+v", deref(c.Reason), c.Probe)
			}
			q.groupsGone()
		}},
		{"held-pipe", func(t *testing.T) {
			// A descendant outside the group keeps both pipes: the reaped
			// leader's Wait is never held; the streams end at the bounded
			// post-cleanup wait and the run is not complete.
			p := planFor(t, map[string]string{"MODE": "held"})
			q := p["q"].(*qualEnv)
			t.Cleanup(func() {
				for _, pid := range q.launchLog()["escaped"] {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			_, c, _ := session(t, p, 5)
			escaped := q.launchLog()["escaped"]
			if !strings.HasPrefix(deref(c.Reason), mcpqual.ReasonStreamHeld) || !c.Session.StdoutHeld || !c.Session.StderrHeld || len(escaped) != 1 {
				t.Fatalf("held: %s %+v", deref(c.Reason), c.Session)
			}
			syscall.Kill(escaped[0], syscall.SIGKILL)
			if err := processgroup.WaitGone(processgroup.SysSignaler{}, processgroup.RealClock{}, 5*time.Second, 10*time.Millisecond, escaped[0]); err != nil {
				t.Fatal(err)
			}
			q.groupsGone()
		}},
		{"cleanup-failure", func(t *testing.T) {
			// Four clients (B1: thirteen process groups at most, with Cursor's
			// one approval preparation); the first cleanup failure stops them.
			q := newQualEnv(t)
			out := filepath.Join(realTemp(t), "out")
			r := q.capture(q.capturePlan(nil, "claude", "codex", "grok", "cursor"), out, []string{mcpqual.FaultEnv + "=" + mcpqual.FaultValue}, "--allow-model-calls")
			m := bundle(t, out).Manifest
			c := captureClient(t, m, "claude")
			if r.code != 5 || m.Cleanup.OK || m.State == mcpqual.CaptureComplete || deref(c.Reason) != mcpqual.ReasonCleanupFailed ||
				deref(captureClient(t, m, "codex").Reason) != mcpqual.ReasonCleanupFailed || len(q.launchLog()["session"]) != 0 ||
				!strings.Contains(r.stderr, "bounded cleanup of up to 13 process groups") || captureClient(t, m, "cursor").Approval.Stage.State != mcpqual.StageNotRun {
				t.Fatalf("cleanup failure = %+v %+v", r, m.Cleanup)
			}
			q.groupsGone()
		}},
	})
}

// enrollmentKit is a temporary repository holding the capture kit's
// bundle enrolled as a fake fixture, its reviewed oracle, the index and
// the test-only qualified registry.
type enrollmentKit struct {
	repo   string
	entry  mcpqual.EnrollmentEntry
	oracle *mcpqual.ExpectedOracle
	reg    mcpqual.Registry
}

var (
	enrollmentOnce sync.Once
	enrollmentVal  *enrollmentKit
)

// sharedEnrollment builds (once per process) the enrollment kit. The
// oracle is written from the inspected bytes (the transcript's recorded
// offset, the manifest's nonce, the probe's clientInfo), never from the
// decoder's output; the platform comes from the manifest, never the
// runtime.
func sharedEnrollment(t *testing.T) *enrollmentKit {
	t.Helper()
	kit := sharedCaptureKit(t)
	enrollmentOnce.Do(func() {
		m := kit.bundle.Manifest
		c := m.Clients[0]
		platform := m.OS + "/" + m.Arch
		version := *c.ObservedVersion
		tr, err := mcpqual.ReplayTranscript(kit.bundle.Files[mcpqual.ClientFile("claude", mcpqual.FileVendorEvents)])
		if err != nil || len(tr.Lines) != 1 {
			t.Fatalf("kit transcript: %v", err)
		}
		off := tr.Lines[0].OffsetNS
		o := &mcpqual.ExpectedOracle{CaseID: c.CaseID, Nonce: c.Nonce,
			Events: []mcpqual.Event{{CaseID: c.CaseID, Kind: mcpqual.KindToolCall, OffsetNS: off, RequestID: "toolu_1"},
				{CaseID: c.CaseID, Kind: mcpqual.KindToolResult, OffsetNS: off, RequestID: "toolu_1", Nonce: c.Nonce}},
			Terminal: true, ClientInfo: mcpqual.ExpectedClientInfo{Name: *c.Probe.ClientName, Version: *c.Probe.ClientVersion}, RequesterCompatible: true,
			ErrorKinds: []string{}, Capabilities: []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}, CaptureState: mcpqual.CaptureComplete,
			SourceCaptureSHA256: sha(kit.bundle.ManifestBytes), Attestation: mcpqual.Attestation{Owner: "function-test owner", Reviewer: "function-test reviewer", Policy: mcpqual.CaptureRedactionPolicy}}
		e := mcpqual.EnrollmentEntry{Client: "claude", Decoder: "claude-json", Version: version, Platform: platform, Fixture: mcpqual.EnrolledFixtureID("claude-json", version, platform),
			Bundle: mcpqual.EnrolledBundlePath("claude", version, platform, m.RunID), ManifestSHA256: sha(kit.bundle.ManifestBytes)}
		dir, err := os.MkdirTemp("", "mcpqual-enrollment-kit-")
		if err != nil {
			t.Fatal(err)
		}
		repo, _ := filepath.EvalSymlinks(dir)
		copyTree(t, kit.out, filepath.Join(repo, filepath.FromSlash(e.Bundle)))
		writeEnrollment(t, repo, &e, o)
		// An injected legacy registry: the synthetic versions and this one
		// fake qualified version (the production registry's real versions
		// are backed only by the production index).
		reg := mcpqual.SyntheticRegistry().WithVersion("claude-json", mcpqual.DecoderVersion{Version: version, Fixture: e.Fixture, Qualified: true,
			Evidence: []mcpqual.DecoderEvidence{{Platform: platform, Fixture: e.Fixture, Kinds: o.Capabilities}}})
		enrollmentVal = &enrollmentKit{repo: repo, entry: e, oracle: o, reg: reg}
	})
	if enrollmentVal == nil {
		t.Fatal("the enrollment kit was not built")
	}
	return enrollmentVal
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeEnrollment writes the oracle and an index naming entry e.
func writeEnrollment(t *testing.T, repo string, e *mcpqual.EnrollmentEntry, o *mcpqual.ExpectedOracle) {
	t.Helper()
	ob, _ := json.MarshalIndent(o, "", "  ")
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(e.Bundle), mcpqual.ExpectedName), ob, 0o600); err != nil {
		t.Fatal(err)
	}
	e.ExpectedSHA256 = sha(ob)
	ib, _ := json.MarshalIndent(mcpqual.EnrollmentIndex{Schema: mcpqual.EnrollmentSchema, Entries: []mcpqual.EnrollmentEntry{*e}}, "", "  ")
	mkdir(t, filepath.Join(repo, filepath.FromSlash(path.Dir(mcpqual.EnrollmentIndexPath))))
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(mcpqual.EnrollmentIndexPath)), ib, 0o600); err != nil {
		t.Fatal(err)
	}
}

// enrolledCopy is a temporary copy of the enrollment kit's repository.
func enrolledCopy(t *testing.T) (string, *enrollmentKit) {
	e := sharedEnrollment(t)
	d := filepath.Join(realTemp(t), "repo")
	copyTree(t, e.repo, d)
	return d, e
}

// FP-5: the production validator rejects forged provenance, identity
// mismatches and unsupported capabilities; the production index holds
// no entry in slice A (real entries pass only in slice B).
func TestMCPEnrollmentContract(t *testing.T) {
	t.Parallel()
	validate := func(repo string, reg mcpqual.Registry) error {
		_, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: reg})
		return err
	}
	runInventory(t, 4, []string{"provenance", "identity-mismatch", "unsupported-capability", "production-index"}, []captureCase{
		{"provenance", func(t *testing.T) {
			repo, e := enrolledCopy(t)
			if err := validate(repo, e.reg); err != nil {
				t.Fatal(err)
			}
			rewrite(t, filepath.Join(repo, filepath.FromSlash(e.entry.Bundle), "manifest.json"), `"harness_version": "`, `"harness_version": "forged-`)
			if err := validate(repo, e.reg); err == nil || !strings.Contains(err.Error(), "forged or altered provenance") {
				t.Fatalf("forged manifest: %v", err)
			}
			repo, e = enrolledCopy(t)
			appendFile(t, filepath.Join(repo, filepath.FromSlash(e.entry.Bundle), "clients", "claude", mcpqual.FileServerEvents), "\n")
			if err := validate(repo, e.reg); err == nil {
				t.Fatal("an altered payload passed")
			}
			// Design decoder-enrollment B1.5: a probe observation that the
			// retained bytes do not replay to is refused even with the index
			// made consistent with the edited manifest.
			repo, e = enrolledCopy(t)
			dir := filepath.Join(repo, filepath.FromSlash(e.entry.Bundle))
			editManifest(t, dir, func(m map[string]any) {
				client0(m)["probe"].(map[string]any)["observation"].(map[string]any)["terminal_kind"] = mcpqual.EvCancelled
			})
			mb, _ := os.ReadFile(filepath.Join(dir, mcpqual.CaptureManifestName))
			en := e.entry
			en.ManifestSHA256 = sha(mb)
			writeEnrollment(t, repo, &en, e.oracle)
			if err := validate(repo, e.reg); err == nil || strings.Contains(err.Error(), "forged or altered provenance") {
				t.Fatalf("a forged observation: %v", err)
			}
		}},
		{"identity-mismatch", func(t *testing.T) {
			for name, mutate := range map[string]func(en *mcpqual.EnrollmentEntry){
				"version":  func(en *mcpqual.EnrollmentEntry) { en.Version = "2.1.292 (Claude Code)" },
				"platform": func(en *mcpqual.EnrollmentEntry) { en.Platform = "darwin/riscv64" },
			} {
				repo, e := enrolledCopy(t)
				en := e.entry
				mutate(&en)
				moved := mcpqual.EnrolledBundlePath(en.Client, en.Version, en.Platform, path.Base(en.Bundle))
				mkdir(t, filepath.Dir(filepath.Join(repo, filepath.FromSlash(moved))))
				if err := os.Rename(filepath.Join(repo, filepath.FromSlash(en.Bundle)), filepath.Join(repo, filepath.FromSlash(moved))); err != nil {
					t.Fatal(err)
				}
				en.Bundle, en.Fixture = moved, mcpqual.EnrolledFixtureID(en.Decoder, en.Version, en.Platform)
				writeEnrollment(t, repo, &en, e.oracle)
				if err := validate(repo, e.reg); err == nil {
					t.Fatalf("%s mismatch passed", name)
				}
			}
		}},
		{"unsupported-capability", func(t *testing.T) {
			repo, e := enrolledCopy(t)
			// The registry claims a typed timeout the setup never showed.
			reg := mcpqual.SyntheticRegistry().WithVersion("claude-json", mcpqual.DecoderVersion{Version: e.entry.Version, Fixture: e.entry.Fixture, Qualified: true,
				Evidence: []mcpqual.DecoderEvidence{{Platform: e.entry.Platform, Fixture: e.entry.Fixture, Kinds: append(append([]string(nil), e.oracle.Capabilities...), mcpqual.CapMCPTimeout)}}})
			if err := validate(repo, reg); err == nil || !strings.Contains(err.Error(), "differ from the oracle") {
				t.Fatalf("unsupported registry capability: %v", err)
			}
			// An oracle claiming an undemonstrated capability.
			o := *e.oracle
			o.Capabilities = append(append([]string(nil), o.Capabilities...), mcpqual.CapAuthError)
			en := e.entry
			writeEnrollment(t, repo, &en, &o)
			if err := validate(repo, e.reg); err == nil || !strings.Contains(err.Error(), "not demonstrated") {
				t.Fatalf("undemonstrated capability: %v", err)
			}
		}},
		{"production-index", func(t *testing.T) {
			root := testkit.MustRepoRoot(t)
			// Confined opening of the repository (code review C5, round 2
			// C2): a held os.Root, every component opened relative to its
			// parent. Design decoder-enrollment B2 and B3: exactly the four
			// sanitised linux/amd64 enrollments and their qualified versions.
			idx, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{Root: root, Registry: mcpqual.DefaultRegistry()})
			if err != nil || len(idx.Entries) != 4 || len(mcpqual.DefaultRegistry().QualifiedVersions()) != 4 {
				t.Fatalf("production enrollment (slices B2 and B3: four entries): %v", err)
			}
			for _, e := range idx.Entries {
				if e.Platform != "linux/amd64" || e.SanitizationSHA256 == nil {
					t.Fatalf("production entry %+v", e)
				}
			}
			// A fake fixture never enters an index.
			e := sharedEnrollment(t).entry
			e.Fixture = "claude-json/actual-test"
			b, _ := json.Marshal(mcpqual.EnrollmentIndex{Schema: mcpqual.EnrollmentSchema, Entries: []mcpqual.EnrollmentEntry{e}})
			if _, err := mcpqual.ParseEnrollmentIndex(b); err == nil {
				t.Fatal("a fake fixture was indexed")
			}
		}},
	})
}

// FP-6 (design decoder-enrollment B2): the three enrolled real fixtures
// replay offline through their exact-version decoders and the independent
// probe replay to their reviewed oracles; Cursor's exact version is
// neither selectable nor qualified and has no real entry (B2); since B3
// the enrolled Cursor fixture replays and only its exact version is
// selectable; synthetic prose never qualifies.
func TestMCPEnrollmentReplay(t *testing.T) {
	t.Parallel()
	replay := func(t *testing.T, client string) {
		e, b, o, _ := enrolledBundle(t, client)
		if err := mcpqual.Replay(mcpqual.DefaultRegistry(), e, b, o); err != nil {
			t.Fatalf("%s replay: %v", client, err)
		}
		if len(o.Events) != 2 || !o.Terminal || o.Inconclusive != "" || len(o.ErrorKinds) != 0 || o.CaptureState != mcpqual.CaptureComplete ||
			!slices.Equal(o.Capabilities, []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}) || !o.RequesterCompatible {
			t.Fatalf("%s oracle %+v", client, o)
		}
	}
	runInventory(t, 5, []string{"claude", "codex", "grok", "cursor", "synthetic-prose"}, []captureCase{
		{"claude", func(t *testing.T) { replay(t, "claude") }},
		{"codex", func(t *testing.T) { replay(t, "codex") }},
		{"grok", func(t *testing.T) { replay(t, "grok") }},
		// Design decoder-enrollment B3 (FP-23, amending B2's cursor-blocked):
		// the enrolled Cursor fixture replays like the others, and only its
		// exact version is selectable and qualified.
		{"cursor", func(t *testing.T) {
			replay(t, "cursor")
			reg := mcpqual.DefaultRegistry()
			if _, v, err := reg.Select("cursor-jsonl", mcpqual.CursorRealVersion); err != nil || !v.Qualified || len(v.Evidence) != 1 {
				t.Fatalf("the enrolled Cursor version: %v %+v", err, v)
			}
			for _, near := range []string{"2026.10.01", "2026.10.01-e373343", mcpqual.CursorRealVersion + " "} {
				if _, _, err := reg.Select("cursor-jsonl", near); err == nil {
					t.Fatalf("near version %q is selectable", near)
				}
			}
		}},
		{"synthetic-prose", func(t *testing.T) {
			e := sharedEnrollment(t)
			b, err := mcpqual.ValidateCaptureBundle(os.DirFS(filepath.Join(e.repo, filepath.FromSlash(e.entry.Bundle))), ".", mcpqual.ExpectedName)
			if err != nil {
				t.Fatal(err)
			}
			if err := mcpqual.Replay(e.reg, e.entry, b, e.oracle); err != nil {
				t.Fatal(err)
			}
			// The same nonce in model prose, with no structured result.
			prose := `[{"type":"assistant","message":{"content":[{"type":"text","text":"The slow tool returned {\"nonce\":\"` + e.oracle.Nonce + `\"} and timed out"}]}},{"type":"result","subtype":"success"}]`
			w, _ := json.Marshal(map[string]any{"offset_ns": 0, "data": prose})
			files := map[string][]byte{}
			for k, v := range b.Files {
				files[k] = v
			}
			files[mcpqual.ClientFile("claude", mcpqual.FileVendorEvents)] = append(w, '\n')
			forged := *b
			forged.Files = files
			if err := mcpqual.Replay(e.reg, e.entry, &forged, e.oracle); err == nil {
				t.Fatal("synthetic prose replayed as a result")
			}
		}},
	})
}

// shortRun is one in-process qualification of the fake vendor under the
// test-only qualified registry (scaled 1:100 like TestMCPShortConfirmation)
// and its evidence directory.
func shortRun(t *testing.T, timeoutMS string, kinds []string) (*mcpqual.Report, string, mcpqual.Registry) {
	t.Helper()
	q := newQualEnv(t)
	version := captureVersions["claude"]
	reg := mcpqual.DefaultRegistry().WithVersion("claude-json", mcpqual.DecoderVersion{Version: version, Fixture: "claude-json/actual-function-test", Qualified: true,
		Evidence: []mcpqual.DecoderEvidence{{Platform: "linux/amd64", Fixture: "claude-json/actual-function-test", Kinds: kinds}}})
	c := q.fakeClient("claude", map[string]string{"VERSION": version, "TIMEOUT_MS": timeoutMS}, map[string]any{"default": map[string]any{"delays_ms": []int{150}}})
	c["decoder_fixture"] = "claude-json/actual-function-test"
	delete(c["config"].(map[string]any), "raised")
	delete(c, "override")
	plan := map[string]any{"version": 1, "clients": []any{c}, "limits": map[string]any{"max_sessions_per_client": 3, "max_case_ms": 1200, "max_client_ms": 3600}}
	b, _ := json.Marshal(plan)
	p, err := mcpqual.ParsePlan(b, reg)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(realTemp(t), "out")
	r := &mcpqual.Runner{Plan: p, PlanSHA256: sha(b), OutDir: out, GOOS: "linux", GOARCH: "amd64", Hostname: "function-host", ServerPath: qualBinary(t),
		BaseEnv: q.env(""), Launcher: procexec.Launcher{}, Reaper: mcpqual.GroupReaper{Sig: mcpqual.SysSignaler{}, Clock: mcpqual.RealClock,
			Policy: mcpqual.CleanupPolicy{Grace: mcpqual.CleanupGrace, Limit: mcpqual.CleanupLimit, Poll: mcpqual.CleanupPoll}},
		Clock: mcpqual.RealClock, Registry: reg, Log: io.Discard, RunID: "run-short-" + timeoutMS, Nonce: "nonceshort", CaptureDate: "2026-10-06",
		HarnessVersion: mcpqual.HarnessVersion, Home: q.home}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q.groupsGone()
	return rep, out, reg
}

// shortRepo is a scratch repository whose claude entry has the given
// exact coordinator (MCP publication) version and platform.
func shortRepo(t *testing.T, version, platform string) (string, *mcpqual.CatalogBase) {
	t.Helper()
	repo := qualRepo(t)
	p := filepath.Join(repo, mcpqual.CatalogJSONPath)
	es, err := catalog.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range es {
		if es[i].ID == "claude" {
			// The publication target (design catalog-version); the worker
			// version stays the checked-in worker identity.
			es[i].CoordinatorVersion, es[i].Platform = version, platform
		}
	}
	b, _ := mcpqual.RenderCatalog(es)
	os.WriteFile(p, b, 0o644)
	base, err := mcpqual.ReadCatalogBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, base
}

// FP-7: with fake qualified evidence a short schedule yields the repeated
// lower bound and a publishable matching-platform fact; an unsupported
// error kind, a version conflict and a platform conflict fail closed.
func TestMCPEnrolledShortConfirmation(t *testing.T) {
	t.Parallel()
	success := []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}
	var lower *mcpqual.Report
	var lowerOut string
	runInventory(t, 4, []string{"repeated-lower-bound-publication", "unsupported-error", "version-conflict", "platform-conflict"}, []captureCase{
		{"repeated-lower-bound-publication", func(t *testing.T) {
			lower, lowerOut, _ = shortRun(t, "0", success)
			def := reportPhase(t, reportClient(t, lower, "claude"), mcpqual.PhaseDefault)
			if lower.Outcome != mcpqual.StatusConclusive || ms(def.LowerBoundMS) != 150 || def.Observations != 2 || len(def.Cases) != 2 {
				t.Fatalf("lower bound %+v", def)
			}
			repo, base := shortRepo(t, captureVersions["claude"], "linux/amd64")
			if _, err := mcpqual.ProposePatch(lower, base, lowerOut); err != nil {
				t.Fatal(err)
			}
			if err := mcpqual.Publish(lowerOut, repo); err != nil {
				t.Fatal(err)
			}
			facts := repoFacts(t, repo)["claude"].Facts
			if f := facts["mcp_timeout"]; f.Status != catalog.Verified || !strings.Contains(f.Value, "this is a lower bound, not the default") {
				t.Fatalf("published %+v", f)
			}
			for _, key := range []string{"mcp_timeout_override", "mcp_progress_extension"} {
				if facts[key].Status != catalog.Unverified {
					t.Fatalf("%s %+v", key, facts[key])
				}
			}
		}},
		{"unsupported-error", func(t *testing.T) {
			rep, out, _ := shortRun(t, "50", success)
			def := reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseDefault)
			if def.Status != mcpqual.StatusInconclusive || !strings.Contains(deref(def.Reason), mcpqual.ReasonUnverifiedEvent) {
				t.Fatalf("typed timeout without capability: %+v", def)
			}
			_, base := shortRepo(t, captureVersions["claude"], "linux/amd64")
			p, err := mcpqual.ProposePatch(rep, base, out)
			if err != nil {
				t.Fatal(err)
			}
			for _, fc := range p.Facts {
				if fc.Final.Status == catalog.Verified {
					t.Fatalf("%s.%s VERIFIED from an unsupported event", fc.Client, fc.Key)
				}
			}
		}},
		{"version-conflict", func(t *testing.T) {
			repo, base := shortRepo(t, "2.1.285 (Claude Code)", "linux/amd64")
			out := filepath.Join(realTemp(t), "copy")
			copyTree(t, lowerOut, out)
			os.RemoveAll(filepath.Join(out, "proposed"))
			os.Remove(filepath.Join(out, mcpqual.PatchFileName))
			if _, err := mcpqual.ProposePatch(lower, base, out); err == nil || contract.ExitCode(err) != 4 {
				t.Fatalf("version conflict: %v", err)
			}
			if _, err := os.Stat(filepath.Join(repo, mcpqual.EvidenceRoot)); err == nil {
				t.Fatal("evidence installed despite the conflict")
			}
		}},
		{"platform-conflict", func(t *testing.T) {
			_, base := shortRepo(t, captureVersions["claude"], "darwin/arm64")
			out := filepath.Join(realTemp(t), "copy")
			copyTree(t, lowerOut, out)
			os.RemoveAll(filepath.Join(out, "proposed"))
			os.Remove(filepath.Join(out, mcpqual.PatchFileName))
			p, err := mcpqual.ProposePatch(lower, base, out)
			if err != nil {
				t.Fatal(err)
			}
			for _, fc := range p.Facts {
				if fc.Key == "mcp_timeout" && (fc.Final.Status != catalog.Unverified || !strings.Contains(fc.Final.Value, "does not certify darwin/arm64")) {
					t.Fatalf("platform conflict published %+v", fc.Final)
				}
			}
		}},
	})
}

// FP-8: the guide's documented capture and confirmation commands, linked
// plans, owner gates, enrollment inventory and publication preflight
// agree with the executable interfaces and the index.
func TestMCPCaptureRunbook(t *testing.T) {
	t.Parallel()
	root := testkit.MustRepoRoot(t)
	guide := string(repoFile(t, mcpqual.SetupDocPath))
	start, end := strings.Index(guide, "### Decoder enrollment: setup capture\n"), strings.Index(guide, "### Short confirmation\n")
	if start < 0 || end < start {
		t.Fatal("the guide has no capture section before the short confirmation")
	}
	sec := guide[start:end]
	runInventory(t, 4, []string{"commands-and-plans", "owner-gates", "enrollment-inventory", "publication-preflight"}, []captureCase{
		{"commands-and-plans", func(t *testing.T) {
			m := regexp.MustCompile("(?s)```text\n(.*?)\n```").FindStringSubmatch(sec)
			if m == nil {
				t.Fatal("no capture command")
			}
			words, err := mcpqual.ShellWords(m[1])
			if err != nil || len(words) != 7 || words[1] != "capture" || !slices.Contains(words, "--allow-model-calls") {
				t.Fatalf("documented command %q", words)
			}
			q := newQualEnv(t)
			plans := filepath.Join(root, "internal", "mcpqual", "testdata", "plans")
			for _, id := range []string{"claude", "codex", "grok", "cursor"} {
				if !strings.Contains(sec, "](../internal/mcpqual/testdata/plans/"+id+"-short.json)") {
					t.Fatalf("%s template not linked", id)
				}
				// The documented argv reaches the template's owner placeholders,
				// before any launch.
				args := append([]string(nil), words[1:]...)
				args[2], args[4] = filepath.Join(plans, id+"-short.json"), filepath.Join(q.dir, "out-"+id)
				if r := runBin(t, qualBinary(t), q.dir, q.env(""), args...); r.code != 2 || !strings.Contains(r.stderr, "unfilled owner placeholder") {
					t.Fatalf("%s documented capture = %+v", id, r)
				}
				// A completed copy of the same template is a valid capture plan.
				b, _ := json.Marshal(q.capturePlan(nil, id))
				if _, err := mcpqual.ParseCapturePlan(b); err != nil {
					t.Fatalf("%s completed template: %v", id, err)
				}
			}
			if _, err := os.Stat(q.launches); err == nil {
				t.Fatal("a documented command launched a vendor")
			}
			if !strings.Contains(guide, "--plan /absolute/vendor-short.json --out /absolute/evidence-dir --allow-model-calls --publish-catalog /absolute/repo") {
				t.Fatal("the confirmation command is not documented")
			}
		}},
		{"owner-gates", func(t *testing.T) {
			for _, w := range []string{"Code gate A", "Owner capture gate", "Code gate B", "Owner short-confirmation gate", "Optional publication gate",
				"Real execution is never part of CI", "Claude must run in the owner's unsandboxed shell", "`!` is the UI escape, not a shell negation",
				"only when the owner has explicitly delegated it", "One capture plus at most three confirmation sessions per client", "Never add `--approve-mcps`",
				"never retried until green", "macOS timeout compatibility remains UNVERIFIED",
				// Design decoder-enrollment B1: the placement outside HOME and
				// work trees, the Cursor owner precheck and the B1/B2 gates.
				"a fresh absolute output directory outside `$HOME` and outside every git work tree", "never a destination under this repository's `design/` tree",
				"an out directory under HOME is rejected because `~/.grok/config.toml` lies on its ancestor chain", "`TMPDIR` itself is not proof of suitable placement",
				"Record the placement check outcome in the delivery notes", "their checked-in location is not their launch location",
				"`expected-blocked: cursor_approval_scope_unverified`", "exceeds 10,000 entries, 64 MiB in all or 8 MiB in one file",
				"A passing precheck never substitutes for the two runtime inventories", "Code gate B1", "Do not reuse completed plans from the failed captures",
				"Code gate B (B2): only after usable reviewed re-captures", "is not a discovery check",
				// Design decoder-enrollment B1.5: version pinning, no concurrent
				// Claude sessions, the inventory exclusions, the project
				// approval, the terminal observation and the macOS limit.
				"Code gate B1.5", "pin the executable and its version from capture through confirmation", "Disable or postpone auto-update",
				"executable SHA-256", "never an edited expected string", "with no other Claude Code session active",
				"neither kills them nor modifies or locks `~/.claude/.config.json`", "`JSON Parse error: Unexpected EOF`", "no stderr classifier and no retry",
				"`cursor-approval-v2`", "the contents only of exactly `~/.cursor/chats` and `~/.cursor/ai-tracking`", "is not detected",
				"computed `~/.cursor/projects/<workspace-project>` directory of the intended case path is absent", "`project_scoped`",
				"take fresh B1.5 captures into fresh output paths", "\"terminal observed; probe exit not observed\"", "which asserts no cause",
				"passing offline macOS tests is not vendor qualification",
				// Design decoder-enrollment B2: the fixture export and owner
				// gate, the scoped Cursor permission and the follow-up.
				"Mid-implementation handback", "`awaiting independent fixture verification and explicit owner approval`",
				"Independent verification and explicit owner acceptance", "Final offline acceptance", "#### Fixture export and owner review (B2)",
				"a raw bundle is never committed", "#### Cursor scoped permission (B2; per qualify case since B3)", "`" + mcpqual.CursorToolPermissionAdapter + "`",
				"`{\"permissions\":{\"allow\":[\"Mcp(probe:slow)\"],\"deny\":[]}}`", "never approval evidence", "Owner Cursor scoped-permission recapture",
				"Cursor follow-up", "A Linux fixture replays in macOS CI only as a parser check"} {
				if !strings.Contains(sec, w) {
					t.Fatalf("the capture section lacks %q", w)
				}
			}
		}},
		{"enrollment-inventory", func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(mcpqual.EnrollmentIndexPath)))
			if err != nil {
				t.Fatal(err)
			}
			idx, err := mcpqual.ParseEnrollmentIndex(b)
			if err != nil {
				t.Fatal(err)
			}
			synthetic := strings.Contains(guide, "mcpqual ships synthetic decoder fixtures only")
			switch {
			case len(idx.Entries) == 0 && !synthetic:
				t.Fatal("an empty index without the synthetic-only statement")
			case len(idx.Entries) > 0 && synthetic:
				t.Fatal("a stale synthetic-only claim beside enrolled versions")
			}
			for _, e := range idx.Entries {
				if !strings.Contains(guide, "](../"+e.Bundle) {
					t.Fatalf("enrolled %s is not linked", e.Fixture)
				}
			}
			if !strings.Contains(sec, "`tests/testdata/mcp-transcripts/index.json`, schema `mcpqual-enrollment-v1`") {
				t.Fatal("the index is not documented")
			}
		}},
		{"publication-preflight", func(t *testing.T) {
			for _, w := range []string{"Publication preflight", "initial invocation", "exits 4", "only retries an existing patch", "never publishes a macOS fact"} {
				if !strings.Contains(guide, w) {
					t.Fatalf("the guide lacks %q", w)
				}
			}
		}},
	})
	// Design catalog-version FP-5: the review then owner gate.
	t.Run("publication-review", func(t *testing.T) { publicationReview(t, guide) })
}

// FP-9: devcheck's native, test, benchmark and stress plans and the
// coverage manifest and CI documentation enforce the design with no
// live-vendor stage.
func TestMCPEnrollmentCIPolicy(t *testing.T) {
	t.Parallel()
	live := func(steps []devcheck.Step) string {
		for _, s := range steps {
			argv := strings.Join(s.Argv, " ")
			for _, bad := range []string{"capture", "--allow-model-calls", "qualify", "claude ", "codex ", "grok ", "cursor-agent"} {
				if strings.Contains(argv, bad) {
					return argv
				}
			}
		}
		return ""
	}
	nine := []string{"TestMCPCaptureInvocation", "TestMCPCaptureRecipes", "TestMCPCaptureEvidence", "TestMCPCaptureLifecycle", "TestMCPEnrollmentContract",
		"TestMCPEnrollmentReplay", "TestMCPEnrolledShortConfirmation", "TestMCPCaptureRunbook", "TestMCPEnrollmentCIPolicy"}
	// Slice B1's four parents (FP-10..FP-13) follow the nine.
	four := []string{"TestMCPCaptureProtocolNegotiation", "TestMCPCaptureCodexApproval", "TestMCPCaptureGrokRecipe", "TestMCPCaptureCursorTrust"}
	// Slice B1.5's three parents (FP-14..FP-16) follow the four.
	three := []string{"TestMCPProbeTerminalObservation", "TestMCPCaptureCursorProjectApproval", "TestMCPCaptureCursorInventoryPolicy"}
	// Slice B2's five parents (FP-17..FP-21) follow the three.
	five := []string{"TestMCPRealDecoderMappings", "TestMCPFixtureSanitization", "TestMCPRealEnrollment", "TestMCPCaptureCursorToolPermission", "TestMCPRealEnrollmentConfirmation"}
	// Slice B3's five parents (FP-22..FP-26) follow the B2 five.
	b3 := []string{"TestMCPCursorRealDecoder", "TestMCPCursorEnrollment", "TestMCPCursorQualifyPreparation", "TestMCPCursorWorkerResidue", "TestMCPCursorConfirmation"}
	runInventory(t, 4, []string{"native-and-test", "bench", "stress", "coverage-and-docs"}, []captureCase{
		{"native-and-test", func(t *testing.T) {
			req := devcheck.NativeRequiredTests()
			if len(req) != 429 || !slices.Equal(req[403:412], nine) || !slices.Equal(req[412:416], four) || !slices.Equal(req[416:419], three) || !slices.Equal(req[419:424], five) ||
				!slices.Equal(req[424:429], b3) ||
				req[402] != "TestMCPShortConfirmation" {
				t.Fatalf("native inventory %d %v", len(req), req[400:])
			}
			for _, goos := range []string{"linux", "darwin"} {
				if bad := live(devcheck.TestSteps(goos)); bad != "" {
					t.Fatalf("%s test step %s", goos, bad)
				}
			}
			steps, err := devcheck.NativeSteps("darwin")
			if err != nil || live(steps) != "" {
				t.Fatalf("native steps %v", err)
			}
		}},
		{"bench", func(t *testing.T) {
			want := "go test ./internal/mcpqual -run=^$ -bench=. -benchmem -benchtime=3x -count=1 -timeout=180s"
			bench := devcheck.BenchSteps()
			tail := devcheck.NativeBenchSteps()
			if len(bench) != 13 || strings.Join(devcheck.MCPQualBenchStep().Argv, " ") != want || !slices.ContainsFunc(bench, func(s devcheck.Step) bool { return strings.Join(s.Argv, " ") == want }) ||
				len(tail) != 5 || strings.Join(tail[4].Argv, " ") != want || live(bench) != "" {
				t.Fatalf("bench %d tail %v", len(bench), tail)
			}
		}},
		{"stress", func(t *testing.T) {
			for _, goos := range []string{"linux", "darwin"} {
				steps, err := devcheck.StressSteps(goos)
				if err != nil {
					t.Fatal(err)
				}
				var mcpqualArgv []string
				for _, s := range steps {
					argv := strings.Join(s.Argv, " ")
					if strings.Contains(argv, "./internal/mcpqual") {
						mcpqualArgv = append(mcpqualArgv, argv)
					}
					if strings.Contains(argv, "MCPCapture") || strings.Contains(argv, "MCPEnroll") || strings.Contains(argv, "container-e2e") && strings.Contains(argv, "--count=20") {
						t.Fatalf("%s stress step %s", goos, argv)
					}
				}
				var want []string
				for _, n := range []string{"1", "2", "4"} {
					want = append(want, "go test -race -count=20 -cpu="+n+" -timeout=6m ./internal/mcpqual")
				}
				if !slices.Equal(mcpqualArgv, want) || live(steps) != "" {
					t.Fatalf("%s mcpqual stress %q", goos, mcpqualArgv)
				}
			}
		}},
		{"coverage-and-docs", func(t *testing.T) {
			listed := map[string]bool{}
			for _, e := range devcheck.WorkspaceCoverageManifest {
				if len(e.Ranges) == 0 && e.Group == devcheck.GroupChanged {
					listed[strings.TrimPrefix(e.File, "github.com/wedevwork/callsheet/")] = true
				}
			}
			for _, f := range []string{"cli", "plan", "session", "runner", "proc", "report", "facts", "decode", "capture", "capture_manifest", "enrollment", "redact"} {
				if !listed["internal/mcpqual/"+f+".go"] {
					t.Fatalf("internal/mcpqual/%s.go is not a whole-file changed entry", f)
				}
			}
			for _, f := range []string{"internal/mcpqual/procexec/exec_unix.go", "cmd/mcpqual/main.go", "internal/devcheck/native.go", "internal/devcheck/devcheck.go"} {
				if !listed[f] {
					t.Fatalf("%s is not a whole-file changed entry", f)
				}
			}
			for _, f := range []string{"probe", "events", "capture_approval", "capture_approval_unix"} {
				if !listed["internal/mcpqual/"+f+".go"] {
					t.Fatalf("B1 file internal/mcpqual/%s.go is not a whole-file changed entry", f)
				}
			}
			if !listed["internal/mcpqual/probe_observation.go"] {
				t.Fatal("B1.5 file internal/mcpqual/probe_observation.go is not a whole-file changed entry")
			}
			for _, f := range []string{"internal/mcpqual/decode_real.go", "internal/mcpqual/fixture_export.go", "cmd/mcpfixture-export/main.go",
				"internal/mcpqual/cursor_preparation.go", "internal/mcpqual/cursor_residue.go"} {
				if !listed[f] {
					t.Fatalf("B2 or B3 file %s is not a whole-file changed entry", f)
				}
			}
			doc := string(repoFile(t, "docs/ci.md"))
			for _, w := range append(append(append(append(append([]string{"Decoder enrollment: baseline is main run 37523901881 (ff8058f)", "9 more names, 412 in all", "(9 ordinary calls)", "CI stays 18 jobs",
				"4 more names, 416 in all", "3 more names, 419 in all", "5 more names, 424 in all", "5 more names, 429 in all"}, nine...), four...), three...), five...), b3...) {
				if !strings.Contains(strings.Join(strings.Fields(doc), " "), w) {
					t.Fatalf("docs/ci.md lacks %q", w)
				}
			}
		}},
	})
	// Design catalog-version FP-8 and FP-9.
	t.Run("catalog-version-policy", func(t *testing.T) { catalogVersionPolicy(t) })
	t.Run("catalog-version-ci", func(t *testing.T) { catalogVersionCI(t, live) })
}

// Decoder enrollment, slice B1 (design decoder-enrollment B1): one
// function parent per new FP (FP-10..FP-13), each with its literal case
// inventory. FP-10, FP-11 and FP-13 run the built mcpqual (or, for the
// injected inventory bounds and cleanup faults, the production runner in
// process) with the fake vendor and the real probe; FP-12 uses the
// in-process runner with its placement-filesystem view (DW8/DW10) and the
// same real fake-vendor and probe processes.

// FP-10: the probe answers 2025-06-18 to the same, a newer and an older
// requested version and records both in its raw events; a client that
// cannot use the answer disconnects without a receipt and stays partial.
func TestMCPCaptureProtocolNegotiation(t *testing.T) {
	t.Parallel()
	negotiate := func(t *testing.T, requested string, decline bool) {
		q := newQualEnv(t)
		settings := map[string]string{"PROTOCOL": requested}
		if decline {
			settings["DECLINE"] = "1"
		}
		out := filepath.Join(realTemp(t), "out")
		r := q.capture(q.capturePlan(map[string]map[string]string{"claude": settings}, "claude"), out, nil, "--allow-model-calls")
		b := bundle(t, out)
		c := captureClient(t, b.Manifest, "claude")
		server := b.Files[mcpqual.ClientFile("claude", mcpqual.FileServerEvents)]
		evs, intact, err := mcpqual.ParseProbeEvents(server)
		if err != nil || !intact || len(evs) < 2 || evs[1].Kind != mcpqual.EvInitialize || *evs[1].RequestedProtocolVersion != requested ||
			*evs[1].SelectedProtocolVersion != mcpqual.ProtocolVersion {
			t.Fatalf("server events %v %s", err, server)
		}
		q2, _ := json.Marshal(requested)
		stderr := string(b.Files[mcpqual.ClientFile("claude", mcpqual.FileVendorStderr)])
		if !bytes.Contains(server, []byte(`"requested_protocol_version":`+string(q2)+`,"selected_protocol_version":"2025-06-18"`)) ||
			!strings.Contains(stderr, "negotiated protocol 2025-06-18 for requested "+requested) || *c.Probe.ClientName != "claude-cli" {
			t.Fatalf("raw evidence %s / %q", server, stderr)
		}
		log := q.launchLog()
		switch {
		case decline && (r.code != 5 || c.State != mcpqual.CapturePartial || deref(c.Reason) != mcpqual.ReasonProbeNotObserved || c.Probe.Receipts != 0 ||
			slices.ContainsFunc(evs, func(ev mcpqual.ProbeEvent) bool { return ev.Kind == mcpqual.EvReceipt }) || len(log["session"]) != 1):
			t.Fatalf("declined: %+v %s", r, deref(c.Reason))
		case !decline && (r.code != 0 || c.State != mcpqual.CaptureComplete || c.Probe.Receipts != 1 || !c.Probe.Completed || len(log["session"]) != 1):
			t.Fatalf("negotiated %s: %+v %s", requested, r, deref(c.Reason))
		}
		q.groupsGone()
	}
	runInventory(t, 4, []string{"same-version", "newer-version", "older-version", "client-declines"}, []captureCase{
		{"same-version", func(t *testing.T) { negotiate(t, mcpqual.ProtocolVersion, false) }},
		{"newer-version", func(t *testing.T) { negotiate(t, "2099-01-01", false) }},
		{"older-version", func(t *testing.T) { negotiate(t, "2024-11-05", false) }},
		{"client-declines", func(t *testing.T) { negotiate(t, "2099-01-01", true) }},
	})
}

// blanketFlags are approval or trust widenings no B1 recipe may carry.
var blanketFlags = []string{"--full-auto", "--dangerously", "--yolo", "--approve-mcps", "--ask-for-approval", "approval_policy", "--sandbox", "--force"}

func noBlanket(t *testing.T, argv []string) {
	t.Helper()
	for _, a := range argv {
		for _, bad := range blanketFlags {
			if strings.Contains(a, bad) {
				t.Fatalf("a blanket option %q in %q", a, argv)
			}
		}
	}
}

// FP-11: Codex gets exactly the invocation-only probe.slow approval; the
// fake Codex checks its exact invocation and configuration and calls the
// real probe only then; a managed denial and an unsupported setting stay
// partial with exactly one session.
func TestMCPCaptureCodexApproval(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, outcome string) (*qualEnv, mcpqual.CaptureClient, *mcpqual.CaptureBundle, result, string) {
		q := newQualEnv(t)
		out := filepath.Join(realTemp(t), "out")
		r := q.capture(q.capturePlan(map[string]map[string]string{"codex": {"RECIPE": "codex", "OUTCOME": outcome}}, "codex"), out, nil, "--allow-model-calls")
		b := bundle(t, out)
		if log := q.launchLog(); len(log["session"]) != 1 {
			t.Fatalf("%d sessions (no fallback or retry)", len(log["session"]))
		}
		return q, captureClient(t, b.Manifest, "codex"), b, r, out
	}
	runInventory(t, 3, []string{"scoped-success", "approval-denied", "unsupported-config"}, []captureCase{
		{"scoped-success", func(t *testing.T) {
			q, c, b, r, out := run(t, "")
			recs := q.argvLog()
			ws := filepath.Join(out, ".work", "codex-capture-setup")
			var overrides []string
			for i, a := range recs[0].Argv {
				if a == "-c" {
					overrides = append(overrides, recs[0].Argv[i+1])
				}
			}
			srv := strings.TrimSuffix(strings.TrimPrefix(overrides[0], `mcp_servers.probe.command="`), `"`)
			want := []string{`mcp_servers.probe.command="` + srv + `"`, `mcp_servers.probe.args=["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]`,
				`mcp_servers.probe.enabled_tools=["slow"]`, `mcp_servers.probe.tools.slow.approval_mode="approve"`}
			if r.code != 0 || c.State != mcpqual.CaptureComplete || !slices.Equal(overrides, want) || c.Probe.Receipts != 1 || mustReal(t, srv) != mustReal(t, qualBinary(t)) {
				t.Fatalf("scoped: %+v %q %q", r, overrides, deref(c.Reason))
			}
			noBlanket(t, recs[0].Argv)
			if n := strings.Count(strings.Join(recs[0].Argv, "\x00"), "approval_mode"); n != 1 {
				t.Fatalf("%d approval settings", n)
			}
			// Provenance: the labeled argv and the configuration snapshot.
			cfg := string(b.Files[mcpqual.ClientFile("codex", mcpqual.FileConfig)])
			if !slices.Contains(c.Argv, `mcp_servers.probe.enabled_tools=["slow"]`) || !slices.Contains(c.Argv, `mcp_servers.probe.tools.slow.approval_mode="approve"`) ||
				!strings.HasSuffix(cfg, "enabled_tools = [\"slow\"]\n[mcp_servers.probe.tools.slow]\napproval_mode = \"approve\"\n") ||
				deref(c.Config.Prerequisite) != "Invocation-only approval for probe.slow; no server-wide approval or timeout override." {
				t.Fatalf("provenance %q %q", c.Argv, cfg)
			}
			q.groupsGone()
		}},
		{"approval-denied", func(t *testing.T) {
			q, c, b, r, _ := run(t, "approval-denied")
			if r.code != 5 || deref(c.Reason) != mcpqual.ReasonProbeNotObserved || len(b.Files[mcpqual.ClientFile("codex", mcpqual.FileServerEvents)]) != 0 ||
				!strings.Contains(string(b.Files[mcpqual.ClientFile("codex", mcpqual.FileVendorEvents)]), "rejected by approval policy") {
				t.Fatalf("denied: %+v %s", r, deref(c.Reason))
			}
			noBlanket(t, q.argvLog()[0].Argv)
			q.groupsGone()
		}},
		{"unsupported-config", func(t *testing.T) {
			q, c, b, r, _ := run(t, "unsupported-config")
			if r.code != 5 || deref(c.Reason) != mcpqual.ReasonProbeNotObserved || *c.Session.Exit != 1 || len(b.Files[mcpqual.ClientFile("codex", mcpqual.FileServerEvents)]) != 0 ||
				!strings.Contains(string(b.Files[mcpqual.ClientFile("codex", mcpqual.FileVendorStderr)]), "mcp_servers.probe.tools.slow.approval_mode") {
				t.Fatalf("unsupported: %+v %s", r, deref(c.Reason))
			}
			q.groupsGone()
		}},
	})
}

// fixtureView is the FP-12 placement-filesystem view (design
// decoder-enrollment B1, DW8 and DW10): at or below the function fixture
// root everything is the real filesystem, including every marker a case
// plants; strictly above it every directory is an ordinary clean one with
// no entry, up to /. It changes observations only: the production gate
// still walks to the root and applies every rule.
type fixtureView struct{ root string }

func (v fixtureView) inside(p string) bool { return p == v.root || strings.HasPrefix(p, v.root+"/") }

func (v fixtureView) above(p string) bool {
	return p != v.root && (p == "/" || strings.HasPrefix(v.root, p+"/"))
}

type viewDir string

func (d viewDir) Name() string       { return string(d) }
func (d viewDir) Size() int64        { return 0 }
func (d viewDir) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (d viewDir) ModTime() time.Time { return time.Time{} }
func (d viewDir) IsDir() bool        { return true }
func (d viewDir) Sys() any           { return nil }

func (v fixtureView) Lstat(p string) (os.FileInfo, error) {
	switch p = filepath.Clean(p); {
	case v.inside(p):
		return os.Lstat(p)
	case v.above(p):
		return viewDir(filepath.Base(p)), nil
	}
	return nil, &os.PathError{Op: "lstat", Path: p, Err: os.ErrNotExist}
}

func (v fixtureView) EvalSymlinks(p string) (string, error) {
	switch p = filepath.Clean(p); {
	case v.inside(p):
		return filepath.EvalSymlinks(p)
	case v.above(p):
		return p, nil
	}
	return "", &os.PathError{Op: "evalsymlinks", Path: p, Err: os.ErrNotExist}
}

// grokFixture is one FP-12 fixture: its root (the view boundary) holding
// the output directory and a fixture-owned HOME separate from it.
type grokFixture struct {
	q               *qualEnv
	root, out, home string
}

func newGrokFixture(t *testing.T, root string) *grokFixture {
	q := newQualEnv(t)
	f := &grokFixture{q: q, root: root, out: filepath.Join(root, "out"), home: mkdir(t, filepath.Join(root, "home"))}
	return f
}

// grokSecret is the planted credential of the Grok stream.
const grokSecret = "sk-grok-planted-0123456789abcdef"

// plan is the shipped trusted Grok template with the fake's B1 checks and
// its streaming-json output.
func (f *grokFixture) plan(settings map[string]string) map[string]any {
	s := map[string]string{"RECIPE": "grok", "FORMAT": "grok-stream", "EXPECT_HOME": f.home, "SECRET": grokSecret}
	for k, v := range settings {
		s[k] = v
	}
	return f.q.capturePlan(map[string]map[string]string{"grok": s}, "grok")
}

// capture runs the production runner in process with the fixture's view,
// a controlled environment and its HOME.
func (f *grokFixture) capture(t *testing.T, plan map[string]any, mutate func(c *mcpqual.CaptureRunner)) *mcpqual.CaptureManifest {
	return inProcessRun(t, f.q, plan, f.out, func(c *mcpqual.CaptureRunner) {
		c.BaseEnv = []string{"PATH=" + f.q.trapDir, "HOME=" + f.home}
		c.Home, c.GrokPlacementFS = f.home, fixtureView{f.root}
		if mutate != nil {
			mutate(c)
		}
	})
}

// leader is a separately owned, pre-existing fake Grok leader (its own
// session and process group, started before and stopped after capture by
// its test owner).
type leader struct {
	cmd  *exec.Cmd
	dir  string
	done chan struct{}
}

func startLeader(t *testing.T, f *grokFixture) *leader {
	dir := mkdir(t, filepath.Join(f.home, ".grok", "leader"))
	for _, n := range []string{"req.fifo", "ack.fifo"} {
		if err := syscall.Mkfifo(filepath.Join(dir, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ready, wait := f.q.readyFIFO()
	cmd := exec.Command(fakeVendorBinary(t))
	cmd.Env = []string{"FAKE_VENDOR_MODE=leader", "FAKE_VENDOR_LEADER=" + dir, "FAKE_VENDOR_READY=" + ready}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	l := &leader{cmd: cmd, dir: dir, done: make(chan struct{})}
	go func() { cmd.Wait(); close(l.done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-l.done })
	if !strings.HasPrefix(wait(), "ready ") {
		t.Fatal("the leader is not ready")
	}
	return l
}

// serving proves the leader still answers (a bounded request/answer
// handshake), then its owner stops it and proves it gone.
func (l *leader) servingThenStop(t *testing.T) {
	t.Helper()
	answered := make(chan error, 1)
	go func() {
		err := os.WriteFile(filepath.Join(l.dir, "req.fifo"), []byte("owner check\n"), 0o600)
		if err == nil {
			var ack []byte
			ack, err = os.ReadFile(filepath.Join(l.dir, "ack.fifo"))
			if err == nil && string(ack) != "ok owner check\n" {
				err = io.ErrUnexpectedEOF
			}
		}
		answered <- err
	}()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the leader no longer serves: %v", err)
		}
	case <-l.done:
		t.Fatal("the owner's leader was ended by the capture")
	case <-time.After(mcpWait):
		t.Fatal("the leader did not answer")
	}
	l.cmd.Process.Kill()
	<-l.done
	if err := processgroup.WaitGone(processgroup.SysSignaler{}, processgroup.RealClock{}, 5*time.Second, 10*time.Millisecond, l.cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
}

// FP-12: the trusted Grok recipe (global --trust, the generated
// .grok/config.toml, unchanged HOME/GROK_HOME, probe__slow, streaming-json)
// against the real probe, its placement gate, its redaction and its
// cleanup, with a separately owned leader left to its owner.
func TestMCPCaptureGrokRecipe(t *testing.T) {
	t.Parallel()
	const m = "fake-model-1"
	success := func(t *testing.T, root string) {
		f := newGrokFixture(t, root)
		l := startLeader(t, f)
		man := f.capture(t, f.plan(map[string]string{"LEADER": l.dir, "MODE": "parent-exits-first"}), nil)
		c := captureClient(t, man, "grok")
		b := bundle(t, f.out)
		recs := f.q.argvLog()
		ws := filepath.Join(f.out, ".work", "grok-capture-setup")
		prompt := mcpqual.PromptForClient("grok", "grok-capture-setup")
		if c.State != mcpqual.CaptureComplete || len(recs) != 1 || !slices.Equal(recs[0].Argv, []string{"--trust", "-p", prompt, "--output-format", "streaming-json", "--cwd", ws, "-m", m}) ||
			filepath.Clean(recs[0].Cwd) != ws || !strings.HasSuffix(recs[0].ConfigPath, "/.grok/config.toml") || !strings.Contains(prompt, "MCP tool probe__slow exactly once") {
			t.Fatalf("trusted session %s %+v", deref(c.Reason), recs)
		}
		// Argv, configuration and provenance together.
		cfg := string(b.Files[mcpqual.ClientFile("grok", mcpqual.FileConfig)])
		if !slices.Equal(c.Argv, []string{"--trust", "-p", prompt, "--output-format", "streaming-json", "--cwd", "<workspace>", "-m", m}) || c.Config.Path != ".grok/config.toml" ||
			cfg != "[mcp_servers.probe]\ncommand = \"<server>\"\nargs = [\"serve\", \"--case-file\", \"<workspace>/case.json\", \"--events\", \"<workspace>/server-events.jsonl\"]\n" ||
			deref(c.Config.Prerequisite) != "Trust only the generated workspace; use normal Grok login; project probe config, probe__slow and streaming-json; no timeout override." ||
			c.Probe.Receipts != 1 || !c.Probe.Completed {
			t.Fatalf("provenance %q %q", c.Argv, cfg)
		}
		// The stream: secrets redacted structured and embedded, usage kept,
		// a fixed point; no credential or home in any file.
		vendor := b.Files[mcpqual.ClientFile("grok", mcpqual.FileVendorEvents)]
		tr, err := mcpqual.ReplayTranscript(vendor)
		if err != nil || len(tr.Lines) != 5 {
			t.Fatalf("stream %v %d", err, len(tr.Lines))
		}
		var decoded strings.Builder
		for _, line := range tr.Lines {
			decoded.Write(line.Data)
		}
		for _, w := range []string{`"api_key":"[REDACTED]"`, `\"token\":\"[REDACTED]\"`, `"input_tokens":812`, `"output_tokens":45`, `"toolName":"probe__slow"`, `\"nonce\":\"nonceflood\"`} {
			if !strings.Contains(decoded.String(), w) {
				t.Fatalf("decoded stream lacks %s", w)
			}
		}
		for p, data := range b.Files {
			if bytes.Contains(data, []byte(grokSecret)) || bytes.Contains(data, []byte(f.home)) {
				t.Fatalf("%s leaks", p)
			}
			if err := mcpqual.RedactionFixedPoint(p, data, mcpqual.NewCaptureRedactor(nil, nil)); err != nil {
				t.Fatal(err)
			}
		}
		// The launcher exited first; its resistant descendant was reaped and
		// is proven gone, while the owner's leader, which served the session,
		// still serves until its owner stops it.
		if !c.Session.Cleanup.LeaderExitedFirst || !c.Session.Cleanup.GroupGone || len(f.q.launchLog()["descendant"]) != 1 {
			t.Fatalf("cleanup %+v", c.Session.Cleanup)
		}
		f.q.groupsGone()
		l.servingThenStop(t)
	}
	denied := func(t *testing.T, f *grokFixture, want string) {
		t.Helper()
		man := f.capture(t, f.plan(nil), nil)
		c := captureClient(t, man, "grok")
		if !strings.HasPrefix(deref(c.Reason), mcpqual.ReasonGrokPlacement+": "+want) || man.ExitCode() != 5 || c.Version.State != mcpqual.StageNotRun {
			t.Fatalf("placement: %s", deref(c.Reason))
		}
		if _, err := os.Stat(f.q.launches); err == nil {
			t.Fatal("grok launched despite the placement")
		}
	}
	runInventory(t, 4, []string{"trusted-project-stream", "trust-rejected", "malformed-stream", "cleanup-failure"}, []captureCase{
		{"trusted-project-stream", func(t *testing.T) {
			success(t, filepath.Join(realTemp(t), "fixture"))
			// The hostile developer layout: the fixture root under a directory
			// holding .grok/config.toml and .git (a TMPDIR under a HOME with
			// normal Grok login). Above the boundary: the same success runs.
			hostile := realTemp(t)
			mkdir(t, filepath.Join(hostile, ".git"))
			mkdir(t, filepath.Join(hostile, ".grok"))
			os.WriteFile(filepath.Join(hostile, ".grok", "config.toml"), []byte("[mcp_servers.other]\n"), 0o600)
			success(t, filepath.Join(hostile, "fixture"))
			// The same markers at or below the boundary refuse the capture
			// before any Grok launch.
			for name, plant := range map[string]func(root string){
				"config-at-root": func(root string) {
					mkdir(t, filepath.Join(root, ".grok"))
					os.WriteFile(filepath.Join(root, ".grok", "config.toml"), nil, 0o600)
				},
				"git-at-root": func(root string) { mkdir(t, filepath.Join(root, ".git")) },
			} {
				root := mkdir(t, filepath.Join(hostile, "fixture-"+name))
				plant(root)
				denied(t, newGrokFixture(t, root), "lexical chain")
			}
		}},
		{"trust-rejected", func(t *testing.T) {
			// The canonical validator: every other --trust form is refused,
			// exit 2, before any launch.
			for name, mutate := range map[string]func(c map[string]any){
				"trust-true":  func(c map[string]any) { c["session"].(map[string]any)["argv"].([]any)[0] = "--trust=true" },
				"trust-false": func(c map[string]any) { c["session"].(map[string]any)["argv"].([]any)[0] = "--trust=false" },
				"trust-empty": func(c map[string]any) { c["session"].(map[string]any)["argv"].([]any)[0] = "--trust=" },
				"appended": func(c map[string]any) {
					c["config"].(map[string]any)["default"].(map[string]any)["argv"] = []string{"--trust"}
				},
				"grok-home": func(c map[string]any) { c["env"].(map[string]string)["GROK_HOME"] = "/elsewhere" },
			} {
				q := newQualEnv(t)
				plan := q.capturePlan(nil, "grok")
				mutate(plan["clients"].([]any)[0].(map[string]any))
				out := filepath.Join(realTemp(t), "out")
				r := q.capture(plan, out, nil, "--allow-model-calls")
				if r.code != 2 || !strings.Contains(r.stderr, "grok trusted recipe must target only the generated workspace") {
					t.Fatalf("%s: %+v", name, r)
				}
				if _, err := os.Stat(q.launches); err == nil {
					t.Fatalf("%s launched", name)
				}
				if _, err := os.Stat(out); err == nil {
					t.Fatalf("%s created its output", name)
				}
			}
			// Placement-denied variants inside the fixture: a linked worktree
			// marker, an ancestor configuration and a symlinked out whose
			// target is in a work tree.
			root := realTemp(t)
			f := newGrokFixture(t, root)
			os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o600)
			denied(t, f, "lexical chain: a .git entry 1 levels up")
			root = realTemp(t)
			nest := mkdir(t, filepath.Join(root, "nest"))
			mkdir(t, filepath.Join(root, ".grok"))
			os.WriteFile(filepath.Join(root, ".grok", "config.toml"), nil, 0o600)
			f = newGrokFixture(t, root)
			f.out = filepath.Join(nest, "out")
			denied(t, f, "lexical chain: a .grok/config.toml entry 2 levels up")
			root = realTemp(t)
			mkdir(t, filepath.Join(root, "repo", ".git"))
			mkdir(t, filepath.Join(root, "repo", "sub"))
			os.Symlink(filepath.Join(root, "repo", "sub"), filepath.Join(root, "alias"))
			f = newGrokFixture(t, root)
			f.out = filepath.Join(root, "alias", "out")
			denied(t, f, "resolved chain: a .git entry 2 levels up")
			// A vendor that rejects the trust request: partial, one session,
			// no retry or bypass.
			f = newGrokFixture(t, filepath.Join(realTemp(t), "fixture"))
			man := f.capture(t, f.plan(map[string]string{"OUTCOME": "trust-rejected"}), nil)
			c := captureClient(t, man, "grok")
			if deref(c.Reason) != mcpqual.ReasonProbeNotObserved || *c.Session.Exit != 2 || len(f.q.launchLog()["session"]) != 1 || man.ExitCode() != 5 {
				t.Fatalf("vendor trust rejection: %s", deref(c.Reason))
			}
		}},
		{"malformed-stream", func(t *testing.T) {
			f := newGrokFixture(t, filepath.Join(realTemp(t), "fixture"))
			man := f.capture(t, f.plan(map[string]string{"MALFORMED": "1"}), nil)
			c := captureClient(t, man, "grok")
			b := bundle(t, f.out)
			vendor := b.Files[mcpqual.ClientFile("grok", mcpqual.FileVendorEvents)]
			if deref(c.Reason) != mcpqual.ReasonEvidenceOmitted || streamOf(t, c, mcpqual.FileVendorEvents).OmittedLines != 1 || bytes.Contains(vendor, []byte(grokSecret)) {
				t.Fatalf("malformed: %s %+v", deref(c.Reason), c.Streams)
			}
			// The omission refuses enrollment: the file is no fixed point.
			if err := mcpqual.RedactionFixedPoint(mcpqual.ClientFile("grok", mcpqual.FileVendorEvents), vendor, mcpqual.NewCaptureRedactor(nil, nil)); err == nil {
				t.Fatal("an omitted stream is a fixed point")
			}
		}},
		{"cleanup-failure", func(t *testing.T) {
			// A resistant session (watchdog, TERM ignored, KILL) whose cleanup
			// is reported failed: partial, never complete; the run's group is
			// still gone; the owner's leader is untouched.
			f := newGrokFixture(t, filepath.Join(realTemp(t), "fixture"))
			l := startLeader(t, f)
			plan := f.plan(map[string]string{"MODE": "resistant", "LEADER": l.dir})
			plan["limits"] = map[string]any{"max_sessions_per_client": 1, "max_case_ms": 800, "max_client_ms": 60000}
			man := f.capture(t, plan, func(c *mcpqual.CaptureRunner) {
				c.Reaper = &faultyReaper{inner: c.Reaper, fail: 3}
			})
			c := captureClient(t, man, "grok")
			if deref(c.Reason) != mcpqual.ReasonCleanupFailed || man.Cleanup.OK || man.State == mcpqual.CaptureComplete || !c.Session.Watchdog || !c.Session.Cleanup.KillSent {
				t.Fatalf("cleanup failure: %s %+v", deref(c.Reason), man.Cleanup)
			}
			f.q.groupsGone()
			l.servingThenStop(t)
		}},
	})
}

// faultyReaper reaps with the real reaper, then reports its fail-th reap
// failed (an injected cleanup fault after real cleanup).
type faultyReaper struct {
	inner mcpqual.Reaper
	fail  int
	n     int
}

func (r *faultyReaper) Reap(p mcpqual.Proc) mcpqual.CaseCleanup {
	cc := r.inner.Reap(p)
	if r.n++; r.n == r.fail {
		cc.Error = sptrF("cleanup: injected")
	}
	return cc
}

func sptrF(s string) *string { return &s }

// cursorPlan is the shipped trusted Cursor template with the fake's B1
// session checks (the workspace, --trust and an approval in place before
// the session) and the enable behavior.
func cursorPlan(q *qualEnv, settings map[string]string, ids ...string) map[string]any {
	s := map[string]string{"RECIPE": "cursor"}
	for k, v := range settings {
		s[k] = v
	}
	all := map[string]map[string]string{"cursor": s}
	return q.capturePlan(all, append([]string{"cursor"}, ids...)...)
}

// ownerCursor plants the owner's existing .cursor state (with a credential
// that must never leave it).
func ownerCursor(t *testing.T, q *qualEnv) {
	mkdir(t, filepath.Join(q.home, ".cursor"))
	os.WriteFile(filepath.Join(q.home, ".cursor", "cli-config.json"), []byte(`{"accessToken":"cursor-owner-secret-1"}`), 0o600)
}

// FP-13: Cursor trusts exactly the generated workspace; its one approval
// preparation must show workspace-only changes before the model session;
// every other observation stops before it, with no rollback or retry.
func TestMCPCaptureCursorTrust(t *testing.T) {
	t.Parallel()
	var lastOut string
	run := func(t *testing.T, settings map[string]string) (*qualEnv, mcpqual.CaptureClient, *mcpqual.CaptureBundle, result) {
		q := newQualEnv(t)
		ownerCursor(t, q)
		out := filepath.Join(realTemp(t), "out")
		r := q.capture(cursorPlan(q, settings), out, nil, "--allow-model-calls")
		b := bundle(t, out)
		lastOut = out
		return q, captureClient(t, b.Manifest, "cursor"), b, r
	}
	stopped := func(t *testing.T, q *qualEnv, c mcpqual.CaptureClient, r result, reason, scope string, enables int) {
		t.Helper()
		log := q.launchLog()
		if r.code != 5 || !strings.HasPrefix(deref(c.Reason), reason) || c.Approval == nil || c.Approval.Scope != scope || len(log["enable"]) != enables ||
			len(log["session"]) != 0 || c.Session.State == mcpqual.StageRan {
			t.Fatalf("stopped: %+v %s %+v %v", r, deref(c.Reason), c.Approval, log)
		}
		q.groupsGone()
	}
	runInventory(t, 7, []string{"scoped-approval", "scope-rejected", "enable-failed", "outside-change", "scope-unverifiable", "mcp-denied", "cleanup-failure"}, []captureCase{
		{"scoped-approval", func(t *testing.T) {
			q, c, b, r := run(t, nil)
			out := lastOut
			a := c.Approval
			log := q.launchLog()
			recs := q.argvLog()
			ws := filepath.Join(out, ".work", "cursor-capture-setup")
			if r.code != 0 || c.State != mcpqual.CaptureComplete || len(log["enable"]) != 1 || len(log["session"]) != 1 || a == nil ||
				!slices.Equal(a.Argv, []string{"mcp", "enable", "probe"}) || a.Cwd != "<workspace>" || a.Stage.State != mcpqual.StageRan || *a.Stage.Exit != 0 ||
				!a.Stage.Cleanup.GroupGone || a.Scope != mcpqual.ScopeWorkspaceOnly || a.Reason != nil || !a.InventoryComplete ||
				!slices.Equal(a.Changes, []mcpqual.CaptureApprovalChange{{Path: "<workspace>/.cursor/approved-servers.json", Kind: "added"}}) {
				t.Fatalf("scoped: %+v %s %+v %v", r, deref(c.Reason), a, log)
			}
			// The session: the generated workspace, explicitly trusted, after
			// the approval (the fake refuses otherwise).
			if !slices.Equal(recs[0].Argv[len(recs[0].Argv)-3:], []string{"--workspace", ws, "--trust"}) || filepath.Clean(recs[0].Cwd) != ws {
				t.Fatalf("session argv %q", recs[0].Argv)
			}
			// Stage streams and hashes; no owner data, digest or content.
			if string(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileApprovalStdout)]) != "probe enabled\n" ||
				len(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileApprovalStderr)]) != 0 || len(b.Manifest.Files) != 11 {
				t.Fatalf("approval evidence %v", b.Manifest.Files)
			}
			for p, data := range map[string][]byte{"manifest": b.ManifestBytes, "config": b.Files[mcpqual.ClientFile("cursor", mcpqual.FileConfig)]} {
				if bytes.Contains(data, []byte("cursor-owner-secret-1")) || bytes.Contains(data, []byte("cli-config")) || bytes.Contains(data, []byte(q.home)) {
					t.Fatalf("%s exports owner data", p)
				}
			}
			if deref(c.Config.Prerequisite) != "Trust only the generated workspace; capture runs mcp enable probe there and verifies workspace-only file changes before the model session; never --approve-mcps." {
				t.Fatalf("prerequisite %q", deref(c.Config.Prerequisite))
			}
			noBlanket(t, recs[0].Argv)
			// Design decoder-enrollment B2 (FP-13/FP-20): for the pinned
			// adapter (the recorded platform) the fixed permission file is in
			// the workspace's initial state before the enable, so it is never
			// one of the changes; the only change stays the approval file.
			if b.Manifest.OS+"/"+b.Manifest.Arch == "linux/amd64" &&
				(c.ToolPermission == nil || c.ToolPermission.State != mcpqual.PermissionVerified || recs[0].Permission != "600 "+mcpqual.CursorToolPermissionContent) {
				t.Fatalf("pinned adapter permission %+v seen %q", c.ToolPermission, recs[0].Permission)
			}
			// Older bundles without the record stay valid; an inconsistent
			// new record is rejected.
			if _, err := mcpqual.ValidateCaptureBundle(os.DirFS(sharedCaptureKit(t).out), "."); err != nil {
				t.Fatal(err)
			}
			d := filepath.Join(realTemp(t), "copy")
			for name, edit := range map[string][2]string{
				"scope":   {`"scope": "workspace_only"`, `"scope": "outside_workspace"`},
				"change":  {`"path": "<workspace>/.cursor/approved-servers.json"`, `"path": "<home>/.cursor/approved-servers.json"`},
				"unknown": {`"scope": "workspace_only"`, `"scope": "workspace_only", "digest": "x"`},
			} {
				os.RemoveAll(d)
				copyTree(t, out, d)
				rewrite(t, filepath.Join(d, "manifest.json"), edit[0], edit[1])
				if _, err := mcpqual.ValidateCaptureBundle(os.DirFS(d), "."); err == nil {
					t.Fatalf("%s: an inconsistent approval record passed", name)
				}
			}
			q.groupsGone()
		}},
		{"scope-rejected", func(t *testing.T) {
			for name, mutate := range map[string]func(argv []any) []any{
				"trust-true":      func(argv []any) []any { argv[len(argv)-1] = "--trust=true"; return argv },
				"trust-false":     func(argv []any) []any { argv[len(argv)-1] = "--trust=false"; return argv },
				"saved-name":      func(argv []any) []any { argv[len(argv)-2] = "my-workspace"; return argv },
				"extra-workspace": func(argv []any) []any { return append(argv, "--workspace", "/") },
				"approve-mcps":    func(argv []any) []any { return append(argv, "--approve-mcps") },
			} {
				q := newQualEnv(t)
				plan := cursorPlan(q, nil)
				session := plan["clients"].([]any)[0].(map[string]any)["session"].(map[string]any)
				session["argv"] = mutate(session["argv"].([]any))
				out := filepath.Join(realTemp(t), "out")
				r := q.capture(plan, out, nil, "--allow-model-calls")
				if r.code != 2 || !strings.Contains(r.stderr, "cursor trusted recipe must target only the generated workspace") {
					t.Fatalf("%s: %+v", name, r)
				}
				if _, err := os.Stat(q.launches); err == nil {
					t.Fatalf("%s launched", name)
				}
			}
		}},
		{"enable-failed", func(t *testing.T) {
			q, c, b, r := run(t, map[string]string{"ENABLE": "fail"})
			stopped(t, q, c, r, mcpqual.ReasonCursorApprovalFailed, mcpqual.ScopeUnverifiable, 1)
			if *c.Approval.Stage.Exit != 1 || !strings.Contains(string(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileApprovalStderr)]), "cannot enable probe") {
				t.Fatalf("enable stage %+v", c.Approval.Stage)
			}
		}},
		{"outside-change", func(t *testing.T) {
			q, c, _, r := run(t, map[string]string{"ENABLE": "owner"})
			stopped(t, q, c, r, mcpqual.ReasonCursorOutside, mcpqual.ScopeOutside, 1)
			if !slices.Equal(c.Approval.Changes, []mcpqual.CaptureApprovalChange{{Path: "<home>/.cursor/approved-servers.json", Kind: "added"}}) {
				t.Fatalf("changes %+v", c.Approval.Changes)
			}
			// Detected after the fact and never undone.
			if _, err := os.Stat(filepath.Join(q.home, ".cursor", "approved-servers.json")); err != nil {
				t.Fatal("the outside write was restored")
			}
			q, c, _, r = run(t, map[string]string{"ENABLE": "ancestor"})
			stopped(t, q, c, r, mcpqual.ReasonCursorOutside, mcpqual.ScopeOutside, 1)
			if !slices.Equal(c.Approval.Changes, []mcpqual.CaptureApprovalChange{{Path: "<ancestor-1>/.cursor", Kind: "added"}, {Path: "<ancestor-1>/.cursor/approved-servers.json", Kind: "added"}}) {
				t.Fatalf("ancestor changes %+v", c.Approval.Changes)
			}
		}},
		{"scope-unverifiable", func(t *testing.T) {
			// No change: no positive workspace evidence.
			q, c, _, r := run(t, map[string]string{"ENABLE": "noop"})
			stopped(t, q, c, r, mcpqual.ReasonCursorScopeUnverified+": no change", mcpqual.ScopeUnverifiable, 1)
			// A symbolic link after the command.
			q, c, _, r = run(t, map[string]string{"ENABLE": "symlink"})
			stopped(t, q, c, r, mcpqual.ReasonCursorScopeUnverified+": the inventory after the command is incomplete (a symbolic link", mcpqual.ScopeUnverifiable, 1)
			// An inaccessible or symlinked owner tree before the command:
			// zero enable launches.
			for name, plant := range map[string]func(q *qualEnv){
				"inaccessible": func(q *qualEnv) {
					locked := mkdir(t, filepath.Join(q.home, ".cursor", "locked"))
					os.Chmod(locked, 0o000)
					t.Cleanup(func() { os.Chmod(locked, 0o700) })
				},
				"symlink": func(q *qualEnv) { os.Symlink("/", filepath.Join(q.home, ".cursor", "root")) },
			} {
				q := newQualEnv(t)
				ownerCursor(t, q)
				plant(q)
				out := filepath.Join(realTemp(t), "out")
				r := q.capture(cursorPlan(q, nil), out, nil, "--allow-model-calls")
				c := captureClient(t, bundle(t, out).Manifest, "cursor")
				stopped(t, q, c, r, mcpqual.ReasonCursorScopeUnverified+": the inventory before the command is incomplete", mcpqual.ScopeUnverifiable, 0)
				if c.Approval.Stage.State != mcpqual.StageNotRun {
					t.Fatalf("%s: stage %+v", name, c.Approval.Stage)
				}
			}
			// Over-bound inventories (injected small caps): before the command
			// no enable runs; after it the session never starts.
			for name, tc := range map[string]struct {
				entries, enables int
				want             string
			}{
				"over-bound-pre": {4, 0, "the inventory before the command is incomplete (the entry bound"},
				// Design decoder-enrollment B2 (FP-13/FP-20): the fixed
				// .cursor/cli.json is one more entry in the initial state.
				"over-bound-post": {7, 1, "the inventory after the command is incomplete (the entry bound"},
			} {
				q := newQualEnv(t)
				ownerCursor(t, q)
				out := filepath.Join(realTemp(t), "out")
				man := inProcessRun(t, q, cursorPlan(q, nil), out, func(c *mcpqual.CaptureRunner) { c.ApprovalLimits = mcpqual.ApprovalScanLimits{Entries: tc.entries} })
				c := captureClient(t, man, "cursor")
				stopped(t, q, c, result{code: man.ExitCode()}, mcpqual.ReasonCursorScopeUnverified+": "+tc.want, mcpqual.ScopeUnverifiable, tc.enables)
				_ = name
			}
		}},
		{"mcp-denied", func(t *testing.T) {
			q, c, _, r := run(t, map[string]string{"SCENARIO": "noprobe", "EXIT": "1", "STDERR": "Error: MCP server probe needs approval\n"})
			log := q.launchLog()
			if r.code != 5 || deref(c.Reason) != mcpqual.ReasonProbeNotObserved || c.Approval.Scope != mcpqual.ScopeWorkspaceOnly || len(log["enable"]) != 1 || len(log["session"]) != 1 {
				t.Fatalf("mcp denied after a scoped approval: %+v %s %v", r, deref(c.Reason), log)
			}
			q.groupsGone()
		}},
		{"cleanup-failure", func(t *testing.T) {
			// The enable's cleanup (the third reap) fails: no session, and no
			// further client starts.
			q := newQualEnv(t)
			ownerCursor(t, q)
			out := filepath.Join(realTemp(t), "out")
			man := inProcessRun(t, q, cursorPlan(q, nil, "claude"), out, func(c *mcpqual.CaptureRunner) { c.Reaper = &faultyReaper{inner: c.Reaper, fail: 3} })
			c := captureClient(t, man, "cursor")
			stopped(t, q, c, result{code: man.ExitCode()}, mcpqual.ReasonCleanupFailed, mcpqual.ScopeUnverifiable, 1)
			if man.Cleanup.OK || captureClient(t, man, "claude").State != mcpqual.CaptureNotRun {
				t.Fatalf("cleanup failure %+v", man.Cleanup)
			}
		}},
	})
}
