#!/usr/bin/env python3
"""Compile audit2524 normal and exact pending-withdrawal control; no networking."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sys
ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('sharing_guardian',ROOT/'.github/scripts/run-owned-media-test.py')
OWNER=importlib.util.module_from_spec(spec);spec.loader.exec_module(OWNER)

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('output',type=Path)
    parser.add_argument('--prepare-only',action='store_true');args=parser.parse_args()
    output=args.output.resolve();output.mkdir(parents=True,exist_ok=False)
    source=ROOT/'go/internal/agent/meshsharing/controller.go';original=source.read_text()
    before='if err := c.policy.SetGateway(ctx, d.GatewayDevice); errors.Is(err, errGatewayPending) {'
    if original.count(before)!=1:raise ValueError('pending-lease source changed')
    mutant=output/'withdraw-pending.go';mutant.write_text(original.replace(before,before+'\nif err := c.withdrawGateway(cleanup); err != nil { return err }'))
    overlay=output/'control.json';overlay.write_text(json.dumps({'Replace':{str(source):str(mutant)}}))
    (output/'sources.json').write_text(json.dumps({str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in [source,ROOT/'go/internal/agent/meshsharing/control.go',ROOT/'go/internal/agent/meshsharing/lease_controller_namespace_linux_test.go',ROOT/'go/internal/agent/meshsharing/namespace_linux_test.go']},indent=2)+'\n')
    if args.prepare_only:return 0
    if sys.platform!='linux' or os.geteuid()==0:raise ValueError('compile as ordinary Linux user')
    for mode in ['normal','control']:
        command=['go','test','-race','-c','-o',str(output/(mode+'.test'))]
        if mode=='control':command+=['-overlay',str(overlay)]
        command+=['./go/internal/agent/meshsharing']
        with (output/(mode+'-compile.log')).open('w') as log:
            if OWNER.run(command,180,cwd=ROOT,stdout=log):raise RuntimeError(mode+' compile failed')
    return 0
if __name__=='__main__':sys.exit(main())
