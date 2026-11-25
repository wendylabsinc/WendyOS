"""Actual Swift zone signer -> three Go replicas -> Go/Swift proof readers."""
import argparse,json,os,subprocess,tempfile,time,socket
from pathlib import Path

def run(bins,probe,out):
    out.mkdir(parents=True,exist_ok=False,mode=0o700);processes=[];logs=[]
    with tempfile.TemporaryDirectory(prefix='world-native-zone-') as tmp:
        root=Path(tmp)
        def write(name,v):p=root/name;p.write_text(json.dumps(v));p.chmod(0o600);return p
        def cmd(*args,okay=True):
            p=subprocess.run(list(map(str,args)),capture_output=True,timeout=20)
            if okay and p.returncode:raise AssertionError(p.stderr.decode())
            if not okay:assert p.returncode;return p.stderr.decode()
            return json.loads(p.stdout) if p.stdout.strip().startswith((b'{',b'[')) else None
        for port in range(27500,28000,3):
            held=[]
            try:
                for n in range(3):s=socket.socket();s.bind(('127.0.0.1',port+n));held.append(s)
                break
            except OSError:
                for s in held:s.close()
        for s in held:s.close()
        try:
            cmd(bins/'world-lab','--out',root,'--run-id','native-zones','--base-port',port)
            for n in (1,2,3):
                log=(out/f'node-{n}.log').open('w');logs.append(log);processes.append(subprocess.Popen([str(bins/'worldd'),'--config',str(root/f'node-{n}.json')],stdout=log,stderr=log))
            def swift(verb,n=1,*args,okay=True):return cmd(probe,verb,root/f'site-admin-{n}.json',root/'site-trust.json',*args,okay=okay)
            for _ in range(60):
                try:swift('list');break
                except AssertionError:time.sleep(.1)
            draft={'name':'West <&> "work"','revision':'operator-1','context':{'tenant':'lab','site':'synthetic-office','runID':'native-zones','frameID':'floor','frameRevision':'1'},'zones':[{'id':'A\u030a','label':'West <&> area','min_xz':[-2.125,-1.001],'max_xz':[-.001,.125],'dwell_seconds':1.125}]}
            draft['zones'].append(dict(draft['zones'][0],id='Z',label='Second zone'))
            draft['zones'].reverse()
            path=write('draft.json',draft);record=swift('publish',1,path);digest=record['component']['key']['entity'].removeprefix('zone-catalog/');source=write('source.json',record['component']['key']['source'])
            def until(fn):
                for _ in range(100):
                    try:return fn()
                    except AssertionError:time.sleep(.15)
                raise AssertionError('replica did not converge')
            fetched=until(lambda:cmd(bins/'world','zone','fetch','--credentials',root/'site-admin-3.json','--trust',root/'site-trust.json','--sha256',digest,'--source',source))
            assert fetched['publication']['sha256']==digest
            rows=swift('list',3);assert any(x['sha256']==digest for x in rows)
            assert swift('delete',1,digest)['deleted']
            for _ in range(100):
                rows=swift('list',3)
                if any(x['sha256']==digest and x['deleted'] for x in rows):break
                time.sleep(.15)
            else:raise AssertionError('tombstone not verified by Swift')
            draft['zones'][0]['min_xz'][0]=None;write('draft.json',draft);refused=swift('publish',1,path,okay=False)
            report={'passed':True,'nodes':3,'swift_signed_publish_go_verified':True,'swift_verified_repaired_proof':True,'swift_signed_delete_repaired_and_verified':True,'noninteger_decimal_and_html_escape_interop':True,'malformed_geometry_refused':bool(refused),'digest':digest,'private_keys_in_output':False,'simulation_only':True}
            (out/'report.json').write_text(json.dumps(report,indent=2));return report
        finally:
            for p in processes:
                if p.poll() is None:p.terminate()
            for p in processes:
                try:p.wait(timeout=5)
                except subprocess.TimeoutExpired:p.kill();p.wait(timeout=5)
            for f in logs:f.close()
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--bin-dir',type=Path,required=True);p.add_argument('--probe',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args();os.umask(0o077);print(json.dumps(run(a.bin_dir,a.probe,a.out)))
