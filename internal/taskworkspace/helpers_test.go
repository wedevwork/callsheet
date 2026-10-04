package taskworkspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Package-internal fixtures (iteration 10b): an in-memory "plane" store
// serving packs through a scriptable fetcher, and task directories.

const (
	testWait = 30 * time.Second
	instA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	instB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func spec(kv ...string) map[string]testkit.FileSpec {
	out := map[string]testkit.FileSpec{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte(kv[i+1])}
	}
	return out
}

// source is a plane-side object store.
type source struct {
	t *testing.T
	s *memory.Storage
}

func newSource(t *testing.T) *source { return &source{t: t, s: testkit.NewMemoryStore()} }

// commit commits files with parents.
func (s *source) commit(files map[string]testkit.FileSpec, parents ...plumbing.Hash) plumbing.Hash {
	s.t.Helper()
	c, err := testkit.CommitFiles(s.s, files, parents, "c")
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// fetcher is a scriptable Remote over a source.
type fetcher struct {
	src *source
	mu  sync.Mutex
	// calls are the haves of every fetch.
	calls [][]plumbing.Hash
	// before runs at each fetch (blocking, failing); omit drops objects
	// from the pack; garbage replaces the pack.
	before  func(n int, ctx context.Context) error
	omit    func(n int, h plumbing.Hash) bool
	garbage func(n int) bool
	refs    map[string]plumbing.Hash
	refsErr error
	// stall makes fetch n a stalled peer: its pack body yields a few bytes
	// and then delivers nothing more until the fetch's context ends (as a
	// cancelled transport body does), announcing on parked once the
	// receiver is blocked in it.
	stall  func(n int) bool
	parked chan struct{}
}

// stalledBody is a peer that stopped sending mid-body.
type stalledBody struct {
	ctx    context.Context
	head   []byte
	parked chan struct{}
}

func (s *stalledBody) Read(p []byte) (int, error) {
	if len(s.head) > 0 {
		n := copy(p, s.head)
		s.head = s.head[n:]
		return n, nil
	}
	select {
	case s.parked <- struct{}{}:
	default:
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func (f *fetcher) UploadRefs(ctx context.Context) (map[string]plumbing.Hash, error) {
	return f.refs, f.refsErr
}

func (f *fetcher) Fetch(ctx context.Context, wants, haves []plumbing.Hash, sink func(io.Reader) error) error {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, append([]plumbing.Hash(nil), haves...))
	before, omit, garbage, stall := f.before, f.omit, f.garbage, f.stall
	f.mu.Unlock()
	if before != nil {
		if err := before(n, ctx); err != nil {
			return err
		}
	}
	var known []plumbing.Hash
	for _, h := range haves {
		if f.src.s.HasEncodedObject(h) == nil {
			known = append(known, h)
		}
	}
	objs, err := revlist.Objects(f.src.s, wants, known)
	if err != nil {
		return err
	}
	if omit != nil {
		var keep []plumbing.Hash
		for _, h := range objs {
			if !omit(n, h) {
				keep = append(keep, h)
			}
		}
		objs = keep
	}
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, f.src.s, false).Encode(objs, 0); err != nil {
		return err
	}
	b := buf.Bytes()
	if garbage != nil && garbage(n) {
		b = []byte("PACK not a pack at all")
	}
	if stall != nil && stall(n) {
		return sink(&stalledBody{ctx: ctx, head: b[:16], parked: f.parked})
	}
	return sink(bytes.NewReader(b))
}

func (f *fetcher) nCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// openCache opens a cache under a fresh root.
func openCache(t *testing.T, target int64) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	m, err := OpenCache(context.Background(), CacheOptions{Root: root, Origin: "https://plane.test fp", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

// taskDB creates a fresh task database.
func newTaskDB(t *testing.T) *workspacetransfer.TaskDB {
	t.Helper()
	db, err := workspacetransfer.CreateTaskDB(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// hasClosure requires db to hold base's whole closure.
func hasClosure(t *testing.T, db *workspacetransfer.TaskDB, base plumbing.Hash) {
	t.Helper()
	if _, _, err := db.Closure(context.Background(), []plumbing.Hash{base}); err != nil {
		t.Fatalf("task database lacks the closure of %s: %v", base, err)
	}
}

// stages records a Manager's stage hook.
type stageLog struct {
	mu  sync.Mutex
	got []string
	ch  chan string
}

func hookOf(m *Manager) *stageLog {
	l := &stageLog{ch: make(chan string, 1024)}
	m.hook = func(s string) {
		l.mu.Lock()
		l.got = append(l.got, s)
		l.mu.Unlock()
		select {
		case l.ch <- s:
		default:
		}
	}
	return l
}

func (l *stageLog) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, g := range l.got {
		if g == s {
			return true
		}
	}
	return false
}

func (l *stageLog) await(t *testing.T, s string) {
	t.Helper()
	l.awaitBy(t, s, time.After(testWait))
}

// awaitBy waits until stage s was reached or timeout fires; the timeout
// rechecks the synchronized stage log before failing (a stage and the
// timeout both ready never fail the test).
func (l *stageLog) awaitBy(t testing.TB, s string, timeout <-chan time.Time) {
	t.Helper()
	testkit.AwaitCondBy(t, func() bool { return l.has(s) }, l.ch, timeout, "stage "+s)
}

// within receives the next value of c within testWait (rechecked at the
// timeout: testkit.WithinBy).
func within[T any](t testing.TB, c <-chan T, what string) T {
	t.Helper()
	return testkit.WithinBy(t, c, time.After(testWait), what)
}

// absentFor requires that c delivers nothing while window runs, rechecking
// c at the window's end (testkit.AbsentFor).
func absentFor[T any](t testing.TB, c <-chan T, window <-chan time.Time, what string) {
	t.Helper()
	testkit.AbsentFor(t, c, window, what)
}

// awaitStageBy consumes stage names from c until want arrives or timeout
// fires; the stages already sent are drained at the timeout before failing
// (testkit.AwaitValueBy).
func awaitStageBy(t testing.TB, c <-chan string, want string, timeout <-chan time.Time) {
	t.Helper()
	testkit.AwaitValueBy(t, c, func(s string) bool { return s == want }, timeout, "stage "+want)
}

// TestTimeoutHelpers (review r1 C5): this package's waits, with their
// success and their timeout both ready, never fail; a negative wait with
// its event and its window's end both ready fails.
func TestTimeoutHelpers(t *testing.T) {
	for i := 0; i < 200; i++ {
		l := &stageLog{ch: make(chan string, 1)}
		calls := 0
		l.ch <- "armed"
		if testkit.Fails(func(tb testing.TB) {
			testkit.AwaitCondBy(tb, func() bool {
				l.mu.Lock()
				defer l.mu.Unlock()
				if calls++; calls > 1 {
					l.got = append(l.got, "armed")
				}
				return len(l.got) > 0
			}, l.ch, testkit.Fired(), "stage armed")
		}) {
			t.Fatal("a stage wait failed with its stage and its timeout both ready")
		}
		done := make(chan struct {
			p   Publication
			err error
		}, 1)
		done <- struct {
			p   Publication
			err error
		}{}
		if testkit.Fails(func(tb testing.TB) { testkit.WithinBy(tb, done, testkit.Fired(), "the publication's end") }) {
			t.Fatal("await failed with the publication's end and its timeout both ready")
		}
		st := make(chan string, 2)
		st <- "observe"
		st <- "retry-armed"
		if testkit.Fails(func(tb testing.TB) { awaitStageBy(tb, st, "retry-armed", testkit.Fired()) }) {
			t.Fatal("advanceRetry's stage wait failed with its stage and its timeout both ready")
		}
		ev := make(chan string, 1)
		ev <- "in"
		if !testkit.Fails(func(tb testing.TB) { absentFor(tb, ev, testkit.Fired(), "fetched") }) {
			t.Fatal("a negative wait passed with its event and its window's end both ready")
		}
	}
}

// corruptDir overwrites every regular file below dir.
func corruptDir(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			os.Chmod(p, 0o600)
			os.WriteFile(p, []byte("corrupt"), 0o600)
			n++
		}
		return nil
	})
	return n
}

var errDenied = contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskAssignmentMismatch, "denied (fixture)")

// binding is a main-branch binding of base in instA.
func binding(base plumbing.Hash) contract.WorkspaceBinding {
	c := base.String()
	return contract.WorkspaceBinding{Name: "proj", Instance: instA, BaseSelector: contract.DefaultBranchRef, BaseCommit: &c}
}

// prepared runs Prepare in a fresh task directory.
func prepared(t *testing.T, m *Manager, f *fetcher, b contract.WorkspaceBinding, runtimeDir bool) (string, *Prepared) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, WorkName), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(context.Background(), PrepareInput{GOOS: runtime.GOOS, TaskDir: dir, Binding: b, Remote: f, Cache: m, RuntimeDir: runtimeDir})
	if err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.Remove(p)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func isCode(err error, code string) bool { return PublicationCode(err) == code }

// noise is n deterministic incompressible bytes (seed varies them).
func noise(n int, seed uint64) string {
	b := make([]byte, n)
	x := seed*0x9e3779b97f4a7c15 + 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = byte(x)
	}
	return string(b)
}

var _ = errors.New
var _ = strings.Repeat
