"""Offline driver regressions: fake compiler transport, real Swift formatter."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

BASE = Path(__file__).resolve().parent
FILES = ('EnglishCacheClearTests.swift', 'EnglishEntitlementAddTests.swift',
         'EnglishEntitlementInvalidTests.swift', 'EnglishDeviceDefaultTests.swift')


class CompilerDriverTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        package = self.root / 'repo/swift/WendyE2ETests'
        self.inputs = package / 'EnglishSpecs'
        self.inputs.mkdir(parents=True)
        for source in [BASE / 'compile.py', *BASE.glob('*.md')]:
            shutil.copyfile(source, self.inputs / source.name)
        shutil.copyfile(BASE.parents[2] / '.swift-format', self.root / 'repo/.swift-format')
        self.stored = package / 'Tests/WendyE2ETests/GeneratedEnglishSpecs'
        self.stored.mkdir(parents=True)
        self.originals = {name: f'// Stored original: {name}\n'.encode() for name in FILES}
        for name, content in self.originals.items():
            (self.stored / name).write_bytes(content)
        self.compiler = self.root / 'fake-codex'
        self.compiler.write_text(f'#!{sys.executable}\n' + '''
import json
from pathlib import Path
import sys
raw = Path(sys.argv[sys.argv.index('--output-last-message') + 1])
raw.write_bytes(Path(__file__).with_name('transport-output.swift').read_bytes())
events = Path(__file__).with_name('transport-events.json')
items = json.loads(events.read_text()) if events.exists() else [
    {'type': 'thread.started', 'thread_id': 'offline-fixture'},
    {'type': 'turn.completed', 'usage': {'input_tokens': 0, 'output_tokens': 0}},
]
for event in items:
    print(json.dumps(event))
''')
        self.compiler.chmod(0o755)

    def run_driver(self, label, *arguments):
        return subprocess.run(
            [sys.executable, str(self.inputs / 'compile.py'), '--run-dir',
             str(self.root / label), *arguments], capture_output=True, text=True, timeout=30
        )

    def test_malformed_result_preserves_every_stored_file(self):
        parts = []
        for index, name in enumerate(FILES):
            body = 'func broken( {\n' if index == 1 else f'struct Fixture{index} {{}}\n'
            parts.append(f'// FILE: {name}\n' + body)
        (self.root / 'transport-output.swift').write_text(''.join(parts))
        result = self.run_driver('malformed', '--codex', str(self.compiler))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('error', result.stderr.lower())
        for name, original in self.originals.items():
            self.assertEqual((self.stored / name).read_bytes(), original)

    def test_success_has_replayable_provenance_and_repository_formatting(self):
        raw = b'// FILE: EnglishCacheClearTests.swift\nstruct Fixture {\nfunc value() -> Int { 1 }\n}\n'
        (self.root / 'transport-output.swift').write_bytes(raw)
        result = self.run_driver('success', '--cache-only', '--codex', str(self.compiler))
        self.assertEqual(result.returncode, 0, result.stderr)
        measurement = json.loads((self.root / 'success/measurement.json').read_text())
        self.assertEqual(measurement['raw_output_sha256'], hashlib.sha256(raw).hexdigest())
        self.assertEqual(measurement, json.loads((self.root / 'success/import.json').read_text()))
        formatted = (self.stored / FILES[0]).read_bytes()
        self.assertIn(b'\n    func value()', formatted)
        result = self.run_driver('replay', '--cache-only', '--import-raw',
                                 str(self.root / 'success/raw-output.swift'))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.stored / FILES[0]).read_bytes(), formatted)
        for name in FILES[1:]:
            self.assertEqual((self.stored / name).read_bytes(), self.originals[name])

    def test_rejected_tool_call_cannot_be_replayed(self):
        (self.root / 'transport-output.swift').write_text(
            '// FILE: EnglishCacheClearTests.swift\nstruct Fixture {}\n')
        (self.root / 'transport-events.json').write_text(json.dumps([
            {'type': 'thread.started', 'thread_id': 'offline-rejected'},
            {'type': 'item.completed', 'item': {'type': 'command_execution'}},
            {'type': 'turn.completed', 'usage': {'input_tokens': 0, 'output_tokens': 0}},
        ]))
        result = self.run_driver('rejected', '--cache-only', '--codex', str(self.compiler))
        self.assertNotEqual(result.returncode, 0)
        measurement = json.loads((self.root / 'rejected/measurement.json').read_text())
        self.assertTrue(measurement['acceptance']['process_succeeded'])
        self.assertFalse(measurement['acceptance']['no_tools'])
        result = self.run_driver('rejected-replay', '--cache-only', '--import-raw',
                                 str(self.root / 'rejected/raw-output.swift'))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('replay rejected', result.stderr)
        for name, original in self.originals.items():
            self.assertEqual((self.stored / name).read_bytes(), original)


if __name__ == '__main__':
    unittest.main()
