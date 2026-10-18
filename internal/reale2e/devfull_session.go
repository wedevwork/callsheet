package reale2e

import (
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
// ASSUMPTION (unverified, the design's repeated-resume probe settles it):
// the coordinator's print-mode stream carries the background completion
// notification as a user record in the same phase.

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
	devWakeMismatch     = "wake_mismatch"
	devWakeNotAutomatic = "wake_not_automatic"
	devWaitFailed       = "wait_failed"
)

// devStream is one decoded phase stream.
type devStream struct {
	records   []sessionRecord
	sessionID string
	// problem is the first fatal problem's fixed code ("" none).
	problem string
	// result is the last result record's text and ok (the phase result).
	resultText string
	resultOK   bool
	hasResult  bool
}

// decodeDevStream decodes a phase stream. want is the captured session a
// resumed phase must keep ("" for the first phase).
func decodeDevStream(stdout []byte, want string) devStream {
	d := &deriver{codes: map[string]bool{}}
	s := devStream{records: d.decodeLines("phase-stream", stdout), sessionID: want}
	fail := func(code string) {
		if s.problem == "" {
			s.problem = code
		}
	}
	if len(d.findings) > 0 {
		fail(devSessionMalformed)
	}
	for _, r := range s.records {
		if r.sessionID != "" {
			if s.sessionID == "" {
				s.sessionID = r.sessionID
			} else if r.sessionID != s.sessionID {
				fail(devSessionMismatch)
			}
		}
		switch r.kind {
		case "init":
			if r.malformed || r.init.PermissionMode != autoModeFlag || r.sessionID == "" {
				fail(devAutoModeMissing)
			}
		case "result":
			if len(r.denials) > 0 {
				fail(devToolDenied)
			}
			if r.malformed || !r.resultOK {
				fail(devResultFailed)
			}
			s.hasResult, s.resultOK = true, !r.malformed && r.resultOK
		case "user":
			for _, b := range r.blocks {
				if b.typ == "tool_result" && b.isError {
					fail(devToolFailed)
				}
			}
		case "assistant":
			for _, b := range r.blocks {
				if b.typ == "tool_use" && b.name == toolTaskWait {
					fail(devLongPoll)
				}
			}
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

// devWaitInputKeys are a wait's only Bash input keys.
var devWaitInputKeys = map[string]bool{"command": true, "run_in_background": true, "description": true}

// qualifyDevWait derives the slot's wait evidence for task from a phase's
// decoded records. problem is a fixed code when the evidence is wrong;
// incomplete evidence without a problem may still complete.
func qualifyDevWait(records []sessionRecord, cmd, task string) (devWait, string) {
	var w devWait
	interrupted := false
	showUse := map[string]bool{}
	for _, r := range records {
		switch r.kind {
		case "assistant":
			for _, b := range r.blocks {
				if b.typ != "tool_use" {
					continue
				}
				switch b.name {
				case toolBash:
					var in map[string]json.RawMessage
					if json.Unmarshal(b.input, &in) != nil {
						continue
					}
					c := rawString(in["command"])
					if c != cmd {
						if strings.Contains(c, task) && (strings.Contains(c, " dev-wait-task ") || strings.Contains(c, " task wait ")) {
							return w, devWaitMismatch
						}
						continue
					}
					if w.StartUse != "" {
						return w, devDuplicateWait
					}
					var bg bool
					if !rawTyped(in["run_in_background"], &bg) || !bg {
						return w, devWaitMismatch
					}
					for k := range in {
						if !devWaitInputKeys[k] {
							return w, devWaitMismatch
						}
					}
					w.StartUse = b.id
				case toolTaskShow:
					var in struct {
						ID string `json:"id"`
					}
					if json.Unmarshal(b.input, &in) == nil && in.ID == task && w.Woke && !w.Consumed {
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
						return w, devWaitMismatch
					}
					w.Handle = m[1]
				case showUse[b.toolUseID] && !w.Consumed:
					v, err := contract.ParseTaskShowResponse([]byte(b.text))
					if err == nil && v.TaskID == task && contract.TaskTerminal(v.State) {
						w.Consumed, w.ConsumeUse, w.Automatic = true, b.toolUseID, !interrupted
					}
				}
			}
			if handled || !r.hasText {
				continue
			}
			n, ok, unsupported := parseNotification(strings.TrimSpace(r.userText))
			switch {
			case unsupported:
				return w, devWakeUnsupported
			case ok && w.Handle != "" && n.taskID == w.Handle:
				if n.toolUseID != w.StartUse || w.Woke {
					return w, devWakeMismatch
				}
				if n.status != "completed" {
					return w, devWaitFailed
				}
				w.Woke = true
			case ok:
				// Another background job's completion is not this wake.
			case w.Woke && !w.Consumed:
				interrupted = true
			}
		}
	}
	if w.Consumed && !w.Automatic {
		return w, devWakeNotAutomatic
	}
	return w, ""
}

// devWaitCommand is the simple background command for one workflow slot.
func devWaitCommand(reale2e, runtime, task string, ordinal int) string {
	return reale2e + " dev-wait-task --run " + runtime + " --task " + task + " --ordinal " + itoa(ordinal)
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
