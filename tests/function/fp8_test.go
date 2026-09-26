package function

import (
	"context"
	"debug/elf"
	"debug/macho"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP8BuildMatrix invokes devcheck.Cross with ExecRunner and the
// four-target Linux/macOS matrix from the repository root, then inspects
// (never executes) every artifact's target metadata.
func TestFP8BuildMatrix(t *testing.T) {
	// The supported set is asserted independently of the planner.
	supported := []devcheck.Target{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "linux", GOARCH: "arm64"}, {GOOS: "darwin", GOARCH: "amd64"}, {GOOS: "darwin", GOARCH: "arm64"}}
	if len(devcheck.Matrix) != len(supported) {
		t.Fatalf("matrix = %v, want %v", devcheck.Matrix, supported)
	}
	for i, tg := range supported {
		if devcheck.Matrix[i] != tg {
			t.Fatalf("matrix = %v, want %v", devcheck.Matrix, supported)
		}
	}
	root := testkit.MustRepoRoot(t)
	t.Chdir(root)
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := devcheck.Cross(ctx, devcheck.ExecRunner, out, devcheck.Matrix); err != nil {
		t.Fatalf("cross: %v", err)
	}
	var want []string
	for _, tg := range supported {
		want = append(want, devcheck.CallsheetArtifact(tg), devcheck.FakeArtifact(tg), devcheck.ProcessTestArtifact(tg))
	}
	if len(want) != 12 {
		t.Fatalf("expected 4+4+4 artifacts, planned %d", len(want))
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != len(want) {
		t.Fatalf("artifacts = %d, want %d", len(entries), len(want))
	}
	for _, tg := range supported {
		for _, n := range []string{devcheck.CallsheetArtifact(tg), devcheck.FakeArtifact(tg), devcheck.ProcessTestArtifact(tg)} {
			p := filepath.Join(out, n)
			st, err := os.Stat(p)
			if err != nil || st.Size() == 0 {
				t.Fatalf("%s missing or empty: %v", n, err)
			}
			goos, goarch := inspect(t, p)
			if goos != tg.GOOS || goarch != tg.GOARCH {
				t.Fatalf("%s metadata = %s/%s, want %s", n, goos, goarch, tg)
			}
		}
	}
}

// inspect reads executable headers to determine the target without running it.
func inspect(t *testing.T, p string) (string, string) {
	t.Helper()
	if f, err := elf.Open(p); err == nil {
		defer f.Close()
		arch := map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64"}[f.Machine]
		if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
			t.Fatalf("%s is not an executable ELF", p)
		}
		return "linux", arch
	}
	if f, err := macho.Open(p); err == nil {
		defer f.Close()
		arch := map[macho.Cpu]string{macho.CpuAmd64: "amd64", macho.CpuArm64: "arm64"}[f.Cpu]
		if f.Type != macho.TypeExec {
			t.Fatalf("%s is not an executable Mach-O", p)
		}
		return "darwin", arch
	}
	t.Fatalf("%s: unrecognised executable format", p)
	return "", ""
}
