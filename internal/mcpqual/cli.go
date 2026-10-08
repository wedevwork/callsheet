package mcpqual

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// HarnessVersion identifies this harness in reports and serverInfo.
const HarnessVersion = "07b.1"

// FaultEnv is the developer-only fault variable. Only cmd/mcpqual reads it
// (and passes its value to SignalerFor); callsheet never does.
const FaultEnv = "MCPQUAL_TEST_FAULT"

// Env is everything the developer entrypoint supplies from the host.
type Env struct {
	GOOS, GOARCH string
	Args         []string
	Getenv       func(string) string
	// LookupEnv reports a variable's presence (cmd/mcpqual wires
	// os.LookupEnv): capture and qualify refuse any CI presence, even an
	// empty CI=, and refuse when no lookup is supplied.
	LookupEnv  func(string) (string, bool)
	Environ    []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Signaler   Signaler
	Launcher   Launcher
	Clock      Clock
	Registry   Registry
	Hostname   string
	Executable string
	Home, User string
	Now        func() time.Time
	Rand       io.Reader
	// Notify returns a context canceled on SIGINT or SIGTERM (qualify and
	// publish only; the probe keeps the default signal behaviour).
	Notify func(context.Context) (context.Context, context.CancelFunc)
}

const usage = `usage:
  mcpqual qualify --plan ABSOLUTE_JSON --out ABSOLUTE_DIR [--allow-model-calls] [--publish-catalog ABSOLUTE_REPO]
  mcpqual capture --plan ABSOLUTE_JSON --out ABSOLUTE_NEW_OR_EMPTY_DIR --allow-model-calls
  mcpqual serve --case-file PATH --events PATH
  mcpqual publish --out ABSOLUTE_DIR --repo ABSOLUTE_REPO   (retries a --publish-catalog run's patch)
  mcpqual version

qualify and capture never run when CI is present (even empty). Without
--allow-model-calls qualify runs only version/help/config checks and
model-free direct drivers; capture refuses to start. capture records one
decoder-free setup session per client and never evaluates vendor behavior.
`

// Main runs the developer command and returns its exit code: 0 conclusive
// (capture: complete), 1 filesystem or internal failure, 2 invalid input
// (including CI present for qualify or capture, and capture without
// --allow-model-calls), 4 publication conflict, 5 partial/unqualified or
// cleanup failure, 130 interrupted.
func Main(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}
	switch env.Args[0] {
	case "qualify":
		return qualify(ctx, env, env.Args[1:])
	case "capture":
		return capture(ctx, env, env.Args[1:])
	case "serve":
		return serve(env, env.Args[1:])
	case "publish":
		return publish(env, env.Args[1:])
	case "version":
		fmt.Fprintln(env.Stdout, "mcpqual "+HarnessVersion)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, usage)
		return 0
	}
	fmt.Fprintf(env.Stderr, "mcpqual: unknown command %q\n%s", env.Args[0], usage)
	return 2
}

func fail(env Env, err error) int {
	fmt.Fprintf(env.Stderr, "mcpqual: %v\n", err)
	if code := contract.ExitCode(err); code != 1 {
		return code
	}
	return 1
}

func invalid(format string, a ...any) error {
	return contract.New(contract.CodeInvalidArgument, fmt.Sprintf(format, a...))
}

func newFlags(env Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("mcpqual "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return invalid("%v", err)
	}
	if fs.NArg() > 0 {
		return invalid("unexpected arguments %q", fs.Args())
	}
	return nil
}

func readBounded(p string, limit int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err == nil && len(b) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", p, limit)
	}
	return b, err
}

// ciPresent is the CI prohibition of qualify and capture (design
// decoder-enrollment, Capture interface): any presence of CI, including an
// empty value, and a missing lookup (fail closed) refuse the run.
func ciPresent(env Env) bool {
	if env.LookupEnv == nil {
		return true
	}
	_, ok := env.LookupEnv("CI")
	return ok || env.Getenv != nil && env.Getenv("CI") != ""
}

func qualify(ctx context.Context, env Env, args []string) int {
	// The CI refusal comes first: before the plan is read and before any
	// vendor launch, even with --allow-model-calls.
	if ciPresent(env) {
		return fail(env, invalid("qualify refused: CI is set (present, even empty); real-vendor qualification never runs in CI"))
	}
	fs := newFlags(env, "qualify")
	planPath := fs.String("plan", "", "absolute path of the version-1 qualification plan")
	out := fs.String("out", "", "absolute, empty or new evidence directory")
	allow := fs.Bool("allow-model-calls", false, "explicitly permit model sessions up to the plan budget")
	repo := fs.String("publish-catalog", "", "absolute repository whose catalog and evidence to update after the run")
	if err := parseFlags(fs, args); err != nil {
		return fail(env, err)
	}
	policy, supported := PolicyFor(env.GOOS)
	switch {
	case !supported:
		return fail(env, invalid("qualification runs on linux or darwin only, not %s", env.GOOS))
	case !filepath.IsAbs(*planPath) || filepath.Clean(*planPath) != *planPath:
		return fail(env, invalid("--plan %q must be an absolute clean path", *planPath))
	case *out == "":
		return fail(env, invalid("--out is required"))
	}
	raw, err := readBounded(*planPath, MaxPlanBytes)
	if err != nil {
		return fail(env, invalid("--plan: %v", err))
	}
	plan, err := ParsePlan(raw, env.Registry)
	if err != nil {
		return fail(env, invalid("%v", err))
	}
	var base *CatalogBase
	if *repo != "" {
		if base, err = ReadCatalogBase(*repo); err != nil {
			return fail(env, err)
		}
	}
	rnd := make([]byte, 22)
	if _, err := io.ReadFull(env.Rand, rnd); err != nil {
		return fail(env, err)
	}
	now := env.Now().UTC()
	runID := now.Format("20060102T150405Z") + "-" + hex.EncodeToString(rnd[:3])
	ctx, stop := env.Notify(ctx)
	defer stop()
	r := &Runner{Plan: plan, PlanSHA256: sha256Hex(raw), OutDir: *out, AllowModelCalls: *allow, GOOS: env.GOOS, GOARCH: env.GOARCH,
		Hostname: env.Hostname, ServerPath: env.Executable, BaseEnv: withoutCI(env.Environ), Launcher: env.Launcher,
		Reaper: GroupReaper{Sig: env.Signaler, Clock: env.Clock, Policy: policy}, Clock: env.Clock, Registry: env.Registry, Log: env.Stderr,
		RunID: runID, Nonce: hex.EncodeToString(rnd[3:19]), CaptureDate: now.Format("2006-01-02"), HarnessVersion: HarnessVersion, Home: env.Home, User: env.User}
	rep, err := r.Run(ctx)
	if err != nil {
		if rep == nil {
			return fail(env, err)
		}
		fmt.Fprintf(env.Stderr, "mcpqual: %v\n", err)
		return 1
	}
	code := rep.ExitCode()
	fmt.Fprintf(env.Stdout, "mcpqual: run %s: %s; %s; report %s (exit %d)\n", rep.RunID, rep.Outcome, rep.VendorBehavior, filepath.Join(*out, "report.json"), code)
	if base == nil {
		return code
	}
	switch {
	case !rep.Cleanup.OK:
		fmt.Fprintln(env.Stderr, "mcpqual: publication refused: cleanup failed ("+strings.Join(rep.Cleanup.Failures, "; ")+")")
		return code
	case rep.Interrupted:
		fmt.Fprintln(env.Stderr, "mcpqual: publication skipped: the run was interrupted")
		return code
	}
	if _, err := ProposePatch(rep, base, *out); err != nil {
		return fail(env, err)
	}
	if err := Publish(*out, base.Repo); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "mcpqual: published run %s into %s\n", rep.RunID, base.Repo)
	return code
}

// capture is "mcpqual capture" (design decoder-enrollment, FP-1): refused
// with CI present before anything else, and without --allow-model-calls
// before the plan is read; then a decoder-free setup capture of the
// capture plan into a new or empty directory.
func capture(ctx context.Context, env Env, args []string) int {
	if ciPresent(env) {
		return fail(env, invalid("capture refused: CI is set (present, even empty); real-vendor capture never runs in CI"))
	}
	fs := newFlags(env, "capture")
	planPath := fs.String("plan", "", "absolute path of the version-1 short plan to capture")
	out := fs.String("out", "", "absolute, new or empty capture directory")
	allow := fs.Bool("allow-model-calls", false, "explicitly permit one model setup session per client")
	if err := parseFlags(fs, args); err != nil {
		return fail(env, err)
	}
	policy, supported := PolicyFor(env.GOOS)
	switch {
	case !*allow:
		return fail(env, invalid("capture needs --allow-model-calls: it launches one model setup session per client"))
	case !supported:
		return fail(env, invalid("capture runs on linux or darwin only, not %s", env.GOOS))
	case !filepath.IsAbs(*planPath) || filepath.Clean(*planPath) != *planPath:
		return fail(env, invalid("--plan %q must be an absolute clean path", *planPath))
	case *out == "":
		return fail(env, invalid("--out is required"))
	}
	raw, err := readBounded(*planPath, MaxPlanBytes)
	if err != nil {
		return fail(env, invalid("--plan: %v", err))
	}
	plan, err := ParseCapturePlan(raw)
	if err != nil {
		return fail(env, invalid("%v", err))
	}
	rnd := make([]byte, 19)
	if _, err := io.ReadFull(env.Rand, rnd); err != nil {
		return fail(env, err)
	}
	ctx, stop := env.Notify(ctx)
	defer stop()
	c := newCaptureRunner(env, plan, *out, policy, rnd)
	man, err := c.Run(ctx)
	if err != nil {
		if contract.ExitCode(err) != 1 {
			return fail(env, err)
		}
		fmt.Fprintf(env.Stderr, "mcpqual: %v\n", err)
		return 1
	}
	status := "capture " + man.State
	if man.State == CaptureComplete {
		status = "capture complete"
	}
	fmt.Fprintf(env.Stdout, "mcpqual: run %s: %s; vendor behavior not evaluated; manifest %s (exit %d)\n", man.RunID, status,
		filepath.Join(*out, CaptureManifestName), man.ExitCode())
	return man.ExitCode()
}

// newCaptureRunner is the production capture runner, built only from Env
// and the parsed invocation. It never sets GrokPlacementFS (the placement
// gate always observes the real filesystem) nor any other test seam
// (design decoder-enrollment B1, production wiring pin).
func newCaptureRunner(env Env, plan *Plan, out string, policy CleanupPolicy, rnd []byte) *CaptureRunner {
	now := env.Now().UTC()
	return &CaptureRunner{Plan: plan, OutDir: out, GOOS: env.GOOS, GOARCH: env.GOARCH, ServerPath: env.Executable, BaseEnv: withoutCI(env.Environ),
		Launcher: env.Launcher, Reaper: GroupReaper{Sig: env.Signaler, Clock: env.Clock, Policy: policy}, Clock: env.Clock, Log: env.Stderr,
		RunID: now.Format("20060102T150405Z") + "-" + hex.EncodeToString(rnd[:3]), Nonce: hex.EncodeToString(rnd[3:19]), CapturedAt: now,
		HarnessVersion: HarnessVersion, Home: env.Home, User: env.User}
}

func withoutCI(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "CI=") {
			out = append(out, kv)
		}
	}
	return out
}

func serve(env Env, args []string) int {
	fs := newFlags(env, "serve")
	casePath := fs.String("case-file", "", "the probe's case file")
	events := fs.String("events", "", "the events file to append to")
	if err := parseFlags(fs, args); err != nil {
		return fail(env, err)
	}
	if *casePath == "" || *events == "" {
		return fail(env, invalid("serve needs --case-file and --events"))
	}
	raw, err := readBounded(*casePath, MaxCaseFileBytes)
	if err != nil {
		return fail(env, invalid("--case-file: %v", err))
	}
	cf, err := ParseCaseFile(raw)
	if err != nil {
		return fail(env, invalid("%v", err))
	}
	f, err := os.OpenFile(*events, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(env, invalid("--events: %v", err))
	}
	defer f.Close()
	st, _ := f.Stat()
	limit := MaxEvidenceFileBytes
	if st != nil {
		limit = max(2*exitReserve, limit-int(st.Size()))
	}
	if err := Serve(ProbeConfig{Cases: cf, In: env.Stdin, Out: env.Stdout, Events: f, Clock: env.Clock, Version: HarnessVersion, EventLimit: limit}); err != nil {
		if errors.Is(err, ErrProbeFatal) {
			return fail(env, contract.Wrap(contract.CodeUnavailable, err.Error(), err))
		}
		return fail(env, invalid("%v", err))
	}
	return 0
}

func publish(env Env, args []string) int {
	fs := newFlags(env, "publish")
	out := fs.String("out", "", "the evidence directory of a finished run with a proposed patch")
	repo := fs.String("repo", "", "the absolute repository to publish into")
	if err := parseFlags(fs, args); err != nil {
		return fail(env, err)
	}
	if !filepath.IsAbs(*out) {
		return fail(env, invalid("--out %q must be absolute", *out))
	}
	if err := Publish(*out, *repo); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "mcpqual: published %s into %s\n", *out, *repo)
	return 0
}
