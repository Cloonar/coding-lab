package gitx

import "context"

// The fetch observer seam (issue #61): the readiness report answers "did the
// last fetch from the remote work?" from what lab already knows, so the one
// fetch primitive (Fetch) and the clone (CloneBare) report their outcome here
// instead of anything fetching per page view.
//
// Reporting is OPT-IN per call, through the context: only a caller that
// fetches WITH the repo's own git credential attributes its context
// (AttributeFetch), and an unattributed fetch is never reported. That is the
// point, not a convenience — the reconcile sweep fetches every repo with no
// credential at all (internal/reconcile RuntimeSweep) and fails on every
// private remote by design, so reporting it would turn each sweep into a
// false "credential broken" verdict. A new caller that forgets to attribute
// only withholds evidence; it can never plant a wrong one.

// FetchAttribution says whose fetch this is and which credential it
// authenticates with.
type FetchAttribution struct {
	// RepoID is the repo whose reference repo is fetched (the import TARGET
	// for a read-only import's snapshot fetch).
	RepoID string
	// Credential names the exact version of the git credential the fetch
	// runs with — store.CredentialStampByID over the repo's credential_id,
	// read BEFORE the credential is materialized. An outcome is only as good
	// as the credential it was observed with: once the operator picks or
	// rotates another, the stamp no longer matches and the outcome is stale.
	Credential string
	// OnBehalfOf is the repo whose spawn or /pull-base caused a fetch of
	// ANOTHER repo's reference repo — the importing repo of a read-only
	// import. "" for a repo fetching its own.
	OnBehalfOf string
}

type fetchAttributionKey struct{}

// AttributeFetch returns a context under which Fetch and CloneBare report
// their outcome to the engine's fetch observer, attributed to a. It survives
// context.WithoutCancel, so the cancellation-immune pull and merge paths
// keep their attribution.
func AttributeFetch(ctx context.Context, a FetchAttribution) context.Context {
	return context.WithValue(ctx, fetchAttributionKey{}, a)
}

// FetchObserver receives the outcome of one attributed fetch: err is nil for
// a fetch (or clone) that completed, else the engine's own shaped error
// (stderr verbatim). It is called synchronously from the fetching goroutine
// and must not block.
type FetchObserver func(a FetchAttribution, err error)

// SetFetchObserver wires the observer. Call once during startup wiring,
// before the engine runs anything — the field is read without a lock.
func (e *Engine) SetFetchObserver(obs FetchObserver) { e.onFetch = obs }

// reportFetch hands an attributed fetch outcome to the observer. Nothing is
// reported once the CALLER's context is done (a dropped request, a
// force-delete cancelling the clone, shutdown): a fetch that died with its
// caller says nothing about the remote or the credential, and a clone that
// completed for a repo being deleted must leave no record behind. The
// engine's own timeout is different and IS reported — a remote that stalls
// for the whole gitTimeout did fail the fetch, and the caller's context is
// still live then.
func (e *Engine) reportFetch(ctx context.Context, err error) {
	if e.onFetch == nil || ctx.Err() != nil {
		return
	}
	if a, ok := ctx.Value(fetchAttributionKey{}).(FetchAttribution); ok {
		e.onFetch(a, err)
	}
}
