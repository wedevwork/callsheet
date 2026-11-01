package devcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// The release builder behind cmd/releasebuild (design 12-2-release-quickstart,
// release.md): four stamped products, each verified without execution
// (executable header, the version value read from its symbol table and its
// retained build settings), then one deterministic SHA256SUMS. It runs no git
// command, performs no tag lookup and publishes nothing. Cross stage planning
// (CrossPlan, Cross, VerifyArtifacts) is deliberately separate: its twelve
// outputs and unstamped products are a test contract.

const (
	// ReleaseManifest is the checksum manifest's file name.
	ReleaseManifest = "SHA256SUMS"
	// ReleaseVersionSymbol is the variable the release build stamps with -X
	// and the verifier reads back from each product's symbol table.
	ReleaseVersionSymbol = modulePath + "/internal/cli.Version"

	releaseUsage = "usage: releasebuild --version VERSION --out DIRECTORY\n"
)

var releaseVersionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$`)

// ValidReleaseVersion reports whether v is exactly vMAJOR.MINOR.PATCH or
// vMAJOR.MINOR.PATCH-rc.N.
func ValidReleaseVersion(v string) bool { return releaseVersionRE.MatchString(v) }

// StampReader returns the version value stored in the product at path.
type StampReader func(path string) (string, error)

// BuildSettingsReader returns the Go version and build settings recorded in
// the product at path.
type BuildSettingsReader func(path string) (goVersion string, settings map[string]string, err error)

// ReleaseDeps are the release builder's injected dependencies.
type ReleaseDeps struct {
	Run      Runner
	Stamp    StampReader
	Settings BuildSettingsReader
}

// DefaultReleaseDeps are the production dependencies.
func DefaultReleaseDeps() ReleaseDeps {
	return ReleaseDeps{Run: ExecRunner, Stamp: ReadReleaseStamp, Settings: ReadBuildSettings}
}

// ReleaseAssets returns the published asset names for targets: one product
// per target, in target order, then the manifest.
func ReleaseAssets(targets []Target) []string {
	out := make([]string, 0, len(targets)+1)
	for _, t := range targets {
		out = append(out, CallsheetArtifact(t))
	}
	return append(out, ReleaseManifest)
}

// ReleasePlan returns the build steps that stamp version into one product per
// target, written to dir. Any invalid version or target fails the whole plan.
func ReleasePlan(version, dir string, targets []Target) ([]Step, error) {
	if !ValidReleaseVersion(version) {
		return nil, fmt.Errorf("devcheck: release: invalid version %q", version)
	}
	if len(targets) == 0 {
		return nil, errors.New("devcheck: release: no targets")
	}
	steps := make([]Step, 0, len(targets))
	for _, t := range targets {
		if err := ValidateTarget(t); err != nil {
			return nil, err
		}
		steps = append(steps, Step{
			Name: "release build " + t.String(),
			Env:  []string{"CGO_ENABLED=0", "GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH},
			Argv: []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-X " + ReleaseVersionSymbol + "=" + version,
				"-o", filepath.Join(dir, CallsheetArtifact(t)), "./cmd/callsheet"},
		})
	}
	return steps, nil
}

// PrepareReleaseDir creates the output directory, which must not exist: an
// existing directory, file or symlink is refused and left untouched.
func PrepareReleaseDir(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("devcheck: release: output %s already exists; choose a fresh directory", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("devcheck: release: output: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return fmt.Errorf("devcheck: release: output: %w", err)
	}
	return nil
}

// BuildRelease builds version for targets into dir, which must already exist,
// then verifies the products and writes the manifest. A failure leaves the
// partial output for diagnosis and never leaves a manifest.
func BuildRelease(ctx context.Context, deps ReleaseDeps, version, dir string, targets []Target, out io.Writer) error {
	steps, err := ReleasePlan(version, dir, targets)
	if err != nil {
		return err
	}
	for _, s := range steps {
		fmt.Fprintf(out, "releasebuild: %s\n", s.Name)
		if err := runStep(ctx, deps.Run, s, nil); err != nil {
			return fmt.Errorf("devcheck: release: build phase: %w", err)
		}
	}
	return VerifyReleaseAndWriteManifest(deps, version, dir, targets, out)
}

// VerifyReleaseAndWriteManifest verifies dir's products for targets and, only
// if every check passes, writes the manifest.
func VerifyReleaseAndWriteManifest(deps ReleaseDeps, version, dir string, targets []Target, out io.Writer) error {
	if err := VerifyReleaseProducts(deps, version, dir, targets, out); err != nil {
		return err
	}
	return WriteReleaseManifest(dir, targets)
}

// VerifyReleaseProducts checks dir holds exactly one product per target and
// nothing else, each a nonempty regular file whose header names its target,
// whose stored version equals version and whose retained settings match the
// production build. Every problem is reported, naming its target and phase.
func VerifyReleaseProducts(deps ReleaseDeps, version, dir string, targets []Target, out io.Writer) error {
	if !ValidReleaseVersion(version) {
		return fmt.Errorf("devcheck: release: invalid version %q", version)
	}
	if len(targets) == 0 {
		return errors.New("devcheck: release: no targets")
	}
	var problems []error
	planned := map[string]bool{}
	for _, t := range targets {
		if err := ValidateTarget(t); err != nil {
			return err
		}
		planned[CallsheetArtifact(t)] = true
		if err := verifyReleaseProduct(deps, version, filepath.Join(dir, CallsheetArtifact(t)), t, out); err != nil {
			problems = append(problems, fmt.Errorf("devcheck: release: verify %s: %w", t, err))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		problems = append(problems, fmt.Errorf("devcheck: release: verify output: %w", err))
	}
	for _, e := range entries {
		if !planned[e.Name()] {
			problems = append(problems, fmt.Errorf("devcheck: release: verify output: unexpected entry %s", e.Name()))
		}
	}
	return errors.Join(problems...)
}

func verifyReleaseProduct(deps ReleaseDeps, version, path string, t Target, out io.Writer) error {
	st, err := os.Lstat(path)
	switch {
	case err != nil:
		return fmt.Errorf("file: missing: %w", err)
	case !st.Mode().IsRegular():
		return fmt.Errorf("file: not a regular file (%v)", st.Mode().Type())
	case st.Size() == 0:
		return errors.New("file: empty")
	}
	got, err := InspectExecutable(path)
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if got != t {
		return fmt.Errorf("header: built for %s", got)
	}
	stamp, err := deps.Stamp(path)
	if err != nil {
		return fmt.Errorf("stamp: %w", err)
	}
	if stamp != version {
		return fmt.Errorf("stamp: stored version %q, want %q", stamp, version)
	}
	goVersion, settings, err := deps.Settings(path)
	if err != nil {
		return fmt.Errorf("build settings: %w", err)
	}
	if err := CheckReleaseSettings(t, settings); err != nil {
		return fmt.Errorf("build settings: %w", err)
	}
	fmt.Fprintf(out, "releasebuild: verified %s go=%s stamp=%s\n", t, goVersion, stamp)
	return nil
}

// CheckReleaseSettings requires the settings a production product retains:
// its target, no cgo and -trimpath. -ldflags is neither required nor read: a
// trimmed binary normally has no such setting.
func CheckReleaseSettings(t Target, settings map[string]string) error {
	var problems []error
	for _, want := range [][2]string{{"GOOS", t.GOOS}, {"GOARCH", t.GOARCH}, {"CGO_ENABLED", "0"}, {"-trimpath", "true"}} {
		got, ok := settings[want[0]]
		switch {
		case !ok:
			problems = append(problems, fmt.Errorf("setting %s is absent, want %q", want[0], want[1]))
		case got != want[1]:
			problems = append(problems, fmt.Errorf("setting %s = %q, want %q", want[0], got, want[1]))
		}
	}
	return errors.Join(problems...)
}

// ReadBuildSettings is the production BuildSettingsReader.
func ReadBuildSettings(path string) (string, map[string]string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	settings := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	return info.GoVersion, settings, nil
}

// WriteReleaseManifest streams each product through SHA-256 and publishes the
// manifest by rename: one "<hex>  <name>\n" line per product, sorted by name.
func WriteReleaseManifest(dir string, targets []Target) (err error) {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, CallsheetArtifact(t))
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		sum, err := hashReleaseFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("devcheck: release: manifest %s: %w", name, err)
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return fmt.Errorf("devcheck: release: manifest: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("devcheck: release: manifest: %w", err)
	}
	if err = tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("devcheck: release: manifest: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("devcheck: release: manifest: %w", err)
	}
	if err = os.Rename(tmp.Name(), filepath.Join(dir, ReleaseManifest)); err != nil {
		return fmt.Errorf("devcheck: release: manifest: %w", err)
	}
	return nil
}

func hashReleaseFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// onceValue is a string flag that refuses a second assignment.
type onceValue struct {
	value string
	set   bool
}

func (o *onceValue) String() string { return o.value }

func (o *onceValue) Set(s string) error {
	if o.set {
		return errors.New("given more than once")
	}
	o.value, o.set = s, true
	return nil
}

// ReleaseMain is the releasebuild command: 0 success, 1 a runtime, build,
// verification or I/O failure, 2 invalid arguments. The public command always
// builds all four targets.
func ReleaseMain(ctx context.Context, args []string, out, errOut io.Writer, deps ReleaseDeps) int {
	fs := flag.NewFlagSet("releasebuild", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var version, dir onceValue
	fs.Var(&version, "version", "")
	fs.Var(&dir, "out", "")
	usage := func(format string, a ...any) int {
		fmt.Fprintf(errOut, "releasebuild: "+format+"\n%s", append(a, releaseUsage)...)
		return 2
	}
	switch err := fs.Parse(args); {
	case err != nil:
		return usage("%v", err)
	case fs.NArg() != 0:
		return usage("unexpected operand %q", fs.Arg(0))
	case !version.set:
		return usage("--version is required")
	case !dir.set || dir.value == "":
		return usage("--out is required")
	case !ValidReleaseVersion(version.value):
		return usage("invalid version %q", version.value)
	}
	if err := PrepareReleaseDir(dir.value); err != nil {
		fmt.Fprintln(errOut, "releasebuild: "+err.Error())
		return 1
	}
	if err := BuildRelease(ctx, deps, version.value, dir.value, Matrix, out); err != nil {
		fmt.Fprintf(errOut, "releasebuild: %v\nreleasebuild: partial output retained in %s; retry with a fresh directory\n", err, dir.value)
		return 1
	}
	fmt.Fprintf(out, "releasebuild: wrote %s for %s in %s\n", ReleaseManifest, version.value, dir.value)
	return 0
}

// The stamped variable is a Go string: a 16-byte header of little-endian
// virtual address and byte length.
const stringHeaderSize = 16

// ReadReleaseStamp is the production StampReader: it reads the version
// variable's bytes from the 64-bit little-endian ELF or Mach-O file at path
// through its symbol table, without executing the file. Any symbol or layout
// it does not recognize is an error; it never searches the file for a string.
func ReadReleaseStamp(path string) (string, error) {
	offset, length, err := ReleaseStampRange(path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, length)
	if _, err := f.ReadAt(b, int64(offset)); err != nil {
		return "", fmt.Errorf("reading the version bytes: %w", err)
	}
	return string(b), nil
}

// ReleaseStampRange returns the file offset and length of the version bytes
// in the file at path.
func ReleaseStampRange(path string) (offset, length uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil {
		return 0, 0, fmt.Errorf("reading the file format: %w", err)
	}
	var im stampImage
	switch {
	case bytes.Equal(magic[:], []byte(elf.ELFMAG)):
		im, err = elfStampImage(f, uint64(st.Size()))
	case binary.LittleEndian.Uint32(magic[:]) == macho.Magic64:
		im, err = machoStampImage(f, uint64(st.Size()))
	default:
		return 0, 0, errors.New("unsupported file format: not a 64-bit little-endian ELF or Mach-O file")
	}
	if err != nil {
		return 0, 0, err
	}
	return im.resolve(f)
}

// stampSection is one file-backed section an address may map through.
type stampSection struct {
	addr, size, offset uint64
}

// stampImage is a parsed executable: the version variable's address and the
// candidate file-backed sections.
type stampImage struct {
	symbol   uint64
	sections []stampSection
}

// mapRange returns the file offset of [addr, addr+n), which must lie wholly
// in the one candidate section containing addr.
func (im stampImage) mapRange(what string, addr, n uint64) (uint64, error) {
	if addr == 0 {
		return 0, fmt.Errorf("%s has a zero address", what)
	}
	var found []stampSection
	for _, s := range im.sections {
		if addr >= s.addr && addr-s.addr < s.size {
			found = append(found, s)
		}
	}
	switch {
	case len(found) == 0:
		return 0, fmt.Errorf("%s at %#x is not in a file-backed section", what, addr)
	case len(found) > 1:
		return 0, fmt.Errorf("%s at %#x maps through %d sections", what, addr, len(found))
	}
	s := found[0]
	if n > s.size-(addr-s.addr) {
		return 0, fmt.Errorf("%s at %#x with length %d exceeds its section", what, addr, n)
	}
	return s.offset + (addr - s.addr), nil
}

// resolve reads the string header and returns its bytes' file range.
func (im stampImage) resolve(r io.ReaderAt) (offset, length uint64, err error) {
	headerAt, err := im.mapRange("the version header", im.symbol, stringHeaderSize)
	if err != nil {
		return 0, 0, err
	}
	var h [stringHeaderSize]byte
	if _, err := r.ReadAt(h[:], int64(headerAt)); err != nil {
		return 0, 0, fmt.Errorf("reading the version header: %w", err)
	}
	addr, n := binary.LittleEndian.Uint64(h[:8]), binary.LittleEndian.Uint64(h[8:])
	if n == 0 {
		return 0, 0, errors.New("the version string is empty")
	}
	bytesAt, err := im.mapRange("the version bytes", addr, n)
	if err != nil {
		return 0, 0, err
	}
	return bytesAt, n, nil
}

// addStampSection appends a candidate whose complete file range lies in the
// file: a section beyond the file is truncated, never a mapping target.
func addStampSection(in []stampSection, name string, addr, size, offset, fileSize uint64) ([]stampSection, error) {
	if offset > fileSize || size > fileSize-offset {
		return nil, fmt.Errorf("section %s exceeds the file", name)
	}
	return append(in, stampSection{addr, size, offset}), nil
}

func elfStampImage(r io.ReaderAt, fileSize uint64) (stampImage, error) {
	f, err := elf.NewFile(r)
	if err != nil {
		return stampImage{}, fmt.Errorf("malformed ELF file: %w", err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
		return stampImage{}, fmt.Errorf("unsupported ELF layout %v %v", f.Class, f.Data)
	}
	syms, err := f.Symbols()
	if err != nil {
		return stampImage{}, fmt.Errorf("the ELF symbol table: %w", err)
	}
	var im stampImage
	matches := 0
	for _, s := range syms {
		if s.Name != ReleaseVersionSymbol || s.Section == elf.SHN_UNDEF || s.Section >= elf.SHN_LORESERVE || int(s.Section) >= len(f.Sections) {
			continue
		}
		matches++
		if s.Size != stringHeaderSize {
			return stampImage{}, fmt.Errorf("symbol %s has size %d, want %d", ReleaseVersionSymbol, s.Size, stringHeaderSize)
		}
		im.symbol = s.Value
	}
	if err := symbolCount(matches); err != nil {
		return stampImage{}, err
	}
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_ALLOC == 0 || s.Addr == 0 || s.Type == elf.SHT_NOBITS || s.FileSize == 0 {
			continue
		}
		if im.sections, err = addStampSection(im.sections, s.Name, s.Addr, s.FileSize, s.Offset, fileSize); err != nil {
			return stampImage{}, err
		}
	}
	return im, nil
}

// Mach-O section types without file contents (loader.h: SECTION_TYPE mask,
// S_ZEROFILL, S_GB_ZEROFILL and S_THREAD_LOCAL_ZEROFILL); debug/macho exposes
// only the raw flags. nSect and nStab are the nlist type bits of a
// section-defined symbol and a debugging entry.
const (
	machoSectionType = 0xff
	machoNTypeMask   = 0x0e
	machoNSect       = 0x0e
	machoNStabMask   = 0xe0
)

var machoZeroFill = []uint32{0x1, 0xC, 0x12}

func machoStampImage(r io.ReaderAt, fileSize uint64) (stampImage, error) {
	f, err := macho.NewFile(r)
	if err != nil {
		return stampImage{}, fmt.Errorf("malformed Mach-O file: %w", err)
	}
	defer f.Close()
	if f.Symtab == nil {
		return stampImage{}, fmt.Errorf("symbol %s is absent: no symbol table", ReleaseVersionSymbol)
	}
	var im stampImage
	matches := 0
	for _, s := range f.Symtab.Syms {
		if s.Name != ReleaseVersionSymbol || s.Type&machoNStabMask != 0 || s.Type&machoNTypeMask != machoNSect || s.Sect == 0 || int(s.Sect) > len(f.Sections) {
			continue
		}
		matches++
		im.symbol = s.Value
	}
	if err := symbolCount(matches); err != nil {
		return stampImage{}, err
	}
	for _, s := range f.Sections {
		if s.Addr == 0 || s.Offset == 0 || s.Size == 0 || slices.Contains(machoZeroFill, s.Flags&machoSectionType) {
			continue
		}
		if im.sections, err = addStampSection(im.sections, s.Name, s.Addr, s.Size, uint64(s.Offset), fileSize); err != nil {
			return stampImage{}, err
		}
	}
	return im, nil
}

func symbolCount(n int) error {
	switch n {
	case 1:
		return nil
	case 0:
		return fmt.Errorf("symbol %s is absent", ReleaseVersionSymbol)
	}
	return fmt.Errorf("symbol %s is defined %d times", ReleaseVersionSymbol, n)
}
