package vault

import (
	"strings"
	"testing"
)

func TestKindHelpers(t *testing.T) {
	if got := Kinds(); len(got) != 3 || got[0] != KindSSHKey || got[1] != KindHTTPSToken || got[2] != KindForgeToken {
		t.Errorf("Kinds() = %v", got)
	}
	for _, kind := range Kinds() {
		if !ValidKind(kind) {
			t.Errorf("ValidKind(%q) = false", kind)
		}
	}
	for _, kind := range []string{"", "ssh", "SSH_KEY", "password", "forge"} {
		if ValidKind(kind) {
			t.Errorf("ValidKind(%q) = true", kind)
		}
	}

	// The credential/forge split (design §3a): git credential is
	// ssh_key|https_token only; forge credential is forge_token only.
	for kind, wantGit := range map[string]bool{
		KindSSHKey:     true,
		KindHTTPSToken: true,
		KindForgeToken: false,
	} {
		if IsGitKind(kind) != wantGit {
			t.Errorf("IsGitKind(%q) = %v, want %v", kind, !wantGit, wantGit)
		}
		if IsForgeKind(kind) != !wantGit {
			t.Errorf("IsForgeKind(%q) = %v, want %v", kind, wantGit, !wantGit)
		}
	}
}

func TestValidateKindPayload(t *testing.T) {
	const secret = "SECRET-PAYLOAD-VALUE-9c1f"
	for _, tc := range []struct {
		name    string
		kind    string
		payload string
		wantErr string // substring; "" means valid
	}{
		{"ssh_key valid", KindSSHKey, `{"private_key":"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END-----"}`, ""},
		{"ssh_key with passphrase", KindSSHKey, `{"private_key":"k","passphrase":"p"}`, ""},
		{"ssh_key missing private_key", KindSSHKey, `{"passphrase":"p"}`, "missing private_key"},
		{"ssh_key empty private_key", KindSSHKey, `{"private_key":""}`, "missing private_key"},
		{"ssh_key unknown field", KindSSHKey, `{"private_key":"k","public_key":"` + secret + `"}`, "expected fields"},
		{"ssh_key not an object", KindSSHKey, `"` + secret + `"`, "expected fields"},
		{"ssh_key malformed json", KindSSHKey, `{`, "expected fields"},
		{"ssh_key trailing data", KindSSHKey, `{"private_key":"k"}{"x":1}`, "trailing data"},
		{"ssh_key null", KindSSHKey, `null`, "missing private_key"},

		{"https_token valid", KindHTTPSToken, `{"username":"lab","token":"t"}`, ""},
		{"https_token missing username", KindHTTPSToken, `{"token":"t"}`, "missing username"},
		{"https_token missing token", KindHTTPSToken, `{"username":"lab"}`, "missing token"},
		{"https_token unknown field", KindHTTPSToken, `{"username":"u","token":"t","host":"h"}`, "expected fields"},

		{"forge_token valid", KindForgeToken, `{"host":"git.cloonar.com","token":"t"}`, ""},
		{"forge_token missing host", KindForgeToken, `{"token":"t"}`, "missing host"},
		{"forge_token missing token", KindForgeToken, `{"host":"h"}`, "missing token"},

		{"unknown kind", "api_key", `{"token":"t"}`, `unknown credential kind "api_key"`},
		{"empty kind", "", `{}`, "unknown credential kind"},

		// Line-based transports (askpass stdout, HTTP headers) silently
		// truncate at CR/LF, so multi-line values must 400 at create time
		// instead of failing as a confusing remote 401 at op time. NUL is
		// equally fatal. The multi-line private_key is of course still fine.
		{"https_token newline in token", KindHTTPSToken, `{"username":"u","token":"` + secret + `\nline2"}`, "token must be a single line"},
		{"https_token trailing newline in token", KindHTTPSToken, `{"username":"u","token":"t\n"}`, "token must be a single line"},
		{"https_token CR in token", KindHTTPSToken, `{"username":"u","token":"t\rx"}`, "token must be a single line"},
		{"https_token NUL in token", KindHTTPSToken, `{"username":"u","token":"t\u0000x"}`, "token must be a single line"},
		{"https_token newline in username", KindHTTPSToken, `{"username":"u\nser","token":"t"}`, "username must be a single line"},
		{"forge_token newline in host", KindForgeToken, `{"host":"h\nost","token":"t"}`, "host must be a single line"},
		{"forge_token CRLF in token", KindForgeToken, `{"host":"h","token":"` + secret + `\r\nx"}`, "token must be a single line"},
		{"forge_token NUL in token", KindForgeToken, `{"host":"h","token":"t\u0000"}`, "token must be a single line"},
		{"ssh_key newline in passphrase", KindSSHKey, `{"private_key":"k","passphrase":"p\np"}`, "passphrase must be a single line"},
		{"ssh_key multiline private_key still valid", KindSSHKey, `{"private_key":"a\r\nb\nc"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKindPayload(tc.kind, []byte(tc.payload))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q contains payload bytes", err)
			}
		})
	}
}

// Validation errors must never echo payload bytes — not even when the
// secret shows up as a field NAME (a confused client swapping keys and
// values must not get its token reflected back).
func TestValidateKindPayloadNeverEchoesSecrets(t *testing.T) {
	const secret = "lab_pat_SECRETSECRETSECRET"
	payloads := []string{
		`{"` + secret + `":"x"}`,
		`{"username":"u","token":"t","extra":"` + secret + `"}`,
		`"` + secret + `"`,
		`{"username":` + `"` + secret + "\"", // malformed: unclosed object
	}
	for _, p := range payloads {
		for _, kind := range Kinds() {
			if err := ValidateKindPayload(kind, []byte(p)); err != nil {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("kind %s payload %q: error %q echoes payload bytes", kind, p, err)
				}
			}
		}
	}
}

func TestEncryptDecryptPayloadRoundtrip(t *testing.T) {
	v := testVault(t)

	sshIn := SSHKeyPayload{PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n", Passphrase: "pp"}
	blob, err := v.EncryptPayload(sshIn)
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	var sshOut SSHKeyPayload
	if err := v.DecryptPayload(blob, &sshOut); err != nil {
		t.Fatalf("DecryptPayload: %v", err)
	}
	if sshOut != sshIn {
		t.Errorf("ssh roundtrip: got %+v", sshOut)
	}

	httpsIn := HTTPSTokenPayload{Username: "lab", Token: "tok"}
	blob, err = v.EncryptPayload(httpsIn)
	if err != nil {
		t.Fatal(err)
	}
	var httpsOut HTTPSTokenPayload
	if err := v.DecryptPayload(blob, &httpsOut); err != nil {
		t.Fatal(err)
	}
	if httpsOut != httpsIn {
		t.Errorf("https roundtrip: got %+v", httpsOut)
	}

	forgeIn := ForgeTokenPayload{Host: "git.cloonar.com", Token: "forge-tok"}
	blob, err = v.EncryptPayload(forgeIn)
	if err != nil {
		t.Fatal(err)
	}
	var forgeOut ForgeTokenPayload
	if err := v.DecryptPayload(blob, &forgeOut); err != nil {
		t.Fatal(err)
	}
	if forgeOut != forgeIn {
		t.Errorf("forge roundtrip: got %+v", forgeOut)
	}
}

func TestDecryptPayloadTamperedBlob(t *testing.T) {
	v := testVault(t)
	blob, err := v.EncryptPayload(HTTPSTokenPayload{Username: "u", Token: "SECRET-TOKEN-77"})
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)/2] ^= 0x01
	var out HTTPSTokenPayload
	err = v.DecryptPayload(blob, &out)
	if err == nil {
		t.Fatal("DecryptPayload accepted a tampered blob")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN-77") {
		t.Errorf("error %q contains payload bytes", err)
	}
}

func TestEncryptPayloadRawJSON(t *testing.T) {
	// The API validates raw JSON with ValidateKindPayload, then can seal
	// it as-is via json.RawMessage.
	v := testVault(t)
	raw := []byte(`{"username":"lab","token":"t"}`)
	if err := ValidateKindPayload(KindHTTPSToken, raw); err != nil {
		t.Fatal(err)
	}
	blob, err := v.EncryptPayload(jsonRaw(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out HTTPSTokenPayload
	if err := v.DecryptPayload(blob, &out); err != nil {
		t.Fatal(err)
	}
	if out.Username != "lab" || out.Token != "t" {
		t.Errorf("raw JSON roundtrip: got %+v", out)
	}
}

// jsonRaw avoids importing encoding/json in the test just for a cast.
type jsonRaw []byte

func (r jsonRaw) MarshalJSON() ([]byte, error) { return r, nil }
