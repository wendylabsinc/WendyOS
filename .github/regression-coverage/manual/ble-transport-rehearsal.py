#!/usr/bin/env python3
"""Prepare exact-host/peripheral BLE acceptance offline; all hardware stays not_run."""
import argparse
import importlib.util
import json
from pathlib import Path
import platform
import re
import shlex

ROOT=Path(__file__).resolve().parents[3]
SPEC=importlib.util.spec_from_file_location('ble_plan_helpers',Path(__file__).with_name('orin-firstboot-rehearsal.py'))
COMMON=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(COMMON)
artifact,line=COMMON.artifact,COMMON.line


def generate(inventory,output):
    data=json.loads(Path(inventory).read_text())
    def reject(value):
        if isinstance(value,dict):
            for key,child in value.items():
                if any(part in key.lower() for part in ('password','private_key','token','passphrase')):raise ValueError('no credentials in physical BLE inventory')
                reject(child)
        elif isinstance(value,list):
            for child in value:reject(child)
    reject(data)
    host=data['host'];board=data['peripheral']
    osname={'Darwin':'darwin','Linux':'linux','Windows':'windows'}[platform.system()]
    if host['platform']!=osname:raise ValueError('generate on the exact intended host platform')
    for key in ('identity','owner','os_version','adapter_identity','radio_firmware','permission_record'):line(host[key])
    for key in ('serial','device_id','board_model','owner','power_connection','recovery_record'):line(board[key])
    if not re.fullmatch('[a-f0-9]{32}',data['trial']):raise ValueError('independent trial nonce required')
    for key in ('source_commit','firmware_source_commit'):
        if not re.fullmatch('[a-f0-9]{40}',data[key]):raise ValueError('pin exact source commits')
    address=line(board['address'])
    if osname=='darwin':
        if not re.fullmatch('[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}',address):raise ValueError('require independently matched CoreBluetooth UUID')
    elif not re.fullmatch('(?:[0-9A-F]{2}:){5}[0-9A-F]{2}',address):raise ValueError('require exact observed uppercase BLE MAC')
    if board.get('gatt_profile')!='wendy-lite-info-and-provisioning-status':raise ValueError('procedure requires actual Lite info/status services')
    keys=('probe_binary','cli_binary','firmware_binary','identity_record','build_record','authorization_record','permission_record','recovery_record')
    records={key:artifact(data[key]) for key in keys}
    out=Path(output).resolve();out.mkdir(mode=0o700,parents=True,exist_ok=False)
    plan={'Platform':osname,'Address':address,'DeviceID':board['device_id'],'Trial':data['trial'],'RestoreFile':str(out/'restored.nonce'),'PermitFile':str(out/'supervisor.nonce'),'SuccessFile':str(out/'completed.nonce')}
    (out/'probe-plan.json').write_text(json.dumps(plan,indent=2)+'\n')
    runner=Path(__file__).with_name('ble-transport-run.py');(out/runner.name).write_text(runner.read_text())
    test='TestPhysicalWinRTScanRestart' if osname=='windows' else 'TestPhysicalLiteGATTDisconnect'
    command=['python3',str(out/runner.name),str(out/'evidence.json')]
    guide=Path(__file__).with_suffix('.txt').read_text()
    for key,value in {'@PLATFORM@':osname,'@HOST@':host['identity'],'@SERIAL@':board['serial'],'@TRIAL@':data['trial'],'@TEST@':test,'@ADDRESS@':address}.items():guide=guide.replace(key,value)
    rendered=('& '+' '.join("'"+part.replace("'","''")+"'" for part in command)) if osname=='windows' else shlex.join(command)
    guide+='\nExact generated physical command, not executed by generation:\n'+rendered+'\n'
    guide+='\nAfter authorized restoration on macOS/Linux only, write the exact fresh acknowledgment:\n'+shlex.join(['python3','-c','from pathlib import Path; import sys; p=Path(sys.argv[1]); f=p.open("x"); f.write(sys.argv[2]+"\\n"); f.close()',plan['RestoreFile'],data['trial']])+'\n'
    (out/'procedure.txt').write_text(guide)
    source_paths=[Path(__file__),Path(__file__).with_suffix('.txt'),runner,Path(__file__).with_name('orin-firstboot-rehearsal.py')]+[ROOT/p for p in ('.github/scripts/run-owned-media-test.py','.github/scripts/test-linklocal-windows.py','go/internal/shared/ble/central/physical_disconnect_test.go','go/internal/shared/ble/central/ble_darwin.m','go/internal/shared/ble/central/ble_darwin.go','go/internal/shared/ble/central/ble_linux.go','go/internal/shared/ble/central/gatt_linux.go','go/internal/shared/ble/central/bluez_linux.go','go/internal/shared/ble/central/ble_windows.go','go/internal/shared/ble/scan/physical_windows_test.go','go/internal/shared/ble/scan/scan_windows.go','go/internal/shared/ble/lite_info.go','go/internal/cli/ble/lite_client.go')]
    evidence={'audit_entries':['1903'],'hardware_execution':'not_run','inventory':data,'inventory_record':artifact(str(Path(inventory).resolve())),'records':records,'sources':[artifact(str(p)) for p in source_paths],'generated':[artifact(str(out/p)) for p in ('probe-plan.json',runner.name,'procedure.txt')],'probe_plan':str(out/'probe-plan.json'),'test':test,'guardian':str(ROOT/'.github/scripts/run-owned-media-test.py'),'job_helper':str(ROOT/'.github/scripts/test-linklocal-windows.py'),'cases':[{'id':name,'status':'not_run','observations':[]} for name in ('host_peripheral_provenance','permissions','actual_transport','cleanup_and_reconnect')],'unsupported':['Windows GATT and L2CAP central are not implemented'] if osname=='windows' else []}
    (out/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
    return evidence


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('inventory',type=Path);parser.add_argument('output',type=Path)
    args=parser.parse_args();generate(args.inventory,args.output)
