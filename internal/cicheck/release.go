package cicheck

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// The release workflow contract (design 12-2-release-quickstart, release.md):
// one tag-triggered job with exact identity, permissions, action pins and run
// scripts. It shares this package's YAML parsing and hygiene helpers and
// leaves ValidateWorkflow's accepted CI grammar untouched.

// Pinned action revisions, identical to the CI workflow's.
const (
	CheckoutPin = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	SetupGoPin  = "4b73464bb391d4059bd26b0524d20df3927bd417"
)

const (
	releaseJob     = "release"
	releaseRef     = "${{ github.ref_name }}"
	releaseGroup   = "release-${{ github.ref }}"
	releaseToken   = "${{ github.token }}"
	releaseRepo    = "${{ github.repository }}"
	releaseTagGlob = "v*"
	releaseDir     = `"$RUNNER_TEMP/callsheet-release/`

	// ReleaseBuildScript is the preflight and build step.
	ReleaseBuildScript = `set -euo pipefail
test "$GITHUB_REF_TYPE" = tag
gh --version
create_help="$(gh release create --help)"
edit_help="$(gh release edit --help)"
for capability in --verify-tag --draft --prerelease --latest; do
  grep -q -e "$capability" <<<"$create_help"
  grep -q -e "$capability" <<<"$edit_help"
done
go run ./cmd/releasebuild --version "$RELEASE_TAG" --out "$RUNNER_TEMP/callsheet-release"
`

	// ReleaseSmokeScript is the host version check.
	ReleaseSmokeScript = `set -euo pipefail
output="$(` + releaseDir + `callsheet-linux-amd64" version)"
version_re='^callsheet ([^ ]+) protocol=([0-9]+)$'
[[ "$output" =~ $version_re ]] || { printf 'unexpected version output: %s\n' "$output" >&2; exit 1; }
test "${BASH_REMATCH[1]}" = "$RELEASE_TAG"
`

	// ReleasePublishScript is the fail-fast publication step.
	ReleasePublishScript = `set -euo pipefail
tag_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$'
[[ "$RELEASE_TAG" =~ $tag_re ]] || { printf 'invalid release tag\n' >&2; exit 1; }
prerelease=false
if [[ "$RELEASE_TAG" == *-rc.* ]]; then
  prerelease=true
fi
gh release create "$RELEASE_TAG" --verify-tag --draft --title "$RELEASE_TAG" --notes "Callsheet $RELEASE_TAG. Select the binary for your OS and architecture; verify SHA256SUMS before use." --prerelease="$prerelease" --latest=false ` +
		releaseDir + `callsheet-linux-amd64" ` + releaseDir + `callsheet-linux-arm64" ` + releaseDir + `callsheet-darwin-amd64" ` + releaseDir + `callsheet-darwin-arm64" ` + releaseDir + `SHA256SUMS"
gh release edit "$RELEASE_TAG" --verify-tag --draft=false --prerelease="$prerelease" --latest=false
`
)

// releaseStep is one exact run step after checkout, setup-go.
type releaseStep struct {
	run string
	env map[string]string
}

var releaseRunSteps = []releaseStep{
	{run: "go mod download\n"},
	{run: ReleaseBuildScript, env: map[string]string{"GOPROXY": "off", "GOSUMDB": "off", "RELEASE_TAG": releaseRef}},
	{run: ReleaseSmokeScript, env: map[string]string{"RELEASE_TAG": releaseRef}},
	{run: ReleasePublishScript, env: map[string]string{"GH_TOKEN": releaseToken, "GH_REPO": releaseRepo, "RELEASE_TAG": releaseRef}},
}

// releaseExpressions maps each path where an expression may appear to the
// only value allowed there.
func releaseExpressions() map[string]string {
	out := map[string]string{"jobs." + releaseJob + ".concurrency.group": releaseGroup}
	for i, s := range releaseRunSteps {
		for k, v := range s.env {
			if strings.Contains(v, "${{") {
				out[fmt.Sprintf("jobs.%s.steps[%d].env.%s", releaseJob, i+2, k)] = v
			}
		}
	}
	return out
}

// ValidateReleaseWorkflow checks data against the exact release workflow
// contract, rejecting duplicate keys, anchors, aliases, merge keys, extra
// documents, every field, action, step or expression outside the contract.
func ValidateReleaseWorkflow(data []byte) error {
	root, err := parse(data)
	if err != nil {
		return err
	}
	v := &validator{exprs: releaseExpressions()}
	v.hygiene(root, "")
	v.releaseWorkflow(root)
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

func (v *validator) releaseWorkflow(root *yaml.Node) {
	top := v.fields(root, "", []string{"name", "on", "permissions", "jobs"}, nil)
	if top == nil {
		return
	}
	v.scalar(top["name"], "name", "Release", "")
	if on := top["on"]; on != nil {
		events := v.fields(on, "on", []string{"push"}, nil)
		if n := events["push"]; n != nil {
			f := v.fields(n, "on.push", []string{"tags"}, nil)
			if tags := f["tags"]; tags != nil {
				if tags.Kind != yaml.SequenceNode || len(tags.Content) != 1 {
					v.addf("on.push.tags", "must be exactly [%s]", releaseTagGlob)
				} else {
					v.scalar(tags.Content[0], "on.push.tags[0]", releaseTagGlob, "")
				}
			}
		}
	}
	v.exactMap(top["permissions"], "permissions", map[string]string{"contents": "read"}, nil)
	if js := top["jobs"]; js != nil {
		got := v.fields(js, "jobs", []string{releaseJob}, nil)
		if n := got[releaseJob]; n != nil {
			v.releaseJob(n, "jobs."+releaseJob)
		}
	}
}

func (v *validator) releaseJob(n *yaml.Node, path string) {
	f := v.fields(n, path, []string{"name", "runs-on", "timeout-minutes", "permissions", "concurrency", "defaults", "env", "steps"}, nil)
	if f == nil {
		return
	}
	v.scalar(f["name"], join(path, "name"), releaseJob, "")
	v.scalar(f["runs-on"], join(path, "runs-on"), "ubuntu-24.04", "")
	v.scalar(f["timeout-minutes"], join(path, "timeout-minutes"), "20", "!!int")
	v.exactMap(f["permissions"], join(path, "permissions"), map[string]string{"contents": "write"}, nil)
	v.exactMap(f["concurrency"], join(path, "concurrency"), map[string]string{"group": releaseGroup, "cancel-in-progress": "false"}, map[string]string{"cancel-in-progress": "!!bool"})
	if d := f["defaults"]; d != nil {
		df := v.fields(d, join(path, "defaults"), []string{"run"}, nil)
		v.exactMap(df["run"], join(path, "defaults.run"), map[string]string{"shell": "bash"}, nil)
	}
	v.exactMap(f["env"], join(path, "env"), map[string]string{"GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly"}, nil)
	steps := f["steps"]
	if steps == nil {
		return
	}
	sp := join(path, "steps")
	if steps.Kind != yaml.SequenceNode {
		v.addf(sp, "must be a sequence")
		return
	}
	want := 2 + len(releaseRunSteps)
	if len(steps.Content) != want {
		v.addf(sp, "must have exactly %d steps (checkout, setup-go, go mod download, build, version check, publish), got %d", want, len(steps.Content))
	}
	for i, st := range steps.Content {
		p := index(sp, i)
		switch {
		case i == 0:
			v.releaseAction(st, p, CheckoutAction, CheckoutPin, map[string]string{"persist-credentials": "false"}, map[string]string{"persist-credentials": "!!bool"})
		case i == 1:
			v.releaseAction(st, p, SetupGoAction, SetupGoPin,
				map[string]string{"go-version-file": "go.mod", "cache": "true", "cache-dependency-path": "go.sum", "check-latest": "false"},
				map[string]string{"cache": "!!bool", "check-latest": "!!bool"})
		case i < want:
			v.releaseRun(st, p, releaseRunSteps[i-2])
		default:
			v.addf(p, "unexpected extra step")
		}
	}
}

func (v *validator) releaseAction(n *yaml.Node, path, identity, pin string, with, tags map[string]string) {
	f := v.fields(n, path, []string{"uses", "with"}, []string{"name"})
	if f == nil {
		return
	}
	v.stepName(f, path)
	if u := f["uses"]; u != nil && (u.Kind != yaml.ScalarNode || u.Value != identity+"@"+pin) {
		v.addf(join(path, "uses"), "must be %s@%s, got %q", identity, pin, u.Value)
	}
	v.exactMap(f["with"], join(path, "with"), with, tags)
}

func (v *validator) releaseRun(n *yaml.Node, path string, want releaseStep) {
	required := []string{"run"}
	if want.env != nil {
		required = append(required, "env")
	}
	f := v.fields(n, path, required, []string{"name"})
	if f == nil {
		return
	}
	v.stepName(f, path)
	if want.env != nil {
		v.exactMap(f["env"], join(path, "env"), want.env, nil)
	}
	if run := f["run"]; run != nil {
		// A one-line command may be a plain scalar without the final newline.
		if run.Kind != yaml.ScalarNode || strings.TrimSuffix(run.Value, "\n") != strings.TrimSuffix(want.run, "\n") {
			v.addf(join(path, "run"), "must be the exact script %q, got %q", want.run, run.Value)
		}
	}
}

// ReleaseStepScript returns the run script of the workflow's step at index,
// from the workflow's own text, for execution against fakes.
func ReleaseStepScript(data []byte, step int) (string, error) {
	if err := ValidateReleaseWorkflow(data); err != nil {
		return "", err
	}
	root, err := parse(data)
	if err != nil {
		return "", err
	}
	steps := child(child(child(root, "jobs"), releaseJob), "steps")
	if step < 2 || step >= len(steps.Content) {
		return "", fmt.Errorf("cicheck: release workflow has no run step %d", step)
	}
	return child(steps.Content[step], "run").Value, nil
}

// Release workflow step indexes.
const (
	ReleaseStepBuild   = 3
	ReleaseStepSmoke   = 4
	ReleaseStepPublish = 5
)
