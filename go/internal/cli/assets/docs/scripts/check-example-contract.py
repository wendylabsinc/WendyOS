#!/usr/bin/env python3
"""Resolve published Lite examples against pinned, read-only upstream Git trees.

This checks catalog selection and referenced source files. It does not claim to
compile, flash, run an LED, or validate live GitHub branch availability.
"""
import argparse
from html.parser import HTMLParser
import json
from pathlib import Path
import shlex
import subprocess
from urllib.parse import urlsplit, unquote

REVISIONS = {
    "templates": "44d5d1ba804c332c1b6787b9812bbcdd4e005aee",
    "native": "73271d49c1c6872ca46e63a36e94012c6fd68f2f",
}


class Article(HTMLParser):
    def __init__(self, raw):
        super().__init__(convert_charrefs=True)
        self.code = []
        self.links = []
        self.current = None
        self.feed(raw)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "code":
            self.current = []
        if tag == "a" and attrs.get("href"):
            self.links.append(attrs["href"])

    def handle_data(self, data):
        if self.current is not None:
            self.current.append(data)

    def handle_endtag(self, tag):
        if tag == "code" and self.current is not None:
            self.code.append("".join(self.current))
            self.current = None


def git(repo, *args):
    return subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True, text=True, timeout=15).stdout


def check(root, templates, native):
    # Read immutable committed content, not an untracked local substitute.
    trees = {}
    for name, repo in (("templates", templates), ("native", native)):
        revision = REVISIONS[name]
        actual = git(repo, "rev-parse", revision + "^{commit}").strip()
        if actual != revision:
            raise ValueError(f"wrong {name} provenance: {actual}")
        trees[name] = set(git(repo, "ls-tree", "-r", "--name-only", revision).splitlines())
    catalog = json.loads(git(templates, "show", REVISIONS["templates"] + ":meta.json"))
    guide = Article((root / "advanced/wendy-lite/getting-started/index.html").read_text())
    native_guide = Article((root / "advanced/wendy-lite/native-apps/index.html").read_text())
    commands = [shlex.split(line) for block in guide.code for line in block.splitlines() if line.strip()]
    selected = [c[c.index("--template") + 1] for c in commands if c[:2] == ["wendy", "init"] and "--template" in c]
    if len(selected) != 1:
        raise ValueError("getting-started must contain one unambiguous template selection")
    name = selected[0]
    entries = [t for t in catalog["templates"] if t["name"] == name and "wendy-lite" in t.get("targets", [])]
    if len(entries) != 1 or not entries[0].get("languages"):
        raise ValueError(f"documented Lite template is absent from real catalog: {name}")
    # A guide's project directory and prose must agree with its actual init
    # command. The old #1980 text used blink-rgb, which is only an upstream
    # native example, not this catalog's CLI template.
    if ["mkdir", name] not in commands or ["cd", name] not in commands:
        raise ValueError("project directory differs from the selected template")
    named_examples = [c for c in guide.code if c.startswith("blink-") and "/" not in c and "\n" not in c]
    if not named_examples or any(c != name for c in named_examples):
        raise ValueError(f"prose example differs from template command: {named_examples}")
    paths = [c[len(name) + 1:] for c in guide.code if c.startswith(name + "/")]
    if not paths:
        raise ValueError("guide no longer identifies an editable example source")
    checked = []
    for language in entries[0]["languages"]:
        prefix = f"{language}/{name}/"
        manifest = json.loads(git(templates, "show", REVISIONS["templates"] + ":" + prefix + "template.json"))
        if manifest["name"] != name:
            raise ValueError("template manifest differs from catalog selection")
        for path in paths:
            if prefix + path not in trees["templates"]:
                raise ValueError(f"documented editable file is absent: {prefix + path}")
            checked.append(prefix + path)
    examples = []
    for href in guide.links + native_guide.links:
        url = urlsplit(href)
        if url.netloc != "github.com" or not url.path.startswith("/wendylabsinc/wendy-lite-native-apps"):
            continue
        parts = unquote(url.path).strip("/").split("/")
        if len(parts) == 2:
            continue
        if len(parts) < 5 or parts[2:4] != ["tree", "main"]:
            raise ValueError(f"unsupported native example reference: {href}")
        path = "/".join(parts[4:]).rstrip("/")
        if not any(p.startswith(path + "/") for p in trees["native"]):
            raise ValueError(f"native example directory does not exist: {path}")
        if path + "/main/main.c" not in trees["native"]:
            raise ValueError(f"native example has no C entry point: {path}")
        examples.append(path)
    if not examples:
        raise ValueError("native-apps no longer links a concrete native example")
    return {"revisions": REVISIONS, "template": name, "editable_sources": checked, "native_examples": sorted(set(examples))}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--templates", type=Path, required=True)
    parser.add_argument("--native-examples", type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(check(args.root, args.templates, args.native_examples), indent=2))
