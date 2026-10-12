package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The coordinator's restricted helpers (design 12a-real-e2e, A3.6, A3.7):
// wait-task runs a hop's Callsheet wait itself, saves its output and
// status and returns that status, and review-material emits the code
// reviewer's labelled reference files. Each accepts exactly its listed
// options, requires the live runtime's ownership marker and run identity,
// makes one control exchange and touches only fixed paths derived from
// the runtime, never following a symlink below it and never reading a
// nonregular file. They dispatch and approve nothing; the only subprocess
// is wait-task's `callsheet task wait`, with the executable, plane and CA
// of the run file.

// errUnsafePath is a runtime path that is not a real directory chain
// ending in a regular file.
var errUnsafePath = errors.New("a runtime path is not a real directory or regular file below the runtime")

// errHelperBound is a helper input or output over its bound.
var errHelperBound = errors.New("a helper input or output exceeds its bound")

// errReviewMismatch is a pulled coder file whose bytes differ from the
// supervisor's admitted coder commit.
var errReviewMismatch = errors.New("a pulled coder file does not match the admitted coder commit")

// helperFlags parses exactly the listed options, each given once as
// --name value or --name=value; an unknown option, a duplicate, a missing
// option or a positional argument fails.
func helperFlags(args []string, names ...string) (map[string]string, bool) {
	out := map[string]string{}
	for len(args) > 0 {
		opt, ok := strings.CutPrefix(args[0], "--")
		name, val, inline := strings.Cut(opt, "=")
		if _, dup := out[name]; !ok || dup || !slices.Contains(names, name) {
			return nil, false
		}
		args = args[1:]
		if !inline {
			if len(args) == 0 {
				return nil, false
			}
			val, args = args[0], args[1:]
		}
		out[name] = val
	}
	return out, len(out) == len(names)
}

// readRuntimeFile reads the fixed file rel below the runtime within limit:
// every intermediate component a real directory, the file itself regular,
// opened without following a symlink.
func readRuntimeFile(runtime string, limit int64, rel ...string) ([]byte, error) {
	p := runtime
	for i, c := range rel {
		p = filepath.Join(p, c)
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if last := i == len(rel)-1; (!last && !st.IsDir()) || (last && !st.Mode().IsRegular()) {
			return nil, errUnsafePath
		}
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errUnsafePath
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errHelperBound
	}
	return b, nil
}

// liveRuntime validates a helper's runtime: an absolute, clean, real
// directory whose ownership marker names the run its run file holds. It
// returns that run file.
func liveRuntime(runtime string) (RunFile, error) {
	if !filepath.IsAbs(runtime) || filepath.Clean(runtime) != runtime {
		return RunFile{}, errUnsafePath
	}
	if st, err := os.Lstat(runtime); err != nil {
		return RunFile{}, err
	} else if !st.IsDir() {
		return RunFile{}, errUnsafePath
	}
	rf, err := readRunFile(runtime)
	if err != nil {
		return RunFile{}, err
	}
	marker, err := readRuntimeFile(runtime, maxControlLine, ownerMarker)
	if err != nil {
		return RunFile{}, err
	}
	if string(marker) != rf.RunID+"\n" {
		return RunFile{}, errors.New("the runtime's ownership marker does not name its run")
	}
	return rf, nil
}

// errWaitArtifact is a hop wait file that exists before its wait ran.
var errWaitArtifact = errors.New("the hop's wait output already exists")

// boundedFile writes at most n bytes to f and discards the rest, so a
// wait's output past its bound is detected by the capture without
// stalling the wait; the first write error is kept for later.
type boundedFile struct {
	f   *os.File
	n   int64
	err error
}

func (b *boundedFile) Write(p []byte) (int, error) {
	if q := p[:min(int64(len(p)), b.n)]; len(q) > 0 && b.err == nil {
		_, b.err = b.f.Write(q)
		b.n -= int64(len(q))
	}
	return len(p), nil
}

// runWait runs hop i's Callsheet wait for task with the run file's
// executable, plane and CA, streaming its stdout and stderr into the
// hop's new result and error files (each kept to one byte over its bound,
// so the capture refuses an oversized output). It returns the wait's
// status as a shell reports it (128+N for signal N); an error means no
// status exists.
func runWait(runtime string, rf RunFile, i int, task string) (int, error) {
	dir := waitDir(runtime, i)
	for _, p := range []string{filepath.Join(runtime, "coord"), filepath.Join(runtime, "coord", "waits"), dir} {
		if st, err := os.Lstat(p); err != nil {
			return 0, err
		} else if !st.IsDir() {
			return 0, errUnsafePath
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, runtimeWaitExit)); !errors.Is(err, fs.ErrNotExist) {
		return 0, errWaitArtifact
	}
	var outs [2]*boundedFile
	for k, name := range []string{runtimeWaitResult, runtimeWaitStderr} {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				err = errWaitArtifact
			}
			return 0, err
		}
		defer f.Close()
		outs[k] = &boundedFile{f: f, n: maxTextFile + 1}
	}
	cmd := exec.Command(rf.Callsheet, "task", "wait", task, "--until-done", "--json", "--plane", rf.Plane, "--ca", rf.CA)
	cmd.Stdout, cmd.Stderr = outs[0], outs[1]
	err := cmd.Run()
	if werr := errors.Join(outs[0].err, outs[1].err, outs[0].f.Close(), outs[1].f.Close()); werr != nil {
		return 0, werr
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return 0, err
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return exit.ExitCode(), nil
}

// captureFiles is wait-task's capture once the wait ended: hop i's bounded
// result and stderr are read, its exit artifact is created (never
// overwritten) with code, and the captured streams are copied to stdout
// and stderr.
func captureFiles(runtime string, i, code int, stdout, stderr io.Writer) error {
	dir := []string{"coord", "waits", hopDir(i)}
	result, err := readRuntimeFile(runtime, maxTextFile, dir[0], dir[1], dir[2], runtimeWaitResult)
	if err != nil {
		return err
	}
	errText, err := readRuntimeFile(runtime, maxTextFile, dir[0], dir[1], dir[2], runtimeWaitStderr)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(waitDir(runtime, i), runtimeWaitExit), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "%d\n", code)
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	stdout.Write(result)
	stderr.Write(errText)
	return nil
}

// reviewOutput is review-material's local action: the three pulled coder
// files, each verified against the supervisor's expected hash, in the
// labelled BEGIN/END FILE format, complete or not at all.
func reviewOutput(runtime string, files []ReviewFile) ([]byte, error) {
	if len(files) != len(inlinePaths) {
		return nil, errReviewMismatch
	}
	var b bytes.Buffer
	for i, p := range inlinePaths {
		if files[i].Path != p {
			return nil, errReviewMismatch
		}
		data, err := readRuntimeFile(runtime, projectBound, "coord", "coder", p)
		if err != nil {
			return nil, err
		}
		if sha256Hex(data) != files[i].SHA256 {
			return nil, errReviewMismatch
		}
		b.WriteString(fileBlockBegin(p, files[i].SHA256))
		b.Write(data)
		b.WriteString(fileBlockEnd(p))
	}
	if b.Len() > adapter.MaxGrokPromptBytes {
		return nil, errHelperBound
	}
	return b.Bytes(), nil
}

// waitTaskHelper is reale2e wait-task (A3.7): once the supervisor confirms
// the task is the one it accepted for the hop, it runs the wait, captures
// it and exits with the wait's status; 1 on the supervisor's refusal
// (nothing runs), 2 on a usage, local validation, start, I/O or protocol
// error (no exit artifact is then written).
func waitTaskHelper(h Host) int {
	v, ok := helperFlags(h.Args[1:], "run", "task", "hop")
	if !ok || !filepath.IsAbs(v["run"]) || !contract.ValidTaskID(v["task"]) || hopIndex(v["hop"]) < 0 {
		return usageError(h, "wait-task needs exactly an absolute --run runtime directory, a --task ID and a --hop of designer, design-reviewer, "+
			"coder or code-reviewer")
	}
	fail := func(err error) int {
		fmt.Fprintf(h.Stderr, "reale2e wait-task: %v\n", err)
		return 2
	}
	rf, err := liveRuntime(v["run"])
	if err != nil {
		return fail(err)
	}
	resp, err := controlCall(v["run"], ControlRequest{Op: opWaitTask, Task: v["task"], Hop: v["hop"]})
	if err != nil {
		return fail(err)
	}
	if !resp.OK {
		fmt.Fprintf(h.Stderr, "reale2e wait-task: refused: %s\n", resp.Code)
		return 1
	}
	i := hopIndex(v["hop"])
	code, err := runWait(v["run"], rf, i, v["task"])
	if err != nil {
		return fail(err)
	}
	if err := captureFiles(v["run"], i, code, h.Stdout, h.Stderr); err != nil {
		return fail(err)
	}
	return code
}

// reviewHelper is reale2e review-material: 0 on complete output, 1 on the
// supervisor's refusal or a file mismatch, 2 on a usage, transport or I/O
// error; nothing is written to stdout unless every file verified.
func reviewHelper(h Host) int {
	v, ok := helperFlags(h.Args[1:], "run")
	if !ok || !filepath.IsAbs(v["run"]) {
		return usageError(h, "review-material needs exactly an absolute --run runtime directory")
	}
	if _, err := liveRuntime(v["run"]); err != nil {
		fmt.Fprintf(h.Stderr, "reale2e review-material: %v\n", err)
		return 2
	}
	resp, err := controlCall(v["run"], ControlRequest{Op: opReview})
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e review-material: %v\n", err)
		return 2
	}
	if !resp.OK {
		fmt.Fprintf(h.Stderr, "reale2e review-material: refused: %s\n", resp.Code)
		return 1
	}
	out, err := reviewOutput(v["run"], resp.Files)
	if err != nil {
		fmt.Fprintf(h.Stderr, "reale2e review-material: %v\n", err)
		if errors.Is(err, errReviewMismatch) {
			return 1
		}
		return 2
	}
	h.Stdout.Write(out)
	return 0
}
