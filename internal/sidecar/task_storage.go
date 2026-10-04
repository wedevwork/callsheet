package sidecar

import "github.com/wedevwork/callsheet/internal/contract"

// TaskStorage is the worker's production durable task storage of one
// sidecar state root (iteration 10b), the same writers the task supervisor
// uses: a task directory published with its first execution journal (a
// staged directory renamed into tasks/), a publication checkpoint
// (publication.json), an execution journal replaced in place (a frozen
// outcome is the outbox) and the crash-recoverable removal of the whole
// task directory, its work and trusted objects included. Every write is a
// synced temporary, a rename and a directory sync. It holds no lock: the
// caller owns the state root. It exists so that the taskworkspace
// publication benchmark times the node's real persistence rather than a
// stand-in; Run never uses it.
type TaskStorage struct{ jr journal }

// NewTaskStorage returns the storage of state root root (an existing
// private directory).
func NewTaskStorage(root string) *TaskStorage {
	return &TaskStorage{jr: journal{l: layout{root: root}, d: defaultDeps()}}
}

// Dir is task id's directory.
func (s *TaskStorage) Dir(id string) string { return s.jr.l.path(taskDirRel(id)) }

// Create publishes j's new task directory with j inside.
func (s *TaskStorage) Create(j contract.ExecutionJournal) error { return s.jr.create(j) }

// Write replaces j's execution journal.
func (s *TaskStorage) Write(j contract.ExecutionJournal) error { return s.jr.write(j) }

// SaveCheckpoint replaces c's task's publication checkpoint.
func (s *TaskStorage) SaveCheckpoint(c contract.PublicationCheckpoint) error {
	return s.jr.writeCheckpoint(c)
}

// Remove deletes task id's directory (work and objects included).
func (s *TaskStorage) Remove(id string) error { return s.jr.remove(id) }
