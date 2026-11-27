#!/usr/bin/env python3
"""Launch one detached passive Go2 patrol in private Linux containers and a Mac twin."""
import argparse
import hashlib
import shutil
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from integration import nativeapp


def command(args, **kw):
    return subprocess.check_output(args, cwd=ROOT, text=True, timeout=kw.pop('timeout', 30), **kw).strip()


def get(url):
    with urllib.request.urlopen(url, timeout=4) as response:
        return json.load(response)


def wait(url, predicate, timeout=180):
    deadline = time.monotonic()+timeout
    error = None
    while time.monotonic()<deadline:
        try:
            value = get(url)
            if predicate(value):
                return value
        except Exception as exc:
            error = str(exc)
        time.sleep(.3)
    raise RuntimeError(f'{url} did not become ready: {error}')


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1',0)); return s.getsockname()[1]


def default_source():
    candidates = sorted((Path.home()/'Library/Caches/wendy-world').glob('*/prepared/source/go/simulator/go2'),reverse=True)
    return next((p for p in candidates if (p/'go2_sim/simulation.py').is_file()),None)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--out',type=Path)
    p.add_argument('--prepare-offline',type=Path,help='prepare a retained one-use offline kit in this private catalog; no mission starts')
    p.add_argument('--offline-catalog',type=Path,help='inspect and choose an existing offline kit; no build, fetch or signer')
    p.add_argument('--activate-kit',help='explicitly activate this prepared kit instead of the native chooser')
    p.add_argument('--kit-name',default='Offline office patrol')
    p.add_argument('--inspection-tour',action='store_true',help='Review a finite selected-room inspection and return')
    p.add_argument('--duration',type=int,default=3600)
    p.add_argument('--world-watch-site',type=Path,help='explicitly commission office World-watch fixture using this directory of worldd/world-gas-replica binaries')
    p.add_argument('--world-watch-reader',type=Path,help='private World read credential for a signed watch policy')
    p.add_argument('--schedule',type=Path,help='reviewed JSON with revisit_seconds for every room')
    p.add_argument('--configure',action='store_true',help='review/edit room intervals in a native window before signing and starting')
    p.add_argument('--source',type=Path,default=os.environ.get('WENDY_GO2_SOURCE') or default_source())
    p.add_argument('--assets',type=Path,default=os.environ.get('WENDY_GO2_ASSETS') or ROOT/'.local/go2-inspection-video/assets')
    p.add_argument('--build',action='store_true',help='build private local container images')
    p.add_argument('--nav-base',default='ros:humble-ros-base')
    p.add_argument('--headless',action='store_true')
    p.add_argument('--app',type=Path,help='integrated Wendy.app or WendyDesktop executable (also WENDY_DESKTOP_APP)')
    p.add_argument('--physics-image',default='wendy-world-patrol-physics:local')
    p.add_argument('--navigation-image',default='wendy-world-patrol-nav:local')
    args=p.parse_args()
    if args.inspection_tour:args.configure=True
    if args.offline_catalog:
        import offline_patrol_kit
        return offline_patrol_kit.activate(args)
    if args.activate_kit:p.error('--activate-kit requires --offline-catalog')
    if not 1<=args.duration<=86400 or args.source is None or not (args.source/'go2_sim/simulation.py').is_file() or not (args.assets/'robot/go2.xml').is_file():
        p.error('duration 1..86400 and cached pinned --source/--assets required')
    os.umask(0o077)
    run_id='patrol-'+uuid.uuid4().hex[:12]
    out=(args.out or Path.home()/'Library/Caches/wendy-world'/run_id).resolve()
    out.mkdir(mode=0o700,parents=True,exist_ok=False)
    docker_info=json.loads(command(['docker','info','--format','{{json .}}']))
    if docker_info.get('OSType')!='linux':raise RuntimeError('Local Linux Docker engine required')
    context=command(['docker','context','show'])
    endpoint=json.loads(command(['docker','context','inspect',context]))[0]['Endpoints']['docker']['Host']
    if os.environ.get('DOCKER_HOST') and not os.environ.get('DOCKER_CONTEXT'):endpoint=os.environ['DOCKER_HOST']
    if not endpoint.startswith('unix://'):raise RuntimeError('Use a local Docker Unix-socket context')
    with (out/'build.log').open('w') as log:
        if args.build:
            for dockerfile,tag,extra in [('sim/patrol-physics.Dockerfile',args.physics_image,[]),('deploy/patrol-nav/Dockerfile',args.navigation_image,['--build-arg','NAV_BASE='+args.nav_base])]:
                subprocess.run(['docker','build','--pull=false','-f',dockerfile,'-t',tag,*extra,'.'],cwd=ROOT,stdout=log,stderr=subprocess.STDOUT,check=True,timeout=1800)
        retained=args.prepare_offline.resolve()/'tool/world-patrol' if args.prepare_offline else None
        if retained and retained.exists():
            catalog=args.prepare_offline.resolve()
            if catalog.stat().st_uid!=os.getuid() or catalog.stat().st_mode & 0o077 or retained.is_symlink() or not 0<retained.stat().st_size<=32<<20:
                raise RuntimeError('Existing catalog requires an owner-private trusted installed tool')
            binary=retained.read_bytes()
            if hashlib.sha256(binary).hexdigest()!=(catalog/'tool/sha256').read_text().strip():raise RuntimeError('Installed catalog tool changed')
            (out/'world-patrol').write_bytes(binary);(out/'world-patrol').chmod(0o500)
        else:
            subprocess.run(['go','build','-o',str(out/'world-patrol'),'./cmd/world-patrol'],cwd=ROOT,stdout=log,stderr=subprocess.STDOUT,check=True,timeout=180)
        app=args.app
        if args.prepare_offline:
            catalog=args.prepare_offline.resolve();catalog.mkdir(mode=0o700,parents=True,exist_ok=True)
            tool=catalog/'tool';tool.mkdir(mode=0o700,exist_ok=True)
            binary=(out/'world-patrol').read_bytes();digest=hashlib.sha256(binary).hexdigest()
            trusted=tool/'world-patrol'
            if trusted.exists() and hashlib.sha256(trusted.read_bytes()).hexdigest()!=digest:
                raise RuntimeError('Catalog tool version differs; use a new catalog for this runtime version')
            if not trusted.exists():
                with trusted.open('xb') as f:f.write(binary);f.flush();os.fsync(f.fileno())
                trusted.chmod(0o500)
                (tool/'sha256').write_text(digest+'\n')

        if not args.headless or args.configure:
            app=Path(nativeapp.executable(app))
    images={name:command(['docker','image','inspect',tag,'--format','{{.Id}}']) for name,tag in [('physics',args.physics_image),('navigation',args.navigation_image)]}
    names={'physics':run_id+'-physics','navigation':run_id+'-nav'}
    network=run_id+'-network'; processes=[]; created=[]
    receipt=dict(schema='wendy.nav.patrol.run.v1',run_id=run_id,out=str(out),duration_seconds=args.duration,images=images,
        containers=names,network=network,source=str(args.source.resolve()),assets=str(args.assets.resolve()),hardware=False,
        simulation=True,mode='passive observe-and-report',viewer_lifetime='independent of mission')
    def save():
        (out/'run.json').write_text(json.dumps(receipt,indent=2)+'\n')
    def spawn(argv,name):
        with (out/(name+'.log')).open('w') as log:
            proc=subprocess.Popen(argv,cwd=ROOT,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
        processes.append(proc);receipt[name+'_pid']=proc.pid;return proc
    try:
        command(['docker','network','create',network]);created.append(network)
        command(['docker','run','-d','--name',names['physics'],'--network',network,'--network-alias','physics',
            '--restart','no','--cpus','4','--memory','2g','-p','127.0.0.1::8080',
            '-v',str(args.source.resolve())+':/source:ro','-v',str(args.assets.resolve())+':/assets:ro',
            '--pull=never',images['physics'],'--source','/source','--assets','/assets','--run-id',run_id])
        created.append(names['physics'])
        physics='http://'+command(['docker','port',names['physics'],'8080/tcp'])
        wait(physics+'/sensors',lambda v:v.get('ready'))
        physical_scope=get(physics+'/state')
        command(['docker','run','-d','--name',names['navigation'],'--network',network,'--restart','no',
            '--cpus','3','--memory','2g','-p','127.0.0.1::8092','--pull=never',images['navigation'],'--physics','http://physics:8080','--frame-id',physical_scope['frame']['id'],'--frame-revision',physical_scope['frame']['revision']])
        created.append(names['navigation'])
        navigation='http://'+command(['docker','port',names['navigation'],'8092/tcp'])
        wait(navigation+'/status',lambda v:v.get('ready'))
        listen='127.0.0.1:'+str(free_port());mission='http://'+listen
        schedule=args.schedule.resolve() if args.schedule else None
        if args.world_watch_site:
            if args.prepare_offline:raise ValueError('Prepare offline World policies from an externally commissioned retained contract; this one-run fixture is online commissioning')
            watch=out/'world-watch'
            spawn([sys.executable,'-u','-m','sim.patrol_world_site','--physics',physics,'--binaries',str(args.world_watch_site.resolve()),'--out',str(watch),'--deadline',str(time.time()+args.duration)],'world_site')
            end=time.time()+30
            while not (watch/'ready.json').exists():
                if time.time()>end:raise RuntimeError('World watch commissioning timed out')
                time.sleep(.1)
            ready=json.loads((watch/'ready.json').read_text());args.world_watch_reader=Path(ready['reader'])
            configured=json.loads(schedule.read_text()) if schedule else dict(revisit_seconds={r['id']:300 for r in get(navigation+'/candidates')['rooms']})
            configured['world_watch']=ready['policy'];schedule=out/'world-watch-schedule.json';schedule.write_text(json.dumps(configured,indent=2)+'\n')
            receipt['world_watch_site']=str(watch)
        if args.configure:
            candidates=get(navigation+'/candidates')['rooms']
            pose=get(navigation+'/status')['pose']
            options=[{'id':'start','name':'Patrol starting position','x':pose['x'],'y':pose['y'],'yaw':pose['yaw']}]
            for room in candidates:
                if room.get('candidates'):
                    options.append(dict(id=room['id'],name=room['name'],**room['candidates'][0]))
            review={'rooms':[{'id':r['id'],'name':r['name']} for r in candidates], 'rendezvous_options':options,'duration_seconds':args.duration,'navigation_url':navigation,'policy_context':get(navigation+'/policy/context'),'offline_prepare':bool(args.prepare_offline)}
            if schedule:
                if schedule.stat().st_size>16384:raise ValueError('Schedule exceeds 16 KiB')
                review.update(json.loads(schedule.read_text()))
            review_input=out/'schedule-review.json';review_input.write_text(json.dumps(review,indent=2)+'\n')
            schedule=out/'schedule.json'
            nativeapp.review(app, '--room-inspection-review' if args.inspection_tour else '--patrol-schedule-review', review_input, schedule,
                lambda value, source: nativeapp.validate_schedule(value, source, tour=args.inspection_tour))
        schedule_args=['--schedule',str(schedule)] if schedule else []
        world_args=['--world-watch-reader',str(args.world_watch_reader.resolve())] if args.world_watch_reader else []
        if schedule:receipt['schedule']=json.loads(schedule.read_text())
        if args.configure and not receipt.get('schedule',{}).get('exclusions'):
            req=urllib.request.Request(navigation+'/policy/reset',data=b'{}',headers={'Content-Type':'application/json'})
            with urllib.request.urlopen(req,timeout=12) as response: json.load(response)
        if args.prepare_offline:
            prepared=json.loads(command([str(out/'world-patrol'),'kit','prepare','--catalog',str(args.prepare_offline.resolve()),'--name',args.kit_name,
                '--source',str(args.source.resolve()),'--assets',str(args.assets.resolve()),'--office',str(ROOT/'examples/collaborative-office/office.xml'),
                '--viewer',str(ROOT/'sim/patrol_viewer.py'),'--physics-image',images['physics'],'--navigation-image',images['navigation'],
                '--physics',physics,'--navigation',navigation,'--duration',str(args.duration),*schedule_args],timeout=90))
            receipt.update(prepared_kit=prepared,preparation_only=True,physics_url=physics,navigation_url=navigation)
            (args.prepare_offline.resolve()/'preparation-inputs.json').write_text(json.dumps(dict(source=str(args.source.resolve()),assets=str(args.assets.resolve()),physics_image=images['physics'],navigation_image=images['navigation']))+'\n')
            for container in reversed(created[1:]):command(['docker','rm','-f',container])
            command(['docker','network','rm',network]);created.clear()
            receipt['preparation_resources_removed']=True;save();print(json.dumps(receipt,indent=2));return
        executor=out/'executor';spawn([str(out/'world-patrol'),'--out',str(executor),'--physics',physics,'--navigation',navigation,'--listen',listen,'--duration',str(args.duration),*schedule_args,*world_args],'executor')
        wait(mission+'/status',lambda v:'patrol' in v,30)
        session=out/'viewer'
        spawn([sys.executable,'-u','-m','sim.patrol_viewer','--physics',physics,'--mission',mission,'--executor-dir',str(executor),'--session',str(session)],'bridge')
        deadline=time.monotonic()+30
        while not (session/'bridge-ready.json').exists():
            if time.monotonic()>deadline:raise RuntimeError('Native viewer bridge not ready')
            time.sleep(.2)
        receipt.update(physics_url=physics,navigation_url=navigation,mission_url=mission,session=str(session))
        if not args.headless:
            spawn([nativeapp.executable(app),'--world-session',str(session)],'viewer')
            receipt['app']=str(app)
        save()
        print(json.dumps(receipt,indent=2))
        print('Patrol continues after this launcher or the viewer closes. Pause, Resume and Stop are in the twin.\nRun details: '+str(out/'run.json'))
    except BaseException:
        save()
        for name in created[1:]:
            result=subprocess.run(['docker','logs','--tail','100',name],capture_output=True,text=True,timeout=20)
            (out/(name+'.log')).write_text(result.stdout+result.stderr)
        for proc in processes:
            if proc.poll() is None:proc.terminate()
        for name in reversed(created[1:]):
            subprocess.run(['docker','rm','-f',name],capture_output=True,timeout=20)
        if created:subprocess.run(['docker','network','rm',network],capture_output=True,timeout=20)
        raise

if __name__=='__main__':main()
