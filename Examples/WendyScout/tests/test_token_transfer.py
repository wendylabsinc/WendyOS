"""No real credential, device, GitHub request or remote command in these tests."""

import importlib.util
import pathlib
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

    def test_receiver_disables_echo_before_ready(self):
        self.assertLess(
            module.REMOTE.index("termios.tcsetattr(0,termios.TCSAFLUSH,quiet)"),
            module.REMOTE.index("print('SCOUT_SECRET_READY'"),
        )
        self.assertIn("termios.ECHO|termios.ECHONL", module.REMOTE)
