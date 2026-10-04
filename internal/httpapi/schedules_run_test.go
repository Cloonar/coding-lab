package httpapi

// Schedule Run now (issue #61) over the engine-backed AFK harness, plus the
// Schedules list's derived next_run_*/last_run fields over the store-only
// Schedule harness. The engine's own Run now rules (pass ordering, cadence
// memo, failure accounting) are pinned in internal/afk.

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// seedSchedule stores a Schedule on repoID directly — the Run now suite needs
// rows in states (paused) the API deliberately cannot write.
func seedSchedule(t *testing.T, st *store.Store, repoID, name string, mut func(*store.Schedule)) store.Schedule {
	t.Helper()
	sc := store.Schedule{
		ID: ids.NewID("sched"), RepoID: repoID, Name: name,
		Cadence: "0 6 * * 1", Prompt: "Investigate dependency updates.",
		Flows: []string{"autolander"}, Enabled: true,
		CreatedAt: afkClock, UpdatedAt: afkClock,
	}
	if mut != nil {
		mut(&sc)
	}
	created, err := st.CreateSchedule(context.Background(), sc)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return created
}

func runNowPath(repoID, schedID string) string {
	return "/api/v1/repos/" + repoID + "/schedules/" + schedID + "/run"
}

// The happy path: 202 with handleAFKStart's {run} envelope, an ordinary
// scheduled run attributed by schedule_id — on a switched-off Schedule, which
// is how a prompt is tested before the cadence is armed — the run.changed the
// runs rail refetches on, schedule_id on the runs history too, and the
// Schedules list's last_run pointing at it.
func TestAPI_ScheduleRunNow(t *testing.T) {
	x := newAFKServer(t)
	log := recordBus(t, x.bus)
	sc := seedSchedule(t, x.st, x.repo.ID, "deps", func(s *store.Schedule) { s.Enabled = false })

	resp := x.do("POST", runNowPath(x.repo.ID, sc.ID), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusAccepted)
	run, ok := decodeBody(t, resp)["run"].(map[string]any)
	if !ok {
		t.Fatalf("202 body has no run envelope")
	}
	if run["kind"] != "scheduled" || run["outcome"] != "active" {
		t.Fatalf("run kind/outcome = %v/%v, want scheduled/active", run["kind"], run["outcome"])
	}
	if run["schedule_id"] != sc.ID {
		t.Errorf("schedule_id = %v, want %s", run["schedule_id"], sc.ID)
	}
	if run["issue_number"] != nil {
		t.Errorf("issue_number = %v, want null", run["issue_number"])
	}
	label := afk.ScheduleLabel(sc.ID, afkClock)
	if run["session_name"] != "proj~"+label || run["branch"] != "lab/"+label {
		t.Errorf("session/branch = %v/%v, want the scheduled identity for %s", run["session_name"], run["branch"], label)
	}
	if want := store.FormatTime(afkClock.Add(30 * time.Minute)); run["budget_deadline"] != want {
		t.Errorf("budget_deadline = %v, want %v (the ordinary scheduled-run budget)", run["budget_deadline"], want)
	}
	if _, live := x.runner.Session("proj~" + label); !live {
		t.Error("no live tmux session for the Run now")
	}
	if !sawEvent(log, "run.changed") {
		t.Error("no run.changed event on Run now")
	}

	// Attribution on the wire: the runs history carries schedule_id, and a
	// run of another kind carries the key as null.
	manual, err := x.st.CreateRun(context.Background(), store.Run{
		ID: ids.NewID("run"), RepoID: x.repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/scratch", WorktreePath: "/wt/y", SessionName: "proj~manual",
		Model: "opus[1m]", Effort: "max", StartedAt: afkClock.Add(-time.Hour), Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	resp = x.do("GET", "/api/v1/runs?repo="+x.repo.ID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	byID := map[string]map[string]any{}
	for _, raw := range decodeBody(t, resp)["runs"].([]any) {
		row := raw.(map[string]any)
		byID[row["id"].(string)] = row
	}
	if got := byID[run["id"].(string)]["schedule_id"]; got != sc.ID {
		t.Errorf("history schedule_id = %v, want %s", got, sc.ID)
	}
	if got, present := byID[manual.ID]["schedule_id"]; !present || got != nil {
		t.Errorf("manual run schedule_id = %v (present=%v), want present as null", got, present)
	}

	// The Schedules list: last_run is the Run now's run, live; a switched-off
	// Schedule has no next cadence firing.
	resp = x.do("GET", "/api/v1/repos/"+x.repo.ID+"/schedules", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	row := schedulesOf(t, decodeBody(t, resp))[0]
	last, ok := row["last_run"].(map[string]any)
	if !ok {
		t.Fatalf("last_run = %v, want the Run now's run", row["last_run"])
	}
	if last["id"] != run["id"] || last["outcome"] != "active" || last["ended_at"] != nil ||
		last["started_at"] != run["started_at"] {
		t.Errorf("last_run = %v, want the live run %v", last, run["id"])
	}
	if row["next_run_at"] != nil || row["next_run_display"] != nil {
		t.Errorf("next_run = %v/%v, want null for a switched-off Schedule", row["next_run_at"], row["next_run_display"])
	}
	// A Run now is not a cadence firing.
	if row["last_fired_at"] != nil {
		t.Errorf("last_fired_at = %v, want null", row["last_fired_at"])
	}
}

// The 409 matrix: each refusal's exact message (the UI shows it verbatim),
// and nothing launched.
func TestAPI_ScheduleRunNowConflictMatrix(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, x *afkTestServer, sc store.Schedule)
		want  string
	}{
		{
			name: "paused",
			setup: func(t *testing.T, x *afkTestServer, sc store.Schedule) {
				if _, err := x.st.SetSchedulePaused(context.Background(), sc.ID, true); err != nil {
					t.Fatal(err)
				}
			},
			want: "schedule is paused after consecutive failures — re-enable to re-arm",
		},
		{
			name: "previous run still live",
			setup: func(t *testing.T, x *afkTestServer, sc store.Schedule) {
				resp := x.do("POST", runNowPath(x.repo.ID, sc.ID), nil, csrfHeaders(x.ts.URL))
				wantStatus(t, resp, http.StatusAccepted)
				_ = resp.Body.Close()
			},
			want: "schedule's previous run is still live",
		},
		{
			name: "at cap",
			setup: func(t *testing.T, x *afkTestServer, sc store.Schedule) {
				if err := x.st.SetSetting(context.Background(), store.SettingMaxInstances, "1"); err != nil {
					t.Fatal(err)
				}
				x.runner.AddLive("proj~existing")
			},
			want: "instance cap reached",
		},
		{
			name: "logged out",
			setup: func(t *testing.T, x *afkTestServer, sc store.Schedule) {
				x.prov.SetLoggedIn(false)
			},
			want: "provider is logged out",
		},
		{
			// Run now, then the operator's Stop (neutral: worktree and branch
			// survive), then Run now again inside the same minute — the
			// engine clock is fixed, so the label would collide.
			name: "already started a run this minute",
			setup: func(t *testing.T, x *afkTestServer, sc store.Schedule) {
				h := csrfHeaders(x.ts.URL)
				resp := x.do("POST", runNowPath(x.repo.ID, sc.ID), nil, h)
				wantStatus(t, resp, http.StatusAccepted)
				run := decodeBody(t, resp)["run"].(map[string]any)
				resp = x.do("DELETE", "/api/v1/instances/"+url.PathEscape(run["session_name"].(string)), nil, h)
				wantStatus(t, resp, http.StatusOK)
				_ = resp.Body.Close()
			},
			want: "schedule already started a run this minute — try again in a minute",
		},
	}
	// The pinned strings ARE the sentinels' messages.
	if cases[0].want != afk.ErrSchedulePaused.Error() || cases[1].want != afk.ErrScheduleRunLive.Error() ||
		cases[2].want != instance.ErrOverCap.Error() || cases[3].want != instance.ErrLoggedOut.Error() ||
		cases[4].want != afk.ErrScheduleStartedThisMinute.Error() {
		t.Fatal("a pinned refusal string drifted from its sentinel")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newAFKServer(t)
			sc := seedSchedule(t, x.st, x.repo.ID, "deps", nil)
			tc.setup(t, x, sc)
			before, err := x.st.RunsByRepo(context.Background(), x.repo.ID, 0)
			if err != nil {
				t.Fatal(err)
			}

			resp := x.do("POST", runNowPath(x.repo.ID, sc.ID), nil, csrfHeaders(x.ts.URL))
			wantStatus(t, resp, http.StatusConflict)
			if got := decodeBody(t, resp)["error"]; got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
			after, err := x.st.RunsByRepo(context.Background(), x.repo.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Errorf("runs %d → %d, want a refused Run now to launch nothing", len(before), len(after))
			}
		})
	}
}

// A Schedule of another repo is a 404 — never a cross-repo launch — like an
// unknown Schedule or repo.
func TestAPI_ScheduleRunNowNotFound(t *testing.T) {
	x := newAFKServer(t)
	other := seedTrackerRepo(t, x.testServer, "other", nil)
	sc := seedSchedule(t, x.st, x.repo.ID, "deps", nil)
	h := csrfHeaders(x.ts.URL)

	for _, path := range []string{
		runNowPath(other.ID, sc.ID),            // cross-repo
		runNowPath(x.repo.ID, "sched_missing"), // unknown Schedule
		runNowPath("repo_missing", sc.ID),      // unknown repo
	} {
		resp := x.do("POST", path, nil, h)
		wantStatus(t, resp, http.StatusNotFound)
		_ = resp.Body.Close()
	}
	if runs, err := x.st.RunsByRepo(context.Background(), x.repo.ID, 0); err != nil || len(runs) != 0 {
		t.Fatalf("runs = %d (err %v), want none launched", len(runs), err)
	}
}

// The Schedules list's derived fields, on every response that carries a
// Schedule: next_run_at/next_run_display are exactly /cron/preview's first
// entry for an enabled, unpaused Schedule and null otherwise; last_run is the
// newest run of THIS Schedule in the run outcome vocabulary, null until one
// exists.
func TestScheduleListNextRunAndLastRun(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/schedules"
	ctx := context.Background()

	// A weekly cadence, so the next match cannot move between the two reads
	// below unless the test runs across Monday 06:00 itself.
	resp := x.do("POST", base, map[string]any{"name": "weekly", "cadence": "0 6 * * 1", "prompt": "p"}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	id := created["id"].(string)
	for _, key := range []string{"next_run_at", "next_run_display", "last_run"} {
		if _, ok := created[key]; !ok {
			t.Fatalf("create response is missing key %q: %v", key, created)
		}
	}
	if created["last_run"] != nil {
		t.Fatalf("fresh last_run = %v, want null", created["last_run"])
	}

	resp = x.do("GET", "/api/v1/cron/preview?expr="+url.QueryEscape("0 6 * * 1"), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	preview := decodeBody(t, resp)
	wantAt := stringsOf(t, preview, "next")[0]
	wantDisplay := stringsOf(t, preview, "next_display")[0]
	if created["next_run_at"] != wantAt || created["next_run_display"] != wantDisplay {
		t.Fatalf("create next_run = %v/%v, want the preview's %s/%s",
			created["next_run_at"], created["next_run_display"], wantAt, wantDisplay)
	}

	list := func() map[string]any {
		t.Helper()
		resp := x.do("GET", base, nil, nil)
		wantStatus(t, resp, http.StatusOK)
		return schedulesOf(t, decodeBody(t, resp))[0]
	}
	row := list()
	if row["next_run_at"] != wantAt || row["next_run_display"] != wantDisplay {
		t.Fatalf("list next_run = %v/%v, want %s/%s", row["next_run_at"], row["next_run_display"], wantAt, wantDisplay)
	}
	at, err := time.Parse(time.RFC3339, wantAt)
	if err != nil || at.Weekday() != time.Monday || at.Hour() != 6 || !at.After(time.Now()) {
		t.Fatalf("next_run_at = %s (%v), want a future Monday 06:00", wantAt, err)
	}

	// last_run follows this Schedule's newest run — live, then ended — and
	// ignores another Schedule's newer one.
	mkRun := func(label string, sched *string, started time.Time) store.Run {
		t.Helper()
		r, err := x.st.CreateRun(ctx, store.Run{
			ID: ids.NewID("run"), RepoID: repo.ID, Kind: store.RunKindScheduled, Provider: "claude-code",
			ScheduleID: sched, Branch: "lab/" + label, WorktreePath: "/wt/" + label,
			SessionName: "proj~" + label, Model: "opus[1m]", Effort: "max",
			StartedAt: started, Outcome: store.RunOutcomeActive,
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		return r
	}
	started := time.Date(2026, 7, 6, 6, 0, 0, 0, time.UTC)
	run := mkRun("sched-1", &id, started)
	other := seedSchedule(t, x.st, repo.ID, "zz-other", nil)
	mkRun("sched-2", &other.ID, started.Add(time.Hour))

	last, ok := list()["last_run"].(map[string]any)
	if !ok || last["id"] != run.ID || last["outcome"] != "active" || last["ended_at"] != nil ||
		last["started_at"] != store.FormatTime(started) {
		t.Fatalf("live last_run = %v, want run %s active with ended_at null", last, run.ID)
	}
	if len(last) != 4 {
		t.Errorf("last_run = %v, want exactly id/started_at/ended_at/outcome", last)
	}
	ended := started.Add(5 * time.Minute)
	if err := x.st.EndRun(ctx, run.ID, store.RunOutcomeDeath, ended, "session died"); err != nil {
		t.Fatalf("EndRun: %v", err)
	}
	last, _ = list()["last_run"].(map[string]any)
	if last["outcome"] != "death" || last["ended_at"] != store.FormatTime(ended) {
		t.Fatalf("ended last_run = %v, want death ended at %s", last, store.FormatTime(ended))
	}

	// No cadence firing coming: switched off (PATCH response included) …
	resp = x.do("PATCH", base+"/"+id, map[string]any{"enabled": false}, h)
	wantStatus(t, resp, http.StatusOK)
	patched := decodeBody(t, resp)
	if patched["next_run_at"] != nil || patched["next_run_display"] != nil {
		t.Errorf("disabled next_run = %v/%v, want null", patched["next_run_at"], patched["next_run_display"])
	}
	if patched["last_run"] == nil {
		t.Error("PATCH response dropped last_run")
	}
	// … or paused, even while enabled; the re-enable response re-arms it.
	resp = x.do("PATCH", base+"/"+id, map[string]any{"enabled": true}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	if _, err := x.st.SetSchedulePaused(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if row := list(); row["next_run_at"] != nil || row["next_run_display"] != nil {
		t.Errorf("paused next_run = %v/%v, want null", row["next_run_at"], row["next_run_display"])
	}
	resp = x.do("POST", base+"/"+id+"/reenable", nil, h)
	wantStatus(t, resp, http.StatusOK)
	if reenabled := decodeBody(t, resp); reenabled["next_run_at"] != wantAt {
		t.Errorf("re-enabled next_run_at = %v, want %s", reenabled["next_run_at"], wantAt)
	}
}
