package reale2e

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// readCommitTree reads one published commit's root files. The commit must
// have exactly one parent, present in the repository. Every entry must be
// a regular nonexecutable file (a directory, such as leftover review
// scratch, a symlink or a submodule is refused), and the source and
// artifact budgets are checked from the blob sizes before any blob is
// read.
func readCommitTree(repo *git.Repository, hash string) ([]TreeFile, string, string, error) {
	c, err := repo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		return nil, "", "", fmt.Errorf("publication: missing commit: %w", err)
	}
	if len(c.ParentHashes) != 1 {
		return nil, "", "", fmt.Errorf("publication: want exactly one parent")
	}
	parent := c.ParentHashes[0].String()
	if _, err := repo.CommitObject(c.ParentHashes[0]); err != nil {
		return nil, "", "", fmt.Errorf("publication: missing parent: %w", err)
	}
	files, err := readTree(repo, c, "publication")
	if err != nil {
		return nil, "", "", err
	}
	return files, parent, c.TreeHash.String(), nil
}

// loadBaseTree reads the planned parent. A missing object fails closed.
func loadBaseTree(repo *git.Repository, hash string) ([]TreeFile, error) {
	c, err := repo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		return nil, fmt.Errorf("publication: missing base: %w", err)
	}
	return readTree(repo, c, "base")
}

// readTree reads c's root entries within the budgets.
func readTree(repo *git.Repository, c *object.Commit, what string) ([]TreeFile, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if len(tree.Entries) > maxTreeEntries {
		return nil, fmt.Errorf("%s: %d tree entries", what, len(tree.Entries))
	}
	sizes := map[string]int64{}
	blobs := map[string]*object.Blob{}
	for _, e := range tree.Entries {
		switch {
		case e.Name == reviewScratch:
			return nil, fmt.Errorf("%s: scratch remnant %s", what, e.Name)
		case e.Mode != filemode.Regular:
			return nil, fmt.Errorf("%s: %s is not a regular nonexecutable file", what, e.Name)
		}
		blob, err := repo.BlobObject(e.Hash)
		if err != nil {
			return nil, fmt.Errorf("%s: missing blob %s: %w", what, e.Name, err)
		}
		sizes[e.Name], blobs[e.Name] = blob.Size, blob
	}
	if err := sizeBudgets(sizes); err != nil {
		return nil, err
	}
	var files []TreeFile
	for _, e := range tree.Entries {
		r, err := blobs[e.Name].Reader()
		if err != nil {
			return nil, err
		}
		b, rerr := io.ReadAll(io.LimitReader(r, maxArtifactBytes+1))
		r.Close()
		if rerr != nil {
			return nil, rerr
		}
		if int64(len(b)) != sizes[e.Name] {
			return nil, fmt.Errorf("%s: %s changed size while it was read", what, e.Name)
		}
		files = append(files, TreeFile{Path: e.Name, Mode: e.Mode, Content: b})
	}
	return files, nil
}
