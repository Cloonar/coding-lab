// The leave guard (issue #61, issue #85): leaving a settings page's area with
// pending changes opens an in-page dialog instead of a browser confirm.
//
//   Keep editing     stay; nothing changes.
//   Discard          drop the edits, then leave.
//   Save and leave   save; leave only if the save succeeded. A problem — a
//                    browser check, a refusal, a network error — keeps the
//                    operator here and is shown at its field (or in the save
//                    bar). Edits typed while the save was in flight are still
//                    pending afterwards, so the dialog asks about them again.
//
// "Leaving" is any in-app navigation to a URL the page's `inside` predicate
// rejects: the router's useBeforeLeave fires for EVERY URL change, so the
// guard looks at the destination and lets everything inside the area through.
// For the repo page that is all of /repos/:id/… — tab switches, section URLs,
// issue pages, the schedule editor URLs, and the route-notice helper's
// replace of the current URL; for global Settings, /settings/…. A tab close
// or reload is the form store's beforeunload prompt, not this dialog.
//
// One more thing passes: a navigation that carries a route notice
// (lib/routeNotice.ts). That is a page reporting an action that already
// happened — the repo was deleted — and there is nothing left to save or to
// keep editing.

import { useBeforeLeave, type BeforeLeaveEventArgs } from '@solidjs/router';
import { createEffect, createSignal } from 'solid-js';
import Dialog from '../Dialog';
import { takeRouteNotice } from '../../lib/routeNotice';
import { plural, useSettingsFormContext } from './form';

/** Whether a URL (path, with or without query and hash) is `base` or below it. */
export function isInsidePath(url: string, base: string): boolean {
  const path = url.split(/[?#]/, 1)[0] ?? '';
  return path === base || path.startsWith(`${base}/`);
}

export default function LeaveGuard(props: {
  /** Whether a destination (path, maybe with query and hash) is inside the area. */
  inside: (url: string) => boolean;
  /** What the changes are to, for the dialog's line ("coding-lab", "settings"). */
  subject: string;
}) {
  const form = useSettingsFormContext();
  // The navigation being held while the dialog is open.
  const [held, setHeld] = createSignal<BeforeLeaveEventArgs | null>(null);
  let keepButton: HTMLButtonElement | undefined;

  useBeforeLeave((event) => {
    if (event.defaultPrevented || !form.dirty()) return;
    // A number is a history move (Back/Forward): the browser has already put
    // the destination in the address bar when the router asks.
    const destination = typeof event.to === 'number' ? window.location.pathname : event.to;
    if (props.inside(destination)) return;
    if (takeRouteNotice({ state: event.options?.state }) !== null) return;
    event.preventDefault();
    setHeld(event);
  });

  // Every choice closes the dialog FIRST: it traps focus while open, and a
  // failed save must be able to put focus on the field with the problem.
  const release = (): BeforeLeaveEventArgs | null => {
    const event = held();
    setHeld(null);
    return event;
  };
  const stay = (): void => void release();
  // Nothing left to ask about (a refresh showed the server already holds the
  // edits): the navigation simply goes ahead.
  createEffect(() => {
    if (held() !== null && !form.dirty()) release()?.retry();
  });
  // retry() without force: nothing is pending by then, so this guard lets it
  // pass — and any other guard on the page still gets its say.
  const discardAndLeave = (): void => {
    const event = release();
    form.drop();
    event?.retry();
  };
  const saveAndLeave = async (): Promise<void> => {
    const event = release();
    // Not saved (a browser check, a refusal, a network error): stay. The
    // form store has already shown the problem at its field or in the save bar.
    if (await form.save()) event?.retry();
  };

  return (
    <Dialog
      open={held() !== null}
      onClose={stay}
      role="alertdialog"
      title={`Leave with ${plural(form.changed().length, 'unsaved change')}?`}
      initialFocus={() => keepButton}
      actions={
        <>
          <button type="button" ref={keepButton} onClick={stay}>
            Keep editing
          </button>
          {/* Not while a save is in flight: the edits would be dropped here
              and saved there all the same. */}
          <button type="button" onClick={discardAndLeave} disabled={form.busy()}>
            Discard
          </button>
          <button
            type="button"
            class="primary"
            onClick={() => void saveAndLeave()}
            disabled={form.busy()}
          >
            Save and leave
          </button>
        </>
      }
    >
      <p class="muted">Your changes to {props.subject} are not saved yet.</p>
    </Dialog>
  );
}
