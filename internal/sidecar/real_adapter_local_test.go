//go:build realadaptercheck && (linux || darwin)

package sidecar

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 08's ordinary-only sidecar contract (build tag
// realadaptercheck: devcheck's tagged test steps, the tagged coverage and
// native streams and the function suite's prebuilt tagged binary run it;
// it never enters a stress shard). The real vendor adapters run with
// probe-free wrappers and the counted injected guardians (zero OS
// processes); the final-file helper runs on the real filesystem.

// countingProbe is a vendor adapter whose probe runs nothing and is
// counted.
type countingProbe struct {
	adapter.Adapter
	n *atomic.Int32
}

func (c countingProbe) Probe(context.Context, string) error {
	c.n.Add(1)
	return nil
}

// vendorRun is a task run with claude, codex and fake enabled (a
// never-executed file for each path) and probe-free adapters.
type vendorRun struct {
	*taskRun
	probes *atomic.Int32
}

func startVendorRun(t *testing.T, fp *fakePlane, goos string, options func(o *RunOptions), adjust func(d *deps)) *vendorRun {
	t.Helper()
	n := &atomic.Int32{}
	exe := fakeExeFile(t)
	reg := func(string) adapter.Registry {
		r, err := adapter.NewRegistry(countingProbe{adapter.NewClaude(""), n}, countingProbe{adapter.NewCodex(""), n}, countingProbe{adapter.NewFake(""), n})
		if err != nil {
			panic(err)
		}
		return r
	}
	tr := startTaskRun(t, fp, taskOpts{goos: goos, adapters: reg, adjust: adjust, options: func(o *RunOptions) {
		o.ClaudeAdapterPath, o.CodexAdapterPath = exe, exe
		if options != nil {
			options(o)
		}
	}})
	return &vendorRun{taskRun: tr, probes: n}
}

// vendorRole is a resolved role of adapter id with its observed pair (an
// explicit test selection, never a default).
func vendorRole(id, roleID, ins, run string) contract.RoleConfig {
	c := roleConfig(roleID, ins, run)
	c.Adapter = id
	for _, q := range adapter.Qualifications() {
		if q.ID == id {
			c.Model, c.Effort = q.Model, q.Effort
		}
	}
	return c
}

// realCapture reads a checked-in qualification capture.
func realCapture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "tests", "testdata", "real-adapters", "linux-2026-09-30", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// finalPathOf returns the final path in a Codex child's argv.
func finalPathOf(t *testing.T, ch *fakeChild) string {
	t.Helper()
	i := slices.Index(ch.spec.argv, "--output-last-message")
	if i < 0 || i+1 >= len(ch.spec.argv) {
		t.Fatalf("codex argv %q has no final path", ch.spec.argv)
	}
	p := ch.spec.argv[i+1]
	if p != filepath.Join(ch.spec.dir, adapter.CodexFinalName) {
		t.Fatalf("final path %s outside the child's scratch %s", p, ch.spec.dir)
	}
	return p
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestRealAdapterLocal is iteration 08's ordinary-only sidecar contract
// (FP-5, FP-6), delegated from tests/function through a prebuilt tagged
// test binary and required by name in the native stream. Do not rename or
// skip its subtests.
func TestRealAdapterLocal(t *testing.T) {
	t.Run("selection", func(t *testing.T) {
		// Explicit absolute vendor paths, validated in Run itself.
		for _, o := range []RunOptions{{ClaudeAdapterPath: "claude"}, {CodexAdapterPath: "./bin/codex"}, {ClaudeAdapterPath: "rel/claude"}} {
			o.StateDir, o.SoftwareVersion = t.TempDir(), "test-1"
			err := testDeps(nil).run(bg, o)
			wantCode(t, err, contract.CodeInvalidArgument, "adapter must be an absolute path to the")
		}
		// The Darwin warning is decided by RunOptions.GOOS: once with a
		// vendor enabled on darwin, never on linux or with only the fake.
		for _, c := range []struct {
			goos   string
			vendor bool
			want   int
		}{{"darwin", true, 1}, {"linux", true, 0}, {"darwin", false, 0}} {
			fp := startFakePlane(t)
			var tr *taskRun
			if c.vendor {
				tr = startVendorRun(t, fp, c.goos, nil, nil).taskRun
			} else {
				tr = startTaskRun(t, fp, taskOpts{goos: c.goos})
			}
			tr.ev.await(t, evStarted)
			if n := strings.Count(tr.logs.String(), DarwinVendorWarning); n != c.want {
				t.Fatalf("%s vendor=%v: %d warnings, want %d:\n%s", c.goos, c.vendor, n, c.want, tr.logs.String())
			}
			if strings.Count(tr.logs.String(), FakeAdapterWarning) != 1 {
				t.Fatal("the fake warning changed")
			}
		}
		// Role validation (design 12a-worker-selection): an alternate valid
		// pair, neither observed nor a default, passes after one probe; the
		// session's contract parse refuses an effort outside the union with
		// the existing contract error before any probe; the manuals precede
		// the selection; a disabled vendor names its own flag.
		fp := startFakePlane(t)
		vr := startVendorRun(t, fp, runtime.GOOS, func(o *RunOptions) { o.CodexAdapterPath = "" }, nil)
		ins, run := manuals(t, vr.dir, "a", "m")
		c := fp.accept(t)
		c.connect()
		alt := vendorRole("claude", "a", ins, run)
		alt.Model, alt.Effort = "claude-opus-5-5", "high"
		c.validate("p1", alt)
		if e := c.result("p1"); e != nil || vr.probes.Load() != 1 {
			t.Fatalf("alternate claude role = %v (%d probes)", e, vr.probes.Load())
		}
		bad := vendorRole("claude", "b", ins, run)
		bad.Effort = "ultra"
		c.validate("p2", bad)
		e := c.result("p2")
		wantResult(t, e, contract.CodeInvalidArgument, "effort", "")
		if !strings.Contains(e.Message, "effort is not allowed for adapter claude; allowed: low, medium, high, xhigh, max") || vr.probes.Load() != 1 {
			t.Fatalf("out-of-union effort %q after %d probes", e.Message, vr.probes.Load())
		}
		missing := vendorRole("claude", "b", "/nonexistent/instruction.md", run)
		missing.Model = "claude-fable-5-1"
		c.validate("p3", missing)
		wantResult(t, c.result("p3"), contract.CodeInvalidArgument, "instruction", contract.ReasonManualUnreadable)
		c.validate("p4", vendorRole("claude", "a", ins, run))
		if e := c.result("p4"); e != nil || vr.probes.Load() != 2 {
			t.Fatalf("observed-pair claude role = %v (%d probes)", e, vr.probes.Load())
		}
		c.validate("p5", vendorRole("codex", "c", ins, run))
		e = c.result("p5")
		wantResult(t, e, contract.CodeInvalidArgument, "adapter", contract.ReasonAdapterDisabled)
		if e.Message != "codex adapter is disabled on node; start sidecar with --codex-adapter ABSOLUTE_PATH" {
			t.Fatalf("disabled codex %q", e.Message)
		}
		// Every ready-check cycle revalidates each role's selection: a
		// persisted role whose effort is outside the union is unready alone.
		stale := vendorRole("claude", "s", ins, run)
		stale.Effort = "none"
		if got := vr.d.checkCycle(bg, vr.super(t).env, records(vendorRole("claude", "a", ins, run), alt, stale)); !slices.Equal(got, []bool{true, true, false}) {
			t.Fatalf("cycle = %v", got)
		}
		// Version transitions, shared probes and independent failures on
		// scripted probes; then the worker's task-start defenses.
		fp = startFakePlane(t)
		vr = startVendorRun(t, fp, runtime.GOOS, nil, nil)
		ins, run = manuals(t, vr.dir, "a", "m")
		selectionLifecycle(t, vr, ins, run)
		overrideStarts(t)
	})
	t.Run("file", func(t *testing.T) {
		// The real no-follow helper on the filesystem.
		scratch := t.TempDir()
		final := filepath.Join(scratch, adapter.CodexFinalName)
		// Ownership: only the scratch directory's fixed basename, and only
		// while nothing exists under it.
		if fin, err := ownFinal(adapter.Invocation{}, scratch); fin != nil || err != nil {
			t.Fatalf("stdout source %v %v", fin, err)
		}
		for _, p := range []string{filepath.Join(scratch, "other.txt"), filepath.Join(t.TempDir(), adapter.CodexFinalName), "callsheet-final.txt",
			scratch + "/./callsheet-final.txt", filepath.Join(scratch, "sub", adapter.CodexFinalName)} {
			if fin, err := ownFinal(adapter.Invocation{FinalFile: p}, scratch); !errors.Is(err, errFinalPath) || fin != nil {
				t.Fatalf("final path %s accepted: %v", p, err)
			}
		}
		for name, prep := range map[string]func(){
			"file":      func() { os.WriteFile(final, []byte("stale"), 0o600) },
			"dangling":  func() { os.Symlink(filepath.Join(scratch, "gone"), final) },
			"directory": func() { os.Mkdir(final, 0o700) },
			"link to file": func() {
				os.WriteFile(filepath.Join(scratch, "t"), nil, 0o600)
				os.Symlink(filepath.Join(scratch, "t"), final)
			},
		} {
			prep()
			if fin, err := ownFinal(adapter.Invocation{FinalFile: final}, scratch); !errors.Is(err, errFinalExists) || fin != nil {
				t.Fatalf("%s: pre-existing final accepted: %v", name, err)
			}
			os.RemoveAll(final)
		}
		if _, err := ownFinal(adapter.Invocation{FinalFile: filepath.Join(scratch, "missing", adapter.CodexFinalName)}, filepath.Join(scratch, "missing")); err == nil {
			t.Fatal("a missing scratch directory accepted")
		}
		codex := adapter.NewCodex("")
		reads, closes := &atomic.Int64{}, &atomic.Int32{}
		ops := finalOps{read: func(fd int, p []byte) (int, error) {
			n, err := unix.Read(fd, p)
			reads.Add(int64(max(n, 0)))
			return n, err
		}, close: func(fd int) error { closes.Add(1); return unix.Close(fd) }}
		read := func(t *testing.T, prep func(), o finalOps) (adapter.FinalMessage, error) {
			t.Helper()
			fin, err := ownFinal(adapter.Invocation{FinalFile: final}, scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer fin.close()
			if prep != nil {
				prep()
			}
			defer os.RemoveAll(final)
			reads.Store(0)
			return o.readFinal(fin, codex.NewFinalExtractor())
		}
		exactly := func(b []byte) func() { return func() { os.WriteFile(final, b, 0o600) } }
		for name, c := range map[string]struct {
			prep       func()
			msg, class string
		}{
			"pong":   {exactly([]byte("pong")), "pong", ""},
			"empty":  {exactly(nil), "", ""},
			"prose":  {exactly(realCapture(t, "runs-scratch/codex-skip-forbidden-home/final.saved.txt")), string(realCapture(t, "runs-scratch/codex-skip-forbidden-home/final.saved.txt")), ""},
			"absent": {nil, "<nil>", adapter.FinalMissing},
			"symlink": {func() {
				os.WriteFile(filepath.Join(scratch, "real"), []byte("x"), 0o600)
				os.Symlink(filepath.Join(scratch, "real"), final)
			}, "<nil>", adapter.FinalUnsafe},
			"outside symlink": {func() { os.Symlink("/etc/hostname", final) }, "<nil>", adapter.FinalUnsafe},
			"fifo":            {func() { unix.Mkfifo(final, 0o600) }, "<nil>", adapter.FinalUnsafe},
			"directory":       {func() { os.Mkdir(final, 0o700) }, "<nil>", adapter.FinalUnsafe},
			"hard link":       {func() { os.WriteFile(final, []byte("x"), 0o600); os.Link(final, filepath.Join(scratch, "second")) }, "<nil>", adapter.FinalUnsafe},
			"invalid utf8":    {exactly([]byte("po\xffng")), "<nil>", adapter.FinalInvalid},
			"oversize":        {exactly(make([]byte, adapter.MaxVendorFinalBytes+4096)), "<nil>", adapter.FinalTooLarge},
		} {
			closes.Store(0)
			fm, err := read(t, c.prep, ops)
			os.Remove(filepath.Join(scratch, "second"))
			os.Remove(filepath.Join(scratch, "real"))
			if strOf(fm.Message) != c.msg || fm.Error != c.class || (c.class == "") != (err == nil) {
				t.Fatalf("%s: %q %q %v", name, strOf(fm.Message), fm.Error, err)
			}
			if err != nil && (err.Error() != c.class || strings.Contains(err.Error(), scratch)) {
				t.Fatalf("%s: error %q is not the bare classification", name, err)
			}
			// Bounded I/O: never more than 8 MiB + 1 bytes; every opened
			// file closed exactly once.
			if reads.Load() > adapter.MaxVendorFinalBytes+1 {
				t.Fatalf("%s: read %d bytes", name, reads.Load())
			}
			if opened := c.class != adapter.FinalMissing && name != "symlink" && name != "outside symlink"; opened && closes.Load() != 1 || !opened && closes.Load() != 0 {
				t.Fatalf("%s: %d closes", name, closes.Load())
			}
		}
		// Exactly 8 MiB is read whole (a truncated 64 KiB answer).
		fm, err := read(t, exactly(bytes.Repeat([]byte("z"), adapter.MaxVendorFinalBytes)), ops)
		if err != nil || !fm.Truncated || len(*fm.Message) != contract.MaxFinalMessageBytes || reads.Load() != adapter.MaxVendorFinalBytes {
			t.Fatalf("8 MiB: %v %v %d", err, fm.Truncated, reads.Load())
		}
		// An unreadable file (as a non-root user) and a failing read are
		// final_output_unreadable, never an empty answer.
		if os.Geteuid() != 0 {
			fm, err := read(t, func() { os.WriteFile(final, []byte("secret"), 0o000) }, ops)
			if fm.Message != nil || fm.Error != adapter.FinalUnreadable || err == nil {
				t.Fatalf("unreadable: %+v %v", fm, err)
			}
		}
		fm, err = read(t, exactly([]byte("pong")), finalOps{read: func(int, []byte) (int, error) { return 0, unix.EIO }, close: unix.Close})
		if fm.Message != nil || fm.Error != adapter.FinalUnreadable || err == nil {
			t.Fatalf("read error: %+v %v", fm, err)
		}
		// The retained directory handle defeats parent-path replacement:
		// the scratch path now names another directory with another file.
		fin, err := ownFinal(adapter.Invocation{FinalFile: final}, scratch)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(final, []byte("original"), 0o600)
		moved := scratch + ".moved"
		if err := os.Rename(scratch, moved); err != nil {
			t.Fatal(err)
		}
		os.Mkdir(scratch, 0o700)
		os.WriteFile(final, []byte("replacement"), 0o600)
		fm, err = readFinalFile(fin, codex.NewFinalExtractor())
		fin.close()
		fin.close() // idempotent
		os.RemoveAll(moved)
		if err != nil || strOf(fm.Message) != "original" {
			t.Fatalf("after replacement: %q %v", strOf(fm.Message), err)
		}
	})
	t.Run("ordering", func(t *testing.T) {
		// The role checks keep their order with an alternate selection
		// (design 12a-worker-selection): manuals, selection, probe.
		{
			fp := startFakePlane(t)
			vr := startVendorRun(t, fp, runtime.GOOS, nil, nil)
			ins, run := manuals(t, vr.dir, "a", "m")
			checkOrder(t, vr, ins, run)
		}
		// Extraction happens after the group is gone and before the
		// journal-owned work directory is removed; stdout is never the
		// answer.
		type readAt struct {
			present bool
			cleaned int
		}
		var mu sync.Mutex
		var seen []readAt
		var work string
		var vr *vendorRun
		fp := startFakePlane(t)
		vr = startVendorRun(t, fp, runtime.GOOS, nil, func(d *deps) {
			d.taskFinalRead = func(fin *finalSource, ext adapter.FinalExtractor) (adapter.FinalMessage, error) {
				mu.Lock()
				dir := work
				mu.Unlock()
				fi, err := os.Stat(dir)
				present := dir != "" && err == nil && fi.IsDir()
				vr.groups.mu.Lock()
				cleaned := len(vr.groups.cleaned)
				vr.groups.mu.Unlock()
				mu.Lock()
				seen = append(seen, readAt{present: present, cleaned: cleaned})
				mu.Unlock()
				return readFinalFile(fin, ext)
			}
		})
		ins, run := manuals(t, vr.dir, "a", "m")
		s := vr.connect(t, 1, 1, vendorRole("codex", "a", ins, run))
		stdout := realCapture(t, "runs-scratch/codex-skip-success/stdout.bin")
		st, ch := s.run(t, 1, 0, "codex")
		mu.Lock()
		work = ch.spec.dir
		mu.Unlock()
		if prompt := ch.prompt(t); !bytes.Contains(prompt, []byte(`"goal":"codex"`)) {
			t.Fatalf("prompt %q", prompt)
		}
		os.WriteFile(finalPathOf(t, ch), []byte("pong"), 0o600)
		ch.out(stdout)
		ch.exitCode(0)
		res, logs := s.drain(t, st)
		if strOf(res.FinalMessage) != "pong" || res.ExitCode == nil || *res.ExitCode != 0 || !bytes.Equal(logs, stdout) || res.OutputBytes != len(stdout) {
			t.Fatalf("codex result %+v logs %q", res, logs)
		}
		mu.Lock()
		if len(seen) != 1 || !seen[0].present || seen[0].cleaned != 1 {
			t.Fatalf("read order %+v (scratch %s)", seen, ch.spec.dir)
		}
		mu.Unlock()
		if _, err := os.Stat(ch.spec.dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scratch kept after cleanup: %v", err)
		}
		// Two concurrent tasks never exchange files.
		st2, ch2 := s.run(t, 2, 0, "two")
		st3, ch3 := s.run(t, 3, 0, "three")
		if ch2.spec.dir == ch3.spec.dir || finalPathOf(t, ch2) == finalPathOf(t, ch3) {
			t.Fatal("two tasks share a final path")
		}
		os.WriteFile(finalPathOf(t, ch3), []byte("answer three"), 0o600)
		os.WriteFile(finalPathOf(t, ch2), []byte("answer two"), 0o600)
		ch3.exitCode(0)
		vr.settled(t, st3.TaskID)
		ch2.exitCode(0)
		vr.settled(t, st2.TaskID)
		// Results are offered one exchange at a time, in completion order.
		r3 := s.result(t, st3)
		r2 := s.result(t, st2)
		if strOf(r2.FinalMessage) != "answer two" || strOf(r3.FinalMessage) != "answer three" {
			t.Fatalf("isolation: %q %q", strOf(r2.FinalMessage), strOf(r3.FinalMessage))
		}
		// A cancellation with confirmed group absence keeps its control
		// outcome and extracts the safe final file present by then.
		st4, ch4 := s.run(t, 4, 0, "cancel me")
		ch4.prompt(t)
		os.WriteFile(finalPathOf(t, ch4), []byte("partial answer"), 0o600)
		in := stopOf("c")
		s.sendCancel(t, st4, in)
		vr.settled(t, st4.TaskID)
		r4 := s.result(t, st4)
		if r4.Outcome != contract.OutcomeCancelled || r4.StopID == nil || *r4.StopID != in.ID || strOf(r4.FinalMessage) != "partial answer" {
			t.Fatalf("cancelled %+v final %q", r4, strOf(r4.FinalMessage))
		}
		// A start failure closes the handle and removes the scratch
		// directory through the refusal path, expecting no final file.
		vr.ledger.mu.Lock()
		vr.ledger.adapterErr = errUnsupported
		vr.ledger.mu.Unlock()
		if _, r := s.start(t, 5, 0, "refused"); r.Err == nil {
			t.Fatal("a failed adapter start was accepted")
		}
		vr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == taskID(5) })
		if entries, _ := os.ReadDir(vr.tmp); len(entries) != 0 {
			t.Fatalf("refused start left %v", entries)
		}
		// Unconfirmed cleanup: no read is attempted, the answer is null
		// with final_output_unavailable and the scratch directory stays.
		fp = startFakePlane(t)
		reads := &atomic.Int32{}
		vu := startVendorRun(t, fp, runtime.GOOS, nil, func(d *deps) {
			d.taskFinalRead = func(fin *finalSource, ext adapter.FinalExtractor) (adapter.FinalMessage, error) {
				reads.Add(1)
				return readFinalFile(fin, ext)
			}
		})
		vu.groups.err = errors.New("the group is still present")
		ins, run = manuals(t, vu.dir, "a", "m")
		su := vu.connect(t, 1, 1, vendorRole("codex", "a", ins, run))
		stu, chu := su.run(t, 1, 0, "unconfirmed")
		os.WriteFile(finalPathOf(t, chu), []byte("unsafe to read"), 0o600)
		chu.exitCode(0)
		ru, lu := su.drain(t, stu)
		if ru.FinalMessage != nil || reads.Load() != 0 || string(lu) != "callsheet: codex final-message extraction failed (final_output_unavailable)\n" {
			t.Fatalf("unconfirmed %+v logs %q (%d reads)", ru, lu, reads.Load())
		}
		if _, err := os.Stat(finalPathOf(t, chu)); err != nil {
			t.Fatalf("scratch removed without confirmed cleanup: %v", err)
		}
	})
	t.Run("diagnostic", func(t *testing.T) {
		fp := startFakePlane(t)
		vr := startVendorRun(t, fp, runtime.GOOS, nil, nil)
		ins, run := manuals(t, vr.dir, "a", "m")
		fake := roleConfig("f", ins, run)
		s := vr.connect(t, 1, 1, vendorRole("claude", "a", ins, run), vendorRole("codex", "b", ins, run), fake)
		diag := func(id, code string) string {
			return "callsheet: " + id + " final-message extraction failed (" + code + ")\n"
		}
		for i, c := range []struct {
			role              int
			stdout, stderr    []byte
			exit              int
			final             func(p string)
			wantFinal, wantDx string
		}{
			// Malformed Claude stdout with exit 0: succeeded, null, one line.
			{0, []byte("not json\n"), nil, 0, nil, "<nil>", diag("claude", adapter.FinalInvalid)},
			// The captured failure: its exact result and exit 1, no line.
			{0, realCapture(t, "runs-scratch/claude-stdin-fail/stdout.bin"), nil, 1, nil,
				"There's an issue with the selected model (callsheet-no-such-model). It may not exist or you may not have access to it. Run --model to pick a different model.", ""},
			// The captured Codex failure: no file, exit 1 kept, one line.
			{1, realCapture(t, "runs-scratch/codex-skip-fail/stdout.bin"), nil, 1, nil, "<nil>", diag("codex", adapter.FinalMissing)},
			// An unsafe final file after exit 0.
			{1, nil, []byte("warn\n"), 0, func(p string) { os.Symlink("/etc/hostname", p) }, "<nil>", diag("codex", adapter.FinalUnsafe)},
			// The fake's absent marker: no line, no warning.
			{2, []byte("progress only\n"), nil, 0, nil, "<nil>", ""},
		} {
			st, ch := s.run(t, 10+i, c.role, "case")
			if c.final != nil {
				c.final(finalPathOf(t, ch))
			}
			ch.out(c.stdout)
			ch.errOut(c.stderr)
			ch.exitCode(c.exit)
			res, logs := s.drain(t, st)
			want := string(c.stdout) + string(c.stderr) + c.wantDx
			if strOf(res.FinalMessage) != c.wantFinal || string(logs) != want || res.OutputBytes != len(want) || res.ExitCode == nil || *res.ExitCode != c.exit ||
				res.Outcome != contract.OutcomeNatural {
				t.Fatalf("case %d: %+v final %q logs %q", i, res, strOf(res.FinalMessage), logs)
			}
			warned := hasLog(vr.logs.String(), "task final-message extraction failed", "task_id", st.TaskID)
			if warned != (c.wantDx != "") {
				t.Fatalf("case %d: structured warning %v:\n%s", i, warned, vr.logs.String())
			}
			if c.wantDx != "" {
				id := s.cfgs[c.role].Adapter
				code := strings.TrimSuffix(strings.TrimPrefix(c.wantDx, "callsheet: "+id+" final-message extraction failed ("), ")\n")
				if !hasLog(vr.logs.String(), "task final-message extraction failed", "adapter", id) || !hasLog(vr.logs.String(), "task final-message extraction failed", "classification", code) {
					t.Fatalf("case %d: warning attributes:\n%s", i, vr.logs.String())
				}
			}
		}
	})
	t.Run("restart", func(t *testing.T) {
		// A sealed outcome is immutable: its retransmission and a
		// restarted Run's recovery offer the same digest, final bytes and
		// journaled tail (the diagnostic line once), without the scratch
		// directory, which is gone.
		for _, c := range []struct {
			role              string
			write             bool
			stdout            string
			wantFinal, wantLg string
		}{
			{"claude", false, "garbage", "<nil>", "garbage" + "callsheet: claude final-message extraction failed (invalid_final_output)\n"},
			{"codex", true, "{\"type\":\"turn.completed\"}\n", "pong", "{\"type\":\"turn.completed\"}\n"},
		} {
			fp := startFakePlane(t)
			vr := startVendorRun(t, fp, runtime.GOOS, nil, nil)
			ins, run := manuals(t, vr.dir, "a", "m")
			cfg := vendorRole(c.role, "a", ins, run)
			s := vr.connect(t, 1, 1, cfg)
			st, ch := s.run(t, 1, 0, "restart")
			if c.write {
				os.WriteFile(finalPathOf(t, ch), []byte("pong"), 0o600)
			}
			ch.out([]byte(c.stdout))
			ch.exitCode(0)
			// The outcome is frozen (journaled with the unsent tail) before
			// evTaskExited: acknowledging the logs only after it keeps that
			// tail in the journal the restarted Run replays.
			vr.settled(t, st.TaskID)
			if got := s.logs(t, st, len(c.wantLg)); string(got) != c.wantLg {
				t.Fatalf("%s logs %q", c.role, got)
			}
			first := s.resultAck(t, st, false)
			if err := vr.clk.AwaitWaiter(testWait, testkit.HasTimer(resultRetry)); err != nil {
				t.Fatal(err)
			}
			vr.clk.Advance(resultRetry)
			again := s.resultAck(t, st, false)
			if again.Digest != first.Digest || strOf(first.FinalMessage) != c.wantFinal || strOf(again.FinalMessage) != c.wantFinal {
				t.Fatalf("%s retry %+v then %+v", c.role, first, again)
			}
			if _, err := os.Stat(ch.spec.dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch kept: %v", err)
			}
			jb, err := os.ReadFile(filepath.Join(vr.root, journalDir, st.TaskID, executionName))
			if err != nil {
				t.Fatal(err)
			}
			j, err := contract.ParseExecutionJournal(jb, adapter.Lookup())
			if err != nil || j.Result == nil || j.Result.Digest != first.Digest || strings.Count(string(j.Log.Data), "final-message extraction failed") > 1 {
				t.Fatalf("journal %+v", j)
			}
			// A restarted Run reports the journaled outcome only.
			vr.run.cancel()
			select {
			case err := <-vr.run.done:
				vr.run.done <- err // the helper's cleanup joins it again
			case <-time.After(testWait):
				t.Fatal("the first Run did not stop")
			}
			root := vr.root
			exe := fakeExeFile(t)
			n := &atomic.Int32{}
			tr2 := startTaskRun(t, fp, taskOpts{root: root, adapters: func(string) adapter.Registry {
				r, _ := adapter.NewRegistry(countingProbe{adapter.NewClaude(""), n}, countingProbe{adapter.NewCodex(""), n})
				return r
			}, options: func(o *RunOptions) { o.ClaudeAdapterPath, o.CodexAdapterPath = exe, exe }})
			s2, entries := tr2.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionSendResult}, cfg)
			if len(entries) != 1 || entries[0].TaskID != st.TaskID {
				t.Fatalf("inventory %+v", entries)
			}
			res, logs := s2.drain(t, st)
			if res.Digest != first.Digest || strOf(res.FinalMessage) != c.wantFinal || string(logs) != c.wantLg {
				t.Fatalf("%s recovered %+v logs %q", c.role, res, logs)
			}
			tr2.noChild(t)
		}
	})
	// Iteration 11 (wave 2: Grok and Cursor), direct subtests of this
	// contract in real_adapter_wave2_local_test.go; required by name in the
	// native stream.
	t.Run("wave2-posture", wave2Posture)
	t.Run("wave2-invocation", wave2Invocation)
	t.Run("wave2-outcomes", wave2Outcomes)
	t.Run("wave2-retry", wave2Retry)
}

// hasLog reports a JSON log record with msg whose attribute key is value.
func hasLog(logs, msg, key, value string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"msg":"`+msg+`"`) && strings.Contains(line, `"`+key+`":"`+value+`"`) {
			return true
		}
	}
	return false
}

// BenchmarkRealAdapterFile measures the final-file helper over a real
// 8 MiB file and an oversized one: bytes consumed (at most 8 MiB + 1), the
// exact result or classification and the file's closure every iteration.
func BenchmarkRealAdapterFile(b *testing.B) {
	for _, c := range []struct {
		name  string
		size  int
		read  int64
		check func(adapter.FinalMessage, error) bool
	}{
		{"8MiB", adapter.MaxVendorFinalBytes, adapter.MaxVendorFinalBytes, func(fm adapter.FinalMessage, err error) bool {
			return err == nil && fm.Truncated && len(*fm.Message) == contract.MaxFinalMessageBytes
		}},
		{"oversized", adapter.MaxVendorFinalBytes + 64<<10, adapter.MaxVendorFinalBytes + 1, func(fm adapter.FinalMessage, err error) bool {
			return err != nil && fm.Message == nil && fm.Error == adapter.FinalTooLarge
		}},
	} {
		b.Run(c.name, func(b *testing.B) {
			scratch := b.TempDir()
			final := filepath.Join(scratch, adapter.CodexFinalName)
			if err := os.WriteFile(final, bytes.Repeat([]byte("q"), c.size), 0o600); err != nil {
				b.Fatal(err)
			}
			dir, err := openFinalDir(scratch)
			if err != nil {
				b.Fatal(err)
			}
			fin := &finalSource{dir: dir, name: adapter.CodexFinalName}
			defer fin.close()
			var read int64
			var closes int
			ops := finalOps{read: func(fd int, p []byte) (int, error) {
				n, err := unix.Read(fd, p)
				read += int64(max(n, 0))
				return n, err
			}, close: func(fd int) error { closes++; return unix.Close(fd) }}
			codex := adapter.NewCodex("")
			b.SetBytes(c.read)
			b.ReportAllocs()
			n := 0
			for b.Loop() {
				n++
				read = 0
				fm, err := ops.readFinal(fin, codex.NewFinalExtractor())
				if !c.check(fm, err) || read != c.read || closes != n {
					b.Fatalf("%s: %+v %v read %d closes %d", c.name, fm.Error, err, read, closes)
				}
			}
		})
	}
}
