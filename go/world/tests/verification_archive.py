"""Explicit opt-in for original historical runtime recordings, never fixtures to invent."""
import os
from pathlib import Path
import unittest


def archive_root():
    value = os.environ.get("WENDY_WORLD_VERIFICATION_ARCHIVE")
    if not value:
        raise unittest.SkipTest("historical recording check: set WENDY_WORLD_VERIFICATION_ARCHIVE to the original docs/verification archive; see tests/README.md")
    root = Path(value)
    if not root.is_absolute() or not root.is_dir():
        raise ValueError("WENDY_WORLD_VERIFICATION_ARCHIVE must be an existing absolute archive directory")
    return root


def archive_file(relative):
    path = archive_root() / relative
    if not path.is_file():
        raise FileNotFoundError("explicit historical archive is incomplete: " + relative)
    return path
