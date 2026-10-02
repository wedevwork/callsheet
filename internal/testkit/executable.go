package testkit

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// SyntheticExecutable returns the smallest executable file header that
// debug/elf (goos linux) or debug/macho (goos darwin) accepts for goarch
// amd64 or arm64: a 64-bit little-endian header with no sections, segments
// or load commands. It is a fixture for header inspection (the devcheck
// cross verifier and the fake runners that stand in for its builds) and is
// never runnable.
func SyntheticExecutable(goos, goarch string) ([]byte, error) {
	var b bytes.Buffer
	switch goos {
	case "linux":
		machine, ok := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[goarch]
		if !ok {
			return nil, fmt.Errorf("testkit: no synthetic ELF for %s/%s", goos, goarch)
		}
		h := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT),
			Ehsize: 64, Phentsize: 56, Shentsize: 64}
		copy(h.Ident[:], elf.ELFMAG)
		h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
		h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
		h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
		binary.Write(&b, binary.LittleEndian, h)
	case "darwin":
		cpu, ok := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[goarch]
		if !ok {
			return nil, fmt.Errorf("testkit: no synthetic Mach-O for %s/%s", goos, goarch)
		}
		binary.Write(&b, binary.LittleEndian, macho.FileHeader{Magic: macho.Magic64, Cpu: cpu, Type: macho.TypeExec})
		b.Write(make([]byte, 4)) // the 64-bit header's reserved word
	default:
		return nil, fmt.Errorf("testkit: no synthetic executable for %s/%s", goos, goarch)
	}
	return b.Bytes(), nil
}

// FakeGoBuild stands in for a "go build -o PATH" or "go test -c -o PATH"
// child in an injected devcheck runner: it writes SyntheticExecutable for
// the child environment's GOOS/GOARCH (the last assignment of each) to
// PATH, so a verified cross build sees a complete, correct output. It
// reports whether argv was such a build; any other argv writes nothing.
func FakeGoBuild(argv, env []string) (bool, error) {
	if len(argv) < 3 || argv[0] != "go" || (argv[1] != "build" && (argv[1] != "test" || argv[2] != "-c")) {
		return false, nil
	}
	out := ""
	for i, a := range argv[:len(argv)-1] {
		if a == "-o" {
			out = argv[i+1]
		}
	}
	if out == "" {
		return false, nil
	}
	var goos, goarch string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GOOS="); ok {
			goos = v
		}
		if v, ok := strings.CutPrefix(kv, "GOARCH="); ok {
			goarch = v
		}
	}
	b, err := SyntheticExecutable(goos, goarch)
	if err != nil {
		return true, err
	}
	return true, os.WriteFile(out, b, 0o755)
}
