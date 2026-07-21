// Messages/tool-panel behavioral contract (issue #7), stream-rendering
// slice of the RunChat contract split (issue #194):
// - the stream renders user/assistant text, tool chips, and lifecycle/errors;
//   thinking is permanently dropped at paint — in the stream and in the tool
//   panel's list — with no toggle to reveal it (issue #68);
// - tool chips and group summaries are buttons whose click branches on the
//   1024px breakpoint (issue #154): on desktop they toggle a RICH inline
//   expansion in place (a lone chip its ToolViewBody, a group its member chips,
//   each independently expandable); on mobile they open the tool detail sheet
//   (issue #145) — a lone chip straight at its detail, a group at its list. On
//   desktop a FILE chip (diff/write/read) also carries an "open in sidebar"
//   affordance opening the flush-right, file-detail-only sidebar (§2);
//   command/fallback chips and group summary rows never get one, and mobile
//   gets none anywhere. Both the panel selection (seq-keyed, resolving live
//   across SSE refetches — decision 12; swaps in place on a retarget, closes via
//   ✕/Esc) and the desktop expansions (seq-keyed too) survive refetches and
//   clear on stream resets; the sheet flips to the in-flow sidebar when the
//   viewport crosses to >=1024px, and a non-file (group / command) selection
//   clears on that crossing since the desktop sidebar shows files only;

import { describe, expect, it } from 'vitest';
import {
  DESKTOP_QUERY,
  RUN_ID,
  buttonByLabel,
  buttonByText,
  container,
  emitMessagesChangedSettled,
  h,
  hashed,
  installChatHooks,
  mountChat,
  settle,
  stubClipboard,
  stubMatchMedia,
  withAssistantText,
} from './harness';

installChatHooks();

describe('Messages', () => {
  it('renders text and tool chips; thinking never renders (issue #68)', async () => {
    await mountChat();

    expect(container.textContent).toContain('do the thing');
    expect(container.textContent).toContain('all done');
    expect(container.querySelector('.chat-tool')?.textContent).toContain('Ran ls');
    // Thinking is permanently dropped at paint — there is no toggle to reveal it.
    expect(container.textContent).not.toContain('secret reasoning');
    expect(buttonByText('Show thinking')).toBeNull();
  });

  it('renders assistant markdown: heading, bold, inline code, and a code block', async () => {
    withAssistantText('# Title\n\nsome **bold** and `inline`\n\n```js\nconst x = 1;\n```');
    await mountChat();

    expect(container.querySelector('.md .md-h')?.textContent).toBe('Title');
    expect(container.querySelector('.md strong')?.textContent).toBe('bold');
    expect(container.querySelector('.md-code')?.textContent).toBe('inline');
    expect(container.querySelector('.md-codeblock-lang')?.textContent).toBe('js');
    expect(container.querySelector('.md-pre')?.textContent).toContain('const x = 1;');
    // No literal asterisks leaked through — markdown was parsed, not shown raw.
    expect(container.querySelector('.md strong')?.textContent).not.toContain('*');
  });

  it('renders only allowed-scheme links and never emits a javascript: href', async () => {
    withAssistantText('[ok](https://ex.com) and [bad](javascript:alert(1))');
    await mountChat();

    const links = Array.from(container.querySelectorAll('.md a')) as HTMLAnchorElement[];
    expect(links).toHaveLength(1);
    expect(links[0]?.getAttribute('href')).toBe('https://ex.com');
    expect(links[0]?.getAttribute('rel')).toBe('noopener noreferrer');
    expect(links[0]?.getAttribute('target')).toBe('_blank');
    expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
    // the rejected link degrades to visible text, not a dropped node
    expect(container.textContent).toContain('javascript:alert(1)');
  });

  it('renders markdown for user messages but shows no whole-message copy button', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'user', text: 'do **this**' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.role-user .md strong')?.textContent).toBe('this');
    expect(container.querySelector('.role-user .chat-msg-actions')).toBeNull();
  });

  it('coalesces a run of 2+ tool calls into one disclosure, rolling up errors', async () => {
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'assistant', text: 'on it' },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'ls', status: 'ok', output: 'a' } },
        { seq: 3, kind: 'text', role: 'assistant', thinking: true, text: 'hmm' },
        { seq: 4, kind: 'tool', tool: { name: 'Read', title: 'read', status: 'error' } },
        { seq: 5, kind: 'tool', tool: { name: 'Bash', title: 'grep', status: 'ok' } },
      ],
      state: 'needs_input',
      cursor: 5,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-tool-group')).not.toBeNull();
    // Count is tools only — the folded-in thinking is not counted.
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('3 tool calls');
    expect(container.querySelector('.tool-group-failed')?.textContent).toContain('1 failed');
    expect(container.querySelector('.tool-group-summary')?.classList.contains('has-error')).toBe(
      true,
    );
  });

  it('leaves a lone tool call as a plain chip (threshold is 2+)', async () => {
    // The default fixture has a single tool at seq 3.
    await mountChat();
    expect(container.querySelector('.chat-tool')).not.toBeNull();
    expect(container.querySelector('.chat-tool-group')).toBeNull();
  });

  it('drops folded-in thinking from the group and its panel list, permanently (issue #68)', async () => {
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok' } },
        { seq: 2, kind: 'text', role: 'assistant', thinking: true, text: 'secret group reasoning' },
        { seq: 3, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'ok' } },
      ],
      state: 'needs_input',
      cursor: 3,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // Thinking folds in (keeps the run together) but is never counted.
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('2 tool calls');
    expect(container.textContent).not.toContain('secret group reasoning');

    // Opening the panel list (a user tap) still never reveals it — one row
    // per TOOL, and there is no toggle left to flip.
    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();

    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(2);
    expect(container.textContent).not.toContain('secret group reasoning');
    expect(buttonByText('Show thinking')).toBeNull();
  });

  it('shows a live "running…" summary while a trailing run is still in flight', async () => {
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok' } },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'running' } },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.tool-group-count')?.textContent).toBe('2 tool calls');
    expect(container.querySelector('.tool-group-running')?.textContent).toContain('running');
  });

  it('keeps the open panel live across an SSE refetch (decision 12)', async () => {
    // The group is re-derived on every refetch; the panel selection is keyed
    // by the first tool's immutable seq, so an SSE tick must neither close an
    // open list nor reset a pushed detail — it only grows them.
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok', output: 'one' } },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'running' } },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(2);

    // An SSE tick appends another tool and re-derives the group: the panel
    // stays open and the list shows the new row.
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: [
        ...h.messagesOnServer.messages,
        { seq: 3, kind: 'tool', tool: { name: 'Bash', title: 'c', status: 'running' } },
      ],
      cursor: 3,
    };
    await emitMessagesChangedSettled();
    expect(container.querySelector('.tool-panel')).not.toBeNull();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(3);
    expect(container.querySelector('.tool-group-count')?.textContent).toBe('3 tool calls');

    // Push into a detail, then grow ITS output on the next tick: the shown
    // text grows in place (live resolution, never a captured message) and the
    // page does not reset back to the list.
    panelRow('a').click();
    await settle();
    expect(container.querySelector('.tool-panel .tool-body.tool-output')?.textContent).toBe('one');

    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: h.messagesOnServer.messages.map((m) =>
        m.seq === 1 ? { ...m, tool: { ...m.tool!, output: 'one\ntwo' } } : m,
      ),
    };
    // A back-patch below the cursor rides the event's backpatchSeq (issue
    // #175) so the light refetch reaches back to it.
    await emitMessagesChangedSettled(RUN_ID, { backpatchSeq: 1 });
    expect(container.querySelector('.tool-panel .tool-body.tool-output')?.textContent).toBe(
      'one\ntwo',
    );
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(0); // still on detail
    expect(buttonByLabel('Back to list')).not.toBeNull();
  });

  function panelRow(title: string): HTMLButtonElement {
    const row = Array.from(container.querySelectorAll<HTMLButtonElement>('.tool-panel-row')).find(
      (r) => r.querySelector('.tool-title')?.textContent === title,
    );
    if (!row) throw new Error(`missing panel row "${title}"`);
    return row;
  }

  function withToolRunAndLoneChip(): void {
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'assistant', text: 'on it' },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'ls', status: 'ok', output: 'a' } },
        { seq: 3, kind: 'text', role: 'assistant', thinking: true, text: 'hmm' },
        { seq: 4, kind: 'tool', tool: { name: 'Read', title: 'read', status: 'error' } },
        { seq: 5, kind: 'tool', tool: { name: 'Bash', title: 'grep', status: 'ok' } },
        { seq: 6, kind: 'text', role: 'assistant', text: 'between' },
        {
          seq: 7,
          kind: 'tool',
          tool: { name: 'Bash', title: 'lone', status: 'ok', output: 'solo out' },
        },
      ],
      state: 'needs_input',
      cursor: 7,
      has_more: false,
      transcript: 'available',
    };
  }

  it('opens the panel straight at detail for a lone chip tap (mobile: modal sheet)', async () => {
    await mountChat(); // default fixture: one lone tool, 'Ran ls', output 'a\nb'
    expect(container.querySelector('.tool-panel')).toBeNull();

    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    chip.click();
    await settle();

    // Mobile: a modal bottom sheet.
    const panel = container.querySelector('aside.tool-panel');
    expect(panel).not.toBeNull();
    expect(panel!.getAttribute('role')).toBe('dialog');
    expect(panel!.classList.contains('tool-panel-sheet')).toBe(true);
    // Straight at detail — the tool's output, and NO back affordance (a lone
    // chip has no list to return to).
    expect(panel!.querySelector('.tool-body.tool-output')?.textContent).toBe('a\nb');
    expect(buttonByLabel('Back to list')).toBeNull();
    // The chip wears the selected state…
    expect(chip.classList.contains('selected')).toBe(true);
    expect(chip.getAttribute('aria-pressed')).toBe('true');
    // …and nothing expanded inline (issue #154 is desktop-only): the stream
    // carries no I/O pane and no inline body.
    expect(container.querySelector('.chat-stream .tool-body')).toBeNull();
    expect(container.querySelector('.tool-inline-body')).toBeNull();
  });

  it('opens the panel at the list page for a group tap, one row per tool', async () => {
    withToolRunAndLoneChip();
    await mountChat();

    const group = container.querySelector('button.tool-group-summary') as HTMLButtonElement;
    group.click();
    await settle();

    // The list page: the count heading and one row per TOOL (folded-in
    // thinking is excluded).
    expect(container.querySelector('.tool-panel-title')?.textContent).toBe('3 tool calls');
    const rows = Array.from(container.querySelectorAll('.tool-panel-row .tool-title')).map(
      (el) => el.textContent,
    );
    expect(rows).toEqual(['ls', 'read', 'grep']);
    // The group line is marked as the panel's source.
    expect(group.classList.contains('selected')).toBe(true);
    expect(group.getAttribute('aria-pressed')).toBe('true');
    // Nothing expanded inline on mobile (issue #154 is desktop-only).
    expect(container.querySelector('.tool-group-body')).toBeNull();
  });

  it('pushes a list row to its detail and returns via back', async () => {
    withToolRunAndLoneChip();
    await mountChat();
    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();

    panelRow('read').click();
    await settle();
    // Detail: the tool's title in the head, plus the back affordance (a group
    // target entered at its list).
    expect(container.querySelector('.tool-panel-title .tool-title')?.textContent).toBe('read');
    const back = buttonByLabel('Back to list');
    expect(back).not.toBeNull();

    back!.click();
    await settle();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(3);
    expect(buttonByLabel('Back to list')).toBeNull();
  });

  it('swaps the panel in place when a different chip is tapped while open', async () => {
    withToolRunAndLoneChip();
    await mountChat();

    const group = container.querySelector('button.tool-group-summary') as HTMLButtonElement;
    group.click();
    await settle();
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(3);

    // Tap the separate lone chip: ONE panel swaps to its detail (no
    // close/reopen churn) and the selected highlight moves with it.
    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    chip.click();
    await settle();
    expect(container.querySelectorAll('.tool-panel')).toHaveLength(1);
    expect(container.querySelector('.tool-panel .tool-body.tool-output')?.textContent).toBe(
      'solo out',
    );
    expect(container.querySelectorAll('.tool-panel-row')).toHaveLength(0);
    expect(chip.classList.contains('selected')).toBe(true);
    expect(group.classList.contains('selected')).toBe(false);
  });

  it('closes the panel and clears the selected highlight via ✕', async () => {
    await mountChat();
    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    chip.click();
    await settle();
    expect(container.querySelector('.tool-panel')).not.toBeNull();

    // The panel's own ✕ (the error banner's dismiss is labeled "Dismiss").
    container.querySelector<HTMLButtonElement>('.tool-panel button[aria-label="Close"]')!.click();
    await settle();
    expect(container.querySelector('.tool-panel')).toBeNull();
    expect(container.querySelector('.chat-tool.selected')).toBeNull();
    expect(chip.getAttribute('aria-pressed')).toBe('false');
  });

  it('resets the panel when the stream resets (transcript rotation)', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, transcript_id: 'sess-A' };
    await mountChat();
    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('.tool-panel')).not.toBeNull();

    // /clear rotates: the accumulated stream — and the seq-keyed panel
    // selection with it — drops before the fresh transcript merges in (the
    // same resetStream that clears on a route-param change).
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'user', text: 'fresh start' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      transcript_id: 'sess-B',
      pending_dialog: null,
    };
    await emitMessagesChangedSettled();
    await settle();

    expect(container.textContent).toContain('fresh start');
    expect(container.querySelector('.tool-panel')).toBeNull();
    expect(container.querySelector('.selected')).toBeNull();
  });

  it('keeps a FILE panel open across the 1024px breakpoint, switching containers', async () => {
    const media = stubMatchMedia(); // DESKTOP_QUERY reads false → mobile
    // A file tool: only a FILE selection survives the crossing to desktop (the
    // sidebar is file-only — a command/group selection clears, tested below).
    h.messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit foo.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'foo.ts', text: '@@ -1 +1 @@\n+x' },
          },
        },
      ],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();
    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')?.getAttribute('role')).toBe('dialog');
    expect(container.querySelector('.tool-scrim')).not.toBeNull();

    // Cross to desktop: shared state, pure render switch — the panel stays
    // open with the same file, now the in-flow complementary sidebar (no
    // dialog role, no scrim).
    media.set(DESKTOP_QUERY, true);
    await settle();
    const side = container.querySelector('aside.tool-panel');
    expect(side).not.toBeNull();
    expect(side!.getAttribute('role')).toBeNull();
    expect(side!.classList.contains('tool-panel-side')).toBe(true);
    expect(container.querySelector('.tool-scrim')).toBeNull();
    expect(side!.querySelector('.tool-view-path-text')?.textContent).toBe('foo.ts');
  });

  it('expands a lone chip inline on desktop; a second click collapses it (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    await mountChat(); // default fixture: one lone tool, 'Ran ls', output 'a\nb'
    const chip = container.querySelector('button.tool-summary') as HTMLButtonElement;
    expect(chip.getAttribute('aria-expanded')).toBe('false');
    expect(container.querySelector('.tool-inline-body')).toBeNull();

    chip.click();
    await settle();
    // The rich body renders inline under the stream — no sidebar / sheet panel.
    const body = container.querySelector('.chat-stream .tool-inline-body');
    expect(body).not.toBeNull();
    expect(body!.querySelector('.tool-body.tool-output')?.textContent).toBe('a\nb');
    expect(container.querySelector('aside.tool-panel')).toBeNull();
    expect(container.querySelector('.tool-panel-side')).toBeNull();
    expect(chip.getAttribute('aria-expanded')).toBe('true');
    // Desktop expansion never touches the sheet-selection state.
    expect(chip.getAttribute('aria-pressed')).toBe('false');
    expect(container.querySelector('.chat-tool.selected')).toBeNull();

    chip.click();
    await settle();
    expect(container.querySelector('.tool-inline-body')).toBeNull();
    expect(chip.getAttribute('aria-expanded')).toBe('false');
  });

  it('expands a group inline on desktop, each member chip independently (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withToolRunAndLoneChip();
    await mountChat();
    const group = container.querySelector('button.tool-group-summary') as HTMLButtonElement;
    expect(group.getAttribute('aria-expanded')).toBe('false');
    expect(container.querySelector('.tool-group-body')).toBeNull();

    group.click();
    await settle();
    const body = container.querySelector('.tool-group-body');
    expect(body).not.toBeNull();
    // One chip per TOOL, folded-in thinking excluded: ls, read, grep.
    const titles = Array.from(body!.querySelectorAll('.chat-tool .tool-title')).map(
      (el) => el.textContent,
    );
    expect(titles).toEqual(['ls', 'read', 'grep']);
    expect(group.getAttribute('aria-expanded')).toBe('true');
    expect(container.querySelector('aside.tool-panel')).toBeNull();

    // A member chip expands its OWN inline body, independent of the group.
    const lsChip = Array.from(
      body!.querySelectorAll<HTMLButtonElement>('button.tool-summary'),
    ).find((b) => b.querySelector('.tool-title')?.textContent === 'ls')!;
    lsChip.click();
    await settle();
    expect(lsChip.getAttribute('aria-expanded')).toBe('true');
    // The summary sits inside a .tool-summary-row now (issue #154 split it so a
    // sidebar affordance can be its sibling), so reach the body via the frame.
    const inline = lsChip.closest('.chat-tool')!.querySelector('.tool-inline-body');
    expect(inline).not.toBeNull();
    expect(inline!.querySelector('.tool-body.tool-output')?.textContent).toBe('a');
  });

  it('keeps an inline expansion open across an SSE refetch, keyed by seq (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    await mountChat(); // lone tool at seq 3, output 'a\nb'
    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('.tool-inline-body .tool-body.tool-output')?.textContent).toBe(
      'a\nb',
    );

    // An SSE tick grows the output at seq 3 — a back-patch below the cursor,
    // so its content_hash flips (re-derived) and the event names backpatchSeq
    // (issue #175). The seq-keyed expansion neither closes (native <details>
    // would) nor freezes (a captured message would) — it grows in place.
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: h.messagesOnServer.messages.map((m) =>
        m.seq === 3 ? hashed({ ...m, tool: { ...m.tool!, output: 'a\nb\nc' } }) : m,
      ),
    };
    await emitMessagesChangedSettled(RUN_ID, { backpatchSeq: 3 });
    const body = container.querySelector('.tool-inline-body');
    expect(body).not.toBeNull();
    expect(body!.querySelector('.tool-body.tool-output')?.textContent).toBe('a\nb\nc');
    // The freshly re-rendered summary button still reads expanded.
    expect(
      (container.querySelector('button.tool-summary') as HTMLButtonElement).getAttribute(
        'aria-expanded',
      ),
    ).toBe('true');
  });

  function withFileChips(): void {
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'text', role: 'assistant', text: 'editing' },
        {
          seq: 2,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit foo.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'foo.ts', text: '@@ -1 +1 @@\n-old\n+new-foo' },
          },
        },
        { seq: 3, kind: 'text', role: 'assistant', text: 'and' },
        {
          seq: 4,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit bar.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'bar.ts', text: '@@ -1 +1 @@\n+new-bar' },
          },
        },
        { seq: 5, kind: 'text', role: 'assistant', text: 'then' },
        {
          seq: 6,
          kind: 'tool',
          tool: {
            name: 'Bash',
            title: 'run tests',
            status: 'ok',
            view: { kind: 'command', command: 'npm test' },
            output: 'passed',
          },
        },
      ],
      state: 'needs_input',
      cursor: 6,
      has_more: false,
      transcript: 'available',
    };
  }

  function chipFrame(title: string): HTMLElement {
    const summary = Array.from(
      container.querySelectorAll<HTMLButtonElement>('button.tool-summary'),
    ).find((b) => b.querySelector('.tool-title')?.textContent === title);
    if (!summary) throw new Error(`missing chip "${title}"`);
    return summary.closest('.chat-tool') as HTMLElement;
  }

  function rowAffordance(frame: HTMLElement): HTMLButtonElement | null {
    return frame.querySelector<HTMLButtonElement>(
      ':scope > .tool-summary-row button[aria-label="Open in sidebar"]',
    );
  }

  it('shows the open-in-sidebar affordance on desktop file chips only (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withFileChips();
    await mountChat();

    // The two file chips carry the affordance in their collapsed row…
    expect(rowAffordance(chipFrame('edit foo.ts'))).not.toBeNull();
    expect(rowAffordance(chipFrame('edit bar.ts'))).not.toBeNull();
    // …the command chip never does.
    expect(rowAffordance(chipFrame('run tests'))).toBeNull();
  });

  it('never shows the affordance on a group summary row, only its member chips (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    // A run of file edits coalesces into a group.
    h.messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit a.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'a.ts', text: '@@ -1 +1 @@\n+a' },
          },
        },
        {
          seq: 2,
          kind: 'tool',
          tool: {
            name: 'Edit',
            title: 'edit b.ts',
            status: 'ok',
            view: { kind: 'diff', path: 'b.ts', text: '@@ -1 +1 @@\n+b' },
          },
        },
      ],
      state: 'needs_input',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    const groupFrame = container.querySelector('.chat-tool-group') as HTMLElement;
    const groupSummary = groupFrame.querySelector('button.tool-group-summary') as HTMLButtonElement;
    // Collapsed group: no affordance anywhere in the frame (the summary row is
    // not a file chip).
    expect(groupFrame.querySelector('button[aria-label="Open in sidebar"]')).toBeNull();

    // Expanded: each member chip carries its own affordance (via the recursion).
    groupSummary.click();
    await settle();
    expect(
      groupFrame.querySelectorAll('.tool-group-body button[aria-label="Open in sidebar"]'),
    ).toHaveLength(2);
  });

  it('opens the file sidebar from a chip affordance, replaces on a second file, Escape closes (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withFileChips();
    await mountChat();

    const fooFrame = chipFrame('edit foo.ts');
    rowAffordance(fooFrame)!.click();
    await settle();

    // The in-flow desktop sidebar shows foo.ts, and the chip is selected/pressed.
    const side = container.querySelector('aside.tool-panel.tool-panel-side');
    expect(side).not.toBeNull();
    expect(side!.querySelector('.tool-view-path-text')?.textContent).toBe('foo.ts');
    expect(fooFrame.classList.contains('selected')).toBe(true);
    expect(
      (fooFrame.querySelector('button.tool-summary') as HTMLButtonElement).getAttribute(
        'aria-pressed',
      ),
    ).toBe('true');

    // Opening a second file replaces the content in place — one panel, new path.
    rowAffordance(chipFrame('edit bar.ts'))!.click();
    await settle();
    expect(container.querySelectorAll('aside.tool-panel')).toHaveLength(1);
    const side2 = container.querySelector('aside.tool-panel-side')!;
    expect(side2.querySelector('.tool-view-path-text')?.textContent).toBe('bar.ts');
    expect(side2.textContent).not.toContain('foo.ts');
    expect(fooFrame.classList.contains('selected')).toBe(false);
    expect(chipFrame('edit bar.ts').classList.contains('selected')).toBe(true);

    // Escape closes the sidebar and clears the highlight.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(container.querySelector('aside.tool-panel')).toBeNull();
    expect(container.querySelector('.chat-tool.selected')).toBeNull();
  });

  it('carries the affordance in the expanded inline path header too (issue #154)', async () => {
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withFileChips();
    await mountChat();

    const fooFrame = chipFrame('edit foo.ts');
    // A body (summary) click toggles inline expansion — not the sidebar.
    (fooFrame.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();

    const inline = fooFrame.querySelector('.tool-inline-body');
    expect(inline).not.toBeNull();
    expect(
      inline!.querySelector('.tool-view-path button[aria-label="Open in sidebar"]'),
    ).not.toBeNull();
    // The body click opened no sidebar/sheet.
    expect(container.querySelector('aside.tool-panel')).toBeNull();
  });

  it('shows no open-in-sidebar affordance anywhere on mobile (issue #154)', async () => {
    withFileChips();
    await mountChat(); // no matchMedia stub → mobile

    expect(container.querySelector('button[aria-label="Open in sidebar"]')).toBeNull();

    // Opening a file chip's sheet adds none either (the sheet has no affordance).
    (chipFrame('edit foo.ts').querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')).not.toBeNull();
    expect(container.querySelector('button[aria-label="Open in sidebar"]')).toBeNull();
  });

  it('closes an open group sheet when the viewport crosses to desktop (file-only sidebar) (issue #154)', async () => {
    const media = stubMatchMedia(); // mobile
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'tool', tool: { name: 'Bash', title: 'a', status: 'ok' } },
        { seq: 2, kind: 'tool', tool: { name: 'Bash', title: 'b', status: 'ok' } },
      ],
      state: 'needs_input',
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    (container.querySelector('button.tool-group-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')).not.toBeNull();

    // Cross to desktop: the sidebar shows file details only, so a group
    // selection clears — the panel closes rather than showing a list.
    media.set(DESKTOP_QUERY, true);
    await settle();
    expect(container.querySelector('aside.tool-panel')).toBeNull();
  });

  it('closes an open command-tool sheet when crossing to desktop (issue #154)', async () => {
    const media = stubMatchMedia(); // mobile
    await mountChat(); // default fixture: lone Bash 'Ran ls', no file view

    (container.querySelector('button.tool-summary') as HTMLButtonElement).click();
    await settle();
    expect(container.querySelector('aside.tool-panel')).not.toBeNull();

    // A non-file tool selection clears when the sidebar (file-only) takes over.
    media.set(DESKTOP_QUERY, true);
    await settle();
    expect(container.querySelector('aside.tool-panel')).toBeNull();
  });

  it('copies the raw markdown of an assistant message to the clipboard', async () => {
    const writeText = stubClipboard();
    withAssistantText('# Title\n\n**raw** markdown');
    await mountChat();

    const btn = container.querySelector('.chat-msg-actions .copy-btn') as HTMLButtonElement;
    expect(btn).not.toBeNull();
    btn.click();
    await settle();
    expect(writeText).toHaveBeenCalledWith('# Title\n\n**raw** markdown');
    // Feedback swaps to the copied state.
    expect(container.querySelector('.copy-btn.copied')).not.toBeNull();
  });

  it('copies the raw source of a fenced code block from its header bar', async () => {
    const writeText = stubClipboard();
    withAssistantText('```py\nprint(1)\nprint(2)\n```');
    await mountChat();

    const btn = container.querySelector('.md-codeblock-bar .copy-btn') as HTMLButtonElement;
    expect(btn).not.toBeNull();
    btn.click();
    await settle();
    expect(writeText).toHaveBeenCalledWith('print(1)\nprint(2)');
  });

  it('renders the needs-input status line as the last stream item, not a composer note', async () => {
    withAssistantText('done'); // fixture state is needs_input
    await mountChat();

    const stream = container.querySelector('.chat-stream')!;
    const line = stream.querySelector('.chat-needs-input');
    expect(line?.textContent).toBe('Claude Code is waiting for your reply.');
    // §3: it is the LAST stream child (so it only shows at the bottom).
    expect(stream.lastElementChild).toBe(line);
    // A status line, not a chat bubble, and not the old composer note.
    expect(line?.closest('.chat-msg')).toBeNull();
    expect(container.querySelector('.chat-composer-note')).toBeNull();
    expect(container.querySelector('.chat-input')).not.toBeNull();
  });

  it('hides the needs-input line when the run resumes working', async () => {
    withAssistantText('done');
    await mountChat();
    expect(container.querySelector('.chat-needs-input')).not.toBeNull();

    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' };
    await emitMessagesChangedSettled();

    // The needs-input line clears. The composer no longer reacts to `working`
    // (ADR-0029): no working hint element exists, and Send stays present.
    expect(container.querySelector('.chat-needs-input')).toBeNull();
    expect(container.textContent).not.toContain('is waiting for your reply.');
    expect(container.querySelector('.chat-composer-hint')).toBeNull();
    expect(buttonByLabel('Send')).not.toBeNull();
  });

  it('drops the old claude.ai wording from the needs-input surface', async () => {
    withAssistantText('done');
    await mountChat();

    expect(container.textContent).not.toContain('reply below');
    expect(container.textContent).not.toContain('open it in claude.ai');
  });
});
