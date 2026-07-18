// Render-time coalescing of tool-call runs for the embedded chat (issue #13,
// decisions 7–12). The server emits a flat message list; collapsing consecutive
// tool activity into one "N tool calls" disclosure is a pure display concern —
// no backend/schema change. This helper turns the flat list into render items:
// either a passthrough message, or a tool group.
//
// Run rule: scan maximal runs of {tool | thinking} messages (a run breaks on
// text/dialog/lifecycle). thinking FOLDS IN — it never breaks a run and is not
// counted (agents interleave thinking → tool → thinking → tool, and thinking
// is hidden-by-default noise). A run with 2+ tools becomes a group; a run with
// 0 or 1 tools passes through message-by-message (a lone tool renders exactly
// as before). The group key is the first tool's immutable seq, so the view's
// open-state signal survives SSE refetches (decision 12).

import type { ChatMessage } from '../api';

export interface ToolGroup {
  kind: 'toolGroup';
  /** The first tool's seq — the stable key across refetches (decision 12). */
  key: number;
  /** Tools and interleaved thinking, in transcript order. */
  items: ChatMessage[];
  /** Tool messages only (folded-in thinking is not counted). */
  toolCount: number;
  /** Tools whose status is 'error' — surfaced on the collapsed summary. */
  errorCount: number;
  /** A tool is still in flight — the summary shows liveness (decision 11). */
  running: boolean;
}

export type RenderItem = { kind: 'message'; message: ChatMessage } | ToolGroup;

function isThinking(m: ChatMessage): boolean {
  return m.kind === 'text' && m.thinking === true;
}

function foldsIntoRun(m: ChatMessage): boolean {
  return m.kind === 'tool' || isThinking(m);
}

export function groupMessages(messages: ChatMessage[]): RenderItem[] {
  const items: RenderItem[] = [];
  let i = 0;
  while (i < messages.length) {
    const m = messages[i]!;
    if (!foldsIntoRun(m)) {
      items.push({ kind: 'message', message: m });
      i += 1;
      continue;
    }
    // Gather the maximal {tool | thinking} run starting here.
    let j = i;
    while (j < messages.length && foldsIntoRun(messages[j]!)) j += 1;
    const run = messages.slice(i, j);
    const tools = run.filter((r) => r.kind === 'tool');
    if (tools.length >= 2) {
      items.push({
        kind: 'toolGroup',
        key: tools[0]!.seq,
        items: run,
        toolCount: tools.length,
        errorCount: tools.filter((t) => t.tool?.status === 'error').length,
        running: tools.some((t) => t.tool?.status === 'running'),
      });
    } else {
      // 0 or 1 tools: no group — every message renders as it does today.
      for (const r of run) items.push({ kind: 'message', message: r });
    }
    i = j;
  }
  return items;
}

/**
 * Reconcile a fresh groupMessages() result against the previous one (issue
 * #175). groupMessages builds new wrapper objects on every call, so even
 * messages whose identity survived chatStream's hash merge would re-key
 * Solid's reference-keyed <For> and tear the settled DOM down on every SSE
 * tick. A prev item is reused when it is EQUIVALENT: a message wrapper
 * carrying the SAME message reference, or a group with the same key, the same
 * items length, and every items[i] reference-equal (the count/error/running
 * rollups derive from those same members, so they cannot differ). Matching is
 * keyed (message seq / group key), not positional, so a prepend ("Load
 * earlier") still reuses the untouched tail. When every position reuses
 * prev's item and the lengths match, the PREV ARRAY itself is returned, so a
 * no-op regroup propagates nothing.
 */
export function reconcileRenderItems(prev: RenderItem[], next: RenderItem[]): RenderItem[] {
  const prevMessages = new Map<number, Extract<RenderItem, { kind: 'message' }>>();
  const prevGroups = new Map<number, ToolGroup>();
  for (const item of prev) {
    if (item.kind === 'message') prevMessages.set(item.message.seq, item);
    else prevGroups.set(item.key, item);
  }
  const out = next.map((item): RenderItem => {
    if (item.kind === 'message') {
      const p = prevMessages.get(item.message.seq);
      return p !== undefined && p.message === item.message ? p : item;
    }
    const p = prevGroups.get(item.key);
    return p !== undefined &&
      p.items.length === item.items.length &&
      item.items.every((m, i) => p.items[i] === m)
      ? p
      : item;
  });
  return out.length === prev.length && out.every((item, i) => item === prev[i]) ? prev : out;
}

/** The collapsed summary line: "N tool calls[ · M failed][ · running…]". */
export function toolGroupSummary(group: ToolGroup): {
  label: string;
  failed: string | null;
  running: boolean;
} {
  return {
    label: `${group.toolCount} tool calls`,
    failed: group.errorCount > 0 ? `${group.errorCount} failed` : null,
    running: group.running,
  };
}
