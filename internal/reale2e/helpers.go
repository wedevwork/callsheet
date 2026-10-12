package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// The coordinator's restricted helpers (design 12a-real-e2e, A3.6):
// capture-wait saves a hop's background wait status after the plain CLI
// wait and returns it, and review-material emits the code reviewer's
// labelled reference files. Each accepts exactly its listed options,
// requires the live runtime's ownership marker and run identity, makes one
// control exchange and touches only fixed paths derived from the runtime,
// never following a symlink below it and never reading a nonregular file.
// They dispatch, approve and wait for nothing, and run no subprocess.

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
// returns that run ID.
func liveRuntime(runtime string) (string, error) {
	if !filepath.IsAbs(runtime) || filepath.Clean(runtime) != runtime {
		return "", errUnsafePath
	}
	if st, err := os.Lstat(runtime); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", errUnsafePath
	}
	run, err := readRunFile(runtime)
	if err != nil {
		return "", err
	}
	marker, err := readRuntimeFile(runtime, maxControlLine, ownerMarker)
	if err != nil {
		return "", err
	}
	if string(marker) != run+"\n" {
		return "", errors.New("the runtime's ownership marker does not name its run")
	}
	return run, nil
}

// captureFiles is capture-wait's local action once the supervisor
// accepted it: hop i's bounded result and stderr are read, its exit
// artifact is created (never overwritten) with code, and the captured
// streams are copied to stdout and stderr.
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

// captureHelper is reale2e capture-wait: exit N on a successful capture,
// 1 on the supervisor's refusal, 2 on a usage, local validation, I/O or
// protocol error (no exit artifact is then written).
func captureHelper(h Host) int {
	v, ok := helperFlags(h.Args[1:], "run", "hop", "code")
	code, err := strconv.Atoi(v["code"])
	if !ok || !filepath.IsAbs(v["run"]) || hopIndex(v["hop"]) < 0 || err != nil || code < 0 || code > 255 || strconv.Itoa(code) != v["code"] {
		return usageError(h, "capture-wait needs exactly an absolute --run runtime directory, a --hop of designer, design-reviewer, coder or "+
			"code-reviewer and a --code from 0 to 255")
	}
	fail := func(err error) int {
		fmt.Fprintf(h.Stderr, "reale2e capture-wait: %v\n", err)
		return 2
	}
	if _, err := liveRuntime(v["run"]); err != nil {
		return fail(err)
	}
	resp, err := controlCall(v["run"], ControlRequest{Op: opCapture, Hop: v["hop"], Code: code})
	if err != nil {
		return fail(err)
	}
	if !resp.OK {
		fmt.Fprintf(h.Stderr, "reale2e capture-wait: refused: %s\n", resp.Code)
		return 1
	}
	if err := captureFiles(v["run"], hopIndex(v["hop"]), code, h.Stdout, h.Stderr); err != nil {
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
