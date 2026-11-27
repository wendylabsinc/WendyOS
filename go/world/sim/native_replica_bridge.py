"""Live ordinary-reader output to the existing native macOS twin protocol.

The owned collector supplies current reader receipts and appclock observations.
This module does no inference, signature verification, network or device access.
"""
from __future__ import annotations

import base64
import calendar
from copy import deepcopy
from datetime import datetime
import hashlib
import json
import math
import os
import re
import time
import uuid

from sim.macos_demo_bridge import SessionFiles, STATE_SCHEMA, COMMAND_SCHEMA, json_bytes, _unique_object

MAX_UPDATE_BYTES = 8 << 20
MAX_DISPLAY_AGE_NS = 1_000_000_000
MAX_NATIVE_EVIDENCE = 16


def require(value, message):
    if not value:
        raise ValueError(message)


def integer(value, low=0, high=(1 << 63) - 1):
    require(type(value) is int and low <= value <= high, 'bounded integer required')
    return value


def timestamp(value):
    require(isinstance(value, str) and len(value) <= 40, 'original UTC timestamp required')
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z', value)
    require(match is not None, 'original UTC timestamp required')
    return calendar.timegm(datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S').timetuple()) * 10**9 + int((match[2] or '').ljust(9, '0'))


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def unbase64(value, limit):
    require(isinstance(value, str) and len(value) <= 4 * ((limit + 2) // 3), 'bounded encoded bytes required')
    raw = base64.b64decode(value, validate=True)
    require(0 < len(raw) <= limit, 'bounded decoded bytes required')
    return raw


def decoded(raw, limit=MAX_UPDATE_BYTES):
    require(isinstance(raw, bytes) and 0 < len(raw) <= limit, 'bounded JSON required')
    return json.loads(raw, object_pairs_hook=_unique_object,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite JSON')))


def vector(value, count, bound=10000):
    require(isinstance(value, list) and len(value) == count and
            all(type(n) in (int, float) and math.isfinite(n) and abs(n) <= bound for n in value), 'finite geometry required')
    return value


def scene_point(point):
    x, y, z = vector(point, 3)
    return [x, -z, y]


def native_evidence_descriptor(proof, media, *, label, role, sample_time):
    """Preserve a complete media reference from an already checked input proof.

    Descriptors belong to the containing result's original_envelope_sha256.
    They are retrieval references, not downloaded bytes or retention promises.
    Missing metadata leaves evidence unavailable; ownership is never inferred
    from a sensor label, geometry, or the result publisher.
    """
    try:
        source = proof['key']['source']
        require(isinstance(source, dict) and set(source) == {'device', 'app', 'service', 'stream'} and
                all(isinstance(value, str) and 0 < len(value) <= 512 and value.strip() == value
                    for value in source.values()), 'complete evidence source required')
        require(proof['key']['type'] == 'wendy.sensor.capture.v1', 'capture proof required')
        for value in (proof['envelope_sha256'], media['sha256']):
            require(isinstance(value, str) and re.fullmatch('[0-9a-f]{64}', value), 'evidence digest required')
        integer(media['size'], 1, 8 << 20)
        require(media['content_type'] in ('image/png', 'image/jpeg', 'audio/wav'), 'supported native media required')
        require(role in ('active', 'previous', 'original'), 'known evidence role required')
        timestamp(sample_time)
        return {'label': label, 'role': role,
                'source': {**source, 'component_type': proof['key']['type']},
                **{key: media[key] for key in ('sha256', 'size', 'content_type')},
                'proof_envelope_sha256': proof['envelope_sha256'], 'sample_time': sample_time}
    except (ValueError, KeyError, TypeError, OverflowError):
        return None


def hazard_native_evidence(hazard, references, *, tenant, site):
    """Project media from an already validated typed hazard and its input refs.

    Live and recorded feeds use the same descriptors. This helper does not
    establish result freshness, verify signatures, or retrieve media.
    """
    inputs = hazard.get('inputs', [])
    if not isinstance(inputs, list) or not isinstance(references, list) or len(inputs) != len(references) or len(inputs) > 8:
        return []
    descriptors = []
    for used, ref in zip(inputs, references):
        try:
            sensor, media = used['sensor_id'], used.get('evidence')
            expected_key = {'tenant': tenant, 'site': site, 'entity': 'sensor/'+sensor,
                            'type': 'wendy.sensor.capture.v1', 'source': used['source']}
            if not (ref['key'] == expected_key and used['version'] == ref['version'] and
                    used['capture']['proof_sha256'] == ref['envelope_sha256'] and isinstance(media, dict) and
                    used.get('sample', {}).get('evidence') == media and
                    ((used['kind'] == 'camera' and media.get('content_type') in ('image/png', 'image/jpeg')) or
                     (used['kind'] == 'audio' and media.get('content_type') == 'audio/wav'))):
                continue
            active = native_evidence_descriptor(ref, media, label=sensor, role='active', sample_time=used['sample_time'])
            if active is None:
                continue
            descriptors.append(active)
            original = used['capture'].get('original_evidence')
            if (used['kind'] == 'camera' and media['content_type'] == 'image/jpeg' and
                    isinstance(original, dict) and original.get('content_type') == 'image/png'):
                ancestor = native_evidence_descriptor(ref, original, label=sensor,
                                                      role='original', sample_time=used['sample_time'])
                if ancestor is not None:
                    descriptors.append(ancestor)
        except (KeyError, TypeError):
            continue  # Incomplete ownership or media metadata is unavailable.
    require(len(descriptors) <= MAX_NATIVE_EVIDENCE, 'bounded native evidence required')
    return descriptors


class NativeReplicaBridge:
    """One run, reader incarnation and source configuration; no restart restoration.

    collection_started_monotonic_ns is measured by this host before collection,
    using the same clock as monotonic_ns. Charging the whole collection interval
    conservatively advances physical upper time, never the simulation clock.
    """
    def __init__(self, config, presentation, files: SessionFiles | None = None, *, monotonic_ns=time.monotonic_ns):
        self.config = deepcopy(config)
        c = self.config
        require(c.get('version') == 1 and isinstance(c.get('run_id'), str) and
                re.fullmatch(r'[A-Za-z0-9_-]{1,80}', c['run_id']), 'one live run required')
        for key in ('tenant', 'site', 'reader_boot_id', 'replica_node'):
            require(isinstance(c.get(key), str) and 0 < len(c[key]) <= 256, 'live scope required')
        require(re.fullmatch(r'[A-Za-z0-9_-]{1,64}', c['replica_node']), 'replica label required')
        integer(c['reader_generation'], 1, (1 << 64) - 1)
        integer(c['anchor_unix_ns'], 1)
        require(c['clock_domain'] == 'simulation:' + c['run_id'] and isinstance(c['frame'], dict), 'simulation frame required')
        require(set(c['source']) == {'device', 'app', 'service', 'stream'} and
                all(isinstance(v, str) and 0 < len(v) <= 512 for v in c['source'].values()), 'exact result source required')
        require(isinstance(c['input_sources'], dict) and 4 <= len(c['input_sources']) <= 8 and
                all(re.fullmatch(r'[A-Za-z0-9_-]{1,80}', k) and set(v) == {'device', 'app', 'service', 'stream'}
                    and v['stream'] == k for k, v in c['input_sources'].items()), 'exact input catalogue required')
        self.continuous = c.get('presentation_mode', 'stepped') == 'continuous'
        require(c.get('presentation_mode', 'stepped') in ('stepped', 'continuous'), 'known presentation mode required')
        self.source_catalog = deepcopy(c['input_sources'])
        self.people = None
        if 'people' in c:
            from sim.native_people_layer import NativePeopleLayer
            self.people = NativePeopleLayer(c, c['people'], monotonic_ns)
            for name, source in c['people']['input_sources'].items():
                require(name not in self.source_catalog or self.source_catalog[name] == source,
                        'shared camera catalogue differs')
                self.source_catalog[name] = deepcopy(source)
            require(len(self.source_catalog) <= 16, 'bounded shared source catalogue required')
        self.files, self.monotonic_ns = files, monotonic_ns
        require(c.get('display_clock') in (None, 'mach_absolute_time_ns'), 'unsupported local display clock')
        self.sequence = self.command_sequence = self.last_collection = 0
        self.last_tick = self.last_attempt = 0
        self.last_status = None
        self.sample_ns = 0
        self.minimum_presentation_ns = 0
        self.pending_source = None
        self.latest = None
        self.unknown_reason = 'Waiting for the live native reader.'
        self.last_clock_sample = self.last_clock_lower = 0
        self.last_host_now = integer(self.monotonic_ns())
        self.geometry = deepcopy(presentation['geometry'])
        self.presentation = self._presentation(presentation)
        if self.continuous:
            self.sample_ns = self.presentation['simulation_time_ns']
        if files is not None:
            files.write('geometry.json', self.geometry)
        self.export()
        if files is not None:
            files.write('bridge-ready.json', {'schema': 'wendy.macos-demo.ready.v1', 'run_id': c['run_id'],
                'mode': 'live-native-replica', 'geometry_sha256': digest(json_bytes(self.geometry)),
                'reader_generation': c['reader_generation'], 'replica_node': c['replica_node'],
                'independent_crypto_verification': False, 'local_inference': False})

    def _presentation(self, value):
        c = self.config
        require(isinstance(value, dict) and value['geometry'] == self.geometry and
                self.geometry.get('schema') == 'wendy.macos-demo.geometry.v1' and
                self.geometry.get('run_id') == c['run_id'] and self.geometry.get('model_sha256') == c['model_sha256'] and
                c['frame'].get('revision') in ({'office-' + c['model_sha256'][:16],
                    'shared-office-' + c['model_sha256'][:16]} if self.people is not None else
                    {'office-' + c['model_sha256'][:16]}), 'presentation model/run/frame differs')
        ns = integer(value['simulation_time_ns'], 0, 120_000_000_000)
        require(isinstance(value['dynamic_geoms'], list) and len(value['dynamic_geoms']) <= 12000, 'bounded dynamic geometry required')
        rows = value['sensor_sources']
        require(isinstance(rows, list) and len(rows) == len(self.source_catalog) and
                {row['id'] for row in rows} == set(self.source_catalog) and
                all(type(row['enabled']) is bool for row in rows), 'exact presented source switches required')
        json_bytes(value)
        return deepcopy(value)

    def ingest_people(self, update):
        """Consume people separately; only its observed source presentation is shared."""
        require(self.people is not None, 'people reader is not configured')
        try:
            update = decoded(update) if isinstance(update, bytes) else decoded(json_bytes(update, MAX_UPDATE_BYTES))
            presentation = self._presentation(update['presentation'])
            if presentation['simulation_time_ns'] < self.minimum_presentation_ns:
                return False
            if (self.continuous and update['reader']['status'] == 'ready' and
                    timestamp(update['reader']['sample_time']) < self.config['anchor_unix_ns']+self.minimum_presentation_ns):
                return False
            consumed = self.people.ingest(update)
            if consumed and self.people.status == 'ready':
                self._accept_presentation(presentation)
            return consumed
        except (ValueError, KeyError, TypeError, OverflowError) as exc:
            self.people.latest = None
            raise ValueError('invalid live native people update') from exc

    def _accept_presentation(self, presentation):
        # A later original source instant may expire either layer, never renew
        # it. Arrival order of independently queried readers cannot rewind the
        # reference scene or change either reader's own observation clock.
        if presentation['simulation_time_ns'] >= self.presentation['simulation_time_ns']:
            self.presentation = presentation
            self.sample_ns = presentation['simulation_time_ns']

    def present(self, presentation):
        """Display one completed continuous-source capture without renewing a reader.

        Geometry stays at its actual capture instant. No interpolation, reader
        clock substitution or motion command is performed here.
        """
        require(self.continuous, 'independent presentation requires continuous mode')
        presentation = self._presentation(presentation)
        if presentation['simulation_time_ns'] < max(self.minimum_presentation_ns, self.sample_ns):
            return False
        self._accept_presentation(presentation)
        return True

    def ingest(self, update):
        """Return False for an already consumed collection/reader event, without refreshing it."""
        try:
            if isinstance(update, bytes):
                update = decoded(update)
            else:
                update = decoded(json_bytes(update, MAX_UPDATE_BYTES))
            return self._ingest(update)
        except (ValueError, KeyError, TypeError, OverflowError) as exc:
            self.latest = None
            self.unknown_reason = 'Live reader update unavailable or mismatched.'
            raise ValueError('invalid live native replica update') from exc

    def _ingest(self, update):
        c = self.config
        require(set(update) <= {'version', 'run_id', 'sequence', 'collection_started_monotonic_ns',
                               'reader', 'clock', 'presentation', 'media'} and
                update['version'] == 1 and update['run_id'] == c['run_id'], 'collector scope differs')
        collection = integer(update['sequence'], 1)
        if collection <= self.last_collection:
            return False
        now = integer(self.monotonic_ns())
        require(now >= self.last_host_now, 'collector monotonic clock regressed')
        self.last_host_now = now
        started = integer(update['collection_started_monotonic_ns'])
        require(0 <= now - started < MAX_DISPLAY_AGE_NS, 'collector observation is stale')
        reader = update['reader']
        clock = update.get('clock', reader.get('clock'))
        require(reader.get('clock') is None or clock == reader['clock'], 'companion clock differs from reader observation')
        require(reader['version'] == 1 and reader['run_id'] == c['run_id'] and
                reader['reader_generation'] == c['reader_generation'] and
                reader['raw_inputs_independently_verified'] is False and
                reader['status'] in ('ready', 'waiting_for_source'), 'ordinary reader scope/status differs')
        tick = integer(reader['tick'], 1)
        attempt = integer(reader.get('startup_attempt', 0))
        status = reader['status']
        if tick < self.last_tick:
            return False
        if tick == self.last_tick:
            transition = self.last_status == 'waiting_for_source' and (
                status == 'ready' or status == 'waiting_for_source' and attempt > self.last_attempt)
            if not transition:
                return False
        require(status != 'waiting_for_source' or self.last_status != 'ready', 'startup waiting cannot replace ready reader')
        if status == 'waiting_for_source' and clock is None:
            require(reader['hazards'] == [] and not update.get('media'), 'waiting carries no result/evidence')
            presentation = self._presentation(update['presentation'])
            if presentation['simulation_time_ns'] < self.minimum_presentation_ns:
                return False
            self.last_collection, self.last_tick, self.last_attempt, self.last_status = collection, tick, attempt, status
            self._accept_presentation(presentation)
            self.latest = {'started': started, 'status': status, 'clock_current': False}
            return True
        require(clock['server_boot_id'] == c['reader_boot_id'] and
                clock['deployment_generation'] == c['reader_generation'] and
                clock['sample_clock_domain'] == c['clock_domain'], 'companion clock identity differs')
        sample, lower, upper = timestamp(clock['sample_time']), timestamp(clock['physical_earliest']), timestamp(clock['physical_latest'])
        require(0 < lower <= upper and upper - lower <= 5_000_000_000 and sample >= self.last_clock_sample and
                lower >= self.last_clock_lower and c['anchor_unix_ns'] <= timestamp(reader['sample_time']) <= sample,
                'fresh companion clock required')
        if (self.continuous and status == 'ready' and
                timestamp(reader['sample_time']) < c['anchor_unix_ns']+self.minimum_presentation_ns):
            return False
        presentation = self._presentation(update['presentation'])
        if presentation['simulation_time_ns'] < self.minimum_presentation_ns:
            return False
        require(self.continuous or presentation['simulation_time_ns'] == sample - c['anchor_unix_ns'],
                'presentation/sample clock differs')
        hazards = reader['hazards']
        require(isinstance(hazards, list) and len(hazards) <= 1, 'one configured hazard stream required')
        require(status != 'waiting_for_source' or not hazards, 'waiting is not a successful hazard read')
        if status == 'ready':
            require(reader['origin_attested_inference'] is True, 'verified ordinary read required')
        parsed = self._hazard(hazards[0], timestamp(reader['sample_time'])) if hazards else None
        media = self._media(update.get('media', []), parsed)
        self.last_collection, self.last_tick, self.last_attempt, self.last_status = collection, tick, attempt, status
        self.last_clock_sample, self.last_clock_lower = sample, lower
        self._accept_presentation(presentation)
        self.latest = {'started': started, 'lower': lower, 'upper': upper, 'sample': sample, 'status': status,
                       'parsed': parsed, 'media': media, 'clock_current': True, 'reader_sample': reader['sample_time'], 'tick': tick}
        return True

    def _hazard(self, row, read_sample):
        c = self.config
        raw = unbase64(row['envelope_json'], 384 << 10)
        require(digest(raw) == row['envelope_sha256'], 'result original digest differs')
        record_raw = unbase64(decoded(raw)['record'], 64 << 10)
        require(digest(record_raw) == row['record_sha256'], 'result record digest differs')
        record = decoded(record_raw)
        require(row['version'] == {key: record[key] for key in ('epoch', 'sequence')}, 'result version differs')
        scope, source = record['scope'], c['source']
        require(all(scope.get(key) == value for key, value in {'tenant': c['tenant'], 'site': c['site'],
                'device': source['device'], 'app': source['app'], 'service': source['service'],
                'execution_domain': 'simulation'}.items()), 'result origin differs')
        pub = record['publication']
        require(pub['component_type'] == 'wendy.inference.result.v1' and pub['stream'] == source['stream'] and
                pub['frame'] == c['frame'] and pub['entity'] == c['run_id'] + '/possible-gas-hazard' and
                pub['tombstone'] is False, 'result publication differs')
        generic = decoded(unbase64(pub['payload'], 32 << 10))
        require(generic['version'] == 1 and generic['result_schema'] == 'wendy.hazard.v1', 'typed hazard required')
        hazard = decoded(unbase64(generic['result_json'], 32 << 10))
        require(hazard == row['payload'] and hazard['label'] == 'possible-gas-hazard', 'typed original differs')
        timing = pub['timing']; sample = timestamp(timing['sample_time'])
        margin = max(integer(timing['error_bound_ns']), integer(timing['age_hint_ns']))
        outer_until = sample + integer(pub['ttl_ns'], 1) - margin
        require(timing['clock_domain'] == c['clock_domain'] and timing['error_known'] is True and
                sample <= read_sample < outer_until, 'reader result interval differs')
        sample_until = min(outer_until, timestamp(hazard['valid_until']))
        physical_until = min(timestamp(hazard['physical_valid_until']), integer(record['expires_at_unix_ms'], 1) * 1_000_000)
        physical_from = integer(record['issued_at_unix_ms'], 1) * 1_000_000
        require(sample_until > read_sample and physical_until > physical_from, 'original result has expired')
        vector(hazard['position_m'], 3)
        require(0 <= vector([hazard['uncertainty_m']], 1, 100)[0] and
                0 <= vector([hazard['confidence']], 1, 1)[0], 'bounded original hazard required')
        inputs = hazard['inputs']
        require(isinstance(inputs, list) and len(inputs) == len(c['input_sources']) and
                {i['sensor_id'] for i in inputs} == set(c['input_sources']) and len(generic['inputs']) == len(inputs),
                'complete original inputs required')
        for used, ref in zip(inputs, generic['inputs']):
            sensor = used['sensor_id']
            require(used['source'] == c['input_sources'][sensor] == ref['key']['source'] and
                    used['version'] == ref['version'] and used['capture']['proof_sha256'] == ref['envelope_sha256'] and
                    timestamp(used['sample_time']) <= read_sample < timestamp(used['valid_until']), 'input lineage differs')
            if used['kind'] != 'lidar':
                original = used['sample']
                require(original['run_id'] == c['run_id'] and original['frame'] == c['frame'] and
                        original['sensor_id'] == sensor and original['id'] == used['sample_id'], 'original sample differs')
                vector(original['position_m'], 3)
        cameras = hazard['cameras']
        camera_ids = {i['sensor_id'] for i in inputs if i['kind'] == 'camera'}
        require(isinstance(cameras, list) and len(cameras) == len(camera_ids) and
                {row['sensor_id'] for row in cameras} == camera_ids and
                all(0 <= vector([row['red_fraction']], 1, 1)[0] for row in cameras), 'camera features differ')
        require(0 <= vector([hazard['audio']['tone_fraction']], 1, 1)[0] and
                0 <= vector([hazard['gas_value_ppm']], 1, 1_000_000)[0], 'finite observed features required')
        native_evidence = hazard_native_evidence(hazard, generic['inputs'], tenant=c['tenant'], site=c['site'])
        return {'hazard': hazard, 'native_evidence': native_evidence,
                'sample_until': sample_until, 'physical_until': physical_until,
                'physical_from': physical_from, 'sha256': row['envelope_sha256']}

    def attach_media(self, rows, *, envelope_sha256):
        """Attach newly cached evidence to this exact, still-current result.

        The caller supplies completed cache rows only. This does no collection,
        reingestion or deadline renewal; export remains a separate operation.
        Return False when nothing changes or the original result is no longer
        current. Invalid evidence raises ValueError without replacing the reader.
        """
        current = self.latest
        if (current is None or current.get('parsed') is None or
                current['parsed']['sha256'] != envelope_sha256 or self._view()['state'] != 'hazard'):
            return False
        try:
            rows = decoded(json_bytes(rows, MAX_UPDATE_BYTES))
            media = self._media(rows, current['parsed'], existing=current['media'])
        except (ValueError, KeyError, TypeError, OverflowError) as exc:
            raise ValueError('invalid late source evidence') from exc
        # Local file writes may consume the remaining display interval. Never
        # turn their completion into a new observation or a fresh lifetime.
        if self.latest is not current or self._view()['state'] != 'hazard':
            return False
        merged = {**current['media'], **media}
        if merged == current['media']:
            return False
        current['media'] = merged
        return True

    def _media(self, rows, parsed, *, existing=None):
        require(isinstance(rows, list) and len(rows) <= 3, 'bounded optional source evidence required')
        require(parsed is not None or not rows, 'no evidence without a received result')
        expected = {i['sensor_id']: i for i in parsed['hazard']['inputs']} if parsed else {}
        media, images = {}, []
        for row in rows:
            sensor = row['source']['service']
            require(sensor in expected and sensor not in media, 'one exact evidence owner required')
            used = expected[sensor]
            require(used['kind'] in ('camera', 'audio') and row['source'] == {
                    **used['source'], 'component_type': 'wendy.sensor.capture.v1'} and
                    row['reference'] == used['evidence'], 'evidence source/reference differs')
            ref = row['reference']; raw = unbase64(row['data'], 4 << 20)
            require(len(raw) == ref['size'] and digest(raw) == ref['sha256'] and
                    ref['content_type'] in ('image/png', 'image/jpeg', 'audio/wav'), 'original evidence bytes differ')
            media[sensor] = {'reference': deepcopy(ref), 'source': deepcopy(row['source'])}
            if existing is not None and sensor in existing:
                require(all(existing[sensor][key] == media[sensor][key] for key in ('reference', 'source')),
                        'attached evidence differs from current result')
                media[sensor] = deepcopy(existing[sensor])
                continue
            if self.files is not None and used['kind'] == 'camera':
                suffix = '.png' if ref['content_type'] == 'image/png' else '.jpg'
                images.append((sensor, raw, 'native-' + sensor + suffix))
        # Validate the complete bounded set before changing thumbnail files.
        for sensor, raw, name in images:
            temporary = '.native-' + uuid.uuid4().hex
            fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=self.files.fd)
            try:
                with os.fdopen(fd, 'wb') as stream:
                    stream.write(raw)
                os.replace(temporary, name, src_dir_fd=self.files.fd, dst_dir_fd=self.files.fd)
            finally:
                try:
                    os.unlink(temporary, dir_fd=self.files.fd)
                except FileNotFoundError:
                    pass
            media[sensor]['path'] = name
        return media

    def _view(self):
        current = self.latest
        base = {'state': 'unknown', 'label': 'Live observations unavailable', 'reason': self.unknown_reason,
                'basis': 'LIVE SIMULATION · Original evidence from the named gateways.',
                'sample_time_s': self.sample_ns / 1e9, 'observations': []}
        if current is None:
            return base
        now = self.monotonic_ns()
        if now < self.last_host_now:
            self.latest = None
            self.unknown_reason = base['reason'] = 'The local display clock regressed. Waiting for a new session.'
            return base
        self.last_host_now = now
        age = now - current['started']
        if not 0 <= age < MAX_DISPLAY_AGE_NS:
            base['reason'] = 'The live reader or companion clock is stale. Waiting for a new observation.'
            return base
        if current['status'] == 'waiting_for_source':
            return {**base, 'state': 'waiting', 'label': 'Waiting for native sources',
                    'reason': 'The ordinary reader has not completed a successful current-source query.'}
        parsed = current['parsed']
        if parsed is None:
            return {**base, 'state': 'no_live_result', 'label': 'No live hazard result',
                    'reason': 'The latest successful native reader observation is empty. This is not clearance.'}
        # Continuous display spends the remaining original sample budget as
        # elapsed time; this is an early display cutoff, not a new sample clock.
        elapsed_sample = age if self.continuous else 0
        if (max(current['sample'] + elapsed_sample, self.config['anchor_unix_ns'] + self.sample_ns) >= parsed['sample_until'] or
                current['upper'] + age >= parsed['physical_until']):
            return {**base, 'state': 'expired', 'label': 'Hazard result expired',
                    'reason': 'The original sample or physical validity ended. Waiting for a fresh native result.'}
        if current['lower'] < parsed['physical_from']:
            return {**base, 'reason': 'The companion clock does not cover the original result window.'}
        hazard, observations = parsed['hazard'], []
        target = scene_point(hazard['position_m'])
        cameras = {row['sensor_id']: row for row in hazard['cameras']}
        labels = self.config.get('device_labels', {})
        for used in hazard['inputs']:
            kind, sensor = used['kind'], used['sensor_id']
            if kind == 'lidar':
                continue  # A compact summary supplies no source pose.
            row = {'id': sensor, 'kind': kind, 'label': sensor + ' · ' + labels.get(used['source']['device'], 'origin source'),
                   'position': scene_point(used['sample']['position_m']), 'active': True,
                   'source_device': used['source']['device'], 'original_sample_time': used['sample_time']}
            if kind == 'camera':
                row.update(target=target, value=100 * cameras[sensor]['red_fraction'], unit='% red')
            elif kind == 'audio':
                row.update(value=100 * hazard['audio']['tone_fraction'], unit='% tone')
            elif kind == 'gas':
                row.update(value=hazard['gas_value_ppm'], unit='ppm')
            if used.get('evidence'):
                row['evidence_reference'] = deepcopy(used['evidence'])
            if current['media'].get(sensor, {}).get('path'):
                row['evidence_path'] = current['media'][sensor]['path']
            observations.append(row)
        return {**base, 'state': 'hazard', 'label': 'Possible gas hazard',
                'reason': 'Camera, LiDAR, gas and audio support this location.',
                'basis': 'LIVE SIMULATION · Original evidence from the named gateways.',
                'position': target, 'uncertainty_m': hazard['uncertainty_m'], 'confidence': hazard['confidence'],
                'observations': observations, 'original_envelope_sha256': parsed['sha256'],
                'native_evidence': deepcopy(parsed['native_evidence'])}

    def export(self, *, closed=False):
        self.sequence += 1
        c = self.config
        view = self._view()
        observation_current = (not closed and self.latest is not None and self.latest['status'] == 'ready' and
                               self.latest.get('clock_current') is True and
                               0 <= self.monotonic_ns() - self.latest['started'] < MAX_DISPLAY_AGE_NS)
        controls_available = observation_current and self.pending_source is None
        sensor_sources = [{key: row[key] for key in ('id', 'enabled', 'kind', 'label') if key in row}
                          for row in self.presentation['sensor_sources']]
        snapshot = {'run_id': c['run_id'], 'time_s': self.sample_ns / 1e9, 'phase': 'fusion',
                    'controls_available': controls_available,
                    'sensor_sources': sensor_sources,
                    'authorized': False, 'stopped': False, 'robots': [], 'jobs': [], 'completed': 0, 'total': 0,
                    'events': [], 'voxels': [], 'voxel_size': .2, 'map_node': c['replica_node'],
                    'fusion': view, 'network': {'edges': [], 'internet': {'id': 'internet',
                    'a': c['replica_node'], 'b': 'internet', 'enabled': False, 'up': False, 'packets': 0},
                    'gossip_rounds': 0, 'peer_voxels': {}}}
        if self.people is not None:
            snapshot['tracking'] = self.people.snapshot(self.sample_ns, sensor_sources, closed=closed)
        value = {'schema': STATE_SCHEMA, 'run_id': c['run_id'], 'sequence': self.sequence,
                 'worker_status': 'closed' if closed else 'running', 'last_command_sequence': self.command_sequence,
                 'mode': 'live-native-replica', 'replay': False, 'snapshot': snapshot,
                 'dynamic_geoms': deepcopy(self.presentation['dynamic_geoms'])}
        if c.get('display_clock') == 'mach_absolute_time_ns':
            observation_deadline = self.latest['started'] + MAX_DISPLAY_AGE_NS if observation_current else 0
            display_deadline = 0
            if not closed and view['state'] == 'hazard':
                display_deadline = min(observation_deadline, self.latest['started'] +
                    self.latest['parsed']['physical_until'] - self.latest['upper'])
                if self.continuous:
                    display_deadline = min(display_deadline, self.latest['started'] +
                        self.latest['parsed']['sample_until'] - self.latest['sample'])
            value.update(display_clock=c['display_clock'],
                         observation_deadline_monotonic_ns=observation_deadline,
                         display_deadline_monotonic_ns=max(0, display_deadline))
        json_bytes(value)
        if self.files is not None:
            self.files.write('state.json', value, replace=self.sequence > 1)
        return value

    def begin_source_step(self, command, next_sample_ns):
        """Consume one control and return its bounded existing Source.step request.

        The main thread calls begin/finish; an external worker may perform the
        I/O. Until finish there is no receipt or optimistic switch change. The
        last observed hazard keeps its original deadline while rendering waits.
        """
        require(self.pending_source is None, 'one source step is already pending')
        sequence = self.command_sequence + 1
        require(command == {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': sequence,
                            'action': command.get('action'), 'target': command.get('target')}, 'exact next command required')
        action, target = command['action'], command['target']
        require(action in ('toggle_sensor', 'toggle_camera') and target in self.source_catalog and
                (action != 'toggle_camera' or target.startswith('camera-')), 'known source toggle required')
        require(self.latest is not None and self.latest['status'] == 'ready' and
                self.latest.get('clock_current') is True and 0 <= self.monotonic_ns() - self.latest['started'] < MAX_DISPLAY_AGE_NS,
                'current source presentation required')
        integer(next_sample_ns, self.presentation['simulation_time_ns'] + 1, 120_000_000_000)
        enabled = {row['id'] for row in self.presentation['sensor_sources'] if row['enabled']}
        enabled.symmetric_difference_update({target})
        request = {'simulation_time_ns': next_sample_ns, 'sensors': [key for key in self.source_catalog if key in enabled]}
        self.command_sequence = sequence  # Consume before a possibly completed source mutation.
        self.pending_source = {'sequence': sequence, 'action': action, 'request': deepcopy(request)}
        return deepcopy(request)

    def finish_source_step(self, response=None, *, error=None):
        """Finish once; exact source acknowledgment or an unknown outcome only."""
        require(self.pending_source is not None, 'no source step is pending')
        pending, self.pending_source = self.pending_source, None
        sequence, action, request = pending['sequence'], pending['action'], pending['request']
        receipt = {'schema': 'wendy.macos-demo.command-result.v1', 'run_id': self.config['run_id'],
                   'sequence': sequence, 'action': action, 'accepted': False, 'error': 'unknown_outcome',
                   'source_request': request, 'no_mutation_retry': True}
        try:
            require(error is None, 'source outcome unknown')
            require(isinstance(response, dict) and set(response) == {'sequence', *request} and
                    type(response['sequence']) is int and response['sequence'] > 0 and
                    response['sensors'] == request['sensors'] and
                    (integer(response['simulation_time_ns'], request['simulation_time_ns'], 120_000_000_000)
                     if self.continuous else response['simulation_time_ns'] == request['simulation_time_ns']),
                    'source step response differs')
            receipt.update(accepted=True, error=None, source_response=deepcopy(response))
            self.minimum_presentation_ns = response['simulation_time_ns']
        except Exception:
            pass
        self.latest = None
        self.unknown_reason = ('Source change acknowledged. Waiting for new native observations.' if receipt['accepted']
                               else 'Source change outcome unknown. No automatic retry.')
        if self.files is not None:
            self.files.write(f'{sequence:06d}.json', receipt, receipt=True)
        return receipt

    def source_step(self, command, next_sample_ns, step):
        """Compatible synchronous wrapper; the source mutation is called once."""
        request = self.begin_source_step(command, next_sample_ns)
        try:
            response = step(request)
        except Exception as exc:
            return self.finish_source_step(error=exc)
        return self.finish_source_step(response)
