"""Map an ordinary people reader's exact typed results into the live twin.

The ordinary reader supplies signature/current-source verification. This layer
checks byte and lineage correspondence, preserves original bounds, and maps
geometry. It performs no inference, media fetch, crypto or new clock sampling.
"""
from copy import deepcopy
import re

from sim.native_replica_bridge import (MAX_DISPLAY_AGE_NS, MAX_NATIVE_EVIDENCE, decoded, digest,
                                       integer, native_evidence_descriptor, require, timestamp, unbase64, vector)

SCHEMA = 'wendy.person.track.v1'
ALGORITHM = 'camera-ankle-two-observation-tracking'
LIFETIME_NS = 1_250_000_000
CAPTURE_AGE_NS = 5_000_000_000


def sha(value):
    require(isinstance(value, str) and re.fullmatch('[0-9a-f]{64}', value), 'original digest required')
    return value


def text(value, limit):
    require(isinstance(value, str) and 0 < len(value) <= limit and value.strip() == value,
            'bounded original text required')
    return value


def covariance(value):
    a, b, c, d = vector(value, 4, 100)
    require(a > 0 and d > 0 and abs(b-c) <= 1e-10 and a*d > b*c, 'positive covariance required')


class NativePeopleLayer:
    def __init__(self, shared, config, monotonic_ns):
        require(isinstance(config, dict) and set(config) ==
                {'source', 'reader_generation', 'reader_boot_id', 'input_sources'}, 'exact people reader configuration required')
        self.shared, self.config, self.monotonic_ns = deepcopy(shared), deepcopy(config), monotonic_ns
        require(shared['anchor_unix_ns'] == 1_767_225_600_000_000_000, 'native people display uses the original January 2026 anchor')
        integer(config['reader_generation'], 1, (1 << 64)-1)
        text(config['reader_boot_id'], 256)
        require(isinstance(config['input_sources'], dict) and 1 <= len(config['input_sources']) <= 8,
                'bounded people camera catalogue required')
        for name, source in [(None, config['source']), *config['input_sources'].items()]:
            require(isinstance(source, dict) and set(source) == {'device', 'app', 'service', 'stream'},
                    'exact people source required')
            for value in source.values():
                text(value, 512)
            if name is not None:
                require(re.fullmatch(r'[A-Za-z0-9_-]{1,80}', name) and source['stream'] == name,
                        'exact camera stream required')
        self.collection = self.tick = self.attempt = self.sample = self.lower = 0
        self.status = self.latest = None
        self.last_now = integer(monotonic_ns())

    def ingest(self, update):
        c, shared = self.config, self.shared
        require(set(update) <= {'version', 'run_id', 'sequence', 'collection_started_monotonic_ns',
                               'reader', 'clock', 'presentation'} and
                update['version'] == 1 and update['run_id'] == shared['run_id'], 'people collection scope differs')
        collection = integer(update['sequence'], 1)
        if collection <= self.collection:
            return False
        now, started = integer(self.monotonic_ns()), integer(update['collection_started_monotonic_ns'])
        require(now >= self.last_now and 0 <= now-started < MAX_DISPLAY_AGE_NS, 'people observation is stale')
        self.last_now = now
        reader = update['reader']
        status = reader['status']
        require(reader['version'] == 1 and reader['run_id'] == shared['run_id'] and
                reader['reader_generation'] == c['reader_generation'] and
                reader['camera_inputs_independently_verified'] is False and
                status in ('ready', 'waiting_for_source', 'failed'), 'people reader scope/status differs')
        tick, attempt = integer(reader['tick'], 1), integer(reader.get('startup_attempt', 0), 0, 32)
        if tick < self.tick or tick == self.tick and not (self.status == 'waiting_for_source' and
                (status in ('ready', 'failed') or status == 'waiting_for_source' and attempt > self.attempt)):
            return False
        require(status != 'waiting_for_source' or self.status != 'ready', 'startup waiting cannot replace ready people')
        rows = reader['tracks']
        require(isinstance(rows, list) and len(rows) <= 8 and (status == 'ready' or not rows),
                'bounded successful people observation required')
        clock = update.get('clock', reader.get('clock'))
        require(reader.get('clock') is None or clock == reader['clock'], 'people companion clock differs')
        if status != 'ready' and clock is None:
            self.collection, self.tick, self.attempt, self.status = collection, tick, attempt, status
            self.latest = None
            return True
        require(clock['server_boot_id'] == c['reader_boot_id'] and
                clock['deployment_generation'] == c['reader_generation'] and
                clock['sample_clock_domain'] == shared['clock_domain'], 'people clock identity differs')
        sample = timestamp(clock['sample_time'])
        lower, upper = timestamp(clock['physical_earliest']), timestamp(clock['physical_latest'])
        read_sample = timestamp(reader['sample_time'])
        require(sample >= self.sample and 0 < lower <= upper and upper-lower <= 5_000_000_000 and
                lower >= self.lower and shared['anchor_unix_ns'] <= read_sample <= sample and
                (shared.get('presentation_mode', 'stepped') == 'continuous' or
                 update['presentation']['simulation_time_ns'] == sample-shared['anchor_unix_ns']),
                'original people observation clock differs')
        require(status != 'ready' or reader['origin_attested_inference'] is True, 'verified ordinary people read required')
        parsed = [self._track(row, read_sample) for row in rows]
        require(len({row['track']['id'] for row in parsed}) == len(parsed) and
                len({row['sha256'] for row in parsed}) == len(parsed), 'duplicate current track')
        self.collection, self.tick, self.attempt, self.status = collection, tick, attempt, status
        self.sample, self.lower = sample, lower
        self.latest = {'started': started, 'sample': sample, 'lower': lower, 'upper': upper,
                       'status': status, 'parsed': parsed}
        return True

    def _track(self, row, read_sample):
        c, shared = self.config, self.shared
        raw = unbase64(row['envelope_json'], 384 << 10)
        require(digest(raw) == sha(row['envelope_sha256']), 'people envelope differs')
        original = unbase64(decoded(raw)['record'], 64 << 10)
        require(digest(original) == sha(row['record_sha256']), 'people record differs')
        record = decoded(original)
        version = {key: integer(record[key], 1, (1 << 64)-1) for key in ('epoch', 'sequence')}
        require(row['version'] == version, 'people version differs')
        source = c['source']
        require(all(record['scope'].get(key) == value for key, value in
                {'tenant': shared['tenant'], 'site': shared['site'], 'execution_domain': 'simulation',
                 **{key: source[key] for key in ('device', 'app', 'service')}}.items()), 'people origin differs')
        pub = record['publication']
        require(pub['component_type'] == 'wendy.inference.result.v1' and pub['stream'] == source['stream'] and
                pub['frame'] == shared['frame'] and pub['tombstone'] is False, 'people publication differs')
        generic = decoded(unbase64(pub['payload'], 32 << 10))
        require(generic['version'] == 1 and generic['result_schema'] == SCHEMA, 'typed person result required')
        payload = decoded(unbase64(generic['result_json'], 32 << 10))
        require(payload == row['payload'] and payload['schema'] == SCHEMA and payload['algorithm'] == ALGORITHM and
                payload['algorithm_version'] == '1' and payload['run_id'] == shared['run_id'] and
                payload['clock_domain'] == shared['clock_domain'] and payload['frame'] == shared['frame'],
                'typed original person result differs')
        identity = text(payload['track_id'], 160)
        integer(payload['revision'], 1, (1 << 64)-1)
        require(pub['entity'] == shared['run_id']+'/track/'+identity, 'people entity differs')
        observed, until = timestamp(payload['observed_at']), timestamp(payload['valid_until'])
        require(observed <= read_sample < until and until-observed <= LIFETIME_NS, 'original track sample expired')
        vector(payload['position_xz'], 2)
        covariance(payload['covariance'])
        require(type(payload['ambiguous']) is bool and type(payload['motion_known']) is bool, 'original track flags required')
        motion = payload['motion_known']
        previous = payload.get('previous', [])
        require(isinstance(previous, list) and len(previous) <= 8, 'bounded previous camera inputs required')
        if motion:
            vector(payload['velocity_xz'], 2, 50)
            covariance(payload['velocity_covariance'])
            require(previous and timestamp(payload['previous_observed_at']) < observed, 'two-observation motion required')
        else:
            require(not previous and not any(k in payload for k in ('velocity_xz', 'velocity_covariance', 'previous_observed_at')),
                    'unknown motion cannot carry velocity')
        current = payload['current']
        require(isinstance(current, list) and 1 <= len(current) <= 8, 'current camera inputs required')
        physical = integer(record['expires_at_unix_ms'], 1)*1_000_000
        physical_from = integer(record['issued_at_unix_ms'], 1)*1_000_000
        refs, seen, samples, deadlines, models, native_members = [], {}, [], [], set(), []
        native_evidence = []
        for index, members in enumerate((current, previous)):
            sensors, group_samples = set(), []
            for member in members:
                sensor, ref = member['sensor_id'], member['proof']
                require(sensor in c['input_sources'] and sensor not in sensors, 'original camera source differs')
                sensors.add(sensor)
                require(ref['key'] == {'tenant': shared['tenant'], 'site': shared['site'],
                        'entity': 'sensor/'+sensor, 'type': 'wendy.sensor.capture.v1', 'source': c['input_sources'][sensor]},
                        'original camera proof scope differs')
                for key in ('epoch', 'sequence'):
                    integer(ref['version'][key], 1, (1 << 64)-1)
                sha(ref['envelope_sha256']); sha(ref['record_sha256'])
                proof_hash = ref['envelope_sha256']
                require(proof_hash not in seen or seen[proof_hash] == ref, 'repeated proof changed identity')
                if proof_hash not in seen:
                    seen[proof_hash] = ref
                    refs.append(ref)
                sample, deadline = timestamp(member['sample_time']), timestamp(member['valid_until'])
                require(sample < deadline <= sample+LIFETIME_NS and sample <= read_sample < deadline,
                        'original camera sample expired')
                samples.append(sample); group_samples.append(sample); deadlines.append(deadline)
                captured = integer(member['captured_unix_ns'], 1, (1 << 63)-1-CAPTURE_AGE_NS)
                member_until = timestamp(member['physical_until'])
                require(captured < member_until <= captured+CAPTURE_AGE_NS and
                        member['simulation_anchor_unix_ns'] == shared['anchor_unix_ns'], 'original camera physical bound differs')
                physical = min(physical, member_until, captured+CAPTURE_AGE_NS)
                integer(member['capture_sequence'], 1, (1 << 64)-1)
                integer(member['member_index'], 0, 63)
                text(member['scene_boot_id'], 128); text(member['sample_id'], 512)
                text(member['calibration_revision'], 256); sha(member['calibration_sha256'])
                models.add((text(member['model_id'], 256), text(member['model_version'], 256), sha(member['model_sha256'])))
                vector(member['position_xz'], 2); covariance(member['covariance'])
                image = member['image']
                sha(image['sha256']); integer(image['size'], 1, 8 << 20)
                require(image['content_type'] in ('image/png', 'image/jpeg'), 'original camera media reference required')
                descriptor = native_evidence_descriptor(ref, image, label=sensor,
                    role='active' if index == 0 else 'previous', sample_time=member['sample_time'])
                if descriptor is not None:
                    native_evidence.append(descriptor)
                if index == 0:
                    native_members.append({'source': sensor, 'sample_id': member['sample_id'],
                                           'evidence': [{'sha256': image['sha256']}]})
            if members:
                require(max(group_samples) == (observed if index == 0 else timestamp(payload['previous_observed_at'])),
                        'observation differs from original camera group')
        require(len(models) == 1 and min(deadlines) == until and generic['inputs'] == refs,
                'original camera lineage differs from generic result')
        provenance = pub['provenance']
        require(provenance['origin'] == 'inferred' and provenance['app_version'] == 'world-live-people-v1' and
                provenance['model_id'] == ALGORITHM and provenance['model_version'] == '1' and
                len(provenance['inputs']) == len(refs), 'people inference provenance differs')
        for ref, leaf in zip(refs, provenance['inputs']):
            require(leaf['id'] == 'sha256:'+ref['envelope_sha256'] and leaf['origin'] == 'simulated' and
                    leaf['simulation']['run_id'] == shared['run_id'] and
                    leaf['simulation']['clock_domain'] == shared['clock_domain'], 'camera simulation lineage differs')
        timing = pub['timing']
        sample = timestamp(timing['sample_time'])
        error, age = integer(timing['error_bound_ns']), integer(timing['age_hint_ns'])
        ttl = integer(pub['ttl_ns'], 1, LIFETIME_NS)
        require(timing['clock_domain'] == shared['clock_domain'] and timing['error_known'] is True and
                sample == min(samples) and sample+ttl+error <= until and physical > physical_from,
                'original publication bounds differ')
        sample_until = min(until, sample+ttl-max(error, age))
        require(sample <= read_sample < sample_until, 'outer person result has expired')
        require(len(native_evidence) <= MAX_NATIVE_EVIDENCE, 'bounded native evidence required')
        track = {'id': identity, 'state': 'observed', 'position_xz': deepcopy(payload['position_xz']),
                 'covariance': deepcopy(payload['covariance']), 'last_observed': payload['observed_at'],
                 'valid_until': payload['valid_until'], 'ambiguous': payload['ambiguous'],
                 'motion_known': motion, 'members': native_members, 'original_envelope_sha256': row['envelope_sha256'],
                 'native_evidence': native_evidence}
        if motion:
            track['velocity_xz'] = deepcopy(payload['velocity_xz'])
        return {'track': track, 'sha256': row['envelope_sha256'], 'sample_until': sample_until,
                'physical_until': physical, 'physical_from': physical_from}

    def snapshot(self, source_sample_ns, sensor_sources, *, closed=False):
        current, now = self.latest, integer(self.monotonic_ns())
        if now < self.last_now:
            self.latest = current = None
        self.last_now = now
        known_sample = max(self.shared['anchor_unix_ns']+source_sample_ns, self.sample)
        tracks = []
        if not closed and current is not None and current['status'] == 'ready':
            age = now-current['started']
            for parsed in current['parsed']:
                deadline = min(current['started']+MAX_DISPLAY_AGE_NS,
                               current['started']+parsed['physical_until']-current['upper'])
                if self.shared.get('presentation_mode', 'stepped') == 'continuous':
                    deadline = min(deadline, current['started']+parsed['sample_until']-current['sample'])
                if (0 <= age and now < deadline and known_sample < parsed['sample_until'] and
                        current['lower'] >= parsed['physical_from']):
                    tracks.append({**deepcopy(parsed['track']), 'display_deadline_monotonic_ns': deadline})
        return {'participant_id': self.shared['replica_node'],
                'now_ns': known_sample-self.shared['anchor_unix_ns'], 'tracks': tracks, 'cameras': [],
                'camera_sources': [{'id': row['id'], 'label': row.get('label', row['id'].replace('-', ' ').title()),
                                    'enabled': row['enabled']} for row in sensor_sources
                                   if row['id'] in self.config['input_sources']]}
