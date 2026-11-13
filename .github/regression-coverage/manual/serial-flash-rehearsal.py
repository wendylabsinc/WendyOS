#!/usr/bin/env python3
"""Generate reserved ESP32 CP210x/native-USB acceptance; never open a device."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex

ROOT=Path(__file__).resolve().parents[3]
spec=importlib.util.spec_from_file_location('serial_common',Path(__file__).with_name('orin-firstboot-rehearsal.py'))
COMMON=importlib.util.module_from_spec(spec);spec.loader.exec_module(COMMON)
artifact=COMMON.artifact
line=COMMON.line

def generate(inventory,output):
    data=json.loads(Path(inventory).read_text());host=data['host'];board=data['board']
    if host['os'] not in ('linux','macos') or host['reserved_account'] is not True:raise ValueError('reserved Unix account required')
    for key in ('identity','os_build','owner'):line(host[key])
    if type(host['uid']) is not int or host['uid']<501:raise ValueError('ordinary reserved UID required')
    if board['reserved'] is not True:raise ValueError('reserved board required')
    for key in ('serial','hardware_id','owner','model','recovery_instructions'):line(board[key])
    if not re.fullmatch('[a-z][a-z0-9-]{2,40}',board['device_name']):raise ValueError('unique device name required')
    if not re.fullmatch('[a-f0-9]{40}',data['cli_source_commit']):raise ValueError('pin CLI revision')
    catalog_source=(ROOT/'go/internal/cli/commands/wlite_variants.go').read_text()
    catalog=json.loads(catalog_source.split('const wendyLiteCatalogJSON = `',1)[1].split('`',1)[0])
    selected=next((b for b in catalog['boards'] if b['name']==board['catalog_board']),None)
    if selected is None:raise ValueError('unknown catalog board')
    binary=next(b for b in catalog['binaries'] if b['name']==selected['binary'].removeprefix('wendy_mcu_'))
    if binary['target']!=board['chip']:raise ValueError('board/chip mismatch')
    # This physical matrix needs both supported connectors on the same board.
    if board['cp210x']['vid_pid']!='10c4:ea60' or board['native']['vid_pid']!='303a:1001':raise ValueError('require actual CP210x and native Serial/JTAG pair')
    for transport in ('cp210x','native'):
        for key in ('path','usb_serial','location'):line(board[transport][key])
        if not board[transport]['path'].startswith('/dev/'):raise ValueError('exact native serial path required')
    if board['cp210x']['path']==board['native']['path']:raise ValueError('different physical connectors required')
    for key in ('native_replug_path',):
        if not line(board[key]).startswith('/dev/') or board[key]==board['native']['path']:raise ValueError('predeclare changed native path for replug trial')
    records={key:artifact(data[key]) for key in ('cli_binary','identity_record','recovery_record','blank_state_record','reinstall_state_record','app_review_record','main_manifest','firmware_manifest','firmware_binary')}
    if not os.access(records['cli_binary']['path'],os.X_OK):raise ValueError('executable candidate CLI required')
    main=json.loads(Path(records['main_manifest']['path']).read_text());fm=json.loads(Path(records['firmware_manifest']['path']).read_text())
    firmware_id=selected['binary'].removeprefix('wendy_mcu_');entry=main['firmware'][firmware_id];version=entry['latest'];fw=fm['versions'][version]
    if fm['firmware_id']!=firmware_id or version in ('latest','nightly') or not re.fullmatch('[A-Za-z0-9._-]+',version):raise ValueError('exact stable firmware identity required')
    if fw['checksum'].removeprefix('sha256:')!=records['firmware_binary']['sha256'] or fw['size_bytes']!=records['firmware_binary']['size_bytes']:raise ValueError('published firmware bytes mismatch')
    if any(x in fw['path'] for x in ('..','?','#')) or fw['path'].startswith('/'):raise ValueError('unsafe firmware object path')
    project=Path(line(data['project'])).resolve(strict=True)
    if not (project/'wendy.json').is_file():raise ValueError('reviewed harmless Lite app required')
    project_records=[]
    for p in sorted(project.rglob('*')):
        if p.is_symlink():raise ValueError('app input must not follow symlinks')
        if p.is_file():
            if p.stat().st_size>4*1024*1024 or len(project_records)>=100:raise ValueError('use a small reviewed source-only app')
            project_records.append(artifact(str(p)))
    out=Path(output).resolve();out.mkdir(parents=True,exist_ok=False,mode=0o700)
    for name in ('home','cli-profile','cache'):(out/name).mkdir(mode=0o700)
    cache=out/('home/Library/Caches' if host['os']=='macos' else 'cache')/'wendy/wendy-lite-firmware';cache.mkdir(parents=True)
    cached=cache/(version+'-'+Path(fw['path']).name)
    # Private cache binds the reviewed bytes when the live manifest still selects
    # this exact asset. Manifest drift invalidates the trial; no pin flag exists.
    cached.write_bytes(Path(records['firmware_binary']['path']).read_bytes());cached.chmod(0o600)
    env=['env','HOME='+str(out/'home'),'XDG_CACHE_HOME='+str(out/'cache'),'WENDY_CONFIG_DIR='+str(out/'cli-profile'),'WENDY_SECRET_STORE=file']
    cli=env+[records['cli_binary']['path']]
    guardian=['python3',str(ROOT/'.github/scripts/run-owned-media-test.py'),'--timeout']
    commands={
      'discover':guardian+['20','--']+cli+['--json','discover','--type','usb','--timeout','8s'],
      'install':guardian+['900','--']+cli+['os','install','--no-wifi','--pre-enroll=false','--device-name',board['device_name']],
      'identity_before':guardian+['45','--']+cli+['--json','--device','wendy-lite:'+board['native']['path'],'device','info','--read-only'],
      'identity_after':guardian+['45','--']+cli+['--json','--device','wendy-lite:'+board['native_replug_path'],'device','info','--read-only'],
      'reinstall':guardian+['900','--']+cli+['--device','wendy-lite:'+board['native']['path'],'run','--prefix',str(project),'--skip-cloud-registration'],
    }
    if host['os']=='linux':commands['usb_inventory']=['udevadm','info','--query=property','--name',board['native']['path']]
    else:commands['usb_inventory']=['ioreg','-p','IOUSB','-l','-w','0']
    guide=Path(__file__).with_suffix('.txt').read_text()
    guide+='\nExact candidate commands (generation did not execute them):\n'
    for name,args in commands.items():guide+='\n'+name+':\n'+shlex.join(args)+'\n'
    guide+='\nSelected catalog board: '+board['catalog_board']+'; chip: '+board['chip']+'; version: '+version+'\nPrivate original firmware cache: '+str(cached)+'\n'
    (out/'procedure.txt').write_text(guide)
    (out/'commands.json').write_text(json.dumps(commands,indent=2)+'\n')
    paths=['go/internal/cli/commands/espflash.go','go/internal/cli/commands/os_install.go','go/internal/cli/commands/run.go','go/internal/cli/commands/wlite_variants.go','go/internal/shared/discovery/serial_discovery.go','go/internal/shared/discovery/serial_usb.go','go/internal/cli/liteclient/client.go']
    evidence={'audit_entries':['1768','1572'],'hardware_execution':'not_run','inventory':data,'inventory_record':artifact(str(Path(inventory).resolve())),'records':records,'app_sources':project_records,'sources':[artifact(str(ROOT/p)) for p in paths]+[artifact(str(Path(__file__))),artifact(str(Path(__file__).with_suffix('.txt')))],'generated':[artifact(str(out/p)) for p in ('commands.json','procedure.txt')],'firmware':{'id':firmware_id,'version':version,'cache':str(cached),'sha256':records['firmware_binary']['sha256']},'cases':[{'id':key,'status':'not_run','observations':[]} for key in ('host_board_identity','cp210x_first_flash','native_first_flash_reset','unplug_replug','reinstall_and_live_app','wrong_chip_refusal','restoration')]}
    (out/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n');return evidence

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('inventory',type=Path);p.add_argument('output',type=Path);a=p.parse_args();generate(a.inventory,a.output)
