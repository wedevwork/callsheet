package cicheck

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func releaseWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestValidateReleaseWorkflow(t *testing.T) {
	yml := releaseWorkflow(t)
	if err := ValidateReleaseWorkflow([]byte(yml)); err != nil {
		t.Fatalf("shipped workflow: %v", err)
	}
	rep := func(old, repl string) string {
		t.Helper()
		if !strings.Contains(yml, old) {
			t.Fatalf("mutation target %q not found", old)
		}
		return strings.Replace(yml, old, repl, 1)
	}
	for _, c := range []struct {
		name, yml, want string
	}{
		{"empty", "", "empty workflow"},
		{"two-documents", yml + "---\nname: other\n", "extra YAML document"},
		{"malformed", "name: [", "malformed YAML"},
		{"not-a-mapping", "- a\n", "(root): must be a mapping"},
		{"duplicate-key", rep("name: Release\n", "name: Release\nname: Release\n"), "duplicate key"},
		{"anchor", rep("timeout-minutes: 20", "timeout-minutes: &t 20"), "anchors are not allowed"},
		{"alias", rep("contents: write", "contents: &w write\n      actions: *w"), "aliases are not allowed"},
		{"merge-key", rep("timeout-minutes: 20", "<<: {}\n    timeout-minutes: 20"), "merge keys are not allowed"},
		{"unknown-top", rep("name: Release\n", "name: Release\nenv: {}\n"), "env: unknown field"},
		{"unknown-job-field", rep("timeout-minutes: 20", "timeout-minutes: 20\n    continue-on-error: true"), "continue-on-error: unknown field"},
		{"unknown-step-field", rep("- name: Download modules\n", "- name: Download modules\n        continue-on-error: true\n"), "continue-on-error: unknown field"},
		{"step-if", rep("- name: Publish the release\n", "- name: Publish the release\n        if: always()\n"), "if: unknown field"},
		{"name", rep("name: Release\n", "name: Publish\n"), `name: must be "Release"`},
		{"branch-trigger", rep("tags: ['v*']", "branches: [main]"), "on.push.branches: unknown field"},
		{"extra-tags-filter", rep("tags: ['v*']", "tags: ['v*', 'x*']"), "must be exactly [v*]"},
		{"wrong-glob", rep("tags: ['v*']", "tags: ['*']"), `on.push.tags[0]: must be "v*"`},
		{"pull-request", rep("    tags: ['v*']\n", "    tags: ['v*']\n  pull_request: {}\n"), "on.pull_request: unknown field"},
		{"dispatch", rep("    tags: ['v*']\n", "    tags: ['v*']\n  workflow_dispatch: {}\n"), "on.workflow_dispatch: unknown field"},
		{"write-all-top", rep("permissions:\n  contents: read", "permissions: write-all"), "permissions: must be a mapping"},
		{"write-top", rep("permissions:\n  contents: read", "permissions:\n  contents: write"), `permissions.contents: must be "read"`},
		{"write-all-job", rep("permissions:\n      contents: write", "permissions: write-all"), "jobs.release.permissions: must be a mapping"},
		{"extra-grant", rep("contents: write", "contents: write\n      id-token: write"), "id-token: unknown field"},
		{"read-job", rep("contents: write", "contents: read"), `jobs.release.permissions.contents: must be "write"`},
		{"runner", rep("runs-on: ubuntu-24.04", "runs-on: ubuntu-latest"), `runs-on: must be "ubuntu-24.04"`},
		{"timeout", rep("timeout-minutes: 20", "timeout-minutes: 45"), `timeout-minutes: must be "20"`},
		{"timeout-string", rep("timeout-minutes: 20", `timeout-minutes: "20"`), `timeout-minutes: must be "20"`},
		{"shell", rep("shell: bash", "shell: sh"), `shell: must be "bash"`},
		{"cancel", rep("cancel-in-progress: false", "cancel-in-progress: true"), `cancel-in-progress: must be "false"`},
		{"group", rep("group: release-${{ github.ref }}", "group: release"), "group: must be"},
		{"group-expression", rep("group: release-${{ github.ref }}", "group: release-${{ github.sha }}"), "expressions are not allowed"},
		{"job-env", rep("GOFLAGS: -mod=readonly", "GOFLAGS: -mod=mod"), `GOFLAGS: must be "-mod=readonly"`},
		{"second-job", rep("jobs:\n", "jobs:\n  other:\n    runs-on: ubuntu-24.04\n"), "jobs.other: unknown field"},
		{"floating-checkout", rep("actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd", "actions/checkout@v6"), "steps[0].uses: must be actions/checkout@" + CheckoutPin},
		{"other-sha", rep("actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417", "actions/setup-go@"+CheckoutPin), "steps[1].uses: must be actions/setup-go@" + SetupGoPin},
		{"wrong-action", rep("actions/checkout@", "evil/checkout@"), "steps[0].uses: must be actions/checkout@"},
		{"persist", rep("persist-credentials: false", "persist-credentials: true"), `persist-credentials: must be "false"`},
		{"checkout-ref", rep("persist-credentials: false", "persist-credentials: false\n          ref: main"), "ref: unknown field"},
		{"no-download", rep("go mod download", "true"), "steps[2].run: must be the exact script"},
		{"missing-verify-tag", rep(`"$RELEASE_TAG" --verify-tag --draft --title`, `"$RELEASE_TAG" --draft --title`), "steps[5].run: must be the exact script"},
		{"missing-draft", rep("--verify-tag --draft --title", "--verify-tag --title"), "steps[5].run: must be the exact script"},
		{"early-publish", rep("--verify-tag --draft --title", "--verify-tag --title"), "steps[5].run"},
		{"wildcard-asset", rep(`"$RUNNER_TEMP/callsheet-release/SHA256SUMS"`, `"$RUNNER_TEMP/callsheet-release/"*`), "steps[5].run"},
		{"extra-asset", rep(`"$RUNNER_TEMP/callsheet-release/SHA256SUMS"`, `"$RUNNER_TEMP/callsheet-release/SHA256SUMS" "$RUNNER_TEMP/extra"`), "steps[5].run"},
		{"latest", rep("--draft=false --prerelease=\"$prerelease\" --latest=false", "--draft=false --prerelease=\"$prerelease\" --latest=true"), "steps[5].run"},
		{"unsafe-interpolation", rep(`gh release edit "$RELEASE_TAG"`, "gh release edit ${{ github.ref_name }}"), "expressions are not allowed"},
		{"token-in-build", rep("          GOPROXY: \"off\"\n", "          GOPROXY: \"off\"\n          GH_TOKEN: ${{ github.token }}\n"), "expressions are not allowed"},
		{"token-from-secret", rep("GH_TOKEN: ${{ github.token }}", "GH_TOKEN: ${{ secrets.PAT }}"), "expressions are not allowed"},
		{"tag-from-context", rep("RELEASE_TAG: ${{ github.ref_name }}", "RELEASE_TAG: ${{ github.head_ref }}"), "expressions are not allowed"},
		{"no-preflight", rep("gh --version\n", ""), "steps[3].run"},
		{"no-tag-check", rep("test \"$GITHUB_REF_TYPE\" = tag\n", ""), "steps[3].run"},
		{"no-build", rep(`go run ./cmd/releasebuild --version "$RELEASE_TAG" --out "$RUNNER_TEMP/callsheet-release"`, "true"), "steps[3].run"},
		{"network-build", rep("          GOPROXY: \"off\"\n", ""), "GOPROXY: missing required field"},
		{"no-smoke", rep("test \"${BASH_REMATCH[1]}\" = \"$RELEASE_TAG\"", "true"), "steps[4].run"},
		{"extra-step", yml + "      - name: Extra\n        run: \"true\"\n", "must have exactly 6 steps"},
		{"release-action", rep("- name: Publish the release\n        env:", "- name: Publish the release\n        uses: softprops/action-gh-release@v2\n        env:"), "uses: unknown field"},
		{"steps-scalar", rep("    steps:\n", "    steps: none\n    x-steps:\n"), "x-steps: unknown field"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateReleaseWorkflow([]byte(c.yml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestReleaseWorkflowLeavesCIContract: the release grammar never widens the
// CI validator: the release workflow is not a CI workflow, and the job name
// does not collide in the directory-wide check.
func TestReleaseWorkflowLeavesCIContract(t *testing.T) {
	yml := []byte(releaseWorkflow(t))
	if err := ValidateWorkflow(yml); err == nil {
		t.Fatal("ValidateWorkflow accepted the release workflow")
	}
	ci, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkflow(ci); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReleaseWorkflow(ci); err == nil {
		t.Fatal("ValidateReleaseWorkflow accepted the CI workflow")
	}
	if err := CheckJobNames(map[string][]byte{"ci.yml": ci, "release.yml": yml}); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{"actions/checkout@" + CheckoutPin, "actions/setup-go@" + SetupGoPin} {
		if !strings.Contains(string(ci), pin) {
			t.Fatalf("ci.yml does not use %s", pin)
		}
	}
}

func TestReleaseStepScript(t *testing.T) {
	yml := []byte(releaseWorkflow(t))
	for step, want := range map[int]string{ReleaseStepBuild: ReleaseBuildScript, ReleaseStepSmoke: ReleaseSmokeScript, ReleaseStepPublish: ReleasePublishScript} {
		if got, err := ReleaseStepScript(yml, step); err != nil || got != want {
			t.Fatalf("step %d = %q, %v", step, got, err)
		}
	}
	for _, step := range []int{-1, 0, 1, 6} {
		if _, err := ReleaseStepScript(yml, step); err == nil {
			t.Errorf("step %d extracted", step)
		}
	}
	if _, err := ReleaseStepScript([]byte("name: x\n"), ReleaseStepPublish); err == nil {
		t.Error("an invalid workflow yielded a script")
	}
}

// fakeTools writes executables (name to /bin/sh body) into a new directory.
func fakeTools(t *testing.T, tools map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeGH records one argv line per call and fails on the selected call.
const fakeGH = `n=$(($(cat "$GH_LOG.count" 2>/dev/null || echo 0)+1))
echo "$n" > "$GH_LOG.count"
printf '%s\n' "$*" >> "$GH_LOG"
if [ "$n" = "$GH_FAIL_AT" ]; then
  echo "gh failed at call $n" >&2
  exit 7
fi
`

func runRelease(t *testing.T, script, tools string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append([]string{"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	}
	t.Fatal(err)
	return "", 0
}

func readLog(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestReleasePublishScriptExecution(t *testing.T) {
	script, err := ReleaseStepScript([]byte(releaseWorkflow(t)), ReleaseStepPublish)
	if err != nil {
		t.Fatal(err)
	}
	tools := fakeTools(t, map[string]string{"gh": fakeGH})
	assets := func(temp string) string {
		var out []string
		for _, a := range []string{"callsheet-linux-amd64", "callsheet-linux-arm64", "callsheet-darwin-amd64", "callsheet-darwin-arm64", "SHA256SUMS"} {
			out = append(out, temp+"/callsheet-release/"+a)
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		name, tag  string
		failAt     int
		code       int
		calls      int
		prerelease string
	}{
		{"stable", "v1.0.0", 0, 0, 2, "false"},
		{"rc", "v1.0.0-rc.1", 0, 0, 2, "true"},
		{"create-fails-no-publish", "v1.0.0", 1, 7, 1, "false"},
		{"edit-fails", "v1.0.0-rc.2", 2, 7, 2, "true"},
		{"invalid-tag", "v1.0.0+meta", 0, 1, 0, ""},
		{"newline-tag", "v1.0.0\nv2.0.0", 0, 1, 0, ""},
		{"empty-tag", "", 0, 1, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			log, temp := filepath.Join(t.TempDir(), "gh.log"), t.TempDir()
			out, code := runRelease(t, script, tools, "GH_LOG="+log, "GH_FAIL_AT="+strconv.Itoa(c.failAt), "RELEASE_TAG="+c.tag, "RUNNER_TEMP="+temp)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, out)
			}
			calls := readLog(t, log)
			if len(calls) != c.calls {
				t.Fatalf("calls = %q, want %d", calls, c.calls)
			}
			if c.calls >= 1 {
				want := "release create " + c.tag + " --verify-tag --draft --title " + c.tag + " --notes Callsheet " + c.tag +
					". Select the binary for your OS and architecture; verify SHA256SUMS before use. --prerelease=" + c.prerelease + " --latest=false " + assets(temp)
				if calls[0] != want {
					t.Errorf("create = %q\nwant     %q", calls[0], want)
				}
			}
			if c.calls == 2 {
				if want := "release edit " + c.tag + " --verify-tag --draft=false --prerelease=" + c.prerelease + " --latest=false"; calls[1] != want {
					t.Errorf("edit = %q, want %q", calls[1], want)
				}
			}
		})
	}
}

func TestReleaseBuildScriptExecution(t *testing.T) {
	script, err := ReleaseStepScript([]byte(releaseWorkflow(t)), ReleaseStepBuild)
	if err != nil {
		t.Fatal(err)
	}
	const fullHelp = "--verify-tag --draft --prerelease --latest"
	gh := func(help string) string {
		return `printf '%s\n' "gh $*" >> "$LOG"
case "$*" in
  "--version") echo "gh version 0" ;;
  *) echo "` + help + `" ;;
esac
`
	}
	const goTool = `printf '%s\n' "go $*" >> "$LOG"
`
	for _, c := range []struct {
		name, refType, help string
		code                int
		built               bool
	}{
		{"tag", "tag", fullHelp, 0, true},
		{"branch-ref", "branch", fullHelp, 1, false},
		{"missing-capability", "tag", "--verify-tag --draft --prerelease", 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tools := fakeTools(t, map[string]string{"gh": gh(c.help), "go": goTool})
			log, temp := filepath.Join(t.TempDir(), "calls.log"), t.TempDir()
			out, code := runRelease(t, script, tools, "LOG="+log, "GITHUB_REF_TYPE="+c.refType, "RELEASE_TAG=v1.0.0", "RUNNER_TEMP="+temp)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, out)
			}
			calls := readLog(t, log)
			want := "go run ./cmd/releasebuild --version v1.0.0 --out " + temp + "/callsheet-release"
			if got := len(calls) > 0 && calls[len(calls)-1] == want; got != c.built {
				t.Fatalf("built = %v, calls %q", got, calls)
			}
		})
	}
}

func TestReleaseSmokeScriptExecution(t *testing.T) {
	script, err := ReleaseStepScript([]byte(releaseWorkflow(t)), ReleaseStepSmoke)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, output string
		code         int
	}{
		{"exact", "callsheet v1.0.0 protocol=6", 0},
		{"wrong-version", "callsheet v1.0.1 protocol=6", 1},
		{"substring-version", "callsheet v1.0.0-rc.1 protocol=6", 1},
		{"dev", "callsheet dev protocol=6", 1},
		{"no-protocol", "callsheet v1.0.0 protocol=", 1},
		{"text-protocol", "callsheet v1.0.0 protocol=x", 1},
		{"extra-line", "callsheet v1.0.0 protocol=6\nmore", 1},
		{"prefix", "x callsheet v1.0.0 protocol=6", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			temp := t.TempDir()
			dir := filepath.Join(temp, "callsheet-release")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := "#!/bin/sh\nprintf '%s\\n' '" + c.output + "'\n"
			if err := os.WriteFile(filepath.Join(dir, "callsheet-linux-amd64"), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			if out, code := runRelease(t, script, "", "RELEASE_TAG=v1.0.0", "RUNNER_TEMP="+temp); code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, out)
			}
		})
	}
}
