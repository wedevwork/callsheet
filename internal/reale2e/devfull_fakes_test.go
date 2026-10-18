package reale2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Offline doubles for the development path: the fake plane's workspace
// refs (a seed push sets the main branch; every published task result has
// its task ref) and a fake workspace-transfer plane, so the production
// transfer puller (workspacetransfer.Pull) runs against the in-memory hub
// with its own destination rules.

// pushSeed records the coordinator's seed push: the seed objects in the
// hub and the workspace's main branch at commit.
func (p *fakePlane) pushSeed(commit plumbing.Hash) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wsRefs == nil {
		p.wsRefs = map[string]string{}
	}
	p.wsRefs[contract.DefaultBranchRef] = commit.String()
}

// refsLocked is every workspace ref, sorted by name.
func (p *fakePlane) refsLocked() []contract.WorkspaceRef {
	var out []contract.WorkspaceRef
	for name, c := range p.wsRefs {
		out = append(out, contract.WorkspaceRef{Name: name, Commit: c})
	}
	for id, v := range p.tasks {
		if v.Result != nil && v.Result.Workspace != nil && v.Result.Workspace.Commit != nil {
			at := contract.FormatTime(p.clock.Now())
			out = append(out, contract.WorkspaceRef{Name: contract.TaskRefPrefix + id, Commit: *v.Result.Workspace.Commit, PublishedAt: &at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (p *fakePlane) WorkspaceStatus(_ context.Context, name string, _ client.StatusPage) (contract.WorkspaceStatusResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.statusErr != nil {
		return contract.WorkspaceStatusResponse{}, p.statusErr
	}
	if p.ws == nil || p.ws.Name != name {
		return contract.WorkspaceStatusResponse{}, contract.New(contract.CodeNotFound, "no workspace")
	}
	return contract.WorkspaceStatusResponse{Name: name, Instance: p.ws.Instance, Generation: hexID("", 7), DefaultBranch: contract.DefaultBranchRef,
		Refs: p.refsLocked()}, nil
}

func (p *fakePlane) ShowWorkspace(_ context.Context, name string) (contract.WorkspaceView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ws == nil || p.ws.Name != name {
		return contract.WorkspaceView{}, contract.New(contract.CodeNotFound, "no workspace")
	}
	return *p.ws, nil
}

func (p *fakePlane) WorkspaceDiff(context.Context, string, client.DiffPage) (contract.WorkspaceDiffResponse, error) {
	return contract.WorkspaceDiffResponse{}, errors.New("the fake transfer plane serves task refs only")
}

func (p *fakePlane) WorkspaceGit(string) (workspacetransfer.Git, error) { return fakeWSGit{p: p}, nil }

// fakeWSGit serves fetches from the hub as a packfile.
type fakeWSGit struct{ p *fakePlane }

func (g fakeWSGit) ReceiveRefs(context.Context, string) (map[string]plumbing.Hash, error) {
	g.p.mu.Lock()
	defer g.p.mu.Unlock()
	out := map[string]plumbing.Hash{}
	for _, r := range g.p.refsLocked() {
		out[r.Name] = plumbing.NewHash(r.Commit)
	}
	return out, nil
}

func (g fakeWSGit) Push(context.Context, string, string, plumbing.Hash, plumbing.Hash, io.Reader) error {
	return errors.New("the fake transfer plane does not accept pushes")
}

func (g fakeWSGit) Fetch(_ context.Context, wants, _ []plumbing.Hash, sink func(io.Reader) error) error {
	hashes, err := closure(g.p.hub, wants)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, g.p.hub, false).Encode(hashes, 0); err != nil {
		return err
	}
	return sink(&buf)
}

func (g fakeWSGit) Close() {}

// productionPuller is the live transfer puller over the fake plane: the
// task's result is resolved from its authoritative view as the client's
// ResolveTaskResult does, then pulled with workspacetransfer.Pull.
func productionPuller(p *fakePlane) Puller {
	resolve := func(ctx context.Context, id string) (client.TaskResultSelection, error) {
		v, err := p.ShowTask(ctx, id, 0)
		if err != nil {
			return client.TaskResultSelection{}, err
		}
		b, w := v.WorkspaceBinding, v.Result
		if b == nil || w == nil || w.Workspace == nil || w.Workspace.Commit == nil || w.Workspace.Publication != contract.PublicationPublished {
			return client.TaskResultSelection{}, contract.TaskResultUnavailable(id, "no published result")
		}
		return client.TaskResultSelection{Name: b.Name, Instance: b.Instance, Ref: contract.TaskRefPrefix + id, Commit: *w.Workspace.Commit,
			BaseCommit: b.BaseCommit}, nil
	}
	return transferPuller{resolve: resolve, plane: p, goos: runtime.GOOS,
		env: workspacetransfer.ProcessEnv([]string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin"})}
}
