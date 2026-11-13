#!/usr/bin/env python3
"""Generate reserved BLE/mesh hardware acceptance; generation never runs a device command."""
import argparse
import importlib.util
import ipaddress
import json
from pathlib import Path
import re
import shlex
from urllib.parse import parse_qs, urlsplit

ROOT=Path(__file__).resolve().parents[3]
SPEC=importlib.util.spec_from_file_location('ble_mesh_common',Path(__file__).with_name('orin-firstboot-rehearsal.py'))
COMMON=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(COMMON)
line,artifact=COMMON.line,COMMON.artifact
UUID=r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}'

def generate(inventory,output):
    data=json.loads(Path(inventory).read_text())
    def reject(value):
        if isinstance(value,dict):
            for key,child in value.items():
                if any(word in key.lower() for word in ('password','private_key','token','passphrase')):raise ValueError('no secrets in radio inventory')
                reject(child)
        elif isinstance(value,list):
            for child in value:reject(child)
    reject(data)
    if not re.fullmatch('[a-f0-9]{32}',data['trial']):raise ValueError('fresh trial nonce required')
    for key in ('source_commit','lite_source_commit'):
        if not re.fullmatch('[a-f0-9]{40}',data[key]):raise ValueError('pin actual source revisions')
    tenant=data['tenant']
    if not re.fullmatch(UUID,tenant):raise ValueError('exact reserved tenant UUID required')
    operator=data['operator']
    for key in ('identity','owner','os_version','controller_identity','permission_record'):line(operator[key])
    if operator['platform']!='darwin' or type(operator['uid'])!=int or operator['uid']<501:raise ValueError('reserved native Mac account required')
    profile=Path(line(operator['profile_directory']))
    if not profile.is_absolute() or '..' in profile.parts:raise ValueError('absolute reserved CLI profile required')
    ids=set()
    for role in ('borrower','donor','lite'):
        node=data['nodes'][role]
        for key in ('serial','owner','board','os_version','controller_identity','controller_firmware','management','management_interface','recovery_record'):line(node[key])
        if not re.fullmatch(UUID,node['device_id']) or node['device_id'] in ids:raise ValueError('three distinct device identities required')
        ids.add(node['device_id'])
        if node.get('tenant')!=tenant:raise ValueError('all peers must share the reserved tenant')
        if role=='lite':
            if node.get('profile')!='c5-psram-native-operator-router':raise ValueError('use supported native C5 PSRAM router, not C6 entry')
            if not node['management'].startswith('wendy-lite:'):raise ValueError('Lite requires its independent USB management selector')
        else:
            host,sep,port=node['management'].rpartition(':')
            if not sep or not port.isdecimal() or not 1<=int(port)<=65535:raise ValueError('explicit wired Linux endpoint required')
            ipaddress.ip_address(host.strip('[]'))
            if not re.fullmatch('hci[0-9]+',node['adapter']):raise ValueError('exact Linux HCI adapter required')
            if not re.fullmatch(r'[A-Za-z0-9_.@-]+\.service',node['agent_unit']):raise ValueError('pin exact reserved Agent systemd unit')
        baseline=node['baseline']
        if {key:baseline.get(key) for key in ('participate','mode','advertising','connections')}!={'participate':False,'mode':'normal','advertising':'default','connections':'default'}:raise ValueError('reserved idle/default baseline required; do not overwrite an existing topology')
        flags=('ble','nan','infrastructure-wifi') if role=='lite' else ('ble','nan','ethernet','infrastructure-wifi','roam','share-uplink')
        if set(baseline.get('settings',{}))!=set(flags) or any(type(value)!=bool for value in baseline['settings'].values()):raise ValueError('exact saved carrier/sharing flags required for restoration')
    if data['nodes']['borrower'].get('controller_usb_id')!='13d3:3549':raise ValueError('2527 requires the original Realtek USB controller class')
    if data['nodes']['donor'].get('coded_phy_supported') is not True or data['nodes']['lite'].get('coded_phy_supported') is not True:raise ValueError('2603 requires independently qualified coded-PHY peers')
    power=data['approved_power_dbm']
    if type(power)!=int or not -127<=power<=20:raise ValueError('explicit approved numeric power required; no implicit maximum')
    url=urlsplit(line(data['traffic_url']))
    if url.scheme!='https' or not url.hostname or url.username or url.password or url.fragment:raise ValueError('owner-controlled HTTPS traffic endpoint required')
    if parse_qs(url.query).get('trial')!=[data['trial']]:raise ValueError('traffic challenge must bind the independent trial nonce')
    if not re.fullmatch('[a-f0-9]{64}',data['traffic_sha256']):raise ValueError('independent exact traffic body hash required')
    recordkeys=('cli_binary','agent_binary','lite_firmware','identity_record','authorization_record','topology_record','native_ap_record','recovery_record','refusal_qualification_record','slow_link_qualification_record','coded_phy_qualification_record','operator_profile_record','baseline_record','build_record','permission_record','traffic_ca','traffic_record')
    records={key:artifact(data[key]) for key in recordkeys}
    traffic_ca=Path(line(data['traffic_ca_on_borrower']))
    if not traffic_ca.is_absolute() or '..' in traffic_ca.parts:raise ValueError('pin an absolute borrower-console CA path')
    out=Path(output).resolve();out.mkdir(mode=0o700,parents=True,exist_ok=False)
    cli=records['cli_binary']['path']; guardian=ROOT/'.github/scripts/run-owned-media-test.py'
    prefix=['env','WENDY_CONFIG_DIR='+str(profile),'WENDY_SECRET_STORE=file',cli,'--json']
    commands={}
    def add(name,args,target=None):
        argv=prefix+(['--device',target] if target else [])+args
        commands[name]={'host':operator['identity'],'timeout_seconds':60,'argv':argv,'shell':shlex.join(['python3',str(guardian),'--timeout','60','--']+argv)}
    for role in ('borrower','donor','lite'):
        node=data['nodes'][role];target=node['management']
        for name,args in [('info',['device','info']),('status',['net','status']),('topology',['net','topology']),('mode',['device','bluetooth','mode']),('power',['device','bluetooth','power'])]:add(role+'-'+name,args,target)
        add(role+'-ble-only',['net','configure','--participate=true','--ble=true','--nan=false','--infrastructure-wifi=false']+([] if role=='lite' else ['--ethernet=false','--roam='+str(role=='borrower').lower(),'--share-uplink='+str(role=='donor').lower()]),target)
        add(role+'-off',['net','configure','--participate=false'],target)
        add(role+'-restore-net',['net','configure','--participate=false']+['--'+flag+'='+str(value).lower() for flag,value in node['baseline']['settings'].items()],target)
        add(role+'-normal',['device','bluetooth','mode','normal'],target)
        add(role+'-power-default',['device','bluetooth','power','--advertising=default','--connections=default'],target)
        add(role+'-mesh-info',['device','info'],'mesh://'+tenant+'/'+node['device_id'])
    for role in ('donor','lite'):
        for mode in ('coded-s2','coded-s8'):add(role+'-'+mode,['device','bluetooth','mode',mode],data['nodes'][role]['management'])
    add('donor-approved-power',['device','bluetooth','power','--advertising='+str(power),'--connections='+str(power)],data['nodes']['donor']['management'])
    add('lite-wifi-and-ble',['net','configure','--infrastructure-wifi=true'],data['nodes']['lite']['management'])
    add('lite-wifi-off',['net','configure','--infrastructure-wifi=false'],data['nodes']['lite']['management'])
    add('donor-no-sharing',['net','configure','--share-uplink=false'],data['nodes']['donor']['management'])
    add('donor-lan-and-ble',['net','configure','--ethernet=true'],data['nodes']['donor']['management'])
    add('borrower-lan-and-ble',['net','configure','--ethernet=true'],data['nodes']['borrower']['management'])
    add('operator-connect',['net','connect']);add('operator-disconnect',['net','disconnect'])
    add('operator-topology',['net','topology','--local-cli','--observation'])
    add('operator-discovery',['discover','--type','mesh','--timeout','5s'])
    (out/'commands.json').write_text(json.dumps(commands,indent=2)+'\n')
    guide=Path(__file__).with_suffix('.txt').read_text().replace('@TRIAL@',data['trial'])
    guide+='\nExact Mac commands (execute only after the stated identity/ownership gates):\n'
    guide+='\n'.join(name+':\n'+item['shell'] for name,item in commands.items())+'\n'
    guide+='\nOn each selected Linux console only, bounded adapter trace (replace OUT with a fresh console-owned evidence file):\n'
    for role in ('borrower','donor'):
        guide+=role+': '+shlex.join(['timeout','--signal=INT','--kill-after=3s','180s','btmon','--index',data['nodes'][role]['adapter'][3:],'--write','OUT'])+'\n'
        guide+=role+' journal: '+shlex.join(['timeout','--signal=INT','--kill-after=3s','180s','journalctl','--unit',data['nodes'][role]['agent_unit'],'--since','now','--follow','--output','short-monotonic'])+'\n'
    guide+='\nOn the borrower console, after route/lease admission, exact external body challenge (OUTPUT must be a fresh private filename):\n'
    guide+=shlex.join(['curl','--noproxy','*','--proto','=https','--max-time','20','--fail','--cacert',str(traffic_ca),data['traffic_url'],'--output','OUTPUT'])+'\nExpected SHA256: '+data['traffic_sha256']+'\n'
    (out/'procedure.txt').write_text(guide)
    paths=[Path(__file__),Path(__file__).with_suffix('.txt'),guardian,Path(__file__).with_name('ble-transport-rehearsal.py')]
    paths += [ROOT/name for name in ('go/internal/agent/bleprovider/provider_linux.go','go/internal/agent/bleprovider/conninterval_linux.go','go/internal/agent/meshsharing/control.go','go/internal/agent/meshsharing/controller.go','go/internal/agent/btradio/controller_linux.go','go/internal/agent/btradio/policy.go','go/internal/cli/commands/bluetooth_radio.go','go/internal/cli/commands/device_local_mesh.go','go/internal/cli/sessionbroker/operator.go','go/internal/cli/operatormesh/carriers.go')]
    evidence={'audit_entries':['2669','2576','2524','2527','2603'],'hardware_execution':'not_run','inventory':data,'inventory_record':artifact(str(Path(inventory).resolve())),'records':records,'sources':[artifact(str(p)) for p in paths],'generated':[artifact(str(out/p)) for p in ('commands.json','procedure.txt')],'cases':[{'id':name,'status':'not_run','observations':[]} for name in ('2527_refused_interval_live_acl','2524_slow_roaming_lease','2576_native_carrier_ownership','2669_native_mac_restart_discovery','2603_profile_tune_serialization','restoration')],'limits':['No physical result is inferred from generated commands or synthetic HCI fixtures.','A run without the exact refusal, long handshake, crossed carriers or overlapping profile/tune stimulus is inconclusive.','Historical controls require separately reviewed isolated builds and owner-qualified restoration; no automated install/downgrade is generated.']}
    (out/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n');return evidence

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('inventory',type=Path);parser.add_argument('output',type=Path)
    args=parser.parse_args();generate(args.inventory,args.output)
