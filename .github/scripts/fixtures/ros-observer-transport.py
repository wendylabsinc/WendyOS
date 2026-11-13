#!/usr/bin/env python3
"""Actual production Observer + real DDS; controlled odometry, no robot physics."""
import argparse
import importlib.util
import json
import hashlib
from pathlib import Path
import time

import rclpy
from nav_msgs.msg import Odometry
from rclpy.executors import ExternalShutdownException
from rclpy.node import Node
from rclpy.qos import QoSProfile, ReliabilityPolicy


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('mode',choices=('publish','observe'))
    parser.add_argument('--nonce',required=True)
    parser.add_argument('--seconds',type=float,default=8)
    args=parser.parse_args()
    if not 1<=args.seconds<=12 or not args.nonce:raise ValueError('bounded duration/nonce required')
    rclpy.init(args=[])
    if args.mode=='publish':
        node=Node('controlled_go2_host_odometry',enable_rosout=False,start_parameter_services=False)
        publisher=node.create_publisher(Odometry,'/odom',QoSProfile(depth=5,reliability=ReliabilityPolicy.BEST_EFFORT))
        count=0;deadline=time.monotonic()+60
        try:
            while rclpy.ok() and time.monotonic()<deadline:
                count+=1;stamp=time.time_ns();message=Odometry()
                message.header.stamp.sec=stamp//1_000_000_000;message.header.stamp.nanosec=stamp%1_000_000_000
                message.header.frame_id=args.nonce;message.child_frame_id='controlled-source-no-physics'
                message.pose.pose.position.x=float(count)
                publisher.publish(message)
                rclpy.spin_once(node,timeout_sec=.05)
        except ExternalShutdownException:
            pass
        finally:
            node.destroy_node();rclpy.try_shutdown()
            print(json.dumps({'event':'controlled_publisher_complete','nonce':args.nonce,'published':count}),flush=True)
        return
    spec=importlib.util.spec_from_file_location('shipped_observer','/production-observe.py')
    production=importlib.util.module_from_spec(spec);spec.loader.exec_module(production)
    class Observed(production.Observer):
        # Keep the production subscription, callback and freshness accounting;
        # this tap only binds received wire messages to this controlled source.
        def __init__(self):
            self.counters=[];self.foreign=False
            super().__init__()
        def observe(self,message):
            if message.header.frame_id!=args.nonce or message.child_frame_id!='controlled-source-no-physics':
                self.foreign=True
            counter=message.pose.pose.position.x
            if not counter.is_integer() or counter<=0 or (self.counters and counter<=self.counters[-1]):
                self.foreign=True
            self.counters.append(counter)
            super().observe(message)
    node=Observed();started=time.monotonic()
    try:
        while time.monotonic()-started<args.seconds:rclpy.spin_once(node,timeout_sec=.05)
        snapshot=node.snapshot()
        passed=(not node.foreign and node.fresh_samples>=3 and len(node.counters)>=3
                and snapshot['source_time_advanced_seconds']>0
                and -.1<=snapshot['last_source_age_seconds']<=1
                and snapshot['last_receipt_age_seconds']<=1)
        print(json.dumps({'event':'production_observer_result','passed':passed,'nonce':args.nonce,
                          'counters':node.counters,'snapshot':snapshot,
                          'source_sha256':{name:hashlib.sha256(Path('/'+name).read_bytes()).hexdigest() for name in ('production-observe.py','observer-wendy.json','go2-cyclonedds.xml')},'scope':'DDS only; no simulator status/physics'},allow_nan=False),flush=True)
        if not passed:raise RuntimeError('OBSERVER_HOST_ODOMETRY: production observer did not receive fresh owned host DDS')
    finally:
        node.destroy_node();rclpy.try_shutdown()

if __name__=='__main__':main()
