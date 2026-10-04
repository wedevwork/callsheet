package workspace

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Workspace tasks (iteration 10b): admission's base resolution, the node
// transfer routes of an assigned task (an authorized fetch of exactly its
// bound base, and the receive of exactly its create-once task ref inside
// an open publication intent), task-ref provenance and the publication
// fences that hold rm and prune off a publication between its workspace
// commit and its terminal task record. The plane's task service supplies
// every assignment and intent decision through TaskGuard; nothing here
// authenticates a node.

// TaskBaseRef is the synthetic ref a task's upload advertisement names:
// exactly its pinned base commit (absent for the empty base).
const TaskBaseRef = "refs/heads/base"

// ResolveTaskBase resolves a dispatch's workspace selection once, under
// the workspace's read lock: the live instance (a nonempty instance must
// match: a compare-only precondition), the selected commit (main by
// default, never an implicit empty base when it is unborn) with its
// complete verified closure, or the empty base (null commit). Nothing is
// retained: it is not a lease.
func (m *Manager) ResolveTaskBase(ctx context.Context, name, instance string, sel contract.Selector) (contract.WorkspaceBinding, error) {
	var b contract.WorkspaceBinding
	if !contract.ValidWorkspaceName(name) {
		return b, contract.InvalidWorkspaceName()
	}
	ctx, done := m.opCtx(ctx)
	defer done()
	h, release, err := m.read(ctx, name, instance)
	if err != nil {
		return b, opError("base resolution", err)
	}
	defer release()
	b = contract.WorkspaceBinding{Name: name, Instance: h.instance, BaseSelector: contract.SelectorString(sel)}
	if sel.Kind == contract.SelectorKindEmpty {
		return b, nil
	}
	refs, err := readRefs(h.repo())
	if err != nil {
		return b, opError("base resolution", err)
	}
	s, sfs := openStorage(h.repo(), m.d)
	defer closeStorage(s, sfs)
	c, err := resolve(ctx, s, refs, sel)
	if err != nil {
		return b, opError("base resolution", err)
	}
	if _, err := walkClosure(ctx, s, []plumbing.Hash{c}, nil); err != nil {
		return b, opError("base resolution", err)
	}
	cs := c.String()
	b.BaseCommit = &cs
	return b, nil
}

// TaskAccess is an assigned task's transfer scope: its bound workspace
// instance and base (zero for the empty base) and its own task ref.
type TaskAccess struct {
	Name     string
	Instance string
	Base     plumbing.Hash
	TaskRef  string
}

// TaskReceive is one open receive inside a durable publication intent:
// the expected deterministic commit (its hash pins the tree, parent,
// identity and message), the intent's correlation ID and deadline. The
// guard's publication operation is held until Done. Published runs after
// the workspace commit is durable, outside every workspace lock, and must
// persist the terminal task record before a success report is written.
type TaskReceive struct {
	Access    TaskAccess
	Commit    plumbing.Hash
	PubID     string
	Deadline  time.Time
	Published func(ctx context.Context, publishedAt time.Time, md TaskMetadata) error
	Done      func()
}

// TaskMetadata is a published result's tree metadata against its base,
// recomputed by the plane under the workspace transaction: exact totals
// and the first rows by raw path bytes (one more than a result DTO holds).
type TaskMetadata struct {
	Diffstat contract.TaskDiffstat
	Rows     []contract.ChangeRow
}

// taskRows bounds the rows kept: one more than a DTO can hold.
const taskRows = contract.MaxWorkspaceChanges + 1

// errMetadataOverflow is a counter beyond the safe integer range.
var errMetadataOverflow = errors.New("metadata overflow")

// taskMetadata diffs base's tree (empty for zero) against commit's tree in
// s with 09a's diff semantics, counting every row exactly.
func taskMetadata(ctx context.Context, s *storageT, base, commit plumbing.Hash) (TaskMetadata, error) {
	var md TaskMetadata
	var baseTree plumbing.Hash
	var err error
	if !base.IsZero() {
		if baseTree, err = commitTree(s, base); err != nil {
			return md, err
		}
	}
	target, err := commitTree(s, commit)
	if err != nil {
		return md, err
	}
	var st diffStats
	add := func(c *int64, n int64) error {
		if n < 0 || *c > contract.MaxSafeInteger-n {
			return errMetadataOverflow
		}
		*c += n
		return nil
	}
	var ferr error
	w := &treeDiff{ctx: ctx, s: s, max: int(^uint(0) >> 1), st: &st}
	w.emit = func(r contract.WorkspaceChange) {
		ds := &md.Diffstat
		var err error
		switch r.Kind {
		case contract.ChangeAdded:
			err = add(&ds.Added, 1)
		case contract.ChangeDeleted:
			err = add(&ds.Deleted, 1)
		default:
			err = add(&ds.Modified, 1)
		}
		if err == nil && r.OldBytes != nil {
			err = add(&ds.OldBytes, *r.OldBytes)
		}
		if err == nil && r.NewBytes != nil {
			err = add(&ds.NewBytes, *r.NewBytes)
		}
		if err != nil && ferr == nil {
			ferr = err
		}
		if len(md.Rows) < taskRows {
			p, _ := contract.ParsePathCursor(r.PathBase64)
			md.Rows = append(md.Rows, contract.ChangeRow{Path: p, Kind: r.Kind, OldMode: r.OldMode, NewMode: r.NewMode, OldBytes: r.OldBytes, NewBytes: r.NewBytes})
		}
	}
	if err := w.walk(nil, baseTree, target); err != nil {
		return md, err
	}
	return md, ferr
}

// TaskDiff recomputes a published task ref's tree metadata from the hub
// (recovery and late settlement) under the instance's read lock.
func (m *Manager) TaskDiff(ctx context.Context, name, instance string, base, commit plumbing.Hash) (TaskMetadata, error) {
	ctx, done := m.opCtx(ctx)
	defer done()
	h, release, err := m.read(ctx, name, instance)
	if err != nil {
		return TaskMetadata{}, opError("task diff", err)
	}
	defer release()
	s, sfs := openStorage(h.repo(), m.d)
	defer closeStorage(s, sfs)
	md, err := taskMetadata(ctx, s, base, commit)
	if errors.Is(err, errMetadataOverflow) {
		return md, contract.TaskError(contract.CodeInternal, "", contract.PubErrMetadataOverflow, "the task result's tree metadata exceeds the safe integer range")
	}
	if err != nil {
		return md, opError("task diff", err)
	}
	return md, nil
}

// TaskGuard is the plane's assignment and intent authority for the node
// routes.
type TaskGuard interface {
	// Upload checks a transfer's assignment: the task is live in
	// preparation, execution or finalization on this node's current
	// attachment with its start digest and bound instance.
	Upload(ctx context.Context, taskID string, a contract.NodeAssignment) (TaskAccess, error)
	// Receive opens a receive of publication pubID (its operation mutex is
	// held until Done).
	Receive(ctx context.Context, taskID string, a contract.NodeAssignment, pubID string) (*TaskReceive, error)
}

// taskConflict is a 409 refusal with a fixed reason.
func taskConflict(reason, msg string) error {
	return contract.TaskError(contract.CodeConflict, "", reason, "%s", msg)
}

// TaskConflict returns the fixed 409 refusal of reason (task_assignment_mismatch,
// workspace_instance_mismatch, task_publication_closed, task_ref_exists).
func TaskConflict(reason string) error {
	return taskConflict(reason, "the node transfer was refused: "+strings.ReplaceAll(reason, "_", " "))
}

// taskStatus writes a guard or lock failure as a fixed plain-text answer:
// malformed requests 400, unknown tasks or workspaces 404, stale,
// mismatched, closed or existing 409 (the body is the fixed reason).
func taskStatus(w http.ResponseWriter, err error) {
	reason := ""
	var ce *contract.Error
	if errors.As(err, &ce) {
		reason, _ = ce.Details["reason"].(string)
	}
	switch contract.CodeOf(err) {
	case contract.CodeInvalidArgument:
		plainError(w, http.StatusBadRequest, "invalid_argument")
	case contract.CodeNotFound:
		plainError(w, http.StatusNotFound, "not_found")
	case contract.CodeConflict:
		switch reason {
		case contract.ReasonTaskAssignmentMismatch, contract.ReasonWorkspaceInstanceMismatch, contract.ReasonTaskPublicationClosed,
			contract.ReasonTaskRefExists, contract.ReasonWorkspaceBaseUnavailable:
		default:
			reason = contract.ReasonWorkspaceInstanceMismatch
		}
		plainError(w, http.StatusConflict, reason)
	case contract.CodeUnavailable:
		plainError(w, http.StatusServiceUnavailable, "unavailable")
	default:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			plainError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		plainError(w, http.StatusInternalServerError, "internal")
	}
}

// ServeTaskGit serves the node transfer routes
// /api/v1/node-workspaces/<task_id>.git/{info/refs,git-upload-pack,git-receive-pack}:
// the route, method and query, the assignment headers (and on receive the
// publication ID) are checked before any lookup or pack processing; the
// guard checks the assignment before any disk staging.
func (m *Manager) ServeTaskGit(w http.ResponseWriter, r *http.Request, g TaskGuard) {
	if r.ProtoMajor != 1 {
		plainError(w, http.StatusHTTPVersionNotSupported, "the git transport is served over HTTP/1.1 only")
		return
	}
	if r.URL.RawPath != "" {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	rest, _ := strings.CutPrefix(r.URL.Path, contract.PathNodeWorkspaces)
	id, route, ok := strings.Cut(rest, gitSuffix)
	if !ok || !contract.ValidTaskID(id) {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	a, err := contract.ParseNodeAssignment(r.Header.Values)
	if err != nil {
		taskStatus(w, err)
		return
	}
	switch route {
	case routeInfoRefs:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			plainError(w, http.StatusMethodNotAllowed, "invalid_argument")
			return
		}
		svc, ok := strings.CutPrefix(r.URL.RawQuery, "service=")
		if !ok || (svc != serviceUpload && svc != serviceReceive) {
			plainError(w, http.StatusBadRequest, "invalid_argument")
			return
		}
		if svc == serviceReceive {
			m.taskReceiveAdvertise(w, r, id, a, g)
			return
		}
		m.taskUploadAdvertise(w, r, id, a, g)
	case routeUpload, routeReceive:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			plainError(w, http.StatusMethodNotAllowed, "invalid_argument")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			plainError(w, http.StatusBadRequest, "invalid_argument")
			return
		}
		if route == routeUpload {
			m.taskUpload(w, r, id, a, g)
			return
		}
		m.taskReceive(w, r, id, a, g)
	default:
		plainError(w, http.StatusNotFound, "not_found")
	}
}

// publicationHeader returns the single valid publication ID header.
func publicationHeader(r *http.Request) (string, bool) {
	v := r.Header.Values(contract.PublicationIDHeader)
	if len(v) != 1 || !contract.ValidWorkspaceToken(v[0]) {
		return "", false
	}
	return v[0], true
}

// taskBase checks, under the caller's read lock, that the access's
// instance is the live one and its base is still reachable from a current
// ref (the empty base needs only the live instance).
func (m *Manager) taskBase(ctx context.Context, h *handle, acc TaskAccess) error {
	if h.instance != acc.Instance {
		return taskConflict(contract.ReasonWorkspaceInstanceMismatch, "the workspace instance changed")
	}
	if acc.Base.IsZero() {
		return nil
	}
	refs, err := readRefs(h.repo())
	if err != nil {
		return err
	}
	s, sfs := openStorage(h.repo(), m.d)
	defer closeStorage(s, sfs)
	reach, err := reachableCommits(ctx, s, tips(refs), func(x plumbing.Hash) bool { return x == acc.Base })
	if err != nil {
		return err
	}
	if !reach[acc.Base] {
		return taskConflict(contract.ReasonWorkspaceBaseUnavailable, "the task's base is no longer reachable from a ref of its workspace")
	}
	return nil
}

// readTask takes the access's workspace read lock (the instance must
// match) and checks its base.
func (m *Manager) readTask(ctx context.Context, acc TaskAccess) (*handle, func(), error) {
	h, release, err := m.read(ctx, acc.Name, acc.Instance)
	if err != nil {
		if contract.CodeOf(err) == contract.CodeConflict {
			return nil, nil, taskConflict(contract.ReasonWorkspaceInstanceMismatch, "the workspace instance changed")
		}
		return nil, nil, err
	}
	if err := m.taskBase(ctx, h, acc); err != nil {
		release()
		return nil, nil, err
	}
	return h, release, nil
}

// encodeAdv writes a synthetic advertisement of refs with caps.
func encodeAdv(w http.ResponseWriter, g *gitIO, svc string, refs map[string]plumbing.Hash, caps map[capability.Capability]bool) {
	ar := packp.NewAdvRefs()
	for c := range caps {
		if c != capability.Agent {
			ar.Capabilities.Set(c)
		}
	}
	for n, h := range refs {
		ar.References[n] = h
	}
	ar.Prefix = [][]byte{[]byte("# service=" + svc), pktline.Flush}
	var buf bytes.Buffer
	if err := ar.Encode(&buf); err != nil {
		plainError(w, http.StatusInternalServerError, "internal")
		return
	}
	g.header(svc, true)
	g.Write(buf.Bytes())
	g.finish()
}

func (m *Manager) taskUploadAdvertise(w http.ResponseWriter, r *http.Request, id string, a contract.NodeAssignment, g TaskGuard) {
	ctx, done := m.opCtx(r.Context())
	defer done()
	io := newGitIO(w, r, m.d.idle)
	acc, err := g.Upload(ctx, id, a)
	if err != nil {
		taskStatus(w, err)
		return
	}
	_, release, err := m.readTask(ctx, acc)
	if err != nil {
		taskStatus(w, err)
		return
	}
	defer release()
	refs := map[string]plumbing.Hash{}
	if !acc.Base.IsZero() {
		refs[TaskBaseRef] = acc.Base
	}
	encodeAdv(w, io, serviceUpload, refs, uploadAllow)
}

// taskUpload serves exactly the bound base's full closure (no other want,
// no shallow, deepen or filter) from the locked current generation; haves
// are hints the store may lack.
func (m *Manager) taskUpload(w http.ResponseWriter, r *http.Request, id string, a contract.NodeAssignment, g TaskGuard) {
	ctx, done := m.opCtx(r.Context())
	defer done()
	io := newGitIO(w, r, m.d.idle)
	defer io.halt()
	acc, err := g.Upload(ctx, id, a)
	if err != nil {
		taskStatus(w, err)
		return
	}
	if acc.Base.IsZero() {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	h, release, err := m.readTask(ctx, acc)
	if err != nil {
		taskStatus(w, err)
		return
	}
	defer release()
	s, sfs := openStorage(h.repo(), m.d)
	defer closeStorage(s, sfs)
	req := packp.NewUploadPackRequest()
	if err := req.Decode(io); err != nil {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	if err := decodeHaves(io, req, s); err != nil {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	io.halt()
	if len(req.Shallows) > 0 || req.Depth != packp.DepthCommits(0) || req.Filter != "" || len(req.Wants) != 1 || req.Wants[0] != acc.Base {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	for _, c := range req.Capabilities.All() {
		if !uploadAllow[c] {
			plainError(w, http.StatusBadRequest, "invalid_argument")
			return
		}
	}
	m.d.stage("task-upload-locked", ctx)
	if slices.Contains(req.Haves, acc.Base) {
		// The node already holds the base (a verified cache hit; the
		// library refuses such a request as empty): acknowledge it and send
		// an empty pack, so the authorized round trip still happens.
		io.header(serviceUpload, false)
		sr := packp.ServerResponse{ACKs: []plumbing.Hash{acc.Base}}
		if err := sr.Encode(io, false); err == nil {
			packfile.NewEncoder(io, s, false).Encode(nil, 0)
		}
		io.finish()
		return
	}
	sess, err := libraryServer(s).NewUploadPackSession(endpoint, nil)
	if err != nil {
		plainError(w, http.StatusInternalServerError, "internal")
		return
	}
	defer sess.Close()
	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	io.header(serviceUpload, false)
	resp.Encode(io)
	resp.Close()
	io.finish()
}

// taskReceiveAdvertise names only the bound base (when still reachable)
// and the task's own ref (when it exists).
func (m *Manager) taskReceiveAdvertise(w http.ResponseWriter, r *http.Request, id string, a contract.NodeAssignment, g TaskGuard) {
	ctx, done := m.opCtx(r.Context())
	defer done()
	io := newGitIO(w, r, m.d.idle)
	acc, err := g.Upload(ctx, id, a)
	if err != nil {
		taskStatus(w, err)
		return
	}
	h, release, err := m.read(ctx, acc.Name, acc.Instance)
	if err != nil {
		if contract.CodeOf(err) == contract.CodeConflict {
			err = taskConflict(contract.ReasonWorkspaceInstanceMismatch, "the workspace instance changed")
		}
		taskStatus(w, err)
		return
	}
	defer release()
	refs := map[string]plumbing.Hash{}
	stored, err := readRefs(h.repo())
	if err != nil {
		plainError(w, http.StatusInternalServerError, "internal")
		return
	}
	if c, ok := stored[acc.TaskRef]; ok {
		refs[acc.TaskRef] = c
	}
	if !acc.Base.IsZero() && m.taskBase(ctx, h, acc) == nil {
		refs[TaskBaseRef] = acc.Base
	}
	encodeAdv(w, io, serviceReceive, refs, receiveAllow)
}

// taskReceive is the guarded create-once receive of the task ref: the
// publication header, then the guard's open intent (its operation mutex
// held to the end), the publication fence, the framing (exactly one
// create command for the own task ref with the intent's commit, the
// report-status capability, no thin pack), the workspace write lock, a
// conflict for an existing ref, the library receive into a private copied
// generation, the staged ref and complete closure, the task metadata with
// plane publication time and provenance, and the generation commit. The
// terminal task record is persisted (Published) outside every workspace
// lock before the success report.
func (m *Manager) taskReceive(w http.ResponseWriter, r *http.Request, id string, a contract.NodeAssignment, g TaskGuard) {
	ctx, done := m.opCtx(r.Context())
	defer done()
	io := newGitIO(w, r, m.d.idle)
	defer io.halt()
	pub, ok := publicationHeader(r)
	if !ok {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	rv, err := g.Receive(ctx, id, a, pub)
	if err != nil {
		taskStatus(w, err)
		return
	}
	defer rv.Done()
	if !rv.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, rv.Deadline)
		defer cancel()
	}
	// The fence spans the workspace commit through terminal task
	// durability: released on every path except a committed ref whose
	// terminal record is unconfirmed (recovery or settlement releases it).
	release := m.fencePublication(rv.Access.Name, id+"/"+pub)
	keep := false
	defer func() {
		if !keep {
			release()
		}
	}()
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(io); err != nil {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	if len(req.Commands) != 1 || !req.Capabilities.Supports(capability.ReportStatus) {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	for _, c := range req.Capabilities.All() {
		if !receiveAllow[c] {
			plainError(w, http.StatusBadRequest, "invalid_argument")
			return
		}
	}
	cmd := req.Commands[0]
	if !cmd.Old.IsZero() || cmd.New.IsZero() || string(cmd.Name) != rv.Access.TaskRef {
		plainError(w, http.StatusBadRequest, "invalid_argument")
		return
	}
	if cmd.New != rv.Commit {
		report(io, unpackNotAttempted, cmd.Name, "commit does not match the publication intent")
		return
	}
	at, md, rerr := m.receiveTaskRef(ctx, io, req, rv, pub)
	if rerr != nil {
		var re receiveError
		if errors.As(rerr, &re) && re.committed {
			// CURRENT was renamed but its durability is unconfirmed: the
			// workspace is fenced and the publication fence stays until the
			// plane's restart recovers CURRENT and replays the intent.
			keep = true
		}
		var ce *contract.Error
		if errors.As(rerr, &ce) && ce.Code == contract.CodeConflict {
			taskStatus(w, rerr)
			return
		}
		report(io, rerrUnpack(rerr), cmd.Name, rerr.Error())
		return
	}
	if err := rv.Published(ctx, at, md); err != nil {
		// The ref is durable but the terminal record is not: no success
		// report, the fence stays until recovery or settlement.
		keep = true
		report(io, unpackOK, cmd.Name, reasonStorage)
		return
	}
	report(io, unpackOK, cmd.Name, "ok")
}

// receiveError is a failed task receive's report reason with its unpack
// status; committed marks a failure after the CURRENT rename (an ambiguous,
// fenced outcome).
type receiveError struct {
	unpack, reason string
	committed      bool
}

func (e receiveError) Error() string { return e.reason }

func rerrUnpack(err error) string {
	var re receiveError
	if errors.As(err, &re) {
		return re.unpack
	}
	return unpackNotAttempted
}

// receiveTaskRef runs the receive transaction under the workspace write
// lock and returns the plane publication time of the committed ref and the
// tree metadata recomputed inside the transaction (before the commit
// point: an overflow publishes nothing).
func (m *Manager) receiveTaskRef(ctx context.Context, io *gitIO, req *packp.ReferenceUpdateRequest, rv *TaskReceive, pub string) (time.Time, TaskMetadata, error) {
	fail := func(err error) (time.Time, TaskMetadata, error) { return time.Time{}, TaskMetadata{}, err }
	cmd := req.Commands[0]
	h, release, err := m.write(ctx, rv.Access.Name, rv.Access.Instance)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return fail(receiveError{unpack: unpackNotAttempted, reason: reasonCancelled})
		case contract.CodeOf(err) == contract.CodeConflict, contract.CodeOf(err) == contract.CodeNotFound:
			return fail(taskConflict(contract.ReasonWorkspaceInstanceMismatch, "the workspace instance changed or was removed"))
		}
		return fail(receiveError{unpack: unpackNotAttempted, reason: reasonStorage})
	}
	defer release()
	m.d.stage("task-receive-locked", ctx)
	refs, err := readRefs(h.repo())
	if err != nil {
		return fail(receiveError{unpack: unpackNotAttempted, reason: reasonStorage})
	}
	if _, exists := refs[string(cmd.Name)]; exists {
		return fail(taskConflict(contract.ReasonTaskRefExists, "the task ref exists"))
	}
	tasks, err := readRefsDoc(filepath.Join(h.genDir(h.gen), refsName))
	if err != nil {
		return fail(receiveError{unpack: unpackNotAttempted, reason: reasonStorage})
	}
	t, err := m.begin(ctx, h, true)
	if err != nil {
		return fail(receiveError{unpack: unpackNotAttempted, reason: cancelOr(ctx, reasonStorage)})
	}
	s, sfs := openStorage(t.repo(), m.d)
	var check *packCheck
	if req.Packfile != nil {
		check = newPackCheck(req.Packfile)
		req.Packfile = readCloser{Reader: check, Closer: req.Packfile}
	}
	var rs *packp.ReportStatus
	sess, rerr := libraryServer(s).NewReceivePackSession(endpoint, nil)
	if rerr == nil {
		rs, rerr = sess.ReceivePack(ctx, req)
		sess.Close()
	}
	cerr := closeStorage(s, sfs)
	if rerr == nil && rs != nil && rs.Error() == nil && check != nil {
		if err := check.verify(); err != nil {
			rs.UnpackStatus = unpackFailed
		}
	}
	io.halt()
	if req.Packfile != nil {
		req.Packfile.Close()
	}
	if rerr != nil || rs == nil || rs.Error() != nil || cerr != nil {
		t.discard()
		switch {
		case ctx.Err() != nil:
			return fail(receiveError{unpack: unpackFailed, reason: reasonCancelled})
		case rs != nil && rs.UnpackStatus == unpackOK:
			return fail(receiveError{unpack: unpackOK, reason: reasonStorage})
		}
		return fail(receiveError{unpack: unpackFailed, reason: reasonClosure})
	}
	m.d.stage("task-receive-unpacked", ctx)
	staged, err := readRefs(t.repo())
	if err != nil || staged[string(cmd.Name)] != cmd.New {
		t.discard()
		return fail(receiveError{unpack: unpackOK, reason: reasonStorage})
	}
	at := m.d.now().UTC()
	tasks[string(cmd.Name)] = taskMeta{Commit: cmd.New.String(), PublishedAt: contract.FormatTime(at), PublicationID: pub}
	err = m.d.writeFile(filepath.Join(t.dir, refsName+".tmp"), encodeRefsDoc(tasks))
	if err == nil {
		err = m.d.rename(filepath.Join(t.dir, refsName+".tmp"), filepath.Join(t.dir, refsName))
	}
	if err != nil {
		t.discard()
		return fail(receiveError{unpack: unpackOK, reason: reasonStorage})
	}
	if _, err := m.d.validateGeneration(ctx, t.dir, nil); err != nil {
		t.discard()
		var cl errClosure
		switch {
		case ctx.Err() != nil:
			return fail(receiveError{unpack: unpackOK, reason: reasonCancelled})
		case errors.As(err, &cl):
			return fail(receiveError{unpack: unpackOK, reason: reasonClosure})
		}
		return fail(receiveError{unpack: unpackOK, reason: reasonStorage})
	}
	// The plane recomputes the tree metadata in the staged generation.
	ms, mfs := openStorage(t.repo(), m.d)
	md, err := taskMetadata(ctx, ms, rv.Access.Base, cmd.New)
	closeStorage(ms, mfs)
	if err != nil {
		t.discard()
		if errors.Is(err, errMetadataOverflow) {
			return fail(receiveError{unpack: unpackOK, reason: reasonMetadataOverflow})
		}
		return fail(receiveError{unpack: unpackOK, reason: cancelOr(ctx, reasonStorage)})
	}
	if err := t.commit(ctx); err != nil {
		var te txnError
		if errors.As(err, &te) && !te.committed {
			return fail(receiveError{unpack: unpackOK, reason: cancelOr(ctx, reasonStorage)})
		}
		// Published but ambiguous: the workspace is fenced; recovery
		// decides.
		return fail(receiveError{unpack: unpackOK, reason: reasonStorage, committed: true})
	}
	return at, md, nil
}

// reasonMetadataOverflow is the report reason of a result whose tree
// metadata exceeds the safe integer range (nothing was published).
const reasonMetadataOverflow = "metadata overflow"

// TaskRefState is a task ref's provenance: its commit, publication ID
// and plane publication time (exists=false: absent, or the workspace
// instance is gone).
type TaskRefState struct {
	Exists        bool
	Commit        string
	PublicationID string
	PublishedAt   time.Time
}

// errUnconfirmed is a provenance read refused because the workspace's (or
// the manager's) last commit has an unconfirmed storage outcome: its
// in-memory CURRENT may not be the one a restart recovers, so neither
// presence nor absence of a task ref can settle a publication.
func errUnconfirmed() error {
	return contract.New(contract.CodeInternal, "workspace durability is unconfirmed after an ambiguous storage outcome; the task publication stays completion-pending until the plane restarts and recovers CURRENT")
}

// TaskRef reads a task ref's provenance under the read lock of the
// instance (a removed or recreated workspace reports absence). A fenced
// workspace or manager (an ambiguous commit whose CURRENT durability is
// unconfirmed) is refused: only a restart's CURRENT validation confirms it.
func (m *Manager) TaskRef(ctx context.Context, name, instance, taskID string) (TaskRefState, error) {
	var st TaskRefState
	ctx, done := m.opCtx(ctx)
	defer done()
	if m.managerFenced() {
		return st, errUnconfirmed()
	}
	h, release, err := m.read(ctx, name, instance)
	if err != nil {
		switch contract.CodeOf(err) {
		case contract.CodeNotFound, contract.CodeConflict:
			if m.managerFenced() {
				return st, errUnconfirmed()
			}
			return st, nil
		}
		return st, opError("task ref", err)
	}
	defer release()
	if h.fenced.Load() || m.managerFenced() {
		return st, errUnconfirmed()
	}
	tasks, err := readRefsDoc(filepath.Join(h.genDir(h.gen), refsName))
	if err != nil {
		return st, opError("task ref", err)
	}
	meta, ok := tasks[contract.TaskRefPrefix+taskID]
	if !ok {
		return st, nil
	}
	at, _ := contract.ParseTime(meta.PublishedAt)
	return TaskRefState{Exists: true, Commit: meta.Commit, PublicationID: meta.PublicationID, PublishedAt: at}, nil
}

// ---- Publication fences ----

// fences tracks active publications per workspace name (key
// task_id/publication_id): rm and prune wait (cancellably) until none
// remains.
type fences struct {
	mu     sync.Mutex
	active map[string]map[string]bool
	wake   chan struct{}
}

func (m *Manager) fenceState() *fences {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pubs == nil {
		m.pubs = &fences{active: map[string]map[string]bool{}, wake: make(chan struct{})}
	}
	return m.pubs
}

// fencePublication registers key's fence on workspace name and returns
// its release (idempotent).
func (m *Manager) fencePublication(name, key string) func() {
	f := m.fenceState()
	f.mu.Lock()
	if f.active[name] == nil {
		f.active[name] = map[string]bool{}
	}
	f.active[name][key] = true
	f.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { m.unfence(name, key) }) }
}

func (m *Manager) unfence(name, key string) {
	f := m.fenceState()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active[name][key] {
		return
	}
	delete(f.active[name], key)
	if len(f.active[name]) == 0 {
		delete(f.active, name)
	}
	close(f.wake)
	f.wake = make(chan struct{})
}

// FencePublication registers a publication's fence (plane startup, before
// workspace mutation is permitted, for every authorized intent) and
// returns its release.
func (m *Manager) FencePublication(name, taskID, pubID string) func() {
	return m.fencePublication(name, taskID+"/"+pubID)
}

// ReleasePublication releases a publication fence kept after its terminal
// record failed (recovery or a later settlement made it durable).
func (m *Manager) ReleasePublication(name, taskID, pubID string) { m.unfence(name, taskID+"/"+pubID) }

// awaitFences waits, cancellably, until workspace name has no active
// publication. The caller holds no workspace lock.
func (m *Manager) awaitFences(ctx context.Context, name string) error {
	f := m.fenceState()
	for {
		f.mu.Lock()
		busy := len(f.active[name]) > 0
		wake := f.wake
		f.mu.Unlock()
		if !busy {
			return nil
		}
		m.d.stage("fence-wait", ctx)
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Fenced reports whether workspace name has an active publication.
func (m *Manager) Fenced(name string) bool {
	f := m.fenceState()
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.active[name]) > 0
}
