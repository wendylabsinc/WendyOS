#!/usr/bin/env python3
"""Audit2527: real provider/TLS traffic with controlled HCI syscall outcomes."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sys

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('interval_guardian',ROOT/'.github/scripts/run-owned-media-test.py')
OWNER=importlib.util.module_from_spec(spec);spec.loader.exec_module(OWNER)


def overlays(output):
    source=ROOT/'go/internal/agent/bleprovider/conninterval_linux.go';text=source.read_text()
    for before,after in (
        ('func meshACLHandle(conn net.Conn) (uint16, error) {',
         'func meshACLHandle(conn net.Conn) (uint16, error) {\nreturn intervalTransportHandle(conn)'),
        ('func prepareMeshIntervalIO(ctx context.Context, hciIndex int) (*leUpdateIO, error) {',
         'func prepareMeshIntervalIO(ctx context.Context, hciIndex int) (*leUpdateIO, error) {\nreturn intervalTransportPrepare(ctx,hciIndex)'),
    ):
        if text.count(before)!=1:raise ValueError('HCI syscall seam changed')
        text=text.replace(before,after)
    stub=output/'controller.go';stub.write_text(text)
    policy=ROOT/'go/internal/agent/btradio/policy.go';text=policy.read_text()
    before='var policyPath = "/config/bluetooth-radio.json"'
    if text.count(before)!=1:raise ValueError('radio policy path changed')
    isolated=output/'policy.go';isolated.write_text(text.replace(before,'var policyPath = os.Getenv("WENDY_BLE_INTERVAL_POLICY")'))
    shared={str(source):str(stub),str(policy):str(isolated)}
    provider=ROOT/'go/internal/agent/bleprovider/provider_linux.go';text=provider.read_text()
    before='var rejected *leUpdateRejectedError'
    after='errors.As(err, &unavailable) || errors.As(err, &rejected) || errors.As(err, &completed)'
    if text.count(before)!=1 or text.count(after)!=1:raise ValueError('original rejection fallback changed')
    mutant=output/'reject-fallback.go';mutant.write_text(text.replace(before,'').replace(after,'errors.As(err, &unavailable) || errors.As(err, &completed)'))
    for name,mapping in [('normal',shared),('reject-fallback',{**shared,str(provider):str(mutant)})]:
        (output/(name+'.json')).write_text(json.dumps({'Replace':mapping}))


def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('output',type=Path)
    parser.add_argument('--prepare-only',action='store_true',help='write overlays only; no runtime acceptance')
    args=parser.parse_args();output=args.output.resolve();output.mkdir(parents=True,exist_ok=False)
    overlays(output)
    sources=['go/internal/agent/bleprovider/conninterval_linux.go','go/internal/agent/bleprovider/provider_linux.go','go/internal/agent/bleprovider/interval_transport_linux_test.go','go/internal/agent/btradio/policy.go']
    (output/'sources.json').write_text(json.dumps({name:hashlib.sha256((ROOT/name).read_bytes()).hexdigest() for name in sources},indent=2)+'\n')
    if args.prepare_only:return 0
    if sys.platform!='linux' or os.geteuid()==0:raise ValueError('ordinary Linux user required; no radio access')
    env={**os.environ,'WENDY_BLE_INTERVAL_POLICY':str(output/'radio.json')}
    for name in ('normal','reject-fallback'):
        binary=output/(name+'.test')
        command=['go','test','-race','-vet=off','-tags','regression_ble_interval','-overlay',str(output/(name+'.json')),'-c','-o',str(binary),'./go/internal/agent/bleprovider']
        with (output/(name+'-compile.log')).open('w') as log:
            if OWNER.run(command,180,cwd=ROOT,env=env,stdout=log):raise RuntimeError('compile failed: '+name)
        selection='^TestBLEControllerOutcomePreservesAuthenticatedTraffic$'
        if name!='normal':selection+='/^command-disallowed$'
        with (output/(name+'.log')).open('w') as log:
            status=OWNER.run([str(binary),'-test.run',selection,'-test.v','-test.count=1','-test.timeout=25s'],30,cwd=ROOT,env=env,stdout=log)
        log=(output/(name+'.log')).read_text();print(log)
        if '--- SKIP:' in log or 'WARNING: DATA RACE' in log:raise RuntimeError('invalid interval runtime evidence')
        if name=='normal':
            if status or log.count('BLE_INTERVAL_TRANSPORT_VERIFIED mode=')!=8:raise RuntimeError('normal matrix failed')
        elif status!=1 or 'controller outcome did not preserve authenticated traffic: no authenticated attachment' not in log:
            raise RuntimeError('control failed outside intended traffic boundary')
        binary.unlink()
    print('BLE_INTERVAL_PROVIDER_MATRIX_VERIFIED actual TLS/hello/echo; synthetic HCI, no physical radio')
    return 0


if __name__=='__main__':sys.exit(main())
