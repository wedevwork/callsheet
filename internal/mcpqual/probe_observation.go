package mcpqual

// The shared probe terminal observation (design decoder-enrollment B1.5,
// FP-14). One bounded analyzer turns the raw probe events, the requested
// case, whether the bytes were cut and the trusted harness's execution
// attestation into the parsed evidence capture records, the correlated
// endpoints qualify measures and an observation both record. Capture,
// qualify, bundle validation, report evidence checking and enrollment
// replay all use it, so they cannot drift.
//
// ParseProbeEvents' intact (the stream ends in an exit event and LF) is
// unchanged. The one new state, terminal_observed_without_exit, asserts
// only that the requested call's single correlated terminal record was the
// last complete LF-terminated record of a clean session and that no exit
// record followed; it never asserts why the probe ended (a client teardown,
// a signal or anything else) or that the vendor received the result.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Probe observation end states.
const (
	EndIntact              = "intact"
	EndTerminalWithoutExit = "terminal_observed_without_exit"
	EndIncomplete          = "incomplete"
)

// TerminalObservedLabel is the human label of EndTerminalWithoutExit: the
// cause is unknown, so it never says intact, killed or terminated.
const TerminalObservedLabel = "terminal observed; probe exit not observed"

// ProbeObservation is the recorded observation of one analyzed probe log:
// exactly these four members (design decoder-enrollment B1.5, FP-14).
type ProbeObservation struct {
	// EndState is intact, terminal_observed_without_exit or incomplete.
	EndState string `json:"end_state"`
	// Intact is ParseProbeEvents' unchanged intact flag.
	Intact bool `json:"intact"`
	// TerminalKind is the unique correlated terminal's kind (completed,
	// cancelled or eof), or null when there is none: evidence, never a
	// vendor verdict.
	TerminalKind *string `json:"terminal_kind"`
	// CleanSession is the trusted harness's attestation of the session's
	// execution: it ran, exited exactly 0 by itself (no signal, watchdog,
	// interruption or held stream), its cleanup proved the group gone
	// without error and none of its evidence was cut or omitted.
	CleanSession bool `json:"clean_session"`
}

// observationMembers are ProbeObservation's members, in order.
var observationMembers = []string{"end_state", "intact", "terminal_kind", "clean_session"}

// Label is the observation's human label.
func (o *ProbeObservation) Label() string {
	switch {
	case o == nil:
		return "not analyzed"
	case o.EndState == EndTerminalWithoutExit:
		return TerminalObservedLabel
	case o.EndState == EndIntact:
		return "intact (probe exit observed)"
	}
	return "incomplete"
}

// observedEndpoint reports an end state that can stand for the probe's
// endpoint: intact, or the clean terminal observation without exit.
func (o *ProbeObservation) observedEndpoint() bool {
	return o.EndState == EndIntact || o.EndState == EndTerminalWithoutExit
}

// equal compares every recorded member.
func (o *ProbeObservation) equal(p *ProbeObservation) bool {
	switch {
	case o == nil || p == nil:
		return o == p
	case o.EndState != p.EndState || o.Intact != p.Intact || o.CleanSession != p.CleanSession:
		return false
	case o.TerminalKind == nil || p.TerminalKind == nil:
		return o.TerminalKind == p.TerminalKind
	}
	return *o.TerminalKind == *p.TerminalKind
}

// validate checks the observation's own consistency: its enums, intact
// with an intact end, and the new state only with intact=false, a terminal
// kind and a clean session.
func (o *ProbeObservation) validate() error {
	switch o.EndState {
	case EndIntact, EndTerminalWithoutExit, EndIncomplete:
	default:
		return fmt.Errorf("observation: end_state %q", o.EndState)
	}
	if k := o.TerminalKind; k != nil && *k != EvCompleted && *k != EvCancelled && *k != EvEOF {
		return fmt.Errorf("observation: terminal_kind %q", *k)
	}
	switch {
	case o.EndState == EndIntact && !o.Intact:
		return errors.New("observation: an intact end state without an intact log")
	case o.EndState == EndTerminalWithoutExit && (o.Intact || o.TerminalKind == nil || !o.CleanSession):
		return errors.New("observation: " + EndTerminalWithoutExit + " needs intact=false, a terminal kind and a clean session")
	}
	return nil
}

// checkObservationRaw checks a present observation object's raw members:
// an object (never null) with exactly the four members, each present and
// non-null except terminal_kind, which may be null.
func checkObservationRaw(raw json.RawMessage, where string) error {
	return checkObjectRaw(raw, where, observationMembers, map[string]bool{"terminal_kind": true})
}

// checkObjectRaw checks an optional object member's raw value: absent
// (nil) is fine; present it must be an object, never null, with exactly
// members, each non-null unless nullable.
func checkObjectRaw(raw json.RawMessage, where string, members []string, nullable map[string]bool) error {
	if raw == nil {
		return nil
	}
	var m map[string]json.RawMessage
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &m) != nil {
		return fmt.Errorf("%s: an explicit null or a non-object (only absence denotes legacy or unanalyzed evidence)", where)
	}
	if len(m) != len(members) {
		return fmt.Errorf("%s: members %d, want exactly %s", where, len(m), strings.Join(members, ", "))
	}
	for _, k := range members {
		v, ok := m[k]
		if !ok {
			return fmt.Errorf("%s: missing member %s", where, k)
		}
		if string(bytes.TrimSpace(v)) == "null" && !nullable[k] {
			return fmt.Errorf("%s: member %s is null", where, k)
		}
	}
	return nil
}

// probeAnalysis is the shared analyzer's result.
type probeAnalysis struct {
	// events and err are the parse of the bytes (none when cut or empty).
	events []ProbeEvent
	err    error
	// capture is capture's parsed summary (its anomalies included).
	capture *CaptureProbe
	// view is qualify's correlated receipt, endpoint and progress.
	view probeSummary
	obs  ProbeObservation
}

// analyzeProbe is the shared bounded analyzer of one probe events file:
// raw are the bounded server bytes, caseID the requested case, cut that
// the bytes were cut (they are then never parsed) and clean the harness's
// execution attestation (never inferred from vendor prose).
func analyzeProbe(raw []byte, caseID string, cut, clean bool) probeAnalysis {
	a := probeAnalysis{capture: &CaptureProbe{Anomalies: []string{}}, obs: ProbeObservation{EndState: EndIncomplete, CleanSession: clean}}
	p := a.capture
	if cut {
		p.Anomalies = append(p.Anomalies, "events_truncated")
		return a
	}
	if len(raw) == 0 {
		return a
	}
	evs, intact, err := ParseProbeEvents(raw)
	if err != nil {
		a.err = err
		p.Anomalies = append(p.Anomalies, "events_invalid")
		return a
	}
	a.events = evs
	p.Intact, a.obs.Intact = intact, intact
	summarizeProbe(p, evs, caseID)
	a.view = probeView(evs, caseID)
	term, recv := correlatedTerminal(evs, caseID)
	// Qualify's endpoint follows the same correlation rules (code review
	// B1.5 round 1, C1): only the unique correlated terminal ends the call.
	a.view.end, a.view.completed = term, false
	if term != nil {
		kind := term.Kind
		a.obs.TerminalKind = &kind
		a.view.completed = kind == EvCompleted
	}
	switch {
	case intact && len(p.Anomalies) == 0:
		a.obs.EndState = EndIntact
	case !intact && clean && exitlessTerminal(raw, evs, caseID, p, term, recv):
		a.obs.EndState = EndTerminalWithoutExit
	}
	return a
}

// sameRequestID compares two raw request IDs as the trimmed JSON text: a
// number and a string are never equal, and a missing ID never correlates.
func sameRequestID(a, b json.RawMessage) bool {
	a, b = bytes.TrimSpace(a), bytes.TrimSpace(b)
	return len(a) > 0 && bytes.Equal(a, b)
}

// correlatedTerminal is the requested case's unique correlated terminal
// (completed, cancelled or case-correlated eof) and its receipt: the only
// receipt of the case in the stream, and the only terminal of the case,
// in the receipt's probe instance with its request ID, after it. Any other
// shape has none.
func correlatedTerminal(evs []ProbeEvent, caseID string) (term, recv *ProbeEvent) {
	instance, recvInstance, receipts, terminals := 0, -1, 0, 0
	for i := range evs {
		ev := &evs[i]
		switch ev.Kind {
		case EvStart:
			instance++
		case EvReceipt:
			if ev.CaseID == caseID {
				receipts++
				recv, recvInstance = ev, instance
			}
		case EvCompleted, EvCancelled, EvEOF:
			if ev.CaseID != caseID {
				continue
			}
			terminals++
			if recv != nil && instance == recvInstance && sameRequestID(ev.RequestID, recv.RequestID) && ev.RunID == recv.RunID {
				term = ev
			}
		}
	}
	if receipts != 1 || terminals != 1 || term == nil {
		return nil, nil
	}
	return term, recv
}

// exitlessTerminal applies the allowance's framing and correlation rules
// to a parsed log without an exit (the caller has checked the clean
// session): the bytes end in exactly one LF right after the terminal JSON
// record, with no blank record anywhere and no whitespace around it; the
// stream's only receipt is the requested case's, after its instance's
// initialization; its unique correlated terminal is the last event; there
// is no anomaly; and the last instance holds nothing but its start,
// initialization, receipt, the receipt's own progress and scheduled
// records and that terminal (no other terminal, late cancellation,
// rejection, error, write failure, idle EOF or exit). Earlier instances
// must have closed with their exit (an unclosed one is an anomaly) and,
// with the stream's single receipt in the last one, held no receipt.
func exitlessTerminal(raw []byte, evs []ProbeEvent, caseID string, p *CaptureProbe, term, recv *ProbeEvent) bool {
	if term == nil || len(p.Anomalies) > 0 || !p.Initialized || p.Receipts != 1 || &evs[len(evs)-1] != term {
		return false
	}
	if !bytes.HasSuffix(raw, []byte{'\n'}) || bytes.Contains(raw, []byte("\n\n")) || raw[0] == '\n' {
		return false
	}
	body := raw[:len(raw)-1]
	last := body[bytes.LastIndexByte(body, '\n')+1:]
	if len(last) == 0 || !bytes.Equal(last, bytes.TrimSpace(last)) || last[len(last)-1] != '}' {
		return false
	}
	start := 0
	receipts := 0
	for i, ev := range evs {
		if ev.Kind == EvReceipt {
			receipts++
		}
		if ev.Kind == EvStart {
			start = i
		}
	}
	if receipts != 1 {
		return false
	}
	afterReceipt := false
	for i := start; i < len(evs); i++ {
		ev := &evs[i]
		switch ev.Kind {
		case EvStart:
			if i != start {
				return false
			}
		case EvInitialize:
		case EvReceipt:
			if ev != recv {
				return false
			}
			afterReceipt = true
		case EvProgress, EvScheduled:
			if !afterReceipt || ev.CaseID != caseID || !sameRequestID(ev.RequestID, recv.RequestID) {
				return false
			}
		default:
			if ev != term {
				return false
			}
		}
	}
	return true
}

// summarizeProbe fills capture's parsed summary of evs for caseID: the
// first initialize clientInfo, the case's receipts and completion, and
// anomalies (a marker or other receipt, a second receipt, a rejection, a
// fatal or write error, completion and lifecycle disorder).
func summarizeProbe(p *CaptureProbe, evs []ProbeEvent, caseID string) {
	add := func(a string) {
		if !strings.Contains(strings.Join(p.Anomalies, ","), a) {
			p.Anomalies = append(p.Anomalies, a)
		}
	}
	// The receipt's identity: its probe instance (a start event begins a
	// new one) and request ID. A completion counts only for exactly that
	// receipt, once, after it (code review C7).
	//
	// Initialization and lifecycle are per instance too (code review round
	// 2, C3): the receipt-bearing instance must itself have initialized
	// before the receipt, and an instance ends at its exit event. The
	// recorded clientInfo stays the first one in the stream.
	//
	// Every instance closes (code review round 3, C2): an instance must
	// reach its exit before the next starts (instance_unclosed); the last
	// one either closes too (an intact log) or ends at the clean terminal
	// observation's own last record (design decoder-enrollment B1.5,
	// FP-14), so a later instance's exit never stands in for it.
	//
	// Every record belongs to its instance's run (code review B1.5 round 1,
	// C2): a record whose run ID is not its start record's is
	// instance_run_mismatch, so no record of another run can initialize,
	// receive or end the requested call.
	instance, recvInstance, recvID, done := 0, -1, "", false
	initialized, exited := false, false
	run := ""
	for _, ev := range evs {
		if exited && ev.Kind != EvStart {
			add("event_after_exit")
		}
		if ev.Kind != EvStart && ev.RunID != run {
			add("instance_run_mismatch")
		}
		switch ev.Kind {
		case EvStart:
			if instance > 0 && !exited {
				add("instance_unclosed")
			}
			instance++
			initialized, exited, run = false, false, ev.RunID
		case EvExit:
			exited = true
		case EvInitialize:
			initialized = true
			if !p.Initialized {
				p.Initialized = true
				p.ClientName, p.ClientVersion = ev.ClientName, ev.ClientVersion
			}
		case EvReceipt:
			switch ev.CaseID {
			case caseID:
				p.Receipts++
				if p.Receipts > 1 {
					add("extra_receipt")
				}
				if !initialized {
					add("receipt_without_initialize")
				}
				recvInstance, recvID = instance, string(bytes.TrimSpace(ev.RequestID))
			case caseID + "-marker":
				add("marker_receipt")
			default:
				add("unexpected_receipt")
			}
		case EvCompleted:
			switch {
			case ev.CaseID != caseID:
				add("unexpected_completion")
			case recvInstance < 0:
				add("completion_without_receipt")
			case recvInstance != instance || recvID != string(bytes.TrimSpace(ev.RequestID)):
				add("completion_mismatch")
			case done:
				add("duplicate_completion")
			default:
				done, p.Completed = true, true
			}
		case EvRejected:
			add("rejected")
		case EvError:
			add("fatal_error")
		case EvWriteFail:
			add("write_failed")
		}
	}
}

// sessionStreamFiles are the files a launched capture session writes; its
// clean_session attestation covers exactly their collection.
var sessionStreamFiles = []string{FileVendorEvents, FileVendorStderr, FileServerEvents}

// captureSessionClean is the clean_session attestation of a capture
// session from its recorded facts: a launched stage that exited 0 by
// itself with proven cleanup, and the session's three evidence streams
// neither cut (input or output) nor with omitted lines. The runner and
// every validator compute it from the same record.
func captureSessionClean(id string, session CaptureStage, streams []CaptureStream) bool {
	if !provenClean(session) {
		return false
	}
	for _, s := range streams {
		for _, f := range sessionStreamFiles {
			if s.Path == ClientFile(id, f) && !s.Clean() {
				return false
			}
		}
	}
	return true
}

// cleanRun is qualify's clean_session attestation of one launched case
// from the runner's own facts: exit exactly 0, no signal, watchdog,
// interruption or held stream, cleanup proven without error, and no cut
// probe events, evidence, transcript or stderr and no omitted transcript
// line.
func (run *caseRun) cleanRun() bool {
	return run.launchErr == nil && run.exit != nil && *run.exit == 0 && run.signal == nil && !run.watchdog && !run.interrupted && !run.stdoutHeld && !run.stderrHeld &&
		run.cleanup.Error == nil && run.cleanup.GroupGone && !run.probeCut && !run.serverCut && !run.evidenceCut && !run.transcript.Truncated && !run.stderrCut &&
		run.omitted == 0 && run.writeErr == nil
}

// redactJSONLFramed redacts each JSON line of b with red and keeps the
// framing exactly: blank records and a missing final LF stay as they were,
// so a replay of the retained bytes sees the same records and the same
// terminating LF (or its absence) the analyzer saw.
func redactJSONLFramed(red *Redactor, b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	var buf bytes.Buffer
	lines := bytes.Split(b, []byte{'\n'})
	for i, line := range lines {
		if len(line) > 0 {
			buf.Write(red.Line(line))
		}
		if i < len(lines)-1 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}
