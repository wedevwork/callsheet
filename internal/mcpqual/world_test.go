package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// vendorModel is a fake vendor's behaviour: its silent default timeout,
// the timeout under the raised configuration, whether progress resets it
// and any absolute cap under progress.
type vendorModel struct {
	decoder        string
	version        string
	timeoutMS      int64
	raisedMS       int64
	resets         bool
	capMS          int64
	noToken        bool
	clientName     string
	clientVersion  string
	scenario       func(caseID string) string // a decoder fixture scenario overriding success/timeout
	noCancelEvent  bool                       // the probe records no endpoint on timeout
	transcriptTail string
	advance        bool   // the case consumes fake time
	versionExit    int    // the version command's exit code
	toolID         string // replaces the claude fixture's tool-use ID when set
	noProgress     bool   // the probe records no progress even with a token and interval
}

// fakeProc is a launched fake: stdout is fixed, it exits at once unless
// hang is set.
type fakeProc struct {
	pgid   int
	out    io.Reader
	exited chan struct{}
	exit   *int
}

func (p *fakeProc) PGID() int               { return p.pgid }
func (p *fakeProc) Stdout() io.Reader       { return p.out }
func (p *fakeProc) Exited() <-chan struct{} { return p.exited }
func (p *fakeProc) Status() (*int, *string) { return p.exit, nil }
func (p *fakeProc) CloseStdout()            {}

type fakeLauncher struct {
	t        *testing.T
	model    vendorModel
	clock    *testkit.FakeClock
	origin   int64 // the probe clock's origin, unrelated to the harness clock
	mu       sync.Mutex
	launches []ProcSpec
	hang     func(ProcSpec) bool
	onStart  func(ProcSpec) // runs at each launch, before the process "exits"
	hung     map[int]*fakeProc
	started  chan ProcSpec
	startErr error
}

func newWorld(t *testing.T, m vendorModel) *fakeLauncher {
	if m.decoder == "" {
		m.decoder = "claude-json"
	}
	if m.version == "" {
		m.version = "2.1.282 (Claude Code)"
	}
	if m.clientName == "" {
		m.clientName, m.clientVersion = "claude-code", "2.1.282"
	}
	return &fakeLauncher{t: t, model: m, clock: testkit.NewFakeClock(epoch), origin: 7_000_000_000_000, started: make(chan ProcSpec, 256)}
}

func (f *fakeLauncher) sessions() []ProcSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ProcSpec
	for _, s := range f.launches {
		if !slices.Contains(s.Args, "--version") && !slices.Contains(s.Args, "--help") {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeLauncher) Start(spec ProcSpec) (Proc, error) {
	f.mu.Lock()
	f.launches = append(f.launches, spec)
	n := len(f.launches)
	f.mu.Unlock()
	f.started <- spec
	if f.startErr != nil {
		return nil, f.startErr
	}
	if !strings.HasPrefix(spec.Path, "/fake/") && !strings.HasSuffix(spec.Path, "/claude") {
		f.t.Errorf("launched a non-fixture executable %s", spec.Path)
	}
	if f.onStart != nil {
		f.onStart(spec)
	}
	p := &fakeProc{pgid: 1000 + n, exited: make(chan struct{}), exit: new(int)}
	if f.hang != nil && f.hang(spec) {
		p.out = strings.NewReader("")
		f.mu.Lock()
		if f.hung == nil {
			f.hung = map[int]*fakeProc{}
		}
		f.hung[p.pgid] = p
		f.mu.Unlock()
		return p, nil
	}
	defer close(p.exited)
	switch {
	case slices.Contains(spec.Args, "--version"):
		p.out = strings.NewReader(f.model.version + "\n")
		*p.exit = f.model.versionExit
	case slices.Contains(spec.Args, "--help"):
		p.out = strings.NewReader("usage: fake --token sk-live-ABCDEFGHIJKLMNOPQRS\n")
	default:
		p.out = bytes.NewReader(f.session(spec))
	}
	return p, nil
}

// session plays one scripted session against the case file named by the
// rendered config, writing the probe's events itself.
func (f *fakeLauncher) session(spec ProcSpec) []byte {
	t := f.t
	caseID := spec.Args[slices.Index(spec.Args, "--case")+1]
	cfgRaw, err := os.ReadFile(spec.Args[slices.Index(spec.Args, "--config")+1])
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Server   string `json:"server"`
		CaseFile string `json:"case_file"`
		Events   string `json:"events"`
		Raised   bool   `json:"raised"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatalf("config %s: %v", cfgRaw, err)
	}
	cfRaw, _ := os.ReadFile(cfg.CaseFile)
	cf, err := ParseCaseFile(cfRaw)
	if err != nil {
		t.Fatal(err)
	}
	pc, _ := cf.lookup(caseID)
	m := f.model
	limit := m.timeoutMS
	if cfg.Raised {
		limit = m.raisedMS
	}
	token := !m.noToken
	progress := pc.ProgressIntervalMS > 0 && token
	if progress && m.resets {
		limit = m.capMS
	}
	timedOut := limit > 0 && pc.DelayMS >= limit
	end := pc.DelayMS
	if timedOut {
		end = limit
	}
	var ev bytes.Buffer
	seq := 0
	add := func(off int64, e ProbeEvent) {
		seq++
		e.Seq, e.OffsetNS, e.RunID = seq, f.origin+off*int64(time.Millisecond), cf.RunID
		b, _ := encodeJSON(e)
		ev.Write(append(b, '\n'))
	}
	name, ver := m.clientName, m.clientVersion
	add(0, ProbeEvent{Kind: EvStart})
	add(1, ProbeEvent{Kind: EvInitialize, ClientName: &name, ClientVersion: &ver})
	sc := scSuccess
	if timedOut {
		sc = scTimeout
	}
	if m.scenario != nil {
		if s := m.scenario(caseID); s != "" {
			sc = s
		}
	}
	if sc == scSuccess || sc == scTimeout || sc == scToolError || sc == scTwoCalls || sc == scUnknownEnd {
		tp := token
		rid := json.RawMessage(`1`)
		re := ProbeEvent{Kind: EvReceipt, CaseID: caseID, RequestID: rid, TokenPresent: &tp}
		if token {
			re.Token = json.RawMessage(`"tok"`)
		}
		add(2, re)
		if progress && !m.noProgress {
			for k := int64(1); k*pc.ProgressIntervalMS < end; k++ {
				add(2+k*pc.ProgressIntervalMS, ProbeEvent{Kind: EvProgress, CaseID: caseID, RequestID: rid, Progress: int(k)})
			}
		}
		switch {
		case !timedOut:
			add(2+end, ProbeEvent{Kind: EvScheduled, CaseID: caseID, RequestID: rid})
			add(2+end, ProbeEvent{Kind: EvCompleted, CaseID: caseID, RequestID: rid})
		case !m.noCancelEvent:
			add(2+end, ProbeEvent{Kind: EvCancelled, CaseID: caseID, RequestID: rid})
		}
	}
	add(3+end, ProbeEvent{Kind: EvEOF})
	add(3+end, ProbeEvent{Kind: EvExit, Reason: "eof"})
	if err := os.WriteFile(cfg.Events, ev.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.advance {
		f.clock.Advance(time.Duration(end) * time.Millisecond)
	}
	lines := fixture(m.decoder, sc, caseID, cf.Nonce, 0)
	if m.toolID != "" {
		for i := range lines {
			lines[i] = strings.ReplaceAll(lines[i], `"t1"`, `"`+m.toolID+`"`)
		}
	}
	if m.transcriptTail != "" {
		lines = append(lines, strings.Split(m.transcriptTail, "\n")...)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// fakeReaper records reaps and fails them when told to.
type fakeReaper struct {
	mu    sync.Mutex
	pgids []int
	fail  func(pgid int) bool
}

func (r *fakeReaper) Reap(p Proc) CaseCleanup {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pgids = append(r.pgids, p.PGID())
	if r.fail != nil && r.fail(p.PGID()) {
		return CaseCleanup{TermSent: true, KillSent: true, Error: sptr("cleanup: injected")}
	}
	return CaseCleanup{GroupGone: true}
}

const testConfig = `{"server":"{server}","case_file":"{case_file}","events":"{events}","raised":false}`

func fullPlan() *Plan {
	return &Plan{Version: 1, Limits: &Limits{MaxSessionsPerClient: 12, MaxCaseMS: 10000, MaxClientMS: 60000}, Clients: []PlanClient{{
		ID: "claude", Executable: "/fake/claude", ExpectedVersion: "2.1.282 (Claude Code)", VersionArgv: []string{"--version"},
		Driver: DriverDirect, Decoder: "claude-json", DecoderFixture: "claude-json/synthetic",
		Session: Recipe{Argv: []string{"--case", "{case}", "--config", "{config}", "-p", "{prompt}", "--server", "{server}", "--ws", "{workspace}"}},
		Config: ConfigRecipes{Default: ConfigRecipe{Path: "cfg/mcp.json", Content: testConfig},
			Raised: &ConfigRecipe{Path: "mcp.json", Content: strings.Replace(testConfig, "false", "true", 1), Env: map[string]string{"FAKE_TIMEOUT": "long-raised-value"}}},
		Override: &Override{Setting: "tool_timeout_ms", Value: "1000", Explanation: "raises the per-call timeout", Evidence: []string{"tests/testdata/cli-help/claude-help.txt"}},
		Env:      map[string]string{"FAKE_SECRET": "hunter2-secret-value"},
		Phases: Phases{Default: &DefaultPhase{DelaysMS: []int64{100, 300, 900}}, Override: &DelayPhase{DelayMS: 600, BoundDelayMS: 2000},
			Progress: &struct{}{}, Absolute: &DelayPhase{DelayMS: 1500}},
	}}}
}

// planWith is fullPlan limited to the given phases (setup always runs).
func planWith(ph Phases) *Plan {
	p := fullPlan()
	p.Clients[0].Phases = ph
	return p
}

func defaultOnly(delays ...int64) Phases { return Phases{Default: &DefaultPhase{DelaysMS: delays}} }

func fullModel() vendorModel {
	return vendorModel{timeoutMS: 500, raisedMS: 1000, resets: true, capMS: 1200}
}

func newRunner(t *testing.T, w *fakeLauncher, p *Plan) *Runner {
	return &Runner{Plan: p, PlanSHA256: strings.Repeat("a", 64), OutDir: filepath.Join(t.TempDir(), "out"), GOOS: "linux", GOARCH: "amd64", Hostname: "host-1",
		ServerPath: "/opt/mcpqual", BaseEnv: []string{"PATH=/usr/bin"}, Launcher: w, Reaper: &fakeReaper{}, Clock: w.clock, Registry: DefaultRegistry(),
		Log: io.Discard, RunID: "run-1", Nonce: "nonce-1", CaptureDate: "2026-09-29", HarnessVersion: "test", Home: "/home/owner", User: "owner",
		HashFile: func(p string) (string, error) {
			if strings.HasPrefix(p, "/fake/") {
				return strings.Repeat("b", 64), nil
			}
			return "", fs.ErrNotExist
		}}
}

// runPlan runs r (Run validates the report it writes) and requires the
// disposable workspace to be gone.
func runPlan(t *testing.T, r *Runner, ctx context.Context) *Report {
	t.Helper()
	rep, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.OutDir, workDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the disposable workspace survived the run")
	}
	return rep
}

// strictReport re-reads the written report through the strict schema and
// checks every evidence hash.
func strictReport(t *testing.T, r *Runner) *Report {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.OutDir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.CheckEvidence(r.OutDir); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func phase(t *testing.T, rep *Report, client, name string) PhaseReport {
	t.Helper()
	for _, c := range rep.Clients {
		if c.ID == client {
			for _, ph := range c.Phases {
				if ph.Name == name {
					return ph
				}
			}
		}
	}
	t.Fatalf("no phase %s.%s", client, name)
	return PhaseReport{}
}

func (ph PhaseReport) String() string {
	r, why := "", ""
	if ph.Result != nil {
		r = *ph.Result
	}
	if ph.Reason != nil {
		why = *ph.Reason
	}
	return fmt.Sprintf("%s:%s:%s%s:%s obs=%d cases=%d", ph.Name, ph.Status, r, bracketText(ph.LowerBoundMS, ph.UpperBoundMS), why, ph.Observations, len(ph.Cases))
}

// worldSignaler signals the fake world's process groups: a hung process
// exits on TERM or KILL; every other group is already gone.
type worldSignaler struct{ w *fakeLauncher }

func (s worldSignaler) Signal(pid int, sig syscall.Signal) error {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	p, ok := s.w.hung[-pid]
	if !ok {
		return syscall.ESRCH
	}
	select {
	case <-p.exited:
		return syscall.ESRCH
	default:
	}
	if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
		close(p.exited)
	}
	return nil
}
