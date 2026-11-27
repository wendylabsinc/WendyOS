import json
import base64
from copy import deepcopy
import tempfile
import unittest
from pathlib import Path
from integration.localinspection import run


class LocalInspectionTests(unittest.TestCase):
    def test_existing_signed_policy_survives_path_relocation(self):
        cfg = {'listen': ':8093', 'audience': 'atlas', 'data_path': '/state/world.json',
            'proof_path': '/state/proof.json', 'replay_path': '/state/replay.json',
            'revocation_path': '/state/revocations.json', 'tasks': {'path':'/state/tasks.json',
            'assignee':'atlas/navigation', 'control_context': {'context':{'run_id':'actual'}, 'signature':'exact'}},
            'proposals':{'path':'/state/proposals.json'},'evidence':{'path':'/state/evidence'},
            'peer_grant':{'grant':{'principal':'gateway'}}, 'root_public_key':'retained'}
        original=json.loads(json.dumps(cfg))
        changed=run.relocate_gateway(cfg,Path('/tmp/owned/state'),'127.0.0.1:1234')
        self.assertEqual(cfg,original)
        self.assertEqual(changed['tasks']['control_context'],cfg['tasks']['control_context'])
        self.assertEqual(changed['root_public_key'],'retained')
        self.assertEqual(changed['task_relay']['routes']['atlas'],
                         {'assignee':'atlas/navigation','status_principal':'gateway'})
        self.assertEqual(changed['evidence']['path'],'/tmp/owned/state/evidence')

    def test_expired_selected_hazard_clears_map_without_geometry_rewrite(self):
        hazard={'position_m':[1,.65,2],'cameras':[], 'inputs':[], 'audio':{},
                'uncertainty_m':.4,'confidence':.8,'valid_until':'2026-01-01T00:00:04.17Z'}
        p={'dynamic_geoms':[{'id':1,'position':[1,-2,.24],'rotation':[1,0,0,0,1,0,0,0,1]}]}
        current=run.display_state('run',1,p,hazard,'a'*64,4_160_000_000,'inspection',observation_sample_ns=1_200_000_000)
        expired=run.display_state('run',2,p,hazard,'a'*64,4_170_000_000,'inspection',observation_sample_ns=1_200_000_000)
        self.assertEqual(current['snapshot']['fusion']['original_envelope_sha256'],'a'*64)
        self.assertEqual(current['snapshot']['fusion']['position'],[1,-2,.65])
        self.assertEqual(current['snapshot']['fusion']['sample_time_s'],1.2)
        self.assertEqual(current['snapshot']['time_s'],4.16)
        earlier=run.display_state('run',0,p,hazard,'a'*64,2_000_000_000,'inspection',observation_sample_ns=1_200_000_000)
        self.assertEqual(earlier['snapshot']['fusion']['sample_time_s'],current['snapshot']['fusion']['sample_time_s'])
        self.assertEqual(current['snapshot']['fusion']['valid_until_s'],4.17)
        self.assertEqual(expired['snapshot']['fusion']['state'],'no_live_result')
        self.assertNotIn('original_envelope_sha256',expired['snapshot']['fusion'])
        self.assertEqual(expired['dynamic_geoms'],p['dynamic_geoms'])
        self.assertEqual(current['mode'],'local-inspection')
        self.assertEqual(current['snapshot']['robots'],[])

    def test_exclusive_output_preserves_existing_original(self):
        with tempfile.TemporaryDirectory() as directory:
            p=Path(directory)/'original.json'
            run.write(p,b'original')
            with self.assertRaises(FileExistsError):run.write(p,b'replacement')
            self.assertEqual(p.read_bytes(),b'original')

    def test_completion_requires_named_task_and_exact_selected_original(self):
        trigger={'sha256':'a'*64,'component':{'key':{'entity':'selected'}}}
        handoff={'proposal_id':'selected-proposal','mission_input':{
            'trigger_json':base64.b64encode(json.dumps(trigger).encode()).decode(),
            'trigger_sha256':'a'*64},'navigation_target':[1,.24,2],
            'inspection_destination':{'version':1,'mode':'planar_nearby'}}
        task={'spec':{'id':'expected','proposal':{'id':'selected-proposal'},'trigger':trigger,
                      'target':[1,.24,2],'inspection_destination':handoff['inspection_destination']}}
        self.assertTrue(run.selected_task(task,handoff,'expected'))
        self.assertFalse(run.selected_task(task,handoff,'other'))
        for field,value in [('proposal',{'id':'other'}),('trigger',{'sha256':'b'*64}),('target',[3,.24,2])]:
            changed=deepcopy(task);changed['spec'][field]=value
            self.assertFalse(run.selected_task(changed,handoff,'expected'))
        with self.assertRaises(ValueError):run.validate_mode('auto',['native-ui'])
        run.validate_mode('ready',['native-ui'])

if __name__=='__main__':unittest.main()
