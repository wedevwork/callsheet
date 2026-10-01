package workspacetransfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// The source manifest: while a source is scanned, every traversed
// directory's entry names and identity and every read file's identity
// are recorded. Before publication a second walk over the same
// directories compares membership and identities (device, inode, type,
// permissions, size, modification and change time); any difference is a
// source change (conflict), never retried. It detects ordinary edits, not
// an atomic snapshot of a hostile concurrent writer.

type manifestDir struct {
	path  string // raw slash-separated path below the root ("" = root)
	names []string
	id    identity
	files map[string]identity // entries whose identity matters
}

type manifest struct {
	dirs []*manifestDir
	// extra are checks outside the walk (HEAD, index, configuration):
	// a digest taken now and the function that takes it again.
	extra []extraCheck
}

type extraCheck struct {
	sum [32]byte
	fn  func() [32]byte
}

func newManifest() *manifest { return &manifest{} }

// addCheck records fn's current digest.
func (m *manifest) addCheck(fn func() [32]byte) {
	m.extra = append(m.extra, extraCheck{sum: fn(), fn: fn})
}

// addBaseline records sum, the digest of what was read when the data was
// first used, to be compared with fn at verification.
func (m *manifest) addBaseline(sum [32]byte, fn func() [32]byte) {
	m.extra = append(m.extra, extraCheck{sum: sum, fn: fn})
}

func (m *manifest) dir(path string, names []string, id identity) *manifestDir {
	d := &manifestDir{path: path, names: slices.Clone(names), id: id, files: map[string]identity{}}
	m.dirs = append(m.dirs, d)
	return d
}

func (d *manifestDir) file(name string, st *unix.Stat_t) { d.files[name] = identityOf(st) }

// addFile records an absolute file's digest (an absent or unreadable file
// records the zero digest).
func (m *manifest) addFile(p string) {
	m.addCheck(func() [32]byte { return digestFile(p) })
}

func digestFile(p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(b)
}

var errChanged = errors.New("source changed")

// openRel opens the raw relative directory path below rootFD without
// following any component.
func openRel(rootFD int, path string) (int, error) {
	fd, err := unix.Dup(rootFD)
	if err != nil {
		return -1, err
	}
	if path == "" {
		return fd, nil
	}
	for _, c := range strings.Split(path, "/") {
		next, err := openDirAt(fd, c)
		closeFD(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// verify re-walks the recorded directories below rootFD.
func (m *manifest) verify(ctx context.Context, rootFD int) error {
	for _, d := range m.dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		fd, err := openRel(rootFD, d.path)
		if err != nil {
			return errChanged
		}
		err = d.verify(fd)
		closeFD(fd)
		if err != nil {
			return err
		}
	}
	for _, x := range m.extra {
		if x.fn() != x.sum {
			return errChanged
		}
	}
	return nil
}

func (d *manifestDir) verify(fd int) error {
	st, err := fstatFD(fd)
	if err != nil || identityOf(&st) != d.id {
		return errChanged
	}
	names, err := readNames(fd)
	if err != nil || !slices.Equal(names, d.names) {
		return errChanged
	}
	for name, id := range d.files {
		st, err := lstatAt(fd, name)
		if err != nil || identityOf(&st) != id {
			return errChanged
		}
	}
	return nil
}
