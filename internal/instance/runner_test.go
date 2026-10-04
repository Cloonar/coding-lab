package instance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// fakeRunnerSettings is the settings seam of EffectiveRunner and
// EffectiveDevImage, scripted per case: a row value (present=true), an absent
// row, or a store read error — the last being the one case a real database
// cannot produce on demand. keys records every key read, in order.
type fakeRunnerSettings struct {
	value   string
	present bool
	err     error
	reads   int
	keys    []string
}

func (f *fakeRunnerSettings) GetSetting(_ context.Context, key string) (string, error) {
	f.reads++
	f.keys = append(f.keys, key)
	if f.err != nil {
		return "", f.err
	}
	if !f.present {
		return "", fmt.Errorf("setting %q: %w", key, store.ErrNotFound)
	}
	return f.value, nil
}

// TestEffectiveRunner pins the one effective-Runner resolver (issue #55): a
// pin wins without reading the setting; nil inherits runner_default; and
// every way the setting can fail — absent, unreadable, outside the enum —
// is an error that names runner_default (and the bad value, when there is
// one) and where to fix it, returning "" rather than ever falling back to
// host. A pin outside the enum (a hand-edited row) is refused too.
func TestEffectiveRunner(t *testing.T) {
	repo := func(pin *string) store.Repo { return store.Repo{Name: "proj", Runner: pin} }
	cases := []struct {
		name      string
		repo      store.Repo
		settings  *fakeRunnerSettings
		want      string
		wantReads int      // pins must not even read the setting
		wantErr   []string // substrings the error must carry; nil = success
		wantIs    error    // a wrapped cause the error must still match
	}{
		{name: "pinned host", repo: repo(new(store.RunnerHost)),
			settings: &fakeRunnerSettings{value: store.RunnerContainer, present: true}, want: store.RunnerHost},
		{name: "pinned container", repo: repo(new(store.RunnerContainer)),
			settings: &fakeRunnerSettings{value: store.RunnerHost, present: true}, want: store.RunnerContainer},
		{name: "inherit host", repo: repo(nil),
			settings: &fakeRunnerSettings{value: store.RunnerHost, present: true}, want: store.RunnerHost, wantReads: 1},
		{name: "inherit container", repo: repo(nil),
			settings: &fakeRunnerSettings{value: store.RunnerContainer, present: true}, want: store.RunnerContainer, wantReads: 1},
		{name: "absent runner_default", repo: repo(nil), settings: &fakeRunnerSettings{}, wantReads: 1,
			wantErr: []string{"repo proj", "inherits the global runner default", "runner_default", "not found", "Settings → Runner"},
			wantIs:  store.ErrNotFound},
		{name: "unreadable runner_default", repo: repo(nil), settings: &fakeRunnerSettings{err: errors.New("database is locked")}, wantReads: 1,
			wantErr: []string{"runner_default", "could not be read", "database is locked", "Settings → Runner"}},
		{name: "invalid runner_default", repo: repo(nil), settings: &fakeRunnerSettings{value: "podman", present: true}, wantReads: 1,
			wantErr: []string{"runner_default", `"podman"`, `"host" or "container"`, "Settings → Runner"}},
		{name: "blank runner_default", repo: repo(nil), settings: &fakeRunnerSettings{value: "", present: true}, wantReads: 1,
			wantErr: []string{"runner_default", `holds ""`}},
		{name: "invalid pin", repo: repo(new("podman")), settings: &fakeRunnerSettings{value: store.RunnerHost, present: true},
			wantErr: []string{"repo proj", `pinned runner "podman"`, "repo's Runner settings"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffectiveRunner(t.Context(), tc.settings, tc.repo)
			if tc.settings.reads != tc.wantReads {
				t.Errorf("setting reads = %d, want %d", tc.settings.reads, tc.wantReads)
			}
			if tc.wantErr == nil {
				if err != nil || got != tc.want {
					t.Fatalf("EffectiveRunner = %q, %v; want %q, nil", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("EffectiveRunner = %q, nil; want an error (never a fall back to host)", got)
			}
			if got != "" {
				t.Errorf("EffectiveRunner returned %q alongside its error, want \"\"", got)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %q does not wrap %v", err, tc.wantIs)
			}
		})
	}
}

// TestEffectiveDevImage pins the one effective-dev-image resolver (issue #55 /
// ADR-0071): repo image_ref → dev_image_default → the --container-image
// fallback, the first one set winning. A repo ref wins without even reading
// the setting; an absent, blank or whitespace setting falls through to the
// flag; a setting that cannot be read is an error naming dev_image_default —
// and returns "" rather than the flag image, which is set in that case on
// purpose; with nothing set anywhere the error names all three knobs.
func TestEffectiveDevImage(t *testing.T) {
	const (
		repoRef = "registry.example.com/repo/dev:v1@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
		setting = "registry.example.com/global/dev:v2@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
		flag    = "docker.io/library/debian:stable-slim"
	)
	repo := func(ref *string) store.Repo { return store.Repo{Name: "proj", ImageRef: ref} }
	threeKnobs := []string{"no dev image for this repo", "repo settings → Runner", "Settings → Runner → Dev image", "dev_image_default", "--container-image"}
	dbErr := errors.New("database is locked")
	cases := []struct {
		name      string
		repo      store.Repo
		settings  *fakeRunnerSettings
		fallback  string
		want      string
		wantReads int      // a repo ref must not even read the setting
		wantErr   []string // substrings the error must carry; nil = success
		wantIs    error    // a wrapped cause the error must still match
	}{
		{name: "repo ref beats the setting and the flag", repo: repo(new(repoRef)),
			settings: &fakeRunnerSettings{value: setting, present: true}, fallback: flag, want: repoRef},
		{name: "setting beats the flag", repo: repo(nil),
			settings: &fakeRunnerSettings{value: setting, present: true}, fallback: flag, want: setting, wantReads: 1},
		{name: "empty repo ref is no ref", repo: repo(new("")),
			settings: &fakeRunnerSettings{value: setting, present: true}, fallback: flag, want: setting, wantReads: 1},
		{name: "setting is trimmed", repo: repo(nil),
			settings: &fakeRunnerSettings{value: "  " + setting + "\n", present: true}, fallback: flag, want: setting, wantReads: 1},
		{name: "absent setting falls through to the flag", repo: repo(nil),
			settings: &fakeRunnerSettings{}, fallback: flag, want: flag, wantReads: 1},
		{name: "blank setting falls through to the flag", repo: repo(nil),
			settings: &fakeRunnerSettings{value: "", present: true}, fallback: flag, want: flag, wantReads: 1},
		{name: "whitespace setting falls through to the flag", repo: repo(nil),
			settings: &fakeRunnerSettings{value: " \t ", present: true}, fallback: flag, want: flag, wantReads: 1},
		{name: "unreadable setting refuses instead of using the flag", repo: repo(nil),
			settings: &fakeRunnerSettings{err: dbErr}, fallback: flag, wantReads: 1,
			wantErr: []string{"repo proj", "dev_image_default", "could not be read", "database is locked", "rather than falling back to --container-image"},
			wantIs:  dbErr},
		{name: "unreadable setting is irrelevant under a repo ref", repo: repo(new(repoRef)),
			settings: &fakeRunnerSettings{err: dbErr}, fallback: flag, want: repoRef},
		{name: "nothing set names all three knobs", repo: repo(nil),
			settings: &fakeRunnerSettings{}, wantReads: 1, wantErr: threeKnobs},
		{name: "blank setting and no flag names all three knobs", repo: repo(new("")),
			settings: &fakeRunnerSettings{value: "  ", present: true}, wantReads: 1, wantErr: threeKnobs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffectiveDevImage(t.Context(), tc.settings, tc.repo, tc.fallback)
			if tc.settings.reads != tc.wantReads {
				t.Errorf("setting reads = %d, want %d", tc.settings.reads, tc.wantReads)
			}
			for _, k := range tc.settings.keys {
				if k != store.SettingDevImageDefault {
					t.Errorf("read setting %q, want only %q", k, store.SettingDevImageDefault)
				}
			}
			if tc.wantErr == nil {
				if err != nil || got != tc.want {
					t.Fatalf("EffectiveDevImage = %q, %v; want %q, nil", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("EffectiveDevImage = %q, nil; want an error (never a silent fall back to the flag)", got)
			}
			if got != "" {
				t.Errorf("EffectiveDevImage returned %q alongside its error, want \"\"", got)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %q does not wrap %v", err, tc.wantIs)
			}
		})
	}
}

// isContainerPane reports whether a recorded pane argv is the container
// runner's `podman run …` (issue #205) rather than the host pane's bare
// provider argv.
func isContainerPane(argv []string) bool {
	return len(argv) > 1 && argv[0] == testPodmanBin && slices.Contains(argv, "run")
}

// An inheriting repo spawns on whatever runner_default says AT SPAWN TIME, and
// a change of the setting flips the NEXT spawn — inheritance is live, nothing
// is cached — while a pinned repo ignores the setting in both directions
// (issue #55). One fixture walks the sequence with the container seam wired
// green, so each step's pane argv is what tells the effective Runner apart.
func TestStart_runnerDefaultInheritanceIsLive(t *testing.T) {
	f := newFixture(t) // the fixture repo is created with Runner nil: inherit
	f.wireContainer()

	steps := []struct {
		name          string
		def           string  // runner_default before this spawn
		pin           *string // the repo's runner before this spawn; nil = inherit
		wantContainer bool
	}{
		{"inherit, default host", store.RunnerHost, nil, false},
		{"inherit, default flipped to container", store.RunnerContainer, nil, true},
		{"pinned host ignores a container default", store.RunnerContainer, new(store.RunnerHost), false},
		{"pinned container ignores a host default", store.RunnerHost, new(store.RunnerContainer), true},
		{"pin cleared, back to inheriting host", store.RunnerHost, nil, false},
	}
	for i, st := range steps {
		if err := f.st.SetSetting(t.Context(), store.SettingRunnerDefault, st.def); err != nil {
			t.Fatalf("%s: SetSetting(runner_default): %v", st.name, err)
		}
		if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{Runner: store.Set(st.pin)}); err != nil {
			t.Fatalf("%s: UpdateRepoSettings(runner): %v", st.name, err)
		}
		run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: fmt.Sprintf("step%d", i)})
		if err != nil {
			t.Fatalf("%s: Start: %v", st.name, err)
		}
		sess, live := f.runner.Session(run.SessionName)
		if !live {
			t.Fatalf("%s: session %s not live", st.name, run.SessionName)
		}
		if got := isContainerPane(sess.Argv); got != st.wantContainer {
			t.Errorf("%s: container pane = %v, want %v (argv %q)", st.name, got, st.wantContainer, sess.Argv)
		}
		if sess.NoNofileCap != st.wantContainer {
			t.Errorf("%s: NoNofileCap = %v, want %v (the prlimit cap is retired for container panes only)",
				st.name, sess.NoNofileCap, st.wantContainer)
		}
	}
}

// An inheriting repo whose runner_default is absent or invalid is refused
// BEFORE anything exists (issue #55) — a *BadRequestError (400) whose message
// names the setting — exactly like TestStart_containerRefusals: no worktree,
// no branch, no run row, no session (in particular no host pane: the refusal
// never falls back to host), no per-run tree. A pinned repo never reads the
// setting, so the same broken default leaves its spawns alone.
func TestStart_unresolvableRunnerRefusedBeforeClaim(t *testing.T) {
	cases := []struct {
		name    string
		opts    fixtureOpts
		def     string // runner_default to write; "" = leave as the fixture has it
		wantMsg []string
	}{
		{name: "absent runner_default", opts: fixtureOpts{noSeed: true},
			wantMsg: []string{"runner_default", "not found", "Settings → Runner"}},
		{name: "invalid runner_default", def: "podman",
			wantMsg: []string{"runner_default", `"podman"`, "Settings → Runner"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWith(t, tc.opts)
			if tc.def != "" {
				if err := f.st.SetSetting(t.Context(), store.SettingRunnerDefault, tc.def); err != nil {
					t.Fatalf("SetSetting(runner_default): %v", err)
				}
			}

			_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
			if err == nil {
				t.Fatal("Start succeeded, want the unresolvable-Runner refusal")
			}
			var bad *BadRequestError
			if !errors.As(err, &bad) {
				t.Errorf("refusal error type = %T (%v), want *BadRequestError (the 400 mapping)", err, err)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}

			// Refused BEFORE the claim: nothing was created anywhere.
			if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
				t.Error("refused spawn created a worktree")
			}
			if f.branchExists("lab/20260608-1530") {
				t.Error("refused spawn created a branch")
			}
			if _, err := f.st.RunBySession(t.Context(), "proj~20260608-1530"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("RunBySession after refusal: %v, want ErrNotFound (no run row)", err)
			}
			if _, live := f.runner.Session("proj~20260608-1530"); live {
				t.Error("refused spawn left a session — an unresolvable Runner must never start a host pane")
			}
			if dirExists(f.instancesDir) {
				t.Error("refused spawn materialized a per-run tree")
			}

			// The same broken default is invisible to a pinned repo.
			if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
				Runner: store.Set(new(store.RunnerHost)),
			}); err != nil {
				t.Fatalf("UpdateRepoSettings(runner=host): %v", err)
			}
			run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
			if err != nil {
				t.Fatalf("pinned-host Start under a broken runner_default: %v", err)
			}
			if sess, live := f.runner.Session(run.SessionName); !live || isContainerPane(sess.Argv) {
				t.Errorf("pinned-host spawn: live=%v argv=%q, want a live host pane", live, sess.Argv)
			}
		})
	}
}
