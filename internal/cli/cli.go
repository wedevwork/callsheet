// Package cli implements the callsheet command tree: parsing, help, the
// implemented leaves (version, since iteration 02 the four plane commands,
// since iteration 03 sidecar enroll and run and node ls and show) and the
// deterministic outcomes of reserved (not yet implemented) commands. Reserved commands never contact a plane, create
// state or invoke a vendor CLI.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Version is the build version; builds may override it with
// -ldflags "-X github.com/wedevwork/callsheet/internal/cli.Version=...".
var Version = "dev"

// Command is a node of the command tree. Help and execution use the same tree.
type Command struct {
	Name     string
	Summary  string
	Children []*Command

	parent *Command
	// builtin marks the built-in version leaf.
	builtin bool
	// run implements an implemented leaf other than version; usage and
	// details extend its help.
	run     leafFunc
	usage   string
	details string
}

// leafFunc executes an implemented leaf with the arguments after its name.
type leafFunc func(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int

// implemented reports whether c is an implemented leaf.
func (c *Command) implemented() bool { return c.builtin || c.run != nil }

// Path returns the space-separated command path, e.g. "callsheet task ls".
func (c *Command) Path() string {
	if c.parent == nil {
		return c.Name
	}
	return c.parent.Path() + " " + c.Name
}

// IsLeaf reports whether c has no children.
func (c *Command) IsLeaf() bool { return len(c.Children) == 0 }

// Child returns the named immediate child or nil.
func (c *Command) Child(name string) *Command {
	for _, ch := range c.Children {
		if ch.Name == name {
			return ch
		}
	}
	return nil
}

// Leaves returns every leaf below c in tree order.
func (c *Command) Leaves() []*Command {
	if c.IsLeaf() {
		return []*Command{c}
	}
	var out []*Command
	for _, ch := range c.Children {
		out = append(out, ch.Leaves()...)
	}
	return out
}

// Groups returns c and every non-leaf below it in tree order.
func (c *Command) Groups() []*Command {
	if c.IsLeaf() {
		return nil
	}
	out := []*Command{c}
	for _, ch := range c.Children {
		out = append(out, ch.Groups()...)
	}
	return out
}

func node(name, summary string, children ...*Command) *Command {
	c := &Command{Name: name, Summary: summary, Children: children}
	for _, ch := range children {
		ch.parent = c
	}
	return c
}

// supportedOS reports whether goos is a supported v1 platform (Linux, macOS).
func supportedOS(goos string) bool { return goos == "linux" || goos == "darwin" }

// NewTree builds the command tree for goos without reading host state.
// Linux and macOS get the identical full tree; every other goos (including
// "windows" and "") is unsupported and yields nil.
func NewTree(goos string) *Command {
	if !supportedOS(goos) {
		return nil
	}
	children := []*Command{
		{Name: "version", Summary: "Print the callsheet and protocol version", builtin: true},
		node("plane", "Plane service commands (Linux/macOS)",
			planeLeaf("init", "Initialize plane state", initUsage, initDetails, planeInit),
			planeLeaf("run", "Run the plane service", runUsage, runDetails, planeRun),
			planeLeaf("status", "Show plane status", statusUsage, statusDetails, planeStatus),
			node("cert", "Plane certificate commands",
				planeLeaf("reissue", "Reissue the plane server certificate", reissueUsage, reissueDetails, planeReissue),
			),
		),
		node("sidecar", "Node sidecar commands (Linux/macOS)",
			planeLeaf("enroll", "Enroll this node with a plane", enrollUsage, enrollDetails, sidecarEnroll),
			planeLeaf("run", "Run the node sidecar", sidecarUsage, sidecarRunDetails, sidecarRun),
		),
		node("role", "Role commands",
			node("add", "Add a role"),
			node("set", "Change a role"),
			node("ls", "List roles"),
			node("show", "Show a role"),
			node("rm", "Remove a role"),
		),
		node("node", "Node commands",
			planeLeaf("ls", "List nodes", nodeLsUsage, nodeLsDetails, nodeLs),
			planeLeaf("show", "Show a node", nodeShowUsage, nodeShowDetails, nodeShow),
		),
		node("dispatch", "Dispatch a task to a role"),
		node("task", "Task commands",
			node("ls", "List tasks"),
			node("show", "Show a task"),
			node("logs", "Show task output"),
			node("cancel", "Cancel a task"),
			node("wait", "Wait for a task"),
			node("prune", "Prune finished tasks"),
		),
		node("ws", "Workspace commands",
			node("create", "Create a workspace"),
			node("ls", "List workspaces"),
			node("show", "Show a workspace"),
			node("rm", "Remove a workspace"),
			node("prune", "Prune workspaces"),
			node("push", "Push local changes to a workspace"),
			node("pull", "Pull a workspace"),
			node("status", "Show local workspace status"),
			node("diff", "Diff a workspace"),
			node("ref", "Workspace ref commands",
				node("set", "Set a workspace ref"),
			),
		),
		node("mcp", "Run the local MCP server (stdio)"),
	}
	return node("callsheet", "Callsheet coordinates AI coding agents across machines", children...)
}

// Run executes args (without the program name) against the tree for the
// running OS and returns the process exit code. An unsupported OS is
// rejected with exit 2 before any argument is parsed. Run is the only host
// OS read; every decision is made by runFor from its goos argument.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return runFor(ctx, runtime.GOOS, args, in, out, errOut)
}

// runFor is Run for an explicit goos. Input is currently unused: no command
// reads stdin in this build.
func runFor(ctx context.Context, goos string, args []string, in io.Reader, out, errOut io.Writer) int {
	return run(ctx, NewTree(goos), goos, args, out, errOut)
}

func run(ctx context.Context, root *Command, goos string, args []string, out, errOut io.Writer) int {
	if ctx.Err() != nil {
		fmt.Fprintf(errOut, "callsheet: interrupted\n")
		return contract.ExitInterrupted
	}
	if root == nil || !supportedOS(goos) {
		e := contract.New(contract.CodeInvalidArgument,
			fmt.Sprintf("unsupported operating system %q; supported: linux, darwin", goos))
		diag(errOut, e)
		return contract.ExitCode(e)
	}
	cur := root
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case cur == root && tok == "help":
			return helpPath(root, args[i+1:], out, errOut)
		case tok == "--help" || tok == "-h":
			writeHelp(out, cur)
			return 0
		case cur == root && tok == "--version":
			return runVersion(root.Child("version"), args[i+1:], out, errOut)
		case strings.HasPrefix(tok, "-"):
			return usageError(errOut, cur, fmt.Sprintf("unknown flag %q for %q", tok, cur.Path()))
		}
		next := cur.Child(tok)
		if next == nil {
			return unknownChild(errOut, cur, tok)
		}
		cur = next
		if cur.IsLeaf() {
			return runLeaf(ctx, goos, cur, args[i+1:], out, errOut)
		}
	}
	writeHelp(out, cur)
	return 0
}

func unknownChild(errOut io.Writer, cur *Command, tok string) int {
	return usageError(errOut, cur, fmt.Sprintf("unknown command %q for %q", tok, cur.Path()))
}

func helpPath(root *Command, path []string, out, errOut io.Writer) int {
	cur := root
	for _, tok := range path {
		if strings.HasPrefix(tok, "-") {
			return usageError(errOut, cur, fmt.Sprintf("unknown flag %q for help", tok))
		}
		next := cur.Child(tok)
		if next == nil {
			return unknownChild(errOut, cur, tok)
		}
		cur = next
	}
	writeHelp(out, cur)
	return 0
}

func runLeaf(ctx context.Context, goos string, c *Command, rest []string, out, errOut io.Writer) int {
	for _, tok := range rest {
		if tok == "--" {
			break
		}
		if tok == "--help" || tok == "-h" {
			writeHelp(out, c)
			return 0
		}
	}
	if c.builtin {
		return runVersion(c, rest, out, errOut)
	}
	if c.run != nil {
		return c.run(ctx, goos, c, rest, out, errOut)
	}
	diag(errOut, contract.New(contract.CodeNotImplemented, fmt.Sprintf("%q is not implemented yet", c.Path())))
	return contract.ExitCode(contract.New(contract.CodeNotImplemented, ""))
}

func runVersion(c *Command, rest []string, out, errOut io.Writer) int {
	for _, tok := range rest {
		if tok == "--help" || tok == "-h" {
			writeHelp(out, c)
			return 0
		}
	}
	if len(rest) > 0 {
		return usageError(errOut, c, "version takes no arguments")
	}
	fmt.Fprintf(out, "callsheet %s protocol=%d\n", Version, contract.ProtocolVersion)
	return 0
}

func diag(errOut io.Writer, e *contract.Error) {
	fmt.Fprintf(errOut, "callsheet: %s: %s\n", e.Code, e.Message)
}

func usageError(errOut io.Writer, c *Command, msg string) int {
	e := contract.New(contract.CodeInvalidArgument, msg)
	diag(errOut, e)
	writeHelp(errOut, c)
	return contract.ExitCode(e)
}

func writeHelp(w io.Writer, c *Command) {
	var b strings.Builder
	if c.IsLeaf() {
		usage := c.Path()
		if c.usage != "" {
			usage += " " + c.usage
		}
		fmt.Fprintf(&b, "Usage: %s\n\n%s\n", usage, c.Summary)
		if c.details != "" {
			b.WriteString("\n" + c.details)
		}
		if c.implemented() {
			b.WriteString("\nStatus: implemented.\n")
		} else {
			b.WriteString("\nStatus: future stub; not implemented yet (exits 8).\n")
		}
		io.WriteString(w, b.String())
		return
	}
	fmt.Fprintf(&b, "Usage: %s <command>\n\n%s\n\nCommands:\n", c.Path(), c.Summary)
	width := 0
	for _, ch := range c.Children {
		width = max(width, len(ch.Name))
	}
	for _, ch := range c.Children {
		status := "future stub"
		if ch.implemented() {
			status = "implemented"
		} else if !ch.IsLeaf() {
			status = "group"
		}
		fmt.Fprintf(&b, "  %-*s  %s [%s]\n", width, ch.Name, ch.Summary, status)
	}
	if c.parent == nil {
		b.WriteString("\nUse \"callsheet help [command path]\" or --help for details. " +
			"Commands marked [implemented] work in this build; [future stub] commands are reserved and exit 8.\n")
	}
	io.WriteString(w, b.String())
}
