package claudecode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// writeFileWithModTime writes body to path and sets its mtime, so the marker
// staleness check (transcript mtime vs marker mtime) can be exercised
// deterministically.
func writeFileWithModTime(t *testing.T, path, body string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// prePayload is the Appendix PreToolUse payload (2.1.198), single-question.
// session_id matches the transcript filename stem, as live payloads do — the
// rotation-staleness key pendingDialog compares.
const prePayload = `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","session_id":"sess-live","transcript_path":"/projects/x/sess-live.jsonl","cwd":"c","permission_mode":"auto","tool_use_id":"toolu_ABC","tool_input":{"questions":[{"question":"Pick one?","header":"H","multiSelect":false,"options":[{"label":"A","description":"da"},{"label":"B","description":"db"}]}]}}`

func spoolTestProvider(t *testing.T) *Provider {
	t.Helper()
	// The spool methods use only their dir/runID arguments, not the Provider's
	// fields, so a bare struct suffices (New requires runner/bus this doesn't
	// touch). Named distinctly from the package's other testProvider helper.
	return &Provider{}
}

func writeSpool(t *testing.T, dir, sub, runID, body string) {
	t.Helper()
	d := filepath.Join(dir, sub)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, runID+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSetup_shapeAndArgs(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	settings, settingsPath, args := p.Setup("run_1", dir, provider.SetupOpts{})

	if settingsPath != filepath.Join(dir, "settings.run_1.json") {
		t.Errorf("settingsPath = %q", settingsPath)
	}
	if len(args) != 2 || args[0] != "--settings" || args[1] != settingsPath {
		t.Errorf("args = %v; want [--settings %s]", args, settingsPath)
	}

	// Zero opts → NO env block at all: an AFK run's settings file must stay
	// byte-identical to the pre-#124 hooks-only payload.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(settings, &raw); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	if _, ok := raw["env"]; ok {
		t.Errorf(`settings for zero opts carry an "env" key: %s`, settings)
	}

	var s hookSettings
	if err := json.Unmarshal(settings, &s); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	if len(s.Hooks.PreToolUse) != 1 || s.Hooks.PreToolUse[0].Matcher != "AskUserQuestion|ExitPlanMode" {
		t.Errorf("PreToolUse matcher = %+v", s.Hooks.PreToolUse)
	}
	if len(s.Hooks.PostToolUse) != 1 || s.Hooks.PostToolUse[0].Matcher != "AskUserQuestion|ExitPlanMode" {
		t.Errorf("PostToolUse matcher = %+v", s.Hooks.PostToolUse)
	}
	if len(s.Hooks.Notification) != 1 || s.Hooks.Notification[0].Matcher != "" {
		t.Errorf("Notification matcher = %+v; want empty (match all)", s.Hooks.Notification)
	}
	// PreToolUse spools to dialogs/<runID>.json via an atomic temp+rename; the
	// command must reference the run's spool path and be observational (no exit 2).
	pre := s.Hooks.PreToolUse[0].Hooks[0].Command
	spool := filepath.Join(dir, "dialogs", "run_1.json")
	if !strings.Contains(pre, spool) || !strings.Contains(pre, "mkdir -p") || !strings.Contains(pre, "mv ") {
		t.Errorf("PreToolUse command = %q; want an atomic write to %q", pre, spool)
	}
	if strings.Contains(pre, "exit 2") {
		t.Errorf("PreToolUse command must never exit 2 (would block the tool): %q", pre)
	}
	// PostToolUse removes the same spool.
	if post := s.Hooks.PostToolUse[0].Hooks[0].Command; !strings.Contains(post, "rm -f") || !strings.Contains(post, spool) {
		t.Errorf("PostToolUse command = %q; want rm -f %q", post, spool)
	}
}

// A DialogTimeout in the opts (issue #124) rides the settings env block as
// CLAUDE_AFK_TIMEOUT_MS in milliseconds (compat §11), clamped to the JS
// setTimeout hazard ceiling 2^31−1 — a larger delay would fire IMMEDIATELY,
// re-enabling the instant auto-dismiss the knob exists to defeat. The hooks
// block must be byte-identical to the zero-opts payload either way.
func TestSetup_dialogTimeoutEnvBlock(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	baseline, _, _ := p.Setup("run_1", dir, provider.SetupOpts{})
	var base map[string]json.RawMessage
	if err := json.Unmarshal(baseline, &base); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		timeout time.Duration
		wantMs  string
	}{
		{"90s in milliseconds", 90 * time.Second, "90000"},
		{"over the cap clamps to 2^31-1", 40 * 24 * time.Hour, "2147483647"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, _, _ := p.Setup("run_1", dir, provider.SetupOpts{DialogTimeout: tc.timeout})
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(settings, &raw); err != nil {
				t.Fatal(err)
			}
			var env map[string]string
			if err := json.Unmarshal(raw["env"], &env); err != nil {
				t.Fatalf(`no valid "env" block in %s: %v`, settings, err)
			}
			if len(env) != 1 || env["CLAUDE_AFK_TIMEOUT_MS"] != tc.wantMs {
				t.Errorf(`env = %v; want exactly {"CLAUDE_AFK_TIMEOUT_MS":%q}`, env, tc.wantMs)
			}
			// The hooks block is untouched by the knob.
			if string(raw["hooks"]) != string(base["hooks"]) {
				t.Errorf("hooks block changed by DialogTimeout:\n got %s\nwant %s", raw["hooks"], base["hooks"])
			}
		})
	}
}

func TestPendingDialog_readsAndSuppressesResolved(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)

	// No transcript → the dialog is live.
	d, ok := p.pendingDialog("run_1", dir, "")
	if !ok {
		t.Fatal("pendingDialog: want a live dialog")
	}
	if d.ToolID != "toolu_ABC" || !d.Answerable || len(d.Options) != 3 { // A, B, Other
		t.Fatalf("dialog = %+v; want answerable A/B/Other with ToolID toolu_ABC", d)
	}

	// Same-session transcripts (the filename stem matches the spool's
	// session_id), in subdirs so both can carry the session's name.
	mkTranscript := func(sub, line string) string {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(d, "sess-live.jsonl")
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A transcript that already contains the tool_use_id (retro-flushed) →
	// resolved → suppressed.
	transcript := mkTranscript("a", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ABC","name":"AskUserQuestion","input":{}}]}}`+"\n")
	if _, ok := p.pendingDialog("run_1", dir, transcript); ok {
		t.Error("pendingDialog: want suppressed once the tool_use_id is in the transcript")
	}

	// A different tool_use_id in the transcript does NOT suppress.
	other := mkTranscript("b", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ZZZ","name":"Bash","input":{}}]}}`+"\n")
	if _, ok := p.pendingDialog("run_1", dir, other); !ok {
		t.Error("pendingDialog: an unrelated tool_use_id must not suppress the dialog")
	}
}

// A transcript WRITE during the pending window must not hide the dialog.
// Verified live (2026-07-10, grill session d4be520a): Claude Code appends
// queue-operation/attachment entries while a picker is up (an operator message
// queued mid-dialog), so the old "byte-frozen while pending" premise behind an
// mtime staleness check is false — it suppressed a genuinely pending dialog
// until it resolved (the chat's dialog card vanished while the TUI picker
// stayed up). Staleness is keyed to session identity now; same-session mtime
// ordering must be irrelevant in both directions.
func TestPendingDialog_survivesTranscriptWritesDuringPending(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	spool := dialogSpoolPath(dir, "run_1")
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	writeFileWithModTime(t, spool, prePayload, t0)

	// Same session, written a minute AFTER the spool (a queued message landed),
	// tool_use_id not present → still pending → still served.
	transcript := filepath.Join(dir, "sess-live.jsonl")
	writeFileWithModTime(t, transcript, `{"type":"queue-operation"}`+"\n", t0.Add(time.Minute))
	if _, ok := p.pendingDialog("run_1", dir, transcript); !ok {
		t.Error("a same-session transcript write during the pending window must not suppress the dialog")
	}
}

// pendingDialog rides the same mapper as the transcript (dialogFromToolUse),
// so the shapes issue #51 made answerable — multi-question AskUserQuestion
// and ExitPlanMode — are answerable through the spool with no spool-specific
// code. Payloads mirror the live 2.1.198 hook/tool inputs (2026-07-08).
func TestPendingDialog_multiQuestionAndPlanAnswerable(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()

	multiQ := `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_use_id":"toolu_MQ","tool_input":{"questions":[{"question":"Which color do you prefer?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":"warm"},{"label":"Blue","description":"cool"}]},{"question":"Which fruits do you like?","header":"Fruits","multiSelect":true,"options":[{"label":"Apple"},{"label":"Banana"},{"label":"Cherry"}]}]}}`
	writeSpool(t, dir, dialogsSubdir, "run_mq", multiQ)
	d, ok := p.pendingDialog("run_mq", dir, "")
	if !ok || !d.Answerable || d.Kind != provider.DialogKindQuestion {
		t.Fatalf("multi-question spool = %+v,%v; want an answerable question dialog", d, ok)
	}
	if len(d.Questions) != 2 || d.Prompt != "2 questions" {
		t.Fatalf("dialog = %+v; want Questions len 2 with the summary prompt", d)
	}
	if q := d.Questions[1]; !q.MultiSelect || q.Header != "Fruits" || len(q.Options) != 4 || !q.Options[3].IsOther {
		t.Errorf("question 1 = %+v; want multiSelect Fruits with Apple/Banana/Cherry + Other", q)
	}

	plan := `{"hook_event_name":"PreToolUse","tool_name":"ExitPlanMode","tool_use_id":"toolu_PL","tool_input":{"plan":"# Plan\n\n- do the thing\n","planFilePath":"/tmp/plan.md"}}`
	writeSpool(t, dir, dialogsSubdir, "run_pl", plan)
	d, ok = p.pendingDialog("run_pl", dir, "")
	if !ok || !d.Answerable || d.Kind != provider.DialogKindPlan {
		t.Fatalf("plan spool = %+v,%v; want an answerable plan dialog", d, ok)
	}
	if len(d.Options) != 4 || !d.Options[3].IsOther {
		t.Errorf("plan options = %+v; want the four pinned picker rows with the feedback row last", d.Options)
	}
}

func TestPendingDialog_missAndGarbage(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	if _, ok := p.pendingDialog("run_missing", dir, ""); ok {
		t.Error("no spool → want false")
	}
	writeSpool(t, dir, dialogsSubdir, "run_g", "{not json")
	if _, ok := p.pendingDialog("run_g", dir, ""); ok {
		t.Error("garbage spool → want false")
	}
	// A non-interactive tool name is not a dialog.
	writeSpool(t, dir, dialogsSubdir, "run_b", `{"tool_name":"Bash","tool_use_id":"x","tool_input":{}}`)
	if _, ok := p.pendingDialog("run_b", dir, ""); ok {
		t.Error("non-dialog tool → want false")
	}
}

// After a /clear (or /rewind) re-points the run at a fresh transcript (issue
// #34), a pre-rotation dialog spool does NOT self-heal via the tool-id check
// (its tool_use_id is absent from the new file). The session-identity check
// suppresses it — the new file's sessionId differs from the one the spool was
// captured against — so it cannot lock the composer against the new session,
// regardless of which file happens to be newer on disk.
func TestPendingDialog_staleAfterTranscriptRotation(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	spool := dialogSpoolPath(dir, "run_1")
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	writeFileWithModTime(t, spool, prePayload, t0)

	// Same session → the dialog shows.
	same := filepath.Join(dir, "sess-live.jsonl")
	writeFileWithModTime(t, same, "{}", t0.Add(-time.Minute))
	if _, ok := p.pendingDialog("run_1", dir, same); !ok {
		t.Error("a spool for the current session must show the dialog")
	}

	// Rotation: /clear created a brand-new transcript (fresh sessionId). The old
	// spool is stale and must be suppressed even though its tool_use_id is not in
	// the new file — whether the new file is newer OR older on disk.
	for _, mod := range []time.Time{t0.Add(time.Minute), t0.Add(-time.Minute)} {
		rotated := filepath.Join(dir, "sess-rotated.jsonl")
		writeFileWithModTime(t, rotated, "{}", mod)
		if _, ok := p.pendingDialog("run_1", dir, rotated); ok {
			t.Errorf("a spool for a rotated-out session must be suppressed as stale (transcript mtime %v)", mod)
		}
	}
}

// A spool payload with no session identity (not a shape 2.1.198 emits — the
// degraded fallback) keeps the old mtime backstop: a transcript newer than the
// spool suppresses, an older one serves.
func TestPendingDialog_legacyPayloadFallsBackToMtime(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	spool := dialogSpoolPath(dir, "run_1")
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_use_id":"toolu_ABC","tool_input":{"questions":[{"question":"Pick one?","header":"H","multiSelect":false,"options":[{"label":"A"},{"label":"B"}]}]}}`
	t0 := time.Now().Add(-time.Hour)
	writeFileWithModTime(t, spool, legacy, t0)

	older := filepath.Join(dir, "older.jsonl")
	writeFileWithModTime(t, older, "{}", t0.Add(-time.Minute))
	if _, ok := p.pendingDialog("run_1", dir, older); !ok {
		t.Error("legacy payload + older transcript → served")
	}
	newer := filepath.Join(dir, "newer.jsonl")
	writeFileWithModTime(t, newer, "{}", t0.Add(time.Minute))
	if _, ok := p.pendingDialog("run_1", dir, newer); ok {
		t.Error("legacy payload + newer transcript → suppressed (mtime backstop)")
	}
}

func TestBlockedState_markerAndStaleness(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, stateSubdir, "run_1", `{"notification_type":"permission_prompt"}`)

	// No transcript → the marker is current.
	if st, ok := p.blockedState("run_1", dir, ""); !ok || st != provider.StateNeedsInput {
		t.Fatalf("blockedState = (%q,%v); want (needs_input,true)", st, ok)
	}

	marker := markerPath(dir, "run_1")
	mi, _ := os.Stat(marker)

	// A transcript written BEFORE the marker → still blocked.
	older := filepath.Join(dir, "older.jsonl")
	writeFileWithModTime(t, older, "x", mi.ModTime().Add(-time.Hour))
	if _, ok := p.blockedState("run_1", dir, older); !ok {
		t.Error("transcript older than the marker → still blocked")
	}

	// A transcript written AFTER the marker → the block resolved (next activity).
	newer := filepath.Join(dir, "newer.jsonl")
	writeFileWithModTime(t, newer, "x", mi.ModTime().Add(time.Hour))
	if _, ok := p.blockedState("run_1", dir, newer); ok {
		t.Error("transcript newer than the marker → stale, want not blocked")
	}
}

func TestSpoolSig_changesWithFiles(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	if p.SpoolSig("run_1", dir) != "" {
		t.Error("no spool/marker → empty sig")
	}
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	sig := p.SpoolSig("run_1", dir)
	if sig == "" {
		t.Fatal("spool present → non-empty sig")
	}
	writeSpool(t, dir, stateSubdir, "run_1", `{"notification_type":"idle_prompt"}`)
	if p.SpoolSig("run_1", dir) == sig {
		t.Error("adding a marker must change the sig")
	}
}

// The generated hook commands are run through a REAL /bin/sh with the Appendix
// PreToolUse payload on stdin, exactly as Claude Code would invoke them — the
// end-to-end check that shell quoting + the atomic temp/rename + the read path
// all agree (unit tests exercise the read side alone). Then the PostToolUse
// command is run to confirm it clears the spool.
func TestHookCommands_endToEndThroughSh(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	p := spoolTestProvider(t)
	dir := t.TempDir()
	settings, _, _ := p.Setup("run_e2e", dir, provider.SetupOpts{})

	var s hookSettings
	if err := json.Unmarshal(settings, &s); err != nil {
		t.Fatal(err)
	}
	runSh := func(command, stdin string) {
		t.Helper()
		cmd := exec.Command("sh", "-c", command)
		cmd.Stdin = strings.NewReader(stdin)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh %q: %v\n%s", command, err, out)
		}
	}

	// PreToolUse: feed the real payload; the spool must appear and read back.
	runSh(s.Hooks.PreToolUse[0].Hooks[0].Command, prePayload)
	if _, err := os.Stat(dialogSpoolPath(dir, "run_e2e")); err != nil {
		t.Fatalf("PreToolUse hook did not spool: %v", err)
	}
	d, ok := p.pendingDialog("run_e2e", dir, "")
	if !ok || d.ToolID != "toolu_ABC" || !d.Answerable {
		t.Fatalf("spooled-then-read dialog = %+v,%v; want an answerable dialog toolu_ABC", d, ok)
	}

	// No stray temp file left behind by the atomic write.
	if _, err := os.Stat(dialogSpoolPath(dir, "run_e2e") + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("atomic-write temp file left behind: %v", err)
	}

	// PostToolUse clears the spool.
	runSh(s.Hooks.PostToolUse[0].Hooks[0].Command, "")
	if _, err := os.Stat(dialogSpoolPath(dir, "run_e2e")); !os.IsNotExist(err) {
		t.Errorf("PostToolUse hook did not clear the spool: %v", err)
	}

	// Notification: the marker spools and drives blockedState.
	runSh(s.Hooks.Notification[0].Hooks[0].Command, `{"notification_type":"permission_prompt"}`)
	if st, ok := p.blockedState("run_e2e", dir, ""); !ok || st != provider.StateNeedsInput {
		t.Errorf("Notification hook marker = (%q,%v); want (needs_input,true)", st, ok)
	}
}

// SweepSpools and its tests are gone (issue #205): every file this adapter
// spools lives in the run's private runtime dir, wiped with the run's tree
// (instancehome.Wipe/SweepAll) — including crash-orphaned .tmp siblings.
