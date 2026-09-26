package devcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// NativePackage is the package whose process-group tests qualify a native
// macOS run.
const NativePackage = "github.com/wedevwork/callsheet/tests/function"

// nativeRequired are the tests in NativePackage that must both run and pass.
var nativeRequired = []string{
	"TestFP6ProcessGroups",
	"TestFP6ProcessGroups/cooperative",
	"TestFP6ProcessGroups/resistant",
	"TestFP6ProcessGroups/leader-exits-first",
}

// NativeRequiredTests returns a fresh copy of the test names in NativePackage
// that a native run must execute and pass.
func NativeRequiredTests() []string { return append([]string(nil), nativeRequired...) }

// unobserved marks every failure that leaves native qualification unproven.
const unobserved = "native qualification unobserved"

func unsupportedNative(goos string) error {
	return fmt.Errorf("devcheck: native stage is unsupported on %q: it qualifies darwin only (use test or all on other hosts)", goos)
}

// NativeSteps returns the native qualification plan: the complete suite as a
// go test JSON event stream. Only darwin is supported; every other goos
// (including linux and windows) is rejected before any child runs.
func NativeSteps(goos string) ([]Step, error) {
	if goos != "darwin" {
		return nil, unsupportedNative(goos)
	}
	return []Step{{Name: "native", Argv: []string{"go", "test", "-json", "-count=1", "-timeout=180s", "./..."}}}, nil
}

// acceptedActions is the exact go test -json Action set the parser accepts.
var acceptedActions = map[string]bool{
	"start": true, "run": true, "pause": true, "cont": true, "output": true, "bench": true,
	"pass": true, "fail": true, "skip": true, "build-output": true, "build-fail": true,
}

// testEvent holds the fields the parser reads; unknown fields are ignored.
type testEvent struct {
	Action      *string
	Package     string
	Test        string
	Output      string
	FailedBuild string
	ImportPath  string
}

const (
	maxQuote     = 600
	maxTailLines = 20
	maxTailLine  = 300
)

// quote renders a raw event for a diagnostic, bounded in size.
func quote(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > maxQuote {
		s = s[:maxQuote] + "..."
	}
	return s
}

type pkgState struct {
	done, passed, ranTests bool
}

type testState struct {
	ran, done, passed bool
	// tail is a bounded window of the test's most recent output lines.
	tail []string
}

// nativeChecker is the streaming validation state: per-package and per-test
// status plus bounded diagnostics, never the full output.
type nativeChecker struct {
	n     int
	pkgs  map[string]*pkgState
	order []string
	tests map[string]*testState
}

func testKey(pkg, test string) string { return pkg + "\x00" + test }

func (c *nativeChecker) test(pkg, name string) *testState {
	k := testKey(pkg, name)
	st := c.tests[k]
	if st == nil {
		st = &testState{}
		c.tests[k] = st
	}
	return st
}

func (c *nativeChecker) fail(raw []byte, format string, args ...any) error {
	return fmt.Errorf("devcheck: native: event %d: %s: %s", c.n, fmt.Sprintf(format, args...), quote(raw))
}

// output appends an output line to the owning test's bounded tail.
func (c *nativeChecker) output(ev testEvent) {
	line := strings.TrimRight(ev.Output, "\n")
	if line == "" || ev.Package == "" {
		return
	}
	if len(line) > maxTailLine {
		line = line[:maxTailLine] + "..."
	}
	st := c.test(ev.Package, ev.Test)
	st.tail = append(st.tail, line)
	if len(st.tail) > maxTailLines {
		st.tail = st.tail[len(st.tail)-maxTailLines:]
	}
}

func (c *nativeChecker) tailOf(pkg, test string) string {
	st := c.tests[testKey(pkg, test)]
	if st == nil || len(st.tail) == 0 {
		return ""
	}
	return "\n" + strings.Join(st.tail, "\n")
}

func describe(pkg, test string) string {
	if test == "" {
		return "package " + pkg
	}
	return "test " + test + " in " + pkg
}

func (c *nativeChecker) apply(raw []byte, ev testEvent) error {
	if ev.Action == nil {
		return c.fail(raw, "event has no Action")
	}
	action := *ev.Action
	switch {
	case action == "":
		return c.fail(raw, "event has an empty Action")
	case !acceptedActions[action]:
		return c.fail(raw, "unknown Action %q", action)
	}
	switch action {
	case "build-output":
		return nil
	case "build-fail":
		return c.fail(raw, "build failed for %s", ev.ImportPath)
	case "fail":
		msg := describe(ev.Package, ev.Test) + " failed"
		if ev.FailedBuild != "" {
			msg += " (FailedBuild " + ev.FailedBuild + ")"
		}
		return fmt.Errorf("%w%s", c.fail(raw, "%s", msg), c.tailOf(ev.Package, ev.Test))
	}
	if ev.Package == "" {
		if action == "output" || action == "bench" {
			return nil
		}
		return c.fail(raw, "%s event names no Package", action)
	}
	p := c.pkgs[ev.Package]
	if action == "start" {
		if p != nil {
			return c.fail(raw, "package %s started twice", ev.Package)
		}
		c.pkgs[ev.Package] = &pkgState{}
		c.order = append(c.order, ev.Package)
		return nil
	}
	if p == nil {
		return c.fail(raw, "%s event for package %s before its start", action, ev.Package)
	}
	if p.done {
		return c.fail(raw, "%s event after package %s finished", action, ev.Package)
	}
	switch action {
	case "output", "bench":
		// Diagnostics only; a bench result never satisfies a required test.
		c.output(ev)
	case "run":
		if ev.Test == "" {
			return c.fail(raw, "run event names no Test")
		}
		c.test(ev.Package, ev.Test).ran = true
		p.ranTests = true
	case "pause", "cont":
		st := c.tests[testKey(ev.Package, ev.Test)]
		if ev.Test == "" || st == nil || !st.ran || st.done {
			return c.fail(raw, "%s event for a test that is not running", action)
		}
	case "pass":
		if ev.Test == "" {
			p.done, p.passed = true, true
			return nil
		}
		st := c.test(ev.Package, ev.Test)
		st.done, st.passed = true, true
	case "skip":
		if ev.Test != "" {
			return fmt.Errorf("%w%s", c.fail(raw, "%s skipped: %s", describe(ev.Package, ev.Test), unobserved), c.tailOf(ev.Package, ev.Test))
		}
		if ev.Package == NativePackage {
			return c.fail(raw, "required package %s skipped: %s", ev.Package, unobserved)
		}
		if p.ranTests {
			return c.fail(raw, "package %s skipped after running tests", ev.Package)
		}
		p.done = true
	}
	return nil
}

func (c *nativeChecker) finish() error {
	if c.n == 0 {
		return fmt.Errorf("devcheck: native: empty event stream: %s", unobserved)
	}
	for _, pkg := range c.order {
		if !c.pkgs[pkg].done {
			return fmt.Errorf("devcheck: native: package %s did not finish before the end of the stream", pkg)
		}
	}
	var missing []string
	if p := c.pkgs[NativePackage]; p == nil {
		missing = append(missing, "package "+NativePackage+" has no start event")
	} else if !p.passed {
		missing = append(missing, "package "+NativePackage+" has no pass event")
	}
	for _, name := range nativeRequired {
		st := c.tests[testKey(NativePackage, name)]
		switch {
		case st == nil || !st.ran:
			missing = append(missing, name+" has no run event")
		case !st.passed:
			missing = append(missing, name+" has no pass event")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("devcheck: native: required evidence missing (%s): %s", unobserved, strings.Join(missing, "; "))
	}
	return nil
}

// CheckNativeResults validates a go test -json stdout event stream from the
// NativeSteps plan without invoking any tool. Only goos darwin is accepted.
// It fails for malformed or truncated JSON, a missing, empty or unknown
// Action, any fail or build-fail, any test skip, a package skip other than
// a nonrequired no-test-files package, a started package that never
// finishes, and missing run/pass evidence for NativePackage and its
// required process-group tests.
func CheckNativeResults(goos string, r io.Reader) error {
	if goos != "darwin" {
		return unsupportedNative(goos)
	}
	c := &nativeChecker{pkgs: map[string]*pkgState{}, tests: map[string]*testState{}}
	dec := json.NewDecoder(r)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("devcheck: native: malformed event stream after %d events: %v", c.n, err)
		}
		c.n++
		var ev testEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return c.fail(raw, "malformed event: %v", err)
		}
		if err := c.apply(raw, ev); err != nil {
			return err
		}
	}
	return c.finish()
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

// native runs the native plan with separate stdout and stderr: stdout goes
// to the user and a scratch JSON file (the only parser input), stderr to a
// scratch log and the user's diagnostics. It stops at the first child
// failure and validates the JSON only after every child succeeded.
func (d *driver) native(steps []Step) error {
	jsonPath := filepath.Join(d.scratch, "native-events.jsonl")
	errPath := filepath.Join(d.scratch, "native-stderr.log")
	events, err := os.Create(jsonPath)
	if err != nil {
		return err
	}
	stderrLog, err := os.Create(errPath)
	if err != nil {
		events.Close()
		return err
	}
	runErr := d.nativeChildren(steps, events, stderrLog)
	closeErr := errors.Join(events.Close(), stderrLog.Close())
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}
	f, err := os.Open(jsonPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := CheckNativeResults(d.goos, f); err != nil {
		return fmt.Errorf("%w\ndevcheck: native events %s, stderr %s", err, jsonPath, errPath)
	}
	fmt.Fprintf(d.out, "devcheck: native qualification passed on %s/%s: %s: %s\n",
		d.goos, runtime.GOARCH, NativePackage, strings.Join(nativeRequired, ", "))
	return nil
}

func (d *driver) nativeChildren(steps []Step, events, stderrLog io.Writer) error {
	for _, s := range steps {
		fmt.Fprintf(d.out, "devcheck: %s: %s\n", s.Name, strings.Join(s.Argv, " "))
		tail := &tailBuffer{max: 2000}
		err := d.run(d.ctx, s.Argv, MergeEnv(os.Environ(), s.Env), "",
			io.MultiWriter(events, d.out), io.MultiWriter(stderrLog, d.errOut, tail))
		if err != nil {
			return &StepError{Step: s, Err: err, Stderr: string(tail.b)}
		}
	}
	return nil
}
