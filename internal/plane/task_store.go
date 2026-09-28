package plane

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Task state (iteration 05): the optional tasks/ directory holds one
// atomic document per task, tasks/<task_id>.json, with its base64 log
// tail; no index, counter or mutable log file is authoritative.
const (
	tasksName = "tasks"
	// maxTaskFile bounds one task document, JSON and final LF included.
	maxTaskFile = 16 << 20
)

// taskRel is the root-relative path of a task document.
func taskRel(id string) string { return tasksName + "/" + id + ".json" }

// isTaskFile reports whether name is a canonical task document name.
func isTaskFile(name string) bool {
	id, ok := strings.CutSuffix(name, ".json")
	return ok && contract.ValidTaskID(id)
}

// checkPrivate accepts task documents only without any group or other
// access: they hold goals, attribution and output (unlike the public
// certificates).
func checkPrivate(p string, fi fs.FileInfo) error {
	if err := checkRegular(p, fi); err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return errf(contract.CodeTrustFailed, "%s has mode %04o; task state must not be accessible by group or others (chmod 600; permissions are never repaired automatically)", p, perm)
	}
	return nil
}

// scanTasks checks the optional tasks/ directory without reading
// documents: a real 0700 directory (never a symlink), canonical
// <task_id>.json files that are regular and private, and regular .tmp-
// artifacts; every other entry, subdirectories included, is unexpected.
func (l layout) scanTasks() (present bool, unexpected []string, err error) {
	p := l.path(tasksName)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	if err := checkDir(p, fi); err != nil {
		return true, nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return true, nil, wrapf(contract.CodeInternal, err, "cannot list %s: %v", p, err)
	}
	for _, e := range entries {
		ep := filepath.Join(p, e.Name())
		task := isTaskFile(e.Name())
		if !task && !strings.HasPrefix(e.Name(), tempPrefix) {
			unexpected = append(unexpected, ep)
			continue
		}
		fi, err := os.Lstat(ep)
		if err != nil {
			return true, nil, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", ep, err)
		}
		if fi.IsDir() {
			unexpected = append(unexpected, ep)
			continue
		}
		check := checkRegular
		if task {
			check = checkPrivate
		}
		if err := check(ep, fi); err != nil {
			return true, nil, err
		}
	}
	return true, unexpected, nil
}

// taskCorrupt is the conflict for invalid task state: never repaired,
// never deleted, and no role or node record is recreated to satisfy it.
func taskCorrupt(p string, err error) error {
	return wrapf(contract.CodeConflict, err, "invalid task state %s: %v; task state is never repaired or regenerated automatically: stop the plane, then restore a complete stopped backup of the plane state directory (or move the file aside after inspecting it)", p, err)
}

// readTaskFile reads and strictly validates one task document (its
// private mode, the 16 MiB bound with a sentinel byte, the schema and
// lifecycle, and that its task ID matches the file name). It never reads
// manual paths or probes adapters.
func (l layout) readTaskFile(id string, lookup contract.AdapterLookup) (contract.TaskRecord, error) {
	p := l.path(taskRel(id))
	fi, err := os.Lstat(p)
	if err != nil {
		return contract.TaskRecord{}, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	if err := checkPrivate(p, fi); err != nil {
		return contract.TaskRecord{}, err
	}
	if fi.Size() > maxTaskFile {
		return contract.TaskRecord{}, taskCorrupt(p, fmt.Errorf("file is larger than %d bytes", maxTaskFile))
	}
	f, err := os.Open(p)
	if err != nil {
		return contract.TaskRecord{}, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	defer f.Close()
	b, err := readSized(f, fi.Size(), maxTaskFile+1)
	if err != nil {
		return contract.TaskRecord{}, wrapf(contract.CodeInternal, err, "cannot read %s: %v", p, err)
	}
	if len(b) > maxTaskFile {
		return contract.TaskRecord{}, taskCorrupt(p, fmt.Errorf("file is larger than %d bytes", maxTaskFile))
	}
	rec, err := contract.ParseTaskRecord(b, lookup)
	if err != nil {
		return contract.TaskRecord{}, taskCorrupt(p, err)
	}
	if rec.TaskID != id {
		return contract.TaskRecord{}, taskCorrupt(p, errors.New("task_id does not match the file name"))
	}
	return rec, nil
}

// readSized reads r to EOF, at most limit bytes, into one buffer sized
// from hint (the inspected file size): a full 16 MiB document costs one
// allocation, not io.ReadAll's doubling copies. A file that grew after
// inspection still stops at limit.
func readSized(r io.Reader, hint, limit int64) ([]byte, error) {
	b := make([]byte, 0, min(hint, limit)+1)
	lr := io.LimitReader(r, limit)
	for {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)]
		}
		n, err := lr.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// loadTasks validates every task document (after scan admitted the
// layout) against the loaded registry and the enrolled nodes, one at a
// time. Both schemas are decoded strictly (schema 1 with iteration 05's
// exact validation, converted in memory; nothing is written). Each
// snapshot's node must be known and its registration order below
// next_registration_order; an order that still exists must belong to the
// same role ID and node, while a vanished order is accepted as historical
// (a removed role) for any state. Terminal logs are dropped after
// validation; nonterminal ones are kept for reconciliation.
// loadedTask is one validated document: a terminal record's log data is
// dropped after validation, its retained length kept.
type loadedTask struct {
	rec      contract.TaskRecord
	retained int
	// lateRetained is the late evidence's retained tail length (its data
	// is dropped after validation, like a terminal primary tail).
	lateRetained int
}

func (l layout) loadTasks(lookup contract.AdapterLookup, doc *roleDoc, nodes map[string]bool) ([]loadedTask, error) {
	p := l.path(tasksName)
	entries, err := os.ReadDir(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapf(contract.CodeInternal, err, "cannot list %s: %v", p, err)
	}
	byOrder := map[int]contract.RoleRecord{}
	for _, r := range doc.roles {
		byOrder[r.RegistrationOrder] = r
	}
	var out []loadedTask
	for _, e := range entries {
		if !isTaskFile(e.Name()) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		rec, err := l.readTaskFile(id, lookup)
		if err != nil {
			return nil, err
		}
		role := rec.Role
		fp := l.path(taskRel(id))
		switch cur, ok := byOrder[role.RegistrationOrder]; {
		case !nodes[role.Node]:
			return nil, taskCorrupt(fp, fmt.Errorf("its role snapshot references node %s, which is not enrolled with this plane", role.Node))
		case role.RegistrationOrder >= doc.nextOrder:
			return nil, taskCorrupt(fp, fmt.Errorf("its role snapshot has registration_order %d, not below the registry's next_registration_order %d", role.RegistrationOrder, doc.nextOrder))
		case ok && (cur.ID != role.ID || cur.Node != role.Node):
			return nil, taskCorrupt(fp, fmt.Errorf("registration_order %d belongs to role %s on node %s, not to the task's role %s on node %s", role.RegistrationOrder, cur.ID, cur.Node, role.ID, role.Node))
		}
		lt := loadedTask{rec: rec, retained: len(rec.Log.Data)}
		if contract.TaskTerminal(rec.State) {
			lt.rec.Log.Data = nil
		}
		if rec.Late != nil {
			late := *rec.Late
			lt.lateRetained = len(late.Log.Data)
			late.Log.Data = nil
			lt.rec.Late = &late
		}
		out = append(out, lt)
	}
	return out, nil
}

// loadState validates the role registry and the task documents against
// the durable node records (every state-loading entrypoint).
func (l layout) loadState(lookup contract.AdapterLookup) (*roleDoc, []loadedTask, error) {
	recs, err := l.loadNodeRecords()
	if err != nil {
		return nil, nil, err
	}
	nodes := map[string]bool{}
	for _, r := range recs {
		nodes[r.id] = true
	}
	doc, err := l.loadRoleDoc(lookup, nodes)
	if err != nil {
		return nil, nil, err
	}
	tasks, err := l.loadTasks(lookup, doc, nodes)
	return doc, tasks, err
}

// taskStore publishes task documents: a synced, closed temporary sibling,
// then a no-replace link for the first publication or an atomic rename
// for an update, then a sync of tasks/. There is no in-place truncation.
type taskStore struct {
	l layout
	d *deps
	// parentSynced records a successful sync of the state root after
	// tasks/ existed; exists that documents were loaded or published.
	// Admission and every task's writer read and set them concurrently.
	parentSynced, exists atomic.Bool
}

// ensureDir makes tasks/ (0700) durable before the first publication: it
// creates the directory when absent and syncs the state root, on every
// attempt until one sync succeeds (a durability step never skipped on
// retry).
func (st *taskStore) ensureDir() error {
	if st.parentSynced.Load() || st.exists.Load() {
		return nil
	}
	p := st.l.path(tasksName)
	if _, err := os.Lstat(p); err != nil {
		if err := st.d.hook("mkdir", tasksName); err != nil {
			return err
		}
		if err := os.Mkdir(p, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if err := st.d.syncDir(st.l, rootName); err != nil {
		return err
	}
	st.parentSynced.Store(true)
	return nil
}

// errTaskUnconfirmed marks a publication whose link or rename succeeded
// (it is visible and authoritative) but whose directory sync failed.
var errTaskUnconfirmed = errors.New("task durability unconfirmed")

// encodeTask renders a record, checking the document bound first.
func encodeTask(rec contract.TaskRecord) ([]byte, error) {
	b, err := contract.EncodeTaskRecord(rec)
	if err != nil {
		return nil, err
	}
	if len(b) > maxTaskFile {
		return nil, fmt.Errorf("the task document would be %d bytes, above %d", len(b), maxTaskFile)
	}
	return b, nil
}

// prepare writes rec's bytes to a synced, closed private temporary
// sibling. On failure nothing is left behind.
func (st *taskStore) prepare(rec contract.TaskRecord) (string, error) {
	if err := st.ensureDir(); err != nil {
		return "", err
	}
	b, err := encodeTask(rec)
	if err != nil {
		return "", err
	}
	return st.d.writeTemp(st.l, taskRel(rec.TaskID), b)
}

// publishFirst links the prepared temporary to the new task's name
// without ever replacing a document (fs.ErrExist reports a collision; the
// caller picks another ID), then syncs tasks/. After the link a sync
// failure is errTaskUnconfirmed: the document is visible. visible is the
// instant the link succeeded, read before the sync (the start deadline's
// anchor: a slow sync counts against the window, never extends it).
func (st *taskStore) publishFirst(tmp, id string) (visible time.Time, err error) {
	if err := st.d.publishNew(st.l, tmp, taskRel(id)); err != nil {
		return time.Time{}, err
	}
	visible = st.d.nodeClock.Now()
	st.exists.Store(true)
	if err := st.d.syncDir(st.l, tasksName); err != nil {
		return visible, errors.Join(errTaskUnconfirmed, err)
	}
	return visible, nil
}

// update atomically replaces a task's document with rec, then syncs
// tasks/. Before the rename the old document stays authoritative and the
// temporary is removed; after it a sync failure is errTaskUnconfirmed.
func (st *taskStore) update(rec contract.TaskRecord) error {
	tmp, err := st.prepare(rec)
	if err != nil {
		return err
	}
	rel := taskRel(rec.TaskID)
	if err := st.d.hook("rename", rel); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, st.l.path(rel)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := st.d.syncDir(st.l, tasksName); err != nil {
		return errors.Join(errTaskUnconfirmed, err)
	}
	return nil
}

// resync retries the directory sync that confirms every visible
// publication in tasks/; it is never skipped because the bytes are
// visible.
func (st *taskStore) resync() error { return st.d.syncDir(st.l, tasksName) }

// legacyPending counts the schema-1 nonterminal records a Run would
// resolve lost (the read-only loaders' prospective migration diagnostic).
func legacyPending(tasks []loadedTask) int {
	n := 0
	for _, t := range tasks {
		if t.rec.Schema == contract.LegacyTaskRecordSchemaVersion && !contract.TaskTerminal(t.rec.State) {
			n++
		}
	}
	return n
}

// migrate resolves every schema-1 nonterminal record lost (FP-8): 05 had
// no durable worker execution journal and protocol 3 cannot reconnect to
// 4, so such an execution is unrecoverable. One file at a time it writes
// schema 2 by atomic replacement: state lost with reason
// legacy_execution_unrecoverable, started_at and output preserved,
// finished_at at the migration decision. A visible but unconfirmed
// replacement is resynced before the next record; any failure fails Run
// before listening (no admission), deleting nothing and inventing no
// registry entry, and a rerun converts only the records still in schema
// 1. Terminal schema-1 history is never rewritten here. No PGID is
// invented and nothing is signalled.
func (st *taskStore) migrate(tasks []loadedTask, now time.Time, logger func(id string)) ([]loadedTask, error) {
	for i, t := range tasks {
		rec := t.rec
		if rec.Schema != contract.LegacyTaskRecordSchemaVersion || contract.TaskTerminal(rec.State) {
			continue
		}
		if rec.Revision >= contract.MaxSafeInteger {
			return nil, errf(contract.CodeConflict, "legacy task %s cannot be migrated: its revision counter is exhausted; no task was admitted", rec.TaskID)
		}
		f := now.UTC()
		rec.State, rec.FinishedAt, rec.Revision = contract.TaskLost, &f, rec.Revision+1
		rec.Reason = &contract.TaskReason{Code: contract.ReasonLegacyUnrecoverable,
			Message: "an iteration 05 execution cannot be reconciled after the upgrade; its outcome is unconfirmed"}
		rec.Schema, rec.TimeoutPolicy = contract.TaskRecordSchemaVersion, contract.TimeoutPolicyLegacy
		err := st.update(rec)
		if errors.Is(err, errTaskUnconfirmed) {
			err = st.resync()
		}
		if err != nil {
			return nil, wrapf(contract.CodeInternal, err, "cannot migrate legacy task %s: %v; no task was admitted (a rerun of the plane retries the remaining legacy records)", rec.TaskID, err)
		}
		if logger != nil {
			logger(rec.TaskID)
		}
		tasks[i] = loadedTask{rec: rec, retained: len(rec.Log.Data)}
		tasks[i].rec.Log.Data = nil
	}
	return tasks, nil
}
