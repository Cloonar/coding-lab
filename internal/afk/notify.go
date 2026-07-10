package afk

// The done-signal notify seam mirrors the chat tailer's needs-input trigger
// (issue #99, internal/chat/notify.go): afk reaches Web Push through an
// injected nil-safe closure so it never imports internal/push (ADR-0038
// import-boundary reasoning, the same seam shape as the AFKRunEnded metrics
// report). The trigger is the reaper's terminal classification — one send per
// run at the success edge, riding the idempotent claim, never a bus subscriber.

import (
	"context"
	"fmt"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// Notification mirrors push.Payload field-for-field so cmd/lab maps it 1:1
// without afk importing push (same import-boundary reasoning as the chat
// needs-input trigger and the AFKRunEnded metrics seam, ADR-0038 pattern).
type Notification struct {
	Title string
	Body  string
	Tag   string
	Route string
}

// doneNotification builds the one push a successful reap fires. The noun tracks
// the repo's tracker binding — the domain glossary reserves "PR" for the forge
// object and "change request" for the built-in one (CONTEXT.md). Body wants the
// pull's title, which the hot-path PullRef deliberately drops, so it makes the
// heavier Pull detail call; a fetch error or an empty title degrades the body
// to the bare "<noun> #<n>" form and warns — the send still fires and the reap
// is never affected (every tracker REST call is already bounded at 30s per
// request, so no extra timeout is layered on). Tag is the run ID so this
// replaces the same run's needs-input item on the lock screen; Route is the
// run's PWA-internal chat path, never the forge URL.
func (s *Service) doneNotification(ctx context.Context, trk tracker.Tracker, repo store.Repo, run store.Run, pull tracker.PullRef) Notification {
	noun := "change request"
	if repo.TrackerBinding == store.TrackerBindingForge {
		noun = "PR"
	}
	body := fmt.Sprintf("%s #%d", noun, pull.Number)
	if d, err := trk.Pull(ctx, pull.Number); err != nil {
		s.log.Warn("afk notify: pull detail", "component", "afk", "run", run.ID, "pull", pull.Number, "err", err)
	} else if title := strings.TrimSpace(d.Title); title != "" {
		body = title
	} else {
		s.log.Warn("afk notify: pull detail", "component", "afk", "run", run.ID, "pull", pull.Number, "reason", "empty title")
	}
	return Notification{
		Title: fmt.Sprintf("%s opened %s #%d", run.SessionName, noun, pull.Number),
		Body:  body,
		Tag:   run.ID,
		Route: "/runs/" + run.ID,
	}
}
