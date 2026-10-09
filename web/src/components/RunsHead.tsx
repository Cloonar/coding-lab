// The Runs page's head (issue #76), shared by its two sides — Live at `/`
// (routes/Runs) and Ended at `/history` (routes/History). Below 1024px it is
// the brand `lab.` and the SSE live dot: with the drawer's top strip retired,
// this is the only place the brand appears on a phone. At >=1024px the rail
// carries the brand, so the head is a "Runs" heading instead.
//
// The Live / Ended switch is two links styled as Segmented's pill (links, not
// radios: each side has its own URL, and History keeps its /history). The
// router marks the current side aria-current="page". The Live link carries the
// live-run count when the shell's instances context is present; it reads the
// context softly (useContext, not useShellInstances) so the head never throws
// where no shell is mounted.

import { A } from '@solidjs/router';
import { Show, useContext } from 'solid-js';
import { useEvents } from '../events';
import { createMediaQuery } from '../lib/media';
import { ShellInstancesContext } from '../lib/shellInstances';

export default function RunsHead() {
  const desktop = createMediaQuery('(min-width: 1024px)');
  const events = useEvents();
  const shell = useContext(ShellInstancesContext);
  const liveCount = () => shell?.all().filter((instance) => instance.live).length ?? 0;

  const viewSwitch = () => (
    <nav class="runs-switch" aria-label="Runs view">
      <A href="/" end class="runs-switch-link">
        Live
        <Show when={liveCount() > 0}> · {liveCount()}</Show>
      </A>
      <A href="/history" end class="runs-switch-link">
        Ended
      </A>
    </nav>
  );

  return (
    <>
      <header class="runs-head">
        <Show
          when={desktop()}
          fallback={
            <>
              {/* The page still owns a heading for assistive tech. */}
              <h1 class="visually-hidden">Runs</h1>
              <A href="/" class="brand plain runs-brand">
                lab<span class="brand-dot">.</span>
              </A>
              <span class="spacer" />
              <span
                classList={{ 'live-dot': true, on: events.connected() }}
                role="status"
                aria-label={events.connected() ? 'Live' : 'Reconnecting'}
                title={events.connected() ? 'Live' : 'Reconnecting…'}
              />
            </>
          }
        >
          <h1>Runs</h1>
          <span class="spacer" />
          {viewSwitch()}
        </Show>
      </header>
      <Show when={!desktop()}>{viewSwitch()}</Show>
    </>
  );
}
