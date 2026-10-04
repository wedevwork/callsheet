package sidecar

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The sidecar task journal (iteration 06a, resilience.md "Recoverable
// launch"): private tasks/ under the sidecar state root, one directory per
// stable execution holding the sidecar's execution.json, the guardian's
// owner.json, the guardian's control FIFO and regular .tmp- transaction
// artifacts. Directories are 0700, files and the FIFO 0600; symlinks,
// devices, wrong IDs, unsafe modes and unknown entries are rejected on
// every state-loading path. Every publication is a synced temporary, an
// atomic rename and a directory sync; a task directory is removed only
// after the committed result acknowledgement and verified group cleanup.
const (
	journalDir    = "tasks"
	executionName = "execution.json"
	ownerName     = "owner.json"
	controlName   = "control"
	// Iteration 10b owned task storage: the work directory (the child's
	// cwd), a workspace task's trusted object database and its publication
	// checkpoint. Their contents are the child's or the sidecar's own and
	// are never scanned; only the entries' kinds are checked.
	workName        = taskworkspace.WorkName
	objectsName     = taskworkspace.ObjectsName
	publicationName = taskworkspace.PublicationName
	// cacheDirName is the state root's disposable workspace cache.
	cacheDirName = taskworkspace.CacheDirName
)

// taskDirRel is the state-relative path of a task directory.
func taskDirRel(id string) string { return filepath.Join(journalDir, id) }

// checkPrivateFile accepts a regular file with no group or other access.
func checkPrivateFile(p string, fi fs.FileInfo) error {
	if err := checkRegular(p, fi); err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return errf(contract.CodeTrustFailed, "%s has mode %04o; task journal files must not be accessible by group or others (permissions are never repaired automatically)", p, perm)
	}
	return nil
}

// checkControl accepts the exact control FIFO: a named pipe (never a
// symlink or device) with no group or other access.
func checkControl(p string, fi fs.FileInfo) error {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return errf(contract.CodeTrustFailed, "%s is a symbolic link; sidecar state paths must be real directories and files", p)
	}
	if fi.Mode()&fs.ModeNamedPipe == 0 || fi.Mode()&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeSocket) != 0 {
		return errf(contract.CodeTrustFailed, "%s is not the task's control FIFO", p)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return errf(contract.CodeTrustFailed, "%s has mode %04o; the control FIFO must not be accessible by group or others", p, perm)
	}
	return nil
}

// Task directory transactions are crash-recoverable: a new execution's
// directory is prepared under a staged name (its first execution.json
// published inside) and renamed to its task ID in one step, and a task
// directory is renamed to a deleting name in one step before its entries
// are removed. A crash therefore leaves either a complete task directory
// or a recognizable leftover: a staged one (never published, so no
// guardian was ever started for it) or a deleting one (deleted only after
// its outcome was committed and its group proved gone, or for a refused
// start that launched nothing). Run removes both before recovery.
const (
	stagePrefix  = tempPrefix + "stage-"
	deletePrefix = tempPrefix + "delete-"
)

func stageRel(id string) string  { return filepath.Join(journalDir, stagePrefix+id) }
func deleteRel(id string) string { return filepath.Join(journalDir, deletePrefix+id) }

// leftoverID returns the task ID of a staged or deleting directory name.
func leftoverID(name string) (string, bool) {
	for _, prefix := range []string{stagePrefix, deletePrefix} {
		if id, ok := strings.CutPrefix(name, prefix); ok && contract.ValidTaskID(id) {
			return id, true
		}
	}
	return "", false
}

// scanJournals checks the optional tasks/ directory without reading
// documents (see scanJournalsFull) and returns the task IDs in order.
func (l layout) scanJournals() ([]string, error) {
	ids, _, err := l.scanJournalsFull()
	return ids, err
}

// scanJournalsFull checks the optional tasks/ directory without reading
// documents: a real private directory of task directories named by task
// IDs, each holding only execution.json, owner.json, the control FIFO and
// regular .tmp- artifacts, plus regular .tmp- files and the staged or
// deleting directories of interrupted transactions (checked alike). It
// returns the task IDs and the leftover directory names, in order.
func (l layout) scanJournalsFull() (ids, leftovers []string, err error) {
	p := l.path(journalDir)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	if err := checkDir(p, fi); err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, nil, wrapf(contract.CodeInternal, err, "cannot list %s: %v", p, err)
	}
	var unexpected []string
	for _, e := range entries {
		ep := filepath.Join(p, e.Name())
		_, leftover := leftoverID(e.Name())
		if !contract.ValidTaskID(e.Name()) && !leftover {
			if strings.HasPrefix(e.Name(), tempPrefix) {
				fi, err := os.Lstat(ep)
				if err == nil && fi.Mode().IsRegular() {
					continue
				}
			}
			unexpected = append(unexpected, ep)
			continue
		}
		fi, err := os.Lstat(ep)
		if err != nil {
			return nil, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", ep, err)
		}
		if err := checkDir(ep, fi); err != nil {
			return nil, nil, err
		}
		inner, err := os.ReadDir(ep)
		if err != nil {
			return nil, nil, wrapf(contract.CodeInternal, err, "cannot list %s: %v", ep, err)
		}
		for _, f := range inner {
			fp := filepath.Join(ep, f.Name())
			fi, err := os.Lstat(fp)
			if err != nil {
				return nil, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", fp, err)
			}
			switch n := f.Name(); {
			case n == executionName, n == ownerName, n == publicationName:
				err = checkPrivateFile(fp, fi)
			case n == controlName:
				err = checkControl(fp, fi)
			case n == workName, n == objectsName:
				err = checkOwnedDir(fp, fi)
			case strings.HasPrefix(n, tempPrefix):
				err = checkRegular(fp, fi)
			default:
				unexpected = append(unexpected, fp)
			}
			if err != nil {
				return nil, nil, err
			}
		}
		if leftover {
			leftovers = append(leftovers, e.Name())
		} else {
			ids = append(ids, e.Name())
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return nil, nil, errf(contract.CodeConflict, "sidecar task journal %s holds unexpected %s; it may contain only task directories with execution.json, owner.json and the control FIFO. Nothing was changed: move the unexpected entries out, or restore a stopped backup",
			p, strings.Join(unexpected, ", "))
	}
	sort.Strings(ids)
	sort.Strings(leftovers)
	return ids, leftovers, nil
}

// checkOwnedDir accepts an owned task directory entry (work or objects):
// a real directory, never a symlink. Its mode is the child's business
// (a child may change its own cwd's permissions).
func checkOwnedDir(p string, fi fs.FileInfo) error {
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return errf(contract.CodeTrustFailed, "%s is not a real directory; owned task storage is never followed", p)
	}
	return nil
}

// readBounded reads one private journal file, at most limit bytes (a
// sentinel byte detects a larger file).
func readPrivate(p string, limit int) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if err := checkPrivateFile(p, fi); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return b, nil
}

// journalCorrupt is the conflict for an invalid journal: never repaired or
// deleted automatically.
func journalCorrupt(p string, err error) error {
	return wrapf(contract.CodeConflict, err, "invalid sidecar task journal %s: %v; task journals are never repaired or deleted automatically: stop the sidecar, verify that the task's process group is gone, then restore a stopped backup or move the directory aside", p, err)
}

// loadedJournal is one validated journal and, if present, its owner.
type loadedJournal struct {
	j     contract.ExecutionJournal
	owner *contract.OwnerRecord
	dir   string
}

// loadJournal reads and validates one task directory's documents: the
// execution journal names its directory, and an owner record names the
// same execution.
func (l layout) loadJournal(id string, lookup contract.AdapterLookup) (loadedJournal, error) {
	dir := l.path(taskDirRel(id))
	ep := filepath.Join(dir, executionName)
	b, err := readPrivate(ep, contract.MaxExecutionJournalBytes)
	if err != nil {
		return loadedJournal{}, journalCorrupt(ep, err)
	}
	j, err := contract.ParseExecutionJournal(b, lookup)
	if err != nil {
		return loadedJournal{}, journalCorrupt(ep, err)
	}
	if j.TaskID != id {
		return loadedJournal{}, journalCorrupt(ep, errors.New("task_id does not match the directory name"))
	}
	lj := loadedJournal{j: j, dir: dir}
	op := filepath.Join(dir, ownerName)
	if _, err := os.Lstat(op); err == nil {
		ob, err := readPrivate(op, contract.MaxOwnerBytes)
		if err != nil {
			return loadedJournal{}, journalCorrupt(op, err)
		}
		o, err := contract.ParseOwner(ob)
		if err != nil {
			return loadedJournal{}, journalCorrupt(op, err)
		}
		if o.TaskID != id || o.Execution != j.Execution || (j.OwnerNonce != nil && *j.OwnerNonce != o.Nonce) {
			return loadedJournal{}, journalCorrupt(op, errors.New("the owner record names another execution"))
		}
		lj.owner = &o
	} else if !errors.Is(err, fs.ErrNotExist) {
		return loadedJournal{}, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", op, err)
	}
	return lj, nil
}

// journal publishes task journals durably through the state's failure
// injection seam; a durability step is never skipped on retry.
type journal struct {
	l layout
	d *deps
}

// syncDirAt fsyncs the directory rel (state-relative).
func (d *deps) syncDirAt(l layout, rel string) error {
	if err := d.hook("dirsync", rel); err != nil {
		return err
	}
	f, err := os.Open(l.path(rel))
	if err != nil {
		return err
	}
	err = d.syncDirFile(f)
	return errors.Join(err, f.Close())
}

// ensureRoot makes tasks/ durable (created 0700 when absent, then the
// state root synced) before a task directory is created in it.
func (jr journal) ensureRoot() error {
	p := jr.l.path(journalDir)
	if _, err := os.Lstat(p); err != nil {
		if err := jr.d.hook("mkdir", journalDir); err != nil {
			return err
		}
		if err := os.Mkdir(p, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return jr.d.syncDirAt(jr.l, rootName)
}

// create publishes a new execution's task directory with its first
// document j: the staged directory (0700) is prepared with j published
// inside it, then renamed to the task ID and tasks/ synced. A leftover
// staged directory of an earlier attempt is removed first.
func (jr journal) create(j contract.ExecutionJournal) error {
	if err := jr.ensureRoot(); err != nil {
		return err
	}
	stage := stageRel(j.TaskID)
	if err := jr.removeTree(stage); err != nil {
		return err
	}
	if err := jr.d.hook("mkdir", stage); err != nil {
		return err
	}
	if err := os.Mkdir(jr.l.path(stage), dirMode); err != nil {
		return err
	}
	if err := jr.writeIn(stage, j); err != nil {
		return err
	}
	rel := taskDirRel(j.TaskID)
	if err := jr.d.hook("rename", rel); err != nil {
		return err
	}
	if err := os.Rename(jr.l.path(stage), jr.l.path(rel)); err != nil {
		return err
	}
	return jr.d.syncDirAt(jr.l, journalDir)
}

// write atomically replaces the published execution.json of j's task
// with j (see writeIn).
func (jr journal) write(j contract.ExecutionJournal) error {
	return jr.writeIn(taskDirRel(j.TaskID), j)
}

// writeIn atomically replaces dirRel's execution.json with j: a synced
// private temporary, a rename, then the directory's sync.
func (jr journal) writeIn(dirRel string, j contract.ExecutionJournal) error {
	b, err := contract.EncodeExecutionJournal(j)
	if err != nil {
		return err
	}
	rel := filepath.Join(dirRel, executionName)
	if err := jr.d.hook("create", rel); err != nil {
		return err
	}
	f, err := os.CreateTemp(jr.l.path(dirRel), tempPrefix+executionName+"-")
	if err != nil {
		return err
	}
	name := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := jr.d.hook("write", rel); err != nil {
		return fail(err)
	}
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if err := jr.d.hook("sync", rel); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := jr.d.hook("rename", rel); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, jr.l.path(rel)); err != nil {
		os.Remove(name)
		return err
	}
	return jr.d.syncDirAt(jr.l, dirRel)
}

// writeCheckpoint atomically replaces c's task's publication.json (a
// synced private temporary, a rename, the directory's sync).
func (jr journal) writeCheckpoint(c contract.PublicationCheckpoint) error {
	b, err := contract.EncodePublicationCheckpoint(c)
	if err != nil {
		return err
	}
	return jr.writeFileIn(taskDirRel(c.TaskID), publicationName, b)
}

// readCheckpoint reads id's publication.json (nil when absent).
func (l layout) readCheckpoint(id string) (*contract.PublicationCheckpoint, error) {
	p := filepath.Join(l.path(taskDirRel(id)), publicationName)
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	b, err := readPrivate(p, contract.MaxPublicationCheckpoint)
	if err != nil {
		return nil, journalCorrupt(p, err)
	}
	c, err := contract.ParsePublicationCheckpoint(b)
	if err != nil {
		return nil, journalCorrupt(p, err)
	}
	if c.TaskID != id {
		return nil, journalCorrupt(p, errors.New("task_id does not match the directory name"))
	}
	return &c, nil
}

// writeFileIn atomically replaces dirRel/name with b: a synced private
// temporary, a rename, then the directory's sync.
func (jr journal) writeFileIn(dirRel, name string, b []byte) error {
	rel := filepath.Join(dirRel, name)
	if err := jr.d.hook("create", rel); err != nil {
		return err
	}
	f, err := os.CreateTemp(jr.l.path(dirRel), tempPrefix+name+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := jr.d.hook("write", rel); err != nil {
		return fail(err)
	}
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if err := jr.d.hook("sync", rel); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := jr.d.hook("rename", rel); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, jr.l.path(rel)); err != nil {
		os.Remove(tmp)
		return err
	}
	return jr.d.syncDirAt(jr.l, dirRel)
}

// remove deletes id's task directory crash-recoverably: a staged
// leftover is removed; the published directory is renamed to its deleting
// name (tasks/ synced) and then emptied and removed, each directory
// synced; an already absent directory still resyncs tasks/ (a retry never
// skips the durability step).
func (jr journal) remove(id string) error {
	if err := jr.removeTree(stageRel(id)); err != nil {
		return err
	}
	rel, del := taskDirRel(id), deleteRel(id)
	if _, err := os.Lstat(jr.l.path(rel)); err == nil {
		if err := jr.removeTree(del); err != nil {
			return err
		}
		if err := jr.d.hook("rename", del); err != nil {
			return err
		}
		if err := os.Rename(jr.l.path(rel), jr.l.path(del)); err != nil {
			return err
		}
		if err := jr.d.syncDirAt(jr.l, journalDir); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := jr.removeTree(del); err != nil {
		return err
	}
	if _, err := os.Lstat(jr.l.path(journalDir)); errors.Is(err, fs.ErrNotExist) {
		return nil // tasks/ was never created: nothing was published
	}
	return jr.d.syncDirAt(jr.l, journalDir)
}

// removeTree removes the transaction directory rel (staged or deleting)
// if present: its entries, the directory's sync, then the directory.
func (jr journal) removeTree(rel string) error {
	dir := jr.l.path(rel)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := jr.d.hook("remove", filepath.Join(rel, e.Name())); err != nil {
			return err
		}
		p := filepath.Join(dir, e.Name())
		if n := e.Name(); (n == workName || n == objectsName) && e.IsDir() {
			// Owned task storage (iteration 10b): removed recursively,
			// descriptor-rooted, never following a link out of it.
			if err := workspacetransfer.RemoveTree(p); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := jr.d.syncDirAt(jr.l, rel); err != nil {
		return err
	}
	if err := jr.d.hook("remove", rel); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// dropLeftovers finishes interrupted transactions at Run start, under the
// state lock and before recovery: every staged or deleting directory is
// removed and tasks/ synced.
func (jr journal) dropLeftovers(names []string) error {
	if len(names) == 0 {
		return nil
	}
	for _, n := range names {
		if err := jr.removeTree(filepath.Join(journalDir, n)); err != nil {
			return err
		}
	}
	return jr.d.syncDirAt(jr.l, journalDir)
}
