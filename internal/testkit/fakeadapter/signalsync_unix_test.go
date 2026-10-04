//go:build linux || darwin

package fakeadapter

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTermExitReturnsWhileSignalSyncBlocks pins that the cooperative TERM
// exit does not wait on a flush of the signal file. A slow darwin
// F_FULLFSYNC cannot be reproduced on a fast local disk, so the guard is
// structural: appendSignal, and every package function it reaches, must not
// call a sync. The behavioural half requires exit-mode Run to return 0 within
// 200ms of SIGTERM, with the signal line written.
func TestTermExitReturnsWhileSignalSyncBlocks(t *testing.T) {
	funcs := parsePackageFuncs(t)
	// The detector must see the sync publishReady keeps, or a clean
	// appendSignal result proves nothing.
	if calls := syncCalls(funcs, "publishReady"); len(calls) == 0 {
		t.Fatal("sync detector found no sync in publishReady; the guard is blind")
	}
	if calls := syncCalls(funcs, "appendSignal"); len(calls) != 0 {
		t.Fatalf("appendSignal reaches a sync on the TERM-exit path: %s", strings.Join(calls, "; "))
	}

	dir := t.TempDir()
	sigs := newFakeSignals()
	var stderr bytes.Buffer
	type completion struct {
		code int
		at   time.Time
	}
	done := make(chan completion, 1)
	go func() {
		code := Run(context.Background(), Env{
			Args:    []string{"--duration=1h", "--signal-file=s/sig.jsonl"},
			Dir:     dir,
			Signals: sigs,
			PID:     91,
			Stdout:  &bytes.Buffer{},
			Stderr:  &stderr,
		})
		done <- completion{code, time.Now()}
	}()

	c := <-sigs.registered
	// The bound starts before TERM is sent and Run's completion is stamped
	// as it returns, so neither descheduling the sender nor a late receive
	// here can stretch it.
	deadline := time.Now().Add(200 * time.Millisecond)
	c <- syscall.SIGTERM
	var r completion
	select {
	case r = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("exit-mode Run did not return within 20s of SIGTERM")
	}
	if r.at.After(deadline) {
		t.Fatalf("exit-mode Run returned %v after the 200ms SIGTERM deadline", r.at.Sub(deadline))
	}
	if r.code != 0 {
		t.Fatalf("exit-mode TERM: code = %d, want 0 (stderr %q)", r.code, stderr.String())
	}
	recs := readSignals(t, filepath.Join(dir, "s", "sig.jsonl"))
	if len(recs) != 1 || recs[0].PID != 91 || recs[0].Signal != "SIGTERM" {
		t.Fatalf("signal records = %+v, want one SIGTERM from 91", recs)
	}
}

// parsePackageFuncs parses this package's non-test sources (every GOOS
// variant) and indexes their function declarations by name.
func parsePackageFuncs(t *testing.T) map[string][]*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string][]*ast.FuncDecl{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
				funcs[fd.Name.Name] = append(funcs[fd.Name.Name], fd)
			}
		}
	}
	if len(funcs["appendSignal"]) == 0 || len(funcs["publishReady"]) == 0 {
		t.Fatal("appendSignal or publishReady not found in package sources")
	}
	return funcs
}

// syncCalls reports every sync reachable from the named function through
// direct calls to package functions: a call whose name contains "sync" in
// any case (f.Sync, unix.Fsync, syscall.Fdatasync, ...) or a use of
// F_FULLFSYNC.
func syncCalls(funcs map[string][]*ast.FuncDecl, start string) []string {
	var found []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, fd := range funcs[name] {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					var callee string
					switch fn := n.Fun.(type) {
					case *ast.Ident:
						callee = fn.Name
						if _, local := funcs[callee]; local {
							walk(callee)
						}
					case *ast.SelectorExpr:
						callee = fn.Sel.Name
					}
					if strings.Contains(strings.ToLower(callee), "sync") {
						found = append(found, name+" calls "+callee)
					}
				case *ast.Ident:
					if strings.Contains(n.Name, "FULLFSYNC") {
						found = append(found, name+" uses "+n.Name)
					}
				}
				return true
			})
		}
	}
	walk(start)
	sort.Strings(found)
	return found
}
