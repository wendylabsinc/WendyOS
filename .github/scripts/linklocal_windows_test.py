#!/usr/bin/env python3
"""Portable host-layout rejection checks; no Windows interfaces are changed."""
import copy
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest import mock
import tempfile
import ctypes
import json

SCRIPT = Path(__file__).with_name('test-linklocal-windows.py')
spec = importlib.util.spec_from_file_location('linklocal_windows', SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

def snapshot():
    return {'adapters':[{'InterfaceIndex':7}], 'addresses':[{'IPAddress':'10.0.0.2'}],
            'wintun_devices':[], 'wintun_drivers':[],
            'routes':[{'DestinationPrefix':'0.0.0.0/0','InterfaceIndex':7},
                      {'DestinationPrefix':'169.254.169.254/32','InterfaceIndex':7}]}

class WindowsLinkLocalAdmissionTests(unittest.TestCase):
    def test_safe_metadata_host_route_is_preserved_not_created(self):
        state=snapshot()
        original=copy.deepcopy(state)
        module.validate_host(state)
        self.assertEqual(state,original)

    def test_existing_driver_or_orphan_is_refused_before_installation(self):
        for key in ('wintun_devices','wintun_drivers'):
            with self.subTest(key=key):
                state=snapshot();state[key]=[{'Name':'existing'}]
                with self.assertRaisesRegex(ValueError,'preexisting Wintun'):
                    module.validate_host(state)

    def test_missing_or_unowned_metadata_route_is_refused(self):
        for missing in (True,False):
            with self.subTest(missing=missing):
                state=snapshot()
                if missing: state['routes'].pop()
                else: state['routes'][-1]['InterfaceIndex']=8
                with self.assertRaises(ValueError): module.validate_host(state)

    def test_existing_linklocal_interface_or_overlapping_route_is_refused(self):
        for prefix in ('169.254.0.0/16','169.254.100.0/24','169.0.0.0/8','128.0.0.0/1'):
            with self.subTest(prefix=prefix):
                state=snapshot();state['routes'].append({'DestinationPrefix':prefix,'InterfaceIndex':7})
                with self.assertRaisesRegex(ValueError,'route could be affected'): module.validate_host(state)
        state=snapshot();state['addresses'].append({'IPAddress':'169.254.10.2'})
        with self.assertRaisesRegex(ValueError,'existing link-local addresses'): module.validate_host(state)

    def test_foreign_route_to_fixture_peer_is_refused(self):
        state=snapshot();state['routes'].append({'DestinationPrefix':'169.254.100.2/32','InterfaceIndex':7})
        with self.assertRaisesRegex(ValueError,'foreign host route'): module.validate_host(state)

    def test_other_explicit_linklocal_host_routes_remain_protected(self):
        state=snapshot();state['routes'].append({'DestinationPrefix':'169.254.169.123/32','InterfaceIndex':7})
        module.validate_host(state)

    def test_local_execution_refuses_before_files_downloads_or_interfaces(self):
        result=subprocess.run([sys.executable,str(SCRIPT),'/not-created-linklocal-fixture'],
            env=dict(os.environ,GITHUB_ACTIONS='',RUNNER_ENVIRONMENT=''),capture_output=True,text=True,timeout=3)
        self.assertEqual(result.returncode,2)
        self.assertIn('disposable hosted Windows runner',result.stderr)

class JobDrainTests(unittest.TestCase):
    def job(self, counts):
        job = module.Job.__new__(module.Job)
        job.handle = 42
        job.api = mock.Mock()
        remaining = iter(counts)
        def query(handle, kind, output, size, unused):
            self.assertEqual(handle, 42)
            self.assertEqual(kind, 1)
            self.assertEqual(size, 48)
            output._obj.ActiveProcesses = next(remaining)
            return True
        job.api.QueryInformationJobObject.side_effect = query
        job.api.TerminateJobObject.return_value = True
        return job

    def test_job_drain_observes_all_descendants_before_close(self):
        self.assertEqual(ctypes.sizeof(module.Accounting), 48)
        job = self.job([2, 1, 0])
        with mock.patch.object(module.time, 'sleep') as sleep:
            job.drain()
        sleep.assert_called_once_with(0.05)
        job.api.TerminateJobObject.assert_called_once_with(42, 125)
        job.api.CloseHandle.assert_not_called()
        job.close()
        job.api.CloseHandle.assert_called_once_with(42)

    def test_stuck_job_refuses_network_cleanup(self):
        job = self.job([2, 1])
        with mock.patch.object(module.time, 'monotonic', side_effect=[0, 9]):
            with self.assertRaisesRegex(module.JobDrainError, 'did not drain'):
                job.drain()
        job.api.CloseHandle.assert_not_called()

    def test_unobservable_or_unterminable_job_is_not_treated_as_empty(self):
        job = self.job([1])
        job.api.QueryInformationJobObject.side_effect = None
        job.api.QueryInformationJobObject.return_value = False
        with self.assertRaisesRegex(module.JobDrainError, 'Cannot observe'):
            job.drain()
        job = self.job([1])
        job.api.TerminateJobObject.return_value = False
        with self.assertRaisesRegex(module.JobDrainError, 'Cannot terminate'):
            job.drain()

    def test_network_cleanup_requires_drain_and_reports_refusal(self):
        with tempfile.TemporaryDirectory() as work:
            output = Path(work)
            with mock.patch.object(module, 'execute', side_effect=module.JobDrainError('stuck')), mock.patch.object(module, 'powershell') as cleanup:
                with self.assertRaises(module.JobDrainError):
                    module.execute_and_cleanup(Path('test.exe'), Path('plan.json'), output)
                cleanup.assert_not_called()
                self.assertIn('REFUSED', (output/'cleanup.log').read_text())
            with mock.patch.object(module, 'execute', return_value=1), mock.patch.object(module, 'powershell', return_value='clean') as cleanup:
                self.assertEqual(module.execute_and_cleanup(Path('test.exe'), Path('plan.json'), output), 1)
                cleanup.assert_called_once_with('Cleanup', Path('plan.json'), timeout=30)

    def test_execute_admits_only_after_job_assignment_and_drains_after_leader(self):
        with tempfile.TemporaryDirectory() as work:
            output = Path(work)
            plan = output/'plan.json'
            permit = output/'permit'
            plan.write_text(json.dumps({'permit': str(permit), 'nonce': 'owned'}))
            events = []
            process = mock.Mock()
            job = mock.Mock()
            def assign(child):
                self.assertIs(child, process)
                self.assertFalse(permit.exists())
                events.append('assign')
            def wait(**kwargs):
                self.assertEqual(permit.read_text(), 'owned')
                events.append('leader')
                return 0
            job.assign.side_effect = assign
            job.drain.side_effect = lambda: events.append('drain-all')
            job.close.side_effect = lambda: events.append('close-job')
            process.wait.side_effect = wait
            with mock.patch.object(module, 'Job', return_value=job), mock.patch.object(module.subprocess, 'Popen', return_value=process):
                self.assertEqual(module.execute(Path('test.exe'), plan, output), 0)
            self.assertEqual(events, ['assign', 'leader', 'drain-all', 'leader', 'close-job'])

if __name__=='__main__':unittest.main()
