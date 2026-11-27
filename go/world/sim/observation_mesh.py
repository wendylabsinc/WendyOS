"""One-hop observation delivery between local simulated participant workers.

Only worker exports enter this helper. It has no sensor, filesystem or media
loader. Go replicas own persistence, freshness and fusion; delivery never changes
an observation's source, version, payload, provenance or capture time.
"""
import base64
import copy
import hashlib
import json


MAX_SOURCES = 16
MAX_RECORD_BYTES = 128 << 10
MAX_RAW_MEDIA_BYTES = 8 << 20
MAX_MEDIA_BYTES = ((MAX_RAW_MEDIA_BYTES + 2) // 3) * 4 + 4 * MAX_SOURCES


def _encoded(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def _version(record):
    version = record['version']
    values = (version['epoch'], version['sequence'])
    if any(type(v) is not int or not 0 < v < 1 << 64 for v in values):
        raise ValueError('invalid observation version')
    return values


def _latest(old, new):
    if old is None or _version(new) > _version(old):
        return new
    if _version(new) < _version(old):
        return old
    # Replica export may conservatively raise only the transit age hint.
    left, right = copy.deepcopy(old), copy.deepcopy(new)
    left.setdefault('timing', {})['age_hint_ns'] = 0
    right.setdefault('timing', {})['age_hint_ns'] = 0
    if left != right:
        raise ValueError('conflicting observation version')
    if new.get('timing', {}).get('age_hint_ns', 0) > old.get('timing', {}).get('age_hint_ns', 0):
        return new
    return old


def _reference(record):
    if record.get('tombstone'):
        return None
    payload = record.get('payload')
    return payload.get('evidence') if isinstance(payload, dict) else None


class ObservationMesh:
    def __init__(self, nodes, catalog):
        nodes = list(nodes)
        if (not 1 <= len(nodes) <= 16 or len(set(nodes)) != len(nodes)
                or any(not isinstance(node, str) or not node for node in nodes)
                or not 1 <= len(catalog) <= MAX_SOURCES):
            raise ValueError('invalid observation participants or catalog')
        self.nodes = tuple(sorted(nodes))
        self.catalog = {}
        for sensor in catalog:
            name = sensor['sensor_id']
            if (not isinstance(name, str) or not name or name in self.catalog
                    or sensor['owner'] not in self.nodes
                    or sensor['kind'] not in ('camera', 'gas', 'audio', 'lidar')):
                raise ValueError('invalid observation sensor')
            self.catalog[name] = dict(sensor)
        self._exports = {node: {'records': {}, 'media': {}} for node in self.nodes}

    def update_export(self, node, records, media):
        """Retain owned copies of this worker's latest source registers/media.

        Missing media replaces prior availability. It is never recovered from
        another worker's export or from previously retained bytes here.
        """
        if node not in self._exports or not isinstance(records, list) or not isinstance(media, dict):
            raise ValueError('invalid observation export')
        if len(records) > MAX_SOURCES or len(media) > MAX_SOURCES:
            raise ValueError('observation export exceeds source bound')
        selected = dict(self._exports[node]['records'])
        for original in records:
            if len(_encoded(original)) > MAX_RECORD_BYTES:
                raise ValueError('observation exceeds byte bound')
            record = copy.deepcopy(original)
            key = record['key']
            source = key['source']
            sensor = self.catalog.get(source['service'])
            if (sensor is None or source != {'device': sensor['owner'], 'app': 'world-office-sensors',
                                            'service': sensor['sensor_id'], 'stream': 'samples'}
                    or key['type'] != 'wendy.sensor.v1'):
                raise ValueError('observation is outside the sensor catalog')
            _version(record)
            name = sensor['sensor_id']
            previous = selected.get(name)
            if previous is not None and previous['key'] != key:
                raise ValueError('observation key changed within a source')
            selected[name] = _latest(previous, record)
        # Check only the small exported map, then copy only matching bytes.
        if any(not isinstance(value, str) for value in media.values()) or sum(map(len, media.values())) > MAX_MEDIA_BYTES:
            raise ValueError('observation media exceeds byte bound')
        retained_media = {}
        raw_bytes = 0
        for record in selected.values():
            ref = _reference(record)
            if not ref or ref['sha256'] not in media:
                continue
            digest = ref['sha256']
            encoded = media[digest]
            data = base64.b64decode(encoded, validate=True)
            if len(data) != ref['size'] or hashlib.sha256(data).hexdigest() != digest:
                raise ValueError('exported media does not match its observation')
            if digest not in retained_media:
                raw_bytes += len(data)
            if raw_bytes > MAX_RAW_MEDIA_BYTES:
                raise ValueError('observation media exceeds byte bound')
            retained_media[digest] = encoded
        self._exports[node] = {'records': selected, 'media': retained_media}

    def round(self, mesh):
        """Return one bounded chunk per target using only direct up edges.

        The helper does not apply returned deliveries to any export. A receiver
        can forward them only after its Go worker accepts and exports them in a
        later round, so a chain cannot move data two hops in this call.
        """
        exports = copy.deepcopy(self._exports)
        pending = {node: {} for node in self.nodes}
        counters = []
        for _, edge in sorted(mesh.edges.items()):
            if not edge.get('up') or edge.get('a') not in exports or edge.get('b') not in exports:
                continue
            packets, byte_count = 0, 0
            for sender, target in ((edge['a'], edge['b']), (edge['b'], edge['a'])):
                records, media = exports[sender]['records'], exports[sender]['media']
                if not records:
                    continue
                packets += 1
                sent_media = set()
                for name, record in sorted(records.items()):
                    previous = pending[target].get(name)
                    if previous is not None and previous[0]['key'] != record['key']:
                        raise ValueError('received source keys disagree')
                    selected = _latest(previous[0] if previous else None, record)
                    ref = _reference(record)
                    blob = media.get(ref['sha256']) if ref else None
                    if blob is not None and ref['sha256'] not in sent_media:
                        byte_count += ref['size']
                        sent_media.add(ref['sha256'])
                    if previous is None or selected is record:
                        if previous is not None and _version(previous[0]) == _version(record) and blob is None:
                            blob = previous[1]
                        pending[target][name] = (record, blob)
                    elif _version(previous[0]) == _version(record) and previous[1] is None and blob is not None:
                        pending[target][name] = (previous[0], blob)
            counters.append((edge, packets, byte_count))
        result = {}
        for node, records in pending.items():
            chunk = {'received': [], 'media': {}}
            for name, (record, blob) in sorted(records.items()):
                chunk['received'].append(record)
                ref = _reference(record)
                if blob is not None:
                    chunk['media'][ref['sha256']] = blob
            if sum(map(len, chunk['media'].values())) > MAX_MEDIA_BYTES:
                raise ValueError('received media exceeds byte bound')
            references = {_reference(record)['sha256']: _reference(record)['size']
                          for record in chunk['received'] if _reference(record)}
            if sum(references[digest] for digest in chunk['media']) > MAX_RAW_MEDIA_BYTES:
                raise ValueError('received media exceeds byte bound')
            result[node] = chunk
        for edge, packets, byte_count in counters:
            edge['observation_packets'] = edge.get('observation_packets', 0) + packets
            edge['evidence_bytes'] = edge.get('evidence_bytes', 0) + byte_count
            edge['packets'] = edge.get('packets', 0) + packets
        return result
