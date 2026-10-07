// The banners directly above the New run composer's field (issue #66): what
// blocks — or warns about — a run, said where the run is started, and nothing
// else. A repo that can run shows nothing at all; the full readiness report
// stays on the repo home.
//
// The blockers come from lib/newRun.ts's composerBlockers(), already ordered
// (errors, the warning, notices) and each carrying its remedy: a link
// (Reconnect → /credentials for a logged-out agent, Fix → the settings field
// a failing tracker check names) or the clone Retry, which the page wires to
// the existing retryClone api. After them, whenever the effective Runner is
// `host`, the host-Runner warning.
//
// Variants map onto Banner's: 'error' stays the alerting error; 'notice' and
// 'warning' both render Banner's amber 'notice' palette with a polite
// role="status" — a failing tracker check leaves the field enabled, so it is
// no emergency, and the mockup draws it in the same amber as the cloning
// notice. The `composer-blocker-<kind>` class keeps the two apart for styling
// and tests.

import { A } from '@solidjs/router';
import { For, Show, type JSX } from 'solid-js';
import { HOST_RUNNER_WARNING, type Blocker } from '../../lib/newRun';
import Banner from '../Banner';

const BANNER_VARIANT: Record<Blocker['variant'], 'error' | 'notice'> = {
  error: 'error',
  warning: 'notice',
  notice: 'notice',
};

export default function ComposerBlockers(props: {
  /** From composerBlockers() in lib/newRun.ts, already ordered. */
  blockers: Blocker[];
  /** The effective runner is 'host' → the HOST_RUNNER_WARNING banner. */
  hostRunner: boolean;
  /** The clone-failed Retry (the page calls the existing retryClone api). */
  onRetryClone: () => void;
  retrying: boolean;
}): JSX.Element {
  const action = (blocker: Blocker): JSX.Element => {
    if (blocker.retryClone === true) {
      // aria-disabled rather than disabled while busy: the button keeps focus
      // and its place in the tab order, and a second tap is simply ignored.
      return (
        <button
          type="button"
          class="composer-blocker-action"
          classList={{ busy: props.retrying }}
          aria-disabled={props.retrying ? 'true' : undefined}
          onClick={() => {
            if (!props.retrying) props.onRetryClone();
          }}
        >
          {props.retrying ? 'Retrying…' : 'Retry'}
        </button>
      );
    }
    if (blocker.fixHref !== undefined) {
      return (
        <A href={blocker.fixHref} class="composer-blocker-action">
          {blocker.fixLabel ?? 'Fix'}
        </A>
      );
    }
    return undefined;
  };

  return (
    <Show when={props.blockers.length > 0 || props.hostRunner}>
      <div class="composer-blockers">
        <For each={props.blockers}>
          {(blocker) => (
            <Banner
              message={blocker.message}
              variant={BANNER_VARIANT[blocker.variant]}
              class={`composer-blocker composer-blocker-${blocker.kind}`}
              action={action(blocker)}
            />
          )}
        </For>
        <Show when={props.hostRunner}>
          <Banner
            message={HOST_RUNNER_WARNING}
            variant="notice"
            class="composer-blocker composer-blocker-host"
          />
        </Show>
      </div>
    </Show>
  );
}
