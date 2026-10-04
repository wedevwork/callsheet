package workspacetransfer

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 09b benchmarks: the 09a fixture of 1,024 deterministic
// pseudorandom 4 KiB files (one changed per incremental round) as a git
// source and as a plain folder. Every operation runs with the production
// deps (real syncs) and includes scanning, staging, closure verification,
// sync, publication and cleanup; only fixture creation and resets run
// outside the timer. Exact hashes, trees and the unchanged checkout are
// asserted on every iteration; payload bounds are asserted (09a's initial
// < 8 MiB and incremental push plus fetch < 256 KiB and < 10% of the
// initial for the git source; the folder path's mandatory full-history
// fetch is reported separately and not bounded). Timings are reported,
// never gated. Every benchmark uses b.Loop, so its setup runs once per
// invocation (not again for a one-iteration probe) and -benchtime=3x runs
// exactly three measured operations.
const (
	benchFiles        = 1024
	benchBlobSize     = 4 << 10
	initialLimitBytes = 8 << 20
	incrementLimit    = 256 << 10
)

// blobCache memoizes fixture blobs (fixture generation stays outside the
// measured work and cheap across benchmark runs).
var blobCache sync.Map

func benchBlob(i, v int) string {
	key := [2]int{i, v}
	if b, ok := blobCache.Load(key); ok {
		return b.(string)
	}
	r := rand.New(rand.NewPCG(uint64(i), uint64(v)+0x5eed))
	b := make([]byte, benchBlobSize)
	for j := 0; j < len(b); j += 8 {
		binary.LittleEndian.PutUint64(b[j:], r.Uint64())
	}
	blobCache.Store(key, string(b))
	return string(b)
}

// benchTree is the fixture (version v of file changed) with nested
// .gitignore files.
func benchTree(changed, v int) map[string]fspec {
	files := map[string]fspec{".gitignore": reg("*.tmp\n!keep.tmp\n"), "d00/.gitignore": reg("/local\n")}
	for i := 0; i < benchFiles; i++ {
		ver := 0
		if i == changed {
			ver = v
		}
		files[fmt.Sprintf("d%02d/f%04d.bin", i%32, i)] = reg(benchBlob(i, ver))
	}
	return files
}

func BenchmarkTransferStatus(b *testing.B) {
	r := newRepo(b, benchTree(-1, 0))
	env := emptyEnv(b)
	for _, dirty := range []bool{false, true} {
		b.Run(map[bool]string{false: "clean", true: "dirty"}[dirty], func(b *testing.B) {
			if dirty {
				r.writeFile("d05/f0005.bin", reg(benchBlob(5, 9)))
			}
			var files, bytes int64
			d := defaultDeps()
			d.metric = func(name string, v int64) {
				switch name {
				case "status-files":
					files = v
				case "status-bytes":
					bytes = v
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				rp, err := openRepo(context.Background(), env, r.root)
				if err != nil {
					b.Fatal(err)
				}
				_, err = checkClean(context.Background(), d, rp, env, hostGOOS())
				rp.close()
				if dirty != (contract.TransferReason(err) == contract.ReasonDirtySource) || (!dirty && err != nil) {
					b.Fatalf("status: %v", err)
				}
			}
			b.StopTimer()
			if files != benchFiles+2 || bytes < benchFiles*benchBlobSize {
				b.Fatalf("visited %d files, read %d bytes", files, bytes)
			}
			b.ReportMetric(float64(files), "files/op")
			b.ReportMetric(float64(bytes), "bytesread/op")
		})
	}
}

// benchPlane is a production test plane for benchmarks.
func benchPlane(b *testing.B) *testPlane { return newTestPlane(b) }

// historyStore holds n commits of the fixture (each changing one file)
// and a tip whose tree is either the fixture itself (same) or the fixture
// with one file changed.
var historyCache sync.Map

func historyStore(b *testing.B, n int, same bool) (*testkitStore, plumbing.Hash) {
	b.Helper()
	type cached struct {
		s *testkitStore
		h plumbing.Hash
	}
	key := fmt.Sprint(n, same)
	if c, ok := historyCache.Load(key); ok {
		return c.(cached).s, c.(cached).h
	}
	st, h := buildHistory(b, n, same)
	historyCache.Store(key, cached{st, h})
	return st, h
}

func buildHistory(b *testing.B, n int, same bool) (*testkitStore, plumbing.Hash) {
	b.Helper()
	s := testkit.NewMemoryStore()
	var parent plumbing.Hash
	for c := 0; c < n; c++ {
		files := benchTree(c, c+1)
		if c == n-1 {
			if same {
				files = benchTree(-1, 0)
			} else {
				files = benchTree(3, 99)
			}
		}
		specs := map[string]testkit.FileSpec{}
		for p, f := range files {
			specs[p] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
		}
		var ps []plumbing.Hash
		if !parent.IsZero() {
			ps = []plumbing.Hash{parent}
		}
		h, err := testkit.CommitFiles(s, specs, ps, fmt.Sprintf("c%d", c))
		if err != nil {
			b.Fatal(err)
		}
		parent = h
	}
	return &testkitStore{s}, parent
}

type testkitStore struct{ s *memory.Storage }

// ensure returns name's view, creating it if needed (a benchmark body
// runs more than once).
func (tp *testPlane) ensure(b *testing.B, name string) contract.WorkspaceView {
	b.Helper()
	if v, err := tp.m().Show(context.Background(), name); err == nil {
		return v
	}
	return tp.create(b, name)
}

// setMain moves (or with zero deletes) name's main to h on the plane.
func (tp *testPlane) setMain(b *testing.B, name, instance string, h plumbing.Hash) {
	b.Helper()
	cur := tp.refs(b, name)[contract.DefaultBranchRef]
	in := contract.RefSetIntent{Instance: instance, Ref: contract.DefaultBranchRef, Expected: cur}
	switch {
	case h.IsZero() && cur == "":
		return
	case h.IsZero():
		in.Delete = true
	default:
		in.Target = contract.Selector{Kind: contract.SelectorKindHash, Value: h.String()}
	}
	if _, err := tp.m().SetRef(context.Background(), name, in); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkTransferSnapshot(b *testing.B) {
	env := emptyEnv(b)
	tp := benchPlane(b)
	p := tp.plane(b)
	folder := writeFolder(b, benchTree(-1, 0), nil)
	cases := []struct {
		name    string
		history int
		same    bool
	}{{"initial", 0, false}, {"unchanged/history=1", 1, true}, {"one-change/history=8", 8, false}}
	for ci, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			name := fmt.Sprintf("snap%d", ci)
			v := tp.ensure(b, name)
			parent := plumbing.ZeroHash
			if c.history > 0 {
				var hs *testkitStore
				hs, parent = historyStore(b, c.history, c.same)
				if _, seeded := tp.refs(b, name)[contract.DefaultBranchRef]; !seeded {
					if res := tp.remote(b, name, v.Instance).Push(context.Background(), hs.s, contract.DefaultBranchRef, plumbing.ZeroHash, parent); !res.OK() {
						b.Fatal(res.Err)
					}
				}
			}
			var in, out, objs, bytesRead int64
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				tp.setMain(b, name, v.Instance, parent)
				req0, res0 := tp.gitReq.Load(), tp.gitRes.Load()
				d := defaultDeps()
				d.metric = func(n string, v int64) {
					switch n {
					case "snapshot-objects":
						objs = v
					case "snapshot-bytes-read":
						bytesRead = v
					}
				}
				b.StartTimer()
				r, err := d.push(context.Background(), Options{GOOS: hostGOOS(), Env: env, Cwd: "/", Plane: p},
					PushRequest{Name: name, Instance: v.Instance, Path: folder, PathSet: true})
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				in, out = tp.gitRes.Load()-res0, tp.gitReq.Load()-req0
				if c.same == r.Changed || (!parent.IsZero() && (r.OldCommit == nil || *r.OldCommit != parent.String())) {
					b.Fatalf("result %+v", r)
				}
				if tp.refs(b, name)[contract.DefaultBranchRef] != r.Commit {
					b.Fatal("branch is not the snapshot")
				}
				if want := snapshotCommitHash(b, folderTree(b), parent).String(); (!c.same && r.Commit != want) || (c.same && r.Commit != parent.String()) {
					b.Fatal("snapshot identity")
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(in), "inbound-bytes/op")
			b.ReportMetric(float64(out), "outbound-bytes/op")
			b.ReportMetric(float64(objs), "objects-created/op")
			b.ReportMetric(float64(bytesRead), "bytes-read/op")
		})
	}
}

var folderTreeOnce struct {
	sync.Once
	h plumbing.Hash
}

// folderTree is the fixture's tree hash, computed independently.
func folderTree(b *testing.B) plumbing.Hash {
	folderTreeOnce.Do(func() {
		specs := map[string]testkit.FileSpec{}
		for p, f := range benchTree(-1, 0) {
			specs[p] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
		}
		folderTreeOnce.h, _ = testkit.WriteTree(testkit.NewMemoryStore(), specs)
	})
	return folderTreeOnce.h
}

// benchSource is a git repository holding the fixture.
func benchSource(b *testing.B) *fixtureRepo { return newRepo(b, benchTree(-1, 0)) }

func BenchmarkTransferPush(b *testing.B) {
	env := emptyEnv(b)
	tp := benchPlane(b)
	p := tp.plane(b)
	r := benchSource(b)
	b.Run("initial", func(b *testing.B) {
		v := tp.ensure(b, "initial")
		b.ReportAllocs()
		b.ResetTimer()
		var payload int64
		for b.Loop() {
			b.StopTimer()
			tp.setMain(b, "initial", v.Instance, plumbing.ZeroHash)
			req0 := tp.gitReq.Load()
			b.StartTimer()
			res, err := pushWith(b, defaultDeps(), p, env, "initial", v.Instance, "", r.root)
			b.StopTimer()
			if err != nil || res.Commit != r.head.String() || res.OldCommit != nil || !res.Changed {
				b.Fatalf("%+v %v", res, err)
			}
			payload = tp.gitReq.Load() - req0
			if payload >= initialLimitBytes {
				b.Fatalf("initial payload %d", payload)
			}
			b.StartTimer()
		}
		b.ReportMetric(float64(payload), "payload-bytes/op")
	})
	b.Run("incremental", func(b *testing.B) {
		v := tp.ensure(b, "ws")
		tp.setMain(b, "ws", v.Instance, plumbing.ZeroHash)
		req0 := tp.gitReq.Load()
		if _, err := pushWith(b, defaultDeps(), p, env, "ws", v.Instance, "", r.root); err != nil {
			b.Fatal(err)
		}
		initial := tp.gitReq.Load() - req0
		dst := newRepo(b, map[string]fspec{"x": reg("x")})
		if _, err := pullWith(b, fastDeps(), p, env, "main", dst.root); err != nil {
			b.Fatal(err)
		}
		before := checkoutFingerprint(b, dst.root)
		b.ReportAllocs()
		b.ResetTimer()
		var combined int64
		for i := 0; b.Loop(); i++ {
			b.StopTimer()
			r.commit(benchTree(i%benchFiles, i+1), fmt.Sprintf("inc %d", i))
			req0, res0 := tp.gitReq.Load(), tp.gitRes.Load()
			b.StartTimer()
			res, err := pushWith(b, defaultDeps(), p, env, "ws", v.Instance, "", r.root)
			if err != nil || !res.Changed {
				b.Fatalf("%+v %v", res, err)
			}
			pr, err := pullWith(b, defaultDeps(), p, env, "main", dst.root)
			b.StopTimer()
			if err != nil || pr.Commit != r.head.String() {
				b.Fatalf("%+v %v", pr, err)
			}
			if checkoutFingerprint(b, dst.root) != before {
				b.Fatal("the destination checkout changed")
			}
			combined = (tp.gitReq.Load() - req0) + (tp.gitRes.Load() - res0)
			if combined >= incrementLimit || combined*10 >= initial {
				b.Fatalf("incremental push+fetch %d bytes (initial %d)", combined, initial)
			}
			b.StartTimer()
		}
		b.ReportMetric(float64(combined), "push+fetch-bytes/op")
		b.ReportMetric(float64(initial), "initial-bytes")
	})
}

// checkoutFingerprint fingerprints a destination's working tree, index,
// HEAD, config and refs other than refs/callsheet (not objects).
func checkoutFingerprint(b *testing.B, root string) string {
	b.Helper()
	var out []string
	for _, l := range strings.Split(fingerprint(b, root), "\n") {
		if !strings.HasPrefix(l, ".git/objects") && !strings.HasPrefix(l, ".git/refs/callsheet") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// BenchmarkTransferPullGit pulls one new commit (one changed file) into a
// repository that has the previous history, selected by branch (status
// pages) or by hash (the plane's reachability diff over the whole tree).
func BenchmarkTransferPullGit(b *testing.B) {
	env := emptyEnv(b)
	tp := benchPlane(b)
	p := tp.plane(b)
	r := newRepo(b, exportTree(-1, 0))
	v := tp.create(b, "ws")
	if _, err := pushWith(b, defaultDeps(), p, env, "ws", v.Instance, "", r.root); err != nil {
		b.Fatal(err)
	}
	n := 0
	// Iteration 10c adds task-ID selection: a planted task ref pulled with
	// the paired expected instance and commit (the timed transfer), and
	// the task-ref lookup's exact-ref check (client.CheckTaskResultRef)
	// reported separately as ref-check-ns/op.
	cl := tp.client(b)
	for _, sel := range []string{"branch", "hash", "task"} {
		b.Run(sel, func(b *testing.B) {
			dst := newRepo(b, map[string]fspec{"x": reg("x")})
			if _, err := pullWith(b, fastDeps(), p, env, "main", dst.root); err != nil {
				b.Fatal(err)
			}
			before := checkoutFingerprint(b, dst.root)
			var objs, installed int64
			var check time.Duration
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				n++
				r.commit(exportTree(n%exportFilesN, n+1), fmt.Sprintf("pull %d", n))
				if _, err := pushWith(b, fastDeps(), p, env, "ws", v.Instance, "", r.root); err != nil {
					b.Fatal(err)
				}
				req := PullRequest{Name: "ws", Ref: "main", Path: dst.root, PathSet: true}
				switch sel {
				case "hash":
					req.Ref = r.head.String()
				case "task":
					id := fmt.Sprintf("t_%032x", n)
					tp.plantTask(b, "ws", id, r.head)
					req.Ref, req.ExpectedInstance, req.ExpectedCommit = contract.TaskRefPrefix+id, v.Instance, r.head.String()
					start := time.Now()
					if err := cl.CheckTaskResultRef(context.Background(), client.TaskResultSelection{Name: "ws", Instance: v.Instance, Ref: req.Ref, Commit: req.ExpectedCommit}); err != nil {
						b.Fatal(err)
					}
					check += time.Since(start)
				}
				d := defaultDeps()
				d.metric = func(n string, v int64) {
					switch n {
					case "pull-objects-verified":
						objs = v
					case "pull-objects-installed":
						installed = v
					}
				}
				b.StartTimer()
				res, err := d.pull(context.Background(), Options{GOOS: hostGOOS(), Env: env, Cwd: "/", Plane: p}, req)
				b.StopTimer()
				if err != nil || res.Commit != r.head.String() {
					b.Fatalf("%+v %v", res, err)
				}
				if readRefB(dst.root, *res.LocalRef) != r.head.String() || checkoutFingerprint(b, dst.root) != before {
					b.Fatal("ref or checkout")
				}
				if sel == "task" && *res.LocalRef != "refs/callsheet/ws/tasks/"+req.Ref[len(contract.TaskRefPrefix):] {
					b.Fatalf("task local ref %s", *res.LocalRef)
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(objs), "objects-verified/op")
			b.ReportMetric(float64(installed), "objects-installed/op")
			if sel == "task" {
				b.ReportMetric(float64(check.Nanoseconds())/float64(b.N), "ref-check-ns/op")
			}
		})
	}
}

func readRefB(root, ref string) string {
	bs, _ := os.ReadFile(filepath.Join(root, ".git", filepath.FromSlash(ref)))
	if len(bs) > 0 {
		return string(bs[:len(bs)-1])
	}
	return ""
}

// exportFilesN is the pull and export benchmarks' file count: every
// installed object and exported file is synced (the durability contract),
// so their fixture is the first 64 files of the 1,024-file one.
const exportFilesN = 64

// exportTree is benchTree limited to its first exportFilesN files.
func exportTree(changed, v int) map[string]fspec {
	files := map[string]fspec{}
	for k, f := range benchTree(changed, v) {
		var i int
		if _, err := fmt.Sscanf(filepath.Base(k), "f%04d.bin", &i); err == nil && i >= exportFilesN {
			continue
		}
		files[k] = f
	}
	return files
}

func BenchmarkTransferExport(b *testing.B) {
	env := emptyEnv(b)
	tp := benchPlane(b)
	p := tp.plane(b)
	r := newRepo(b, exportTree(-1, 0))
	v := tp.create(b, "ws")
	if _, err := pushWith(b, defaultDeps(), p, env, "ws", v.Instance, "", r.root); err != nil {
		b.Fatal(err)
	}
	var entries, objs int64
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		dest := filepath.Join(tempDir(b), "out")
		d := defaultDeps()
		d.metric = func(n string, v int64) {
			switch n {
			case "export-files":
				entries = v
			case "pull-objects-verified":
				objs = v
			}
		}
		b.StartTimer()
		res, err := pullWith(b, d, p, env, "main", dest)
		b.StopTimer()
		if err != nil || res.Commit != r.head.String() {
			b.Fatalf("%+v %v", res, err)
		}
		got, err := os.ReadFile(filepath.Join(dest, "d07/f0007.bin"))
		if err != nil || string(got) != benchBlob(7, 0) {
			b.Fatal("exported bytes")
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(entries), "entries/op")
	b.ReportMetric(float64(objs), "objects-verified/op")
	b.ReportMetric(float64(exportFilesN*benchBlobSize), "bytes-copied/op")
}
