package chat

// Transcript exposure detection + masking (issue #108). A secret value
// appearing in a run's transcript is a disclosure: lab cannot rewrite the
// provider-owned transcript file, so the disk copy stays dirty and ROTATION is
// the remediation — masking here is defense-in-depth for every surface lab
// itself renders. Both read paths flow through scanAndRedact: Service.Read
// (the HTTP path, including ended runs' history) and the tailer's tick loop
// (which must mask BEFORE the notify gate snapshots a push body — see
// tail's call site). The store's sticky exposed flag (MarkRepoSecretExposed,
// first-writer-wins) turns "the value matched somewhere" into exactly one
// unset→exposed edge per exposure episode, which drives one log line, one
// push, and one run.changed — never one per matching transcript line.
//
// Broker-redaction placeholders — literal "[REDACTED:NAME]" text a broker
// already wrote — never trip any of this: the Redactor matches only a
// secret's derived VALUE forms, and its own placeholder output reproduces
// none of them (secrets.Redactor's idempotence contract).

import (
	"context"
	"sort"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// repoScopedPayload is the run.changed envelope (mirrors instance's
// repoScopedPayload — envelopes, not state, brief §8.1): the SPA refetches the
// run on it, so the chat-header exposure badge appears live. RunID names the
// one run the event concerns (issue #175 — an exposure is always detected on a
// specific run's transcript), so the SPA refetches that run instead of every
// run of the repo.
type repoScopedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
	RunID  string `json:"runID,omitempty"`
}

// scanAndRedact masks every operator-visible string of the composed chat in
// place and records each secret whose value (in any derived form) was found.
// It is a no-op when cmd/lab wired no Secrets seam, and near-free for a
// zero-secret repo (the Source returns a nil redactor — THE fast path — so no
// message is ever walked).
//
// Never let a secret value out of here: no log call, error, event payload, or
// notification built below may carry the value, a matched byte form, or any
// message content — names and ids only.
func (s *Service) scanAndRedact(ctx context.Context, run store.Run, chat *provider.Chat) {
	if s.secrets == nil {
		return
	}
	red, err := s.secrets(ctx, run.RepoID)
	if err != nil {
		// Fail OPEN, deliberately: masking is defense-in-depth — the
		// provider-owned disk copy is already dirty and rotation is the real
		// remediation — so chat availability wins over failing closed. The chat
		// is served untouched and the next read retries. Names/ids only in the
		// log; the Source's error carries repo id and secret name, never a blob
		// or value.
		s.log.Warn("building secret redactor", "component", "chat", "run", run.ID, "err", err)
		return
	}
	if red == nil {
		return // zero-secret repo: nothing to scan
	}
	for _, name := range redactChat(red, chat) {
		newly, err := s.store.MarkRepoSecretExposed(ctx, run.RepoID, name, run.ID, s.now())
		if err != nil {
			s.log.Warn("marking secret exposed", "component", "chat", "secret", name, "run", run.ID, "err", err)
			continue
		}
		if !newly {
			continue // sticky: someone (an earlier read, a concurrent tick) already flipped it
		}
		// The unset→exposed edge — exactly once per exposure episode, by the
		// store's first-writer-wins UPDATE. Name/ids only, never the value, the
		// matched text, or message content.
		s.log.Info("secret exposed in transcript", "component", "chat",
			"secret", name, "run", run.ID, "repo", run.RepoID)
		// The exposure push is deliberately NOT debounced through the
		// notifyGate: the DB edge above already guarantees exactly-once per
		// episode, and a disclosure should not wait out a flap window. Same
		// injected seam as the needs-input push (ADR-0039 — chat never imports
		// push), nil-safe. The Tag is distinct from the needs-input tag (bare
		// run.ID) so tag-coalescing never swallows an exposure behind a
		// needs-input push, and distinct per secret so two exposures both
		// surface.
		if s.notify != nil {
			s.notify(Notification{
				Title: "Secret " + name + " exposed",
				Body:  "Its value appeared in " + run.DisplayName() + "'s transcript. Rotate the secret to clear the exposure.",
				Tag:   run.ID + ":exposed:" + name,
				Route: "/runs/" + run.ID,
			})
		}
		// run.changed (not run.messages.changed): the exposure badge hangs off
		// the RUN resource, so the SPA must refetch the run itself — and with
		// the run identity in the envelope (issue #175), only that run.
		s.bus.Publish(events.Event{Type: eventRunChanged,
			Payload: repoScopedPayload{Type: eventRunChanged, RepoID: run.RepoID, RunID: run.ID}})
	}
}

// redactChat masks every operator-visible string in chat in place and returns
// the sorted, de-duplicated union of secret names hit anywhere. Masked fields
// are exactly the free-form content an agent (or its tools) can echo a value
// into; provider-controlled vocabulary — message Kind/Role, Tool.Name/Status,
// timestamps, the state string — is left alone, since a secret can only ride
// free text. Lifecycle summaries ride Message.Text, so the Text sweep covers
// every message kind. A tool's rich View union (#146), when present, carries
// the same operator-visible free-form content — a file path, a diff or write
// body, a shell command line — so its Path/Text/Command are masked field-for-
// field beside the chip's Input/Output.
func redactChat(red *secrets.Redactor, chat *provider.Chat) []string {
	hit := map[string]struct{}{}
	mask := func(p *string) {
		masked, names := red.Redact(*p)
		if names == nil {
			return // no-hit fast path: the input string is returned unchanged
		}
		*p = masked
		for _, n := range names {
			hit[n] = struct{}{}
		}
	}
	for i := range chat.Messages {
		m := &chat.Messages[i]
		mask(&m.Text)
		if m.Tool != nil {
			mask(&m.Tool.Title)
			mask(&m.Tool.Input)
			mask(&m.Tool.Output)
			if m.Tool.View != nil {
				mask(&m.Tool.View.Path)
				mask(&m.Tool.View.Text)
				mask(&m.Tool.View.Command)
			}
		}
		if m.Dialog != nil {
			redactDialog(mask, m.Dialog)
		}
	}
	// The live side-channel dialog is not a message — walk it with the same
	// helper so the pending picker the operator is looking at is masked too.
	if chat.PendingDialog != nil {
		redactDialog(mask, chat.PendingDialog)
	}
	if len(hit) == 0 {
		return nil
	}
	names := make([]string, 0, len(hit))
	for n := range hit {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// redactDialog masks one dialog's operator-visible strings in place — the
// prompt (a plan-review dialog carries the whole plan body there), every
// option's label/description in both the flat and the multi-question shape,
// and an answered dialog's recorded outcome (the Q→A history summary renders
// those verbatim). Shared by message-embedded dialogs and Chat.PendingDialog.
func redactDialog(mask func(*string), d *provider.Dialog) {
	mask(&d.Prompt)
	redactOptions(mask, d.Options)
	for i := range d.Questions {
		q := &d.Questions[i]
		mask(&q.Header)
		mask(&q.Text)
		redactOptions(mask, q.Options)
	}
	if d.Outcome != nil {
		mask(&d.Outcome.Feedback)
		for i := range d.Outcome.Results {
			r := &d.Outcome.Results[i]
			mask(&r.Question)
			for j := range r.Chosen {
				mask(&r.Chosen[j])
			}
			mask(&r.OtherText)
		}
	}
}

func redactOptions(mask func(*string), opts []provider.DialogOption) {
	for i := range opts {
		mask(&opts[i].Label)
		mask(&opts[i].Description)
	}
}
