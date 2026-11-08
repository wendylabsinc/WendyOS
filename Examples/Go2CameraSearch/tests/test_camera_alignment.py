"""Client behavior only. Calibration policy tests live in go/internal/twin*."""
import threading
import time
from types import SimpleNamespace
import numpy as np
import pytest


@pytest.mark.parametrize('cancelled',[False,True])
def test_alignment_worker_only_commits_current_service_result(cancelled):
    import app
    from controller import Controller
    from test_search import Twin
    c=Controller(Twin())
    c.owner='operator';c.frame='room/revision';c.camera_id='sensorlink:test'
    c.scene={'floor':0};c.phase='camera-calibrating'
    c.validate_frame=lambda:None
    c.propose_camera=lambda record:setattr(c,'camera_proposal',record)
    calls=[]
    def call(operation,**values):
        calls.append(operation)
        if operation=='verify' and cancelled:c.stop()
        return dict(policyVersion=1,token='ticket',metadata={'stable_id':'sensorlink:test'})
    c.twin.camera_alignment=call
    runtime=app.Runtime.__new__(app.Runtime)
    runtime.controller=c;runtime.quit=threading.Event()
    runtime.camera_lock=threading.Lock()
    runtime.camera=SimpleNamespace(calibration_frame=lambda:(time.monotonic(),np.zeros((32,32),np.uint8),1))
    runtime.alignment_lock=threading.Lock();runtime.alignment_lock.acquire()
    runtime._align_camera('operator',0,'room/revision','sensorlink:test')
    assert calls==(['begin','verify','cancel'] if cancelled else ['begin','verify','commit'])
    assert not runtime.alignment_lock.locked()
    if not cancelled:assert c.camera_proposal['stable_id']=='sensorlink:test'


def test_h264_calibration_matches_full_range_luma_conversion():
    import av
    from hardware import calibration_luma
    frame=av.VideoFrame(32,32,'yuv420p')
    frame.color_range=1
    for plane in frame.planes:
        plane.update(bytes([128])*plane.buffer_size)
    y=np.frombuffer(frame.planes[0],np.uint8).reshape(-1,frame.planes[0].line_size)
    y[0,:5]=[0,16,128,235,255]
    gray=calibration_luma(frame)
    assert gray.shape==(32,32)
    assert gray[0,:5].tolist()==[0,0,130,255,255]
    frame.color_range=2
    assert calibration_luma(frame)[0,:5].tolist()==[0,16,128,235,255]
