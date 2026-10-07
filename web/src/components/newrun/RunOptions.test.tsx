// Run options contract (issue #66):
// - ChoiceChip: the chip shows the resolved option's label with a caret and
//   is a dialog opener (aria-haspopup/aria-expanded); one click opens its
//   picker titled by `name`, one row per option with "default" on the
//   inherited default and a check (aria-selected) on the current value, then
//   the hint naming where the default comes from; picking an option reports
//   it and closes — two clicks in all; `changed` adds the accent outline
//   class; a second chip click closes; a disabled chip never opens.
// - MoreOptions: the ⋯ chip (aria-label "More options", accent when
//   `changed`) opens "More options": the Agent segmented control only with
//   two or more providers; the Remote control switch reads "inherited · on",
//   "inherited · off" or "set here", and is disabled with "<Agent> ignores
//   this" for a provider without a remote knob; the Label input is optional,
//   maxlength 32 and never reports more than 32 characters; one sentence
//   names the resolved Runner, "(inherited)" when inherited, and links to the
//   repo's Runner settings.
// - AttachmentChip: shows its text and its remove button reports the removal.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { createSignal, type JSX } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AttachmentChip, ChoiceChip, MoreOptions, type ChoiceOption } from './RunOptions';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  document.body.style.overflow = '';
});

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await flush();
}

/** Renders `view` at "/" inside a MemoryRouter (MoreOptions renders an <A>). */
async function mount(view: () => JSX.Element): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/' });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route path="/" component={view} />
        <Route path="*" component={() => <p class="elsewhere">navigated</p>} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

const panel = () => document.querySelector<HTMLElement>('.picker');
const panelTitle = () =>
  document.getElementById(panel()!.getAttribute('aria-labelledby')!)?.textContent;
const options = () => Array.from(document.querySelectorAll<HTMLButtonElement>('[role="option"]'));
const optionNamed = (title: string) =>
  options().find((o) => o.querySelector('.picker-option-title')?.textContent === title);

const MODELS: ChoiceOption[] = [
  { value: 'opus', label: 'Opus 4.1', description: 'most capable' },
  { value: 'sonnet', label: 'Sonnet 4.5' },
  { value: 'haiku', label: 'Haiku 4.5' },
];

describe('ChoiceChip', () => {
  async function mountChip(over: { changed?: boolean; disabled?: boolean } = {}) {
    const onPick = vi.fn();
    await mount(() => {
      const [value, setValue] = createSignal('sonnet');
      return (
        <ChoiceChip
          name="Model"
          value={value()}
          defaultValue="sonnet"
          options={MODELS}
          changed={over.changed ?? false}
          defaultSource="coding-lab's settings"
          disabled={over.disabled}
          onPick={(v) => {
            onPick(v);
            setValue(v);
          }}
        />
      );
    });
    const chip = () => container.querySelector<HTMLButtonElement>('.run-chip')!;
    return { onPick, chip };
  }

  it('shows the resolved label with a caret and is a dialog opener', async () => {
    const { chip } = await mountChip();
    expect(chip().querySelector('.composer-chip-label')?.textContent).toBe('Sonnet 4.5');
    expect(chip().querySelector('.composer-chip-caret')).not.toBeNull();
    expect(chip().getAttribute('aria-label')).toBe('Model: Sonnet 4.5');
    expect(chip().getAttribute('aria-haspopup')).toBe('dialog');
    expect(chip().getAttribute('aria-expanded')).toBe('false');
    expect(chip().classList.contains('changed')).toBe(false);
  });

  it('opens on one click: options, default tag, check, and the hint naming the source', async () => {
    const { chip } = await mountChip();
    chip().click();
    expect(panel()).not.toBeNull();
    expect(panelTitle()).toBe('Model');
    expect(chip().getAttribute('aria-expanded')).toBe('true');
    expect(options().map((o) => o.querySelector('.picker-option-title')?.textContent)).toEqual([
      'Opus 4.1',
      'Sonnet 4.5',
      'Haiku 4.5',
    ]);
    expect(optionNamed('Opus 4.1')?.querySelector('.picker-option-desc')?.textContent).toBe(
      '· most capable',
    );
    const sonnet = optionNamed('Sonnet 4.5')!;
    expect(sonnet.getAttribute('aria-selected')).toBe('true');
    expect(sonnet.querySelector('.picker-option-default')?.textContent).toBe('default');
    expect(sonnet.querySelector('.picker-option-check')).not.toBeNull();
    const opus = optionNamed('Opus 4.1')!;
    expect(opus.getAttribute('aria-selected')).toBe('false');
    expect(opus.querySelector('.picker-option-default')).toBeNull();
    expect(panel()!.querySelector('.picker-hint')?.textContent).toBe(
      "The default comes from coding-lab's settings. A pick here applies to this run only.",
    );
  });

  it('a pick reports the value and closes: two clicks in all', async () => {
    const { chip, onPick } = await mountChip();
    chip().click();
    optionNamed('Opus 4.1')!.click();
    expect(onPick).toHaveBeenCalledExactlyOnceWith('opus');
    expect(panel()).toBeNull();
    expect(chip().getAttribute('aria-expanded')).toBe('false');
    expect(chip().querySelector('.composer-chip-label')?.textContent).toBe('Opus 4.1');
  });

  it('toggles closed on a second chip click', async () => {
    const { chip } = await mountChip();
    chip().click();
    expect(panel()).not.toBeNull();
    chip().click();
    expect(panel()).toBeNull();
  });

  it('changed adds the accent outline class', async () => {
    const { chip } = await mountChip({ changed: true });
    expect(chip().classList.contains('changed')).toBe(true);
  });

  it('a disabled chip never opens', async () => {
    const { chip } = await mountChip({ disabled: true });
    expect(chip().disabled).toBe(true);
    chip().click();
    expect(panel()).toBeNull();
  });
});

describe('MoreOptions', () => {
  type MoreProps = Parameters<typeof MoreOptions>[0];
  async function mountMore(over: Partial<MoreProps> = {}) {
    const calls = { provider: vi.fn(), remote: vi.fn(), label: vi.fn() };
    const props: MoreProps = {
      providers: [{ id: 'claude-code', label: 'Claude Code' }],
      providerId: 'claude-code',
      onProvider: calls.provider,
      remote: true,
      remoteSetHere: false,
      remoteBlocker: null,
      onRemote: calls.remote,
      label: '',
      onLabel: calls.label,
      runner: 'container',
      runnerInherited: true,
      runnerHref: '/repos/r_lab/settings#runner',
      changed: false,
      ...over,
    };
    await mount(() => <MoreOptions {...props} />);
    const chip = container.querySelector<HTMLButtonElement>('button[aria-label="More options"]')!;
    chip.click();
    return { calls, chip };
  }
  const remoteSwitch = () => panel()!.querySelector<HTMLButtonElement>('[role="switch"]')!;
  const remoteDesc = () =>
    document.getElementById(remoteSwitch().getAttribute('aria-describedby')!)?.textContent;
  const runner = () => panel()!.querySelector('.run-more-runner');

  it('the ⋯ chip opens More options; changed adds the accent outline', async () => {
    const { chip } = await mountMore({ changed: true });
    expect(chip.getAttribute('aria-haspopup')).toBe('dialog');
    expect(chip.getAttribute('aria-expanded')).toBe('true');
    expect(chip.classList.contains('changed')).toBe(true);
    expect(chip.querySelector('svg')).not.toBeNull();
    expect(panelTitle()).toBe('More options');
    chip.click();
    expect(panel()).toBeNull();
  });

  it('hides the agent control with a single provider', async () => {
    const { chip } = await mountMore();
    expect(chip.classList.contains('changed')).toBe(false);
    expect(panel()!.querySelector('[role="radiogroup"]')).toBeNull();
  });

  it('shows the agent segmented control with two providers and reports a pick', async () => {
    const { calls } = await mountMore({
      providers: [
        { id: 'claude-code', label: 'Claude Code' },
        { id: 'codex', label: 'Codex' },
      ],
    });
    const group = panel()!.querySelector('[role="radiogroup"]')!;
    const segments = Array.from(group.querySelectorAll<HTMLButtonElement>('[role="radio"]'));
    expect(segments.map((s) => [s.textContent, s.getAttribute('aria-checked')])).toEqual([
      ['Claude Code', 'true'],
      ['Codex', 'false'],
    ]);
    expect(document.getElementById(group.getAttribute('aria-labelledby')!)?.textContent).toBe(
      'Agent',
    );
    segments[1]!.click();
    expect(calls.provider).toHaveBeenCalledExactlyOnceWith('codex');
  });

  it('remote control reads "inherited · on" and reports a toggle', async () => {
    const { calls } = await mountMore({ remote: true });
    expect(remoteSwitch().getAttribute('aria-checked')).toBe('true');
    expect(remoteSwitch().disabled).toBe(false);
    expect(remoteDesc()).toBe('inherited · on');
    remoteSwitch().click();
    expect(calls.remote).toHaveBeenCalledExactlyOnceWith(false);
  });

  it('remote control reads "inherited · off"', async () => {
    await mountMore({ remote: false });
    expect(remoteSwitch().getAttribute('aria-checked')).toBe('false');
    expect(remoteDesc()).toBe('inherited · off');
  });

  it('remote control reads "set here" for a per-spawn pick', async () => {
    await mountMore({ remote: false, remoteSetHere: true });
    expect(remoteDesc()).toBe('set here');
  });

  it('remote control is disabled with "<Agent> ignores this" without a remote knob', async () => {
    const { calls } = await mountMore({ remoteBlocker: 'Codex' });
    expect(remoteSwitch().disabled).toBe(true);
    expect(remoteDesc()).toBe('Codex ignores this');
    remoteSwitch().click();
    expect(calls.remote).not.toHaveBeenCalled();
  });

  it('the label input is optional, ≤32 characters, placeholder "debug"', async () => {
    const { calls } = await mountMore({ label: 'abc' });
    const input = panel()!.querySelector<HTMLInputElement>('input[name="label"]')!;
    expect(input.value).toBe('abc');
    expect(input.maxLength).toBe(32);
    expect(input.placeholder).toBe('debug');
    expect(input.required).toBe(false);
    input.value = 'x'.repeat(40);
    input.dispatchEvent(new Event('input', { bubbles: true }));
    expect(calls.label).toHaveBeenCalledExactlyOnceWith('x'.repeat(32));
  });

  it('names an inherited container Runner and links to the Runner settings', async () => {
    await mountMore();
    expect(runner()?.textContent).toBe('Runs in a container (inherited) · Runner settings');
    const link = runner()!.querySelector('a')!;
    expect(link.textContent).toBe('Runner settings');
    expect(link.getAttribute('href')).toBe('/repos/r_lab/settings#runner');
  });

  it('names a host Runner set on the repo', async () => {
    await mountMore({ runner: 'host', runnerInherited: false });
    expect(runner()?.textContent).toBe('Runs directly on the host, unsandboxed · Runner settings');
  });

  it('leaves the Runner sentence out while the Runner is unknown', async () => {
    await mountMore({ runner: null });
    expect(runner()).toBeNull();
  });
});

describe('AttachmentChip', () => {
  it('shows the text and reports the removal', async () => {
    const onRemove = vi.fn();
    await mount(() => <AttachmentChip text="Triage #47 · Flaky login test" onRemove={onRemove} />);
    expect(container.querySelector('.composer-attach-text')?.textContent).toBe(
      'Triage #47 · Flaky login test',
    );
    const remove = container.querySelector<HTMLButtonElement>(
      'button[aria-label="Remove Triage #47 · Flaky login test"]',
    )!;
    expect(remove).not.toBeNull();
    remove.click();
    expect(onRemove).toHaveBeenCalledOnce();
  });
});
