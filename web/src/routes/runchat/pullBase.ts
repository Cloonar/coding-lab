// Shared Pull base action (issue #58): the Run details surface and the idle
// status line both offer "Pull base" while the run is behind its base branch.
// It is not a dedicated endpoint — it sends the `/pull-base` lab command down
// the ordinary reply path, exactly as typing it in the composer would (the
// server intercepts lab commands, ADR-0063 / issue #149), so the reply's
// informational notice ("already up to date…") rides the same channel.

import { createSignal } from 'solid-js';
import { errorMessage, replyRun } from '../../api';

/** The lab command the action sends (the slash-command catalog's `/pull-base`). */
export const PULL_BASE_COMMAND = '/pull-base';

export function createPullBase(
  runID: () => string,
  onError: (message: string) => void,
  onNotice: (message: string) => void,
  onDone: () => void,
) {
  const [busy, setBusy] = createSignal(false);
  const run = async () => {
    if (busy()) return;
    setBusy(true);
    try {
      const result = await replyRun(runID(), PULL_BASE_COMMAND);
      if (result?.notice) onNotice(result.notice);
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      setBusy(false);
      onDone();
    }
  };
  return { busy, run };
}
