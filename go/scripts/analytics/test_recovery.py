"""Exercise the report with temporary tables in disposable PostgreSQL.
Run with TEST_DATABASE_URL set. No production data is read or changed.
"""
import csv
import io
import json
import os
from pathlib import Path
import subprocess
import unittest

QUERY = Path(__file__).with_name('recovery.sql').read_text()

@unittest.skipUnless(os.environ.get('TEST_DATABASE_URL'), 'requires disposable PostgreSQL')
class RecoveryReport(unittest.TestCase):
    def query(self, rows):
        sql = """BEGIN;
CREATE TEMP TABLE cli_events (anonymous_id text, attempt_id uuid, event text,
 command_name text DEFAULT 'wendy run', occurred_at timestamptz, created_at timestamptz DEFAULT '2026-11-11',
 success boolean DEFAULT false, error_class text, properties jsonb DEFAULT '{}',
 is_dev_build boolean DEFAULT false, cli_version text DEFAULT '2026.11.01-120000', schema_version int DEFAULT 2);
"""
        for row in rows:
            def literal(value):
                if value is None: return 'NULL'
                if isinstance(value, bool): return 'true' if value else 'false'
                if isinstance(value, dict): value = json.dumps(value)
                return "'" + str(value).replace("'", "''") + "'"
            sql += 'INSERT INTO cli_events (' + ','.join(row) + ') VALUES (' + ','.join(map(literal, row.values())) + ');\n'
        sql += QUERY + '\nROLLBACK;'
        result = subprocess.run(['psql', '-Xq', '--csv', '-v', 'ON_ERROR_STOP=1',
            '-v', 'start=2026-11-09', '-v', 'end=2026-11-12', '-v', 'as_of=2026-11-12',
            os.environ['TEST_DATABASE_URL']], input=sql, text=True, capture_output=True, check=True)
        return list(csv.DictReader(io.StringIO(result.stdout)))

    def event(self, kind, second=0, attempt=1, **extra):
        row = dict(anonymous_id='install-a', attempt_id=f'00000000-0000-4000-8000-{attempt:012d}',
                   event=kind, occurred_at=f'2026-11-10 12:00:{second:02d}+00')
        row.update(extra)
        return row

    def test_progress_and_cancel_are_not_failures(self):
        result = self.query([self.event('run_started'), self.event('run_finished', 5,
            properties={'outcome':'cancelled'}, error_class='context_canceled')])[0]
        self.assertEqual(result['failed_attempts'], '0')
        self.assertEqual(result['cancelled_attempts'], '1')
        self.assertEqual(result['ready_attempts'], '0')

    def test_app_start_does_not_prove_readiness(self):
        result = self.query([self.event('run_started'), self.event('app_started', 5, success=True)])[0]
        self.assertEqual(result['app_started_attempts'], '1')
        self.assertEqual(result['ready_attempts'], '0')
        self.assertEqual(result['attempts_without_terminal_event'], '1')
        self.assertEqual(result['median_seconds_to_ready'], '')

    def test_failure_then_later_readiness_and_duplicate_events(self):
        ready = self.event('app_ready', 40, attempt=2, success=True)
        result = self.query([self.event('run_started'), self.event('run_finished', 10,
            properties={'outcome':'failed'}), self.event('run_started', 20, attempt=2), ready, ready])[0]
        self.assertEqual(result['observed_attempts'], '2')
        self.assertEqual(result['failed_attempts'], '1')
        self.assertEqual(result['ready_attempts'], '1')
        self.assertEqual(result['failures_with_later_readiness_observation'], '1')
        self.assertEqual(result['median_seconds_to_ready'], '20.00')
        self.assertEqual(result['installations_ready_on_first_observed_attempt'], '0')

    def test_development_and_late_delivery(self):
        result = self.query([self.event('run_started', cli_version='dev-battery-watch'),
            self.event('run_finished', 5, attempt=2, success=True),
            self.event('app_ready', 10, created_at='2026-11-13')])[0]
        self.assertEqual(result['segment'], 'observed_development')
        self.assertEqual(result['ready_attempts'], '0')

    def test_first_observed_readiness(self):
        result = self.query([self.event('run_started'), self.event('app_ready', 5, success=True)])[0]
        self.assertEqual(result['installations_ready_on_first_observed_attempt'], '1')

    def test_legacy_and_missing_attempts_are_excluded(self):
        self.assertEqual(self.query([self.event('run_finished', schema_version=1),
            self.event('run_finished', attempt_id=None)]), [])

if __name__ == '__main__': unittest.main()
