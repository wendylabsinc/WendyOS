"""Three actual loopback worldd processes and the actual world CLI.

Uses disposable simulation credentials, two controllable HTTP links, and no
executor or physical motion. Runtime credentials are removed at completion.
"""
import argparse
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]


def run(bin_dir, output):
    transcript, processes, logs, proxies = [], {}, [], []
    partition = threading.Event()
    run_id = 'proposal-inbox-' + str(time.time_ns())
    # Reserve three consecutive loopback ports before creating any processes.
    for port in range(24200, 25200, 3):
        held = []
        try:
            for number in range(3):
                sock = socket.socket(); sock.bind(('127.0.0.1', port + number)); held.append(sock)
            break
        except OSError:
            for sock in held: sock.close()
    else:
        raise RuntimeError('no local port block available')
    for sock in held: sock.close()
    with tempfile.TemporaryDirectory(prefix='world-proposal-inbox-') as tmp:
        folder = Path(tmp)

        def command(args, okay=True):
            result = subprocess.run([str(bin_dir / args[0]), *args[1:]], cwd=ROOT,
                                    capture_output=True, timeout=20)
            if okay and result.returncode:
                raise AssertionError(result.stderr.decode()[:2000])
            if not okay:
                assert result.returncode, 'expected command refusal'
                return result.stderr.decode()[:500]
            return json.loads(result.stdout) if args[0] == 'world' and result.stdout.strip() else None

        def write(name, value):
            path = folder / name
            path.write_text(json.dumps(value)); path.chmod(0o600)
            return path

        def cli(node, *args, okay=True, agent=False):
            return command(['world', 'task', *args, '--credentials', str(folder / (
                'agent-1.json' if agent else f'operator-{node}.json')), '--json'], okay)

        def proxy(target):
            class Handler(BaseHTTPRequestHandler):
                def log_message(self, *_): pass
                def do_GET(self):
                    if partition.is_set():
                        self.send_error(503, 'modeled link partition'); return
                    conn = http.client.HTTPConnection('127.0.0.1', target, timeout=3)
                    try:
                        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                        conn.request(self.command, self.path, body=body, headers=dict(self.headers))
                        response = conn.getresponse(); body = response.read()
                        self.send_response(response.status)
                        self.send_header('Content-Length', str(len(body)))
                        self.end_headers(); self.wfile.write(body)
                    except (OSError, http.client.HTTPException):
                        self.send_error(503, 'peer unavailable')
                    finally: conn.close()
                do_POST = do_GET
            server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            proxies.append(server)
            return f'http://127.0.0.1:{server.server_port}'

        def start(n):
            out = open(folder / f'node-{n}.log', 'ab'); logs.append(out)
            processes[n] = subprocess.Popen([str(bin_dir / 'worldd'), '--config', str(folder / f'node-{n}.json')],
                                            cwd=ROOT, stdout=out, stderr=out)

        def until(check, seconds=12):
            deadline = time.monotonic() + seconds
            last = None
            while time.monotonic() < deadline:
                try:
                    value = check()
                    if value: return value
                except (AssertionError, OSError, KeyError) as exc: last = str(exc)
                time.sleep(.1)
            raise AssertionError('condition deadline: ' + str(last))

        def record(event, **value):
            transcript.append({'event': event, **value})

        try:
            command(['world-lab', '--out', str(folder), '--base-port', str(port), '--missions', '--sensors',
                     '--task-relay', '--proposals', '--native-mission-package', str(ROOT / 'examples/live-office/world.json'),
                     '--run-id', run_id])
            configs = {n: json.loads((folder / f'node-{n}.json').read_text()) for n in (1, 2, 3)}
            origin = {'principal': configs[1]['peer_grant']['grant']['principal'],
                      'assignee': configs[1]['tasks']['assignee'], 'run_id': run_id,
                      'catalog': configs[1]['tasks']['catalog']}
            configs[1]['proposals']['export'] = True
            configs[1]['peers'] = [{'url': proxy(port + 1), 'audience': 'node-2'}]
            configs[2]['peers'] = [{'url': proxy(port), 'audience': 'node-1'}]
            configs[3]['peers'] = [{'url': f'http://127.0.0.1:{port + 1}', 'audience': 'node-2'}]
            for n in (2, 3): configs[n]['proposal_inbox'] = {'origins': {'node-1': origin}}
            for n in (1, 2, 3):
                write(f'node-{n}.json', configs[n]); start(n)
            until(lambda: cli(1, 'catalog'))
            until(lambda: cli(3, 'proposal', 'list', '--origin', 'node-1'))
            request = {'version': 1, 'id': 'inspect-pump', 'tenant': 'lab', 'site': 'synthetic-office',
                       'run_id': run_id, 'assignee': origin['assignee'], 'type': 'wendy.nav.inspect.v1',
                       'parameters': {'target': [1, .2, 0]}, 'summary': 'Review an inspection near the pump.',
                       'not_after_unix_ms': int(time.time() * 1000) + 180000}
            write('proposal.json', request)
            original = cli(1, 'propose', '--proposal', str(folder / 'proposal.json'), agent=True)
            digest = original['record']['body_sha256']
            until(lambda: cli(3, 'proposal', 'get', '--origin', 'node-1', '--id', request['id']))
            views = [cli(n, 'proposal', 'get', '--origin', 'node-1', '--id', request['id']) for n in (2, 3)]
            assert all(v['record'] == original['record'] and not v['executable'] for v in views)
            assert cli(1, 'list')['tasks'] == []
            record('two_operator_views', origin='node-1', proposal=request['id'], body_sha256=digest,
                   cursor=views[1]['progress']['through'], executable=False,
                   current_verification=views[1]['current_verification'], response=views[1])
            partition.set()
            before = cli(2, 'proposal', 'get', '--origin', 'node-1', '--id', request['id'])
            processes[2].kill(); processes[2].wait(timeout=5)
            retained = cli(3, 'proposal', 'get', '--origin', 'node-1', '--id', request['id'])
            assert retained['record'] == original['record']
            start(2)
            recovered = until(lambda: cli(2, 'proposal', 'get', '--origin', 'node-1', '--id', request['id']))
            assert recovered['record'] == original['record'] and recovered['progress']['through'] == 1
            assert recovered['progress']['last_sync_unix_ms'] == before['progress']['last_sync_unix_ms']
            record('partition_and_reader_kill', retained_at_two_hops=True, recovered_cursor=1,
                   original_proof_unchanged=True, origin_link_blocked=True, response=recovered)
            # Approve only after an explicit operator command; the local inbox
            # never enters the task store. Queue the signed command during loss.
            signed_context = json.loads((folder / 'mission-context.json').read_text())
            context = signed_context['context']
            spec = {k: context[k] for k in ('type', 'site', 'assignee', 'class', 'mode', 'frame', 'area',
                    'required_sensors', 'minimum_energy', 'fallback', 'speed_mps', 'tolerance_m', 'stop_speed_mps')}
            spec.update(id='reviewed-inspection', domain='simulation', target=[1, .2, 0],
                        not_after_unix_ms=int(time.time() * 1000) + 90000, control_context=signed_context,
                        proposal={'audience': 'node-1', 'id': request['id'], 'body_sha256': digest})
            order = ('control_context', 'proposal', 'id', 'type', 'site', 'assignee', 'domain', 'class', 'mode',
                     'frame', 'not_after_unix_ms', 'area', 'required_sensors', 'minimum_energy', 'fallback',
                     'target', 'speed_mps', 'tolerance_m', 'stop_speed_mps')
            spec = {key: spec[key] for key in order}
            write('reviewed-spec.json', spec)
            queued = cli(2, 'send', '--spec', str(folder / 'reviewed-spec.json'), '--destination', 'node-1')
            assert queued['state'] == 'queued'
            assert cli(1, 'list')['tasks'] == []
            record('explicit_operator_approval_queued', origin_tasks=0, delivery='queued', automatic_approval=False, response=queued)
            partition.clear()
            admitted = until(lambda: next((r for r in cli(1, 'list')['tasks'] if r['spec']['id'] == spec['id']), None))
            assert admitted['status']['Phase'] == 'accepted', admitted['status']
            assert admitted['spec']['proposal']['body_sha256'] == digest
            original_instruction = admitted['proof']
            time.sleep(.8)
            records = cli(1, 'list')['tasks']
            assert len(records) == 1 and records[0]['proof'] == original_instruction
            assert records[0]['status']['Phase'] == 'accepted', records[0]['status']
            record('reconnected_origin_admission', task_count=1, proposal_digest=digest,
                   signed_instruction_unchanged=True, execution_started=False, response=admitted)
            # An expired proposal remains labeled history but cannot authorize a
            # new first admission at the assignee.
            expired = dict(request, id='expired-proposal', not_after_unix_ms=int(time.time() * 1000) + 1200)
            write('expiring.json', expired)
            exp = cli(1, 'propose', '--proposal', str(folder / 'expiring.json'), agent=True)
            until(lambda: cli(3, 'proposal', 'get', '--origin', 'node-1', '--id', expired['id']))
            until(lambda: cli(3, 'proposal', 'get', '--origin', 'node-1', '--id', expired['id'])['expired'])
            bad = dict(spec, id='refused-expired', proposal={'audience': 'node-1', 'id': expired['id'],
                           'body_sha256': exp['record']['body_sha256']})
            write('expired-spec.json', bad)
            denied = cli(1, 'send', '--spec', str(folder / 'expired-spec.json'), okay=False)
            assert 'expired' in denied.lower() or 'rejected' in denied.lower() or '403' in denied
            record('expired_proposal_refused', historical_row_visible=True, new_approval_refused=True, error=denied)
            # Original-scoped lookups are explicit, never an unqualified ID merge.
            cli(3, 'proposal', 'get', '--origin', 'unconfigured-origin', '--id', request['id'], okay=False)
            record('unconfigured_origin_refused', passed=True)
            result = {'status': 'passed', 'scope': 'Actual loopback Go executables, simulated authority, modeled bidirectional HTTP partition; no executor or motion',
                      'transcript': transcript, 'runtime_credentials_retained': False}
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(json.dumps(result, indent=2) + '\n')
            print(json.dumps({'status': 'passed', 'events': len(transcript), 'report': str(output)}))
        except BaseException:
            output.parent.mkdir(parents=True, exist_ok=True)
            for n in processes:
                path = folder / f"node-{n}.log"
                if path.exists(): (output.parent / f"failure-node-{n}.log").write_text(path.read_text())
            raise
        finally:
            for proc in processes.values():
                if proc.poll() is None:
                    proc.terminate()
                    try: proc.wait(timeout=5)
                    except subprocess.TimeoutExpired: proc.kill(); proc.wait(timeout=5)
            for server in proxies: server.shutdown(); server.server_close()
            for out in logs: out.close()


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--bin-dir', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    run(args.bin_dir.resolve(), args.out.resolve())
