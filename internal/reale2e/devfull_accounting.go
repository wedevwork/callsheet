package reale2e

import (
	"bytes"
	"slices"
	"sort"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Admission sweeps and accounting (design 12a-dev-flow-verify, Bundle and
// checker, Accounting and no damage): the sweep records the collector
// writes, the pure cost derivation the collector writes and the checker
// repeats, and the event-map derivation's input from a bundle.

// DevSweepPage is one ListTasks page: its cursor, the listed IDs and the
// next cursor ("" at the end).
type DevSweepPage struct {
	After string   `json:"after"`
	IDs   []string `json:"ids"`
	Next  string   `json:"next"`
}

// DevSweepTask is one listed task's captured request identity.
type DevSweepTask struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	CreatedAt   string `json:"created_at"`
	RequestedBy string `json:"requested_by"`
}

// DevSweep is one complete sweep (or a run of identical complete sweeps,
// coalesced to their first and last times and count), or one sweep a
// transport error closed.
type DevSweep struct {
	Kind  string         `json:"kind"`
	First string         `json:"first"`
	Last  string         `json:"last"`
	Count int            `json:"count"`
	Pages []DevSweepPage `json:"pages"`
	Tasks []DevSweepTask `json:"tasks"`
	Error string         `json:"error"`
}

// DevAdmissions is admissions.json.
type DevAdmissions struct {
	Schema string     `json:"schema"`
	RunID  string     `json:"run_id"`
	Sweeps []DevSweep `json:"sweeps"`
}

// Sweep kinds.
const (
	sweepMonitor = "monitor"
	sweepFinal   = "final"
)

// sweepRecorder accumulates the sweeps.
type sweepRecorder struct {
	clock  Clock
	sweeps []DevSweep
}

// sweep is one sweep under way.
type sweep struct {
	rec   *sweepRecorder
	s     DevSweep
	tasks map[string]DevSweepTask
}

// begin starts a sweep of kind.
func (r *sweepRecorder) begin(kind string) *sweep {
	at := formatTime(r.clock.Now())
	return &sweep{rec: r, s: DevSweep{Kind: kind, First: at, Last: at, Count: 1, Pages: []DevSweepPage{}, Tasks: []DevSweepTask{}},
		tasks: map[string]DevSweepTask{}}
}

// page records one page.
func (w *sweep) page(after string, page []contract.TaskSummary, next *string) {
	p := DevSweepPage{After: after, IDs: []string{}}
	if next != nil {
		p.Next = *next
	}
	for _, t := range page {
		p.IDs = append(p.IDs, t.TaskID)
		w.tasks[t.TaskID] = DevSweepTask{ID: t.TaskID, Role: t.Role.ID, CreatedAt: t.CreatedAt, RequestedBy: t.RequestedBy.Name}
	}
	w.s.Pages = append(w.s.Pages, p)
}

// end closes the sweep (err: a transport error closed it) and appends it,
// coalescing it into an identical previous complete sweep.
func (w *sweep) end(err error) {
	for _, id := range sortedTaskIDs(w.tasks) {
		w.s.Tasks = append(w.s.Tasks, w.tasks[id])
	}
	if err != nil {
		w.s.Error = contract.SafeText(err.Error(), 200)
	}
	r := w.rec
	if n := len(r.sweeps); n > 0 && err == nil && r.sweeps[n-1].Error == "" && r.sweeps[n-1].Kind == w.s.Kind &&
		bytes.Equal(mustJSON(r.sweeps[n-1].Pages), mustJSON(w.s.Pages)) && bytes.Equal(mustJSON(r.sweeps[n-1].Tasks), mustJSON(w.s.Tasks)) {
		r.sweeps[n-1].Count++
		r.sweeps[n-1].Last = w.s.First
		return
	}
	r.sweeps = append(r.sweeps, w.s)
}

func sortedTaskIDs(m map[string]DevSweepTask) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DevTaskCost is one admitted task's accounting.
type DevTaskCost struct {
	Task       string `json:"task"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	State      string `json:"state"`
	DurationMS *int64 `json:"duration_ms"`
}

// DevPhaseCost is one coordinator phase's duration.
type DevPhaseCost struct {
	Phase      int    `json:"phase"`
	DurationMS *int64 `json:"duration_ms"`
}

// DevCost is cost.json: derived accounting, never an estimate. Provider
// usage is copied only when provider-reported; this harness records none,
// so it is unknown (never zero).
type DevCost struct {
	Schema            string         `json:"schema"`
	RunID             string         `json:"run_id"`
	PreflightTasks    []string       `json:"preflight_tasks"`
	WorkflowTasks     []string       `json:"workflow_tasks"`
	RejectedTasks     []string       `json:"rejected_tasks"`
	PhaseStarts       int            `json:"coordinator_phase_starts"`
	QualificationRefs []string       `json:"qualification_refs"`
	WorkerCalls       int            `json:"worker_calls"`
	Completed         int            `json:"completed"`
	Failed            int            `json:"failed"`
	Tasks             []DevTaskCost  `json:"tasks"`
	Phases            []DevPhaseCost `json:"phases"`
	OwnerWaitMS       int64          `json:"owner_wait_ms"`
	AttemptElapsedMS  *int64         `json:"attempt_elapsed_ms"`
	CleanupElapsedMS  *int64         `json:"cleanup_elapsed_ms"`
	ProviderUsage     string         `json:"provider_usage"`
}

// usageUnknown is the provider usage this harness can attest.
const usageUnknown = "unknown"

// msBetween is the milliseconds from a to b (nil when either is unusable).
func msBetween(a, b string) *int64 {
	s, ok1 := parseTime(a)
	e, ok2 := parseTime(b)
	if !ok1 || !ok2 || e.Before(s) {
		return nil
	}
	ms := e.Sub(s).Milliseconds()
	return &ms
}

// deriveDevCost is the pure accounting of a bundle: every admitted task
// (preflights, workflow tasks and every rejected or late admission,
// failed paid calls included), each phase's duration, the owner windows,
// the attempt and the cleanup.
func deriveDevCost(b *DevBundle, runID string) DevCost {
	c := DevCost{Schema: b.kind("cost"), RunID: runID, PreflightTasks: []string{}, WorkflowTasks: []string{}, RejectedTasks: []string{},
		QualificationRefs: []string{}, Tasks: []DevTaskCost{}, Phases: []DevPhaseCost{}, ProviderUsage: usageUnknown}
	addView := func(kind, label string, v *contract.TaskShowResponse) {
		tc := DevTaskCost{Task: v.Task.TaskID, Kind: kind, Label: label, State: v.Task.State}
		if d, ok := duration(v.Task); ok {
			ms := d.Milliseconds()
			tc.DurationMS = &ms
		}
		c.Tasks = append(c.Tasks, tc)
		if v.Task.State == contract.TaskSucceeded {
			c.Completed++
		} else {
			c.Failed++
		}
	}
	for _, n := range b.ordinalsOf("preflight") {
		if v := doc[contract.TaskShowResponse](b, "preflight/"+hopDir(n-1)+"/task.json"); v != nil {
			c.PreflightTasks = append(c.PreflightTasks, v.Task.TaskID)
			addView("preflight", preflightHop(n-1), v)
		}
	}
	workflow := func(dir, label string) {
		if v := doc[contract.TaskShowResponse](b, dir+"/task.json"); v != nil {
			c.WorkflowTasks = append(c.WorkflowTasks, v.Task.TaskID)
			addView("workflow", label, v)
		}
	}
	for _, n := range b.ordinalsOf("tasks") {
		workflow(taskDir(n), workflowLabel(n))
	}
	// A Cursor diagnostic probe's one supervisor-owned coder task (design
	// 12a-3-cursor-worker; only its closed layout admits the directory).
	workflow(probeCoderDir, probeCoderLabel)
	// A qualification record's workflow calls: its wakes, then its
	// reviewer probe.
	for _, n := range b.ordinalsOf("probe/wake") {
		workflow("probe/wake/"+hopDir(n-1), probeWakeNamespace+"-"+hopDir(n-1))
	}
	workflow("probe/reviewer/01", probeReviewNamespace+"-01")
	// A light or both qualification's triage and light reviewer probes.
	workflow(probeTriageDir, probeTriageNamespace+"-01")
	workflow(probeLightReviewDir, probeLightReviewNamespace+"-01")
	settled := map[string]string{}
	if cl := doc[devEnvelope[DevCleanup]](b, devCleanupFile); cl != nil {
		for _, s := range cl.Data.Settled {
			settled[s.Task] = s.State
			// A workflow call stopped in flight has no stored task view: its
			// settlement at cleanup accounts it (a paid call is never
			// hidden).
			isWorkflow := strings.HasPrefix(s.Label, "workflow-") || strings.HasPrefix(s.Label, probeWakeNamespace+"-") ||
				strings.HasPrefix(s.Label, probeReviewNamespace+"-") || strings.HasPrefix(s.Label, probeTriageNamespace+"-") ||
				strings.HasPrefix(s.Label, probeLightReviewNamespace+"-") || s.Label == probeCoderLabel
			if isWorkflow && !slices.Contains(c.WorkflowTasks, s.Task) {
				c.WorkflowTasks = append(c.WorkflowTasks, s.Task)
				c.Tasks = append(c.Tasks, DevTaskCost{Task: s.Task, Kind: "workflow", Label: s.Label, State: s.State, DurationMS: s.DurationMS})
				if s.State == contract.TaskSucceeded {
					c.Completed++
				} else {
					c.Failed++
				}
			}
		}
	}
	var phaseStart map[string]string
	var prompt string
	phaseStart = map[string]string{}
	var preflightAt, collectAt string
	for _, e := range b.Events {
		switch e.Type {
		case EvExtraAdmission:
			if !slices.Contains(c.RejectedTasks, e.Task) {
				c.RejectedTasks = append(c.RejectedTasks, e.Task)
				tc := DevTaskCost{Task: e.Task, Kind: "rejected", Label: e.Code, State: cmpOr(settled[e.Task], "unknown")}
				c.Tasks = append(c.Tasks, tc)
				if tc.State == contract.TaskSucceeded {
					c.Completed++
				} else {
					c.Failed++
				}
			}
		case EvPhaseStarted:
			c.PhaseStarts++
			phaseStart[e.Handle] = e.Time
		case EvPhaseExited:
			n := len(c.Phases) + 1
			c.Phases = append(c.Phases, DevPhaseCost{Phase: n, DurationMS: msBetween(phaseStart[e.Handle], e.Time)})
		case EvAwaitingOwner:
			prompt = e.Time
		case EvOwnerDecision:
			if d := msBetween(prompt, e.Time); d != nil {
				c.OwnerWaitMS += *d
			}
			prompt = ""
		case EvPreflightStarted:
			preflightAt = e.Time
		case EvCollectionStarted:
			collectAt = e.Time
			if prompt != "" {
				// A window still open when the attempt stopped.
				if d := msBetween(prompt, e.Time); d != nil {
					c.OwnerWaitMS += *d
				}
				prompt = ""
			}
		}
	}
	c.AttemptElapsedMS = msBetween(preflightAt, collectAt)
	if cl := doc[devEnvelope[DevCleanup]](b, devCleanupFile); cl != nil {
		c.CleanupElapsedMS = msBetween(cl.Data.StartedAt, cl.Data.FinishedAt)
	}
	c.WorkerCalls = len(c.PreflightTasks) + len(c.WorkflowTasks) + len(c.RejectedTasks)
	return c
}

// sessionMapInput is the event-map derivation input from a bundle: the
// phase streams, and every observed task with its plan's goal, its phase
// and its exact wait command (from the manifest's recorded render values).
func sessionMapInput(b *DevBundle, runID, goos, reale2e, runtime string) devMapInput {
	in := devMapInput{runID: runID, goos: goos, streams: map[int][]byte{}, waitPrefix: devWaitPrefixOf(reale2e, runtime), schema: b.kind("event-map")}
	for p := 1; p <= maxPhases; p++ {
		if data, ok := b.Files[devStreamFile(p)]; ok {
			in.streams[p], in.phases = data, p
		}
	}
	for _, n := range b.ordinalsOf("tasks") {
		obs := doc[DevObservation](b, taskDir(n)+"/observation.json")
		plan := doc[devEnvelope[TaskPlan]](b, taskDir(n)+"/plan.json")
		if obs == nil || plan == nil {
			continue
		}
		dd := doc[devDispatchDoc](b, taskDir(n)+"/dispatch.json")
		if dd == nil {
			// No prepared document: match the plan's goal, never a request.
			dd = &devDispatchDoc{Goal: plan.Data.Goal, Acceptance: "\x00"}
		}
		var auth *contract.TaskView
		if v := doc[contract.TaskShowResponse](b, taskDir(n)+"/task.json"); v != nil {
			auth = &v.Task
		}
		in.tasks = append(in.tasks, devMapTask{ordinal: n, task: obs.Task, doc: dd, auth: auth, phase: obs.Phase,
			waitCmd: devWaitCommand(reale2e, runtime, obs.Task, n)})
	}
	return in
}
