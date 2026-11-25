import unittest
from sim.patrol_work import WorkMeter

class WorkTests(unittest.TestCase):
    def meter(self): return WorkMeter('run', 'a'*64)
    def test_actual_joint_units_positive_only_and_no_mutation(self):
        m=self.meter();torque=[2.]*6+[-2.]*6;velocity=[3.]*12
        for i in range(1,501):m.step(i*.002,.002,torque,velocity,1000+i*2)
        s=m.snapshot();self.assertAlmostEqual(s['total_j'],36.);self.assertEqual(s['power_w'],36.)
        self.assertEqual(s['sequence'],500);self.assertEqual(s['simulation_ns'],1_000_000_000)
        self.assertEqual(torque,[2.]*6+[-2.]*6);self.assertEqual(velocity,[3.]*12)
        s['total_j']=0;self.assertGreater(m.snapshot()['total_j'],35.9)
    def test_reads_do_not_retimestamp_and_reset_is_unknown(self):
        m=self.meter();m.step(.002,.002,[1]*12,[1]*12,1234)
        first=m.snapshot();self.assertEqual(first,m.snapshot())
        m.step(.002,.002,[1]*12,[1]*12,1240);self.assertFalse(m.snapshot()['valid'])
        m.step(.004,.002,[1]*12,[1]*12,1241);self.assertFalse(m.snapshot()['valid'])
        self.assertNotEqual(m.sample['boot_id'],self.meter().sample['boot_id'])
    def test_gaps_and_unknown_inputs_never_report_valid_zero(self):
        for torque,stamp in [([float('nan')]*12,.002),([1]*11,.002),([1]*12,.006)]:
            m=self.meter();m.step(.002,.002,[1]*12,[1]*12,1);m.step(stamp,.002,torque,[1]*12,2)
            self.assertFalse(m.snapshot()['valid']);self.assertGreater(m.snapshot()['total_j'],0)

if __name__=='__main__':unittest.main()
