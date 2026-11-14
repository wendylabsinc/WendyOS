#!/usr/bin/env python3
"""Explicit owner action for a generated reserved-Mac enrollment plan."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import plistlib
import pwd
import subprocess
import sys
import urllib.parse
import urllib.request


def command(argv):
    return subprocess.check_output(argv, timeout=15, stderr=subprocess.STDOUT).decode().strip()


def artifact(record):
    path=Path(record['path'])
    if path.is_symlink() or not path.is_file() or path.stat().st_size==0:
        raise ValueError('artifact changed: '+str(path))
    digest=hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda:stream.read(1024*1024),b''):digest.update(chunk)
    if digest.hexdigest()!=record['sha256']:raise ValueError('artifact hash changed: '+str(path))
    return str(path)


def local_url(value):
    url=urllib.parse.urlsplit(value)
    if url.scheme!='http' or url.hostname!='127.0.0.1' or not url.port or url.username or url.password or url.query or url.fragment:
        raise ValueError('only the disposable loopback HTTP peer is allowed')
    return value


def listener_pid(text, port, uid):
    rows=text.splitlines()
    pids={row[1:] for row in rows if row.startswith('p')}
    users={row[1:] for row in rows if row.startswith('u')}
    addresses={row[1:] for row in rows if row.startswith('n')}
    allowed={'127.0.0.1:'+str(port),'[::1]:'+str(port)}
    if len(pids)!=1 or users!={str(uid)} or not addresses or not addresses<=allowed:
        raise ValueError('ambiguous, non-loopback or foreign Agent listener')
    return pids.pop()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def local_status(address):
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
    with opener.open(local_url(address),timeout=3) as response:
        if response.status!=200:raise ValueError('unexpected loopback status response')
        raw=response.read(65537)
    if len(raw)>65536:raise ValueError('oversized loopback status response')
    return json.loads(raw)


def admit(plan, port):
    if sys.platform!='darwin' or os.getuid()!=plan['mac']['uid'] or pwd.getpwuid(os.getuid()).pw_name!=plan['mac']['account']:
        raise ValueError('wrong reserved Mac account')
    if command(['/usr/bin/sw_vers','-buildVersion'])!=plan['mac']['os_build']:raise ValueError('OS build changed')
    hardware=plistlib.loads(subprocess.check_output(['/usr/sbin/ioreg','-rd1','-c','IOPlatformExpertDevice','-a'],timeout=15))[0]
    if hardware.get('IOPlatformSerialNumber')!=plan['mac']['serial'] or hardware.get('IOPlatformUUID')!=plan['mac']['hardware_uuid']:
        raise ValueError('wrong reserved physical Mac')
    for record in (plan['cli'],plan['peer'],plan['candidate']):artifact(record)
    installed=Path(plan['installed'])
    binary=installed/'Contents/MacOS/WendyAgentMac'
    record={**plan['candidate'],'path':str(binary)}
    artifact(record)
    command(['/usr/bin/codesign','--verify','--deep','--strict',str(installed)])
    # Require one listener owned by this account and the exact signed app path.
    pid=listener_pid(command(['/usr/sbin/lsof','-nP','-iTCP:'+str(port),'-sTCP:LISTEN','-Fpun']),port,os.getuid())
    if command(['/bin/ps','-p',pid,'-o','comm='])!=str(binary):raise ValueError('listener is not the pinned signed app')
    return pid


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('plan',type=Path);parser.add_argument('action',choices=('peer','enroll','apps','status'))
    args=parser.parse_args();plan=json.loads(args.plan.read_text())
    port=plan['port']+(args.action in ('apps','status'))
    pid=admit(plan,port)
    root=Path(plan['output']);peer=root/'enrollment-peer'
    environment={'PATH':'/usr/bin:/bin:/usr/sbin:/sbin','HOME':str(root/'enrollment-home'),'WENDY_CONFIG_DIR':str(peer/'cli'),'WENDY_SECRET_STORE':'file','WENDY_CLOUD_INSECURE':'true','WENDY_ANALYTICS':'false','WENDY_NO_UPDATE_CHECK':'1','WENDY_TLS_SESSION_STORE':'off','CI':'1','TERM':'dumb'}
    state_path=Path('/Users')/plan['mac']['account']/'Library/Application Support/sh.wendy.WendyAgentMac/provisioning.json'
    if args.action in ('peer','enroll') and state_path.exists():
        raise ValueError('fresh reserved account must not already have a provisioning identity')
    if args.action=='peer':
        if peer.exists():raise ValueError('peer requires a fresh plan directory')
        os.execve(plan['peer']['path'],[plan['peer']['path'],str(peer)],environment)
    ready_path=peer/'ready.json'
    info=ready_path.lstat()
    if ready_path.is_symlink() or info.st_uid!=os.getuid() or info.st_mode&0o777!=0o600 or info.st_size>128*1024:
        raise ValueError('invalid private peer receipt')
    ready=json.loads(ready_path.read_text())
    local_url(ready['directory']);local_url(ready['status'])
    if not ready['cloud'].startswith('127.0.0.1:'):raise ValueError('foreign Cloud endpoint')
    if args.action=='status':
        counters=local_status(ready['status'])
        if any(counters.get(name)!=1 for name in ('enrolled','accounts','orders','finalizes')) or counters.get('errors'):
            raise ValueError('incomplete/repeated enrollment or peer errors')
        if state_path.is_symlink() or state_path.stat().st_uid!=os.getuid() or state_path.stat().st_size>1024*1024:raise ValueError('foreign Agent state')
        state=json.loads(state_path.read_text())
        expected='spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/'+counters['device']
        if state.get('enrolled') is not True or state.get('principalURI')!=expected or state.get('cloudHost')!=ready['cloud'] or not state.get('certPem'):
            raise ValueError('packaged Agent did not persist the minted identity')
        print(json.dumps({'agent_pid':pid,'principal':expected,'certificate_sha256':hashlib.sha256(state['certPem'].encode()).hexdigest(),'key_backing':state.get('keyBacking'),'counts':{name:counters[name] for name in ('enrolled','accounts','orders','finalizes')},'candidate_sha256':plan['candidate']['sha256']}))
        return
    argv=[plan['cli']['path'],'--device','127.0.0.1:'+str(port),'--json','device']
    argv+=['enroll','--name','mac-enrollment-fixture','--acme-directory-url',ready['directory']] if args.action=='enroll' else ['apps','list']
    os.execve(argv[0],argv,environment)


if __name__=='__main__':main()
