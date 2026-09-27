package fakeadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const taskSentinel = "TASK-SENTINEL-MANUAL-BODY-7c1e"

// prompt is a valid task prompt for model/effort, as the sidecar composes
// it (manual bodies carry the sentinel).
func prompt(model, effort string) []byte {
	env := map[string]any{
		"format":      TaskFormat,
		"instruction": taskSentinel + " instruction",
		"runbook":     taskSentinel + " runbook",
		"task": map[string]any{
			"task_id": "t_0123456789abcdef0123456789abcdef", "target": map[string]string{"kind": "id", "value": "worker"},
			"role": map[string]string{"id": "worker", "name": "implementer"}, "goal": "g", "payload": []string{}, "acceptance": "a",
			"effective": map[string]string{"model": model, "effort": effort, "timeout": "2h0m0s"}, "timeout_enforced": false,
			"requested_by": map[string]string{"name": "callsheet", "version": "dev", "hostname": "coord"},
		},
	}
	b, _ := json.Marshal(env)
	return append(b, '\n')
}

// runTaskMode runs the fixture in process (no child) with stdin and mode.
func runTaskMode(args []string, stdin []byte, mode string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), Env{Args: args, Stdout: &out, Stderr: &errOut, Stdin: bytes.NewReader(stdin),
		Getenv: func(k string) string {
			if k == EnvTaskMode {
				return mode
			}
			return ""
		}})
	return code, out.String(), errOut.String()
}

// TestTaskMode is UT FP-4 for the fixture's task mode, in process: argv,
// prompt validation, the three behaviors and no manual echo. It starts no
// child (launch ledger: 0).
func TestTaskMode(t *testing.T) {
	args := []string{TaskFlag, "--model", "example model", "--effort", "medium"}
	good := prompt("example model", "medium")
	t.Run("success", func(t *testing.T) {
		for _, mode := range []string{"", "success"} {
			code, out, errOut := runTaskMode(args, good, mode)
			if code != 0 || out != FinalMarker || errOut != "" {
				t.Fatalf("%q = %d %q %q", mode, code, out, errOut)
			}
		}
		// Flags in any order, --flag=value spelling.
		if code, out, _ := runTaskMode([]string{"--effort=medium", TaskFlag, "--model=example model"}, good, ""); code != 0 || out != FinalMarker {
			t.Fatalf("reordered = %d %q", code, out)
		}
	})
	t.Run("fail", func(t *testing.T) {
		code, out, errOut := runTaskMode(args, good, "fail")
		if code != 7 || TaskFailExit != 7 || out != "" || errOut != FailureLine {
			t.Fatalf("fail = %d %q %q", code, out, errOut)
		}
	})
	t.Run("output", func(t *testing.T) {
		code, out, errOut := runTaskMode(args, good, "output")
		if code != 0 || errOut != "" || len(out) < OutputBytes || len(out) <= 10<<20 || !strings.HasSuffix(out, FinalMarker) || !strings.HasPrefix(out, "fake output line 0\n") {
			t.Fatalf("output = %d, %d bytes, %q", code, len(out), errOut)
		}
		// The volume is the offset-numbered line sequence (deterministic by
		// construction) and never echoes a manual.
		if !strings.HasPrefix(out, "fake output line 0\nfake output line 19\n") || strings.Contains(out, taskSentinel) {
			t.Fatal("output mode content")
		}
	})
	t.Run("rejections", func(t *testing.T) {
		for name, c := range map[string]struct {
			args  []string
			stdin []byte
			mode  string
		}{
			"no model":      {[]string{TaskFlag, "--effort", "medium"}, good, ""},
			"no effort":     {[]string{TaskFlag, "--model", "m"}, good, ""},
			"empty model":   {[]string{TaskFlag, "--model", "", "--effort", "medium"}, good, ""},
			"operand":       {append(append([]string{}, args...), "extra"), good, ""},
			"fixture flag":  {append(append([]string{}, args...), "--exit-code=3"), good, ""},
			"echo":          {append(append([]string{}, args...), "--echo-argv"), good, ""},
			"unknown mode":  {args, good, "explode"},
			"no newline":    {args, bytes.TrimSuffix(good, []byte("\n")), ""},
			"format":        {args, bytes.Replace(good, []byte(TaskFormat), []byte("callsheet-task-v2"), 1), ""},
			"not json":      {args, []byte("instructions: do things\n"), ""},
			"empty":         {args, nil, ""},
			"trailing":      {args, append(bytes.TrimSuffix(bytes.Clone(good), []byte("\n")), []byte(" {}\n")...), ""},
			"extra field":   {args, bytes.Replace(good, []byte(`"format"`), []byte(`"extra":1,"format"`), 1), ""},
			"model differs": {[]string{TaskFlag, "--model", "other", "--effort", "medium"}, good, ""},
			"enforced":      {args, bytes.Replace(good, []byte(`"timeout_enforced":false`), []byte(`"timeout_enforced":true`), 1), ""},
			"no task id":    {args, bytes.Replace(good, []byte(`"task_id":"t_0123456789abcdef0123456789abcdef"`), []byte(`"task_id":"x"`), 1), ""},
			"oversize":      {args, append(bytes.Repeat([]byte(" "), maxTaskStdin), good...), ""},
		} {
			code, out, errOut := runTaskMode(c.args, c.stdin, c.mode)
			if code != 2 || out != "" || !strings.HasPrefix(errOut, "fake-adapter: ") {
				t.Fatalf("%s = %d %q %q", name, code, out, errOut)
			}
			if strings.Contains(out+errOut, taskSentinel) {
				t.Fatalf("%s echoed a manual", name)
			}
		}
		// Probe mode stays exclusive and unchanged.
		if _, err := Parse([]string{ProbeFlag, TaskFlag}); err == nil {
			t.Fatal("probe and task modes combined")
		}
	})
	t.Run("report", func(t *testing.T) {
		// EnvTaskReport adds one stderr line with the working directory,
		// its permissions and the sorted environment; never the prompt.
		dir := t.TempDir()
		os.Chmod(dir, 0o700)
		run := func(d string) (int, string, string) {
			var out, errOut bytes.Buffer
			code := Run(context.Background(), Env{Args: args, Stdout: &out, Stderr: &errOut, Stdin: bytes.NewReader(good), Dir: d,
				Getenv:  func(k string) string { return map[string]string{EnvTaskReport: "1"}[k] },
				Environ: func() []string { return []string{"Z=1", "A=2"} }})
			return code, out.String(), errOut.String()
		}
		code, out, errOut := run(dir)
		var r TaskReport
		if code != 0 || out != FinalMarker || json.Unmarshal([]byte(errOut), &r) != nil || r.Type != "callsheet_fake_report" || r.CWD != dir ||
			r.CWDMode != "0700" || strings.Join(r.Env, ",") != "A=2,Z=1" || strings.Contains(errOut, taskSentinel) || !strings.HasSuffix(errOut, "}\n") {
			t.Fatalf("report = %d %q %q", code, out, errOut)
		}
		if code, _, errOut := run(filepath.Join(dir, "gone")); code != 1 || !strings.Contains(errOut, "report") {
			t.Fatalf("unreadable cwd = %d %q", code, errOut)
		}
	})
	t.Run("no-echo", func(t *testing.T) {
		// (The prompt must be accepted, or not echoing it proves nothing.)
		for mode, want := range map[string]int{"": 0, "fail": TaskFailExit} { // output: checked in its subtest
			code, out, errOut := runTaskMode(args, good, mode)
			if code != want || strings.Contains(out+errOut, taskSentinel) {
				t.Fatalf("mode %q = %d, or echoed a manual", mode, code)
			}
		}
	})
}
