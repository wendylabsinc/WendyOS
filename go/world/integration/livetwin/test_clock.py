"""Actual same-host Python/Swift clock comparison, without any GUI or runtime."""
import ctypes
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from integration.livetwin.clock import host_clock, ticks_to_ns


class ClockTests(unittest.TestCase):
    def test_integer_conversion_matches_native_formula_without_float_loss(self):
        ticks = (1 << 54) + 123
        self.assertEqual(ticks_to_ns(ticks, 125, 3), ticks * 125 // 3)
        for args in ((1, 1, 0), (-1, 1, 1), (True, 1, 1), (1 << 63, 1, 1)):
            with self.subTest(args=args), self.assertRaises(ValueError): ticks_to_ns(*args)

    def test_non_mac_clock_is_headless_and_has_no_native_epoch_label(self):
        with patch('integration.livetwin.clock.sys.platform', 'linux'), \
                patch('integration.livetwin.clock.time.monotonic_ns', return_value=1234), \
                patch('integration.livetwin.clock.ctypes.CDLL') as load:
            clock = host_clock()
            self.assertEqual(clock(), 1234)
            self.assertIsNone(clock.display_clock)
            load.assert_not_called()

    @unittest.skipUnless(sys.platform == 'darwin', 'raw Mach exists only on Darwin')
    def test_actual_provider_falls_inside_independent_raw_mach_bracket(self):
        class Scale(ctypes.Structure):
            _fields_ = [('numer', ctypes.c_uint32), ('denom', ctypes.c_uint32)]
        library = ctypes.CDLL('/usr/lib/libSystem.B.dylib')
        library.mach_absolute_time.restype = ctypes.c_uint64
        scale = Scale(); self.assertEqual(library.mach_timebase_info(ctypes.byref(scale)), 0)
        clock = host_clock()
        self.assertEqual(clock.display_clock, 'mach_absolute_time_ns')
        for _ in range(100):
            before = library.mach_absolute_time() * scale.numer // scale.denom
            value = clock()
            after = library.mach_absolute_time() * scale.numer // scale.denom
            self.assertLessEqual(before, value); self.assertLessEqual(value, after)

    @unittest.skipUnless(sys.platform == 'darwin' and shutil.which('swiftc'), 'native Swift compiler required')
    def test_actual_compiled_viewer_clock_shares_python_provider_epoch(self):
        model = Path(__file__).resolve().parents[3] / 'macos/WorldClient/Sources/WorldTwin/Model.swift'
        source = model.read_text()
        start = source.index('func demoMonotonicNS() -> Int64 {')
        end = source.index('\n}', start) + 2
        function = source[start:end]
        with tempfile.TemporaryDirectory(prefix='wendy-clock-test-') as directory:
            root = Path(directory); swift = root / 'main.swift'; binary = root / 'clock'
            swift.write_text('import Foundation\nimport Darwin\n' + function + '\nprint(demoMonotonicNS())\n')
            subprocess.run([shutil.which('swiftc'), str(swift), '-o', str(binary)], check=True,
                           capture_output=True, timeout=30)
            clock = host_clock()
            for _ in range(3):
                before = clock()
                result = subprocess.run([str(binary)], check=True, capture_output=True, timeout=3)
                after = clock()
                self.assertEqual(result.stderr, b'')
                value = int(result.stdout)
                self.assertLessEqual(before, value); self.assertLessEqual(value, after)


if __name__ == '__main__':
    unittest.main()
