package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"
)

// Coordinator settings (design 12a-real-e2e, amendment A2): the owner
// extracts the sandbox object of their own user settings into a new
// private file and passes it with --coordinator-settings. The harness
// never reads the user's settings or auth stores itself: it reads that
// regular, non-symlinked file (at most 64 KiB) after the startup gate and
// before any probe, and accepts exactly one strict UTF-8 JSON object whose
// only key is "sandbox", an object whose "enabled" is the boolean true.
// The whole sandbox object is preserved byte for byte (unknown vendor
// settings included) and never interpreted or weakened. The validated
// bytes are copied to the runtime (the copy the coordinator is launched
// with) and to private evidence, whose manifest records their SHA-256.

// Settings names and launch values.
const (
	// SettingsFile is the runtime copy's name.
	SettingsFile = "coordinator-settings.json"
	// SettingsEvidence is the evidence copy's name.
	SettingsEvidence = "setup/" + SettingsFile
	// SettingSources is the launch's --setting-sources value: the user's
	// own settings are excluded.
	SettingSources = "project,local"
	// SettingsArg is the recorded launch's --settings value.
	SettingsArg = "$RUN/" + SettingsFile
	// maxSettingsBytes bounds the settings file.
	maxSettingsBytes = 64 << 10
	// codeSettingsModified is the failure of a runtime copy changed or
	// removed during the session.
	codeSettingsModified = "coordinator_settings_modified"
)

// readCoordinatorSettings reads and validates the owner's settings file:
// a regular file, never through a symlink, within the bound.
func readCoordinatorSettings(p string) ([]byte, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("the coordinator settings must be a regular file, not a symlink")
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("the coordinator settings must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSettingsBytes+1))
	if err != nil {
		return nil, err
	}
	if err := ValidateCoordinatorSettings(data); err != nil {
		return nil, err
	}
	return data, nil
}

// ValidateCoordinatorSettings checks a settings snapshot: at most 64 KiB
// of valid UTF-8, one JSON value with no duplicate key at any depth and
// nothing after it, an object whose sole key is "sandbox", whose value is
// an object with "enabled" exactly true. Other sandbox settings are not
// interpreted.
func ValidateCoordinatorSettings(data []byte) error {
	switch {
	case len(data) > maxSettingsBytes:
		return fmt.Errorf("the coordinator settings exceed %d bytes", maxSettingsBytes)
	case !utf8.Valid(data):
		return errors.New("the coordinator settings are not valid UTF-8")
	}
	if err := checkDuplicateKeys(data); err != nil {
		return fmt.Errorf("the coordinator settings: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return errors.New("the coordinator settings must be a JSON object")
	}
	raw, ok := root["sandbox"]
	if !ok || len(root) != 1 {
		return errors.New(`the coordinator settings must hold exactly one key, "sandbox"`)
	}
	var sandbox map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sandbox); err != nil || sandbox == nil {
		return errors.New("the coordinator settings' sandbox must be an object")
	}
	if !bytes.Equal(bytes.TrimSpace(sandbox["enabled"]), []byte("true")) {
		return errors.New("the coordinator settings' sandbox.enabled must be true")
	}
	return nil
}

// checkSettingsCopy re-reads the runtime copy before collection: it must
// still hold exactly the validated snapshot.
func (s *supervisor) checkSettingsCopy() error {
	data, err := readBounded(filepath.Join(s.runtime, SettingsFile), maxSettingsBytes)
	if err != nil {
		return fmt.Errorf("the runtime coordinator settings are unreadable: %w", err)
	}
	if !bytes.Equal(data, s.settings) {
		return errors.New("the runtime coordinator settings changed during the session")
	}
	return nil
}
