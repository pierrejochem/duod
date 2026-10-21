#!/usr/bin/env python3
"""Build the duod documentation site (static HTML, no dependencies).

    python3 docs-src/build.py                       # production URLs -> docs/
    SITE_URL=http://localhost:8000 BASE_PATH= python3 docs-src/build.py   # local preview

Page bodies live in docs-src/pages/<slug>.html. This script wraps them in the
shared layout and writes SEO metadata, sitemap.xml, robots.txt and 404.html.
"""
import html
import json
import os
import re
import shutil
from datetime import date
from pathlib import Path

ROOT = Path(__file__).resolve().parent
OUT = ROOT.parent / "docs"
ASSETS = ROOT.parent / "assets"

SITE_URL = os.environ.get("SITE_URL", "https://pierrejochem.github.io").rstrip("/")
BASE = os.environ.get("BASE_PATH", "/duod").rstrip("/")
REPO = "https://github.com/pierrejochem/duod"
SITE_NAME = "duod"
LASTMOD = os.environ.get("LASTMOD", date.today().isoformat())

# slug, nav title, <title> (SEO), meta description, group
PAGES = [
    ("", "Overview", "duod: GitOps blue/green deployments for Docker Compose and Traefik",
     "duod is an open-source Go operator that brings the Argo CD and Argo Rollouts blue/green workflow to a plain Docker host. Git is the source of truth, Docker Compose runs the colors, Traefik switches the traffic.",
     "Start"),
    ("getting-started", "Try the demo", "Getting started: run the duod demo in five minutes",
     "Run the duod demo with one command: registry, Traefik, duod, three services and a live dashboard. Roll out a release and watch a zero-downtime blue/green switch.",
     "Start"),
    ("installation", "Install duod", "Install duod on a Docker host: deb, rpm, binary or Docker",
     "Install duod from a deb or rpm package, a release tarball, or source. Set up Traefik, the shared Docker network, the systemd service and the API token.",
     "Start"),
    ("concepts", "How it works", "How duod works: rollout lifecycle, slots and traffic switching",
     "Learn how duod rolls out a Git commit with blue/green slots: detect, preview, wiring check, health analysis, atomic Traefik switch, post-promotion analysis, scale down and rollback.",
     "Guide"),
    ("configuration", "Configuration reference", "duod.yaml configuration reference: every option and default",
     "Complete reference for duod.yaml: global settings, Traefik, apps, source, health checks, strategy, post-promotion analysis and routes, with defaults.",
     "Guide"),
    ("compose-files", "Write a service", "Write a Docker Compose file for blue/green deployment with duod",
     "Rules for a service compose file that duod runs twice as blue and green: networks, ports, replicas, stateful services, database migrations and background workers.",
     "Guide"),
    ("webhooks", "Webhooks and Git", "duod webhooks: GitHub, Gitea, Forgejo and GitLab push triggers",
     "Trigger duod rollouts from a push webhook. Covers GitHub, Gitea, Forgejo and GitLab signature checks, per-app and global endpoints, and monorepo path scoping.",
     "Guide"),
    ("cli", "CLI and REST API", "duod CLI and REST API reference: promote, rollback, abort, retry",
     "Reference for the duod command line and REST API: list, get, history, sync, promote, abort, rollback, retry, validate, plus Prometheus metrics.",
     "Reference"),
    ("apache", "Apache instead of Traefik", "Use Apache instead of Traefik for blue/green switching with duod",
     "Switch traffic with Apache on the host: loopback ports per slot, an owned include file, apachectl configtest and a graceful reload.",
     "Reference"),
    ("argo-comparison", "duod vs Argo", "duod vs Argo CD and Argo Rollouts: feature mapping for Docker",
     "Map Argo CD and Argo Rollouts concepts to duod: sync, blueGreen strategy, analysis, promote, abort, undo and self-heal on a plain Docker host.",
     "Reference"),
    ("troubleshooting", "Troubleshooting", "duod troubleshooting: failed rollouts, Traefik 400 errors, health checks",
     "Fix common duod problems: Traefik Docker provider 400 errors on Docker Engine 29, missing duod-edge network, failed health checks, stuck rollouts and Apache configtest failures.",
     "Reference"),
    ("faq", "FAQ", "duod FAQ: Kubernetes, canary, databases, multiple hosts",
     "Answers to common questions about duod: does it need Kubernetes, canary releases, database migrations, several hosts, rollbacks and image builds.",
     "Reference"),
]

FAQ = [
    ("Does duod need Kubernetes?",
     "No. duod runs on a single plain Docker host with Docker Compose v2 and Traefik. It copies the Argo CD and Argo Rollouts blue/green workflow, but nothing in it needs a Kubernetes cluster."),
    ("Does duod support canary releases?",
     "Not yet. Traffic switching is all at once (blue/green). Canary weights would map onto Traefik weighted services and could be added later."),
    ("How do I roll back a release?",
     "Run duod rollback <app>. The previous slot is stopped but kept after a promotion, so duod only restarts it, health-checks it and switches traffic back. No Git change is needed."),
    ("Can duod manage several Docker hosts?",
     "One duod instance manages one Docker host. For several hosts, run one duod per host with the same Git repository."),
    ("How do database migrations work with blue/green?",
     "Both colors run during the switch, and the old color keeps running until it is scaled down. Keep migrations backward-compatible (expand, then contract) and run databases outside the slot, in a separate Compose project."),
    ("Does a change in one service redeploy the others?",
     "No. An app's target revision is the newest commit that touched its own source.path, so a commit to apps/api never redeploys apps/shop, even in the same repository and branch."),
]


def e(s):
    return html.escape(s, quote=True)


def url(slug):
    return f"{SITE_URL}{BASE}/{slug + '/' if slug else ''}"


def path(slug):
    return f"{BASE}/{slug + '/' if slug else ''}"


def nav_html(current):
    out, group = [], None
    for slug, title, _, _, g in PAGES:
        if g != group:
            if group:
                out.append("</ul>")
            out.append(f'<p class="nav-group">{e(g)}</p><ul>')
            group = g
        cur = ' aria-current="page"' if slug == current else ""
        out.append(f'<li><a href="{path(slug)}"{cur}>{e(title)}</a></li>')
    out.append("</ul>")
    return "\n".join(out)


def jsonld(obj):
    return '<script type="application/ld+json">' + json.dumps(obj, ensure_ascii=False) + "</script>"


def structured(idx, slug, title, desc, body):
    graph = []
    if slug == "":
        graph.append({
            "@context": "https://schema.org", "@type": "SoftwareApplication",
            "name": SITE_NAME, "applicationCategory": "DeveloperApplication",
            "operatingSystem": "Linux", "description": desc, "url": url(""),
            "license": "https://www.apache.org/licenses/LICENSE-2.0",
            "codeRepository": REPO, "programmingLanguage": "Go",
            "offers": {"@type": "Offer", "price": "0", "priceCurrency": "USD"},
        })
        graph.append({"@context": "https://schema.org", "@type": "WebSite",
                      "name": SITE_NAME, "url": url("")})
    else:
        graph.append({
            "@context": "https://schema.org", "@type": "TechArticle",
            "headline": title, "description": desc, "url": url(slug),
            "inLanguage": "en", "dateModified": LASTMOD,
            "image": f"{SITE_URL}{BASE}/assets/duod-logo.png",
            "author": {"@type": "Organization", "name": SITE_NAME, "url": REPO},
            "publisher": {"@type": "Organization", "name": SITE_NAME, "url": REPO},
            "mainEntityOfPage": url(slug),
        })
        graph.append({
            "@context": "https://schema.org", "@type": "BreadcrumbList",
            "itemListElement": [
                {"@type": "ListItem", "position": 1, "name": "duod", "item": url("")},
                {"@type": "ListItem", "position": 2, "name": PAGES[idx][1], "item": url(slug)},
            ],
        })
    if slug == "faq":
        graph.append({
            "@context": "https://schema.org", "@type": "FAQPage",
            "mainEntity": [
                {"@type": "Question", "name": q,
                 "acceptedAnswer": {"@type": "Answer", "text": a}} for q, a in FAQ
            ],
        })
    return "\n".join(jsonld(g) for g in graph)


def add_heading_anchors(body):
    """Give every h2/h3 an id and a self-link so sections can be deep-linked."""
    seen = set()

    def repl(m):
        tag, attrs, inner = m.group(1), m.group(2), m.group(3)
        if "id=" in attrs:
            return m.group(0)
        text = re.sub(r"<[^>]+>", "", inner)
        slug = re.sub(r"[^a-z0-9]+", "-", html.unescape(text).lower()).strip("-") or "section"
        base, n = slug, 2
        while slug in seen:
            slug, n = f"{base}-{n}", n + 1
        seen.add(slug)
        return f'<{tag}{attrs} id="{slug}">{inner} <a class="anchor" href="#{slug}" aria-label="Link to this section">#</a></{tag}>'

    return re.sub(r"<(h[23])([^>]*)>(.*?)</\1>", repl, body, flags=re.S)


def toc_html(body):
    items = re.findall(r'<h2[^>]* id="([^"]+)"[^>]*>(.*?) <a class="anchor"', body, flags=re.S)
    if len(items) < 3:
        return ""
    lis = "".join(f'<li><a href="#{i}">{t}</a></li>' for i, t in items)
    return f'<nav class="toc" aria-label="On this page"><p>On this page</p><ul>{lis}</ul></nav>'


def pager(idx):
    prev = PAGES[idx - 1] if idx > 0 else None
    nxt = PAGES[idx + 1] if idx < len(PAGES) - 1 else None
    out = '<nav class="pager" aria-label="Pagination">'
    out += f'<a rel="prev" href="{path(prev[0])}"><small>Previous</small>{e(prev[1])}</a>' if prev else "<span></span>"
    out += f'<a rel="next" href="{path(nxt[0])}"><small>Next</small>{e(nxt[1])}</a>' if nxt else "<span></span>"
    return out + "</nav>"


def layout(*, slug, title, desc, body, idx, noindex=False, canonical=None):
    canonical = canonical or url(slug)
    og_img = f"{SITE_URL}{BASE}/assets/duod-logo.png"
    robots = '<meta name="robots" content="noindex">' if noindex else '<meta name="robots" content="index,follow,max-image-preview:large">'
    ld = "" if noindex else structured(idx, slug, title, desc, body)
    crumbs = "" if slug == "" or noindex else (
        f'<p class="crumbs"><a href="{path("")}">duod</a> &rsaquo; {e(PAGES[idx][1])}</p>')
    toc = "" if noindex else toc_html(body)
    foot = "" if noindex else pager(idx)
    return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{e(title)}</title>
<meta name="description" content="{e(desc)}">
<link rel="canonical" href="{canonical}">
{robots}
<meta name="theme-color" content="#0B2545">
<link rel="icon" href="{BASE}/assets/favicon.svg" type="image/svg+xml">
<meta property="og:type" content="{'website' if slug == '' else 'article'}">
<meta property="og:site_name" content="{SITE_NAME}">
<meta property="og:title" content="{e(title)}">
<meta property="og:description" content="{e(desc)}">
<meta property="og:url" content="{canonical}">
<meta property="og:image" content="{og_img}">
<meta property="og:image:alt" content="duod logo">
<meta name="twitter:card" content="summary">
<meta name="twitter:title" content="{e(title)}">
<meta name="twitter:description" content="{e(desc)}">
<meta name="twitter:image" content="{og_img}">
<link rel="stylesheet" href="{BASE}/assets/site.css">
{ld}
</head>
<body>
<a class="skip" href="#main">Skip to content</a>
<header class="top">
  <a class="brand" href="{path('')}"><img src="{BASE}/assets/favicon.svg" width="28" height="28" alt=""><span>duod</span></a>
  <nav class="top-links" aria-label="Project"><a href="{REPO}">GitHub</a><a href="{REPO}/releases">Releases</a></nav>
</header>
<div class="shell">
  <nav class="side" aria-label="Documentation">
{nav_html(slug)}
  </nav>
  <main id="main">
    <article>
{crumbs}
{toc}
{body}
    </article>
{foot}
  </main>
</div>
<footer class="foot">
  <p>duod is open source under the <a href="{REPO}/blob/main/LICENSE">Apache License 2.0</a>. <a href="{REPO}">Source on GitHub</a>. <a href="{path('troubleshooting')}">Troubleshooting</a>.</p>
</footer>
</body>
</html>
"""


def faq_body():
    items = "".join(f"<h2>{e(q)}</h2><p>{e(a)}</p>" for q, a in FAQ)
    return "<h1>Frequently asked questions</h1>\n" + items


def main():
    if OUT.exists():
        shutil.rmtree(OUT)
    (OUT / "assets").mkdir(parents=True)
    for name in ("duod-logo.svg", "duod-logo-dark.svg", "duod-logo.png", "favicon.svg", "duod-mark-512.png"):
        shutil.copy(ASSETS / name, OUT / "assets" / name)
    shutil.copy(ROOT / "site.css", OUT / "assets" / "site.css")
    (OUT / ".nojekyll").write_text("")

    for idx, (slug, _, title, desc, _) in enumerate(PAGES):
        if slug == "faq":
            body = faq_body()
        else:
            body = (ROOT / "pages" / f"{slug or 'index'}.html").read_text()
        body = add_heading_anchors(body.replace("{{BASE}}", BASE).replace("{{REPO}}", REPO))
        target = OUT / slug / "index.html" if slug else OUT / "index.html"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(layout(slug=slug, title=title, desc=desc, body=body, idx=idx))

    notfound = ('<h1>Page not found</h1><p>This page does not exist. Start at the '
                f'<a href="{path("")}">duod overview</a> or pick a topic from the menu.</p>')
    (OUT / "404.html").write_text(layout(
        slug="404", title="Page not found | duod", desc="Page not found.", body=notfound,
        idx=0, noindex=True, canonical=url("")))

    urls = "".join(
        f"  <url><loc>{url(s)}</loc><lastmod>{LASTMOD}</lastmod>"
        f"<priority>{'1.0' if s == '' else '0.7'}</priority></url>\n" for s, *_ in PAGES)
    (OUT / "sitemap.xml").write_text(
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n' + urls + "</urlset>\n")
    (OUT / "robots.txt").write_text(f"User-agent: *\nAllow: /\n\nSitemap: {SITE_URL}{BASE}/sitemap.xml\n")
    print(f"built {len(PAGES)} pages -> {OUT} ({SITE_URL}{BASE}/)")


if __name__ == "__main__":
    main()
