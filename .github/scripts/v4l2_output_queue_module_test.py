"""Offline provenance and refusal checks; never compile/load a kernel module."""
import hashlib
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).parent
spec = importlib.util.spec_from_file_location('queue_provenance', HERE / 'v4l2-output-queue-provenance.py')
PROVENANCE = importlib.util.module_from_spec(spec)
spec.loader.exec_module(PROVENANCE)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class ModuleProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source, self.builder = self.root / 'source', self.root / 'builder'
        for root in (self.source, self.builder):
            root.mkdir()
            subprocess.run(['git', 'init', '-q', str(root)], check=True)
        (self.source / 'v4l2loopback.c').write_text('synthetic provenance fixture; not a kernel implementation\n')
        (self.source / '.gitversion').write_text('synthetic snapshot\n')
        upstream = self.commit(self.source)
        recipe = self.builder / PROVENANCE.RECIPE
        recipe.parent.mkdir(parents=True)
        recipe.write_text('SRCREV = "' + upstream + '"\nSRC_URI += "file://' + PROVENANCE.PATCH.name + '"\n')
        patch = self.builder / PROVENANCE.PATCH
        patch.parent.mkdir()
        patch.write_text('synthetic source provenance only\n')
        builder = self.commit(self.builder)
        self.pins = {'upstream_revision': upstream, 'builder_revision': builder,
                     'source_sha256': digest(self.source / 'v4l2loopback.c'),
                     'recipe_sha256': digest(recipe), 'patch_sha256': digest(patch)}

    def commit(self, root):
        subprocess.run(['git', '-C', str(root), 'add', '-f', '.'], check=True)
        subprocess.run(['git', '-C', str(root), '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
                        '-c', 'commit.gpgsign=false', 'commit', '-qm', 'synthetic provenance fixture'], check=True)
        return PROVENANCE.git(root, 'rev-parse', 'HEAD')

    def validate(self):
        return PROVENANCE.validate(self.source, self.builder, self.pins)

    def test_exact_sources_and_only_version_marker_removal_are_accepted(self):
        self.validate()
        (self.source / '.gitversion').unlink()
        self.validate()
        (self.source / 'v4l2loopback.c').write_text('changed source\n')
        with self.assertRaisesRegex(ValueError, 'changes beyond'):
            self.validate()

    def test_committed_different_candidate_and_changed_patch_are_refused(self):
        patch = self.builder / PROVENANCE.PATCH
        patch.write_text('different patch\n')
        with self.assertRaisesRegex(ValueError, 'differs from'):
            self.validate()
        self.commit(self.builder)
        with self.assertRaisesRegex(ValueError, 'unexpected Builder revision'):
            self.validate()

    def test_hashes_and_recipe_source_and_patch_selection_are_required(self):
        for key in ('source_sha256', 'recipe_sha256', 'patch_sha256'):
            with self.subTest(key=key):
                pins = {**self.pins, key: '0' * 64}
                with self.assertRaisesRegex(ValueError, key):
                    PROVENANCE.validate(self.source, self.builder, pins)
        recipe = self.builder / PROVENANCE.RECIPE
        original = recipe.read_text()
        for altered, message in ((original.replace(self.pins['upstream_revision'], '0' * 40), 'verified upstream'),
                                 (original.replace('file://' + PROVENANCE.PATCH.name, 'file://other.patch'), 'exact candidate patch')):
            with self.subTest(message=message):
                recipe.write_text(altered)
                self.pins['builder_revision'] = self.commit(self.builder)
                self.pins['recipe_sha256'] = digest(recipe)
                with self.assertRaisesRegex(ValueError, message):
                    self.validate()

    def test_local_self_hosted_and_unowned_invocations_stop_before_actions(self):
        for actions, environment, owned in (('false', 'github-hosted', '1'), ('true', 'self-hosted', '1'), ('true', 'github-hosted', '0')):
            result = subprocess.run(['bash', str(HERE / 'test-v4l2-output-queue-module.sh')],
                                    env={**os.environ, 'GITHUB_ACTIONS': actions, 'RUNNER_ENVIRONMENT': environment,
                                         'WENDY_LOOPBACK_JOB_OWNED': owned}, text=True, capture_output=True, timeout=3)
            self.assertEqual(result.returncode, 2)
            self.assertIn('requires the disposable hosted media job', result.stderr)


if __name__ == '__main__':
    unittest.main()
