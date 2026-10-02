package testkit

import (
	"debug/elf"
	"debug/macho"
	"os"
	"path/filepath"
	"testing"
)

// TestSyntheticExecutable: each supported pair parses as an executable of
// its format and machine; anything else is refused.
func TestSyntheticExecutable(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		goos, goarch string
		elf          elf.Machine
		cpu          macho.Cpu
	}{{"linux", "amd64", elf.EM_X86_64, 0}, {"linux", "arm64", elf.EM_AARCH64, 0}, {"darwin", "amd64", 0, macho.CpuAmd64}, {"darwin", "arm64", 0, macho.CpuArm64}} {
		b, err := SyntheticExecutable(c.goos, c.goarch)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, c.goos+"-"+c.goarch)
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		if c.goos == "linux" {
			f, err := elf.Open(p)
			if err != nil || f.Type != elf.ET_EXEC || f.Machine != c.elf || f.OSABI != elf.ELFOSABI_NONE {
				t.Fatalf("%s/%s: %v %+v", c.goos, c.goarch, err, f)
			}
			f.Close()
			continue
		}
		f, err := macho.Open(p)
		if err != nil || f.Type != macho.TypeExec || f.Cpu != c.cpu {
			t.Fatalf("%s/%s: %v %+v", c.goos, c.goarch, err, f)
		}
		f.Close()
	}
	for _, bad := range [][2]string{{"linux", "386"}, {"darwin", "riscv64"}, {"windows", "amd64"}, {"", ""}} {
		if _, err := SyntheticExecutable(bad[0], bad[1]); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

// TestFakeGoBuild: a go build or go test -c with -o writes the synthetic
// executable for the environment's last GOOS/GOARCH; any other argv writes
// nothing; an unsupported target or an unwritable path fails.
func TestFakeGoBuild(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "a")
	ok, err := FakeGoBuild([]string{"go", "build", "-o", out, "./cmd/callsheet"}, []string{"GOOS=linux", "GOARCH=amd64", "GOOS=darwin", "GOARCH=arm64"})
	if !ok || err != nil {
		t.Fatalf("build = %v %v", ok, err)
	}
	if f, err := macho.Open(out); err != nil || f.Cpu != macho.CpuArm64 {
		t.Fatalf("written %v", err)
	} else {
		f.Close()
	}
	test := filepath.Join(dir, "b.test")
	if ok, err := FakeGoBuild([]string{"go", "test", "-c", "-o", test, "./internal/spikes/processgroup"}, []string{"GOOS=linux", "GOARCH=arm64"}); !ok || err != nil {
		t.Fatalf("test -c = %v %v", ok, err)
	}
	if f, err := elf.Open(test); err != nil || f.Machine != elf.EM_AARCH64 {
		t.Fatalf("written %v", err)
	} else {
		f.Close()
	}
	for _, argv := range [][]string{nil, {"go", "test", "-count=1", "-o", "x", "./..."}, {"go", "build", "./cmd/callsheet"}, {"go", "vet", "-o", "x"}, {"gofmt", "build", "-o", "x"}, {"go", "build", "-o"}} {
		if ok, err := FakeGoBuild(argv, []string{"GOOS=linux", "GOARCH=amd64"}); ok || err != nil {
			t.Fatalf("%v = %v %v", argv, ok, err)
		}
	}
	if ok, err := FakeGoBuild([]string{"go", "build", "-o", filepath.Join(dir, "c"), "."}, []string{"GOOS=plan9", "GOARCH=amd64"}); !ok || err == nil {
		t.Fatalf("unsupported target = %v %v", ok, err)
	}
	if ok, err := FakeGoBuild([]string{"go", "build", "-o", filepath.Join(dir, "absent", "d"), "."}, []string{"GOOS=linux", "GOARCH=amd64"}); !ok || err == nil {
		t.Fatalf("unwritable path = %v %v", ok, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("written %v", entries)
	}
}
