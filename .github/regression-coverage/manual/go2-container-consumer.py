#!/usr/bin/env python3
"""Finite read-only V4L2 consumer for the exact generated Go2 acceptance app."""
import hashlib
import json
import os
import subprocess

node=os.environ['GO2_CAMERA_NODE']
if not node.startswith('/dev/video') or not node.removeprefix('/dev/video').isdigit():
    raise SystemExit('invalid selected camera node')
# Kernel compressed data is decoded independently. Fixed caps reject another
# size/codec rather than silently rescaling it to look like the expected camera.
command=['gst-launch-1.0','-q','v4l2src','device='+node,'num-buffers=60','!',
         'video/x-h264,width=640,height=360','!','h264parse','!','avdec_h264','!',
         'videoconvert','!','video/x-raw,format=RGB','!','fdsink','fd=1']
result=subprocess.run(command,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=25,check=True)
size=640*360*3
if len(result.stdout)!=60*size:raise SystemExit('did not independently decode exactly60 full640x360 RGB frames')
rows=[]
for index in range(60):
    frame=result.stdout[index*size:(index+1)*size]
    pixels=[frame[(y*640+x)*3:(y*640+x)*3+3] for y in range(140,220,8) for x in range(280,360,8)]
    rows.append({'frame':index,'sha256':hashlib.sha256(frame).hexdigest(),
                 'center_rgb_mean':[sum(p[c] for p in pixels)//len(pixels) for c in range(3)]})
if len({r['sha256'] for r in rows})<20:raise SystemExit('fewer than20 distinct decoded frames; scene challenge inconclusive')
print(json.dumps({'nonce':os.environ['GO2_CAMERA_NONCE'],'node':node,'codec':'H264','width':640,'height':360,
                  'frames':rows,'human_challenge_review':'required','physical_acceptance':'not_run'}),flush=True)
