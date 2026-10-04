package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

const (
	wsInst = "0123456789abcdef0123456789abcdef"
	wsGen  = "fedcba9876543210fedcba9876543210"
	wsHash = "0123456789abcdef0123456789abcdef01234567"
)

func wsView(name string) contract.WorkspaceView {
	return contract.WorkspaceView{Name: name, Instance: wsInst, CreatedAt: "2026-09-30T12:00:00Z", Generation: wsGen,
		DefaultBranch: contract.DefaultBranchRef, SizeBytes: 123, Retention: contract.NewRetention()}
}

// happyWs answers every workspace operation with a valid result.
func happyWs(_ context.Context, method string, args ...any) (any, error) {
	switch method {
	case "CreateWorkspace", "ShowWorkspace":
		return wsView(args[0].(string)), nil
	case "ListWorkspaces":
		return contract.WorkspaceListResponse{Workspaces: []contract.WorkspaceSummary{{Name: "a", Instance: wsInst, CreatedAt: "2026-09-30T12:00:00Z"}}}, nil
	case "RemoveWorkspace":
		return contract.WorkspaceRemoveResponse{Name: args[0].(string), Instance: args[1].(string), Removed: true}, nil
	case "PruneWorkspace":
		return contract.WorkspacePruneResponse{Name: args[0].(string), Instance: args[1].(string), Generation: wsGen}, nil
	case "SetWorkspaceRef":
		h := wsHash
		return contract.WorkspaceRefSetResponse{Name: args[0].(string), Instance: wsInst, Generation: wsGen, Ref: "refs/heads/main", NewCommit: &h}, nil
	case "WorkspaceStatus":
		return contract.WorkspaceStatusResponse{Name: args[0].(string), Instance: wsInst, Generation: wsGen, DefaultBranch: contract.DefaultBranchRef, Refs: []contract.WorkspaceRef{}}, nil
	case "WorkspaceDiff":
		return contract.WorkspaceDiffResponse{Name: args[0].(string), Instance: wsInst, Generation: wsGen, TargetCommit: wsHash, Changes: []contract.WorkspaceChange{}}, nil
	}
	return nil, errUnscripted
}

// wsSchemaCases are TestSchemas's workspace examples: schema validity and
// the tool's own acceptance agree, and contract-only rules still refuse.
func wsSchemaCases() []struct {
	tool, args string
	schemaOK   bool
	accepted   bool
} {
	long := strings.Repeat("a", 200)
	full512 := "refs/heads/" + long + "/" + long + "/" + strings.Repeat("b", 99)
	return []struct {
		tool, args string
		schemaOK   bool
		accepted   bool
	}{
		{toolWsCreate, `{"name":"alpha"}`, true, true},
		{toolWsCreate, `{"name":"Alpha"}`, false, false},
		{toolWsCreate, `{}`, false, false},
		{toolWsCreate, `{"name":"a","x":1}`, false, false},
		{toolWsLs, `{}`, true, true},
		{toolWsLs, `{"after":"a","limit":1}`, true, true},
		{toolWsLs, `{"limit":0}`, false, false},
		{toolWsLs, `{"limit":101}`, false, false},
		{toolWsShow, `{"name":"alpha"}`, true, true},
		{toolWsShow, `{"name":""}`, false, false},
		{toolWsRm, `{"name":"alpha","instance":"` + wsInst + `"}`, true, true},
		{toolWsRm, `{"name":"alpha"}`, false, false},
		{toolWsRm, `{"name":"alpha","instance":"XYZ"}`, false, false},
		{toolWsPrune, `{"name":"alpha","instance":"` + wsInst + `","before":"2026-09-30T12:00:00Z"}`, true, true},
		{toolWsPrune, `{"name":"alpha","instance":"` + wsInst + `","before":"2026-09-30T12:00:00"}`, true, false}, // no zone: contract only
		{toolWsPrune, `{"name":"alpha","instance":"` + wsInst + `"}`, false, false},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"main","expected":"absent","target":"` + wsHash + `"}`, true, true},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"main","expected":"` + wsHash + `","delete":true}`, true, true},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"` + full512 + `","expected":"absent","target":"main"}`, true, true},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"` + full512 + `b","expected":"absent","target":"main"}`, false, false},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"Main","expected":"absent","target":"main"}`, false, false},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"café","expected":"absent","target":"main"}`, false, false},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"a..b","expected":"absent","target":"main"}`, true, false},   // go-git rule: contract only
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"x.lock","expected":"absent","target":"main"}`, true, false}, // contract only
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"main","expected":"absent","delete":true}`, true, false},     // delete needs a hash
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"main","expected":"absent"}`, true, false},                   // neither target nor delete
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"main","expected":"absent","target":"HEAD"}`, false, false},
		{toolWsStatus, `{"name":"alpha"}`, true, true},
		{toolWsStatus, `{"name":"alpha","after":"refs/heads/main","instance":"` + wsInst + `","generation":"` + wsGen + `"}`, true, true},
		{toolWsStatus, `{"name":"alpha","after":"refs/heads/main"}`, true, false}, // partial continuation: contract only
		{toolWsStatus, `{"name":"alpha","after":"refs/heads/Main","instance":"` + wsInst + `","generation":"` + wsGen + `"}`, false, false},
		{toolWsDiff, `{"name":"alpha","base":"empty","target":"main"}`, true, true},
		{toolWsDiff, `{"name":"alpha","base":"empty","target":"` + wsHash + `","after":"YQ==","instance":"` + wsInst + `","generation":"` + wsGen + `"}`, true, true},
		{toolWsDiff, `{"name":"alpha","base":"main"}`, true, false},                               // missing target: a runtime form rule (iteration 10c flat schema)
		{toolWsDiff, `{"name":"alpha","base":"empty","target":"empty"}`, true, true},              // a target "empty" is the branch refs/heads/empty
		{toolWsDiff, `{"name":"alpha","base":"empty","target":"refs/tags/v1"}`, true, false},      // full refs outside refs/heads/: contract only
		{toolWsDiff, `{"name":"alpha","base":"empty","target":"main","after":"YQ"}`, true, false}, // noncanonical base64: contract only
	}
}

// UT-7: the workspace tools relay one client operation each, return the
// exact API object as compact JSON, carry truthful annotations and
// plane-only descriptions, and never repair invalid UTF-8.
func TestWorkspaceTools(t *testing.T) {
	h := start(t)
	h.ready()
	var got []any
	h.fake.ws = func(ctx context.Context, method string, args ...any) (any, error) {
		got = append([]any{method}, args...)
		return happyWs(ctx, method, args...)
	}
	a := h.ask(toolWsCreate, `{"name":"alpha"}`)
	want, _ := contract.Encode(wsView("alpha"))
	if a.isError || a.text != string(want) {
		t.Fatalf("create %+v", a)
	}
	h.ask(toolWsRefSet, `{"name":"alpha","instance":"`+wsInst+`","branch":"refs/heads/topic/x","expected":"absent","target":"main"}`)
	req := got[2].(contract.WorkspaceRefSetRequest)
	if got[0] != "SetWorkspaceRef" || got[1] != "alpha" || req.Branch != "refs/heads/topic/x" || *req.Target != "main" || req.Delete != nil {
		t.Fatalf("ref set relay %v", got)
	}
	h.ask(toolWsStatus, `{"name":"alpha","limit":5}`)
	if p := got[2].(client.StatusPage); p.Limit != 5 || p.After != "" {
		t.Fatalf("status relay %v", got)
	}
	h.ask(toolWsDiff, `{"name":"alpha","base":"empty","target":"main"}`)
	// Iteration 10c: explicit selectors are relayed canonically.
	if p := got[2].(client.DiffPage); p.Base != "empty" || p.Target != "refs/heads/main" || p.Limit != contract.DefaultWorkspaceLimit {
		t.Fatalf("diff relay %v", got)
	}
	// Invalid UTF-8 (an unpaired surrogate escape), the JSON replacement
	// character, case and normalization variants are invalid_argument,
	// never repaired, before any client call.
	before := len(h.fake.called())
	for _, b := range []string{`"ma�in"`, "\"ma�in\"", `"Main"`, `"café"`, `"café"`} {
		e := h.ask(toolWsRefSet, `{"name":"alpha","instance":"`+wsInst+`","branch":`+b+`,"expected":"absent","target":"main"}`).errorOf(t)
		if e.Code != contract.CodeInvalidArgument {
			t.Fatalf("%s: %+v", b, e)
		}
	}
	// Invalid UTF-8 cannot be JSON: raw invalid bytes and unpaired
	// surrogate escapes are refused at framing (parse error) and no tool
	// runs.
	for _, b := range []string{"\"main\xff\"", `"main\udcff"`} {
		h.send(`{"jsonrpc":"2.0","id":900,"method":"tools/call","params":{"name":"ws_ref_set","arguments":{"branch":` + b + `}}}`)
		if code := protoCode(t, h.nextRaw()); code != codeParse {
			t.Fatalf("%q = %d", b, code)
		}
	}
	if len(h.fake.called()) != before {
		t.Fatalf("invalid names reached the client: %v", h.fake.called()[before:])
	}
	// A plane answer is relayed exactly; a lost mutation answer carries
	// inspection guidance; reads carry none.
	lost := contract.Wrap(contract.CodeUnavailable, "cannot reach the plane at https://p: timed out", errors.New("SECRET cause"))
	h.fake.ws = func(context.Context, string, ...any) (any, error) { return nil, lost }
	for _, c := range []struct{ tool, args string }{
		{toolWsCreate, `{"name":"alpha"}`},
		{toolWsRm, `{"name":"alpha","instance":"` + wsInst + `"}`},
		{toolWsPrune, `{"name":"alpha","instance":"` + wsInst + `","before":"2026-09-30T12:00:00Z"}`},
		{toolWsRefSet, `{"name":"alpha","instance":"` + wsInst + `","branch":"main","expected":"absent","target":"main"}`},
	} {
		a := h.ask(c.tool, c.args)
		if e := a.errorOf(t); e.Code != contract.CodeUnavailable || !strings.Contains(e.Message, "ws_show or ws_status") || strings.Contains(a.text, "SECRET") {
			t.Fatalf("%s lost: %+v", c.tool, a)
		}
	}
	if e := h.ask(toolWsShow, `{"name":"alpha"}`).errorOf(t); strings.Contains(e.Message, "ws_show or ws_status") {
		t.Fatalf("read guidance %+v", e)
	}
	conflict := contract.New(contract.CodeConflict, "stale expected value")
	h.fake.ws = func(context.Context, string, ...any) (any, error) { return nil, conflict }
	if e := h.ask(toolWsRefSet, `{"name":"alpha","instance":"`+wsInst+`","branch":"main","expected":"absent","target":"main"}`).errorOf(t); e.Code != contract.CodeConflict || e.Message != conflict.Message {
		t.Fatalf("conflict %+v", e)
	}
	// Null is never an omitted optional.
	if e := h.ask(toolWsLs, `{"after":null}`).errorOf(t); e.Code != contract.CodeInvalidArgument {
		t.Fatalf("null %+v", e)
	}
}

// UT-7: annotations and descriptions of the workspace tools.
func TestWorkspaceToolDefinitions(t *testing.T) {
	tools := workspaceTools()
	if len(tools) != 8 {
		t.Fatalf("%d workspace tools", len(tools))
	}
	for _, tl := range tools {
		if !strings.Contains(tl.description, "operates on the plane") || !strings.Contains(tl.description, "never on a local repository") {
			t.Fatalf("%s description %q", tl.name, tl.description)
		}
		ro, _ := tl.annotations["readOnlyHint"].(bool)
		de, _ := tl.annotations["destructiveHint"].(bool)
		id, _ := tl.annotations["idempotentHint"].(bool)
		switch tl.name {
		case toolWsLs, toolWsShow, toolWsStatus, toolWsDiff:
			if !ro {
				t.Fatalf("%s must be read-only", tl.name)
			}
		case toolWsCreate, toolWsRefSet:
			if ro || de || id {
				t.Fatalf("%s must be mutating, non-destructive, non-idempotent: %v", tl.name, tl.annotations)
			}
		case toolWsRm, toolWsPrune:
			if ro || !de || id {
				t.Fatalf("%s must be destructive and non-idempotent: %v", tl.name, tl.annotations)
			}
		}
		s := schemaFor(tl.name)
		if s["additionalProperties"] != false {
			t.Fatalf("%s schema is not strict", tl.name)
		}
		for k := range s["properties"].(schema) {
			switch k {
			case "content", "data", "patch", "command", "config", "blob", "file", "path":
				t.Fatalf("%s has a content-like argument %s", tl.name, k)
			}
		}
	}
	if schemaFor("no_such_tool") != nil {
		t.Fatal("unknown tool has a schema")
	}
}
