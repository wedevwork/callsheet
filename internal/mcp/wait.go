package mcp

import (
	"strconv"
	"time"
)

// The waiting-call budget (iteration 07a, FP-7). B is an outer wall-clock
// budget for task_wait and dispatch with a wait, from handler admission
// through response delivery; trust setup counts against it. It is
// separate from worker execution timeouts and from the CLI/API wait
// defaults. The shipping default of 10 s is a deliberately short poll
// (design nonblocking-coordinator-waits): long waits use the CLI's
// callsheet task wait --until-done in a harness-managed background
// command, and no coordinator client's tool-call timeout is claimed
// beyond named local evidence (docs/support-catalog.md).
const (
	// DefaultBudget is the shipping short-poll budget B.
	DefaultBudget = 10 * time.Second
	// MinBudget and MaxBudget bound --wait-call-budget inclusively.
	MinBudget = time.Second
	MaxBudget = 5 * time.Minute
	// AdmissionAllowance is the fixed share a dispatch with a wait keeps
	// for admission: it reduces the requested wait, never the ability to
	// submit the dispatch inside the remaining client context.
	AdmissionAllowance = 6 * time.Second
	// maxReserve caps the response and transport reserves.
	maxReserve = time.Second
)

// BudgetDeferralNote is what the budget means for delivery, stated in the
// task_wait and dispatch-with-wait descriptions (bounded deferral, design
// r0.4): the budget is not a promise that the call ends within B.
const BudgetDeferralNote = "The budget includes trust setup and response delivery. An incomplete reply ends the session at the budget deadline unless its final write is in progress; " +
	"then a reply not fully written before the deadline ends the session once it is written, or the 1 s writer progress watchdog ends it first, " +
	"and a short final write returning at or after the deadline also ends the session."

// ShortPollNotice is the sentence every waiting tool's description, the
// mcp help text and the coordinator guide carry verbatim.
const ShortPollNotice = "Default 10s call budget is a short poll, not a verified coordinator timeout. For long tasks, run callsheet task wait --until-done in a harness-managed background command; without wake support, poll task_wait on the next turn. Raising the budget requires local timeout qualification."

// Budget is the configured outer budget B with its response reserve R
// and transport reserve N, each min(1s, B/4).
type Budget struct {
	B, R, N time.Duration
}

// NewBudget derives the reserves of b (callers validate the range).
func NewBudget(b time.Duration) Budget {
	r := min(maxReserve, b/4)
	return Budget{B: b, R: r, N: r}
}

// ValidBudget reports whether b is within MinBudget..MaxBudget.
func ValidBudget(b time.Duration) bool { return b >= MinBudget && b <= MaxBudget }

// Deadlines are a call's absolute delivery deadline D = start+B and its
// client-context deadline D-R.
func (b Budget) Deadlines(start time.Time) (delivery, client time.Time) {
	delivery = start.Add(b.B)
	return delivery, delivery.Add(-b.R)
}

// Available is the plane wait a call may still request at now: D minus
// now minus both reserves (and the admission allowance for a dispatch),
// never negative, truncated to milliseconds. Progress, retries and trust
// setup never extend D.
func (b Budget) Available(delivery, now time.Time, dispatch bool) time.Duration {
	a := delivery.Sub(now) - b.R - b.N
	if dispatch {
		a -= AdmissionAllowance
	}
	return max(0, a).Truncate(time.Millisecond)
}

// EffectiveWait is the plane wait to request: the explicit wait capped by
// the available wait, or the available wait when none was given.
func EffectiveWait(explicit *time.Duration, available time.Duration) time.Duration {
	if explicit == nil {
		return available
	}
	return min(*explicit, available)
}

// String renders the budget for descriptions and diagnostics.
func (b Budget) String() string { return formatDuration(b.B) }

// formatDuration renders whole minutes as "5m", whole seconds as "10s"
// and anything else in time.Duration's own form.
func formatDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}
