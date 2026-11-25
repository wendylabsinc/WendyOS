#!/usr/bin/env python3
"""Record actual office physics and route-editor callbacks with declared scripted controls.

Uses only this application's framebuffer, never desktop capture. Output remains
private and outside Git. No hardware adapter, clock substitution or robot reset.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import time

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT))

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out',required=True,type=Path)
    parser.add_argument('--coordinator',required=True,type=Path)
    args=parser.parse_args()
    out=args.out.resolve()
    if not args.out.is_absolute() or out.is_relative_to(ROOT) or out.exists():
        parser.error('Choose a fresh absolute private artifact directory outside the repository')
    os.umask(0o077);out.mkdir(parents=True);frames=out/'frames';frames.mkdir()
    import glfw
    import mujoco as mj
    from sim.collaborative_runtime import SceneRuntime
    from sim.collaborative_viewer import OfficeView
    if not glfw.init():raise RuntimeError('Native rendering unavailable')
    glfw.window_hint(glfw.VISIBLE,glfw.FALSE)
    glfw.window_hint(glfw.COCOA_RETINA_FRAMEBUFFER,glfw.FALSE)
    window=glfw.create_window(1440,960,'Wendy route exclusion recording',None,None)
    if window is None:raise RuntimeError('Cannot create native framebuffer')
    glfw.make_context_current(window);glfw.swap_interval(0)
    runtime=SceneRuntime(ROOT/'examples/collaborative-office/scene.json',args.coordinator.resolve(),'route-exclusions-recording')
    view=OfficeView(runtime,window)
    # Use the same display controls as the viewer; keep geometry and physics intact.
    view.flags.update(lidar=False,voxels=False,links=False)
    view.camera.azimuth=125;view.camera.elevation=-62;view.camera.distance=26
    runtime.select_preview('atlas','lounge')
    events=[];samples=[];index=0;stage='Review the original route'
    region={'id':'area-1','min':[-3.5,-2.7],'max':[-3.,-2.3]}
    def record_event(action):
        events.append({'time_s':float(runtime.data.time),'action':action,'preview':runtime.route_preview,'exclusions':runtime.exclusions.copy()})
    def rectangle(lo,hi):
        view.draw()
        viewport=mj.MjrRect(view.panel,0,view.width-view.panel,view.height)
        view.exclusion_corner=None;view.drawing_exclusion=True
        for point in (lo,hi):
            screen=view.project([*point,.025],viewport)
            if screen is None:raise RuntimeError('Rectangle corner outside view')
            recovered=view.floor_point(*screen)
            if recovered is None or math.dist(point,recovered)>1e-5:raise RuntimeError('Floor picking projection differs')
            view.exclusion_click(*screen)
        if not runtime.exclusions:raise RuntimeError('Real rectangle callback refused edit')
    try:
        for step in range(3250): # 65 seconds of genuine advancing MuJoCo time.
            if step==150:
                stage='Close a destination: preview reports unreachable'
                rectangle([-3.3,-3.9],[-2.7,-3.3]);record_event('draw destination exclusion through floor-picking callback')
            elif step==330:
                if runtime.route_preview is None or runtime.route_preview['reachable']:raise RuntimeError('Expected unreachable preview')
                record_event('unreachable preview verified');runtime.set_exclusions([])
                stage='Clear the exclusion: original route returns'
            elif step==480:
                if runtime.route_preview is None or not runtime.route_preview['reachable']:raise RuntimeError('Removal did not restore preview')
                record_event('baseline route restored');rectangle(region['min'],region['max'])
                stage='Add a work area: compare the alternative route'
            elif step==680:
                preview=runtime.route_preview
                if preview is None or not preview['reachable'] or preview['length_m']<=preview['baseline_length_m']:raise RuntimeError('Expected longer reachable detour')
                record_event('reviewed detour')
                if not runtime.start_mission():raise RuntimeError('Reviewed start refused')
                stage='Start the mission: exclusions stay fixed'
                record_event('start through existing application control')
            runtime.tick(.02)
            if runtime.fault:raise RuntimeError(runtime.fault)
            time.sleep(.002) # Allow actual coordinator responses; no synthetic response injection.
            if step%5==0:
                if runtime.completed==6:stage='All six inspections completed around the work area'
                view.recording_caption='RECORDED MUJOCO SIMULATION | SCRIPTED APPLICATION CONTROLS\n'+stage
                view.capture_path=frames/f'{index:06d}.png';view.draw();glfw.poll_events()
                samples.append({'time_s':float(runtime.data.time),'phase':runtime.phase,'completed':runtime.completed,
                                'positions':{rid:runtime._pose(rid)[0][:2] for rid in runtime.links}})
                index+=1
        final=runtime.snapshot()
        frozen=not runtime.set_exclusions([])
        report={'schema':'wendy.route-exclusions.recording.v1','scope':'Actual native application framebuffer and MuJoCo 4 planar physics; scripted controls; no physical robot',
                'fps':10,'frames':index,'duration_s':index/10,'events':events,'final':final,'frozen_edit_refused':frozen,
                'scene_sha256':hashlib.sha256((ROOT/'examples/collaborative-office/scene.json').read_bytes()).hexdigest()}
        (out/'recording.json').write_text(json.dumps(report,indent=2)+'\n')
        (out/'motion.jsonl').write_text(''.join(json.dumps(row)+'\n' for row in samples))
        if final['completed']!=6:raise RuntimeError('Recorded run did not complete all six inspections; partial report retained')
    finally:
        runtime.close();view.close();glfw.destroy_window(window);glfw.terminate()
    with (out/'encode.log').open('w') as log:
        subprocess.run(['ffmpeg','-y','-framerate','10','-i',str(frames/'%06d.png'),'-c:v','libx264','-crf','18','-pix_fmt','yuv420p','-movflags','+faststart',str(out/'route-exclusions.mp4')],check=True,stdout=log,stderr=subprocess.STDOUT,timeout=180)
    print(out/'route-exclusions.mp4')

if __name__=='__main__':main()
