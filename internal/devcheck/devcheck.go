// Package devcheck is the development-only, pure-Go check driver behind
// cmd/devcheck: test, coverage, bench, cross, all, native, stress and the
// seven stress shards stress-packages, stress-plane-cpu1, stress-plane,
// stress-sidecar-cpu1, stress-sidecar, stress-processgroup and
// stress-functions. It is
// not distributed and imports no product services. Child tools run with argv
// (no shell).
package devcheck

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
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
// A Runner must be safe for concurrent calls: the Parallel stress shards,
// the plane and sidecar CPU2/CPU4 pairs (iteration 05b and its sidecar
// follow-up, CPU 1 in singleton shards since design 06a-perf) and
// processgroup (iteration 02c), call it from up to three goroutines at
// once, each with its own writers.
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
	planned, err := crossPlan(outDir, targets)
	if err != nil {
		return nil, err
	}
	steps := make([]Step, len(planned))
	for i, p := range planned {
		steps[i] = p.step
	}
	return steps, nil
}

// Artifact is one planned cross-build output: its path and the target its
// executable header must name.
type Artifact struct {
	Path   string
	Target Target
}

// CrossArtifacts returns the artifacts CrossPlan writes for targets into
// outDir, in plan order: the cross stage verifies exactly these.
func CrossArtifacts(outDir string, targets []Target) ([]Artifact, error) {
	planned, err := crossPlan(outDir, targets)
	if err != nil {
		return nil, err
	}
	out := make([]Artifact, len(planned))
	for i, p := range planned {
		out[i] = p.artifact
	}
	return out, nil
}

// plannedStep is one cross step with the artifact it writes.
type plannedStep struct {
	step     Step
	artifact Artifact
}

// crossPlan is the single cross plan behind CrossPlan and CrossArtifacts.
func crossPlan(outDir string, targets []Target) ([]plannedStep, error) {
	if len(targets) == 0 {
		return nil, errors.New("devcheck: no targets")
	}
	var plan []plannedStep
	for _, t := range targets {
		if err := ValidateTarget(t); err != nil {
			return nil, err
		}
		env := []string{"CGO_ENABLED=0", "GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH}
		// add plans "<cmd...> -o <outDir>/<name> <pkg>".
		add := func(what, name, pkg string, cmd ...string) {
			path := filepath.Join(outDir, name)
			argv := append(append(cmd, "-o", path), pkg)
			plan = append(plan, plannedStep{Step{Name: "cross " + t.String() + " " + what, Env: env, Argv: argv}, Artifact{path, t}})
		}
		add("callsheet", CallsheetArtifact(t), "./cmd/callsheet", "go", "build")
		add("fake-adapter", FakeArtifact(t), "./cmd/fake-adapter", "go", "build")
		add("processgroup test", ProcessTestArtifact(t), "./internal/spikes/processgroup", "go", "test", "-c")
	}
	return plan, nil
}

// InspectExecutable reads path's executable header, never running the
// file, and returns the target it was built for: an executable ELF file is
// a linux target, an executable Mach-O file a darwin target, and the
// header's machine type gives amd64 or arm64. Any other file fails.
func InspectExecutable(path string) (Target, error) {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		arch, ok := map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64"}[f.Machine]
		switch {
		case f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN:
			return Target{}, fmt.Errorf("ELF type %v is not an executable", f.Type)
		case f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX:
			return Target{}, fmt.Errorf("ELF OS ABI %v is not linux", f.OSABI)
		case !ok:
			return Target{}, fmt.Errorf("ELF machine %v is not amd64 or arm64", f.Machine)
		}
		return Target{"linux", arch}, nil
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		arch, ok := map[macho.Cpu]string{macho.CpuAmd64: "amd64", macho.CpuArm64: "arm64"}[f.Cpu]
		switch {
		case f.Type != macho.TypeExec:
			return Target{}, fmt.Errorf("Mach-O type %v is not an executable", f.Type)
		case !ok:
			return Target{}, fmt.Errorf("Mach-O CPU %v is not amd64 or arm64", f.Cpu)
		}
		return Target{"darwin", arch}, nil
	}
	return Target{}, errors.New("neither an ELF nor a Mach-O file")
}

// VerifyArtifacts checks a cross build into outDir without executing
// anything: outDir holds exactly the artifacts CrossArtifacts plans for
// targets, and each is a nonempty regular file whose executable header
// (InspectExecutable) names its target. Every problem is reported, each
// naming its artifact.
func VerifyArtifacts(outDir string, targets []Target) error {
	arts, err := CrossArtifacts(outDir, targets)
	if err != nil {
		return err
	}
	var problems []error
	bad := func(name, format string, args ...any) {
		problems = append(problems, fmt.Errorf("devcheck: cross artifact %s: %s", name, fmt.Sprintf(format, args...)))
	}
	planned := map[string]bool{}
	for _, a := range arts {
		name := filepath.Base(a.Path)
		planned[name] = true
		st, err := os.Lstat(a.Path)
		switch {
		case err != nil:
			bad(name, "missing: %v", err)
			continue
		case !st.Mode().IsRegular():
			bad(name, "not a regular file (%v)", st.Mode().Type())
			continue
		case st.Size() == 0:
			bad(name, "empty")
			continue
		}
		got, err := InspectExecutable(a.Path)
		if err != nil {
			bad(name, "not a %s executable: %v", a.Target, err)
		} else if got != a.Target {
			bad(name, "built for %s, want %s", got, a.Target)
		}
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		problems = append(problems, fmt.Errorf("devcheck: cross output: %w", err))
	}
	for _, e := range entries {
		if !planned[e.Name()] {
			bad(e.Name(), "not a planned artifact")
		}
	}
	return errors.Join(problems...)
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
// every target into outDir, using the caller's working directory (the
// repository root), then verifies the build (VerifyArtifacts): outDir
// holds exactly the planned artifacts, each nonempty with an ELF or
// Mach-O header naming its GOOS/GOARCH. It neither creates nor removes
// outDir, and never executes the foreign binaries.
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
	return VerifyArtifacts(outDir, targets)
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
	// Iteration 08: the ordinary-only tagged sidecar contract, as its own
	// step (and its race counterpart where the suite has one), on the task
	// platforms only.
	if goos != "linux" && goos != "darwin" {
		return steps
	}
	tagged := []string{"-tags=" + RealAdapterTag, "./internal/sidecar", "-run=^TestRealAdapterLocal$", "-count=1"}
	steps = append(steps, Step{Name: "test " + RealAdapterTag, Argv: append([]string{"go", "test"}, tagged...)})
	if goos == "linux" {
		steps = append(steps, Step{Name: "test " + RealAdapterTag + " -race", Env: []string{"CGO_ENABLED=1"},
			Argv: append([]string{"go", "test", "-race"}, tagged...)})
	}
	return steps
}

// BenchSteps runs the git transport (iteration 01 FP-5) benchmarks, then
// the plane benchmarks (iteration 02 trust: issuance, initialization and
// verified TLS health; iteration 03 nodes: heartbeat, snapshot and
// enrollment; iteration 04 roles: snapshot views and durable mutations),
// then the wire-contract frame benchmarks (iteration 03 nodes, iteration 04
// role frames), then the sidecar ready checks and the adapter's fake probe
// (iteration 04), then the MCP codec, relay and wait-budget benchmarks
// (iteration 07a), then the qualification harness's transcript and probe
// benchmarks (iteration 07b), then the workspace hub (iteration 09a), then
// the local workspace transfers (iteration 09b). Timings are reported,
// never gated.
func BenchSteps() []Step {
	pkg := func(name, dir string) Step {
		return Step{Name: name, Argv: []string{"go", "test", dir, "-run=^$", "-bench=.", "-benchmem", "-benchtime=3x", "-count=1", "-timeout=180s"}}
	}
	return []Step{
		{Name: "bench", Argv: []string{"go", "test", "./internal/spikes/gittransport", "-run", "^$", "-bench", ".", "-benchmem", "-benchtime=3x", "-count=1", "-timeout=180s"}},
		pkg("bench plane", "./internal/plane"),
		pkg("bench contract", "./internal/contract"),
		pkg("bench sidecar", "./internal/sidecar"),
		pkg("bench adapter", "./internal/adapter"),
		pkg("bench mcp", "./internal/mcp"),
		pkg("bench mcpqual", "./internal/mcpqual"),
		// Iteration 09a: the workspace hub's transfer, transaction,
		// pagination, accounting, diff and prune benchmarks.
		WorkspaceBenchStep(),
		// Iteration 09b: the local transfers' status, snapshot, push,
		// pull and export benchmarks.
		TransferBenchStep(),
		// Iteration 10b: the task workspace's prepare, snapshot, publish
		// and metadata benchmarks.
		TaskWorkspaceBenchStep(),
		// Iteration 08: the tagged final-file helper benchmark (the vendor
		// extractor and invocation benchmarks run in "bench adapter").
		{Name: "bench sidecar " + RealAdapterTag, Argv: []string{"go", "test", "./internal/sidecar", "-tags=" + RealAdapterTag, "-run=^$",
			"-bench=^BenchmarkRealAdapterFile$", "-benchmem", "-benchtime=3x", "-count=1", "-timeout=180s"}},
	}
}

// WorkspaceBenchStep is the workspace hub's benchmark step (iteration
// 09a), shared by the bench stage and the native driver.
func WorkspaceBenchStep() Step {
	return Step{Name: "bench workspace", Argv: []string{"go", "test", "./internal/workspace", "-run=^$", "-bench=.", "-benchmem", "-benchtime=3x", "-count=1", "-timeout=180s"}}
}

// TransferBenchStep is the local workspace transfers' benchmark step
// (iteration 09b), shared by the bench stage and the native driver.
func TransferBenchStep() Step {
	return Step{Name: "bench workspacetransfer", Argv: []string{"go", "test", "./internal/workspacetransfer", "-run=^$", "-bench=.", "-benchmem", "-benchtime=3x", "-count=1", "-timeout=180s"}}
}

// TaskWorkspaceBenchStep is the task workspace's benchmark step
// (iteration 10b), shared by the bench stage and the native driver.
func TaskWorkspaceBenchStep() Step {
	return Step{Name: "bench taskworkspace", Argv: []string{"go", "test", "./internal/taskworkspace", "-run=^$", "-bench=.", "-benchmem", "-benchtime=3x", "-count=1", "-timeout=180s"}}
}

// CoverageSteps returns the profile run, the func report and the cmd
// listing. The profile run compiles the realadaptercheck tag (iteration 08)
// so its single profile includes the tagged sidecar contract.
func CoverageSteps(profile string) (test, report, list Step) {
	test = Step{Name: "coverage", Argv: []string{"go", "test", "-count=1", "-tags=" + RealAdapterTag, "-covermode=atomic", "-coverpkg=./internal/...,./cmd/...",
		"-coverprofile=" + profile, "./internal/...", "./cmd/..."}}
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

// stepLogName is the scratch log file name of a step.
func stepLogName(name string) string {
	return strings.NewReplacer(" ", "-", "/", "-").Replace(name) + ".log"
}

// logged runs s, teeing output to out and a scratch log; capture (if non-nil)
// also receives stdout.
func (d *driver) logged(s Step, capture io.Writer) error {
	f, err := os.Create(filepath.Join(d.scratch, stepLogName(s.Name)))
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
	// Iteration 09a: the new and changed production code of the manifest,
	// per group, from the same profile; since iteration 09b per file too,
	// with the entries applicable to this host's explicit goos.
	groups, err := CheckCoverageManifest(d.goos, string(prof), WorkspaceCoverageManifest)
	for _, g := range groups {
		fmt.Fprintf(d.out, "devcheck: new/changed coverage %s %.1f%% (%d/%d statements, %d blocks, %d files) (bar: > %.1f%%)\n",
			g.Group, g.Percent, g.Hit, g.Statements, g.Blocks, g.Files, CoverageThreshold)
	}
	return err
}

func (d *driver) cross() error {
	out := filepath.Join(d.scratch, "cross")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "devcheck: cross: %d targets into %s\n", len(Matrix), out)
	if err := Cross(d.ctx, d.run, out, Matrix); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "devcheck: cross: verified %d artifacts (exist, nonempty, ELF/Mach-O header matches GOOS/GOARCH)\n", 3*len(Matrix))
	return nil
}

const usage = "usage: devcheck test | coverage [-o profile] | bench | cross | all | native | stress | stress-packages | stress-plane-cpu1 | stress-plane | stress-sidecar-cpu1 | stress-sidecar | stress-processgroup | stress-functions\n"

// stageNames is the single stage definition used by argument dispatch and
// advertised by Stages. The seven stress-* stages each run one stress shard
// (iteration 02c, one CI worker job per platform each; stress-plane since
// iteration 05b, stress-sidecar since its sidecar follow-up, and
// stress-plane-cpu1 and stress-sidecar-cpu1 since design 06a-perf, which
// leaves stress-plane and stress-sidecar with CPU 2 and 4 only); "stress"
// runs all seven.
var stageNames = [...]string{"test", "coverage", "bench", "cross", "all", "native", "stress", "stress-packages", "stress-plane-cpu1", "stress-plane", "stress-sidecar-cpu1", "stress-sidecar", "stress-processgroup", "stress-functions"}

// allStages is the stage sequence of "all". "native" and the stress stages
// are selected explicitly: stress repeats subprocess builds and process
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
	var nativeSteps []Step
	var stressShards []StressShard
	var planErr error
	switch {
	case sub == "native":
		nativeSteps, planErr = NativeSteps(goos)
	case isStressStage(sub):
		stressShards, planErr = stressPlan(goos, sub)
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
		case "stress", "stress-packages", "stress-plane-cpu1", "stress-plane", "stress-sidecar-cpu1", "stress-sidecar", "stress-processgroup", "stress-functions":
			err = d.stress(stressShards)
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
