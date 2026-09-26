#!/usr/bin/env python3
"""Comment on a Renovate PR with what the bump means for this repository.

Ported from the infrastructure repo's reviewer, with its Helm rendering replaced by Go facts.

  1. triage   (jev-latest): risk and merge/review/block, from the change and its notes
  2. locate   (jev-latest): relevance of every source file in the repo to those notes
  3. comment  (jev-router): the PR comment, reading the notes against the most relevant files

For a Go module bump the notes are extended with where this repository imports each changed
module, file by file. That is the most trustworthy input: a change in a package we never import
cannot reach us, whatever the notes say.

Advisory only: it never approves, merges or blocks. A PR with no notes and no Go facts gets a
fixed "review manually" comment instead of a guess.

DESCRIBE_BUMPS decides which PRs get steps 2 and 3; triage always runs and is cheap:
  none     triage verdict only
  flagged  describe only bumps triage marks review or block (the default)
  all      describe every bump, including the ones that do not affect us

Environment: OPENROUTER_API_KEY, GITHUB_TOKEN, GITHUB_REPOSITORY, PR_NUMBER, and optionally
DESCRIBE_BUMPS. Pass --dry-run to print the comment instead of posting it.
"""

import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
REPO = os.environ["GITHUB_REPOSITORY"]
MARKER = "<!-- review-dependency-bump -->"
MAX_FILES = 6
RELEVANCE_FLOOR = 0.3
SOURCES = ["*.go", "go.mod", "Dockerfile", ".github/workflows/*.yaml", "conform.example.yaml"]


def request(url, body=None, method=None, headers=None, attempts=2):
    data = json.dumps(body).encode() if body is not None else None
    for attempt in range(attempts):
        req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
        try:
            with urllib.request.urlopen(req, timeout=300) as response:
                raw = response.read().decode()
                return json.loads(raw) if "json" in response.headers.get("Content-Type", "") else raw
        except urllib.error.HTTPError as error:
            # jev-router intermittently times out choosing a model; one retry clears it.
            if error.code >= 500 and attempt + 1 < attempts:
                time.sleep(10)
                continue
            sys.exit(f"{method or ('POST' if data else 'GET')} {url}: HTTP {error.code}: "
                     f"{error.read().decode()[:1000]}")


def openrouter(path, body):
    return request("https://openrouter.ai/api" + path, body, headers={
        "Authorization": f"Bearer {os.environ['OPENROUTER_API_KEY']}",
        "Content-Type": "application/json"})


def github(path, body=None, method=None, accept="application/vnd.github+json"):
    return request("https://api.github.com" + path, body, method, headers={
        "Authorization": f"Bearer {os.environ['GITHUB_TOKEN']}",
        "Accept": accept, "X-GitHub-Api-Version": "2022-11-28"})


def pull_request(number):
    meta = github(f"/repos/{REPO}/pulls/{number}")
    diff = github(f"/repos/{REPO}/pulls/{number}", accept="application/vnd.github.diff")
    # go.sum churns checksum lines that say nothing about the upgrade.
    diff = "".join(part for part in re.split(r"(?m)^(?=diff --git )", diff)
                   if not part.startswith("diff --git a/go.sum "))
    body = meta["body"] or ""
    has_notes = "### Release Notes" in body
    notes = body.split("### Configuration")[0]
    notes = re.sub(r"<[^>]+>|&#8203;", "", notes)
    notes = re.sub(r"\n{2,}", "\n", notes).strip()
    return {"title": meta["title"], "diff": diff, "release_notes": notes}, has_notes


def go_mod_changes(diff):
    section = re.search(r"(?ms)^diff --git a/go\.mod .*?(?=^diff --git |\Z)", diff)
    if not section:
        return {}
    changes = {}
    for sign, module, version, indirect in re.findall(
            r"(?m)^([-+])\s*(?:require\s+)?([\w.-]+\.[\w.-]+/\S+)\s+(v\S+)(\s*// indirect)?", section.group(0)):
        entry = changes.setdefault(module, {"indirect": bool(indirect)})
        entry["old" if sign == "-" else "new"] = version
    return {m: c for m, c in changes.items() if c.get("old") != c.get("new")}


def go_files():
    return subprocess.run(["git", "ls-files", "*.go"], cwd=ROOT, capture_output=True,
                          text=True, check=True).stdout.split()


def import_sites(module):
    pattern = re.compile(rf'^\s*(?:\w+\s+)?"({re.escape(module)}(?:/[^"]*)?)"', re.M)
    sites = []
    for path in go_files():
        text = (ROOT / path).read_text(errors="replace")
        for match in pattern.finditer(text):
            sites.append(f"{path}:{text.count(chr(10), 0, match.start()) + 1} imports {match.group(1)}")
    return sites


def go_facts(diff):
    changes = go_mod_changes(diff)
    if not changes:
        return ""
    lines = []
    for module, change in sorted(changes.items()):
        span = f"{change.get('old', 'added')} -> {change.get('new', 'removed')}"
        sites = import_sites(module)
        if sites:
            lines.append(f"- {module} {span}, imported by our code at:\n" +
                         "\n".join(f"  - {s}" for s in sites))
        else:
            kind = "indirect" if change["indirect"] else "direct"
            lines.append(f"- {module} {span} ({kind}), not imported anywhere in our code")
    return "\n\n## Where this repository imports each changed module\n" + "\n".join(lines)


def triage(change):
    questions = {
        "risk": {"type": "score",
                 "instructions": "How likely is this dependency bump to need attention beyond merging: breaking changes, required manual steps, or changed defaults?",
                 "criteria": ["0: routine; nothing in the notes needs attention",
                              "1: almost certainly needs manual attention or will break something"]},
        "action": {"type": "choice",
                   "instructions": "Based on the change and its release/upgrade notes, what should happen to this pull request?",
                   "criteria": {"merge": "Routine; merge without further review.",
                                "review": "Something in the notes may affect how this code builds or behaves and should be checked against the repository.",
                                "block": "The notes make clear this cannot be merged as-is."}},
    }
    return openrouter("/alpha/decisions", {"model": "~typesafe/jev-latest", "state": change,
                                           "questions": questions})


def summarise(path):
    text = (ROOT / path).read_text(errors="replace")
    if path.endswith(".go"):
        package = re.search(r"(?m)^package\s+(\w+)", text)
        block = re.search(r"(?ms)^import\s*\((.*?)^\)|^import\s+(.*?)$", text)
        imports = re.findall(r'"([^"]+)"', block.group(0)) if block else []
        return f"package {package.group(1) if package else '?'}; imports: {', '.join(imports) or 'none'}"
    if path == "go.mod":
        return "module requirements: " + ", ".join(re.findall(r"(?m)^\t(\S+) v", text)[:15])
    if path == "Dockerfile":
        return "image build; " + "; ".join(re.findall(r"(?m)^(?:FROM|ARG) .*$", text))
    keys = re.findall(r"(?m)^([A-Za-z][\w.-]*):", text)
    return "top-level keys: " + ", ".join(keys[:15])


def locate(change):
    files = subprocess.run(["git", "ls-files", *SOURCES], cwd=ROOT, capture_output=True,
                           text=True, check=True).stdout.split()
    ids = {f"file_{i}": path for i, path in enumerate(files)}
    state = {"change": {"title": change["title"], "diff": change["diff"]},
             "notes": change["release_notes"],
             "repository_files": {fid: f"{path}: {summarise(path)}" for fid, path in ids.items()}}
    question = lambda path: {
        "type": "score",
        "instructions": f"How relevant is repository file {path} to checking whether anything in the notes affects this repository? Relevant files import the upgraded dependency, call APIs the notes change, or build or run it.",
        "criteria": ["0: unrelated to the upgraded dependency and to everything the notes mention",
                     "1: must be read to judge whether the notes affect this repository"]}
    result = openrouter("/alpha/decisions", {"model": "~typesafe/jev-latest", "state": state,
                                             "questions": {fid: question(p) for fid, p in ids.items()}})
    scored = sorted(((a["score"], ids[fid]) for fid, a in result["answers"].items()), reverse=True)
    return scored, result["usage"]["cost"]


def numbered(path):
    lines = (ROOT / path).read_text(errors="replace").splitlines()
    return "\n".join(f"{n:4}  {line}" for n, line in enumerate(lines, 1))


DEPTH = {
    "merge": "Triage judged this routine. Keep the comment short: say why it does not affect us, and "
             "mention anything relevant only if the notes actually touch code we use.",
    "review": "Triage flagged this for review. Check every note item that could plausibly matter "
              "against the files, and be specific about what is affected.",
    "block": "Triage judged this unsafe to merge as-is. Explain precisely what breaks and where.",
}


def comment(change, paths, action):
    files = "\n\n".join(f"=== {p} ===\n{numbered(p)}" for p in paths)
    prompt = f"""You are writing the pull request comment for a dependency bump in conform, a Go
media transcoder: a CLI that plans and runs ffmpeg transcodes, and an orchestrator that creates one
Kubernetes Job per file through client-go. Its container image is built from the golang image and
runs on debian, whose apt ffmpeg is the one conform drives. The reader is the repository owner
deciding whether to merge. They care only about what this bump means for *this* code, not about
upstream changes in general.

{DEPTH[action]}

Write GitHub markdown with exactly this structure and nothing before or after it:

### <emoji> <one-line headline>
Use 🟢 when nothing affects this repository, 🟡 when something affects it but merging is fine with
the noted follow-up, 🔴 when it should not be merged as-is.

**Why:** one to three sentences on the reasoning behind the verdict.

**Affects us:** a bullet per change that touches this repository, each citing `file:line` from the
files below and saying what it changes for us. If nothing does, write exactly: None.

**Doesn't affect us:** one line summarising the remaining upstream changes and why they are inert
here (e.g. "changes to packages we don't import; docs and CI changes").

**Before merging:** bullets of concrete actions, only if any are needed. Omit this heading otherwise.

Rules: cite only what the files and diffs below show. When they cannot settle whether something is
handled, say what to check (a command or a file) instead of asserting either way. Do not repeat the
release notes; translate them into consequences for this code. A change to a package this
repository does not import cannot affect it, except through a package it does import. CI already
runs go vet, go test and govulncheck on this PR, so a compile error will surface there; focus on
behaviour that still compiles but changes.

## Pull request
{change['title']}

{change['diff']}

## Notes
{change['release_notes']}

## Repository files (line-numbered)
{files}
"""
    result = openrouter("/v1/chat/completions", {"model": "typesafe/jev-router",
                                                 "messages": [{"role": "user", "content": prompt}]})
    return (result["choices"][0]["message"]["content"].strip(), result.get("model"),
            result["usage"].get("cost") or 0)


NO_NOTES = """### ⚪ No release notes found — review manually

**Why:** Renovate found no release notes for this package, and it is not a Go module bump, so
there is nothing to judge it against. An automated verdict here would be a guess.

If the project publishes notes somewhere Renovate cannot see, a `sourceUrl` packageRule in
`renovate.json` pointing at the upstream repository usually fixes this for future bumps."""


def post(number, body):
    body = f"{MARKER}\n{body}"
    existing = [c for c in github(f"/repos/{REPO}/issues/{number}/comments?per_page=100")
                if MARKER in (c["body"] or "")]
    if existing:
        github(f"/repos/{REPO}/issues/comments/{existing[0]['id']}", {"body": body}, "PATCH")
    else:
        github(f"/repos/{REPO}/issues/{number}/comments", {"body": body}, "POST")


TRIAGE_ONLY = {
    "merge": "### 🟢 Routine bump\n\nTriage found nothing in the notes that needs attention.",
    "review": "### 🟡 Flagged for review\n\nTriage found something in the notes that may affect this "
              "code. Check the notes against the repository before merging.",
    "block": "### 🔴 Flagged as unsafe to merge\n\nTriage judged from the notes that this cannot be merged as-is.",
}


def describe_mode():
    mode = os.environ.get("DESCRIBE_BUMPS") or "flagged"
    if mode not in ("none", "flagged", "all"):
        sys.exit(f"DESCRIBE_BUMPS must be none, flagged or all, not {mode!r}")
    return mode


def main():
    number = os.environ["PR_NUMBER"]
    dry_run = "--dry-run" in sys.argv
    mode = describe_mode()
    change, has_notes = pull_request(number)
    facts = go_facts(change["diff"])

    if not has_notes and not facts:
        body = NO_NOTES
    else:
        change["release_notes"] += facts
        step1 = triage(change)
        a = step1["answers"]
        action = a["action"]["choice"]
        probabilities = ", ".join(f"{k} {v:.2f}" for k, v in
                                  sorted(a["action"]["probabilities"].items(), key=lambda kv: -kv[1]))
        details = [f"- **Triage** (jev-latest): {action} ({probabilities}), "
                   f"confidence {a['action']['confidence']:.2f}, risk {a['risk']['score']:.2f}"]
        total = step1["usage"]["cost"]

        if mode == "all" or (mode == "flagged" and action != "merge"):
            scored, locate_cost = locate(change)
            picked = [p for s, p in scored[:MAX_FILES] if s >= RELEVANCE_FLOOR]
            body, model, write_cost = comment(change, picked, action)
            total += locate_cost + write_cost
            considered = "\n".join(f"  - `{p}` ({s:.2f})" for s, p in scored if p in picked)
            details += [f"- **Files read** (jev-latest relevance, of {len(scored)}):\n{considered}",
                        f"- **Written by** jev-router → `{model}`"]
        else:
            body = TRIAGE_ONLY[action]
            details.append(f"- **Not described**: `DESCRIBE_BUMPS={mode}`")

        details.append(f"- **Cost** ${total:.4f}")
        body += ("\n\n<details><summary>How this was decided</summary>\n\n" + "\n".join(details) +
                 "\n\nAdvisory only — this comment never approves, merges or blocks.\n</details>")

    if dry_run:
        print(body)
    else:
        post(number, body)


if __name__ == "__main__":
    main()
