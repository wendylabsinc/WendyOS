"""Cloud copy ownership and post-work freshness remain bridge boundaries."""
import copy
import unittest
from unittest.mock import patch

from sim.go2_bridge import core
from sim.go2_bridge.protocol import Refused
from sim.test_go2_bridge import Time
from sim.test_go2_cloud_bridge_review import CloudTransport, cloud_configuration


class CloudCopyTests(unittest.TestCase):
    def setUp(self):
        self.cfg=cloud_configuration();self.time=Time()
        self.transport=CloudTransport(self.time,self.cfg)
        self.bridge=core.Bridge(self.cfg,self.transport,self.time.clock(),fault_reporter=lambda _:None)
        self.state=self.bridge.dispatch('/v1/session',{'binding':self.cfg.binding})
        self.bridge.dispatch('/v1/begin',{'session':self.state['session'],'executor_epoch':7,
            'task_id':'copy-test','spec_digest':'a'*64,'binding_sha256':self.cfg.binding_sha256})

    def command(self):
        return dict(session=self.state['session'],sequence=1,executor_epoch=7,task_id='copy-test',
                    spec_digest='a'*64,capture_sequence=1,body_vx=.1,body_vy=0.,yaw_rate=0.,
                    not_after_unix_ms=self.time.w//1_000_000+500,decision_remaining_ms=500)

    def test_all_mutable_capture_containers_are_detached(self):
        source=self.transport.capture()
        self.bridge._cloud(source)
        result=core._copy_validated_capture(source)
        self.assertEqual(result,copy.deepcopy(source))
        def walk(left,right):
            if isinstance(left,dict):
                self.assertIsNot(left,right)
                for key,value in left.items():walk(value,right[key])
            elif isinstance(left,list):
                self.assertIsNot(left,right)
                for a,b in zip(left,right):walk(a,b)
        walk(source,result)
        source['cloud']['ranges'][0]=2.
        source['tilted_pose']['quaternion_wxyz'][0]=.8
        self.assertNotEqual(source['cloud']['ranges'],result['cloud']['ranges'])
        self.assertNotEqual(source['tilted_pose'],result['tilted_pose'])

    def test_output_edits_do_not_change_cache_or_original(self):
        retained=copy.deepcopy(self.bridge.served_captures[1])
        self.state['cloud']['ranges'][0]=3.
        self.state['cloud']['scan_sha256']='e'*64
        self.state['scan']['ranges'][0]=2.
        self.state['pose_covariance'][0]=7.
        self.assertEqual(self.bridge.served_captures[1],retained)
        current=self.bridge.dispatch('/v1/state',{'session':self.state['session']})
        self.assertEqual(current['cloud'],retained['cloud'])
        self.assertEqual(current['scan']['ranges'],retained['scan']['ranges'])
        self.assertEqual(current['pose_covariance'],retained['pose_covariance'])

    def test_changed_transport_does_not_mutate_previously_served_cache(self):
        retained=copy.deepcopy(self.bridge.served_captures[1])
        self.transport.latest['cloud']['ranges'][0]=2.
        with self.assertRaisesRegex(Refused,'cloud_capture_changed'):
            self.bridge.dispatch('/v1/state',{'session':self.state['session']})
        self.assertEqual(self.bridge.served_captures[1],retained)

    def test_copy_delay_still_expires_original_before_publication(self):
        self.time.advance(249)
        original=core._copy_validated_capture
        def delayed(c):
            out=original(c);self.time.advance(1);return out
        with patch.object(core,'_copy_validated_capture',delayed):
            with self.assertRaisesRegex(Refused,'served_capture_stale'):
                self.bridge.dispatch('/v1/command',self.command())
        self.assertFalse(any(any(values) for values,_,_ in self.transport.sent))
        self.assertEqual(self.bridge.used_capture,0)
        self.assertEqual(self.bridge.sequence,0)


if __name__=='__main__':unittest.main()
