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
	got := strings.Join(claudecode.SpawnArgv("claude", provider.SpawnSpec{
		SessionName: "repo~dom-20260706-0910", Model: "opus[1m]", Effort: "max",
	}), " ")
	want := "claude --remote-control repo~dom-20260706-0910 --permission-mode auto --model opus[1m] --effort max"
	if got != want {
		t.Errorf("spawn argv drifted:\n got  %q\n want %q", got, want)
	}
}

// AFK spawn argv: the seed prompt is the trailing positional AFTER the
// model/effort flags (pinned v0 mechanism, claude CLI `[options] [prompt]`) —
// carried at spawn, never injected post-spawn.
func TestCompat_SpawnArgvSeedPromptSnapshot(t *testing.T) {
	got := strings.Join(claudecode.SpawnArgv("claude", provider.SpawnSpec{
		SessionName: "repo~afk-7", Model: "opus[1m]", Effort: "max", InitialPrompt: "resolve #7",
	}), " ")
	want := "claude --remote-control repo~afk-7 --permission-mode auto --model opus[1m] --effort max resolve #7"
	if got != want {
		t.Errorf("seeded spawn argv drifted:\n got  %q\n want %q", got, want)
	}
}

// ultracode spawn argv (issue #19 / ADR-0021): the provider-owned directive is
// prepended to the non-empty seed prompt, kept as ONE trailing positional. The
// wording is lab's own (compat.md §1) — NOT a pinned Claude coupling — so this
// snapshot guards the argv builder, not a claude version. A manual spawn's empty
// prompt makes it a natural no-op (covered by TestSpawnArgv).
func TestCompat_SpawnArgvUltracodeSnapshot(t *testing.T) {
	argv := claudecode.SpawnArgv("claude", provider.SpawnSpec{
		SessionName: "repo~afk-7", Model: "opus[1m]", Effort: "max",
		Options: map[string]string{"ultracode": "true"}, InitialPrompt: "resolve #7",
	})
	// The directive rides as the single trailing positional (never split).
	if last := argv[len(argv)-1]; !strings.HasSuffix(last, "\n\nresolve #7") || !strings.Contains(last, "ultracode mode") {
		t.Errorf("ultracode prompt not prepended as one trailing positional: %q", last)
	}
	// The flags ahead of the prompt are unchanged.
	head := strings.Join(argv[:len(argv)-1], " ")
	wantHead := "claude --remote-control repo~afk-7 --permission-mode auto --model opus[1m] --effort max"
	if head != wantHead {
		t.Errorf("ultracode spawn flags drifted:\n got  %q\n want %q", head, wantHead)
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

// Onboarding-key pin (compat.md §4a): `claude auth login` does not complete
// first-run onboarding, so SeedTrust must set the exact top-level key claude's
// wizard gates on, or the first --remote-control spawn on a fresh install
// blocks on the theme picker. Live-verified 2.1.198 (2026-07-07): a fresh HOME
// shows "Let's get started — choose the text style…" as its first screen, and
// seeding this single key drops the spawn through to the (separately seeded)
// trust dialog. theme is intentionally NOT seeded — it reads null even on a
// fully onboarded host, so hasCompletedOnboarding is the sole gate.
func TestCompat_OnboardingKey_seed(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	dir := t.TempDir()
	if err := claudecode.SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}

	global, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(global), `"hasCompletedOnboarding": true`) {
		t.Errorf("global config lacks the hasCompletedOnboarding grant: %s", global)
	}
	// theme must NOT be seeded — hasCompletedOnboarding is the sole gate.
	if strings.Contains(string(global), `"theme"`) {
		t.Errorf("theme was seeded; only hasCompletedOnboarding gates onboarding: %s", global)
	}

	var parsed map[string]any
	if err := json.Unmarshal(global, &parsed); err != nil {
		t.Fatalf("global config no longer valid JSON: %v", err)
	}
	if v, _ := parsed["hasCompletedOnboarding"].(bool); !v {
		t.Errorf("top-level hasCompletedOnboarding != true")
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
// isMeta skip, a local slash-command echo + its captured output (rendered as
// a user text + lifecycle pair since issue #51 decision 2), a surfaced API
// error, and a pending dialog.
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

	if chat.Cursor != 11 || len(chat.Messages) != 11 {
		t.Fatalf("mapped %d messages (cursor %d); want 11 (the isMeta line is dropped; the echo maps to a command line + output)", len(chat.Messages), chat.Cursor)
	}
	// The slash-command echo renders PARSED (issue #51 decision 2) — the raw
	// <command-…> / <local-command-stdout> tags never leak into the chat.
	for i, msg := range chat.Messages {
		if strings.Contains(msg.Text, "<command-name>") ||
			strings.Contains(msg.Text, "<command-message>") ||
			strings.Contains(msg.Text, "<local-command-stdout>") {
			t.Errorf("msg%d leaked a raw local-command tag: %q", i, msg.Text)
		}
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
	// The /help echo: a visible user text message with the command line, then
	// its captured output as a lifecycle message (issue #51 decision 2).
	if m[6].Kind != provider.MessageText || m[6].Role != "user" || m[6].Text != "/help" {
		t.Errorf("msg6 = %+v; want the echoed command line \"/help\" as user text", m[6])
	}
	if m[7].Kind != provider.MessageLifecycle || m[7].Error || m[7].Text != "Available commands: /clear, /compact, /help" {
		t.Errorf("msg7 = %+v; want the command stdout as a non-error lifecycle", m[7])
	}
	if m[8].Kind != provider.MessageTool || m[8].Tool == nil || m[8].Tool.Status != "error" {
		t.Errorf("msg8 = %+v; want an error tool chip (is_error result)", m[8])
	}
	if m[9].Kind != provider.MessageLifecycle || !m[9].Error {
		t.Errorf("msg9 = %+v; want a surfaced API error", m[9])
	}
	d := m[10]
	if d.Kind != provider.MessageDialog || d.Dialog == nil {
		t.Fatalf("msg10 = %+v; want a dialog", d)
	}
	if !d.Dialog.Answerable || d.Dialog.Multi || len(d.Dialog.Options) != 3 {
		t.Errorf("dialog = %+v; want an answerable single-select with 2 options + Other", d.Dialog)
	}
	if !d.Dialog.Options[2].IsOther {
		t.Errorf("dialog last option = %+v; want the synthesized Other row", d.Dialog.Options[2])
	}
}

// Command echoes are excluded from state derivation even though they render
// (issue #45's stuck-composer fix, kept by issue #51 decision 2): the fixture
// tail below a real assistant turn is only the echo + output, and the state
// must stay the assistant's — while a fresh post-/clear transcript holding
// ONLY the echo derives idle, never `working`.
func TestCompat_TranscriptEcho_stateNeutral(t *testing.T) {
	echo := `{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"<local-command-stdout>ok</local-command-stdout>"}}` + "\n"
	turns := `{"type":"user","message":{"role":"user","content":"go"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n"

	chat, err := claudecode.ParseTranscript(strings.NewReader(turns + echo))
	if err != nil {
		t.Fatal(err)
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("echo-tailed state = %q; want %q (pre-echo state kept)", chat.State, provider.StateNeedsInput)
	}
	chat, err = claudecode.ParseTranscript(strings.NewReader(echo))
	if err != nil {
		t.Fatal(err)
	}
	if chat.State != provider.StateIdle {
		t.Errorf("post-/clear echo-only state = %q; want %q (the issue-#45 stuck-composer pin)", chat.State, provider.StateIdle)
	}
}

// Hook payload → Dialog (compat.md §9): the live 2.1.198 PreToolUse payload
// (invisible in the transcript while pending — §5) maps through the SAME mapper
// as the transcript into an answerable single-question dialog. The fixture is
// the captured throwaway-session payload.
func TestCompat_HookPayload_maps(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "hook-pretooluse-2.1.198.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := claudecode.DialogFromHookPayload(b)
	if !ok {
		t.Fatal("DialogFromHookPayload: not recognised as a dialog")
	}
	if d.ToolID != "toolu_01KU2pbDQNUNFim79s9FKf6i" {
		t.Errorf("ToolID = %q; want the payload tool_use_id", d.ToolID)
	}
	if d.Kind != "question" || !d.Answerable || d.Multi {
		t.Errorf("dialog = %+v; want an answerable single-select question", d)
	}
	if d.Prompt != "Which flavor of test question do you prefer?" {
		t.Errorf("Prompt = %q; want the question text", d.Prompt)
	}
	// Three listed options (A/B/C) plus the synthesized Other row.
	if len(d.Options) != 4 || d.Options[0].Label != "Option A" || !d.Options[3].IsOther {
		t.Fatalf("options = %+v; want A/B/C + Other", d.Options)
	}
	if d.Options[1].Description != "The second test option." {
		t.Errorf("option B description = %q; want the payload description", d.Options[1].Description)
	}
}

// Dialog answer keystrokes (compat.md §7, live-verified 2026-07-08 and
// re-driven 2026-07-09 on 2.1.198): per-shape recipes over per-shape picker
// geometry. The Other and multi-select recipes pin the live-bug FIXES —
// type-first free text (Enter on the empty row declines the dialog), the
// Submit-row commit (Enter on an option row toggles instead of committing),
// and ONE NAMED KEY PER OP (the picker drops a burst of keys sent in one
// send-keys call: live 2026-07-09, a four-Down burst moved the cursor zero
// rows, so batched walks never reached the Submit row) — and the
// multi-question/plan/review snapshots pin the remaining shapes.
func TestCompat_DialogKeystrokes(t *testing.T) {
	single := provider.Dialog{Answerable: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "Other", IsOther: true},
	}}
	got, err := claudecode.DialogKeystrokes(single, provider.DialogAnswer{Index: 1})
	if err != nil {
		t.Fatalf("single-select: %v", err)
	}
	// No climb (LIVE 2026-07-08: Up wraps, and the picker opens on the top
	// row) — a pure downward walk: Down to the chosen row, Enter.
	want := []claudecode.KeyOp{{Named: []string{"Down"}}, {Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("single-select recipe = %v; want %v", got, want)
	}

	// Other/free-text (LIVE BUG FIX 2026-07-08): paste FIRST, then Enter — the
	// old [Enter][text][Enter] declined the whole dialog on the empty row.
	// Each Down is its own op (2026-07-09).
	got, _ = claudecode.DialogKeystrokes(single, provider.DialogAnswer{Index: 2, OtherText: "custom"})
	want = []claudecode.KeyOp{{Named: []string{"Down"}}, {Named: []string{"Down"}},
		{Text: "custom"}, {Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Other recipe = %v; want %v", got, want)
	}

	// Multi-select (LIVE BUG FIXES 2026-07-08 + 2026-07-09): commit via the
	// unnumbered Submit row (navigation index = row count, here 4), never a
	// bare Enter (Enter toggles); every Down its own op (a batched walk is
	// dropped and the recipe then commits nothing — the 2026-07-09 root cause).
	// From the top: toggle a (Space), Down, Down to c, toggle, Down×(4−2) onto
	// Submit, Enter. A single multiSelect question then ends on the review
	// screen ([Enter] — cursor defaults to "Submit answers").
	multi := provider.Dialog{Answerable: true, Multi: true, Options: []provider.DialogOption{
		{Label: "a"}, {Label: "b"}, {Label: "c"}, {Label: "Other", IsOther: true},
	}}
	got, _ = claudecode.DialogKeystrokes(multi, provider.DialogAnswer{Selected: []int{0, 2}})
	want = []claudecode.KeyOp{{Named: []string{"Space"}},
		{Named: []string{"Down"}}, {Named: []string{"Down"}}, {Named: []string{"Space"}},
		{Named: []string{"Down"}}, {Named: []string{"Down"}}, {Named: []string{"Enter"}}, // onto Submit, commit
		{Named: []string{"Enter"}}} // review: Submit answers (cursor already there)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-select recipe = %v; want %v", got, want)
	}

	// Multi-select free text (LIVE 2026-07-09): pasting onto the "Type
	// something" row FILLS it and CHECKS it — no Space (Space would type a
	// literal leading space into the field). The row is the appended LAST
	// option, so the walk stays downward: toggle b, Down to the Other row,
	// paste, Down onto Submit, Enter, review Enter. Recorded ground truth:
	// "Onions, no anchovies" — toggled labels plus the text as the last
	// segment.
	got, _ = claudecode.DialogKeystrokes(multi, provider.DialogAnswer{Selected: []int{1}, OtherText: "no anchovies"})
	want = []claudecode.KeyOp{{Named: []string{"Down"}}, {Named: []string{"Space"}},
		{Named: []string{"Down"}}, {Named: []string{"Down"}}, {Text: "no anchovies"},
		{Named: []string{"Down"}}, {Named: []string{"Enter"}},
		{Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-select+other recipe = %v; want %v", got, want)
	}

	// Free text ALONE is a valid multi-select answer (nothing toggled).
	got, _ = claudecode.DialogKeystrokes(multi, provider.DialogAnswer{OtherText: "surprise me"})
	want = []claudecode.KeyOp{{Named: []string{"Down"}}, {Named: []string{"Down"}}, {Named: []string{"Down"}},
		{Text: "surprise me"}, {Named: []string{"Down"}}, {Named: []string{"Enter"}},
		{Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-select other-only recipe = %v; want %v", got, want)
	}
}

// Multi-question sequencing (compat.md §7, live 2026-07-08): per-question
// recipes concatenated in question order — committing a question auto-advances
// the form, so there are no transition keys — then the review screen [Enter].
// No climb on any picker (Up wraps; pickers open on the top row).
func TestCompat_DialogKeystrokes_multiQuestion(t *testing.T) {
	d := provider.Dialog{
		Kind: provider.DialogKindQuestion, Prompt: "2 questions", Answerable: true,
		Questions: []provider.Question{
			{Header: "Color", Text: "Which color do you prefer?", Options: []provider.DialogOption{
				{Label: "Red"}, {Label: "Blue"}, {Label: "Other", IsOther: true}}},
			{Header: "Fruits", Text: "Which fruits do you like?", MultiSelect: true, Options: []provider.DialogOption{
				{Label: "Apple"}, {Label: "Banana"}, {Label: "Cherry"}, {Label: "Other", IsOther: true}}},
		},
	}
	got, err := claudecode.DialogKeystrokes(d, provider.DialogAnswer{Answers: []provider.QuestionAnswer{
		{Index: 0},              // Q0: Red (single-select, resolves on Enter, auto-advances)
		{Selected: []int{0, 2}}, // Q1: Apple + Cherry, committed via the Submit row
	}})
	if err != nil {
		t.Fatalf("multi-question: %v", err)
	}
	want := []claudecode.KeyOp{
		// Q0: Red is the top row → Enter selects it (auto-advances to Q1).
		{Named: []string{"Enter"}},
		// Q1: from the top, toggle Apple (Space), Down twice to Cherry (one
		// key per op — 2026-07-09), toggle; Down twice onto Submit (index 4),
		// Enter.
		{Named: []string{"Space"}},
		{Named: []string{"Down"}}, {Named: []string{"Down"}}, {Named: []string{"Space"}},
		{Named: []string{"Down"}}, {Named: []string{"Down"}}, {Named: []string{"Enter"}},
		// Review: cursor already on "Submit answers"; a bare Enter submits.
		{Named: []string{"Enter"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-question recipe = %v; want %v", got, want)
	}
}

// Plan-approval keystrokes (compat.md §7, live 2026-07-08): four pinned rows,
// no review screen, no climb (Enter on the chosen row resolves directly). Row
// 3 follows the Other path: type the feedback first, then Enter.
func TestCompat_DialogKeystrokes_plan(t *testing.T) {
	plan := provider.Dialog{Kind: provider.DialogKindPlan, Answerable: true, Prompt: "# Plan",
		Options: []provider.DialogOption{
			{Label: "Approve — auto-accept edits"},
			{Label: "Approve — review each edit"},
			{Label: "Reject — refine the plan"},
			{Label: "Reject with feedback", IsOther: true},
		}}
	got, err := claudecode.DialogKeystrokes(plan, provider.DialogAnswer{Index: 0})
	if err != nil {
		t.Fatalf("plan approve: %v", err)
	}
	// Row 0 is the top row → Enter approves directly (no climb, no review).
	want := []claudecode.KeyOp{{Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan approve recipe = %v; want %v", got, want)
	}

	got, err = claudecode.DialogKeystrokes(plan, provider.DialogAnswer{Index: 3, OtherText: "tighten the tests"})
	if err != nil {
		t.Fatalf("plan feedback: %v", err)
	}
	want = []claudecode.KeyOp{{Named: []string{"Down"}}, {Named: []string{"Down"}}, {Named: []string{"Down"}},
		{Text: "tighten the tests"}, {Named: []string{"Enter"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan feedback recipe = %v; want %v", got, want)
	}
}

// Multi-question hook payload → Dialog (compat.md §7/§9): the 2-question form
// captured live 2026-07-08 (the tool_input is byte-identical between the
// PreToolUse payload and the transcript tool_use — one mapper, two sources;
// this fixture's input is the live transcript's, wrapped in the §9 payload
// shape). Since issue #51 the form maps to Kind=question with per-question
// Questions and IS answerable.
func TestCompat_HookPayload_multiQuestion_maps(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "hook-pretooluse-multiquestion-2.1.198.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := claudecode.DialogFromHookPayload(b)
	if !ok {
		t.Fatal("DialogFromHookPayload: not recognised as a dialog")
	}
	if d.Kind != "question" || !d.Answerable || d.Prompt != "2 questions" {
		t.Fatalf("dialog = %+v; want an answerable question form with the summary prompt", d)
	}
	if len(d.Options) != 0 || d.Multi {
		t.Errorf("flat fields = %+v/%v; want empty (the form lives in Questions)", d.Options, d.Multi)
	}
	if len(d.Questions) != 2 {
		t.Fatalf("questions = %+v; want 2", d.Questions)
	}
	q0, q1 := d.Questions[0], d.Questions[1]
	if q0.Header != "Color" || q0.Text != "Which color do you prefer?" || q0.MultiSelect {
		t.Errorf("q0 = %+v; want the single-select Color question", q0)
	}
	// Listed options + the synthesized Other row, per question. lab's label is
	// the stable "Other" (the 2.1.198 TUI renders "Type something." — a
	// documented divergence, compat §7; the recipe navigates by index).
	if len(q0.Options) != 3 || q0.Options[0].Label != "Red" || q0.Options[1].Description != "cool" || !q0.Options[2].IsOther || q0.Options[2].Label != "Other" {
		t.Errorf("q0 options = %+v; want Red/Blue + Other", q0.Options)
	}
	if q1.Header != "Fruits" || !q1.MultiSelect || len(q1.Options) != 4 || !q1.Options[3].IsOther {
		t.Errorf("q1 = %+v; want the multiSelect Fruits question with Apple/Banana/Cherry + Other", q1)
	}
}

// ExitPlanMode payload → Dialog (compat.md §7): Kind=plan, the plan markdown
// as the prompt, and the four PINNED picker rows (live 2026-07-08 under lab's
// exact spawn shape) — answerable since issue #51. The input's planFilePath is
// deliberately ignored.
func TestCompat_HookPayload_planDialog_maps(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "hook-pretooluse-exitplanmode-2.1.198.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := claudecode.DialogFromHookPayload(b)
	if !ok {
		t.Fatal("DialogFromHookPayload: not recognised as a dialog")
	}
	if d.Kind != "plan" || !d.Answerable || d.Multi {
		t.Fatalf("dialog = %+v; want an answerable single-select plan", d)
	}
	if !strings.HasPrefix(d.Prompt, "# Plan: Add README.md note") {
		t.Errorf("Prompt = %q; want the plan markdown", d.Prompt)
	}
	// The four rows, 1:1 by index with the real picker — lab's own semantic
	// labels (the TUI's row-0 text drifts with session state; compat §7).
	wantRows := []string{
		"Approve — auto-accept edits",
		"Approve — review each edit",
		"Reject — refine the plan",
		"Reject with feedback",
	}
	if len(d.Options) != len(wantRows) {
		t.Fatalf("options = %+v; want the 4 pinned rows", d.Options)
	}
	for i, want := range wantRows {
		if d.Options[i].Label != want {
			t.Errorf("row %d = %q; want %q", i, d.Options[i].Label, want)
		}
		if d.Options[i].Description != "" {
			t.Errorf("row %d carries an invented description %q; the live picker shows none", i, d.Options[i].Description)
		}
	}
	if d.Options[0].IsOther || d.Options[1].IsOther || d.Options[2].IsOther {
		t.Errorf("rows 0-2 must not be free-text rows: %+v", d.Options)
	}
	if !d.Options[3].IsOther {
		t.Errorf("row 3 = %+v; want the free-text feedback row (IsOther)", d.Options[3])
	}
}

// The full live transcripts (captured 2026-07-08, 2.1.198 — the issue #51
// dialog verification runs) parse through the real mapper: every resolved
// dialog stays a DIALOG message whose Outcome carries the recorded resolution
// (issue #56 — never a demoted tool chip). These pin the RESOLUTION shapes
// (compat §5) that both the verification backstop and the outcome derivation
// read: the answered answers map (label, ", "-joined multi-select, free text
// verbatim), the user-rejected denial, the 60s afkTimeout, and the plan
// approve/reject-with-feedback pair.
func TestCompat_LiveTranscripts_resolutionShapes(t *testing.T) {
	parse := func(name string) provider.Chat {
		t.Helper()
		f, err := os.Open(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		chat, err := claudecode.ParseTranscript(f)
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", name, err)
		}
		return chat
	}
	outcome := func(chat provider.Chat, seq int64) *provider.DialogOutcome {
		t.Helper()
		for _, m := range chat.Messages {
			if m.Seq == seq {
				if m.Kind != provider.MessageDialog || m.Dialog == nil || m.Tool != nil {
					t.Fatalf("seq %d = %+v; want a dialog message (resolved dialogs are never tool chips)", seq, m)
				}
				if m.Dialog.Outcome == nil {
					t.Fatalf("seq %d dialog has no Outcome; a resolved dialog must not look pending", seq)
				}
				return m.Dialog.Outcome
			}
		}
		t.Fatalf("no message with seq %d", seq)
		return nil
	}

	// 2-question form answered + a decline + a free-text "Ferret" answer.
	chat := parse("transcript-askuserquestion-live-2.1.198.jsonl")
	if chat.Cursor != 11 {
		t.Fatalf("askuserquestion cursor = %d; want 11", chat.Cursor)
	}
	answered := outcome(chat, 4)
	// "Apple, Cherry" splits into the two listed labels (the fixture's Fruits
	// options are Apple/Banana/Cherry), in the recorded order.
	wantForm := []provider.QuestionResult{
		{Question: "Which color do you prefer?", Chosen: []string{"Red"}},
		{Question: "Which fruits do you like?", Chosen: []string{"Apple", "Cherry"}},
	}
	if answered.Dismissed || !reflect.DeepEqual(answered.Results, wantForm) {
		t.Errorf("answered form outcome = %+v; want results %+v", answered, wantForm)
	}
	declined := outcome(chat, 7)
	if !declined.Dismissed || len(declined.Results) != 0 {
		t.Errorf("declined outcome = %+v; want Dismissed with no results", declined)
	}
	ferret := outcome(chat, 10)
	// "Ferret" is not a listed label (Dog/Cat) — the typed Other text verbatim.
	if !reflect.DeepEqual(ferret.Results, []provider.QuestionResult{{Question: "Favorite pet?", OtherText: "Ferret"}}) {
		t.Errorf("free-text outcome = %+v; want the typed answer verbatim in OtherText", ferret)
	}

	// Single-question multiSelect: one 60s afkTimeout, one Submit-row answer.
	chat = parse("transcript-multiselect-timeout-live-2.1.198.jsonl")
	timeout := outcome(chat, 2)
	if !timeout.Dismissed || len(timeout.Results) != 0 {
		t.Errorf("timeout outcome = %+v; want Dismissed (answers:{} + afkTimeoutMs — no answer to show)", timeout)
	}
	submitted := outcome(chat, 5)
	if !reflect.DeepEqual(submitted.Results, []provider.QuestionResult{{Question: "Which toppings?", Chosen: []string{"Olives"}}}) {
		t.Errorf("submit-row outcome = %+v; want the committed multi-select label Olives", submitted)
	}

	// ExitPlanMode: reject-with-feedback, then the revised plan approved.
	chat = parse("transcript-exitplanmode-live-2.1.198.jsonl")
	rejected := outcome(chat, 7)
	if rejected.Approved || rejected.Dismissed ||
		rejected.Feedback != "Also add the current date to the note" {
		t.Errorf("plan reject outcome = %+v; want the typed feedback extracted from the denial string", rejected)
	}
	approved := outcome(chat, 9)
	if !approved.Approved || approved.Dismissed || approved.Feedback != "" {
		t.Errorf("plan approve outcome = %+v; want Approved", approved)
	}
}

// Async background-task fixtures (compat.md §5 async-task subsection, issue
// #159): three captured transcripts — an async Agent roundtrip (wild 2.1.204,
// mengenkauf-125), a Workflow roundtrip and a background Bash roundtrip (live
// 2.1.206) — drive ParseTranscript at cumulative prefixes as the conformance
// canary for the pending-work hold. The full add/remove/exclusion state matrix
// is unit-pinned in claudecode (TestParseTranscript_pendingWork*); THIS test
// pins the real on-disk shapes those inline lines were distilled from: the
// top-level toolUseResult.status=="async_launched" gate with agentId/taskId,
// and the three <task-notification> carriers. A future CLI that renames the
// status, moves agentId/taskId, or reshapes a carrier fails HERE first — the
// live failure mode is silent (a stuck `working` badge with suppressed pushes,
// or the spurious turn-end pushes returning), so don't wait for a field report.
//
// The workflow and bash files carry ONE hand-added turn-end assistant text
// line right after the launch result (marked in compat.md §5): in both live
// captures the work completed mid-turn, before any turn-ending text, so the
// verbatim order alone could never show the hold (workflow) or the no-hold
// (bash) — the enqueue would already have spoken. Marker lines are verbatim.
func TestCompat_AsyncTaskFixtures(t *testing.T) {
	readLines := func(name string, wantLines int) []string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) != wantLines {
			t.Fatalf("%s has %d lines; want %d (prefix indices below key off the pinned layout)", name, len(lines), wantLines)
		}
		return lines
	}
	parsePrefix := func(name string, lines []string, n int) provider.Chat {
		t.Helper()
		chat, err := claudecode.ParseTranscript(strings.NewReader(strings.Join(lines[:n], "\n") + "\n"))
		if err != nil {
			t.Fatalf("%s[:%d]: ParseTranscript: %v", name, n, err)
		}
		return chat
	}

	// Async Agent: launch → async_launched result (agentId) → turn-end text →
	// enqueue → dequeue → user-form delivery → resume text.
	name := "transcript-asyncagent-live-2.1.204.jsonl"
	lines := readLines(name, 7)
	// THE hold — the issue #159 drift canary: the turn ended but the launched
	// agent is pending, so the tail derives working, never needs_input. A CLI
	// that renames status "async_launched" or moves agentId flips this to
	// needs_input (the spurious push returns).
	if chat := parsePrefix(name, lines, 3); chat.State != provider.StateWorking {
		t.Errorf("agent launch + turn-end prefix: state = %q; want %q (the async-agent pending hold)", chat.State, provider.StateWorking)
	}
	chat := parsePrefix(name, lines, len(lines))
	// Full file: the completion notification released the id, so the resume
	// text is a real turn end again.
	if chat.State != provider.StateNeedsInput {
		t.Errorf("agent full file: state = %q; want %q (notification released the hold)", chat.State, provider.StateNeedsInput)
	}
	// Carrier 2 reaches the fold with rendering unchanged: the between-turns
	// standalone user message (plain-string content, origin task-notification,
	// no isMeta) stays a VISIBLE user text message carrying the raw payload.
	if len(chat.Messages) != 4 {
		t.Fatalf("agent full file mapped %d messages; want 4 (chip, turn-end text, delivery, resume text)", len(chat.Messages))
	}
	if d := chat.Messages[2]; d.Kind != provider.MessageText || d.Role != "user" ||
		!strings.Contains(d.Text, "<task-notification>") ||
		!strings.Contains(d.Text, "<task-id>a66020d7eda3c3c35</task-id>") {
		t.Errorf("agent delivery msg = %+v; want visible user text carrying the <task-notification> payload with its <task-id>", d)
	}

	// Workflow: launch → async_launched result (taskId, no isAsync) →
	// [hand-added turn-end text] → enqueue → mined mid-turn text → queue
	// remove → attachment delivery → resume thinking (captured signature-only,
	// empty thinking body).
	name = "transcript-asyncworkflow-live-2.1.206.jsonl"
	lines = readLines(name, 8)
	// The taskId hold: notifications reference toolUseResult.taskId (w…),
	// never the wf_… runId — a CLI that moves or renames it flips this.
	if chat := parsePrefix(name, lines, 3); chat.State != provider.StateWorking {
		t.Errorf("workflow launch + turn-end prefix: state = %q; want %q (the taskId pending hold)", chat.State, provider.StateWorking)
	}
	// The wild enqueue payload (<task-id> + <status>completed</status> in the
	// queue-operation's content — carrier 1) released the taskId, so the mined
	// mid-turn text derives needs_input. Pins the release against the real
	// payload shape: if the CLI reshapes the tags or moves the content field,
	// the id stays held and this flips to working.
	if chat := parsePrefix(name, lines, 5); chat.State != provider.StateNeedsInput {
		t.Errorf("workflow post-enqueue text prefix: state = %q; want %q (carrier 1 released the taskId)", chat.State, provider.StateNeedsInput)
	}
	// Full file: the tail is the attachment delivery (carrier 3) — a
	// working-deriving key, the model is about to resume — followed by the
	// empty-thinking line, which emits nothing.
	if chat := parsePrefix(name, lines, len(lines)); chat.State != provider.StateWorking {
		t.Errorf("workflow full file: state = %q; want %q (attachment delivery tail)", chat.State, provider.StateWorking)
	}

	// Background Bash: launch (run_in_background) → backgroundTaskId result →
	// [hand-added turn-end text] → enqueue → remove → attachment completion →
	// next tool_use.
	name = "transcript-asyncbash-live-2.1.206.jsonl"
	lines = readLines(name, 7)
	// The exclusion canary: backgroundTaskId with NO status never enters the
	// pending set, so the turn end is a genuine needs_input. A CLI that starts
	// stamping async_launched onto Bash results flips this to working — the
	// dev-server-never-exits trap (working pinned forever, pushes suppressed).
	if chat := parsePrefix(name, lines, 3); chat.State != provider.StateNeedsInput {
		t.Errorf("bash launch + turn-end prefix: state = %q; want %q (background Bash must never hold)", chat.State, provider.StateNeedsInput)
	}
	// Full file: the tail is the model's next tool_use after the completion
	// attachment — ordinary working.
	if chat := parsePrefix(name, lines, len(lines)); chat.State != provider.StateWorking {
		t.Errorf("bash full file: state = %q; want %q (next tool_use tail)", chat.State, provider.StateWorking)
	}
}

// Builtin slash-command table pin (compat.md §10, bundle-extracted 2.1.198):
// /clear's exact row (description verbatim, the role=clear tag, its arg
// hint), pinned-order builtins-first, and the exact chat-safe set — the
// curation is a per-row verdict, so any drift (a new safe command, a flipped
// verdict) must be a conscious edit here and in §10.
func TestCompat_BuiltinCommands_pinned(t *testing.T) {
	cmds := claudecode.BuiltinCommands()
	if len(cmds) == 0 || cmds[0].Name != "clear" {
		t.Fatalf("builtins[0] = %+v; want /clear first (pinned order)", cmds)
	}
	clear := cmds[0]
	if clear.Description != "Start a new session with empty context; previous session stays on disk (resumable with /resume)" {
		t.Errorf("clear description = %q; want the 2.1.198 bundle text verbatim", clear.Description)
	}
	if clear.ArgHint != "[name]" || clear.Role != provider.CommandRoleClear || !clear.ChatSafe || clear.Source != "builtin" {
		t.Errorf("clear row = %+v; want [name]/role=clear/chat-safe/builtin", clear)
	}

	wantSafe := []string{"clear", "compact", "context", "usage", "status", "export",
		"release-notes", "init", "review", "security-review"}
	var gotSafe []string
	for _, c := range cmds {
		if c.Source != "builtin" {
			t.Errorf("builtin table carries a non-builtin source: %+v", c)
		}
		if c.ChatSafe {
			gotSafe = append(gotSafe, c.Name)
		}
		if c.Name == "clear" && c.Role != provider.CommandRoleClear {
			t.Errorf("clear lost its role tag: %+v", c)
		}
		if c.Name != "clear" && c.Role != "" {
			t.Errorf("%s carries an undefined role %q; only clear is tagged", c.Name, c.Role)
		}
	}
	if !reflect.DeepEqual(gotSafe, wantSafe) {
		t.Errorf("chat-safe builtin set drifted:\n got  %v\n want %v", gotSafe, wantSafe)
	}
	// Curated-out rows that must stay present-but-unsafe (the API filters;
	// the catalog stays honest): the picker/editor/UI and lifecycle commands.
	unsafe := map[string]bool{}
	for _, c := range cmds {
		if !c.ChatSafe {
			unsafe[c.Name] = true
		}
	}
	for _, name := range []string{"model", "config", "permissions", "hooks", "mcp", "agents",
		"memory", "resume", "rewind", "statusline", "privacy-settings", "add-dir", "ide",
		"feedback", "doctor", "login", "logout", "exit", "upgrade", "install-github-app",
		"terminal-setup", "usage-credits", "plan"} {
		if !unsafe[name] {
			t.Errorf("expected curated-out builtin %q missing from the table", name)
		}
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

// Live logout pin (issue #46 DoD): the machine-level logout is a fragile Claude
// coupling — that `claude auth logout` needs no TTY and clears the creds file.
// Opt-in via LAB_COMPAT_LIVE=1 so CI stays hermetic.
//
// ⚠️ ISOLATION IS LOAD-BEARING: this runs the REAL `claude auth logout`. It is
// pinned to a throwaway CLAUDE_CONFIG_DIR seeded with a fake credentials file,
// so it can never touch the lab's real login. t.Setenv restores the env after
// the test, and claude honors CLAUDE_CONFIG_DIR — the same seam
// claudecode.credentialsPath resolves. Never drop the isolation.
func TestCompat_Live_authLogout(t *testing.T) {
	if os.Getenv("LAB_COMPAT_LIVE") != "1" {
		t.Skip("set LAB_COMPAT_LIVE=1 to probe the installed claude binary")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	// Isolate: a throwaway config dir with a fake credentials file. Nothing
	// here reaches the operator's real ~/.claude.
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	creds := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"fake","refreshToken":"fake","expiresAt":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Non-interactive: no TTY is wired up. A hang here (waiting on a prompt)
	// trips the outer `go test` timeout, which is the failure we want to catch.
	out, runErr := exec.Command(bin, "auth", "logout").CombinedOutput()
	t.Logf("live `claude auth logout` exit=%v output=%q", runErr, strings.TrimSpace(string(out)))

	// Observable state, not exit code (D: "Success = status"). The seeded
	// credentials file must be gone.
	if _, err := os.Stat(creds); !os.IsNotExist(err) {
		t.Errorf("credentials file still present after `claude auth logout`: stat err = %v", err)
	}
}
