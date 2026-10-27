"""Thin Core interop. Product conveniences belong in the Wendy Python SDK."""
from ctypes import CDLL, c_int32, c_char_p
from pathlib import Path
from sys import platform

__all__ = ["WendyHelloWorld", "WendyCoreVersion"]

_filename = {"darwin": "libWendyCore.dylib", "linux": "libWendyCore.so", "win32": "WendyCore.dll"}.get(platform)
if _filename is None:
    raise ImportError(f"Wendy Core does not support {platform}")
_library = CDLL(str(Path(__file__).parent / "native" / _filename))
_hello = _library.WendyHelloWorld
_hello.argtypes = []
_hello.restype = c_int32


def WendyHelloWorld() -> int:
    """Return 42 from the shared Go engine."""
    return _hello()

_version = _library.WendyCoreVersion
_version.argtypes = []
_version.restype = c_char_p


def WendyCoreVersion() -> str:
    """Return the effective version of the loaded Wendy Core engine."""
    return _version().decode("utf-8")
