"""Adapter checks only. Synthetic envelopes below are deliberately not signed.

The injected executable response tests orchestration, not cryptographic trust or
authenticated-time acquisition. The separately qualified Go utility owns those.
"""
import base64
import copy
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

import model
import proof
from test_model import pair_fixture


def b64(raw):
    return base64.b64encode(raw).decode()


def encoded(value):
    return b64(model.json_bytes(value))


def fixture(*, action='publish', epoch=1, sequence=1):
    pair, peers = pair_fixture()
    tasks = [{'app': model.APP, 'service': model.SERVICE, 'device': item['public']['principal'],
              'generation': 40 + index} for index, item in enumerate(pair['nodes'])]
    run_id = 'proof-unit-test'
    domain = model.sample_domain(run_id)
    command = {'version': 1, 'sequence': 2, 'action': action, 'ttl_ms': 30000}
    if action == 'publish': command['value'] = 23.5
    request = {'entity': 'replica-probe', 'componentType': model.COMPONENT, 'stream': model.STREAM,
               'frame': model.FRAME, 'timing': {'sampleUnixNs': '1767225600000000000', 'clockDomain': domain, 'errorKnown': True},
               'ttlNs': '30000000000', 'provenance': {'origin': 'ORIGIN_SIMULATED', 'appVersion': 'replica-probe-v1',
                   'simulation': {'runId': run_id, 'scenarioVersion': 'agentpair-1', 'seed': '1', 'clockDomain': domain}}}
    if action == 'publish':
        request['payloadJson'] = encoded({'value': command['value'], 'unit': 'ppm', 'sensor_type': 'synthetic-gas',
                                         'calibration_revision': 'replica-probe-v1', 'uncertainty': 0.5})
    else:
        request['tombstone'] = True
    scope = {'tenant': pair['tenant'], 'site': 'office', 'device': tasks[0]['device'], 'app': model.APP, 'service': model.SERVICE,
             'deployment_generation': tasks[0]['generation'], 'execution_domain': 'simulation',
             'publish': [{'component_type': model.COMPONENT, 'stream': model.STREAM}]}
    record = model.json_bytes({'scope': scope, 'epoch': epoch, 'sequence': sequence})
    # Whitespace is intentional: preparation must preserve bytes, not create
    # a canonical-looking surrogate. The actual utility will reject this fake.
    origin = json.dumps({'delegation': {'claims': b64(b'unsigned test claims')}, 'record': b64(record),
                         'signature': b64(b'unsigned test signature')}, indent=1).encode()
    original = {'envelope_json': b64(origin), 'envelope_sha256': model.digest(origin), 'record_sha256': model.digest(record)}

    def receipt(action, command, request, reply, task):
        command_raw = model.json_bytes(command)
        return {'version': 1, 'run_id': run_id, 'sequence': command['sequence'], 'action': action, 'status': 'complete',
                'rpc_code': 'OK', 'validation': 'socket_scope_and_structure_only',
                'command_json': b64(command_raw), 'command_sha256': model.digest(command_raw),
                'clock': {'sample_unix_ns': 1767225600000000000, 'sample_clock_domain': domain,
                          'deployment_generation': task['generation'], 'server_boot_id': 'a' * 32,
                          'physical_earliest_unix_ns': 1794441600000000000, 'physical_latest_unix_ns': 1794441600100000000,
                          'round_trip_ns': 1000000}, 'request_json': encoded(request), 'reply_json': encoded(reply),
                'originals': [copy.deepcopy(original)]}
    publication = receipt(action, command, request, {'accepted': True, 'signedRecordJson': b64(origin)}, tasks[0])
    query_request = {'sources': [{'device': tasks[0]['device'], 'app': model.APP, 'service': model.SERVICE,
                                 'componentType': model.COMPONENT, 'stream': model.STREAM}], 'limit': 8}
    received = receipt('query', {'version': 1, 'sequence': 3, 'action': 'query'}, query_request,
                       {'signedRecordJson': [b64(origin)]}, tasks[1])
    return dict(pair=pair, peers=peers, node='a', run_id=run_id, task=tasks[0], publication_receipt=publication,
                expected_epoch=epoch, expected_sequence=sequence, received_receipt=received, received_task=tasks[1])


def update_encoded(receipt, field, mutate):
    value = model.strict_json(base64.b64decode(receipt[field]))
    mutate(value)
    receipt[field] = encoded(value)


def await_fixture():
    inputs = fixture()
    receipt = inputs['received_receipt']
    command = {'version': 1, 'sequence': receipt['sequence'], 'action': 'await_query', 'expect': 'present',
               'envelope_sha256': receipt['originals'][0]['envelope_sha256']}
    receipt.update(action='await_query', command_json=encoded(command), command_sha256=model.digest(model.json_bytes(command)),
                   await_elapsed_ns=250000000)
    receipt['attempts'] = [
        {'attempt': 1, 'clock': copy.deepcopy(receipt['clock']), 'rpc_code': 'Unavailable',
         'outcome': 'rpc_retryable', 'elapsed_ns': 1000000},
        {'attempt': 2, 'clock': copy.deepcopy(receipt['clock']), 'rpc_code': 'OK', 'outcome': 'matched',
         'elapsed_ns': 1000000, 'reply_json_sha256': model.digest(base64.b64decode(receipt['reply_json'])),
         'envelope_sha256': command['envelope_sha256']}]
    return inputs


class ProofTests(unittest.TestCase):
    def test_exact_owned_bytes_and_independent_expectation(self):
        inputs = fixture()
        original = base64.b64decode(inputs['publication_receipt']['originals'][0]['envelope_json'])
        request = base64.b64decode(inputs['publication_receipt']['request_json'])
        bundle = proof.prepare(**inputs)
        self.assertEqual(bundle.origin, original)
        self.assertEqual(bundle.received, original)
        self.assertEqual(bundle.request, request)
        expectation = model.strict_json(bundle.expectation)
        self.assertEqual(expectation['root_der'], inputs['pair']['nodes'][0]['public']['root_der'])
        self.assertEqual(expectation['leaf_sha256'], inputs['peers']['nodes'][0]['leaf_sha256'])
        self.assertEqual(expectation['key_sha256'], inputs['peers']['nodes'][0]['device_spki_sha256'])
        self.assertEqual(expectation['scope']['deployment_generation'], 40)
        self.assertEqual(expectation['epoch'], 1)
        self.assertEqual(expectation['sequence'], 1)  # Command sequence is 2.
        inputs['task']['generation'] = 88
        inputs['publication_receipt']['originals'].clear()
        inputs['pair']['nodes'][0]['public']['root_der'] = ''
        self.assertEqual(bundle.origin, original)
        self.assertEqual(model.strict_json(bundle.expectation), expectation)

    def test_origin_only_tombstone_and_explicit_new_epoch(self):
        for action in ('publish', 'tombstone'):
            inputs = fixture(action=action, epoch=2, sequence=1)
            inputs['received_receipt'] = inputs['received_task'] = None
            bundle = proof.prepare(**inputs)
            self.assertIsNone(bundle.received)
            self.assertFalse(model.strict_json(bundle.expected_result)['exact_received_bytes'])
            self.assertEqual(model.strict_json(bundle.expectation)['epoch'], 2)
        with self.assertRaises(model.Refused): proof.prepare(**fixture(action='tombstone'))

    def test_unknown_and_malformed_acknowledgments_never_become_inputs(self):
        changes = [lambda r: r.update(status='unknown_outcome'), lambda r: r.update(rpc_code='DeadlineExceeded'),
                   lambda r: r.update(error='publication_outcome_unknown'), lambda r: r.update(originals=[]),
                   lambda r: r['originals'].append(copy.deepcopy(r['originals'][0])),
                   lambda r: r.update(reply_json=encoded({'accepted': False, 'signedRecordJson': r['originals'][0]['envelope_json']})),
                   lambda r: r.update(reply_json=encoded({'accepted': True, 'signedRecordJson': b64(b'other')})),
                   lambda r: r['originals'][0].update(envelope_sha256='f' * 64),
                   lambda r: r['originals'][0].update(record_sha256='f' * 64),
                   lambda r: r.update(command_sha256='f' * 64),
                   lambda r: r.update(sequence=True),
                   lambda r: r.update(request_json=b64(b'{"entity":1,"entity":2}'))]
        for change in changes:
            with self.subTest(change=change):
                inputs = fixture(); change(inputs['publication_receipt'])
                with self.assertRaises(model.Refused): proof.prepare(**inputs)

    def test_expected_scope_and_version_are_not_adopted_from_proof(self):
        changes = [lambda x: x.update(expected_epoch=2), lambda x: x.update(expected_sequence=2),
                   lambda x: x.update(expected_epoch=True), lambda x: x.update(expected_sequence=1 << 64),
                   lambda x: x.update(node='b'), lambda x: x.update(run_id='other-run'),
                   lambda x: x['task'].update(generation=41), lambda x: x['task'].update(device=x['received_task']['device']),
                   lambda x: x['peers']['nodes'].reverse(),
                   lambda x: x['pair']['nodes'][0]['public'].update(not_after='2026-11-14T00:00:00Z'),
                   lambda x: x['pair']['nodes'][0]['public'].update(not_after='2026-11-12T00:00:00Z'),
                   lambda x: x['pair']['nodes'][0]['public'].update(not_after='2026-11-12T01:00:00'),
                   lambda x: x['pair']['nodes'][0]['public'].update(root_der=b64(b'wrong root'))]
        for change in changes:
            with self.subTest(change=change):
                inputs = fixture(); change(inputs)
                with self.assertRaises(model.Refused): proof.prepare(**inputs)

    def test_original_sample_payload_lifetime_and_provenance_required(self):
        changes = [lambda q: q.update(ttlNs='60000000000'), lambda q: q['timing'].update(sampleUnixNs='1767225600000000001'),
                   lambda q: q['timing'].update(clockDomain='utc'), lambda q: q['timing'].update(ageHintNs='1'),
                   lambda q: q['frame'].update(revision='other'), lambda q: q.update(tombstone=True),
                   lambda q: q['provenance'].update(origin='ORIGIN_MEASURED'),
                   lambda q: q['provenance']['simulation'].update(runId='another'),
                   lambda q: q['provenance']['simulation'].update(seed='2'),
                   lambda q: q.update(payloadJson=encoded({'value': 23.5}))]
        for change in changes:
            with self.subTest(change=change):
                inputs = fixture(); update_encoded(inputs['publication_receipt'], 'request_json', change)
                with self.assertRaises(model.Refused): proof.prepare(**inputs)

    def test_remote_query_exact_device_reader_and_reply_required(self):
        changes = [lambda x: x.update(received_task=None), lambda x: x['received_task'].update(generation=40),
                   lambda x: x['received_task'].update(device=x['task']['device']),
                   lambda x: x['received_receipt'].update(originals=[]),
                   lambda x: update_encoded(x['received_receipt'], 'request_json', lambda q: q['sources'][0].pop('device')),
                   lambda x: update_encoded(x['received_receipt'], 'request_json', lambda q: q.update(after='cursor')),
                   lambda x: update_encoded(x['received_receipt'], 'reply_json', lambda q: q.update(next='cursor')),
                   lambda x: update_encoded(x['received_receipt'], 'reply_json', lambda q: q.update(signedRecordJson=[b64(b'other')]))]
        for change in changes:
            with self.subTest(change=change):
                inputs = fixture(); change(inputs)
                with self.assertRaises(model.Refused): proof.prepare(**inputs)
        inputs = fixture()
        row = inputs['received_receipt']['originals'][0]
        changed = base64.b64decode(row['envelope_json']) + b'\n'
        row.update(envelope_json=b64(changed), envelope_sha256=model.digest(changed))
        inputs['received_receipt']['reply_json'] = encoded({'signedRecordJson': [b64(changed)]})
        with self.assertRaises(model.Refused): proof.prepare(**inputs)

    def test_bounds_duplicate_keys_and_noncanonical_base64(self):
        for field, content in [('request_json', b' ' * (proof.MAX_INPUT + 1)), ('reply_json', b'{}' + b' ' * proof.MAX_INPUT),
                               ('command_json', b' ' * 1025)]:
            inputs = fixture(); inputs['publication_receipt'][field] = b64(content)
            with self.assertRaises(model.Refused): proof.prepare(**inputs)
        inputs = fixture(); inputs['publication_receipt']['request_json'] += '\n'
        with self.assertRaises(model.Refused): proof.prepare(**inputs)
        inputs = fixture(); inputs['publication_receipt'] = b'{"version":1,"version":1}'
        with self.assertRaises(model.Refused): proof.prepare(**inputs)

    def test_await_requires_final_exact_present_match_and_current_reader(self):
        inputs = await_fixture()
        bundle = proof.prepare(**inputs)
        self.assertEqual(bundle.received, bundle.origin)
        changes = [lambda r: r.update(attempts=[]), lambda r: r.update(attempts=r['attempts'] * 17),
                   lambda r: r['attempts'][0].update(attempt=2), lambda r: r['attempts'][0].update(outcome='matched'),
                   lambda r: r['attempts'][0].update(outcome='rpc_refused'), lambda r: r['attempts'][0].update(rpc_code='Internal'),
                   lambda r: r['attempts'][-1].update(outcome='predicate_mismatch'),
                   lambda r: r['attempts'][-1].update(reply_json_sha256='f' * 64),
                   lambda r: r['attempts'][-1].update(envelope_sha256='f' * 64),
                   lambda r: r['attempts'][-1]['clock'].update(deployment_generation=42),
                   lambda r: r['attempts'][-1]['clock'].update(sample_unix_ns=1767225600000000001),
                   lambda r: r['attempts'][0]['clock'].update(server_boot_id='b' * 32),
                   lambda r: r['attempts'][0].update(reply_json_sha256='d' * 64),
                   lambda r: r['attempts'][-1].update(elapsed_ns=-1),
                   lambda r: r.update(await_elapsed_ns=0),
                   lambda r: r.update(await_elapsed_ns=10_000_000_001),
                   lambda r: r.update(await_elapsed_ns=201000000)]
        for change in changes:
            with self.subTest(change=change):
                inputs = await_fixture(); change(inputs['received_receipt'])
                with self.assertRaises(model.Refused): proof.prepare(**inputs)
        for predicate in ('ready', 'absent'):
            inputs = await_fixture(); receipt = inputs['received_receipt']
            command = model.strict_json(base64.b64decode(receipt['command_json']))
            command['expect'] = predicate; del command['envelope_sha256']
            receipt.update(command_json=encoded(command), command_sha256=model.digest(model.json_bytes(command)))
            with self.assertRaises(model.Refused): proof.prepare(**inputs)

    def test_await_durations_are_per_attempt_and_not_a_substitute_clock(self):
        inputs = await_fixture()
        receipt = inputs['received_receipt']
        receipt['attempts'][0]['elapsed_ns'] = 20_000_000
        receipt['attempts'][1]['elapsed_ns'] = 1_000_000
        bundle = proof.prepare(**inputs)  # Durations may decrease.
        self.assertEqual(model.strict_json(bundle.expectation)['not_before'], inputs['pair']['nodes'][0]['public']['not_before'])
        self.assertNotIn('clock', model.strict_json(bundle.expectation))
        first = receipt['attempts'][0]
        first.update(outcome='predicate_mismatch', rpc_code='OK', reply_json_sha256='c' * 64)
        proof.prepare(**inputs)  # An empty or different valid snapshot may retry.
        first['envelope_sha256'] = receipt['originals'][0]['envelope_sha256']
        with self.assertRaises(model.Refused): proof.prepare(**inputs)

    def test_single_execution_fixed_arguments_and_public_files_only(self):
        bundle = proof.prepare(**fixture())
        expected = model.strict_json(bundle.expected_result)
        output = dict(expected, authority_time_evidence='e' * 64)
        writes = []
        execute = Mock(return_value=SimpleNamespace(returncode=0, stdout=model.json_bytes(output)))
        prefix = model.TOOLS + '/proof-baseline-a'
        actual = proof.verify(bundle, write_public=lambda path, raw: writes.append((path, raw)), execute=execute, prefix=prefix)
        self.assertEqual(actual, output)
        self.assertEqual([raw for _, raw in writes], [bundle.expectation, bundle.request, bundle.origin, bundle.received])
        execute.assert_called_once_with(model.TOOLS + '/proof', '--expectation', prefix + '.expectation.json',
            '--request', prefix + '.request.json', '--origin', prefix + '.origin.json', '--received', prefix + '.received.json',
            timeout=25, check=False)
        self.assertNotIn('clock', ' '.join(execute.call_args.args))

    def test_execution_failure_malformed_or_wrong_receipt_never_retried(self):
        bundle = proof.prepare(**fixture())
        good = dict(model.strict_json(bundle.expected_result), authority_time_evidence='e' * 64)
        bad = [SimpleNamespace(returncode=1, stdout=model.json_bytes(good)), SimpleNamespace(returncode=124, stdout=b''),
               SimpleNamespace(returncode=0, stdout=b'{}'), SimpleNamespace(returncode=0, stdout=b'x' * (proof.MAX_RESULT + 1))]
        for key, value in [('exact_received_bytes', False), ('sequence', True), ('envelope_sha256', 'f' * 64),
                           ('leaf_sha256', 'f' * 64), ('authority_time_evidence', ''), ('device', 'other'), ('clock', 'wall')]:
            wrong = dict(good); wrong[key] = value
            bad.append(SimpleNamespace(returncode=0, stdout=model.json_bytes(wrong)))
        for result in bad:
            with self.subTest(result=result):
                execute = Mock(return_value=result)
                with self.assertRaises(model.Refused):
                    proof.verify(bundle, write_public=Mock(), execute=execute, prefix=model.TOOLS + '/proof-failure')
                execute.assert_called_once()
        for error in (TimeoutError('ambiguous'), OSError('write failed')):
            execute, write = Mock(), Mock()
            if isinstance(error, TimeoutError): execute.side_effect = error
            else: write.side_effect = error
            with self.assertRaises(type(error)):
                proof.verify(bundle, write_public=write, execute=execute, prefix=model.TOOLS + '/proof-exception')
            self.assertEqual(execute.call_count, 1 if isinstance(error, TimeoutError) else 0)

    def test_no_staging_for_unsafe_prefix_and_no_received_flag_for_origin_only(self):
        inputs = fixture(); inputs['received_receipt'] = inputs['received_task'] = None
        bundle = proof.prepare(**inputs)
        for prefix in ('/tmp/proof-a', model.TOOLS + '/proof-../a', model.TOOLS + '/proof-a/next', model.TOOLS + '/proof-' + 'x' * 65):
            write, execute = Mock(), Mock()
            with self.assertRaises(model.Refused): proof.verify(bundle, write_public=write, execute=execute, prefix=prefix)
            write.assert_not_called(); execute.assert_not_called()
        good = dict(model.strict_json(bundle.expected_result), authority_time_evidence='e' * 64)
        execute = Mock(return_value=SimpleNamespace(returncode=0, stdout=model.json_bytes(good)))
        write = Mock()
        proof.verify(bundle, write_public=write, execute=execute, prefix=model.TOOLS + '/proof-origin')
        self.assertEqual(write.call_count, 3)
        self.assertNotIn('--received', execute.call_args.args)


if __name__ == '__main__':
    unittest.main()
