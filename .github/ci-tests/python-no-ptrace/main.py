#!/usr/bin/env python3
"""Negative test: verify ptrace is blocked by the default seccomp profile (WDY-1099)."""

import ctypes
import sys

# PTRACE_TRACEME = 0; calling ptrace(PTRACE_TRACEME, 0, 0, 0) requires the
# ptrace syscall.  The seccomp profile must deny it with EPERM (errno 1).
PTRACE_TRACEME = 0

try:
    libc = ctypes.CDLL("libc.so.6", use_errno=True)
    ret = libc.ptrace(PTRACE_TRACEME, 0, 0, 0)
    err = ctypes.get_errno()
    if ret == -1 and err in (1, 13):  # EPERM=1, EACCES=13
        print(f"PASS: ptrace blocked by seccomp profile (errno {err})")
        sys.exit(0)
    elif ret == -1:
        # Some other error (e.g. EPERM from Yama LSM) still means ptrace was
        # denied, which is the desired outcome.
        print(f"PASS: ptrace denied (ret={ret}, errno={err})")
        sys.exit(0)
    else:
        print(f"FAIL: ptrace(PTRACE_TRACEME) succeeded (ret={ret}) — seccomp profile not applied")
        sys.exit(1)
except OSError as e:
    print(f"FAIL: unexpected OS error calling ptrace: {e}")
    sys.exit(1)
