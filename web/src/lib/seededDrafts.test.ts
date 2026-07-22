// seededDrafts contract (issue #198), extracted from the RepoSettings
// stale-draft resync semantics: after a model refresh an untouched draft
// follows the server, a dirty draft keeps the operator's edit, a dirty draft
// the server caught up with reads clean again, and seed() advances so
// buildPatch diffs against the new baseline. Object-valued fields resync by
// the caller's `equals` (key-order-insensitive here), not identity.

import { createRoot, createSignal } from 'solid-js';
import { afterEach, describe, expect, it } from 'vitest';
import { createSeededDrafts } from './seededDrafts';

interface Model {
  name: string;
  branch: string;
  bag: Record<string, boolean>;
}

function base(): Model {
  return { name: 'coding-lab', branch: 'main', bag: { ultra: false, verbose: true } };
}

/** Key-order-independent equality — the repo AFK options bag convention. */
const bagKey = (bag: Record<string, boolean>): string =>
  JSON.stringify(
    Object.fromEntries(
      Object.keys(bag)
        .sort()
        .map((key) => [key, bag[key]]),
    ),
  );
const bagEquals = (a: Record<string, boolean>, b: Record<string, boolean>): boolean =>
  bagKey(a) === bagKey(b);

let dispose: (() => void) | undefined;

afterEach(() => {
  dispose?.();
  dispose = undefined;
});

/** Store + one draft per model field, created inside a reactive root so the
 *  resync effect is owned and re-runs on setModel. */
function setup() {
  const [model, setModel] = createSignal<Model>(base());
  return createRoot((d) => {
    dispose = d;
    const drafts = createSeededDrafts(model);
    const [name, setName] = drafts.field((m) => m.name);
    const [branch, setBranch] = drafts.field((m) => m.branch);
    const [bag, setBag] = drafts.field((m) => m.bag, bagEquals);
    return { setModel, drafts, name, setName, branch, setBranch, bag, setBag };
  });
}

describe('createSeededDrafts', () => {
  it('untouched drafts follow a model refresh', () => {
    const s = setup();
    s.setModel({ ...base(), name: 'renamed', branch: 'dev' });
    expect(s.name()).toBe('renamed');
    expect(s.branch()).toBe('dev');
  });

  it('dirty drafts survive a refresh; unrelated untouched fields still follow', () => {
    const s = setup();
    s.setName('operator-edit');
    s.setModel({ ...base(), name: 'server-renamed', branch: 'dev' });
    expect(s.name()).toBe('operator-edit'); // the in-flight edit is kept
    expect(s.branch()).toBe('dev'); // untouched neighbour follows
  });

  it('a dirty draft the server caught up with becomes clean', () => {
    const s = setup();
    s.setName('saved-name');
    // Our own save landing: fresh carries exactly the drafted value.
    s.setModel({ ...base(), name: 'saved-name' });
    expect(s.name()).toBe('saved-name');
    expect(s.drafts.seed().name).toBe('saved-name'); // draft === seed → clean
    // Clean again means the NEXT refresh is followed, not fought.
    s.setModel({ ...base(), name: 'later-rename' });
    expect(s.name()).toBe('later-rename');
  });

  it('seed() advances on refresh — the dirty diff runs against the new baseline', () => {
    const s = setup();
    s.setBranch('feature');
    s.setModel({ ...base(), branch: 'dev' });
    expect(s.branch()).toBe('feature'); // still the operator's edit
    expect(s.drafts.seed().branch).toBe('dev'); // baseline moved with the server
    expect(s.branch()).not.toBe(s.drafts.seed().branch); // still diffs as dirty
    expect(s.name()).toBe(s.drafts.seed().name); // untouched field diffs clean
  });

  it('custom equals: a key-order variant counts as untouched and follows fresh', () => {
    const s = setup();
    // Same content, new identity + reversed key order: `===` would call this
    // dirty and strand it; bagEquals reads it as untouched.
    s.setBag({ verbose: true, ultra: false });
    s.setModel({ ...base(), bag: { ultra: true, verbose: true } });
    expect(s.bag()).toEqual({ ultra: true, verbose: true });
    // A genuinely dirty bag survives the next refresh.
    s.setBag({ ultra: false, verbose: false });
    s.setModel({ ...base(), bag: { ultra: true, verbose: true } });
    expect(s.bag()).toEqual({ ultra: false, verbose: false });
  });
});
