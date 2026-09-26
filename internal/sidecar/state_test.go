package sidecar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

const testID = "n_0123456789abcdef0123456789abcdef"

// TestSidecarPlatformContract is delegated from tests/function
// (TestNodePlatform/paths): the state path table for linux and darwin, on
// any host, with the plane's resolution rules. Do not rename or skip.
func TestSidecarPlatformContract(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]struct {
		override, home, xdg string
		want                string
		code                contract.Code
	}{
		"linux": {
			{"", "/home/u", "", "/home/u/.local/state/callsheet/sidecar", ""},
			{"", "/home/u", "/var/xdg", "/var/xdg/callsheet/sidecar", ""},
			{"", "/home/u", "/var/xdg/../x/", "/var/x/callsheet/sidecar", ""},
			{"", "", "/var/xdg", "/var/xdg/callsheet/sidecar", ""},
			{"", "/home/u", "rel/xdg", "", contract.CodeInvalidArgument},
			{"", "", "", "", contract.CodeInvalidArgument},
			{"", "relhome", "", "", contract.CodeInvalidArgument},
			{"/abs/./dir/", "", "rel", "/abs/dir", ""},
			{"rel/dir", "", "", filepath.Join(cwd, "rel/dir"), ""},
			{"~/x", "/home/u", "", filepath.Join(cwd, "~/x"), ""},
		},
		"darwin": {
			{"", "/Users/u", "", "/Users/u/Library/Application Support/callsheet/sidecar", ""},
			{"", "/Users/u", "/var/xdg", "/Users/u/Library/Application Support/callsheet/sidecar", ""},
			{"", "/Users/u", "rel/xdg", "/Users/u/Library/Application Support/callsheet/sidecar", ""},
			{"", "", "/var/xdg", "", contract.CodeInvalidArgument},
			{"", "rel", "", "", contract.CodeInvalidArgument},
			{"/abs/dir", "", "", "/abs/dir", ""},
			{"rel/dir", "/Users/u", "", filepath.Join(cwd, "rel/dir"), ""},
		},
	}
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			for _, c := range cases[goos] {
				got, err := ResolveStateDir(goos, c.override, c.home, c.xdg)
				if contract.CodeOf(err) != c.code || got != c.want {
					t.Errorf("ResolveStateDir(%q, %q, %q, %q) = %q, %v; want %q, %q", goos, c.override, c.home, c.xdg, got, err, c.want, c.code)
				}
			}
		})
	}
	for _, goos := range []string{"windows", "", "freebsd"} {
		if _, err := ResolveStateDir(goos, "", "/home/u", ""); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%q accepted", goos)
		}
	}
}

func TestStateCodecs(t *testing.T) {
	id := encode(identityFile{SchemaVersion: 1, NodeID: testID})
	if string(id) != "{\n  \"schema_version\": 1,\n  \"node_id\": \""+testID+"\"\n}\n" {
		t.Fatalf("identity bytes %q", id)
	}
	if got, err := parseIdentity(id); err != nil || got != testID {
		t.Fatalf("parse identity = %q %v", got, err)
	}
	ca, _ := testkit.NewFixtureCA()
	e := encode(enrollmentFile{SchemaVersion: 1, PlaneURL: "https://127.0.0.1:1", CAPEM: string(ca.CertPEM), CAFingerprint: "sha256:" + strings.Repeat("0", 64)})
	if !strings.HasPrefix(string(e), "{\n  \"schema_version\": 1,\n  \"plane_url\": \"https://127.0.0.1:1\",\n  \"ca_pem\": \"-----BEGIN CERTIFICATE-----\\n") || !strings.HasSuffix(string(e), "\"ca_fingerprint\": \"sha256:"+strings.Repeat("0", 64)+"\"\n}\n") {
		t.Fatalf("enrollment bytes %q", e)
	}
	if got, err := parseEnrollment(e); err != nil || got.PlaneURL != "https://127.0.0.1:1" {
		t.Fatalf("parse enrollment = %+v %v", got, err)
	}
	goodID := `{"schema_version":1,"node_id":"` + testID + `"}`
	for in, msg := range map[string]string{
		`[]`:           "not a JSON object",
		goodID + ` {}`: "trailing data",
		`{"schema_version":1,"schema_version":1}`:                     "duplicate key",
		`{"schema_version":1,"node_id":"` + testID + `","x":1}`:       `unknown field "x"`,
		`{"schema_version":1}`:                                        `"node_id" is required`,
		`{"schema_version":null,"node_id":"` + testID + `"}`:          "must not be null",
		`{"schema_version":2,"node_id":"` + testID + `"}`:             "unsupported schema_version 2",
		`{"schema_version":"1","node_id":"` + testID + `"}`:           "unsupported schema_version",
		`{"schema_version":1,"node_id":"host.example"}`:               "not a valid node ID",
		`{"schema_version":1,"node_id":7}`:                            `invalid "node_id"`,
		`{"schema_version":1,`:                                        "malformed JSON",
		`{"schema_version":1 "node_id"}`:                              "malformed JSON",
		`{"schema_version":1,"node_id":"` + testID + `"`:              "malformed JSON",
		`{"schema_version":1,"node_id":"` + testID + `",`:             "malformed JSON",
		`{"schema_version":1,"node_id":"` + testID + `":`:             "malformed JSON",
		`{"schema_version":[1,"node_id":"` + testID + `"}`:            "malformed JSON",
		`{"schema_version":1,"schema_version":2,"node_id":"` + testID: "duplicate key",
	} {
		if _, err := parseIdentity([]byte(in)); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("parseIdentity(%q) = %v, want %q", in, err, msg)
		}
	}
	for name, mut := range map[string]func(*enrollmentFile){
		"url":         func(e *enrollmentFile) { e.PlaneURL = "https://127.0.0.1:1/" },
		"http":        func(e *enrollmentFile) { e.PlaneURL = "http://x" },
		"fingerprint": func(e *enrollmentFile) { e.CAFingerprint = "sha256:x" },
	} {
		ef := enrollmentFile{SchemaVersion: 1, PlaneURL: "https://127.0.0.1:1", CAPEM: string(ca.CertPEM), CAFingerprint: "sha256:" + strings.Repeat("0", 64)}
		mut(&ef)
		if _, err := parseEnrollment(encode(ef)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := parseEnrollment([]byte(`{"schema_version":1}`)); err == nil {
		t.Error("partial enrollment accepted")
	}
	if _, err := parseEnrollment([]byte(`{"schema_version":1,"plane_url":1}`)); err == nil {
		t.Error("typed enrollment accepted")
	}
}

// TestStateScanAndLoad is UT-SidecarState's strict scan: modes, symlinks,
// unexpected entries, corrupt files and CA/fingerprint agreement, with
// nothing repaired.
func TestStateScanAndLoad(t *testing.T) {
	ca, _ := testkit.NewFixtureCA()
	fresh := func() string {
		root := filepath.Join(t.TempDir(), "sidecar")
		writeState(t, root, testID, "https://127.0.0.1:1", ca.CertPEM)
		return root
	}
	root := fresh()
	l := layout{root: root}
	s, err := l.scan()
	if err != nil || !s.rootExists || !s.hasIdentity || !s.hasEnrollment {
		t.Fatalf("scan = %+v %v", s, err)
	}
	if id, err := l.loadIdentity(); err != nil || id != testID {
		t.Fatalf("identity %q %v", id, err)
	}
	if _, err := l.loadEnrollment(); err != nil {
		t.Fatal(err)
	}
	// Regular unpublished temporaries are ignored; the lock is allowed.
	os.WriteFile(filepath.Join(root, tempPrefix+"x"), []byte("junk"), 0o600)
	os.WriteFile(filepath.Join(root, lockName), nil, 0o600)
	if _, err := l.scan(); err != nil {
		t.Fatalf("temporaries and lock: %v", err)
	}
	if s, err := (layout{root: filepath.Join(t.TempDir(), "none")}).scan(); err != nil || s.rootExists {
		t.Fatalf("missing root = %+v %v", s, err)
	}
	for _, c := range []struct {
		name  string
		setup func(root string)
		code  contract.Code
		msg   string
	}{
		{"root mode", func(r string) { os.Chmod(r, 0o750) }, contract.CodeTrustFailed, "mode 0750"},
		{"root file", func(r string) { os.RemoveAll(r); os.WriteFile(r, nil, 0o600) }, contract.CodeTrustFailed, "not a directory"},
		{"root symlink", func(r string) { real := r + ".real"; os.Rename(r, real); os.Symlink(real, r) }, contract.CodeTrustFailed, "symbolic link"},
		{"identity mode", func(r string) { os.Chmod(filepath.Join(r, identityName), 0o644) }, contract.CodeTrustFailed, "0644"},
		{"identity exec", func(r string) { os.Chmod(filepath.Join(r, identityName), 0o700) }, contract.CodeTrustFailed, "0700"},
		{"enrollment mode", func(r string) { os.Chmod(filepath.Join(r, enrollmentName), 0o640) }, contract.CodeTrustFailed, "0640"},
		{"identity symlink", func(r string) {
			p := filepath.Join(r, identityName)
			os.Rename(p, filepath.Join(t.TempDir(), "id"))
			os.Symlink("/nonexistent", p)
		}, contract.CodeTrustFailed, "symbolic link"},
		{"identity dir", func(r string) {
			p := filepath.Join(r, identityName)
			os.Remove(p)
			os.Mkdir(p, 0o700)
		}, contract.CodeTrustFailed, "not a regular file"},
		{"lock mode", func(r string) {
			os.WriteFile(filepath.Join(r, lockName), nil, 0o666)
			os.Chmod(filepath.Join(r, lockName), 0o666)
		}, contract.CodeTrustFailed, "writable by group or others"},
		{"unexpected", func(r string) { os.WriteFile(filepath.Join(r, "config.json"), nil, 0o600) }, contract.CodeConflict, "unexpected"},
		{"unexpected dir", func(r string) { os.Mkdir(filepath.Join(r, "pki"), 0o700) }, contract.CodeConflict, "unexpected"},
		{"temp dir", func(r string) { os.Mkdir(filepath.Join(r, tempPrefix+"d"), 0o700) }, contract.CodeConflict, "unexpected"},
	} {
		r := fresh()
		c.setup(r)
		_, err := layout{root: r}.scan()
		if contract.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %s %q", c.name, err, c.code, c.msg)
		}
	}
	for _, c := range []struct {
		name string
		file string
		data []byte
		code contract.Code
		msg  string
	}{
		{"identity corrupt", identityName, []byte("{"), contract.CodeConflict, "invalid sidecar state"},
		{"identity big", identityName, bytes.Repeat([]byte(" "), maxStateFile+1), contract.CodeConflict, "larger than 64 KiB"},
		{"enrollment corrupt", enrollmentName, []byte(`{"schema_version":3}`), contract.CodeConflict, "unsupported schema_version 3"},
		{"enrollment big", enrollmentName, bytes.Repeat([]byte(" "), maxStateFile+1), contract.CodeConflict, "larger than 64 KiB"},
		{"fingerprint", enrollmentName, encode(enrollmentFile{SchemaVersion: 1, PlaneURL: "https://127.0.0.1:1", CAPEM: string(ca.CertPEM), CAFingerprint: "sha256:" + strings.Repeat("0", 64)}), contract.CodeTrustFailed, "does not match its fingerprint"},
		{"ca pem", enrollmentName, encode(enrollmentFile{SchemaVersion: 1, PlaneURL: "https://127.0.0.1:1", CAPEM: "junk", CAFingerprint: "sha256:" + strings.Repeat("0", 64)}), contract.CodeTrustFailed, "invalid"},
	} {
		r := fresh()
		os.WriteFile(filepath.Join(r, c.file), c.data, 0o600)
		var err error
		if c.file == identityName {
			_, err = layout{root: r}.loadIdentity()
		} else {
			_, err = layout{root: r}.loadEnrollment()
		}
		if contract.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %s %q", c.name, err, c.code, c.msg)
		}
		if b, _ := os.ReadFile(filepath.Join(r, c.file)); !bytes.Equal(b, c.data) {
			t.Errorf("%s: state was repaired", c.name)
		}
	}
	if _, err := (layout{root: t.TempDir()}).readState(identityName); contract.CodeOf(err) != contract.CodeInternal {
		t.Fatalf("missing file = %v", err)
	}
}

func TestLockLifecycle(t *testing.T) {
	root := t.TempDir()
	l := layout{root: root}
	a, err := l.acquire()
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.acquire()
	wantCode(t, err, contract.CodeConflict, "in use by another callsheet process (sidecar run or enroll)")
	a.release()
	a.release()
	b, err := l.acquire()
	if err != nil {
		t.Fatal(err)
	}
	b.release()
	os.Chmod(l.path(lockName), 0o622)
	_, err = l.acquire()
	wantCode(t, err, contract.CodeTrustFailed, "writable by group or others")
	os.Remove(l.path(lockName))
	os.Symlink(filepath.Join(root, "elsewhere"), l.path(lockName))
	_, err = l.acquire()
	wantCode(t, err, contract.CodeTrustFailed, "symbolic link")
	_, err = layout{root: filepath.Join(root, "missing")}.acquire()
	wantCode(t, err, contract.CodeInternal, "cannot open lock file")
}

func TestDirSyncFallback(t *testing.T) {
	pathErr := func(errno syscall.Errno) error { return &fs.PathError{Op: "sync", Path: "dir", Err: errno} }
	for _, c := range []struct {
		first, raw error
		want       string
		rawCalls   int
	}{
		{nil, nil, "", 0},
		{pathErr(syscall.ENOTSUP), nil, "", 1},
		{pathErr(syscall.EINVAL), nil, "", 1},
		{pathErr(syscall.EIO), nil, "input/output error", 0},
		{pathErr(syscall.ENOTTY), syscall.EIO, "cannot sync the directory", 1},
	} {
		d := testDeps(nil)
		calls := 0
		d.fileSync = func(*os.File) error { return c.first }
		d.rawFsync = func(*os.File) error { calls++; return c.raw }
		err := d.syncDir(layout{root: t.TempDir()})
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) || calls != c.rawCalls {
			t.Fatalf("%v/%v: %v %d", c.first, c.raw, err, calls)
		}
	}
	if err := testDeps(nil).syncDir(layout{root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing directory synced")
	}
	f, _ := os.Open(t.TempDir())
	defer f.Close()
	if err := rawFsync(f); err != nil {
		t.Fatalf("native fsync: %v", err)
	}
}

// startLockHolder runs the lock helper on root and waits until it holds
// the lock; release closes its stdin, kill ends it.
func startLockHolder(t *testing.T, root string) (release, kill func()) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	// Under -race the helper is race-built; without atexit_sleep_ms=0 its
	// race runtime sleeps a second before exiting, which only slows the
	// release path (the lock semantics are unchanged).
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+root, "GORACE=atexit_sleep_ms=0")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var once sync.Once
	wait := func() {
		once.Do(func() {
			select {
			case <-done:
			case <-time.After(testWait):
				t.Error("lock helper not reaped")
			}
		})
	}
	if s := readLine(t, stdout); s != "locked\n" {
		t.Fatalf("lock helper: %q", s)
	}
	go func() { io.Copy(io.Discard, stdout); done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill(); wait() })
	return func() { stdin.Close(); wait() }, func() { cmd.Process.Kill(); wait() }
}

// TestSidecarNativeStateContract is delegated from tests/function
// (TestNodePlatform/native-state): real mode bits, kernel flock between
// processes and atomic old/new visibility on this host. Do not rename or
// skip its subtests.
func TestSidecarNativeStateContract(t *testing.T) {
	t.Run("modes", func(t *testing.T) {
		old := syscall.Umask(0)
		defer syscall.Umask(old)
		p := startPlane(t)
		d := testDeps(nil)
		root := filepath.Join(t.TempDir(), "a", "sidecar")
		if _, err := d.enroll(bg, EnrollOptions{StateDir: root, PlaneURL: p.url, CAFile: p.caFile, SoftwareVersion: "dev"}); err != nil {
			t.Fatal(err)
		}
		for rel, want := range map[string]os.FileMode{rootName: fs.ModeDir | 0o700, identityName: 0o600, enrollmentName: 0o600, lockName: 0o600} {
			fi, err := os.Lstat(layout{root: root}.path(rel))
			if err != nil || fi.Mode() != want {
				t.Fatalf("%s mode = %v, want %v (%v)", rel, fi.Mode(), want, err)
			}
		}
		if fi, _ := os.Stat(filepath.Dir(root)); fi.Mode().Perm() != 0o700 {
			t.Fatalf("created parent mode %v", fi.Mode())
		}
		entries, _ := os.ReadDir(root)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if strings.Join(names, " ") != ".lock enrollment.json identity.json" {
			t.Fatalf("root entries = %v", names)
		}
	})
	t.Run("flock", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "s")
		os.Mkdir(root, 0o700)
		l := layout{root: root}
		release, _ := startLockHolder(t, root)
		_, err := l.acquire()
		wantCode(t, err, contract.CodeConflict, "in use")
		// Enroll (after its trust check) and Run contend too, before any
		// state is read or written.
		d := testDeps(nil)
		d.resolveTrust = func(context.Context, client.TrustOptions) (client.Trust, error) { return client.Trust{}, nil }
		_, err = d.enroll(bg, EnrollOptions{StateDir: root, PlaneURL: "https://127.0.0.1:1", CAFile: "x", SoftwareVersion: "dev"})
		wantCode(t, err, contract.CodeConflict, "in use")
		wantCode(t, d.run(bg, RunOptions{StateDir: root, SoftwareVersion: "dev"}), contract.CodeConflict, "in use")
		if entries, _ := os.ReadDir(root); len(entries) != 1 {
			t.Fatalf("contending operations wrote state: %v", entries)
		}
		release()
		lk, err := l.acquire()
		if err != nil {
			t.Fatalf("after release: %v", err)
		}
		lk.release()
		_, kill := startLockHolder(t, root)
		_, err = l.acquire()
		wantCode(t, err, contract.CodeConflict)
		kill()
		lk, err = l.acquire()
		if err != nil {
			t.Fatalf("after kill: %v", err)
		}
		lk.release()
		if _, err := os.Stat(l.path(lockName)); err != nil {
			t.Fatal("lock file was unlinked")
		}
	})
	t.Run("atomic", func(t *testing.T) {
		// Concurrent readers only ever see a complete old or new
		// enrollment.json while it is replaced repeatedly.
		root := t.TempDir()
		ca, _ := testkit.NewFixtureCA()
		writeState(t, root, testID, "https://127.0.0.1:1", ca.CertPEM)
		l := layout{root: root}
		oldB, _ := os.ReadFile(l.path(enrollmentName))
		newE := enrollmentFile{SchemaVersion: 1, PlaneURL: "https://127.0.0.1:2", CAPEM: string(ca.CertPEM), CAFingerprint: "sha256:" + strings.Repeat("1", 64)}
		newB := encode(newE)
		d := testDeps(nil)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var bad []string
		var mu sync.Mutex
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					b, err := os.ReadFile(l.path(enrollmentName))
					if err != nil || (!bytes.Equal(b, oldB) && !bytes.Equal(b, newB)) {
						mu.Lock()
						bad = append(bad, string(b))
						mu.Unlock()
					}
				}
			}()
		}
		for i := range 20 {
			e := newE
			if i%2 == 1 {
				e, _ = parseEnrollment(oldB)
			}
			if err := d.writeEnrollment(l, e); err != nil {
				t.Fatal(err)
			}
		}
		close(stop)
		wg.Wait()
		if len(bad) != 0 {
			t.Fatalf("readers saw %d partial or missing files, e.g. %q", len(bad), bad[0])
		}
		if left, _ := filepath.Glob(filepath.Join(root, tempPrefix+"*")); len(left) != 0 {
			t.Fatalf("temporaries left: %v", left)
		}
		// No-replace publication never overwrites an identity.
		tmp, err := d.writeTemp(l, identityName, []byte("new"))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.publishNew(l, tmp, identityName); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("publish over identity = %v", err)
		}
		if id, err := l.loadIdentity(); err != nil || id != testID {
			t.Fatal("identity replaced")
		}
	})
}
