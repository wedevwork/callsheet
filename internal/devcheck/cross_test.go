package devcheck

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// matrixNames is the twelve-artifact set of the four supported targets,
// written out independently of the planner.
var matrixNames = []string{
	"callsheet-linux-amd64", "fake-adapter-linux-amd64", "processgroup-linux-amd64.test",
	"callsheet-linux-arm64", "fake-adapter-linux-arm64", "processgroup-linux-arm64.test",
	"callsheet-darwin-amd64", "fake-adapter-darwin-amd64", "processgroup-darwin-amd64.test",
	"callsheet-darwin-arm64", "fake-adapter-darwin-arm64", "processgroup-darwin-arm64.test",
}

// TestCrossArtifactsPlannedNames: the verifier's artifact set is exactly
// the plan's -o outputs, in plan order, each with its step's target.
func TestCrossArtifactsPlannedNames(t *testing.T) {
	arts, err := CrossArtifacts("/out", Matrix)
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := CrossPlan("/out", Matrix)
	if len(arts) != 12 || len(steps) != 12 {
		t.Fatalf("artifacts %d, steps %d", len(arts), len(steps))
	}
	for i, a := range arts {
		if a.Path != filepath.Join("/out", matrixNames[i]) || a.Target != Matrix[i/3] {
			t.Fatalf("artifact %d = %+v, want %s for %s", i, a, matrixNames[i], Matrix[i/3])
		}
		argv := steps[i].Argv
		if j := slices.Index(argv, "-o"); j < 0 || argv[j+1] != a.Path {
			t.Fatalf("step %d argv %v does not write %s", i, argv, a.Path)
		}
		if !slices.Contains(steps[i].Env, "GOOS="+a.Target.GOOS) || !slices.Contains(steps[i].Env, "GOARCH="+a.Target.GOARCH) {
			t.Fatalf("step %d env %v, artifact target %s", i, steps[i].Env, a.Target)
		}
	}
	for _, bad := range [][]Target{nil, {{"windows", "amd64"}}} {
		if _, err := CrossArtifacts("/out", bad); err == nil {
			t.Fatalf("CrossArtifacts(%v) planned", bad)
		}
		if err := VerifyArtifacts(t.TempDir(), bad); err == nil {
			t.Fatalf("VerifyArtifacts(%v) passed", bad)
		}
	}
}

// writeMatrix writes a synthetic, correct artifact for every planned
// artifact of targets into a fresh directory.
func writeMatrix(t *testing.T, targets []Target) string {
	t.Helper()
	dir := t.TempDir()
	arts, err := CrossArtifacts(dir, targets)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range arts {
		writeSynthetic(t, a.Path, a.Target)
	}
	return dir
}

func writeSynthetic(t *testing.T, path string, tg Target) []byte {
	t.Helper()
	b, err := testkit.SyntheticExecutable(tg.GOOS, tg.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

// patch overwrites a little-endian field of size 2 or 4 at off in path.
func patch(t *testing.T, path string, off int, v uint32, size int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if size == 2 {
		binary.LittleEndian.PutUint16(b[off:], uint16(v))
	} else {
		binary.LittleEndian.PutUint32(b[off:], v)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyArtifacts: the complete synthetic matrix passes; each defect
// (wrong arch, wrong OS, missing, empty, not a regular file, not an
// executable, unknown machine, unknown format, extra file) fails, naming
// its artifact; several defects are all reported.
func TestVerifyArtifacts(t *testing.T) {
	if err := VerifyArtifacts(writeMatrix(t, Matrix), Matrix); err != nil {
		t.Fatalf("complete matrix: %v", err)
	}
	cases := []struct {
		name   string
		break_ func(t *testing.T, dir string)
		want   []string
	}{
		{"wrong arch", func(t *testing.T, dir string) {
			writeSynthetic(t, filepath.Join(dir, "callsheet-linux-arm64"), Target{"linux", "amd64"})
		}, []string{"cross artifact callsheet-linux-arm64: built for linux/amd64, want linux/arm64"}},
		{"wrong os", func(t *testing.T, dir string) {
			writeSynthetic(t, filepath.Join(dir, "fake-adapter-darwin-arm64"), Target{"linux", "arm64"})
		}, []string{"cross artifact fake-adapter-darwin-arm64: built for linux/arm64, want darwin/arm64"}},
		{"missing", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "processgroup-darwin-amd64.test"))
		}, []string{"cross artifact processgroup-darwin-amd64.test: missing"}},
		{"empty", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "callsheet-darwin-amd64"), nil, 0o755)
		}, []string{"cross artifact callsheet-darwin-amd64: empty"}},
		{"directory", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "callsheet-linux-amd64")
			os.Remove(p)
			os.Mkdir(p, 0o755)
		}, []string{"cross artifact callsheet-linux-amd64: not a regular file"}},
		{"elf object", func(t *testing.T, dir string) {
			patch(t, filepath.Join(dir, "fake-adapter-linux-amd64"), 16, 1, 2) // e_type ET_REL
		}, []string{"cross artifact fake-adapter-linux-amd64: not a linux/amd64 executable: ELF type ET_REL is not an executable"}},
		{"elf os abi", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "fake-adapter-linux-arm64")
			b, _ := os.ReadFile(p)
			b[7] = 9 // EI_OSABI FreeBSD
			os.WriteFile(p, b, 0o755)
		}, []string{"cross artifact fake-adapter-linux-arm64: not a linux/arm64 executable: ELF OS ABI ELFOSABI_FREEBSD is not linux"}},
		{"elf machine", func(t *testing.T, dir string) {
			patch(t, filepath.Join(dir, "callsheet-linux-amd64"), 18, 3, 2) // e_machine EM_386
		}, []string{"cross artifact callsheet-linux-amd64: not a linux/amd64 executable: ELF machine EM_386 is not amd64 or arm64"}},
		{"macho object", func(t *testing.T, dir string) {
			patch(t, filepath.Join(dir, "callsheet-darwin-arm64"), 12, 1, 4) // filetype MH_OBJECT
		}, []string{"cross artifact callsheet-darwin-arm64: not a darwin/arm64 executable: Mach-O type Obj is not an executable"}},
		{"macho cpu", func(t *testing.T, dir string) {
			patch(t, filepath.Join(dir, "callsheet-darwin-amd64"), 4, 7, 4) // CPU_TYPE_X86 (386)
		}, []string{"cross artifact callsheet-darwin-amd64: not a darwin/amd64 executable: Mach-O CPU Cpu386 is not amd64 or arm64"}},
		{"unknown format", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "processgroup-linux-amd64.test"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
		}, []string{"cross artifact processgroup-linux-amd64.test: not a linux/amd64 executable: neither an ELF nor a Mach-O file"}},
		{"extra file", func(t *testing.T, dir string) {
			writeSynthetic(t, filepath.Join(dir, "callsheet-linux-amd64.old"), Target{"linux", "amd64"})
		}, []string{"cross artifact callsheet-linux-amd64.old: not a planned artifact"}},
		{"several", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "callsheet-linux-amd64"))
			writeSynthetic(t, filepath.Join(dir, "processgroup-darwin-arm64.test"), Target{"darwin", "amd64"})
		}, []string{"cross artifact callsheet-linux-amd64: missing", "cross artifact processgroup-darwin-arm64.test: built for darwin/amd64, want darwin/arm64"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeMatrix(t, Matrix)
			c.break_(t, dir)
			err := VerifyArtifacts(dir, Matrix)
			if err == nil {
				t.Fatal("verification passed")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not contain %q", err, w)
				}
			}
			if n := strings.Count(err.Error(), "cross artifact "); n != len(c.want) {
				t.Fatalf("%d problems reported, want %d: %v", n, len(c.want), err)
			}
		})
	}
	// An unreadable output directory fails.
	if err := VerifyArtifacts(filepath.Join(t.TempDir(), "absent"), Matrix[:1]); err == nil || !strings.Contains(err.Error(), "devcheck: cross output:") {
		t.Fatalf("absent output: %v", err)
	}
}

// TestVerifyArtifactsRealExecutable: a real executable (this host-built
// test binary) is recognised as its own target and passes as that target,
// and the same bytes planned under foreign target names fail: another OS,
// and the same OS on another architecture.
func TestVerifyArtifactsRealExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	host := Target{runtime.GOOS, runtime.GOARCH}
	if ValidateTarget(host) != nil {
		t.Fatalf("host %s is not a supported target", host)
	}
	if got, err := InspectExecutable(self); err != nil || got != host {
		t.Fatalf("InspectExecutable(test binary) = %v, %v; want %s", got, err, host)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	plant := func(tg Target) string {
		dir := t.TempDir()
		arts, err := CrossArtifacts(dir, []Target{tg})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range arts {
			if err := os.WriteFile(a.Path, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	if err := VerifyArtifacts(plant(host), []Target{host}); err != nil {
		t.Fatalf("host artifacts: %v", err)
	}
	for _, tg := range Matrix {
		if tg == host {
			continue
		}
		err := VerifyArtifacts(plant(tg), []Target{tg})
		if err == nil || !strings.Contains(err.Error(), "cross artifact "+CallsheetArtifact(tg)+": built for "+host.String()+", want "+tg.String()) ||
			strings.Count(err.Error(), "cross artifact ") != 3 {
			t.Fatalf("host binary planned as %s: %v", tg, err)
		}
	}
}

// TestCrossVerifiesItsBuild: Cross fails after a build that leaves an
// artifact missing or built for the wrong target, naming the artifact, and
// runs every build step first.
func TestCrossVerifiesItsBuild(t *testing.T) {
	// A runner whose builds write nothing.
	var calls int
	nothing := func(context.Context, []string, []string, string, io.Writer, io.Writer) error { calls++; return nil }
	err := Cross(context.Background(), nothing, t.TempDir(), Matrix)
	if err == nil || calls != 12 || strings.Count(err.Error(), ": missing") != 12 || !strings.Contains(err.Error(), "cross artifact callsheet-darwin-arm64: missing") {
		t.Fatalf("empty build after %d calls: %v", calls, err)
	}
	// A runner that ignores GOARCH for one artifact.
	wrong := func(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
		if slices.Contains(env, "GOARCH=arm64") && strings.Contains(strings.Join(argv, " "), "fake-adapter-linux-arm64") {
			env = append(slices.Clone(env), "GOARCH=amd64")
		}
		_, err := testkit.FakeGoBuild(argv, env)
		return err
	}
	err = Cross(context.Background(), wrong, t.TempDir(), Matrix)
	var se *StepError
	if err == nil || errors.As(err, &se) || err.Error() != "devcheck: cross artifact fake-adapter-linux-arm64: built for linux/amd64, want linux/arm64" {
		t.Fatalf("wrong arch build: %v", err)
	}
	// The driver's cross stage fails on it and retains its logs.
	code, out, errOut := runDriver(t, "linux", &wrongArchRunner{}, "cross")
	if code != 1 || !strings.Contains(errOut, "stage cross FAILED: devcheck: cross artifact callsheet-darwin-amd64: built for darwin/arm64, want darwin/amd64") ||
		strings.Contains(out, "verified") {
		t.Fatalf("cross stage = %d %s %s", code, out, errOut)
	}
	os.RemoveAll(scratchFrom(out))
}

// wrongArchRunner builds every artifact correctly except
// callsheet-darwin-amd64, which it builds for darwin/arm64.
type wrongArchRunner struct{}

func (wrongArchRunner) run(_ context.Context, argv, env []string, _ string, _, _ io.Writer) error {
	if strings.Contains(strings.Join(argv, " "), "callsheet-darwin-amd64") {
		env = append(slices.Clone(env), "GOARCH=arm64")
	}
	_, err := testkit.FakeGoBuild(argv, env)
	return err
}
