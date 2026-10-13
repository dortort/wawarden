"""MkDocs hooks for the WaWarden documentation site.

The Markdown in this repository is written for GitHub first. These hooks make the
same files render on the site without changing them:

- The root files (README, CONTRIBUTING, CHANGELOG, RELEASING, SECURITY) join the
  site under project/, and every relative link is resolved the way GitHub
  resolves it, then pointed at the page it names on the site. A link to a file
  that is not a page or an asset of the site, such as LICENSE or a Go source
  file, points at that file on GitHub.
- List items indented the GitHub way (content under the marker, so 2 spaces for
  "- " and 3 for "1. ") are re-indented to the 4 spaces Python-Markdown needs,
  so that fenced code and nested lists stay inside their item.
- GitHub alerts ("> [!NOTE]") become admonitions.
- The home page's first heading becomes the logo banner.
"""

from __future__ import annotations

import os
import posixpath
import re

from mkdocs.structure.files import File, Files

REPO_BLOB = "https://github.com/dortort/wawarden/blob/main/"
REPO_TREE = "https://github.com/dortort/wawarden/tree/main/"

# Root file -> its page on the site, relative to docs_dir.
ROOT_PAGES = {
    "README.md": "project/readme.md",
    "CONTRIBUTING.md": "project/contributing.md",
    "CHANGELOG.md": "project/changelog.md",
    "RELEASING.md": "project/releasing.md",
    "SECURITY.md": "project/security-policy.md",
}
SITE_TO_ROOT = {site: root for root, site in ROOT_PAGES.items()}

FENCE = re.compile(r"^(\s*)(`{3,}|~{3,})")
LIST_MARKER = re.compile(r"^( *)([-*+]|\d{1,9}[.)])( +|$)")
SCHEME = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]*:")
INLINE_LINK = re.compile(r"(!?\[(?:[^\[\]]|\[[^\[\]]*\])*\]\()(<[^>]*>|[^\s)]+)((?:\s+\"[^\"]*\"|\s+'[^']*')?\))")
REF_DEF = re.compile(r"^( {0,3}\[[^\]]+\]:[ \t]*)(<[^>]*>|\S+)(.*)$", re.MULTILINE)
HTML_ATTR = re.compile(r"""(\b(?:href|src|srcset)=)(["'])([^"']*)\2""")
CODE_SPAN = re.compile(r"(`+)(?:(?!\1)[\s\S])+?\1")
ALERT = re.compile(r"^ {0,3}> ?\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*$", re.IGNORECASE)
ALERT_KIND = {"note": "note", "tip": "tip", "important": "info", "warning": "warning", "caution": "danger"}


def _root_dir(config) -> str:
    return os.path.dirname(os.path.abspath(config["config_file_path"]))


def on_files(files: Files, config) -> Files:
    root = _root_dir(config)
    for name, site_path in ROOT_PAGES.items():
        path = os.path.join(root, name)
        if not os.path.isfile(path):
            continue  # the nav entry then fails the strict build
        with open(path, encoding="utf-8") as f:
            content = f.read()
        files.append(File.generated(config, site_path, content=content))
    return files


def on_serve(server, config, builder):
    root = _root_dir(config)
    for name in ROOT_PAGES:
        path = os.path.join(root, name)
        if os.path.isfile(path):
            server.watch(path)
    return server


def _repo_path(page) -> str:
    src = page.file.src_uri
    return SITE_TO_ROOT.get(src, "docs/" + src)


def _rewrite_target(target: str, page, files: Files, root: str, html: bool) -> str:
    raw = target
    wrapped = target.startswith("<") and target.endswith(">")
    if wrapped:
        target = target[1:-1]
    if not target or target.startswith(("#", "/")) or SCHEME.match(target):
        return raw
    path, sep, fragment = target.partition("#")
    path, qsep, query = path.partition("?")
    repo_dir = posixpath.dirname(_repo_path(page))
    resolved = posixpath.normpath(posixpath.join(repo_dir, path))
    if resolved.startswith("../") or resolved == "..":
        return raw  # outside the repository: leave it for the strict build to report
    if resolved in ROOT_PAGES:
        site_path = ROOT_PAGES[resolved]
    elif resolved == "docs":
        site_path = "index.md"
    elif resolved.startswith("docs/"):
        site_path = resolved[len("docs/"):]
        if not files.get_file_from_path(site_path):
            return raw  # a missing page: leave it for the strict build to report
    elif not os.path.exists(os.path.join(root, resolved)):
        return raw
    else:
        base = REPO_TREE if os.path.isdir(os.path.join(root, resolved)) else REPO_BLOB
        new = base + ("" if resolved == "." else resolved) + (qsep + query) + (sep + fragment)
        return f"<{new}>" if wrapped else new
    if html:
        # Raw HTML is not rewritten by MkDocs: make it relative to the page's URL.
        target_url = files.get_file_from_path(site_path).url
        page_dir = page.url if page.url.endswith("/") or not page.url else posixpath.dirname(page.url) + "/"
        new = posixpath.relpath(target_url or ".", page_dir or ".")
        if target_url.endswith("/") and not new.endswith("/"):
            new += "/"
    else:
        new = posixpath.relpath(site_path, posixpath.dirname(page.file.src_uri) or ".")
    new += (qsep + query) + (sep + fragment)
    return f"<{new}>" if wrapped else new


def _rewrite_paragraph(text: str, page, files: Files, root: str) -> str:
    """Rewrite the links in one paragraph (no blank lines, no fenced code)."""

    def outside_code(part: str) -> str:  # part has its code spans hidden
        part = REF_DEF.sub(lambda m: m.group(1) + _rewrite_target(m.group(2), page, files, root, False) + m.group(3), part)
        part = INLINE_LINK.sub(lambda m: m.group(1) + _rewrite_target(m.group(2), page, files, root, False) + m.group(3), part)
        return HTML_ATTR.sub(lambda m: m.group(1) + m.group(2) + _rewrite_target(m.group(3), page, files, root, True) + m.group(2), part)

    # Hide code spans behind placeholders, so that a link inside one is left
    # alone while a link whose text is a code span is still rewritten.
    spans: list[str] = []

    def hide(m: re.Match) -> str:
        spans.append(m.group(0))
        return f"\x00{len(spans) - 1}\x00"

    masked = outside_code(CODE_SPAN.sub(hide, text))
    return re.sub(r"\x00(\d+)\x00", lambda m: spans[int(m.group(1))], masked)


def _reindent_lists(lines: list[str]) -> list[str]:
    """Re-indent GitHub-style list item content to multiples of 4 spaces."""
    out: list[str] = []
    stack: list[tuple[int, int]] = []  # (original content column, new content column)
    fence: tuple[str, int] | None = None  # (fence chars, shift applied to the fence's lines)
    blank = False
    for line in lines:
        if fence is not None:
            shift = fence[1]
            indent = len(line) - len(line.lstrip(" "))
            out.append(_shift(line, shift, indent))
            if line.strip().startswith(fence[0]) and set(line.strip()) == {fence[0][0]}:
                fence = None
                if stack:
                    out.append("")  # and a blank line after it
            blank = False
            continue
        if not line.strip():
            out.append(line)
            blank = True
            continue
        indent = len(line) - len(line.lstrip(" "))
        marker = LIST_MARKER.match(line)
        # Close the items this line is not part of.
        if not stack and indent >= 4:
            marker = None  # an indented code block, not a list
        popped = 0
        while stack and indent < stack[-1][0]:
            if marker or blank or line.lstrip().startswith(("#", "|", ">", "```", "~~~")):
                stack.pop()
                popped += 1
            else:
                break  # a lazy continuation line of the open paragraph
        if marker and popped > 1 and out and out[-1].strip():
            out.append("")  # back out of a nested list: Python-Markdown would read a continuation line
        if not stack and not marker:
            out.append(line)
            blank = False
            f = FENCE.match(line)
            if f:
                fence = (f.group(2), 0)
            continue
        shift = (stack[-1][1] - stack[-1][0]) if stack else 0
        if marker:
            new_indent = indent + shift
            if not stack:
                new_indent = indent  # a top-level list keeps its own indent (0 to 3)
            width = len(marker.group(2)) + max(1, len(marker.group(3)))
            stack.append((indent + width, new_indent + 4))
            out.append(" " * new_indent + line[indent:])
        else:
            f = FENCE.match(line)
            if f:
                if out and out[-1].strip():
                    out.append("")  # Python-Markdown needs a blank line before a fence in a list
                fence = (f.group(2), shift)
            out.append(_shift(line, shift, indent))
        blank = False
    return out


def _shift(line: str, shift: int, indent: int) -> str:
    if shift >= 0:
        return " " * shift + line
    return line[min(-shift, indent):]


def _alerts(lines: list[str]) -> list[str]:
    out: list[str] = []
    i = 0
    while i < len(lines):
        m = ALERT.match(lines[i])
        if not m:
            out.append(lines[i])
            i += 1
            continue
        out.append(f'!!! {ALERT_KIND[m.group(1).lower()]}')
        out.append("")
        i += 1
        while i < len(lines) and lines[i].lstrip().startswith(">"):
            body = re.sub(r"^ {0,3}> ?", "", lines[i])
            out.append(("    " + body) if body.strip() else "")
            i += 1
        out.append("")
    return out


def on_page_markdown(markdown: str, page, config, files: Files) -> str:
    root = _root_dir(config)
    lines = markdown.split("\n")
    lines = _reindent_lists(lines)
    rewritten: list[str] = []
    paragraph: list[str] = []
    in_fence: str | None = None

    def flush():
        if paragraph:
            rewritten.extend(_rewrite_paragraph("\n".join(paragraph), page, files, root).split("\n"))
            paragraph.clear()

    for line in lines:
        f = FENCE.match(line)
        if in_fence:
            rewritten.append(line)
            if f and f.group(2).startswith(in_fence) and not line.strip()[len(f.group(2)):].strip():
                in_fence = None
            continue
        if f:
            flush()
            in_fence = f.group(2)
            rewritten.append(line)
        elif not line.strip():
            flush()
            rewritten.append(line)
        else:
            paragraph.append(line)
    flush()
    lines = rewritten
    lines = _alerts(lines)
    if page.file.src_uri == "index.md":
        for i, line in enumerate(lines):
            if line.startswith("# "):
                title = line[2:].strip()
                lines[i] = (
                    f'<h1 class="ww-hero" id="wawarden"><img src="assets/hero-light.svg#only-light" alt="{title}">'
                    f'<img src="assets/hero-dark.svg#only-dark" alt="{title}"></h1>'
                )
                break
    return "\n".join(lines)


def on_page_context(context, page, config, nav):
    src = page.file.src_uri
    if src in SITE_TO_ROOT:
        page.edit_url = "https://github.com/dortort/wawarden/edit/main/" + SITE_TO_ROOT[src]
    return context
