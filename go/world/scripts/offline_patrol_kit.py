"""Activate a locally approved simulation kit without fetching or signing."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid


def activate(args):
    # Import the installed launch helpers only; activation does not read source,
    # assets, office models or binaries from the development checkout.
    spec=importlib.util.spec_from_file_location('patrol_launch',Path(__file__).with_name('go2-patrol.py'))
    launch=importlib.util.module_from_spec(spec);spec.loader.exec_module(launch)
    nativeapp=launch.nativeapp
    if not args.headless:
        args.app=Path(nativeapp.executable(args.app))
    catalog=args.offline_catalog.resolve();tool=catalog/'tool/world-patrol'
    if args.build or args.configure or args.schedule or args.prepare_offline or args.world_watch_site:
        raise ValueError('Offline activation cannot build, replace policy, or prepare authority')
    if not tool.is_file() or tool.is_symlink() or not 0<tool.stat().st_size<=32<<20 or not 0<(catalog/'tool/sha256').stat().st_size<=65 or hashlib.sha256(tool.read_bytes()).hexdigest()!=(catalog/'tool/sha256').read_text().strip():
        raise ValueError('Trusted installed catalog tool is unavailable; preparation is required')
    os.umask(0o077)
    inspect=json.loads(launch.command([str(tool),'kit','inspect','--catalog',str(catalog)],timeout=90))
    selected=args.activate_kit
    if selected is None:
        if args.headless or args.app is None:raise ValueError('Choose --activate-kit explicitly, or supply the installed --app for the native chooser')
        review=catalog/('review-'+uuid.uuid4().hex);review.mkdir(mode=0o700)
        input_path=review/'kits.json';output=review/'selection.json'
        input_path.write_text(json.dumps(inspect))
        try:
            choice=nativeapp.review(args.app, '--offline-patrol-kits', input_path, output, nativeapp.validate_selection)
        except nativeapp.ReviewCancelled:
            return
        if choice.get('action')=='prepare':
            inputs_path=catalog/'preparation-inputs.json'
            if not inputs_path.is_file() or inputs_path.stat().st_size>16384:raise ValueError('Retained preparation input locations unavailable; prepare explicitly with cached source and assets')
            inputs=json.loads(inputs_path.read_text())
            argv=[sys.executable,str(Path(__file__).with_name('go2-patrol.py')),'--prepare-offline',str(catalog),'--configure','--duration',str(args.duration),'--app',str(args.app),'--physics-image',inputs['physics_image'],'--navigation-image',inputs['navigation_image'],'--source',inputs['source'],'--assets',inputs['assets']]
            subprocess.run(argv,check=True);return
        if choice.get('action')!='activate':raise ValueError('Explicit activation was not selected')
        selected=choice.get('id')
    entries=[v for v in inspect if v.get('profile',{}).get('id')==selected and v.get('state')=='ready']
    if len(entries)!=1:raise ValueError('Kit is not ready; inspect its expiry, integrity and one-use claim')
    entry=entries[0];profile=entry['profile'];runtime=profile['runtime']
    if args.app is None and not args.headless:raise ValueError('Offline activation requires an already installed --app')
    info=json.loads(launch.command(['docker','info','--format','{{json .}}']))
    context=launch.command(['docker','context','show'])
    endpoint=json.loads(launch.command(['docker','context','inspect',context]))[0]['Endpoints']['docker']['Host']
    if os.environ.get('DOCKER_HOST') and not os.environ.get('DOCKER_CONTEXT'):endpoint=os.environ['DOCKER_HOST']
    if info.get('OSType')!='linux' or not endpoint.startswith('unix://'):raise ValueError('Local Linux Docker Unix-socket engine required')
    for digest in (runtime['physics_image'],runtime['navigation_image']):
        actual=launch.command(['docker','image','inspect',digest,'--format','{{.Id}}'])
        if actual!=digest:raise ValueError('Approved runtime image unavailable; offline activation never pulls images')
    run_id=profile['run_id'];out=(args.out or Path.home()/'Library/Caches/wendy-world'/run_id).resolve()
    out.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    claim=json.loads(launch.command([str(tool),'kit','claim','--catalog',str(catalog),'--id',selected,'--out',str(out)],timeout=90))
    root=out/'kit';names={'physics':run_id+'-physics','navigation':run_id+'-nav'};network=run_id+'-network';created=[];processes=[]
    receipt=dict(schema='wendy.nav.offline-patrol.run.v1',run_id=run_id,out=str(out),kit=entry,claim=claim,containers=names,network=network,images={'physics':runtime['physics_image'],'navigation':runtime['navigation_image']},hardware=False,simulation=True,offline_activation=True,signer_used=False,package_service_used=False)
    def save():(out/'run.json').write_text(json.dumps(receipt,indent=2)+'\n')
    def spawn(argv,name):
        with (out/(name+'.log')).open('w') as log:proc=subprocess.Popen(argv,cwd=out,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
        processes.append(proc);receipt[name+'_pid']=proc.pid;return proc
    try:
        launch.command(['docker','network','create',network]);created.append(network)
        launch.command(['docker','run','-d','--name',names['physics'],'--network',network,'--network-alias','physics','--restart','no','--cpus','4','--memory','2g','-p','127.0.0.1::8080',
            '-v',str(root/'world/source')+':/source:ro','-v',str(root/'world/assets')+':/assets:ro','-v',str(root/'world')+':/package:ro','--pull=never',runtime['physics_image'],'--source','/source','--assets','/assets','--office','/package/office.xml','--run-id',run_id]);created.append(names['physics'])
        physics='http://'+launch.command(['docker','port',names['physics'],'8080/tcp'])
        launch.wait(physics+'/sensors',lambda v:v.get('ready'));physical=launch.get(physics+'/state')
        if physical['run_id']!=run_id or physical['frame']!=profile['frame'] or physical['model_sha256']!=profile['model_sha256']:raise ValueError('Fresh physics does not match original signed run, frame and model')
        launch.command(['docker','run','-d','--name',names['navigation'],'--network',network,'--restart','no','--cpus','3','--memory','2g','-p','127.0.0.1::8092','--pull=never',runtime['navigation_image'],'--physics','http://physics:8080','--frame-id',physical['frame']['id'],'--frame-revision',physical['frame']['revision']]);created.append(names['navigation'])
        navigation='http://'+launch.command(['docker','port',names['navigation'],'8092/tcp']);launch.wait(navigation+'/status',lambda v:v.get('ready'))
        listen='127.0.0.1:'+str(launch.free_port());mission='http://'+listen
        spawn([str(root/'runtime/world-patrol'),'--out',str(out),'--offline-catalog',str(catalog),'--offline-kit',selected,'--physics',physics,'--navigation',navigation,'--listen',listen,*(['--world-watch-reader',str(args.world_watch_reader.resolve())] if args.world_watch_reader else [])],'executor')
        launch.wait(mission+'/status',lambda v:'patrol' in v,30)
        session=out/'viewer';spawn([sys.executable,'-u',str(root/'runtime/patrol_viewer.py'),'--physics',physics,'--mission',mission,'--executor-dir',str(out),'--session',str(session)],'bridge')
        deadline=time.monotonic()+30
        while not (session/'bridge-ready.json').exists():
            if time.monotonic()>deadline:raise RuntimeError('Native bridge unavailable')
            time.sleep(.2)
        receipt.update(physics_url=physics,navigation_url=navigation,mission_url=mission,session=str(session),activation_boot=json.loads((out/'activation-boot.json').read_text()))
        if not args.headless:spawn([nativeapp.executable(args.app),'--world-session',str(session)],'viewer')
        save();print(json.dumps(receipt,indent=2));print('Original approved expiry remains unchanged. Closing the viewer does not stop the local mission.')
    except BaseException as error:
        receipt['launch_error']=str(error);save()
        for proc in processes:
            if proc.poll() is None:proc.terminate()
        for name in reversed(created[1:]):subprocess.run(['docker','rm','-f',name],capture_output=True,timeout=20)
        if created:subprocess.run(['docker','network','rm',network],capture_output=True,timeout=20)
        raise
