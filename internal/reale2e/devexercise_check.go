package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// The combined exercise root's offline check (design 12a-dev-flow-light,
// Exercise invocation; FP-8). It rechecks both children independently
// with the shared checker (each child's own report and audit compared byte
// for byte), then the root: the typed cost authorization, the fixed child
// order and directories, unique identities, the shared harness and
// selection, the separate deadlines and finalization tails, the
// cleanup-to-next-start ordering, the summed actual accounting and the
// root's own inventory. Combined PASS requires both independent PASSes;
// any FAIL or NOT_RUN is a combined FAIL (exit 1); a malformed or
// unsupported root is exit 2. A standalone full or light success cannot be
// spliced into a root: the authorization and ordering evidence binds them.

// DevExerciseChildResult is one child's recomputed result in the root
// report.
type DevExerciseChildResult struct {
	Dir            string `json:"dir"`
	Scenario       string `json:"scenario"`
	RunID          string `json:"run_id"`
	Status         string `json:"status"`
	Stop           string `json:"stop"`
	Mechanical     string `json:"mechanical"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ReportSHA256   string `json:"report_sha256"`
	AuditSHA256    string `json:"audit_sha256"`
	ElapsedMS      *int64 `json:"elapsed_ms"`
	PreflightCalls int    `json:"preflight_calls"`
	WorkflowCalls  int    `json:"workflow_calls"`
	RejectedCalls  int    `json:"rejected_calls"`
	WorkerCalls    int    `json:"worker_calls"`
	Phases         int    `json:"coordinator_phases"`
}

// DevExerciseAccounting is the root's summed actual accounting.
type DevExerciseAccounting struct {
	PreflightCalls      int    `json:"preflight_calls"`
	WorkflowCalls       int    `json:"workflow_calls"`
	RejectedCalls       int    `json:"rejected_calls"`
	WorkerCalls         int    `json:"worker_calls"`
	CoordinatorPhases   int    `json:"coordinator_phases"`
	MaxWorkerCalls      int    `json:"max_worker_calls"`
	HappyPathCalls      int    `json:"happy_path_calls"`
	ProviderUsage       string `json:"provider_usage"`
	AuthorizationWaitMS *int64 `json:"authorization_wait_ms"`
	TotalElapsedMS      *int64 `json:"total_elapsed_ms"`
}

// DevExerciseReport is the root's report.json.
type DevExerciseReport struct {
	Schema     string                   `json:"schema"`
	ExerciseID string                   `json:"exercise_id"`
	Result     string                   `json:"result"`
	Claim      string                   `json:"claim"`
	Judgments  string                   `json:"judgments"`
	Children   []DevExerciseChildResult `json:"children"`
	Accounting DevExerciseAccounting    `json:"accounting"`
	Codes      []string                 `json:"codes"`
	Inventory  []InventoryEntry         `json:"inventory"`
	Error      string                   `json:"error"`
}

// DevExerciseAudit is the root's audit.json.
type DevExerciseAudit struct {
	Schema       string        `json:"schema"`
	ExerciseID   string        `json:"exercise_id"`
	ReportSHA256 string        `json:"report_sha256"`
	Mechanical   string        `json:"mechanical"`
	Judgments    string        `json:"judgments"`
	Rows         []DevAuditRow `json:"rows"`
}

// exerciseCheckResult is one root check's canonical outputs.
type exerciseCheckResult struct {
	report     DevExerciseReport
	reportJSON []byte
	reportText string
	audit      DevExerciseAudit
	auditJSON  []byte
	auditText  string
}

// exerciseRootFiles are the root's only files.
var exerciseRootFiles = map[string]bool{devExerciseFile: true, "report.json": true, "report.txt": true, "audit.json": true, "audit.md": true,
	"audit-review.md": true}

// readExerciseRoot reads the closed root layout: only its files and the
// attempts directory with only the two fixed child directories, the
// root-only text within 1 MiB.
func readExerciseRoot(root string) (string, map[string][]byte, error) {
	resolved, err := safeEvidenceRoot(root)
	if err != nil {
		return "", nil, err
	}
	files := map[string][]byte{}
	var total int64
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return "", nil, schemaErr("exercise root: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		st, err := os.Lstat(filepath.Join(resolved, name))
		switch {
		case err != nil:
			return "", nil, schemaErr("exercise root %s: %v", name, err)
		case st.Mode()&fs.ModeSymlink != 0:
			return "", nil, schemaErr("exercise root entry %s is a symlink", name)
		case name == "attempts" && st.IsDir():
			kids, err := os.ReadDir(filepath.Join(resolved, name))
			if err != nil {
				return "", nil, schemaErr("exercise attempts: %v", err)
			}
			for _, k := range kids {
				kst, err := os.Lstat(filepath.Join(resolved, name, k.Name()))
				if err != nil || !kst.IsDir() || kst.Mode()&fs.ModeSymlink != 0 || (k.Name() != "full" && k.Name() != "light") {
					return "", nil, schemaErr("exercise attempts entry %s is not a fixed child directory", k.Name())
				}
			}
		case !st.Mode().IsRegular() || !exerciseRootFiles[name]:
			return "", nil, schemaErr("exercise root entry %s is not part of the layout", name)
		default:
			total += st.Size()
			if total > maxExerciseRootText {
				return "", nil, schemaErr("the exercise root text exceeds %d bytes", maxExerciseRootText)
			}
			b, err := readBounded(filepath.Join(resolved, name), maxExerciseRootText)
			if err != nil || !utf8.Valid(b) {
				return "", nil, schemaErr("exercise root file %s is unreadable or not UTF-8", name)
			}
			files[name] = b
		}
	}
	return resolved, files, nil
}

// checkExerciseRoot checks an exercise root (initial: the live writer's
// form, before the derived root files exist).
func checkExerciseRoot(root string, initial bool) (exerciseCheckResult, error) {
	return checkExerciseRootData(root, nil, initial)
}

// checkExerciseRootData is checkExerciseRoot with the live writer's
// exercise.json bytes in place of the stored record (nil: the stored
// one): the root is aggregated before its record is written.
func checkExerciseRootData(root string, record []byte, initial bool) (exerciseCheckResult, error) {
	var res exerciseCheckResult
	resolved, files, err := readExerciseRoot(root)
	if err != nil {
		return res, err
	}
	if record != nil {
		files[devExerciseFile] = record
	}
	data, ok := files[devExerciseFile]
	if !ok {
		return res, schemaErr("%s is missing", devExerciseFile)
	}
	schema, err := manifestSchema(data)
	if err != nil || schema != DevExerciseSchema && schema != DevCursorExerciseSchema {
		return res, schemaErr("%s: unsupported schema", devExerciseFile)
	}
	// A Cursor root is distinguished at the loader by its own schema, never
	// guessed from its children (design 12a-3-cursor-worker, slice 3b).
	cursorRoot := schema == DevCursorExerciseSchema
	if err := checkCursorField(data, cursorRoot); err != nil {
		return res, schemaErr("%s: %v", devExerciseFile, err)
	}
	var ex DevExercise
	if err := decodeStrict(data, &ex); err != nil {
		return res, schemaErr("%s: %v", devExerciseFile, err)
	}
	reportSchema, auditSchema := devExerciseReportSchema, devExerciseAuditSchema
	if cursorRoot {
		reportSchema, auditSchema = devCursorExerciseReportSchema, devCursorExerciseAuditSchema
	}
	rep := DevExerciseReport{Schema: reportSchema, ExerciseID: ex.ExerciseID, Result: ResultFail, Judgments: PendingReview,
		Children: []DevExerciseChildResult{}, Codes: []string{}, Inventory: []InventoryEntry{{Path: devExerciseFile, Bytes: int64(len(data)), SHA256: sha256Hex(data)}},
		Accounting: DevExerciseAccounting{MaxWorkerCalls: exerciseMaxCalls, HappyPathCalls: exerciseHappyCalls, ProviderUsage: usageUnknown}}
	bad := func(code string) {
		if !slices.Contains(rep.Codes, code) {
			rep.Codes = append(rep.Codes, code)
		}
	}
	rows := &exerciseRows{}
	if len(ex.Children) != 2 || ex.Children[0].Dir != exerciseFullDir || ex.Children[0].Scenario != scenarioFull || ex.Children[1].Dir != exerciseLightDir ||
		ex.Children[1].Scenario != scenarioLight {
		return res, schemaErr("%s: the children are not exactly attempts/full then attempts/light", devExerciseFile)
	}
	if !ValidRunID(ex.ExerciseID) {
		bad("exercise_id_invalid")
	}
	// The cost authorization.
	auth := ex.Authorization
	arow := rows.add("exercise/authorization", "Before any paid work, the owner typed exactly exercise <exercise-id> as a fresh line after the disclosure within its 20m window; a refusal, EOF or expiry started no attempt")
	arow.ref(devExerciseFile, data)
	pt, ok1 := parseTime(auth.Prompt)
	dt, ok2 := parseTime(auth.Decision)
	dl, ok3 := parseTime(auth.Deadline)
	switch {
	case !ok1 || !ok2 || !ok3 || !dl.Equal(pt.Add(OwnerDecisionBound)) || dt.Before(pt):
		arow.bad("authorization_invalid")
	case auth.Kind == authExercise && (auth.Response != authExercise+" "+ex.ExerciseID || !dt.Before(dl)):
		arow.bad("authorization_invalid")
	case auth.Kind == intakeNo && auth.Response != "no "+ex.ExerciseID, auth.Kind == intakeEOF && auth.Response != "",
		auth.Kind == intakeExpired && (auth.Response != "" || dt.Before(dl)):
		arow.bad("authorization_invalid")
	case auth.Kind != authExercise && auth.Kind != intakeNo && auth.Kind != intakeEOF && auth.Kind != intakeExpired:
		arow.bad("authorization_invalid")
	}
	if auth.Kind != authExercise {
		for _, c := range ex.Children {
			if c.Status != childNotRun || c.Stop != "owner_"+auth.Kind {
				arow.bad("unauthorized_attempt")
			}
		}
	}
	arow.see("authorization %s at %s", auth.Kind, auth.Decision)
	arow.done(bad)
	// The children, each independently checked.
	var reached []*DevBundle
	var manifests []*DevManifest
	for k, c := range ex.Children {
		crow := rows.add("exercise/attempt/"+c.Scenario, fmt.Sprintf("The %s child in %s is an independent attempt: its own bundle checks with its own canonical report and audit, its status is that check's result within its 90m deadline and 2m finalization tail, or it was not run with a recorded cause", c.Scenario, c.Dir))
		cr := DevExerciseChildResult{Dir: c.Dir, Scenario: c.Scenario, RunID: c.RunID, Status: c.Status, Stop: c.Stop}
		dir := filepath.Join(resolved, filepath.FromSlash(c.Dir))
		if c.Status == childNotRun {
			if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) || c.RunID != "" || c.ManifestSHA256 != "" || c.Stop == "" {
				crow.bad("not_run_child_inconsistent")
			}
			crow.see("not run: %s", auditText(c.Stop, 80))
			crow.done(bad)
			rep.Children = append(rep.Children, cr)
			reached = append(reached, nil)
			manifests = append(manifests, nil)
			continue
		}
		b, err := LoadDevBundle(dir)
		if err != nil {
			return res, schemaErr("%s: %v", c.Dir, err)
		}
		if b.Scenario != c.Scenario || b.ver != devBundleV2 || b.Manifest == nil {
			return res, schemaErr("%s: the child is not a %s v2 bundle", c.Dir, c.Scenario)
		}
		if b.cursor != cursorRoot {
			// A default child under a Cursor root or a Cursor child under a
			// default root: never one combined result, whatever each says.
			crow.bad(CodeCursorSelectionMismatch)
		}
		m := b.Manifest
		chk := checkDevBundle(b, false)
		cr.Mechanical = chk.report.Result
		cr.ManifestSHA256 = sha256Hex(b.Files[devManifestFile])
		cr.ReportSHA256, cr.AuditSHA256 = sha256Hex(b.Files["report.json"]), sha256Hex(b.Files["audit.json"])
		for _, n := range []string{devManifestFile, "report.json", "report.txt", "audit.json", "audit.md"} {
			if d, ok := b.Files[n]; ok {
				rep.Inventory = append(rep.Inventory, InventoryEntry{Path: c.Dir + "/" + n, Bytes: int64(len(d)), SHA256: sha256Hex(d)})
			}
			crow.ref(c.Dir+"/"+n, b.Files[n])
		}
		cost := deriveDevCost(b, m.RunID)
		cr.PreflightCalls, cr.WorkflowCalls, cr.RejectedCalls, cr.WorkerCalls, cr.Phases = len(cost.PreflightTasks), len(cost.WorkflowTasks),
			len(cost.RejectedTasks), cost.WorkerCalls, cost.PhaseStarts
		cr.ElapsedMS = msBetween(c.Start, c.End)
		start, _ := parseTime(c.Start)
		end, okE := parseTime(c.End)
		fin, okF := parseTime(c.FinalizeBy)
		deadline, _ := parseTime(c.Deadline)
		switch {
		case c.RunID != m.RunID || c.ManifestSHA256 != cr.ManifestSHA256 || c.Start != m.AttemptStart || c.Deadline != m.AttemptDeadline:
			crow.bad("child_identity_mismatch")
		case !okE || !okF || !deadline.Equal(start.Add(AttemptBound)) || fin.After(deadline.Add(CleanupBound)) || end.Before(start):
			crow.bad("child_window_invalid")
		}
		switch {
		case !end.Before(fin):
			if c.Status != childFail || c.Stop != stopFinalized {
				crow.bad(stopFinalized)
			}
		case c.Status == childPass && cr.Mechanical != ResultPass, c.Status == childFail && cr.Mechanical == ResultPass:
			crow.bad("child_status_mismatch")
		case c.Status != childPass && c.Status != childFail:
			crow.bad("child_status_mismatch")
		}
		if m.StopCode != nil && c.Stop != *m.StopCode && c.Stop != stopFinalized || m.StopCode == nil && c.Stop != "" && c.Stop != stopFinalized {
			crow.bad("child_stop_mismatch")
		}
		ceiling := 19
		if c.Scenario == scenarioLight {
			ceiling = 14
		}
		if cr.WorkerCalls > ceiling {
			crow.bad("cost_ceiling_exceeded")
		}
		crow.see("%s %s (mechanical %s), %d worker calls", cr.RunID, c.Status, cr.Mechanical, cr.WorkerCalls)
		crow.done(bad)
		rep.Children = append(rep.Children, cr)
		reached = append(reached, b)
		manifests = append(manifests, m)
		_ = k
	}
	// Order, uniqueness, harness and accounting.
	orow := rows.add("exercise/order", "The full attempt started after the cost authorization, the light attempt started only after the full attempt's cleanup, collection and check finished, both run IDs are unique and the root ended after the last reached child")
	full, light := ex.Children[0], ex.Children[1]
	if full.Status != childNotRun {
		if fs, ok := parseTime(full.Start); !ok || fs.Before(dt) {
			orow.bad("exercise_order_invalid")
		}
	}
	if full.Status != childNotRun && light.Status != childNotRun {
		fe, _ := parseTime(full.End)
		ls, _ := parseTime(light.Start)
		if ls.Before(fe) || full.RunID == light.RunID {
			orow.bad("exercise_order_invalid")
		}
	}
	if full.Status == childNotRun && light.Status != childNotRun {
		orow.bad("exercise_order_invalid")
	}
	re, okR := parseTime(ex.End)
	var lastFin time.Time
	for _, c := range ex.Children {
		if e, ok := parseTime(c.End); c.Status != childNotRun && (!ok || !okR || re.Before(e)) {
			orow.bad("exercise_order_invalid")
		}
		if f, ok := parseTime(c.FinalizeBy); c.Status != childNotRun && ok {
			lastFin = f
		}
	}
	// The root's aggregation fits the last reached child's finalization
	// tail.
	if !lastFin.IsZero() && okR && !re.Before(lastFin) {
		orow.bad(stopFinalized)
	}
	orow.done(bad)
	hrow := rows.add("exercise/harness", "Both reached children ran the root's harness executable (unchanged) and its selected flow with the same frozen role selection")
	var sel string
	for _, m := range manifests {
		if m == nil {
			continue
		}
		if m.Executable.Before != ex.Executable.Before || m.Executable.After != ex.Executable.Before || m.FlowSHA256 != ex.FlowSHA256 {
			hrow.bad("harness_mismatch")
		}
		if sel != "" && m.SelectionSHA256 != sel {
			hrow.bad("selection_mismatch")
		}
		sel = m.SelectionSHA256
		// Each Cursor child ran the root's bound executable, unchanged.
		if cursorRoot && (ex.Cursor == nil || m.Cursor == nil || m.Cursor.Path != ex.Cursor.Path || m.Cursor.BeforeSHA256 != ex.Cursor.BeforeSHA256 ||
			m.Cursor.AfterSHA256 != ex.Cursor.BeforeSHA256) {
			hrow.bad(CodeCursorExecutableMismatch)
		}
	}
	if cursorRoot {
		// The root binding: hashed before the authorization and after the
		// last reached child, unchanged.
		if ex.Cursor == nil || ex.Cursor.Path == "" || !hex64RE.MatchString(ex.Cursor.BeforeSHA256) || ex.Cursor.AfterSHA256 != ex.Cursor.BeforeSHA256 {
			hrow.bad(CodeCursorExecutableMismatch)
		} else {
			hrow.see("Cursor executable %s sha256 %s (root before and after)", auditText(ex.Cursor.Path, 120), ex.Cursor.BeforeSHA256)
		}
		// The reached children that recorded versions_checked ran one
		// Cursor build: each stored exactly one nonempty Cursor version and
		// all are byte-equal (design 12a-3c-cursor-binding). The root has
		// no version of its own; a child without the event is left to its
		// own failure and incompleteness codes.
		var versions []string
		for k, b := range reached {
			if b == nil || manifests[k] == nil || countEvents(b, func(e Event) bool { return e.Type == EvVersionsChecked }) == 0 {
				continue
			}
			v, ok := cursorStoredVersion(manifests[k].Versions)
			if !ok {
				hrow.bad(CodeCursorExecutableMismatch)
				continue
			}
			if len(versions) > 0 && v != versions[0] {
				hrow.bad(CodeCursorExecutableMismatch)
			}
			versions = append(versions, v)
		}
		if len(versions) > 0 {
			hrow.see("Cursor version %s (%d reached children that checked versions)", auditText(versions[0], 120), len(versions))
		}
	}
	hrow.done(bad)
	crow := rows.add("exercise/accounting", "The root's accounting is the sum of the children's actual derived accounting (failed, late and recovered calls included once), at most 33 worker calls, with unknown monetary cost")
	acc := &rep.Accounting
	for _, cr := range rep.Children {
		acc.PreflightCalls += cr.PreflightCalls
		acc.WorkflowCalls += cr.WorkflowCalls
		acc.RejectedCalls += cr.RejectedCalls
		acc.WorkerCalls += cr.WorkerCalls
		acc.CoordinatorPhases += cr.Phases
	}
	acc.AuthorizationWaitMS = msBetween(auth.Prompt, auth.Decision)
	acc.TotalElapsedMS = msBetween(ex.Start, ex.End)
	if acc.WorkerCalls > exerciseMaxCalls {
		crow.bad("cost_ceiling_exceeded")
	}
	crow.see("%d worker calls (%d preflight, %d workflow, %d rejected), %d coordinator phases", acc.WorkerCalls, acc.PreflightCalls, acc.WorkflowCalls,
		acc.RejectedCalls, acc.CoordinatorPhases)
	crow.done(bad)
	sort.Slice(rep.Inventory, func(i, j int) bool { return rep.Inventory[i].Path < rep.Inventory[j].Path })
	sort.Strings(rep.Codes)
	pass := len(rep.Codes) == 0 && full.Status == childPass && light.Status == childPass && rep.Children[0].Mechanical == ResultPass &&
		rep.Children[1].Mechanical == ResultPass
	if pass {
		rep.Result = ResultPass
	}
	rep.Claim = exerciseClaim(rep.Result)
	res.report = rep
	res.reportJSON, _ = encodeJSON(rep)
	res.reportText = renderExerciseReport(rep)
	a := DevExerciseAudit{Schema: auditSchema, ExerciseID: ex.ExerciseID, ReportSHA256: sha256Hex(res.reportJSON), Mechanical: rep.Result,
		Judgments: PendingReview, Rows: rows.rows}
	res.audit = a
	res.auditJSON, _ = encodeJSON(a)
	res.auditText = renderExerciseAudit(a)
	if initial {
		return res, nil
	}
	// The stored root report and audit must equal their recomputation.
	for name, want := range map[string][]byte{"report.json": res.reportJSON, "report.txt": []byte(res.reportText), "audit.json": res.auditJSON,
		"audit.md": []byte(res.auditText)} {
		if got, ok := files[name]; !ok || !bytes.Equal(got, want) {
			rep.Codes = append(slices.Clone(rep.Codes), CodeDerivedMismatch)
			sort.Strings(rep.Codes)
			rep.Codes = slices.Compact(rep.Codes)
			rep.Result, rep.Claim = ResultFail, exerciseClaim(ResultFail)
			res.report = rep
			res.reportJSON, _ = encodeJSON(rep)
			res.reportText = renderExerciseReport(rep)
			break
		}
	}
	return res, nil
}

// cursorStoredVersion is the complete stored Cursor version of a version
// list, exactly as written (no parsing or normalization): false when the
// list holds no Cursor entry, more than one, or an empty one.
func cursorStoredVersion(versions []VersionRecord) (string, bool) {
	var found []string
	for _, v := range versions {
		if v.Tool == adapter.CursorID {
			found = append(found, v.Version)
		}
	}
	if len(found) != 1 || strings.TrimSpace(found[0]) == "" {
		return "", false
	}
	return found[0], true
}

// exerciseClaim states what a combined result demonstrates.
func exerciseClaim(result string) string {
	if result != ResultPass {
		return "not demonstrated"
	}
	return "both independent attempts (full then light) of one authorized combined exercise passed their mechanical and deterministic criteria; each child's judgment rows still need its own external audit-review.md"
}

// exerciseRows accumulates the root's audit rows.
type exerciseRows struct{ rows []DevAuditRow }

// exerciseRow is one root row under construction.
type exerciseRow struct {
	set *exerciseRows
	r   DevAuditRow
	obs []string
}

// add starts root row id.
func (s *exerciseRows) add(id, requirement string) *exerciseRow {
	return &exerciseRow{set: s, r: DevAuditRow{ID: id, Layer: LayerMechanical, Criterion: "exercise", Requirement: requirement, Sources: []AuditRef{},
		Codes: []string{}}}
}

// ref cites a root-relative (attempt-prefixed) file.
func (r *exerciseRow) ref(rel string, data []byte) {
	if data == nil {
		return
	}
	r.r.Sources = append(r.r.Sources, AuditRef{File: rel, SHA256: sha256Hex(data), Lines: []int{}})
}

func (r *exerciseRow) bad(code string) {
	if !slices.Contains(r.r.Codes, code) {
		r.r.Codes = append(r.r.Codes, code)
	}
}

func (r *exerciseRow) see(format string, a ...any) { r.obs = append(r.obs, fmt.Sprintf(format, a...)) }

// done records the row and reports its codes to the root.
func (r *exerciseRow) done(bad func(string)) {
	sort.Strings(r.r.Codes)
	r.r.Observation = cmpOr(strings.Join(r.obs, "; "), "nothing observed")
	r.r.Verdict = VerdictCorrect
	if len(r.r.Codes) > 0 {
		r.r.Verdict = VerdictIncorrect
		r.r.Correction = "The root evidence contradicts this requirement (" + strings.Join(r.r.Codes, ", ") + "); the combined result is a mechanical FAIL."
		for _, c := range r.r.Codes {
			bad(c)
		}
	}
	r.set.rows = append(r.set.rows, r.r)
}

// renderExerciseReport is the root's report.txt.
func renderExerciseReport(r DevExerciseReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "result: %s (combined full and light exercise)\n%s\n", r.Result, JudgmentReportLine)
	fmt.Fprintf(&b, "exercise: %s\nclaim: %s\n", r.ExerciseID, r.Claim)
	for _, c := range r.Children {
		fmt.Fprintf(&b, "child %s (%s): %s", c.Scenario, c.Dir, c.Status)
		if c.RunID != "" {
			fmt.Fprintf(&b, ", run %s, mechanical %s, %d worker calls", c.RunID, c.Mechanical, c.WorkerCalls)
		}
		if c.Stop != "" {
			fmt.Fprintf(&b, ", stop %s", c.Stop)
		}
		b.WriteString("\n")
	}
	a := r.Accounting
	fmt.Fprintf(&b, "worker calls: %d (%d preflight, %d workflow, %d rejected; at most %d, happy path %d); coordinator phases: %d; provider usage: %s\n",
		a.WorkerCalls, a.PreflightCalls, a.WorkflowCalls, a.RejectedCalls, a.MaxWorkerCalls, a.HappyPathCalls, a.CoordinatorPhases, a.ProviderUsage)
	for _, c := range r.Codes {
		fmt.Fprintf(&b, "root code: %s\n", c)
	}
	fmt.Fprintf(&b, "inventory: %d root-referenced files (names and SHA-256 in report.json)\n", len(r.Inventory))
	return b.String()
}

// renderExerciseAudit is the root's audit.md.
func renderExerciseAudit(a DevExerciseAudit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Callsheet combined exercise audit\n\nExercise %s; report sha256 %s; mechanical %s; %s\n\n", a.ExerciseID, a.ReportSHA256, a.Mechanical,
		a.Judgments)
	for _, r := range a.Rows {
		fmt.Fprintf(&b, "## %s\n\n- requirement: %s\n- verdict: %s\n- observation: %s\n", r.ID, r.Requirement, r.Verdict, r.Observation)
		for _, s := range r.Sources {
			fmt.Fprintf(&b, "- source: %s sha256 %s\n", s.File, s.SHA256)
		}
		if r.Correction != "" {
			fmt.Fprintf(&b, "- correction: %s\n", r.Correction)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// CheckExercise checks a combined exercise root offline and writes its
// report: 0 combined PASS, 1 FAIL or NOT_RUN, 2 malformed or unsupported.
func CheckExercise(root string, out, errOut io.Writer) int {
	res, err := checkExerciseRoot(root, false)
	if err != nil {
		fmt.Fprintf(errOut, "reale2e check: %v\n", err)
		data, _ := encodeJSON(DevExerciseReport{Schema: devExerciseReportSchema, Result: ResultFail, Claim: exerciseClaim(ResultFail), Judgments: PendingReview,
			Children: []DevExerciseChildResult{}, Codes: []string{}, Inventory: []InventoryEntry{}, Error: "evidence_schema_error"})
		out.Write(data)
		return 2
	}
	out.Write(res.reportJSON)
	if res.report.Result == ResultPass {
		return 0
	}
	return 1
}
