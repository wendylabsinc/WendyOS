"""Operator-only scoped-token transfer over Wendy's authenticated device PTY.

Never imports a Mac auth profile. The receiver disables terminal echo before the
sender reads the new token file. No credential enters argv, environment or logs.
"""

import argparse
import os
import pathlib
import re
import selectors
import signal
import stat
import subprocess
import time

REMOTE = r"""
import os,pathlib,re,subprocess,sys,termios
subprocess.run(['nerdctl','exec','sh.wendy.scout_control','python3','-c',
 'import os; from local import Local; Local(os.environ["WENDY_AGENT_SOCKET"]).verify_development()'],
 check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=20)
if not os.isatty(0):raise RuntimeError('Authenticated PTY required')
original=termios.tcgetattr(0); quiet=termios.tcgetattr(0)
quiet[3]&=~(termios.ECHO|termios.ECHONL)
termios.tcsetattr(0,termios.TCSAFLUSH,quiet)
try:
 print('SCOUT_SECRET_READY',flush=True)
 token=sys.stdin.readline(4097).strip()
 if not re.fullmatch(r'(?:github_pat_|ghs_)[A-Za-z0-9_]+',token):raise RuntimeError('Scoped token required')
 root=pathlib.Path('/var/lib/wendy/volumes/wendy-scout/secrets')
 root.mkdir(mode=0o700,exist_ok=True);os.chmod(root,0o700)
 fd=os.open(root/'github-issues-token.new',os.O_WRONLY|os.O_CREAT|os.O_TRUNC|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'w') as f:
  os.fchmod(f.fileno(),0o600);f.write(token+'\n');f.flush();os.fsync(f.fileno())
 os.replace(root/'github-issues-token.new',root/'github-issues-token')
 print('SCOUT_SECRET_STORED',flush=True)
finally:termios.tcsetattr(0,termios.TCSANOW,original)
"""


def transfer(device, token_file, cli="wendy"):
    uuid = r"[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}"
    if not re.fullmatch(
        r"cloud://api\.dev\.wendy\.sh:443/tenant/" + uuid + r"/asset/" + uuid, device
    ):
        raise ValueError("An explicit Development device selector is required")
    p = subprocess.Popen(
        [
            cli,
            "--json",
            "--device",
            device,
            "device",
            "shell",
            "--",
            "python3",
            "-c",
            REMOTE,
        ],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    sent = False
    data = bytearray()
    deadline = time.monotonic() + 60
    sel = selectors.DefaultSelector()
    sel.register(p.stdout, selectors.EVENT_READ)
    try:
        while sel.get_map():
            if time.monotonic() > deadline:
                raise TimeoutError("Credential transfer deadline")
            for key, _ in sel.select(0.2):
                block = os.read(key.fileobj.fileno(), 4096)
                if not block:
                    sel.unregister(key.fileobj)
                    continue
                data.extend(block)
                if len(data) > 65536:
                    raise RuntimeError("Unexpected transfer output")
                if not sent and b"SCOUT_SECRET_READY" in data:
                    fd = os.open(
                        token_file, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
                    )
                    with os.fdopen(fd) as f:
                        st = os.fstat(f.fileno())
                        if (
                            not stat.S_ISREG(st.st_mode)
                            or stat.S_IMODE(st.st_mode) != 0o600
                            or st.st_uid != os.getuid()
                        ):
                            raise ValueError(
                                "New scoped-token file must be yours, mode 0600"
                            )
                        token = f.read(4097).strip()
                    if len(token) > 4096 or not re.fullmatch(
                        r"(?:github_pat_|ghs_)[A-Za-z0-9_]+", token
                    ):
                        raise ValueError("New scoped token required")
                    p.stdin.write((token + "\n").encode())
                    p.stdin.flush()
                    sent = True
        if p.wait(timeout=2) != 0 or b"SCOUT_SECRET_STORED" not in data:
            raise RuntimeError("Transfer failed; remote output withheld")
    finally:
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGKILL)
        p.wait()
        p.stdin.close()
        p.stdout.close()
        sel.close()
    print(
        "Scoped credential stored in Scout control volume; no publication test performed."
    )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--device", required=True)
    parser.add_argument(
        "--file",
        type=pathlib.Path,
        required=True,
        help="NEW approved issues-only token, mode 0600",
    )
    parser.add_argument("--wendy", default="wendy")
    args = parser.parse_args()
    transfer(args.device, args.file, args.wendy)
