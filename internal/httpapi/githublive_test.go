package httpapi

// ADR-0015 github-half acceptance smoke: the FULL live chain — operator API →
// real registry (flavor routing, credential decrypt, RepoPath) → the REAL
// GitHub REST client — against a canned-JSON httptest GitHub. The only override
// is BaseURL at the injected GitHubFactory seam: the registry pins
// https://<host> (api.github.com), which can never reach a plain-HTTP httptest
// server, so the factory keeps the registry-resolved Token/Owner/Repo and swaps
// in the fake's URL. This complements TestForgeFlavorMismatch (registry
// resolution), the github package's own httptest suite (wire assertions), and
// the forgelive smoke (the forgejo flavor) by proving the github seam composes
// end to end.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/github"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

func TestGitHubLiveReadPath(t *testing.T) {
	const liveToken = "sekret-gh-live-tok"

	var (
		mu         sync.Mutex
		readyQuery string
	)
	mux := http.NewServeMux()
	requireBearer := func(w http.ResponseWriter, r *http.Request) bool {
		if got := r.Header.Get("Authorization"); got != "Bearer "+liveToken {
			t.Errorf("Authorization = %q, want Bearer %s", got, liveToken)
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got == "" {
			t.Errorf("missing X-GitHub-Api-Version header")
		}
		return true
	}
	writeJSONBody := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("GET /repos/Cloonar/githublive/issues", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		q := r.URL.Query()
		if q.Get("labels") == tracker.ReadyLabel {
			mu.Lock()
			readyQuery = "state=" + q.Get("state") + " labels=" + q.Get("labels")
			mu.Unlock()
			// One PR folds into the issues list and must be dropped.
			writeJSONBody(w, `[
				{"number":4,"title":"queued on github","body":"live","state":"open",
				 "labels":[{"name":"ready-for-agent"}],"comments":2,
				 "created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"},
				{"number":6,"title":"a ready PR","state":"open","pull_request":{"url":"u"},
				 "labels":[{"name":"ready-for-agent"}],
				 "created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"}
			]`)
			return
		}
		writeJSONBody(w, `[
			{"number":7,"title":"github issue","body":"b","state":"open",
			 "labels":[{"name":"bug"}],
			 "created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"}
		]`)
	})
	mux.HandleFunc("GET /repos/Cloonar/githublive/issues/7", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		writeJSONBody(w, `{"number":7,"title":"github issue","body":"b","state":"open",
			"labels":[{"name":"bug"}],
			"created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"}`)
	})
	mux.HandleFunc("GET /repos/Cloonar/githublive/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		writeJSONBody(w, `[
			{"body":"from github","user":{"login":"alice"},"created_at":"2026-07-01T12:05:00Z"}
		]`)
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	// Real registry, real GitHub client; only BaseURL redirected to the fake.
	x, vlt := newTrackerServer(t, nil, func(c tracker.GitHubConfig) tracker.Tracker {
		return github.New(c.HTTPClient, fake.URL, c.Token, c.Owner, c.Repo)
	})
	blob, err := vlt.EncryptPayload(vault.ForgeTokenPayload{Host: "api.github.com", Token: liveToken, Forge: vault.ForgeGitHub})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	credID := ids.NewID("cred")
	if _, err := x.st.CreateCredential(context.Background(), credID, "github", store.CredentialKindForgeToken, blob, time.Now()); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	repo := seedTrackerRepo(t, x, "githublive", func(r *store.Repo) {
		r.TrackerBinding = store.TrackerBindingForge
		r.ForgeKind = "github"
		r.ForgeCredentialID = &credID
		r.RemoteURL = "git@github.com:Cloonar/githublive.git"
	})
	base := "/api/v1/repos/" + repo.ID

	// Ready queue flows through the real REST client; the folded PR is dropped
	// and the `comments` count survives.
	resp := x.do("GET", base+"/ready", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	items := issuesOf(t, decodeBody(t, resp))
	if len(items) != 1 || items[0]["number"] != float64(4) || items[0]["title"] != "queued on github" {
		t.Fatalf("live ready queue = %v", items)
	}
	if items[0]["comments_count"] != float64(2) {
		t.Fatalf("live ready comments_count = %v, want 2", items[0]["comments_count"])
	}
	mu.Lock()
	if readyQuery != "state=open labels="+tracker.ReadyLabel {
		t.Fatalf("ready-queue query = %q", readyQuery)
	}
	mu.Unlock()

	// Detail assembles issue + comments across live REST calls.
	resp = x.do("GET", base+"/issues/7", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	detail := decodeBody(t, resp)
	cs, _ := detail["comments"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["author"] != "alice" {
		t.Fatalf("live detail comments = %v", cs)
	}

	// An issue GitHub does not know (the mux answers a plain 404) maps through
	// the real client's typed sentinel to an operator-facing 404.
	resp = x.do("GET", base+"/issues/999", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	if body := decodeBody(t, resp); body["error"] != "not found" {
		t.Fatalf("live github miss = %v, want opaque not found", body)
	}

	// Mutations on a forge-bound repo answer the pinned 409 (binding-level,
	// flavor-agnostic).
	resp = x.do("POST", base+"/issues", map[string]any{"title": "nope"}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
	if body := decodeBody(t, resp); body["error"] != forgeTrackerMessage {
		t.Fatalf("409 body = %v", body)
	}
}
