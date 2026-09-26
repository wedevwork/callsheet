package cli

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

const (
	nodeLsUsage   = trustUsage + " [--json]"
	nodeShowUsage = "ID " + trustUsage + " [--json]"

	nodeJSONHelp = "  --json             print the plane's response envelope as one JSON value\n"

	nodeLsDetails = "Flags:\n" + trustHelp + nodeJSONHelp + "\n" +
		"Lists every node the plane knows, sorted by ID, as tab-separated columns\n" +
		"ID, LIVENESS, LAST_SEEN, PROTOCOL_VERSION, SOFTWARE_VERSION and ROLES (unknown values\n" +
		"print as -). It reads the running plane over verified TLS from any machine; nothing\n" +
		"is cached and no local state is needed.\n\n" +
		"Example, on an operator machine:\n" +
		"  callsheet node ls --plane https://plane.example:8443 --ca plane-ca.crt\n"
	nodeShowDetails = "Flags:\n" + trustHelp + nodeJSONHelp + "\n" +
		"Shows one node as label: value lines (id, liveness, last_seen, protocol_version,\n" +
		"software_version, roles). Flags may come before or after ID.\n\n" +
		"Example, on an operator machine:\n" +
		"  callsheet node show n_0123456789abcdef0123456789abcdef \\\n" +
		"    --plane https://plane.example:8443 --ca-fingerprint sha256:<fingerprint>\n"
)

// nodeClient resolves trust and returns a verified client. It never reads
// local state or environment configuration.
func nodeClient(ctx context.Context, f *remoteFlags) (*client.Client, error) {
	u, err := f.trustSyntax()
	if err != nil {
		return nil, err
	}
	trust, err := client.ResolveTrust(ctx, client.TrustOptions{PlaneURL: u, CAFile: f.ca.val, CAFingerprint: f.pin.val})
	if err != nil {
		return nil, err
	}
	return client.New(u, trust)
}

func nodeLs(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, _, code, ok := parseRemote(c, args, true, false, true, 0, errOut)
	if !ok {
		return code
	}
	cl, err := nodeClient(ctx, f)
	if err != nil {
		return planeFail(errOut, err)
	}
	defer cl.Close()
	nodes, err := cl.ListNodes(ctx)
	if err != nil {
		return planeFail(errOut, err)
	}
	var s string
	if f.json.val {
		b, err := contract.Encode(contract.NodeListResponse{Version: contract.ProtocolVersion, Nodes: nodes})
		if err != nil {
			return planeFail(errOut, err)
		}
		s = string(b) + "\n"
	} else {
		s = RenderNodes(nodes)
	}
	return writeOut(out, errOut, s)
}

func nodeShow(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut)
	if !ok {
		return code
	}
	if len(ops) != 1 {
		return usageError(errOut, c, "node show takes exactly one node ID")
	}
	if !contract.ValidNodeID(ops[0]) {
		return planeFail(errOut, contract.New(contract.CodeInvalidArgument, "invalid node ID; want n_ followed by 32 lowercase hex digits"))
	}
	cl, err := nodeClient(ctx, f)
	if err != nil {
		return planeFail(errOut, err)
	}
	defer cl.Close()
	n, err := cl.ShowNode(ctx, ops[0])
	if err != nil {
		return planeFail(errOut, err)
	}
	var s string
	if f.json.val {
		b, err := contract.Encode(contract.NodeResponse{Version: contract.ProtocolVersion, Node: n})
		if err != nil {
			return planeFail(errOut, err)
		}
		s = string(b) + "\n"
	} else {
		s = RenderNode(n)
	}
	return writeOut(out, errOut, s)
}

// writeOut writes the complete, already buffered output; a writer failure
// is internal.
func writeOut(out, errOut io.Writer, s string) int {
	if _, err := io.WriteString(out, s); err != nil {
		return planeFail(errOut, contract.Wrap(contract.CodeInternal, "cannot write to stdout: "+err.Error(), err))
	}
	return 0
}

// nodeFields renders a node's six values; null renders "-" and empty
// roles "[]". Version strings hold no controls or whitespace.
func nodeFields(n contract.Node) []string {
	dash := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	seen := "-"
	if n.LastSeen != nil {
		seen = n.LastSeen.UTC().Format(time.RFC3339Nano)
	}
	pv := "-"
	if n.ProtocolVersion != nil {
		pv = strconv.Itoa(*n.ProtocolVersion)
	}
	roles := "[]"
	if len(n.Roles) > 0 {
		b, _ := contract.Encode(n.Roles)
		roles = string(b)
	}
	return []string{n.ID, n.Liveness, seen, pv, dash(n.SoftwareVersion), roles}
}

// NodeColumns is the text header of node ls.
var NodeColumns = []string{"ID", "LIVENESS", "LAST_SEEN", "PROTOCOL_VERSION", "SOFTWARE_VERSION", "ROLES"}

// RenderNodes renders the header and one tab-separated row per node.
func RenderNodes(nodes []contract.Node) string {
	var b strings.Builder
	b.WriteString(joinFields(NodeColumns))
	for _, n := range nodes {
		b.WriteString(joinFields(nodeFields(n)))
	}
	return b.String()
}

// RenderNode renders six "label: value" lines in field order.
func RenderNode(n contract.Node) string {
	var b strings.Builder
	for i, v := range nodeFields(n) {
		b.WriteString([]string{"id", "liveness", "last_seen", "protocol_version", "software_version", "roles"}[i] + ": " + v + "\n")
	}
	return b.String()
}
