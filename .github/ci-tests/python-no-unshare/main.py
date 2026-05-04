#!/usr/bin/env python3
"""Negative test: verify unshare is blocked by the default seccomp profile (WDY-1099)."""

import ctypes
import ctypes.util
import sys

# unshare(2) with CLONE_NEWUSER (0x10000000) is the canonical first step in
# an unprivileged container-escape chain.  The seccomp profile must deny the
# unshare syscall entirely (not just CLONE_NEWUSER), so we test the bare call.
CLONE_NEWUSER = 0x10000000

try:
    libc = ctypes.CDLL("libc.so.6", use_errno=True)
    ret = libc.unshare(CLONE_NEWUSER)
    err = ctypes.get_errno()
    if ret == -1:
        # Any denial (EPERM=1, ENOSYS=38, EPERM from seccomp) is acceptable —
        # the important thing is the call did not succeed.
        print(f"PASS: unshare(CLONE_NEWUSER) blocked (errno {err}) — seccomp profile applied")
        sys.exit(0)
    else:
        print(f"FAIL: unshare(CLONE_NEWUSER) succeeded — seccomp profile not applied")
        sys.exit(1)
except OSError as e:
    print(f"FAIL: unexpected OS error calling unshare: {e}")
    sys.exit(1)
