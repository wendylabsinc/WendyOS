"""Offline fixtures and temporary loopback HTTP only; never a Wendy VM."""
import copy
import gzip
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import go2_scene_preflight as check
from verification_archive import archive_file

ROOT = Path(__file__).resolve().parents[1]


def fixtures():
    # Synthetic protocol fixture only. These identities are not a runtime receipt.
    status = dict(epoch=31, vm_name="test-simulator", source_digest="sha256:"+"c"*64,
                  policy_bundle="test-policy", simulation=True, robot_kind="go2",
                  mode="paused", armed=False, healthy=True, error=None, time=0,
                  command=[0,0,0], applied_command=[0,0,0],
                  project=dict(project_id="d"*32, revision="e"*64, epoch=31, error=None),
                  ros_commands=dict(owner=None, auto_control=False))
    scene = dict(id="c"*64, version=1, bodies=[dict(id=0,name="world"),dict(id=1,name="sandbox_obstacle")])
    state = dict(scene_id=scene["id"], epoch=31, generation=85, mode="paused", valid=True, time=0,
                 positions=[0,0,0,2.5,1.5,.4], quaternions=[1,0,0,0,1,0,0,0])
    pins = {k:status[k] for k in ('epoch','vm_name','source_digest','policy_bundle')}
    pins.update(project_id=status['project']['project_id'], project_revision=status['project']['revision'])
    return [status, scene, state, copy.deepcopy(state), copy.deepcopy(status)], pins


def archived_fixtures():
    status = json.loads(archive_file('go2-cloud-live/cleanup/status.json').read_text())
    state = json.loads(archive_file('go2-cloud-live/cleanup/scene-state.json').read_text())
    scene = json.loads(gzip.decompress(archive_file('go2-cloud-shadow/preparation/initial-scene.json.gz').read_bytes()))
    pins = {k:status[k] for k in ('epoch','vm_name','source_digest','policy_bundle')}
    pins.update(project_id=status['project']['project_id'], project_revision=status['project']['revision'])
    return [status, scene, state, copy.deepcopy(state), copy.deepcopy(status)], pins


class Fake:
    def __init__(self, values):
        self.values = values
        self.calls = []
        self.now = 100.0
    def clock(self):
        return self.now
    def get(self, runtime, path, deadline, maximum):
        self.calls.append(path)
        self.now += .01
        return check.encode(self.values[len(self.calls)-1])


class Tests(unittest.TestCase):
    def run_case(self, change=None, **kwargs):
        values, pins = fixtures()
        if change:
            change(values, pins)
        fake = Fake(values)
        report = check.preflight('http://127.0.0.1:8890', pins, 'sandbox_obstacle', [2.5,1.5,.4],
                                 fetch=fake.get, clock=fake.clock, **kwargs)
        return report, fake

    def test_synthetic_body_mapping_and_receipt(self):
        report, fake = self.run_case(scene_id=fixtures()[0][1]['id'], generation=85)
        self.assertEqual(fake.calls, list(check.PATHS))
        self.assertTrue(report['accepted'])
        self.assertFalse(report['operational_authority'])
        self.assertEqual(report['observed']['body_id'], 1)
        self.assertEqual(report['observed']['position_runtime_world_z_up_m'], [2.5,1.5,.4])
        self.assertEqual(report['observed']['generation'], 85)
        self.assertTrue(report['observed']['valid'])
        self.assertEqual(report['observed']['simulation_time_s'], 0)
        raw = check.bounded_output(report)
        self.assertLessEqual(len(raw), 16 << 10)
        self.assertNotIn(b'positions', raw)
        self.assertNotIn(b'quaternions', raw)
        for row in report['requests']:
            self.assertEqual(len(row['sha256']), 64)

    def test_actual_large_static_scene(self):
        values, pins = archived_fixtures()
        fake = Fake(values)
        result = check.preflight('http://127.0.0.1:8890', pins, 'sandbox_obstacle', [2.5,1.5,.4],fetch=fake.get,clock=fake.clock)
        self.assertGreater(result['requests'][1]['bytes'], 1 << 20)
        self.assertLess(len(check.bounded_output(result)), 16 << 10)

    def test_invalid_or_changed_observation(self):
        cases = {
            'reset_epoch':lambda v,p:v[2].update(epoch=32),
            'generation_changed':lambda v,p:v[3].update(generation=86),
            'invalid_state':lambda v,p:v[2].update(valid=False),
            'truthy_valid':lambda v,p:v[2].update(valid=1),
            'state_running':lambda v,p:v[2].update(mode='running'),
            'scene_changed':lambda v,p:v[2].update(scene_id='a'*64),
            'static_changed':lambda v,p:v[1].update(id='b'*64),
            'body_missing':lambda v,p:v[1]['bodies'][1].update(name='other'),
            'body_duplicate_id':lambda v,p:v[1]['bodies'][1].update(id=0),
            'body_duplicate_name':lambda v,p:v[1]['bodies'][1].update(name='world'),
            'body_negative_id':lambda v,p:v[1]['bodies'][1].update(id=-1),
            'body_bool_id':lambda v,p:v[1]['bodies'][1].update(id=True),
            'short_positions':lambda v,p:v[2].update(positions=[0,0,0]),
            'position_after_reset':lambda v,p:v[2]['positions'].__setitem__(3,4),
            'bool_position':lambda v,p:v[2]['positions'].__setitem__(3,True),
            'runtime_epoch':lambda v,p:v[4].update(epoch=32),
            'project_epoch_changes':lambda v,p:v[0]['project'].update(epoch=30),
            'project_epoch_missing':lambda v,p:v[0]['project'].pop('epoch'),
            'missing_owner':lambda v,p:v[0]['ros_commands'].pop('owner'),
            'project_revision':lambda v,p:v[4]['project'].update(revision='a'*64),
            'runtime_source':lambda v,p:v[0].update(source_digest='sha256:'+'a'*64),
            'runtime_vm':lambda v,p:v[0].update(vm_name='other'),
            'policy':lambda v,p:v[0].update(policy_bundle='other'),
            'physical':lambda v,p:v[0].update(simulation=False),
            'armed':lambda v,p:v[4].update(armed=True),
            'runtime_running':lambda v,p:v[4].update(mode='running'),
            'owner':lambda v,p:v[0]['ros_commands'].update(owner='writer'),
            'auto_control':lambda v,p:v[0]['ros_commands'].update(auto_control=True),
            'nonzero_target':lambda v,p:v[4].update(command=[.1,0,0]),
            'nonzero_applied':lambda v,p:v[4].update(applied_command=[.1,0,0]),
            'simulation_time_changed':lambda v,p:v[4].update(time=.1),
        }
        for name, change in cases.items():
            with self.subTest(name=name), self.assertRaises(check.Refused):
                self.run_case(change)

    def test_id_mapping_uses_id_not_array_order(self):
        report,_ = self.run_case(lambda values,pins:values[1]['bodies'].reverse())
        self.assertEqual(report['observed']['body_id'], 1)

    def test_archived_r_project_load_epoch_survives_runtime_reset(self):
        raw = gzip.decompress(archive_file('go2-cloud-live/trials/r/observer-samples.ndjson.gz').read_bytes())
        observed = json.loads(raw.splitlines()[0])['observation']
        self.assertEqual(observed['epoch'], 30)
        self.assertEqual(observed['project']['epoch'], 29)
        def same_project_new_runtime(values, pins):
            pins['epoch'] = observed['epoch']
            for at in (0,4):
                values[at]['epoch'] = observed['epoch']
                values[at]['project']['epoch'] = observed['project']['epoch']
            for at in (2,3):values[at]['epoch'] = observed['epoch']
        report,_ = self.run_case(same_project_new_runtime)
        self.assertEqual(report['observed']['epoch'], 30)
        self.assertEqual(report['observed_project_load_epoch'], 29)

    def test_optional_pins_and_expected_position_limits(self):
        for kwargs in ({'generation':86},{'generation':True},{'scene_id':'a'*64},{'tolerance_m':.1},{'timeout_s':10}):
            with self.subTest(kwargs=kwargs),self.assertRaises(check.Refused):self.run_case(**kwargs)
        values,pins=fixtures();fake=Fake(values)
        for pos in ([4,4,.4],[True,1.5,.4],[float('nan'),0,0]):
            with self.subTest(position=pos),self.assertRaises(check.Refused):
                check.preflight('http://127.0.0.1:8890',pins,'sandbox_obstacle',pos,fetch=fake.get,clock=fake.clock)

    def test_total_deadline_does_not_renew_between_requests(self):
        values,pins=fixtures();fake=Fake(values)
        def delayed(*args):
            raw=fake.get(*args);fake.now+=.35;return raw
        with self.assertRaises(check.Refused) as caught:
            check.preflight('http://127.0.0.1:8890',pins,'sandbox_obstacle',[2.5,1.5,.4],timeout_s=1,fetch=delayed,clock=fake.clock)
        self.assertEqual(caught.exception.reason,'total_deadline')
        self.assertEqual(len(fake.calls),3)

    def test_report_encoding_is_inside_total_deadline(self):
        values,pins=fixtures();fake=Fake(values)
        original=check.bounded_output
        def slow(report):
            raw=original(report);fake.now+=2;return raw
        with patch.object(check,'bounded_output',side_effect=slow),self.assertRaises(check.Refused) as caught:
            check.preflight('http://127.0.0.1:8890',pins,'sandbox_obstacle',[2.5,1.5,.4],fetch=fake.get,clock=fake.clock)
        self.assertEqual(caught.exception.reason,'total_deadline')

    def test_response_and_output_bounds(self):
        values,pins=fixtures()
        with self.assertRaises(check.Refused):
            check.preflight('http://127.0.0.1:8890',pins,'sandbox_obstacle',[2.5,1.5,.4],fetch=lambda *args:b' '*(check.LIMITS['/api/status']+1))
        with self.assertRaises(check.Refused):check.bounded_output({'too_large':'x'*check.MAX_OUTPUT})
        for raw in (b'{"epoch":31,"epoch":31}',b'{"value":NaN}'):
            with self.assertRaises(check.Refused):
                check.preflight('http://127.0.0.1:8890',pins,'sandbox_obstacle',[2.5,1.5,.4],fetch=lambda *args:raw)

    def test_cli_new_file_and_refusal_report(self):
        _,pins=fixtures()
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'receipt.json'
            args=['--runtime','http://127.0.0.1:8890','--body','sandbox_obstacle','--position','4','4','.4','--out',str(path)]
            for key,value in pins.items():args+=['--expected-'+key.replace('_','-'),str(value)]
            with patch.object(check,'preflight',side_effect=check.Refused('obstacle_position_mismatch')) as call:
                self.assertEqual(check.main(args),1)
                self.assertFalse(json.loads(path.read_text())['accepted'])
                self.assertEqual(path.stat().st_mode&0o777,0o600)
                self.assertEqual(check.main(args),1)
                self.assertEqual(call.call_count,1)

    def test_http_redirect_bound_and_delayed_body_refuse(self):
        requests=[]
        class Handler(BaseHTTPRequestHandler):
            mode='redirect'
            def log_message(self,*args):pass
            def do_GET(self):
                requests.append(self.path)
                if self.mode=='redirect':
                    self.send_response(302);self.send_header('Location','http://127.0.0.1:1/api/reset');self.send_header('Content-Length','2');self.end_headers();return
                self.send_response(200)
                self.send_header('Content-Length',str(check.LIMITS['/api/status']+1 if self.mode=='bound' else 2))
                self.end_headers()
                if self.mode=='delay':time.sleep(.15)
                try:self.wfile.write(b'{}')
                except OSError:pass
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            runtime='http://127.0.0.1:'+str(server.server_port)
            for mode in ('redirect','bound','delay'):
                Handler.mode=mode;started=time.monotonic()
                with self.subTest(mode=mode),self.assertRaises(check.Refused):
                    check.get(runtime,'/api/status',started+.05,check.LIMITS['/api/status'])
                self.assertLess(time.monotonic()-started,.5)
            self.assertEqual(requests,['/api/status']*3)
        finally:server.shutdown();server.server_close();thread.join()


if __name__=='__main__':unittest.main()
