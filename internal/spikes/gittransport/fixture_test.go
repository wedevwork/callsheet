package gittransport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

type fileSpec struct {
	Mode    filemode.FileMode
	Content []byte
}

var fixedWhen = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func sig(when time.Time) object.Signature {
	return object.Signature{Name: "Callsheet Spike", Email: "spike@callsheet.invalid", When: when}
}

func writeRaw(s storer.EncodedObjectStorer, t plumbing.ObjectType, content []byte) (plumbing.Hash, error) {
	o := s.NewEncodedObject()
	o.SetType(t)
	o.SetSize(int64(len(content)))
	w, err := o.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	w.Write(content)
	w.Close()
	return s.SetEncodedObject(o)
}

func writeObject(s storer.EncodedObjectStorer, obj interface {
	Encode(plumbing.EncodedObject) error
}) (plumbing.Hash, error) {
	o := s.NewEncodedObject()
	if err := obj.Encode(o); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.SetEncodedObject(o)
}

// writeTree stores blobs and (nested) trees for files and returns the root.
func writeTree(s storer.EncodedObjectStorer, files map[string]fileSpec) (plumbing.Hash, error) {
	type dir struct {
		files map[string]fileSpec
		dirs  map[string]*dir
	}
	root := &dir{files: map[string]fileSpec{}, dirs: map[string]*dir{}}
	for p, f := range files {
		parts := strings.Split(p, "/")
		d := root
		for _, part := range parts[:len(parts)-1] {
			if d.dirs[part] == nil {
				d.dirs[part] = &dir{files: map[string]fileSpec{}, dirs: map[string]*dir{}}
			}
			d = d.dirs[part]
		}
		d.files[parts[len(parts)-1]] = f
	}
	var build func(d *dir) (plumbing.Hash, error)
	build = func(d *dir) (plumbing.Hash, error) {
		var entries []object.TreeEntry
		for name, f := range d.files {
			h, err := writeRaw(s, plumbing.BlobObject, f.Content)
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
		sortEntries(entries)
		return writeObject(s, &object.Tree{Entries: entries})
	}
	return build(root)
}

// sortEntries applies git's tree ordering (directories compare with '/').
func sortEntries(entries []object.TreeEntry) {
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
}

func writeCommit(s storer.EncodedObjectStorer, tree plumbing.Hash, parents []plumbing.Hash, msg string, when time.Time) (plumbing.Hash, error) {
	return writeObject(s, &object.Commit{
		Author: sig(when), Committer: sig(when), Message: msg, TreeHash: tree, ParentHashes: parents,
	})
}

// seedFiles is C0's content: text, an executable, binary bytes and a UTF-8
// filename.
func seedFiles() map[string]fileSpec {
	bin := make([]byte, 512)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	return map[string]fileSpec{
		"README.md":  {filemode.Regular, []byte("# Callsheet spike\nline two\n")},
		"bin/run.sh": {filemode.Executable, []byte("#!/bin/sh\necho spike\n")},
		"data.bin":   {filemode.Regular, bin},
		"日本語.txt":    {filemode.Regular, []byte("こんにちは\n")},
	}
}

// seedBare creates a bare repo with refs/heads/main = C0 via go-git APIs.
func seedBare(t testing.TB, dir string) (plumbing.Hash, plumbing.Hash) {
	t.Helper()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		Bare:        true,
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := writeTree(repo.Storer, seedFiles())
	if err != nil {
		t.Fatal(err)
	}
	c0, err := writeCommit(repo.Storer, tree, nil, "C0 seed", fixedWhen)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.Main, c0)); err != nil {
		t.Fatal(err)
	}
	return c0, tree
}

// treeFiles flattens a tree into path -> (mode, content).
func treeFiles(t testing.TB, s storer.EncodedObjectStorer, tree plumbing.Hash) map[string]fileSpec {
	t.Helper()
	tr, err := object.GetTree(s, tree)
	if err != nil {
		t.Fatalf("tree %s: %v", tree, err)
	}
	out := map[string]fileSpec{}
	var walk func(prefix string, tr *object.Tree)
	walk = func(prefix string, tr *object.Tree) {
		for _, e := range tr.Entries {
			p := path.Join(prefix, e.Name)
			if e.Mode == filemode.Dir {
				sub, err := object.GetTree(s, e.Hash)
				if err != nil {
					t.Fatalf("subtree %s: %v", p, err)
				}
				walk(p, sub)
				continue
			}
			b, err := object.GetBlob(s, e.Hash)
			if err != nil {
				t.Fatalf("blob %s: %v", p, err)
			}
			r, _ := b.Reader()
			content, _ := io.ReadAll(r)
			r.Close()
			out[p] = fileSpec{e.Mode, content}
		}
	}
	walk("", tr)
	return out
}

func equalFiles(a, b map[string]fileSpec) error {
	if len(a) != len(b) {
		return fmt.Errorf("file count %d != %d", len(a), len(b))
	}
	for p, fa := range a {
		fb, ok := b[p]
		if !ok {
			return fmt.Errorf("missing %s", p)
		}
		if fa.Mode != fb.Mode || !bytes.Equal(fa.Content, fb.Content) {
			return fmt.Errorf("%s differs (mode %s vs %s)", p, fa.Mode, fb.Mode)
		}
	}
	return nil
}

// packOf encodes a non-thin pack containing exactly hashes from s.
func packOf(t testing.TB, s storer.EncodedObjectStorer, hashes ...plumbing.Hash) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, s, false).Encode(hashes, 10); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// rpcResult is the outcome of a raw receive-pack RPC.
type rpcResult struct {
	Status int
	Report *packp.ReportStatus
	Body   string
}

func (r rpcResult) ok() bool {
	return r.Status == http.StatusOK && r.Report != nil && r.Report.Error() == nil
}

func (r rpcResult) String() string {
	if r.Report != nil {
		var sb strings.Builder
		sb.WriteString("unpack " + r.Report.UnpackStatus)
		for _, c := range r.Report.CommandStatuses {
			sb.WriteString("; " + string(c.ReferenceName) + " " + c.Status)
		}
		return fmt.Sprintf("HTTP %d: %s", r.Status, sb.String())
	}
	return fmt.Sprintf("HTTP %d: %q", r.Status, r.Body)
}

func encodeRequest(t testing.TB, cmds []*packp.Command, caps []capability.Capability, pack []byte) []byte {
	t.Helper()
	req := packp.NewReferenceUpdateRequest()
	for _, c := range caps {
		req.Capabilities.Set(c)
	}
	req.Commands = cmds
	if pack != nil {
		req.Packfile = io.NopCloser(bytes.NewReader(pack))
	}
	var buf bytes.Buffer
	if err := req.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func postReceive(ctx context.Context, client *http.Client, repoURL string, body io.Reader) (rpcResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repoURL+"/git-receive-pack", body)
	if err != nil {
		return rpcResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := client.Do(req)
	if err != nil {
		return rpcResult{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return rpcResult{}, err
	}
	res := rpcResult{Status: resp.StatusCode, Body: string(b)}
	if resp.StatusCode == http.StatusOK {
		rs := packp.NewReportStatus()
		if err := rs.Decode(bytes.NewReader(b)); err == nil {
			res.Report = rs
		}
	}
	return res, nil
}

// rawReceive sends one raw packp receive-pack RPC with report-status.
func rawReceive(t testing.TB, ctx context.Context, client *http.Client, repoURL string, cmds []*packp.Command, pack []byte, extraCaps ...capability.Capability) rpcResult {
	t.Helper()
	caps := append([]capability.Capability{capability.ReportStatus}, extraCaps...)
	res, err := postReceive(ctx, client, repoURL, bytes.NewReader(encodeRequest(t, cmds, caps, pack)))
	if err != nil {
		t.Fatalf("receive-pack RPC: %v", err)
	}
	return res
}

func emptyPack(t testing.TB, s storer.EncodedObjectStorer) []byte { return packOf(t, s) }

// refSnapshot returns all refs (name -> hash or "-> target") from a freshly
// opened copy of the bare repository.
func refSnapshot(t testing.TB, bare string) map[string]string {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	iter, err := repo.Storer.IterReferences()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	iter.ForEach(func(r *plumbing.Reference) error {
		if r.Type() == plumbing.SymbolicReference {
			out[string(r.Name())] = "-> " + string(r.Target())
		} else {
			out[string(r.Name())] = r.Hash().String()
		}
		return nil
	})
	return out
}

func sameRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
