package httpapi

// M4 forge-half acceptance smoke: the FULL live chain — operator API →
// real registry (credential decrypt, RepoPath) → the REAL Forgejo REST
// client — against a canned-JSON httptest Forgejo. The only override is
// BaseURL at the injected ForgejoFactory seam: the registry pins
// https://<host>/api/v1, which can never reach a plain-HTTP httptest
// server, so the factory keeps the registry-resolved Token/Owner/Repo and
// swaps in the fake's URL. This complements TestForgeBoundRepo (stub
// Tracker, registry-resolution assertions) and the forgejo package's own
// httptest suite (client-level wire assertions) by proving the two seams
// compose end to end.

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
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/forgejo"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

func TestForgeLiveReadPath(t *testing.T) {
	const liveToken = "sekret-live-tok"

	// Canned Forgejo REST fake. Every handler asserts the Authorization
	// header the real client must send; a mutex-guarded log records which
	// ready-queue query arrived so the pinned params can be asserted.
	var (
		mu         sync.Mutex
		readyQuery string
	)
	mux := http.NewServeMux()
	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if got := r.Header.Get("Authorization"); got != "token "+liveToken {
			t.Errorf("Authorization = %q, want token %s", got, liveToken)
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	writeJSONBody := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("GET /api/v1/repos/Cloonar/forgelive/issues", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		q := r.URL.Query()
		if q.Get("page") != "1" {
			// The real client paginates until an empty page.
			writeJSONBody(w, `[]`)
			return
		}
		if q.Get("labels") == tracker.ReadyLabel {
			mu.Lock()
			readyQuery = "type=" + q.Get("type") + " state=" + q.Get("state") + " labels=" + q.Get("labels")
			mu.Unlock()
			writeJSONBody(w, `[
				{"number":4,"title":"queued on the forge","body":"live","state":"open",
				 "labels":[{"name":"ready-for-agent"}],"comments":2,
				 "created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"}
			]`)
			return
		}
		writeJSONBody(w, `[
			{"number":7,"title":"forge issue","body":"b","state":"open",
			 "labels":[{"name":"bug"}],
			 "created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"},
			{"number":4,"title":"queued on the forge","body":"live","state":"open",
			 "labels":[{"name":"ready-for-agent"}],
			 "created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"}
		]`)
	})
	mux.HandleFunc("GET /api/v1/repos/Cloonar/forgelive/issues/7", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSONBody(w, `{"number":7,"title":"forge issue","body":"b","state":"open",
			"labels":[{"name":"bug"}],
			"created_at":"2026-07-01T12:00:00Z","updated_at":"2026-07-01T12:00:00Z"}`)
	})
	mux.HandleFunc("GET /api/v1/repos/Cloonar/forgelive/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSONBody(w, `[
			{"body":"from the forge","user":{"login":"alice"},"created_at":"2026-07-01T12:05:00Z"}
		]`)
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	// Real registry, real Forgejo client; only BaseURL redirected to the fake.
	x, vlt := newTrackerServer(t, func(c tracker.ForgejoConfig) tracker.Tracker {
		return forgejo.New(c.HTTPClient, fake.URL+"/api/v1", c.Token, c.Owner, c.Repo)
	})
	blob, err := vlt.EncryptPayload(vault.ForgeTokenPayload{Host: "forge.example.com", Token: liveToken})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	credID := ids.NewID("cred")
	if _, err := x.st.CreateCredential(context.Background(), credID, "forge", store.CredentialKindForgeToken, blob, time.Now()); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	repo := seedTrackerRepo(t, x, "forgelive", func(r *store.Repo) {
		r.TrackerBinding = store.TrackerBindingForge
		r.ForgeKind = "forgejo"
		r.ForgeCredentialID = &credID
	})
	base := "/api/v1/repos/" + repo.ID

	// Ready queue flows through the real REST client to the canned JSON,
	// including Forgejo's `comments` count (list views load no bodies).
	resp := x.do("GET", base+"/ready", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	items := issuesOf(t, decodeBody(t, resp))
	if len(items) != 1 ||
		items[0]["number"] != float64(4) || items[0]["title"] != "queued on the forge" {
		t.Fatalf("live ready queue = %v", items)
	}
	if items[0]["comments_count"] != float64(2) {
		t.Fatalf("live ready comments_count = %v, want 2", items[0]["comments_count"])
	}
	mu.Lock()
	if readyQuery != "type=issues state=open labels="+tracker.ReadyLabel {
		t.Fatalf("ready-queue query = %q", readyQuery)
	}
	mu.Unlock()

	// Detail assembles issue + comments across two live REST calls.
	resp = x.do("GET", base+"/issues/7", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	detail := decodeBody(t, resp)
	cs, _ := detail["comments"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["author"] != "alice" {
		t.Fatalf("live detail comments = %v", cs)
	}

	// An issue the forge does not know (the fake's mux answers a plain 404)
	// maps through the real client's typed sentinel to an operator-facing
	// 404 — not a 502 masquerading as a forge outage.
	resp = x.do("GET", base+"/issues/999", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	if body := decodeBody(t, resp); body["error"] != "not found" {
		t.Fatalf("live forge miss = %v, want opaque not found", body)
	}

	// Mutations on a forge-bound repo answer the pinned 409.
	resp = x.do("POST", base+"/issues", map[string]any{"title": "nope"}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
	if body := decodeBody(t, resp); body["error"] != "repository uses a forge tracker; manage issues on the forge" {
		t.Fatalf("409 body = %v", body)
	}
}
