"""Continuous launcher contract checks; no Agent, source process or native window."""
from concurrent.futures import Future
from collections import OrderedDict
from copy import deepcopy
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from integration.sharedinspection.live import ContinuousLiveSession, Runner, continuous_update
from integration.livetwin.run import LiveSession
from integration.livetwin.test_run import update
from sim.native_replica_bridge import NativeReplicaBridge, MAX_DISPLAY_AGE_NS
from sim.test_native_people_layer import Clock, people_fixture


def runner():
    config, presentation, hazard, people = people_fixture()
    config['presentation_mode'] = 'continuous'
    r = ContinuousLiveSession.__new__(ContinuousLiveSession)
    r.clock = Clock()
    r.mission_config = r.operator_forward = r.selection_worker = r.selection_pending = None
    r.viewer_forward = r.viewer_profile = None
    r.started_executors = 0
    def start(): r.started_executors += 1
    r.start_inspection_executor = start
    r.bridge = NativeReplicaBridge(config, presentation, monotonic_ns=r.clock)
    r.bridge.ingest(hazard); r.bridge.ingest_people(people)
    r.presentation = deepcopy(presentation)
    r.presentation_count = r.source_capture_sequence = r.source_sequence = 0
    r.simulation_ns = presentation['simulation_time_ns']
    r.enabled = [v['id'] for v in presentation['sensor_sources']]
    r.saved = {}
    r.save = lambda name, value: r.saved.setdefault(name, deepcopy(value))
    r.presentation_worker = r.presentation_future = None
    r.next_presentation_poll = 0
    return r, presentation


class ContinuousLauncherTests(unittest.TestCase):
    def test_window_receives_both_separate_private_profiles(self):
        r, _ = runner()
        r.executable, r.files, r.out, r.walkthrough = '/app', None, '/out', False
        r.mission_config = '/host/mission/config.json'
        r.viewer_profile = '/host/viewer/profile.json'
        with patch('integration.sharedinspection.live.InspectionWindow') as window:
            r.native_window('/session')
        self.assertEqual(window.call_args.kwargs, {
            'mission_config': r.mission_config, 'viewer_profile': r.viewer_profile})

    def test_hazard_selection_retains_only_accepted_originals_with_person_history(self):
        r, _ = runner()
        person = {'envelope_sha256':'a'*64, 'envelope_json':'person-original',
                  'mission_input':{'envelope_sha256':'a'*64, 'target':[1,.24,2]}}
        hazard = {'envelope_sha256':'b'*64, 'envelope_json':'hazard-original',
                  'mission_input':{'envelope_sha256':'b'*64, 'target':[3,.65,4]}}
        r.mission_originals = OrderedDict()
        r.retain_mission_originals([person])
        event = {'reader':{'status':'ready', 'hazards':[hazard]}}
        with patch.object(r.bridge, 'ingest', return_value=False):
            self.assertFalse(r.ingest_hazard_reader(event))
        self.assertEqual(list(r.mission_originals), ['a'*64])
        with patch.object(r.bridge, 'ingest', return_value=True):
            self.assertTrue(r.ingest_hazard_reader(event))
        self.assertEqual(list(r.mission_originals), ['a'*64, 'b'*64])
        self.assertEqual(r.mission_originals['b'*64], hazard)
        hazard['mission_input']['target'][1] = 0
        self.assertEqual(r.mission_originals['b'*64]['mission_input']['target'], [3,.65,4])
        self.assertEqual(r.mission_originals['a'*64], person)
        with self.assertRaisesRegex(ValueError, 'prepared mission original differs'):
            r.retain_mission_originals([{'envelope_sha256':'c'*64,
                                       'mission_input':{'envelope_sha256':'d'*64}}])

    def test_retained_original_limit_is_shared_between_result_kinds(self):
        r, _ = runner(); r.mission_originals = OrderedDict()
        for i in range(65):
            digest = '%064x' % i
            r.retain_mission_originals([{'envelope_sha256':digest,
                                       'mission_input':{'envelope_sha256':digest}}])
        self.assertEqual(len(r.mission_originals), 64)
        self.assertNotIn('0'*64, r.mission_originals)
        self.assertIn('%064x' % 64, r.mission_originals)

    def test_launcher_reuses_inspection_lifecycle_and_exact_app_binding(self):
        from integration.sharedinspection.run import Runner as Deployment
        from integration.sharedinspection import model
        from integration.sharedperception.test_live import runner_fixture
        self.assertTrue(issubclass(Runner, Deployment))
        self.assertTrue(issubclass(Runner, ContinuousLiveSession))
        r, presentation, hazard, people = runner_fixture()
        r.__class__ = Runner
        r.people_clock = people['reader']['clock']
        cfg = r.live_bridge_config(presentation, hazard['clock'])
        self.assertEqual(cfg['presentation_mode'], 'continuous')
        self.assertEqual(cfg['source']['app'], model.APP)
        self.assertEqual(cfg['people']['source']['app'], model.APP)
        self.assertEqual(cfg['reader_generation'], 9)
        self.assertEqual(cfg['people']['reader_generation'], 12)
        self.assertEqual({s['app'] for s in cfg['input_sources'].values()}, {model.APP})

    def test_window_records_valid_empty_view_without_qualifying_positive_results(self):
        from integration.sharedinspection.live import InspectionWindow
        from integration.sharedperception.live import SharedWindow
        window = InspectionWindow.__new__(InspectionWindow)
        state = {'snapshot':{'controls_available':True,'fusion':{'state':'no_live_result'},
                             'tracking':{'tracks':[]}}}
        self.assertTrue(window.ready(state))
        self.assertFalse(SharedWindow.ready(window,state))
        state['snapshot']['controls_available'] = False
        self.assertFalse(window.ready(state))
        state['snapshot']['tracking']['tracks'] = [{'id':'original current track'}]
        self.assertTrue(window.ready(state))
        self.assertFalse(SharedWindow.ready(window,state))

    def test_reader_keeps_its_clock_and_checkpoint_with_a_different_actual_frame(self):
        row = update(13, '2026-01-01T00:00:04.123456789Z')
        before = deepcopy(row)
        presentation = {'simulation_time_ns': 3_987_654_321}
        match, queued = continuous_update([row], presentation, None,
                                          row['collection_started_monotonic_ns']+100)
        self.assertIs(match[0], row)
        self.assertEqual(match[1], presentation)
        self.assertIsNot(match[1], presentation)
        self.assertIsNone(queued)
        self.assertEqual(row, before)

    def test_duplicate_wait_does_not_renew_and_newer_empty_supersedes_positive(self):
        row = update(13, '2026-01-01T00:00:04Z')
        start = row['collection_started_monotonic_ns']
        match, queued = continuous_update([row], None, None, start)
        self.assertIsNone(match); self.assertIs(queued, row)
        duplicate = deepcopy(row); duplicate['collection_started_monotonic_ns'] += 900_000_000
        self.assertEqual(continuous_update([duplicate], {'simulation_time_ns': 6}, queued,
                                          start+MAX_DISPLAY_AGE_NS), (None, None))
        empty = update(14, '2026-01-01T00:00:04.2Z'); empty['reader']['hazards'] = []
        match, _ = continuous_update([empty], {'simulation_time_ns': 4_250_000_000}, queued, start+500)
        self.assertIs(match[0], empty)
        with self.assertRaises(RuntimeError):
            continuous_update([update(15, '', status='failed')], {}, None, start+500)

    def test_capture_ack_may_skip_sequence_and_complete_later_without_rewriting_request(self):
        r, presentation = runner()
        request = dict(simulation_time_ns=1_000_000_001, sensors=r.enabled)
        response = dict(sequence=83, simulation_time_ns=1_040_000_000, sensors=r.enabled)
        presentation['simulation_time_ns'] = 1_060_000_000
        original = deepcopy((request, response, presentation, r.bridge.latest, r.bridge.people.latest))
        r.apply_source(request, response, presentation)
        self.assertEqual((r.source_sequence, r.source_capture_sequence, r.simulation_ns), (1, 83, 1_060_000_000))
        self.assertEqual(r.started_executors, 1)
        self.assertEqual(r.saved['live/source-0001.json'], dict(request=request, response=response))
        self.assertEqual((request, response, presentation, r.bridge.latest, r.bridge.people.latest), original)
        self.assertIsNone(r.periodic_source_request())
        self.assertEqual(r.source_command_minimum(), 1_060_000_001)
        with self.assertRaises(ValueError): r.apply_source(request, response, presentation)
        bad = {**response, 'sequence': 84, 'simulation_time_ns': 1_000_000_000}
        with self.assertRaises(ValueError): r.apply_source(request, bad, presentation)
        with self.assertRaises(ValueError): r.apply_source(request, {**response, 'sequence': 84, 'sensors': []}, presentation)
        # Existing source selection keeps its exact stepped response contract.
        with self.assertRaises(ValueError): LiveSession.apply_source(r, request, response, presentation)

    def test_completed_presentation_future_moves_physics_without_reader_or_capture_calls(self):
        r, presentation = runner()
        presentation['simulation_time_ns'] += 60_000_000
        presentation['dynamic_geoms'] = [{'id': 4, 'position': [-3.9, 1.8, .24], 'rotation': [1,0,0,0,1,0,0,0,1]}]
        completed = Future(); completed.set_result(presentation)
        next_read = Future()
        submits = []
        def submit(fn):
            submits.append(fn)
            return next_read
        r.presentation_worker = SimpleNamespace(submit=submit)
        r.presentation_future = completed
        original = deepcopy((r.bridge.latest, r.bridge.people.latest))
        r.poll_source_presentation()
        self.assertEqual(len(submits), 1)
        self.assertIs(r.presentation_future, next_read)
        state = r.bridge.export()
        self.assertEqual(state['dynamic_geoms'], presentation['dynamic_geoms'])
        self.assertEqual((r.bridge.latest, r.bridge.people.latest), original)
        self.assertEqual(r.presentation_count, 1)
        r.poll_source_presentation()  # Pending I/O never blocks the main loop.
        self.assertEqual(len(submits), 1)
        self.assertEqual(r.presentation_count, 1)
        r._record_presentation(presentation)
        self.assertEqual(r.presentation_count, 1)

    def test_continuous_presentation_does_not_make_failed_or_stale_reader_current(self):
        r, presentation = runner()
        r.bridge.latest = r.bridge.people.latest = None
        presentation['simulation_time_ns'] += 50_000_000
        r._record_presentation(presentation)
        state = r.bridge.export()['snapshot']
        self.assertEqual(state['fusion']['state'], 'unknown')
        self.assertEqual(state['tracking']['tracks'], [])
        self.assertFalse(state['controls_available'])

class OperatorBridgeTests(unittest.TestCase):
    def test_navigation_handoff_keeps_observation_and_passes_read_only_source_binding(self):
        import json
        r, _ = runner()
        r.mission_container = 'a'*64
        r.mission_directory = '/owned/mission/commissioned'
        r.mission_scene_read_token_file = '/owned/scene/scene-read-token'
        request = {'request_id':'12345678-1234-1234-1234-123456789abc',
                   'original_envelope_sha256':'a'*64}
        row = {'envelope_json':'exact-envelope', 'mission_input':{'target':[1,.65,2],
               'envelope_sha256':'a'*64}}
        handoff = {'version':1,'proposal_id':'native-'+request['request_id'],
                   'mission_input':deepcopy(row['mission_input']), 'original_envelope_sha256':'a'*64,
                   'navigation_target':[1.2,.24,2.1], 'inspection_destination':{'mode':'planar_nearby'},
                   'route_length_m':4.2,'nominal_travel_ms':12000}
        with patch('integration.sharedinspection.live.subprocess.run',
                   return_value=SimpleNamespace(returncode=0,stdout=json.dumps(handoff).encode())) as call:
            prepared = r._prepare_handoff(request,row)
        argv = call.call_args.args[0]
        self.assertEqual(argv[-1],r.mission_scene_read_token_file)
        self.assertNotIn('scene-control-token',' '.join(argv))
        self.assertEqual(json.loads(call.call_args.kwargs['input']),row['mission_input'])
        self.assertEqual(prepared['mission_input']['target'],[1,.65,2])
        self.assertEqual(prepared['navigation_target'],[1.2,.24,2.1])
        self.assertEqual(prepared['envelope_json'],row['envelope_json'])

    def test_pre_submit_navigation_refusal_is_distinct_from_unknown_proposal(self):
        from integration.sharedinspection.live import InspectionUnavailable
        r, _ = runner()
        r.mission_container='a'*64;r.mission_directory='/owned/mission'
        r.mission_scene_read_token_file='/owned/scene/scene-read-token'
        request={'request_id':'12345678-1234-1234-1234-123456789abc'}
        for code, exception in ((3,InspectionUnavailable),(1,ValueError)):
            with self.subTest(code=code), patch('integration.sharedinspection.live.subprocess.run',
                   return_value=SimpleNamespace(returncode=code,stdout=b'')):
                with self.assertRaises(exception):r._prepare_handoff(request,{'mission_input':{}})

    def test_unreachable_selection_reports_without_replacing_original_or_retrying(self):
        import json,tempfile
        from pathlib import Path
        from integration.sharedinspection.live import InspectionUnavailable,private_json
        r, _ = runner()
        with tempfile.TemporaryDirectory() as directory:
            directory=Path(directory);r.inputs={'run_id':'run-1'};r.mission_config=directory/'config.json'
            r.selection_request=directory/'request.json';r.selection_response=directory/'response.json';r.mission_handoff=directory/'handoff.json'
            request={'version':1,'request_id':'12345678-1234-1234-1234-123456789abc',
                     'run_id':'run-1','original_envelope_sha256':'a'*64}
            r.selection_ids={request['request_id']}
            r.mission_originals={'a'*64:{'mission_input':{'target':[1,.65,2]}}}
            before=deepcopy(r.mission_originals)
            future=Future();future.set_exception(InspectionUnavailable())
            r.selection_pending=(request,future)
            private_json(r.selection_request,request)
            r.poll_mission_selection();r.poll_mission_selection()
            self.assertEqual(json.loads(r.selection_response.read_bytes())['error'],'inspection_unavailable')
            self.assertEqual(r.mission_originals,before)
            self.assertIsNone(r.selection_pending)
            self.assertFalse(r.mission_handoff.exists())

    def test_owned_forward_moves_exact_bytes_and_closes_its_connection(self):
        import socket, subprocess, sys
        from integration.sharedinspection.live import OperatorForward, FORWARD_SCRIPT
        calls = []
        def child(argv, **kwargs):
            calls.append(argv)
            # Local subprocess fixture only; no Docker or native service.
            return subprocess.Popen([sys.executable,'-u','-c',
                'import os\nwhile True:\n b=os.read(0,16384)\n if not b:break\n os.write(1,b)'], **kwargs)
        forward = OperatorForward(process_factory=child)
        try:
            forward.configure('a'*64, '10.20.0.7')
            with socket.create_connection(('127.0.0.1', int(forward.url.rsplit(':',1)[1])), timeout=3) as connection:
                raw = b'POST /mission HTTP/1.1\r\nSignature: original\r\n\r\n\x00exact-body\xff'
                connection.sendall(raw)
                got = b''
                while len(got) < len(raw): got += connection.recv(16384)
                self.assertEqual(got, raw)
            self.assertEqual(calls[0], ['docker','exec','-i','a'*64,'python3','-c',FORWARD_SCRIPT,'10.20.0.7','8093'])
            with self.assertRaises(ValueError): forward.configure('b'*64,'10.20.0.8')
        finally:
            forward.close()
        self.assertFalse(forward.thread.is_alive())
        self.assertFalse(forward.flows)

    def test_explicit_viewer_port_moves_exact_tcp_bytes_without_docker(self):
        import socket, subprocess, sys, threading
        from integration.sharedinspection.live import OperatorForward, FORWARD_SCRIPT
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0)); listener.listen(1); listener.settimeout(5)
        port = listener.getsockname()[1]
        received = []
        def echo():
            with listener.accept()[0] as peer:
                while True:
                    raw = peer.recv(16384)
                    if not raw: break
                    received.append(raw); peer.sendall(raw)
        worker = threading.Thread(target=echo); worker.start()
        calls = []
        def child(argv, **kwargs):
            calls.append(argv)
            # Run the actual byte-forward script against an owned local server.
            return subprocess.Popen([sys.executable,'-u','-c',argv[6],*argv[7:]], **kwargs)
        forward = OperatorForward(process_factory=child)
        try:
            for invalid in (0,65536,True,'8094'):
                with self.assertRaises(ValueError): forward.configure('b'*64,'127.0.0.1',invalid)
            forward.configure('b'*64,'127.0.0.1',port)
            with socket.create_connection(('127.0.0.1',int(forward.url.rsplit(':',1)[1])),timeout=5) as local:
                raw = b'GET /v1/components HTTP/1.1\r\nAuthorization: synthetic\r\n\r\n\x00\xff'
                local.sendall(raw)
                got = b''
                while len(got) < len(raw):
                    chunk = local.recv(16384)
                    self.assertTrue(chunk)
                    got += chunk
                self.assertEqual(got,raw)
            self.assertEqual(calls[0],['docker','exec','-i','b'*64,'python3','-c',FORWARD_SCRIPT,'127.0.0.1',str(port)])
            worker.join(timeout=5)
            self.assertEqual(b''.join(received),raw)
        finally:
            forward.close(); listener.close(); worker.join(timeout=5)
        self.assertFalse(worker.is_alive())

    def test_both_host_urls_precede_commission_and_forward_to_exact_nodes(self):
        from unittest.mock import patch
        from integration.sharedperception.live import CombinedLiveSession
        r, _ = runner()
        r.nodes = {k:SimpleNamespace(target=lambda k=k:k*64,
                                    service_volume=lambda service,k=k:'/owned/'+k+'/'+service)
                   for k in ('a','b')}
        r.tasks = {'a/mission-gateway':{'ipv4':'10.20.0.7'},'b/viewer':{'ipv4':'10.30.0.8'}}
        created, events = [], []
        class Forward:
            def __init__(self):
                self.url = 'http://127.0.0.1:%d' % (19000+len(created)); created.append(self)
            def configure(self,container,address,port=8093): self.target=(container,address,port)
        def commission(current):
            events.append('commission')
            self.assertEqual((current.operator_url,current.viewer_url),tuple(f.url for f in created))
        r._export_operator = lambda:events.append('operator-export')
        r._export_viewer = lambda:events.append('viewer-export')
        with patch('integration.sharedinspection.live.OperatorForward',Forward), \
             patch.object(CombinedLiveSession,'commission_missions',commission,create=True):
            r.commission_missions()
        self.assertEqual(created[0].target,('a'*64,'10.20.0.7',8093))
        self.assertEqual(created[1].target,('b'*64,'10.30.0.8',8094))
        self.assertEqual(r.mission_scene_read_token_file,'/owned/a/scene/scene-read-token')
        self.assertEqual(events,['commission','operator-export','viewer-export'])

    def test_viewer_export_preserves_private_profile_without_logging_credentials(self):
        import json,stat,tempfile
        from pathlib import Path
        from unittest.mock import patch
        from integration.sharedinspection.live import VIEWER_EXPORT_SCRIPT
        r, _ = runner()
        with tempfile.TemporaryDirectory() as directory:
            r.out = Path(directory); r.nodes = {'a':SimpleNamespace(target=lambda:'a'*64)}
            r.mission_directory='/owned/mission/commissioned'; r.viewer_url='http://127.0.0.1:19001'
            r.tasks={'b/viewer':{'ipv4':'10.30.0.8','generation':17}}
            raw=json.dumps({'name':'Native viewer','read_mode':'native_inference','credentials':{
                'url':r.viewer_url,'audience':'sharedinspection-native-viewer','private_key':'synthetic-secret'}}).encode()
            with patch('integration.sharedinspection.live.subprocess.run',return_value=SimpleNamespace(returncode=0,stdout=raw)) as call:
                r._export_viewer()
            self.assertEqual(r.viewer_profile.read_bytes(),raw)
            self.assertEqual(stat.S_IMODE(r.viewer_profile.stat().st_mode),0o600)
            self.assertEqual(stat.S_IMODE(r.viewer_profile.parent.stat().st_mode),0o700)
            self.assertEqual(list(r.viewer_profile.parent.iterdir()),[r.viewer_profile])
            self.assertEqual(call.call_args.args[0],['docker','exec','a'*64,'python3','-c',VIEWER_EXPORT_SCRIPT,r.mission_directory])
            self.assertEqual(call.call_args.kwargs['stderr'],__import__('subprocess').DEVNULL)
            receipt=r.saved['live/viewer-bridge.json']
            self.assertEqual(receipt['viewer_task'],r.tasks['b/viewer'])
            self.assertFalse(receipt['request_readiness_verified'])
            self.assertFalse(receipt['android_endpoint_configured'])
            self.assertEqual(receipt['address_scope'],'host-loopback-only')
            self.assertNotIn('synthetic-secret',json.dumps(r.saved))
            self.assertEqual(VIEWER_EXPORT_SCRIPT.count("'/viewer-profile.json'"),1)
            self.assertNotIn('operator.json',VIEWER_EXPORT_SCRIPT)

    def test_viewer_export_refuses_wrong_profile_or_oversize_before_writing(self):
        import json,tempfile
        from pathlib import Path
        from unittest.mock import patch
        for change in ('url','audience','read_mode','extra','size','failed'):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                r,_=runner();r.out=Path(directory);r.nodes={'a':SimpleNamespace(target=lambda:'a'*64)}
                r.mission_directory='/owned/mission';r.viewer_url='http://127.0.0.1:19001'
                profile={'name':'Native viewer','read_mode':'native_inference','credentials':{
                    'url':r.viewer_url,'audience':'sharedinspection-native-viewer'}}
                if change in ('url','audience'):profile['credentials'][change]='other'
                if change=='read_mode':profile['read_mode']='full'
                if change=='extra':profile['operator']={'private_key':'not-exported'}
                if change=='size':profile['name']='x'*(64<<10)
                raw=json.dumps(profile).encode()
                with patch('integration.sharedinspection.live.subprocess.run',return_value=SimpleNamespace(returncode=int(change=='failed'),stdout=raw)):
                    with self.assertRaises(ValueError):r._export_viewer()
                self.assertFalse((r.out/'viewer-client').exists());self.assertEqual(r.saved,{})

    def test_viewer_forward_closes_even_if_operator_close_fails(self):
        r,_=runner();events=[]
        def fail():events.append('operator');raise RuntimeError('owned operator close failed')
        r.operator_forward=SimpleNamespace(close=fail)
        r.viewer_forward=SimpleNamespace(close=lambda:events.append('viewer'))
        with self.assertRaises(RuntimeError):r.close_source_presentation()
        self.assertEqual(events,['operator','viewer'])

    def test_export_preserves_private_bytes_and_relocates_every_client_path(self):
        import base64,json,os,stat,tempfile
        from pathlib import Path
        from unittest.mock import patch
        r, _ = runner()
        with tempfile.TemporaryDirectory() as directory:
            r.out = Path(directory)
            r.nodes = {'a':SimpleNamespace(target=lambda:'a'*64)}
            r.mission_directory = '/owned/gateway/commissioned'
            r.operator_url = 'http://127.0.0.1:19283'
            r.tasks = {'a/mission-gateway':{'ipv4':'10.20.0.7'}}
            blobs = {'operator.json':json.dumps({'url':r.operator_url,'private_key':'test-secret'}).encode(),
                     'evidence-reader.json':json.dumps({'url':r.operator_url,'private_key':'separate-reader-secret'}).encode(),
                     'mission-context.json':b'{"test":"original context bytes"}',
                     'mission-clock-trust.json':b'{"test":"original pins"}'}
            packed = {name:base64.b64encode(raw).decode() for name,raw in blobs.items()}
            result = SimpleNamespace(returncode=0,stdout=json.dumps(packed).encode())
            with patch('integration.sharedinspection.live.subprocess.run',return_value=result) as call:
                r._export_operator()
            cfg = json.loads(r.mission_config.read_bytes())
            self.assertEqual(cfg['profile'],{'kind':'planar','floor_y':.24})
            for field,name in [('credentials_file','operator.json'),('evidence_credentials_file','evidence-reader.json'),('context_file','mission-context.json'),('clock_pins_file','mission-clock-trust.json')]:
                path = Path(cfg[field]); self.assertTrue(path.is_absolute())
                self.assertEqual(path.read_bytes(),blobs[name])
                self.assertEqual(stat.S_IMODE(path.stat().st_mode),0o600)
            self.assertEqual(stat.S_IMODE(r.mission_config.stat().st_mode),0o600)
            self.assertEqual(stat.S_IMODE(Path(cfg['journal_directory']).stat().st_mode),0o700)
            self.assertNotIn('test-secret',json.dumps(r.saved))
            self.assertNotIn('separate-reader-secret',json.dumps(r.saved))
            self.assertEqual(call.call_args.kwargs['stderr'],__import__('subprocess').DEVNULL)

    def test_evidence_export_requires_its_exact_file_and_observed_endpoint(self):
        import base64,json,tempfile
        from pathlib import Path
        from unittest.mock import patch
        for change in ('missing-reader','other-endpoint','extra-secret'):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                r, _ = runner()
                r.out = Path(directory)
                r.nodes = {'a':SimpleNamespace(target=lambda:'a'*64)}
                r.mission_directory = '/owned/gateway/commissioned'
                r.operator_url = 'http://127.0.0.1:19283'
                blobs = {'operator.json':json.dumps({'url':r.operator_url}).encode(),
                         'evidence-reader.json':json.dumps({'url':r.operator_url}).encode(),
                         'mission-context.json':b'{}','mission-clock-trust.json':b'{}'}
                if change == 'missing-reader': del blobs['evidence-reader.json']
                elif change == 'other-endpoint': blobs['evidence-reader.json'] = b'{"url":"http://127.0.0.1:9999"}'
                else: blobs['executor.json'] = b'{"private_key":"not-exported"}'
                packed = {name:base64.b64encode(raw).decode() for name,raw in blobs.items()}
                with patch('integration.sharedinspection.live.subprocess.run', return_value=SimpleNamespace(returncode=0,stdout=json.dumps(packed).encode())):
                    with self.assertRaises(ValueError): r._export_operator()
                self.assertFalse((r.out/'mission-client').exists())
                self.assertEqual(r.saved,{})

    def test_exact_selection_is_prepared_once_while_new_originals_arrive(self):
        import json,tempfile
        from pathlib import Path
        from collections import OrderedDict
        from integration.sharedinspection.live import private_json
        r, _ = runner()
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            r.inputs = {'run_id':'run-1'}
            r.mission_config = directory/'config.json'
            r.selection_request = directory/'request.json'; r.selection_response = directory/'response.json'
            r.mission_handoff = directory/'handoff.json'
            r.selection_ids = set(); r.selection_pending = None
            old = {'envelope_sha256':'a'*64,'envelope_json':'exact-original',
                   'mission_input':{'version':1,'envelope_sha256':'a'*64,'trigger_json':'original-canonical'}}
            r.mission_originals = OrderedDict([('a'*64,deepcopy(old))])
            future = Future(); calls=[]
            def submit(fn, request, row): calls.append((request,row)); return future
            r.selection_worker = SimpleNamespace(submit=submit)
            request = {'version':1,'request_id':'12345678-1234-1234-1234-123456789abc',
                       'run_id':'run-1','original_envelope_sha256':'a'*64}
            private_json(r.selection_request,request)
            r.poll_mission_selection()
            self.assertEqual(len(calls),1); self.assertEqual(calls[0][1],old)
            r.mission_originals.clear()  # Changing live observations cannot change the retained selection.
            r.poll_mission_selection(); self.assertEqual(len(calls),1)
            handoff={'version':1,'request_id':request['request_id'],'mission_input':old['mission_input'],
                     'original_envelope_sha256':'a'*64,'envelope_json':old['envelope_json']}
            future.set_result(handoff)
            r.poll_mission_selection(); r.poll_mission_selection()
            self.assertEqual(len(calls),1)
            self.assertEqual(json.loads(r.mission_handoff.read_bytes()),handoff)
            self.assertEqual(json.loads(r.selection_response.read_bytes())['status'],'ready')
            self.assertNotIn('original-canonical',json.dumps(r.saved))

    def test_unknown_proposal_or_missing_selection_is_not_retried(self):
        import json,tempfile
        from pathlib import Path
        from collections import OrderedDict
        from integration.sharedinspection.live import private_json
        r, _ = runner()
        with tempfile.TemporaryDirectory() as directory:
            directory=Path(directory);r.inputs={'run_id':'run-1'};r.mission_config=directory/'config.json'
            r.selection_request=directory/'request.json';r.selection_response=directory/'response.json';r.mission_handoff=directory/'handoff.json'
            r.selection_ids=set();r.selection_pending=None;r.mission_originals=OrderedDict()
            request={'version':1,'request_id':'12345678-1234-1234-1234-123456789abc','run_id':'run-1','original_envelope_sha256':'a'*64}
            private_json(r.selection_request,request);r.poll_mission_selection()
            self.assertEqual(json.loads(r.selection_response.read_bytes())['error'],'selection_unavailable')
            r.mission_originals['a'*64]={'mission_input':{'exact':'old'}}
            r.poll_mission_selection();self.assertIsNone(r.selection_pending)
            request['request_id']='12345678-1234-1234-1234-123456789abd'
            future=Future();future.set_exception(RuntimeError('lost reply'))
            calls=[]
            r.selection_worker=SimpleNamespace(submit=lambda *args:(calls.append(args),future)[1])
            private_json(r.selection_request,request);r.poll_mission_selection();r.poll_mission_selection();r.poll_mission_selection()
            self.assertEqual(len(calls),1)
            self.assertEqual(json.loads(r.selection_response.read_bytes())['error'],'unknown_outcome')
            self.assertFalse(r.mission_handoff.exists())
