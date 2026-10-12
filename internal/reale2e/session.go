package reale2e

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Machine-derived session evidence (design 12a-real-e2e, amendment A3):
// a bounded local decoder of the coordinator's raw phase streams (Claude
// print-mode stream-json) and its read-only transcript snapshots, derived
// into a version 2 event map with source references. Facts come only from
// structure: tool-use IDs and their linked results, init and result
// records, the supervisor's own events. An assistant claiming something
// is never evidence. Malformed, duplicated-key, conflicting or dangling
// records needed for a fact make it unobserved and are findings.
//
// Three vendor behaviours are undocumented assumptions, observed per run
// and failing closed: several background-completion turns within one -p
// phase (phase_result_early otherwise), the phase-1 transcript being an
// exact prefix of the phase-2 transcript (session_evidence_unobserved
// otherwise) and the background-completion notification's shape. The
// accepted notification is a user-role text that is exactly one
// <task-notification> element whose children are <task-id>,
// <tool-use-id>, <status> and optionally <output-file> and <summary>; any
// other text mentioning <task-notification> is
// wake_representation_unsupported. The background handle is read from the
// linked Bash result "Command running in background with ID: <id>".

// Fixed session file names.
var (
	phaseStreamFiles     = [2]string{"session/phase-1.stdout.jsonl", "session/phase-2.stdout.jsonl"}
	phaseStderrFiles     = [2]string{"session/phase-1.stderr.txt", "session/phase-2.stderr.txt"}
	transcriptFiles      = [2]string{"session/transcript-phase-1.jsonl", "session/transcript-phase-2.jsonl"}
	launchFiles          = [2]string{"setup/launch-phase-1.json", "setup/launch-phase-2.json"}
	coordinatorExitFile  = "session/coordinator-exit.json"
	eventMapFile         = "session/event-map.json"
	maxSessionRecordSize = 4 << 20
)

// Callsheet MCP tool names as the coordinator sees them (server
// "callsheet").
const (
	mcpServerName = "callsheet"
	toolDispatch  = "mcp__callsheet__dispatch"
	toolTaskShow  = "mcp__callsheet__task_show"
	toolBash      = "Bash"
)

// Session finding codes.
const (
	CodeSessionUnobserved = "session_evidence_unobserved"
	CodeSessionMalformed  = "session_evidence_malformed"
	CodeSessionMismatch   = "session_mismatch"
	CodeWakeUnsupported   = "wake_representation_unsupported"
	CodePhaseEarly        = "phase_result_early"
	CodeToolDenied        = "coordinator_tool_denied"
	CodeToolFailed        = "coordinator_tool_failed"
	CodeUserInput         = "coordinator_user_input"
	CodeDispatchMismatch  = "dispatch_identity_mismatch"
	CodeWaitMismatch      = "wait_command_mismatch"
	CodeLongPoll          = "coordinator_long_poll"
)

// toolTaskWait is the MCP long-poll the automatic attempt never uses.
const toolTaskWait = "mcp__callsheet__task_wait"

// SessionInput is everything a derivation reads.
type SessionInput struct {
	RunID       string
	Streams     [2][]byte
	Transcripts [2][]byte
	Launches    [2]*LaunchRecord
	Events      []Event
}

// sessionBlock is one content block.
type sessionBlock struct {
	typ       string
	text      string
	id        string
	name      string
	input     json.RawMessage
	toolUseID string
	isError   bool
}

// sessionRecord is one decoded record of a stream or transcript.
type sessionRecord struct {
	src       SourceRef
	kind      string // init, assistant, user, result, other
	sessionID string
	cwd       string
	blocks    []sessionBlock
	userText  string
	hasText   bool
	init      InitRecord
	resultOK  bool
	// malformed: a result's required field has the wrong type or is
	// absent (is_error, subtype; session_id when present).
	malformed bool
	denials   []string
	phase     int
}

// deriver holds one derivation's state.
type deriver struct {
	in       SessionInput
	sid      string
	findings []MapFinding
	entries  []MapEntry
	codes    map[string]bool
	init     []InitRecord
	// streamResults is each phase stream's recordsResult.
	streamResults [2][2]bool
}

func (d *deriver) find(code, toolUse, tool string, src ...SourceRef) {
	key := code + "\x00" + toolUse
	if d.codes[key] {
		return
	}
	d.codes[key] = true
	if src == nil {
		src = []SourceRef{}
	}
	d.findings = append(d.findings, MapFinding{Code: code, ToolUseID: toolUse, Tool: tool, Sources: src})
}

// decodeLines decodes a JSONL source strictly: valid UTF-8, one object per
// nonempty line, no duplicate key. Bad lines are findings.
func (d *deriver) decodeLines(file string, data []byte) []sessionRecord {
	var out []sessionRecord
	sum := sha256Hex(data)
	lines := bytes.Split(data, []byte("\n"))
	for i, l := range lines {
		if len(l) == 0 {
			continue
		}
		src := SourceRef{File: file, SHA256: sum, FirstLine: i + 1, LastLine: i + 1}
		if len(l) > maxSessionRecordSize || !utf8.Valid(l) {
			d.find(CodeSessionMalformed, "", "", src)
			continue
		}
		// Unmarshal proves l one valid value before rawKeysUnique judges it.
		var raw map[string]json.RawMessage
		if json.Unmarshal(l, &raw) != nil || raw == nil || !rawKeysUnique(l) && walkDuplicateKeys(l) != nil {
			d.find(CodeSessionMalformed, "", "", src)
			continue
		}
		out = append(out, parseRecord(raw, src))
	}
	return out
}

func rawString(raw json.RawMessage) string {
	var s string
	json.Unmarshal(raw, &s)
	return s
}

// rawTyped decodes a present, non-null field into v; false when it is
// absent, null or of another type.
func rawTyped(raw json.RawMessage, v any) bool {
	return raw != nil && string(raw) != "null" && json.Unmarshal(raw, v) == nil
}

// parseRecord classifies one record (stream or transcript envelope).
func parseRecord(raw map[string]json.RawMessage, src SourceRef) sessionRecord {
	r := sessionRecord{src: src, kind: "other"}
	typ, subtype := rawString(raw["type"]), rawString(raw["subtype"])
	r.sessionID = rawString(raw["session_id"])
	if s := rawString(raw["sessionId"]); s != "" {
		r.sessionID = s
	}
	r.cwd = rawString(raw["cwd"])
	switch {
	case typ == "system" && subtype == "init":
		r.kind = "init"
		r.init = InitRecord{SessionID: r.sessionID, Model: rawString(raw["model"]), PermissionMode: rawString(raw["permissionMode"]),
			Tools: []string{}, MCPServers: []string{}}
		var tools []string
		if json.Unmarshal(raw["tools"], &tools) == nil {
			r.init.Tools = append(r.init.Tools, tools...)
		}
		var servers []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if json.Unmarshal(raw["mcp_servers"], &servers) == nil {
			for _, s := range servers {
				r.init.MCPServers = append(r.init.MCPServers, s.Name+":"+s.Status)
			}
		}
	case typ == "result":
		r.kind = "result"
		var isErr bool
		var sub, sid string
		r.malformed = !rawTyped(raw["is_error"], &isErr) || !rawTyped(raw["subtype"], &sub) ||
			(raw["session_id"] != nil && !rawTyped(raw["session_id"], &sid))
		r.resultOK = !r.malformed && r.sessionID != "" && sub == "success" && !isErr
		var denials []struct {
			ToolName  string `json:"tool_name"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(raw["permission_denials"], &denials) == nil {
			for _, x := range denials {
				r.denials = append(r.denials, x.ToolUseID+"\x00"+x.ToolName)
			}
		}
	case typ == "assistant" || typ == "user":
		r.kind = typ
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw["message"], &msg) != nil {
			r.kind = "other"
			break
		}
		var text string
		if json.Unmarshal(msg.Content, &text) == nil {
			r.userText, r.hasText = text, true
			break
		}
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			r.kind = "other"
			break
		}
		for _, b := range blocks {
			sb := sessionBlock{typ: b.Type, text: b.Text, id: b.ID, name: b.Name, input: b.Input, toolUseID: b.ToolUseID, isError: b.IsError}
			if b.Type == "tool_result" {
				sb.text = resultText(b.Content)
			}
			if b.Type == "text" && typ == "user" {
				r.userText += b.Text
				r.hasText = true
			}
			r.blocks = append(r.blocks, sb)
		}
	}
	return r
}

// resultText flattens a tool result's content (a string or text blocks).
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	var b strings.Builder
	for _, x := range blocks {
		if x.Type == "text" {
			b.WriteString(x.Text)
		}
	}
	return b.String()
}

// notification is a parsed background-completion notification.
type notification struct {
	taskID, toolUseID, status string
}

var notifChildRE = regexp.MustCompile(`^<([a-z-]+)>([^<>]*)</([a-z-]+)>$`)

// parseNotification parses the accepted task-notification envelope: ok
// false with unsupported true when the text mentions one in any other
// shape.
func parseNotification(text string) (n notification, ok, unsupported bool) {
	t := strings.TrimSpace(text)
	if !strings.Contains(t, "<task-notification>") {
		return n, false, false
	}
	if !strings.HasPrefix(t, "<task-notification>") || !strings.HasSuffix(t, "</task-notification>") {
		return n, false, true
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(t, "<task-notification>"), "</task-notification>")
	seen := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(inner), "\n") {
		m := notifChildRE.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil || m[1] != m[3] {
			return n, false, true
		}
		switch m[1] {
		case "task-id", "tool-use-id", "status", "output-file", "summary":
		default:
			return n, false, true
		}
		if _, dup := seen[m[1]]; dup {
			return n, false, true
		}
		seen[m[1]] = m[2]
	}
	n = notification{taskID: seen["task-id"], toolUseID: seen["tool-use-id"], status: seen["status"]}
	if n.taskID == "" || n.toolUseID == "" || n.status == "" {
		return n, false, true
	}
	return n, true, false
}

// backgroundRE reads a background Bash result's handle.
var backgroundRE = regexp.MustCompile(`^Command running in background with ID: ([A-Za-z0-9_-]+)\.`)

// canonicalJSON renders a value with sorted keys (for payload equality).
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// toolUse is one deduplicated tool use and its linked result.
type toolUse struct {
	block     sessionBlock
	src       []SourceRef
	pos       int
	phase     int
	result    *sessionBlock
	resultSrc []SourceRef
	resultPos int
	hasResult bool
	// streamPhase and streamLine locate the result's first copy in a raw
	// phase stream (0: no stream copy).
	streamPhase int
	streamLine  int
}

// streamAnchor is a transcript record (its position and phase) whose
// matching copy is at line of phase streamPhase's raw stream.
type streamAnchor struct {
	pos, phase, streamPhase, line int
}

// timelineItem is one ordered transcript item.
type timelineItem struct {
	pos   int
	phase int
	kind  string // prompt, notification, user, tool_use, tool_result
	src   SourceRef
	use   string
	notif notification
}

// DeriveSession derives the version 2 event map. It is pure and
// deterministic: the same inputs give the same map.
func DeriveSession(in SessionInput) EventMap {
	em, _ := deriveSession(in)
	return em
}

// deriveSession is DeriveSession, also returning each phase stream's
// result as streamResult reports it (present, ok), from the records the
// derivation decoded.
func deriveSession(in SessionInput) (EventMap, [2][2]bool) {
	d := &deriver{in: in, codes: map[string]bool{}}
	em := d.derive()
	return em, d.streamResults
}

// derive derives the event map of d's input.
func (d *deriver) derive() EventMap {
	in := d.in
	em := EventMap{Schema: EventMapSchema, RunID: in.RunID, Source: MapSourceDerived, Entries: []MapEntry{}, Findings: []MapFinding{}, Init: []InitRecord{}}
	var streams [2][]sessionRecord
	results := [2]*sessionRecord{}
	for p := 0; p < 2; p++ {
		if in.Streams[p] == nil {
			continue
		}
		streams[p] = d.decodeLines(phaseStreamFiles[p], in.Streams[p])
		d.streamResults[p][0], d.streamResults[p][1] = recordsResult(streams[p])
		// The raw chronology: the init record precedes all session work
		// and the terminal result follows it all.
		phaseSID, hasInit := "", false
		for i := range streams[p] {
			r := &streams[p][i]
			r.phase = p + 1
			work := r.kind == "assistant" || r.kind == "user" || r.kind == "result"
			if work && (!hasInit || results[p] != nil) {
				d.find(CodeSessionMalformed, "", "", r.src)
			}
			switch r.kind {
			case "init":
				if len(d.init) > p {
					d.find(CodeSessionMalformed, "", "", r.src)
					continue
				}
				r.init.Phase = p + 1
				d.init = append(d.init, r.init)
				phaseSID, hasInit = r.sessionID, true
				if p == 0 && d.sid == "" {
					d.sid = r.sessionID
				}
			case "result":
				// A result binds to its phase's init session with its
				// required fields present and typed.
				switch {
				case r.malformed:
					d.find(CodeSessionMalformed, "", "", r.src)
				case r.sessionID == "":
					d.find(CodeSessionUnobserved, "", "", r.src)
				}
				if !hasInit || r.sessionID != phaseSID {
					r.resultOK = false
				}
				if results[p] != nil {
					d.find(CodeSessionMalformed, "", "", r.src)
				}
				results[p] = r
				for _, den := range r.denials {
					use, tool, _ := strings.Cut(den, "\x00")
					d.find(CodeToolDenied, use, tool, r.src)
				}
			}
		}
	}
	if in.Streams[0] == nil || len(d.init) == 0 || !sessionIDRE.MatchString(d.sid) {
		d.find(CodeSessionUnobserved, "", "")
	}
	// One session ID in every session-bearing stream record.
	for p := 0; p < 2; p++ {
		for _, r := range streams[p] {
			if r.sessionID != "" && r.sessionID != d.sid {
				d.find(CodeSessionMismatch, "", "", r.src)
			}
		}
	}
	if in.Streams[1] != nil && (len(d.init) < 2 || d.init[1].SessionID != d.sid) {
		d.find(CodeSessionMismatch, "", "")
	}
	em.SessionID = d.sid
	em.Init = d.init
	// The transcript: the latest snapshot, the first an exact prefix.
	tfile, tdata := -1, []byte(nil)
	for p := 1; p >= 0; p-- {
		if in.Transcripts[p] != nil {
			tfile, tdata = p, in.Transcripts[p]
			break
		}
	}
	if tfile < 0 {
		d.find(CodeSessionUnobserved, "", "")
		return d.finish(em)
	}
	prefixLen := len(tdata)
	if tfile == 1 {
		if in.Transcripts[0] == nil || !bytes.HasPrefix(tdata, in.Transcripts[0]) {
			// Append-on-resume did not hold (an undocumented vendor
			// behaviour): no fact is attributed from rewritten records.
			d.find(CodeSessionUnobserved, "", "")
			return d.finish(em)
		}
		prefixLen = len(in.Transcripts[0])
	}
	trecs := d.decodeLines(transcriptFiles[tfile], tdata)
	// Phase-1 records keep their phase-1 snapshot reference.
	offsets := lineOffsets(tdata)
	for i := range trecs {
		r := &trecs[i]
		r.phase = 2
		if tfile == 0 || offsets[r.src.FirstLine-1] < prefixLen {
			r.phase = 1
			if tfile == 1 && in.Transcripts[0] != nil {
				r.src.File, r.src.SHA256 = transcriptFiles[0], sha256Hex(in.Transcripts[0])
			}
		}
		if r.kind != "user" && r.kind != "assistant" {
			continue
		}
		if r.sessionID != d.sid {
			d.find(CodeSessionMismatch, "", "", r.src)
		}
		if l := in.Launches[r.phase-1]; l != nil && r.cwd != "" && r.cwd != l.Cwd {
			d.find(CodeSessionMismatch, "", "", r.src)
		}
	}
	// Index tool uses and results; build the ordered timeline.
	uses := map[string]*toolUse{}
	var order []string
	var timeline []timelineItem
	prompts := [2]string{}
	for p := 0; p < 2; p++ {
		if l := in.Launches[p]; l != nil {
			prompts[p] = launchPrompt(l)
		}
	}
	for pos, r := range trecs {
		switch r.kind {
		case "assistant":
			for _, b := range r.blocks {
				if b.typ != "tool_use" {
					continue
				}
				if u, ok := uses[b.id]; ok {
					if u.block.name != b.name || canonicalJSON(u.block.input) != canonicalJSON(b.input) {
						d.find(CodeSessionMalformed, b.id, b.name, r.src)
					}
					u.src = append(u.src, r.src)
					continue
				}
				uses[b.id] = &toolUse{block: b, src: []SourceRef{r.src}, pos: pos, phase: r.phase}
				order = append(order, b.id)
				timeline = append(timeline, timelineItem{pos: pos, phase: r.phase, kind: "tool_use", src: r.src, use: b.id})
			}
		case "user":
			handled := false
			for _, b := range r.blocks {
				if b.typ != "tool_result" {
					continue
				}
				handled = true
				u, ok := uses[b.toolUseID]
				if !ok {
					d.find(CodeSessionMalformed, b.toolUseID, "", r.src)
					continue
				}
				if u.hasResult {
					if u.result.text != b.text || u.result.isError != b.isError {
						d.find(CodeSessionMalformed, b.toolUseID, u.block.name, r.src)
					}
					u.resultSrc = append(u.resultSrc, r.src)
					continue
				}
				bb := b
				u.result, u.hasResult, u.resultSrc, u.resultPos = &bb, true, []SourceRef{r.src}, pos
				timeline = append(timeline, timelineItem{pos: pos, phase: r.phase, kind: "tool_result", src: r.src, use: b.toolUseID})
			}
			if handled || !r.hasText {
				continue
			}
			text := strings.TrimSpace(r.userText)
			switch n, ok, unsupported := parseNotification(text); {
			case ok:
				timeline = append(timeline, timelineItem{pos: pos, phase: r.phase, kind: "notification", src: r.src, notif: n})
			case unsupported:
				d.find(CodeWakeUnsupported, "", "", r.src)
			case text == strings.TrimSpace(prompts[r.phase-1]) && text != "":
				timeline = append(timeline, timelineItem{pos: pos, phase: r.phase, kind: "prompt", src: r.src})
			default:
				d.find(CodeUserInput, "", "", r.src)
				timeline = append(timeline, timelineItem{pos: pos, phase: r.phase, kind: "user", src: r.src})
			}
		}
	}
	// Every stream tool use and result appears in the transcript with the
	// same payload; each matching copy anchors its transcript record in
	// the raw stream.
	var anchors []streamAnchor
	for p := 0; p < 2; p++ {
		for _, r := range streams[p] {
			for _, b := range r.blocks {
				switch b.typ {
				case "tool_use":
					u, ok := uses[b.id]
					switch {
					case !ok:
						d.find(CodeSessionUnobserved, b.id, b.name, r.src)
					case u.block.name != b.name || canonicalJSON(u.block.input) != canonicalJSON(b.input):
						d.find(CodeSessionMalformed, b.id, b.name, r.src)
					default:
						u.src = append(u.src, r.src)
						anchors = append(anchors, streamAnchor{u.pos, trecs[u.pos].phase, p + 1, r.src.FirstLine})
					}
				case "tool_result":
					u, ok := uses[b.toolUseID]
					switch {
					case !ok:
						d.find(CodeSessionMalformed, b.toolUseID, "", r.src)
					case !u.hasResult || u.result.text != b.text || u.result.isError != b.isError:
						d.find(CodeSessionMalformed, b.toolUseID, u.block.name, r.src)
					default:
						u.resultSrc = append(u.resultSrc, r.src)
						if u.streamLine == 0 {
							u.streamPhase, u.streamLine = p+1, r.src.FirstLine
						}
						anchors = append(anchors, streamAnchor{u.resultPos, trecs[u.resultPos].phase, p + 1, r.src.FirstLine})
					}
				}
			}
		}
	}
	// Tool failures are findings linked to the exact tool use.
	for _, id := range order {
		u := uses[id]
		if u.hasResult && u.result.isError {
			d.find(CodeToolFailed, id, u.block.name, u.resultSrc...)
		}
	}
	d.deriveFacts(timeline, uses, anchors, results, streams)
	return d.finish(em)
}

// lineOffsets returns each line's starting byte offset.
func lineOffsets(data []byte) []int {
	out := []int{0}
	for i, c := range data {
		if c == '\n' {
			out = append(out, i+1)
		}
	}
	return out
}

// launchPrompt is a launch's -p user prompt.
func launchPrompt(l *LaunchRecord) string {
	for i, a := range l.Argv {
		if a == "-p" && i+1 < len(l.Argv) {
			return l.Argv[i+1]
		}
	}
	return ""
}

// hopTasks returns the supervisor-observed task of each hop.
func hopTasksOf(evs []Event) ([4]string, [4]int) {
	var tasks [4]string
	var seqs [4]int
	for _, e := range evs {
		if e.Type == EvObserved {
			if i := hopIndex(e.Hop); i >= 0 && tasks[i] == "" {
				tasks[i], seqs[i] = e.Task, e.Seq
			}
		}
	}
	return tasks, seqs
}

// firstSeq returns the sequence of the first event of typ with handle (or
// any handle when ""), or 0.
func firstSeq(evs []Event, typ, handle string) int {
	for _, e := range evs {
		if e.Type == typ && (handle == "" || e.Handle == handle) {
			return e.Seq
		}
	}
	return 0
}

// sources merges sorted source references.
func sources(groups ...[]SourceRef) []SourceRef {
	var out []SourceRef
	seen := map[SourceRef]bool{}
	for _, g := range groups {
		for _, s := range g {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].FirstLine < out[j].FirstLine
	})
	if out == nil {
		out = []SourceRef{}
	}
	return out
}

// deriveFacts derives the ordered entries from the timeline.
func (d *deriver) deriveFacts(timeline []timelineItem, uses map[string]*toolUse, anchors []streamAnchor, results [2]*sessionRecord, streams [2][]sessionRecord) {
	tasks, observedSeq := hopTasksOf(d.in.Events)
	templates := [4]string{}
	if l := d.in.Launches[0]; l != nil {
		for i := range Hops {
			if i < len(l.WaitTemplates) {
				templates[i] = l.WaitTemplates[i]
			}
		}
	}
	initSrc := func(p int) []SourceRef {
		for _, r := range streams[p] {
			if r.kind == "init" {
				return []SourceRef{r.src}
			}
		}
		return []SourceRef{}
	}
	resultSrc := func(p int) []SourceRef {
		if results[p] != nil {
			return []SourceRef{results[p].src}
		}
		return []SourceRef{}
	}
	if len(d.init) > 0 {
		d.entries = append(d.entries, MapEntry{Kind: MapLaunch, Phase: 1, SessionID: d.sid, Seq: firstSeq(d.in.Events, EvPhaseStarted, phaseHandle(1)), Sources: initSrc(0)})
	}
	waits := map[string]int{} // background handle -> hop
	waitUse := [4]string{}    // hop -> Bash tool use
	consumedAfter := [4]int{-1, -1, -1, -1}
	consumedUse := [4]string{}
	wakePos := [4]int{-1, -1, -1, -1}
	// notifAt is the first completion notification's position per
	// background Bash tool use.
	notifAt := map[string]int{}
	for _, it := range timeline {
		if _, seen := notifAt[it.notif.toolUseID]; it.kind == "notification" && !seen {
			notifAt[it.notif.toolUseID] = it.pos
		}
	}
	afterWake := func(use string, pos int) bool {
		w, ok := notifAt[use]
		return !ok || w < pos
	}
	phaseDone := false
	for _, it := range timeline {
		if it.phase == 2 && !phaseDone {
			phaseDone = true
			d.pauseEntries(results, resultSrc, initSrc)
		}
		switch it.kind {
		case "tool_use":
			u := uses[it.use]
			// A long blocking MCP wait, or a dispatch that waits, is never
			// part of the automatic attempt (the direct CLI background wait
			// is), whatever its result.
			if u.block.name == toolTaskWait {
				d.find(CodeLongPoll, it.use, u.block.name, u.src...)
				continue
			}
			if !u.hasResult || u.result.isError {
				continue
			}
			switch u.block.name {
			case toolDispatch:
				var in struct {
					Target struct {
						Kind  string `json:"kind"`
						Value string `json:"value"`
					} `json:"target"`
					Payload []string        `json:"payload"`
					Wait    json.RawMessage `json:"wait"`
				}
				json.Unmarshal(u.block.input, &in)
				if w := strings.TrimSpace(string(in.Wait)); w != "" && w != "null" && w != "0" && w != `"0"` && w != `"0s"` {
					d.find(CodeLongPoll, it.use, u.block.name, u.src...)
				}
				hop := -1
				for i, h := range Hops {
					if len(in.Payload) > 0 && in.Payload[0] == markerFor(d.in.RunID, h) {
						hop = i
					}
				}
				var out struct {
					TaskID string `json:"task_id"`
				}
				json.Unmarshal([]byte(u.result.text), &out)
				if hop < 0 || in.Target.Kind != "id" || in.Target.Value != Hops[hop] || out.TaskID == "" || out.TaskID != tasks[hop] {
					d.find(CodeDispatchMismatch, it.use, u.block.name, u.src...)
					continue
				}
				d.entries = append(d.entries, MapEntry{Kind: MapDispatch, Phase: it.phase, SessionID: d.sid, Hop: Hops[hop], Task: out.TaskID,
					ToolUseID: it.use, Seq: observedSeq[hop], Sources: sources(u.src, u.resultSrc)})
			case toolBash:
				var in map[string]json.RawMessage
				json.Unmarshal(u.block.input, &in)
				cmd := rawString(in["command"])
				hop := -1
				for i := range Hops {
					if tasks[i] != "" && templates[i] != "" && cmd == strings.ReplaceAll(templates[i], "TASK_ID", tasks[i]) {
						hop = i
					}
				}
				if hop < 0 {
					if strings.Contains(cmd, " task wait ") {
						d.find(CodeWaitMismatch, it.use, toolBash, u.src...)
					}
					continue
				}
				var bg bool
				json.Unmarshal(in["run_in_background"], &bg)
				extra := false
				for k := range in {
					if k != "command" && k != "run_in_background" && k != "description" {
						extra = true
					}
				}
				m := backgroundRE.FindStringSubmatch(strings.TrimSpace(u.result.text))
				if !bg || extra || m == nil || waitUse[hop] != "" {
					d.find(CodeWaitMismatch, it.use, toolBash, u.src...)
					continue
				}
				waits[m[1]], waitUse[hop] = hop, it.use
				d.entries = append(d.entries, MapEntry{Kind: MapWaitStart, Phase: it.phase, SessionID: d.sid, Hop: Hops[hop], Task: tasks[hop],
					ToolUseID: it.use, Handle: m[1], Sources: sources(u.src, u.resultSrc)})
			case toolTaskShow:
				var in struct {
					ID string `json:"id"`
				}
				json.Unmarshal(u.block.input, &in)
				// Consumption is the linked response showing that hop's
				// task terminal, read after its completion notification;
				// it is placed at the response.
				view, verr := contract.ParseTaskShowResponse([]byte(u.result.text))
				for i := range Hops {
					if in.ID == tasks[i] && waitUse[i] != "" && consumedAfter[i] < 0 && it.pos > uses[waitUse[i]].pos && it.phase == hopPhase(i) &&
						verr == nil && view.TaskID == tasks[i] && contract.TaskTerminal(view.State) && afterWake(waitUse[i], u.resultPos) {
						consumedAfter[i], consumedUse[i] = u.resultPos, it.use
						d.entries = append(d.entries, MapEntry{Kind: MapWaitConsumed, Phase: it.phase, SessionID: d.sid, Hop: Hops[i], Task: tasks[i],
							ToolUseID: it.use, Handle: handleOf(waits, i), Sources: sources(u.src, u.resultSrc)})
					}
				}
			}
		case "notification":
			hop, ok := waits[it.notif.taskID]
			switch {
			case !ok:
				// Another background job's completion: not a hop's wake.
				continue
			case waitUse[hop] != it.notif.toolUseID || wakePos[hop] >= 0:
				d.find(CodeWakeUnsupported, it.notif.toolUseID, toolBash, it.src)
				continue
			case it.phase != hopPhase(hop):
				// A later phase's prompt never substitutes for this wake.
				continue
			}
			wakePos[hop] = it.pos
			d.entries = append(d.entries, MapEntry{Kind: MapWake, Phase: it.phase, SessionID: d.sid, Hop: Hops[hop], Task: tasks[hop],
				ToolUseID: it.notif.toolUseID, Handle: it.notif.taskID, Sources: []SourceRef{it.src}})
		}
	}
	if !phaseDone {
		d.pauseEntries(results, resultSrc, initSrc)
	}
	if results[1] != nil && results[1].resultOK {
		d.entries = append(d.entries, MapEntry{Kind: MapFinalResult, Phase: 2, SessionID: d.sid, Seq: firstSeq(d.in.Events, EvPhaseExited, phaseHandle(2)),
			Sources: resultSrc(1)})
	}
	// A wake is automatic when it precedes its consumption with no
	// ordinary user input or phase prompt between them.
	for i := range Hops {
		if wakePos[i] < 0 || consumedAfter[i] < 0 || wakePos[i] > consumedAfter[i] {
			continue
		}
		auto := true
		for _, it := range timeline {
			if it.pos > wakePos[i] && it.pos < consumedAfter[i] && (it.kind == "user" || it.kind == "prompt") {
				auto = false
			}
		}
		for j := range d.entries {
			if d.entries[j].Kind == MapWake && d.entries[j].Hop == Hops[i] {
				d.entries[j].Automatic = auto
			}
		}
	}
	// A phase ending before its required hops' waits were consumed. The
	// transcript orders the consumption's response; that phase's raw
	// stream must hold, before its result, a matching copy of the response
	// or of a later record of the same phase. A response present only in
	// the transcript is allowed; a stream copy of it elsewhere is not.
	for p, hops := range [2][]int{{0, 1}, {2, 3}} {
		if results[p] == nil {
			continue
		}
		end := results[p].src.FirstLine
		for _, i := range hops {
			switch u := uses[consumedUse[i]]; {
			case consumedAfter[i] < 0,
				u.streamLine != 0 && (u.streamPhase != p+1 || u.streamLine >= end),
				!slices.ContainsFunc(anchors, func(a streamAnchor) bool {
					return a.pos >= consumedAfter[i] && a.phase == p+1 && a.streamPhase == p+1 && a.line < end
				}):
				d.find(CodePhaseEarly, "", "", resultSrc(p)...)
			}
		}
	}
}

// pauseEntries appends the phase-1 pause, the owner decision and the
// phase-2 resume.
func (d *deriver) pauseEntries(results [2]*sessionRecord, resultSrc, initSrc func(int) []SourceRef) {
	if results[0] != nil && results[0].resultOK {
		d.entries = append(d.entries, MapEntry{Kind: MapPause, Phase: 1, SessionID: d.sid, Seq: firstSeq(d.in.Events, EvAwaitingOwner, ""), Sources: resultSrc(0)})
	}
	if seq := firstSeq(d.in.Events, EvOwnerDecision, ""); seq > 0 {
		d.entries = append(d.entries, MapEntry{Kind: MapDecision, Phase: 1, SessionID: d.sid, Seq: seq, Sources: []SourceRef{}})
	}
	if len(d.init) > 1 && d.init[1].SessionID == d.sid {
		d.entries = append(d.entries, MapEntry{Kind: MapResume, Phase: 2, SessionID: d.sid, Seq: firstSeq(d.in.Events, EvPhaseStarted, phaseHandle(2)), Sources: initSrc(1)})
	}
}

// handleOf returns hop i's background handle.
func handleOf(waits map[string]int, i int) string {
	var hs []string
	for h, hop := range waits {
		if hop == i {
			hs = append(hs, h)
		}
	}
	slices.Sort(hs)
	if len(hs) == 0 {
		return ""
	}
	return hs[0]
}

// hopPhase is the coordinator phase (1 or 2) that runs 0-based hop i.
func hopPhase(i int) int {
	if i < 2 {
		return 1
	}
	return 2
}

// phaseHandle is a coordinator phase's process handle.
func phaseHandle(p int) string { return "coordinator-phase-" + strconv.Itoa(p) }

// finish sorts findings and fills the map.
func (d *deriver) finish(em EventMap) EventMap {
	sort.SliceStable(d.findings, func(i, j int) bool {
		if d.findings[i].Code != d.findings[j].Code {
			return d.findings[i].Code < d.findings[j].Code
		}
		return d.findings[i].ToolUseID < d.findings[j].ToolUseID
	})
	em.Entries = d.entries
	if em.Entries == nil {
		em.Entries = []MapEntry{}
	}
	em.Findings = d.findings
	if em.Findings == nil {
		em.Findings = []MapFinding{}
	}
	em.Init = d.init
	if em.Init == nil {
		em.Init = []InitRecord{}
	}
	return em
}

// findingCodes returns a map's distinct finding codes.
func findingCodes(em EventMap) []string {
	var out []string
	for _, f := range em.Findings {
		if !slices.Contains(out, f.Code) {
			out = append(out, f.Code)
		}
	}
	return out
}

// entriesOf returns the entries of kind for hop ("" any).
func entriesOf(em EventMap, kind, hop string) []MapEntry {
	var out []MapEntry
	for _, e := range em.Entries {
		if e.Kind == kind && (hop == "" || e.Hop == hop) {
			out = append(out, e)
		}
	}
	return out
}
