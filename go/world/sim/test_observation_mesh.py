"""Functional checks for worker-export-only, one-hop sensor synchronization."""
import base64
import copy
import hashlib
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from sim.observation_mesh import ObservationMesh, MAX_RECORD_BYTES, MAX_SOURCES


NODES = ['hub', 'atlas', 'iris', 'echo']
CATALOG = [
    {'sensor_id': 'camera-a', 'kind': 'camera', 'owner': 'hub'},
    {'sensor_id': 'camera-b', 'kind': 'camera', 'owner': 'hub'},
    {'sensor_id': 'gas-1', 'kind': 'gas', 'owner': 'hub'},
    *[{'sensor_id': 'lidar-' + node, 'kind': 'lidar', 'owner': node}
      for node in ('atlas', 'iris', 'echo')],
]


def record(sensor='camera-a', sequence=1, data=None, age=0):
    source = next(item for item in CATALOG if item['sensor_id'] == sensor)
    result = {
        'key': {'tenant': 'simulation', 'site': 'studio-01', 'entity': 'run/' + sensor,
                'type': 'wendy.sensor.v1', 'source': {'device': source['owner'],
                'app': 'world-office-sensors', 'service': sensor, 'stream': 'samples'}},
        'version': {'epoch': 1, 'sequence': sequence},
        'timing': {'sample_time': '2026-01-01T00:00:01Z', 'clock_domain': 'simulation:run',
                   'age_hint_ns': age, 'error_known': True, 'error_bound_ns': 0},
        'ttl_ns': 2_000_000_000,
        'frame': {'id': 'office-floor', 'revision': 'test'},
        'payload': {'sensor_id': sensor, 'sample_ns': 1_000_000_000, 'kind': source['kind']},
        'provenance': {'origin': 'simulated', 'simulation': {'run_id': 'run'}},
        'tombstone': False,
    }
    if data is not None:
        result['payload']['evidence'] = {'sha256': hashlib.sha256(data).hexdigest(),
                                        'size': len(data), 'content_type': 'image/png'}
    return result


def media(data):
    return {hashlib.sha256(data).hexdigest(): base64.b64encode(data).decode('ascii')}


def graph(*pairs):
    return SimpleNamespace(edges={str(i): {'a': a, 'b': b, 'up': True, 'packets': 0}
                                  for i, (a, b) in enumerate(pairs)})


class ObservationMeshTests(unittest.TestCase):
    def setUp(self):
        self.transport = ObservationMesh(NODES, CATALOG)

    def test_one_hop_requires_receiver_worker_export_before_forwarding(self):
        original = record(data=b'camera pixels')
        blobs = media(b'camera pixels')
        mesh = graph(('hub', 'atlas'), ('atlas', 'iris'), ('iris', 'echo'))
        self.transport.update_export('hub', [original], blobs)
        first = self.transport.round(mesh)
        self.assertEqual(first['atlas'], {'received': [original], 'media': blobs})
        self.assertEqual(first['iris'], {'received': [], 'media': {}})
        self.assertEqual(first['echo']['received'], [])
        # Calling transport again alone cannot acknowledge or cache reception.
        self.assertEqual(self.transport.round(mesh)['iris']['received'], [])
        self.transport.update_export('atlas', first['atlas']['received'], first['atlas']['media'])
        second = self.transport.round(mesh)
        self.assertEqual(second['iris']['received'], [original])
        self.assertEqual(second['iris']['media'], blobs)
        self.assertEqual(second['echo']['received'], [])

    def test_isolated_local_updates_reconnect_with_original_capture_and_provenance(self):
        mesh = graph(('hub', 'iris'))
        mesh.edges['0']['up'] = False
        latest = record('lidar-iris', 9, age=8_000_000_000)
        self.transport.update_export('iris', [record('lidar-iris', 8), latest], {})
        self.assertEqual(self.transport.round(mesh)['hub']['received'], [])
        mesh.edges['0']['up'] = True
        self.assertEqual(self.transport.round(mesh)['hub']['received'], [latest])
        # Expired registers are forwarded unchanged; the Go store determines visibility.
        self.assertEqual(latest['timing']['sample_time'], '2026-01-01T00:00:01Z')

    def test_latest_version_deduplicates_neighbors_without_media_from_an_older_record(self):
        mesh = graph(('hub', 'echo'), ('atlas', 'echo'))
        old, new = record(data=b'old'), record(sequence=2, data=b'new')
        self.transport.update_export('hub', [old], media(b'old'))
        self.transport.update_export('atlas', [new], {})
        self.assertEqual(self.transport.round(mesh)['echo'], {'received': [new], 'media': {}})
        self.transport.update_export('atlas', [old], {})
        self.assertEqual(self.transport.round(mesh)['echo']['received'], [new])

    def test_same_version_age_hint_can_only_select_exported_more_conservative_record(self):
        mesh = graph(('hub', 'echo'), ('atlas', 'echo'))
        old, aged = record(data=b'pixels'), record(data=b'pixels', age=1234)
        self.transport.update_export('hub', [old], media(b'pixels'))
        self.transport.update_export('atlas', [aged], {})
        chunk = self.transport.round(mesh)['echo']
        self.assertEqual(chunk, {'received': [aged], 'media': media(b'pixels')})
        conflict = copy.deepcopy(aged)
        conflict['payload']['sample_ns'] += 1
        with self.assertRaises(ValueError):
            self.transport.update_export('atlas', [conflict], {})

    def test_media_is_sender_local_referenced_and_currently_exported(self):
        original = record(data=b'pixels')
        # Hub holds media, but has no route to iris. Atlas knows only its reference.
        self.transport.update_export('hub', [original], media(b'pixels'))
        self.transport.update_export('atlas', [original], media(b'unrelated'))
        mesh = graph(('atlas', 'iris'))
        self.assertEqual(self.transport.round(mesh)['iris']['media'], {})
        self.transport.update_export('atlas', [original], media(b'pixels'))
        self.assertEqual(self.transport.round(mesh)['iris']['media'], media(b'pixels'))
        self.transport.update_export('atlas', [original], {})
        self.assertEqual(self.transport.round(mesh)['iris']['media'], {})

    def test_worker_and_delivery_objects_are_not_shared(self):
        original = record()
        expected = copy.deepcopy(original)
        self.transport.update_export('hub', [original], {})
        original['payload']['sample_ns'] = -1
        mesh = graph(('hub', 'atlas'))
        first = self.transport.round(mesh)
        self.assertEqual(first['atlas']['received'], [expected])
        first['atlas']['received'][0]['payload']['sample_ns'] = -2
        self.assertEqual(self.transport.round(mesh)['atlas']['received'], [expected])

    def test_tombstone_and_internet_edge(self):
        original = record(sequence=3)
        original['tombstone'] = True
        del original['payload']
        self.transport.update_export('hub', [original], {})
        mesh = graph(('hub', 'atlas'), ('hub', 'internet'))
        self.assertEqual(self.transport.round(mesh)['atlas']['received'], [original])
        self.assertEqual(mesh.edges['0']['observation_packets'], 1)
        self.assertEqual(mesh.edges['1']['packets'], 0)
        self.assertNotIn('observation_packets', mesh.edges['1'])

    def test_bounds_and_bad_media_leave_previous_export_intact(self):
        original = record(data=b'pixels')
        self.transport.update_export('hub', [original], media(b'pixels'))
        oversized = record(sequence=2)
        oversized['payload']['extra'] = 'x' * MAX_RECORD_BYTES
        invalid_owner = record(sequence=2)
        invalid_owner['key']['source']['device'] = 'iris'
        for records, blobs in (([original] * (MAX_SOURCES + 1), {}),
                               ([oversized], {}), ([invalid_owner], {}),
                               ([original], {next(iter(media(b'pixels'))): base64.b64encode(b'wrong').decode()})):
            with self.assertRaises(ValueError):
                self.transport.update_export('hub', records, blobs)
        self.assertEqual(self.transport.round(graph(('hub', 'atlas')))['atlas'],
                         {'received': [original], 'media': media(b'pixels')})

    def test_union_media_bound_is_checked_before_counting_delivery(self):
        mesh = graph(('hub', 'echo'), ('atlas', 'echo'))
        self.transport.update_export('hub', [record(data=b'abcdef')], media(b'abcdef'))
        self.transport.update_export('atlas', [record('camera-b', data=b'ghijkl')], media(b'ghijkl'))
        with patch('sim.observation_mesh.MAX_RAW_MEDIA_BYTES', 10):
            with self.assertRaises(ValueError):
                self.transport.round(mesh)
        self.assertEqual([edge['packets'] for edge in mesh.edges.values()], [0, 0])

    def test_actual_mesh_edge_counts_observation_and_evidence_delivery(self):
        from sim.collaborative_runtime import SimulatedMesh
        mesh = SimulatedMesh(['atlas', 'iris', 'echo'])
        self.transport.update_export('hub', [record(data=b'pixels')], media(b'pixels'))
        output = self.transport.round(mesh)
        self.assertEqual(set(output), set(NODES))
        for node in ('atlas', 'iris', 'echo'):
            edge = mesh.edges[mesh.edge_id('hub', node)]
            self.assertEqual(edge['observation_packets'], 1)
            self.assertEqual(edge['evidence_bytes'], 6)
            self.assertEqual(edge['packets'], 1)
        mesh.toggle(mesh.edge_id('hub', 'iris'))
        self.assertEqual(self.transport.round(mesh)['iris']['received'], [])


if __name__ == '__main__':
    unittest.main()
