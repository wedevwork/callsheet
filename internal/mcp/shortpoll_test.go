package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// schemaDigest is the SHA-256 of every tool's name and inputSchema, in
// tools/list order, as main a09b711 lists them (design
// nonblocking-coordinator-waits: the 23 schemas are unchanged; only the
// two waiting descriptions change).
const schemaDigest = "7c866a1d826fc656985b7e49d61531da9b3898155523579c13503facb6861650"

// TestShortPollGuidance is UT-6 (FP-6): for every budget the two waiting
// tools carry B, the unchanged deferral note and then the exact short-poll
// notice once, dispatch's wait is the short-task fast path and task_wait
// a compact short poll that must not be a multi-hour coordinator wait;
// no other tool mentions it; the 23 tools, their order and their input
// schemas are byte-identical (no until_done argument); budgets and
// reserves are unchanged.
func TestShortPollGuidance(t *testing.T) {
	for _, b := range []time.Duration{DefaultBudget, MinBudget, MaxBudget} {
		h := start(t, withBudget(b))
		h.ready()
		h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var r struct {
			Result struct {
				Tools []struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					InputSchema json.RawMessage `json:"inputSchema"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(h.nextRaw(), &r); err != nil {
			t.Fatal(err)
		}
		sum := sha256.New()
		var names []string
		for _, tl := range r.Result.Tools {
			names = append(names, tl.Name)
			sum.Write([]byte(tl.Name + "\x00"))
			sum.Write(tl.InputSchema)
			sum.Write([]byte{0})
			if strings.Contains(string(tl.InputSchema), "until_done") || strings.Contains(string(tl.InputSchema), "until-done") {
				t.Fatalf("%s gained an until_done argument", tl.Name)
			}
			waiting := tl.Name == toolTaskWait || tl.Name == toolDispatch
			if got := strings.Count(tl.Description, ShortPollNotice); got != map[bool]int{true: 1, false: 0}[waiting] {
				t.Fatalf("%s carries the notice %d times", tl.Name, got)
			}
			if waiting && !strings.HasSuffix(tl.Description, " This server's call budget is B="+NewBudget(b).String()+". "+BudgetDeferralNote+" "+ShortPollNotice) {
				t.Fatalf("%s description order %q", tl.Name, tl.Description)
			}
		}
		if len(names) != 23 || strings.Join(names, ",") != strings.Join(ToolNames, ",") {
			t.Fatalf("tools %v", names)
		}
		if got := hex.EncodeToString(sum.Sum(nil)); got != schemaDigest {
			t.Fatalf("input schemas changed: digest %s", got)
		}
		desc := map[string]string{}
		for _, tl := range r.Result.Tools {
			desc[tl.Name] = tl.Description
		}
		for _, s := range []string{"dispatch stays asynchronous", "The optional wait (0 to 5m) is the short-task fast path", "follow a longer task with the background CLI wait"} {
			if !strings.Contains(desc[toolDispatch], s) {
				t.Fatalf("dispatch lacks %q", s)
			}
		}
		for _, s := range []string{"A compact short poll:", "never use it as a multi-hour coordinator wait", "after still_running poll again on a later turn"} {
			if !strings.Contains(desc[toolTaskWait], s) {
				t.Fatalf("task_wait lacks %q", s)
			}
		}
	}
	// The budget and its reserves are unchanged: 10s default, 1s..5m,
	// reserves min(1s, B/4).
	if DefaultBudget != 10*time.Second || MinBudget != time.Second || MaxBudget != 5*time.Minute || NewBudget(DefaultBudget) != (Budget{B: 10 * time.Second, R: time.Second, N: time.Second}) ||
		NewBudget(time.Second) != (Budget{B: time.Second, R: 250 * time.Millisecond, N: 250 * time.Millisecond}) || AdmissionAllowance != 6*time.Second {
		t.Fatal("the short-poll budget changed")
	}
	if ShortPollNotice != "Default 10s call budget is a short poll, not a verified coordinator timeout. For long tasks, run callsheet task wait --until-done in a harness-managed background command; "+
		"without wake support, poll task_wait on the next turn. Raising the budget requires local timeout qualification." {
		t.Fatal("the short-poll notice is not the design's sentence")
	}
}
