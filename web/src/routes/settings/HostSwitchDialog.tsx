// The host-switch question (issues #55, #85): a Save whose PATCH switches the
// global runner default to host opens this in-page dialog before anything is
// sent — the same message the browser confirm used to carry, naming how many
// repos inherit the default, because the flip reaches every one of them at its
// next spawn and host runs are unsandboxed.
//
//   Cancel           nothing is sent; every edit stays pending.
//   Switch to host   the Save goes ahead with the whole pending patch.
//
// Escape and the scrim are Cancel. Focus starts on Cancel: the safe answer is
// the one a stray Enter gives. The form store asks (form.tsx `hostSwitch`);
// this only shows the question and hands back the answer. A switch to
// container never asks.

import Dialog from '../../components/Dialog';
import { useGlobalSettingsForm } from './form';

/** The dialog's body: what the switch does, naming the inheriting repos. */
export function hostSwitchMessage(count: number | null): string {
  if (count === null) {
    return 'Repos that inherit it will run their next sessions unsandboxed with full host access.';
  }
  if (count === 0) {
    return 'No repos inherit it now, but new repos will, and their sessions will run unsandboxed with full host access.';
  }
  if (count === 1) {
    return '1 repo inherits it and its next session will run unsandboxed with full host access.';
  }
  return `${count} repos inherit it and their next sessions will run unsandboxed with full host access.`;
}

export default function HostSwitchDialog() {
  const form = useGlobalSettingsForm();
  let cancelButton: HTMLButtonElement | undefined;
  const cancel = (): void => form.hostSwitch.answer(false);

  return (
    <Dialog
      open={form.hostSwitch.open()}
      onClose={cancel}
      role="alertdialog"
      title="Switch the global runner default to Host?"
      initialFocus={() => cancelButton}
      actions={
        <>
          <button type="button" ref={cancelButton} onClick={cancel}>
            Cancel
          </button>
          <button type="button" class="danger" onClick={() => form.hostSwitch.answer(true)}>
            Switch to host
          </button>
        </>
      }
    >
      <p class="muted">{hostSwitchMessage(form.inheritingRepos())}</p>
    </Dialog>
  );
}
