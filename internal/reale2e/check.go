package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Check (design 12a-real-e2e, Evidence and checker) is the pure offline
// validation of a loaded bundle: it reads only the bundle's bytes and its
// validated evidence repository, never a shell, a vendor process or any
// command named in evidence. Every missing or negative criterion is a
// hard failure, including unobserved ones.

// Criterion IDs, in report order.
const (
	CritManifest   = "manifest"
	CritEvents     = "events"
	CritRoles      = "roles"
	CritPreflight  = "preflight"
	CritSetup      = "setup"
	CritEventMap   = "event_map"
	CritWaits      = "waits"
	CritHops       = "hops"
	CritChain      = "chain"
	CritArtifacts  = "artifacts"
	CritOwner      = "owner"
	CritAdmissions = "admissions"
	CritGrok       = "grok_review"
	CritFinalTests = "final_tests"
	CritCleanup    = "cleanup"
	CritDeadlines  = "deadlines"
	// A3.
	CritOwnerEnv = "owner_environment"
	CritResult   = "result_correctness"
)

// criteriaOrder is the report order.
var criteriaOrder = []string{CritManifest, CritEvents, CritRoles, CritPreflight, CritSetup, CritEventMap, CritWaits, CritHops,
	CritChain, CritArtifacts, CritOwner, CritAdmissions, CritGrok, CritFinalTests, CritResult, CritCleanup, CritOwnerEnv, CritDeadlines}

// CodeDerivedMismatch is a stored report or audit that differs from its
// recomputation.
const CodeDerivedMismatch = "derived_report_mismatch"

// Criterion is one predicate's result: pass with no codes, or fail with
// its fixed failure codes.
type Criterion struct {
	ID     string   `json:"id"`
	Result string   `json:"result"`
	Codes  []string `json:"codes"`
}

// checker accumulates failure codes per criterion.
type checker struct {
	b     *Bundle
	codes map[string][]string
	// Derived facts.
	views   [4]*contract.TaskView
	commits [4]*object.Commit
	files   [5]map[string][]byte // seed, hop 1..4
	seed    *object.Commit
	derived EventMap
	memo    *checkMemo
}

// checkMemo holds one check's derivations, shared by its checker and every
// scratch copy. Each is a pure function of the bundle and the facts
// newChecker loaded (criteria never write those), so it is computed once:
// every criterion method's codes, evaluated alone, and the audit rows.
type checkMemo struct {
	codes map[uintptr]map[string][]string
	rows  []AuditRow
	// waits is each hop's reparsed wait record against its stored one:
	// 0 unknown, 1 equal, -1 different.
	waits [4]int
	// streams is each phase stream's streamResult, from the derivation.
	streams [2][2]bool
}

// criterionMethods are the criterion predicates in evaluation order.
var criterionMethods = []func(*checker){(*checker).manifest, (*checker).events, (*checker).roles, (*checker).preflight, (*checker).setup,
	(*checker).eventMap, (*checker).waits, (*checker).hops, (*checker).chain, (*checker).artifacts, (*checker).owner, (*checker).admissions,
	(*checker).grok, (*checker).finalTests, (*checker).resultCorrectness, (*checker).cleanup, (*checker).ownerEnvironment, (*checker).deadlines}

func (c *checker) fail(crit, code string) {
	if !slices.Contains(c.codes[crit], code) {
		c.codes[crit] = append(c.codes[crit], code)
	}
}

// Check validates b and returns its criteria (PASS only when every
// criterion passes).
func Check(b *Bundle) []Criterion {
	out, _ := check(b)
	return out
}

// check is Check, also returning its checker, whose memoised derivations
// build b's audit.
func check(b *Bundle) ([]Criterion, *checker) {
	c := newChecker(b)
	c.run()
	out := c.criteria()
	// The stored derived outputs, when present, must equal their
	// recomputation from the source evidence (never trusted).
	if stored, ok := b.Files["report.json"]; ok {
		r := BuildReport(b, out)
		data, _ := encodeJSON(r)
		mismatch := !bytes.Equal(stored, data) || !bytes.Equal(b.Files["report.txt"], []byte(RenderText(r)))
		if aj, ok := b.Files["audit.json"]; ok || len(b.Files["audit.md"]) > 0 {
			au := buildAudit(c, r)
			want, _ := encodeJSON(au)
			mismatch = mismatch || !ok || !bytes.Equal(aj, want) || !bytes.Equal(b.Files["audit.md"], []byte(RenderAudit(au)))
		} else {
			mismatch = true
		}
		if mismatch {
			c.fail(CritResult, CodeDerivedMismatch)
			out = c.criteria()
		}
	}
	return out, c
}

// newChecker returns a checker of b with its derived facts loaded.
func newChecker(b *Bundle) *checker {
	c := &checker{b: b, codes: map[string][]string{}, memo: &checkMemo{codes: map[uintptr]map[string][]string{}}}
	c.loadHops()
	c.derived, c.memo.streams = deriveSession(sessionInputOf(b))
	return c
}

// run evaluates every criterion: the union of each method's codes
// evaluated alone (no method reads another's codes).
func (c *checker) run() {
	for _, f := range criterionMethods {
		for crit, codes := range c.methodCodes(f) {
			for _, code := range codes {
				c.fail(crit, code)
			}
		}
	}
}

// methodCodes evaluates criterion method f alone, once per check, and
// returns its codes by criterion (shared: never modify them).
func (c *checker) methodCodes(f func(*checker)) map[string][]string {
	key := reflect.ValueOf(f).Pointer()
	if c.memo != nil {
		if codes, ok := c.memo.codes[key]; ok {
			return codes
		}
	}
	s := c.scratch()
	f(s)
	if c.memo != nil {
		c.memo.codes[key] = s.codes
	}
	return s.codes
}

// criteria returns the criteria in report order.
func (c *checker) criteria() []Criterion {
	out := make([]Criterion, 0, len(criteriaOrder))
	for _, id := range criteriaOrder {
		cr := Criterion{ID: id, Result: "pass", Codes: []string{}}
		if codes := slices.Clone(c.codes[id]); len(codes) > 0 {
			sort.Strings(codes)
			cr.Result, cr.Codes = "fail", codes
		}
		out = append(out, cr)
	}
	return out
}

// sessionInputOf is the derivation input stored in a bundle.
func sessionInputOf(b *Bundle) SessionInput {
	in := SessionInput{Launches: b.Launches, Events: b.Events}
	if b.Manifest != nil {
		in.RunID = b.Manifest.RunID
	}
	for i := 0; i < 2; i++ {
		if d, ok := b.Files[phaseStreamFiles[i]]; ok {
			in.Streams[i] = d
		}
		if d, ok := b.Files[transcriptFiles[i]]; ok {
			in.Transcripts[i] = d
		}
	}
	return in
}

// passed reports whether every criterion passed.
func passed(cs []Criterion) bool {
	for _, c := range cs {
		if c.Result != "pass" {
			return false
		}
	}
	return len(cs) > 0
}

// flow is the bundle's flow (the setup copy), or nil.
func (c *checker) flow() *Flow { return c.b.Flow }

func (c *checker) manifest() {
	m := c.b.Manifest
	if m == nil {
		c.fail(CritManifest, "manifest_missing")
		return
	}
	switch {
	case !ValidRunID(m.RunID):
		c.fail(CritManifest, "run_id_invalid")
	case m.GOOS != "linux":
		c.fail(CritManifest, "platform_not_linux")
	}
	if !m.Gate.OptIn || !m.Gate.CIAbsent || m.Gate.Platform != "linux" {
		c.fail(CritManifest, "gate_not_passed")
	}
	if !hex40RE.MatchString(m.Revision) || strings.TrimSpace(m.CallsheetVersion) == "" {
		c.fail(CritManifest, "revision_missing")
	}
	want := []string{"claude", "codex", "grok", "go", "git"}
	var tools []string
	for _, v := range m.Versions {
		tools = append(tools, v.Tool)
		if !v.Eligible || strings.TrimSpace(v.Version) == "" {
			c.fail(CritManifest, "version_not_eligible")
		}
	}
	if !slices.Equal(tools, want) {
		c.fail(CritManifest, "versions_incomplete")
	}
	f := c.flow()
	switch {
	case f == nil:
		c.fail(CritManifest, "flow_missing")
	case m.FlowKind != f.Kind() || m.FlowSHA256 != sha256Hex(c.b.Files["setup/flow.json"]) || !slices.Equal(m.Roles, f.Roles):
		c.fail(CritManifest, "flow_mismatch")
	}
	w := m.Workspace
	if !contract.ValidWorkspaceName(w.Name) || !contract.ValidWorkspaceToken(w.Instance) || !hex40RE.MatchString(w.SeedCommit) || !hex40RE.MatchString(w.SeedTree) {
		c.fail(CritManifest, "workspace_invalid")
	}
	if len(m.Templates) == 0 || len(m.Rendered) == 0 {
		c.fail(CritManifest, "template_hashes_missing")
	}
	// The source-template inventory is exactly the shipped templates, in
	// their fixed order, each once with a valid hash.
	var names []string
	for _, t := range m.Templates {
		names = append(names, t.Name)
		if !hex64RE.MatchString(t.SHA256) {
			c.fail(CritManifest, "template_hashes_missing")
		}
	}
	if !slices.Equal(names, TemplateNames()) {
		c.fail(CritManifest, "template_inventory_mismatch")
	}
	// The rendered inventory is exactly the rendered setup files (the
	// prompt, the flow, the sanitized MCP configuration and every role
	// manual), sorted, each once, each present with its hash.
	names = nil
	for _, r := range m.Rendered {
		names = append(names, r.Name)
		data, ok := c.b.Files[r.Name]
		if !ok || sha256Hex(data) != r.SHA256 {
			c.fail(CritManifest, "rendered_hash_mismatch")
		}
	}
	if !slices.Equal(names, renderedInventory(c.b.Decision != nil && c.b.Decision.Decision == DecisionYes)) {
		c.fail(CritManifest, "rendered_inventory_mismatch")
	}
	// The launch inventory is exactly the launch records present, in phase
	// order, each with its hash.
	names = nil
	for _, l := range m.Launches {
		names = append(names, l.Name)
		if data, ok := c.b.Files[l.Name]; !ok || sha256Hex(data) != l.SHA256 {
			c.fail(CritManifest, "launch_hash_mismatch")
		}
	}
	var launched []string
	for _, f := range launchFiles {
		if _, ok := c.b.Files[f]; ok {
			launched = append(launched, f)
		}
	}
	if !slices.Equal(names, launched) {
		c.fail(CritManifest, "launch_inventory_mismatch")
	}
	if _, ok := parseTime(m.StartedAt); !ok {
		c.fail(CritManifest, "started_at_invalid")
	}
	// The roots are in canonical-path order, which their tokens need not
	// follow lexically; only uniqueness is checkable here.
	if m.ProtectedRoots == nil || len(slices.Compact(slices.Sorted(slices.Values(m.ProtectedRoots)))) != len(m.ProtectedRoots) {
		c.fail(CritManifest, "protected_roots_invalid")
	}
}

// renderedInventory is the exact sorted list of rendered setup files: the
// coordinator instructions, both phase prompts (phase 2's exists once the
// owner answered yes), the flow, the sanitized MCP configuration and every
// role manual.
func renderedInventory(withPhase2 bool) []string {
	out := append([]string{"setup/coordinator-prompt.md", "setup/" + PhasePrompt1, "setup/flow.json", "setup/mcp-config.json"}, manualFiles()...)
	if withPhase2 {
		out = append(out, "setup/"+PhasePrompt2)
	}
	sort.Strings(out)
	return out
}

// eventsOf returns the events of type typ.
func (c *checker) eventsOf(typ string) []Event {
	var out []Event
	for _, e := range c.b.Events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// firstEvent returns the first event of typ matching pred (nil pred
// matches all).
func (c *checker) firstEvent(typ string, pred func(Event) bool) (Event, bool) {
	for _, e := range c.b.Events {
		if e.Type == typ && (pred == nil || pred(e)) {
			return e, true
		}
	}
	return Event{}, false
}

func eventTime(e Event) time.Time {
	t, _ := parseTime(e.Time)
	return t
}

func (c *checker) events() {
	evs := c.b.Events
	if len(evs) == 0 {
		c.fail(CritEvents, "events_missing")
		return
	}
	var last time.Time
	for i, e := range evs {
		t, ok := parseTime(e.Time)
		switch {
		case e.Seq != i+1:
			c.fail(CritEvents, "sequence_not_increasing")
		case !ok:
			c.fail(CritEvents, "time_invalid")
		case t.Before(last):
			c.fail(CritEvents, "time_not_monotonic")
		}
		last = t
	}
	if evs[0].Type != EvRunStarted {
		c.fail(CritEvents, "run_started_not_first")
	}
	for _, typ := range []string{EvPreflightStarted, EvVersionsChecked, EvRuntimeCreated, EvRolesRegistered, EvSidecarsReconnected, EvRolesReady,
		EvSeedCreated, EvBaselineTests, EvCoordinatorReady, EvFinalTests, EvCollectionStarted, EvCleanupStarted, EvCleanupFinished, EvRuntimeRemoved,
		EvAwaitingOwner, EvOwnerDecision, EvReceiptCreated} {
		if len(c.eventsOf(typ)) != 1 {
			c.fail(CritEvents, "required_event_missing")
		}
	}
	for _, typ := range []string{EvSnapshotTaken, EvPhaseStarted, EvPhaseExited, EvTranscriptCaptured} {
		if len(c.eventsOf(typ)) != 2 {
			c.fail(CritEvents, "required_event_missing")
		}
	}
	if len(c.eventsOf(EvAttemptFailed)) != 0 {
		c.fail(CritEvents, "attempt_failed")
	}
	// A3 lifecycle order: the baseline fingerprint before any probe; phase
	// 1, its exit, the prompt, the decision, the receipt, phase 2 and its
	// exit; collection, cleanup, runtime removal and the final fingerprint.
	seq := func(typ, handle, code string) int {
		for _, e := range evs {
			if e.Type == typ && (handle == "" || e.Handle == handle) && (code == "" || e.Code == code) {
				return e.Seq
			}
		}
		return 0
	}
	order := []int{seq(EvPreflightStarted, "", ""), seq(EvSnapshotTaken, "", "before"), seq(EvVersionsChecked, "", ""), seq(EvCoordinatorReady, "", ""),
		seq(EvPhaseStarted, phaseHandle(1), ""), seq(EvPhaseExited, phaseHandle(1), ""), seq(EvAwaitingOwner, "", ""), seq(EvOwnerDecision, "", ""),
		seq(EvReceiptCreated, "", ""), seq(EvPhaseStarted, phaseHandle(2), ""), seq(EvPhaseExited, phaseHandle(2), ""), seq(EvCollectionStarted, "", ""),
		seq(EvCleanupFinished, "", ""), seq(EvSnapshotTaken, "", "after")}
	for i := 1; i < len(order); i++ {
		if order[i] == 0 || order[i] <= order[i-1] {
			c.fail(CritEvents, "lifecycle_out_of_order")
			break
		}
	}
}

func (c *checker) roles() {
	r, f := c.b.Roles, c.flow()
	if r == nil || f == nil {
		c.fail(CritRoles, "roles_missing")
		return
	}
	if len(r.Roles) != len(f.Roles) {
		c.fail(CritRoles, "roles_mismatch")
		return
	}
	nodes := map[string]string{}
	for i, s := range r.Roles {
		w := f.Roles[i]
		if s.ID != w.ID || s.Adapter != w.Adapter || s.Model != w.Model || s.Effort != w.Effort || s.Concurrency != RoleConcurrency ||
			s.Timeout != RoleTimeout.String() || !contract.ValidNodeID(s.Node) {
			c.fail(CritRoles, "roles_mismatch")
		}
		if !s.CanAccept {
			c.fail(CritRoles, "role_not_ready")
		}
		if n, ok := nodes[s.Adapter]; ok && n != s.Node {
			c.fail(CritRoles, "adapter_nodes_inconsistent")
		}
		nodes[s.Adapter] = s.Node
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n] {
			c.fail(CritRoles, "adapter_nodes_inconsistent")
		}
		seen[n] = true
	}
	if _, ok := parseTime(r.ReadyAt); !ok {
		c.fail(CritRoles, "ready_at_invalid")
	}
}

// selectionMatches reports whether a view ran its flow role's pair with no
// override.
func (c *checker) selectionMatches(v contract.TaskView, hop string) bool {
	f := c.flow()
	if f == nil {
		return false
	}
	r, ok := f.Role(hop)
	return ok && v.Request.Override == nil && v.Effective.Model == r.Model && v.Effective.Effort == r.Effort && v.Role.Adapter == r.Adapter
}

// succeeded reports a succeeded task with exit 0 and a complete final
// message equal to its final.txt.
func succeeded(t *taskEvidence) bool {
	v := t.View
	r := v.Result
	return v.State == contract.TaskSucceeded && r != nil && r.ExitCode != nil && *r.ExitCode == 0 && r.FinalMessage != nil &&
		!r.FinalMessageTruncated && t.HasFinal && bytes.Equal(t.Final, []byte(*r.FinalMessage))
}

// duration returns a terminal view's created-to-finished duration.
func duration(v contract.TaskView) (time.Duration, bool) {
	s, ok1 := contract.ParseTime(v.CreatedAt)
	if v.FinishedAt == nil || !ok1 {
		return 0, false
	}
	e, ok2 := contract.ParseTime(*v.FinishedAt)
	return e.Sub(s), ok2
}

func (c *checker) preflight() {
	m := c.b.Manifest
	for i, h := range Hops {
		t := c.b.Preflight[i]
		if t == nil {
			c.fail(CritPreflight, "preflight_missing")
			continue
		}
		v := t.View
		if v.Role.ID != h || v.WorkspaceBinding != nil || len(v.Request.Payload) == 0 || m == nil || v.Request.Payload[0] != markerFor(m.RunID, preflightHop(i)) {
			c.fail(CritPreflight, "preflight_binding_mismatch")
		}
		if !c.selectionMatches(v, h) {
			c.fail(CritPreflight, "preflight_selection_mismatch")
		}
		if !succeeded(t) || !authAnswerOK(*v.Result.FinalMessage) {
			c.fail(CritPreflight, "preflight_not_authenticated")
		}
	}
}

// shellWord splits the first shellQuote-rendered word off s, reporting
// whether it round-trips exactly.
func shellWord(s string) (word, rest string, ok bool) {
	if !strings.HasPrefix(s, "'") {
		i := strings.IndexByte(s, ' ')
		if i < 0 {
			i = len(s)
		}
		word, rest = s[:i], s[i:]
	} else {
		var b strings.Builder
		r := s[1:]
		for {
			i := strings.IndexByte(r, '\'')
			if i < 0 {
				return "", "", false
			}
			b.WriteString(r[:i])
			r = r[i+1:]
			if !strings.HasPrefix(r, `\''`) {
				break
			}
			b.WriteByte('\'')
			r = r[3:]
		}
		word, rest = b.String(), r
	}
	return word, rest, word != "" && shellQuote(word)+rest == s
}

// tokenSpells reports whether a sanitized path token spells real.
func tokenSpells(token, real string) bool {
	if token == real {
		return filepath.IsAbs(real)
	}
	for _, t := range []string{"$CHECKOUT", "$RUN", "$HOME"} {
		if rest, ok := strings.CutPrefix(token, t); ok && strings.HasPrefix(rest, "/") {
			return filepath.IsAbs(real) && len(real) > len(rest) && strings.HasSuffix(real, rest)
		}
	}
	return false
}

// permissionInputs reconstructs the allow-list's fixed values (A3.6) from
// independently bound inputs, never from a recorded rule list: the
// runtime from the launch's working directory, the plane URL and the CA's
// runtime path from the sanitized MCP configuration, and the two
// executables from the first rendered wait template (itself bound to the
// hashed system prompt), the Callsheet one consistent with the MCP
// command.
func (c *checker) permissionInputs(l *LaunchRecord) (permissionValues, bool) {
	var v permissionValues
	if l == nil || !filepath.IsAbs(l.Cwd) || filepath.Base(l.Cwd) != "coord" || len(l.WaitTemplates) != len(Hops) {
		return v, false
	}
	v.runtime = filepath.Dir(l.Cwd)
	cfgBytes := c.b.Files["setup/mcp-config.json"]
	var cfg struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(cfgBytes, &cfg) != nil {
		return v, false
	}
	srv, ok := cfg.Servers[mcpServerName]
	if !ok || len(srv.Args) != 7 {
		return v, false
	}
	caRest, ok := strings.CutPrefix(srv.Args[4], "$RUN")
	if want, err := mcpConfig(srv.Command, srv.Args[2], srv.Args[4]); !ok || !strings.HasPrefix(caRest, "/") || err != nil || !bytes.Equal(want, cfgBytes) {
		return v, false
	}
	v.url, v.ca = srv.Args[2], v.runtime+caRest
	callsheet, rest, ok := shellWord(l.WaitTemplates[0])
	_, capture, found := strings.Cut(rest, "; ")
	reale2e, _, ok2 := shellWord(capture)
	if !ok || !found || !ok2 || !tokenSpells(srv.Command, callsheet) || !filepath.IsAbs(reale2e) {
		return v, false
	}
	v.callsheet, v.reale2e = callsheet, reale2e
	return v, true
}

// expectedLaunch is phase p's only permitted launch: the shared argv with
// the allow-list and wait templates rendered from the reconstructed
// fixed values, the MCP configuration at <runtime>/mcp.json and the
// rendered prompts (phase 2 resumes the phase-1 session). ok is false
// when the fixed values are unrecoverable or hold a rule metacharacter.
func (c *checker) expectedLaunch(p int) (argv, waits []string, ok bool) {
	v, ok := c.permissionInputs(c.refLaunch())
	if !ok {
		return nil, nil, false
	}
	allowed, err := allowedTools(v)
	if err != nil {
		return nil, nil, false
	}
	for i := range Hops {
		waits = append(waits, waitCommand(v, i))
	}
	prompt := string(c.b.Files["setup/coordinator-phase-"+fmt.Sprint(p)+".md"])
	system := string(c.b.Files["setup/coordinator-prompt.md"])
	argv = launchArgv(p, prompt, allowed, filepath.Join(v.runtime, "mcp.json"), system, c.derived.SessionID)
	if !strings.Contains(system, shellQuote(v.reale2e)+" observe --run "+shellQuote(v.runtime)+" ") {
		return nil, nil, false
	}
	return argv, waits, true
}

// refLaunch is the first recorded launch, whose working directory and
// templates every launch must share.
func (c *checker) refLaunch() *LaunchRecord {
	if c.b.Launches[0] != nil {
		return c.b.Launches[0]
	}
	return c.b.Launches[1]
}

// launchValid reports whether phase p's recorded launch has exactly the
// permitted shape.
func (c *checker) launchValid(p int) bool {
	l, ref := c.b.Launches[p-1], c.refLaunch()
	argv, waits, ok := c.expectedLaunch(p)
	prompt := c.b.Files["setup/coordinator-phase-"+fmt.Sprint(p)+".md"]
	system := c.b.Files["setup/coordinator-prompt.md"]
	return ok && l.Phase == p && slices.Equal(l.Argv, argv) && slices.Equal(l.WaitTemplates, waits) && l.Executable == ref.Executable &&
		l.Cwd == ref.Cwd && l.PromptSHA256 == sha256Hex(prompt) && l.SystemSHA256 == sha256Hex(system)
}

// setup (A3, A3.6): each recorded launch has the permitted shape with the
// exact 16-rule allow-list, the rendered prompt bytes and one MCP
// configuration, the same executable, cwd and wait templates; phase 2
// resumes the session phase 1 reported, observed again in phase 2's init;
// each launched phase's supervisor-observed exit is 0 with its group
// gone. Each phase is validated on its own: an unlaunched phase is only
// launch_missing, and an exit is unobserved only for a launched phase.
func (c *checker) setup() {
	for _, f := range []string{"setup/coordinator-prompt.md", "setup/mcp-config.json", "setup/" + PhasePrompt1} {
		if len(c.b.Files[f]) == 0 {
			c.fail(CritSetup, "setup_file_missing")
		}
	}
	sid := c.derived.SessionID
	system := string(c.b.Files["setup/coordinator-prompt.md"])
	l1, l2 := c.b.Launches[0], c.b.Launches[1]
	for p, l := range c.b.Launches {
		if l == nil {
			c.fail(CritSetup, "launch_missing")
			continue
		}
		if !c.launchValid(p + 1) {
			c.fail(CritSetup, "launch_shape_invalid")
		}
	}
	if ref := c.refLaunch(); ref != nil {
		for _, w := range ref.WaitTemplates {
			if !strings.Contains(system, w) {
				c.fail(CritSetup, "launch_shape_invalid")
			}
		}
	}
	if l1 != nil && (!sessionIDRE.MatchString(sid) || l1.ResumeSession != "") {
		c.fail(CritSetup, "session_resume_mismatch")
	}
	if l2 != nil && (!sessionIDRE.MatchString(sid) || l2.ResumeSession != sid || len(c.derived.Init) != 2 || c.derived.Init[1].SessionID != sid) {
		c.fail(CritSetup, "session_resume_mismatch")
	}
	for i, l := range c.b.Launches {
		if l == nil {
			continue
		}
		obs := c.phaseObservations(i + 1)
		if len(obs) != 1 {
			c.fail(CritSetup, "coordinator_exit_unobserved")
			continue
		}
		if p := obs[0]; !p.Reaped || !p.GroupGone || p.ExitCode == nil || *p.ExitCode != 0 || p.Signal != "" || p.PID <= 0 ||
			p.StartedSeq != l.StartedSeq || p.ExitedSeq <= p.StartedSeq {
			c.fail(CritSetup, "coordinator_exit_nonzero")
		}
	}
}

// phaseObservations are the coordinator exit observations of phase p.
func (c *checker) phaseObservations(p int) []PhaseProcess {
	var out []PhaseProcess
	if ce := c.b.CoordExit; ce != nil {
		for _, o := range ce.Phases {
			if o.Phase == p {
				out = append(out, o)
			}
		}
	}
	return out
}

// loadHops derives the views, commits and tree files used by later
// criteria.
func (c *checker) loadHops() {
	for i := range Hops {
		if t := c.b.Hops[i].Task; t != nil {
			v := t.View
			c.views[i] = &v
		}
	}
	r := c.b.Repo
	if r == nil {
		return
	}
	seed, ok, err := r.commit(evidenceRefs[0])
	if ok && err == nil {
		c.seed = seed
		if t, err := seed.Tree(); err == nil {
			c.files[0], _ = treeFiles(t, maxDecodedBytes)
		}
	}
	for i := range Hops {
		cm, ok, err := r.commit(evidenceRefs[i+1])
		if !ok || err != nil {
			continue
		}
		c.commits[i] = cm
		if t, err := cm.Tree(); err == nil {
			c.files[i+1], _ = treeFiles(t, maxDecodedBytes)
		}
	}
}

// hopTask is hop i's task ID, or "".
func (c *checker) hopTask(i int) string {
	if c.views[i] == nil {
		return ""
	}
	return c.views[i].TaskID
}

// mapStep is one required event-map entry: a kind and, for hop-bound
// kinds, the 0-based hop.
type mapStep struct {
	kind string
	hop  int
}

// canonicalMap is the event map's required entries in their required
// order (a hop's wake, when observed, sits between its wait start and its
// consumption and is not itself required for every hop).
func canonicalMap() []mapStep {
	steps := []mapStep{{MapLaunch, -1}}
	for i := range Hops {
		if i == 2 {
			steps = append(steps, mapStep{MapPause, -1}, mapStep{MapDecision, -1}, mapStep{MapResume, -1})
		}
		steps = append(steps, mapStep{MapDispatch, i}, mapStep{MapWaitStart, i}, mapStep{MapWaitConsumed, i})
	}
	return append(steps, mapStep{MapFinalResult, -1})
}

// eventMap (A3): the stored map is re-derived from the raw phase streams,
// transcript snapshots, launches and supervisor events and must equal the
// derivation; it is machine-derived with every source reference bound to
// a fixed bundle file's hash and line span, holds the required entries in
// order and has no finding (each finding's code fails the criterion).
func (c *checker) eventMap() {
	em, m := c.b.EventMap, c.b.Manifest
	if em == nil {
		c.fail(CritEventMap, "event_map_missing")
	} else {
		stored, _ := encodeJSON(em)
		derived, _ := encodeJSON(c.derived)
		if !bytes.Equal(stored, derived) {
			c.fail(CritEventMap, "event_map_mismatch")
		}
		if em.Source != MapSourceDerived {
			c.fail(CritEventMap, "event_map_not_derived")
		}
	}
	d := c.derived
	if m == nil || d.RunID != m.RunID || !sessionIDRE.MatchString(d.SessionID) {
		c.fail(CritEventMap, "event_map_binding_mismatch")
	}
	for _, f := range d.Findings {
		c.fail(CritEventMap, f.Code)
	}
	allowed := map[string]bool{phaseStreamFiles[0]: true, phaseStreamFiles[1]: true, transcriptFiles[0]: true, transcriptFiles[1]: true}
	// Each source file's digest and line count, computed once.
	type sourceFile struct {
		sha256 string
		lines  int
	}
	sources := map[string]sourceFile{}
	for _, e := range d.Entries {
		for _, s := range e.Sources {
			data, ok := c.b.Files[s.File]
			if !allowed[s.File] || !ok {
				c.fail(CritEventMap, "source_span_invalid")
				continue
			}
			f, seen := sources[s.File]
			if !seen {
				f = sourceFile{sha256Hex(data), len(transcriptLines(data))}
				sources[s.File] = f
			}
			if s.SHA256 != f.sha256 || s.FirstLine < 1 || s.LastLine < s.FirstLine || s.LastLine > f.lines {
				c.fail(CritEventMap, "source_span_invalid")
			}
		}
	}
	var ordered []MapEntry
	for _, e := range d.Entries {
		if e.Kind != MapWake {
			ordered = append(ordered, e)
		}
	}
	steps := canonicalMap()
	if len(ordered) != len(steps) {
		c.fail(CritEventMap, "event_map_incomplete")
		return
	}
	for i, s := range steps {
		e := ordered[i]
		ok := e.Kind == s.kind && e.SessionID == d.SessionID
		if s.hop >= 0 {
			ok = ok && e.Hop == Hops[s.hop] && e.Task != "" && e.Task == c.hopTask(s.hop)
		}
		if !ok {
			c.fail(CritEventMap, "event_map_incomplete")
			return
		}
		if e.Kind == MapDispatch {
			if dr := c.b.Hops[s.hop].Dispatch; dr == nil || dr.ObservedSeq != e.Seq {
				c.fail(CritEventMap, "dispatch_not_observed")
			}
		}
	}
}

// waits (A3): every hop's direct CLI background wait (the rendered
// template run with run_in_background, its handle returned), its raw
// redirected files reparsed into exactly the stored wait record, its
// consumption after the wait started, and at least one automatic wake (a
// vendor completion notification consumed with no ordinary user input
// between). A long blocking MCP call is a finding of the derivation.
func (c *checker) waits() {
	d := c.derived
	wakes := 0
	for _, e := range d.Entries {
		if e.Kind == MapWake && e.Automatic {
			wakes++
		}
	}
	if wakes == 0 {
		c.fail(CritWaits, "automatic_wake_unobserved")
	}
	if slices.ContainsFunc(d.Findings, func(f MapFinding) bool { return f.Code == CodeLongPoll }) {
		c.fail(CritWaits, CodeLongPoll)
	}
	for i := range Hops {
		if len(entriesOf(d, MapWaitStart, Hops[i])) != 1 || len(entriesOf(d, MapWaitConsumed, Hops[i])) != 1 {
			c.fail(CritWaits, "wait_unobserved")
		}
		w := c.b.Hops[i].Wait
		dir := "hops/" + hopDir(i) + "/"
		result, okR := c.b.Files[dir+waitResultFile]
		stderr := c.b.Files[dir+waitStderrFile]
		exit, okX := c.b.Files[dir+waitExitFile]
		switch {
		case w == nil:
			c.fail(CritWaits, "wait_record_missing")
		case !okR || !okX:
			c.fail(CritWaits, "wait_files_missing")
		case w.Hop != Hops[i] || w.Handle != waitHandle(i) || w.Task == "" || w.Task != c.hopTask(i) || w.Winner != w.Task:
			c.fail(CritWaits, "wait_binding_mismatch")
		case w.ExitCode == nil || *w.ExitCode != 0 || w.WinnerState != contract.TaskSucceeded || !hex64RE.MatchString(w.ResultSHA256):
			c.fail(CritWaits, "wait_unsuccessful")
		}
		if w != nil && okR && okX && !c.waitRecordMatches(i, w, result, stderr, exit) {
			c.fail(CritWaits, "wait_record_mismatch")
		}
	}
}

// waitRecordMatches reports whether hop i's stored wait record w is
// exactly its reparse from the raw wait files (once per check).
func (c *checker) waitRecordMatches(i int, w *WaitRecord, result, stderr, exit []byte) bool {
	if c.memo != nil && c.memo.waits[i] != 0 {
		return c.memo.waits[i] > 0
	}
	re := waitRecordOf(i, w.Task, result, stderr, exit, true, true)
	ok := bytes.Equal(mustJSON(re), mustJSON(*w))
	if c.memo != nil {
		c.memo.waits[i] = -1
		if ok {
			c.memo.waits[i] = 1
		}
	}
	return ok
}

// mustJSON renders v (a value the package itself encodes).
func mustJSON(v any) []byte {
	b, _ := encodeJSON(v)
	return b
}

func (c *checker) hops() {
	m := c.b.Manifest
	ids := map[string]bool{}
	for i := range Hops {
		if t := c.b.Preflight[i]; t != nil {
			ids[t.View.TaskID] = true
		}
	}
	for i, h := range Hops {
		he := c.b.Hops[i]
		if he.Task == nil {
			c.fail(CritHops, "hop_missing")
			continue
		}
		v := he.Task.View
		if ids[v.TaskID] {
			c.fail(CritHops, "task_not_unique")
		}
		ids[v.TaskID] = true
		if v.Role.ID != h || m == nil || len(v.Request.Payload) == 0 || v.Request.Payload[0] != markerFor(m.RunID, h) {
			c.fail(CritHops, "hop_binding_mismatch")
		}
		if !c.selectionMatches(v, h) {
			c.fail(CritHops, "selection_mismatch")
		}
		if !succeeded(he.Task) {
			c.fail(CritHops, "hop_not_succeeded")
		} else if finalMarker(h, *v.Result.FinalMessage) != approvedMarkers[h] {
			c.fail(CritHops, "final_marker_missing")
		}
		if v.Result == nil || v.Result.Workspace == nil || v.Result.Workspace.Publication != contract.PublicationPublished || v.Result.Workspace.Commit == nil {
			c.fail(CritHops, "publication_missing")
		}
		if !he.HasLogs || he.LogMeta == nil {
			c.fail(CritHops, "logs_missing")
		}
		d := he.Dispatch
		if d == nil {
			c.fail(CritHops, "dispatch_record_missing")
			continue
		}
		b := v.WorkspaceBinding
		if d.Hop != h || d.Task != v.TaskID || d.RoleID != h || b == nil || b.BaseCommit == nil || d.Workspace != b.Name || d.Instance != b.Instance ||
			d.BaseCommit != *b.BaseCommit || d.Model != v.Effective.Model || d.Effort != v.Effective.Effort || len(v.Request.Payload) == 0 ||
			d.Marker != v.Request.Payload[0] {
			c.fail(CritHops, "dispatch_record_mismatch")
		}
	}
}

// resultCommit is hop i's published commit, or "".
func (c *checker) resultCommit(i int) string {
	v := c.views[i]
	if v == nil || v.Result == nil || v.Result.Workspace == nil || v.Result.Workspace.Commit == nil {
		return ""
	}
	return *v.Result.Workspace.Commit
}

func (c *checker) chain() {
	m, tm, r := c.b.Manifest, c.b.Trees, c.b.Repo
	if r == nil {
		c.fail(CritChain, "repository_missing")
		if c.b.RepoErr != nil {
			c.fail(CritChain, "repository_invalid")
		}
		return
	}
	if tm == nil || m == nil {
		c.fail(CritChain, "tree_manifest_missing")
		return
	}
	if c.seed == nil || c.seed.Hash.String() != m.Workspace.SeedCommit || tm.SeedCommit != m.Workspace.SeedCommit ||
		c.seed.TreeHash.String() != m.Workspace.SeedTree || tm.SeedTree != m.Workspace.SeedTree {
		c.fail(CritChain, "seed_mismatch")
	}
	if len(tm.Hops) != len(Hops) {
		c.fail(CritChain, "tree_manifest_incomplete")
	}
	base := m.Workspace.SeedCommit
	for i, h := range Hops {
		v, cm := c.views[i], c.commits[i]
		if v == nil || cm == nil {
			c.fail(CritChain, "result_commit_missing")
			return
		}
		b := v.WorkspaceBinding
		if b == nil || b.Name != m.Workspace.Name || b.Instance != m.Workspace.Instance {
			c.fail(CritChain, "workspace_instance_mismatch")
		}
		if b == nil || b.BaseCommit == nil || *b.BaseCommit != base {
			c.fail(CritChain, "base_binding_mismatch")
		}
		if c.resultCommit(i) != cm.Hash.String() {
			c.fail(CritChain, "result_commit_mismatch")
		}
		if len(cm.ParentHashes) != 1 || cm.ParentHashes[0].String() != base {
			c.fail(CritChain, "parent_mismatch")
		}
		if i < len(tm.Hops) {
			ht := tm.Hops[i]
			if ht.Hop != h || ht.Task != v.TaskID || ht.Commit != cm.Hash.String() || ht.Parent != base || ht.Tree != cm.TreeHash.String() {
				c.fail(CritChain, "tree_manifest_mismatch")
			}
		}
		base = cm.Hash.String()
	}
}

// projectBound is the throwaway project's tracked-size limit.
const projectBound = 16 << 10

func (c *checker) artifacts() {
	if c.files[0] == nil {
		c.fail(CritArtifacts, "seed_tree_missing")
		return
	}
	for _, p := range []string{"go.mod", "main.go", "main_test.go", "FEATURE.md"} {
		if _, ok := c.files[0][p]; !ok {
			c.fail(CritArtifacts, "seed_incomplete")
		}
	}
	for _, p := range []string{"design.md", "design-review.md"} {
		if _, ok := c.files[0][p]; ok {
			c.fail(CritArtifacts, "seed_has_feature_artifacts")
		}
	}
	for i, h := range Hops {
		before, after := c.files[i], c.files[i+1]
		if after == nil {
			c.fail(CritArtifacts, "hop_tree_missing")
			return
		}
		for _, p := range changedPaths(before, after) {
			if !slices.Contains(allowedChanges[h], p) {
				c.fail(CritArtifacts, "disallowed_change")
			}
		}
		size := 0
		for _, b := range after {
			size += len(b)
		}
		if size >= projectBound {
			c.fail(CritArtifacts, "project_too_large")
		}
	}
	if d := c.files[1]["design.md"]; len(bytes.TrimSpace(d)) == 0 {
		c.fail(CritArtifacts, "design_missing")
	}
	rev, ok := c.files[2]["design-review.md"]
	if !ok || lastNonEmptyLine(string(rev)) != approvedMarkers[HopDesignReviewer] {
		c.fail(CritArtifacts, "design_review_not_approved")
	}
	// The coder (hop 3) implements both files; the approved design and
	// review stay byte-identical (allowedChanges), and the Grok reviewer's
	// published tree (hop 4) is exactly the coder's.
	for _, p := range []string{"main.go", "main_test.go"} {
		if bytes.Equal(c.files[2][p], c.files[3][p]) {
			c.fail(CritArtifacts, "implementation_missing")
		}
	}
	if c.commits[2].TreeHash != c.commits[3].TreeHash {
		c.fail(CritArtifacts, "reviewer_tree_changed")
	}
}

func (c *checker) owner() {
	d, m := c.b.Decision, c.b.Manifest
	if d == nil {
		c.fail(CritOwner, "decision_missing")
		return
	}
	if m == nil || d.RunID != m.RunID {
		c.fail(CritOwner, "decision_binding_mismatch")
	}
	if d.Decision != DecisionYes {
		c.fail(CritOwner, "owner_did_not_approve")
	}
	if d.ReviewedTask == "" || d.ReviewedTask != c.hopTask(1) || d.ResultCommit != c.resultCommit(1) {
		c.fail(CritOwner, "decision_binding_mismatch")
	}
	if f := c.files[2]; f == nil || d.DesignSHA256 != sha256Hex(f["design.md"]) || d.ReviewSHA256 != sha256Hex(f["design-review.md"]) {
		c.fail(CritOwner, "decision_digest_mismatch")
	}
	decisions := c.eventsOf(EvOwnerDecision)
	if len(decisions) != 1 || decisions[0].Seq != d.Seq || decisions[0].Time != d.DecidedAt {
		c.fail(CritOwner, "decision_event_mismatch")
		return
	}
	dec := decisions[0]
	review, ok1 := c.firstEvent(EvTaskTerminal, func(e Event) bool { return e.Hop == HopDesignReviewer })
	waiting, ok2 := c.firstEvent(EvAwaitingOwner, nil)
	receipt, ok3 := c.firstEvent(EvReceiptCreated, nil)
	if !ok1 || !ok2 || !ok3 || !(review.Seq < waiting.Seq && waiting.Seq < dec.Seq && dec.Seq < receipt.Seq) {
		c.fail(CritOwner, "decision_out_of_order")
	}
	if ok2 && eventTime(dec).Sub(eventTime(waiting)) > OwnerDecisionBound {
		c.fail(CritOwner, "decision_deadline_exceeded")
	}
	coder, okc := c.firstEvent(EvTaskAdmitted, func(e Event) bool { return e.Hop == HopCoder })
	obs, oko := c.firstEvent(EvObserved, func(e Event) bool { return e.Hop == HopCoder })
	if !okc || !oko || coder.Seq < receipt.Seq || obs.Seq < receipt.Seq {
		c.fail(CritOwner, "coding_before_approval")
	}
	if v := c.views[2]; v != nil {
		created, ok := contract.ParseTime(v.CreatedAt)
		if !ok || created.Before(eventTime(dec)) {
			c.fail(CritOwner, "coding_before_approval")
		}
	}
}

func (c *checker) admissions() {
	if len(c.eventsOf(EvPrematureAdmission)) != 0 {
		c.fail(CritAdmissions, "premature_admission")
	}
	if len(c.eventsOf(EvExtraAdmission)) != 0 || len(c.eventsOf(EvTaskCancelled)) != 0 {
		c.fail(CritAdmissions, "extra_admission")
	}
	want := map[string]string{}
	for i := range Hops {
		if t := c.b.Preflight[i]; t != nil {
			want[t.View.TaskID] = preflightHop(i)
		}
		if id := c.hopTask(i); id != "" {
			want[id] = Hops[i]
		}
	}
	admitted := c.eventsOf(EvTaskAdmitted)
	if len(admitted) != 2*len(Hops) || len(want) != 2*len(Hops) {
		c.fail(CritAdmissions, "admissions_mismatch")
		return
	}
	for _, e := range admitted {
		if want[e.Task] != e.Hop {
			c.fail(CritAdmissions, "admissions_mismatch")
		}
	}
	// Feature tasks are admitted in hop order, each after the previous
	// hop's terminal event.
	for i := 1; i < len(Hops); i++ {
		adm, ok1 := c.firstEvent(EvTaskAdmitted, func(e Event) bool { return e.Hop == Hops[i] })
		prev, ok2 := c.firstEvent(EvTaskTerminal, func(e Event) bool { return e.Hop == Hops[i-1] })
		if !ok1 || !ok2 || adm.Seq < prev.Seq {
			c.fail(CritAdmissions, "admission_out_of_order")
		}
	}
}

func (c *checker) grok() {
	he := c.b.Hops[3]
	if he.Task == nil || c.files[3] == nil {
		c.fail(CritGrok, "review_missing")
		return
	}
	v := he.Task.View
	if v.Result == nil || v.Result.FinalMessage == nil || strings.TrimSpace(*v.Result.FinalMessage) == "" || v.Result.FinalMessageTruncated {
		c.fail(CritGrok, "review_missing")
	} else if lastNonEmptyLine(*v.Result.FinalMessage) != approvedMarkers[HopCodeReviewer] {
		c.fail(CritGrok, "review_not_approved")
	}
	inline, err := parseInlineFiles(v.Request.Goal)
	if err != nil || len(inline) != len(inlinePaths) {
		c.fail(CritGrok, "inline_files_invalid")
	}
	for _, p := range inlinePaths {
		if !bytes.Equal(inline[p], c.files[3][p]) {
			c.fail(CritGrok, "inline_source_mismatch")
		}
	}
	d := he.Dispatch
	if d == nil || len(d.InlineFiles) != len(inlinePaths) {
		c.fail(CritGrok, "inline_hashes_missing")
		return
	}
	for i, f := range d.InlineFiles {
		if f.Path != inlinePaths[i] || f.SHA256 != sha256Hex(c.files[3][f.Path]) {
			c.fail(CritGrok, "inline_source_mismatch")
		}
	}
}

// finalTestEnv must be present in the final test's environment record.
var finalTestEnv = []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off"}

// testArgv is the required test command. -buildvcs=false: the throwaway
// export never consults the VCS state of a directory above it.
var testArgv = []string{"go", "test", "-buildvcs=false", "-count=1", "./..."}

func (c *checker) testRun(t *TestRun, commit, tree string, files map[string][]byte, missing, bad string) {
	if t == nil {
		c.fail(CritFinalTests, missing)
		return
	}
	var want []string
	for p := range files {
		want = append(want, p)
	}
	sort.Strings(want)
	envOK := true
	for _, e := range finalTestEnv {
		envOK = envOK && slices.Contains(t.Env, e)
	}
	if !slices.Equal(t.Argv, testArgv) || !envOK || t.ExitCode != 0 || t.Signal != "" || t.TimedOut || t.DurationMS < 0 ||
		time.Duration(t.DurationMS)*time.Millisecond > FinalTestBound || strings.TrimSpace(t.Toolchain) == "" || commit == "" ||
		t.Commit != commit || t.Tree != tree || !slices.Equal(t.Files, want) {
		c.fail(CritFinalTests, bad)
	}
}

func (c *checker) finalTests() {
	seedCommit, seedTree := "", ""
	if c.seed != nil {
		seedCommit, seedTree = c.seed.Hash.String(), c.seed.TreeHash.String()
	}
	c.testRun(c.b.Baseline, seedCommit, seedTree, c.files[0], "baseline_missing", "baseline_failed")
	commit, tree := "", ""
	if cm := c.commits[3]; cm != nil {
		commit, tree = cm.Hash.String(), cm.TreeHash.String()
	}
	c.testRun(c.b.Final, commit, tree, c.files[4], "final_tests_missing", "final_tests_failed")
	c.validationRuns(commit, tree)
}

func (c *checker) cleanup() {
	cl := c.b.Cleanup
	if cl == nil {
		c.fail(CritCleanup, "cleanup_missing")
		return
	}
	if !cl.Complete || len(cl.UnsettledTasks) != 0 {
		c.fail(CritCleanup, "cleanup_incomplete")
	}
	// A3.6: started-instance accounting. Every successful start event (a
	// process or phase start with its captured PID) is one instance; the
	// cleanup inventory lists exactly these, in order, and each has exactly
	// one stop record of the same name, PID and start sequence, verified.
	var started []StartedProc
	for _, e := range c.b.Events {
		if e.Type == EvProcessStarted || e.Type == EvPhaseStarted {
			started = append(started, StartedProc{Name: e.Handle, PID: e.PID, StartedSeq: e.Seq})
			if e.PID <= 0 || e.Handle == "" {
				c.fail(CritCleanup, "process_record_conflict")
			}
		}
	}
	if !slices.Equal(started, cl.Started) {
		c.fail(CritCleanup, "process_record_conflict")
	}
	bySeq := map[int]StartedProc{}
	for _, s := range started {
		bySeq[s.StartedSeq] = s
	}
	stops := map[int]int{}
	for _, p := range cl.Processes {
		if s, ok := bySeq[p.StartedSeq]; !ok || s.Name != p.Name || s.PID != p.PID {
			c.fail(CritCleanup, "process_record_conflict")
			continue
		}
		stops[p.StartedSeq]++
		if !p.ExitVerified || p.PID <= 0 {
			c.fail(CritCleanup, "process_exit_unverified")
		}
	}
	for _, s := range started {
		switch stops[s.StartedSeq] {
		case 0:
			c.fail(CritCleanup, "process_record_missing")
		case 1:
		default:
			c.fail(CritCleanup, "process_record_conflict")
		}
	}
	if !cl.RuntimeRemoved {
		c.fail(CritCleanup, "runtime_not_removed")
	}
	// A3: the coordinator phases are the supervisor's own observation: each
	// started phase's leader reaped and its whole group (MCP child,
	// background waits) gone, observed exactly once for that start; an
	// unstarted phase has nothing to observe.
	for p := 1; p <= 2; p++ {
		var starts []Event
		for _, e := range c.b.Events {
			if e.Type == EvPhaseStarted && e.Handle == phaseHandle(p) {
				starts = append(starts, e)
			}
		}
		obs := c.phaseObservations(p)
		if len(starts) == 0 && len(obs) == 0 {
			continue
		}
		if len(starts) != 1 || len(obs) != 1 {
			c.fail(CritCleanup, "coordinator_group_unverified")
			continue
		}
		l := c.b.Launches[p-1]
		if o := obs[0]; o.PID != starts[0].PID || o.StartedSeq != starts[0].Seq || !o.Reaped || !o.GroupGone || (l != nil && l.StartedSeq != starts[0].Seq) {
			c.fail(CritCleanup, "coordinator_group_unverified")
		}
	}
	if ce := c.b.CoordExit; ce == nil {
		c.fail(CritCleanup, "coordinator_group_unverified")
	} else {
		for _, o := range ce.Phases {
			if o.Phase != 1 && o.Phase != 2 {
				c.fail(CritCleanup, "coordinator_group_unverified")
			}
		}
	}
	s, ok1 := parseTime(cl.StartedAt)
	e, ok2 := parseTime(cl.FinishedAt)
	if !ok1 || !ok2 || e.Before(s) || e.Sub(s) > CleanupBound {
		c.fail(CritDeadlines, "cleanup_deadline_exceeded")
	}
}

func (c *checker) deadlines() {
	for i := range Hops {
		if t := c.b.Preflight[i]; t != nil {
			if d, ok := duration(t.View); !ok || d > PreflightBound {
				c.fail(CritDeadlines, "preflight_deadline_exceeded")
			}
		}
		if v := c.views[i]; v != nil {
			if d, ok := duration(*v); !ok || d > FeatureTaskBound {
				c.fail(CritDeadlines, "feature_deadline_exceeded")
			}
		}
	}
	start, ok1 := c.firstEvent(EvPreflightStarted, nil)
	end, ok2 := c.firstEvent(EvCollectionStarted, nil)
	if !ok1 || !ok2 || eventTime(end).Sub(eventTime(start)) > AttemptBound {
		c.fail(CritDeadlines, "attempt_deadline_exceeded")
	}
}

// validationArgvJSON and validationArgvBuild are the final validation's go
// test -json and build commands ($BIN: the runtime's binary path).
var (
	validationArgvJSON  = []string{"go", "test", "-json", "-buildvcs=false", "-count=1", "./..."}
	validationArgvBuild = []string{"go", "build", "-buildvcs=false", "-o", "$BIN", "."}
)

// validationRuns (A3, final_tests): the build, the ten CLI cases and the
// JSON test run are bound to the fourth result's commit and tree, ran
// within the one shared final-validation budget, and each start and end is
// a supervisor validation event of that command.
func (c *checker) validationRuns(commit, tree string) {
	cs, rq := c.b.Cases, c.b.Requirements
	if cs == nil || rq == nil {
		c.fail(CritFinalTests, "validation_missing")
		return
	}
	execs := []ExecRecord{cs.Build}
	ok := cs.Commit == commit && cs.Tree == tree && commit != "" && cs.Build.Name == "go_build" && slices.Equal(cs.Build.Argv, validationArgvBuild) &&
		cs.Build.Started && cs.Build.ExitCode == 0 &&
		!cs.Build.TimedOut && cs.Build.Signal == "" && len(cs.Cases) == len(FeatureCases)
	for i, r := range cs.Cases {
		if i >= len(FeatureCases) {
			break
		}
		fc := FeatureCases[i]
		ok = ok && r.ID == fc.ID && r.Exec.Name == "case_"+fc.ID && slices.Equal(r.Exec.Argv, append([]string{"greeting"}, fc.Args...))
		if r.Exec.Started {
			execs = append(execs, r.Exec)
		}
	}
	t := rq.TestRun
	ok = ok && t.Name == "go_test_json" && slices.Equal(t.Argv, validationArgvJSON) && t.Started && !t.TimedOut && t.Signal == "" &&
		t.StdoutSHA256 == sha256Hex(c.b.Files["validation/tests.jsonl"])
	execs = append(execs, t)
	if passed, _ := testRunVerdict(t, c.b.Files["validation/tests.jsonl"]); !passed {
		c.fail(CritFinalTests, CodeValidationFail)
	}
	var total int64
	for _, e := range execs {
		ok = ok && e.Schema == ExecSchema && e.Commit == commit && e.Tree == tree
		start, end := c.eventAt(e.StartSeq), c.eventAt(e.EndSeq)
		ok = ok && start != nil && end != nil && start.Type == EvValidationStarted && end.Type == EvValidationFinished && start.Code == e.Name &&
			end.Code == e.Name && e.StartSeq < e.EndSeq
		total += max(0, e.DurationMS)
	}
	if !ok {
		c.fail(CritFinalTests, "validation_binding_mismatch")
	}
	starts, ends := c.eventsOf(EvValidationStarted), c.eventsOf(EvValidationFinished)
	if time.Duration(total)*time.Millisecond > FinalTestBound ||
		(len(starts) > 0 && len(ends) > 0 && eventTime(ends[len(ends)-1]).Sub(eventTime(starts[0])) > FinalTestBound) {
		c.fail(CritFinalTests, "validation_deadline_exceeded")
	}
}

// eventAt returns the event with sequence seq, or nil.
func (c *checker) eventAt(seq int) *Event {
	if seq < 1 || seq > len(c.b.Events) || c.b.Events[seq-1].Seq != seq {
		return nil
	}
	return &c.b.Events[seq-1]
}

// resultCorrectness (A3, FP-7/FP-12): the deterministic feature checks
// recomputed from the stored records (every case's verdict from its
// captured output, the requirement-to-test map and source structure from
// the fourth tree and the go test -json stream), an approving review never
// alongside failing tests, and every mechanical audit row correct. The
// semantic judgments are explicitly excluded.
func (c *checker) resultCorrectness() {
	cs, rq := c.b.Cases, c.b.Requirements
	commit := ""
	if cm := c.commits[3]; cm != nil {
		commit = cm.Hash.String()
	}
	switch {
	case cs == nil:
		c.fail(CritResult, CodeCaseFailed)
	case len(cs.Cases) != len(FeatureCases):
		c.fail(CritResult, CodeCaseFailed)
	}
	if cs != nil {
		for i, r := range cs.Cases {
			if i >= len(FeatureCases) {
				break
			}
			pass, code := evaluateCase(FeatureCases[i], r.Exec, r.Stdout, r.Stderr)
			if pass != r.Pass || code != r.Code || r.ID != FeatureCases[i].ID ||
				(r.Exec.Started && (sha256Hex([]byte(r.Stdout)) != r.Exec.StdoutSHA256 || sha256Hex([]byte(r.Stderr)) != r.Exec.StderrSHA256)) {
				c.fail(CritResult, "case_record_mismatch")
			}
			if !pass {
				c.fail(CritResult, CodeCaseFailed)
			}
		}
	}
	testsFailed := c.b.Final == nil || c.b.Final.ExitCode != 0 || c.b.Final.TimedOut
	if rq == nil {
		c.fail(CritResult, CodeTestMapping)
	} else {
		var cases []CaseResult
		if cs != nil {
			cases = cs.Cases
		}
		want := buildRequirements(c.files[4], c.b.Files["validation/tests.jsonl"], cases, rq.TestRun, commit)
		if !requirementsEqual(want, *rq) {
			c.fail(CritResult, "requirements_mismatch")
		}
		if len(want.MissingTests) > 0 || len(want.FailedTests) > 0 || !want.RunSignature || !want.TableTests || !want.TestCallsRun {
			c.fail(CritResult, CodeTestMapping)
		}
		testsFailed = testsFailed || len(want.FailedTests) > 0 || !want.TestRunPassed
	}
	if v := c.views[3]; v != nil && v.Result != nil && v.Result.FinalMessage != nil &&
		finalMarker(HopCodeReviewer, *v.Result.FinalMessage) == approvedMarkers[HopCodeReviewer] && testsFailed {
		c.fail(CritResult, CodeReviewContra)
	}
	for _, r := range auditRows(c) {
		if r.Layer == LayerMechanical && r.Verdict != VerdictCorrect {
			c.fail(CritResult, "audit_row_not_correct")
		}
	}
}

// snapshotShards returns a phase's two stored shards, or false when one
// is missing.
func (c *checker) snapshotShards(phase string) ([][]byte, bool) {
	var out [][]byte
	for _, name := range envFiles(phase) {
		data, ok := c.b.Files[name]
		if !ok {
			return nil, false
		}
		out = append(out, data)
	}
	return out, true
}

// ownerEnvironment (A3, FP-11): the manifest's supplied roots (sorted,
// unique, never nested) are exactly both snapshots' and the diff's; both
// snapshots parse and are complete, every entry's class the one derived
// from its vendor and path; the diff's full-record detail is bound to the
// entries whose compact records differ (A3.4: every one that is not
// tool-maintained and a key-ordered prefix of the tool-maintained ones,
// A3.6), and the stored diff, its tool-maintained summary and counts
// included, is exactly its recomputation from that detail and the
// complete compact inventories; every protected difference fails
// (owner_environment_modified, whatever else failed) and a
// tool-maintained change, or omitted tool-maintained detail, alone never
// does. It resolves no host path.
func (c *checker) ownerEnvironment() {
	m, d := c.b.Manifest, c.b.Diff
	var roots []string
	if m != nil {
		roots = m.ProtectedRoots
	}
	var snaps [2]*Snapshot
	for i, phase := range []string{"before", "after"} {
		shards, ok := c.snapshotShards(phase)
		if !ok {
			c.fail(CritOwnerEnv, CodeEnvUnobserved)
			continue
		}
		s, err := parseSnapshot(shards)
		if err != nil || s.Phase != phase {
			c.fail(CritOwnerEnv, "snapshot_invalid")
			continue
		}
		// A complete claim needs the vendor homes and supplied roots
		// observed, with every record of a valid kind, scope and path.
		if err := checkInventory(s, roots); err != nil {
			c.fail(CritOwnerEnv, CodeEnvInventory)
			c.fail(CritOwnerEnv, CodeEnvUnobserved)
			continue
		}
		snaps[i] = s
	}
	for i, a := range roots {
		for _, b := range roots[i+1:] {
			if a == b || tokenStrictlyUnder(a, b) || tokenStrictlyUnder(b, a) {
				c.fail(CritOwnerEnv, "protected_roots_invalid")
			}
		}
	}
	for _, s := range snaps {
		if s != nil && (m == nil || !slices.Equal(s.Roots, roots)) {
			c.fail(CritOwnerEnv, "protected_roots_mismatch")
		}
		// A3.2: both snapshots discovered at the manifest's depth.
		if s != nil && (m == nil || s.Depth != m.ProtectDepth || m.ProtectDepth < 0 || m.ProtectDepth > maxProtectDepth) {
			c.fail(CritOwnerEnv, "protect_depth_mismatch")
		}
	}
	if d == nil {
		c.fail(CritOwnerEnv, "environment_diff_missing")
		return
	}
	if m == nil || !slices.Equal(d.ProtectedRoots, roots) {
		c.fail(CritOwnerEnv, "protected_roots_mismatch")
	}
	if snaps[0] != nil && snaps[1] != nil {
		// A3.4: the stored snapshots are compact; the diff's detail must
		// list exactly the entries whose compact records differ, each full
		// record hashing to its snapshot's, and every change is re-derived
		// from that bound detail.
		if err := verifyDetail(snaps[0], snaps[1], d.Detail); err != nil {
			c.fail(CritOwnerEnv, "environment_diff_mismatch")
			c.fail(CritOwnerEnv, CodeEnvUnobserved)
			snaps[0] = nil
		}
	}
	if snaps[0] != nil && snaps[1] != nil {
		want := diffFromDetail(snaps[0], snaps[1], roots, d.Detail)
		for _, phase := range []string{"before", "after"} {
			for _, name := range envFiles(phase) {
				fh := FileHash{Name: name, SHA256: sha256Hex(c.b.Files[name])}
				if phase == "before" {
					want.Before = append(want.Before, fh)
				} else {
					want.After = append(want.After, fh)
				}
			}
		}
		if !bytes.Equal(mustJSON(want), mustJSON(*d)) {
			c.fail(CritOwnerEnv, "environment_diff_mismatch")
		}
		for _, code := range want.Codes {
			c.fail(CritOwnerEnv, code)
		}
	}
	for _, code := range d.Codes {
		c.fail(CritOwnerEnv, code)
	}
	if !d.Complete {
		c.fail(CritOwnerEnv, CodeEnvUnobserved)
	}
	if len(d.Protected) > 0 {
		c.fail(CritOwnerEnv, CodeEnvModified)
	}
}
