"""Ordinary-app scalar transfer acceptance over two owned native agents.

No action runs on import. The parent Runner owns every resource and cleanup.
Empty reads are observations, not proofs of durable tombstone/floor admission.
"""
import base64
import copy

import control
import model
import proof


def _bytes(value, limit=512 << 10):
    model.require(isinstance(value, str) and len(value) <= 4 * ((limit + 2) // 3), 'bounded receipt bytes required')
    try:
        raw = base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as error:
        raise model.Refused('invalid receipt byte encoding') from error
    model.require(len(raw) <= limit and base64.b64encode(raw).decode() == value, 'noncanonical receipt byte encoding')
    return raw


class Scenario:
    def __init__(self, runner):
        model.require(runner.runtime_state == 'deploy' and 'proof' in runner.inputs['binaries'], 'completed normal deployments and proof tool required')
        self.runner, self.nodes = runner, runner.nodes
        self.elapsed = 0
        self.sample_raw, self.sample_ns = control.sample_clock(runner.inputs['run_id'], 0)
        self.boots = {}
        self.partition = {}
        self.publications = {}
        self.summary = {'version': 1, 'scalar_transfer': False, 'partition_local_progress': False,
                        'remote_expiry_observed': False, 'repair_exact_originals': False,
                        'tombstone_consistent_absence': False, 'receiver_restart_fresh_transfer': False,
                        'retained_remote_original_after_restart_proven': False,
                        'durable_remote_tombstone_proven': False, 'durable_remote_floor_proven': False,
                        'full_native_repair_gate_complete': False}
        for node in self.nodes.values():
            model.require(node.task_state is not None and node.generation == 1 and node.clock_raw == self.sample_raw,
                          'fresh fixed-clock ordinary probe required')
            node.runtime_identity = runner.current_container_id(node.name)

    def step(self, name):
        self.runner.step('replication-' + name)

    def clock(self, node, receipt, *, new_boot=False):
        clock = receipt.get('clock')
        device = self.runner.pair['nodes'][('a', 'b').index(node.key)]['public']['principal']
        proof._clock(clock, self.runner.inputs['run_id'], node.task_state, device)
        model.require(clock['sample_unix_ns'] == self.sample_ns, 'source clock did not reach synchronized value')
        if node.key in self.boots:
            model.require((clock['server_boot_id'] != self.boots[node.key]) if new_boot else
                          (clock['server_boot_id'] == self.boots[node.key]), 'unexpected agent clock incarnation')
        self.boots[node.key] = clock['server_boot_id']

    def query(self, key, expect, envelope=None, *, new_boot=False):
        node = self.nodes[key]
        fields = {'expect': expect}
        if envelope is not None:
            fields['envelope_sha256'] = envelope
        receipt = node.command('await_query', **fields)
        self.clock(node, receipt, new_boot=new_boot)
        peer = self.runner.pair['nodes'][1 - ('a', 'b').index(key)]['public']['principal']
        request = model.strict_json(_bytes(receipt.get('request_json')))
        model.require(request == {'sources': [{'device': peer, 'app': model.APP, 'service': model.SERVICE,
                                              'componentType': model.COMPONENT, 'stream': model.STREAM}], 'limit': 8},
                      'exact remote selector required')
        model.require(receipt.get('rpc_code') == 'OK' and isinstance(receipt.get('originals'), list), 'successful bounded query required')
        reply = model.strict_json(_bytes(receipt.get('reply_json')))
        model.require(isinstance(reply, dict) and set(reply) <= {'signedRecordJson'} and
                      isinstance(reply.get('signedRecordJson', []), list) and len(reply.get('signedRecordJson', [])) <= 1,
                      'bounded no-cursor reply required')
        originals = receipt['originals']
        model.require(len(originals) == len(reply.get('signedRecordJson', [])), 'query evidence count differs')
        for original, encoded in zip(originals, reply.get('signedRecordJson', [])):
            model.require(isinstance(original, dict) and set(original) == {'envelope_json', 'envelope_sha256', 'record_sha256'} and
                          original['envelope_json'] == encoded and model.digest(_bytes(encoded)) == original['envelope_sha256'],
                          'exact original query bytes required')
        if expect == 'absent':
            model.require(originals == [], 'remote source remains present')
        elif expect == 'present':
            model.require(len(originals) == 1 and originals[0]['envelope_sha256'] == envelope, 'expected original missing')
        attempts = receipt.get('attempts', [])
        duration = receipt.get('await_elapsed_ns')
        model.require(isinstance(attempts, list) and 1 <= len(attempts) <= 32 and attempts[-1].get('outcome') == 'matched' and
                      type(duration) is int and 0 < duration <= 10_000_000_000, 'bounded matched read evidence required')
        return receipt

    def verify(self, key, label, publication, epoch, sequence, received=None):
        sender = self.nodes[key]
        other = self.nodes['b' if key == 'a' else 'a']
        bundle = proof.prepare(self.runner.pair, self.runner.peers, node=key, run_id=self.runner.inputs['run_id'],
                               task=sender.task_state, publication_receipt=publication, expected_epoch=epoch,
                               expected_sequence=sequence, received_receipt=received,
                               received_task=other.task_state if received is not None else None)
        executor = other if received is not None else sender
        result = proof.verify(bundle, write_public=executor.write, execute=executor.inside,
                              prefix=model.TOOLS + '/proof-' + label + '-' + key)
        self.runner.save('proof-' + label + '-' + key + '.json', result)
        return result

    def publish(self, key, value, epoch, sequence, label):
        receipt = self.nodes[key].command('publish', value=value, ttl_ms=30000)
        self.clock(self.nodes[key], receipt)
        self.verify(key, label, receipt, epoch, sequence)
        self.publications[key] = (receipt, epoch, sequence)
        return receipt

    def transfer(self, key, label):
        publication, epoch, sequence = self.publications[key]
        original = publication['originals'][0]['envelope_sha256']
        received = self.query('b' if key == 'a' else 'a', 'present', original)
        return self.verify(key, label, publication, epoch, sequence, received)

    def native(self, label):
        """Local-namespace telemetry plus authorized reads, never TLS attestation."""
        rows = {}
        for key, node in self.nodes.items():
            mesh = node.read(model.config_path(key) + '/local-mesh.json')
            expected = model.json_bytes(model.mesh_configuration(self.runner.pair, key, self.runner.addresses))
            model.require(mesh == node.mesh_raw == expected, 'native carrier configuration changed')
            observed = {}
            def available():
                result = node.cli('net', 'status', timeout=5, check=False)
                if result.returncode:
                    return False
                value = model.strict_json(result.stdout)
                observed['status'] = value
                return value.get('runtimeAvailable') is True and type(value.get('authenticatedPeers')) is int and value['authenticatedPeers'] > 0
            node.wait(available, 20)
            topology = node.cli('net', 'topology', '--observation', timeout=5, check=False)
            # Admin UDS has no attested remote reporter identity in the pinned
            # CLI. Keep that limitation; do not manufacture one from metadata.
            topo = None
            if topology.stdout:
                try:
                    topo = model.strict_json(topology.stdout)
                except model.Refused:
                    pass
            rows[key] = {'container_id': node.runtime_identity, 'agent_pid': node.agent_pid,
                         'expected_self': self.runner.pair['nodes'][('a', 'b').index(key)]['public']['principal'],
                         'mesh_sha256': model.digest(mesh), 'local_status': observed['status'],
                         'topology_exit': topology.returncode, 'topology_observation': topo,
                         'reporter_attestation_claimed': False, 'world_tls_ready_claimed': False}
        self.runner.save('native-' + label + '.json', rows)

    def advance(self, seconds):
        model.require(type(seconds) is int and self.elapsed < seconds <= 600, 'source time may only advance')
        raw, instant = control.sample_clock(self.runner.inputs['run_id'], seconds)
        rows = {}
        for key, node in self.nodes.items():
            model.require(node.clock_raw == self.sample_raw, 'previous synchronized clock differs')
            node.atomic_write(model.config_path(key) + '/simclock.json', raw, previous=node.clock_raw)
            rows[key] = {'old_sha256': model.digest(node.clock_raw), 'new_sha256': model.digest(raw)}
            node.clock_raw = raw  # A partial advancement is never rolled back.
        for node in self.nodes.values():
            model.require(node.read(model.config_path(node.key) + '/simclock.json') == raw, 'sample clock readback differs')
        self.elapsed, self.sample_raw, self.sample_ns = seconds, raw, instant
        self.runner.save('sample-clock-' + str(seconds) + '.json', {'nodes': rows, 'sample_unix_ns': instant,
                                                                 'domain': model.sample_domain(self.runner.inputs['run_id']),
                                                                 'physical_authority_clock_changed': False})

    def cut(self):
        for key, node in self.nodes.items():
            peer = self.runner.addresses['b' if key == 'a' else 'a']
            rows = control.partition_rules(self.runner.name, peer)
            self.partition[key] = rows
            for row in rows:
                node.inside('iptables', '-w', '2', '-N', row['chain'])
                for rule in row['rules']:
                    node.inside('iptables', '-w', '2', '-A', row['chain'], *rule)
                node.inside('iptables', '-w', '2', '-I', row['direction'], '1', '-j', row['chain'])
        self.partition_observation('installed')

    def partition_observation(self, label, require_drops=False):
        evidence = {}
        for key, node in self.nodes.items():
            records = []
            for row in self.partition[key]:
                rules = node.inside('iptables', '-w', '2', '-S', row['chain']).stdout
                control.validate_chain(rules, row)
                control.validate_jump(node.inside('iptables', '-w', '2', '-S', row['direction']).stdout, row)
                counters = node.inside('iptables', '-w', '2', '-n', '-v', '-x', '-L', row['chain']).stdout
                records.append({'chain': row['chain'], 'direction': row['direction'], 'rules': rules.decode('ascii'),
                                'counter_listing': counters.decode('ascii'), 'dropped_packets': control.chain_drops(counters, row)})
            evidence[key] = {'container_id': node.runtime_identity, 'peer_ipv4': self.runner.addresses['b' if key == 'a' else 'a'],
                             'chains': records}
        if require_drops:
            model.require(all(sum(row['dropped_packets'] for row in item['chains']) > 0 for item in evidence.values()),
                          'actual carrier drops not observed on both nodes')
        self.runner.save('partition-' + label + '.json', evidence)

    def restore(self):
        self.partition_observation('before-restore', require_drops=True)
        for key, node in self.nodes.items():
            for row in self.partition[key]:
                control.validate_chain(node.inside('iptables', '-w', '2', '-S', row['chain']).stdout, row)
                node.inside('iptables', '-w', '2', '-D', row['direction'], '-j', row['chain'])
                node.inside('iptables', '-w', '2', '-F', row['chain'])
                node.inside('iptables', '-w', '2', '-X', row['chain'])
                listing = node.inside('iptables', '-w', '2', '-S').stdout
                model.require(row['chain'].encode() not in listing, 'owned partition chain remains')
        self.runner.save('partition-restored.json', {'owned_chains_absent': True, 'containers': {k: n.runtime_identity for k, n in self.nodes.items()}})

    def restart_receiver(self):
        node = self.nodes['b']
        original = copy.deepcopy(node.task_state)
        old_pid, old_boot = node.agent_pid, self.boots['b']
        node.stop_agent()
        node.start_agent('restart')  # Existing stores only; initializer is never called.
        model.require(node.agent_pid != old_pid, 'receiver process did not change')
        current = node.observe_task()
        model.require(current == original, 'old ordinary process changed before mount refusal check')
        refused = node.command('query', expect_refusal=True)
        self.runner.save('restart-old-reader.json', {'old_task': original, 'old_agent_pid': old_pid,
                                                    'new_agent_pid': node.agent_pid, 'old_server_boot_id': old_boot,
                                                    'refused_sequence': refused['sequence'], 'error': refused['error'],
                                                    'no_originals': True, 'specific_authority_predicate_proven': False})
        node.cli('device', 'apps', 'remove', model.APP, '--force', timeout=45)
        self.runner.deploy_node(node)
        model.require(node.task_state['generation'] != original['generation'] and
                      node.task_state['socket_directory'] != original['socket_directory'] and
                      (node.task_state['pid'], node.task_state['start_ticks']) != (original['pid'], original['start_ticks']),
                      'fresh ordinary reader generation/socket/process required')
        self.native('restart')
        self.query('b', 'ready', new_boot=True)
        self.query('a', 'ready')
        self.runner.save('restart-new-reader.json', {'old_generation': original['generation'], 'new_task': node.task_state,
                                                    'old_server_boot_id': old_boot, 'new_server_boot_id': self.boots['b'],
                                                    'stores_reinitialized': False})

    def execute(self):
        self.step('baseline')
        self.native('baseline')
        for key in ('a', 'b'):
            self.query(key, 'ready')
        self.publish('a', 11, 1, 1, 'baseline-origin')
        self.publish('b', 22, 1, 1, 'baseline-origin')
        for key in ('a', 'b'):
            self.transfer(key, 'baseline-transferred')
        self.summary['scalar_transfer'] = True

        self.step('partition')
        self.cut()
        self.advance(31)
        # Observe expiry before writing replacement samples, then local progress
        # while their exact new originals remain unavailable to the other node.
        for key in ('a', 'b'):
            self.query(key, 'absent')
        self.summary['remote_expiry_observed'] = True
        self.publish('a', 111, 1, 2, 'partition-origin')
        self.publish('b', 222, 1, 2, 'partition-origin')
        for key in ('a', 'b'):
            self.query(key, 'absent')
        self.summary['partition_local_progress'] = True

        self.step('repair')
        self.restore()
        self.native('rejoined')
        for key in ('a', 'b'):
            self.transfer(key, 'repaired-original')
        self.summary['repair_exact_originals'] = True

        self.step('tombstone')
        # A's previous version was positively read on B immediately above.
        tombstone = self.nodes['a'].command('tombstone', ttl_ms=30000)
        self.clock(self.nodes['a'], tombstone)
        self.verify('a', 'tombstone-origin', tombstone, 1, 3)
        absent = self.query('b', 'absent')
        self.runner.save('tombstone-observation.json', {'prior_envelope_sha256': self.publications['a'][0]['originals'][0]['envelope_sha256'],
                                                       'tombstone_envelope_sha256': tombstone['originals'][0]['envelope_sha256'],
                                                       'sample_unix_ns': self.sample_ns, 'absence_sequence': absent['sequence'],
                                                       'durable_remote_admission_proven': False,
                                                       'other_current_authority_filters_excluded': False})
        self.summary['tombstone_consistent_absence'] = True

        self.step('restart')
        self.restart_receiver()
        self.publish('a', 333, 1, 4, 'restart-origin')
        self.publish('b', 444, 2, 1, 'restart-origin')
        for key in ('a', 'b'):
            self.transfer(key, 'restart-transferred')
        self.summary['receiver_restart_fresh_transfer'] = True
        for node in self.nodes.values():
            node.command('finish')
        self.runner.save('replication-observations.json', self.summary)
        return self.summary


def execute(runner):
    return Scenario(runner).execute()
