// The New run page's repository selector (issue #66): a horizontal row of
// pills — the recently used repositories, each with its readiness dot, then
// an "All N" pill that opens the repository picker. The pills are the page's
// only repository control (there is no Repository chip on the composer), and
// nothing here navigates: "All N" opens a picker, never the Repositories page.
//
// The page computes which repos get a pill (recentRepos(), plus the selected
// repo when it is not startable) and which ids are "recent"; this component
// only renders them and reports a pick. The picker is components/Picker.tsx:
// a pinned filter (PickerSearch — name or host as you type, Enter picks the
// first enabled match), then a Recent group and the All repositories group.
// While a query is typed the groups give way to one flat list of matches, as
// in the reference mockup (docs/reference/new-run-mockup.html). Non-startable
// rows (cloning with a live percent, clone failed) are shown disabled with
// their status; a ready repo whose tracker fails shows "tracker failing" and
// stays pickable, so its composer banner can be read and fixed.
//
// Picker ignores a mousedown on its anchor, so the All pill TOGGLES the picker
// on click. Layout and look: styles/newrun-repos.css.

import { For, Show, createMemo, createSignal, type JSX } from 'solid-js';
import type { Repo } from '../../api';
import {
  filterRepos,
  readinessDot,
  repoHost,
  repoRowDisabled,
  repoRowStatus,
} from '../../lib/newRun';
import type { CloneProgress } from '../../stores/cloneProgress';
import Icon from '../Icon';
import Picker, { PickerGroup, PickerList, PickerOption, PickerSearch } from '../Picker';

export interface RepoPillsProps {
  /** Every repo the server listed (the "All N" count and the picker's All group). */
  repos: Repo[];
  /** The pills, already computed by the page (recentRepos(), plus the selected repo when it is not startable), in order. */
  pills: Repo[];
  /** The stored recent ids, most recent first — the picker's Recent group. */
  recentIds: string[];
  selectedId: string | null;
  /** Live clone progress for a cloning repo (stores/cloneProgress). */
  progress: (repoId: string) => CloneProgress | null;
  onPick: (repo: Repo) => void;
}

/** The readiness dot: filled green / red, hollow while pending. The state is
 *  also in words wherever it matters (the picker's status text). */
function Dot(props: { repo: Repo }): JSX.Element {
  return <i class={`repo-dot ${readinessDot(props.repo)}`} aria-hidden="true" />;
}

export default function RepoPills(props: RepoPillsProps): JSX.Element {
  const [open, setOpen] = createSignal(false);
  const [query, setQuery] = createSignal('');
  let allPill: HTMLButtonElement | undefined;
  let search: HTMLInputElement | undefined;

  const close = (): void => {
    setOpen(false);
    setQuery('');
  };
  const toggle = (): void => {
    if (open()) close();
    else setOpen(true);
  };

  // The unfiltered picker body: the stored recent ids that still exist, in
  // order, then everything else.
  const recent = createMemo(() => {
    const byId = new Map(props.repos.map((repo) => [repo.id, repo]));
    const seen = new Set<string>();
    const out: Repo[] = [];
    for (const id of props.recentIds) {
      const repo = byId.get(id);
      if (repo !== undefined && !seen.has(id)) {
        seen.add(id);
        out.push(repo);
      }
    }
    return out;
  });
  const others = createMemo(() => {
    const ids = new Set(recent().map((repo) => repo.id));
    return props.repos.filter((repo) => !ids.has(repo.id));
  });
  const matches = createMemo(() => filterRepos(props.repos, query()));
  const filtering = () => query().trim() !== '';
  // What Enter picks from: the rows as displayed, top to bottom.
  const shown = () => (filtering() ? matches() : [...recent(), ...others()]);

  const pick = (repo: Repo): void => {
    close();
    props.onPick(repo);
  };
  const pickFirst = (): void => {
    const first = shown().find((repo) => !repoRowDisabled(repo));
    if (first !== undefined) pick(first);
  };

  const row = (repo: Repo): JSX.Element => (
    <PickerOption
      selected={repo.id === props.selectedId}
      onSelect={() => pick(repo)}
      leading={<Dot repo={repo} />}
      title={repo.name}
      description={repoHost(repo) || undefined}
      status={repoRowStatus(repo, props.progress(repo.id))}
      disabled={repoRowDisabled(repo)}
    />
  );

  return (
    <>
      <div class="repo-pills-wrap">
        <div class="repo-pills" role="group" aria-label="Repository">
          <For each={props.pills}>
            {(repo) => (
              <button
                type="button"
                class="repo-pill"
                aria-pressed={repo.id === props.selectedId}
                onClick={() => props.onPick(repo)}
              >
                <Dot repo={repo} />
                {repo.name}
              </button>
            )}
          </For>
          <button
            type="button"
            class="repo-pill repo-pill-all"
            ref={allPill}
            aria-haspopup="dialog"
            aria-expanded={open()}
            onClick={toggle}
          >
            All {props.repos.length}
            <Icon name="chevron-down" size={14} />
          </button>
        </div>
      </div>
      <Picker
        open={open()}
        onClose={close}
        title="Repository"
        size="regular"
        anchor={() => allPill}
        initialFocus={() => search}
        header={
          <PickerSearch
            ref={(el) => (search = el)}
            value={query()}
            onInput={setQuery}
            placeholder="Type a repository name"
            aria-label="Filter repositories"
            onEnter={pickFirst}
          />
        }
      >
        <Show
          when={filtering()}
          fallback={
            <>
              <Show when={recent().length > 0}>
                <PickerGroup id="repo-recent">Recent</PickerGroup>
                <PickerList labelledBy="repo-recent">
                  <For each={recent()}>{row}</For>
                </PickerList>
              </Show>
              <Show when={others().length > 0}>
                <PickerGroup id="repo-all">All repositories · {props.repos.length}</PickerGroup>
                <PickerList labelledBy="repo-all">
                  <For each={others()}>{row}</For>
                </PickerList>
              </Show>
            </>
          }
        >
          <Show
            when={matches().length > 0}
            fallback={<p class="repo-picker-empty">No repository matches.</p>}
          >
            <PickerList label="Matching repositories">
              <For each={matches()}>{row}</For>
            </PickerList>
          </Show>
        </Show>
      </Picker>
    </>
  );
}
