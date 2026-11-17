#!/usr/bin/env python3
"""Read the real Linux input registry for one reserved Bluetooth address.

This records observations only. It never creates input devices, opens an event
node, injects events, changes Bluetooth state or marks a case passed.
"""
import argparse
import datetime
import json
from pathlib import Path
import re
import sys
import time


def snapshot(address, root=Path('/sys/class/input')):
    found=[]
    paths=sorted(root.glob('input*/uniq'))
    if len(paths)>4096:
        raise ValueError('input registry exceeds observation bound')
    for path in paths:
        try:
            uniq=path.read_text().strip()
            if uniq.casefold()!=address.casefold():
                continue
            found.append({'input':str(path.parent.resolve()),'uniq':uniq,
                          'events':[str(p.resolve()) for p in sorted(path.parent.glob('event*'))]})
        except FileNotFoundError:
            continue
    return found


def record(address, seconds, output):
    deadline=time.monotonic()+seconds
    with Path(output).open('x',encoding='utf8') as stream:
        while True:
            stream.write(json.dumps({'utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),
                'monotonic':time.monotonic(),'address':address,'input_devices':snapshot(address)})+'\n')
            stream.flush()
            remaining=deadline-time.monotonic()
            if remaining<=0:
                return
            time.sleep(min(.25,remaining))


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--address',required=True)
    parser.add_argument('--seconds',type=float,default=65)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    if sys.platform!='linux' or not re.fullmatch(r'(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}',args.address) or not 0<args.seconds<=90:
        parser.error('requires Linux, one exact address and a duration in (0,90] seconds')
    record(args.address,args.seconds,args.output)
