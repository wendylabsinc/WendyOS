"""Agent video transport and the existing PR #2725 Linux twin API."""
import json
import os
import threading
import time
import urllib.request
import urllib.error
from core import RobotSessionExpired

import av
import cv2
import grpc
import numpy as np
import video_pb2 as video
import video_pb2_grpc
import unitree_pb2 as unitree
import unitree_pb2_grpc
import calibrated_pb2 as calibrated
import calibrated_pb2_grpc
from google.protobuf.json_format import MessageToDict


class Twin:
    def __init__(self):
        # Host networking reaches the existing twin's loopback-only listener.
        self.url = 'http://127.0.0.1:8791'
        self.handle = ''
        self.sequence = 0

    def rooms(self):
        return self._request('/v1/rooms')

    def room(self, frame):
        return self._request('/v1/rooms/'+frame.split('/')[0], timeout=10)

    def _request(self, path, data=None, timeout=.4):
        body = None if data is None else json.dumps(data, allow_nan=False).encode()
        req = urllib.request.Request(self.url + path, data=body, headers={'Content-Type': 'application/json'})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            message = error.read(4096).decode(errors='replace').strip()
            if path == '/v1/robot' and error.code == 400 and message == 'robot session expired':
                self.handle = ''
                raise RobotSessionExpired('Go2 session ended. Choose Retry setup to reconnect.') from error
            raise RuntimeError(message) from error

    def camera_alignment(self, operation, **values):
        return self._request('/v1/camera-alignment', dict(operation=operation, **values), timeout=60)

    def call(self, operation, **kwargs):
        self.sequence += 1
        result = self._request('/v1/robot', dict(Operation=operation, Handle=self.handle, Sequence=self.sequence, **kwargs), timeout=8 if operation in ('fuse','localize') else .4)
        if operation == 'open':
            self.handle = result['Handle']
        return result


class Agent:
    def __init__(self):
        socket = os.environ.get('WENDY_AGENT_SOCKET')
        if not socket:
            raise RuntimeError('WENDY_AGENT_SOCKET is missing. Run with the admin entitlement.')
        self.channel = grpc.insecure_channel('unix:' + socket, options=[('grpc.max_receive_message_length', 16 << 20)])
        self.video = video_pb2_grpc.WendyVideoServiceStub(self.channel)
        self.robots = unitree_pb2_grpc.WendyUnitreeServiceStub(self.channel)
        self.metadata_service = calibrated_pb2_grpc.WendyCalibratedFrameServiceStub(self.channel)

    def camera_metadata(self, stable_id):
        response = self.metadata_service.ListCameraMetadata(calibrated.ListCameraMetadataRequest(stable_ids=[stable_id]), timeout=3)
        matches = [c for c in response.cameras if c.stable_id == stable_id]
        if len(matches) != 1:
            raise ValueError('Align this camera with the room in Wendy, then choose Retry setup.')
        # Preserve numeric enum values for the calibration adapter.
        record = MessageToDict(matches[0], preserving_proto_field_name=True, use_integers_for_enums=True)
        for pose in record.get('site_poses', []):
            pose['valid_until_unix_ms'] = int(pose.get('valid_until_unix_ms', 0))
        return record

    def inventory(self):
        cameras = self.video.ListVideoDevices(video.ListVideoDevicesRequest(), timeout=3).devices
        robots = self.robots.ListUnitreePairings(unitree.ListUnitreePairingsRequest(), timeout=3).robots
        return {
            'cameras': [dict(id=c.stable_id, name=c.name, online=c.online) for c in cameras if c.stable_id.startswith('sensorlink:')],
            'robots': [dict(serial=r.serial, name=r.name, connected=r.connected) for r in robots if r.paired],
        }


class Camera:
    def __init__(self, agent, stable_id):
        self.lock = threading.Lock()
        self.latest = None
        self.latest_calibration = None
        self.error = 'Waiting for SensorLink video'
        self.stopped = threading.Event()
        self.stream = agent.video.StreamVideo(video.StreamVideoRequest(stable_id=stable_id, codec=video.VIDEO_CODEC_H264))
        self.thread = threading.Thread(target=self._read, daemon=True)
        self.thread.start()

    def _read(self):
        decoder = av.CodecContext.create('h264', 'r')
        decoder.thread_count = 1
        last_stamp = 0
        sequence = 0
        try:
            for chunk in self.stream:
                if self.stopped.is_set():
                    break
                if chunk.codec != video.VIDEO_CODEC_H264:
                    raise ValueError('Agent returned a codec other than H264.')
                # This is the agent delivery stamp, not a proven ESP32 exposure time.
                stamp = chunk.timestamp_ns / 1e9
                age = time.time() - stamp
                if not -.25 <= age <= 1.5 or stamp < last_stamp:
                    raise ValueError('Camera delivery timestamp is stale or out of order. Check gateway clock.')
                last_stamp = stamp
                for packet in decoder.parse(chunk.data):
                    for frame in decoder.decode(packet):
                        image = frame.to_ndarray(format='bgr24')
                        if image.shape[0] > 4096 or image.shape[1] > 4096:
                            raise ValueError('Camera image exceeds the supported size.')
                        sequence += 1
                        gray = calibration_luma(frame)
                        at = time.monotonic() - max(0, age)
                        with self.lock:
                            self.latest_calibration = (at, gray, sequence)
                            self.latest = (time.monotonic() - max(0, age), image)
                            self.error = ''
        except Exception as error:
            if not self.stopped.is_set():
                with self.lock:
                    self.error = str(error)
                    self.latest = None
                    self.latest_calibration = None
        finally:
            self.stream.cancel()

    def read(self):
        with self.lock:
            return self.latest, self.error

    def calibration_frame(self):
        with self.lock:
            return self.latest_calibration

    def close(self):
        self.stopped.set()
        self.stream.cancel()
        self.thread.join(timeout=2)



def calibration_luma(frame):
    """Match Apple's H.264 calibration input: full-range, unscaled Y plane."""
    if frame.format.name not in ('yuv420p', 'yuvj420p', 'nv12'):
        raise ValueError('Unsupported camera calibration pixel format.')
    plane=frame.planes[0]
    gray=np.frombuffer(plane,dtype=np.uint8).reshape(-1,plane.line_size)[:frame.height,:frame.width].copy()
    if frame.color_range != 2 and frame.format.name != 'yuvj420p':
        gray=np.clip((gray.astype(np.int32)-16)*255//219,0,255).astype(np.uint8)
    return gray
