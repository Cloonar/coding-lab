// The shared per-section settings form primitive (issue #198): dirty
// tracking + dirty-fields-only PATCH submit + the unsaved-changes guard.
// Save semantics — and the exact 'Nothing to save.' / 'Saved.' strings — are
// lifted verbatim from the monolith Settings/RepoSettings save() handlers so
// the category subpages built on top behave byte-identically. The guard
// covers both exits: in-app navigation via the router's useBeforeLeave and a
// tab close/reload via beforeunload. It never fires while clean, and
// device-local sections (push devices, install) simply don't use this
// primitive — guard-free by construction.

import { createSignal, onCleanup } from 'solid-js';
import type { Accessor } from 'solid-js';
import { useBeforeLeave } from '@solidjs/router';
import { errorMessage } from '../../api';

export interface SettingsFormController {
  busy: Accessor<boolean>;
  error: Accessor<string | null>;
  note: Accessor<string | null>;
  setError(message: string | null): void;
  setNote(message: string | null): void;
  save(event: SubmitEvent): Promise<void>;
}

export function useSettingsForm<TPatch extends object>(opts: {
  /** True while any draft differs from its seed — arms the leave guard. */
  dirty: Accessor<boolean>;
  /** A string result is a validation error to surface; it blocks the save. */
  buildPatch: () => TPatch | string;
  submit: (patch: TPatch) => Promise<unknown>;
  onSaved?: () => void;
}): SettingsFormController {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [note, setNote] = createSignal<string | null>(null);

  const save = async (event: SubmitEvent): Promise<void> => {
    event.preventDefault();
    setError(null);
    setNote(null);
    const patch = opts.buildPatch();
    if (typeof patch === 'string') {
      setError(patch);
      return;
    }
    if (Object.keys(patch).length === 0) {
      setNote('Nothing to save.');
      return;
    }
    setBusy(true);
    try {
      await opts.submit(patch);
      setNote('Saved.');
      opts.onSaved?.();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // In-app navigation guard: block first, then retry(true) past ALL guards on
  // confirm — the documented router pattern (retry without force would re-run
  // this listener and block again forever).
  useBeforeLeave((e) => {
    if (!opts.dirty() || e.defaultPrevented) return;
    e.preventDefault();
    if (window.confirm('Discard unsaved changes?')) e.retry(true);
  });

  // Tab close/reload guard, mounted for the section's lifetime. Chrome's
  // legacy contract: preventDefault AND set returnValue to arm the prompt.
  const onBeforeUnload = (event: BeforeUnloadEvent): void => {
    if (!opts.dirty()) return;
    event.preventDefault();
    event.returnValue = '';
  };
  window.addEventListener('beforeunload', onBeforeUnload);
  onCleanup(() => window.removeEventListener('beforeunload', onBeforeUnload));

  return {
    busy,
    error,
    note,
    setError: (message) => void setError(message),
    setNote: (message) => void setNote(message),
    save,
  };
}
