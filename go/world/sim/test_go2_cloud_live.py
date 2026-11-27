"""Finite live collector tests with original CDR and injected clocks/processes."""
import copy
import dataclasses
import json
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from sim.go2_bridge.protocol import Refused
from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_capture import convert
from sim.go2_cloud_live import LiveCloudSensors, _Clock, MAX_ORIGINALS, probe, main
from sim.test_go2_cloud_stream import (Feeder, assembler, STAMP, MS, epoch_wire,
    mount_wire, odom_wire, scan_wire, cloud_wire)


def profile(duration=60000):
    return {'observation_max_age_ms': 250, 'source_skew_max_ms': 50,
            'cloud_experiment': {'model': 'coherent-multiring-slab-v1',
                'scope': 'contained-simulation', 'collector_duration_ms': duration}}


class Clock:
    def __init__(self):
        self.wall, self.mono = STAMP+10*MS, 0
        self.bad = False
    def read(self):
        if self.bad:
            raise Refused('clock_continuity')
        return self.wall, self.mono
    def advance(self, ns):
        self.wall += ns; self.mono += ns


class Process:
    def __init__(self, *args, **kwargs):
        self.args, self.options = args, kwargs
        self.returncode = None
        self.stdout = self.stderr = None
        self.terminated = self.killed = 0
    def poll(self): return self.returncode
    def terminate(self): self.terminated += 1; self.returncode = -15
    def kill(self): self.killed += 1; self.returncode = -9
    def wait(self, timeout=None): return self.returncode


class Feed:
    def __init__(self, live):
        self.live = live; self.f = Feeder(assembler())
    def send(self, topic, raw, **kw):
        row = self.f.row(topic, raw, **kw)
        self.live._ingest_line((json.dumps(row)+'\n').encode())
    def base(self):
        self.send('/simulation/epoch', epoch_wire())
        self.send('/tf_static', mount_wire())
    def complete(self, stamp=STAMP):
        self.send('/scan', scan_wire(stamp)); self.send('/odom', odom_wire(stamp))
        self.send('/utlidar/cloud', cloud_wire(stamp))


class LiveTests(unittest.TestCase):
    def make(self, duration=60000, **kw):
        clock = Clock()
        live = LiveCloudSensors(SOURCE_DIGEST, 31, profile(duration), clock=clock,
                                process_factory=Process, start_thread=False, **kw)
        self.addCleanup(live.close)
        return live, clock, Feed(live)

    def test_explicit_profile_and_fixed_before_spawn_lifetime(self):
        for bad in ({}, {'cloud_experiment': {}}, profile(60001), profile(0)):
            with self.subTest(bad=bad), self.assertRaises(Refused):
                LiveCloudSensors(SOURCE_DIGEST,31,bad,process_factory=Process,start_thread=False)
        clock = Clock()
        def factory(*args, **kwargs):
            clock.advance(20*MS)
            return Process(*args, **kwargs)
        live = LiveCloudSensors(SOURCE_DIGEST,31,profile(100),clock=clock,
                                process_factory=factory,start_thread=False)
        self.addCleanup(live.close)
        self.assertEqual(live.deadline_ns,100*MS)
        self.assertEqual(live.process.args[0], ['/opt/world/world-go2-cloud-live','--duration-ms','100','--max-clouds','2048'])
        self.assertEqual(live.process.options['stdin'],subprocess.DEVNULL)

    def test_same_latest_capture_same_sequence_and_exact_cloud_payload(self):
        live,clock,f=self.make()
        with self.assertRaisesRegex(Refused,'sensors_not_ready'):live.snapshot()
        f.base();f.complete()
        a=live.snapshot();b=live.snapshot()
        self.assertEqual(a,b);self.assertEqual(a['capture_sequence'],1)
        payload=a['cloud'];self.assertEqual(set(payload), {'model','source_digest','frame','publisher_gid',
            'serialized_sha256','captured_unix_ns','ranges','sequence','observed_epoch','scan_sha256',
            'odom_sha256','epoch_sha256','mount_sha256','cloud_mount_sha256','epoch_publisher',
            'mount_publisher','cloud_mount_publisher'})
        original=live.original_capture(1)
        self.assertEqual(payload['scan_sha256'],original.scan.serialized_sha256)
        self.assertEqual(payload['odom_sha256'],original.odom.serialized_sha256)
        self.assertEqual(payload['captured_unix_ns'],STAMP)
        self.assertIsNone(payload['ranges'][1]);self.assertEqual(payload['ranges'][4320],1.)
        a['cloud']['ranges'][4320]=9
        self.assertEqual(live.snapshot()['cloud']['ranges'][4320],1.)
        self.assertIs(live.original_capture(1),original)

    def test_missing_candidate_clears_positive_then_later_original_can_recover(self):
        live,clock,f=self.make();f.base();f.complete()
        clock.advance(10*MS);f.send('/utlidar/cloud',cloud_wire(STAMP+10*MS))
        clock.advance(100*MS);live._tick()
        self.assertFalse(live.fault)
        with self.assertRaisesRegex(Refused,'sensor_cloud_unavailable'):live.snapshot()
        f.complete(STAMP+110*MS)
        self.assertEqual(live.snapshot()['capture_sequence'],2)
        self.assertEqual(live.snapshot()['cloud']['captured_unix_ns'],STAMP+110*MS)

    def test_post_conversion_exclusive_companion_deadline_and_no_renewal(self):
        live,clock,f=self.make();f.base()
        def delayed(c, **kw):
            result=convert(c,**kw);clock.advance(100*MS);return result
        with patch('sim.go2_cloud_live.convert',side_effect=delayed):f.complete()
        self.assertEqual(live.sequence,0);self.assertIsNone(live.latest)
        self.assertEqual(live.unavailable_reason,'sensor_cloud_companion_deadline')
        self.assertFalse(live.fault)
        f.complete(STAMP+100*MS)
        self.assertEqual(live.snapshot()['capture_sequence'],1)

    def test_post_copy_original_age_is_strict_and_source_time_never_changes(self):
        live,clock,f=self.make();f.base();f.complete()
        clock.advance(240*MS)
        with self.assertRaisesRegex(Refused,'sensor_cloud_stale'):live.snapshot()
        self.assertIsNone(live.latest)
        self.assertEqual(live.original_capture(1).cloud.value.capture_unix_ns,STAMP)
        self.assertEqual(live.sequence,1)

    def test_future_original_and_configured_pair_skew_unavailable(self):
        live,clock,f=self.make();f.base();f.complete(STAMP+11*MS)
        self.assertEqual(live.unavailable_reason,'sensor_cloud_stale')
        self.assertEqual(live.sequence,0)
        live.close()
        p=profile();p['source_skew_max_ms']=1
        clock=Clock();clock.wall=STAMP+30*MS
        live=LiveCloudSensors(SOURCE_DIGEST,31,p,clock=clock,process_factory=Process,start_thread=False)
        self.addCleanup(live.close);f=Feed(live);f.base()
        f.send('/scan',scan_wire());f.send('/odom',odom_wire(STAMP+2*MS));f.send('/utlidar/cloud',cloud_wire())
        self.assertEqual(live.unavailable_reason,'sensor_cloud_pair_skew')

    def test_identity_epoch_and_input_failures_latch_and_cannot_recover(self):
        for mode in ('publisher','epoch','malformed','complete'):
            with self.subTest(mode=mode):
                live,clock,f=self.make();f.base();f.complete()
                if mode=='publisher':f.send('/odom',odom_wire(STAMP+MS),publisher_gid='34'*24)
                elif mode=='epoch':f.send('/simulation/epoch',epoch_wire(32))
                elif mode=='malformed':live._ingest_line(b'{oops}\n')
                else:
                    live._ingest_line(json.dumps({'kind':'complete','reason':'duration','messages':live.messages,
                        'clouds':live.clouds,'serialized_bytes':live.raw_bytes}).encode())
                fault=live.fault;self.assertTrue(fault);self.assertIsNone(live.latest)
                f.complete(STAMP+2*MS)
                self.assertEqual(live.fault,fault);self.assertEqual(live.sequence,1)
                with self.assertRaises(Refused):live.snapshot()
                self.assertEqual(live.rejected_sample()['reason'],fault)

    def test_idle_startup_run_and_clock_faults_latch(self):
        live,clock,f=self.make();clock.advance(3000*MS);live._tick()
        self.assertEqual(live.fault,'sensor_cloud_startup_deadline')
        live,clock,f=self.make(1000);f.base();f.complete();clock.advance(1000*MS);live._tick()
        self.assertEqual(live.fault,'sensor_cloud_run_deadline')
        live,clock,f=self.make();f.base();f.complete();clock.bad=True
        with self.assertRaisesRegex(Refused,'sensor_cloud_clock'):live.snapshot()
        clock.bad=False;f.complete(STAMP+MS)
        self.assertEqual(live.fault,'sensor_cloud_clock');self.assertIsNone(live.latest)

    def test_bounded_originals_and_diagnostics_detached(self):
        live,clock,f=self.make();f.base()
        for i in range(12):
            f.complete(STAMP+i*MS);clock.advance(MS)
        self.assertEqual(len(live.originals),MAX_ORIGINALS)
        self.assertIsNone(live.original_capture(1));self.assertIsNotNone(live.original_capture(12))
        d=live.diagnostics();d['assembler']['publisher_gids'].clear();d['retained_sequences'].clear()
        self.assertEqual(len(live.diagnostics()['retained_sequences']),8)
        self.assertTrue(live.diagnostics()['assembler']['publisher_gids'])
        self.assertNotIn('messages',live.__dict__.get('originals',{}))

    def test_parent_raw_message_cloud_and_native_time_quotas(self):
        for attr,value in (('raw_bytes',256<<20),('messages',32768),('clouds',2048)):
            live,clock,f=self.make();f.base();setattr(live,attr,value)
            f.complete();self.assertEqual(live.fault,'sensor_cloud_quota')
        live,clock,f=self.make();f.send('/simulation/epoch',epoch_wire(),helper_elapsed_ns=60_000*MS)
        self.assertEqual(live.fault,'sensor_cloud_helper_clock')

    def test_close_reaps_once_and_never_marks_deliberate_close_as_fault(self):
        live,clock,f=self.make();f.base();f.complete();live.close();live.close()
        self.assertFalse(live.fault);self.assertTrue(live.closed);self.assertIsNone(live.latest)
        self.assertEqual(live.process.terminated,1);self.assertTrue(live.cleanup['reaped'])
        f.complete(STAMP+MS);self.assertEqual(live.sequence,1)
        with self.assertRaisesRegex(Refused,'sensor_cloud_closed'):live.snapshot()

    def test_default_clock_five_millisecond_continuity_latches(self):
        t=Clock();clock=_Clock(wall=lambda:t.wall,mono=lambda:t.mono)
        clock.read();t.wall+=5*MS+1
        with self.assertRaisesRegex(Refused,'clock_continuity'):clock.read()
        t.wall-=5*MS+1
        with self.assertRaisesRegex(Refused,'clock_fault_latched'):clock.read()

    def test_actual_pipe_worker_eof_and_stderr_quota_reap_owned_children(self):
        for mode in ('eof','stderr'):
            clock=Clock()
            source='pass' if mode=='eof' else 'import os,time;os.write(2,b"x"*70000);time.sleep(10)'
            def factory(*args,**kwargs):
                return subprocess.Popen([sys.executable,'-c',source],**kwargs)
            live=LiveCloudSensors(SOURCE_DIGEST,31,profile(),clock=clock,process_factory=factory)
            self.addCleanup(live.close)
            end=time.monotonic()+2
            while not live.fault and time.monotonic()<end:time.sleep(.005)
            self.assertTrue(live.fault)
            live.close();self.assertIsNotNone(live.process.poll());self.assertTrue(live.cleanup['reaped'])
            if mode=='stderr':
                self.assertEqual(live.fault,'sensor_cloud_stderr_quota');self.assertEqual(len(live.stderr),65536)

    def test_passive_probe_retains_failures_original_admission_and_cleanup(self):
        live,clock,f=self.make();calls=[]
        def wait(_):
            calls.append(1)
            if len(calls)==1:return
            if len(calls)==2:f.base()
            clock.advance(MS);f.complete(STAMP+(len(calls)-2)*MS)
        with patch('sim.go2_cloud_live.LiveCloudSensors',return_value=live), \
             patch('sim.go2_cloud_live.time.monotonic_ns',side_effect=lambda:clock.mono), \
             patch('sim.go2_cloud_live.time.sleep',side_effect=wait):
            result=probe(SOURCE_DIGEST,31,profile(),2)
        self.assertTrue(result['complete']);self.assertEqual(len(result['samples']),2)
        self.assertEqual(result['snapshot_failures'],2)
        self.assertEqual(result['failure_counts'],{'sensors_not_ready':2})
        self.assertTrue(result['after_close']['cleanup']['reaped'])
        for item in result['samples']:
            self.assertLess(item['admission_monotonic_ns'],item['companion_deadline_ns'])
            self.assertEqual(item['samples']['cloud']['serialized_sha256'],
                             live.original_capture(item['sequence']).cloud.serialized_sha256)

    def test_cli_writes_new_private_bounded_report_and_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory).resolve();(path/'profile.json').write_text(json.dumps(profile()))
            args=['--source-digest',SOURCE_DIGEST,'--expected-epoch','31','--profile',str(path/'profile.json'),
                  '--samples','1','--out',str(path/'report.json')]
            report={'schema':'test','complete':True,'samples':[{}],'snapshot_failures':0,'error':None}
            with patch('sim.go2_cloud_live.probe',return_value=report) as observe:
                self.assertEqual(main(args),0)
                self.assertEqual((path/'report.json').stat().st_mode&0o777,0o600)
                self.assertIn('profile_sha256',json.loads((path/'report.json').read_bytes()))
                with self.assertRaises(ValueError):main(args)
                self.assertEqual(observe.call_count,1)


if __name__ == '__main__':
    unittest.main()
