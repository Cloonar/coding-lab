// Cross-cutting chat helper shared by the route's stream notes and the
// composer's agent-name strings (issue #51 decision 9).

/** Sentence-start capitalization for the "the agent" display-name fallback. */
export function capitalize(s: string): string {
  return s === '' ? s : s.charAt(0).toUpperCase() + s.slice(1);
}
