package codexcompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/codex"
)

// spawnBase is the pinned flag prefix of every codex spawn (compat.md §1):
// the never-ask/full-access posture plus the AGENTS.local.md fallback key,
// each -c value one argv element with no shell quoting.
const spawnBase = `codex --ask-for-approval never --sandbox danger-full-access -c project_doc_fallback_filenames=["AGENTS.local.md"]`

// Spawn argv snapshot (compat.md §1). Manual spawn: model + effort, no seed
// prompt, so no trailing positional. SessionName is deliberately present in
// the spec and absent from the argv — codex has no --remote-control
// equivalent.
func TestCompat_SpawnArgvSnapshot(t *testing.T) {
	got := strings.Join(codex.SpawnArgv("codex", provider.SpawnSpec{
		SessionName: "repo~dom-20260710-1030", Model: "gpt-5.5", Effort: "high",
	}), " ")
	want := spawnBase + " --model gpt-5.5 -c model_reasoning_effort=high"
	if got != want {
		t.Errorf("spawn argv drifted:\n got  %q\n want %q", got, want)
	}
}

// AFK spawn argv (compat.md §1): the seed prompt is the single trailing
// positional AFTER every flag (official `codex [options] [prompt]` shape) —
// carried at spawn, never piped via stdin (`codex exec` blocks reading a
// dangling pipe).
func TestCompat_SpawnArgvSeedPromptSnapshot(t *testing.T) {
	got := strings.Join(codex.SpawnArgv("codex", provider.SpawnSpec{
		SessionName: "repo~afk-87", Model: "gpt-5.5", Effort: "medium", InitialPrompt: "resolve #87",
	}), " ")
	want := spawnBase + " --model gpt-5.5 -c model_reasoning_effort=medium resolve #87"
	if got != want {
		t.Errorf("seeded spawn argv drifted:\n got  %q\n want %q", got, want)
	}
}

// Remote is received and IGNORED (issue #163): codex declares no
// provider.RemoteCapable, so lab's remote knob must not reach its argv at all —
// byte-identical for both values, prompt included. (0.133's `codex
// remote-control` is a host-level app-server daemon, not a per-session flag;
// nothing here to honor.)
func TestCompat_SpawnArgvIgnoresRemote(t *testing.T) {
	spec := provider.SpawnSpec{
		SessionName: "repo~afk-87", Model: "gpt-5.5", Effort: "medium", InitialPrompt: "resolve #87",
	}
	off := codex.SpawnArgv("codex", spec)
	spec.Remote = true
	on := codex.SpawnArgv("codex", spec)
	if !reflect.DeepEqual(off, on) {
		t.Errorf("spec.Remote leaked into the codex argv:\n Remote:false %q\n Remote:true  %q", off, on)
	}
	if _, ok := any((*codex.Provider)(nil)).(provider.RemoteCapable); ok {
		t.Error("codex implements provider.RemoteCapable; it ignores the knob, so it must not advertise it (issue #163)")
	}
}

// Effort is ALWAYS explicit (compat.md §1): `codex exec` defaults the
// reasoning effort to NONE (the TUI to medium), so an empty spec.Effort
// injects medium instead of omitting the flag; an empty model IS omitted
// (codex resolves its own default slug).
func TestCompat_SpawnArgvEffortAlwaysExplicit(t *testing.T) {
	got := strings.Join(codex.SpawnArgv("codex", provider.SpawnSpec{}), " ")
	want := spawnBase + " -c model_reasoning_effort=medium"
	if got != want {
		t.Errorf("default spawn argv drifted:\n got  %q\n want %q", got, want)
	}
	if joined := strings.Join(codex.SpawnArgv("codex", provider.SpawnSpec{Model: "gpt-5.5"}), " "); !strings.Contains(joined, "model_reasoning_effort=medium") {
		t.Errorf("empty effort must inject the explicit medium default; got %q", joined)
	}
}

// Model/effort catalogs against the trimmed `codex debug models` fixture
// (compat.md §1, cli extraction): the adapter's ENRICHED catalog (issue #156)
// must be exactly the visibility=="list" entries (the internal
// codex-auto-review is "hide" and filtered), each carrying that model's OWN
// supported_reasoning_levels (mapped through the adapter's pinned effort
// labels) and its default_reasoning_level; the pinned union efforts must
// match the default model's supported_reasoning_levels with medium as its
// default.
func TestCompat_ModelCatalog_matchesDebugModelsFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "models-0.133.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Models []struct {
			Slug                  string `json:"slug"`
			DisplayName           string `json:"display_name"`
			Visibility            string `json:"visibility"`
			DefaultReasoningLevel string `json:"default_reasoning_level"`
			SupportedLevels       []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("models fixture does not parse: %v", err)
	}

	// The adapter's pinned effort labels (codex reports no display names for
	// reasoning levels, so the labels are adapter-owned and pinned here).
	effortLabels := map[string]string{
		"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra high",
	}
	var wantModels []provider.ModelOption
	for _, m := range doc.Models {
		if m.Visibility != "list" {
			continue // codex-auto-review et al: not operator-selectable
		}
		mo := provider.ModelOption{
			Option:        provider.Option{Value: m.Slug, Label: m.DisplayName},
			DefaultEffort: m.DefaultReasoningLevel,
		}
		for _, l := range m.SupportedLevels {
			label, ok := effortLabels[l.Effort]
			if !ok {
				t.Fatalf("fixture reports reasoning level %q with no pinned adapter label — extend the codex effort catalog", l.Effort)
			}
			mo.Efforts = append(mo.Efforts, provider.Option{Value: l.Effort, Label: label})
		}
		wantModels = append(wantModels, mo)
	}
	p := &codex.Provider{}
	if got := p.Models(); !reflect.DeepEqual(got, wantModels) {
		t.Errorf("model catalog drifted from the debug-models fixture:\n got  %v\n want %v", got, wantModels)
	}

	// The default model (first listed) pins the effort catalog and default.
	def := doc.Models[0]
	if def.Slug != "gpt-5.5" || def.DefaultReasoningLevel != "medium" {
		t.Errorf("fixture default model = %s (default effort %s); want gpt-5.5/medium", def.Slug, def.DefaultReasoningLevel)
	}
	var wantEfforts []string
	for _, l := range def.SupportedLevels {
		wantEfforts = append(wantEfforts, l.Effort)
	}
	var gotEfforts []string
	for _, e := range p.Efforts() {
		gotEfforts = append(gotEfforts, e.Value)
	}
	if !reflect.DeepEqual(gotEfforts, wantEfforts) {
		t.Errorf("effort catalog drifted:\n got  %v\n want %v (no minimal tier on 0.133)", gotEfforts, wantEfforts)
	}
}

// Auth-status coupling (compat.md §2): both pinned `codex login status`
// shapes, exit code + text together. The logged-out fixture carries the
// leading CODEX_HOME-under-/tmp WARNING line — parsing is containment-based
// and must tolerate leading noise.
func TestCompat_AuthStatusFixtures_parse(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "login-status-in-0.133.0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	st := codex.ParseAuthStatus(in, true)
	if !st.LoggedIn {
		t.Errorf("logged-in fixture: LoggedIn = false; want true")
	}
	if st.Method != "chatgpt" {
		t.Errorf("logged-in fixture: Method = %q; want chatgpt (lowercased token after \"using \")", st.Method)
	}
	if st.Email != "" {
		t.Errorf("logged-in fixture: Email = %q; codex prints none", st.Email)
	}
	// The exit code is load-bearing: the same stdout with a non-zero exit is
	// NOT logged in (the pinned verdict needs both signals).
	if codex.ParseAuthStatus(in, false).LoggedIn {
		t.Error("logged-in text with a non-zero exit parsed as logged in; the exit code must gate the verdict")
	}

	out, err := os.ReadFile(filepath.Join("testdata", "login-status-out-0.133.0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Not logged in") {
		t.Fatalf("logged-out fixture lost its pinned verdict line: %q", out)
	}
	if codex.ParseAuthStatus(out, false).LoggedIn {
		t.Error("logged-out fixture (exit 1) parsed as logged in")
	}
}

// Device-code login scrape (compat.md §2): the verification URL and one-time
// user code extract from the captured `codex login --device-auth` stdout
// (ANSI-stripped — the production scrape source is tmux capture-pane rendered
// text; the code in the fixture expired 15 minutes after the 2026-07-10
// capture).
func TestCompat_DeviceAuthFixture_extracts(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "device-auth-0.133.0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if got, want := codex.ExtractLoginURL(s), "https://auth.openai.com/codex/device"; got != want {
		t.Errorf("ExtractLoginURL = %q; want %q", got, want)
	}
	if got, want := codex.ExtractUserCode(s), "Y9HC-QKI85"; got != want {
		t.Errorf("ExtractUserCode = %q; want %q", got, want)
	}
	// The version banner ("Welcome to Codex [v0.133.0]") precedes the code and
	// must never be mistaken for one — the code regex boundaries exclude '-'
	// and the version has no XXXX- shape; pin that the banner is present so
	// the fixture keeps exercising that noise.
	if !strings.Contains(s, "Welcome to Codex [v0.133.0]") {
		t.Error("fixture lost the version banner the scrape must tolerate")
	}
}

// parseRollout maps a rollout fixture through the production parser.
func parseRollout(t *testing.T, name string) provider.Chat {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	chat, err := codex.ParseTranscript(f)
	if err != nil {
		t.Fatalf("%s: ParseTranscript: %v", name, err)
	}
	return chat
}

// Rollout grammar, plain + exec tools (compat.md §5): a REAL `codex exec`
// rollout (base_instructions stubbed, all else verbatim). Pins the
// double-render guard (response_item messages skipped — prose arrives via
// user_message/agent_message only), exec_command chip titles, call_id
// back-patching, and the task_complete → needs_input state fold.
func TestCompat_RolloutPlainFixture_maps(t *testing.T) {
	chat := parseRollout(t, "rollout-plain-0.133.0.jsonl")
	if len(chat.Messages) != 7 || chat.Cursor != 7 {
		t.Fatalf("mapped %d messages (cursor %d); want 7 (response_item messages and token_count/turn_context must not render)",
			len(chat.Messages), chat.Cursor)
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("State = %q; want %q (tail is task_complete)", chat.State, provider.StateNeedsInput)
	}
	// Context-pressure meter (issue #243 / ADR-0061): the last usable token_count
	// carries last_token_usage.total_tokens 11894 / reasoning_output_tokens 0 and a
	// self-reported 258400 window, so the meter reads 11894 of 258400 (token_count
	// still never renders as a message — the count above stays 7).
	if cu := chat.ContextUsage; cu == nil || cu.Used != 11894 || cu.Limit != 258400 {
		t.Errorf("ContextUsage = %+v; want Used 11894 / Limit 258400 (last token_count's last_token_usage)", cu)
	}

	m := chat.Messages
	if m[0].Kind != provider.MessageText || m[0].Role != "user" ||
		m[0].Text != `Commit the existing file hello.txt with git using the commit message "add hello". Then reply DONE.` {
		t.Errorf("msg0 = %+v; want the user_message prompt verbatim", m[0])
	}
	if m[1].Kind != provider.MessageText || m[1].Role != "assistant" || m[1].Thinking {
		t.Errorf("msg1 = %+v; want visible assistant prose (agent_message)", m[1])
	}
	if m[2].Kind != provider.MessageTool || m[2].Tool == nil ||
		m[2].Tool.Title != "Ran git status --short" || m[2].Tool.Status != "ok" {
		t.Errorf("msg2 = %+v; want the exec_command chip, back-patched ok", m[2])
	}
	if m[5].Tool == nil || m[5].Tool.Title != `Ran git add hello.txt && git commit -m "add hello"` {
		t.Errorf("msg5 = %+v; want the commit chip title from the arguments cmd", m[5])
	}
	if m[5].Tool != nil && !strings.Contains(m[5].Tool.Output, "add hello") {
		t.Errorf("msg5 output = %q; want the git commit output back-patched by call_id", m[5].Tool.Output)
	}
	if m[6].Kind != provider.MessageText || m[6].Role != "assistant" || m[6].Text != "DONE" {
		t.Errorf("msg6 = %+v; want the final agent_message", m[6])
	}
	// This rollout is also the §9 attribution ground truth: the commit the
	// agent made carries the verbatim message and nothing else.
	for _, msg := range m {
		if msg.Tool != nil && strings.Contains(strings.ToLower(msg.Tool.Input), "co-authored-by") {
			t.Errorf("commit input carries an attribution trailer: %q", msg.Tool.Input)
		}
	}
}

// Rollout grammar, apply_patch + encrypted reasoning (compat.md §5): the
// custom_tool_call chip ("Applied patch <file>") resolved by
// custom_tool_call_output via the same call_id map; encrypted reasoning with
// empty summaries renders NOTHING; a failed command (the read-only-.git
// sandbox trap, §1) still back-patches status "ok" — the exit code rides in
// the output text, not a distinct error status.
func TestCompat_RolloutPatchFixture_maps(t *testing.T) {
	chat := parseRollout(t, "rollout-patch-0.133.0.jsonl")
	if len(chat.Messages) != 17 || chat.Cursor != 17 {
		t.Fatalf("mapped %d messages (cursor %d); want 17", len(chat.Messages), chat.Cursor)
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("State = %q; want %q", chat.State, provider.StateNeedsInput)
	}

	m := chat.Messages
	patch := m[5]
	if patch.Kind != provider.MessageTool || patch.Tool == nil ||
		patch.Tool.Name != "apply_patch" || patch.Tool.Title != "Applied patch hello.txt" {
		t.Fatalf("msg5 = %+v; want the apply_patch chip titled from the patch's first file line", patch)
	}
	if patch.Tool.Status != "ok" || !strings.Contains(patch.Tool.Output, "Success. Updated the following files") {
		t.Errorf("apply_patch chip = %+v; want ok with the custom_tool_call_output back-patched", patch.Tool)
	}
	// The failing `git commit` (exit 128 — .git mounted read-only under
	// workspace-write): status stays "ok", the failure rides in the text.
	commit := m[9]
	if commit.Tool == nil || !strings.HasPrefix(commit.Tool.Title, "Ran git add hello.txt") {
		t.Fatalf("msg9 = %+v; want the failing commit chip", commit)
	}
	if commit.Tool.Status != "ok" || !strings.Contains(commit.Tool.Output, "Process exited with code 128") {
		t.Errorf("failed-commit chip = %+v; codex marks no distinct error status — the 128 must ride in the output text", commit.Tool)
	}
	// Encrypted reasoning (three records, all with empty summaries) must not
	// render: no thinking messages anywhere in this fixture.
	for i, msg := range m {
		if msg.Thinking {
			t.Errorf("msg%d is a thinking message; encrypted reasoning with an empty summary must render nothing", i)
		}
	}
}

// Rollout grammar, interrupted TUI session (compat.md §5/§6): a REAL TUI
// rollout ending in `turn_aborted` reason "interrupted" (a single mid-turn
// Esc). Pins the interrupted lifecycle message + needs_input state, the
// multi-line bracketed-paste user text, and the queue-with-steer mid-turn
// message landing as an ordinary user_message.
func TestCompat_RolloutAbortedFixture_maps(t *testing.T) {
	chat := parseRollout(t, "rollout-aborted-0.133.0.jsonl")
	if len(chat.Messages) != 12 || chat.Cursor != 12 {
		t.Fatalf("mapped %d messages (cursor %d); want 12", len(chat.Messages), chat.Cursor)
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("State = %q; want %q (turn_aborted hands the composer back)", chat.State, provider.StateNeedsInput)
	}

	m := chat.Messages
	// Multi-line reply delivered via bracketed paste (§6): newlines preserved.
	if m[2].Role != "user" || m[2].Text != "line one of a multi-line message\nline two of it\nReply with exactly: GOT-3-LINES" {
		t.Errorf("msg2 = %+v; want the multi-line pasted user message verbatim", m[2])
	}
	if m[7].Kind != provider.MessageTool || m[7].Tool == nil ||
		m[7].Tool.Title != "Ran sleep 25" || m[7].Tool.Status != "ok" {
		t.Errorf("msg7 = %+v; want the sleep 25 chip resolved ok", m[7])
	}
	// Queue-with-steer (§6): the message sent MID-TURN was queued and
	// delivered in the same turn — it lands as an ordinary user_message.
	if m[8].Role != "user" || m[8].Text != "Additionally, after SLEPT also reply with exactly: POLO" {
		t.Errorf("msg8 = %+v; want the queued mid-turn user message", m[8])
	}
	if m[9].Role != "assistant" || m[9].Text != "SLEPT\nPOLO" {
		t.Errorf("msg9 = %+v; want the same-turn answer to both markers", m[9])
	}
	// The interrupt tail: a lifecycle message carrying the pinned reason.
	last := m[11]
	if last.Kind != provider.MessageLifecycle || last.Text != "interrupted" || last.Error {
		t.Errorf("msg11 = %+v; want a non-error lifecycle %q (turn_aborted reason)", last, "interrupted")
	}
}

// Trust-append round-trip (compat.md §3): the exact TOML table codex reads,
// appended to the exact file it reads it from. The `-c` pre-grant route is
// dead on 0.133; the append is the working mechanism, and the guard must be
// append-only and tolerant of codex mutating config.toml on its own.
func TestCompat_TrustAppend_roundtrip(t *testing.T) {
	dir := "/var/lib/lab/worktrees/some-repo"

	// Fresh file: created with the header + trust_level, parents included.
	fresh := filepath.Join(t.TempDir(), ".codex", "config.toml")
	if err := codex.SeedTrust(fresh, dir); err != nil {
		t.Fatalf("SeedTrust(fresh): %v", err)
	}
	b, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `[projects."`+dir+`"]`) || !strings.Contains(string(b), `trust_level = "trusted"`) {
		t.Errorf("fresh config lacks the pinned trust table:\n%s", b)
	}

	// Existing content — operator settings plus a codex-auto-written entry
	// for ANOTHER dir — is preserved verbatim, the new table appended below.
	pre := "model = \"gpt-5.5\"\n\n[projects.\"/home/op/other\"]\ntrust_level = \"trusted\"\n"
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := codex.SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust(existing): %v", err)
	}
	b, _ = os.ReadFile(cfg)
	if !strings.HasPrefix(string(b), pre) {
		t.Errorf("append rewrote existing content (must be append-only):\n%s", b)
	}
	if !strings.Contains(string(b), `[projects."`+dir+`"]`) {
		t.Errorf("worktree trust table missing after append:\n%s", b)
	}

	// Idempotent: a second seed is a byte-for-byte no-op.
	before, _ := os.ReadFile(cfg)
	if err := codex.SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust(again): %v", err)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Errorf("second seed rewrote the file:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// codex-mutated entry: codex auto-writes trust entries itself — a header
	// hit means HANDS OFF, even when codex chose a different trust_level.
	mutated := filepath.Join(t.TempDir(), "config.toml")
	own := "[projects.\"" + dir + "\"]\ntrust_level = \"none\"\n"
	if err := os.WriteFile(mutated, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := codex.SeedTrust(mutated, dir); err != nil {
		t.Fatalf("SeedTrust(codex-mutated): %v", err)
	}
	b, _ = os.ReadFile(mutated)
	if string(b) != own {
		t.Errorf("seed touched a codex-owned entry (must tolerate codex's own writes):\n%s", b)
	}
}

// AGENTS.md bridge round-trip (compat.md §4): create when missing, append
// below an operator's own global instructions when unmarked, hands off when
// the marker is present. The marker line is the guard; the wording below it
// is lab's own.
func TestCompat_AgentsBridge_roundtrip(t *testing.T) {
	const marker = "<!-- lab:agents-local-bridge -->"

	// Create: missing file gets exactly the marker-guarded block.
	created := filepath.Join(t.TempDir(), ".codex", "AGENTS.md")
	if err := codex.SeedAgentsBridge(created); err != nil {
		t.Fatalf("SeedAgentsBridge(create): %v", err)
	}
	b, err := os.ReadFile(created)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), marker+"\n") || !strings.Contains(string(b), "AGENTS.local.md") {
		t.Errorf("created bridge lacks the marker-guarded block:\n%s", b)
	}

	// Append: unmarked operator content survives above the block.
	appended := filepath.Join(t.TempDir(), "AGENTS.md")
	own := "# My global codex instructions\n\nAlways use rg.\n"
	if err := os.WriteFile(appended, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := codex.SeedAgentsBridge(appended); err != nil {
		t.Fatalf("SeedAgentsBridge(append): %v", err)
	}
	b, _ = os.ReadFile(appended)
	if !strings.HasPrefix(string(b), own) {
		t.Errorf("append rewrote the operator's global instructions:\n%s", b)
	}
	if !strings.Contains(string(b), marker) {
		t.Errorf("bridge block missing after append:\n%s", b)
	}

	// Skip: a marked file is left byte-identical (marker anywhere = installed).
	before, _ := os.ReadFile(appended)
	if err := codex.SeedAgentsBridge(appended); err != nil {
		t.Fatalf("SeedAgentsBridge(skip): %v", err)
	}
	after, _ := os.ReadFile(appended)
	if string(before) != string(after) {
		t.Errorf("re-seed rewrote a bridged file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// popupCommandRe extracts the command names from the raw slash-popup scrape:
// a row is "  /name  description…" (two-plus spaces after the name; wrapped
// description lines carry no slash).
var popupCommandRe = regexp.MustCompile(`(?m)^\s+/([a-z-]+)\s{2,}`)

// Builtin slash-command catalog (compat.md §7, live TUI scrape 2026-07-10):
// the pinned table against the raw popup capture — count, pinned chat-safe
// set and order, /new's clear role, and table/scrape matching exactly in
// both directions (any divergence must be a deliberate edit of commands.go
// AND compat.md §7).
func TestCompat_BuiltinCommands_pinned(t *testing.T) {
	cmds := codex.BuiltinCommands()
	if len(cmds) == 0 || cmds[0].Name != "new" {
		t.Fatalf("builtins[0] = %+v; want /new first (pinned order)", cmds)
	}
	clear := cmds[0]
	if clear.Description != "start a new chat during a conversation" {
		t.Errorf("/new description = %q; want the 0.133.0 popup text verbatim", clear.Description)
	}
	if clear.Role != provider.CommandRoleClear || !clear.ChatSafe || clear.Source != "builtin" {
		t.Errorf("/new row = %+v; want role=clear/chat-safe/builtin (native rotation, compat §5)", clear)
	}

	wantSafe := []string{"new", "clear", "compact", "status", "diff", "mcp", "ps", "stop", "copy"}
	var gotSafe []string
	table := map[string]bool{}
	for _, c := range cmds {
		table[c.Name] = true
		if c.Source != "builtin" {
			t.Errorf("catalog carries a non-builtin source (codex has no project/user discovery): %+v", c)
		}
		if c.ChatSafe {
			gotSafe = append(gotSafe, c.Name)
		}
		if c.Name != "new" && c.Role != "" {
			t.Errorf("%s carries role %q; only /new is tagged", c.Name, c.Role)
		}
	}
	if !reflect.DeepEqual(gotSafe, wantSafe) {
		t.Errorf("chat-safe builtin set drifted:\n got  %v\n want %v", gotSafe, wantSafe)
	}

	// Cross-check against the raw scrape fixture.
	b, err := os.ReadFile(filepath.Join("testdata", "commands-popup-0.133.0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	scraped := map[string]bool{}
	for _, m := range popupCommandRe.FindAllStringSubmatch(string(b), -1) {
		scraped[m[1]] = true
	}
	if len(scraped) != 40 {
		t.Errorf("popup scrape yields %d distinct commands; want 40 (re-scrape or update §7)", len(scraped))
	}
	for name := range table {
		if !scraped[name] {
			t.Errorf("pinned builtin /%s is not in the popup scrape", name)
		}
	}
	for name := range scraped {
		if !table[name] {
			t.Errorf("scraped command /%s is missing from the pinned table (unpinned drift — update commands.go and compat.md §7 together)", name)
		}
	}
}

// Scrub-marker samples (compat.md §9): codex 0.133 writes NO attribution at
// the source (live ground truth), so the declared ScrubPatterns are
// defensive — but they must actually catch the documented upstream marker
// shapes (the Tier-1 conformance AttributionSamples) and spare the near-miss
// clean lines, through the RE2 engine the agent-API sanitizer runs.
func TestCompat_ScrubPatterns_markerSamples(t *testing.T) {
	meta := (&codex.Provider{}).SeedMeta()
	res, err := provider.CompileScrubPatterns(meta.ScrubPatterns)
	if err != nil {
		t.Fatalf("CompileScrubPatterns: %v", err)
	}
	matchAny := func(line string) bool {
		for _, re := range res {
			if re.MatchString(line) {
				return true
			}
		}
		return false
	}
	for _, marker := range []string{
		"Co-authored-by: Codex <noreply@openai.com>", // the documented upstream default
		"Co-authored-by: ChatGPT Codex <bot@openai.com>",
		"Generated with Codex",
	} {
		if !matchAny(marker) {
			t.Errorf("no scrub pattern catches the marker sample %q", marker)
		}
	}
	for _, clean := range []string{
		"Co-authored-by: Alice <alice@example.com>",
		"The openai.com docs describe the responses API.",
	} {
		if matchAny(clean) {
			t.Errorf("a scrub pattern over-matches the clean line %q", clean)
		}
	}
}
