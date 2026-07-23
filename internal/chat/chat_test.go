package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

func newService(t *testing.T) (*Service, *store.Store, *providertest.Fake, *events.Bus) {
	t.Helper()
	st := testutil.TempStore(t)
	fake := providertest.New()
	reg, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	// Per-run runtime dirs (issue #205), shaped like production's
	// instancehome.RuntimePath — the seam-args tests re-derive the same path
	// through svc.runtimeDir(runID).
	base := t.TempDir()
	svc, err := New(Options{Store: st, Providers: reg, Bus: bus, Logger: logx.New(io.Discard),
		Poll:          5 * time.Millisecond,
		RuntimeDirFor: func(runID string) string { return filepath.Join(base, runID, "runtime") }})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, fake, bus
}

func seedRun(t *testing.T, st *store.Store, outcome string) store.Run {
	t.Helper()
	repo, err := st.CreateRepo(context.Background(), store.Repo{
		ID: "repo1", Name: "proj", RemoteURL: "file:///x", TrackerBinding: store.TrackerBindingBuiltin,
		ForgeKind: "none", DefaultBranch: "main", AFKBranchPattern: "afk/<N>",
		ManualBranchPrefix: "lab/", CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(context.Background(), store.Run{
		ID: "run1", RepoID: repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/x", WorktreePath: "/wt/x", SessionName: "proj~x", StartedAt: time.Now(),
		Outcome: outcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRead_persistsLocatedPath(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "user", Text: "hi"}}})

	chat, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if chat.State != provider.StateWorking || len(chat.Messages) != 1 {
		t.Errorf("chat = %+v; want one working message", chat)
	}
	// The located path was persisted so ended runs stay readable.
	got, err := st.RunByID(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TranscriptPath == nil || *got.TranscriptPath != "/transcript.jsonl" {
		t.Errorf("persisted transcript_path = %v; want /transcript.jsonl", got.TranscriptPath)
	}
}

func TestRead_reLocatesOnRotation(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/a.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "old"}}})

	v1, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read A: %v", err)
	}
	idA := v1.TranscriptID
	if idA == "" {
		t.Fatal("TranscriptID empty for a located transcript")
	}
	// Re-read the row so the persisted path (A) is in hand, exactly as httpapi
	// does per request.
	run, _ = st.RunByID(context.Background(), run.ID)

	// /clear rotates: a DIFFERENT, non-empty file with fresh (cleared) content.
	fake.SetTranscriptPath("/b.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateNeedsInput, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "new"}}})

	v2, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read B: %v", err)
	}
	if len(v2.Messages) != 1 || v2.Messages[0].Text != "new" {
		t.Errorf("messages = %+v; want the fresh transcript's 'new'", v2.Messages)
	}
	if v2.TranscriptID == "" || v2.TranscriptID == idA {
		t.Errorf("TranscriptID = %q; want a new non-empty id after rotation (was %q)", v2.TranscriptID, idA)
	}
	// Re-pointed and persisted (single value — re-point & forget).
	got, _ := st.RunByID(context.Background(), run.ID)
	if got.TranscriptPath == nil || *got.TranscriptPath != "/b.jsonl" {
		t.Errorf("persisted path = %v; want /b.jsonl", got.TranscriptPath)
	}
}

func TestRead_emptyLocateKeepsPath(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	// Path A persisted from an earlier live read.
	if err := st.UpdateRunTranscriptPath(context.Background(), run.ID, "/a.jsonl"); err != nil {
		t.Fatal(err)
	}
	run, _ = st.RunByID(context.Background(), run.ID)
	fake.SetChat(provider.Chat{State: provider.StateWorking,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "a"}}})
	// A transient locate gap (the process is momentarily absent from the
	// registry / between states) must NEVER clobber a path already in hand.
	fake.SetTranscriptPath("")

	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.TranscriptID != transcriptID("/a.jsonl") {
		t.Errorf("TranscriptID = %q; want the id of the kept path A", v.TranscriptID)
	}
	got, _ := st.RunByID(context.Background(), run.ID)
	if got.TranscriptPath == nil || *got.TranscriptPath != "/a.jsonl" {
		t.Errorf("persisted path = %v; want /a.jsonl unchanged", got.TranscriptPath)
	}
}

func TestRead_endedRunIgnoresRotation(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeStopped)
	if err := st.UpdateRunTranscriptPath(context.Background(), run.ID, "/a.jsonl"); err != nil {
		t.Fatal(err)
	}
	run, _ = st.RunByID(context.Background(), run.ID)
	fake.SetChat(provider.Chat{State: provider.StateNeedsInput})
	// A live successor's transcript is locatable in the reused worktree — an
	// ended run must ignore it entirely (never re-locate, never re-point).
	fake.SetTranscriptPath("/successor.jsonl")

	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.State != provider.StateEnded {
		t.Errorf("state = %q; want ended", v.State)
	}
	if n := fake.LocateCount(); n != 0 {
		t.Errorf("LocateTranscript ran %d times for an ended run; want 0", n)
	}
	if v.TranscriptID != transcriptID("/a.jsonl") {
		t.Errorf("TranscriptID = %q; want the id of the persisted A (no re-locate)", v.TranscriptID)
	}
	got, _ := st.RunByID(context.Background(), run.ID)
	if got.TranscriptPath == nil || *got.TranscriptPath != "/a.jsonl" {
		t.Errorf("persisted path = %v; want /a.jsonl (never re-pointed)", got.TranscriptPath)
	}
}

// locateActive must NOT persist a rotation for a run that has since ended, even
// when the caller holds a stale ACTIVE snapshot — the tailer can outlive its
// run's end by up to one resync interval (a dropped run.changed), and
// LocateTranscript is a pure cwd-match, so a successor run reusing the worktree
// would otherwise be adopted onto the ended run's row (successor-safety).
func TestLocateActive_skipsPersistForEndedRun(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	if err := st.UpdateRunTranscriptPath(context.Background(), run.ID, "/a.jsonl"); err != nil {
		t.Fatal(err)
	}
	// The caller's snapshot still says active (as the tailer's would), but the
	// run has since ended and its worktree now hosts a successor's transcript.
	staleActive, _ := st.RunByID(context.Background(), run.ID)
	if err := st.EndRun(context.Background(), run.ID, store.RunOutcomeStopped, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	fake.SetTranscriptPath("/successor.jsonl")
	prov, _ := svc.providers.Get(run.Provider)

	got := svc.locateActive(context.Background(), prov, staleActive, "/a.jsonl")
	if got != "/a.jsonl" {
		t.Errorf("locateActive adopted %q for a since-ended run; want the known path /a.jsonl", got)
	}
	row, _ := st.RunByID(context.Background(), run.ID)
	if row.TranscriptPath == nil || *row.TranscriptPath != "/a.jsonl" {
		t.Errorf("persisted path = %v; want /a.jsonl (a successor must never be adopted onto an ended run)", row.TranscriptPath)
	}
}

// Issue #202: locateActive resolves the transcript strictly under the run's
// private instance HOME — the injected HomeFor closure's value for the run id
// reaches the provider's LocateTranscript, so a real per-run home is never
// bypassed for the master store.
func TestLocateActive_passesRunHomeToProvider(t *testing.T) {
	st := testutil.TempStore(t)
	fake := providertest.New()
	reg, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	homeFor := func(runID string) string { return "/state/instances/" + runID + "/home" }
	rtDir := t.TempDir()
	svc, err := New(Options{Store: st, Providers: reg, Bus: events.NewBus(),
		Logger: logx.New(io.Discard), Poll: 5 * time.Millisecond,
		RuntimeDirFor: func(string) string { return rtDir },
		HomeFor:       homeFor})
	if err != nil {
		t.Fatal(err)
	}
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	if _, err := svc.Read(context.Background(), run); err != nil {
		t.Fatalf("Read: %v", err)
	}
	homes := fake.LocateHomes()
	if len(homes) == 0 {
		t.Fatal("LocateTranscript never ran")
	}
	want := homeFor(run.ID)
	for i, got := range homes {
		if got != want {
			t.Errorf("LocateTranscript call %d home = %q, want the run's instance home %q", i, got, want)
		}
	}
}

func TestRead_endedRunOverridesState(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeStopped)
	// The path was persisted while the run was live (tailer/first read).
	if err := st.UpdateRunTranscriptPath(context.Background(), run.ID, "/transcript.jsonl"); err != nil {
		t.Fatal(err)
	}
	run, err := st.RunByID(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	chat, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if chat.State != provider.StateEnded {
		t.Errorf("ended run state = %q; want ended", chat.State)
	}
}

func TestRead_endedRunNeverLocates(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeStopped)
	// A live claude in the reused worktree would be locatable — but it belongs
	// to a newer run, so an ended run must neither read nor persist it.
	fake.SetTranscriptPath("/successor-run.jsonl")

	if _, err := svc.Read(context.Background(), run); !errors.Is(err, provider.ErrTranscriptGone) {
		t.Fatalf("Read ended run without a transcript = %v; want ErrTranscriptGone", err)
	}
	if n := fake.LocateCount(); n != 0 {
		t.Errorf("LocateTranscript ran %d times for an ended run; want 0", n)
	}
	got, err := st.RunByID(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TranscriptPath != nil {
		t.Errorf("transcript_path = %q; want unset (nothing may be persisted)", *got.TranscriptPath)
	}
}

func TestPendingDialog_requiresQuestionTail(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")

	// An abandoned dialog mid-transcript (conversation moved on) is history,
	// not a keystroke target.
	fake.SetChat(provider.Chat{State: provider.StateWorking, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{ToolID: "t_old"}},
		{Seq: 2, Kind: provider.MessageTool, Tool: &provider.ToolInfo{Name: "Bash", Status: "running"}},
	}})
	if _, ok := svc.PendingDialog(context.Background(), run); ok {
		t.Error("PendingDialog = true for an abandoned mid-transcript dialog; want false")
	}

	fake.SetChat(provider.Chat{State: provider.StateQuestion, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{ToolID: "t_live"}},
	}})
	d, ok := svc.PendingDialog(context.Background(), run)
	if !ok || d.ToolID != "t_live" {
		t.Errorf("PendingDialog = %+v, %v; want the tail dialog t_live", d, ok)
	}

	// An ANSWERED dialog (Outcome set, issue #56) is history, never a keystroke
	// target — the dormant-fallback scan must skip it, even at the tail.
	fake.SetChat(provider.Chat{State: provider.StateQuestion, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{ToolID: "t_done",
			Outcome: &provider.DialogOutcome{Approved: true}}},
	}})
	if d, ok := svc.PendingDialog(context.Background(), run); ok {
		t.Errorf("PendingDialog = %+v for an answered dialog; want none (must never re-target a resolved picker)", d)
	}
}

func TestAnswerDialog_toolIDGuard(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateQuestion, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{ToolID: "t1", Answerable: true}},
	}})

	if err := svc.AnswerDialog(context.Background(), run, "t_stale", provider.DialogAnswer{Index: 0}); !errors.Is(err, ErrDialogChanged) {
		t.Errorf("stale tool_id = %v; want ErrDialogChanged", err)
	}
	if err := svc.AnswerDialog(context.Background(), run, "t1", provider.DialogAnswer{Index: 0}); err != nil {
		t.Errorf("matching tool_id = %v; want nil", err)
	}
	if got := fake.Answers(); len(got) != 1 {
		t.Errorf("answers recorded = %d; want exactly the matching one", len(got))
	}

	fake.SetChat(provider.Chat{State: provider.StateWorking})
	if err := svc.AnswerDialog(context.Background(), run, "t1", provider.DialogAnswer{Index: 0}); !errors.Is(err, ErrNoDialog) {
		t.Errorf("no pending dialog = %v; want ErrNoDialog", err)
	}
}

func TestReply_guards(t *testing.T) {
	svc, st, fake, _ := newService(t)
	fake.SetTranscriptPath("/transcript.jsonl")

	ended := seedRunID(t, st, "run_ended", store.RunOutcomeStopped)
	if err := svc.Reply(context.Background(), ended, "hi"); err != ErrRunEnded {
		t.Errorf("reply to ended run = %v; want ErrRunEnded", err)
	}

	active := seedRunID(t, st, "run_active", store.RunOutcomeActive)
	fake.SetChat(provider.Chat{State: provider.StateQuestion,
		Messages: []provider.Message{{Kind: provider.MessageDialog, Dialog: &provider.Dialog{ToolID: "t1"}}}})
	if err := svc.Reply(context.Background(), active, "hi"); err != ErrDialogPending {
		t.Errorf("reply while dialog pending = %v; want ErrDialogPending", err)
	}

	fake.SetChat(provider.Chat{State: provider.StateWorking})
	if err := svc.Reply(context.Background(), active, "go on"); err != nil {
		t.Errorf("reply to working run = %v; want nil", err)
	}
	if got := fake.Replies(); len(got) != 1 || got[0] != "go on" {
		t.Errorf("replies = %v; want [go on]", got)
	}
}

// --- live signals through the seam (ADR-0020, adapter-owned since #92) -----
//
// State composition (spool dialog / blocked-state precedence) moved INTO the
// adapters with issue #92 — the precedence itself is pinned by claudecode's
// own tests and mirrored by the fake's ReadChat. The tests below are the
// core-side forwarding guards: core must hand ReadChat the runtime dir for an
// ACTIVE run (so the adapter's signals apply at all) and forward the composed
// view — state, side-channel dialog, transcript-derived messages — unaltered.

func TestRead_spoolDialogForcesQuestion(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	// The transcript derives 'working' and carries NO dialog message (Claude
	// Code never flushes a pending tool_use) — the adapter's live signals are
	// the only source, so this passes only if core passed the runtime dir.
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "…"}}})
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t_spool", Kind: provider.DialogKindQuestion, Answerable: true,
		Options: []provider.DialogOption{{Label: "A"}, {Label: "Other", IsOther: true}}})

	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.State != provider.StateQuestion {
		t.Errorf("state = %q; want question (spool overrides transcript working)", v.State)
	}
	if v.PendingDialog == nil || v.PendingDialog.ToolID != "t_spool" {
		t.Errorf("PendingDialog = %+v; want the spool dialog t_spool", v.PendingDialog)
	}
	// The message stream stays transcript-derived — the spool dialog is a
	// side-channel field, never injected as a message (seq stays reparse-stable,
	// issues #89/#90).
	if len(v.Messages) != 1 || v.Messages[0].Kind != provider.MessageText {
		t.Errorf("messages = %+v; want only the transcript's one text message", v.Messages)
	}
}

func TestRead_blockedMarkerForcesNeedsInput(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	// A tool is 'running' in the transcript, but the adapter's blocked marker
	// says the agent is actually waiting (e.g. a permission prompt) →
	// needs_input, composed inside ReadChat and forwarded by core.
	fake.SetChat(provider.Chat{State: provider.StateWorking})
	fake.SetBlockedState(provider.StateNeedsInput, true)

	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.State != provider.StateNeedsInput || v.PendingDialog != nil {
		t.Errorf("view = {state:%q dialog:%+v}; want needs_input with no dialog", v.State, v.PendingDialog)
	}
}

func TestRead_pendingDialogBeatsBlockedMarker(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking})
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t1", Answerable: true, Options: []provider.DialogOption{{Label: "A"}}})
	fake.SetBlockedState(provider.StateNeedsInput, true)

	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.State != provider.StateQuestion || v.PendingDialog == nil {
		t.Errorf("view = {state:%q dialog:%+v}; want the dialog to win over the marker", v.State, v.PendingDialog)
	}
}

// A transcript-derived StateQuestion (the dormant flushed-tool_use fallback)
// stands on its own: the adapter's blocked marker must not clobber it, and no
// side-channel dialog is synthesized for it — the dialog stays in Messages.
func TestRead_transcriptQuestionStandsOverMarker(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateQuestion, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{ToolID: "t_flushed"}},
	}})
	fake.SetBlockedState(provider.StateNeedsInput, true)

	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.State != provider.StateQuestion || v.PendingDialog != nil {
		t.Errorf("view = {state:%q dialog:%+v}; want the transcript question to stand with no side-channel dialog", v.State, v.PendingDialog)
	}
}

// An ended run must never show spool residue: core reads it transcript-only BY
// CONSTRUCTION — runtimeDir "" down the seam, so the adapter's live signals
// are off — and then forces the core-owned terminal state (issue #92).
func TestRead_endedRunIgnoresSpool(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeStopped)
	if err := st.UpdateRunTranscriptPath(context.Background(), run.ID, "/transcript.jsonl"); err != nil {
		t.Fatal(err)
	}
	run, _ = st.RunByID(context.Background(), run.ID)
	fake.SetChat(provider.Chat{State: provider.StateWorking})
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t1", Answerable: true})

	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.State != provider.StateEnded || v.PendingDialog != nil {
		t.Errorf("ended run view = {state:%q dialog:%+v}; want ended with no spool dialog", v.State, v.PendingDialog)
	}
	// The by-construction guarantee: the ended read went down the seam with NO
	// runtime dir — the residue was never even readable, not merely filtered.
	calls := fake.ReadCalls()
	if len(calls) != 1 {
		t.Fatalf("ReadChat calls = %d; want 1", len(calls))
	}
	if calls[0].RuntimeDir != "" || calls[0].RunID != run.ID || calls[0].TranscriptPath != "/transcript.jsonl" {
		t.Errorf("ended ReadChat call = %+v; want {RunID:%s RuntimeDir:\"\" TranscriptPath:/transcript.jsonl}", calls[0], run.ID)
	}
}

// Core's side of the #92 seam contract for an ACTIVE run: ReadChat gets the
// run id (keys the adapter's spool), the run's PRIVATE runtime dir (issue
// #205 — live signals on, resolved through RuntimeDirFor), and the resolved
// transcript path.
func TestRead_activeRunPassesSeamArgs(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	if _, err := svc.Read(context.Background(), run); err != nil {
		t.Fatalf("Read: %v", err)
	}
	calls := fake.ReadCalls()
	if len(calls) != 1 {
		t.Fatalf("ReadChat calls = %d; want 1", len(calls))
	}
	want := providertest.ReadCall{RunID: run.ID, RuntimeDir: svc.runtimeDir(run.ID), TranscriptPath: "/transcript.jsonl"}
	if want.RuntimeDir == "" {
		t.Fatal("fixture wired no RuntimeDirFor — the per-run dir assertion would be vacuous")
	}
	if calls[0] != want {
		t.Errorf("ReadChat call = %+v; want %+v", calls[0], want)
	}
}

// Pre-transcript consult: an ACTIVE run with no transcript located yet still
// reads through ReadChat("",…) with the runtime dir, so the adapter consults
// its live signals — a pending dialog can exist before LocateTranscript first
// hits (ADR-0020) — and otherwise composes an idle empty chat.
func TestRead_preTranscriptConsultsLiveSignals(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("") // LocateTranscript misses; nothing persisted

	// No dialog pending: the adapter's idle empty base stands, no identity yet.
	v, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read (no dialog): %v", err)
	}
	if v.State != provider.StateIdle || len(v.Messages) != 0 || v.TranscriptID != "" {
		t.Errorf("view = {state:%q msgs:%d id:%q}; want an idle empty chat with no transcript id", v.State, len(v.Messages), v.TranscriptID)
	}

	// The picker opens before any transcript exists: the dialog must surface.
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t_early", Answerable: true,
		Options: []provider.DialogOption{{Label: "A"}}})
	v, err = svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read (dialog pending): %v", err)
	}
	if v.State != provider.StateQuestion || v.PendingDialog == nil || v.PendingDialog.ToolID != "t_early" {
		t.Errorf("view = {state:%q dialog:%+v}; want question with the early dialog t_early", v.State, v.PendingDialog)
	}
	// Both reads went down the seam with the run's runtime dir and an empty path.
	for i, c := range fake.ReadCalls() {
		if c.RunID != run.ID || c.RuntimeDir != svc.runtimeDir(run.ID) || c.TranscriptPath != "" {
			t.Errorf("ReadChat call %d = %+v; want {RunID:%s RuntimeDir:%s TranscriptPath:\"\"}", i, c, run.ID, svc.runtimeDir(run.ID))
		}
	}
}

// A ReadChat error passes through Read untouched — ErrTranscriptGone is the
// adapter's vanished-file signal and httpapi keys the "transcript no longer
// available" rendering on errors.Is against it.
func TestRead_readErrorPassesThrough(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetReadError(provider.ErrTranscriptGone)

	if _, err := svc.Read(context.Background(), run); !errors.Is(err, provider.ErrTranscriptGone) {
		t.Fatalf("Read = %v; want ErrTranscriptGone passed through", err)
	}
}

func TestPendingDialog_prefersSpoolAndGuardsAnswer(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking}) // transcript shows no dialog
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t_spool", Answerable: true,
		Options: []provider.DialogOption{{Label: "A"}, {Label: "Other", IsOther: true}}})

	d, ok := svc.PendingDialog(context.Background(), run)
	if !ok || d.ToolID != "t_spool" {
		t.Fatalf("PendingDialog = %+v,%v; want the spool dialog t_spool", d, ok)
	}
	// Stale tool_id is refused; the matching one plays keystrokes.
	if err := svc.AnswerDialog(context.Background(), run, "t_stale", provider.DialogAnswer{Index: 0}); !errors.Is(err, ErrDialogChanged) {
		t.Errorf("stale answer = %v; want ErrDialogChanged", err)
	}
	if err := svc.AnswerDialog(context.Background(), run, "t_spool", provider.DialogAnswer{Index: 0}); err != nil {
		t.Errorf("matching answer = %v; want nil", err)
	}
	if got := fake.Answers(); len(got) != 1 {
		t.Errorf("answers recorded = %d; want exactly the matching one", len(got))
	}
}

func TestReply_lockedBySpoolDialog(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking}) // transcript alone would NOT lock
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t1", Answerable: true})

	if err := svc.Reply(context.Background(), run, "hi"); err != ErrDialogPending {
		t.Errorf("reply while a spool dialog is pending = %v; want ErrDialogPending", err)
	}
}

// The critical ADR-0020 behavior: a pending dialog appears while the transcript
// is byte-frozen (Claude Code never flushes a pending tool_use), so the tailer
// must notice the SPOOL change and republish state — watching the file alone
// would never fire.
func TestTailer_republishesOnSpoolChangeWhileTranscriptFrozen(t *testing.T) {
	svc, st, fake, bus := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}") // written once, never touched again — the frozen transcript
	fake.SetTranscriptPath(path)
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	sub, cancel := bus.Subscribe(context.Background())
	defer cancel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateWorking
	}, "tailer to derive initial working state")
	drainFor(sub, EventMessagesChanged, 2*time.Second) // consume the initial publish

	// The picker opens: a spool appears (the transcript file stays frozen). Only
	// the spool signature changes.
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t1", Answerable: true, Options: []provider.DialogOption{{Label: "A"}}})
	fake.SetSpoolSig("dialogs/run1.json:1:200;")

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateQuestion
	}, "tailer to flip to question on the spool change alone")
	if !drainFor(sub, EventMessagesChanged, 2*time.Second) {
		t.Error("no run.messages.changed published on the spool-only change")
	}
}

// A /clear (or /rewind) rotates the sessionId → a brand-new transcript file.
// The tailer must re-resolve each tick, follow the rotation, re-point the run
// row, re-derive state from the fresh file, and republish — otherwise it stats
// the frozen old file forever and the chat never updates (issue #34).
func TestTailer_followsTranscriptRotation(t *testing.T) {
	svc, st, fake, bus := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	dir := t.TempDir()
	pathA := dir + "/a.jsonl"
	pathB := dir + "/b.jsonl"
	writeFile(t, pathA, "{}")
	fake.SetTranscriptPath(pathA)
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	sub, cancel := bus.Subscribe(context.Background())
	defer cancel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateWorking
	}, "tailer to derive the initial working state on transcript A")
	drainFor(sub, EventMessagesChanged, 2*time.Second) // consume the initial publish

	got, _ := st.RunByID(context.Background(), run.ID)
	if got.TranscriptPath == nil || *got.TranscriptPath != pathA {
		t.Fatalf("persisted path = %v; want A (%s)", got.TranscriptPath, pathA)
	}

	// /clear rotates: a brand-new file B with fresh (cleared) content. Set the
	// new chat BEFORE the new path so that once the tailer sees B it reads B's
	// content, never an intermediate mix.
	writeFile(t, pathB, "{}")
	fake.SetChat(provider.Chat{State: provider.StateNeedsInput})
	fake.SetTranscriptPath(pathB)

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateNeedsInput
	}, "tailer to follow the rotation to B and re-derive state")
	if !drainFor(sub, EventMessagesChanged, 2*time.Second) {
		t.Error("no run.messages.changed published on the rotation")
	}
	got, _ = st.RunByID(context.Background(), run.ID)
	if got.TranscriptPath == nil || *got.TranscriptPath != pathB {
		t.Errorf("persisted path after rotation = %v; want B (%s)", got.TranscriptPath, pathB)
	}
}

func TestTailer_derivesStateAndPublishes(t *testing.T) {
	svc, st, fake, bus := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	// A real transcript file so the tailer's os.Stat sees it change.
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}")
	fake.SetTranscriptPath(path)
	fake.SetChat(provider.Chat{State: provider.StateNeedsInput})

	sub, cancel := bus.Subscribe(context.Background())
	defer cancel()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	// The tailer should arm from the initial sync, derive state, and publish.
	waitFor(t, func() bool {
		st, ok := svc.State(run.SessionName)
		return ok && st == provider.StateNeedsInput
	}, "tailer to derive needs_input state")

	// The envelope carries the composed state beside the run identity (issue
	// #175), and a first tick — no baseline yet — never claims a backpatch.
	p, ok := drainPayloadFor(sub, EventMessagesChanged, 2*time.Second)
	if !ok {
		t.Fatal("no run.messages.changed published")
	}
	if p.Type != EventMessagesChanged || p.RepoID != run.RepoID || p.RunID != run.ID {
		t.Errorf("payload identity = %+v; want {%s %s %s}", p, EventMessagesChanged, run.RepoID, run.ID)
	}
	if p.State != provider.StateNeedsInput {
		t.Errorf("payload State = %q; want %q (the tick's composed state)", p.State, provider.StateNeedsInput)
	}
	if p.BackpatchSeq != 0 {
		t.Errorf("first-tick BackpatchSeq = %d; want 0 (no baseline yet)", p.BackpatchSeq)
	}
}

// backpatchMessages builds a fresh transcript base for the backpatch test —
// fresh per read because the fake's ReadChat shares its Messages backing array
// and the tailer stamps hashes in place. toolDone flips the seq-2 tool from
// running to ok with output (the back-patch under test); extra appends that
// many trailing text messages after it.
func backpatchMessages(toolDone bool, extra int) provider.Chat {
	tool := &provider.ToolInfo{Name: "Bash", Title: "go test", Status: "running"}
	if toolDone {
		tool = &provider.ToolInfo{Name: "Bash", Title: "go test", Status: "ok", Output: "PASS"}
	}
	msgs := []provider.Message{
		{Seq: 1, Kind: provider.MessageText, Role: "user", Text: "run the tests"},
		{Seq: 2, Kind: provider.MessageTool, Tool: tool},
	}
	for i := range extra {
		msgs = append(msgs, provider.Message{Seq: int64(3 + i), Kind: provider.MessageText,
			Role: "assistant", Text: fmt.Sprintf("progress %d", i)})
	}
	return provider.Chat{State: provider.StateWorking, Cursor: msgs[len(msgs)-1].Seq, Messages: msgs}
}

// The backpatchSeq semantics on run.messages.changed (issue #175): append-only
// growth publishes without one; growth that re-renders an earlier message (a
// tool_result completing a prior tool_use) publishes the LOWEST changed seq;
// and a rotation tick publishes none even when the fresh file's restarted seqs
// carry different content — the baseline resets with the file.
func TestTailer_backpatchSeq(t *testing.T) {
	svc, st, fake, bus := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	dir := t.TempDir()
	pathA := dir + "/a.jsonl"
	writeFile(t, pathA, "1")
	fake.SetTranscriptPath(pathA)
	fake.SetChat(backpatchMessages(false, 0))

	sub, cancel := bus.Subscribe(context.Background())
	defer cancel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	// First tick: no baseline yet — never a backpatch.
	p, ok := drainPayloadFor(sub, EventMessagesChanged, 2*time.Second)
	if !ok {
		t.Fatal("no initial run.messages.changed")
	}
	if p.RunID != run.ID || p.BackpatchSeq != 0 {
		t.Errorf("first tick payload = %+v; want runID %s with no BackpatchSeq", p, run.ID)
	}

	// (1) Append-only growth: a new seq 3 lands, nothing prior re-renders.
	// Chat is set BEFORE the file write (the stat trigger), so the tailer can
	// never read an intermediate mix.
	fake.SetChat(backpatchMessages(false, 1))
	writeFile(t, pathA, "12")
	p, ok = drainPayloadFor(sub, EventMessagesChanged, 2*time.Second)
	if !ok {
		t.Fatal("no run.messages.changed on the append-only tick")
	}
	if p.BackpatchSeq != 0 {
		t.Errorf("append-only BackpatchSeq = %d; want 0 (new seqs are appends, never backpatch)", p.BackpatchSeq)
	}

	// (2) The tool_result lands: seq 2 flips running→ok (with output) while
	// seq 4 appends — the envelope names the back-patched seq, not the append.
	fake.SetChat(backpatchMessages(true, 2))
	writeFile(t, pathA, "123")
	p, ok = drainPayloadFor(sub, EventMessagesChanged, 2*time.Second)
	if !ok {
		t.Fatal("no run.messages.changed on the backpatch tick")
	}
	if p.BackpatchSeq != 2 {
		t.Errorf("backpatch tick BackpatchSeq = %d; want 2 (the completed tool message's seq)", p.BackpatchSeq)
	}

	// (3) Rotation: a fresh file whose restarted seq 1 carries DIFFERENT
	// content than the old seq 1 — without the baseline reset this would read
	// as backpatchSeq 1. The client resets its stream via transcript_id.
	pathB := dir + "/b.jsonl"
	writeFile(t, pathB, "{}")
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "fresh conversation"},
	}})
	fake.SetTranscriptPath(pathB)
	p, ok = drainPayloadFor(sub, EventMessagesChanged, 2*time.Second)
	if !ok {
		t.Fatal("no run.messages.changed on the rotation tick")
	}
	if p.BackpatchSeq != 0 {
		t.Errorf("rotation tick BackpatchSeq = %d; want 0 (baseline reset before comparing)", p.BackpatchSeq)
	}
}

// A sig-only tick can still observe a back-patch: the read races the agent's
// writer, so content can change between a tick's stat (unchanged) and its
// ReadChat. The publish gate must fire on the backpatch alone — the baseline
// advances after every successful read, so a swallowed announcement is never
// re-derived and the seq would stay stale on every client until navigation
// (ADR-0047's delta-loss leg).
func TestTailer_publishesBackpatchOnSigOnlyTick(t *testing.T) {
	svc, st, fake, bus := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}") // frozen: the stat never changes again
	fake.SetTranscriptPath(path)
	fake.SetChat(backpatchMessages(false, 0))

	sub, cancel := bus.Subscribe(context.Background())
	defer cancel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	// Baseline tick: seq 1..2 read, the seq-2 tool still running.
	p, ok := drainPayloadFor(sub, EventMessagesChanged, 2*time.Second)
	if !ok {
		t.Fatal("no initial run.messages.changed")
	}
	if p.BackpatchSeq != 0 {
		t.Errorf("first-tick BackpatchSeq = %d; want 0 (no baseline yet)", p.BackpatchSeq)
	}

	// The seq-2 tool flips running→ok while the transcript file stays frozen
	// (same stat) and the state stays working: only the spool signature flips,
	// so the read gate opens while the stat and state legs of the publish gate
	// are both closed. Chat is set BEFORE the sig (the read trigger), so the
	// tailer can never read an intermediate mix.
	fake.SetChat(backpatchMessages(true, 0))
	fake.SetSpoolSig("dialogs/run1.json:1:200;")

	p, ok = drainPayloadFor(sub, EventMessagesChanged, 2*time.Second)
	if !ok {
		t.Fatal("no run.messages.changed on the sig-only backpatch tick")
	}
	if p.RunID != run.ID || p.BackpatchSeq != 2 {
		t.Errorf("sig-only tick payload = %+v; want runID %s with BackpatchSeq 2 (the flipped tool's seq)", p, run.ID)
	}
}

func TestTailerSet_removeIsGenerationAware(t *testing.T) {
	ts := newTailerSet()
	h1 := &tailerHandle{cancel: func() {}}
	h2 := &tailerHandle{cancel: func() {}}
	ts.add("s", h1)
	// The session name is reused (stop→start in the same minute): the
	// successor registers before the predecessor goroutine finishes exiting.
	ts.add("s", h2)
	ts.setState("s", provider.StateWorking)

	ts.remove("s", h1) // late-exiting predecessor
	if ts.handles["s"] != h2 {
		t.Error("stale remove deleted the successor's registration")
	}
	if _, ok := ts.state("s"); !ok {
		t.Error("stale remove deleted the successor's state")
	}

	ts.remove("s", h2)
	if _, ok := ts.handles["s"]; ok {
		t.Error("own remove left the registration behind")
	}
}

func seedRunID(t *testing.T, st *store.Store, id, outcome string) store.Run {
	t.Helper()
	if _, err := st.RepoByID(context.Background(), "repo1"); err != nil {
		_, _ = st.CreateRepo(context.Background(), store.Repo{
			ID: "repo1", Name: "proj", RemoteURL: "file:///x", TrackerBinding: store.TrackerBindingBuiltin,
			ForgeKind: "none", DefaultBranch: "main", AFKBranchPattern: "afk/<N>",
			ManualBranchPrefix: "lab/", CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
		})
	}
	run, err := st.CreateRun(context.Background(), store.Run{
		ID: id, RepoID: "repo1", Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/" + id, WorktreePath: "/wt/" + id, SessionName: "proj~" + id,
		StartedAt: time.Now(), Outcome: outcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func drainFor(sub <-chan events.Event, typ string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case e := <-sub:
			if e.Type == typ {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// drainPayloadFor is drainFor returning the matched event's decoded
// run.messages.changed payload — the issue #175 envelope assertions.
func drainPayloadFor(sub <-chan events.Event, typ string, d time.Duration) (messagesChangedPayload, bool) {
	deadline := time.After(d)
	for {
		select {
		case e := <-sub:
			if e.Type != typ {
				continue
			}
			if p, ok := e.Payload.(messagesChangedPayload); ok {
				return p, true
			}
		case <-deadline:
			return messagesChangedPayload{}, false
		}
	}
}
