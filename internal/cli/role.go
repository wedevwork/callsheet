package cli

import (
	"context"
	"flag"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The role leaves (iteration 04): add, set, ls, show and rm over the
// verified client. Nothing is cached locally; manual paths and the adapter
// executable are never opened or resolved on the coordinator.
const (
	roleAddUsage  = "ID --name NAME --node NODE --adapter fake --instruction PATH --runbook PATH --model MODEL --effort EFFORT --concurrency N [--timeout DURATION] " + trustUsage + " [--json]"
	roleSetUsage  = "ID [--name NAME] [--adapter ID] [--instruction PATH] [--runbook PATH] [--model MODEL] [--effort EFFORT] [--concurrency N] [--timeout DURATION] " + trustUsage + " [--json]"
	roleLsUsage   = trustUsage + " [--json]"
	roleShowUsage = "ID " + trustUsage + " [--json]"
	roleRmUsage   = "ID [--force] " + trustUsage + " [--json]"

	roleNameHelp = "  --name NAME        logical role name (a slug; several roles may share it)\n"
	roleNodeHelp = "  --node NODE        the worker node ID (n_ + 32 hex digits; see callsheet node ls)\n"
	roleRestHelp = "  --adapter ID       the adapter: fake (test/demo adapter; never calls a model)\n" +
		"  --instruction PATH absolute path of the instruction manual on the worker node\n" +
		"  --runbook PATH     absolute path of the runbook manual on the worker node\n" +
		"  --model MODEL      model name passed to the adapter (free text, never inferred)\n" +
		"  --effort EFFORT    effort, one of the adapter's efforts (fake: low, medium, high)\n" +
		"  --concurrency N    concurrent tasks for this role, shared by all coordinators (1 or more)\n" +
		"  --timeout DURATION task timeout, e.g. 2h or 90m; 0 is unlimited (default 2h)\n"
	roleJSONHelp = "  --json             print the plane's response envelope as one JSON value\n"

	roleAddDetails = "Flags:\n" + roleNameHelp + roleNodeHelp + roleRestHelp + trustHelp + roleJSONHelp + "\n" +
		"Registers a role on the plane from any machine. The plane asks the node's sidecar to\n" +
		"check that both manuals are readable regular files there and that the adapter\n" +
		"executable can be invoked; manual contents never leave the node. An offline node, a\n" +
		"missing or unreadable manual or a disabled adapter rejects the role and nothing is\n" +
		"recorded. Role IDs are unique: repeating an add conflicts (read callsheet role show).\n" +
		"Flags may come before or after ID.\n\n" +
		"Example, on an operator machine:\n" +
		"  callsheet role add worker-a --name implementer --node n_0123456789abcdef0123456789abcdef \\\n" +
		"    --adapter fake --instruction /srv/manuals/instruction.md --runbook /srv/manuals/runbook.md \\\n" +
		"    --model \"example model\" --effort medium --concurrency 2 \\\n" +
		"    --plane https://plane.example:8443 --ca plane-ca.crt\n"
	roleSetDetails = "Flags:\n" + roleNameHelp + roleRestHelp + trustHelp + roleJSONHelp + "\n" +
		"Changes the given fields of a role; omitted fields keep their values. The ID, node\n" +
		"and registration order never change. The node revalidates the whole changed\n" +
		"configuration, so the node must be online. The change applies to future work only.\n"
	roleLsDetails = "Flags:\n" + trustHelp + roleJSONHelp + "\n" +
		"Lists every role as tab-separated columns NAME, ID, NODE, NODE_LIVENESS, ADAPTER,\n" +
		"MODEL (a JSON string), EFFORT, INFLIGHT, CONCURRENCY and CAN_ACCEPT, sorted by name,\n" +
		"then registration order: roles sharing a name are adjacent, in the order a pick by\n" +
		"name tries them. It reads the plane over verified TLS; nothing is cached.\n"
	roleShowDetails = "Flags:\n" + trustHelp + roleJSONHelp + "\n" +
		"Shows one role as label: value lines in field order; instruction, runbook and model\n" +
		"are JSON strings.\n"
	roleRmDetails = "Flags:\n" +
		"  --force            accepted for tasks in flight; with no tasks yet (this build) rm\n" +
		"                     and rm --force behave identically\n" +
		trustHelp + roleJSONHelp + "\n" +
		"Removes a role from the plane, also while its node is offline. It prints removed: ID.\n"
)

// roleFlags are a role leaf's own flags.
type roleFlags struct {
	name, node, adapter, instruction, runbook, model, effort, concurrency, timeout single
	force                                                                          boolFlag
}

func (rf *roleFlags) register(withNode, withForce bool) func(*flag.FlagSet) {
	return func(fs *flag.FlagSet) {
		fs.Var(&rf.name, "name", "")
		if withNode {
			fs.Var(&rf.node, "node", "")
		}
		fs.Var(&rf.adapter, "adapter", "")
		fs.Var(&rf.instruction, "instruction", "")
		fs.Var(&rf.runbook, "runbook", "")
		fs.Var(&rf.model, "model", "")
		fs.Var(&rf.effort, "effort", "")
		fs.Var(&rf.concurrency, "concurrency", "")
		fs.Var(&rf.timeout, "timeout", "")
		if withForce {
			fs.Var(&rf.force, "force", "")
		}
	}
}

func onlyForce(rf *roleFlags) func(*flag.FlagSet) {
	return func(fs *flag.FlagSet) { fs.Var(&rf.force, "force", "") }
}

// syntax parses the integer and duration flags; a malformed value is a
// usage error (ok=false with its code).
func (rf *roleFlags) syntax(c *Command, errOut io.Writer) (conc int, timeout time.Duration, code int, ok bool) {
	if rf.concurrency.set {
		n, valid := contract.ParseInteger(rf.concurrency.val)
		if !valid {
			return 0, 0, usageError(errOut, c, "--concurrency must be an integer"), false
		}
		conc = n
	}
	if rf.timeout.set {
		if strings.TrimSpace(rf.timeout.val) == "" {
			return 0, 0, usageError(errOut, c, "--timeout must be a Go duration such as 2h, 90m or 0"), false
		}
		if _, err := time.ParseDuration(rf.timeout.val); err != nil {
			return 0, 0, usageError(errOut, c, "--timeout must be a Go duration such as 2h, 90m or 0"), false
		}
		d, err := contract.ParseRoleTimeout(rf.timeout.val)
		if err != nil {
			return 0, 0, planeFail(errOut, err), false
		}
		timeout = d
	}
	return conc, timeout, 0, true
}

// roleOperand returns the single role ID operand.
func roleOperand(c *Command, ops []string, errOut io.Writer) (string, int, bool) {
	if len(ops) != 1 {
		return "", usageError(errOut, c, c.Path()+" takes exactly one role ID"), false
	}
	if !contract.ValidSlug(ops[0]) {
		return "", planeFail(errOut, contract.New(contract.CodeInvalidArgument, "invalid role ID; want a 1-63 character slug of lowercase letters, digits and internal hyphens")), false
	}
	return ops[0], 0, true
}

// roleClient validates trust syntax and returns a verified client.
func roleClient(ctx context.Context, f *remoteFlags, errOut io.Writer) (*client.Client, int, bool) {
	cl, err := nodeClient(ctx, f)
	if err != nil {
		return nil, planeFail(errOut, err), false
	}
	return cl, 0, true
}

func roleAdd(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	rf := &roleFlags{}
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, rf.register(true, false))
	if !ok {
		return code
	}
	id, code, ok := roleOperand(c, ops, errOut)
	if !ok {
		return code
	}
	for _, req := range []struct {
		name string
		v    *single
	}{{"--name", &rf.name}, {"--node", &rf.node}, {"--adapter", &rf.adapter}, {"--instruction", &rf.instruction}, {"--runbook", &rf.runbook}, {"--model", &rf.model}, {"--effort", &rf.effort}, {"--concurrency", &rf.concurrency}} {
		if !req.v.set {
			field := strings.TrimPrefix(req.name, "--")
			return planeFail(errOut, contract.RoleError(contract.CodeInvalidArgument, id, "", field, "", "%s is required: every role needs %s", req.name, field))
		}
	}
	conc, timeout, code, ok := rf.syntax(c, errOut)
	if !ok {
		return code
	}
	rc := contract.RoleConfig{ID: id, Name: rf.name.val, Node: rf.node.val, Adapter: rf.adapter.val, Instruction: rf.instruction.val,
		Runbook: rf.runbook.val, Model: rf.model.val, Effort: rf.effort.val, Concurrency: conc, Timeout: timeout, HasTimeout: rf.timeout.set}
	if err := contract.ValidateRoleConfig(rc, adapter.Lookup()); err != nil {
		return planeFail(errOut, err)
	}
	if _, err := f.trustSyntax(); err != nil {
		return planeFail(errOut, err)
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	v, err := cl.AddRole(ctx, rc)
	if err != nil {
		return planeFail(errOut, err)
	}
	return writeRole(out, errOut, f.json.val, v)
}

func roleSet(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	rf := &roleFlags{}
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, rf.register(false, false))
	if !ok {
		return code
	}
	id, code, ok := roleOperand(c, ops, errOut)
	if !ok {
		return code
	}
	conc, timeout, code, ok := rf.syntax(c, errOut)
	if !ok {
		return code
	}
	var p contract.RolePatch
	str := func(s *single) *string {
		if !s.set {
			return nil
		}
		v := s.val
		return &v
	}
	p.Name, p.Adapter, p.Instruction, p.Runbook, p.Model, p.Effort = str(&rf.name), str(&rf.adapter), str(&rf.instruction), str(&rf.runbook), str(&rf.model), str(&rf.effort)
	if rf.concurrency.set {
		p.Concurrency = &conc
	}
	if rf.timeout.set {
		p.Timeout = &timeout
	}
	if p.Empty() {
		return usageError(errOut, c, "role set needs at least one field flag to change")
	}
	if err := contract.ValidatePatch(p); err != nil {
		return planeFail(errOut, err)
	}
	if p.Adapter != nil {
		if _, known := adapter.Lookup()(*p.Adapter); !known {
			return planeFail(errOut, contract.RoleError(contract.CodeInvalidArgument, id, "", "adapter", "", "unknown adapter; registered adapters: fake"))
		}
	}
	if _, err := f.trustSyntax(); err != nil {
		return planeFail(errOut, err)
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	v, err := cl.SetRole(ctx, id, p)
	if err != nil {
		return planeFail(errOut, err)
	}
	return writeRole(out, errOut, f.json.val, v)
}

func roleLs(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, _, code, ok := parseRemote(c, args, true, false, true, 0, errOut)
	if !ok {
		return code
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	views, err := cl.ListRoles(ctx)
	if err != nil {
		return planeFail(errOut, err)
	}
	if f.json.val {
		b, err := contract.Encode(contract.RoleListResponse{Version: contract.ProtocolVersion, Roles: views})
		if err != nil {
			return planeFail(errOut, err)
		}
		return writeOut(out, errOut, string(b)+"\n")
	}
	return writeOut(out, errOut, RenderRoles(views))
}

func roleShow(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut)
	if !ok {
		return code
	}
	id, code, ok := roleOperand(c, ops, errOut)
	if !ok {
		return code
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	v, err := cl.ShowRole(ctx, id)
	if err != nil {
		return planeFail(errOut, err)
	}
	return writeRole(out, errOut, f.json.val, v)
}

func roleRm(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	rf := &roleFlags{}
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, onlyForce(rf))
	if !ok {
		return code
	}
	id, code, ok := roleOperand(c, ops, errOut)
	if !ok {
		return code
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	if err := cl.RemoveRole(ctx, id, rf.force.val); err != nil {
		return planeFail(errOut, err)
	}
	if f.json.val {
		b, err := contract.Encode(contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: id})
		if err != nil {
			return planeFail(errOut, err)
		}
		return writeOut(out, errOut, string(b)+"\n")
	}
	return writeOut(out, errOut, "removed: "+id+"\n")
}

// writeRole prints one role view as text or its JSON envelope.
func writeRole(out, errOut io.Writer, asJSON bool, v contract.RoleView) int {
	if asJSON {
		b, err := contract.Encode(contract.RoleResponse{Version: contract.ProtocolVersion, Role: v})
		if err != nil {
			return planeFail(errOut, err)
		}
		return writeOut(out, errOut, string(b)+"\n")
	}
	return writeOut(out, errOut, RenderRole(v))
}

// jsonString renders s as a JSON string literal (no HTML escaping), so
// arbitrary model and path text stays on one line.
func jsonString(s string) string {
	b, err := contract.Encode(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// RoleColumns is the text header of role ls.
var RoleColumns = []string{"NAME", "ID", "NODE", "NODE_LIVENESS", "ADAPTER", "MODEL", "EFFORT", "INFLIGHT", "CONCURRENCY", "CAN_ACCEPT"}

// RenderRoles renders the header and one tab-separated row per role, in
// the given (list) order; repeated names are adjacent rows.
func RenderRoles(views []contract.RoleView) string {
	var b strings.Builder
	b.WriteString(joinFields(RoleColumns))
	for _, v := range views {
		b.WriteString(joinFields([]string{v.Name, v.ID, v.Node, v.NodeLiveness, v.Adapter, jsonString(v.Model), v.Effort,
			strconv.Itoa(v.Inflight), strconv.Itoa(v.Concurrency), strconv.FormatBool(v.CanAccept)}))
	}
	return b.String()
}

// RenderRole renders one "label: value" line per RoleView field in wire
// order; instruction, runbook and model are JSON string literals.
func RenderRole(v contract.RoleView) string {
	var b strings.Builder
	r := v.Resolved()
	for _, kv := range [][2]string{
		{"id", r.ID}, {"name", r.Name}, {"node", r.Node}, {"adapter", r.Adapter},
		{"instruction", jsonString(r.Instruction)}, {"runbook", jsonString(r.Runbook)}, {"model", jsonString(r.Model)},
		{"effort", r.Effort}, {"concurrency", strconv.Itoa(r.Concurrency)}, {"timeout", r.Timeout.String()},
		{"registration_order", strconv.Itoa(v.RegistrationOrder)}, {"inflight", strconv.Itoa(v.Inflight)},
		{"can_accept", strconv.FormatBool(v.CanAccept)}, {"node_liveness", v.NodeLiveness},
		{"adapter_test_only", strconv.FormatBool(v.AdapterTestOnly)},
	} {
		b.WriteString(kv[0] + ": " + kv[1] + "\n")
	}
	return b.String()
}
