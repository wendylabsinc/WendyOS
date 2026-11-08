"""Fixed SensorLink camera search with a supervised Go2 approach."""
from contextlib import asynccontextmanager
from pathlib import Path
import base64
import json
import logging
import os
import threading
import time

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import FileResponse, Response
from core import ITEMS
from controller import Controller

ROOT = Path(__file__).resolve().parent
log = logging.getLogger('camera-search')


class Runtime:
    def __init__(self):
        from hardware import Agent, Twin
        self.agent = Agent()
        self.controller = Controller(Twin())
        self.camera = None
        self.camera_lock = threading.Lock()
        self.image_lock = threading.Lock()
        self.jpeg, self.image_at, self.width, self.height = b'', 0., 0, 0
        self.vision_error = 'Select a camera to start perception.'
        self.detections = []
        self.raw = None
        self.tracker = None
        self.search_generation = 0
        self.search_event = threading.Event()
        self.quit = threading.Event()
        self.alignment_lock = threading.Lock()
        self.threads = [threading.Thread(target=self.control_loop,daemon=True),threading.Thread(target=self.vision_loop,daemon=True),threading.Thread(target=self.ground_loop,daemon=True)]
        for thread in self.threads:
            thread.start()
        log.warning('Go2 Camera Search is idle. Open http://<gateway>:%s/', os.environ.get('PORT','18993'))

    def control_loop(self):
        while not self.quit.wait(.15):
            self.controller.tick()

    def vision_loop(self):
        import cv2
        previous = 0
        while not self.quit.wait(.02):
            with self.camera_lock:
                camera = self.camera
            if camera is None:
                continue
            frame, error = camera.read()
            if error:
                self.vision_error = error
            if not frame or frame[0] <= previous or time.monotonic()-frame[0] > 1.5:
                continue
            at, image = frame
            previous = at
            try:
                with self.image_lock:
                    self.raw = (at,image.copy())
                    tracker = self.tracker
                detections = tracker.update(image) if tracker else []
                # A slow model must not turn an old image into a fresh observation.
                if time.monotonic()-at > 1.5:
                    self.vision_error = 'Camera processing is too slow. Waiting for a live frame.'
                    continue
                with self.camera_lock:
                    if camera is not self.camera:
                        continue
                    if tracker is not None:
                        self.controller.observe(detections,at,image.shape[1],image.shape[0])
                image = image.copy()
                for d in detections:
                    x,y,w,h = map(round,d['box'])
                    cv2.rectangle(image,(x,y),(x+w,y+h),(70,220,130),2)
                    cv2.putText(image,f"{d['name']} {d['confidence']:.0%}",(x,max(18,y-6)),cv2.FONT_HERSHEY_SIMPLEX,.5,(70,220,130),1)
                ok, encoded = cv2.imencode('.jpg',image,[cv2.IMWRITE_JPEG_QUALITY,80])
                if ok:
                    with self.image_lock:
                        self.jpeg,self.image_at = encoded.tobytes(),at
                        self.width,self.height = image.shape[1],image.shape[0]
                        self.detections = detections
                        self.vision_error = ''
            except Exception as error:
                self.vision_error = str(error)

    def find(self, owner, query):
        self.controller.search(owner, query)
        with self.image_lock:
            self.tracker = None
            self.search_generation += 1
        self.controller.message = 'Looking for ' + self.controller.item + '…'
        self.search_event.set()

    def ground_loop(self):
        from vision import Grounder, Tracker
        grounder = Grounder()
        while not self.quit.is_set():
            if not self.search_event.wait(.2):
                continue
            self.search_event.clear()
            with self.image_lock:
                generation, source = self.search_generation, self.raw
            with self.controller.lock:
                query = self.controller.item
            try:
                if source is None or time.monotonic()-source[0] > 1.5:
                    raise ValueError('Wait for a live camera image, then try Find again.')
                hit = grounder.locate(source[1],query)
                tracker = Tracker(source[1],hit)
                with self.image_lock:
                    if generation != self.search_generation or self.quit.is_set():
                        continue
                    current = self.raw
                    if current is None or time.monotonic()-current[0] > 1.5 or not tracker.update(current[1]):
                        raise ValueError('The scene changed while I was looking. Try Find again.')
                    self.tracker = tracker
                with self.controller.lock:
                    if self.controller.item == query:
                        self.controller.message = 'Found a possible ' + query + '. Checking its position…'
            except Exception as error:
                with self.image_lock:
                    current = generation == self.search_generation
                if current:
                    with self.controller.lock:
                        self.controller.phase = 'not-found'
                        self.controller.message = str(error)

    def recalibrate_camera(self, owner):
        c = self.controller
        with c.lock:
            c.assert_owner(owner)
            if not c.scene or not c.camera_id:
                raise ValueError('Choose a camera and room first.')
            if not self.alignment_lock.acquire(blocking=False):
                raise ValueError('Camera alignment is still finishing. Wait a moment and retry.')
            try:
                c.stop('Recalibrating camera')
                c.calibration = None
                c.camera_overlay = []
                c.camera_proposal = None
                c.item = ''
                c.history.clear()
                c.observed = None
                c.phase = 'camera-calibrating'
                c.camera_message = 'Matching the camera to saved room features. Keep the camera fixed.'
                epoch, frame, camera_id = c.calibration_epoch, c.frame, c.camera_id
            except Exception:
                self.alignment_lock.release()
                raise
        threading.Thread(target=self._align_camera,args=(owner,epoch,frame,camera_id),daemon=True).start()

    def _align_camera(self, owner, epoch, frame, camera_id):
        c = self.controller
        token = None
        def current():
            return (not self.quit.is_set() and c.owner == owner and c.calibration_epoch == epoch
                    and c.frame == frame and c.camera_id == camera_id and c.phase == 'camera-calibrating')
        def fresh(after=0):
            deadline=time.monotonic()+8
            while time.monotonic()<deadline:
                with c.lock:
                    if not current():raise ValueError('Camera alignment cancelled.')
                with self.camera_lock:
                    source=self.camera.calibration_frame() if self.camera else None
                    if source and source[0]>after and time.monotonic()-source[0]<1.5:
                        at,gray,sequence=source
                        return dict(width=gray.shape[1],height=gray.shape[0],codec='H.264',sequence=sequence,
                                    ageMS=max(0,(time.monotonic()-at)*1000),gray=base64.b64encode(gray.tobytes()).decode())
                if self.quit.wait(.1):break
            raise ValueError('Waiting for a fresh camera frame. Check the camera connection and retry.')
        def call(operation,**values):
            response=c.twin.camera_alignment(operation,**values)
            if response.get('policyVersion')!=1:raise ValueError('Update the app to use this camera alignment service.')
            return response
        try:
            first=call('begin',roomFrame=frame,camera=camera_id,frame=fresh())
            token=first['token']
            verified=call('verify',token=token,frame=fresh(time.monotonic()+.85))
            with c.lock:
                if not current():return
                c.validate_frame()
                # Only the shared service can commit the verified result. It
                # checks the room and metadata revision again before writing.
                saved=call('commit',token=token)['metadata']
                token=None
                c.propose_camera(saved)
                c.phase='setup'
                c.message='Camera alignment updated. Review the floor grid before confirming.'
        except Exception as error:
            with c.lock:
                if current():
                    c.phase='setup'
                    c.camera_message='Camera alignment was not updated. '+str(error)
                    c.message=c.camera_message
        finally:
            if token:
                try:call('cancel',token=token)
                except Exception:pass
            self.alignment_lock.release()

    def close(self):
        self.quit.set()
        try:
            if self.controller.twin.handle:
                self.controller.stop('Application shutting down')
                self.controller.twin.call('close')
        except Exception:
            log.exception('Shutdown stop failed; no further lease renewals will be sent')
        if self.camera:
            self.camera.close()
        self.agent.channel.close()
        for thread in self.threads:
            thread.join(timeout=2)


@asynccontextmanager
async def lifespan(app):
    app.state.runtime = Runtime()
    try:
        yield
    finally:
        app.state.runtime.close()


app = FastAPI(lifespan=lifespan,docs_url=None,redoc_url=None,openapi_url=None)


@app.middleware('http')
async def check_origin(request, call_next):
    if request.url.path.startswith('/api/'):
        # LAN-only demo, intentionally no login token. Browser requests must
        # originate from this dashboard; command-line clients may omit Origin.
        origin = request.headers.get('origin')
        if request.headers.get('sec-fetch-site') == 'cross-site' or (
            origin and origin != f"{request.url.scheme}://{request.headers.get('host', '')}"
        ):
            return Response('Cross-site requests are not allowed.', status_code=403)
        if request.method == 'POST' and request.headers.get('content-type','').split(';')[0] != 'application/json':
            return Response('JSON required',status_code=415)
        length = request.headers.get('content-length','0')
        if not length.isdigit() or int(length) > 16384:
            return Response('Request too large',status_code=413)
    response = await call_next(request)
    response.headers['Cache-Control'] = 'no-store'
    response.headers['X-Content-Type-Options'] = 'nosniff'
    response.headers['Referrer-Policy'] = 'no-referrer'
    return response


@app.get('/')
def index():
    return FileResponse(ROOT/'index.html')


@app.get('/health')
def health():
    return {'app':'go2-camera-search','status':'listening','motion':'requires explicit operator start'}


@app.get('/bark.wav')
def bark():
    return FileResponse(ROOT/'audio/bark.wav')


@app.get('/api/inventory')
def inventory(request: Request):
    runtime = request.app.state.runtime
    try:
        result = runtime.agent.inventory()
        result['rooms'] = [dict(id=r['site']['id']+'/'+r['site']['modelRevision'],name=r['site']['name']) for r in runtime.controller.twin.rooms() if not r.get('removed')]
        result['items'] = ITEMS
        return result
    except Exception as error:
        raise HTTPException(503,str(error)) from error


@app.get('/api/state')
def state(request: Request):
    r = request.app.state.runtime
    state = r.controller.state()
    with r.image_lock:
        state.update(width=r.width,height=r.height,imageAge=time.monotonic()-r.image_at if r.image_at else None,visionError=r.vision_error,detections=r.detections)
    return state


@app.get('/api/frame')
def frame(request: Request):
    r = request.app.state.runtime
    with r.image_lock:
        if not r.jpeg or time.monotonic()-r.image_at > 1.5:
            raise HTTPException(503,'No fresh camera image')
        return Response(r.jpeg,media_type='image/jpeg')


@app.post('/api/{action}')
def action(action: str, body: dict, request: Request):
    r = request.app.state.runtime
    c = r.controller
    owner = request.headers.get('x-operator','')
    if not 16 <= len(owner) <= 100:
        raise HTTPException(400,'Missing browser operator identity')
    try:
        if c.phase == 'camera-calibrating' and action not in ('heartbeat','stop','connect'):
            raise ValueError('Wait for camera alignment to finish, or choose Stop to cancel.')
        if action == 'heartbeat':
            c.pulse(owner)
        elif action == 'stop':
            with r.image_lock:
                r.search_generation += 1
                r.tracker = None
            c.stop('Stopped by operator')
        elif action == 'connect':
            from hardware import Camera
            inventory = r.agent.inventory()
            if not any(v['id'] == body['camera'] and v['online'] for v in inventory['cameras']):
                raise ValueError('The camera is not online yet. Check its power and choose Retry setup.')
            from scene import read_room
            room = c.twin.room(body['frame'])
            scene = read_room(room)
            if scene['frame'].lower() != body['frame'].lower() or room.get('removed'):
                raise ValueError('This room changed. Choose it again.')
            serial = body.get('serial','')
            if not any(v['serial'] == serial and v['connected'] for v in inventory['robots']):
                serial = ''  # Camera setup remains usable while the dog is offline.
            with r.camera_lock:
                c.prepare(owner,body['camera'],scene,serial)
                if r.camera:
                    r.camera.close()
                r.camera = Camera(r.agent,body['camera'])
                with r.image_lock:
                    r.jpeg,r.image_at,r.width,r.height = b'',0.,0,0
                    r.raw,r.tracker = None,None
                    r.search_generation += 1
                try:
                    c.propose_camera(r.agent.camera_metadata(body['camera']))
                except Exception as error:
                    c.camera_message = str(error)
            return {'ok':True,'robotConnected':bool(serial)}
        elif action == 'recalibrate-camera':
            r.recalibrate_camera(owner)
        elif action == 'confirm-camera':
            with r.image_lock:
                if time.monotonic()-r.image_at > 1.5:
                    raise ValueError('Wait for a live camera image before confirming.')
                width,height = r.width,r.height
            c.confirm_camera(owner,width,height)
        elif action in ('locate','look-around'):
            c.assert_owner(owner)
            if not c.twin.handle:
                inventory = r.agent.inventory()
                choices = [v for v in inventory['robots'] if v['connected'] and (not body.get('serial') or v['serial'] == body['serial'])]
                if len(choices) != 1:
                    raise ValueError('Turn Go2 on and select it, then try again.')
                c.attach(owner,choices[0]['serial'])
            if action == 'look-around':
                try:
                    c.look_around(owner)
                except Exception:
                    c.stop('Could not start looking around. Movement cancelled.')
                    raise
            else:
                c.locate(owner)
        elif action == 'camera-robot':
            with r.image_lock:
                if time.monotonic()-r.image_at > 1.5:
                    raise ValueError('Wait for a live camera image.')
                width,height = r.width,r.height
            c.preview_camera_robot(owner,body['front'],body['rear'],width,height)
        elif action == 'confirm-robot':
            if c.camera_robot_preview is not None:
                with r.image_lock:
                    if time.monotonic()-r.image_at > 1.5:
                        raise ValueError('Wait for a live camera image before confirming Go2.')
                    if (r.width,r.height) != c.camera_robot_preview[-2:]:
                        raise ValueError('Camera format changed. Mark Go2 again.')
            c.confirm_robot(owner,body.get('index'))
        elif action == 'search':
            r.find(owner,str(body['query']))
        elif action == 'preview':
            c.preview(owner)
        elif action == 'start':
            try:
                c.start(owner)
            except Exception:
                # A timeout may occur after the twin accepted a destination.
                c.stop('Start failed. Movement cancelled; preview again.')
                raise
        else:
            raise HTTPException(404,'Unknown action')
        return {'ok':True}
    except (ValueError,KeyError,TypeError) as error:
        raise HTTPException(422,str(error)) from error
    except HTTPException:
        raise
    except Exception as error:
        raise HTTPException(503,str(error)) from error


if __name__ == '__main__':
    import uvicorn
    logging.basicConfig(level=logging.INFO)
    uvicorn.run(app,host='0.0.0.0',port=int(os.environ.get('PORT','18993')),access_log=False)
