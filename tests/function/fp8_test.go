package function

import (
	"context"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP8BuildMatrix invokes devcheck.Cross with ExecRunner and the six-target
// matrix from the repository root, then inspects (never executes) every
// artifact's target metadata.
func TestFP8BuildMatrix(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	t.Chdir(root)
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := devcheck.Cross(ctx, devcheck.ExecRunner, out, devcheck.Matrix); err != nil {
		t.Fatalf("cross: %v", err)
	}
	var want []string
	for _, tg := range devcheck.Matrix {
		want = append(want, devcheck.CallsheetArtifact(tg))
		if tg.Unix() {
			want = append(want, devcheck.FakeArtifact(tg), devcheck.ProcessTestArtifact(tg))
		}
	}
	if len(want) != 14 {
		t.Fatalf("expected 6+4+4 artifacts, planned %d", len(want))
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != len(want) {
		t.Fatalf("artifacts = %d, want %d", len(entries), len(want))
	}
	for _, tg := range devcheck.Matrix {
		names := []string{devcheck.CallsheetArtifact(tg)}
		if tg.Unix() {
			names = append(names, devcheck.FakeArtifact(tg), devcheck.ProcessTestArtifact(tg))
		}
		for _, n := range names {
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
	if f, err := pe.Open(p); err == nil {
		defer f.Close()
		arch := map[uint16]string{pe.IMAGE_FILE_MACHINE_AMD64: "amd64", pe.IMAGE_FILE_MACHINE_ARM64: "arm64"}[f.Machine]
		if f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 {
			t.Fatalf("%s is not an executable PE image", p)
		}
		return "windows", arch
	}
	t.Fatalf("%s: unrecognised executable format", p)
	return "", ""
}
