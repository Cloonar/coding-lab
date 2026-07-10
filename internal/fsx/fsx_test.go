package fsx

import (
	"path/filepath"
	"testing"
)

// syncDir is the durability half of the atomic-write fix (fsync the
// parent directory after rename/link). Power-loss behavior cannot be
// unit-tested; this pins that the helper works on a real directory and
// surfaces a missing one.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir on a real dir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("syncDir on a missing dir succeeded, want error")
	}
}
