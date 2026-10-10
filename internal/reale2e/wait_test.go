package reale2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBackgroundWaitEvidence (FP-6): background waits whose completion was
// consumed (with at least one automatic wake) pass; a long blocking MCP
// call, polling only, a foreground wait or an absent wake fail; the
// supervisor records each wait from the files its own redirected command
// wrote, so a killed or failed wait fails too.
func TestBackgroundWaitEvidence(t *testing.T) {
	t.Parallel()
	if code, r, _ := checkBundle(t, passingBundle(t)); code != 0 || criterion(r, CritWaits).Result != "pass" {
		t.Fatalf("background waits = %d %v", code, failedCodes(r))
	}
	for name, m := range map[string]struct {
		code string
		f    func(t *testing.T, b string)
	}{
		"long blocking call": {"unqualified_long_poll", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				m.Events = append(m.Events, MapEvent{Kind: MapMCPWait, BudgetMS: 300000, Recovery: true, Lines: []int{2, 2}})
			})
		}},
		"polling only": {"automatic_wake_unobserved", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].AutomaticWake = false
				}
				m.Events = append(m.Events, MapEvent{Kind: MapMCPWait, BudgetMS: 10000, Recovery: true, Lines: []int{2, 2}})
			})
		}},
		"foreground wait": {"wait_not_background", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].Background = false
				}
			})
		}},
		"absent wake": {"automatic_wake_unobserved", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].UserMessageBetween = true
				}
			})
		}},
	} {
		b := passingBundle(t)
		m.f(t, b)
		code, r, _ := checkBundle(t, b)
		requireCode(t, name, code, r, CritWaits, m.code)
	}
	// The live collector's wait records.
	for name, tc := range map[string]struct {
		exit string
		prep func(dir string)
		code string
	}{
		"failed wait":     {"7", nil, "wait_unsuccessful"},
		"killed wait":     {"", func(dir string) { os.Remove(filepath.Join(dir, "exit-code")) }, "wait_unsuccessful"},
		"unreadable wait": {"0", func(dir string) { os.WriteFile(filepath.Join(dir, "result.json"), []byte("{"), 0o600) }, "wait_binding_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.coord.waitExit = tc.exit
			if tc.prep != nil {
				f.coord.mutateOwner = func(owner string) { tc.prep(filepath.Join(filepath.Dir(owner), "waits", "02")) }
			}
			if code := f.attempt(); code != 1 {
				t.Fatalf("attempt = %d", code)
			}
			r := f.report()
			if c := criterion(r, CritWaits); c.Result != "fail" || !containsCode(c.Codes, tc.code) {
				t.Fatalf("waits %+v", c)
			}
		})
	}
}

func containsCode(codes []string, c string) bool {
	for _, x := range codes {
		if x == c {
			return true
		}
	}
	return false
}
