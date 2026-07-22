// useSettingsForm contract (issue #198): the save() pipeline (dirty-only
// patch → submit; the exact 'Saved.' / 'Nothing to save.' strings; a string
// buildPatch result and a rejected submit both surface as the error) and the
// unsaved-changes guard (router leave intercepted while dirty — confirm=false
// stays, confirm=true retries past the guard; beforeunload armed only while
// dirty). The host mounts in a MemoryRouter with two routes so real
// navigation exercises useBeforeLeave.

import { MemoryRouter, Route, createMemoryHistory, useNavigate } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Mock } from 'vitest';
import type { MemoryHistory } from '@solidjs/router';
import { ApiError } from '../../api';
import { useSettingsForm } from './useSettingsForm';

type Patch = Record<string, unknown>;

// Per-test knobs the host closes over; reset in beforeEach.
let dirtyFlag = false;
let patchResult: Patch | string = {};
let submitMock: Mock<(patch: Patch) => Promise<unknown>>;
let onSavedMock: Mock<() => void>;

function Host() {
  const navigate = useNavigate();
  const form = useSettingsForm<Patch>({
    dirty: () => dirtyFlag,
    buildPatch: () => patchResult,
    submit: (patch) => submitMock(patch),
    onSaved: () => onSavedMock(),
  });
  return (
    <form class="test-form" onSubmit={(e) => void form.save(e)}>
      <p class="test-note">{form.note() ?? ''}</p>
      <p class="test-error">{form.error() ?? ''}</p>
      <button type="button" class="test-nav" onClick={() => navigate('/away')}>
        Leave
      </button>
      <button type="submit">Save</button>
    </form>
  );
}

let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let history: MemoryHistory;

function mount(): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route path="/" component={Host} />
        <Route path="/away" component={() => <p class="test-away-page">away</p>} />
      </MemoryRouter>
    ),
    container,
  );
}

const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

function submitForm(): void {
  const form = container.querySelector('form.test-form') as HTMLFormElement;
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
}

const noteText = (): string => container.querySelector('.test-note')?.textContent ?? '';
const errorText = (): string => container.querySelector('.test-error')?.textContent ?? '';

beforeEach(() => {
  dirtyFlag = false;
  patchResult = {};
  submitMock = vi.fn(() => Promise.resolve(undefined));
  onSavedMock = vi.fn();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.restoreAllMocks();
});

describe('useSettingsForm', () => {
  it('submits exactly the built patch and notes Saved.', async () => {
    patchResult = { afk_branch_pattern: 'afk/<N>' };
    mount();
    submitForm();
    await settle();

    expect(submitMock).toHaveBeenCalledTimes(1);
    expect(submitMock).toHaveBeenCalledWith({ afk_branch_pattern: 'afk/<N>' });
    expect(noteText()).toBe('Saved.');
    expect(errorText()).toBe('');
    expect(onSavedMock).toHaveBeenCalledTimes(1);
  });

  it('an empty patch notes Nothing to save. and never submits', async () => {
    patchResult = {};
    mount();
    submitForm();
    await settle();

    expect(noteText()).toBe('Nothing to save.');
    expect(submitMock).not.toHaveBeenCalled();
    expect(onSavedMock).not.toHaveBeenCalled();
  });

  it('a string buildPatch result surfaces as the error and blocks the save', async () => {
    patchResult = 'Name must not be empty.';
    mount();
    submitForm();
    await settle();

    expect(errorText()).toBe('Name must not be empty.');
    expect(noteText()).toBe('');
    expect(submitMock).not.toHaveBeenCalled();
  });

  it('a rejected submit surfaces errorMessage(err)', async () => {
    patchResult = { name: 'x' };
    submitMock = vi.fn(() => Promise.reject(new ApiError(400, 'bad afk pattern')));
    mount();
    submitForm();
    await settle();

    expect(errorText()).toBe('bad afk pattern');
    expect(noteText()).toBe('');
    expect(onSavedMock).not.toHaveBeenCalled();
  });

  it('navigation while dirty with confirm=false stays put', async () => {
    dirtyFlag = true;
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    mount();
    (container.querySelector('.test-nav') as HTMLButtonElement).click();
    await settle();

    expect(confirm).toHaveBeenCalledWith('Discard unsaved changes?');
    expect(history.get()).toBe('/');
    expect(container.querySelector('.test-away-page')).toBeNull();
  });

  it('navigation while dirty with confirm=true retries past the guard', async () => {
    dirtyFlag = true;
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    mount();
    (container.querySelector('.test-nav') as HTMLButtonElement).click();
    await settle();

    expect(history.get()).toBe('/away');
    expect(container.querySelector('.test-away-page')).not.toBeNull();
  });

  it('navigation while clean never asks', async () => {
    dirtyFlag = false;
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    mount();
    (container.querySelector('.test-nav') as HTMLButtonElement).click();
    await settle();

    expect(confirm).not.toHaveBeenCalled();
    expect(history.get()).toBe('/away');
  });

  it('beforeunload is prevented while dirty', () => {
    dirtyFlag = true;
    mount();
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });

  it('beforeunload passes while clean', () => {
    dirtyFlag = false;
    mount();
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});
