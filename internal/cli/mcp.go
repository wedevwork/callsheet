package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/mcp"
)

// The coordinator door (iteration 07a): callsheet mcp, the local stdio MCP
// server a coordinator CLI starts from its MCP configuration. It validates
// its flags at startup and connects to the plane lazily, per tool call,
// over verified TLS; stdout carries the MCP protocol only.
const (
	mcpUsage = "--plane URL (--ca FILE | --ca-fingerprint SHA256) [--wait-call-budget DURATION]"

	mcpDetails = "Flags:\n" + trustHelp +
		"  --wait-call-budget DURATION\n" +
		"                     the outer budget of one task_wait or dispatch-with-wait tool call,\n" +
		"                     from admission through delivery of its answer: 1s to 5m (default\n" +
		"                     10s). The plane wait it asks for is shorter: reserves for transport,\n" +
		"                     response and (dispatch) admission are kept\n\n" +
		"Serves the Model Context Protocol (revision " + mcp.ProtocolVersion + ") on stdin and stdout:\n" +
		"newline-delimited JSON-RPC, one message per line. Stdout carries the protocol only;\n" +
		"diagnostics go to stderr. The twenty-three tools mirror the CLI one to one: node_ls,\n" +
		"node_show, role_add, role_set, role_ls, role_show, role_rm, dispatch, task_ls,\n" +
		"task_show, task_logs, task_cancel, task_wait, the workspace tools ws_create, ws_ls,\n" +
		"ws_show, ws_rm, ws_prune, ws_ref_set, ws_status and ws_diff (these operate on the\n" +
		"plane's stored workspaces, never on local files) and the local transfers ws_push\n" +
		"(reads local files) and ws_pull (writes local files), whose paths are on this\n" +
		"machine and resolve against this server's working directory. It is a stateless relay: every tool\n" +
		"call resolves trust afresh and reads the plane, nothing is cached, and a plane outage\n" +
		"fails the call instead of serving an earlier answer. Dispatches are attributed to the\n" +
		"MCP client's self-reported name and version and this machine's hostname.\n\n" +
		mcp.UnverifiedNotice + "\n\n" +
		"It exits 0 when stdin ends, 130 on SIGINT or SIGTERM, 5 when stdout breaks or stops\n" +
		"accepting output, and 2 on an input line over 512 KiB or an unterminated last line.\n" +
		"Exiting never cancels a task, removes a role or stops the plane.\n\n" +
		"Example MCP server entry (command and arguments):\n" +
		"  callsheet mcp --plane https://plane.example:8443 --ca /path/to/plane-ca.crt\n"
)

// Seams of the SIGPIPE registration (tests record them).
var (
	signalNotify = signal.Notify
	signalStop   = signal.Stop
)

// mcpServe runs the MCP leaf. Startup validates the URL, the exclusive
// trust flags and the budget without contacting the plane. Only this leaf
// registers for SIGPIPE, for its own duration: a write to a broken stdout
// then fails with EPIPE (exit 5) instead of Go's default SIGPIPE death on
// fd 1; the plane, sidecar and every other command keep their
// disposition.
func mcpServe(ctx context.Context, goos string, c *Command, args []string, in io.Reader, out, errOut io.Writer) int {
	var budgetFlag single
	f, _, code, ok := parseRemote(c, args, true, false, false, 0, errOut, func(fs *flag.FlagSet) { fs.Var(&budgetFlag, "wait-call-budget", "") })
	if !ok {
		return code
	}
	u, err := f.trustSyntax()
	if err != nil {
		return planeFail(errOut, err)
	}
	if !f.ca.set && !f.pin.set {
		// The existing mapping of a missing trust input (trust_failed),
		// decided before any network access.
		_, err := client.ResolveTrust(ctx, client.TrustOptions{PlaneURL: u})
		return planeFail(errOut, err)
	}
	budget := mcp.DefaultBudget
	if budgetFlag.set {
		d, err := time.ParseDuration(budgetFlag.val)
		if err != nil || strings.TrimSpace(budgetFlag.val) != budgetFlag.val || !mcp.ValidBudget(d) {
			return usageError(errOut, c, "--wait-call-budget must be a Go duration from 1s to 5m (default 10s)")
		}
		budget = d
	}
	if goos != "linux" && goos != "darwin" {
		e := contract.New(contract.CodeInvalidArgument, fmt.Sprintf("unsupported operating system %q for callsheet mcp; supported: linux, darwin", goos))
		diag(errOut, e)
		return contract.ExitCode(e)
	}
	sigpipe := make(chan os.Signal, 1)
	signalNotify(sigpipe, syscall.SIGPIPE)
	defer signalStop(sigpipe)
	st := mcpStreams(in, out, errOut)
	return mcp.Serve(ctx, mcp.Config{
		Factory:  mcp.PlaneFactory(u, f.ca.val, f.pin.val),
		Budget:   budget,
		Version:  Version,
		Hostname: hostname,
		Clock:    mcp.RealClock,
		In:       st.in,
		Out:      st.out,
		Stderr:   errOut,
		CloseIn:  st.closeIn,
		CloseOut: st.closeOut,
		GOOS:     goos,
		Cwd:      processCwd,
		Transfer: mcp.PlaneTransferFactory(u, f.ca.val, f.pin.val, goos, processEnv),
	})
}

// mcpIO are the leaf's owned stdio handles and the closers that unblock
// them; a nil closer marks a stream whose pending I/O cannot be
// interrupted, which the server then never waits for.
type mcpIO struct {
	in                io.Reader
	out               io.Writer
	closeIn, closeOut func() error
}

// ownGeneric owns a non-file stream: a closable one (a test pipe) is
// closed to unblock it; nil input is an empty one.
func ownGeneric(in io.Reader, out io.Writer) mcpIO {
	st := mcpIO{in: in, out: out}
	if in == nil {
		st.in = strings.NewReader("")
	} else if c, ok := in.(io.Closer); ok {
		st.closeIn = c.Close
	}
	if c, ok := out.(io.Closer); ok {
		st.closeOut = c.Close
	}
	return st
}
