package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
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

// Criterion is one predicate's result: pass with no codes, fail with its
// fixed failure codes, or (A3.8) not_reached with no codes and the code
// and event sequence of the attempt failure that stopped before its
// producing step.
type Criterion struct {
	ID       string   `json:"id"`
	Result   string   `json:"result"`
	Codes    []string `json:"codes"`
	Cause    string   `json:"cause,omitempty"`
	CauseSeq int      `json:"cause_seq,omitempty"`
}

// Criterion results.
const (
	ResultCriterionPass       = "pass"
	ResultCriterionFail       = "fail"
	ResultCriterionNotReached = "not_reached"
)

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
	// stop is the stopping point of a checker without a memo (A3.8).
	stop *stopPoint
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
	// stop is the event log's stopping point (A3.8).
	stop *stopPoint
}

// criterionMethods are the criterion predicates in evaluation order (set
// in init: result_correctness consults the others, A3.8).
var criterionMethods []func(*checker)

func init() {
	criterionMethods = []func(*checker){(*checker).manifest, (*checker).events, (*checker).roles, (*checker).preflight, (*checker).setup,
		(*checker).eventMap, (*checker).waits, (*checker).hops, (*checker).chain, (*checker).artifacts, (*checker).owner, (*checker).admissions,
		(*checker).grok, (*checker).finalTests, (*checker).resultCorrectness, (*checker).cleanup, (*checker).ownerEnvironment, (*checker).deadlines}
}

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

// criteria returns the criteria in report order. A criterion whose
// evaluated part has no failure but whose producing step was never reached
// is not_reached, referencing the stopping cause; a real failure always
// wins and is reported without the unreached part.
func (c *checker) criteria() []Criterion {
	out := make([]Criterion, 0, len(criteriaOrder))
	for _, id := range criteriaOrder {
		cr := Criterion{ID: id, Result: ResultCriterionPass, Codes: []string{}}
		codes := slices.Clone(c.codes[id])
		unreached := slices.Contains(codes, codeNotReached)
		codes = slices.DeleteFunc(codes, func(s string) bool { return s == codeNotReached })
		switch {
		case len(codes) > 0:
			sort.Strings(codes)
			cr.Result, cr.Codes = ResultCriterionFail, codes
		case unreached:
			s := c.stopPoint()
			cr.Result, cr.Cause, cr.CauseSeq = ResultCriterionNotReached, s.cause, s.seq
		}
		out = append(out, cr)
	}
	return out
}

// codeNotReached marks, among a criterion's codes, a part of it whose
// producing step the attempt never reached (A3.8). It is never reported as
// a failure code.
const codeNotReached = "~not_reached"

// milestone is one attempt step whose supervisor event shows that it ran,
// in lifecycle order (A3.8).
type milestone int

const (
	msBaseline milestone = iota
	msVersions
	msRuntime
	msRegistered
	msReconnected
	msReady
	msPreflight1
	msPreflight2
	msPreflight3
	msPreflight4
	msSeed
	msBaselineTests
	msCoordinator
	msPhase1
	// msHopN is hop N's admission (or observation); msTermN its task's
	// terminal state seen before any failure (review C1: an admitted hop
	// has not yet ended, and a task the cleanup cancelled after the stop
	// never ended on its own).
	msHop1
	msTerm1
	msHop2
	msTerm2
	msAwaiting
	msDecision
	msReceipt
	msPhase2
	msHop3
	msTerm3
	msHop4
	msTerm4
	msFinal
	// msPhase1Done and msPhase2Done are a phase's own exit before any
	// failure (its result record); an exit the cleanup caused after the
	// failure is not it. A phase may exit early, so neither implies the
	// steps before it: they stay outside the lifecycle chain, as does
	// msObservedN, the coordinator's observation of hop N (its dispatch
	// record), which a supervisor admission need not be preceded by.
	msPhase1Done
	msPhase2Done
	msObserved1
	msObserved2
	msObserved3
	msObserved4
	msCount
)

// msPreflight is preflight i's admission.
func msPreflight(i int) milestone { return msPreflight1 + milestone(i) }

// msHop is feature hop i's admission (or observation).
func msHop(i int) milestone { return [...]milestone{msHop1, msHop2, msHop3, msHop4}[i] }

// msTerm is feature hop i's terminal task before the stop.
func msTerm(i int) milestone { return [...]milestone{msTerm1, msTerm2, msTerm3, msTerm4}[i] }

// msObserved is the coordinator's observation of feature hop i.
func msObserved(i int) milestone { return msObserved1 + milestone(i) }

// msPhase is coordinator phase p's start.
func msPhase(p int) milestone {
	if p == 1 {
		return msPhase1
	}
	return msPhase2
}

// matches reports whether e is the supervisor event of step m.
func (m milestone) matches(e Event) bool {
	for i := range Hops {
		switch m {
		case msPreflight(i):
			return e.Type == EvTaskAdmitted && e.Hop == preflightHop(i)
		case msHop(i):
			return (e.Type == EvTaskAdmitted || e.Type == EvPrematureAdmission || e.Type == EvObserved) && e.Hop == Hops[i]
		case msTerm(i):
			return e.Type == EvTaskTerminal && e.Hop == Hops[i]
		case msObserved(i):
			return e.Type == EvObserved && e.Hop == Hops[i]
		}
	}
	switch m {
	case msBaseline:
		return e.Type == EvSnapshotTaken && e.Code == "before"
	case msPhase1, msPhase2:
		return e.Type == EvPhaseStarted && e.Handle == phaseHandle(1+boolInt(m == msPhase2))
	case msPhase1Done, msPhase2Done:
		return e.Type == EvPhaseExited && e.Handle == phaseHandle(1+boolInt(m == msPhase2Done))
	}
	return e.Type == milestoneEvents[m]
}

// boolInt is 1 for true.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// milestoneEvents are the event types of the steps matched by type alone.
var milestoneEvents = map[milestone]string{msVersions: EvVersionsChecked, msRuntime: EvRuntimeCreated, msRegistered: EvRolesRegistered,
	msReconnected: EvSidecarsReconnected, msReady: EvRolesReady, msSeed: EvSeedCreated, msBaselineTests: EvBaselineTests,
	msCoordinator: EvCoordinatorReady, msAwaiting: EvAwaitingOwner, msDecision: EvOwnerDecision, msReceipt: EvReceiptCreated, msFinal: EvFinalTests}

// stopPoint is where the event log shows the attempt stopped (A3.8): the
// first attempt_failed event's code (a fixed code, or attempt_failed when
// the recorded one is not code-shaped) and sequence, and which steps were
// reached. A step is unreached only when the attempt failed, the step's
// own event does not precede the failure and neither does any later
// step's; a step's evidence missing after it ran (or with a later step
// recorded before the failure) is still missing, never excused.
type stopPoint struct {
	cause   string
	seq     int
	reached [msCount]bool
}

// stopCodeRE is a fixed diagnostic code's shape.
var stopCodeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// stopPointOf derives the stopping point of an event log.
func stopPointOf(evs []Event) *stopPoint {
	s := &stopPoint{}
	failAt := slices.IndexFunc(evs, func(e Event) bool { return e.Type == EvAttemptFailed })
	if failAt < 0 {
		for m := range s.reached {
			s.reached[m] = true
		}
		return s
	}
	s.cause, s.seq = evs[failAt].Code, evs[failAt].Seq
	if !stopCodeRE.MatchString(s.cause) {
		s.cause = EvAttemptFailed
	}
	var before [msCount]bool
	for _, e := range evs[:failAt] {
		for m := range msCount {
			before[m] = before[m] || m.matches(e)
		}
	}
	later := false
	for m := msCount - 1; m >= 0; m-- {
		if m > msFinal {
			s.reached[m] = before[m]
			continue
		}
		later = later || before[m]
		s.reached[m] = later
	}
	return s
}

// msDone is phase p's own exit.
func msDone(p int) milestone { return msPhase1Done + milestone(p-1) }

// stopped reports whether the attempt stopped before some step.
func (s *stopPoint) stopped() bool { return slices.Contains(s.reached[:], false) }

// stopPoint is the bundle's stopping point (once per check).
func (c *checker) stopPoint() *stopPoint {
	switch {
	case c.memo != nil && c.memo.stop != nil:
		return c.memo.stop
	case c.memo == nil && c.stop != nil:
		return c.stop
	}
	s := stopPointOf(c.b.Events)
	if c.memo != nil {
		c.memo.stop = s
	} else {
		c.stop = s
	}
	return s
}

// reached reports whether the attempt reached step m.
func (c *checker) reached(m milestone) bool { return c.stopPoint().reached[m] }

// unreached marks crit's part produced by step m not reached when the
// attempt never reached m, and reports that.
func (c *checker) unreached(crit string, m milestone) bool {
	if c.reached(m) {
		return false
	}
	c.fail(crit, codeNotReached)
	return true
}

// due reports whether crit's part is due (its producing step ran),
// marking it not reached otherwise.
func (c *checker) due(crit string, ok bool) bool {
	if !ok {
		c.fail(crit, codeNotReached)
	}
	return ok
}

// The producing steps of a hop beyond its admission (A3.8 review C1),
// each ordered against the stop: its dispatch record (the coordinator's
// observation, or the hop ended), its wait's start (seen in the stream, or
// implied by its consumption), the wait's end (started, and the task ended
// before the stop) and the coordinator's consumption (it went on past the
// hop before the stop: the next hop admitted, the owner's prompt opened
// after phase 1, or, for the last hop, nothing left unreached).

func (c *checker) dispatchDue(i int) bool { return c.reached(msObserved(i)) || c.reached(msTerm(i)) }

func (c *checker) consumedDue(i int) bool {
	switch i {
	case 0, 2:
		return c.reached(msHop(i + 1))
	case 1:
		return c.reached(msAwaiting)
	}
	return !c.stopPoint().stopped()
}

func (c *checker) waitStarted(i int) bool {
	return len(entriesOf(c.derived, MapWaitStart, Hops[i])) > 0 || c.consumedDue(i)
}

func (c *checker) waitEnded(i int) bool { return c.waitStarted(i) && c.reached(msTerm(i)) }

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
	// A3.8: each part is judged once the step that records it ran; an
	// attempt that stopped before it leaves that part not reached.
	if !c.unreached(CritManifest, msVersions) {
		c.manifestVersions(m)
	}
	f := c.flow()
	switch {
	case f == nil && c.unreached(CritManifest, msCoordinator):
	case f == nil:
		c.fail(CritManifest, "flow_missing")
	case m.FlowKind != f.Kind() || m.FlowSHA256 != sha256Hex(c.b.Files["setup/flow.json"]) || !slices.Equal(m.Roles, f.Roles):
		c.fail(CritManifest, "flow_mismatch")
	}
	w := m.Workspace
	if !c.unreached(CritManifest, msSeed) && (!contract.ValidWorkspaceName(w.Name) || !contract.ValidWorkspaceToken(w.Instance) ||
		!hex40RE.MatchString(w.SeedCommit) || !hex40RE.MatchString(w.SeedTree)) {
		c.fail(CritManifest, "workspace_invalid")
	}
	if !c.unreached(CritManifest, msCoordinator) {
		c.manifestRendered(m)
	}
	// The launch inventory is exactly the launch records present, in phase
	// order, each with its hash.
	var names []string
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

// manifestVersions checks the probed revision and tool versions.
func (c *checker) manifestVersions(m *Manifest) {
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
}

// manifestRendered checks the template and rendered setup inventories.
func (c *checker) manifestRendered(m *Manifest) {
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
	// Phase 2's prompt follows a yes; an attempt that stopped before
	// phase 2 may never have rendered it (A3.8).
	withPhase2 := c.b.Decision != nil && c.b.Decision.Decision == DecisionYes
	if withPhase2 && !slices.Contains(names, "setup/"+PhasePrompt2) && c.unreached(CritManifest, msPhase2) {
		withPhase2 = false
	}
	if !slices.Equal(names, renderedInventory(withPhase2)) {
		c.fail(CritManifest, "rendered_inventory_mismatch")
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
	// A3.8: an event of a step the attempt stopped before is not reached,
	// not missing (always is a step every attempt reaches).
	const always = msCount
	reached := func(m milestone) bool { return m == always || !c.unreached(CritEvents, m) }
	for _, r := range []struct {
		typ string
		m   milestone
	}{{EvPreflightStarted, always}, {EvVersionsChecked, msVersions}, {EvRuntimeCreated, msRuntime}, {EvRolesRegistered, msRegistered},
		{EvSidecarsReconnected, msReconnected}, {EvRolesReady, msReady}, {EvSeedCreated, msSeed}, {EvBaselineTests, msBaselineTests},
		{EvCoordinatorReady, msCoordinator}, {EvFinalTests, msFinal}, {EvCollectionStarted, always}, {EvCleanupStarted, always},
		{EvCleanupFinished, always}, {EvRuntimeRemoved, msRuntime}, {EvAwaitingOwner, msAwaiting}, {EvOwnerDecision, msDecision},
		{EvReceiptCreated, msReceipt}} {
		if reached(r.m) && len(c.eventsOf(r.typ)) != 1 {
			c.fail(CritEvents, "required_event_missing")
		}
	}
	// Both fingerprints follow the baseline step; each started phase has
	// one start, exit and transcript capture.
	phases := boolInt(reached(msPhase1)) + boolInt(reached(msPhase2))
	for typ, n := range map[string]int{EvSnapshotTaken: 2 * boolInt(reached(msBaseline)), EvPhaseStarted: phases, EvPhaseExited: phases,
		EvTranscriptCaptured: phases} {
		if len(c.eventsOf(typ)) != n {
			c.fail(CritEvents, "required_event_missing")
		}
	}
	// The attempt's failure is the stopping cause every unreached part
	// references: the lifecycle it cut short is not reached, and it fails
	// this criterion itself only when no step was left unreached.
	if len(c.eventsOf(EvAttemptFailed)) != 0 {
		if c.stopPoint().stopped() {
			c.fail(CritEvents, codeNotReached)
		} else {
			c.fail(CritEvents, "attempt_failed")
		}
	}
	// A3 lifecycle order: the baseline fingerprint before any probe; phase
	// 1, its exit, the prompt, the decision, the receipt, phase 2 and its
	// exit; collection, cleanup, runtime removal and the final fingerprint.
	// The steps an early stop left unreached are not part of the order.
	seq := func(typ, handle, code string) int {
		for _, e := range evs {
			if e.Type == typ && (handle == "" || e.Handle == handle) && (code == "" || e.Code == code) {
				return e.Seq
			}
		}
		return 0
	}
	var order []int
	for _, o := range []struct {
		typ, handle, code string
		m                 milestone
	}{{EvPreflightStarted, "", "", always}, {EvSnapshotTaken, "", "before", msBaseline}, {EvVersionsChecked, "", "", msVersions},
		{EvCoordinatorReady, "", "", msCoordinator}, {EvPhaseStarted, phaseHandle(1), "", msPhase1}, {EvPhaseExited, phaseHandle(1), "", msPhase1Done},
		{EvAwaitingOwner, "", "", msAwaiting}, {EvOwnerDecision, "", "", msDecision}, {EvReceiptCreated, "", "", msReceipt},
		{EvPhaseStarted, phaseHandle(2), "", msPhase2}, {EvPhaseExited, phaseHandle(2), "", msPhase2Done}, {EvCollectionStarted, "", "", always},
		{EvCleanupFinished, "", "", always}, {EvSnapshotTaken, "", "after", msBaseline}} {
		if reached(o.m) {
			order = append(order, seq(o.typ, o.handle, o.code))
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] == 0 || order[i] <= order[i-1] {
			c.fail(CritEvents, "lifecycle_out_of_order")
			break
		}
	}
}

func (c *checker) roles() {
	r, want := c.b.Roles, c.configuredRoles()
	if r == nil && c.unreached(CritRoles, msReady) {
		return
	}
	if r == nil || want == nil {
		c.fail(CritRoles, "roles_missing")
		return
	}
	if len(r.Roles) != len(want) {
		c.fail(CritRoles, "roles_mismatch")
		return
	}
	nodes := map[string]string{}
	for i, s := range r.Roles {
		w := want[i]
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

// configuredRoles are the configured role/pairs: the frozen flow's, or,
// when the attempt stopped before its setup copy was written (A3.8), the
// same pairs the manifest recorded from that flow when it was loaded. Nil
// when neither is available.
func (c *checker) configuredRoles() []FlowRole {
	if f := c.flow(); f != nil {
		return f.Roles
	}
	if m := c.b.Manifest; m != nil && len(m.Roles) > 0 && !c.reached(msCoordinator) {
		return m.Roles
	}
	return nil
}

// selectionMatches reports whether a view ran its configured role's pair
// with no override.
func (c *checker) selectionMatches(v contract.TaskView, hop string) bool {
	i := slices.IndexFunc(c.configuredRoles(), func(r FlowRole) bool { return r.ID == hop })
	if i < 0 {
		return false
	}
	r := c.configuredRoles()[i]
	return v.Request.Override == nil && v.Effective.Model == r.Model && v.Effective.Effort == r.Effort && v.Role.Adapter == r.Adapter
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
			if !c.unreached(CritPreflight, msPreflight(i)) {
				c.fail(CritPreflight, "preflight_missing")
			}
			continue
		}
		v := t.View
		if v.Role.ID != h || v.WorkspaceBinding != nil || len(v.Request.Payload) == 0 || m == nil || v.Request.Payload[0] != markerFor(m.RunID, preflightHop(i)) {
			c.fail(CritPreflight, "preflight_binding_mismatch")
		}
		if !c.selectionMatches(v, h) {
			c.fail(CritPreflight, "preflight_selection_mismatch")
		}
		// A3.8: any present, non-empty, untruncated answer authenticates;
		// whether it echoed the token is information only.
		if !succeeded(t) || !authAnswerPresent(*v.Result.FinalMessage) {
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

// permissionInputs reconstructs the rendered commands' fixed values
// (A3.7) from independently bound inputs, never from a recorded argument
// list: the runtime from the launch's working directory and the reale2e
// executable from the first rendered wait template (itself bound to the
// hashed system prompt). The sanitized MCP configuration must be exactly
// the renderer's, its CA inside the runtime.
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
	reale2e, _, ok := shellWord(l.WaitTemplates[0])
	if !ok || !filepath.IsAbs(reale2e) {
		return v, false
	}
	v.reale2e = reale2e
	return v, true
}

// waitLinesMatch reports whether each reconstructed wait template is a
// whole line of the hashed system prompt (A3.7) and every Bash line of
// the prompt is one simple command, without a redirect, separator, pipe
// or quote. It does not compare generated Bash approval rules.
func waitLinesMatch(system string, v permissionValues, waits []string) bool {
	lines := strings.Split(system, "\n")
	if len(waits) != len(Hops) || slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "capture-wait") }) {
		return false
	}
	for _, w := range waits {
		if !slices.Contains(lines, w) || strings.Count(w, "TASK_ID") != 1 || !simpleBashLine(w) {
			return false
		}
	}
	bash := promptBashLines(system, v.reale2e)
	return len(bash) >= 2*len(Hops)+1 && !slices.ContainsFunc(bash, func(l string) bool { return !simpleBashLine(l) })
}

// expectedLaunch is phase p's only permitted launch: the shared auto-mode
// argv and wait templates rendered from the reconstructed fixed values,
// the MCP configuration at <runtime>/mcp.json and the rendered prompts
// (phase 2 resumes the phase-1 session). ok is false when the fixed
// values are unrecoverable or are not plain command words. An older
// allow-list launch is not this argv and is not treated as auto evidence.
func (c *checker) expectedLaunch(p int) (argv, waits []string, ok bool) {
	v, ok := c.permissionInputs(c.refLaunch())
	if !ok {
		return nil, nil, false
	}
	if err := validRuleInputs(v.reale2e, v.runtime); err != nil {
		return nil, nil, false
	}
	for i := range Hops {
		waits = append(waits, waitCommand(v, i))
	}
	prompt := string(c.b.Files["setup/coordinator-phase-"+fmt.Sprint(p)+".md"])
	system := string(c.b.Files["setup/coordinator-prompt.md"])
	argv = launchArgv(p, prompt, filepath.Join(v.runtime, "mcp.json"), system, c.derived.SessionID)
	if !strings.Contains(system, v.reale2e+" observe --run "+v.runtime+" ") || !waitLinesMatch(system, v, waits) {
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

// setup (A3, FP-8): each recorded launch has the permitted auto-mode
// shape (sandbox-off settings, --permission-mode auto,
// --permission-prompts none, no allow-list and no bypass), the rendered
// prompt bytes and one MCP configuration, the same executable, cwd and
// wait templates; phase 2 resumes the session phase 1 reported, observed
// again in phase 2's init; every coordinator init the checker uses
// reports auto, and a repeated init of that session that differs is
// session_mismatch. Each launched phase's supervisor-observed exit is 0
// with its group gone. Each phase is validated on its own: an unlaunched
// phase is only launch_missing (A3.8: not reached when the attempt
// stopped before its launch), and an exit is unobserved only for a
// launched phase. A historical allow-list launch fails launch_shape_invalid
// here; it is not read as auto-mode evidence.
func (c *checker) setup() {
	for _, f := range []string{"setup/coordinator-prompt.md", "setup/mcp-config.json", "setup/" + PhasePrompt1} {
		if len(c.b.Files[f]) == 0 && !c.unreached(CritSetup, msCoordinator) {
			c.fail(CritSetup, "setup_file_missing")
		}
	}
	sid := c.derived.SessionID
	system := string(c.b.Files["setup/coordinator-prompt.md"])
	l1, l2 := c.b.Launches[0], c.b.Launches[1]
	for p, l := range c.b.Launches {
		if l == nil {
			if !c.unreached(CritSetup, msPhase(p+1)) {
				c.fail(CritSetup, "launch_missing")
			}
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
	// Do not infer auto from the launch flags. Every recorded init,
	// including one a same-session repeat had to match, must say auto.
	for _, init := range c.derived.Init {
		if init.PermissionMode != autoModeFlag {
			c.fail(CritSetup, "launch_shape_invalid")
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
		// The exit status is the phase's own only when it exited before the
		// stop (review C1): a phase the cleanup ended has none.
		p := obs[0]
		badOwnExit := (p.ExitCode == nil || *p.ExitCode != 0 || p.Signal != "") && c.due(CritSetup, c.reached(msDone(i+1)))
		if !p.Reaped || !p.GroupGone || badOwnExit || p.PID <= 0 || p.StartedSeq != l.StartedSeq || p.ExitedSeq <= p.StartedSeq {
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

// stepDue reports whether the attempt step that produces a required entry
// ran (A3.8): a hop's wait start and consumption are their own steps, not
// its dispatch's (review C1).
func (c *checker) stepDue(s mapStep) bool {
	switch s.kind {
	case MapLaunch:
		return c.reached(msPhase1)
	case MapPause:
		return c.reached(msAwaiting)
	case MapDecision:
		return c.reached(msDecision)
	case MapResume:
		return c.reached(msPhase2)
	case MapFinalResult:
		return c.reached(msPhase2Done)
	case MapWaitStart:
		return c.waitStarted(s.hop)
	case MapWaitConsumed:
		return c.consumedDue(s.hop)
	}
	return c.reached(msHop(s.hop))
}

// eventMap (A3): the stored map is re-derived from the raw phase streams,
// transcript snapshots, launches and supervisor events and must equal the
// derivation; it is machine-derived with every source reference bound to
// a fixed bundle file's hash and line span, holds the required entries in
// order and has no finding (each finding's code fails the criterion).
func (c *checker) eventMap() {
	em, m := c.b.EventMap, c.b.Manifest
	// A3.8: without phase 1 nothing produced a session to map.
	if c.unreached(CritEventMap, msPhase1) {
		return
	}
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
	// The required entries are those of the steps the attempt reached
	// (A3.8); an early stop leaves the rest not reached, never missing.
	steps := canonicalMap()
	reached := slices.IndexFunc(steps, func(s mapStep) bool { return !c.due(CritEventMap, c.stepDue(s)) })
	if reached >= 0 {
		steps = steps[:reached]
		ordered = ordered[:min(len(ordered), len(steps))]
	}
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
			dr := c.b.Hops[s.hop].Dispatch
			if (dr != nil || c.due(CritEventMap, c.dispatchDue(s.hop))) && (dr == nil || dr.ObservedSeq != e.Seq) {
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
	// A3.8: each part of a hop's wait is due once its own step ran (review
	// C1): an admitted hop's wait may never have started, a started wait's
	// task may have ended only by the cleanup's cancellation, and the
	// coordinator may have been stopped before consuming it. A wake is due
	// once any hop's consumption is.
	consumedDue := false
	for i := range Hops {
		consumedDue = consumedDue || c.consumedDue(i)
	}
	if wakes == 0 && c.due(CritWaits, consumedDue) {
		c.fail(CritWaits, "automatic_wake_unobserved")
	}
	if slices.ContainsFunc(d.Findings, func(f MapFinding) bool { return f.Code == CodeLongPoll }) {
		c.fail(CritWaits, CodeLongPoll)
	}
	for i := range Hops {
		if c.unreached(CritWaits, msHop(i)) {
			continue
		}
		starts, consumed := len(entriesOf(d, MapWaitStart, Hops[i])), len(entriesOf(d, MapWaitConsumed, Hops[i]))
		if starts > 1 || consumed > 1 || (starts == 0 && c.due(CritWaits, c.waitStarted(i))) || (consumed == 0 && c.due(CritWaits, c.consumedDue(i))) {
			c.fail(CritWaits, "wait_unobserved")
		}
		w := c.b.Hops[i].Wait
		dir := "hops/" + hopDir(i) + "/"
		result, okR := c.b.Files[dir+waitResultFile]
		stderr := c.b.Files[dir+waitStderrFile]
		exit, okX := c.b.Files[dir+waitExitFile]
		switch {
		case !okR && !okX && !c.due(CritWaits, c.waitEnded(i)):
		case w == nil:
			c.fail(CritWaits, "wait_record_missing")
		case !okR || !okX:
			c.fail(CritWaits, "wait_files_missing")
		case w.Hop != Hops[i] || w.Handle != waitHandle(i) || w.Task == "" || w.Task != c.hopTask(i) || w.Winner != w.Task:
			c.fail(CritWaits, "wait_binding_mismatch")
		case (w.ExitCode == nil || *w.ExitCode != 0 || w.WinnerState != contract.TaskSucceeded || !hex64RE.MatchString(w.ResultSHA256)) &&
			!c.unreached(CritWaits, msTerm(i)):
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
			if !c.unreached(CritHops, msHop(i)) {
				c.fail(CritHops, "hop_missing")
			}
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
		// The outcome is due once the task ended before the stop (review
		// C1): a task the cleanup cancelled after it never had one.
		if !c.unreached(CritHops, msTerm(i)) {
			if !succeeded(he.Task) {
				c.fail(CritHops, "hop_not_succeeded")
			} else if finalMarker(h, *v.Result.FinalMessage) != approvedMarkers[h] {
				c.fail(CritHops, "final_marker_missing")
			}
			if v.Result == nil || v.Result.Workspace == nil || v.Result.Workspace.Publication != contract.PublicationPublished || v.Result.Workspace.Commit == nil {
				c.fail(CritHops, "publication_missing")
			}
		}
		if !he.HasLogs || he.LogMeta == nil {
			c.fail(CritHops, "logs_missing")
		}
		d := he.Dispatch
		if d == nil {
			if c.due(CritHops, c.dispatchDue(i)) {
				c.fail(CritHops, "dispatch_record_missing")
			}
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
	if r == nil && c.b.RepoErr == nil && c.unreached(CritChain, msSeed) {
		return
	}
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
	// A3.8: one tree per hop whose task ended before the stop.
	reached := len(Hops)
	for i := range Hops {
		if c.unreached(CritChain, msTerm(i)) {
			reached = i
			break
		}
	}
	if len(tm.Hops) != reached {
		c.fail(CritChain, "tree_manifest_incomplete")
	}
	base := m.Workspace.SeedCommit
	for i, h := range Hops {
		v, cm := c.views[i], c.commits[i]
		if cm == nil && c.unreached(CritChain, msTerm(i)) {
			return
		}
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
		if !c.unreached(CritArtifacts, msSeed) {
			c.fail(CritArtifacts, "seed_tree_missing")
		}
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
			if !c.unreached(CritArtifacts, msTerm(i)) {
				c.fail(CritArtifacts, "hop_tree_missing")
			}
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
		if !c.unreached(CritOwner, msDecision) {
			c.fail(CritOwner, "decision_missing")
		}
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
	// A3.8: the receipt and the coder's admission follow only a yes and
	// steps the attempt may have stopped before.
	noReceipt := !ok3 && c.unreached(CritOwner, msReceipt)
	if !ok1 || !ok2 || (!ok3 && !noReceipt) || !(review.Seq < waiting.Seq && waiting.Seq < dec.Seq) || (ok3 && dec.Seq >= receipt.Seq) {
		c.fail(CritOwner, "decision_out_of_order")
	}
	if ok2 && eventTime(dec).Sub(eventTime(waiting)) > OwnerDecisionBound {
		c.fail(CritOwner, "decision_deadline_exceeded")
	}
	coder, okc := c.firstEvent(EvTaskAdmitted, func(e Event) bool { return e.Hop == HopCoder })
	obs, oko := c.firstEvent(EvObserved, func(e Event) bool { return e.Hop == HopCoder })
	// The coder's admission follows the receipt; so does its observation,
	// once that is due (review C1: an admitted coder the stop kept from
	// being observed is not a premature one).
	if (okc || oko || !c.unreached(CritOwner, msHop(2))) &&
		(!okc || coder.Seq < receipt.Seq || (oko && obs.Seq < receipt.Seq) || (!oko && c.due(CritOwner, c.dispatchDue(2)))) {
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
	want := map[string]string{}
	for i := range Hops {
		if t := c.b.Preflight[i]; t != nil {
			want[t.View.TaskID] = preflightHop(i)
		}
		if id := c.hopTask(i); id != "" {
			want[id] = Hops[i]
		}
	}
	// A cancelled task outside the chain is an extra admission; the chain's
	// own tasks cancelled by a deadline watchdog or by the cleanup after a
	// stop are that stop's consequence, reported by its cause (A3.8 review
	// C1).
	if len(c.eventsOf(EvExtraAdmission)) != 0 || slices.ContainsFunc(c.eventsOf(EvTaskCancelled), func(e Event) bool { return want[e.Task] == "" }) {
		c.fail(CritAdmissions, "extra_admission")
	}
	// A3.8: exactly one admission per reached preflight and hop; the
	// tasks an early stop never dispatched are not reached.
	n := 0
	for i := range Hops {
		n += boolInt(!c.unreached(CritAdmissions, msPreflight(i))) + boolInt(!c.unreached(CritAdmissions, msHop(i)))
	}
	admitted := c.eventsOf(EvTaskAdmitted)
	if len(admitted) != n || len(want) != n {
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
		if !c.reached(msHop(i)) {
			break
		}
		adm, ok1 := c.firstEvent(EvTaskAdmitted, func(e Event) bool { return e.Hop == Hops[i] })
		prev, ok2 := c.firstEvent(EvTaskTerminal, func(e Event) bool { return e.Hop == Hops[i-1] })
		if !ok1 || !ok2 || adm.Seq < prev.Seq {
			c.fail(CritAdmissions, "admission_out_of_order")
		}
	}
}

func (c *checker) grok() {
	he := c.b.Hops[3]
	if he.Task == nil && c.unreached(CritGrok, msHop(3)) {
		return
	}
	if he.Task == nil || c.files[3] == nil {
		c.fail(CritGrok, "review_missing")
		return
	}
	v := he.Task.View
	switch {
	case c.unreached(CritGrok, msTerm(3)):
		// Review C1: the review was never given (the task ended only by
		// the cleanup's cancellation, or not at all).
	case v.Result == nil || v.Result.FinalMessage == nil || strings.TrimSpace(*v.Result.FinalMessage) == "" || v.Result.FinalMessageTruncated:
		c.fail(CritGrok, "review_missing")
	case lastNonEmptyLine(*v.Result.FinalMessage) != approvedMarkers[HopCodeReviewer]:
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
	if d == nil && !c.due(CritGrok, c.dispatchDue(3)) {
		return
	}
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

func (c *checker) testRun(t *TestRun, step milestone, commit, tree string, files map[string][]byte, missing, bad string) {
	if t == nil {
		if !c.unreached(CritFinalTests, step) {
			c.fail(CritFinalTests, missing)
		}
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
	c.testRun(c.b.Baseline, msBaselineTests, seedCommit, seedTree, c.files[0], "baseline_missing", "baseline_failed")
	commit, tree := "", ""
	if cm := c.commits[3]; cm != nil {
		commit, tree = cm.Hash.String(), cm.TreeHash.String()
	}
	c.testRun(c.b.Final, msFinal, commit, tree, c.files[4], "final_tests_missing", "final_tests_failed")
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
	if !cl.RuntimeRemoved && !c.unreached(CritCleanup, msRuntime) {
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
	if cs == nil && rq == nil && c.unreached(CritFinalTests, msFinal) {
		return
	}
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
	case cs == nil && c.unreached(CritResult, msFinal):
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
		if !c.unreached(CritResult, msFinal) {
			c.fail(CritResult, CodeTestMapping)
		}
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
	// A row unobserved only because the attempt stopped before its step is
	// not reached (A3.8); every other row short of correct fails. When the
	// attempt stopped before this criterion's own validation ran, a row
	// whose shortfall a criterion it cites already reports is not restated
	// here: the early stop reports only its real cause, once.
	var reported map[string][]string
	if !c.reached(msFinal) {
		reported = c.otherCodes()
	}
	for _, r := range auditRows(c) {
		switch {
		case r.Layer != LayerMechanical || r.Verdict == VerdictCorrect:
		case r.notReached || rowReported(r, reported):
			c.fail(CritResult, codeNotReached)
		default:
			c.fail(CritResult, "audit_row_not_correct")
		}
	}
}

// otherCodes are the real failure codes of every criterion but this one,
// by criterion.
func (c *checker) otherCodes() map[string][]string {
	self := reflect.ValueOf((*checker).resultCorrectness).Pointer()
	out := map[string][]string{}
	for _, f := range criterionMethods {
		if reflect.ValueOf(f).Pointer() == self {
			continue
		}
		for crit, codes := range c.methodCodes(f) {
			out[crit] = append(out[crit], slices.DeleteFunc(slices.Clone(codes), func(s string) bool { return s == codeNotReached })...)
		}
	}
	return out
}

// rowReported reports whether a row's shortfall is already reported: a
// criterion it cites fails, and each contradiction code it names is a
// failure code of one of those criteria (nil reported: never).
func rowReported(r AuditRow, reported map[string][]string) bool {
	var codes []string
	for _, x := range r.Criteria {
		codes = append(codes, reported[x]...)
	}
	if len(codes) == 0 {
		return false
	}
	for _, x := range r.Criteria {
		if code, ok := strings.CutPrefix(x, "code:"); ok && !slices.Contains(codes, code) {
			return false
		}
	}
	return true
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
	in := envCheckInput{files: c.b.Files, diff: c.b.Diff, notReached: func() bool { return c.unreached(CritOwnerEnv, msBaseline) }}
	if m := c.b.Manifest; m != nil {
		in.manifest, in.roots, in.depth = true, m.ProtectedRoots, m.ProtectDepth
	}
	checkOwnerEnvironment(in, func(code string) { c.fail(CritOwnerEnv, code) })
}

// envCheckInput is one owner-environment check's evidence: the manifest's
// supplied roots and depth (manifest false: no manifest), the bundle's
// files (snapshot shards by their fixed names), the stored diff and
// notReached, which reports (and records) that the attempt stopped before
// its baseline fingerprint.
type envCheckInput struct {
	manifest   bool
	roots      []string
	depth      int
	files      map[string][]byte
	diff       *EnvDiff
	notReached func() bool
	// policy is the ownership policy the classes are derived under (the
	// development checkers' own, never the bundle's).
	policy envPolicy
}

// checkOwnerEnvironment is the owner-environment criterion over captured
// evidence alone (shared by the legacy and the development checkers): it
// recomputes the diff from the two shard sets and compares it with the
// stored one, failing every protected change.
func checkOwnerEnvironment(in envCheckInput, fail func(code string)) {
	roots, d := in.roots, in.diff
	shardsOf := func(phase string) ([][]byte, bool) {
		var out [][]byte
		for _, name := range envFiles(phase) {
			data, ok := in.files[name]
			if !ok {
				return nil, false
			}
			out = append(out, data)
		}
		return out, true
	}
	var snaps [2]*Snapshot
	for i, phase := range []string{"before", "after"} {
		shards, ok := shardsOf(phase)
		if !ok {
			if !in.notReached() {
				fail(CodeEnvUnobserved)
			}
			continue
		}
		s, err := parseSnapshot(shards)
		if err != nil || s.Phase != phase {
			fail("snapshot_invalid")
			continue
		}
		// A complete claim needs the vendor homes and supplied roots
		// observed, with every record of a valid kind, scope and path.
		if err := checkInventoryFor(in.policy, s, roots); err != nil {
			fail(CodeEnvInventory)
			fail(CodeEnvUnobserved)
			continue
		}
		snaps[i] = s
	}
	for i, a := range roots {
		for _, b := range roots[i+1:] {
			if a == b || tokenStrictlyUnder(a, b) || tokenStrictlyUnder(b, a) {
				fail("protected_roots_invalid")
			}
		}
	}
	for _, s := range snaps {
		if s != nil && (!in.manifest || !slices.Equal(s.Roots, roots)) {
			fail("protected_roots_mismatch")
		}
		// A3.2: both snapshots discovered at the manifest's depth.
		if s != nil && (!in.manifest || s.Depth != in.depth || in.depth < 0 || in.depth > maxProtectDepth) {
			fail("protect_depth_mismatch")
		}
	}
	if d == nil {
		if !in.notReached() {
			fail("environment_diff_missing")
		}
		return
	}
	if !in.manifest || !slices.Equal(d.ProtectedRoots, roots) {
		fail("protected_roots_mismatch")
	}
	if snaps[0] != nil && snaps[1] != nil {
		// A3.4: the stored snapshots are compact; the diff's detail must
		// list exactly the entries whose compact records differ, each full
		// record hashing to its snapshot's, and every change is re-derived
		// from that bound detail.
		if err := verifyDetailFor(in.policy, snaps[0], snaps[1], d.Detail); err != nil {
			fail("environment_diff_mismatch")
			fail(CodeEnvUnobserved)
			snaps[0] = nil
		}
	}
	if snaps[0] != nil && snaps[1] != nil {
		want := diffFromDetailFor(in.policy, snaps[0], snaps[1], roots, d.Detail)
		for _, phase := range []string{"before", "after"} {
			for _, name := range envFiles(phase) {
				fh := FileHash{Name: name, SHA256: sha256Hex(in.files[name])}
				if phase == "before" {
					want.Before = append(want.Before, fh)
				} else {
					want.After = append(want.After, fh)
				}
			}
		}
		if !bytes.Equal(mustJSON(want), mustJSON(*d)) {
			fail("environment_diff_mismatch")
		}
		for _, code := range want.Codes {
			fail(code)
		}
	}
	for _, code := range d.Codes {
		fail(code)
	}
	if !d.Complete {
		fail(CodeEnvUnobserved)
	}
	if len(d.Protected) > 0 {
		fail(CodeEnvModified)
	}
}
