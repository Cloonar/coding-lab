// --- Markdown rendering (issues #13, #103) --------------------------------
// One renderer, two consumers: the run chat (issue #13) and the issue view
// (issue #103). The parser (lib/markdown) emits a plain node tree; the view
// maps it to Solid JSX nodes directly — never an HTML string, never innerHTML —
// so rendering is XSS-safe by construction. Only allowed-scheme hrefs reach an
// <a>; an issue reference's href comes from the caller's resolver, never from
// the source text. The caller owns the wrapper class and whether `#N` links.

import { A } from '@solidjs/router';
import { Dynamic } from 'solid-js/web';
import {
  For,
  Match,
  Show,
  Switch,
  createContext,
  createMemo,
  createSignal,
  onCleanup,
  useContext,
} from 'solid-js';
import Icon from './Icon';
import { parseMarkdown, type Block, type Inline } from '../lib/markdown';

/** Where an issue reference `#N` points: an in-app route (SPA navigation) or
 *  an external page (new tab). */
export interface RefTarget {
  href: string;
  external: boolean;
}
/** Maps an issue number to its link target; null leaves `#N` as plain text. */
export type RefResolver = (n: number) => RefTarget | null;

// The resolver rides a context so the deep inline renderer reaches it without
// threading a prop through every block. Absent (the chat) → refs stay text.
const RefContext = createContext<RefResolver>(() => null);

export function Markdown(props: {
  source: string;
  class: string;
  resolveRef?: RefResolver;
  taskLists?: boolean;
}) {
  const blocks = createMemo(() => parseMarkdown(props.source, { taskLists: props.taskLists }));
  return <MarkdownTree blocks={blocks()} class={props.class} resolveRef={props.resolveRef} />;
}

/** Render an already-parsed tree — for a caller that inspects the blocks
 *  first (the issue view's Agent-brief detection) without parsing twice. */
export function MarkdownTree(props: { blocks: Block[]; class: string; resolveRef?: RefResolver }) {
  const resolve: RefResolver = (n) => props.resolveRef?.(n) ?? null;
  return (
    <RefContext.Provider value={resolve}>
      <div class={props.class}>
        <For each={props.blocks}>{(b) => <BlockView block={b} />}</For>
      </div>
    </RefContext.Provider>
  );
}

/** Flatten inline nodes to plain text (a ref reads as `#N`, a hard break as a
 *  newline) — for callers matching on a heading's wording. */
export function inlineText(nodes: Inline[]): string {
  return nodes
    .map((n) => {
      switch (n.type) {
        case 'text':
        case 'code':
          return n.value;
        case 'ref':
          return `#${n.number}`;
        case 'break':
          return '\n';
        default:
          return inlineText(n.children);
      }
    })
    .join('');
}

function BlockView(props: { block: Block }) {
  return (
    <Switch>
      <Match when={props.block.type === 'heading' && props.block}>
        {(b) => (
          <Dynamic component={`h${Math.min(Math.max(b().level, 1), 6)}`} class="md-h">
            <InlineNodes nodes={b().children} />
          </Dynamic>
        )}
      </Match>
      <Match when={props.block.type === 'paragraph' && props.block}>
        {(b) => (
          <p class="md-p">
            <InlineNodes nodes={b().children} />
          </p>
        )}
      </Match>
      <Match when={props.block.type === 'code' && props.block}>
        {(b) => <CodeBlock lang={b().lang} text={b().text} />}
      </Match>
      <Match when={props.block.type === 'list' && props.block}>
        {(b) => <ListView block={b()} />}
      </Match>
      <Match when={props.block.type === 'blockquote' && props.block}>
        {(b) => (
          <blockquote class="md-quote">
            <For each={b().children}>{(c) => <BlockView block={c} />}</For>
          </blockquote>
        )}
      </Match>
      <Match when={props.block.type === 'table' && props.block}>
        {(b) => <TableView block={b()} />}
      </Match>
      <Match when={props.block.type === 'hr'}>
        <hr class="md-hr" />
      </Match>
    </Switch>
  );
}

function ListView(props: { block: Extract<Block, { type: 'list' }> }) {
  const items = () => props.block.items;
  // A list carrying task state gets `md-task` too; a plain list keeps the bare
  // class so the chat's DOM is unchanged.
  const cls = () => (props.block.checked ? 'md-list md-task' : 'md-list');
  const checked = (i: number) => props.block.checked?.[i] ?? null;
  return (
    <Switch>
      <Match when={props.block.ordered}>
        <ol class={cls()} start={props.block.start}>
          <For each={items()}>{(item, i) => <ListItem blocks={item} checked={checked(i())} />}</For>
        </ol>
      </Match>
      <Match when={!props.block.ordered}>
        <ul class={cls()}>
          <For each={items()}>{(item, i) => <ListItem blocks={item} checked={checked(i())} />}</For>
        </ul>
      </Match>
    </Switch>
  );
}

// A GFM task item renders a read-only checkbox beside its body; a plain item
// (checked null — including the plain items of a mixed list) renders as ever.
function ListItem(props: { blocks: Block[]; checked: boolean | null }) {
  return (
    <Show
      when={props.checked !== null}
      fallback={
        <li>
          <For each={props.blocks}>{(c) => <BlockView block={c} />}</For>
        </li>
      }
    >
      <li class="md-task-item" classList={{ done: props.checked === true }}>
        <input
          type="checkbox"
          disabled
          checked={props.checked === true}
          aria-label={props.checked ? 'done' : 'open'}
        />
        <div class="md-task-body">
          <For each={props.blocks}>{(c) => <BlockView block={c} />}</For>
        </div>
      </li>
    </Show>
  );
}

function TableView(props: { block: Extract<Block, { type: 'table' }> }) {
  const align = (i: number) => props.block.align[i] ?? 'none';
  // Own overflow-x scroll container: faithful shape, swipe sideways on mobile
  // (decision 5) — never forces the message column to scroll horizontally.
  return (
    <div class="md-table-wrap">
      <table class="md-table">
        <thead>
          <tr>
            <For each={props.block.header}>
              {(cell, i) => (
                <th classList={{ [`md-align-${align(i())}`]: true }}>
                  <InlineNodes nodes={cell} />
                </th>
              )}
            </For>
          </tr>
        </thead>
        <tbody>
          <For each={props.block.rows}>
            {(row) => (
              <tr>
                <For each={row}>
                  {(cell, i) => (
                    <td classList={{ [`md-align-${align(i())}`]: true }}>
                      <InlineNodes nodes={cell} />
                    </td>
                  )}
                </For>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}

function InlineNodes(props: { nodes: Inline[] }) {
  return <For each={props.nodes}>{(n) => <InlineNode node={n} />}</For>;
}

function InlineNode(props: { node: Inline }) {
  return (
    <Switch>
      <Match when={props.node.type === 'text' && props.node}>{(n) => <>{n().value}</>}</Match>
      <Match when={props.node.type === 'break'}>
        <br />
      </Match>
      <Match when={props.node.type === 'code' && props.node}>
        {(n) => <code class="md-code">{n().value}</code>}
      </Match>
      <Match when={props.node.type === 'strong' && props.node}>
        {(n) => (
          <strong>
            <InlineNodes nodes={n().children} />
          </strong>
        )}
      </Match>
      <Match when={props.node.type === 'em' && props.node}>
        {(n) => (
          <em>
            <InlineNodes nodes={n().children} />
          </em>
        )}
      </Match>
      <Match when={props.node.type === 'link' && props.node}>
        {(n) => (
          <a class="md-link" href={n().href} target="_blank" rel="noopener noreferrer">
            <InlineNodes nodes={n().children} />
          </a>
        )}
      </Match>
      <Match when={props.node.type === 'ref' && props.node}>
        {(n) => <RefView number={n().number} />}
      </Match>
    </Switch>
  );
}

// An issue reference: linked only when the caller's resolver says where to —
// external → new tab, internal → SPA navigation via the router's <A> (only
// reached under a resolver, so the router-less chat never renders it).
function RefView(props: { number: number }) {
  const resolve = useContext(RefContext);
  const target = createMemo(() => resolve(props.number));
  const label = () => `#${props.number}`;
  return (
    <Switch fallback={label()}>
      <Match when={target()?.external === true && target()}>
        {(t) => (
          <a class="md-ref" href={t().href} target="_blank" rel="noopener noreferrer">
            {label()}
          </a>
        )}
      </Match>
      <Match when={target()?.external === false && target()}>
        {(t) => (
          <A class="md-ref" href={t().href}>
            {label()}
          </A>
        )}
      </Match>
    </Switch>
  );
}

// A fenced code block with a chat-app-style header bar: language label left,
// copy-raw button right, always visible (mobile has no hover). The copy source
// is the parser-retained literal fence content (decision 13).
function CodeBlock(props: { lang: string; text: string }) {
  return (
    <div class="md-codeblock">
      <div class="md-codeblock-bar">
        <span class="md-codeblock-lang">{props.lang || 'text'}</span>
        <CopyButton text={props.text} title="Copy the code" />
      </div>
      <pre class="md-pre mono">{props.text}</pre>
    </div>
  );
}

// Copy-to-clipboard with icon→check feedback (decision 15): inline SVG icons,
// navigator.clipboard (the embedded server is a secure context). Silent on a
// missing/blocked clipboard rather than throwing.
export function CopyButton(props: { text: string; label?: string; title?: string }) {
  const [copied, setCopied] = createSignal(false);
  let timer: ReturnType<typeof setTimeout> | undefined;
  const copy = async () => {
    try {
      await navigator.clipboard?.writeText(props.text);
      setCopied(true);
      clearTimeout(timer);
      timer = setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable or denied — leave the button unchanged */
    }
  };
  onCleanup(() => clearTimeout(timer));
  return (
    <button
      type="button"
      class="copy-btn"
      classList={{ copied: copied() }}
      aria-label={props.label ?? 'Copy'}
      title={props.title ?? 'Copy'}
      onClick={() => void copy()}
    >
      <Switch>
        <Match when={copied()}>
          <Icon name="check" size={15} class="copy-icon" />
          <span class="copy-label">Copied</span>
        </Match>
        <Match when={!copied()}>
          <Icon name="copy" size={15} class="copy-icon" />
          <Show when={props.label}>
            <span class="copy-label">{props.label}</span>
          </Show>
        </Match>
      </Switch>
    </button>
  );
}
