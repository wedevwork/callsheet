package cli

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Design nonblocking-coordinator-waits, the CLI halves of UT-1 (FP-1:
// --until-done parsing, conflicts, IDs, help and the unchanged bounded
// mode), UT-3 (FP-3: every error row's exit code and single diagnostic,
// cancellation) and UT-4 (FP-4: silence, the exact text and JSON
// answers, escaping, a failed write and cancellation before the output
// commit), against the stub plane. The schedule matrix is the client's
// (TestWaitUntilDoneContract); here a retry costs one real backoff.

// sequence answers the n-th request (from 0) with answers[n], repeating
// the last one.
func (s *stubPlane) sequence(answers ...[3]string) {
	var n atomic.Int64
	s.mu.Lock()
	s.reqs = nil
	s.route = func(stubReq) (int, string, string) {
		i := min(int(n.Add(1))-1, len(answers)-1)
		a := answers[i]
		status := map[string]int{"200": 200, "404": 404, "409": 409, "500": 500, "501": 501, "503": 503}[a[0]]
		return status, a[1], a[2]
	}
	s.mu.Unlock()
}

func (s *stubPlane) bodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.reqs {
		out = append(out, r.body)
	}
	return out
}

func encodedWaitCLI(t *testing.T, r contract.WaitResponse) string {
	t.Helper()
	b, err := contract.EncodeWaitResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// countingCtx is a never-done context whose Err reports cancellation from
// its trip-th call on (0: never): it places a cancellation between two
// explicit checks deterministically.
type countingCtx struct {
	context.Context
	calls atomic.Int64
	trip  int64
}

func (c *countingCtx) Err() error {
	if n := c.calls.Add(1); c.trip > 0 && n >= c.trip {
		return context.Canceled
	}
	return nil
}

// TestUntilDoneCommand is UT-1/UT-3/UT-4 on the CLI. Do not rename or
// skip it.
func TestUntilDoneCommand(t *testing.T) {
	isolate(t)
	sp := startStubPlane(t)
	trust := []string{"--plane", sp.url, "--ca", sp.caFile}
	ids := []string{cliTask, cliTaskB}
	still := encodedWaitCLI(t, contract.WaitResponse{Version: 6, Status: contract.WaitStillRunning, EffectiveWaitMS: 30000,
		Tasks: []contract.WaitRow{{TaskID: cliTask, State: contract.TaskRunning, LastLogLine: "x"}, {TaskID: cliTaskB, State: contract.TaskPending}}})
	v := cliView(cliTaskB, contract.TaskSucceeded)
	termResp := contract.WaitResponse{Version: 6, Status: contract.WaitTerminal, EffectiveWaitMS: 120, Winner: cliTaskB, Task: &v}
	term := encodedWaitCLI(t, termResp)
	args := func(extra ...string) []string {
		return append(append(append([]string{"task", "wait"}, ids...), extra...), trust...)
	}
	t.Run("help", func(t *testing.T) {
		code, out, _ := exec(t, "linux", "task", "wait", "--help")
		if code != 0 || !strings.HasPrefix(out, "Usage: callsheet task wait ID [ID ...] [--wait DURATION | --until-done] --plane URL (--ca FILE | --ca-fingerprint SHA256) [--json]\n") ||
			!strings.Contains(out, "  --until-done       wait with no overall deadline") || !strings.Contains(out, "a plane gone for good is waited on\nuntil the command is stopped") ||
			!strings.Contains(out, "SIGINT or SIGTERM ends it with 130") || !strings.Contains(out, "default 5s; 0 answers at once") {
			t.Fatalf("help %d\n%s", code, out)
		}
	})
	t.Run("parsing", func(t *testing.T) {
		// Conflicts (an explicit --wait, zero included), a repeated or
		// valued flag and every invalid ID list exit 2 before any request.
		n := sp.count()
		many := make([]string, 17)
		for i := range many {
			many[i] = "t_" + strings.Repeat("0", 30) + string("0123456789abcdefg"[i]) + "1"
		}
		for _, a := range [][]string{args("--until-done", "--wait", "0"), args("--wait", "5s", "--until-done"), args("--until-done", "--until-done"),
			args("--until-done=false"), append([]string{"task", "wait", "--until-done"}, trust...),
			append([]string{"task", "wait", cliTask, cliTask, "--until-done"}, trust...),
			append(append([]string{"task", "wait", "--until-done"}, many...), trust...),
			append([]string{"task", "wait", "t_X", "--until-done"}, trust...),
			{"task", "wait", cliTask, "--until-done", "--plane", "http://x", "--ca", sp.caFile}} {
			if code, out, errOut := exec(t, "linux", a...); code != 2 || out != "" || !strings.HasPrefix(errOut, "callsheet: invalid_argument: ") {
				t.Fatalf("%v = %d %q %q", a, code, out, errOut)
			}
		}
		if code, _, errOut := exec(t, "linux", args("--wait", "1s", "--until-done")...); code != 2 || !strings.Contains(errOut, "give at most one of --wait and --until-done") {
			t.Fatalf("conflict %d %q", code, errOut)
		}
		if sp.count() != n {
			t.Fatal("an invalid until-done wait reached the plane")
		}
	})
	t.Run("text", func(t *testing.T) {
		// Renewed after a still_running slice: one winner, the exact
		// rendering (escaped), nothing on stderr, every request the same
		// 30 s slice in caller order.
		sp.sequence([3]string{"200", "6", still}, [3]string{"200", "6", term})
		code, out, errOut := exec(t, "linux", args("--until-done")...)
		if code != 0 || out != "winner: "+cliTaskB+"\n"+RenderTask(v) || errOut != "" || strings.Contains(out, "\x1b") ||
			!regexp.MustCompile(`^winner: t_[0-9a-f]{32}\n`).MatchString(out) {
			t.Fatalf("text %d %q %q", code, out, errOut)
		}
		if b := sp.bodies(); len(b) != 2 || b[0] != `{"task_ids":["`+cliTask+`","`+cliTaskB+`"],"wait":"30s"}` || b[1] != b[0] {
			t.Fatalf("requests %v", b)
		}
	})
	t.Run("json", func(t *testing.T) {
		// Exactly one compact terminal envelope and LF; effective_wait_ms is
		// the final slice's; no still_running envelope precedes it.
		sp.sequence([3]string{"200", "6", still}, [3]string{"200", "6", term})
		code, out, errOut := exec(t, "linux", args("--until-done", "--json")...)
		if code != 0 || out != term+"\n" || errOut != "" || strings.Count(out, "\n") != 1 || !strings.Contains(out, `"effective_wait_ms":120`) {
			t.Fatalf("json %d %q %q", code, out, errOut)
		}
	})
	t.Run("silent-retry", func(t *testing.T) {
		// A typed capacity 503 and an unstructured 503 are retried with no
		// output at all; the pin trust path works the same.
		sp.sequence([3]string{"503", "6", `{"error":{"code":"unavailable","message":"full","details":{"reason":"wait_capacity"}}}`},
			[3]string{"503", "6", "busy"}, [3]string{"200", "6", term})
		code, out, errOut := exec(t, "linux", "task", "wait", cliTaskB, "--until-done", "--json", "--plane", sp.url, "--ca-fingerprint", sp.pin)
		if code != 0 || errOut != "" || len(sp.bodies()) != 3 {
			t.Fatalf("retry %d %q %q after %d requests", code, out, errOut, len(sp.bodies()))
		}
	})
	t.Run("errors", func(t *testing.T) {
		// Every permanent row: its exit code, one diagnostic, no stdout,
		// one request.
		for _, c := range []struct {
			name           string
			status, header string
			body           string
			code           int
		}{
			{"malformed", "200", "6", `{"version":6}`, 2},
			{"unknown", "404", "6", `{"error":{"code":"not_found","message":"task t_x does not exist"}}`, 3},
			{"conflict", "409", "6", `{"error":{"code":"conflict","message":"no"}}`, 4},
			{"mismatch", "200", "5", term, 7},
			{"not-implemented", "501", "6", `{"error":{"code":"not_implemented","message":"no"}}`, 8},
			{"internal", "500", "6", "oops", 1},
		} {
			sp.sequence([3]string{c.status, c.header, c.body})
			code, out, errOut := exec(t, "linux", args("--until-done")...)
			if code != c.code || out != "" || strings.Count(errOut, "\n") != 1 || !strings.HasPrefix(errOut, "callsheet: ") || len(sp.bodies()) != 1 {
				t.Fatalf("%s: %d %q %q", c.name, code, out, errOut)
			}
		}
		// Trust: a missing trust input and a foreign CA exit 6 without a
		// wait request.
		n := sp.count()
		if code, out, errOut := exec(t, "linux", "task", "wait", cliTask, "--until-done", "--plane", sp.url); code != 6 || out != "" || !strings.Contains(errOut, "connection not trusted") {
			t.Fatalf("no trust %d %q", code, errOut)
		}
		foreign := strings.Repeat("a", 64)
		if code, _, errOut := exec(t, "linux", "task", "wait", cliTask, "--until-done", "--plane", sp.url, "--ca-fingerprint", "sha256:"+foreign); code != 6 || !strings.Contains(errOut, "connection not trusted") {
			t.Fatalf("foreign pin %d %q", code, errOut)
		}
		if sp.count() != n {
			t.Fatal("an untrusted wait sent a request")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		// A cancelled caller exits 130 with the one interrupted line and no
		// winner: before the call, and between the winner's arrival and
		// the output commit (the last check; the count is learned from a
		// complete run).
		sp.sequence([3]string{"200", "6", term})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out, errOut bytes.Buffer
		if code := run(ctx, NewTree("linux"), "linux", args("--until-done"), &out, &errOut); code != 130 || out.Len() != 0 || errOut.String() != "callsheet: interrupted\n" {
			t.Fatalf("cancelled %d %q %q", code, out.String(), errOut.String())
		}
		learn := &countingCtx{Context: context.Background()}
		out.Reset()
		if code := run(learn, NewTree("linux"), "linux", args("--until-done"), &out, &errOut); code != 0 || out.Len() == 0 {
			t.Fatalf("complete run %d", code)
		}
		late := &countingCtx{Context: context.Background(), trip: learn.calls.Load()}
		out.Reset()
		errOut.Reset()
		if code := run(late, NewTree("linux"), "linux", args("--until-done"), &out, &errOut); code != 130 || out.Len() != 0 || errOut.String() != "callsheet: interrupted\n" {
			t.Fatalf("cancelled before the output commit: %d %q %q", code, out.String(), errOut.String())
		}
	})
	t.Run("write-failure", func(t *testing.T) {
		// A failed output write is internal (exit 1), never a usable result.
		sp.sequence([3]string{"200", "6", term})
		var errOut bytes.Buffer
		if code := run(context.Background(), NewTree("linux"), "linux", args("--until-done"), failWriter{}, &errOut); code != 1 || !strings.Contains(errOut.String(), "cannot write to stdout") {
			t.Fatalf("write %d %q", code, errOut.String())
		}
	})
	t.Run("sigpipe", func(t *testing.T) {
		// The output is written with SIGPIPE registered (a closed stdout
		// pipe is then an error, exit 1, not death by signal), and only for
		// the write: registered after the winner arrived, before the
		// output, and stopped (review C3).
		rec := &signalRecorder{}
		rec.install(t)
		sp.sequence([3]string{"200", "6", term})
		out := &orderWriter{rec: rec}
		var errOut bytes.Buffer
		code := run(context.Background(), NewTree("linux"), "linux", args("--until-done"), out, &errOut)
		if code != 0 || rec.log() != "notify "+syscall.SIGPIPE.String()+",output,stop" {
			t.Fatalf("%d %q %q", code, rec.log(), errOut.String())
		}
		rec.events = nil
		sp.sequence([3]string{"404", "6", `{"error":{"code":"not_found","message":"no"}}`})
		if code := run(context.Background(), NewTree("linux"), "linux", args("--until-done"), out, &errOut); code != 3 || rec.log() != "" {
			t.Fatalf("an error registered SIGPIPE: %d %q", code, rec.log())
		}
	})
	t.Run("bounded-unchanged", func(t *testing.T) {
		// Without --until-done: the default 5 s request and the still_running
		// table, exit 0, as before.
		sp.sequence([3]string{"200", "6", encodedWaitCLI(t, contract.WaitResponse{Version: 6, Status: contract.WaitStillRunning, EffectiveWaitMS: 5000,
			Tasks: []contract.WaitRow{{TaskID: cliTask, State: contract.TaskRunning}, {TaskID: cliTaskB, State: contract.TaskPending}}})})
		code, out, _ := exec(t, "linux", args()...)
		if code != 0 || !strings.HasPrefix(out, "TASK_ID\tSTATE\t") || sp.bodies()[0] != `{"task_ids":["`+cliTask+`","`+cliTaskB+`"],"wait":"5s"}` {
			t.Fatalf("bounded %d %q", code, out)
		}
	})
}
