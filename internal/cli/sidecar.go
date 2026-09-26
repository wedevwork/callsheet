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

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/logging"
	"github.com/wedevwork/callsheet/internal/sidecar"
)

// The sidecar and node leaves (iteration 03).
const (
	trustUsage   = "--plane URL (--ca FILE | --ca-fingerprint SHA256)"
	enrollUsage  = trustUsage + " [--state-dir PATH]"
	sidecarUsage = "[--state-dir PATH]"

	trustHelp = "  --plane URL        the plane's https origin, e.g. https://plane.example:8443; a DNS name\n" +
		"                     or IP address the plane's certificate names (callsheet plane status)\n" +
		"  --ca FILE          the plane's CA certificate (a copy of pki/ca.crt from the plane)\n" +
		"  --ca-fingerprint SHA256\n" +
		"                     the CA fingerprint printed by callsheet plane init or status\n" +
		"                     (sha256:<64 hex>); the CA is fetched once and verified against it\n" +
		"Exactly one of --ca and --ca-fingerprint is required: without one the connection is\n" +
		"not trusted and nothing is contacted. Verification can never be disabled.\n"
	sidecarStateHelp = "  --state-dir PATH   sidecar state directory (default: $XDG_STATE_HOME/callsheet/sidecar or\n" +
		"                     ~/.local/state/callsheet/sidecar on Linux; ~/Library/Application Support/\n" +
		"                     callsheet/sidecar on macOS)\n"

	enrollDetails = "Flags:\n" + trustHelp + sidecarStateHelp + "\n" +
		"Creates a stable node identity (kept across restarts and re-enrollment), registers it\n" +
		"with the plane and records the plane URL and CA. Re-enrolling keeps the identity; a\n" +
		"new --plane or CA replaces only the recorded target. It prints node_id, plane_url and\n" +
		"ca_fingerprint. It does not start the connection: run callsheet sidecar run next.\n\n" +
		"Example, on the worker machine:\n" +
		"  callsheet sidecar enroll --plane https://plane.example:8443 \\\n" +
		"    --ca-fingerprint sha256:<fingerprint from callsheet plane status>\n" +
		"  callsheet sidecar run\n"
	sidecarRunDetails = "Flags:\n" + sidecarStateHelp + "\n" +
		"Connects out to the enrolled plane over verified TLS (it listens on nothing), proves\n" +
		"the protocol version and heartbeats every 5 s. When the plane goes away it reconnects\n" +
		"with backoff (1, 2, 4, 8, 16, then 30 s) and never exits for that reason. It exits on\n" +
		"SIGINT or SIGTERM (130), or on a configuration error: protocol version mismatch (7),\n" +
		"an invalid protocol exchange (2), a node unknown to the plane (3) or failed trust\n" +
		"(6). Logs are JSON on stderr; stdout stays empty.\n"
)

// boolFlag is a boolean flag that may be given at most once.
type boolFlag struct{ val, set bool }

func (b *boolFlag) String() string   { return fmt.Sprint(b.val) }
func (b *boolFlag) IsBoolFlag() bool { return true }

func (b *boolFlag) Set(v string) error {
	if b.set {
		return errors.New("may be given only once")
	}
	if v != "true" {
		return errors.New("takes no value")
	}
	b.val, b.set = true, true
	return nil
}

// remoteFlags are the sidecar enroll and node flags.
type remoteFlags struct {
	plane, ca, pin, stateDir single
	json                     boolFlag
}

// parseRemote parses flags that may surround at most maxOps operands. It
// returns ok=false with the exit code of a usage error. Empty explicit
// values are rejected.
func parseRemote(c *Command, args []string, withTrust, withStateDir, withJSON bool, maxOps int, errOut io.Writer) (*remoteFlags, []string, int, bool) {
	f := &remoteFlags{}
	fs := flag.NewFlagSet(c.Path(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if withTrust {
		fs.Var(&f.plane, "plane", "")
		fs.Var(&f.ca, "ca", "")
		fs.Var(&f.pin, "ca-fingerprint", "")
	}
	if withStateDir {
		fs.Var(&f.stateDir, "state-dir", "")
	}
	if withJSON {
		fs.Var(&f.json, "json", "")
	}
	var ops []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, nil, usageError(errOut, c, fmt.Sprintf("%s: %v", c.Path(), err)), false
		}
		if fs.NArg() == 0 {
			break
		}
		ops = append(ops, fs.Arg(0))
		rest = fs.Args()[1:]
		if len(ops) > maxOps {
			return nil, nil, usageError(errOut, c, fmt.Sprintf("unexpected argument %q for %q", ops[len(ops)-1], c.Path())), false
		}
	}
	for _, s := range []struct {
		name string
		v    *single
	}{{"--plane", &f.plane}, {"--ca", &f.ca}, {"--ca-fingerprint", &f.pin}, {"--state-dir", &f.stateDir}} {
		if s.v.set && s.v.val == "" {
			return nil, nil, usageError(errOut, c, s.name+" must not be empty"), false
		}
	}
	return f, ops, 0, true
}

// trustSyntax checks the syntax of --plane and the trust flags before any
// state or network access: a missing or invalid URL, both trust flags, or
// a malformed fingerprint are invalid_argument. A missing trust flag is
// left to client.ResolveTrust (trust_failed).
func (f *remoteFlags) trustSyntax() (string, error) {
	if !f.plane.set {
		return "", contract.New(contract.CodeInvalidArgument, "--plane is required: give the plane's https URL")
	}
	u, err := client.NormalizePlaneURL(f.plane.val)
	if err != nil {
		return "", err
	}
	if f.ca.set && f.pin.set {
		return "", contract.New(contract.CodeInvalidArgument, "give only one of --ca and --ca-fingerprint")
	}
	if f.pin.set && !client.ValidPin(f.pin.val) {
		return "", contract.New(contract.CodeInvalidArgument, "invalid --ca-fingerprint: want sha256: followed by 64 lowercase hex digits, as printed by callsheet plane init")
	}
	return u, nil
}

// resolveSidecar returns the sidecar state root for goos.
func (f *remoteFlags) resolveSidecar(goos string) (string, error) {
	return sidecar.ResolveStateDir(goos, f.stateDir.val, os.Getenv("HOME"), os.Getenv("XDG_STATE_HOME"))
}

func sidecarEnroll(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, ops, code, ok := parseRemote(c, args, true, true, false, 0, errOut)
	if !ok {
		return code
	}
	_ = ops
	u, err := f.trustSyntax()
	if err != nil {
		return planeFail(errOut, err)
	}
	dir, err := f.resolveSidecar(goos)
	if err != nil {
		return planeFail(errOut, err)
	}
	e, err := sidecar.Enroll(ctx, sidecar.EnrollOptions{StateDir: dir, PlaneURL: u, CAFile: f.ca.val, CAFingerprint: f.pin.val, SoftwareVersion: Version})
	if err != nil {
		return planeFail(errOut, err)
	}
	report := "node_id: " + e.NodeID + "\nplane_url: " + e.PlaneURL + "\nca_fingerprint: " + e.CAFingerprint + "\n"
	if _, err := io.WriteString(out, report); err != nil {
		return planeFail(errOut, contract.Wrap(contract.CodeInternal, "cannot write the report to stdout: "+err.Error()+"; the enrollment may already have completed (run callsheet sidecar enroll again to see it)", err))
	}
	return 0
}

func sidecarRun(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, _, code, ok := parseRemote(c, args, false, true, false, 0, errOut)
	if !ok {
		return code
	}
	dir, err := f.resolveSidecar(goos)
	if err != nil {
		return planeFail(errOut, err)
	}
	logger := logging.Component(logging.New(errOut, slog.LevelInfo), "sidecar")
	err = sidecar.Run(ctx, sidecar.RunOptions{StateDir: dir, SoftwareVersion: Version, Logger: logger})
	return planeFail(errOut, err)
}

// joinFields renders one tab-separated row.
func joinFields(fields []string) string { return strings.Join(fields, "\t") + "\n" }
