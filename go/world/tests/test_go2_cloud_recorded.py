import copy
import gzip
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0,str(Path(__file__).resolve().parent))
import go2_cloud_recorded as replay

ROOT=Path(__file__).resolve().parents[1]
from verification_archive import archive_root


class RecordedChecks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        archive=archive_root()
        config=archive/'go2-zero-selection/preparation'
        cls.profile=json.loads((config/'candidate-profile.json').read_text())
        cls.alignment=json.loads((config/'candidate-alignment.json').read_text())
        cls.captures={}
        for letter in ['a','b']:
            base=archive/'go2-cloud/capture'/letter
            raw=gzip.decompress((base/f'passive-{letter}.json.gz').read_bytes())
            report,decoded,clouds=replay.load_recording(raw)
            before=json.loads((base/f'passive-{letter}-runtime-before.json').read_text())
            after=json.loads((base/f'passive-{letter}-runtime-after.json').read_text())
            cls.captures[letter]=(report,decoded,clouds,before,after)
        cls.bounds={'Min':{'X':-4,'Y':-4},'Max':{'X':4,'Y':4}}

    def request(self, report,decoded,clouds,before,after,index=0,prior=None):
        age=replay.context(report,before,after,self.profile,self.alignment)
        return replay.make_request(report,decoded,clouds[index],report['clouds'][index],self.profile,self.alignment,age,self.bounds,[] if prior is None else prior)

    def test_actual_complete_and_partial_records(self):
        counts=[]
        for values in self.captures.values():
            report,decoded,clouds,before,after=values; accepted=0
            for i,item in enumerate(report['clouds']):
                if item['missing_joins']:
                    with self.assertRaises(ValueError):self.request(*values,index=i)
                else:
                    request=self.request(*values,index=i); accepted+=1
                    self.assertEqual(request['observation']['captured_unix_ns'],item['capture_unix_ns'])
                    self.assertEqual(request['observation']['scan']['TiltedPose']['odom_position_xyz'],item['original_odometry']['position_xyz'])
                    self.assertEqual(len(request['observation']['ranges']),9000)
                    self.assertEqual(request['continuation']['prior'],[])
            counts.append(accepted)
        self.assertEqual(counts,[4,3])

    def test_runtime_oracle_does_not_enter_geometry(self):
        values=copy.deepcopy(self.captures['a']); expected=self.request(*values)
        for state in values[-2:]:
            state['position']=[99,99,99];state['quaternion_wxyz']=[0,1,0,0]
            state['linear_velocity_world']=[99,99,99];state['joints']={'untrusted':'ignored'}
        self.assertEqual(self.request(*values),expected)

    def test_original_capture_and_motion_survive_counterfactual(self):
        values=self.captures['a']; a=self.request(*values)
        b=self.request(*values,prior=[{'ForwardMPS':.3,'YawRateRPS':0}])
        self.assertEqual(a['observation'],b['observation'])
        self.assertEqual(a['continuation']['measured'],b['continuation']['measured'])
        self.assertEqual(b['continuation']['proposed'],{'ForwardMPS':0,'YawRateRPS':0})

    def test_context_changes_refuse(self):
        for key,value in [('epoch',999),('source_digest','wrong'),('time',121),('simulation',False)]:
            values=copy.deepcopy(self.captures['a']);values[-1][key]=value
            with self.subTest(key=key),self.assertRaises(ValueError):self.request(*values)

    def test_full_epoch_age_increases_error_without_retiming(self):
        values=copy.deepcopy(self.captures['a']);a=self.request(*values);values[-1]['time']+=1;b=self.request(*values)
        self.assertEqual(a['observation']['captured_unix_ns'],b['observation']['captured_unix_ns'])
        self.assertGreater(b['observation']['scan']['PositionUncertaintyM'],a['observation']['scan']['PositionUncertaintyM'])
        self.assertGreater(b['observation']['scan']['TiltedPose']['base_height_max_m'],a['observation']['scan']['TiltedPose']['base_height_max_m'])

    def test_both_mount_frames_must_match(self):
        values=copy.deepcopy(self.captures['a']);report,decoded,*_=values
        for value,row in zip(decoded,report['messages']):
            if row['topic']=='/tf_static':
                for t in value['transforms']:
                    if t['child_frame_id']=='lidar_link':t['translation_xyz'][0]=.29
        with self.assertRaises(ValueError):self.request(*values)

    def test_no_complete_capture_does_not_claim_assessment(self):
        report,decoded,clouds,before,after=copy.deepcopy(self.captures['a'])
        for item in report['clouds']:item['missing_joins']=['same_capture_scan']
        with tempfile.TemporaryDirectory() as directory:
            base=Path(directory)
            files={'capture':{},'runtime-before':before,'runtime-after':after,'profile':self.profile,'alignment':self.alignment,'checker':{}}
            for key,value in files.items():(base/key).write_bytes(replay.encoded(value))
            argv=['go2_cloud_recorded.py']
            for key in files:argv+=['--'+key,str(base/key)]
            argv+=['--out',str(base/'result'),'--area-min','-4','-4','--area-max','4','4']
            with mock.patch.object(sys,'argv',argv),mock.patch.object(replay,'load_recording',return_value=(report,decoded,clouds)),mock.patch.object(replay.subprocess,'run') as run,mock.patch('builtins.print') as output:
                self.assertEqual(replay.main(),1)
                run.assert_not_called()
                self.assertFalse(json.loads(output.call_args.args[0])['all_checks_ran'])
            summary=json.loads((base/'result/summary.json').read_text())
            self.assertEqual(summary['cases'],[])
            self.assertEqual(len(summary['skipped']),5)


if __name__=='__main__':unittest.main()
