package reale2e

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// mutation corrupts one checker predicate's evidence.
type mutation struct {
	name   string
	crit   string
	code   string
	mutate func(t *testing.T, b string)
}

const zeros40 = "0000000000000000000000000000000000000000"

var zeros64 = strings.Repeat("0", 64)

// hopDirOf is hop i's evidence directory.
func hopDirOf(i int) string { return "hops/" + hopDir(i) }

// setFinal replaces a stored task's final message and final.txt.
func setFinal(t *testing.T, b, dir, final string) {
	editTask(t, b, dir, func(v *contract.TaskView) { v.Result.FinalMessage = &final })
	writeBundleFile(t, b, dir+"/final.txt", []byte(final))
}

// shiftFinished sets a stored task's finish time to its creation plus d.
func shiftFinished(t *testing.T, b, dir string, d time.Duration) {
	editTask(t, b, dir, func(v *contract.TaskView) {
		c, _ := contract.ParseTime(v.CreatedAt)
		f := contract.FormatTime(c.Add(d))
		v.FinishedAt = &f
	})
}

// rebindHop rewrites hop i's workspace binding consistently (request,
// binding and result) with f applied to the binding.
func rebindHop(t *testing.T, b string, i int, f func(w *contract.WorkspaceBinding)) {
	editTask(t, b, hopDirOf(i), func(v *contract.TaskView) {
		f(v.WorkspaceBinding)
		v.Request.Workspace, v.Request.WorkspaceInstance = &v.WorkspaceBinding.Name, &v.WorkspaceBinding.Instance
		v.Request.Base = v.WorkspaceBinding.BaseCommit
		w := *v.Result.Workspace
		w.Name, w.Instance, w.BaseCommit = v.WorkspaceBinding.Name, v.WorkspaceBinding.Instance, v.WorkspaceBinding.BaseCommit
		r := v.Result.WithWorkspace(&w)
		v.Result = &r
	})
}

// hopFilesAt reads hop i's (0 seed) files from the bundle's repository.
func hopFilesAt(t *testing.T, b string, i int) (map[string][]byte, plumbing.Hash) {
	r := evidenceRepo(t, b)
	c := refCommit(t, r, evidenceRefs[i])
	return filesOf(t, c), c.Hash
}

// moveEvent moves the first event of typ (matching hop when set) to just
// before the first event of before.
func moveEvent(evs []Event, typ, hop, before string) []Event {
	i := slices.IndexFunc(evs, func(e Event) bool { return e.Type == typ && (hop == "" || e.Hop == hop) })
	e := evs[i]
	evs = slices.Delete(evs, i, i+1)
	j := slices.IndexFunc(evs, func(e Event) bool { return e.Type == before })
	return slices.Insert(evs, j, e)
}

// checkerMutations is one or more mutation per checker predicate.
func checkerMutations() []mutation {
	m := []mutation{
		// manifest
		{"manifest missing", CritManifest, "manifest_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "manifest.json") }},
		{"run ID", CritManifest, "run_id_invalid", func(t *testing.T, b string) {
			editJSON(t, b, "manifest.json", func(m map[string]any) { m["run_id"] = "bad" })
		}},
		{"platform", CritManifest, "platform_not_linux", func(t *testing.T, b string) {
			editJSON(t, b, "manifest.json", func(m map[string]any) { m["goos"] = "darwin" })
		}},
		{"gate", CritManifest, "gate_not_passed", func(t *testing.T, b string) {
			editDoc(t, b, "manifest.json", func(m *Manifest) { m.Gate.CIAbsent = false })
		}},
		{"revision", CritManifest, "revision_missing", func(t *testing.T, b string) { editDoc(t, b, "manifest.json", func(m *Manifest) { m.Revision = "" }) }},
		{"version eligibility", CritManifest, "version_not_eligible", func(t *testing.T, b string) {
			editDoc(t, b, "manifest.json", func(m *Manifest) { m.Versions[2].Eligible = false })
		}},
		{"versions", CritManifest, "versions_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "manifest.json", func(m *Manifest) { m.Versions = m.Versions[:4] })
		}},
		{"flow missing", CritManifest, "flow_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "setup/flow.json") }},
		{"flow kind", CritManifest, "flow_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "manifest.json", func(m *Manifest) { m.FlowKind = FlowOverride })
		}},
		{"workspace", CritManifest, "workspace_invalid", func(t *testing.T, b string) {
			editDoc(t, b, "manifest.json", func(m *Manifest) { m.Workspace.Instance = "xyz" })
		}},
		{"templates", CritManifest, "template_hashes_missing", func(t *testing.T, b string) {
			editDoc(t, b, "manifest.json", func(m *Manifest) { m.Templates = []FileHash{} })
		}},
		{"rendered", CritManifest, "rendered_hash_mismatch", func(t *testing.T, b string) {
			writeBundleFile(t, b, "setup/coordinator-prompt.md", []byte("an extra instruction\n"))
		}},
		{"started", CritManifest, "started_at_invalid", func(t *testing.T, b string) {
			editDoc(t, b, "manifest.json", func(m *Manifest) { m.StartedAt = "yesterday" })
		}},
		// events
		{"events missing", CritEvents, "events_missing", func(t *testing.T, b string) { writeBundleFile(t, b, "events.jsonl", nil) }},
		{"sequence", CritEvents, "sequence_not_increasing", func(t *testing.T, b string) {
			editEvents(t, b, false, func(e []Event) []Event { e[3].Seq = 99; return e })
		}},
		{"time", CritEvents, "time_invalid", func(t *testing.T, b string) {
			editEvents(t, b, false, func(e []Event) []Event { e[3].Time = "2026-10-10T12:00:00+01:00"; return e })
		}},
		{"monotonic", CritEvents, "time_not_monotonic", func(t *testing.T, b string) {
			editEvents(t, b, false, func(e []Event) []Event { e[5].Time = "2000-01-01T00:00:00Z"; return e })
		}},
		{"first", CritEvents, "run_started_not_first", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event { e[0], e[1] = e[1], e[0]; return e })
		}},
		{"required", CritEvents, "required_event_missing", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				return slices.DeleteFunc(e, func(x Event) bool { return x.Type == EvSidecarsReconnected })
			})
		}},
		{"failed attempt", CritEvents, "attempt_failed", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				return append(e, Event{Type: EvAttemptFailed, Time: e[len(e)-1].Time, Code: "x"})
			})
		}},
		// roles
		{"roles missing", CritRoles, "roles_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "setup/roles.json") }},
		{"role pair", CritRoles, "roles_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "setup/roles.json", func(r *RolesRecord) { r.Roles[2].Effort = "low" })
		}},
		{"role timeout", CritRoles, "roles_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "setup/roles.json", func(r *RolesRecord) { r.Roles[1].Timeout = "1h0m0s" })
		}},
		{"role ready", CritRoles, "role_not_ready", func(t *testing.T, b string) {
			editDoc(t, b, "setup/roles.json", func(r *RolesRecord) { r.Roles[0].CanAccept = false })
		}},
		{"role nodes", CritRoles, "adapter_nodes_inconsistent", func(t *testing.T, b string) {
			editDoc(t, b, "setup/roles.json", func(r *RolesRecord) { r.Roles[2].Node = r.Roles[0].Node })
		}},
		{"ready time", CritRoles, "ready_at_invalid", func(t *testing.T, b string) {
			editDoc(t, b, "setup/roles.json", func(r *RolesRecord) { r.ReadyAt = "" })
		}},
		// preflight
		{"preflight missing", CritPreflight, "preflight_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "preflight/02/task.json") }},
		{"preflight marker", CritPreflight, "preflight_binding_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "preflight/01", func(v *contract.TaskView) { v.Request.Payload = []string{"other"} })
		}},
		{"preflight role", CritPreflight, "preflight_binding_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "preflight/02", func(v *contract.TaskView) { v.Role.ID = "designer" })
		}},
		{"preflight pair", CritPreflight, "preflight_selection_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "preflight/03", func(v *contract.TaskView) { v.Effective.Effort = "low" })
		}},
		{"preflight answer", CritPreflight, "preflight_not_authenticated", func(t *testing.T, b string) { setFinal(t, b, "preflight/04", "NOPE") }},
		{"preflight truncated", CritPreflight, "preflight_not_authenticated", func(t *testing.T, b string) {
			editTask(t, b, "preflight/04", func(v *contract.TaskView) { v.Result.FinalMessageTruncated = true })
		}},
		{"preflight final file", CritPreflight, "preflight_not_authenticated", func(t *testing.T, b string) { removeBundleFile(t, b, "preflight/01/final.txt") }},
		// setup
		{"launch missing", CritSetup, "launch_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "setup/launch.json") }},
		{"session ID", CritSetup, "session_id_invalid", func(t *testing.T, b string) {
			editDoc(t, b, "setup/launch.json", func(l *LaunchRecord) { l.SessionID = "abc" })
		}},
		{"resume flag", CritSetup, "launch_flag_forbidden", func(t *testing.T, b string) {
			editDoc(t, b, "setup/launch.json", func(l *LaunchRecord) { l.Argv = append(l.Argv, "--resume") })
		}},
		{"bare flag", CritSetup, "launch_flag_forbidden", func(t *testing.T, b string) {
			editDoc(t, b, "setup/launch.json", func(l *LaunchRecord) { l.Argv = append(l.Argv, "--bare") })
		}},
		{"session argument", CritSetup, "session_id_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "setup/launch.json", func(l *LaunchRecord) { l.Argv[2] = "other" })
		}},
		{"strict MCP", CritSetup, "launch_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "setup/launch.json", func(l *LaunchRecord) {
				l.Argv = slices.DeleteFunc(l.Argv, func(a string) bool { return a == "--strict-mcp-config" })
			})
		}},
		{"MCP config", CritSetup, "setup_file_missing", func(t *testing.T, b string) { writeBundleFile(t, b, "setup/mcp-config.json", nil) }},
		{"transcript", CritSetup, "transcript_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "session/transcript.jsonl") }},
		{"attestation missing", CritSetup, "attestation_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "session/setup-attestation.json") }},
		{"attestation run", CritSetup, "attestation_binding_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.RunID = "20200101T000000Z-00000000" })
		}},
		{"resumed session", CritSetup, "session_resumed", func(t *testing.T, b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.Resumed = true })
		}},
		{"stale session", CritSetup, "session_resumed", func(t *testing.T, b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.FreshSession = false })
		}},
		{"extra prompt", CritSetup, "setup_not_clean", func(t *testing.T, b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.NoExtraPrompt = false })
		}},
		{"no inputs", CritSetup, "setup_inputs_missing", func(t *testing.T, b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.SetupInputs = []string{} })
		}},
		{"undeclared input", CritSetup, "undeclared_setup_input", func(t *testing.T, b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.SetupInputs = append(a.SetupInputs, "personal-skill") })
		}},
		{"export", CritSetup, "attestation_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "session/setup-attestation.json", func(a *SetupAttestation) { a.TranscriptExport = "" })
		}},
		// event map
		{"map missing", CritEventMap, "event_map_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "session/event-map.json") }},
		{"map attestation", CritEventMap, "event_map_not_attested", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.AttestedByOwner = false })
		}},
		{"map binding", CritEventMap, "event_map_binding_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.TranscriptSHA256 = zeros64 })
		}},
		{"map kind", CritEventMap, "event_kind_unknown", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events = append(m.Events, MapEvent{Kind: "other", Lines: []int{1, 1}}) })
		}},
		{"span range", CritEventMap, "transcript_span_invalid", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events[1].Lines = []int{1, 9999} })
		}},
		{"span shape", CritEventMap, "transcript_span_invalid", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events[1].Lines = []int{3} })
		}},
		{"span record", CritEventMap, "transcript_span_invalid", func(t *testing.T, b string) {
			data, _ := os.ReadFile(filepath.Join(b, "session", "transcript.jsonl"))
			data = []byte("not json\n" + strings.SplitN(string(data), "\n", 2)[1])
			writeBundleFile(t, b, "session/transcript.jsonl", data)
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.TranscriptSHA256 = sha256Hex(data) })
		}},
		{"map incomplete", CritEventMap, "event_map_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events = slices.Delete(m.Events, 3, 4) })
		}},
		{"map task", CritEventMap, "event_map_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events[1].Task = "t_" + strings.Repeat("9", 32) })
		}},
		{"map order", CritEventMap, "event_map_out_of_order", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events[5].Lines = []int{1, 1} })
		}},
		// waits
		{"foreground wait", CritWaits, "wait_not_background", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events[2].Background = false })
		}},
		{"no wake", CritWaits, "automatic_wake_unobserved", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].AutomaticWake = false
				}
			})
		}},
		{"user message", CritWaits, "automatic_wake_unobserved", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].UserMessageBetween = m.Events[i].Kind == MapWaitConsumed
				}
			})
		}},
		{"long poll", CritWaits, "unqualified_long_poll", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				m.Events = append(m.Events, MapEvent{Kind: MapMCPWait, BudgetMS: 60000, Recovery: true, Lines: []int{1, 1}})
			})
		}},
		{"polling only", CritWaits, "unqualified_long_poll", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				m.Events = append(m.Events, MapEvent{Kind: MapMCPWait, BudgetMS: 10000, Lines: []int{1, 1}})
			})
		}},
		{"dispatch wait", CritWaits, "dispatch_with_wait", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) { m.Events[1].BudgetMS = 10000 })
		}},
		{"wait missing", CritWaits, "wait_record_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "hops/02/wait.json") }},
		{"wait winner", CritWaits, "wait_binding_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "hops/03/wait.json", func(w *WaitRecord) { w.Winner = "t_" + strings.Repeat("9", 32) })
		}},
		{"wait exit", CritWaits, "wait_unsuccessful", func(t *testing.T, b string) {
			editDoc(t, b, "hops/01/wait.json", func(w *WaitRecord) { w.ExitCode = ptr(1) })
		}},
		{"wait killed", CritWaits, "wait_unsuccessful", func(t *testing.T, b string) {
			editDoc(t, b, "hops/04/wait.json", func(w *WaitRecord) { w.ExitCode = nil })
		}},
		// hops
		{"hop missing", CritHops, "hop_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "hops/04/task.json") }},
		{"duplicate task", CritHops, "task_not_unique", func(t *testing.T, b string) {
			id := readTask(t, b, "preflight/01").TaskID
			editTask(t, b, "hops/02", func(v *contract.TaskView) { v.TaskID = id })
		}},
		{"hop marker", CritHops, "hop_binding_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "hops/01", func(v *contract.TaskView) { v.Request.Payload = []string{"x"} })
		}},
		{"hop pair", CritHops, "selection_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "hops/03", func(v *contract.TaskView) { v.Effective.Model = "other" })
		}},
		{"hop override", CritHops, "selection_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "hops/02", func(v *contract.TaskView) { v.Request.Override = &contract.TaskOverride{Effort: ptr("high")} })
		}},
		{"hop failed", CritHops, "hop_not_succeeded", func(t *testing.T, b string) {
			editTask(t, b, "hops/01", func(v *contract.TaskView) {
				v.State, v.Result.State, v.Result.ExitCode = contract.TaskFailed, contract.TaskFailed, ptr(1)
			})
		}},
		{"hop marker line", CritHops, "final_marker_missing", func(t *testing.T, b string) { setFinal(t, b, "hops/01", "done\n") }},
		{"hop publication", CritHops, "publication_missing", func(t *testing.T, b string) {
			editTask(t, b, "hops/02", func(v *contract.TaskView) {
				w := *v.Result.Workspace
				e := contract.PubErrPublicationTimeout
				w.Publication, w.Commit, w.Ref, w.Diffstat, w.Error = contract.PublicationFailed, nil, nil, nil, &e
				r := v.Result.WithWorkspace(&w)
				v.Result = &r
			})
		}},
		{"hop logs", CritHops, "logs_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "hops/01/logs.txt") }},
		{"dispatch missing", CritHops, "dispatch_record_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "hops/02/dispatch.json") }},
		{"dispatch base", CritHops, "dispatch_record_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "hops/03/dispatch.json", func(d *DispatchRecord) { d.BaseCommit = zeros40 })
		}},
		// chain
		{"repository missing", CritChain, "repository_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "workspace/repo.git") }},
		{"repository corrupt", CritChain, "repository_invalid", func(t *testing.T, b string) {
			objs, _ := validateRepoLayout(filepath.Join(b, "workspace", "repo.git"), maxRepoFileBytes)
			writeBundleFile(t, b, "workspace/repo.git/"+objs[0], []byte("garbage"))
		}},
		{"tree manifest missing", CritChain, "tree_manifest_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "workspace/tree-manifest.json") }},
		{"seed", CritChain, "seed_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "workspace/tree-manifest.json", func(m *TreeManifest) { m.SeedTree = zeros40 })
		}},
		{"tree manifest hops", CritChain, "tree_manifest_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "workspace/tree-manifest.json", func(m *TreeManifest) { m.Hops = m.Hops[:3] })
		}},
		{"tree manifest tree", CritChain, "tree_manifest_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "workspace/tree-manifest.json", func(m *TreeManifest) { m.Hops[1].Tree = zeros40 })
		}},
		{"instance", CritChain, "workspace_instance_mismatch", func(t *testing.T, b string) {
			rebindHop(t, b, 1, func(w *contract.WorkspaceBinding) { w.Instance = strings.Repeat("e", 32) })
		}},
		{"base", CritChain, "base_binding_mismatch", func(t *testing.T, b string) {
			rebindHop(t, b, 2, func(w *contract.WorkspaceBinding) { w.BaseCommit = ptr(zeros40); w.BaseSelector = zeros40 })
		}},
		{"result commit", CritChain, "result_commit_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "hops/02", func(v *contract.TaskView) {
				w := *v.Result.Workspace
				w.Commit = ptr(strings.Repeat("1", 40))
				r := v.Result.WithWorkspace(&w)
				v.Result = &r
			})
		}},
		{"parent", CritChain, "parent_mismatch", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 2)
			_, seed := hopFilesAt(t, b, 0)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[2], files, seed)
		}},
		{"merge", CritChain, "parent_mismatch", func(t *testing.T, b string) {
			files, h1 := hopFilesAt(t, b, 1)
			_, seed := hopFilesAt(t, b, 0)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[1], files, seed, h1)
		}},
		// artifacts
		{"seed files", CritArtifacts, "seed_incomplete", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 0)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[0], withFiles(files, map[string][]byte{"FEATURE.md": nil}))
		}},
		{"seed feature", CritArtifacts, "seed_has_feature_artifacts", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 0)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[0], withFiles(files, map[string][]byte{"design.md": []byte("early\n")}))
		}},
		{"hop tree", CritArtifacts, "hop_tree_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "workspace/repo.git/refs/heads/hop-03") }},
		{"designer change", CritArtifacts, "disallowed_change", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 1)
			_, seed := hopFilesAt(t, b, 0)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[1], withFiles(files, map[string][]byte{"main.go": []byte("package main\n")}), seed)
		}},
		{"project size", CritArtifacts, "project_too_large", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 1)
			_, seed := hopFilesAt(t, b, 0)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[1], withFiles(files, map[string][]byte{"design.md": bytes.Repeat([]byte("x\n"), 9<<10)}), seed)
		}},
		{"empty design", CritArtifacts, "design_missing", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 1)
			_, seed := hopFilesAt(t, b, 0)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[1], withFiles(files, map[string][]byte{"design.md": []byte(" \n")}), seed)
		}},
		{"review verdict", CritArtifacts, "design_review_not_approved", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 2)
			_, h1 := hopFilesAt(t, b, 1)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[2], withFiles(files, map[string][]byte{"design-review.md": []byte("no\nSTATUS: DESIGN_CHANGES_REQUESTED\n")}), h1)
		}},
		{"coder edits design", CritArtifacts, "disallowed_change", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 3)
			_, h2 := hopFilesAt(t, b, 2)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[3], withFiles(files, map[string][]byte{"design.md": []byte("rewritten\n")}), h2)
		}},
		{"no implementation", CritArtifacts, "implementation_missing", func(t *testing.T, b string) {
			files, _ := hopFilesAt(t, b, 3)
			before, h2 := hopFilesAt(t, b, 2)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[3], withFiles(files, map[string][]byte{"main.go": before["main.go"]}), h2)
		}},
		{"reviewer writes", CritArtifacts, "reviewer_tree_changed", func(t *testing.T, b string) {
			files, h3 := hopFilesAt(t, b, 3)
			addCommit(t, evidenceRepo(t, b), evidenceRefs[4], withFiles(files, map[string][]byte{"review.md": []byte("review\n")}), h3)
		}},
		// owner
		{"decision missing", CritOwner, "decision_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "owner-decision.json") }},
		{"decision no", CritOwner, "owner_did_not_approve", func(t *testing.T, b string) {
			editDoc(t, b, "owner-decision.json", func(d *DecisionRecord) { d.Decision = DecisionNo })
		}},
		{"decision task", CritOwner, "decision_binding_mismatch", func(t *testing.T, b string) {
			id := readTask(t, b, "hops/01").TaskID
			editDoc(t, b, "owner-decision.json", func(d *DecisionRecord) { d.ReviewedTask = id })
		}},
		{"decision commit", CritOwner, "decision_binding_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "owner-decision.json", func(d *DecisionRecord) { d.ResultCommit = zeros40 })
		}},
		{"stale digest", CritOwner, "decision_digest_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "owner-decision.json", func(d *DecisionRecord) { d.DesignSHA256 = zeros64 })
		}},
		{"decision event", CritOwner, "decision_event_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "owner-decision.json", func(d *DecisionRecord) { d.Seq++ })
		}},
		{"two decisions", CritOwner, "decision_event_mismatch", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				i := slices.IndexFunc(e, func(x Event) bool { return x.Type == EvOwnerDecision })
				return slices.Insert(e, i+1, e[i])
			})
		}},
		{"decision order", CritOwner, "decision_out_of_order", func(t *testing.T, b string) {
			var seq int
			editEvents(t, b, true, func(e []Event) []Event {
				e = moveEvent(e, EvOwnerDecision, "", EvAwaitingOwner)
				seq = slices.IndexFunc(e, func(x Event) bool { return x.Type == EvOwnerDecision }) + 1
				return e
			})
			editDoc(t, b, "owner-decision.json", func(d *DecisionRecord) { d.Seq = seq })
		}},
		{"decision deadline", CritOwner, "decision_deadline_exceeded", func(t *testing.T, b string) {
			editEvents(t, b, false, func(e []Event) []Event {
				i := slices.IndexFunc(e, func(x Event) bool { return x.Type == EvAwaitingOwner })
				at, _ := parseTime(e[i].Time)
				e[i].Time = formatTime(at.Add(-21 * time.Minute))
				return e
			})
		}},
		{"premature coding", CritOwner, "coding_before_approval", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event { return moveEvent(e, EvTaskAdmitted, HopCoder, EvReceiptCreated) })
		}},
		{"coder created early", CritOwner, "coding_before_approval", func(t *testing.T, b string) {
			editTask(t, b, "hops/03", func(v *contract.TaskView) {
				c, _ := contract.ParseTime(v.CreatedAt)
				v.CreatedAt = contract.FormatTime(c.Add(-time.Hour))
			})
		}},
		// admissions
		{"premature event", CritAdmissions, "premature_admission", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				return append(e, Event{Type: EvPrematureAdmission, Time: e[len(e)-1].Time, Hop: HopCoder, Task: "t_" + strings.Repeat("9", 32)})
			})
		}},
		{"extra event", CritAdmissions, "extra_admission", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				return append(e, Event{Type: EvExtraAdmission, Time: e[len(e)-1].Time, Hop: HopDesigner, Task: "t_" + strings.Repeat("9", 32)})
			})
		}},
		{"cancelled task", CritAdmissions, "extra_admission", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				return append(e, Event{Type: EvTaskCancelled, Time: e[len(e)-1].Time, Hop: HopDesigner, Task: "t_" + strings.Repeat("9", 32)})
			})
		}},
		{"admission count", CritAdmissions, "admissions_mismatch", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				i := slices.IndexFunc(e, func(x Event) bool { return x.Type == EvTaskAdmitted && x.Hop == HopDesigner })
				return slices.Delete(e, i, i+1)
			})
		}},
		{"admission order", CritAdmissions, "admission_out_of_order", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event {
				i := slices.IndexFunc(e, func(x Event) bool { return x.Type == EvTaskAdmitted && x.Hop == HopCodeReviewer })
				x := e[i]
				e = slices.Delete(e, i, i+1)
				j := slices.IndexFunc(e, func(y Event) bool { return y.Type == EvTaskTerminal && y.Hop == HopCoder })
				return slices.Insert(e, j, x)
			})
		}},
		// grok review
		{"review absent", CritGrok, "review_missing", func(t *testing.T, b string) {
			editTask(t, b, "hops/04", func(v *contract.TaskView) { v.Result.FinalMessage = nil })
		}},
		{"review truncated", CritGrok, "review_missing", func(t *testing.T, b string) {
			editTask(t, b, "hops/04", func(v *contract.TaskView) { v.Result.FinalMessageTruncated = true })
		}},
		{"review verdict", CritGrok, "review_not_approved", func(t *testing.T, b string) {
			setFinal(t, b, "hops/04", "Bugs.\nSTATUS: REVIEW_CHANGES_REQUESTED\n")
		}},
		{"inline absent", CritGrok, "inline_files_invalid", func(t *testing.T, b string) {
			editTask(t, b, "hops/04", func(v *contract.TaskView) { v.Request.Goal = "Review it." })
		}},
		{"inline traversal", CritGrok, "inline_source_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "hops/04", func(v *contract.TaskView) {
				v.Request.Goal = strings.Replace(v.Request.Goal, "BEGIN FILE main.go ", "BEGIN FILE ../main.go ", 1)
				v.Request.Goal = strings.Replace(v.Request.Goal, "END FILE main.go ", "END FILE ../main.go ", 1)
			})
		}},
		{"inline content", CritGrok, "inline_source_mismatch", func(t *testing.T, b string) {
			editTask(t, b, "hops/04", func(v *contract.TaskView) {
				old := string(codedMain)
				i := strings.Index(v.Request.Goal, old)
				head := v.Request.Goal[:i]
				head = strings.Replace(head, "main.go sha256="+sha256Hex(codedMain), "main.go sha256="+sha256Hex([]byte("other\n")), 1)
				v.Request.Goal = head + "other\n" + v.Request.Goal[i+len(old):]
			})
		}},
		{"inline hashes", CritGrok, "inline_hashes_missing", func(t *testing.T, b string) {
			editDoc(t, b, "hops/04/dispatch.json", func(d *DispatchRecord) { d.InlineFiles = []InlineFile{} })
		}},
		{"inline record", CritGrok, "inline_source_mismatch", func(t *testing.T, b string) {
			editDoc(t, b, "hops/04/dispatch.json", func(d *DispatchRecord) { d.InlineFiles[1].SHA256 = zeros64 })
		}},
		// final tests
		{"baseline missing", CritFinalTests, "baseline_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "validation/baseline.json") }},
		{"baseline failed", CritFinalTests, "baseline_failed", func(t *testing.T, b string) {
			editDoc(t, b, "validation/baseline.json", func(r *TestRun) { r.ExitCode = 1 })
		}},
		{"final missing", CritFinalTests, "final_tests_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "validation/final-test.json") }},
		{"final failed", CritFinalTests, "final_tests_failed", func(t *testing.T, b string) {
			editDoc(t, b, "validation/final-test.json", func(r *TestRun) { r.ExitCode = 1 })
		}},
		{"final argv", CritFinalTests, "final_tests_failed", func(t *testing.T, b string) {
			editDoc(t, b, "validation/final-test.json", func(r *TestRun) { r.Argv = []string{"go", "test", "./..."} })
		}},
		{"final env", CritFinalTests, "final_tests_failed", func(t *testing.T, b string) {
			editDoc(t, b, "validation/final-test.json", func(r *TestRun) { r.Env = r.Env[:1] })
		}},
		{"final duration", CritFinalTests, "final_tests_failed", func(t *testing.T, b string) {
			editDoc(t, b, "validation/final-test.json", func(r *TestRun) { r.DurationMS = 61000 })
		}},
		{"final commit", CritFinalTests, "final_tests_failed", func(t *testing.T, b string) {
			editDoc(t, b, "validation/final-test.json", func(r *TestRun) { r.Commit = zeros40 })
		}},
		{"final files", CritFinalTests, "final_tests_failed", func(t *testing.T, b string) {
			editDoc(t, b, "validation/final-test.json", func(r *TestRun) { r.Files = r.Files[1:] })
		}},
		// cleanup
		{"cleanup missing", CritCleanup, "cleanup_missing", func(t *testing.T, b string) { removeBundleFile(t, b, "cleanup.json") }},
		{"cleanup incomplete", CritCleanup, "cleanup_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Complete = false })
		}},
		{"unsettled", CritCleanup, "cleanup_incomplete", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.UnsettledTasks = []string{"t_x"} })
		}},
		{"exit unverified", CritCleanup, "process_exit_unverified", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Processes[0].ExitVerified = false })
		}},
		{"process record", CritCleanup, "process_record_missing", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) {
				c.Processes = slices.DeleteFunc(c.Processes, func(p ProcRecord) bool { return p.Name == "plane" })
			})
		}},
		{"owner children", CritCleanup, "owner_children_unverified", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Owner = nil })
		}},
		{"session open", CritCleanup, "owner_children_unverified", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Owner.SessionClosed = false })
		}},
		{"wait handle exit", CritCleanup, "owner_children_unverified", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Owner.WaitHandles[1].ExitCode = 1 })
		}},
		{"wait handles", CritCleanup, "owner_children_unverified", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Owner.WaitHandles = c.Owner.WaitHandles[:3] })
		}},
		// deadlines
		{"preflight deadline", CritDeadlines, "preflight_deadline_exceeded", func(t *testing.T, b string) {
			shiftFinished(t, b, "preflight/01", 3*time.Minute)
		}},
		{"feature deadline", CritDeadlines, "feature_deadline_exceeded", func(t *testing.T, b string) {
			shiftFinished(t, b, "hops/02", 11*time.Minute)
		}},
		{"attempt deadline", CritDeadlines, "attempt_deadline_exceeded", func(t *testing.T, b string) {
			editEvents(t, b, false, func(e []Event) []Event {
				s := slices.IndexFunc(e, func(x Event) bool { return x.Type == EvPreflightStarted })
				i := slices.IndexFunc(e, func(x Event) bool { return x.Type == EvCollectionStarted })
				at, _ := parseTime(e[s].Time)
				for j := i; j < len(e); j++ {
					e[j].Time = formatTime(at.Add(91 * time.Minute))
				}
				return e
			})
		}},
		{"cleanup deadline", CritDeadlines, "cleanup_deadline_exceeded", func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) {
				s, _ := parseTime(c.StartedAt)
				c.FinishedAt = formatTime(s.Add(3 * time.Minute))
			})
		}},
	}
	return m
}

// TestCheckerMutations (UT-4, UT-6): the passing synthetic bundle passes
// and every independent mutation of a predicate fails that predicate with
// its fixed code (exit 1).
func TestCheckerMutations(t *testing.T) {
	t.Parallel()
	b := passingBundle(t)
	code, r, errOut := checkBundle(t, b)
	if code != 0 || r.Result != ResultPass || len(r.Criteria) != len(criteriaOrder) {
		t.Fatalf("passing bundle = %d %s %v %s", code, r.Result, failedCodes(r), errOut)
	}
	covered := map[string]bool{}
	for _, m := range checkerMutations() {
		covered[m.crit] = true
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			b := passingBundle(t)
			m.mutate(t, b)
			code, r, errOut := checkBundle(t, b)
			if code == 2 {
				t.Fatalf("%s: schema error instead of a failed criterion: %s", m.name, errOut)
			}
			requireCode(t, m.name, code, r, m.crit, m.code)
		})
	}
	for _, c := range criteriaOrder {
		if !covered[c] {
			t.Errorf("criterion %s has no mutation", c)
		}
	}
}

// TestEvidenceCommand (FP-7) is the offline check command at its public
// boundary: a complete synthetic bundle passes with exit 0 and a
// structured report; a corruption of each criterion fails with exit 1;
// malformed evidence is a schema error (exit 2); usage errors exit 2.
func TestEvidenceCommand(t *testing.T) {
	t.Parallel()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := MainWith(Host{GOOS: "linux", GOARCH: "amd64", Args: args, Getenv: func(string) string { return "" },
			LookupEnv: func(string) (string, bool) { return "", true }, Stdout: &out, Stderr: &errOut,
			NewWorld: func(Host) (*World, func(), error) { t.Fatal("check built a live world"); return nil, nil, nil }})
		return code, out.String(), errOut.String()
	}
	b := passingBundle(t)
	code, out, errOut := run("check", "--evidence", b)
	var r Report
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Result != ResultPass || r.Claim != claimFor(ResultPass, FlowDefault) {
		t.Fatalf("passing check = %d %q %q", code, out, errOut)
	}
	// One corruption per criterion through the command.
	seen := map[string]bool{}
	for _, m := range checkerMutations() {
		if seen[m.crit] {
			continue
		}
		seen[m.crit] = true
		b := passingBundle(t)
		m.mutate(t, b)
		code, out, _ := run("check", "--evidence", b)
		var r Report
		json.Unmarshal([]byte(out), &r)
		requireCode(t, m.name, code, r, m.crit, m.code)
	}
	if len(seen) != len(criteriaOrder) {
		t.Fatalf("criteria exercised %d of %d", len(seen), len(criteriaOrder))
	}
	// A schema error.
	bad := passingBundle(t)
	writeBundleFile(t, bad, "manifest.json", []byte("{"))
	if code, out, _ := run("check", "--evidence", bad); code != 2 || !strings.Contains(out, "evidence_schema_error") {
		t.Fatalf("schema error = %d %q", code, out)
	}
	for _, args := range [][]string{{"check"}, {"check", "--evidence", "relative"}, {"check", "--evidence", b, "extra"}, {"check", "--bogus", "x"}} {
		if code, _, errOut := run(args...); code != 2 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v = %d %q", args, code, errOut)
		}
	}
}

// schemaCase makes a bundle unvalidatable.
type schemaCase struct {
	name   string
	mutate func(t *testing.T, b string)
}

// TestCheckerSchemaErrors (UT-6): malformed JSON, duplicate keys, unknown
// fields, schemas and files, special entries, bad UTF-8 and invalid
// repository layouts are refused before any semantic check (exit 2).
func TestCheckerSchemaErrors(t *testing.T) {
	t.Parallel()
	cases := []schemaCase{
		{"malformed json", func(t *testing.T, b string) { writeBundleFile(t, b, "cleanup.json", []byte(`{"schema":`)) }},
		{"duplicate key", func(t *testing.T, b string) {
			data, _ := os.ReadFile(filepath.Join(b, "owner-decision.json"))
			writeBundleFile(t, b, "owner-decision.json", bytes.Replace(data, []byte(`"decision": "yes",`), []byte(`"decision": "yes", "decision": "no",`), 1))
		}},
		{"duplicate nested key", func(t *testing.T, b string) {
			data, _ := os.ReadFile(filepath.Join(b, "manifest.json"))
			writeBundleFile(t, b, "manifest.json", bytes.Replace(data, []byte(`"opt_in": true,`), []byte(`"opt_in": true, "opt_in": true,`), 1))
		}},
		{"unknown field", func(t *testing.T, b string) {
			editJSON(t, b, "cleanup.json", func(m map[string]any) { m["extra"] = 1 })
		}},
		{"trailing data", func(t *testing.T, b string) {
			data, _ := os.ReadFile(filepath.Join(b, "manifest.json"))
			writeBundleFile(t, b, "manifest.json", append(data, []byte("{}")...))
		}},
		{"unknown schema", func(t *testing.T, b string) {
			editJSON(t, b, "workspace/tree-manifest.json", func(m map[string]any) { m["schema"] = "callsheet-real-e2e-trees/v2" })
		}},
		{"dispatch schema", func(t *testing.T, b string) {
			editJSON(t, b, "hops/01/dispatch.json", func(m map[string]any) { m["schema"] = "x" })
		}},
		{"wait schema", func(t *testing.T, b string) {
			editJSON(t, b, "hops/01/wait.json", func(m map[string]any) { m["schema"] = "x" })
		}},
		{"log schema", func(t *testing.T, b string) {
			editJSON(t, b, "hops/01/logs.json", func(m map[string]any) { m["schema"] = "x" })
		}},
		{"unknown event", func(t *testing.T, b string) {
			editEvents(t, b, true, func(e []Event) []Event { return append(e, Event{Type: "bogus", Time: e[0].Time}) })
		}},
		{"malformed event", func(t *testing.T, b string) {
			data, _ := os.ReadFile(filepath.Join(b, "events.jsonl"))
			writeBundleFile(t, b, "events.jsonl", append(data, []byte("{\n")...))
		}},
		{"invalid task", func(t *testing.T, b string) {
			editJSON(t, b, "hops/01/task.json", func(m map[string]any) { m["task"].(map[string]any)["state"] = "bogus" })
		}},
		{"duplicate task key", func(t *testing.T, b string) {
			data, _ := os.ReadFile(filepath.Join(b, "hops", "02", "task.json"))
			writeBundleFile(t, b, "hops/02/task.json", bytes.Replace(data, []byte(`"version": 6,`), []byte(`"version": 6, "version": 6,`), 1))
		}},
		{"invalid preflight", func(t *testing.T, b string) {
			editJSON(t, b, "preflight/01/task.json", func(m map[string]any) { m["version"] = 1 })
		}},
		{"invalid flow", func(t *testing.T, b string) {
			editJSON(t, b, "setup/flow.json", func(m map[string]any) { m["roles"].([]any)[0].(map[string]any)["adapter"] = "cursor" })
		}},
		{"unknown file", func(t *testing.T, b string) { writeBundleFile(t, b, "notes.txt", []byte("x")) }},
		{"temporary file", func(t *testing.T, b string) { writeBundleFile(t, b, "hops/01/.tmp-123", []byte("x")) }},
		{"unknown directory", func(t *testing.T, b string) { os.Mkdir(filepath.Join(b, "extra"), 0o700) }},
		{"symlink", func(t *testing.T, b string) {
			removeBundleFile(t, b, "report.txt")
			if err := os.Symlink(filepath.Join(b, "manifest.json"), filepath.Join(b, "report.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, b string) {
			removeBundleFile(t, b, "report.txt")
			if err := syscall.Mkfifo(filepath.Join(b, "report.txt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"bad utf-8", func(t *testing.T, b string) { writeBundleFile(t, b, "hops/01/final.txt", []byte("bad \xff byte")) }},
		{"oversized file", func(t *testing.T, b string) { truncateTo(t, filepath.Join(b, "report.txt"), maxTextFile+1) }},
		{"aggregate text", func(t *testing.T, b string) {
			names := []string{"hops/01/logs.txt", "hops/02/logs.txt", "hops/03/logs.txt", "hops/04/logs.txt", "report.txt",
				"validation/baseline-stdout.txt", "validation/baseline-stderr.txt", "validation/final-stdout.txt", "validation/final-stderr.txt"}
			for _, n := range names {
				truncateTo(t, filepath.Join(b, filepath.FromSlash(n)), maxTextFile)
			}
		}},
		{"repo hook", func(t *testing.T, b string) {
			writeBundleFile(t, b, "workspace/repo.git/hooks/pre-commit", []byte("#!/bin/sh\n"))
		}},
		{"repo alternates", func(t *testing.T, b string) {
			writeBundleFile(t, b, "workspace/repo.git/objects/info/alternates", []byte("/elsewhere\n"))
		}},
		{"repo pack", func(t *testing.T, b string) {
			writeBundleFile(t, b, "workspace/repo.git/objects/pack/pack-1.pack", []byte("PACK"))
		}},
		{"packed refs", func(t *testing.T, b string) { writeBundleFile(t, b, "workspace/repo.git/packed-refs", []byte("")) }},
		{"shallow", func(t *testing.T, b string) { writeBundleFile(t, b, "workspace/repo.git/shallow", []byte("")) }},
		{"reflog", func(t *testing.T, b string) { writeBundleFile(t, b, "workspace/repo.git/logs/HEAD", []byte("")) }},
		{"extra ref", func(t *testing.T, b string) {
			writeBundleFile(t, b, "workspace/repo.git/refs/heads/extra", []byte(zeros40+"\n"))
		}},
		{"repo symlink", func(t *testing.T, b string) {
			if err := os.Symlink("/etc", filepath.Join(b, "workspace", "repo.git", "objects", "info", "x")); err != nil {
				t.Fatal(err)
			}
		}},
		{"repo bytes", func(t *testing.T, b string) {
			p := filepath.Join(b, "workspace", "repo.git", "objects", "ab", strings.Repeat("c", 38))
			os.MkdirAll(filepath.Dir(p), 0o700)
			truncateTo(t, p, maxRepoFileBytes+1)
		}},
		{"decoded bound", func(t *testing.T, b string) {
			var z bytes.Buffer
			w := zlib.NewWriter(&z)
			w.Write([]byte("blob 70000000\x00"))
			w.Close()
			p := filepath.Join(b, "workspace", "repo.git", "objects", "ab", strings.Repeat("d", 38))
			os.MkdirAll(filepath.Dir(p), 0o700)
			os.WriteFile(p, z.Bytes(), 0o600)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b := passingBundle(t)
			c.mutate(t, b)
			code, r, errOut := checkBundle(t, b)
			if code != 2 || r.Error != "evidence_schema_error" || r.Result != ResultFail || errOut == "" {
				t.Fatalf("%s = %d %+v %q", c.name, code, r, errOut)
			}
		})
	}
}

// truncateTo sets a file's size (sparse; nothing is written).
func truncateTo(t *testing.T, p string, n int64) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(n); err != nil {
		t.Fatal(err)
	}
}

// TestCheckerRepository (UT-6): the collector-shaped repository with every
// empty PlainInit directory passes; a missing object, a lying object size
// and an object stored under another name fail; a non-allowlisted file in
// an empty PlainInit directory is refused.
func TestCheckerRepository(t *testing.T) {
	t.Parallel()
	b := passingBundle(t)
	repo := filepath.Join(b, "workspace", "repo.git")
	for _, d := range []string{"objects/info", "objects/pack", "refs/tags"} {
		es, err := os.ReadDir(filepath.Join(repo, filepath.FromSlash(d)))
		if err != nil || len(es) != 0 {
			t.Fatalf("collector repository lacks the empty %s: %v %v", d, es, err)
		}
	}
	if code, _, _ := checkBundle(t, b); code != 0 {
		t.Fatal("the collector-shaped repository did not pass")
	}
	objects, err := validateRepoLayout(repo, maxRepoFileBytes)
	if err != nil || len(objects) == 0 {
		t.Fatal(objects, err)
	}
	failing := map[string]func(b string){
		"missing object": func(b string) {
			r := evidenceRepo(t, b)
			c := refCommit(t, r, evidenceRefs[4])
			h := c.TreeHash.String()
			os.Remove(filepath.Join(b, "workspace", "repo.git", "objects", h[:2], h[2:]))
		},
		"lying size": func(b string) {
			var z bytes.Buffer
			w := zlib.NewWriter(&z)
			w.Write([]byte("blob 2\x00hello"))
			w.Close()
			writeBundleFile(t, b, "workspace/repo.git/"+objects[0], z.Bytes())
		},
		"wrong name": func(b string) {
			data, _ := os.ReadFile(filepath.Join(b, "workspace", "repo.git", filepath.FromSlash(objects[0])))
			writeBundleFile(t, b, "workspace/repo.git/objects/ee/"+strings.Repeat("e", 38), data)
		},
		"bad header": func(b string) {
			var z bytes.Buffer
			w := zlib.NewWriter(&z)
			w.Write([]byte("weird 1\x00x"))
			w.Close()
			writeBundleFile(t, b, "workspace/repo.git/objects/ee/"+strings.Repeat("e", 38), z.Bytes())
		},
		"head elsewhere": func(b string) {
			writeBundleFile(t, b, "workspace/repo.git/HEAD", []byte("ref: refs/heads/hop-01\n"))
		},
	}
	for name, f := range failing {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := passingBundle(t)
			f(b)
			code, r, errOut := checkBundle(t, b)
			if code != 1 || r.Result != ResultFail {
				t.Fatalf("%s = %d %v %s", name, code, failedCodes(r), errOut)
			}
		})
	}
	t.Run("file in an empty PlainInit directory", func(t *testing.T) {
		t.Parallel()
		b := passingBundle(t)
		writeBundleFile(t, b, "workspace/repo.git/refs/tags/v1", []byte(zeros40+"\n"))
		if code, _, _ := checkBundle(t, b); code != 2 {
			t.Fatalf("a file in refs/tags = %d", code)
		}
	})
	t.Run("empty unknown directory", func(t *testing.T) {
		t.Parallel()
		b := passingBundle(t)
		os.MkdirAll(filepath.Join(b, "workspace", "repo.git", "branches"), 0o700)
		if code, _, _ := checkBundle(t, b); code != 0 {
			t.Fatalf("a tolerated empty directory = %d", code)
		}
	})
	t.Run("decoded aggregate", func(t *testing.T) {
		t.Parallel()
		if err := verifyLooseObjects(repo, objects, 10); !errors.Is(err, errObjectBound) {
			t.Fatalf("aggregate bound = %v", err)
		}
	})
}

// TestCheckerRoots (UT-6): a root reached through a symlinked ancestor
// (macOS /var → /private/var) is accepted; a root that is itself not a
// directory, or missing, is refused.
func TestCheckerRoots(t *testing.T) {
	t.Parallel()
	b := passingBundle(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Dir(b), link); err != nil {
		t.Fatal(err)
	}
	if code, r, _ := checkBundle(t, filepath.Join(link, filepath.Base(b))); code != 0 || r.Result != ResultPass {
		t.Fatalf("symlinked ancestor = %d %v", code, failedCodes(r))
	}
	if code, _, _ := checkBundle(t, filepath.Join(b, "manifest.json")); code != 2 {
		t.Fatal("a file root passed")
	}
	if code, _, _ := checkBundle(t, filepath.Join(b, "missing")); code != 2 {
		t.Fatal("a missing root passed")
	}
}

// TestReportDeterministic (UT-6): the same bundle yields byte-identical
// reports, and the report carries no prose, prompt or log.
func TestReportDeterministic(t *testing.T) {
	t.Parallel()
	b := passingBundle(t)
	var a1, a2, e bytes.Buffer
	CheckEvidence(b, Redactor{}, &a1, &e)
	CheckEvidence(b, Redactor{}, &a2, &e)
	if !bytes.Equal(a1.Bytes(), a2.Bytes()) {
		t.Fatal("reports differ")
	}
	for _, leak := range []string{"Looks right", "Implement the approved", string(designDoc[:10]), "log of t_"} {
		if strings.Contains(a1.String(), leak) {
			t.Fatalf("the report leaks %q", leak)
		}
	}
	text := RenderText(Report{Result: ResultFail, Error: "evidence_schema_error", Criteria: []Criterion{{ID: "x", Result: "fail", Codes: []string{"a"}}}})
	if !strings.Contains(text, "error: evidence_schema_error") || !strings.Contains(text, "criterion x: fail (a)") {
		t.Fatal(text)
	}
}

// TestRedaction (UT-6): known prefixes become tokens (the longest first),
// URL credentials and credential-shaped fields are replaced.
func TestRedaction(t *testing.T) {
	t.Parallel()
	r := Redactor{Home: "/home/ada", Run: "/tmp/ce-123", Checkout: "/home/ada/src/callsheet"}
	for in, want := range map[string]string{
		"/home/ada/src/callsheet/design/x":  "$CHECKOUT/design/x",
		"/home/ada/.local/bin/claude":       "$HOME/.local/bin/claude",
		"/tmp/ce-123/p/pki/ca.crt":          "$RUN/p/pki/ca.crt",
		"/tmp/ce-abc9/s.sock":               "$RUN/s.sock",
		"https://user:pw@host/x":            "https://REDACTED@host/x",
		"token=abcdef api_key: xyz":         "token=REDACTED api_key: REDACTED",
		"Authorization: Bearer abc.def":     "Authorization: REDACTED abc.def",
		"key sk-abcdefghijklmnopqrstu done": "key REDACTED done",
		"plain 2.1.290 (Claude Code)":       "plain 2.1.290 (Claude Code)",
	} {
		if got := r.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	// The report redacts version strings of the manifest.
	b := passingBundle(t)
	editDoc(t, b, "manifest.json", func(m *Manifest) {
		m.Versions[0].Version = "2.1.290 token=secret123"
		m.Versions[0].Path = "/home/ada/bin/claude"
	})
	bundle, err := LoadBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	rep := BuildReport(bundle, Check(bundle), Redactor{Home: "/home/ada"})
	if rep.Versions[0].Version != "2.1.290 token=REDACTED" || rep.Versions[0].Path != "$HOME/bin/claude" {
		t.Fatalf("report versions %+v", rep.Versions[0])
	}
	if claimFor(ResultPass, FlowOverride) == claimFor(ResultPass, FlowDefault) || claimFor(ResultFail, FlowDefault) != "not demonstrated" {
		t.Fatal("claims do not distinguish override flows")
	}
}
