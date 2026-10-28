package reale2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Independent Cursor evidence checking (design 12a-3-cursor-worker, slice
// 3b FP-1 and FP-2). Cursor evidence carries explicit outer format
// discriminators instead of broadening the historical v2 admission: a
// fresh Cursor qualification probe writes callsheet-dev-cursor-
// qualification/v1, a Cursor full or light attempt callsheet-dev-cursor-
// evidence/v1 and a Cursor combined exercise callsheet-dev-cursor-
// exercise/v1. Their nested records keep the callsheet-dev-<kind>/v2
// envelopes and validators; default writers keep their v2 formats. Every
// fact is reconstructed from primary records before a stored summary is
// compared: the frozen selection and method sources, the platform, the
// version list and the harness and vendor executable bindings, the five
// preflights, role readiness, the four sidecars' lifecycle, the coder
// fixture, request, push, task, publication and independent validation,
// and the report-only shallow samples. No checker executes Git, tests,
// helpers or a vendor, consults installed configuration or opens a path
// recorded in the evidence (those are labels). Checksums are internal
// consistency, not signatures.

// Cursor formats.
const (
	DevCursorQualificationSchema       = "callsheet-dev-cursor-qualification/v1"
	DevCursorBundleSchema              = "callsheet-dev-cursor-evidence/v1"
	DevCursorExerciseSchema            = "callsheet-dev-cursor-exercise/v1"
	devCursorReportSchema              = "callsheet-dev-cursor-report/v1"
	devCursorAuditSchema               = "callsheet-dev-cursor-audit/v1"
	devCursorExerciseReportSchema      = "callsheet-dev-cursor-exercise-report/v1"
	devCursorExerciseAuditSchema       = "callsheet-dev-cursor-exercise-audit/v1"
	devCursorQualificationReportSchema = "callsheet-dev-cursor-qualification-report/v1"
)

// Cursor evidence codes (stable; existing codes keep their meaning where
// their checks apply).
const (
	CodeCursorSelectionMismatch   = "cursor_selection_mismatch"
	CodeCursorExecutableMismatch  = "cursor_executable_mismatch"
	CodeCursorNodesMismatch       = "cursor_nodes_mismatch"
	CodeCursorCoderMissing        = "cursor_coder_missing"
	CodeCursorFixtureMismatch     = "cursor_fixture_mismatch"
	CodeCursorRequestMismatch     = "cursor_request_mismatch"
	CodeCursorPushMismatch        = "cursor_push_mismatch"
	CodeCursorTaskMismatch        = "cursor_task_mismatch"
	CodeCursorPublicationMismatch = "cursor_publication_mismatch"
	CodeCursorValidationMismatch  = "cursor_validation_mismatch"
	CodeCursorActivityMissing     = "cursor_activity_missing"
	CodeCursorActivityMismatch    = "cursor_activity_mismatch"
)

// DevCursorBinding is the explicit Cursor executable's binding: its
// tokenized path (the Cursor version record's path) and its SHA-256
// before any vendor execution and after cleanup. It is never the
// harness's DevExecutableRecord. Since design 12a-3c-cursor-binding the
// writer stores the frozen symlink-resolved target as the path and leaves
// the after hash empty when that target changed, became unreadable or an
// earlier binding failure was latched; older writers stored the unresolved
// --cursor path, an inert label either way.
type DevCursorBinding struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
}

// DevCursorQualification is a Cursor qualification record's cursor
// object: the binding, the coder workspace and its task and commits.
type DevCursorQualification struct {
	Binding      DevCursorBinding `json:"binding"`
	Workspace    WorkspaceRecord  `json:"workspace"`
	Task         string           `json:"task"`
	InputCommit  string           `json:"input_commit"`
	ResultCommit string           `json:"result_commit"`
}

// DevCursorQualificationReport is the offline check's (and the live
// collector's) Cursor qualification report: qualification mechanics only,
// never a workflow PASS or a semantic judgment.
type DevCursorQualificationReport struct {
	Schema   string                `json:"schema"`
	RunID    string                `json:"run_id"`
	Result   string                `json:"result"`
	Scope    string                `json:"scope"`
	Codes    []string              `json:"codes"`
	Activity CursorActivitySummary `json:"activity"`
	Error    string                `json:"error"`
}

// cursorShippedCoder is the one Cursor coder row a Cursor format admits.
var cursorShippedCoder = FlowRole{ID: HopCoder, Adapter: adapter.CursorID, Model: "grok-4.7", Effort: "high"}

// cursorSelectionOK reports a flow that is exactly the default five-role
// selection with only its coder row replaced by the shipped Cursor pair.
func cursorSelectionOK(f DevFullFlow) bool {
	def := DefaultDevFullFlow()
	if f.Schema != DevFullFlowSchema || len(f.Roles) != len(def.Roles) || f.Validate() != nil {
		return false
	}
	for i, r := range f.Roles {
		want := def.Roles[i]
		if want.ID == HopCoder {
			want = cursorShippedCoder
		}
		if r != want {
			return false
		}
	}
	return true
}

// rolesSelectCursor reports a role table whose coder selects Cursor.
func rolesSelectCursor(roles []FlowRole) bool {
	return slices.ContainsFunc(roles, func(r FlowRole) bool { return r.Adapter == adapter.CursorID })
}

// checkCursorField enforces the cursor object's presence at raw-object
// decoding (a pointer alone cannot tell missing from null): a Cursor
// format requires a non-null cursor object; a historical or default format
// never carries the key, even null.
func checkCursorField(data []byte, want bool) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	v, present := raw["cursor"]
	switch {
	case want && (!present || string(bytes.TrimSpace(v)) == "null"):
		return errors.New("a Cursor format requires a non-null cursor object")
	case !want && present:
		return errors.New("this format has no cursor field")
	}
	return nil
}

// cursorFieldFile reports a record whose outer format decides the cursor
// field: the manifest and the qualification record.
func cursorFieldFile(rel string) bool { return rel == devManifestFile || rel == devQualificationFile }

// ---- Layouts ----

// The Cursor probe's coder records by base name, typed for admission.
var probeCoderDocRE = regexp.MustCompile(`^probe/coder/01/(request|fixture|push-request|push-response|logs|publication|verdict)\.json$`)

// cursorRequestDoc is request.json's wire form (contract.DispatchRequest's
// canonical encoding, decoded strictly).
type cursorRequestDoc struct {
	Target            contract.TaskTarget    `json:"target"`
	Goal              string                 `json:"goal"`
	Payload           []string               `json:"payload"`
	Acceptance        string                 `json:"acceptance"`
	RequestedBy       contract.RequestedBy   `json:"requested_by"`
	Override          *contract.TaskOverride `json:"override,omitempty"`
	Workspace         *string                `json:"workspace,omitempty"`
	Base              *string                `json:"base,omitempty"`
	WorkspaceInstance *string                `json:"workspace_instance,omitempty"`
}

// cursorDocType is a Cursor record's strict type and schema at rel (nil
// when rel is not one of the Cursor records); other records keep the base
// layout's dispatch.
func cursorDocType(rel string) (any, string) {
	switch {
	case slices.Contains(cursorActivityFiles, rel):
		return &ActivityAggregate{}, ActivitySchema
	case strings.HasPrefix(rel, probeCoderDir+"/validation/"):
		return devDocTypeV(devBundleV2, scenarioFull, "validation/"+filepath.Base(rel))
	}
	m := probeCoderDocRE.FindStringSubmatch(rel)
	if m == nil {
		return nil, ""
	}
	switch m[1] {
	case "request":
		return &cursorRequestDoc{}, ""
	case "fixture":
		return &devEnvelope[cursorCoderFixtureRecord]{}, devKind("probe-coder-fixture")
	case "push-request":
		return &cursorPushRecord{}, ""
	case "push-response":
		return &cursorPushResponse{}, ""
	case "logs":
		return &DevLogRecord{}, devKind("logs")
	case "publication":
		return &devEnvelope[cursorPublicationRecord]{}, devKind("probe-coder-publication")
	}
	return &devEnvelope[cursorCoderVerdict]{}, devKind("probe-coder-verdict")
}

// cursorEvidenceLayout is the exact Cursor qualification layout of scope:
// the v2 probe layout of the scope plus the slice 3a coder records,
// validation records, inert captures, coder repository and the two
// shallow samples, with an actual typed document for every added JSON
// file and the Cursor qualification schema. No diagnostic receipt belongs
// to it.
func cursorEvidenceLayout(scope string) devLayout {
	l := cursorProbeLayout(scope)
	l.cursor = true
	l.doc = func(rel string) (any, string) {
		if rel == devQualificationFile {
			return &DevQualification{}, DevCursorQualificationSchema
		}
		if v, schema := cursorDocType(rel); v != nil {
			return v, schema
		}
		return probeDocTypeV(devBundleV2, rel)
	}
	return l
}

// cursorAttemptLayout is a Cursor full or light attempt's layout: the v2
// attempt layout of scenario plus the two shallow samples only, under the
// Cursor outer manifest, report and audit schemas.
func cursorAttemptLayout(scenario string) devLayout {
	l := devLayoutFor(devBundleV2, scenario)
	l.cursor = true
	file := l.file
	l.file = func(rel string) bool { return slices.Contains(cursorActivityFiles, rel) || file(rel) }
	l.doc = func(rel string) (any, string) {
		switch {
		case rel == devManifestFile:
			return &DevManifest{}, DevCursorBundleSchema
		case rel == "report.json":
			return &DevReport{}, devCursorReportSchema
		case rel == "audit.json":
			return &DevAudit{}, devCursorAuditSchema
		case slices.Contains(cursorActivityFiles, rel):
			return &ActivityAggregate{}, ActivitySchema
		}
		return devDocTypeV(devBundleV2, scenario, rel)
	}
	return l
}

func (m *ActivityAggregate) schemaID() string { return m.Schema }
func (m *ActivityAggregate) runIDOf() string  { return m.RunID }

// reportSchema and auditSchema are the bundle's derived schemas.
func (b *DevBundle) reportSchema() string {
	if b.cursor {
		return devCursorReportSchema
	}
	return b.kind("report")
}

func (b *DevBundle) auditSchema() string {
	if b.cursor {
		return devCursorAuditSchema
	}
	return b.kind("audit")
}

// loadCursorQualification loads a Cursor qualification directory (its
// record's scope already read) under the strict Cursor layout.
func loadCursorQualification(root, scope string) (*DevBundle, error) {
	return loadDevLayout(root, cursorEvidenceLayout(scope))
}

// ---- Qualification report and command ----

// cursorQualificationReport is the pure Cursor qualification result of a
// loaded bundle: QUALIFIED exactly when the record holds no contradiction,
// the codes in check order, the recomputed activity summary.
func cursorQualificationReport(b *DevBundle) DevCursorQualificationReport {
	rep := DevCursorQualificationReport{Schema: devCursorQualificationReportSchema, Result: ResultBlocked, Codes: []string{},
		Activity: CursorActivitySummary{Changes: []ActivityChange{}}}
	q := doc[DevQualification](b, devQualificationFile)
	if q != nil {
		if ValidRunID(q.RunID) {
			rep.RunID = q.RunID
		}
		if probeScopeOK(q.Scope) {
			rep.Scope = q.Scope
		}
		rep.Activity = cursorActivityOf(b, q.RunID).summary
	}
	if !b.cursor {
		// Only the recognized Cursor format yields a Cursor qualification.
		rep.Codes = append(rep.Codes, CodeCursorSelectionMismatch)
		return rep
	}
	rep.Codes = append(rep.Codes, CheckDevQualification(b)...)
	if len(rep.Codes) == 0 {
		rep.Result = ResultQualified
	}
	return rep
}

// cursorQualificationReportAt loads root strictly (the diagnostic refusal
// first) and checks it: the offline command's and the live collector's
// one result. A malformed, unsafe or unsupported directory is BLOCKED with
// the schema error.
func cursorQualificationReportAt(root string) DevCursorQualificationReport {
	b, err := LoadDevQualification(root)
	if err != nil {
		return DevCursorQualificationReport{Schema: devCursorQualificationReportSchema, Result: ResultBlocked, Codes: []string{},
			Activity: CursorActivitySummary{Changes: []ActivityChange{}}, Error: "evidence_schema_error"}
	}
	return cursorQualificationReport(b)
}

// checkCursorQualificationEvidence is reale2e check's Cursor qualification
// route: the report on out, exit 0 only when checked QUALIFIED, 1 for a
// blocked, incomplete or contradicted record, 2 for malformed, unsafe or
// unsupported evidence. It writes nothing and executes nothing.
func checkCursorQualificationEvidence(root string, out, errOut io.Writer) int {
	b, err := LoadDevQualification(root)
	if err != nil {
		data, _ := encodeJSON(DevCursorQualificationReport{Schema: devCursorQualificationReportSchema, Result: ResultBlocked, Codes: []string{},
			Activity: CursorActivitySummary{Changes: []ActivityChange{}}, Error: "evidence_schema_error"})
		out.Write(data)
		fmt.Fprintf(errOut, "reale2e check: %v\n", err)
		return 2
	}
	rep := cursorQualificationReport(b)
	data, _ := encodeJSON(rep)
	out.Write(data)
	if rep.Result == ResultQualified {
		return 0
	}
	return 1
}

// cursorQualificationExit prints a Cursor probe's checked result: exit 0
// only for a record its independent check qualified. A blocked probe
// names its primary live cause first (the stop, a protected change or
// incomplete cleanup), then the independent check's codes.
func (d *devSupervisor) cursorQualificationExit() int {
	rep := d.probe.cursor.report
	if d.passed && rep != nil && rep.Result == ResultQualified {
		fmt.Fprintln(d.s.stdout, "qualification probe: "+ResultQualified)
		return 0
	}
	cause := d.stopCode
	switch {
	case cause != "":
	case d.protected:
		cause = CodeEnvModified
	case !d.cleanupOK:
		cause = "cleanup_incomplete"
	case rep == nil:
		cause = "evidence_collection_failed"
	default:
		cause = cmpOr(firstCode(rep.Codes), cmpOr(rep.Error, "not_qualified"))
	}
	fmt.Fprintf(d.s.stdout, "qualification probe: %s (%s)\n", ResultBlocked, cause)
	if rep != nil {
		fmt.Fprintf(d.s.stdout, "independent check: %s (%s)\n", rep.Result, cmpOr(strings.Join(rep.Codes, ", "), rep.Error))
	}
	return 1
}

// firstCode is the first code ("" none).
func firstCode(codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	return codes[0]
}

// ---- Shared reconstruction ----

// cursorCodes accumulates deduplicated codes in check order.
type cursorCodes struct{ codes []string }

func (c *cursorCodes) bad(code string) {
	if !slices.Contains(c.codes, code) {
		c.codes = append(c.codes, code)
	}
}

// cursorVersionTools are a Cursor run's version records in order.
var cursorVersionTools = []string{"claude", "codex", "grok", "cursor", "go", "git"}

// probeFlowSelectsCursor reports a qualification bundle whose captured
// selected flow selects a Cursor coder.
func probeFlowSelectsCursor(b *DevBundle) bool {
	f, err := ParseDevFullFlow(b.Files["setup/flow.json"])
	return err == nil && f.CursorCoder()
}

// cursorBindingOK reports an unchanged, complete vendor binding at the
// Cursor version record's path.
func cursorBindingOK(bind DevCursorBinding, versionPath string) bool {
	return bind.Path != "" && bind.Path == versionPath && hex64RE.MatchString(bind.BeforeSHA256) && bind.BeforeSHA256 == bind.AfterSHA256
}

// cursorVersionPath is the Cursor version record's path ("" none).
func cursorVersionPath(versions []VersionRecord) string {
	for _, v := range versions {
		if v.Tool == adapter.CursorID {
			return v.Path
		}
	}
	return ""
}

// cursorVersionCodes are a Cursor run's version-list contradictions:
// exactly the six tools in order, each with a recorded version and the
// live verdict eligible, and each vendor's recorded version line itself
// eligible under the existing adapter minimum-version policy, re-derived
// offline (nothing is executed; the stored verdict alone is never
// trusted).
func cursorVersionCodes(versions []VersionRecord) []string {
	var cc cursorCodes
	var tools []string
	for _, v := range versions {
		tools = append(tools, v.Tool)
		if !v.Eligible || strings.TrimSpace(v.Version) == "" {
			cc.bad("version_not_eligible")
		}
		if slices.Contains(cursorVersionVendors, v.Tool) && adapter.CheckRecordedVersion(v.Tool, v.Version) != nil {
			cc.bad("version_not_eligible")
		}
	}
	if !slices.Equal(tools, cursorVersionTools) {
		cc.bad("versions_incomplete")
	}
	return cc.codes
}

// cursorRoleCodes reconstructs role registration and readiness from
// setup/roles.json and the events: the frozen flow's five roles, ready,
// one node per adapter, the coder alone on the Cursor node, and exactly
// one registration, reconnection and readiness event.
func cursorRoleCodes(b *DevBundle, flow DevFullFlow, flowOK bool) []string {
	var cc cursorCodes
	r := doc[RolesRecord](b, "setup/roles.json")
	if r == nil {
		return []string{"roles_missing"}
	}
	if !flowOK || len(r.Roles) != len(flow.Roles) {
		return []string{"roles_mismatch"}
	}
	nodes, owners := map[string]string{}, map[string]string{}
	for i, s := range r.Roles {
		w := flow.Roles[i]
		if s.ID != w.ID || s.Adapter != w.Adapter || s.Model != w.Model || s.Effort != w.Effort || s.Concurrency != RoleConcurrency ||
			s.Timeout != FeatureTaskBound.String() || !contract.ValidNodeID(s.Node) {
			cc.bad("roles_mismatch")
		}
		if !s.CanAccept {
			cc.bad("role_not_ready")
		}
		if n, ok := nodes[s.Adapter]; ok && n != s.Node {
			cc.bad("adapter_nodes_inconsistent")
		}
		if a, ok := owners[s.Node]; ok && a != s.Adapter {
			cc.bad("adapter_nodes_inconsistent")
		}
		nodes[s.Adapter], owners[s.Node] = s.Node, s.Adapter
	}
	// The coder is the only role on the Cursor adapter's node.
	cursorNode, coderNode := nodes[adapter.CursorID], ""
	for _, s := range r.Roles {
		if s.ID == HopCoder {
			coderNode = s.Node
		}
		if s.ID != HopCoder && s.Node == cursorNode {
			cc.bad(CodeCursorNodesMismatch)
		}
	}
	if cursorNode == "" || coderNode != cursorNode {
		cc.bad(CodeCursorNodesMismatch)
	}
	// Every authoritative task view (the five preflights, the coder and
	// every workflow or probe task) ran on its role's registered node and
	// adapter: the coder's preflight and tasks on the Cursor node, ready
	// with the cursor adapter, which only a node running the Cursor sidecar
	// can accept.
	registered := map[string]RoleSnapshot{}
	for _, s := range r.Roles {
		registered[s.ID] = s
	}
	coderViews := 0
	for rel, d := range b.docs {
		v, ok := d.(*contract.TaskShowResponse)
		if !ok || !strings.HasSuffix(rel, "/task.json") {
			continue
		}
		s, ok := registered[v.Task.Role.ID]
		if !ok || v.Task.Role.Node != s.Node || v.Task.Role.Adapter != s.Adapter {
			cc.bad(CodeCursorNodesMismatch)
		}
		if v.Task.Role.ID == HopCoder {
			coderViews++
			if v.Task.Role.Node != cursorNode || v.Task.Role.Adapter != adapter.CursorID {
				cc.bad(CodeCursorNodesMismatch)
			}
		}
	}
	if coderViews == 0 {
		// At least the coder's preflight ran on the Cursor node.
		cc.bad(CodeCursorNodesMismatch)
	}
	for _, typ := range []string{EvRolesRegistered, EvSidecarsReconnected, EvRolesReady} {
		if n := countEvents(b, func(e Event) bool { return e.Type == typ }); n != 1 {
			cc.bad("required_event_missing")
		}
	}
	if e, ok := firstEvent(b, func(e Event) bool { return e.Type == EvRolesReady }); !ok || e.Time != r.ReadyAt {
		cc.bad("ready_at_mismatch")
	}
	return cc.codes
}

// countEvents counts b's events matching pred.
func countEvents(b *DevBundle, pred func(Event) bool) int {
	n := 0
	for _, e := range b.Events {
		if pred(e) {
			n++
		}
	}
	return n
}

// firstEvent is b's first event matching pred.
func firstEvent(b *DevBundle, pred func(Event) bool) (Event, bool) {
	for _, e := range b.Events {
		if pred(e) {
			return e, true
		}
	}
	return Event{}, false
}

// cursorSidecarsOK reconciles the four sidecars' lifecycle from the
// events with cleanup.json: deployment in the order codex, claude, grok,
// cursor (the idle Grok node included), the reconnect stop and start of
// each, in that order, between role registration and reconnection, no
// other sidecar handle, and every sidecar lifetime ended by its own stop
// event and recorded at cleanup by its handle, PID, start sequence and stop
// method with its exit verified. Several lifetimes per sidecar are
// expected; all must be cleaned up.
func cursorSidecarsOK(b *DevBundle) bool {
	cl := doc[devEnvelope[DevCleanup]](b, devCleanupFile)
	if cl == nil {
		return false
	}
	want := make([]string, 0, len(cursorSidecars))
	for _, a := range cursorSidecars {
		want = append(want, "sidecar-"+a)
	}
	registered, _ := firstEvent(b, func(e Event) bool { return e.Type == EvRolesRegistered })
	reconnected, _ := firstEvent(b, func(e Event) bool { return e.Type == EvSidecarsReconnected })
	var first, cycle []string
	starts := map[int]Event{}
	for _, e := range b.Events {
		if !strings.HasPrefix(e.Handle, "sidecar-") || (e.Type != EvProcessStarted && e.Type != EvProcessStopped) {
			continue
		}
		if !slices.Contains(want, e.Handle) {
			return false
		}
		if e.Type == EvProcessStarted {
			starts[e.Seq] = e
			if !slices.Contains(first, e.Handle) {
				first = append(first, e.Handle)
			}
		}
		if e.Seq > registered.Seq && e.Seq < reconnected.Seq {
			cycle = append(cycle, e.Type+" "+e.Handle)
		}
	}
	var wantCycle []string
	for _, h := range want {
		wantCycle = append(wantCycle, EvProcessStopped+" "+h, EvProcessStarted+" "+h)
	}
	if !slices.Equal(first, want) || registered.Seq == 0 || reconnected.Seq <= registered.Seq || !slices.Equal(cycle, wantCycle) {
		return false
	}
	// Each sidecar lifetime, walked from its own start and stop events
	// (a stop matched to its handle's active start, never a cleanup
	// summary), ends in its own stop and reconciles with its cleanup
	// record, stop method included; a stop event without an active start
	// of its handle (a renamed final stop) refuses.
	lifetimes, ok := processLifetimes(b.Events)
	if !ok {
		return false
	}
	var sidecar []procLifetime
	for _, l := range lifetimes {
		if _, isSidecar := starts[l.start.Seq]; isSidecar {
			sidecar = append(sidecar, l)
		}
	}
	return len(sidecar) == len(starts) && len(lifetimeCodes(sidecar, cl)) == 0
}

// procLifetime is one owned process lifetime: its start event and the
// process_stopped event that ended it (nil: never stopped).
type procLifetime struct {
	start Event
	stop  *Event
}

// processLifetimes walks the event log's process lifetimes in order: a
// process_started or phase_started opens its handle's lifetime, and a
// process_stopped closes that handle's active lifetime. ok is false for a
// start while the same handle is active or a stop without an active start.
func processLifetimes(evs []Event) ([]procLifetime, bool) {
	var out []procLifetime
	active := map[string]int{}
	ok := true
	for _, e := range evs {
		switch e.Type {
		case EvProcessStarted, EvPhaseStarted:
			if _, busy := active[e.Handle]; busy {
				ok = false
			}
			active[e.Handle] = len(out)
			out = append(out, procLifetime{start: e})
		case EvProcessStopped:
			i, has := active[e.Handle]
			if !has {
				ok = false
				continue
			}
			stop := e
			out[i].stop = &stop
			delete(active, e.Handle)
		}
	}
	return out, ok
}

// lifetimeCodes reconciles lifetimes with cleanup.json: every lifetime
// stopped by its own process_stopped event (final termination), recorded
// at cleanup by its handle, PID and start sequence, its stop method that
// event's, its exit verified; and cleanup's started list exactly the
// started events.
func lifetimeCodes(lifetimes []procLifetime, cl *devEnvelope[DevCleanup]) []string {
	var cc cursorCodes
	if cl == nil {
		return []string{"cleanup_incomplete"}
	}
	recs := map[int]ProcRecord{}
	for _, p := range cl.Data.Processes {
		if _, dup := recs[p.StartedSeq]; dup {
			cc.bad("process_record_conflict")
		}
		recs[p.StartedSeq] = p
	}
	started := map[int]StartedProc{}
	for _, s := range cl.Data.Started {
		started[s.StartedSeq] = s
	}
	for _, l := range lifetimes {
		p, ok := recs[l.start.Seq]
		s, recorded := started[l.start.Seq]
		switch {
		case !ok:
			cc.bad("process_record_missing")
		case p.Name != l.start.Handle || p.PID != l.start.PID || !recorded || s.Name != l.start.Handle || s.PID != l.start.PID:
			cc.bad("process_record_conflict")
		case l.stop == nil || p.StoppedBy != l.stop.Code:
			cc.bad("process_record_conflict")
		case !p.ExitVerified:
			cc.bad("process_exit_unverified")
		}
	}
	return cc.codes
}

// startedProcessCodes requires every owned process and coordinator phase
// lifetime of the events to be closed by its own stop event and recorded
// at cleanup, stop method and verified exit included.
func startedProcessCodes(b *DevBundle) []string {
	lifetimes, ok := processLifetimes(b.Events)
	codes := lifetimeCodes(lifetimes, doc[devEnvelope[DevCleanup]](b, devCleanupFile))
	if !ok && !slices.Contains(codes, "process_record_conflict") {
		codes = append(codes, "process_record_conflict")
	}
	return codes
}

// cursorPreflightCodes reconstructs the five authentication preflights
// from their own authoritative task views and admission events: no
// workspace or override, the frozen role's exact effective pair, a
// complete nonempty answer within 2m, the run-bound marker, the stored
// observation and final text, unique tasks, each admitted before the
// coder stage and the first coordinator phase.
func cursorPreflightCodes(b *DevBundle, flow DevFullFlow, flowOK bool, runID string) []string {
	var cc cursorCodes
	coderSeq := 0
	if e, ok := firstEvent(b, func(e Event) bool { return e.Type == EvTaskAdmitted && e.Hop == probeCoderLabel }); ok {
		coderSeq = e.Seq
	}
	phaseSeq := 0
	if e, ok := firstEvent(b, func(e Event) bool { return e.Type == EvPhaseStarted }); ok {
		phaseSeq = e.Seq
	}
	ids := map[string]bool{}
	for i := range len(DevFullRoleIDs) {
		label, dir := preflightHop(i), "preflight/"+hopDir(i)
		v := doc[contract.TaskShowResponse](b, dir+"/task.json")
		obs := doc[devEnvelope[DevPreflightRecord]](b, dir+"/observation.json")
		if v == nil || obs == nil {
			cc.bad("preflight_missing")
			continue
		}
		role := FlowRole{}
		if flowOK {
			role = flow.Roles[i]
		}
		if err := preflightVerdict(label, role, v.Task); err != nil {
			cc.bad(codeOf(err))
		}
		if len(v.Task.Request.Payload) != 1 || v.Task.Request.Payload[0] != markerFor(runID, label) || v.Task.Request.Goal != preflightGoal {
			cc.bad("preflight_binding_mismatch")
		}
		want := DevPreflightRecord{Label: label, Role: role.ID, Task: v.Task.TaskID, Adapter: v.Task.Role.Adapter, Model: v.Task.Effective.Model,
			Effort: v.Task.Effective.Effort, State: v.Task.State}
		if r := v.Task.Result; r != nil && r.FinalMessage != nil && !r.FinalMessageTruncated {
			want.Answered, want.TokenEchoed = authAnswerPresent(*r.FinalMessage), authAnswerOK(*r.FinalMessage)
			if got, ok := b.Files[dir+"/final.txt"]; !ok || string(got) != *r.FinalMessage {
				cc.bad("final_text_mismatch")
			}
		}
		if obs.Data != want {
			cc.bad("preflight_record_mismatch")
		}
		if d, ok := duration(v.Task); !ok || d >= PreflightBound {
			cc.bad("preflight_deadline_exceeded")
		}
		adm, _ := firstEvent(b, func(e Event) bool { return e.Type == EvTaskAdmitted && e.Hop == label && e.Task == v.Task.TaskID })
		if adm.Seq == 0 || coderSeq > 0 && adm.Seq > coderSeq || phaseSeq > 0 && adm.Seq > phaseSeq {
			cc.bad("preflight_out_of_order")
		}
		if ids[v.Task.TaskID] {
			cc.bad("task_not_unique")
		}
		ids[v.Task.TaskID] = true
	}
	return cc.codes
}

// cursorProvenanceCodes reconstructs the frozen selection and the method
// provenance of a probe record from its captures: the exact shipped
// coder-only Cursor selection (the four other rows the unchanged default),
// labelled override-flow with its canonical selection digest; the fifteen
// captured method sources with their records, the captured default
// template flow as the default selection (separate from the selected
// setup/flow.json), the triage manual budget, the installed manuals and
// the generated feature request.
func cursorProvenanceCodes(b *DevBundle, roles []FlowRole, flowLabel, selectionSHA string) (DevFullFlow, bool, []string) {
	var cc cursorCodes
	flowRaw, hasFlow := b.Files["setup/flow.json"]
	flow, ferr := ParseDevFullFlow(flowRaw)
	flowOK := hasFlow && ferr == nil
	if !flowOK || !cursorSelectionOK(flow) || !slices.Equal(roles, flow.Roles) || flowLabel != FlowOverride {
		cc.bad(CodeCursorSelectionMismatch)
	}
	sel, serr := flow.SelectionCanonical()
	label := doc[DevFlowLabel](b, "setup/flow-label.json")
	if !flowOK || label == nil || serr != nil || label.Label != FlowOverride || label.Label != flowLabel || label.FlowSHA256 != sha256Hex(flowRaw) ||
		label.SelectionSHA256 != sha256Hex(sel) || label.Selection != string(sel) || selectionSHA != label.SelectionSHA256 {
		cc.bad("flow_label_mismatch")
	}
	loaded, want, missing := capturedMethodSources(b, scenarioFull)
	if missing {
		cc.bad("source_missing")
	}
	if srcs := doc[devEnvelope[[]SourceRecord]](b, "setup/sources.json"); srcs == nil || !bytes.Equal(mustJSON(want), mustJSON(srcs.Data)) {
		cc.bad("source_inventory_mismatch")
	}
	if def, err := ParseDevFullFlow(loaded.Files[FlowFile]); err != nil || !slices.Equal(def.Roles, DefaultDevFullFlow().Roles) {
		cc.bad(CodeCursorSelectionMismatch)
	}
	if ins, run, err := loaded.ManualPair("triage"); err != nil || checkTriageManualBudget(ins, run) != nil {
		cc.bad(CodePromptBudget)
	}
	if installed := doc[devEnvelope[[]SourceRecord]](b, "setup/installed-manuals.json"); installed == nil ||
		!bytes.Equal(mustJSON(installed.Data), mustJSON(installedManualRecords(loaded))) {
		cc.bad("installed_manuals_mismatch")
	}
	gen, _ := devFullFeatureRequest()
	if f := doc[DevFeatureRecord](b, "setup/feature-request.json"); f == nil || f.OriginalSHA256 != sha256Hex([]byte(FeatureRequest)) ||
		f.GeneratedSHA256 != sha256Hex([]byte(gen)) {
		cc.bad("feature_request_mismatch")
	}
	return flow, flowOK, cc.codes
}

// checkCursorQualification reconstructs a Cursor qualification record's
// selection and provenance (step 1); its platform, gate, versions, harness
// and vendor bindings, preflights, role readiness and four sidecars (step
// 2); the coder stage's place between the models stage and the
// coordinator phases (step 7); and its two shallow samples. Codes are
// deduplicated, in check order.
func checkCursorQualification(b *DevBundle, q *DevQualification) []string {
	var cc cursorCodes
	flow, flowOK, codes := cursorProvenanceCodes(b, q.Roles, q.FlowLabel, q.SelectionSHA256)
	for _, c := range codes {
		cc.bad(c)
	}
	switch {
	case q.GOOS != "linux":
		cc.bad("platform_not_linux")
	case !q.Gate.OptIn || !q.Gate.CIAbsent || q.Gate.Platform != "linux":
		cc.bad("gate_not_passed")
	}
	if !hex40RE.MatchString(q.Revision) || strings.TrimSpace(q.CallsheetVersion) == "" {
		cc.bad("revision_missing")
	}
	for _, c := range cursorVersionCodes(q.Versions) {
		cc.bad(c)
	}
	if !hex64RE.MatchString(q.Executable.Before) || q.Executable.Before != q.Executable.After {
		cc.bad("executable_changed")
	}
	if q.Cursor == nil || !cursorBindingOK(q.Cursor.Binding, cursorVersionPath(q.Versions)) {
		cc.bad(CodeCursorExecutableMismatch)
	}
	for _, c := range cursorPreflightCodes(b, flow, flowOK, q.RunID) {
		cc.bad(c)
	}
	for _, c := range cursorRoleCodes(b, flow, flowOK) {
		cc.bad(c)
	}
	if !cursorSidecarsOK(b) {
		cc.bad(CodeCursorNodesMismatch)
	}
	for _, c := range startedProcessCodes(b) {
		cc.bad(c)
	}
	for _, c := range cursorTimeCodes(b, q) {
		cc.bad(c)
	}
	// The coder stage: one admission, after every preflight finished and
	// before the first coordinator phase, its validation finished first.
	admits := countEvents(b, func(e Event) bool { return e.Type == EvTaskAdmitted && e.Hop == probeCoderLabel })
	adm, _ := firstEvent(b, func(e Event) bool { return e.Type == EvTaskAdmitted && e.Hop == probeCoderLabel })
	lastPreflight, phase := 0, 0
	for _, e := range b.Events {
		switch {
		case e.Type == EvPreflightFinished:
			lastPreflight = e.Seq
		case e.Type == EvPhaseStarted && phase == 0:
			phase = e.Seq
		}
	}
	lateValidation := countEvents(b, func(e Event) bool {
		return e.Hop == probeCoderLabel && (e.Type == EvValidationStarted || e.Type == EvValidationFinished) && phase > 0 && e.Seq > phase
	})
	switch {
	case admits == 0:
		cc.bad(CodeCursorCoderMissing)
	case admits != 1 || adm.Seq < lastPreflight || phase > 0 && adm.Seq > phase || lateValidation > 0:
		cc.bad(CodeCursorTaskMismatch)
	}
	for _, c := range cursorActivityOf(b, q.RunID).codes {
		cc.bad(c)
	}
	return cc.codes
}

// coderInputs reconstructs the coder fixture's input files independently:
// the captured full reviewer fixture's four files as the good sources,
// FEATURE.md and go.mod by the development seed's algorithm from the Go
// directive of the captured coder go.mod (whose exact generated form is
// required), and the input main.go by the one pinned prefix replacement.
// ok is false when a source is missing or contradicts its reconstruction.
func coderInputs(b *DevBundle) (input, good map[string][]byte, ok bool) {
	good = map[string][]byte{}
	for _, name := range probeFixtureFiles {
		data, has := b.captured("setup/probe-fixture/" + name)
		if !has {
			return nil, nil, false
		}
		good[name] = data
	}
	gomod, has := b.captured(probeCoderDir + "/input/go.mod")
	if !has {
		return nil, nil, false
	}
	ver, err := goDirective(gomod)
	if err != nil {
		return nil, nil, false
	}
	seed, err := devSeedFiles(ver)
	if err != nil || !bytes.Equal(seed["go.mod"], gomod) {
		return nil, nil, false
	}
	pending, err := probeCoderInputMain(good["main.go"])
	if err != nil {
		return nil, nil, false
	}
	return map[string][]byte{"FEATURE.md": seed["FEATURE.md"], "go.mod": seed["go.mod"], "main.go": pending, "main_test.go": good["main_test.go"],
		"design.md": good["design.md"], "revision-history.md": good["revision-history.md"]}, good, true
}

// coderFixtureRecord is fixture.json's data as the writer forms it.
func coderFixtureRecord(input, good map[string][]byte) cursorCoderFixtureRecord {
	rec := cursorCoderFixtureRecord{Line: probeCoderGoodLine, From: probeCoderGoodPrefix, To: probeCoderBadPrefix, Input: []DevProbeSource{},
		Expected: []DevProbeSource{}}
	for _, name := range probeCoderFixtureFiles {
		path := probeCoderDir + "/input/" + name
		rec.Input = append(rec.Input, DevProbeSource{Path: path, Bytes: len(input[name]), SHA256: sha256Hex(input[name]), Origin: probeCoderOrigin(name),
			StoragePath: capturedPath(path)})
	}
	path := probeCoderDir + "/expected/main.go"
	rec.Expected = append(rec.Expected, DevProbeSource{Path: path, Bytes: len(good["main.go"]), SHA256: sha256Hex(good["main.go"]),
		Origin: devProbeFixtureDir + "/main.go", StoragePath: capturedPath(path)})
	return rec
}

// coderRef is the coder evidence repository's ref: its commit, tree hash
// and files (false when absent or unreadable).
func coderRef(b *DevBundle, ref string) (string, string, map[string]TreeFile, bool) {
	return repoRefTree(b.Repos[probeCoderRepoDir], ref)
}

// requestDoc is a dispatch request's strict wire form.
func requestDoc(r contract.DispatchRequest) cursorRequestDoc {
	payload := r.Payload
	if payload == nil {
		payload = []string{}
	}
	return cursorRequestDoc{Target: r.Target, Goal: r.Goal, Payload: payload, Acceptance: r.Acceptance, RequestedBy: r.RequestedBy, Override: r.Override,
		Workspace: r.Workspace, Base: r.Base, WorkspaceInstance: r.WorkspaceInstance}
}

// checkCursorCoder reconstructs the supervisor-owned coder stage from its
// primary records (steps 3 to 6): the fixture from its pinned sources, the
// exact request from its re-rendered envelope, the supervisor push, the
// terminal task and its single role-valid IMPL_COMPLETE, the published
// result's exact one-file repair, and the independent validation of the
// result tree. Stored verdicts and publication records are only
// comparison targets; the expected tree never substitutes for published
// bytes.
func checkCursorCoder(b *DevBundle, q *DevQualification) []string {
	var cc cursorCodes
	c := q.Cursor
	dir := probeCoderDir
	req := doc[cursorRequestDoc](b, dir+"/request.json")
	fix := doc[devEnvelope[cursorCoderFixtureRecord]](b, dir+"/fixture.json")
	preq := doc[cursorPushRecord](b, dir+"/push-request.json")
	pres := doc[cursorPushResponse](b, dir+"/push-response.json")
	view := doc[contract.TaskShowResponse](b, dir+"/task.json")
	pub := doc[devEnvelope[cursorPublicationRecord]](b, dir+"/publication.json")
	verdict := doc[devEnvelope[cursorCoderVerdict]](b, dir+"/verdict.json")
	logs := doc[DevLogRecord](b, dir+"/logs.json")
	logText, hasLog := b.Files[dir+"/logs.txt"]
	if c == nil || req == nil || fix == nil || preq == nil || pres == nil || view == nil || pub == nil || verdict == nil || logs == nil || !hasLog ||
		b.Repos[probeCoderRepoDir] == nil {
		return []string{CodeCursorCoderMissing}
	}
	if logs.Task != c.Task || logs.Meta.StoredBytes != len(logText) || !logs.Meta.StoredChanged && !logs.Meta.StoredTruncated &&
		logs.Meta.DataBytes == len(logText) && logs.Meta.RawSHA256 != sha256Hex(logText) {
		// The retained task log is the coder task's own.
		cc.bad(CodeCursorTaskMismatch)
	}
	ws := c.Workspace
	// Step 3: the fixture, from its pinned sources.
	input, good, ok := coderInputs(b)
	inCommit, inTree, inFiles, inOK := coderRef(b, "input")
	fixtureOK := ok && inOK && inCommit == c.InputCommit && ws.SeedCommit == inCommit && ws.SeedTree == inTree &&
		bytes.Equal(mustJSON(fix.Data), mustJSON(coderFixtureRecord(input, good)))
	if exp, has := b.captured(dir + "/expected/main.go"); !has || !ok || !bytes.Equal(exp, good["main.go"]) {
		fixtureOK = false
	}
	if ok && inOK {
		names := sortedTree(inFiles)
		fixtureOK = fixtureOK && slices.Equal(names, slices.Sorted(slices.Values(probeCoderFixtureFiles)))
		for _, name := range probeCoderFixtureFiles {
			capt, has := b.captured(dir + "/input/" + name)
			f := inFiles[name]
			fixtureOK = fixtureOK && has && bytes.Equal(capt, input[name]) && f.Mode == filemode.Regular && bytes.Equal(f.Content, input[name])
		}
	}
	if !fixtureOK {
		cc.bad(CodeCursorFixtureMismatch)
	}
	// Step 4: the exact request, the push and the one coder task.
	base := c.InputCommit
	want := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: HopCoder}, Acceptance: cursorProbeAcceptance,
		RequestedBy: requester, Workspace: &ws.Name, WorkspaceInstance: &ws.Instance, Base: &base}
	if ok {
		want.Goal = cursorProbeEnvelope(q.RunID, ws, c.InputCommit, input)
	}
	v := view.Task
	if !ok || !bytes.Equal(mustJSON(*req), mustJSON(requestDoc(want))) || !bytes.Equal(mustJSON(requestDoc(v.Request)), mustJSON(requestDoc(want))) ||
		!contract.ValidWorkspaceName(ws.Name) || !strings.HasPrefix(ws.Name, "e2e-"+probeCoderNamespace+"-") || ws.Instance == "" {
		cc.bad(CodeCursorRequestMismatch)
	}
	if preq.Name != ws.Name || preq.Instance != ws.Instance || preq.Branch != "main" || preq.Path == "" || preq.Commit != c.InputCommit ||
		pres.Error != "" || pres.Result.Name != ws.Name || pres.Result.Instance != ws.Instance || pres.Result.Branch != contract.DefaultBranchRef ||
		pres.Result.Commit != c.InputCommit {
		cc.bad(CodeCursorPushMismatch)
	}
	if !coderTaskAccounted(b, c.Task) || v.TaskID != c.Task || v.Role.ID != HopCoder || v.Role.Adapter != cursorShippedCoder.Adapter ||
		v.Effective.Model != cursorShippedCoder.Model || v.Effective.Effort != cursorShippedCoder.Effort || v.Request.Override != nil {
		cc.bad(CodeCursorTaskMismatch)
	}
	// Step 5: the authoritative final and the publication.
	r := v.Result
	wb := v.WorkspaceBinding
	finalOK := v.State == contract.TaskSucceeded && r != nil && r.ExitCode != nil && *r.ExitCode == 0 && r.FinalMessage != nil && !r.FinalMessageTruncated
	if finalOK {
		got, has := b.Files[dir+"/final.txt"]
		marker, err := parseTerminalMarker(*r.FinalMessage)
		finalOK = has && string(got) == *r.FinalMessage && err == nil && marker == markerImplComplete && markerAllowed(HopCoder, modeImplement, marker)
	}
	if !finalOK || wb == nil || wb.Name != ws.Name || wb.Instance != ws.Instance || wb.BaseCommit == nil || *wb.BaseCommit != c.InputCommit {
		cc.bad(CodeCursorTaskMismatch)
	}
	result, resultTree, resultFiles, pubOK := coderPublication(b, c, r, input, good)
	if !pubOK || !bytes.Equal(mustJSON(pub.Data), mustJSON(result)) {
		cc.bad(CodeCursorPublicationMismatch)
	}
	// Step 6: the independent validation of the result tree.
	validated := pubOK && coderValidationOK(b, q, c, v, input, resultTree, resultFiles)
	if !validated {
		cc.bad(CodeCursorValidationMismatch)
	}
	wantVerdict := cursorCoderVerdict{Task: c.Task, Marker: markerImplComplete, InputCommit: c.InputCommit, ResultCommit: c.ResultCommit,
		PublicationAdmitted: pubOK, ValidationPassed: validated}
	if verdict.Data != wantVerdict {
		// A stored verdict contradicting the reconstruction (a forged
		// success beside a failing primary result included).
		cc.bad(CodeCursorPublicationMismatch)
	}
	return cc.codes
}

// coderTaskAccounted reports exactly one coder task in the cost record and
// the cleanup settlements, and no second coder admission.
func coderTaskAccounted(b *DevBundle, task string) bool {
	cost := doc[DevCost](b, devCostFile)
	cl := doc[devEnvelope[DevCleanup]](b, devCleanupFile)
	if cost == nil || cl == nil || task == "" {
		return false
	}
	n := 0
	for _, t := range cost.Tasks {
		if t.Label == probeCoderLabel {
			n++
			if t.Task != task {
				return false
			}
		}
	}
	m := 0
	for _, s := range cl.Data.Settled {
		if s.Label == probeCoderLabel {
			m++
			if s.Task != task {
				return false
			}
		}
	}
	admits := countEvents(b, func(e Event) bool { return e.Type == EvTaskAdmitted && e.Hop == probeCoderLabel && e.Task == task })
	return n == 1 && m == 1 && admits == 1
}

// coderPublication reconstructs the publication record from the published
// result: the task's published workspace result in the coder workspace on
// the input, the retained result ref, one parent equal to the input, the
// existing coder admission checks and exactly main.go changed to the
// known-good bytes with every other file and mode unchanged.
func coderPublication(b *DevBundle, c *DevCursorQualification, r *contract.TaskResult, input, good map[string][]byte) (cursorPublicationRecord, string,
	map[string]TreeFile, bool) {
	rec := cursorPublicationRecord{Changed: []string{}, Files: []DevProbeSource{}}
	if r == nil || r.Workspace == nil || r.Workspace.Publication != contract.PublicationPublished || r.Workspace.Commit == nil ||
		*r.Workspace.Commit != c.ResultCommit || r.Workspace.Name != c.Workspace.Name || r.Workspace.Instance != c.Workspace.Instance ||
		r.Workspace.BaseCommit == nil || *r.Workspace.BaseCommit != c.InputCommit || input == nil {
		return rec, "", nil, false
	}
	repo := b.Repos[probeCoderRepoDir]
	if h, ok := repo.refs["refs/heads/result"]; !ok || h.String() != c.ResultCommit {
		return rec, "", nil, false
	}
	files, parent, tree, err := readCommitTree(repo.repo, c.ResultCommit)
	if err != nil {
		return rec, "", nil, false
	}
	var basef []TreeFile
	for _, name := range probeCoderFixtureFiles {
		basef = append(basef, TreeFile{Path: name, Mode: filemode.Regular, Content: input[name]})
	}
	bmap, _ := indexTree(basef)
	rmap, ierr := indexTree(files)
	if ierr != nil {
		return rec, "", nil, false
	}
	rec = cursorPublicationRecord{Commit: c.ResultCommit, Parent: parent, Tree: tree, Changed: append([]string{}, changedPaths2(bmap, rmap)...),
		Files: []DevProbeSource{}, Admitted: true}
	for _, name := range sortedTree(rmap) {
		rec.Files = append(rec.Files, DevProbeSource{Path: name, Bytes: len(rmap[name].Content), SHA256: sha256Hex(rmap[name].Content), Origin: c.ResultCommit})
	}
	ok := parent == c.InputCommit && AdmitTree(HopCoder, modeImplement, markerImplComplete, basef, files) == nil &&
		slices.Equal(rec.Changed, []string{"main.go"}) && bytes.Equal(rmap["main.go"].Content, good["main.go"])
	for _, f := range rmap {
		ok = ok && f.Mode == filemode.Regular
	}
	return rec, tree, rmap, ok
}

// coderValidationOK reconstructs the coder result's independent
// validation from the result tree and the retained records, with the full
// checker's oracle, requirement and coverage derivations scoped to
// probe/coder/01/validation: the exact commands bound to the result
// commit and tree and to their own validation events within one 60s
// budget, every output hash, the build and the ten CLI cases, the go test
// -json stream with every TestRun/<case-id>, and changed-code coverage
// strictly above 80% against the input main.go; the coder's admission
// through publication within 10m, inside the 90m attempt.
func coderValidationOK(b *DevBundle, q *DevQualification, c *DevCursorQualification, v contract.TaskView, input map[string][]byte, tree string,
	rfiles map[string]TreeFile) bool {
	vd := probeCoderDir + "/validation/"
	ft, cs := doc[TestRun](b, vd+"final-test.json"), doc[CasesRecord](b, vd+"cases.json")
	rq, cov := doc[RequirementsRecord](b, vd+"requirements.json"), doc[DevCoverageRecord](b, vd+"coverage.json")
	if ft == nil || cs == nil || rq == nil || cov == nil {
		return false
	}
	for _, n := range probeCoderValidationFiles {
		if _, ok := b.Files[vd+n]; !ok {
			return false
		}
	}
	commit := c.ResultCommit
	files := map[string][]byte{}
	for p, f := range rfiles {
		files[p] = f.Content
	}
	pinned := ""
	for _, ver := range q.Versions {
		if ver.Tool == "go" {
			pinned = ver.Version
		}
	}
	if testRunCode(ft, commit, tree, rfiles) != "" || ft.Toolchain != pinned {
		return false
	}
	execs := []ExecRecord{cs.Build, rq.TestRun, cov.Exec}
	for _, r := range cs.Cases {
		if r.Exec.Started {
			execs = append(execs, r.Exec)
		}
	}
	start, ok := parseTime(q.AttemptStart)
	if !ok || !coderExecBinding(b, execs, commit, tree, start) {
		return false
	}
	stream := b.Files[vd+"tests.jsonl"]
	if passed, _ := testRunVerdict(rq.TestRun, stream); !passed || rq.TestRun.StdoutSHA256 != sha256Hex(stream) || rq.TestRun.Name != "go_test_json" ||
		!slices.Equal(rq.TestRun.Argv, validationArgvJSON) {
		return false
	}
	if bd := cs.Build; bd.Name != "go_build" || !slices.Equal(bd.Argv, validationArgvBuild) || !bd.Started || bd.ExitCode != 0 || bd.Signal != "" ||
		bd.TimedOut || bd.Truncated {
		return false
	}
	if cs.Build.StdoutSHA256 != sha256Hex(b.Files[vd+"build-stdout.txt"]) || cs.Build.StderrSHA256 != sha256Hex(b.Files[vd+"build-stderr.txt"]) ||
		cov.Exec.StdoutSHA256 != sha256Hex(b.Files[vd+"coverage-stdout.txt"]) || cov.Exec.StderrSHA256 != sha256Hex(b.Files[vd+"coverage-stderr.txt"]) ||
		rq.TestRun.StderrSHA256 != sha256Hex(b.Files[vd+"test-json-stderr.txt"]) || cov.FinalStdout != sha256Hex(b.Files[vd+"final-stdout.txt"]) ||
		cov.FinalStderr != sha256Hex(b.Files[vd+"final-stderr.txt"]) {
		return false
	}
	wantReq := buildRequirements(files, stream, cs.Cases, rq.TestRun, commit)
	if !requirementsEqual(wantReq, *rq) || !wantReq.RunSignature || !wantReq.TableTests || !wantReq.TestCallsRun || len(wantReq.MissingTests) > 0 ||
		len(wantReq.FailedTests) > 0 || !wantReq.TestRunPassed {
		return false
	}
	if len(cs.Cases) != len(FeatureCases) || cs.Commit != commit || cs.Tree != tree {
		return false
	}
	for i, fc := range FeatureCases {
		r := cs.Cases[i]
		pass, code := evaluateCase(fc, r.Exec, r.Stdout, r.Stderr)
		if r.ID != fc.ID || !pass || !r.Pass || code != r.Code || !slices.Equal(r.Exec.Argv, append([]string{"greeting"}, fc.Args...)) ||
			r.Exec.Name != "case_"+fc.ID || sha256Hex([]byte(r.Stdout)) != r.Exec.StdoutSHA256 || sha256Hex([]byte(r.Stderr)) != r.Exec.StderrSHA256 {
			return false
		}
	}
	profile := b.Files[vd+"coverage.out"]
	got, err := deriveChangedCoverage(input["main.go"], files["main.go"], profile, modulePathOf(files["go.mod"]))
	if err != nil || cov.Commit != commit || cov.Tree != tree || cov.Exec.Name != "go_test_cover" || !slices.Equal(cov.Exec.Argv, validationArgvCover) ||
		cov.Profile != sha256Hex(profile) || !cov.Exec.Started || cov.Exec.ExitCode != 0 || cov.Exec.TimedOut || cov.Exec.Signal != "" ||
		!bytes.Equal(mustJSON(&got), mustJSON(cov.Coverage)) || !got.Pass {
		return false
	}
	// Admission through publication within 10m (the plane's own record),
	// all inside the 90m attempt.
	d, dok := duration(v)
	created, cok := contract.ParseTime(v.CreatedAt)
	return dok && cok && d < FeatureTaskBound && created.Before(start.Add(AttemptBound)) && !created.Before(start)
}

// coderExecBinding binds every coder validation command to the result
// commit and tree and to its own coder validation start and end events,
// all within one shared 60s deadline from the first coder validation
// start, clipped by the attempt.
func coderExecBinding(b *DevBundle, execs []ExecRecord, commit, tree string, attempt time.Time) bool {
	at := func(seq int) *Event {
		if seq < 1 || seq > len(b.Events) || b.Events[seq-1].Seq != seq {
			return nil
		}
		return &b.Events[seq-1]
	}
	var first, last time.Time
	if e, ok := firstEvent(b, func(e Event) bool { return e.Type == EvValidationStarted && e.Hop == probeCoderLabel }); ok {
		first, _ = parseTime(e.Time)
	}
	type span struct {
		rec      ExecRecord
		st, done time.Time
	}
	var spans []span
	for _, e := range execs {
		s, f := at(e.StartSeq), at(e.EndSeq)
		if e.Schema != ExecSchema || e.Commit != commit || e.Tree != tree || s == nil || f == nil || s.Type != EvValidationStarted ||
			f.Type != EvValidationFinished || s.Hop != probeCoderLabel || f.Hop != probeCoderLabel || s.Code != e.Name || f.Code != e.Name ||
			e.StartSeq >= e.EndSeq {
			return false
		}
		st, ok1 := parseTime(s.Time)
		ft, ok2 := parseTime(f.Time)
		if !ok1 || !ok2 || ft.Before(st) {
			return false
		}
		if first.IsZero() || st.Before(first) {
			first = st
		}
		if ft.After(last) {
			last = ft
		}
		spans = append(spans, span{e, st, ft})
	}
	if first.IsZero() {
		return false
	}
	// Each command's recorded duration and deadline against its own events
	// and the one shared 60s budget: 0 <= duration <= its deadline <= 60s,
	// the duration within its start-to-end events, and its deadline the
	// shared one (each within one second of clock granularity).
	shared := finalDeadline(first, attempt)
	const slack = time.Second
	for _, s := range spans {
		// The raw integers first, 0 <= duration <= deadline <= 60 000 ms with
		// a positive deadline, so no recorded value can wrap through
		// time.Duration; only bounded values are converted.
		if s.rec.DurationMS < 0 || s.rec.DeadlineMS <= 0 || s.rec.DurationMS > s.rec.DeadlineMS || s.rec.DeadlineMS > FinalTestBound.Milliseconds() {
			return false
		}
		dur, dl := time.Duration(s.rec.DurationMS)*time.Millisecond, time.Duration(s.rec.DeadlineMS)*time.Millisecond
		if dur > s.done.Sub(s.st)+slack || s.st.Add(dl).After(shared.Add(slack)) {
			return false
		}
	}
	return last.Before(shared)
}

// cursorTimeCodes validates a Cursor qualification's time bounds: the
// event log consecutively numbered, run_started first and its times
// ordered; the 90m attempt window exactly from its start; every admission,
// observation, decision and validation event inside that window; and
// cleanup within its 2m bound.
func cursorTimeCodes(b *DevBundle, q *DevQualification) []string {
	var cc cursorCodes
	start, ok1 := parseTime(q.AttemptStart)
	deadline, ok2 := parseTime(q.AttemptDeadline)
	if !ok1 || !ok2 || !deadline.Equal(start.Add(AttemptBound)) {
		cc.bad("attempt_window_invalid")
	}
	if len(b.Events) == 0 || b.Events[0].Type != EvRunStarted {
		cc.bad("run_started_not_first")
	}
	var last time.Time
	for i, e := range b.Events {
		t, ok := parseTime(e.Time)
		switch {
		case e.Seq != i+1:
			cc.bad("sequence_not_increasing")
		case !ok:
			cc.bad("time_invalid")
		case t.Before(last):
			cc.bad("time_not_monotonic")
		}
		last = t
		switch e.Type {
		case EvTaskAdmitted, EvObserved, EvOwnerDecision, EvValidationStarted, EvValidationFinished, EvPhaseStarted, EvPreflightStarted:
			if ok1 && ok && t.Before(start) {
				cc.bad("attempt_window_invalid")
			}
			if ok2 && ok && !t.Before(deadline) {
				cc.bad("attempt_deadline_exceeded")
			}
		}
	}
	for _, c := range cursorCleanupCodes(b, q.AttemptStart, q.AttemptDeadline) {
		cc.bad(c)
	}
	return cc.codes
}

// cursorCleanupCodes reconciles cleanup.json's own timestamps with the
// collection and cleanup events and the attempt: exactly one
// collection_started, cleanup_started and cleanup_finished event; the
// recorded start the collection's start (at most one second before its
// event, never after it), the recorded deadline exactly that start plus the
// 2m cleanup bound, the recorded finish between the cleanup_started and
// cleanup_finished events and before that deadline, and the whole cleanup
// inside the attempt plus its cleanup bound. Shifting the record's
// timestamps away from the events cannot pass.
func cursorCleanupCodes(b *DevBundle, attemptStart, attemptDeadline string) []string {
	cl := doc[devEnvelope[DevCleanup]](b, devCleanupFile)
	if cl == nil {
		return nil // an absent cleanup.json fails the closed layout
	}
	eventTime := func(typ string) (time.Time, bool) {
		if countEvents(b, func(e Event) bool { return e.Type == typ }) != 1 {
			return time.Time{}, false
		}
		e, _ := firstEvent(b, func(e Event) bool { return e.Type == typ })
		return parseTime(e.Time)
	}
	const slack = time.Second
	s, okS := parseTime(cl.Data.StartedAt)
	f, okF := parseTime(cl.Data.FinishedAt)
	dl, okD := parseTime(cl.Data.Deadline)
	collect, okC := eventTime(EvCollectionStarted)
	begun, okB := eventTime(EvCleanupStarted)
	ended, okE := eventTime(EvCleanupFinished)
	start, ok1 := parseTime(attemptStart)
	deadline, ok2 := parseTime(attemptDeadline)
	switch {
	case !okS || !okF || !okD || !okC || !okB || !okE || !ok1 || !ok2,
		!dl.Equal(s.Add(CleanupBound)),
		s.After(collect) || collect.Sub(s) > slack,
		f.Before(begun) || f.After(ended) || !f.Before(dl),
		s.Before(start) || !f.Before(deadline.Add(CleanupBound)):
		return []string{"cleanup_deadline_exceeded"}
	}
	return nil
}

// ---- Shallow samples ----

// cursorActivityFacts are a Cursor bundle's reconstructed samples: the
// recomputed summary, the evidence codes (missing or internally
// contradictory records only; sample quality never) and per-root
// observation lines with every quality flag.
type cursorActivityFacts struct {
	summary        CursorActivitySummary
	before, after  *ActivityAggregate
	codes          []string
	missing        []string
	lines          []string
	contradictions []string
}

// cursorActivityOf reconstructs b's two shallow samples for run.
func cursorActivityOf(b *DevBundle, run string) cursorActivityFacts {
	f := cursorActivityFacts{summary: CursorActivitySummary{Changes: []ActivityChange{}}}
	var cc cursorCodes
	for i, phase := range []string{"before", "after"} {
		rel := cursorActivityFiles[i]
		a := doc[ActivityAggregate](b, rel)
		if a == nil {
			cc.bad(CodeCursorActivityMissing)
			f.missing = append(f.missing, phase)
			continue
		}
		if i == 0 {
			f.before, f.summary.Before = a, rel
		} else {
			f.after, f.summary.After = a, rel
		}
		if problems := activityProblems(a, run, phase); len(problems) > 0 {
			cc.bad(CodeCursorActivityMismatch)
			f.contradictions = append(f.contradictions, problems...)
		}
		for _, r := range a.Roots {
			f.lines = append(f.lines, fmt.Sprintf("%s %s %s: status %s, exhaustive %t (top-level %t), %d rows, sampled direct %s bytes %d, sampled children %s bytes %d; flags %s",
				phase, r.Scope, r.Path, r.Status, r.Exhaustive, r.TopLevelExhaustive, len(r.Rows), countsText(r.ObservedCounts.Direct), r.ObservedFileBytes.Direct,
				countsText(r.ObservedCounts.Children), r.ObservedFileBytes.Children, flagsText(r.Flags)))
			// Every retained row with its own child-sample quality flags (a
			// directory's entry or depth limit is not a root flag).
			for _, row := range r.Rows {
				f.lines = append(f.lines, fmt.Sprintf("%s %s %s row %s: type %s, status %s; sampled children %s bytes %d; children exhaustive %t; flags %s",
					phase, r.Scope, r.Path, row.Name, row.Type, row.Status, countsText(row.ObservedCounts), row.ObservedFileBytes, row.ChildrenExhaustive,
					flagsText(row.Flags)))
			}
		}
	}
	f.summary.Changes = compareActivity(f.before, f.after)
	f.codes = cc.codes
	return f
}

// activityRootPlan is the closed root list: scope and token in order.
var activityRootPlan = [][2]string{{activityScopeCache, "$HOME/.cache"}, {activityScopeTmp, "/tmp"}, {activityScopeVarTmp, "/var/tmp"}}

// activityStatuses and activityTypes are the closed vocabularies.
var (
	activityStatuses = []string{actOK, actAbsent, actUnreadable, actForeign, actRaced, actNotDir}
	activityRowTypes = []string{actDir, actUnknown}
)

// validActivityName reports a lossless encoded basename within the bound:
// plain valid UTF-8 without control characters and not starting b64:, or
// b64: with standard base64 of bytes that need encoding.
func validActivityName(name string) bool {
	if name == "" || len(name) > activityMaxName {
		return false
	}
	if rest, ok := strings.CutPrefix(name, "b64:"); ok {
		raw, err := base64.StdEncoding.DecodeString(rest)
		return err == nil && len(raw) > 0 && encodePath(string(raw)) == name && !strings.Contains(string(raw), "/") && string(raw) != "." && string(raw) != ".."
	}
	return encodePath(name) == name && !strings.Contains(name, "/") && name != "." && name != ".."
}

// countSum is the saturated sum of c's counts.
func countSum(c ActivityCounts) uint64 {
	var overflow bool
	n := uint64(0)
	for _, x := range []uint64{c.File, c.Directory, c.Symlink, c.Other, c.Unknown} {
		n = satAdd(n, x, &overflow)
	}
	return n
}

// activityProblems validates one sample under the observer's rules: its
// run, phase, sampling, depth and limits; exactly the three named roots
// in order with their token spellings; legal statuses and types; unique
// sorted, safely encoded row names within the row bound; and conservative
// internal consistency (retained-row subtotals equal the root's child
// totals where no row was omitted, at most those totals otherwise; direct
// totals at least the retained rows; exhaustive claims consistent with
// their flags). Recorded direct totals and metadata hashes are
// observations, never remeasured; sample quality is never a problem.
func activityProblems(a *ActivityAggregate, run, phase string) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	limits := ActivityLimits{TopLevelEntries: activityMaxEntries, ChildEntries: activityMaxEntries, OutputBytes: activityMaxOutput,
		ScanBudgetMS: activityScanBudget.Milliseconds()}
	if a.RunID != run || a.Phase != phase || a.Sampling != activitySampling || a.MaxDepth != activityMaxDepth || a.Limits != limits {
		bad("%s: run, phase, sampling, depth or limits", phase)
	}
	if len(a.Roots) != len(activityRootPlan) {
		bad("%s: %d roots", phase, len(a.Roots))
		return out
	}
	for i, r := range a.Roots {
		if r.Scope != activityRootPlan[i][0] || r.Path != activityRootPlan[i][1] || !slices.Contains(activityStatuses, r.Status) {
			bad("%s root %d: scope, path or status", phase, i)
			continue
		}
		for _, p := range activityRootProblems(r) {
			bad("%s %s: %s", phase, r.Scope, p)
		}
	}
	return out
}

// activityRootProblems validates one root under the observer's rules.
func activityRootProblems(r ActivityRoot) []string {
	var out []string
	f := r.Flags
	zeroCounts := r.ObservedCounts == ActivityRootCounts{} && r.ObservedFileBytes == ActivityRootBytes{}
	switch r.Status {
	case actAbsent:
		// An absent root is a complete, empty observation.
		if len(r.Rows) > 0 || !zeroCounts || !r.Exhaustive || !r.TopLevelExhaustive || f != (ActivityFlags{}) {
			out = append(out, "absent root with observations")
		}
		return out
	case actUnreadable, actRaced, actNotDir:
		if len(r.Rows) > 0 || !zeroCounts || r.Exhaustive || r.TopLevelExhaustive {
			out = append(out, "unenumerated root with observations")
		}
		return out
	}
	if len(r.Rows) > activityMaxEntries {
		out = append(out, "more rows than the entry limit")
	}
	if r.TopLevelExhaustive && f.EntryLimited {
		out = append(out, "complete enumeration beside an entry limit")
	}
	if r.Exhaustive && (!r.TopLevelExhaustive || f.TimeLimited || f.OutputLimited || f.NameUnrecorded > 0 || f.ExcludedEntries > 0 ||
		f.UnreadableEntries > 0 || f.RacedEntries > 0 || f.ForeignEntries > 0) {
		out = append(out, "exhaustive beside a quality flag")
	}
	var children ActivityCounts
	var childBytes uint64
	var overflow bool
	dirs, unknown := uint64(0), uint64(0)
	prev := ""
	for i, row := range r.Rows {
		if !validActivityName(row.Name) || i > 0 && row.Name <= prev {
			out = append(out, "row names not unique, sorted and safely encoded")
		}
		prev = row.Name
		for _, p := range activityRowProblems(row) {
			out = append(out, row.Name+": "+p)
		}
		if r.Exhaustive && (!row.ChildrenExhaustive || row.Flags.UnreadableEntries > 0 || row.Flags.RacedEntries > 0 || row.Flags.ExcludedEntries > 0 ||
			row.Flags.TimeLimited) {
			out = append(out, "exhaustive beside an incomplete row")
		}
		switch row.Type {
		case actDir:
			dirs++
		case actUnknown:
			unknown++
		}
		addCounts(&children, row.ObservedCounts, &overflow)
		childBytes = satAdd(childBytes, row.ObservedFileBytes, &overflow)
	}
	// Retained-row subtotals: equal to the child totals where no row was
	// omitted, at most them where output limiting or an unrecorded name
	// means rows can be missing.
	omitted := f.OutputLimited || f.NameUnrecorded > 0
	if !omitted && (children != r.ObservedCounts.Children || childBytes != r.ObservedFileBytes.Children) {
		out = append(out, "child totals are not the retained rows' subtotal")
	}
	if omitted && (!countsAtLeast(r.ObservedCounts.Children, children) || r.ObservedFileBytes.Children < childBytes) {
		out = append(out, "child totals below the retained rows' subtotal")
	}
	d := r.ObservedCounts.Direct
	if d.Directory < dirs || d.Unknown < unknown || !f.NumericOverflow && countSum(d) > activityMaxEntries {
		out = append(out, "direct totals contradict the retained rows or the entry limit")
	}
	if d.File == 0 && r.ObservedFileBytes.Direct != 0 {
		out = append(out, "direct file bytes without a file")
	}
	return out
}

// countsAtLeast reports a >= b in every type.
func countsAtLeast(a, b ActivityCounts) bool {
	return a.File >= b.File && a.Directory >= b.Directory && a.Symlink >= b.Symlink && a.Other >= b.Other && a.Unknown >= b.Unknown
}

// activityRowProblems validates one retained row under the observer's
// rules: a directory row has its metadata digest, an unknown row none and
// no sample; a foreign row is never descended; a complete child
// enumeration has no entry or time limit; the child sample is bounded; the
// newest mtime exists exactly when a child was counted.
func activityRowProblems(row ActivityRow) []string {
	var out []string
	if !slices.Contains(activityRowTypes, row.Type) || !slices.Contains(activityStatuses, row.Status) || row.Status == actAbsent || row.Status == actNotDir {
		return append(out, "type or status")
	}
	n := countSum(row.ObservedCounts)
	switch {
	case row.Type == actUnknown && (row.MetadataSHA256 != nil || n != 0 || row.Status != actUnreadable && row.Status != actRaced):
		out = append(out, "unknown row with metadata or a sample")
	case row.Type == actDir && (row.MetadataSHA256 == nil || !hex64RE.MatchString(*row.MetadataSHA256)):
		out = append(out, "directory row without its metadata digest")
	}
	if row.Status == actForeign && (n != 0 || row.ChildrenExhaustive) {
		out = append(out, "foreign row descended")
	}
	if row.ChildrenExhaustive && (row.Flags.EntryLimited || row.Flags.TimeLimited) {
		out = append(out, "complete children beside a limit")
	}
	if n > activityMaxEntries || (row.NewestMtimeNS == nil) != (n == 0) || row.ObservedCounts.File == 0 && row.ObservedFileBytes != 0 ||
		row.Flags.DepthLimited && row.ObservedCounts.Directory == 0 {
		out = append(out, "child sample contradicts its bounds")
	}
	return out
}
