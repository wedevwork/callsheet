package testkit

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Iteration 09a test-side git client: embedded go-git over the plane's
// production TLS endpoint, used to seed and fetch workspaces. It keeps the
// verified client posture: an explicit CA pool (no system roots), no
// ambient proxy, no redirects, HTTP/1.1 only, and the instance header
// through a scoped go-git AuthMethod; no global transport is registered.

// InstanceHeader mirrors contract.WorkspaceInstanceHeader (testkit
// imports no product package).
const InstanceHeader = "X-Callsheet-Workspace-Instance"

// GitHTTPClient is a verified client for one CA.
func GitHTTPClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("testkit: no CA certificate in PEM")
	}
	tr := &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("testkit: redirects are never followed")
	}}, nil
}

// instanceAuth sets the workspace instance header on every request.
type instanceAuth struct{ instance string }

func (a instanceAuth) Name() string   { return "callsheet-workspace-instance" }
func (a instanceAuth) String() string { return a.Name() }
func (a instanceAuth) SetAuth(r *http.Request) {
	if a.instance != "" {
		r.Header.Set(InstanceHeader, a.instance)
	}
}

// GitRemote is one workspace's smart-HTTP URL (https://host/ws/<name>.git)
// with its instance token (receive-pack only) and verified client.
type GitRemote struct {
	URL      string
	Instance string
	HTTP     *http.Client
}

func (r GitRemote) transport() (transport.Transport, *transport.Endpoint, error) {
	ep, err := transport.NewEndpoint(r.URL)
	if err != nil {
		return nil, nil, err
	}
	return githttp.NewClient(r.HTTP), ep, nil
}

// Refs returns the upload-pack advertisement's refs (name to hash).
func (r GitRemote) Refs(ctx context.Context) (map[string]plumbing.Hash, error) {
	tr, ep, err := r.transport()
	if err != nil {
		return nil, err
	}
	s, err := tr.NewUploadPackSession(ep, nil)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	ar, err := s.AdvertisedReferencesContext(ctx)
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return map[string]plumbing.Hash{}, nil
		}
		return nil, err
	}
	out := map[string]plumbing.Hash{}
	for n, h := range ar.References {
		out[n] = h
	}
	return out, nil
}

// PushResult is a receive-pack outcome: the decoded report (nil when the
// plane answered an HTTP error) and the transport error.
type PushResult struct {
	Report *packp.ReportStatus
	Err    error
	// PackBytes is the size of the pack sent.
	PackBytes int
}

// OK reports a fully successful push.
func (p PushResult) OK() bool { return p.Err == nil && p.Report != nil && p.Report.Error() == nil }

// DefaultPackWindow is Push's pack delta search window (go-git's default),
// so functional and stress pushes exercise the receiver's delta
// resolution.
const DefaultPackWindow uint = 10

// PushOption adjusts one Push.
type PushOption func(*pushConfig)

type pushConfig struct{ window uint }

// PackWindow sets the pack delta search window. Window 0 (no deltas) is
// for benchmark setups whose fixtures are incompressible, where the delta
// search costs CPU and saves no bytes.
func PackWindow(w uint) PushOption { return func(c *pushConfig) { c.window = w } }

// Push sends one command name old->new with a pack of every object of new
// the remote does not advertise (its advertised tips are haves), encoded
// with DefaultPackWindow unless an option says otherwise.
func (r GitRemote) Push(ctx context.Context, local storer.EncodedObjectStorer, name string, old, new plumbing.Hash, opts ...PushOption) PushResult {
	cfg := pushConfig{window: DefaultPackWindow}
	for _, o := range opts {
		o(&cfg)
	}
	tr, ep, err := r.transport()
	if err != nil {
		return PushResult{Err: err}
	}
	s, err := tr.NewReceivePackSession(ep, instanceAuth{r.Instance})
	if err != nil {
		return PushResult{Err: err}
	}
	defer s.Close()
	ar, err := s.AdvertisedReferencesContext(ctx)
	if err != nil {
		return PushResult{Err: err}
	}
	var haves []plumbing.Hash
	for _, h := range ar.References {
		if local.HasEncodedObject(h) == nil {
			haves = append(haves, h)
		}
	}
	objs, err := revlist.Objects(local, []plumbing.Hash{new}, haves)
	if err != nil {
		return PushResult{Err: err}
	}
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, local, false).Encode(objs, cfg.window); err != nil {
		return PushResult{Err: err}
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{Name: plumbing.ReferenceName(name), Old: old, New: new}}
	n := buf.Len()
	req.Packfile = io.NopCloser(&buf)
	rs, err := s.ReceivePack(ctx, req)
	return PushResult{Report: rs, Err: err, PackBytes: n}
}

// Fetch fetches wants (with the local store's commits among haves as
// haves) into local.
func (r GitRemote) Fetch(ctx context.Context, local storer.Storer, wants []plumbing.Hash, haves []plumbing.Hash) error {
	tr, ep, err := r.transport()
	if err != nil {
		return err
	}
	s, err := tr.NewUploadPackSession(ep, nil)
	if err != nil {
		return err
	}
	defer s.Close()
	ar, err := s.AdvertisedReferencesContext(ctx)
	if err != nil {
		return err
	}
	req := packp.NewUploadPackRequestFromCapabilities(ar.Capabilities)
	req.Wants = wants
	req.Haves = haves
	resp, err := s.UploadPack(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Close()
	return packfile.UpdateObjectStorage(local, resp)
}

// ---- Deterministic fixtures ----

// FileSpec is one fixture file.
type FileSpec struct {
	Mode    filemode.FileMode
	Content []byte
}

// FixedWhen is the fixtures' commit time.
var FixedWhen = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// WriteRaw stores a raw object.
func WriteRaw(s storer.EncodedObjectStorer, t plumbing.ObjectType, content []byte) (plumbing.Hash, error) {
	o := s.NewEncodedObject()
	o.SetType(t)
	o.SetSize(int64(len(content)))
	w, err := o.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.SetEncodedObject(o)
}

// WriteObject stores an encodable object (commit or tree).
func WriteObject(s storer.EncodedObjectStorer, obj interface {
	Encode(plumbing.EncodedObject) error
}) (plumbing.Hash, error) {
	o := s.NewEncodedObject()
	if err := obj.Encode(o); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.SetEncodedObject(o)
}

// WriteTree stores blobs and nested trees for files (slash-separated raw
// paths) in git order and returns the root tree.
func WriteTree(s storer.EncodedObjectStorer, files map[string]FileSpec) (plumbing.Hash, error) {
	type dir struct {
		files map[string]FileSpec
		dirs  map[string]*dir
	}
	root := &dir{files: map[string]FileSpec{}, dirs: map[string]*dir{}}
	for p, f := range files {
		parts := strings.Split(p, "/")
		d := root
		for _, part := range parts[:len(parts)-1] {
			if d.dirs[part] == nil {
				d.dirs[part] = &dir{files: map[string]FileSpec{}, dirs: map[string]*dir{}}
			}
			d = d.dirs[part]
		}
		d.files[parts[len(parts)-1]] = f
	}
	var build func(d *dir) (plumbing.Hash, error)
	build = func(d *dir) (plumbing.Hash, error) {
		var entries []object.TreeEntry
		for name, f := range d.files {
			h, err := WriteRaw(s, plumbing.BlobObject, f.Content)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: f.Mode, Hash: h})
		}
		for name, sub := range d.dirs {
			h, err := build(sub)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
		}
		SortEntries(entries)
		return WriteObject(s, &object.Tree{Entries: entries})
	}
	return build(root)
}

// SortEntries applies git's tree ordering (directories compare with '/').
func SortEntries(entries []object.TreeEntry) {
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
}

// WriteCommit stores a commit of tree with parents at a fixed identity.
func WriteCommit(s storer.EncodedObjectStorer, tree plumbing.Hash, parents []plumbing.Hash, msg string, when time.Time) (plumbing.Hash, error) {
	sig := object.Signature{Name: "Callsheet Fixture", Email: "fixture@callsheet.invalid", When: when}
	return WriteObject(s, &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree, ParentHashes: parents})
}

// CommitFiles writes files as a commit with parents into s.
func CommitFiles(s storer.EncodedObjectStorer, files map[string]FileSpec, parents []plumbing.Hash, msg string) (plumbing.Hash, error) {
	tree, err := WriteTree(s, files)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return WriteCommit(s, tree, parents, msg, FixedWhen)
}

// NewMemoryStore is an in-memory object store for fixtures.
func NewMemoryStore() *memory.Storage { return memory.NewStorage() }

// TreeFiles flattens a commit's tree into path -> (mode, content).
func TreeFiles(s storer.EncodedObjectStorer, commit plumbing.Hash) (map[string]FileSpec, error) {
	c, err := object.GetCommit(s, commit)
	if err != nil {
		return nil, err
	}
	out := map[string]FileSpec{}
	var walk func(prefix string, h plumbing.Hash) error
	walk = func(prefix string, h plumbing.Hash) error {
		t, err := object.GetTree(s, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := prefix + e.Name
			if e.Mode == filemode.Dir {
				if err := walk(p+"/", e.Hash); err != nil {
					return err
				}
				continue
			}
			b, err := object.GetBlob(s, e.Hash)
			if err != nil {
				return err
			}
			r, err := b.Reader()
			if err != nil {
				return err
			}
			content, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				return err
			}
			out[p] = FileSpec{e.Mode, content}
		}
		return nil
	}
	return out, walk("", c.TreeHash)
}

// EqualFiles reports the first difference between two flattened trees.
func EqualFiles(a, b map[string]FileSpec) error {
	if len(a) != len(b) {
		return fmt.Errorf("file count %d != %d", len(a), len(b))
	}
	for p, fa := range a {
		fb, ok := b[p]
		if !ok {
			return fmt.Errorf("missing %q", p)
		}
		if fa.Mode != fb.Mode || !bytes.Equal(fa.Content, fb.Content) {
			return fmt.Errorf("%q differs (mode %s vs %s)", p, fa.Mode, fb.Mode)
		}
	}
	return nil
}
