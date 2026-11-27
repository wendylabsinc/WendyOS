"""Actual three-node historical inbox and native approval, with no executor.

Disposable simulation keys stay in the private artifact directory. Native stages
invoke the application's ordinary callbacks, not OS mouse interaction.
"""
import argparse
import base64
import hashlib
import http.client
import json
import signal
import subprocess
import threading
import time
from pathlib import Path
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]


def sha(path): return hashlib.sha256(path.read_bytes()).hexdigest()
def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")
    path.chmod(0o600)


def run(args):
    out = args.out.resolve(); out.mkdir(mode=0o700, parents=True, exist_ok=False)
    site = out / 'private-site'; site.mkdir(mode=0o700)
    processes, logs, servers, nodes, checks, requests = [], [], [], {}, [], []
    partition = threading.Event(); hold = threading.Event(); reached = threading.Event(); release = threading.Event(); lost = threading.Event(); tamper = threading.Event(); oversized = threading.Event(); chronology = threading.Event()
    receipt = {'scope': 'Actual authenticated loopback nodes and native callbacks; synthetic simulation proposals, no executor or physical motion', 'checks': checks, 'processes': []}
    def command(argv, okay=True):
        result = subprocess.run([str(x) for x in argv], cwd=ROOT, capture_output=True, timeout=30)
        if bool(result.returncode == 0) != okay: raise AssertionError(result.stderr.decode()[-1000:])
        if not okay: return result.stderr.decode()
        return json.loads(result.stdout) if result.stdout.strip().startswith((b'{', b'[')) else result.stdout.decode()
    def start(argv, label):
        log = (out / (label + '.log')).open('wb'); logs.append(log)
        process = subprocess.Popen([str(x) for x in argv], cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
        process.receipt = {'label': label, 'pid': process.pid, 'started_at': time.time(), 'executable_sha256': sha(Path(argv[0]))}
        receipt['processes'].append(process.receipt); processes.append(process); return process
    def stop(process, kill=False):
        if 'exited_at' in process.receipt: return
        if process.poll() is None:
            process.send_signal(signal.SIGKILL if kill else signal.SIGTERM)
            try: process.wait(timeout=4)
            except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=4)
        process.receipt.update(exited_at=time.time(), returncode=process.returncode, deliberate_sigkill=kill)
    def until(action, seconds=12):
        end = time.monotonic() + seconds; error = None
        while time.monotonic() < end:
            try:
                value = action()
                if value: return value
            except (AssertionError, OSError, KeyError) as e: error = str(e)
            time.sleep(.08)
        raise AssertionError('condition timeout: ' + str(error))
    def invalid_chronology(data):
        page = json.loads(data); row = page['proposals'][0]; record = row['record']
        record['accepted_at_unix_ms'] = record['proof']['issued_at_unix_ms'] - 1
        raw = lambda value: json.dumps(value, separators=(',', ':'), ensure_ascii=False).encode()
        proof = row['origin_proof']; proof['body_sha256'] = hashlib.sha256(raw({'purpose': 'wendy.world.proposal-history.v1', 'origin': row['origin'], 'record': record})).hexdigest()
        message = b'wendy.world.authority.v1/request\0' + raw({'version': 1, 'grant_digest': hashlib.sha256(raw(proof['grant'])).hexdigest(), 'audience': proof['audience'], 'operation': proof['operation'], 'body_digest': proof['body_sha256'], 'nonce': proof['nonce'], 'issued': proof['issued_at_unix_ms'], 'expires': proof['expires_at_unix_ms']})
        key = site / 'malformed-history-key.der'; message_path = site / 'malformed-history-message'; signature = site / 'malformed-history-signature'
        key.write_bytes(bytes.fromhex('302e020100300506032b657004220420') + base64.b64decode(configs[1]['peer_private_key'])[:32]); key.chmod(0o600)
        message_path.write_bytes(message)
        subprocess.run([args.openssl, 'pkeyutl', '-sign', '-rawin', '-inkey', str(key), '-keyform', 'DER', '-in', str(message_path), '-out', str(signature)], check=True, capture_output=True)
        public_key = site / 'malformed-history-public.der'
        subprocess.run([args.openssl, 'pkey', '-in', str(key), '-inform', 'DER', '-pubout', '-outform', 'DER', '-out', str(public_key)], check=True, capture_output=True)
        subprocess.run([args.openssl, 'pkeyutl', '-verify', '-rawin', '-pubin', '-inkey', str(public_key), '-keyform', 'DER', '-in', str(message_path), '-sigfile', str(signature)], check=True, capture_output=True)
        receipt['malformed_chronology'] = {'signature_verified': True, 'accepted_at_unix_ms': record['accepted_at_unix_ms'], 'proposal_issued_at_unix_ms': record['proof']['issued_at_unix_ms'], 'signing_message_sha256': sha(message_path)}
        proof['signature'] = base64.b64encode(signature.read_bytes()).decode()
        (out / 'signed-invalid-chronology.json').write_bytes(raw(page))
        return raw(page)
    def proxy(target, origin=False):
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_GET(self): self.forward()
            def do_POST(self): self.forward()
            def forward(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                if origin and partition.is_set(): self.send_error(503, 'modeled origin partition'); return
                conn = http.client.HTTPConnection('127.0.0.1', target, timeout=10)
                try:
                    conn.request(self.command, self.path, body, {k: v for k, v in self.headers.items() if k.lower() not in ('host', 'connection', 'content-length')})
                    response = conn.getresponse(); data = response.read(); status = response.status
                    info = {'path': self.path, 'method': self.command, 'status': status, 'origin_link': origin, 'time': time.time()}
                    if origin and self.command == 'POST' and self.path == '/v1/tasks':
                        info['instruction_sha256'] = hashlib.sha256(body).hexdigest()
                        task_file = out / f'task-http-{len(requests):03d}.json'; task_file.write_bytes(data); info['raw_task_response'] = task_file.name; info['raw_task_response_sha256'] = sha(task_file)
                        if lost.is_set() and status == 200:
                            lost.clear(); status = 503; data = b'acknowledgement deliberately lost'; info['lost_acknowledgement'] = True
                    if not origin and self.path.startswith('/v1/proposal-inbox') and status == 200:
                        (out / f'raw-inbox-{len(requests):03d}.json').write_bytes(data)
                        if 'after=' in self.path and not self.path.endswith('after=') and hold.is_set():
                            reached.set(); release.wait(15)
                    if not origin and self.path.startswith('/v1/proposal-inbox') and status == 200:
                        if chronology.is_set(): data = invalid_chronology(data); info['signed_invalid_chronology'] = True
                        if tamper.is_set(): data = data.replace(b'Inspect the reported pump', b'Forged modified proposal', 1); info['tampered_reply'] = True
                        if oversized.is_set(): data = b' ' * ((1 << 20) + 1); info['oversized_reply'] = True
                    requests.append(info)
                    self.send_response(status); self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
                except (BrokenPipeError, ConnectionResetError): pass
                except Exception: self.send_error(503)
                finally: conn.close()
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler); server.daemon_threads = True
        threading.Thread(target=server.serve_forever, daemon=True).start(); servers.append(server)
        return f'http://127.0.0.1:{server.server_port}'
    def node(number):
        nodes[number] = start([args.bin_dir / 'worldd', '--config', site / f'node-{number}.json'], f'node-{number}-{len(processes)}')
        def ready():
            conn = http.client.HTTPConnection('127.0.0.1', args.port + number - 1, timeout=.3)
            try: conn.request('GET', '/healthz'); response = conn.getresponse(); response.read(); return response.status == 200
            finally: conn.close()
        until(ready)
    def cli(number, *words, okay=True, credential=None):
        return command([args.bin_dir / 'world', 'task', *words, '--credentials', credential or site / f'operator-{number}.json', '--json'], okay)
    def native(stage, label, profile='profile', wait=True, proposal='inspect-pump', task='inbox-inspection', origin='node-1', handoff=None):
        path = out / (label + '.json')
        argv = [args.app / 'Contents/MacOS/Wendy World', '--profile', site / (profile + '.json'), '--storage-directory', out / ('storage-' + ('reader' if profile == 'reader-profile' else 'operator')), '--inbox-test-stage', stage, '--inbox-report', path, '--inbox-origin', origin, '--inbox-proposal', proposal, '--inbox-task', task]
        if handoff: argv += ['--inbox-handoff', handoff]
        process = start(argv, label)
        if not wait: return process
        until(lambda: path.exists() or (process.poll() is not None and (_ for _ in ()).throw(AssertionError('native exited'))), seconds=25)
        value = json.loads(path.read_text()); stop(process); return value
    try:
        command([args.bin_dir / 'world-lab', '--out', site, '--base-port', args.port, '--missions', '--sensors', '--proposals', '--native-mission-package', ROOT / 'examples/live-office/world.json', '--run-id', 'native-inbox'])
        configs = {n: json.loads((site / f'node-{n}.json').read_text()) for n in (1, 2, 3)}
        operator3 = json.loads((site / 'operator-1.json').read_text()); operator3['url'] = f'http://127.0.0.1:{args.port + 2}'; operator3['audience'] = 'node-3'; write(site / 'operator-3.json', operator3)
        origin_link = proxy(args.port, origin=True); reader_link = proxy(args.port + 2)
        config_origin = {'principal': configs[1]['peer_grant']['grant']['principal'], 'assignee': configs[1]['tasks']['assignee'], 'run_id': 'native-inbox', 'catalog': configs[1]['tasks']['catalog']}
        configs[1]['proposals']['export'] = True; configs[1]['peers'] = []
        configs[2]['proposals'] = {'path': str(site / 'node-2.proposals'), 'export': True}
        configs[2]['tasks'] = {'assignee': config_origin['assignee'], 'catalog': config_origin['catalog'], 'path': str(site / 'node-2.tasks'), 'executor_principal': configs[1]['tasks']['executor_principal']}
        configs[2]['peers'] = [{'url': origin_link, 'audience': 'node-1'}]
        configs[3]['peers'] = [{'url': f'http://127.0.0.1:{args.port + 1}', 'audience': 'node-2'}]
        for n in (2, 3): configs[n]['proposal_inbox'] = {'origins': {'node-1': config_origin}}
        config_origin2 = dict(config_origin, principal=configs[2]['peer_grant']['grant']['principal'])
        configs[3]['proposal_inbox']['origins']['node-2'] = config_origin2
        agent2 = json.loads((site / 'agent-1.json').read_text()); agent2['url'] = f'http://127.0.0.1:{args.port + 1}'; agent2['audience'] = 'node-2'; write(site / 'agent-2.json', agent2)
        for n in (1, 2, 3): write(site / f'node-{n}.json', configs[n]); node(n)
        pin = {'origin': 'node-1', 'principal': config_origin['principal'], 'assignee': config_origin['assignee'], 'run_id': 'native-inbox', 'root_public_key': configs[1]['root_public_key']}
        reader = json.loads((site / 'viewer-2.json').read_text()); reader['url'] = reader_link; reader['audience'] = 'node-3'
        mission = json.loads((site / 'mission-macos.json').read_text()); mission['url'] = origin_link
        profile = {'credentials': reader, 'proposal_inbox': {'version': 1, 'origins': [pin, dict(pin, origin='node-2', principal=config_origin2['principal'])]}, 'mission_credentials': mission, 'mission_context': json.loads((site / 'mission-context.json').read_text()), 'mission_clock_trust': json.loads((site / 'mission-clock-trust.json').read_text()), 'mission_navigation_profile': {'kind': 'planar', 'floor_y': .2}}
        write(site / 'profile.json', profile); write(site / 'reader-profile.json', {'credentials': reader, 'proposal_inbox': profile['proposal_inbox']})
        def propose(name, ttl=180000, number=1, target=None):
            request = {'version': 1, 'id': name, 'tenant': 'lab', 'site': 'synthetic-office', 'run_id': 'native-inbox', 'assignee': config_origin['assignee'], 'type': 'wendy.nav.inspect.v1', 'parameters': {'target': target or [1, .2, 0]}, 'summary': 'Inspect the reported pump. Operator review required.', 'not_after_unix_ms': int(time.time() * 1000) + ttl}
            path = site / (name + '.json'); write(path, request)
            return cli(number, 'propose', '--proposal', path, credential=site / f'agent-{number}.json')
        original = propose('inspect-pump')
        propose('review-east'); propose('review-west'); propose('inspect-pump', number=2)
        until(lambda: len(cli(3, 'proposal', 'list', '--origin', 'node-1')['proposals']) == 3)
        # Save untouched Go HTTP bytes for independent Swift interoperability.
        config = json.loads((site / 'operator-3.json').read_text()); config['url'] = reader_link; write(site / 'proxy-reader.json', config)
        cli(3, 'proposal', 'list', '--origin', 'node-1', credential=site / 'proxy-reader.json')
        raw = sorted(out.glob('raw-inbox-*.json'))[-1]; (out / 'sdk-page.json').write_bytes(raw.read_bytes())
        write(out / 'sdk-scope.json', {'origin': pin, 'tenant': 'lab', 'site': 'synthetic-office'})
        if args.fixture_only:
            receipt['passed'] = True; checks.append('Exact Go historical receipt fixture retained'); return
        first = native('refresh', '01-relayed-inbox'); independent = native('refresh', '02-independent-reader', profile='reader-profile')
        assert len(first['rows']) == len(independent['rows']) == 3 and first['rows'][0]['record'] == independent['rows'][0]['record']
        assert cli(1, 'list')['tasks'] == []
        checks.append('Two native readers verify exact relayed historical proofs; browsing creates no tasks')
        until(lambda: len(cli(3, 'proposal', 'list', '--origin', 'node-2')['proposals']) == 1)
        second_origin = native('refresh', '02b-same-id-independent-origin', origin='node-2')
        assert len(second_origin['rows']) == 1 and second_origin['rows'][0]['record']['proposal']['id'] == first['rows'][0]['record']['proposal']['id']
        assert second_origin['rows'][0]['origin'] != first['rows'][0]['origin']
        wrong_origin = native('review', '02c-other-origin-no-approval', origin='node-2')
        assert wrong_origin['reviewed'] is None and not wrong_origin['missions']
        checks.append('Same proposal ID on two actual origins retains separate signed rows; mission profile cannot approve the other origin')
        partition.set(); stop(nodes[2], kill=True); node(2)
        reopened = native('reopen', '03-partition-reopen')
        assert reopened['rows'] == first['rows'] and not reopened['online']
        denied = native('review', '04-origin-unavailable')
        assert denied['reviewed'] is None and not denied['missions'] and cli(1, 'list')['tasks'] == []
        checks.append('Partition and relay SIGKILL preserve exact history; unreachable origin cannot create an unsigned draft or task')
        partition.clear()
        review = native('review', '05-exact-origin-review'); assert review['reviewed'] == 'node-1:inspect-pump' and not review['missions']
        assert cli(1, 'list')['tasks'] == []
        draft = native('draft', '06-explicit-unsigned-draft'); assert len(draft['missions']) == 1 and draft['missions'][0]['state'] == 'draft' and draft['missions'][0]['instruction'] is None
        assert cli(1, 'list')['tasks'] == []
        assert draft['missions'][0]['draft']['target'] == [1, .2, 0]
        checks.append('Exact configured origin review precedes a separate unsigned draft; neither review nor draft dispatches')
        lost.set(); uncertain = native('send', '07-lost-acknowledgement')
        assert len(cli(1, 'list')['tasks']) == 1 and uncertain['missions'][0]['instruction_sha256']
        assert cli(1, 'list')['tasks'][0]['spec']['target'] == [1, .2, 0]
        assert cli(1, 'list')['tasks'][0]['status']['Phase'] == 'accepted'
        assert not lost.is_set(), 'fixture must actually lose a successful admission reply'
        saved_hash = uncertain['missions'][0]['instruction_sha256']
        retry = native('send', '08-exact-retry-after-reopen')
        assert retry['missions'][0]['instruction_sha256'] == saved_hash and len(cli(1, 'list')['tasks']) == 1
        attempts = [r for r in requests if r.get('instruction_sha256')]
        assert len(attempts) == 2 and all(r['instruction_sha256'] == saved_hash and r['status'] == 200 for r in attempts)
        assert attempts[0].get('lost_acknowledgement') is True and not attempts[1].get('lost_acknowledgement')
        assert retry['missions'][0]['state'] == 'delivered' and retry['missions'][0]['record']['status']['Phase'] == 'accepted'
        checks.append('Explicit approval persists before send; lost acknowledgement and reopening retry identical instruction bytes, one origin task')
        # App dies while a later complete-history page is delayed.
        propose('review-z-last'); until(lambda: len(cli(3, 'proposal', 'list', '--origin', 'node-1')['proposals']) == 4)
        hold.set(); process = native('refresh', '09-midpage-kill', wait=False)
        assert reached.wait(10); stop(process, kill=True); hold.clear(); release.set()
        old = native('reopen', '10-retained-complete-history'); assert old['rows'] == first['rows']
        refreshed = native('refresh', '11-complete-refresh'); assert len(refreshed['rows']) == 4
        checks.append('SIGKILL during paginated refresh retains exact prior complete history; successful retry adds the new record atomically')
        tamper.set(); rejected = native('refresh', '11a-tampered-proof-refused'); tamper.clear()
        assert rejected['rows'] == refreshed['rows'] and not rejected['online']
        chronology.set(); invalid = native('refresh', '11c-signed-invalid-chronology'); chronology.clear()
        assert invalid['rows'] == refreshed['rows'] and not invalid['online']
        checks.append('Correctly origin-signed impossible acceptance chronology is rejected by the native reader without replacing history')
        oversized.set(); bounded = native('refresh', '11b-oversized-response-refused'); oversized.clear()
        assert bounded['rows'] == refreshed['rows'] and not bounded['online']
        checks.append('Tampered signed content and oversized HTTP pages fail without replacing the exact prior complete document')
        expired = propose('expired-review', 1800)
        until(lambda: cli(3, 'proposal', 'get', '--origin', 'node-1', '--id', 'expired-review')['expired'])
        historical = native('refresh', '12-expired-history')
        refused = native('draft', '13-expired-review-refused', proposal='expired-review', task='refused-expired')
        assert refused['reviewed'] is None and len(refused['missions']) == 1 and len(cli(1, 'list')['tasks']) == 1
        checks.append('Expired proposal remains historical but cannot enter a new current-origin draft')
        propose('unsupported-planar', target=[1, .2, .05])
        until(lambda: cli(3, 'proposal', 'get', '--origin', 'node-1', '--id', 'unsupported-planar')['record']['proposal']['id'] == 'unsupported-planar')
        native('refresh', '13f-unsupported-planar-history')
        unsupported = native('draft', '13g-unsupported-planar-read-only', proposal='unsupported-planar', task='unsupported-planar-task')
        assert len(unsupported['missions']) == 1 and len(cli(1, 'list')['tasks']) == 1
        checks.append('A plain planar proposal without prepared trigger/destination remains read-only and cannot create an approval draft or task')
        prepared_dir = out / 'prepared-planar'; prepared_dir.mkdir(mode=0o700)
        command([args.fixture, '--context', site / 'mission-context.json', '--out', prepared_dir])
        cli(1, 'propose', '--proposal', prepared_dir / 'proposal.json', credential=site / 'agent-1.json')
        until(lambda: cli(3, 'proposal', 'get', '--origin', 'node-1', '--id', 'planar-review')['record']['proposal']['id'] == 'planar-review')
        historical = native('refresh', '13a-planar-proposal-relayed')
        for label, mutate in [('mismatch', lambda h: h.update(proposal_id='other')), ('stale', lambda h: h['mission_input'].update(sample_valid_until='2020-01-01T00:00:00Z'))]:
            altered = json.loads((prepared_dir / 'handoff.json').read_text()); mutate(altered); path = prepared_dir / (label + '.json'); write(path, altered)
            failed = native('draft', '13b-' + label + '-handoff-refused', proposal='planar-review', task='refused-' + label, handoff=path)
            assert len(failed['missions']) == 1 and len(cli(1, 'list')['tasks']) == 1
        planar = native('draft', '13c-prepared-full-vector-draft', proposal='planar-review', task='planar-inspection', handoff=prepared_dir / 'handoff.json')
        entry = next(m for m in planar['missions'] if m['id'] == 'planar-inspection')
        assert entry['state'] == 'draft' and entry['instruction'] is None and entry['draft']['target'] == [1, .2, .05]
        assert entry['draft']['preparedUntilUnixMS'] > int(time.time()*1000)
        prepared_input = json.loads((prepared_dir / 'handoff.json').read_text())['mission_input']
        assert prepared_input['sample_valid_until'].startswith('2026-01-01T')
        assert not prepared_input['physical_valid_until'].startswith('2026-01-01T')
        write(out / 'separate-clock-domains.json', {'sample_valid_until': prepared_input['sample_valid_until'], 'physical_valid_until': prepared_input['physical_valid_until'], 'draft_physical_deadline_unix_ms': entry['draft']['preparedUntilUnixMS'], 'logical_sample_is_not_wall_clock': True})
        assert len(cli(1, 'list')['tasks']) == 1
        checks.append('Explicit prepared-reader import keeps separate simulation/physical bounds, rejects wrong/stale files and preserves the exact full-vector unsigned draft')
        lost.set(); sent = native('send', '13d-planar-accepted-lost-ack', proposal='planar-review', task='planar-inspection')
        tasks = cli(1, 'list')['tasks']; write(out / 'planar-admission-records.json', tasks)
        admitted = next(t for t in tasks if t['spec']['id'] == 'planar-inspection')
        trigger = json.loads((prepared_dir / 'trigger.json').read_text()); handoff = json.loads((prepared_dir / 'handoff.json').read_text())
        assert len(tasks) == 2 and admitted['status']['Phase'] == 'accepted' and admitted['spec']['target'] == [1, .2, .05]
        assert admitted['spec']['trigger'] == trigger and admitted['spec']['inspection_destination'] == handoff['inspection_destination'] and not lost.is_set()
        again = native('send', '13e-planar-exact-retry', proposal='planar-review', task='planar-inspection')
        retried = next(m for m in again['missions'] if m['id'] == 'planar-inspection')
        assert retried['state'] == 'delivered' and retried['record']['status']['Phase'] == 'accepted'
        planar_posts = [r for r in requests if r.get('instruction_sha256') == retried['instruction_sha256']]
        assert len(planar_posts) == 2 and all(r['status'] == 200 for r in planar_posts) and planar_posts[0].get('lost_acknowledgement') and not planar_posts[1].get('lost_acknowledgement')
        assert len(cli(1, 'list')['tasks']) == 2
        checks.append('Prepared full-vector planar approval receives actual HTTP200/accepted phase; lost reply and explicit retry retain exact trigger, destination and instruction bytes')
        wrong = json.loads(json.dumps(profile)); wrong['mission_credentials']['audience'] = 'node-2'; write(site / 'wrong-origin.json', wrong)
        bad = native('review', '14-wrong-profile-refused', profile='wrong-origin')
        assert bad['reviewed'] is None and len(cli(1, 'list')['tasks']) == 2
        checks.append('Wrong mission audience/profile cannot turn a replicated receipt into approval')
        # Revoke the current operator without changing the saved historical bytes.
        if args.publisher:
            revocations = command([args.publisher, '--issuer', site / 'issuer.json', '--revoke-id', mission['grant']['grant']['id']])
            configs[1]['revocations'] = revocations; write(site / 'node-1.json', configs[1])
            stop(nodes[1]); node(1)
            revoke = native('review', '15-revoked-operator-refused')
            assert revoke['reviewed'] is None and len(cli(1, 'list')['tasks']) == 2
            checks.append('A revoked live mission operator cannot review for approval; historical browsing does not grant authority')
            issuer = json.loads((site / 'issuer.json').read_text()); issuer['revocations'] = revocations; write(site / 'issuer.json', issuer)
            revocations = command([args.publisher, '--issuer', site / 'issuer.json', '--revoke-id', reader['grant']['grant']['id']])
            configs[3]['revocations'] = revocations; write(site / 'node-3.json', configs[3]); stop(nodes[3]); node(3)
            denied_read = native('refresh', '16-revoked-reader-history-retained')
            assert len(denied_read['rows']) == len(historical['rows']) and not denied_read['online']
            assert [r['record'] for r in denied_read['rows']] == [r['record'] for r in historical['rows']]
            checks.append('Revoked reader refresh fails while exact prior complete history stays available without current authorization claims')
        receipt['passed'] = True
    finally:
        release.set()
        for process in reversed(processes): stop(process)
        for server in servers: server.shutdown(); server.server_close()
        for log in logs: log.close()
        write(out / 'requests.json', requests)
        receipt['driver_sha256'] = sha(Path(__file__))
        receipt['executables'] = {str(args.bin_dir / n): sha(args.bin_dir / n) for n in ('world', 'worldd', 'world-lab')}
        receipt['executables'][str(args.fixture)] = sha(args.fixture)
        if args.app: receipt['executables'][str(args.app / 'Contents/MacOS/Wendy World')] = sha(args.app / 'Contents/MacOS/Wendy World')
        receipt['finished_at'] = time.time(); write(out / 'verification.json', receipt)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--bin-dir', type=Path, required=True)
    parser.add_argument('--app', type=Path)
    parser.add_argument('--fixture', type=Path, required=True)
    parser.add_argument('--publisher', type=Path)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--port', type=int, default=21160)
    parser.add_argument('--openssl', default='/opt/homebrew/opt/openssl@3/bin/openssl')
    parser.add_argument('--fixture-only', action='store_true')
    run(parser.parse_args())
