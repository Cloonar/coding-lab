package codex

// RetainTranscript (issue #81): the adapter moves the located rollout out of
// <home>/.codex/sessions/YYYY/MM/DD/ into core's retention dir, flat (the date
// dirs are LocateTranscript's concern only), and an ENDED ReadChat of the
// retained path renders exactly what the in-place read did. The move
// mechanics (containment, refusals) are pinned on provider.RetainFile; this
// pins the adapter's wiring and layout.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

func TestRetainTranscript(t *testing.T) {
	exdev := func(o, n string) error { return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV} }
	for _, tc := range []struct {
		name   string
		rename provider.RenameFunc
	}{
		{"same-fs rename", nil},
		{"cross-device copy fallback", exdev},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := testProvider(t, newFakeRunner())
			p.rename = tc.rename
			ctx := context.Background()
			home, destDir := t.TempDir(), t.TempDir()
			const worktree, name = "/work/retain", "rollout-2026-10-09T00-00-00-aaa.jsonl"
			src := writeRollout(t, filepath.Join(instanceCodexHome(home), "sessions"), "2026", "10", "09", name, worktree)
			f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(`{"timestamp":"2026-10-09T00:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}` + "\n" +
				`{"timestamp":"2026-10-09T00:00:02.000Z","type":"event_msg","payload":{"type":"agent_message","message":"hello operator"}}` + "\n")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatal(err)
			}
			if located, err := p.LocateTranscript(ctx, "sess", worktree, home); err != nil || located != src {
				t.Fatalf("LocateTranscript = %q, %v; want %q", located, err, src)
			}

			before, err := p.ReadChat(provider.ReadSpec{RunID: "r1", TranscriptPath: src})
			if err != nil || len(before.Messages) != 2 {
				t.Fatalf("pre-retain ENDED read = %d message(s), %v; want 2", len(before.Messages), err)
			}
			got, err := p.RetainTranscript(ctx, worktree, home, src, destDir)
			if err != nil {
				t.Fatalf("RetainTranscript: %v", err)
			}
			if want := filepath.Join(destDir, name); got != want {
				t.Errorf("retained path = %q; want %q (base name kept, no date dirs)", got, want)
			}
			if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("rollout still in the HOME after the move (lstat err %v)", err)
			}

			if err := os.RemoveAll(home); err != nil {
				t.Fatal(err)
			}
			after, err := p.ReadChat(provider.ReadSpec{RunID: "r1", TranscriptPath: got})
			if err != nil {
				t.Fatalf("ENDED read of the retained path: %v", err)
			}
			if !reflect.DeepEqual(after.Messages, before.Messages) || after.Cursor != before.Cursor {
				t.Errorf("retained read = %+v; want the pre-retain messages %+v", after.Messages, before.Messages)
			}
		})
	}
}

func TestRetainTranscript_nothingToKeep(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()
	home, destDir := t.TempDir(), t.TempDir()
	gone := filepath.Join(instanceCodexHome(home), "sessions", "2026", "10", "09", "rollout-gone.jsonl")
	for _, src := range []string{"", gone} {
		if got, err := p.RetainTranscript(ctx, "/work/retain", home, src, destDir); err != nil || got != "" {
			t.Errorf("RetainTranscript(%q) = %q, %v; want \"\", nil", src, got, err)
		}
	}
	// Outside the run's HOME: refused, nothing moved.
	foreign := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	if err := os.WriteFile(foreign, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := p.RetainTranscript(ctx, "/work/retain", home, foreign, destDir); err == nil || got != "" {
		t.Errorf("RetainTranscript(outside home) = %q, %v; want an error", got, err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("out-of-HOME file moved: %v", err)
	}
}
