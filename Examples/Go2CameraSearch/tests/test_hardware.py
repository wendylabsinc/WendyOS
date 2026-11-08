"""Run in the built image to exercise video decode and ONNX preprocessing."""
import time
from types import SimpleNamespace
import threading
import pytest
av = pytest.importorskip('av')
pytest.importorskip('video_pb2')
import numpy as np
from hardware import Camera


class Stream:
    def __init__(self, chunks):
        self.chunks=chunks
        self.done=threading.Event()
    def __iter__(self):
        yield from self.chunks
        self.done.wait(3)
    def cancel(self):
        self.done.set()


def video_stream(stale=False):
    encoder=av.CodecContext.create('libx264','w')
    encoder.width,encoder.height=64,48
    encoder.pix_fmt='yuv420p'
    encoder.options={'preset':'ultrafast','tune':'zerolatency'}
    image=np.zeros((48,64,3),np.uint8);image[:,:,2]=255
    chunks=[]
    for i in range(5):
        frame=av.VideoFrame.from_ndarray(image,format='bgr24')
        frame.pts=i
        for packet in encoder.encode(frame):
            chunks.append(SimpleNamespace(data=bytes(packet),codec=0,timestamp_ns=int((time.time()-(10 if stale else 0))*1e9)))
    return Stream(chunks)


def test_real_h264_decode_and_stable_camera_selection():
    stream=video_stream()
    requests=[]
    def open_stream(request):
        requests.append(request)
        return stream
    camera=Camera(SimpleNamespace(video=SimpleNamespace(StreamVideo=open_stream)),'sensorlink:test:camera')
    try:
        deadline=time.monotonic()+2
        while camera.read()[0] is None and time.monotonic()<deadline: time.sleep(.01)
        result,error=camera.read()
        assert result is not None,error
        assert result[1].shape==(48,64,3)
        assert result[1][:,:,2].mean()>240
        assert requests[0].stable_id=='sensorlink:test:camera'
    finally:
        camera.close()
    assert not camera.thread.is_alive()


def test_stale_video_delivery_is_rejected():
    stream=video_stream(stale=True)
    camera=Camera(SimpleNamespace(video=SimpleNamespace(StreamVideo=lambda _:stream)),'sensorlink:test:camera')
    camera.thread.join(2)
    result,error=camera.read()
    assert result is None and 'timestamp' in error
    camera.close()


@pytest.mark.parametrize('code,message,expired', [(400,b'robot session expired\n',True),(503,b'robot session expired\n',False),(400,b'stop failed',False)])
def test_twin_classifies_only_explicit_session_expiry(monkeypatch,code,message,expired):
    import io
    import urllib.error
    from hardware import Twin
    from core import RobotSessionExpired
    twin=Twin(); twin.handle='old'
    def reject(*args,**kwargs):
        raise urllib.error.HTTPError('http://local',code,'error',{},io.BytesIO(message))
    monkeypatch.setattr('urllib.request.urlopen',reject)
    with pytest.raises(RuntimeError) as result: twin.call('poll')
    assert isinstance(result.value,RobotSessionExpired)==expired
    assert (twin.handle=='')==expired
