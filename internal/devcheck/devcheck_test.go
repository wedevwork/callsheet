package devcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorded struct {
	argv []string
	env  []string
	dir  string
}

// fakeRunner records calls and scripts results: fail matches a substring of
// the joined argv; coverTotal feeds "go tool cover -func" output.
type fakeRunner struct {
	calls      []recorded
	fail       string
	coverTotal string
	cmdList    string
	profile    string
}

func (f *fakeRunner) run(_ context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, recorded{argv, env, dir})
	joined := strings.Join(argv, " ")
	if f.fail != "" && strings.Contains(joined, f.fail) {
		fmt.Fprint(stderr, "boom from child")
		return errors.New("exit status 1")
	}
	for _, a := range argv {
		if p, ok := strings.CutPrefix(a, "-coverprofile="); ok {
			os.WriteFile(p, []byte(f.profile), 0o600)
		}
	}
	switch {
	case strings.HasPrefix(joined, "go tool cover"):
		fmt.Fprintf(stdout, "github.com/wedevwork/callsheet/internal/cli/cli.go:10:\tRun\t100.0%%\ntotal:\t\t\t(statements)\t%s\n", f.coverTotal)
	case strings.HasPrefix(joined, "go list"):
		io.WriteString(stdout, f.cmdList)
	}
	return nil
}

func (f *fakeRunner) argvs() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c.argv, " "))
	}
	return out
}

const goodProfile = "mode: atomic\n" +
	"github.com/wedevwork/callsheet/cmd/callsheet/main.go:13.13,15.2 1 1\n" +
	"github.com/wedevwork/callsheet/cmd/devcheck/main.go:13.13,15.2 1 1\n" +
	"github.com/wedevwork/callsheet/cmd/fake-adapter/main.go:13.13,15.2 1 1\n"

const cmdList = "github.com/wedevwork/callsheet/cmd/callsheet|1\ngithub.com/wedevwork/callsheet/cmd/devcheck|1\ngithub.com/wedevwork/callsheet/cmd/fake-adapter|1\n"

func TestMatrixAndValidation(t *testing.T) {
	if len(Matrix) != 6 {
		t.Fatalf("matrix = %v", Matrix)
	}
	want := "linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"
	var got []string
	for _, m := range Matrix {
		got = append(got, m.String())
		if err := ValidateTarget(m); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, " ") != want {
		t.Fatalf("matrix = %v", got)
	}
	for _, bad := range []Target{{"windows", "386"}, {"plan9", "amd64"}, {"linux", "riscv64"}, {"", ""}} {
		if ValidateTarget(bad) == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	if _, err := CrossPlan("/o", []Target{{"freebsd", "amd64"}}); err == nil {
		t.Fatal("unsupported pair planned")
	}
	if _, err := CrossPlan("/o", nil); err == nil {
		t.Fatal("empty target list planned")
	}
}

func TestCrossPlanArgvAndArtifacts(t *testing.T) {
	steps, err := CrossPlan("/out", Matrix)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 6+4+4 {
		t.Fatalf("steps = %d", len(steps))
	}
	outputs := map[string]bool{}
	for _, s := range steps {
		if s.Argv[0] != "go" {
			t.Fatalf("argv = %v", s.Argv)
		}
		for i, a := range s.Argv {
			if a == "-o" {
				outputs[s.Argv[i+1]] = true
			}
		}
		env := strings.Join(s.Env, " ")
		if !strings.Contains(env, "CGO_ENABLED=0") || !strings.Contains(env, "GOOS=") || !strings.Contains(env, "GOARCH=") {
			t.Fatalf("env = %v", s.Env)
		}
	}
	for _, name := range []string{
		"callsheet-linux-amd64", "callsheet-linux-arm64", "callsheet-darwin-amd64", "callsheet-darwin-arm64",
		"callsheet-windows-amd64.exe", "callsheet-windows-arm64.exe",
		"fake-adapter-linux-amd64", "fake-adapter-linux-arm64", "fake-adapter-darwin-amd64", "fake-adapter-darwin-arm64",
		"processgroup-linux-amd64.test", "processgroup-linux-arm64.test", "processgroup-darwin-amd64.test", "processgroup-darwin-arm64.test",
	} {
		if !outputs[filepath.Join("/out", name)] {
			t.Fatalf("missing artifact %s in %v", name, outputs)
		}
	}
	first := strings.Join(steps[0].Argv, " ")
	if first != "go build -o "+filepath.Join("/out", "callsheet-linux-amd64")+" ./cmd/callsheet" {
		t.Fatalf("first step = %s", first)
	}
	if got := strings.Join(steps[2].Argv, " "); got != "go test -c -o "+filepath.Join("/out", "processgroup-linux-amd64.test")+" ./internal/spikes/processgroup" {
		t.Fatalf("process test step = %s", got)
	}
}

func TestCrossRunsInCallerDirAndPropagatesFailure(t *testing.T) {
	f := &fakeRunner{}
	if err := Cross(context.Background(), f.run, "/out", Matrix); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 14 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	for _, c := range f.calls {
		if c.dir != "" {
			t.Fatalf("Cross must use the caller's cwd, got dir %q", c.dir)
		}
		env := strings.Join(c.env, "\n")
		if !strings.Contains(env, "\nCGO_ENABLED=0\n") && !strings.Contains(env, "CGO_ENABLED=0\n") {
			t.Fatal("CGO_ENABLED=0 missing from child env")
		}
		if strings.Count(env, "GOOS=") != 1 {
			t.Fatal("GOOS must appear exactly once")
		}
	}
	f = &fakeRunner{fail: "fake-adapter-darwin-amd64"}
	err := Cross(context.Background(), f.run, "/out", Matrix)
	var se *StepError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "fake-adapter-darwin-amd64") || !strings.Contains(err.Error(), "boom from child") {
		t.Fatalf("err = %v", err)
	}
	if last := f.argvs()[len(f.calls)-1]; !strings.Contains(last, "fake-adapter-darwin-amd64") {
		t.Fatalf("did not stop at the failure: %s", last)
	}
	if errors.Unwrap(se) == nil {
		t.Fatal("unwrap")
	}
	if err := Cross(context.Background(), f.run, "/out", []Target{{"js", "wasm"}}); err == nil {
		t.Fatal("invalid target built")
	}
}

func TestMergeEnv(t *testing.T) {
	got := MergeEnv([]string{"A=1", "GOOS=plan9", "B=2", "noequals"}, []string{"GOOS=linux", "C=3"})
	if strings.Join(got, ",") != "A=1,B=2,noequals,GOOS=linux,C=3" {
		t.Fatalf("env = %v", got)
	}
}

func TestParseCoverTotalAndThreshold(t *testing.T) {
	for in, want := range map[string]float64{
		"a 1%\ntotal:\t(statements)\t83.4%\n":            83.4,
		"x 1%\ntotal:\t\t\t(statements)\t80.0%":          80.0,
		"total:\t(statements)\t100.0%\ntrailing garbage": 100.0,
	} {
		got, err := ParseCoverTotal(in)
		if err != nil || got != want {
			t.Fatalf("%q = %v %v", in, got, err)
		}
	}
	if _, err := ParseCoverTotal("no total here"); err == nil {
		t.Fatal("missing total accepted")
	}
	if _, err := ParseCoverTotal("total: (statements) abc%"); err == nil {
		t.Fatal("bad number accepted")
	}
}

func TestMissingCmdPackages(t *testing.T) {
	if m := MissingCmdPackages(cmdList, goodProfile); len(m) != 0 {
		t.Fatalf("missing = %v", m)
	}
	profile := "mode: atomic\ngithub.com/wedevwork/callsheet/cmd/callsheet/main.go:1.1,2.2 1 1\n"
	m := MissingCmdPackages(cmdList+"github.com/wedevwork/callsheet/cmd/empty|0\nbad line\n", profile)
	if strings.Join(m, ",") != "github.com/wedevwork/callsheet/cmd/devcheck,github.com/wedevwork/callsheet/cmd/fake-adapter" {
		t.Fatalf("missing = %v", m)
	}
	if m := MissingCmdPackages("github.com/x/cmd/a|1", "github.com/x/cmd/a/main.go:1.1,2.2 1 1"); len(m) != 0 {
		t.Fatalf("profile without mode line: %v", m)
	}
}

type runnerLike interface {
	run(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error
}

func runDriver(t *testing.T, goos string, f runnerLike, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runFor(context.Background(), goos, args, &out, &errOut, f.run)
	return code, out.String(), errOut.String()
}

func scratchFrom(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "devcheck: scratch "); ok {
			return p
		}
	}
	return ""
}

func TestStagePlanning(t *testing.T) {
	f := &fakeRunner{}
	code, out, _ := runDriver(t, "linux", f, "test")
	if code != 0 {
		t.Fatal(code)
	}
	if got := strings.Join(f.argvs(), "|"); got != "go test -count=1 -timeout=180s ./...|go test -race -count=1 -timeout=180s ./..." {
		t.Fatalf("linux test = %s", got)
	}
	if !strings.Contains(strings.Join(f.calls[1].env, " "), "CGO_ENABLED=1") {
		t.Fatal("race run needs CGO_ENABLED=1")
	}
	if _, err := os.Stat(scratchFrom(out)); !os.IsNotExist(err) {
		t.Fatal("successful scratch not deleted")
	}
	f = &fakeRunner{}
	runDriver(t, "darwin", f, "test")
	if len(f.calls) != 1 {
		t.Fatalf("darwin test = %v", f.argvs())
	}
	f = &fakeRunner{}
	runDriver(t, "linux", f, "bench")
	if got := f.argvs()[0]; got != "go test ./internal/spikes/gittransport -run ^$ -bench . -benchmem -benchtime=3x -count=1 -timeout=180s" {
		t.Fatalf("bench = %s", got)
	}
	f = &fakeRunner{}
	code, _, errOut := runDriver(t, "linux", f, "cross")
	if code != 0 || len(f.calls) != 14 {
		t.Fatalf("cross = %d %v %s", code, len(f.calls), errOut)
	}
}

func TestCoverageStage(t *testing.T) {
	f := &fakeRunner{coverTotal: "85.2%", cmdList: cmdList, profile: goodProfile}
	outPath := filepath.Join(t.TempDir(), "cov.out")
	code, out, errOut := runDriver(t, "linux", f, "coverage", "-o", outPath)
	if code != 0 {
		t.Fatalf("coverage = %d %s", code, errOut)
	}
	want := "go test -count=1 -covermode=atomic -coverpkg=./internal/...,./cmd/... -coverprofile=" + outPath + " ./internal/... ./cmd/..."
	if f.argvs()[0] != want || f.argvs()[1] != "go tool cover -func="+outPath || !strings.HasPrefix(f.argvs()[2], "go list -f") {
		t.Fatalf("coverage plan = %v", f.argvs())
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatal("requested coverage output must be kept")
	}
	if !strings.Contains(out, "coverage total 85.2%") {
		t.Fatalf("out = %s", out)
	}
	// Exactly 80.0% is rejected.
	f = &fakeRunner{coverTotal: "80.0%", cmdList: cmdList, profile: goodProfile}
	code, out, errOut = runDriver(t, "linux", f, "coverage")
	if code != 1 || !strings.Contains(errOut, "not greater than 80.0%") {
		t.Fatalf("80.0%% = %d %s", code, errOut)
	}
	if _, err := os.Stat(scratchFrom(out)); err != nil {
		t.Fatal("failure logs must be retained")
	}
	os.RemoveAll(scratchFrom(out))
	// A cmd package absent from the profile fails.
	f = &fakeRunner{coverTotal: "90.0%", cmdList: cmdList, profile: "mode: atomic\n"}
	code, out, errOut = runDriver(t, "linux", f, "coverage")
	if code != 1 || !strings.Contains(errOut, "absent from coverage profile") || !strings.Contains(errOut, "cmd/callsheet") {
		t.Fatalf("missing cmd = %d %s", code, errOut)
	}
	os.RemoveAll(scratchFrom(out))
	// Unparseable total and failing child tools.
	for _, fr := range []*fakeRunner{
		{coverTotal: "n/a", cmdList: cmdList, profile: goodProfile},
		{fail: "go tool cover", profile: goodProfile},
		{fail: "go list", coverTotal: "90%", profile: goodProfile},
		{fail: "-coverprofile"},
	} {
		code, out, _ := runDriver(t, "linux", fr, "coverage")
		if code != 1 {
			t.Fatalf("expected failure for %+v", fr)
		}
		os.RemoveAll(scratchFrom(out))
	}
	// Profile missing after a "successful" run.
	fr := &fakeRunner{coverTotal: "90%", cmdList: cmdList}
	code, out, _ = runDriver(t, "linux", &noProfileRunner{fr}, "coverage")
	if code != 1 {
		t.Fatal("missing profile accepted")
	}
	os.RemoveAll(scratchFrom(out))
}

type noProfileRunner struct{ *fakeRunner }

func (n *noProfileRunner) run(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
	var clean []string
	for _, a := range argv {
		if strings.HasPrefix(a, "-coverprofile=") {
			a = "-coverprofile="
		}
		clean = append(clean, a)
	}
	return n.fakeRunner.run(ctx, clean, env, dir, stdout, stderr)
}

func TestAllStopsAtFirstFailure(t *testing.T) {
	f := &fakeRunner{coverTotal: "81%", cmdList: cmdList, profile: goodProfile}
	code, _, errOut := runDriver(t, "linux", f, "all")
	if code != 0 {
		t.Fatalf("all = %d %s", code, errOut)
	}
	// test(2) + coverage(3) + bench(1) + cross(14), in that order.
	a := f.argvs()
	if len(a) != 20 || !strings.Contains(a[0], "go test -count=1") || !strings.Contains(a[2], "-coverprofile") || !strings.Contains(a[5], "-bench") || !strings.Contains(a[6], "go build") {
		t.Fatalf("all order = %v", a)
	}
	f = &fakeRunner{fail: "-bench", coverTotal: "81%", cmdList: cmdList, profile: goodProfile}
	code, out, errOut := runDriver(t, "linux", f, "all")
	if code != 1 || !strings.Contains(errOut, "stage bench FAILED") || !strings.Contains(errOut, "go test ./internal/spikes/gittransport") {
		t.Fatalf("bench failure = %d %s", code, errOut)
	}
	for _, c := range f.argvs() {
		if strings.Contains(c, "go build") {
			t.Fatal("cross ran after a failed stage")
		}
	}
	scratch := scratchFrom(out)
	logs, _ := filepath.Glob(filepath.Join(scratch, "*.log"))
	if len(logs) == 0 {
		t.Fatal("failure logs not retained")
	}
	os.RemoveAll(scratch)
	f = &fakeRunner{fail: "-race"}
	code, out, _ = runDriver(t, "linux", f, "all")
	if code != 1 || len(f.calls) != 2 {
		t.Fatalf("race failure: %d %v", code, f.argvs())
	}
	os.RemoveAll(scratchFrom(out))
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"test", "extra"}, {"bench", "-o", "x"}, {"coverage", "-zzz"}} {
		code, _, errOut := runDriver(t, "linux", &fakeRunner{}, args...)
		if code != 2 || !strings.Contains(errOut, "usage") {
			t.Fatalf("%v = %d %q", args, code, errOut)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"nope"}, &out, &errOut, (&fakeRunner{}).run); code != 2 {
		t.Fatal("Run usage")
	}
}

func TestExecRunner(t *testing.T) {
	if err := ExecRunner(context.Background(), nil, nil, "", io.Discard, io.Discard); err == nil {
		t.Fatal("empty argv")
	}
	var out bytes.Buffer
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Run this test binary listing its tests: proves argv/dir/env plumbing
	// without a shell and without invoking devcheck recursively.
	if err := ExecRunner(context.Background(), []string{exe, "-test.list", "^TestExecRunner$"}, os.Environ(), dir, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "TestExecRunner") {
		t.Fatalf("out = %q", out.String())
	}
	if err := ExecRunner(context.Background(), []string{filepath.Join(dir, "missing")}, nil, dir, io.Discard, io.Discard); err == nil {
		t.Fatal("missing executable")
	}
	se := &StepError{Step: Step{Name: "n", Argv: []string{"go"}}, Err: errors.New("e"), Stderr: strings.Repeat("x", 3000)}
	if !strings.Contains(se.Error(), "...") || len(se.Error()) > 2100 {
		t.Fatal("stderr tail truncation")
	}
}
