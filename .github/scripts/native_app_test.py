"""Keep the native simulator job runnable without changing its layout or hiding skips."""

import importlib.util
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('simulator', Path(__file__).with_name('select-ios-simulator.py'))
simulator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(simulator)


def runtime(version, devices=None, **kwargs):
    return dict(identifier='com.apple.CoreSimulator.SimRuntime.iOS-' + version.replace('.', '-'),
                version=version, isAvailable=True,
                supportedDeviceTypes=[{'identifier': device} for device in
                                      (devices if devices is not None else [simulator.DEVICE_TYPE])], **kwargs)


class NativeAppTests(unittest.TestCase):
    def test_latest_runtime_must_support_the_compact_phone(self):
        compact = runtime('27.0')
        duo = runtime('27.1', ['com.apple.CoreSimulator.SimDeviceType.iPhone-Duo'])
        internal = runtime('28.0', isInternal=True)
        unavailable = dict(runtime('29.0'), isAvailable=False)
        self.assertEqual(simulator.select([runtime('26.4'), duo, compact, internal, unavailable]),
                         (compact['identifier'], simulator.DEVICE_TYPE))

    def test_no_supported_runtime_fails_instead_of_switching_layout(self):
        for runtimes in ([], [runtime('25.0')], [runtime('27.1', [])]):
            with self.subTest(runtimes=runtimes), self.assertRaises(ValueError):
                simulator.select(runtimes)

    def test_ui_exclusions_only_cover_explicit_fixture_or_device_tests(self):
        script = (ROOT / '.github/scripts/test-native-app.sh').read_text()
        exclusions = set(re.findall(r'-skip-testing:WendyMobileUITests/([\w/]+)', script))
        used = set()
        selected = []
        for source in (ROOT / 'go/macos/UITests-iOS').glob('*.swift'):
            text = source.read_text()
            suite = re.search(r'class (\w+): XCTestCase', text)[1]
            methods = re.split(r'\n    func (test\w+)\([^\n]*\{', text)
            for name, body in zip(methods[1::2], methods[2::2]):
                path = suite + '/' + name
                excluded = suite in exclusions or path in exclusions
                self.assertEqual(excluded, 'XCTSkip' in body, path)
                if excluded:
                    used.add(suite if suite in exclusions else path)
                else:
                    selected.append(path)
        self.assertEqual(used, exclusions)
        self.assertEqual(len(selected), 6)
        self.assertIn('result["skippedTests"] == 0', script)


if __name__ == '__main__':
    unittest.main()
