// groupMessages contract (issue #13, decisions 7–12): consecutive tool activity
// coalesces into one disclosure; thinking folds in but is not counted; a lone
// tool stays a plain chip; the summary rolls up errors and liveness; the key is
// the first tool's seq so the view's open state survives refetches. The
// collapsed summary line describes the run by kind (issue #58), classifying by
// view kind only (provider-blind) and falling back to "N tool calls".

import { beforeEach, describe, expect, it } from 'vitest';
import type { ChatMessage, ToolInfo, ToolView } from '../api';
import {
  groupMessages,
  reconcileRenderItems,
  toolGroupSummary,
  type ToolGroup,
} from './toolGroups';

let seq = 0;
const tool = (status: ToolInfo['status'], view?: ToolView): ChatMessage => ({
  seq: (seq += 1),
  kind: 'tool',
  tool: { name: 'Bash', title: 't', status, ...(view ? { view } : {}) },
});
const edit = (path: string): ChatMessage => tool('ok', { kind: 'diff', path, text: '@@' });
const write = (path: string): ChatMessage => tool('ok', { kind: 'write', path, text: 'x' });
const read = (path: string): ChatMessage => tool('ok', { kind: 'read', path, text: 'x' });
const search = (path?: string): ChatMessage =>
  tool('ok', path === undefined ? { kind: 'search' } : { kind: 'search', path });
const run = (command = 'ls'): ChatMessage => tool('ok', { kind: 'command', command });
/** The summary label for a run of the given tools (they must form a group). */
const labelOf = (...tools: ChatMessage[]): string =>
  toolGroupSummary(groupMessages(tools)[0] as ToolGroup).label;
const think = (): ChatMessage => ({
  seq: (seq += 1),
  kind: 'text',
  role: 'assistant',
  thinking: true,
  text: '…',
});
const text = (t: string): ChatMessage => ({
  seq: (seq += 1),
  kind: 'text',
  role: 'assistant',
  text: t,
});
const lifecycle = (): ChatMessage => ({ seq: (seq += 1), kind: 'lifecycle', text: 'started' });

beforeEach(() => {
  seq = 0;
});

describe('groupMessages', () => {
  it('leaves a lone tool call as a plain message (threshold is 2+)', () => {
    const items = groupMessages([text('hi'), tool('ok'), text('bye')]);
    expect(items.map((i) => i.kind)).toEqual(['message', 'message', 'message']);
  });

  it('coalesces 2+ consecutive tools into one group keyed by the first tool seq', () => {
    const t1 = tool('ok');
    const items = groupMessages([text('a'), t1, tool('ok'), tool('ok'), text('b')]);
    expect(items.map((i) => i.kind)).toEqual(['message', 'toolGroup', 'message']);
    const group = items[1] as ToolGroup;
    expect(group.key).toBe(t1.seq);
    expect(group.toolCount).toBe(3);
  });

  it('folds interleaved thinking into the run without counting it', () => {
    const items = groupMessages([tool('ok'), think(), tool('ok'), think()]);
    expect(items).toHaveLength(1);
    const group = items[0] as ToolGroup;
    expect(group.toolCount).toBe(2); // thinking not counted
    expect(group.items).toHaveLength(4); // but present, in order
    expect(group.items[1]!.thinking).toBe(true);
  });

  it('breaks a run on a (non-thinking) text message', () => {
    const items = groupMessages([tool('ok'), tool('ok'), text('prose'), tool('ok'), tool('ok')]);
    expect(items.map((i) => i.kind)).toEqual(['toolGroup', 'message', 'toolGroup']);
  });

  it('breaks a run on a lifecycle message', () => {
    const items = groupMessages([tool('ok'), tool('ok'), lifecycle(), tool('ok'), tool('ok')]);
    expect(items.map((i) => i.kind)).toEqual(['toolGroup', 'message', 'toolGroup']);
  });

  it('does not group a thinking-only run', () => {
    const items = groupMessages([think(), think()]);
    expect(items.map((i) => i.kind)).toEqual(['message', 'message']);
  });

  it('keeps a lone tool surrounded by thinking as individual messages', () => {
    const items = groupMessages([think(), tool('ok'), think()]);
    expect(items.map((i) => i.kind)).toEqual(['message', 'message', 'message']);
  });

  it('rolls up an error count onto the group', () => {
    const items = groupMessages([tool('ok'), tool('error'), tool('ok'), tool('error'), tool('ok')]);
    const group = items[0] as ToolGroup;
    expect(group.toolCount).toBe(5);
    expect(group.errorCount).toBe(2);
    expect(toolGroupSummary(group)).toEqual({
      label: '5 tool calls',
      failed: '2 failed',
      running: false,
    });
  });

  it('flags a still-running trailing group as live', () => {
    const items = groupMessages([tool('ok'), tool('running')]);
    const group = items[0] as ToolGroup;
    expect(group.running).toBe(true);
    expect(toolGroupSummary(group)).toEqual({ label: '2 tool calls', failed: null, running: true });
  });

  it('is a no-op on an empty list', () => {
    expect(groupMessages([])).toEqual([]);
  });
});

// The by-kind summary label (issue #58): "Edited N files, ran N commands, read N
// files" in that fixed order, zero counts omitted, the first segment capitalized;
// provider-blind (view kind only); a run with nothing recognized keeps
// "N tool calls".
describe('toolGroupSummary label (issue #58)', () => {
  it('describes a mixed run by kind, in the fixed order', () => {
    const label = labelOf(
      read('r1.ts'),
      run('npm test'),
      edit('a.ts'),
      read('r2.ts'),
      write('b.ts'),
      run('npm run lint'),
      edit('c.ts'),
      read('r3.ts'),
      run('git status'),
      read('r4.ts'),
      edit('d.ts'),
      read('r5.ts'),
    );
    expect(label).toBe('Edited 4 files, ran 3 commands, read 5 files');
  });

  it('counts distinct edit paths: two edits of one file are one file', () => {
    expect(labelOf(edit('a.ts'), edit('a.ts'))).toBe('Edited 1 file');
    expect(labelOf(edit('a.ts'), edit('b.ts'), edit('a.ts'))).toBe('Edited 2 files');
  });

  it('counts a write and a diff of the same path once', () => {
    expect(labelOf(write('a.ts'), edit('a.ts'))).toBe('Edited 1 file');
    expect(labelOf(write('a.ts'), edit('b.ts'), edit('a.ts'))).toBe('Edited 2 files');
  });

  it('counts distinct read paths, and every command call', () => {
    expect(labelOf(read('a.ts'), read('a.ts'), read('b.ts'))).toBe('Read 2 files');
    // Commands are never deduped, even when the text repeats.
    expect(labelOf(run('ls'), run('ls'))).toBe('Ran 2 commands');
  });

  it('counts each search call toward read, on top of distinct read paths', () => {
    expect(labelOf(search(), search())).toBe('Read 2 files');
    // A search root is not a read path: the same root twice is still two searches.
    expect(labelOf(search('src'), search('src'))).toBe('Read 2 files');
    expect(labelOf(read('a.ts'), read('a.ts'), search(), search('src'))).toBe('Read 3 files');
  });

  it('keeps the edited and read file sets independent of one another', () => {
    // The same file edited and read is one edited AND one read.
    expect(labelOf(edit('a.ts'), read('a.ts'))).toBe('Edited 1 file, read 1 file');
  });

  it('uses the singular for one file / one command', () => {
    expect(labelOf(edit('a.ts'), run(), read('a.ts'))).toBe(
      'Edited 1 file, ran 1 command, read 1 file',
    );
  });

  it('capitalizes the first segment whichever kind leads', () => {
    expect(labelOf(run(), run(), read('a.ts'))).toBe('Ran 2 commands, read 1 file');
    expect(labelOf(read('a.ts'), read('b.ts'))).toBe('Read 2 files');
    expect(labelOf(edit('a.ts'), run())).toBe('Edited 1 file, ran 1 command');
  });

  it('omits zero counts', () => {
    expect(labelOf(edit('a.ts'), read('a.ts'))).toBe('Edited 1 file, read 1 file');
    expect(labelOf(edit('a.ts'), run(), run())).toBe('Edited 1 file, ran 2 commands');
  });

  it('falls back to "N tool calls" when no tool has a view', () => {
    expect(labelOf(tool('ok'), tool('ok'), tool('ok'))).toBe('3 tool calls');
  });

  it('falls back to "N tool calls" when every view is an unrecognized kind', () => {
    // A future/unknown kind (server ahead of client) is not named, and with
    // nothing else recognized the run keeps the generic count.
    const unknown = tool('ok', { kind: 'diagram' } as unknown as ToolView);
    expect(labelOf(unknown, tool('ok'))).toBe('2 tool calls');
  });

  it('names only the recognized kinds in a mixed recognized + unrecognized run', () => {
    const unknown = tool('ok', { kind: 'diagram' } as unknown as ToolView);
    expect(labelOf(edit('a.ts'), tool('ok'), unknown, run())).toBe('Edited 1 file, ran 1 command');
  });

  it('never counts folded-in thinking', () => {
    const items = groupMessages([edit('a.ts'), think(), run(), think()]);
    const group = items[0] as ToolGroup;
    expect(group.items).toHaveLength(4);
    expect(toolGroupSummary(group).label).toBe('Edited 1 file, ran 1 command');
  });

  it('is provider-blind: the tool name never decides the kind', () => {
    // A tool NAMED like an edit/command with no view is unrecognized; a view
    // of a recognized kind classifies whatever the tool is called.
    const named = (name: string, view?: ToolView): ChatMessage => ({
      seq: (seq += 1),
      kind: 'tool',
      tool: { name, title: 't', status: 'ok', ...(view ? { view } : {}) },
    });
    expect(labelOf(named('Edit'), named('Bash'))).toBe('2 tool calls');
    expect(
      labelOf(
        named('mystery-tool', { kind: 'command', command: 'x' }),
        named('other-tool', { kind: 'diff', path: 'a.ts', text: '@@' }),
      ),
    ).toBe('Edited 1 file, ran 1 command');
  });

  it('leaves the failed count and running marker independent of the label', () => {
    const group = groupMessages([
      edit('a.ts'),
      tool('error', { kind: 'command', command: 'false' }),
      tool('running', { kind: 'read', path: 'b.ts', text: '' }),
    ])[0] as ToolGroup;
    expect(toolGroupSummary(group)).toEqual({
      label: 'Edited 1 file, ran 1 command, read 1 file',
      failed: '1 failed',
      running: true,
    });
    // The fallback label carries them too.
    const plain = groupMessages([tool('error'), tool('running')])[0] as ToolGroup;
    expect(toolGroupSummary(plain)).toEqual({
      label: '2 tool calls',
      failed: '1 failed',
      running: true,
    });
  });
});

// The render-item reconciler (issue #175): groupMessages builds fresh wrapper
// objects per call, so RunChat's memo reuses the previous run's items — by
// message reference for wrappers, by key + member identity for groups — and
// returns the prev ARRAY itself for a no-op regroup.
describe('reconcileRenderItems (issue #175)', () => {
  it('returns the prev array itself when nothing changed', () => {
    const msgs = [text('a'), tool('ok'), tool('ok'), text('b')];
    const prev = groupMessages(msgs);
    const next = groupMessages(msgs); // fresh wrappers over the SAME messages
    expect(next).not.toBe(prev);
    expect(next[1]).not.toBe(prev[1]); // the group wrapper churned…
    expect(reconcileRenderItems(prev, next)).toBe(prev); // …but reconcile hides it
  });

  it('reuses untouched wrappers when a message is appended', () => {
    const msgs = [text('a'), tool('ok'), tool('ok')];
    const prev = groupMessages(msgs); // [message, toolGroup]
    const out = reconcileRenderItems(prev, groupMessages([...msgs, text('b')]));
    expect(out).not.toBe(prev);
    expect(out).toHaveLength(3);
    expect(out[0]).toBe(prev[0]); // the text wrapper survives
    expect(out[1]).toBe(prev[1]); // the untouched group survives
    expect(out[2]!.kind).toBe('message'); // only the append is new
  });

  it('rebuilds only the group whose member changed, reusing the neighbors', () => {
    const running = tool('running');
    const msgs = [text('a'), running, tool('ok'), text('b')];
    const prev = groupMessages(msgs); // [message, toolGroup(key=running.seq), message]
    // The running tool flips ok: a NEW message object at the same seq.
    const flipped: ChatMessage = { ...running, tool: { ...running.tool!, status: 'ok' } };
    const out = reconcileRenderItems(prev, groupMessages([msgs[0]!, flipped, msgs[2]!, msgs[3]!]));
    expect(out).not.toBe(prev);
    expect(out[0]).toBe(prev[0]); // neighbor reused
    expect(out[2]).toBe(prev[2]); // neighbor reused
    expect(out[1]).not.toBe(prev[1]); // the group rebuilt around the new member
    expect((out[1] as ToolGroup).running).toBe(false);
    expect((prev[1] as ToolGroup).running).toBe(true); // prev untouched (pure)
  });
});
