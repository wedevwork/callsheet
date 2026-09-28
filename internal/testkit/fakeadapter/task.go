package fakeadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
)

// Task mode (iteration 05): the fake worker behind a dispatched task. It
// reads the composed prompt from stdin, validates the envelope without
// executing any instruction, never echoes manuals, and ends with exactly
// one final-message marker line. Only fixture code knows the mode
// variable; production knows only the task argv and the marker.
const (
	// TaskFlag selects task mode.
	TaskFlag = "--callsheet-task"
	// EnvTaskMode selects the fixture's task behavior: success (default),
	// fail, or output. Any other value exits 2.
	EnvTaskMode = "CALLSHEET_FAKE_TASK_MODE"
	// EnvTaskReport, set to 1, makes a task child also write one TaskReport
	// line on stderr describing what it actually got: its working
	// directory, that directory's permissions and its environment (never
	// the prompt). Fixture-only, for the native process qualification.
	EnvTaskReport = "CALLSHEET_FAKE_TASK_REPORT"
	// EnvTaskGroupDir and EnvTaskDescendantTerm drive the "group" task
	// mode (iteration 06a native group qualification, fixture only): the
	// task child spawns one descendant in its own process group (term mode
	// exit or ignore), publishes group.json {leader_pid, descendant_pid}
	// atomically in the absolute directory EnvTaskGroupDir, then waits
	// until a file named "go" appears there to write one output line
	// naming its PID and complete (it writes one "native started" line
	// naming its PID first). It installs no signal handler: TERM
	// ends it (the descendant follows its own term mode).
	EnvTaskGroupDir       = "CALLSHEET_FAKE_TASK_GROUP_DIR"
	EnvTaskDescendantTerm = "CALLSHEET_FAKE_TASK_DESCENDANT_TERM"
	// GroupFile and GroupTrigger are the group mode's file names.
	GroupFile    = "group.json"
	GroupTrigger = "go"
	// TaskFormat is the prompt envelope's format.
	TaskFormat = "callsheet-task-v1"
	// FinalMarker is the exact stdout line of a completed task.
	FinalMarker = `{"type":"callsheet_final","message":"fake task completed"}` + "\n"
	// FailureLine is the fail mode's stderr line; it exits TaskFailExit.
	FailureLine  = "fake task failure\n"
	TaskFailExit = 7
	// maxTaskStdin bounds the prompt read (16 MiB, plus one sentinel byte).
	maxTaskStdin = 16 << 20
	// OutputBytes is the output mode's deterministic volume before the
	// marker: beyond the 10 MiB retained tail.
	OutputBytes = 11 << 20
)

// TaskReport is the EnvTaskReport stderr line.
type TaskReport struct {
	Type    string   `json:"type"` // callsheet_fake_report
	CWD     string   `json:"cwd"`
	CWDMode string   `json:"cwd_mode"` // permission bits, 4-digit octal
	Env     []string `json:"env"`      // sorted
}

var taskIDRE = regexp.MustCompile(`^t_[0-9a-f]{32}$`)

// taskEnvelope is the part of the prompt the fixture validates.
type taskEnvelope struct {
	Format      *string          `json:"format"`
	Instruction *string          `json:"instruction"`
	Runbook     *string          `json:"runbook"`
	Task        *json.RawMessage `json:"task"`
}

type taskFields struct {
	TaskID          *string          `json:"task_id"`
	Target          *json.RawMessage `json:"target"`
	Role            *json.RawMessage `json:"role"`
	Goal            *string          `json:"goal"`
	Payload         *[]string        `json:"payload"`
	Acceptance      *string          `json:"acceptance"`
	TimeoutEnforced *bool            `json:"timeout_enforced"`
	RequestedBy     *json.RawMessage `json:"requested_by"`
	Effective       *struct {
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Timeout string `json:"timeout"`
	} `json:"effective"`
}

// validatePrompt checks the envelope: one JSON object and a final LF, the
// format, both manuals (content never inspected or echoed) and the task
// object whose effective model and effort equal the argv values.
func validatePrompt(b []byte, opts Options) error {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return fmt.Errorf("the prompt must be one JSON object followed by LF")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var env taskEnvelope
	if err := dec.Decode(&env); err != nil {
		return fmt.Errorf("the prompt is not a task envelope")
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("the prompt has trailing data")
	}
	switch {
	case env.Format == nil || *env.Format != TaskFormat:
		return fmt.Errorf("the prompt format must be %s", TaskFormat)
	case env.Instruction == nil || env.Runbook == nil || env.Task == nil:
		return fmt.Errorf("the prompt lacks instruction, runbook or task")
	}
	tdec := json.NewDecoder(bytes.NewReader(*env.Task))
	tdec.DisallowUnknownFields()
	var t taskFields
	if err := tdec.Decode(&t); err != nil {
		return fmt.Errorf("the task envelope is invalid")
	}
	switch {
	case t.TaskID == nil || !taskIDRE.MatchString(*t.TaskID):
		return fmt.Errorf("the task envelope lacks a task ID")
	case t.Target == nil || t.Role == nil || t.Goal == nil || t.Payload == nil || t.Acceptance == nil || t.RequestedBy == nil || t.Effective == nil || t.TimeoutEnforced == nil:
		return fmt.Errorf("the task envelope lacks a required field")
	case *t.TimeoutEnforced:
		return fmt.Errorf("the task envelope claims an enforced timeout")
	case t.Effective.Model != opts.Model || t.Effective.Effort != opts.Effort:
		return fmt.Errorf("the task envelope's model and effort differ from the explicit arguments")
	}
	return nil
}

// runTask is task mode: read and validate the prompt, then behave as
// EnvTaskMode selects.
func runTask(env Env, opts Options) int {
	stdout, stderr := env.Stdout, env.stderrOrDiscard()
	if stdout == nil {
		stdout = io.Discard
	}
	getenv := env.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	mode := getenv(EnvTaskMode)
	switch mode {
	case "", "success", "fail", "output", "group":
	default:
		fmt.Fprintf(stderr, "fake-adapter: unknown %s %q\n", EnvTaskMode, mode)
		return 2
	}
	in := env.Stdin
	if in == nil {
		in = os.Stdin
	}
	b, err := io.ReadAll(io.LimitReader(in, maxTaskStdin+1))
	if err != nil {
		fmt.Fprintf(stderr, "fake-adapter: reading the prompt: %v\n", err)
		return 1
	}
	if len(b) > maxTaskStdin {
		fmt.Fprintf(stderr, "fake-adapter: the prompt exceeds %d bytes\n", maxTaskStdin)
		return 2
	}
	if err := validatePrompt(b, opts); err != nil {
		fmt.Fprintf(stderr, "fake-adapter: %v\n", err)
		return 2
	}
	if getenv(EnvTaskReport) == "1" {
		if err := writeReport(stderr, env); err != nil {
			fmt.Fprintf(stderr, "fake-adapter: report: %v\n", err)
			return 1
		}
	}
	switch mode {
	case "fail":
		io.WriteString(stderr, FailureLine)
		return TaskFailExit
	case "group":
		return runGroup(env, stdout, stderr, getenv)
	case "output":
		w := bufio.NewWriterSize(stdout, 64<<10)
		for n := 0; n < OutputBytes; {
			line := "fake output line " + strconv.Itoa(n) + "\n"
			w.WriteString(line)
			n += len(line)
		}
		w.Flush()
	}
	io.WriteString(stdout, FinalMarker)
	return 0
}

// writeReport writes the TaskReport line: the working directory the
// child actually has, its permission bits and the sorted environment.
func writeReport(w io.Writer, env Env) error {
	dir := env.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = wd
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	environ := env.Environ
	if environ == nil {
		environ = os.Environ
	}
	vars := append([]string{}, environ()...)
	sort.Strings(vars)
	b, err := json.Marshal(TaskReport{Type: "callsheet_fake_report", CWD: dir, CWDMode: fmt.Sprintf("%04o", fi.Mode().Perm()), Env: vars})
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
