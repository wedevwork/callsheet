// Package devcheck is the development-only, pure-Go check driver behind
// cmd/devcheck: test, coverage, bench, cross, all, native and stress. It is
// not distributed and imports no product services. Child tools run with argv
// (no shell).
package devcheck

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Runner runs argv (argv[0] is the executable) with the complete child
// environment env in directory dir ("" = the caller's working directory).
type Runner func(ctx context.Context, argv []string, env []string, dir string, stdout, stderr io.Writer) error

// ExecRunner implements Runner with exec.CommandContext.
func ExecRunner(ctx context.Context, argv []string, env []string, dir string, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("devcheck: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// Target is one GOOS/GOARCH pair.
type Target struct {
	GOOS   string
	GOARCH string
}

func (t Target) String() string { return t.GOOS + "/" + t.GOARCH }

// Unix reports whether t is a plane/sidecar (Linux/macOS) target.
func (t Target) Unix() bool { return t.GOOS == "linux" || t.GOOS == "darwin" }

// Matrix is exactly the supported four-target (Linux/macOS) cross-build
// matrix. v1 supports no other platform.
var Matrix = []Target{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
}

// ValidateTarget rejects any pair outside Matrix.
func ValidateTarget(t Target) error {
	for _, m := range Matrix {
		if m == t {
			return nil
		}
	}
	return fmt.Errorf("devcheck: unsupported target %s", t)
}

// CallsheetArtifact is the callsheet artifact name for a target. Formatting
// a name grants no target support; CrossPlan validates targets.
func CallsheetArtifact(t Target) string { return "callsheet-" + t.GOOS + "-" + t.GOARCH }

// FakeArtifact is the fake-adapter artifact name.
func FakeArtifact(t Target) string { return "fake-adapter-" + t.GOOS + "-" + t.GOARCH }

// ProcessTestArtifact is the compiled process-group test artifact name.
func ProcessTestArtifact(t Target) string { return "processgroup-" + t.GOOS + "-" + t.GOARCH + ".test" }

// Step is one planned child command; Env holds overrides applied on top of
// the parent environment.
type Step struct {
	Name string
	Argv []string
	Env  []string
}

// CrossPlan returns the build steps for targets into outDir: callsheet,
// fake-adapter and the process-group test binary for every target. Any
// target outside Matrix fails the whole plan.
func CrossPlan(outDir string, targets []Target) ([]Step, error) {
	if len(targets) == 0 {
		return nil, errors.New("devcheck: no targets")
	}
	var steps []Step
	for _, t := range targets {
		if err := ValidateTarget(t); err != nil {
			return nil, err
		}
		env := []string{"CGO_ENABLED=0", "GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH}
		steps = append(steps,
			Step{Name: "cross " + t.String() + " callsheet", Env: env,
				Argv: []string{"go", "build", "-o", filepath.Join(outDir, CallsheetArtifact(t)), "./cmd/callsheet"}},
			Step{Name: "cross " + t.String() + " fake-adapter", Env: env,
				Argv: []string{"go", "build", "-o", filepath.Join(outDir, FakeArtifact(t)), "./cmd/fake-adapter"}},
			Step{Name: "cross " + t.String() + " processgroup test", Env: env,
				Argv: []string{"go", "test", "-c", "-o", filepath.Join(outDir, ProcessTestArtifact(t)), "./internal/spikes/processgroup"}},
		)
	}
	return steps, nil
}

// MergeEnv returns base with every KEY in overrides replaced.
func MergeEnv(base, overrides []string) []string {
	keys := map[string]bool{}
	for _, kv := range overrides {
		keys[strings.SplitN(kv, "=", 2)[0]] = true
	}
	var out []string
	for _, kv := range base {
		if !keys[strings.SplitN(kv, "=", 2)[0]] {
			out = append(out, kv)
		}
	}
	return append(out, overrides...)
}

// StepError names the failed command.
type StepError struct {
	Step   Step
	Err    error
	Stderr string
}

func (e *StepError) Error() string {
	msg := fmt.Sprintf("%s failed: %s: %v", e.Step.Name, strings.Join(e.Step.Argv, " "), e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		if len(s) > 2000 {
			s = "..." + s[len(s)-2000:]
		}
		msg += "\n" + s
	}
	return msg
}

func (e *StepError) Unwrap() error { return e.Err }

func runStep(ctx context.Context, run Runner, s Step, stdout io.Writer) error {
	var stderr bytes.Buffer
	var errOut io.Writer = &stderr
	if stdout != nil {
		errOut = io.MultiWriter(&stderr, stdout)
	} else {
		stdout = io.Discard
	}
	if err := run(ctx, s.Argv, MergeEnv(os.Environ(), s.Env), "", stdout, errOut); err != nil {
		return &StepError{Step: s, Err: err, Stderr: stderr.String()}
	}
	return nil
}

// Cross builds callsheet, fake-adapter and the process-group test binary for
// every target into outDir, using the
// caller's working directory (the repository root). It neither creates nor
// removes outDir, and never executes the foreign binaries.
func Cross(ctx context.Context, run Runner, outDir string, targets []Target) error {
	steps, err := CrossPlan(outDir, targets)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if err := runStep(ctx, run, s, nil); err != nil {
			return err
		}
	}
	return nil
}

// Coverage bar: strictly greater than this percentage.
const CoverageThreshold = 80.0

// ParseCoverTotal extracts the total percentage from "go tool cover -func".
func ParseCoverTotal(out string) (float64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.Fields(lines[i])
		if len(f) >= 2 && f[0] == "total:" {
			return strconv.ParseFloat(strings.TrimSuffix(f[len(f)-1], "%"), 64)
		}
	}
	return 0, errors.New("devcheck: no total line in coverage output")
}

// MissingCmdPackages returns cmd packages with Go files that have no file
// represented in the coverage profile. listOut lines are "importpath|N".
func MissingCmdPackages(listOut, profile string) []string {
	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(listOut), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil || n == 0 {
			continue
		}
		if !strings.Contains(profile, "\n"+parts[0]+"/") && !strings.HasPrefix(profile, parts[0]+"/") {
			missing = append(missing, parts[0])
		}
	}
	sort.Strings(missing)
	return missing
}

// TestSteps, BenchSteps and CoverageSteps are the planned stage commands.
func TestSteps(goos string) []Step {
	steps := []Step{{Name: "test", Argv: []string{"go", "test", "-count=1", "-timeout=180s", "./..."}}}
	if goos == "linux" {
		steps = append(steps, Step{Name: "test -race", Env: []string{"CGO_ENABLED=1"},
			Argv: []string{"go", "test", "-race", "-count=1", "-timeout=180s", "./..."}})
	}
	return steps
}

// BenchSteps runs the FP-5 benchmarks.
func BenchSteps() []Step {
	return []Step{{Name: "bench", Argv: []string{"go", "test", "./internal/spikes/gittransport", "-run", "^$", "-bench", ".", "-benchmem", "-benchtime=3x", "-count=1", "-timeout=180s"}}}
}

// CoverageSteps returns the profile run, the func report and the cmd listing.
func CoverageSteps(profile string) (test, report, list Step) {
	test = Step{Name: "coverage", Argv: []string{"go", "test", "-count=1", "-covermode=atomic", "-coverpkg=./internal/...,./cmd/...", "-coverprofile=" + profile, "./internal/...", "./cmd/..."}}
	report = Step{Name: "coverage report", Argv: []string{"go", "tool", "cover", "-func=" + profile}}
	list = Step{Name: "list cmd packages", Argv: []string{"go", "list", "-f", "{{.ImportPath}}|{{len .GoFiles}}", "./cmd/..."}}
	return
}

type driver struct {
	ctx     context.Context
	run     Runner
	out     io.Writer
	errOut  io.Writer
	scratch string
	goos    string
}

func (d *driver) steps(steps []Step) error {
	for _, s := range steps {
		fmt.Fprintf(d.out, "devcheck: %s: %s\n", s.Name, strings.Join(s.Argv, " "))
		if err := d.logged(s, nil); err != nil {
			return err
		}
	}
	return nil
}

// logged runs s, teeing output to out and a scratch log; capture (if non-nil)
// also receives stdout.
func (d *driver) logged(s Step, capture io.Writer) error {
	name := strings.NewReplacer(" ", "-", "/", "-").Replace(s.Name) + ".log"
	f, err := os.Create(filepath.Join(d.scratch, name))
	if err != nil {
		return err
	}
	defer f.Close()
	w := io.MultiWriter(f, d.out)
	if capture != nil {
		w = io.MultiWriter(f, capture)
	}
	return runStep(d.ctx, d.run, s, w)
}

func (d *driver) coverage(outPath string) error {
	profile := outPath
	if profile == "" {
		profile = filepath.Join(d.scratch, "coverage.out")
	}
	test, report, list := CoverageSteps(profile)
	if err := d.steps([]Step{test}); err != nil {
		return err
	}
	var rep, lst bytes.Buffer
	if err := d.logged(report, &rep); err != nil {
		return err
	}
	io.Copy(d.out, bytes.NewReader(rep.Bytes()))
	total, err := ParseCoverTotal(rep.String())
	if err != nil {
		return err
	}
	fmt.Fprintf(d.out, "devcheck: coverage total %.1f%% (bar: > %.1f%%)\n", total, CoverageThreshold)
	if !(total > CoverageThreshold) {
		return fmt.Errorf("devcheck: coverage %.1f%% is not greater than %.1f%%", total, CoverageThreshold)
	}
	if err := d.logged(list, &lst); err != nil {
		return err
	}
	prof, err := os.ReadFile(profile)
	if err != nil {
		return err
	}
	if missing := MissingCmdPackages(lst.String(), string(prof)); len(missing) > 0 {
		return fmt.Errorf("devcheck: cmd packages absent from coverage profile: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (d *driver) cross() error {
	out := filepath.Join(d.scratch, "cross")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "devcheck: cross: %d targets into %s\n", len(Matrix), out)
	return Cross(d.ctx, d.run, out, Matrix)
}

const usage = "usage: devcheck test | coverage [-o profile] | bench | cross | all | native | stress\n"

// stageNames is the single stage definition used by argument dispatch and
// advertised by Stages.
var stageNames = [...]string{"test", "coverage", "bench", "cross", "all", "native", "stress"}

// allStages is the stage sequence of "all". "native" and "stress" are
// selected explicitly: stress repeats subprocess builds and process
// experiments for minutes, so release and review verification run both
// "all" and "stress".
var allStages = []string{"test", "coverage", "bench", "cross"}

// Stages returns a fresh copy of the subcommand names accepted by dispatch.
func Stages() []string { return append([]string(nil), stageNames[:]...) }

func isStage(name string) bool {
	for _, s := range stageNames {
		if s == name {
			return true
		}
	}
	return false
}

// Run executes a devcheck subcommand and returns the process exit code:
// 0 success, 1 failed stage, 2 usage error.
func Run(ctx context.Context, args []string, out, errOut io.Writer, run Runner) int {
	return runFor(ctx, runtime.GOOS, args, out, errOut, run)
}

func runFor(ctx context.Context, goos string, args []string, out, errOut io.Writer, run Runner) int {
	if len(args) == 0 {
		io.WriteString(errOut, usage)
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("devcheck "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	coverOut := fs.String("o", "", "coverage profile output path")
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || (*coverOut != "" && sub != "coverage" && sub != "all") {
		fmt.Fprintf(errOut, "devcheck: invalid arguments for %s\n%s", sub, usage)
		return 2
	}
	if !isStage(sub) {
		fmt.Fprintf(errOut, "devcheck: unknown subcommand %q\n%s", sub, usage)
		return 2
	}
	stages := []string{sub}
	if sub == "all" {
		stages = allStages
	}
	// An unsupported native or stress host is rejected before any scratch
	// exists and before any child runs.
	var nativeSteps, stressSteps []Step
	var planErr error
	switch sub {
	case "native":
		nativeSteps, planErr = NativeSteps(goos)
	case "stress":
		stressSteps, planErr = StressSteps(goos)
	}
	if planErr != nil {
		fmt.Fprintf(errOut, "devcheck: stage %s FAILED: %v\n", sub, planErr)
		return 1
	}
	scratch, err := os.MkdirTemp("", "callsheet-devcheck-")
	if err != nil {
		fmt.Fprintf(errOut, "devcheck: scratch: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "devcheck: scratch %s\n", scratch)
	d := &driver{ctx: ctx, run: run, out: out, errOut: errOut, scratch: scratch, goos: goos}
	for _, st := range stages {
		var err error
		switch st {
		case "test":
			err = d.steps(TestSteps(goos))
		case "coverage":
			err = d.coverage(*coverOut)
		case "bench":
			err = d.steps(BenchSteps())
		case "cross":
			err = d.cross()
		case "native":
			err = d.native(nativeSteps)
		case "stress":
			err = d.stress(stressSteps)
		default:
			err = fmt.Errorf("devcheck: stage %q is advertised but not implemented", st)
		}
		if err != nil {
			fmt.Fprintf(errOut, "devcheck: stage %s FAILED: %v\ndevcheck: logs retained in %s\n", st, err, scratch)
			return 1
		}
		fmt.Fprintf(out, "devcheck: stage %s ok\n", st)
	}
	if err := os.RemoveAll(scratch); err != nil {
		fmt.Fprintf(errOut, "devcheck: remove scratch: %v\n", err)
		return 1
	}
	return 0
}
