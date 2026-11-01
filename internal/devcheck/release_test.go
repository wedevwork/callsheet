package devcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

const releaseTestVersion = "v0.0.0-rc.1"

// releaseProductNames are the four products, written out independently of
// the planner, in lexical order.
var releaseProductNames = []string{"callsheet-darwin-amd64", "callsheet-darwin-arm64", "callsheet-linux-amd64", "callsheet-linux-arm64"}

func TestValidReleaseVersion(t *testing.T) {
	for _, v := range []string{"v0.1.0", "v1.0.0-rc.1", "v0.0.0", "v10.20.30", "v1.2.3-rc.12"} {
		if !ValidReleaseVersion(v) {
			t.Errorf("%q rejected", v)
		}
	}
	for _, v := range []string{"", "v1", "v1.0", "1.0.0", "v01.0.0", "v1.00.0", "v1.0.00", "v1.0.0-rc.0", "v1.0.0-rc.01", "v1.0.0-rc", "v1.0.0+meta", "v1.0.0-beta.1",
		"v1.0.0 ", " v1.0.0", "v1.0.0\n", "v1.0.0/x", "../v1.0.0", "v1.0.0-rc.1+meta", "V1.0.0"} {
		if ValidReleaseVersion(v) {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestReleasePlan(t *testing.T) {
	steps, err := ReleasePlan(releaseTestVersion, "/out", Matrix)
	if err != nil || len(steps) != 4 {
		t.Fatalf("plan = %v, %v", steps, err)
	}
	for i, s := range steps {
		tg := Matrix[i]
		want := []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-X github.com/wedevwork/callsheet/internal/cli.Version=" + releaseTestVersion,
			"-o", "/out/callsheet-" + tg.GOOS + "-" + tg.GOARCH, "./cmd/callsheet"}
		if !slices.Equal(s.Argv, want) {
			t.Errorf("step %d argv = %q", i, s.Argv)
		}
		if !slices.Equal(s.Env, []string{"CGO_ENABLED=0", "GOOS=" + tg.GOOS, "GOARCH=" + tg.GOARCH}) {
			t.Errorf("step %d env = %q", i, s.Env)
		}
		if slices.ContainsFunc(s.Argv, func(a string) bool { return a == "-s" || a == "-w" || strings.Contains(a, "-s -w") }) {
			t.Errorf("step %d strips: %q", i, s.Argv)
		}
	}
	for _, c := range []struct {
		version string
		targets []Target
		want    string
	}{
		{"v1", Matrix, "invalid version"},
		{releaseTestVersion, nil, "no targets"},
		{releaseTestVersion, []Target{{"windows", "amd64"}}, "unsupported target"},
	} {
		if _, err := ReleasePlan(c.version, "/out", c.targets); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("plan(%q, %v) = %v, want %q", c.version, c.targets, err, c.want)
		}
	}
	if got := ReleaseAssets(Matrix); !slices.Equal(got, []string{"callsheet-linux-amd64", "callsheet-linux-arm64", "callsheet-darwin-amd64", "callsheet-darwin-arm64", "SHA256SUMS"}) {
		t.Errorf("assets = %q", got)
	}
}

func TestPrepareReleaseDir(t *testing.T) {
	root := t.TempDir()
	fresh := filepath.Join(root, "fresh")
	if err := PrepareReleaseDir(fresh); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(fresh); err != nil || !st.IsDir() {
		t.Fatalf("not created: %v", err)
	}
	// An existing directory (with its content), file and symlink are refused
	// and left untouched.
	if err := os.WriteFile(filepath.Join(fresh, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "absent"), link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fresh, file, link} {
		if err := PrepareReleaseDir(p); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("%s: %v", p, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(fresh, "keep")); string(b) != "x" {
		t.Error("existing directory content changed")
	}
	if b, _ := os.ReadFile(file); string(b) != "y" {
		t.Error("existing file changed")
	}
	if _, err := os.Lstat(filepath.Join(root, "absent")); err == nil {
		t.Error("symlink target created")
	}
	if err := PrepareReleaseDir(filepath.Join(root, "no", "parent")); err == nil {
		t.Error("missing parent accepted")
	}
	if err := PrepareReleaseDir(filepath.Join(file, "under")); err == nil {
		t.Error("path below a file accepted")
	}
}

// releaseFakes are injected dependencies that accept header-only products.
func releaseFakes(version string) ReleaseDeps {
	return ReleaseDeps{
		Run: func(_ context.Context, argv, env []string, _ string, _, _ io.Writer) error {
			_, err := testkit.FakeGoBuild(argv, env)
			return err
		},
		Stamp: func(string) (string, error) { return version, nil },
		Settings: func(path string) (string, map[string]string, error) {
			got, err := InspectExecutable(path)
			if err != nil {
				return "", nil, err
			}
			return "go-test", map[string]string{"GOOS": got.GOOS, "GOARCH": got.GOARCH, "CGO_ENABLED": "0", "-trimpath": "true"}, nil
		},
	}
}

func writeReleaseProducts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tg := range Matrix {
		writeSynthetic(t, filepath.Join(dir, CallsheetArtifact(tg)), tg)
	}
	return dir
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func wantManifest(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range releaseProductNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		b.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	return b.String()
}

func TestBuildRelease(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := BuildRelease(context.Background(), releaseFakes(releaseTestVersion), releaseTestVersion, dir, Matrix, &out); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, append([]string{"SHA256SUMS"}, releaseProductNames...)) {
		t.Fatalf("entries = %q", got)
	}
	b, err := os.ReadFile(filepath.Join(dir, ReleaseManifest))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != wantManifest(t, dir) {
		t.Fatalf("manifest = %q", b)
	}
	for _, tg := range Matrix {
		if !strings.Contains(out.String(), "verified "+tg.String()+" go=go-test stamp="+releaseTestVersion+"\n") {
			t.Errorf("no verification line for %s:\n%s", tg, out.String())
		}
	}

	// A runner failure names its target and leaves the earlier products and
	// no manifest.
	dir = t.TempDir()
	deps := releaseFakes(releaseTestVersion)
	calls := 0
	deps.Run = func(_ context.Context, argv, env []string, _ string, _, _ io.Writer) error {
		if calls++; calls == 3 {
			return errors.New("boom")
		}
		_, err := testkit.FakeGoBuild(argv, env)
		return err
	}
	err = BuildRelease(context.Background(), deps, releaseTestVersion, dir, Matrix, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "build phase") || !strings.Contains(err.Error(), "release build darwin/amd64") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("runner failure: %v", err)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"callsheet-linux-amd64", "callsheet-linux-arm64"}) {
		t.Fatalf("partial output = %q", got)
	}
	if err := BuildRelease(context.Background(), deps, "bad", dir, Matrix, io.Discard); err == nil {
		t.Fatal("invalid version built")
	}
}

func TestVerifyReleaseProducts(t *testing.T) {
	okDeps := releaseFakes(releaseTestVersion)
	for _, c := range []struct {
		name   string
		mutate func(t *testing.T, dir string, deps *ReleaseDeps)
		want   string
	}{
		{"missing", func(t *testing.T, dir string, _ *ReleaseDeps) {
			os.Remove(filepath.Join(dir, "callsheet-linux-arm64"))
		}, "verify linux/arm64: file: missing"},
		{"empty", func(t *testing.T, dir string, _ *ReleaseDeps) {
			os.WriteFile(filepath.Join(dir, "callsheet-darwin-amd64"), nil, 0o755)
		}, "verify darwin/amd64: file: empty"},
		{"symlink", func(t *testing.T, dir string, _ *ReleaseDeps) {
			p := filepath.Join(dir, "callsheet-darwin-arm64")
			os.Rename(p, p+".real")
			os.Symlink(p+".real", p)
		}, "verify darwin/arm64: file: not a regular file"},
		{"directory", func(t *testing.T, dir string, _ *ReleaseDeps) {
			p := filepath.Join(dir, "callsheet-linux-amd64")
			os.Remove(p)
			os.Mkdir(p, 0o755)
		}, "verify linux/amd64: file: not a regular file"},
		{"wrong-target", func(t *testing.T, dir string, _ *ReleaseDeps) {
			writeSynthetic(t, filepath.Join(dir, "callsheet-linux-amd64"), Target{"linux", "arm64"})
		}, "verify linux/amd64: header: built for linux/arm64"},
		{"not-executable", func(t *testing.T, dir string, _ *ReleaseDeps) {
			os.WriteFile(filepath.Join(dir, "callsheet-linux-amd64"), []byte("#!/bin/sh\n"), 0o755)
		}, "verify linux/amd64: header:"},
		{"extra", func(t *testing.T, dir string, _ *ReleaseDeps) {
			os.WriteFile(filepath.Join(dir, "fake-adapter-linux-amd64"), []byte("x"), 0o755)
		}, "unexpected entry fake-adapter-linux-amd64"},
		{"stale-manifest", func(t *testing.T, dir string, _ *ReleaseDeps) {
			os.WriteFile(filepath.Join(dir, ReleaseManifest), []byte("x"), 0o644)
		}, "unexpected entry SHA256SUMS"},
		{"stamp-dev", func(_ *testing.T, _ string, d *ReleaseDeps) {
			d.Stamp = func(string) (string, error) { return "dev", nil }
		}, `stamp: stored version "dev"`},
		{"stamp-wrong", func(_ *testing.T, _ string, d *ReleaseDeps) {
			d.Stamp = func(string) (string, error) { return "v0.0.0-rc.2", nil }
		}, `stamp: stored version "v0.0.0-rc.2"`},
		{"stamp-superstring", func(_ *testing.T, _ string, d *ReleaseDeps) {
			d.Stamp = func(string) (string, error) { return releaseTestVersion + "-extra", nil }
		}, "stamp: stored version"},
		{"stamp-absent", func(_ *testing.T, _ string, d *ReleaseDeps) {
			d.Stamp = func(string) (string, error) { return "", errors.New("symbol is absent") }
		}, "stamp: symbol is absent"},
		{"settings-error", func(_ *testing.T, _ string, d *ReleaseDeps) {
			d.Settings = func(string) (string, map[string]string, error) { return "", nil, errors.New("no build info") }
		}, "build settings: no build info"},
		{"settings-untrimmed", func(_ *testing.T, _ string, d *ReleaseDeps) {
			inner := d.Settings
			d.Settings = func(p string) (string, map[string]string, error) {
				v, s, err := inner(p)
				delete(s, "-trimpath")
				return v, s, err
			}
		}, "setting -trimpath is absent"},
		{"settings-cgo", func(_ *testing.T, _ string, d *ReleaseDeps) {
			inner := d.Settings
			d.Settings = func(p string) (string, map[string]string, error) {
				v, s, err := inner(p)
				s["CGO_ENABLED"] = "1"
				return v, s, err
			}
		}, `setting CGO_ENABLED = "1"`},
		{"settings-target", func(_ *testing.T, _ string, d *ReleaseDeps) {
			d.Settings = func(string) (string, map[string]string, error) {
				return "go", map[string]string{"GOOS": "plan9", "GOARCH": "amd64", "CGO_ENABLED": "0", "-trimpath": "true"}, nil
			}
		}, `setting GOOS = "plan9"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, deps := writeReleaseProducts(t), okDeps
			c.mutate(t, dir, &deps)
			err := VerifyReleaseAndWriteManifest(deps, releaseTestVersion, dir, Matrix, io.Discard)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if st, err := os.Stat(filepath.Join(dir, ReleaseManifest)); err == nil && st.Size() != 1 {
				t.Fatal("a manifest was written")
			}
		})
	}
	t.Run("arguments", func(t *testing.T) {
		dir := writeReleaseProducts(t)
		for _, c := range []struct {
			version string
			targets []Target
			dir     string
		}{{"bad", Matrix, dir}, {releaseTestVersion, nil, dir}, {releaseTestVersion, []Target{{"plan9", "amd64"}}, dir}, {releaseTestVersion, Matrix, filepath.Join(dir, "absent")}} {
			if err := VerifyReleaseProducts(okDeps, c.version, c.dir, c.targets, io.Discard); err == nil {
				t.Errorf("verify(%q, %v, %q) passed", c.version, c.targets, c.dir)
			}
		}
	})
}

func TestCheckReleaseSettings(t *testing.T) {
	tg := Target{"darwin", "arm64"}
	good := map[string]string{"GOOS": "darwin", "GOARCH": "arm64", "CGO_ENABLED": "0", "-trimpath": "true"}
	if err := CheckReleaseSettings(tg, good); err != nil {
		t.Fatalf("absent -ldflags rejected: %v", err)
	}
	withStale := map[string]string{"GOOS": "darwin", "GOARCH": "arm64", "CGO_ENABLED": "0", "-trimpath": "true", "-ldflags": "-X something=else"}
	if err := CheckReleaseSettings(tg, withStale); err != nil {
		t.Fatalf("-ldflags interpreted: %v", err)
	}
	for key, val := range map[string]string{"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1", "-trimpath": "false"} {
		bad := map[string]string{"GOOS": "darwin", "GOARCH": "arm64", "CGO_ENABLED": "0", "-trimpath": "true"}
		bad[key] = val
		if err := CheckReleaseSettings(tg, bad); err == nil || !strings.Contains(err.Error(), "setting "+key+" = ") {
			t.Errorf("%s=%s: %v", key, val, err)
		}
		delete(bad, key)
		if err := CheckReleaseSettings(tg, bad); err == nil || !strings.Contains(err.Error(), "setting "+key+" is absent") {
			t.Errorf("%s absent: %v", key, err)
		}
	}
}

func TestReadBuildSettingsMalformed(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ReadBuildSettings(filepath.Join(dir, "absent")); err == nil {
		t.Error("absent file read")
	}
	p := filepath.Join(dir, "text")
	if err := os.WriteFile(p, []byte("not an executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadBuildSettings(p); err == nil {
		t.Error("text file read")
	}
	synthetic := filepath.Join(dir, "synthetic")
	writeSynthetic(t, synthetic, Target{"linux", "amd64"})
	if _, _, err := ReadBuildSettings(synthetic); err == nil {
		t.Error("a header-only file has no build info")
	}
}

// TestReadBuildSettingsOwnBinary exercises the projection on the running test
// binary, the one real Go executable available without compiling.
func TestReadBuildSettingsOwnBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	v, s, err := ReadBuildSettings(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, "go") || s["GOOS"] == "" || s["GOARCH"] == "" {
		t.Fatalf("go version %q, settings %v", v, s)
	}
}

func TestWriteReleaseManifest(t *testing.T) {
	dir := writeReleaseProducts(t)
	if err := WriteReleaseManifest(dir, Matrix); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ReleaseManifest))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != wantManifest(t, dir) {
		t.Fatalf("manifest = %q", b)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d", len(lines))
	}
	if got := dirNames(t, dir); len(got) != 5 {
		t.Fatalf("entries = %q", got)
	}
	// The manifest depends only on the bytes: a second write is identical.
	if err := WriteReleaseManifest(dir, []Target{Matrix[3], Matrix[0], Matrix[2], Matrix[1]}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(dir, ReleaseManifest))
	if !bytes.Equal(b, again) {
		t.Fatal("manifest depends on target order")
	}

	t.Run("read-error", func(t *testing.T) {
		dir := writeReleaseProducts(t)
		os.Remove(filepath.Join(dir, "callsheet-linux-amd64"))
		if err := WriteReleaseManifest(dir, Matrix); err == nil || !strings.Contains(err.Error(), "manifest callsheet-linux-amd64") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ReleaseManifest)); err == nil {
			t.Fatal("manifest written")
		}
	})
	t.Run("directory-product", func(t *testing.T) {
		dir := writeReleaseProducts(t)
		p := filepath.Join(dir, "callsheet-linux-amd64")
		os.Remove(p)
		os.Mkdir(p, 0o755)
		if err := WriteReleaseManifest(dir, Matrix); err == nil {
			t.Fatal("hashed a directory")
		}
	})
	t.Run("rename-error", func(t *testing.T) {
		dir := writeReleaseProducts(t)
		if err := os.Mkdir(filepath.Join(dir, ReleaseManifest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := WriteReleaseManifest(dir, Matrix); err == nil {
			t.Fatal("renamed over a directory")
		}
		if got := dirNames(t, dir); len(got) != 5 || slices.ContainsFunc(got, func(n string) bool { return strings.HasPrefix(n, ".manifest") }) {
			t.Fatalf("temporary left: %q", got)
		}
	})
	t.Run("create-error", func(t *testing.T) {
		dir := writeReleaseProducts(t)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		if err := WriteReleaseManifest(dir, Matrix); err == nil && os.Geteuid() != 0 {
			t.Fatal("wrote a manifest into a read-only directory")
		}
	})
}

func TestReleaseMainArguments(t *testing.T) {
	root := t.TempDir()
	fresh := func() string { return filepath.Join(t.TempDir(), "out") }
	calls := 0
	deps := releaseFakes(releaseTestVersion)
	inner := deps.Run
	deps.Run = func(ctx context.Context, argv, env []string, dir string, o, e io.Writer) error {
		calls++
		return inner(ctx, argv, env, dir, o, e)
	}
	for _, c := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"none", nil, 2, "--version is required"},
		{"no-out", []string{"--version", releaseTestVersion}, 2, "--out is required"},
		{"empty-out", []string{"--version", releaseTestVersion, "--out", ""}, 2, "--out is required"},
		{"no-version", []string{"--out", fresh()}, 2, "--version is required"},
		{"unknown", []string{"--version", releaseTestVersion, "--out", fresh(), "--publish"}, 2, "not defined"},
		{"repeated-version", []string{"--version", releaseTestVersion, "--version", releaseTestVersion, "--out", fresh()}, 2, "more than once"},
		{"repeated-out", []string{"--version", releaseTestVersion, "--out", fresh(), "--out", fresh()}, 2, "more than once"},
		{"operand", []string{"--version", releaseTestVersion, "--out", fresh(), "extra"}, 2, `unexpected operand "extra"`},
		{"bad-version", []string{"--version", "v1", "--out", fresh()}, 2, `invalid version "v1"`},
		{"existing-out", []string{"--version", releaseTestVersion, "--out", root}, 1, "already exists"},
	} {
		var out, errOut bytes.Buffer
		if code := ReleaseMain(context.Background(), c.args, &out, &errOut, deps); code != c.code || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%s = %d, %q", c.name, code, errOut.String())
		}
		if c.code == 2 && !strings.Contains(errOut.String(), releaseUsage) {
			t.Errorf("%s: no usage: %q", c.name, errOut.String())
		}
		if calls != 0 {
			t.Fatalf("%s: runner called", c.name)
		}
	}
	// The bad-version attempt created no output directory.
	bad := fresh()
	ReleaseMain(context.Background(), []string{"--version", "v1", "--out", bad}, io.Discard, io.Discard, deps)
	if _, err := os.Lstat(bad); err == nil {
		t.Fatal("invalid arguments created the output directory")
	}

	out := fresh()
	var stdout, stderr bytes.Buffer
	if code := ReleaseMain(context.Background(), []string{"--version", releaseTestVersion, "--out", out}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("success = %d, %s", code, stderr.String())
	}
	if calls != 4 || len(dirNames(t, out)) != 5 || !strings.Contains(stdout.String(), "wrote SHA256SUMS for "+releaseTestVersion) {
		t.Fatalf("calls %d, %q", calls, stdout.String())
	}

	// A verification failure exits 1, names the target and keeps the output.
	failing := deps
	failing.Stamp = func(string) (string, error) { return "dev", nil }
	out = fresh()
	stderr.Reset()
	if code := ReleaseMain(context.Background(), []string{"--version", releaseTestVersion, "--out", out}, io.Discard, &stderr, failing); code != 1 ||
		!strings.Contains(stderr.String(), "verify linux/amd64") || !strings.Contains(stderr.String(), "partial output retained") {
		t.Fatalf("verify failure = %d, %q", code, stderr.String())
	}
	if got := dirNames(t, out); len(got) != 4 {
		t.Fatalf("retained = %q", got)
	}
	if d := DefaultReleaseDeps(); d.Run == nil || d.Stamp == nil || d.Settings == nil {
		t.Fatal("incomplete default dependencies")
	}
}

// stampFixture builds a complete symbol-bearing executable for the real
// reader: a data section holding the 16-byte string header and the version
// bytes, a symbol table and string table. No compiler is involved.
type stampFixture struct {
	target  Target
	version string
	// name overrides the symbol name; drop omits the symbol; dup repeats it.
	name string
	drop bool
	dup  bool
	// elfSize overrides ELF st_size when set; useSize forces it to zero.
	elfSize uint64
	useSize bool
	// headerAddr and headerLen override the stored header when set.
	headerAddr, headerLen *uint64
	// symAddr overrides the symbol's value when set.
	symAddr *uint64
	// zeroFill makes the data section a no-contents section.
	zeroFill bool
	// debugSection adds a non-allocated, zero-address, file-backed section
	// larger than the data address (ELF), or a zero-address section (Mach-O).
	debugSection bool
	// duplicateData adds a second candidate section over the same addresses.
	duplicateData bool
	// truncate removes this many bytes from the end of the file.
	truncate int
	// noSymtab omits the symbol table.
	noSymtab bool
	// undefined marks the symbol undefined rather than section-backed.
	undefined bool
}

const stampDataAddr = 0x1000

func (f stampFixture) symbolName() string {
	if f.name != "" {
		return f.name
	}
	return ReleaseVersionSymbol
}

func (f stampFixture) dataBytes() []byte {
	addr, n := uint64(stampDataAddr+stringHeaderSize), uint64(len(f.version))
	if f.headerAddr != nil {
		addr = *f.headerAddr
	}
	if f.headerLen != nil {
		n = *f.headerLen
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, addr)
	binary.Write(&b, binary.LittleEndian, n)
	b.WriteString(f.version)
	return b.Bytes()
}

func (f stampFixture) symbolValue() uint64 {
	if f.symAddr != nil {
		return *f.symAddr
	}
	return stampDataAddr
}

func (f stampFixture) bytes(t *testing.T) []byte {
	t.Helper()
	var out []byte
	if f.target.GOOS == "linux" {
		out = f.elfBytes(t)
	} else {
		out = f.machoBytes(t)
	}
	return out[:len(out)-f.truncate]
}

func (f stampFixture) write(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "product")
	if err := os.WriteFile(p, f.bytes(t), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f stampFixture) elfBytes(t *testing.T) []byte {
	t.Helper()
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[f.target.GOARCH]
	const hdr, shent = 64, 64
	data := f.dataBytes()
	strtab := append([]byte{0}, append([]byte(f.symbolName()), 0)...)
	var symtab bytes.Buffer
	binary.Write(&symtab, binary.LittleEndian, elf.Sym64{})
	size := uint64(stringHeaderSize)
	if f.elfSize != 0 || f.useSize {
		size = f.elfSize
	}
	sect := uint16(1)
	if f.undefined {
		sect = uint16(elf.SHN_UNDEF)
	}
	sym := elf.Sym64{Name: 1, Info: elf.ST_INFO(elf.STB_LOCAL, elf.STT_OBJECT), Shndx: sect, Value: f.symbolValue(), Size: size}
	if !f.drop {
		binary.Write(&symtab, binary.LittleEndian, sym)
		if f.dup {
			binary.Write(&symtab, binary.LittleEndian, sym)
		}
	}
	var debug []byte
	if f.debugSection {
		debug = make([]byte, stampDataAddr*2)
	}
	shstr := []byte("\x00.data\x00.dup\x00.debug\x00.symtab\x00.strtab\x00.shstrtab\x00")
	nameAt := func(s string) uint32 { return uint32(bytes.Index(shstr, []byte("\x00"+s+"\x00"))) + 1 }

	var body bytes.Buffer
	body.Write(make([]byte, hdr))
	place := func(b []byte) uint64 {
		at := uint64(body.Len())
		body.Write(b)
		return at
	}
	dataType := elf.SHT_PROGBITS
	if f.zeroFill {
		dataType = elf.SHT_NOBITS
	}
	alloc := uint64(elf.SHF_ALLOC | elf.SHF_WRITE)
	secs := []elf.Section64{{}, {Name: nameAt(".data"), Type: uint32(dataType), Flags: alloc, Addr: stampDataAddr, Off: place(data), Size: uint64(len(data)), Addralign: 8}}
	if f.duplicateData {
		secs = append(secs, elf.Section64{Name: nameAt(".dup"), Type: uint32(elf.SHT_PROGBITS), Flags: alloc, Addr: stampDataAddr, Off: place(data), Size: uint64(len(data)), Addralign: 8})
	}
	if f.debugSection {
		secs = append(secs, elf.Section64{Name: nameAt(".debug"), Type: uint32(elf.SHT_PROGBITS), Off: place(debug), Size: uint64(len(debug)), Addralign: 1})
	}
	strIdx := uint32(len(secs) + 1)
	if !f.noSymtab {
		secs = append(secs, elf.Section64{Name: nameAt(".symtab"), Type: uint32(elf.SHT_SYMTAB), Off: place(symtab.Bytes()), Size: uint64(symtab.Len()), Link: strIdx, Info: 1, Addralign: 8, Entsize: 24})
		secs = append(secs, elf.Section64{Name: nameAt(".strtab"), Type: uint32(elf.SHT_STRTAB), Off: place(strtab), Size: uint64(len(strtab)), Addralign: 1})
	}
	secs = append(secs, elf.Section64{Name: nameAt(".shstrtab"), Type: uint32(elf.SHT_STRTAB), Off: place(shstr), Size: uint64(len(shstr)), Addralign: 1})
	shoff := uint64(body.Len())
	for _, s := range secs {
		binary.Write(&body, binary.LittleEndian, s)
	}
	h := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT), Shoff: shoff,
		Ehsize: hdr, Shentsize: shent, Shnum: uint16(len(secs)), Shstrndx: uint16(len(secs) - 1)}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	var head bytes.Buffer
	binary.Write(&head, binary.LittleEndian, h)
	out := body.Bytes()
	copy(out, head.Bytes())
	return out
}

func machoName(s string) (out [16]byte) {
	copy(out[:], s)
	return out
}

func (f stampFixture) machoBytes(t *testing.T) []byte {
	t.Helper()
	cpu := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[f.target.GOARCH]
	data := f.dataBytes()
	// The on-disk Mach-O name has the platform's leading underscore; debug/macho
	// removes it from names holding a dot, so the reader sees the exact Go name.
	strtab := append([]byte{0}, append([]byte("_"+f.symbolName()), 0)...)
	nsect := 1
	if f.duplicateData {
		nsect++
	}
	if f.debugSection {
		nsect++
	}
	segSize := binary.Size(macho.Segment64{}) + nsect*binary.Size(macho.Section64{})
	symtabSize := binary.Size(macho.SymtabCmd{})
	cmds := segSize
	ncmd := uint32(1)
	if !f.noSymtab {
		cmds += symtabSize
		ncmd++
	}
	const hdr = 32
	dataOff := uint32(hdr + cmds)
	sectFlags := uint32(0)
	if f.zeroFill {
		sectFlags = 0x1
	}
	var body bytes.Buffer
	binary.Write(&body, binary.LittleEndian, macho.FileHeader{Magic: macho.Magic64, Cpu: cpu, Type: macho.TypeExec, Ncmd: ncmd, Cmdsz: uint32(cmds)})
	body.Write(make([]byte, 4))
	binary.Write(&body, binary.LittleEndian, macho.Segment64{Cmd: macho.LoadCmdSegment64, Len: uint32(segSize), Name: machoName("__DATA"), Addr: stampDataAddr,
		Memsz: uint64(len(data)), Offset: uint64(dataOff), Filesz: uint64(len(data)), Nsect: uint32(nsect)})
	binary.Write(&body, binary.LittleEndian, macho.Section64{Name: machoName("__data"), Seg: machoName("__DATA"), Addr: stampDataAddr, Size: uint64(len(data)), Offset: dataOff, Flags: sectFlags})
	next := dataOff + uint32(len(data))
	var extra []byte
	if f.duplicateData {
		binary.Write(&body, binary.LittleEndian, macho.Section64{Name: machoName("__dup"), Seg: machoName("__DATA"), Addr: stampDataAddr, Size: uint64(len(data)), Offset: next})
		next += uint32(len(data))
		extra = append(extra, data...)
	}
	if f.debugSection {
		// A zero-address section is never a candidate, even if it would
		// otherwise contain the header address.
		debug := make([]byte, stampDataAddr*2)
		binary.Write(&body, binary.LittleEndian, macho.Section64{Name: machoName("__debug"), Seg: machoName("__DWARF"), Addr: 0, Size: uint64(len(debug)), Offset: next})
		next += uint32(len(debug))
		extra = append(extra, debug...)
	}
	if !f.noSymtab {
		var syms bytes.Buffer
		sect := uint8(1)
		typ := uint8(machoNSect)
		if f.undefined {
			sect, typ = 0, 0
		}
		sym := macho.Nlist64{Name: 1, Type: typ, Sect: sect, Value: f.symbolValue()}
		count := uint32(0)
		if !f.drop {
			binary.Write(&syms, binary.LittleEndian, sym)
			count++
			if f.dup {
				binary.Write(&syms, binary.LittleEndian, sym)
				count++
			}
		}
		binary.Write(&body, binary.LittleEndian, macho.SymtabCmd{Cmd: macho.LoadCmdSymtab, Len: uint32(symtabSize), Symoff: next, Nsyms: count,
			Stroff: next + uint32(syms.Len()), Strsize: uint32(len(strtab))})
		body.Write(data)
		body.Write(extra)
		body.Write(syms.Bytes())
		body.Write(strtab)
		return body.Bytes()
	}
	body.Write(data)
	body.Write(extra)
	return body.Bytes()
}

func u64(v uint64) *uint64 { return &v }

// TestReadReleaseStampFixtures: the real parser on complete fixtures of all
// four targets, each mutation rejected.
func TestReadReleaseStampFixtures(t *testing.T) {
	for _, tg := range Matrix {
		t.Run(tg.GOOS+"-"+tg.GOARCH, func(t *testing.T) {
			base := stampFixture{target: tg, version: releaseTestVersion}
			if got, err := ReadReleaseStamp(base.write(t)); err != nil || got != releaseTestVersion {
				t.Fatalf("stamp = %q, %v", got, err)
			}
			// A symbol with this name for another layout: the reader is exact.
			if got, err := ReadReleaseStamp(stampFixture{target: tg, version: "dev"}.write(t)); err != nil || got != "dev" {
				t.Fatalf("dev stamp = %q, %v", got, err)
			}
			// The debug section must not take part in the mapping.
			debug := base
			debug.debugSection = true
			if got, err := ReadReleaseStamp(debug.write(t)); err != nil || got != releaseTestVersion {
				t.Fatalf("with debug section: %q, %v", got, err)
			}
			for _, c := range []struct {
				name string
				mut  func(*stampFixture)
				want string
			}{
				{"wrong-name", func(f *stampFixture) { f.name = ReleaseVersionSymbol + "X" }, "is absent"},
				{"absent", func(f *stampFixture) { f.drop = true }, "is absent"},
				{"undefined", func(f *stampFixture) { f.undefined = true }, "is absent"},
				{"duplicate", func(f *stampFixture) { f.dup = true }, "defined 2 times"},
				{"zero-symbol-address", func(f *stampFixture) { f.symAddr = u64(0) }, "zero address"},
				{"symbol-outside", func(f *stampFixture) { f.symAddr = u64(stampDataAddr * 16) }, "not in a file-backed section"},
				{"header-runs-off-section", func(f *stampFixture) { f.symAddr = u64(stampDataAddr + 20) }, "exceeds its section"},
				{"zero-pointer", func(f *stampFixture) { f.headerAddr = u64(0) }, "zero address"},
				{"pointer-outside", func(f *stampFixture) { f.headerAddr = u64(stampDataAddr * 16) }, "not in a file-backed section"},
				{"empty-length", func(f *stampFixture) { f.headerLen = u64(0) }, "empty"},
				{"long-length", func(f *stampFixture) { f.headerLen = u64(1 << 20) }, "exceeds its section"},
				{"overflow-length", func(f *stampFixture) { f.headerLen = u64(^uint64(0)) }, "exceeds its section"},
				{"overflow-pointer", func(f *stampFixture) { f.headerAddr = u64(^uint64(0) - 1) }, "not in a file-backed section"},
				{"zero-fill", func(f *stampFixture) { f.zeroFill = true }, "not in a file-backed section"},
				{"duplicate-mapping", func(f *stampFixture) { f.duplicateData = true }, "maps through 2 sections"},
			} {
				t.Run(c.name, func(t *testing.T) {
					f := base
					c.mut(&f)
					if got, err := ReadReleaseStamp(f.write(t)); err == nil || !strings.Contains(err.Error(), c.want) {
						t.Fatalf("stamp = %q, err = %v, want %q", got, err, c.want)
					}
				})
			}
			t.Run("no-symbol-table", func(t *testing.T) {
				f := base
				f.noSymtab = true
				if got, err := ReadReleaseStamp(f.write(t)); err == nil {
					t.Fatalf("stamp = %q", got)
				}
			})
			t.Run("wrong-bytes", func(t *testing.T) {
				f := base
				f.version = "v0.0.0-rc.2"
				dir := t.TempDir()
				p := filepath.Join(dir, CallsheetArtifact(tg))
				if err := os.WriteFile(p, f.bytes(t), 0o755); err != nil {
					t.Fatal(err)
				}
				deps := ReleaseDeps{Stamp: ReadReleaseStamp, Settings: releaseFakes("").Settings}
				err := VerifyReleaseAndWriteManifest(deps, releaseTestVersion, dir, []Target{tg}, io.Discard)
				if err == nil || !strings.Contains(err.Error(), "verify "+tg.String()+": stamp:") {
					t.Fatalf("err = %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, ReleaseManifest)); err == nil {
					t.Fatal("manifest written")
				}
			})
			t.Run("truncated", func(t *testing.T) {
				f := base
				full := len(f.bytes(t))
				for _, keep := range []int{3, 4, 40, full / 2} {
					f.truncate = full - keep
					if got, err := ReadReleaseStamp(f.write(t)); err == nil {
						t.Errorf("kept %d of %d bytes: stamp = %q", keep, full, got)
					}
				}
			})
		})
	}
}

func TestReadReleaseStampELFSymbolSize(t *testing.T) {
	for _, tg := range Matrix[:2] {
		for _, c := range []struct {
			name string
			size uint64
		}{{"zero", 0}, {"eight", 8}, {"seventeen", 17}} {
			f := stampFixture{target: tg, version: releaseTestVersion, elfSize: c.size, useSize: true}
			if got, err := ReadReleaseStamp(f.write(t)); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("has size %d", c.size)) {
				t.Errorf("%s %s: %q, %v", tg, c.name, got, err)
			}
		}
	}
}

// TestReadReleaseStampFormats: other formats, word sizes and byte orders are
// refused by format, and a recognized malformed file never falls back.
func TestReadReleaseStampFormats(t *testing.T) {
	write := func(name string, b []byte) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := ReadReleaseStamp(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("absent file read")
	}
	for name, b := range map[string][]byte{
		"text":          []byte("#!/bin/sh\necho v0.0.0-rc.1\n"),
		"too-short":     []byte("ab"),
		"empty":         {},
		"macho-32":      {0xce, 0xfa, 0xed, 0xfe, 0, 0, 0, 0},
		"macho-big":     {0xfe, 0xed, 0xfa, 0xcf, 0, 0, 0, 0},
		"macho-fat":     {0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 0},
		"elf-malformed": []byte("\x7fELF\x02\x01"),
		"macho-short":   {0xcf, 0xfa, 0xed, 0xfe},
	} {
		if got, err := ReadReleaseStamp(write(name, b)); err == nil {
			t.Errorf("%s: stamp = %q", name, got)
		}
	}
	// A 32-bit ELF and a big-endian 64-bit ELF are recognized and unsupported.
	elfIdent := func(class elf.Class, data elf.Data) []byte {
		ident := make([]byte, 16)
		copy(ident, elf.ELFMAG)
		ident[elf.EI_CLASS], ident[elf.EI_DATA], ident[elf.EI_VERSION] = byte(class), byte(data), byte(elf.EV_CURRENT)
		var b bytes.Buffer
		b.Write(ident)
		var order binary.ByteOrder = binary.LittleEndian
		if data == elf.ELFDATA2MSB {
			order = binary.BigEndian
		}
		if class == elf.ELFCLASS32 {
			h := elf.Header32{Type: uint16(elf.ET_EXEC), Machine: uint16(elf.EM_386), Version: uint32(elf.EV_CURRENT), Ehsize: 52, Shentsize: 40}
			copy(h.Ident[:], ident)
			b.Reset()
			binary.Write(&b, order, h)
			return b.Bytes()
		}
		h := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT), Ehsize: 64, Shentsize: 64}
		copy(h.Ident[:], ident)
		b.Reset()
		binary.Write(&b, order, h)
		return b.Bytes()
	}
	for name, b := range map[string][]byte{"elf32": elfIdent(elf.ELFCLASS32, elf.ELFDATA2LSB), "elf-big": elfIdent(elf.ELFCLASS64, elf.ELFDATA2MSB)} {
		if _, err := ReadReleaseStamp(write(name, b)); err == nil || !strings.Contains(err.Error(), "unsupported ELF layout") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReleaseStampSectionBounds(t *testing.T) {
	if _, err := addStampSection(nil, "s", 1, 100, 50, 100); err == nil {
		t.Error("section past the end of the file accepted")
	}
	if _, err := addStampSection(nil, "s", 1, 1, 200, 100); err == nil {
		t.Error("section offset past the file accepted")
	}
	if _, err := addStampSection(nil, "s", 1, ^uint64(0), 1, 100); err == nil {
		t.Error("overflowing section size accepted")
	}
	got, err := addStampSection(nil, "s", 1, 50, 50, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("exact fit: %v, %v", got, err)
	}
}
