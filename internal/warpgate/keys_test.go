package warpgate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const testRunID = "run_0123456789abcdef0123456789abcdef"

// testAuthorizedKey returns a fresh ed25519 public key as an authorized_keys
// line with a comment, the shape lab's spawn path produces.
func testAuthorizedKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + RunKeyLabel(testRunID)
}

func TestRunKeyLabelRoundTrip(t *testing.T) {
	if got := RunKeyLabel(testRunID); got != "lab-run:"+testRunID {
		t.Errorf("RunKeyLabel = %q", got)
	}
	if id, ok := RunIDFromLabel(RunKeyLabel(testRunID)); !ok || id != testRunID {
		t.Errorf("RunIDFromLabel(RunKeyLabel) = %q, %v", id, ok)
	}
	for _, label := range []string{
		"",
		"laptop",
		"lab-run:",
		"lab-run:run_",
		"lab-run:run_0123456789ABCDEF0123456789abcdef",  // uppercase hex is not a lab id
		"lab-run:run_0123456789abcdef0123456789abcde",   // 31 hex
		"lab-run:run_0123456789abcdef0123456789abcdef0", // 33 hex
		"lab-run:repo_0123456789abcdef0123456789abcdef",
		"lab-run: run_0123456789abcdef0123456789abcdef",
		"lab-run:run_0123456789abcdef0123456789abcdef\n",
		"LAB-RUN:run_0123456789abcdef0123456789abcdef",
		"xlab-run:run_0123456789abcdef0123456789abcdef",
	} {
		if id, ok := RunIDFromLabel(label); ok {
			t.Errorf("RunIDFromLabel(%q) = %q, true; want not a lab run label", label, id)
		}
	}
}

func TestAddPublicKey(t *testing.T) {
	f := newFake(t)
	uid := f.seedUser("", testSlug, "Coding Lab", nil)
	c := f.client(t)
	authorized := testAuthorizedKey(t)
	fields := strings.Fields(authorized)

	key, err := c.AddPublicKey(context.Background(), uid, RunKeyLabel(testRunID), authorized)
	if err != nil {
		t.Fatalf("AddPublicKey: %v", err)
	}
	if key.ID == "" || key.Label != RunKeyLabel(testRunID) || key.OpenSSH != fields[0]+" "+fields[1] {
		t.Errorf("key = %+v", key)
	}
	w := f.writes()
	if len(w) != 1 || w[0].Method != http.MethodPost || w[0].Path != "/@warpgate/admin/api/users/"+uid+"/credentials/public-keys" {
		t.Fatalf("writes = %v", w)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(w[0].Body), &body); err != nil {
		t.Fatalf("body %q: %v", w[0].Body, err)
	}
	if want := map[string]string{"label": RunKeyLabel(testRunID), "openssh_public_key": authorized}; !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
}

// TestAddPublicKeyCanonicalizesOptions: authorized_keys options are dropped
// (upstream's parser rejects them), leaving the validated key.
func TestAddPublicKeyCanonicalizesOptions(t *testing.T) {
	s := newStub(t, answer(http.StatusCreated, `{"id":"k1","label":"l","date_added":null,"last_used":null,"openssh_public_key":"x"}`))
	c := newTestClient(t, s.URL)
	authorized := testAuthorizedKey(t)
	if _, err := c.AddPublicKey(context.Background(), "u1", "l", `no-pty,from="10.0.0.1" `+authorized+"\n"); err != nil {
		t.Fatalf("AddPublicKey: %v", err)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(s.only(t).Body), &body)
	if body["openssh_public_key"] != authorized {
		t.Errorf("sent key = %q, want %q", body["openssh_public_key"], authorized)
	}
}

// TestAddPublicKeyValidatesLocally: a malformed key is an opaque 500
// upstream; it must fail here, before any request.
func TestAddPublicKeyValidatesLocally(t *testing.T) {
	s := newStub(t, answer(http.StatusCreated, `{}`))
	c := newTestClient(t, s.URL)
	good := testAuthorizedKey(t)
	for _, tc := range []struct{ name, user, label, key string }{
		{"garbage key", "u1", "l", "ssh-ed25519 not-base64!!"},
		{"empty key", "u1", "l", ""},
		{"two keys", "u1", "l", good + "\n" + good},
		{"empty user", "", "l", good},
		{"empty label", "u1", " ", good},
		{"multi-line label", "u1", "a\nb", good},
	} {
		if _, err := c.AddPublicKey(context.Background(), tc.user, tc.label, tc.key); err == nil {
			t.Errorf("%s: AddPublicKey succeeded, want refusal", tc.name)
		}
	}
	if reqs := s.requests(); len(reqs) != 0 {
		t.Errorf("refused keys reached the server: %v", reqs)
	}
}

func TestAddPublicKeyUpstreamRefusals(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		s := newStub(t, answer(status, `"Cannot manage SSH keys for LDAP-linked users. Keys are synced from LDAP."`))
		_, err := newTestClient(t, s.URL).AddPublicKey(context.Background(), "u1", "l", testAuthorizedKey(t))
		if !isStatus(err, status) {
			t.Errorf("status %d: error = %v, want the *APIError", status, err)
		}
	}
}

func TestListPublicKeys(t *testing.T) {
	s := newStub(t, answer(http.StatusOK, `[`+
		`{"id":"k1","label":"lab-run:`+testRunID+`","date_added":"2026-09-29T10:00:00Z","last_used":null,"openssh_public_key":"ssh-ed25519 AAAA1"},`+
		`{"id":"k2","label":"laptop","date_added":null,"last_used":"2026-09-29T11:00:00Z","openssh_public_key":"ssh-rsa AAAA2"}]`))
	got, err := newTestClient(t, s.URL).ListPublicKeys(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ListPublicKeys: %v", err)
	}
	want := []PublicKey{
		{ID: "k1", Label: "lab-run:" + testRunID, OpenSSH: "ssh-ed25519 AAAA1"},
		{ID: "k2", Label: "laptop", OpenSSH: "ssh-rsa AAAA2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %#v, want %#v", got, want)
	}
	if r := s.only(t); r.Method != http.MethodGet || r.Path != "/@warpgate/admin/api/users/u1/credentials/public-keys" {
		t.Errorf("request = %v", r)
	}

	empty := newStub(t, answer(http.StatusOK, `[]`))
	if got, err := newTestClient(t, empty.URL).ListPublicKeys(context.Background(), "u1"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty list = %#v, %v; want an empty non-nil slice", got, err)
	}
}

func TestRemovePublicKeyIsIdempotent(t *testing.T) {
	f := newFake(t)
	uid := f.seedUser("", testSlug, "Coding Lab", nil)
	c := f.client(t)
	key, err := c.AddPublicKey(context.Background(), uid, "l", testAuthorizedKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RemovePublicKey(context.Background(), uid, key.ID); err != nil {
		t.Fatalf("RemovePublicKey: %v", err)
	}
	if err := c.RemovePublicKey(context.Background(), uid, key.ID); err != nil {
		t.Fatalf("repeat RemovePublicKey (upstream 404): %v", err)
	}
	if keys, _ := c.ListPublicKeys(context.Background(), uid); len(keys) != 0 {
		t.Errorf("keys after removal = %+v", keys)
	}
	f.forced["DELETE "+rKey] = http.StatusForbidden
	if err := c.RemovePublicKey(context.Background(), uid, "k"); !isStatus(err, http.StatusForbidden) {
		t.Errorf("remove against a 403: error = %v, want it surfaced", err)
	}
	if err := c.RemovePublicKey(context.Background(), uid, ""); err == nil {
		t.Error("RemovePublicKey with an empty key id succeeded")
	}
}
