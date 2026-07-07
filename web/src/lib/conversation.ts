// Conversational-state badge mapping (issue #7): the chat tailer's per-instance
// state → a short label and a CSS modifier class. Returns null for the empty /
// idle / ended states, which carry no live badge (the row's other affordances
// already say "not running" or show the outcome).

import type { ConversationState } from '../api';

export interface StateBadge {
  label: string;
  cls: string;
  title: string;
}

export function stateBadge(state: ConversationState): StateBadge | null {
  switch (state) {
    case 'working':
      return { label: 'working', cls: 'working', title: 'The agent is working' };
    case 'needs_input':
      return { label: 'needs input', cls: 'needs-input', title: 'The agent is waiting for you' };
    case 'question':
      return { label: 'question', cls: 'question', title: 'The agent is asking a question' };
    default:
      return null;
  }
}
