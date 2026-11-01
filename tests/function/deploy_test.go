package function

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/plane"
	"github.com/wedevwork/callsheet/internal/sidecar"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 12-3 function tests (design 12-3-deploy): one top-level TestDeploy*
// per FP over the shipped docs/deploy.md and docs/examples/deploy/. Validators
// read those files as untrusted data and never hand their text to a shell; the
// two lifecycle fixtures run the chapter's own argv on loopback, in test-owned
// temporary directories, with the fake adapter. They prove Callsheet's side of
// the procedures only: no service manager runs, nothing is installed, and no
// vendor authentication, boot behavior or usability is certified here.

const (
	deployDoc  = "docs/deploy.md"
	sampleDir  = "docs/examples/deploy/"
	planeUnit  = "callsheet-plane.service"
	workerUnit = "callsheet-sidecar.service"
	planePlist = "org.callsheet.plane.plist"
	sidePlist  = "org.callsheet.sidecar.plist"
	// The three shipped diagnostics the chapter quotes, verbatim from
	// internal/plane; the lifecycle fixtures compare them with real output.
	deployBindDiag    = "--bind %s differs from the configured bind %s; init never edits existing configuration: stop the plane and edit %s to change it"
	deployRootDiag    = "state directory %s has mode %04o; it must not be accessible by group or others (chmod 700; permissions are never repaired automatically)"
	deployKeyDiag     = "private key %s has mode %04o; it must be owner-only (0600 or 0400; permissions are never repaired automatically)"
	deployIncomplete  = "Preserve the directory and restore a complete stopped backup, or choose a fresh --state-dir"
	deployUnknownExec = "node reported an execution unknown to this plane"
	deployUntrusted   = "connection not trusted"
	deployExitTrust   = 6
	deployExitStopped = 130
	// deployEmptyTaskList is task ls's plain rendering of no tasks: its header.
	deployEmptyTaskList = "TASK_ID\tSTATE\tROLE_ID\tNODE\tELAPSED_MS\tRECONCILING\n"
)

var deployMarker = regexp.MustCompile(`^<!-- deploy: ([a-z0-9-]+) -->$`)

// deployFenceRow is one marked fence: its shared spec and the only variables it
// may use. A row with no leaf is a service-manager, account or file-management
// fence, narrowly parsed by parseDeployFence.
type deployFenceRow struct {
	docFenceSpec
	vars []string
}

func callsheetRow(id string, leaves []string, vars ...string) deployFenceRow {
	return deployFenceRow{docFenceSpec{id, "sh", leaves}, vars}
}

func operationRow(id string, vars ...string) deployFenceRow {
	return deployFenceRow{docFenceSpec{id, "sh", nil}, vars}
}

// deployFences is the chapter's pinned inventory.
var deployFences = []deployFenceRow{
	callsheetRow("state-status", []string{"plane status"}, "STATE"),
	operationRow("ca-mode", "CA"),
	callsheetRow("ca-enroll", []string{"sidecar enroll"}, "PLANE", "CA", "SIDECAR_STATE"),
	callsheetRow("ca-enroll-pinned", []string{"sidecar enroll"}, "PLANE", "FINGERPRINT", "SIDECAR_STATE"),
	operationRow("linux-accounts"),
	operationRow("install-binary", "CALLSHEET"),
	operationRow("login-plane"),
	operationRow("plane-dirs", "HOME"),
	operationRow("plane-env", "HOME", "STATE", "PLANE_IP", "PLANE"),
	callsheetRow("plane-init", []string{"plane init"}, "STATE", "PLANE_IP"),
	operationRow("login-worker"),
	operationRow("worker-dirs", "HOME"),
	operationRow("worker-env", "HOME", "SIDECAR_STATE", "CA", "PLANE_IP", "PLANE", "WORKER"),
	operationRow("linux-check"),
	operationRow("linux-lint"),
	operationRow("linux-install"),
	operationRow("linux-start-plane"),
	callsheetRow("verify-plane", []string{"node ls"}, "PLANE", "CA"),
	callsheetRow("worker-enroll", []string{"sidecar enroll"}, "SIDECAR_STATE", "PLANE", "CA"),
	operationRow("linux-start-sidecar"),
	callsheetRow("verify-roster", []string{"node ls", "role ls"}, "PLANE", "CA"),
	operationRow("linux-logs"),
	operationRow("linux-stop"),
	operationRow("linux-start"),
	operationRow("linux-uninstall"),
	operationRow("macos-logs-plane", "HOME"),
	operationRow("macos-logs-worker", "HOME"),
	operationRow("macos-check"),
	operationRow("macos-install"),
	operationRow("macos-start-plane"),
	operationRow("macos-start-sidecar"),
	operationRow("macos-stop"),
	operationRow("macos-remove"),
	operationRow("macos-read-logs", "HOME"),
	operationRow("confirm-exit"),
	callsheetRow("address-status", []string{"plane status"}, "STATE"),
	callsheetRow("address-reissue", []string{"plane cert reissue"}, "STATE", "PLANE_IP"),
	callsheetRow("address-verify-ca", []string{"plane status"}, "STATE"),
	callsheetRow("address-node-list", []string{"node ls"}, "PLANE", "CA"),
	callsheetRow("address-reenroll", []string{"sidecar enroll"}, "SIDECAR_STATE", "PLANE", "CA"),
	operationRow("recover-plane-paths", "HOME", "STATE", "OLD_STATE"),
	operationRow("recover-plane-mount", "HOME", "STATE"),
	operationRow("recover-plane-move", "STATE", "OLD_STATE"),
	operationRow("backup-env", "HOME", "STATE", "BACKUP_STATE"),
	operationRow("backup-plane", "STATE", "BACKUP_STATE"),
	operationRow("restore-select", "HOME", "BACKUP_STATE"),
	operationRow("restore-backup", "STATE", "BACKUP_STATE"),
	callsheetRow("restore-status", []string{"plane status"}, "STATE"),
	operationRow("fresh-env", "PLANE_IP", "PLANE"),
	callsheetRow("fresh-init", []string{"plane init"}, "STATE", "PLANE_IP"),
	callsheetRow("fresh-plane-run", []string{"plane run"}, "STATE"),
	operationRow("recover-sidecar-paths", "HOME", "SIDECAR_STATE", "OLD_SIDECAR_STATE"),
	operationRow("recover-sidecar-mount", "HOME", "SIDECAR_STATE"),
	operationRow("recover-sidecar-move", "SIDECAR_STATE", "OLD_SIDECAR_STATE"),
	operationRow("fresh-ca-mode", "HOME", "CA", "PLANE_IP", "PLANE"),
	callsheetRow("fresh-enroll", []string{"sidecar enroll"}, "SIDECAR_STATE", "PLANE", "CA"),
	callsheetRow("fresh-sidecar-run", []string{"sidecar run"}, "SIDECAR_STATE", "WORKER"),
	callsheetRow("fresh-task-list", []string{"task ls"}, "PLANE", "CA"),
}

func deployRow(id string) (deployFenceRow, bool) {
	i := slices.IndexFunc(deployFences, func(r deployFenceRow) bool { return r.id == id })
	if i < 0 {
		return deployFenceRow{}, false
	}
	return deployFences[i], true
}

// deployMoveFences are the only fences that may hold guard and move operations,
// and deployCopyFences the only ones that may copy state.
var (
	deployMoveFences = []string{"recover-plane-move", "recover-sidecar-move"}
	deployCopyFences = []string{"backup-plane", "restore-backup"}
)

// parseDeployFences returns every fence in the chapter.
func parseDeployFences(md string) ([]qsFence, error) { return parseMarkedFences(md, deployMarker) }

// parseDeployFence tokenizes one fence with the shared tokenizer. Only the
// guarded move fences may use a trailing && and a "test !"; anything else that
// the tokenizer does not know fails closed. Every non-Callsheet line must be a
// known literal operation, and nothing is ever evaluated by a shell.
func parseDeployFence(f qsFence) (qsParsed, error) {
	var p qsParsed
	body := f.body
	if slices.Contains(deployMoveFences, f.id) {
		lines := strings.Split(body, "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "test ") || strings.HasPrefix(strings.TrimSpace(l), "mv ") {
				l = strings.TrimSuffix(l, " &&")
			}
			lines[i] = strings.Replace(l, "test ! ", `test "!" `, 1)
		}
		body = strings.Join(lines, "\n")
	}
	cmds, err := tokenizeShell(f.id, body, f.line+1, false)
	if err != nil {
		return p, err
	}
	row, known := deployRow(f.id)
	for _, c := range cmds {
		if first, ok := c.words[0].literal(); ok && first == "callsheet" {
			p.callsheet = append(p.callsheet, c)
			continue
		}
		// A Callsheet row is pinned by its argv alone, so it may hold nothing else.
		if known && len(row.leaves) > 0 {
			return p, fmt.Errorf("fence %s line %d: a Callsheet-only fence holds a setup operation", f.id, c.line)
		}
		if err := checkDeployOperation(f.id, c); err != nil {
			return p, err
		}
		if known {
			for _, w := range c.words {
				for _, part := range w.parts {
					if part.variable != "" && !slices.Contains(row.vars, part.variable) {
						return p, fmt.Errorf("fence %s line %d: unknown variable $%s", f.id, c.line, part.variable)
					}
				}
			}
			if name := deployAssigned(c); name != "" && !slices.Contains(row.vars, name) {
				return p, fmt.Errorf("fence %s line %d: unknown variable $%s", f.id, c.line, name)
			}
		}
		p.setup = append(p.setup, c)
	}
	return p, nil
}

// deployAssigned names the variable an assignment or read sets, or "".
func deployAssigned(c qsCommand) string {
	first, _ := c.words[0].literal()
	if first == "read" {
		if len(c.words) != 3 {
			return ""
		}
		name, _ := c.words[2].literal()
		return name
	}
	if len(c.words) == 1 && len(c.words[0].parts) > 0 && c.words[0].parts[0].variable == "" && qsAssignment.MatchString(c.words[0].parts[0].text) {
		name, _, _ := strings.Cut(c.words[0].parts[0].text, "=")
		return name
	}
	return ""
}

// deploySudoTools are the only programs a chapter fence may run as administrator.
var deploySudoTools = []string{"useradd", "install", "systemctl", "journalctl", "launchctl", "rm"}

// checkDeployOperation accepts only the chapter's literal operations.
func checkDeployOperation(id string, c qsCommand) error {
	bad := func(what string) error {
		return fmt.Errorf("fence %s line %d: unrecognized setup operation: %s", id, c.line, what)
	}
	if name := deployAssigned(qsCommand{words: c.words[:1]}); name != "" && len(c.words) == 1 {
		return nil
	}
	first, ok := c.words[0].literal()
	if !ok {
		if len(c.words) == 2 && len(c.words[0].parts) == 1 && c.words[0].parts[0].variable == "WORKER" {
			if v, _ := c.words[1].literal(); v == "--version" {
				return nil
			}
		}
		return bad("a variable command")
	}
	flag := func(i int) string {
		if i >= len(c.words) {
			return ""
		}
		v, _ := c.words[i].literal()
		return v
	}
	n := len(c.words)
	switch first {
	case "read":
		if n != 3 || flag(1) != "-r" {
			return bad("read takes -r NAME")
		}
	case "mkdir":
		if n != 4 || flag(1) != "-m" || flag(2) != "700" {
			return bad("mkdir takes -m 700 DIRECTORY")
		}
	case "chmod":
		if n != 3 || flag(1) != "600" {
			return bad("chmod takes 600 PATH")
		}
	case "touch":
		if n != 2 {
			return bad("touch takes one path")
		}
	case "test":
		if !slices.Contains(deployMoveFences, id) || (n != 3 && n != 4) {
			return bad("test outside a guarded move")
		}
	case "mv":
		if !slices.Contains(deployMoveFences, id) || n != 3 {
			return bad("mv outside a guarded move")
		}
	case "printf":
		if !slices.Contains(deployMoveFences, id) || n != 2 {
			return bad("printf outside a guarded move")
		}
	case "cp":
		if !slices.Contains(deployCopyFences, id) || n != 4 || flag(1) != "-Rp" {
			return bad("cp outside a stopped backup or restore")
		}
	case "df":
		if n != 4 || flag(1) != "-P" {
			return bad("df takes -P STATE HOME")
		}
	case "grep":
		if n != 4 || flag(1) != "-n" || flag(2) != "REPLACE_WITH" {
			return bad("grep takes -n REPLACE_WITH PATH")
		}
	case "plutil":
		if n != 3 || flag(1) != "-lint" {
			return bad("plutil takes -lint PATH")
		}
	case "systemd-analyze":
		if n < 3 || flag(1) != "verify" {
			return bad("systemd-analyze takes verify PATH...")
		}
	case "tail":
		if n != 4 || flag(1) != "-n" {
			return bad("tail takes -n COUNT PATH")
		}
	case "ps":
		if n != 3 || flag(1) != "-u" {
			return bad("ps takes -u ACCOUNT")
		}
	case "sudo":
		if flag(1) == "-iu" && n == 3 {
			return nil
		}
		if !slices.Contains(deploySudoTools, flag(1)) {
			return bad("sudo " + flag(1))
		}
	default:
		return bad(first)
	}
	return nil
}

func deployFence(md, id string) (qsFence, error) {
	fences, err := parseDeployFences(md)
	if err != nil {
		return qsFence{}, err
	}
	f, ok := fenceByID(fences, id)
	if !ok {
		return qsFence{}, fmt.Errorf("fence %s is missing", id)
	}
	return f, nil
}

// deployArgs returns fence id's single Callsheet command, variables as $NAME.
func deployArgs(md, id string) ([]string, error) {
	f, err := deployFence(md, id)
	if err != nil {
		return nil, err
	}
	p, err := parseDeployFence(f)
	if err != nil {
		return nil, err
	}
	if len(p.callsheet) != 1 {
		return nil, fmt.Errorf("fence %s holds %d Callsheet commands, want 1", id, len(p.callsheet))
	}
	return renderWords(p.callsheet[0].words), nil
}

func wantDeployArgs(md, id string, want ...string) error {
	got, err := deployArgs(md, id)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("fence %s = %q, want %q", id, got, want)
	}
	return nil
}

func wantFenceBody(md, id, want string) error {
	f, err := deployFence(md, id)
	if err != nil {
		return err
	}
	if f.body != want {
		return fmt.Errorf("fence %s = %q, want %q", id, f.body, want)
	}
	return nil
}

// inOrder requires each of steps in text, in order, whitespace normalized.
func inOrder(where, text string, steps ...string) error {
	norm := normSpace(text)
	at := -1
	for _, s := range steps {
		i := strings.Index(norm, normSpace(s))
		if i < 0 {
			return fmt.Errorf("%s: lacks step %q", where, s)
		}
		if i < at {
			return fmt.Errorf("%s: step %q is out of order", where, s)
		}
		at = i
	}
	return nil
}

func forbidAll(where, text string, forbidden ...string) error {
	lower := strings.ToLower(normSpace(text))
	for _, f := range forbidden {
		if strings.Contains(lower, strings.ToLower(f)) {
			return fmt.Errorf("%s: forbidden %q", where, f)
		}
	}
	return nil
}

// deployH2 are the chapter's stable headings, in order.
var deployH2 = []string{"Before you deploy", "Security posture", "State and certificates", "Give clients the CA", "Run as services", "Change the plane address", "Recover after CA loss", "Connection not trusted", "Verify the deployment"}

// deployPaths maps each documented state path to its distribution rule.
var deployPaths = []struct {
	rel, rule string
	mode      fs.FileMode
}{
	{"pki/ca.crt", "the only file copied to clients", 0o600},
	{"pki/ca.key", "never leaves the plane machine", 0o600},
	{"pki/server.crt", "stays on the plane", 0o600},
	{"pki/server.key", "never leaves the plane machine", 0o600},
}

// deployDefaults are the documented default state directories, checked against
// the real resolvers by TestDeployTrustAndSecurity/layout.
var deployDefaults = []string{
	"`$XDG_STATE_HOME/callsheet/plane` when `XDG_STATE_HOME` is set and absolute",
	"otherwise `$HOME/.local/state/callsheet/plane`",
	"A relative `XDG_STATE_HOME` is an error.",
	"`$HOME/Library/Application Support/callsheet/plane`, ignoring `XDG_STATE_HOME`",
	"`--state-dir` overrides these; quote shell paths.",
	"Sidecar defaults are the corresponding paths ending in `sidecar`.",
	"The service samples deliberately use explicit paths",
}

// checkDeployLayout is FP-1's layout validator.
func checkDeployLayout(md string) error {
	d, err := parseReadme(md)
	if err != nil {
		return err
	}
	var names []string
	for _, s := range d.sections {
		names = append(names, s.name)
	}
	if !slices.Equal(names, deployH2) {
		return fmt.Errorf("layout: H2 headings %q, want %q", names, deployH2)
	}
	body, err := d.section("State and certificates")
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range deployPaths {
		found := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "| `"+p.rel+"` |") {
				found = true
				if !strings.Contains(line, p.rule) {
					errs = append(errs, fmt.Errorf("layout: row %s lacks %q", p.rel, p.rule))
				}
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("layout: no row for %s", p.rel))
		}
	}
	errs = append(errs, needAll("layout", body, deployDefaults...))
	errs = append(errs, needAll("layout", body,
		"`STATE` below is the selected absolute plane state directory.", "| `config.json` |",
		"Both private keys stay on the plane machine and out of git, logs and example files.", "it is not the CA",
		"State and PKI directories are owner-only 0700.", "Keys must be 0600 or 0400; generated files are 0600.",
		"must not be group- or other-writable", "Use 0600 for distributed CA copies", "Never make the plane directory readable to the worker",
		"It is not service health."))
	errs = append(errs, wantDeployArgs(md, "state-status", "callsheet", "plane", "status", "--state-dir", "$STATE"))
	verify, err := d.section("Verify the deployment")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	errs = append(errs, needAll("layout", stripInlineLinks(verify),
		"They do not certify vendor authentication, host boot behavior, service-manager operation or usability.", "Time your first dispatch and record every snag.",
		"| OS and architecture | Callsheet and vendor versions | Service account | Start and end time | Action | Expected result | Observed result | Snag |",
		"|  |  |  |  |  |  |  |  |"))
	if strings.Contains(verify, "- [x]") || strings.Contains(strings.ToLower(verify), "| pass") {
		errs = append(errs, errors.New("layout: the evidence checklist is prefilled"))
	}
	return errors.Join(errs...)
}

// checkDeployDistribution is FP-1's distribution validator.
func checkDeployDistribution(md string) error {
	body, err := sectionOf(md, "Give clients the CA")
	if err != nil {
		return err
	}
	var errs []error
	errs = append(errs, needAll("distribution", stripInlineLinks(body),
		"Transfer only `pki/ca.crt`", "the plane operator sends it", "owned by that account", "through an independently trusted channel",
		"**Sidecars** enroll with the copy", "Enrollment embeds the CA bytes in `enrollment.json`, so replacing the original copied certificate alone does not update enrollment",
		"It is mutually exclusive with `--ca` and requires a fingerprint you received independently; there is no unauthenticated trust-on-first-use.",
		"**Coordinator and operator machines** each receive their own CA copy and the plane URL.",
		"Never copy a worker credential store or `ca.key` to a coordinator.", "it is not a fifth service"))
	errs = append(errs, forbidAll("distribution", body, "copy `ca.key` to", "distribute `ca.key`", "distribute `pki/ca.key`", "copy `pki/ca.key`", "trusted automatically", "updates enrollment"))
	errs = append(errs, wantDeployArgs(md, "ca-enroll", "callsheet", "sidecar", "enroll", "--plane", "$PLANE", "--ca", "$CA", "--state-dir", "$SIDECAR_STATE"))
	errs = append(errs, wantDeployArgs(md, "ca-enroll-pinned", "callsheet", "sidecar", "enroll", "--plane", "$PLANE", "--ca-fingerprint", "$FINGERPRINT", "--state-dir", "$SIDECAR_STATE"))
	errs = append(errs, wantFenceBody(md, "ca-mode", `chmod 600 "$CA"`))
	links := markdownLinks(body)
	for _, l := range []string{"coordinator.md#claude-code", "coordinator.md#openai-codex", "coordinator.md#grok-build", "coordinator.md#cursor-agent"} {
		if !slices.Contains(links, l) {
			errs = append(errs, fmt.Errorf("distribution: missing link %s", l))
		}
	}
	return errors.Join(errs...)
}

// checkDeployPosture is FP-1's posture validator.
func checkDeployPosture(md string) error {
	before, err := sectionOf(md, "Before you deploy")
	if err != nil {
		return err
	}
	body, err := sectionOf(md, "Security posture")
	if err != nil {
		return err
	}
	var errs []error
	errs = append(errs, needAll("posture", stripInlineLinks(before),
		"exactly one plane, on Linux or macOS", "**same Callsheet version**", "an installed and authenticated eligible vendor CLI on each worker",
		"a private LAN of trusted peers or, preferably, a VPN restricted to those peers", "Callsheet bundles no VPN and has no SaaS account of its own.",
		"Your vendor accounts may still be required.", "not a hostname, a wildcard or a public address", "whose host is one of the server certificate's SANs",
		"Only trusted peers may reach the plane's port", "do not open the port globally", "Sidecars connect out and have no inbound listener."))
	if !slices.Contains(markdownLinks(before), "quickstart.md#install-callsheet") {
		errs = append(errs, errors.New("posture: no link to the quickstart installation"))
	}
	errs = append(errs, needAll("posture", body,
		"TLS protects confidentiality but provides no client identity or authorization. Any reachable peer holding the CA can dispatch work and read or write every workspace. "+
			"Containment is whatever the vendor sandbox provides, which often does not block host reads. Run sidecars as a dedicated OS user. "+
			"Callsheet adds no sandbox in v1; a Callsheet-side sandbox is planned as E8.",
		"A CA certificate is public trust material, not an access token.",
		"A dedicated user reduces accessible files through OS permissions but does not make world-readable host files private."))
	for _, l := range []string{"real-adapters.md#enabling-a-worker", "support-catalog.md"} {
		if !slices.Contains(markdownLinks(body), l) {
			errs = append(errs, fmt.Errorf("posture: missing link %s", l))
		}
	}
	errs = append(errs, forbidAll("posture", before+body, "no vendor account is needed", "includes a vpn", "bundled vpn", "approvals enforce", "ufw ", "iptables", "firewall-cmd"))
	return errors.Join(errs...)
}

// checkDeployNotTrusted is FP-1's not-trusted validator.
func checkDeployNotTrusted(md string) error {
	body, err := sectionOf(md, "Connection not trusted")
	if err != nil {
		return err
	}
	var errs []error
	steps := orderedSteps(body)
	if len(steps) != 3 {
		return fmt.Errorf("not-trusted: %d numbered checks, want 3", len(steps))
	}
	for i, wants := range [][]string{
		{"hostname or IP against the server SANs", "offline `plane status`"},
		{"exists, is readable by the actual process user and belongs to this plane", "Re-enroll sidecars whose embedded CA is stale"},
		{"client and plane clocks", "certificate validity"},
	} {
		for _, w := range wants {
			if !strings.Contains(normSpace(steps[i].text), w) {
				errs = append(errs, fmt.Errorf("not-trusted: check %d lacks %q", i+1, w))
			}
		}
	}
	errs = append(errs, needAll("not-trusted", stripInlineLinks(body),
		"Every trust failure carries the phrase `"+deployUntrusted+"`", "A missing or wrong CA and a name mismatch can therefore produce the same diagnostic",
		"Use `plane cert reissue` for the server certificate under a valid surviving CA.", "CA loss or an expired CA goes to Recover after CA loss.",
		"Never disable verification, which Callsheet does not offer, and never copy private keys."))
	rest := strings.ReplaceAll(normSpace(body), "Never disable verification, which Callsheet does not offer", "")
	errs = append(errs, forbidAll("not-trusted", rest, "insecure", "disable verification", "skip verification", "copy the private key"))
	return errors.Join(errs...)
}

// TestDeployTrustAndSecurity (FP-1): the chapter's four trust subcontracts, each
// rejecting its own mutations.
func TestDeployTrustAndSecurity(t *testing.T) {
	md := string(repoFile(t, deployDoc))
	const secState, secBefore, secClients, secPosture = "State and certificates", "Before you deploy", "Give clients the CA", "Security posture"
	t.Run("layout", func(t *testing.T) {
		if err := checkDeployLayout(md); err != nil {
			t.Fatalf("shipped chapter: %v", err)
		}
		// The documented paths are the ones a real init reports and creates.
		p := newNodeCLI(t)
		state := filepath.Join(t.TempDir(), "callsheet", "plane")
		if r := p.run(t, "plane", "init", "--state-dir", state, "--bind", "127.0.0.1:0", "--san", "127.0.0.1"); r.code != 0 {
			t.Fatalf("plane init = %+v", r)
		}
		status := p.run(t, "plane", "status", "--state-dir", state)
		if status.code != 0 {
			t.Fatalf("plane status = %+v", status)
		}
		for key, rel := range map[string]string{"ca_cert_path": "pki/ca.crt", "ca_key_path": "pki/ca.key", "server_cert_path": "pki/server.crt", "server_key_path": "pki/server.key"} {
			if !hasLine(status.stdout, key+": "+filepath.Join(state, filepath.FromSlash(rel))) {
				t.Errorf("status lacks %s at %s:\n%s", key, rel, status.stdout)
			}
		}
		for _, f := range deployPaths {
			st, err := os.Stat(filepath.Join(state, filepath.FromSlash(f.rel)))
			if err != nil || st.Mode().Perm() != f.mode {
				t.Errorf("%s: %v, %v", f.rel, st, err)
			}
		}
		if st, err := os.Stat(filepath.Join(state, "config.json")); err != nil || st.Mode().Perm()&0o022 != 0 {
			t.Errorf("config.json: %v, %v", st, err)
		}
		for _, rel := range []string{".", "pki"} {
			if st, err := os.Stat(filepath.Join(state, rel)); err != nil || st.Mode().Perm() != 0o700 {
				t.Errorf("%s: %v, %v", rel, st, err)
			}
		}
		// The documented defaults are the real resolvers', without touching HOME.
		const home, xdg = "/deploy-home", "/deploy-xdg"
		resolvers := map[string]func(goos, override, home, xdg string) (string, error){"plane": plane.ResolveStateDir, "sidecar": sidecar.ResolveStateDir}
		for name, resolve := range resolvers {
			for _, c := range []struct {
				goos, override, home, xdg, want string
			}{
				{"linux", "", home, "", home + "/.local/state/callsheet/" + name},
				{"linux", "", home, xdg, xdg + "/callsheet/" + name},
				{"darwin", "", home, "", home + "/Library/Application Support/callsheet/" + name},
				{"darwin", "", home, xdg, home + "/Library/Application Support/callsheet/" + name},
				{"linux", "/explicit", home, xdg, "/explicit"},
				{"darwin", "/explicit", home, xdg, "/explicit"},
			} {
				if got, err := resolve(c.goos, c.override, c.home, c.xdg); err != nil || got != c.want {
					t.Errorf("%s %+v = %q, %v", name, c, got, err)
				}
			}
			if _, err := resolve("linux", "", home, "relative"); err == nil {
				t.Errorf("%s: a relative XDG_STATE_HOME was accepted", name)
			}
		}
		runFPMutations(t, md, checkDeployLayout, []readmeMutation{
			{"rename-ca-crt", inSection(secState, "| `pki/ca.crt` |", "| `pki/ca.pem` |"), "layout: no row for pki/ca.crt"},
			{"rename-ca-key", inSection(secState, "| `pki/ca.key` |", "| `pki/ca-key.pem` |"), "layout: no row for pki/ca.key"},
			{"rename-server-crt", inSection(secState, "| `pki/server.crt` |", "| `pki/tls.crt` |"), "layout: no row for pki/server.crt"},
			{"rename-server-key", inSection(secState, "| `pki/server.key` |", "| `pki/tls.key` |"), "layout: no row for pki/server.key"},
			{"distribute-key-rule", inSection(secState, "| `pki/ca.key` | the CA private key | never leaves the plane machine |", "| `pki/ca.key` | the CA private key | copy to clients |"), "layout: row pki/ca.key lacks"},
			{"wrong-linux-default", inSection(secState, "`$HOME/.local/state/callsheet/plane`", "`$HOME/.callsheet/plane`"), "layout: lacks \"otherwise"},
			{"wrong-xdg-semantics", inSection(secState, "A relative `XDG_STATE_HOME` is an error.", "A relative `XDG_STATE_HOME` is resolved from the working directory."), "layout: lacks \"A relative"},
			{"wrong-macos-default", inSection(secState, "$HOME/Library/Application Support/callsheet/plane", "$HOME/.local/state/callsheet/plane"), "layout: lacks \"`$HOME/Library"},
			{"macos-honors-xdg", inSection(secState, ", ignoring `XDG_STATE_HOME`", ""), "layout: lacks \"`$HOME/Library"},
			{"missing-explicit-override", inSection(secState, "`--state-dir` overrides these; quote shell paths.", ""), "layout: lacks \"`--state-dir` overrides"},
			{"status-is-health", inSection(secState, "It is not service health.", "It shows service health."), "layout: lacks \"It is not service health.\""},
			{"open-worker-read", inSection(secState, "Never make the plane directory readable to the worker", "Make the plane directory readable to the worker"), "layout: lacks \"Never make"},
			{"renamed-heading", replaceOnce("## Connection not trusted\n", "## Trust problems\n"), "layout: H2 headings"},
			{"setup-op-in-status", inSection(secState, "callsheet plane status --state-dir \"$STATE\"", "callsheet plane status --state-dir \"$STATE\"\nsudo rm /tmp/reviewer-example"), "a Callsheet-only fence holds a setup operation"},
			{"prefilled-checklist", inSection("Verify the deployment", "- [ ] Check logs", "- [x] Check logs"), "layout: the evidence checklist is prefilled"},
			{"prefilled-evidence", inSection("Verify the deployment", "|  |  |  |  |  |  |  |  |", "| Linux | v1 | worker | now | start | ok | pass |  |"), "layout: lacks"},
			{"m5-certified", inSection("Verify the deployment", "They do not certify vendor authentication", "They certify vendor authentication"), "layout: lacks \"They do not certify"},
		})
	})
	t.Run("distribution", func(t *testing.T) {
		if err := checkDeployDistribution(md); err != nil {
			t.Fatalf("shipped chapter: %v", err)
		}
		runFPMutations(t, md, checkDeployDistribution, []readmeMutation{
			{"distribute-ca-key", inSection(secClients, "Transfer only `pki/ca.crt`", "Transfer `pki/ca.key`"), "distribution: lacks \"Transfer only"},
			{"appended-key-copy", appendToSection(secClients, "Also copy `pki/ca.key` to each worker."), "distribution: forbidden"},
			{"omit-sidecars", inSection(secClients, "**Sidecars** enroll with the copy", "Enroll with the copy"), "distribution: lacks \"**Sidecars**"},
			{"omit-coordinator-class", inSection(secClients, "**Coordinator and operator machines** each receive their own CA copy and the plane URL.", ""), "distribution: lacks \"**Coordinator"},
			{"omit-claude-link", inSection(secClients, "(coordinator.md#claude-code)", "(coordinator.md)"), "distribution: missing link coordinator.md#claude-code"},
			{"omit-codex-link", inSection(secClients, "(coordinator.md#openai-codex)", "(coordinator.md)"), "distribution: missing link coordinator.md#openai-codex"},
			{"omit-grok-link", inSection(secClients, "(coordinator.md#grok-build)", "(coordinator.md)"), "distribution: missing link coordinator.md#grok-build"},
			{"omit-cursor-link", inSection(secClients, "(coordinator.md#cursor-agent)", "(coordinator.md)"), "distribution: missing link coordinator.md#cursor-agent"},
			{"copy-updates-enrollment", inSection(secClients, "so replacing the original copied certificate alone does not update enrollment", "so replacing the copied certificate updates enrollment"), "distribution: lacks \"Enrollment embeds"},
			{"unpinned-fetch", inSection(secClients, "there is no unauthenticated trust-on-first-use", "the first fetch is trusted automatically"), "distribution: lacks \"It is mutually exclusive"},
			{"not-exclusive", inSection(secClients, "It is mutually exclusive with `--ca` and", "It can be combined with `--ca` and"), "distribution: lacks \"It is mutually exclusive"},
			{"both-trust-flags", inSection(secClients, `--ca-fingerprint "$FINGERPRINT"`, `--ca "$CA" --ca-fingerprint "$FINGERPRINT"`), "fence ca-enroll-pinned ="},
		})
	})
	t.Run("posture", func(t *testing.T) {
		if err := checkDeployPosture(md); err != nil {
			t.Fatalf("shipped chapter: %v", err)
		}
		runFPMutations(t, md, checkDeployPosture, []readmeMutation{
			{"remove-no-authorization", inSection(secPosture, "provides no client identity or authorization", "authenticates every client"), "posture: lacks \"TLS protects"},
			{"remove-reachable-peer", inSection(secPosture, "Any reachable peer holding the CA can dispatch work and read or write every workspace. ", ""), "posture: lacks \"TLS protects"},
			{"remove-host-read", inSection(secPosture, ", which often does not block host reads", ""), "posture: lacks \"TLS protects"},
			{"remove-e8", inSection(secPosture, "; a Callsheet-side sandbox is planned as E8", ""), "posture: lacks \"TLS protects"},
			{"remove-dedicated-user", inSection(secPosture, "Run sidecars as a dedicated OS user. ", ""), "posture: lacks \"TLS protects"},
			{"ca-is-token", inSection(secPosture, "public trust material, not an access token", "an access token"), "posture: lacks \"A CA certificate"},
			{"bundled-vpn", inSection(secBefore, "Callsheet bundles no VPN and has no SaaS account of its own.", "Callsheet includes a VPN."), "posture: lacks \"Callsheet bundles no VPN"},
			{"no-vendor-account", inSection(secBefore, "Your vendor accounts may still be required.", "No vendor account is needed."), "posture: lacks \"Your vendor accounts"},
			{"appended-vpn-claim", appendToSection(secBefore, "A bundled VPN handles this."), "posture: forbidden \"bundled vpn\""},
			{"no-network-boundary", inSection(secBefore, "a private LAN of trusted peers or, preferably, a VPN restricted to those peers", "any network"), "posture: lacks \"a private LAN"},
			{"global-port", inSection(secBefore, "Only trusted peers may reach the plane's port", "Everyone may reach the plane's port"), "posture: lacks \"Only trusted peers"},
			{"firewall-command", appendToSection(secBefore, "Run ufw allow 8443."), "posture: forbidden \"ufw \""},
			{"approvals-confine", appendToSection(secPosture, "Model approvals enforce confinement."), "posture: forbidden \"approvals enforce\""},
		})
	})
	t.Run("not-trusted", func(t *testing.T) {
		if err := checkDeployNotTrusted(md); err != nil {
			t.Fatalf("shipped chapter: %v", err)
		}
		const sec = "Connection not trusted"
		runFPMutations(t, md, checkDeployNotTrusted, []readmeMutation{
			{"remove-check", inSectionRe(sec, `(?m)^3\. .*\n`, ""), "not-trusted: 2 numbered checks, want 3"},
			{"swap-checks", inSectionRe(sec, `(?m)^1\. (.*)\n2\. (.*)\n`, "1. $2\n2. $1\n"), "not-trusted: check 1 lacks"},
			{"omit-process-user", inSection(sec, "is readable by the actual process user and ", ""), "not-trusted: check 2 lacks"},
			{"omit-stale-enrollment", inSection(sec, " Re-enroll sidecars whose embedded CA is stale.", ""), "not-trusted: check 2 lacks"},
			{"omit-clock", inSection(sec, "The client and plane clocks, and certificate validity.", "The weather."), "not-trusted: check 3 lacks"},
			{"insecure-advice", appendToSection(sec, "Otherwise pass an insecure flag."), "not-trusted: forbidden \"insecure\""},
			{"skip-verification", appendToSection(sec, "Otherwise skip verification."), "not-trusted: forbidden \"skip verification\""},
			{"remove-never", inSection(sec, "Never disable verification, which Callsheet does not offer, and never copy private keys.", "Disable verification if needed."), "not-trusted: lacks \"Never disable"},
			{"same-diagnostic", inSection(sec, "A missing or wrong CA and a name mismatch can therefore produce the same diagnostic", "Each cause has its own diagnostic"), "not-trusted: lacks"},
		})
	})
}

// checkDeployAddressDoc is FP-2's static validator.
func checkDeployAddressDoc(md string) error {
	body, err := sectionOf(md, "Change the plane address")
	if err != nil {
		return err
	}
	text := stripInlineLinks(body)
	var errs []error
	errs = append(errs, needAll("address", text,
		"This keeps the CA and the node identities.", "not the fresh-plane branch", "they are not shared between shells", "For IPv6, the bind and URL bracket the address",
		"Record the current CA fingerprint, the state path, the full desired SAN list and every client's URL.",
		"Stop service-managed sidecars, then the plane, through their manager, and confirm the processes exited.", "Do not merely kill a process that a manager will restart.",
		"Preserve state; do not reinitialize it.", "edit **only** `bind` in `STATE/config.json` while stopped, retaining `schema_version` 1, ownership and mode",
		"`plane run --bind NEW` is not a configuration update", deployBindDiag,
		"The placeholders are the requested bind, the configured bind and the `config.json` path; the check is shared with initialization, so it says \"init\" even when `plane run` reports it.",
		"If only DNS or a SAN changes and the listening bind remains valid, no configuration edit is needed.",
		"These are a **complete replacement** list: include every DNS name and IP address still used", "Ports do not belong in SANs.", "verify the CA fingerprint is unchanged",
		"its service definition passes no bind or SAN override flags",
		"Re-enroll a sidecar only when its enrolled URL is no longer valid; otherwise leave its enrollment unchanged and restart any sidecar stopped for maintenance.",
		"enroll using the **same** sidecar state directory and the existing CA at the new URL, confirm the printed `node_id` is unchanged",
		"only where their URL changes", "A client whose URL remains valid keeps its URL and CA.", "never delete state, blindly redispatch or rotate the CA",
		"Rollback, if required, uses the recorded bind and the full old SAN set"))
	errs = append(errs, inOrder("address", body, "Record the current CA fingerprint", "deploy: address-status", "Stop service-managed sidecars", "edit **only** `bind`", "deploy: address-reissue", "deploy: address-verify-ca",
		"Start the plane with the same state path", "deploy: address-node-list", "Re-enroll a sidecar only when", "deploy: address-reenroll", "Confirm node liveness"))
	errs = append(errs, wantDeployArgs(md, "address-status", "callsheet", "plane", "status", "--state-dir", "$STATE"))
	errs = append(errs, wantDeployArgs(md, "address-reissue", "callsheet", "plane", "cert", "reissue", "--state-dir", "$STATE", "--san", "127.0.0.1", "--san", "$PLANE_IP"))
	errs = append(errs, wantDeployArgs(md, "address-verify-ca", "callsheet", "plane", "status", "--state-dir", "$STATE"))
	errs = append(errs, wantDeployArgs(md, "address-node-list", "callsheet", "node", "ls", "--plane", "$PLANE", "--ca", "$CA"))
	errs = append(errs, wantDeployArgs(md, "address-reenroll", "callsheet", "sidecar", "enroll", "--state-dir", "$SIDECAR_STATE", "--plane", "$PLANE", "--ca", "$CA"))
	_, _, err = addressExamples(md)
	errs = append(errs, err)
	errs = append(errs, forbidAll("address", body, "rm -rf", "plane init", "--insecure", "re-enroll every", "kill -9", "pkill"))
	return errors.Join(errs...)
}

// addressExamples returns the chapter's before and after JSON binds, requiring
// that exactly the bind differs and both are parseable, schema 1 configurations.
func addressExamples(md string) (before, after string, err error) {
	body, err := sectionOf(md, "Change the plane address")
	if err != nil {
		return "", "", err
	}
	fences, err := parseDeployFences(body)
	if err != nil {
		return "", "", err
	}
	var docs []map[string]any
	for _, f := range fences {
		if f.lang != "json" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(f.body), &d); err != nil {
			return "", "", fmt.Errorf("address: JSON example at line %d: %v", f.line, err)
		}
		docs = append(docs, d)
	}
	if len(docs) != 2 {
		return "", "", fmt.Errorf("address: %d JSON examples, want a before and an after", len(docs))
	}
	for i, d := range docs {
		if len(d) != 2 || d["schema_version"] != float64(1) {
			return "", "", fmt.Errorf("address: JSON example %d changes more than bind: %v", i+1, d)
		}
	}
	before, _ = docs[0]["bind"].(string)
	after, _ = docs[1]["bind"].(string)
	if before == "" || after == "" || before == after {
		return "", "", fmt.Errorf("address: JSON examples do not change bind: %q to %q", before, after)
	}
	return before, after, nil
}

// deployRig drives the chapter's fences beside the shared quickstart rig.
type deployRig struct {
	*qsRig
	md     string
	fences []qsFence
	root   string
}

func newDeployRig(t *testing.T) *deployRig {
	t.Helper()
	r := newQuickstartRig(t)
	md := string(repoFile(t, deployDoc))
	fences, err := parseDeployFences(md)
	if err != nil {
		t.Fatal(err)
	}
	return &deployRig{qsRig: r, md: md, fences: fences, root: filepath.Dir(r.home)}
}

// account returns a fresh account home, selected for following fences.
func (d *deployRig) account(name string) string {
	d.t.Helper()
	home := mkdir(d.t, filepath.Join(d.root, name))
	d.vars["HOME"] = home
	return home
}

func (d *deployRig) parsed(id string) qsParsed {
	d.t.Helper()
	f, ok := fenceByID(d.fences, id)
	if !ok {
		d.t.Fatalf("no fence %s", id)
	}
	p, err := parseDeployFence(f)
	if err != nil {
		d.t.Fatal(err)
	}
	return p
}

// deployArgv returns fence id's single command with the variables and the fixture
// translations applied (a loopback bind, the fake adapter).
func (d *deployRig) deployArgv(id string) []string {
	d.t.Helper()
	p := d.parsed(id)
	if len(p.callsheet) != 1 {
		d.t.Fatalf("fence %s: %d commands", id, len(p.callsheet))
	}
	var out []string
	for _, w := range p.callsheet[0].words[1:] {
		out = append(out, d.expand(w))
	}
	if err := translateFixtureArgs(out, d.fake); err != nil {
		d.t.Fatalf("fence %s: %v", id, err)
	}
	return out
}

// apply executes a fence's fixture-relevant setup: assignments, directories and
// the modes it names. A read takes its value from the fixture.
func (d *deployRig) apply(id string) {
	d.t.Helper()
	for _, c := range d.parsed(id).setup {
		first, ok := c.words[0].literal()
		switch {
		case !ok && len(c.words) == 2:
			// The worker version check is a vendor step, not a fixture.
		case ok && first == "read":
			name, _ := c.words[2].literal()
			if _, ok := d.vars[name]; !ok {
				d.t.Fatalf("fence %s: no fixture value for prompted %s", id, name)
			}
		case ok && first == "mkdir":
			if err := os.Mkdir(d.expand(c.words[3]), 0o700); err != nil {
				d.t.Fatal(err)
			}
		case ok && first == "chmod":
			if err := os.Chmod(d.expand(c.words[2]), 0o600); err != nil {
				d.t.Fatal(err)
			}
		default:
			name, val, _ := strings.Cut(c.words[0].parts[0].text, "=")
			rest := qsWord{parts: slices.Clone(c.words[0].parts)}
			rest.parts[0].text = val
			d.vars[name] = d.expand(rest)
		}
	}
}

// guardedMove executes one parsed guarded-move fence against d's variables.
func (d *deployRig) guardedMove(id string) error {
	d.t.Helper()
	return runGuardedMove(d.t, d.parsed(id), d.vars)
}

// runGuardedMove executes the allowlisted operations of a move fence in order,
// stopping at the first failure as && does. It evaluates the guards itself and
// runs the real mv with exactly the fence's argv after substitution; an archive
// that is not an unused sibling of its source is refused here too.
func runGuardedMove(t *testing.T, p qsParsed, vars map[string]string) error {
	t.Helper()
	for _, c := range p.setup {
		words := make([]string, 0, len(c.words))
		for _, w := range c.words {
			v, err := w.expand(vars)
			if err != nil {
				return err
			}
			words = append(words, v)
		}
		switch words[0] {
		case "test":
			if err := evalGuard(words[1:]); err != nil {
				return err
			}
		case "mv":
			src, dst := words[1], words[2]
			if filepath.Dir(src) != filepath.Dir(dst) {
				return fmt.Errorf("move: %s is not a sibling of %s", dst, src)
			}
			ctx, cancel := context.WithTimeout(context.Background(), qsWait)
			defer cancel()
			cmd := exec.CommandContext(ctx, "mv", src, dst)
			cmd.WaitDelay = 5 * time.Second
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("mv: %v: %s", err, out)
			}
		case "printf":
		default:
			return fmt.Errorf("move: operation %s is not allowed", words[0])
		}
	}
	return nil
}

// evalGuard evaluates "-d P", "! -L P" or "! -e P" as test(1) does.
func evalGuard(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "-d":
		st, err := os.Stat(args[1])
		if err != nil || !st.IsDir() {
			return fmt.Errorf("guard: %s is not a directory", args[1])
		}
	case len(args) == 3 && args[0] == "!" && args[1] == "-L":
		if st, err := os.Lstat(args[2]); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("guard: %s is a symbolic link", args[2])
		}
	case len(args) == 3 && args[0] == "!" && args[1] == "-e":
		if _, err := os.Stat(args[2]); err == nil {
			return fmt.Errorf("guard: %s exists", args[2])
		}
	default:
		return fmt.Errorf("guard: unsupported test %q", args)
	}
	return nil
}

// treeInventory is every relative path's mode and contents below dir, so a
// retained archive's preservation is an exact comparison.
func treeInventory(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		st, err := e.Info()
		if err != nil {
			return err
		}
		entry := st.Mode().String()
		if st.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entry += " " + string(b)
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameInventory(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Fatalf("%s changed: %d entries, want %d", what, len(got), len(want))
	}
}

func statusFingerprint(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "ca_fingerprint: "); ok {
			return v
		}
	}
	t.Fatalf("no ca_fingerprint in %q", stdout)
	return ""
}

// enrolledNode returns the node ID from a sidecar enroll report.
func enrolledNode(t *testing.T, r result) string {
	t.Helper()
	id, ok := strings.CutPrefix(strings.Split(r.stdout, "\n")[0], "node_id: ")
	if !ok || !contract.ValidNodeID(id) {
		t.Fatalf("enroll = %+v", r)
	}
	return id
}

// enrollmentOf reads a sidecar's stored target and trust.
func enrollmentOf(t *testing.T, state string) (url, ca string) {
	t.Helper()
	var doc struct {
		PlaneURL string `json:"plane_url"`
		CAPEM    string `json:"ca_pem"`
	}
	if err := json.Unmarshal(nodeFile(t, filepath.Join(state, "enrollment.json")), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.PlaneURL, doc.CAPEM
}

// planeUnitArgs renders the shipped plane unit for planeHome and returns its
// real plane run flags: a service restart in the fixtures uses exactly these.
func planeUnitArgs(t *testing.T, vars sampleVars) []string {
	t.Helper()
	return sampleArgv(t, planeUnit, vars)[2:]
}

// sampleArgv is one rendered shipped sample's Callsheet argv, command path
// included and executable excluded.
func sampleArgv(t *testing.T, name string, vars sampleVars) []string {
	t.Helper()
	text, err := renderSample(string(repoFile(t, sampleDir+name)), vars)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".service") {
		u, err := parseUnit(text)
		if err != nil {
			t.Fatal(err)
		}
		argv, err := unitArgv(name, u, vars)
		if err != nil {
			t.Fatal(err)
		}
		return argv
	}
	root, err := parsePlist(text)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := plistArgv(name, root, vars)
	if err != nil {
		t.Fatal(err)
	}
	return argv
}

// TestDeployAddressChange (FP-2): the procedure's order and contract, and the
// chapter's argv on a loopback plane: an ephemeral port, an added localhost SAN
// and the same CA, keys and node identity throughout. No LAN evidence.
func TestDeployAddressChange(t *testing.T) {
	md := string(repoFile(t, deployDoc))
	t.Run("document", func(t *testing.T) {
		if err := checkDeployAddressDoc(md); err != nil {
			t.Fatalf("shipped chapter: %v", err)
		}
		const sec = "Change the plane address"
		runFPMutations(t, md, checkDeployAddressDoc, []readmeMutation{
			{"remove-stop", inSection(sec, "Stop service-managed sidecars, then the plane, through their manager, and confirm the processes exited.", ""), "address: lacks \"Stop service-managed"},
			{"remove-conditional-recipe", func(t *testing.T, md string) string {
				t.Helper()
				b, _ := markedFenceBlock(t, md, "<!-- deploy: address-reenroll -->")
				return strings.Replace(md, b, "", 1)
			}, "address: lacks step \"deploy: address-reenroll\""},
			{"remove-existing-san", inSection(sec, `--san 127.0.0.1 --san "$PLANE_IP"`, `--san "$PLANE_IP"`), "fence address-reissue ="},
			{"remove-ca-preservation", inSection(sec, "verify the CA fingerprint is unchanged", "continue"), "address: lacks \"verify the CA fingerprint"},
			{"unconditional-reenroll", inSection(sec, "Re-enroll a sidecar only when its enrolled URL is no longer valid; otherwise leave its enrollment unchanged and restart any sidecar stopped for maintenance.", "Re-enroll every sidecar after any address change."), "address: lacks \"Re-enroll a sidecar only when"},
			{"remove-diagnostic-explanation", inSection(sec, "so it says \"init\" even when `plane run` reports it", "so read it as a warning"), "address: lacks \"The placeholders"},
			{"changed-diagnostic", inSection(sec, "init never edits existing configuration", "init does not edit configuration"), "address: lacks"},
			{"config-edit-to-run-bind", inSection(sec, "edit **only** `bind` in `STATE/config.json` while stopped", "run `plane run --bind NEW`"), "address: lacks"},
			{"different-sidecar-state", inSection(sec, `callsheet sidecar enroll --state-dir "$SIDECAR_STATE"`, `callsheet sidecar enroll --state-dir "$OLD_SIDECAR_STATE"`), "fence address-reenroll ="},
			{"partial-san-claim", inSection(sec, "These are a **complete replacement** list: include every DNS name and IP address still used", "Add only the new name"), "address: lacks \"These are a"},
			{"reorder-reissue-before-stop", func(t *testing.T, md string) string {
				t.Helper()
				a, _ := markedFenceBlock(t, md, "<!-- deploy: address-status -->")
				b, _ := markedFenceBlock(t, md, "<!-- deploy: address-node-list -->")
				md = strings.Replace(md, a, "@@A@@", 1)
				md = strings.Replace(md, b, a, 1)
				return strings.Replace(md, "@@A@@", b, 1)
			}, "out of order"},
			{"example-changes-schema", inSection(sec, `{ "schema_version": 1, "bind": "192.168.1.20:8443" }`, `{ "schema_version": 2, "bind": "192.168.1.20:8443" }`), "changes more than bind"},
			{"example-adds-key", inSection(sec, `{ "schema_version": 1, "bind": "192.168.1.20:8443" }`, `{ "schema_version": 1, "bind": "192.168.1.20:8443", "sans": [] }`), "changes more than bind"},
			{"example-no-change", inSection(sec, `"bind": "192.168.1.20:8443"`, `"bind": "192.168.1.10:8443"`), "do not change bind"},
			{"reinitialize", appendToSection(sec, "Run `plane init` again."), "address: forbidden \"plane init\""},
		})
	})
	t.Run("lifecycle", func(t *testing.T) {
		d := newDeployRig(t)
		planeHome := d.account("plane-account")
		vars := sampleVars{planeHome: planeHome, callsheet: d.p.bin, workerPath: d.fake}
		d.vars["PLANE_IP"] = "127.0.0.1"
		d.apply("plane-dirs")
		d.apply("plane-env")
		if want := filepath.Join(planeHome, "callsheet", "plane"); d.vars["STATE"] != want {
			t.Fatalf("documented STATE = %q, want %q", d.vars["STATE"], want)
		}
		state := d.vars["STATE"]
		if r := d.ok("plane init", d.deployArgv("plane-init")...); !strings.Contains(r.stdout, "ip_addresses: 127.0.0.1") {
			t.Fatalf("plane init = %+v", r)
		}
		run := planeUnitArgs(t, vars)
		first := d.p.start(t, run...)
		oldURL := "https://" + first.addr
		pki := filepath.Join(state, "pki")
		config := filepath.Join(state, "config.json")

		// The worker holds its own private CA copy and enrolls at the old URL.
		workerHome := d.account("worker-account")
		vars.workerHome = workerHome
		d.apply("worker-dirs")
		d.vars["WORKER"] = d.fake
		d.apply("worker-env")
		if err := os.WriteFile(d.vars["CA"], nodeFile(t, filepath.Join(pki, "ca.crt")), 0o600); err != nil {
			t.Fatal(err)
		}
		d.apply("ca-mode")
		d.vars["PLANE"] = oldURL
		node := enrolledNode(t, d.ok("enroll", d.deployArgv("worker-enroll")...))
		sidecarState := d.vars["SIDECAR_STATE"]
		if url, _ := enrollmentOf(t, sidecarState); url != oldURL {
			t.Fatalf("enrolled at %q, want %q", url, oldURL)
		}

		// The name is not yet a SAN: this is what the procedure changes.
		_, port, _ := strings.Cut(first.addr, ":")
		if r := d.p.run(t, "node", "ls", "--plane", "https://localhost:"+port, "--ca", d.vars["CA"]); r.code != deployExitTrust || !strings.Contains(r.stderr, deployUntrusted) {
			t.Fatalf("localhost before reissue = %+v", r)
		}
		status := d.ok("status", d.deployArgv("address-status")...)
		fingerprint := statusFingerprint(t, status.stdout)

		// Step 1: stop, preserving state.
		if code := first.stop(t, os.Interrupt); code != deployExitStopped {
			t.Fatalf("stop = %d", code)
		}
		caBefore, keyBefore, serverKeyBefore, certBefore := nodeFile(t, filepath.Join(pki, "ca.crt")), nodeFile(t, filepath.Join(pki, "ca.key")), nodeFile(t, filepath.Join(pki, "server.key")), nodeFile(t, filepath.Join(pki, "server.crt"))
		configBefore := nodeFile(t, config)
		st, err := os.Stat(config)
		if err != nil {
			t.Fatal(err)
		}
		mode := st.Mode()

		// Step 2 with the chapter's own example shape: only bind changes.
		if _, _, err := addressExamples(d.md); err != nil {
			t.Fatal(err)
		}
		var newBind string
		editBind := func() {
			t.Helper()
			newBind = freeLoopbackPort(t, first.addr)
			edited := bytes.Replace(configBefore, []byte(`"127.0.0.1:0"`), []byte(`"`+newBind+`"`), 1)
			if bytes.Equal(edited, configBefore) {
				t.Fatalf("config holds no bind to edit: %s", configBefore)
			}
			if err := os.WriteFile(config, edited, mode.Perm()); err != nil {
				t.Fatal(err)
			}
		}
		editBind()

		// Step 3: the complete replacement SAN list.
		d.vars["PLANE_IP"] = "localhost"
		d.ok("reissue", d.deployArgv("address-reissue")...)
		if !slices.Equal(nodeFile(t, filepath.Join(pki, "ca.crt")), caBefore) || !slices.Equal(nodeFile(t, filepath.Join(pki, "ca.key")), keyBefore) || !slices.Equal(nodeFile(t, filepath.Join(pki, "server.key")), serverKeyBefore) {
			t.Fatal("reissue changed the CA or a private key")
		}
		if slices.Equal(nodeFile(t, filepath.Join(pki, "server.crt")), certBefore) {
			t.Fatal("reissue left the server certificate unchanged")
		}
		dns, ips := certSANs(t, filepath.Join(pki, "server.crt"))
		if !slices.Equal(dns, []string{"localhost"}) || !slices.Equal(ips, []string{"127.0.0.1"}) {
			t.Fatalf("SANs = %q, %q", dns, ips)
		}
		if got := statusFingerprint(t, d.ok("status", d.deployArgv("address-verify-ca")...).stdout); got != fingerprint {
			t.Fatalf("fingerprint %q, want %q", got, fingerprint)
		}

		// Step 4: an explicit mismatching bind is refused; the service's own argv
		// starts. A port taken in the meantime picks another one, exactly as the
		// quickstart's fixture does; that is the only retry.
		var again *planeProc
		for attempt := 0; again == nil; attempt++ {
			if attempt > 0 {
				editBind()
			}
			refused := d.p.run(t, "plane", "run", "--state-dir", state, "--bind", first.addr)
			if want := fmt.Sprintf(deployBindDiag, first.addr, newBind, config); refused.code == 0 || !strings.Contains(refused.stderr, want) {
				t.Fatalf("mismatching --bind = %+v, want %q", refused, want)
			}
			pp, err := tryStartPlane(t, d.p, run...)
			switch {
			case err == nil:
				again = pp
			case attempt < 5 && strings.Contains(err.Error(), "address already in use"):
				t.Logf("retrying: %v", err)
			default:
				t.Fatal(err)
			}
		}
		if again.addr != newBind {
			t.Fatalf("listening on %s, want %s", again.addr, newBind)
		}
		var doc map[string]any
		if err := json.Unmarshal(nodeFile(t, config), &doc); err != nil || doc["schema_version"] != float64(1) || doc["bind"] != newBind || len(doc) != 2 {
			t.Fatalf("config = %s, %v", nodeFile(t, config), err)
		}
		if st, err := os.Stat(config); err != nil || st.Mode() != mode {
			t.Fatalf("config mode: %v, %v", st, err)
		}

		// The new URL under the unchanged CA; the old address is gone.
		_, newPort, _ := strings.Cut(newBind, ":")
		d.vars["PLANE"] = "https://localhost:" + newPort
		d.ok("node ls", d.deployArgv("address-node-list")...)
		if r := d.p.run(t, "node", "ls", "--plane", oldURL, "--ca", d.vars["CA"]); r.code == 0 {
			t.Fatalf("the old address still answers: %+v", r)
		}

		// Step 5: same state, same identity, updated target, same trust.
		if got := enrolledNode(t, d.ok("re-enroll", d.deployArgv("address-reenroll")...)); got != node {
			t.Fatalf("node %q after re-enrollment, want %q", got, node)
		}
		url, ca := enrollmentOf(t, sidecarState)
		if url != d.vars["PLANE"] || ca != string(caBefore) {
			t.Fatalf("enrollment = %q and a CA of %d bytes", url, len(ca))
		}
		if r := d.ok("node ls", d.deployArgv("address-node-list")...); !strings.Contains(r.stdout, node) {
			t.Fatalf("node ls = %q", r.stdout)
		}
		if code := again.stop(t, os.Interrupt); code != deployExitStopped {
			t.Fatalf("final stop = %d", code)
		}
	})
}

// deployMoveBody is a guarded move's exact pinned text.
func deployMoveBody(src, dst string) string {
	return strings.Join([]string{
		`test -d "$` + src + `" &&`,
		`  test ! -L "$` + src + `" &&`,
		`  test ! -e "$` + dst + `" &&`,
		`  test ! -L "$` + dst + `" &&`,
		`  mv "$` + src + `" "$` + dst + `" &&`,
		`  test -d "$` + dst + `" &&`,
		`  test ! -e "$` + src + `" &&`,
		`  printf 'archive ready\n'`,
	}, "\n")
}

// checkDeployRecoveryDoc is FP-3's static validator.
func checkDeployRecoveryDoc(md string) error {
	body, err := sectionOf(md, "Recover after CA loss")
	if err != nil {
		return err
	}
	text := stripInlineLinks(body)
	var errs []error
	errs = append(errs, needAll("recovery", text,
		"Callsheet has no CA reset, rotation or in-place PKI reconstruction command.", "It deliberately refuses incomplete trust", "Do not remove PKI files, overwrite the configuration or run `init` hoping it repairs them.",
		"First stop dispatch and all affected services through their manager, then confirm that all Callsheet commands and task processes using the affected state have exited", "An unconfirmed cleanup is a blocker.",
		"as its owner and from its home, not from inside the state directory", "`OLD_STATE` is an unused sibling", "add a unique suffix if the name is taken",
		"the destination is absent, including no dangling symlink", "the expected real directory, not a symlink", "Never move onto an existing directory, which can nest the source, and never use a force or overwrite option.",
		"A deployment under a custom mount or custom unit is outside this recipe", "contents, not inode metadata",
		"### Complete backup available", "taken with the plane stopped", "Restore it into the now-vacant canonical path on the same machine", "Never merge fragments or overwrite the only damaged copy.",
		"Validate the status and the expected CA fingerprint before starting the plane with its unchanged service definition", deployRootDiag, deployKeyDiag,
		"correct the permissions and do not re-initialize", "The root check rejects group and other bits, not every owner-only mode different from 0700.",
		"Existing clients keep their CA if the restored CA is identical.", "State rolls back to the backup point", "do not automatically redispatch missing work",
		"this is not an off-host disaster-backup promise", "Even a complete plane backup can lack tasks admitted after its snapshot.", "Do not erase journals",
		"### No complete backup", "This is a new plane, not a recovery of the old registry, history or workspace database.", "No manual import or merge is supported",
		"Keep the service path unchanged.", "Distribute only the new `ca.crt` to **every** sidecar, coordinator and operator machine.", "Coordinator and operator CA copies keep their existing paths with the new certificate",
		"each coordinator's MCP connection restarts with the new trust", "confirm its task processes have exited",
		"Move the entire stopped old sidecar state directory to an unused sibling and preserve its contents untouched for inspection, including `identity.json`, `enrollment.json`, `tasks/` and `workspace-cache/`.",
		"Do not re-enroll or run that old directory against the new plane.", "replace the public CA copy in place at `<worker-home>/callsheet/plane-ca.crt`, retaining worker ownership and mode 0600; `CA` continues to name that path",
		"It gets a **new node ID**", "Keep the sidecar service's state path unchanged and start it with the same explicit adapter paths",
		"Explicitly re-register roles on the new plane, using the new node IDs", "Recreate or reseed needed workspaces from separately retained source and results; `workspace-cache/` is a disposable cache, not a backup or an import path.",
		"Do not copy plane private keys or vendor credentials.", "Old in-flight tasks and results do not transfer.", "Do not automatically redispatch old work; review its side effects first.",
		"The new plane shows no old-plane tasks or results, even if the stopped old sidecar still has files under `tasks/`. Keep that old state for inspection until you choose to delete it.",
		"A fresh plane has an empty list before new dispatch; after new work, only its own tasks appear.",
		"Its journals are not automatically forgotten", "`worker_unready`", "Neither service definition changes, because both keep their canonical state paths."))
	if !slices.Contains(markdownLinks(body), "quickstart.md#one-machine") {
		errs = append(errs, errors.New("recovery: no link to the quickstart registration recipe"))
	}
	errs = append(errs, inOrder("recovery", body, "First stop dispatch", "deploy: recover-plane-paths", "deploy: recover-plane-mount", "deploy: recover-plane-move", "### Complete backup available", "deploy: restore-backup", "deploy: restore-status",
		"### No complete backup", "deploy: fresh-init", "Distribute only the new", "stop the old sidecar", "deploy: recover-sidecar-paths", "deploy: recover-sidecar-move", "deploy: fresh-ca-mode", "deploy: fresh-enroll", "deploy: fresh-sidecar-run",
		"Explicitly re-register roles", "deploy: fresh-task-list"))
	errs = append(errs, checkServiceFences(md, "recover-plane-move", "recover-sidecar-move", "recover-plane-paths", "recover-sidecar-paths", "recover-plane-mount", "recover-sidecar-mount",
		"backup-env", "backup-plane", "restore-select", "restore-backup", "fresh-env", "fresh-ca-mode"))
	// Taking a backup is pre-loss maintenance: over the whole chapter, its path is
	// defined and the copy taken before recovery moves state away, and a restore
	// selects an existing backup before it copies.
	errs = append(errs, inOrder("recovery", md, "### Back up the plane", "deploy: backup-env", "deploy: backup-plane", "## Recover after CA loss", "deploy: recover-plane-move", "deploy: restore-select", "deploy: restore-backup"))
	errs = append(errs, needAll("recovery", stripInlineLinks(md),
		"Take this backup before anything has gone wrong, as routine maintenance.", "it cannot be created after the CA is lost", "`BACKUP_STATE` is an absolute, unused path in that account",
		"Select an existing complete backup", "Do not take a new one now", "Set `BACKUP_STATE` to the selected backup's absolute path"))
	if !slices.Contains(markdownLinks(body), "#back-up-the-plane") {
		errs = append(errs, errors.New("recovery: no link to the pre-loss backup"))
	}
	errs = append(errs, wantDeployArgs(md, "fresh-init", "callsheet", "plane", "init", "--state-dir", "$STATE", "--bind", "$PLANE_IP:8443", "--san", "127.0.0.1", "--san", "$PLANE_IP"))
	errs = append(errs, wantDeployArgs(md, "fresh-plane-run", "callsheet", "plane", "run", "--state-dir", "$STATE"))
	errs = append(errs, wantDeployArgs(md, "fresh-enroll", "callsheet", "sidecar", "enroll", "--state-dir", "$SIDECAR_STATE", "--plane", "$PLANE", "--ca", "$CA"))
	errs = append(errs, wantDeployArgs(md, "fresh-sidecar-run", "callsheet", "sidecar", "run", "--state-dir", "$SIDECAR_STATE", "--claude-adapter", "$WORKER"))
	errs = append(errs, wantDeployArgs(md, "fresh-task-list", "callsheet", "task", "ls", "--plane", "$PLANE", "--ca", "$CA"))
	errs = append(errs, wantDeployArgs(md, "restore-status", "callsheet", "plane", "status", "--state-dir", "$STATE"))
	errs = append(errs, forbidAll("recovery", body, "callsheet ca ", "plane ca ", "rm -rf", "mv -f", "--force", "cache restores", "restore from `workspace-cache", "results transfer automatically",
		"old tasks transfer", "delete the journal", "`forget`", "plane-new", "sidecar-new", "edit the unit", "edit the plist"))
	return errors.Join(errs...)
}

// oldNodeJournal seeds one valid, ownerless completed task journal into a stopped
// sidecar: synthetic old-plane evidence on the shared contract encoder, for a fake
// role, with no live guardian or workspace binding and an empty output tail.
func oldNodeJournal(t *testing.T, state, node, instruction, runbook string) string {
	t.Helper()
	id := "t_" + strings.Repeat("a", 26) + "000123"
	role := contract.RoleRecord{RoleConfig: contract.RoleConfig{ID: "old-role", Name: "old", Node: node, Adapter: adapter.FakeID, Instruction: instruction, Runbook: runbook,
		Model: qsFakeModel, Effort: "low", Concurrency: 1, Timeout: 5 * time.Minute, HasTimeout: true}, RegistrationOrder: 1}
	st := contract.TaskStartBody{TaskID: id, Execution: contract.ExecutionToken{Epoch: strings.Repeat("e", 32), Attachment: 1}, RolesRevision: 1, Role: role,
		Request: contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: role.ID}, Goal: "old goal", Acceptance: "old acceptance",
			RequestedBy: contract.RequestedBy{Name: "callsheet", Version: "dev", Hostname: "old-coordinator"}},
		Effective: contract.TaskEffective{Model: role.Model, Effort: role.Effort, Timeout: role.Timeout}}
	nonce, started, exit := strings.Repeat("0f", 32), time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC), 0
	res := contract.TaskResultBody{TaskID: id, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit}.Sealed()
	j := contract.ExecutionJournal{TaskID: id, Execution: st.Execution, StartDigest: st.StartDigestHex(), Role: st.Role, Effective: st.Effective,
		TimeoutPolicy: contract.TimeoutPolicyEnforced, Phase: contract.JournalCompleted, OwnerNonce: &nonce, StartedAt: &started, Result: &res}
	b, err := contract.EncodeExecutionJournal(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.ParseExecutionJournal(b, adapter.Lookup()); err != nil {
		t.Fatalf("seeded journal: %v", err)
	}
	dir := filepath.Join(state, "tasks", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(state, "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "execution.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

// roleAccepts reports whether role id on node reports can_accept.
func (d *deployRig) roleAccepts(id, node string) bool {
	res := d.p.run(d.t, d.withJSON("role-list", d.argv("role-list"))...)
	if res.code != 0 {
		return false
	}
	roles, err := contract.ParseRoleListResponse([]byte(strings.TrimSuffix(res.stdout, "\n")), nil)
	return err == nil && slices.ContainsFunc(roles, func(v contract.RoleView) bool { return v.ID == id && v.Node == node && v.CanAccept })
}

// addRole registers fixture role id through the linked quickstart's role-add argv.
func (d *deployRig) addRole(id, node string) {
	d.t.Helper()
	d.vars["NODE"] = node
	args := d.argv("role-add")
	if len(args) < 3 || args[0] != "role" || args[1] != "add" || args[2] != "quickstart-local" {
		d.t.Fatalf("role-add = %q", args)
	}
	args[2] = id
	d.ok("role add", args...)
}

// dispatchArgs is the linked quickstart's dispatch argv for role id, with the
// fixture's fake goal. This fixture needs no workspace, so the fence's three
// workspace selection flags are the only ones removed.
func (d *deployRig) dispatchArgs(id string) []string {
	d.t.Helper()
	d.vars["INSTANCE"] = "unused"
	args := d.argv("dispatch")
	var out []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--workspace", "--base", "--workspace-instance":
			i++
			continue
		case "--role-id":
			out = append(out, args[i], id)
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// TestDeployCALossRecovery (FP-3): both documented branches on disposable loopback
// state, and the evidence for the fresh-sidecar decision: an old node stays
// unready under a fresh plane while a fresh node takes a dispatch.
func TestDeployCALossRecovery(t *testing.T) {
	md := string(repoFile(t, deployDoc))
	t.Run("document", func(t *testing.T) {
		if err := checkDeployRecoveryDoc(md); err != nil {
			t.Fatalf("shipped chapter: %v", err)
		}
		const sec = "Recover after CA loss"
		drop := func(id string) func(*testing.T, string) string {
			return func(t *testing.T, md string) string {
				t.Helper()
				b, _ := markedFenceBlock(t, md, "<!-- deploy: "+id+" -->")
				return strings.Replace(md, b, "", 1)
			}
		}
		runFPMutations(t, md, checkDeployRecoveryDoc, []readmeMutation{
			{"omit-stop", inSection(sec, "First stop dispatch and all affected services through their manager, then confirm that all Callsheet commands and task processes using the affected state have exited", "Continue"), "recovery: lacks \"First stop dispatch"},
			{"omit-plane-move", drop("recover-plane-move"), "recovery: lacks step \"deploy: recover-plane-move\""},
			{"omit-sidecar-move", drop("recover-sidecar-move"), "recovery: lacks step \"deploy: recover-sidecar-move\""},
			{"omit-destination-guard", inSection(sec, "  test ! -e \"$OLD_STATE\" &&\n", ""), "fence recover-plane-move ="},
			{"omit-symlink-guard", inSection(sec, "  test ! -L \"$OLD_SIDECAR_STATE\" &&\n", ""), "fence recover-sidecar-move ="},
			{"unchained-guard", inSection(sec, "test -d \"$STATE\" &&\n", "test -d \"$STATE\"\n"), "fence recover-plane-move ="},
			{"nested-archive", inSection(sec, `OLD_STATE="$HOME/callsheet/plane-old-REPLACE_WITH_DATE"`, `OLD_STATE="$STATE/plane-old-REPLACE_WITH_DATE"`), "fence recover-plane-paths ="},
			{"nested-sidecar-archive", inSection(sec, `OLD_SIDECAR_STATE="$HOME/callsheet/sidecar-old-REPLACE_WITH_DATE"`, `OLD_SIDECAR_STATE="$SIDECAR_STATE/old"`), "fence recover-sidecar-paths ="},
			{"service-path-edit", inSection(sec, "Keep the sidecar service's state path unchanged and start it", "Edit the unit to name a new state path and start it"), "recovery: lacks \"Keep the sidecar service's"},
			{"change-plane-state-path", inSection(sec, `STATE="$HOME/callsheet/plane"`, `STATE="$HOME/callsheet/plane-new"`), "fence recover-plane-paths ="},
			{"old-state-reuse", inSection(sec, "Do not re-enroll or run that old directory against the new plane.", "Re-enroll that old directory against the new plane."), "recovery: lacks \"Do not re-enroll"},
			{"automatic-journal-deletion", appendToSection(sec, "Then delete the journal files under tasks/."), "recovery: forbidden \"delete the journal\""},
			{"automatic-forget", appendToSection(sec, "The new plane sends `forget` for old journals."), "recovery: forbidden \"`forget`\""},
			{"omit-new-node", inSection(sec, "It gets a **new node ID**", "It keeps its node ID"), "recovery: lacks \"It gets a"},
			{"omit-role-registration", inSection(sec, "Explicitly re-register roles on the new plane, using the new node IDs", "Roles carry over"), "recovery: lacks"},
			{"cache-as-backup", inSection(sec, "`workspace-cache/` is a disposable cache, not a backup or an import path", "the cache restores workspaces"), "recovery: lacks"},
			{"cache-restores", appendToSection(sec, "The cache restores workspaces."), "recovery: forbidden \"cache restores\""},
			{"omit-preservation", inSection(sec, "Keep that old state for inspection until you choose to delete it.", ""), "recovery: lacks \"The new plane shows"},
			{"omit-task-list", drop("fresh-task-list"), "recovery: lacks step \"deploy: fresh-task-list\""},
			{"tasks-transfer", inSection(sec, "Old in-flight tasks and results do not transfer.", "Old tasks transfer."), "recovery: lacks \"Old in-flight"},
			{"omit-client-class", inSection(sec, "**every** sidecar, coordinator and operator machine", "every sidecar"), "recovery: lacks \"Distribute only"},
			{"omit-coordinator-trust", inSection(sec, "each coordinator's MCP connection restarts with the new trust", "coordinators need nothing"), "recovery: lacks"},
			{"changed-root-diagnostic", inSection(sec, "it must not be accessible by group or others", "it is too open"), "recovery: lacks \"state directory"},
			{"changed-key-diagnostic", inSection(sec, "it must be owner-only", "it is too open"), "recovery: lacks \"private key"},
			{"invented-reset", appendToSection(sec, "Or run `callsheet ca reset`."), "recovery: forbidden \"callsheet ca \""},
			{"force-move", appendToSection(sec, "Use `mv -f` if the name is taken."), "recovery: forbidden \"mv -f\""},
			{"no-registration-link", inSection(sec, "(quickstart.md#one-machine)", "(quickstart.md)"), "recovery: no link"},
			{"backup-after-move", func(t *testing.T, md string) string {
				t.Helper()
				env, _ := markedFenceBlock(t, md, "<!-- deploy: backup-env -->")
				cp, _ := markedFenceBlock(t, md, "<!-- deploy: backup-plane -->")
				mv, _ := markedFenceBlock(t, md, "<!-- deploy: recover-plane-move -->")
				md = strings.Replace(strings.Replace(md, env, "", 1), cp, "", 1)
				return strings.Replace(md, mv, mv+"\n\n"+env+"\n\n"+cp, 1)
			}, "out of order"},
			{"backup-copy-after-move", func(t *testing.T, md string) string {
				t.Helper()
				cp, _ := markedFenceBlock(t, md, "<!-- deploy: backup-plane -->")
				mv, _ := markedFenceBlock(t, md, "<!-- deploy: recover-plane-move -->")
				md = strings.Replace(md, cp, "", 1)
				return strings.Replace(md, mv, mv+"\n\n"+cp, 1)
			}, "out of order"},
			{"backup-copy-before-its-path", func(t *testing.T, md string) string {
				t.Helper()
				env, _ := markedFenceBlock(t, md, "<!-- deploy: backup-env -->")
				cp, _ := markedFenceBlock(t, md, "<!-- deploy: backup-plane -->")
				md = strings.Replace(md, env, "@@A@@", 1)
				md = strings.Replace(md, cp, env, 1)
				return strings.Replace(md, "@@A@@", cp, 1)
			}, "out of order"},
			{"backup-path-undefined", replaceOnce("BACKUP_STATE=\"$HOME/callsheet/plane-backup-REPLACE_WITH_DATE\"\n", ""), "fence backup-env ="},
			{"backup-env-removed", drop("backup-env"), "fence backup-env is missing"},
			{"backup-path-relative", replaceOnce(`BACKUP_STATE="$HOME/callsheet/plane-backup-REPLACE_WITH_DATE"`, `BACKUP_STATE="plane-backup"`), "fence backup-env ="},
			{"restore-path-undefined", drop("restore-select"), "fence restore-select is missing"},
			{"restore-path-relative", inSection(sec, `BACKUP_STATE="$HOME/callsheet/plane-backup-REPLACE_WITH_DATE"`, `BACKUP_STATE="plane-backup"`), "fence restore-select ="},
			{"restore-before-selection", func(t *testing.T, md string) string {
				t.Helper()
				sel, _ := markedFenceBlock(t, md, "<!-- deploy: restore-select -->")
				cp, _ := markedFenceBlock(t, md, "<!-- deploy: restore-backup -->")
				md = strings.Replace(md, sel, "@@A@@", 1)
				md = strings.Replace(md, cp, sel, 1)
				return strings.Replace(md, "@@A@@", cp, 1)
			}, "out of order"},
			{"backup-not-pre-loss", replaceOnce("Take this backup before anything has gone wrong, as routine maintenance.", "Take this backup when you need it."), "recovery: lacks \"Take this backup"},
			{"backup-heading-removed", replaceOnce("### Back up the plane\n", ""), "recovery: lacks step \"### Back up the plane\""},
			{"recovery-takes-new-backup", inSection(sec, "Do not take a new one now", "Take a new one now"), "recovery: lacks \"Do not take a new one now\""},
			{"no-backup-link", inSection(sec, "(#back-up-the-plane)", "(#complete-backup-available)"), "recovery: no link to the pre-loss backup"},
		})
		// The move and archive guards themselves, without real state.
		for _, c := range []struct {
			name string
			args []string
			want string
		}{
			{"unknown-test", []string{"-f", "x"}, "unsupported test"},
			{"not-a-directory", []string{"-d", filepath.Join(t.TempDir(), "absent")}, "is not a directory"},
			{"exists", []string{"!", "-e", t.TempDir()}, "exists"},
		} {
			if err := evalGuard(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v", c.name, err)
			}
		}
	})

	t.Run("moves", func(t *testing.T) {
		d := newDeployRig(t)
		base := d.account("move-account")
		src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
		for _, p := range []string{src, dst} {
			mkdir(t, p)
			if err := os.WriteFile(filepath.Join(p, "f"), []byte(p), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		d.vars["STATE"], d.vars["OLD_STATE"] = src, dst
		before, beforeDst := treeInventory(t, src), treeInventory(t, dst)
		if err := d.guardedMove("recover-plane-move"); err == nil || !strings.Contains(err.Error(), "exists") {
			t.Fatalf("existing destination: %v", err)
		}
		sameInventory(t, "refused source", treeInventory(t, src), before)
		sameInventory(t, "refused destination", treeInventory(t, dst), beforeDst)

		// A dangling symlink is a taken name too, and a symlink is no source.
		dangling := filepath.Join(base, "dangling")
		if err := os.Symlink(filepath.Join(base, "nowhere"), dangling); err != nil {
			t.Fatal(err)
		}
		d.vars["OLD_STATE"] = dangling
		if err := d.guardedMove("recover-plane-move"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("dangling destination: %v", err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(src, link); err != nil {
			t.Fatal(err)
		}
		d.vars["STATE"], d.vars["OLD_STATE"] = link, filepath.Join(base, "free")
		if err := d.guardedMove("recover-plane-move"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("symlink source: %v", err)
		}
		d.vars["STATE"], d.vars["OLD_STATE"] = src, filepath.Join(src, "nested")
		if err := d.guardedMove("recover-plane-move"); err == nil || !strings.Contains(err.Error(), "not a sibling") {
			t.Fatalf("nested archive: %v", err)
		}
		sameInventory(t, "after every refusal", treeInventory(t, src), before)

		// The real move preserves relative bytes and modes on this native OS.
		if err := os.Chmod(filepath.Join(src, "f"), 0o400); err != nil {
			t.Fatal(err)
		}
		before = treeInventory(t, src)
		free := filepath.Join(base, "free")
		d.vars["STATE"], d.vars["OLD_STATE"] = src, free
		if err := d.guardedMove("recover-plane-move"); err != nil {
			t.Fatal(err)
		}
		sameInventory(t, "archive", treeInventory(t, free), before)
		if _, err := os.Lstat(src); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("source remains: %v", err)
		}
	})

	t.Run("lifecycle", func(t *testing.T) { deployRecoveryLifecycle(t) })
}

func deployRecoveryLifecycle(t *testing.T) {
	d := newDeployRig(t)
	planeHome := d.account("plane-account")
	d.vars["PLANE_IP"] = "127.0.0.1"
	d.apply("plane-dirs")
	d.apply("recover-plane-paths")
	state := d.vars["STATE"]
	if state != filepath.Join(planeHome, "callsheet", "plane") {
		t.Fatalf("documented STATE = %q", state)
	}
	parent := filepath.Dir(state)
	if filepath.Dir(d.vars["OLD_STATE"]) != parent {
		t.Fatalf("documented OLD_STATE = %q", d.vars["OLD_STATE"])
	}
	damaged, restored := filepath.Join(parent, "plane-old-damaged"), filepath.Join(parent, "plane-old-restored")
	pki := filepath.Join(state, "pki")
	vars := sampleVars{planeHome: planeHome, callsheet: d.p.bin, workerPath: d.fake}
	d.ok("plane init", d.deployArgv("plane-init")...)
	planeArgs := planeUnitArgs(t, vars)
	originalFP := statusFingerprint(t, d.ok("status", d.deployArgv("state-status")...).stdout)
	first := d.p.start(t, planeArgs...)
	url := "https://" + first.addr

	// The worker enrolls under the original CA, then stops being connected.
	workerHome := d.account("worker-account")
	vars.workerHome = workerHome
	d.apply("worker-dirs")
	d.vars["WORKER"] = d.fake
	d.apply("worker-env")
	d.apply("recover-sidecar-paths")
	sidecarState := d.vars["SIDECAR_STATE"]
	if sidecarState != filepath.Join(workerHome, "callsheet", "sidecar") {
		t.Fatalf("documented SIDECAR_STATE = %q", sidecarState)
	}
	caPath := d.vars["CA"]
	if caPath != filepath.Join(workerHome, "callsheet", "plane-ca.crt") {
		t.Fatalf("documented CA = %q", caPath)
	}
	if err := os.WriteFile(caPath, nodeFile(t, filepath.Join(pki, "ca.crt")), 0o600); err != nil {
		t.Fatal(err)
	}
	d.vars["PLANE"] = url
	oldNode := enrolledNode(t, d.ok("enroll", d.deployArgv("worker-enroll")...))
	if code := first.stop(t, os.Interrupt); code != deployExitStopped {
		t.Fatalf("stop = %d", code)
	}
	argvBefore := [][]string{sampleArgv(t, planeUnit, vars), sampleArgv(t, workerUnit, vars), sampleArgv(t, planePlist, vars), sampleArgv(t, sidePlist, vars)}

	// A complete stopped backup, then the loss: everything refuses and changes nothing.
	d.vars["HOME"] = planeHome
	d.apply("backup-env")
	backup := d.vars["BACKUP_STATE"]
	if !filepath.IsAbs(backup) || filepath.Dir(backup) != parent || d.vars["STATE"] != state {
		t.Fatalf("documented backup paths: %q from %q", backup, d.vars["STATE"])
	}
	runDeployCopy(t, d, "backup-plane")
	if err := os.Remove(filepath.Join(pki, "ca.key")); err != nil {
		t.Fatal(err)
	}
	before := treeInventory(t, state)
	for what, args := range map[string][]string{
		"init":    d.deployArgv("plane-init"),
		"run":     slices.Concat([]string{"plane", "run"}, planeArgs),
		"reissue": d.deployArgv("address-reissue"),
	} {
		r := d.p.run(t, args...)
		if r.code == 0 || !strings.Contains(r.stderr, deployIncomplete) {
			t.Fatalf("%s on incomplete trust = %+v", what, r)
		}
		sameInventory(t, what+" on incomplete trust", treeInventory(t, state), before)
	}

	// Branch one: move the damage aside, restore, prove the original CA connects.
	d.vars["OLD_STATE"] = damaged
	if err := d.guardedMove("recover-plane-move"); err != nil {
		t.Fatal(err)
	}
	sameInventory(t, "damaged archive", treeInventory(t, damaged), before)
	if _, err := os.Lstat(state); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("canonical plane path remains: %v", err)
	}
	d.apply("restore-select")
	if d.vars["BACKUP_STATE"] != backup {
		t.Fatalf("selected backup %q, want %q", d.vars["BACKUP_STATE"], backup)
	}
	runDeployCopy(t, d, "restore-backup")
	checkRestoreDiagnostics(t, d, state)
	if got := statusFingerprint(t, d.ok("status", d.deployArgv("restore-status")...).stdout); got != originalFP {
		t.Fatalf("restored fingerprint %q, want %q", got, originalFP)
	}
	back := d.p.start(t, planeArgs...)
	if r := d.ok("node ls", "node", "ls", "--plane", "https://"+back.addr, "--ca", caPath); !strings.Contains(r.stdout, oldNode) {
		t.Fatalf("restored roster = %q", r.stdout)
	}
	if code := back.stop(t, os.Interrupt); code != deployExitStopped {
		t.Fatalf("stop = %d", code)
	}

	// Branch two: a fresh plane at the same canonical path has a new CA.
	restoredBefore := treeInventory(t, state)
	d.vars["OLD_STATE"] = restored
	if err := d.guardedMove("recover-plane-move"); err != nil {
		t.Fatal(err)
	}
	sameInventory(t, "restored archive", treeInventory(t, restored), restoredBefore)
	d.apply("fresh-env")
	d.ok("fresh init", d.deployArgv("fresh-init")...)
	newFP := statusFingerprint(t, d.ok("status", d.deployArgv("state-status")...).stdout)
	if newFP == originalFP {
		t.Fatal("a fresh plane kept the old CA")
	}
	freshArgs, err := argsAfterLeaf(d.deployArgv("fresh-plane-run"), "plane", "run")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(freshArgs, planeArgs) {
		t.Fatalf("fresh plane run %q differs from the service's %q", freshArgs, planeArgs)
	}
	fresh := d.p.start(t, planeArgs...)
	url = "https://" + fresh.addr
	d.vars["PLANE"] = url

	// An isolated old CA copy for the negative trust assertion; the fixture's
	// client CA keeps its path with the new certificate.
	oldCA := filepath.Join(d.root, "old-ca.crt")
	if err := os.WriteFile(oldCA, nodeFile(t, caPath), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, nodeFile(t, filepath.Join(pki, "ca.crt")), 0o600); err != nil {
		t.Fatal(err)
	}
	d.vars["HOME"] = workerHome
	d.apply("fresh-ca-mode")
	d.vars["PLANE"] = url
	if d.vars["CA"] != caPath {
		t.Fatalf("documented CA changed path: %q", d.vars["CA"])
	}
	if r := d.p.run(t, "node", "ls", "--plane", url, "--ca", oldCA); r.code != deployExitTrust || !strings.Contains(r.stderr, deployUntrusted) {
		t.Fatalf("old CA = %+v", r)
	}
	d.ok("node ls", d.deployArgv("verify-plane")...)
	if r := d.ok("task ls", d.deployArgv("fresh-task-list")...); r.stdout != deployEmptyTaskList {
		t.Fatalf("fresh plane tasks = %q", r.stdout)
	}
	rolesOut := d.ok("role ls", d.withJSON("role-list", d.argv("role-list"))...)
	if roles, err := contract.ParseRoleListResponse([]byte(strings.TrimSuffix(rolesOut.stdout, "\n")), nil); err != nil || len(roles) != 0 {
		t.Fatalf("fresh plane roles = %q, %v", rolesOut.stdout, err)
	}

	// The stopped old sidecar holds one valid old-plane journal. QS names the
	// worker's callsheet directory, so the quickstart's manual paths are the
	// chapter's <worker-home>/callsheet/manuals.
	d.vars["QS"] = filepath.Dir(sidecarState)
	d.vars["MODEL"] = qsFakeModel
	instruction, runbook := filepath.Join(d.vars["QS"], "manuals", "instruction.md"), filepath.Join(d.vars["QS"], "manuals", "runbook.md")
	if err := os.WriteFile(instruction, []byte(qsInstruction), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runbook, []byte(qsRunbook), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTask := oldNodeJournal(t, sidecarState, oldNode, instruction, runbook)
	sidecarBefore := treeInventory(t, sidecarState)

	// The negative probe, on a private copy only: re-enrolled against the fresh
	// plane, it reports an execution that plane cannot know and stays unready.
	probe := filepath.Join(d.root, "probe-sidecar")
	if out, err := exec.Command("cp", "-Rp", sidecarState, probe).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v: %s", err, out)
	}
	d.vars["SIDECAR_STATE"] = probe
	if got := enrolledNode(t, d.ok("probe enroll", d.deployArgv("fresh-enroll")...)); got != oldNode {
		t.Fatalf("probe node %q, want %q", got, oldNode)
	}
	d.startSidecarArgs(d.deployArgv("fresh-sidecar-run"))
	probeProc := d.sidecar
	d.await("the old node online", func() bool {
		r := d.p.run(t, d.argv("node-list")...)
		return r.code == 0 && strings.Contains(r.stdout, oldNode+"\tonline\t")
	})
	d.addRole("old-probe", oldNode)
	d.await("the plane's unknown_execution", func() bool {
		return hasRecord(fresh.logs.String(), deployUnknownExec, map[string]any{"reason": contract.ReasonUnknownExecution, "task_id": oldTask})
	})
	refused := d.dispatchArgs("old-probe")
	d.await("a worker_unready refusal", func() bool {
		r := d.p.run(t, refused...)
		if r.code == 0 {
			t.Fatalf("dispatch to the old node was admitted: %+v", r)
		}
		if strings.Contains(r.stderr, "role_id=old-probe") && strings.Contains(r.stderr, "reason="+contract.ReasonWorkerUnready) {
			t.Logf("old node refused: exit %d: %s", r.code, r.stderr)
			return true
		}
		return false
	})
	if r := d.ok("task ls", d.deployArgv("fresh-task-list")...); r.stdout != deployEmptyTaskList {
		t.Fatalf("a refused dispatch admitted a task: %q", r.stdout)
	}
	if code := probeProc.stop(t, os.Interrupt); code != deployExitStopped {
		t.Fatalf("probe stop = %d", code)
	}
	if b := nodeFile(t, filepath.Join(probe, "tasks", oldTask, "execution.json")); !bytes.Contains(b, []byte(oldTask)) {
		t.Fatalf("probe journal not retained: %s", b)
	}
	sameInventory(t, "the original sidecar during the probe", treeInventory(t, sidecarState), sidecarBefore)

	// The operator recipe: archive the original, enroll fresh at the same path.
	d.vars["SIDECAR_STATE"] = sidecarState
	d.vars["OLD_SIDECAR_STATE"] = filepath.Join(filepath.Dir(sidecarState), "sidecar-old-recovered")
	archive := d.vars["OLD_SIDECAR_STATE"]
	if err := d.guardedMove("recover-sidecar-move"); err != nil {
		t.Fatal(err)
	}
	sameInventory(t, "sidecar archive", treeInventory(t, archive), sidecarBefore)
	if _, err := os.Lstat(sidecarState); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("canonical sidecar path remains: %v", err)
	}
	newNode := enrolledNode(t, d.ok("fresh enroll", d.deployArgv("fresh-enroll")...))
	if newNode == oldNode {
		t.Fatalf("fresh enrollment kept node %q", newNode)
	}
	if _, ca := enrollmentOf(t, sidecarState); ca != string(nodeFile(t, filepath.Join(pki, "ca.crt"))) {
		t.Fatal("fresh enrollment does not embed the new CA")
	}
	for _, leftover := range []string{"tasks", "workspace-cache"} {
		entries, err := os.ReadDir(filepath.Join(sidecarState, leftover))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("fresh %s holds %d entries", leftover, len(entries))
		}
	}

	// The service's own sidecar argv, restored capacity and one fake dispatch.
	svc := sampleArgv(t, workerUnit, vars)
	if want := d.deployArgvBeforeTranslation("fresh-sidecar-run"); !slices.Equal(svc, want) {
		t.Fatalf("service argv %q differs from the chapter's %q", svc, want)
	}
	d.startSidecarArgs(d.deployArgv("fresh-sidecar-run"))
	d.addRole("new-worker", newNode)
	d.await("the new role's can_accept", func() bool { return d.roleAccepts("new-worker", newNode) })
	admitted := d.ok("dispatch", d.withJSON("dispatch", d.dispatchArgs("new-worker"))...)
	task, err := contract.ParseDispatchResponse([]byte(strings.TrimSuffix(admitted.stdout, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	d.vars["TASK"] = task.TaskID
	d.ok("task wait", d.argv("wait")...)
	show := d.ok("task show", d.withJSON("task-show", d.argv("task-show"))...)
	done, err := contract.ParseTaskShowResponse([]byte(strings.TrimSuffix(show.stdout, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if done.State != contract.TaskSucceeded || done.Role.Node != newNode {
		t.Fatalf("task = %+v", done)
	}
	listed := d.ok("task ls", d.deployArgv("fresh-task-list")...)
	if !strings.Contains(listed.stdout, task.TaskID) || strings.Contains(listed.stdout, oldTask) {
		t.Fatalf("task ls = %q, want %s and not %s", listed.stdout, task.TaskID, oldTask)
	}
	t.Logf("fresh node %s (old %s) ran:\n%s", newNode, oldNode, listed.stdout)
	if r := d.p.run(t, refused...); r.code == 0 {
		t.Fatalf("dispatch to the old probe role was admitted: %+v", r)
	}

	// Everything preserved; the canonical paths hold only the new state; no
	// service definition changed.
	sameInventory(t, "damaged archive after recovery", treeInventory(t, damaged), before)
	sameInventory(t, "restored archive after recovery", treeInventory(t, restored), restoredBefore)
	sameInventory(t, "sidecar archive after recovery", treeInventory(t, archive), sidecarBefore)
	for _, canonical := range []string{filepath.Join(sidecarState, "tasks", oldTask)} {
		if _, err := os.Lstat(canonical); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("old journal in the active state: %v", err)
		}
	}
	argvAfter := [][]string{sampleArgv(t, planeUnit, vars), sampleArgv(t, workerUnit, vars), sampleArgv(t, planePlist, vars), sampleArgv(t, sidePlist, vars)}
	for i := range argvBefore {
		if !slices.Equal(argvBefore[i], argvAfter[i]) {
			t.Fatalf("service argv changed: %q to %q", argvBefore[i], argvAfter[i])
		}
	}
	if code := d.sidecar.stop(t, os.Interrupt); code != deployExitStopped {
		t.Fatalf("sidecar stop = %d", code)
	}
	if code := fresh.stop(t, os.Interrupt); code != deployExitStopped {
		t.Fatalf("plane stop = %d", code)
	}
}

// deployArgvBeforeTranslation is fence id's expanded argv with no fixture
// translation, for comparing a document command with a rendered sample.
func (d *deployRig) deployArgvBeforeTranslation(id string) []string {
	d.t.Helper()
	p := d.parsed(id)
	if len(p.callsheet) != 1 {
		d.t.Fatalf("fence %s: %d commands", id, len(p.callsheet))
	}
	var out []string
	for _, w := range p.callsheet[0].words[1:] {
		out = append(out, d.expand(w))
	}
	return out
}

// runDeployCopy runs a stopped backup or restore fence's exact cp argv.
func runDeployCopy(t *testing.T, d *deployRig, id string) {
	t.Helper()
	p := d.parsed(id)
	if len(p.setup) != 1 {
		t.Fatalf("fence %s: %d operations", id, len(p.setup))
	}
	var argv []string
	for _, w := range p.setup[0].words {
		argv = append(argv, d.expand(w))
	}
	if argv[0] != "cp" {
		t.Fatalf("fence %s runs %q", id, argv[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), qsWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 5 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%q: %v: %s", argv, err, out)
	}
}

// checkRestoreDiagnostics proves the two permission diagnostics the chapter quotes,
// on a throwaway copy of the restored state, and that an owner-only root other than
// 0700 is accepted as the chapter says.
func checkRestoreDiagnostics(t *testing.T, d *deployRig, state string) {
	t.Helper()
	copyDir := filepath.Join(d.root, "permission-probe")
	if out, err := exec.Command("cp", "-Rp", state, copyDir).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v: %s", err, out)
	}
	if err := os.Chmod(copyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	r := d.p.run(t, "plane", "status", "--state-dir", copyDir)
	if want := fmt.Sprintf(deployRootDiag, copyDir, 0o750); r.code == 0 || !strings.Contains(r.stderr, want) || !strings.Contains(r.stderr, string(contract.CodeTrustFailed)) {
		t.Fatalf("group-readable root = %+v, want %q", r, want)
	}
	if err := os.Chmod(copyDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if r := d.p.run(t, "plane", "status", "--state-dir", copyDir); r.code != 0 {
		t.Fatalf("owner-only root 0500 = %+v", r)
	}
	if err := os.Chmod(copyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(copyDir, "pki", "ca.key")
	if err := os.Chmod(key, 0o640); err != nil {
		t.Fatal(err)
	}
	r = d.p.run(t, "plane", "status", "--state-dir", copyDir)
	if want := fmt.Sprintf(deployKeyDiag, key, 0o640); r.code == 0 || !strings.Contains(r.stderr, want) {
		t.Fatalf("wide key = %+v, want %q", r, want)
	}
	if err := os.Chmod(key, 0o400); err != nil {
		t.Fatal(err)
	}
	if r := d.p.run(t, "plane", "status", "--state-dir", copyDir); r.code != 0 {
		t.Fatalf("corrected permissions = %+v", r)
	}
}

// ---- FP-4 and FP-5: the service samples.

// sampleVars are the only substitutions a sample allows: the account homes, the
// Callsheet path and the vendor executable. Rendering cannot repair a bad flag or
// strip an extra argument: argv is compared with the expected shape afterwards.
type sampleVars struct {
	planeHome, workerHome, callsheet, workerPath string
}

// sampleTemplate is the placeholder state of a shipped sample.
var sampleTemplate = sampleVars{planeHome: "/REPLACE_WITH_PLANE_HOME", workerHome: "/REPLACE_WITH_WORKER_HOME", callsheet: "/usr/local/bin/callsheet", workerPath: "/REPLACE_WITH_ABSOLUTE_WORKER_PATH"}

// renderSample replaces only the declared tokens; a newline or NUL is refused, and
// plist replacements are XML escaped.
func renderSample(text string, v sampleVars) (string, error) {
	isPlist := strings.HasPrefix(text, "<?xml")
	esc := func(s string) string {
		if !isPlist {
			return s
		}
		var b strings.Builder
		if err := xml.EscapeText(&b, []byte(s)); err != nil {
			return s
		}
		return b.String()
	}
	for _, s := range []string{v.planeHome, v.workerHome, v.callsheet, v.workerPath} {
		if strings.ContainsAny(s, "\n\x00") {
			return "", fmt.Errorf("render: replacement %q holds a newline or NUL", s)
		}
	}
	return strings.NewReplacer(
		sampleTemplate.planeHome, esc(v.planeHome),
		sampleTemplate.workerHome, esc(v.workerHome),
		sampleTemplate.callsheet, esc(v.callsheet),
		sampleTemplate.workerPath, esc(v.workerPath),
	).Replace(text), nil
}

// sampleSpec is one sample's expected values under vars.
type sampleSpec struct {
	label, user, home, state, stdout, stderr string
	argv                                     []string
}

func sampleSpecOf(name string, v sampleVars) (sampleSpec, error) {
	switch name {
	case planeUnit, planePlist:
		state := v.planeHome + "/callsheet/plane"
		return sampleSpec{label: "org.callsheet.plane", user: "callsheet-plane", home: v.planeHome, state: state,
			stdout: v.planeHome + "/callsheet/logs/plane.out.log", stderr: v.planeHome + "/callsheet/logs/plane.err.log",
			argv: []string{"plane", "run", "--state-dir", state}}, nil
	case workerUnit, sidePlist:
		state := v.workerHome + "/callsheet/sidecar"
		return sampleSpec{label: "org.callsheet.sidecar", user: "callsheet-worker", home: v.workerHome, state: state,
			stdout: v.workerHome + "/callsheet/logs/sidecar.out.log", stderr: v.workerHome + "/callsheet/logs/sidecar.err.log",
			argv: []string{"sidecar", "run", "--state-dir", state, "--claude-adapter", v.workerPath}}, nil
	}
	return sampleSpec{}, fmt.Errorf("no sample %s", name)
}

const samplePath = "/usr/local/bin:/usr/bin:/bin"

// unitFile is a parsed unit: only sections and key/value entries.
type unitFile map[string]map[string][]string

// unitRepeatable are the only keys that may appear more than once.
var unitRepeatable = []string{"Environment"}

// parseUnit parses the narrow unit subset, rejecting anything else.
func parseUnit(text string) (unitFile, error) {
	u := unitFile{}
	section := ""
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("unit line %d: malformed section %q", i+1, line)
			}
			section = strings.Trim(line, "[]")
			if u[section] != nil {
				return nil, fmt.Errorf("unit line %d: duplicate section %q", i+1, section)
			}
			u[section] = map[string][]string{}
		default:
			key, val, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				return nil, fmt.Errorf("unit line %d: not a directive: %q", i+1, line)
			}
			if len(u[section][key]) > 0 && !slices.Contains(unitRepeatable, key) {
				return nil, fmt.Errorf("unit line %d: duplicate directive %s", i+1, key)
			}
			u[section][key] = append(u[section][key], val)
		}
	}
	return u, nil
}

// unitArgv tokenizes the single ExecStart without shell expansion and normalizes
// the marked executable path.
func unitArgv(name string, u unitFile, v sampleVars) ([]string, error) {
	vals := u["Service"]["ExecStart"]
	if len(vals) != 1 {
		return nil, fmt.Errorf("systemd %s: %d ExecStart lines, want 1", name, len(vals))
	}
	cmds, err := tokenizeShell(name, vals[0], 1, false)
	if err != nil {
		return nil, fmt.Errorf("systemd %s: ExecStart: %w", name, err)
	}
	if len(cmds) != 1 {
		return nil, fmt.Errorf("systemd %s: ExecStart holds %d commands", name, len(cmds))
	}
	var argv []string
	for _, w := range cmds[0].words {
		s, ok := w.literal()
		if !ok {
			return nil, fmt.Errorf("systemd %s: ExecStart holds a variable", name)
		}
		argv = append(argv, s)
	}
	if len(argv) == 0 || argv[0] != v.callsheet {
		return nil, fmt.Errorf("systemd %s: ExecStart runs %q, want %s", name, argv, v.callsheet)
	}
	return argv[1:], nil
}

// checkSystemdSample is FP-4's sample validator: the exact narrow schema for a
// template or a rendered copy.
func checkSystemdSample(name, text string, v sampleVars) error {
	u, err := parseUnit(text)
	if err != nil {
		return fmt.Errorf("systemd %s: %w", name, err)
	}
	spec, err := sampleSpecOf(name, v)
	if err != nil {
		return err
	}
	want := unitFile{
		"Unit": {"Description": {map[string]string{planeUnit: "Callsheet plane", workerUnit: "Callsheet sidecar"}[name]}, "After": {"network-online.target"}, "Wants": {"network-online.target"}},
		"Service": {"Type": {"simple"}, "User": {spec.user}, "Environment": {"HOME=" + spec.home, "PATH=" + samplePath}, "WorkingDirectory": {spec.home}, "UMask": {"0077"},
			"Restart": {"on-failure"}, "RestartSec": {"5s"}, "SuccessExitStatus": {"130"}, "KillMode": {"control-group"}, "TimeoutStopSec": {"60s"},
			"StandardOutput": {"journal"}, "StandardError": {"journal"}},
		"Install": {"WantedBy": {"multi-user.target"}},
	}
	var errs []error
	for section := range u {
		if want[section] == nil {
			errs = append(errs, fmt.Errorf("systemd %s: unexpected section [%s]", name, section))
		}
	}
	for section, keys := range want {
		for key, vals := range keys {
			if got := u[section][key]; !slices.Equal(got, vals) {
				errs = append(errs, fmt.Errorf("systemd %s: %s = %q, want %q", name, key, got, vals))
			}
		}
		for key := range u[section] {
			if keys[key] == nil && key != "ExecStart" {
				errs = append(errs, fmt.Errorf("systemd %s: unexpected directive %s", name, key))
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	argv, err := unitArgv(name, u, v)
	if err != nil {
		return err
	}
	if !slices.Equal(argv, spec.argv) {
		return fmt.Errorf("systemd %s: argv %q, want %q", name, argv, spec.argv)
	}
	return nil
}

// deployServiceFences are the exact bodies of every operation row: service-manager
// and account recipes, file operations and environment setup. The allowlist in
// checkDeployOperation only bounds the kinds of operation; this pins each one.
var deployServiceFences = map[string]string{
	"linux-accounts":      "sudo useradd --create-home --shell /bin/bash callsheet-plane\nsudo useradd --create-home --shell /bin/bash callsheet-worker",
	"install-binary":      `sudo install -m 0755 "$CALLSHEET" /usr/local/bin/callsheet`,
	"login-plane":         "sudo -iu callsheet-plane",
	"login-worker":        "sudo -iu callsheet-worker",
	"plane-dirs":          "mkdir -m 700 \"$HOME/callsheet\"\nmkdir -m 700 \"$HOME/callsheet/logs\"",
	"worker-dirs":         "mkdir -m 700 \"$HOME/callsheet\"\nmkdir -m 700 \"$HOME/callsheet/logs\"\nmkdir -m 700 \"$HOME/callsheet/manuals\"",
	"plane-env":           "STATE=\"$HOME/callsheet/plane\"\nread -r PLANE_IP\nPLANE=\"https://$PLANE_IP:8443\"",
	"worker-env":          "SIDECAR_STATE=\"$HOME/callsheet/sidecar\"\nCA=\"$HOME/callsheet/plane-ca.crt\"\nread -r PLANE_IP\nPLANE=\"https://$PLANE_IP:8443\"\nread -r WORKER\n\"$WORKER\" --version",
	"linux-check":         "grep -n REPLACE_WITH callsheet-plane.service\ngrep -n REPLACE_WITH callsheet-sidecar.service",
	"linux-lint":          "systemd-analyze verify callsheet-plane.service callsheet-sidecar.service",
	"linux-install":       "sudo install -m 0644 callsheet-plane.service /etc/systemd/system/callsheet-plane.service\nsudo install -m 0644 callsheet-sidecar.service /etc/systemd/system/callsheet-sidecar.service\nsudo systemctl daemon-reload",
	"linux-start-plane":   "sudo systemctl enable --now callsheet-plane.service\nsudo systemctl status callsheet-plane.service",
	"linux-start-sidecar": "sudo systemctl enable --now callsheet-sidecar.service\nsudo systemctl status callsheet-sidecar.service",
	"linux-logs":          "sudo journalctl -u callsheet-plane.service\nsudo journalctl -u callsheet-sidecar.service",
	"linux-stop":          "sudo systemctl stop callsheet-sidecar.service\nsudo systemctl stop callsheet-plane.service",
	"linux-start":         "sudo systemctl start callsheet-plane.service\nsudo systemctl start callsheet-sidecar.service",
	"linux-uninstall":     "sudo systemctl disable --now callsheet-sidecar.service\nsudo systemctl disable --now callsheet-plane.service\nsudo rm /etc/systemd/system/callsheet-sidecar.service\nsudo rm /etc/systemd/system/callsheet-plane.service\nsudo systemctl daemon-reload",
	"confirm-exit":        "ps -u callsheet-worker\nps -u callsheet-plane",
	"macos-logs-plane":    "touch \"$HOME/callsheet/logs/plane.out.log\"\ntouch \"$HOME/callsheet/logs/plane.err.log\"\nchmod 600 \"$HOME/callsheet/logs/plane.out.log\"\nchmod 600 \"$HOME/callsheet/logs/plane.err.log\"",
	"macos-logs-worker":   "touch \"$HOME/callsheet/logs/sidecar.out.log\"\ntouch \"$HOME/callsheet/logs/sidecar.err.log\"\nchmod 600 \"$HOME/callsheet/logs/sidecar.out.log\"\nchmod 600 \"$HOME/callsheet/logs/sidecar.err.log\"",
	"macos-check":         "grep -n REPLACE_WITH org.callsheet.plane.plist\ngrep -n REPLACE_WITH org.callsheet.sidecar.plist\nplutil -lint org.callsheet.plane.plist\nplutil -lint org.callsheet.sidecar.plist",
	"macos-install":       "sudo install -m 0644 org.callsheet.plane.plist /Library/LaunchDaemons/org.callsheet.plane.plist\nsudo install -m 0644 org.callsheet.sidecar.plist /Library/LaunchDaemons/org.callsheet.sidecar.plist",
	"macos-start-plane":   "sudo launchctl bootstrap system /Library/LaunchDaemons/org.callsheet.plane.plist\nsudo launchctl print system/org.callsheet.plane",
	"macos-start-sidecar": "sudo launchctl bootstrap system /Library/LaunchDaemons/org.callsheet.sidecar.plist\nsudo launchctl print system/org.callsheet.sidecar",
	"macos-stop":          "sudo launchctl bootout system/org.callsheet.sidecar\nsudo launchctl bootout system/org.callsheet.plane",
	"macos-remove":        "sudo rm /Library/LaunchDaemons/org.callsheet.sidecar.plist\nsudo rm /Library/LaunchDaemons/org.callsheet.plane.plist",
	"macos-read-logs":     "tail -n 50 \"$HOME/callsheet/logs/plane.err.log\"\ntail -n 50 \"$HOME/callsheet/logs/plane.out.log\"",

	"ca-mode":               `chmod 600 "$CA"`,
	"backup-env":            "STATE=\"$HOME/callsheet/plane\"\nBACKUP_STATE=\"$HOME/callsheet/plane-backup-REPLACE_WITH_DATE\"",
	"backup-plane":          `cp -Rp "$STATE" "$BACKUP_STATE"`,
	"restore-select":        `BACKUP_STATE="$HOME/callsheet/plane-backup-REPLACE_WITH_DATE"`,
	"restore-backup":        `cp -Rp "$BACKUP_STATE" "$STATE"`,
	"recover-plane-paths":   "STATE=\"$HOME/callsheet/plane\"\nOLD_STATE=\"$HOME/callsheet/plane-old-REPLACE_WITH_DATE\"",
	"recover-plane-mount":   `df -P "$STATE" "$HOME"`,
	"recover-plane-move":    deployMoveBody("STATE", "OLD_STATE"),
	"fresh-env":             "read -r PLANE_IP\nPLANE=\"https://$PLANE_IP:8443\"",
	"recover-sidecar-paths": "SIDECAR_STATE=\"$HOME/callsheet/sidecar\"\nOLD_SIDECAR_STATE=\"$HOME/callsheet/sidecar-old-REPLACE_WITH_DATE\"",
	"recover-sidecar-mount": `df -P "$SIDECAR_STATE" "$HOME"`,
	"recover-sidecar-move":  deployMoveBody("SIDECAR_STATE", "OLD_SIDECAR_STATE"),
	"fresh-ca-mode":         "CA=\"$HOME/callsheet/plane-ca.crt\"\nchmod 600 \"$CA\"\nread -r PLANE_IP\nPLANE=\"https://$PLANE_IP:8443\"",
}

// checkOperationBodies requires every operation row's exact pinned body.
func checkOperationBodies(md string) error {
	var errs []error
	for _, r := range deployFences {
		if len(r.leaves) > 0 {
			continue
		}
		if _, ok := deployServiceFences[r.id]; !ok {
			errs = append(errs, fmt.Errorf("inventory: operation fence %s has no pinned body", r.id))
			continue
		}
		errs = append(errs, wantFenceBody(md, r.id, deployServiceFences[r.id]))
	}
	return errors.Join(errs...)
}

func checkServiceFences(md string, ids ...string) error {
	var errs []error
	for _, id := range ids {
		errs = append(errs, wantFenceBody(md, id, deployServiceFences[id]))
	}
	return errors.Join(errs...)
}

// checkAccountsDoc is the shared account, permission and authentication policy.
func checkAccountsDoc(md string) error {
	body, err := sectionOf(md, "Run as services")
	if err != nil {
		return err
	}
	text := stripInlineLinks(body)
	var errs []error
	errs = append(errs, needAll("services", text,
		"two distinct non-administrator accounts", "`callsheet-plane` and `callsheet-worker`", "a dedicated worker must never inherit the human operator's HOME", "Do not run either process as root.",
		"OS administrator privileges are for account creation, shared binary installation and service registration only.", "they never run Callsheet as root",
		"Ubuntu/systemd example, from `useradd --help` on Ubuntu 24.04", "System Settings > Users & Groups", "The required outcome is the account name, a real home directory and no administrator rights",
		"There are only three kinds of replacement", "Existing tutorial state is not silently moved or chowned", "Never create a second sidecar identity by accidentally changing its state directory.",
		"Service files (root-owned, 0644) hold no secrets.", "do not recursively open plane permissions to the worker", "do not recursively chmod vendor credential stores",
		"Initialize the state as the plane user, before enabling its service.", "enroll before enabling the sidecar's service",
		"Authentication is a prerequisite, not something inferred from `--version` or role readiness.", "exact account, executable, HOME, PATH and model and effort",
		"Do not copy the operator's credential files or place tokens in units or plists, and do not disable vendor safeguards on a failure.",
		"is a baseline, not a guarantee for every installation", "Never rely on shell profile initialization",
		"**Unverified:** whether a selected vendor's credentials", "Settle it with a real task launched through the installed service, and again after a reboot before any login to the worker account.",
		"To use another vendor, change the single adapter flag and path.",
		"Maintenance is idle-only: let tasks finish, stop the sidecar and then the plane through their manager, and verify exit before editing state.", "do not expect active tasks to survive a sidecar restart",
		"Log retention and rotation are your responsibility, not an automatic Callsheet feature."))
	errs = append(errs, checkServiceFences(md, "linux-accounts", "install-binary", "login-plane", "login-worker", "plane-dirs", "worker-dirs", "plane-env", "worker-env", "confirm-exit"))
	errs = append(errs, wantDeployArgs(md, "plane-init", "callsheet", "plane", "init", "--state-dir", "$STATE", "--bind", "$PLANE_IP:8443", "--san", "127.0.0.1", "--san", "$PLANE_IP"))
	errs = append(errs, wantDeployArgs(md, "worker-enroll", "callsheet", "sidecar", "enroll", "--state-dir", "$SIDECAR_STATE", "--plane", "$PLANE", "--ca", "$CA"))
	for _, name := range []string{planeUnit, workerUnit, planePlist, sidePlist} {
		if !slices.Contains(markdownLinks(body), "examples/deploy/"+name) {
			errs = append(errs, fmt.Errorf("services: no link to the sample %s", name))
		}
	}
	return errors.Join(errs...)
}

// checkSystemdDoc is FP-4's chapter validator.
func checkSystemdDoc(md string) error {
	body, err := sectionOf(md, "Run as services")
	if err != nil {
		return err
	}
	text := stripInlineLinks(body)
	var errs []error
	errs = append(errs, checkAccountsDoc(md))
	errs = append(errs, needAll("systemd", text,
		"Install system units, not user units.", "No `REPLACE_WITH` token may remain", "A running or restarting unit is not proof of readiness.",
		"A service-manager stop does not request a restart.", "`SuccessExitStatus=130` acknowledges Callsheet's documented interrupt exit.",
		"is not proof that a VPN address is ready", "A sidecar unit has no dependency on a plane unit", "Editing a unit requires stop, `daemon-reload`, start.",
		"disable and remove only the unit files; state stays where it is", "It is not a promise that an active task is preserved."))
	errs = append(errs, checkServiceFences(md, "linux-check", "linux-lint", "linux-install", "linux-start-plane", "linux-start-sidecar", "linux-logs", "linux-stop", "linux-start", "linux-uninstall"))
	errs = append(errs, inOrder("systemd", body, "deploy: linux-check", "deploy: linux-lint", "deploy: linux-install", "deploy: linux-start-plane", "deploy: verify-plane", "deploy: worker-enroll", "deploy: linux-start-sidecar", "deploy: verify-roster"))
	for _, l := range deployServiceSources[:4] {
		if !slices.Contains(markdownLinks(body), l) {
			errs = append(errs, fmt.Errorf("systemd: source %s is not cited", l))
		}
	}
	errs = append(errs, forbidAll("systemd", body, "dynamicuser", "statedirectory=", "user=root", "--user enable"))
	return errors.Join(errs...)
}

// deployServiceSources are the external references the chapter cites.
var deployServiceSources = []string{
	"https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html",
	"https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html",
	"https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html",
	"https://www.freedesktop.org/software/systemd/man/latest/systemd-analyze.html",
	"https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html",
	"https://raw.githubusercontent.com/apple-oss-distributions/launchd/main/man/launchd.plist.5",
	"https://it-training.apple.com/compliance/tutorials/course/sec050/",
}

// sampleMutation is one in-memory change to a sample and its required diagnostic.
type sampleMutation struct {
	name, sample, old, repl, want string
}

// runSampleMutations proves each target exists, then that check rejects the result.
func runSampleMutations(t *testing.T, check func(name, text string) error, ms []sampleMutation) {
	t.Helper()
	for _, m := range ms {
		t.Run(m.name, func(t *testing.T) {
			text := string(repoFile(t, sampleDir+m.sample))
			if !strings.Contains(text, m.old) {
				t.Fatalf("mutation target %q not found", m.old)
			}
			err := check(m.sample, strings.Replace(text, m.old, m.repl, 1))
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("err = %v, want one containing %q", err, m.want)
			}
			t.Logf("rejected: %v", firstLine(err))
		})
	}
}

// TestDeploySystemdSamples (FP-4): both shipped units, rendered too, and the
// chapter's account, installation and lifecycle instructions. Structural only: no
// unit is installed or started here.
func TestDeploySystemdSamples(t *testing.T) {
	md := string(repoFile(t, deployDoc))
	check := func(name, text string) error { return checkSystemdSample(name, text, sampleTemplate) }
	for _, name := range []string{planeUnit, workerUnit} {
		text := string(repoFile(t, sampleDir+name))
		if err := check(name, text); err != nil {
			t.Fatalf("shipped sample: %v", err)
		}
		v := sampleVars{planeHome: "/srv/plane-account", workerHome: "/srv/worker-account", callsheet: "/opt/tools/callsheet", workerPath: "/opt/tools/claude"}
		rendered, err := renderSample(text, v)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkSystemdSample(name, rendered, v); err != nil {
			t.Fatalf("rendered sample: %v", err)
		}
		if strings.Contains(rendered, "REPLACE_WITH") {
			t.Fatalf("rendered %s keeps a placeholder", name)
		}
		if !strings.HasPrefix(text, "# Callsheet ") || !strings.Contains(text, "REPLACE_WITH") || !strings.Contains(text, "docs/deploy.md") {
			t.Errorf("%s: no leading replacement comment", name)
		}
	}
	if err := checkSystemdDoc(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	if _, err := renderSample("x", sampleVars{planeHome: "/a\nb"}); err == nil || !strings.Contains(err.Error(), "newline or NUL") {
		t.Errorf("newline: %v", err)
	}
	t.Run("sample-mutations", func(t *testing.T) {
		runSampleMutations(t, check, []sampleMutation{
			{"root-worker", workerUnit, "User=callsheet-worker", "User=root", "User ="},
			{"root-plane", planeUnit, "User=callsheet-plane", "User=root", "User ="},
			{"missing-home", workerUnit, "Environment=HOME=/REPLACE_WITH_WORKER_HOME\n", "", "Environment ="},
			{"human-home", workerUnit, "Environment=HOME=/REPLACE_WITH_WORKER_HOME", "Environment=HOME=/REPLACE_WITH_PLANE_HOME", "Environment ="},
			{"missing-adapter", workerUnit, " --claude-adapter /REPLACE_WITH_ABSOLUTE_WORKER_PATH", "", "argv"},
			{"missing-state", planeUnit, " --state-dir /REPLACE_WITH_PLANE_HOME/callsheet/plane", "", "argv"},
			{"state-outside-home", workerUnit, "--state-dir /REPLACE_WITH_WORKER_HOME/callsheet/sidecar", "--state-dir /REPLACE_WITH_PLANE_HOME/callsheet/sidecar", "argv"},
			{"init-hook", planeUnit, "ExecStart=", "ExecStartPre=/usr/local/bin/callsheet plane init\nExecStart=", "unexpected directive ExecStartPre"},
			{"first-start-sans", planeUnit, "callsheet/plane\n", "callsheet/plane --san 127.0.0.1\n", "argv"},
			{"unsafe-umask", planeUnit, "UMask=0077", "UMask=0022", "UMask ="},
			{"shell-wrapper", planeUnit, "ExecStart=/usr/local/bin/callsheet plane run", "ExecStart=/bin/sh -c /usr/local/bin/callsheet plane run", "ExecStart runs"},
			{"shell-chain", planeUnit, "callsheet/plane\n", "callsheet/plane; true\n", "semicolon chain"},
			{"background", planeUnit, "callsheet/plane\n", "callsheet/plane &\n", "unsupported shell syntax"},
			{"environment-expansion", workerUnit, "--claude-adapter /REPLACE_WITH_ABSOLUTE_WORKER_PATH", "--claude-adapter $WORKER", "holds a variable"},
			{"always-restart", planeUnit, "Restart=on-failure", "Restart=always", "Restart ="},
			{"duplicate-directive", planeUnit, "RestartSec=5s", "RestartSec=5s\nRestartSec=1s", "duplicate directive RestartSec"},
			{"unknown-directive", planeUnit, "RestartSec=5s", "RestartSec=5s\nDynamicUser=yes", "unexpected directive DynamicUser"},
			{"unknown-section", planeUnit, "[Install]", "[Socket]\nListenStream=8443\n\n[Install]", "unexpected section [Socket]"},
			{"second-execstart", planeUnit, "Restart=on-failure", "ExecStart=/usr/local/bin/callsheet plane status\nRestart=on-failure", "duplicate directive ExecStart"},
			{"not-a-directive", planeUnit, "Restart=on-failure", "Restart on-failure", "not a directive"},
			{"remote-plane-dependency", workerUnit, "Wants=network-online.target", "Wants=network-online.target callsheet-plane.service", "Wants ="},
		})
	})
	t.Run("document-mutations", func(t *testing.T) {
		const sec = "Run as services"
		drop := func(id string) func(*testing.T, string) string {
			return func(t *testing.T, md string) string {
				t.Helper()
				b, _ := markedFenceBlock(t, md, "<!-- deploy: "+id+" -->")
				return strings.Replace(md, b, "", 1)
			}
		}
		runFPMutations(t, md, checkSystemdDoc, []readmeMutation{
			{"missing-stop-procedure", drop("linux-stop"), "fence linux-stop is missing"},
			{"stop-plane-first", inSection(sec, "sudo systemctl stop callsheet-sidecar.service\nsudo systemctl stop callsheet-plane.service", "sudo systemctl stop callsheet-plane.service\nsudo systemctl stop callsheet-sidecar.service"), "fence linux-stop ="},
			{"missing-install", drop("linux-install"), "fence linux-install is missing"},
			{"user-units", inSection(sec, "Install system units, not user units.", "Install user units."), "systemd: lacks"},
			{"root-execution", inSection(sec, "Do not run either process as root.", "Run the plane as root."), "services: lacks"},
			{"no-auth-prerequisite", inSection(sec, "Authentication is a prerequisite, not something inferred from `--version` or role readiness.", "Readiness proves authentication."), "services: lacks"},
			{"copied-credentials", inSection(sec, "Do not copy the operator's credential files or place tokens in units or plists, and do not disable vendor safeguards on a failure.", "Copy the operator's credential files."), "services: lacks"},
			{"reboot-unverified-removed", inSection(sec, "**Unverified:** whether a selected vendor's credentials", "Whether a selected vendor's credentials"), "services: lacks"},
			{"restart-means-ready", inSection(sec, "A running or restarting unit is not proof of readiness.", "A running unit is ready."), "systemd: lacks"},
			{"uninstall-deletes-state", inSection(sec, "disable and remove only the unit files; state stays where it is", "disable the units and delete the state"), "systemd: lacks"},
			{"uncited-source", inSection(sec, "(https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html)", "(https://example.invalid/kill)"), "systemd: source"},
			{"no-sample-link", inSection(sec, "(examples/deploy/callsheet-sidecar.service)", "(examples/deploy/other.service)"), "services: no link to the sample callsheet-sidecar.service"},
			{"enroll-after-start", func(t *testing.T, md string) string {
				t.Helper()
				a, _ := markedFenceBlock(t, md, "<!-- deploy: worker-enroll -->")
				b, _ := markedFenceBlock(t, md, "<!-- deploy: linux-start-sidecar -->")
				md = strings.Replace(md, a, "@@A@@", 1)
				md = strings.Replace(md, b, a, 1)
				return strings.Replace(md, "@@A@@", b, 1)
			}, "out of order"},
			{"dynamic-user", appendToSection(sec, "Set DynamicUser=yes."), "systemd: forbidden \"dynamicuser\""},
		})
	})
}

// plistValue is the restricted typed plist representation.
type plistValue struct {
	kind string // string, integer, bool, array or dict
	text string
	bool bool
	arr  []plistValue
	dict map[string]plistValue
}

// parsePlist decodes the XML plist subset with no DTD fetch: the standard DOCTYPE
// is inert text; duplicate keys, wrong shapes and any other element are errors.
func parsePlist(text string) (plistValue, error) {
	dec := xml.NewDecoder(strings.NewReader(text))
	var root *plistValue
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return plistValue{}, fmt.Errorf("plist: malformed XML: %w", err)
		}
		switch tt := tok.(type) {
		case xml.ProcInst, xml.Comment:
		case xml.Directive:
			if !strings.HasPrefix(string(tt), "DOCTYPE plist ") {
				return plistValue{}, fmt.Errorf("plist: unexpected directive %q", tt)
			}
		case xml.CharData:
			if strings.TrimSpace(string(tt)) != "" {
				return plistValue{}, errors.New("plist: text outside the root element")
			}
		case xml.StartElement:
			if tt.Name.Local != "plist" || root != nil {
				return plistValue{}, fmt.Errorf("plist: unexpected root element %s", tt.Name.Local)
			}
			if len(tt.Attr) != 1 || tt.Attr[0].Name.Local != "version" || tt.Attr[0].Value != "1.0" {
				return plistValue{}, errors.New("plist: root must be version 1.0")
			}
			v, end, err := plistNext(dec)
			if err != nil {
				return plistValue{}, err
			}
			if end || v.kind != "dict" {
				return plistValue{}, errors.New("plist: the root must hold one dict")
			}
			if _, end, err := plistNext(dec); err != nil {
				return plistValue{}, err
			} else if !end {
				return plistValue{}, errors.New("plist: the root holds more than one value")
			}
			root = &v
		default:
			return plistValue{}, fmt.Errorf("plist: unexpected token %T", tok)
		}
	}
	if root == nil {
		return plistValue{}, errors.New("plist: no root element")
	}
	return *root, nil
}

// plistNext reads the next value, or end=true at the parent's end element.
func plistNext(dec *xml.Decoder) (plistValue, bool, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return plistValue{}, false, fmt.Errorf("plist: malformed XML: %w", err)
		}
		switch tt := tok.(type) {
		case xml.Comment:
		case xml.CharData:
			if strings.TrimSpace(string(tt)) != "" {
				return plistValue{}, false, errors.New("plist: stray text")
			}
		case xml.EndElement:
			return plistValue{}, true, nil
		case xml.StartElement:
			v, err := plistElement(dec, tt)
			return v, false, err
		default:
			return plistValue{}, false, fmt.Errorf("plist: unexpected token %T", tok)
		}
	}
}

func plistElement(dec *xml.Decoder, start xml.StartElement) (plistValue, error) {
	switch start.Name.Local {
	case "string", "integer":
		var s string
		if err := dec.DecodeElement(&s, &start); err != nil {
			return plistValue{}, fmt.Errorf("plist: malformed XML: %w", err)
		}
		return plistValue{kind: start.Name.Local, text: s}, nil
	case "true", "false":
		if err := dec.Skip(); err != nil {
			return plistValue{}, fmt.Errorf("plist: malformed XML: %w", err)
		}
		return plistValue{kind: "bool", bool: start.Name.Local == "true"}, nil
	case "array":
		v := plistValue{kind: "array"}
		for {
			item, end, err := plistNext(dec)
			if err != nil {
				return plistValue{}, err
			}
			if end {
				return v, nil
			}
			v.arr = append(v.arr, item)
		}
	case "dict":
		v := plistValue{kind: "dict", dict: map[string]plistValue{}}
		for {
			k, end, err := plistDictKey(dec)
			if err != nil {
				return plistValue{}, err
			}
			if end {
				return v, nil
			}
			if _, dup := v.dict[k]; dup {
				return plistValue{}, fmt.Errorf("plist: duplicate key %s", k)
			}
			item, end, err := plistNext(dec)
			if err != nil {
				return plistValue{}, err
			}
			if end {
				return plistValue{}, fmt.Errorf("plist: key %s has no value", k)
			}
			v.dict[k] = item
		}
	}
	return plistValue{}, fmt.Errorf("plist: unsupported element %s", start.Name.Local)
}

func plistDictKey(dec *xml.Decoder) (string, bool, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false, fmt.Errorf("plist: malformed XML: %w", err)
		}
		switch tt := tok.(type) {
		case xml.Comment:
		case xml.CharData:
			if strings.TrimSpace(string(tt)) != "" {
				return "", false, errors.New("plist: stray text")
			}
		case xml.EndElement:
			return "", true, nil
		case xml.StartElement:
			if tt.Name.Local != "key" {
				return "", false, fmt.Errorf("plist: a dict holds %s where a key belongs", tt.Name.Local)
			}
			var k string
			if err := dec.DecodeElement(&k, &tt); err != nil {
				return "", false, fmt.Errorf("plist: malformed XML: %w", err)
			}
			return k, false, nil
		default:
			return "", false, fmt.Errorf("plist: unexpected token %T", tok)
		}
	}
}

// plistArgv reads ProgramArguments without shell tokenization and normalizes the
// marked executable path.
func plistArgv(name string, root plistValue, v sampleVars) ([]string, error) {
	pa, ok := root.dict["ProgramArguments"]
	if !ok {
		return nil, fmt.Errorf("launchd %s: no ProgramArguments", name)
	}
	if pa.kind != "array" {
		return nil, fmt.Errorf("launchd %s: ProgramArguments is %s, want array", name, pa.kind)
	}
	var argv []string
	for _, item := range pa.arr {
		if item.kind != "string" {
			return nil, fmt.Errorf("launchd %s: ProgramArguments holds %s, want string", name, item.kind)
		}
		argv = append(argv, item.text)
	}
	if len(argv) == 0 || argv[0] != v.callsheet {
		return nil, fmt.Errorf("launchd %s: ProgramArguments runs %q, want %s", name, argv, v.callsheet)
	}
	return argv[1:], nil
}

// checkLaunchdSample is FP-5's sample validator: exact keys, types and values.
func checkLaunchdSample(name, text string, v sampleVars) error {
	root, err := parsePlist(text)
	if err != nil {
		return fmt.Errorf("launchd %s: %w", name, err)
	}
	spec, err := sampleSpecOf(name, v)
	if err != nil {
		return err
	}
	type want struct{ kind, text string }
	strs := map[string]want{
		"Label": {"string", spec.label}, "UserName": {"string", spec.user}, "WorkingDirectory": {"string", spec.home},
		"ThrottleInterval": {"integer", "5"}, "ExitTimeOut": {"integer", "60"}, "Umask": {"integer", "63"},
		"StandardOutPath": {"string", spec.stdout}, "StandardErrorPath": {"string", spec.stderr},
	}
	var errs []error
	for key := range root.dict {
		switch {
		case strs[key].kind != "", key == "ProgramArguments", key == "EnvironmentVariables", key == "RunAtLoad", key == "KeepAlive":
		default:
			errs = append(errs, fmt.Errorf("launchd %s: unexpected key %s", name, key))
		}
	}
	for key, w := range strs {
		got, ok := root.dict[key]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("launchd %s: no %s", name, key))
		case got.kind != w.kind:
			errs = append(errs, fmt.Errorf("launchd %s: %s is %s, want %s", name, key, got.kind, w.kind))
		case got.text != w.text:
			errs = append(errs, fmt.Errorf("launchd %s: %s = %q, want %q", name, key, got.text, w.text))
		}
	}
	for _, key := range []string{"RunAtLoad", "KeepAlive"} {
		got, ok := root.dict[key]
		if !ok || got.kind != "bool" || !got.bool {
			errs = append(errs, fmt.Errorf("launchd %s: %s must be boolean true, got %+v", name, key, got))
		}
	}
	env, ok := root.dict["EnvironmentVariables"]
	switch {
	case !ok:
		errs = append(errs, fmt.Errorf("launchd %s: no EnvironmentVariables", name))
	case env.kind != "dict":
		errs = append(errs, fmt.Errorf("launchd %s: EnvironmentVariables is %s, want dict", name, env.kind))
	case len(env.dict) != 2 || env.dict["HOME"].kind != "string" || env.dict["HOME"].text != spec.home || env.dict["PATH"].kind != "string" || env.dict["PATH"].text != samplePath:
		errs = append(errs, fmt.Errorf("launchd %s: EnvironmentVariables = %+v", name, env.dict))
	}
	argv, err := plistArgv(name, root, v)
	if err != nil {
		errs = append(errs, err)
	} else if !slices.Equal(argv, spec.argv) {
		errs = append(errs, fmt.Errorf("launchd %s: argv %q, want %q", name, argv, spec.argv))
	}
	return errors.Join(errs...)
}

// checkLaunchdDoc is FP-5's chapter validator.
func checkLaunchdDoc(md string) error {
	body, err := sectionOf(md, "Run as services")
	if err != nil {
		return err
	}
	text := stripInlineLinks(body)
	var errs []error
	errs = append(errs, checkAccountsDoc(md))
	errs = append(errs, needAll("launchd", text,
		"Claude and Codex may be enabled with `--claude-adapter` or `--codex-adapter`; the sidecar warns once at startup that their recipes are Linux-qualified and that macOS vendor sandbox and exit behavior are unverified.",
		"`--grok-adapter` and `--cursor-adapter` are accepted syntactically, but their roles are refused on macOS: they are not working macOS worker choices.",
		"The sample stays on Claude; the usable alternative is Codex, with the same unverified qualification.",
		"two system LaunchDaemons under `/Library/LaunchDaemons/`", "`org.callsheet.plane` and `org.callsheet.sidecar`", "Do not substitute LaunchAgents", "`UserName` is never root",
		"**Unverified:** that your macOS version supports this `launchctl` syntax", "`launchctl help bootstrap`, `launchctl help bootout`, `launchctl help print`, `man launchd.plist` and `plutil -help`",
		"In particular, macOS service access to user authentication is not promised.", "plutil is part of the macOS base system", "Lint is syntax only and does not settle runtime behavior.",
		"`KeepAlive` restarts any exit while the job is loaded, so intentional maintenance must use `bootout`, not a signal.", "a manager cleanup limit, not a supported task-duration bound",
		"Restart means `bootout`, verify exit, then `bootstrap`.", "Do not use `kickstart` as an address-change stop.", "state and logs stay",
		"In a plist, escape `&` and `<`", "do not use `$HOME`, a tilde or any shell expansion inside XML arguments"))
	errs = append(errs, checkServiceFences(md, "macos-logs-plane", "macos-logs-worker", "macos-check", "macos-install", "macos-start-plane", "macos-start-sidecar", "macos-stop", "macos-remove", "macos-read-logs"))
	errs = append(errs, inOrder("launchd", body, "deploy: macos-logs-plane", "deploy: macos-check", "deploy: macos-install", "deploy: macos-start-plane", "deploy: macos-start-sidecar", "deploy: macos-stop"))
	for _, l := range deployServiceSources[4:] {
		if !slices.Contains(markdownLinks(body), l) {
			errs = append(errs, fmt.Errorf("launchd: source %s is not cited", l))
		}
	}
	macos := body[strings.Index(body, "### macOS with launchd"):]
	errs = append(errs, forbidAll("launchd", macos, "grok worker runs", "cursor worker runs", "their roles run", "login keychain is available", "launchagents/"))
	return errors.Join(errs...)
}

// runPlutil is the bounded native lint: it distinguishes a lint verdict (the
// process ran and exited) from a failure to run it at all, which never counts as a
// verdict in either direction.
func runPlutil(tool, path string) (code int, out string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), qsWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, "-lint", path)
	cmd.WaitDelay = 5 * time.Second
	b, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return 0, string(b), fmt.Errorf("plutil timed out: %w", ctx.Err())
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
		return 0, string(b), nil
	case errors.As(runErr, &ee):
		return ee.ExitCode(), string(b), nil
	}
	return 0, string(b), runErr
}

// TestDeployLaunchdSamples (FP-5): both XML samples on every OS, and, inside this
// parent, darwin's real plutil -lint on both rendered candidates. Linux runs only
// the structural checks and is no evidence of native lint. No service is loaded.
func TestDeployLaunchdSamples(t *testing.T) {
	md := string(repoFile(t, deployDoc))
	check := func(name, text string) error { return checkLaunchdSample(name, text, sampleTemplate) }
	v := sampleVars{planeHome: "/srv/plane & co", workerHome: "/srv/worker-account", callsheet: "/opt/tools/callsheet", workerPath: "/opt/tools/claude"}
	rendered := map[string]string{}
	for _, name := range []string{planePlist, sidePlist} {
		text := string(repoFile(t, sampleDir+name))
		if err := check(name, text); err != nil {
			t.Fatalf("shipped sample: %v", err)
		}
		r, err := renderSample(text, v)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkLaunchdSample(name, r, v); err != nil {
			t.Fatalf("rendered sample: %v", err)
		}
		rendered[name] = r
		if !strings.Contains(text, "REPLACE_WITH") || !strings.Contains(text, "docs/deploy.md") || strings.Index(text, "<!--") > strings.Index(text, "<plist") {
			t.Errorf("%s: no leading replacement comment", name)
		}
	}
	if err := checkLaunchdDoc(md); err != nil {
		t.Fatalf("shipped chapter: %v", err)
	}
	// What the chapter says about macOS workers is the product's rule.
	if !strings.Contains(sidecar.DarwinVendorWarning, "Linux-qualified") || !strings.Contains(sidecar.DarwinVendorWarning, "UNVERIFIED") {
		t.Errorf("warning = %q", sidecar.DarwinVendorWarning)
	}
	help, err := leafHelp(t, "sidecar run")
	if err != nil {
		t.Fatal(err)
	}
	if h := normSpace(help.stdout); !strings.Contains(h, "On macOS grok roles are refused") || !strings.Contains(h, "On every other OS its roles are refused") || !strings.Contains(h, "A grok or cursor flag is accepted even where its execution is refused") {
		t.Errorf("sidecar run help no longer states the macOS rule:\n%s", help.stdout)
	}
	t.Run("sample-mutations", func(t *testing.T) {
		runSampleMutations(t, check, []sampleMutation{
			{"wrong-plist-type", planePlist, "<key>RunAtLoad</key>\n\t<true/>", "<key>RunAtLoad</key>\n\t<string>true</string>", "RunAtLoad must be boolean true"},
			{"wrong-umask-type", planePlist, "<integer>63</integer>", "<string>63</string>", "Umask is string, want integer"},
			{"octal-umask", planePlist, "<integer>63</integer>", "<integer>77</integer>", "Umask = \"77\""},
			{"env-as-string", planePlist, "<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>HOME</key>\n\t\t<string>/REPLACE_WITH_PLANE_HOME</string>\n\t\t<key>PATH</key>\n\t\t<string>/usr/local/bin:/usr/bin:/bin</string>\n\t</dict>", "<key>EnvironmentVariables</key>\n\t<string>HOME=x</string>", "EnvironmentVariables is string"},
			{"duplicate-key", planePlist, "<key>RunAtLoad</key>", "<key>KeepAlive</key>\n\t<true/>\n\t<key>RunAtLoad</key>", "duplicate key KeepAlive"},
			{"stringified-argv", planePlist, "<string>/usr/local/bin/callsheet</string>\n\t\t<string>plane</string>\n\t\t<string>run</string>", "<string>/usr/local/bin/callsheet plane run</string>", "ProgramArguments runs"},
			{"argv-as-string", planePlist, "<key>ProgramArguments</key>\n\t<array>", "<key>ProgramArguments</key>\n\t<string>x</string>\n\t<key>Extra</key>\n\t<array>", "ProgramArguments is string, want array"},
			{"missing-username", sidePlist, "<key>UserName</key>\n\t<string>callsheet-worker</string>\n", "", "no UserName"},
			{"root-username", sidePlist, "<string>callsheet-worker</string>", "<string>root</string>", "UserName ="},
			{"wrong-label", planePlist, "<string>org.callsheet.plane</string>", "<string>org.callsheet.other</string>", "Label ="},
			{"extra-execution-key", planePlist, "<key>RunAtLoad</key>", "<key>Sockets</key>\n\t<dict/>\n\t<key>RunAtLoad</key>", "unexpected key Sockets"},
			{"shell-wrapper", planePlist, "<string>/usr/local/bin/callsheet</string>", "<string>/bin/sh</string>\n\t\t<string>-c</string>\n\t\t<string>/usr/local/bin/callsheet</string>", "ProgramArguments runs"},
			{"missing-adapter", sidePlist, "<string>--claude-adapter</string>\n\t\t<string>/REPLACE_WITH_ABSOLUTE_WORKER_PATH</string>\n", "", "argv"},
			{"grok-worker", sidePlist, "<string>--claude-adapter</string>", "<string>--grok-adapter</string>", "argv"},
			{"cursor-worker", sidePlist, "<string>--claude-adapter</string>", "<string>--cursor-adapter</string>", "argv"},
			{"init-argv", planePlist, "<string>run</string>", "<string>init</string>", "argv"},
			{"keepalive-false", planePlist, "<key>KeepAlive</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<false/>", "KeepAlive must be boolean true"},
			{"logs-elsewhere", planePlist, "/REPLACE_WITH_PLANE_HOME/callsheet/logs/plane.err.log", "/tmp/plane.err.log", "StandardErrorPath ="},
			{"home-mismatch", sidePlist, "<key>HOME</key>\n\t\t<string>/REPLACE_WITH_WORKER_HOME</string>", "<key>HOME</key>\n\t\t<string>/REPLACE_WITH_PLANE_HOME</string>", "EnvironmentVariables ="},
			{"malformed-xml", planePlist, "</dict>\n</plist>", "</plist>", "malformed XML"},
			{"entity-directive", planePlist, "<!DOCTYPE plist PUBLIC", "<!ENTITY x \"y\">\n<!DOCTYPE plist PUBLIC", "unexpected directive"},
			{"second-value", planePlist, "</dict>\n</plist>", "</dict>\n<dict/>\n</plist>", "more than one value"},
		})
	})
	t.Run("document-mutations", func(t *testing.T) {
		const sec = "Run as services"
		runFPMutations(t, md, checkLaunchdDoc, []readmeMutation{
			{"fake-login-guarantee", inSection(sec, "In particular, macOS service access to user authentication is not promised.", "A LaunchDaemon always has the login keychain."), "launchd: lacks"},
			{"missing-bootout", func(t *testing.T, md string) string {
				t.Helper()
				b, _ := markedFenceBlock(t, md, "<!-- deploy: macos-stop -->")
				return strings.Replace(md, b, "", 1)
			}, "fence macos-stop is missing"},
			{"remove-warning", inSection(sec, "the sidecar warns once at startup that their recipes are Linux-qualified and that macOS vendor sandbox and exit behavior are unverified", "they run normally"), "launchd: lacks \"Claude and Codex"},
			{"remove-refusal", inSection(sec, "but their roles are refused on macOS: they are not working macOS worker choices", "and they work"), "launchd: lacks"},
			{"promise-grok", appendToSection(sec, "A Grok worker runs on macOS: their roles run after enrollment."), "launchd: forbidden \"grok worker runs\""},
			{"sample-to-grok", inSection(sec, "The sample stays on Claude", "The sample uses Grok"), "launchd: lacks"},
			{"silent-agents", inSection(sec, "Do not substitute LaunchAgents", "You may use LaunchAgents"), "launchd: lacks"},
			{"no-plutil-lint", inSection(sec, "plutil -lint org.callsheet.plane.plist\n", ""), "fence macos-check ="},
			{"restart-by-signal", inSection(sec, "so intentional maintenance must use `bootout`, not a signal", "so send a signal"), "launchd: lacks"},
			{"kickstart-stop", inSection(sec, "Do not use `kickstart` as an address-change stop.", "Use `kickstart` to stop."), "launchd: lacks"},
			{"unverified-removed", inSection(sec, "**Unverified:** that your macOS version supports", "Your macOS version supports"), "launchd: lacks"},
		})
	})
	// Native lint. On darwin a missing tool, start error, timeout or a non-zero
	// candidate exit fails; there is no skip, stub or fallback. Elsewhere this
	// branch does not execute and establishes nothing about plutil.
	if runtime.GOOS == "darwin" {
		tool, err := exec.LookPath("plutil")
		if err != nil {
			t.Fatalf("plutil: %v", err)
		}
		dir := t.TempDir()
		for name, text := range rendered {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			code, out, err := runPlutil(tool, p)
			t.Logf("plutil -lint %s: exit %d: %s", name, code, out)
			if err != nil {
				t.Fatalf("plutil -lint %s: %v", name, err)
			}
			if code != 0 {
				t.Fatalf("plutil -lint %s exited %d: %s", name, code, out)
			}
		}
		bad := filepath.Join(dir, "malformed.plist")
		if err := os.WriteFile(bad, []byte(strings.Replace(rendered[planePlist], "</dict>\n</plist>", "</plist>", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, err := runPlutil(tool, bad)
		t.Logf("plutil -lint malformed: exit %d: %s", code, out)
		if err != nil {
			t.Fatalf("plutil -lint malformed: %v", err)
		}
		if code == 0 {
			t.Fatalf("plutil accepted malformed XML: %s", out)
		}
	}
}

// ---- FP-6: navigation.

// checkDeployNavigation is FP-6's validator over the four documents.
func checkDeployNavigation(readme, coordinator, quickstart, deploy string, look repoLookup) error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	docs, err := sectionOf(readme, "Documentation")
	if err != nil {
		return err
	}
	summary, err := sectionOf(readme, "Quickstart summary")
	if err != nil {
		return err
	}
	security, err := sectionOf(readme, "Security")
	if err != nil {
		return err
	}
	if !strings.Contains(docs, "[Deployment](docs/deploy.md)") {
		add(errors.New("README: the documentation map does not link the deployment chapter"))
	}
	for _, line := range strings.Split(docs, "\n") {
		if strings.Contains(line, "docs/deploy.md") && strings.Contains(line, "coming in v1") {
			add(errors.New("README: deployment is still a coming-in-v1 entry"))
		}
	}
	if !strings.Contains(docs, "`docs/guide/index.md`: the user guide; coming in v1.") {
		add(errors.New("README: the user guide lost its coming-in-v1 entry"))
	}
	if !slices.Contains(markdownLinks(summary), "docs/deploy.md") {
		add(errors.New("README: the summary does not link the deployment chapter"))
	}
	if strings.Contains(readme, "Until the deployment guide exists") {
		add(errors.New("README: still says the deployment guide does not exist"))
	}
	if !slices.Contains(markdownLinks(security), "docs/deploy.md#security-posture") {
		add(errors.New("README: the CA link does not reach the security posture"))
	}
	add(checkEntryPoint(readme))
	add(checkSupportAndSecurity(readme))
	add(checkDocMap(readme, look))

	body, err := sectionOf(quickstart, "Next steps")
	if err != nil {
		return err
	}
	if !slices.Contains(markdownLinks(body), "deploy.md") {
		add(errors.New("quickstart: Next steps does not link the deployment chapter"))
	}
	prereq, err := sectionOf(quickstart, "Prerequisites")
	if err != nil {
		return err
	}
	if !slices.Contains(markdownLinks(prereq), "deploy.md#security-posture") {
		add(errors.New("quickstart: prerequisites do not link the security posture"))
	}
	second, err := sectionOf(quickstart, "Add a second machine")
	if err != nil {
		return err
	}
	if !slices.Contains(markdownLinks(second), "deploy.md#change-the-plane-address") {
		add(errors.New("quickstart: the second machine does not link the maintenance procedure"))
	}
	add(checkSourceLinks(quickstartDoc, quickstart, look))

	shares, err := sectionOf(coordinator, "What every configuration shares")
	if err != nil {
		return err
	}
	var added []string
	for _, l := range markdownLinks(shares) {
		if strings.HasPrefix(l, "deploy.md") {
			added = append(added, l)
		}
	}
	for _, l := range []string{"deploy.md#give-clients-the-ca", "deploy.md#change-the-plane-address", "deploy.md#recover-after-ca-loss"} {
		if !slices.Contains(added, l) {
			add(fmt.Errorf("coordinator: missing link %s", l))
		}
	}
	for _, l := range added {
		add(checkSourceLinks("docs/coordinator.md", "[link]("+l+")", look))
	}

	add(checkSourceLinks(deployDoc, deploy, look))
	links := markdownLinks(deploy)
	for _, l := range deployServiceSources {
		if !slices.Contains(links, l) {
			add(fmt.Errorf("deploy: source %s is not cited", l))
		}
	}
	for _, l := range links {
		if strings.Contains(l, "guide/index.md") {
			add(errors.New("deploy: links the future user guide"))
		}
	}
	add(checkPrivateText("deploy", deploy))
	add(forbidAll("deploy", deploy, "binaries are available", "releases/latest", "download the latest", "is already published", "v1 is released", "is certified", "fully qualified"))
	if readmeQualified.MatchString(strings.ReplaceAll(deploy, "Linux-qualified", "")) {
		add(fmt.Errorf("deploy: affirmative qualification claim %q", readmeQualified.FindString(strings.ReplaceAll(deploy, "Linux-qualified", ""))))
	}
	return errors.Join(errs...)
}

// TestDeployNavigation (FP-6): the deployment entry and security destination in
// README, the links from the quickstart and coordinator guide, and the retained
// future user-guide entry.
func TestDeployNavigation(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	readme, coordinator, quickstart, deploy := string(repoFile(t, "README.md")), string(repoFile(t, "docs/coordinator.md")), string(repoFile(t, quickstartDoc)), string(repoFile(t, deployDoc))
	look := checkoutLookup(root)
	check := func(readme, coordinator, quickstart, deploy string) error {
		return checkDeployNavigation(readme, coordinator, quickstart, deploy, func(rel string) ([]byte, bool) {
			switch rel {
			case deployDoc:
				return []byte(deploy), true
			case quickstartDoc:
				return []byte(quickstart), true
			}
			return look(rel)
		})
	}
	if err := check(readme, coordinator, quickstart, deploy); err != nil {
		t.Fatalf("shipped documents: %v", err)
	}
	for _, name := range []string{planeUnit, workerUnit, planePlist, sidePlist} {
		text := string(repoFile(t, sampleDir+name))
		if err := checkPrivateText(name, text); err != nil {
			t.Error(err)
		}
		if err := forbidAll(name, text, "token=", "api_key", "apikey", "password", "secret", "bearer "); err != nil {
			t.Error(err)
		}
	}
	for _, c := range []struct {
		name  string
		apply func() error
		want  string
	}{
		{"restored-coming-in-v1", func() error {
			return check(replaceOnce("- [Deployment](docs/deploy.md): deployment, trust and service management.", "- `docs/deploy.md`: deployment, trust and service management; coming in v1.")(t, readme), coordinator, quickstart, deploy)
		}, "the documentation map does not link the deployment chapter"},
		{"coming-in-v1-with-link", func() error {
			return check(replaceOnce("- [Deployment](docs/deploy.md): deployment, trust and service management.", "- [Deployment](docs/deploy.md): coming in v1.")(t, readme), coordinator, quickstart, deploy)
		}, "deployment is still a coming-in-v1 entry"},
		{"old-security-destination", func() error {
			return check(replaceOnce("[CA](docs/deploy.md#security-posture)", "[CA](docs/coordinator.md#what-every-configuration-shares)")(t, readme), coordinator, quickstart, deploy)
		}, "the CA link does not reach the security posture"},
		{"old-summary-prose", func() error {
			return check(replaceOnce("The [deployment guide](docs/deploy.md) covers services and certificates; each subcommand's", "Until the deployment guide exists, each subcommand's")(t, readme), coordinator, quickstart, deploy)
		}, "the summary does not link the deployment chapter"},
		{"dead-security-anchor", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("## Security posture\n", "## Security\n")(t, deploy))
		}, "fragment #security-posture"},
		{"dead-coordinator-anchor", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("## Give clients the CA\n", "## Clients\n")(t, deploy))
		}, "fragment #give-clients-the-ca"},
		{"dead-local-anchor", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("### Maintenance\n", "### Upkeep\n")(t, deploy))
		}, "fragment #maintenance"},
		{"dead-sample-link", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("(examples/deploy/org.callsheet.plane.plist)", "(examples/deploy/missing.plist)")(t, deploy))
		}, "does not exist"},
		{"private-path", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("## Security posture\n", "## Security posture\n\nSee /home/someone/state.\n")(t, deploy))
		}, "deploy: private path sentinel"},
		{"private-macos-path", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("## Security posture\n", "## Security posture\n\nSee /Users/someone/state.\n")(t, deploy))
		}, "deploy: private path sentinel"},
		{"linked-future-guide", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("## Security posture\n", "## Security posture\n\n[guide](guide/index.md)\n")(t, deploy))
		}, "docs/guide/index.md does not exist"},
		{"readme-linked-future-guide", func() error {
			return check(replaceOnce("`docs/guide/index.md`", "[guide](docs/guide/index.md)")(t, readme), coordinator, quickstart, deploy)
		}, "lost its coming-in-v1 entry"},
		{"guide-entry-removed", func() error {
			return check(replaceOnce("- `docs/guide/index.md`: the user guide; coming in v1.\n", "")(t, readme), coordinator, quickstart, deploy)
		}, "lost its coming-in-v1 entry"},
		{"quickstart-next-step-removed", func() error {
			return check(readme, coordinator, replaceOnce("- [Deployment](deploy.md): sustained operation, services, address changes and recovery.\n", "")(t, quickstart), deploy)
		}, "Next steps does not link"},
		{"quickstart-prerequisite-removed", func() error {
			return check(readme, coordinator, replaceOnce("(deploy.md#security-posture)", "(deploy.md)")(t, quickstart), deploy)
		}, "prerequisites do not link"},
		{"quickstart-maintenance-removed", func() error {
			return check(readme, coordinator, replaceOnce("(deploy.md#change-the-plane-address)", "(deploy.md)")(t, quickstart), deploy)
		}, "does not link the maintenance procedure"},
		{"coordinator-link-removed", func() error {
			return check(readme, replaceOnce("(deploy.md#give-clients-the-ca)", "(deploy.md)")(t, coordinator), quickstart, deploy)
		}, "missing link deploy.md#give-clients-the-ca"},
		{"coordinator-recovery-removed", func() error {
			return check(readme, replaceOnce("(deploy.md#recover-after-ca-loss)", "(deploy.md)")(t, coordinator), quickstart, deploy)
		}, "missing link deploy.md#recover-after-ca-loss"},
		{"coordinator-bad-anchor", func() error {
			return check(readme, replaceOnce("(deploy.md#change-the-plane-address)", "(deploy.md#change-address)")(t, coordinator), quickstart, deploy)
		}, "missing link deploy.md#change-the-plane-address"},
		{"release-claim", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("## Security posture\n", "## Security posture\n\nPrebuilt binaries are available.\n")(t, deploy))
		}, "deploy: forbidden \"binaries are available\""},
		{"qualification-claim", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("## Security posture\n", "## Security posture\n\nEvery worker is qualified on macOS.\n")(t, deploy))
		}, "affirmative qualification claim"},
		{"uncited-source", func() error {
			return check(readme, coordinator, quickstart, replaceOnce("(https://it-training.apple.com/compliance/tutorials/course/sec050/)", "(https://example.invalid/)")(t, deploy))
		}, "is not cited"},
		{"word-cap", func() error {
			return check(padWords(t, readme, readmeWordCap+1), coordinator, quickstart, deploy)
		}, "budget: 751 words"},
		{"maintainer-link-in-user-map", func() error {
			return check(replaceOnce("- [Deployment](docs/deploy.md): deployment, trust and service management.", "- [Deployment](docs/deploy.md): deployment, trust and service management.\n- [CI](docs/maintainers/ci.md): checks.")(t, readme), coordinator, quickstart, deploy)
		}, "maintainer link docs/maintainers/ci.md outside"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.apply()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			t.Logf("rejected: %v", firstLine(err))
		})
	}
}

// ---- FP-7: the executable documentation contract.

func deployContract() docFenceContract {
	inv := make([]docFenceSpec, 0, len(deployFences))
	for _, r := range deployFences {
		inv = append(inv, r.docFenceSpec)
	}
	return docFenceContract{marker: deployMarker, inventory: inv, parse: parseDeployFence, variables: func(id string) []string {
		row, _ := deployRow(id)
		return row.vars
	}}
}

// checkSampleArgv validates a sample's argv through the same real-tree checker as
// the chapter's fences, then requires the exact run leaf and its operational
// arguments, which the help vocabulary alone cannot detect the omission of.
func checkSampleArgv(t *testing.T, name string, argv []string) error {
	t.Helper()
	words := make([]qsWord, 0, len(argv)+1)
	for _, a := range append([]string{"callsheet"}, argv...) {
		words = append(words, qsWord{parts: []qsPart{{text: a}}})
	}
	leaf, errs := checkCallsheetCommand(t, cli.NewTree(runtime.GOOS), name, qsCommand{words: words, line: 1}, nil)
	spec, err := sampleSpecOf(name, sampleTemplate)
	if err != nil {
		return err
	}
	wantLeaf := strings.Join(spec.argv[:2], " ")
	if leaf != wantLeaf {
		errs = append(errs, fmt.Errorf("sample %s runs %q, want %q", name, leaf, wantLeaf))
	}
	if v, ok := flagValue(argv, "--state-dir"); !ok || v != spec.state {
		errs = append(errs, fmt.Errorf("sample %s: --state-dir = %q, want %q", name, v, spec.state))
	}
	if strings.HasPrefix(wantLeaf, "sidecar") {
		if v, ok := flagValue(argv, "--claude-adapter"); !ok || !strings.HasPrefix(v, "/") {
			errs = append(errs, fmt.Errorf("sample %s: no enabled worker flag with an absolute path", name))
		}
	}
	for _, a := range slices.Concat(argv, []string{spec.home, spec.state, spec.stdout, spec.stderr}) {
		if !strings.HasPrefix(a, "/") && !strings.HasPrefix(a, "--") && !slices.Contains(spec.argv[:2], a) {
			errs = append(errs, fmt.Errorf("sample %s: %q is not an absolute path", name, a))
		}
	}
	return errors.Join(errs...)
}

// TestDeployCommandContract (FP-7): every chapter shell fence and all four sample
// argv against cli.NewTree and cached real help, with the drift mutations rejected.
func TestDeployCommandContract(t *testing.T) {
	md := string(repoFile(t, deployDoc))
	check := func(md string) error {
		return errors.Join(checkFenceContract(t, md, deployContract()), checkOperationBodies(md))
	}
	t.Run("shipped", func(t *testing.T) {
		if err := check(md); err != nil {
			t.Fatal(err)
		}
		// Both inventories are real: some fences run Callsheet, some do not.
		calls, ops := 0, 0
		for _, r := range deployFences {
			if len(r.leaves) > 0 {
				calls++
			} else {
				ops++
			}
		}
		t.Logf("%d Callsheet fences, %d operation fences", calls, ops)
		for id := range deployServiceFences {
			if r, ok := deployRow(id); !ok || len(r.leaves) != 0 {
				t.Errorf("service fence %s is not an operation row", id)
			}
		}
	})
	t.Run("samples", func(t *testing.T) {
		for _, name := range []string{planeUnit, workerUnit, planePlist, sidePlist} {
			if err := checkSampleArgv(t, name, sampleArgv(t, name, sampleTemplate)); err != nil {
				t.Error(err)
			}
		}
		base := sampleArgv(t, workerUnit, sampleTemplate)
		for _, c := range []struct {
			name, sample string
			argv         []string
			want         string
		}{
			{"unknown-leaf", planeUnit, []string{"plane", "serve", "--state-dir", "/x"}, "unknown command \"serve\""},
			{"wrong-leaf", planeUnit, []string{"plane", "status", "--state-dir", sampleTemplate.planeHome + "/callsheet/plane"}, "runs \"plane status\""},
			{"unknown-flag", workerUnit, append(slices.Clone(base), "--no-such-flag"), "unknown flag --no-such-flag of sidecar run"},
			{"omitted-state", planeUnit, []string{"plane", "run"}, "--state-dir = \"\""},
			{"omitted-adapter", workerUnit, base[:4], "no enabled worker flag"},
			{"relative-adapter", workerUnit, slices.Concat(base[:5], []string{"claude"}), "no enabled worker flag"},
			{"relative-state", planeUnit, []string{"plane", "run", "--state-dir", "plane"}, "--state-dir = \"plane\""},
			{"init-instead", planeUnit, []string{"plane", "init", "--state-dir", sampleTemplate.planeHome + "/callsheet/plane"}, "runs \"plane init\""},
		} {
			t.Run(c.name, func(t *testing.T) {
				err := checkSampleArgv(t, c.sample, c.argv)
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want one containing %q", err, c.want)
				}
				t.Logf("rejected: %v", firstLine(err))
			})
		}
	})
	t.Run("operations", func(t *testing.T) {
		for _, body := range []string{`rm -rf "$STATE"`, `curl https://x`, `eval "$X"`, `bash -c "callsheet version"`, `sudo bash -c x`, `sudo callsheet plane run`, `chmod 777 "$STATE"`, `mkdir "$STATE"`, `read X`, `test -d x`, `mv a b`, `cp -r a b`, `printf 'x'`, `$OTHER --version`, `"$WORKER"`} {
			if _, err := parseDeployFence(qsFence{id: "f", lang: "sh", body: body, line: 1}); err == nil || !strings.Contains(err.Error(), "fence f line") {
				t.Errorf("%q: err = %v", body, err)
			}
		}
		for _, body := range []string{`mkdir -m 700 "$HOME/x"`, `STATE="$HOME/x"`, `read -r PLANE_IP`, `"$WORKER" --version`, `chmod 600 "$CA"`, `sudo systemctl status x`, `sudo -iu callsheet-worker`, `df -P "$STATE" "$HOME"`} {
			if _, err := parseDeployFence(qsFence{id: "f", lang: "sh", body: body, line: 1}); err != nil {
				t.Errorf("%q: %v", body, err)
			}
		}
		// A guard chain is the move fences' alone, and hides nothing.
		move := func(body string) error {
			_, err := parseDeployFence(qsFence{id: "recover-plane-move", lang: "sh", body: body, line: 1})
			return err
		}
		if err := move(deployMoveBody("STATE", "OLD_STATE")); err != nil {
			t.Errorf("the shipped chain: %v", err)
		}
		for _, body := range []string{"test -d x && mv a b", "test -d x &&\n  rm -rf y", "printf 'x' &&\n test -d y", "test -d x || true"} {
			if err := move(body); err == nil {
				t.Errorf("%q accepted", body)
			}
		}
		if _, err := parseDeployFence(qsFence{id: "state-status", lang: "sh", body: "test -d x &&\n  mv a b", line: 1}); err == nil {
			t.Error("a guard chain accepted outside a move fence")
		}
		if _, err := parseDeployFence(qsFence{id: "plane-dirs", lang: "sh", body: `chmod 600 "$CA"`, line: 1}); err == nil || !strings.Contains(err.Error(), "unknown variable $CA") {
			t.Errorf("a variable outside its fence's allowlist: %v", err)
		}
		if _, err := parseDeployFence(qsFence{id: "plane-dirs", lang: "sh", body: `CA=x`, line: 1}); err == nil || !strings.Contains(err.Error(), "unknown variable $CA") {
			t.Errorf("an assignment outside its fence's allowlist: %v", err)
		}
		// Every Callsheet row holds Callsheet commands and nothing else, and
		// every operation row has a pinned body.
		for _, r := range deployFences {
			if len(r.leaves) > 0 {
				_, err := parseDeployFence(qsFence{id: r.id, lang: "sh", body: `sudo rm /tmp/reviewer-example`, line: 1})
				if err == nil || !strings.Contains(err.Error(), "a Callsheet-only fence holds a setup operation") {
					t.Errorf("%s: %v", r.id, err)
				}
			} else if _, ok := deployServiceFences[r.id]; !ok {
				t.Errorf("operation fence %s has no pinned body", r.id)
			}
		}
		// The compatibility wrapper keeps reading only its own markers.
		if _, err := parseQuickstartFences("<!-- deploy: a -->\n```sh\nx\n```\n"); err == nil || !strings.Contains(err.Error(), "unmarked sh fence at line 2") {
			t.Errorf("a deploy marker in the quickstart parser: %v", err)
		}
		if _, err := parseDeployFences("<!-- quickstart: a -->\n```sh\nx\n```\n"); err == nil || !strings.Contains(err.Error(), "unmarked sh fence at line 2") {
			t.Errorf("a quickstart marker in the deploy parser: %v", err)
		}
	})
	t.Run("mutations", func(t *testing.T) {
		const status = `callsheet plane status --state-dir "$STATE"`
		runFPMutations(t, md, check, []readmeMutation{
			{"unknown-leaf", replaceOnce(status, `callsheet plane state --state-dir "$STATE"`), "unknown command \"state\""},
			{"unknown-flag", replaceOnce(`callsheet task ls --plane "$PLANE"`, `callsheet task ls --all --plane "$PLANE"`), "unknown flag --all of task ls"},
			{"unknown-variable", replaceOnce(`callsheet task ls --plane "$PLANE"`, `callsheet task ls --plane "$OTHER"`), "unknown variable $OTHER"},
			{"foreign-variable", replaceOnce(status, `callsheet plane status --state-dir "$SIDECAR_STATE"`), "unknown variable $SIDECAR_STATE"},
			{"operation-unknown-variable", replaceOnce(`chmod 600 "$CA"`, `chmod 600 "$OTHER"`), "unknown variable $OTHER"},
			{"duplicate-id", replaceOnce("<!-- deploy: address-status -->", "<!-- deploy: state-status -->"), "duplicate fence ID state-status"},
			{"missing-id", func(t *testing.T, md string) string {
				t.Helper()
				b, _ := markedFenceBlock(t, md, "<!-- deploy: fresh-task-list -->")
				return strings.Replace(md, b, "", 1)
			}, "required fence fresh-task-list is missing"},
			{"marked-extra-command", appendToSection("Verify the deployment", "<!-- deploy: extra -->\n```sh\ncallsheet task ls\n```"), "fence extra is not in the pinned inventory"},
			{"unmarked-extra-command", appendToSection("Verify the deployment", "```sh\ncallsheet task ls\n```"), "unmarked sh fence"},
			{"hidden-in-plain-fence", appendToSection("Verify the deployment", "```\ncallsheet task ls\n```"), "unlabelled fence"},
			{"hidden-in-text-fence", appendToSection("Verify the deployment", "```text\ncallsheet plane run\n```"), "holds a Callsheet command"},
			{"extra-command-in-fence", replaceOnce(`callsheet task ls --plane "$PLANE" --ca "$CA"`, "callsheet task ls --plane \"$PLANE\" --ca \"$CA\"\ncallsheet node ls --plane \"$PLANE\" --ca \"$CA\""), "inventory: fence fresh-task-list runs"},
			{"swapped-leaf", replaceOnce(`callsheet plane init --state-dir "$STATE"`, `callsheet plane status --state-dir "$STATE"`), "inventory: fence plane-init runs"},
			{"wrong-language", replaceOnce("<!-- deploy: state-status -->\n```sh", "<!-- deploy: state-status -->\n```bash"), "fence state-status is bash, want sh"},
			{"hidden-shell", replaceOnce("sudo systemctl daemon-reload\n```\n\n<!-- deploy: linux-start-plane -->", "sudo bash -c 'callsheet plane run'\n```\n\n<!-- deploy: linux-start-plane -->"), "unrecognized setup operation: sudo bash"},
			{"eval", replaceOnce("read -r WORKER\n", "eval \"$WORKER\"\n"), "unrecognized setup operation: eval"},
			{"substitution", replaceOnce(`chmod 600 "$CA"`, `chmod 600 "$(cat x)"`), "command substitution"},
			{"pipe", replaceOnce(status, status+" | cat"), "pipe"},
			{"redirect", replaceOnce(status, status+" > out"), "redirect"},
			{"chain", replaceOnce(status, status+"; true"), "semicolon chain"},
			{"background", replaceOnce(status, status+" &"), "unsupported shell syntax &"},
			{"move-outside-guard", replaceOnce(`chmod 600 "$CA"`, `mv "$CA" "$CA"`), "mv outside a guarded move"},
			{"setup-op-in-callsheet-fence", replaceOnce(status, status+"\nsudo rm /tmp/reviewer-example"), "a Callsheet-only fence holds a setup operation"},
			{"setup-op-before-callsheet", replaceOnce(status, "sudo rm /tmp/reviewer-example\n"+status), "a Callsheet-only fence holds a setup operation"},
			{"setup-op-in-multi-leaf-fence", replaceOnce("callsheet node ls --plane \"$PLANE\" --ca \"$CA\"\ncallsheet role ls --plane \"$PLANE\" --ca \"$CA\"", "callsheet node ls --plane \"$PLANE\" --ca \"$CA\"\nsudo rm /tmp/reviewer-example\ncallsheet role ls --plane \"$PLANE\" --ca \"$CA\""), "a Callsheet-only fence holds a setup operation"},
			{"assignment-in-callsheet-fence", replaceOnce(status, "STATE=/tmp/other\n"+status), "a Callsheet-only fence holds a setup operation"},
			{"setup-op-in-env-fence", replaceOnce("read -r PLANE_IP\nPLANE=\"https://$PLANE_IP:8443\"\n```\n\n<!-- deploy: fresh-init -->", "read -r PLANE_IP\nPLANE=\"https://$PLANE_IP:8443\"\nsudo rm /tmp/reviewer-example\n```\n\n<!-- deploy: fresh-init -->"), "fence fresh-env ="},
			{"setup-op-in-service-fence", replaceOnce("sudo systemctl stop callsheet-plane.service\n", "sudo systemctl stop callsheet-plane.service\nsudo rm /tmp/reviewer-example\n"), "fence linux-stop ="},
			{"setup-op-in-macos-fence", replaceOnce("sudo launchctl bootout system/org.callsheet.plane\n", "sudo launchctl bootout system/org.callsheet.plane\nsudo rm /tmp/reviewer-example\n"), "fence macos-stop ="},
		})
	})
}
