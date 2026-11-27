"""Continuous shared-physics presentations with ordinary native reader overlays.

Only source sensor-selection controls are exposed here. Signed inspection motion
continues through the separate NodeGateway/executor route. Reader clocks and
original results never come from the presentation poller.
"""
from __future__ import annotations

import argparse
import base64
from collections import OrderedDict
import ipaddress
import json
import os
import re
import socket
import stat
import subprocess
from concurrent.futures import ThreadPoolExecutor
from copy import deepcopy
from pathlib import Path
import threading

from integration.sharedperception.live import CombinedLiveSession, SharedWindow, bridge_config
from sim.native_replica_bridge import MAX_DISPLAY_AGE_NS, integer, require


def continuous_update(updates, presentation, queued, now_ns):
    """Select one new reader event without requiring its clock to equal a frame."""
    newest = queued
    def order(update):
        reader = update['reader']
        return reader['tick'], reader['status'] == 'ready', reader.get('startup_attempt', 0)
    for update in updates:
        if update['reader']['status'] == 'failed':
            raise RuntimeError('ordinary native reader failed')
        if newest is None or order(update) > order(newest):
            newest = update
    if newest is None or not 0 <= now_ns-newest['collection_started_monotonic_ns'] < MAX_DISPLAY_AGE_NS:
        return None, None
    if presentation is None or newest['reader'].get('clock') is None:
        return None, newest
    return (newest, deepcopy(presentation)), None


# The ordinary signed HTTP messages pass through unchanged. The peer process is
# always inside this run's owned container and dials only its observed service.
FORWARD_SCRIPT = r'''
import os,select,socket,sys,time
s=socket.create_connection((sys.argv[1],int(sys.argv[2])),5);s.setblocking(False)
end=time.monotonic()+120;reading=True
while time.monotonic()<end:
 ready,_,_=select.select(([0] if reading else [])+[s],[],[],min(30,end-time.monotonic()))
 if not ready:break
 if 0 in ready:
  raw=os.read(0,16384)
  if not raw:reading=False;s.shutdown(socket.SHUT_WR)
  else:s.setblocking(True);s.settimeout(5);s.sendall(raw);s.setblocking(False)
 if s in ready:
  raw=s.recv(16384)
  if not raw:break
  while raw:
   written=os.write(1,raw);raw=raw[written:]
s.close()
'''
EXPORT_SCRIPT = r'''
import base64,json,os,stat,sys
root=sys.argv[1];result={}
for name in ('operator.json','evidence-reader.json','mission-context.json','mission-clock-trust.json'):
 fd=os.open(root+'/'+name,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb') as f:
  st=os.fstat(f.fileno());assert stat.S_ISREG(st.st_mode) and st.st_uid==0 and st.st_nlink==1 and not st.st_mode&0o077
  raw=f.read((128<<10)+1);assert 0<len(raw)<=128<<10
 result[name]=base64.b64encode(raw).decode()
sys.stdout.write(json.dumps(result))
'''
VIEWER_EXPORT_SCRIPT = r'''
import os,stat,sys
fd=os.open(sys.argv[1]+'/viewer-profile.json',os.O_RDONLY|os.O_NOFOLLOW)
with os.fdopen(fd,'rb') as f:
 st=os.fstat(f.fileno());assert stat.S_ISREG(st.st_mode) and st.st_uid==0 and st.st_nlink==1 and not st.st_mode&0o077
 raw=f.read((64<<10)+1);assert 0<len(raw)<=64<<10
sys.stdout.buffer.write(raw)
'''
PROPOSE_SCRIPT = r'''
import os,subprocess,sys
root,tool,request_id,read_token=sys.argv[1:];path=root+'/selection-'+request_id
os.mkdir(path,0o700)
raw=sys.stdin.buffer.read((768<<10)+1);assert 0<len(raw)<=768<<10
fd=os.open(path+'/input.json',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,'wb') as f:f.write(raw)
p=subprocess.run([tool,'propose','--credentials',root+'/proposer.json','--mission-input',path+'/input.json',
 '--handoff',path+'/handoff.json','--id','native-'+request_id,
 '--navigation-directory',root,'--source-read-token-file',read_token],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=8)
if p.returncode==3:sys.exit(3)
if p.returncode:sys.exit(1)
fd=os.open(path+'/handoff.json',os.O_RDONLY|os.O_NOFOLLOW)
with os.fdopen(fd,'rb') as f:
 raw=f.read((512<<10)+1);assert 0<len(raw)<=512<<10
sys.stdout.buffer.write(raw)
'''


class InspectionUnavailable(Exception):
    """The owned proposer refused navigation before submitting a proposal."""


def private_json(path, value):
    """Replace only this launcher's owned private control file."""
    raw = json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()
    temporary = path.with_name(path.name+'.tmp')
    fd = os.open(str(temporary), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(raw)
        os.replace(str(temporary), str(path))
    finally:
        if temporary.exists():
            temporary.unlink()


class OperatorForward:
    """One owned loopback listener; at most eight transparent byte streams."""
    def __init__(self, *, process_factory=subprocess.Popen):
        self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.listener.bind(('127.0.0.1', 0)); self.listener.listen(8); self.listener.settimeout(.2)
        self.url = 'http://127.0.0.1:%d' % self.listener.getsockname()[1]
        self.process_factory = process_factory
        self.destination = None
        self.done = threading.Event()
        self.lock = threading.Lock()
        self.flows, self.threads = {}, []
        self.thread = threading.Thread(target=self._serve, name='inspection-operator-forward', daemon=True)
        self.thread.start()

    def configure(self, container, address, port=8093):
        require(self.destination is None and isinstance(container, str) and re.fullmatch('[0-9a-f]{64}', container),
                'one observed owned container required')
        require(str(ipaddress.IPv4Address(address)) == address, 'observed gateway IPv4 required')
        require(type(port) is int and 1 <= port <= 65535, 'observed service port required')
        self.destination = (container, address, port)

    def _serve(self):
        while not self.done.is_set():
            try:
                local, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            with self.lock:
                if self.destination is None or len(self.flows) >= 8 or self.done.is_set():
                    local.close(); continue
                self.flows[local] = None
                worker = threading.Thread(target=self._flow, args=(local,), daemon=True)
                self.threads = [t for t in self.threads if t.is_alive()]
                self.threads.append(worker)
                worker.start()

    def _flow(self, local):
        process = upload = None
        try:
            container, address, port = self.destination
            process = self.process_factory(['docker','exec','-i',container,'python3','-c',FORWARD_SCRIPT,address,str(port)],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
            with self.lock:
                self.flows[local] = process
            if self.done.is_set():
                return
            def send():
                try:
                    while True:
                        raw = local.recv(16384)
                        if not raw: break
                        process.stdin.write(raw); process.stdin.flush()
                except (OSError, ValueError):
                    pass
                finally:
                    try: process.stdin.close()
                    except OSError: pass
            upload = threading.Thread(target=send, daemon=True); upload.start()
            while True:
                raw = os.read(process.stdout.fileno(), 16384)
                if not raw: break
                local.sendall(raw)
        except (OSError, ValueError):
            pass
        finally:
            try: local.shutdown(socket.SHUT_RDWR)
            except OSError: pass
            local.close()
            if process is not None:
                if process.poll() is None: process.kill()
                process.wait(timeout=5)
                process.stdout.close()
                if upload is None:
                    process.stdin.close()
            if upload is not None: upload.join(timeout=5)
            with self.lock: self.flows.pop(local, None)

    def close(self):
        self.done.set(); self.listener.close()
        with self.lock:
            flows = list(self.flows.items())
        for local, process in flows:
            try: local.shutdown(socket.SHUT_RDWR)
            except OSError: pass
            local.close()
            if process is not None and process.poll() is None: process.kill()
        self.thread.join(timeout=3)
        for worker in self.threads: worker.join(timeout=6)
        require(not self.thread.is_alive() and not any(t.is_alive() for t in self.threads),
                'owned operator forward did not stop')


class InspectionWindow(SharedWindow):
    def ready(self, state):
        # Record the loaded interactive view before both result layers coexist.
        # The inherited final capture qualification still requires actual
        # current hazard and track originals together.
        snapshot = state['snapshot']
        return bool(snapshot.get('controls_available') or snapshot.get('tracking', {}).get('tracks'))


class ContinuousLiveSession(CombinedLiveSession):
    def __init__(self, *args, **kwargs):
        self.operator_forward = self.mission_config = None
        self.mission_scene_read_token_file = None
        self.viewer_forward = self.viewer_profile = None
        self.selection_worker = self.selection_pending = None
        self.selection_ids = set()
        self.mission_originals = OrderedDict()
        self.source_request_lock = threading.Lock()
        self.presentation_worker = self.presentation_future = None
        self.presentation_count = self.source_capture_sequence = 0
        self.next_presentation_poll = 0
        super().__init__(*args, **kwargs)

    def commission_missions(self):
        # Bind the actual host endpoint before commissioning operator credentials.
        # No gateway address or port-forward readiness is guessed.
        require(not getattr(self, 'operator_url', ''), 'this launcher owns its operator endpoint')
        require(not getattr(self, 'viewer_url', ''), 'this launcher owns its viewer endpoint')
        self.operator_forward = OperatorForward()
        self.operator_url = self.operator_forward.url
        self.viewer_forward = OperatorForward()
        self.viewer_url = self.viewer_forward.url
        super().commission_missions()
        a = self.nodes['a']
        self.mission_container = a.target()
        self.mission_scene_read_token_file = a.service_volume('scene')+'/scene-read-token'
        self.operator_forward.configure(self.mission_container, self.tasks['a/mission-gateway']['ipv4'])
        self.viewer_forward.configure(self.nodes['b'].target(), self.tasks['b/viewer']['ipv4'], 8094)
        self._export_operator()
        self._export_viewer()

    def _export_viewer(self):
        from . import model
        completed = subprocess.run(['docker','exec',self.nodes['a'].target(),'python3','-c',
                                   VIEWER_EXPORT_SCRIPT,self.mission_directory],
                                  stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=8)
        require(completed.returncode == 0, 'private viewer export failed')
        raw = completed.stdout
        profile = model.strict_json(raw, 64 << 10)
        require(isinstance(profile, dict) and set(profile) == {'name','read_mode','credentials'} and
                isinstance(profile['name'], str) and profile['name'] and
                profile['read_mode'] == 'native_inference' and isinstance(profile['credentials'], dict) and
                profile['credentials'].get('url') == self.viewer_url and
                profile['credentials'].get('audience') == 'sharedinspection-native-viewer',
                'exact commissioned viewer endpoint/profile required')
        directory = self.out/'viewer-client'; directory.mkdir(mode=0o700)
        self.viewer_profile = directory/'profile.json'
        fd = os.open(str(self.viewer_profile), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as stream: stream.write(raw)
        self.save('live/viewer-bridge.json', {'version':1, 'viewer_url':self.viewer_url,
            'viewer_task':self.tasks['b/viewer'], 'transport':'owned-loopback-byte-forward',
            'target_node':'b', 'target_port':8094, 'max_connections':8,
            'host_profile':str(self.viewer_profile), 'credentials_logged':False,
            'address_scope':'host-loopback-only', 'android_endpoint_configured':False,
            'request_readiness_verified':False})

    def _export_operator(self):
        from . import model
        a = self.nodes['a']
        completed = subprocess.run(['docker','exec',a.target(),'python3','-c',EXPORT_SCRIPT,self.mission_directory],
                                   stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=8)
        require(completed.returncode == 0, 'private operator export failed')
        packed = model.strict_json(completed.stdout, 512 << 10)
        names = {'operator.json','evidence-reader.json','mission-context.json','mission-clock-trust.json'}
        require(set(packed) == names, 'exact operator export required')
        raw = {name: base64.b64decode(packed[name], validate=True) for name in names}
        require(all(0 < len(value) <= 128 << 10 for value in raw.values()), 'bounded operator files required')
        require(model.strict_json(raw['operator.json'])['url'] == self.operator_url, 'operator endpoint differs')
        require(model.strict_json(raw['evidence-reader.json'])['url'] == self.operator_url, 'evidence reader endpoint differs')
        directory = self.out/'mission-client'; directory.mkdir(mode=0o700)
        for name, value in raw.items():
            fd = os.open(str(directory/name), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, 'wb') as stream: stream.write(value)
        (directory/'journal').mkdir(mode=0o700)
        self.selection_request = directory/'selection-request.json'
        self.selection_response = directory/'selection-response.json'
        self.mission_handoff = directory/'handoff.json'
        self.mission_config = directory/'config.json'
        private_json(self.mission_config, {'version':1, 'profile':{'kind':'planar','floor_y':.24},
            'credentials_file':str(directory/'operator.json'), 'context_file':str(directory/'mission-context.json'),
            'evidence_credentials_file':str(directory/'evidence-reader.json'),
            'clock_pins_file':str(directory/'mission-clock-trust.json'), 'journal_directory':str(directory/'journal'),
            'handoff_file':str(self.mission_handoff), 'selection_request_file':str(self.selection_request),
            'selection_response_file':str(self.selection_response)})
        self.save('live/mission-bridge.json', {'version':1, 'operator_url':self.operator_url,
            'gateway_task':self.tasks['a/mission-gateway'], 'transport':'owned-loopback-byte-forward',
            'max_connections':8, 'mission_signed':False, 'credentials_logged':False,
            'host_config':str(self.mission_config), 'selection':'exact retained ordinary reader original'})

    def ingest_additional_readers(self):
        selected = self.selected_people
        before = self.people_ready_updates
        super().ingest_additional_readers()
        if self.people_ready_updates == before or selected is None:
            return
        self.retain_mission_originals(selected[0]['reader']['tracks'])

    def ingest_hazard_reader(self, update):
        accepted = super().ingest_hazard_reader(update)
        if accepted and update['reader']['status'] == 'ready':
            self.retain_mission_originals(update['reader']['hazards'])
        return accepted

    def retain_mission_originals(self, rows):
        for row in rows:
            if 'mission_input' in row:
                original = row['envelope_sha256']
                require(row['mission_input']['envelope_sha256'] == original, 'prepared mission original differs')
                self.mission_originals[original] = deepcopy(row)
                self.mission_originals.move_to_end(original)
                while len(self.mission_originals) > 64:
                    self.mission_originals.popitem(last=False)

    def _prepare_handoff(self, request, row):
        from . import model
        raw = model.json_bytes(row['mission_input'])
        result = subprocess.run(['docker','exec','-i',self.mission_container,'python3','-c',PROPOSE_SCRIPT,
            self.mission_directory, model.TOOLS+'/mission-commissioner', request['request_id'],
            self.mission_scene_read_token_file],
            input=raw, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=20)
        if result.returncode == 3:
            raise InspectionUnavailable()
        require(result.returncode == 0, 'proposal outcome unknown; no automatic retry')
        handoff = model.strict_json(result.stdout, 512 << 10)
        require(handoff.get('version') == 1 and handoff.get('proposal_id') == 'native-'+request['request_id'] and
                handoff.get('mission_input') == row['mission_input'] and
                handoff.get('original_envelope_sha256') == request['original_envelope_sha256'],
                'proposal handoff differs; no automatic retry')
        return {**handoff, 'request_id':request['request_id'], 'envelope_json':row['envelope_json']}

    def _selection_reply(self, request, *, error=None, handoff=None):
        reply = {'version':1, 'request_id':request['request_id'],
                 'original_envelope_sha256':request['original_envelope_sha256'],
                 'status':'failed' if error else 'ready'}
        if handoff is not None:
            private_json(self.mission_handoff, handoff)
        if error:
            reply['error'] = error
        private_json(self.selection_response, reply)
        # These receipts carry no HTTP request, signature, credentials or trigger bytes.
        self.save('live/mission-selection-%02d.json' % len(self.selection_ids), reply)

    def poll_mission_selection(self):
        from . import model
        if self.mission_config is None:
            return
        if self.selection_pending is not None:
            request, future = self.selection_pending
            if not future.done():
                return
            self.selection_pending = None
            try:
                handoff = future.result()
            except InspectionUnavailable:
                self._selection_reply(request, error='inspection_unavailable')
            except Exception:
                self._selection_reply(request, error='unknown_outcome')
            else:
                self._selection_reply(request, handoff=handoff)
        try:
            fd = os.open(str(self.selection_request), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        except FileNotFoundError:
            return
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and not info.st_mode&0o077 and
                    0 < info.st_size <= 2048, 'bounded private mission selection required')
            request = model.strict_json(stream.read(2049), 2048)
        require(set(request) == {'version','request_id','run_id','original_envelope_sha256'} and
                request['version'] == 1 and request['run_id'] == self.inputs['run_id'] and
                re.fullmatch(r'[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}', request['request_id']) and
                re.fullmatch('[0-9a-f]{64}', request['original_envelope_sha256']), 'exact selected original required')
        identity = request['request_id']
        if identity in self.selection_ids:
            return
        require(len(self.selection_ids) < 16, 'mission selection budget exhausted')
        self.selection_ids.add(identity)  # Consume before a potentially committed proposal.
        row = self.mission_originals.get(request['original_envelope_sha256'])
        if row is None:
            self._selection_reply(request, error='selection_unavailable')
            return
        if self.selection_worker is None:
            self.selection_worker = ThreadPoolExecutor(max_workers=1, thread_name_prefix='inspection-proposal')
        # Own the exact historical observation while live readers keep advancing.
        self.selection_pending = (deepcopy(request), self.selection_worker.submit(self._prepare_handoff,
                                  deepcopy(request), deepcopy(row)))

    def native_window(self, session):
        return InspectionWindow(self.executable, session, self.files, self.out, self.walkthrough,
                                self.clock, mission_config=self.mission_config,
                                viewer_profile=self.viewer_profile)

    def live_bridge_config(self, presentation, clock):
        if self.people_clock is None:
            return None
        from . import model
        config = bridge_config(self, presentation, clock, self.people_clock, app=model.APP)
        config['presentation_mode'] = 'continuous'
        return config

    def select_reader_update(self, updates, queued):
        return continuous_update(updates, self.presentation, queued, self.clock())

    def _record_presentation(self, presentation):
        ns = integer(presentation['simulation_time_ns'], 0, 120_000_000_000)
        if self.presentation is not None and ns <= self.presentation['simulation_time_ns']:
            return
        if self.bridge is not None:
            require(self.bridge.present(presentation), 'continuous source presentation regressed')
        elif self.presentation is not None:
            require(presentation['geometry'] == self.presentation['geometry'], 'continuous scene model changed')
        self.presentation = deepcopy(presentation)
        self.simulation_ns = ns
        self.presentation_count += 1
        require(self.presentation_count <= 512, 'continuous presentation budget exceeded')
        self.save('live/presentation-%04d.json' % self.presentation_count, presentation)

    def apply_source(self, request, response, presentation):
        require(isinstance(response, dict) and set(response) == {'sequence', 'simulation_time_ns', 'sensors'},
                'exact continuous source response required')
        integer(response['sequence'], self.source_capture_sequence+1)
        ns = integer(response['simulation_time_ns'], request['simulation_time_ns'], 120_000_000_000)
        require(response['sensors'] == request['sensors'] and presentation['simulation_time_ns'] >= ns,
                'source selection/capture differs; no retry')
        require({r['id'] for r in presentation['sensor_sources'] if r['enabled']} == set(request['sensors']),
                'completed presentation has a different source selection')
        self.source_capture_sequence = response['sequence']
        self.source_sequence += 1
        self.enabled = deepcopy(request['sensors'])
        self.save('live/source-%04d.json' % self.source_sequence, dict(request=request, response=response))
        self._record_presentation(presentation)
        if self.source_sequence == 1:
            self.start_inspection_executor()

    def source_io(self, request):
        with self.source_request_lock:
            return super().source_io(request)

    def _read_presentation(self):
        from . import model
        with self.source_request_lock:
            return model.strict_json(self.nodes['a'].scene_request('/v1/presentation'), 4 << 20)

    def poll_source_presentation(self):
        self.poll_mission_selection()
        # Exactly one independent read can block. Neither reader cursor nor the
        # native display waits for this I/O, and a held frame is not interpolated.
        if self.presentation_worker is None:
            self.presentation_worker = ThreadPoolExecutor(max_workers=1, thread_name_prefix='inspection-presentation')
        if self.presentation_future is not None and self.presentation_future.done():
            presentation = self.presentation_future.result()
            self.presentation_future = None
            self._record_presentation(presentation)
        now = self.clock()
        if self.presentation_future is None and now >= self.next_presentation_poll:
            self.next_presentation_poll = now+125_000_000
            self.presentation_future = self.presentation_worker.submit(self._read_presentation)

    def close_source_presentation(self):
        try:
            if self.presentation_worker is not None:
                self.presentation_worker.shutdown(wait=True, cancel_futures=True)
        finally:
            try:
                if self.selection_worker is not None:
                    self.selection_worker.shutdown(wait=True, cancel_futures=True)
            finally:
                try:
                    if self.operator_forward is not None:
                        self.operator_forward.close()
                finally:
                    if self.viewer_forward is not None:
                        self.viewer_forward.close()

    def source_command_minimum(self):
        return self.simulation_ns+1

    def validate_additional_layers(self):
        return {**super().validate_additional_layers(), 'presentation_mode': 'continuous',
                'completed_presentations': self.presentation_count,
                'last_acknowledged_source_capture_sequence': self.source_capture_sequence}

    def periodic_source_request(self):
        # ContinuousSource already produces captures. /step changes selection;
        # it must not be used to drive or rewind this physics clock.
        return None


from .run import Runner as InspectionRunner


class Runner(ContinuousLiveSession, InspectionRunner):
    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--executable', type=Path)
    parser.add_argument('--duration', type=int, default=45)
    parser.add_argument('--walkthrough', action='store_true')
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    runner = Runner(args.stage, args.output, executable=args.executable, duration=args.duration,
                    walkthrough=args.walkthrough, trace_rpc=args.trace_rpc)
    runner.execute()


if __name__ == '__main__':
    main()
