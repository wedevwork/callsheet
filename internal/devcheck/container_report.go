package devcheck

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The retained container evidence (design m3-m4-container-e2e, Evidence
// location and workflow publication). RunOptions.EvidenceDir selects the
// directory; cmd/devcheck passes the fixed /tmp path and every test a
// temporary one. A sibling lock file keyed to that directory serializes
// writers; only the final component, the lock and the known output files
// are checked for symlinks (ancestors such as /tmp may be links).

// Evidence output files: each invocation replaces only these.
const (
	evidenceReportFile  = "report.txt"
	evidenceLedgerFile  = "ledger.jsonl"
	evidenceRuntimeFile = "runtime.log"
	// evidenceDiagLimit bounds the retained diagnostic text per invocation
	// (the ledger is separate and always complete).
	evidenceDiagLimit = 8 << 20
	// reportDiagTail is how much of the runtime diagnostics report.txt
	// repeats at its end.
	reportDiagTail = 64 << 10
)

// RunOptions are the explicit inputs of a devcheck invocation that are not
// command-line arguments.
type RunOptions struct {
	// EvidenceDir is the absolute container evidence directory (required:
	// there is no fallback). Its parent must exist.
	EvidenceDir string
}

// validateRunOptions rejects an empty or relative evidence directory.
func validateRunOptions(o RunOptions) error {
	if o.EvidenceDir == "" || !filepath.IsAbs(o.EvidenceDir) {
		return fmt.Errorf("devcheck: the evidence directory must be an absolute path, got %q", o.EvidenceDir)
	}
	return nil
}

// boundedLog keeps at most limit bytes of diagnostics in w and counts the
// rest; it is safe for concurrent writers.
type boundedLog struct {
	mu      sync.Mutex
	w       interface{ Write([]byte) (int, error) }
	limit   int
	written int
	dropped int
	tail    tailBuffer
	err     error
}

func newBoundedLog(w interface{ Write([]byte) (int, error) }, limit int) *boundedLog {
	return &boundedLog{w: w, limit: limit, tail: tailBuffer{max: reportDiagTail}}
}

func (l *boundedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tail.Write(p)
	keep := p
	if room := l.limit - l.written; room < len(keep) {
		if room < 0 {
			room = 0
		}
		l.dropped += len(keep) - room
		keep = keep[:room]
	}
	if len(keep) > 0 && l.err == nil && l.w != nil {
		if _, err := l.w.Write(keep); err != nil {
			l.err = err
		}
	}
	l.written += len(keep)
	return len(p), nil
}

func (l *boundedLog) state() (written, dropped int, tail string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written, l.dropped, string(l.tail.b), l.err
}

// evidenceReport is one invocation's retained evidence: the held lock,
// report.txt (rewritten after every phase and failure), ledger.jsonl and
// runtime.log.
type evidenceReport struct {
	dir      string
	lock     *os.File
	runtime  *os.File
	diag     *boundedLog
	RunID    string
	stage    string
	count    int
	started  time.Time
	revision string
	outcome  string
	progress string
	phases   []string
	problems []string
	ledger   [][]byte
	closed   bool
}

// newRunID returns an invocation-unique opaque run ID (32 hex).
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// rejectSymlink fails if p exists and is a symlink or not of kind want
// (dir or regular); a missing p is fine.
func rejectSymlink(p string, dir bool) error {
	st, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("devcheck: evidence path %s: %w", p, err)
	case st.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("devcheck: evidence path %s is a symlink", p)
	case dir && !st.IsDir():
		return fmt.Errorf("devcheck: evidence path %s is not a directory", p)
	case !dir && !st.Mode().IsRegular():
		return fmt.Errorf("devcheck: evidence path %s is not a regular file", p)
	}
	return nil
}

// openNoFollow opens (creating) a known output file without following a
// final symlink, truncated unless appending.
func openNoFollow(p string, flags int) (*os.File, error) {
	if err := rejectSymlink(p, false); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|flags, 0o644)
}

// openEvidence locks dir's sibling lock file (nonblocking), creates dir if
// needed and initializes the three output files: report.txt as "not
// started" for this run, an empty ledger and an empty runtime log. The
// lock is held until close.
func openEvidence(dir, stage string, count int, now time.Time) (*evidenceReport, error) {
	dir = filepath.Clean(dir)
	lockPath := dir + ".lock"
	if err := rejectSymlink(lockPath, false); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, fmt.Errorf("devcheck: evidence lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("devcheck: evidence directory %s is in use by another devcheck run (lock %s)", dir, lockPath)
		}
		return nil, fmt.Errorf("devcheck: evidence lock %s: %w", lockPath, err)
	}
	r := &evidenceReport{dir: dir, lock: lock, stage: stage, count: count, started: now.UTC(), outcome: "not started", progress: "not started"}
	fail := func(err error) (*evidenceReport, error) {
		r.release()
		return nil, err
	}
	if err := rejectSymlink(dir, true); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fail(fmt.Errorf("devcheck: evidence directory %s: %w", dir, err))
	}
	if err := rejectSymlink(dir, true); err != nil {
		return fail(err)
	}
	if r.RunID, err = newRunID(); err != nil {
		return fail(err)
	}
	if r.runtime, err = openNoFollow(filepath.Join(dir, evidenceRuntimeFile), os.O_TRUNC); err != nil {
		return fail(fmt.Errorf("devcheck: evidence %s: %w", evidenceRuntimeFile, err))
	}
	r.diag = newBoundedLog(r.runtime, evidenceDiagLimit)
	if err := r.writeLedger(); err != nil {
		return fail(err)
	}
	if err := r.flush(); err != nil {
		return fail(err)
	}
	return r, nil
}

// release closes the runtime log and releases and closes the lock; the
// lock file stays in place.
func (r *evidenceReport) release() {
	if r.runtime != nil {
		r.runtime.Close()
		r.runtime = nil
	}
	if r.lock != nil {
		syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN)
		r.lock.Close()
		r.lock = nil
	}
}

// phase records a phase and flushes the report.
func (r *evidenceReport) phase(format string, args ...any) error {
	r.phases = append(r.phases, fmt.Sprintf(format, args...))
	return r.flush()
}

// fail records a failure: the outcome becomes fail.
func (r *evidenceReport) fail(err error) error {
	r.outcome = "fail"
	r.problems = append(r.problems, err.Error())
	return r.flush()
}

// writeLedger replaces ledger.jsonl with the records so far.
func (r *evidenceReport) writeLedger() error {
	f, err := openNoFollow(filepath.Join(r.dir, evidenceLedgerFile), os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("devcheck: evidence %s: %w", evidenceLedgerFile, err)
	}
	for _, rec := range r.ledger {
		if _, err := f.Write(append(append([]byte(nil), rec...), '\n')); err != nil {
			f.Close()
			return fmt.Errorf("devcheck: evidence %s: %w", evidenceLedgerFile, err)
		}
	}
	return f.Close()
}

// appendLedger adds one iteration's received records (also those of a
// failed iteration) and flushes ledger.jsonl and report.txt.
func (r *evidenceReport) appendLedger(records [][]byte) error {
	r.ledger = append(r.ledger, records...)
	if err := r.writeLedger(); err != nil {
		return err
	}
	return r.flush()
}

// sanitize escapes control bytes other than newline and tab.
func sanitize(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c == '\n' || c == '\t':
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// render is report.txt's complete text.
func (r *evidenceReport) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "callsheet container-e2e evidence report\n")
	fmt.Fprintf(&b, "run_id: %s\nstage: %s\nstarted_at: %s\n", r.RunID, r.stage, r.started.Format(time.RFC3339))
	rev := r.revision
	if rev == "" {
		rev = "-"
	}
	fmt.Fprintf(&b, "source_revision: %s\nrequested_iterations: %d\n", rev, r.count)
	fmt.Fprintf(&b, "outcome: %s\ncontainer: %s\n", r.outcome, r.progress)
	b.WriteString("phases:\n")
	for _, p := range r.phases {
		fmt.Fprintf(&b, "  - %s\n", sanitize(p))
	}
	b.WriteString("diagnostics:\n")
	for _, p := range r.problems {
		for _, line := range strings.Split(sanitize(p), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	if r.diag != nil {
		written, dropped, tail, err := r.diag.state()
		fmt.Fprintf(&b, "runtime_log: %s (%d bytes retained, %d bytes dropped, truncated: %v)\n", evidenceRuntimeFile, written, dropped, dropped > 0)
		if err != nil {
			fmt.Fprintf(&b, "runtime_log_error: %s\n", sanitize(err.Error()))
		}
		if tail != "" {
			fmt.Fprintf(&b, "runtime_log_tail (last %d bytes at most):\n", reportDiagTail)
			for _, line := range strings.Split(strings.TrimRight(sanitize(tail), "\n"), "\n") {
				fmt.Fprintf(&b, "  | %s\n", line)
			}
		}
	}
	fmt.Fprintf(&b, "ledger: %d records\n", len(r.ledger))
	for _, rec := range r.ledger {
		b.Write(rec)
		b.WriteByte('\n')
	}
	return b.String()
}

// flush replaces report.txt with the current state.
func (r *evidenceReport) flush() error {
	f, err := openNoFollow(filepath.Join(r.dir, evidenceReportFile), os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("devcheck: evidence %s: %w", evidenceReportFile, err)
	}
	if _, err := f.WriteString(r.render()); err != nil {
		f.Close()
		return fmt.Errorf("devcheck: evidence %s: %w", evidenceReportFile, err)
	}
	return f.Close()
}

// close writes the final report and releases the lock; it is idempotent.
func (r *evidenceReport) close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.flush()
	r.release()
	return err
}
