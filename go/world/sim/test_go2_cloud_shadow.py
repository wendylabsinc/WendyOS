"""Passive wrapper tests use original CDR, fake clocks and local fake helpers."""
import copy
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

from sim import go2_cloud_shadow as shadow
from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_stream import Assembler
from sim.test_go2_cloud_stream import (STAMP, MS, epoch_wire, mount_wire, odom_wire,
                                     scan_wire, cloud_wire)


class Clock:
    def __init__(self,wall=STAMP+50*MS):self.now=0;self.wall=wall
    def mono(self):return self.now
    def utc(self):return self.wall
    def advance(self,n):self.now+=n;self.wall+=n


def row(topic,raw,index):
    return {'kind':'sample','topic':topic,'publisher_gid':'12'*24,
            'source_timestamp_ns':STAMP+index+1,'received_timestamp_ns':0,
            'helper_elapsed_ns':index,'payload_hex':raw.hex()}


def rows(stamp=STAMP):
    return [row(t,b,i) for i,(t,b) in enumerate([
        ('/simulation/epoch',epoch_wire()),('/tf_static',mount_wire()),
        ('/scan',scan_wire(stamp)),('/odom',odom_wire(stamp)),
        ('/utlidar/cloud',cloud_wire(stamp))])]


def completion(values):
    return {'kind':'complete','reason':'max_clouds','messages':len(values),
            'clouds':sum(r['topic']=='/utlidar/cloud' for r in values),
            'serialized_bytes':sum(len(r['payload_hex'])//2 for r in values)}


def capture(clock=None,**kwargs):
    c=clock or Clock()
    return shadow.ShadowCapture(SOURCE_DIGEST,31,1000,1,monotonic_ns=c.mono,wall_ns=c.utc,**kwargs),c


def feed(c,values=None,finish=True):
    values=rows() if values is None else values
    for r in values:c.ingest_line(shadow.encoded(r))
    if finish:c.ingest_line(shadow.encoded(completion(values)))
    return c.finish(0,True,{'reaped':True,'actions':[],'exit_code':0})


class CaptureTests(unittest.TestCase):
    def test_original_refs_clocks_and_no_authority(self):
        c,clock=capture();report=feed(c)
        self.assertTrue(report['capture_complete']);self.assertEqual(report['retained_candidates'],1)
        self.assertFalse(report['operational_authority']);self.assertEqual(report['authorization_status'],'not_evaluated')
        candidate=report['candidates'][0]
        self.assertEqual(candidate['original_capture_stamps_ns'],dict(cloud=STAMP,scan=STAMP,odom=STAMP))
        self.assertEqual(candidate['local_wall_ages_ns'],dict(cloud=50*MS,scan=50*MS,odom=50*MS))
        for key,index in [('epoch_sample',0),('scan',2),('odom',3),('cloud',4)]:
            ref=candidate['sample_references'][key];self.assertEqual(ref['message_index'],index)
            self.assertEqual(ref['serialized_sha256'],report['messages'][index]['serialized_sha256'])
        self.assertEqual(report['messages'][4]['row'],rows()[4])
        self.assertEqual(report['commands_published'],0);self.assertEqual(report['runtime_http_calls'],0)

    def test_report_preserves_explicit_companion_timeout(self):
        c,_=capture(companion_ms=23);report=feed(c)
        self.assertEqual(report['companion_timeout_ns'],23*MS)
        candidate=report['candidates'][0]
        original=report['messages'][candidate['sample_references']['cloud']['message_index']]
        self.assertEqual(candidate['companion_deadline_ns'],original['arrival_clock']['monotonic_after_ns']+23*MS)

    def test_slow_decoder_cannot_emit_after_companion_deadline(self):
        clock=Clock()
        class Slow(Assembler):
            def ingest(self,r,now):
                result=super().ingest(r,now)
                if r['topic']=='/utlidar/cloud':clock.advance(100*MS)
                return result
        c,_=capture(clock,assembler_factory=Slow)
        report=feed(c)
        self.assertEqual(report['retained_candidates'],0)
        self.assertEqual(report['candidates'][0]['refusal_reason'],'postdecode_companion_deadline')
        self.assertEqual(report['candidates'][0]['original_capture_stamps_ns']['cloud'],STAMP)

    def test_first_rejected_candidate_does_not_complete_startup(self):
        c,clock=capture(startup_ms=100)
        for r in rows(STAMP-300*MS):c.ingest_line(shadow.encoded(r))
        self.assertEqual(c.retained,0);clock.advance(100*MS);c.tick()
        self.assertEqual(c.error,'startup_deadline')

    def test_slow_decode_startup_or_run_equality_refuses(self):
        for delay,expected in [(100*MS,'startup_deadline'),(1000*MS,'run_deadline')]:
            clock=Clock()
            class Slow(Assembler):
                def ingest(self,r,now):
                    result=super().ingest(r,now)
                    if r['topic']=='/utlidar/cloud':clock.advance(delay)
                    return result
            c,_=capture(clock,assembler_factory=Slow,startup_ms=100)
            report=feed(c)
            self.assertEqual(report['error'],expected);self.assertEqual(report['retained_candidates'],0)
            self.assertFalse(report['capture_complete'])

    def test_local_age_inclusive_future_and_stale_keep_originals(self):
        for age,reason in [(250*MS,None),(250*MS+1,'original_age_exceeds_local_limit'),(-1,'original_stamp_future_local_wall')]:
            c,_=capture(Clock(STAMP+age));r=feed(c)
            self.assertEqual(r['candidates'][0]['refusal_reason'],reason)
            self.assertEqual(r['candidates'][0]['local_wall_ages_ns']['odom'],age)
            self.assertEqual(r['candidates'][0]['original_capture_stamps_ns']['odom'],STAMP)

    def test_wall_regression_divergence_and_monotonic_latch(self):
        for field,delta,reason in [('wall',-1,'wall_regression'),('wall',51*MS,'wall_monotonic_divergence'),('now',-1,'helper_integer')]:
            clock=Clock();c,_=capture(clock);setattr(clock,field,getattr(clock,field)+delta);c.tick()
            self.assertEqual(c.error,reason)
            for r in rows():c.ingest_line(shadow.encoded(r))
            self.assertEqual(c.messages,[])

    def test_failed_fresh_tick_has_no_old_clock_timestamp(self):
        c,clock=capture();clock.advance(10)
        fresh=c.tick();self.assertEqual(fresh['monotonic_after_ns'],10)
        clock.wall-=1
        self.assertIsNone(c.tick());self.assertEqual(c.error,'wall_regression')
        self.assertEqual(c.last_clock['monotonic_after_ns'],10)

    def test_postdecode_wall_fault_suppresses_candidate(self):
        clock=Clock()
        class Jump(Assembler):
            def ingest(self,r,now):
                result=super().ingest(r,now)
                if r['topic']=='/utlidar/cloud':clock.wall+=51*MS
                return result
        c,_=capture(clock,assembler_factory=Jump);report=feed(c)
        self.assertEqual(report['error'],'wall_monotonic_divergence');self.assertEqual(report['retained_candidates'],0)
        self.assertEqual(len(report['messages']),5)

    def test_sample_at_run_cutoff_not_admitted(self):
        c,clock=capture();clock.advance(1000*MS);raw=shadow.encoded(rows()[0]);c.ingest_line(raw)
        self.assertEqual(c.messages,[]);self.assertEqual(c.error,'run_deadline')
        self.assertEqual(bytes.fromhex(c.terminal_input['prefix_hex']),raw)

    def test_completion_eof_exit_and_counts_separate(self):
        for mode,reason in [('missing','helper_missing_completion'),('eof','helper_missing_eof'),('exit','helper_nonzero_exit'),('count','completion_counts'),('after','sample_after_completion')]:
            c,_=capture()
            for r in rows():c.ingest_line(shadow.encoded(r))
            if mode!='missing':
                done=completion(rows())
                if mode=='count':done['messages']+=1
                c.ingest_line(shadow.encoded(done))
            if mode=='after':c.ingest_line(shadow.encoded(rows()[0]))
            report=c.finish(1 if mode=='exit' else 0,mode!='eof',{'reaped':True})
            self.assertEqual(report['error'],reason);self.assertFalse(report['capture_complete'])

    def test_missing_companions_remain_unavailable(self):
        values=[r for r in rows() if r['topic']!='/scan'];c,_=capture();report=feed(c,values)
        self.assertTrue(report['capture_complete']);self.assertEqual(report['retained_candidates'],0)
        self.assertTrue(any(e['kind']=='unavailable' for e in report['events']))

    def test_malformed_bounded_input_and_resource_limits(self):
        for raw in [b'{',b'{"x":1,"x":2}',b'{"x":NaN}',b'{"x":'+b'9'*21+b'}',b' '* (shadow.MAX_ROW+1)]:
            c,_=capture();c.ingest_line(raw)
            self.assertIsNotNone(c.error);self.assertEqual(c.messages,[])
            self.assertLessEqual(len(c.terminal_input['prefix_hex']),2*shadow.MAX_ROW)
        for limit,value in [('MAX_MESSAGES',2),('MAX_RAW',4)]:
            with mock.patch.object(shadow,limit,value):
                c,_=capture();report=feed(c)
                self.assertEqual(report['error'],'capture_resource_limit');self.assertFalse(report['capture_complete'])

    def test_native_elapsed_and_finished_capture_are_terminal(self):
        c,_=capture();r=rows()[0];r['helper_elapsed_ns']=1000*MS
        c.ingest_line(shadow.encoded(r));self.assertEqual(c.error,'helper_after_native_deadline')
        c,clock=capture();report=feed(c);snapshot=copy.deepcopy(report)
        clock.wall-=1;c.tick();c.ingest_line(b'{')
        self.assertEqual(c.error,None);self.assertEqual(len(c.messages),5)
        report['messages'].clear();self.assertEqual(len(c.messages),5)
        self.assertEqual(snapshot['retained_candidates'],1)

    def test_reordered_identity_and_input_mutation(self):
        c,_=capture();values=rows();values[4]['publisher_gid']='34'*24
        # Distinct topics may have distinct GIDs; a same-topic change cannot.
        for r in values:c.ingest_line(shadow.encoded(r))
        original=copy.deepcopy(c.messages[4]);values[4]['payload_hex']='00'
        self.assertEqual(c.messages[4],original)
        changed=row('/utlidar/cloud',cloud_wire(STAMP+1),6);changed['publisher_gid']='56'*24
        c.ingest_line(shadow.encoded(changed));self.assertEqual(c.error,'cloud_limit')
        c,_=capture();c.ingest_line(shadow.encoded(rows()[0]));changed=row('/simulation/epoch',epoch_wire(),8);changed['publisher_gid']='34'*24
        c.ingest_line(shadow.encoded(changed));self.assertEqual(c.error,'publisher_changed')


class ProcessTests(unittest.TestCase):
    def run_helper(self,script,duration=300,max_clouds=1,startup=200):
        real=subprocess.Popen;children=[]
        def launch(command,**kwargs):
            self.assertEqual(command[0],shadow.HELPER)
            child=real([sys.executable,'-c',script],**kwargs);children.append(child);return child
        with mock.patch.object(shadow.subprocess,'Popen',side_effect=launch):
            report=shadow.observe(SOURCE_DIGEST,31,duration,max_clouds,startup_ms=startup)
        self.assertTrue(children);self.assertIsNotNone(children[0].poll())
        self.assertTrue(children[0].stdout.closed);self.assertTrue(children[0].stderr.closed)
        return report

    def test_successful_local_helper_no_ros(self):
        # Exact timestamps intentionally old here: subprocess lifecycle can be
        # complete while local-age candidate retention is refused.
        data=b''.join(shadow.encoded(r) for r in rows()+[completion(rows())])
        r=self.run_helper('import sys;sys.stdout.buffer.write('+repr(data)+');sys.stdout.flush()')
        self.assertTrue(r['capture_complete']);self.assertEqual(r['retained_candidates'],0)
        self.assertTrue(r['helper_eof']);self.assertEqual(r['helper_exit_code'],0)

    def test_fresh_original_child_timestamps_are_retained(self):
        values=rows(time.time_ns());data=b''.join(shadow.encoded(r) for r in values+[completion(values)])
        result=self.run_helper('import sys;sys.stdout.buffer.write('+repr(data)+');sys.stdout.flush()',duration=1000,startup=500)
        self.assertTrue(result['capture_complete']);self.assertEqual(result['retained_candidates'],1)
        self.assertTrue(result['helper_exit_observed_before_cleanup'])
        self.assertLess(result['helper_exit_observed_monotonic_ns'],result['run_deadline_ns'])
        self.assertLess(result['helper_eof_observed_monotonic_ns'],result['run_deadline_ns'])

    def test_stdout_total_is_bounded_independently_of_row_size(self):
        with mock.patch.object(shadow,'MAX_STDOUT',1024):
            result=self.run_helper('import sys;sys.stdout.write("x"*4096);sys.stdout.flush()')
        self.assertEqual(result['error'],'stdout_limit')
        self.assertIsNotNone(result['terminal_input']);self.assertFalse(result['capture_complete'])

    def test_partial_oversized_stderr_and_nonzero_helpers(self):
        for script,expected in [('import sys;sys.stdout.write("{");sys.stdout.flush()','helper_partial_row'),
                                ('import sys;sys.stdout.write("x"*270000);sys.stdout.flush()','helper_row_bound'),
                                ('import sys;sys.stderr.write("x"*70000);sys.stderr.flush()','stderr_limit'),
                                ('import sys;sys.exit(4)','helper_missing_completion')]:
            r=self.run_helper(script);self.assertEqual(r['error'],expected);self.assertFalse(r['capture_complete'])
            self.assertLessEqual(len(r['stderr_hex']),2*shadow.MAX_STDERR)
            self.assertLessEqual(len(r['partial_stdout_hex']),2*shadow.MAX_ROW)

    def test_hanging_helper_is_killed_and_reaped(self):
        started=time.monotonic()
        r=self.run_helper('import signal,time;signal.signal(signal.SIGTERM,signal.SIG_IGN);time.sleep(20)',duration=200,startup=100)
        self.assertEqual(r['error'],'startup_deadline');self.assertTrue(r['cleanup']['reaped'])
        self.assertIn('kill',r['cleanup']['actions']);self.assertLess(time.monotonic()-started,2)

    def test_eof_clock_fault_does_not_reuse_prior_timestamp(self):
        original=shadow.ShadowCapture;real_read=os.read;real_launch=subprocess.Popen
        children=[];stdout_fds=set()
        class FailingClock(original):
            break_clock=False
            def _clock(self):
                if self.break_clock:raise shadow.ShadowError('wall_regression')
                return super()._clock()
        def launch(command,**kwargs):
            child=real_launch([sys.executable,'-c','pass'],**kwargs)
            children.append(child);stdout_fds.add(child.stdout.fileno());return child
        def read(fd,size):
            value=real_read(fd,size)
            if fd in stdout_fds and value==b'':FailingClock.break_clock=True
            return value
        with mock.patch.object(shadow,'ShadowCapture',FailingClock), mock.patch.object(shadow.subprocess,'Popen',side_effect=launch), mock.patch.object(shadow.os,'read',side_effect=read):
            result=shadow.observe(SOURCE_DIGEST,31,1000,1,startup_ms=500)
        self.assertIsNotNone(children[0].poll())
        self.assertEqual(result['error'],'wall_regression')
        self.assertTrue(result['helper_eof'])
        self.assertIsNone(result['helper_eof_observed_monotonic_ns'])
        self.assertIsNone(result['helper_exit_observed_monotonic_ns'])

    def test_output_path_cannot_replace_or_follow_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp)/'existing';p.write_text('keep');link=Path(tmp)/'alias';link.symlink_to(p)
            for target in [p,link]:
                with self.assertRaises(ValueError):shadow.output_path(target)
            self.assertEqual(p.read_text(),'keep')


if __name__=='__main__':unittest.main()
