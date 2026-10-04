package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// RunnerSettings is the slice of the store the two resolvers behind the
// Settings → Runner section read — EffectiveRunner (runner_default) and
// EffectiveDevImage (dev_image_default): the raw settings row, never a
// defaulting getter, because both must tell an absent row apart from an
// unreadable one. *store.Store satisfies it; the narrow interface lets the
// resolvers' table tests script a store read error that a real database
// cannot produce on demand.
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

// EffectiveDevImage resolves the dev image a container run of repo executes
// in, and is the ONE place that answer is computed (issue #55 / ADR-0071) —
// EffectiveRunner's twin for the image. The container gate
// (refuseContainerSpawn) asks it, and Launch carries the answer to both the
// pre-claim pull-if-missing (podmanx.EnsureImage) and the podman pane, so the
// image that is pulled is the image that runs. No consumer reads repo.ImageRef
// or the --container-image flag for a run's image directly.
//
// Three layers, the first one set wins: the repo's own image_ref (pinned by
// reposvc on save) outright, without reading the setting; else the global
// default dev image, the dev_image_default setting (store.SettingDevImageDefault,
// pinned by the settings PATCH), read here on every call so a re-saved
// default reaches every inheriting repo's NEXT spawn; else fallback, the
// --container-image flag — the deployed fallback dev image, the last layer.
//
// An absent or blank setting falls through to the flag; it is not an error.
// The row is deliberately unseeded, and clearing it is how an operator
// returns to the deployed default. Any OTHER read failure is an error naming
// dev_image_default, and it never falls back to the flag: that would quietly
// run a different image from the one the operator configured (ADR-0071's
// rejected option). With no layer set the error names all three knobs, since
// setting any one of them fixes it. The caller decides what an error costs:
// Launch refuses the spawn before the claim, as a 400.
//
// Provider login and the provider CLI containers deliberately do NOT come
// through here: they are repo-less and run the flag image alone
// (providercli.Config.Image, ADR-0057), whatever dev_image_default holds.
func EffectiveDevImage(ctx context.Context, settings RunnerSettings, repo store.Repo, fallback string) (string, error) {
	if repo.ImageRef != nil && *repo.ImageRef != "" {
		return *repo.ImageRef, nil
	}
	v, err := settings.GetSetting(ctx, store.SettingDevImageDefault)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Unseeded and never saved: fall through to the flag.
	case err != nil:
		return "", fmt.Errorf("cannot resolve the dev image for repo %s: it inherits the global default dev image, but the %s setting could not be read (%w) — the spawn is refused rather than falling back to --container-image; check Settings → Runner → Dev image, or set the repo's own Dev image in its Runner settings",
			repo.Name, store.SettingDevImageDefault, err)
	default:
		if v = strings.TrimSpace(v); v != "" {
			return v, nil
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no dev image for this repo — set the repo's Dev image (repo settings → Runner), the global default dev image (Settings → Runner → Dev image, %s), or the deployed fallback with --container-image",
		store.SettingDevImageDefault)
}
