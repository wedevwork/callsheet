package taskworkspace_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskpublication"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/taskhub"
)

// External tests and benchmarks (iteration 10b) on the real hub over
// verified TLS (testkit/taskhub): the published result's metadata (UT-B7)
// and the four lifecycle benchmarks.

func put(t testing.TB, p string, b []byte, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.Remove(p)
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
}

// publish runs one task end to end on h with edit applied in its work
// directory and returns the settled result.
func publish(t testing.TB, h *taskhub.Hub, w *taskhub.Worker, n int, b contract.WorkspaceBinding, edit func(work string)) contract.TaskResultBody {
	t.Helper()
	tk, a := h.NewTask(t, n, b)
	dir, p, err := w.Prepare(context.Background(), t, h, tk)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(filepath.Join(dir, taskworkspace.WorkName))
	}
	cp := w.Seal(context.Background(), t, dir, p, taskhub.Candidate(tk, 0))
	var j taskhub.Checkpoints
	pub, err := taskworkspace.Publish(context.Background(), taskhub.Input(tk, dir, cp, h.API(tk.TaskID, a), &j))
	if err != nil || pub.Result == nil {
		t.Fatalf("publish: %+v %v", pub, err)
	}
	return *pub.Result
}

// kindsExpect is TestMetadata/kinds' expected change rows and the diffstat
// counters that depend on them.
type kindsExpect struct {
	rows            []string
	added, newBytes int64
}

// kindsWant selects kindsExpect by the outcome of creating the non-UTF-8
// name "\xff\xfe" in the work tree (createErr): created (Linux), every row;
// refused for its encoding (APFS: EILSEQ), the same rows without that
// empty file. Any other create error fails.
func kindsWant(createErr error, sentinel string) (kindsExpect, error) {
	switch {
	case createErr == nil:
		return kindsExpect{
			rows: []string{`"bin" added - 100644`, `"ctl\x01" added - 100644`, `"del" deleted 100644 -`, `"ln" added - 120000`,
				`"mod" modified 100644 100644`, `"tx" modified 100644 100755`, `"\xff\xfe" added - 100644`},
			added: 4, newBytes: int64(3 + 0 + 0 + 4 + len("new "+sentinel) + 1),
		}, nil
	case errors.Is(createErr, syscall.EILSEQ):
		return kindsExpect{
			rows: []string{`"bin" added - 100644`, `"ctl\x01" added - 100644`, `"del" deleted 100644 -`, `"ln" added - 120000`,
				`"mod" modified 100644 100644`, `"tx" modified 100644 100755`},
			added: 3, newBytes: int64(3 + 0 + 4 + len("new "+sentinel) + 1),
		}, nil
	}
	return kindsExpect{}, fmt.Errorf("create the non-UTF-8 name: %w", createErr)
}

// TestMetadataKindsSelector pins kindsWant: a created name keeps every row,
// a filesystem that refuses the name's encoding (APFS: EILSEQ) omits only
// that empty file, and any other create error fails.
func TestMetadataKindsSelector(t *testing.T) {
	t.Parallel()
	const sentinel = "S"
	full, err := kindsWant(nil, sentinel)
	if err != nil || full.added != 4 || full.newBytes != int64(3+0+0+4+len("new "+sentinel)+1) || len(full.rows) != 7 || full.rows[6] != `"\xff\xfe" added - 100644` {
		t.Fatalf("created: %+v %v", full, err)
	}
	refused, err := kindsWant(&os.PathError{Op: "open", Path: "work/\xff\xfe", Err: syscall.EILSEQ}, sentinel)
	want := []string{`"bin" added - 100644`, `"ctl\x01" added - 100644`, `"del" deleted 100644 -`, `"ln" added - 120000`,
		`"mod" modified 100644 100644`, `"tx" modified 100644 100755`}
	if err != nil || strings.Join(refused.rows, "|") != strings.Join(want, "|") || refused.added != 3 || refused.newBytes != int64(3+0+4+len("new "+sentinel)+1) {
		t.Fatalf("encoding refused: %+v %v", refused, err)
	}
	if _, err := kindsWant(&os.PathError{Op: "open", Path: "work/\xff\xfe", Err: syscall.EIO}, sentinel); err == nil {
		t.Fatal("an I/O error selected an outcome")
	}
}

// TestMetadata is UT-B7 (iteration 10b): a published result's tree
// metadata as the plane recomputes it: exact counters, kinds, modes and
// sizes, raw-byte sorted paths (control and non-UTF-8 names as base64),
// links without their targets, the 100-row and 32 KiB bounds with exact
// totals, and no file content anywhere. Delegated from tests/function
// (TestTaskWorkspaceMetadata/matrix). Do not rename it.
func TestMetadata(t *testing.T) {
	t.Parallel()
	h := taskhub.Start(t, taskhub.Options{})
	inst := h.Workspace(t, "proj")
	h.Seed(t, "proj", inst, "main", plumbing.ZeroHash, map[string]testkit.FileSpec{
		"keep": {Mode: filemode.Regular, Content: []byte("k")}, "mod": {Mode: filemode.Regular, Content: []byte("old")},
		"del": {Mode: filemode.Regular, Content: []byte("gone!")}, "tx": {Mode: filemode.Regular, Content: []byte("x")}})
	w := taskhub.NewWorker(t, h, 0, runtime.GOOS)
	b := h.Binding(t, "proj", inst, nil)
	const sentinel = "METADATA-SENTINEL-10b"
	t.Run("kinds", func(t *testing.T) {
		var exp kindsExpect
		res := publish(t, h, w, 1, b, func(work string) {
			put(t, filepath.Join(work, "mod"), []byte("new "+sentinel), 0o644)
			os.Remove(filepath.Join(work, "del"))
			put(t, filepath.Join(work, "bin"), []byte{0, 1, 0xff}, 0o644)
			os.Symlink("keep", filepath.Join(work, "ln"))
			os.Chmod(filepath.Join(work, "tx"), 0o755)
			put(t, filepath.Join(work, "ctl\x01"), nil, 0o644)
			// APFS refuses a non-UTF-8 name; kindsWant selects the rows by
			// the real create's outcome.
			illegal := filepath.Join(work, "\xff\xfe")
			os.Remove(illegal)
			var err error
			if exp, err = kindsWant(os.WriteFile(illegal, nil, 0o644), sentinel); err != nil {
				t.Fatal(err)
			}
			os.MkdirAll(filepath.Join(work, "emptydir"), 0o755)
		})
		d := res.Workspace
		var got []string
		for _, c := range d.Changes {
			p, _ := base64.StdEncoding.DecodeString(c.PathBase64)
			om, nm := "-", "-"
			if c.OldMode != nil {
				om = *c.OldMode
			}
			if c.NewMode != nil {
				nm = *c.NewMode
			}
			got = append(got, fmt.Sprintf("%q %s %s %s", p, c.Kind, om, nm))
		}
		if strings.Join(got, "|") != strings.Join(exp.rows, "|") {
			t.Fatalf("changes\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(exp.rows, "\n"))
		}
		ds := d.Diffstat
		if ds.Added != exp.added || ds.Modified != 2 || ds.Deleted != 1 || ds.OldBytes != 3+5+1 || ds.NewBytes != exp.newBytes {
			t.Fatalf("diffstat %+v", ds)
		}
		enc, _ := contract.Encode(res)
		if bytes.Contains(enc, []byte(sentinel)) || bytes.Contains(enc, []byte("keep\"")) {
			t.Fatal("file content or a link target reached the result")
		}
	})
	t.Run("bounds", func(t *testing.T) {
		for _, c := range []struct {
			n, pathLen int
			truncated  bool
		}{{100, 1, false}, {101, 1, true}, {150, 200, true}} {
			res := publish(t, h, w, 10+c.n, b, func(work string) {
				for i := 0; i < c.n; i++ {
					put(t, filepath.Join(work, "m", fmt.Sprintf("%s%04d", strings.Repeat("p", c.pathLen), i)), []byte("x"), 0o644)
				}
			})
			d := res.Workspace
			enc, _ := contract.Encode(*d)
			if d.ChangesTruncated != c.truncated || d.Diffstat.Added != int64(c.n) || len(enc) > contract.MaxWorkspaceDTOBytes || len(d.Changes) > contract.MaxWorkspaceChanges {
				t.Fatalf("%d paths of %d: %d rows truncated %v, %d bytes, %+v", c.n, c.pathLen, len(d.Changes), d.ChangesTruncated, len(enc), d.Diffstat)
			}
			if c.truncated && (d.NextAfter == nil || *d.NextAfter != d.Changes[len(d.Changes)-1].PathBase64) {
				t.Fatalf("next_after %v", d.NextAfter)
			}
			if !sort.SliceIsSorted(d.Changes, func(i, j int) bool {
				a, _ := base64.StdEncoding.DecodeString(d.Changes[i].PathBase64)
				b, _ := base64.StdEncoding.DecodeString(d.Changes[j].PathBase64)
				return bytes.Compare(a, b) < 0
			}) {
				t.Fatal("rows are not sorted by raw path bytes")
			}
		}
	})
}

// ---- Benchmarks ----

// taskSeq numbers fixture tasks across every benchmark run of the process
// (the N=1 probe and the timed run share hubs: a task ID is never reused).
var taskSeq atomic.Int64

func nextTask() int { return int(taskSeq.Add(1)) + 1000 }

const (
	benchFiles    = 1024
	benchBlobSize = 4 << 10
	initialLimit  = 8 << 20
	incrementMax  = 256 << 10
)

// benchBlob is 09a's deterministic pseudorandom 4 KiB blob i, version v.
func benchBlob(i, v int) []byte {
	r := rand.New(rand.NewPCG(uint64(i), uint64(v)+0x5eed))
	b := make([]byte, benchBlobSize)
	for j := 0; j < len(b); j += 8 {
		binary.LittleEndian.PutUint64(b[j:], r.Uint64())
	}
	return b
}

func benchFilesV(v, changed int) map[string]testkit.FileSpec {
	out := map[string]testkit.FileSpec{}
	for i := 0; i < benchFiles; i++ {
		ver := 0
		if i < changed {
			ver = v
		}
		out[fmt.Sprintf("d%02d/f%04d.bin", i%32, i)] = testkit.FileSpec{Mode: filemode.Regular, Content: benchBlob(i, ver)}
	}
	return out
}

// benchHubs are the per-process seeded hubs, by history length: seeding is
// fixture setup, shared by the benchmarks (each task is fresh).
var benchHubs = map[int]*benchFixture{}

type benchFixture struct {
	h       *taskhub.Hub
	b       contract.WorkspaceBinding
	commits []plumbing.Hash
}

// benchHub is a hub with a 1,024 x 4 KiB base of history length depth (one
// changed blob per commit), started once per process.
func benchHub(b *testing.B, depth int) (*taskhub.Hub, contract.WorkspaceBinding, []plumbing.Hash) {
	b.Helper()
	if f := benchHubs[depth]; f != nil {
		return f.h, f.b, f.commits
	}
	h, bind, commits := seedBenchHub(benchProcess{b}, depth)
	benchHubs[depth] = &benchFixture{h, bind, commits}
	return h, bind, commits
}

// benchProcess is a testing.TB whose cleanups and temporary directories
// last until TestMain's teardown (the shared hubs outlive one benchmark).
type benchProcess struct{ *testing.B }

var (
	processCleanups []func()
	processDirs     []string
)

func (benchProcess) Cleanup(f func()) { processCleanups = append(processCleanups, f) }

func (p benchProcess) TempDir() string {
	d, err := os.MkdirTemp("", "cs10b-bench-")
	if err != nil {
		p.Fatal(err)
	}
	processDirs = append(processDirs, d)
	return d
}

// TestMain stops the shared benchmark hubs and removes their directories.
func TestMain(m *testing.M) {
	code := m.Run()
	for i := len(processCleanups) - 1; i >= 0; i-- {
		processCleanups[i]()
	}
	for _, d := range processDirs {
		os.RemoveAll(d)
	}
	os.Exit(code)
}

func seedBenchHub(b testing.TB, depth int) (*taskhub.Hub, contract.WorkspaceBinding, []plumbing.Hash) {
	h := taskhub.Start(b, taskhub.Options{})
	inst := h.Workspace(b, "bench")
	var commits []plumbing.Hash
	old := plumbing.ZeroHash
	for v := 0; v < depth; v++ {
		var parents []plumbing.Hash
		if v > 0 {
			parents = []plumbing.Hash{old}
		}
		c := h.Seed(b, "bench", inst, "main", old, benchFilesV(v, v), parents...)
		commits = append(commits, c)
		old = c
	}
	return h, h.Binding(b, "bench", inst, nil), commits
}

// BenchmarkTaskWorkspacePrepare measures a task's preparation over the
// node route: cold (empty cache) and warm (the previous base cached),
// history length 1 and 8, including its cleanup, and a cancelled
// preparation's cleanup. It reports fetched, saved and copied bytes and
// the independent copy's disk bytes; the initial transfer is below 8 MiB
// and a warm one-change fetch below 256 KiB and 10% of it.
func BenchmarkTaskWorkspacePrepare(b *testing.B) {
	for _, depth := range []int{1, 8} {
		h, bind, commits := benchHub(b, depth)
		b.Run(fmt.Sprintf("cold/history=%d", depth), func(b *testing.B) {
			b.ReportAllocs()
			var st taskworkspace.FetchStats
			for i := 0; i < b.N; i++ {
				w := taskhub.NewWorker(b, h, 0, runtime.GOOS)
				tk, _ := h.NewTask(b, nextTask(), bind)
				dir, p, err := w.Prepare(context.Background(), b, h, tk)
				if err != nil {
					b.Fatal(err)
				}
				st = p.Stats
				if st.Hit || st.Fetched <= 0 || st.Fetched >= initialLimit || p.Files != benchFiles {
					b.Fatalf("cold %+v files %d", st, p.Files)
				}
				size, _ := dirSize(filepath.Join(dir, taskworkspace.ObjectsName))
				b.ReportMetric(float64(size), "copy-disk-bytes")
				if err := os.RemoveAll(dir); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(st.Fetched), "fetched-bytes")
			b.ReportMetric(float64(st.Copied), "copied-bytes")
		})
		b.Run(fmt.Sprintf("warm/history=%d", depth), func(b *testing.B) {
			b.ReportAllocs()
			// A cache warmed with the base's parent (or the base itself): each
			// run starts from a copy of the same warmed cache, so its first
			// iteration fetches the increment.
			wc := warmCacheOf(b, h, bind, commits, depth)
			root := filepath.Join(b.TempDir(), "worker")
			if err := copyTree(root, wc.root); err != nil {
				b.Fatal(err)
			}
			cache, err := taskworkspace.OpenCache(context.Background(), taskworkspace.CacheOptions{Root: root, Origin: h.URL})
			if err != nil {
				b.Fatal(err)
			}
			w := &taskhub.Worker{Root: root, Cache: cache, GOOS: runtime.GOOS}
			p0 := wc.p0
			b.ResetTimer()
			var st, first taskworkspace.FetchStats
			for i := 0; i < b.N; i++ {
				tk, _ := h.NewTask(b, nextTask(), bind)
				dir, p, err := w.Prepare(context.Background(), b, h, tk)
				if err != nil {
					b.Fatal(err)
				}
				st = p.Stats
				if !st.Hit || (depth > 1 && (st.Fetched >= incrementMax || st.Fetched*10 >= p0.Stats.Fetched)) {
					b.Fatalf("warm %+v (initial %d)", st, p0.Stats.Fetched)
				}
				if i == 0 {
					first = st
				}
				os.RemoveAll(dir)
			}
			// The first iteration fetches the base over the cached parent
			// (history 8: the one-change increment; history 1: the base
			// itself is cached); later ones find the base cached.
			b.ReportMetric(float64(first.Fetched), "incremental-fetched-bytes")
			b.ReportMetric(float64(st.Fetched), "fetched-bytes")
			b.ReportMetric(float64(st.Saved), "saved-bytes")
			b.ReportMetric(float64(st.Copied), "copied-bytes")
		})
		b.Run(fmt.Sprintf("cancelled/history=%d", depth), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				w := taskhub.NewWorker(b, h, 0, runtime.GOOS)
				tk, _ := h.NewTask(b, nextTask(), bind)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				dir, _, err := w.Prepare(ctx, b, h, tk)
				if err == nil {
					b.Fatal("a cancelled preparation succeeded")
				}
				if err := os.RemoveAll(dir); err != nil {
					b.Fatal(err)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					b.Fatal("work retained after a cancelled preparation")
				}
			}
		})
	}
}

// warmCache is a worker state root whose cache holds a benchmark base's
// parent (history 1: the base itself), warmed once per process by a cold
// preparation p0 (fixture setup; each warm run copies it).
type warmCache struct {
	root string
	p0   *taskworkspace.Prepared
}

var warmCaches = map[int]*warmCache{}

func warmCacheOf(b *testing.B, h *taskhub.Hub, bind contract.WorkspaceBinding, commits []plumbing.Hash, depth int) *warmCache {
	b.Helper()
	if c := warmCaches[depth]; c != nil {
		return c
	}
	tb := benchProcess{b}
	w := taskhub.NewWorker(tb, h, 0, runtime.GOOS)
	prev := commits[len(commits)-1]
	if depth > 1 {
		prev = commits[len(commits)-2]
	}
	c := prev.String()
	warm := bind
	warm.BaseCommit, warm.BaseSelector = &c, c
	tk, _ := h.NewTask(tb, nextTask(), warm)
	cold, p0, err := w.Prepare(context.Background(), tb, h, tk)
	if err != nil {
		b.Fatal(err)
	}
	if err := os.RemoveAll(cold); err != nil {
		b.Fatal(err)
	}
	warmCaches[depth] = &warmCache{root: w.Root, p0: p0}
	return warmCaches[depth]
}

// copyTree is testkit.CopyTree (mode-preserving fixture copies).
func copyTree(dst, src string) error { return testkit.CopyTree(dst, src) }

func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return err
	})
	return n, err
}

// BenchmarkTaskWorkspaceSnapshot measures the result snapshot of a 1,024 x
// 4 KiB checkout: unchanged, one changed file, every file changed; it
// reports scanned paths, bytes read and objects created.
func BenchmarkTaskWorkspaceSnapshot(b *testing.B) {
	h, bind, _ := benchHub(b, 1)
	w := taskhub.NewWorker(b, h, 0, runtime.GOOS)
	// One checkout per case, prepared once for the probe and the timed run
	// (fixture setup); each iteration writes fresh versions of the changed
	// files outside the timer (a process-wide version: new blobs every
	// time, in every run).
	type checkout struct {
		dir string
		p   *taskworkspace.Prepared
	}
	checkouts := map[string]checkout{}
	version := 99
	for _, c := range []struct {
		name    string
		changed int
	}{{"unchanged", 0}, {"one-change", 1}, {"full-change", benchFiles}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			co, ok := checkouts[c.name]
			if !ok {
				tk, _ := h.NewTask(b, nextTask(), bind)
				dir, p, err := w.Prepare(context.Background(), b, h, tk)
				if err != nil {
					b.Fatal(err)
				}
				co = checkout{dir, p}
				checkouts[c.name] = co
			}
			dir, p := co.dir, co.p
			work := filepath.Join(dir, taskworkspace.WorkName)
			b.ResetTimer()
			var last struct{ files, bytes, objects int64 }
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				version++
				for j := 0; j < c.changed; j++ {
					put(b, filepath.Join(work, fmt.Sprintf("d%02d/f%04d.bin", j%32, j)), benchBlob(j, version), 0o644)
				}
				b.StartTimer()
				_, s, err := taskworkspace.Snapshot(context.Background(), taskworkspace.SnapshotInput{GOOS: w.GOOS, TaskDir: dir, Map: p.Map})
				if err != nil || s.Files != benchFiles {
					b.Fatalf("snapshot %+v %v", s, err)
				}
				last.files, last.bytes, last.objects = s.Files, s.Bytes, s.Objects
			}
			b.ReportMetric(float64(last.files), "scanned-paths")
			b.ReportMetric(float64(last.bytes), "bytes-read")
			b.ReportMetric(float64(last.objects), "objects-created")
		})
	}
}

// countingAPI counts the pack bytes its receive sessions send.
type countingAPI struct {
	taskworkspace.PlaneAPI
	n *atomic.Int64
}

func (c countingAPI) Pusher(pubID string) (taskworkspace.Pusher, error) {
	p, err := c.PlaneAPI.Pusher(pubID)
	if err != nil {
		return nil, err
	}
	return countingPusher{p, c.n}, nil
}

type countingPusher struct {
	taskworkspace.Pusher
	n *atomic.Int64
}

func (c countingPusher) PushTask(ctx context.Context, ref string, commit plumbing.Hash, pack io.Reader) error {
	return c.Pusher.PushTask(ctx, ref, commit, countingReader{pack, c.n})
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// BenchmarkTaskWorkspaceMetadata measures the plane's recomputed result
// metadata and its bounded DTO for 100 and 10,000 changed paths: exact
// totals and a DTO of at most 32 KiB.
func BenchmarkTaskWorkspaceMetadata(b *testing.B) {
	// One hub per path count, seeded once for the probe and the timed run
	// (fixture setup).
	type seeded struct {
		h            *taskhub.Hub
		inst         string
		base, commit plumbing.Hash
		bind         contract.WorkspaceBinding
	}
	hubs := map[int]seeded{}
	for _, n := range []int{100, 10000} {
		b.Run(fmt.Sprintf("paths=%d", n), func(b *testing.B) {
			sd, ok := hubs[n]
			if !ok {
				tb := benchProcess{b}
				sd.h = taskhub.Start(tb, taskhub.Options{})
				sd.inst = sd.h.Workspace(tb, "meta")
				sd.base = sd.h.Seed(tb, "meta", sd.inst, "main", plumbing.ZeroHash, map[string]testkit.FileSpec{"seed": {Mode: filemode.Regular, Content: []byte("s")}})
				files := map[string]testkit.FileSpec{"seed": {Mode: filemode.Regular, Content: []byte("s")}}
				for i := 0; i < n; i++ {
					files[fmt.Sprintf("m%03d/p%05d.txt", i%100, i)] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte(fmt.Sprint(i))}
				}
				sd.commit = sd.h.Seed(tb, "meta", sd.inst, "next", plumbing.ZeroHash, files, sd.base)
				sd.bind = sd.h.Binding(tb, "meta", sd.inst, nil)
				hubs[n] = sd
			}
			h, inst, base, commit, bind := sd.h, sd.inst, sd.base, sd.commit, sd.bind
			b.ReportAllocs()
			b.ResetTimer()
			var size int
			for i := 0; i < b.N; i++ {
				md, err := h.M.TaskDiff(context.Background(), "meta", inst, base, commit)
				if err != nil {
					b.Fatal(err)
				}
				dto, err := taskpublication.PublishedDTO(bind, taskhub.TaskID(1), commit.String(), md)
				if err != nil {
					b.Fatal(err)
				}
				enc, _ := contract.Encode(dto)
				size = len(enc)
				if dto.Diffstat.Added != int64(n) || size > contract.MaxWorkspaceDTOBytes || len(dto.Changes) > contract.MaxWorkspaceChanges || (n > 100) != dto.ChangesTruncated {
					b.Fatalf("metadata %+v %d bytes truncated %v", dto.Diffstat, size, dto.ChangesTruncated)
				}
			}
			b.ReportMetric(float64(size), "dto-bytes")
		})
	}
}
