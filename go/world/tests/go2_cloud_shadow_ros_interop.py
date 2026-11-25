#!/usr/bin/env python3
"""Bounded synthetic ROS tests of the separate passive shadow executable.

Run inside an isolated, network-none Linux ROS container. No runtime, HTTP,
command topic or arming action is used. Callback metadata is not authentication.
"""
from __future__ import annotations
import argparse
from array import array
import hashlib
import json
import math
import os
from pathlib import Path
import re
import selectors
import signal
import struct
import subprocess
import sys
import time
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from go2_cloud_ros_interop import SPARSE, xyz_bytes, check_cloud, Refused, require

ROW_LIMIT=256<<10
STDOUT_LIMIT=40<<20
RAW_LIMIT=16<<20
MESSAGE_LIMIT=8192


class Lines:
    def __init__(self):self.pending=bytearray();self.total=0
    def feed(self,chunk):
        self.total+=len(chunk);require(self.total<=STDOUT_LIMIT,'stdout_bound')
        self.pending.extend(chunk);rows=[]
        while b'\n' in self.pending:
            end=self.pending.index(10);require(end+1<=ROW_LIMIT,'row_bound')
            line=bytes(self.pending[:end]);del self.pending[:end+1]
            try:row=json.loads(line)
            except (ValueError,UnicodeError):raise Refused('helper_json') from None
            require(type(row) is dict,'helper_object');rows.append(row)
        require(len(self.pending)<ROW_LIMIT,'row_bound')
        return rows


def arguments(helper,observer):
    result={'schema':'wendy.world.cloud-shadow-arguments.v1','success':False,'cases':[]}
    for name,binary,duration,clouds in [
      ('shadow_duration_low',helper,'99','1'),('shadow_duration_high',helper,'60001','1'),
      ('shadow_cloud_low',helper,'100','0'),('shadow_cloud_high',helper,'100','129'),
      ('shadow_bad_number',helper,'1e3','8'),('observer_cloud_unchanged',observer,'100','6'),
      ('observer_duration_unchanged',observer,'15001','1')]:
        r=subprocess.run([str(binary),'--duration-ms',duration,'--max-clouds',clouds],capture_output=True,timeout=2)
        result['cases'].append({'case':name,'exit_code':r.returncode,'stdout_bytes':len(r.stdout),'stderr':r.stderr.decode('ascii')})
        require(r.returncode==1 and r.stdout==b'' and r.stderr==b'passive_cloud_helper_unavailable\n','argument_bound')
    result['success']=True;return result


def run(helper,case):
    started=time.monotonic();process=node=ros=None;selector=selectors.DefaultSelector()
    maximum=8 if case in ('delayed-scan','missing-scan') else 128
    duration=1800 if case=='duration' else 12000 if case=='raw-limit' else 60000
    result={'schema':'wendy.world.go2-cloud-shadow-interop.v1','case':case,'success':False,
      'simulation':True,'operational_authority':False,'runtime_calls':0,'commands_published':0,
      'identity_scope':'isolated_sole_synthetic_publisher_exact_payload_and_stable_callback_gid',
      'identity_limit':'Callback GID is not graph endpoint ID or authenticated runtime identity; received_timestamp_ns=0 is unavailable.',
      'helper_sha256':hashlib.sha256(helper.read_bytes()).hexdigest(),'samples':[],'scans':[],
      'extra_clouds_during_drain':0,'complete':None,'configured_duration_ms':duration,'configured_max_clouds':maximum}
    def emergency(_s,_f):
        if process is not None:
            try:process.kill();process.wait(timeout=.2)
            except (ProcessLookupError,subprocess.TimeoutExpired):pass
        os.write(2,b'shadow_interop_absolute_deadline\n');os._exit(124)
    previous=signal.signal(signal.SIGALRM,emergency);signal.setitimer(signal.ITIMER_REAL,16)
    try:
        import rclpy
        from rclpy.node import Node
        from rclpy.qos import QoSProfile,ReliabilityPolicy,DurabilityPolicy
        from rclpy.serialization import deserialize_message
        from sensor_msgs.msg import PointCloud2,PointField,LaserScan
        ros=rclpy;rclpy.init(args=[]);node=Node('world_go2_shadow_synthetic_test')
        qos=QoSProfile(depth=1,reliability=ReliabilityPolicy.BEST_EFFORT,durability=DurabilityPolicy.VOLATILE)
        publisher=node.create_publisher(PointCloud2,'/utlidar/cloud',qos)
        with_scans=case in ('delayed-scan','missing-scan','raw-limit')
        scans=node.create_publisher(LaserScan,'/scan',qos) if with_scans else None
        variants={};messages={};scan_messages={}
        for i in range(maximum):
            stamp=123000000456+i*100000000
            indices=tuple(range(9000)) if i==0 or case=='raw-limit' else SPARSE
            raw=xyz_bytes(indices);variants[stamp]=('full' if len(indices)==9000 else 'sparse',indices,raw)
            m=PointCloud2();m.header.frame_id='utlidar_lidar';m.header.stamp.sec,m.header.stamp.nanosec=divmod(stamp,10**9)
            m.height=1;m.width=len(indices);m.point_step=12;m.row_step=len(raw);m.is_bigendian=False;m.is_dense=True
            m.fields=[PointField(name=n,offset=j*4,datatype=PointField.FLOAT32,count=1) for j,n in enumerate(('x','y','z'))]
            m.data=array('B',raw);messages[stamp]=m
            if with_scans:
                s=LaserScan();s.header.frame_id='lidar_link';s.header.stamp=m.header.stamp
                s.angle_min=-math.pi;s.angle_increment=2*math.pi/360;s.angle_max=math.pi-2*math.pi/360;s.range_min=.1;s.range_max=12.
                s.ranges=[1+(j%11)/10 if j in indices else math.inf for j in range(4320,4680)]
                if case=='raw-limit':s.intensities=[.25]*(8192-360)
                scan_messages[stamp]=s
        command=[str(helper),'--duration-ms',str(duration),'--max-clouds',str(maximum)];result['command']=command
        launch=time.monotonic();process=subprocess.Popen(command,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,bufsize=0,close_fds=True)
        for stream,label in ((process.stdout,'stdout'),(process.stderr,'stderr')):
            os.set_blocking(stream.fileno(),False);selector.register(stream,selectors.EVENT_READ,label)
        lines=Lines();stderr=bytearray();cloud_gid=scan_gid=None;raw_total=0;message_count=0
        stamps=list(variants);next_index=0;waiting_cloud=waiting_scan=None;pending_scan=None
        final_observed=None;first_publish=None;last_publish=last_extra=0.;last_elapsed=-1
        while time.monotonic()-started<15:
            now=time.monotonic()
            if process.poll() is None and result['complete'] is None:
                if (waiting_cloud is None and waiting_scan is None and pending_scan is None and next_index<maximum
                    and now-last_publish >= (.03 if case=='duration' else .005)
                    and publisher.get_subscription_count()>0 and (scans is None or scans.get_subscription_count()>0)):
                    for topic in ('/utlidar/cloud','/scan') if scans else ('/utlidar/cloud',):
                        endpoints=node.get_publishers_info_by_topic(topic)
                        require(len(endpoints)==1 and endpoints[0].node_name==node.get_name(),'unexpected_publisher')
                    stamp=stamps[next_index];first_publish=time.time_ns() if first_publish is None else first_publish
                    publisher.publish(messages[stamp]);waiting_cloud=stamp;next_index+=1;last_publish=now
                if pending_scan and now>=pending_scan[1]:
                    stamp,_,observed=pending_scan;scans.publish(scan_messages[stamp]);waiting_scan=stamp;pending_scan=None
                    if stamp==stamps[-1]:result['final_scan_sent_after_cloud_observed_ms']=(now-observed)*1000
                if final_observed is not None and case in ('delayed-scan','missing-scan') and now-last_extra>=.01:
                    publisher.publish(messages[stamps[-1]]);last_extra=now;result['extra_clouds_during_drain']+=1
            for key,_ in selector.select(timeout=.005):
                chunk=os.read(key.fileobj.fileno(),32768)
                if not chunk:selector.unregister(key.fileobj);continue
                if key.data=='stderr':stderr.extend(chunk);require(len(stderr)<=65536,'stderr_bound');continue
                for row in lines.feed(chunk):
                    if row.get('kind')=='complete':
                        require(result['complete'] is None,'duplicate_complete');result['complete']=row
                        result['completion_after_launch_ms']=(time.monotonic()-launch)*1000
                        if final_observed is not None:result['completion_after_final_cloud_observed_ms']=(time.monotonic()-final_observed)*1000
                        continue
                    require(result['complete'] is None and row.get('kind')=='sample','unexpected_sample')
                    require(type(row.get('helper_elapsed_ns')) is int and last_elapsed<=row['helper_elapsed_ns']<duration*1_000_000,'helper_elapsed_bound')
                    last_elapsed=row['helper_elapsed_ns'];message_count+=1
                    raw=bytes.fromhex(row['payload_hex']);raw_total+=len(raw)
                    require(message_count<=MESSAGE_LIMIT and raw_total<=RAW_LIMIT,'native_total_bound')
                    if row.get('topic')=='/utlidar/cloud':
                        require(first_publish is not None and len(result['samples'])<maximum,'cloud_count')
                        record=check_cloud(row,variants,cloud_gid,(first_publish,time.time_ns()));cloud_gid=record['publisher_gid']
                        require(record['capture_unix_ns']==waiting_cloud,'duplicate_or_unrequested_cloud');waiting_cloud=None
                        result['samples'].append(record);observed=time.monotonic()
                        if len(result['samples'])==maximum:final_observed=observed
                        if scans and (case!='missing-scan' or len(result['samples'])<maximum):
                            pending_scan=(record['capture_unix_ns'],observed+(.025 if len(result['samples'])==maximum else 0),observed)
                    elif row.get('topic')=='/scan':
                        require(scans is not None,'unexpected_scan');s=deserialize_message(raw,LaserScan)
                        stamp=s.header.stamp.sec*10**9+s.header.stamp.nanosec
                        require(stamp==waiting_scan,'unexpected_scan_stamp');waiting_scan=None
                        expected=scan_messages[stamp]
                        require(s.header.frame_id=='lidar_link' and s.ranges==expected.ranges and s.intensities==expected.intensities,'scan_values')
                        for f in ('angle_min','angle_max','angle_increment','time_increment','scan_time','range_min','range_max'):
                            require(getattr(s,f)==struct.unpack('<f',struct.pack('<f',getattr(expected,f)))[0],'scan_scalar')
                        gid=row['publisher_gid'];require(re.fullmatch('[0-9a-f]{48}',gid) is not None and gid!='0'*48 and (scan_gid is None or gid==scan_gid),'scan_gid');scan_gid=gid
                        require(type(row['source_timestamp_ns']) is int and first_publish<=row['source_timestamp_ns']<=time.time_ns(),'scan_source_stamp')
                        require(type(row['received_timestamp_ns']) is int and row['received_timestamp_ns']>=0,'scan_received_stamp')
                        result['scans'].append({'capture_unix_ns':stamp,'serialized_bytes':len(raw),'serialized_sha256':hashlib.sha256(raw).hexdigest(),'publisher_gid':gid,'source_timestamp_ns':row['source_timestamp_ns'],'received_timestamp_ns':row['received_timestamp_ns'],'helper_elapsed_ns':row['helper_elapsed_ns']})
                    else:raise Refused('unexpected_topic')
            if process.poll() is not None and not selector.get_map():break
        require(process.poll() is not None,'helper_deadline');result['helper_exit']=process.returncode
        result['stdout_bytes']=lines.total;result['stderr_bytes']=len(stderr);result['stderr_sha256']=hashlib.sha256(stderr).hexdigest()
        require(process.returncode==0 and not lines.pending,'helper_completion')
        c=result['complete'];require(type(c) is dict and c.get('messages')==message_count and c.get('clouds')==len(result['samples']) and c.get('serialized_bytes')==raw_total,'completion_counts')
        require(len(result['samples'])>5,'extended_cloud_case_not_exercised')
        if case=='raw-limit':
            require(c['reason']=='capture_limit' and len(result['samples'])<maximum,'raw_limit_not_reached')
            require(0<=RAW_LIMIT-raw_total<110000,'raw_limit_headroom')
        elif case=='duration':
            require(c['reason']=='duration' and len(result['samples'])<maximum,'duration_not_reached')
            require(duration-100<=result['completion_after_launch_ms']<=duration+1500,'observed_duration_bound')
        else:
            require(c['reason']=='max_clouds' and len(result['samples'])==maximum,'cloud_limit')
            if scans:
                expected=maximum-(case=='missing-scan');require(len(result['scans'])==expected,'scan_count')
                missing=[stamp for stamp in stamps if stamp not in {s['capture_unix_ns'] for s in result['scans']}]
                result['missing_same_capture_scan']=missing
                require(missing==([stamps[-1]] if case=='missing-scan' else []),'missing_scan_classification')
                require(result['extra_clouds_during_drain']>0,'drain_flood_not_exercised')
        result['success']=True
    except Exception as error:result['failure']=str(error)[:120] if isinstance(error,Refused) else type(error).__name__
    finally:
        if process is not None:
            if process.poll() is None:process.kill()
            process.wait(timeout=.3);result['helper_reaped']=True
            for stream in (process.stdout,process.stderr):stream.close()
        selector.close()
        if node is not None:node.destroy_node()
        if ros is not None and ros.ok():ros.shutdown()
        result['elapsed_s']=time.monotonic()-started
        signal.setitimer(signal.ITIMER_REAL,0);signal.signal(signal.SIGALRM,previous)
    return result


class BoundsTests(unittest.TestCase):
    def test_fragmented_rows_and_total_cap(self):
        x=Lines();self.assertEqual(x.feed(b'{"kind":'),[]);self.assertEqual(x.feed(b'"complete"}\n'),[{'kind':'complete'}])
        x.total=STDOUT_LIMIT
        with self.assertRaises(Refused):x.feed(b'x')
    def test_row_bound_and_nonobjects(self):
        for payload in (b'x'*ROW_LIMIT,b'[]\n',b'{broken}\n'):
            with self.assertRaises(Refused):Lines().feed(payload)


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--self-test',action='store_true');p.add_argument('--helper',type=Path);p.add_argument('--observer',type=Path)
    p.add_argument('--case',choices=('many-clouds','delayed-scan','missing-scan','duration','raw-limit','arguments'),default='many-clouds');a=p.parse_args()
    if a.self_test:return 0 if unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(BoundsTests)).wasSuccessful() else 1
    for helper in [a.helper]+([a.observer] if a.case=='arguments' else []):
        require(helper is not None and helper.is_absolute() and helper.is_file() and not helper.is_symlink() and os.access(helper,os.X_OK),'helper_path')
    result=arguments(a.helper,a.observer) if a.case=='arguments' else run(a.helper,a.case)
    print(json.dumps(result,sort_keys=True),flush=True);return 0 if result['success'] else 1

if __name__=='__main__':raise SystemExit(main())
