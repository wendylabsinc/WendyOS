"""Deterministic acceptance-wait regression; no simulation or wall-clock sleep."""
import unittest
from unittest.mock import patch
import incident_change

class Clock:
    now=0.0
    def monotonic(self):return self.now
    def sleep(self,seconds):self.now+=seconds

class Demo:
    def __init__(self,valid_after=None):self.sequence=0;self.valid_after=valid_after;self.last={}
    def step(self):
        self.sequence+=1
        ready=self.valid_after is not None and self.sequence>=self.valid_after
        self.last={name:{'result':{'state':'unavailable','reason':'synthetic acceptance transition','spatial':{'state':'consistent' if ready or name=='east' else 'unavailable'},'temporal':{'state':'unavailable'}}} for name in ('east','west')}
        return self.last

class SettleTests(unittest.TestCase):
    def run_wait(self,demo,clock,records):
        with patch.object(incident_change.time,'monotonic',clock.monotonic),patch.object(incident_change.time,'sleep',clock.sleep):
            return incident_change.settle_state(demo,records.append,'temporal missing','unavailable',2,lambda values:all(x['result']['spatial']['state']=='consistent' for x in values.values()))
    def test_wrong_substate_does_not_return_early(self):
        demo=Demo(valid_after=40);clock=Clock();records=[]
        result=self.run_wait(demo,clock,records)
        self.assertEqual(demo.sequence,40)
        self.assertGreaterEqual(clock.now,2)
        self.assertTrue(all(x['result']['spatial']['state']=='consistent' for x in result.values()))
        self.assertEqual(records[0]['observed'][0]['west']['spatial'],'unavailable')
    def test_step_finishing_after_bound_cannot_pass(self):
        demo=Demo(valid_after=1);clock=Clock();records=[];step=demo.step
        def slow_step():
            clock.now+=14.01
            return step()
        demo.step=slow_step
        with self.assertRaises(AssertionError):self.run_wait(demo,clock,records)
        self.assertEqual(demo.sequence,1)
        self.assertEqual(records,[])
    def test_wrong_substate_still_fails_at_original_bound(self):
        demo=Demo();clock=Clock();records=[]
        with self.assertRaises(AssertionError) as caught:self.run_wait(demo,clock,records)
        self.assertGreaterEqual(clock.now,14)
        self.assertLess(clock.now,14.08)
        self.assertEqual(records,[])
        self.assertEqual(caught.exception.args[0]['observed'][-1]['west']['spatial'],'unavailable')

if __name__=='__main__':unittest.main()
