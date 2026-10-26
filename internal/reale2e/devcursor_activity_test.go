package reale2e

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The shallow observer's synthetic filesystem (design 12a-3-cursor-worker,
// UT-A3): directories may be arbitrarily large generators that never
// materialize their entries, every handle fails the test when more than
// the remaining budget plus one sentinel is requested or consumed, and
// every metadata operation is counted.

// synthNode is one synthetic entry.
type synthNode struct {
	typ   string
	uid   uint32
	size  int64
	mtime int64
	ino   uint64
	// kids are a directory's ordered static entries; n and gen a generated
	// directory's count and its i-th entry.
	kids []synthKid
	n    int
	gen  func(i int) (string, *synthNode)
	// lstatErr fails its lstat, openErr its open; fstatIno, when set, is
	// what its open descriptor reports (a replacement).
	lstatErr, openErr error
	fstatIno          uint64
	// nextErr fails its directory stream after the first batch;
	// eofWithLast returns the last entries together with io.EOF.
	nextErr     error
	eofWithLast bool
}

type synthKid struct {
	name string
	node *synthNode
}

// synthFS is the observer's synthetic filesystem.
type synthFS struct {
	t     testing.TB
	roots map[string]*synthNode
	// consumed counts every entry handed out; stats every lstat/fstat;
	// opens every directory open; maxBatch the largest request.
	consumed, stats, opens atomic.Int64
	maxBatch               atomic.Int64
	// tick advances clock on every operation (time-limit tests).
	clock *tickClock
	// dirs are the opened directories (one observer goroutine).
	dirs []*synthDir
}

func (f *synthFS) op() {
	if f.clock != nil {
		f.clock.step()
	}
}

func (f *synthFS) Lstat(path string) (activityStat, error) {
	f.op()
	f.stats.Add(1)
	n, ok := f.roots[path]
	if !ok {
		return activityStat{}, &os.PathError{Op: "lstat", Path: path, Err: syscall.ENOENT}
	}
	return n.stat()
}

func (f *synthFS) OpenDir(path string) (activityDir, error) {
	f.op()
	n := f.roots[path]
	return f.open(n)
}

func (f *synthFS) open(n *synthNode) (activityDir, error) {
	f.opens.Add(1)
	if n.openErr != nil {
		return nil, n.openErr
	}
	if n.typ != actDir {
		return nil, syscall.ENOTDIR
	}
	d := &synthDir{f: f, n: n, statted: map[string]int{}}
	f.dirs = append(f.dirs, d)
	return d, nil
}

func (n *synthNode) stat() (activityStat, error) {
	if n.lstatErr != nil {
		return activityStat{}, n.lstatErr
	}
	return activityStat{Type: n.typ, Mode: 0o755, UID: n.uid, Dev: 7, Ino: n.ino, Size: n.size, MtimeNS: n.mtime, CtimeNS: n.mtime}, nil
}

// synthDir is one open synthetic directory.
type synthDir struct {
	f   *synthFS
	n   *synthNode
	pos int
	// handed are the names enumerated in order; statted counts each
	// name's explicit lstat.
	handed  []string
	statted map[string]int
}

// sentinelChecks verifies, for every opened directory, that a consumed
// overflow sentinel was never stat'd and that no name was stat'd twice;
// it returns the number of sentinels seen.
func (f *synthFS) sentinelChecks(t testing.TB) int {
	t.Helper()
	sentinels := 0
	for _, d := range f.dirs {
		if len(d.handed) > activityMaxEntries {
			sentinels++
			if d.statted[d.handed[activityMaxEntries]] != 0 {
				t.Errorf("the overflow sentinel %s was stat'd", d.handed[activityMaxEntries])
			}
		}
		for name, n := range d.statted {
			if n > 1 {
				t.Errorf("%s stat'd %d times", name, n)
			}
		}
	}
	return sentinels
}

func (d *synthDir) count() int {
	if d.n.gen != nil {
		return d.n.n
	}
	return len(d.n.kids)
}

func (d *synthDir) entry(i int) (string, *synthNode) {
	if d.n.gen != nil {
		return d.n.gen(i)
	}
	return d.n.kids[i].name, d.n.kids[i].node
}

func (d *synthDir) Stat() (activityStat, error) {
	d.f.op()
	d.f.stats.Add(1)
	st, err := d.n.stat()
	if d.n.fstatIno != 0 {
		st.Ino = d.n.fstatIno
	}
	return st, err
}

func (d *synthDir) Next(n int) ([]string, error) {
	d.f.op()
	if int64(n) > d.f.maxBatch.Load() {
		d.f.maxBatch.Store(int64(n))
	}
	if n <= 0 || d.pos+n > activityMaxEntries+1 {
		d.f.t.Errorf("a batch of %d at position %d exceeds the budget plus its sentinel", n, d.pos)
	}
	var out []string
	for len(out) < n && d.pos < d.count() {
		name, _ := d.entry(d.pos)
		out = append(out, name)
		d.pos++
	}
	d.handed = append(d.handed, out...)
	d.f.consumed.Add(int64(len(out)))
	if d.pos > activityMaxEntries+1 {
		d.f.t.Errorf("%d entries consumed from one directory", d.pos)
	}
	switch {
	case d.n.nextErr != nil && d.pos > len(out):
		return nil, d.n.nextErr
	case len(out) == 0:
		return nil, io.EOF
	case d.n.eofWithLast && d.pos == d.count():
		return out, io.EOF
	}
	return out, nil
}

func (d *synthDir) child(name string) (*synthNode, bool) {
	if d.n.gen != nil {
		i, err := strconv.Atoi(strings.TrimPrefix(name, "e"))
		if err != nil || i < 0 || i >= d.n.n {
			return nil, false
		}
		_, node := d.n.gen(i)
		return node, true
	}
	for _, k := range d.n.kids {
		if k.name == name {
			return k.node, true
		}
	}
	return nil, false
}

func (d *synthDir) Lstat(name string) (activityStat, error) {
	d.f.op()
	d.f.stats.Add(1)
	d.statted[name]++
	n, ok := d.child(name)
	if !ok {
		return activityStat{}, syscall.ENOENT
	}
	return n.stat()
}

func (d *synthDir) OpenDir(name string) (activityDir, error) {
	d.f.op()
	n, ok := d.child(name)
	if !ok {
		return nil, syscall.ENOENT
	}
	if n.uid != 1000 {
		d.f.t.Errorf("the foreign directory %s was opened", name)
	}
	return d.f.open(n)
}

func (d *synthDir) Close() error { return nil }

// tickClock advances by step on every step call.
type tickClock struct {
	now  atomic.Int64
	step func()
}

func newTickClock(each time.Duration) *tickClock {
	c := &tickClock{}
	c.now.Store(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC).UnixNano())
	c.step = func() { c.now.Add(int64(each)) }
	return c
}

func (c *tickClock) Now() time.Time { return time.Unix(0, c.now.Load()).UTC() }

func (c *tickClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	ch <- c.Now().Add(d)
	return ch, func() bool { return false }
}

// bigDir is a generated directory of n entries, each a directory of m
// generated children (directories again), owned by uid 1000.
func bigDir(n, m int) *synthNode {
	var leaf func(i int) (string, *synthNode)
	leaf = func(i int) (string, *synthNode) {
		return fmt.Sprintf("e%07d", i), &synthNode{typ: actDir, uid: 1000, ino: uint64(10_000_000 + i), n: m, gen: leaf}
	}
	return &synthNode{typ: actDir, uid: 1000, ino: 1, n: n, gen: func(i int) (string, *synthNode) {
		name, node := leaf(i)
		node.ino = uint64(100 + i)
		return name, node
	}}
}

// observeSynth observes one root of the synthetic filesystem at uid 1000.
func observeSynth(f *synthFS, clock Clock, output int, specs ...activityRootSpec) *ActivityAggregate {
	if clock == nil {
		clock = newTickClock(0)
	}
	o := activityObserver{fs: f, clock: clock, uid: 1000, output: output, exclude: []string{"/x/excluded"}}
	return o.observe("run", "before", specs, clock.Now().Add(activityScanBudget))
}

func spec(scope, path string) activityRootSpec {
	return activityRootSpec{Scope: scope, Path: path, Token: path}
}

func TestCursorActivityBounded(t *testing.T) {
	t.Parallel()
	// A million-entry root of million-entry directories: work and output
	// are bounded independently of the generated cardinality.
	f := &synthFS{t: t, roots: map[string]*synthNode{"/c": bigDir(1_000_000, 1_000_000), "/t": bigDir(1_000_000, 1_000_000)}}
	agg := observeSynth(f, nil, 0, spec(activityScopeCache, "/c"), spec(activityScopeTmp, "/t"))
	if c := f.consumed.Load(); c > 2*(65+64*65) {
		t.Fatalf("consumed %d entries", c)
	}
	// Exactly the explicit operations: per root its lstat and fstat, 64
	// entry lstats, and per sampled directory its fstat and 64 child
	// lstats; no sentinel (one per directory, 65 here) is ever stat'd.
	if s := f.stats.Load(); s != 2*(1+1+64+64*(1+64)) {
		t.Fatalf("%d metadata operations", s)
	}
	if n := f.sentinelChecks(t); n != 2*65 {
		t.Fatalf("%d sentinels", n)
	}
	if f.maxBatch.Load() > activityMaxEntries+1 {
		t.Fatalf("batch %d", f.maxBatch.Load())
	}
	b, _ := json.Marshal(agg)
	if len(b) > activityMaxOutput {
		t.Fatalf("output %d bytes", len(b))
	}
	for _, r := range agg.Roots {
		if r.Status != actOK || r.TopLevelExhaustive || r.Exhaustive || !r.Flags.EntryLimited || len(r.Rows) != 64 ||
			r.ObservedCounts.Direct.Directory != 64 || r.ObservedCounts.Children.Directory != 64*64 {
			t.Fatalf("root %+v", r.Flags)
		}
		for _, row := range r.Rows {
			if row.ChildrenExhaustive || !row.Flags.EntryLimited || !row.Flags.DepthLimited || row.MetadataSHA256 == nil || row.ObservedCounts.Directory != 64 {
				t.Fatalf("row %+v", row)
			}
		}
	}
	if agg.Schema != ActivitySchema || agg.Sampling != "directory-prefix" || agg.MaxDepth != 2 ||
		agg.Limits != (ActivityLimits{TopLevelEntries: 64, ChildEntries: 64, OutputBytes: 256 << 10, ScanBudgetMS: 5000}) {
		t.Fatalf("record %+v", agg.Limits)
	}
}

func TestCursorActivityStatuses(t *testing.T) {
	t.Parallel()
	file := func(size int64) *synthNode { return &synthNode{typ: actFile, uid: 1000, size: size, mtime: 5} }
	dir := func(uid uint32, kids ...synthKid) *synthNode {
		return &synthNode{typ: actDir, uid: uid, ino: 9, kids: kids}
	}
	roots := map[string]*synthNode{
		"/symlink":    {typ: actSymlink, uid: 1000},
		"/unreadable": {typ: actDir, lstatErr: syscall.EACCES},
		"/openfail":   {typ: actDir, uid: 1000, openErr: syscall.EACCES},
		"/raced":      {typ: actDir, uid: 1000, ino: 3, fstatIno: 4},
		"/foreign": dir(0, synthKid{"own", dir(1000, synthKid{"a", file(3)}, synthKid{"b", file(4)}, synthKid{"sub", dir(1000)},
			synthKid{"link", &synthNode{typ: actSymlink, uid: 1000}}, synthKid{"gone", &synthNode{typ: actFile, lstatErr: syscall.ENOENT}},
			synthKid{"locked", &synthNode{typ: actFile, lstatErr: syscall.EACCES}}, synthKid{"root-file", &synthNode{typ: actFile, uid: 0, size: 1}})},
			synthKid{"theirs", dir(0, synthKid{"secret", file(1)})},
			synthKid{"loop", &synthNode{typ: actDir, uid: 1000, openErr: unix.ELOOP}},
			synthKid{"denied", &synthNode{typ: actDir, uid: 1000, openErr: syscall.EACCES}},
			synthKid{"swapped", &synthNode{typ: actDir, uid: 1000, ino: 1, fstatIno: 2}},
			synthKid{"vanished", &synthNode{typ: actDir, lstatErr: syscall.ENOENT}},
			synthKid{"unknown", &synthNode{typ: actOther, lstatErr: syscall.EIO}},
			synthKid{"fifo", &synthNode{typ: actOther, uid: 1000}},
			synthKid{"big", file(math.MaxInt64)}, synthKid{"big2", file(math.MaxInt64)}, synthKid{"big3", file(math.MaxInt64)},
			synthKid{strings.Repeat("\xff", 400), file(1)}, synthKid{"excluded", dir(1000)}),
	}
	f := &synthFS{t: t, roots: roots}
	agg := observeSynth(f, nil, 0, spec("a", "/absent"), spec("s", "/symlink"), spec("u", "/unreadable"), spec("o", "/openfail"),
		spec("r", "/raced"), spec("f", "/foreign"))
	want := []string{actAbsent, actNotDir, actUnreadable, actUnreadable, actRaced, actForeign}
	for i, r := range agg.Roots {
		if r.Status != want[i] {
			t.Errorf("root %s status %s want %s", r.Scope, r.Status, want[i])
		}
	}
	if a := agg.Roots[0]; !a.Exhaustive || !a.TopLevelExhaustive || len(a.Rows) != 0 {
		t.Errorf("an absent root is an explicit complete observation: %+v", a)
	}
	fr := agg.Roots[5]
	rows := rowsByName(fr.Rows)
	excludedRoot := agg.Roots[5]
	_ = excludedRoot
	checks := map[string]string{"own": actOK, "theirs": actForeign, "loop": actRaced, "denied": actUnreadable, "swapped": actRaced, "vanished": actRaced,
		"unknown": actUnreadable}
	for name, status := range checks {
		if rows[name].Status != status {
			t.Errorf("row %s status %q want %q", name, rows[name].Status, status)
		}
	}
	if rows["unknown"].Type != actUnknown || rows["unknown"].MetadataSHA256 != nil || rows["theirs"].MetadataSHA256 == nil {
		t.Errorf("unknown/foreign rows %+v %+v", rows["unknown"], rows["theirs"])
	}
	own := rows["own"]
	if own.ObservedCounts != (ActivityCounts{File: 3, Directory: 1, Symlink: 1}) || own.ObservedFileBytes != 8 || !own.Flags.DepthLimited ||
		own.Flags.RacedEntries != 1 || own.Flags.UnreadableEntries != 1 || own.Flags.ForeignEntries != 1 || !own.ChildrenExhaustive ||
		own.NewestMtimeNS == nil || *own.NewestMtimeNS != 5 {
		t.Errorf("own row %+v", own)
	}
	if _, ok := rows["fifo"]; ok {
		t.Error("a non-directory entry has a row")
	}
	if !fr.Flags.NumericOverflow || fr.ObservedFileBytes.Direct != math.MaxUint64 || fr.Flags.NameUnrecorded != 1 || fr.Exhaustive ||
		!fr.TopLevelExhaustive || fr.Flags.ForeignEntries != 1 || fr.ObservedCounts.Direct.Other != 1 ||
		fr.ObservedCounts.Direct.Unknown != 2 || fr.ObservedCounts.Direct.Directory != 6 || fr.Flags.RacedEntries != 3 || fr.Flags.UnreadableEntries != 2 {
		t.Errorf("foreign root %+v %+v", fr.Flags, fr.ObservedCounts)
	}
	// The excluded name is skipped by its exact path.
	f2 := &synthFS{t: t, roots: map[string]*synthNode{"/x": dir(1000, synthKid{"excluded", dir(1000, synthKid{"a", file(1)})}, synthKid{"kept", dir(1000)})}}
	agg2 := observeSynth(f2, nil, 0, spec("x", "/x"))
	if r := agg2.Roots[0]; r.Flags.ExcludedEntries != 1 || len(r.Rows) != 1 || r.Exhaustive || !r.TopLevelExhaustive {
		t.Errorf("exclusion %+v", r)
	}
}

func TestCursorActivityNameOnly(t *testing.T) {
	t.Parallel()
	// C1 (review round 1): enumeration is name-only; every entry's type
	// comes from one explicit lstat of a retained entry. Unknown-type and
	// disappearing entries are recorded (never dropped), the sentinel is
	// never stat'd, and the operation count is exactly the explicit one.
	var kids []synthKid
	for i := 0; i < 100; i++ {
		var n *synthNode
		switch i % 4 {
		case 0:
			// Vanished between the directory read and its lstat.
			n = &synthNode{typ: actDir, lstatErr: syscall.ENOENT}
		case 1:
			// Metadata unobtainable: an unknown entry.
			n = &synthNode{typ: actOther, lstatErr: syscall.EIO}
		case 2:
			n = &synthNode{typ: actOther, uid: 1000}
		default:
			child := []synthKid{{"gone", &synthNode{typ: actFile, lstatErr: syscall.ENOENT}}, {"f", &synthNode{typ: actFile, uid: 1000, size: 2}}}
			n = &synthNode{typ: actDir, uid: 1000, ino: uint64(200 + i), kids: child}
		}
		kids = append(kids, synthKid{fmt.Sprintf("e%07d", i), n})
	}
	f := &synthFS{t: t, roots: map[string]*synthNode{"/r": {typ: actDir, uid: 1000, ino: 1, kids: kids}}}
	r := observeSynth(f, nil, 0, spec("r", "/r")).Roots[0]
	if c := f.consumed.Load(); c != 65+16*2 {
		t.Fatalf("consumed %d", c)
	}
	// Root lstat and fstat, 64 entry lstats, 16 sampled dirs' fstat and
	// their two child lstats.
	if s := f.stats.Load(); s != 2+64+16*3 {
		t.Fatalf("%d metadata operations", s)
	}
	if f.sentinelChecks(t) != 1 {
		t.Fatal("no sentinel consumed")
	}
	d := r.ObservedCounts.Direct
	if d.Unknown != 32 || d.Other != 16 || d.Directory != 16 || d.Unknown+d.Other+d.Directory+d.File+d.Symlink != 64 {
		t.Fatalf("every sampled entry is accounted for: %+v", d)
	}
	if r.Flags.RacedEntries != 16 || r.Flags.UnreadableEntries != 16 || !r.Flags.EntryLimited || r.Exhaustive || len(r.Rows) != 48 {
		t.Fatalf("flags %+v rows %d", r.Flags, len(r.Rows))
	}
	for _, row := range r.Rows {
		switch row.Status {
		case actRaced, actUnreadable:
			if row.Type != actUnknown || row.MetadataSHA256 != nil {
				t.Fatalf("a failed lstat claimed a type or digest: %+v", row)
			}
		case actOK:
			if row.Flags.RacedEntries != 1 || row.ObservedCounts.File != 1 || row.ObservedFileBytes != 2 {
				t.Fatalf("a vanished child was dropped: %+v", row)
			}
		}
	}
}

// vanishFS is the live filesystem whose first directory read deletes one
// named entry after enumerating it (a race between read and lstat).
type vanishFS struct {
	liveActivityFS
	victim string
}

func (v vanishFS) OpenDir(path string) (activityDir, error) {
	d, err := v.liveActivityFS.OpenDir(path)
	if err != nil {
		return nil, err
	}
	return &vanishDir{activityDir: d, victim: filepath.Join(path, v.victim)}, nil
}

type vanishDir struct {
	activityDir
	victim string
	lstats int
}

func (d *vanishDir) Next(n int) ([]string, error) {
	names, err := d.activityDir.Next(n)
	if d.victim != "" {
		os.RemoveAll(d.victim)
		d.victim = ""
	}
	return names, err
}

func (d *vanishDir) Lstat(name string) (activityStat, error) {
	d.lstats++
	return d.activityDir.Lstat(name)
}

func TestCursorActivityLiveNameOnly(t *testing.T) {
	t.Parallel()
	// The live descriptor's enumeration is name-only (Readdirnames): a
	// directory removed after it was enumerated is a raced row of unknown
	// type (no type from the directory read), never dropped; a 70-entry
	// root yields 64 names plus its sentinel and exactly 64 lstats.
	root := t.TempDir()
	for i := 0; i < 70; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d%02d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	names, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	// The first name in directory order is the one that will vanish.
	d, err := liveActivityFS{}.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.Next(1)
	d.Close()
	if err != nil || len(first) != 1 || len(names) != 70 {
		t.Fatalf("names %v %v", first, err)
	}
	var vd *vanishDir
	fsys := vanishFS{victim: first[0]}
	o := activityObserver{fs: wrapOpen{fsys, func(d *vanishDir) { vd = d }}, clock: realClock{}, uid: uint32(os.Getuid())}
	r := o.observe("run", "before", []activityRootSpec{spec(activityScopeTmp, root)}, time.Now().Add(activityScanBudget)).Roots[0]
	gone := rowsByName(r.Rows)[first[0]]
	if gone.Status != actRaced || gone.Type != actUnknown || r.Flags.RacedEntries != 1 || len(r.Rows) != 64 || !r.Flags.EntryLimited ||
		r.ObservedCounts.Direct.Directory != 63 || r.ObservedCounts.Direct.Unknown != 1 {
		t.Fatalf("vanished %+v root %+v %+v", gone, r.Flags, r.ObservedCounts.Direct)
	}
	if vd == nil || vd.lstats != 64 {
		t.Fatalf("root lstats %v", vd)
	}
}

// wrapOpen reports the root's wrapped directory.
type wrapOpen struct {
	vanishFS
	seen func(*vanishDir)
}

func (w wrapOpen) OpenDir(path string) (activityDir, error) {
	d, err := w.vanishFS.OpenDir(path)
	if vd, ok := d.(*vanishDir); ok {
		w.seen(vd)
	}
	return d, err
}

func TestCursorActivityReadEdges(t *testing.T) {
	t.Parallel()
	gen := func(n int, edit func(*synthNode)) *synthNode {
		d := bigDir(n, 2)
		edit(d)
		return d
	}
	f := &synthFS{t: t, roots: map[string]*synthNode{
		// A failed stream is unreadable, never an entry limit.
		"/fail": gen(10, func(d *synthNode) { d.nextErr = syscall.EIO }),
		// Entries delivered with EOF at exactly the bound are complete.
		"/exact": gen(64, func(d *synthNode) { d.eofWithLast = true }),
		// The sentinel delivered together with EOF is still an overflow.
		"/over": gen(65, func(d *synthNode) { d.eofWithLast = true }),
	}}
	agg := observeSynth(f, nil, 0, spec("f", "/fail"), spec("e", "/exact"), spec("o", "/over"))
	fail, exact, over := agg.Roots[0], agg.Roots[1], agg.Roots[2]
	if fail.Flags.EntryLimited || fail.TopLevelExhaustive || fail.Exhaustive || fail.Flags.UnreadableEntries != 1 || len(fail.Rows) != 10 {
		t.Fatalf("failed stream %+v", fail.Flags)
	}
	if exact.Flags.EntryLimited || !exact.TopLevelExhaustive || len(exact.Rows) != 64 || !exact.Exhaustive {
		t.Fatalf("exact %+v", exact.Flags)
	}
	if !over.Flags.EntryLimited || over.TopLevelExhaustive || len(over.Rows) != 64 {
		t.Fatalf("overflow %+v %d", over.Flags, len(over.Rows))
	}
	// A child stream failure is the row's unreadable observation.
	child := &synthNode{typ: actDir, uid: 1000, ino: 5, nextErr: syscall.EIO, kids: []synthKid{{"a", &synthNode{typ: actFile, uid: 1000}}}}
	f2 := &synthFS{t: t, roots: map[string]*synthNode{"/r": {typ: actDir, uid: 1000, ino: 4, kids: []synthKid{{"d", child}}}}}
	r := observeSynth(f2, nil, 0, spec("r", "/r")).Roots[0]
	if row := r.Rows[0]; row.ChildrenExhaustive || row.Flags.EntryLimited || row.Flags.UnreadableEntries != 1 || r.Exhaustive {
		t.Fatalf("child stream %+v", row)
	}
}

func TestCursorActivityLimits(t *testing.T) {
	t.Parallel()
	t.Run("output", func(t *testing.T) {
		t.Parallel()
		f := &synthFS{t: t, roots: map[string]*synthNode{"/c": bigDir(100, 3), "/t": bigDir(100, 3)}}
		agg := observeSynth(f, nil, activitySkeleton+2000, spec(activityScopeCache, "/c"), spec(activityScopeTmp, "/t"))
		c, tm := agg.Roots[0], agg.Roots[1]
		if !c.Flags.OutputLimited || len(c.Rows) == 0 || len(c.Rows) >= 64 || c.Exhaustive || !tm.Flags.OutputLimited || len(tm.Rows) != 0 ||
			c.ObservedCounts.Direct.Directory != 64 || tm.ObservedCounts.Direct.Directory != 64 {
			t.Fatalf("output limit %+v %d / %+v %d", c.Flags, len(c.Rows), tm.Flags, len(tm.Rows))
		}
		var sum ActivityCounts
		for _, r := range c.Rows {
			addCounts(&sum, r.ObservedCounts, new(bool))
		}
		if sum != c.ObservedCounts.Children {
			t.Fatalf("children %+v are not the retained rows' sum %+v", c.ObservedCounts.Children, sum)
		}
		if nameCoverage(c) {
			t.Fatal("an output-limited root claims name coverage")
		}
	})
	t.Run("time", func(t *testing.T) {
		t.Parallel()
		clock := newTickClock(100 * time.Millisecond)
		f := &synthFS{t: t, roots: map[string]*synthNode{"/c": bigDir(1000, 1000), "/t": bigDir(10, 10)}, clock: clock}
		agg := observeSynth(f, clock, 0, spec(activityScopeCache, "/c"), spec(activityScopeTmp, "/t"))
		c, tm := agg.Roots[0], agg.Roots[1]
		if !c.Flags.TimeLimited || c.Exhaustive || !tm.Flags.TimeLimited || tm.Status != actUnreadable {
			t.Fatalf("time limit %+v / %+v", c, tm)
		}
		// 5s at 100ms per operation: about fifty operations, then nothing.
		if ops := f.stats.Load() + f.opens.Load(); ops > 60 {
			t.Fatalf("%d operations after the budget", ops)
		}
	})
	t.Run("saturation", func(t *testing.T) {
		t.Parallel()
		over := false
		if satAdd(math.MaxUint64-1, 2, &over) != math.MaxUint64 || !over {
			t.Fatal("no saturation")
		}
		over = false
		if satInc(1, &over) != 2 || over {
			t.Fatal("false overflow")
		}
	})
}

func TestCursorActivityLiveFS(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(outside, fmt.Sprintf("o%d", i)), nil, 0o600)
	}
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cache := mk("cache")
	mk("cache/pkg/sub")
	os.WriteFile(filepath.Join(cache, "pkg", "a.bin"), []byte("12345"), 0o600)
	os.WriteFile(filepath.Join(cache, "top.txt"), []byte("abc"), 0o600)
	// Never followed: a root-level and a child symlink to a populated dir.
	os.Symlink(outside, filepath.Join(cache, "link"))
	os.Symlink(outside, filepath.Join(cache, "pkg", "link"))
	// Never opened: FIFOs (opening one for reading would block).
	if err := unix.Mkfifo(filepath.Join(cache, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	unix.Mkfifo(filepath.Join(cache, "pkg", "fifo"), 0o600)
	locked := mk("cache/locked")
	os.WriteFile(filepath.Join(locked, "x"), nil, 0o600)
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	linkRoot := filepath.Join(root, "linkroot")
	os.Symlink(cache, linkRoot)
	o := activityObserver{fs: liveActivityFS{}, clock: realClock{}, uid: uint32(os.Getuid())}
	agg := o.observe("run", "before", []activityRootSpec{spec(activityScopeCache, cache), spec(activityScopeTmp, linkRoot),
		spec(activityScopeVarTmp, filepath.Join(root, "absent"))}, time.Now().Add(activityScanBudget))
	c := agg.Roots[0]
	if c.Status != actOK || !c.TopLevelExhaustive || c.ObservedCounts.Direct != (ActivityCounts{File: 1, Directory: 2, Symlink: 1, Other: 1}) ||
		c.ObservedFileBytes.Direct != 3 {
		t.Fatalf("cache %+v %+v", c.ObservedCounts, c.Flags)
	}
	pkg := rowsByName(c.Rows)["pkg"]
	if pkg.Status != actOK || pkg.ObservedCounts != (ActivityCounts{File: 1, Directory: 1, Symlink: 1, Other: 1}) || pkg.ObservedFileBytes != 5 ||
		!pkg.Flags.DepthLimited || !pkg.ChildrenExhaustive || pkg.MetadataSHA256 == nil {
		t.Fatalf("pkg %+v", pkg)
	}
	if os.Getuid() != 0 {
		if l := rowsByName(c.Rows)["locked"]; l.Status != actUnreadable || c.Flags.UnreadableEntries != 1 || c.Exhaustive {
			t.Fatalf("locked %+v %+v", l, c.Flags)
		}
	}
	if agg.Roots[1].Status != actNotDir || agg.Roots[2].Status != actAbsent {
		t.Fatalf("roots %+v", agg.Roots[1:])
	}
	// The descriptor-relative opens refuse a substituted symlink.
	d, err := liveActivityFS{}.OpenDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.OpenDir("link"); openStatus(err) != actRaced {
		t.Fatalf("a symlinked child opened: %v", err)
	}
	if _, err := (liveActivityFS{}).OpenDir(linkRoot); openStatus(err) != actRaced {
		t.Fatalf("a symlinked root opened: %v", err)
	}
	if _, err := d.Lstat("missing"); !errorsIsNotExist(err) {
		t.Fatalf("lstat of a missing child: %v", err)
	}
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err) || err != nil && strings.Contains(err.Error(), fs.ErrNotExist.Error())
}

func TestCursorActivityCompare(t *testing.T) {
	t.Parallel()
	sum := func(s string) *string { return &s }
	m := func(n int64) *int64 { return &n }
	row := func(name, digest string, files uint64) ActivityRow {
		r := ActivityRow{Name: name, Type: actDir, Status: actOK, ObservedCounts: ActivityCounts{File: files}, ChildrenExhaustive: true, NewestMtimeNS: m(1)}
		if digest != "" {
			r.MetadataSHA256 = sum(digest)
		}
		return r
	}
	full := func(rows ...ActivityRow) ActivityRoot {
		return ActivityRoot{Scope: activityScopeTmp, Status: actOK, Exhaustive: true, TopLevelExhaustive: true, Rows: rows}
	}
	partial := func(rows ...ActivityRow) ActivityRoot {
		r := full(rows...)
		r.TopLevelExhaustive, r.Flags.EntryLimited = false, true
		return r
	}
	agg := func(r ActivityRoot) *ActivityAggregate { return &ActivityAggregate{Roots: []ActivityRoot{r}} }
	for name, c := range map[string]struct {
		before, after ActivityRoot
		want          []string
	}{
		"new":                {full(), full(row("n", "d", 0)), []string{"n:new"}},
		"newly_observed":     {partial(), full(row("n", "d", 0)), []string{"n:newly_observed"}},
		"removed":            {full(row("r", "d", 0)), full(), []string{"r:removed"}},
		"not_observed_after": {full(row("r", "d", 0)), partial(), []string{"r:not_observed_after"}},
		"changed":            {full(row("c", "d1", 1)), full(row("c", "d2", 1)), []string{"c:changed"}},
		"sample_changed":     {full(row("c", "d", 1)), full(row("c", "d", 2)), []string{"c:sample_changed"}},
		"both":               {full(row("c", "d1", 1)), full(row("c", "d2", 2)), []string{"c:changed", "c:sample_changed"}},
		"unobserved":         {full(row("c", "", 1)), full(row("c", "d", 1)), []string{"c:metadata_unobserved"}},
		"unchanged":          {full(row("c", "d", 1)), full(row("c", "d", 1)), nil},
		"unknown_row":        {full(ActivityRow{Name: "u", Type: actUnknown, Status: actUnreadable}), full(row("n", "d", 0)), []string{"n:newly_observed", "u:removed"}},
	} {
		var got []string
		for _, ch := range compareActivity(agg(c.before), agg(c.after)) {
			got = append(got, ch.Name+":"+ch.Mark)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v want %v", name, got, c.want)
		}
	}
	if len(compareActivity(nil, agg(full()))) != 0 {
		t.Error("a missing phase compared")
	}
	if !nameCoverage(ActivityRoot{Status: actAbsent}) || nameCoverage(ActivityRoot{Status: actNotDir}) {
		t.Error("coverage of absent/not-directory roots")
	}
	if metadataDigest(activityStat{Type: actDir, Mode: 1}) == metadataDigest(activityStat{Type: actDir, Mode: 2}) {
		t.Error("the metadata digest ignores the mode")
	}
	if flagsText(ActivityFlags{}) != "none" || !strings.Contains(flagsText(ActivityFlags{OutputLimited: true, RacedEntries: 2}), "output_limited,raced_entries=2") {
		t.Error("flags text")
	}
}
