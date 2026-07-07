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
const prePayload = `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","session_id":"s","transcript_path":"t","cwd":"c","permission_mode":"auto","tool_use_id":"toolu_ABC","tool_input":{"questions":[{"question":"Pick one?","header":"H","multiSelect":false,"options":[{"label":"A","description":"da"},{"label":"B","description":"db"}]}]}}`

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

func TestHookSettings_shapeAndArgs(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	settings, settingsPath, args := p.HookSettings("run_1", dir)

	if settingsPath != filepath.Join(dir, "settings.run_1.json") {
		t.Errorf("settingsPath = %q", settingsPath)
	}
	if len(args) != 2 || args[0] != "--settings" || args[1] != settingsPath {
		t.Errorf("args = %v; want [--settings %s]", args, settingsPath)
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

func TestPendingDialog_readsAndSuppressesResolved(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)

	// No transcript → the dialog is live.
	d, ok := p.PendingDialog("run_1", dir, "")
	if !ok {
		t.Fatal("PendingDialog: want a live dialog")
	}
	if d.ToolID != "toolu_ABC" || !d.Answerable || len(d.Options) != 3 { // A, B, Other
		t.Fatalf("dialog = %+v; want answerable A/B/Other with ToolID toolu_ABC", d)
	}

	// Per compat §5 the transcript is byte-frozen BEFORE a genuine pending dialog
	// opens, so it is always older than the spool. Write these fixtures older than
	// the spool to model that (and to isolate the tool-id logic from the mtime
	// staleness backstop, which only fires on a transcript NEWER than the spool —
	// a rotation, exercised by TestPendingDialog_staleAfterTranscriptRotation).
	si, err := os.Stat(dialogSpoolPath(dir, "run_1"))
	if err != nil {
		t.Fatal(err)
	}
	frozen := si.ModTime().Add(-time.Minute)

	// A transcript that already contains the tool_use_id (retro-flushed) →
	// resolved → suppressed.
	transcript := filepath.Join(dir, "t.jsonl")
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ABC","name":"AskUserQuestion","input":{}}]}}` + "\n"
	writeFileWithModTime(t, transcript, line, frozen)
	if _, ok := p.PendingDialog("run_1", dir, transcript); ok {
		t.Error("PendingDialog: want suppressed once the tool_use_id is in the transcript")
	}

	// A different tool_use_id in the transcript does NOT suppress.
	other := filepath.Join(dir, "other.jsonl")
	writeFileWithModTime(t, other, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ZZZ","name":"Bash","input":{}}]}}`+"\n", frozen)
	if _, ok := p.PendingDialog("run_1", dir, other); !ok {
		t.Error("PendingDialog: an unrelated tool_use_id must not suppress the dialog")
	}
}

func TestPendingDialog_missAndGarbage(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	if _, ok := p.PendingDialog("run_missing", dir, ""); ok {
		t.Error("no spool → want false")
	}
	writeSpool(t, dir, dialogsSubdir, "run_g", "{not json")
	if _, ok := p.PendingDialog("run_g", dir, ""); ok {
		t.Error("garbage spool → want false")
	}
	// A non-interactive tool name is not a dialog.
	writeSpool(t, dir, dialogsSubdir, "run_b", `{"tool_name":"Bash","tool_use_id":"x","tool_input":{}}`)
	if _, ok := p.PendingDialog("run_b", dir, ""); ok {
		t.Error("non-dialog tool → want false")
	}
}

// After a /clear (or /rewind) re-points the run at a fresh transcript (issue
// #34), a pre-rotation dialog spool does NOT self-heal via the tool-id check
// (its tool_use_id is absent from the new file). The mtime staleness backstop
// suppresses it so it cannot lock the composer against the new session — while a
// spool newer than the byte-frozen transcript (the genuine pending case) still
// shows.
func TestPendingDialog_staleAfterTranscriptRotation(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	spool := dialogSpoolPath(dir, "run_1")
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	writeFileWithModTime(t, spool, prePayload, t0)

	// Genuine pending: the transcript is byte-frozen from BEFORE the dialog (§5),
	// so it is older than the spool → the dialog still shows. (Its content does
	// not carry the spool's tool_use_id, so only the mtime rule is in play.)
	frozen := filepath.Join(dir, "frozen.jsonl")
	writeFileWithModTime(t, frozen, "{}", t0.Add(-time.Minute))
	if _, ok := p.PendingDialog("run_1", dir, frozen); !ok {
		t.Error("a spool newer than the byte-frozen transcript must still show the dialog")
	}

	// Rotation: /clear created a brand-new transcript AFTER the spool. The old
	// spool is stale and must be suppressed even though its tool_use_id is not in
	// the new file.
	rotated := filepath.Join(dir, "rotated.jsonl")
	writeFileWithModTime(t, rotated, "{}", t0.Add(time.Minute))
	if _, ok := p.PendingDialog("run_1", dir, rotated); ok {
		t.Error("a spool older than the rotated transcript must be suppressed as stale")
	}
}

func TestBlockedState_markerAndStaleness(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, stateSubdir, "run_1", `{"notification_type":"permission_prompt"}`)

	// No transcript → the marker is current.
	if st, ok := p.BlockedState("run_1", dir, ""); !ok || st != provider.StateNeedsInput {
		t.Fatalf("BlockedState = (%q,%v); want (needs_input,true)", st, ok)
	}

	marker := markerPath(dir, "run_1")
	mi, _ := os.Stat(marker)

	// A transcript written BEFORE the marker → still blocked.
	older := filepath.Join(dir, "older.jsonl")
	writeFileWithModTime(t, older, "x", mi.ModTime().Add(-time.Hour))
	if _, ok := p.BlockedState("run_1", dir, older); !ok {
		t.Error("transcript older than the marker → still blocked")
	}

	// A transcript written AFTER the marker → the block resolved (next activity).
	newer := filepath.Join(dir, "newer.jsonl")
	writeFileWithModTime(t, newer, "x", mi.ModTime().Add(time.Hour))
	if _, ok := p.BlockedState("run_1", dir, newer); ok {
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
	settings, _, _ := p.HookSettings("run_e2e", dir)

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
	d, ok := p.PendingDialog("run_e2e", dir, "")
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

	// Notification: the marker spools and drives BlockedState.
	runSh(s.Hooks.Notification[0].Hooks[0].Command, `{"notification_type":"permission_prompt"}`)
	if st, ok := p.BlockedState("run_e2e", dir, ""); !ok || st != provider.StateNeedsInput {
		t.Errorf("Notification hook marker = (%q,%v); want (needs_input,true)", st, ok)
	}
}

func TestSweepSpools_GCsInactiveRuns(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	// Two runs' full triplet of files.
	for _, r := range []string{"run_keep", "run_drop"} {
		writeSpool(t, dir, dialogsSubdir, r, prePayload)
		writeSpool(t, dir, stateSubdir, r, `{"notification_type":"idle_prompt"}`)
		if err := os.WriteFile(settingsFilePath(dir, r), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.SweepSpools(dir, func(runID string) bool { return runID == "run_keep" }); err != nil {
		t.Fatalf("SweepSpools: %v", err)
	}
	// run_keep survives; run_drop is gone (all three files).
	for _, path := range []string{dialogSpoolPath(dir, "run_keep"), markerPath(dir, "run_keep"), settingsFilePath(dir, "run_keep")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("kept run file missing: %s", path)
		}
	}
	for _, path := range []string{dialogSpoolPath(dir, "run_drop"), markerPath(dir, "run_drop"), settingsFilePath(dir, "run_drop")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("dropped run file still present: %s (%v)", path, err)
		}
	}
}

// A hook (or lab) killed between the atomic temp-write and the rename orphans a
// .tmp sibling; SweepSpools reaps it once aged past spoolTempMaxAge, but leaves
// a fresh one (a possibly in-flight write) alone.
func TestSweepSpools_reapsStaleTempOrphans(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	stale := time.Now().Add(-spoolTempMaxAge - time.Minute)

	// Aged orphans: a hook temp in dialogs/ and a settings temp in the root.
	if err := os.MkdirAll(filepath.Join(dir, dialogsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	staleDialogTmp := dialogSpoolPath(dir, "run_x") + ".tmp"
	staleSettingsTmp := filepath.Join(dir, settingsTempPrefix+"abc123")
	writeFileWithModTime(t, staleDialogTmp, "partial", stale)
	writeFileWithModTime(t, staleSettingsTmp, "partial", stale)
	// A fresh temp (mid-write) must be left alone.
	freshTmp := markerPath(dir, "run_y") + ".tmp"
	if err := os.MkdirAll(filepath.Join(dir, stateSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshTmp, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.SweepSpools(dir, func(string) bool { return false }); err != nil {
		t.Fatalf("SweepSpools: %v", err)
	}
	for _, path := range []string{staleDialogTmp, staleSettingsTmp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale temp orphan not reaped: %s", path)
		}
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Errorf("fresh temp (possibly in-flight) wrongly reaped: %s (%v)", freshTmp, err)
	}
}
