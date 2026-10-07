package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		"codex": {[]string{"exec", "--help"}, func(ws, srv string) []string {
			return []string{"exec", "--json", "--skip-git-repo-check", "-C", ws, "-m", m, "-c", `mcp_servers.probe.command="` + srv + `"`, "-c",
				`mcp_servers.probe.args=["serve","--case-file","` + ws + `/case.json","--events","` + ws + `/server-events.jsonl"]`, mcpqual.Prompt("codex-capture-setup")}
		}, "probe-config.toml", tomlCfg},
		"grok": {[]string{"--help"}, func(ws, srv string) []string {
			return []string{"-p", mcpqual.Prompt("grok-capture-setup"), "--output-format", "json", "-m", m}
		}, ".grok/config.toml", tomlCfg},
		"cursor": {[]string{"--help"}, func(ws, srv string) []string {
			return []string{"-p", mcpqual.Prompt("cursor-capture-setup"), "--output-format", "stream-json", "--model", m}
		}, ".cursor/mcp.json", jsonCfg},
	}
	serverRe := regexp.MustCompile(`"command":"([^"]+)"|command = "([^"]+)"`)
	one := func(id string) func(t *testing.T) {
		return func(t *testing.T) {
			rc := recipes[id]
			q := newQualEnv(t)
			out := filepath.Join(realTemp(t), "out")
			r := q.capture(q.capturePlan(nil, id), out, nil, "--allow-model-calls")
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
			if c.State != mcpqual.CaptureComplete || len(log["version"]) != 1 || len(log["help"]) != 1 || len(log["session"]) != 1 || c.Config.Path != rc.cfgRel ||
				!strings.Contains(cfg, "<workspace>/case.json") || strings.Contains(cfg, ws) || c.Probe.Receipts != 1 || !c.Probe.Completed || b.Manifest.Limits.SessionsPerClient != 1 {
				t.Fatalf("%s: %+v launches %v config %q", id, c, log, cfg)
			}
			// The help command, with the exact argv after the executable.
			if !strings.Contains(string(b.Files[mcpqual.ClientFile(id, mcpqual.FileHelpStdout)]), "usage: fake") || !slices.Equal(c.Argv[:1], rc.argv("<workspace>", "<server>")[:1]) {
				t.Fatalf("%s: help or argv %q", id, c.Argv)
			}
			// Setup only: the case file named a zero-delay case, and no 15 s
			// session ran (one receipt, one completion).
			if !strings.Contains(r.stderr, "setup only; default phase not executed") || strings.Contains(string(b.Files[mcpqual.ClientFile(id, mcpqual.FileServerEvents)]), "15000") {
				t.Fatalf("%s: scheduling %s", id, r.stderr)
			}
			q.groupsGone()
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
	b, _ := json.Marshal(plan)
	p, err := mcpqual.ParseCapturePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(realTemp(t), "out")
	c := &mcpqual.CaptureRunner{Plan: p, OutDir: out, GOOS: "linux", GOARCH: "amd64", ServerPath: qualBinary(t), BaseEnv: q.env(""), Launcher: procexec.Launcher{},
		Reaper: mcpqual.GroupReaper{Sig: mcpqual.SysSignaler{}, Clock: mcpqual.RealClock,
			Policy: mcpqual.CleanupPolicy{Grace: mcpqual.CleanupGrace, Limit: mcpqual.CleanupLimit, Poll: mcpqual.CleanupPoll}},
		Clock: mcpqual.RealClock, Log: io.Discard, RunID: "run-flood", Nonce: "nonceflood", CapturedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		HarnessVersion: mcpqual.HarnessVersion, Home: q.home, Limits: lim}
	m, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q.groupsGone()
	return m, out
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
			q := newQualEnv(t)
			out := filepath.Join(realTemp(t), "out")
			r := q.capture(q.capturePlan(nil, "claude", "codex"), out, []string{mcpqual.FaultEnv + "=" + mcpqual.FaultValue}, "--allow-model-calls")
			m := bundle(t, out).Manifest
			c := captureClient(t, m, "claude")
			if r.code != 5 || m.Cleanup.OK || m.State == mcpqual.CaptureComplete || deref(c.Reason) != mcpqual.ReasonCleanupFailed ||
				deref(captureClient(t, m, "codex").Reason) != mcpqual.ReasonCleanupFailed || len(q.launchLog()["session"]) != 0 {
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
		reg := mcpqual.DefaultRegistry().WithVersion("claude-json", mcpqual.DecoderVersion{Version: version, Fixture: e.Fixture, Qualified: true,
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
			reg := mcpqual.DefaultRegistry().WithVersion("claude-json", mcpqual.DecoderVersion{Version: e.entry.Version, Fixture: e.entry.Fixture, Qualified: true,
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
			// parent.
			idx, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{Root: root, Registry: mcpqual.DefaultRegistry()})
			if err != nil || len(idx.Entries) != 0 || len(mcpqual.DefaultRegistry().QualifiedVersions()) != 0 {
				t.Fatalf("production enrollment (slice A: zero entries): %v", err)
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

// FP-6: slice A's production index parses with zero entries and no
// production version is qualified; synthetic prose never qualifies.
func TestMCPEnrollmentReplay(t *testing.T) {
	t.Parallel()
	runInventory(t, 2, []string{"empty-index", "synthetic-prose"}, []captureCase{
		{"empty-index", func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(mcpqual.EnrollmentIndexPath)))
			if err != nil {
				t.Fatal(err)
			}
			idx, err := mcpqual.ParseEnrollmentIndex(b)
			if err != nil || idx.Schema != mcpqual.EnrollmentSchema || len(idx.Entries) != 0 || len(mcpqual.DefaultRegistry().QualifiedVersions()) != 0 {
				t.Fatalf("index %v %+v", err, idx)
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
// exact version and platform.
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
			es[i].Version, es[i].Platform = version, platform
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
				"never retried until green", "macOS timeout compatibility remains UNVERIFIED"} {
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
	runInventory(t, 4, []string{"native-and-test", "bench", "stress", "coverage-and-docs"}, []captureCase{
		{"native-and-test", func(t *testing.T) {
			req := devcheck.NativeRequiredTests()
			if len(req) != 412 || !slices.Equal(req[403:], nine) || req[402] != "TestMCPShortConfirmation" {
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
			doc := string(repoFile(t, "docs/ci.md"))
			for _, w := range append([]string{"Decoder enrollment: baseline is main run 37523901881 (ff8058f)", "9 more names, 412 in all", "(9 ordinary calls)", "CI stays 18 jobs"}, nine...) {
				if !strings.Contains(doc, w) {
					t.Fatalf("docs/ci.md lacks %q", w)
				}
			}
		}},
	})
}
