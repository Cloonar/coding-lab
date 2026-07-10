package claudecode

// The hook-spool coupling (issue #17 / ADR-0020): Claude Code does NOT write a
// pending AskUserQuestion / ExitPlanMode tool_use to the session JSONL while it
// is pending — the tool_use and its tool_result are flushed together,
// retroactively, only when the dialog resolves (compat §5). So a pending dialog
// is invisible in the transcript live. This file feeds the pending dialog from
// a source that exists *while the question is open*: a per-run PreToolUse hook
// spools the exact structured tool_input to a runtime file the moment the
// picker is about to show. The same mapper that reads the transcript
// (dialogFromToolUse) maps the spooled tool_input — one mapper, two sources.
//
// Every exact string here is a fragile Claude Code coupling pinned in
// internal/compat §9 (the hook payload shapes + the spool protocol), live-
// verified against 2.1.198.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

var _ provider.LiveSignals = (*Provider)(nil)

// spoolTempMaxAge is how old an atomic-write temp sibling (`<runID>.json.tmp`
// from a hook, or `.settings.tmp-*` from lab's own write) must be before
// SweepSpools reaps it as a crash orphan — a hook (external sh) or lab killed
// between the temp write and the rename leaves one behind. The age guard keeps
// the sweep from racing an in-flight write (mirrors vault.staleTempMaxAge).
const spoolTempMaxAge = 2 * time.Minute

// settingsTempPrefix is the prefix of lab's per-run settings atomic-write temp
// (instance.writeFileAtomic0600 uses os.CreateTemp(dir, settingsTempPrefix+"*")).
// Kept here so SweepSpools recognises the orphan; the two must stay in sync.
const settingsTempPrefix = ".settings.tmp-"

// Runtime spool layout under lab's runtime dir (lab-owned; the provider owns
// the per-run names). One dialog spool and one marker per run — only one dialog
// can be pending per session, so a single overwritten file per run suffices.
const (
	dialogsSubdir  = "dialogs" // <dir>/dialogs/<runID>.json  — the PreToolUse dialog spool
	stateSubdir    = "state"   // <dir>/state/<runID>.json    — the Notification blocked marker
	spoolExt       = ".json"
	settingsPrefix = "settings." // <dir>/settings.<runID>.json — the per-run --settings file
)

// dialogMatcher is the PreToolUse/PostToolUse matcher: the two interactive
// tools dialogFromToolUse recognises, as a tmux-free regex alternation
// (compat §9). Kept in lockstep with toolAskUserQuestion/toolExitPlanMode.
const dialogMatcher = toolAskUserQuestion + "|" + toolExitPlanMode

func dialogSpoolPath(dir, runID string) string {
	return filepath.Join(dir, dialogsSubdir, runID+spoolExt)
}

func markerPath(dir, runID string) string {
	return filepath.Join(dir, stateSubdir, runID+spoolExt)
}

func settingsFilePath(dir, runID string) string {
	return filepath.Join(dir, settingsPrefix+runID+spoolExt)
}

// hookSettings is the Claude Code settings.json subset lab injects per run
// (compat §9). Only the hooks block is set; --settings merges it additively
// over the repo-shipped .claude settings, so nothing else is disturbed.
type hookSettings struct {
	Hooks hookGroups `json:"hooks"`
}

type hookGroups struct {
	PreToolUse   []hookMatcher `json:"PreToolUse,omitempty"`
	PostToolUse  []hookMatcher `json:"PostToolUse,omitempty"`
	Notification []hookMatcher `json:"Notification,omitempty"`
}

type hookMatcher struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []hookCmd `json:"hooks"`
}

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// Setup implements provider.LiveSignals: the per-run settings file that
// arms the three dialog-capture hooks, plus the --settings flag pointing at it.
// The hook commands self-create their spool subdirs (mkdir -p) so a dialog that
// opens after a lab restart still spools with no re-arming.
//
//   - PreToolUse (AskUserQuestion|ExitPlanMode): atomic-write stdin (the exact
//     hook payload) to the dialog spool. Purely observational — it exits 0 with
//     no output, so the picker still shows for the local operator and the tool
//     is never blocked (compat §9: only a PreToolUse exit code 2 blocks).
//   - PostToolUse (same matcher): delete the dialog spool the instant the
//     question resolves by any route (TUI, chat send-keys, or claude.ai).
//   - Notification: atomic-write stdin (carrying notification_type) to the
//     blocked marker, so residual blocked states drive the badge (decision 7).
func (p *Provider) Setup(runID, dir string) (settings []byte, settingsPath string, args []string) {
	spool := dialogSpoolPath(dir, runID)
	marker := markerPath(dir, runID)
	s := hookSettings{Hooks: hookGroups{
		PreToolUse: []hookMatcher{{
			Matcher: dialogMatcher,
			Hooks:   []hookCmd{{Type: "command", Command: atomicWriteCmd(spool)}},
		}},
		PostToolUse: []hookMatcher{{
			Matcher: dialogMatcher,
			Hooks:   []hookCmd{{Type: "command", Command: removeCmd(spool)}},
		}},
		Notification: []hookMatcher{{
			Hooks: []hookCmd{{Type: "command", Command: atomicWriteCmd(marker)}},
		}},
	}}
	// Indented so an operator inspecting the runtime file can read it; the
	// content is machine-generated and never re-parsed by lab.
	b, _ := json.MarshalIndent(s, "", "  ")
	settingsPath = settingsFilePath(dir, runID)
	return b, settingsPath, []string{"--settings", settingsPath}
}

// atomicWriteCmd renders the POSIX-sh command that atomically writes the hook's
// stdin to path: mkdir -p the parent, cat stdin to a temp sibling, rename over
// path. Rename is atomic within a directory, so a reader (lab's tailer) never
// sees a half-written spool. Best-effort: on any failure it still exits
// non-zero-but-not-2, which Claude Code treats as a non-blocking hook error
// (compat §9) — the spool simply isn't updated, the agent is never impeded.
func atomicWriteCmd(path string) string {
	q := shellSingleQuote(path)
	dir := shellSingleQuote(filepath.Dir(path))
	// Fixed .tmp sibling: dialogs are serial per session (one pending at a
	// time), so there is never a concurrent writer to this run's spool.
	return "mkdir -p " + dir + " && cat > " + q + ".tmp && mv " + q + ".tmp " + q
}

// removeCmd renders `rm -f <path>` — the PostToolUse spool clear.
func removeCmd(path string) string {
	return "rm -f " + shellSingleQuote(path)
}

// shellSingleQuote single-quotes s for /bin/sh, escaping any embedded single
// quote as '\” so the value reaches the command verbatim. (vault.shellQuote's
// sibling, kept local to the claude coupling.)
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// spooledTool is the PreToolUse payload subset the mapper needs (compat §9).
// tool_input is the exact structured input the mapper also reads from a
// transcript tool_use block, so dialogFromToolUse handles both sources.
// session_id/transcript_path identify the session the dialog was captured
// against — the rotation-staleness key (a /clear or /rewind starts a new
// sessionId, so a spool naming the old one is stale).
type spooledTool struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// DialogFromHookPayload maps a raw PreToolUse hook payload to a Dialog, the
// pure mapper behind PendingDialog. It reports false when the payload is not a
// recognised interactive dialog. Exported for the compat fixture test (its
// sibling is ParseTranscript for the transcript source — one mapper, two
// sources).
func DialogFromHookPayload(payload []byte) (provider.Dialog, bool) {
	var s spooledTool
	if json.Unmarshal(payload, &s) != nil || s.ToolUseID == "" {
		return provider.Dialog{}, false
	}
	return dialogFromToolUse(tBlock{Name: s.ToolName, ID: s.ToolUseID, Input: s.ToolInput})
}

// PendingDialog implements provider.LiveSignals: read the dialog spool, map it
// through the shared mapper, and suppress it once resolved or stale. A spool
// whose tool_use_id is already present in the transcript is answered (the
// tool_use is flushed only on resolution) — return false so a stale spool never
// re-opens an answered picker.
func (p *Provider) PendingDialog(runID, dir, transcriptPath string) (provider.Dialog, bool) {
	spool := dialogSpoolPath(dir, runID)
	si, err := os.Stat(spool)
	if err != nil {
		return provider.Dialog{}, false
	}
	b, err := os.ReadFile(spool)
	if err != nil {
		return provider.Dialog{}, false
	}
	var st spooledTool
	if json.Unmarshal(b, &st) != nil {
		return provider.Dialog{}, false
	}
	d, ok := DialogFromHookPayload(b)
	if !ok {
		return provider.Dialog{}, false
	}
	// d.ToolID carries the spool's tool_use_id (dialogFromToolUse copies it);
	// its presence in the transcript means the retro-flush landed → resolved.
	// This is the primary check (the PostToolUse hook is the primary clear).
	if transcriptPath != "" && d.ToolID != "" && toolIDInTranscript(transcriptPath, d.ToolID) {
		return provider.Dialog{}, false
	}
	// Rotation staleness (issue #34): a spool captured against a rotated-out
	// session (a /clear or /rewind started a new sessionId → new file) must not
	// keep the composer locked against the fresh session. Keyed to the SESSION
	// IDENTITY the hook payload names, never to file mtimes: the transcript is
	// NOT byte-frozen during a pending window — Claude Code appends
	// queue-operation/attachment entries live when the operator queues a message
	// while the picker is up (compat §5) — so an mtime comparison hides a
	// genuinely pending dialog. A payload with no session identity (not a shape
	// 2.1.198 emits) degrades to the old mtime backstop rather than never
	// going stale.
	if transcriptPath != "" {
		if sid := spoolSessionID(st); sid != "" {
			if sid != transcriptSessionID(transcriptPath) {
				return provider.Dialog{}, false
			}
		} else if ti, err := os.Stat(transcriptPath); err == nil && ti.ModTime().After(si.ModTime()) {
			return provider.Dialog{}, false
		}
	}
	return d, true
}

// spoolSessionID is the session identity a PreToolUse payload was captured
// against: session_id when present, else the transcript_path's stem. "" when
// the payload carries neither.
func spoolSessionID(s spooledTool) string {
	if s.SessionID != "" {
		return s.SessionID
	}
	return transcriptSessionID(s.TranscriptPath)
}

// transcriptSessionID extracts the sessionId from a transcript path — the
// filename stem (<sessionId>.jsonl, compat §5). "" in → "" out.
func transcriptSessionID(path string) string {
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// spooledNotification is the Notification payload subset the marker carries.
type spooledNotification struct {
	NotificationType string `json:"notification_type"`
}

// BlockedState implements provider.LiveSignals: a live Notification marker maps
// any blocked notification_type to StateNeedsInput. The marker is stale — the
// block resolved — once the transcript is written after it (next activity), so
// a transcript mtime past the marker mtime suppresses it.
func (p *Provider) BlockedState(runID, dir, transcriptPath string) (string, bool) {
	mi, err := os.Stat(markerPath(dir, runID))
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(markerPath(dir, runID))
	if err != nil {
		return "", false
	}
	var n spooledNotification
	if json.Unmarshal(b, &n) != nil || n.NotificationType == "" {
		return "", false
	}
	if transcriptPath != "" {
		if ti, err := os.Stat(transcriptPath); err == nil && ti.ModTime().After(mi.ModTime()) {
			return "", false // the transcript advanced past the block
		}
	}
	return provider.StateNeedsInput, true
}

// SpoolSig implements provider.LiveSignals: a cheap existence+mtime+size digest
// of the dialog spool and the marker, so the tailer republishes when a dialog
// appears while the transcript stays byte-frozen. "" when neither file exists.
func (p *Provider) SpoolSig(runID, dir string) string {
	var b strings.Builder
	for _, path := range []string{dialogSpoolPath(dir, runID), markerPath(dir, runID)} {
		if fi, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", filepath.Base(path), fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return b.String()
}

// SweepSpools implements provider.LiveSignals: remove the dialog spool, the
// marker, and the per-run settings file for every run whose keep(runID) is
// false, plus any stale atomic-write temp orphan. Enumerates by directory so an
// orphan from a crashed run (whose row is gone) is still reaped. Missing files
// are not an error.
func (p *Provider) SweepSpools(dir string, keep func(runID string) bool) error {
	var errs []error
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	// reapTemp removes an atomic-write temp sibling once it has aged past
	// spoolTempMaxAge (a hook or lab killed between the temp write and the
	// rename); the age guard never races an in-flight write.
	reapTemp := func(path string, e os.DirEntry) {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < spoolTempMaxAge {
			return
		}
		remove(path)
	}

	// dialogs/ and state/ spools: <runID>.json, plus <runID>.json.tmp orphans.
	for _, sub := range []string{dialogsSubdir, stateSubdir} {
		subdir := filepath.Join(dir, sub)
		entries, err := os.ReadDir(subdir)
		if err != nil {
			continue // subdir not created yet — nothing to sweep
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(name, ".tmp") {
				reapTemp(filepath.Join(subdir, name), e)
				continue
			}
			if !strings.HasSuffix(name, spoolExt) {
				continue
			}
			runID := strings.TrimSuffix(name, spoolExt)
			if keep != nil && keep(runID) {
				continue
			}
			remove(filepath.Join(subdir, name))
		}
	}

	// settings.<runID>.json directly under dir, plus .settings.tmp-* orphans.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return joinErrs(errs)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, settingsTempPrefix) {
			reapTemp(filepath.Join(dir, name), e)
			continue
		}
		if !strings.HasPrefix(name, settingsPrefix) || !strings.HasSuffix(name, spoolExt) {
			continue
		}
		runID := strings.TrimSuffix(strings.TrimPrefix(name, settingsPrefix), spoolExt)
		if keep != nil && keep(runID) {
			continue
		}
		remove(filepath.Join(dir, name))
	}
	return joinErrs(errs)
}

// toolIDInTranscript reports whether a tool_use or tool_result block carrying
// id appears anywhere in the transcript at path. The tool_use of a pending
// dialog is flushed only on resolution (compat §5), so a hit means the dialog
// is answered. A read error is "not present" — the PostToolUse hook is the
// primary spool clear, this scan is only the backstop.
func toolIDInTranscript(path, id string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		// Cheap pre-filter: the id must appear as a substring before we pay for
		// a JSON parse (transcripts are large; a pending window is rare).
		if !strings.Contains(sc.Text(), id) {
			continue
		}
		var it tItem
		if json.Unmarshal(sc.Bytes(), &it) != nil || it.Message == nil {
			continue
		}
		for _, blk := range it.Message.blocks() {
			if (blk.Type == "tool_use" && blk.ID == id) ||
				(blk.Type == "tool_result" && blk.ToolUseID == id) {
				return true
			}
		}
	}
	return false
}

func joinErrs(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("sweep spools: %d errors: %v", len(errs), errs)
	}
}
