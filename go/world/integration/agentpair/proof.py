"""Public-input adapter for the independently qualified replica proof executable.

prepare() checks receipt structure and commissioning bindings, not signatures.
verify() invokes the pinned executable once through the runner's owned-node
callback. That executable acquires fresh authenticated physical time itself;
the fixture interval is only a narrower permission window, never a clock.
Neither step establishes route choice, durable remote admission, lease validity
or observation freshness. Those require the scenario's separate assertions.
"""
import base64
from dataclasses import dataclass
from datetime import datetime, timedelta
import math
import re

import model

KIND = 'UNTRUSTED-PRIVATE-AGENT-PAIR-PROOF-v1'
MAX_INPUT = 256 << 10
MAX_RECEIPT = 512 << 10
MAX_RESULT = 4096
AWAIT_BUDGET_NS = 10_000_000_000
AWAIT_INTERVAL_NS = 200_000_000


def _object(value, required, optional=()):
    model.require(isinstance(value, dict) and set(required) <= set(value) <= set(required) | set(optional), 'unexpected proof input fields')
    return value


def _integer(value, minimum=1, maximum=(1 << 64) - 1):
    model.require(type(value) is int and minimum <= value <= maximum, 'bounded integer required')
    return value


def _bytes(value, limit=MAX_INPUT):
    model.require(isinstance(value, str) and 0 < len(value) <= 4 * ((limit + 2) // 3), 'bounded public byte encoding required')
    try:
        raw = base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as error:
        raise model.Refused('invalid public byte encoding') from error
    model.require(0 < len(raw) <= limit and base64.b64encode(raw).decode() == value, 'noncanonical or oversized public byte encoding')
    return raw


def _owned(value, limit=MAX_RECEIPT):
    # Receipt dictionaries are supplied by the runner, not used as shared
    # mutable state across its write/execute callbacks.
    try:
        raw = value if isinstance(value, bytes) else model.json_bytes(value)
    except (ValueError, TypeError, RecursionError) as error:
        raise model.Refused('invalid public proof input') from error
    return model.strict_json(raw, limit)


def _task(task, device):
    model.require(isinstance(task, dict) and task.get('device') == device and
                  task.get('app') == model.APP and task.get('service') == model.SERVICE,
                  'observed ordinary task differs')
    return _integer(task.get('generation'))


def _receipt(value, run_id, task, device, action):
    receipt = _owned(value)
    fields = ('version', 'run_id', 'sequence', 'action', 'status', 'rpc_code',
              'command_json', 'command_sha256', 'clock', 'request_json',
              'reply_json', 'originals', 'validation')
    if action == 'await_query':
        fields += ('attempts', 'await_elapsed_ns')
    _object(receipt, fields)
    model.require(type(receipt['version']) is int and receipt['version'] == 1 and
                  receipt['run_id'] == run_id and receipt['action'] == action and
                  receipt['status'] == 'complete' and receipt['rpc_code'] == 'OK' and
                  receipt['validation'] == 'socket_scope_and_structure_only',
                  'successful exact probe receipt required')
    sequence = _integer(receipt['sequence'], maximum=32)
    raw_command = _bytes(receipt['command_json'], 1024)
    model.require(model.digest(raw_command) == receipt['command_sha256'], 'command bytes differ')
    command = model.strict_json(raw_command, 1024)
    fields = ('version', 'sequence', 'action')
    if action in ('publish', 'tombstone'):
        fields += ('ttl_ms',)
    if action == 'publish':
        fields += ('value',)
    if action == 'await_query':
        fields += ('expect', 'envelope_sha256')
    _object(command, fields)
    model.require(type(command['version']) is int and command['version'] == 1 and
                  type(command['sequence']) is int and command['sequence'] == sequence and
                  command['action'] == action, 'original command binding differs')
    if action in ('publish', 'tombstone'):
        _integer(command['ttl_ms'], 100, 30000)
    if action == 'publish':
        value = command['value']
        model.require(type(value) in (int, float) and math.isfinite(value) and 0 <= value <= 1000000, 'scalar command refused')
    if action == 'await_query':
        model.require(command['expect'] == 'present' and model.digest_shape(command['envelope_sha256']),
                      'only an exact present-proof predicate can verify receipt')
    _clock(receipt['clock'], run_id, task, device)
    return receipt, command


def _clock(value, run_id, task, device):
    clock = _object(value, ('sample_unix_ns', 'sample_clock_domain', 'deployment_generation',
                                    'server_boot_id', 'physical_earliest_unix_ns', 'physical_latest_unix_ns', 'round_trip_ns'))
    model.require(_integer(clock['deployment_generation']) == _task(task, device) and
                  clock['sample_clock_domain'] == model.sample_domain(run_id),
                  'probe clock belongs to another deployment')
    _integer(clock['sample_unix_ns'], -(1 << 63), (1 << 63) - 1)
    early = _integer(clock['physical_earliest_unix_ns'], 1, (1 << 63) - 1)
    late = _integer(clock['physical_latest_unix_ns'], early, (1 << 63) - 1)
    model.require(late - early <= 5_000_000_000, 'probe physical interval exceeds commissioned width')
    _integer(clock['round_trip_ns'], 0, 1_000_000_000)
    model.require(isinstance(clock['server_boot_id'], str) and re.fullmatch('[a-f0-9]{32}', clock['server_boot_id']), 'probe server incarnation required')


def _await(receipt, command, origin, run_id, task, device):
    """Check bounded retry evidence without treating it as authority or a clock."""
    attempts = receipt['attempts']
    # These are untrusted monotonic-duration diagnostics, never physical time.
    # No serialization grace extends the action's declared ten-second budget.
    elapsed = _integer(receipt['await_elapsed_ns'], 1, AWAIT_BUDGET_NS)
    model.require(isinstance(attempts, list) and 1 <= len(attempts) <= 32 and
                  command['envelope_sha256'] == model.digest(origin), 'exact bounded await required')
    duration_sum = 0
    for number, attempt in enumerate(attempts, 1):
        _object(attempt, ('attempt', 'outcome', 'elapsed_ns'), ('clock', 'rpc_code', 'reply_json_sha256', 'envelope_sha256'))
        model.require(_integer(attempt['attempt'], 1, 32) == number, 'await attempt ordering differs')
        duration_sum += _integer(attempt['elapsed_ns'], 1, elapsed)
        # A completed wait can contain only retryable RPC failures or valid
        # predicate mismatches before its final match. Other outcomes are terminal.
        final = number == len(attempts)
        model.require(attempt['outcome'] in (('matched',) if final else ('rpc_retryable', 'predicate_mismatch')),
                      'await did not end at its first successful exact match')
        _clock(attempt.get('clock'), run_id, task, device)
        model.require(attempt['clock']['server_boot_id'] == receipt['clock']['server_boot_id'], 'await reader incarnation changed')
        expected_codes = ('PermissionDenied', 'Unavailable', 'DeadlineExceeded') if attempt['outcome'] == 'rpc_retryable' else ('OK',)
        model.require(attempt.get('rpc_code') in expected_codes, 'await RPC classification differs')
        for name in ('reply_json_sha256', 'envelope_sha256'):
            if name in attempt:
                model.require(model.digest_shape(attempt[name]), 'await digest shape differs')
        if attempt['outcome'] == 'rpc_retryable':
            model.require('reply_json_sha256' not in attempt and 'envelope_sha256' not in attempt,
                          'failed await RPC retained a prior reply')
        elif not final:
            model.require('reply_json_sha256' in attempt and attempt.get('envelope_sha256') != command['envelope_sha256'],
                          'await mismatch retained an exact match')
        if final:
            model.require(attempt['clock'] == receipt['clock'] and
                          attempt.get('reply_json_sha256') == model.digest(_bytes(receipt['reply_json'])) and
                          attempt.get('envelope_sha256') == model.digest(origin), 'final await bytes or observation differ')
    model.require(duration_sum + (len(attempts) - 1) * AWAIT_INTERVAL_NS <= elapsed,
                  'await duration omits attempts or required backoff')


def _original(receipt):
    rows = receipt['originals']
    model.require(isinstance(rows, list) and len(rows) == 1, 'one exact original required')
    row = _object(rows[0], ('envelope_json', 'envelope_sha256', 'record_sha256'))
    raw = _bytes(row['envelope_json'])
    envelope = _object(model.strict_json(raw), ('delegation', 'record', 'signature'))
    record_bytes = _bytes(envelope['record'], 64 << 10)
    model.require(row['envelope_sha256'] == model.digest(raw) and row['record_sha256'] == model.digest(record_bytes), 'original proof digest differs')
    # This is structural decoding only. The executable verifies the original
    # envelope's canonical encoding and all cryptographic material.
    record = model.strict_json(record_bytes, 64 << 10)
    model.require(isinstance(record, dict), 'record object required')
    return raw, record, row['record_sha256']


def _publication_request(receipt, command, run_id):
    raw = _bytes(receipt['request_json'])
    request = model.strict_json(raw)
    _object(request, ('entity', 'componentType', 'stream', 'frame', 'timing', 'ttlNs', 'provenance'), ('payloadJson', 'tombstone'))
    model.require(request['entity'] == 'replica-probe' and request['componentType'] == model.COMPONENT and
                  request['stream'] == model.STREAM and request['frame'] == model.FRAME,
                  'publication does not match commissioning')
    model.require(request['timing'] == {'sampleUnixNs': str(receipt['clock']['sample_unix_ns']),
                                      'clockDomain': model.sample_domain(run_id), 'errorKnown': True},
                  'original sample timing differs')
    model.require(request['ttlNs'] == str(command['ttl_ms'] * 1_000_000), 'original lifetime differs')
    model.require(request['provenance'] == {'origin': 'ORIGIN_SIMULATED', 'appVersion': 'replica-probe-v1',
        'simulation': {'runId': run_id, 'scenarioVersion': 'agentpair-1', 'seed': '1', 'clockDomain': model.sample_domain(run_id)}},
        'original simulation provenance differs')
    if command['action'] == 'tombstone':
        model.require(request.get('tombstone') is True and 'payloadJson' not in request, 'tombstone request differs')
    else:
        model.require('tombstone' not in request, 'unexpected tombstone')
        payload = model.strict_json(_bytes(request.get('payloadJson'), 4096), 4096)
        expected = {'sensor_type': 'synthetic-gas', 'value': command['value'], 'unit': 'ppm',
                    'calibration_revision': 'replica-probe-v1', 'uncertainty': 0.5}
        model.require(payload == expected and type(payload.get('value')) in (int, float), 'original scalar payload differs')
    return raw


def _window(public):
    values = []
    for name in ('not_before', 'not_after'):
        text = public.get(name)
        model.require(isinstance(text, str) and re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z', text), 'explicit fixture interval required')
        try:
            values.append(datetime.fromisoformat(text[:-1] + '+00:00'))
        except ValueError as error:
            raise model.Refused('invalid fixture interval') from error
    model.require(timedelta(0) < values[1] - values[0] <= timedelta(days=1), 'bounded fixture interval required')


@dataclass(frozen=True)
class Bundle:
    """Owned public bytes. Construction through prepare() is required."""
    expectation: bytes
    request: bytes
    origin: bytes
    received: object  # None or exact bytes; None means origin-only verification.
    expected_result: bytes


def prepare(pair, peers, *, node, run_id, task, publication_receipt, expected_epoch,
            expected_sequence, received_receipt=None, received_task=None):
    """Bind untrusted receipts to independently observed commissioning inputs.

    expected_epoch/sequence are the scenario's expected durable source version,
    not values silently adopted from the received envelope. task and received_task
    come from normal deployment inspection. No physical-time substitution occurs.
    """
    pair, peers, task = _owned(pair), _owned(peers), _owned(task)
    model.validate_peers(pair, peers)
    model.require(node in ('a', 'b') and isinstance(run_id, str) and re.fullmatch('[a-z0-9][a-z0-9-]{0,39}', run_id), 'known node and run required')
    index = ('a', 'b').index(node)
    public, pin = pair['nodes'][index]['public'], peers['nodes'][index]
    _window(public)
    generation = _task(task, public['principal'])
    epoch, sequence = _integer(expected_epoch), _integer(expected_sequence)
    publication_receipt = _owned(publication_receipt)
    action = publication_receipt.get('action') if isinstance(publication_receipt, dict) else None
    model.require(action in ('publish', 'tombstone'), 'publication acknowledgment required')
    publication, command = _receipt(publication_receipt, run_id, task, public['principal'], action)
    request = _publication_request(publication, command, run_id)
    origin, record, record_hash = _original(publication)
    reply = _object(model.strict_json(_bytes(publication['reply_json'])), ('accepted', 'signedRecordJson'))
    model.require(reply['accepted'] is True and _bytes(reply['signedRecordJson']) == origin, 'acknowledged envelope differs')
    scope = {'tenant': pair['tenant'], 'site': 'office', 'device': public['principal'], 'app': model.APP,
             'service': model.SERVICE, 'deployment_generation': generation, 'execution_domain': 'simulation',
             'publish': [{'component_type': model.COMPONENT, 'stream': model.STREAM}]}
    model.require(record.get('scope') == scope and type(record.get('epoch')) is int and record['epoch'] == epoch and
                  type(record.get('sequence')) is int and record['sequence'] == sequence, 'origin version or commissioned scope differs')
    received = None
    if received_receipt is not None:
        model.require(action == 'publish' and received_task is not None, 'remote live query requires publication and observed reader')
        remote = pair['nodes'][1 - index]['public']['principal']
        received_receipt, received_task = _owned(received_receipt), _owned(received_task)
        action = received_receipt.get('action') if isinstance(received_receipt, dict) else None
        model.require(action in ('query', 'await_query'), 'remote query receipt required')
        query, query_command = _receipt(received_receipt, run_id, received_task, remote, action)
        query_request = model.strict_json(_bytes(query['request_json']))
        model.require(query_request == {'sources': [{'device': public['principal'], 'app': model.APP, 'service': model.SERVICE,
                                                    'componentType': model.COMPONENT, 'stream': model.STREAM}], 'limit': 8},
                      'query did not select the exact remote source')
        received, _, _ = _original(query)
        query_reply = _object(model.strict_json(_bytes(query['reply_json'])), ('signedRecordJson',))
        model.require(query_reply['signedRecordJson'] == [base64.b64encode(received).decode()] and received == origin,
                      'remote reply is not the exact acknowledged envelope')
        if action == 'await_query':
            _await(query, query_command, origin, run_id, received_task, remote)
    else:
        model.require(received_task is None, 'unexpected reader without received proof')
    expectation = {'kind': KIND, 'root_der': public['root_der'], 'leaf_sha256': pin['leaf_sha256'],
                   'key_sha256': pin['device_spki_sha256'], 'scope': scope, 'epoch': epoch, 'sequence': sequence,
                   'not_before': public['not_before'], 'not_after': public['not_after']}
    expected = {'kind': KIND, 'device': scope['device'], 'deployment_generation': generation,
                'epoch': epoch, 'sequence': sequence, 'envelope_sha256': model.digest(origin),
                'record_sha256': record_hash, 'request_sha256': model.digest(request),
                'leaf_sha256': pin['leaf_sha256'], 'exact_received_bytes': received is not None}
    return Bundle(model.json_bytes(expectation), request, origin, received, model.json_bytes(expected))


def verify(bundle, *, write_public, execute, prefix):
    """Stage O_EXCL public inputs and run the pinned owned-node utility once.

    write_public must be Node.write (protected root-owned 0600 files). execute
    must be Node.inside, with its bounded command/stdout/stderr receipt capture.
    A failed/ambiguous call is terminal; this adapter never retries. A successful
    result proves only the utility's documented current cryptographic checks.
    """
    model.require(isinstance(bundle, Bundle) and callable(write_public) and callable(execute), 'prepared proof and owned-node callbacks required')
    model.require(isinstance(prefix, str) and re.fullmatch(re.escape(model.TOOLS) + r'/proof-[a-z0-9][a-z0-9-]{0,63}', prefix), 'fixed protected proof prefix required')
    files = [('expectation', bundle.expectation), ('request', bundle.request), ('origin', bundle.origin)]
    if bundle.received is not None:
        files.append(('received', bundle.received))
    model.require(all(isinstance(raw, bytes) and 0 < len(raw) <= MAX_INPUT for _, raw in files), 'bounded proof files required')
    expected = model.strict_json(bundle.expected_result, MAX_RESULT)
    argv = [model.TOOLS + '/proof']
    for name, raw in files:
        path = prefix + '.' + name + '.json'
        write_public(path, raw)
        argv.extend(('--' + name, path))
    result = execute(*argv, timeout=25, check=False)
    model.require(result.returncode == 0, 'independent current proof verification refused')
    receipt = model.strict_json(result.stdout, MAX_RESULT)
    _object(receipt, tuple(expected) + ('authority_time_evidence',))
    model.require(all(type(receipt[k]) is type(value) and receipt[k] == value for k, value in expected.items()) and
                  model.digest_shape(receipt['authority_time_evidence']), 'independent proof receipt differs')
    return receipt
