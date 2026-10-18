package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Prompt budgets (design 12a-dev-flow-harness, Selection manifest). These
// are Callsheet limits. Oversized material is referenced, never truncated.

const (
	triageManualCap    = 20480
	triageTaskCeiling  = 8192
	devPromptReserve   = 4096
	maxDispatchDocByte = 32 << 10
)

// errPromptBudget is the named refusal for an envelope that cannot fit.
var errPromptBudget = fmt.Errorf("prompt_budget_exceeded")

// jsonContentLen is the JSON string content length with HTML escaping
// disabled, excluding the surrounding quotes and the encoder's trailing LF.
func jsonContentLen(b []byte) (int, error) {
	if !utf8.Valid(b) {
		return 0, fmt.Errorf("prompt_budget_exceeded: not UTF-8")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(string(b)); err != nil {
		return 0, err
	}
	raw := buf.Bytes()
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '\n' || raw[len(raw)-2] != '"' {
		return 0, fmt.Errorf("prompt_budget_exceeded: encoder shape")
	}
	return len(raw) - 3, nil
}

// checkTriageManualBudget is the pre-registration check of the triage pair.
func checkTriageManualBudget(instruction, runbook []byte) error {
	raw := len(instruction) + len(runbook)
	if raw > triageManualCap {
		return fmt.Errorf("%w: triage manuals are %d bytes", errPromptBudget, raw)
	}
	left, err := jsonContentLen(instruction)
	if err != nil {
		return err
	}
	right, err := jsonContentLen(runbook)
	if err != nil {
		return err
	}
	if raw+triageTaskCeiling+devPromptReserve > adapter.MaxGrokPromptBytes {
		return fmt.Errorf("%w: raw triage envelope %d", errPromptBudget, raw+triageTaskCeiling+devPromptReserve)
	}
	if left+right+triageTaskCeiling+devPromptReserve > adapter.MaxGrokPromptBytes {
		return fmt.Errorf("%w: encoded triage envelope %d", errPromptBudget, left+right+triageTaskCeiling+devPromptReserve)
	}
	return nil
}

// checkTriageTaskBudget bounds goal plus acceptance, raw and JSON-encoded,
// before a recovery triage dispatch. Payload stays empty.
func checkTriageTaskBudget(goal, acceptance string) error {
	gb, ab := []byte(goal), []byte(acceptance)
	if len(gb)+len(ab) > triageTaskCeiling {
		return fmt.Errorf("%w: triage task text %d", errPromptBudget, len(gb)+len(ab))
	}
	eg, err := jsonContentLen(gb)
	if err != nil {
		return err
	}
	ea, err := jsonContentLen(ab)
	if err != nil {
		return err
	}
	if eg+ea > triageTaskCeiling {
		return fmt.Errorf("%w: encoded triage task text %d", errPromptBudget, eg+ea)
	}
	return nil
}

// checkGoalBudget enforces contract.MaxGoalBytes on the raw UTF-8 goal
// before a plan is exposed through dev-next.
func checkGoalBudget(goal string) error {
	if !utf8.ValidString(goal) {
		return fmt.Errorf("%w: goal is not UTF-8", errPromptBudget)
	}
	if len(goal) > contract.MaxGoalBytes {
		return fmt.Errorf("%w: goal is %d bytes", errPromptBudget, len(goal))
	}
	return nil
}
