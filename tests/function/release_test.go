package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/cicheck"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 12-2 function tests (design 12-2-release-quickstart, release.md):
// one top-level test each for the build, the workflow and the manifest. They
// never publish, tag or contact a network; the real four-target trimmed build
// is the separate opt-in TestReleaseTrimmedProducts.

const releaseFixtureVersion = "v0.0.0-rc.1"

// releaseDeps are injected dependencies for header-only products.
func releaseDeps(stamp string) devcheck.ReleaseDeps {
	return devcheck.ReleaseDeps{
		Run: func(_ context.Context, argv, env []string, _ string, _, _ io.Writer) error {
			_, err := testkit.FakeGoBuild(argv, env)
			return err
		},
		Stamp: func(string) (string, error) { return stamp, nil },
		Settings: func(p string) (string, map[string]string, error) {
			tg, err := devcheck.InspectExecutable(p)
			return "go-fixture", map[string]string{"GOOS": tg.GOOS, "GOARCH": tg.GOARCH, "CGO_ENABLED": "0", "-trimpath": "true"}, err
		},
	}
}

// releaseHostBinary is the single stamped untrimmed host build of the test
// process: ordinary compile-cache reuse, never -trimpath in this package.
func releaseHostBinary(t *testing.T) string {
	return fixture(t, "release-host", func(dir string) (string, error) {
		out := filepath.Join(dir, "callsheet-stamped")
		root, err := testkit.RepoRoot()
		if err != nil {
			return "", err
		}
		var stderr bytes.Buffer
		env := devcheck.MergeEnv(os.Environ(), []string{"CGO_ENABLED=0", "GOOS=" + runtime.GOOS, "GOARCH=" + runtime.GOARCH})
		argv := []string{"go", "build", "-buildvcs=false", "-ldflags", "-X " + devcheck.ReleaseVersionSymbol + "=" + releaseFixtureVersion, "-o", out, "./cmd/callsheet"}
		if err := devcheck.ExecRunner(context.Background(), argv, env, root, io.Discard, &stderr); err != nil {
			return "", fmt.Errorf("%v: %w: %s", argv, err, stderr.String())
		}
		return out, nil
	})
}

// TestReleaseArtifacts (FP-1): the four-target production plan through injected
// readers, then the one real host product for the real readers and execution.
func TestReleaseArtifacts(t *testing.T) {
	t.Run("production-plan", func(t *testing.T) {
		dir := t.TempDir()
		steps, err := devcheck.ReleasePlan(releaseFixtureVersion, dir, devcheck.Matrix)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 4 {
			t.Fatalf("%d steps", len(steps))
		}
		for i, s := range steps {
			tg := devcheck.Matrix[i]
			want := []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-X github.com/wedevwork/callsheet/internal/cli.Version=" + releaseFixtureVersion,
				"-o", filepath.Join(dir, "callsheet-"+tg.GOOS+"-"+tg.GOARCH), "./cmd/callsheet"}
			if !slices.Equal(s.Argv, want) {
				t.Errorf("argv = %q", s.Argv)
			}
			if !slices.Equal(s.Env, []string{"CGO_ENABLED=0", "GOOS=" + tg.GOOS, "GOARCH=" + tg.GOARCH}) {
				t.Errorf("env = %q", s.Env)
			}
			joined := " " + strings.Join(s.Argv, " ") + " "
			for _, strip := range []string{" -s ", " -w ", "-s -w"} {
				if strings.Contains(joined, strip) {
					t.Errorf("plan strips symbols: %q", s.Argv)
				}
			}
		}
		// The cross stage's twelve-artifact contract is untouched.
		cross, err := devcheck.CrossPlan(dir, devcheck.Matrix)
		if err != nil || len(cross) != 12 {
			t.Fatalf("cross plan = %d, %v", len(cross), err)
		}
	})
	t.Run("injected", func(t *testing.T) {
		good := func(t *testing.T) string {
			t.Helper()
			dir := t.TempDir()
			if err := devcheck.BuildRelease(context.Background(), releaseDeps(releaseFixtureVersion), releaseFixtureVersion, dir, devcheck.Matrix, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, devcheck.ReleaseManifest)); err != nil {
				t.Fatal(err)
			}
			return dir
		}
		if err := devcheck.VerifyReleaseProducts(releaseDeps(releaseFixtureVersion), releaseFixtureVersion, good(t), devcheck.Matrix, io.Discard); err != nil {
			t.Fatalf("absent -ldflags must be accepted: %v", err)
		}
		synthetic := func(t *testing.T, path string, tg devcheck.Target) {
			t.Helper()
			b, err := testkit.SyntheticExecutable(tg.GOOS, tg.GOARCH)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range []struct {
			name string
			mut  func(t *testing.T, dir string, d *devcheck.ReleaseDeps)
			want string
		}{
			{"wrong-target", func(t *testing.T, dir string, _ *devcheck.ReleaseDeps) {
				synthetic(t, filepath.Join(dir, "callsheet-darwin-arm64"), devcheck.Target{GOOS: "linux", GOARCH: "arm64"})
			}, "verify darwin/arm64: header: built for linux/arm64"},
			{"wrong-stamp", func(_ *testing.T, _ string, d *devcheck.ReleaseDeps) {
				d.Stamp = func(string) (string, error) { return "v0.0.0-rc.2", nil }
			}, `stored version "v0.0.0-rc.2"`},
			{"unstamped", func(_ *testing.T, _ string, d *devcheck.ReleaseDeps) {
				d.Stamp = func(string) (string, error) { return "dev", nil }
			}, `stored version "dev"`},
			{"missing-symbol", func(_ *testing.T, _ string, d *devcheck.ReleaseDeps) {
				d.Stamp = func(string) (string, error) { return "", errors.New("symbol is absent") }
			}, "stamp: symbol is absent"},
			{"missing-trimpath", func(_ *testing.T, _ string, d *devcheck.ReleaseDeps) {
				d.Settings = func(string) (string, map[string]string, error) {
					return "go", map[string]string{"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}, nil
				}
			}, "setting -trimpath is absent"},
			{"cgo-enabled", func(_ *testing.T, _ string, d *devcheck.ReleaseDeps) {
				d.Settings = func(string) (string, map[string]string, error) {
					return "go", map[string]string{"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1", "-trimpath": "true"}, nil
				}
			}, `setting CGO_ENABLED = "1"`},
			{"extra-file", func(t *testing.T, dir string, _ *devcheck.ReleaseDeps) {
				synthetic(t, filepath.Join(dir, "fake-adapter-linux-amd64"), devcheck.Target{GOOS: "linux", GOARCH: "amd64"})
			}, "unexpected entry fake-adapter-linux-amd64"},
			{"extra-product", func(t *testing.T, dir string, _ *devcheck.ReleaseDeps) {
				synthetic(t, filepath.Join(dir, "callsheet-linux-386"), devcheck.Target{GOOS: "linux", GOARCH: "amd64"})
			}, "unexpected entry callsheet-linux-386"},
			{"missing-product", func(t *testing.T, dir string, _ *devcheck.ReleaseDeps) {
				if err := os.Remove(filepath.Join(dir, "callsheet-linux-arm64")); err != nil {
					t.Fatal(err)
				}
			}, "verify linux/arm64: file: missing"},
		} {
			t.Run(c.name, func(t *testing.T) {
				dir, deps := good(t), releaseDeps(releaseFixtureVersion)
				c.mut(t, dir, &deps)
				err := devcheck.VerifyReleaseAndWriteManifest(deps, releaseFixtureVersion, dir, devcheck.Matrix, io.Discard)
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want %q", err, c.want)
				}
				if _, err := os.Stat(filepath.Join(dir, devcheck.ReleaseManifest)); err == nil {
					t.Fatal("a manifest was written")
				}
			})
		}
	})
	t.Run("real-host-product", func(t *testing.T) {
		p := releaseHostBinary(t)
		host := devcheck.Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
		if got, err := devcheck.InspectExecutable(p); err != nil || got != host {
			t.Fatalf("header = %v, %v", got, err)
		}
		stamp, err := devcheck.ReadReleaseStamp(p)
		if err != nil || stamp != releaseFixtureVersion {
			t.Fatalf("stamp = %q, %v", stamp, err)
		}
		goVersion, settings, err := devcheck.ReadBuildSettings(p)
		if err != nil {
			t.Fatal(err)
		}
		if settings["GOOS"] != host.GOOS || settings["GOARCH"] != host.GOARCH || settings["CGO_ENABLED"] != "0" {
			t.Fatalf("settings = %v", settings)
		}
		if v, ok := settings["-trimpath"]; ok && v == "true" {
			t.Fatalf("the host fixture is trimmed: %v", settings)
		}
		// This untrimmed fixture never passes the production trimpath policy.
		if err := devcheck.CheckReleaseSettings(host, settings); err == nil || !strings.Contains(err.Error(), "-trimpath") {
			t.Fatalf("policy = %v", err)
		}
		t.Logf("go %s, stamp %s", goVersion, stamp)
		home := t.TempDir()
		res := runBin(t, p, home, []string{"HOME=" + home}, "version")
		if want := "callsheet " + releaseFixtureVersion + " protocol=" + strconv.Itoa(contract.ProtocolVersion) + "\n"; res.code != 0 || res.stdout != want {
			t.Fatalf("version = %+v, want %q", res, want)
		}
		// The native output and the read value agree; an ordinary build stays dev.
		if !strings.Contains(res.stdout, " "+stamp+" ") {
			t.Fatalf("read %q, executed %q", stamp, res.stdout)
		}
		if res := runBin(t, nodeBinary(t), home, []string{"HOME=" + home}, "version"); !strings.HasPrefix(res.stdout, "callsheet dev protocol=") {
			t.Fatalf("ordinary build = %+v", res)
		}
	})
}

// checkReleaseManifest validates a manifest against independently hashed
// product bytes: exact order, names and digests, nothing else.
func checkReleaseManifest(manifest string, products map[string][]byte) error {
	names := make([]string, 0, len(products))
	for name := range products {
		names = append(names, name)
	}
	slices.Sort(names)
	if !strings.HasSuffix(manifest, "\n") {
		return errors.New("manifest: no final newline")
	}
	lines := strings.Split(strings.TrimSuffix(manifest, "\n"), "\n")
	if len(lines) != len(names) {
		return fmt.Errorf("manifest: %d lines, want %d", len(lines), len(names))
	}
	re := regexp.MustCompile(`^([0-9a-f]{64})  ([^ /]+)$`)
	for i, l := range lines {
		m := re.FindStringSubmatch(l)
		if m == nil {
			return fmt.Errorf("manifest: line %d is malformed: %q", i+1, l)
		}
		if m[2] != names[i] {
			return fmt.Errorf("manifest: line %d names %s, want %s", i+1, m[2], names[i])
		}
		sum := sha256.Sum256(products[names[i]])
		if m[1] != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("manifest: wrong digest for %s", names[i])
		}
	}
	return nil
}

// TestReleaseChecksums (FP-8): the manifest over injected product bytes, hashed
// independently, with each mutation rejected and none written on a failure.
// No real build.
func TestReleaseChecksums(t *testing.T) {
	deps := releaseDeps(releaseFixtureVersion)
	products := map[string][]byte{}
	inner := deps.Run
	deps.Run = func(ctx context.Context, argv, env []string, dir string, o, e io.Writer) error {
		if err := inner(ctx, argv, env, dir, o, e); err != nil {
			return err
		}
		// Distinct bytes per product, so digests and names cannot be swapped.
		out := argv[slices.Index(argv, "-o")+1]
		f, err := os.OpenFile(out, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.WriteString(filepath.Base(out)); err != nil {
			return err
		}
		return nil
	}
	dir := t.TempDir()
	if err := devcheck.BuildRelease(context.Background(), deps, releaseFixtureVersion, dir, devcheck.Matrix, io.Discard); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if e.Name() != devcheck.ReleaseManifest {
			if products[e.Name()], err = os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Equal(names, []string{"SHA256SUMS", "callsheet-darwin-amd64", "callsheet-darwin-arm64", "callsheet-linux-amd64", "callsheet-linux-arm64"}) {
		t.Fatalf("entries = %q", names)
	}
	manifest := string(nodeFile(t, filepath.Join(dir, devcheck.ReleaseManifest)))
	if err := checkReleaseManifest(manifest, products); err != nil {
		t.Fatalf("manifest: %v\n%s", err, manifest)
	}
	lines := strings.Split(strings.TrimSuffix(manifest, "\n"), "\n")
	for _, m := range []struct{ name, manifest, want string }{
		{"wrong-digest", strings.Replace(manifest, lines[0][:8], "00000000", 1), "wrong digest"},
		{"wrong-order", lines[1] + "\n" + lines[0] + "\n" + lines[2] + "\n" + lines[3] + "\n", "line 1 names"},
		{"wrong-name", strings.Replace(manifest, "callsheet-linux-amd64", "callsheet-linux-x86", 1), "names callsheet-linux-x86"},
		{"missing-entry", lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n", "3 lines, want 4"},
		{"duplicate-entry", manifest + lines[3] + "\n", "5 lines, want 4"},
		{"absolute-path", strings.Replace(manifest, "  callsheet-linux-arm64", "  "+dir+"/callsheet-linux-arm64", 1), "malformed"},
		{"one-space", strings.Replace(manifest, "  callsheet-darwin-arm64", " callsheet-darwin-arm64", 1), "malformed"},
		{"uppercase-digest", strings.ToUpper(lines[0][:64]) + lines[0][64:] + "\n" + strings.Join(lines[1:], "\n") + "\n", "malformed"},
		{"no-final-newline", strings.TrimSuffix(manifest, "\n"), "no final newline"},
		{"self-hash", manifest + strings.Repeat("0", 64) + "  SHA256SUMS\n", "5 lines, want 4"},
	} {
		t.Run(m.name, func(t *testing.T) {
			err := checkReleaseManifest(m.manifest, products)
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("err = %v, want %q", err, m.want)
			}
			t.Logf("rejected: %v", firstLine(err))
		})
	}
	// Deterministic for the given bytes.
	again := t.TempDir()
	for name, b := range products {
		if err := os.WriteFile(filepath.Join(again, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := devcheck.WriteReleaseManifest(again, devcheck.Matrix); err != nil {
		t.Fatal(err)
	}
	if got := string(nodeFile(t, filepath.Join(again, devcheck.ReleaseManifest))); got != manifest {
		t.Fatalf("second manifest differs:\n%s", got)
	}
	// An extra file or a build failure leaves no manifest at all.
	extra := t.TempDir()
	for name, b := range products {
		if err := os.WriteFile(filepath.Join(extra, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(extra, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := devcheck.VerifyReleaseAndWriteManifest(releaseDeps(releaseFixtureVersion), releaseFixtureVersion, extra, devcheck.Matrix, io.Discard); err == nil || !strings.Contains(err.Error(), "unexpected entry notes.txt") {
		t.Fatalf("extra file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extra, devcheck.ReleaseManifest)); err == nil {
		t.Fatal("manifest beside an extra file")
	}
	failing := releaseDeps(releaseFixtureVersion)
	calls := 0
	buildInner := failing.Run
	failing.Run = func(ctx context.Context, argv, env []string, d string, o, e io.Writer) error {
		if calls++; calls == 4 {
			return errors.New("the last build failed")
		}
		return buildInner(ctx, argv, env, d, o, e)
	}
	partial := t.TempDir()
	if err := devcheck.BuildRelease(context.Background(), failing, releaseFixtureVersion, partial, devcheck.Matrix, io.Discard); err == nil || !strings.Contains(err.Error(), "the last build failed") {
		t.Fatalf("build failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(partial, devcheck.ReleaseManifest)); err == nil {
		t.Fatal("manifest on a build failure")
	}
	if n := len(listTree(t, partial)); n != 3 {
		t.Fatalf("partial output = %d entries", n)
	}
}

// checkReleaseOperations pins the appended maintainer section's offline facts.
func checkReleaseOperations(doc string) error {
	h := "\n## Release operations\n"
	i := strings.Index(doc, h)
	if i < 0 {
		return errors.New("release operations: no section")
	}
	if j := strings.Index(doc, "\n## Local verification\n"); j < 0 || j > i {
		return errors.New("release operations: not after Local verification")
	}
	body := doc[i+len(h):]
	if strings.Contains(body, "\n## ") {
		return errors.New("release operations: not the final H2")
	}
	for _, tok := range []string{"ci-windows", "-X PUT", "--method PUT", "-X POST", "--method POST", "-X PATCH", "--method PATCH", "-X DELETE", "--method DELETE", "curl "} {
		if strings.Contains(body, tok) {
			return fmt.Errorf("release operations: forbidden token %q", tok)
		}
	}
	if err := checkPrivateText("release operations", body); err != nil {
		return err
	}
	wants := []string{"`vMAJOR.MINOR.PATCH` or `vMAJOR.MINOR.PATCH-rc.N`", "go run ./cmd/releasebuild --version v0.0.0-rc.1 --out <fresh-temporary-directory>",
		"`contents: read` at the top level and `contents: write` on the single `release` job", "--verify-tag", "--latest=false", "never modifies a published release",
		"Inspect and remove an incomplete draft", "Tag creation is an owner action", "go test -tags=releaseacceptance -run '^TestReleaseTrimmedProducts$' -count=1 ./internal/devcheck",
		"record the observed Go version", "They cannot establish hosted token permission"}
	for _, a := range devcheck.ReleaseAssets(devcheck.Matrix) {
		wants = append(wants, "`"+a+"`")
	}
	return needAll("release operations", body, wants...)
}

// TestReleaseWorkflow (FP-2): the shipped workflow through its exact validator,
// its pins against CI's, and its own publication step against a fake gh.
func TestReleaseWorkflow(t *testing.T) {
	yml := string(repoFile(t, ".github/workflows/release.yml"))
	ci := ciWorkflow(t)
	if err := cicheck.ValidateReleaseWorkflow([]byte(yml)); err != nil {
		t.Fatalf("shipped workflow: %v", err)
	}
	if err := cicheck.ValidateWorkflow(ci); err != nil {
		t.Fatalf("ci.yml: %v", err)
	}
	if err := cicheck.ValidateWorkflow([]byte(yml)); err == nil {
		t.Fatal("the CI validator accepted the release workflow")
	}
	if err := cicheck.CheckJobNames(map[string][]byte{"ci.yml": ci, "release.yml": []byte(yml)}); err != nil {
		t.Fatal(err)
	}
	pins := regexp.MustCompile(`uses: (actions/[a-z-]+@[0-9a-f]{40})`)
	ciPins := map[string]bool{}
	for _, m := range pins.FindAllStringSubmatch(string(ci), -1) {
		ciPins[m[1]] = true
	}
	found := pins.FindAllStringSubmatch(yml, -1)
	if len(found) != 2 {
		t.Fatalf("release pins = %q", found)
	}
	for _, m := range found {
		if !ciPins[m[1]] {
			t.Errorf("%s is not an approved CI pin", m[1])
		}
	}
	rep := func(old, repl string) string {
		t.Helper()
		if !strings.Contains(yml, old) {
			t.Fatalf("mutation target %q not found", old)
		}
		return strings.Replace(yml, old, repl, 1)
	}
	for _, m := range []struct{ name, yml, want string }{
		{"missing-tag-verification", rep(`"$RELEASE_TAG" --verify-tag --draft --title`, `"$RELEASE_TAG" --draft --title`), "steps[5].run"},
		{"missing-edit-verification", rep(`gh release edit "$RELEASE_TAG" --verify-tag`, `gh release edit "$RELEASE_TAG"`), "steps[5].run"},
		{"write-all", rep("permissions:\n      contents: write", "permissions: write-all"), "jobs.release.permissions: must be a mapping"},
		{"write-top-level", rep("permissions:\n  contents: read", "permissions:\n  contents: write"), `permissions.contents: must be "read"`},
		{"branch-trigger", rep("tags: ['v*']", "branches: [main]"), "on.push.branches: unknown field"},
		{"floating-action", rep("actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417", "actions/setup-go@v6"), "steps[1].uses"},
		{"early-publish", rep("--verify-tag --draft --title", "--verify-tag --draft=false --title"), "steps[5].run"},
		{"extra-asset", rep(`"$RUNNER_TEMP/callsheet-release/SHA256SUMS"`, `"$RUNNER_TEMP/callsheet-release/SHA256SUMS" "$RUNNER_TEMP/callsheet-release/fake-adapter-linux-amd64"`), "steps[5].run"},
		{"wildcard-asset", rep(`"$RUNNER_TEMP/callsheet-release/SHA256SUMS"`, `"$RUNNER_TEMP/callsheet-release/"*`), "steps[5].run"},
		{"always-publish", rep("- name: Publish the release\n", "- name: Publish the release\n        if: always()\n"), "if: unknown field"},
		{"continue-on-error", rep("- name: Publish the release\n", "- name: Publish the release\n        continue-on-error: true\n"), "continue-on-error: unknown field"},
		{"tag-interpolated", rep(`--version "$RELEASE_TAG" --out`, `--version ${{ github.ref_name }} --out`), "expressions are not allowed"},
		{"personal-token", rep("GH_TOKEN: ${{ github.token }}", "GH_TOKEN: ${{ secrets.RELEASE_PAT }}"), "expressions are not allowed"},
		{"no-smoke", rep("test \"${BASH_REMATCH[1]}\" = \"$RELEASE_TAG\"", "true"), "steps[4].run"},
	} {
		t.Run(m.name, func(t *testing.T) {
			err := cicheck.ValidateReleaseWorkflow([]byte(m.yml))
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("err = %v, want %q", err, m.want)
			}
			t.Logf("rejected: %v", firstLine(err))
		})
	}

	t.Run("publication", func(t *testing.T) {
		script, err := cicheck.ReleaseStepScript([]byte(yml), cicheck.ReleaseStepPublish)
		if err != nil {
			t.Fatal(err)
		}
		tools := t.TempDir()
		gh := "#!/bin/sh\nn=$(($(cat \"$GH_LOG.count\" 2>/dev/null || echo 0)+1))\necho \"$n\" > \"$GH_LOG.count\"\nprintf '%s\\n' \"$*\" >> \"$GH_LOG\"\nif [ \"$n\" = \"$GH_FAIL_AT\" ]; then exit 7; fi\n"
		if err := os.WriteFile(filepath.Join(tools, "gh"), []byte(gh), 0o755); err != nil {
			t.Fatal(err)
		}
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Fatal(err)
		}
		run := func(t *testing.T, tag string, failAt int) ([]string, int) {
			t.Helper()
			temp := t.TempDir()
			log := filepath.Join(temp, "gh.log")
			home := t.TempDir()
			// No token or repository reaches the fake; there is no real gh on this PATH first.
			res := runBin(t, bash, home, []string{"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home, "GH_LOG=" + log, "GH_FAIL_AT=" + strconv.Itoa(failAt),
				"RELEASE_TAG=" + tag, "RUNNER_TEMP=" + temp}, "-c", script)
			b, err := os.ReadFile(log)
			if errors.Is(err, os.ErrNotExist) {
				return nil, res.code
			}
			if err != nil {
				t.Fatal(err)
			}
			return strings.Split(strings.ReplaceAll(strings.TrimSuffix(string(b), "\n"), temp, "$RUNNER_TEMP"), "\n"), res.code
		}
		assets := ""
		for _, a := range devcheck.ReleaseAssets(devcheck.Matrix) {
			assets += " $RUNNER_TEMP/callsheet-release/" + a
		}
		create := func(tag, pre string) string {
			return "release create " + tag + " --verify-tag --draft --title " + tag + " --notes Callsheet " + tag + ". Select the binary for your OS and architecture; verify SHA256SUMS before use. --prerelease=" + pre + " --latest=false" + assets
		}
		edit := func(tag, pre string) string {
			return "release edit " + tag + " --verify-tag --draft=false --prerelease=" + pre + " --latest=false"
		}
		for _, c := range []struct {
			name, tag string
			failAt    int
			code      int
			calls     []string
		}{
			{"stable", "v1.0.0", 0, 0, []string{create("v1.0.0", "false"), edit("v1.0.0", "false")}},
			{"rc", "v1.0.0-rc.1", 0, 0, []string{create("v1.0.0-rc.1", "true"), edit("v1.0.0-rc.1", "true")}},
			{"failed-upload-prevents-publish", "v1.0.0", 1, 7, []string{create("v1.0.0", "false")}},
			{"failed-edit", "v1.0.0", 2, 7, []string{create("v1.0.0", "false"), edit("v1.0.0", "false")}},
			{"invalid-tag-no-gh", "v1.0.0+meta", 0, 1, nil},
		} {
			t.Run(c.name, func(t *testing.T) {
				calls, code := run(t, c.tag, c.failAt)
				if code != c.code || !slices.Equal(calls, c.calls) {
					t.Fatalf("exit %d, calls %q; want %d, %q", code, calls, c.code, c.calls)
				}
			})
		}
	})
	t.Run("operations", func(t *testing.T) {
		doc := string(repoFile(t, "docs/maintainers/ci.md"))
		if err := checkReleaseOperations(doc); err != nil {
			t.Fatal(err)
		}
		if err := checkSourceLinks("docs/maintainers/ci.md", doc[strings.Index(doc, "\n## Release operations\n"):], checkoutLookup(testkit.MustRepoRoot(t))); err != nil {
			t.Fatal(err)
		}
		runFPMutations(t, doc, checkReleaseOperations, []readmeMutation{
			{"forbidden-curl", appendToSection("Release operations", "Then curl the asset."), "forbidden token \"curl \""},
			{"forbidden-method", appendToSection("Release operations", "Use --method PATCH."), "forbidden token \"--method PATCH\""},
			{"another-h2", appendToSection("Release operations", "## More"), "not the final H2"},
			{"no-acceptance", replaceOnce("-tags=releaseacceptance", "-tags=other"), "lacks"},
			{"no-toolchain-record", replaceOnce("record the observed Go version", "note it"), "lacks"},
			{"private-path", appendToSection("Release operations", "See /home/someone."), "private path sentinel"},
		})
		// ci.yml stays at its pinned bytes: this slice enrolls release.yml here.
		sum := sha256.Sum256(ci)
		if hex.EncodeToString(sum[:]) != ciWorkflowSHA256 {
			t.Fatal("ci.yml changed")
		}
	})
}
