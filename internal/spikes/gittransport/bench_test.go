package gittransport

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/testkit"
)

const (
	benchFiles         = 1024
	benchBlobSize      = 4 << 10
	initialLimitBytes  = 8 << 20
	incrementLimitByte = 256 << 10
)

// benchBlob returns deterministic pseudorandom bytes for file i, version v.
func benchBlob(i, v int) []byte {
	r := rand.New(rand.NewPCG(uint64(i), uint64(v)+0x5eed))
	b := make([]byte, benchBlobSize)
	for j := 0; j < len(b); j += 8 {
		binary.LittleEndian.PutUint64(b[j:], r.Uint64())
	}
	return b
}

type benchFixture struct {
	h    *smartHandler
	hk   *testkit.Harness
	url  string
	head plumbing.Hash
	tree plumbing.Hash
}

func newBenchFixture(b *testing.B) *benchFixture {
	b.Helper()
	bare := filepath.Join(b.TempDir(), "bench.git")
	repo, err := git.PlainInitWithOptions(bare, &git.PlainInitOptions{Bare: true, InitOptions: git.InitOptions{DefaultBranch: plumbing.Main}})
	if err != nil {
		b.Fatal(err)
	}
	files := make(map[string]fileSpec, benchFiles)
	for i := range benchFiles {
		files[fmt.Sprintf("f%04d.bin", i)] = fileSpec{filemode.Regular, benchBlob(i, 0)}
	}
	tree, err := writeTree(repo.Storer, files)
	if err != nil {
		b.Fatal(err)
	}
	head, err := writeCommit(repo.Storer, tree, nil, "bench baseline", fixedWhen)
	if err != nil {
		b.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.Main, head)); err != nil {
		b.Fatal(err)
	}
	hnd, err := NewSmartHandler(bare)
	if err != nil {
		b.Fatal(err)
	}
	f := &benchFixture{h: hnd.(*smartHandler), head: head, tree: tree}
	f.hk = testkit.NewHarness(b, testkit.WithGitHandler(f.h))
	f.url = f.hk.URL + RepoURLPath
	logEnvironment(b)
	return f
}

func logEnvironment(b *testing.B) {
	lib := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/go-git/go-git/v5" {
				lib = d.Version
			}
		}
	}
	if lib == "unknown" {
		// Test binaries omit dependency build info; use the go.mod pin.
		if root, err := testkit.RepoRoot(); err == nil {
			if mod, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
				for _, line := range strings.Split(string(mod), "\n") {
					if f := strings.Fields(line); len(f) == 2 && f[0] == "github.com/go-git/go-git/v5" {
						lib = f[1] + " (go.mod)"
					}
				}
			}
		}
	}
	b.Logf("go=%s go-git=%s os=%s arch=%s cpus=%d", runtime.Version(), lib, runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
}

func payload(xs []exchange) int64 {
	var n int64
	for _, x := range xs {
		n += x.RequestBytes + x.ResponseBytes
	}
	return n
}

func countObjects(b *testing.B, s storer.EncodedObjectStorer) int {
	iter, err := s.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		b.Fatal(err)
	}
	n := 0
	iter.ForEach(func(plumbing.EncodedObject) error { n++; return nil })
	return n
}

// cloneBare clones main into a fresh in-memory store and verifies the
// commit/tree; it returns the repository and the transfer payload.
func (f *benchFixture) cloneBare(b *testing.B, worktree bool) (*git.Repository, int64) {
	mark := len(f.h.transcript())
	var repo *git.Repository
	var err error
	if worktree {
		repo, err = git.CloneContext(context.Background(), memory.NewStorage(), memfs.New(), &git.CloneOptions{URL: f.url, CABundle: f.hk.CAPEM})
	} else {
		repo, err = git.CloneContext(context.Background(), memory.NewStorage(), nil, &git.CloneOptions{URL: f.url, CABundle: f.hk.CAPEM})
	}
	if err != nil {
		b.Fatalf("clone: %v", err)
	}
	return repo, payload(f.h.transcript()[mark:])
}

func verifyTip(b *testing.B, repo *git.Repository, ref plumbing.ReferenceName, head, tree plumbing.Hash) {
	r, err := repo.Reference(ref, true)
	if err != nil || r.Hash() != head {
		b.Fatalf("%s = %v %v, want %s", ref, r, err, head)
	}
	c, err := repo.CommitObject(head)
	if err != nil || c.TreeHash != tree {
		b.Fatalf("tree mismatch for %s: %v", head, err)
	}
}

func BenchmarkGitInitialTransfer(b *testing.B) {
	f := newBenchFixture(b)
	var total int64
	var objects int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		repo, bytes := f.cloneBare(b, false)
		b.StopTimer()
		verifyTip(b, repo, plumbing.Main, f.head, f.tree)
		if bytes >= initialLimitBytes {
			b.Fatalf("initial transfer payload %d bytes >= %d", bytes, initialLimitBytes)
		}
		total += bytes
		objects = countObjects(b, repo.Storer)
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(total)/float64(b.N), "payload-B/op")
	b.ReportMetric(float64(objects), "objects/op")
}

func BenchmarkGitIncrementalTransfer(b *testing.B) {
	f := newBenchFixture(b)
	_, initial := f.cloneBare(b, false)
	var total int64
	var objects int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		worker, _ := f.cloneBare(b, true)
		observer, _ := f.cloneBare(b, false)
		wt, err := worker.Worktree()
		if err != nil {
			b.Fatal(err)
		}
		name := fmt.Sprintf("f%04d.bin", (i*37)%benchFiles)
		fh, err := wt.Filesystem.Create(name)
		if err != nil {
			b.Fatal(err)
		}
		fh.Write(benchBlob(i, i+1))
		fh.Close()
		if _, err := wt.Add(name); err != nil {
			b.Fatal(err)
		}
		s := sig(fixedWhen)
		next, err := wt.Commit(fmt.Sprintf("bench change %d", i), &git.CommitOptions{Author: &s, Committer: &s})
		if err != nil {
			b.Fatal(err)
		}
		ref := fmt.Sprintf("refs/callsheet/tasks/bench-%d", i)
		mark := len(f.h.transcript())
		b.StartTimer()

		if err := worker.Push(&git.PushOptions{RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:" + ref)}, CABundle: f.hk.CAPEM}); err != nil {
			b.Fatalf("push: %v", err)
		}
		if err := observer.Fetch(&git.FetchOptions{RefSpecs: []config.RefSpec{config.RefSpec("+" + ref + ":refs/remotes/origin/bench")}, CABundle: f.hk.CAPEM}); err != nil {
			b.Fatalf("fetch: %v", err)
		}

		b.StopTimer()
		bytes := payload(f.h.transcript()[mark:])
		wc, _ := worker.CommitObject(next)
		verifyTip(b, observer, "refs/remotes/origin/bench", next, wc.TreeHash)
		if bytes >= incrementLimitByte || bytes*10 >= initial {
			b.Fatalf("incremental payload %d bytes (limit %d, initial %d)", bytes, incrementLimitByte, initial)
		}
		total += bytes
		objects = f.h.lastPromoted()
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(total)/float64(b.N), "payload-B/op")
	b.ReportMetric(float64(initial), "initial-payload-B")
	b.ReportMetric(float64(objects), "objects/op")
}
