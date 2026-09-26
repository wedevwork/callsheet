package cicheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// validYAML satisfies the four-job contract with job order, formatting, key
// order and comments that differ from the checked-in workflow; only
// semantics are fixed.
const validYAML = `permissions: {contents: read}
jobs:
  macos:
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
        with: {persist-credentials: false}
      - uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0
        with:
          check-latest: false
          go-version-file: go.mod
          cache-dependency-path: go.sum
          cache: true
      - run: go mod download
      - name: native
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck native
    name: ci-macos
    runs-on: macos-15
    timeout-minutes: 30
    defaults: {run: {shell: bash}}
    env: {GOTOOLCHAIN: local, GOFLAGS: -mod=readonly}
  linux:
    name: ci-linux
    runs-on: ubuntu-24.04
    timeout-minutes: 45
    defaults:
      run:
        shell: bash
    env:
      GOTOOLCHAIN: local
      GOFLAGS: -mod=readonly
    steps:
      - name: Checkout
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
        with:
          persist-credentials: false
      - name: Setup
        uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0
        with:
          go-version-file: go.mod
          cache: true
          cache-dependency-path: go.sum
          check-latest: false
      - name: Download
        run: go mod download
      - name: test
        run: go run ./cmd/devcheck test
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
      - name: coverage
        run: go run ./cmd/devcheck coverage
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
      - name: bench
        run: go run ./cmd/devcheck bench
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
      - name: cross
        run: |
          go run ./cmd/devcheck cross
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
  macos-stress:
    name: ci-macos-stress
    runs-on: macos-15
    timeout-minutes: 20
    defaults: {run: {shell: bash}}
    env: {GOFLAGS: -mod=readonly, GOTOOLCHAIN: local}
    steps:
      - with: {persist-credentials: false}
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
      - with: {go-version-file: go.mod, cache: true, cache-dependency-path: go.sum, check-latest: false}
        uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417
      - name: Download (macos-stress)
        run: go mod download
      - name: devcheck stress (race, repeat count, varied -cpu)
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck stress
  linux-stress:
    name: ci-linux-stress
    runs-on: ubuntu-24.04
    timeout-minutes: 20
    defaults:
      run:
        shell: bash
    env:
      GOTOOLCHAIN: local
      GOFLAGS: -mod=readonly
    steps:
      - name: Checkout
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
        with:
          persist-credentials: false
      - name: Setup
        uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0
        with:
          go-version-file: go.mod
          cache: true
          cache-dependency-path: go.sum
          check-latest: false
      - name: Download (linux-stress)
        run: go mod download
      - name: devcheck stress (race, repeat count, varied -cpu)
        run: go run ./cmd/devcheck stress
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
name: CI
on:
  pull_request:
    branches: [main]
  push:
    branches:
      - main
`

// rep replaces the first occurrence of old, which must exist.
func rep(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	return strings.Replace(s, old, new, 1)
}

const coverageStep = `      - name: coverage
        run: go run ./cmd/devcheck coverage
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
`

// The stress steps and download steps of the two stress jobs are written
// differently so each can be mutated independently.
const (
	macosStressDownload = `      - name: Download (macos-stress)
        run: go mod download
`
	linuxStressDownload = `      - name: Download (linux-stress)
        run: go mod download
`
	macosStressStep = `      - name: devcheck stress (race, repeat count, varied -cpu)
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck stress
`
	linuxStressStep = `      - name: devcheck stress (race, repeat count, varied -cpu)
        run: go run ./cmd/devcheck stress
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
`
	crossStep = `      - name: cross
        run: |
          go run ./cmd/devcheck cross
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
`
	nativeStep = `      - name: native
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck native
`
)

const benchStep = `      - name: bench
        run: go run ./cmd/devcheck bench
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
`

func TestValidWorkflows(t *testing.T) {
	if err := ValidateWorkflow([]byte(validYAML)); err != nil {
		t.Fatalf("valid fixture: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkflow(data); err != nil {
		t.Fatalf("checked-in workflow: %v", err)
	}
	// Future pins may change hashes as long as they stay full commit SHAs.
	other := strings.ReplaceAll(validYAML, "de0fac2e4500dabe0009e67214ff5f5447ce83dd", strings.Repeat("a", 40))
	if err := ValidateWorkflow([]byte(other)); err != nil {
		t.Fatalf("repinned: %v", err)
	}
}

func TestMutationsFailWithPath(t *testing.T) {
	cases := []struct {
		name    string
		old     string
		new     string
		wantErr []string
	}{
		{"malformed", "name: CI\n", "name: [CI\n", []string{"malformed YAML"}},
		{"duplicate key", "name: CI\n", "name: CI\nname: CI\n", []string{"name: duplicate key"}},
		{"duplicate nested key", "    runs-on: ubuntu-24.04\n", "    runs-on: ubuntu-24.04\n    runs-on: ubuntu-24.04\n", []string{"jobs.linux.runs-on: duplicate key"}},
		{"anchor and alias", "    env:\n      GOTOOLCHAIN: local\n      GOFLAGS: -mod=readonly\n    steps:", "    env: &e\n      GOTOOLCHAIN: local\n      GOFLAGS: -mod=readonly\n    x: *e\n    steps:",
			[]string{"jobs.linux.env: YAML anchors are not allowed", "jobs.linux.x: YAML aliases are not allowed"}},
		{"merge key", "    env:\n      GOTOOLCHAIN: local\n      GOFLAGS: -mod=readonly\n    steps:", "    env:\n      <<: {GOTOOLCHAIN: local}\n      GOFLAGS: -mod=readonly\n    steps:",
			[]string{"jobs.linux.env.<<: YAML merge keys are not allowed"}},
		{"unknown top-level field", "name: CI\n", "name: CI\nconcurrency: {group: ci, cancel-in-progress: true}\n", []string{"concurrency: unknown field"}},
		{"top-level env", "name: CI\n", "name: CI\nenv: {GOPROXY: direct}\n", []string{"env: unknown field"}},
		{"workflow name", "name: CI\n", "name: Build\n", []string{`name: must be "CI", got "Build"`}},
		{"event added", "  pull_request:\n", "  workflow_dispatch: {}\n  pull_request:\n", []string{"on.workflow_dispatch: unknown field"}},
		{"event removed", "  pull_request:\n    branches: [main]\n", "", []string{"on.pull_request: missing required field"}},
		{"pull_request_target", "  pull_request:\n", "  pull_request_target:\n", []string{"on.pull_request_target: unknown field", "on.pull_request: missing required field"}},
		{"on as list", "on:\n  pull_request:\n    branches: [main]\n  push:\n    branches:\n      - main\n", "on: [push, pull_request]\n", []string{"on: must be a mapping"}},
		{"branch changed", "    branches: [main]\n", "    branches: [develop]\n", []string{`on.pull_request.branches[0]: must be "main", got "develop"`}},
		{"branch added", "    branches: [main]\n", "    branches: [main, dev]\n", []string{"on.pull_request.branches: must be exactly [main]"}},
		{"paths filter", "    branches: [main]\n", "    branches: [main]\n    paths: [internal/**]\n", []string{"on.pull_request.paths: unknown field"}},
		{"types filter", "    branches: [main]\n", "    branches: [main]\n    types: [opened]\n", []string{"on.pull_request.types: unknown field"}},
		{"job name", "    name: ci-linux\n", "    name: linux\n", []string{`jobs.linux.name: must be "ci-linux", got "linux"`}},
		{"runner", "    runs-on: ubuntu-24.04\n", "    runs-on: ubuntu-latest\n", []string{`jobs.linux.runs-on: must be "ubuntu-24.04", got "ubuntu-latest"`}},
		{"runner list", "    runs-on: macos-15\n", "    runs-on: [self-hosted]\n", []string{`jobs.macos.runs-on: must be "macos-15"`}},
		{"timeout", "    timeout-minutes: 30\n", "    timeout-minutes: 360\n", []string{`jobs.macos.timeout-minutes: must be "30", got "360"`}},
		{"timeout as string", "    timeout-minutes: 45\n", "    timeout-minutes: \"45\"\n", []string{`jobs.linux.timeout-minutes: must be "45"`}},
		{"timeout omitted", "    timeout-minutes: 45\n", "", []string{"jobs.linux.timeout-minutes: missing required field"}},
		{"extra windows job", "jobs:\n", "jobs:\n  windows:\n    name: ci-windows\n    runs-on: windows-2025\n    timeout-minutes: 30\n", []string{"jobs.windows: unknown field"}},
		{"job removed", "  macos:\n", "  macos-old:\n", []string{"jobs.macos-old: unknown field", "jobs.macos: missing required field"}},
		{"permissions omitted", "permissions: {contents: read}\n", "", []string{"permissions: missing required field"}},
		{"permissions write-all", "permissions: {contents: read}\n", "permissions: write-all\n", []string{"permissions: must be a mapping"}},
		{"permissions elevated", "permissions: {contents: read}\n", "permissions: {contents: write}\n", []string{`permissions.contents: must be "read", got "write"`}},
		{"permissions extra", "permissions: {contents: read}\n", "permissions: {contents: read, id-token: write}\n", []string{"permissions.id-token: unknown field"}},
		{"job permission override", "    name: ci-linux\n", "    name: ci-linux\n    permissions: {contents: write}\n", []string{"jobs.linux.permissions: unknown field"}},
		{"job if", "    name: ci-linux\n", "    name: ci-linux\n    if: github.event_name == 'push'\n", []string{"jobs.linux.if: unknown field"}},
		{"job needs", "    name: ci-macos\n", "    name: ci-macos\n    needs: linux\n", []string{"jobs.macos.needs: unknown field"}},
		{"job continue-on-error", "    name: ci-linux\n", "    name: ci-linux\n    continue-on-error: true\n", []string{"jobs.linux.continue-on-error: unknown field"}},
		{"job container", "    name: ci-linux\n", "    name: ci-linux\n    container: golang:1.26\n", []string{"jobs.linux.container: unknown field"}},
		{"job matrix", "    name: ci-linux\n", "    name: ci-linux\n    strategy: {matrix: {os: [a, b]}}\n", []string{"jobs.linux.strategy: unknown field"}},
		{"job not mapping", "  macos:\n    steps:\n", "  macos: ci\n  x:\n    steps:\n", []string{"jobs.macos: must be a mapping"}},
		{"shell", "        shell: bash\n", "        shell: sh\n", []string{`jobs.linux.defaults.run.shell: must be "bash", got "sh"`}},
		{"working directory", "        shell: bash\n", "        shell: bash\n        working-directory: internal\n", []string{"jobs.linux.defaults.run.working-directory: unknown field"}},
		{"toolchain env", "      GOTOOLCHAIN: local\n", "      GOTOOLCHAIN: auto\n", []string{`jobs.linux.env.GOTOOLCHAIN: must be "local", got "auto"`}},
		{"network env at job level", "      GOFLAGS: -mod=readonly\n", "      GOFLAGS: -mod=readonly\n      GOPROXY: \"off\"\n", []string{"jobs.linux.env.GOPROXY: unknown field"}},
		{"cache omitted", "          cache: true\n", "", []string{"jobs.macos.steps[1].with.cache: missing required field"}},
		{"cache disabled", "          cache: true\n", "          cache: false\n", []string{`jobs.macos.steps[1].with.cache: must be "true", got "false"`}},
		{"module version omitted", "          go-version-file: go.mod\n", "", []string{"jobs.macos.steps[1].with.go-version-file: missing required field"}},
		{"literal go version", "          go-version-file: go.mod\n", "          go-version-file: go.mod\n          go-version: '1.26'\n", []string{"jobs.macos.steps[1].with.go-version: unknown field"}},
		{"check-latest", "          check-latest: false\n", "          check-latest: true\n", []string{`jobs.macos.steps[1].with.check-latest: must be "false", got "true"`}},
		{"unpinned tag", "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2\n        with:\n          persist", "actions/checkout@v6\n        with:\n          persist", []string{`jobs.linux.steps[0].uses: must pin a full 40-hex commit SHA, got "v6"`}},
		{"short sha", "actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0\n        with:\n          go-version-file", "actions/setup-go@4b73464\n        with:\n          go-version-file", []string{"jobs.linux.steps[1].uses: must pin a full 40-hex commit SHA"}},
		{"uppercase sha", "actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0\n        with:\n          go-version-file", "actions/setup-go@4B73464BB391D4059BD26B0524D20DF3927BD417\n        with:\n          go-version-file", []string{"jobs.linux.steps[1].uses: must pin a full 40-hex commit SHA"}},
		{"wrong action identity", "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2\n        with:\n          persist", "evil/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd\n        with:\n          persist", []string{`jobs.linux.steps[0].uses: must use action actions/checkout, got "evil/checkout"`}},
		{"docker action", "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2\n        with:\n          persist", "docker://alpine\n        with:\n          persist", []string{"jobs.linux.steps[0].uses: must be actions/checkout@<40-hex commit SHA>"}},
		{"ref override", "          persist-credentials: false\n", "          persist-credentials: false\n          ref: main\n", []string{"jobs.linux.steps[0].with.ref: unknown field"}},
		{"persisted credentials", "          persist-credentials: false\n", "          persist-credentials: true\n", []string{`jobs.linux.steps[0].with.persist-credentials: must be "false", got "true"`}},
		{"step condition", "      - name: test\n", "      - name: test\n        if: always()\n", []string{"jobs.linux.steps[3].if: unknown field"}},
		{"step continue-on-error", "      - name: test\n", "      - name: test\n        continue-on-error: true\n", []string{"jobs.linux.steps[3].continue-on-error: unknown field"}},
		{"step working directory", "      - name: test\n", "      - name: test\n        working-directory: tests\n", []string{"jobs.linux.steps[3].working-directory: unknown field"}},
		{"step shell", "      - name: Download\n", "      - name: Download\n        shell: sh\n", []string{"jobs.linux.steps[2].shell: unknown field"}},
		{"step name expression", "      - name: test\n", "      - name: test ${{ github.ref }}\n", []string{"jobs.linux.steps[3].name: expressions are not allowed"}},
		{"empty step name", "      - name: test\n", "      - name: \"\"\n", []string{"jobs.linux.steps[3].name: must be a nonempty string"}},
		{"extra step", "  push:\n", "  push:\n", nil}, // placeholder replaced below
		{"reordered steps", coverageStep + benchStep, benchStep + coverageStep,
			[]string{`jobs.linux.steps[4].run: must run devcheck stage "coverage", got "bench"`, `jobs.linux.steps[5].run: must run devcheck stage "bench", got "coverage"`}},
		{"omitted coverage gate", coverageStep, "", []string{"jobs.linux.steps: must have exactly 7 steps", `jobs.linux.steps: missing check step for devcheck stage "coverage"`}},
		{"network env on download", "        run: go mod download\n", "        run: go mod download\n        env: {GOPROXY: \"off\"}\n", []string{"jobs.linux.steps[2].env: unknown field"}},
		{"network env missing", "        run: go run ./cmd/devcheck test\n        env:\n          GOPROXY: \"off\"\n          GOSUMDB: \"off\"\n", "        run: go run ./cmd/devcheck test\n", []string{"jobs.linux.steps[3].env: missing required field"}},
		{"network env wrong", "          GOPROXY: \"off\"\n", "          GOPROXY: direct\n", []string{`jobs.linux.steps[3].env.GOPROXY: must be "off", got "direct"`}},
		{"download changed", "        run: go mod download\n", "        run: go mod tidy\n", []string{`jobs.linux.steps[2].run: must be "go mod download", got "go mod tidy"`}},
		{"download with operator", "        run: go mod download\n", "        run: go mod download || true\n", []string{`jobs.linux.steps[2].run: token "||" contains a shell operator`}},
		{"command suffix operator", "run: go run ./cmd/devcheck test\n", "run: go run ./cmd/devcheck test && echo ok\n", []string{`jobs.linux.steps[3].run: token "&&" contains a shell operator`}},
		{"command semicolon", "run: go run ./cmd/devcheck test\n", "run: go run ./cmd/devcheck test; true\n", []string{`jobs.linux.steps[3].run: token "test;"`}},
		{"command substitution", "run: go run ./cmd/devcheck test\n", "run: go run ./cmd/devcheck $(echo test)\n", []string{`jobs.linux.steps[3].run: token "$(echo"`}},
		{"command extra argument", "run: go run ./cmd/devcheck test\n", "run: go run ./cmd/devcheck test -o x\n", []string{`jobs.linux.steps[3].run: must be "go run ./cmd/devcheck test"`}},
		{"command multi-line", "        run: go run ./cmd/devcheck test\n", "        run: |\n          go run ./cmd/devcheck test\n          echo done\n", []string{"jobs.linux.steps[3].run: must be a single command line"}},
		{"command as list", "        run: go run ./cmd/devcheck test\n", "        run: [go, run]\n", []string{"jobs.linux.steps[3].run: must be a single command string"}},
		{"command empty", "        run: go run ./cmd/devcheck test\n", "        run: \"\"\n", []string{"jobs.linux.steps[3].run: empty command"}},
		{"nonexistent stage", "run: go run ./cmd/devcheck test\n", "run: go run ./cmd/devcheck race\n", []string{`jobs.linux.steps[3].run: unknown devcheck stage "race"`}},
		{"existing stage in wrong job", "run: go run ./cmd/devcheck native\n", "run: go run ./cmd/devcheck test\n", []string{`jobs.macos.steps[3].run: must run devcheck stage "native", got "test"`}},
		{"steps not a list", "    steps:\n      - uses: actions/checkout", "    steps: {}\n    x:\n      - uses: actions/checkout", []string{"jobs.macos.steps: must be a sequence"}},
		{"step not a mapping", "      - run: go mod download\n", "      - go mod download\n", []string{"jobs.macos.steps[2]: must be a mapping"}},
		{"non-scalar key", "name: CI\n", "name: CI\n? [a, b]\n: c\n", []string{"(root): non-scalar mapping key"}},
		{"expression in key", "name: CI\n", "name: CI\n${{ x }}: y\n", []string{"expressions are not allowed in keys"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := rep(t, validYAML, c.old, c.new)
			if c.name == "extra step" {
				data = rep(t, validYAML, crossStep, crossStep+"      - run: go run ./cmd/devcheck all\n        env: {GOPROXY: \"off\", GOSUMDB: \"off\"}\n")
				c.wantErr = []string{"jobs.linux.steps: must have exactly 7 steps", "jobs.linux.steps[7]: unexpected extra step"}
			}
			err := ValidateWorkflow([]byte(data))
			if err == nil {
				t.Fatalf("mutation accepted:\n%s", data)
			}
			for _, w := range c.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

// S2: a deleted middle check step is named, not the contract's last stage.
func TestMissingMiddleStepNamed(t *testing.T) {
	for _, c := range []struct{ step, stage string }{{coverageStep, "coverage"}, {benchStep, "bench"}} {
		err := ValidateWorkflow([]byte(rep(t, validYAML, c.step, "")))
		if err == nil || !strings.Contains(err.Error(), `jobs.linux.steps: missing check step for devcheck stage "`+c.stage+`"`) {
			t.Fatalf("deleted %s: %v", c.stage, err)
		}
		if strings.Contains(err.Error(), `missing check step for devcheck stage "cross"`) {
			t.Fatalf("deleted %s but cross reported missing: %v", c.stage, err)
		}
	}
	// Reordering removes nothing, so no stage is reported absent.
	err := ValidateWorkflow([]byte(rep(t, validYAML, coverageStep+benchStep, benchStep+coverageStep)))
	if err == nil || strings.Contains(err.Error(), "missing check step") {
		t.Fatalf("reordered: %v", err)
	}
}

func TestDocumentLevelRejections(t *testing.T) {
	for name, c := range map[string]struct{ data, want string }{
		"empty":          {"", "empty workflow"},
		"null document":  {"---\n", "(root): must be a mapping"},
		"scalar":         {"hello\n", "(root): must be a mapping"},
		"multi-document": {validYAML + "---\nname: other\n", "extra YAML document"},
		"bad second doc": {validYAML + "---\n[\n", "malformed YAML"},
		"tab indent":     {"name: CI\n\tjobs: x\n", "malformed YAML"},
	} {
		err := ValidateWorkflow([]byte(c.data))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var ve *ValidationError
	if err := ValidateWorkflow([]byte("name: x\n")); !errors.As(err, &ve) || len(ve.Problems) < 4 {
		t.Fatalf("problems = %v", err)
	}
}

func TestContractAccessors(t *testing.T) {
	if got := strings.Join(RequiredChecks(), ","); got != "ci-linux,ci-macos,ci-linux-stress,ci-macos-stress" {
		t.Fatalf("required = %s", got)
	}
	js := Jobs()
	var got []string
	for _, j := range js {
		got = append(got, fmt.Sprintf("%s|%s|%s|%d|%s", j.ID, j.Name, j.RunsOn, j.TimeoutMinutes, strings.Join(j.Stages, " ")))
	}
	if want := []string{
		"linux|ci-linux|ubuntu-24.04|45|test coverage bench cross",
		"macos|ci-macos|macos-15|30|native",
		"linux-stress|ci-linux-stress|ubuntu-24.04|20|stress",
		"macos-stress|ci-macos-stress|macos-15|20|stress",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("jobs = %q", got)
	}
	js[0].Stages[0] = "mutated"
	js[1].Stages[0] = "mutated"
	js[3].Stages[0] = "mutated"
	js[1].Name = "mutated"
	js[2].Name = "mutated"
	if Jobs()[0].Stages[0] != "test" || Jobs()[1].Stages[0] != "native" || Jobs()[3].Stages[0] != "stress" ||
		RequiredChecks()[1] != "ci-macos" || RequiredChecks()[2] != "ci-linux-stress" {
		t.Fatal("Jobs exposes contract state")
	}
	rc := RequiredChecks()
	rc[3] = "mutated"
	if RequiredChecks()[3] != "ci-macos-stress" {
		t.Fatal("RequiredChecks exposes contract state")
	}
}

func TestExtractStages(t *testing.T) {
	got, err := ExtractStages([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got["linux"], " ") != "test coverage bench cross" || strings.Join(got["macos"], " ") != "native" ||
		strings.Join(got["linux-stress"], " ") != "stress" || strings.Join(got["macos-stress"], " ") != "stress" || len(got) != 4 {
		t.Fatalf("stages = %v", got)
	}
	got, err = ExtractStages([]byte("jobs:\n  a:\n    steps: {}\n  b:\n    steps:\n      - uses: x\n      - run: [x]\n      - run: go run ./cmd/devcheck bogus\n"))
	if err != nil || len(got["a"]) != 0 || strings.Join(got["b"], " ") != "bogus" {
		t.Fatalf("loose stages = %v %v", got, err)
	}
	for _, bad := range []string{"", "jobs: []\n", "name: x\n", "[\n"} {
		if _, err := ExtractStages([]byte(bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestCheckJobNames(t *testing.T) {
	ci := []byte(validYAML)
	if err := CheckJobNames(map[string][]byte{"ci.yml": ci}); err != nil {
		t.Fatal(err)
	}
	if err := CheckJobNames(map[string][]byte{"ci.yml": ci, "lint.yml": []byte("jobs:\n  lint:\n    runs-on: x\n")}); err != nil {
		t.Fatalf("unrelated workflow: %v", err)
	}
	for name, c := range map[string]struct {
		files map[string][]byte
		want  []string
	}{
		"duplicate display name": {map[string][]byte{"ci.yml": ci, "other.yml": []byte("jobs:\n  x:\n    name: ci-linux\n")},
			[]string{`job name "ci-linux" is used by both ci.yml jobs.linux and other.yml jobs.x`}},
		"duplicate by job id": {map[string][]byte{"a.yml": ci, "b.yml": []byte("jobs:\n  ci-macos:\n    runs-on: x\n")},
			[]string{`job name "ci-macos" is used by both`}},
		"required check missing": {map[string][]byte{"ci.yml": []byte("jobs:\n  linux:\n    name: ci-linux\n")},
			[]string{`required check "ci-macos" is not defined by any workflow`, `required check "ci-linux-stress" is not defined by any workflow`, `required check "ci-macos-stress" is not defined by any workflow`}},
		"stress contexts missing": {map[string][]byte{"ci.yml": []byte("jobs:\n  linux:\n    name: ci-linux\n  macos:\n    name: ci-macos\n")},
			[]string{`required check "ci-linux-stress" is not defined by any workflow`, `required check "ci-macos-stress" is not defined by any workflow`}},
		"duplicate stress context": {map[string][]byte{"ci.yml": ci, "other.yml": []byte("jobs:\n  x:\n    name: ci-macos-stress\n")},
			[]string{`job name "ci-macos-stress" is used by both ci.yml jobs.macos-stress and other.yml jobs.x`}},
		"no workflows": {map[string][]byte{}, []string{`required check "ci-linux"`, `required check "ci-macos"`, `required check "ci-linux-stress"`, `required check "ci-macos-stress"`}},
		"malformed":    {map[string][]byte{"ci.yml": ci, "bad.yml": []byte("jobs: [\n")}, []string{"bad.yml: cicheck: malformed YAML"}},
		"no jobs":      {map[string][]byte{"ci.yml": ci, "x.yml": []byte("name: x\n")}, []string{"x.yml: no jobs mapping"}},
	} {
		err := CheckJobNames(c.files)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("%s: %v lacks %q", name, err, w)
			}
		}
	}
}

// FP-6 (01c), moved by 02b: the stress step is required, exact and
// unconditional as step 3 of both stress jobs; each mutation is rejected
// with a path-specific error.
func TestStressStepMutations(t *testing.T) {
	for _, j := range []struct {
		id, step, prev string
		index, count   int
	}{
		{"linux-stress", linuxStressStep, linuxStressDownload, 3, 4},
		{"macos-stress", macosStressStep, macosStressDownload, 3, 4},
	} {
		p := fmt.Sprintf("jobs.%s.steps[%d]", j.id, j.index)
		prev := fmt.Sprintf("jobs.%s.steps[%d]", j.id, j.index-1)
		for _, c := range []struct {
			name string
			new  string
			want []string
		}{
			{"removed", "", []string{fmt.Sprintf("jobs.%s.steps: must have exactly %d steps", j.id, j.count), fmt.Sprintf(`jobs.%s.steps: missing check step for devcheck stage "stress"`, j.id)}},
			{"substituted all", strings.Replace(j.step, "./cmd/devcheck stress", "./cmd/devcheck all", 1), []string{p + `.run: must run devcheck stage "stress", got "all"`, `missing check step for devcheck stage "stress"`}},
			{"substituted test", strings.Replace(j.step, "./cmd/devcheck stress", "./cmd/devcheck test", 1), []string{p + `.run: must run devcheck stage "stress", got "test"`}},
			{"count flag", strings.Replace(j.step, "devcheck stress\n", "devcheck stress -count=1\n", 1), []string{p + `.run: must be "go run ./cmd/devcheck stress", got "go run ./cmd/devcheck stress -count=1"`}},
			{"cpu flag", strings.Replace(j.step, "devcheck stress\n", "devcheck stress -cpu=1\n", 1), []string{p + `.run: must be "go run ./cmd/devcheck stress"`}},
			{"if", j.step + "        if: always()\n", []string{p + ".if: unknown field"}},
			{"continue-on-error", j.step + "        continue-on-error: true\n", []string{p + ".continue-on-error: unknown field"}},
			{"timeout", j.step + "        timeout-minutes: 5\n", []string{p + ".timeout-minutes: unknown field"}},
			{"offline env omitted", strings.Replace(strings.Replace(j.step, "        env: {GOSUMDB: \"off\", GOPROXY: \"off\"}\n", "", 1), "        env:\n          GOPROXY: \"off\"\n          GOSUMDB: \"off\"\n", "", 1), []string{p + ".env: missing required field"}},
			{"proxy enabled", strings.Replace(j.step, `GOPROXY: "off"`, "GOPROXY: direct", 1), []string{p + `.env.GOPROXY: must be "off", got "direct"`}},
		} {
			t.Run(j.id+" "+c.name, func(t *testing.T) {
				data := rep(t, validYAML, j.step, c.new)
				err := ValidateWorkflow([]byte(data))
				if err == nil {
					t.Fatalf("mutation accepted:\n%s", data)
				}
				for _, w := range c.want {
					if !strings.Contains(err.Error(), w) {
						t.Fatalf("error lacks %q:\n%v", w, err)
					}
				}
			})
		}
		// Swapping the stress step with the download step before it puts
		// the check where the download belongs and vice versa.
		t.Run(j.id+" reordered with download", func(t *testing.T) {
			err := ValidateWorkflow([]byte(rep(t, validYAML, j.prev+j.step, j.step+j.prev)))
			for _, w := range []string{
				prev + ".env: unknown field",
				prev + `.run: must be "go mod download", got "go run ./cmd/devcheck stress"`,
				p + ".env: missing required field",
				p + `.run: must be "go run ./cmd/devcheck stress", got "go mod download"`,
				fmt.Sprintf(`jobs.%s.steps: missing check step for devcheck stage "stress"`, j.id),
			} {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Fatalf("reordered: %v lacks %q", err, w)
				}
			}
		})
	}
}

// fixtureJobs parses validYAML, applies f to its jobs (by ID) in memory and
// returns the re-encoded workflow.
func fixtureJobs(t *testing.T, f func(jobs map[string]*yaml.Node)) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(validYAML), &doc); err != nil {
		t.Fatal(err)
	}
	js := child(doc.Content[0], "jobs")
	byID := map[string]*yaml.Node{}
	for i := 0; i+1 < len(js.Content); i += 2 {
		byID[js.Content[i].Value] = js.Content[i+1]
	}
	f(byID)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// at returns the node at a path of mapping keys and sequence indexes.
func at(t *testing.T, n *yaml.Node, path ...any) *yaml.Node {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			n = child(n, k)
		case int:
			if n == nil || n.Kind != yaml.SequenceNode || k >= len(n.Content) {
				n = nil
			} else {
				n = n.Content[k]
			}
		}
		if n == nil {
			t.Fatalf("no %v in path %v", p, path)
		}
	}
	return n
}

func str(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

func addKey(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content, str(key), value)
}

func removeKey(t *testing.T, m *yaml.Node, key string) {
	t.Helper()
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
	t.Fatalf("no key %q", key)
}

// TestFourJobContract (UT-2, iteration 02b): every job of the four-job
// contract, main and stress alike, keeps its identity, budget, stages,
// setup and restrictions; each drift is rejected with the job's path.
func TestFourJobContract(t *testing.T) {
	if err := ValidateWorkflow(fixtureJobs(t, func(map[string]*yaml.Node) {})); err != nil {
		t.Fatalf("re-encoded fixture: %v", err)
	}
	for _, j := range []struct {
		id, name, runner string
		timeout, last    int
		stages           []string
	}{
		{"linux", "ci-linux", "ubuntu-24.04", 45, 6, []string{"test", "coverage", "bench", "cross"}},
		{"macos", "ci-macos", "macos-15", 30, 3, []string{"native"}},
		{"linux-stress", "ci-linux-stress", "ubuntu-24.04", 20, 3, []string{"stress"}},
		{"macos-stress", "ci-macos-stress", "macos-15", 20, 3, []string{"stress"}},
	} {
		p := "jobs." + j.id
		last := fmt.Sprintf("%s.steps[%d]", p, j.last)
		cases := []struct {
			name   string
			mutate func(jobs map[string]*yaml.Node)
			want   []string
		}{
			{"job emptied", func(js map[string]*yaml.Node) {
				*js[j.id] = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "~"}
			}, []string{p + ": must be a mapping"}},
			{"context renamed", func(js map[string]*yaml.Node) { at(t, js[j.id], "name").Value = j.name + "-old" },
				[]string{fmt.Sprintf(`%s.name: must be %q, got %q`, p, j.name, j.name+"-old")}},
			{"runner", func(js map[string]*yaml.Node) { at(t, js[j.id], "runs-on").Value = "ubuntu-latest" },
				[]string{fmt.Sprintf(`%s.runs-on: must be %q, got "ubuntu-latest"`, p, j.runner)}},
			{"budget", func(js map[string]*yaml.Node) { at(t, js[j.id], "timeout-minutes").Value = "360" },
				[]string{fmt.Sprintf(`%s.timeout-minutes: must be "%d", got "360"`, p, j.timeout)}},
			{"budget omitted", func(js map[string]*yaml.Node) { removeKey(t, js[j.id], "timeout-minutes") },
				[]string{p + ".timeout-minutes: missing required field"}},
			{"needs", func(js map[string]*yaml.Node) { addKey(js[j.id], "needs", str("linux")) }, []string{p + ".needs: unknown field"}},
			{"job condition", func(js map[string]*yaml.Node) { addKey(js[j.id], "if", str("github.event_name == 'push'")) }, []string{p + ".if: unknown field"}},
			{"error bypass", func(js map[string]*yaml.Node) { addKey(js[j.id], "continue-on-error", str("true")) }, []string{p + ".continue-on-error: unknown field"}},
			{"matrix", func(js map[string]*yaml.Node) { addKey(js[j.id], "strategy", str("x")) }, []string{p + ".strategy: unknown field"}},
			{"concurrency", func(js map[string]*yaml.Node) { addKey(js[j.id], "concurrency", str("ci")) }, []string{p + ".concurrency: unknown field"}},
			{"permission override", func(js map[string]*yaml.Node) { addKey(js[j.id], "permissions", str("write-all")) }, []string{p + ".permissions: unknown field"}},
			{"shell", func(js map[string]*yaml.Node) { at(t, js[j.id], "defaults", "run", "shell").Value = "sh" },
				[]string{p + `.defaults.run.shell: must be "bash", got "sh"`}},
			{"module mode", func(js map[string]*yaml.Node) { at(t, js[j.id], "env", "GOFLAGS").Value = "-mod=mod" },
				[]string{p + `.env.GOFLAGS: must be "-mod=readonly", got "-mod=mod"`}},
			{"checkout pin", func(js map[string]*yaml.Node) { at(t, js[j.id], "steps", 0, "uses").Value = "actions/checkout@v6" },
				[]string{p + `.steps[0].uses: must pin a full 40-hex commit SHA, got "v6"`}},
			{"checkout credentials", func(js map[string]*yaml.Node) {
				at(t, js[j.id], "steps", 0, "with", "persist-credentials").Value = "true"
			},
				[]string{p + `.steps[0].with.persist-credentials: must be "false", got "true"`}},
			{"checkout ref", func(js map[string]*yaml.Node) { addKey(at(t, js[j.id], "steps", 0, "with"), "ref", str("main")) },
				[]string{p + ".steps[0].with.ref: unknown field"}},
			{"setup identity", func(js map[string]*yaml.Node) {
				at(t, js[j.id], "steps", 1, "uses").Value = "actions/cache@" + strings.Repeat("a", 40)
			}, []string{p + `.steps[1].uses: must use action actions/setup-go, got "actions/cache"`}},
			{"setup cache", func(js map[string]*yaml.Node) { at(t, js[j.id], "steps", 1, "with", "cache").Value = "false" },
				[]string{p + `.steps[1].with.cache: must be "true", got "false"`}},
			{"download online override", func(js map[string]*yaml.Node) {
				addKey(at(t, js[j.id], "steps", 2), "env", &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{str("GOPROXY"), str("off")}})
			}, []string{p + ".steps[2].env: unknown field"}},
			{"download replaced", func(js map[string]*yaml.Node) { at(t, js[j.id], "steps", 2, "run").Value = "go mod tidy" },
				[]string{p + `.steps[2].run: must be "go mod download", got "go mod tidy"`}},
			{"offline env", func(js map[string]*yaml.Node) {
				at(t, js[j.id], "steps", j.last, "env", "GOSUMDB").Value = "sum.golang.org"
			},
				[]string{last + `.env.GOSUMDB: must be "off", got "sum.golang.org"`}},
			{"step bypass", func(js map[string]*yaml.Node) {
				addKey(at(t, js[j.id], "steps", j.last), "continue-on-error", str("true"))
			},
				[]string{last + ".continue-on-error: unknown field"}},
			{"step condition", func(js map[string]*yaml.Node) { addKey(at(t, js[j.id], "steps", j.last), "if", str("always()")) },
				[]string{last + ".if: unknown field"}},
			{"last stage", func(js map[string]*yaml.Node) {
				at(t, js[j.id], "steps", j.last, "run").Value = "go run ./cmd/devcheck all"
			},
				[]string{fmt.Sprintf(`%s.run: must run devcheck stage %q, got "all"`, last, j.stages[len(j.stages)-1]),
					fmt.Sprintf(`%s.steps: missing check step for devcheck stage %q`, p, j.stages[len(j.stages)-1])}},
			{"extra stress", func(js map[string]*yaml.Node) {
				steps := at(t, js[j.id], "steps")
				steps.Content = append(steps.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
					str("run"), str("go run ./cmd/devcheck stress"),
					str("env"), {Kind: yaml.MappingNode, Content: []*yaml.Node{str("GOPROXY"), str("off"), str("GOSUMDB"), str("off")}}}})
			}, []string{fmt.Sprintf("%s.steps: must have exactly %d steps", p, 3+len(j.stages)), fmt.Sprintf("%s.steps[%d]: unexpected extra step", p, j.last+1)}},
		}
		for _, c := range cases {
			t.Run(j.id+" "+c.name, func(t *testing.T) {
				data := fixtureJobs(t, c.mutate)
				err := ValidateWorkflow(data)
				if err == nil {
					t.Fatalf("mutation accepted:\n%s", data)
				}
				for _, w := range c.want {
					if !strings.Contains(err.Error(), w) {
						t.Fatalf("error lacks %q:\n%v", w, err)
					}
				}
			})
		}
		// Removing or renaming the job is named at the jobs level.
		t.Run(j.id+" removed", func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(validYAML), &doc); err != nil {
				t.Fatal(err)
			}
			removeKey(t, child(doc.Content[0], "jobs"), j.id)
			data, _ := yaml.Marshal(&doc)
			if err := ValidateWorkflow(data); err == nil || !strings.Contains(err.Error(), p+": missing required field") {
				t.Fatalf("removed %s: %v", j.id, err)
			}
		})
		t.Run(j.id+" renamed", func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(validYAML), &doc); err != nil {
				t.Fatal(err)
			}
			js := child(doc.Content[0], "jobs")
			for i := 0; i+1 < len(js.Content); i += 2 {
				if js.Content[i].Value == j.id {
					js.Content[i].Value = j.id + "-old"
				}
			}
			data, _ := yaml.Marshal(&doc)
			err := ValidateWorkflow(data)
			if err == nil || !strings.Contains(err.Error(), p+"-old: unknown field") || !strings.Contains(err.Error(), p+": missing required field") {
				t.Fatalf("renamed %s: %v", j.id, err)
			}
		})
	}
}
