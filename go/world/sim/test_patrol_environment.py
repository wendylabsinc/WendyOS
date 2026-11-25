import unittest
from sim.patrol_environment import EnvironmentMeter

class EnvironmentTests(unittest.TestCase):
    def meter(self):return EnvironmentMeter('run','a'*64,'boot')
    def test_original_cadence_and_nonfreshening_reads(self):
        meter=self.meter();calls=[]
        def measure(seq):calls.append(seq);return 10.,1.5
        for i in range(1,1001):meter.step(i*.002,measure,1000+i*2)
        self.assertEqual(calls,list(range(1,21)))
        samples=meter.snapshot()['samples'];self.assertEqual(samples[-1]['simulation_ns'],2_000_000_000)
        self.assertEqual(samples[-1]['captured_unix_ms'],3000)
        self.assertEqual(meter.snapshot()['samples'],samples)
        samples[-1]['value_ppm']=999;self.assertEqual(meter.snapshot()['samples'][-1]['value_ppm'],10)
    def test_between_poll_spike_retained_and_history_bounded(self):
        meter=self.meter()
        for i in range(1,101):meter.step(i*.1,lambda seq:(100. if seq==99 else 10.,1.5),1000+i*100)
        samples=meter.snapshot()['samples'];self.assertEqual(len(samples),64)
        self.assertEqual(samples[-2]['value_ppm'],100);self.assertEqual(samples[-1]['value_ppm'],10)
        self.assertEqual(samples[0]['sequence'],37)
    def test_original_clock_reset_and_unknown_measurement_are_invalid(self):
        meter=self.meter();meter.step(.1,lambda seq:(10.,1.5),1100);meter.step(.1,lambda seq:(10.,1.5),1200)
        self.assertFalse(meter.snapshot()['samples'][-1]['valid'])
        meter.step(.2,lambda seq:(10.,1.5),1300);self.assertEqual(meter.sequence,1)
        meter=self.meter();meter.step(.1,lambda seq:(float('nan'),1.5),1100)
        self.assertFalse(meter.snapshot()['samples'][-1]['valid'])

if __name__=='__main__':unittest.main()
