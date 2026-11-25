import copy
import importlib.util
import json
from pathlib import Path
import unittest

from sim import go2_cloud_shadow as shadow
from sim.test_go2_cloud_shadow import Clock, capture, rows, completion, MS, STAMP

spec=importlib.util.spec_from_file_location('shadow_verify',Path(__file__).with_name('go2_cloud_shadow_verify.py'))
verify_module=importlib.util.module_from_spec(spec);spec.loader.exec_module(verify_module)


def report(values=None,clock=None,complete=True):
    values=rows() if values is None else values
    c,clock=capture(clock)
    raw=[shadow.encoded(r) for r in values]
    for line in raw:c.ingest_line(line)
    if complete:
        end=shadow.encoded(completion(values));raw.append(end);c.ingest_line(end)
    return c.finish(0,True,{'reaped':True,'actions':[],'exit_code':0},stdout_bytes=sum(map(len,raw)),
                    eof_observed_ns=clock.now,exit_observed_ns=clock.now,stderr_observed_bytes=0)


def verify(value):return verify_module.verify(shadow.encoded(value))


class VerifierTests(unittest.TestCase):
    def test_complete_original_capture(self):
        r=report();s=verify(r)
        self.assertTrue(s['integrity_valid']);self.assertTrue(s['capture_complete'])
        self.assertEqual(s['retained_candidates'],1);self.assertEqual(s['clouds'],1)
        self.assertEqual(s['cloud_dispositions'][0]['disposition'],'retained')

    def test_complete_but_missing_companion(self):
        r=report([x for x in rows() if x['topic']!='/scan']);s=verify(r)
        self.assertTrue(s['capture_complete']);self.assertEqual(s['retained_candidates'],0)
        self.assertEqual(s['unavailable_clouds'],1)

    def test_failure_capture_integrity_is_not_success(self):
        r=report(complete=False);s=verify(r)
        self.assertTrue(s['integrity_valid']);self.assertFalse(s['capture_complete'])
        self.assertEqual(s['capture_error'],'helper_missing_completion')

    def test_original_future_and_stale_refusals(self):
        for wall in [STAMP-1,STAMP+251*MS]:
            r=report(clock=Clock(wall));s=verify(r)
            self.assertEqual(s['retained_candidates'],0);self.assertEqual(s['refused_candidates'],1)
            self.assertTrue(s['capture_complete'])

    def test_refusal_after_slow_decode_preserved(self):
        from sim.go2_cloud_stream import Assembler
        clock=Clock()
        class Slow(Assembler):
            def ingest(self,r,now):
                events=super().ingest(r,now)
                if r['topic']=='/utlidar/cloud':clock.advance(100*MS)
                return events
        c,_=capture(clock,assembler_factory=Slow)
        for r in rows():c.ingest_line(shadow.encoded(r))
        c.ingest_line(shadow.encoded(completion(rows())))
        r=c.finish(0,True,{'reaped':True,'actions':[],'exit_code':0},eof_observed_ns=clock.now,exit_observed_ns=clock.now,
                   stdout_bytes=sum(len(shadow.encoded(x)) for x in rows()+[completion(rows())]))
        s=verify(r);self.assertEqual(s['refused_candidates'],1)
        self.assertEqual(s['cloud_dispositions'][0]['reason'],'postdecode_companion_deadline')

    def test_startup_failure_without_clouds_is_valid_partial_evidence(self):
        c,clock=capture(startup_ms=100)
        for r in rows()[:2]:c.ingest_line(shadow.encoded(r))
        clock.advance(100*MS);c.tick()
        result=c.finish(-15,False,{'reaped':True,'actions':['terminate'],'exit_code':-15},
                        stdout_bytes=sum(len(shadow.encoded(x)) for x in rows()[:2]))
        summary=verify(result)
        self.assertTrue(summary['integrity_valid']);self.assertFalse(summary['capture_complete'])
        self.assertEqual(summary['clouds'],0);self.assertEqual(summary['capture_error'],'startup_deadline')

    def test_tampering_raw_metadata_age_deadline_and_counts_refuses(self):
        changes=[lambda r:r['messages'][0]['row'].update(payload_hex='00010000'),
                 lambda r:r['messages'][0].update(serialized_sha256='0'*64),
                 lambda r:r['candidates'][0]['sample_references']['cloud'].update(message_index=0),
                 lambda r:r['candidates'][0]['local_wall_ages_ns'].update(odom=0),
                 lambda r:r['candidates'][0].update(companion_deadline_ns=999999999),
                 lambda r:r.update(retained_candidates=2),lambda r:r['helper_completion']['row'].update(messages=1),
                 lambda r:r.update(operational_authority=True),lambda r:r.update(run_deadline_ns=999),
                 lambda r:r.update(helper_exit_observed_monotonic_ns=None),
                 lambda r:r.update(extra='unrecognized'),
                 lambda r:r['assembler_status']['counters'].update(ready=999),
                 lambda r:r.update(retained_candidates=True)]
        for change in changes:
            r=report();change(r)
            with self.subTest(change=change),self.assertRaises(ValueError):verify(r)

    def test_candidate_cannot_be_checked_after_reported_final_clock(self):
        r=report();candidate=r['candidates'][0]
        candidate['postdecode_clock']['monotonic_before_ns']=50*MS
        candidate['postdecode_clock']['monotonic_after_ns']=50*MS
        candidate['postdecode_clock']['wall_ns']+=50*MS
        candidate['local_wall_ages_ns']={k:100*MS for k in ['cloud','scan','odom']}
        with self.assertRaises(ValueError):verify(r)

    def test_no_promotion_of_missing_or_refused_candidate(self):
        r=report(clock=Clock(STAMP+251*MS));r['candidates'][0]['disposition']='retained';r['candidates'][0]['refusal_reason']=None
        r['retained_candidates']=1;r['refused_candidates']=0
        with self.assertRaises(ValueError):verify(r)
        r=report();r['candidates']=[];r['retained_candidates']=0
        with self.assertRaises(ValueError):verify(r)

    def test_decoder_failure_and_clock_fault_remain_partial(self):
        from sim.go2_cloud_stream import Assembler
        clock=Clock()
        class Jump(Assembler):
            def ingest(self,r,now):
                events=super().ingest(r,now)
                if r['topic']=='/utlidar/cloud':clock.wall+=51*MS
                return events
        c,_=capture(clock,assembler_factory=Jump)
        for r in rows():c.ingest_line(shadow.encoded(r))
        r=c.finish(-15,False,{'reaped':True,'actions':['terminate'],'exit_code':-15},
                   stdout_bytes=sum(len(shadow.encoded(x)) for x in rows()))
        s=verify(r);self.assertFalse(s['capture_complete']);self.assertEqual(s['retained_candidates'],0)
        self.assertTrue(s['gaps'])
        bad=rows();bad[-1]['payload_hex']='00010000';s=verify(report(bad))
        self.assertFalse(s['capture_complete'])

    def test_strict_json_numbers_duplicates_bounds(self):
        for raw in [b'{}',b'{"x":0,"x":1}',b'{"x":NaN}',b'{"x":1.0}',b'{"x":'+b'9'*21+b'}']:
            with self.assertRaises(ValueError):verify_module.verify(raw)


if __name__=='__main__':unittest.main()
