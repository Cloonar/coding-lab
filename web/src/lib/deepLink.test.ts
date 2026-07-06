// Deep-link connecting-state logic: the row pulses while the capture runs,
// a captured link always wins, and a finished capture with no link falls
// back to the generic claude.ai session picker with the v0 title.

import { describe, expect, it } from 'vitest';
import { GENERIC_DEEP_LINK, GENERIC_LINK_TITLE, openState } from './deepLink';

describe('openState', () => {
  it('shows the connecting pulse while capture runs and no link exists', () => {
    expect(openState({ connecting: true, deep_link_url: null })).toEqual({ kind: 'connecting' });
  });

  it('renders the exact deep link once captured', () => {
    expect(
      openState({ connecting: false, deep_link_url: 'https://claude.ai/code/session_abc' }),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('a captured link beats a still-set connecting flag', () => {
    expect(
      openState({ connecting: true, deep_link_url: 'https://claude.ai/code/session_abc' }),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('falls back to the generic session picker after a missed capture', () => {
    expect(openState({ connecting: false, deep_link_url: null })).toEqual({
      kind: 'link',
      url: GENERIC_DEEP_LINK,
      exact: false,
      title: GENERIC_LINK_TITLE,
    });
  });

  it('treats an empty-string link like a missing one', () => {
    expect(openState({ connecting: false, deep_link_url: '' })).toMatchObject({
      kind: 'link',
      url: GENERIC_DEEP_LINK,
      exact: false,
    });
  });

  it('pins the v0 fallback URL and title verbatim', () => {
    expect(GENERIC_DEEP_LINK).toBe('https://claude.ai/code');
    expect(GENERIC_LINK_TITLE).toBe(
      "Opens the claude.ai session picker — the exact deep link wasn't captured",
    );
  });
});
