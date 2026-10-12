package reale2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Authenticated preflight (design 12a-real-e2e, FP-10). After the gate:
// only the explicit absolute vendor paths are probed, with the existing
// adapter version policy deciding eligibility (its parser and ordering;
// no warning for newer versions), the flow's selections pass the existing
// validators, Go and Git are invocable and the private temporary
// directory is usable. After deployment, one no-workspace task per
// configured role/pair must answer exactly CALLSHEET_AUTH_OK; a 2m
// supervisor watchdog cancels a preflight that runs longer. A failure
// stops before feature work; there is no login automation, credential
// inspection or weaker retry.

// preflightGoal and preflightAcceptance are the authentication task.
const (
	preflightGoal       = "Callsheet authentication preflight. Do not use any tools and do not read or write files. Reply with exactly " + AuthToken + " and nothing else."
	preflightAcceptance = "The final answer is exactly " + AuthToken + "."
)

// requester is the supervisor's dispatch attribution.
var requester = contract.RequestedBy{Name: "callsheet-real-e2e", Version: "1", Hostname: "localhost"}

// probeOutput runs exe with args and returns its trimmed first output
// line.
func (s *supervisor) probeOutput(exe string, args ...string) (string, error) {
	res, err := s.command(ProcSpec{Path: exe, Args: args, Env: s.w.Environ, Dir: s.w.TempRoot}, 30*time.Second, 4096)
	if err != nil {
		return "", err
	}
	if res.TimedOut || res.ExitCode != 0 {
		return "", fmt.Errorf("%s exited %d", filepath.Base(exe), res.ExitCode)
	}
	return strings.TrimSpace(strings.SplitN(string(res.Stdout), "\n", 2)[0]), nil
}

// staticPreflight checks executables, versions, the flow, the templates
// and the checkout before anything is deployed.
func (s *supervisor) staticPreflight() error {
	if err := s.loadFlow(); err != nil {
		return failure("flow_invalid", err)
	}
	tpl, err := loadTemplates(filepath.Join(s.checkout, filepath.FromSlash(ExamplesDir)))
	if err != nil {
		return failure("templates_missing", err)
	}
	s.templates, s.rendered = tpl, map[string][]byte{}
	for _, h := range Hops {
		for _, k := range []string{"instruction", "runbook"} {
			n := manualName(h, k)
			if s.rendered[n], err = renderTemplate(n, tpl[n], map[string]string{"RUN_ID": s.runID}); err != nil {
				return failure("templates_invalid", err)
			}
		}
	}
	if s.manifest.CallsheetVersion, err = s.probeOutput(s.args.callsheet, "version"); err != nil {
		return failure("callsheet_unusable", err)
	}
	if r, err := git.PlainOpen(s.checkout); err == nil {
		if h, err := r.Head(); err == nil {
			s.manifest.Revision = h.Hash().String()
		}
	}
	var problems []error
	for _, v := range []string{"claude", "codex", "grok"} {
		exe := s.args.vendor(v)
		rec := VersionRecord{Tool: v, Path: s.tok(exe)}
		rec.Version, err = s.probeOutput(exe, "--version")
		if err == nil {
			err = s.w.Probe(s.ctx, v, exe, s.w.TempRoot)
		}
		rec.Eligible = err == nil
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %v", v, err))
		}
		s.manifest.Versions = append(s.manifest.Versions, rec)
	}
	for _, tool := range []struct{ name, arg string }{{"go", "version"}, {"git", "--version"}} {
		rec := VersionRecord{Tool: tool.name}
		exe, lerr := s.w.LookPath(tool.name)
		if lerr == nil {
			rec.Path = s.tok(exe)
			rec.Version, lerr = s.probeOutput(exe, tool.arg)
		}
		rec.Eligible = lerr == nil
		if lerr != nil {
			problems = append(problems, fmt.Errorf("%s: %v", tool.name, lerr))
		} else if tool.name == "go" {
			s.goBin = exe
		}
		s.manifest.Versions = append(s.manifest.Versions, rec)
	}
	if d, err := os.MkdirTemp(s.w.TempRoot, "ce-probe-"); err != nil {
		problems = append(problems, fmt.Errorf("private temporary directory: %v", err))
	} else {
		os.Remove(d)
	}
	s.ev.event(Event{Type: EvVersionsChecked})
	if err := errors.Join(problems...); err != nil {
		s.say("preflight failed; resolve vendor installation, login, model or posture outside the harness, then start a new attempt")
		return failure("version_preflight_failed", err)
	}
	s.pathEnv = envValue(s.w.Environ, "PATH")
	return nil
}

// envValue returns key's value in environ.
func envValue(environ []string, key string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// loadFlow reads the default flow or the explicit --flow copy.
func (s *supervisor) loadFlow() error {
	if s.args.flow == "" {
		s.flow = DefaultFlow()
		b, err := encodeJSON(s.flow)
		if err != nil {
			return err
		}
		s.flowBytes = b
	} else {
		b, err := readBounded(s.args.flow, maxFlowBytes)
		if err != nil {
			return err
		}
		if s.flow, err = ParseFlow(b); err != nil {
			return err
		}
		s.flowBytes = b
	}
	s.manifest.FlowKind, s.manifest.FlowSHA256, s.manifest.Roles = s.flow.Kind(), sha256Hex(s.flowBytes), s.flow.Roles
	return nil
}

// authPreflight runs the four authenticated preflight tasks in order.
func (s *supervisor) authPreflight() error {
	for i, h := range Hops {
		if err := s.preflightOne(i, h); err != nil {
			s.say("authentication preflight %s failed; resolve vendor login, model or posture outside the harness, then start a new attempt", h)
			return err
		}
	}
	return nil
}

// dispatchOwn dispatches one supervisor task (a preflight). A lost answer
// is resolved by inspecting the known tasks of that role by run marker:
// exactly one match is adopted, anything else aborts (never redispatch).
func (s *supervisor) dispatchOwn(req contract.DispatchRequest, deadline time.Time) (string, error) {
	ctx, cancel := s.reqCtx(deadline)
	defer cancel()
	v, err := s.plane.Dispatch(ctx, req)
	if err == nil {
		return v.TaskID, nil
	}
	if code := contract.CodeOf(err); code == contract.CodeInvalidArgument || code == contract.CodeNotFound || code == contract.CodeConflict {
		return "", failure("dispatch_refused", err)
	}
	var matches []string
	after := ""
	for {
		// The inspection is bounded per request, not by the dispatch's
		// deadline: an adopted task past it must still be found to be
		// cancelled.
		lctx, lcancel := s.opCtx()
		page, next, lerr := s.plane.ListTasks(lctx, after, contract.MaxTaskListLimit)
		lcancel()
		if lerr != nil {
			return "", failure("dispatch_lost_ambiguous", errors.Join(err, lerr))
		}
		for _, t := range page {
			if t.Role.ID != req.Target.Value || s.known[t.TaskID] != "" {
				continue
			}
			sctx, scancel := s.opCtx()
			v, serr := s.plane.ShowTask(sctx, t.TaskID, 0)
			scancel()
			if serr != nil {
				return "", failure("dispatch_lost_ambiguous", errors.Join(err, serr))
			}
			if len(v.Request.Payload) > 0 && v.Request.Payload[0] == req.Payload[0] {
				matches = append(matches, t.TaskID)
			}
		}
		if next == nil {
			break
		}
		after = *next
	}
	if len(matches) != 1 {
		return "", failure("dispatch_lost_ambiguous", err)
	}
	return matches[0], nil
}

// preflightOne runs preflight i against hop's role.
func (s *supervisor) preflightOne(i int, hop string) error {
	label := preflightHop(i)
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: hop}, Goal: preflightGoal,
		Payload: []string{markerFor(s.runID, label)}, Acceptance: preflightAcceptance, RequestedBy: requester}
	// The bound runs from the request's sending until the task's
	// authoritative creation time is known, never from the answer's arrival.
	sent := s.w.Clock.Now()
	id, err := s.dispatchOwn(req, sent.Add(PreflightBound))
	if err != nil {
		return err
	}
	s.known[id] = label
	s.preflight = append(s.preflight, id)
	s.ev.event(Event{Type: EvTaskAdmitted, Hop: label, Task: id})
	v, err := s.awaitTerminal(id, PreflightBound, sent)
	if v != nil {
		s.storeTask("preflight/"+hopDir(i), *v)
	}
	s.ev.event(Event{Type: EvPreflightFinished, Hop: label, Task: id})
	if err != nil {
		return err
	}
	r := v.Result
	switch {
	case v.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0:
		return failure("preflight_failed", fmt.Errorf("%s ended %s", label, v.State))
	case r.FinalMessage == nil || r.FinalMessageTruncated || !authAnswerOK(*r.FinalMessage):
		return failure("preflight_not_authenticated", fmt.Errorf("%s did not answer %s", label, AuthToken))
	}
	return nil
}

// awaitTerminal polls task id until it is terminal, within bound of the
// task's authoritative creation time (of sent, the request's sending,
// until that time is known or when it is unusable). Every poll is bounded
// by the time remaining. At the bound the supervisor cancels the task
// through the plane and awaits its terminal cleanup (bounded), returning
// the last view and a deadline failure; a result past the bound is not
// accepted either.
func (s *supervisor) awaitTerminal(id string, bound time.Duration, sent time.Time) (*contract.TaskView, error) {
	deadline, known := sent.Add(bound), false
	for {
		ctx, cancel := s.reqCtx(deadline)
		v, err := s.plane.ShowTask(ctx, id, contract.DefaultTailLines)
		cancel()
		if err == nil && !known {
			if c, ok := contract.ParseTime(v.CreatedAt); ok && !c.After(s.w.Clock.Now()) {
				deadline, known = c.Add(bound), true
			}
		}
		if err == nil && contract.TaskTerminal(v.State) {
			if d, ok := duration(v); !ok || d > bound || s.w.Clock.Now().After(deadline) {
				return &v, failure("deadline_exceeded", fmt.Errorf("task %s ended after its %s bound", id, bound))
			}
			return &v, nil
		}
		if !s.w.Clock.Now().Before(deadline) {
			break
		}
		if s.ctx.Err() != nil {
			return nil, failure("interrupted", s.ctx.Err())
		}
		s.sleep(min(s.w.Poll, deadline.Sub(s.w.Clock.Now())))
	}
	s.cancelTask(id)
	v, settled := s.settle(id, 30*time.Second)
	if !settled {
		s.unsettled = append(s.unsettled, id)
	}
	return v, failure("deadline_exceeded", fmt.Errorf("task %s passed its %s bound and was cancelled", id, bound))
}

// planeCallBound bounds one plane request with no tighter deadline.
const planeCallBound = 10 * time.Second

// reqCtx bounds one plane request by the time left until deadline (on the
// supervisor's clock) and by planeCallBound. An expired deadline gives an
// expired context: the request is not made late.
func (s *supervisor) reqCtx(deadline time.Time) (context.Context, context.CancelFunc) {
	left := deadline.Sub(s.w.Clock.Now())
	return context.WithTimeout(s.ctx, max(0, min(left, planeCallBound)))
}

// opCtx bounds one plane request by planeCallBound.
func (s *supervisor) opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.ctx, planeCallBound)
}

// show is one bounded task read.
func (s *supervisor) show(id string, lines int) (contract.TaskView, error) {
	ctx, cancel := s.opCtx()
	defer cancel()
	return s.plane.ShowTask(ctx, id, lines)
}

// attemptCtx bounds a longer request (a result pull) by the attempt's
// remaining time.
func (s *supervisor) attemptCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.ctx, max(0, s.attempt.Add(AttemptBound).Sub(s.w.Clock.Now())))
}

// cancelTask cancels id through the plane and records it.
func (s *supervisor) cancelTask(id string) {
	ctx, cancel := s.opCtx()
	defer cancel()
	s.plane.CancelTask(ctx, id)
	s.cancelled = append(s.cancelled, id)
	s.ev.event(Event{Type: EvTaskCancelled, Hop: s.known[id], Task: id})
}

// settle polls id until terminal within bound.
func (s *supervisor) settle(id string, bound time.Duration) (*contract.TaskView, bool) {
	deadline := s.w.Clock.Now().Add(bound)
	var last *contract.TaskView
	for {
		ctx, cancel := s.reqCtx(deadline)
		v, err := s.plane.ShowTask(ctx, id, contract.DefaultTailLines)
		cancel()
		if err == nil {
			last = &v
			if contract.TaskTerminal(v.State) {
				return last, true
			}
		}
		if !s.w.Clock.Now().Before(deadline) {
			return last, false
		}
		s.sleep(time.Second)
	}
}

// storeTask writes dir/task.json (the plane's task show structure) and
// dir/final.txt when the task has a final message.
func (s *supervisor) storeTask(dir string, v contract.TaskView) {
	s.ev.writeJSON(dir+"/task.json", contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
	if v.Result != nil && v.Result.FinalMessage != nil {
		s.ev.writeFile(dir+"/final.txt", []byte(*v.Result.FinalMessage))
	}
}
