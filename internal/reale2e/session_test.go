package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// synthRun is a synthetic headless session for derivation tests: both
// phases' streams, both transcript snapshots, the launches and the
// supervisor's own events, built through the fake coordinator's writer.
type synthRun struct {
	t        *testing.T
	runID    string
	tasks    [4]string
	sess     *fakeSession
	out      [2]*bytes.Buffer
	snap     [2][]byte
	launches [2]*LaunchRecord
	events   []Event
	handles  [4]string
	waitUse  [4]string
	showUse  [4]string
	// Variations applied while building.
	skipHop     int
	endAfterHop int
	before      func(r *synthRun, phase, hop int)
	afterWake   func(r *synthRun, hop int)
	waitInput   func(hop int, in map[string]any)
	dispatchIn  func(hop int, in map[string]any)
	phase2SID   string
	noConsume   map[int]bool
	// showOut replaces hop i's task_show response; showFirst reads the
	// task before its completion notification arrives.
	showOut   func(i int) string
	showFirst map[int]bool
}

// shownTask is the strict task_show response of a succeeded task id, as
// the Callsheet MCP server renders it.
func shownTask(t *testing.T, id string) string {
	t.Helper()
	p := newFakePlane(t, newAutoClock())
	role, err := p.AddRole(context.Background(), contract.RoleConfig{ID: HopDesigner, Name: HopDesigner, Node: hexID("n_", 1), Adapter: "codex", Model: "m",
		Effort: "low", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: HopDesigner}, Goal: "g", Acceptance: "a",
		RequestedBy: contract.RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"}}
	v := fakeView(id, req, role, p.clock.Now())
	p.tasks[id] = &v
	p.finish(id, contract.TaskSucceeded, ptr("done\n"), false, nil)
	b, err := json.Marshal(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: *p.tasks[id]})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// synthTemplate is hop i's rendered wait template.
func synthTemplate(i int) string {
	return waitCommand(permissionValues{callsheet: "/bin/callsheet", reale2e: "/bin/reale2e", url: "https://127.0.0.1:1", ca: "/rt/ca.crt", runtime: "/rt"}, i)
}

func newSynth(t *testing.T) *synthRun {
	r := &synthRun{t: t, runID: "20261010T120000Z-01020304", skipHop: -1, endAfterHop: -1}
	for i := range Hops {
		r.tasks[i] = hexID("t_", 10+i)
	}
	r.out = [2]*bytes.Buffer{{}, {}}
	r.sess = &fakeSession{t: t, sid: testSession, cwd: "/rt/coord"}
	r.sess.stream = [2]io.Writer{r.out[0], r.out[1]}
	for p := 1; p <= 2; p++ {
		l := &LaunchRecord{Schema: LaunchSchema, Phase: p, Executable: "/bin/claude", Argv: []string{"-p", fmt.Sprintf("Phase %d prompt.", p)}, Cwd: "/rt/coord"}
		for i := range Hops {
			l.WaitTemplates = append(l.WaitTemplates, synthTemplate(i))
		}
		r.launches[p-1] = l
	}
	return r
}

// event appends a supervisor event and returns its sequence.
func (r *synthRun) event(typ, hop, task, handle string) int {
	r.events = append(r.events, Event{Seq: len(r.events) + 1, Type: typ, Hop: hop, Task: task, Handle: handle, Time: "2026-10-10T12:00:00Z"})
	return len(r.events)
}

// hop runs hop i inside phase p: dispatch, observation, background wait,
// completion notification and consumption.
func (r *synthRun) hop(p, i int) {
	s := r.sess
	if r.before != nil {
		r.before(r, p, i)
	}
	in := map[string]any{"target": map[string]string{"kind": "id", "value": Hops[i]}, "goal": "g", "acceptance": "a",
		"payload": []string{markerFor(r.runID, Hops[i])}, "workspace": "w", "base": strings.Repeat("a", 40)}
	if r.dispatchIn != nil {
		r.dispatchIn(i, in)
	}
	s.use(p, toolDispatch, in, `{"task_id":"`+r.tasks[i]+`","state":"running"}`, false)
	r.event(EvObserved, Hops[i], r.tasks[i], "")
	r.handles[i] = fmt.Sprintf("bash_%d", i+1)
	win := map[string]any{"command": strings.ReplaceAll(synthTemplate(i), "TASK_ID", r.tasks[i]), "run_in_background": true, "description": "wait"}
	if r.waitInput != nil {
		r.waitInput(i, win)
	}
	r.waitUse[i] = s.use(p, toolBash, win, "Command running in background with ID: "+r.handles[i]+". Output is being written to: /tmp/o", false)
	if r.noConsume[i] {
		return
	}
	show := shownTask(r.t, r.tasks[i])
	if r.showOut != nil {
		show = r.showOut(i)
	}
	if r.showFirst[i] {
		s.use(p, toolTaskShow, map[string]any{"id": r.tasks[i]}, show, false)
		s.notify(r.handles[i], r.waitUse[i])
		return
	}
	s.notify(r.handles[i], r.waitUse[i])
	if r.afterWake != nil {
		r.afterWake(r, i)
	}
	r.showUse[i] = s.use(p, toolTaskShow, map[string]any{"id": r.tasks[i]}, show, false)
}

// streamLines edits phase p's (0-based) raw stream line by line.
func streamLines(in *SessionInput, p int, f func(lines []string) []string) {
	lines := strings.Split(strings.TrimSuffix(string(in.Streams[p]), "\n"), "\n")
	in.Streams[p] = []byte(strings.Join(f(lines), "\n") + "\n")
}

// moveResult moves a stream's terminal result record to index at.
func moveResult(lines []string, at int) []string {
	last := lines[len(lines)-1]
	lines = slices.Insert(lines[:len(lines)-1], at, last)
	return lines
}

// build writes both phases and returns the derivation input.
func (r *synthRun) build() SessionInput {
	s := r.sess
	r.event(EvPhaseStarted, "", "", phaseHandle(1))
	s.init(1, testSession)
	s.prompt(promptOf(ProcSpec{Args: r.launches[0].Argv}))
	for _, i := range []int{0, 1} {
		if i == r.skipHop {
			continue
		}
		r.hop(1, i)
		if i == r.endAfterHop {
			break
		}
	}
	s.result(1, true, nil)
	r.event(EvPhaseExited, "", "", phaseHandle(1))
	r.snap[0] = []byte(strings.Join(s.transcript, "\n") + "\n")
	r.event(EvAwaitingOwner, HopDesignReviewer, r.tasks[1], "")
	r.event(EvOwnerDecision, HopDesignReviewer, r.tasks[1], "")
	r.event(EvPhaseStarted, "", "", phaseHandle(2))
	s.init(2, map[bool]string{false: testSession, true: r.phase2SID}[r.phase2SID != ""])
	s.prompt(promptOf(ProcSpec{Args: r.launches[1].Argv}))
	for _, i := range []int{2, 3} {
		r.hop(2, i)
	}
	s.result(2, true, nil)
	r.event(EvPhaseExited, "", "", phaseHandle(2))
	r.snap[1] = []byte(strings.Join(s.transcript, "\n") + "\n")
	return SessionInput{RunID: r.runID, Streams: [2][]byte{r.out[0].Bytes(), r.out[1].Bytes()}, Transcripts: r.snap, Launches: r.launches, Events: r.events}
}

// codesOfMap returns a map's finding codes.
func codesOfMap(em EventMap) []string { return findingCodes(em) }

// TestSessionDerivation (UT-3): the machine-derived event map from
// structured stream and transcript records: the canonical entries with
// deduplicated sources, supervisor sequences and automatic wakes; and each
// defect as its fixed finding: missing or rewritten transcripts, unknown
// wake envelopes, a phase ending before its hops, malformed, duplicated or
// conflicting records, dangling results, denied and failed tools, long
// polls, changed wait commands, unknown dispatches, a changed session and
// ordinary user input. False notification prose is never a wake.
func TestSessionDerivation(t *testing.T) {
	t.Parallel()
	r := newSynth(t)
	in := r.build()
	em := DeriveSession(in)
	if len(em.Findings) != 0 || em.SessionID != testSession || em.Source != MapSourceDerived || len(em.Init) != 2 || em.Init[1].Model != "claude-opus-5-5" {
		t.Fatalf("clean derivation %+v", em)
	}
	var kinds []string
	for _, e := range em.Entries {
		kinds = append(kinds, e.Kind)
	}
	want := []string{MapLaunch}
	for i := range Hops {
		if i == 2 {
			want = append(want, MapPause, MapDecision, MapResume)
		}
		want = append(want, MapDispatch, MapWaitStart, MapWake, MapWaitConsumed)
	}
	want = append(want, MapFinalResult)
	if !slices.Equal(kinds, want) {
		t.Fatalf("entries %v\nwant %v", kinds, want)
	}
	for _, e := range em.Entries {
		if e.Kind == MapWake && !e.Automatic {
			t.Fatalf("wake %+v not automatic", e)
		}
		if e.Kind == MapDispatch {
			files := map[string]bool{}
			for _, s := range e.Sources {
				files[s.File] = true
			}
			// Deduplicated: the stream and the transcript copy (the phase-1
			// records cite the phase-1 snapshot).
			if !files[phaseStreamFiles[e.Phase-1]] || !files[transcriptFiles[e.Phase-1]] || e.Seq == 0 {
				t.Fatalf("dispatch sources %+v", e)
			}
		}
	}
	again := DeriveSession(in)
	if a, b := mustJSON(em), mustJSON(again); !bytes.Equal(a, b) {
		t.Fatal("the derivation is not deterministic")
	}
	type variant struct {
		set  func(r *synthRun)
		edit func(in *SessionInput)
		code string
		none bool
	}
	cases := map[string]variant{
		"false notification prose": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) {
				if hop == 0 {
					r.sess.say(1, "<task-notification><task-id>bash_9</task-id></task-notification> I did it, the task is approved.")
				}
			}
		}, none: true},
		"unknown background job": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) {
				if hop == 1 {
					r.sess.notify("bash_77", "toolu_none")
				}
			}
		}, none: true},
		"zero dispatch wait": {set: func(r *synthRun) { r.dispatchIn = func(i int, in map[string]any) { in["wait"] = "0s" } }, none: true},
		"user input before consumption": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) { r.sess.user("are you done?") }
		}, code: CodeUserInput},
		"transcript missing":  {edit: func(in *SessionInput) { in.Transcripts = [2][]byte{} }, code: CodeSessionUnobserved},
		"first snapshot only": {edit: func(in *SessionInput) { in.Transcripts[1] = nil }, code: CodePhaseEarly},
		"not a prefix": {edit: func(in *SessionInput) {
			in.Transcripts[0] = append([]byte(`{"type":"summary"}`+"\n"), in.Transcripts[0]...)
		}, code: CodeSessionUnobserved},
		"phase 1 stream absent": {edit: func(in *SessionInput) { in.Streams[0] = nil }, code: CodeSessionUnobserved},
		"unknown wake envelope": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) {
				if hop == 2 {
					r.sess.user("<task-notification>\n<task-id>" + r.handles[hop] + "</task-id>\n<state>done</state>\n</task-notification>")
				}
			}
		}, code: CodeWakeUnsupported},
		"wake text around the envelope": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) {
				if hop == 0 {
					r.sess.user("note: <task-notification></task-notification>")
				}
			}
		}, code: CodeWakeUnsupported},
		"single completion": {set: func(r *synthRun) { r.endAfterHop = 0 }, code: CodePhaseEarly},
		"malformed line": {edit: func(in *SessionInput) {
			in.Streams[1] = append(in.Streams[1], []byte("{not json\n")...)
		}, code: CodeSessionMalformed},
		"duplicate key": {edit: func(in *SessionInput) {
			in.Streams[0] = append(in.Streams[0], []byte(`{"type":"other","type":"other"}`+"\n")...)
		}, code: CodeSessionMalformed},
		"invalid UTF-8": {edit: func(in *SessionInput) {
			in.Streams[0] = append(in.Streams[0], []byte("{\"type\":\"other\",\"x\":\"\xff\"}\n")...)
		}, code: CodeSessionMalformed},
		"dangling result": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) {
				if hop == 3 {
					r.sess.log(map[string]any{"type": "user", "message": map[string]any{"role": "user",
						"content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_ghost", "content": "x"}}}})
				}
			}
		}, code: CodeSessionMalformed},
		"conflicting copy": {edit: func(in *SessionInput) {
			in.Streams[0] = bytes.Replace(in.Streams[0], []byte(`"description":"wait"`), []byte(`"description":"changed"`), 1)
		}, code: CodeSessionMalformed},
		"stream use not in the transcript": {edit: func(in *SessionInput) {
			in.Streams[1] = append(in.Streams[1], []byte(`{"type":"assistant","session_id":"`+testSession+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_x","name":"Bash","input":{}}]}}`+"\n")...)
		}, code: CodeSessionUnobserved},
		"duplicate init": {edit: func(in *SessionInput) {
			in.Streams[0] = append(in.Streams[0], bytes.SplitAfter(in.Streams[0], []byte("\n"))[0]...)
		}, code: CodeSessionMalformed},
		"two results": {edit: func(in *SessionInput) {
			lines := bytes.SplitAfter(in.Streams[1], []byte("\n"))
			in.Streams[1] = append(in.Streams[1], lines[len(lines)-2]...)
		}, code: CodeSessionMalformed},
		"denied tool": {edit: func(in *SessionInput) {
			in.Streams[1] = bytes.Replace(in.Streams[1], []byte(`"is_error":false,"result":`), []byte(`"is_error":false,"permission_denials":[{"tool_name":"Bash","tool_use_id":"toolu_0001"}],"result":`), 1)
		}, code: CodeToolDenied},
		"failed tool": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) {
				if hop == 1 {
					r.sess.use(1, "Read", map[string]any{"file_path": "/x"}, "File does not exist.", true)
				}
			}
		}, code: CodeToolFailed},
		"long poll": {set: func(r *synthRun) {
			r.afterWake = func(r *synthRun, hop int) {
				if hop == 0 {
					r.sess.use(1, toolTaskWait, map[string]any{"task_ids": []string{r.tasks[0]}}, `{}`, false)
				}
			}
		}, code: CodeLongPoll},
		"dispatch that waits": {set: func(r *synthRun) {
			r.dispatchIn = func(i int, in map[string]any) {
				if i == 2 {
					in["wait"] = "5m"
				}
			}
		}, code: CodeLongPoll},
		"changed wait command": {set: func(r *synthRun) {
			r.waitInput = func(i int, in map[string]any) {
				if i == 1 {
					in["command"] = strings.Replace(in["command"].(string), ` --code "$?"`, " --code 0", 1)
				}
			}
		}, code: CodeWaitMismatch},
		"foreground wait": {set: func(r *synthRun) {
			r.waitInput = func(i int, in map[string]any) {
				if i == 3 {
					in["run_in_background"] = false
				}
			}
		}, code: CodeWaitMismatch},
		"wait with a timeout": {set: func(r *synthRun) {
			r.waitInput = func(i int, in map[string]any) {
				if i == 0 {
					in["timeout"] = 600000
				}
			}
		}, code: CodeWaitMismatch},
		"unknown dispatch": {set: func(r *synthRun) {
			r.dispatchIn = func(i int, in map[string]any) {
				if i == 1 {
					in["target"] = map[string]string{"kind": "name", "value": Hops[1]}
				}
			}
		}, code: CodeDispatchMismatch},
		"changed session": {set: func(r *synthRun) { r.phase2SID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" }, code: CodeSessionMismatch},
		// A phase result binds to its phase's init session with its
		// required fields typed; otherwise it is no successful result.
		"result without session": {edit: func(in *SessionInput) {
			in.Streams[0] = bytes.Replace(in.Streams[0], []byte(`,"session_id":"`+testSession+`","subtype":"success"`), []byte(`,"subtype":"success"`), 1)
		}, code: CodeSessionUnobserved},
		"result is_error not a boolean": {edit: func(in *SessionInput) {
			in.Streams[1] = bytes.Replace(in.Streams[1], []byte(`"is_error":false,"result"`), []byte(`"is_error":"false","result"`), 1)
		}, code: CodeSessionMalformed},
		"result is_error missing": {edit: func(in *SessionInput) {
			in.Streams[0] = bytes.Replace(in.Streams[0], []byte(`"is_error":false,"result"`), []byte(`"result"`), 1)
		}, code: CodeSessionMalformed},
		"result subtype not a string": {edit: func(in *SessionInput) {
			in.Streams[0] = bytes.Replace(in.Streams[0], []byte(`"subtype":"success","type":"result"`), []byte(`"subtype":1,"type":"result"`), 1)
		}, code: CodeSessionMalformed},
		// Raw stream chronology: init first, the required consumption
		// before the terminal result, no phase work after it.
		"result before init": {edit: func(in *SessionInput) {
			streamLines(in, 0, func(l []string) []string { return moveResult(l, 0) })
		}, code: CodeSessionMalformed},
		"result before consumption": {edit: func(in *SessionInput) {
			streamLines(in, 0, func(l []string) []string { return moveResult(l, 1) })
		}, code: CodePhaseEarly},
		"phase 2 result first": {edit: func(in *SessionInput) {
			streamLines(in, 1, func(l []string) []string { return moveResult(l, 1) })
		}, code: CodePhaseEarly},
		"work after the result": {edit: func(in *SessionInput) {
			streamLines(in, 1, func(l []string) []string {
				return append(l, `{"type":"assistant","session_id":"`+testSession+`","message":{"role":"assistant","content":[{"type":"text","text":"more"}]}}`)
			})
		}, code: CodeSessionMalformed},
		// A task_show is consumption only when its linked response shows
		// that hop's task terminal, after the completion notification.
		"task_show of another task": {set: func(r *synthRun) {
			r.showOut = func(i int) string {
				if i == 0 {
					return `{"task_id":"wrong","state":"running"}`
				}
				return shownTask(r.t, r.tasks[i])
			}
		}, code: CodePhaseEarly},
		"task_show of a running task": {set: func(r *synthRun) {
			r.showOut = func(i int) string {
				if i == 3 {
					return strings.Replace(shownTask(r.t, r.tasks[i]), `"state":"succeeded"`, `"state":"running"`, 1)
				}
				return shownTask(r.t, r.tasks[i])
			}
		}, code: CodePhaseEarly},
		"task_show before the wake": {set: func(r *synthRun) { r.showFirst = map[int]bool{1: true} }, code: CodePhaseEarly},
		"another cwd": {edit: func(in *SessionInput) {
			in.Launches[1] = &LaunchRecord{Argv: in.Launches[1].Argv, Cwd: "/elsewhere", WaitTemplates: in.Launches[1].WaitTemplates}
		}, code: CodeSessionMismatch},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newSynth(t)
			if v.set != nil {
				v.set(r)
			}
			in := r.build()
			if v.edit != nil {
				v.edit(&in)
			}
			em := DeriveSession(in)
			codes := codesOfMap(em)
			if v.none {
				if len(codes) != 0 || len(entriesOf(em, MapWake, "")) != 4 {
					t.Fatalf("findings %v entries %d", codes, len(em.Entries))
				}
				return
			}
			if !slices.Contains(codes, v.code) {
				t.Fatalf("findings %v, want %s", codes, v.code)
			}
		})
	}
	t.Run("user input breaks the automatic wake", func(t *testing.T) {
		t.Parallel()
		r := newSynth(t)
		r.afterWake = func(r *synthRun, hop int) {
			if hop == 0 {
				r.sess.user("hello")
			}
		}
		em := DeriveSession(r.build())
		for _, e := range entriesOf(em, MapWake, "") {
			if e.Automatic != (e.Hop != HopDesigner) {
				t.Fatalf("wake %+v", e)
			}
		}
	})
	t.Run("transcript-only consumption", func(t *testing.T) {
		t.Parallel()
		// A task_show response missing from the raw stream is consumption
		// when the transcript orders it before a later record of the phase
		// that its stream holds before the result (the next hop's
		// dispatch); after a phase's last hop no such record exists, so
		// the result may have come first.
		for _, c := range []struct {
			name   string
			hop    int
			andUse bool
			early  bool
		}{
			{"response", 0, false, false},
			{"tool use and response", 0, true, false},
			{"phase 2 response", 2, false, false},
			{"phase 1 last hop", 1, false, true},
			{"phase 2 last hop", 3, true, true},
		} {
			r := newSynth(t)
			in := r.build()
			id := r.showUse[c.hop]
			streamLines(&in, hopPhase(c.hop)-1, func(l []string) []string {
				return slices.DeleteFunc(l, func(s string) bool {
					return strings.Contains(s, `"tool_use_id":"`+id+`"`) || c.andUse && strings.Contains(s, `"id":"`+id+`"`)
				})
			})
			em := DeriveSession(in)
			codes := codesOfMap(em)
			if c.early != slices.Contains(codes, CodePhaseEarly) || !c.early && (len(codes) != 0 || len(entriesOf(em, MapWaitConsumed, Hops[c.hop])) != 1) {
				t.Errorf("%s: findings %v", c.name, codes)
			}
		}
	})
	t.Run("a later phase does not wake an earlier hop", func(t *testing.T) {
		t.Parallel()
		r := newSynth(t)
		// Hop 2's completion is consumed only after the resume: the phase-2
		// prompt never substitutes for a phase-1 wake.
		r.noConsume = map[int]bool{1: true}
		r.before = func(r *synthRun, p, hop int) {
			if hop == 2 {
				r.sess.notify(r.handles[1], r.waitUse[1])
				r.sess.use(2, toolTaskShow, map[string]any{"id": r.tasks[1]}, "{}", false)
			}
		}
		em := DeriveSession(r.build())
		if !slices.Contains(codesOfMap(em), CodePhaseEarly) || len(entriesOf(em, MapWake, HopDesignReviewer)) != 0 ||
			len(entriesOf(em, MapWaitConsumed, HopDesignReviewer)) != 0 {
			t.Fatalf("findings %v entries %+v", codesOfMap(em), em.Entries)
		}
	})
	t.Run("notifications", func(t *testing.T) {
		t.Parallel()
		for text, want := range map[string]string{
			"<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>u1</tool-use-id>\n<status>completed</status>\n</task-notification>":                             "ok",
			"<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>u1</tool-use-id>\n<status>failed</status>\n<output-file>/o</output-file>\n</task-notification>": "ok",
			"plain text": "none",
			"<task-notification>\n<task-id>b1</task-id>\n<task-id>b2</task-id>\n<tool-use-id>u</tool-use-id>\n<status>x</status>\n</task-notification>": "unsupported",
			"<task-notification>\n<task-id>b1</task-id>\n<status>x</status>\n</task-notification>":                                                      "unsupported",
			"<task-notification>\n<task-id>b1</tool-use-id>\n</task-notification>":                                                                      "unsupported",
			"<task-notification>\n<extra>b1</extra>\n</task-notification>":                                                                              "unsupported",
		} {
			_, ok, unsupported := parseNotification(text)
			got := map[[2]bool]string{{true, false}: "ok", {false, false}: "none", {false, true}: "unsupported"}[[2]bool{ok, unsupported}]
			if got != want {
				t.Errorf("%q = %s, want %s", text, got, want)
			}
		}
		if resultText([]byte(`[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`)) != "ab" || canonicalJSON([]byte("{")) != "{" ||
			handleOf(map[string]int{}, 0) != "" || len(lineOffsets([]byte("a\nb"))) != 2 || launchPrompt(&LaunchRecord{Argv: []string{"-p"}}) != "" {
			t.Fatal("helpers")
		}
	})
}
