// groupMessages contract (issue #13, decisions 7–12): consecutive tool activity
// coalesces into one disclosure; thinking folds in but is not counted; a lone
// tool stays a plain chip; the summary rolls up errors and liveness; the key is
// the first tool's seq so the view's open state survives refetches.

import { beforeEach, describe, expect, it } from 'vitest';
import type { ChatMessage, ToolInfo } from '../api';
import { groupMessages, toolGroupSummary, type ToolGroup } from './toolGroups';

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
