"""Linux kernel sandbox for offline creative tests. Failure never falls back to UID-only."""

import ctypes, errno, json, os, pathlib, resource, selectors, signal, subprocess, time

UID = 10002
MAX_OUTPUT = 65536
ACTIVE = set()
libc = ctypes.CDLL(None, use_errno=True)


class Ruleset(ctypes.Structure):
    _fields_ = [("handled_access_fs", ctypes.c_uint64)]


class Beneath(ctypes.Structure):
    _pack_ = 1
    _fields_ = [("allowed_access", ctypes.c_uint64), ("parent_fd", ctypes.c_int)]


def landlock(job):
    abi = libc.syscall(444, 0, 0, 1)
    if abi < 3:
        raise RuntimeError(
            "Landlock ABI >= 3 required (including truncate confinement)"
        )
    handled = (1 << 15) - 1
    attr = Ruleset(handled)
    fd = libc.syscall(444, ctypes.byref(attr), ctypes.sizeof(attr), 0)
    if fd < 0:
        raise OSError(ctypes.get_errno(), "Landlock ruleset")
    try:
        # Worker container has neither the admin mount nor publisher credentials.
        # Landlock additionally denies /proc, /ipc, other runs and session files.
        for path, rights in [
            ("/usr", 13),
            ("/lib", 13),
            ("/lib64", 13),
            ("/bin", 13),
            ("/etc/ld.so.cache", 4),
            ("/dev/null", 6),
            ("/opt/scout-offline", 13),
            (str(job), 13 | 2 | (1 << 14)),
        ]:
            if not pathlib.Path(path).exists():
                continue
            opened = os.open(path, os.O_PATH | os.O_CLOEXEC)
            try:
                rule = Beneath(rights, opened)
                if libc.syscall(445, fd, 1, ctypes.byref(rule), 0) < 0:
                    raise OSError(ctypes.get_errno(), "Landlock path rule")
            finally:
                os.close(opened)
        if libc.prctl(38, 1, 0, 0, 0) != 0 or libc.syscall(446, fd, 0) < 0:
            raise OSError(ctypes.get_errno(), "Landlock restrict")
    finally:
        os.close(fd)


def filter_syscalls():
    import seccomp

    f = seccomp.SyscallFilter(defaction=seccomp.ERRNO(errno.EPERM))
    names = "read write readv writev pread64 pwrite64 close close_range fstat newfstatat statx lseek mmap mprotect munmap brk rt_sigaction rt_sigprocmask rt_sigreturn sigaltstack futex set_robust_list rseq set_tid_address arch_prctl getrandom madvise fcntl dup dup2 dup3 pipe pipe2 pselect6 poll ppoll select getcwd fstatat64 fstat64 stat stat64 lstat lstat64 getdents64 readlink readlinkat access faccessat faccessat2 getpid getppid gettid getuid getgid geteuid getegid getgroups uname sysinfo sched_getaffinity sched_yield clock_gettime gettimeofday clock_nanosleep nanosleep times getrusage prlimit64 exit exit_group execve wait4 waitid restart_syscall ftruncate fsync fdatasync epoll_create1 epoll_ctl epoll_wait epoll_pwait eventfd2 timerfd_create timerfd_settime timerfd_gettime getrlimit".split()
    for name in names:
        if seccomp.resolve_syscall(seccomp.Arch.NATIVE, name) != -1:
            f.add_rule(seccomp.ALLOW, name)
    # No file creation => hard aggregate disk bound (only precreated output is
    # writable). No sockets, ptrace, process_vm/pidfd access, mount/setns/unshare,
    # metadata mutation, package installation, execveat or memfd executable payloads.
    forbidden = os.O_CREAT | (getattr(os, "O_TMPFILE", 0) & ~os.O_DIRECTORY)
    for name, arg in [("openat", 2), ("open", 1)]:
        if seccomp.resolve_syscall(seccomp.Arch.NATIVE, name) != -1:
            f.add_rule(
                seccomp.ALLOW, name, seccomp.Arg(arg, seccomp.MASKED_EQ, forbidden, 0)
            )
    mask = (
        0x20000
        | 0x02000000
        | 0x04000000
        | 0x08000000
        | 0x10000000
        | 0x20000000
        | 0x40000000
        | 0x8000
        | 0x00800000
    )
    f.add_rule(seccomp.ALLOW, "clone", seccomp.Arg(0, seccomp.MASKED_EQ, mask, 0))
    for name in ["fork", "vfork"]:
        if seccomp.resolve_syscall(seccomp.Arch.NATIVE, name) != -1:
            f.add_rule(seccomp.ALLOW, name)
    f.add_rule(seccomp.ALLOW, "tgkill", seccomp.Arg(0, seccomp.EQ, os.getpid()))
    f.load()


def confine(job):
    os.setgroups([])
    os.setgid(UID)
    os.setuid(UID)
    if libc.prctl(1, signal.SIGKILL, 0, 0, 0) != 0:
        raise RuntimeError("PDEATHSIG failed")
    for name, value in [
        (resource.RLIMIT_AS, 1536 * 1024**2),
        (resource.RLIMIT_CPU, 12),
        (resource.RLIMIT_NPROC, 24),
        (resource.RLIMIT_FSIZE, 1024**2),
        (resource.RLIMIT_NOFILE, 48),
        (resource.RLIMIT_CORE, 0),
    ]:
        resource.setrlimit(name, (value, value))
    landlock(job)
    filter_syscalls()


def run(script, job, timeout=20, inputs=None):
    job = pathlib.Path(job)
    job.mkdir(parents=True, exist_ok=True)
    os.chmod(job, 0o755)
    (job / "inputs.json").write_text(json.dumps(inputs or {}))
    os.chmod(job / "inputs.json", 0o444)
    src = job / "test.py"
    src.write_text(script)
    os.chmod(src, 0o444)
    out = job / "output.txt"
    out.write_text("")
    os.chown(out, UID, UID)
    os.chmod(out, 0o600)
    env = {
        "PATH": "/usr/bin:/bin",
        "HOME": str(job),
        "TMPDIR": str(job),
        "PYTHONDONTWRITEBYTECODE": "1",
        "LANG": "C.UTF-8",
    }
    t = time.monotonic()
    p = subprocess.Popen(
        [
            "/usr/bin/python3",
            "-I",
            "-B",
            str(pathlib.Path(__file__).resolve()),
            "--execute",
            str(job),
        ],
        cwd=job,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        start_new_session=True,
        close_fds=True,
    )
    ACTIVE.add(p)
    data = bytearray()
    failure = None
    try:
        sel = selectors.DefaultSelector()
        sel.register(p.stdout, selectors.EVENT_READ)
        while sel.get_map():
            if time.monotonic() - t > timeout:
                raise TimeoutError("Offline test wall-time limit")
            for key, _ in sel.select(0.1):
                block = os.read(key.fileobj.fileno(), 8192)
                if not block:
                    sel.unregister(key.fileobj)
                    continue
                data.extend(block)
                if len(data) > MAX_OUTPUT:
                    raise ValueError("Offline test output limit")
        p.wait(timeout=1)
    except (TimeoutError, ValueError, subprocess.TimeoutExpired) as e:
        failure = str(e)
    finally:
        # Always kill the whole group, even when the leader exited before its
        # forked children. setsid/setpgid are unavailable after filter installation.
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        p.wait()
        p.stdout.close()
        ACTIVE.discard(p)
        deadline = time.monotonic() + 1
        while True:
            try:
                pid, _ = os.waitpid(-p.pid, os.WNOHANG)
                if pid == 0:
                    if time.monotonic() >= deadline:
                        break
                    time.sleep(0.01)
            except ChildProcessError:
                break
    return {
        "evidence_kind": "offline_sandbox",
        "exit": p.returncode,
        "limit": failure,
        "seconds": time.monotonic() - t,
        "output": data[:MAX_OUTPUT].decode(errors="replace"),
        "file_output": out.read_text(errors="replace")[:MAX_OUTPUT],
    }


if __name__ == "__main__":
    # Install restrictions in a fresh single-threaded trusted process. Python
    # preexec_fn in the multithreaded bridge is unsafe and hides setup errors.
    import sys

    if len(sys.argv) != 3 or sys.argv[1] != "--execute":
        raise SystemExit("Internal sandbox launcher only")
    job = pathlib.Path(sys.argv[2]).resolve()
    script = (job / "test.py").read_text()
    os.chdir(job)
    try:
        confine(job)
    except BaseException:
        import traceback

        traceback.print_exc()
        raise SystemExit(125)
    os.execve(
        "/usr/bin/python3", ["python3", "-I", "-B", "-c", script], dict(os.environ)
    )
