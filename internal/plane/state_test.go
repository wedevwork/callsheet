package plane

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

func TestResolveStateDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		goos, override, home, xdg string
		want                      string
		code                      contract.Code
	}{
		{"linux", "", "/home/u", "", "/home/u/.local/state/callsheet/plane", ""},
		{"linux", "", "/home/u", "/var/xdg", "/var/xdg/callsheet/plane", ""},
		{"linux", "", "/home/u", "/var/xdg/../x/", "/var/x/callsheet/plane", ""},
		{"linux", "", "", "/var/xdg", "/var/xdg/callsheet/plane", ""},
		{"linux", "", "/home/u", "rel/xdg", "", contract.CodeInvalidArgument},
		{"linux", "", "", "", "", contract.CodeInvalidArgument},
		{"linux", "", "relhome", "", "", contract.CodeInvalidArgument},
		{"darwin", "", "/Users/u", "", "/Users/u/Library/Application Support/callsheet/plane", ""},
		{"darwin", "", "/Users/u", "/var/xdg", "/Users/u/Library/Application Support/callsheet/plane", ""},
		{"darwin", "", "/Users/u", "rel/xdg", "/Users/u/Library/Application Support/callsheet/plane", ""},
		{"darwin", "", "", "/var/xdg", "", contract.CodeInvalidArgument},
		{"darwin", "", "rel", "", "", contract.CodeInvalidArgument},
		{"linux", "/abs/./dir/", "", "rel", "/abs/dir", ""},
		{"darwin", "/abs/dir", "", "", "/abs/dir", ""},
		{"linux", "rel/dir", "", "", filepath.Join(cwd, "rel/dir"), ""},
		{"darwin", "~/x", "/Users/u", "", filepath.Join(cwd, "~/x"), ""},
		{"windows", "", "/home/u", "", "", contract.CodeInvalidArgument},
		{"", "", "/home/u", "", "", contract.CodeInvalidArgument},
	} {
		got, err := ResolveStateDir(c.goos, c.override, c.home, c.xdg)
		if codeOf(err) != c.code || got != c.want {
			t.Errorf("ResolveStateDir(%q, %q, %q, %q) = %q, %v; want %q, %q", c.goos, c.override, c.home, c.xdg, got, err, c.want, c.code)
		}
	}
}

func TestConfigBytesAndParsing(t *testing.T) {
	def := "{\n  \"schema_version\": 1,\n  \"bind\": \"127.0.0.1:8443\"\n}\n"
	if got := string(encodeConfig(netip.MustParseAddrPort(DefaultBind))); got != def {
		t.Fatalf("default config = %q", got)
	}
	v6 := "{\n  \"schema_version\": 1,\n  \"bind\": \"[fd00::1]:8443\"\n}\n"
	if got := string(encodeConfig(netip.MustParseAddrPort("[fd00::1]:8443"))); got != v6 {
		t.Fatalf("ipv6 config = %q", got)
	}
	for _, ok := range []string{
		def,
		`{"bind":"127.0.0.1:0","schema_version":1}`,
		" \t\r\n{ \"schema_version\" : 1 , \"bind\" : \"[fd00::1]:9\" } \n\n",
	} {
		if _, err := parseConfig([]byte(ok)); err != nil {
			t.Errorf("parseConfig(%q) = %v", ok, err)
		}
	}
	for in, want := range map[string]string{
		``:             "not a JSON object",
		`[]`:           "not a JSON object",
		"\ufeff" + def: "not a JSON object",
		`{"schema_version":1,"bind":"127.0.0.1:1","x":1}`:              `unknown field "x"`,
		`{"schema_version":1,"schema_version":1,"bind":"127.0.0.1:1"}`: `duplicate key "schema_version"`,
		`{"schema_version":1,"bind":"127.0.0.1:1"}{}`:                  "trailing data",
		`{"schema_version":1,"bind":"127.0.0.1:1"} x`:                  "trailing data",
		`{"schema_version":2,"bind":"127.0.0.1:1"}`:                    "unsupported schema_version 2",
		`{"schema_version":1.0,"bind":"127.0.0.1:1"}`:                  "unsupported schema_version",
		`{"schema_version":"1","bind":"127.0.0.1:1"}`:                  `invalid "schema_version"`,
		`{"bind":"127.0.0.1:1"}`:                                       "required",
		`{"schema_version":1}`:                                         "required",
		`{"schema_version":1,"bind":7}`:                                `invalid "bind"`,
		`{"schema_version":1,"bind":"0.0.0.0:1"}`:                      "wildcard",
		`{"schema_version":1,"bind":"localhost:1"}`:                    "not a literal IP",
		`{"schema_version":1,"bind":"127.0.0.1:1"`:                     "malformed JSON",
		`{"schema_version":1,,}`:                                       "malformed JSON",
	} {
		_, err := parseConfig([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseConfig(%q) = %v, want %q", in, err, want)
		}
	}
}

func TestConfigSizeBound(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	mustInit(t, d, root, "localhost")
	p := layout{root: root}.path(configName)
	big := append(encodeConfig(netip.MustParseAddrPort("127.0.0.1:0")), bytes.Repeat([]byte(" "), maxFileSize)...)
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := d.inspect(bg, root)
	wantCode(t, err, contract.CodeConflict, "larger than 64 KiB")
	// Exactly at the bound with valid content is accepted.
	cfg := encodeConfig(netip.MustParseAddrPort("127.0.0.1:0"))
	exact := append(cfg, bytes.Repeat([]byte(" "), maxFileSize-len(cfg))...)
	if len(exact) != maxFileSize {
		t.Fatal(len(exact))
	}
	os.WriteFile(p, exact, 0o600)
	if _, err := d.inspect(bg, root); err != nil {
		t.Fatalf("64 KiB config: %v", err)
	}
	// Other whitespace is accepted and never reformatted by repeat init.
	before, _ := os.ReadFile(p)
	mustInit(t, d, root, "localhost")
	if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
		t.Fatal("repeat init reformatted config.json")
	}
}

func TestStateClassification(t *testing.T) {
	d := testDeps(t)
	// Missing root: empty, not found for status and reissue, nothing created.
	root := newRoot(t)
	_, err := d.inspect(bg, root)
	wantCode(t, err, contract.CodeNotFound, "run callsheet plane init")
	_, err = d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"a"}})
	wantCode(t, err, contract.CodeNotFound)
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("status/reissue created the root")
	}
	// New state requires SANs; nothing is created.
	_, err = d.init(bg, InitOptions{StateDir: root})
	wantCode(t, err, contract.CodeInvalidArgument, "at least one --san")
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("init without SANs created the root")
	}
	// A root holding anything but the managed names (here unrelated.txt)
	// is not empty initialization state: every operation refuses with
	// conflict and init publishes no credential beside the file.
	os.MkdirAll(filepath.Join(root, "pki"), 0o700)
	os.MkdirAll(filepath.Join(root, "tmp"), 0o700)
	os.WriteFile(filepath.Join(root, ".lock"), nil, 0o600)
	os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("x"), 0o644)
	unrelated := filepath.Join(root, "unrelated.txt")
	_, err = d.inspect(bg, root)
	wantCode(t, err, contract.CodeConflict, "unexpected", unrelated)
	_, err = d.init(bg, initOpts(root, "localhost"))
	wantCode(t, err, contract.CodeConflict, "unexpected", unrelated)
	err = d.run(bg, RunOptions{StateDir: root, SANs: []string{"localhost"}, SANsSet: true})
	wantCode(t, err, contract.CodeConflict, "unexpected", unrelated)
	_, err = d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"localhost"}})
	wantCode(t, err, contract.CodeConflict, "unexpected", unrelated)
	for rel, b := range snapshot(t, root) {
		if b != nil {
			t.Fatalf("%s was published beside an unrelated file", rel)
		}
	}
	if left := tempLeftovers(t, root); len(left) != 0 {
		t.Fatalf("temporaries left: %v", left)
	}
	// Only empty managed dirs, the lock and .tmp- leftovers (in the root,
	// pki/ and tmp/) are empty state.
	os.Remove(unrelated)
	os.WriteFile(filepath.Join(root, tempPrefix+"config.json-1"), []byte("junk"), 0o600)
	os.WriteFile(filepath.Join(root, "pki", tempPrefix+"ca.crt-1"), []byte("junk"), 0o600)
	os.WriteFile(filepath.Join(root, "tmp", tempPrefix+"x"), []byte("junk"), 0o600)
	_, err = d.inspect(bg, root)
	wantCode(t, err, contract.CodeNotFound)
	_, err = d.init(bg, InitOptions{StateDir: root})
	wantCode(t, err, contract.CodeInvalidArgument, "at least one --san")
	st := mustInit(t, d, root, "localhost")
	if st.StateDir != root {
		t.Fatalf("state dir %q", st.StateDir)
	}
	// Unexpected entries inside managed directories are not empty state.
	root2 := newRoot(t)
	os.MkdirAll(filepath.Join(root2, "tmp"), 0o700)
	os.WriteFile(filepath.Join(root2, "tmp", "stray"), nil, 0o600)
	_, err = d.init(bg, initOpts(root2, "localhost"))
	wantCode(t, err, contract.CodeConflict, "unexpected", "stray")
	// Complete and partial trees with an unexpected entry are refused too,
	// in the root or inside pki/ or tmp/, and nothing is rewritten.
	for _, rel := range []string{"pki/stray", "tmp/stray", "unrelated.txt", "pki/sub/"} {
		for _, partial := range []bool{false, true} {
			r := newRoot(t)
			mustInit(t, d, r, "localhost")
			if partial {
				os.Rename(layout{root: r}.path(serverKeyName), filepath.Join(t.TempDir(), "server.key"))
			}
			p := layout{root: r}.path(strings.TrimSuffix(rel, "/"))
			if strings.HasSuffix(rel, "/") {
				os.Mkdir(p, 0o700)
			} else {
				os.WriteFile(p, []byte("x"), 0o600)
			}
			before := snapshot(t, r)
			_, err := d.inspect(bg, r)
			wantCode(t, err, contract.CodeConflict, "unexpected", p)
			_, err = d.init(bg, InitOptions{StateDir: r})
			wantCode(t, err, contract.CodeConflict, "unexpected", p)
			err = d.run(bg, RunOptions{StateDir: r})
			wantCode(t, err, contract.CodeConflict, "unexpected", p)
			_, err = d.reissue(bg, ReissueOptions{StateDir: r, SANs: []string{"x"}})
			wantCode(t, err, contract.CodeConflict, "unexpected", p)
			sameSnapshot(t, before, snapshot(t, r))
		}
	}
}

// TestAllPartialSubsets refuses every strict nonempty subset of the five
// durable files for init, run, reissue and status, naming the missing paths
// and replacing nothing.
func TestAllPartialSubsets(t *testing.T) {
	d := testDeps(t)
	src := newRoot(t)
	mustInit(t, d, src, "localhost")
	full := snapshot(t, src)
	for mask := 1; mask < 1<<len(durable)-1; mask++ {
		root := newRoot(t)
		os.MkdirAll(filepath.Join(root, "pki"), 0o700)
		var present, missing []string
		for i, rel := range durable {
			if mask&(1<<i) != 0 {
				os.WriteFile(layout{root: root}.path(rel), full[rel], 0o600)
				present = append(present, rel)
			} else {
				missing = append(missing, layout{root: root}.path(rel))
			}
		}
		before := snapshot(t, root)
		for name, op := range map[string]func() error{
			"init":    func() error { _, err := d.init(bg, InitOptions{StateDir: root}); return err },
			"init+":   func() error { _, err := d.init(bg, initOpts(root, "localhost")); return err },
			"run":     func() error { return d.run(bg, RunOptions{StateDir: root, SANs: []string{"localhost"}, SANsSet: true}) },
			"reissue": func() error { _, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"x"}}); return err },
			"status":  func() error { _, err := d.inspect(bg, root); return err },
		} {
			err := op()
			wantCode(t, err, contract.CodeConflict, append([]string{"incomplete", "restore a complete stopped backup"}, missing...)...)
			if name == "status" && strings.Contains(err.Error(), ".lock") {
				t.Fatal("status mentions a lock")
			}
		}
		sameSnapshot(t, before, snapshot(t, root))
		for _, rel := range present {
			if !bytes.Equal(before[rel], full[rel]) {
				t.Fatalf("%v: %s replaced", present, rel)
			}
		}
		if left := tempLeftovers(t, root); len(left) != 0 {
			t.Fatalf("temporaries left: %v", left)
		}
	}
}

func TestLockLifecycle(t *testing.T) {
	root := newRoot(t)
	os.MkdirAll(root, 0o700)
	l := layout{root: root}
	a, err := l.acquire()
	if err != nil {
		t.Fatal(err)
	}
	// A second descriptor in the same process contends too (flock is per
	// open file description).
	_, err = l.acquire()
	wantCode(t, err, contract.CodeConflict, "in use by another callsheet process")
	a.release()
	a.release() // idempotent
	b, err := l.acquire()
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	b.release()
	if fi, err := os.Lstat(l.path(lockName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file persists 0600: %v %v", fi, err)
	}
	// Unsafe lock files are rejected before locking.
	os.Chmod(l.path(lockName), 0o622)
	_, err = l.acquire()
	wantCode(t, err, contract.CodeTrustFailed, "writable by group or others")
	os.Remove(l.path(lockName))
	os.Symlink(filepath.Join(root, "elsewhere"), l.path(lockName))
	_, err = l.acquire()
	wantCode(t, err, contract.CodeTrustFailed, "symbolic link")
	// A missing root cannot hold a lock.
	_, err = layout{root: filepath.Join(root, "missing")}.acquire()
	wantCode(t, err, contract.CodeInternal, "cannot open lock file")
	// Operations contend for the lock held by run and write nothing.
	d := testDeps(t)
	sroot := newRoot(t)
	mustInit(t, d, sroot, "localhost")
	before := snapshot(t, sroot)
	s := serveBG(t, d, RunOptions{StateDir: sroot})
	_, err = d.init(bg, InitOptions{StateDir: sroot})
	wantCode(t, err, contract.CodeConflict, "in use")
	_, err = d.reissue(bg, ReissueOptions{StateDir: sroot, SANs: []string{"new"}})
	wantCode(t, err, contract.CodeConflict, "in use")
	err = testDeps(t).run(bg, RunOptions{StateDir: sroot})
	wantCode(t, err, contract.CodeConflict, "in use")
	if _, err := d.inspect(bg, sroot); err != nil {
		t.Fatalf("status must not lock: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, sroot))
	s.stop(t)
	if _, err := d.reissue(bg, ReissueOptions{StateDir: sroot, SANs: []string{"new"}}); err != nil {
		t.Fatalf("reissue after stop: %v", err)
	}
}

func TestModesAndPaths(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	mustInit(t, d, root, "localhost")
	l := layout{root: root}
	restore := snapshot(t, root)
	check := func(name string, wantErr contract.Code, substr string) {
		t.Helper()
		_, err := d.inspect(bg, root)
		if wantErr == "" {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return
		}
		wantCode(t, err, wantErr, substr)
		if _, err := d.init(bg, InitOptions{StateDir: root}); codeOf(err) != wantErr {
			t.Fatalf("%s: init = %v", name, err)
		}
	}
	for _, c := range []struct {
		rel  string
		mode os.FileMode
		code contract.Code
	}{
		{rootName, 0o750, contract.CodeTrustFailed}, {rootName, 0o701, contract.CodeTrustFailed}, {rootName, 0o700, ""},
		{pkiName, 0o705, contract.CodeTrustFailed}, {tmpName, 0o770, contract.CodeTrustFailed},
		{caKeyName, 0o640, contract.CodeTrustFailed}, {caKeyName, 0o604, contract.CodeTrustFailed}, {caKeyName, 0o700, contract.CodeTrustFailed},
		{caKeyName, 0o200, contract.CodeTrustFailed}, {serverKeyName, 0o440, contract.CodeTrustFailed}, {serverKeyName, 0o400, ""}, {caKeyName, 0o600, ""},
		{caCertName, 0o620, contract.CodeTrustFailed}, {caCertName, 0o644, ""}, {serverCertName, 0o602, contract.CodeTrustFailed},
		{configName, 0o666, contract.CodeTrustFailed}, {configName, 0o644, ""}, {lockName, 0o660, contract.CodeTrustFailed}, {lockName, 0o644, ""},
	} {
		p := l.path(c.rel)
		fi, _ := os.Lstat(p)
		old := fi.Mode().Perm()
		if err := os.Chmod(p, c.mode); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprintf("%s %04o", c.rel, c.mode), c.code, fmt.Sprintf("%04o", c.mode))
		os.Chmod(p, old)
	}
	sameSnapshot(t, restore, snapshot(t, root))
	// Symlinked root and managed descendants, and nonregular files.
	for _, rel := range []string{caCertName, caKeyName, configName} {
		p := l.path(rel)
		os.Rename(p, p+".real")
		os.Symlink(p+".real", p)
		check("symlink "+rel, contract.CodeTrustFailed, "symbolic link")
		os.Remove(p)
		os.Rename(p+".real", p)
	}
	os.Rename(l.path(pkiName), l.path("pki.real"))
	os.Symlink(l.path("pki.real"), l.path(pkiName))
	check("symlink pki", contract.CodeTrustFailed, "symbolic link")
	os.Remove(l.path(pkiName))
	os.Rename(l.path("pki.real"), l.path(pkiName))
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(root, link)
	if _, err := d.inspect(bg, link); codeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("symlink root: %v", err)
	}
	os.Rename(l.path(serverCertName), l.path("pki/server.crt.real"))
	os.Mkdir(l.path(serverCertName), 0o700)
	check("dir server.crt", contract.CodeTrustFailed, "not a regular file")
	os.Remove(l.path(serverCertName))
	os.Rename(l.path("pki/server.crt.real"), l.path(serverCertName))
	os.Rename(l.path(tmpName), l.path("tmp.real"))
	os.WriteFile(l.path(tmpName), nil, 0o600)
	check("file tmp", contract.CodeTrustFailed, "not a directory")
	os.Remove(l.path(tmpName))
	os.Rename(l.path("tmp.real"), l.path(tmpName))
	check("restored", "", "")
	sameSnapshot(t, restore, snapshot(t, root))
	// A regular file as the root.
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	if _, err := d.inspect(bg, f); codeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("file root: %v", err)
	}
	// Newly created intermediate parents use 0700; existing ancestors are
	// not changed.
	parent := t.TempDir()
	os.Chmod(parent, 0o755)
	deep := filepath.Join(parent, "a", "b", "state")
	mustInit(t, d, deep, "localhost")
	for _, p := range []string{filepath.Join(parent, "a"), filepath.Join(parent, "a", "b"), deep, filepath.Join(deep, "pki"), filepath.Join(deep, "tmp")} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v %v", p, fi.Mode(), err)
		}
	}
	if fi, _ := os.Stat(parent); fi.Mode().Perm() != 0o755 {
		t.Fatal("ancestor mode changed")
	}
}

// TestStateFailureContract is delegated from tests/function
// (TestPlaneState/validation). Do not rename or skip its subtests.
func TestStateFailureContract(t *testing.T) {
	// Every failure before publication leaves no durable file and no
	// temporary; a clean retry then initializes normally.
	for _, op := range []string{"create", "write", "sync", "close"} {
		t.Run(op, func(t *testing.T) {
			for _, rel := range durable {
				d := testDeps(t)
				root := newRoot(t)
				d.fail = failAt(op, rel)
				st, err := d.init(bg, initOpts(root, "localhost"))
				wantCode(t, err, contract.CodeInternal, "before any state file was published", "injected "+op)
				if st.CAFingerprint != "" {
					t.Fatal("failure returned a report")
				}
				for _, b := range snapshot(t, root) {
					if b != nil {
						t.Fatalf("%s %s published a file", op, rel)
					}
				}
				if left := tempLeftovers(t, root); len(left) != 0 {
					t.Fatalf("%s %s left %v", op, rel, left)
				}
				d.fail = nil
				mustInit(t, d, root, "localhost")
			}
		})
	}
	t.Run("publish", func(t *testing.T) {
		for i, rel := range durable {
			d := testDeps(t)
			root := newRoot(t)
			d.fail = failAt("publish", rel)
			_, err := d.init(bg, initOpts(root, "localhost"))
			wantCode(t, err, contract.CodeInternal, "publishing "+rel, "Preserve the directory", "never rolled back")
			snap := snapshot(t, root)
			for j, r := range durable {
				if (snap[r] != nil) != (j < i) {
					t.Fatalf("publish failure at %s: %s present=%v", rel, r, snap[r] != nil)
				}
			}
			if left := tempLeftovers(t, root); len(left) != 0 {
				t.Fatalf("temporaries left: %v", left)
			}
			d.fail = nil
			if i == 0 {
				mustInit(t, d, root, "localhost")
				continue
			}
			// The partial state is refused and the CA is never replaced.
			_, err = d.init(bg, initOpts(root, "localhost"))
			wantCode(t, err, contract.CodeConflict, "incomplete")
			sameSnapshot(t, snap, snapshot(t, root))
		}
		// Directory sync failures: after pki/ publication (partial) and
		// after config.json (complete, with the same CA on retry).
		for _, c := range []struct{ rel, step string }{{pkiName, "syncing pki/"}, {rootName, "syncing the state directory"}} {
			d := testDeps(t)
			root := newRoot(t)
			d.fail = failAt("dirsync", c.rel)
			_, err := d.init(bg, initOpts(root, "localhost"))
			wantCode(t, err, contract.CodeInternal, c.step, "injected dirsync")
			snap := snapshot(t, root)
			d.fail = nil
			_, err = d.init(bg, initOpts(root, "localhost"))
			if c.rel == pkiName {
				wantCode(t, err, contract.CodeConflict, "incomplete")
			} else if err != nil {
				t.Fatalf("complete state after root sync failure: %v", err)
			}
			sameSnapshot(t, snap, snapshot(t, root))
		}
	})
	t.Run("partial", func(t *testing.T) {
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		full := snapshot(t, root)
		aside := t.TempDir() // outside the root: a parked file there would be an unexpected entry
		for _, rel := range durable {
			p := layout{root: root}.path(rel)
			moved := filepath.Join(aside, filepath.Base(rel))
			os.Rename(p, moved)
			before := snapshot(t, root)
			for _, err := range []error{
				func() error { _, err := d.init(bg, initOpts(root, "localhost")); return err }(),
				d.run(bg, RunOptions{StateDir: root}),
				func() error { _, err := d.inspect(bg, root); return err }(),
				func() error { _, err := d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"x"}}); return err }(),
			} {
				wantCode(t, err, contract.CodeConflict, "incomplete", p)
			}
			sameSnapshot(t, before, snapshot(t, root))
			os.Rename(moved, p)
		}
		sameSnapshot(t, full, snapshot(t, root))
	})
	t.Run("corrupt", func(t *testing.T) {
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		other := newRoot(t)
		mustInit(t, d, other, "localhost")
		full, alien := snapshot(t, root), snapshot(t, other)
		certPEMBytes := full[caCertName]
		for _, c := range []struct {
			rel  string
			data []byte
			code contract.Code
			msg  string
		}{
			{configName, []byte("{"), contract.CodeConflict, "invalid plane configuration"},
			{configName, []byte(`{"schema_version":9,"bind":"127.0.0.1:0"}`), contract.CodeConflict, "unsupported schema_version"},
			{caCertName, []byte("garbage"), contract.CodeTrustFailed, "no PEM block"},
			{caCertName, append(append([]byte{}, certPEMBytes...), certPEMBytes...), contract.CodeTrustFailed, "extra PEM blocks"},
			{caCertName, append([]byte("junk\n"), certPEMBytes...), contract.CodeTrustFailed, "data before the PEM block"},
			{caCertName, alien[caCertName], contract.CodeTrustFailed, "not issued by ca.crt"},
			{serverCertName, alien[serverCertName], contract.CodeTrustFailed, "not issued by ca.crt"},
			{serverCertName, full[caCertName], contract.CodeTrustFailed, "must not be a CA"},
			{caCertName, full[serverCertName], contract.CodeTrustFailed, "not a CA certificate"},
			{caKeyName, alien[caKeyName], contract.CodeTrustFailed, "ca.key does not match ca.crt"},
			{serverKeyName, alien[serverKeyName], contract.CodeTrustFailed, "server.key does not match server.crt"},
			{caKeyName, full[caCertName], contract.CodeTrustFailed, `PEM block type "CERTIFICATE"`},
			{serverKeyName, []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), contract.CodeTrustFailed, "not a PKCS#8 private key"},
			{caKeyName, bytes.Repeat([]byte("A"), maxFileSize+1), contract.CodeTrustFailed, "larger than 64 KiB"},
		} {
			p := layout{root: root}.path(c.rel)
			os.WriteFile(p, c.data, 0o600)
			before := snapshot(t, root)
			_, err := d.init(bg, InitOptions{StateDir: root})
			wantCode(t, err, c.code, c.msg)
			if strings.Contains(err.Error(), "-----") || strings.Contains(err.Error(), string(full[caKeyName][28:60])) {
				t.Fatalf("diagnostic leaks PEM: %v", err)
			}
			err = d.run(bg, RunOptions{StateDir: root})
			wantCode(t, err, c.code)
			sameSnapshot(t, before, snapshot(t, root))
			os.WriteFile(p, full[c.rel], 0o600)
		}
		sameSnapshot(t, full, snapshot(t, root))
	})
}

// TestNativeStateContract is delegated from tests/function
// (TestPlaneState/persistence). It uses real native mode bits, hard-link
// publication, rename and kernel flock between processes.
func TestNativeStateContract(t *testing.T) {
	t.Run("modes", func(t *testing.T) {
		old := syscall.Umask(0)
		defer syscall.Umask(old)
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		l := layout{root: root}
		for rel, want := range map[string]os.FileMode{
			rootName: 0o700 | fs.ModeDir, pkiName: 0o700 | fs.ModeDir, tmpName: 0o700 | fs.ModeDir,
			configName: 0o600, caCertName: 0o600, caKeyName: 0o600, serverCertName: 0o600, serverKeyName: 0o600, lockName: 0o600,
		} {
			fi, err := os.Lstat(l.path(rel))
			if err != nil || fi.Mode() != want {
				t.Fatalf("%s mode = %v, want %v (%v)", rel, fi.Mode(), want, err)
			}
		}
		entries, _ := os.ReadDir(root)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if strings.Join(names, " ") != ".lock config.json pki tmp" {
			t.Fatalf("root entries = %v", names)
		}
		os.Chmod(l.path(caKeyName), 0o644)
		_, err := d.inspect(bg, root)
		wantCode(t, err, contract.CodeTrustFailed, "0644")
		if fi, _ := os.Stat(l.path(caKeyName)); fi.Mode().Perm() != 0o644 {
			t.Fatal("permissions were repaired automatically")
		}
	})
	t.Run("no-replace", func(t *testing.T) {
		d := testDeps(t)
		root := newRoot(t)
		l := layout{root: root}
		os.MkdirAll(l.path(pkiName), 0o700)
		os.WriteFile(l.path(caCertName), []byte("existing"), 0o600)
		tmp, err := d.writeTemp(l, caCertName, []byte("new"))
		if err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(tmp); fi.Mode().Perm() != 0o600 || !strings.HasPrefix(filepath.Base(tmp), tempPrefix) {
			t.Fatalf("temporary %s mode %v", tmp, fi.Mode())
		}
		err = d.publishNew(l, tmp, caCertName)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("publish over existing = %v", err)
		}
		if b, _ := os.ReadFile(l.path(caCertName)); string(b) != "existing" {
			t.Fatal("existing destination replaced")
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatal("temporary not removed")
		}
		// Initialization over a preexisting CA refuses (partial state).
		_, err = d.init(bg, initOpts(root, "localhost"))
		wantCode(t, err, contract.CodeConflict, "incomplete")
		if b, _ := os.ReadFile(l.path(caCertName)); string(b) != "existing" {
			t.Fatal("init replaced a preexisting CA")
		}
	})
	t.Run("rename", func(t *testing.T) {
		d := testDeps(t)
		root := newRoot(t)
		mustInit(t, d, root, "localhost")
		l := layout{root: root}
		before := snapshot(t, root)
		oldInfo, _ := os.Stat(l.path(serverCertName))
		if err := d.replaceServerCert(l, []byte("replacement")); err != nil {
			t.Fatal(err)
		}
		newInfo, _ := os.Stat(l.path(serverCertName))
		if b, _ := os.ReadFile(l.path(serverCertName)); string(b) != "replacement" || os.SameFile(oldInfo, newInfo) || newInfo.Mode().Perm() != 0o600 {
			t.Fatalf("rename did not replace atomically: %q", b)
		}
		sameSnapshot(t, before, snapshot(t, root), serverCertName)
		if left := tempLeftovers(t, root); len(left) != 0 {
			t.Fatalf("temporaries left: %v", left)
		}
	})
	t.Run("flock", func(t *testing.T) {
		root := newRoot(t)
		os.MkdirAll(root, 0o700)
		l := layout{root: root}
		h := startLockHolder(t, root)
		_, err := l.acquire()
		wantCode(t, err, contract.CodeConflict, "in use by another callsheet process")
		h.release(t)
		lk, err := l.acquire()
		if err != nil {
			t.Fatalf("after release: %v", err)
		}
		lk.release()
		// A killed holder's lock is released by the kernel.
		h = startLockHolder(t, root)
		_, err = l.acquire()
		wantCode(t, err, contract.CodeConflict)
		h.kill(t)
		lk, err = l.acquire()
		if err != nil {
			t.Fatalf("after kill: %v", err)
		}
		lk.release()
		if _, err := os.Stat(l.path(lockName)); err != nil {
			t.Fatal("lock file was unlinked")
		}
	})
}

// A restored complete stopped backup (a copy of the root) loads with the
// same trust.
func TestStoppedBackupRestore(t *testing.T) {
	d := testDeps(t)
	root := newRoot(t)
	st := mustInit(t, d, root, "localhost", "10.0.0.1")
	backup := newRoot(t)
	copyTree(t, root, backup)
	st2, err := d.init(bg, InitOptions{StateDir: backup})
	if err != nil {
		t.Fatal(err)
	}
	if st2.CAFingerprint != st.CAFingerprint || strings.Join(st2.IPAddresses, ",") != "10.0.0.1" {
		t.Fatalf("restored = %+v", st2)
	}
	sameSnapshot(t, snapshot(t, root), snapshot(t, backup))
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, _ := e.Info()
		if e.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDirSyncFallback: File.Sync failing with ENOTSUP, ENOTTY or EINVAL
// is retried as a plain fsync on the same descriptor; other errors are
// not; if both fail, initialization reports the preserved files. The
// policy has no OS branch, so one table covers linux and darwin.
func TestDirSyncFallback(t *testing.T) {
	pathErr := func(errno syscall.Errno) error { return &fs.PathError{Op: "sync", Path: "dir", Err: errno} }
	for _, c := range []struct {
		name     string
		first    error
		raw      error
		want     string
		rawCalls int
	}{
		{"ok", nil, nil, "", 0},
		{"ENOTSUP", pathErr(syscall.ENOTSUP), nil, "", 1},
		{"ENOTTY", pathErr(syscall.ENOTTY), nil, "", 1},
		{"EINVAL", pathErr(syscall.EINVAL), nil, "", 1},
		{"EIO no fallback", pathErr(syscall.EIO), nil, "input/output error", 0},
		{"both fail", pathErr(syscall.ENOTTY), syscall.EIO, "cannot sync the directory (File.Sync: sync dir: inappropriate ioctl for device; fsync fallback: input/output error)", 1},
	} {
		d := testDeps(t)
		var synced []string
		rawCalls := 0
		d.fileSync = func(f *os.File) error { synced = append(synced, f.Name()); return c.first }
		d.rawFsync = func(f *os.File) error {
			rawCalls++
			if f.Name() != synced[len(synced)-1] {
				t.Fatalf("%s: fallback on another descriptor", c.name)
			}
			return c.raw
		}
		root := t.TempDir()
		err := d.syncDir(layout{root: root}, rootName)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) || rawCalls != c.rawCalls {
			t.Fatalf("%s: err=%v rawCalls=%d", c.name, err, rawCalls)
		}
		if len(synced) != 1 || synced[0] != root {
			t.Fatalf("%s: synced %v", c.name, synced)
		}
	}
	// Both failing during initialization: the already-published PKI files
	// are preserved and the diagnostic says so and what to do.
	d := testDeps(t)
	d.fileSync = func(*os.File) error { return pathErr(syscall.EINVAL) }
	d.rawFsync = func(*os.File) error { return syscall.EIO }
	root := newRoot(t)
	_, err := d.init(bg, initOpts(root, "localhost"))
	wantCode(t, err, contract.CodeInternal, "syncing pki/", "cannot sync the directory", "Published files were preserved",
		"published: pki/ca.crt, pki/ca.key, pki/server.crt, pki/server.key", "restore a complete stopped backup, or choose a fresh --state-dir")
	snap := snapshot(t, root)
	for _, rel := range durable[:4] {
		if snap[rel] == nil {
			t.Fatalf("%s was not preserved", rel)
		}
	}
	// A working fallback lets initialization complete.
	d.rawFsync = rawFsync
	mustInit(t, d, newRoot(t), "localhost")
	// The native fallback fsyncs a real directory descriptor.
	f, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := rawFsync(f); err != nil {
		t.Fatalf("native directory fsync: %v", err)
	}
}
