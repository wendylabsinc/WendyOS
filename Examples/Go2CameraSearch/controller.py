"""One supervised operator, one frozen target, one bounded movement at a time."""
import math
import threading
import time
from collections import deque
from core import RobotSessionExpired, Calibration, RunGuard, approach, finite, requested_item


class Controller:
    def __init__(self, twin, clock=time.monotonic):
        self.twin, self.clock = twin, clock
        self.lock = threading.RLock()
        self.owner = ''
        self.heartbeat = -math.inf
        self.frame = self.camera_id = ''
        self.calibration = None
        self.snapshot = {}
        self.phase, self.message = 'setup', 'Connect a paired camera and robot.'
        self.item = ''
        self.history = deque(maxlen=3)
        self.observed = None
        self.detected_at = -math.inf
        self.target = self.destination = None
        self.guard = None
        self.preview_at = -math.inf
        self.arrivals = 0
        self.last_room_check = -math.inf
        self.scene = None
        self.camera_proposal = None
        self.camera_overlay = []
        self.candidates = []
        self.match_sequence = 0
        self.camera_message = 'Choose your camera and room.'
        self.rotation_search = False
        self.rotation_started = 0
        self.camera_robot_preview = None
        self.calibration_epoch = 0

    def assert_owner(self, owner):
        if not owner or owner != self.owner:
            raise ValueError('Connect from this browser to take control.')

    def pulse(self, owner):
        with self.lock:
            self.assert_owner(owner)
            self.heartbeat = self.clock()

    def discard_expired_session(self):
        self.twin.handle = ''
        self.guard = None
        self.rotation_search = False
        self.camera_robot_preview = None
        self.candidates = []
        self.target = self.destination = None
        self.observed = None
        self.detected_at = -math.inf
        self.history.clear()
        self.preview_at = -math.inf
        self.match_sequence = 0
        self.snapshot = {}
        self.heartbeat = -math.inf
        self.owner = ''
        self.phase, self.message = 'setup', 'Go2 session ended. Choose Retry setup to reconnect.'

    def release_session(self):
        if not self.twin.handle:
            return
        try:
            self.stop('Changing session')
            self.twin.call('close')
            self.twin.handle = ''
        except RobotSessionExpired:
            # The server positively rejected this handle. Never apply this
            # recovery to an ambiguous timeout or a failed stop command.
            self.discard_expired_session()

    def connect(self, owner, serial, camera, frame):
        with self.lock:
            if self.owner and self.owner != owner and self.clock() - self.heartbeat <= .8:
                raise ValueError('Another browser is supervising this session.')
            self.release_session()
            self.owner, self.frame, self.camera_id = owner, frame, camera
            self.heartbeat = self.clock()
            self.calibration = None
            self.target = self.destination = None
            self.item = ''
            self.history.clear()
            self.snapshot = {}
            self.twin.call('open', Serial=serial)
            try:
                # Use the same supervised settings as the macOS controller.
                for operation in ('timing', 'underbody', 'floor'):
                    self.twin.call(operation, Enabled=True)
            except Exception:
                self.twin.call('close')
                self.twin.handle = ''
                raise
            self.phase, self.message = 'setup', 'Confirm the robot position and calibrate the floor.'

    def prepare(self, owner, camera, scene, serial=''):
        with self.lock:
            if self.owner and self.owner != owner and self.clock()-self.heartbeat <= .8:
                raise ValueError('Another browser is supervising this session.')
            if serial:
                self.connect(owner,serial,camera,scene['frame'])
            else:
                self.release_session()
                self.owner,self.frame,self.camera_id = owner,scene['frame'],camera
                self.heartbeat = self.clock()
                self.calibration = None
                self.snapshot = {}
                self.item = ''
                self.guard = None
                self.target = self.destination = None
            self.calibration_epoch += 1
            self.scene = scene
            self.camera_proposal = None
            self.camera_overlay = []
            self.candidates = []
            self.phase,self.message = 'setup','Check the floor overlay and locate Go2 in the room.'

    def propose_camera(self, record):
        from scene import CameraPose
        proposal = CameraPose(record,self.scene,self.camera_id)
        overlay = proposal.overlay()
        if not overlay:
            raise ValueError('The saved camera placement does not show this floor. Align the camera in Wendy and retry.')
        with self.lock:
            self.camera_proposal = proposal
            self.camera_overlay = overlay
            self.camera_message = ('Estimated camera placement. Check that the grid lies on the floor.' if proposal.data['approximate'] else 'Camera placement loaded. Check that the grid lies on the floor.')

    def confirm_camera(self, owner, width, height):
        with self.lock:
            self.assert_owner(owner)
            proposal = self.camera_proposal
            if proposal is None:
                raise ValueError(self.camera_message)
            if (proposal.width,proposal.height)!=(width,height):
                raise ValueError('The camera format changed. Align it in Wendy and retry setup.')
            if self.guard:
                self.stop('Camera placement changed')
            self.calibration = proposal
            self.camera_message = ('Estimated camera placement. Floor overlay reviewed; robot alignment requires a measured lens.'
                                   if proposal.data.get('approximate') else 'Camera placement confirmed.')
            self.phase,self.message = 'idle','Camera ready. Locate and confirm Go2, then find an object.'

    def attach(self, owner, serial):
        with self.lock:
            self.assert_owner(owner)
            calibration = self.calibration
            self.connect(owner,serial,self.camera_id,self.frame)
            self.calibration = calibration

    def look_around(self, owner):
        with self.lock:
            self.assert_owner(owner)
            if not self.twin.handle or not self.scene:
                raise ValueError('Connect Go2 and choose its room first.')
            if self.clock()-self.heartbeat > .8:
                raise ValueError('Keep this dashboard open while Go2 looks around.')
            self.stop('Starting a position search')
            self.validate_frame()
            self.candidates = []
            self.snapshot = self.twin.call('poll')['Snapshot']
            if self.snapshot.get('TimingUnavailable') and not self.snapshot.get('ArrivalTiming'):
                self.phase = 'blocked'
                self.message = self.snapshot.get('Message') or 'LiDAR capture timing is unavailable. Go2 cannot turn.'
                return
            self.twin.call('search',Fusion=dict(frame=self.frame,architecture=self.scene['architecture'],furniture=self.scene['furniture']))
            self.match_sequence = self.twin.sequence
            self.rotation_search = True
            self.rotation_started = self.clock()
            self.phase,self.message = 'looking-around','Go2 will turn in small steps and check its surroundings.'

    def locate(self, owner):
        with self.lock:
            self.assert_owner(owner)
            if not self.twin.handle:
                raise ValueError('Go2 is offline. Turn it on, then choose Retry setup.')
            self.stop('Matching Go2 to the room…')
            self.candidates = []
            self.validate_frame()
            result = self.twin.call('fuse',Fusion=dict(frame=self.frame,architecture=self.scene['architecture'],furniture=self.scene['furniture']))
            report = result.get('Fusion') or {}
            self.match_sequence = self.twin.sequence
            self.candidates = report.get('candidates') or []
            self.phase = 'locating' if not self.candidates else 'review'
            self.message = report.get('message') or ('Choose Go2’s suggested position on the map.' if self.candidates else 'Waiting for a clear scan. Keep Go2 still and try Locate again.')
            return self.candidates

    def preview_camera_robot(self, owner, front, rear, width, height):
        """Operator-marked floor contacts are a manual alignment, not visual inliers."""
        with self.lock:
            self.assert_owner(owner)
            if not self.calibration or not self.twin.handle or not self.scene:
                raise ValueError('Confirm the camera placement and connect Go2 first.')
            if self.calibration.data.get('approximate'):
                raise ValueError('This camera uses an estimated lens. Floor-grid confirmation does not verify position or heading. Calibrate its lens in Wendy before marking Go2.')
            self.stop('Checking the marked Go2 position')
            self.validate_frame()
            if len(front) != 2 or len(rear) != 2:
                raise ValueError('Mark the front and rear floor points.')
            a = self.calibration.project(*front, width, height)
            b = self.calibration.project(*rear, width, height)
            if not .25 <= math.dist(a, b) <= .85:
                raise ValueError('The marked body length must be 25–85 cm. Mark the floor between the front feet, then between the rear feet.')
            candidate = dict(X=(a[0]+b[0])/2, Z=(a[1]+b[1])/2,
                             Yaw=math.atan2(-(a[1]-b[1]), a[0]-b[0]), FloorY=self.scene['floor'])
            self.camera_robot_preview = (self.clock(), self.calibration, self.frame, candidate, front, rear, width, height)
            self.candidates = [candidate]
            self.phase, self.message = 'review', 'Check the blue position and heading against the live camera. Keep Go2 still, then confirm within 15 seconds.'

    def confirm_robot(self, owner, index):
        with self.lock:
            self.assert_owner(owner)
            if not isinstance(index,int) or isinstance(index,bool) or not 0<=index<len(self.candidates):
                raise ValueError('Select a suggested robot position on the map.')
            self.validate_frame()
            if self.camera_robot_preview is not None:
                at, calibration, frame, candidate, front, rear, width, height = self.camera_robot_preview
                if self.clock()-at > 15 or calibration is not self.calibration or frame != self.frame:
                    self.candidates = []
                    self.camera_robot_preview = None
                    raise ValueError('Camera position preview expired. Mark Go2 again.')
                if calibration.data.get('approximate'):
                    raise ValueError('Robot marking requires a measured camera lens.')
                # Revalidate calibration expiry and image geometry before manual alignment.
                calibration.project(*front, width, height)
                calibration.project(*rear, width, height)
                self.align(owner, candidate['X'], candidate['Z'], math.degrees(candidate['Yaw']), candidate['FloorY'])
                self.candidates = []
                return
            self.twin.call('confirm',MatchSequence=self.match_sequence,Frame=self.frame,Index=index)
            self.candidates = []
            self.snapshot = self.twin.call('poll')['Snapshot']
            self.phase,self.message = 'idle','Go2 position confirmed. Find an object when ready.'

    def validate_frame(self):
        rooms = self.twin.rooms()
        if not any(r['site']['id']+'/'+r['site']['modelRevision'] == self.frame and not r.get('removed') for r in rooms):
            raise ValueError('Room revision changed. Reconnect and recalibrate.')

    def align(self, owner, x, z, yaw, floor):
        with self.lock:
            self.assert_owner(owner)
            finite([x,z,yaw,floor])
            if max(abs(x), abs(z), abs(floor)) > 1000 or abs(yaw) > 360:
                raise ValueError('Robot alignment is outside bounds.')
            self.stop('Confirming position')
            self.validate_frame()
            self.twin.call('align', Frame=self.frame, X=x, Z=z, Floor=floor, Yaw=math.radians(yaw))
            self.phase, self.message = 'idle', 'Robot position confirmed. Calibrate, then find an object.'

    def calibrate(self, owner, data):
        with self.lock:
            self.assert_owner(owner)
            calibration = Calibration(data)
            if calibration.frame != self.frame or calibration.camera != self.camera_id:
                raise ValueError('Calibration belongs to a different camera or room revision.')
            self.stop('Calibration changed. Search again.')
            self.calibration = calibration
            self.history.clear()
            return calibration.data

    def search(self, owner, query):
        item = requested_item(query)  # Invalid requests do not cancel an active run.
        with self.lock:
            self.assert_owner(owner)
            if not self.calibration:
                raise ValueError('Check the floor overlay and confirm the camera placement first.')
            self.stop('Searching')
            self.item = item
            self.history.clear()
            self.observed = None
            self.detected_at = -math.inf
            self.phase, self.message = 'searching', 'Looking for ' + item

    def observe(self, detections, at, width, height):
        with self.lock:
            if not self.item or not self.calibration:
                return
            hits = [d for d in detections if d['name'] == self.item]
            self.observed = None
            if len(hits) != 1:
                self.history.clear()
                if not self.guard:
                    self.target = self.destination = None
                    self.phase = 'searching'
                    self.message = 'No target visible.' if not hits else 'Multiple matching objects. Leave only one in view.'
                return
            try:
                x, y, w, h = hits[0]['box']
                point = self.calibration.project(x+w/2, y+h, width, height)
            except ValueError as error:
                self.history.clear()
                if not self.guard:
                    self.target = self.destination = None
                    self.phase, self.message = 'searching', str(error)
                return
            self.observed, self.detected_at = point, at
            if self.guard:
                return
            if self.phase == 'preview':
                if math.dist(point, self.target) > .25:
                    self.target = self.destination = None
                    self.phase, self.message = 'searching', 'Target changed. Find a fresh route.'
                return
            if self.phase != 'searching':
                return
            if self.history and (at <= self.history[-1][0] or math.dist(point,self.history[-1][1]) > .2):
                self.history.clear()
            self.history.append((at,point))
            if len(self.history) == 3 and self.clock() - self.history[0][0] <= 1.5:
                self.target = tuple(sum(p[i] for _,p in self.history)/3 for i in (0,1))
                self.phase, self.message = 'found', 'Object found. Preview its approach route.'

    def ready(self):
        now = self.clock()
        if now-self.heartbeat > .8:
            raise ValueError('Browser supervision is stale.')
        if not self.target or not self.observed or now-self.detected_at > 1.5 or math.dist(self.target,self.observed) > .25:
            raise ValueError('Wait for a fresh, unambiguous target detection.')
        s = self.twin.call('poll')['Snapshot']
        self.snapshot = s
        if not s.get('Pose') or not s.get('Ready') or s.get('Disconnected') or s.get('ControlWarning'):
            raise ValueError(s.get('ControlWarning') or s.get('Message') or 'Robot is not ready.')
        return s

    def preview(self, owner):
        with self.lock:
            self.assert_owner(owner)
            if self.phase not in ('found','preview'):
                raise ValueError('Find an object first.')
            self.validate_frame()
            s = self.ready()
            self.destination = approach((s['X'],s['Z']),self.target)
            self.twin.call('destination', Frame=self.frame, X=self.destination[0], Z=self.destination[1], Enabled=False)
            self.snapshot = self.twin.call('poll')['Snapshot']
            self.phase, self.message = 'preview', 'Review the destination and route, then start the walk.'
            self.preview_at = self.clock()

    def start(self, owner):
        with self.lock:
            self.assert_owner(owner)
            if self.phase != 'preview' or not self.destination or self.clock()-self.preview_at > 10:
                raise ValueError('Preview a fresh route before starting.')
            self.validate_frame()
            s = self.ready()
            origin = (s['X'],s['Z'])
            if math.dist(origin,self.destination) > 5:
                raise ValueError('Destination exceeds the movement bound.')
            # Keep the reviewed endpoint. The twin replans against its current scans.
            self.twin.call('destination', Frame=self.frame, X=self.destination[0], Z=self.destination[1], Enabled=True)
            self.guard = RunGuard(self.clock(),origin,self.target,origin)
            self.phase, self.message = 'moving', 'Walking to the approach point.'

    def stop(self, message='Stopped'):
        with self.lock:
            self.calibration_epoch += 1
            self.guard = None
            self.camera_robot_preview = None
            self.candidates = []
            self.rotation_search = False
            self.target = self.destination = None
            self.phase, self.message = 'stopped', message
            if self.twin.handle:
                # Do not swallow a failed Stop. Stop renewing the lease as well.
                try:
                    self.twin.call('stop')
                    state = self.twin.call('poll')['Snapshot']
                    self.snapshot = state
                    if state.get('ControlWarning'):
                        raise RuntimeError(state['ControlWarning'])
                except RobotSessionExpired:
                    self.discard_expired_session()
                    raise
                except Exception as error:
                    self.heartbeat = -math.inf
                    self.owner = ''
                    self.message = 'Stop not confirmed: ' + str(error)
                    raise

    def tick(self):
        with self.lock:
            if not self.twin.handle:
                return
            now = self.clock()
            try:
                if now-self.heartbeat > .8:
                    if self.guard or self.rotation_search:
                        self.stop('Browser supervision lost. Start again explicitly.')
                    return  # Never renew the twin lease without browser supervision.
                if self.guard and now-self.last_room_check > 1:
                    self.validate_frame()
                    self.last_room_check = now
                self.snapshot = self.twin.call('poll')['Snapshot']
                if self.rotation_search:
                    if self.snapshot.get('TimingUnavailable') and not self.snapshot.get('ArrivalTiming'):
                        reason = self.snapshot.get('Message') or 'LiDAR capture timing is unavailable. Go2 cannot turn.'
                        self.stop(reason)
                        self.phase = 'blocked'
                        return
                    if now-self.rotation_started > 120:
                        self.stop('Position search timed out. Keep Go2 in view and try again.')
                        return
                    report = self.twin.call('search-poll')
                    self.message = report.get('SearchMessage') or 'Looking around…'
                    if report.get('SearchState') == 'complete':
                        self.rotation_search = False
                        self.candidates = (report.get('Fusion') or {}).get('candidates') or []
                        self.phase = 'review' if self.candidates else 'locating'
                    elif report.get('SearchState') == 'failed' or self.snapshot.get('Disconnected'):
                        self.stop(self.message)
                if self.guard:
                    reason = self.guard.check(now,self.heartbeat,self.detected_at,self.observed,self.snapshot)
                    if reason:
                        self.stop(reason)
                    elif not self.snapshot.get('Moving'):
                        if self.snapshot.get('Message') == 'Arrived' and math.dist((self.snapshot['X'],self.snapshot['Z']),self.destination) < .4:
                            self.guard = None
                            self.phase, self.message = 'arrived', 'Arrived beside the object.'
                            self.arrivals += 1
                        else:
                            self.stop(self.snapshot.get('Message') or 'Navigation stopped before arrival.')
            except RobotSessionExpired:
                self.discard_expired_session()
            except Exception as error:
                self.guard = None
                self.rotation_search = False
                self.phase, self.message = 'fault', str(error)
                # Fail closed: a transport error also withdraws browser ownership.
                self.heartbeat = -math.inf
                self.owner = ''
                try:
                    self.twin.call('stop')
                except Exception:
                    self.message += ' Stop unconfirmed; the controller lease must expire.'

    def state(self):
        with self.lock:
            camera_pose = None
            proposal = self.camera_proposal
            if (proposal is not None and proposal.frame == self.frame and proposal.camera == self.camera_id
                    and proposal.expires > time.time()):
                camera_pose = dict(position=proposal.data['position'], forward=proposal.data['forward'],
                                   approximate=proposal.data['approximate'], confirmed=self.calibration is proposal)
            return dict(phase=self.phase,message=self.message,item=self.item,target=self.target,destination=self.destination,
                        robot=self.snapshot,arrivals=self.arrivals,calibration=self.calibration.data if self.calibration else None,
                        frame=self.frame,camera=self.camera_id,supervised=self.clock()-self.heartbeat <= .8,
                        room={k:self.scene[k] for k in ('name','floors','walls')} if self.scene else None,
                        cameraOverlay=self.camera_overlay,cameraMessage=self.camera_message,cameraPose=camera_pose,
                        cameraProposed=self.camera_proposal is not None,cameraConfirmed=self.calibration is not None,
                        candidates=self.candidates,robotConnected=bool(self.twin.handle),lookingAround=self.rotation_search)
