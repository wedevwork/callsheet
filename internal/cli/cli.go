// Package cli implements the callsheet command tree: parsing, help and the
// deterministic outcomes of reserved (not yet implemented) commands.
//
// Iteration 01 reserves names only. No command contacts a plane, creates
// state or invokes a vendor CLI.
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
	// builtin marks leaves that are implemented now (only "version").
	builtin bool
}

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

// unixOnly lists root commands that exist only on Linux and macOS.
var unixOnly = map[string]bool{"plane": true, "sidecar": true}

// NewTree builds the command tree for goos without reading host state.
// Windows omits the plane and sidecar groups.
func NewTree(goos string) *Command {
	children := []*Command{
		{Name: "version", Summary: "Print the callsheet and protocol version", builtin: true},
		node("plane", "Plane service commands (Linux/macOS)",
			node("init", "Initialize plane state"),
			node("run", "Run the plane service"),
			node("status", "Show plane status"),
			node("cert", "Plane certificate commands",
				node("reissue", "Reissue the plane server certificate"),
			),
		),
		node("sidecar", "Node sidecar commands (Linux/macOS)",
			node("enroll", "Enroll this node with a plane"),
			node("run", "Run the node sidecar"),
		),
		node("role", "Role commands",
			node("add", "Add a role"),
			node("set", "Change a role"),
			node("ls", "List roles"),
			node("show", "Show a role"),
			node("rm", "Remove a role"),
		),
		node("node", "Node commands",
			node("ls", "List nodes"),
			node("show", "Show a node"),
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
	var kept []*Command
	for _, c := range children {
		if goos == "windows" && unixOnly[c.Name] {
			continue
		}
		kept = append(kept, c)
	}
	return node("callsheet", "Callsheet coordinates AI coding agents across machines", kept...)
}

// Run executes args (without the program name) against the tree for the
// running OS and returns the process exit code.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return run(ctx, NewTree(runtime.GOOS), runtime.GOOS, args, out, errOut)
}

func run(ctx context.Context, root *Command, goos string, args []string, out, errOut io.Writer) int {
	if ctx.Err() != nil {
		fmt.Fprintf(errOut, "callsheet: interrupted\n")
		return contract.ExitInterrupted
	}
	cur := root
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case cur == root && tok == "help":
			return helpPath(root, goos, args[i+1:], out, errOut)
		case tok == "--help" || tok == "-h":
			writeHelp(out, cur, goos)
			return 0
		case cur == root && tok == "--version":
			return runVersion(root.Child("version"), args[i+1:], out, errOut, goos)
		case strings.HasPrefix(tok, "-"):
			return usageError(errOut, cur, goos, fmt.Sprintf("unknown flag %q for %q", tok, cur.Path()))
		}
		next := cur.Child(tok)
		if next == nil {
			return unknownChild(errOut, cur, goos, tok)
		}
		cur = next
		if cur.IsLeaf() {
			return runLeaf(cur, args[i+1:], out, errOut, goos)
		}
	}
	writeHelp(out, cur, goos)
	return 0
}

func unknownChild(errOut io.Writer, cur *Command, goos, tok string) int {
	if goos == "windows" && cur.parent == nil && unixOnly[tok] {
		return usageError(errOut, cur, goos,
			fmt.Sprintf("%q is available on Linux and macOS only; Windows supports coordinator commands", tok))
	}
	return usageError(errOut, cur, goos, fmt.Sprintf("unknown command %q for %q", tok, cur.Path()))
}

func helpPath(root *Command, goos string, path []string, out, errOut io.Writer) int {
	cur := root
	for _, tok := range path {
		if strings.HasPrefix(tok, "-") {
			return usageError(errOut, cur, goos, fmt.Sprintf("unknown flag %q for help", tok))
		}
		next := cur.Child(tok)
		if next == nil {
			return unknownChild(errOut, cur, goos, tok)
		}
		cur = next
	}
	writeHelp(out, cur, goos)
	return 0
}

func runLeaf(c *Command, rest []string, out, errOut io.Writer, goos string) int {
	for _, tok := range rest {
		if tok == "--" {
			break
		}
		if tok == "--help" || tok == "-h" {
			writeHelp(out, c, goos)
			return 0
		}
	}
	if c.builtin {
		return runVersion(c, rest, out, errOut, goos)
	}
	diag(errOut, contract.New(contract.CodeNotImplemented, fmt.Sprintf("%q is not implemented yet", c.Path())))
	return contract.ExitCode(contract.New(contract.CodeNotImplemented, ""))
}

func runVersion(c *Command, rest []string, out, errOut io.Writer, goos string) int {
	for _, tok := range rest {
		if tok == "--help" || tok == "-h" {
			writeHelp(out, c, goos)
			return 0
		}
	}
	if len(rest) > 0 {
		return usageError(errOut, c, goos, "version takes no arguments")
	}
	fmt.Fprintf(out, "callsheet %s protocol=%d\n", Version, contract.ProtocolVersion)
	return 0
}

func diag(errOut io.Writer, e *contract.Error) {
	fmt.Fprintf(errOut, "callsheet: %s: %s\n", e.Code, e.Message)
}

func usageError(errOut io.Writer, c *Command, goos, msg string) int {
	e := contract.New(contract.CodeInvalidArgument, msg)
	diag(errOut, e)
	writeHelp(errOut, c, goos)
	return contract.ExitCode(e)
}

func writeHelp(w io.Writer, c *Command, goos string) {
	var b strings.Builder
	if c.IsLeaf() {
		fmt.Fprintf(&b, "Usage: %s\n\n%s\n", c.Path(), c.Summary)
		if c.builtin {
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
		if ch.builtin {
			status = "implemented"
		} else if !ch.IsLeaf() {
			status = "group"
		}
		fmt.Fprintf(&b, "  %-*s  %s [%s]\n", width, ch.Name, ch.Summary, status)
	}
	if c.parent == nil {
		b.WriteString("\nUse \"callsheet help [command path]\" or --help for details. " +
			"All commands except version are future stubs in this build.\n")
		if goos == "windows" {
			b.WriteString("The plane and sidecar commands are available on Linux and macOS only.\n")
		}
	}
	io.WriteString(w, b.String())
}
