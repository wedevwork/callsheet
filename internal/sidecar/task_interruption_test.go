package sidecar

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestTaskInterruption is UT FP-9 of iteration 05 on the worker, its
// assertions replaced by iteration 06a's boundary: task workers belong to
// Run and survive their attachment; the next attachment reconciles them
// before anything of theirs is sent; Run shutdown asks every running
// guardian to clean its group, joins every worker and journals the
// interrupted execution lost. Its disconnect, remaining-capacity and
// recovery-remove subtests are delegated from tests/function
// (TestTaskRecoveryBoundary), together with the plane's. Do not rename
// or skip them.
func TestTaskInterruption(t *testing.T) {
	t.Parallel()
	t.Run("disconnect", func(t *testing.T) {
		t.Parallel()
		t.Run("reconnect-and-shutdown", func(t *testing.T) { t.Parallel(); disconnectAndShutdown(t) })
		t.Run("shutdown-during-start", func(t *testing.T) {
			t.Parallel()
			// Run shutdown while a worker's launch is released but not yet
			// confirmed (its guardian's release in progress): the guardian
			// is asked to clean up exactly once, with the launch completing
			// either before or after the shutdown began, and Run joins it.
			for _, startFirst := range []bool{false, true} {
				fp := startFakePlane(t)
				block := make(chan struct{})
				gate := &gatedFactory{block: block, arrived: make(chan struct{})}
				tr := startTaskRun(t, fp, taskOpts{wrap: gate.wrap})
				ins, run := manuals(t, tr.dir, "a", "i")
				s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
				st := startBody(1, s.cfgs[0], 1, 1, "start in progress")
				s.c.sendStart("p2", st)
				<-gate.arrived
				if startFirst {
					close(block)
					tr.ev.awaitMatch(t, evChildStarted, func(ev event) bool { return ev.id == st.TaskID })
					tr.run.cancel()
				} else {
					tr.run.cancel()
					tr.ev.await(t, evSupervisorClosing)
					close(block)
				}
				tr.run.result(t)
				tr.ledger.mu.Lock()
				gs := append([]*fakeGuardian(nil), tr.ledger.guardians...)
				tr.ledger.mu.Unlock()
				if sig := tr.groups.signals(); len(gs) != 1 || len(sig) != 1 || sig[0] != gs[0].pid {
					t.Fatalf("start first %v: shutdown stopped %v for guardians %d", startFirst, sig, len(gs))
				}
				// The interrupted execution is journaled lost for the next Run.
				lj, err := layout{root: tr.root}.loadJournal(st.TaskID, tr.lookup())
				if err != nil || lj.j.Phase != contract.JournalLost || lj.j.Result.Outcome != contract.OutcomeLost {
					t.Fatalf("start first %v: journal %+v %v", startFirst, lj.j, err)
				}
			}
		})
	})
	t.Run("remaining-capacity", func(t *testing.T) {
		t.Parallel()
		sidecarRemainingCapacity(t, true)
	})
	t.Run("recovery-remove", func(t *testing.T) {
		t.Parallel()
		sidecarRecoveryRemove(t, true)
	})
}

// disconnectAndShutdown is the attachment-loss and shutdown contract.
func disconnectAndShutdown(t *testing.T) {
	// Loss while preparing (no reply ever sent; the preparation completes
	// and its adapter runs, supervised) and loss after the start reply
	// (the child runs on across the reconnect): the next attachment's
	// inventory reports both running and the plane continues them; their
	// output flows there only after reconciliation. Run shutdown stops
	// the still-running group through its guardian, joins its worker and
	// journals it lost; nothing is invented on the wire.
	fp := startFakePlane(t)
	bo := &blockingOpen{arrived: make(chan struct{}), release: make(chan struct{})}
	tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) { d.openManual = bo.open }})
	ins, run := manuals(t, tr.dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	s := tr.connect(t, 1, 1, cfg)
	tr.ev.awaitMatch(t, evCycleDone, nil)
	after, chAfter := s.run(t, 1, 0, "after reply")
	chAfter.prompt(t)
	bo.mu.Lock()
	bo.path = ins
	bo.mu.Unlock()
	preparing := startBody(2, cfg, 1, 1, "preparing")
	s.c.sendStart(s.nextP(), preparing)
	<-bo.arrived
	s.detach(t, false, after.TaskID, preparing.TaskID)
	close(bo.release)
	chPrep := tr.child(t)
	chPrep.prompt(t)
	tr.ev.awaitMatch(t, evChildStarted, func(ev event) bool { return ev.id == preparing.TaskID })
	s, inv := tr.reconnect(t, 2, 2, map[string]string{after.TaskID: contract.ActionContinue, preparing.TaskID: contract.ActionContinue}, cfg)
	if len(inv) != 2 || inv[0].Phase != contract.PhaseRunning || inv[1].Phase != contract.PhaseRunning {
		t.Fatalf("inventory %+v", inv)
	}
	chAfter.out([]byte("after the reconnect\n"))
	chAfter.exitCode(0)
	tr.settled(t, after.TaskID)
	if r, out := s.drain(t, after); string(out) != "after the reconnect\n" || *r.ExitCode != 0 {
		t.Fatalf("reconciled result %+v %q", r, out)
	}
	tr.clk.Advance(heartbeatInterval)
	if hb := s.beat(t, 2); hb.Roles[0].Inflight != 1 {
		t.Fatalf("heartbeat %+v", hb.Roles)
	}
	// Shutdown: the still-running child's group is stopped through its
	// guardian and joined before Run returns; nothing is sent for it.
	tr.run.cancel()
	if err := tr.run.result(t); err == nil {
		t.Fatal("Run returned no error on cancellation")
	}
	if sig := tr.groups.signals(); !slices.Contains(sig, chPrep.gpid) {
		t.Fatalf("shutdown stopped %v, want %d", sig, chPrep.gpid)
	}
	if w := tr.super(t).find(preparing.TaskID); w == nil {
		t.Fatal("the interrupted execution was forgotten")
	} else {
		w.mu.Lock()
		done := w.done
		w.mu.Unlock()
		if !done {
			t.Fatal("Run returned before joining its worker")
		}
	}
	lj, err := layout{root: tr.root}.loadJournal(preparing.TaskID, tr.lookup())
	if err != nil || lj.j.Phase != contract.JournalLost || lj.j.Result.Signal == nil || *lj.j.Result.Signal != "SIGTERM" {
		t.Fatalf("interrupted journal %+v %v", lj.j, err)
	}
}

// TestTaskPlatform is UT FP-4/5/7 for the execution platform seams; its
// platforms subtest is delegated from tests/function
// (TestTaskExecution/platform). Real process groups and pipes belong only
// to TestTaskExecutionContract/process. Do not rename or skip it.
func TestTaskPlatform(t *testing.T) {
	t.Parallel()
	t.Run("platforms", func(t *testing.T) {
		t.Parallel()
		t.Run("group-teardown", func(t *testing.T) { t.Parallel(); realGroupsContract(t) })
		// Linux and Darwin seams on any host, the unsupported rejection,
		// the private scratch directory (permissions, TMPDIR root, unique,
		// physical path on Darwin), its guarded removal, the child
		// environment filter and real pipe endpoints.
		for goos, physical := range map[string]bool{"linux": false, "darwin": true} {
			p, err := taskPlatformFor(goos)
			if err != nil || p.physicalScratch != physical || p.goos != goos {
				t.Fatalf("%s seam = %+v %v", goos, p, err)
			}
		}
		for _, goos := range []string{"windows", "freebsd", ""} {
			if _, err := taskPlatformFor(goos); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("%q accepted: %v", goos, err)
			}
		}
		real := t.TempDir()
		link := filepath.Join(t.TempDir(), "tmp-link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		const id = "t_0123456789abcdef0123456789abcdef"
		linux, _ := taskPlatformFor("linux")
		darwin, _ := taskPlatformFor("darwin")
		dl, err := linux.scratch(link, id)
		if err != nil || filepath.Dir(dl) != link || !filepath.IsAbs(dl) {
			t.Fatalf("linux scratch %s %v", dl, err)
		}
		physical, _ := filepath.EvalSymlinks(real)
		dd, err := darwin.scratch(link, id)
		if err != nil || filepath.Dir(dd) != physical {
			t.Fatalf("darwin scratch %s %v (want under %s)", dd, err, real)
		}
		for _, d := range []string{dl, dd} {
			fi, err := os.Stat(d)
			if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 || !strings.HasPrefix(filepath.Base(d), scratchPrefix+id+"-") {
				t.Fatalf("scratch %s: %v %v", d, fi, err)
			}
		}
		if dl2, _ := linux.scratch(link, id); dl2 == dl {
			t.Fatal("scratch directories are not unique per task")
		}
		if _, err := linux.scratch(filepath.Join(real, "missing"), id); err == nil {
			t.Fatal("an unavailable temp root produced a scratch directory")
		}
		// Removal deletes the known directory, never a symlink's target.
		os.WriteFile(filepath.Join(dl, "f"), []byte("x"), 0o600)
		if err := removeScratch(dl); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dl); !os.IsNotExist(err) {
			t.Fatal("scratch not removed")
		}
		victim := t.TempDir()
		os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600)
		trap := filepath.Join(t.TempDir(), "trap")
		os.Symlink(victim, trap)
		if err := removeScratch(trap); err == nil {
			t.Fatal("a symlinked scratch path was accepted")
		}
		if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
			t.Fatal("removal followed a symlink")
		}
		// Environment: exact fixture descriptor keys (every occurrence)
		// and PWD removed, PWD set last, everything else kept in order.
		env := childEnv([]string{"A=1", "PWD=/x", "CALLSHEET_FAKE_READY_FD=3", "CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD=4", "CALLSHEET_FAKE_READY_FD=5",
			"CALLSHEET_FAKE_READY_FD2=keep", "PWD=/y", "PWDX=keep", "B"}, "/scratch")
		if want := "A=1|CALLSHEET_FAKE_READY_FD2=keep|PWDX=keep|B|PWD=/scratch"; strings.Join(env, "|") != want {
			t.Fatalf("env %q", env)
		}
		// Real pipe endpoints: one per stream, all closable twice.
		p, err := newPipes()
		if err != nil {
			t.Fatal(err)
		}
		go func() { p.stdoutW.Write([]byte("through a pipe")); p.stdoutW.Close() }()
		r := newOutputRing(1 << 10)
		drain(p.stdoutR, r, nil, func() {})
		if string(r.retained()) != "through a pipe" {
			t.Fatalf("drained %q", r.retained())
		}
		p.closeAll()
		p.closeAll()
		// A Run on an unsupported system refuses every start (the seam),
		// before any preparation.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{goos: "plan9"})
		ins, run := manuals(t, tr.dir, "a", "m")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		_, res := s.start(t, 1, 0, "unsupported")
		if res.Err == nil || res.Err.Details["reason"] != contract.ReasonStartFailed {
			t.Fatalf("unsupported start %+v", res)
		}
		tr.noChild(t)
		if entries, _ := os.ReadDir(tr.tmp); len(entries) != 0 {
			t.Fatalf("unsupported start prepared %v", entries)
		}
	})
}
