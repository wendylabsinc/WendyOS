"""Portable evidence admission; actual filesystem behavior requires macOS."""
import importlib.util
import json
import os
import sys
import tempfile
from pathlib import Path
import unittest

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location('cache_publication', HERE/'test-g1-cache-publication.py')
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


def events(*records):
    return '\n'.join(json.dumps(record) for record in records)


class Admission(unittest.TestCase):
    def test_real_child_keeps_download_diagnostics_outside_test_events(self):
        marker = 'G1_FAILED_PUBLICATION_LEAKED'
        for marker_in_events in (True, False):
            with self.subTest(marker_in_events=marker_in_events), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary)
                raw = events({'Test':'TestG1CachePublicationLifecycle','Action':'fail'},
                             {'Output':marker if marker_in_events else 'unrelated failure'})
                diagnostic = 'go: downloading golang.org/x/sys v0.50.0\n' + marker
                child = ('import sys; print(' + repr(raw) + '); print(' + repr(diagnostic) +
                         ', file=sys.stderr); sys.exit(1)')
                code, actual = GATE.run_logged([sys.executable, '-c', child], output, 'normal', dict(os.environ))
                self.assertEqual(code, 1)
                self.assertEqual(actual, raw+'\n')
                self.assertEqual((output/'normal.stderr.log').read_text(), diagnostic+'\n')
                self.assertEqual(GATE.parse_result(code, actual, marker), marker_in_events)
                with self.assertRaises(json.JSONDecodeError):
                    GATE.parse_result(code, diagnostic+'\n'+actual, marker)

    def test_historical_success_is_explicitly_not_reproduction(self):
        raw = events({'Test':'TestMaterializeIdentity','Action':'pass'})
        self.assertFalse(GATE.historical_result(0, raw))

    def test_only_actual_rename_permission_failure_is_reproduction(self):
        raw = events({'Test':'TestMaterializeIdentity','Action':'fail'},
                     {'Output':'publishing G1 source cache: rename source target: permission denied'})
        self.assertTrue(GATE.historical_result(1, raw))
        for text in ('compilation failed', 'cache was modified', 'protecting published G1 source cache: permission denied'):
            with self.assertRaises(RuntimeError):
                GATE.historical_result(1, events({'Test':'TestMaterializeIdentity','Action':'fail'}, {'Output':text}))

    def test_original_sequence_has_no_synthetic_observers(self):
        source = (GATE.ROOT/'go/simulator/g1/bundle.go').read_text()
        historical = GATE.historical_source(source)
        self.assertNotIn('cachePublicationRename', historical)
        self.assertNotIn('cachePublicationCleanup', historical)
        self.assertIn('os.Rename(temporary, target)', historical)
        with self.assertRaises(RuntimeError):
            GATE.historical_source(source.replace('temporary = target', 'temporary = ""'))

    def test_control_needs_actual_named_failure_without_skip_or_race(self):
        marker = 'G1_FAILED_PUBLICATION_LEAKED'
        raw = events({'Test':'TestG1CachePublicationLifecycle','Action':'fail'}, {'Output':marker})
        self.assertTrue(GATE.parse_result(1, raw, marker))
        for code in (0,2,124,130,143):
            self.assertFalse(GATE.parse_result(code, raw, marker))
        self.assertFalse(GATE.parse_result(1, raw+'\n'+events({'Action':'skip'}), marker))
        self.assertFalse(GATE.parse_result(1, raw+'\n'+events({'Output':'WARNING: DATA RACE'}), marker))
        self.assertFalse(GATE.parse_result(1, events({'Output':marker}), marker))
        for failure in ('G1_CACHE_CHILD_DRAIN_UNKNOWN', 'G1_CACHE_RETAINED_UNKNOWN_DRAIN',
                        'G1_CLEANUP_OBSERVER_ERROR', 'panic:', 'test timed out'):
            self.assertFalse(GATE.parse_result(1, raw+'\n'+events({'Output':failure}), marker))


if __name__ == '__main__':
    unittest.main()
