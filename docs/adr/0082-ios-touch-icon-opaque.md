# The iOS touch icon is its own opaque, full-bleed asset

Adding lab to an iPhone's Home Screen never produced the blue terminal-prompt tile, while other PWAs on the same device got theirs. The server side was correct: the deployed binary served the linked icon with `200 image/png`, and the shipped `index.html` carried the `apple-touch-icon` link. The link, however, pointed at `/icons/icon-192.png`, the manifest's rounded-rect icon: an RGBA PNG whose corners are fully transparent. iOS requires an opaque Home Screen icon. It composites alpha onto black, which leaves black wedges in the corners, and some versions discard the image altogether and fall back to a screenshot or a letter tile. Android and desktop browsers, which read the manifest's `icons` and handle alpha, were never affected, which is why the defect survived from the M8 PWA groundwork (ADR-0013) until the first real iOS install.

Decisions, pinned:

- **A separate file for iOS.** `web/public/apple-touch-icon.png` is 180×180, truecolor RGB with no alpha channel, the `#2563eb` background bleeding to every edge, square corners, and the glyph at the same scale as the "any" icon. iOS applies its own squircle mask, so a pre-rounded tile would only ever add black corners. `index.html` links it with `sizes="180x180"`. The manifest's `icons` are unchanged: the rounded "any" icons and the maskable pair keep serving every other platform.
- **It lives at the site root.** Safari probes `/apple-touch-icon.png` on its own when it has not parsed the link, and uses the same file for bookmarks and Reading List. The embedded-UI test `TestEmbeddedPWAAssets` pins the path with its content type, so a build that drops it fails rather than regressing silently.
- **Every PNG icon is generated.** `web/scripts/icons.sh` redraws the five PNGs from one geometry with ImageMagick draw primitives and overwrites them in place. The SVG stays the design source, but it is not the render input: ImageMagick's built-in SVG renderer drops stroked paths, and the librsvg delegate is not a repository dependency. The script's coordinates mirror `icon.svg` and say so.
- **Cache behaviour is documented, not worked around.** iOS keeps the tile it captured at install time, so an app already on the Home Screen has to be removed and added again once. No cache-busting name is used: the file's name is the contract Safari probes.

## Status

Accepted 2026-10-09. Resolves issue #94.

- **ADR-0013:** the PWA deliverable's "icons" gain this iOS-specific asset; the manifest, service worker and offline shell are unchanged.

## Considered options

- **Make the existing `icon-192.png` opaque.** Rejected: it is the manifest's "any" icon, and launchers that honour "any" draw it as-is, so filling its corners would put a hard square where Android today shows the rounded tile.
- **Reuse the maskable icon for iOS.** Rejected: maskable icons keep the glyph inside an 80% safe circle, which under Apple's much larger squircle reads as a small glyph on an empty tile.
- **Render the icons from the SVG.** Rejected for now: the only SVG renderer available in every build environment loses the strokes. Switching to librsvg or resvg is a tooling decision that can replace the draw primitives later without changing any output.

## Consequences

- Adding lab to an iOS Home Screen shows the blue terminal-prompt tile. Existing installs see it after a remove-and-re-add.
- A change to the icon artwork is a change to `icons.sh` followed by running it; hand-edited PNGs will be overwritten by the next run.
- The embedded-UI build and its test now require `apple-touch-icon.png` in `web/public`.
