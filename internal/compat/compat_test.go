package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
)

// Registry-file coupling (compat.md §2): the parser must extract exactly
// the four fields lab reads from a real 2.1.198-shaped registry file and
// ignore every observed extra.
func TestCompat_RegistryFixture_parses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "registry-2.1.198.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e claudecode.RegistryEntry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("registry fixture does not parse: %v", err)
	}
	if e.PID != 1236522 {
		t.Errorf("PID = %d; want 1236522", e.PID)
	}
	if e.Cwd != "/home/dominik/projects/cloonar/coding-lab" {
		t.Errorf("Cwd = %q; want the fixture's absolute path", e.Cwd)
	}
	if e.StartedAt != 1783279526950 {
		t.Errorf("StartedAt = %d; want unix-millis 1783279526950", e.StartedAt)
	}
	if e.BridgeSessionID != "session_01WcC4ywH8xh8jeCA2NpzMDa" {
		t.Errorf("BridgeSessionID = %q; want the session_-prefixed id", e.BridgeSessionID)
	}
	// 2.1.198 stores the URL-ready session_ form; the URL builder must
	// pass it through unchanged.
	if got, want := claudecode.BridgeURL(e.BridgeSessionID), "https://claude.ai/code/session_01WcC4ywH8xh8jeCA2NpzMDa"; got != want {
		t.Errorf("BridgeURL = %q; want %q", got, want)
	}
}

// cse_ → session_ normalization (claude's toCompatSessionId), kept for
// tolerance of the transcript spelling.
func TestCompat_BridgeURLNormalization(t *testing.T) {
	if got, want := claudecode.BridgeURL("cse_abc123"), "https://claude.ai/code/session_abc123"; got != want {
		t.Errorf("BridgeURL(cse_) = %q; want %q", got, want)
	}
	if got, want := claudecode.BridgeURL("session_abc123"), "https://claude.ai/code/session_abc123"; got != want {
		t.Errorf("BridgeURL(session_) = %q; want %q", got, want)
	}
}

// Auth-status coupling (compat.md §3): the 2.1.198 stdout shape must
// yield logged_in/email/method; the observed extra keys are ignored.
func TestCompat_AuthStatusFixture_parses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "auth-status-2.1.198.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := claudecode.ParseAuthStatus(b)
	if err != nil {
		t.Fatalf("auth-status fixture does not parse: %v", err)
	}
	if !st.LoggedIn {
		t.Error("LoggedIn = false; want true")
	}
	if st.Email != "operator@example.com" {
		t.Errorf("Email = %q; want operator@example.com", st.Email)
	}
	if st.Method != "claude.ai" {
		t.Errorf("Method = %q; want claude.ai", st.Method)
	}
}

// Spawn argv snapshot (compat.md §1, pinned M3 constant). Manual spawn: no
// seed prompt, so no trailing positional.
func TestCompat_SpawnArgvSnapshot(t *testing.T) {
	got := strings.Join(claudecode.SpawnArgv("claude", "repo~dom-20260706-0910", "opus[1m]", "max", ""), " ")
	want := "claude --remote-control repo~dom-20260706-0910 --permission-mode auto --model opus[1m] --effort max"
	if got != want {
		t.Errorf("spawn argv drifted:\n got  %q\n want %q", got, want)
	}
}

// AFK spawn argv: the seed prompt is the trailing positional AFTER the
// model/effort flags (pinned v0 mechanism, claude CLI `[options] [prompt]`) —
// carried at spawn, never injected post-spawn.
func TestCompat_SpawnArgvSeedPromptSnapshot(t *testing.T) {
	got := strings.Join(claudecode.SpawnArgv("claude", "repo~afk-7", "opus[1m]", "max", "resolve #7"), " ")
	want := "claude --remote-control repo~afk-7 --permission-mode auto --model opus[1m] --effort max resolve #7"
	if got != want {
		t.Errorf("seeded spawn argv drifted:\n got  %q\n want %q", got, want)
	}
}

// OAuth URL regex against the real captured authorize line (claude
// 2.1.150 — fixture provenance, see compat.md §3). The encoded
// redirect_uri embeds %2Foauth%2F, which must not fool the regex.
func TestCompat_OAuthURLRegex_realAuthorizeLine(t *testing.T) {
	const realLine = "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=3paXVTV6FPSlOHej-t8CPB3Azy7demaQ-wTe13tPw6E&code_challenge_method=S256&state=84xH4rqCmCJzD1NFDMGF8YMkafUHjFOIhzfLXzd8ERQ"
	const realURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=3paXVTV6FPSlOHej-t8CPB3Azy7demaQ-wTe13tPw6E&code_challenge_method=S256&state=84xH4rqCmCJzD1NFDMGF8YMkafUHjFOIhzfLXzd8ERQ"
	if got := claudecode.ExtractOAuthURL(realLine); got != realURL {
		t.Errorf("ExtractOAuthURL(real 2.1.150 line) = %q; want the full authorize URL", got)
	}
}

// Trust-key round-trip (compat.md §4): the exact key strings claude
// reads, in the exact files it reads them from — plus the shipped-bug
// regression guard (MCP approval must NOT land in the global config).
func TestCompat_TrustKeys_roundtrip(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()
	if err := claudecode.SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}

	global, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(global), `"hasTrustDialogAccepted": true`) {
		t.Errorf("global config lacks the hasTrustDialogAccepted grant: %s", global)
	}
	if strings.Contains(string(global), "enableAllProjectMcpServers") {
		t.Errorf("MCP approval leaked into the global config; claude ignores it there: %s", global)
	}

	var parsed map[string]any
	if err := json.Unmarshal(global, &parsed); err != nil {
		t.Fatalf("global config no longer valid JSON: %v", err)
	}
	entry, _ := parsed["projects"].(map[string]any)[dir].(map[string]any)
	if v, _ := entry["hasTrustDialogAccepted"].(bool); !v {
		t.Errorf("projects.%s.hasTrustDialogAccepted != true", dir)
	}

	local, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local), `"enableAllProjectMcpServers": true`) {
		t.Errorf("worktree settings lack the enableAllProjectMcpServers grant: %s", local)
	}
}

// Attribution-key pin (compat.md §4, M7 incogni measure 1): the exact key
// strings the 2.1.198 settings schema reads, written to the exact file
// claude reads them from. attribution{commit:"",pr:"",sessionUrl:false} is
// the current mechanism; includeCoAuthoredBy:false is the deprecated-but-
// honored fallback seeded alongside it.
func TestCompat_AttributionKeys_seed(t *testing.T) {
	dir := t.TempDir()
	if err := claudecode.SeedAttributionOff(dir); err != nil {
		t.Fatalf("SeedAttributionOff: %v", err)
	}

	local, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"includeCoAuthoredBy": false`,
		`"commit": ""`,
		`"pr": ""`,
		`"sessionUrl": false`,
	} {
		if !strings.Contains(string(local), want) {
			t.Errorf("worktree settings lack %s:\n%s", want, local)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(local, &parsed); err != nil {
		t.Fatalf("settings.local.json not valid JSON: %v", err)
	}
	if _, ok := parsed["attribution"].(map[string]any); !ok {
		t.Errorf("attribution is not a JSON object: %s", local)
	}
}

// Registry sessionId (compat.md §5): the transcript filename stem lab reads
// from the same registry file the deep link comes from.
func TestCompat_RegistryFixture_hasSessionID(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "registry-2.1.198.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e claudecode.RegistryEntry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("registry fixture does not parse: %v", err)
	}
	if e.SessionID != "9a1b2c3d-4e5f-4a6b-8c7d-0e1f2a3b4c5d" {
		t.Errorf("SessionID = %q; want the transcript-filename stem", e.SessionID)
	}
}

// Transcript project-dir slug (compat.md §5): every non-alphanumeric byte of
// the absolute cwd becomes '-', so a '/.' boundary doubles. Pinned against the
// dirs claude actually created under ~/.claude/projects on 2.1.198.
func TestCompat_SlugForDir(t *testing.T) {
	cases := map[string]string{
		"/home/dominik/projects/cloonar/coding-lab":                                      "-home-dominik-projects-cloonar-coding-lab",
		"/home/dominik/.local/state/lab/worktrees/nixos-test-20260706-2315":              "-home-dominik--local-state-lab-worktrees-nixos-test-20260706-2315",
		"/home/dominik/projects/cloonar/coding-lab/.claude/worktrees/feat-embedded-chat": "-home-dominik-projects-cloonar-coding-lab--claude-worktrees-feat-embedded-chat",
	}
	for dir, want := range cases {
		if got := claudecode.SlugForDir(dir); got != want {
			t.Errorf("SlugForDir(%q) = %q; want %q", dir, got, want)
		}
	}
}

// Transcript JSONL → universal schema (compat.md §5): the field names and
// shapes lab folds a real 2.1.198-shaped transcript through. Drives the parser
// from a captured fixture covering every mapped event: bridge lifecycle, user
// text, hidden thinking, assistant text, tool chips with ok/error results, an
// isMeta skip, a surfaced API error, and a pending dialog.
func TestCompat_TranscriptFixture_maps(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "transcript-2.1.198.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	chat, err := claudecode.ParseTranscript(f)
	if err != nil {
		t.Fatalf("ParseTranscript: %v", err)
	}

	if chat.Cursor != 9 || len(chat.Messages) != 9 {
		t.Fatalf("mapped %d messages (cursor %d); want 9 (the isMeta line is dropped)", len(chat.Messages), chat.Cursor)
	}
	if chat.State != provider.StateQuestion {
		t.Errorf("State = %q; want %q (tail is a pending dialog)", chat.State, provider.StateQuestion)
	}

	m := chat.Messages
	if m[0].Kind != provider.MessageLifecycle || m[0].Error {
		t.Errorf("msg0 = %+v; want a non-error lifecycle (bridge status)", m[0])
	}
	if m[1].Kind != provider.MessageText || m[1].Role != "user" {
		t.Errorf("msg1 = %+v; want user text", m[1])
	}
	if m[2].Kind != provider.MessageText || !m[2].Thinking {
		t.Errorf("msg2 = %+v; want assistant thinking (hidden)", m[2])
	}
	if m[4].Kind != provider.MessageTool || m[4].Tool == nil || m[4].Tool.Status != "ok" {
		t.Errorf("msg4 = %+v; want an ok tool chip (result landed)", m[4])
	}
	if got := m[4].Tool.Title; got != "Ran labctl issue create --title \"Test issue\" --labels needs-triage" {
		t.Errorf("tool title = %q; want the Bash first-line chip", got)
	}
	if m[6].Kind != provider.MessageTool || m[6].Tool == nil || m[6].Tool.Status != "error" {
		t.Errorf("msg6 = %+v; want an error tool chip (is_error result)", m[6])
	}
	if m[7].Kind != provider.MessageLifecycle || !m[7].Error {
		t.Errorf("msg7 = %+v; want a surfaced API error", m[7])
	}
	d := m[8]
	if d.Kind != provider.MessageDialog || d.Dialog == nil {
		t.Fatalf("msg8 = %+v; want a dialog", d)
	}
	if !d.Dialog.Answerable || d.Dialog.Multi || len(d.Dialog.Options) != 3 {
		t.Errorf("dialog = %+v; want an answerable single-select with 2 options + Other", d.Dialog)
	}
	if !d.Dialog.Options[2].IsOther {
		t.Errorf("dialog last option = %+v; want the synthesized Other row", d.Dialog.Options[2])
	}
}

// Dialog answer keystrokes (compat.md §7): the send-keys recipe for a picker.
// Normalise to the top (Up × rows−1), navigate down to the choice, Enter;
// multi-select toggles with Space; Other selects then types.
func TestCompat_DialogKeystrokes(t *testing.T) {
	single := provider.Dialog{Answerable: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true},
	}}
	got, err := claudecode.DialogKeystrokes(single, provider.DialogAnswer{Index: 1})
	if err != nil {
		t.Fatalf("single-select: %v", err)
	}
	want := []claudecode.KeyOp{{Named: []string{"Up", "Up"}}, {Named: []string{"Down"}}, {Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("single-select recipe = %v; want %v", got, want)
	}

	multi := provider.Dialog{Answerable: true, Multi: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "c"},
	}}
	got, _ = claudecode.DialogKeystrokes(multi, provider.DialogAnswer{Selected: []int{0, 2}})
	want = []claudecode.KeyOp{{Named: []string{"Up", "Up"}}, {Named: []string{"Space"}},
		{Named: []string{"Down", "Down"}}, {Named: []string{"Space"}}, {Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-select recipe = %v; want %v", got, want)
	}

	got, _ = claudecode.DialogKeystrokes(single, provider.DialogAnswer{Index: 2, OtherText: "custom"})
	want = []claudecode.KeyOp{{Named: []string{"Up", "Up"}}, {Named: []string{"Down", "Down"}},
		{Named: []string{"Enter"}}, {Text: "custom"}, {Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Other recipe = %v; want %v", got, want)
	}
}

// Live re-verification hook: parses the installed claude's actual status
// output. Opt-in via LAB_COMPAT_LIVE=1 so CI stays hermetic.
func TestCompat_Live_authStatusParses(t *testing.T) {
	if os.Getenv("LAB_COMPAT_LIVE") != "1" {
		t.Skip("set LAB_COMPAT_LIVE=1 to probe the installed claude binary")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}
	out, _ := exec.Command(bin, "auth", "status", "--json").Output() // exit code deliberately ignored
	st, err := claudecode.ParseAuthStatus(out)
	if err != nil {
		t.Fatalf("live `claude auth status --json` stdout does not parse: %v\nstdout: %s", err, out)
	}
	t.Logf("live status: logged_in=%v method=%q email=%q", st.LoggedIn, st.Method, st.Email)
}
