package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Node workspace transfers (iteration 10b): a task's assignment-guarded
// node routes (/api/v1/node-workspaces/<task_id>.git/...) over the same
// verified trust, with the node's assignment headers (and on receive the
// publication ID), and the publication endpoints. The headers are
// correctness checks, never credentials.

// NodeTaskGit returns the node transfer session of task taskID with
// assignment a (pubID, when nonempty, is the publication a receive
// belongs to). Close it after use.
func (c *Client) NodeTaskGit(taskID string, a contract.NodeAssignment, pubID string) (*WorkspaceGit, error) {
	if !contract.ValidTaskID(taskID) {
		return nil, invalidTaskID()
	}
	h := a.Headers()
	if pubID != "" {
		if !contract.ValidWorkspaceToken(pubID) {
			return nil, contract.New(contract.CodeInvalidArgument, "invalid publication ID")
		}
		h[contract.PublicationIDHeader] = pubID
	}
	tr := newTransportHeader(c.tr.TLSClientConfig.Clone(), 0)
	return &WorkspaceGit{c: c, name: taskID, base: c.ep.url + contract.NodeWorkspacePath(taskID), tr: tr, hc: newHTTPClient(tr),
		clock: realClock{}, idle: GitIdle, headers: h, node: true}, nil
}

// maxReasonBody bounds a node route refusal's plain-text reason.
const maxReasonBody = 128

// nodeStatusError maps a node route refusal: 400 invalid_argument, 404
// not_found, 409 conflict with its fixed reason, 503 unavailable.
func nodeStatusError(status int, body io.Reader, what string) error {
	b, _ := readLimited(body, maxReasonBody, -1)
	reason := strings.TrimSpace(string(b))
	switch status {
	case http.StatusBadRequest:
		return contract.TaskError(contract.CodeInvalidArgument, "", "", "the plane refused the node %s request", what)
	case http.StatusNotFound:
		return contract.TaskError(contract.CodeNotFound, "", "", "the plane does not know the task or its workspace")
	case http.StatusConflict:
		switch reason {
		case contract.ReasonTaskAssignmentMismatch, contract.ReasonWorkspaceInstanceMismatch, contract.ReasonTaskPublicationClosed,
			contract.ReasonTaskRefExists, contract.ReasonWorkspaceBaseUnavailable:
		default:
			reason = contract.ReasonTaskAssignmentMismatch
		}
		return contract.TaskError(contract.CodeConflict, "", reason, "the plane refused the node %s request: %s", what, strings.ReplaceAll(reason, "_", " "))
	case http.StatusServiceUnavailable:
		return contract.TaskError(contract.CodeUnavailable, "", "", "the plane cancelled the node %s request", what)
	}
	return gitStatusError(status, what)
}

// PushTask sends exactly one create command for the session's own task
// ref with the result commit and the pack streamed from pack; it succeeds
// only on an ok unpack and command status. A non-ok report is the fixed
// reason (no ref was created); a lost answer is ambiguous (unavailable).
func (g *WorkspaceGit) PushTask(ctx context.Context, ref string, new plumbing.Hash, pack io.Reader) error {
	if !g.node || ref != contract.TaskRefPrefix+g.name || new.IsZero() {
		return contract.New(contract.CodeInvalidArgument, "a task push names its own task ref and a nonzero commit")
	}
	caps := capability.NewList()
	caps.Set(capability.ReportStatus)
	var head bytes.Buffer
	e := pktline.NewEncoder(&head)
	if err := e.Encodef("%s %s %s\x00%s", plumbing.ZeroHash, new, ref, caps.String()); err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode the push command", err)
	}
	if err := e.Flush(); err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode the push command", err)
	}
	s, err := g.do(ctx, http.MethodPost, "git-receive-pack", "application/x-git-receive-pack-request", "", io.MultiReader(&head, pack), "receive-pack")
	if err != nil {
		return err
	}
	defer s.close()
	rs := packp.NewReportStatus()
	if err := rs.Decode(s.body); err != nil {
		if s.ctx.Err() != nil || ctx.Err() != nil {
			return g.failure(ctx, s, err)
		}
		return contract.New(contract.CodeUnavailable, "the plane's push report is malformed; the publication outcome is observed")
	}
	if len(rs.CommandStatuses) != 1 || string(rs.CommandStatuses[0].ReferenceName) != ref {
		return contract.New(contract.CodeUnavailable, "the plane's push report does not name the task ref; the publication outcome is observed")
	}
	status := rs.CommandStatuses[0].Status
	switch {
	case rs.UnpackStatus == "ok" && status == "ok":
		return nil
	case status == "storage failure", status == "cancelled before publication":
		// The plane may have committed: observe.
		return contract.New(contract.CodeUnavailable, "the plane did not confirm the task ref ("+status+"); the publication outcome is observed")
	case status == "metadata overflow":
		return contract.TaskError(contract.CodeInvalidArgument, "", contract.PubErrMetadataOverflow, "the result's tree metadata exceeds the safe integer range")
	}
	return contract.TaskError(contract.CodeInvalidArgument, "", contract.PubErrPublicationTransportFail, "the plane refused the pushed result (%s)", contract.SafeText(status, 64))
}

// TaskPusher is a publication's receive session bound to the assignment's
// workspace instance: ReceiveRefs reads the advertisement under that
// instance, PushTask sends the one create command.
type TaskPusher struct {
	g    *WorkspaceGit
	inst string
}

// NodeTaskPusher returns the receive session of publication pubID of task
// taskID with assignment a. Close it after use.
func (c *Client) NodeTaskPusher(taskID string, a contract.NodeAssignment, pubID string) (*TaskPusher, error) {
	if pubID == "" {
		return nil, contract.New(contract.CodeInvalidArgument, "a task push names its publication")
	}
	g, err := c.NodeTaskGit(taskID, a, pubID)
	if err != nil {
		return nil, err
	}
	return &TaskPusher{g: g, inst: a.Instance}, nil
}

// ReceiveRefs reads the receive advertisement.
func (p *TaskPusher) ReceiveRefs(ctx context.Context) (map[string]plumbing.Hash, error) {
	return p.g.ReceiveRefs(ctx, p.inst)
}

// PushTask sends the task ref's create command and pack.
func (p *TaskPusher) PushTask(ctx context.Context, ref string, commit plumbing.Hash, pack io.Reader) error {
	return p.g.PushTask(ctx, ref, commit, pack)
}

// Close releases the session.
func (p *TaskPusher) Close() { p.g.Close() }

// TaskPublications is a task's publication endpoints with a node's
// assignment headers.
type TaskPublications struct {
	c      *Client
	taskID string
	a      contract.NodeAssignment
}

// TaskPublications returns task taskID's publication endpoints.
func (c *Client) TaskPublications(taskID string, a contract.NodeAssignment) (*TaskPublications, error) {
	if !contract.ValidTaskID(taskID) {
		return nil, invalidTaskID()
	}
	return &TaskPublications{c: c, taskID: taskID, a: a}, nil
}

func (p *TaskPublications) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	_, b, err, _ := p.c.requestWith(ctx, p.c.http, operationTimeout, method, path, body, contract.MaxPublicationReply, p.a.Headers())
	return b, err
}

// Begin asks the plane to arbitrate the sealed candidate and its tree.
func (p *TaskPublications) Begin(ctx context.Context, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error) {
	body, err := encodeBody(req)
	if err != nil {
		return contract.PublicationBeginResponse{}, err
	}
	b, err := p.do(ctx, http.MethodPost, contract.PublicationPath(p.taskID, "", false), body)
	if err != nil {
		return contract.PublicationBeginResponse{}, err
	}
	return contract.ParsePublicationBeginResponse(b)
}

// Finish reports a definitive failure of publication pubID.
func (p *TaskPublications) Finish(ctx context.Context, pubID, code string) (contract.PublicationStatus, error) {
	body, err := encodeBody(contract.PublicationFinishRequest{Error: code})
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	b, err := p.do(ctx, http.MethodPost, contract.PublicationPath(p.taskID, pubID, true), body)
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	return contract.ParsePublicationStatus(b, false)
}

// Observe reads publication pubID ("" is the lookup form).
func (p *TaskPublications) Observe(ctx context.Context, pubID string) (contract.PublicationStatus, error) {
	b, err := p.do(ctx, http.MethodGet, contract.PublicationPath(p.taskID, pubID, false), nil)
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	return contract.ParsePublicationStatus(b, pubID == "")
}
