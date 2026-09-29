package mcpqual

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// FP-16 unit tests: the explicit invocation gate (flag absent or present,
// a model-free driver, the CI prohibition with exit 2), fake-only
// launches, and the thin main's exit mapping (0, 2, 4, 5, 130).

type cliRun struct {
	code           int
	stdout, stderr string
}

func testEnv(t *testing.T, w *fakeLauncher, args ...string) (Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return Env{GOOS: "linux", GOARCH: "amd64", Args: args, Getenv: func(string) string { return "" }, Environ: []string{"PATH=/usr/bin", "CI="},
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Signaler: worldSignaler{w}, Launcher: w, Clock: w.clock, Registry: DefaultRegistry(),
		Hostname: "host-1", Executable: "/opt/mcpqual", Home: "/home/owner", User: "owner", Now: func() time.Time { return epoch },
		Rand: bytes.NewReader(bytes.Repeat([]byte{7}, 64)), Notify: func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }}, &out, &errOut
}

func run(env Env) cliRun {
	code := Main(context.Background(), env)
	return cliRun{code, env.Stdout.(*bytes.Buffer).String(), env.Stderr.(*bytes.Buffer).String()}
}

// writePlan writes p with its executable replaced by a real file (Main
// hashes the executable; the fake launcher never runs it).
func writePlan(t *testing.T, p *Plan) string {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	os.WriteFile(exe, []byte("#!/bin/false\n"), 0o700)
	p.Clients[0].Executable = exe
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, planJSON(t, p), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func modelPlan() *Plan {
	p := planWith(Phases{})
	p.Clients[0].Driver, p.Clients[0].Model = DriverModel, "model-x"
	p.Clients[0].Session.Argv = append(p.Clients[0].Session.Argv, "--model", "model-x")
	return p
}

func TestMainUsage(t *testing.T) {
	w := newWorld(t, fullModel())
	for _, tc := range []struct {
		args []string
		code int
		out  string
	}{
		{nil, 2, "usage:"}, {[]string{"bogus"}, 2, "unknown command"}, {[]string{"version"}, 0, "mcpqual " + HarnessVersion}, {[]string{"help"}, 0, "usage:"},
		{[]string{"qualify", "--nope"}, 2, "flag provided but not defined"}, {[]string{"qualify", "--plan", "/p", "--out", "/o", "extra"}, 2, "unexpected arguments"},
		{[]string{"qualify", "--plan", "rel.json", "--out", "/o"}, 2, "absolute clean path"}, {[]string{"qualify", "--plan", "/p.json"}, 2, "--out is required"},
		{[]string{"qualify", "--plan", "/nonexistent/plan.json", "--out", "/o"}, 2, "no such file"},
		{[]string{"serve"}, 2, "needs --case-file and --events"}, {[]string{"serve", "--case-file", "/nonexistent", "--events", "/tmp/x"}, 2, "no such file"},
		{[]string{"publish", "--out", "rel", "--repo", "/r"}, 2, "must be absolute"}, {[]string{"publish", "--bad"}, 2, "not defined"},
	} {
		env, _, _ := testEnv(t, w, tc.args...)
		r := run(env)
		if r.code != tc.code || !strings.Contains(r.stdout+r.stderr, tc.out) {
			t.Errorf("%v = %d %q %q", tc.args, r.code, r.stdout, r.stderr)
		}
	}
	env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, fullPlan()), "--out", t.TempDir())
	env.GOOS = "windows"
	if r := run(env); r.code != 2 || !strings.Contains(r.stderr, "linux or darwin only") {
		t.Fatalf("windows = %+v", r)
	}
	if len(w.launches) != 0 {
		t.Fatal("a usage error launched a process")
	}
}

func TestInvocationGate(t *testing.T) {
	t.Run("ci-denied", func(t *testing.T) {
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", "/does/not/exist.json", "--out", filepath.Join(t.TempDir(), "o"), "--allow-model-calls")
		env.Getenv = func(k string) string { return map[string]string{"CI": "true"}[k] }
		r := run(env)
		if r.code != 2 || !strings.Contains(r.stderr, "CI is set") || len(w.launches) != 0 {
			t.Fatalf("CI run = %+v, %d launches", r, len(w.launches))
		}
	})
	t.Run("denied", func(t *testing.T) {
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, modelPlan()), "--out", filepath.Join(t.TempDir(), "o"))
		r := run(env)
		if r.code != 5 || len(w.sessions()) != 0 || len(w.launches) != 1 || !strings.Contains(r.stdout, "vendor behavior not measured") {
			t.Fatalf("denied = %+v, %d sessions", r, len(w.sessions()))
		}
	})
	t.Run("allowed", func(t *testing.T) {
		w := newWorld(t, fullModel())
		out := filepath.Join(t.TempDir(), "o")
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, modelPlan()), "--out", out, "--allow-model-calls")
		r := run(env)
		if r.code != 0 || len(w.sessions()) != 1 || !strings.Contains(r.stdout, "conclusive") || !strings.Contains(r.stderr, "planned upper bound 1 sessions") {
			t.Fatalf("allowed = %+v, %d sessions", r, len(w.sessions()))
		}
		for _, s := range w.launches {
			if !strings.HasSuffix(s.Path, "/claude") || strings.Contains(strings.Join(s.Env, " "), "CI=") {
				t.Fatalf("launch %+v", s)
			}
		}
		if _, err := os.Stat(filepath.Join(out, "report.json")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("model-free", func(t *testing.T) {
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", filepath.Join(t.TempDir(), "o"))
		if r := run(env); r.code != 0 || len(w.sessions()) != 1 {
			t.Fatalf("model-free = %+v", r)
		}
	})
	t.Run("template-plan", func(t *testing.T) {
		b, _ := os.ReadFile(filepath.Join("testdata", "plans", "claude.json"))
		path := filepath.Join(t.TempDir(), "p.json")
		os.WriteFile(path, b, 0o600)
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", path, "--out", filepath.Join(t.TempDir(), "o"), "--allow-model-calls")
		if r := run(env); r.code != 2 || !strings.Contains(r.stderr, "unfilled owner placeholder") || len(w.launches) != 0 {
			t.Fatalf("template = %+v", r)
		}
	})
}

func TestMainExitMapping(t *testing.T) {
	t.Run("interrupt", func(t *testing.T) {
		w := newWorld(t, fullModel())
		w.hang = func(s ProcSpec) bool { return strings.Contains(strings.Join(s.Args, " "), "claude-setup") }
		out := filepath.Join(t.TempDir(), "o")
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", out)
		var cancel context.CancelFunc
		env.Notify = func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel = context.WithCancel(ctx)
			return ctx, cancel
		}
		go func() {
			for s := range w.started {
				if strings.Contains(strings.Join(s.Args, " "), "claude-setup") {
					if err := w.clock.AwaitWaiter(testWait, func(ws []testkit.Waiter) bool { return len(ws) > 0 }); err == nil {
						cancel()
					}
					return
				}
			}
		}()
		r := run(env)
		if r.code != 130 || !strings.Contains(r.stdout, "partial") {
			t.Fatalf("interrupt = %+v", r)
		}
		b, _ := os.ReadFile(filepath.Join(out, "report.json"))
		if rep, err := ParseReport(b); err != nil || !rep.Interrupted {
			t.Fatalf("report %v", err)
		}
	})
	t.Run("bad-out", func(t *testing.T) {
		w := newWorld(t, fullModel())
		out := t.TempDir()
		os.WriteFile(filepath.Join(out, "x"), nil, 0o600)
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", out)
		if r := run(env); r.code != 2 || !strings.Contains(r.stderr, "must be empty") {
			t.Fatalf("bad out = %+v", r)
		}
	})
	t.Run("rand", func(t *testing.T) {
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", filepath.Join(t.TempDir(), "o"))
		env.Rand = strings.NewReader("")
		if r := run(env); r.code != 1 || !errors.Is(io.ErrUnexpectedEOF, io.ErrUnexpectedEOF) {
			t.Fatalf("rand = %+v", r)
		}
	})
}

func TestMainServe(t *testing.T) {
	dir := t.TempDir()
	caseFile, events := filepath.Join(dir, "case.json"), filepath.Join(dir, "events.jsonl")
	os.WriteFile(caseFile, mustJSON(t, testCases()), 0o600)
	w := newWorld(t, fullModel())
	env, _, _ := testEnv(t, w, "serve", "--case-file", caseFile, "--events", events)
	env.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	if r := run(env); r.code != 0 || r.stdout != `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n" {
		t.Fatalf("serve = %+v", r)
	}
	// A second instance appends to the same events file.
	env, _, _ = testEnv(t, w, "serve", "--case-file", caseFile, "--events", events)
	env.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if r := run(env); r.code != 5 {
		t.Fatalf("truncated input = %+v", r)
	}
	b, _ := os.ReadFile(events)
	if evs, _, err := ParseProbeEvents(b); err != nil || kinds(evs) != "start,eof,exit,start,error,exit" {
		t.Fatalf("events %s %v", kinds(evs), err)
	}
	os.WriteFile(caseFile, []byte(`{"version":2}`), 0o600)
	env, _, _ = testEnv(t, w, "serve", "--case-file", caseFile, "--events", events)
	if r := run(env); r.code != 2 {
		t.Fatalf("bad case file = %+v", r)
	}
	os.WriteFile(caseFile, mustJSON(t, testCases()), 0o600)
	env, _, _ = testEnv(t, w, "serve", "--case-file", caseFile, "--events", filepath.Join(dir, "no", "dir", "e.jsonl"))
	if r := run(env); r.code != 2 {
		t.Fatalf("bad events path = %+v", r)
	}
	if len(w.launches) != 0 {
		t.Fatal("serve launched a process")
	}
}
