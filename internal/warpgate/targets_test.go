package warpgate

// Targets: SSH-only filtering, deterministic order, idempotent assignment —
// and the hygiene rule that a target's credentials never reach a lab value or
// error, which the fixtures below make observable by planting fake secrets.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// fakeSecret marks every credential planted in a target fixture. If it shows
// up in any returned value or error string, a target secret leaked.
const fakeSecret = "FAKE-TARGET-SECRET"

// sshTarget renders a Warpgate 0.29.1 SSH target as GET /targets answers it
// (config/target.rs: Target, TargetSSHOptions, SSHTargetAuth) with a
// PASSWORD auth block carrying password.
func sshTarget(id, name, password string) string {
	if password == "" {
		password = fakeSecret + "-" + id
	}
	return fmt.Sprintf(`{"id":%q,"name":%q,"description":"target %s","allow_roles":[],`+
		`"options":{"kind":"Ssh","host":"10.0.0.9","port":22,"username":"deploy","allow_insecure_algos":false,`+
		`"auth":{"kind":"Password","password":%q},"jump_host":null},`+
		`"rate_limit_bytes_per_second":null,"group_id":null,"ticket_max_duration_seconds":null,`+
		`"ticket_requests_disabled":false,"ticket_require_approval":false,"require_approval":false,"ticket_max_uses":null}`,
		id, name, name, password)
}

// targetFixture mixes SSH targets (password and public-key auth, a duplicate
// name to exercise the id tiebreak) with other kinds that carry their own
// secrets and must be dropped.
func targetFixture() []string {
	return []string{
		sshTarget("t-2", "staging", ""),
		`{"id":"t-1","name":"prod","description":"","allow_roles":[],"options":{"kind":"Ssh","host":"10.0.0.1","port":2200,"username":"root","allow_insecure_algos":false,"auth":{"kind":"PublicKey"},"jump_host":null},"rate_limit_bytes_per_second":null,"group_id":null,"ticket_max_duration_seconds":null,"ticket_requests_disabled":false,"ticket_require_approval":false,"require_approval":false,"ticket_max_uses":null}`,
		`{"id":"t-9","name":"web","description":"","allow_roles":[],"options":{"kind":"Http","url":"https://intranet","tls":{"mode":"Preferred","verify":true},"headers":{"Authorization":"Bearer ` + fakeSecret + `-http"},"external_host":null},"rate_limit_bytes_per_second":null,"group_id":null,"ticket_max_duration_seconds":null,"ticket_requests_disabled":false,"ticket_require_approval":false,"require_approval":false,"ticket_max_uses":null}`,
		`{"id":"t-8","name":"db","description":"","allow_roles":[],"options":{"kind":"Postgres","host":"db","port":5432,"username":"app","auth":{"kind":"Password","password":"` + fakeSecret + `-pg"}},"rate_limit_bytes_per_second":null,"group_id":null,"ticket_max_duration_seconds":null,"ticket_requests_disabled":false,"ticket_require_approval":false,"require_approval":false,"ticket_max_uses":null}`,
		`{"id":"t-7","name":"k8s","description":"","allow_roles":[],"options":{"kind":"Kubernetes","cluster_url":"https://k8s","auth":{"kind":"Token","token":"` + fakeSecret + `-k8s"}},"rate_limit_bytes_per_second":null,"group_id":null,"ticket_max_duration_seconds":null,"ticket_requests_disabled":false,"ticket_require_approval":false,"require_approval":false,"ticket_max_uses":null}`,
		sshTarget("t-0", "prod", ""),
		// Serde's lowercase config spelling is NOT the API's discriminator
		// (wire.go point 7); a row carrying it is not an SSH target to lab.
		`{"id":"t-5","name":"lowercase","description":"","options":{"kind":"ssh","auth":{"kind":"Password","password":"` + fakeSecret + `-lc"}}}`,
	}
}

func TestListSSHTargetsFiltersSortsAndCarriesNoSecret(t *testing.T) {
	f := newFake(t)
	f.targets = targetFixture()
	got, err := f.client(t).ListSSHTargets(context.Background())
	if err != nil {
		t.Fatalf("ListSSHTargets: %v", err)
	}
	want := []Target{
		{ID: "t-0", Name: "prod", Description: "target prod"},
		{ID: "t-1", Name: "prod", Description: ""},
		{ID: "t-2", Name: "staging", Description: "target staging"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %#v\nwant %#v", got, want)
	}
	for _, rendered := range []string{fmt.Sprintf("%#v", got), fmt.Sprintf("%+v", got)} {
		if strings.Contains(rendered, fakeSecret) {
			t.Errorf("returned targets carry a target secret: %s", rendered)
		}
	}
}

func TestRoleSSHTargets(t *testing.T) {
	f := newFake(t)
	f.targets = targetFixture()
	rid := f.seedRole("", testSlug, "Coding Lab", false)
	f.targetRoles["t-2/"+rid] = true
	f.targetRoles["t-9/"+rid] = true // an HTTP target on the role: dropped
	c := f.client(t)

	got, err := c.RoleSSHTargets(context.Background(), rid)
	if err != nil {
		t.Fatalf("RoleSSHTargets: %v", err)
	}
	if want := []Target{{ID: "t-2", Name: "staging", Description: "target staging"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %#v, want %#v", got, want)
	}
	reqs := f.requests()
	if last := reqs[len(reqs)-1]; last.Method != http.MethodGet || last.Path != "/@warpgate/admin/api/role/"+rid+"/targets" {
		t.Errorf("request = %v, want GET the SINGULAR /role/{id}/targets", last)
	}

	if _, err := c.RoleSSHTargets(context.Background(), "no-such-role"); !isStatus(err, http.StatusNotFound) {
		t.Errorf("unknown role: error = %v, want a 404 *APIError", err)
	}
	if _, err := c.RoleSSHTargets(context.Background(), ""); err == nil {
		t.Error("RoleSSHTargets with an empty role id succeeded")
	}
}

// TestTargetErrorsNeverCarryTargetBodies: a targets body is never folded into
// an error — not on a non-2xx, and not when it fails to decode — even though
// every other endpoint folds a capped snippet.
func TestTargetErrorsNeverCarryTargetBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"non-2xx echoing a target", http.StatusInternalServerError, "[" + sshTarget("t-1", "prod", "") + "]"},
		{"syntax error inside a secret", http.StatusOK, `[{"id":"t-1","name":"prod","options":{"kind":"Ssh","auth":{"password":"` + fakeSecret + `\x01"}}}]`},
		{"truncated body", http.StatusOK, `[{"id":"t-1","options":{"kind":"Ssh","auth":{"password":"` + fakeSecret},
		{"type error on a secret-looking number", http.StatusOK, `[{"id":"t-1","name":424242424242,"options":{"kind":"Ssh"}}]`},
		{"not an array", http.StatusOK, `{"password":"` + fakeSecret + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, answer(tc.status, tc.body))
			c := newTestClient(t, s.URL)
			for _, call := range []func() error{
				func() error { _, err := c.ListSSHTargets(context.Background()); return err },
				func() error { _, err := c.RoleSSHTargets(context.Background(), "r-1"); return err },
			} {
				err := call()
				if err == nil {
					t.Fatal("call succeeded, want an error")
				}
				for _, leak := range []string{fakeSecret, "424242424242", "prod"} {
					if strings.Contains(err.Error(), leak) {
						t.Errorf("error %q carries %q from the targets body", err, leak)
					}
				}
			}
		})
	}
}

// TestWireTargetDeclaresNoCredentialFields pins the structural half of the
// hygiene rule: wireTarget must declare nothing beyond id, name, description
// and options.kind, so the decoder never materializes a credential. Adding a
// field here is a decision to review against SECRET_PATHS, not an accident.
func TestWireTargetDeclaresNoCredentialFields(t *testing.T) {
	var tags []string
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, prefix string) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			name := prefix + strings.Split(f.Tag.Get("json"), ",")[0]
			tags = append(tags, name)
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, name+".")
			}
		}
	}
	walk(reflect.TypeFor[wireTarget](), "")
	want := []string{"id", "name", "description", "options", "options.kind"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("wireTarget JSON fields = %v, want exactly %v", tags, want)
	}
	if n := reflect.TypeFor[Target]().NumField(); n != 3 {
		t.Errorf("Target has %d fields, want 3 (ID, Name, Description)", n)
	}
}

func TestAssignTargetRoleIsIdempotent(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	ctx := context.Background()
	if err := c.AssignTargetRole(ctx, "t-1", "r-1"); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	if err := c.AssignTargetRole(ctx, "t-1", "r-1"); err != nil {
		t.Fatalf("repeat assign (upstream 409): %v", err)
	}
	for _, r := range f.writes() {
		if r.Method != http.MethodPost || r.Path != "/@warpgate/admin/api/targets/t-1/roles/r-1" || r.Body != "" {
			t.Errorf("assign request = %v body %q, want a bodyless POST /targets/t-1/roles/r-1", r, r.Body)
		}
	}
	if !f.targetRoles["t-1/r-1"] {
		t.Error("assignment not recorded")
	}

	f.forced["POST "+rTargetRole] = http.StatusInternalServerError
	if err := c.AssignTargetRole(ctx, "t-2", "r-1"); !isStatus(err, http.StatusInternalServerError) {
		t.Errorf("assign against a 500: error = %v, want it surfaced", err)
	}
	if err := c.AssignTargetRole(ctx, "", "r-1"); err == nil {
		t.Error("assign with an empty target id succeeded")
	}
}

func TestUnassignTargetRoleIsIdempotent(t *testing.T) {
	f := newFake(t)
	f.targetRoles["t-1/r-1"] = true
	c := f.client(t)
	ctx := context.Background()
	if err := c.UnassignTargetRole(ctx, "t-1", "r-1"); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if err := c.UnassignTargetRole(ctx, "t-1", "r-1"); err != nil {
		t.Fatalf("repeat unassign (upstream 404): %v", err)
	}
	if f.targetRoles["t-1/r-1"] {
		t.Error("assignment still recorded")
	}
	for _, r := range f.writes() {
		if r.Method != http.MethodDelete || r.Path != "/@warpgate/admin/api/targets/t-1/roles/r-1" {
			t.Errorf("unassign request = %v", r)
		}
	}
	f.forced["DELETE "+rTargetRole] = http.StatusForbidden
	if err := c.UnassignTargetRole(ctx, "t-1", "r-1"); !isStatus(err, http.StatusForbidden) {
		t.Errorf("unassign against a 403: error = %v, want it surfaced", err)
	}
	if err := c.UnassignTargetRole(ctx, "t-1", ""); err == nil {
		t.Error("unassign with an empty role id succeeded")
	}
}

// TestTargetFixtureIsUpstreamShaped guards the fixture itself: every row is
// valid JSON with the discriminator where upstream puts it.
func TestTargetFixtureIsUpstreamShaped(t *testing.T) {
	for _, raw := range targetFixture() {
		var row struct {
			Options map[string]any `json:"options"`
		}
		if err := json.Unmarshal([]byte(raw), &row); err != nil || row.Options["kind"] == nil {
			t.Errorf("fixture row %s: %v", raw, err)
		}
	}
}
