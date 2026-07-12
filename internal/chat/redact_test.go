package chat

// Exposure detection + masking tests (issue #108). The service-level tests
// drive Service.Read directly (the HTTP path); the loop-level tests follow
// notify_test.go's harness (captureNotifier + a real tailer over the fake
// provider) to prove the tick-loop path: masking-before-gate and the
// no-HTTP-read exposure push. The Secrets seam is wired through a REAL
// secrets.Source over the store with an identity Decrypt (the test store
// holds the plaintext bytes as the "encrypted" blob), so the store→source→
// redactor path is the production one minus the vault cipher.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// testSecretValue is the plaintext test secret. It appears ONLY in this test
// file — never in a fixture a non-test path could log.
const (
	testSecretName  = "API_TOKEN"
	testSecretValue = "hunter2-xk9-value"
	testPlaceholder = "[REDACTED:" + testSecretName + "]"
)

// all returns a snapshot of every notification captured so far (redact tests
// need to pick one out by Tag when both the exposure and the needs-input push
// fire on the same run).
func (c *captureNotifier) all() []Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Notification(nil), c.got...)
}

// newSecretService is newNotifyService's variant that additionally wires the
// Secrets seam: a real secrets.Source over the test store with an identity
// Decrypt, exactly the shape cmd/lab injects (minus the vault cipher).
func newSecretService(t *testing.T, notify func(Notification), debounce time.Duration) (*Service, *store.Store, *providertest.Fake, *events.Bus) {
	t.Helper()
	st := testutil.TempStore(t)
	fake := providertest.New()
	reg, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	src := &secrets.Source{
		Values:  st.AllRepoSecretValues,
		Decrypt: func(blob []byte) ([]byte, error) { return blob, nil },
	}
	svc, err := New(Options{Store: st, Providers: reg, Bus: bus, Logger: logx.New(io.Discard),
		Poll: 5 * time.Millisecond, RuntimeDir: t.TempDir(),
		Notify: notify, NotifyDebounce: debounce, Secrets: src.Redactor})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, fake, bus
}

// seedSecret creates a secret row whose "encrypted" blob is the plaintext
// bytes (paired with newSecretService's identity Decrypt).
func seedSecret(t *testing.T, st *store.Store, repoID, name, value string) store.RepoSecret {
	t.Helper()
	rs, err := st.CreateRepoSecret(context.Background(), "sec_"+name, repoID, name, "", []byte(value), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// secretChat builds a fresh transcript base whose Text, Tool.Input, and a
// dialog message's prompt all carry the given value (exact) plus one encoded
// form (standard base64) in the tool input. Built fresh per read: the fake's
// ReadChat shares its Messages backing array, so an in-place mask on one read
// would otherwise leak into the next scripted read.
func secretChat(value string) provider.Chat {
	b64 := base64.StdEncoding.EncodeToString([]byte(value))
	return provider.Chat{State: provider.StateWorking, Cursor: 4, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "the token is " + value + " ok"},
		{Seq: 2, Kind: provider.MessageTool, Tool: &provider.ToolInfo{
			Name: "Bash", Title: "Ran a command", Input: "curl -H 'Auth: " + b64 + "'", Output: "done", Status: "ok"}},
		{Seq: 3, Kind: provider.MessageDialog, Dialog: &provider.Dialog{
			ToolID: "t1", Kind: provider.DialogKindQuestion, Prompt: "use " + value + "?",
			Options: []provider.DialogOption{{Label: "yes"}}, Answerable: true,
			Outcome: &provider.DialogOutcome{Results: []provider.QuestionResult{
				{Question: "use " + value + "?", Chosen: []string{"yes"}}}}}},
		{Seq: 4, Kind: provider.MessageLifecycle, Text: "session note: " + value},
	}}
}

// viewJSON renders the whole view as JSON — the "appears NOWHERE" assertion
// surface: every operator-visible string of the view is in there.
func viewJSON(t *testing.T, v View) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 1. Read masks the exact value and an encoded form everywhere it appears —
// Text, Tool.Input, a dialog message, and the live PendingDialog.
func TestReadRedact_masksEverywhere(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	fake.SetTranscriptPath("/t.jsonl")
	fake.SetChat(secretChat(testSecretValue))
	fake.SetPendingDialog(&provider.Dialog{ToolID: "t9", Kind: provider.DialogKindQuestion,
		Prompt: "deploy with " + testSecretValue + " now?", Answerable: true})

	view, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	js := viewJSON(t, view)
	if strings.Contains(js, testSecretValue) {
		t.Error("plaintext value survived in the rendered view")
	}
	if b64 := base64.StdEncoding.EncodeToString([]byte(testSecretValue)); strings.Contains(js, b64) {
		t.Error("base64 form survived in the rendered view")
	}
	if got := view.Messages[0].Text; got != "the token is "+testPlaceholder+" ok" {
		t.Errorf("Text = %q; want the placeholder", got)
	}
	if got := view.Messages[1].Tool.Input; !strings.Contains(got, testPlaceholder) {
		t.Errorf("Tool.Input = %q; want the placeholder for the base64 form", got)
	}
	if got := view.Messages[2].Dialog.Prompt; got != "use "+testPlaceholder+"?" {
		t.Errorf("dialog Prompt = %q; want the placeholder", got)
	}
	if got := view.Messages[2].Dialog.Outcome.Results[0].Question; got != "use "+testPlaceholder+"?" {
		t.Errorf("outcome Question = %q; want the placeholder", got)
	}
	if got := view.Messages[3].Text; got != "session note: "+testPlaceholder {
		t.Errorf("lifecycle Text = %q; want the placeholder", got)
	}
	if view.PendingDialog == nil || view.PendingDialog.Prompt != "deploy with "+testPlaceholder+" now?" {
		t.Errorf("PendingDialog = %+v; want the masked prompt", view.PendingDialog)
	}
}

// 2. The hit marks the secret exposed (run id + timestamp) and sends EXACTLY
// one exposure notification with the pinned fields — never the value.
func TestReadRedact_marksExposedAndNotifiesOnce(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	fake.SetTranscriptPath("/t.jsonl")
	fake.SetChat(secretChat(testSecretValue))

	if _, err := svc.Read(context.Background(), run); err != nil {
		t.Fatalf("Read: %v", err)
	}
	secretsList, err := st.RepoSecrets(context.Background(), run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(secretsList) != 1 {
		t.Fatalf("secrets = %d; want 1", len(secretsList))
	}
	rs := secretsList[0]
	if rs.ExposedRunID == nil || *rs.ExposedRunID != run.ID {
		t.Errorf("ExposedRunID = %v; want %q", rs.ExposedRunID, run.ID)
	}
	if rs.ExposedAt == nil {
		t.Error("ExposedAt = nil; want the exposure timestamp")
	}
	if n := cn.count(); n != 1 {
		t.Fatalf("notifications = %d; want exactly 1", n)
	}
	got := cn.last()
	want := Notification{
		Title: "Secret " + testSecretName + " exposed",
		Body:  "Its value appeared in proj~x's transcript. Rotate the secret to clear the exposure.",
		Tag:   run.ID + ":exposed:" + testSecretName,
		Route: "/runs/" + run.ID,
	}
	if got != want {
		t.Errorf("notification = %+v; want %+v", got, want)
	}
	if strings.Contains(got.Title+got.Body+got.Tag+got.Route, testSecretValue) {
		t.Error("the exposure notification carries the plaintext value")
	}
}

// 3. A second Read with the same content: no second notification (the flag is
// sticky), content still masked.
func TestReadRedact_stickyFlagNoSecondNotification(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	fake.SetTranscriptPath("/t.jsonl")
	fake.SetChat(secretChat(testSecretValue))
	if _, err := svc.Read(context.Background(), run); err != nil {
		t.Fatalf("first Read: %v", err)
	}

	// Fresh unmasked content for the second read (the fake shares its message
	// array with the first read's in-place mask).
	fake.SetChat(secretChat(testSecretValue))
	view, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if js := viewJSON(t, view); strings.Contains(js, testSecretValue) {
		t.Error("second read: plaintext survived")
	}
	if n := cn.count(); n != 1 {
		t.Errorf("notifications = %d after a second read; want still 1 (sticky flag)", n)
	}
}

// 4. Rotation clears the flag: content carrying the NEW value re-sets it and
// fires a second notification — a fresh edge after remediation.
func TestReadRedact_rotationReArmsTheEdge(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive)
	rs := seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	fake.SetTranscriptPath("/t.jsonl")
	fake.SetChat(secretChat(testSecretValue))
	if _, err := svc.Read(context.Background(), run); err != nil {
		t.Fatalf("first Read: %v", err)
	}

	const rotated = "rotated-fresh-77-value"
	if _, err := st.RotateRepoSecret(context.Background(), rs.ID, []byte(rotated), time.Now()); err != nil {
		t.Fatal(err)
	}
	fake.SetChat(secretChat(rotated))
	view, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if js := viewJSON(t, view); strings.Contains(js, rotated) {
		t.Error("rotated value survived in the rendered view")
	}
	after, err := st.RepoSecrets(context.Background(), run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].ExposedRunID == nil || *after[0].ExposedRunID != run.ID || after[0].ExposedAt == nil {
		t.Errorf("post-rotation exposure = %+v; want re-set to run %q", after[0], run.ID)
	}
	if n := cn.count(); n != 2 {
		t.Errorf("notifications = %d; want 2 (a fresh edge after rotation)", n)
	}
}

// 5. Broker-redaction placeholders alone — no value anywhere — change nothing:
// content untouched, no flag, no notification.
func TestReadRedact_placeholdersAloneAreInert(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	fake.SetTranscriptPath("/t.jsonl")
	text := "broker said [REDACTED:OTHER] and " + testPlaceholder + " already"
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: text}}})

	view, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := view.Messages[0].Text; got != text {
		t.Errorf("Text = %q; want unchanged %q", got, text)
	}
	after, err := st.RepoSecrets(context.Background(), run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].ExposedAt != nil || after[0].ExposedRunID != nil {
		t.Errorf("exposure flag set by placeholder text alone: %+v", after[0])
	}
	if n := cn.count(); n != 0 {
		t.Errorf("notifications = %d for placeholder-only content; want 0", n)
	}
}

// 6. Zero-secret repo: the Source returns a nil redactor and the read is a
// pure passthrough — no mask, no store write, no notification.
func TestReadRedact_zeroSecretRepoPassthrough(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive) // no seedSecret
	fake.SetTranscriptPath("/t.jsonl")
	text := "free text mentioning " + testSecretValue + " means nothing here"
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: text}}})

	view, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := view.Messages[0].Text; got != text {
		t.Errorf("Text = %q; want untouched passthrough", got)
	}
	if after, _ := st.RepoSecrets(context.Background(), run.RepoID); len(after) != 0 {
		t.Errorf("secrets = %+v; want none (no store writes)", after)
	}
	if n := cn.count(); n != 0 {
		t.Errorf("notifications = %d; want 0", n)
	}
}

// 7. Options.Secrets nil: the feature is absent — passthrough even with a
// secret row in the store.
func TestReadRedact_nilSecretsSeamIsAbsent(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newNotifyService(t, cn.notify, time.Minute) // no Secrets wired
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	fake.SetTranscriptPath("/t.jsonl")
	text := "the token is " + testSecretValue + " ok"
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: text}}})

	view, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := view.Messages[0].Text; got != text {
		t.Errorf("Text = %q; want untouched (feature absent)", got)
	}
	after, _ := st.RepoSecrets(context.Background(), run.RepoID)
	if after[0].ExposedAt != nil {
		t.Error("exposure flag set with a nil Secrets seam")
	}
	if n := cn.count(); n != 0 {
		t.Errorf("notifications = %d; want 0", n)
	}
}

// 7b. A secret inside a tool's rich View union (#146) — its Path/Text/Command
// are operator-visible free-form strings — is masked field-for-field, and the
// hit reports the secret by name (the exposure edge fires) exactly like the
// chip's Input/Output.
func TestReadRedact_masksToolView(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	fake.SetTranscriptPath("/t.jsonl")
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageTool, Tool: &provider.ToolInfo{
			Name: "Bash", Title: "Ran a command", Status: "ok",
			View: &provider.ToolView{
				Kind:    provider.ToolViewCommand,
				Path:    "/tmp/" + testSecretValue + ".env",
				Text:    "wrote token " + testSecretValue + " to disk",
				Command: "deploy --token " + testSecretValue,
			}}}}})

	view, err := svc.Read(context.Background(), run)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if js := viewJSON(t, view); strings.Contains(js, testSecretValue) {
		t.Error("plaintext value survived in a tool View")
	}
	gv := view.Messages[0].Tool.View
	if want := "/tmp/" + testPlaceholder + ".env"; gv.Path != want {
		t.Errorf("View.Path = %q; want %q", gv.Path, want)
	}
	if want := "wrote token " + testPlaceholder + " to disk"; gv.Text != want {
		t.Errorf("View.Text = %q; want %q", gv.Text, want)
	}
	if want := "deploy --token " + testPlaceholder; gv.Command != want {
		t.Errorf("View.Command = %q; want %q", gv.Command, want)
	}
	// The View hit reports the secret by name: exactly one exposure push, its
	// pinned title, and never the value.
	if n := cn.count(); n != 1 {
		t.Fatalf("notifications = %d; want exactly 1 (the View hit reports the name)", n)
	}
	got := cn.last()
	if got.Title != "Secret "+testSecretName+" exposed" {
		t.Errorf("notification Title = %q; want the exposure for %s", got.Title, testSecretName)
	}
	if strings.Contains(got.Title+got.Body+got.Tag+got.Route, testSecretValue) {
		t.Error("the exposure notification carries the plaintext value")
	}
}

// --- loop-level integration tests ------------------------------------------

// 8. needs-input body masking: the tailer masks BEFORE the notify gate
// snapshots its payload, so a needs-input push built from a dialog prompt
// carries the placeholder, never the value.
func TestTailerRedact_needsInputBodyIsMasked(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, _ := newSecretService(t, cn.notify, 20*time.Millisecond)
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
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

	// The dialog opens with the value in its prompt: the exposure push fires
	// immediately on the edge (undebounced) and the needs-input push after the
	// gate's window — both must be value-free.
	fake.SetPendingDialog(&provider.Dialog{Prompt: "deploy with " + testSecretValue + "?", Answerable: true})
	fake.SetSpoolSig("2")

	var needsInput Notification
	waitFor(t, func() bool {
		for _, n := range cn.all() {
			if n.Tag == run.ID {
				needsInput = n
				return true
			}
		}
		return false
	}, "the needs-input notification")
	if want := "deploy with " + testPlaceholder + "?"; needsInput.Body != want {
		t.Errorf("needs-input Body = %q; want %q", needsInput.Body, want)
	}
	for _, n := range cn.all() {
		if strings.Contains(n.Title+n.Body+n.Tag+n.Route, testSecretValue) {
			t.Errorf("notification %+v carries the plaintext value", n)
		}
	}
}

// 9. A transcript hit detected by the tailer tick alone (no HTTP Read) sends
// the exposure push and publishes run.changed.
func TestTailerRedact_tickHitNotifiesAndPublishesRunChanged(t *testing.T) {
	cn := &captureNotifier{}
	svc, st, fake, bus := newSecretService(t, cn.notify, time.Minute)
	run := seedRun(t, st, store.RunOutcomeActive)
	seedSecret(t, st, run.RepoID, testSecretName, testSecretValue)
	path := t.TempDir() + "/t.jsonl"
	writeFile(t, path, "{}")
	fake.SetTranscriptPath(path)
	fake.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1, Messages: []provider.Message{
		{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "leaked: " + testSecretValue}}})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	// Capture run.changed envelopes; subscribe before the loop starts so the
	// exposure's publish cannot be missed.
	sub, cancel := bus.Subscribe(ctx)
	defer cancel()
	var (
		mu      sync.Mutex
		changed []repoScopedPayload
	)
	go func() {
		for e := range sub {
			if p, ok := e.Payload.(repoScopedPayload); ok && e.Type == eventRunChanged {
				mu.Lock()
				changed = append(changed, p)
				mu.Unlock()
			}
		}
	}()

	go svc.Run(ctx)

	waitFor(t, func() bool { return cn.count() == 1 }, "the exposure notification off the tick loop")
	got := cn.last()
	if want := run.ID + ":exposed:" + testSecretName; got.Tag != want {
		t.Errorf("Tag = %q; want %q", got.Tag, want)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changed) >= 1
	}, "a run.changed publish for the exposure")
	mu.Lock()
	first := changed[0]
	mu.Unlock()
	if first.Type != eventRunChanged || first.RepoID != run.RepoID {
		t.Errorf("run.changed payload = %+v; want {%s %s}", first, eventRunChanged, run.RepoID)
	}
	// The flag stuck via the tick path too.
	after, err := st.RepoSecrets(context.Background(), run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].ExposedRunID == nil || *after[0].ExposedRunID != run.ID {
		t.Errorf("ExposedRunID = %v; want %q", after[0].ExposedRunID, run.ID)
	}
	// Give the loop a few more ticks: sticky, so the count must not climb.
	time.Sleep(50 * time.Millisecond)
	if n := cn.count(); n != 1 {
		t.Errorf("notifications = %d after settling; want exactly 1", n)
	}
}
