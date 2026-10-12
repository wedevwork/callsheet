package reale2e

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestBackgroundWaitEvidence (FP-6, amendment A3): each hop's direct CLI
// wait runs as one Bash background job from the rendered template; the
// vendor's completion notification wakes the coordinator, which consumes
// it with task_show; the supervisor captures the raw redirected wait
// files and reparses them. A long blocking MCP call, polling only (no
// wake) and a changed wait command fail, and a failed or killed wait or an
// unreadable result fails even though the worker itself succeeded.
func TestBackgroundWaitEvidence(t *testing.T) {
	t.Parallel()
	t.Run("background completion and consumption pass", func(t *testing.T) {
		t.Parallel()
		code, r, _ := checkBundle(t, passingBundle(t))
		if code != 0 || criterion(r, CritWaits).Result != "pass" {
			t.Fatalf("background waits = %d %v", code, failedCodes(r))
		}
		b := passingBundle(t)
		bl, err := LoadBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		em := DeriveSession(sessionInputOf(bl))
		for i, h := range Hops {
			starts, wakes, consumed := entriesOf(em, MapWaitStart, h), entriesOf(em, MapWake, h), entriesOf(em, MapWaitConsumed, h)
			if len(starts) != 1 || len(wakes) != 1 || !wakes[0].Automatic || len(consumed) != 1 || starts[0].Phase != hopPhase(i) {
				t.Fatalf("hop %s: starts %v wakes %v consumed %v", h, starts, wakes, consumed)
			}
			for _, name := range []string{waitResultFile, waitExitFile, waitStderrFile} {
				if _, err := os.Stat(filepath.Join(b, "hops", hopDir(i), name)); err != nil {
					t.Fatalf("raw wait file %s: %v", name, err)
				}
			}
		}
	})
	for name, tc := range map[string]struct {
		set  func(c *coordScript)
		fail string
		crit string
		code string
	}{
		"long blocking call": {func(c *coordScript) { c.longPoll = true }, CodeLongPoll, CritWaits, CodeLongPoll},
		"polling only":       {func(c *coordScript) { c.noWake = [4]bool{true, true, true, true} }, "", CritWaits, "automatic_wake_unobserved"},
		"user message before consumption": {func(c *coordScript) {
			c.noWake = [4]bool{false, true, true, true}
			c.userBetween = 0
		}, CodeUserInput, CritWaits, "automatic_wake_unobserved"},
		"changed wait command": {func(c *coordScript) {
			c.waitCommand = func(i int, cmd string) string { return cmd + "; true" }
		}, CodeWaitMismatch, CritEventMap, CodeWaitMismatch},
		"unknown wake envelope": {func(c *coordScript) { c.badWake = true }, CodeWakeUnsupported, CritEventMap, CodeWakeUnsupported},
		"failed wait":           {func(c *coordScript) { c.waitExit = "7" }, CodePhaseEarly, CritWaits, "wait_unsuccessful"},
		"killed wait":           {func(c *coordScript) { c.waitExit = "" }, CodePhaseEarly, CritWaits, "wait_files_missing"},
		"unreadable wait": {func(c *coordScript) {
			c.afterPhase = func(c *coordScript, p int) {
				if p == 1 {
					os.WriteFile(filepath.Join(waitDir(c.s.runtime, 1), "result.json"), []byte("{"), 0o600)
				}
			}
		}, CodePhaseEarly, CritWaits, "wait_binding_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			tc.set(f.coord)
			code := f.attempt()
			if code != 1 || (tc.fail != "" && f.sup.failCode != tc.fail) {
				t.Fatalf("attempt = %d %q, want %q\n%s", code, f.sup.failCode, tc.fail, f.stderr.String())
			}
			r := f.report()
			if c := criterion(r, tc.crit); c.Result != "fail" || !slices.Contains(c.Codes, tc.code) {
				t.Fatalf("%s = %+v (all %v)", tc.crit, c, failedCodes(r))
			}
		})
	}
}
