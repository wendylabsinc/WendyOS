#!/usr/bin/env python3
"""Launch the staged ordinary-app core path in one owned existing-VM runtime.

This is a simulation qualification. It creates no VM, controls no existing
simulator and never mounts a host runtime socket or host network namespace.
"""
import argparse
import hashlib
import importlib.util
import io
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import tarfile
import threading
import time
import uuid

import configs
import stage
import validation

HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location("agentvm_base",HERE.parent/"agentvm/verify.py")
base=importlib.util.module_from_spec(spec);spec.loader.exec_module(base)
REMOTE="/var/lib/agentmultimodal"
LABEL="wendy.world.agentmultimodal"
MAX_LOG=32<<20


CREATE_STAGE = r'''# stage:create-private-root
import os,pathlib,stat,sys
p=pathlib.PurePosixPath(sys.argv[1]);assert p.is_absolute() and str(p)==sys.argv[1] and '..' not in p.parts
fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
try:
 for part in p.parts[1:-1]:
  st=os.fstat(fd);assert st.st_uid==0 and not st.st_mode&0o022
  child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd);os.close(fd);fd=child
 st=os.fstat(fd);assert st.st_uid==0 and not st.st_mode&0o022
 os.mkdir(p.name,0o700,dir_fd=fd)
 child=os.open(p.name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
 try:
  st=os.fstat(child);assert st.st_uid==0 and stat.S_IMODE(st.st_mode)==0o700 and not os.listdir(child)
 finally:os.close(child)
finally:os.close(fd)
print('fresh private stage created')
'''

VERIFY_STAGE = r'''# stage:verify-extracted-tree
import hashlib,json,os,pathlib,stat,sys
p=pathlib.PurePosixPath(sys.argv[1]);assert p.is_absolute() and str(p)==sys.argv[1] and '..' not in p.parts
fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
try:
 for part in p.parts[1:]:
  st=os.fstat(fd);assert st.st_uid==0 and not st.st_mode&0o022
  child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd);os.close(fd);fd=child
 st=os.fstat(fd);assert st.st_uid==0 and stat.S_IMODE(st.st_mode)==0o700
 index=os.open('files.json',os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=fd)
 with os.fdopen(index,'rb') as f:
  st=os.fstat(f.fileno());assert stat.S_ISREG(st.st_mode) and st.st_uid==0 and st.st_nlink==1 and stat.S_IMODE(st.st_mode)==0o600 and st.st_size<=1<<20
  raw=f.read((1<<20)+1);assert len(raw)<=1<<20 and hashlib.sha256(raw).hexdigest()==sys.argv[2]
 manifest=json.loads(raw);assert set(manifest)=={'version','files'} and manifest['version']==1
 expected=manifest['files'];assert 'files.json' not in expected
 directories=set()
 for name in expected:
  rel=pathlib.PurePosixPath(name);assert not rel.is_absolute() and str(rel)==name and all(x not in ('','.','..') for x in name.split('/')) and '\\' not in name
  directories.update(str(parent) for parent in rel.parents if str(parent)!='.')
 assert not directories.intersection(expected)
 expected['files.json']={'sha256':sys.argv[2],'size':len(raw),'mode':0o600}
 seen_files=set();seen_dirs=set()
 def walk(directory,prefix):
  for name in os.listdir(directory):
   relative=prefix+name
   st=os.stat(name,dir_fd=directory,follow_symlinks=False)
   if stat.S_ISDIR(st.st_mode):
    assert relative in directories
    child=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=directory)
    try:
     bound=os.fstat(child);assert (bound.st_dev,bound.st_ino)==(st.st_dev,st.st_ino) and bound.st_uid==0 and stat.S_IMODE(bound.st_mode)==0o755
     seen_dirs.add(relative);walk(child,relative+'/')
    finally:os.close(child)
   else:
    assert relative in expected and stat.S_ISREG(st.st_mode)
    target=expected[relative];child=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=directory)
    with os.fdopen(child,'rb') as f:
     bound=os.fstat(f.fileno());assert (bound.st_dev,bound.st_ino)==(st.st_dev,st.st_ino) and stat.S_ISREG(bound.st_mode) and bound.st_uid==0 and bound.st_nlink==1 and not bound.st_mode&0o022
     assert bound.st_size==target['size'] and stat.S_IMODE(bound.st_mode)==target['mode']
     digest=hashlib.sha256();count=0
     while True:
      block=f.read(min(1<<20,target['size']-count+1))
      if not block:break
      count+=len(block);assert count<=target['size'];digest.update(block)
     assert count==target['size'] and os.fstat(f.fileno()).st_size==count and digest.hexdigest()==target['sha256']
    seen_files.add(relative)
 walk(fd,'');assert seen_files==set(expected) and seen_dirs==directories
finally:os.close(fd)
print('stage bytes and protected directory metadata verified')
'''


def upload_inventory(root):
    """Pin file entries and derive directories without importing their modes."""
    raw=stage.regular(root/'files.json',1<<20).read_bytes()
    value=stage.strict_json(raw)
    if set(value)!={'version','files'} or value['version']!=1 or not isinstance(value['files'],dict):
        raise stage.Refused('invalid staged upload inventory')
    files=value['files'];directories=set()
    if not files or 'files.json' in files:raise stage.Refused('invalid staged upload members')
    for name,metadata in files.items():
        if stage.tar_path(name)!=name or any(part in ('','.','..') for part in name.split('/')):
            raise stage.Refused('noncanonical staged upload member')
        if set(metadata)!={'sha256','size','mode'} or not re.fullmatch('[a-f0-9]{64}',metadata['sha256']):
            raise stage.Refused('invalid staged upload metadata')
        if type(metadata['size']) is not int or metadata['size']<0 or type(metadata['mode']) is not int or metadata['mode'] not in (0o600,0o644,0o755):
            raise stage.Refused('unsafe staged upload metadata')
        directories.update(str(parent) for parent in Path(name).parents if str(parent)!='.')
    if directories.intersection(files) or 'files.json' in directories:
        raise stage.Refused('staged file and directory collide')
    actual=set()
    for path in root.rglob('*'):
        mode=path.lstat().st_mode
        if stat.S_ISREG(mode):actual.add(str(path.relative_to(root)))
        elif not stat.S_ISDIR(mode):raise stage.Refused('staged upload contains a link or special file')
    if actual!=set(files)|{'files.json'}:raise stage.Refused('staged upload file membership changed')
    return raw,files,sorted(directories,key=lambda name:(name.count('/'),name))


def write_stage_archive(archive,root,raw,files,directories):
    # GNU tar invents mode0777 parents under docker exec's umask0000 when
    # directory entries are omitted. Supply a fixed safe mode before any file.
    for name in directories:
        info=tarfile.TarInfo(name);info.type=tarfile.DIRTYPE;info.mode=0o755;info.uid=info.gid=0
        archive.addfile(info)
    for name in sorted(set(files)|{'files.json'}):
        metadata=files.get(name,{'size':len(raw),'mode':0o600})
        info=tarfile.TarInfo(name);info.size=metadata['size'];info.mode=metadata['mode'];info.uid=info.gid=0
        if name=='files.json':archive.addfile(info,io.BytesIO(raw));continue
        fd=os.open(root/name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
        with os.fdopen(fd,'rb') as source:
            st=os.fstat(source.fileno())
            if not stat.S_ISREG(st.st_mode) or st.st_size!=info.size or stat.S_IMODE(st.st_mode)!=info.mode or st.st_nlink!=1:
                raise stage.Refused('staged upload file metadata changed')
            archive.addfile(info,source)


def history_options(args):
    names=("history_bundle","history_reader_sha256","history_plan_sha256","history_inventory_sha256")
    values={name:getattr(args,name,None) for name in names}
    if all(value is None for value in values.values()):return None
    if not all(values.values()):raise stage.Refused("history requires its bundle and all three exact pins")
    for name in names[1:]:
        if not re.fullmatch(r"[a-f0-9]{64}",values[name]):raise stage.Refused("history requires lowercase SHA256 pins")
    return values


class InterruptedRun(BaseException):
    def __init__(self, signum):
        self.signum=signum
        super().__init__("runner interrupted by "+signal.Signals(signum).name)


def runtime_args(image,name,network,volume):
    stage.immutable_image(image)
    argv=["docker","run","--detach","--name",name,"--label",LABEL+"="+name,"--cgroupns=private","--network",network,"--read-only","--cap-drop","ALL"]
    for cap in base.CAPS:argv += ["--cap-add",cap]
    argv += ["--security-opt","seccomp=unconfined","--pids-limit","2048","--memory","6g","--cpus","4","--ulimit","nofile=65536:65536"]
    for target,size,mode in (("/run","512m","0755"),("/tmp","512m","1777"),("/etc/wendy-agent","16m","0700"),("/config","16m","0700")):
        argv += ["--tmpfs",target+":rw,nosuid,nodev,size="+size+",mode="+mode]
    return argv+["--mount","type=volume,src="+volume+",dst=/var/lib",image]


class Runner:
    def __init__(self,args):
        self.args=args
        history_args=history_options(args)
        self.source=args.stage.resolve()
        self.plan=stage.validate_stage(self.source)
        self.out=stage.safe_output(args.output)
        self.name="world-multimodal-"+uuid.uuid4().hex[:12]
        self.network=self.name+"-net";self.volume=self.name+"-state"
        self.created=[];self.count=0;self.agent_pid=0;self.scene=None
        self.states={};self.public=None
        self.history=None
        if history_args is not None:
            from history_runtime import HistoryRuntime
            self.history=HistoryRuntime(self,history_args["history_bundle"],reader_sha256=history_args["history_reader_sha256"],baseline_plan_sha256=history_args["history_plan_sha256"],baseline_inventory_sha256=history_args["history_inventory_sha256"])

    def call(self,argv,timeout=60,check=True,input_data=None,max_output=None):
        if not 0<timeout<=300:raise stage.Refused("bounded command timeout required")
        if input_data is not None and len(input_data)>1<<20:raise stage.Refused("bounded command input required")
        maximum=MAX_LOG if max_output is None else max_output
        if not 0<maximum<=(32<<20)+65536:raise stage.Refused("bounded command output required")
        self.count+=1;stem=self.out/f"{self.count:03d}"
        overflow=threading.Event();data={};copy_errors=[]
        process=subprocess.Popen([str(v) for v in argv],stdin=subprocess.PIPE if input_data is not None else subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        def kill_client():
            if process.poll() is not None:return
            try:os.killpg(process.pid,signal.SIGKILL)
            except ProcessLookupError:pass
            except PermissionError:
                # Darwin can report EPERM after the short-lived group exits.
                # Reap/check this exact child before deciding it is an error.
                if process.poll() is None:raise
        def copy(stream,kind):
            chunks=[];size=0
            try:
                with stem.with_suffix("."+kind).open("xb") as log:
                    while True:
                        b=stream.read(65536)
                        if not b:break
                        kept=b[:max(0,maximum-size)];log.write(kept);chunks.append(kept);size+=len(b)
                        if size>maximum:
                            overflow.set()
                            kill_client()
                            break
            except Exception as exc:
                copy_errors.append(type(exc).__name__)
                try:kill_client()
                except OSError as failure:copy_errors.append(type(failure).__name__)
            finally:stream.close()
            data[kind]=b"".join(chunks)
        threads=[threading.Thread(target=copy,args=(process.stdout,"stdout"),daemon=True),threading.Thread(target=copy,args=(process.stderr,"stderr"),daemon=True)]
        for thread in threads:thread.start()
        if input_data is not None:
            # Only small, non-secret configuration payloads use this method.
            try:process.stdin.write(input_data);process.stdin.close()
            except BrokenPipeError:pass
        timed_out=False;interrupted=None
        try:code=process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out=True
            kill_client()
            process.wait(timeout=5);code=124
        except BaseException as exc:
            interrupted=exc
            kill_client()
            process.wait(timeout=5);code=128+getattr(exc,"signum",signal.SIGINT)
        for thread in threads:thread.join(timeout=5)
        if any(thread.is_alive() for thread in threads):copy_errors.append("output_collection_timeout")
        if overflow.is_set():code=125
        if copy_errors:code=126
        stage.write_new(stem.with_suffix(".json"),stage.json_bytes({"argv":[str(x) for x in argv],"exit_code":code,"timeout":timed_out,"output_limited":overflow.is_set(),"interrupted":interrupted is not None,"copy_errors":copy_errors}))
        if interrupted is not None:raise interrupted
        result=subprocess.CompletedProcess(argv,code,data.get("stdout",b""),data.get("stderr",b""))
        if check and code:raise stage.Refused(f"bounded runner step {self.count} failed with exit {code}")
        return result

    def inside(self,*args,timeout=60,check=True):return self.call(["docker","exec",self.name,*args],timeout,check)
    def python(self,code,*args,check=True,timeout=60):return self.inside("python3","-c",code,*map(str,args),check=check,timeout=timeout)
    def cli(self,*args,timeout=300,check=True):
        env=[item for key,value in base.ENV.items() for item in ("-e",key+"="+value)]
        return self.call(["docker","exec",*env,self.name,REMOTE+"/tools/cli","--json",*args],timeout,check)
    def wait(self,condition,seconds=60):
        deadline=time.monotonic()+seconds
        while time.monotonic()<deadline:
            if condition():return
            if self.call(["docker","inspect","--format","{{.State.Running}}",self.name],check=False).stdout.strip()!=b"true":raise stage.Refused("owned runtime exited before readiness")
            time.sleep(.25)
        raise stage.Refused("bounded readiness wait expired")

    def upload(self):
        raw,files,directories=upload_inventory(self.source)
        self.python(CREATE_STAGE,REMOTE)
        # Stream only the verified stage inventory. A 486MB rootfs never needs
        # a host bind mount or one giant in-memory tar buffer.
        self.count+=1;stem=self.out/f"{self.count:03d}"
        command=["docker","exec","-i",self.name,"tar","--no-same-owner","-xf","-","-C",REMOTE]
        with stem.with_suffix(".stdout").open("xb") as stdout,stem.with_suffix(".stderr").open("xb") as stderr:
            process=subprocess.Popen(command,stdin=subprocess.PIPE,stdout=stdout,stderr=stderr,start_new_session=True)
            timer=threading.Timer(120,lambda:process.kill() if process.poll() is None else None);timer.start()
            try:
                with tarfile.open(fileobj=process.stdin,mode="w|") as archive:
                    write_stage_archive(archive,self.source,raw,files,directories)
                process.stdin.close();code=process.wait(timeout=10)
            finally:
                timer.cancel()
                if process.poll() is None:process.kill();process.wait(timeout=5)
        stage.write_new(stem.with_suffix(".json"),stage.json_bytes({"argv":command,"exit_code":code}))
        if code:raise stage.Refused("streamed private stage upload failed")
        self.python(VERIFY_STAGE,REMOTE,hashlib.sha256(raw).hexdigest())

    def start_agent(self,phase):
        self.inside("mkdir","-p","/var/lib/agentvm/logs")
        environment=["-e","WENDY_CONFIG_PATH=/etc/wendy-agent","-e","WENDY_CONTAINERD_ADDR=/run/containerd/containerd.sock"]
        if getattr(self.args,"trace_rpc",False):environment += ["-e","WENDY_WORLD_RPC_TIMING=1"]
        self.call(["docker","exec","-d",*environment,self.name,"sh","-c",'echo $$ > /run/agentvm/agent.pid; exec /var/lib/agentmultimodal/tools/agent > "/var/lib/agentvm/logs/agent-$1.log" 2>&1',"sh",phase])
        def ready():
            r=self.python("import os,sys; p=int(open('/run/agentvm/agent.pid').read()); assert p>1 and p!=int(sys.argv[1]); assert os.readlink('/proc/'+str(p)+'/exe')=='/var/lib/agentmultimodal/tools/agent'; print(p)",self.agent_pid,check=False)
            if r.returncode:return False
            pid=int(r.stdout)
            if self.cli("device","apps","list",timeout=8,check=False).returncode:return False
            self.python("import os,sys; p=int(sys.argv[1]); assert os.readlink('/proc/'+str(p)+'/exe')=='/var/lib/agentmultimodal/tools/agent'",pid)
            self.agent_pid=pid;return True
        self.wait(ready,75)

    def stop_agent(self):
        self.python("import os,signal,sys; p=int(sys.argv[1]); assert p>1; assert os.readlink('/proc/'+str(p)+'/exe')=='/var/lib/agentmultimodal/tools/agent'; os.kill(p,signal.SIGTERM)",self.agent_pid)
        self.wait(lambda:self.python("import os,sys; assert not os.path.exists('/proc/'+sys.argv[1])",self.agent_pid,check=False).returncode==0,50)

    def volumes(self):
        names={s:{k:stage.volume_name(self.plan["run_id"],s,k) for k in ("config","proofs")} for s in stage.SERVICES}
        # Tokens are generated and copied only inside the owned node. No token
        # value crosses stdout, host argv, a build context or a receipt.
        self.python("import json,os,pathlib,secrets,sys; names=json.loads(sys.argv[1]); base=pathlib.Path('/var/lib/wendy/volumes'); base.mkdir(parents=True,exist_ok=True); read=secrets.token_urlsafe(32); actuator=secrets.token_urlsafe(32); metadata=secrets.token_urlsafe(32)\n"
                    "def write(path,value,uid):\n fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)\n with os.fdopen(fd,'w') as f:f.write(value+'\\n')\n os.chown(path,uid,uid)\n"
                    "for service,parts in names.items():\n uid=65534 if service in ('scene','sensorlink-source') else 0\n for kind,name in parts.items():\n  p=base/name;p.mkdir(mode=0o700);os.chown(p,uid,uid)\n config=base/parts['config']\n if service in ('scene','camera-a','gas','audio','sensorlink-source'):write(config/'scene-read-token',read,uid)\n if service=='scene':write(config/'actuator-token',actuator,uid)\n if service in ('sensorlink-source','sensorlink-bridge'):write(config/'metadata-read-token',metadata,uid)\n"
                    "write(pathlib.Path('/etc/wendy-agent/.vm-fixture/scene-read-token'),read,0)\nprint('distinct service volumes prepared')",json.dumps(names))

    def task(self,service):
        cid=stage.APP+"_"+service
        info=json.loads(self.inside("ctr","containers","info",cid).stdout)
        labels=info.get("Labels",{})
        if labels.get("sh.wendy/app.id")!=stage.APP or labels.get("sh.wendy/service")!=service:raise stage.Refused("unexpected app task identity")
        rows=self.inside("ctr","tasks","list").stdout.decode().splitlines()[1:]
        matches=[r.split() for r in rows if r.split() and r.split()[0]==cid]
        if len(matches)!=1 or len(matches[0])<3 or matches[0][2]!="RUNNING":raise stage.Refused("ordinary service task is not running")
        pid=int(matches[0][1])
        state=json.loads(self.python("import json,os,sys; p=int(sys.argv[1]); assert p>1; raw=open('/proc/'+str(p)+'/stat').read(); fields=raw.rsplit(')',1)[1].split(); print(json.dumps({'pid':p,'start_ticks':int(fields[19]),'netns_inode':os.stat('/proc/'+str(p)+'/ns/net').st_ino,'cgroup':open('/proc/'+str(p)+'/cgroup').read().strip()}))",pid).stdout)
        interfaces=json.loads(self.inside("nsenter","--target",str(pid),"--net","ip","-j","-4","addr","show","dev","eth0").stdout)
        ips=[a["local"] for i in interfaces for a in i.get("addr_info",[]) if a.get("family")=="inet"]
        if len(ips)!=1:raise stage.Refused("exact CNI IPv4 address required")
        state.update(container=cid,app=stage.APP,service=service,ipv4=ips[0],world_socket=bool(labels.get("sh.wendy/world.socket-directory")))
        if service in stage.SENSORS or service=="fusion":
            if not state["world_socket"] or int(labels.get("sh.wendy/world.deployment-generation","0"))<=0:raise stage.Refused("World app lacks its distinct scoped socket")
            self.states[service]={**state,"generation":int(labels["sh.wendy/world.deployment-generation"]),"socket_directory":labels["sh.wendy/world.socket-directory"]}
        else:
            if state["world_socket"]:raise stage.Refused("source unexpectedly received World authority")
            self.states[service]=state
        return state

    def observe_scene(self):
        task=self.task("scene");endpoint="http://"+task["ipv4"]+":8091";nonce=uuid.uuid4().hex+uuid.uuid4().hex
        result=self.python("import json,sys,urllib.request\n"
                           "class NoRedirect(urllib.request.HTTPRedirectHandler):\n def redirect_request(self,*args,**kwargs):return None\n"
                           "token=open('/etc/wendy-agent/.vm-fixture/scene-read-token').read().strip(); request=urllib.request.Request(sys.argv[1]+'/v1/clock',headers={'Authorization':'Bearer '+token,'X-Wendy-Clock-Nonce':sys.argv[2]}); opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())\n"
                           "try:\n response=opener.open(request,timeout=1); raw=response.read(4097); assert response.status==200 and len(raw)<=4096; print(raw.decode())\nexcept Exception:raise SystemExit(1)",endpoint,nonce,check=False)
        if result.returncode:raise stage.Refused("bounded scene clock request failed")
        subnets=json.loads(self.inside("cat","/run/wendy/cni/subnets.json").stdout)
        return validation.scene_receipt(self.plan,json.loads(result.stdout),nonce,endpoint,task,subnets[stage.APP])

    def deploy(self,service):
        self.cli(*validation.selective_run_args(service))
        state=self.task(service)
        if self.scene is not None:validation.scene_unchanged(self.scene,self.observe_scene())
        stage.write_new(self.out/("task-"+service+".json"),stage.json_bytes(self.states[service]))
        return state

    def write_configs(self,values):
        mapping={service:stage.volume_name(self.plan["run_id"],service,"config") for service in values}
        self.python("import json,os,pathlib,sys; values=json.loads(sys.argv[1]); names=json.loads(sys.argv[2]); base=pathlib.Path('/var/lib/wendy/volumes')\n"
                    "for service,value in values.items():\n directory=base/names[service]; path=directory/'config.json'; fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)\n with os.fdopen(fd,'w') as f:json.dump(value,f);f.write('\\n')\n uid=65534 if service=='sensorlink-source' else 0;os.chown(path,uid,uid);os.chmod(directory,0o500)\nprint('service configs staged')",json.dumps(values),json.dumps(mapping))

    def release(self):
        services=[s for s in stage.SERVICES if s!="scene"]
        mapping={s:stage.volume_name(self.plan["run_id"],s,"config") for s in services}
        self.python("import json,os,pathlib,sys; names=json.loads(sys.argv[1]); base=pathlib.Path('/var/lib/wendy/volumes'); assert all((base/n/'config.json').is_file() and not (base/n/'start').exists() for n in names.values())\n"
                    "for name in names.values():\n fd=os.open(base/name/'start',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.close(fd)\nprint('all service gates released')",json.dumps(mapping))

    def snapshot_file(self,source,destination,limit,container=None):
        # docker cp works for both a running and a stopped owned container.
        # Never extract an archive: inspect its single regular member and copy
        # only a bounded prefix. Symlinks, extra members and truncation fail
        # validation, but any already received regular-file bytes are retained.
        response=self.call(["docker","cp",(container or self.name)+":"+source,"-"],timeout=10,check=False,max_output=limit+65536)
        receipt={"source":source,"status":"unavailable","command_exit":response.returncode}
        if not response.stdout:return receipt
        try:
            with tarfile.open(fileobj=io.BytesIO(response.stdout),mode="r:") as archive:
                member=archive.next()
                if member is None or member.name!=Path(source).name or not member.isfile():raise stage.Refused("receipt archive must contain the exact regular file")
                stream=archive.extractfile(member)
                data=stream.read(min(member.size,limit))
                stage.write_new(destination,data)
                receipt.update(size=len(data),declared_size=member.size,sha256=hashlib.sha256(data).hexdigest(),status="captured")
                if member.size>limit or len(data)!=member.size or response.returncode:
                    receipt["status"]="truncated_or_failed"
                elif archive.next() is not None:
                    receipt["status"]="extra_archive_members"
        except (OSError,ValueError,tarfile.TarError) as exc:
            receipt.update(status="invalid",error=type(exc).__name__)
        return receipt

    def snapshot_outputs(self,destination):
        result={}
        for service in stage.SERVICES:
            result[service]={}
            directory="/var/lib/wendy/volumes/"+stage.volume_name(self.plan["run_id"],service,"proofs")
            for name,limit in (("exit.json",4096),("stdout.json",32<<20),("stderr.log",65536)):
                # Scene stdout is a stream, not an application JSON receipt.
                target="stdout.stream" if service=="scene" and name=="stdout.json" else name
                try:result[service][name]=self.snapshot_file(directory+"/"+name,destination/service/target,limit)
                except Exception as exc:result[service][name]={"status":"error","error":type(exc).__name__}
        stage.write_new(destination/"snapshot.json",stage.json_bytes(result))
        return result

    def collect(self):
        services=[s for s in stage.SERVICES if s!="scene"]
        def done():
            names=[stage.volume_name(self.plan["run_id"],s,"proofs") for s in services]
            return self.python("import json,pathlib,sys; base=pathlib.Path('/var/lib/wendy/volumes'); assert all((base/n/'exit.json').is_file() for n in json.loads(sys.argv[1]))",json.dumps(names),check=False).returncode==0
        wait_error=None
        try:self.wait(done,150)
        except BaseException as exc:wait_error=exc
        snapshots=self.snapshot_outputs(self.out/"receipts")
        if wait_error is not None:raise wait_error
        result={}
        for service in services:
            result[service]={}
            for name in ("exit.json","stdout.json","stderr.log"):
                if snapshots[service][name]["status"]!="captured":raise stage.Refused("app output is missing, truncated or invalid: "+service+"/"+name)
                if name=="stderr.log":
                    if result[service]["exit.json"].get("stderr_bytes")!=snapshots[service][name]["size"]:raise stage.Refused("wrapper byte count differs from exact retained stderr")
                    continue
                limit=4096 if name=="exit.json" else 32<<20
                data=(self.out/"receipts"/service/name).read_bytes()
                result[service][name]=stage.strict_json(data,limit)
                if name=="stdout.json" and result[service]["exit.json"].get("stdout_bytes")!=len(data):raise stage.Refused("wrapper byte count differs from exact retained stdout")
        return result

    def run(self):
        stage.image_info(self.plan["runtime_image"]["id"])
        stage.write_new(self.out/"inputs.json",stage.json_bytes({"plan_sha256":stage.digest_file(self.source/"plan.json"),"inventory_sha256":stage.digest_file(self.source/"files.json"),"runtime_image":self.plan["runtime_image"],"name":self.name,"network":self.network,"volume":self.volume,"simulation_only":True,"rpc_timing_requested":bool(getattr(self.args,"trace_rpc",False))}))
        self.created.append("network");self.call(["docker","network","create","--label",LABEL+"="+self.name,self.network])
        self.created.append("volume");self.call(["docker","volume","create","--label",LABEL+"="+self.name,self.volume])
        argv=runtime_args(self.plan["runtime_image"]["id"],self.name,self.network,self.volume)
        self.created.append("container");self.call(argv)
        self.call(["docker","inspect",self.name])
        self.wait(lambda:self.python("import json; assert json.load(open('/run/agentvm/ready.json'))['ready'] is True",check=False).returncode==0)
        self.upload()
        initialized=self.inside(REMOTE+"/tools/fixture","init-enrollment","--directory","/etc/wendy-agent","--state-directory","/var/lib/wendy/world")
        self.public=json.loads(initialized.stdout)
        stage.write_new(self.out/"fixture-public.json",stage.json_bytes(self.public))
        self.volumes()
        self.python("import os; assert not os.path.exists('/etc/wendy-agent/world.json'); open('/etc/wendy-agent/network-access','x').write('local\\n')")
        self.inside("mkdir","-p","/var/lib/agentvm/logs")
        self.call(["docker","exec","-d",self.name,"sh","-c","exec /var/lib/agentmultimodal/tools/fixture serve --directory /etc/wendy-agent --handoff /config/acme-enrollment.json --duration 10m > /var/lib/agentvm/logs/issuer.log 2>&1"])
        self.wait(lambda:self.python("import os; assert os.path.isfile('/config/acme-enrollment.json')",check=False).returncode==0)
        self.start_agent("scene-bootstrap")
        self.wait(lambda:self.python("import json,os; assert json.load(open('/etc/wendy-agent/provisioning.json')).get('enrolled'); assert os.path.isfile('/etc/wendy-agent/.vm-fixture/issued.json')",check=False).returncode==0,90)
        self.deploy("scene")
        def scene_ready():
            try:self.scene=self.observe_scene();return True
            except stage.Refused:return False
        self.wait(scene_ready,30)
        stage.write_new(self.out/"scene-before.json",stage.json_bytes(self.scene))
        commission=configs.commission_spec(self.scene)
        if self.history is not None:commission=self.history.commission(commission)
        self.python("import json,os,sys; path='/etc/wendy-agent/.vm-fixture/commission-spec.json'; fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)\nwith os.fdopen(fd,'w') as f:json.dump(json.loads(sys.argv[1]),f)",json.dumps(commission))
        self.inside(REMOTE+"/tools/fixture","commission","--directory","/etc/wendy-agent","--state-directory","/var/lib/wendy/world","--commission-spec","/etc/wendy-agent/.vm-fixture/commission-spec.json")
        self.inside(REMOTE+"/tools/initializer","--config","/etc/wendy-agent/world.json",timeout=45)
        self.stop_agent();self.start_agent("world-active")
        after=self.observe_scene();validation.scene_unchanged(self.scene,after);stage.write_new(self.out/"scene-after-restart.json",stage.json_bytes(after))
        for service in stage.SERVICES:
            if service!="scene":self.deploy(service)
        source=self.states["sensorlink-source"]
        if ipaddress.IPv4Address(source["ipv4"]) not in ipaddress.IPv4Network(self.scene["app_subnet"]):raise stage.Refused("SensorLink source escaped its app subnet")
        sockets=[state["socket_directory"] for state in self.states.values() if state.get("world_socket")]
        if len(sockets)!=5 or len(set(sockets))!=5:raise stage.Refused("World services must have five separate socket mounts")
        values=configs.configurations(self.plan,self.scene,self.public,source["ipv4"])
        values["fusion"]["expected_generation"]=self.states["fusion"]["generation"]
        if self.history is not None:values=self.history.configurations(values)
        self.write_configs(values)
        self.release()
        collected=self.collect()
        # The separate validator reports structural/hash checks separately
        # from the fuser's in-app certificate and signature verification.
        from receipts import validate_receipts
        verification_plan={**self.plan,"tenant":self.public["tenant"],"device":self.public["principal"]}
        validation_result=validate_receipts(verification_plan,self.scene,self.states,collected)
        validation.scene_unchanged(self.scene,self.observe_scene())
        result={"passed":True,"simulation_only":True,"normal_first_boot_acme":True,"normal_cli_containerd_deployment":True,"scene_preserved_across_agent_restart":True,"validation":validation_result,"remaining":["expiry and history through fresh ordinary read service","fault scenarios","multi-node transport","physical commissioning"]}
        if self.history is not None:
            result["history"]=self.history.execute(self.history.source_commission(),(self.out/"receipts/fusion/stdout.json").read_bytes(),(self.out/"receipts/fusion/exit.json").read_bytes())
            result["remaining"].remove("expiry and history through fresh ordinary read service")
        return result

    def resource_state(self,kind,name):
        response=self.call(["docker",kind,"inspect",name],timeout=15,check=False)
        if not response.returncode:
            try:
                rows=stage.strict_json(response.stdout)
                if len(rows)!=1:raise ValueError("one resource expected")
                item=rows[0];labels=item.get("Config",{}).get("Labels",{}) if kind=="container" else item.get("Labels",{})
                return "owned" if (labels or {}).get(LABEL)==self.name else "foreign"
            except (ValueError,KeyError,IndexError,TypeError):return "unknown"
        # A failed inspect is not evidence of absence (the daemon may be down).
        command=["docker",kind,"ls"]+(["--all"] if kind=="container" else [])
        command += ["--filter","name="+name,"--format","{{.Names}}" if kind=="container" else "{{.Name}}"]
        listing=self.call(command,timeout=15,check=False)
        if listing.returncode:return "unknown"
        return "unknown" if name in listing.stdout.decode(errors="replace").splitlines() else "absent"

    def snapshot_stopped_state(self):
        # A lost acknowledgment can follow a committed write. Retain only the
        # two bounded, non-credential databases after all writers have stopped.
        # Absence of a record later cannot prove that it was never committed:
        # normal expiry or pruning may already have removed it.
        receipt={"version":1,"qualification":"offline diagnostic bytes only; not current authority or proof of noncommit","writers_stopped":False,"files":{}}
        before=self.call(["docker","container","inspect",self.name],timeout=10,check=False)
        container_id=None
        if before.returncode==0:
            try:
                rows=stage.strict_json(before.stdout)
                if len(rows)==1 and (rows[0].get("Config",{}).get("Labels",{}) or {}).get(LABEL)==self.name:
                    candidate=rows[0].get("Id","")
                    if isinstance(candidate,str) and re.fullmatch(r"[0-9a-f]{64}",candidate):container_id=candidate
            except (ValueError,KeyError,TypeError):
                pass
        if container_id is None:
            stage.write_new(self.out/"private-runtime-state"/"snapshot.json",stage.json_bytes(receipt))
            return receipt
        receipt["container_id"]=container_id
        response=self.call(["docker","stop","--time","10",container_id],timeout=20,check=False)
        receipt["stop_exit"]=response.returncode
        inspected=self.call(["docker","container","inspect",container_id],timeout=10,check=False)
        if inspected.returncode==0:
            try:
                rows=stage.strict_json(inspected.stdout)
                if len(rows)==1:
                    item=rows[0]
                    owned=item.get("Id")==container_id and (item.get("Config",{}).get("Labels",{}) or {}).get(LABEL)==self.name
                    state=item.get("State",{})
                    receipt["writers_stopped"]=owned and state.get("Running") is False and type(state.get("Pid")) is int and state["Pid"]==0
            except (ValueError,KeyError,TypeError):
                pass
        if receipt["writers_stopped"]:
            for relative in ("journal/journal.db","archive/proofs.db"):
                try:
                    receipt["files"][relative]=self.snapshot_file("/var/lib/wendy/world/"+relative,self.out/"private-runtime-state"/relative,16<<20,container=container_id)
                except Exception as exc:
                    receipt["files"][relative]={"status":"error","error":type(exc).__name__}
        stage.write_new(self.out/"private-runtime-state"/"snapshot.json",stage.json_bytes(receipt))
        return receipt

    def finish(self):
        receipt={"version":1,"explicit_keep":self.args.keep,"resources":{},"evidence_errors":[]}
        for kind,name in (("container",self.name),("volume",self.volume),("network",self.network)):
            item={"name":name,"state":"not_created"};receipt["resources"][kind]=item
            if kind not in self.created:continue
            try:item["state"]=self.resource_state(kind,name)
            except Exception as exc:item.update(state="unknown",inspection_error=type(exc).__name__)
            if item["state"]!="owned":continue
            if kind=="container":
                # Capture all available outputs before any removal, including
                # empty/failed stdout and a still-streaming scene. Nothing in
                # a config or credential directory is copied.
                try:self.snapshot_outputs(self.out/"cleanup-receipts")
                except Exception as exc:receipt["evidence_errors"].append({"step":"app_outputs","error":type(exc).__name__})
                try:self.call(["docker","logs",self.name],timeout=15,check=False)
                except Exception as exc:receipt["evidence_errors"].append({"step":"runtime_logs","error":type(exc).__name__})
                logfiles=["issuer.log","agent-scene-bootstrap.log","agent-world-active.log"]
                if getattr(self,"history",None) is not None:logfiles.append("agent-history-restarted.log")
                for logfile in logfiles:
                    try:self.snapshot_file("/var/lib/agentvm/logs/"+logfile,self.out/"private-runtime-logs"/logfile,8<<20)
                    except Exception as exc:receipt["evidence_errors"].append({"step":logfile,"error":type(exc).__name__})
                if not self.args.keep:
                    try:receipt["stopped_state"]=self.snapshot_stopped_state()
                    except Exception as exc:receipt["evidence_errors"].append({"step":"stopped_state","error":type(exc).__name__})
            if self.args.keep:
                item["state"]="retained"
                continue
            removal_target=name
            if kind=="container":
                # Snapshotting may take time. Recheck ownership, then remove
                # that exact ID so a same-name replacement cannot be affected.
                removal_target=None
                try:
                    response=self.call(["docker","container","inspect",name],timeout=10,check=False)
                    if response.returncode==0:
                        rows=stage.strict_json(response.stdout)
                        if len(rows)==1 and (rows[0].get("Config",{}).get("Labels",{}) or {}).get(LABEL)==self.name:
                            candidate=rows[0].get("Id","")
                            if isinstance(candidate,str) and re.fullmatch(r"[0-9a-f]{64}",candidate):removal_target=candidate
                except Exception as exc:item["removal_identity_error"]=type(exc).__name__
                if removal_target is None:
                    try:item["state"]=self.resource_state(kind,name)
                    except Exception as exc:item.update(state="unknown",verification_error=type(exc).__name__)
                    continue
            command=["docker","rm","-f",removal_target] if kind=="container" else ["docker",kind,"rm",name]
            try:item["remove_exit"]=self.call(command,timeout=30,check=False).returncode
            except Exception as exc:item["remove_error"]=type(exc).__name__
            try:item["state"]=self.resource_state(kind,name)
            except Exception as exc:item.update(state="unknown",verification_error=type(exc).__name__)
        accepted={"absent","not_created"}|({"retained"} if self.args.keep else set())
        receipt["complete"]=all(value["state"] in accepted for value in receipt["resources"].values())
        stage.write_new(self.out/"cleanup.json",stage.json_bytes(receipt))
        return receipt

    def execute(self):
        primary=None;cleanup=None;result={"passed":False,"simulation_only":True}
        previous={signum:signal.getsignal(signum) for signum in (signal.SIGTERM,signal.SIGINT)}
        def interrupted(signum,frame):raise InterruptedRun(signum)
        for signum in previous:signal.signal(signum,interrupted)
        try:
            try:result=self.run()
            except BaseException as exc:
                primary=exc
                result.update(error_type=type(exc).__name__,error=str(exc)[:1024])
            # A second interrupt cannot skip bounded owned-resource cleanup.
            for signum in previous:signal.signal(signum,signal.SIG_IGN)
            try:cleanup=self.finish()
            except BaseException as exc:cleanup={"complete":False,"error_type":type(exc).__name__,"error":str(exc)[:1024]}
            result["qualification_passed"]=bool(result.get("passed"))
            result["cleanup"]=cleanup
            result["passed"]=result["qualification_passed"] and cleanup["complete"]
            try:stage.write_new(self.out/"result.json",stage.json_bytes(result))
            except Exception:
                if primary is not None:raise primary
                raise
            if primary is not None:raise primary
            if not result["passed"]:raise stage.Refused("run did not complete qualification and verified cleanup; inspect result.json")
            return result
        finally:
            for signum,handler in previous.items():signal.signal(signum,handler)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--stage",type=Path,required=True);parser.add_argument("--output",type=Path,required=True)
    parser.add_argument("--keep",action="store_true",help="retain only this owned runtime for debugging")
    parser.add_argument("--trace-rpc",action="store_true",help="request bounded private-agent RPC timings without changing authority checks")
    parser.add_argument("--history-bundle",type=Path,help="opt in to two retained-history reader phases")
    parser.add_argument("--history-reader-sha256")
    parser.add_argument("--history-plan-sha256")
    parser.add_argument("--history-inventory-sha256")
    args=parser.parse_args();runner=Runner(args)
    runner.execute()


if __name__=="__main__":main()
