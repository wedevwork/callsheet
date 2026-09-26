package gittransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Safe report messages.
const (
	msgNotAttempted   = "not attempted"
	msgStaleOld       = "stale old ref"
	msgClosure        = "incomplete object closure"
	msgNotCommit      = "new object is not a commit"
	msgInvalidRef     = "invalid ref name"
	msgDelete         = "deletes are not supported"
	msgZeroNew        = "new object id must be nonzero"
	msgCapability     = "unsupported capability"
	msgRefConflict    = "ref namespace conflict"
	msgReceiveFailed  = "receive failed"
	msgCancelled      = "cancelled before publication"
	msgPromoteFailed  = "object promotion failed"
	msgPublishFailed  = "ref publication failed"
	msgStorageFailure = "storage failure"
)

// allowedNamespaces are the only ref namespaces a push may update.
var allowedNamespaces = []string{"refs/heads/", "refs/callsheet/tasks/"}

// guardError carries a safe report message for a single-command failure.
type guardError struct {
	msg   string
	cause error
}

func (e *guardError) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *guardError) Unwrap() error { return e.cause }

func guard(msg string, cause error) error { return &guardError{msg: msg, cause: cause} }

func safeMessage(err error) string {
	var g *guardError
	if errors.As(err, &g) {
		return g.msg
	}
	return msgStorageFailure
}

// validateCommand is step 1's pure request policy: capabilities, a single
// non-delete command with a nonzero new hash, and an allowed, valid,
// non-symbolic ref name. It runs before ReceivePack for every request.
func validateCommand(req *packp.ReferenceUpdateRequest) error {
	if len(req.Commands) != 1 {
		return guard("exactly one command is supported", nil)
	}
	for _, c := range req.Capabilities.All() {
		if !receiveAllow[c] {
			return guard(msgCapability, fmt.Errorf("%s", c))
		}
	}
	if !req.Capabilities.Supports(capability.ReportStatus) {
		return guard("report-status is required", nil)
	}
	cmd := req.Commands[0]
	if cmd.New == plumbing.ZeroHash {
		if cmd.Old != plumbing.ZeroHash {
			return guard(msgDelete, nil)
		}
		return guard(msgZeroNew, nil)
	}
	return validateRefName(cmd.Name)
}

func validateRefName(name plumbing.ReferenceName) error {
	s := string(name)
	if name == plumbing.HEAD || s == "" {
		return guard(msgInvalidRef, nil)
	}
	if err := name.Validate(); err != nil {
		return guard(msgInvalidRef, err)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return guard(msgInvalidRef, nil)
		}
	}
	for _, ns := range allowedNamespaces {
		if strings.HasPrefix(s, ns) && len(s) > len(ns) {
			return nil
		}
	}
	return guard(msgInvalidRef, errors.New("namespace not allowed"))
}

// checkOld is step 2's compare-and-swap: zero old means create-only, a
// nonzero old must equal the current non-symbolic ref exactly.
func checkOld(current *plumbing.Reference, cmd *packp.Command) error {
	if current != nil && current.Type() != plumbing.HashReference {
		return guard(msgInvalidRef, errors.New("symbolic ref"))
	}
	if cmd.Old == plumbing.ZeroHash {
		if current != nil {
			return guard(msgStaleOld, nil)
		}
		return nil
	}
	if current == nil || current.Hash() != cmd.Old {
		return guard(msgStaleOld, nil)
	}
	return nil
}

// readRef reads name without symbolic resolution; absent returns nil.
func readRef(s storer.ReferenceStorer, name plumbing.ReferenceName) (*plumbing.Reference, error) {
	ref, err := s.Reference(name)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	return ref, err
}

// checkRefConflict rejects a name that is a path prefix of an existing ref
// or has an existing ref as a path prefix.
func checkRefConflict(s storer.ReferenceStorer, name plumbing.ReferenceName) error {
	iter, err := s.IterReferences()
	if err != nil {
		return err
	}
	defer iter.Close()
	n := string(name)
	return iter.ForEach(func(r *plumbing.Reference) error {
		o := string(r.Name())
		if o != n && (strings.HasPrefix(o, n+"/") || strings.HasPrefix(n, o+"/")) {
			return guard(msgRefConflict, nil)
		}
		return nil
	})
}

// newQuarantine copies every encoded object (as independent byte copies) and
// every ref, including symbolic HEAD, from durable into a fresh memory store.
func newQuarantine(durable storer.Storer) (*memory.Storage, error) {
	q := memory.NewStorage()
	iter, err := durable.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return nil, err
	}
	err = iter.ForEach(func(o plumbing.EncodedObject) error {
		_, err := copyObject(q, o)
		return err
	})
	iter.Close()
	if err != nil {
		return nil, err
	}
	refs, err := durable.IterReferences()
	if err != nil {
		return nil, err
	}
	err = refs.ForEach(func(r *plumbing.Reference) error { return q.SetReference(r) })
	refs.Close()
	if err != nil {
		return nil, err
	}
	head, err := readRef(durable, plumbing.HEAD)
	if err != nil {
		return nil, err
	}
	if head != nil {
		if err := q.SetReference(head); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// copyObject writes an independent copy of o into dst and checks the hash.
func copyObject(dst storer.EncodedObjectStorer, o plumbing.EncodedObject) (plumbing.Hash, error) {
	content, err := readContent(o)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	n := dst.NewEncodedObject()
	n.SetType(o.Type())
	n.SetSize(int64(len(content)))
	w, err := n.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	h, err := dst.SetEncodedObject(n)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if h != o.Hash() {
		return plumbing.ZeroHash, fmt.Errorf("object %s copied as %s", o.Hash(), h)
	}
	return h, nil
}

func readContent(o plumbing.EncodedObject) ([]byte, error) {
	r, err := o.Reader()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	_, rerr := io.Copy(&buf, r)
	cerr := r.Close()
	if err := errors.Join(rerr, cerr); err != nil {
		return nil, err
	}
	if int64(buf.Len()) != o.Size() {
		return nil, fmt.Errorf("object %s size mismatch", o.Hash())
	}
	return buf.Bytes(), nil
}

type pending struct {
	h plumbing.Hash
	t plumbing.ObjectType
}

// validateClosure walks the complete reachable closure of the commit tip in
// s: parents must be commits, trees must be trees, file entries must be
// blobs; every object must exist, decode, and hash to its id. Gitlinks are
// rejected. It returns the closure's hashes in traversal order.
func validateClosure(s storer.EncodedObjectStorer, tip plumbing.Hash) ([]plumbing.Hash, error) {
	first, err := s.EncodedObject(plumbing.AnyObject, tip)
	if err != nil {
		return nil, guard(msgClosure, err)
	}
	if first.Type() != plumbing.CommitObject {
		return nil, guard(msgNotCommit, nil)
	}
	visited := map[plumbing.Hash]bool{}
	var order []plumbing.Hash
	stack := []pending{{tip, plumbing.CommitObject}}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[p.h] {
			continue
		}
		visited[p.h] = true
		o, err := s.EncodedObject(plumbing.AnyObject, p.h)
		if err != nil {
			return nil, guard(msgClosure, fmt.Errorf("missing %s %s", p.t, p.h))
		}
		if o.Type() != p.t {
			return nil, guard(msgClosure, fmt.Errorf("%s is %s, want %s", p.h, o.Type(), p.t))
		}
		content, err := readContent(o)
		if err != nil {
			return nil, guard(msgClosure, err)
		}
		if plumbing.ComputeHash(o.Type(), content) != p.h {
			return nil, guard(msgClosure, fmt.Errorf("hash mismatch for %s", p.h))
		}
		order = append(order, p.h)
		switch p.t {
		case plumbing.CommitObject:
			c := &object.Commit{}
			if err := c.Decode(o); err != nil {
				return nil, guard(msgClosure, err)
			}
			stack = append(stack, pending{c.TreeHash, plumbing.TreeObject})
			for _, ph := range c.ParentHashes {
				stack = append(stack, pending{ph, plumbing.CommitObject})
			}
		case plumbing.TreeObject:
			t := &object.Tree{}
			if err := t.Decode(o); err != nil {
				return nil, guard(msgClosure, err)
			}
			for _, e := range t.Entries {
				switch e.Mode {
				case filemode.Dir:
					stack = append(stack, pending{e.Hash, plumbing.TreeObject})
				case filemode.Regular, filemode.Deprecated, filemode.Executable, filemode.Symlink:
					stack = append(stack, pending{e.Hash, plumbing.BlobObject})
				default:
					return nil, guard(msgClosure, fmt.Errorf("unsupported entry mode %s", e.Mode))
				}
			}
		}
	}
	return order, nil
}

// promoteObjects copies closure objects missing from durable, checking every
// write and the request context. Already-copied objects may remain on
// failure; they are unreachable from any published ref.
func (h *smartHandler) promoteObjects(ctx context.Context, q storer.EncodedObjectStorer, closure []plumbing.Hash) (int, error) {
	n := 0
	for _, id := range closure {
		if err := ctx.Err(); err != nil {
			return n, guard(msgCancelled, err)
		}
		if h.store.HasEncodedObject(id) == nil {
			continue
		}
		if h.hooks.promote != nil {
			if err := h.hooks.promote(id); err != nil {
				return n, guard(msgPromoteFailed, err)
			}
		}
		o, err := q.EncodedObject(plumbing.AnyObject, id)
		if err != nil {
			return n, guard(msgPromoteFailed, err)
		}
		if _, err := copyObject(h.store, o); err != nil {
			return n, guard(msgPromoteFailed, err)
		}
		n++
	}
	return n, nil
}

// publishRef atomically replaces the single loose ref file for name with
// "<hash>\n" via a temporary sibling and rename. name must be validated.
func publishRef(repoPath string, name plumbing.ReferenceName, h plumbing.Hash) error {
	target := filepath.Join(repoPath, filepath.FromSlash(string(name)))
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".callsheet-ref-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(h.String() + "\n")
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// lockWrite acquires the repository write lock unless ctx ends first.
func (h *smartHandler) lockWrite(ctx context.Context) error {
	acquired := make(chan struct{})
	go func() {
		h.mu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		go func() {
			<-acquired
			h.mu.Unlock()
		}()
		return ctx.Err()
	}
}

type notifyReader struct {
	io.ReadCloser
	once sync.Once
	fn   func()
}

func (n *notifyReader) Read(p []byte) (int, error) {
	n.once.Do(n.fn)
	return n.ReadCloser.Read(p)
}

func writeReport(w http.ResponseWriter, unpack string, ref plumbing.ReferenceName, status string) {
	rs := packp.NewReportStatus()
	rs.UnpackStatus = unpack
	rs.CommandStatuses = []*packp.CommandStatus{{ReferenceName: ref, Status: status}}
	w.Header().Set("Content-Type", fmt.Sprintf(contentTypeRPC, serviceReceive))
	w.Header().Set("Cache-Control", "no-cache")
	rs.Encode(w)
}

func (h *smartHandler) recordFailure(err error) {
	h.tmu.Lock()
	h.lastError = err.Error()
	h.tmu.Unlock()
}

// receivePack implements "Receive guards and publication" steps 1-6.
func (h *smartHandler) receivePack(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer body.Close()

	// Step 1: framing and command policy, before ReceivePack.
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(body); err != nil {
		httpDecodeError(w, err)
		return
	}
	if req.Packfile != nil {
		defer req.Packfile.Close()
	}
	if len(req.Commands) != 1 {
		http.Error(w, "exactly one command is supported", http.StatusBadRequest)
		return
	}
	if !req.Capabilities.Supports(capability.ReportStatus) {
		http.Error(w, "report-status capability is required", http.StatusBadRequest)
		return
	}
	cmd := req.Commands[0]
	fail := func(unpack string, err error) {
		h.recordFailure(err)
		writeReport(w, unpack, cmd.Name, safeMessage(err))
	}
	if err := validateCommand(req); err != nil {
		fail(msgNotAttempted, err)
		return
	}

	// Step 2: CAS under the repository write lock, held through publication.
	if err := h.lockWrite(ctx); err != nil {
		fail(msgNotAttempted, guard(msgCancelled, err))
		return
	}
	defer h.mu.Unlock()
	current, err := readRef(h.store, cmd.Name)
	if err != nil {
		fail(msgNotAttempted, err)
		return
	}
	if err := checkOld(current, cmd); err != nil {
		fail(msgNotAttempted, err)
		return
	}
	if err := checkRefConflict(h.store, cmd.Name); err != nil {
		fail(msgNotAttempted, err)
		return
	}

	// Step 3: a fresh library session bound only to a copied quarantine.
	q, err := newQuarantine(h.store)
	if err != nil {
		fail(msgNotAttempted, err)
		return
	}
	if req.Packfile != nil && h.hooks.packRead != nil {
		req.Packfile = &notifyReader{ReadCloser: req.Packfile, fn: h.hooks.packRead}
	}
	sess, err := h.transportFor(q).NewReceivePackSession(h.ep, nil)
	if err != nil {
		fail(msgNotAttempted, err)
		return
	}
	rs, rerr := sess.ReceivePack(ctx, req)
	sess.Close()
	if rerr != nil || rs == nil || rs.Error() != nil {
		var mb *http.MaxBytesError
		if errors.As(rerr, &mb) {
			h.recordFailure(rerr)
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		unpack := msgReceiveFailed
		if rs != nil && rs.UnpackStatus != "" {
			unpack = rs.UnpackStatus
		}
		if rerr == nil {
			rerr = errors.New("receive-pack reported failure")
		}
		fail(unpack, guard(msgReceiveFailed, rerr))
		return
	}

	// Step 4: the quarantine ref must be exactly cmd.New with full closure.
	qref, err := readRef(q, cmd.Name)
	if err != nil || qref == nil || qref.Hash() != cmd.New {
		fail("ok", guard(msgReceiveFailed, errors.New("quarantine ref mismatch")))
		return
	}
	closure, err := validateClosure(q, cmd.New)
	if err != nil {
		fail("ok", err)
		return
	}
	if h.hooks.afterValidate != nil {
		h.hooks.afterValidate(cancel)
	}

	// Step 5: promote, revalidate against durable, re-check CAS, publish.
	if err := ctx.Err(); err != nil {
		fail("ok", guard(msgCancelled, err))
		return
	}
	n, err := h.promoteObjects(ctx, q, closure)
	if err != nil {
		fail("ok", err)
		return
	}
	if _, err := validateClosure(h.store, cmd.New); err != nil {
		fail("ok", err)
		return
	}
	if err := ctx.Err(); err != nil {
		fail("ok", guard(msgCancelled, err))
		return
	}
	current, err = readRef(h.store, cmd.Name)
	if err == nil {
		err = checkOld(current, cmd)
	}
	if err != nil {
		fail("ok", err)
		return
	}
	if err := publishRef(h.path, cmd.Name, cmd.New); err != nil {
		fail("ok", guard(msgPublishFailed, err))
		return
	}
	h.tmu.Lock()
	h.promoted = n
	h.tmu.Unlock()

	// Step 6: success is reported only after publication.
	writeReport(w, "ok", cmd.Name, "ok")
}
