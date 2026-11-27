import ctypes
import importlib.util
import os
from pathlib import Path
import stat
import sys
import tempfile
from types import SimpleNamespace
import unittest

import control
import model


class AtomicControlTests(unittest.TestCase):
    def receiver(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        path = Path(directory.name) / 'receiver.py'
        path.write_text(control.ATOMIC_WRITE)
        spec = importlib.util.spec_from_file_location('fixed_receiver_test', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        scope = module.__dict__
        if sys.platform == 'darwin':
            # Test the same receiver with Darwin's native no-replace rename.
            # Production receiver has only Linux renameat2, no fallback.
            def rename(fd, source, destination):
                fn = ctypes.CDLL(None, use_errno=True).renameatx_np
                fn.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
                fn.restype = ctypes.c_int
                if fn(fd, os.fsencode(source), fd, os.fsencode(destination), 4):
                    raise OSError(ctypes.get_errno(), 'no-replace test rename refused')
            scope['no_replace'] = rename
        return scope

    def test_atime_is_excluded_but_identity_mode_content_metadata_are_pinned(self):
        fn = self.receiver()['stable']
        fields = {'st_dev': 1, 'st_ino': 2, 'st_mode': 0o100600, 'st_uid': 3, 'st_gid': 4,
                  'st_nlink': 1, 'st_size': 10, 'st_mtime_ns': 100, 'st_ctime_ns': 200, 'st_atime_ns': 300}
        first = SimpleNamespace(**fields)
        self.assertEqual(fn(first), fn(SimpleNamespace(**{**fields, 'st_atime_ns': 400})))
        for name in fields.keys() - {'st_atime_ns'}:
            with self.subTest(name=name):
                self.assertNotEqual(fn(first), fn(SimpleNamespace(**{**fields, name: fields[name] + 1})))

    def test_complete_single_link_visible_and_existing_final_never_overwritten(self):
        scope = self.receiver()
        with tempfile.TemporaryDirectory() as directory:
            fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                original = scope['no_replace']; visible = []
                def inspect(fd, source, target):
                    original(fd, source, target)
                    # This is the first point at which a polling reader can
                    # open final. It already has exactly one link/full bytes.
                    body, _ = scope['read'](fd, target)
                    visible.append((body, os.stat(target, dir_fd=fd).st_nlink))
                scope['no_replace'] = inspect
                scope['publish'](fd, '000001.json', b'{"complete":true}', '-')
                self.assertEqual(visible, [(b'{"complete":true}', 1)])
                with self.assertRaises(OSError): scope['publish'](fd, '000001.json', b'other', '-')
                self.assertEqual(Path(directory, '000001.json').read_bytes(), b'{"complete":true}')
                self.assertEqual(os.listdir(directory), ['000001.json'])
            finally: os.close(fd)

    def test_clock_replacement_requires_exact_previous_protected_bytes(self):
        scope = self.receiver()
        with tempfile.TemporaryDirectory() as directory:
            fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                scope['publish'](fd, 'clock.json', b'first', '-')
                with self.assertRaises(AssertionError): scope['publish'](fd, 'clock.json', b'second', 'a' * 64)
                self.assertEqual(Path(directory, 'clock.json').read_bytes(), b'first')
                scope['publish'](fd, 'clock.json', b'second', model.digest(b'first'))
                self.assertEqual(Path(directory, 'clock.json').read_bytes(), b'second')
                os.chmod(Path(directory, 'clock.json'), 0o666)
                with self.assertRaises(AssertionError): scope['publish'](fd, 'clock.json', b'third', model.digest(b'second'))
                self.assertEqual(os.listdir(directory), ['clock.json'])
            finally: os.close(fd)

    def test_symlink_and_hardlinked_previous_file_refused(self):
        scope = self.receiver()
        with tempfile.TemporaryDirectory() as directory:
            fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                scope['publish'](fd, 'a', b'old', '-')
                os.symlink('a', Path(directory, 's'))
                with self.assertRaises(OSError): scope['publish'](fd, 's', b'new', model.digest(b'old'))
                os.link(Path(directory, 'a'), Path(directory, 'b'))
                with self.assertRaises(AssertionError): scope['publish'](fd, 'a', b'new', model.digest(b'old'))
                self.assertEqual(Path(directory, 'a').read_bytes(), b'old')
            finally: os.close(fd)

    def test_unsafe_or_uncanonical_parent_refused(self):
        scope = self.receiver()
        for path in ('relative', '//private/x', '/a/../b', '/a//b'):
            with self.subTest(path=path), self.assertRaises(AssertionError): scope['parent'](path)
        value = SimpleNamespace(st_uid=os.geteuid(), st_mode=stat.S_IFDIR | 0o777)
        with self.assertRaises(AssertionError): scope['checked'](value)


class ContractTests(unittest.TestCase):
    def test_commands_and_sample_domains_are_bounded(self):
        self.assertEqual(model.strict_json(control.command('await_query', 3, {'expect': 'absent'}))['sequence'], 3)
        for fields in ({'expect': 'present'}, {'expect': 'absent', 'envelope_sha256': 'a' * 64}, {'expect': 'other'}):
            with self.assertRaises(model.Refused): control.command('await_query', 1, fields)
        a, at = control.sample_clock('run-id', 0); b, bt = control.sample_clock('run-id', 31)
        self.assertEqual(bt - at, 31_000_000_000)
        self.assertEqual(model.strict_json(a)['domain'], model.strict_json(b)['domain'])
        self.assertEqual(model.strict_json(a)['now'], model.SAMPLE_START)
        with self.assertRaises(model.Refused): control.sample_clock('run-id', -1)

    def test_partition_matches_only_exact_private_peer_tcp_carrier(self):
        rows = control.partition_rules('world-agentpair-' + 'a' * 12, '172.19.0.4')
        self.assertEqual([r['direction'] for r in rows], ['INPUT', 'OUTPUT'])
        for row in rows:
            raw = ('-N ' + row['chain'] + '\n' + ''.join('-A ' + row['chain'] + ' ' + ' '.join(rule) + '\n' for rule in row['rules'])).encode()
            control.validate_chain(raw, row)
            with self.assertRaises(model.Refused): control.validate_chain(raw.replace(b'43020', b'443'), row)
            with self.assertRaises(model.Refused): control.validate_chain(raw + b'-A FOREIGN -j DROP\n', row)
            self.assertTrue(all('tcp' in rule and '172.19.0.4/32' in rule and '43020' in rule for rule in row['rules']))
        for address in ('127.0.0.1', '8.8.8.8', '::1', '172.19.0.4/24'):
            with self.assertRaises(model.Refused): control.partition_rules('world-agentpair-' + 'a' * 12, address)

    def test_packet_counters_are_not_inferred_from_rules(self):
        row = control.partition_rules('world-agentpair-' + 'a' * 12, '172.19.0.4')[0]
        raw = ('Chain ' + row['chain'] + ' (1 references)\n pkts bytes target prot opt in out source destination\n'
               ' 2 100 DROP tcp -- * * 172.19.0.4 0.0.0.0/0 tcp spt:43020\n'
               ' 3 100 DROP tcp -- * * 172.19.0.4 0.0.0.0/0 tcp dpt:43020\n').encode()
        self.assertEqual(control.chain_drops(raw, row), 5)
        with self.assertRaises(model.Refused): control.chain_drops(raw.replace(b'DROP tcp', b'ACCEPT udp'), row)


class VolumeDirectoryTests(unittest.TestCase):
    def fixture(self):
        # Keep ancestors protected so the real anchored receiver can be used.
        configured = os.environ.get('WENDY_PAIR_TEST_TMPDIR')
        parent = Path(configured) if configured else Path(__file__).resolve().parents[2] / '.local'
        directory = tempfile.TemporaryDirectory(prefix='volume-mode-test-', dir=parent)
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        path = root / 'volume_creator.py'
        path.write_text(control.VOLUME_DIRECTORIES)
        spec = importlib.util.spec_from_file_location('fixed_volume_creator_test', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        names = list(model.volume_names('mode-test', 'a', 1).values())
        return root, module.prepare, names

    def test_umask_zero_reproduces_old_guard_failure_and_new_creation_passes(self):
        root, prepare, names = self.fixture()
        receiver = AtomicControlTests.receiver(self)
        previous = os.umask(0)
        try:
            legacy = root / 'legacy'; legacy.mkdir(mode=0o700)
            old = legacy / 'wendy/volumes'
            old.mkdir(parents=True, exist_ok=True)
            for name in names: (old / name).mkdir(mode=0o700)
            self.assertEqual(stat.S_IMODE(old.stat().st_mode), 0o777)
            with self.assertRaises(AssertionError): receiver['parent'](str(old / names[1] / '000001.json'))

            fixed = root / 'fixed'; fixed.mkdir(mode=0o700)
            destination = fixed / 'wendy/volumes'
            receipt = prepare(str(destination), names)
            self.assertEqual(len(receipt['directories']), 5)
            self.assertTrue(all(row['created'] and row['mode'] == 0o700 for row in receipt['directories']))
            fd, name = receiver['parent'](str(destination / names[1] / '000001.json'))
            try: receiver['publish'](fd, name, b'{"complete":true}', '-')
            finally: os.close(fd)
            self.assertEqual((destination / names[1] / '000001.json').read_bytes(), b'{"complete":true}')
        finally: os.umask(previous)

    def test_safe_existing_roots_are_preserved_and_phase_names_are_exclusive(self):
        root, prepare, names = self.fixture()
        wendy = root / 'wendy'; wendy.mkdir(mode=0o755); os.chmod(wendy, 0o755)
        volumes = wendy / 'volumes'; volumes.mkdir(mode=0o750); os.chmod(volumes, 0o750)
        before = [(p.stat().st_ino, stat.S_IMODE(p.stat().st_mode)) for p in (wendy, volumes)]
        receipt = prepare(str(volumes), names)
        self.assertEqual([r['created'] for r in receipt['directories']], [False, False, True, True, True])
        self.assertEqual(before, [(p.stat().st_ino, stat.S_IMODE(p.stat().st_mode)) for p in (wendy, volumes)])
        with self.assertRaises(FileExistsError): prepare(str(volumes), names)
        second = list(model.volume_names('mode-test', 'a', 2).values())
        (volumes / second[-1]).mkdir(mode=0o700)
        with self.assertRaises(FileExistsError): prepare(str(volumes), second)
        self.assertFalse((volumes / second[0]).exists())
        self.assertFalse((volumes / second[1]).exists())

    def test_unsafe_existing_roots_and_symlinks_are_refused_without_repair(self):
        for location in ('wendy', 'volumes'):
            with self.subTest(location=location):
                root, prepare, names = self.fixture()
                wendy = root / 'wendy'; wendy.mkdir(mode=0o700)
                volumes = wendy / 'volumes'; volumes.mkdir(mode=0o700)
                bad = wendy if location == 'wendy' else volumes
                os.chmod(bad, 0o777)
                with self.assertRaises(AssertionError): prepare(str(volumes), names)
                self.assertEqual(stat.S_IMODE(bad.stat().st_mode), 0o777)
                self.assertEqual(list(volumes.iterdir()), [])
        for location in ('wendy', 'volumes', 'phase'):
            with self.subTest(symlink=location):
                root, prepare, names = self.fixture()
                outside = root / 'outside'; outside.mkdir(mode=0o700)
                wendy = root / 'wendy'; volumes = wendy / 'volumes'
                if location == 'wendy': wendy.symlink_to(outside, target_is_directory=True)
                else:
                    wendy.mkdir(mode=0o700)
                    if location == 'volumes': volumes.symlink_to(outside, target_is_directory=True)
                    else:
                        volumes.mkdir(mode=0o700)
                        (volumes / names[-1]).symlink_to(outside, target_is_directory=True)
                with self.assertRaises(OSError): prepare(str(volumes), names)
                self.assertEqual(list(outside.iterdir()), [])

    def test_metadata_requires_exact_root_owner_modes_and_phase_components(self):
        names = list(model.volume_names('mode-test', 'a', 1).values())
        value = {'version': 1, 'directories': [{'component': n, 'uid': 0, 'mode': 0o755 if i < 2 else 0o700,
                                               'created': i >= 2} for i, n in enumerate(['wendy', 'volumes', *names])]}
        self.assertEqual(control.volume_receipt(model.json_bytes(value), names), value)
        import copy
        changes = [lambda x: x['directories'][0].update(uid=1000),
                   lambda x: x['directories'][1].update(mode=0o777),
                   lambda x: x['directories'][2].update(mode=0o755),
                   lambda x: x['directories'][2].update(created=False),
                   lambda x: x['directories'][4].update(component='foreign'),
                   lambda x: x.update(raw_error='unbounded'),
                   lambda x: x['directories'].append(x['directories'][0])]
        for change in changes:
            damaged = copy.deepcopy(value); change(damaged)
            with self.subTest(change=change), self.assertRaises(model.Refused):
                control.volume_receipt(model.json_bytes(damaged), names)


if __name__ == '__main__': unittest.main()
