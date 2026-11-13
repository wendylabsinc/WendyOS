"""Simulator-only autonomous exploration sample. Starts parked; no hardware fallback."""
import array
import http.client
import json
import math
import os
from pathlib import Path
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

from planner import Planner


class Simulator:
    def __init__(self, url):
        target = urlsplit(url)
        if target.scheme != 'http' or not target.hostname or target.path not in ('', '/'):
            raise ValueError('R2_SIMULATOR_URL must be an HTTP origin')
        self.host, self.port = target.hostname, target.port or 80
        self.connection = None

    def request(self, path, body=None, raw=False):
        if self.connection is None:
            self.connection = http.client.HTTPConnection(self.host, self.port, timeout=.4)
        try:
            self.connection.request('GET' if body is None else 'POST', path,
                                    None if body is None else json.dumps(body),
                                    {'Content-Type': 'application/json'})
            response = self.connection.getresponse()
            data = response.read()
            if response.status != 200:
                raise RuntimeError(json.loads(data).get('error', 'Simulator request failed'))
            return data if raw else json.loads(data)
        except Exception:
            self.close()
            raise

    def close(self):
        if self.connection:
            self.connection.close()
        self.connection = None


def check_status(status):
    if status.get('simulation') is not True or status.get('robot_kind') != 'rosmaster-r2':
        raise ValueError('This sample requires the Wendy ROSMASTER R2 simulator')
    if not status.get('ready') or not status.get('healthy') or status.get('mode') == 'paused':
        raise ValueError('Resume a healthy simulator before exploring')


def check_fresh(capture_ns, now_ns):
    age = (now_ns-capture_ns)/1e9
    if not math.isfinite(age) or not -.2 <= age <= .5:
        raise ValueError('Sensor data is stale; exploration stopped')


def front_depth(data, calibration):
    width, height = calibration['width'], calibration['height']
    if calibration.get('encoding') != '16UC1' or calibration.get('depth_unit') != 'mm':
        raise ValueError('Expected 16-bit depth in millimetres')
    if len(data) != width*height*2:
        raise ValueError('Incomplete depth frame')
    values = array.array('H', data)
    if sys.byteorder != 'little':
        values.byteswap()
    # Above the horizon: floor returns must not look like a wall.
    samples = [values[y*width+x]/1000. for y in range(int(height*.3), int(height*.5), 4)
               for x in range(int(width*.35), int(width*.65), 4) if values[y*width+x]]
    return min(samples) if samples else None  # zero is unknown, not zero distance


class Explorer:
    def __init__(self, url):
        self.sim = Simulator(url)
        self.lock = threading.RLock()
        self.closed = threading.Event()
        self.planner = Planner()
        self.running = False
        self.token = None
        self.sequence = 0
        self.message = 'Ready. Start a two-minute exploration.'
        self.pose = None
        self.epoch = None
        self.deadline = 0.
        self.depth = None
        self.distance = 0.
        self.thread = threading.Thread(target=self.loop, daemon=True)

    def command(self, speed=0., steering=0., stop=False):
        self.sequence += 1
        return self.sim.request('/api/app/command', {'token': self.token, 'sequence': self.sequence,
                                                   'speed': speed, 'steering': steering, 'stop': stop})

    def start(self, duration=120):
        if type(duration) not in (int, float) or not math.isfinite(duration) or not 5 <= duration <= 600:
            raise ValueError('Duration must be between 5 and 600 seconds')
        with self.lock:
            if self.running:
                return
            status = self.sim.request('/api/status')
            check_status(status)
            self.calibration = self.sim.request('/api/camera/info')
            # The explicit Start button transfers simulator control to this app.
            self.sim.request('/api/arm', {'mode': 'app'})
            self.token = self.sim.request('/api/app/claim', {})['token']
            self.sequence = 0
            self.epoch = status['epoch']
            self.planner = Planner()
            self.start_distance = status['state']['distance']
            self.deadline = time.monotonic()+duration
            self.running = True
            self.message = 'Exploring'

    def stop(self, message='Stopped'):
        with self.lock:
            self.running = False
            if self.token:
                try:
                    self.command(stop=True)
                except Exception:
                    pass  # A revoked token must never stop the new controller.
                self.token = None
            self.message = message

    def step(self):
        status = self.sim.request('/api/status')
        check_status(status)
        if status['epoch'] != self.epoch or status['control_mode'] != 'app':
            raise ValueError('Simulator control changed; press Start to explore again')
        if status['state']['collision']:
            raise ValueError('Obstacle contact; exploration stopped')
        scan = self.sim.request('/api/scan')
        if scan['epoch'] != self.epoch or scan['paused']:
            raise ValueError('Scan belongs to a paused or reset world')
        check_fresh(scan['capture_ns'], time.time_ns())
        check_fresh(status['camera']['capture_ns'], time.time_ns())
        valid = [v for v in scan['ranges'] if isinstance(v, (int, float)) and math.isfinite(v) and v > 0]
        if len(valid) < len(scan['ranges'])*.8 or len(valid) < 90:
            raise ValueError('Insufficient lidar returns; exploration stopped')
        depth = self.sim.request('/api/camera/depth.raw', raw=True)
        self.depth = front_depth(depth, self.calibration)
        # Check again after network reads; never drive using a delayed response.
        check_fresh(scan['capture_ns'], time.time_ns())
        check_fresh(status['camera']['capture_ns'], time.time_ns())
        self.pose = status['state']
        self.distance = max(0., self.pose['distance']-self.start_distance)
        speed, steering, self.message = self.planner.command(self.pose, scan, self.depth)
        self.command(speed, steering, stop=speed == 0)

    def loop(self):
        while not self.closed.wait(.1):
            with self.lock:
                if not self.running:
                    continue
                try:
                    if time.monotonic() >= self.deadline:
                        self.stop('Exploration finished')
                    else:
                        self.step()
                except Exception as error:
                    self.stop(str(error))

    def snapshot(self):
        with self.lock:
            return {'running': self.running, 'message': self.message, 'pose': self.pose,
                    'distance': round(self.distance, 2), 'depth_front': self.depth,
                    'remaining': max(0, round(self.deadline-time.monotonic())) if self.running else 0,
                    **self.planner.map()}

    def close(self):
        self.closed.set()
        self.stop('App shut down')
        self.thread.join(timeout=3)
        self.sim.close()


class Server(ThreadingHTTPServer):
    request_queue_size = 128


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *_):
        pass

    def send(self, code, value, mime='application/json'):
        data = json.dumps(value, allow_nan=False).encode() if mime == 'application/json' else value
        self.send_response(code)
        self.send_header('Content-Type', mime)
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Cache-Control', 'no-store')
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_GET(self):
        path = urlsplit(self.path).path
        if path == '/':
            self.send(200, Path(__file__).with_name('index.html').read_bytes(), 'text/html; charset=utf-8')
        elif path in ('/api/status', '/health'):
            self.send(200, self.server.explorer.snapshot())
        elif path in ('/camera/color.jpg', '/camera/depth.jpg'):
            # A separate connection keeps dashboard images out of the control loop.
            sim = Simulator(self.server.sim_url)
            try:
                self.send(200, sim.request('/api'+path, raw=True), 'image/jpeg')
            except Exception as error:
                self.send(503, {'error': str(error)})
            finally:
                sim.close()
        else:
            self.send(404, {'error': 'Not found'})

    def do_POST(self):
        origin = self.headers.get('Origin')
        if (origin and urlsplit(origin).netloc != self.headers.get('Host')) or self.headers.get('Sec-Fetch-Site') == 'cross-site':
            self.close_connection = True
            self.send(403, {'error': 'Cross-origin control is disabled'})
            return
        try:
            size = int(self.headers.get('Content-Length', '0'))
            if not 0 <= size <= 1024:
                self.close_connection = True
                raise ValueError('Invalid request size')
            data = json.loads(self.rfile.read(size) or b'{}')
            if self.path == '/api/start':
                self.server.explorer.start(data.get('duration', 120))
            elif self.path == '/api/stop':
                self.server.explorer.stop()
            else:
                self.send(404, {'error': 'Not found'})
                return
            self.send(200, self.server.explorer.snapshot())
        except Exception as error:
            self.send(400, {'error': str(error)})


def main():
    url = os.environ.get('R2_SIMULATOR_URL', 'http://127.0.0.1:8890')
    explorer = Explorer(url)
    server = Server(('0.0.0.0', int(os.environ.get('PORT', '3510'))), Handler)
    server.explorer, server.sim_url = explorer, url
    explorer.thread.start()
    def shutdown(*_):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)
    print(f'Explorer ready at :{server.server_port}; parked until Start is pressed', flush=True)
    try:
        server.serve_forever()
    finally:
        explorer.close()
        server.server_close()


if __name__ == '__main__':
    main()
