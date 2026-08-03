// The chrome shared by both settings areas (issue #198): on mobile the bare
// index route lists tappable category rows, and a section shows a back header
// (deliberately "back to the index", not browser-back) above its content; on
// desktop (>=1024px — the AppShell DESKTOP_MIN_PX shell breakpoint) the bare
// index REDIRECTS to the first category and a section renders master-detail
// with a slim inline category nav. The redirect is reactive: growing the
// viewport to desktop while sitting on the bare index triggers it live. Dumb
// by design — no data fetching; the page-level <main class="page">, Crumbs
// and RequireAuth stay in the area root components, not here.

import { A, Navigate } from '@solidjs/router';
import { For, Match, Show, Switch } from 'solid-js';
import type { JSX } from 'solid-js';
import Icon from '../Icon';
import { createMediaQuery } from '../../lib/media';
import type { SettingsCategory } from './categories';

/** Same breakpoint as the app shell (AppShell.tsx DESKTOP_MIN_PX). */
const DESKTOP_QUERY = '(min-width: 1024px)';

export default function SettingsLayout(props: {
  /** Index path, e.g. '/settings' or `/repos/${id}/settings`. */
  base: string;
  categories: SettingsCategory[];
  /** Active slug; undefined = the bare index route. */
  section?: string;
  /** Heading block rendered above the mobile index rows. */
  indexTitle?: JSX.Element;
  /** The active section's content. */
  children?: JSX.Element;
}) {
  const desktop = createMediaQuery(DESKTOP_QUERY);
  const active = () => props.categories.find((c) => c.slug === props.section);

  return (
    <Switch>
      <Match when={props.section === undefined}>
        {/* Bare index: desktop has no index page — it bounces to the first
            category. Show is keyed on the media signal, so growing the
            viewport while the index sits open redirects live. */}
        <Show
          when={desktop() ? props.categories[0] : undefined}
          fallback={
            <>
              {props.indexTitle}
              <div class="settings-index">
                <For each={props.categories}>
                  {(c) => (
                    <A
                      href={`${props.base}/${c.slug}`}
                      classList={{ 'settings-index-row': true, danger: c.danger === true }}
                    >
                      {/* Icon takes no color prop: the glyph strokes with
                          currentColor, so the danger modifier tints it
                          through CSS alone. */}
                      <Icon name={c.icon} size={20} class="settings-index-icon" />
                      <span class="settings-index-text">
                        <span class="settings-index-title">{c.title}</span>
                        <span class="settings-index-desc">{c.description}</span>
                      </span>
                      <Icon name="chevron-right" size={18} class="settings-index-chevron" />
                    </A>
                  )}
                </For>
              </div>
            </>
          }
        >
          {(first) => <Navigate href={`${props.base}/${first().slug}`} />}
        </Show>
      </Match>
      {/* Unknown slug → the index: mobile shows it; desktop's index
          immediately re-redirects to the first slug. */}
      <Match when={active() === undefined}>
        <Navigate href={props.base} />
      </Match>
      <Match when={active()}>
        {(category) => (
          <Show
            when={desktop()}
            fallback={
              <>
                <div class="settings-back-head">
                  <A href={props.base} class="settings-back-link" aria-label="All settings">
                    <Icon name="chevron-left" size={20} />
                  </A>
                  <h2>{category().title}</h2>
                </div>
                {props.children}
              </>
            }
          >
            <div class="settings-split">
              {/* Deliberately lighter than the SideNav rail: plain text links,
                  no icons, no pills — active is accent color + weight only,
                  danger in --danger. The explicit classList wins over the
                  router A's own active/inactive marking (merged after it). */}
              <nav class="settings-split-nav" aria-label="Settings sections">
                <For each={props.categories}>
                  {(c) => (
                    <A
                      href={`${props.base}/${c.slug}`}
                      classList={{
                        'settings-split-link': true,
                        active: c.slug === category().slug,
                        danger: c.danger === true,
                      }}
                    >
                      {c.title}
                    </A>
                  )}
                </For>
              </nav>
              <div class="settings-split-body">{props.children}</div>
            </div>
          </Show>
        )}
      </Match>
    </Switch>
  );
}
