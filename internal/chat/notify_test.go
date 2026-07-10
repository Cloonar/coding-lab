package chat

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// fakeClock is a hand-cranked clock for the gate unit tests — no goroutines, no
// sleeps: the deadline arithmetic is exercised by advancing time directly.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func gateRun() store.Run { return store.Run{ID: "r1", SessionName: "proj~afk-12"} }

func chatOf(state string) provider.Chat { return provider.Chat{State: state} }

// --- gate unit tests -------------------------------------------------------

// 1. Entering the notify set fires exactly once, and only after the window.
func TestNotifyGate_edgeFiresOnceAfterWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newNotifyGate(clk.now, 2*time.Second, gateRun())

	g.observe(provider.StateWorking, chatOf(provider.StateQuestion))
	if _, ok := g.due(); ok {
		t.Fatal("fired before the debounce window elapsed")
	}
	clk.advance(2 * time.Second)
	n, ok := g.due()
	if !ok {
		t.Fatal("did not fire once the window elapsed")
	}
	if n.Tag != "r1" {
		t.Errorf("Tag = %q; want r1", n.Tag)
	}
	if _, ok := g.due(); ok {
		t.Error("fired a second time for the same episode")
	}
	// An identity re-read while latched must not re-arm or re-fire.
	g.observe(provider.StateQuestion, chatOf(provider.StateQuestion))
	if _, ok := g.due(); ok {
		t.Error("an identity re-read re-fired after the episode already latched")
	}
}

// 2. A flap that leaves the set before the window fires nothing.
func TestNotifyGate_flapFiresNothing(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newNotifyGate(clk.now, 2*time.Second, gateRun())

	g.observe(provider.StateWorking, chatOf(provider.StateQuestion))
	clk.advance(1 * time.Second)
	g.observe(provider.StateQuestion, chatOf(provider.StateWorking))
	clk.advance(10 * time.Second)
	if _, ok := g.due(); ok {
		t.Error("a flap out of the set before the window fired a notification")
	}
}

// 3. Re-adoption edge: the first-tick empty prev counts as non-notify, so an
// already-in-question run arms and fires once (the deliberate re-adoption
// re-fire).
func TestNotifyGate_adoptionEdgeFires(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newNotifyGate(clk.now, 2*time.Second, gateRun())

	g.observe("", chatOf(provider.StateNeedsInput))
	clk.advance(2 * time.Second)
	if _, ok := g.due(); !ok {
		t.Fatal("re-adoption into needs_input did not fire")
	}
	if _, ok := g.due(); ok {
		t.Error("re-adoption fired more than once")
	}
}

// 4. Transitions that never touch the notify set never arm.
func TestNotifyGate_nonNotifyNeverArms(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	edges := []struct{ prev, cur string }{
		{provider.StateIdle, provider.StateWorking},
		{provider.StateWorking, provider.StateIdle},
		{provider.StateWorking, provider.StateEnded},
	}
	for _, e := range edges {
		g := newNotifyGate(clk.now, 2*time.Second, gateRun())
		g.observe(e.prev, chatOf(e.cur))
		clk.advance(time.Hour)
		if _, ok := g.due(); ok {
			t.Errorf("%s→%s armed the gate; want never", e.prev, e.cur)
		}
	}
}

// 5. Leaving the set re-arms: a second, distinct episode fires again.
func TestNotifyGate_reFiresOnNewEpisode(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newNotifyGate(clk.now, 2*time.Second, gateRun())

	g.observe(provider.StateWorking, chatOf(provider.StateQuestion))
	clk.advance(2 * time.Second)
	if _, ok := g.due(); !ok {
		t.Fatal("first episode did not fire")
	}
	g.observe(provider.StateQuestion, chatOf(provider.StateWorking)) // leaves the set
	g.observe(provider.StateWorking, chatOf(provider.StateNeedsInput))
	clk.advance(2 * time.Second)
	if _, ok := g.due(); !ok {
		t.Error("a new episode after leaving the set did not re-fire")
	}
}

// 6. Moving between the two notify states is ONE episode: the newer body wins,
// the deadline is unchanged, exactly one fire, and no second fire on a
// same-episode re-entry after the latch.
func TestNotifyGate_betweenNotifyStatesSameEpisode(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newNotifyGate(clk.now, 2*time.Second, gateRun())

	// Enter on needs_input; body derived from the assistant text.
	g.observe(provider.StateWorking, provider.Chat{State: provider.StateNeedsInput,
		Messages: []provider.Message{{Kind: provider.MessageText, Role: "assistant", Text: "here is my plan"}}})
	clk.advance(1 * time.Second)
	// Still inside the set: a dialog opens; body must be refreshed to its prompt.
	g.observe(provider.StateNeedsInput, provider.Chat{State: provider.StateQuestion,
		PendingDialog: &provider.Dialog{Prompt: "approve the plan?"}})
	clk.advance(1 * time.Second) // now at the ORIGINAL deadline (t0+2s), not t0+3s
	n, ok := g.due()
	if !ok {
		t.Fatal("did not fire at the original deadline")
	}
	if n.Body != "approve the plan?" {
		t.Errorf("Body = %q; want the refreshed dialog prompt", n.Body)
	}
	// A same-episode re-entry after the latch must not fire again.
	g.observe(provider.StateQuestion, chatOf(provider.StateNeedsInput))
	if _, ok := g.due(); ok {
		t.Error("a same-episode re-entry fired a second time after the latch")
	}
}

// 7. Re-observing inside the window does not push the deadline out.
func TestNotifyGate_deadlineNotExtended(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newNotifyGate(clk.now, 2*time.Second, gateRun())

	g.observe(provider.StateWorking, chatOf(provider.StateQuestion)) // deadline t0+2s
	clk.advance(1 * time.Second)
	g.observe(provider.StateQuestion, chatOf(provider.StateQuestion)) // identity, no extend
	clk.advance(1 * time.Second)                                      // t0+2s exactly
	if _, ok := g.due(); !ok {
		t.Error("fired later than the original deadline — the re-observe extended it")
	}
}

// 8. Payload identity fields.
func TestNotifyGate_payloadFields(t *testing.T) {
	g := newNotifyGate(time.Now, 2*time.Second, gateRun())
	n := g.buildPayload(chatOf(provider.StateNeedsInput))
	if n.Title != "proj~afk-12 needs input" {
		t.Errorf("Title = %q", n.Title)
	}
	if n.Tag != "r1" {
		t.Errorf("Tag = %q", n.Tag)
	}
	if n.Route != "/runs/r1" {
		t.Errorf("Route = %q", n.Route)
	}
}

// 9. Body precedence and rune-safe truncation.
func TestNotifyGate_bodyPrecedenceAndTruncation(t *testing.T) {
	g := newNotifyGate(time.Now, 2*time.Second, gateRun())

	// (a) A live dialog prompt beats assistant text.
	got := g.buildPayload(provider.Chat{State: provider.StateQuestion,
		PendingDialog: &provider.Dialog{Prompt: "dialog wins"},
		Messages:      []provider.Message{{Kind: provider.MessageText, Role: "assistant", Text: "text loses"}}}).Body
	if got != "dialog wins" {
		t.Errorf("dialog precedence: Body = %q; want dialog wins", got)
	}

	// (b) No dialog: the NEWEST VISIBLE assistant text wins — the trailing user
	// reply proves the backward scan skips non-assistant tail messages, and the
	// trailing thinking block proves hidden chain-of-thought NEVER becomes a
	// lock-screen body.
	got = g.buildPayload(provider.Chat{State: provider.StateNeedsInput, Messages: []provider.Message{
		{Kind: provider.MessageText, Role: "assistant", Text: "older assistant"},
		{Kind: provider.MessageText, Role: "assistant", Text: "newest assistant"},
		{Kind: provider.MessageText, Role: "assistant", Text: "private reasoning", Thinking: true},
		{Kind: provider.MessageText, Role: "user", Text: "a trailing user reply"},
	}}).Body
	if got != "newest assistant" {
		t.Errorf("assistant precedence: Body = %q; want newest assistant", got)
	}

	// (c) Neither dialog nor visible assistant text (a thinking block alone
	// doesn't count): the literal fallback.
	got = g.buildPayload(provider.Chat{State: provider.StateNeedsInput, Messages: []provider.Message{
		{Kind: provider.MessageText, Role: "assistant", Text: "private reasoning", Thinking: true},
		{Kind: provider.MessageText, Role: "user", Text: "only a user message"},
	}}).Body
	if got != "needs your input" {
		t.Errorf("fallback: Body = %q; want needs your input", got)
	}

	// (d) A >150-rune MULTI-BYTE prompt truncates to exactly 150 runes ending in
	// the ellipsis rune — asserted on []rune length to prove rune-safety.
	long := strings.Repeat("ä", 200)
	got = g.buildPayload(provider.Chat{State: provider.StateQuestion,
		PendingDialog: &provider.Dialog{Prompt: long}}).Body
	r := []rune(got)
	if len(r) != 150 {
		t.Fatalf("truncated body = %d runes; want exactly 150", len(r))
	}
	if r[149] != '…' {
		t.Errorf("last rune = %q; want the ellipsis", string(r[149]))
	}
	if string(r[:149]) != strings.Repeat("ä", 149) {
		t.Error("truncation split a multi-byte rune; the kept prefix is corrupted")
	}
}

// --- loop-level integration tests ------------------------------------------

// captureNotifier records every Notification the tailer emits.
type captureNotifier struct {
	mu  sync.Mutex
	got []Notification
}

func (c *captureNotifier) notify(n Notification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, n)
}

func (c *captureNotifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func (c *captureNotifier) last() Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.got) == 0 {
		return Notification{}
	}
	return c.got[len(c.got)-1]
}

// newNotifyService is newService's variant that wires a Notify closure and a
// short debounce — a local helper so the existing newService and its callers
// stay untouched.
func newNotifyService(t *testing.T, notify func(Notification), debounce time.Duration) (*Service, *store.Store, *providertest.Fake, *events.Bus) {
	t.Helper()
	st := testutil.TempStore(t)
	fake := providertest.New()
	reg, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	svc, err := New(Options{Store: st, Providers: reg, Bus: bus, Logger: logx.New(io.Discard),
		Poll: 5 * time.Millisecond, RuntimeDir: t.TempDir(), Notify: notify, NotifyDebounce: debounce})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, fake, bus
}

// 10. A working→question transition notifies exactly once, end-to-end.
func TestTailer_notifiesOnceOnTransition(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newNotifyService(t, cn.notify, 20*time.Millisecond)
	run := seedRun(t, st, store.RunOutcomeActive)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}")
	fake.SetTranscriptPath(path)
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateWorking
	}, "tailer to derive the initial working state")

	// The dialog opens: the spool signature flips (the transcript stays frozen),
	// forcing a re-read that composes StateQuestion + the pending dialog.
	fake.SetPendingDialog(&provider.Dialog{Prompt: "approve the plan?", Answerable: true})
	fake.SetSpoolSig("2")

	waitFor(t, func() bool { return cn.count() == 1 }, "exactly one notification")
	got := cn.last()
	want := Notification{Title: "proj~x needs input", Body: "approve the plan?", Tag: "run1", Route: "/runs/run1"}
	if got != want {
		t.Errorf("notification = %+v; want %+v", got, want)
	}
	// Give the loop ~10 more poll intervals: the count must not climb.
	time.Sleep(50 * time.Millisecond)
	if n := cn.count(); n != 1 {
		t.Errorf("notifications = %d after settling; want exactly 1", n)
	}
}

// 11. A run adopted already IN a notify state notifies exactly once.
func TestTailer_notifiesOnceOnAdoption(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newNotifyService(t, cn.notify, 20*time.Millisecond)
	seedRun(t, st, store.RunOutcomeActive)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}")
	fake.SetTranscriptPath(path)
	// Question from the very first read: the empty first-tick prev is non-notify,
	// so the first observe arms the gate.
	fake.SetChat(provider.Chat{State: provider.StateWorking})
	fake.SetPendingDialog(&provider.Dialog{Prompt: "pick one", Answerable: true})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	waitFor(t, func() bool { return cn.count() == 1 }, "one notification on adoption")
	time.Sleep(50 * time.Millisecond)
	if n := cn.count(); n != 1 {
		t.Errorf("notifications = %d; want exactly 1", n)
	}
}

// 12. A nil notifier changes nothing: the loop runs the same transition with no
// panic (the state change is the completion signal).
func TestTailer_nilNotifierIsAbsent(t *testing.T) {
	svc, st, fake, _ := newNotifyService(t, nil, 20*time.Millisecond)
	run := seedRun(t, st, store.RunOutcomeActive)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}")
	fake.SetTranscriptPath(path)
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateWorking
	}, "tailer to derive working with a nil notifier")

	fake.SetPendingDialog(&provider.Dialog{Prompt: "?", Answerable: true})
	fake.SetSpoolSig("2")
	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateQuestion
	}, "tailer to flip to question with a nil notifier (no panic)")
}

// 13. A transition that never enters the notify set produces no notification.
func TestTailer_nonNotifyTransitionSilent(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newNotifyService(t, cn.notify, 20*time.Millisecond)
	run := seedRun(t, st, store.RunOutcomeActive)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}")
	fake.SetTranscriptPath(path)
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateWorking
	}, "tailer to derive working")

	fake.SetChat(provider.Chat{State: provider.StateIdle})
	fake.SetSpoolSig("2")
	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateIdle
	}, "tailer to flip to idle")

	time.Sleep(50 * time.Millisecond) // debounce (20ms) would have elapsed by now
	if n := cn.count(); n != 0 {
		t.Errorf("notifications = %d for a non-notify transition; want 0", n)
	}
}

// 14. A run that ends before the debounce elapses fires nothing: the armed gate
// dies with the cancelled goroutine.
func TestTailer_runEndsBeforeDebounceFiresNothing(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, bus := newNotifyService(t, cn.notify, 1*time.Hour) // never elapses in the test
	run := seedRun(t, st, store.RunOutcomeActive)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}")
	fake.SetTranscriptPath(path)
	fake.SetChat(provider.Chat{State: provider.StateWorking})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go svc.Run(ctx)

	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateWorking
	}, "tailer to derive working")

	// Arm the gate (question), then end the run before the 1h debounce.
	fake.SetPendingDialog(&provider.Dialog{Prompt: "will never fire", Answerable: true})
	fake.SetSpoolSig("2")
	waitFor(t, func() bool {
		s, ok := svc.State(run.SessionName)
		return ok && s == provider.StateQuestion
	}, "tailer to arm on question")

	if err := st.EndRun(context.Background(), run.ID, store.RunOutcomeStopped, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	bus.Publish(events.Event{Type: eventRunChanged})

	waitFor(t, func() bool {
		_, ok := svc.State(run.SessionName)
		return !ok // the tailer disarmed
	}, "tailer to disarm on the ended run")
	time.Sleep(30 * time.Millisecond)
	if n := cn.count(); n != 0 {
		t.Errorf("notifications = %d; want 0 (the armed gate died with the goroutine)", n)
	}
}
