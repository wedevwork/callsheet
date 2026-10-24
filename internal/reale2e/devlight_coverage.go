package reale2e

import (
	"fmt"
	"math/big"
)

// The light coverage comparison (design 12a-dev-flow-light, Coverage
// comparison; FP-10). Both sides are the normal green suites (never the
// deliberately red overlay): the immutable buggy seed source with its nine
// seed tests, and the candidate source with its complete tests, each run
// with the pinned go test -count=1 -covermode=set -coverprofile=<fresh
// scratch>/coverage.out ./... in the same module, Go and environment. Every
// production statement of the module's main.go counts, main included; no
// function is selected or excluded. The candidate passes exactly when
// candidateCovered*baselineTotal >= baselineCovered*candidateTotal, in
// checked (arbitrary-precision) integer arithmetic, never rounded
// percentages. Both denominators must be positive.

// Light coverage codes.
const (
	CodeCoverageDecreased = "coverage_decreased"
)

// lightCoverScope is the light production scope's file under module.
func lightCoverScope(module string) string { return module + "/main.go" }

// LightCoverage is one side's whole-production coverage.
type LightCoverage struct {
	Module        string `json:"module"`
	Scope         string `json:"scope"`
	Blocks        int    `json:"blocks"`
	Covered       int    `json:"covered"`
	Total         int    `json:"total"`
	ProfileSHA256 string `json:"profile_sha256"`
	SourceSHA256  string `json:"source_sha256"`
}

// deriveWholeCoverage counts every production statement of src (the
// module's main.go) from a validated mode: set profile: mode, file, span,
// count and overlap checks against src, the parse of src, and every block
// inside one declaration.
func deriveWholeCoverage(src, profile []byte, module string) (LightCoverage, error) {
	cov := LightCoverage{Module: module, Scope: lightCoverScope(module), ProfileSHA256: sha256Hex(profile), SourceSHA256: sha256Hex(src)}
	if module == "" {
		return cov, coverageErr("no module path")
	}
	blocks, err := parseCoverProfile(profile, lightCoverScope(module))
	if err != nil {
		return cov, err
	}
	if err := validateBlocks(blocks, src); err != nil {
		return cov, err
	}
	regs, _, err := declRegions(src)
	if err != nil {
		return cov, coverageErr("main.go does not parse")
	}
	for _, b := range blocks {
		in := false
		for _, r := range regs {
			if !(coverPos{b.sl, b.sc}).before(r.start) && !(r.end).before(coverPos{b.el, b.ec}) {
				in = true
				break
			}
		}
		if !in {
			return cov, coverageErr("a profile block maps to no declaration")
		}
		cov.Blocks++
		cov.Total += b.stmts
		if b.count > 0 {
			cov.Covered += b.stmts
		}
	}
	if cov.Total <= 0 {
		return cov, coverageErr("no executable statement")
	}
	return cov, nil
}

// LightCoverageComparison is the integer comparison of two sides.
type LightCoverageComparison struct {
	// Left is candidateCovered*baselineTotal and Right
	// baselineCovered*candidateTotal, in decimal.
	Left  string `json:"left"`
	Right string `json:"right"`
	Pass  bool   `json:"pass"`
	Code  string `json:"code"`
}

// compareLightCoverage decides the nondecreasing fraction: an invalid side
// (nonpositive denominator, covered outside 0..total) is unobserved.
func compareLightCoverage(base, cand LightCoverage) LightCoverageComparison {
	if base.Total <= 0 || cand.Total <= 0 || base.Covered < 0 || cand.Covered < 0 || base.Covered > base.Total || cand.Covered > cand.Total {
		return LightCoverageComparison{Code: CodeCoverageUnobserved}
	}
	left := new(big.Int).Mul(big.NewInt(int64(cand.Covered)), big.NewInt(int64(base.Total)))
	right := new(big.Int).Mul(big.NewInt(int64(base.Covered)), big.NewInt(int64(cand.Total)))
	c := LightCoverageComparison{Left: left.String(), Right: right.String(), Pass: left.Cmp(right) >= 0}
	if !c.Pass {
		c.Code = CodeCoverageDecreased
	}
	return c
}

// coverageFraction renders covered/total for a report line.
func coverageFraction(c LightCoverage) string { return fmt.Sprintf("%d/%d", c.Covered, c.Total) }
