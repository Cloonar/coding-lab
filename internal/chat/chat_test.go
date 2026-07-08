package chat

import (
	"context"
	"errors"
	"io"
	"os"
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
	svc, err := New(Options{Store: st, Providers: reg, Bus: bus, Logger: logx.New(io.Discard),
		Poll: 5 * time.Millisecond, RuntimeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, fake, bus
}

func seedRun(t *testing.T, st *store.Store, outcome string) store.Run {
	t.Helper()
	repo, err := st.CreateRepo(context.Background(), store.Repo{
		ID: "repo1", Name: "proj", RemoteURL: "file:///x", TrackerBinding: store.TrackerBindingBuiltin,
		ForgeKind: "none", DefaultBranch: "main", Provider: "claude-code", AFKBranchPattern: "afk/<N>",
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

// --- live dialog spool (ADR-0020) ----------------------------------------

func TestRead_spoolDialogForcesQuestion(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	// The transcript derives 'working' and carries NO dialog message (Claude
	// Code never flushes a pending tool_use) — the spool is the only live source.
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
	// side-channel field, never injected as a message (seq stays reparse-stable).
	if len(v.Messages) != 1 || v.Messages[0].Kind != provider.MessageText {
		t.Errorf("messages = %+v; want only the transcript's one text message", v.Messages)
	}
}

func TestRead_blockedMarkerForcesNeedsInput(t *testing.T) {
	svc, st, fake, _ := newService(t)
	run := seedRun(t, st, store.RunOutcomeActive)
	fake.SetTranscriptPath("/transcript.jsonl")
	// A tool is 'running' in the transcript, but a Notification marker says the
	// agent is actually blocked (e.g. a permission prompt) → needs_input.
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

func TestSweepSpools_runsPerProvider(t *testing.T) {
	svc, _, fake, _ := newService(t)
	svc.sweepSpools(map[string]bool{"run_active": true})
	if got := fake.Sweeps(); got != 1 {
		t.Errorf("SweepSpools calls = %d; want 1 (once per LiveSignals provider)", got)
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

	if !drainFor(sub, EventMessagesChanged, 2*time.Second) {
		t.Error("no run.messages.changed published")
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
			ForgeKind: "none", DefaultBranch: "main", Provider: "claude-code", AFKBranchPattern: "afk/<N>",
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
