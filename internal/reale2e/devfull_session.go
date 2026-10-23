package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Development session observation (design 12a-dev-flow-harness, Execution
// lifecycle). Each phase's raw stream is decoded with the legacy strict
// record decoder; the legacy four-hop event map is not extended. A wait
// qualifies only when one exact background command for the slot's task
// started, its result named the background handle, exactly one automatic
// completion notification of that handle and tool use arrived, and a later
// task_show of that task showed it terminal with no ordinary user input
// between the notification and that response. The captured wait files must
// record exit 0 and a terminal result for the task. A narrative or a phase
// result alone never satisfies a wait.
//
// Qualified wake envelopes (design 12a-dev-flow-verify r0.8, owner
// decision 2026-10-22): the completion notification is either the user
// text <task-notification> envelope or a top-level system
// task_notification record (captured from Claude Code 2.1.306), in the
// same phase. Both bind to a preceding background Bash start by its
// tool-use ID and to that start's handle result by the handle; only a
// dev-wait-task start's notification is a wake. An observed notification
// that is malformed, unbound, mis-bound, duplicated, failed or in another
// location is refused at its record (wake_representation_unsupported).

// Development stream problem codes.
const (
	devToolDenied       = "tool_denied"
	devAutoModeMissing  = "auto_mode_missing"
	devSessionMismatch  = "session_mismatch"
	devSessionMalformed = "session_malformed"
	devToolFailed       = "coordinator_tool_failed"
	devResultFailed     = "coordinator_result_failed"
	devLongPoll         = "coordinator_long_poll"
	devWaitMismatch     = "wait_command_mismatch"
	devDuplicateWait    = "duplicate_wait"
	devWakeUnsupported  = "wake_representation_unsupported"
	devWakeNotAutomatic = "wake_not_automatic"
	devWaitFailed       = "wait_failed"
)

// devStream is one decoded phase stream.
type devStream struct {
	records   []sessionRecord
	sessionID string
	// problem is the first fatal problem's fixed code ("" none);
	// problemSrc is the record it was found at (zero when no single
	// record is the cause).
	problem    string
	problemSrc SourceRef
	// result is the last result record's text and ok (the phase result).
	resultText string
	resultOK   bool
	hasResult  bool
}

// decodeDevStream decodes a phase stream. want is the captured session a
// resumed phase must keep ("" for the first phase).
func decodeDevStream(stdout []byte, want string) devStream {
	return decodeDevStreamAs("phase-stream", stdout, want)
}

// decodeDevStreamAs is decodeDevStream whose source references name file.
func decodeDevStreamAs(file string, stdout []byte, want string) devStream {
	return decodeDevStreamFor(file, stdout, want, "")
}

// decodeDevStreamFor is decodeDevStreamAs knowing the slot commands'
// prefix (devWaitPrefixOf): a record the strict decoder refuses (a
// duplicate key) holding a dev-wait-task start fails wait_command_mismatch
// (design 12a-dev-flow-verify r0.9), never merely session_malformed.
func decodeDevStreamFor(file string, stdout []byte, want, waitPrefix string) devStream {
	d := &deriver{codes: map[string]bool{}}
	s := devStream{records: d.decodeLines(file, stdout), sessionID: want}
	fail := func(code string, src SourceRef) {
		if s.problem == "" {
			s.problem, s.problemSrc = code, src
		}
	}
	// A line the strict decoder refused (a duplicate key, say) that is a
	// notification candidate in either form (devNoteOf of its lenient
	// reading) is an unsupported notification, never merely a malformed
	// record (code review W1).
	if len(d.findings) > 0 {
		f := d.findings[0]
		code, src := devSessionMalformed, SourceRef{}
		if len(f.Sources) == 1 {
			src = f.Sources[0]
			lines := bytes.Split(stdout, []byte("\n"))
			var raw map[string]json.RawMessage
			if i := src.FirstLine - 1; i >= 0 && i < len(lines) && json.Unmarshal(lines[i], &raw) == nil && raw != nil {
				r := parseRecord(raw, src)
				switch {
				case devNoteOf(r).candidate:
					code = devWakeUnsupported
				case devWaitStartIn(r, waitPrefix):
					code = devWaitMismatch
				}
			}
		}
		fail(code, src)
	}
	// An error result of a Callsheet MCP call is judged by the recorded
	// MCP call policy (design 12a-dev-flow-verify r0.6, Error-result
	// policy): a tolerated no-evidence answer, a lost dispatch reply
	// reconciled through task_ls, or an invalid call. Only a vendor tool's
	// error, or an error result matching no tool use, fails the phase here.
	names := map[string]string{}
	for _, r := range s.records {
		for _, b := range r.blocks {
			if r.kind == "assistant" && b.typ == "tool_use" {
				names[b.id] = b.name
			}
		}
	}
	// A notification's own session is validated as part of the
	// notification (r0.8): a mismatch refuses it before the generic
	// session check; one seen before the phase's session is known is
	// checked once it is.
	var unsessioned []devNote
	for _, r := range s.records {
		if note := devNoteOf(r); note.candidate {
			switch {
			case note.unsupported:
				fail(devWakeUnsupported, r.src)
			case !note.hasSession:
			case s.sessionID == "":
				unsessioned = append(unsessioned, note)
			case note.session != s.sessionID:
				fail(devWakeUnsupported, r.src)
			}
			continue
		}
		if r.sessionID != "" {
			if s.sessionID == "" {
				s.sessionID = r.sessionID
			} else if r.sessionID != s.sessionID {
				fail(devSessionMismatch, r.src)
			}
		}
		switch r.kind {
		case "init":
			if r.malformed || r.init.PermissionMode != autoModeFlag || r.sessionID == "" {
				fail(devAutoModeMissing, r.src)
			}
		case "result":
			if len(r.denials) > 0 {
				fail(devToolDenied, r.src)
			}
			if r.malformed || !r.resultOK {
				fail(devResultFailed, r.src)
			}
			s.hasResult, s.resultOK = true, !r.malformed && r.resultOK
		case "user":
			for _, b := range r.blocks {
				if b.typ == "tool_result" && b.isError && !strings.HasPrefix(names[b.toolUseID], mcpToolPrefix) {
					fail(devToolFailed, r.src)
				}
			}
		case "assistant":
			for _, b := range r.blocks {
				if b.typ == "tool_use" && b.name == toolTaskWait {
					fail(devLongPoll, r.src)
				}
			}
		}
	}
	for _, note := range unsessioned {
		if note.session != s.sessionID {
			fail(devWakeUnsupported, note.src)
		}
	}
	if s.hasResult {
		s.resultText = lastResultText(stdout)
	}
	return s
}

// lastResultText is the last result record's result string.
func lastResultText(stdout []byte) string {
	text := ""
	for _, l := range strings.Split(string(stdout), "\n") {
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(l), &raw) != nil || rawString(raw["type"]) != "result" {
			continue
		}
		text = rawString(raw["result"])
	}
	return text
}

// devWait is one slot's qualified wait evidence.
type devWait struct {
	StartUse   string `json:"start_tool_use"`
	Handle     string `json:"handle"`
	Woke       bool   `json:"woke"`
	Consumed   bool   `json:"consumed"`
	ConsumeUse string `json:"consume_tool_use"`
	Automatic  bool   `json:"automatic"`
}

// complete reports a fully qualified wait.
func (w devWait) complete() bool {
	return w.StartUse != "" && w.Handle != "" && w.Woke && w.Consumed && w.Automatic
}

// devWaitInputKeys are a wait's only Bash input keys: command,
// description, run_in_background and, since design 12a-dev-flow-verify
// r0.9 (owner decision 2026-10-22), an optional timeout (devWaitInputOK).
var devWaitInputKeys = map[string]bool{"command": true, "run_in_background": true, "description": true, "timeout": true}

// devWaitTimeoutRE is the only accepted raw timeout token: a positive
// decimal integer, matched as text (no float conversion, no ceiling).
var devWaitTimeoutRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// qualifyDevWait derives the slot's wait evidence for task from a phase's
// decoded records. problem is a fixed code when the evidence is wrong;
// incomplete evidence without a problem may still complete.
func qualifyDevWait(records []sessionRecord, cmd, task string) (devWait, string) {
	w, _, problem := deriveDevWait(records, cmd, task)
	return w, problem
}

// devWaitSources are a qualified wait's facts' source lines: the
// background start's tool use and its handle result, the notification and
// the consuming task_show result. refused is the notification record a
// wake_representation_unsupported refusal names.
type devWaitSources struct {
	start, handle, wake, consume SourceRef
	refused                      SourceRef
}

// devNote is one record classified as a completion-notification
// candidate (design 12a-dev-flow-verify r0.8): candidate false for every
// other record; unsupported for a candidate in no accepted form; n its
// normalized fields; session the record's own session ID when it has one.
type devNote struct {
	candidate, unsupported bool
	n                      notification
	session                string
	hasSession             bool
	src                    SourceRef
}

// devSystemNoteKeys are the system notification's only fields: the
// required task_id, tool_use_id and status, the optional session_id, and
// the report-only output_file, summary, run_id and uuid.
var devSystemNoteKeys = map[string]bool{"type": true, "subtype": true, "task_id": true, "tool_use_id": true, "status": true, "session_id": true,
	"output_file": true, "summary": true, "run_id": true, "uuid": true}

// devNoteOf classifies r. The two accepted forms are a top-level system
// record with subtype task_notification (exact field set and types) and a
// user text record whose whole text is the <task-notification> envelope
// parseNotification accepts. The task_notification subtype on another
// record type, any other system notification shape, an envelope (or its
// mention) beside tool results in a user record, and an envelope
// parseNotification refuses are unsupported candidates. Assistant prose,
// tool output and other records are never candidates.
func devNoteOf(r sessionRecord) devNote {
	note := devNote{src: r.src}
	switch {
	case r.subtype == "task_notification":
		note.candidate = true
		if r.typ != "system" || r.raw == nil {
			note.unsupported = true
			return note
		}
		for k := range r.raw {
			if !devSystemNoteKeys[k] {
				note.unsupported = true
				return note
			}
		}
		required := func(k string) string {
			var v string
			if !rawTyped(r.raw[k], &v) || v == "" {
				note.unsupported = true
			}
			return v
		}
		note.n = notification{taskID: required("task_id"), toolUseID: required("tool_use_id"), status: required("status")}
		for _, k := range []string{"output_file", "summary", "run_id", "uuid"} {
			var v string
			if !optTyped(r.raw[k], &v) {
				note.unsupported = true
			}
		}
		if v, ok := r.raw["session_id"]; ok {
			note.hasSession = true
			if !rawTyped(v, &note.session) || note.session == "" {
				note.unsupported = true
			}
		}
		return note
	case r.kind == "user":
		withResults := false
		for _, b := range r.blocks {
			withResults = withResults || b.typ == "tool_result"
		}
		if !r.hasText || !strings.Contains(r.userText, "<task-notification>") {
			return note
		}
		note.candidate = true
		if withResults {
			note.unsupported = true
			return note
		}
		n, ok, _ := parseNotification(strings.TrimSpace(r.userText))
		note.n, note.unsupported = n, !ok
		note.session, note.hasSession = r.sessionID, r.sessionID != ""
		return note
	}
	return note
}

// devWaitPrefix is the part of slot command cmd (devWaitCommand) before
// its task: the known executable and runtime ("" when cmd is not the
// slot's command).
func devWaitPrefix(cmd, task string) string {
	i := strings.LastIndex(cmd, " --task "+task+" --ordinal ")
	if i <= 0 {
		return ""
	}
	return cmd[:i]
}

// devWaitStartIn reports whether assistant record r holds a Bash tool use
// whose command is a dev-wait-task command (any slot) for prefix.
func devWaitStartIn(r sessionRecord, prefix string) bool {
	if r.kind != "assistant" {
		return false
	}
	for _, b := range r.blocks {
		if b.typ == "tool_use" && b.name == toolBash && devWaitStartClass(b.input, prefix) != devStartUnrelated {
			return true
		}
	}
	return false
}

// devStartClass is a Bash start's classification (design
// 12a-dev-flow-verify r0.9): an unrelated command, a dev-wait-task
// command whose input is invalid, or a recognized dev-wait-task start.
type devStartClass int

const (
	devStartUnrelated devStartClass = iota
	devStartInvalid
	devStartWait
)

// devWaitGrammar reports whether command is exactly devWaitCommand's
// grammar for the known executable and runtime (prefix, devWaitPrefixOf):
// any valid task ID and any positive ordinal without leading zeros.
func devWaitGrammar(command, prefix string) bool {
	if prefix == "" {
		return false
	}
	rest, ok := strings.CutPrefix(command, prefix+" --task ")
	if !ok {
		return false
	}
	task, ordinal, ok := strings.Cut(rest, " --ordinal ")
	n, err := strconv.Atoi(ordinal)
	return ok && contract.ValidTaskID(task) && err == nil && n >= 1 && itoa(n) == ordinal
}

// devWaitInputOK reports whether a dev-wait-task Bash input object (raw,
// and its keys in) is valid: no duplicate key, only devWaitInputKeys,
// run_in_background exactly true, and a timeout, when present, a JSON
// number whose raw token is a positive decimal integer.
func devWaitInputOK(raw json.RawMessage, in map[string]json.RawMessage) bool {
	if !rawKeysUnique(raw) && walkDuplicateKeys(raw) != nil {
		return false
	}
	for k := range in {
		if !devWaitInputKeys[k] {
			return false
		}
	}
	var bg bool
	if !rawTyped(in["run_in_background"], &bg) || !bg {
		return false
	}
	if t, ok := in["timeout"]; ok && !devWaitTimeoutRE.MatchString(strings.Trim(string(t), " \t\r\n")) {
		return false
	}
	return true
}

// devWaitStartClass classifies a Bash tool input of any slot (r0.9),
// before any filter on run_in_background: a command outside the
// dev-wait-task grammar is unrelated; a grammar command with an invalid
// input is invalid (wait_command_mismatch, never an unrelated job); else
// it is a recognized dev-wait-task start.
func devWaitStartClass(raw json.RawMessage, prefix string) devStartClass {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil || in == nil || !devWaitGrammar(rawString(in["command"]), prefix) {
		return devStartUnrelated
	}
	if !devWaitInputOK(raw, in) {
		return devStartInvalid
	}
	return devStartWait
}

// devBackground is one background Bash start of a phase: its handle once
// its result named one, whether it is a dev-wait-task start and whether
// its one notification arrived.
type devBackground struct {
	devWait bool
	handle  string
	woke    bool
}

// deriveDevWait is qualifyDevWait with the source line of each fact
// (the sources are zero when problem is set, except refused).
//
// Every notification candidate is judged at its record, in source order,
// against all preceding background Bash starts of the phase: it must name
// a start's tool-use ID and that start's handle from its successful
// background result. A completed notification of another background job
// is a non-wake; one of a dev-wait-task start (this slot's or another's)
// must be completed and the first for that start, and wakes only that
// start. Anything else is refused as wake_representation_unsupported, even
// when later evidence would complete the wait.
func deriveDevWait(records []sessionRecord, cmd, task string) (devWait, devWaitSources, string) {
	var w devWait
	var src devWaitSources
	var cur SourceRef
	fail := func(code string) (devWait, devWaitSources, string) {
		out := devWaitSources{}
		if code == devWakeUnsupported {
			out.refused = cur
		}
		return w, out, code
	}
	prefix := devWaitPrefix(cmd, task)
	starts := map[string]*devBackground{}
	interrupted := false
	showUse := map[string]bool{}
	for _, r := range records {
		cur = r.src
		if note := devNoteOf(r); note.candidate {
			if note.unsupported {
				return fail(devWakeUnsupported)
			}
			bg := starts[note.n.toolUseID]
			if bg == nil || bg.handle == "" || bg.handle != note.n.taskID {
				// Unbound, or naming one start's use with another's handle.
				return fail(devWakeUnsupported)
			}
			if !bg.devWait {
				// Another background job's completion is not a wake.
				continue
			}
			if note.n.status != "completed" || bg.woke {
				return fail(devWakeUnsupported)
			}
			bg.woke = true
			if note.n.toolUseID == w.StartUse {
				w.Woke, src.wake = true, r.src
			}
			continue
		}
		switch r.kind {
		case "assistant":
			for _, b := range r.blocks {
				if b.typ != "tool_use" {
					continue
				}
				switch b.name {
				case toolBash:
					// Classified before the background filter (r0.9): a
					// dev-wait-task command of any slot with an invalid input
					// fails here, never counted as an unrelated job.
					class := devWaitStartClass(b.input, prefix)
					if class == devStartInvalid {
						return fail(devWaitMismatch)
					}
					var in map[string]json.RawMessage
					if json.Unmarshal(b.input, &in) != nil {
						continue
					}
					var bgFlag bool
					if rawTyped(in["run_in_background"], &bgFlag) && bgFlag && starts[b.id] == nil {
						starts[b.id] = &devBackground{devWait: class == devStartWait}
					}
					c := rawString(in["command"])
					if c != cmd {
						if strings.Contains(c, task) && (strings.Contains(c, " dev-wait-task ") || strings.Contains(c, " task wait ")) {
							return fail(devWaitMismatch)
						}
						continue
					}
					if w.StartUse != "" {
						return fail(devDuplicateWait)
					}
					if class != devStartWait {
						return fail(devWaitMismatch)
					}
					w.StartUse, src.start = b.id, r.src
				case toolTaskShow:
					// The whole input by the MCP server's own validator: exactly
					// this task's id; lines (how much log tail to read, within
					// the server's range) is the one field a consuming read may
					// legitimately choose.
					id, _, err := contract.ParseTaskShowArgs(b.input)
					if err != nil {
						if strings.Contains(string(b.input), task) {
							// A task_show of this task the MCP tool would refuse
							// is never its consumption.
							return fail(devWaitMismatch)
						}
						continue
					}
					if id == task && w.Woke && !w.Consumed {
						showUse[b.id] = true
					}
				}
			}
		case "user":
			handled := false
			for _, b := range r.blocks {
				if b.typ != "tool_result" {
					continue
				}
				handled = true
				switch {
				case b.toolUseID == w.StartUse && w.StartUse != "" && w.Handle == "":
					m := backgroundRE.FindStringSubmatch(strings.TrimSpace(b.text))
					if b.isError || m == nil {
						return fail(devWaitMismatch)
					}
					w.Handle, src.handle = m[1], r.src
					starts[b.toolUseID].handle = m[1]
				case starts[b.toolUseID] != nil && starts[b.toolUseID].handle == "":
					// Another background start's handle, when its result
					// names one.
					if m := backgroundRE.FindStringSubmatch(strings.TrimSpace(b.text)); !b.isError && m != nil {
						starts[b.toolUseID].handle = m[1]
					}
				case showUse[b.toolUseID] && !w.Consumed && taskShowReplaced(b.text):
					// The vendor's persisted-output replacement (r0.10): no
					// evidence; the wait stays unconsumed.
				case showUse[b.toolUseID] && !w.Consumed:
					v, err := contract.ParseTaskShowResponse([]byte(b.text))
					if err == nil && v.TaskID == task && contract.TaskTerminal(v.State) {
						w.Consumed, w.ConsumeUse, w.Automatic, src.consume = true, b.toolUseID, !interrupted, r.src
					}
				}
			}
			if handled || !r.hasText {
				continue
			}
			// An ordinary user message between the wake and its
			// consumption.
			if w.Woke && !w.Consumed {
				interrupted = true
			}
		}
	}
	if w.Consumed && !w.Automatic {
		return fail(devWakeNotAutomatic)
	}
	return w, src, ""
}

// taskShowReplacedFirstRE is the first sentence Claude Code 2.1.306
// substitutes for a tool result over its output limit (fourth probe,
// design 12a-dev-flow-verify r0.10c): the size N in decimal digits,
// optionally comma-grouped, and the saved path P, which begins with "/",
// has at least one further character and holds no CR, LF or NUL. The final
// period is punctuation, not part of P.
var taskShowReplacedFirstRE = regexp.MustCompile(`^Error: result \((?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+) characters across 1 line\) exceeds maximum allowed tokens\. Output has been saved to (/[^\r\n\x00]+)\.$`)

// taskShowReplacedGuidance is the captured guidance that follows the
// first sentence in the multiline form, with the first sentence's P
// substituted literally into its first bullet (P is opaque text: never
// captured a second time, resolved or opened). The lines join with LF.
func taskShowReplacedGuidance(p string) string {
	return strings.Join([]string{
		"Format: Plain text",
		"Search within the file for specific content, and use jq if the content is JSON.",
		"REQUIREMENTS FOR SUMMARIZATION/ANALYSIS/REVIEW:",
		"- You MUST read the content from the file at " + p + " in sequential chunks until 100% of the content has been read.",
		"- Note: this file's lines are too long for Read's offset/limit chunking. If a shell tool is available, slice by character range (e.g. python read()[A:B], dd, or cut -c) instead.",
		"- If you receive truncation warnings when reading the file, reduce the chunk size until you have read 100% of the content without truncation.",
		"- Before producing ANY summary or analysis, you MUST explicitly describe what portion of the content you have read. ***If you did not read the entire content, you MUST explicitly state this.***",
		"- If after a few attempts you cannot read the file (file not found, lines too long for Read's offset/limit, no shell access), STOP retrying. Summarize what you were able to read, explicitly state which portion you could not read and why, and proceed.",
	}, "\n")
}

// taskShowReplaced reports a task_show result replaced by the vendor's
// persisted-output text (r0.10c), in one of its two captured forms: the
// first sentence alone, with an optional single LF or CRLF ending; or the
// first sentence, LF, and exactly the captured eight guidance lines with
// the same P in its first bullet, compared byte for byte, with an optional
// single trailing LF. Any other text is not this replacement. It is no
// task evidence at all (never a consuming read, never a response to
// bind); a later, separately paired valid read may still consume.
func taskShowReplaced(text string) bool {
	first, rest, multiline := strings.Cut(text, "\n")
	switch {
	case !multiline:
		return taskShowReplacedFirstRE.MatchString(first)
	case rest == "":
		// The one-line form's LF or CRLF ending.
		return taskShowReplacedFirstRE.MatchString(strings.TrimSuffix(first, "\r"))
	}
	m := taskShowReplacedFirstRE.FindStringSubmatch(first)
	if m == nil {
		return false
	}
	want := taskShowReplacedGuidance(m[1])
	return rest == want || rest == want+"\n"
}

// devWaitCommand is the simple background command for one workflow slot.
func devWaitCommand(reale2e, runtime, task string, ordinal int) string {
	return devWaitPrefixOf(reale2e, runtime) + " --task " + task + " --ordinal " + itoa(ordinal)
}

// devWaitPrefixOf is every slot command's part before its task, for the
// known executable and runtime.
func devWaitPrefixOf(reale2e, runtime string) string {
	return reale2e + " dev-wait-task --run " + runtime
}

// devWaitFiles is one slot's captured wait output.
type devWaitFiles struct {
	Result, Stderr, Exit []byte
	OKR, OKE, OKX        bool
}

// readDevWaitFiles reads ordinal's captured wait files from the runtime.
func readDevWaitFiles(runtime string, ordinal int) devWaitFiles {
	var f devWaitFiles
	dir := []string{"coord", "waits", hopDir(ordinal - 1)}
	read := func(name string, limit int64) ([]byte, bool) {
		b, err := readRuntimeFile(runtime, limit, dir[0], dir[1], dir[2], name)
		return b, err == nil
	}
	f.Result, f.OKR = read(runtimeWaitResult, maxTextFile)
	f.Stderr, f.OKE = read(runtimeWaitStderr, maxTextFile)
	f.Exit, f.OKX = read(runtimeWaitExit, 64)
	return f
}

// DevWaitRecord is a slot's wait evidence record.
type DevWaitRecord struct {
	Ordinal      int     `json:"ordinal"`
	Task         string  `json:"task"`
	Command      string  `json:"command"`
	StartSeq     int     `json:"start_seq"`
	ConsumedSeq  int     `json:"consumed_seq"`
	Wait         devWait `json:"wake"`
	ExitCode     *int    `json:"exit_code"`
	ResultSHA256 string  `json:"result_sha256"`
	Winner       string  `json:"winner"`
	WinnerState  string  `json:"winner_state"`
	StderrBytes  int     `json:"stderr_bytes"`
}

// captured reports whether the wait files record exit 0 and a terminal
// result for task, filling rec.
func (f devWaitFiles) captured(task string, rec *DevWaitRecord) bool {
	if f.OKX {
		if n, err := strconv.Atoi(strings.TrimSpace(string(f.Exit))); err == nil {
			rec.ExitCode = &n
		}
	}
	if f.OKR {
		rec.ResultSHA256 = sha256Hex(f.Result)
		if wr, err := contract.ParseWaitResponse([]byte(strings.TrimSpace(string(f.Result))), []string{task}, 40*time.Second); err == nil &&
			wr.Status == contract.WaitTerminal && wr.Task != nil {
			rec.Winner, rec.WinnerState = wr.Winner, wr.Task.State
		}
	}
	rec.StderrBytes = len(f.Stderr)
	return rec.ExitCode != nil && *rec.ExitCode == 0 && rec.Winner == task && contract.TaskTerminal(rec.WinnerState)
}

// phaseGateRE is a phase result's gate line.
var phaseGateRE = regexp.MustCompile(`^gate=(owner|answer) generation=(0|[1-9][0-9]{0,8})$`)

// parsePhaseGate reads the single standalone gate line of a phase result.
func parsePhaseGate(text string) (string, int, error) {
	gate, gen, n := "", 0, 0
	for _, l := range scanLines(text) {
		if !l.plain || !strings.HasPrefix(l.text, "gate=") {
			continue
		}
		m := phaseGateRE.FindStringSubmatch(l.text)
		if m == nil {
			return "", 0, fmt.Errorf("malformed gate line")
		}
		gate, n = m[1], n+1
		gen, _ = strconv.Atoi(m[2])
	}
	if n != 1 {
		return "", 0, fmt.Errorf("want exactly one gate line, got %d", n)
	}
	return gate, gen, nil
}
