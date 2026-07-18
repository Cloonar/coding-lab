// Package secretscan is the server-side leak guard on the Tracker write path:
// every content-bearing write arriving through a run token — a PR/change-request
// create, an issue create, an issue edit, an issue comment — is scanned against
// the repo's own secret values before it reaches the backend, and a write that
// carries a secret (in plaintext or any of the encodings a secret most often leaks
// through: base64, hex, URL-encoding) is REJECTED with an error naming the
// matching secret. Nothing is silently rewritten.
//
// # Why a decorator in front of the Tracker seam
//
// The scan is a Tracker decorator, not logic bolted onto each backend or each
// agent handler. A repo answers to one of two bindings — the forge REST client
// or the built-in store-backed tracker — and the whole point of the Tracker
// interface is that callers never branch on which. Wrapping the interface once,
// where the registry hands a resolved Tracker back, covers BOTH bindings with
// identical behaviour and a single implementation: there is exactly one place a
// secret-bearing body can be born and exactly one place it is checked. A guard
// duplicated per backend would inevitably drift; a guard in the HTTP handlers
// would have to be re-derived for every future write surface.
//
// # Why run-token-only is a wiring property, not a flag here
//
// The issue draws the line at "writes arriving through a run token": an
// operator editing an issue in the UI is trusted and must not be second-guessed
// by a value scan. This package contains NO notion of who the caller is — it
// scans every content write it is asked to. The restriction to agent writes is
// achieved entirely by wiring: only the resolver handed to agentapi is wrapped
// with NewResolver, so only run-token writes ever flow through a scanner. The
// operator's tracker resolver stays the bare registry. Keeping "who is trusted"
// out of this package is what makes it a leaf with one job.
//
// # Why reject, not redact
//
// A secret spliced into an issue body could be handled by punching a
// `[REDACTED]` hole through the middle of the sentence. That is worse: it lands
// a mangled, half-true comment on the forge under the run's identity, it is
// irreversible once posted, and it hides from the agent that anything went
// wrong. A clean rejection with a message the agent reads verbatim — which
// secret, which field, which form — lets the agent rewrite and retry with the
// full original text under its control. The message names the secret and the
// encoding ONLY; a value never appears in the error, and nothing in this
// package logs, so no value can reach a log line by any path.
//
// # Fail closed
//
// If the secret set cannot be read or a value cannot be decrypted, the write is
// refused with a plain error (not a BlockedError) and never delegated. A store
// hiccup or a key mismatch must not become a hole through which an unscanned
// write reaches the forge. The only cheap path is the genuinely safe one: a
// repo with zero secrets costs a single metadata query and then delegates
// untouched.
package secretscan

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// Inner is the resolution seam this package decorates: the thing that turns a
// repo into its bound Tracker. *tracker.Registry satisfies it. The interface is
// declared here (rather than importing agentapi's TrackerResolver) so the
// package stays a leaf — it depends on tracker/store/vault/secrets and nothing
// above it. A *Resolver structurally satisfies the same shape, so a wrapped
// resolver drops straight into agentapi's TrackerResolver slot.
type Inner interface {
	TrackerFor(ctx context.Context, repo store.Repo) (tracker.Tracker, error)
}

var _ Inner = (*tracker.Registry)(nil)

// Resolver wraps an Inner resolver so every Tracker it hands back scans its
// content writes against the resolved repo's secret values.
type Resolver struct {
	inner Inner
	st    *store.Store
	vlt   *vault.Vault
}

// NewResolver builds a scanning resolver. st reads the repo's secret metadata
// and encrypted blobs; vlt decrypts them at scan time (the store only ever
// holds ciphertext). Wrap ONLY the resolver handed to the run-token surface —
// operator writes must stay on the bare inner resolver.
func NewResolver(inner Inner, st *store.Store, vlt *vault.Vault) *Resolver {
	return &Resolver{inner: inner, st: st, vlt: vlt}
}

// TrackerFor resolves the repo's Tracker via the inner resolver, then binds a
// scanning decorator to repo.ID. A resolution error passes through unwrapped —
// there is nothing to scan when there is no tracker.
func (r *Resolver) TrackerFor(ctx context.Context, repo store.Repo) (tracker.Tracker, error) {
	inner, err := r.inner.TrackerFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &scanner{inner: inner, st: r.st, vlt: r.vlt, repoID: repo.ID}, nil
}

// scanner is the Tracker decorator: it scans the content of the three
// content-bearing writes and delegates everything else untouched. It is bound
// to one repo id, resolved once by TrackerFor.
type scanner struct {
	inner  tracker.Tracker
	st     *store.Store
	vlt    *vault.Vault
	repoID string
}

var (
	_ tracker.Tracker   = (*scanner)(nil)
	_ tracker.RunScoper = (*scanner)(nil)
)

// field is one named write field to scan. Field names are exactly "title" and
// "body" — the vocabulary BlockedError renders and callers see.
type field struct {
	name  string
	value string
}

const (
	fieldTitle = "title"
	fieldBody  = "body"
)

// scan runs the leak guard over the given fields, in the order supplied. It
// returns nil to allow the write, *BlockedError when a field carries a secret,
// or a plain error when the secret set cannot be read or decrypted (fail
// closed). A repo with no secrets returns nil after a single metadata query —
// the only cost the guard imposes on the common case.
func (s *scanner) scan(ctx context.Context, fields ...field) error {
	metas, err := s.st.RepoSecrets(ctx, s.repoID)
	if err != nil {
		return fmt.Errorf("secretscan: load repo secrets: %w", err)
	}
	if len(metas) == 0 {
		return nil
	}

	names := make([]string, len(metas))
	for i, m := range metas {
		names[i] = m.Name
	}
	blobs, err := s.st.RepoSecretValues(ctx, s.repoID, names)
	if err != nil {
		return fmt.Errorf("secretscan: load repo secret values: %w", err)
	}

	values := make(map[string]string, len(blobs))
	for name, blob := range blobs {
		plain, err := s.vlt.Decrypt(blob)
		if err != nil {
			// Fail closed. The message names the secret only — never the blob,
			// never the plaintext (ErrDecrypt itself is deliberately opaque).
			return fmt.Errorf("secretscan: decrypt repo secret %q: %w", name, err)
		}
		values[name] = string(plain)
	}

	matcher := secrets.NewMatcher(values)
	var hits []Match
	for _, f := range fields {
		matcher.Reset()
		for _, m := range matcher.Feed([]byte(f.value)) {
			hits = append(hits, Match{Secret: m.Name, Field: f.name, Form: m.Form})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return &BlockedError{Matches: summarize(hits)}
}

// ForRun forwards the identity-rescoping seam through the decorator (the
// instrument.go pattern): a backend that can re-author as a run is re-wrapped
// so the rescoped tracker still scans, and a backend without the seam yields
// this same scanner unchanged. Without this, wiring the scanner would mask the
// builtin tracker's ForRun and agent comments would silently fall back to the
// operator identity.
func (s *scanner) ForRun(runID string) tracker.Tracker {
	if rs, ok := s.inner.(tracker.RunScoper); ok {
		return &scanner{inner: rs.ForRun(runID), st: s.st, vlt: s.vlt, repoID: s.repoID}
	}
	return s
}

// --- content-bearing writes: scanned, then delegated ---

func (s *scanner) CreateComment(ctx context.Context, number int, body string) error {
	if err := s.scan(ctx, field{fieldBody, body}); err != nil {
		return err
	}
	return s.inner.CreateComment(ctx, number, body)
}

func (s *scanner) CreateIssue(ctx context.Context, title, body string, labels []string) (tracker.Issue, error) {
	if err := s.scan(ctx, field{fieldTitle, title}, field{fieldBody, body}); err != nil {
		return tracker.Issue{}, err
	}
	return s.inner.CreateIssue(ctx, title, body, labels)
}

func (s *scanner) CreatePull(ctx context.Context, head, base, title, body string) (tracker.PullRef, error) {
	// head/base are server-derived branch names, not agent free text — not
	// scanned. Only the agent-authored title and body are.
	if err := s.scan(ctx, field{fieldTitle, title}, field{fieldBody, body}); err != nil {
		return tracker.PullRef{}, err
	}
	return s.inner.CreatePull(ctx, head, base, title, body)
}

func (s *scanner) EditIssue(ctx context.Context, number int, edit tracker.IssueEdit) (tracker.Issue, error) {
	// An edit is a content-bearing write through a run token, so its set fields
	// are scanned before delegating — but only the fields the patch actually
	// sets: a nil (untouched) Title or Body carries no new content to leak.
	var fields []field
	if edit.Title != nil {
		fields = append(fields, field{fieldTitle, *edit.Title})
	}
	if edit.Body != nil {
		fields = append(fields, field{fieldBody, *edit.Body})
	}
	if err := s.scan(ctx, fields...); err != nil {
		return tracker.Issue{}, err
	}
	return s.inner.EditIssue(ctx, number, edit)
}

// --- everything else: pure delegation ---

func (s *scanner) ReadyIssues(ctx context.Context) ([]tracker.Issue, error) {
	return s.inner.ReadyIssues(ctx)
}

func (s *scanner) Issues(ctx context.Context, state string) ([]tracker.Issue, error) {
	return s.inner.Issues(ctx, state)
}

func (s *scanner) Issue(ctx context.Context, number int) (tracker.Issue, error) {
	return s.inner.Issue(ctx, number)
}

func (s *scanner) Pulls(ctx context.Context) ([]tracker.PullRef, error) {
	return s.inner.Pulls(ctx)
}

func (s *scanner) PullsForHead(ctx context.Context, head, base string) ([]tracker.PullRef, error) {
	// A read; head/base are server-derived branch names and a PullRef carries
	// no body — nothing to scan (CreatePull's head/base rationale).
	return s.inner.PullsForHead(ctx, head, base)
}

func (s *scanner) Pull(ctx context.Context, number int) (tracker.PullDetail, error) {
	return s.inner.Pull(ctx, number)
}

func (s *scanner) Checks(ctx context.Context, number int) ([]tracker.Check, error) {
	return s.inner.Checks(ctx, number)
}

func (s *scanner) MergePull(ctx context.Context, number int) (tracker.PullRef, error) {
	return s.inner.MergePull(ctx, number)
}

func (s *scanner) CloseIssue(ctx context.Context, number int) error {
	return s.inner.CloseIssue(ctx, number)
}

func (s *scanner) AddIssueLabels(ctx context.Context, number int, labels []string) error {
	return s.inner.AddIssueLabels(ctx, number, labels)
}

func (s *scanner) RemoveIssueLabels(ctx context.Context, number int, labels []string) error {
	return s.inner.RemoveIssueLabels(ctx, number, labels)
}

func (s *scanner) Labels(ctx context.Context) ([]tracker.Label, error) {
	return s.inner.Labels(ctx)
}

func (s *scanner) EnsureLabel(ctx context.Context, name, color, description string) (tracker.Label, error) {
	return s.inner.EnsureLabel(ctx, name, color, description)
}

// Match is one rejected occurrence: a secret name, the field it was found in
// ("title" or "body"), and the encoding form it was recognised under. It
// deliberately carries no offset and no bytes — a name/field/form triple is all
// the agent needs and all that is safe to render.
type Match struct {
	Secret string
	Field  string
	Form   secrets.Form
}

// BlockedError is returned when a content write carries a secret value. Its
// Error() is shown verbatim to the agent as the 400 body, so it is written to
// be actionable — it names every matched secret, field, and form and asks for a
// rewrite — and it contains names/fields/forms ONLY, never a value in any
// encoding.
type BlockedError struct {
	Matches []Match
}

// Error renders the rejection. A single match reads as a sentence; several are
// listed compactly. Both end with the same rewrite-and-retry instruction.
func (e *BlockedError) Error() string {
	if len(e.Matches) == 1 {
		m := e.Matches[0]
		return fmt.Sprintf(
			"tracker write rejected: %s matches the value of repo secret %q (%s form); rewrite without the secret value and retry",
			m.Field, m.Secret, m.Form)
	}
	parts := make([]string, len(e.Matches))
	for i, m := range e.Matches {
		parts[i] = fmt.Sprintf("%s (%s, %s)", m.Secret, m.Field, m.Form)
	}
	return "tracker write rejected: content matches repo secret values: " +
		strings.Join(parts, ", ") + "; rewrite without the secret values and retry"
}

// summarize deduplicates raw hits by (Secret, Field) keeping the first form
// seen (matches arrive title-before-body and, within a field, in ascending
// stream offset, so "first" is the earliest occurrence), then sorts
// deterministically by Secret then Field so the rendered message is stable.
func summarize(raw []Match) []Match {
	seen := make(map[[2]string]struct{}, len(raw))
	out := make([]Match, 0, len(raw))
	for _, m := range raw {
		key := [2]string{m.Secret, m.Field}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Secret != out[j].Secret {
			return out[i].Secret < out[j].Secret
		}
		return out[i].Field < out[j].Field
	})
	return out
}
