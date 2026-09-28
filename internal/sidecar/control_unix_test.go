//go:build linux || darwin

package sidecar

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The guardian's private entrypoint in process (TestControlLaunch): real
// pipes for its fixed descriptors, a real FIFO and owner record in a
// private task directory, an injected adapter start, TERM notification,
// group signaler and clock. No process is started and no real signal is
// sent.

// guardianPipes are one guardian's descriptor pipes: the guardian's ends
// (fds) and the parent's.
type guardianPipes struct {
	fds                                          guardianFDs
	invW, lifeW, relW, statR, stdinW, outR, errR *os.File
}

func newGuardianPipes(t *testing.T) *guardianPipes {
	t.Helper()
	p := &guardianPipes{}
	pipe := func() (*os.File, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		return r, w
	}
	p.fds.inv, p.invW = pipe()
	p.fds.life, p.lifeW = pipe()
	p.fds.release, p.relW = pipe()
	p.statR, p.fds.status = pipe()
	p.fds.stdin, p.stdinW = pipe()
	p.outR, p.fds.stdout = pipe()
	p.errR, p.fds.stderr = pipe()
	return p
}

// status reads the guardian's next status message.
func (p *guardianPipes) status(t *testing.T) contract.GuardianStatus {
	t.Helper()
	type res struct {
		s   contract.GuardianStatus
		err error
	}
	c := make(chan res, 1)
	go func() {
		b, err := contract.ReadFramed(p.statR, contract.MaxGuardianStatusBytes)
		if err != nil {
			c <- res{err: err}
			return
		}
		s, err := contract.ParseGuardianStatus(b)
		c <- res{s, err}
	}()
	select {
	case r := <-c:
		if r.err != nil {
			t.Fatalf("guardian status: %v", r.err)
		}
		return r.s
	case <-time.After(testWait):
		t.Fatal("no guardian status")
		return contract.GuardianStatus{}
	}
}

// fakeRunAdapter is an injected adapter the guardian started.
type fakeRunAdapter struct {
	pid  int
	exit chan procExit
}

func (a *fakeRunAdapter) PID() int       { return a.pid }
func (a *fakeRunAdapter) Wait() procExit { return <-a.exit }

// termGroup is a scripted guardian group: TERM ends the adapter
// (signaled) and the group when cooperative; KILL ends it.
type termGroup struct {
	scriptedGroup
	mu      sync.Mutex
	adapter *fakeRunAdapter
}

func (g *termGroup) Signal(pid int, sig syscall.Signal) error {
	err := g.scriptedGroup.Signal(pid, sig)
	g.mu.Lock()
	a := g.adapter
	g.mu.Unlock()
	if a != nil && (sig == syscall.SIGTERM || sig == syscall.SIGKILL) {
		select {
		case a.exit <- procExit{signal: map[syscall.Signal]string{syscall.SIGTERM: "SIGTERM", syscall.SIGKILL: "SIGKILL"}[sig]}:
		default:
		}
	}
	return err
}

// guardianRig is one in-process guardian over a prepared task directory.
type guardianRig struct {
	p       *guardianPipes
	env     guardianEnv
	inv     contract.GuardianInvocation
	dir     string
	diag    *syncLog
	group   *termGroup
	started chan *fakeRunAdapter
	code    chan int
}

const rigPID = 4242

func newGuardianRig(t *testing.T, phase string) *guardianRig {
	t.Helper()
	root := t.TempDir()
	script := newScript()
	lookup := adapter.ContractLookup(script.registry())
	st := startBody(1, roleConfig("a", "/srv/i.md", "/srv/r.md"), 1, 1, "guarded")
	j := journalOf(st, phase)
	if phase == contract.JournalPrepared {
		j.OwnerNonce, j.StartedAt = nil, nil
	}
	writeJournal(t, root, j, nil)
	dir := filepath.Join(root, journalDir, st.TaskID)
	r := &guardianRig{p: newGuardianPipes(t), dir: dir, diag: newSyncLog(), group: &termGroup{scriptedGroup: scriptedGroup{alive: true, cooperative: true}},
		started: make(chan *fakeRunAdapter, 1), code: make(chan int, 1)}
	r.inv = contract.GuardianInvocation{Version: contract.GuardianInvocationVersion, TaskID: st.TaskID, Execution: st.Execution, StartDigest: st.StartDigestHex(),
		TaskDir: dir, Nonce: ctlNonce, Path: "/opt/fake/adapter", Argv: []string{"run"}, Env: []string{"A=1"}, Dir: root, Timeout: "0s",
		TimeoutPolicy: contract.TimeoutPolicyEnforced}
	r.env = guardianEnv{
		fds:     func() (guardianFDs, error) { return r.p.fds, nil },
		getpid:  func() int { return rigPID },
		getpgrp: func() int { return rigPID },
		mkfifo:  func(p string) error { return unix.Mkfifo(p, 0o600) },
		startAdapter: func(inv contract.GuardianInvocation, stdin, stdout, stderr *os.File) (adapterRun, error) {
			a := &fakeRunAdapter{pid: rigPID + 1, exit: make(chan procExit, 1)}
			r.group.mu.Lock()
			r.group.adapter = a
			r.group.mu.Unlock()
			r.started <- a
			return a, nil
		},
		notifyTerm: func(chan<- os.Signal) func() { return func() {} },
		sig:        r.group,
		clock:      &stepClock{now: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)},
		grace:      groupGrace,
		now:        func() time.Time { return time.Date(2026, 9, 27, 0, 0, 1, 0, time.UTC) },
		lookup:     lookup,
	}
	return r
}

// run starts the guardian and delivers its invocation (raw, when set).
func (r *guardianRig) run(t *testing.T, raw []byte) {
	t.Helper()
	go func() { r.code <- runGuardian([]string{GuardianToken}, r.diag, r.env) }()
	if raw == nil {
		b, err := contract.EncodeGuardianInvocation(r.inv)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		contract.WriteFramed(&buf, b)
		raw = buf.Bytes()
	}
	go func() {
		r.p.invW.Write(raw)
		r.p.invW.Close()
	}()
}

func (r *guardianRig) exit(t *testing.T) int {
	t.Helper()
	select {
	case c := <-r.code:
		return c
	case <-time.After(testWait):
		t.Fatal("the guardian did not return")
		return -1
	}
}

func (r *guardianRig) owner(t *testing.T) *contract.OwnerRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, ownerName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	o, err := contract.ParseOwner(b)
	if err != nil {
		t.Fatal(err)
	}
	return &o
}

// command writes one line to the rig's control FIFO.
func (r *guardianRig) command(t *testing.T, line []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(r.dir, controlName), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		t.Fatal(err)
	}
}

func (r *guardianRig) stopCommand(t *testing.T, nonce string) []byte {
	t.Helper()
	b, err := contract.EncodeGuardianCommand(contract.GuardianCommand{TaskID: r.inv.TaskID, Execution: r.inv.Execution, Nonce: nonce, Command: contract.GuardianStop})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// guardianEntrypoint is TestControlLaunch's entrypoint and descriptor
// matrix.
func guardianEntrypoint(t *testing.T) {
	t.Run("lifecycle", func(t *testing.T) {
		t.Parallel()
		// Ownership is durable (owner.json armed, the FIFO) before ready;
		// released is durable before the adapter starts; the adapter's
		// exact wait status is forwarded; then TERM goes to the guardian's
		// own group, never another.
		r := newGuardianRig(t, contract.JournalPrepared)
		r.run(t, nil)
		ready := r.p.status(t)
		if ready.Type != contract.GuardianReady || ready.PID != rigPID || ready.PGID != rigPID || ready.Nonce != ctlNonce {
			t.Fatalf("ready %+v", ready)
		}
		if o := r.owner(t); o == nil || o.Phase != contract.OwnerArmed || o.PGID != rigPID || o.Nonce != ctlNonce {
			t.Fatalf("owner before release %+v", o)
		}
		if fi, err := os.Lstat(filepath.Join(r.dir, controlName)); err != nil || fi.Mode()&os.ModeNamedPipe == 0 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("control FIFO %v %v", fi, err)
		}
		if ids, err := (layout{root: filepath.Dir(filepath.Dir(r.dir))}).scanJournals(); err != nil || len(ids) != 1 {
			t.Fatalf("the armed task directory does not scan: %v", err)
		}
		r.p.relW.Write([]byte{0x01})
		r.p.relW.Close()
		a := <-r.started
		if o := r.owner(t); o.Phase != contract.OwnerReleased {
			t.Fatalf("owner after release %+v", o)
		}
		started := r.p.status(t)
		if started.Type != contract.GuardianStarted || started.PID != a.pid || started.StartedAt == nil {
			t.Fatalf("started %+v", started)
		}
		a.exit <- procExit{code: 3}
		ex := r.p.status(t)
		if ex.Type != contract.GuardianExit || ex.ExitCode == nil || *ex.ExitCode != 3 {
			t.Fatalf("exit %+v", ex)
		}
		if code := r.exit(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		if sig := r.group.signals(); len(sig) == 0 || sig[0] != syscall.SIGTERM {
			t.Fatalf("group cleanup %v", sig)
		}
	})
	t.Run("never-released", func(t *testing.T) {
		t.Parallel()
		// Premature EOF, another byte, the parent's death or a stop
		// command before release: never an adapter.
		for name, act := range map[string]func(r *guardianRig){
			"eof":         func(r *guardianRig) { r.p.relW.Close() },
			"other-byte":  func(r *guardianRig) { r.p.relW.Write([]byte{0x02}); r.p.relW.Close() },
			"extra-data":  func(r *guardianRig) { r.p.relW.Write([]byte{0x01, 0x01}); r.p.relW.Close() },
			"parent-gone": func(r *guardianRig) { r.p.lifeW.Close() },
			"stop":        nil,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newGuardianRig(t, contract.JournalPrepared)
				r.run(t, nil)
				r.p.status(t)
				if act == nil {
					r.command(t, r.stopCommand(t, ctlNonce))
				} else {
					act(r)
				}
				if code := r.exit(t); code != 0 {
					t.Fatalf("exit %d", code)
				}
				select {
				case <-r.started:
					t.Fatal("an adapter started without its release")
				default:
				}
				if o := r.owner(t); o == nil || o.Phase != contract.OwnerArmed {
					t.Fatalf("owner %+v", o)
				}
			})
		}
	})
	t.Run("commands", func(t *testing.T) {
		t.Parallel()
		// Malformed, overlong or foreign commands are drained and
		// reported, never executed; a valid stop after the launch starts
		// the group cleanup (TERM to its own group), and the adapter's
		// signaled wait status is forwarded.
		r := newGuardianRig(t, contract.JournalPrepared)
		r.run(t, nil)
		r.p.status(t)
		other := strings.Repeat("1", 64)
		r.command(t, []byte("stop\n"))
		r.command(t, append(bytes.Repeat([]byte("x"), 3*contract.MaxGuardianCommandBytes), '\n'))
		r.command(t, r.stopCommand(t, other))
		r.p.relW.Write([]byte{0x01})
		r.p.relW.Close()
		<-r.started
		r.p.status(t)
		r.diag.await(t, "ignored commands", func(s string) bool {
			return strings.Count(s, "was ignored") >= 3 && strings.Contains(s, "overlong")
		})
		if len(r.group.signals()) != 0 {
			t.Fatal("an invalid command signaled")
		}
		r.command(t, r.stopCommand(t, ctlNonce))
		// A cause-less stop (06a's recovery form) latches lost (iteration
		// 06b: reported before the group KILL), then the exit is forwarded.
		if st := r.p.status(t); st.Type != contract.GuardianStopping || *st.Cause != contract.CauseLost || st.StopID != nil {
			t.Fatalf("stopping %+v", st)
		}
		ex := r.p.status(t)
		if ex.Type != contract.GuardianExit || ex.Signal == nil || *ex.Signal != "SIGTERM" {
			t.Fatalf("exit %+v", ex)
		}
		if code := r.exit(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		if sig := r.group.signals(); sig[0] != syscall.SIGTERM {
			t.Fatalf("signals %v", sig)
		}
	})
	t.Run("fifo-failed", func(t *testing.T) {
		t.Parallel()
		r := newGuardianRig(t, contract.JournalPrepared)
		r.env.mkfifo = func(string) error { return errors.New("injected mkfifo failure") }
		r.run(t, nil)
		if s := r.p.status(t); s.Type != contract.GuardianError || *s.Reason != guardianFIFOFailed {
			t.Fatalf("status %+v", s)
		}
		if code := r.exit(t); code != 1 || r.owner(t) != nil {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		// Every invalid invocation exits 2 with a bounded diagnostic
		// before any mutation (no FIFO, no owner record, no status).
		huge := make([]byte, 4)
		binary.BigEndian.PutUint32(huge, contract.MaxGuardianInvocationBytes+1)
		framed := func(b []byte) []byte {
			var buf bytes.Buffer
			contract.WriteFramed(&buf, b)
			return buf.Bytes()
		}
		for name, c := range map[string]struct {
			args  []string
			phase string
			raw   []byte
			mut   func(r *guardianRig)
			want  string
		}{
			"extra argv": {args: []string{GuardianToken, "x"}, want: "reserved token"},
			"no token":   {args: []string{"guardian"}, want: "reserved token"},
			"missing fd": {mut: func(r *guardianRig) {
				r.env.fds = func() (guardianFDs, error) { return guardianFDs{}, errors.New("fd 5 (release) is missing") }
			}, want: "invalid descriptors"},
			"not leader":   {mut: func(r *guardianRig) { r.env.getpgrp = func() int { return 77 } }, want: "process group"},
			"pid 1":        {mut: func(r *guardianRig) { r.env.getpid = func() int { return 1 }; r.env.getpgrp = func() int { return 1 } }, want: "process group"},
			"overflow":     {raw: huge, want: "invalid invocation"},
			"malformed":    {raw: framed([]byte("{nope")), want: "invalid invocation"},
			"trailing":     {raw: append(framed([]byte("{}")), 'x'), want: "invalid invocation"},
			"short":        {raw: []byte{0, 0}, want: "invalid invocation"},
			"not prepared": {phase: contract.JournalRunning, want: "invalid task directory"},
			"identity":     {mut: func(r *guardianRig) { r.inv.StartDigest = strings.Repeat("9", 64) }, want: "invalid task directory"},
			"owner exists": {mut: func(r *guardianRig) {
				ob, _ := contract.EncodeOwner(contract.OwnerRecord{TaskID: r.inv.TaskID, Execution: r.inv.Execution, Nonce: ctlNonce, PGID: 9, GuardianPID: 9, Phase: contract.OwnerArmed})
				os.WriteFile(filepath.Join(r.dir, ownerName), ob, 0o600)
			}, want: "invalid task directory"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				phase := c.phase
				if phase == "" {
					phase = contract.JournalPrepared
				}
				r := newGuardianRig(t, phase)
				if c.mut != nil {
					c.mut(r)
				}
				args := c.args
				if args == nil {
					args = []string{GuardianToken}
				}
				raw := c.raw
				if raw == nil {
					b, _ := contract.EncodeGuardianInvocation(r.inv)
					raw = framed(b)
				}
				go func() { r.p.invW.Write(raw); r.p.invW.Close() }()
				if code := runGuardian(args, r.diag, r.env); code != 2 {
					t.Fatalf("exit %d", code)
				}
				out := r.diag.String()
				if !strings.Contains(out, c.want) || len(out) > maxGuardianDiag+64 {
					t.Fatalf("diagnostic %q", out)
				}
				if _, err := os.Lstat(filepath.Join(r.dir, controlName)); err == nil {
					t.Fatal("an invalid invocation created the FIFO")
				}
				if name != "owner exists" && r.owner(t) != nil {
					t.Fatal("an invalid invocation wrote an owner record")
				}
				select {
				case <-r.started:
					t.Fatal("an adapter started")
				default:
				}
			})
		}
	})
	t.Run("descriptors", func(t *testing.T) {
		t.Parallel()
		// The fixed table 3-9: each present, a pipe, the right end, no
		// two the same pipe; all made close-on-exec.
		type fd struct {
			mode int
			ino  uint64
			typ  uint32
			gone bool
		}
		good := func() map[int]*fd {
			m := map[int]*fd{}
			for _, l := range guardianFDLayout {
				mode := unix.O_RDONLY
				if l.write {
					mode = unix.O_WRONLY
				}
				m[l.fd] = &fd{mode: mode, ino: uint64(100 + l.fd), typ: unix.S_IFIFO}
			}
			return m
		}
		check := func(m map[int]*fd) (guardianFDs, error, []int) {
			var cloexec []int
			fds, err := checkFDs(func(f uintptr, cmd, arg int) (int, error) {
				d := m[int(f)]
				if d == nil || d.gone {
					return 0, unix.EBADF
				}
				return d.mode, nil
			}, func(f int, st *unix.Stat_t) error {
				d := m[f]
				setStatMode(st, d.typ|0o600)
				st.Ino = d.ino
				return nil
			}, func(f int) { cloexec = append(cloexec, f) }, func(f uintptr, name string) *os.File {
				return os.NewFile(uintptr(1000+f), name)
			})
			return fds, err, cloexec
		}
		if fds, err, ce := check(good()); err != nil || len(ce) != 7 || fds.inv == nil || fds.stderr == nil {
			t.Fatalf("good layout: %v %v", err, ce)
		}
		for name, c := range map[string]struct {
			mut  func(m map[int]*fd)
			want string
		}{
			"missing":   {func(m map[int]*fd) { m[5].gone = true }, "fd 5 (release) is missing"},
			"wrong end": {func(m map[int]*fd) { m[6].mode = unix.O_RDONLY }, "fd 6 (status) is the wrong end"},
			"rdwr":      {func(m map[int]*fd) { m[3].mode = unix.O_RDWR }, "fd 3 (invocation) is the wrong end"},
			"not pipe":  {func(m map[int]*fd) { m[8].typ = unix.S_IFREG }, "fd 8 (adapter stdout) is not a pipe"},
			"duplicate": {func(m map[int]*fd) { m[9].ino = m[8].ino }, "fd 9 (adapter stderr) duplicates"},
		} {
			m := good()
			c.mut(m)
			if _, err, _ := check(m); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("sender", func(t *testing.T) {
		t.Parallel()
		// The restart's command channel: no reader means no guardian
		// (ENXIO), a reader receives the whole line, and a regular file
		// is refused.
		dir := t.TempDir()
		fifo := filepath.Join(dir, controlName)
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sendCommand(fifo, []byte("x\n")); !errors.Is(err, errNoGuardian) {
			t.Fatalf("no reader: %v", err)
		}
		rd, err := os.OpenFile(fifo, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Close()
		if err := sendCommand(fifo, []byte("line\n")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16)
		if n, _ := rd.Read(buf); string(buf[:n]) != "line\n" {
			t.Fatalf("read %q", buf[:n])
		}
		plain := filepath.Join(dir, "plain")
		os.WriteFile(plain, nil, 0o600)
		if err := sendCommand(plain, []byte("x\n")); err == nil || errors.Is(err, errNoGuardian) {
			t.Fatalf("regular file: %v", err)
		}
		if err := sendCommand(filepath.Join(dir, "absent"), []byte("x\n")); err == nil {
			t.Fatal("an absent FIFO accepted a command")
		}
	})
	t.Run("journal-scan", func(t *testing.T) {
		t.Parallel()
		// The task directory's only nonregular entry is the exact control
		// FIFO; a FIFO under another name, a symlink or a public mode is
		// refused on every state-loading path.
		for name, mk := range map[string]func(dir string){
			"fifo name": func(dir string) { unix.Mkfifo(filepath.Join(dir, "other"), 0o600) },
			"public fifo": func(dir string) {
				unix.Mkfifo(filepath.Join(dir, controlName), 0o600)
				os.Chmod(filepath.Join(dir, controlName), 0o644)
			},
			"symlink":      func(dir string) { os.Symlink("/etc/passwd", filepath.Join(dir, ownerName)) },
			"public owner": func(dir string) { os.WriteFile(filepath.Join(dir, ownerName), []byte("{}"), 0o644) },
			"control file": func(dir string) { os.WriteFile(filepath.Join(dir, controlName), nil, 0o600) },
		} {
			root := t.TempDir()
			dir := filepath.Join(root, journalDir, taskID(1))
			os.MkdirAll(dir, 0o700)
			os.Chmod(filepath.Join(root, journalDir), 0o700)
			mk(dir)
			if _, err := (layout{root: root}).scanJournals(); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		root := t.TempDir()
		dir := filepath.Join(root, journalDir, taskID(1))
		os.MkdirAll(dir, 0o700)
		os.Chmod(filepath.Join(root, journalDir), 0o700)
		unix.Mkfifo(filepath.Join(dir, controlName), 0o600)
		os.WriteFile(filepath.Join(dir, tempPrefix+"x"), nil, 0o600)
		if ids, err := (layout{root: root}).scanJournals(); err != nil || len(ids) != 1 {
			t.Fatalf("valid directory: %v %v", ids, err)
		}
	})
}

// TestControlJournal is UT FP-4/5 for the task journal's storage rules:
// the private layout on every loading path, strict and bounded reads,
// durable publication steps that leave nothing behind on failure and
// removal that always resyncs; and the guardian's OS seam pieces that run
// in process.
func TestControlJournal(t *testing.T) {
	t.Parallel()
	lookup := adapter.ContractLookup(newScript().registry())
	st := startBody(1, roleConfig("a", "/srv/i.md", "/srv/r.md"), 1, 1, "journal")
	prepared := journalOf(st, contract.JournalPrepared)
	prepared.OwnerNonce, prepared.StartedAt = nil, nil
	t.Run("scan", func(t *testing.T) {
		t.Parallel()
		for name, mk := range map[string]func(root string){
			"unknown root entry": func(root string) { os.WriteFile(filepath.Join(root, journalDir, "notes"), nil, 0o600) },
			"temp directory":     func(root string) { os.MkdirAll(filepath.Join(root, journalDir, tempPrefix+"d"), 0o700) },
			"task symlink": func(root string) {
				os.MkdirAll(filepath.Join(root, "elsewhere"), 0o700)
				os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(root, journalDir, taskID(2)))
			},
			"unknown file":     func(root string) { os.WriteFile(filepath.Join(root, journalDir, taskID(1), "notes"), nil, 0o600) },
			"public execution": func(root string) { os.Chmod(filepath.Join(root, journalDir, taskID(1), executionName), 0o644) },
			"public directory": func(root string) { os.Chmod(filepath.Join(root, journalDir, taskID(1)), 0o755) },
		} {
			root := t.TempDir()
			writeJournal(t, root, prepared, nil)
			mk(root)
			if _, err := (layout{root: root}).scanJournals(); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, journalDir), nil, 0o600)
		if _, err := (layout{root: root}).scanJournals(); err == nil {
			t.Fatal("a tasks file accepted")
		}
		root = t.TempDir()
		writeJournal(t, root, prepared, nil)
		os.WriteFile(filepath.Join(root, journalDir, tempPrefix+"x"), nil, 0o600)
		if ids, err := (layout{root: root}).scanJournals(); err != nil || len(ids) != 1 {
			t.Fatalf("a regular temporary: %v %v", ids, err)
		}
		if ids, err := (layout{root: t.TempDir()}).scanJournals(); err != nil || ids != nil {
			t.Fatalf("no tasks/: %v %v", ids, err)
		}
	})
	t.Run("load", func(t *testing.T) {
		t.Parallel()
		other := *ownerOf(st, 9001, contract.OwnerArmed)
		other.Execution.Attachment = 7
		for name, c := range map[string]struct {
			j     contract.ExecutionJournal
			owner *contract.OwnerRecord
			raw   string
			dir   string
		}{
			"owner of another execution": {j: prepared, owner: &other},
			"owner nonce": {j: journalOf(st, contract.JournalRunning), owner: func() *contract.OwnerRecord {
				o := ownerOf(st, 9001, contract.OwnerArmed)
				o.Nonce = strings.Repeat("a", 64)
				return o
			}()},
			"corrupt owner":   {j: prepared, raw: "{"},
			"oversized owner": {j: prepared, raw: strings.Repeat(" ", contract.MaxOwnerBytes+1)},
			"directory name":  {j: prepared, dir: taskID(5)},
		} {
			root := t.TempDir()
			writeJournal(t, root, c.j, c.owner)
			dir := filepath.Join(root, journalDir, c.j.TaskID)
			if c.raw != "" {
				os.WriteFile(filepath.Join(dir, ownerName), []byte(c.raw), 0o600)
			}
			id := c.j.TaskID
			if c.dir != "" {
				os.Rename(dir, filepath.Join(root, journalDir, c.dir))
				id = c.dir
			}
			if _, err := (layout{root: root}).loadJournal(id, lookup); contract.CodeOf(err) != contract.CodeConflict {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := (layout{root: t.TempDir()}).loadJournal(taskID(1), lookup); err == nil {
			t.Fatal("a missing journal loaded")
		}
	})
	t.Run("publication", func(t *testing.T) {
		t.Parallel()
		// Every durability step's failure leaves the old document and no
		// temporary; removal resyncs tasks/ even for an absent directory.
		root := t.TempDir()
		fc := &failCounter{}
		d := testDeps(nil)
		d.fail = fc.fail
		jr := journal{l: layout{root: root}, d: d}
		// Creation's steps fail before publication: nothing is published
		// and a retry succeeds.
		for _, c := range []struct{ op, name string }{{"mkdir", stageRel(st.TaskID)}, {"create", filepath.Join(stageRel(st.TaskID), executionName)},
			{"rename", taskDirRel(st.TaskID)}, {"dirsync", journalDir}} {
			fc.set(c.op, c.name, 1)
			if err := jr.create(prepared); err == nil {
				t.Fatalf("create with %s %s failing succeeded", c.op, c.name)
			}
			if c.op != "dirsync" {
				if _, err := os.Lstat(filepath.Join(root, taskDirRel(st.TaskID))); err == nil {
					t.Fatalf("%s: a task directory was published", c.op)
				}
			}
			jr.remove(st.TaskID)
		}
		fc.set("", "", 0)
		if err := jr.create(prepared); err != nil {
			t.Fatal(err)
		}
		rel := filepath.Join(taskDirRel(st.TaskID), executionName)
		running := journalOf(st, contract.JournalRunning)
		for _, op := range []string{"create", "write", "sync", "rename", "dirsync"} {
			name := rel
			if op == "dirsync" {
				name = taskDirRel(st.TaskID)
			}
			fc.set(op, name, 1)
			if err := jr.write(running); err == nil {
				t.Fatalf("%s: no error", op)
			}
			entries, _ := os.ReadDir(filepath.Join(root, journalDir, st.TaskID))
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), tempPrefix) {
					t.Fatalf("%s left %s", op, e.Name())
				}
			}
		}
		fc.set("", "", 0)
		del := deleteRel(st.TaskID)
		for _, c := range []struct{ op, name string }{{"rename", del}, {"dirsync", journalDir}, {"remove", filepath.Join(del, executionName)},
			{"dirsync", del}, {"remove", del}, {"dirsync", journalDir}} {
			fc.set(c.op, c.name, 1)
			if err := jr.remove(st.TaskID); err == nil {
				t.Fatalf("remove with %s %s failing succeeded", c.op, c.name)
			}
		}
		fc.set("", "", 0)
		if err := jr.remove(st.TaskID); err != nil {
			t.Fatal(err)
		}
		fc.set("dirsync", journalDir, 1)
		if err := jr.remove(st.TaskID); err == nil {
			t.Fatal("an absent directory's removal skipped the tasks/ resync")
		}
		// Unencodable or oversized documents never reach the disk.
		huge := prepared
		huge.Phase = contract.JournalCompleted
		res := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: new(int)}.Sealed()
		huge.Result = &res
		huge.Log = contract.TaskLog{Data: make([]byte, 13<<20), SourceBytes: 13 << 20, ReceivedBytes: 13 << 20}
		if err := jr.write(huge); err == nil {
			t.Fatal("an oversized journal was written")
		}
		if _, err := readPrivate(filepath.Join(root, "absent"), 10); err == nil {
			t.Fatal("an absent file was read")
		}
		big := filepath.Join(root, "big")
		os.WriteFile(big, make([]byte, 11), 0o600)
		if _, err := readPrivate(big, 10); err == nil {
			t.Fatal("an oversized file was read")
		}
	})
	t.Run("seams", func(t *testing.T) {
		t.Parallel()
		// The guardian's production OS seam, the pieces that run in
		// process (no adapter start, no signal).
		env := defaultGuardianEnv()
		if env.getpid() != os.Getpid() || env.getpgrp() != syscall.Getpgrp() || env.grace != groupGrace || env.now().Location() != time.UTC || env.lookup == nil {
			t.Fatal("the production seam")
		}
		fifo := filepath.Join(t.TempDir(), "f")
		if err := env.mkfifo(fifo); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(fifo); err != nil || fi.Mode()&os.ModeNamedPipe == 0 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("fifo %v %v", fi, err)
		}
		c := make(chan os.Signal, 1)
		env.notifyTerm(c)()
		// Bounded diagnostics keep the newest bytes.
		tw := &tailWriter{max: 4}
		tw.Write([]byte("abc"))
		if n, _ := tw.Write([]byte("def")); n != 3 || string(tw.b) != "cdef" {
			t.Fatalf("tail %q", tw.b)
		}
		// An invalid invocation never starts a guardian and closes the
		// adapter's pipe ends.
		p, err := newPipes()
		if err != nil {
			t.Fatal(err)
		}
		defer p.closeAll()
		g := newExecGuardian(guardianSpec{exe: "/nonexistent/callsheet", proc: procSpec{stdin: p.stdinR, stdout: p.stdoutW, stderr: p.stderrW}})
		if err := g.Start(); err == nil {
			t.Fatal("an invalid invocation started")
		}
		if _, err := p.stdoutW.Write([]byte("x")); err == nil {
			t.Fatal("the adapter's pipe ends were not closed")
		}
	})
	t.Run("owner-failed", func(t *testing.T) {
		t.Parallel()
		// The guardian cannot publish its ownership: an error status (no
		// adapter), exit 1.
		r := newGuardianRig(t, contract.JournalPrepared)
		r.env.mkfifo = func(p string) error {
			err := unix.Mkfifo(p, 0o600)
			os.Chmod(r.dir, 0o500)
			return err
		}
		t.Cleanup(func() { os.Chmod(r.dir, 0o700) })
		r.run(t, nil)
		if s := r.p.status(t); s.Type != contract.GuardianError || *s.Reason != guardianOwnerFailed {
			t.Fatalf("status %+v", s)
		}
		if code := r.exit(t); code != 1 {
			t.Fatalf("exit %d", code)
		}
		select {
		case <-r.started:
			t.Fatal("an adapter started")
		default:
		}
	})
}

// copyState copies a sidecar state root as a crash would leave it on
// disk: directories, regular files and FIFOs with their modes.
func copyState(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
			return os.Chmod(dst, fi.Mode().Perm())
		case fi.Mode()&os.ModeNamedPipe != 0:
			return unix.Mkfifo(dst, uint32(fi.Mode().Perm()))
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, b, fi.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("copy state: %v", err)
	}
}

// createJournal publishes a new execution's first journal document.
func createJournal(jr journal, j contract.ExecutionJournal) error { return jr.create(j) }

// journalCrashWindows is TestControlRestart's crash matrix for the task
// journal's own transactions (C2): one execution's journal life (its
// creation with the prepared document, the guardian's owner record and
// FIFO, the running, completed updates, and the committed outbox's
// deletion) is replayed once per filesystem operation, the state root
// being copied at that operation as a crash there leaves it (the
// operation not done, nothing after it). A new Run started on every such
// image must start (a half-created or half-deleted task directory is
// never a corrupt journal) and leave only valid journals behind.
func journalCrashWindows(t *testing.T) {
	type op struct{ op, name string }
	st := startBody(1, roleConfig("a", "/srv/i.md", "/srv/r.md"), 1, 1, "crash windows")
	prepared := journalOf(st, contract.JournalPrepared)
	prepared.OwnerNonce, prepared.StartedAt = nil, nil
	running := journalOf(st, contract.JournalRunning)
	completed, _ := completedJournal(st, 0, "tail\n")
	// life runs the journal life on a fresh enrolled root, stopping at the
	// first failing operation (a crash stops everything after it).
	life := func(t *testing.T, fp *fakePlane, root string, hook func(op, name string) error) {
		writeState(t, root, testID, fp.url, fp.caPEM)
		d := testDeps(nil)
		d.fail = hook
		jr := journal{l: layout{root: root}, d: d}
		dir := filepath.Join(root, journalDir, st.TaskID)
		for _, step := range []func() error{
			func() error { return createJournal(jr, prepared) },
			func() error {
				// The guardian's durable ownership (not the sidecar's
				// operations).
				ob, _ := contract.EncodeOwner(*ownerOf(st, 9001, contract.OwnerArmed))
				if err := os.WriteFile(filepath.Join(dir, ownerName), ob, 0o600); err != nil {
					return err
				}
				return unix.Mkfifo(filepath.Join(dir, controlName), 0o600)
			},
			func() error { return jr.write(running) },
			func() error { return jr.write(completed) },
			func() error { return jr.remove(st.TaskID) },
		} {
			if err := step(); err != nil {
				break
			}
		}
	}
	var ops []op
	fp := startFakePlane(t)
	life(t, fp, newRoot(t), func(o, name string) error {
		ops = append(ops, op{o, name})
		return nil
	})
	if len(ops) < 10 {
		t.Fatalf("recorded only %d journal operations: %v", len(ops), ops)
	}
	for k := range ops {
		t.Run(strconv.Itoa(k)+"-"+ops[k].op+"-"+strings.ReplaceAll(ops[k].name, "/", "_"), func(t *testing.T) {
			image := filepath.Join(t.TempDir(), "image")
			root := newRoot(t)
			n := 0
			life(t, fp, root, func(o, name string) error {
				n++
				if n-1 == k {
					copyState(t, root, image)
					return errors.New("injected crash at " + o + " " + name)
				}
				return nil
			})
			if n <= k {
				t.Fatalf("operation %d (%v) was not reached", k, ops[k])
			}
			// The restarted Run starts over the crash image and leaves only
			// valid journals.
			rfp := startFakePlane(t)
			tr := startTaskRun(t, rfp, taskOpts{root: image})
			deadline := time.After(testWait)
		wait:
			for {
				select {
				case ev := <-tr.ev.ch:
					if ev.kind == evRecoveryDone {
						break wait
					}
				case err := <-tr.run.done:
					tr.run.done <- err
					t.Fatalf("the restarted Run ended: %v", err)
				case <-deadline:
					t.Fatal("no recovery-done event")
				}
			}
			ids, err := (layout{root: image}).scanJournals()
			if err != nil {
				t.Fatalf("after recovery: %v", err)
			}
			entries, _ := os.ReadDir(filepath.Join(image, journalDir))
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), tempPrefix) && e.IsDir() {
					t.Fatalf("a transaction leftover %s remains after recovery", e.Name())
				}
			}
			for _, id := range ids {
				if _, err := (layout{root: image}).loadJournal(id, tr.lookup()); err != nil {
					t.Fatalf("journal %s: %v", id, err)
				}
			}
		})
	}
}
