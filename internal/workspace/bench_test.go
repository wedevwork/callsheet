package workspace

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// The iteration 01 fixture: 1,024 deterministic pseudorandom 4 KiB blobs,
// one changed per incremental round. Benchmarks use the production deps
// (real syncs): transaction timing includes the generation copy, closure
// validation, sync, publication and cleanup. Only fixture resets run
// outside the timer. Timings are reported, never gated; payload bounds
// and output are asserted on every iteration.
//
// The baseline and diff fixture pushes (outside the timer) use pack window
// 0 (setupWindow): they carry incompressible blobs and no similar trees,
// so the client's delta search costs CPU and saves no bytes. The timed
// push and prune's orphan history (three similar trees, which do delta)
// keep testkit's default window, so the stored state is unchanged.
const (
	benchFiles        = 1024
	benchBlobSize     = 4 << 10
	initialLimitBytes = 8 << 20
	incrementLimit    = 256 << 10
)

// setupWindow is the fixture pushes' pack window option.
var setupWindow = testkit.PackWindow(0)

func benchBlob(i, v int) []byte {
	r := rand.New(rand.NewPCG(uint64(i), uint64(v)+0x5eed))
	b := make([]byte, benchBlobSize)
	for j := 0; j < len(b); j += 8 {
		binary.LittleEndian.PutUint64(b[j:], r.Uint64())
	}
	return b
}

// counting counts HTTP request and response bytes of the git endpoint.
type counting struct {
	h   http.Handler
	req atomic.Int64
	res atomic.Int64
}

type countBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type countWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// Unwrap exposes the connection for the handler's deadline controller.
func (c countWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *counting) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = countBody{r.Body, &c.req}
	c.h.ServeHTTP(countWriter{w, &c.res}, r)
}

func (c *counting) take() int64 {
	return c.req.Swap(0) + c.res.Swap(0)
}

type benchEnv struct {
	m      *Manager
	srv    *httptest.Server
	cnt    *counting
	remote testkit.GitRemote
	inst   string
	local  *memory.Storage
	files  map[string]testkit.FileSpec
	head   plumbing.Hash
	tree   plumbing.Hash

	mu      sync.Mutex
	metrics map[string]int64
}

func (e *benchEnv) record(name string, v int64) {
	e.mu.Lock()
	e.metrics[name] += v
	e.mu.Unlock()
}

func (e *benchEnv) takeMetrics() map[string]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.metrics
	e.metrics = map[string]int64{}
	return m
}

func newBenchEnv(b *testing.B) *benchEnv {
	b.Helper()
	e := &benchEnv{metrics: map[string]int64{}, local: testkit.NewMemoryStore(), files: map[string]testkit.FileSpec{}}
	d := defaultDeps()
	d.metric = e.record
	e.m = openAt(b, d, tempRoot(b))
	e.cnt = &counting{h: http.HandlerFunc(e.m.ServeGit)}
	e.srv = httptest.NewUnstartedServer(e.cnt)
	e.srv.StartTLS()
	b.Cleanup(e.srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.srv.Certificate().Raw})
	hc, err := testkit.GitHTTPClient(caPEM)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(hc.CloseIdleConnections)
	v := mustCreate(b, e.m, "bench")
	e.inst = v.Instance
	e.remote = testkit.GitRemote{URL: e.srv.URL + "/ws/bench.git", Instance: v.Instance, HTTP: hc}
	for i := range benchFiles {
		e.files[fmt.Sprintf("f%04d.bin", i)] = testkit.FileSpec{Mode: filemode.Regular, Content: benchBlob(i, 0)}
	}
	e.head = commit(b, e.local, e.files, "bench baseline")
	e.tree = commitTreeOfStore(b, e.local, e.head)
	push(b, e.remote, e.local, "refs/heads/main", plumbing.ZeroHash, e.head, setupWindow)
	e.cnt.take()
	e.takeMetrics()
	return e
}

func commitTreeOfStore(b testing.TB, s *memory.Storage, c plumbing.Hash) plumbing.Hash {
	cm, err := object.GetCommit(s, c)
	if err != nil {
		b.Fatal(err)
	}
	return cm.TreeHash
}

func verifyFetched(b *testing.B, s *memory.Storage, c, tree plumbing.Hash) {
	cm, err := object.GetCommit(s, c)
	if err != nil || cm.TreeHash != tree {
		b.Fatalf("fetched %s: %v", c, err)
	}
	if _, err := object.GetTree(s, tree); err != nil {
		b.Fatalf("tree %s: %v", tree, err)
	}
}

// BenchmarkWorkspaceInitialTransfer fetches the whole fixture over the
// production TLS endpoint into a fresh store (payload < 8 MiB).
func BenchmarkWorkspaceInitialTransfer(b *testing.B) {
	e := newBenchEnv(b)
	var payload int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := testkit.NewMemoryStore()
		if err := e.remote.Fetch(context.Background(), s, []plumbing.Hash{e.head}, nil); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		n := e.cnt.take()
		verifyFetched(b, s, e.head, e.tree)
		if n >= initialLimitBytes {
			b.Fatalf("initial payload %d >= %d", n, initialLimitBytes)
		}
		payload += n
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(payload)/float64(b.N), "payload-B/op")
}

// BenchmarkWorkspaceIncrementalTransfer changes one 4 KiB file, pushes it
// (a full guarded transaction: generation copy, closure validation, sync,
// publication, cleanup) and fetches it incrementally into an observer
// that holds the previous head; the combined payload stays < 256 KiB and
// < 10% of the initial transfer.
func BenchmarkWorkspaceIncrementalTransfer(b *testing.B) {
	e := newBenchEnv(b)
	observer := testkit.NewMemoryStore()
	if err := e.remote.Fetch(context.Background(), observer, []plumbing.Hash{e.head}, nil); err != nil {
		b.Fatal(err)
	}
	initial := e.cnt.take()
	var payload, copied, visited, files int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		name := fmt.Sprintf("f%04d.bin", (i*37)%benchFiles)
		e.files[name] = testkit.FileSpec{Mode: filemode.Regular, Content: benchBlob(i, i+1)}
		next := commit(b, e.local, e.files, fmt.Sprint("bench change ", i), e.head)
		tree := commitTreeOfStore(b, e.local, next)
		e.cnt.take()
		e.takeMetrics()
		b.StartTimer()
		if res := e.remote.Push(context.Background(), e.local, "refs/heads/main", e.head, next); !res.OK() {
			b.Fatalf("push: %s", pushString(res))
		}
		if err := e.remote.Fetch(context.Background(), observer, []plumbing.Hash{next}, []plumbing.Hash{e.head}); err != nil {
			b.Fatalf("fetch: %v", err)
		}
		b.StopTimer()
		n := e.cnt.take()
		mt := e.takeMetrics()
		verifyFetched(b, observer, next, tree)
		if n >= incrementLimit || n*10 >= initial {
			b.Fatalf("incremental payload %d (limit %d, initial %d)", n, incrementLimit, initial)
		}
		payload += n
		copied += mt["copied-bytes"]
		files += mt["copied-files"]
		visited += mt["visited-objects"]
		e.head = next
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(payload)/float64(b.N), "payload-B/op")
	b.ReportMetric(float64(initial), "initial-payload-B")
	b.ReportMetric(float64(copied)/float64(b.N), "copied-B/op")
	b.ReportMetric(float64(files)/float64(b.N), "copied-files/op")
	b.ReportMetric(float64(visited)/float64(b.N), "visited-objects/op")
}

// BenchmarkWorkspaceStatus pages through 150 refs, 100 per page, and
// asserts the exact sorted inventory. The timed work is read-only, so the
// 150 refs are set up (outside the timer) on a small repository.
func BenchmarkWorkspaceStatus(b *testing.B) {
	m := openAt(b, fastDeps(), tempRoot(b))
	v := mustCreate(b, m, "bench")
	s := testkit.NewMemoryStore()
	c0 := commit(b, s, seedFiles(), "c0")
	if r := receiveDirect(b, m, "bench", v.Instance, cmd("refs/heads/main", plumbing.ZeroHash, c0), packOf(b, s, objectsFor(b, s, c0)...)); !r.ok() {
		b.Fatal(r)
	}
	want := []string{"refs/heads/main"}
	for i := range 149 {
		name := fmt.Sprintf("branch-%03d", i)
		if _, err := setRef(m, "bench", v.Instance, name, contract.ExpectedAbsent, strp("main"), false); err != nil {
			b.Fatal(err)
		}
		want = append(want, "refs/heads/"+name)
	}
	sort.Strings(want)
	b.ResetTimer()
	var scanned int
	for i := 0; i < b.N; i++ {
		var got []string
		q := StatusQuery{Limit: 100}
		for {
			p, err := m.Status(context.Background(), "bench", q)
			if err != nil {
				b.Fatal(err)
			}
			for _, r := range p.Refs {
				got = append(got, r.Name)
			}
			if p.NextAfter == nil {
				break
			}
			q = StatusQuery{After: *p.NextAfter, Instance: p.Instance, Generation: p.Generation, Limit: 100}
		}
		b.StopTimer()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			b.Fatalf("status %d rows", len(got))
		}
		scanned += len(got)
		b.StartTimer()
	}
	b.ReportMetric(float64(scanned)/float64(b.N), "refs/op")
}

// BenchmarkWorkspaceList pages through 150 workspaces and asserts order.
func BenchmarkWorkspaceList(b *testing.B) {
	m := openAt(b, fastDeps(), tempRoot(b))
	var want []string
	for i := range 150 {
		n := fmt.Sprintf("ws-%03d", i)
		mustCreate(b, m, n)
		want = append(want, n)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var got []string
		after := ""
		for {
			p, err := m.List(after, 100)
			if err != nil {
				b.Fatal(err)
			}
			for _, s := range p.Workspaces {
				got = append(got, s.Name)
			}
			if p.NextAfter == nil {
				break
			}
			after = *p.NextAfter
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			b.Fatalf("list %d rows", len(got))
		}
	}
	b.ReportMetric(150, "rows/op")
}

// BenchmarkWorkspaceShow measures the disk accounting walk and asserts the
// size against an independent sum.
func BenchmarkWorkspaceShow(b *testing.B) {
	e := newBenchEnv(b)
	h, _ := e.m.lookup("bench")
	want, err := treeSize(h.dir)
	if err != nil {
		b.Fatal(err)
	}
	e.takeMetrics()
	b.ResetTimer()
	var scanned int64
	for i := 0; i < b.N; i++ {
		v, err := e.m.Show(context.Background(), "bench")
		if err != nil || v.SizeBytes != want {
			b.Fatalf("show %d want %d: %v", v.SizeBytes, want, err)
		}
		b.StopTimer()
		scanned += e.takeMetrics()["scanned-entries"]
		b.StartTimer()
	}
	b.ReportMetric(float64(scanned)/float64(b.N), "entries/op")
	b.ReportMetric(float64(want), "size-B")
}

// BenchmarkWorkspaceDiff pages the tree-metadata diff of 100 modified and
// 10 added files between two 1,024-file snapshots; rows are exact.
func BenchmarkWorkspaceDiff(b *testing.B) {
	e := newBenchEnv(b)
	for i := range 100 {
		e.files[fmt.Sprintf("f%04d.bin", i*10)] = testkit.FileSpec{Mode: filemode.Regular, Content: benchBlob(i, 7)}
	}
	for i := range 10 {
		e.files[fmt.Sprintf("new/n%02d.bin", i)] = testkit.FileSpec{Mode: filemode.Regular, Content: benchBlob(i, 9)}
	}
	base := e.head
	next := commit(b, e.local, e.files, "bench diff", base)
	push(b, e.remote, e.local, "refs/heads/main", base, next, setupWindow)
	if _, err := setRef(e.m, "bench", e.inst, "base", contract.ExpectedAbsent, strp(base.String()), false); err != nil {
		b.Fatal(err)
	}
	e.takeMetrics()
	b.ResetTimer()
	var entries int64
	for i := 0; i < b.N; i++ {
		var added, modified int
		q := DiffQuery{Base: sel(contract.SelectorKindHash, base.String()), Target: sel(contract.SelectorKindHash, next.String()), Limit: 100}
		for {
			p, err := e.m.Diff(context.Background(), "bench", q)
			if err != nil {
				b.Fatal(err)
			}
			for _, c := range p.Changes {
				switch c.Kind {
				case contract.ChangeAdded:
					added++
				case contract.ChangeModified:
					modified++
				}
			}
			if p.NextAfter == nil {
				break
			}
			after, _ := contract.ParsePathCursor(*p.NextAfter)
			q.After, q.Instance, q.Generation = after, p.Instance, p.Generation
		}
		b.StopTimer()
		if added != 10 || modified != 100 {
			b.Fatalf("diff added %d modified %d", added, modified)
		}
		entries += e.takeMetrics()["diff-entries"]
		b.StartTimer()
	}
	b.ReportMetric(float64(entries)/float64(b.N), "entries/op")
}

// BenchmarkWorkspacePrune collects an orphaned history (three commits
// pushed on a branch that is then deleted, reset outside the timer) while
// retaining main's full closure.
func BenchmarkWorkspacePrune(b *testing.B) {
	e := newBenchEnv(b)
	var reclaimed, objects int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		files := map[string]testkit.FileSpec{}
		for k, v := range e.files {
			files[k] = v
		}
		tip := e.head
		for j := range 3 {
			files[fmt.Sprintf("f%04d.bin", (i*3+j)%benchFiles)] = testkit.FileSpec{Mode: filemode.Regular, Content: benchBlob(i, 100+j)}
			tip = commit(b, e.local, files, fmt.Sprint("orphan ", i, j), tip)
		}
		push(b, e.remote, e.local, "refs/heads/orphan", plumbing.ZeroHash, tip)
		if _, err := setRef(e.m, "bench", e.inst, "orphan", tip.String(), nil, true); err != nil {
			b.Fatal(err)
		}
		e.takeMetrics()
		b.StartTimer()
		r, err := e.m.Prune(context.Background(), "bench", e.inst, time.Now())
		b.StopTimer()
		if err != nil || r.ReclaimedBytes <= 0 {
			b.Fatalf("prune %+v %v", r, err)
		}
		if !hasObject(b, e.m, "bench", e.head) || !hasObject(b, e.m, "bench", e.tree) || hasObject(b, e.m, "bench", tip) {
			b.Fatal("prune retained the orphan or dropped main's closure")
		}
		mt := e.takeMetrics()
		reclaimed += r.ReclaimedBytes
		objects += mt["pruned-objects"]
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(reclaimed)/float64(b.N), "reclaimed-B/op")
	b.ReportMetric(float64(objects)/float64(b.N), "retained-objects/op")
}
