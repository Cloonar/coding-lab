// The conversation stream's message renderers: text bubbles with markdown and
// copy-raw (issue #13 decisions 3/14), tool chips and grouped tool runs whose
// click branches on the breakpoint (issue #154 inline expansion vs the issue
// #145 detail sheet), answered-dialog summaries (issue #56 decision 3) and
// lifecycle lines. Thinking is permanently dropped at paint (issue #68).

import { For, Match, Show, Switch } from 'solid-js';
import { type ChatMessage } from '../../api';
import Icon from '../../components/Icon';
import { toolStatusMark } from '../../components/ToolPanel';
import ToolViewBody, { isFileView } from '../../components/ToolViews';
import { toolGroupSummary, type ToolGroup } from '../../lib/toolGroups';
import { AnsweredDialog } from './Dialogs';
import { CopyButton, Markdown } from './Markdown';

export function MessageView(props: {
  message: ChatMessage;
  /** Desktop (>=1024px): the chip's click toggles inline expansion; mobile: it
   *  opens the sheet. Threaded through the group-body recursion too, which only
   *  renders on desktop. Meaningful only for kind 'tool'. */
  desktop: boolean;
  /** Panel wiring for a lone tool chip (issue #145): whether this chip is what
   *  the sheet shows on mobile, and the tap that opens/retargets it. Meaningful
   *  only for kind 'tool'; the other kinds ignore both. */
  toolSelected: boolean;
  onOpenTool: () => void;
  /** Desktop inline expansion (issue #154): whether this chip's rich body is
   *  open, and the tap that toggles it. */
  toolExpanded: boolean;
  onToggleTool: () => void;
}) {
  const m = () => props.message;
  // Copy-raw (decision 14) is assistant-only: user replies are already plain
  // and selectable, thinking is hidden noise.
  const copyable = () => m().kind === 'text' && m().role !== 'user' && m().thinking !== true;
  return (
    <Switch>
      <Match when={m().kind === 'text'}>
        <div
          classList={{
            'chat-msg': true,
            [`role-${m().role ?? 'assistant'}`]: true,
            thinking: m().thinking === true,
          }}
        >
          <Show when={m().thinking}>
            <span class="chat-msg-tag">thinking</span>
          </Show>
          {/* Markdown render (decision 3): every text/thinking message, any role. */}
          <Markdown source={m().text ?? ''} />
          <Show when={copyable()}>
            <div class="chat-msg-actions">
              <CopyButton text={m().text ?? ''} label="Copy" title="Copy the raw markdown" />
            </div>
          </Show>
        </div>
      </Match>
      <Match when={m().kind === 'tool'}>
        <ToolChip
          message={m()}
          desktop={props.desktop}
          selected={props.toolSelected}
          onOpen={props.onOpenTool}
          expanded={props.toolExpanded}
          onToggle={props.onToggleTool}
        />
      </Match>
      {/* An ANSWERED dialog stays in history as a compact, inert Q→A summary
          (issue #56 decision 3). Outcome PRESENCE is the answered signal —
          never the inner fields: a plan rejected without typed feedback
          serializes as an all-omitempty {}. A dialog message WITHOUT an
          outcome keeps the prompt-only line below (the pending interactive
          card is intercepted upstream by pendingCardFor, not here). */}
      <Match when={m().kind === 'dialog' && m().dialog?.outcome ? m().dialog : null}>
        {(d) => <AnsweredDialog dialog={d()} />}
      </Match>
      <Match when={m().kind === 'dialog'}>
        <div class="chat-dialog-inline">
          <p class="chat-dialog-prompt">{m().dialog?.prompt}</p>
        </div>
      </Match>
      <Match when={m().kind === 'lifecycle'}>
        <p classList={{ 'chat-lifecycle': true, error: m().error === true }}>{m().text}</p>
      </Match>
    </Switch>
  );
}

// A single tool call: a summary-row button inside a .chat-tool frame. The click
// branches on the breakpoint (issue #154) — desktop toggles the rich inline
// body (ToolViewBody) in place, mobile opens the detail sheet (issue #145). The
// frame class (.chat-tool) is the wrapper div, the row class (.tool-summary) the
// inner button, split from the single pre-#154 button so the body can hang below
// the row without native <details> fighting the breakpoint branch.
//
// On DESKTOP a FILE tool (diff/write/read — isFileView) also gets an "open in
// sidebar" affordance (issue #154 §2): a small icon-button as the summary's
// SIBLING inside the row wrapper (never nested — a button in a button is invalid
// HTML), and the SAME button as ToolViewBody's pathAction in the expanded body.
// Its click opens/retargets the file sidebar (onOpen); the summary click still
// toggles inline expansion. Command/fallback tools, and every chip on mobile,
// get no affordance anywhere. `selected` (the sidebar's source on desktop, the
// sheet's on mobile) rides the wrapper for the highlight and the summary button
// for its aria-pressed; `aria-expanded` reflects the desktop inline state.
function ToolChip(props: {
  message: ChatMessage;
  desktop: boolean;
  selected: boolean;
  onOpen: () => void;
  expanded: boolean;
  onToggle: () => void;
}) {
  const t = () => props.message.tool;
  // Desktop file tools only: the sidebar accepts diff/write/read, nothing else.
  const showAffordance = () => props.desktop && isFileView(props.message);
  const sidebarButton = () => (
    <button
      type="button"
      class="icon-btn tool-open-sidebar"
      aria-label="Open in sidebar"
      title="Open in sidebar"
      onClick={() => props.onOpen()}
    >
      <Icon name="panel-right" size={16} />
    </button>
  );
  return (
    <div classList={{ 'chat-tool': true, selected: props.selected }}>
      {/* wrapper > row: the summary button (flex:1) plus, on desktop file tools,
          the sidebar affordance as a SIBLING — never nested. */}
      <div class="tool-summary-row">
        <button
          type="button"
          classList={{
            'tool-summary': true,
            [`tool-${t()?.status ?? 'ok'}`]: true,
            selected: props.selected,
          }}
          aria-pressed={props.selected}
          aria-expanded={props.desktop && props.expanded}
          onClick={() => (props.desktop ? props.onToggle() : props.onOpen())}
        >
          <span class="tool-title">{t()?.title}</span>
          <span class="tool-status">{toolStatusMark(t()?.status)}</span>
        </button>
        <Show when={showAffordance()}>{sidebarButton()}</Show>
      </div>
      {/* Desktop only: the rich body in place. props.message is passed live (a
          JSX getter, never a captured snapshot) so a refetch that grows the
          tool output flows straight through — see ToolViews' liveness note. A
          file tool's path header carries the same sidebar affordance
          (pathAction); the sheet and the sidebar itself never get one. */}
      <Show when={props.desktop && props.expanded}>
        <div class="tool-inline-body">
          <ToolViewBody
            message={props.message}
            pathAction={showAffordance() ? sidebarButton() : undefined}
          />
        </div>
      </Show>
    </div>
  );
}

// A run of 2+ tool calls behind one summary line (decisions 8–11): the count
// plus rolled-up failure/liveness. The click branches on the breakpoint (issue
// #154) — desktop toggles the run open in place (member chips stack in the
// body, each independently expandable), mobile opens the panel at the group's
// LIST page (issue #145). Folded-in thinking is dropped at paint (issue #68).
export function ToolGroupView(props: {
  group: ToolGroup;
  desktop: boolean;
  selected: boolean;
  onOpen: () => void;
  open: boolean;
  onToggle: () => void;
  toolExpanded: (seq: number) => boolean;
  onToggleTool: (seq: number) => void;
  toolSelected: (m: ChatMessage) => boolean;
  onOpenTool: (seq: number) => void;
}) {
  const summary = () => toolGroupSummary(props.group);
  const items = () => props.group.items.filter((m) => !(m.kind === 'text' && m.thinking));
  return (
    <div classList={{ 'chat-tool-group': true, selected: props.selected }}>
      <button
        type="button"
        classList={{
          'tool-group-summary': true,
          'has-error': props.group.errorCount > 0,
          selected: props.selected,
        }}
        aria-pressed={props.selected}
        aria-expanded={props.desktop && props.open}
        onClick={() => (props.desktop ? props.onToggle() : props.onOpen())}
      >
        <span class="tool-group-count">{summary().label}</span>
        <Show when={summary().failed}>
          {(f) => <span class="tool-group-failed"> · {f()}</span>}
        </Show>
        <Show when={summary().running}>
          <span class="tool-group-running"> · running…</span>
        </Show>
      </button>
      {/* Desktop only: the expanded run recurses into MessageView per non-thinking
          item, each chip an independently expandable body of its own. */}
      <Show when={props.desktop && props.open}>
        <div class="tool-group-body">
          <For each={items()}>
            {(m) => (
              <MessageView
                message={m}
                desktop={props.desktop}
                toolSelected={props.toolSelected(m)}
                onOpenTool={() => props.onOpenTool(m.seq)}
                toolExpanded={props.toolExpanded(m.seq)}
                onToggleTool={() => props.onToggleTool(m.seq)}
              />
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}
