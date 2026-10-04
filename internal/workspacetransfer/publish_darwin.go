//go:build darwin

package workspacetransfer

import "golang.org/x/sys/unix"

// The native publication primitives of macOS (the only build-selected
// file of this package besides publish_linux.go).

// renameNoReplaceAt atomically renames from (below the directory fromDir)
// onto to (below toDir) and fails with EEXIST when to exists:
// renameatx_np(RENAME_EXCL). A filesystem without it fails (ENOTSUP);
// there is no check-then-rename fallback.
func renameNoReplaceAt(fromDir int, from string, toDir int, to string) error {
	return unix.RenameatxNp(fromDir, from, toDir, to, unix.RENAME_EXCL)
}

// fsyncFD makes an open file's or directory's content durable: F_FULLFSYNC
// (as os.File.Sync does on darwin), falling back to fsync where the
// filesystem does not support it.
func fsyncFD(fd int) error {
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0); err == nil {
		return nil
	}
	return unix.Fsync(fd)
}

// syncBatch makes a batch of new files and directories below dirFD durable
// together (iteration 10b task databases): fsync(2) hands each one's data
// and metadata to the drive (on darwin it does not flush the drive's
// cache), then one F_FULLFSYNC flushes the drive cache for all of them,
// instead of a full flush per object.
func syncBatch(dirFD int, files, dirs []string) error {
	if err := eachSync(dirFD, append(append([]string(nil), files...), dirs...), unix.Fsync); err != nil {
		return err
	}
	return fsyncFD(dirFD)
}
