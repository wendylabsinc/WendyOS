#!/usr/bin/env python3
"""Export one existing passive report via bounded public Wendy MCP requests.

No capture, runtime HTTP, motion, admin interface or credential access occurs.
The complete digest is checked before creating the requested local output file.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import time

APP = 'com.wendy.world.go2.cloud-shadow'
MAX_REPORT = 48 << 20
CHUNK = 128 << 10
MAX_STDERR = 64 << 10


def require(ok, reason):
    if not ok: raise ValueError(reason)


def value(result):
    require(not result.get('isError'), 'MCP tool refused')
    if result.get('structuredContent') is not None: return result['structuredContent']
    for item in result.get('content', []):
        if item.get('type') == 'text':
            try: return json.loads(item['text'])
            except ValueError: pass
    raise ValueError('missing structured result')


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--device',required=True)
    parser.add_argument('--capture',required=True)
    parser.add_argument('--out',required=True,type=Path)
    args=parser.parse_args(argv)
    require(re.fullmatch(r'vm:[a-zA-Z0-9_-]{1,64}',args.device), 'explicit VM selector required')
    require(re.fullmatch(r'[a-z0-9][a-z0-9-]{0,63}',args.capture), 'capture name')
    out=args.out.absolute()
    require(not out.exists() and not out.is_symlink(), 'new output required')
    require(all(p.is_dir() and not p.is_symlink() for p in out.parents), 'regular output parents required')
    stderr=out.with_suffix(out.suffix+'.mcp-stderr')
    receipt_path=out.with_suffix(out.suffix+'.transfer.json')
    require(not receipt_path.exists() and not receipt_path.is_symlink(), 'new transfer receipt required')
    deadline=time.monotonic()+60
    process=None;selector=selectors.DefaultSelector();pending=b'';total=0;identifier=0;stderr_bytes=0
    receipts=[]
    try:
        with stderr.open('xb') as error:
            process=subprocess.Popen(['wendy','mcp','serve','--device',args.device],stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
            selector.register(process.stdout,selectors.EVENT_READ,'stdout')
            selector.register(process.stderr,selectors.EVENT_READ,'stderr')
            def request(method,params):
                nonlocal pending,total,identifier,stderr_bytes
                identifier+=1
                require(time.monotonic()<deadline,'export deadline')
                process.stdin.write((json.dumps({'jsonrpc':'2.0','id':identifier,'method':method,'params':params})+'\n').encode())
                process.stdin.flush()
                while True:
                    require(time.monotonic()<deadline,'export deadline')
                    if b'\n' in pending:
                        line,pending=pending.split(b'\n',1)
                        require(len(line)<=1<<20,'MCP row bound')
                        message=json.loads(line)
                        if message.get('id')!=identifier:continue
                        require('error' not in message,'MCP request failed')
                        return message['result']
                    require(len(pending)<=1<<20,'MCP row bound')
                    selected=selector.select(max(0,deadline-time.monotonic()))
                    require(selected,'export deadline')
                    for key,_ in selected:
                        raw=os.read(key.fileobj.fileno(),65536)
                        if key.data=='stderr':
                            if not raw:
                                selector.unregister(key.fileobj);continue
                            error.write(raw[:max(0,MAX_STDERR-stderr_bytes)])
                            stderr_bytes+=len(raw)
                            require(stderr_bytes<=MAX_STDERR,'MCP stderr bound')
                        else:
                            require(raw,'MCP closed before response')
                            total+=len(raw);require(total<=160<<20,'MCP total bound');pending+=raw
            request('initialize',{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'world-cloud-shadow-export','version':'1'}})
            process.stdin.write(b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n');process.stdin.flush()
            def call(name,arguments):return value(request('tools/call',{'name':name,'arguments':arguments}))
            status=call('wendy_status',{})
            ready_deadline=min(deadline-10,time.monotonic()+5)
            while not status.get('connected') and time.monotonic()<ready_deadline:
                time.sleep(.1);status=call('wendy_status',{})
            require(status.get('connected') and status.get('command_target',{}).get('device')==args.device,'wrong or missing VM connection')
            def remote(offset=None):
                command=['python3','/opt/world/cloud-shadow-launch.py','export','--capture',args.capture]
                if offset is not None:command.extend(['--offset',str(offset)])
                result=call('container_exec',{'app_name':APP,'command':command,'max_bytes':250000,'timeout_seconds':5})
                require(result.get('exit_code')==0 and not result.get('truncated'),'incomplete report transfer')
                return json.loads(result['stdout'])
            first=remote()
            require(type(first['bytes']) is int and 0<first['bytes']<=MAX_REPORT
                    and re.fullmatch('[0-9a-f]{64}',first['sha256']), 'report metadata')
            data=bytearray()
            while len(data)<first['bytes']:
                offset=len(data);part=remote(offset)
                raw=base64.b64decode(part['base64'],validate=True)
                require(part['identity']==first['identity'] and part['offset']==offset
                        and part['bytes']==len(raw)==min(CHUNK,first['bytes']-offset)
                        and hashlib.sha256(raw).hexdigest()==part['sha256'],'chunk binding')
                receipts.append({k:v for k,v in part.items() if k!='base64'})
                data.extend(raw)
            final=remote()
            require(final==first and hashlib.sha256(data).hexdigest()==first['sha256'],'complete report binding')
            require(time.monotonic()<deadline,'export deadline')
            fd=os.open(out,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
            with os.fdopen(fd,'wb') as target:target.write(data)
            receipt={'schema':'wendy.world.go2-cloud-shadow-export.v1','device':args.device,'app':APP,
                'capture':args.capture,'metadata':first,'chunks':receipts,'public_mcp_only':True,
                'capture_started':False,'operational_authority':False,'stderr_captured_bytes':stderr_bytes}
            with receipt_path.open('x') as target:target.write(json.dumps(receipt,sort_keys=True,indent=2)+'\n')
            print(json.dumps({'bytes':len(data),'sha256':first['sha256'],'chunks':len(receipts),'exported':str(out)}))
    finally:
        selector.close()
        if process is not None:
            if process.poll() is None:
                os.killpg(process.pid,signal.SIGTERM)
                try:process.wait(timeout=1)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid,signal.SIGKILL);process.wait(timeout=1)
            for stream in (process.stdin,process.stdout,process.stderr):
                if stream is not None:stream.close()


if __name__=='__main__':main()
