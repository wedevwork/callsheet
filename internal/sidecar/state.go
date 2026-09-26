package sidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Managed sidecar state, relative to the state root.
const (
	identityName   = "identity.json"
	enrollmentName = "enrollment.json"
	lockName       = ".lock"
	// rootName names the state root itself for failure injection.
	rootName = "."
	// tempPrefix starts every unpublished temporary sibling's name.
	tempPrefix = ".tmp-"
	// SchemaVersion is the identity.json and enrollment.json schema.
	SchemaVersion = 1
	// maxStateFile bounds identity.json and enrollment.json.
	maxStateFile = 64 << 10
	dirMode      = 0o700
)

// errLocked reports advisory-lock contention.
var errLocked = errors.New("state lock is held by another process")

func errf(code contract.Code, format string, args ...any) *contract.Error {
	return contract.New(code, fmt.Sprintf(format, args...))
}

func wrapf(code contract.Code, cause error, format string, args ...any) *contract.Error {
	return contract.Wrap(code, fmt.Sprintf(format, args...), cause)
}

// ResolveStateDir returns the absolute sidecar state root, with exactly
// the plane's resolution rules: a nonempty override (--state-dir) wins,
// resolved against the working directory and cleaned (no tilde
// expansion); otherwise linux uses an absolute XDG_STATE_HOME or
// $HOME/.local/state, darwin $HOME/Library/Application Support (ignoring
// XDG), each followed by callsheet/sidecar. goos is explicit.
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
			return filepath.Join(xdg, "callsheet", "sidecar"), nil
		}
		base = filepath.Join(".local", "state", "callsheet", "sidecar")
	case "darwin":
		base = filepath.Join("Library", "Application Support", "callsheet", "sidecar")
	default:
		return "", errf(contract.CodeInvalidArgument, "unsupported operating system %q; supported: linux, darwin", goos)
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errf(contract.CodeInvalidArgument, "HOME %q is not an absolute path; set HOME or pass --state-dir", home)
	}
	return filepath.Join(home, base), nil
}

// layout derives every managed path from the root.
type layout struct{ root string }

func (l layout) path(rel string) string {
	if rel == rootName {
		return l.root
	}
	return filepath.Join(l.root, rel)
}

// scanResult describes validated state without reading file contents.
type scanResult struct {
	rootExists, hasIdentity, hasEnrollment bool
}

// scan checks the root and its entries: only .lock, identity.json,
// enrollment.json and regular unpublished .tmp- siblings are allowed.
// Symlinks, nonregular files and unsafe modes are trust_failed; any other
// entry is conflict. Nothing is repaired.
func (l layout) scan() (scanResult, error) {
	var s scanResult
	fi, err := os.Lstat(l.root)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, wrapf(contract.CodeInternal, err, "cannot inspect state directory: %v", err)
	}
	s.rootExists = true
	if err := checkDir(l.root, fi); err != nil {
		return s, err
	}
	entries, err := os.ReadDir(l.root)
	if err != nil {
		return s, wrapf(contract.CodeInternal, err, "cannot list %s: %v", l.root, err)
	}
	var unexpected []string
	for _, e := range entries {
		p := filepath.Join(l.root, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return s, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
		}
		switch n := e.Name(); {
		case n == lockName:
			if err := checkPublic(p, fi); err != nil {
				return s, err
			}
		case n == identityName, n == enrollmentName:
			if err := checkKey(p, fi); err != nil {
				return s, err
			}
			if n == identityName {
				s.hasIdentity = true
			} else {
				s.hasEnrollment = true
			}
		case strings.HasPrefix(n, tempPrefix) && fi.Mode().IsRegular():
		default:
			unexpected = append(unexpected, p)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return s, errf(contract.CodeConflict, "sidecar state directory %s holds unexpected %s; it may contain only identity.json, enrollment.json and .lock. Nothing was changed: move the unexpected entries out, or choose a fresh --state-dir",
			l.root, strings.Join(unexpected, ", "))
	}
	return s, nil
}

func checkDir(p string, fi fs.FileInfo) error {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return errf(contract.CodeTrustFailed, "%s is a symbolic link; sidecar state paths must be real directories and files", p)
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
		return errf(contract.CodeTrustFailed, "%s is a symbolic link; sidecar state paths must be real directories and files", p)
	}
	if !fi.Mode().IsRegular() {
		return errf(contract.CodeTrustFailed, "%s is not a regular file", p)
	}
	return nil
}

// checkKey accepts owner-only identity and enrollment files: 0600 or 0400.
func checkKey(p string, fi fs.FileInfo) error {
	if err := checkRegular(p, fi); err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm != 0o600 && perm != 0o400 {
		return errf(contract.CodeTrustFailed, "%s has mode %04o; it must be owner-only (0600 or 0400; permissions are never repaired automatically)", p, perm)
	}
	return nil
}

// checkPublic accepts the lock when not writable by group or others.
func checkPublic(p string, fi fs.FileInfo) error {
	if err := checkRegular(p, fi); err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return errf(contract.CodeTrustFailed, "%s has mode %04o; it must not be writable by group or others", p, perm)
	}
	return nil
}

// identityFile is the ordered identity.json schema.
type identityFile struct {
	SchemaVersion int    `json:"schema_version"`
	NodeID        string `json:"node_id"`
}

// enrollmentFile is the ordered enrollment.json schema: target and trust
// in one atomically replaceable file.
type enrollmentFile struct {
	SchemaVersion int    `json:"schema_version"`
	PlaneURL      string `json:"plane_url"`
	CAPEM         string `json:"ca_pem"`
	CAFingerprint string `json:"ca_fingerprint"`
}

// encode renders the exact writer bytes: two-space indentation, schema
// field order, one final LF.
func encode(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // unreachable: fixed string and integer fields
	}
	return append(b, '\n')
}

// decodeStrict decodes one JSON object with exactly the given keys: no
// duplicate, unknown or missing key, no null, no trailing JSON. set
// decodes one value.
func decodeStrict(data []byte, keys []string, set func(key string, dec *json.Decoder) error) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("malformed JSON: %v", err)
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("duplicate key %q", key)
		}
		if !allowed[key] {
			return fmt.Errorf("unknown field %q", contract.SafeText(key, 32))
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("malformed JSON: %v", err)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%q must not be null", key)
		}
		if err := set(key, json.NewDecoder(bytes.NewReader(raw))); err != nil {
			return fmt.Errorf("invalid %q: %v", key, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("malformed JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON object")
	}
	for _, k := range keys {
		if !seen[k] {
			return fmt.Errorf("%q is required", k)
		}
	}
	return nil
}

func decodeVersion(dec *json.Decoder) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if n, ok := contract.ParseInteger(string(raw)); !ok || n != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %s (this build supports %d)", contract.SafeText(string(raw), 32), SchemaVersion)
	}
	return nil
}

func parseIdentity(data []byte) (string, error) {
	var id string
	err := decodeStrict(data, []string{"schema_version", "node_id"}, func(key string, dec *json.Decoder) error {
		if key == "schema_version" {
			return decodeVersion(dec)
		}
		return dec.Decode(&id)
	})
	if err != nil {
		return "", err
	}
	if !contract.ValidNodeID(id) {
		return "", errors.New("node_id is not a valid node ID")
	}
	return id, nil
}

func parseEnrollment(data []byte) (enrollmentFile, error) {
	e := enrollmentFile{SchemaVersion: SchemaVersion}
	err := decodeStrict(data, []string{"schema_version", "plane_url", "ca_pem", "ca_fingerprint"}, func(key string, dec *json.Decoder) error {
		switch key {
		case "schema_version":
			return decodeVersion(dec)
		case "plane_url":
			return dec.Decode(&e.PlaneURL)
		case "ca_pem":
			return dec.Decode(&e.CAPEM)
		default:
			return dec.Decode(&e.CAFingerprint)
		}
	})
	if err != nil {
		return e, err
	}
	if norm, err := client.NormalizePlaneURL(e.PlaneURL); err != nil || norm != e.PlaneURL {
		return e, errors.New("plane_url is not a normalized https plane URL")
	}
	if !client.ValidPin(e.CAFingerprint) {
		return e, errors.New("ca_fingerprint is not a sha256: fingerprint")
	}
	return e, nil
}

// readState reads one owner-only state file, at most maxStateFile bytes.
func (l layout) readState(rel string) ([]byte, error) {
	p := l.path(rel)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	if err := checkKey(p, fi); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStateFile+1))
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	return b, nil
}

func (l layout) invalid(rel string, err error) error {
	return wrapf(contract.CodeConflict, err, "invalid sidecar state %s: %v; sidecar state is never repaired or regenerated automatically: restore it, or choose a fresh --state-dir and enroll again (a fresh state gets a new node identity)", l.path(rel), err)
}

// loadIdentity reads and validates identity.json.
func (l layout) loadIdentity() (string, error) {
	b, err := l.readState(identityName)
	if err != nil {
		return "", err
	}
	if len(b) > maxStateFile {
		return "", l.invalid(identityName, errors.New("file is larger than 64 KiB"))
	}
	id, err := parseIdentity(b)
	if err != nil {
		return "", l.invalid(identityName, err)
	}
	return id, nil
}

// loadEnrollment reads and validates enrollment.json, including the
// embedded CA against its fingerprint.
func (l layout) loadEnrollment() (enrollmentFile, error) {
	b, err := l.readState(enrollmentName)
	if err != nil {
		return enrollmentFile{}, err
	}
	if len(b) > maxStateFile {
		return enrollmentFile{}, l.invalid(enrollmentName, errors.New("file is larger than 64 KiB"))
	}
	e, err := parseEnrollment(b)
	if err != nil {
		return e, l.invalid(enrollmentName, err)
	}
	if _, err := client.New(e.PlaneURL, client.Trust{CAPEM: []byte(e.CAPEM), Fingerprint: e.CAFingerprint}); err != nil {
		return e, wrapf(contract.CodeTrustFailed, err, "the CA embedded in %s is invalid or does not match its fingerprint; enroll again with --ca or --ca-fingerprint", l.path(enrollmentName))
	}
	return e, nil
}

// stateLock is an exclusive advisory lock on .lock, held by an open file
// descriptor for the whole Enroll or Run.
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
			return nil, errf(contract.CodeConflict, "sidecar state %s is in use by another callsheet process (sidecar run or enroll); stop it and retry", l.root)
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

func (d *deps) hook(op, name string) error {
	if d.fail == nil {
		return nil
	}
	return d.fail(op, name)
}

// writeTemp writes data to a new unique 0600 temporary sibling of rel,
// syncs and closes it. On failure the temporary is removed.
func (d *deps) writeTemp(l layout, rel string, data []byte) (string, error) {
	if err := d.hook("create", rel); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(l.root, tempPrefix+rel+"-")
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

// replace atomically renames tmp over rel.
func (d *deps) replace(l layout, tmp, rel string) error {
	if err := d.hook("rename", rel); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, l.path(rel)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// syncDir fsyncs the state root so that published names are durable.
func (d *deps) syncDir(l layout) error {
	if err := d.hook("dirsync", rootName); err != nil {
		return err
	}
	f, err := os.Open(l.root)
	if err != nil {
		return err
	}
	err = d.syncDirFile(f)
	return errors.Join(err, f.Close())
}

// syncDirFile syncs an open directory with File.Sync; on ENOTSUP, ENOTTY
// or EINVAL it retries a plain fsync on the same descriptor, the plane's
// policy on every system.
func (d *deps) syncDirFile(f *os.File) error {
	err := d.fileSync(f)
	if err == nil || !(errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL)) {
		return err
	}
	if ferr := d.rawFsync(f); ferr != nil {
		return fmt.Errorf("this filesystem cannot sync the directory (File.Sync: %v; fsync fallback: %v); sidecar state needs a local POSIX filesystem that supports directory sync", err, ferr)
	}
	return nil
}
