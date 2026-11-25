"""Actual Go HTTP / Swift retention interoperability, with bounded owned processes."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import threading

p = argparse.ArgumentParser()
p.add_argument('--out', required=True)
p.add_argument('--port', type=int, default=19440)
a = p.parse_args()
root = Path(a.out).resolve()
root.mkdir(mode=0o700, parents=True, exist_ok=False)
repo = Path(__file__).resolve().parents[1]
processes = []
logs = []
def run(args, **kwargs):
    return subprocess.run(args, cwd=repo, check=True, **kwargs)
try:
    run(['go', 'build', '-o', str(root/'fixture'), './tests/investigationfixture'])
    run(['go', 'build', '-o', str(root/'worldd'), './cmd/worldd'])
    run([str(root/'fixture'), '--out', str(root/'private'), '--port', str(a.port)])
    private = root/'private'
    for i, name in enumerate(('capture-a', 'capture-b')):
        log = (root/(name+'.log')).open('w');logs.append(log)
        processes.append(subprocess.Popen([str(root/'worldd'), '--config', str(private/(name+'.json'))], stdout=log, stderr=log))
        directory = private/(name+'-proxy')
        directory.mkdir(mode=0o700)
        processes.append(subprocess.Popen([sys.executable, str(repo/'tests/investigation_proxy.py'), '--port', str(a.port+2+i), '--upstream', str(a.port+i), '--directory', str(directory)], stdout=log, stderr=log))
    time.sleep(0.5)
    (private/'unit-bytes').write_bytes(b'abc')
    for name in ('capture-a','capture-b'):
        run([str(root/'fixture'),'--mode','upload','--out',str(private),'--node',name,'--file',str(private/'unit-bytes')],stdout=(root/(name+'-upload.json')).open('w'))
    stop = threading.Event()
    failures = []
    def actions():
        try:
            for action in ('restart-a','revoke-a','wrong-authority','quota-pressure'):
                while not (private/('request-'+action)).exists():
                    if stop.wait(.03):return
                if action == 'quota-pressure':
                    pressure=private/'pressure-bytes';pressure.write_bytes(b'p'*(8<<20))
                    with (root/'quota-pressure.log').open('w') as log:
                        result=subprocess.run([str(root/'fixture'),'--mode','upload','--out',str(private),'--node','capture-a','--file',str(pressure)],cwd=repo,stdout=log,stderr=log)
                    assert result.returncode != 0 and 'node HTTP 507: evidence quota exhausted' in (root/'quota-pressure.log').read_text(), 'expected explicit capacity refusal preserving pinned bytes'
                elif action == 'wrong-authority':
                    run([str(root/'fixture'),'--mode','wrong-authority','--out',str(private)],stdout=(root/'wrong-authority.log').open('w'))
                else:
                    if action == 'revoke-a':run([str(root/'fixture'),'--mode','revoke','--out',str(private)])
                    old=processes[0];old.kill();old.wait(timeout=5)
                    processes[0]=subprocess.Popen([str(root/'worldd'),'--config',str(private/'capture-a.json')],stdout=logs[0],stderr=logs[0])
                    time.sleep(.15)
                    if processes[0].poll() is not None:raise RuntimeError('node restart failed')
                (private/('done-'+action)).write_text('done')
        except Exception as error:failures.append(str(error))
    thread=threading.Thread(target=actions,daemon=True);thread.start()
    with (root/'swift-http.log').open('w') as log:
        run(['swift','test','--package-path','../macos/WorldClient','--filter','InvestigationHTTPTests'],env={**os.environ,'INVESTIGATION_HTTP_FIXTURE':str(private)},stdout=log,stderr=log)
    stop.set();thread.join(timeout=2)
    assert not failures, failures
    (root/'receipt.json').write_text(json.dumps({'passed':True,'scope':'Actual authenticated standalone Go stores; synthetic three-byte unit payload, not inspection media','nodes':2,'checks':['read/pin separate capability','lost create acknowledgement','partition/reconnect','journal reopen','lost release acknowledgement','exact immutable retries','released tombstone','capture process SIGKILL/reopen','pin quota','expiry','current revocation','wrong scope and site','changed receipt digest/deadline','expiry during response','byte quota failure preserves pinned evidence']},indent=2))
finally:
    for proc in processes:
        if proc.poll() is None:proc.terminate()
    for proc in processes:
        try:proc.wait(timeout=5)
        except subprocess.TimeoutExpired:proc.kill();proc.wait()
    for log in logs:log.close()
    (root/'cleanup.json').write_text(json.dumps({'owned_pids':[x.pid for x in processes],'returncodes':[x.returncode for x in processes],'all_exited':all(x.poll() is not None for x in processes)}))
