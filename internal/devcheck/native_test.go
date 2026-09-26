package devcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// evt is one go test -json event. Synthetic events carry a "Synthetic"
// field (an unknown field the parser must accept) so they can never be
// mistaken for recorded evidence.
type evt map[string]any

func ev(action, pkg, test string) evt {
	e := evt{"Synthetic": "UT-3 synthetic event"}
	if action != "" {
		e["Action"] = action
	}
	if pkg != "" {
		e["Package"] = pkg
	}
	if test != "" {
		e["Test"] = test
	}
	return e
}

func (e evt) with(k string, v any) evt { e[k] = v; return e }

func stream(evs ...evt) string {
	var b strings.Builder
	for _, e := range evs {
		j, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

const fp6 = "TestFP6ProcessGroups"

var scenarios = []string{"cooperative", "resistant", "leader-exits-first"}

// qualification is a complete synthetic tests/function stream for FP-6.
func qualification() []evt {
	evs := []evt{ev("start", NativePackage, ""), ev("run", NativePackage, fp6),
		ev("output", NativePackage, fp6).with("Output", "=== RUN   TestFP6ProcessGroups\n")}
	for _, s := range scenarios {
		evs = append(evs, ev("run", NativePackage, fp6+"/"+s))
	}
	for _, s := range scenarios {
		evs = append(evs, ev("pass", NativePackage, fp6+"/"+s))
	}
	return append(evs, ev("pass", NativePackage, fp6), ev("output", NativePackage, "").with("Output", "ok\n"), ev("pass", NativePackage, ""))
}

// without drops events matching action and test.
func without(evs []evt, action, test string) []evt {
	var out []evt
	for _, e := range evs {
		if e["Action"] == action && (e["Test"] == test || (test == "" && e["Test"] == nil)) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// replacing swaps the first event matching action/test for repl.
func replacing(evs []evt, action, test string, repl evt) []evt {
	out := append([]evt(nil), evs...)
	for i, e := range out {
		if e["Action"] == action && e["Test"] == test {
			out[i] = repl
			return out
		}
	}
	panic("no event to replace")
}

func check(s string) error { return CheckNativeResults("darwin", strings.NewReader(s)) }

func mustFail(t *testing.T, name, s string, wants ...string) {
	t.Helper()
	err := check(s)
	if err == nil {
		t.Fatalf("%s: accepted", name)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("%s: error %q lacks %q", name, err, w)
		}
	}
}

type countingReader struct{ reads int }

func (c *countingReader) Read([]byte) (int, error) { c.reads++; return 0, io.EOF }

func TestNativeStepsAndUnsupportedOS(t *testing.T) {
	steps, err := NativeSteps("darwin")
	if err != nil || len(steps) != 1 || steps[0].Name != "native" || len(steps[0].Env) != 0 ||
		strings.Join(steps[0].Argv, " ") != "go test -json -count=1 -timeout=180s ./..." {
		t.Fatalf("darwin plan = %+v %v", steps, err)
	}
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		if steps, err := NativeSteps(goos); err == nil || steps != nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported on %q", goos)) {
			t.Fatalf("NativeSteps(%q) = %v %v", goos, steps, err)
		}
		r := &countingReader{}
		if err := CheckNativeResults(goos, r); err == nil || !strings.Contains(err.Error(), "darwin only") || r.reads != 0 {
			t.Fatalf("CheckNativeResults(%q) = %v after %d reads", goos, err, r.reads)
		}
	}
	req := NativeRequiredTests()
	if strings.Join(req, ",") != "TestFP6ProcessGroups,TestFP6ProcessGroups/cooperative,TestFP6ProcessGroups/resistant,TestFP6ProcessGroups/leader-exits-first" {
		t.Fatalf("required = %v", req)
	}
	req[0] = "mutated"
	if NativeRequiredTests()[0] != fp6 {
		t.Fatal("NativeRequiredTests exposes internal state")
	}
}

// TestNativeHostFixture replays the verbatim host capture (see
// testdata/cli-go-test.txt) through the production decoder.
func TestNativeHostFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "cli-go-test.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"Package":"github.com/wedevwork/callsheet/internal/cli"`)) || bytes.Contains(raw, []byte("Synthetic")) {
		t.Fatal("fixture is not the recorded internal/cli capture")
	}
	err = CheckNativeResults("darwin", bytes.NewReader(raw))
	if err == nil {
		t.Fatal("a cli-only stream qualified darwin")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "devcheck: native: required evidence missing ("+unobserved+")") ||
		!strings.Contains(msg, "package "+NativePackage+" has no start event") || !strings.Contains(msg, "TestFP6ProcessGroups/leader-exits-first has no run event") ||
		strings.Contains(msg, "Action") || strings.Contains(msg, "malformed") || strings.Contains(msg, "internal/cli") {
		t.Fatalf("fixture must fail only for missing qualification, got: %v", err)
	}
	full := string(raw) + stream(qualification()...)
	if err := check(full); err != nil {
		t.Fatalf("fixture plus synthetic qualification: %v", err)
	}
	// Synthetic qualification interleaved into the real segment (which stays
	// unmodified and in order) is also valid.
	lines := strings.SplitAfter(string(raw), "\n")
	q := strings.SplitAfter(stream(qualification()...), "\n")
	var mixed strings.Builder
	for i := 0; i < len(lines) || i < len(q); i++ {
		if i < len(lines) {
			mixed.WriteString(lines[i])
		}
		if i < len(q) {
			mixed.WriteString(q[i])
		}
	}
	if err := check(mixed.String()); err != nil {
		t.Fatalf("interleaved: %v", err)
	}
}

func TestNativeAcceptedActions(t *testing.T) {
	other := "example.com/other"
	nt := "example.com/notests"
	evs := []evt{
		{"ImportPath": other + " [" + other + ".test]", "Action": "build-output", "Output": "# note\n"},
		ev("start", other, ""), ev("start", nt, ""),
		ev("output", nt, "").with("Output", "?   \texample.com/notests\t[no test files]\n"),
		ev("skip", nt, ""),
		ev("run", other, "TestA"), ev("pause", other, "TestA"), ev("cont", other, "TestA"),
		ev("output", other, "TestA").with("Output", "=== RUN TestA\n"),
		ev("run", other, "BenchmarkX"), ev("bench", other, "BenchmarkX").with("Output", "BenchmarkX 3 1 ns/op\n"),
		ev("pass", other, "BenchmarkX"), ev("pass", other, "TestA"),
		ev("bench", "", "").with("Output", "stray\n"), ev("output", "", "").with("Output", "stray\n"),
	}
	evs = append(evs, qualification()...)
	evs = append(evs, ev("pass", other, "").with("Elapsed", 1.5).with("OutputType", "future-field"))
	if err := check(stream(evs...)); err != nil {
		t.Fatalf("all accepted actions: %v", err)
	}
}

func TestNativeActionValidation(t *testing.T) {
	q := qualification()
	for name, bad := range map[string]evt{
		"unknown":  ev("attr", NativePackage, ""),
		"empty":    {"Action": "", "Package": NativePackage},
		"missing":  {"Package": NativePackage, "Output": "x"},
		"non-text": {"Action": 7},
	} {
		s := stream(q[0]) + stream(bad) + stream(q[1:]...)
		j, _ := json.Marshal(bad)
		mustFail(t, name, s, "event 2", string(j))
	}
	mustFail(t, "unknown action text", stream(append([]evt{ev("attr", NativePackage, "")}, q...)...), `unknown Action "attr"`)
	mustFail(t, "empty action text", stream(evt{"Action": ""}), "empty Action")
	mustFail(t, "missing action text", stream(evt{"Package": "p"}), "no Action")
	mustFail(t, "null event", "null\n", "no Action")
	mustFail(t, "non-object", "42\n", "malformed event")
}

func TestNativeFailures(t *testing.T) {
	q := qualification()
	// fail and build-fail always fail; FailedBuild is preserved.
	fb := evt{"Action": "fail", "Package": NativePackage, "FailedBuild": NativePackage + " [" + NativePackage + ".test]"}
	mustFail(t, "fail with FailedBuild", stream(q[0], fb), "FailedBuild "+NativePackage+" ["+NativePackage+".test]", "package "+NativePackage+" failed")
	mustFail(t, "build-fail", stream(append([]evt{{"ImportPath": "x [x.test]", "Action": "build-fail"}}, q...)...), "build failed for x [x.test]")
	mustFail(t, "nonrequired test fail", stream(append([]evt{ev("start", "p", ""), ev("run", "p", "TestX"),
		ev("output", "p", "TestX").with("Output", "denied: operation not permitted\n"), ev("fail", "p", "TestX")}, q...)...),
		"test TestX in p failed", "denied: operation not permitted")
	for _, s := range scenarios {
		name := fp6 + "/" + s
		mustFail(t, "missing run "+s, stream(without(q, "run", name)...), name+" has no run event", unobserved)
		mustFail(t, "missing pass "+s, stream(without(q, "pass", name)...), name+" has no pass event", unobserved)
		mustFail(t, "deleted "+s, stream(without(without(q, "run", name), "pass", name)...), name+" has no run event")
		skip := ev("skip", NativePackage, name)
		mustFail(t, "skipped "+s, stream(replacing(q, "pass", name, skip)...), "test "+name+" in "+NativePackage+" skipped: "+unobserved)
		mustFail(t, "failed "+s, stream(replacing(q, "pass", name, ev("fail", NativePackage, name))...), "test "+name+" in "+NativePackage+" failed")
	}
	mustFail(t, "parent skipped", stream(append(q[:3:3], ev("output", NativePackage, fp6).with("Output", "skipping: no facility\n"), ev("skip", NativePackage, fp6))...),
		fp6+" in "+NativePackage+" skipped", "skipping: no facility")
	mustFail(t, "parent missing", stream(without(without(q, "run", fp6), "pass", fp6)...), fp6+" has no run event")
	mustFail(t, "package pass missing", stream(without(q, "pass", "")...), "did not finish")
	mustFail(t, "required package skipped", stream(ev("start", NativePackage, ""), ev("skip", NativePackage, "")), "required package "+NativePackage+" skipped")
	mustFail(t, "zero tests", stream(ev("start", NativePackage, ""), ev("pass", NativePackage, "")), fp6+" has no run event")
	var wrong []evt
	for _, e := range q {
		c := evt{}
		for k, v := range e {
			c[k] = v
		}
		c["Package"] = "github.com/wedevwork/callsheet/tests/other"
		wrong = append(wrong, c)
	}
	mustFail(t, "wrong package", stream(wrong...), "package "+NativePackage+" has no start event")
	// A pass without its run cannot satisfy a required test.
	mustFail(t, "pass without run", stream(without(q, "run", fp6+"/resistant")...), fp6+"/resistant has no run event")
	full := stream(q...)
	mustFail(t, "truncated", full[:len(full)-9], "malformed event stream after")
	mustFail(t, "garbage", full+"{not json\n", "malformed event stream after "+fmt.Sprint(len(q)))
	mustFail(t, "empty", "", "empty event stream: "+unobserved)
	mustFail(t, "whitespace only", "\n \n", "empty event stream")
	mustFail(t, "unfinished other package", stream(append([]evt{ev("start", "p", "")}, q...)...), "package p did not finish")
	mustFail(t, "skip after tests", stream(append([]evt{ev("start", "p", ""), ev("run", "p", "TestX"), ev("pass", "p", "TestX"), ev("skip", "p", "")}, q...)...), "package p skipped after running tests")
	mustFail(t, "event before start", stream(append([]evt{ev("output", "p", "").with("Output", "x\n")}, q...)...), "output event for package p before its start")
	mustFail(t, "event after finish", stream(append(q, ev("output", NativePackage, "").with("Output", "late\n"))...), "after package "+NativePackage+" finished")
	mustFail(t, "double start", stream(append([]evt{q[0]}, q...)...), "started twice")
	mustFail(t, "run without test", stream(q[0], ev("run", NativePackage, "")), "run event names no Test")
	mustFail(t, "run without package", stream(ev("run", "", "TestX")), "run event names no Package")
	mustFail(t, "pause not running", stream(q[0], ev("pause", NativePackage, "TestX")), "pause event for a test that is not running")
	mustFail(t, "cont after done", stream(q[0], ev("run", NativePackage, "TestX"), ev("pass", NativePackage, "TestX"), ev("cont", NativePackage, "TestX")), "cont event for a test that is not running")
	mustFail(t, "pause without test", stream(q[0], ev("pause", NativePackage, "")), "not running")
}

func TestNativeDiagnosticsBounded(t *testing.T) {
	q := qualification()
	evs := []evt{q[0], ev("run", NativePackage, "TestNoisy")}
	for i := 0; i < 50; i++ {
		evs = append(evs, ev("output", NativePackage, "TestNoisy").with("Output", fmt.Sprintf("line %02d %s\n", i, strings.Repeat("y", 400))))
	}
	evs = append(evs, ev("fail", NativePackage, "TestNoisy").with("Output", strings.Repeat("z", 2000)))
	err := check(stream(evs...))
	if err == nil {
		t.Fatal("accepted")
	}
	msg := err.Error()
	if strings.Contains(msg, "line 29 ") || !strings.Contains(msg, "line 30 ") || !strings.Contains(msg, "line 49 ") {
		t.Fatalf("tail window wrong: %s", msg)
	}
	if strings.Count(msg, "y...") != 20 || len(msg) > 20*(maxTailLine+20)+maxQuote+300 {
		t.Fatalf("diagnostics unbounded (%d bytes)", len(msg))
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 5}
	for _, s := range []string{"abc", "defg", "h"} {
		if n, err := tb.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatal(n, err)
		}
	}
	if string(tb.b) != "defgh" {
		t.Fatalf("tail = %q", tb.b)
	}
}

// --- driver (UT-2 stage dispatch, UT-4 native execution) ---

func TestStagesMatchDispatch(t *testing.T) {
	want := "test coverage bench cross all native stress"
	got := Stages()
	if strings.Join(got, " ") != want {
		t.Fatalf("Stages = %v", got)
	}
	got[0] = "bogus"
	if Stages()[0] != "test" {
		t.Fatal("Stages exposes dispatch state")
	}
	if code, _, errOut := runDriver(t, "linux", &fakeRunner{}, "bogus"); code != 2 || !strings.Contains(errOut, `unknown subcommand "bogus"`) {
		t.Fatalf("mutated name dispatched: %d %s", code, errOut)
	}
	for _, goos := range []string{"linux", "darwin"} {
		for _, st := range Stages() {
			f := &fakeRunner{coverTotal: "90%", cmdList: cmdList, profile: goodProfile, native: stream(qualification()...)}
			code, out, errOut := runDriver(t, goos, f, st)
			if code == 2 || strings.Contains(errOut, "unknown subcommand") {
				t.Fatalf("%s %s not recognized: %s", goos, st, errOut)
			}
			wantCode := 0
			if st == "native" && goos != "darwin" {
				wantCode = 1
			}
			if code != wantCode || (code == 0 && !strings.Contains(out, "stage "+st+" ok") && st != "all") {
				t.Fatalf("%s %s = %d\n%s\n%s", goos, st, code, out, errOut)
			}
			if code == 0 && len(f.calls) == 0 {
				t.Fatalf("%s %s succeeded without running anything", goos, st)
			}
			os.RemoveAll(scratchFrom(out))
		}
	}
	if code, _, errOut := runDriver(t, "linux", &fakeRunner{}, "natives"); code != 2 || !strings.Contains(errOut, "usage: devcheck test | coverage [-o profile] | bench | cross | all | native | stress") {
		t.Fatalf("unknown stage = %d %s", code, errOut)
	}
}

// A stage name added to the definition without an implementation cannot
// pass dispatch.
func TestAdvertisedStageMustBeImplemented(t *testing.T) {
	saved := stageNames
	defer func() { stageNames = saved }()
	stageNames[len(stageNames)-1] = "phantom"
	code, out, errOut := runDriver(t, "darwin", &fakeRunner{}, "phantom")
	if code != 1 || !strings.Contains(errOut, `stage "phantom" is advertised but not implemented`) {
		t.Fatalf("phantom = %d %s", code, errOut)
	}
	os.RemoveAll(scratchFrom(out))
}

func TestNativeStageRunFor(t *testing.T) {
	f := &fakeRunner{native: stream(qualification()...), nativeErr: "go: downloading nothing\n"}
	code, out, errOut := runDriver(t, "darwin", f, "native")
	if code != 0 {
		t.Fatalf("native = %d %s", code, errOut)
	}
	if len(f.calls) != 1 || strings.Join(f.calls[0].argv, " ") != "go test -json -count=1 -timeout=180s ./..." || f.calls[0].dir != "" {
		t.Fatalf("calls = %+v", f.calls)
	}
	if !strings.Contains(strings.Join(f.calls[0].env, "\n"), "PATH=") {
		t.Fatal("child env must extend the parent environment")
	}
	if !strings.Contains(out, "devcheck: native qualification passed on darwin/") || !strings.Contains(out, "TestFP6ProcessGroups/leader-exits-first") ||
		!strings.Contains(out, `"Action":"pass"`) || !strings.Contains(out, "stage native ok") {
		t.Fatalf("out = %s", out)
	}
	if strings.Contains(out, "go: downloading") || !strings.Contains(errOut, "go: downloading nothing") {
		t.Fatalf("stderr not separated: out=%q err=%q", out, errOut)
	}
	if _, err := os.Stat(scratchFrom(out)); !os.IsNotExist(err) {
		t.Fatal("successful scratch not deleted")
	}
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		f := &fakeRunner{native: stream(qualification()...)}
		code, out, errOut := runDriver(t, goos, f, "native")
		if code != 1 || len(f.calls) != 0 || !strings.Contains(errOut, "stage native FAILED") || !strings.Contains(errOut, "darwin only") {
			t.Fatalf("%q native = %d calls=%d %s", goos, code, len(f.calls), errOut)
		}
		os.RemoveAll(scratchFrom(out))
	}
	for _, args := range [][]string{{"native", "-o", "x"}, {"native", "extra"}} {
		if code, _, errOut := runDriver(t, "darwin", &fakeRunner{}, args...); code != 2 || !strings.Contains(errOut, "invalid arguments for native") {
			t.Fatalf("%v = %d %s", args, code, errOut)
		}
	}
}

func TestNativeStageFailuresRetainScratch(t *testing.T) {
	// Child failure: StepError names the command and keeps stderr.
	f := &fakeRunner{fail: "-json"}
	code, out, errOut := runDriver(t, "darwin", f, "native")
	scratch := scratchFrom(out)
	if code != 1 || !strings.Contains(errOut, "native failed: go test -json -count=1 -timeout=180s ./...: exit status 1") ||
		!strings.Contains(errOut, "boom from child") || !strings.Contains(errOut, "logs retained in "+scratch) {
		t.Fatalf("child failure = %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(scratch, "native-events.jsonl")); err != nil {
		t.Fatal("event file not retained")
	}
	os.RemoveAll(scratch)
	// Successful exit with missing evidence fails validation.
	f = &fakeRunner{native: stream(without(qualification(), "run", fp6+"/resistant")...)}
	code, out, errOut = runDriver(t, "darwin", f, "native")
	scratch = scratchFrom(out)
	if code != 1 || !strings.Contains(errOut, unobserved) || !strings.Contains(errOut, filepath.Join(scratch, "native-events.jsonl")) ||
		strings.Contains(out, "stage native ok") {
		t.Fatalf("parser failure = %d %s", code, errOut)
	}
	os.RemoveAll(scratch)
	// Cancellation follows the Runner error path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var o, e bytes.Buffer
	cancelRunner := func(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
		return ctx.Err()
	}
	if code := runFor(ctx, "darwin", []string{"native"}, &o, &e, cancelRunner); code != 1 || !strings.Contains(e.String(), "context canceled") {
		t.Fatalf("canceled = %d %s", code, e.String())
	}
	os.RemoveAll(scratchFrom(o.String()))
}

func newDriver(t *testing.T, run Runner) (*driver, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &driver{ctx: context.Background(), run: run, out: &out, errOut: &errOut, scratch: t.TempDir(), goos: "darwin"}, &out, &errOut
}

func TestNativeDriverStreams(t *testing.T) {
	// stderr carrying a JSON failure event must never reach the parser.
	bogus := `{"Action":"fail","Package":"x"}` + "\n"
	run := func(_ context.Context, _, _ []string, _ string, stdout, stderr io.Writer) error {
		io.WriteString(stdout, stream(qualification()...))
		io.WriteString(stderr, bogus)
		return nil
	}
	d, out, errOut := newDriver(t, run)
	steps, _ := NativeSteps("darwin")
	if err := d.native(steps); err != nil {
		t.Fatalf("native: %v", err)
	}
	events, _ := os.ReadFile(filepath.Join(d.scratch, "native-events.jsonl"))
	stderrLog, _ := os.ReadFile(filepath.Join(d.scratch, "native-stderr.log"))
	if string(events) != stream(qualification()...) || string(stderrLog) != bogus {
		t.Fatalf("events=%q stderr=%q", events, stderrLog)
	}
	if errOut.String() != bogus || strings.Contains(out.String(), bogus) {
		t.Fatalf("user streams: out=%q err=%q", out, errOut)
	}
	// Fail fast: a later child never runs after a failure, even if an
	// earlier child produced a complete successful stream.
	var calls int
	run = func(_ context.Context, _, _ []string, _ string, stdout, _ io.Writer) error {
		calls++
		if calls == 1 {
			io.WriteString(stdout, stream(qualification()...))
			return nil
		}
		return errors.New("exit status 2")
	}
	d, _, _ = newDriver(t, run)
	two := []Step{{Name: "one", Argv: []string{"a"}}, {Name: "two", Argv: []string{"b"}}, {Name: "three", Argv: []string{"c"}}}
	var se *StepError
	if err := d.native(two); !errors.As(err, &se) || se.Step.Name != "two" || calls != 2 {
		t.Fatalf("fail fast: %v after %d calls", err, calls)
	}
}

func TestNativeDriverFileFailures(t *testing.T) {
	ok := func(_ context.Context, _, _ []string, _ string, stdout, _ io.Writer) error {
		io.WriteString(stdout, stream(qualification()...))
		return nil
	}
	steps, _ := NativeSteps("darwin")
	// Scratch missing: the event file cannot be created.
	d, _, _ := newDriver(t, ok)
	d.scratch = filepath.Join(d.scratch, "missing")
	if err := d.native(steps); err == nil || !strings.Contains(err.Error(), "native-events.jsonl") {
		t.Fatalf("create events: %v", err)
	}
	// The stderr log path is occupied by a directory.
	d, _, _ = newDriver(t, ok)
	os.Mkdir(filepath.Join(d.scratch, "native-stderr.log"), 0o700)
	if err := d.native(steps); err == nil || !strings.Contains(err.Error(), "native-stderr.log") {
		t.Fatalf("create stderr log: %v", err)
	}
	// The event file vanishes before it is reopened for validation.
	d, _, _ = newDriver(t, ok)
	d.run = func(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
		ok(ctx, argv, env, dir, stdout, stderr)
		os.Remove(filepath.Join(d.scratch, "native-events.jsonl"))
		return nil
	}
	if err := d.native(steps); err == nil || !os.IsNotExist(err) {
		t.Fatalf("reopen: %v", err)
	}
}

// Other stages keep the merged stdout/stderr execution path.
func TestOldStagesStillMergeStderr(t *testing.T) {
	run := func(_ context.Context, _, _ []string, _ string, stdout, stderr io.Writer) error {
		io.WriteString(stderr, "from-stderr\n")
		return nil
	}
	var out, errOut bytes.Buffer
	if code := runFor(context.Background(), "darwin", []string{"test"}, &out, &errOut, run); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "from-stderr") || strings.Contains(errOut.String(), "from-stderr") {
		t.Fatalf("test stage streams changed: out=%q err=%q", out.String(), errOut.String())
	}
}

// S1: an unsupported native stage is rejected before any scratch directory
// exists, so nothing is created and no retained-logs line is printed.
func TestUnsupportedNativeCreatesNoScratch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		f := &fakeRunner{}
		code, out, errOut := runDriver(t, goos, f, "native")
		if code != 1 || len(f.calls) != 0 || !strings.Contains(errOut, "stage native FAILED") || !strings.Contains(errOut, "darwin only") {
			t.Fatalf("%q native = %d calls=%d %s", goos, code, len(f.calls), errOut)
		}
		if strings.Contains(errOut, "logs retained") || strings.Contains(out, "devcheck: scratch") {
			t.Fatalf("%q native claims a scratch dir: out=%q err=%q", goos, out, errOut)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Fatalf("%q native created %v", goos, entries)
		}
	}
}
