package reale2e

import (
	"regexp"
	"slices"
)

// The light scenario's closed v2 bundle layout (design 12a-dev-flow-light,
// Evidence protocol). It keeps the shared development families (preflights,
// tasks, transitions, the session, validation and the environment) and
// adds the light families; it has no design decisions or receipts, no
// feature-request record and no changed-code coverage.json.

// scenarioLight is the light development scenario.
const scenarioLight = "light"

// Light layout families.
var (
	devLightSessionRE = regexp.MustCompile(`^session/(phase-1\.stdout\.jsonl|phase-1\.stderr\.txt|transcript-phase-1\.jsonl|coordinator-exit\.json|event-map\.json)$`)
	devLightRootFiles = map[string]bool{devManifestFile: true, "events.jsonl": true, devLedgerFile: true, devAdmissionsFile: true, devCleanupFile: true,
		devCostFile: true, "report.json": true, "report.txt": true, "audit.json": true, "audit.md": true, "audit-review.md": true}
	devLightSetupFiles = map[string]bool{"setup/sources.json": true, "setup/rendered.json": true, "setup/installed-manuals.json": true,
		"setup/flow-label.json": true, "setup/seed-push.json": true, "setup/flow.json": true, "setup/roles.json": true, "setup/mcp-config.json": true,
		"setup/coordinator-system.md": true, "setup/phase.md": true, "setup/scenario.md": true, "setup/light-scenario.json": true,
		"setup/launch-phase-1.json": true, lightDecisionFile: true, lightReceiptFile: true}
	devLightValidationFiles = map[string]bool{"validation/light-baseline.json": true, "validation/light-regression.json": true,
		"validation/light-coverage.json": true, "validation/light/baseline-tests.jsonl": true, "validation/light/baseline-coverage.out": true,
		"validation/light/baseline-stdout.txt": true, "validation/light/baseline-stderr.txt": true, "validation/light/red-tests.jsonl": true,
		"validation/light/red-stderr.txt": true, "validation/light/green-tests.jsonl": true, "validation/light/green-stderr.txt": true,
		"validation/final-test.json": true, "validation/final-stdout.txt": true, "validation/final-stderr.txt": true, buildStdoutFile: true,
		buildStderrFile: true, "validation/cases.json": true, "validation/requirements.json": true, "validation/coverage.out": true,
		"validation/coverage-stdout.txt": true, "validation/coverage-stderr.txt": true}
)

// devLightFileAllowed reports a closed light layout file path (captures
// excluded: devLayoutFor adds them).
func devLightFileAllowed(rel string) bool {
	switch {
	case devLightRootFiles[rel], devLightSetupFiles[rel], devLightValidationFiles[rel], rel == devTreesFile:
		return true
	case devPreflightRE.MatchString(rel), devTaskFileRE.MatchString(rel), devTransitionRE.MatchString(rel), devLightSessionRE.MatchString(rel):
		return true
	case rel == "environment/diff.json" || slices.Contains(envFiles("before"), rel) || slices.Contains(envFiles("after"), rel):
		return true
	}
	return false
}

// devLightDirAllowed reports a closed light layout directory.
func devLightDirAllowed(rel string) bool {
	switch rel {
	case "setup", "intake", "preflight", "tasks", "transitions", "session", "workspace", devRepoDir, "validation", "validation/light", "environment":
		return true
	}
	return devDirRE.MatchString(rel)
}

// devLightDocType is a light-only record's strict type (nil: none).
func devLightDocType(rel string) (any, string) {
	switch rel {
	case lightDecisionFile:
		return &devEnvelope[DevIntakeDecision]{}, devKind("intake-decision")
	case lightReceiptFile:
		return &devEnvelope[DevIntakeReceipt]{}, devKind("intake-receipt")
	case "setup/light-scenario.json":
		return &DevLightScenario{}, devKind("light-scenario")
	case "validation/light-baseline.json":
		return &DevLightBaseline{}, devKind("light-baseline")
	case "validation/light-regression.json":
		return &DevLightRegression{}, devKind("light-regression")
	case "validation/light-coverage.json":
		return &DevLightCoverageRecord{}, devKind("light-coverage")
	}
	return nil, ""
}

func (m *DevLightScenario) schemaID() string       { return m.Schema }
func (m *DevLightBaseline) schemaID() string       { return m.Schema }
func (m *DevLightRegression) schemaID() string     { return m.Schema }
func (m *DevLightCoverageRecord) schemaID() string { return m.Schema }
func (m *DevLightScenario) runIDOf() string        { return m.RunID }
func (m *DevLightBaseline) runIDOf() string        { return m.RunID }
func (m *DevLightRegression) runIDOf() string      { return m.RunID }
func (m *DevLightCoverageRecord) runIDOf() string  { return m.RunID }

// The light fixture's captured files and their logical capture directory.
var lightFixtureFiles = []string{"FEATURE.md", "go.mod", "main.go", "main_test.go"}

const (
	lightFixtureCaptureDir = "setup/light-fixture"
	// devLightFixtureDir is the committed light seed fixture.
	devLightFixtureDir = "internal/reale2e/testdata/devlight-seed"
)

// DevMethodNamesFor are scenario's shared-method and exercise inputs in
// provenance order: light selects its scenario prompt.
func DevMethodNamesFor(scenario string) []string {
	names := DevMethodNames()
	if scenario == scenarioLight {
		names[len(names)-1] = DevScenarioLight
	}
	return names
}

// DevScenarioLight is the light scenario prompt's exercise file.
const DevScenarioLight = "scenario-light.md"

// devLight is one light attempt's scenario state.
type devLight struct {
	d  *devSupervisor
	st *devLightState
	// prompted, when set, runs right after the intake prompt is printed
	// (the offline rig types its fresh answer there).
	prompted func()
}
