// Pure label helpers: hex-color normalization + readable-text contrast for
// rendering colored label chips, and the selection algebra the multi-select
// label editor is built on (toggle + set-equality for the dirty gate).

/** Default label color (matches the store's `labels.color` DEFAULT). */
export const DEFAULT_LABEL_COLOR = '#6b7280';

/**
 * Normalizes a hex color to lowercase '#rrggbb'. Accepts an optional leading
 * '#' and 3- or 6-digit hex; anything else → '' (caller falls back).
 */
export function normalizeHex(input: string): string {
  const s = input.trim().replace(/^#/, '').toLowerCase();
  if (/^[0-9a-f]{6}$/.test(s)) return `#${s}`;
  if (/^[0-9a-f]{3}$/.test(s)) return `#${s[0]!}${s[0]!}${s[1]!}${s[1]!}${s[2]!}${s[2]!}`;
  return '';
}

/** Black or white text for readable contrast on a background hex. */
export function readableTextColor(hex: string): '#000000' | '#ffffff' {
  const norm = normalizeHex(hex);
  if (norm === '') return '#000000';
  const r = parseInt(norm.slice(1, 3), 16);
  const g = parseInt(norm.slice(3, 5), 16);
  const b = parseInt(norm.slice(5, 7), 16);
  // Perceived luminance (sRGB weights), 0..1; light backgrounds take black.
  const luminance = (0.299 * r + 0.587 * g + 0.114 * b) / 255;
  return luminance > 0.6 ? '#000000' : '#ffffff';
}

export interface LabelChipStyle {
  background: string;
  color: string;
}

/** Background + contrasting text for a label chip; empty color → default. */
export function labelChipStyle(color: string): LabelChipStyle {
  const background = normalizeHex(color) || DEFAULT_LABEL_COLOR;
  return { background, color: readableTextColor(background) };
}

// --- label-editor selection algebra ---

/** Adds a name if absent, removes it if present; kept items keep their order. */
export function toggleLabel(selected: readonly string[], name: string): string[] {
  return selected.includes(name) ? selected.filter((n) => n !== name) : [...selected, name];
}

/** Set-equality: true when both hold the same names regardless of order. */
export function sameLabelSet(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  const set = new Set(a);
  return b.every((name) => set.has(name));
}
