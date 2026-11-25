#!/usr/bin/env python3
"""Exercise the actual live collector in an isolated existing ROS container.

Synthetic sensor publishers only. No runtime HTTP, motion publisher or arming.
"""
from array import array
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_live import LiveCloudSensors
from sim.go2_bridge.protocol import Refused
from go2_cloud_ros_interop import SPARSE, xyz_bytes


def arguments(helper, observer, shadow):
    rows=[]
    for binary,duration,clouds,code in ((helper,0,1,1),(helper,60001,1,1),(helper,100,0,1),
            (helper,100,2049,1),(observer,100,6,1),(shadow,100,129,1),(helper,1,2048,0)):
        p=subprocess.run([str(binary),'--duration-ms',str(duration),'--max-clouds',str(clouds)],
                         capture_output=True,timeout=3)
        if p.returncode!=code:raise ValueError('argument result')
        rows.append({'binary':binary.name,'duration_ms':duration,'clouds':clouds,'exit_code':p.returncode,
                     'stdout':p.stdout.decode(),'stderr':p.stderr.decode()})
    return {'case':'arguments','success':True,'cases':rows}


def run(case):
    import rclpy
    from rclpy.node import Node
    from rclpy.qos import QoSProfile,ReliabilityPolicy,DurabilityPolicy
    from sensor_msgs.msg import PointCloud2,PointField,LaserScan
    from nav_msgs.msg import Odometry
    from std_msgs.msg import UInt64
    from geometry_msgs.msg import TransformStamped
    from tf2_msgs.msg import TFMessage
    rclpy.init(args=[]);node=Node('world_go2_live_synthetic_test');live=None
    result={'schema':'wendy.world.go2-cloud-live-interop.v1','case':case,'simulation':True,
            'success':False,'runtime_calls':0,'commands_published':0,'operational_authority':False,
            'samples':[],'missing_observed':False,'identity_limit':'Callback GIDs and source pins are not authenticated identity.'}
    started=time.monotonic()
    try:
        sensor=QoSProfile(depth=1,reliability=ReliabilityPolicy.BEST_EFFORT,durability=DurabilityPolicy.VOLATILE)
        retained=QoSProfile(depth=1,reliability=ReliabilityPolicy.RELIABLE,durability=DurabilityPolicy.TRANSIENT_LOCAL)
        pubs={topic:node.create_publisher(cls,topic,retained if topic in ('/simulation/epoch','/tf_static') else sensor)
              for topic,cls in (('/utlidar/cloud',PointCloud2),('/scan',LaserScan),('/odom',Odometry),
                                ('/simulation/epoch',UInt64),('/tf_static',TFMessage))}
        transforms=[]
        for child in ('lidar_link','utlidar_lidar'):
            t=TransformStamped();t.header.frame_id='base_link';t.child_frame_id=child
            t.transform.translation.x=.28;t.transform.translation.z=.08;t.transform.rotation.w=1.;transforms.append(t)
        duration=1800 if case=='duration' else 30000
        profile={'observation_max_age_ms':250,'source_skew_max_ms':50,'cloud_experiment':{
            'model':'coherent-multiring-slab-v1','scope':'contained-simulation','collector_duration_ms':duration}}
        live=LiveCloudSensors(SOURCE_DIGEST,31,profile)
        published_base=False;pending=None;last_publish=0.;sent=0;missing_sent=False
        count=160 if case in ('stream','full-cloud') else 8
        indices=tuple(range(9000)) if case=='full-cloud' else SPARSE
        payload=xyz_bytes(indices)
        while time.monotonic()-started<26:
            rclpy.spin_once(node,timeout_sec=0)
            now=time.monotonic()
            if not published_base and all(p.get_subscription_count()>0 for p in pubs.values()):
                pubs['/simulation/epoch'].publish(UInt64(data=31));pubs['/tf_static'].publish(TFMessage(transforms=transforms))
                published_base=True
            if live.fault:
                if case in ('publisher-change','helper-exit','duration') and len(result['samples'])>=3:
                    break
                raise ValueError('collector fault '+live.fault)
            state=live.diagnostics()['assembler']
            base_ready=state['mounts']==2 and '/simulation/epoch' in state['publisher_gids']
            if published_base and base_ready and pending is None and sent<count and now-last_publish>=.1:
                stamp=time.time_ns();sec,nsec=divmod(stamp,10**9)
                cloud=PointCloud2();cloud.header.frame_id='utlidar_lidar';cloud.header.stamp.sec=sec;cloud.header.stamp.nanosec=nsec
                cloud.height=1;cloud.width=len(indices);cloud.point_step=12;cloud.row_step=len(payload);cloud.is_dense=True
                cloud.fields=[PointField(name=name,offset=i*4,datatype=PointField.FLOAT32,count=1) for i,name in enumerate(('x','y','z'))]
                cloud.data=array('B',payload)
                odom=Odometry();odom.header.frame_id='odom';odom.header.stamp=cloud.header.stamp;odom.child_frame_id='base_link'
                odom.pose.pose.position.z=.33;odom.pose.pose.orientation.w=1.
                scan=LaserScan();scan.header.frame_id='lidar_link';scan.header.stamp=cloud.header.stamp
                scan.angle_min=-math.pi;scan.angle_max=math.pi-2*math.pi/360;scan.angle_increment=2*math.pi/360
                scan.scan_time=.1;scan.range_min=.1;scan.range_max=12.
                scan.ranges=[1+(j%11)/10 if j in indices else math.inf for j in range(4320,4680)]
                pubs['/odom'].publish(odom)
                if case=='missing-scan' and sent==3 and not missing_sent:
                    missing_sent=True
                else:pubs['/scan'].publish(scan)
                pubs['/utlidar/cloud'].publish(cloud)
                pending=(stamp,now);sent+=1;last_publish=now
            try:
                value=live.snapshot()
            except Refused as error:
                if case=='missing-scan' and missing_sent and pending and sent==4 and now-pending[1]>.12:
                    if live.latest is not None:raise ValueError('missing candidate left positive data')
                    result['missing_observed']=True;pending=None
                if live.fault:continue
                if str(error) not in ('sensors_not_ready','sensor_cloud_unavailable','sensor_cloud_stale'):
                    raise
            else:
                if pending and value['cloud']['captured_unix_ns']==pending[0]:
                    original=live.original_capture(value['capture_sequence'])
                    if original is None or original.cloud.value.capture_unix_ns!=pending[0]:raise ValueError('original missing')
                    if value['cloud']['serialized_sha256']!=hashlib.sha256(original.cloud.raw_cdr).hexdigest():raise ValueError('digest changed')
                    if value['scan_captured_unix_ns']!=pending[0] or value['odom_captured_unix_ns']!=pending[0]:raise ValueError('pair changed')
                    result['samples'].append({'sequence':value['capture_sequence'],'captured_unix_ns':pending[0],
                        'serialized_sha256':value['cloud']['serialized_sha256'],'publisher_gid':value['cloud']['publisher_gid'],
                        'finite_rays':sum(v is not None for v in value['cloud']['ranges'])})
                    pending=None
                    if len(result['samples'])==3 and case=='publisher-change':
                        node.destroy_publisher(pubs['/utlidar/cloud'])
                        pubs['/utlidar/cloud']=node.create_publisher(PointCloud2,'/utlidar/cloud',sensor)
                    if len(result['samples'])==3 and case=='helper-exit':live.process.terminate()
            if len(result['samples'])==count-(case=='missing-scan') and case not in ('duration','publisher-change','helper-exit'):
                break
            if pending and now-pending[1]>1:
                raise ValueError('original candidate not retained '+live.unavailable_reason)
            time.sleep(.002)
        result['before_close']=live.diagnostics()
        if case in ('stream','full-cloud') and len(result['samples'])!=160:raise ValueError('did not pass old128 cloud limit')
        if case=='missing-scan' and not result['missing_observed']:raise ValueError('missing scan not exercised')
        if case in ('publisher-change','helper-exit','duration') and not live.fault:raise ValueError('fault did not latch')
        if len(result['samples'])<3:raise ValueError('no positive input before fault')
        if len(live.originals)>8:raise ValueError('original history bound')
        result['success']=True
    except Exception as error:
        result['failure']=str(error)[:160]
        if live is not None:result['before_close']=live.diagnostics()
    finally:
        if live is not None:
            live.close();result['cleanup']=live.diagnostics()['cleanup'];result['helper_reaped']=live.process.poll() is not None
        node.destroy_node();rclpy.shutdown();result['elapsed_s']=time.monotonic()-started
    return result


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--case',required=True,choices=('arguments','stream','full-cloud','missing-scan','publisher-change','helper-exit','duration'))
    p.add_argument('--helper',type=Path,default=Path('/opt/world/world-go2-cloud-live'))
    p.add_argument('--observer',type=Path);p.add_argument('--shadow',type=Path);a=p.parse_args()
    signal.signal(signal.SIGALRM,lambda *_: os._exit(124));signal.alarm(35)
    try:
        result=arguments(a.helper,a.observer,a.shadow) if a.case=='arguments' else run(a.case)
        print(json.dumps(result,sort_keys=True),flush=True);return 0 if result['success'] else 1
    finally:signal.alarm(0)


if __name__=='__main__':raise SystemExit(main())
