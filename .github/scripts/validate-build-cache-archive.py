#!/usr/bin/env python3
"""Reject build cache archives that could escape their extraction root.

  validate-build-cache-archive.py --archive < archive.tar
      Checks a tar stream before extraction.
  validate-build-cache-archive.py --tree DIRECTORY
      Checks the extracted tree, so differences between this parser and tar's
      cannot hide anything.

Allowed entries are regular files, directories and relative symlinks. Paths
and symlink targets must stay inside the root, and neither may pass through
another symlink, so links cannot be chained or written through. Hard links,
absolute links and special files are rejected; real Xcode and SwiftPM build
archives contain none. Used by restore-trusted-build-cache.sh. WDY-3798.
"""

import os
import posixpath
import stat
import sys
import tarfile


def fail(message: str) -> None:
    sys.exit(message)


def walk(base: list, path: str, symlinks: set, original: str, final_may_be_link: bool) -> list:
    """Resolves path component by component from base without following links.

    Fails if the path is absolute, climbs above the root, or passes through a
    symlink, so the lexical result always matches the filesystem's.
    """
    if posixpath.isabs(path):
        fail(f"absolute path: {original}")
    parts = list(base)
    components = [c for c in path.split("/") if c not in ("", ".")]
    for index, component in enumerate(components):
        if component == "..":
            if not parts:
                fail(f"path escapes the root: {original}")
            parts.pop()
            continue
        parts.append(component)
        is_last = index == len(components) - 1
        if "/".join(parts) in symlinks and not (is_last and final_may_be_link):
            fail(f"path passes through a symlink: {original}")
    return parts


def check(entries) -> None:
    """entries: iterable of (path, kind, link_target) with kind in file/dir/symlink."""
    entries = list(entries)
    symlinks = set()
    for path, kind, _ in entries:
        if kind == "symlink":
            symlinks.add("/".join(walk([], path, set(), path, True)))
    for path, kind, target in entries:
        parts = walk([], path, symlinks, path, kind == "symlink")
        if kind == "symlink":
            walk(parts[:-1], target, symlinks, f"{path} -> {target}", False)


def archive_entries():
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as archive:
        for member in archive:
            path = member.name
            if member.isfile():
                yield path, "file", None
            elif member.isdir():
                yield path, "dir", None
            elif member.issym():
                yield path, "symlink", member.linkname
            else:
                fail(f"unsupported entry type: {member.name}")


def tree_entries(root: str):
    for directory, subdirectories, files in os.walk(root, followlinks=False):
        for name in subdirectories + files:
            full = os.path.join(directory, name)
            path = os.path.relpath(full, root).replace(os.sep, "/")
            mode = os.lstat(full).st_mode
            if stat.S_ISLNK(mode):
                yield path, "symlink", os.readlink(full)
            elif stat.S_ISDIR(mode):
                yield path, "dir", None
            elif stat.S_ISREG(mode):
                yield path, "file", None
            else:
                fail(f"unsupported file type: {path}")


def main() -> None:
    if sys.argv[1:] == ["--archive"]:
        check(archive_entries())
    elif len(sys.argv) == 3 and sys.argv[1] == "--tree":
        check(tree_entries(sys.argv[2]))
    else:
        fail("usage: validate-build-cache-archive.py --archive < archive.tar | --tree DIRECTORY")


if __name__ == "__main__":
    main()
