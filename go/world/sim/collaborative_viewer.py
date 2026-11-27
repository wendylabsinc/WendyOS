"""Native interactive view of the private cooperative office simulation."""
import argparse
import json
import math
from pathlib import Path
import struct
import time
import zlib

import numpy as np


def arc_points(start, end, height=2.4, segments=24):
    a, b = np.asarray(start), np.asarray(end)
    return [a * (1-t) + b * t + np.array([0, 0, height * 4*t*(1-t)])
            for t in np.linspace(0, 1, segments+1)]


def segment_distance(point, a, b):
    p, a, b = np.asarray(point), np.asarray(a), np.asarray(b)
    d = b-a
    t = np.clip(np.dot(p-a, d) / max(np.dot(d, d), 1e-12), 0, 1)
    return float(np.linalg.norm(p-(a+t*d)))


def save_rgb(path, pixels):
    """Export this application's framebuffer without desktop capture or codecs."""
    height, width, _ = pixels.shape
    def chunk(kind, data):
        return struct.pack('!I', len(data))+kind+data+struct.pack('!I', zlib.crc32(kind+data)&0xffffffff)
    rows = b''.join(b'\x00'+row.tobytes() for row in np.flipud(pixels))
    path.write_bytes(b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR', struct.pack('!2I5B', width, height, 8, 2, 0, 0, 0))
                     +chunk(b'IDAT', zlib.compress(rows))+chunk(b'IEND', b''))


class OfficeView:
    def __init__(self, runtime, window, width=1460, height=920):
        import glfw
        import mujoco as mj
        self.glfw, self.mj = glfw, mj
        self.runtime, self.window = runtime, window
        self.scene = mj.MjvScene(runtime.model, maxgeom=24000)
        self.camera = mj.MjvCamera()
        self.option = mj.MjvOption()
        self.context = mj.MjrContext(runtime.model, mj.mjtFontScale.mjFONTSCALE_150)
        self.flags = dict(lidar=True, voxels=True, routes=True, links=True, building=True)
        self.hub = np.array([0, 0, 1.6])
        self.internet = np.array([0, 7.0, 3.6])
        self.cursor = (0, 0)
        self.press_origin = (0, 0)
        self.pressed = None
        self.dragged = False
        self.buttons = []
        self.editing_routes = True
        self.drawing_exclusion = False
        self.exclusion_corner = None
        self.arc_hits = []
        self.edge_activity = {}
        self.width, self.height = width, height
        self.scale = 1
        self.font_scale = 150
        self.capture_path = None
        self.map_node = 'hub'
        self.panel = 330
        self.snapshot = runtime.snapshot()
        self.reset_camera()
        glfw.set_key_callback(window, self.key)
        glfw.set_mouse_button_callback(window, self.mouse)
        glfw.set_cursor_pos_callback(window, self.move)
        glfw.set_scroll_callback(window, self.scroll)

    def reset_camera(self):
        self.camera.lookat[:] = [0, 0, .5]
        self.camera.distance = 28
        self.camera.azimuth = 125
        self.camera.elevation = -48

    def key(self, window, key, scancode, action, mods):
        g = self.glfw
        if action != g.PRESS:
            return
        toggles = {g.KEY_L: 'lidar', g.KEY_V: 'voxels', g.KEY_P: 'routes',
                   g.KEY_N: 'links', g.KEY_G: 'building'}
        if key in toggles:
            self.flags[toggles[key]] = not self.flags[toggles[key]]
        elif key == g.KEY_SPACE:
            self.runtime.start_mission()
        elif key == g.KEY_X:
            self.runtime.stop_mission()
        elif key == g.KEY_R:
            self.reset_camera()
        elif key == g.KEY_M:
            self.cycle_map()
        elif key in (g.KEY_1, g.KEY_2, g.KEY_3):
            self.runtime.toggle_link(self.snapshot['robots'][key-g.KEY_1]['id'])
        elif key == g.KEY_ESCAPE:
            g.set_window_should_close(window, True)

    def toggle_route_editor(self):
        self.editing_routes = not self.editing_routes
        self.drawing_exclusion = False
        self.exclusion_corner = None

    def floor_point(self, px, py):
        viewport = self.mj.MjrRect(self.panel,0,self.width-self.panel,self.height)
        if px < viewport.left or not viewport.width or not viewport.height:
            return None
        camera = self.mj.mjv_averageCamera(self.scene.camera[0],self.scene.camera[1])
        span = camera.frustum_top-camera.frustum_bottom
        x = ((px-viewport.left)/viewport.width*2-1)*span*viewport.width/viewport.height/2+camera.frustum_center
        y = ((py-viewport.bottom)/viewport.height*2-1)*span/2+(camera.frustum_top+camera.frustum_bottom)/2
        direction = camera.forward*camera.frustum_near + np.cross(camera.forward,camera.up)*x + camera.up*y
        if abs(direction[2]) < 1e-8: return None
        distance = (.025-camera.pos[2])/direction[2]
        if distance <= 0: return None
        return [round(float(v),6) for v in (camera.pos+distance*direction)[:2]]

    def exclusion_click(self, px, py):
        point = self.floor_point(px,py)
        if point is None: return
        if self.exclusion_corner is None:
            self.exclusion_corner = point
            self.runtime._event('Choose the opposite corner on the floor')
            return
        first,self.exclusion_corner = self.exclusion_corner,None
        regions = list(self.runtime.exclusions)
        ident = next('area-'+str(i) for i in range(1,18) if 'area-'+str(i) not in {r['id'] for r in regions})
        region = {'id':ident,'min':[min(first[a],point[a]) for a in (0,1)],'max':[max(first[a],point[a]) for a in (0,1)]}
        self.runtime.set_exclusions(regions+[region])
        self.drawing_exclusion = False

    def cycle_preview(self, kind):
        request=self.runtime.preview_request
        choices=[r['id'] for r in self.runtime.config['robots' if kind=='robot_id' else 'jobs']]
        selected=choices[(choices.index(request[kind])+1)%len(choices)]
        self.runtime.select_preview(selected if kind=='robot_id' else request['robot_id'],selected if kind=='job_id' else request['job_id'])

    def cycle_map(self):
        nodes = ['hub']+[r['id'] for r in self.snapshot['robots']]
        self.map_node = nodes[(nodes.index(self.map_node)+1) % len(nodes)]

    def framebuffer_cursor(self, x, y):
        w, h = self.glfw.get_window_size(self.window)
        return x*self.width/max(w, 1), self.height-y*self.height/max(h, 1)

    def mouse(self, window, button, action, mods):
        g = self.glfw
        x, y = g.get_cursor_pos(window)
        if action == g.PRESS:
            self.pressed, self.cursor, self.dragged = button, (x, y), False
            self.press_origin = (x, y)
        elif action == g.RELEASE:
            if button == g.MOUSE_BUTTON_LEFT and not self.dragged:
                px, py = self.framebuffer_cursor(x, y)
                for (left, bottom, width, height), callback in self.buttons:
                    if left <= px <= left+width and bottom <= py <= bottom+height:
                        callback()
                        break
                else:
                    if getattr(self,'drawing_exclusion',False) and not self.snapshot.get('route_exclusions_frozen',False):
                        self.exclusion_click(px,py)
                    elif self.flags['links']:
                        candidates = [(segment_distance((px, py), a, b), robot)
                                      for robot, points in self.arc_hits
                                      for a, b in zip(points, points[1:]) if a is not None and b is not None]
                        if candidates:
                            distance, robot = min(candidates)
                            if distance < 16:
                                self.runtime.toggle_edge(robot)
            self.pressed = None

    def move(self, window, x, y):
        old = self.cursor
        self.cursor = (x, y)
        if self.pressed is None or self.framebuffer_cursor(*old)[0] < self.panel:
            return
        dx, dy = x-old[0], y-old[1]
        if not self.dragged and math.hypot(x-self.press_origin[0], y-self.press_origin[1]) < 4:
            return
        self.dragged = True
        _, height = self.glfw.get_window_size(window)
        action = (self.mj.mjtMouse.mjMOUSE_MOVE_V if self.pressed == self.glfw.MOUSE_BUTTON_RIGHT
                  else self.mj.mjtMouse.mjMOUSE_ROTATE_V)
        self.mj.mjv_moveCamera(self.runtime.model, action, dx/max(height, 1), dy/max(height, 1), self.camera)

    def scroll(self, window, dx, dy):
        self.mj.mjv_moveCamera(self.runtime.model, self.mj.mjtMouse.mjMOUSE_ZOOM, 0, -.06*dy, self.camera)

    def geom(self, kind, position, size, color, label=''):
        if self.scene.ngeom >= self.scene.maxgeom:
            return None
        geom = self.scene.geoms[self.scene.ngeom]
        self.mj.mjv_initGeom(geom, kind, np.asarray(size, dtype=float), np.asarray(position, dtype=float),
                            np.eye(3).ravel(), np.asarray(color, dtype=np.float32))
        geom.category = int(self.mj.mjtCatBit.mjCAT_DECOR)
        geom.label = label
        self.scene.ngeom += 1
        return geom

    def line(self, a, b, color, width=.018):
        geom = self.geom(self.mj.mjtGeom.mjGEOM_CAPSULE, a, [width]*3, color)
        if geom is not None:
            self.mj.mjv_connector(geom, self.mj.mjtGeom.mjGEOM_CAPSULE, width, np.asarray(a), np.asarray(b))

    def project(self, point, viewport):
        camera = self.mj.mjv_averageCamera(self.scene.camera[0], self.scene.camera[1])
        rel = np.asarray(point) - camera.pos
        depth = float(np.dot(rel, camera.forward))
        if depth <= 0:
            return None
        right = np.cross(camera.forward, camera.up)
        near = camera.frustum_near
        span = camera.frustum_top-camera.frustum_bottom
        half_width = span * viewport.width / viewport.height / 2
        px = (float(np.dot(rel, right))*near/depth-camera.frustum_center)/half_width
        py = (float(np.dot(rel, camera.up))*near/depth-(camera.frustum_top+camera.frustum_bottom)/2)/(span/2)
        return np.array([viewport.left+(px+1)*viewport.width/2, viewport.bottom+(py+1)*viewport.height/2])

    def overlays(self, viewport):
        mj, snap = self.mj, self.snapshot
        for region in snap.get('route_exclusions',[]):
            lo,hi=region['min'],region['max']
            self.geom(mj.mjtGeom.mjGEOM_BOX,[(lo[0]+hi[0])/2,(lo[1]+hi[1])/2,.03],[(hi[0]-lo[0])/2,(hi[1]-lo[1])/2,.025],[1,.22,.08,.32],region['id'])
            padded=[[lo[0]-.32,lo[1]-.32,.04],[hi[0]+.32,lo[1]-.32,.04],[hi[0]+.32,hi[1]+.32,.04],[lo[0]-.32,hi[1]+.32,.04]]
            for a,b in zip(padded,padded[1:]+padded[:1]):self.line(a,b,[1,.35,.1,.8],.025)
        if getattr(self,'exclusion_corner',None) is not None:
            self.geom(mj.mjtGeom.mjGEOM_SPHERE,[*self.exclusion_corner,.08],[.09]*3,[1,.4,.1,1],'First corner')
        preview=snap.get('route_preview')
        if preview and not snap.get('route_exclusions_frozen'):
            for key,color,height in [('baseline_path',[.65,.65,.65,.65],.05),('path',[.1,.9,.95,1],.09)]:
                points=[[p[0],p[1],height] for p in preview.get(key) or []]
                for a,b in zip(points,points[1:]):self.line(a,b,color,.035)
        colors = {r['id']: list(r['color']) for r in snap['robots']}
        for job in snap.get('jobs', []):
            position = job.get('position')
            if not position:
                continue
            done = job.get('state') == 'completed'
            self.geom(mj.mjtGeom.mjGEOM_SPHERE, [*position, .09], [.09]*3,
                      [.2, .95, .58, 1] if done else [.97, .72, .27, 1], job['label'])
        self.arc_hits = []
        for robot in snap['robots']:
            position = list(robot['position'])
            if len(position) == 2:
                position.append(.24)
            color = colors[robot['id']]
            self.geom(mj.mjtGeom.mjGEOM_SPHERE, [position[0], position[1], 1.05], [.07]*3,
                      color+[1], robot['name'])
            if self.flags['routes']:
                path = [[position[0], position[1], .08]] + [[p[0], p[1], .08] for p in robot.get('path', [])]
                for a, b in zip(path, path[1:]):
                    self.line(a, b, color+[.75], .025)
            if self.flags['lidar']:
                for point in robot.get('lidar_points', []):
                    self.geom(mj.mjtGeom.mjGEOM_SPHERE, point, [.025]*3, color+[1])
        if self.flags['links']:
            self.geom(mj.mjtGeom.mjGEOM_SPHERE, self.hub, [.13]*3, [.7, .9, 1, 1], 'SITE HUB')
            positions = {r['id']: [r['position'][0], r['position'][1], .9] for r in snap['robots']}
            positions['hub'] = self.hub
            colors['hub'] = [.65, .85, 1]
            positions['internet'] = self.internet
            colors['internet'] = [.65, .78, .85]
            network = snap.get('network', {})
            internet = network.get('internet')
            if internet:
                self.geom(mj.mjtGeom.mjGEOM_SPHERE, self.internet, [.22]*3, [.65, .78, .85, 1],
                          'INTERNET '+('ON' if internet['up'] else 'OFF'))
            now = time.monotonic()
            edges = network.get('edges', []) + ([internet] if internet else [])
            for index, edge in enumerate(edges):
                if edge.get('in_range') is False:
                    continue  # A distant peer is not a usable radio edge.
                points = arc_points(positions[edge['a']], positions[edge['b']], height=1.6+index*.25)
                color = ((np.array(colors[edge['a']])+np.array(colors[edge['b']]))/2).tolist()
                link_color = color+[.9] if edge['up'] else [.95, .2, .24, .8]
                for i, (a, b) in enumerate(zip(points, points[1:])):
                    if edge['up'] or i % 3 != 1:
                        self.line(a, b, link_color, .035)
                count, changed = self.edge_activity.get(edge['id'], (-1, now))
                packets = edge.get('packets', 0)
                if count != packets:
                    changed = now
                self.edge_activity[edge['id']] = (packets, changed)
                if edge['up'] and packets > 0 and now-changed < .5:
                    packet = points[min(len(points)-1, int((now-changed)*2*len(points)))]
                    self.geom(mj.mjtGeom.mjGEOM_SPHERE, packet, [.065]*3, [1, 1, .85, 1])
                self.arc_hits.append((edge['id'], [self.project(p, viewport) for p in points]))
        if self.flags['voxels']:
            for voxel in snap.get('voxels', [])[-12000:]:
                color = colors.get(voxel.get('source'), [.35, .8, .9])
                self.geom(mj.mjtGeom.mjGEOM_BOX, voxel['position'], [.105]*3, color+[.45])

    def rect(self, box, color):
        self.mj.mjr_rectangle(self.mj.MjrRect(*(int(v*self.scale) for v in box)), *color)

    def text(self, x, top, text, color=None, width=None):
        box = self.mj.MjrRect(int(x*self.scale), 0, int((width or 330-x-10)*self.scale), int(top*self.scale))
        self.mj.mjr_overlay(self.mj.mjtFont.mjFONT_NORMAL, self.mj.mjtGridPos.mjGRID_TOPLEFT,
                            box, text, '', self.context)

    def button(self, x, top, width, label, callback, active=False):
        box = (x, top-31, width, 29)
        self.rect(box, (.12, .29, .31, 1) if active else (.14, .18, .23, 1))
        self.text(x+5, top-2, label, width=width-8)
        self.buttons.append((tuple(v*self.scale for v in box), callback))

    def panel_draw(self):
        snap = self.snapshot
        height = self.height/self.scale
        self.buttons = []
        self.rect((0, 0, 330, height), (.045, .065, .085, 1))
        self.text(16, height-16, 'WENDY WORLD\nCollaborative office')
        internet_up = snap.get('network', {}).get('internet', {}).get('up', False)
        self.text(16, height-72, 'SYNTHETIC SIMULATION\nInternet '+('ON' if internet_up else 'OFF')+' | Radio range 6 m')
        self.text(16, height-126, f"{snap.get('phase', 'ready').upper().replace('_', ' ')}\nInspections {snap.get('completed',0)} / {snap.get('total',6)}")
        self.button(16, height-185, 178, 'START TEAM [Space]', self.runtime.start_mission)
        self.button(204, height-185, 108, 'STOP [X]', self.runtime.stop_mission)
        top = height-230
        if not snap.get('route_exclusions_frozen',False):
            self.button(16,top,296,'ROUTE EDITOR' if not self.editing_routes else 'NETWORK AND ROBOTS',self.toggle_route_editor)
            if self.editing_routes:
                self.route_panel(top-45)
                return
            top -= 42
        for index, robot in enumerate(snap['robots']):
            color = list(robot['color'])+[1]
            self.rect((16, top-16, 5, 16), color)
            self.text(29, top+4, robot['name'].upper()+'  '+('CONNECTED' if robot['online'] else 'ISOLATED'))
            state = str(robot.get('state', 'idle')).replace('_', ' ')
            route = snap.get('network', {}).get('routes', {}).get(robot['id'], [])
            via = ' -> '.join(route[1:]) if route else 'No route to hub'
            self.text(18, top-25, state[:34]+'\n'+(str(robot.get('job_id') or 'Awaiting assignment'))[:34]+'\n'+via[:34])
            direct = robot.get('direct_link_enabled', robot.get('direct_link_up', robot['online']))
            self.button(16, top-81, 140, f"{'CUT' if direct else 'ADD'} HUB [{index+1}]",
                        lambda rid=robot['id']: self.runtime.toggle_link(rid), direct)
            self.button(168, top-81, 144, 'PAUSE ROBOT' if robot['available'] else 'RESUME ROBOT',
                        lambda rid=robot['id']: self.runtime.toggle_available(rid), not robot['available'])
            top -= 134
        self.text(16, top+3, 'OVERLAYS')
        for index, (flag, label) in enumerate([('lidar','LiDAR [L]'),('voxels','Voxels [V]'),('routes','Routes [P]'),('links','Links [N]'),('building','Building [G]')]):
            self.button(16+(index%2)*152, top-30-(index//2)*36, 144,
                        ('ON  ' if self.flags[flag] else 'OFF ')+label,
                        lambda key=flag: self.flags.__setitem__(key, not self.flags[key]), self.flags[flag])
        self.button(16, top-143, 296, f'MAP: {self.map_node.upper()} [M]', self.cycle_map, True)
        bottom = max(20, top-195)
        self.text(16, bottom, f"{self.map_node.title()} occupied voxels: {len(snap.get('voxels', []))}\nUnseen space remains unknown")
        viewport = self.mj.MjrRect(self.panel+15, 0, self.width-self.panel-30, self.height-15)
        self.mj.mjr_overlay(self.mj.mjtFont.mjFONT_NORMAL, self.mj.mjtGridPos.mjGRID_TOPLEFT, viewport,
            'Click an arch to toggle its link | Radio links over 6 m are hidden\nDrag to orbit | right-drag to pan | scroll to zoom | R reset', '', self.context)
        events = snap.get('events', [])
        self.mj.mjr_overlay(self.mj.mjtFont.mjFONT_NORMAL, self.mj.mjtGridPos.mjGRID_BOTTOMLEFT, viewport,
            '\n'.join(str(e.get('text', e) if isinstance(e, dict) else e)[:110] for e in events[-3:]), '', self.context)

    def route_panel(self, top):
        snap=self.snapshot
        request=snap.get('preview_request',{})
        self.text(16,top,'PRE-FLIGHT ROUTE REVIEW')
        self.button(16,top-48,296,'ROBOT: '+request.get('robot_id',''),lambda:self.cycle_preview('robot_id'))
        self.button(16,top-88,296,'DESTINATION: '+request.get('job_id',''),lambda:self.cycle_preview('job_id'))
        preview=snap.get('route_preview')
        if preview:
            status=(f"Route {preview['length_m']:.2f} m | Base {preview['baseline_length_m']:.2f} m" if preview['reachable'] else 'DESTINATION UNREACHABLE')
        else:status='Computing current preview...'
        self.text(16,top-110,status+'\nCyan: preview | Grey: base route')
        def drawing():
            self.drawing_exclusion=not self.drawing_exclusion;self.exclusion_corner=None
        self.button(16,top-184,296,'CANCEL DRAW' if self.drawing_exclusion else 'DRAW EXCLUSION: TWO FLOOR CLICKS',drawing,self.drawing_exclusion)
        self.button(16,top-224,140,'UNDO LAST',lambda:self.runtime.set_exclusions(self.runtime.exclusions[:-1]))
        self.button(168,top-224,144,'CLEAR ALL',lambda:self.runtime.set_exclusions([]))
        regions=snap.get('route_exclusions',[])
        self.text(16,top-249,f'{len(regions)} / 16 operator exclusions\nOrange border includes 0.32 m clearance')
        lines=[f"{r['id']}: ({r['min'][0]:.1f},{r['min'][1]:.1f}) - ({r['max'][0]:.1f},{r['max'][1]:.1f})" for r in regions[-5:]]
        self.text(16,top-303,'\n'.join(lines) or 'No exclusions. Base geometry is unchanged.')
        self.text(16,top-410,'Preview uses current peer reservations.\nTeam assignments are chosen at Start.\nExclusions freeze for the whole run.\nThis is a simulation-only operator policy.')
        viewport=self.mj.MjrRect(self.panel+15,0,self.width-self.panel-30,self.height-15)
        prompt=('Click two opposite corners on the FLOOR. Right-drag pans; scroll zooms.' if self.drawing_exclusion else 'Review route exclusions before START TEAM. No motion is issued by preview.')
        self.mj.mjr_overlay(self.mj.mjtFont.mjFONT_NORMAL,self.mj.mjtGridPos.mjGRID_TOPLEFT,viewport,prompt,'',self.context)
        events=snap.get('events',[])
        self.mj.mjr_overlay(self.mj.mjtFont.mjFONT_NORMAL,self.mj.mjtGridPos.mjGRID_BOTTOMLEFT,viewport,'\n'.join(e['text'][:115] for e in events[-3:]),'',self.context)

    def draw(self):
        self.width, self.height = self.glfw.get_framebuffer_size(self.window)
        logical_width, _ = self.glfw.get_window_size(self.window)
        self.scale = self.width/max(logical_width, 1)
        self.panel = int(330*self.scale)
        font_scale = min(300, max(100, int(round(100*self.scale/50))*50))
        if font_scale != self.font_scale:
            self.mj.mjr_changeFont(font_scale, self.context)
            self.font_scale = font_scale
        self.snapshot = self.runtime.snapshot(map_node=self.map_node)
        self.option.geomgroup[0] = int(self.flags['building'])
        viewport = self.mj.MjrRect(self.panel, 0, max(1, self.width-self.panel), max(1, self.height))
        self.mj.mjv_updateScene(self.runtime.model, self.runtime.data, self.option, None,
                               self.camera, self.mj.mjtCatBit.mjCAT_ALL, self.scene)
        self.overlays(viewport)
        self.mj.mjr_render(viewport, self.scene, self.context)
        self.panel_draw()
        caption=getattr(self,'recording_caption',None)
        if caption:
            self.mj.mjr_overlay(self.mj.mjtFont.mjFONT_NORMAL,self.mj.mjtGridPos.mjGRID_BOTTOMRIGHT,
                self.mj.MjrRect(self.panel+15,15,self.width-self.panel-30,self.height-30),caption,'',self.context)
        if self.capture_path:
            rgb = np.zeros((self.height, self.width, 3), dtype=np.uint8)
            self.mj.mjr_readPixels(rgb, None, self.mj.MjrRect(0, 0, self.width, self.height), self.context)
            save_rgb(self.capture_path, rgb)
            self.capture_path = None
        self.glfw.swap_buffers(self.window)

    def close(self):
        self.context.free()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scene', type=Path, required=True)
    parser.add_argument('--coordinator', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--run-id', required=True)
    parser.add_argument('--duration', type=int, default=1800)
    parser.add_argument('-ApplePersistenceIgnoreState', choices=['YES'], help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not 1 <= args.duration <= 3600:
        parser.error('duration must be 1..3600 seconds')
    import glfw
    from sim.collaborative_runtime import SceneRuntime
    runtime = SceneRuntime(args.scene, args.coordinator, args.run_id)
    window, view = None, None
    try:
        if not glfw.init():
            raise RuntimeError('Native display unavailable: GLFW initialization failed')
        glfw.window_hint(glfw.SAMPLES, 4)
        window = glfw.create_window(1460, 1020, 'Wendy World | Collaborative Office', None, None)
        if window is None:
            raise RuntimeError('Could not create the native 3D window')
        glfw.make_context_current(window)
        glfw.swap_interval(1)
        view = OfficeView(runtime, window)
        start, last, save = time.monotonic(), time.monotonic(), 0
        printed = False
        while not glfw.window_should_close(window) and time.monotonic()-start < args.duration and not (args.out/'stop').exists():
            now = time.monotonic()
            runtime.tick(min(.04, now-last))
            last = now
            if not printed or (args.out/'capture').exists():
                view.capture_path = args.out/'office-view.png'
                (args.out/'capture').unlink(missing_ok=True)
            view.draw()
            glfw.poll_events()
            if not printed:
                print(json.dumps({'viewer':'ready','run_id':args.run_id,'synthetic':True}), flush=True)
                printed = True
            if now-save > 1:
                (args.out/'state.json').write_text(json.dumps(runtime.snapshot(), allow_nan=False))
                save = now
        (args.out/'final-state.json').write_text(json.dumps(runtime.snapshot(), allow_nan=False))
    finally:
        runtime.close()
        if view:
            view.close()
        if window:
            glfw.destroy_window(window)
        glfw.terminate()


if __name__ == '__main__':
    main()
