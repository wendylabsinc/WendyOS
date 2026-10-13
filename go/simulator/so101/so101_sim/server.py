"""A virtual SO-101. No USB, serial or hardware backend."""
import asyncio
from contextlib import asynccontextmanager
import io
import math
import os
from pathlib import Path
import threading
import time

import mujoco
import numpy as np
from PIL import Image
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import FileResponse, Response, StreamingResponse
from pydantic import BaseModel, ConfigDict, Field
from urllib.parse import urlsplit
from .scene import export_scene, compact_json, numbers, xyzw

ROOT = Path(__file__).resolve().parent
MODEL_ROOT = ROOT.parent / 'model'
MODEL_BUNDLE = 'so101-follower-a758567-v1'
NAMES = ['shoulder_pan', 'shoulder_lift', 'elbow_flex', 'wrist_flex', 'wrist_roll', 'gripper']
HOME = np.array([0, 0, 0, 0, 0, 0.4])


class Simulator:
    def __init__(self):
        self.lock = threading.RLock()
        self.stop = threading.Event()
        self.ready = threading.Event()
        self.error = None
        self.frame = None
        self.frame_id = 0
        self.paused = False
        self.epoch = 0
        self.last_frame = 0.0
        self.last_physics = 0.0
        self.demo_until = 0
        self.azimuth = 135.0
        self.elevation = -25.0
        self.distance = 0.8
        self.model = mujoco.MjModel.from_xml_path(str(MODEL_ROOT / 'scene.xml'))
        self.data = mujoco.MjData(self.model)
        self.jids = np.array([mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_JOINT, n) for n in NAMES])
        self.qids = self.model.jnt_qposadr[self.jids]
        self.aids = np.array([mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_ACTUATOR, n) for n in NAMES])
        self.limits = self.model.actuator_ctrlrange[self.aids]
        self.tip = mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_SITE, 'gripperframe')
        self.scene = export_scene(self.model)
        self.reset()

    def reset(self):
        with self.lock:
            mujoco.mj_resetData(self.model, self.data)
            self.targets = HOME.copy()
            self.command = HOME.copy()
            self.data.qpos[self.qids] = HOME
            self.data.ctrl[self.aids] = HOME
            self.demo_until = 0
            self.paused = False
            self.epoch += 1
            mujoco.mj_forward(self.model, self.data)

    def status(self):
        with self.lock:
            now = time.monotonic()
            healthy = (self.ready.is_set() and self.error is None
                       and now - self.last_frame < 10 and now - self.last_physics < 2)
            return {'robot': 'so101', 'robot_kind': 'so101', 'backend': 'mujoco', 'virtual_only': True,
                    'vm_name': os.getenv('SO101_VM_NAME', ''), 'simulation': True, 'profile_version': 1,
                    'source_digest': os.getenv('SO101_SOURCE_DIGEST', ''), 'policy_bundle': MODEL_BUNDLE,
                    'world': os.getenv('SO101_WORLD', 'indoor'), 'clock_mode': 'device',
                    'seed': int(os.getenv('SO101_SEED', '0')), 'visual_detail': os.getenv('SO101_VISUAL_DETAIL', 'balanced'),
                    'dds_isolation': 'disabled', 'healthy': healthy, 'epoch': self.epoch,
                    'mode': 'paused' if self.paused else 'joint-control',
                    'ready': healthy, 'error': self.error,
                    'paused': self.paused, 'demo': self.demo_until > time.monotonic(),
                    'simulation_seconds': round(self.data.time, 3), 'frames': self.frame_id,
                    'frame_age_ms': round((now - self.last_frame) * 1000),
                    'physics_age_ms': round((now - self.last_physics) * 1000),
                    'tip_m': self.data.site_xpos[self.tip].tolist(),
                    'joints': [{'name': name, 'position_deg': float(np.rad2deg(self.data.qpos[q])),
                                'target_deg': float(np.rad2deg(self.targets[i])),
                                'min_deg': float(np.rad2deg(self.limits[i, 0])),
                                'max_deg': float(np.rad2deg(self.limits[i, 1]))}
                               for i, (name, q) in enumerate(zip(NAMES, self.qids))]}

    def step(self):
        # Called under the physics lock, with a bounded virtual velocity.
        step = 0.8 * self.model.opt.timestep
        self.command += np.clip(self.targets - self.command, -step, step)
        self.data.ctrl[self.aids] = self.command
        mujoco.mj_step(self.model, self.data)
        if not np.isfinite(self.data.qpos).all():
            raise RuntimeError('Non-finite simulation state')

    def physics(self):
        previous = time.monotonic()
        debt = 0.0
        try:
            while not self.stop.is_set():
                start = time.monotonic()
                elapsed = min(start - previous, 0.1)
                previous = start
                with self.lock:
                    if self.demo_until > start and not self.paused:
                        phase = (8 - (self.demo_until - start)) * math.pi / 2
                        self.targets = HOME.copy()
                        self.targets[0] = 0.45 * math.sin(phase)
                        self.targets[4] = 0.6 * math.sin(phase)
                        self.targets[5] = 0.5 + 0.3 * math.sin(phase)
                    elif self.demo_until and start >= self.demo_until:
                        self.demo_until = 0
                        self.targets = HOME.copy()
                    if not self.paused:
                        debt += elapsed
                        steps = int(debt / self.model.opt.timestep)
                        debt -= steps * self.model.opt.timestep
                        for _ in range(steps):
                            self.step()
                    self.last_physics = time.monotonic()
                self.stop.wait(max(0, self.model.opt.timestep - (time.monotonic() - start)))
        except Exception as exc:
            with self.lock:
                self.error = str(exc)
            self.ready.set()
            self.stop.set()

    def run(self):
        renderer = None
        physics = threading.Thread(target=self.physics, daemon=True)
        physics.start()
        try:
            # OpenGL stays on one thread. Render a private copy so slow CPU
            # rendering cannot hold up physics, status or joint commands.
            renderer = mujoco.Renderer(self.model, height=480, width=640)
            rendered = mujoco.MjData(self.model)
            camera = mujoco.MjvCamera()
            camera.lookat[:] = [0.05, 0, 0.16]
            options = mujoco.MjvOption()
            options.geomgroup[3] = 0
            while not self.stop.is_set():
                start = time.monotonic()
                with self.lock:
                    rendered.qpos[:] = self.data.qpos
                    rendered.qvel[:] = self.data.qvel
                    camera.azimuth, camera.elevation, camera.distance = self.azimuth, self.elevation, self.distance
                    epoch = self.epoch
                mujoco.mj_forward(self.model, rendered)
                renderer.update_scene(rendered, camera=camera, scene_option=options)
                pixels = renderer.render()
                output = io.BytesIO()
                Image.fromarray(pixels).save(output, format='JPEG', quality=82)
                with self.lock:
                    # A reset during a slow render cannot publish an old image
                    # as the first frame of the new epoch.
                    if epoch == self.epoch:
                        self.frame = output.getvalue()
                        self.frame_id += 1
                        self.last_frame = time.monotonic()
                        self.ready.set()
                self.stop.wait(max(0, 1 / 4 - (time.monotonic() - start)))
        except Exception as exc:
            with self.lock:
                self.error = str(exc)
            self.ready.set()
            import traceback
            traceback.print_exc()
        finally:
            self.stop.set()
            physics.join(5)
            if renderer:
                renderer.close()


sim = None


@asynccontextmanager
async def lifespan(app):
    global sim
    sim = Simulator()
    worker = threading.Thread(target=sim.run, daemon=True)
    worker.start()
    try:
        if not await asyncio.to_thread(sim.ready.wait, 60) or sim.error:
            raise RuntimeError(sim.error or 'Renderer startup timed out')
        print('SO-101 simulator ready; virtual model only; HTTP :8890', flush=True)
        yield
    finally:
        sim.stop.set()
        await asyncio.to_thread(worker.join, 5)


app = FastAPI(title='SO-101 virtual arm', lifespan=lifespan)


@app.middleware('http')
async def local_control(request: Request, call_next):
    host = request.url.hostname
    if host not in {'127.0.0.1', 'localhost', '::1', 'testserver'}:
        return Response('Invalid simulator host', status_code=403)
    if request.method == 'POST':
        origin = request.headers.get('origin')
        if ((origin and urlsplit(origin).netloc != request.headers.get('host'))
                or request.headers.get('sec-fetch-site') == 'cross-site'):
            return Response('Cross-origin control is disabled', status_code=403)
        body = bytearray()
        async for chunk in request.stream():
            body.extend(chunk)
            if len(body) > 4096:
                return Response('Request is too large', status_code=413)
        request._body = bytes(body)
        if not sim or not sim.status()['healthy']:
            return Response('Simulation unavailable', status_code=503)
    return await call_next(request)


@app.get('/')
def index():
    return FileResponse(ROOT / 'index.html')


@app.get('/health')
@app.get('/api/health')
def health():
    if not sim or not sim.status()['healthy']:
        raise HTTPException(503, 'Simulator unavailable')
    return {'status': 'ok', 'robot': 'so101', 'virtual_only': True}


@app.get('/api/state')
@app.get('/api/status')
def state():
    return sim.status()


@app.get('/frame.jpg')
@app.get('/camera.jpg')
def snapshot():
    with sim.lock:
        if not sim.status()['healthy']:
            raise HTTPException(503, 'No current rendered frame')
        return Response(sim.frame, media_type='image/jpeg', headers={'Cache-Control': 'no-store'})


@app.get('/stream')
async def stream():
    async def frames():
        last = -1
        while not sim.stop.is_set() and not sim.error:
            with sim.lock:
                frame, frame_id = sim.frame, sim.frame_id
            if frame and frame_id != last:
                last = frame_id
                yield b'--frame\r\nContent-Type: image/jpeg\r\n\r\n' + frame + b'\r\n'
            await asyncio.sleep(1 / 15)
    return StreamingResponse(frames(), media_type='multipart/x-mixed-replace; boundary=frame')


class Pose(BaseModel):
    model_config = ConfigDict(extra='forbid')
    joints_deg: dict[str, float] = Field(min_length=1)


@app.post('/api/pose')
def pose(request: Pose):
    commands = []
    for name, degrees in request.joints_deg.items():
        if name not in NAMES or not math.isfinite(degrees):
            raise HTTPException(422, f'Invalid joint: {name}')
        i = NAMES.index(name)
        radians = math.radians(degrees)
        if not sim.limits[i, 0] <= radians <= sim.limits[i, 1]:
            raise HTTPException(422, f'Out of range: {name}')
        commands.append((i, radians))
    with sim.lock:
        sim.demo_until = 0
        for i, radians in commands:
            sim.targets[i] = radians
    return sim.status()


@app.post('/api/reset')
def reset():
    sim.reset()
    return sim.status()


class Pause(BaseModel):
    model_config = ConfigDict(extra='forbid')
    paused: bool


@app.post('/api/pause')
def pause(request: Pause):
    with sim.lock:
        sim.paused = request.paused
    return sim.status()


@app.post('/api/demo')
def demo():
    with sim.lock:
        sim.paused = False
        sim.demo_until = time.monotonic() + 8
    return sim.status()


class Camera(BaseModel):
    model_config = ConfigDict(extra='forbid')
    azimuth: float = Field(ge=-360, le=360, allow_inf_nan=False)
    elevation: float = Field(ge=-85, le=-5, allow_inf_nan=False)
    distance: float = Field(ge=0.35, le=1.5, allow_inf_nan=False)


@app.post('/api/camera')
def camera(request: Camera):
    with sim.lock:
        sim.azimuth, sim.elevation, sim.distance = request.azimuth, request.elevation, request.distance
    return {'ok': True}


@app.get('/api/scene')
def scene():
    return Response(compact_json(sim.scene), media_type='application/json')


@app.get('/api/scene/state')
def scene_state():
    with sim.lock:
        valid = bool(np.isfinite(sim.data.qpos).all()) and sim.status()['healthy']
        if not valid:
            raise HTTPException(503, 'Simulation unavailable')
        return {'scene_id': sim.scene['id'], 'epoch': sim.epoch, 'generation': sim.epoch,
                'time': float(sim.data.time), 'mode': sim.status()['mode'], 'valid': valid,
                'positions': numbers(sim.data.xpos), 'quaternions': xyzw(sim.data.xquat)}


@app.get('/{asset:path}')
def asset(asset: str):
    allowed = {'viewer.js', 'lidar-view.js', 'vendor/three.module.js',
               'vendor/three.core.js', 'vendor/OrbitControls.js'}
    if asset not in allowed:
        raise HTTPException(404, 'Not found')
    return FileResponse(ROOT / asset, media_type='text/javascript')
