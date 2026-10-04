package instance

import (
	"context"
	"fmt"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// RunnerSettings is the slice of the store EffectiveRunner reads: the raw
// settings row, never a defaulting getter. *store.Store satisfies it; the
// narrow interface lets the resolver's table test script a store read error
// that a real database cannot produce on demand.
type RunnerSettings interface {
	GetSetting(ctx context.Context, key string) (string, error)
}

// EffectiveRunner resolves where a repo's instances execute — store.RunnerHost
// or store.RunnerContainer — and is the ONE place that answer is computed
// (issue #55). Every consumer that branches on a repo's Runner goes through
// it instead of reading repo.Runner: the container gate at spawn and the
// advisory write protection of read-only import snapshots (both in Launch,
// from one resolution per spawn), and the same protection in /pull-base's
// import refresh (internal/pull). One function, so the three can never
// disagree about what an inheriting repo means.
//
// The two layers: a non-nil repo.Runner is a pin and wins outright; nil
// inherits the global runner_default setting (store.SettingRunnerDefault),
// read here on every call — inheritance is live, so a changed default
// reaches every inheriting repo's NEXT spawn, and nothing is cached that
// could make two spawns disagree.
//
// It fails rather than guesses. The setting is read with GetSetting, not
// GetString-with-a-default: an absent row, a store error, or a value outside
// the enum is an error naming runner_default (and the bad value), telling the
// operator where to fix it — never a silent fall back to host, which would
// quietly hand an unsandboxed pane to a repo whose operator chose container.
// A pin outside the enum is refused the same way; reposvc.UpdateSettings
// never writes one, so only a hand-edited row can carry it, and reading it as
// host would be exactly that silent fall back. The caller decides what a
// failure costs: Launch refuses the spawn before the claim, /pull-base skips
// the advisory protection and logs.
func EffectiveRunner(ctx context.Context, settings RunnerSettings, repo store.Repo) (string, error) {
	if repo.Runner != nil {
		switch *repo.Runner {
		case store.RunnerHost, store.RunnerContainer:
			return *repo.Runner, nil
		}
		return "", fmt.Errorf("cannot resolve the Runner for repo %s: its pinned runner %q is neither %q nor %q — pick a Runner (or inherit the global default) in the repo's Runner settings",
			repo.Name, *repo.Runner, store.RunnerHost, store.RunnerContainer)
	}
	v, err := settings.GetSetting(ctx, store.SettingRunnerDefault)
	if err != nil {
		return "", fmt.Errorf("cannot resolve the Runner for repo %s: it inherits the global runner default, but the %s setting could not be read (%w) — set %s to %q or %q in Settings → Runner",
			repo.Name, store.SettingRunnerDefault, err, store.SettingRunnerDefault, store.RunnerHost, store.RunnerContainer)
	}
	switch v {
	case store.RunnerHost, store.RunnerContainer:
		return v, nil
	}
	return "", fmt.Errorf("cannot resolve the Runner for repo %s: it inherits the global runner default, but the %s setting holds %q, which is not a Runner — set %s to %q or %q in Settings → Runner",
		repo.Name, store.SettingRunnerDefault, v, store.SettingRunnerDefault, store.RunnerHost, store.RunnerContainer)
}
