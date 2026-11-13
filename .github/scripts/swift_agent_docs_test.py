#!/usr/bin/env python3
"""Admission checks; production configuration executes in the macOS gate."""
import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location('agent_docs', Path(__file__).with_name('check-swift-agent-docs.py'))
DOCS = importlib.util.module_from_spec(SPEC); SPEC.loader.exec_module(DOCS)


class DocumentedAgentDefaults(unittest.TestCase):
    def test_observed_defaults_reject_actual_old_documentation(self):
        text = DOCS.DOC.read_text()
        observed = dict(host='127.0.0.1', port=50051, otel_port=4317, allows_network_access=False)
        DOCS.compare(text, observed)
        row = next(line for line in text.splitlines() if line.startswith('| `WENDY_AGENT_HOST` |'))
        with self.assertRaisesRegex(RuntimeError, 'DOC_AGENT_DEFAULT:'):
            DOCS.compare(text.replace(row, DOCS.ORIGINAL_HOST_ROW), observed)
        with self.assertRaisesRegex(RuntimeError, 'DOC_AGENT_LOCAL_ONLY:'):
            DOCS.compare(text, dict(observed, allows_network_access=True))

    def test_ambiguous_or_missing_rows_do_not_pass(self):
        text = DOCS.DOC.read_text()
        row = next(line for line in text.splitlines() if line.startswith('| `WENDY_AGENT_HOST` |'))
        for bad in (text + '\n' + row, text.replace(row, ''), text.replace('| `50051` |', '| 50051 |')):
            with self.assertRaisesRegex(RuntimeError, 'DOC_AGENT_TABLE:'):
                DOCS.rows(bad)


if __name__ == '__main__':
    unittest.main()
