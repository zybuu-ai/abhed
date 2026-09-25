#!/usr/bin/env python3
"""Render docs/ into web/zybuu/docs/ as static HTML.

The documentation is written as markdown in docs/ because that is where it is
useful to whoever is editing the code. Duplicating it into HTML by hand would
guarantee the two drift, so this renders it at publish time instead: the
markdown stays the source, the site is a build artifact.

Deliberately dependency-free. A docs build that needs a package install is a
docs build that breaks on a machine that has not run it before, and this has to
work from the same laptop that serves the site.
"""
import html
import json
import os
import re
import shutil
import sys
import tempfile

# --embed-only renders the documentation straight into the binary's embed
# directory and nothing else: no site tree, no sitemap, no llms.txt. It is
# what a contributor and the release build run, so a binary built anywhere
# carries its own docs; the public site is published by a separate process.
EMBED_ONLY = "--embed-only" in sys.argv

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SRC = os.path.join(ROOT, "docs")
OUT = tempfile.mkdtemp(prefix="abhed-docs-") if EMBED_ONLY else os.path.join(ROOT, "web", "zybuu", "docs")
# The same HTML is embedded in the binary, so an air-gapped install has local
# documentation with no route to the public copy. One generator, two outputs —
# rendering twice from one source is what keeps them from disagreeing.
EMBED = os.path.join(ROOT, "internal", "docsite", "site")

# Where each page canonically lives. The docs are served from two hosts
# (zybuu.com/docs directly, abhed.zybuu.com/docs through the Worker — see
# web/zybuu/_redirects for why the apex keeps serving them), so every page
# names abhed.zybuu.com as its canonical URL and the search engines index one
# copy. The access policy is the exception: the revocation email links to
# zybuu.com/abhed/access-policy, so that is where it is canonical.
SITE = os.environ.get("ABHED_SITE_URL", "https://zybuu.com")
DOCS = os.environ.get("ABHED_DOCS_URL", "https://abhed.zybuu.com/docs/")
OG_IMAGE = SITE + "/media/og-abhed.png"

# Which trees are published. docs/internal/ is competitive analysis and working
# notes — it stays off the public site.
SECTIONS = [
    ("guide", "Guide", "Using Abhed day to day."),
    ("architecture", "Architecture", "How it is built, and why."),
    ("ops", "Operations", "Running it in a real environment."),
    ("trust", "Trust", "What a security review will ask, answered from the code."),
]


# The summary at the top of web/zybuu/llms.txt (https://llmstxt.org). It is
# what an answer engine reads instead of the marketing page, so it states the
# product plainly, with the same facts and the same limits the page carries.
LLMS_INTRO = """
# Abhed

> Abhed is an on-prem, air-gap-capable agent harness by Zybuu: the agent loop, the tool contracts, the permission engine and the sandbox, as one static binary (CLI, headless mode, JSON output, JSONL RPC, and a multi-user server with a web console) plus a Go SDK. It runs on your own hardware against a model you host, records every action in an append-only replayable log, and the Community Edition is open source under the Apache License 2.0; the Team and Enterprise features are a separate, proprietary edition.

Key facts, each stated on https://zybuu.com/abhed/ and traceable to the repository:

- Runs where the data is: one static binary, from a laptop to an air-gapped rack, with a signed offline bundle (built, verified by signature, then installed) for the rack. No egress by default.
- Runs any model: twenty providers over three wire formats, including a model on your own GPUs through an OpenAI-compatible endpoint such as Ollama. Changing vendors is a line of config.
- Sandboxed by default: tiers none / process / container / vm, chosen as the strongest backend available that meets the configured minimum; it refuses to start rather than silently downgrade.
- Every action on the record: an append-only event log, replayable step by step, with every approval and refusal and who made it, exportable as OpenTelemetry traces. On the Postgres store, events are immutable by database trigger and tenants are isolated by row-level security. The default in-memory store gives neither.
- Deny rules are absolute for every tool call in every permission mode; extensions may veto a tool call and can never permit one. A person's interactive shell in the workbench is bounded by the sandbox, and there deny rules are a best-effort screen on each line typed. No approver means refuse, never assume yes.
- Embeds through a Go SDK with the same loop, policy and record. The SDK builds no sandbox: the host owns isolation.
- Extend without forking: extensions (separate JSONL processes in any language), skills (a directory with a procedure), MCP servers (registered, disabled until enabled, optional tool allowlist).
- Not yet: no SOC 2, ISO 27001, HIPAA or FedRAMP certification; no support SLA; single node, no horizontal scaling or failover; Postgres is the only durable store; a human red-team engagement is outstanding; prompt injection is contained, not solved.
- Pricing: Community is free and open source (Apache-2.0); Team and Enterprise are priced per seat and per site, never per token, at https://zybuu.com/abhed/#pricing.
- Status: 1.x, open source at https://github.com/zybuu-ai/abhed. The command line, configuration, event record and Go SDK follow semantic versioning.

Who it is for: platform and security teams in places with no route out — defence and intelligence contractors, sovereign and public-sector deployments, operational networks, and banks whose policy is no cloud at all. It is not the right tool for a solo developer who is happy in the cloud.

Company: Zybuu (https://zybuu.com), founded 1 September 2026, founder-led. Contact support@zybuu.com.

Documentation index: https://abhed.zybuu.com/docs/
"""


def slug(section, name):
    # No .html extension. Cloudflare Pages serves /a/b for /a/b.html and 308s
    # the extension away, so linking to the extension costs every reader a
    # redirect on every click. The files are still written as .html.
    base = re.sub(r"\.md$", "", name)
    return f"{section}/{base}"


def title_of(text, fallback):
    m = re.search(r"^#\s+(.+)$", text, re.M)
    return m.group(1).strip() if m else fallback


def summary(text, limit=155):
    """The first prose paragraph, flattened and trimmed at a word boundary.

    Used for the index cards, the meta description and llms.txt, so all three
    describe a page the same way — and so the description is the page's own
    first sentence rather than one blurb repeated across a whole section,
    which search engines treat as duplicate content.
    """
    for para in text.split("\n\n"):
        q = para.strip()
        if not q or q.startswith(("#", "|", "```", ">", "- ", "* ")):
            continue
        # A status line ("Status: Draft · 2026-09-02 · Owner: …") is metadata,
        # not a description; the architecture documents all open with one.
        if re.match(r"^(\*\*)?(Status|Evidence status|Owner)\b", q):
            continue
        q = re.sub(r"`([^`]+)`", r"\1", q)                 # code spans
        q = re.sub(r"\*\*([^*]+)\*\*", r"\1", q)          # bold
        q = re.sub(r"\[([^\]]+)\]\([^)]+\)", r"\1", q)     # links
        q = " ".join(q.split())
        # The first sentence, when it can stand alone. A description that
        # stops at a sentence boundary reads better in a result snippet than
        # one cut mid-clause, and a page's opening sentence is usually its
        # best one-line account of itself.
        m = re.match(r"(.{40,%d}?[.!?])(\s|$)" % limit, q)
        if m:
            return m.group(1)
        if len(q) <= limit:
            return q
        cut = q[:limit].rsplit(" ", 1)[0]
        return cut.rstrip(",;:—- ") + "…"
    return ""


# --- a small, strict markdown subset --------------------------------------
# Only what the docs actually use. A partial renderer that is honest about its
# scope beats a permissive one that silently mangles something.
def render(md, section):
    out, i, lines = [], 0, md.split("\n")
    while i < len(lines):
        ln = lines[i]

        # fenced code
        if ln.startswith("```"):
            lang = ln[3:].strip()
            body, i = [], i + 1
            while i < len(lines) and not lines[i].startswith("```"):
                body.append(lines[i]); i += 1
            i += 1
            out.append('<pre class="code"><code class="lang-%s">%s</code></pre>'
                       % (html.escape(lang), html.escape("\n".join(body))))
            continue

        # table
        if ln.startswith("|") and i + 1 < len(lines) and re.match(r"^\|[\s:|-]+\|$", lines[i + 1]):
            head = [c.strip() for c in ln.strip("|").split("|")]
            i += 2
            rows = []
            while i < len(lines) and lines[i].startswith("|"):
                rows.append([c.strip() for c in lines[i].strip("|").split("|")])
                i += 1
            t = ["<div class='tw'><table><thead><tr>"]
            t += ["<th>%s</th>" % inline(c, section) for c in head]
            t.append("</tr></thead><tbody>")
            for r in rows:
                t.append("<tr>" + "".join("<td>%s</td>" % inline(c, section) for c in r) + "</tr>")
            t.append("</tbody></table></div>")
            out.append("".join(t))
            continue

        # heading
        m = re.match(r"^(#{1,4})\s+(.+)$", ln)
        if m:
            lvl = len(m.group(1))
            txt = m.group(2).strip()
            anchor = re.sub(r"[^a-z0-9]+", "-", txt.lower()).strip("-")
            out.append('<h%d id="%s">%s</h%d>' % (lvl, anchor, inline(txt, section), lvl))
            i += 1
            continue

        # list
        if re.match(r"^\s*[-*]\s+", ln):
            items = []
            # A wrapped item continues on indented lines (or any non-blank line
            # that is not itself a new block). Without this, the second line of
            # a two-line bullet rendered as a paragraph outside the list.
            while i < len(lines) and re.match(r"^\s*[-*]\s+", lines[i]):
                item = re.sub(r"^\s*[-*]\s+", "", lines[i]); i += 1
                while i < len(lines) and lines[i].strip() and not re.match(
                        r"^(#{1,4}\s|```|\||\s*[-*]\s|\s*\d+\.\s|>)", lines[i]):
                    item += " " + lines[i].strip(); i += 1
                items.append(item)
            out.append("<ul>" + "".join("<li>%s</li>" % inline(x, section) for x in items) + "</ul>")
            continue
        if re.match(r"^\s*\d+\.\s+", ln):
            items = []
            while i < len(lines) and re.match(r"^\s*\d+\.\s+", lines[i]):
                item = re.sub(r"^\s*\d+\.\s+", "", lines[i]); i += 1
                while i < len(lines) and lines[i].strip() and not re.match(
                        r"^(#{1,4}\s|```|\||\s*[-*]\s|\s*\d+\.\s|>)", lines[i]):
                    item += " " + lines[i].strip(); i += 1
                items.append(item)
            out.append("<ol>" + "".join("<li>%s</li>" % inline(x, section) for x in items) + "</ol>")
            continue

        # blockquote
        if ln.startswith(">"):
            body = []
            while i < len(lines) and lines[i].startswith(">"):
                body.append(lines[i].lstrip("> ")); i += 1
            out.append("<blockquote>%s</blockquote>" % inline(" ".join(body), section))
            continue

        if ln.strip() == "---":
            out.append("<hr>"); i += 1; continue

        if not ln.strip():
            i += 1; continue

        # paragraph
        para = []
        while i < len(lines) and lines[i].strip() and not re.match(
                r"^(#{1,4}\s|```|\||\s*[-*]\s|\s*\d+\.\s|>)", lines[i]):
            para.append(lines[i]); i += 1
        if para:
            out.append("<p>%s</p>" % inline(" ".join(para), section))
    return "\n".join(out)


def inline(s, section):
    # Code spans are extracted first so their contents are never treated as
    # markup — otherwise a documented `<tag>` becomes real markup.
    spans = []

    def stash(m):
        spans.append(m.group(1))
        return "\x00%d\x00" % (len(spans) - 1)

    s = re.sub(r"`([^`]+)`", stash, s)
    s = html.escape(s)
    s = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", s)
    s = re.sub(r"(?<!\*)\*([^*]+)\*(?!\*)", r"<em>\1</em>", s)

    def link(m):
        text, href = m.group(1), m.group(2)
        if href.endswith(".md"):
            href = re.sub(r"\.md$", "", href)
        elif ".md#" in href:
            href = href.replace(".md#", "#")
        return '<a href="%s">%s</a>' % (href, text)

    s = re.sub(r"\[([^\]]+)\]\(([^)]+)\)", link, s)
    for n, c in enumerate(spans):
        s = s.replace("\x00%d\x00" % n, "<code>%s</code>" % html.escape(c))
    return s


def main():
    style = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "docs.css")).read()
    logo = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "logo.html")).read()

    if os.path.isdir(OUT):
        shutil.rmtree(OUT)
    os.makedirs(OUT)

    # Collect pages
    tree = []
    for sec, label, blurb in SECTIONS:
        d = os.path.join(SRC, sec)
        if not os.path.isdir(d):
            continue
        pages = []
        for name in sorted(os.listdir(d)):
            if not name.endswith(".md") or name == "README.md":
                continue
            text = open(os.path.join(d, name)).read()
            pages.append((name, title_of(text, name), text))
        tree.append((sec, label, blurb, pages))

    # Two sources resolving to one URL is a build error, not a sitemap surprise:
    # the second page would silently replace the first.
    seen = {}
    for sec, _, _, pages in tree:
        for name, _, _ in pages:
            url = slug(sec, name)
            if url in seen:
                print("two pages claim /docs/%s (%s and %s) — rename one" % (url, seen[url], name), file=sys.stderr)
                return 1
            seen[url] = name

    total = sum(len(p[3]) for p in tree)
    VISION = os.path.join(SRC, "vision.md")
    if total == 0:
        print("no documentation found under docs/ — refusing to write an empty site",
              file=sys.stderr)
        return 1

    def nav(cur_sec, cur_name):
        n = ['<nav class="side"><a class="side-home" href="/docs/">Documentation</a>']
        if os.path.exists(VISION):
            on = ' class="on"' if cur_name == "vision.md" else ""
            n.append('<div class="sgrp"><p class="slabel">Overview</p><ul><li><a%s href="/docs/vision">'
                     'Why Zybuu, and why Abhed</a></li></ul></div>' % on)
        for sec, label, blurb, pages in tree:
            n.append('<div class="sgrp"><p class="slabel">%s</p><ul>' % label)
            for name, title, _ in pages:
                on = ' class="on"' if (sec == cur_sec and name == cur_name) else ""
                n.append('<li><a%s href="/docs/%s">%s</a></li>' % (on, slug(sec, name), html.escape(title)))
            n.append("</ul></div>")
        n.append("</nav>")
        return "".join(n)

    def shell(title, body, cur_sec="", cur_name="", desc="", canonical="",
              crumbs=(), kind="TechArticle"):
        full = title if title == "Abhed documentation" else title + " — Abhed documentation"
        canonical = canonical or DOCS
        # Structured data for answer engines and rich results: what the page
        # is, where it sits, and who publishes it. Nothing here is a claim the
        # page does not already make in prose.
        graph = [{
            "@type": kind,
            "@id": canonical + "#page",
            "headline": title,
            "name": full,
            "url": canonical,
            "description": desc,
            "inLanguage": "en",
            "isPartOf": {"@type": "WebSite", "@id": DOCS + "#website",
                         "name": "Abhed documentation", "url": DOCS},
            "about": {"@type": "SoftwareApplication", "@id": SITE + "/abhed/#software",
                      "name": "Abhed", "url": SITE + "/abhed/"},
            "publisher": {"@type": "Organization", "@id": SITE + "/#organization",
                          "name": "Zybuu", "url": SITE + "/",
                          "logo": {"@type": "ImageObject", "url": SITE + "/media/zybuu-mark.png"}},
        }]
        if crumbs:
            graph.append({
                "@type": "BreadcrumbList",
                "itemListElement": [
                    {"@type": "ListItem", "position": i + 1, "name": n, "item": u}
                    for i, (n, u) in enumerate(crumbs)],
            })
        ld = json.dumps({"@context": "https://schema.org", "@graph": graph},
                        indent=1, ensure_ascii=False)
        return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{html.escape(full)}</title>
<meta name="description" content="{html.escape(desc)}">
<link rel="canonical" href="{canonical}">
<meta name="robots" content="index, follow, max-image-preview:large">
<meta property="og:site_name" content="Zybuu">
<meta property="og:type" content="{"website" if kind == "CollectionPage" else "article"}">
<meta property="og:url" content="{canonical}">
<meta property="og:title" content="{html.escape(full)}">
<meta property="og:description" content="{html.escape(desc)}">
<meta property="og:image" content="{OG_IMAGE}">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:image:alt" content="Abhed: the agent harness for work that cannot leave the building">
<meta property="og:locale" content="en_GB">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="{html.escape(full)}">
<meta name="twitter:description" content="{html.escape(desc)}">
<meta name="twitter:image" content="{OG_IMAGE}">
<meta name="twitter:image:alt" content="Abhed: the agent harness for work that cannot leave the building">
<link rel="icon" type="image/png" href="data:image/png;base64,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">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500;600&display=swap">
<style>{style}</style>
<script type="application/ld+json">
{ld}
</script>
</head>
<body>
<header>
  <div class="dwrap bar">
    {logo}
    <nav class="nav">
      <a href="https://zybuu.com/" class="hide-sm">Zybuu</a>
      <a href="https://zybuu.com/abhed/">Abhed</a>
      <a href="/docs/">Docs</a>
      <a class="btn" href="https://abhed.zybuu.com">Open console</a>
    </nav>
  </div>
</header>
<div class="dwrap layout">
{nav(cur_sec, cur_name)}
<main class="doc">
{body}
</main>
</div>
<footer><div class="dwrap foot">
  <span><a href="https://zybuu.com/">Zybuu</a></span><span><a href="https://zybuu.com/abhed/">Abhed</a></span>
  <span>Documentation is generated from docs/ in the repository</span>
</div></footer>
</body>
</html>
"""

    written = 0
    for sec, label, blurb, pages in tree:
        os.makedirs(os.path.join(OUT, sec), exist_ok=True)
        for name, title, text in pages:
            body = render(text, sec)
            p = os.path.join(OUT, sec, re.sub(r"\.md$", ".html", name))
            url = DOCS + slug(sec, name)
            open(p, "w").write(shell(
                title, body, sec, name, summary(text) or blurb, canonical=url,
                crumbs=[("Abhed documentation", DOCS), (label, DOCS + "#" + sec), (title, url)]))
            written += 1

    # Index
    idx = ['<h1>Abhed documentation</h1>',
           '<p class="lede">Abhed is a deep agent harness. It runs where your code is, '
           'against whichever model you point it at, and records everything it does.</p>']
    vision_text = open(VISION).read() if os.path.exists(VISION) else ""
    if vision_text:
        idx.append('<h2 id="overview">Overview</h2><p>Why Zybuu exists, and why an agent '
                   'harness is its first product.</p><div class="cards">'
                   '<a class="dcard" href="/docs/vision"><b>%s</b><span>%s</span></a></div>'
                   % (html.escape(title_of(vision_text, "Vision")),
                      html.escape(summary(vision_text, 150))))
    for sec, label, blurb, pages in tree:
        idx.append('<h2 id="%s">%s</h2><p>%s</p><div class="cards">' % (sec, label, blurb))
        for name, title, text in pages:
            idx.append('<a class="dcard" href="/docs/%s"><b>%s</b><span>%s</span></a>'
                       % (slug(sec, name), html.escape(title), html.escape(summary(text, 150))))
        idx.append("</div>")
    open(os.path.join(OUT, "index.html"), "w").write(
        shell("Abhed documentation", "".join(idx), canonical=DOCS, kind="CollectionPage",
              desc="Documentation for Abhed, the on-prem, air-gap-capable agent harness by Zybuu: "
                   "guide, architecture, operations and trust."))
    written += 1

    # The vision document — why Zybuu, and why Abhed — is the answer to the
    # first question a buyer asks, and the site footer has linked to
    # /docs/vision since launch. It lives at the top of docs/ rather than in a
    # section, so it is rendered here rather than by the section loop.
    if vision_text:
        vurl = DOCS + "vision"
        open(os.path.join(OUT, "vision.html"), "w").write(shell(
            title_of(vision_text, "Vision"), render(vision_text, ""), "", "vision.md",
            summary(vision_text), canonical=vurl,
            crumbs=[("Abhed documentation", DOCS), (title_of(vision_text, "Vision"), vurl)]))
        written += 1

    # Standalone pages outside /docs/. The console access policy is linked
    # from the revocation email as zybuu.com/abhed/access-policy, so it is
    # rendered there, in the docs shell, from the same markdown the admin
    # clauses cite. A link in an email that goes nowhere is worse than none.
    policy = os.path.join(SRC, "access-policy.md")
    if os.path.exists(policy) and not EMBED_ONLY:
        text = open(policy).read()
        dst = os.path.join(os.path.dirname(OUT), "abhed", "access-policy.html")
        purl = SITE + "/abhed/access-policy"
        open(dst, "w").write(shell(title_of(text, "access-policy.md"), render(text, ""),
                                   desc="What access to the hosted Abhed console means, and how it ends.",
                                   canonical=purl, kind="WebPage",
                                   crumbs=[("Zybuu", SITE + "/"), ("Abhed", SITE + "/abhed/"),
                                           (title_of(text, "Access policy"), purl)]))
        written += 1

    if not EMBED_ONLY:
        # sitemap.xml and llms.txt are written here because this is the one place
        # that knows every page. The marketing pages are listed by hand; the docs
        # pages come from the tree, so a new document is in the sitemap the next
        # time the site is published rather than when someone remembers.
        urls = [SITE + "/", SITE + "/abhed/", SITE + "/abhed/access-policy", DOCS]
        if vision_text:
            urls.append(DOCS + "vision")
        for sec, label, blurb, pages in tree:
            urls += [DOCS + slug(sec, name) for name, _, _ in pages]
        sm = ['<?xml version="1.0" encoding="UTF-8"?>',
              '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">']
        sm += ["  <url><loc>%s</loc></url>" % html.escape(u) for u in urls]
        sm.append("</urlset>\n")
        open(os.path.join(os.path.dirname(OUT), "sitemap.xml"), "w").write("\n".join(sm))

        llms = [LLMS_INTRO.strip(), ""]
        if vision_text:
            llms += ["## Overview", "",
                     "- [%s](%svision): %s" % (title_of(vision_text, "Vision"), DOCS, summary(vision_text, 200)),
                     ""]
        for sec, label, blurb, pages in tree:
            llms += ["## %s" % label, "", blurb, ""]
            for name, title, text in pages:
                llms.append("- [%s](%s%s): %s" % (title, DOCS, slug(sec, name), summary(text, 200)))
            llms.append("")
        llms += ["## Optional", "",
                 "- [Abhed console access policy](%s/abhed/access-policy): what an invited account on "
                 "the hosted console can do, and what ends access." % SITE, ""]
        open(os.path.join(os.path.dirname(OUT), "llms.txt"), "w").write("\n".join(llms))

    # Mirror into the binary's embed directory.
    if os.path.isdir(EMBED):
        for name in os.listdir(EMBED):
            if name == ".keep":
                continue
            q = os.path.join(EMBED, name)
            shutil.rmtree(q) if os.path.isdir(q) else os.remove(q)
    else:
        os.makedirs(EMBED)
    for name in os.listdir(OUT):
        src_p, dst_p = os.path.join(OUT, name), os.path.join(EMBED, name)
        shutil.copytree(src_p, dst_p) if os.path.isdir(src_p) else shutil.copy2(src_p, dst_p)

    print("  rendered %d pages from docs/" % written)
    print("  embedded into internal/docsite/site" + ("" if EMBED_ONLY else " and written to web/zybuu/docs"))
    if EMBED_ONLY:
        shutil.rmtree(OUT, ignore_errors=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
