package client

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The workspace operations (iteration 09a). Like the role and task
// operations they validate their inputs before any request, decode the
// plane's answer strictly (bounds, grammars, ordering and the requested
// name), never cache and never retry: after an ambiguous mutation answer
// inspect with ShowWorkspace or WorkspaceStatus.

func workspacePath(name string, parts ...string) string {
	p := contract.PathWorkspaces + "/" + name
	for _, s := range parts {
		p += "/" + s
	}
	return p
}

func checkName(name string) error {
	if !contract.ValidWorkspaceName(name) {
		return contract.InvalidWorkspaceName()
	}
	return nil
}

func checkLimit(limit int) error {
	if limit < 1 || limit > contract.MaxWorkspaceLimit {
		return contract.New(contract.CodeInvalidArgument, "limit must be an integer from 1 to "+strconv.Itoa(contract.MaxWorkspaceLimit))
	}
	return nil
}

func (c *Client) workspaceRequest(ctx context.Context, method, path string, body any, want int) ([]byte, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = encodeBody(body); err != nil {
			return nil, err
		}
	}
	status, resp, err := c.request(ctx, method, path, b, contract.MaxWorkspaceResponse)
	if err != nil {
		return nil, err
	}
	if status != want {
		return nil, invalidResponse("unexpected HTTP status for a workspace operation")
	}
	return resp, nil
}

func sameView(v contract.WorkspaceView, name string) (contract.WorkspaceView, error) {
	if v.Name != name {
		return contract.WorkspaceView{}, invalidResponse("it names another workspace")
	}
	return v, nil
}

// CreateWorkspace creates the empty workspace name. A duplicate always
// conflicts, an identical retry included: after an ambiguous answer read
// ShowWorkspace.
func (c *Client) CreateWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error) {
	if err := checkName(name); err != nil {
		return contract.WorkspaceView{}, err
	}
	b, err := c.workspaceRequest(ctx, http.MethodPost, contract.PathWorkspaces, contract.WorkspaceCreateRequest{Name: name}, http.StatusCreated)
	if err != nil {
		return contract.WorkspaceView{}, err
	}
	v, err := contract.ParseWorkspaceView(b)
	if err != nil {
		return v, err
	}
	return sameView(v, name)
}

// ListWorkspaces returns one page sorted by name after the exclusive
// cursor (empty: the first page).
func (c *Client) ListWorkspaces(ctx context.Context, after string, limit int) (contract.WorkspaceListResponse, error) {
	if err := checkLimit(limit); err != nil {
		return contract.WorkspaceListResponse{}, err
	}
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	q.Set("limit", strconv.Itoa(limit))
	b, err := c.workspaceRequest(ctx, http.MethodGet, contract.PathWorkspaces+"?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return contract.WorkspaceListResponse{}, err
	}
	return contract.ParseWorkspaceList(b, after, limit)
}

// ShowWorkspace returns name's view.
func (c *Client) ShowWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error) {
	if err := checkName(name); err != nil {
		return contract.WorkspaceView{}, err
	}
	b, err := c.workspaceRequest(ctx, http.MethodGet, workspacePath(name), nil, http.StatusOK)
	if err != nil {
		return contract.WorkspaceView{}, err
	}
	v, err := contract.ParseWorkspaceView(b)
	if err != nil {
		return v, err
	}
	return sameView(v, name)
}

// RemoveWorkspace removes the whole workspace name of instance.
func (c *Client) RemoveWorkspace(ctx context.Context, name, instance string) (contract.WorkspaceRemoveResponse, error) {
	if err := checkName(name); err != nil {
		return contract.WorkspaceRemoveResponse{}, err
	}
	if err := contract.ValidateInstance(instance); err != nil {
		return contract.WorkspaceRemoveResponse{}, err
	}
	b, err := c.workspaceRequest(ctx, http.MethodDelete, workspacePath(name), contract.WorkspaceRemoveRequest{Instance: instance}, http.StatusOK)
	if err != nil {
		return contract.WorkspaceRemoveResponse{}, err
	}
	r, err := contract.ParseWorkspaceRemove(b)
	if err == nil && (r.Name != name || r.Instance != instance) {
		return r, invalidResponse("it names another workspace or instance")
	}
	return r, err
}

// PruneWorkspace prunes task refs published strictly before before (an
// RFC3339 timestamp with a zone) and collects unreachable objects.
func (c *Client) PruneWorkspace(ctx context.Context, name, instance, before string) (contract.WorkspacePruneResponse, error) {
	if err := checkName(name); err != nil {
		return contract.WorkspacePruneResponse{}, err
	}
	if err := contract.ValidateInstance(instance); err != nil {
		return contract.WorkspacePruneResponse{}, err
	}
	if _, err := contract.ParseCutoff(before); err != nil {
		return contract.WorkspacePruneResponse{}, err
	}
	b, err := c.workspaceRequest(ctx, http.MethodPost, workspacePath(name, "prune"), contract.WorkspacePruneRequest{Instance: instance, Before: before}, http.StatusOK)
	if err != nil {
		return contract.WorkspacePruneResponse{}, err
	}
	r, err := contract.ParseWorkspacePrune(b)
	if err == nil && (r.Name != name || r.Instance != instance) {
		return r, invalidResponse("it names another workspace or instance")
	}
	return r, err
}

// SetWorkspaceRef creates, moves or deletes one branch with CAS.
func (c *Client) SetWorkspaceRef(ctx context.Context, name string, req contract.WorkspaceRefSetRequest) (contract.WorkspaceRefSetResponse, error) {
	if err := checkName(name); err != nil {
		return contract.WorkspaceRefSetResponse{}, err
	}
	in, err := contract.ValidateRefSet(req)
	if err != nil {
		return contract.WorkspaceRefSetResponse{}, err
	}
	if req.Delete != nil && !*req.Delete {
		req.Delete = nil
	}
	b, err := c.workspaceRequest(ctx, http.MethodPost, workspacePath(name, "refs", "set"), req, http.StatusOK)
	if err != nil {
		return contract.WorkspaceRefSetResponse{}, err
	}
	r, err := contract.ParseWorkspaceRefSet(b)
	if err == nil && (r.Name != name || r.Instance != in.Instance || r.Ref != in.Ref || (r.NewCommit == nil) != in.Delete) {
		return r, invalidResponse("it names another workspace, instance or ref")
	}
	return r, err
}

// StatusPage selects one status page: the first page leaves After,
// Instance and Generation empty; a continuation gives all three.
type StatusPage struct {
	After, Instance, Generation string
	Limit                       int
}

func checkContinuation(after, instance, generation string) error {
	if (after == "") != (instance == "") || (after == "") != (generation == "") {
		return contract.New(contract.CodeInvalidArgument, "a continuation needs after, instance and generation together; the first page omits all three")
	}
	if instance != "" && (!contract.ValidWorkspaceToken(instance) || !contract.ValidWorkspaceToken(generation)) {
		return contract.New(contract.CodeInvalidArgument, "instance and generation must be 32-hex tokens from the previous page")
	}
	return nil
}

// WorkspaceStatus returns one page of name's stored refs.
func (c *Client) WorkspaceStatus(ctx context.Context, name string, p StatusPage) (contract.WorkspaceStatusResponse, error) {
	if err := checkName(name); err != nil {
		return contract.WorkspaceStatusResponse{}, err
	}
	if err := checkLimit(p.Limit); err != nil {
		return contract.WorkspaceStatusResponse{}, err
	}
	if err := checkContinuation(p.After, p.Instance, p.Generation); err != nil {
		return contract.WorkspaceStatusResponse{}, err
	}
	if p.After != "" && !contract.ValidStatusCursor(p.After) {
		return contract.WorkspaceStatusResponse{}, contract.New(contract.CodeInvalidArgument, "after must be a complete portable branch ref (refs/heads/...) or task ref (refs/callsheet/tasks/...) from the previous page")
	}
	q := url.Values{}
	if p.After != "" {
		q.Set("after", p.After)
		q.Set("instance", p.Instance)
		q.Set("generation", p.Generation)
	}
	q.Set("limit", strconv.Itoa(p.Limit))
	b, err := c.workspaceRequest(ctx, http.MethodGet, workspacePath(name, "status")+"?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return contract.WorkspaceStatusResponse{}, err
	}
	r, err := contract.ParseWorkspaceStatus(b, p.After, p.Limit)
	if err == nil && (r.Name != name || (p.Instance != "" && (r.Instance != p.Instance || r.Generation != p.Generation))) {
		return r, invalidResponse("it names another workspace, instance or generation")
	}
	return r, err
}

// DiffPage selects one diff page; a continuation gives After (the
// previous next_after), Instance and Generation, and full hashes (or
// empty) as selectors.
type DiffPage struct {
	Base, Target, After, Instance, Generation string
	Limit                                     int
}

// WorkspaceDiff returns one page of changed-path metadata between base
// and target.
func (c *Client) WorkspaceDiff(ctx context.Context, name string, p DiffPage) (contract.WorkspaceDiffResponse, error) {
	if err := checkName(name); err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	if err := checkLimit(p.Limit); err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	base, err := contract.ParseSelector(p.Base, "base", true)
	if err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	target, err := contract.ParseSelector(p.Target, "target", false)
	if err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	if err := checkContinuation(p.After, p.Instance, p.Generation); err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	var after []byte
	if p.After != "" {
		if after, err = contract.ParsePathCursor(p.After); err != nil {
			return contract.WorkspaceDiffResponse{}, err
		}
		if (base.Kind != contract.SelectorKindHash && base.Kind != contract.SelectorKindEmpty) || target.Kind != contract.SelectorKindHash {
			return contract.WorkspaceDiffResponse{}, contract.New(contract.CodeInvalidArgument, "a continuation must name base and target by the previous page's full commit hashes (or empty for the base)")
		}
	}
	q := url.Values{}
	q.Set("base", p.Base)
	q.Set("target", p.Target)
	if p.After != "" {
		q.Set("after", p.After)
		q.Set("instance", p.Instance)
		q.Set("generation", p.Generation)
	}
	q.Set("limit", strconv.Itoa(p.Limit))
	b, err := c.workspaceRequest(ctx, http.MethodGet, workspacePath(name, "diff")+"?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return contract.WorkspaceDiffResponse{}, err
	}
	r, err := contract.ParseWorkspaceDiff(b, after, p.Limit)
	if err != nil {
		return r, err
	}
	switch {
	case r.Name != name, p.Instance != "" && (r.Instance != p.Instance || r.Generation != p.Generation):
		return r, invalidResponse("it names another workspace, instance or generation")
	case base.Kind == contract.SelectorKindEmpty && r.BaseCommit != nil, base.Kind != contract.SelectorKindEmpty && r.BaseCommit == nil:
		return r, invalidResponse("its base does not match the request")
	case base.Kind == contract.SelectorKindHash && *r.BaseCommit != base.Value, target.Kind == contract.SelectorKindHash && r.TargetCommit != target.Value:
		return r, invalidResponse("its commits do not match the requested hashes")
	}
	return r, nil
}

// DecodePath decodes a change's path_base64.
func DecodePath(p string) ([]byte, error) { return base64.StdEncoding.DecodeString(p) }
