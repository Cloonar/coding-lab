// stateBadge contract (issue #7): the chat tailer's live conversational
// states map to a chip label + CSS modifier + title; idle / ended / empty
// carry no badge at all (the row's other affordances already say it).

import { describe, expect, it } from 'vitest';
import type { ConversationState } from '../api';
import { stateBadge } from './conversation';

describe('stateBadge', () => {
  it('maps working to the working chip', () => {
    expect(stateBadge('working')).toEqual({
      label: 'working',
      cls: 'working',
      title: 'The agent is working',
    });
  });

  it('maps needs_input to the needs-input chip', () => {
    expect(stateBadge('needs_input')).toEqual({
      label: 'needs input',
      cls: 'needs-input',
      title: 'The agent is waiting for you',
    });
  });

  it('maps question to the question chip', () => {
    expect(stateBadge('question')).toEqual({
      label: 'question',
      cls: 'question',
      title: 'The agent is asking a question',
    });
  });

  it('returns null for the badge-less states', () => {
    const badgeless: ConversationState[] = ['idle', 'ended', ''];
    for (const state of badgeless) {
      expect(stateBadge(state)).toBeNull();
    }
  });
});
