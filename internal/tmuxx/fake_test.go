package tmuxx

import (
	"errors"
	"slices"
	"testing"
)

func TestFake_startRecordsAndIsIdempotent(t *testing.T) {
	ctx := t.Context()
	f := NewFake()

	argv := []string{"claude", "--remote-control", "repo~afk-7"}
	env := []string{"LAB_TOKEN=lab_run_x"}
	if err := f.Start(ctx, "repo~afk-7", "/wt/repo-7", argv, env); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The record holds copies: mutating the caller's slices afterwards
	// must not reach into the fake (fresh-slice discipline).
	argv[0] = "mutated"
	env[0] = "mutated"

	s, ok := f.Session("repo~afk-7")
	if !ok {
		t.Fatalf("session not recorded")
	}
	if want := []string{"claude", "--remote-control", "repo~afk-7"}; !slices.Equal(s.Argv, want) {
		t.Errorf("recorded argv = %q; want %q", s.Argv, want)
	}
	if want := []string{"LAB_TOKEN=lab_run_x"}; !slices.Equal(s.ExtraEnv, want) {
		t.Errorf("recorded env = %q; want %q", s.ExtraEnv, want)
	}
	if s.Dir != "/wt/repo-7" {
		t.Errorf("recorded dir = %q; want /wt/repo-7", s.Dir)
	}

	// Second Start no-ops and keeps the original record (real tmux
	// behaviour: the existing session wins).
	if err := f.Start(ctx, "repo~afk-7", "/other", []string{"other"}, nil); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	s, _ = f.Session("repo~afk-7")
	if s.Dir != "/wt/repo-7" {
		t.Errorf("second Start overwrote the record; dir = %q", s.Dir)
	}
}

func TestFake_lifecycleAndList(t *testing.T) {
	ctx := t.Context()
	f := NewFake()

	// Empty server: List = nil, nil (the real missing-server shape).
	if names, err := f.List(ctx); err != nil || names != nil {
		t.Fatalf("empty List = %v, %v; want nil, nil", names, err)
	}

	for _, n := range []string{"b~x", "a~y", LoginSession} {
		if err := f.Start(ctx, n, "/d", []string{"cmd"}, nil); err != nil {
			t.Fatalf("Start %s: %v", n, err)
		}
	}
	names, err := f.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"a~y", "b~x", LoginSession}; !slices.Equal(names, want) {
		t.Errorf("List = %q; want sorted %q", names, want)
	}

	ok, err := f.IsRunning(ctx, "a~y")
	if err != nil || !ok {
		t.Fatalf("IsRunning(a~y) = %v, %v; want true", ok, err)
	}
	if err := f.Stop(ctx, "a~y"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ok, _ := f.IsRunning(ctx, "a~y"); ok {
		t.Errorf("still running after Stop")
	}
	// Stop is idempotent, including for never-started names.
	if err := f.Stop(ctx, "a~y"); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if err := f.Stop(ctx, "never-started"); err != nil {
		t.Errorf("Stop on missing session: %v", err)
	}
}

func TestFake_addLiveAndKill(t *testing.T) {
	ctx := t.Context()
	f := NewFake()

	// AddLive: the restart fixture — live in tmux with no Start recorded.
	f.AddLive("repo~20260608-1530")
	if ok, _ := f.IsRunning(ctx, "repo~20260608-1530"); !ok {
		t.Fatalf("AddLive session not running")
	}

	// Kill: dies outside lab's control; liveness flips without Stop.
	f.Kill("repo~20260608-1530")
	if ok, _ := f.IsRunning(ctx, "repo~20260608-1530"); ok {
		t.Errorf("still running after Kill")
	}
}

func TestFake_sendKeysRecorderAndPane(t *testing.T) {
	ctx := t.Context()
	f := NewFake()

	// Absent session: SendKeys and CapturePane error like real tmux.
	if err := f.SendKeys(ctx, "ghost", "x", true); err == nil {
		t.Errorf("SendKeys to missing session: want error")
	}
	if _, err := f.CapturePane(ctx, "ghost"); err == nil {
		t.Errorf("CapturePane on missing session: want error")
	}

	if err := f.Start(ctx, LoginSession, "/home", []string{"claude", "auth", "login", "--claudeai"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.SendKeys(ctx, LoginSession, "code#part1", false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if err := f.SendKeys(ctx, LoginSession, "part2", true); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	want := []SentKey{{Text: "code#part1", Enter: false}, {Text: "part2", Enter: true}}
	if got := f.Sent(LoginSession); !slices.Equal(got, want) {
		t.Errorf("Sent = %v; want %v", got, want)
	}

	f.SetPane(LoginSession, "visit: https://claude.com/cai/oauth/authorize?code=true")
	got, err := f.CapturePane(ctx, LoginSession)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if got != "visit: https://claude.com/cai/oauth/authorize?code=true" {
		t.Errorf("CapturePane = %q", got)
	}

	// The SendKeys record survives Stop so tests can assert post-teardown.
	if err := f.Stop(ctx, LoginSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := f.Sent(LoginSession); !slices.Equal(got, want) {
		t.Errorf("Sent after Stop = %v; want %v", got, want)
	}
}

func TestFake_errorInjection(t *testing.T) {
	ctx := t.Context()
	f := NewFake()
	boom := errors.New("boom")

	f.FailStart("bad", boom)
	if err := f.Start(ctx, "bad", "/d", []string{"c"}, nil); !errors.Is(err, boom) {
		t.Errorf("FailStart: got %v", err)
	}
	if ok, _ := f.IsRunning(ctx, "bad"); ok {
		t.Errorf("failed Start still created a session")
	}
	f.FailStart("bad", nil)
	if err := f.Start(ctx, "bad", "/d", []string{"c"}, nil); err != nil {
		t.Errorf("Start after clearing injection: %v", err)
	}

	f.FailStop("bad", boom)
	if err := f.Stop(ctx, "bad"); !errors.Is(err, boom) {
		t.Errorf("FailStop: got %v", err)
	}
	f.FailStop("bad", nil)

	f.FailSendKeys("bad", boom)
	if err := f.SendKeys(ctx, "bad", "x", true); !errors.Is(err, boom) {
		t.Errorf("FailSendKeys: got %v", err)
	}

	f.FailList(boom)
	if _, err := f.List(ctx); !errors.Is(err, boom) {
		t.Errorf("FailList: got %v", err)
	}
	f.FailList(nil)
	if _, err := f.List(ctx); err != nil {
		t.Errorf("List after clearing injection: %v", err)
	}
}

// TestFake_keyLog covers the unified key-event log across the three delivery
// kinds (issue #7): literal text, named keys, and pastes interleave in call
// order, and FailSendKeys gates all three.
func TestFake_keyLog(t *testing.T) {
	ctx := t.Context()
	f := NewFake()
	const name = "proj~x"
	f.AddLive(name)

	if err := f.SendKeys(ctx, name, "hi", true); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if err := f.SendNamedKeys(ctx, name, "Up", "Up", "Enter"); err != nil {
		t.Fatalf("SendNamedKeys: %v", err)
	}
	if err := f.PasteText(ctx, name, "multi\nline"); err != nil {
		t.Fatalf("PasteText: %v", err)
	}
	want := []KeyEvent{
		{Kind: "text", Text: "hi", Enter: true},
		{Kind: "keys", Keys: "Up Up Enter"},
		{Kind: "paste", Text: "multi\nline"},
	}
	if got := f.KeyLog(name); !slices.Equal(got, want) {
		t.Errorf("KeyLog = %+v; want %+v", got, want)
	}

	// Absent session errors on every delivery kind.
	if err := f.SendNamedKeys(ctx, "ghost", "Escape"); err == nil {
		t.Error("SendNamedKeys to missing session: want error")
	}
	if err := f.PasteText(ctx, "ghost", "x"); err == nil {
		t.Error("PasteText to missing session: want error")
	}

	// FailSendKeys gates named keys and paste too.
	boom := errors.New("boom")
	f.FailSendKeys(name, boom)
	if err := f.SendNamedKeys(ctx, name, "Escape"); !errors.Is(err, boom) {
		t.Errorf("FailSendKeys(named) = %v; want boom", err)
	}
	if err := f.PasteText(ctx, name, "x"); !errors.Is(err, boom) {
		t.Errorf("FailSendKeys(paste) = %v; want boom", err)
	}
}
