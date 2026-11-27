"""Render both map modes and exercise arc picking through the view callback.

Runs a hidden native OpenGL window. Does not inject operating-system input.
Run from the repository root with the pinned MuJoCo Python environment.
"""
from pathlib import Path
import json,time
from unittest.mock import Mock
import glfw
from sim.collaborative_runtime import SceneRuntime
from sim.collaborative_viewer import OfficeView
import argparse
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--out',type=Path,required=True)
parser.add_argument('--coordinator',type=Path,required=True)
parser.add_argument('-ApplePersistenceIgnoreState',choices=['YES'],help=argparse.SUPPRESS)
args=parser.parse_args()
root=args.out.resolve(); root.mkdir(parents=True,exist_ok=False)
r=SceneRuntime(Path('examples/collaborative-office/scene.json'),args.coordinator,'collab-ui-check')
assert glfw.init()
glfw.window_hint(glfw.VISIBLE,glfw.FALSE)
w=glfw.create_window(1460,1020,'Collaborative renderer verification',None,None)
glfw.make_context_current(w)
v=OfficeView(r,w)
try:
 for i in range(30): r.tick(.02); time.sleep(.005)
 v.capture_path=root/'office.png'; v.draw()
 assert 'atlas--echo' not in {edge_id for edge_id,_ in v.arc_hits},'out-of-range peer arch was drawn'
 assert not r.snapshot()['network']['internet']['up'],'internet must start off'
 rid,points=v.arc_hits[0]; p=points[12]
 before={edge['id']:edge['up'] for edge in r.snapshot()['network']['edges']}
 logical=glfw.get_window_size(w)
 x=p[0]*logical[0]/v.width; y=(v.height-p[1])*logical[1]/v.height
 fake=Mock(wraps=glfw)
 fake.MOUSE_BUTTON_LEFT=glfw.MOUSE_BUTTON_LEFT; fake.PRESS=glfw.PRESS; fake.RELEASE=glfw.RELEASE
 fake.get_cursor_pos.return_value=(x,y); v.glfw=fake
 v.mouse(w,glfw.MOUSE_BUTTON_LEFT,glfw.RELEASE,0)
 after={edge['id']:edge['up'] for edge in r.snapshot()['network']['edges']}
 assert not after[rid],'arc click did not cut correct mesh edge'
 assert all(after[key]==value for key,value in before.items() if key!=rid)
 v.flags['building']=False; v.capture_path=root/'voxel-map.png'; v.draw()
 v.mouse(w,glfw.MOUSE_BUTTON_LEFT,glfw.RELEASE,0)
 restored={edge['id']:edge['up'] for edge in r.snapshot()['network']['edges']}
 assert restored==before,'arc click did not restore mesh edge'
 v.draw()
 internet_points=next(points for edge_id,points in v.arc_hits if edge_id=='hub--internet')
 p=internet_points[12]
 fake.get_cursor_pos.return_value=(p[0]*logical[0]/v.width,(v.height-p[1])*logical[1]/v.height)
 routes=r.snapshot()['network']['routes']
 v.mouse(w,glfw.MOUSE_BUTTON_LEFT,glfw.RELEASE,0)
 assert r.snapshot()['network']['internet']['up'],'internet arc did not enable uplink'
 assert r.snapshot()['network']['routes']==routes,'internet changed local routes'
 v.mouse(w,glfw.MOUSE_BUTTON_LEFT,glfw.RELEASE,0)
 assert not r.snapshot()['network']['internet']['up'],'internet arc did not disable uplink'
 (root/'report.json').write_text(json.dumps({'arc_click_cut_and_restored':rid,'other_links_unchanged':True,'distant_peer_arch_hidden':True,'internet_started_off':True,'internet_toggle_preserves_local_routes':True,'voxel_count':len(r.snapshot()['voxels']),'framebuffer':[v.width,v.height],'tested':'rendered mesh and internet arc projection and mouse callbacks; no operating-system input injected'},indent=2)+'\n')
 print((root/'report.json').read_text())
finally:
 r.close(); v.close(); glfw.destroy_window(w); glfw.terminate()
