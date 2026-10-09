package claudecode

// RetainTranscript (issue #81): the adapter moves the located
// <sessionId>.jsonl — and only it — out of the run's HOME into core's
// retention dir, and an ENDED ReadChat of the retained path renders exactly
// what the in-place read did. The move mechanics (containment, refusals) are
// pinned on provider.RetainFile; this pins the adapter's wiring and layout.

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

const retainTranscriptBody = `{"type":"user","timestamp":"2026-10-09T00:00:00.000Z","message":{"role":"user","content":"hello claude"}}` + "\n" +
	`{"type":"assistant","timestamp":"2026-10-09T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello operator"}]}}` + "\n"

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
			home, worktree, destDir := t.TempDir(), "/work/retain", t.TempDir()
			const sid = "11111111-2222-4333-8444-555555555555"
			src := transcriptPathFor(home, worktree, sid)
			if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, []byte(retainTranscriptBody), 0o600); err != nil {
				t.Fatal(err)
			}
			// The sidecar dir claude keeps beside the transcript: ReadChat never
			// reads it, so it deliberately stays behind (and dies with the wipe).
			sidecar := filepath.Join(filepath.Dir(src), sid, "subagents", "agent-a1.jsonl")
			if err := os.MkdirAll(filepath.Dir(sidecar), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sidecar, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			before, err := p.ReadChat(provider.ReadSpec{RunID: "r1", TranscriptPath: src})
			if err != nil || len(before.Messages) != 2 {
				t.Fatalf("pre-retain ENDED read = %d message(s), %v; want 2", len(before.Messages), err)
			}

			got, err := p.RetainTranscript(ctx, worktree, home, src, destDir)
			if err != nil {
				t.Fatalf("RetainTranscript: %v", err)
			}
			if want := filepath.Join(destDir, sid+".jsonl"); got != want {
				t.Errorf("retained path = %q; want %q (base name kept)", got, want)
			}
			if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("transcript still in the HOME after the move (lstat err %v)", err)
			}
			if entries, _ := os.ReadDir(destDir); len(entries) != 1 {
				t.Errorf("destDir holds %d entries; want the transcript alone (no sidecar)", len(entries))
			}
			if _, err := os.Stat(sidecar); err != nil {
				t.Errorf("sidecar moved or removed: %v; it must stay in the HOME", err)
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
	for _, src := range []string{"", transcriptPathFor(home, "/work/retain", "gone")} {
		if got, err := p.RetainTranscript(ctx, "/work/retain", home, src, destDir); err != nil || got != "" {
			t.Errorf("RetainTranscript(%q) = %q, %v; want \"\", nil", src, got, err)
		}
	}
	// Outside the run's HOME: refused, nothing moved.
	other := t.TempDir()
	foreign := filepath.Join(other, "x.jsonl")
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
