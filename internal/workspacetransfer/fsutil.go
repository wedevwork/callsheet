package workspacetransfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sort"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"golang.org/x/sys/unix"
)

// Descriptor-relative filesystem access. A local root is resolved once to
// its canonical path; everything below it is reached through opened
// directory descriptors with O_NOFOLLOW, so a symlink inside the tree
// (or one swapped in later) can never redirect a scan, an object install
// or an export write outside the root.

// deps are the package-private seams: failure injection at named
// boundaries, stage hooks (barriers), durability primitives, entropy and
// work counters. Nothing here is exposed through a product API.
type deps struct {
	// fail, when non-nil, is consulted before every named boundary (for
	// example "export-write", "object-rename") and returns an injected
	// error.
	fail func(op string) error
	// hook, when non-nil, runs at named stages (tests: barriers).
	hook func(stage string)
	// syncFD makes a file's or directory's contents durable.
	syncFD func(fd int) error
	// syncBatchFD makes the files and directories (paths relative to the
	// directory dirFD, "." for itself) durable together (iteration 10b
	// task databases); syncBatch (the platform primitive) in production.
	syncBatchFD func(dirFD int, files, dirs []string) error
	// linkAt publishes a loose object by hard link (unix.Linkat); a
	// filesystem without hard links falls back to a no-replace rename.
	linkAt func(olddirfd int, oldpath string, newdirfd int, newpath string, flags int) error
	// aliasProbe reports whether a directory's filesystem aliases names
	// (case or Unicode normalization); probeAliasing in production.
	aliasProbe func(fd int) (bool, error)
	rand       io.Reader
	// metric, when non-nil, receives work counters (benchmarks).
	metric func(name string, v int64)
	// newObjWriter, when non-nil, constructs the deflating loose-object
	// writer in place of objfile.NewWriter (objWriter; tests count the
	// constructions: a verified compressed loose copy constructs none).
	newObjWriter func(w io.Writer) *objfile.Writer
	// tempDir is the parent of private temporary stores ("" = the
	// process temporary directory).
	tempDir string
}

func defaultDeps() *deps {
	return &deps{syncFD: fsyncFD, syncBatchFD: syncBatch, linkAt: unix.Linkat, aliasProbe: probeAliasing, rand: rand.Reader}
}

// entryDeps are the exported entry points' deps: production, with the
// durability primitives of SetSyncForTest when a test installed them.
func entryDeps() *deps {
	d := defaultDeps()
	if p := syncOverride.Load(); p != nil {
		sync := *p
		d.syncFD = sync
		d.syncBatchFD = func(dirFD int, _, _ []string) error { return sync(dirFD) }
	}
	return d
}

// syncOverride, when set, is the test durability primitive of the
// exported entry points (SetSyncForTest).
var syncOverride atomic.Pointer[func(fd int) error]

// SetSyncForTest replaces the durability primitives of the deps that the
// exported entry points (CreateTaskDB, OpenTaskDB, CheckoutTask,
// SnapshotTask, Push and Pull) construct from then on: a file or directory
// sync becomes sync(fd), and a task database's batch (syncfs on Linux,
// fsyncs and one F_FULLFSYNC on darwin) becomes one sync(fd) of its
// objects directory. nil restores the platform primitives. It returns the
// previous override (nil when none).
//
// Tests only: production never calls it, so production durability is the
// platform's. Tests whose assertions do not depend on power-loss
// durability install a no-op, so repeated stress runs do not flush the
// shared filesystem on every repetition. Failure seams still run first,
// deps a test builds itself are unaffected, and fsyncFD and syncBatch
// themselves are never wrapped.
//
// Concurrency: sync is called from every goroutine that syncs through an
// entry point, so it must tolerate concurrent calls. The override is one
// process-wide value: the swap is atomic, but independent install and
// restore sequences (parallel tests each restoring the previous value)
// can interleave and leave the wrong one installed, so callers must
// coordinate them (taskworkspace's SkipDurability reference-counts its
// holders). The override is captured when an entry point constructs its
// deps: a TaskDB opened (and an operation started) while it was set keeps
// calling it after it is replaced or removed.
func SetSyncForTest(sync func(fd int) error) (previous func(fd int) error) {
	var p *func(fd int) error
	if sync != nil {
		p = &sync
	}
	if old := syncOverride.Swap(p); old != nil {
		return *old
	}
	return nil
}

// syncBatch makes a batch durable through the seam (a test deps without a
// batch primitive syncs each path with syncFD).
func (d *deps) syncBatch(dirFD int, files, dirs []string) error {
	if d.syncBatchFD != nil {
		return d.syncBatchFD(dirFD, files, dirs)
	}
	return eachSync(dirFD, append(append([]string(nil), files...), dirs...), d.syncFD)
}

// eachSync opens each path below dirFD without following links and syncs
// it with sync.
func eachSync(dirFD int, paths []string, sync func(fd int) error) error {
	for _, p := range paths {
		var fd int
		var err error
		if p == "." {
			fd, err = unix.Dup(dirFD)
		} else {
			fd, err = openRelRead(dirFD, p)
		}
		if err != nil {
			return err
		}
		err = sync(fd)
		closeFD(fd)
		if err != nil {
			return err
		}
	}
	return nil
}

// openRelRead opens a relative path below dirFD read-only, never following
// a link.
func openRelRead(dirFD int, p string) (int, error) {
	var fd int
	err := retryEINTR(func() error {
		var err error
		fd, err = unix.Openat(dirFD, p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		return err
	})
	return fd, err
}

func (d *deps) check(op string) error {
	if d.fail != nil {
		return d.fail(op)
	}
	return nil
}

func (d *deps) stage(s string) {
	if d.hook != nil {
		d.hook(s)
	}
}

func (d *deps) emit(name string, v int64) {
	if d.metric != nil {
		d.metric(name, v)
	}
}

// objWriter constructs the deflating loose-object writer over w.
func (d *deps) objWriter(w io.Writer) *objfile.Writer {
	if d.newObjWriter != nil {
		return d.newObjWriter(w)
	}
	return objfile.NewWriter(w)
}

// token returns 32 random lowercase hex digits.
func (d *deps) token() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(d.rand, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// sync syncs fd through the seam.
func (d *deps) sync(fd int, op string) error {
	if err := d.check(op); err != nil {
		return err
	}
	return d.syncFD(fd)
}

const (
	dirFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	readFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
)

func retryEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// openPathDir opens an absolute directory path without following its
// final component.
func openPathDir(p string) (int, error) {
	var fd int
	err := retryEINTR(func() (err error) { fd, err = unix.Open(p, dirFlags, 0); return })
	return fd, err
}

// openDirAt opens the directory name below dfd without following it.
func openDirAt(dfd int, name string) (int, error) {
	var fd int
	err := retryEINTR(func() (err error) { fd, err = unix.Openat(dfd, name, dirFlags, 0); return })
	return fd, err
}

// lstatAt stats name below dfd without following it.
func lstatAt(dfd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := retryEINTR(func() error { return unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) })
	return st, err
}

func fstatFD(fd int) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := retryEINTR(func() error { return unix.Fstat(fd, &st) })
	return st, err
}

// readNames returns the entry names of the open directory dfd sorted by
// raw bytes. The descriptor stays open.
func readNames(dfd int) ([]string, error) {
	dup, err := unix.Dup(dfd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(dup)
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	if _, err := unix.Seek(dup, 0, 0); err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// openFileAt opens the regular file name below dfd for reading without
// following it and without blocking on a special file; the returned
// status is the opened file's.
func openFileAt(dfd int, name string) (*os.File, unix.Stat_t, error) {
	var fd int
	if err := retryEINTR(func() (err error) { fd, err = unix.Openat(dfd, name, readFlags, 0); return }); err != nil {
		return nil, unix.Stat_t{}, err
	}
	st, err := fstatFD(fd)
	if err != nil {
		unix.Close(fd)
		return nil, st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, st, errNotRegular
	}
	return os.NewFile(uintptr(fd), name), st, nil
}

var errNotRegular = errors.New("not a regular file")

// readlinkAt reads the link text of name below dfd.
func readlinkAt(dfd int, name string) (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		var n int
		err := retryEINTR(func() (err error) { n, err = unix.Readlinkat(dfd, name, buf); return })
		if err != nil {
			return "", err
		}
		if n < size {
			return string(buf[:n]), nil
		}
		if size > 1<<20 {
			return "", errors.New("symlink text too long")
		}
	}
}

// fileType is the S_IFMT class of a status.
func fileType(st *unix.Stat_t) uint32 { return uint32(st.Mode) & unix.S_IFMT }

func isDirStat(st *unix.Stat_t) bool  { return fileType(st) == unix.S_IFDIR }
func isRegStat(st *unix.Stat_t) bool  { return fileType(st) == unix.S_IFREG }
func isLinkStat(st *unix.Stat_t) bool { return fileType(st) == unix.S_IFLNK }

// identity is the part of a file status that reveals an ordinary edit:
// device, inode, type and permissions, size and change and modification
// times.
type identity struct {
	dev, ino     uint64
	mode         uint32
	size         int64
	mtime, ctime int64
}

func identityOf(st *unix.Stat_t) identity {
	return identity{dev: uint64(st.Dev), ino: st.Ino, mode: uint32(st.Mode), size: st.Size,
		mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano()}
}

// readAllCtx reads f completely with a fixed buffer, checking ctx between
// chunks.
func readAllCtx(ctx context.Context, f *os.File) ([]byte, error) {
	var out []byte
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// closeFD closes a descriptor, ignoring EINTR's retry semantics (a close
// is never retried).
func closeFD(fd int) { unix.Close(fd) }
