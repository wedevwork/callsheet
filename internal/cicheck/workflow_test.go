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

// validYAML satisfies the ten-job contract (iteration 02c) with job order,
// formatting, key order, step names and comments that differ from the
// checked-in workflow; only semantics are fixed.
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
    runs-on: ubuntu-24.04
    timeout-minutes: 5
    needs:
      - macos-stress-packages
      - macos-stress-processgroup
      - macos-stress-functions
    if: ${{ always() }}
    defaults: {run: {shell: bash}}
    steps:
      - env:
          FUNCTIONS_RESULT: ${{ needs['macos-stress-functions'].result }}
          PACKAGES_RESULT: ${{ needs['macos-stress-packages'].result }}
          PROCESSGROUP_RESULT: ${{ needs['macos-stress-processgroup'].result }}
        run: |
          test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success
  macos-stress-functions:
    name: ci-macos-stress-functions
    runs-on: macos-15
    timeout-minutes: 20
    defaults: {run: {shell: bash}}
    env: {GOFLAGS: -mod=readonly, GOTOOLCHAIN: local}
    steps:
      - with: {persist-credentials: false}
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
      - with: {go-version-file: go.mod, cache: true, cache-dependency-path: go.sum, check-latest: false}
        uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417
      - name: Download (macos-stress-functions)
        run: go mod download
      - name: devcheck stress-functions (macos)
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck stress-functions
  macos-stress-processgroup:
    name: ci-macos-stress-processgroup
    runs-on: macos-15
    timeout-minutes: 20
    defaults: {run: {shell: bash}}
    env: {GOFLAGS: -mod=readonly, GOTOOLCHAIN: local}
    steps:
      - with: {persist-credentials: false}
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
      - with: {go-version-file: go.mod, cache: true, cache-dependency-path: go.sum, check-latest: false}
        uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417
      - name: Download (macos-stress-processgroup)
        run: go mod download
      - name: devcheck stress-processgroup (macos)
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck stress-processgroup
  macos-stress-packages:
    name: ci-macos-stress-packages
    runs-on: macos-15
    timeout-minutes: 20
    defaults: {run: {shell: bash}}
    env: {GOFLAGS: -mod=readonly, GOTOOLCHAIN: local}
    steps:
      - with: {persist-credentials: false}
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
      - with: {go-version-file: go.mod, cache: true, cache-dependency-path: go.sum, check-latest: false}
        uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417
      - name: Download (macos-stress-packages)
        run: go mod download
      - name: devcheck stress-packages (macos)
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck stress-packages
  linux-stress-packages:
    name: ci-linux-stress-packages
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
      - name: Download (linux-stress-packages)
        run: go mod download
      - name: devcheck stress-packages (linux)
        run: go run ./cmd/devcheck stress-packages
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
  linux-stress-processgroup:
    name: ci-linux-stress-processgroup
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
      - name: Download (linux-stress-processgroup)
        run: go mod download
      - name: devcheck stress-processgroup (linux)
        run: go run ./cmd/devcheck stress-processgroup
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
  linux-stress-functions:
    name: ci-linux-stress-functions
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
      - name: Download (linux-stress-functions)
        run: go mod download
      - name: devcheck stress-functions (linux)
        run: go run ./cmd/devcheck stress-functions
        env:
          GOPROXY: "off"
          GOSUMDB: "off"
  linux-stress:
    steps:
      - name: All linux shards succeeded
        run: test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success
        env:
          PACKAGES_RESULT: ${{ needs['linux-stress-packages'].result }}
          PROCESSGROUP_RESULT: ${{ needs['linux-stress-processgroup'].result }}
          FUNCTIONS_RESULT: ${{ needs['linux-stress-functions'].result }}
    defaults:
      run:
        shell: bash
    if: ${{ always() }}
    needs: [linux-stress-packages, linux-stress-processgroup, linux-stress-functions]
    timeout-minutes: 5
    runs-on: ubuntu-24.04
    name: ci-linux-stress
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

// The download and check steps of two stress workers, one block-style on
// Linux and one flow-style on macOS, are written differently from every
// other job's so each can be mutated independently.
const (
	macosWorkerDownload = `      - name: Download (macos-stress-functions)
        run: go mod download
`
	linuxWorkerDownload = `      - name: Download (linux-stress-processgroup)
        run: go mod download
`
	macosWorkerStep = `      - name: devcheck stress-functions (macos)
        env: {GOSUMDB: "off", GOPROXY: "off"}
        run: go run ./cmd/devcheck stress-functions
`
	linuxWorkerStep = `      - name: devcheck stress-processgroup (linux)
        run: go run ./cmd/devcheck stress-processgroup
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

// contractTable is the independent literal ten-job table (design 02c,
// Workflow topology): id|name|runner|timeout|stages|needs|required.
var contractTable = []string{
	"linux|ci-linux|ubuntu-24.04|45|test coverage bench cross||true",
	"macos|ci-macos|macos-15|30|native||true",
	"linux-stress-packages|ci-linux-stress-packages|ubuntu-24.04|20|stress-packages||false",
	"linux-stress-processgroup|ci-linux-stress-processgroup|ubuntu-24.04|20|stress-processgroup||false",
	"linux-stress-functions|ci-linux-stress-functions|ubuntu-24.04|20|stress-functions||false",
	"macos-stress-packages|ci-macos-stress-packages|macos-15|20|stress-packages||false",
	"macos-stress-processgroup|ci-macos-stress-processgroup|macos-15|20|stress-processgroup||false",
	"macos-stress-functions|ci-macos-stress-functions|macos-15|20|stress-functions||false",
	"linux-stress|ci-linux-stress|ubuntu-24.04|5||linux-stress-packages linux-stress-processgroup linux-stress-functions|true",
	"macos-stress|ci-macos-stress|ubuntu-24.04|5||macos-stress-packages macos-stress-processgroup macos-stress-functions|true",
}

func TestContractAccessors(t *testing.T) {
	if got := strings.Join(RequiredChecks(), ","); got != "ci-linux,ci-macos,ci-linux-stress,ci-macos-stress" {
		t.Fatalf("required = %s", got)
	}
	js := Jobs()
	var got []string
	for _, j := range js {
		got = append(got, fmt.Sprintf("%s|%s|%s|%d|%s|%s|%v", j.ID, j.Name, j.RunsOn, j.TimeoutMinutes, strings.Join(j.Stages, " "), strings.Join(j.Needs, " "), j.Required))
		if (len(j.Needs) > 0) == (len(j.Stages) > 0) {
			t.Fatalf("%s must have either stages or needs", j.ID)
		}
	}
	if strings.Join(got, "\n") != strings.Join(contractTable, "\n") {
		t.Fatalf("jobs = %q", got)
	}
	js[0].Stages[0] = "mutated"
	js[1].Stages[0] = "mutated"
	js[4].Stages[0] = "mutated"
	js[8].Needs[0] = "mutated"
	js[9].Needs = js[9].Needs[:1]
	js[1].Name = "mutated"
	js[8].Required = false
	fresh := Jobs()
	if fresh[0].Stages[0] != "test" || fresh[1].Stages[0] != "native" || fresh[4].Stages[0] != "stress-functions" ||
		fresh[8].Needs[0] != "linux-stress-packages" || len(fresh[9].Needs) != 3 || !fresh[8].Required ||
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
	for name, data := range map[string][]byte{"fixture": []byte(validYAML)} {
		got, err := ExtractStages(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 10 {
			t.Fatalf("%s: %d jobs: %v", name, len(got), got)
		}
		for _, j := range Jobs() {
			if s, ok := got[j.ID]; !ok || strings.Join(s, " ") != strings.Join(j.Stages, " ") || s == nil {
				t.Fatalf("%s: %s stages = %#v, want %v", name, j.ID, s, j.Stages)
			}
		}
		if len(got["linux-stress"]) != 0 || len(got["macos-stress"]) != 0 {
			t.Fatalf("summaries have stages: %v", got)
		}
	}
	got, err := ExtractStages([]byte("jobs:\n  a:\n    steps: {}\n  b:\n    steps:\n      - uses: x\n      - run: [x]\n      - run: go run ./cmd/devcheck bogus\n"))
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
	// Worker contexts are diagnostic, not required: the four required
	// contexts alone satisfy the required-check scan.
	four := []byte("jobs:\n  a:\n    name: ci-linux\n  b:\n    name: ci-macos\n  c:\n    name: ci-linux-stress\n  d:\n    name: ci-macos-stress\n")
	if err := CheckJobNames(map[string][]byte{"ci.yml": four}); err != nil {
		t.Fatalf("four required contexts: %v", err)
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
		"stress contexts missing": {map[string][]byte{"ci.yml": []byte("jobs:\n  linux:\n    name: ci-linux\n  macos:\n    name: ci-macos\n  w:\n    name: ci-linux-stress-packages\n")},
			[]string{`required check "ci-linux-stress" is not defined by any workflow`, `required check "ci-macos-stress" is not defined by any workflow`}},
		"duplicate stress context": {map[string][]byte{"ci.yml": ci, "other.yml": []byte("jobs:\n  x:\n    name: ci-macos-stress\n")},
			[]string{`job name "ci-macos-stress" is used by both ci.yml jobs.macos-stress and other.yml jobs.x`}},
		"duplicate worker name": {map[string][]byte{"ci.yml": ci, "other.yml": []byte("jobs:\n  x:\n    name: ci-linux-stress-processgroup\n")},
			[]string{`job name "ci-linux-stress-processgroup" is used by both ci.yml jobs.linux-stress-processgroup and other.yml jobs.x`}},
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

// FP-6 (01c), moved by 02b and sharded by 02c: each worker's shard step is
// required, exact and unconditional as its step 3; each mutation is
// rejected with a path-specific error, and no extra verification step
// (such as a raw go test repeat) is admitted.
func TestStressWorkerStepMutations(t *testing.T) {
	for _, j := range []struct {
		id, stage, other, step, prev string
	}{
		{"linux-stress-processgroup", "stress-processgroup", "stress-packages", linuxWorkerStep, linuxWorkerDownload},
		{"macos-stress-functions", "stress-functions", "stress-processgroup", macosWorkerStep, macosWorkerDownload},
	} {
		p := fmt.Sprintf("jobs.%s.steps[3]", j.id)
		prev := fmt.Sprintf("jobs.%s.steps[2]", j.id)
		cmd := "./cmd/devcheck " + j.stage
		for _, c := range []struct {
			name string
			new  string
			want []string
		}{
			{"removed", "", []string{fmt.Sprintf("jobs.%s.steps: must have exactly 4 steps", j.id), fmt.Sprintf(`jobs.%s.steps: missing check step for devcheck stage %q`, j.id, j.stage)}},
			{"substituted all", strings.Replace(j.step, cmd+"\n", "./cmd/devcheck all\n", 1), []string{fmt.Sprintf(`%s.run: must run devcheck stage %q, got "all"`, p, j.stage), fmt.Sprintf(`missing check step for devcheck stage %q`, j.stage)}},
			{"substituted full stress", strings.Replace(j.step, cmd+"\n", "./cmd/devcheck stress\n", 1), []string{fmt.Sprintf(`%s.run: must run devcheck stage %q, got "stress"`, p, j.stage)}},
			{"substituted other shard", strings.Replace(j.step, cmd+"\n", "./cmd/devcheck "+j.other+"\n", 1), []string{fmt.Sprintf(`%s.run: must run devcheck stage %q, got %q`, p, j.stage, j.other)}},
			{"count flag", strings.Replace(j.step, cmd+"\n", cmd+" -count=1\n", 1), []string{fmt.Sprintf(`%s.run: must be "go run ./cmd/devcheck %s", got "go run ./cmd/devcheck %s -count=1"`, p, j.stage, j.stage)}},
			{"cpu flag", strings.Replace(j.step, cmd+"\n", cmd+" -cpu=1\n", 1), []string{fmt.Sprintf(`%s.run: must be "go run ./cmd/devcheck %s"`, p, j.stage)}},
			{"if", j.step + "        if: always()\n", []string{p + ".if: unknown field"}},
			{"continue-on-error", j.step + "        continue-on-error: true\n", []string{p + ".continue-on-error: unknown field"}},
			{"timeout", j.step + "        timeout-minutes: 5\n", []string{p + ".timeout-minutes: unknown field"}},
			{"offline env omitted", strings.Replace(strings.Replace(j.step, "        env: {GOSUMDB: \"off\", GOPROXY: \"off\"}\n", "", 1), "        env:\n          GOPROXY: \"off\"\n          GOSUMDB: \"off\"\n", "", 1), []string{p + ".env: missing required field"}},
			{"proxy enabled", strings.Replace(j.step, `GOPROXY: "off"`, "GOPROXY: direct", 1), []string{p + `.env.GOPROXY: must be "off", got "direct"`}},
			{"repeat step added", j.step + "      - name: Repeat stress concurrency contract\n        run: go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^TestStressConcurrencyContract$ ./internal/devcheck\n        env: {CGO_ENABLED: \"1\", GOPROXY: \"off\", GOSUMDB: \"off\"}\n",
				[]string{fmt.Sprintf("jobs.%s.steps: must have exactly 4 steps", j.id), fmt.Sprintf("jobs.%s.steps[4]: unexpected extra step", j.id)}},
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
		// Swapping the shard step with the download step before it puts
		// the check where the download belongs and vice versa.
		t.Run(j.id+" reordered with download", func(t *testing.T) {
			err := ValidateWorkflow([]byte(rep(t, validYAML, j.prev+j.step, j.step+j.prev)))
			for _, w := range []string{
				prev + ".env: unknown field",
				fmt.Sprintf(`%s.run: must be "go mod download", got "go run ./cmd/devcheck %s"`, prev, j.stage),
				p + ".env: missing required field",
				fmt.Sprintf(`%s.run: must be "go run ./cmd/devcheck %s", got "go mod download"`, p, j.stage),
				fmt.Sprintf(`jobs.%s.steps: missing check step for devcheck stage %q`, j.id, j.stage),
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

func mapping(kv ...string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Content = append(n.Content, str(kv[i]), str(kv[i+1]))
	}
	return n
}

func seq(vs ...string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode}
	for _, v := range vs {
		n.Content = append(n.Content, str(v))
	}
	return n
}

// mustRejectJobs applies mutate to the fixture and requires every want.
func mustRejectJobs(t *testing.T, mutate func(jobs map[string]*yaml.Node), want []string) {
	t.Helper()
	data := fixtureJobs(t, mutate)
	err := ValidateWorkflow(data)
	if err == nil {
		t.Fatalf("mutation accepted:\n%s", data)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error lacks %q:\n%v", w, err)
		}
	}
}

// TestOrdinaryJobContract (UT-3, iterations 02b and 02c): every main and
// worker job keeps its identity, budget, stages, setup and restrictions;
// each drift is rejected with the job's path.
func TestOrdinaryJobContract(t *testing.T) {
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
		{"linux-stress-packages", "ci-linux-stress-packages", "ubuntu-24.04", 20, 3, []string{"stress-packages"}},
		{"linux-stress-processgroup", "ci-linux-stress-processgroup", "ubuntu-24.04", 20, 3, []string{"stress-processgroup"}},
		{"linux-stress-functions", "ci-linux-stress-functions", "ubuntu-24.04", 20, 3, []string{"stress-functions"}},
		{"macos-stress-packages", "ci-macos-stress-packages", "macos-15", 20, 3, []string{"stress-packages"}},
		{"macos-stress-processgroup", "ci-macos-stress-processgroup", "macos-15", 20, 3, []string{"stress-processgroup"}},
		{"macos-stress-functions", "ci-macos-stress-functions", "macos-15", 20, 3, []string{"stress-functions"}},
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
			{"needs a worker", func(js map[string]*yaml.Node) { addKey(js[j.id], "needs", seq("macos-stress-packages")) }, []string{p + ".needs: unknown field"}},
			{"job condition", func(js map[string]*yaml.Node) { addKey(js[j.id], "if", str("github.event_name == 'push'")) }, []string{p + ".if: unknown field"}},
			{"always condition", func(js map[string]*yaml.Node) { addKey(js[j.id], "if", str("${{ always() }}")) },
				[]string{p + ".if: unknown field", p + `.if: expressions are not allowed: "${{ always() }}"`}},
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
				addKey(at(t, js[j.id], "steps", 2), "env", mapping("GOPROXY", "off"))
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
			{"step expression", func(js map[string]*yaml.Node) {
				at(t, js[j.id], "steps", j.last, "run").Value = "go run ./cmd/devcheck ${{ matrix.stage }}"
			}, []string{last + `.run: expressions are not allowed`}},
			{"last stage", func(js map[string]*yaml.Node) {
				at(t, js[j.id], "steps", j.last, "run").Value = "go run ./cmd/devcheck all"
			},
				[]string{fmt.Sprintf(`%s.run: must run devcheck stage %q, got "all"`, last, j.stages[len(j.stages)-1]),
					fmt.Sprintf(`%s.steps: missing check step for devcheck stage %q`, p, j.stages[len(j.stages)-1])}},
			{"extra stress", func(js map[string]*yaml.Node) {
				steps := at(t, js[j.id], "steps")
				steps.Content = append(steps.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
					str("run"), str("go run ./cmd/devcheck stress"), str("env"), mapping("GOPROXY", "off", "GOSUMDB", "off")}})
			}, []string{fmt.Sprintf("%s.steps: must have exactly %d steps", p, 3+len(j.stages)), fmt.Sprintf("%s.steps[%d]: unexpected extra step", p, j.last+1)}},
		}
		for _, c := range cases {
			t.Run(j.id+" "+c.name, func(t *testing.T) { mustRejectJobs(t, c.mutate, c.want) })
		}
	}
}

// TestJobRemovedOrRenamed: each of the ten jobs removed or renamed is named
// at the jobs level.
func TestJobRemovedOrRenamed(t *testing.T) {
	for _, j := range Jobs() {
		p := "jobs." + j.ID
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(validYAML), &doc); err != nil {
			t.Fatal(err)
		}
		removeKey(t, child(doc.Content[0], "jobs"), j.ID)
		data, _ := yaml.Marshal(&doc)
		if err := ValidateWorkflow(data); err == nil || !strings.Contains(err.Error(), p+": missing required field") {
			t.Fatalf("removed %s: %v", j.ID, err)
		}
		if err := yaml.Unmarshal([]byte(validYAML), &doc); err != nil {
			t.Fatal(err)
		}
		js := child(doc.Content[0], "jobs")
		for i := 0; i+1 < len(js.Content); i += 2 {
			if js.Content[i].Value == j.ID {
				js.Content[i].Value = j.ID + "-old"
			}
		}
		data, _ = yaml.Marshal(&doc)
		err := ValidateWorkflow(data)
		if err == nil || !strings.Contains(err.Error(), p+"-old: unknown field") || !strings.Contains(err.Error(), p+": missing required field") {
			t.Fatalf("renamed %s: %v", j.ID, err)
		}
	}
}

// TestSummaryContract (UT-3, iteration 02c): each summary accepts only the
// exact template for its own platform; every weakening, bypass or extra
// field fails with its path, and expressions stay forbidden everywhere
// except the template's condition and result environment.
func TestSummaryContract(t *testing.T) {
	for _, s := range []struct{ id, name, plat, other string }{
		{"linux-stress", "ci-linux-stress", "linux", "macos"},
		{"macos-stress", "ci-macos-stress", "macos", "linux"},
	} {
		p := "jobs." + s.id
		step := p + ".steps[0]"
		w := func(shard string) string { return s.plat + "-stress-" + shard }
		res := func(id, field string) string { return "${{ needs['" + id + "']." + field + " }}" }
		exact := `test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`
		setRun := func(v string) func(js map[string]*yaml.Node) {
			return func(js map[string]*yaml.Node) { at(t, js[s.id], "steps", 0, "run").Value = v }
		}
		runErr := step + ".run: must be exactly"
		for _, c := range []struct {
			name   string
			mutate func(js map[string]*yaml.Node)
			want   []string
		}{
			{"job emptied", func(js map[string]*yaml.Node) {
				*js[s.id] = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "~"}
			}, []string{p + ": must be a mapping"}},
			{"context renamed", func(js map[string]*yaml.Node) { at(t, js[s.id], "name").Value = s.name + "-summary" },
				[]string{fmt.Sprintf(`%s.name: must be %q, got %q`, p, s.name, s.name+"-summary")}},
			{"runner", func(js map[string]*yaml.Node) { at(t, js[s.id], "runs-on").Value = "macos-15" }, []string{p + `.runs-on: must be "ubuntu-24.04", got "macos-15"`}},
			{"budget", func(js map[string]*yaml.Node) { at(t, js[s.id], "timeout-minutes").Value = "20" }, []string{p + `.timeout-minutes: must be "5", got "20"`}},
			{"needs missing", func(js map[string]*yaml.Node) { removeKey(t, js[s.id], "needs") }, []string{p + ".needs: missing required field"}},
			{"needs extra", func(js map[string]*yaml.Node) {
				at(t, js[s.id], "needs").Content = append(at(t, js[s.id], "needs").Content, str("linux"))
			},
				[]string{p + ".needs: must be exactly [" + w("packages") + ", " + w("processgroup") + ", " + w("functions") + "]"}},
			{"needs short", func(js map[string]*yaml.Node) { n := at(t, js[s.id], "needs"); n.Content = n.Content[:2] }, []string{p + ".needs: must be exactly"}},
			{"needs duplicate", func(js map[string]*yaml.Node) { at(t, js[s.id], "needs", 2).Value = w("packages") },
				[]string{fmt.Sprintf(`%s.needs[2]: must be %q, got %q`, p, w("functions"), w("packages"))}},
			{"needs cross-platform", func(js map[string]*yaml.Node) { at(t, js[s.id], "needs", 0).Value = s.other + "-stress-packages" },
				[]string{fmt.Sprintf(`%s.needs[0]: must be %q, got %q`, p, w("packages"), s.other+"-stress-packages")}},
			{"needs main job", func(js map[string]*yaml.Node) { at(t, js[s.id], "needs", 1).Value = s.plat },
				[]string{fmt.Sprintf(`%s.needs[1]: must be %q, got %q`, p, w("processgroup"), s.plat)}},
			{"needs scalar", func(js map[string]*yaml.Node) { *at(t, js[s.id], "needs") = *str(w("packages")) }, []string{p + ".needs: must be exactly"}},
			{"always removed", func(js map[string]*yaml.Node) { removeKey(t, js[s.id], "if") }, []string{p + ".if: missing required field"}},
			{"success condition", func(js map[string]*yaml.Node) { at(t, js[s.id], "if").Value = "${{ success() }}" },
				[]string{p + `.if: expressions are not allowed: "${{ success() }}"`, p + `.if: must be "${{ always() }}"`}},
			{"predicate on job if", func(js map[string]*yaml.Node) {
				at(t, js[s.id], "if").Value = "${{ always() && needs['" + w("packages") + "'].result == 'success' }}"
			}, []string{p + ".if: expressions are not allowed", p + `.if: must be "${{ always() }}"`}},
			{"bare always", func(js map[string]*yaml.Node) { at(t, js[s.id], "if").Value = "always()" }, []string{p + `.if: must be "${{ always() }}", got "always()"`}},
			{"cancelled condition", func(js map[string]*yaml.Node) { at(t, js[s.id], "if").Value = "${{ !cancelled() }}" }, []string{p + ".if: expressions are not allowed"}},
			{"swapped result", func(js map[string]*yaml.Node) {
				at(t, js[s.id], "steps", 0, "env", "PACKAGES_RESULT").Value = res(w("processgroup"), "result")
			}, []string{step + ".env.PACKAGES_RESULT: expressions are not allowed", step + ".env.PACKAGES_RESULT: must be"}},
			{"outcome not result", func(js map[string]*yaml.Node) {
				at(t, js[s.id], "steps", 0, "env", "FUNCTIONS_RESULT").Value = res(w("functions"), "outcome")
			}, []string{step + ".env.FUNCTIONS_RESULT: expressions are not allowed"}},
			{"cross-platform result", func(js map[string]*yaml.Node) {
				at(t, js[s.id], "steps", 0, "env", "PROCESSGROUP_RESULT").Value = res(s.other+"-stress-processgroup", "result")
			}, []string{step + ".env.PROCESSGROUP_RESULT: expressions are not allowed"}},
			{"literal success", func(js map[string]*yaml.Node) {
				at(t, js[s.id], "steps", 0, "env", "FUNCTIONS_RESULT").Value = "success"
			},
				[]string{step + `.env.FUNCTIONS_RESULT: must be "` + res(w("functions"), "result") + `", got "success"`}},
			{"result env missing", func(js map[string]*yaml.Node) {
				removeKey(t, at(t, js[s.id], "steps", 0, "env"), "PROCESSGROUP_RESULT")
			},
				[]string{step + ".env.PROCESSGROUP_RESULT: missing required field"}},
			{"extra env expression", func(js map[string]*yaml.Node) {
				addKey(at(t, js[s.id], "steps", 0, "env"), "EXTRA", str("${{ github.token }}"))
			}, []string{step + ".env.EXTRA: unknown field", step + ".env.EXTRA: expressions are not allowed"}},
			{"weakened or", setRun(exact + " || true"), []string{runErr}},
			{"weakened comparison", setRun(`test "$PACKAGES_RESULT" != failure && test "$PROCESSGROUP_RESULT" = success && test "$FUNCTIONS_RESULT" = success`), []string{runErr}},
			{"missing comparison", setRun(`test "$PACKAGES_RESULT" = success && test "$PROCESSGROUP_RESULT" = success`), []string{runErr}},
			{"altered operator", setRun(`test "$PACKAGES_RESULT" = success || test "$PROCESSGROUP_RESULT" = success || test "$FUNCTIONS_RESULT" = success`), []string{runErr}},
			{"true", setRun("true"), []string{runErr}},
			{"extra line", setRun(exact + "\nexit 0\n"), []string{runErr}},
			{"two trailing newlines", setRun(exact + "\n\n"), []string{runErr}},
			{"run expression", setRun(`test "${{ needs['` + w("packages") + `'].result }}" = success`), []string{step + ".run: expressions are not allowed", runErr}},
			{"run as list", func(js map[string]*yaml.Node) { *at(t, js[s.id], "steps", 0, "run") = *seq("true") }, []string{runErr}},
			{"extra step", func(js map[string]*yaml.Node) {
				st := at(t, js[s.id], "steps")
				st.Content = append(st.Content, mapping("run", "echo ok"))
			}, []string{p + ".steps: must have exactly 1 step", p + ".steps[1]: unexpected extra step"}},
			{"setup step", func(js map[string]*yaml.Node) {
				st := at(t, js[s.id], "steps")
				st.Content = append([]*yaml.Node{mapping("uses", "actions/checkout@"+strings.Repeat("a", 40))}, st.Content...)
			}, []string{p + ".steps: must have exactly 1 step", step + ".uses: unknown field", step + ".run: missing required field"}},
			{"no steps", func(js map[string]*yaml.Node) { at(t, js[s.id], "steps").Content = nil }, []string{p + ".steps: must have exactly 1 step (the all-success check), got 0"}},
			{"steps mapping", func(js map[string]*yaml.Node) { *at(t, js[s.id], "steps") = *mapping("run", "true") }, []string{p + ".steps: must be a sequence"}},
			{"step not mapping", func(js map[string]*yaml.Node) { at(t, js[s.id], "steps").Content[0] = str("true") }, []string{step + ": must be a mapping"}},
			{"step bypass", func(js map[string]*yaml.Node) { addKey(at(t, js[s.id], "steps", 0), "continue-on-error", str("true")) },
				[]string{step + ".continue-on-error: unknown field"}},
			{"step condition", func(js map[string]*yaml.Node) { addKey(at(t, js[s.id], "steps", 0), "if", str("always()")) }, []string{step + ".if: unknown field"}},
			{"step shell", func(js map[string]*yaml.Node) { addKey(at(t, js[s.id], "steps", 0), "shell", str("sh")) }, []string{step + ".shell: unknown field"}},
			{"step name empty", func(js map[string]*yaml.Node) {
				st := at(t, js[s.id], "steps", 0)
				if n := child(st, "name"); n != nil {
					n.Value = " "
				} else {
					addKey(st, "name", str(" "))
				}
			}, []string{step + ".name: must be a nonempty string"}},
			{"job env", func(js map[string]*yaml.Node) { addKey(js[s.id], "env", mapping("GOTOOLCHAIN", "local")) }, []string{p + ".env: unknown field"}},
			{"job bypass", func(js map[string]*yaml.Node) { addKey(js[s.id], "continue-on-error", str("true")) }, []string{p + ".continue-on-error: unknown field"}},
			{"job matrix", func(js map[string]*yaml.Node) { addKey(js[s.id], "strategy", mapping("fail-fast", "false")) }, []string{p + ".strategy: unknown field"}},
			{"job concurrency expression", func(js map[string]*yaml.Node) { addKey(js[s.id], "concurrency", str("${{ github.ref }}")) },
				[]string{p + ".concurrency: unknown field", p + ".concurrency: expressions are not allowed"}},
			{"shell", func(js map[string]*yaml.Node) { at(t, js[s.id], "defaults", "run", "shell").Value = "sh" }, []string{p + `.defaults.run.shell: must be "bash", got "sh"`}},
			{"defaults omitted", func(js map[string]*yaml.Node) { removeKey(t, js[s.id], "defaults") }, []string{p + ".defaults: missing required field"}},
			{"steps omitted", func(js map[string]*yaml.Node) { removeKey(t, js[s.id], "steps") }, []string{p + ".steps: missing required field"}},
			{"template on a worker", func(js map[string]*yaml.Node) {
				addKey(js[w("packages")], "if", str("${{ always() }}"))
			}, []string{"jobs." + w("packages") + ".if: unknown field", "jobs." + w("packages") + `.if: expressions are not allowed`}},
		} {
			t.Run(s.id+" "+c.name, func(t *testing.T) { mustRejectJobs(t, c.mutate, c.want) })
		}
		// Optional human step names may vary.
		for _, name := range []string{"Require every stress shard", "gate"} {
			data := fixtureJobs(t, func(js map[string]*yaml.Node) {
				st := at(t, js[s.id], "steps", 0)
				if n := child(st, "name"); n != nil {
					n.Value = name
				} else {
					addKey(st, "name", str(name))
				}
			})
			if err := ValidateWorkflow(data); err != nil {
				t.Fatalf("%s step name %q: %v", s.id, name, err)
			}
		}
	}
}
