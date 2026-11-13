#!/usr/bin/env python3
"""Audit1572: actual owned PTY disconnection, discovery callbacks and retry."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sys

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('serial_guardian',ROOT/'.github/scripts/run-owned-media-test.py')
OWNER=importlib.util.module_from_spec(spec);spec.loader.exec_module(OWNER)

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('output',type=Path);args=parser.parse_args()
    if sys.platform not in ('linux','darwin'):raise ValueError('Unix PTY runtime required')
    output=args.output.resolve();output.mkdir(parents=True,exist_ok=False)
    discovery=ROOT/'go/internal/shared/discovery/serial_discovery.go'
    flash=ROOT/'go/internal/cli/commands/espflash.go'
    changes=[('ghost-classification',discovery,'return nil, !errors.Is(err, liteclient.ErrSerialPortUnavailable), err','return nil, false, err','./go/internal/shared/discovery','TestSerialPTYLifecycle','opened PTY handshake failure misclassified'),
             ('missing-is-busy',flash,'return nil, err\n\t}\n\tport, err := serial.Open(portPath, mode)','return nil, fmt.Errorf("%w: %s", errPortBusy, err)\n\t}\n\tport, err := serial.Open(portPath, mode)','./go/internal/cli/commands','TestSerialPTYRetry','missing PTY cause lost')]
    sources=[discovery,flash,ROOT/'go/internal/shared/discovery/serial_pty_test.go',ROOT/'go/internal/cli/commands/espflash_pty_test.go']
    (output/'sources.json').write_text(json.dumps({str(p.relative_to(ROOT)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources},indent=2)+'\n')
    for name,source,before,after,package,test,expected in changes:
        for mutant in (False,True):
            label=name+('-control' if mutant else '-normal')
            command=['go','test','-race','-count=1','-timeout=30s']
            if mutant:
                text=source.read_text()
                if text.count(before)!=1:raise ValueError('historical control boundary changed: '+name)
                replaced=output/(label+'.go');replaced.write_text(text.replace(before,after))
                overlay=output/(label+'.json');overlay.write_text(json.dumps({'Replace':{str(source):str(replaced)}}))
                command+=['-overlay',str(overlay)]
            command+=[package,'-run','^'+test+'$','-v']
            with (output/(label+'.log')).open('w') as log:
                status=OWNER.run(command,180,cwd=ROOT,env=os.environ.copy(),stdout=log)
            text=(output/(label+'.log')).read_text();print(text)
            if '--- SKIP:' in text or 'WARNING: DATA RACE' in text:raise RuntimeError('invalid PTY evidence')
            if mutant:
                if status!=1 or expected not in text:raise RuntimeError('control did not fail at original boundary: '+label)
            elif status or '_VERIFIED ' not in text:raise RuntimeError('normal failed: '+label)
    print('SERIAL_DISCONNECT_MATRIX_VERIFIED; no USB enumeration, reset or firmware flashing claimed')
    return 0

if __name__=='__main__':sys.exit(main())
