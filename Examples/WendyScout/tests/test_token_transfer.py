"""No real credential, device, GitHub request or remote command in these tests."""

import importlib.util
import pathlib
import os, pty, select, subprocess, time
import sys
import tempfile
import textwrap
import unittest
from unittest.mock import patch

root = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location(
    "provision_token", root / "provision_token.py"
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
DEVICE = "cloud://api.dev.wendy.sh:443/tenant/00000000-0000-0000-0000-000000000000/asset/00000000-0000-0000-0000-000000000001"


class TransferTests(unittest.TestCase):
    def test_other_targets_rejected_before_process_or_file_access(self):
        for target in [
            "cloud://api.wendy.sh/tenant/example",
            "localhost",
            DEVICE + ",prod",
            DEVICE + "\n",
        ]:
            with (
                self.subTest(target=target),
                patch.object(module.subprocess, "Popen") as p,
                self.assertRaises(ValueError),
            ):
                module.transfer(target, "/not/read")
                p.assert_not_called()

    def test_sender_waits_for_receiver_before_reading_file(self):
        # A local fake Wendy process asserts no bytes arrive before its ready
        # marker. The scoped value is disposable fixture data, never a token.
        with tempfile.TemporaryDirectory() as d:
            base = pathlib.Path(d)
            key = base / "new-token"
            key.write_text("github_pat_scoped_fixture")
            key.chmod(0o600)
            receiver = base / "fake-wendy"
            receiver.write_text(
                "#!"
                + sys.executable
                + "\n"
                + textwrap.dedent("""\
                import select,sys,time
                assert not select.select([sys.stdin],[],[],.1)[0]
                print('SCOUT_SECRET_READY',flush=True)
                assert sys.stdin.readline().strip()=='github_pat_scoped_fixture'
                print('SCOUT_SECRET_STORED',flush=True)
            """)
            )
            receiver.chmod(0o700)
            with patch("builtins.print") as output:
                module.transfer(DEVICE, key, str(receiver))
            self.assertNotIn("github_pat_scoped_fixture", str(output.call_args_list))

    def test_receiver_real_pty_never_echoes_fixture(self):
        with tempfile.TemporaryDirectory() as d:
            # Replace only the Dev-check transport and destination with local
            # fixtures. Exercise the actual receiver PTY/read/write protocol.
            remote = module.REMOTE.replace(
                "'/var/lib/wendy/volumes/wendy-scout/secrets'", repr(d)
            )
            script = "import subprocess\nsubprocess.run=lambda *a,**k:None\n" + remote
            master, slave = pty.openpty()
            p = subprocess.Popen(
                [sys.executable, "-c", script], stdin=slave, stdout=slave, stderr=slave
            )
            os.close(slave)
            transcript = bytearray()
            sent = False
            deadline = time.monotonic() + 5
            try:
                while time.monotonic() < deadline:
                    if not select.select([master], [], [], 0.1)[0]:
                        if p.poll() is not None:
                            break
                        continue
                    try:
                        block = os.read(master, 4096)
                    except OSError:
                        break
                    if not block:
                        break
                    transcript.extend(block)
                    if not sent and b"SCOUT_SECRET_READY" in transcript:
                        os.write(master, b"github_pat_scoped_fixture\n")
                        sent = True
                self.assertEqual(p.wait(timeout=1), 0)
                self.assertTrue(sent)
                self.assertIn(b"SCOUT_SECRET_STORED", transcript)
                self.assertNotIn(b"github_pat_scoped_fixture", transcript)
                stored = pathlib.Path(d) / "github-issues-token"
                self.assertEqual(stored.read_text(), "github_pat_scoped_fixture\n")
                self.assertEqual(stored.stat().st_mode & 0o777, 0o600)
            finally:
                if p.poll() is None:
                    p.kill()
                    p.wait()
                os.close(master)

    def test_receiver_disables_echo_before_ready(self):
        self.assertLess(
            module.REMOTE.index("termios.tcsetattr(0,termios.TCSAFLUSH,quiet)"),
            module.REMOTE.index("print('SCOUT_SECRET_READY'"),
        )
        self.assertIn("termios.ECHO|termios.ECHONL", module.REMOTE)
