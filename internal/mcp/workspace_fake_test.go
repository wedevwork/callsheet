package mcp

import (
	"context"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The fake client's workspace operations (iteration 09a): each records its
// name and defers to the ws script.

func fakeWs[T any](f *fakeClient, ctx context.Context, method string, args ...any) (T, error) {
	f.record(method)
	var zero T
	if f.ws == nil {
		return zero, errUnscripted
	}
	v, err := f.ws(ctx, method, args...)
	if err != nil {
		return zero, err
	}
	return v.(T), nil
}

func (f *fakeClient) CreateWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error) {
	return fakeWs[contract.WorkspaceView](f, ctx, "CreateWorkspace", name)
}

func (f *fakeClient) ListWorkspaces(ctx context.Context, after string, limit int) (contract.WorkspaceListResponse, error) {
	return fakeWs[contract.WorkspaceListResponse](f, ctx, "ListWorkspaces", after, limit)
}

func (f *fakeClient) ShowWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error) {
	return fakeWs[contract.WorkspaceView](f, ctx, "ShowWorkspace", name)
}

func (f *fakeClient) RemoveWorkspace(ctx context.Context, name, instance string) (contract.WorkspaceRemoveResponse, error) {
	return fakeWs[contract.WorkspaceRemoveResponse](f, ctx, "RemoveWorkspace", name, instance)
}

func (f *fakeClient) PruneWorkspace(ctx context.Context, name, instance, before string) (contract.WorkspacePruneResponse, error) {
	return fakeWs[contract.WorkspacePruneResponse](f, ctx, "PruneWorkspace", name, instance, before)
}

func (f *fakeClient) SetWorkspaceRef(ctx context.Context, name string, req contract.WorkspaceRefSetRequest) (contract.WorkspaceRefSetResponse, error) {
	return fakeWs[contract.WorkspaceRefSetResponse](f, ctx, "SetWorkspaceRef", name, req)
}

func (f *fakeClient) WorkspaceStatus(ctx context.Context, name string, p client.StatusPage) (contract.WorkspaceStatusResponse, error) {
	return fakeWs[contract.WorkspaceStatusResponse](f, ctx, "WorkspaceStatus", name, p)
}

func (f *fakeClient) WorkspaceDiff(ctx context.Context, name string, p client.DiffPage) (contract.WorkspaceDiffResponse, error) {
	return fakeWs[contract.WorkspaceDiffResponse](f, ctx, "WorkspaceDiff", name, p)
}

// Iteration 10c: the task-ID forms, scripted through ws as well.

func (f *fakeClient) TaskWorkspaceStatus(ctx context.Context, id string) (contract.TaskWorkspaceStatusResponse, error) {
	return fakeWs[contract.TaskWorkspaceStatusResponse](f, ctx, "TaskWorkspaceStatus", id)
}

func (f *fakeClient) TaskWorkspaceDiff(ctx context.Context, id string, p client.TaskDiffPage) (contract.WorkspaceDiffResponse, error) {
	return fakeWs[contract.WorkspaceDiffResponse](f, ctx, "TaskWorkspaceDiff", id, p)
}

func (f *fakeClient) ResolveTaskResult(ctx context.Context, id string) (client.TaskResultSelection, error) {
	return fakeWs[client.TaskResultSelection](f, ctx, "ResolveTaskResult", id)
}
