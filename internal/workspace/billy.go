package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// storeFS is the only filesystem go-git's storage sees. It binds the
// library to one repository directory (osfs BoundOS) and, before every
// call, checks the absolute path against the storage-layout bounds,
// reports it to the observation seam, consults the failure seam and
// creates missing parents privately (0700, files 0600, whatever mode the
// library asks for). fence closes it: later calls fail and fence waits
// until every in-flight call and open handle has finished, so no library
// goroutine touches a generation after its workspace lock is released.
type storeFS struct {
	billy.Filesystem
	base string
	d    *deps
	g    *fenceGroup
}

// fenceGroup counts the in-flight calls (filesystem and file I/O) and
// tracks the open handles of one storage.
type fenceGroup struct {
	mu     sync.Mutex
	closed bool
	active int
	files  map[*storeFile]struct{}
	idle   *sync.Cond
}

func newFenceGroup() *fenceGroup {
	g := &fenceGroup{files: map[*storeFile]struct{}{}}
	g.idle = sync.NewCond(&g.mu)
	return g
}

var errFenced = errors.New("workspace storage is closed")

func (g *fenceGroup) enter() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errFenced
	}
	g.active++
	return nil
}

func (g *fenceGroup) leave() {
	g.mu.Lock()
	g.active--
	if g.active == 0 {
		g.idle.Broadcast()
	}
	g.mu.Unlock()
}

func (g *fenceGroup) track(f *storeFile) {
	g.mu.Lock()
	g.files[f] = struct{}{}
	g.mu.Unlock()
}

func (g *fenceGroup) untrack(f *storeFile) {
	g.mu.Lock()
	delete(g.files, f)
	g.mu.Unlock()
}

// close refuses new calls.
func (g *fenceGroup) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// fence refuses new calls, closes every handle still open (go-git v5.16.3
// leaks its pack writer's two temporary handles when a pack fails to
// index; a library goroutine still reading gets an error and unwinds) and
// waits for every in-flight call to return.
func (g *fenceGroup) fence() {
	g.mu.Lock()
	g.closed = true
	open := make([]*storeFile, 0, len(g.files))
	for f := range g.files {
		open = append(open, f)
	}
	g.mu.Unlock()
	for _, f := range open {
		f.Close()
	}
	g.mu.Lock()
	for g.active > 0 {
		g.idle.Wait()
	}
	g.mu.Unlock()
}

func newStoreFS(base string, d *deps) *storeFS {
	return &storeFS{Filesystem: osfs.New(base, osfs.WithBoundOS()), base: base, d: d, g: newFenceGroup()}
}

// storageT is go-git's filesystem storage.
type storageT = filesystem.Storage

// openStorage opens go-git storage over the repository directory repo.
// The caller closes it with closeStorage. ExclusiveAccess stays off: with
// it, go-git v5.16.3's HasEncodedObject answers "not found" for every
// packed object without consulting the packs.
func openStorage(repo string, d *deps) (*filesystem.Storage, *storeFS) {
	fs := newStoreFS(repo, d)
	return filesystem.NewStorageWithOptions(fs, cache.NewObjectLRUDefault(), filesystem.Options{}), fs
}

// closeStorage refuses further library calls, closes the storage's
// descriptors, then joins every in-flight call and open handle.
func closeStorage(s *filesystem.Storage, fs *storeFS) error {
	fs.g.close()
	err := s.Close()
	fs.g.fence()
	return err
}

func (s *storeFS) abs(name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(s.base, name)
}

// check enters the fence and validates, reports and injects for op.
func (s *storeFS) check(op, name string) (string, error) {
	p := s.abs(name)
	if !strings.HasPrefix(p+string(filepath.Separator), s.base+string(filepath.Separator)) {
		return "", errors.New("workspace storage path escapes its repository")
	}
	if err := s.g.enter(); err != nil {
		return "", err
	}
	if err := s.d.at(op, p); err != nil {
		s.g.leave()
		return "", err
	}
	return p, nil
}

// parents creates p's missing parent directories privately.
func (s *storeFS) parents(p string) error { return s.d.mkdirAll(filepath.Dir(p)) }

func (s *storeFS) Create(name string) (billy.File, error) {
	return s.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileMode)
}

func (s *storeFS) Open(name string) (billy.File, error) {
	return s.OpenFile(name, os.O_RDONLY, 0)
}

func (s *storeFS) OpenFile(name string, flag int, _ os.FileMode) (billy.File, error) {
	op := "open"
	if flag&(os.O_CREATE|os.O_WRONLY|os.O_RDWR) != 0 {
		op = "create"
	}
	p, err := s.check(op, name)
	if err != nil {
		return nil, err
	}
	if flag&os.O_CREATE != 0 {
		if err := s.parents(p); err != nil {
			s.g.leave()
			return nil, err
		}
	}
	f, err := s.Filesystem.OpenFile(name, flag, fileMode)
	defer s.g.leave()
	if err != nil {
		return nil, err
	}
	return s.g.wrap(f), nil
}

// wrap tracks a newly opened handle.
func (g *fenceGroup) wrap(f billy.File) *storeFile {
	sf := &storeFile{File: f, g: g}
	g.track(sf)
	return sf
}

// TempFile creates a private temporary file whose complete generated name
// (prefix plus the random suffix, bounded here by 32 bytes) fits the
// budget before the call.
func (s *storeFS) TempFile(dir, prefix string) (billy.File, error) {
	p, err := s.check("create", filepath.Join(dir, prefix+strings.Repeat("0", 32)))
	if err != nil {
		return nil, err
	}
	if err := s.d.mkdirAll(filepath.Dir(p)); err != nil {
		s.g.leave()
		return nil, err
	}
	f, err := s.Filesystem.TempFile(dir, prefix)
	defer s.g.leave()
	if err != nil {
		return nil, err
	}
	if s.d.observe != nil {
		// The generated name, reported for path-budget capture.
		s.d.observe("tempname", s.abs(f.Name()))
	}
	return s.g.wrap(f), nil
}

func (s *storeFS) Rename(from, to string) error {
	if err := checkPath(s.abs(from)); err != nil {
		return err
	}
	p, err := s.check("rename", to)
	if err != nil {
		return err
	}
	defer s.g.leave()
	if err := s.parents(p); err != nil {
		return err
	}
	return s.Filesystem.Rename(from, to)
}

func (s *storeFS) Remove(name string) error {
	_, err := s.check("remove", name)
	if err != nil {
		return err
	}
	defer s.g.leave()
	return s.Filesystem.Remove(name)
}

func (s *storeFS) MkdirAll(name string, _ os.FileMode) error {
	p, err := s.check("mkdir", name)
	if err != nil {
		return err
	}
	defer s.g.leave()
	return s.d.mkdirAll(p)
}

func (s *storeFS) Stat(name string) (os.FileInfo, error) {
	if _, err := s.check("stat", name); err != nil {
		return nil, err
	}
	defer s.g.leave()
	return s.Filesystem.Stat(name)
}

func (s *storeFS) Lstat(name string) (os.FileInfo, error) {
	if _, err := s.check("stat", name); err != nil {
		return nil, err
	}
	defer s.g.leave()
	return s.Filesystem.Lstat(name)
}

func (s *storeFS) ReadDir(name string) ([]os.FileInfo, error) {
	if _, err := s.check("readdir", name); err != nil {
		return nil, err
	}
	defer s.g.leave()
	return s.Filesystem.ReadDir(name)
}

// Symlink is refused: managed storage never holds links.
func (s *storeFS) Symlink(_, link string) error {
	return errUnsafe{s.abs(link)}
}

func (s *storeFS) Readlink(link string) (string, error) {
	return "", errUnsafe{s.abs(link)}
}

// Chroot binds a subdirectory with the same checks and fence.
func (s *storeFS) Chroot(name string) (billy.Filesystem, error) {
	p := s.abs(name)
	return &storeFS{Filesystem: osfs.New(p, osfs.WithBoundOS()), base: p, d: s.d, g: s.g}, nil
}

func (s *storeFS) Root() string { return s.base }

// storeFile is a tracked handle whose every operation takes a call slot:
// after the fence, operations fail and the fence joins the ones running.
type storeFile struct {
	billy.File
	g    *fenceGroup
	once sync.Once
	err  error
}

func (f *storeFile) Close() error {
	f.once.Do(func() {
		f.err = f.File.Close()
		f.g.untrack(f)
	})
	return f.err
}

func (f *storeFile) Read(p []byte) (int, error) {
	if err := f.g.enter(); err != nil {
		return 0, err
	}
	defer f.g.leave()
	return f.File.Read(p)
}

func (f *storeFile) ReadAt(p []byte, off int64) (int, error) {
	if err := f.g.enter(); err != nil {
		return 0, err
	}
	defer f.g.leave()
	return f.File.ReadAt(p, off)
}

func (f *storeFile) Write(p []byte) (int, error) {
	if err := f.g.enter(); err != nil {
		return 0, err
	}
	defer f.g.leave()
	return f.File.Write(p)
}

func (f *storeFile) Seek(off int64, whence int) (int64, error) {
	if err := f.g.enter(); err != nil {
		return 0, err
	}
	defer f.g.leave()
	return f.File.Seek(off, whence)
}

func (f *storeFile) Truncate(n int64) error {
	if err := f.g.enter(); err != nil {
		return err
	}
	defer f.g.leave()
	return f.File.Truncate(n)
}
