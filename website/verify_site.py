from __future__ import annotations

import argparse
import hashlib
import html
import json
import pathlib
import re
import sys
from urllib.parse import urlsplit

import yaml

AGENT_SKILLS_SCHEMA = "https://schemas.agentskills.io/discovery/0.2.0/schema.json"
META_DESC_RE = re.compile(r'<meta\s+name="description"\s+content="([^"]*)"')
ALT_MD_RE = re.compile(r'<link\s+rel="alternate"\s+type="text/markdown"\s+href="([^"]+)"')
MD_LINK_RE = re.compile(r"!?\[(?:[^\[\]]|\[[^\[\]]*\])*\]\((<[^>]*>|[^\s)]+)(?:\s+\"[^\"]*\"|\s+'[^']*')?\)")
FENCE_RE = re.compile(r"^\s*(`{3,}|~{3,})")
CODE_SPAN_RE = re.compile(r"(`+)(?:(?!\1)[\s\S])+?\1")
SCHEME_RE = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]*:")
SHA256_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
REQUIRED_CRAWLERS = ("GPTBot", "OAI-SearchBot", "ClaudeBot", "Claude-SearchBot", "Claude-User", "PerplexityBot", "Google-Extended")


def read_mkdocs_scalar(config_path: pathlib.Path, key: str) -> str:
    prefix = f"{key}:"
    for line in config_path.read_text(encoding="utf-8").splitlines():
        if line.startswith(prefix):
            return line[len(prefix):].strip().strip("\"'")
    raise SystemExit(f"{config_path}: missing {key}")


def resolve_site_url(url: str, site_base: str, site_dir: pathlib.Path, errors: list[str], context: str) -> pathlib.Path | None:
    if not url.startswith(site_base + "/"):
        errors.append(f"{context}: URL {url!r} does not start with {site_base + '/'!r}")
        return None
    rel = urlsplit(url[len(site_base) + 1 :]).path
    if not rel or rel.endswith("/"):
        errors.append(f"{context}: URL {url!r} must point to a file")
        return None
    target = (site_dir / rel).resolve()
    site_root = site_dir.resolve()
    try:
        target.relative_to(site_root)
    except ValueError:
        errors.append(f"{context}: URL {url!r} escapes site directory")
        return None
    if not target.is_file() or target.stat().st_size == 0:
        errors.append(f"{context}: URL {url!r} does not resolve to a non-empty built file ({target})")
        return None
    return target


def verify_robots(site_dir: pathlib.Path, site_base: str, errors: list[str]) -> None:
    robots = site_dir / "robots.txt"
    if not robots.is_file() or robots.stat().st_size == 0:
        errors.append("robots.txt is missing or empty")
        return
    text = robots.read_text(encoding="utf-8")
    for crawler in REQUIRED_CRAWLERS:
        if f"User-agent: {crawler}" not in text:
            errors.append(f"robots.txt missing User-agent: {crawler}")
    if "Content-Signal: search=yes, ai-input=yes, ai-train=yes" not in text:
        errors.append("robots.txt missing expected Content-Signal directive")
    if f"Sitemap: {site_base}/sitemap.xml" not in text:
        errors.append(f"robots.txt missing Sitemap: {site_base}/sitemap.xml")


def verify_markdown_links(md_path: pathlib.Path, site_dir: pathlib.Path, errors: list[str]) -> None:
    in_fence: str | None = None
    rel = md_path.relative_to(site_dir).as_posix()
    for lineno, line in enumerate(md_path.read_text(encoding="utf-8").splitlines(), 1):
        fence = FENCE_RE.match(line)
        if in_fence:
            if fence and fence.group(1).startswith(in_fence) and not line.strip()[len(fence.group(1)):].strip():
                in_fence = None
            continue
        if fence:
            in_fence = fence.group(1)
            continue
        cleaned = CODE_SPAN_RE.sub("", line)
        for match in MD_LINK_RE.finditer(cleaned):
            target = match.group(1)
            if target.startswith("<") and target.endswith(">"):
                target = target[1:-1]
            if not target or target.startswith("#") or SCHEME_RE.match(target):
                continue
            errors.append(f"{rel}:{lineno}: non-absolute link {target!r}")


def verify_pages(site_dir: pathlib.Path, site_base: str, site_description: str, errors: list[str]) -> set[str]:
    html_pages = sorted(site_dir.rglob("index.html"))
    if not html_pages:
        errors.append("no built index.html pages found")
        return set()
    seen_descriptions: dict[str, str] = {}
    expected_md_urls: set[str] = set()
    for html_path in html_pages:
        rel_html = html_path.relative_to(site_dir).as_posix()
        md_path = html_path.with_name("index.md")
        rel_md = md_path.relative_to(site_dir).as_posix()
        md_url = f"{site_base}/{rel_md}"
        expected_md_urls.add(md_url)
        if not md_path.is_file() or md_path.stat().st_size == 0:
            errors.append(f"missing or empty Markdown copy {rel_md} for {rel_html}")
        else:
            verify_markdown_links(md_path, site_dir, errors)
        page_html = html_path.read_text(encoding="utf-8")
        head = page_html.split("</head>", 1)[0]
        alt = ALT_MD_RE.search(head)
        if not alt:
            errors.append(f"{rel_html}: missing <link rel=\"alternate\" type=\"text/markdown\"> in <head>")
        elif html.unescape(alt.group(1)) != md_url:
            errors.append(f"{rel_html}: alternate markdown href {alt.group(1)!r} != {md_url!r}")
        meta = META_DESC_RE.search(head)
        if not meta or not meta.group(1).strip():
            errors.append(f"{rel_html}: missing or empty meta description")
        else:
            desc = html.unescape(meta.group(1)).strip()
            if desc == site_description:
                errors.append(f"{rel_html}: still uses site-wide fallback description")
            elif desc in seen_descriptions:
                errors.append(f"{rel_html}: duplicates meta description of {seen_descriptions[desc]}")
            else:
                seen_descriptions[desc] = rel_html
    return expected_md_urls


def verify_llms_txt(site_dir: pathlib.Path, site_base: str, expected_md_urls: set[str], errors: list[str]) -> None:
    llms_path = site_dir / "llms.txt"
    if not llms_path.is_file() or llms_path.stat().st_size == 0:
        errors.append("llms.txt is missing or empty")
        return
    text = llms_path.read_text(encoding="utf-8")
    found_urls: set[str] = set()
    for match in MD_LINK_RE.finditer(text):
        url = match.group(1)
        if url.startswith("<") and url.endswith(">"):
            url = url[1:-1]
        found_urls.add(url)
        resolve_site_url(url, site_base, site_dir, errors, "llms.txt")
    if not found_urls:
        errors.append("llms.txt contains no Markdown links")
    missing = sorted(expected_md_urls - found_urls)
    for url in missing:
        errors.append(f"llms.txt missing link to {url}")


def parse_frontmatter(text: str, label: str, errors: list[str]) -> dict:
    if not text.startswith("---\n"):
        errors.append(f"{label}: missing YAML frontmatter")
        return {}
    end = text.find("\n---\n", 4)
    if end == -1:
        errors.append(f"{label}: unterminated YAML frontmatter")
        return {}
    meta = yaml.safe_load(text[4:end])
    if not isinstance(meta, dict):
        errors.append(f"{label}: frontmatter is not a YAML mapping")
        return {}
    return meta


def verify_agent_skills(site_dir: pathlib.Path, site_base: str, errors: list[str]) -> None:
    index_path = site_dir / ".well-known" / "agent-skills" / "index.json"
    if not index_path.is_file() or index_path.stat().st_size == 0:
        errors.append(".well-known/agent-skills/index.json is missing or empty")
        return
    try:
        doc = json.loads(index_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        errors.append(f".well-known/agent-skills/index.json: invalid JSON ({exc})")
        return
    if doc.get("$schema") != AGENT_SKILLS_SCHEMA:
        errors.append(f"agent-skills index $schema {doc.get('$schema')!r} != {AGENT_SKILLS_SCHEMA!r}")
    skills = doc.get("skills")
    if not isinstance(skills, list) or not skills:
        errors.append("agent-skills index has no skills")
        return
    for i, skill in enumerate(skills):
        ctx = f"agent-skills index skills[{i}]"
        if not isinstance(skill, dict):
            errors.append(f"{ctx}: entry must be an object")
            continue
        for field in ("name", "type", "description", "url", "digest"):
            val = skill.get(field)
            if not isinstance(val, str) or not val.strip():
                errors.append(f"{ctx}: missing or empty {field!r}")
        url = skill.get("url", "")
        digest = skill.get("digest", "")
        if not isinstance(digest, str) or not SHA256_DIGEST_RE.match(digest):
            errors.append(f"{ctx}: invalid sha256 digest {digest!r}")
        target = resolve_site_url(url, site_base, site_dir, errors, ctx) if isinstance(url, str) else None
        if target is not None:
            actual_bytes = target.read_bytes()
            actual_digest = "sha256:" + hashlib.sha256(actual_bytes).hexdigest()
            if digest != actual_digest:
                errors.append(f"{ctx}: digest mismatch (index={digest}, actual={actual_digest})")
            if skill.get("type") == "skill-md":
                meta = parse_frontmatter(actual_bytes.decode("utf-8"), str(target.relative_to(site_dir)), errors)
                if meta.get("name") != skill.get("name"):
                    errors.append(f"{ctx}: frontmatter name {meta.get('name')!r} != {skill.get('name')!r}")
                if meta.get("description") != skill.get("description"):
                    errors.append(f"{ctx}: frontmatter description does not match index.json")


def main() -> int:
    parser = argparse.ArgumentParser(description="Verify built MkDocs site AI-readiness outputs.")
    parser.add_argument("--site-dir", default="site", type=pathlib.Path)
    parser.add_argument("--config", default="mkdocs.yml", type=pathlib.Path)
    args = parser.parse_args()

    site_dir = args.site_dir.resolve()
    site_base = read_mkdocs_scalar(args.config, "site_url").rstrip("/")
    site_description = read_mkdocs_scalar(args.config, "site_description")

    errors: list[str] = []
    verify_robots(site_dir, site_base, errors)
    expected_md_urls = verify_pages(site_dir, site_base, site_description, errors)
    verify_llms_txt(site_dir, site_base, expected_md_urls, errors)
    verify_agent_skills(site_dir, site_base, errors)

    if errors:
        for err in errors:
            print(f"ERROR: {err}", file=sys.stderr)
        return 1
    print(f"Verified {len(expected_md_urls)} pages, robots.txt, llms.txt, and .well-known/agent-skills/index.json.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
