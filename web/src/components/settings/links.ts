// The settings pages' in-page links (issue #61): a section chip, an outline
// entry and a save bar link are real links to a section's URL, so a modified
// click does what it does on any link — and only a plain click is the page's
// own business (scroll there, replace the URL).

/** A click the browser should handle itself (new tab, new window, download). */
export function isModifiedClick(event: MouseEvent): boolean {
  return event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
}
