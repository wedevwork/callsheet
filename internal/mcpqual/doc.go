// Package mcpqual is iteration 07b's developer-only MCP timeout qualification
// harness (FP-10..FP-16): a deterministic, bounded newline-JSON-RPC probe
// server with one slow tool, strict version-1 plan, case-file and report
// schemas, four version-keyed vendor transcript decoders, the measurement
// scheduler, redacted evidence and catalog-patch publication, and
// process-group cleanup built on internal/spikes/processgroup. Design
// decoder-enrollment adds the decoder-free setup capture (CaptureRunner,
// the mcpqual-capture-v1 manifest) and the offline enrollment validators
// (the mcpqual-enrollment-v1 index, expected oracles, the redaction fixed
// point and replay) that let a qualified decoder version claim only the
// event capabilities a reviewed real transcript demonstrated.
//
// It is reached only through the developer executable cmd/mcpqual. The
// shipped callsheet runtime never imports it and never reads its evidence.
// The probe deliberately duplicates the small subset of internal/mcp's codec
// it needs rather than sharing the production server (design 07b, option b):
// its behaviour is independently tested and not claimed as production
// protocol parity.
//
// Vendor model sessions run only under an explicit --allow-model-calls, never
// when CI is present (even empty), and never from tests: unit tests inject fake launchers and
// clocks, function tests launch fake vendor fixtures. Nothing observed
// through a decoder version without a redacted actual transcript fixture is
// ever published as a VERIFIED catalog fact.
package mcpqual
