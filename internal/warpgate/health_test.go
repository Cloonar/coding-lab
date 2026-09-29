package warpgate

// Health reads the BODY of GET /@warpgate/api/info, never just the status:
// upstream answers 200 to everyone and reveals version and permissions only
// to an authenticated token (wire.go point 3).

import (
	"context"
	"maps"
	"net/http"
	"testing"
)

func TestHealth(t *testing.T) {
	partial := maps.Clone(allPerms)
	partial["access_roles_assign"] = false
	noAdmin := map[string]bool{}
	for k := range allPerms {
		noAdmin[k] = false
	}

	for _, tc := range []struct {
		name string
		body string
		want Info
	}{
		{
			// The static admin token (--enable-admin-token): every permission.
			name: "static admin token", body: infoBody(allPerms, "0.29.1"),
			want: Info{Version: "0.29.1", Authenticated: true},
		},
		{
			// A per-user token whose admin role lacks one permission lab needs
			// authenticates (version shows) but is not enough.
			name: "user token missing a permission", body: infoBody(partial, "0.29.1"),
			want: Info{Version: "0.29.1", Authenticated: false},
		},
		{
			name: "user token without admin roles", body: infoBody(noAdmin, "0.29.1"),
			want: Info{Version: "0.29.1", Authenticated: false},
		},
		{
			// A rejected token is anonymous: 200, version and permissions null.
			name: "rejected token", body: infoBody(nil, ""),
			want: Info{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, answer(http.StatusOK, tc.body))
			got, err := newTestClient(t, s.URL).Health(context.Background())
			if err != nil {
				t.Fatalf("Health: %v", err)
			}
			if got != tc.want {
				t.Errorf("Health = %+v, want %+v", got, tc.want)
			}
			r := s.only(t)
			if r.Method != http.MethodGet || r.Path != "/@warpgate/api/info" {
				t.Errorf("request = %v, want GET /@warpgate/api/info", r)
			}
			if r.Header.Get(tokenHeader) != testToken {
				t.Errorf("the probe did not carry the token")
			}
		})
	}
}

// TestHealthAgainstFakeTokenCheck: end to end through the fake, which answers
// the anonymous shape unless the token matches.
func TestHealthAgainstFakeTokenCheck(t *testing.T) {
	f := newFake(t)
	good, err := f.client(t).Health(context.Background())
	if err != nil || !good.Authenticated || good.Version != "0.29.1" {
		t.Errorf("right token: %+v, %v", good, err)
	}
	wrong, err := New(Options{BaseURL: f.srv.URL, Token: "not-the-token"})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := wrong.Health(context.Background())
	if err != nil || bad.Authenticated || bad.Version != "" {
		t.Errorf("wrong token: %+v, %v; want unauthenticated, no error", bad, err)
	}
}

func TestHealthFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"server error", http.StatusInternalServerError, "Internal Server Error (reference: x)"},
		{"bad gateway", http.StatusBadGateway, "<html>proxy</html>"},
		{"html page with 200", http.StatusOK, "<!doctype html><html></html>"},
		{"array with 200", http.StatusOK, "[]"},
		{"empty 200", http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, answer(tc.status, tc.body))
			if got, err := newTestClient(t, s.URL).Health(context.Background()); err == nil {
				t.Errorf("Health = %+v, nil; want an error", got)
			}
		})
	}

	s := newStub(t, answer(http.StatusOK, "{}"))
	s.Close()
	if _, err := newTestClient(t, s.URL).Health(context.Background()); err == nil {
		t.Error("Health against a closed server succeeded")
	}
}
