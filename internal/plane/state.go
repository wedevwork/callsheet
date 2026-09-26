package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Managed state paths, relative to the state root (slash-separated).
const (
	configName     = "config.json"
	pkiName        = "pki"
	tmpName        = "tmp"
	lockName       = ".lock"
	caCertName     = "pki/ca.crt"
	caKeyName      = "pki/ca.key"
	serverCertName = "pki/server.crt"
	serverKeyName  = "pki/server.key"
	// rootName names the state root itself for failure injection.
	rootName = "."
)

const (
	// SchemaVersion is the config.json schema this build reads and writes.
	SchemaVersion = 1
	// DefaultBind is the bind used when --bind is omitted on new state.
	DefaultBind = "127.0.0.1:8443"
	// tempPrefix starts every unpublished temporary sibling's name.
	tempPrefix = ".tmp-"
	// maxFileSize bounds config.json and every PKI file.
	maxFileSize = 64 << 10
	dirMode     = 0o700
)

// durable are the five durable files, in publication order: config.json
// last, as the completion marker.
var durable = []string{caCertName, caKeyName, serverCertName, serverKeyName, configName}

// errLocked reports advisory-lock contention.
var errLocked = errors.New("state lock is held by another process")

// ResolveStateDir returns the absolute plane state root. A nonempty
// override (--state-dir) wins and is resolved against the working
// directory and cleaned; no tilde expansion. Otherwise linux uses an
// absolute XDG_STATE_HOME or $HOME/.local/state, and darwin uses
// $HOME/Library/Application Support (ignoring XDG), each followed by
// callsheet/plane. Every decision takes goos explicitly.
func ResolveStateDir(goos, override, home, xdg string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", wrapf(contract.CodeInvalidArgument, err, "cannot resolve --state-dir %q: %v", override, err)
		}
		return abs, nil
	}
	var base string
	switch goos {
	case "linux":
		if xdg != "" {
			if !filepath.IsAbs(xdg) {
				return "", errf(contract.CodeInvalidArgument, "XDG_STATE_HOME %q is not an absolute path; set it to an absolute path, unset it, or pass --state-dir", xdg)
			}
			return filepath.Join(xdg, "callsheet", "plane"), nil
		}
		base = filepath.Join(".local", "state", "callsheet", "plane")
	case "darwin":
		base = filepath.Join("Library", "Application Support", "callsheet", "plane")
	default:
		return "", errf(contract.CodeInvalidArgument, "unsupported operating system %q; supported: linux, darwin", goos)
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errf(contract.CodeInvalidArgument, "HOME %q is not an absolute path; set HOME or pass --state-dir", home)
	}
	return filepath.Join(home, base), nil
}

// layout derives every managed path from the root; PKI paths are never
// taken from configuration.
type layout struct{ root string }

func (l layout) path(rel string) string {
	if rel == rootName {
		return l.root
	}
	return filepath.Join(l.root, filepath.FromSlash(rel))
}

func isKey(rel string) bool { return rel == caKeyName || rel == serverKeyName }

type stateKind int

const (
	kindEmpty stateKind = iota
	kindPartial
	kindComplete
)

// scanResult classifies state without reading file contents.
type scanResult struct {
	kind       stateKind
	rootExists bool
	missing    []string
}

// scan checks the root, managed directories, lock and durable files for
// symlinks, nonregular files and unsafe modes, and refuses (conflict) any
// entry that is not a managed name or an unpublished .tmp- temporary: in
// the root, only pki/, tmp/, .lock and config.json; in pki/, only the four
// PKI files; in tmp/, nothing else. It then classifies the state as empty
// (a missing root, or only empty managed directories plus .lock and
// temporaries), partial (any strict subset of the durable files) or
// complete.
func (l layout) scan() (scanResult, error) {
	var s scanResult
	fi, err := os.Lstat(l.root)
	if errors.Is(err, fs.ErrNotExist) {
		s.missing = append([]string(nil), durable...)
		return s, nil
	}
	if err != nil {
		return s, wrapf(contract.CodeInternal, err, "cannot inspect state directory: %v", err)
	}
	s.rootExists = true
	if err := checkDir(l.root, fi); err != nil {
		return s, err
	}
	var unexpected []string
	rootEntries, err := os.ReadDir(l.root)
	if err != nil {
		return s, wrapf(contract.CodeInternal, err, "cannot list %s: %v", l.root, err)
	}
	for _, e := range rootEntries {
		switch n := e.Name(); {
		case n == pkiName, n == tmpName, n == lockName, n == configName, strings.HasPrefix(n, tempPrefix):
		default:
			unexpected = append(unexpected, filepath.Join(l.root, n))
		}
	}
	for _, rel := range []string{pkiName, tmpName} {
		p := l.path(rel)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return s, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
		}
		if err := checkDir(p, fi); err != nil {
			return s, err
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return s, wrapf(contract.CodeInternal, err, "cannot list %s: %v", p, err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), tempPrefix) || (rel == pkiName && isDurableBase(e.Name())) {
				continue
			}
			unexpected = append(unexpected, filepath.Join(p, e.Name()))
		}
	}
	if fi, err := os.Lstat(l.path(lockName)); err == nil {
		if err := checkPublic(l.path(lockName), fi); err != nil {
			return s, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return s, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", l.path(lockName), err)
	}
	for _, rel := range durable {
		p := l.path(rel)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			s.missing = append(s.missing, rel)
			continue
		}
		if err != nil {
			return s, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
		}
		check := checkPublic
		if isKey(rel) {
			check = checkKey
		}
		if err := check(p, fi); err != nil {
			return s, err
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return s, errf(contract.CodeConflict, "state directory %s holds unexpected %s; plane state may contain only config.json, pki/ (the four PKI files), tmp/ and .lock. Nothing was changed: move the unexpected entries out of the state directory, or choose a fresh --state-dir",
			l.root, strings.Join(unexpected, ", "))
	}
	switch len(s.missing) {
	case 0:
		s.kind = kindComplete
	case len(durable):
		s.kind = kindEmpty
	default:
		s.kind = kindPartial
	}
	return s, nil
}

func isDurableBase(name string) bool {
	for _, rel := range durable {
		if strings.HasPrefix(rel, pkiName+"/") && rel[len(pkiName)+1:] == name {
			return true
		}
	}
	return false
}

// partialError names the missing durable files of partial state.
func (l layout) partialError(s scanResult) error {
	paths := make([]string, len(s.missing))
	for i, rel := range s.missing {
		paths[i] = l.path(rel)
	}
	return errf(contract.CodeConflict, "plane state in %s is incomplete (missing %s): an initialization did not finish; no file was replaced. Preserve the directory and restore a complete stopped backup, or choose a fresh --state-dir",
		l.root, strings.Join(paths, ", "))
}

func checkDir(p string, fi fs.FileInfo) error {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return errf(contract.CodeTrustFailed, "%s is a symbolic link; plane state paths must be real directories and files", p)
	}
	if !fi.IsDir() {
		return errf(contract.CodeTrustFailed, "%s is not a directory", p)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return errf(contract.CodeTrustFailed, "state directory %s has mode %04o; it must not be accessible by group or others (chmod 700; permissions are never repaired automatically)", p, perm)
	}
	return nil
}

func checkRegular(p string, fi fs.FileInfo) error {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return errf(contract.CodeTrustFailed, "%s is a symbolic link; plane state paths must be real directories and files", p)
	}
	if !fi.Mode().IsRegular() {
		return errf(contract.CodeTrustFailed, "%s is not a regular file", p)
	}
	return nil
}

// checkKey accepts only owner-only private keys: mode 0600 or 0400.
func checkKey(p string, fi fs.FileInfo) error {
	if err := checkRegular(p, fi); err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm != 0o600 && perm != 0o400 {
		return errf(contract.CodeTrustFailed, "private key %s has mode %04o; it must be owner-only (0600 or 0400; permissions are never repaired automatically)", p, perm)
	}
	return nil
}

// checkPublic accepts config, certificates and the lock when not writable
// by group or others.
func checkPublic(p string, fi fs.FileInfo) error {
	if err := checkRegular(p, fi); err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return errf(contract.CodeTrustFailed, "%s has mode %04o; it must not be writable by group or others", p, perm)
	}
	return nil
}

// stateLock is an exclusive advisory lock on .lock, held by an open file
// descriptor; closing it releases the lock (as does process exit).
type stateLock struct{ f *os.File }

// acquire takes the nonblocking exclusive lock, creating .lock exclusively
// when absent and reopening it otherwise. It never unlinks the file.
func (l layout) acquire() (*stateLock, error) {
	p := l.path(lockName)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		fi, lerr := os.Lstat(p)
		if lerr != nil {
			return nil, wrapf(contract.CodeInternal, lerr, "cannot inspect %s: %v", p, lerr)
		}
		if cerr := checkPublic(p, fi); cerr != nil {
			return nil, cerr
		}
		f, err = os.OpenFile(p, os.O_RDWR, 0)
		if err == nil {
			if st, serr := f.Stat(); serr != nil || !os.SameFile(fi, st) {
				f.Close()
				return nil, errf(contract.CodeTrustFailed, "%s changed while it was being opened", p)
			}
		}
	}
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot open lock file: %v", err)
	}
	if err := flockExclusive(f); err != nil {
		f.Close()
		if errors.Is(err, errLocked) {
			return nil, errf(contract.CodeConflict, "plane state %s is in use by another callsheet process (plane run, init or cert reissue); stop it and retry", l.root)
		}
		return nil, wrapf(contract.CodeInternal, err, "cannot lock %s: %v", p, err)
	}
	return &stateLock{f: f}, nil
}

// release closes the descriptor, releasing the lock. It is idempotent.
func (s *stateLock) release() {
	if s != nil && s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

// ensureDir creates a managed directory with mode 0700 when absent.
func (l layout) ensureDir(rel string) error {
	err := os.Mkdir(l.path(rel), dirMode)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

// writeTemp writes data to a new unique 0600 temporary sibling of rel,
// syncs and closes it, and returns its path. On failure the temporary is
// removed (only its creator may remove it).
func (d *deps) writeTemp(l layout, rel string, data []byte) (string, error) {
	if err := d.hook("create", rel); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(l.path(rel)), tempPrefix+filepath.Base(rel)+"-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := d.hook("write", rel); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := d.hook("sync", rel); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := d.hook("close", rel); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// publishNew hard-links tmp to rel without ever replacing an existing
// destination, then removes tmp.
func (d *deps) publishNew(l layout, tmp, rel string) error {
	defer os.Remove(tmp)
	if err := d.hook("publish", rel); err != nil {
		return err
	}
	return os.Link(tmp, l.path(rel))
}

// syncDir fsyncs the directory rel so that published names are durable.
func (d *deps) syncDir(l layout, rel string) error {
	if err := d.hook("dirsync", rel); err != nil {
		return err
	}
	f, err := os.Open(l.path(rel))
	if err != nil {
		return err
	}
	err = d.syncDirFile(f)
	return errors.Join(err, f.Close())
}

// syncDirFile syncs an open directory with File.Sync (F_FULLFSYNC on
// Darwin, which Go itself retries as fsync only for ENOTSUP). When that
// reports ENOTSUP, ENOTTY or EINVAL, as some filesystems do for a
// directory, it retries a plain fsync on the same descriptor. The policy
// is the same on every system.
func (d *deps) syncDirFile(f *os.File) error {
	err := d.fileSync(f)
	if err == nil || !(errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL)) {
		return err
	}
	if ferr := d.rawFsync(f); ferr != nil {
		return fmt.Errorf("this filesystem cannot sync the directory (File.Sync: %v; fsync fallback: %v); plane state needs a local POSIX filesystem that supports directory sync", err, ferr)
	}
	return nil
}

// initialize generates all trust material in memory, writes every
// temporary sibling, then publishes the four PKI files without
// replacement, syncs pki/, publishes config.json last and syncs the root.
// A failure after the first publication preserves every published file.
func (d *deps) initialize(ctx context.Context, l layout, now time.Time, bind netip.AddrPort, sans sanSet) (*material, error) {
	m, files, err := d.generate(now, bind, sans)
	if err != nil {
		return nil, err
	}
	for _, rel := range []string{pkiName, tmpName} {
		if err := l.ensureDir(rel); err != nil {
			return nil, wrapf(contract.CodeInternal, err, "cannot create %s: %v", l.path(rel), err)
		}
	}
	if err := canceled(ctx); err != nil {
		return nil, err
	}
	temps := map[string]string{}
	cleanup := func() {
		for _, t := range temps {
			os.Remove(t)
		}
	}
	for _, rel := range durable {
		tmp, err := d.writeTemp(l, rel, files[rel])
		if err != nil {
			cleanup()
			return nil, wrapf(contract.CodeInternal, err, "initialization of %s failed before any state file was published (writing %s): %v; nothing was published", l.root, rel, err)
		}
		temps[rel] = tmp
	}
	var published []string
	fail := func(step string, err error) error {
		cleanup()
		done := "none"
		if len(published) > 0 {
			done = strings.Join(published, ", ")
		}
		return wrapf(contract.CodeInternal, err, "initialization of %s failed at %s: %v. Published files were preserved, never rolled back (published: %s). Preserve the directory and restore a complete stopped backup, or choose a fresh --state-dir",
			l.root, step, err, done)
	}
	publish := func(rel string) error {
		tmp := temps[rel]
		delete(temps, rel)
		if err := d.publishNew(l, tmp, rel); err != nil {
			return fail("publishing "+rel, err)
		}
		published = append(published, rel)
		return nil
	}
	for _, rel := range durable[:4] {
		if err := publish(rel); err != nil {
			return nil, err
		}
	}
	if err := d.syncDir(l, pkiName); err != nil {
		return nil, fail("syncing pki/", err)
	}
	if err := publish(configName); err != nil {
		return nil, err
	}
	if err := d.syncDir(l, rootName); err != nil {
		return nil, fail("syncing the state directory", err)
	}
	return m, nil
}

// replaceServerCert atomically replaces pki/server.crt via a synced
// temporary sibling, rename and directory sync.
func (d *deps) replaceServerCert(l layout, data []byte) error {
	tmp, err := d.writeTemp(l, serverCertName, data)
	if err != nil {
		return wrapf(contract.CodeInternal, err, "reissue failed before replacing %s: %v; the previous certificate is unchanged", l.path(serverCertName), err)
	}
	if err := d.hook("rename", serverCertName); err != nil {
		os.Remove(tmp)
		return wrapf(contract.CodeInternal, err, "reissue failed before replacing %s: %v; the previous certificate is unchanged", l.path(serverCertName), err)
	}
	if err := os.Rename(tmp, l.path(serverCertName)); err != nil {
		os.Remove(tmp)
		return wrapf(contract.CodeInternal, err, "reissue failed before replacing %s: %v; the previous certificate is unchanged", l.path(serverCertName), err)
	}
	if err := d.syncDir(l, pkiName); err != nil {
		return wrapf(contract.CodeInternal, err, "%s was replaced but syncing %s failed: %v; the replacement may already be visible, and its durability is not confirmed. Run callsheet plane status to see which certificate is present",
			l.path(serverCertName), l.path(pkiName), err)
	}
	return nil
}

// configFile is the ordered config.json schema.
type configFile struct {
	SchemaVersion int    `json:"schema_version"`
	Bind          string `json:"bind"`
}

// encodeConfig renders the exact writer bytes: two-space indentation,
// fields in schema order, one final LF.
func encodeConfig(bind netip.AddrPort) []byte {
	b, err := json.MarshalIndent(configFile{SchemaVersion: SchemaVersion, Bind: bind.String()}, "", "  ")
	if err != nil {
		panic(err) // unreachable: two fixed-type fields
	}
	return append(b, '\n')
}

// parseConfig strictly decodes config.json: one object, no unknown or
// duplicate keys, no trailing JSON, schema 1 and a policy-valid bind.
// Other whitespace and key order are accepted.
func parseConfig(data []byte) (netip.AddrPort, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return netip.AddrPort{}, errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	var version json.RawMessage
	var bind string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("malformed JSON: %v", err)
		}
		key, _ := tok.(string)
		if seen[key] {
			return netip.AddrPort{}, fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		switch key {
		case "schema_version":
			err = dec.Decode(&version)
		case "bind":
			err = dec.Decode(&bind)
		default:
			return netip.AddrPort{}, fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("invalid %q: %v", key, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return netip.AddrPort{}, fmt.Errorf("malformed JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return netip.AddrPort{}, errors.New("trailing data after the JSON object")
	}
	if !seen["schema_version"] || !seen["bind"] {
		return netip.AddrPort{}, errors.New(`both "schema_version" and "bind" are required`)
	}
	if len(version) == 0 || (version[0] != '-' && (version[0] < '0' || version[0] > '9')) {
		return netip.AddrPort{}, fmt.Errorf(`invalid "schema_version": %s is not a number`, version)
	}
	if string(version) != "1" {
		return netip.AddrPort{}, fmt.Errorf("unsupported schema_version %s (this build supports %d)", version, SchemaVersion)
	}
	ap, err := parseBind(bind)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("bind: %v", err)
	}
	return ap, nil
}

// readBounded reads at most maxFileSize bytes and rejects larger files.
func readBounded(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileSize {
		return nil, errTooLarge
	}
	return b, nil
}

var errTooLarge = errors.New("file is larger than 64 KiB")

// loadConfig reads and validates config.json.
func (l layout) loadConfig() (netip.AddrPort, error) {
	p := l.path(configName)
	b, err := readBounded(p)
	if err != nil && !errors.Is(err, errTooLarge) {
		return netip.AddrPort{}, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	if err == nil {
		var ap netip.AddrPort
		if ap, err = parseConfig(b); err == nil {
			return ap, nil
		}
	}
	return netip.AddrPort{}, wrapf(contract.CodeConflict, err, "invalid plane configuration %s: %v; configuration is never regenerated: fix the file or restore a complete stopped backup", p, err)
}
