// Package containeracceptance is the M3/M4 container acceptance's stub
// coordinator (design m3-m4-container-e2e): inside one Linux container it
// drives the real Callsheet CLI against a real plane and two real sidecars
// with the fake worker over verified TLS on loopback, checks answers and
// published Git results, and returns machine-readable evidence records.
// The tagged package tests/container wraps it; cmd/devcheck's
// container-e2e operation validates the records on the host. It is test
// infrastructure, not product API: production packages never import it,
// it calls no internal service to perform or fabricate a coordinator
// result, and it never runs git, a vendor CLI or a network lookup.
package containeracceptance

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
)

// Fixed image paths and the writable work root.
const (
	ImageCallsheet   = "/callsheet"
	ImageFakeAdapter = "/fake-adapter"
	ImageTest        = "/acceptance.test"
	ImageFixtures    = "/fixtures"
	ImageWorkRoot    = "/tmp/acceptance"
)

// The container metadata environment (literal docker --env entries).
const (
	EnvRunID     = "CALLSHEET_E2E_RUN_ID"
	EnvIteration = "CALLSHEET_E2E_ITERATION"
	EnvTotal     = "CALLSHEET_E2E_TOTAL"
	EnvRevision  = "CALLSHEET_E2E_REVISION"
	EnvArch      = "CALLSHEET_E2E_ARCH"
	EnvHashes    = "CALLSHEET_E2E_HASHES"
)

// Binary hash keys.
const (
	HashCallsheet      = "callsheet"
	HashFakeAdapter    = "fake_adapter"
	HashAcceptanceTest = "acceptance_test"
)

// MaxIterations bounds the host repetition count.
const MaxIterations = 20

// Config is one container process's acceptance configuration: the
// invocation's run ID, its one-based iteration of Total, the host-provided
// source revision and Linux architecture, the expected binary hashes, the
// executables, the fixture bundle and the writable work root.
type Config struct {
	RunID, SourceRevision, Architecture                 string
	Iteration, Total                                    int
	ExpectedHashes                                      map[string]string
	CallsheetPath, FakeAdapterPath, FixtureDir, WorkDir string
}

var (
	runIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	hex40RE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// hashKeys are the exact expected hash keys.
var hashKeys = []string{HashAcceptanceTest, HashCallsheet, HashFakeAdapter}

// Validate checks every field before any child starts.
func (c Config) Validate() error {
	switch {
	case !runIDRE.MatchString(c.RunID):
		return fmt.Errorf("containeracceptance: run ID %q is invalid", c.RunID)
	case c.Iteration < 1 || c.Total < c.Iteration || c.Total > MaxIterations:
		return fmt.Errorf("containeracceptance: iteration %d of %d is outside 1 <= iteration <= total <= %d", c.Iteration, c.Total, MaxIterations)
	case !hex40RE.MatchString(c.SourceRevision):
		return fmt.Errorf("containeracceptance: source revision %q is not a full commit hash", c.SourceRevision)
	case c.Architecture != "amd64" && c.Architecture != "arm64":
		return fmt.Errorf("containeracceptance: architecture %q is not amd64 or arm64", c.Architecture)
	case len(c.ExpectedHashes) != len(hashKeys):
		return fmt.Errorf("containeracceptance: expected hashes must hold exactly %v", hashKeys)
	}
	for _, k := range hashKeys {
		if !hex64RE.MatchString(c.ExpectedHashes[k]) {
			return fmt.Errorf("containeracceptance: expected hash %s is not a lowercase SHA-256", k)
		}
	}
	for name, p := range map[string]string{"callsheet": c.CallsheetPath, "fake adapter": c.FakeAdapterPath, "fixture": c.FixtureDir, "work": c.WorkDir} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("containeracceptance: %s path %q is not absolute", name, p)
		}
	}
	return nil
}

// ConfigFromEnv reads the container metadata variables (and nothing else)
// through getenv, sets the fixed image paths and validates every field.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{RunID: getenv(EnvRunID), SourceRevision: getenv(EnvRevision), Architecture: getenv(EnvArch),
		CallsheetPath: ImageCallsheet, FakeAdapterPath: ImageFakeAdapter, FixtureDir: ImageFixtures, WorkDir: ImageWorkRoot}
	var err error
	if c.Iteration, err = strictInt(getenv(EnvIteration)); err != nil {
		return Config{}, fmt.Errorf("containeracceptance: %s: %v", EnvIteration, err)
	}
	if c.Total, err = strictInt(getenv(EnvTotal)); err != nil {
		return Config{}, fmt.Errorf("containeracceptance: %s: %v", EnvTotal, err)
	}
	raw := getenv(EnvHashes)
	if err := json.Unmarshal([]byte(raw), &c.ExpectedHashes); err != nil || raw == "" {
		return Config{}, fmt.Errorf("containeracceptance: %s is not a JSON object of hashes", EnvHashes)
	}
	return c, c.Validate()
}

// strictInt parses a plain decimal integer (no sign, no padding).
func strictInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || s != strconv.Itoa(n) || n < 0 {
		return 0, fmt.Errorf("%q is not a decimal integer", s)
	}
	return n, nil
}
