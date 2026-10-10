package reale2e

import (
	"bytes"
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
)

// criteriaOrder is the report order.
var criteriaOrder = []string{CritManifest, CritEvents, CritRoles, CritPreflight, CritSetup, CritEventMap, CritWaits, CritHops,
	CritChain, CritArtifacts, CritOwner, CritAdmissions, CritGrok, CritFinalTests, CritCleanup, CritDeadlines}

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
}

func (c *checker) fail(crit, code string) {
	if !slices.Contains(c.codes[crit], code) {
		c.codes[crit] = append(c.codes[crit], code)
	}
}

// Check validates b and returns its criteria (PASS only when every
// criterion passes).
func Check(b *Bundle) []Criterion {
	c := &checker{b: b, codes: map[string][]string{}}
	c.manifest()
	c.events()
	c.roles()
	c.preflight()
	c.setup()
	c.loadHops()
	c.eventMap()
	c.waits()
	c.hops()
	c.chain()
	c.artifacts()
	c.owner()
	c.admissions()
	c.grok()
	c.finalTests()
	c.cleanup()
	c.deadlines()
	out := make([]Criterion, 0, len(criteriaOrder))
	for _, id := range criteriaOrder {
		cr := Criterion{ID: id, Result: "pass", Codes: []string{}}
		if codes := c.codes[id]; len(codes) > 0 {
			sort.Strings(codes)
			cr.Result, cr.Codes = "fail", codes
		}
		out = append(out, cr)
	}
	return out
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
	if !slices.Equal(names, renderedInventory()) {
		c.fail(CritManifest, "rendered_inventory_mismatch")
	}
	if _, ok := parseTime(m.StartedAt); !ok {
		c.fail(CritManifest, "started_at_invalid")
	}
}

// renderedInventory is the exact sorted list of rendered setup files.
func renderedInventory() []string {
	out := append([]string{"setup/coordinator-prompt.md", "setup/flow.json", "setup/mcp-config.json"}, manualFiles()...)
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
		EvSeedCreated, EvBaselineTests, EvCoordinatorReady, EvFinalTests, EvCollectionStarted, EvCleanupStarted, EvCleanupFinished} {
		if len(c.eventsOf(typ)) != 1 {
			c.fail(CritEvents, "required_event_missing")
		}
	}
	if len(c.eventsOf(EvAttemptFailed)) != 0 {
		c.fail(CritEvents, "attempt_failed")
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

// launchForbidden are coordinator launch flags that resume a session,
// change authentication or bypass permissions.
var launchForbidden = []string{"--resume", "-r", "--continue", "-c", "--bare", "--dangerously-skip-permissions", "--fork-session",
	"--permission-mode", "--allow-dangerously-skip-permissions"}

func (c *checker) setup() {
	l, a, m := c.b.Launch, c.b.Attestation, c.b.Manifest
	if l == nil {
		c.fail(CritSetup, "launch_missing")
	} else {
		if !sessionIDRE.MatchString(l.SessionID) {
			c.fail(CritSetup, "session_id_invalid")
		}
		required := map[string]bool{"--session-id": false, "--mcp-config": false, "--strict-mcp-config": false, "--append-system-prompt": false}
		for i, arg := range l.Argv {
			if _, ok := required[arg]; ok {
				required[arg] = true
			}
			if slices.Contains(launchForbidden, arg) || strings.HasPrefix(arg, "--resume=") || strings.HasPrefix(arg, "--permission-mode=") {
				c.fail(CritSetup, "launch_flag_forbidden")
			}
			if arg == "--session-id" && (i+1 >= len(l.Argv) || l.Argv[i+1] != l.SessionID) {
				c.fail(CritSetup, "session_id_mismatch")
			}
		}
		for _, ok := range required {
			if !ok {
				c.fail(CritSetup, "launch_incomplete")
			}
		}
	}
	for _, f := range []string{"setup/coordinator-prompt.md", "setup/mcp-config.json"} {
		if len(c.b.Files[f]) == 0 {
			c.fail(CritSetup, "setup_file_missing")
		}
	}
	if len(transcriptLines(c.b.Files["session/transcript.jsonl"])) == 0 {
		c.fail(CritSetup, "transcript_missing")
	}
	if a == nil {
		c.fail(CritSetup, "attestation_missing")
		return
	}
	if m == nil || a.RunID != m.RunID || l == nil || a.SessionID != l.SessionID {
		c.fail(CritSetup, "attestation_binding_mismatch")
	}
	if a.Resumed || !a.FreshSession {
		c.fail(CritSetup, "session_resumed")
	}
	if !a.EmptyCoordinatorDir || !a.NoPriorConversation || !a.NoExtraPrompt || !a.NoUnrelatedInstructions || !a.InstructionSourcesChecked {
		c.fail(CritSetup, "setup_not_clean")
	}
	if len(a.SetupInputs) == 0 {
		c.fail(CritSetup, "setup_inputs_missing")
	}
	for _, in := range a.SetupInputs {
		if !slices.Contains(allowedSetupInputs, in) {
			c.fail(CritSetup, "undeclared_setup_input")
		}
	}
	if strings.TrimSpace(a.ClaudeVersion) == "" || strings.TrimSpace(a.TranscriptExport) == "" {
		c.fail(CritSetup, "attestation_incomplete")
	}
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

// mapCanonical is the event map's required kinds in their required order:
// each entry is a kind and, for hop-bound kinds, the 0-based hop.
type mapStep struct {
	kind string
	hop  int
}

func canonicalMap() []mapStep {
	steps := []mapStep{{MapSessionLaunch, -1}}
	for i := range Hops {
		if i == 2 {
			steps = append(steps, mapStep{MapOwnerPause, -1}, mapStep{MapOwnerResume, -1})
		}
		steps = append(steps, mapStep{MapDispatch, i}, mapStep{MapWaitStart, i}, mapStep{MapWaitConsumed, i})
	}
	return append(steps, mapStep{MapFinalResult, -1})
}

func (c *checker) eventMap() {
	em, m := c.b.EventMap, c.b.Manifest
	if em == nil {
		c.fail(CritEventMap, "event_map_missing")
		return
	}
	data := c.b.Files["session/transcript.jsonl"]
	lines := transcriptLines(data)
	if !em.AttestedByOwner {
		c.fail(CritEventMap, "event_map_not_attested")
	}
	if m == nil || em.RunID != m.RunID || c.b.Launch == nil || em.SessionID != c.b.Launch.SessionID || em.TranscriptSHA256 != sha256Hex(data) {
		c.fail(CritEventMap, "event_map_binding_mismatch")
	}
	known := map[string]bool{MapSessionLaunch: true, MapDispatch: true, MapWaitStart: true, MapWaitConsumed: true, MapOwnerPause: true,
		MapOwnerResume: true, MapMCPWait: true, MapFinalResult: true}
	var ordered []MapEvent
	for _, e := range em.Events {
		if !known[e.Kind] {
			c.fail(CritEventMap, "event_kind_unknown")
			continue
		}
		if len(e.Lines) != 2 || e.Lines[0] < 1 || e.Lines[1] < e.Lines[0] || e.Lines[1] > len(lines) {
			c.fail(CritEventMap, "transcript_span_invalid")
			continue
		}
		for _, l := range lines[e.Lines[0]-1 : e.Lines[1]] {
			if !validJSONLine(l) {
				c.fail(CritEventMap, "transcript_span_invalid")
				break
			}
		}
		if e.Kind != MapMCPWait {
			ordered = append(ordered, e)
		}
	}
	steps := canonicalMap()
	if len(ordered) != len(steps) {
		c.fail(CritEventMap, "event_map_incomplete")
		return
	}
	prev := 0
	for i, s := range steps {
		e := ordered[i]
		ok := e.Kind == s.kind
		if s.hop >= 0 {
			ok = ok && e.Hop == Hops[s.hop] && e.Task != "" && e.Task == c.hopTask(s.hop)
			if s.kind != MapDispatch {
				ok = ok && e.Handle == waitHandle(s.hop)
			}
		}
		if !ok {
			c.fail(CritEventMap, "event_map_incomplete")
			return
		}
		if len(e.Lines) == 2 && e.Lines[0] < prev {
			c.fail(CritEventMap, "event_map_out_of_order")
		}
		if len(e.Lines) == 2 {
			prev = e.Lines[0]
		}
	}
}

func (c *checker) waits() {
	em := c.b.EventMap
	wakes := 0
	if em != nil {
		for _, e := range em.Events {
			switch e.Kind {
			case MapWaitStart:
				if !e.Background {
					c.fail(CritWaits, "wait_not_background")
				}
			case MapWaitConsumed:
				if e.AutomaticWake && !e.UserMessageBetween {
					wakes++
				}
			case MapMCPWait:
				if !e.Recovery || e.BudgetMS <= 0 || e.BudgetMS > 10000 {
					c.fail(CritWaits, "unqualified_long_poll")
				}
			case MapDispatch:
				if e.BudgetMS != 0 {
					c.fail(CritWaits, "dispatch_with_wait")
				}
			}
		}
	}
	if wakes == 0 {
		c.fail(CritWaits, "automatic_wake_unobserved")
	}
	for i := range Hops {
		w := c.b.Hops[i].Wait
		switch {
		case w == nil:
			c.fail(CritWaits, "wait_record_missing")
		case w.Hop != Hops[i] || w.Handle != waitHandle(i) || w.Task == "" || w.Task != c.hopTask(i) || w.Winner != w.Task:
			c.fail(CritWaits, "wait_binding_mismatch")
		case w.ExitCode == nil || *w.ExitCode != 0 || w.WinnerState != contract.TaskSucceeded || !hex64RE.MatchString(w.ResultSHA256):
			c.fail(CritWaits, "wait_unsuccessful")
		}
	}
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

// testArgv is the required test command.
var testArgv = []string{"go", "test", "-count=1", "./..."}

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
	names := map[string]bool{}
	for _, p := range cl.Processes {
		names[p.Name] = true
		if !p.ExitVerified || p.PID <= 0 {
			c.fail(CritCleanup, "process_exit_unverified")
		}
	}
	for _, n := range []string{"plane", "sidecar-codex", "sidecar-claude", "sidecar-grok"} {
		if !names[n] {
			c.fail(CritCleanup, "process_record_missing")
		}
	}
	o := cl.Owner
	if o == nil || o.Schema != CoordExitSchema || !o.SessionClosed || !o.MCPChildExited {
		c.fail(CritCleanup, "owner_children_unverified")
	} else {
		var handles []string
		for _, w := range o.WaitHandles {
			handles = append(handles, w.Handle)
			if w.ExitCode != 0 {
				c.fail(CritCleanup, "owner_children_unverified")
			}
		}
		if !slices.Equal(handles, []string{waitHandle(0), waitHandle(1), waitHandle(2), waitHandle(3)}) {
			c.fail(CritCleanup, "owner_children_unverified")
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
