package reale2e

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
)

// Shipped templates (design 12a-real-e2e, Coordinator setup and
// provenance): docs/examples/real-e2e holds the flow, the coordinator
// prompt and an instruction/runbook pair per role. Rendering replaces
// placeholders only ({{NAME}}, from a fixed value set); an unknown or
// unreplaced placeholder is an error. Source and rendered hashes are
// recorded in the manifest.

// ExamplesDir is the templates' directory relative to the checkout.
const ExamplesDir = "docs/examples/real-e2e"

// CoordinatorPrompt is the coordinator prompt template's name.
const CoordinatorPrompt = "coordinator-prompt.md"

// FlowFile is the shipped flow's name.
const FlowFile = "flow.json"

// maxTemplateBytes bounds one template.
const maxTemplateBytes = 32 << 10

// TemplateNames are every shipped template, in manifest order.
func TemplateNames() []string {
	out := []string{FlowFile, CoordinatorPrompt}
	for _, h := range Hops {
		out = append(out, manualName(h, "instruction"), manualName(h, "runbook"))
	}
	return out
}

// manualName is a role manual's file name.
func manualName(hop, kind string) string { return hop + "-" + kind + ".md" }

// placeholderRE matches one placeholder.
var placeholderRE = regexp.MustCompile(`\{\{([A-Z0-9_]*)\}\}`)

// Placeholders are the only placeholder names a template may use.
var Placeholders = []string{"RUN_ID", "RUNTIME", "PLANE_URL", "CA_PATH", "CALLSHEET", "REALE2E", "WORKSPACE", "INSTANCE",
	"SEED_PATH", "SEED_COMMIT", "WAITS_DIR", "RECEIPT", "OWNER_DIR", "COORD_DIR"}

// loadTemplates reads every template from dir.
func loadTemplates(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, n := range TemplateNames() {
		b, err := readBounded(filepath.Join(dir, n), maxTemplateBytes)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", n, err)
		}
		out[n] = b
	}
	return out, nil
}

// renderTemplate replaces every placeholder of tpl with its value; a
// placeholder outside Placeholders or without a value fails.
func renderTemplate(name string, tpl []byte, vals map[string]string) ([]byte, error) {
	allowed := map[string]bool{}
	for _, p := range Placeholders {
		allowed[p] = true
	}
	var bad []string
	out := placeholderRE.ReplaceAllFunc(tpl, func(m []byte) []byte {
		k := string(placeholderRE.FindSubmatch(m)[1])
		v, ok := vals[k]
		if !allowed[k] || !ok {
			bad = append(bad, k)
			return m
		}
		return []byte(v)
	})
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("template %s: unknown or unset placeholder(s) %v", name, bad)
	}
	return out, nil
}

// templateHashes lists every template's hash in TemplateNames order.
func templateHashes(t map[string][]byte) []FileHash {
	var out []FileHash
	for _, n := range TemplateNames() {
		out = append(out, FileHash{Name: n, SHA256: sha256Hex(t[n])})
	}
	return out
}
