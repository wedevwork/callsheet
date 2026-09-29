package mcpqual

import (
	"fmt"
	"regexp"
)

// Limits shared by the probe, the case file and the evidence files.
const (
	// MaxFrameBytes bounds each probe input and output frame (without its
	// line terminator).
	MaxFrameBytes = 64 << 10
	// MaxEvidenceFileBytes bounds every per-case event file and vendor
	// transcript; reaching it invalidates the affected experiment.
	MaxEvidenceFileBytes = 8 << 20
	// MaxCaseFileBytes bounds a probe case file.
	MaxCaseFileBytes = 64 << 10
	// MaxDelay bounds a planned probe delay.
	MaxDelayMS = 2 * 60 * 60 * 1000
	// maxCases bounds the cases of one case file.
	maxCases = 8
	// maxMinProgress bounds a case's min_progress gate.
	maxMinProgress = 100
	// maxTokenBytes bounds a progress token's JSON encoding.
	maxTokenBytes = 1024
	// ProtocolVersion is the only MCP protocol revision the probe accepts.
	ProtocolVersion = "2025-06-18"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CaseFile is the probe's measurement input: the case parameters come from
// this file, never from the model, so a session cannot alter them.
type CaseFile struct {
	Version int         `json:"version"`
	RunID   string      `json:"run_id"`
	Nonce   string      `json:"nonce"`
	Cases   []ProbeCase `json:"cases"`
}

// ProbeCase is one slow-tool case: its delay before the result and, when
// nonzero, the interval of progress notifications (sent only when the
// request carries a progress token). A planned zero-delay health marker is
// listed as its own case.
type ProbeCase struct {
	CaseID             string `json:"case_id"`
	DelayMS            int64  `json:"delay_ms"`
	ProgressIntervalMS int64  `json:"progress_interval_ms"`
	// MinProgress holds the result until that many progress notifications
	// were written (when the request carries a token), so a progress case
	// always has delivered progress evidence, whatever the scheduling.
	MinProgress int `json:"min_progress,omitempty"`
}

// ParseCaseFile strictly decodes and validates a version-1 case file.
func ParseCaseFile(b []byte) (CaseFile, error) {
	var cf CaseFile
	if len(b) > MaxCaseFileBytes {
		return cf, fmt.Errorf("case file: %d bytes exceeds %d", len(b), MaxCaseFileBytes)
	}
	if err := decodeStrict(b, &cf); err != nil {
		return cf, fmt.Errorf("case file: %w", err)
	}
	return cf, cf.Validate()
}

// Validate checks the case file contract.
func (cf CaseFile) Validate() error {
	switch {
	case cf.Version != 1:
		return fmt.Errorf("case file: version %d, want 1", cf.Version)
	case !idPattern.MatchString(cf.RunID):
		return fmt.Errorf("case file: run_id %q is not a 1-64 character identifier", cf.RunID)
	case !idPattern.MatchString(cf.Nonce):
		return fmt.Errorf("case file: nonce %q is not a 1-64 character identifier", cf.Nonce)
	case len(cf.Cases) == 0 || len(cf.Cases) > maxCases:
		return fmt.Errorf("case file: %d cases, want 1-%d", len(cf.Cases), maxCases)
	}
	seen := map[string]bool{}
	for _, c := range cf.Cases {
		switch {
		case !idPattern.MatchString(c.CaseID):
			return fmt.Errorf("case file: case_id %q is not a 1-64 character identifier", c.CaseID)
		case seen[c.CaseID]:
			return fmt.Errorf("case file: duplicate case_id %q", c.CaseID)
		case c.DelayMS < 0 || c.DelayMS > MaxDelayMS:
			return fmt.Errorf("case file: %s delay_ms %d outside 0-%d", c.CaseID, c.DelayMS, MaxDelayMS)
		case c.ProgressIntervalMS < 0 || c.ProgressIntervalMS > MaxDelayMS:
			return fmt.Errorf("case file: %s progress_interval_ms %d outside 0-%d", c.CaseID, c.ProgressIntervalMS, MaxDelayMS)
		case c.MinProgress < 0 || c.MinProgress > maxMinProgress || c.MinProgress > 0 && (c.ProgressIntervalMS == 0 || c.DelayMS == 0):
			return fmt.Errorf("case file: %s min_progress %d needs a progress interval and a delay (at most %d)", c.CaseID, c.MinProgress, maxMinProgress)
		}
		seen[c.CaseID] = true
	}
	return nil
}

func (cf CaseFile) lookup(id string) (ProbeCase, bool) {
	for _, c := range cf.Cases {
		if c.CaseID == id {
			return c, true
		}
	}
	return ProbeCase{}, false
}
