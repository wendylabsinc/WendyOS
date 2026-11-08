import copy
import math
import sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import pytest
from core import Calibration, RunGuard, approach, requested_item
from controller import Controller

DATA = dict(camera='sensorlink:paired:camera',frame='room/revision',width=640,height=480,
            pixels=[[0,0],[639,0],[639,479],[0,479]],world=[[0,0],[4,0],[4,3],[0,3]])

class Twin:
    handle = ''
    def __init__(self):
        self.calls = []
        self.fail = ''
        self.revision = 'revision'
        self.snapshot = dict(Pose=True,Ready=True,Aligned=True,Moving=False,X=0,Z=1.5,Message='Ready')
    def rooms(self):
        return [{'site':dict(id='room',modelRevision=self.revision)}]
    def call(self,op,**kwargs):
        self.calls.append((op,kwargs))
        if op == self.fail:
            raise RuntimeError('lost transport')
        if op == 'open': self.handle = 'handle'
        if op == 'stop': self.snapshot['Moving'] = False
        if op == 'destination': self.snapshot['Moving'] = kwargs['Enabled']
        return {'Snapshot':copy.deepcopy(self.snapshot),'Handle':self.handle}

@pytest.fixture
def controller():
    now = [100.]
    c = Controller(Twin(),lambda:now[0])
    c.connect('operator','serial',DATA['camera'],DATA['frame'])
    c.calibrate('operator',copy.deepcopy(DATA))
    c.search('operator','find the bottle')
    c._test_time = now
    for i in range(3):
        now[0] += .1
        c.observe([dict(name='bottle',box=[300,100,39,139.5])],now[0],640,480)
    return c


def test_floor_projection_and_resolution():
    c=Calibration(copy.deepcopy(DATA))
    assert c.project(319.5,239.5,640,480)==pytest.approx((2,1.5))
    with pytest.raises(ValueError): c.project(650,100,640,480)
    with pytest.raises(ValueError): c.project(200,100,1280,960)

@pytest.mark.parametrize('pixels',[[[0,0],[20,0],[40,0],[60,0]],[[0,0],[639,479],[639,0],[0,479]],[[math.nan,0],[639,0],[639,479],[0,479]]])
def test_bad_calibration_rejected(pixels):
    data=copy.deepcopy(DATA);data['pixels']=pixels
    with pytest.raises(ValueError): Calibration(data)


def test_request_and_distance_bounds():
    assert requested_item('find the waternig can')=='watering can'
    assert requested_item('the striped watering can')=='the striped watering can'
    with pytest.raises(ValueError): requested_item('')
    with pytest.raises(ValueError): requested_item('a'*121)
    assert approach((0,0),(2,0))==pytest.approx((1.2,0))
    with pytest.raises(ValueError): approach((0,0),(6,0))
    with pytest.raises(ValueError): approach((0,0),(.5,0))


def test_connection_matches_all_macos_supervised_settings(controller):
    settings=[(op,data) for op,data in controller.twin.calls if op in ('timing','underbody','floor')]
    assert settings==[(op,{'Enabled':True}) for op in ('timing','underbody','floor')]
    assert not any(data.get('Enabled') for op,data in controller.twin.calls if op=='destination')


def test_preview_never_moves_and_start_requires_preview(controller):
    c=controller
    assert c.phase=='found'
    with pytest.raises(ValueError): c.start('operator')
    c.preview('operator')
    assert c.phase=='preview'
    assert not c.twin.snapshot['Moving']
    c.start('operator')
    assert c.twin.snapshot['Moving'] and c.guard

@pytest.mark.parametrize('fault',['camera','browser','target','pose','control','revision','timeout','distance'])
def test_faults_stop_and_never_resume(controller,fault):
    c=controller;c.preview('operator');c.start('operator')
    if fault=='camera': c.detected_at-=2
    if fault=='browser': c.heartbeat-=2
    if fault=='target': c.observed=(4.,3.)
    if fault=='pose': c.twin.snapshot['Pose']=False
    if fault=='control': c.twin.snapshot['ControlWarning']='Stop unconfirmed'
    if fault=='revision': c.twin.revision='changed'
    if fault=='timeout': c.guard.started-=31
    if fault=='distance': c.twin.snapshot['X']=6
    c.tick()
    assert not c.guard
    assert any(op=='stop' for op,_ in c.twin.calls[-3:])
    assert not c.twin.snapshot['Moving']
    c.twin.snapshot.update(Pose=True,ControlWarning='',X=0)
    before=len([1 for op,d in c.twin.calls if op=='destination' and d['Enabled']])
    c.tick()
    assert len([1 for op,d in c.twin.calls if op=='destination' and d['Enabled']])==before


def test_idle_browser_does_not_renew_controller_lease(controller):
    c=controller;c.heartbeat-=2
    before=len(c.twin.calls)
    c.tick()
    assert len(c.twin.calls)==before


def test_ambiguous_target_invalidates_preview(controller):
    c=controller;c.preview('operator')
    hit=dict(name='bottle',box=[300,100,39,139.5])
    c.observe([hit,hit],c.clock(),640,480)
    assert c.phase=='searching' and c.destination is None
    with pytest.raises(ValueError): c.start('operator')


def test_room_revision_checked_before_start(controller):
    c=controller;c.preview('operator');c.twin.revision='new'
    with pytest.raises(ValueError): c.start('operator')
    assert not c.twin.snapshot['Moving']


def test_no_false_arrival_on_stopped_navigation(controller):
    c=controller;c.preview('operator');c.start('operator')
    c.twin.snapshot.update(Moving=False,Message='Blocked')
    c.tick()
    assert c.arrivals==0 and c.phase=='stopped'


def test_arrival_requires_controller_and_position(controller):
    c=controller;c.preview('operator');c.start('operator')
    c.twin.snapshot.update(Moving=False,Message='Arrived',X=c.destination[0],Z=c.destination[1])
    c.tick()
    assert c.arrivals==1 and c.phase=='arrived'
    c.tick();assert c.arrivals==1


def test_operator_cannot_replace_live_owner(controller):
    c=controller
    with pytest.raises(ValueError): c.connect('other','serial',DATA['camera'],DATA['frame'])
    with pytest.raises(ValueError): c.preview('other')


def test_stop_failure_withdraws_lease(controller):
    c=controller;c.preview('operator');c.start('operator');c.twin.fail='stop'
    with pytest.raises(RuntimeError): c.stop()
    assert not c.owner and not c.guard
    before=len(c.twin.calls);c.tick();assert len(c.twin.calls)==before


def test_invalid_request_does_not_cancel_run(controller):
    c=controller;c.preview('operator');c.start('operator')
    with pytest.raises(ValueError): c.search('operator','')
    assert c.guard


def test_preview_expires(controller):
    c=controller;c.preview('operator');c._test_time[0]+=11
    with pytest.raises(ValueError): c.start('operator')


def test_visual_localization_preview_and_confirmation(controller):
    c=controller
    c.scene={'architecture':[0.]*18,'furniture':[]}
    original=c.twin.call
    c.twin.sequence=0
    def call(op,**kwargs):
        c.twin.sequence+=1
        if op=='fuse':return {'Fusion':{'candidates':[dict(X=1,Z=2,Yaw=0,FloorY=0)],'message':'Check this position'}}
        return original(op,**kwargs)
    c.twin.call=call
    c.locate('operator')
    assert c.phase=='review' and len(c.candidates)==1
    match=c.match_sequence
    c.confirm_robot('operator',0)
    assert not c.candidates
    command=[v for op,v in c.twin.calls if op=='confirm'][-1]
    assert command['MatchSequence']==match
    assert not any(op=='destination' and v.get('Enabled') for op,v in c.twin.calls)


def test_look_around_requires_click_and_stops_without_supervision(controller):
    c=controller;c.scene={'architecture':[0.]*18,'furniture':[]}
    original=c.twin.call;c.twin.sequence=0
    def call(op,**kwargs):
        c.twin.sequence+=1
        return original(op,**kwargs)
    c.twin.call=call
    assert not c.rotation_search
    c.look_around('operator')
    assert c.rotation_search
    assert any(op=='search' for op,_ in c.twin.calls)
    c.heartbeat-=2;c.tick()
    assert not c.rotation_search
    assert c.phase=='stopped'


def test_rotation_search_has_its_own_time_bound(controller):
    c=controller;c.rotation_search=True;c.rotation_started=c.clock()-121
    c.tick()
    assert not c.rotation_search
    assert 'timed out' in c.message


def test_coarse_lidar_timing_never_starts_search(controller):
    c=controller;c.scene={'architecture':[], 'furniture':[]}
    c.twin.snapshot.update(TimingUnavailable=True,ArrivalTiming=False,Message='Capture timestamp too coarse')
    c.look_around('operator')
    assert c.phase=='blocked' and not c.rotation_search
    assert c.message=='Capture timestamp too coarse'
    assert not any(op=='search' for op,_ in c.twin.calls)


def test_timing_failure_during_search_stops_and_explains(controller):
    c=controller;c.rotation_search=True;c.rotation_started=c.clock()
    c.twin.snapshot.update(TimingUnavailable=True,ArrivalTiming=False,Message='Capture timestamp too coarse')
    c.tick()
    assert c.phase=='blocked' and not c.rotation_search
    assert c.message=='Capture timestamp too coarse'
    assert any(op=='stop' for op,_ in c.twin.calls)


def test_macos_timing_allows_supervised_search(controller):
    c=controller;c.scene={'architecture':[], 'furniture':[]};c.twin.sequence=1
    c.twin.snapshot.update(TimingUnavailable=True,ArrivalTiming=True,UsingArrivalTiming=True)
    c.look_around('operator')
    assert c.rotation_search
    assert any(op=='search' for op,_ in c.twin.calls)
    c.heartbeat-=2;c.tick()
    assert not c.rotation_search


def test_macos_timing_does_not_block_route_preview(controller):
    c=controller
    c.twin.snapshot.update(ArrivalTiming=True,UsingArrivalTiming=True,UnderBodyAllowance=True,AllowUnobservedFloor=True)
    c.preview('operator')
    assert c.phase=='preview' and not c.twin.snapshot['Moving']


def test_camera_robot_preview_requires_review_and_preserves_heading(controller):
    c = controller
    c.scene = dict(floor=0)
    c.preview_camera_robot('operator', [320,240], [240,240], 640,480)
    assert c.phase == 'review'
    assert c.candidates[0]['Yaw'] == pytest.approx(0)
    assert not any(op == 'align' for op,_ in c.twin.calls)
    c.confirm_robot('operator',0)
    calls = [args for op,args in c.twin.calls if op == 'align']
    assert len(calls) == 1
    assert calls[0]['X'] == pytest.approx(280*4/639)
    assert not c.candidates


def test_camera_robot_expiry_and_stop_invalidate_preview(controller):
    c = controller
    c.scene = dict(floor=0)
    c.preview_camera_robot('operator', [320,240], [240,240], 640,480)
    c._test_time[0] += 16
    with pytest.raises(ValueError, match='expired'): c.confirm_robot('operator',0)
    assert not any(op == 'align' for op,_ in c.twin.calls)
    c.preview_camera_robot('operator', [320,240], [240,240], 640,480)
    c.stop()
    assert c.camera_robot_preview is None and not c.candidates


@pytest.mark.parametrize('front,rear', [([320,240],[320,240]),([600,240],[100,240]),([float('nan'),240],[240,240]),([-1,240],[240,240])])
def test_camera_robot_rejects_invalid_marks(controller,front,rear):
    controller.scene = dict(floor=0)
    with pytest.raises(ValueError): controller.preview_camera_robot('operator',front,rear,640,480)
    assert not controller.candidates
    assert not any(op == 'align' for op,_ in controller.twin.calls)


@pytest.mark.parametrize('operation', ['prepare', 'connect', 'tick'])
def test_expired_session_can_reconnect_without_replaying_motion(controller, operation):
    from core import RobotSessionExpired
    c = controller
    original = c.twin.call
    c.candidates = [{'X': 2}]
    c.rotation_search = True
    c.camera_robot_preview = ('old',)
    calls = []
    def expired(op, **kwargs):
        calls.append(op)
        if op in ('poll', 'stop'):
            raise RobotSessionExpired('expired')
        return original(op, **kwargs)
    c.twin.call = expired
    if operation == 'prepare':
        c.prepare('operator', DATA['camera'], {'frame':DATA['frame']})
        assert c.owner == 'operator'
    elif operation == 'connect':
        c.connect('operator','serial',DATA['camera'],DATA['frame'])
        assert c.twin.handle == 'handle'
    else:
        c.tick()
        assert not c.owner and not c.twin.handle
        assert calls == ['poll']
    assert not c.candidates and c.camera_robot_preview is None
    assert not c.rotation_search and c.guard is None and not c.snapshot
    assert not any(op in ('search', 'destination', 'align') for op in calls)
    c.twin.call = original
    c.prepare('operator', DATA['camera'], {'frame':DATA['frame']})
    c.attach('operator','serial')
    assert c.twin.handle and c.owner == 'operator'


def test_reconnect_does_not_ignore_unknown_stop_failure(controller):
    c=controller
    c.twin.fail='stop'
    before=sum(op=='open' for op,_ in c.twin.calls)
    with pytest.raises(RuntimeError): c.connect('operator','serial',DATA['camera'],DATA['frame'])
    assert sum(op=='open' for op,_ in c.twin.calls)==before


def test_estimated_camera_cannot_install_robot_alignment(controller):
    c = controller
    c.scene = dict(floor=0)
    c.calibration.data['approximate'] = True
    before = len(c.twin.calls)
    with pytest.raises(ValueError, match='estimated lens'):
        c.preview_camera_robot('operator',[320,240],[240,240],640,480)
    assert len(c.twin.calls) == before
    assert c.camera_robot_preview is None and not c.candidates


def test_camera_confirmation_preserves_estimated_calibration_warning(controller):
    c = controller
    c.calibration.data['approximate'] = True
    c.camera_proposal = c.calibration
    c.confirm_camera('operator',640,480)
    assert 'Estimated' in c.camera_message
    assert c.calibration.data['approximate']
