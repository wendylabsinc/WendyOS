"""Ordinary owned-process regression; no devices or kernel configuration."""
import importlib.util
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import tempfile
import threading
import unittest

SCRIPT=Path(__file__).with_name('run-owned-media-test.py')
spec=importlib.util.spec_from_file_location('owned_media_report',SCRIPT)
MEDIA=importlib.util.module_from_spec(spec);spec.loader.exec_module(MEDIA)


@unittest.skipUnless(os.name=='posix','Unix process-group guardian')
class GuardianReportTests(unittest.TestCase):
    def test_signal_during_successful_child_drain_is_not_success(self):
        for signum in (signal.SIGTERM, signal.SIGINT):
            with self.subTest(signal=signum), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                ready, draining = root / "ready", root / "draining"
                # The leaf is a real member of the owned guardian group. It
                # observes the guardian's final TERM only after the direct
                # child has exited zero and its result has been reported.
                leaf = (
                    "import signal,time; from pathlib import Path; "
                    "signal.signal(signal.SIGTERM, lambda s,f: (Path(" + repr(str(draining)) + ").write_text('term'), exit(0))); "
                    "Path(" + repr(str(ready)) + ").write_text('ready'); "
                    "time.sleep(30)"
                )
                direct = (
                    "import subprocess,sys,time; from pathlib import Path; "
                    "subprocess.Popen([sys.executable,'-c'," + repr(leaf) + "]); "
                    "deadline=time.monotonic()+5\n"
                    "while not Path(" + repr(str(ready)) + ").exists():\n"
                    " if time.monotonic()>=deadline: sys.exit(2)\n"
                    " time.sleep(.01)\n"
                    "sys.exit(0)\n"
                )
                stop, sent = threading.Event(), threading.Event()
                def interrupt_drain():
                    deadline = time.monotonic() + 8
                    while not stop.wait(.005) and time.monotonic() < deadline:
                        if draining.exists():
                            # Signal this process while MEDIA owns its handler;
                            # never signal a recorded/reaped external PID.
                            os.kill(os.getpid(), signum)
                            sent.set()
                            return
                sender = threading.Thread(target=interrupt_drain)
                previous = signal.getsignal(signum)
                outside = []
                def outside_guard(received, frame):
                    outside.append(received)
                # Under extreme scheduler starvation a late sender must fail
                # this test, not terminate the enclosing test process.
                signal.signal(signum, outside_guard)
                sender.start()
                try:
                    result = MEDIA.run([sys.executable, "-c", direct], 10)
                    self.assertEqual(signal.getsignal(signum), outside_guard)
                finally:
                    stop.set()
                    sender.join(2)
                    signal.signal(signum, previous)
                self.assertFalse(sender.is_alive())
                self.assertTrue(sent.is_set(), "real owned descendant never reached final TERM drain")
                self.assertEqual(outside, [], "signal arrived outside the owned cleanup window")
                self.assertEqual(result, 128 + signum, "DRAIN_SIGNAL_MUST_NOT_REPORT_SUCCESS")

    def test_closed_report_reader_does_not_release_group_leader(self):
        directory=tempfile.TemporaryDirectory()
        ready=Path(directory.name)/'child-ready'
        reader,writer=os.pipe()
        # Model the real cancellation path: the supervisor closes its reader
        # before the owned child finishes graceful shutdown and reports status.
        os.close(reader)
        child=subprocess.Popen([sys.executable,'-c',MEDIA.GUARDIAN,str(writer),
                                sys.executable,'-c','from pathlib import Path; Path('+repr(str(ready))+').write_text("ready")'],
                               pass_fds=(writer,),start_new_session=True,
                               stdout=subprocess.DEVNULL,stderr=subprocess.PIPE,text=True)
        os.close(writer)
        try:
            deadline=time.monotonic()+5
            while not ready.exists() and child.poll() is None and time.monotonic()<deadline:
                time.sleep(.01)
            self.assertTrue(ready.exists(),'owned child did not start after guardian installed its handlers')
            time.sleep(.1)
            self.assertIsNone(child.poll(),'closed report pipe released the pinned guardian')
            child.send_signal(signal.SIGTERM)
            time.sleep(.1)
            self.assertIsNone(child.poll(),'guardian must survive TERM until its owner finishes group signaling')
        finally:
            # Only this retained direct child is signaled. Its fixture child
            # has already exited; no saved PID/PGID is used after a reap.
            if child.poll() is None:child.kill()
            _,stderr=child.communicate(timeout=3)
            directory.cleanup()
        self.assertNotIn('BrokenPipeError',stderr)

if __name__=='__main__':unittest.main()
