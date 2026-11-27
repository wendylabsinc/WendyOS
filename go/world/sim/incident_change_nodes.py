"""Optional bounded Linux venue for the incident-change simulation acceptance.

Runs the identical worldd implementation on durable Docker volumes. This is a
three-node test fixture, with no scheduler or device deployment. The host native
clients and fault proxies remain unchanged; no credentials enter an image.
"""
from __future__ import annotations
import hashlib
import json
from pathlib import Path
import subprocess
import time
import uuid


def docker(*args, timeout=15):
    return subprocess.run(['docker', *map(str,args)], check=True, capture_output=True,
                          text=True, timeout=timeout).stdout.strip()


class LinuxNode:
    def __init__(self, binary, image, config_path, runtime, logs, number, port):
        binary=Path(binary).resolve();runtime=Path(runtime);logs=Path(logs)
        if not binary.is_file() or binary.is_symlink():raise ValueError('local Linux binary required')
        image_info=json.loads(docker('image','inspect',image))[0]
        if image_info['Architecture']!='arm64':raise ValueError('arm64 Linux image required')
        self.image=image_info['Id'];self.name='incident-change-'+uuid.uuid4().hex[:16]
        self.volume=self.name+'-state';self.runtime=runtime;self.logs=logs;self.number=number
        self.created=False;self.volume_created=False;self.closed=False
        config=json.loads(Path(config_path).read_text())
        config['listen']=f'0.0.0.0:{port}'
        for field,suffix in [('data_path','store'),('proof_path','proofs'),('replay_path','replay')]:
            config[field]=f'/state/node-{number}.{suffix}'
        config['diagnostics']['path']=f'/state/node-{number}.metrics'
        for field in ('tasks','task_relay','proposals','evidence'):
            if field in config:
                config[field]['path']=f'/state/node-{number}-{field}'
        for peer in config['peers']:
            if not peer['url'].startswith('http://127.0.0.1:'):raise ValueError('loopback fixture peer required')
            peer['url']=peer['url'].replace('127.0.0.1','host.docker.internal',1)
        private=runtime/f'linux-node-{number}.json';private.write_text(json.dumps(config));private.chmod(0o600)
        self.receipt={'venue':'Docker Desktop Linux VM; native clients and MuJoCo on macOS',
            'image_id':self.image,'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),
            'container':self.name,'persistent_volume':self.volume,'cpus':1,'memory_bytes':536870912,
            'fsync':'unchanged worldd durable stores; named volume, no tmpfs',
            'host_listen':f'127.0.0.1:{port}','peer_transport':'host loopback fault proxies'}
        try:
            docker('volume','create','--label','wendy.fixture=incident-change',self.volume);self.volume_created=True
            docker('create','--pull=never','--name',self.name,'--label','wendy.fixture=incident-change',
                   '--read-only','--cap-drop','ALL','--security-opt','no-new-privileges','--cpus','1',
                   '--memory','512m','--pids-limit','128','--publish',f'127.0.0.1:{port}:{port}',
                   '--mount',f'type=bind,src={binary},dst=/worldd,readonly',
                   '--mount',f'type=bind,src={private.resolve()},dst=/config.json,readonly',
                   '--mount',f'type=volume,src={self.volume},dst=/state',
                   '--entrypoint','/worldd',self.image,'--config','/config.json');self.created=True
            docker('start',self.name)
        except Exception:
            self.cleanup();raise

    def poll(self):
        if not self.created:return 0
        state=json.loads(docker('inspect','--format','{{json .State}}',self.name))
        return None if state['Running'] else state['ExitCode']

    def terminate(self):
        if self.created:docker('stop','--time','3',self.name,timeout=8)

    def kill(self):
        if self.created and self.poll() is None:docker('kill',self.name)

    def wait(self,timeout=3):
        end=time.monotonic()+timeout
        while time.monotonic()<end:
            code=self.poll()
            if code is not None:return code
            time.sleep(.05)
        raise subprocess.TimeoutExpired(self.name,timeout)

    def cleanup(self):
        if self.closed:return
        errors=[]
        if self.created:
            try:
                if self.poll() is None:self.terminate()
                self.receipt['exit_code']=self.poll()
                log=subprocess.run(['docker','logs',self.name],check=True,capture_output=True,text=True,timeout=15)
                (self.logs/f'node-{self.number}.log').write_text(log.stdout+log.stderr)
                export=self.runtime/f'linux-state-{self.number}';export.mkdir(exist_ok=True)
                docker('cp',self.name+':/state/.',export)
                metric=export/f'node-{self.number}.metrics'
                if metric.exists():(self.runtime/metric.name).write_bytes(metric.read_bytes())
                self.receipt['state_exported']=True
            except Exception as error:errors.append(repr(error))
            try:docker('rm','--force',self.name);self.created=False
            except Exception as error:errors.append(repr(error))
        if self.volume_created:
            try:docker('volume','rm',self.volume);self.volume_created=False
            except Exception as error:errors.append(repr(error))
        self.receipt.update(container_removed=not self.created,volume_removed=not self.volume_created,cleanup_errors=errors)
        (self.runtime/f'linux-node-{self.number}.receipt').write_text(json.dumps(self.receipt,indent=2))
        self.closed=not self.created and not self.volume_created
        if errors:raise RuntimeError('Linux fixture cleanup: '+repr(errors))
