// --- Markdown rendering (issue #13) --------------------------------------
// The parser (lib/markdown) emits a plain node tree; the view maps it to Solid
// JSX nodes directly — never an HTML string, never innerHTML — so rendering is
// XSS-safe by construction. Only allowed-scheme hrefs reach an <a>.

import { Dynamic } from 'solid-js/web';
import { For, Match, Show, Switch, createMemo, createSignal, onCleanup } from 'solid-js';
import Icon from '../../components/Icon';
import { parseMarkdown, type Block, type Inline } from '../../lib/markdown';

export function Markdown(props: { source: string }) {
  const blocks = createMemo(() => parseMarkdown(props.source));
  return (
    <div class="chat-msg-text md">
      <For each={blocks()}>{(b) => <BlockView block={b} />}</For>
    </div>
  );
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
  return (
    <Switch>
      <Match when={props.block.ordered}>
        <ol class="md-list" start={props.block.start}>
          <For each={items()}>
            {(item) => (
              <li>
                <For each={item}>{(c) => <BlockView block={c} />}</For>
              </li>
            )}
          </For>
        </ol>
      </Match>
      <Match when={!props.block.ordered}>
        <ul class="md-list">
          <For each={items()}>
            {(item) => (
              <li>
                <For each={item}>{(c) => <BlockView block={c} />}</For>
              </li>
            )}
          </For>
        </ul>
      </Match>
    </Switch>
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
