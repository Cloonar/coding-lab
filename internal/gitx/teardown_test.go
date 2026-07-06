package gitx

import "testing"

// TestDecideTeardown is the FULL v0 decision table (git-worktrees port-spec
// §2.1 — the contract, transcribed row for row): dirty keeps both, even
// when merged; clean always drops the worktree and deletes the branch only
// when merged.
func TestDecideTeardown(t *testing.T) {
	tests := []struct {
		name               string
		dirty, merged      bool
		wantRemoveWorktree bool
		wantDeleteBranch   bool
	}{
		{"dirty keeps both", true, false, false, false},
		{"dirty wins even if merged", true, true, false, false},
		{"clean unmerged drops worktree keeps branch", false, false, true, false},
		{"clean merged drops both", false, true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideTeardown(tt.dirty, tt.merged)
			if got.RemoveWorktree != tt.wantRemoveWorktree || got.DeleteBranch != tt.wantDeleteBranch {
				t.Errorf("decideTeardown(dirty=%v, merged=%v) = %+v; want {RemoveWorktree:%v DeleteBranch:%v}",
					tt.dirty, tt.merged, got, tt.wantRemoveWorktree, tt.wantDeleteBranch)
			}
		})
	}
}
