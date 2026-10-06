package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/mcp"
)

// FP-1/FP-7: mcp help is ordinary help (no connection) and states the
// shipping 10s budget as a short poll (ShortPollNotice).
func TestMCPHelp(t *testing.T) {
	for _, args := range [][]string{{"mcp", "--help"}, {"help", "mcp"}, {"mcp", "--plane", "https://x", "-h"}} {
		code, out, errOut := exec(t, "linux", args...)
		if code != 0 || errOut != "" || !strings.HasPrefix(out, "Usage: callsheet mcp "+mcpUsage+"\n") || !strings.Contains(out, mcp.ShortPollNotice) ||
			!strings.Contains(out, "1s to 5m (default\n                     10s)") || !strings.Contains(out, "Status: implemented.") {
			t.Fatalf("%v = %d %q %q", args, code, out, errOut)
		}
	}
}

// FP-2/FP-7: startup validates the URL, the exclusive trust flags and the
// budget without contacting the plane, with the existing exit mappings.
func TestMCPStartupFlags(t *testing.T) {
	pin := "sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"mcp", "--ca", "x"}, 2, "--plane is required"},
		{[]string{"mcp", "--plane", "http://x", "--ca", "x"}, 2, "scheme must be https"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "--ca-fingerprint", pin}, 2, "only one of --ca and --ca-fingerprint"},
		{[]string{"mcp", "--plane", "https://x", "--ca-fingerprint", "sha256:zz"}, 2, "invalid --ca-fingerprint"},
		{[]string{"mcp", "--plane", "https://x"}, 6, "connection not trusted: supply --ca or --ca-fingerprint"},
		{[]string{"mcp", "--plane", "https://x", "--ca", ""}, 2, "--ca must not be empty"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "operand"}, 2, "unexpected argument"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "--json"}, 2, "flag provided but not defined: -json"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "--wait-call-budget", "999ms"}, 2, "--wait-call-budget must be a Go duration from 1s to 5m"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "--wait-call-budget", "5m1s"}, 2, "from 1s to 5m"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "--wait-call-budget", "soon"}, 2, "from 1s to 5m"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "--wait-call-budget", " 1s"}, 2, "from 1s to 5m"},
		{[]string{"mcp", "--plane", "https://x", "--ca", "x", "--wait-call-budget", "2s", "--wait-call-budget", "3s"}, 2, "may be given only once"},
	} {
		code, out, errOut := exec(t, "linux", c.args...)
		if code != c.code || out != "" || !strings.Contains(errOut, c.msg) {
			t.Fatalf("%v = %d %q %q", c.args, code, out, errOut)
		}
	}
	for _, b := range []string{"1s", "10s", "5m", "90s"} {
		if code, out, errOut := exec(t, "linux", "mcp", "--plane", "https://x", "--ca-fingerprint", pin, "--wait-call-budget", b); code != 0 || out != "" || errOut != "" {
			t.Fatalf("budget %s = %d %q %q", b, code, out, errOut)
		}
	}
}

// signalRecorder replaces the SIGPIPE registration seam.
type signalRecorder struct {
	mu      sync.Mutex
	events  []string
	channel chan<- os.Signal
}

func (r *signalRecorder) install(t *testing.T) {
	oldNotify, oldStop := signalNotify, signalStop
	t.Cleanup(func() { signalNotify, signalStop = oldNotify, oldStop })
	signalNotify = func(c chan<- os.Signal, sigs ...os.Signal) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.channel = c
		for _, s := range sigs {
			r.events = append(r.events, "notify "+s.String())
		}
		if cap(c) != 1 {
			r.events = append(r.events, "bad capacity")
		}
	}
	signalStop = func(c chan<- os.Signal) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if c != r.channel {
			r.events = append(r.events, "stop of another channel")
		}
		r.events = append(r.events, "stop")
	}
}

func (r *signalRecorder) log() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.events, ",")
}

// orderWriter records whether the SIGPIPE registration preceded output.
type orderWriter struct {
	mu  sync.Mutex
	rec *signalRecorder
	buf bytes.Buffer
}

func (w *orderWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() == 0 {
		w.rec.mu.Lock()
		w.rec.events = append(w.rec.events, "output")
		w.rec.mu.Unlock()
	}
	return w.buf.Write(p)
}

// FP-8: only the mcp leaf registers SIGPIPE, on linux and darwin, with a
// one-slot channel before any protocol output, and stops it on exit; an
// unsupported goos is rejected before any registration.
func TestMCPSignalSeam(t *testing.T) {
	rec := &signalRecorder{}
	rec.install(t)
	root := NewTree("linux")
	for _, goos := range []string{"linux", "darwin"} {
		rec.events = nil
		out := &orderWriter{rec: rec}
		in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
		// The reader ends right after the ping: the reply may or may not
		// be written before EOF discards it, but never before registration.
		code := mcpServe(context.Background(), goos, root.Child("mcp"), []string{"--plane", "https://x", "--ca", "c"}, in, out, io.Discard)
		if code != 0 || !strings.HasPrefix(rec.log(), "notify "+syscall.SIGPIPE.String()) || !strings.HasSuffix(rec.log(), "stop") ||
			strings.Contains(rec.log(), "bad capacity") || strings.Contains(rec.log(), "another channel") {
			t.Fatalf("%s: %d %q", goos, code, rec.log())
		}
	}
	for _, goos := range []string{"windows", "freebsd", ""} {
		rec.events = nil
		var errOut bytes.Buffer
		code := mcpServe(context.Background(), goos, root.Child("mcp"), []string{"--plane", "https://x", "--ca", "c"}, strings.NewReader(""), io.Discard, &errOut)
		if code != 2 || rec.log() != "" || !strings.Contains(errOut.String(), "unsupported operating system") {
			t.Fatalf("%q: %d %q %q", goos, code, rec.log(), errOut.String())
		}
	}
	// No other command registers SIGPIPE.
	rec.events = nil
	for _, args := range [][]string{{"version"}, {"node", "ls", "--plane", "https://127.0.0.1:1", "--ca", "/nonexistent"}, {"task", "prune"}} {
		exec(t, "linux", args...)
	}
	if rec.log() != "" {
		t.Fatalf("other commands registered %q", rec.log())
	}
}

// FP-1/FP-8: the leaf serves the protocol over the threaded input without
// contacting the plane; EOF exits 0 and a signal 130.
func TestMCPLeafSession(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var errOut bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- runFor(ctx, "linux", []string{"mcp", "--plane", "https://127.0.0.1:1", "--ca", "/nonexistent", "--wait-call-budget", "2s"}, inR, outW, &errOut)
	}()
	br := bufio.NewReader(outR)
	send := func(s string) string {
		t.Helper()
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	if r := send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`); !strings.Contains(r, `"serverInfo":{"name":"callsheet","version":"dev"}`) {
		t.Fatalf("initialize %s", r)
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	if r := send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); !strings.Contains(r, "B=2s") || strings.Count(r, `"inputSchema":`) != 23 {
		t.Fatalf("tools/list %.200s", r)
	}
	// The per-call trust resolution fails (the CA file is missing): a tool
	// error, and the session stays usable.
	if r := send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"node_ls"}}`); !strings.Contains(r, `trust_failed`) || !strings.Contains(r, `"isError":true`) {
		t.Fatalf("node_ls %s", r)
	}
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("exit %d %q", code, errOut.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the leaf did not end")
	}
}

// keepFiles keeps test files whose descriptor a pollable twin now owns
// from ever being finalized (their finalizer would close a reused number).
var keepFiles []*os.File

// FP-8: stream ownership: pipes become pollable handles whose Close
// interrupts a pending Read or Write; regular files, a stdout sharing
// stderr's pipe and non-closable readers are never waited for.
func TestMCPStreams(t *testing.T) {
	t.Run("blocking-pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		fd, err := unix.Dup(int(r.Fd())) // Fd makes the descriptor blocking, as os.Stdin's is
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		blocking := os.NewFile(uintptr(fd), "blocking")
		keepFiles = append(keepFiles, blocking)
		// The blocking file cannot be interrupted: it has no poller.
		if err := blocking.SetReadDeadline(time.Now()); !errors.Is(err, os.ErrNoDeadline) {
			t.Fatalf("the fixture is not a blocking file: %v", err)
		}
		st := mcpStreams(blocking, io.Discard, io.Discard)
		f, ok := st.in.(*os.File)
		if st.closeIn == nil || !ok || f == blocking {
			t.Fatal("a blocking pipe was not made pollable")
		}
		// The owned handle is polled: a pending Read observes a deadline
		// and, the same way, the Close that ends a session.
		if err := f.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			t.Fatalf("the owned stdin is not pollable: %v", err)
		}
		if _, err := f.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("pending read: %v", err)
		}
		f.SetReadDeadline(time.Time{})
		got := make(chan error, 1)
		go func() {
			_, err := f.Read(make([]byte, 1))
			got <- err
		}()
		st.closeIn()
		select {
		case err := <-got:
			if err == nil {
				t.Fatal("the read returned data")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("closing did not interrupt the pending read")
		}
	})
	t.Run("pollable-pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		st := mcpStreams(r, w, io.Discard)
		if st.in != io.Reader(r) || st.out != io.Writer(w) || st.closeIn == nil || st.closeOut == nil {
			t.Fatal("an already pollable pipe was not used as is")
		}
		st.closeIn()
		st.closeOut()
	})
	t.Run("shared-stderr", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if st := mcpStreams(strings.NewReader(""), w, w); st.closeOut != nil {
			t.Fatal("a stdout sharing stderr's pipe was taken over")
		}
	})
	t.Run("regular-file", func(t *testing.T) {
		f, err := os.Create(filepath.Join(t.TempDir(), "in"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if st := mcpStreams(f, f, io.Discard); st.closeIn != nil || st.closeOut != nil || st.in != io.Reader(f) {
			t.Fatal("a regular file was taken over")
		}
	})
	t.Run("generic", func(t *testing.T) {
		pr, pw := io.Pipe()
		if st := mcpStreams(pr, pw, io.Discard); st.closeIn == nil || st.closeOut == nil {
			t.Fatal("closable test streams lack closers")
		}
		st := mcpStreams(nil, io.Discard, io.Discard)
		if n, err := st.in.Read(make([]byte, 1)); n != 0 || err != io.EOF || st.closeIn != nil || st.closeOut != nil {
			t.Fatal("nil input is not an empty stream")
		}
	})
	// The default seams are the process's signal functions.
	if signalNotify == nil || signalStop == nil {
		t.Fatal("seams unset")
	}
	_ = signal.Notify
}
