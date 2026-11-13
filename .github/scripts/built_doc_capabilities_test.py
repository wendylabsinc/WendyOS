#!/usr/bin/env python3
"""Synthetic admission controls; the required gate separately reads real SBOMs."""
import copy
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('capabilities', Path(__file__).with_name('check-built-doc-capabilities.py'))
capabilities = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capabilities)


def graph():
    return {'@context': 'https://spdx.org/rdf/3.0.1/spdx-context.jsonld', '@graph': [
        {'type': 'software_Sbom', 'spdxId': 'sbom', 'name': 'fixture-image', 'rootElement': ['owned/rootfs/image']},
        {'type': 'software_Package', 'spdxId': 'owned/rootfs/image'},
        {'type': 'Relationship', 'spdxId': 'edge', 'from': 'owned/rootfs/image', 'relationshipType': 'contains', 'to': ['installed']},
        {'type': 'software_File', 'spdxId': 'installed', 'name': 'usr/bin/wendyos-update',
         'verifiedUsing': [{'algorithm': 'sha256', 'hashValue': 'a' * 64}]},
        # A recipe's source can appear in the same SBOM without being installed.
        {'type': 'software_File', 'spdxId': 'source-only', 'name': 'usr/lib/systemd/system/var-log.mount',
         'verifiedUsing': [{'algorithm': 'sha256', 'hashValue': 'b' * 64}]}]}


class BuiltCapabilitiesAdmission(unittest.TestCase):
    def test_only_actual_root_membership_qualifies(self):
        self.assertEqual(capabilities.installed(graph(), 'fixture-image'), {'usr/bin/wendyos-update': 'a' * 64})

    def test_empty_wrong_image_and_ambiguous_graphs_refused(self):
        empty = graph(); empty['@graph'][2]['to'] = []
        duplicate = graph(); duplicate['@graph'].append(copy.deepcopy(duplicate['@graph'][3]))
        wrong = graph(); wrong['@graph'][0]['name'] = 'another-image'
        for document in (empty, duplicate, wrong):
            with self.assertRaises(RuntimeError):
                capabilities.installed(document, 'fixture-image')

    def test_missing_digest_cannot_qualify_as_installed_source(self):
        document = graph(); document['@graph'][3]['verifiedUsing'] = []
        with self.assertRaisesRegex(RuntimeError, 'missing installed file digest'):
            capabilities.installed(document, 'fixture-image')

    def test_old_age_retention_claim_fails_against_configuration(self):
        journal = '[Journal]\nStorage=persistent\nSystemMaxUse=512M\n'
        mount = '[Mount]\nWhat=/data/log\nWhere=/var/log\n'
        docs = '```ini\n' + journal + '```\n```ini\n' + mount + '```'
        capabilities.check_configuration(docs, journal, mount)
        with self.assertRaisesRegex(RuntimeError, 'DOC_PACKAGED_JOURNAL'):
            capabilities.check_configuration(docs.replace('Storage=persistent', 'Storage=persistent\nMaxRetentionSec=1month'), journal, mount)

    def test_tampered_cached_artifact_fails_before_parsing(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            (directory / 'fixture').write_text('{}')
            with self.assertRaisesRegex(RuntimeError, 'artifact digest mismatch'):
                capabilities.acquire(directory, 'fixture', 'unused', '0' * 64, offline=True)


if __name__ == '__main__':
    unittest.main()
