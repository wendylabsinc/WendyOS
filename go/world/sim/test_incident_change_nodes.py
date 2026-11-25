"""Fixture wiring checks; real durability evidence comes from the actual run."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from incident_change_nodes import LinuxNode

class LinuxNodeChecks(unittest.TestCase):
    def test_persistent_state_and_exact_loopback_scope(self):
        calls=[]
        def fake(*args,**kwargs):
            calls.append(args)
            if args[:2]==('image','inspect'):return json.dumps([{'Architecture':'arm64','Id':'sha256:exact'}])
            return ''
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);binary=root/'worldd';binary.write_bytes(b'exact binary');config=root/'node.json'
            config.write_text(json.dumps({'listen':'127.0.0.1:29880','data_path':'old','proof_path':'old2','replay_path':'old3','diagnostics':{'path':'old4'},'peers':[{'url':'http://127.0.0.1:29883','audience':'node-1'}],'max_records':128}))
            with patch('incident_change_nodes.docker',fake):
                node=LinuxNode(binary,'local',config,root,root,1,29880)
            actual=json.loads((root/'linux-node-1.json').read_text())
            self.assertEqual(actual['data_path'],'/state/node-1.store')
            self.assertEqual(actual['peers'][0]['url'],'http://host.docker.internal:29883')
            self.assertEqual(actual['max_records'],128)
            create=next(c for c in calls if c[0]=='create')
            self.assertIn('type=volume,src='+node.volume+',dst=/state',create)
            self.assertIn('127.0.0.1:29880:29880',create)
            self.assertIn('--read-only',create)
            self.assertIn('--pull=never',create)
            self.assertNotIn('--tmpfs',create)
            self.assertEqual(create[-3:],('sha256:exact','--config','/config.json'))
            self.assertEqual((root/'linux-node-1.json').stat().st_mode & 0o777,0o600)

if __name__=='__main__':unittest.main()
