package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/logging"
	"github.com/wedevwork/callsheet/internal/plane"
)

// The four implemented plane leaves (iteration 02). Flags follow the leaf;
// there are no positional operands and no prompts.
const (
	initUsage    = "[--state-dir PATH] [--bind IP:PORT] --san NAME [--san NAME ...]"
	runUsage     = "[--state-dir PATH] [--bind IP:PORT] [--san NAME ...]"
	statusUsage  = "[--state-dir PATH]"
	reissueUsage = "[--state-dir PATH] --san NAME [--san NAME ...]"

	stateDirHelp = "  --state-dir PATH  plane state directory (default: $XDG_STATE_HOME/callsheet/plane or\n" +
		"                    ~/.local/state/callsheet/plane on Linux; ~/Library/Application Support/\n" +
		"                    callsheet/plane on macOS)\n"
	bindHelp = "  --bind IP:PORT    private listen address: 127.0.0.1, ::1 or a local 10/8, 172.16/12,\n" +
		"                    192.168/16, 100.64/10 or fc00::/7 address (default 127.0.0.1:8443)\n"

	initDetails = "Flags:\n" + stateDirHelp + bindHelp +
		"  --san NAME        DNS name or IP address clients use to reach the plane; repeat for\n" +
		"                    each name (required for new state)\n\n" +
		"Creates the state directory, the internal CA and the server certificate, and prints\n" +
		"their paths, validity and the CA fingerprint. On initialized state nothing is\n" +
		"rewritten: supplied flags must match, and the same report is printed.\n"
	runDetails = "Flags:\n" + stateDirHelp + bindHelp +
		"  --san NAME        first-start SANs when the state is empty (as plane init)\n\n" +
		"Serves HTTPS only, on the configured private bind, until interrupted (SIGINT or\n" +
		"SIGTERM; exit 130). Logs are JSON on stderr; stdout stays empty.\n"
	statusDetails = "Flags:\n" + stateDirHelp + "\n" +
		"Prints paths, SANs, the CA fingerprint and certificate validity offline, and warns\n" +
		"on stderr when either certificate expires within 30 days. It does not show whether\n" +
		"the plane is running.\n"
	reissueDetails = "Flags:\n" + stateDirHelp +
		"  --san NAME        complete replacement SAN list; repeat for each name (required)\n\n" +
		"Stop the plane first. Issues a new server certificate for the same key under the\n" +
		"unchanged CA; clients keep their CA file. Restart the plane to serve it.\n"
)

func planeLeaf(name, summary, usage, details string, run leafFunc) *Command {
	return &Command{Name: name, Summary: summary, usage: usage, details: details, run: run}
}

// single is a flag that may be given at most once.
type single struct {
	val string
	set bool
}

func (s *single) String() string { return s.val }

func (s *single) Set(v string) error {
	if s.set {
		return errors.New("may be given only once")
	}
	s.val, s.set = v, true
	return nil
}

// multi is a repeatable flag.
type multi struct {
	vals []string
	set  bool
}

func (m *multi) String() string { return strings.Join(m.vals, ",") }

func (m *multi) Set(v string) error {
	m.vals = append(m.vals, v)
	m.set = true
	return nil
}

type planeFlags struct {
	stateDir, bind single
	sans           multi
}

// parsePlane parses a plane leaf's flags. withBind and withSAN select the
// optional flags. It returns ok=false with the exit code of a usage error.
func parsePlane(c *Command, args []string, withBind, withSAN bool, errOut io.Writer) (*planeFlags, int, bool) {
	f := &planeFlags{}
	fs := flag.NewFlagSet(c.Path(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&f.stateDir, "state-dir", "")
	if withBind {
		fs.Var(&f.bind, "bind", "")
	}
	if withSAN {
		fs.Var(&f.sans, "san", "")
	}
	if err := fs.Parse(args); err != nil {
		return nil, usageError(errOut, c, fmt.Sprintf("%s: %v", c.Path(), err)), false
	}
	if fs.NArg() > 0 {
		return nil, usageError(errOut, c, fmt.Sprintf("unexpected argument %q for %q", fs.Arg(0), c.Path())), false
	}
	if f.stateDir.set && f.stateDir.val == "" {
		return nil, usageError(errOut, c, "--state-dir must not be empty"), false
	}
	return f, 0, true
}

// resolve returns the state root for goos from --state-dir, HOME and
// XDG_STATE_HOME.
func (f *planeFlags) resolve(goos string) (string, error) {
	return plane.ResolveStateDir(goos, f.stateDir.val, os.Getenv("HOME"), os.Getenv("XDG_STATE_HOME"))
}

// planeFail reports err: cancellation exits 130, contract errors use their
// code, anything else is internal.
func planeFail(errOut io.Writer, err error) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(errOut, "callsheet: interrupted\n")
		return contract.ExitInterrupted
	}
	var ce *contract.Error
	if !errors.As(err, &ce) {
		ce = contract.Wrap(contract.CodeInternal, err.Error(), err)
	}
	diag(errOut, ce)
	return contract.ExitCode(ce)
}

func planeInit(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, code, ok := parsePlane(c, args, true, true, errOut)
	if !ok {
		return code
	}
	dir, err := f.resolve(goos)
	if err != nil {
		return planeFail(errOut, err)
	}
	st, err := plane.Init(ctx, plane.InitOptions{StateDir: dir, Bind: f.bind.val, BindSet: f.bind.set, SANs: f.sans.vals, SANsSet: f.sans.set})
	if err != nil {
		return planeFail(errOut, err)
	}
	return writeReport(out, errOut, st)
}

func planeRun(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, code, ok := parsePlane(c, args, true, true, errOut)
	if !ok {
		return code
	}
	dir, err := f.resolve(goos)
	if err != nil {
		return planeFail(errOut, err)
	}
	logger := logging.Component(logging.New(errOut, slog.LevelInfo), "plane")
	err = plane.Run(ctx, plane.RunOptions{StateDir: dir, Bind: f.bind.val, BindSet: f.bind.set, SANs: f.sans.vals, SANsSet: f.sans.set, Logger: logger})
	return planeFail(errOut, err)
}

func planeStatus(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, code, ok := parsePlane(c, args, false, false, errOut)
	if !ok {
		return code
	}
	dir, err := f.resolve(goos)
	if err != nil {
		return planeFail(errOut, err)
	}
	st, err := plane.Inspect(ctx, dir)
	if err != nil {
		return planeFail(errOut, err)
	}
	io.WriteString(errOut, RenderWarnings(st.Warnings))
	return writeReport(out, errOut, st)
}

func planeReissue(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, code, ok := parsePlane(c, args, false, true, errOut)
	if !ok {
		return code
	}
	if !f.sans.set {
		return usageError(errOut, c, "reissue requires the complete replacement list as one or more --san flags")
	}
	dir, err := f.resolve(goos)
	if err != nil {
		return planeFail(errOut, err)
	}
	st, err := plane.Reissue(ctx, plane.ReissueOptions{StateDir: dir, SANs: f.sans.vals})
	if err != nil {
		return planeFail(errOut, err)
	}
	return writeReport(out, errOut, st)
}

// writeReport prints the report; a writer failure is internal (1) and
// never rolls back the completed operation.
func writeReport(out, errOut io.Writer, st plane.Status) int {
	if _, err := io.WriteString(out, RenderStatus(st)); err != nil {
		return planeFail(errOut, contract.Wrap(contract.CodeInternal, "cannot write the report to stdout: "+err.Error()+"; the operation itself completed", err))
	}
	return 0
}

// pathEscaper escapes backslash, CR, LF and TAB in one pass, so a
// backslash is never escaped twice.
var pathEscaper = strings.NewReplacer(`\`, `\\`, "\r", `\r`, "\n", `\n`, "\t", `\t`)

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// RenderStatus renders the 13-line report shared by init, status and
// reissue: "label: value" lines in fixed order, each ending in LF.
func RenderStatus(s plane.Status) string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"state_dir", pathEscaper.Replace(s.StateDir)},
		{"bind", s.Bind},
		{"ca_cert_path", pathEscaper.Replace(s.CACertPath)},
		{"ca_key_path", pathEscaper.Replace(s.CAKeyPath)},
		{"server_cert_path", pathEscaper.Replace(s.ServerCertPath)},
		{"server_key_path", pathEscaper.Replace(s.ServerKeyPath)},
		{"dns_names", strings.Join(s.DNSNames, ",")},
		{"ip_addresses", strings.Join(s.IPAddresses, ",")},
		{"ca_fingerprint", s.CAFingerprint},
		{"ca_not_before", rfc3339(s.CANotBefore)},
		{"ca_not_after", rfc3339(s.CANotAfter)},
		{"server_not_before", rfc3339(s.ServerNotBefore)},
		{"server_not_after", rfc3339(s.ServerNotAfter)},
	} {
		b.WriteString(kv[0] + ": " + kv[1] + "\n")
	}
	return b.String()
}

// RenderWarnings renders status warnings for stderr, CA before server.
func RenderWarnings(ws []plane.Warning) string {
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "callsheet: warning: %s %s: %s\n", w.Certificate, w.Condition, rfc3339(w.At))
	}
	return b.String()
}
