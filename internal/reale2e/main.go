// Package reale2e is the development-only real local end-to-end harness
// (design 12a-real-e2e, amendment A3): the manual startup gate, the live
// supervisor of one automatic attempt (owner-environment fingerprints,
// deployment, authenticated preflight, the headless two-phase coordinator,
// observation, the one-terminal owner decision, final validation,
// evidence, cleanup, the check and the post-run audit), the coordinator's
// observe, wait-task and review-material helpers and the offline
// evidence checker. The reale2e-tagged
// cmd/reale2e only forwards host inputs to Main; every decision lives here
// and is tested offline with injected processes, clock, plane, terminal
// and Git. It is never run in CI with real vendors.
package reale2e

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Usage is the command's usage text, with the bounds shown before opt-in.
const Usage = `usage:
  reale2e run --callsheet ABS --claude ABS --codex ABS --grok ABS --evidence ABS [--flow ABS] [--protect-root ABS ...] [--protect-depth N]
      (requires ` + EnvOptIn + `=1, no CI and no CLAUDE_CONFIG_DIR variable; linux only; calls paid models)
  reale2e check --evidence ABS
  reale2e observe --run ABS --task ID --hop designer|design-reviewer|coder|code-reviewer
  reale2e wait-task --run ABS --task ID --hop designer|design-reviewer|coder|code-reviewer
  reale2e review-material --run ABS
See docs/real-e2e.md.
` + Bounds

// EnvClaudeConfigDir present in any form refuses a live run: the
// effective Claude home could differ from the protected one.
const EnvClaudeConfigDir = "CLAUDE_CONFIG_DIR"

// Host is the command's host inputs; NewWorld builds the side-effect
// capabilities and is called only after the startup gate passed.
type Host struct {
	GOOS, GOARCH string
	Args         []string
	Getenv       func(string) string
	LookupEnv    func(string) (string, bool)
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
	NewWorld     func(h Host) (*World, func(), error)
	// observe, when set, sees the new supervisor before its attempt runs
	// (offline tests drive their scripted coordinator through it).
	observe func(*supervisor)
}

// Main is the reale2e command: 0 success, 1 failure or refusal of a
// helper's request, 2 usage, gate, schema or transport error;
// wait-task returns the wait's status on success.
func Main(goos, goarch string, args []string, getenv func(string) string, lookupEnv func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer) int {
	return MainWith(Host{GOOS: goos, GOARCH: goarch, Args: args, Getenv: getenv, LookupEnv: lookupEnv, Stdin: stdin, Stdout: stdout, Stderr: stderr,
		NewWorld: LiveWorld})
}

// usageError prints msg and the usage.
func usageError(h Host, msg string) int {
	fmt.Fprintf(h.Stderr, "reale2e: %s\n%s", msg, Usage)
	return 2
}

// flags parses a subcommand's string flags; every listed name is
// required except those in optional, each must be absolute when abs is
// set, and no positional argument is allowed. A non-nil roots collects
// the repeatable --protect-root values (each must be absolute).
func flags(h Host, sub string, names []string, optional map[string]bool, abs map[string]bool, roots *[]string) (map[string]string, bool) {
	fs := flag.NewFlagSet("reale2e "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vals := map[string]*string{}
	for _, n := range names {
		vals[n] = fs.String(n, "", "")
	}
	if roots != nil {
		fs.Var(collectRoots{vals: roots}, "protect-root", "")
	}
	if err := fs.Parse(h.Args[1:]); err != nil || fs.NArg() != 0 {
		return nil, false
	}
	if roots != nil {
		for _, r := range *roots {
			if !filepath.IsAbs(r) {
				return nil, false
			}
		}
	}
	out := map[string]string{}
	for _, n := range names {
		v := *vals[n]
		if v == "" && !optional[n] {
			return nil, false
		}
		if v != "" && abs[n] && !filepath.IsAbs(v) {
			return nil, false
		}
		out[n] = v
	}
	return out, true
}

// MainWith is Main over an explicit host.
func MainWith(h Host) int {
	if len(h.Args) == 0 {
		return usageError(h, "missing subcommand")
	}
	all := func(ns ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range ns {
			m[n] = true
		}
		return m
	}
	switch h.Args[0] {
	case "run":
		// The gate comes first: a refused live startup touches nothing.
		if err := Gate(h.GOOS, h.Getenv, h.LookupEnv); err != nil {
			fmt.Fprintf(h.Stderr, "%v\n%s", err, Bounds)
			return 2
		}
		// Immediately after the gate, before any capability exists: an
		// inherited CLAUDE_CONFIG_DIR (even empty) is refused, never unset.
		if _, set := h.LookupEnv(EnvClaudeConfigDir); set {
			fmt.Fprintf(h.Stderr, "reale2e run refused: claude_config_dir_unsupported: %s is set, so Claude's effective home could differ from "+
				"the protected ~/.claude; unset it yourself and start again\n", EnvClaudeConfigDir)
			return 2
		}
		names := []string{"callsheet", "claude", "codex", "grok", "evidence", "flow", "protect-depth"}
		var roots []string
		v, ok := flags(h, "run", names, all("flow", "protect-depth"), all("callsheet", "claude", "codex", "grok", "evidence", "flow"), &roots)
		depth, depthOK := DefaultProtectDepth, true
		if ok && v["protect-depth"] != "" {
			var err error
			depth, err = strconv.Atoi(v["protect-depth"])
			depthOK = err == nil && depth >= 0 && depth <= maxProtectDepth
		}
		if !ok || !depthOK {
			return usageError(h, "run needs absolute --callsheet, --claude, --codex, --grok and --evidence paths, an optional absolute --flow, "+
				"optional absolute --protect-root directories and an optional --protect-depth from 0 to 64 (default 3)")
		}
		return runLive(h, runArgs{callsheet: v["callsheet"], claude: v["claude"], codex: v["codex"], grok: v["grok"], evidence: v["evidence"], flow: v["flow"],
			protectRoots: roots, protectDepth: depth})
	case "check":
		v, ok := flags(h, "check", []string{"evidence"}, nil, all("evidence"), nil)
		if !ok {
			return usageError(h, "check needs an absolute --evidence bundle directory")
		}
		return CheckEvidence(v["evidence"], h.Stdout, h.Stderr)
	case "observe":
		v, ok := flags(h, "observe", []string{"run", "task", "hop"}, nil, all("run"), nil)
		if !ok || !contract.ValidTaskID(v["task"]) || hopIndex(v["hop"]) < 0 {
			return usageError(h, "observe needs an absolute --run runtime directory, a --task ID and a --hop of designer, design-reviewer, coder or code-reviewer")
		}
		return helper(h, v["run"], v["task"], v["hop"])
	case "wait-task":
		return waitTaskHelper(h)
	case "review-material":
		return reviewHelper(h)
	}
	// finish and wait were removed by A3: unknown subcommands.
	return usageError(h, "unknown subcommand "+contract.SafeText(h.Args[0], 32))
}

// helper sends one observe request: 0 ok, 1 refusal, 2 local error.
func helper(h Host, runtime, task, hop string) int {
	resp, err := controlCall(runtime, ControlRequest{Op: opObserve, Task: task, Hop: hop})
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e observe: %v\n", err)
		return 2
	}
	fmt.Fprintln(h.Stdout, resp.Code)
	if !resp.OK {
		return 1
	}
	return 0
}

// runLive builds the world and runs one attempt.
func runLive(h Host, a runArgs) int { return runWith(h, a, h.observe) }

// runWith is runLive with an optional observer of the new supervisor
// (offline tests drive their scripted coordinator through it).
func runWith(h Host, a runArgs, observe func(*supervisor)) int {
	w, release, err := h.NewWorld(h)
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e run: %v\n", err)
		return 1
	}
	defer release()
	s := &supervisor{stdout: h.Stdout, stderr: h.Stderr, goos: h.GOOS, goarch: h.GOARCH, w: w, args: a, ctx: w.Context,
		known: map[string]string{}, stoppedSet: map[*child]bool{}, reqs: make(chan ctrlMsg), loopDone: make(chan struct{})}
	if observe != nil {
		observe(s)
	}
	return s.run()
}

// LiveWorld is the live world: os/exec children, the wall clock, the
// process environment, SIGINT/SIGTERM cancellation, the adapters' own
// version probes and the verified plane client.
func LiveWorld(h Host) (*World, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	home, _ := os.UserHomeDir()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	environ := os.Environ()
	return &World{Context: ctx, Launcher: ExecLauncher{}, Clock: realClock{}, Environ: environ, Cwd: cwd, Executable: exe, Home: home,
		TempRoot: "/tmp", Rand: liveRand, LookPath: exec.LookPath, Probe: probeVendor, Connect: connectFor(h.GOOS, environ), Poll: 2 * time.Second,
		Terminal: &readerTerminal{r: h.Stdin}, Git: execGit(ExecLauncher{}, realClock{}, exec.LookPath, environ)}, stop, nil
}

// gitProbeTimeout bounds one read-only git probe.
const gitProbeTimeout = 60 * time.Second

// execGit is the live GitRunner: the PATH's git started through the
// launcher (its own process group, stopped by handle), read-only options,
// optional locks off, output bounded.
func execGit(l Launcher, clock Clock, lookPath func(string) (string, error), environ []string) GitRunner {
	return func(dir string, out io.Writer, args ...string) ([]byte, int, error) {
		bin, err := lookPath("git")
		if err != nil {
			return nil, 0, err
		}
		// Inherited GIT_* variables (a GIT_DIR or GIT_WORK_TREE) would
		// redirect the probe; they are dropped.
		var env []string
		for _, kv := range environ {
			if !strings.HasPrefix(kv, "GIT_") {
				env = append(env, kv)
			}
		}
		env = append(env, gitProbeEnv)
		// A streamed probe (out set) writes its output there, unbounded by
		// the captured-output limit; its stderr stays bounded.
		res, err := runCommand(l, clock, ProcSpec{Path: bin, Args: append([]string{"-C", dir}, args...), Env: env, Dir: dir, Stdout: out}, gitProbeTimeout, maxGitOutput)
		switch {
		case err != nil:
			return nil, 0, err
		case res.Truncated:
			return nil, res.ExitCode, errGitOutputLimit
		case res.TimedOut || res.Signal != "":
			return nil, res.ExitCode, errors.New("git probe did not complete within its bounds")
		}
		return res.Stdout, res.ExitCode, nil
	}
}

// probeVendor decides eligibility with the existing adapter's own version
// probe and policy.
func probeVendor(ctx context.Context, vendor, exe, dir string) error {
	var a adapter.Adapter
	switch vendor {
	case adapter.ClaudeID:
		a = adapter.NewClaude(dir)
	case adapter.CodexID:
		a = adapter.NewCodex(dir)
	case adapter.GrokID:
		a = adapter.NewGrok(dir)
	default:
		return errors.New("unknown vendor")
	}
	return a.Probe(ctx, exe)
}

// connectFor returns the live plane connection over a verified client;
// pulls install task results with the product's own transfer code.
func connectFor(goos string, environ []string) func(url string, caPEM []byte) (Plane, Puller, func(), error) {
	return func(url string, caPEM []byte) (Plane, Puller, func(), error) {
		c, err := client.New(url, client.Trust{CAPEM: caPEM})
		if err != nil {
			return nil, nil, nil, err
		}
		return c, livePuller{c: c, goos: goos, env: workspacetransfer.ProcessEnv(isolatedGitEnv(environ))}, c.Close, nil
	}
}

// isolatedGitEnv keeps no user or system git configuration for the
// supervisor's own pulls.
func isolatedGitEnv(environ []string) []string {
	return []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=" + envValue(environ, "PATH")}
}

// livePuller pulls a task's published result by task ID.
type livePuller struct {
	c    *client.Client
	goos string
	env  workspacetransfer.Env
}

func (p livePuller) PullResult(ctx context.Context, taskID, repo string) error {
	sel, err := p.c.ResolveTaskResult(ctx, taskID)
	if err != nil {
		return err
	}
	_, err = workspacetransfer.Pull(ctx, workspacetransfer.Options{GOOS: p.goos, Env: p.env, Cwd: repo, Plane: workspacetransfer.ClientPlane(p.c)},
		workspacetransfer.PullRequest{Name: sel.Name, Ref: sel.Ref, Path: repo, PathSet: true, ExpectedInstance: sel.Instance, ExpectedCommit: sel.Commit})
	return err
}
