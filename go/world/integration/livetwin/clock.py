"""Same-host elapsed clock shared with the native Mac viewer, not UTC authority.

Older Python versions on macOS subtract a process-specific origin despite
reporting mach_absolute_time() as their monotonic implementation. Read Mach
ticks directly for values comparable to Swift's demoMonotonicNS().
"""
from __future__ import annotations

import ctypes
from dataclasses import dataclass
import sys
import time
from typing import Callable, Optional


class _Timebase(ctypes.Structure):
    _fields_ = [('numer', ctypes.c_uint32), ('denom', ctypes.c_uint32)]


def ticks_to_ns(ticks, numer, denom):
    if not (type(ticks) is int and ticks >= 0 and type(numer) is int and numer > 0 and type(denom) is int and denom > 0):
        raise ValueError('valid Mach ticks and timebase required')
    value = (ticks // denom) * numer + (ticks % denom) * numer // denom
    if value > (1 << 63) - 1:
        raise ValueError('host elapsed clock exceeds native range')
    return value


@dataclass(frozen=True)
class HostClock:
    read_ns: Callable[[], int]
    display_clock: Optional[str] = None

    def __call__(self):
        return self.read_ns()


def host_clock():
    """Use raw Mach nanoseconds on Darwin; keep headless Python time elsewhere."""
    if sys.platform != 'darwin':
        return HostClock(time.monotonic_ns)
    library = ctypes.CDLL('/usr/lib/libSystem.B.dylib')
    library.mach_timebase_info.argtypes = [ctypes.POINTER(_Timebase)]
    library.mach_timebase_info.restype = ctypes.c_int
    library.mach_absolute_time.argtypes = []
    library.mach_absolute_time.restype = ctypes.c_uint64
    scale = _Timebase()
    if library.mach_timebase_info(ctypes.byref(scale)) != 0 or not scale.numer or not scale.denom:
        raise RuntimeError('Mach timebase unavailable')
    numer, denom = scale.numer, scale.denom
    return HostClock(lambda: ticks_to_ns(library.mach_absolute_time(), numer, denom), 'mach_absolute_time_ns')
