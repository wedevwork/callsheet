package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// Shared-method loading (design 12a-dev-flow-harness, FP-8). Role manuals
// and the playbook are literal bytes. Only the harness-owned exercise
// prompts are interpolated, and only with DevPromptKeys.

const (
	DevFlowDir    = "docs/examples/dev-flow"
	DevFullDir    = "docs/examples/real-e2e/dev-full"
	DevPlaybook   = "coordinator-playbook.md"
	DevPromptFile = "coordinator-prompt.md"
	DevPhaseFile  = "phase.md"
	DevScenario   = "scenario.md"
	// DevSystemName names the composed coordinator system input.
	DevSystemName = "coordinator-system"
)

// DevPromptKeys are the only placeholders a harness-owned prompt may use.
var DevPromptKeys = []string{"RUN_ID", "RUNTIME", "PLANE_URL", "CA_PATH", "REALE2E", "WORKSPACE", "INSTANCE", "SEED_PATH", "SEED_COMMIT", "COORD_DIR"}

// devManualNames are the ten role files, in role order. README is not a
// prompt input.
func devManualNames() []string {
	var out []string
	for _, id := range DevFullRoleIDs {
		out = append(out, manualName(id, "instruction"), manualName(id, "runbook"))
	}
	return out
}

// DevMethodNames are the eleven shared-method inputs, then the exercise
// files, in provenance order.
func DevMethodNames() []string {
	out := append(devManualNames(), DevPlaybook)
	return append(out, FlowFile, DevPromptFile, DevPhaseFile, DevScenario)
}

// SourceRecord is one captured input.
type SourceRecord struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// RenderedRecord names a rendered harness prompt and the sources that
// contributed to it.
type RenderedRecord struct {
	Name    string   `json:"name"`
	Sources []string `json:"sources"`
	SHA256  string   `json:"sha256"`
}

// DevLoaded is one capture of the shared method and the exercise files.
type DevLoaded struct {
	Files    map[string][]byte
	Sources  []SourceRecord
	Rendered map[string][]byte
	Records  []RenderedRecord
	FlowRaw  []byte
	FlowSum  string
}

// readLiteral reads one regular, non-symlink, nonempty UTF-8 file within
// limit. It does not follow a symlink.
func readLiteral(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("the input is a symlink")
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("the input is not a regular file")
	}
	b, err := readBounded(path, limit)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("the input is empty")
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, errors.New("the input is not UTF-8 text")
	}
	return b, nil
}

// LoadDevMethod reads the shared manuals, the playbook, and the exercise
// files from a checkout. Markdown files are capped at 32 KiB and the flow
// JSON at 16 KiB. The returned bytes are the capture; later edits to the
// checkout do not change them.
func LoadDevMethod(checkout string) (DevLoaded, error) {
	var loaded DevLoaded
	loaded.Files = map[string][]byte{}
	method := filepath.Join(checkout, filepath.FromSlash(DevFlowDir))
	exercise := filepath.Join(checkout, filepath.FromSlash(DevFullDir))
	for _, name := range DevMethodNames() {
		rel, limit := filepath.ToSlash(filepath.Join(DevFlowDir, name)), int64(maxTemplateBytes)
		dir := method
		if name == FlowFile || name == DevPromptFile || name == DevPhaseFile || name == DevScenario {
			rel = filepath.ToSlash(filepath.Join(DevFullDir, name))
			dir = exercise
		}
		if name == FlowFile {
			limit = maxFlowBytes
		}
		b, err := readLiteral(filepath.Join(dir, name), limit)
		if err != nil {
			return DevLoaded{}, fmt.Errorf("dev method %s: %w", rel, err)
		}
		loaded.Files[name] = append([]byte(nil), b...)
		loaded.Sources = append(loaded.Sources, SourceRecord{Path: rel, Bytes: len(b), SHA256: sha256Hex(b)})
	}
	loaded.FlowRaw = append([]byte(nil), loaded.Files[FlowFile]...)
	loaded.FlowSum = sha256Hex(loaded.FlowRaw)
	return loaded, nil
}

// RenderDevPrompts interpolates the harness-owned prompts with vals.
// Shared manuals are not templates and are copied unchanged. An unknown
// or unset placeholder in a prompt fails. Every DevPromptKeys entry must
// be set.
func RenderDevPrompts(loaded DevLoaded, vals map[string]string) (DevLoaded, error) {
	for _, k := range DevPromptKeys {
		if vals[k] == "" {
			return DevLoaded{}, fmt.Errorf("dev prompt: unset placeholder %s", k)
		}
	}
	loaded.Rendered = map[string][]byte{}
	for _, name := range []string{DevPromptFile, DevPhaseFile, DevScenario} {
		out, err := renderDevPrompt(name, loaded.Files[name], vals)
		if err != nil {
			return DevLoaded{}, err
		}
		loaded.Rendered[name] = out
		loaded.Records = append(loaded.Records, RenderedRecord{
			Name: name, Sources: []string{filepath.ToSlash(filepath.Join(DevFullDir, name))}, SHA256: sha256Hex(out),
		})
	}
	// The coordinator system input is the literal captured playbook, then
	// the rendered exercise coordinator prompt.
	system := append(append(append([]byte(nil), loaded.Files[DevPlaybook]...), '\n'), loaded.Rendered[DevPromptFile]...)
	loaded.Rendered[DevSystemName] = system
	loaded.Records = append(loaded.Records, RenderedRecord{Name: DevSystemName,
		Sources: []string{filepath.ToSlash(filepath.Join(DevFlowDir, DevPlaybook)), filepath.ToSlash(filepath.Join(DevFullDir, DevPromptFile))},
		SHA256:  sha256Hex(system)})
	return loaded, nil
}

// renderDevPrompt replaces placeholders using only DevPromptKeys.
func renderDevPrompt(name string, tpl []byte, vals map[string]string) ([]byte, error) {
	allowed := map[string]bool{}
	for _, p := range DevPromptKeys {
		allowed[p] = true
	}
	var bad []string
	out := placeholderRE.ReplaceAllFunc(tpl, func(m []byte) []byte {
		k := string(placeholderRE.FindSubmatch(m)[1])
		v, ok := vals[k]
		if !allowed[k] || !ok || v == "" {
			bad = append(bad, k)
			return m
		}
		return []byte(v)
	})
	if len(bad) > 0 {
		return nil, fmt.Errorf("template %s: unknown or unset placeholder(s) %v", name, bad)
	}
	return out, nil
}

// ManualPair returns a role's captured instruction and runbook.
func (l DevLoaded) ManualPair(role string) (instruction, runbook []byte, err error) {
	ins, ok1 := l.Files[manualName(role, "instruction")]
	rb, ok2 := l.Files[manualName(role, "runbook")]
	if !ok1 || !ok2 || len(ins) == 0 || len(rb) == 0 {
		return nil, nil, fmt.Errorf("dev method: role %s has no manual pair", role)
	}
	return ins, rb, nil
}
