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
	svc, err := New(Options{Store: st, Providers: reg, Bus: bus, Logger: logx.New(io.Discard), Poll: 5 * time.Millisecond})
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
