#!/usr/bin/env python3
"""Explicit, read-only ROS subscription for a reserved Go2 camera acceptance.

Run on the operator-verified wired companion, with its pinned ROS/Unitree message
installation. This script never publishes a robot-control message or changes robot state.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import struct
import time


def append_sample(output, payload, rows, received_ns):
    if not isinstance(payload, bytes) or not 24 <= len(payload) <= 1024 * 1024:
        raise ValueError('raw DDS sample is empty, oversized or not bytes')
    if payload[:2] != b'\x00\x01':
        raise ValueError('need observed little-endian CDR encoding')
    timestamp, resolution, length = struct.unpack_from('<QII', payload, 4)
    if resolution != 360 or length < 5 or length > len(payload)-20 or len(payload)-20-length > 3:
        raise ValueError('sample differs from observed 360p resolution/H264 firmware layout')
    unit=payload[20:20+length]
    if not unit.startswith((b'\x00\x00\x01',b'\x00\x00\x00\x01')):
        raise ValueError('sample is not Annex-B H264')
    if len(rows)>=1800 or sum(row['bytes'] for row in rows)+len(payload)>64*1024*1024:
        raise ValueError('bounded recording limit exceeded')
    name=f'{len(rows):04d}.cdr'
    with (output/name).open('xb') as stream:stream.write(payload)
    rows.append({'file':name,'bytes':len(payload),'sha256':hashlib.sha256(payload).hexdigest(),
                 'received_monotonic_ns':received_ns,'firmware_timestamp':timestamp,
                 'resolution':resolution,'h264_bytes':length})


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--plan',required=True,type=Path)
    parser.add_argument('--confirm-robot-serial',required=True)
    parser.add_argument('--output',required=True,type=Path)
    args=parser.parse_args()
    plan=json.loads(args.plan.read_text())
    if args.confirm_robot_serial!=plan['robot']['serial']:
        parser.error('exact physical robot serial confirmation required')
    if os.environ.get('ROS_DOMAIN_ID')!='0':
        parser.error('set the reviewed robot camera domain explicitly to0')
    if plan['robot']['topic']!='/frontvideostream' or plan['robot']['resolution']!=360:
        parser.error('only the exact original front camera360p acceptance is supported')
    args.output.mkdir(mode=0o700,parents=False,exist_ok=False)
    rows=[];error=None;stopped=False
    def stop(_signum,_frame):
        nonlocal stopped
        stopped=True
    previous={sig:signal.signal(sig,stop) for sig in (signal.SIGINT,signal.SIGTERM)}
    try:
        import rclpy
        from rclpy.qos import qos_profile_sensor_data
        from unitree_go.msg import Go2FrontVideoData
        rclpy.init(args=[])
        node=rclpy.create_node('wendy_go2_camera_record_'+plan['nonce'][-12:],
                               enable_rosout=False,start_parameter_services=False)
        try:
            def sample(raw):append_sample(args.output,raw,rows,time.monotonic_ns())
            subscription=node.create_subscription(Go2FrontVideoData,'/frontvideostream',sample,qos_profile_sensor_data,raw=True)
            deadline=time.monotonic()+30
            while time.monotonic()<deadline and not stopped:
                rclpy.spin_once(node,timeout_sec=0.1)
            node.destroy_subscription(subscription)
            if stopped:raise RuntimeError('recording interrupted; cannot accept a partial interval')
            if len(rows)<30:raise RuntimeError('fewer than30 raw camera samples; acceptance remains pending')
        finally:
            node.destroy_node();rclpy.shutdown()
    except BaseException as exc:
        error=f'{type(exc).__name__}: {exc}'
    finally:
        for sig,handler in previous.items():signal.signal(sig,handler)
        report={'schema_version':1,'plan_sha256':hashlib.sha256(args.plan.read_bytes()).hexdigest(),
                'robot':plan['robot'],'target':plan['target'],'samples':rows,'error':error,
                'physical_identity':'operator confirmation required','acceptance':'not_run'}
        (args.output/'recording.json').write_text(json.dumps(report,indent=2)+'\n')
    if error:raise SystemExit(error)

if __name__=='__main__':main()
