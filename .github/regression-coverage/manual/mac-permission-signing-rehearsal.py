#!/usr/bin/env python3
"""Prepare reserved-Mac Location and signed-extension acceptance without running it."""
import argparse
import hashlib
import importlib.util
import ipaddress
import json
from pathlib import Path
import plistlib
import re
import shlex
import zipfile

ROOT=Path(__file__).resolve().parents[3]
def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
LIFECYCLE=load('permission_lifecycle',Path(__file__).with_name('mac-agent-lifecycle-rehearsal.py'))
COMMON=LIFECYCLE.COMMON
VALIDATOR=load('permission_provisioning',ROOT/'swift/Scripts/ValidateMacProvisioning.py')
LABEL='sh.wendy.WendyAgentMac';EXT=LABEL+'.NetProxy'

def extension_artifact(bundle):
    root=Path(bundle['bundle']['path'])
    prefix='Contents/Library/SystemExtensions/'+EXT+'.systemextension/Contents/'
    info=plistlib.loads((root/(prefix+'Info.plist')).read_bytes())
    if info.get('CFBundleIdentifier')!=EXT:raise ValueError('wrong actual system-extension bundle')
    executable=info['CFBundleExecutable']
    if not re.fullmatch('[A-Za-z0-9_.-]+',executable):raise ValueError('invalid extension executable component')
    paths=('Contents/embedded.provisionprofile',prefix+'embedded.provisionprofile',prefix+'Info.plist',prefix+'MacOS/'+executable)
    with zipfile.ZipFile(bundle['archive']['path']) as source:
        top=next(name.split('/')[0] for name in source.namelist() if name.endswith('/Contents/Info.plist'))
        for relative in paths:
            entry=source.getinfo(top+'/'+relative)
            if entry.file_size>512*1024**2:raise ValueError('oversized extension archive member')
            digest=hashlib.sha256()
            with source.open(entry) as stream:
                for chunk in iter(lambda:stream.read(1024*1024),b''):digest.update(chunk)
            if digest.hexdigest()!=COMMON.artifact(root/relative)['sha256']:raise ValueError('ZIP and extension/profile differ: '+relative)
    return info,COMMON.artifact(root/(prefix+'MacOS/'+executable))


def generate(inventory,output):
    data=json.loads(Path(inventory).read_text())
    def reject(value):
        if isinstance(value,dict):
            for key,child in value.items():
                if any(word in key.lower() for word in ('password','private_key','token','passphrase')):raise ValueError('no credentials in rehearsal inventory')
                reject(child)
        elif isinstance(value,list):
            for child in value:reject(child)
    reject(data)
    if not re.fullmatch('[a-f0-9]{32}',data['trial']) or not re.fullmatch('[a-f0-9]{40}',data['source_commit']):raise ValueError('pin fresh trial and full source revision')
    mac=data['mac']
    for key in ('serial','hardware_uuid','os_build','owner','account'):COMMON.line(mac[key],key)
    if not re.fullmatch('[a-z][a-z0-9_]{1,30}',mac['account']) or type(mac['uid'])!=int or mac['uid']<501:raise ValueError('reserved ordinary account required')
    if data.get('reserved_machine_empty') is not True or data.get('reserved_account_empty') is not True:raise ValueError('extension acceptance requires the whole reserved Mac, not only an account')
    if data['installed_bundle']!='/Applications/WendyAgentMac.app':raise ValueError('system-extension host must use the reserved /Applications location')
    host,port=data['endpoint'].rsplit(':',1)
    if not ipaddress.ip_address(host).is_loopback or not port.isdecimal() or not 1<=int(port)<=65535:raise ValueError('exact loopback management endpoint required')
    cli=COMMON.artifact(data['cli_binary'])
    candidate=LIFECYCLE.bundle(data['candidate_bundle'],data['candidate_zip'])
    app=Path(candidate['bundle']['path']);extension=app/'Contents/Library/SystemExtensions'/f'{EXT}.systemextension'
    info=plistlib.loads((app/'Contents/Info.plist').read_bytes())
    if not isinstance(info.get('NSLocationWhenInUseUsageDescription'),str) or not info['NSLocationWhenInUseUsageDescription'].strip():raise ValueError('candidate lacks actual Location usage description')
    extension_info,extension_binary=extension_artifact(candidate)
    profiles=[COMMON.artifact(app/'Contents/embedded.provisionprofile'),COMMON.artifact(extension/'Contents/embedded.provisionprofile')]
    records={name:COMMON.artifact(data['records'][name]) for name in ('identity','empty_machine','empty_account','authorization','recovery','candidate_build','notarization','signer_der','host_entitlements','extension_entitlements','extension_peer','reserved_cloud_profile')}
    signer=hashlib.sha1(Path(records['signer_der']['path']).read_bytes()).hexdigest().upper()
    for kind,key in [('host','host_entitlements'),('net-proxy','extension_entitlements')]:
        VALIDATOR.validate_capabilities(plistlib.loads(Path(records[key]['path']).read_bytes()),kind=kind,exact_network=True)
    source=ROOT/'swift/WendyAgentMac/Sources/AppDelegate.swift';text=source.read_text()
    gate='private let meshVPNIsEnabled = false'
    if text.count(gate)!=1:raise ValueError('shipped extension applicability changed; review generator')
    out=COMMON.new_output(output)
    profile=out/'cli-profile';profile.mkdir(mode=0o700)
    overlay=out/'validation-AppDelegate.swift';overlay.write_text(text.replace(gate,'private let meshVPNIsEnabled = true'))
    commands={
        'local-info':['env','WENDY_CONFIG_DIR='+str(profile),'WENDY_SECRET_STORE=file','WENDY_ANALYTICS=false','WENDY_NO_UPDATE_CHECK=1',cli['path'],'--json','--device',data['endpoint'],'device','info'],
        'release-validation':['bash',str(ROOT/'swift/Scripts/ValidateMacRelease.sh'),str(app),signer],
        'gatekeeper':['/usr/sbin/spctl','--assess','--type','execute','--verbose=4',str(app)],
        'staple':['xcrun','stapler','validate',str(app)],
        'installed-signature':['/usr/bin/codesign','--verify','--deep','--strict','--verbose=2',data['installed_bundle']],
        'open-installed':['/usr/bin/open','-n',data['installed_bundle']],
        'extensions':['/usr/bin/systemextensionsctl','list'],
        'vpn-state':['/usr/sbin/scutil','--nc','list'],
        'events':['/usr/bin/log','show','--last','10m','--style','json','--predicate','subsystem == "sh.wendy.WendyAgentMac" OR process == "sysextd"'],
    }
    validation=None
    if 'validation_bundle' in data:
        validation=LIFECYCLE.bundle(data['validation_bundle'],data['validation_zip'])
        if validation['executable']['sha256']==candidate['executable']['sha256']:raise ValueError('validation build must be distinct from shipped candidate')
        validation['build_record']=COMMON.artifact(data['records']['validation_build'])
        validation['source_overlay']=COMMON.artifact(overlay)
        build=json.loads(Path(validation['build_record']['path']).read_text())
        if build.get('base_source_commit')!=data['source_commit'] or build.get('overlay_sha256')!=validation['source_overlay']['sha256']:raise ValueError('validation build record does not bind the exact reviewed overlay/base')
        validation_app=Path(validation['bundle']['path'])
        validation_info=plistlib.loads((validation_app/'Contents/Info.plist').read_bytes())
        validation_extension,_=extension_artifact(validation)
        if validation_info.get('CFBundleVersion')==info.get('CFBundleVersion') or validation_extension.get('CFBundleVersion')==extension_info.get('CFBundleVersion'):raise ValueError('validation host and extension require reviewed distinct bundle versions')
        commands['validation-release-check']=['bash',str(ROOT/'swift/Scripts/ValidateMacRelease.sh'),str(validation_app),signer]
        commands['validation-gatekeeper']=['/usr/sbin/spctl','--assess','--type','execute','--verbose=4',str(validation_app)]
        commands['validation-staple']=['xcrun','stapler','validate',str(validation_app)]
    guide=Path(__file__).with_suffix('.txt').read_text()
    for key,value in {'@TRIAL@':data['trial'],'@SERIAL@':mac['serial'],'@ACCOUNT@':mac['account'],'@UID@':str(mac['uid']),'@TEAM@':VALIDATOR.TEAM_IDENTIFIER,'@SIGNER@':signer,'@CANDIDATE@':candidate['executable']['sha256'],'@OUT@':str(out)}.items():guide=guide.replace(key,value)
    guardian=ROOT/'.github/scripts/run-owned-media-test.py'
    guide+='\nExact commands; execute only after physical identity/ownership gates:\n'
    guide+='\n'.join(name+':\n'+shlex.join(['python3',str(guardian),'--timeout','60','--']+argv) for name,argv in commands.items())+'\n'
    (out/'commands.json').write_text(json.dumps(commands,indent=2)+'\n');(out/'procedure.txt').write_text(guide)
    paths=[Path(__file__),Path(__file__).with_suffix('.txt'),Path(__file__).with_name('mac-agent-lifecycle-rehearsal.py'),Path(__file__).with_name('mac-camera-rehearsal.py'),guardian]
    paths += [ROOT/p for p in ('swift/WendyAgentMac/Sources/WelcomeAndPermissions.swift','swift/WendyAgentMac/Sources/LocationPermissionRequest.swift','swift/WendyAgentMac/Sources/WelcomeAndPermissionsView.swift','swift/WendyAgentMac/Sources/AppDelegate.swift','swift/WendyAgentMac/Sources/MeshSystemExtensionInstaller.swift','swift/WendyAgentMac/Sources/MeshVPNController.swift','swift/Scripts/Build.sh','swift/Scripts/ValidateMacRelease.sh','swift/Scripts/ValidateMacProvisioning.py','go/internal/cli/commands/device.go')]
    evidence={'audit_entries':['2243','2004','2189'],'hardware_execution':'not_run','inventory':data,'inventory_record':COMMON.artifact(inventory),'cli':cli,'candidate':candidate,'extension_binary':extension_binary,'embedded_profiles':profiles,'signer_sha1':signer,'records':records,'validation_build':validation,'sources':[COMMON.artifact(p) for p in paths],'generated':[COMMON.artifact(out/p) for p in ('commands.json','procedure.txt','validation-AppDelegate.swift')],'cases':[{'id':case,'status':'not_run','observations':[]} for case in ('location_unanswered','location_denied','location_settings_recovery','signed_release_launch','shipped_extension_disabled','instrumented_extension_approval_loading','restoration')],'limits':['Shipped meshVPNIsEnabled is false; instrumented extension loading cannot establish shipped enabled-path acceptance.','No TCC reset, security weakening, provisioning, signing, installation or OS dialog is performed by generation.','No validation bundle means extension loading remains blocked on a reviewed separately signed build.']}
    (out/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n');return evidence
if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('inventory',type=Path);parser.add_argument('output',type=Path)
    args=parser.parse_args();generate(args.inventory,args.output)
