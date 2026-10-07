package taskpublication_test

import (
	"os"
	"testing"

	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// TestMain makes the worker-side durability these tests do not assert
// no-ops for the whole process: the task databases' fsync and syncfs
// (workspacetransfer.SetSyncForTest) and the task workspace's own file and
// directory syncs (taskworkspace.SetSyncForTest). The tests assert
// publication transitions, rows and injected faults, never power loss;
// the hub's and the task store's syncs (and DirSyncFault) are untouched.
// This package has no benchmarks, so nothing here measures real syncs.
func TestMain(m *testing.M) {
	workspacetransfer.SetSyncForTest(func(int) error { return nil })
	taskworkspace.SetSyncForTest(func(*os.File) error { return nil })
	os.Exit(m.Run())
}
