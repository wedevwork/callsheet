package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The full-flow session event map (design 12a-dev-flow-verify, Replay
// and audit). One pure derivation, used by the collector after the final
// phase observation and repeated by the offline checker before it reads
// the stored map: for every phase, its file hash, init records (each must
// keep the session and report auto), its result and the stream problem;
// for every reached task, the MCP dispatch tool use and its response
// (reconciled with a task_ls recovery after a lost reply), and the one
// background wait's start, handle, automatic notification and consuming
// task_show, each with its inclusive source lines. Transcripts are kept
// for diagnosis only; they never repair the print stream.

// DevEventMap is session/event-map.json.
type DevEventMap struct {
	Schema    string          `json:"schema"`
	RunID     string          `json:"run_id"`
	SessionID string          `json:"session_id"`
	Phases    []DevPhaseFacts `json:"phases"`
	Tasks     []DevTaskFacts  `json:"tasks"`
}

// DevPhaseFacts is one phase stream's facts.
type DevPhaseFacts struct {
	Phase      int         `json:"phase"`
	File       string      `json:"file"`
	SHA256     string      `json:"sha256"`
	SessionID  string      `json:"session_id"`
	Inits      []SourceRef `json:"inits"`
	Result     *SourceRef  `json:"result"`
	ResultText string      `json:"result_text"`
	ResultOK   bool        `json:"result_ok"`
	Problem    string      `json:"problem"`
	// ProblemSource is the refused notification record of a
	// wake_representation_unsupported problem (design 12a-dev-flow-verify
	// r0.8).
	ProblemSource *SourceRef `json:"problem_source,omitempty"`
}

// DevTaskFacts is one reached task's coordinator facts.
type DevTaskFacts struct {
	Ordinal     int         `json:"ordinal"`
	Task        string      `json:"task"`
	Phase       int         `json:"phase"`
	DispatchUse string      `json:"dispatch_tool_use"`
	Dispatch    []SourceRef `json:"dispatch"`
	Recovered   bool        `json:"recovered"`
	Wait        devWait     `json:"wait"`
	WaitSources []SourceRef `json:"wait_sources"`
	Problem     string      `json:"problem"`
	// ProblemSource is the refused notification record of a
	// wake_representation_unsupported problem (r0.8).
	ProblemSource *SourceRef `json:"problem_source,omitempty"`
}

// devMapTask is one reached task's derivation input.
type devMapTask struct {
	ordinal int
	task    string
	doc     *devDispatchDoc
	auth    *contract.TaskView
	waitCmd string
	phase   int
}

// devMapInput is the derivation's whole input (goos: the MCP server's
// platform, for its path rules).
type devMapInput struct {
	runID   string
	goos    string
	streams map[int][]byte
	phases  int
	tasks   []devMapTask
	// waitPrefix is the slot commands' prefix (devWaitPrefixOf).
	waitPrefix string
	// schema is the event map's schema in the bundle's evidence version.
	schema string
}

// devStreamFile is phase p's bundle stream path.
func devStreamFile(p int) string { return fmt.Sprintf("session/phase-%d.stdout.jsonl", p) }

// toolTaskList is the MCP task listing a lost dispatch reply is recovered
// with.
const toolTaskList = "mcp__callsheet__task_ls"

// Event-map problem codes.
const (
	devDispatchUnobserved = "dispatch_unobserved"
	devDispatchMismatch   = "dispatch_mismatch"
	devDispatchOrder      = "dispatch_order_invalid"
	devMCPCallInvalid     = "mcp_call_invalid"
)

// deriveDevEventMap is the pure derivation.
func deriveDevEventMap(in devMapInput) DevEventMap {
	em := DevEventMap{Schema: cmpOr(in.schema, devKind("event-map")), RunID: in.runID, Phases: []DevPhaseFacts{}, Tasks: []DevTaskFacts{}}
	records := map[int][]sessionRecord{}
	for p := 1; p <= in.phases; p++ {
		file := devStreamFile(p)
		data := in.streams[p]
		s := decodeDevStreamFor(file, data, em.SessionID, in.waitPrefix)
		if em.SessionID == "" {
			em.SessionID = s.sessionID
		}
		f := DevPhaseFacts{Phase: p, File: file, SHA256: sha256Hex(data), SessionID: s.sessionID, Inits: []SourceRef{}, ResultText: s.resultText,
			ResultOK: s.hasResult && s.resultOK, Problem: s.problem}
		if s.problem == devWakeUnsupported && s.problemSrc.File != "" {
			ps := s.problemSrc
			f.ProblemSource = &ps
		}
		for _, r := range s.records {
			switch r.kind {
			case "init":
				f.Inits = append(f.Inits, r.src)
			case "result":
				src := r.src
				f.Result = &src
			}
		}
		if len(f.Inits) == 0 && f.Problem == "" {
			f.Problem = devAutoModeMissing
		}
		records[p] = s.records
		em.Phases = append(em.Phases, f)
	}
	// consumed is each phase's last consuming task_show line so far.
	consumed := map[int]int{}
	for _, t := range in.tasks {
		tf := DevTaskFacts{Ordinal: t.ordinal, Task: t.task, Phase: t.phase, Dispatch: []SourceRef{}, WaitSources: []SourceRef{}}
		recs := records[t.phase]
		tf.DispatchUse, tf.Dispatch, tf.Recovered, tf.Problem = dispatchFacts(recs, t.doc, t.auth, t.task)
		w, src, problem := deriveDevWait(recs, t.waitCmd, t.task)
		tf.Wait = w
		// Source order: the predecessor's consumption, then this dispatch
		// and its response, then this task's wait.
		if n := len(tf.Dispatch); tf.Problem == "" && n > 0 {
			if tf.Dispatch[0].FirstLine <= consumed[t.phase] || src.start.File != "" && src.start.FirstLine <= tf.Dispatch[n-1].LastLine {
				tf.Problem = devDispatchOrder
			}
		}
		if src.consume.File != "" {
			consumed[t.phase] = src.consume.LastLine
		}
		if tf.Problem == "" {
			tf.Problem = problem
			if problem == devWakeUnsupported && src.refused.File != "" {
				ps := src.refused
				tf.ProblemSource = &ps
			}
		}
		for _, s := range []SourceRef{src.start, src.handle, src.wake, src.consume} {
			if s.File != "" {
				tf.WaitSources = append(tf.WaitSources, s)
			}
		}
		em.Tasks = append(em.Tasks, tf)
	}
	return em
}

// optional is s as an optional argument (absent when empty).
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// expectedDispatchArgs is the one MCP dispatch input the prepared document
// allows: its target, goal, acceptance and workspace selection, with no
// payload, no per-task override and no wait (the asynchronous dispatch).
func expectedDispatchArgs(doc *devDispatchDoc) contract.DispatchArgs {
	var a contract.DispatchArgs
	a.Target.Kind, a.Target.Value = doc.Target.Kind, doc.Target.Value
	a.Goal, a.Acceptance = doc.Goal, doc.Acceptance
	a.Workspace, a.Base, a.WorkspaceInstance = optional(doc.Workspace), optional(doc.Base), optional(doc.Instance)
	return a
}

// sameDispatchArgs reports whether a recorded input is exactly the
// expected one, as whole objects (an explicit empty payload is the absent
// payload the server reads it as).
func sameDispatchArgs(got, want contract.DispatchArgs) bool {
	if len(got.Payload) == 0 {
		got.Payload = nil
	}
	return bytes.Equal(mustJSON(got), mustJSON(want))
}

// taskIdentity is a task view's immutable identity: every field fixed at
// admission (the task ID, the complete request with its attribution, the
// resolved role, the effective selection and timeout policy, the creation
// time and the workspace binding). The dynamic fields are excluded,
// because they legitimately change between the dispatch answer and the
// terminal record: the lifecycle state with its flags and times (state,
// stop_requested, started_at, finished_at, elapsed_ms, reconciling,
// completion_pending, persistence_reason, durability_confirmed, reason),
// the scheduling candidates, the log metadata, tail and truncation, the
// result and late result, and the workspace phase.
type taskIdentity struct {
	TaskID           string                     `json:"task_id"`
	Request          contract.DispatchRequest   `json:"request"`
	Role             contract.TaskRole          `json:"role"`
	Effective        contract.TaskEffective     `json:"effective"`
	TimeoutPolicy    string                     `json:"timeout_policy"`
	CreatedAt        string                     `json:"created_at"`
	WorkspaceBinding *contract.WorkspaceBinding `json:"workspace_binding"`
}

// identityOf is v's immutable identity.
func identityOf(v contract.TaskView) taskIdentity {
	return taskIdentity{TaskID: v.TaskID, Request: v.Request, Role: v.Role, Effective: v.Effective, TimeoutPolicy: v.TimeoutPolicy,
		CreatedAt: v.CreatedAt, WorkspaceBinding: v.WorkspaceBinding}
}

// identityMatches reports whether a recorded task view is task's: its
// request is exactly the one the prepared document determines (its
// attribution aside, which the session supplies), its workspace binding
// the document's selection, and, when the authoritative plane record is
// known, its whole immutable identity equal to that record's.
func identityMatches(v contract.TaskView, task string, doc *devDispatchDoc, auth *contract.TaskView) bool {
	want := contract.DispatchRequest{Target: doc.Target, Goal: doc.Goal, Acceptance: doc.Acceptance, Payload: []string{}, RequestedBy: v.Request.RequestedBy,
		Workspace: optional(doc.Workspace), Base: optional(doc.Base), WorkspaceInstance: optional(doc.Instance)}
	var binding *contract.WorkspaceBinding
	if doc.Workspace != "" {
		base := doc.Base
		binding = &contract.WorkspaceBinding{Name: doc.Workspace, Instance: doc.Instance, BaseSelector: doc.Base, BaseCommit: &base}
	}
	got := identityOf(v)
	switch {
	case v.TaskID != task || !bytes.Equal(mustJSON(got.Request), mustJSON(want)) || !bytes.Equal(mustJSON(got.WorkspaceBinding), mustJSON(binding)):
		return false
	case auth != nil && !bytes.Equal(mustJSON(got), mustJSON(identityOf(*auth))):
		return false
	}
	return true
}

// summaryMatches reports whether a task_ls entry is task's: its
// immutable identity (task ID, target, role, effective selection,
// creation time and attribution) equal to the authoritative record's, or,
// without one, its target the document's. Its state, stop flag, times,
// reconciliation and persistence fields are dynamic and excluded.
func summaryMatches(s contract.TaskSummary, task string, doc *devDispatchDoc, auth *contract.TaskView) bool {
	if s.TaskID != task || s.Target != doc.Target {
		return false
	}
	if auth == nil {
		return true
	}
	type id struct {
		Target      contract.TaskTarget
		Role        contract.TaskRole
		Effective   contract.TaskEffective
		CreatedAt   string
		RequestedBy contract.RequestedBy
	}
	return bytes.Equal(mustJSON(id{s.Target, s.Role, s.Effective, s.CreatedAt, s.RequestedBy}),
		mustJSON(id{auth.Request.Target, auth.Role, auth.Effective, auth.CreatedAt, auth.Request.RequestedBy}))
}

// dispatchFacts finds the MCP dispatch of the prepared document doc
// (matched by its goal) over the whole phase: exactly one tool use whose
// input, decoded by the MCP server's own rule, is exactly the expected
// input; its strict asynchronous dispatch response whose task identity is
// the document's and the authoritative record's (auth, when known); or,
// after any non-conflicting error response or no response (a lost reply),
// a later task_ls whose strict listing holds task with that identity. A
// second dispatch of the goal anywhere in the phase, and an error-flagged
// answer that strictly parses as the success response (a conflict, never
// recovered), are mismatches.
func dispatchFacts(recs []sessionRecord, doc *devDispatchDoc, auth *contract.TaskView, task string) (use string, src []SourceRef, recovered bool, problem string) {
	src = []SourceRef{}
	if doc == nil {
		return "", src, false, devDispatchUnobserved
	}
	want := expectedDispatchArgs(doc)
	var answered, duplicate, differs, lost bool
	lists := map[string]pageRequest{}
	// replied are the tool uses with a recorded result: a recovery listing
	// is requested only after its dispatch's error answer, or after the
	// dispatch when its answer is absent.
	replied := map[string]bool{}
	for _, r := range recs {
		for _, b := range r.blocks {
			if r.kind == "user" && b.typ == "tool_result" {
				replied[b.toolUseID] = true
			}
		}
	}
	for _, r := range recs {
		for _, b := range r.blocks {
			switch {
			case use != "" && !answered && (lost || !replied[use]) && r.kind == "assistant" && b.typ == "tool_use" && b.name == toolTaskList:
				if after, limit, err := contract.ParseTaskListArgs(b.input); err == nil {
					lists[b.id] = pageRequest{after: after, limit: limit}
				}
			case r.kind == "assistant" && b.typ == "tool_use" && b.name == toolDispatch:
				var id struct {
					Goal string `json:"goal"`
				}
				if json.Unmarshal(b.input, &id) != nil || id.Goal != doc.Goal {
					continue
				}
				if use != "" {
					duplicate = true
					continue
				}
				in, err := contract.DecodeDispatchArgs(b.input)
				use, differs = b.id, err != nil || !sameDispatchArgs(in, want)
				src = append(src, r.src)
			case !answered && r.kind == "user" && b.typ == "tool_result" && use != "" && b.toolUseID == use && len(src) == 1:
				src = append(src, r.src)
				if b.isError {
					// An error-flagged answer that strictly parses as the
					// success response is a conflict, never a lost reply.
					if _, err := contract.ParseDispatchResponse([]byte(b.text)); err == nil {
						differs = true
						answered = true
						continue
					}
					lost = true
					continue
				}
				v, err := contract.ParseDispatchResponse([]byte(b.text))
				if err != nil || !identityMatches(v, task, doc, auth) {
					differs = true
				}
				answered = true
			case use != "" && !answered && r.kind == "user" && b.typ == "tool_result" && !b.isError:
				req, listed := lists[b.toolUseID]
				if !listed {
					continue
				}
				page, ok := pageAnswers(b.text, req)
				if !ok {
					// Not the server's answer to its own page request.
					differs = true
					continue
				}
				for _, s := range page {
					if s.TaskID != task {
						continue
					}
					src = append(src, r.src)
					answered, recovered = true, true
					differs = differs || !summaryMatches(s, task, doc, auth)
				}
			}
		}
	}
	switch {
	case use == "":
		return "", []SourceRef{}, false, devDispatchUnobserved
	case duplicate || differs:
		return use, src, recovered, devDispatchMismatch
	case !answered:
		return use, src, false, devDispatchUnobserved
	}
	return use, src, recovered, ""
}

// pageRequest is one task_ls call's validated page request.
type pageRequest struct {
	after string
	limit int
}

// pageAnswers reports whether a listing page is the answer the server
// gives to req: strictly decoded (ascending unique IDs, next_after the
// last ID), at most limit entries, every ID after the exclusive cursor.
func pageAnswers(text string, req pageRequest) ([]contract.TaskSummary, bool) {
	page, _, err := contract.ParseTaskListResponse([]byte(text))
	if err != nil || len(page) > req.limit {
		return nil, false
	}
	for _, s := range page {
		if s.TaskID <= req.after {
			return nil, false
		}
	}
	return page, true
}
