//go:build releaseacceptance

package devcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestReleaseTrimmedProducts is the opt-in, mandatory release acceptance
// (release.md): the real builder with the production argv and real readers
// for all four targets, then a real per-target wrong-stamp mutation through
// the same production verify-and-manifest function. It builds local artifacts
// only and is never a default function-package test or CI stage.
func TestReleaseTrimmedProducts(t *testing.T) {
	const (
		version = "v0.0.0-rc.1"
		wrong   = "v0.0.0-rc.2"
	)
	root := testkit.MustRepoRoot(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	dir := filepath.Join(t.TempDir(), "release")
	if err := PrepareReleaseDir(dir); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := BuildRelease(context.Background(), DefaultReleaseDeps(), version, dir, Matrix, &log); err != nil {
		t.Fatal(err)
	}
	t.Logf("builder output:\n%s", log.String())

	manifest, err := os.ReadFile(filepath.Join(dir, ReleaseManifest))
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, name := range releaseProductNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		want.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	if string(manifest) != want.String() {
		t.Fatalf("manifest:\n%s\nindependently computed:\n%s", manifest, want.String())
	}
	if got := len(dirNames(t, dir)); got != 5 {
		t.Fatalf("%d entries, want 5", got)
	}

	for _, tg := range Matrix {
		p := filepath.Join(dir, CallsheetArtifact(tg))
		gotTarget, err := InspectExecutable(p)
		if err != nil || gotTarget != tg {
			t.Fatalf("%s: header %v, %v", tg, gotTarget, err)
		}
		stamp, err := ReadReleaseStamp(p)
		if err != nil || stamp != version {
			t.Fatalf("%s: stamp %q, %v", tg, stamp, err)
		}
		goVersion, settings, err := ReadBuildSettings(p)
		if err != nil {
			t.Fatalf("%s: settings: %v", tg, err)
		}
		if err := CheckReleaseSettings(tg, settings); err != nil {
			t.Fatalf("%s: %v", tg, err)
		}
		if _, present := settings["-ldflags"]; present {
			t.Errorf("%s: a trimmed product recorded -ldflags: the recorded toolchain behavior changed", tg)
		}
		t.Logf("%s: header ok, stamp %q, go %s, GOOS=%s GOARCH=%s CGO_ENABLED=%s -trimpath=%s", tg, stamp, goVersion,
			settings["GOOS"], settings["GOARCH"], settings["CGO_ENABLED"], settings["-trimpath"])
		if runtime.GOOS == tg.GOOS && runtime.GOARCH == tg.GOARCH {
			out, err := exec.Command(p, "version").Output()
			if err != nil || !strings.HasPrefix(string(out), "callsheet "+stamp+" protocol=") {
				t.Fatalf("%s: native version disagrees with the read stamp: %q, %v", tg, out, err)
			}
			t.Logf("%s: native execution: %s", tg, strings.TrimSpace(string(out)))
		}
	}

	for _, tg := range Matrix {
		t.Run("wrong-stamp-"+tg.GOOS+"-"+tg.GOARCH, func(t *testing.T) {
			mutated := t.TempDir()
			for _, other := range Matrix {
				name := CallsheetArtifact(other)
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if other == tg {
					data = patchStamp(t, filepath.Join(dir, name), data, version, wrong)
				}
				if err := os.WriteFile(filepath.Join(mutated, name), data, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := ReadReleaseStamp(filepath.Join(mutated, CallsheetArtifact(tg))); err != nil || got != wrong {
				t.Fatalf("patched stamp = %q, %v", got, err)
			}
			err := VerifyReleaseAndWriteManifest(DefaultReleaseDeps(), version, mutated, Matrix, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("verify %s: stamp: stored version %q", tg, wrong)) {
				t.Fatalf("err = %v", err)
			}
			if _, err := os.Stat(filepath.Join(mutated, ReleaseManifest)); err == nil {
				t.Fatal("a manifest was written")
			}
			t.Logf("%s: rejected: %v", tg, err)
		})
	}
	// The mutations never touched the successful output.
	again, err := os.ReadFile(filepath.Join(dir, ReleaseManifest))
	if err != nil || !bytes.Equal(again, manifest) {
		t.Fatalf("successful output changed: %v", err)
	}
}

// patchStamp returns data with exactly the stored version bytes replaced by a
// same-length value, and proves nothing else changed.
func patchStamp(t *testing.T, path string, data []byte, from, to string) []byte {
	t.Helper()
	if len(from) != len(to) {
		t.Fatal("the mutation must keep the length")
	}
	offset, length, err := ReleaseStampRange(path)
	if err != nil {
		t.Fatal(err)
	}
	if length != uint64(len(from)) || string(data[offset:offset+length]) != from {
		t.Fatalf("located %q at %d", data[offset:offset+length], offset)
	}
	out := bytes.Clone(data)
	copy(out[offset:], to)
	diff := 0
	for i := range out {
		if out[i] != data[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Fatalf("%d bytes changed, want the version's one differing byte", diff)
	}
	return out
}
