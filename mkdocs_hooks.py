"""Render this repository as a docs site without copying any of it.

The repo is the source of truth; `site/` is a view of it. Three things have to
happen for that to hold, and this module is all three:

1. **Injection.** README.md, CONTEXT.md and CONTRIBUTING.md belong at the repo
   root — that is where GitHub, `go doc` readers and every existing link expect
   them — but MkDocs only sees `docs_dir`. `on_files` reads them from the root
   and appends them as virtual pages (`File.generated`), so the site gets a
   Home, a Glossary and a Contributing page without a second copy on disk that
   would immediately start drifting.

2. **Generation.** `adr/index.md` is built from a listing of `docs/adr/`. The
   point is that adding an ADR is one `git add` and nothing else: no nav entry,
   no index to hand-edit, no chance of the two disagreeing.

3. **Link rewriting.** The Markdown in this repo is written to read on GitHub —
   relative paths into `docs/`, into `internal/`, into `.github/workflows/`.
   Most of those files are not on the site at all. `on_page_markdown` resolves
   every relative target against the *repo*, then sends it to whichever of the
   two worlds actually holds it: a site page, or the file on GitHub.

The rewriting has one subtlety worth stating before the code says it.
Markdown links and raw-HTML attributes need paths relative to *different*
things:

* `[text](target)` — MkDocs' own tree processor rewrites these after us, and it
  works in `src_uri` space (`getting-started.md`, `adr/0001-….md`). So we emit
  paths relative to the page's **source** location and let MkDocs finish the
  job. That is deliberate: MkDocs validates what it resolves, so with
  `validation.unrecognized_links: warn` and `--strict`, every path this module
  emits is checked by the build. The hook cannot silently rot.

* `<img src=…>` / `<a href=…>` — MkDocs never touches raw HTML; whatever we
  write ships verbatim and the **browser** resolves it against the page's URL.
  With `use_directory_urls` those differ: README.md is `index.md` in source
  space but `/` in URL space, and `docs/assets/start.png` is `assets/start.png`
  in both. So HTML attributes get a **URL**-relative path, computed from the
  target File's own `.url` — the same helper MkDocs uses internally.

Whether a target is "published" is read off the live `Files` collection rather
than a list kept here. That is what makes `exclude_docs` in mkdocs.yml and this
module agree by construction: excluding a page there turns every link to it
into a GitHub URL here, with nothing to keep in sync.

Targets that do not resolve to anything on disk are left exactly as written.
There are no broken relative links in the content today, so anything that fails
to resolve is new — and leaving it alone means MkDocs reports it as a broken
link (a build failure under `--strict`) instead of this module quietly minting
a GitHub URL for a typo.

Python 3.14 stdlib plus MkDocs itself; no dependency ever gets added here.
"""

from __future__ import annotations

import logging
import posixpath
import re
from collections.abc import Iterator
from pathlib import Path
from typing import NamedTuple

from mkdocs.config.defaults import MkDocsConfig
from mkdocs.structure.files import File, Files
from mkdocs.structure.pages import Page

log = logging.getLogger(f"mkdocs.hooks.{__name__}")

# Repo-root file -> site src_uri. These pages have no file under docs_dir; the
# mapping is the only record of where each one came from, and the link rewriter
# reads it backwards to resolve a page's own relative links against the
# directory the Markdown was actually written in (the repo root, not docs/).
INJECTED: dict[str, str] = {
    "README.md": "index.md",
    "CONTEXT.md": "context.md",
    "CONTRIBUTING.md": "contributing.md",
}
_ORIGIN: dict[str, str] = {dest: src for src, dest in INJECTED.items()}

# Directory of ADRs, relative to docs_dir, and its generated index page.
_ADR_DIR = "adr"
_ADR_INDEX = f"{_ADR_DIR}/index.md"
# Every ADR filename is `<number>-<slug>.md`. The number is taken verbatim from
# the filename: the log contains duplicates (two 0033, two 0034, two 0060) from
# concurrent branches, and renumbering them here would make the index disagree
# with every cross-reference in the ADR texts themselves.
_ADR_FILE = re.compile(r"^(\d+)-.+\.md$")

# The branch a "view this on GitHub" link should point at. `main` is what the
# repo's default branch is; the host and path come from `repo_url` so there is
# one place that knows the forge.
_GITHUB_BRANCH = "main"

# `scheme:` — http, https, mailto, anything else absolute. Left untouched.
_ABSOLUTE = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")

# `[text](target)` and `![alt](target)`, with one level of nested brackets in
# the text (enough for ``[`code`](…)`` and for footnote-ish labels). The target
# group deliberately excludes whitespace and parentheses: no link in this repo
# has a title or a parenthesised URL, and a target that did would simply fail to
# match and be left alone rather than be mangled.
_MD_LINK = re.compile(r"(!?\[(?:[^\[\]]|\[[^\[\]]*\])*\]\()([^()\s]*)(\))")

# Raw-HTML `src=`/`href=` on the tags that can carry a repo path. Scoped to
# inside a tag so it cannot fire on prose that happens to contain `href=`.
_HTML_ATTR = re.compile(
    r"""(<(?:img|a)\s[^>]*?\b(?:src|href)\s*=\s*["'])([^"']*)(["'])""",
    re.IGNORECASE,
)

# An opening or closing code fence, per CommonMark: three or more backticks or
# tildes, up to three leading spaces.
_FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")


class _Published(NamedTuple):
    """Target is a page or asset in this build. Formatting differs per regime."""

    file: File
    fragment: str


class _External(NamedTuple):
    """Target is in the repo but not on the site: send readers to the forge."""

    url: str
    fragment: str


# --------------------------------------------------------------------------
# Repo layout


def _layout(config: MkDocsConfig) -> tuple[Path, str]:
    """Return (repo root, docs_dir as a repo-relative posix prefix).

    Derived from `docs_dir` rather than from `__file__` or a constant: the hook
    is loaded relative to mkdocs.yml, and that is the only thing that reliably
    locates the checkout in a CI runner, a nix build, or a worktree.
    """
    docs_dir = Path(config.docs_dir).resolve()
    root = docs_dir.parent
    return root, docs_dir.relative_to(root).as_posix()


def _github_url(repo_url: str, kind: str, repo_path: str) -> str:
    """A `blob` (file) or `tree` (directory) URL into the repo on the forge."""
    return f"{repo_url.rstrip('/')}/{kind}/{_GITHUB_BRANCH}/{repo_path}"


# --------------------------------------------------------------------------
# 1 + 2: inject the root files, generate the ADR index


def on_files(files: Files, config: MkDocsConfig) -> Files:
    root, _ = _layout(config)

    for origin, dest_uri in INJECTED.items():
        # Read now, once. `File.generated` snapshots the content, which is why
        # `on_serve` below has to teach the dev server that these files exist.
        files.append(
            File.generated(
                config,
                dest_uri,
                content=(root / origin).read_text(encoding="utf-8"),
            )
        )

    files.append(File.generated(config, _ADR_INDEX, content=_adr_index(config)))
    return files


def _adr_index(config: MkDocsConfig) -> str:
    """Build the ADR index from the directory, sorted by filename.

    Filename order is chronological here (the number is the prefix) and is also
    stable under the duplicate numbers in the log, where a numeric sort would
    have to invent a tie-break.
    """
    adr_dir = Path(config.docs_dir) / _ADR_DIR
    lines = [
        "# Decisions",
        "",
        "Every significant design decision in lab is recorded as an ADR — one"
        " decision per file, in the order it was taken, and kept in place when a"
        " later ADR supersedes it. This list is generated from `docs/adr/` at"
        " build time, so a new ADR appears here as soon as its file lands.",
        "",
    ]

    for path in sorted(adr_dir.iterdir(), key=lambda p: p.name):
        match = _ADR_FILE.match(path.name)
        if match is None:
            continue
        lines.append(f"- [ADR-{match.group(1)} — {_title(path)}]({path.name})")

    return "\n".join(lines) + "\n"


def _title(path: Path) -> str:
    """The ADR's own H1, which every ADR carries on its first line."""
    with path.open(encoding="utf-8") as handle:
        for line in handle:
            if line.startswith("# "):
                return line[2:].strip()
    # Not a build failure: an ADR with no heading is a content bug, and a usable
    # index with one odd entry beats a site that will not build.
    log.info("ADR %s has no H1; falling back to its filename", path.name)
    return path.stem


# --------------------------------------------------------------------------
# 3: link rewriting


def on_page_markdown(
    markdown: str, page: Page, config: MkDocsConfig, files: Files
) -> str:
    root, docs_prefix = _layout(config)
    src_uri = page.file.src_uri
    # Where this page's Markdown physically lives in the repo — the directory
    # its relative links were written against. For injected pages that is the
    # repo root; for everything else it is under docs/.
    source = _ORIGIN.get(src_uri) or f"{docs_prefix}/{src_uri}"
    source_dir = posixpath.dirname(source)
    # `relpath` wants `.`, not `''`, for a page sitting at the site root.
    page_dir = posixpath.dirname(src_uri) or "."

    def resolve(raw: str) -> _Published | _External | None:
        return _classify(raw, source_dir, root, docs_prefix, files, config.repo_url)

    def markdown_link(match: re.Match[str]) -> str:
        target = resolve(match.group(2))
        if target is None:
            return match.group(0)
        if isinstance(target, _External):
            value = target.url
        else:
            # Source-relative: MkDocs resolves and validates it from here.
            value = posixpath.relpath(target.file.src_uri, page_dir)
        return f"{match.group(1)}{value}{_frag(target)}{match.group(3)}"

    def html_attr(match: re.Match[str]) -> str:
        target = resolve(match.group(2))
        if target is None:
            return match.group(0)
        if isinstance(target, _External):
            value = target.url
        else:
            # URL-relative: nothing downstream will touch this, and the browser
            # resolves it against the page's URL, not its source path.
            value = target.file.url_relative_to(page.file)
        return f"{match.group(1)}{value}{_frag(target)}{match.group(3)}"

    out: list[str] = []
    for chunk, fenced in _split_fences(markdown):
        if not fenced:
            chunk = _MD_LINK.sub(markdown_link, chunk)
            chunk = _HTML_ATTR.sub(html_attr, chunk)
        out.append(chunk)
    return "".join(out)


def _frag(target: _Published | _External) -> str:
    return f"#{target.fragment}" if target.fragment else ""


def _classify(
    raw: str,
    source_dir: str,
    root: Path,
    docs_prefix: str,
    files: Files,
    repo_url: str,
) -> _Published | _External | None:
    """Decide what a single link target should become. `None` = leave as is."""
    # Absolute URLs, pure in-page anchors and site-root-absolute paths all
    # already mean the same thing on GitHub and here.
    if not raw or raw.startswith(("#", "/")) or _ABSOLUTE.match(raw):
        return None

    path, _, fragment = raw.partition("#")
    if not path:
        return None

    repo_path = posixpath.normpath(posixpath.join(source_dir, path))
    if repo_path == ".." or repo_path.startswith("../"):
        return None  # points outside the checkout; not ours to redirect

    on_disk = root / repo_path
    if path.endswith("/") or on_disk.is_dir():
        if repo_path == f"{docs_prefix}/{_ADR_DIR}":
            # The one directory that has a site page: the generated index.
            index = files.get_file_from_path(_ADR_INDEX)
            if index is not None:
                return _Published(index, fragment)
        if not on_disk.is_dir():
            return None
        return _External(_github_url(repo_url, "tree", repo_path), fragment)

    if not on_disk.exists():
        return None

    site_uri = _site_uri(repo_path, docs_prefix)
    if site_uri is not None:
        target = files.get_file_from_path(site_uri)
        # `is_included` and not mere presence: `exclude_docs` leaves excluded
        # files in the collection, flagged. Treating them as published would
        # emit a link MkDocs then reports as pointing at nothing.
        if target is not None and target.inclusion.is_included():
            return _Published(target, fragment)

    return _External(_github_url(repo_url, "blob", repo_path), fragment)


def _site_uri(repo_path: str, docs_prefix: str) -> str | None:
    """Repo path -> the src_uri it would have on the site, if it can have one."""
    if repo_path in INJECTED:
        return INJECTED[repo_path]
    prefix = f"{docs_prefix}/"
    if repo_path.startswith(prefix):
        return repo_path[len(prefix) :]
    return None


def _split_fences(markdown: str) -> Iterator[tuple[str, bool]]:
    """Yield (chunk, is_fenced) so callers can skip fenced code blocks.

    Code samples in this repo contain shell comments starting with `#` and
    Markdown examples with link syntax in them; rewriting inside a fence would
    corrupt documented commands. An unterminated fence at EOF is reported as
    fenced, which errs toward leaving text alone.
    """
    buffer: list[str] = []
    fence: str | None = None

    for line in markdown.splitlines(keepends=True):
        match = _FENCE.match(line)
        if fence is None:
            if match is None:
                buffer.append(line)
                continue
            yield "".join(buffer), False
            buffer, fence = [line], match.group(1)
        else:
            buffer.append(line)
            # A closing fence matches the opener's character and is at least as
            # long, so a ``` block can quote a shorter fence without ending.
            if match is not None and match.group(1).startswith(fence):
                yield "".join(buffer), True
                buffer, fence = [], None

    yield "".join(buffer), fence is not None


# --------------------------------------------------------------------------
# 4: dev server


def on_serve(server, config: MkDocsConfig, builder):
    """Watch the injected root files; MkDocs only watches `docs_dir` itself."""
    root, _ = _layout(config)
    for origin in INJECTED:
        server.watch(str(root / origin))
    return server
