package catalog

import (
	"fmt"
	"strings"
)

// The frozen publication baseline (design catalog-version, amendment A1):
// the sixteen complete public facts that the MCP publisher manages
// (mcp_config, mcp_timeout, mcp_timeout_override and mcp_progress_extension
// of claude, codex, grok and cursor), extracted verbatim from
// tests/testdata/support-catalog.json at the immutable pre-change commit
// 1038b09 by read-only git inspection: each fact's status, value, ordered
// evidence and verification iteration, with the short-poll policy already
// applied (as shipped there). It is the pre-publication state that a
// catalog's managed facts either still equal or are regenerated from, fact
// by fact, by replaying validated, receipted qualification runs. It is
// offline test data, not a runtime migration or a trust override; nothing
// here reads git or a design file, and a later evidence change never
// updates it.
var publicationBaseline = map[string]map[string]Fact{
	"claude": {
		"mcp_config": {
			Status:                Unverified,
			Value:                 "Verified subset: `claude mcp add --transport stdio --scope user callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`; help supports scoped configuration and `--mcp-config` JSON files/strings. Candidate file {\"mcpServers\":{\"callsheet\":{\"command\":\"callsheet\",\"args\":[\"mcp\",\"--plane\",\"<plane-url>\",\"--ca\",\"<ca-path>\"]}}} has an unverified schema/location; iteration 08 did not measure it (its worker captures prove nothing about coordinator setup).",
			Evidence:              []string{"tests/testdata/cli-help/claude-mcp-add.txt", "tests/testdata/cli-help/claude-help.txt"},
			VerificationIteration: "08",
		},
		"mcp_timeout": {
			Status:                Unverified,
			Value:                 "MCP tool-call timeout is unknown (distinct from startup timeout); iteration 07b must measure it with a local deterministic MCP test server. The default 10s MCP call budget is a deliberately short poll. Long waits use a harness-managed background CLI command. Vendor timeout compatibility is claimed only by named local evidence; an unmeasured client remains UNVERIFIED. Increasing the budget requires local timeout qualification with response margin.",
			Evidence:              []string{"tests/testdata/cli-help/claude-help.txt"},
			VerificationIteration: "07b",
		},
		"mcp_timeout_override": {
			Status:                Unverified,
			Value:                 "How to raise the tool-call timeout is unknown; `MCP_TOOL_TIMEOUT` is a candidate to investigate, not an endorsed setting or value. Iteration 07b must verify syntax and effect.",
			Evidence:              []string{"tests/testdata/cli-help/claude-help.txt"},
			VerificationIteration: "07b",
		},
		"mcp_progress_extension": {
			Status:                Unverified,
			Value:                 "Whether progress notifications extend a call is unknown; iteration 07b must test a delayed tool with progress on and off and check for a hard deadline.",
			Evidence:              []string{"tests/testdata/cli-help/claude-help.txt"},
			VerificationIteration: "07b",
		},
	},
	"codex": {
		"mcp_config": {
			Status:                Unverified,
			Value:                 "Verified subset: `codex mcp add callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`. The manual TOML `[mcp_servers.callsheet]` command/args shape is an unverified candidate; iteration 08 did not measure it (its worker captures prove nothing about coordinator setup).",
			Evidence:              []string{"tests/testdata/cli-help/codex-mcp-add.txt"},
			VerificationIteration: "08",
		},
		"mcp_timeout": {
			Status:                Unverified,
			Value:                 "MCP tool-call timeout is unknown; iteration 07b must measure it with a local deterministic MCP test server. The default 10s MCP call budget is a deliberately short poll. Long waits use a harness-managed background CLI command. Vendor timeout compatibility is claimed only by named local evidence; an unmeasured client remains UNVERIFIED. Increasing the budget requires local timeout qualification with response margin.",
			Evidence:              []string{"tests/testdata/cli-help/codex-help.txt"},
			VerificationIteration: "07b",
		},
		"mcp_timeout_override": {
			Status:                Unverified,
			Value:                 "Candidate `mcp_servers.callsheet.tool_timeout_sec` lacks schema/runtime evidence; iteration 07b must verify it before any value ships.",
			Evidence:              []string{"tests/testdata/cli-help/codex-help.txt"},
			VerificationIteration: "07b",
		},
		"mcp_progress_extension": {
			Status:                Unverified,
			Value:                 "Whether progress extends calls is unknown; iteration 07b must test idle timeout and absolute timeout separately.",
			Evidence:              []string{"tests/testdata/cli-help/codex-help.txt"},
			VerificationIteration: "07b",
		},
	},
	"grok": {
		"mcp_config": {
			Status:                Unverified,
			Value:                 "Verified subset: `grok mcp add --transport stdio --scope user callsheet -- callsheet mcp --plane <plane-url> --ca <ca-path>`; help names `~/.grok/config.toml` and `./.grok/config.toml`. The exact TOML table schema is unverified; iteration 11 uses the vendor registration command rather than inventing tables.",
			Evidence:              []string{"tests/testdata/cli-help/grok-mcp-add.txt", "tests/testdata/cli-help/grok-mcp.txt"},
			VerificationIteration: "11",
		},
		"mcp_timeout": {
			Status:                Unverified,
			Value:                 "MCP tool-call timeout is unknown; iteration 07b must measure it with a local deterministic MCP test server. The default 10s MCP call budget is a deliberately short poll. Long waits use a harness-managed background CLI command. Vendor timeout compatibility is claimed only by named local evidence; an unmeasured client remains UNVERIFIED. Increasing the budget requires local timeout qualification with response margin.",
			Evidence:              []string{"tests/testdata/cli-help/grok-help.txt"},
			VerificationIteration: "07b",
		},
		"mcp_timeout_override": {
			Status:                Unverified,
			Value:                 "No candidate key is established in local help; iteration 07b must find and verify one.",
			Evidence:              []string{"tests/testdata/cli-help/grok-help.txt"},
			VerificationIteration: "07b",
		},
		"mcp_progress_extension": {
			Status:                Unverified,
			Value:                 "Whether progress extends calls is unknown; iteration 07b must test it.",
			Evidence:              []string{"tests/testdata/cli-help/grok-help.txt"},
			VerificationIteration: "07b",
		},
	},
	"cursor": {
		"mcp_config": {
			Status:                Unverified,
			Value:                 "Verified subset: help names `.cursor/mcp.json` and `~/.cursor/mcp.json`; there is no `mcp add` in captured help. The entry schema is unverified (bundle symbols do not prove the file shape); iteration 11 validates it.",
			Evidence:              []string{"tests/testdata/cli-help/cursor-mcp.txt", "tests/testdata/cli-help/cursor-installed-excerpts.json"},
			VerificationIteration: "11",
		},
		"mcp_timeout": {
			Status:                Unverified,
			Value:                 "MCP tool-call timeout is unknown; iteration 07b must measure it with a local deterministic MCP test server. The default 10s MCP call budget is a deliberately short poll. Long waits use a harness-managed background CLI command. Vendor timeout compatibility is claimed only by named local evidence; an unmeasured client remains UNVERIFIED. Increasing the budget requires local timeout qualification with response margin.",
			Evidence:              []string{"tests/testdata/cli-help/cursor-mcp.txt"},
			VerificationIteration: "07b",
		},
		"mcp_timeout_override": {
			Status:                Unverified,
			Value:                 "How to raise the timeout is unknown; iteration 07b must find and verify a setting.",
			Evidence:              []string{"tests/testdata/cli-help/cursor-mcp.txt"},
			VerificationIteration: "07b",
		},
		"mcp_progress_extension": {
			Status:                Unverified,
			Value:                 "The installed bundle contains `resetTimeoutOnProgress` in protocol code, which does not establish the option used at the call site or any maximum timeout; iteration 07b must test progress behaviour.",
			Evidence:              []string{"tests/testdata/cli-help/cursor-installed-excerpts.json"},
			VerificationIteration: "07b",
		},
	},
}

// PublicationBaselineFacts returns a fresh copy of the frozen publication
// baseline, vendor id to managed fact key to fact: new maps and
// deep-copied evidence slices on every call, so no caller can change the
// baseline another sees.
func PublicationBaselineFacts() map[string]map[string]Fact {
	out := make(map[string]map[string]Fact, len(publicationBaseline))
	for id, facts := range publicationBaseline {
		m := make(map[string]Fact, len(facts))
		for key, f := range facts {
			f.Evidence = append([]string(nil), f.Evidence...)
			m[key] = f
		}
		out[id] = m
	}
	return out
}

// publicationBaselineBullets are the three timeout bullets the publisher
// rewrites in each vendor section of docs/support-catalog.md (MCP call
// timeout, How to raise it, Progress extends calls, in that order), taken
// verbatim from that file at the same commit 1038b09 by the same
// read-only inspection: the Markdown half of the pre-publication baseline,
// so a scratch catalog can start from it whatever the checkout has
// published.
var publicationBaselineBullets = map[string][]string{
	"claude": {
		"- **MCP call timeout: UNVERIFIED.** Do not confuse startup timeout with tool-call timeout.",
		"- **How to raise it: UNVERIFIED.** `MCP_TOOL_TIMEOUT` is a candidate to investigate, not an endorsed setting/value.",
		"- **Progress extends calls: UNVERIFIED.** Require a delayed test tool with progress on/off; no assumption that notifications reset the hard deadline.",
	},
	"codex": {
		"- **MCP call timeout: UNVERIFIED.**",
		"- **How to raise it: UNVERIFIED;** candidate `mcp_servers.callsheet.tool_timeout_sec` needs schema/runtime evidence in iteration 07b.",
		"- **Progress extends calls: UNVERIFIED;** test idle timeout and absolute timeout separately.",
	},
	"grok": {
		"- **MCP call timeout: UNVERIFIED.**",
		"- **How to raise it: UNVERIFIED.** No candidate key is established in local help.",
		"- **Progress extends calls: UNVERIFIED.**",
	},
	"cursor": {
		"- **MCP call timeout: UNVERIFIED.**",
		"- **How to raise it: UNVERIFIED.**",
		"- **Progress extends calls: UNVERIFIED.** Installed bundle contains `resetTimeoutOnProgress` in protocol code; this does not establish the option passed at the actual MCP call site, or whether a separate maximum timeout applies.",
	},
}

// supportSections are the support catalog's vendor section headings and
// timeoutBulletAnchors the three publisher-rewritten bullets' openings.
var (
	supportSections      = map[string]string{"claude": "## Claude Code", "codex": "## OpenAI Codex", "grok": "## Grok Build", "cursor": "## Cursor Agent"}
	timeoutBulletAnchors = []string{"- **MCP call timeout:", "- **How to raise it:", "- **Progress extends calls:"}
)

// PublicationBaselineBullets returns a fresh copy of the frozen baseline
// bullets: vendor id to its three timeout bullet lines, in anchor order.
func PublicationBaselineBullets() map[string][]string {
	out := make(map[string][]string, len(publicationBaselineBullets))
	for id, lines := range publicationBaselineBullets {
		out[id] = append([]string(nil), lines...)
	}
	return out
}

// WithPublicationBaselineBullets returns md (a support catalog Markdown)
// with each vendor section's three timeout bullet lines replaced by the
// frozen baseline ones; every other byte is kept. A missing or repeated
// section or bullet is an error.
func WithPublicationBaselineBullets(md string) (string, error) {
	for id, head := range supportSections {
		h := "\n" + head + "\n"
		if strings.Count(md, h) != 1 {
			return "", fmt.Errorf("catalog markdown: no single %q section", head)
		}
		start := strings.Index(md, h) + 1
		end := len(md)
		if i := strings.Index(md[start+len(h)-1:], "\n## "); i >= 0 {
			end = start + len(h) - 1 + i + 1
		}
		lines := strings.SplitAfter(md[start:end], "\n")
		for n, anchor := range timeoutBulletAnchors {
			found := -1
			for i, l := range lines {
				if strings.HasPrefix(l, anchor) {
					if found >= 0 {
						return "", fmt.Errorf("catalog markdown: %s has a duplicate %q bullet", id, anchor)
					}
					found = i
				}
			}
			if found < 0 {
				return "", fmt.Errorf("catalog markdown: %s has no %q bullet", id, anchor)
			}
			lines[found] = publicationBaselineBullets[id][n] + "\n"
		}
		md = md[:start] + strings.Join(lines, "") + md[end:]
	}
	return md, nil
}
