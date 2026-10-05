// Command worker-replay is the iteration 08 function tests' replay stub
// for the Claude and Codex worker CLIs (testdata keeps it out of ./... and
// of coverage; tests/function builds it once per test process by explicit
// path). It is enabled only through the product's real --claude-adapter or
// --codex-adapter option: a link or copy named claude or codex selects the
// vendor it impersonates. It never invokes a shell, a model, a vendor
// binary, the network or any command named in the captured prose, and it
// never performs the canaries' writes: every output byte comes from a
// capture, or from a scenario's explicitly synthetic robustness case.
//
// Its scenario directory is CALLSHEET_WORKER_REPLAY_DIR, set only in the
// test's sidecar environment (no product code reads it):
//
//	<dir>/<vendor>.version     the exact --version stdout to print
//	<dir>/scenarios/*.json     task scenarios, selected by the task goal
//	<dir>/launches/            one JSON launch record per invocation
//
// In task mode it reads stdin to EOF, selects the scenario whose goal is
// the task envelope's goal, validates the complete argv (one final-path
// placeholder), the working directory (the task's private non-git scratch
// directory) and, when the scenario names it, the exact stdin bytes (one
// task-ID placeholder), records the launch, copies the capture's stdout and
// stderr unchanged, writes the captured final bytes to the requested final
// path only when the capture says the file was present (or applies the
// scenario's file fault), and exits with the captured exit status. A failed
// validation is recorded and reported as a distinct fixture failure: exit
// 97.
//
// Iteration 11 adds the grok and cursor impersonations (a link named grok,
// or cursor-agent or cursor, selects them) for version calls. In Grok's task
// mode the task envelope is the final -p argument and stdin must be empty
// (zero bytes); the scenario's ExpectedPrompt (one task-ID placeholder) is
// compared with that argument and its ExpectedStdin must be the explicit
// empty string. Cursor has no task mode: any attempted Cursor task launch
// is recorded and fails as a fixture failure.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// EnvDir names the scenario directory.
const EnvDir = "CALLSHEET_WORKER_REPLAY_DIR"

// fixtureFailure is the exit status of a failed invocation validation.
const fixtureFailure = 97

// Scenario is one task scenario.
type Scenario struct {
	// Goal selects the scenario (the task envelope's goal).
	Goal   string `json:"goal"`
	Vendor string `json:"vendor"`
	// SourceArgv is the capture's argv (evidence, never executed);
	// ExpectedArgv the production argv with "{final}" for the final path;
	// Normalization states how they differ.
	SourceArgv    []string `json:"source_argv"`
	ExpectedArgv  []string `json:"expected_argv"`
	Normalization string   `json:"normalization"`
	// ExpectedStdin, when set, is the exact stdin with "{task_id}" (for
	// Grok it must be set, to the empty string).
	ExpectedStdin *string `json:"expected_stdin"`
	// ExpectedPrompt (iteration 11), when set, is Grok's exact -p value
	// (the composed prompt) with "{task_id}"; ExpectedArgv names that
	// element "{prompt}".
	ExpectedPrompt *string `json:"expected_prompt"`
	// Capture is the capture directory (absolute); Stdout, Stderr and Exit
	// override it for synthetic robustness cases (Synthetic true).
	Capture   string  `json:"capture"`
	Synthetic bool    `json:"synthetic"`
	Stdout    *string `json:"stdout"`
	Stderr    *string `json:"stderr"`
	Exit      *int    `json:"exit"`
	// FinalMode is the final file's handling: "" (the capture's presence
	// and bytes), "absent", "custom" (Final), "symlink", "fifo",
	// "directory" or "invalid-utf8".
	FinalMode string `json:"final_mode"`
	Final     string `json:"final"`
	// Hold keeps the process alive after its output until it is signaled.
	Hold bool `json:"hold"`
}

// Launch is one invocation's record.
type Launch struct {
	Mode     string   `json:"mode"`
	Vendor   string   `json:"vendor"`
	Argv0    string   `json:"argv0"`
	Argv     []string `json:"argv"`
	Cwd      string   `json:"cwd"`
	CwdMode  string   `json:"cwd_mode"`
	Stdin    []byte   `json:"stdin"`
	Prompt   []byte   `json:"prompt"`
	TaskID   string   `json:"task_id"`
	Scenario string   `json:"scenario"`
	Final    string   `json:"final"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error"`
	PID      int      `json:"pid"`
}

func main() { os.Exit(run()) }

func run() int {
	l := Launch{Argv0: os.Args[0], Argv: os.Args[1:], Vendor: filepath.Base(os.Args[0]), PID: os.Getpid()}
	l.Cwd, _ = os.Getwd()
	if fi, err := os.Stat(l.Cwd); err == nil {
		l.CwdMode = fmt.Sprintf("%04o", fi.Mode().Perm())
	}
	dir := os.Getenv(EnvDir)
	fail := func(format string, a ...any) int {
		l.Error = fmt.Sprintf(format, a...)
		record(dir, l)
		fmt.Fprintf(os.Stderr, "worker-replay: fixture failure: %s\n", l.Error)
		return fixtureFailure
	}
	if !filepath.IsAbs(dir) {
		return fail("%s is not an absolute directory", EnvDir)
	}
	if l.Vendor == "cursor-agent" {
		l.Vendor = "cursor"
	}
	if l.Vendor != "claude" && l.Vendor != "codex" && l.Vendor != "grok" && l.Vendor != "cursor" {
		return fail("invoked as %q, not claude, codex, grok or cursor-agent", l.Vendor)
	}
	if len(l.Argv) == 1 && l.Argv[0] == "--version" {
		l.Mode = "version"
		b, err := os.ReadFile(filepath.Join(dir, l.Vendor+".version"))
		if err != nil {
			return fail("no version for %s", l.Vendor)
		}
		l.OK = true
		record(dir, l)
		os.Stdout.Write(b)
		return 0
	}
	l.Mode = "task"
	stdin, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<20+1))
	l.Stdin = stdin
	if err != nil || len(stdin) > 16<<20 {
		return fail("stdin unreadable or over 16 MiB")
	}
	if l.Vendor == "cursor" {
		// The refused adapter: no production path may launch a task.
		return fail("cursor has no task mode: a task launch of the refused adapter")
	}
	// The envelope: stdin for claude and codex; Grok's final -p argument,
	// with zero stdin bytes.
	envelope := stdin
	if l.Vendor == "grok" {
		if len(l.Argv) < 2 || l.Argv[len(l.Argv)-2] != "-p" {
			return fail("grok argv %q does not end with -p and the prompt", l.Argv)
		}
		if len(stdin) != 0 {
			return fail("grok stdin carries %d bytes, want none", len(stdin))
		}
		envelope = []byte(l.Argv[len(l.Argv)-1])
		l.Prompt = envelope
	}
	var env struct {
		Format string `json:"format"`
		Task   struct {
			TaskID string `json:"task_id"`
			Goal   string `json:"goal"`
		} `json:"task"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil || env.Format != "callsheet-task-v1" {
		return fail("the task input is not a callsheet-task-v1 envelope")
	}
	l.TaskID = env.Task.TaskID
	sc, name, err := scenario(dir, env.Task.Goal)
	if err != nil {
		return fail("%v", err)
	}
	l.Scenario = name
	if sc.Vendor != l.Vendor {
		return fail("scenario %s is for %s, invoked as %s", name, sc.Vendor, l.Vendor)
	}
	// The private, non-git, journal-owned working directory of this task
	// (iteration 10b): <state>/tasks/<task_id>/work.
	if filepath.Base(l.Cwd) != "work" || filepath.Base(filepath.Dir(l.Cwd)) != l.TaskID ||
		filepath.Base(filepath.Dir(filepath.Dir(l.Cwd))) != "tasks" || l.CwdMode != "0700" {
		return fail("cwd %s (mode %s) is not the task's private scratch directory", l.Cwd, l.CwdMode)
	}
	if _, err := os.Lstat(filepath.Join(l.Cwd, ".git")); err == nil {
		return fail("cwd is a git checkout")
	}
	// The complete argv: exactly the expected elements, the final path
	// the only variable, inside this cwd.
	final := ""
	if len(l.Argv) != len(sc.ExpectedArgv) {
		return fail("argv %q, want %q", l.Argv, sc.ExpectedArgv)
	}
	for i, want := range sc.ExpectedArgv {
		if want == "{prompt}" {
			if l.Vendor != "grok" || i != len(sc.ExpectedArgv)-1 {
				return fail("a prompt placeholder outside grok's last argument")
			}
			continue
		}
		if want == "{final}" {
			final = l.Argv[i]
			if final != filepath.Join(l.Cwd, "callsheet-final.txt") {
				return fail("final path %s is not the cwd's callsheet-final.txt", final)
			}
			continue
		}
		if l.Argv[i] != want {
			return fail("argv %q, want %q", l.Argv, sc.ExpectedArgv)
		}
	}
	l.Final = final
	if l.Vendor == "grok" {
		if sc.ExpectedStdin == nil || *sc.ExpectedStdin != "" {
			return fail("a grok scenario must expect the explicit empty stdin")
		}
		if sc.ExpectedPrompt != nil {
			if want := strings.ReplaceAll(*sc.ExpectedPrompt, "{task_id}", l.TaskID); !bytes.Equal(l.Prompt, []byte(want)) {
				return fail("the -p value differs from the expected composed prompt (%d bytes, want %d)", len(l.Prompt), len(want))
			}
		}
	}
	if sc.ExpectedStdin != nil {
		if want := strings.ReplaceAll(*sc.ExpectedStdin, "{task_id}", l.TaskID); !bytes.Equal(stdin, []byte(want)) {
			return fail("stdin differs from the expected composed prompt (%d bytes, want %d)", len(stdin), len(want))
		}
	}
	stdout, stderr, exit, present, finalBytes, err := outputs(sc)
	if err != nil {
		return fail("%v", err)
	}
	if final != "" {
		if err := writeFinal(sc.FinalMode, final, present, finalBytes, sc.Final); err != nil {
			return fail("final file: %v", err)
		}
	}
	l.OK = true
	os.Stdout.Write(stdout)
	os.Stderr.Write(stderr)
	record(dir, l)
	if sc.Hold {
		// Until the group's TERM (default disposition) ends the process.
		for {
			time.Sleep(time.Hour)
		}
	}
	return exit
}

// scenario loads the scenario whose goal is goal.
func scenario(dir, goal string) (Scenario, string, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "scenarios", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var sc Scenario
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sc); err != nil {
			return Scenario{}, "", fmt.Errorf("scenario %s: %v", filepath.Base(f), err)
		}
		if sc.Goal == goal {
			return sc, filepath.Base(f), nil
		}
	}
	return Scenario{}, "", fmt.Errorf("no scenario for the task's goal")
}

// outputs returns the replayed stdout, stderr and exit, and the final
// file's captured presence and bytes.
func outputs(sc Scenario) ([]byte, []byte, int, bool, []byte, error) {
	var stdout, stderr, final []byte
	exit, present := 0, false
	if sc.Capture != "" {
		read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(sc.Capture, name)) }
		var err error
		if stdout, err = read("stdout.bin"); err != nil {
			return nil, nil, 0, false, nil, err
		}
		if stderr, err = read("stderr.bin"); err != nil {
			return nil, nil, 0, false, nil, err
		}
		e, err := read("exit.txt")
		if err != nil {
			return nil, nil, 0, false, nil, err
		}
		if exit, err = strconv.Atoi(strings.TrimSpace(string(e))); err != nil {
			return nil, nil, 0, false, nil, err
		}
		if p, err := read("final-presence.txt"); err == nil && strings.TrimSpace(string(p)) == "present" {
			present = true
			if final, err = read("final.saved.txt"); err != nil {
				return nil, nil, 0, false, nil, err
			}
		}
	} else if !sc.Synthetic {
		return nil, nil, 0, false, nil, errors.New("a scenario without a capture must be marked synthetic")
	}
	if sc.Stdout != nil {
		stdout = []byte(*sc.Stdout)
	}
	if sc.Stderr != nil {
		stderr = []byte(*sc.Stderr)
	}
	if sc.Exit != nil {
		exit = *sc.Exit
	}
	return stdout, stderr, exit, present, final, nil
}

// writeFinal applies the final-file handling at path.
func writeFinal(mode, path string, present bool, captured []byte, custom string) error {
	switch mode {
	case "":
		if present {
			return os.WriteFile(path, captured, 0o600)
		}
		return nil
	case "absent":
		return nil
	case "custom":
		return os.WriteFile(path, []byte(custom), 0o600)
	case "symlink":
		os.WriteFile(filepath.Join(filepath.Dir(path), "elsewhere.txt"), []byte("not the answer"), 0o600)
		return os.Symlink(filepath.Join(filepath.Dir(path), "elsewhere.txt"), path)
	case "fifo":
		return syscall.Mkfifo(path, 0o600)
	case "directory":
		return os.Mkdir(path, 0o700)
	case "invalid-utf8":
		return os.WriteFile(path, []byte("po\xffng"), 0o600)
	}
	return fmt.Errorf("unknown final mode %q", mode)
}

// record writes one launch record (a file per launch: concurrent tasks
// never interleave).
func record(dir string, l Launch) {
	if !filepath.IsAbs(dir) {
		return
	}
	b, err := json.Marshal(l)
	if err != nil {
		return
	}
	d := filepath.Join(dir, "launches")
	os.MkdirAll(d, 0o755)
	name := fmt.Sprintf("%d-%d-%s.json", time.Now().UnixNano(), l.PID, l.Mode)
	// Written aside and renamed: a reader never sees a partial record.
	tmp := filepath.Join(d, "."+name+".tmp")
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, filepath.Join(d, name))
	}
}
