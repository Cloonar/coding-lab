// groupMessages contract (issue #13, decisions 7–12): consecutive tool activity
// coalesces into one disclosure; thinking folds in but is not counted; a lone
// tool stays a plain chip; the summary rolls up errors and liveness; the key is
// the first tool's seq so the view's open state survives refetches.

import { beforeEach, describe, expect, it } from 'vitest';
import type { ChatMessage, ToolInfo } from '../api';
import {
  groupMessages,
  reconcileRenderItems,
  toolGroupSummary,
  type ToolGroup,
} from './toolGroups';

let seq = 0;
const tool = (status: ToolInfo['status']): ChatMessage => ({
  seq: (seq += 1),
  kind: 'tool',
  tool: { name: 'Bash', title: 't', status },
});
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
