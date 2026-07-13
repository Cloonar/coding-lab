// Rich tool detail views (issue #146), extracted from ToolPanel.tsx into a
// shared component (issue #154) so a second surface (a future inline
// expansion) never re-implements the view-kind switch. The server tags a tool
// call with a `view` union; this renders by kind and stays provider-blind (no
// tool-name logic, no brand token). A missing/unknown view falls back to the
// raw input/output <pre> blocks.
//
// LIVENESS: every read below goes through `props.message` (never destructured
// or captured) so an SSE refetch that grows view.text/output flows straight
// through — the same rule the whole panel lives by (see ToolPanel's header
// note). The caller must pass the live-resolved message at render time.

import { For, Match, Show, Switch, type JSX } from 'solid-js';
import type { ChatMessage, ToolView } from '../api';

type PathKind = Extract<ToolView, { kind: 'diff' | 'write' }>;

/** A path-carrying view (diff or write) or undefined. */
function pathView(m: ChatMessage): PathKind | undefined {
  const v = m.tool?.view;
  return v?.kind === 'diff' || v?.kind === 'write' ? v : undefined;
}

/** The command view or undefined. */
function commandView(m: ChatMessage): Extract<ToolView, { kind: 'command' }> | undefined {
  const v = m.tool?.view;
  return v?.kind === 'command' ? v : undefined;
}

/** The read view or undefined (issue #154). */
function readView(m: ChatMessage): Extract<ToolView, { kind: 'read' }> | undefined {
  const v = m.tool?.view;
  return v?.kind === 'read' ? v : undefined;
}

/** A FILE tool (issue #154 §2): its view renders a path — diff / write / read —
 *  the only kinds the desktop file sidebar accepts. A command or an absent view
 *  is not a file tool and gets no "open in sidebar" affordance anywhere. */
export function isFileView(m: ChatMessage): boolean {
  const k = m.tool?.view?.kind;
  return k === 'diff' || k === 'write' || k === 'read';
}

type DiffLine = { text: string; cls: string };

/** Split diff/write text into per-line class assignments. Prefix precedence is
 *  load-bearing: the multi-char file/hunk headers (`+++`, `---`, `@@`) MUST be
 *  tested before the single-char `+`/`-` markers, or a `+++ b/foo` header would
 *  read as an added line. `forceAdd` paints every line added for the write view
 *  — a new file is, in effect, an all-insert diff — and short-circuits the
 *  prefix test so raw content that happens to start with `-`/`@@` isn't mistyped.
 *  Empty lines are kept (blank context lines carry diff alignment); the CSS
 *  gives each line span a min-height so a blank one still occupies a row. */
function diffLines(text: string, forceAdd = false): DiffLine[] {
  return text.split('\n').map((line) => {
    let cls = 'tool-diff-ctx';
    if (forceAdd) cls = 'tool-diff-add';
    else if (line.startsWith('+++') || line.startsWith('---') || line.startsWith('@@'))
      cls = 'tool-diff-hunk';
    else if (line.startsWith('+')) cls = 'tool-diff-add';
    else if (line.startsWith('-')) cls = 'tool-diff-del';
    return { text: line, cls };
  });
}

export interface ToolViewBodyProps {
  /** The tool message to render, resolved LIVE by the caller (never a
   *  captured snapshot) — see the liveness note above. */
  message: ChatMessage;
  /** Optional accessory rendered beside the path text in a path header row
   *  (diff / write / read) — reserved for a later "open in sidebar" action.
   *  Absent today, so the header renders equivalent to before this slot
   *  existed (a plain path div, one inner span around the text). */
  pathAction?: JSX.Element;
}

/** The full rich detail body for one tool message: view arms plus the raw
 *  fallback. Used by ToolPanel's detail page. */
export default function ToolViewBody(props: ToolViewBodyProps): JSX.Element {
  const tool = () => props.message.tool;

  return (
    <Switch
      fallback={
        <>
          <Show when={tool()?.input}>
            <pre class="tool-body mono">{tool()?.input}</pre>
          </Show>
          <Show when={tool()?.output}>
            <pre class="tool-body tool-output mono">{tool()?.output}</pre>
          </Show>
        </>
      }
    >
      {/* diff / write: a path header over a line-styled body. Write forces
          every line to the added style (a new file is an all-insert diff).
          The raw output block rides below ONLY on error — a successful edit's
          "file updated" line is noise, but a failed one must still surface
          its error text. */}
      <Match when={pathView(props.message)}>
        {(view) => (
          <>
            <div class="tool-view-path mono">
              <span class="tool-view-path-text">{view().path}</span>
              {props.pathAction}
            </div>
            <pre class="tool-body mono tool-diff">
              <For each={diffLines(view().text, view().kind === 'write')}>
                {(ln) => <span class={ln.cls}>{ln.text}</span>}
              </For>
            </pre>
            <Show when={tool()?.status === 'error' && tool()?.output}>
              <pre class="tool-body tool-output mono">{tool()?.output}</pre>
            </Show>
          </>
        )}
      </Match>
      {/* command: a $-prefixed command line, then its output as terminal text
          below (shown whenever non-empty, any status — a command speaks
          through its stdout). */}
      <Match when={commandView(props.message)}>
        {(view) => (
          <>
            <pre class="tool-body mono tool-cmd">
              <span class="tool-cmd-prompt">$ </span>
              {view().command}
            </pre>
            <Show when={tool()?.output}>
              <pre class="tool-body tool-output mono">{tool()?.output}</pre>
            </Show>
          </>
        )}
      </Match>
      {/* read (issue #154): a path header over the excerpt, no diff coloring.
          A still-running Read has an empty excerpt — render just the header
          so the operator still sees which file. No error arm: the server
          clears the view entirely on an errored Read. */}
      <Match when={readView(props.message)}>
        {(view) => (
          <>
            <div class="tool-view-path mono">
              <span class="tool-view-path-text">{view().path}</span>
              {props.pathAction}
            </div>
            <Show when={view().text}>
              <pre class="tool-body mono">{view().text}</pre>
            </Show>
          </>
        )}
      </Match>
    </Switch>
  );
}
