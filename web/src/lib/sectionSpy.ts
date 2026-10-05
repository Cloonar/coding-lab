// Which section of a long page is "in view" (issue #61): the pure half of the
// repo settings scroll-spy. The section chips and the outline mark one section
// as current while the operator scrolls; that pick is plain arithmetic over
// each section's top edge, kept here so it can be tested without a layout
// engine (jsdom has neither layout nor IntersectionObserver).
//
// The rule: a horizontal line sits just under whatever is stuck to the top of
// the viewport. The section in view is the LAST one whose top edge has reached
// that line — its heading has scrolled up to (or past) the line and the next
// section's has not. Before the first section reaches the line the first one
// is current, and once the page is scrolled to its end the last one is: a
// short final section can never climb to the line on its own.

export interface SectionTop {
  /** The section's identifier (its slug). */
  id: string;
  /** Its top edge, in px from the top of the viewport (negative = scrolled past). */
  top: number;
}

/**
 * The id of the section in view, or undefined for an empty list. `sections`
 * are in page order; `line` is the px offset of the marker line from the top
 * of the viewport; `atEnd` says the page cannot scroll any further.
 */
export function sectionInView(
  sections: readonly SectionTop[],
  line: number,
  atEnd: boolean,
): string | undefined {
  const last = sections[sections.length - 1];
  if (last === undefined) return undefined;
  if (atEnd) return last.id;
  let current = sections[0]!.id;
  for (const section of sections) {
    if (section.top <= line) current = section.id;
  }
  return current;
}
