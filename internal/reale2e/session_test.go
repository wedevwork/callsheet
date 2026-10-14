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
	// perTurn ends the running turn of a phase (0-based) after each
	// background wait starts: its result, then the next turn's init, as
	// headless claude -p emits one init per turn (A3.9).
	perTurn [2]bool
}

// phaseSID is the session ID phase p's init records carry.
func (r *synthRun) phaseSID(p int) string {
	if p == 2 && r.phase2SID != "" {
		return r.phase2SID
	}
	return testSession
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
	return waitCommand(permissionValues{reale2e: "/bin/reale2e", runtime: "/rt"}, i)
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
	if r.perTurn[p-1] {
		// The turn ends while the wait runs; the completion starts the
		// next turn (the notification is a transcript record only).
		s.result(p, true, nil)
		s.init(p, r.phaseSID(p))
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
	s.init(2, r.phaseSID(2))
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
		// A3.9: an init opens a turn; one after the final result opens a
		// turn that never closes.
		"init after the final result": {edit: func(in *SessionInput) {
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
					in["command"] = strings.Replace(in["command"].(string), " --hop ", " --hop  ", 1)
				}
			}
		}, code: CodeWaitMismatch},
		"redirected wait command": {set: func(r *synthRun) {
			r.waitInput = func(i int, in map[string]any) {
				if i == 2 {
					in["command"] = in["command"].(string) + " >/dev/null"
				}
			}
		}, code: CodeWaitMismatch},
		"direct CLI wait": {set: func(r *synthRun) {
			r.waitInput = func(i int, in map[string]any) {
				if i == 0 {
					in["command"] = "/bin/callsheet task wait x --until-done --json"
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

// recordLines returns the indexes of phase p's (0-based) stream lines
// holding a record of the type/subtype selector ("init" or "result").
func recordLines(in *SessionInput, p int, kind string) []int {
	var out []int
	for i, l := range strings.Split(strings.TrimSuffix(string(in.Streams[p]), "\n"), "\n") {
		if kind == "init" && strings.Contains(l, `"subtype":"init"`) || kind == "result" && strings.Contains(l, `"type":"result"`) {
			out = append(out, i)
		}
	}
	return out
}

// editRecord rewrites the n-th (0-based) record of kind in phase p's
// (0-based) stream through f.
func editRecord(t *testing.T, in *SessionInput, p int, kind string, n int, f func(v map[string]any)) {
	t.Helper()
	at := recordLines(in, p, kind)
	if n >= len(at) {
		t.Fatalf("phase %d has %d %s records", p+1, len(at), kind)
	}
	streamLines(in, p, func(lines []string) []string {
		var v map[string]any
		if err := json.Unmarshal([]byte(lines[at[n]]), &v); err != nil {
			t.Fatal(err)
		}
		f(v)
		lines[at[n]] = rec(v)
		return lines
	})
}

// TestSessionPerTurnInits (UT-3, FP-6, amendment A3.9): headless claude -p
// emits a system/init record at the start of every turn, the initial one
// and each started by a background-completion wake, and a result at the
// end of each. Any number of turns derives the same map as one turn when
// every later init repeats the phase's first init (session, cwd, model,
// permission mode, tools, MCP servers); a differing init is
// session_mismatch, an unparseable one session_evidence_malformed, and
// work outside a turn (before the first init, between a result and the
// next init, after the final result) stays malformed.
func TestSessionPerTurnInits(t *testing.T) {
	t.Parallel()
	build := func(t *testing.T) SessionInput {
		r := newSynth(t)
		r.perTurn = [2]bool{true, true}
		return r.build()
	}
	t.Run("three turns per phase pass", func(t *testing.T) {
		t.Parallel()
		in := build(t)
		for p := 0; p < 2; p++ {
			if n := len(recordLines(&in, p, "init")); n != 3 {
				t.Fatalf("phase %d has %d inits, want 3", p+1, n)
			}
		}
		em := DeriveSession(in)
		if codes := codesOfMap(em); len(codes) != 0 {
			t.Fatalf("findings %v", codes)
		}
		one := DeriveSession(newSynth(t).build())
		var kinds, want []string
		for _, e := range em.Entries {
			kinds = append(kinds, e.Kind)
		}
		for _, e := range one.Entries {
			want = append(want, e.Kind)
		}
		if !slices.Equal(kinds, want) || len(em.Init) != 2 || em.Init[0].Phase != 1 || em.Init[1].Phase != 2 || len(entriesOf(em, MapWake, "")) != 4 {
			t.Fatalf("entries %v want %v, init %+v", kinds, want, em.Init)
		}
		for _, e := range entriesOf(em, MapWake, "") {
			if !e.Automatic {
				t.Fatalf("wake %+v not automatic", e)
			}
		}
		// The phase's result is its final one, after every consumption.
		for _, e := range em.Entries {
			if e.Kind == MapPause && e.Sources[0].FirstLine != recordLines(&in, 0, "result")[2]+1 {
				t.Fatalf("pause cites %+v", e.Sources)
			}
		}
		for p := 0; p < 2; p++ {
			if present, ok := streamResult(in.Streams[p]); !present || !ok {
				t.Fatalf("phase %d result = %t %t", p+1, present, ok)
			}
		}
	})
	for name, tc := range map[string]struct {
		phase int
		edit  func(v map[string]any)
		code  string
	}{
		"another session id":      {0, func(v map[string]any) { v["session_id"] = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" }, CodeSessionMismatch},
		"another tool list":       {0, func(v map[string]any) { v["tools"] = []string{"Bash", "Read", toolDispatch, toolTaskShow, "Write"} }, CodeSessionMismatch},
		"reordered tool list":     {1, func(v map[string]any) { v["tools"] = []string{"Read", "Bash", toolDispatch, toolTaskShow} }, CodeSessionMismatch},
		"another cwd":             {1, func(v map[string]any) { v["cwd"] = "/elsewhere" }, CodeSessionMismatch},
		"another model":           {0, func(v map[string]any) { v["model"] = "claude-sonnet" }, CodeSessionMismatch},
		"another permission mode": {1, func(v map[string]any) { v["permissionMode"] = "bypassPermissions" }, CodeSessionMismatch},
		"another MCP server list": {0, func(v map[string]any) {
			v["mcp_servers"] = []map[string]string{{"name": "callsheet", "status": "failed"}}
		}, CodeSessionMismatch},
		"tools not a list":         {0, func(v map[string]any) { v["tools"] = "Bash" }, CodeSessionMalformed},
		"MCP server name a number": {1, func(v map[string]any) { v["mcp_servers"] = []map[string]any{{"name": 1, "status": "connected"}} }, CodeSessionMalformed},
		"model null":               {0, func(v map[string]any) { v["model"] = nil }, CodeSessionMalformed},
	} {
		t.Run("second init: "+name, func(t *testing.T) {
			t.Parallel()
			in := build(t)
			editRecord(t, &in, tc.phase, "init", 1, tc.edit)
			codes := codesOfMap(DeriveSession(in))
			other := map[string]string{CodeSessionMismatch: CodeSessionMalformed, CodeSessionMalformed: CodeSessionMismatch}[tc.code]
			if !slices.Contains(codes, tc.code) || tc.code == CodeSessionMalformed && slices.Contains(codes, other) {
				t.Fatalf("findings %v, want %s", codes, tc.code)
			}
		})
	}
	t.Run("an unparseable first init", func(t *testing.T) {
		t.Parallel()
		in := build(t)
		editRecord(t, &in, 1, "init", 0, func(v map[string]any) { v["permissionMode"] = 3 })
		if codes := codesOfMap(DeriveSession(in)); !slices.Contains(codes, CodeSessionMalformed) {
			t.Fatalf("findings %v", codes)
		}
	})
	// Review r1 C1: a present null anywhere in a compared field, nested
	// list elements and MCP entry fields included, is unparseable, in the
	// first init and in a later one (never a mere mismatch, or nothing).
	servers := func(entries ...any) func(v map[string]any) {
		return func(v map[string]any) { v["mcp_servers"] = entries }
	}
	for name, edit := range map[string]func(v map[string]any){
		"a null tool":                 func(v map[string]any) { v["tools"] = []any{"Bash", nil, toolDispatch, toolTaskShow} },
		"a null MCP entry":            servers(nil),
		"an MCP entry not an object":  servers("callsheet:connected"),
		"a null MCP name":             servers(map[string]any{"name": nil, "status": "connected"}),
		"a null MCP status":           servers(map[string]any{"name": "callsheet", "status": nil}),
		"an MCP entry without status": servers(map[string]any{"name": "callsheet"}),
	} {
		for n, which := range []string{"first", "later"} {
			t.Run(which+" init: "+name, func(t *testing.T) {
				t.Parallel()
				in := build(t)
				editRecord(t, &in, n, "init", n, edit)
				codes := codesOfMap(DeriveSession(in))
				if !slices.Contains(codes, CodeSessionMalformed) || n == 1 && slices.Contains(codes, CodeSessionMismatch) {
					t.Fatalf("findings %v, want %s only", codes, CodeSessionMalformed)
				}
			})
		}
	}
	t.Run("an MCP status from empty to null", func(t *testing.T) {
		t.Parallel()
		in := build(t)
		for n := 0; n < 3; n++ {
			editRecord(t, &in, 0, "init", n, servers(map[string]any{"name": "callsheet", "status": ""}))
		}
		if codes := codesOfMap(DeriveSession(in)); len(codes) != 0 {
			t.Fatalf("empty statuses: findings %v", codes)
		}
		editRecord(t, &in, 0, "init", 2, servers(map[string]any{"name": "callsheet", "status": nil}))
		if codes := codesOfMap(DeriveSession(in)); !slices.Contains(codes, CodeSessionMalformed) {
			t.Fatalf("findings %v", codes)
		}
	})
	// Review r1 C3: MCP entries compare as name/status pairs, so no
	// delimiter can make two different lists equal.
	t.Run("an MCP delimiter collision", func(t *testing.T) {
		t.Parallel()
		in := build(t)
		editRecord(t, &in, 1, "init", 0, servers(map[string]any{"name": "callsheet:x", "status": "connected"}))
		editRecord(t, &in, 1, "init", 1, servers(map[string]any{"name": "callsheet", "status": "x:connected"}))
		editRecord(t, &in, 1, "init", 2, servers(map[string]any{"name": "callsheet:x", "status": "connected"}))
		codes := codesOfMap(DeriveSession(in))
		if !slices.Contains(codes, CodeSessionMismatch) || slices.Contains(codes, CodeSessionMalformed) {
			t.Fatalf("findings %v, want %s", codes, CodeSessionMismatch)
		}
	})
	// Review r1 C2: an init's session is its stream session_id; a
	// sessionId alias that disagrees with it is malformed and never
	// substitutes for it.
	for n, which := range []string{"first", "later"} {
		t.Run(which+" init: a conflicting session alias", func(t *testing.T) {
			t.Parallel()
			in := build(t)
			editRecord(t, &in, 0, "init", n, func(v map[string]any) {
				v["session_id"], v["sessionId"] = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", testSession
			})
			em := DeriveSession(in)
			if codes := codesOfMap(em); !slices.Contains(codes, CodeSessionMalformed) || !slices.Contains(codes, CodeSessionMismatch) {
				t.Fatalf("findings %v", codes)
			}
		})
	}
	t.Run("an agreeing session alias", func(t *testing.T) {
		t.Parallel()
		in := build(t)
		editRecord(t, &in, 0, "init", 1, func(v map[string]any) { v["sessionId"] = testSession })
		if codes := codesOfMap(DeriveSession(in)); len(codes) != 0 {
			t.Fatalf("findings %v", codes)
		}
	})
	assistant := `{"type":"assistant","session_id":"` + testSession + `","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}`
	for name, edit := range map[string]func(t *testing.T, in *SessionInput){
		"work before the first init": func(t *testing.T, in *SessionInput) {
			streamLines(in, 0, func(l []string) []string { return slices.Insert(l, 0, assistant) })
		},
		"work between a result and the next init": func(t *testing.T, in *SessionInput) {
			at := recordLines(in, 1, "result")[0]
			streamLines(in, 1, func(l []string) []string { return slices.Insert(l, at+1, assistant) })
		},
		"work after the final result": func(t *testing.T, in *SessionInput) {
			streamLines(in, 0, func(l []string) []string { return append(l, assistant) })
		},
		"two results in one turn": func(t *testing.T, in *SessionInput) {
			at := recordLines(in, 0, "result")[0]
			streamLines(in, 0, func(l []string) []string { return slices.Insert(l, at+1, l[at]) })
		},
		// Review r1 W1: an init while the previous turn is still open.
		"an intermediate result removed": func(t *testing.T, in *SessionInput) {
			at := recordLines(in, 0, "result")[0]
			streamLines(in, 0, func(l []string) []string { return slices.Delete(l, at, at+1) })
		},
		"a turn opened after the final result": func(t *testing.T, in *SessionInput) {
			first := recordLines(in, 1, "init")[0]
			streamLines(in, 1, func(l []string) []string { return append(l, l[first]) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := build(t)
			edit(t, &in)
			if codes := codesOfMap(DeriveSession(in)); !slices.Contains(codes, CodeSessionMalformed) {
				t.Fatalf("findings %v, want %s", codes, CodeSessionMalformed)
			}
		})
	}
	t.Run("a failed turn fails the phase", func(t *testing.T) {
		t.Parallel()
		// Every turn's result must succeed, not only the final one.
		in := build(t)
		editRecord(t, &in, 0, "result", 1, func(v map[string]any) { v["subtype"], v["is_error"] = "error_during_execution", true })
		em := DeriveSession(in)
		if present, ok := streamResult(in.Streams[0]); !present || ok || len(entriesOf(em, MapPause, "")) != 0 {
			t.Fatalf("result %t %t, pause %v", present, ok, entriesOf(em, MapPause, ""))
		}
		if present, ok := streamResult(in.Streams[1]); !present || !ok {
			t.Fatalf("phase 2 result %t %t", present, ok)
		}
	})
}
