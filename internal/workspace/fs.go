package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Storage-layout bounds (09a hub.md, "Absolute-path budget"). The state
// root is at most MaxRootBytes; every path this package or go-git touches
// is checked against the aggregate MaxPathBytes and the per-component
// MaxComponentBytes before the filesystem call, never translated from
// ENAMETOOLONG afterwards.
const (
	// MaxRootBytes bounds the cleaned and the canonical plane state root.
	MaxRootBytes = 256
	// MaxPathBytes is 898 (the longest ordinary loose-ref path) plus the
	// 96-byte allowance for generated temporary prefixes and suffixes.
	MaxPathBytes = 994
	// MaxComponentBytes is the filesystem's name limit.
	MaxComponentBytes = 255

	dirMode  = 0o700
	fileMode = 0o600
	// copyChunk is the copy loop's read size; the context is checked
	// between chunks.
	copyChunk = 256 << 10
)

// errPathBudget reports a path outside the storage-layout bounds; it is
// raised before the filesystem call.
var errPathBudget = errors.New("workspace storage path exceeds its length budget")

// errUnsafe reports a symlink or special file inside managed storage.
type errUnsafe struct{ path string }

func (e errUnsafe) Error() string {
	return "workspace storage entry " + e.path + " is not a regular file or directory"
}

// deps are the package-private seams: entropy, clock, failure injection,
// path observation, the sync primitives and the smart-HTTP idle bound.
// Nothing here is exposed through a product API.
type deps struct {
	rand io.Reader
	now  func() time.Time
	// fail, when non-nil, is consulted with the operation and absolute
	// path before every filesystem boundary (mkdir, create, write, close,
	// sync, dirsync, rename, remove) and returns an injected error.
	fail func(op, path string) error
	// observe, when non-nil, receives every checked operation and path
	// (tests capture generated temp, lock and copy destinations).
	observe func(op, path string)
	// syncFile and syncDir make a file's and a directory's contents
	// durable.
	syncFile func(*os.File) error
	syncDir  func(*os.File) error
	// idle is the smart-HTTP no-progress deadline.
	idle time.Duration
	// hook, when non-nil, runs at named stages with the operation's
	// context (tests only: barriers and cancellation points).
	hook func(stage string, ctx context.Context)
	// metric, when non-nil, receives named work counters (benchmarks:
	// bytes and files copied per transaction, objects visited, entries
	// scanned).
	metric func(name string, v int64)
}

func (d *deps) emit(name string, v int64) {
	if d.metric != nil {
		d.metric(name, v)
	}
}

// idleTimeout is the production smart-HTTP no-progress deadline.
const idleTimeout = 30 * time.Second

func defaultDeps() *deps {
	return &deps{rand: rand.Reader, now: time.Now, syncFile: (*os.File).Sync, syncDir: syncDirFile, idle: idleTimeout}
}

func (d *deps) stage(name string, ctx context.Context) {
	if d.hook != nil {
		d.hook(name, ctx)
	}
}

// syncDirFile syncs an open directory with File.Sync and falls back to a
// plain fsync for ENOTSUP, ENOTTY and EINVAL (the plane's policy).
func syncDirFile(f *os.File) error {
	err := f.Sync()
	if err == nil || !(errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL)) {
		return err
	}
	return syscall.Fsync(int(f.Fd()))
}

// checkPath enforces the aggregate and per-component bounds.
func checkPath(p string) error {
	if len(p) > MaxPathBytes {
		return fmt.Errorf("%w: %d bytes", errPathBudget, len(p))
	}
	for _, c := range strings.Split(p, string(filepath.Separator)) {
		if len(c) > MaxComponentBytes {
			return fmt.Errorf("%w: a %d-byte component", errPathBudget, len(c))
		}
	}
	return nil
}

// at checks p, reports it and consults the failure seam.
func (d *deps) at(op, p string) error {
	if err := checkPath(p); err != nil {
		return err
	}
	if d.observe != nil {
		d.observe(op, p)
	}
	if d.fail != nil {
		return d.fail(op, p)
	}
	return nil
}

func (d *deps) mkdir(p string) error {
	if err := d.at("mkdir", p); err != nil {
		return err
	}
	return os.Mkdir(p, dirMode)
}

// mkdirAll creates the missing directories of p (each private), rejecting
// an existing non-directory.
func (d *deps) mkdirAll(p string) error {
	fi, err := os.Lstat(p)
	if err == nil {
		if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
			return errUnsafe{p}
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if parent := filepath.Dir(p); parent != p {
		if err := d.mkdirAll(parent); err != nil {
			return err
		}
	}
	if err := d.mkdir(p); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// writeFile creates p exclusively (0600) with data; it does not sync.
func (d *deps) writeFile(p string, data []byte) error {
	if err := d.at("create", p); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	if err := d.at("write", p); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := d.at("close", p); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeFileSync is writeFile followed by a file sync.
func (d *deps) writeFileSync(p string, data []byte) error {
	if err := d.writeFile(p, data); err != nil {
		return err
	}
	return d.syncFilePath(p)
}

func (d *deps) rename(from, to string) error {
	if err := checkPath(from); err != nil {
		return err
	}
	if err := d.at("rename", to); err != nil {
		return err
	}
	return os.Rename(from, to)
}

func (d *deps) remove(p string) error {
	if err := d.at("remove", p); err != nil {
		return err
	}
	return os.Remove(p)
}

func (d *deps) syncFilePath(p string) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := d.at("sync", p); err != nil {
		return err
	}
	return d.syncFile(f)
}

func (d *deps) syncDirPath(p string) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := d.at("dirsync", p); err != nil {
		return err
	}
	return d.syncDir(f)
}

// readDir lists p's entries sorted by name without following links.
func readDir(p string) ([]os.DirEntry, error) {
	es, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Name() < es[j].Name() })
	return es, nil
}

// copyStats counts one transaction's local copy work.
type copyStats struct {
	files, dirs int
	bytes       int64
}

// copyTree copies the directory src into the new directory dst with
// independent regular-file copies (no hardlinks, never following links),
// checking ctx between files and chunks. Anything but a directory or a
// regular file is rejected.
func (d *deps) copyTree(ctx context.Context, src, dst string, st *copyStats) error {
	if err := d.mkdir(dst); err != nil {
		return err
	}
	st.dirs++
	es, err := readDir(src)
	if err != nil {
		return err
	}
	for _, e := range es {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, t := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		fi, err := os.Lstat(s)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			err = d.copyTree(ctx, s, t, st)
		case fi.Mode().IsRegular():
			err = d.copyFile(ctx, s, t, st)
		default:
			err = errUnsafe{s}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *deps) copyFile(ctx context.Context, src, dst string, st *copyStats) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := d.at("create", dst); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	if err := d.at("write", dst); err != nil {
		out.Close()
		return err
	}
	buf := make([]byte, copyChunk)
	for {
		if err := ctx.Err(); err != nil {
			out.Close()
			return err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				out.Close()
				return err
			}
			st.bytes += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	if err := d.at("close", dst); err != nil {
		out.Close()
		return err
	}
	st.files++
	return out.Close()
}

// syncTree makes a new tree durable bottom-up: every regular file, then
// each directory after its children.
func (d *deps) syncTree(ctx context.Context, dir string) error {
	es, err := readDir(dir)
	if err != nil {
		return err
	}
	for _, e := range es {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			err = d.syncTree(ctx, p)
		case fi.Mode().IsRegular():
			err = d.syncFilePath(p)
		default:
			err = errUnsafe{p}
		}
		if err != nil {
			return err
		}
	}
	return d.syncDirPath(dir)
}

// removeTree deletes p recursively without following links; a symlink or
// special file inside is rejected (it is never managed storage).
func (d *deps) removeTree(p string) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case fi.IsDir():
		es, err := readDir(p)
		if err != nil {
			return err
		}
		for _, e := range es {
			if err := d.removeTree(filepath.Join(p, e.Name())); err != nil {
				return err
			}
		}
	case fi.Mode().IsRegular():
	default:
		return errUnsafe{p}
	}
	return d.remove(p)
}

// treeSize sums the logical sizes of the regular files below p (the
// directory itself has no size). Any unsafe entry, traversal error or
// overflow fails; a partial size is never returned.
func treeSize(p string) (int64, error) {
	var n int64
	return measure(p, &n)
}

// measure is treeSize counting every scanned entry into entries.
func measure(p string, entries *int64) (int64, error) {
	*entries++
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	switch {
	case fi.Mode().IsRegular():
		return fi.Size(), nil
	case !fi.IsDir():
		return 0, errUnsafe{p}
	}
	es, err := readDir(p)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range es {
		n, err := measure(filepath.Join(p, e.Name()), entries)
		if err != nil {
			return 0, err
		}
		if n > math.MaxInt64-total {
			return 0, errors.New("workspace size overflows 64 bits")
		}
		total += n
	}
	return total, nil
}

// checkPrivate rejects a link, special file or group/other-accessible
// entry.
func checkPrivate(p string, fi fs.FileInfo) error {
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return errUnsafe{p}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; workspace storage must not be accessible by group or others", p, fi.Mode().Perm())
	}
	return nil
}
