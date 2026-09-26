//go:build linux || darwin

package processgroup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Helper-process environment. The helper is this package's compiled test
// binary; its TestMain dispatches to RunHelper when EnvHelper is "1".
const (
	EnvHelper  = "CALLSHEET_PG_HELPER"
	EnvFake    = "CALLSHEET_PG_FAKE"
	EnvWorkDir = "CALLSHEET_PG_WORKDIR"
	EnvResults = "CALLSHEET_PG_RESULTS"
	EnvGroups  = "CALLSHEET_PG_GROUPS"
	// envLifetimeFD mirrors fakeadapter.EnvLifetimeFD (asserted in tests).
	envLifetimeFD = "CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD"
	// fakeSignalSuffix mirrors fakeadapter.SignalFileSuffix (asserted in
	// tests): the descendant's signal log is the leader's plus this suffix.
	fakeSignalSuffix = ".grandchild"
	// CaseDeadline bounds one case inside the helper.
	CaseDeadline = 30 * time.Second
	// ReapLimit bounds scheduler/reaping after the escalation.
	ReapLimit = 5 * time.Second
)

// Case is one process-group experiment.
type Case struct {
	Name               string `json:"name"`
	LeaderTermMode     string `json:"leader_term_mode"`
	GrandchildTermMode string `json:"grandchild_term_mode"`
	ExpectKill         bool   `json:"expect_kill"`
	LeaderExitsFirst   bool   `json:"leader_exits_first"`
}

// Cases are the three required experiments.
var Cases = []Case{
	{Name: "cooperative", LeaderTermMode: "exit", GrandchildTermMode: "exit"},
	{Name: "resistant", LeaderTermMode: "ignore", GrandchildTermMode: "ignore", ExpectKill: true},
	{Name: "leader-exits-first", LeaderTermMode: "exit", GrandchildTermMode: "ignore", ExpectKill: true, LeaderExitsFirst: true},
}

// Observation is a liveness snapshot.
type Observation struct {
	Label           string    `json:"label"`
	At              time.Time `json:"at"`
	LeaderExited    bool      `json:"leader_exited"`
	LeaderAlive     bool      `json:"leader_alive"`
	DescendantAlive bool      `json:"descendant_alive"`
	LifetimeOpen    bool      `json:"lifetime_open"`
	Err             string    `json:"err,omitempty"`
}

// ProcStatus is a recorded wait status.
type ProcStatus struct {
	PID      int    `json:"pid"`
	Exited   bool   `json:"exited"`
	ExitCode int    `json:"exit_code"`
	Signaled bool   `json:"signaled"`
	Signal   string `json:"signal,omitempty"`
	ReapedBy string `json:"reaped_by"`
	Err      string `json:"err,omitempty"`
}

// CaseResult is everything one case recorded.
type CaseResult struct {
	Case                 Case          `json:"case"`
	PGID                 int           `json:"pgid"`
	LeaderPID            int           `json:"leader_pid"`
	DescendantPID        int           `json:"descendant_pid"`
	LeaderPGID           int           `json:"leader_pgid"`
	DescendantPGID       int           `json:"descendant_pgid"`
	TermSentAt           time.Time     `json:"term_sent_at"`
	Deadline             time.Time     `json:"deadline"`
	KillSent             bool          `json:"kill_sent"`
	KillSentAt           time.Time     `json:"kill_sent_at"`
	AlreadyExited        bool          `json:"already_exited"`
	LeaderExitedAt       time.Time     `json:"leader_exited_at"`
	LifetimeClosedAt     time.Time     `json:"lifetime_closed_at"`
	Observations         []Observation `json:"observations"`
	Leader               ProcStatus    `json:"leader"`
	Descendant           ProcStatus    `json:"descendant"`
	LeaderSignals        []string      `json:"leader_signals"`
	DescendantSignals    []string      `json:"descendant_signals"`
	LeaderGoneESRCH      bool          `json:"leader_gone_esrch"`
	DescendantGoneESRCH  bool          `json:"descendant_gone_esrch"`
	GroupGoneESRCH       bool          `json:"group_gone_esrch"`
	EmergencyKill        bool          `json:"emergency_kill"`
	Errors               []string      `json:"errors"`
	Pass                 bool          `json:"pass"`
	ObservedGraceMillis  float64       `json:"observed_grace_ms"`
	LeaderExitBeforeKill bool          `json:"leader_exit_before_kill"`
}

// Observation returns the named observation, if any.
func (r *CaseResult) Observation(label string) (Observation, bool) {
	for _, o := range r.Observations {
		if o.Label == label {
			return o, true
		}
	}
	return Observation{}, false
}

// Report is the helper's output.
type Report struct {
	Platform  string       `json:"platform"`
	Subreaper bool         `json:"subreaper"`
	Cases     []CaseResult `json:"cases"`
	Errors    []string     `json:"errors"`
}

// Pass reports whether every case passed and no helper error occurred.
func (r *Report) Pass() bool {
	if len(r.Errors) > 0 || len(r.Cases) != len(Cases) {
		return false
	}
	for _, c := range r.Cases {
		if !c.Pass {
			return false
		}
	}
	return true
}

// Config wires RunCase to the fake and the OS.
type Config struct {
	Fake    string
	WorkDir string
	OnGroup func(pgid int)
	Sys     Signaler
	Clock   Clock
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

type readyInfo struct {
	PID           int `json:"pid"`
	DescendantPID int `json:"descendant_pid"`
}

func waitReady(path string, leaderDone <-chan struct{}, limit time.Duration) (readyInfo, error) {
	deadline := time.Now().Add(limit)
	for {
		if b, err := os.ReadFile(path); err == nil {
			var r readyInfo
			if err := json.Unmarshal(b, &r); err != nil {
				return r, fmt.Errorf("ready file: %w", err)
			}
			return r, nil
		}
		if isClosed(leaderDone) {
			return readyInfo{}, errors.New("leader exited before readiness")
		}
		if time.Now().After(deadline) {
			return readyInfo{}, errors.New("readiness timeout")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func statusFromWait(pid int, ws syscall.WaitStatus, by string) ProcStatus {
	s := ProcStatus{PID: pid, ReapedBy: by}
	switch {
	case ws.Exited():
		s.Exited, s.ExitCode = true, ws.ExitStatus()
	case ws.Signaled():
		s.Signaled, s.Signal = true, signalName(ws.Signal())
	}
	return s
}

func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return strconv.Itoa(int(sig))
	}
}

// readSignals returns the signals the fake recorded for pid in the signal
// log at path. A missing log is (nil, nil): the case's acknowledgment
// assertions then fail. Any other open or read failure is corrupt evidence
// and is returned, wrapped with the path, together with the signals read
// before it.
func readSignals(path string, pid int) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("signal log %s: %w", path, err)
	}
	defer f.Close()
	out, err := scanSignals(f, pid)
	if err != nil {
		return out, fmt.Errorf("signal log %s: %w", path, err)
	}
	return out, nil
}

// scanSignals extracts, in order, the signal names of pid's records from a
// JSON-lines signal log, ignoring malformed lines and other pids. The
// default Scanner token limit is kept: an oversized line is corrupt
// evidence. On a scan failure it returns the signals collected so far and
// the error.
func scanSignals(r io.Reader, pid int) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var rec struct {
			PID    int    `json:"pid"`
			Signal string `json:"signal"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.PID == pid {
			out = append(out, rec.Signal)
		}
	}
	return out, sc.Err()
}

// collectSignals reads the leader's and (when known) the descendant's signal
// logs in dir into res. Both reads always run; each failure is recorded in
// res.Errors, so valid lines read before a failure cannot hide it. It is the
// evidence step of RunCase's deferred cleanup.
func collectSignals(res *CaseResult, dir string, leader, descendant int) {
	var err error
	res.LeaderSignals, err = readSignals(filepath.Join(dir, "signals.jsonl"), leader)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("leader %d signal evidence: %v", leader, err))
	}
	if descendant > 0 {
		res.DescendantSignals, err = readSignals(filepath.Join(dir, "signals.jsonl"+fakeSignalSuffix), descendant)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("descendant %d signal evidence: %v", descendant, err))
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// RunCase runs one experiment. It always attempts group KILL, reaping and
// ESRCH verification before returning, even after a failed assertion.
func RunCase(ctx context.Context, cfg Config, c Case) (res CaseResult) {
	res = CaseResult{Case: c}
	fail := func(format string, a ...any) { res.Errors = append(res.Errors, fmt.Sprintf(format, a...)) }
	sys, clock := cfg.Sys, cfg.Clock
	dir := filepath.Join(cfg.WorkDir, c.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail("workdir: %v", err)
		return res
	}
	lr, lw, err := os.Pipe()
	if err != nil {
		fail("lifetime pipe: %v", err)
		return res
	}
	logf, err := os.Create(filepath.Join(dir, "fake.log"))
	if err != nil {
		lr.Close()
		lw.Close()
		fail("log: %v", err)
		return res
	}
	defer logf.Close()
	cmd := exec.Command(cfg.Fake, "--duration=1h", "--term-mode="+c.LeaderTermMode, "--spawn-grandchild",
		"--grandchild-term-mode="+c.GrandchildTermMode, "--ready-file=ready.json", "--signal-file=signals.jsonl")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{lw}
	cmd.Env = append(os.Environ(), envLifetimeFD+"=3")
	startErr := cmd.Start()
	lw.Close() // the supervisor keeps only the read end
	if startErr != nil {
		lr.Close()
		fail("start fake: %v", startErr)
		return res
	}
	pgid := cmd.Process.Pid
	res.PGID, res.LeaderPID = pgid, pgid
	if cfg.OnGroup != nil {
		cfg.OnGroup(pgid)
	}

	leaderDone := make(chan struct{})
	var leaderStatus ProcStatus
	var leaderExitedAt time.Time
	go func() {
		err := cmd.Wait()
		leaderExitedAt = clock.Now()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			leaderStatus = statusFromWait(pgid, ws, "Cmd.Wait")
		} else if err != nil {
			leaderStatus = ProcStatus{PID: pgid, Err: err.Error()}
		}
		close(leaderDone)
	}()
	lifetimeClosed := make(chan struct{})
	var lifetimeClosedAt time.Time
	go func() {
		io.Copy(io.Discard, lr)
		lifetimeClosedAt = clock.Now()
		lr.Close()
		close(lifetimeClosed)
	}()
	descDone := make(chan struct{})
	allGone := make(chan struct{})
	var descStatus ProcStatus
	desc := 0
	started := false

	defer func() {
		// Cleanup and verification run on every path.
		if !isClosed(allGone) {
			// Start succeeded, so pgid is the leader's pid and names its
			// group: KILL it whenever members may remain, including the
			// failed-startup window where the leader already exited but a
			// descendant can still be alive.
			err := sys.Signal(-pgid, syscall.SIGKILL)
			res.EmergencyKill = err == nil
			if err != nil && !errors.Is(err, syscall.ESRCH) {
				fail("emergency KILL: %v", err)
			}
			waitClosed(leaderDone, ReapLimit)
			if !started {
				reapGroup(pgid, ReapLimit)
				close(descDone)
				close(allGone)
			}
			if !waitClosed(allGone, ReapLimit) {
				fail("members not reaped within %v after emergency KILL", ReapLimit)
			}
		}
		if !waitClosed(lifetimeClosed, ReapLimit) {
			fail("descendant lifetime pipe still open after cleanup")
		}
		// Goroutine-written fields are read only after their channel closed.
		if isClosed(leaderDone) {
			res.Leader = leaderStatus
			res.LeaderExitedAt = leaderExitedAt
		} else {
			fail("leader was never reaped")
		}
		if isClosed(lifetimeClosed) {
			res.LifetimeClosedAt = lifetimeClosedAt
		}
		if started && isClosed(descDone) {
			res.Descendant = descStatus
		}
		targets := []int{pgid, -pgid}
		if desc > 0 {
			targets = append(targets, desc)
		}
		if err := WaitGone(sys, clock, ReapLimit, 5*time.Millisecond, targets...); err != nil {
			fail("teardown not verified: %v", err)
		}
		res.LeaderGoneESRCH = mustGone(sys, pgid, fail)
		res.GroupGoneESRCH = mustGone(sys, -pgid, fail)
		if desc > 0 {
			res.DescendantGoneESRCH = mustGone(sys, desc, fail)
		}
		collectSignals(&res, dir, pgid, desc)
		evaluate(&res)
		res.Pass = len(res.Errors) == 0
	}()

	ready, err := waitReady(filepath.Join(dir, "ready.json"), leaderDone, 10*time.Second)
	if err != nil {
		fail("%v", err)
		return res
	}
	desc = ready.DescendantPID
	res.DescendantPID = desc
	if ready.PID != pgid || desc <= 1 {
		fail("ready file pids = %+v, leader %d", ready, pgid)
		return res
	}
	res.LeaderPGID, _ = syscall.Getpgid(pgid)
	res.DescendantPGID, _ = syscall.Getpgid(desc)
	if res.LeaderPGID != pgid || res.DescendantPGID != pgid {
		fail("pgids leader=%d descendant=%d, want %d", res.LeaderPGID, res.DescendantPGID, pgid)
		return res
	}
	if pgid == syscall.Getpgrp() {
		fail("refusing to signal the runner's own group")
		return res
	}
	started = true
	go func() {
		<-leaderDone
		descStatus = reapAdopted(desc, lifetimeClosed)
		close(descDone)
	}()
	go func() {
		<-leaderDone
		<-descDone
		close(allGone)
	}()

	observe := func(label string) Observation {
		o := Observation{Label: label, At: clock.Now(), LeaderExited: isClosed(leaderDone), LifetimeOpen: !isClosed(lifetimeClosed)}
		var errs []string
		if !o.LeaderExited {
			alive, err := Existence(sys, pgid)
			o.LeaderAlive = alive
			if err != nil {
				errs = append(errs, err.Error())
			}
		}
		alive, err := Existence(sys, desc)
		o.DescendantAlive = alive
		if err != nil {
			errs = append(errs, err.Error())
		}
		o.Err = strings.Join(errs, "; ")
		return o
	}
	afterExit := make(chan Observation, 1)
	go func() {
		select {
		case <-leaderDone:
			afterExit <- observe("after-leader-exit")
		case <-ctx.Done():
		}
	}()
	var pre []Observation
	out, err := Escalate(ctx, Plan{PGID: pgid, Sig: sys, Clock: clock, Done: allGone, BeforeDeadline: func() error {
		pre = append(pre, observe("pre-deadline"))
		return nil
	}})
	res.TermSentAt, res.Deadline = out.TermSentAt, out.Deadline
	res.KillSent, res.KillSentAt, res.AlreadyExited = out.KillSent, out.KillSentAt, out.AlreadyExited
	if err != nil {
		fail("escalation: %v", err)
	}
	if !waitClosed(allGone, ReapLimit) {
		fail("members not reaped within %v", ReapLimit)
	}
	select {
	case o := <-afterExit:
		res.Observations = append(res.Observations, o)
	case <-time.After(ReapLimit):
		fail("no after-leader-exit observation")
	}
	res.Observations = append(res.Observations, pre...)
	return res
}

func waitClosed(c <-chan struct{}, limit time.Duration) bool {
	select {
	case <-c:
		return true
	case <-time.After(limit):
		return false
	}
}

func mustGone(sys Signaler, target int, fail func(string, ...any)) bool {
	alive, err := Existence(sys, target)
	if err != nil {
		fail("kill(%d, 0): %v (not proof of absence)", target, err)
		return false
	}
	if alive {
		fail("kill(%d, 0) succeeded: still present", target)
	}
	return !alive
}

// evaluate applies the per-case expectations to a finished result on the
// host platform.
func evaluate(r *CaseResult) { evaluateFor(r, runtime.GOOS) }

// evaluateFor applies the per-case expectations as they hold on goos. The
// descendant's own exit status is checked only on linux, where the helper is
// a child subreaper and reaps it; on darwin launchd reaps orphans, so that
// status is not observable.
func evaluateFor(r *CaseResult, goos string) {
	fail := func(format string, a ...any) { r.Errors = append(r.Errors, fmt.Sprintf(format, a...)) }
	if r.DescendantPID == 0 {
		return
	}
	if !contains(r.LeaderSignals, "SIGTERM") {
		fail("leader did not acknowledge SIGTERM (%v)", r.LeaderSignals)
	}
	if !contains(r.DescendantSignals, "SIGTERM") {
		fail("descendant did not acknowledge SIGTERM (%v)", r.DescendantSignals)
	}
	if r.AlreadyExited {
		fail("group vanished before TERM")
	}
	if !r.TermSentAt.IsZero() && !r.KillSentAt.IsZero() {
		r.ObservedGraceMillis = float64(r.KillSentAt.Sub(r.TermSentAt)) / float64(time.Millisecond)
	}
	r.LeaderExitBeforeKill = !r.LeaderExitedAt.IsZero() && (!r.KillSent || r.LeaderExitedAt.Before(r.KillSentAt))
	adopted := goos == "linux"
	pre, hasPre := r.Observation("pre-deadline")
	after, hasAfter := r.Observation("after-leader-exit")
	switch r.Case.Name {
	case "cooperative":
		if r.KillSent {
			fail("cooperative case sent KILL")
		}
		if !r.Leader.Exited || r.Leader.ExitCode != 0 {
			fail("leader status %+v, want exit 0", r.Leader)
		}
		if adopted && (!r.Descendant.Exited || r.Descendant.ExitCode != 0) {
			fail("descendant status %+v, want exit 0", r.Descendant)
		}
	case "resistant":
		if !r.KillSent || r.KillSentAt.Before(r.Deadline) {
			fail("KILL sent=%v at %v, deadline %v", r.KillSent, r.KillSentAt, r.Deadline)
		}
		if !hasPre || pre.LeaderExited || !pre.LeaderAlive || !pre.DescendantAlive || !pre.LifetimeOpen || pre.Err != "" {
			fail("pre-deadline liveness not proven: %+v", pre)
		}
		if !r.Leader.Signaled || r.Leader.Signal != "SIGKILL" {
			fail("leader status %+v, want SIGKILL", r.Leader)
		}
		if adopted && (!r.Descendant.Signaled || r.Descendant.Signal != "SIGKILL") {
			fail("descendant status %+v, want SIGKILL", r.Descendant)
		}
	case "leader-exits-first":
		if !r.Leader.Exited || r.Leader.ExitCode != 0 {
			fail("leader status %+v, want exit 0 on TERM", r.Leader)
		}
		if !hasAfter || !after.LeaderExited || !after.DescendantAlive || !after.LifetimeOpen || after.Err != "" {
			fail("descendant not proven alive after leader exit: %+v", after)
		}
		if !hasPre || !pre.LeaderExited || !pre.DescendantAlive || !pre.LifetimeOpen || pre.Err != "" {
			fail("descendant not proven alive immediately before the deadline (leader must exit first): %+v", pre)
		}
		if hasAfter && hasPre && after.At.After(pre.At) {
			fail("after-exit observation came after the pre-deadline observation")
		}
		if !r.KillSent || r.KillSentAt.Before(r.Deadline) {
			fail("group KILL sent=%v at %v, deadline %v", r.KillSent, r.KillSentAt, r.Deadline)
		}
		if r.LifetimeClosedAt.IsZero() || r.LifetimeClosedAt.Before(r.KillSentAt) {
			fail("lifetime pipe closed at %v, before group KILL at %v", r.LifetimeClosedAt, r.KillSentAt)
		}
		if adopted && (!r.Descendant.Signaled || r.Descendant.Signal != "SIGKILL" || r.Descendant.ReapedBy != "subreaper") {
			fail("adopted descendant status %+v, want SIGKILL via subreaper", r.Descendant)
		}
	}
}

// RunHelper is the dedicated helper process's main: it becomes a child
// subreaper (Linux), runs every case, writes the JSON report and returns 0
// only if all cases passed. It only supplies the host platform to
// runHelperFor.
func RunHelper(getenv func(string) string) int {
	return runHelperFor(getenv, runtime.GOOS, runtime.GOARCH)
}

// runHelperFor is RunHelper with the report's platform label given
// explicitly. goos and goarch only label the report; the experiment itself
// still uses the build-selected native syscalls and evaluate.
func runHelperFor(getenv func(string) string, goos, goarch string) int {
	rep := &Report{Platform: goos + "/" + goarch}
	fake, work, results, groups := getenv(EnvFake), getenv(EnvWorkDir), getenv(EnvResults), getenv(EnvGroups)
	write := func() int {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if results != "" {
			if err := os.WriteFile(results, b, 0o600); err != nil {
				return 1
			}
		}
		if rep.Pass() {
			return 0
		}
		return 1
	}
	if getenv(EnvHelper) != "1" {
		// Never turn a test runner into a subreaper: only the dedicated
		// helper process (EnvHelper=1) may run the experiment.
		rep.Errors = append(rep.Errors, "RunHelper must run in the dedicated helper process")
		return write()
	}
	if fake == "" || work == "" || results == "" || groups == "" {
		rep.Errors = append(rep.Errors, "helper environment incomplete")
		return write()
	}
	sub, err := setSubreaper()
	rep.Subreaper = sub
	if err != nil {
		rep.Errors = append(rep.Errors, "subreaper: "+err.Error())
		return write()
	}
	onGroup := func(pgid int) {
		f, err := os.OpenFile(groups, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			rep.Errors = append(rep.Errors, "record group: "+err.Error())
			return
		}
		fmt.Fprintf(f, "%d\n", pgid)
		f.Sync()
		f.Close()
	}
	for _, c := range Cases {
		ctx, cancel := context.WithTimeout(context.Background(), CaseDeadline)
		rep.Cases = append(rep.Cases, RunCase(ctx, Config{Fake: fake, WorkDir: work, OnGroup: onGroup, Sys: SysSignaler{}, Clock: RealClock{}}, c))
		cancel()
	}
	return write()
}

// RunExperiment runs the helper (argv, e.g. the compiled package test binary)
// in dir and returns its report. After the helper exits — always — it
// verifies every recorded group is gone, KILLing and polling any survivor.
func RunExperiment(ctx context.Context, argv []string, env []string, fake, dir string) (*Report, error) {
	groups := filepath.Join(dir, "groups.txt")
	results := filepath.Join(dir, "results.json")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	logf, err := os.Create(filepath.Join(dir, "helper.log"))
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(append([]string(nil), env...), EnvHelper+"=1", EnvFake+"="+fake, EnvWorkDir+"="+work, EnvResults+"="+results, EnvGroups+"="+groups)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		cmd.Process.Kill()
		waitErr = errors.Join(ctx.Err(), <-done)
	}
	cleanupErr := EmergencyCleanup(SysSignaler{}, RealClock{}, groups)
	rep := &Report{}
	b, rerr := os.ReadFile(results)
	if rerr == nil {
		rerr = json.Unmarshal(b, rep)
	}
	var errs []error
	if waitErr != nil {
		errs = append(errs, fmt.Errorf("helper: %w", waitErr))
	}
	if rerr != nil {
		errs = append(errs, fmt.Errorf("helper results: %w", rerr))
	}
	if cleanupErr != nil {
		errs = append(errs, cleanupErr)
	}
	return rep, errors.Join(errs...)
}

// EmergencyCleanup KILLs every recorded group still present and verifies
// ESRCH within ReapLimit. A survivor is an error even if KILL removes it.
func EmergencyCleanup(sys Signaler, clock Clock, groupsFile string) error {
	b, err := os.ReadFile(groupsFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, line := range strings.Fields(string(b)) {
		pgid, err := strconv.Atoi(line)
		if err != nil || pgid <= 1 {
			errs = append(errs, fmt.Errorf("bad recorded group %q", line))
			continue
		}
		alive, err := Existence(sys, -pgid)
		if err != nil {
			errs = append(errs, fmt.Errorf("probe group %d: %w", pgid, err))
		}
		if !alive && err == nil {
			continue
		}
		errs = append(errs, fmt.Errorf("group %d survived the helper; emergency KILL", pgid))
		if err := sys.Signal(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("emergency KILL %d: %w", pgid, err))
		}
		if err := WaitGone(sys, clock, ReapLimit, 5*time.Millisecond, -pgid); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
