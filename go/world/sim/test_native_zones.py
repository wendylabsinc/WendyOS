"""Session handoff tests; no renderer, detector, network or hardware required."""
import copy
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from sim.people_demo import PeopleDemo


class Worker:
    def __init__(self, result=None, failure=None, close_error=False):
        self.result, self.failure, self.close_error = result, failure, close_error
        self.closed = False
        self.calls = []
        self.process = SimpleNamespace(poll=lambda: 0)

    def call(self, request):
        self.calls.append(copy.deepcopy(request))
        if self.failure:
            raise self.failure
        return self.result

    def close(self):
        self.closed = True
        if self.close_error:
            raise OSError('injected reaping failure')


class NativeZonesTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.demo = PeopleDemo.__new__(PeopleDemo)
        d = self.demo
        d.out = Path(self.tmp.name)
        d.zone_reader, d.zone_trust = '/private/reader', '/private/trust'
        d.tracker_binary = Path('/private/world-people')
        d.run_id, d.frame = 'run-1', {'id': 'floor', 'revision': 'r1'}
        d.last_request = {'participant_id':'hub', 'run_id':d.run_id,
                          'frame':d.frame, 'images':[], 'zones':[{'old':True}]}
        d.last_tracking = {'zone_presence': {'events':[{'id':'historic'}]}}
        d.tracker = self.old = Worker()
        d.zones, d.cameras = ['old'], {'old':True}
        d.zone_selection_sequence = 2
        d.command_sequence, d.commands = 2, []
        d.scene = SimpleNamespace(data=SimpleNamespace(time=1.))
        self.target = {'sha256':'a'*64,'source':{'device':'operator','app':'zones','service':'catalog','stream':'main'},
                       'run_id':d.run_id,'frame':d.frame}

    def replacement(self):
        return Worker({'zone_catalog':dict(self.target, state='available_at_replica')})

    def test_verified_handoff_preserves_history_and_removes_inline_geometry(self):
        new = self.replacement()
        with patch('sim.people_demo.JSONProcess', return_value=new) as start:
            receipt = self.demo.command('zone_select', json.dumps(self.target))
        self.assertTrue(receipt['accepted'])
        self.assertEqual(receipt['sequence'],3)
        self.assertEqual(json.loads(receipt['target']),self.target)
        self.assertIs(self.demo.tracker,new)
        self.assertTrue(self.old.closed)
        self.assertFalse(new.closed)
        self.assertNotIn('zones',new.calls[0])
        self.assertIn('zones',self.demo.last_request)
        self.assertEqual(self.demo.zone_selection_sequence,3)
        self.assertEqual(json.loads((self.demo.out/'zone-history-before-3.json').read_text()),self.demo.last_tracking)
        self.assertNotIn('private_key',' '.join(start.call_args.args[0]))

    def test_missing_denied_or_mismatched_revision_keeps_prior_session(self):
        for result in ({}, {'zone_catalog':dict(self.target,state='expired_or_revoked')},
                       {'zone_catalog':dict(self.target,sha256='b'*64,state='available_at_replica')}):
            with self.subTest(result=result):
                new=Worker(result)
                with patch('sim.people_demo.JSONProcess',return_value=new):
                    receipt=self.demo.command('zone_select',json.dumps(self.target))
                self.assertFalse(receipt['accepted'])
                self.assertIs(self.demo.tracker,self.old)
                self.assertFalse(self.old.closed)
                self.assertTrue(new.closed)
                self.assertEqual(self.demo.zone_selection_sequence,2)

    def test_wrong_context_duplicate_fields_and_oversized_targets_are_atomic(self):
        wrong=copy.deepcopy(self.target);wrong['frame']['revision']='other'
        inputs=[json.dumps(wrong),json.dumps(dict(self.target,run_id='other')),
                json.dumps(self.target)[:-1]+',"sha256":"'+('b'*64)+'"}', 'x'*2049]
        with patch('sim.people_demo.JSONProcess') as start:
            for value in inputs:
                self.assertFalse(self.demo.command('zone_select',value)['accepted'])
            start.assert_not_called()
        self.assertIs(self.demo.tracker,self.old)
        self.assertEqual(list(self.demo.out.iterdir()),[])

    def test_history_write_failure_keeps_prior_and_closes_candidate(self):
        (self.demo.out/'zone-history-before-3.json').write_text('existing')
        new=self.replacement()
        with patch('sim.people_demo.JSONProcess',return_value=new):
            self.assertFalse(self.demo.command('zone_select',json.dumps(self.target))['accepted'])
        self.assertIs(self.demo.tracker,self.old)
        self.assertTrue(new.closed)
        self.assertFalse(self.old.closed)

    def test_old_process_reaping_failure_does_not_undo_verified_handoff(self):
        self.old.close_error=True
        new=self.replacement()
        with patch('sim.people_demo.JSONProcess',return_value=new):
            self.assertTrue(self.demo.command('zone_select',json.dumps(self.target))['accepted'])
        self.assertIs(self.demo.tracker,new)
        self.assertFalse(new.closed)

if __name__ == '__main__':
    unittest.main()
